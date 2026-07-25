package index

import (
	"crypto/sha256"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/reactor-team/semantic/pkg/embed"
)

// fakeEmbed is a deterministic stand-in for embed.Get: same text → same
// vector, no ONNX runtime required.
func fakeEmbed(text string) (embed.Vec, error) {
	sum := sha256.Sum256([]byte(text))
	v := make(embed.Vec, 4)
	for i := range v {
		v[i] = float32(sum[i]) / 255.0
	}
	return v, nil
}

func writeFile(t *testing.T, dir, rel, content string) string {
	t.Helper()
	p := filepath.Join(dir, filepath.FromSlash(rel))
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(p, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
	return p
}

func openTemp(t *testing.T) *Store {
	t.Helper()
	s, err := Open(filepath.Join(t.TempDir(), "index.db"))
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	t.Cleanup(func() { s.Close() })
	return s
}

func TestReindex_FreshAddsFilesAndChunks(t *testing.T) {
	t.Parallel()
	vault := t.TempDir()
	writeFile(t, vault, "a.md", "# Alpha\n\nFirst note about widgets.\n")
	writeFile(t, vault, "sub/b.md", "# Beta\n\nSecond note about gadgets.\n")

	s := openTemp(t)
	rep, err := s.Reindex(vault, fakeEmbed, false)
	if err != nil {
		t.Fatalf("Reindex: %v", err)
	}
	if rep.Added != 2 || rep.Updated != 0 || rep.Deleted != 0 {
		t.Errorf("counts = +%d ~%d -%d, want +2 ~0 -0", rep.Added, rep.Updated, rep.Deleted)
	}
	if rep.Files != 2 || rep.Chunks == 0 {
		t.Errorf("index has %d files / %d chunks, want 2 files and some chunks", rep.Files, rep.Chunks)
	}
}

func TestReindex_UnchangedIsSkipped(t *testing.T) {
	t.Parallel()
	vault := t.TempDir()
	writeFile(t, vault, "a.md", "# Alpha\n\nStable content.\n")
	s := openTemp(t)
	if _, err := s.Reindex(vault, fakeEmbed, false); err != nil {
		t.Fatal(err)
	}

	rep, err := s.Reindex(vault, fakeEmbed, false)
	if err != nil {
		t.Fatal(err)
	}
	if rep.Added != 0 || rep.Updated != 0 || rep.Unchanged != 1 {
		t.Errorf("second run = +%d ~%d =%d, want +0 ~0 =1", rep.Added, rep.Updated, rep.Unchanged)
	}
}

func TestReindex_ForceReembedsUnchanged(t *testing.T) {
	t.Parallel()
	vault := t.TempDir()
	writeFile(t, vault, "a.md", "# Alpha\n\nStable content.\n")
	s := openTemp(t)
	if _, err := s.Reindex(vault, fakeEmbed, false); err != nil {
		t.Fatal(err)
	}

	// Content on disk is untouched, but force must re-chunk/re-embed anyway —
	// the escape hatch for a chunker/extractor change that a content hash
	// can't detect on its own.
	rep, err := s.Reindex(vault, fakeEmbed, true)
	if err != nil {
		t.Fatal(err)
	}
	if rep.Updated != 1 || rep.Unchanged != 0 {
		t.Errorf("forced run = ~%d =%d, want ~1 =0", rep.Updated, rep.Unchanged)
	}
}

func TestReindex_ChangedContentReembeds(t *testing.T) {
	t.Parallel()
	vault := t.TempDir()
	p := writeFile(t, vault, "a.md", "# Alpha\n\nOriginal.\n")
	s := openTemp(t)
	if _, err := s.Reindex(vault, fakeEmbed, false); err != nil {
		t.Fatal(err)
	}

	// Rewrite with different content; bump mtime to be safe.
	if err := os.WriteFile(p, []byte("# Alpha\n\nCompletely different prose now.\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	future := time.Now().Add(2 * time.Second)
	if err := os.Chtimes(p, future, future); err != nil {
		t.Fatal(err)
	}

	rep, err := s.Reindex(vault, fakeEmbed, false)
	if err != nil {
		t.Fatal(err)
	}
	if rep.Updated != 1 || rep.Added != 0 {
		t.Errorf("changed run = +%d ~%d, want +0 ~1", rep.Added, rep.Updated)
	}

	// The new text must be searchable, the old text gone.
	chunks, err := s.AllChunks("")
	if err != nil {
		t.Fatal(err)
	}
	var haveNew, haveOld bool
	for _, c := range chunks {
		if strings.Contains(c.Text, "Completely different prose") {
			haveNew = true
		}
		if strings.Contains(c.Text, "Original.") {
			haveOld = true
		}
	}
	if !haveNew || haveOld {
		t.Errorf("after change: haveNew=%v haveOld=%v, want true/false", haveNew, haveOld)
	}
}

func TestReindex_TouchedButIdenticalNotReembedded(t *testing.T) {
	t.Parallel()
	vault := t.TempDir()
	p := writeFile(t, vault, "a.md", "# Alpha\n\nSame bytes.\n")
	s := openTemp(t)
	if _, err := s.Reindex(vault, fakeEmbed, false); err != nil {
		t.Fatal(err)
	}

	// Change mtime only — content identical. The hash tier should catch it.
	future := time.Now().Add(2 * time.Second)
	if err := os.Chtimes(p, future, future); err != nil {
		t.Fatal(err)
	}
	rep, err := s.Reindex(vault, fakeEmbed, false)
	if err != nil {
		t.Fatal(err)
	}
	if rep.Updated != 0 || rep.Unchanged != 1 {
		t.Errorf("touch run = ~%d =%d, want ~0 =1", rep.Updated, rep.Unchanged)
	}
}

func TestReindex_SkipEmbedStoresPlaceholderVecs(t *testing.T) {
	t.Parallel()
	vault := t.TempDir()
	writeFile(t, vault, "a.md", "# Alpha\n\nSome prose.\n")
	s := openTemp(t)

	rep, err := s.Reindex(vault, nil, false)
	if err != nil {
		t.Fatal(err)
	}
	if rep.Added != 1 {
		t.Fatalf("added = %d, want 1", rep.Added)
	}
	chunks, err := s.AllChunks("")
	if err != nil {
		t.Fatal(err)
	}
	for _, c := range chunks {
		if len(c.Vec) != 0 {
			t.Errorf("chunk %s: vec len %d, want 0 (skip-embed placeholder)", c.Key, len(c.Vec))
		}
	}
}

func TestReindex_HealsPlaceholderVecsOnNextRealEmbed(t *testing.T) {
	t.Parallel()
	vault := t.TempDir()
	writeFile(t, vault, "a.md", "# Alpha\n\nSame bytes throughout.\n")
	s := openTemp(t)

	if _, err := s.Reindex(vault, nil, false); err != nil {
		t.Fatal(err)
	}
	// Stat and content are unchanged on disk, but the placeholder vecs must
	// force a real re-embed rather than taking the stat/hash fast path.
	rep, err := s.Reindex(vault, fakeEmbed, false)
	if err != nil {
		t.Fatal(err)
	}
	if rep.Updated != 1 || rep.Unchanged != 0 {
		t.Errorf("heal run = ~%d =%d, want ~1 =0", rep.Updated, rep.Unchanged)
	}
	chunks, err := s.AllChunks("")
	if err != nil {
		t.Fatal(err)
	}
	for _, c := range chunks {
		if len(c.Vec) == 0 {
			t.Errorf("chunk %s: still has placeholder vec after heal", c.Key)
		}
	}

	// A subsequent real-embed pass now takes the ordinary fast path again —
	// healing must not leave the file re-embedding on every run.
	rep2, err := s.Reindex(vault, fakeEmbed, false)
	if err != nil {
		t.Fatal(err)
	}
	if rep2.Unchanged != 1 || rep2.Updated != 0 {
		t.Errorf("post-heal run = ~%d =%d, want ~0 =1", rep2.Updated, rep2.Unchanged)
	}
}

func TestReindex_HealsPlaceholderVecsAfterTouch(t *testing.T) {
	t.Parallel()
	vault := t.TempDir()
	p := writeFile(t, vault, "a.md", "# Alpha\n\nSame bytes.\n")
	s := openTemp(t)
	if _, err := s.Reindex(vault, nil, false); err != nil {
		t.Fatal(err)
	}

	// mtime changes but content doesn't — normally caught by the hash tier and
	// skipped, but the placeholder vec must still force a real embed.
	future := time.Now().Add(2 * time.Second)
	if err := os.Chtimes(p, future, future); err != nil {
		t.Fatal(err)
	}
	rep, err := s.Reindex(vault, fakeEmbed, false)
	if err != nil {
		t.Fatal(err)
	}
	if rep.Updated != 1 || rep.Unchanged != 0 {
		t.Errorf("heal-after-touch run = ~%d =%d, want ~1 =0", rep.Updated, rep.Unchanged)
	}
	chunks, err := s.AllChunks("")
	if err != nil {
		t.Fatal(err)
	}
	for _, c := range chunks {
		if len(c.Vec) == 0 {
			t.Errorf("chunk %s: still placeholder after heal-after-touch", c.Key)
		}
	}
}

func TestReindex_DeletedFileCascades(t *testing.T) {
	t.Parallel()
	vault := t.TempDir()
	writeFile(t, vault, "keep.md", "# Keep\n\nStays.\n")
	p := writeFile(t, vault, "drop.md", "# Drop\n\nGoes away, but first [links](keep.md).\n")
	s := openTemp(t)
	if _, err := s.Reindex(vault, fakeEmbed, false); err != nil {
		t.Fatal(err)
	}

	if err := os.Remove(p); err != nil {
		t.Fatal(err)
	}
	rep, err := s.Reindex(vault, fakeEmbed, false)
	if err != nil {
		t.Fatal(err)
	}
	if rep.Deleted != 1 || rep.Files != 1 {
		t.Errorf("delete run = -%d, files=%d, want -1, files=1", rep.Deleted, rep.Files)
	}
	// No orphan chunks from the dropped file.
	chunks, err := s.AllChunks("")
	if err != nil {
		t.Fatal(err)
	}
	for _, c := range chunks {
		if c.RelPath == "drop.md" {
			t.Errorf("chunk from deleted file survived: %+v", c)
		}
	}
	// Its outbound links cascade away too (links.src_file_id ON DELETE CASCADE).
	links, err := s.AllLinks()
	if err != nil {
		t.Fatal(err)
	}
	for _, l := range links {
		if l.SrcRelPath == "drop.md" {
			t.Errorf("link from deleted file survived: %+v", l)
		}
	}
}

func TestReindex_NewlyIgnoredFileIsPruned(t *testing.T) {
	t.Parallel()
	vault := t.TempDir()
	writeFile(t, vault, "keep.md", "# Keep\n\nStays.\n")
	writeFile(t, vault, "secret.md", "# Secret\n\nIndexed before it was ignored.\n")

	s := openTemp(t)
	if _, err := s.Reindex(vault, fakeEmbed, false); err != nil {
		t.Fatal(err)
	}
	if got := indexedFiles(t, s); !got["secret.md"] {
		t.Fatalf("secret.md should be indexed on the first run; got %v", got)
	}

	// It later lands in .gitignore: the next reindex must prune it just like a
	// delete — it's no longer a seen, indexable file, so deleteMissing removes it.
	writeFile(t, vault, ".gitignore", "secret.md\n")
	rep, err := s.Reindex(vault, fakeEmbed, false)
	if err != nil {
		t.Fatal(err)
	}
	if rep.Deleted != 1 {
		t.Errorf("newly-ignored file should be pruned: deleted=%d, want 1", rep.Deleted)
	}
	if got := indexedFiles(t, s); got["secret.md"] {
		t.Errorf("ignored file survived in the index: %v", got)
	}
}

func TestReindex_AlwaysSkipsWithoutGitignore(t *testing.T) {
	t.Parallel()
	vault := t.TempDir()
	writeFile(t, vault, "real.md", "# Real\n\nIndexed.\n")
	writeFile(t, vault, "node_modules/pkg/readme.md", "# Dep\n\nAlways skipped.\n")
	writeFile(t, vault, ".git/hooks.md", "# Hooks\n\nAlways skipped.\n")
	// Without a .gitignore, a plain dotdir is no longer heuristically skipped —
	// git is the only ignore source, so it gets indexed.
	writeFile(t, vault, ".obsidian/notes.md", "# Notes\n\nNow indexed.\n")

	s := openTemp(t)
	if _, err := s.Reindex(vault, fakeEmbed, false); err != nil {
		t.Fatal(err)
	}
	got := indexedFiles(t, s)
	if !got["real.md"] || !got[".obsidian/notes.md"] {
		t.Errorf("want real.md and .obsidian/notes.md indexed; got %v", got)
	}
	for _, skip := range []string{"node_modules/pkg/readme.md", ".git/hooks.md"} {
		if got[skip] {
			t.Errorf("always-skip dir was indexed: %s", skip)
		}
	}
}

func TestReindex_IndexesMarkdownAndCode(t *testing.T) {
	t.Parallel()
	vault := t.TempDir()
	writeFile(t, vault, "notes.md", "# Notes\n\nProse.\n")
	writeFile(t, vault, "server.go", "// Package srv serves.\npackage srv\n\n// Run starts the server.\nfunc Run() error { return nil }\n")

	s := openTemp(t)
	rep, err := s.Reindex(vault, fakeEmbed, false)
	if err != nil {
		t.Fatal(err)
	}
	if rep.Files != 2 {
		t.Fatalf("indexed %d files, want 2 (markdown + Go)", rep.Files)
	}

	chunks, err := s.AllChunks("server.go")
	if err != nil {
		t.Fatal(err)
	}
	var haveFunc bool
	for _, c := range chunks {
		if c.Key == "go/func/Run" && c.Variant == "func" {
			haveFunc = true
		}
	}
	if !haveFunc {
		t.Errorf("expected a go/func/Run chunk from server.go, got %d chunks", len(chunks))
	}
}

func TestReindex_IndexesTypeScript(t *testing.T) {
	t.Parallel()
	vault := t.TempDir()
	writeFile(t, vault, "server.ts", "/** Run starts the server. */\nexport function run(): boolean { return true; }\n")
	writeFile(t, vault, "button.tsx", "/** Button renders. */\nexport function Button() { return <button/>; }\n")
	writeFile(t, vault, "util.js", "/** add sums two numbers. */\nexport function add(a, b) { return a + b; }\n")
	writeFile(t, vault, "card.jsx", "/** Card renders. */\nexport const Card = () => <div/>;\n")

	s := openTemp(t)
	rep, err := s.Reindex(vault, fakeEmbed, false)
	if err != nil {
		t.Fatal(err)
	}
	if rep.Files != 4 {
		t.Fatalf("indexed %d files, want 4 (.ts + .tsx + .js + .jsx)", rep.Files)
	}

	chunks, err := s.AllChunks("server.ts")
	if err != nil {
		t.Fatal(err)
	}
	var haveFunc bool
	for _, c := range chunks {
		if c.Key == "ts/func/run" && c.Variant == "func" {
			haveFunc = true
		}
	}
	if !haveFunc {
		t.Errorf("expected a ts/func/run chunk from server.ts, got %d chunks", len(chunks))
	}

	// The .tsx/.jsx files go through the JSX-aware grammar; .js is plain
	// JavaScript through the same grammar. Each should still yield a symbol.
	for _, f := range []string{"button.tsx", "util.js", "card.jsx"} {
		got, err := s.AllChunks(f)
		if err != nil {
			t.Fatal(err)
		}
		if len(got) == 0 {
			t.Errorf("expected chunks from %s, got none", f)
		}
	}
}

func TestAllChunks_PathPrefixFilter(t *testing.T) {
	t.Parallel()
	vault := t.TempDir()
	writeFile(t, vault, "docs/guide.md", "# Guide\n\nUnder docs.\n")
	writeFile(t, vault, "notes/todo.md", "# Todo\n\nUnder notes.\n")
	s := openTemp(t)
	if _, err := s.Reindex(vault, fakeEmbed, false); err != nil {
		t.Fatal(err)
	}

	got, err := s.AllChunks("docs/")
	if err != nil {
		t.Fatal(err)
	}
	if len(got) == 0 {
		t.Fatal("expected chunks under docs/")
	}
	for _, c := range got {
		if !strings.Contains(c.RelPath, "docs/") {
			t.Errorf("prefix filter leaked non-docs chunk: %s", c.RelPath)
		}
	}
}

func TestFileHeadings_SlugSets(t *testing.T) {
	t.Parallel()
	vault := t.TempDir()
	writeFile(t, vault, "guide.md", "# Getting Started\n\nIntro.\n\n## API Endpoints\n\nList.\n\n## GET /users/{id} — Fetch\n\nGET.\n")
	writeFile(t, vault, "flat.md", "Just prose, no headings.\n")
	s := openTemp(t)
	if _, err := s.Reindex(vault, fakeEmbed, false); err != nil {
		t.Fatal(err)
	}

	heads, err := s.FileHeadings()
	if err != nil {
		t.Fatal(err)
	}
	got := heads["guide.md"]
	// Slugs are GitHub anchors: punctuation deleted (not hyphenated), so the
	// path heading keeps its double hyphen from the em-dash's spaces.
	for _, slug := range []string{"getting-started", "api-endpoints", "get-usersid--fetch"} {
		if !got[slug] {
			t.Errorf("guide.md missing heading slug %q; got %v", slug, got)
		}
	}
	// A file with no headings has no anchor targets, so it's absent from the map.
	if _, ok := heads["flat.md"]; ok {
		t.Errorf("flat.md has no headings; should not appear, got %v", heads["flat.md"])
	}
}

// indexedFiles returns the set of rel-paths that ended up in the index.
func indexedFiles(t *testing.T, s *Store) map[string]bool {
	t.Helper()
	chunks, err := s.AllChunks("")
	if err != nil {
		t.Fatal(err)
	}
	set := map[string]bool{}
	for _, c := range chunks {
		set[c.RelPath] = true
	}
	return set
}

func TestReindex_HonorsGitignore(t *testing.T) {
	t.Parallel()
	vault := t.TempDir()
	writeFile(t, vault, ".gitignore", "build/\n.venv/\nsecret.md\n")
	writeFile(t, vault, "keep.md", "# Keep\n\nTracked note.\n")
	writeFile(t, vault, "secret.md", "# Secret\n\nGit-ignored file.\n")
	writeFile(t, vault, ".github/guide.md", "# Guide\n\nCommitted dotdir, not ignored.\n")
	writeFile(t, vault, ".obsidian/cfg.md", "# Cfg\n\nDotdir not ignored → indexed in git mode.\n")
	writeFile(t, vault, "build/out.md", "# Out\n\nGenerated, ignored.\n")
	writeFile(t, vault, ".venv/dep.md", "# Dep\n\nVendored, ignored.\n")

	s := openTemp(t)
	if _, err := s.Reindex(vault, fakeEmbed, false); err != nil {
		t.Fatal(err)
	}
	got := indexedFiles(t, s)
	// git trusts its ignore rules: committed files (incl. dotdirs) index;
	// ignored paths do not.
	for _, want := range []string{"keep.md", ".github/guide.md", ".obsidian/cfg.md"} {
		if !got[want] {
			t.Errorf("gitignore mode should index tracked file %s; got %v", want, got)
		}
	}
	for _, skip := range []string{"secret.md", "build/out.md", ".venv/dep.md"} {
		if got[skip] {
			t.Errorf("gitignore mode indexed an ignored path: %s", skip)
		}
	}
}

func TestReindex_PersistsAndReplacesLinks(t *testing.T) {
	t.Parallel()
	vault := t.TempDir()
	p := writeFile(t, vault, "a.md", "# A\n\nLinks to [b](b.md) and [[C Note]].\n")
	writeFile(t, vault, "b.md", "# B\n\nNo links.\n")

	s := openTemp(t)
	if _, err := s.Reindex(vault, fakeEmbed, false); err != nil {
		t.Fatal(err)
	}

	links, err := s.AllLinks()
	if err != nil {
		t.Fatal(err)
	}
	got := map[string]string{} // target → kind
	for _, l := range links {
		if l.SrcRelPath != "a.md" {
			t.Errorf("unexpected link source %s", l.SrcRelPath)
		}
		got[l.Target] = l.Kind
	}
	if got["b.md"] != "md" || got["C Note"] != "wiki" {
		t.Fatalf("links = %v, want b.md(md) and C Note(wiki)", got)
	}

	// Rewriting a.md with a different link must replace, not accumulate.
	if err := os.WriteFile(p, []byte("# A\n\nNow links to [c](c.md) only.\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	future := time.Now().Add(2 * time.Second)
	if err := os.Chtimes(p, future, future); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Reindex(vault, fakeEmbed, false); err != nil {
		t.Fatal(err)
	}
	links, err = s.AllLinks()
	if err != nil {
		t.Fatal(err)
	}
	if len(links) != 1 || links[0].Target != "c.md" {
		t.Fatalf("after rewrite links = %+v, want just c.md", links)
	}
}

func TestVecRoundTrip(t *testing.T) {
	t.Parallel()
	vault := t.TempDir()
	writeFile(t, vault, "a.md", "# A\n\nHello vectors.\n")
	s := openTemp(t)
	if _, err := s.Reindex(vault, fakeEmbed, false); err != nil {
		t.Fatal(err)
	}
	chunks, err := s.AllChunks("")
	if err != nil {
		t.Fatal(err)
	}
	for _, c := range chunks {
		want, _ := fakeEmbed(c.Text)
		if len(c.Vec) != len(want) {
			t.Fatalf("vec len %d, want %d", len(c.Vec), len(want))
		}
		for i := range want {
			if c.Vec[i] != want[i] {
				t.Errorf("vec[%d] = %v, want %v (round-trip corrupted)", i, c.Vec[i], want[i])
			}
		}
	}
}

func TestOpen_ReopenIsIdempotent(t *testing.T) {
	t.Parallel()
	path := filepath.Join(t.TempDir(), "index.db")
	s1, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	s1.Close()
	s2, err := Open(path) // migrate() must be a no-op on an existing schema
	if err != nil {
		t.Fatalf("reopen: %v", err)
	}
	s2.Close()
}
