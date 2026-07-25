package search

import (
	"sort"

	"github.com/reactor-team/semantic/pkg/chunk"
	"github.com/reactor-team/semantic/pkg/embed"
	"github.com/reactor-team/semantic/pkg/index"
)

// DupeOptions tunes a near-duplicate scan.
type DupeOptions struct {
	MinScore   float64 // report pairs at or above this cosine value
	Limit      int     // max pairs to return; <= 0 means no cap
	PathPrefix string  // restrict to files under this rel-path prefix
	WithinFile bool    // also report pairs from the same file (default: cross-file only)
}

// Pair is two chunks found to be near-duplicates, ordered so A sorts before
// B (by rel-path then key) for a stable presentation.
type Pair struct {
	A     Hit     `json:"a"`
	B     Hit     `json:"b"`
	Score float64 `json:"score"`
}

// dupeVariants is the set of chunk kinds that represent a section's own
// content — the retrieval units worth comparing for redundancy. We compare
// one vector per logical section: `narrow` carries a heading's direct text
// (`full` would double-count it against the parent's subtree, manufacturing
// trivial matches), and `path`/`title` are navigational, not content.
var dupeVariants = map[string]bool{
	chunk.VariantNarrow:  true,
	chunk.VariantBody:    true,
	chunk.VariantPackage: true,
	chunk.VariantType:    true,
	chunk.VariantFunc:    true,
	chunk.VariantMethod:  true,
	chunk.VariantValue:   true,
}

// Duplicates scans the index for pairs of chunks whose cosine similarity is
// at or above opts.MinScore — near-duplicate sections that likely encode
// redundant docs or guidance. By default only cross-file pairs are reported
// (set WithinFile to include repetition inside one file). Vectors from the
// embedder are L2-normalized, so this is an all-pairs dot product; at
// personal-vault scale the O(n²) scan is well under a second.
func Duplicates(src ChunkSource, opts DupeOptions) ([]Pair, error) {
	rows, err := src.AllChunks(opts.PathPrefix)
	if err != nil {
		return nil, err
	}

	// Keep only content-bearing variants, so each logical section is
	// represented once and pairs are section-vs-section rather than a
	// heading's own variants matching each other.
	var cand []index.ChunkRow
	for _, r := range rows {
		if dupeVariants[r.Variant] {
			cand = append(cand, r)
		}
	}

	var pairs []Pair
	for i := range cand {
		for j := i + 1; j < len(cand); j++ {
			if !opts.WithinFile && cand[i].RelPath == cand[j].RelPath {
				continue
			}
			score := embed.CosineSim(cand[i].Vec, cand[j].Vec)
			if score < opts.MinScore {
				continue
			}
			a, b := hitOf(cand[i]), hitOf(cand[j])
			if !hitLess(a, b) {
				a, b = b, a
			}
			pairs = append(pairs, Pair{A: a, B: b, Score: score})
		}
	}

	// Most-similar first, with a stable tie-break on the pair's endpoints so
	// equal scores don't reorder run-to-run (matters for tests and diffs).
	sort.Slice(pairs, func(i, j int) bool {
		if pairs[i].Score != pairs[j].Score {
			return pairs[i].Score > pairs[j].Score
		}
		if !eqHit(pairs[i].A, pairs[j].A) {
			return hitLess(pairs[i].A, pairs[j].A)
		}
		return hitLess(pairs[i].B, pairs[j].B)
	})

	if opts.Limit > 0 && len(pairs) > opts.Limit {
		pairs = pairs[:opts.Limit]
	}
	return pairs, nil
}

// hitOf projects an index row into the Hit shape shared with search results,
// minus the query-relative Score (a pair carries its own similarity).
func hitOf(r index.ChunkRow) Hit { //nolint:gocritic // hugeParam: value semantics intentional; callers pass range-loop copies
	return Hit{
		RelPath: r.RelPath,
		Line:    r.Line,
		Heading: r.Heading,
		Variant: r.Variant,
		Key:     r.Key,
		Text:    r.Text,
	}
}

// hitLess orders two hits by rel-path then chunk key — a total order over
// distinct chunks, used to canonicalize each pair and break score ties.
func hitLess(a, b Hit) bool { //nolint:gocritic // hugeParam: value semantics intentional; callers pass range-loop copies
	if a.RelPath != b.RelPath {
		return a.RelPath < b.RelPath
	}
	return a.Key < b.Key
}

func eqHit(a, b Hit) bool { //nolint:gocritic // hugeParam: value semantics intentional; callers pass range-loop copies
	return a.RelPath == b.RelPath && a.Key == b.Key
}
