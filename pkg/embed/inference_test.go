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

// clsPool takes the first token and nothing else. bge-small-en-v1.5 puts the
// sentence representation at the [CLS] position, so the later tokens are not
// averaged in — including any the caller masked out, which is why clsPool needs
// no mask at all.
func TestClsPool(t *testing.T) {
	t.Parallel()
	// Three tokens, two dimensions each.
	hidden := []float32{
		1, 2, // token 0 — [CLS]
		3, 4, // token 1
		99, 99, // token 2
	}
	got := clsPool(hidden, 2)
	if want := []float32{1, 2}; !vecEqual(got, want) {
		t.Errorf("clsPool = %v, want %v", got, want)
	}
}

// A hidden state shorter than the dimension means the model returned a shape
// this code did not expect. Pooling takes what is there and leaves the rest
// zero, so an index run degrades instead of panicking partway through.
func TestClsPool_ShortInputsDoNotPanic(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name   string
		hidden []float32
		dim    int
		want   []float32
	}{
		{"hidden truncated", []float32{4}, 2, []float32{4, 0}},
		{"nothing at all", nil, 2, []float32{0, 0}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			if got := clsPool(tc.hidden, tc.dim); !vecEqual(got, tc.want) {
				t.Errorf("clsPool = %v, want %v", got, tc.want)
			}
		})
	}
}

// A non-positive dimension asks for no vector at all. Returning an empty slice
// keeps the caller on the same path as any other degenerate shape.
func TestClsPool_ZeroDim(t *testing.T) {
	t.Parallel()
	if got := clsPool([]float32{1, 2, 3}, 0); len(got) != 0 {
		t.Errorf("clsPool with dim 0 = %v, want empty", got)
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
// divided by zero, which would poison every later cosine comparison with NaN.
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
	v := clsPool(hidden, 2)
	normalizeVec(v)
	if got := CosineSim(v, v); math.Abs(got-1) > 1e-9 {
		t.Errorf("self-similarity of a pooled+normalized vector = %v, want 1", got)
	}
}

// TestGet_RealModel exercises the ONNX path when the model happens to be
// installed, and skips otherwise. Inference needs a ~127 MB download and a
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
	if len(v) != Current().Dim {
		t.Fatalf("Get returned %d dimensions, want %d", len(v), Current().Dim)
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
