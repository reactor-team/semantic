package embed

import (
	"math"
	"testing"
)

// vecEqual compares two float32 slices at float32 tolerance.
func vecEqual(a, b []float32) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if math.Abs(float64(a[i]-b[i])) > 1e-6 {
			return false
		}
	}
	return true
}

// meanPool averages over the real tokens only. The padding a batch adds
// carries no meaning, and letting it into the average would pull every short
// chunk's vector toward the same point.
func TestMeanPool(t *testing.T) {
	t.Parallel()
	// Three tokens, two dimensions each.
	hidden := []float32{
		1, 2, // token 0
		3, 4, // token 1
		99, 99, // token 2 — masked out
	}
	mask := []int64{1, 1, 0}
	got := meanPool(hidden, mask, 3, 2)
	if want := []float32{2, 3}; !vecEqual(got, want) {
		t.Errorf("meanPool = %v, want %v", got, want)
	}
}

// A fully masked input has nothing to average. The count guard returns zeros
// rather than dividing by zero and producing NaN, which would poison every
// later cosine comparison in the index.
func TestMeanPool_AllMasked(t *testing.T) {
	t.Parallel()
	got := meanPool([]float32{5, 6, 7, 8}, []int64{0, 0}, 2, 2)
	if want := []float32{0, 0}; !vecEqual(got, want) {
		t.Errorf("meanPool with an empty mask = %v, want %v", got, want)
	}
}

// seqLen is the truncated length, which may be shorter than the hidden state
// the model returned. Pooling must stop at seqLen and not read past it.
func TestMeanPool_HonoursSeqLen(t *testing.T) {
	t.Parallel()
	hidden := []float32{1, 1, 2, 2, 100, 100}
	got := meanPool(hidden, []int64{1, 1}, 2, 2)
	if want := []float32{1.5, 1.5}; !vecEqual(got, want) {
		t.Errorf("meanPool = %v, want %v", got, want)
	}
}

// A hidden state shorter than seqLen means the model returned a shape this
// code did not expect. Pooling stops at what is actually there, so an index
// run degrades to a short average instead of panicking partway through.
func TestMeanPool_ShortInputsDoNotPanic(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name   string
		hidden []float32
		mask   []int64
		seqLen int
		want   []float32
	}{
		// Two dimensions promised for three tokens, one token delivered.
		{"hidden truncated", []float32{4, 6}, []int64{1, 1, 1}, 3, []float32{4, 6}},
		// The mask runs out first; the tokens it does not cover are dropped.
		{"mask truncated", []float32{1, 1, 9, 9}, []int64{1}, 2, []float32{1, 1}},
		{"nothing at all", nil, nil, 4, []float32{0, 0}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			if got := meanPool(tc.hidden, tc.mask, tc.seqLen, 2); !vecEqual(got, tc.want) {
				t.Errorf("meanPool = %v, want %v", got, tc.want)
			}
		})
	}
}

// Vectors leave normalizeVec at unit length, which is what lets CosineSim
// reduce to a dot product for stored rows.
func TestNormalizeVec(t *testing.T) {
	t.Parallel()
	v := []float32{3, 4}
	normalizeVec(v)
	if want := []float32{0.6, 0.8}; !vecEqual(v, want) {
		t.Errorf("normalizeVec = %v, want %v", v, want)
	}

	var norm float64
	for _, x := range v {
		norm += float64(x) * float64(x)
	}
	if math.Abs(math.Sqrt(norm)-1) > 1e-6 {
		t.Errorf("normalized length = %v, want 1", math.Sqrt(norm))
	}
}

// A zero vector has no direction to normalize. It is left alone rather than
// divided by zero — the same reason meanPool guards its count.
func TestNormalizeVec_Zero(t *testing.T) {
	t.Parallel()
	v := []float32{0, 0, 0}
	normalizeVec(v)
	for i, x := range v {
		if x != 0 {
			t.Errorf("normalizeVec turned a zero vector into %v at %d", x, i)
		}
	}
}

// Normalizing twice changes nothing, so a vector that makes a second pass
// through the pipeline is not silently rescaled.
func TestNormalizeVec_Idempotent(t *testing.T) {
	t.Parallel()
	v := []float32{0.3, -1.2, 4}
	normalizeVec(v)
	once := append([]float32(nil), v...)
	normalizeVec(v)
	if !vecEqual(v, once) {
		t.Errorf("second normalizeVec changed the vector: %v then %v", once, v)
	}
}

// The two steps compose into what Get returns: a unit vector whose cosine
// similarity with itself is exactly 1.
func TestPoolThenNormalize(t *testing.T) {
	t.Parallel()
	hidden := []float32{0.5, -0.25, 1, 0.75, 0, 0}
	v := meanPool(hidden, []int64{1, 1, 0}, 3, 2)
	normalizeVec(v)
	if got := CosineSim(v, v); math.Abs(got-1) > 1e-9 {
		t.Errorf("self-similarity of a pooled+normalized vector = %v, want 1", got)
	}
}

// TestGet_RealModel exercises the ONNX path when the model happens to be
// installed, and skips otherwise. Inference needs a ~90 MB download and a
// native runtime, so CI does not have it — but a developer who has run
// `semantic init` gets the one check the pure functions above cannot make:
// that the whole pipeline produces a usable vector space.
func TestGet_RealModel(t *testing.T) {
	if err := Check(); err != nil {
		t.Skipf("embedding model not installed: %v", err)
	}

	v, err := Get("the scheduler assigns each job to a worker node")
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if len(v) != modelDim {
		t.Fatalf("Get returned %d dimensions, want %d", len(v), modelDim)
	}
	if got := CosineSim(v, v); math.Abs(got-1) > 1e-6 {
		t.Errorf("Get returned an unnormalized vector: self-similarity %v", got)
	}

	// The space has to be ordered the way search assumes: a paraphrase scores
	// above an unrelated sentence. Without this the vectors could be unit
	// length and still meaningless.
	near, err := Get("jobs are dispatched to worker nodes by the scheduler")
	if err != nil {
		t.Fatal(err)
	}
	far, err := Get("a recipe for sourdough bread")
	if err != nil {
		t.Fatal(err)
	}
	if CosineSim(v, near) <= CosineSim(v, far) {
		t.Errorf("paraphrase scored %v, unrelated text scored %v; want the paraphrase higher",
			CosineSim(v, near), CosineSim(v, far))
	}

	// Embedding is deterministic. The session is pinned to one thread for
	// exactly this reason: a score that moves between runs would make search
	// results unstable and this suite flaky.
	again, err := Get("the scheduler assigns each job to a worker node")
	if err != nil {
		t.Fatal(err)
	}
	if !vecEqual(v, again) {
		t.Error("Get returned different vectors for the same input")
	}
}
