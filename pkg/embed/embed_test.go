package embed

import (
	"math"
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
// in; the index rebuilds when it stops matching. The literal is pinned rather
// than recomputed from the constants, because a test that rebuilds the string
// the same way the code does would agree with any change and so guard nothing.
// Changing this literal means every existing index rebuilds — intended when
// the vectors really did change, a bug otherwise.
func TestRepresentationID(t *testing.T) {
	t.Parallel()
	const want = "all-MiniLM-L6-v2+mean+l2+d384+s256"
	if got := RepresentationID(); got != want {
		t.Errorf("RepresentationID() = %q, want %q\n"+
			"if the vector space genuinely changed, update the literal; every index will rebuild", got, want)
	}
}

// Each component of the ID names something that changes the vectors. This
// pins that all four are present, so dropping one from the format string
// fails here rather than silently letting two spaces collide.
func TestRepresentationID_NamesEveryComponent(t *testing.T) {
	t.Parallel()
	id := RepresentationID()
	for _, part := range []string{modelName, "mean", "l2", "d384", "s256"} {
		if !strings.Contains(id, part) {
			t.Errorf("RepresentationID() = %q, missing %q", id, part)
		}
	}
}
