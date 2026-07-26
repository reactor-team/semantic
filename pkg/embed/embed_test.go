package embed

import (
	"math"
	"path/filepath"
	"strings"
	"testing"
)

// closeTo compares two cosine scores at float64 tolerance.
func closeTo(got, want float64) bool { return math.Abs(got-want) < 1e-9 }

func TestCosineSim(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name string
		a, b Vec
		want float64
	}{
		{"identical", Vec{1, 0, 0}, Vec{1, 0, 0}, 1},
		{"orthogonal", Vec{1, 0}, Vec{0, 1}, 0},
		{"opposite", Vec{1, 0}, Vec{-1, 0}, -1},
		{"forty-five degrees", Vec{1, 0}, Vec{1, 1}, math.Sqrt2 / 2},
		// Magnitude is divided out, so an unnormalized vector scores the same
		// as its unit form. Get returns normalized vectors, but the full
		// formula stays so a caller may pass raw ones.
		{"scale invariant", Vec{3, 0}, Vec{7, 0}, 1},
		// A mismatched length is a caller bug, not a distance. Returning 0
		// keeps a corrupt row out of the results instead of panicking mid-scan.
		{"length mismatch", Vec{1, 0}, Vec{1, 0, 0}, 0},
		{"both empty", Vec{}, Vec{}, 0},
		{"nil", nil, nil, 0},
		// A zero vector has no direction, so no angle exists. The guard also
		// keeps the division from producing NaN.
		{"zero vector", Vec{0, 0}, Vec{1, 1}, 0},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			if got := CosineSim(tc.a, tc.b); !closeTo(got, tc.want) {
				t.Errorf("CosineSim(%v, %v) = %v, want %v", tc.a, tc.b, got, tc.want)
			}
		})
	}
}

// CosineSim is symmetric. Search sorts on it, so an asymmetry would make the
// ranking depend on argument order.
func TestCosineSim_Symmetric(t *testing.T) {
	t.Parallel()
	a, b := Vec{0.2, -0.7, 0.1}, Vec{0.9, 0.3, -0.4}
	if x, y := CosineSim(a, b), CosineSim(b, a); !closeTo(x, y) {
		t.Errorf("CosineSim is not symmetric: %v vs %v", x, y)
	}
}

// The score stays inside [-1, 1] for arbitrary inputs. MinScore comparisons in
// search assume that range.
func TestCosineSim_Bounded(t *testing.T) {
	t.Parallel()
	vecs := []Vec{
		{1, 2, 3}, {-1, -2, -3}, {0.001, 0, 0}, {1e6, -1e6, 0}, {1, 1, 1},
	}
	for _, a := range vecs {
		for _, b := range vecs {
			got := CosineSim(a, b)
			if got < -1.0000001 || got > 1.0000001 {
				t.Errorf("CosineSim(%v, %v) = %v, outside [-1, 1]", a, b, got)
			}
		}
	}
}

// RepresentationID is the index's record of which vector space its rows live
// in; the index rebuilds when it stops matching. The literals are pinned
// rather than recomputed from the fields, because a test that rebuilds the
// string the same way the code does would agree with any change and so guard
// nothing. Changing one of these means every index built with that model
// rebuilds — intended when the vectors really did change, a bug otherwise.
//
// all-MiniLM-L6-v2's literal carries the extra duty of reproducing what
// semantic stamped before the registry existed, so an index built by an older
// release stays valid when that model is selected instead of silently
// rebuilding.
func TestRepresentationID_PinnedPerModel(t *testing.T) {
	t.Parallel()
	want := map[string]string{
		"arctic-embed-xs":   "arctic-embed-xs+cls+l2+d384+s512",
		"bge-small-en-v1.5": "bge-small-en-v1.5+cls+l2+d384+s512",
		"all-MiniLM-L6-v2":  "all-MiniLM-L6-v2+mean+l2+d384+s256",
		// The int8 build shares every field with the checkpoint above except
		// its name, which is the only thing keeping their vectors apart. If
		// these two IDs ever collide, an index embedded with one would be
		// served against a query embedded with the other.
		"bge-small-en-v1.5-int8": "bge-small-en-v1.5-int8+cls+l2+d384+s512",
	}
	for _, m := range Models() {
		w, ok := want[m.Name]
		if !ok {
			t.Errorf("model %q is in the registry with no pinned ID here; add one", m.Name)
			continue
		}
		if got := m.RepresentationID(); got != w {
			t.Errorf("%s: RepresentationID() = %q, want %q\n"+
				"if the vector space genuinely changed, update the literal; every index built with it will rebuild",
				m.Name, got, w)
		}
	}
	if len(want) != len(Models()) {
		t.Errorf("pinned %d IDs for %d registered models", len(want), len(Models()))
	}
}

// Each component of the ID names something that changes the vectors. This
// pins that all four are present, so dropping one from the format string
// fails here rather than silently letting two spaces collide.
func TestRepresentationID_NamesEveryComponent(t *testing.T) {
	t.Parallel()
	m, ok := Lookup(DefaultModel)
	if !ok {
		t.Fatalf("DefaultModel %q is not in the registry", DefaultModel)
	}
	id := m.RepresentationID()
	for _, part := range []string{m.Name, "cls", "l2", "d384", "s512"} {
		if !strings.Contains(id, part) {
			t.Errorf("RepresentationID() = %q, missing %q", id, part)
		}
	}
}

// TestRepresentationID_TracksDocPrefix covers the asymmetry between the two
// prefixes. The query marker never reaches the index, so naming it would force
// a pointless rebuild; the document marker is embedded into every stored
// vector, so omitting it would let two incompatible indexes share an ID.
func TestRepresentationID_TracksDocPrefix(t *testing.T) {
	t.Parallel()

	base := Model{Name: "m", Dim: 384, MaxSeqLen: 512, Pooling: PoolMean}
	queryOnly := base
	queryOnly.QueryPrefix = "query: "
	if base.RepresentationID() != queryOnly.RepresentationID() {
		t.Errorf("a query prefix must not change the ID: %q vs %q",
			base.RepresentationID(), queryOnly.RepresentationID())
	}

	doc := base
	doc.DocPrefix = "passage: "
	if base.RepresentationID() == doc.RepresentationID() {
		t.Errorf("a document prefix must change the ID, both were %q", doc.RepresentationID())
	}

	other := base
	other.DocPrefix = "search_document: "
	if doc.RepresentationID() == other.RepresentationID() {
		t.Errorf("two document prefixes collided on %q", doc.RepresentationID())
	}
}

// A registry entry that names a pooling the inference path does not implement
// would embed with the CLS fallback while RepresentationID advertised
// something else — the stamp would claim a vector space the vectors are not
// from, and the index would never notice.
func TestRegistry_PoolingIsImplemented(t *testing.T) {
	t.Parallel()
	for _, m := range Models() {
		switch m.Pooling {
		case PoolCLS, PoolMean:
		default:
			t.Errorf("%s: pooling %q is not implemented by pool()", m.Name, m.Pooling)
		}
		if m.Dim <= 0 || m.MaxSeqLen <= 0 {
			t.Errorf("%s: Dim=%d MaxSeqLen=%d, both must be positive", m.Name, m.Dim, m.MaxSeqLen)
		}
		if m.ModelURL == "" || m.TokenizerURL == "" {
			t.Errorf("%s: missing a download URL", m.Name)
		}
	}
}

// Lookup is case-insensitive because all-MiniLM-L6-v2 is not a name anyone
// types the same way twice.
func TestLookup_CaseInsensitive(t *testing.T) {
	t.Parallel()
	for _, spelling := range []string{"all-MiniLM-L6-v2", "all-minilm-l6-v2", "ALL-MINILM-L6-V2", "  all-MiniLM-L6-v2  "} {
		m, ok := Lookup(spelling)
		if !ok {
			t.Errorf("Lookup(%q) found nothing", spelling)
			continue
		}
		if m.Name != "all-MiniLM-L6-v2" {
			t.Errorf("Lookup(%q) = %q", spelling, m.Name)
		}
	}
	if _, ok := Lookup("no-such-model"); ok {
		t.Error("Lookup found a model that does not exist")
	}
}

// An unknown name has to fail loudly and say what it could have been. Getting
// this wrong means a typo silently embeds with the default and the user
// notices only in the results.
func TestSelect_UnknownNameListsTheKnownOnes(t *testing.T) {
	err := Select("bge-huge-en-v9")
	if err == nil {
		t.Fatal("Select accepted a model that does not exist")
	}
	for _, name := range ModelNames() {
		if !strings.Contains(err.Error(), name) {
			t.Errorf("error %q does not mention %q", err, name)
		}
	}
}

// Selection drives what everything else in the package reports: the cache
// path, the representation stamp, and whether a query gets a prefix. This
// walks a switch and checks all three move together, because a partial switch
// would embed with one model and record another.
func TestSelect_MovesTheWholePackage(t *testing.T) {
	t.Setenv("SEMANTIC_CACHE_DIR", t.TempDir())
	t.Setenv("SEMANTIC_MODEL_DIR", "")
	t.Cleanup(func() { _ = Select(DefaultModel) })

	if err := Select("all-MiniLM-L6-v2"); err != nil {
		t.Fatalf("Select: %v", err)
	}
	if got := RepresentationID(); got != "all-MiniLM-L6-v2+mean+l2+d384+s256" {
		t.Errorf("after Select, RepresentationID() = %q", got)
	}
	if got := filepath.Base(ModelCacheDir()); got != "all-minilm-l6-v2" {
		t.Errorf("after Select, ModelCacheDir() ends in %q", got)
	}
	if p := Current().QueryPrefix; p != "" {
		t.Errorf("all-MiniLM-L6-v2 is symmetric; QueryPrefix = %q, want empty", p)
	}

	if err := Select("bge-small-en-v1.5"); err != nil {
		t.Fatalf("Select: %v", err)
	}
	if got := RepresentationID(); got != "bge-small-en-v1.5+cls+l2+d384+s512" {
		t.Errorf("after Select back, RepresentationID() = %q", got)
	}
	if got := filepath.Base(ModelCacheDir()); got != "bge-small-en-v1.5" {
		t.Errorf("after Select back, ModelCacheDir() ends in %q", got)
	}
	if p := Current().QueryPrefix; p == "" {
		t.Error("bge-small-en-v1.5 is asymmetric; QueryPrefix is empty")
	}
}

// An empty name is how the CLI passes "the user gave no --model": fall through
// to $SEMANTIC_MODEL, then to the default.
func TestSelect_EmptyNameFallsBack(t *testing.T) {
	t.Setenv("SEMANTIC_MODEL", "all-MiniLM-L6-v2")
	t.Cleanup(func() { _ = Select(DefaultModel) })
	if err := Select(""); err != nil {
		t.Fatalf("Select: %v", err)
	}
	if got := Current().Name; got != "all-MiniLM-L6-v2" {
		t.Errorf("Select(\"\") with $SEMANTIC_MODEL set = %q", got)
	}

	t.Setenv("SEMANTIC_MODEL", "")
	if err := Select(""); err != nil {
		t.Fatalf("Select: %v", err)
	}
	if got := Current().Name; got != DefaultModel {
		t.Errorf("Select(\"\") with no env = %q, want %q", got, DefaultModel)
	}
}
