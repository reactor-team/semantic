// Package graph builds the document link graph from the stored link edges:
// it resolves each raw link target (a relative path or a wikilink) against the
// set of indexed files, then answers the questions worth asking of a docs
// tree — what's orphaned, what links are broken, what points at a given file.
// Resolution lives here (not at index time) so it always reflects the current
// file set: adding or renaming a target fixes resolution on the next query
// without rewriting every source file's edges.
package graph

import (
	"net/url"
	"os"
	"path"
	"path/filepath"
	"sort"
	"strings"

	"github.com/reactor-team/semantic/pkg/chunk"
	"github.com/reactor-team/semantic/pkg/index"
)

// Source is the read side of the index the graph needs. *index.Store
// satisfies it; tests supply a fake.
type Source interface {
	AllFiles() ([]string, error)
	AllLinks() ([]index.LinkRow, error)
	FileHeadings() (map[string]map[string]bool, error)
}

// Edge is one outbound link with its target resolved to an indexed file, or
// left empty when the target resolves to nothing (a candidate broken link).
type Edge struct {
	From   string `json:"from"`
	To     string `json:"to,omitempty"`     // resolved rel-path; empty = unresolved
	Raw    string `json:"target"`           // raw destination as written
	Anchor string `json:"anchor,omitempty"` // #section fragment, if any
	Kind   string `json:"kind"`             // chunk.LinkMarkdown | chunk.LinkWiki
	Line   int    `json:"line"`
}

// DestWithAnchor renders the edge's target as written, re-attaching its
// #section anchor (stored apart from the path) for display.
func (e *Edge) DestWithAnchor() string {
	if e.Anchor == "" {
		return e.Raw
	}
	return e.Raw + "#" + e.Anchor
}

// Graph is the resolved document link graph.
type Graph struct {
	Files    []string
	Edges    []Edge
	headings map[string]map[string]bool // rel-path → set of section-anchor slugs
	resolver *Resolver                  // retained to tell directory links from dead links
}

// Build loads files and links from src and resolves every edge. root is the
// vault directory on disk; it lets the resolver recognize links to real
// directories that hold no indexed files (e.g. an assets folder) as valid
// rather than broken. Pass "" to resolve purely off the index (tests).
func Build(src Source, root string) (*Graph, error) {
	files, err := src.AllFiles()
	if err != nil {
		return nil, err
	}
	links, err := src.AllLinks()
	if err != nil {
		return nil, err
	}
	headings, err := src.FileHeadings()
	if err != nil {
		return nil, err
	}

	r := NewResolver(files)
	r.root = root
	edges := make([]Edge, 0, len(links))
	for _, l := range links {
		// LinkCode references (doc paths written as inline code) aren't real
		// edges — the lint layer owns them; keep them out of the graph.
		if l.Kind == chunk.LinkCode {
			continue
		}
		edges = append(edges, Edge{
			From:   l.SrcRelPath,
			To:     r.Resolve(l.SrcRelPath, l.Target, l.Kind),
			Raw:    l.Target,
			Anchor: l.Anchor,
			Kind:   l.Kind,
			Line:   l.Line,
		})
	}
	return &Graph{Files: files, Edges: edges, headings: headings, resolver: r}, nil
}

// Orphans returns markdown files with no inbound resolved link, sorted. Go
// files are excluded — they're not part of the document graph, so they'd all
// read as orphans. An orphan may be intentional (a top-level index nothing
// links to); the list is a cleanup prompt, not a verdict.
func (g *Graph) Orphans() []string {
	inbound := g.inboundCounts()
	var out []string
	for _, f := range g.Files {
		if chunk.IsMarkdown(f) && inbound[f] == 0 {
			out = append(out, f)
		}
	}
	sort.Strings(out)
	return out
}

// Broken returns edges whose doc-like target resolved to no file — dead links
// to fix. Links to assets (images, PDFs, other non-markdown extensions) are
// not flagged, since those targets aren't indexed by design. Ordered by
// source path then line (the order AllLinks returns).
func (g *Graph) Broken() []Edge {
	var out []Edge
	for _, e := range g.Edges {
		if e.To != "" || !isDocLike(e.Raw) {
			continue
		}
		// A markdown link to a directory (e.g. `../services/`) resolves to no
		// file but renders as that directory's listing on GitHub — valid, not
		// broken.
		if e.Kind == chunk.LinkMarkdown && g.resolver.IsDir(e.From, e.Raw) {
			continue
		}
		out = append(out, e)
	}
	return out
}

// BrokenAnchors returns edges that resolve to a file but whose #section
// fragment matches no heading in that file — a link pointed at the right doc
// but a stale or mistyped section. Edges with no anchor, or whose target file
// didn't resolve (already covered by Broken), are excluded. Ordered by source
// path then line (the order AllLinks returns).
func (g *Graph) BrokenAnchors() []Edge {
	var out []Edge
	for _, e := range g.Edges {
		if e.To == "" || e.Anchor == "" {
			continue
		}
		if !g.headings[e.To][chunk.AnchorSlug(e.Anchor)] {
			out = append(out, e)
		}
	}
	return out
}

// Backlinks returns the edges pointing at target (a rel-path), i.e. the files
// that link to it.
func (g *Graph) Backlinks(target string) []Edge {
	target = path.Clean(strings.TrimPrefix(target, "./"))
	var out []Edge
	for _, e := range g.Edges {
		if e.To == target {
			out = append(out, e)
		}
	}
	return out
}

// Stats summarizes the graph for the default report.
type Stats struct {
	Files         int
	Markdown      int
	Edges         int
	Resolved      int
	Broken        int
	BrokenAnchors int
	Orphans       int
}

// Stats computes the summary counts in one pass.
func (g *Graph) Stats() Stats {
	s := Stats{Files: len(g.Files), Edges: len(g.Edges)}
	for _, f := range g.Files {
		if chunk.IsMarkdown(f) {
			s.Markdown++
		}
	}
	for _, e := range g.Edges {
		if e.To != "" {
			s.Resolved++
		}
	}
	s.Broken = len(g.Broken())
	s.BrokenAnchors = len(g.BrokenAnchors())
	s.Orphans = len(g.Orphans())
	return s
}

func (g *Graph) inboundCounts() map[string]int {
	m := make(map[string]int)
	for _, e := range g.Edges {
		if e.To != "" {
			m[e.To]++
		}
	}
	return m
}

// Resolver maps raw link targets onto indexed files. It's exported so the lint
// layer resolves inline-code doc paths through the same logic as real links.
type Resolver struct {
	files  map[string]bool     // exact rel-paths
	byBase map[string][]string // lowercased basename-without-ext → rel-paths (for wikilinks)
	dirs   map[string]bool     // every directory containing an indexed file (all ancestors)
	root   string              // vault dir on disk for the IsDir stat check; "" disables it
}

// NewResolver builds a resolver over the given indexed file set.
func NewResolver(files []string) *Resolver {
	r := &Resolver{
		files:  make(map[string]bool, len(files)),
		byBase: make(map[string][]string),
		dirs:   make(map[string]bool),
	}
	for _, f := range files {
		r.files[f] = true
		b := strings.ToLower(baseNoExt(f))
		r.byBase[b] = append(r.byBase[b], f)
		for d := path.Dir(f); d != "." && d != "/"; d = path.Dir(d) {
			r.dirs[d] = true
		}
	}
	return r
}

// IsDir reports whether a markdown link target points at a directory (e.g.
// `../services/`). GitHub renders such a link as that directory's listing, so
// it isn't a broken link. It resolves the target to a cleaned rel-path and
// accepts it as a directory if: it's the tree root ("."); it holds an indexed
// file (the index-derived set); or — when a vault root is known — it exists on
// disk as a directory. The on-disk check is what catches directories that hold
// only unindexed content (an assets or data folder with no markdown).
func (r *Resolver) IsDir(src, target string) bool {
	cand := r.candidate(src, target)
	// "." is the tree root — a link resolving there (`../../`, `/`) lands on the
	// repo's top-level listing, always a valid directory.
	if cand == "." || r.dirs[cand] {
		return true
	}
	if r.root != "" {
		if info, err := os.Stat(filepath.Join(r.root, filepath.FromSlash(cand))); err == nil && info.IsDir() {
			return true
		}
	}
	return false
}

// Resolve returns the indexed file a raw target points at, or "" if none.
// Wikilinks resolve vault-root-relative (or by basename for a bare name).
// Inline-code paths try the same vault-root-relative/basename resolution
// first, then fall back to resolving relative to the source file's own
// directory — many docs write an inline-code path that way (e.g. a module's
// own README mentioning "pkg/foo.go" to mean its own pkg/foo.go), the same
// convention markdown links already use. Markdown links resolve relative to
// the source file's dir only.
func (r *Resolver) Resolve(src, target, kind string) string {
	switch kind {
	case chunk.LinkWiki:
		return r.resolveWiki(target)
	case chunk.LinkCode:
		if to := r.resolveWiki(target); to != "" {
			return to
		}
		return r.resolveRel(src, target)
	}
	return r.resolveRel(src, target)
}

// resolveRel resolves a markdown link relative to the source file's directory,
// trying markdown extensions when the target has none. The target is a URL, so
// it's percent-decoded first (`Dynamic%20Models.md` → `Dynamic Models.md`),
// matching how GitHub follows the link. A leading '/' is root-absolute:
// resolved from the index root rather than the source dir.
func (r *Resolver) resolveRel(src, target string) string {
	cand := r.candidate(src, target)
	for _, c := range extCandidates(cand) {
		if r.files[c] {
			return c
		}
	}
	return ""
}

// candidate reduces a markdown link target to the cleaned rel-path it points
// at: percent-decoded, with a leading '/' meaning root-absolute (from the
// index root) and everything else relative to the source file's dir.
func (r *Resolver) candidate(src, target string) string {
	target = pathUnescape(target)
	if after, ok := strings.CutPrefix(target, "/"); ok {
		return path.Clean(after)
	}
	return path.Clean(path.Join(path.Dir(src), target))
}

// resolveWiki resolves an Obsidian wikilink. A target with a slash is treated
// as a vault-root-relative path; a bare name matches any file's basename
// (case-insensitive, Obsidian-style). Ambiguous bare names resolve to the
// first match in sorted order — deterministic, if arbitrary.
func (r *Resolver) resolveWiki(target string) string {
	if strings.Contains(target, "/") {
		cand := path.Clean(strings.TrimPrefix(target, "/"))
		for _, c := range extCandidates(cand) {
			if r.files[c] {
				return c
			}
		}
		return ""
	}
	if m := r.byBase[strings.ToLower(baseNoExt(target))]; len(m) > 0 {
		return m[0]
	}
	return ""
}

// Candidates reports every indexed file a bare basename (no directory,
// e.g. "service.go") could match, filtered to the same extension as target —
// exposing when resolveWiki's first-match pick among same-named files across
// different directories is arbitrary rather than a genuine single answer.
// Always empty for a target with a directory component, since that resolves
// to at most one exact path. Exported for the lint layer, which flags a
// multi-candidate bare-basename LinkCode reference as ambiguous instead of
// silently promoting it to whichever file sorts first.
func (r *Resolver) Candidates(target string) []string {
	if strings.Contains(target, "/") {
		return nil
	}
	ext := strings.ToLower(path.Ext(target))
	var out []string
	for _, c := range r.byBase[strings.ToLower(baseNoExt(target))] {
		if strings.ToLower(path.Ext(c)) == ext {
			out = append(out, c)
		}
	}
	return out
}

// extCandidates returns the path plus markdown-extension variants when it has
// none, so [x](foo) and [x](foo.md) both resolve to foo.md.
func extCandidates(p string) []string {
	if path.Ext(p) != "" {
		return []string{p}
	}
	return []string{p, p + ".md", p + ".markdown"}
}

func baseNoExt(p string) string {
	b := path.Base(p)
	return strings.TrimSuffix(b, path.Ext(b))
}

// pathUnescape percent-decodes a markdown link target (GitHub treats the
// destination as a URL). A malformed escape falls back to the raw target
// rather than dropping the edge; a target with no '%' is returned untouched.
func pathUnescape(s string) string {
	if !strings.Contains(s, "%") {
		return s
	}
	if dec, err := url.PathUnescape(s); err == nil {
		return dec
	}
	return s
}

// isDocLike reports whether a raw target looks like a document or source-code
// reference (no extension, markdown, or Go) — the only unresolved targets
// worth reporting as broken. Links to .png/.pdf/etc. are assets, not dead doc
// links.
func isDocLike(target string) bool {
	if i := strings.IndexAny(target, "#?"); i >= 0 {
		target = target[:i]
	}
	ext := strings.ToLower(path.Ext(target))
	return ext == "" || ext == ".go" || chunk.IsMarkdown(target)
}
