// Package search runs a semantic query against the index: embed the query,
// cosine-rank it against every stored chunk, and return the top matches.
// At personal-vault scale a brute-force scan in memory is well under a
// millisecond, so there is no ANN index — just a sort.
package search

import (
	"fmt"
	"slices"
	"sort"
	"strings"

	"github.com/reactor-team/semantic/pkg/chunk"
	"github.com/reactor-team/semantic/pkg/embed"
	"github.com/reactor-team/semantic/pkg/index"
)

// Kind narrows a search to one class of indexed content. Docs and Code
// partition the index: markdown is docs, every other indexed language is code.
// For a narrower cut than that one bit, see Options.Langs.
type Kind int

// KindAny, KindDocs, and KindCode are the values of Kind.
const (
	KindAny  Kind = iota // no type filter
	KindDocs             // markdown only
	KindCode             // source code only (non-markdown)
)

// matches reports whether a file's rel-path belongs to the requested kind.
func (k Kind) matches(relPath string) bool {
	isMarkdown := chunk.IsMarkdown(relPath)
	switch k {
	case KindDocs:
		return isMarkdown
	case KindCode:
		return !isMarkdown
	}
	return true
}

// ChunkSource is the read side of the index that search needs. *index.Store
// satisfies it; tests supply a fake so search runs without a database.
type ChunkSource interface {
	AllChunks(pathPrefix string) ([]index.ChunkRow, error)
}

// Embedder turns the query text into a vector. Injected so search runs
// without the ONNX runtime; production passes embed.Get.
type Embedder func(text string) (embed.Vec, error)

// Options tunes a query.
type Options struct {
	Limit      int     // max hits to return; <= 0 means no cap
	PathPrefix string  // restrict to files under this rel-path prefix
	MinScore   float64 // drop hits scoring below this cosine value
	Collapse   bool    // keep only the best-scoring chunk per file
	Kind       Kind    // restrict to docs or code; KindAny searches both

	// Langs restricts results to these canonical language names (see
	// chunk.NormalizeLanguage). Empty means every language. This is a finer
	// cut than Kind: "code" is one bit, but with eighteen languages indexed
	// the useful question is usually "the Python one" or "the manifests".
	Langs []string
}

// matchesLang reports whether a file belongs to one of the requested
// languages. Callers pass canonical names; an unresolvable one is rejected at
// the CLI, so a typo cannot silently narrow a search to nothing here.
func (o Options) matchesLang(relPath string) bool {
	if len(o.Langs) == 0 {
		return true
	}
	return slices.Contains(o.Langs, chunk.LanguageName(relPath))
}

// Hit is one ranked result.
type Hit struct {
	RelPath string  `json:"path"`
	Line    int     `json:"line"`
	Heading string  `json:"heading,omitempty"`
	Variant string  `json:"variant"`
	Key     string  `json:"key"`
	Score   float64 `json:"score"`
	Text    string  `json:"text"`
}

// Query embeds q, ranks it against the index chunks, and returns hits
// sorted by descending cosine score. An empty query is an error; an empty
// index is not (it yields no hits).
func Query(src ChunkSource, embedFn Embedder, q string, opts Options) ([]Hit, error) {
	q = strings.TrimSpace(q)
	if q == "" {
		return nil, fmt.Errorf("empty query")
	}

	qv, err := embedFn(q)
	if err != nil {
		return nil, fmt.Errorf("embedding query: %w", err)
	}

	rows, err := src.AllChunks(opts.PathPrefix)
	if err != nil {
		return nil, err
	}

	hits := make([]Hit, 0, len(rows))
	for _, r := range rows {
		if !opts.Kind.matches(r.RelPath) || !opts.matchesLang(r.RelPath) {
			continue
		}
		score := embed.CosineSim(qv, r.Vec)
		if score < opts.MinScore {
			continue
		}
		hits = append(hits, Hit{
			RelPath: r.RelPath,
			Line:    r.Line,
			Heading: r.Heading,
			Variant: r.Variant,
			Key:     r.Key,
			Score:   score,
			Text:    r.Text,
		})
	}

	// Sort by score desc, with a stable tie-break so equal scores don't
	// reorder run-to-run (matters for the collapse pass and for tests).
	sort.Slice(hits, func(i, j int) bool {
		if hits[i].Score != hits[j].Score {
			return hits[i].Score > hits[j].Score
		}
		if hits[i].RelPath != hits[j].RelPath {
			return hits[i].RelPath < hits[j].RelPath
		}
		return hits[i].Key < hits[j].Key
	})

	if opts.Collapse {
		hits = collapseByFile(hits)
	}
	if opts.Limit > 0 && len(hits) > opts.Limit {
		hits = hits[:opts.Limit]
	}
	return hits, nil
}

// collapseByFile keeps only the first (highest-scoring, since hits is
// pre-sorted) hit for each file. When that winner is a breadcrumb-only /path
// chunk — which carries no body to display — it is promoted to the best
// content-bearing sibling under the same heading, so every collapsed result
// shows a real block rather than a bare heading trail.
func collapseByFile(hits []Hit) []Hit {
	seen := make(map[string]bool, len(hits))
	out := make([]Hit, 0, len(hits))
	for _, h := range hits {
		if seen[h.RelPath] {
			continue
		}
		seen[h.RelPath] = true
		if h.Variant == chunk.VariantPath {
			if sib, ok := bestContentSibling(hits, h); ok {
				h = sib
			}
		}
		out = append(out, h)
	}
	return out
}

// bestContentSibling finds the highest-scoring content chunk (narrow/full,
// anything but /path) sharing the /path hit's file and heading. hits is
// pre-sorted by score, so the first match is the best. Returns ok=false for a
// heading with no body of its own (only subheadings) — the caller then keeps
// the /path hit and renders the header alone.
func bestContentSibling(hits []Hit, p Hit) (Hit, bool) { //nolint:gocritic // hugeParam: value semantics intentional; callers pass range-loop copies
	for _, h := range hits {
		if h.RelPath == p.RelPath && h.Heading == p.Heading && h.Variant != chunk.VariantPath {
			return h, true
		}
	}
	return Hit{}, false
}
