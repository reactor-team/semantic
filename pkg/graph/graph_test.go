package graph

import (
	"os"
	"path/filepath"
	"reflect"
	"testing"

	"github.com/reactor-team/semantic/pkg/chunk"
	"github.com/reactor-team/semantic/pkg/index"
)

// fakeSource feeds Build a canned file set, link set, and per-file heading slugs.
type fakeSource struct {
	files    []string
	links    []index.LinkRow
	headings map[string]map[string]bool
}

func (f *fakeSource) AllFiles() ([]string, error)        { return f.files, nil }
func (f *fakeSource) AllLinks() ([]index.LinkRow, error) { return f.links, nil }
func (f *fakeSource) FileHeadings() (map[string]map[string]bool, error) {
	return f.headings, nil
}

func md(src, target, kind string) index.LinkRow {
	return index.LinkRow{SrcRelPath: src, Target: target, Kind: kind}
}

func TestBuild_ResolvesEdges(t *testing.T) {
	t.Parallel()
	src := &fakeSource{
		files: []string{"a.md", "docs/b.md", "docs/c.md", "docs/Dynamic Models.md", "docs/setup.md"},
		links: []index.LinkRow{
			md("a.md", "docs/b.md", chunk.LinkMarkdown),                // root-relative-ish via dir(a)="."
			md("docs/b.md", "c.md", chunk.LinkMarkdown),                // sibling, extensionful
			md("docs/b.md", "../a", chunk.LinkMarkdown),                // up a dir, extensionless → a.md
			md("docs/c.md", "b", chunk.LinkWiki),                       // wiki by basename → docs/b.md
			md("a.md", "ghost.md", chunk.LinkMarkdown),                 // broken
			md("docs/b.md", "Dynamic%20Models.md", chunk.LinkMarkdown), // percent-encoded space → decoded
			md("deep/x.md", "/docs/setup.md", chunk.LinkMarkdown),      // leading '/' → resolved from root
		},
	}
	g, err := Build(src, "")
	if err != nil {
		t.Fatal(err)
	}

	resolved := map[string]string{} // "from|raw" → to
	for _, e := range g.Edges {
		resolved[e.From+"|"+e.Raw] = e.To
	}
	cases := map[string]string{
		"a.md|docs/b.md":                "docs/b.md",
		"docs/b.md|c.md":                "docs/c.md",
		"docs/b.md|../a":                "a.md",
		"docs/c.md|b":                   "docs/b.md",
		"a.md|ghost.md":                 "", // unresolved
		"docs/b.md|Dynamic%20Models.md": "docs/Dynamic Models.md",
		"deep/x.md|/docs/setup.md":      "docs/setup.md",
	}
	for k, want := range cases {
		if got := resolved[k]; got != want {
			t.Errorf("edge %q resolved to %q, want %q", k, got, want)
		}
	}
}

func TestGraph_BrokenOrphansBacklinks(t *testing.T) {
	t.Parallel()
	src := &fakeSource{
		files: []string{"index.md", "a.md", "b.md", "lonely.md", "util.go"},
		links: []index.LinkRow{
			md("index.md", "a.md", chunk.LinkMarkdown),
			md("index.md", "b.md", chunk.LinkMarkdown),
			md("a.md", "b.md", chunk.LinkMarkdown),
			md("a.md", "gone.md", chunk.LinkMarkdown),     // broken (doc-like)
			md("a.md", "diagram.png", chunk.LinkMarkdown), // unresolved asset — NOT broken
		},
	}
	g, err := Build(src, "")
	if err != nil {
		t.Fatal(err)
	}

	// Broken: only the doc-like unresolved target.
	broken := g.Broken()
	if len(broken) != 1 || broken[0].Raw != "gone.md" {
		t.Fatalf("broken = %+v, want just gone.md", broken)
	}

	// Orphans: markdown with no inbound link. index.md (nothing links to it)
	// and lonely.md; a.md and b.md have inbound; util.go is not markdown.
	orphans := g.Orphans()
	want := []string{"index.md", "lonely.md"}
	if !reflect.DeepEqual(orphans, want) {
		t.Errorf("orphans = %v, want %v", orphans, want)
	}

	// Backlinks to b.md: index.md and a.md.
	back := g.Backlinks("b.md")
	if len(back) != 2 {
		t.Fatalf("backlinks to b.md = %d, want 2", len(back))
	}
	froms := map[string]bool{}
	for _, e := range back {
		froms[e.From] = true
	}
	if !froms["index.md"] || !froms["a.md"] {
		t.Errorf("backlinks from = %v, want index.md and a.md", froms)
	}
}

func TestGraph_BrokenGoSourceLink(t *testing.T) {
	t.Parallel()
	src := &fakeSource{
		files: []string{"a.md", "internal/graph/graph.go"},
		links: []index.LinkRow{
			md("a.md", "internal/graph/graph.go", chunk.LinkMarkdown), // resolves
			md("a.md", "internal/graph/gone.go", chunk.LinkMarkdown),  // dead .go path — broken
		},
	}
	g, err := Build(src, "")
	if err != nil {
		t.Fatal(err)
	}
	broken := g.Broken()
	if len(broken) != 1 || broken[0].Raw != "internal/graph/gone.go" {
		t.Fatalf("broken = %+v, want just internal/graph/gone.go", broken)
	}
}

func TestResolver_Candidates(t *testing.T) {
	t.Parallel()
	r := NewResolver([]string{
		"services/settings/service.go",
		"services/babysitter/service.go",
		"docs/service.md",
		"internal/lint/lint.go",
	})

	if got := r.Candidates("service.go"); len(got) != 2 {
		t.Fatalf("Candidates(service.go) = %v, want the 2 same-extension matches", got)
	}
	// A directory component always resolves to at most one exact path, never
	// ambiguous.
	if got := r.Candidates("services/settings/service.go"); got != nil {
		t.Fatalf("Candidates with a directory = %v, want nil", got)
	}
	// docs/service.md shares a basename stem with service.go but not its
	// extension, so it's not a candidate for a .go reference.
	if got := r.Candidates("lint.go"); len(got) != 1 || got[0] != "internal/lint/lint.go" {
		t.Fatalf("Candidates(lint.go) = %v, want just internal/lint/lint.go", got)
	}
	if got := r.Candidates("missing.go"); got != nil {
		t.Fatalf("Candidates(missing.go) = %v, want nil", got)
	}
}

func TestGraph_DirectoryLinksNotBroken(t *testing.T) {
	t.Parallel()
	src := &fakeSource{
		files: []string{"index.md", "services/api/x.md", "guides/setup.md"},
		links: []index.LinkRow{
			md("index.md", "services/", chunk.LinkMarkdown),          // trailing-slash dir link → valid
			md("index.md", "services/api", chunk.LinkMarkdown),       // dir, no slash → valid
			md("guides/setup.md", "../services", chunk.LinkMarkdown), // relative dir link → valid
			md("services/api/x.md", "../../", chunk.LinkMarkdown),    // link to tree root → valid
			md("index.md", "missingdir/", chunk.LinkMarkdown),        // not a real dir → broken
			md("index.md", "gone.md", chunk.LinkMarkdown),            // dead file → broken
		},
	}
	g, err := Build(src, "")
	if err != nil {
		t.Fatal(err)
	}
	broken := g.Broken()
	got := map[string]bool{}
	for _, e := range broken {
		got[e.Raw] = true
	}
	if !got["missingdir/"] || !got["gone.md"] {
		t.Errorf("broken = %+v, want missingdir/ and gone.md", broken)
	}
	if got["services/"] || got["services/api"] || got["../services"] || got["../../"] {
		t.Errorf("directory links must not be reported broken; got %+v", broken)
	}
}

func TestGraph_OnDiskDirectoryLinkNotBroken(t *testing.T) {
	t.Parallel()
	// A directory that exists on disk but holds no indexed files (only data
	// artifacts) — the index-derived dir set can't see it, so resolution falls
	// back to a filesystem stat against the vault root.
	root := t.TempDir()
	if err := os.MkdirAll(filepath.Join(root, "data", "profiling-run"), 0o755); err != nil {
		t.Fatal(err)
	}
	src := &fakeSource{
		files: []string{"index.md"},
		links: []index.LinkRow{
			md("index.md", "data/profiling-run/", chunk.LinkMarkdown), // real on-disk dir → valid
			md("index.md", "data/nope/", chunk.LinkMarkdown),          // no such dir → broken
		},
	}
	g, err := Build(src, root)
	if err != nil {
		t.Fatal(err)
	}
	broken := g.Broken()
	if len(broken) != 1 || broken[0].Raw != "data/nope/" {
		t.Fatalf("broken = %+v, want just data/nope/", broken)
	}
}

func TestBuild_ExcludesCodeRefs(t *testing.T) {
	t.Parallel()
	src := &fakeSource{
		files: []string{"a.md", "b.md"},
		links: []index.LinkRow{
			md("a.md", "b.md", chunk.LinkCode), // inline-code path — not a real edge
		},
	}
	g, err := Build(src, "")
	if err != nil {
		t.Fatal(err)
	}
	if len(g.Edges) != 0 {
		t.Errorf("code-kind links must not become edges; got %+v", g.Edges)
	}
	// b.md would have an inbound edge if code refs counted; it stays orphaned.
	if orphans := g.Orphans(); len(orphans) != 2 {
		t.Errorf("orphans = %v, want both a.md and b.md", orphans)
	}
}

func TestGraph_BrokenAnchors(t *testing.T) {
	t.Parallel()
	anchor := func(src, target, frag, kind string) index.LinkRow {
		return index.LinkRow{SrcRelPath: src, Target: target, Anchor: frag, Kind: kind}
	}
	src := &fakeSource{
		files: []string{"a.md", "docs/b.md"},
		links: []index.LinkRow{
			anchor("a.md", "docs/b.md", "setup", chunk.LinkMarkdown),   // valid section
			anchor("a.md", "docs/b.md", "missing", chunk.LinkMarkdown), // no such section
			anchor("a.md", "b", "Getting Started", chunk.LinkWiki),     // wiki, heading-text form → slug matches
			md("a.md", "docs/b.md", chunk.LinkMarkdown),                // no anchor — never flagged
			anchor("a.md", "gone.md", "setup", chunk.LinkMarkdown),     // file missing → Broken, not BrokenAnchors
		},
		headings: map[string]map[string]bool{
			"docs/b.md": {"setup": true, "getting-started": true},
		},
	}
	g, err := Build(src, "")
	if err != nil {
		t.Fatal(err)
	}

	bad := g.BrokenAnchors()
	if len(bad) != 1 {
		t.Fatalf("broken anchors = %+v, want just the #missing one", bad)
	}
	if bad[0].Raw != "docs/b.md" || bad[0].Anchor != "missing" {
		t.Errorf("broken anchor = %+v, want docs/b.md#missing", bad[0])
	}

	// The file-missing edge is a broken link, not a broken anchor.
	if broken := g.Broken(); len(broken) != 1 || broken[0].Raw != "gone.md" {
		t.Errorf("broken = %+v, want just gone.md", broken)
	}
	if s := g.Stats(); s.BrokenAnchors != 1 {
		t.Errorf("stats.BrokenAnchors = %d, want 1", s.BrokenAnchors)
	}
}

func TestGraph_Stats(t *testing.T) {
	t.Parallel()
	src := &fakeSource{
		files: []string{"a.md", "b.md", "x.go"},
		links: []index.LinkRow{
			md("a.md", "b.md", chunk.LinkMarkdown),    // resolved
			md("a.md", "gone.md", chunk.LinkMarkdown), // broken
		},
	}
	g, err := Build(src, "")
	if err != nil {
		t.Fatal(err)
	}
	s := g.Stats()
	if s.Files != 3 || s.Markdown != 2 {
		t.Errorf("files=%d markdown=%d, want 3/2", s.Files, s.Markdown)
	}
	if s.Edges != 2 || s.Resolved != 1 || s.Broken != 1 {
		t.Errorf("edges=%d resolved=%d broken=%d, want 2/1/1", s.Edges, s.Resolved, s.Broken)
	}
	// a.md has an inbound? no. b.md has inbound from a.md. So orphans = a.md.
	if s.Orphans != 1 {
		t.Errorf("orphans=%d, want 1", s.Orphans)
	}
}
