// Command semantic indexes a directory of markdown files and source code and
// exposes semantic search over them, backed by a local index. See the README
// for the language set and development conventions.
package main

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"runtime/debug"
	"sort"
	"strings"
	"time"

	"github.com/alecthomas/kong"

	"github.com/reactor-team/semantic/pkg/chunk"
	"github.com/reactor-team/semantic/pkg/embed"
	"github.com/reactor-team/semantic/pkg/graph"
	"github.com/reactor-team/semantic/pkg/index"
	"github.com/reactor-team/semantic/pkg/lint"
	"github.com/reactor-team/semantic/pkg/search"
	"github.com/reactor-team/semantic/pkg/toc"
)

// Stamped at build time via -ldflags (see mise-tasks/build.sh). Without the
// stamp, versionString falls back to the recorded build info.
var (
	version = "dev"
	sha     = ""
)

// Globals is the shared flag set for every subcommand. Kong promotes these
// onto the root CLI (embedded below) and binds &CLI.Globals into each
// command's Run so handlers read them without a package-level global.
type Globals struct {
	DB        string `help:"Index database path (default: <vault>/.semantic/index.db)." env:"SEMANTIC_DB" placeholder:"PATH"`
	Vault     string `help:"Vault root to resolve relative paths against (default: cwd)." placeholder:"DIR"`
	NoReindex bool   `name:"no-reindex" help:"Skip the auto-reindex search/dupes/graph/lint normally run first; answer from the index as last built."`
	Model     string `help:"Embedding model to embed with; see 'semantic models'." env:"SEMANTIC_MODEL" placeholder:"NAME"`
}

// resolvedVault is the vault root every command resolves against: --vault when
// set, otherwise the current directory.
func (g *Globals) resolvedVault() string {
	if g.Vault == "" {
		return "."
	}
	return g.Vault
}

// dbPath resolves the index database location: an explicit --db wins,
// otherwise it's <vault>/.semantic/index.db.
func (g *Globals) dbPath() string {
	if g.DB != "" {
		return g.DB
	}
	return index.DefaultDBPath(g.resolvedVault())
}

// printJSON writes v to stdout as indented JSON — the shared encoder every
// command's --json path uses.
func printJSON(v any) error {
	enc := json.NewEncoder(os.Stdout)
	enc.SetIndent("", "  ")
	return enc.Encode(v)
}

// openForRead opens the index database for a read-only command (search,
// dupes, graph, lint) and, unless --no-reindex is set, incrementally
// reindexes the vault first — so these commands never silently answer from a
// stale index. Store.Reindex's stat-only fast path means this costs
// negligible time when nothing changed; a note goes to stderr only when
// files were actually added, updated, or removed. Returns the resolved vault
// alongside the store, since callers building the link graph or resolving
// linkRoot need it too.
//
// skipEmbed drops the embedder from the reindex pass. graph and lint never
// read a chunk's vector, so they always pass it, and the saving is larger
// than the inference they skip: a representation change is only reported when
// an embedder is present, so embedding here would also drag the whole vault
// back through the model on the first lint after a model swap — minutes of
// work for a command that cannot use the result. Chunks indexed this way get
// a placeholder vector and the model stamp is left alone; the next run that
// does embed (search, dupes, or plain `semantic index`) re-embeds exactly
// those chunks.
func (g *Globals) openForRead(skipEmbed bool) (*index.Store, string, error) {
	vault := g.resolvedVault()
	st, err := index.Open(g.dbPath())
	if err != nil {
		return nil, "", err
	}
	if g.NoReindex {
		return st, vault, nil
	}
	var embedFn index.Embedder
	if !skipEmbed {
		// The reindex below is about to embed, so fetch the model if it is
		// missing. Under --no-reindex nothing here embeds and the command is
		// left alone; search asks separately, because its query needs the
		// model whether or not the index was refreshed.
		if err := ensureEmbedReady(); err != nil {
			_ = st.Close()
			return nil, "", err
		}
		embedFn = embed.Get
	}
	// Warn before the work, not after. A representation change makes this
	// implicit reindex rebuild the whole vault, which on a large tree is minutes
	// of silence on a command the user expected to answer immediately.
	if why, err := st.RebuildReason(embedFn != nil); err == nil && why != "" {
		stderrf("semantic: %s — rebuilding the index (one time)", why)
	}
	stop := withIndexProgress(st)
	rep, err := st.Reindex(vault, embedFn, false)
	stop()
	if err != nil {
		_ = st.Close()
		return nil, "", fmt.Errorf("auto-reindex: %w", err)
	}
	if rep.Relinked > 0 {
		stderrf("semantic: re-extracted links for %d file(s)", rep.Relinked)
	}
	if rep.Added+rep.Updated+rep.Deleted > 0 {
		stderrf("semantic: reindexed %d changed file(s) (+%d ~%d -%d)",
			rep.Added+rep.Updated+rep.Deleted, rep.Added, rep.Updated, rep.Deleted)
	}
	return st, vault, nil
}

// status is the one live progress line for the process. It is package-level
// because two things write it — the download hook and the index hook — and a
// second instance would erase a line it did not draw, leaving the first one's
// text stranded on screen.
var status *statusLine

// stderrf prints a milestone, erasing the live progress line first. Every
// write to stderr that is not the progress line itself has to go through here,
// or it lands on top of a line that is still there.
func stderrf(format string, a ...any) {
	status.done()
	fmt.Fprintf(os.Stderr, format+"\n", a...)
}

// withIndexProgress makes the store report each file it reaches, so a rebuild
// on a large tree shows movement instead of sitting silent. The line is erased
// before the caller prints anything of its own.
func withIndexProgress(st *index.Store) func() {
	st.SetProgress(func(rel string, done int) {
		status.set(fmt.Sprintf("  indexing %d: %s", done, rel), false)
	})
	return func() {
		st.SetProgress(nil)
		status.done()
	}
}

// assertVectorsComparable refuses to rank against vectors from a different
// model than the one now selected. Only --no-reindex reaches it: every other
// path re-embeds the index on a mismatch, which is the fix rather than the
// error.
//
// This is a hard failure and not a warning because the alternative is worse
// than a stale answer. Cosine similarity between two models' vectors is a real
// number in the right range, ordered by nothing — the command prints ranked
// results, at plausible-looking scores, that mean nothing at all. An empty
// index and an index that has only ever been linted are both left alone; they
// have no vectors to be wrong about, and search already returns nothing.
func assertVectorsComparable(st *index.Store) error {
	stored, err := st.EmbedStamp()
	if err != nil || stored == "" {
		return err
	}
	if stored == embed.RepresentationID() {
		return nil
	}
	return fmt.Errorf(
		"index was built with %s but this run embeds with %s — scores between the two are meaningless.\n"+
			"       Drop --no-reindex to rebuild the index, or pass --model to match what built it",
		stored, embed.RepresentationID())
}

// ensureEmbedReady fetches the runtime and model when they are missing, so a
// command that embeds does not stop to tell the user to run `init` first. It is
// a no-op once they are cached. `semantic init` remains the way to pay the
// download deliberately rather than in the middle of a query.
func ensureEmbedReady() error {
	return embed.EnsureModel(stderrf)
}

// InitCmd downloads the ONNX runtime library + embedding model into the OS
// cache dir. One-time, idempotent, ~160MB.
type InitCmd struct{}

func (c *InitCmd) Run(_ *Globals) error {
	return embed.DownloadAll(stderrf)
}

// IndexCmd incrementally (re)builds the index for the vault (--vault, default
// cwd) — the same root every other command resolves against. Both markdown and
// source code are indexed, the latter by AST. Reindex is a whole-tree sync (it
// prunes DB rows it no longer sees), so the indexed tree is the vault, not a
// subtree — hence no separate directory argument. --force re-chunks,
// re-embeds, and re-extracts links for every file even when its content is
// unchanged. An upgrade that changes chunking, link extraction, or the
// embedding model is detected from the stamps in the index and redone without
// it (see pkg/index/representation.go), so this is the manual override for
// when that detection cannot help: a chunker being changed locally without a
// version bump, or an index suspected of being damaged.
type IndexCmd struct {
	Force bool `help:"Re-chunk, re-embed, and re-extract links for every file, even unchanged ones. An upgrade rebuilds what it needs on its own; this is the manual override."`
}

func (c *IndexCmd) Run(g *Globals) error {
	st, err := index.Open(g.dbPath())
	if err != nil {
		return err
	}
	defer func() { _ = st.Close() }()

	if err := ensureEmbedReady(); err != nil {
		return err
	}
	if why, err := st.RebuildReason(true); err == nil && why != "" {
		stderrf("semantic: %s — rebuilding the index (one time)", why)
	}
	stop := withIndexProgress(st)
	rep, err := st.Reindex(g.resolvedVault(), embed.Get, c.Force)
	stop()
	if err != nil {
		return err
	}
	if rep.Relinked > 0 {
		fmt.Printf("re-extracted links for %d files in %s\n",
			rep.Relinked, rep.Duration.Round(time.Millisecond))
		return nil
	}
	fmt.Printf("indexed %d files (+%d ~%d =%d -%d) → %d chunks in %s\n",
		rep.Files, rep.Added, rep.Updated, rep.Unchanged, rep.Deleted,
		rep.Chunks, rep.Duration.Round(time.Millisecond))
	return nil
}

// LangsCmd prints the language names --lang accepts.
type LangsCmd struct{}

func (c *LangsCmd) Run(_ *Globals) error {
	fmt.Println(strings.Join(chunk.LanguageNames(), "\n"))
	return nil
}

// ModelsCmd prints the embedding models --model accepts, marking the one in
// force. Switching models re-embeds the index, so the dimension and the window
// are shown alongside: they are what the choice actually trades off.
type ModelsCmd struct {
	JSON bool `help:"Emit the model list as JSON instead of tab-separated rows."`
}

func (c *ModelsCmd) Run(_ *Globals) error {
	type row struct {
		Name      string `json:"name"`
		Dim       int    `json:"dim"`
		MaxSeqLen int    `json:"max_seq_len"`
		Pooling   string `json:"pooling"`
		ApproxMB  int    `json:"approx_mb"`
		Current   bool   `json:"current"`
		Installed bool   `json:"installed"`
	}
	cur := embed.Current()
	rows := make([]row, 0, len(embed.Models()))
	for _, m := range embed.Models() {
		rows = append(rows, row{
			Name: m.Name, Dim: m.Dim, MaxSeqLen: m.MaxSeqLen,
			Pooling: string(m.Pooling), ApproxMB: m.ApproxMB,
			Current: m == cur, Installed: embed.Installed(m),
		})
	}
	if c.JSON {
		return printJSON(rows)
	}
	for _, r := range rows {
		mark := " "
		if r.Current {
			mark = "*"
		}
		state := "not downloaded"
		if r.Installed {
			state = "installed"
		}
		fmt.Printf("%s %s\td%d\ts%d\t%s\t~%dMB\t%s\n",
			mark, r.Name, r.Dim, r.MaxSeqLen, r.Pooling, r.ApproxMB, state)
	}
	return nil
}

// resolveLangs turns --lang values into canonical language names. Values may
// be repeated or comma-separated, since both spellings are what people reach
// for, and aliases are accepted so `--lang c++` and `--lang k8s` work.
//
// An unrecognised name is an error rather than a filter matching nothing: a
// search that silently returns zero hits reads as "no such code", which is a
// much worse answer than "no such language".
func resolveLangs(values []string) ([]string, error) {
	var langs []string
	for _, v := range values {
		for name := range strings.SplitSeq(v, ",") {
			if name = strings.TrimSpace(name); name == "" {
				continue
			}
			canonical, ok := chunk.NormalizeLanguage(name)
			if !ok {
				return nil, fmt.Errorf("unknown language %q; `semantic langs` lists the %d available",
					name, len(chunk.LanguageNames()))
			}
			langs = append(langs, canonical)
		}
	}
	return langs, nil
}

// SearchCmd embeds a query and cosine-ranks it against the index.
type SearchCmd struct {
	Query    string   `arg:"" help:"Natural-language search query."`
	Limit    int      `short:"n" default:"10" help:"Maximum results to return."`
	Path     string   `help:"Restrict to files under this path prefix." placeholder:"PREFIX"`
	MinScore float64  `name:"min-score" help:"Drop hits below this cosine score."`
	Docs     bool     `xor:"kind" help:"Search only Markdown docs."`
	Code     bool     `xor:"kind" help:"Search only source code."`
	Lang     []string `help:"Restrict to these languages (repeatable, or comma-separated); see 'semantic langs'." placeholder:"LANG"`
	JSON     bool     `help:"Emit results as JSON instead of tab-separated rows."`
}

func (c *SearchCmd) Run(g *Globals) error {
	langs, err := resolveLangs(c.Lang)
	if err != nil {
		return err
	}

	// Before openForRead, which only fetches the model when it is about to
	// reindex. The query is embedded either way, so --no-reindex must not
	// leave search without a model.
	if err := ensureEmbedReady(); err != nil {
		return err
	}

	st, _, err := g.openForRead(false)
	if err != nil {
		return err
	}
	defer func() { _ = st.Close() }()
	if err := assertVectorsComparable(st); err != nil {
		return err
	}

	kind := search.KindAny
	switch {
	case c.Docs:
		kind = search.KindDocs
	case c.Code:
		kind = search.KindCode
	}

	hits, err := search.Query(st, embed.GetQuery, c.Query, search.Options{
		Limit:      c.Limit,
		PathPrefix: c.Path,
		MinScore:   c.MinScore,
		Collapse:   true, // one hit per file — cleanest for "find the note about X"
		Kind:       kind,
		Langs:      langs,
	})
	if err != nil {
		return err
	}

	if c.JSON {
		return printJSON(hits)
	}
	// Block layout: a header line — path:line⇥score⇥breadcrumb — with the
	// location first so it stays editor-openable (ripgrep convention) and
	// greppable, followed by the matched chunk's full text indented beneath
	// it. The header repeats the breadcrumb the embedded text carries, so we
	// drop that leading line from the body, leaving just the content. Blocks
	// are separated by a blank line.
	for i, h := range hits {
		if i > 0 {
			fmt.Println()
		}
		fmt.Printf("%s:%d\t%.2f\t%s\n", h.RelPath, h.Line, h.Score, breadcrumb(h.Heading))
		// A /path match carries only the breadcrumb (already in the header),
		// so its body is empty — print the header alone rather than a bare
		// indented blank line.
		if body := hitBody(h); body != "" {
			fmt.Println(indentBlock(body, "    "))
		}
	}
	return nil
}

// DupesCmd reports pairs of near-duplicate chunks — sections whose embeddings
// are near-identical, i.e. likely redundant docs or guidance to consolidate.
type DupesCmd struct {
	MinScore   float64 `name:"min-score" default:"0.9" help:"Report pairs at or above this cosine score."`
	Limit      int     `short:"n" default:"20" help:"Maximum pairs to return (<=0 for no cap)."`
	Path       string  `help:"Restrict to files under this path prefix." placeholder:"PREFIX"`
	WithinFile bool    `name:"within-file" help:"Also report near-duplicate pairs inside a single file."`
	JSON       bool    `help:"Emit pairs as JSON instead of text blocks."`
}

func (c *DupesCmd) Run(g *Globals) error {
	st, _, err := g.openForRead(false)
	if err != nil {
		return err
	}
	defer func() { _ = st.Close() }()
	if err := assertVectorsComparable(st); err != nil {
		return err
	}

	pairs, err := search.Duplicates(st, search.DupeOptions{
		MinScore:   c.MinScore,
		Limit:      c.Limit,
		PathPrefix: c.Path,
		WithinFile: c.WithinFile,
	})
	if err != nil {
		return err
	}

	if c.JSON {
		return printJSON(pairs)
	}
	if len(pairs) == 0 {
		fmt.Printf("no chunk pairs at or above cosine %.2f\n", c.MinScore)
		return nil
	}
	// One block per pair: a score line, then each endpoint as its own
	// path:line⇥breadcrumb header with the chunk body indented beneath —
	// mirroring search's editor-openable layout so both sides are jumpable.
	for i, p := range pairs { //nolint:gocritic // rangeValCopy: value semantics intentional, small slice
		if i > 0 {
			fmt.Println()
		}
		fmt.Printf("%.2f\n", p.Score)
		printDupeSide(p.A)
		printDupeSide(p.B)
	}
	return nil
}

// hitBody returns a hit's chunk text ready to display beneath its header: the
// leading breadcrumb (already in the header) stripped and surrounding
// whitespace trimmed. "" when the chunk carries only its breadcrumb (a /path
// match), so the caller can print the header alone.
func hitBody(h search.Hit) string { //nolint:gocritic // hugeParam: value semantics intentional; callers pass range-loop copies
	text := h.Text
	if h.Heading != "" {
		text = strings.TrimSpace(strings.TrimPrefix(text, h.Heading))
	}
	return strings.TrimSpace(text)
}

// printDupeSide renders one endpoint of a duplicate pair: the location header
// followed by the chunk's body (with the redundant leading breadcrumb dropped,
// as search does), each line indented so the two sides read as a unit.
func printDupeSide(h search.Hit) { //nolint:gocritic // hugeParam: value semantics intentional; callers pass range-loop copies
	fmt.Printf("  %s:%d\t%s\n", h.RelPath, h.Line, breadcrumb(h.Heading))
	if body := hitBody(h); body != "" {
		fmt.Println(indentBlock(body, "      "))
	}
}

// GraphCmd inspects the document link graph — the [text](rel.md) and
// [[wikilink]] edges between markdown files — to surface orphans, dead links,
// and backlinks for docs cleanup.
type GraphCmd struct {
	Orphans       bool   `help:"List markdown files with no inbound links."`
	Broken        bool   `help:"List links whose target resolves to no indexed file."`
	BrokenAnchors bool   `help:"List links whose #section resolves to no heading in the target file."`
	Backlinks     string `help:"List files that link to this rel-path." placeholder:"PATH"`
	JSON          bool   `help:"Emit the full graph (files + resolved edges) as JSON."`
	Dot           bool   `help:"Emit resolved edges in Graphviz DOT format."`
}

func (c *GraphCmd) Run(g *Globals) error {
	st, vault, err := g.openForRead(true)
	if err != nil {
		return err
	}
	defer func() { _ = st.Close() }()

	gr, err := graph.Build(st, vault)
	if err != nil {
		return err
	}

	switch {
	case c.JSON:
		return printJSON(struct {
			Files []string     `json:"files"`
			Edges []graph.Edge `json:"edges"`
		}{gr.Files, gr.Edges})

	case c.Dot:
		fmt.Println("digraph docs {")
		for _, e := range gr.Edges {
			if e.To != "" {
				fmt.Printf("  %q -> %q;\n", e.From, e.To)
			}
		}
		fmt.Println("}")
		return nil

	case c.Backlinks != "":
		printListOr(gr.Backlinks(c.Backlinks), fmt.Sprintf("no backlinks to %s", c.Backlinks), func(e graph.Edge) string {
			return fmt.Sprintf("%s:%d\t(%s)", e.From, e.Line, e.Kind)
		})
		return nil

	case c.Orphans:
		printListOr(gr.Orphans(), "no orphaned markdown files", func(f string) string { return f })
		return nil

	case c.Broken:
		printListOr(gr.Broken(), "no broken links", func(e graph.Edge) string {
			return fmt.Sprintf("%s:%d\t%s\t(%s)", e.From, e.Line, e.DestWithAnchor(), e.Kind)
		})
		return nil

	case c.BrokenAnchors:
		printListOr(gr.BrokenAnchors(), "no broken section anchors", func(e graph.Edge) string {
			return fmt.Sprintf("%s:%d\t%s → %s (no such section)\t(%s)", e.From, e.Line, e.DestWithAnchor(), e.To, e.Kind)
		})
		return nil

	default:
		s := gr.Stats()
		fmt.Printf("files:    %d (%d markdown)\n", s.Files, s.Markdown)
		fmt.Printf("edges:    %d (%d resolved, %d broken, %d broken anchors)\n", s.Edges, s.Resolved, s.Broken, s.BrokenAnchors)
		fmt.Printf("orphans:  %d\n", s.Orphans)
		if s.Edges == 0 && s.Markdown > 0 {
			fmt.Println("\nno links recorded — run `semantic index` to (re)populate the graph")
		} else {
			fmt.Println("\ndetail: --orphans · --broken · --broken-anchors · --backlinks PATH · --json · --dot")
		}
		return nil
	}
}

// LintCmd flags docs-hygiene issues: inline-code doc- and source-path
// references (`docs/foo.md`, `internal/foo/bar.go`) that aren't real markdown
// links and so never register as graph edges — unlinked (resolve to exactly
// one indexed file; turn them into links), ambiguous (a bare basename with no
// directory matches more than one file of that name — needs a fuller path,
// not a guess), or broken (resolve to nothing); deep relative links that
// climb several directories with `../`, which a root-absolute path states
// more stably; and long markdown files (over toc.LineThreshold lines) whose
// committed `## Contents` TOC is missing or stale. --fix rewrites the
// auto-fixable categories in place (unlinked references → real links with
// the original path as backtick-wrapped label and a root-absolute target,
// deep relative links → root-absolute, and Contents TOCs → regenerated);
// ambiguous and broken refs still need a human. An optional list of files
// narrows every check (and --fix) to just those paths — the scope a
// pre-commit hook wants so it touches only staged files, not the whole
// vault; `--unlinked`/`--ambiguous`/`--broken`/`--deep`/`--toc` narrow both
// the report and --fix to just the selected kinds.
type LintCmd struct {
	Files     []string `arg:"" optional:"" name:"files" help:"Limit all checks (and --fix) to these files (cwd- or repo-relative); default is the whole vault."`
	Unlinked  bool     `help:"Show only unlinked references (resolve to exactly one indexed file)."`
	Ambiguous bool     `help:"Show only ambiguous references (bare basename matches more than one indexed file)."`
	Broken    bool     `help:"Show only broken inline references (resolve to nothing)."`
	Deep      bool     `help:"Show only deep relative links (climb several dirs; prefer a root-absolute path)."`
	Toc       bool     `help:"Show only long files missing an up-to-date Contents TOC."`
	Fix       bool     `help:"Rewrite the auto-fixable findings (unlinked refs → real links, deep links → root-absolute, TOCs regenerated) in place; reindex after."`
	JSON      bool     `help:"Emit the full report as JSON."`
	NoEmbed   bool     `name:"no-embed" hidden:"" help:"Deprecated no-op: lint never embeds. Accepted so existing hooks and CI keep working."`
}

func (c *LintCmd) Run(g *Globals) error {
	st, vault, err := g.openForRead(true)
	if err != nil {
		return err
	}
	defer func() { _ = st.Close() }()

	rep, err := lint.Analyze(st, linkRoot(vault))
	if err != nil {
		return err
	}
	if err := c.narrowReport(st, vault, rep); err != nil {
		return err
	}

	if c.Fix {
		return fixLint(vault, rep, c.JSON)
	}

	total := len(rep.Unlinked) + len(rep.Ambiguous) + len(rep.Broken) + len(rep.DeepRelative) + len(rep.MissingTOC)

	if c.JSON {
		if err := printJSON(rep); err != nil {
			return err
		}
		if total > 0 {
			return fmt.Errorf("%d lint issue(s) flagged", total)
		}
		return nil
	}

	if total == 0 {
		fmt.Println("no lint issues flagged")
		return nil
	}
	printLintReport(rep)
	return fmt.Errorf("%d lint issue(s) flagged", total)
}

// narrowReport runs the index-free TOC audit and narrows rep in place to the
// command's file scope and selected kinds. The link graph must resolve against
// the whole index, so lint.Analyze always runs vault-wide; a file scope narrows
// the findings afterward. The TOC audit is scoped at its source (it reads
// content straight from disk) so a new, not-yet-indexed file still gets its
// Contents TOC checked and fixed.
func (c *LintCmd) narrowReport(st *index.Store, vault string, rep *lint.Report) error {
	scope, err := scopePaths(vault, c.Files)
	if err != nil {
		return err
	}

	// wantTOC mirrors the kind-narrowing below: MissingTOC only survives when
	// no kind flag is set (default: everything) or --toc is. Skipping the
	// audit otherwise avoids reading every file in scope from disk only to
	// throw the result away — the whole point of a category-narrowed call
	// like the CI removed-path scan.
	wantTOC := (!c.Unlinked && !c.Ambiguous && !c.Broken && !c.Deep) || c.Toc
	if wantTOC {
		tocFiles := scope
		if scope == nil {
			tocFiles, err = st.AllFiles()
			if err != nil {
				return err
			}
		}
		rep.MissingTOC, err = lint.AuditTOCs(tocFiles, vaultReader(vault))
		if err != nil {
			return err
		}
	}
	if scope != nil {
		set := make(map[string]bool, len(scope))
		for _, f := range scope {
			set[f] = true
		}
		rep.Unlinked = lint.KeepFiles(rep.Unlinked, set)
		rep.Ambiguous = lint.KeepFiles(rep.Ambiguous, set)
		rep.Broken = lint.KeepFiles(rep.Broken, set)
		rep.DeepRelative = lint.KeepFiles(rep.DeepRelative, set)
	}

	// All sections by default; any flag narrows the report to just the
	// selected kinds — for both the printed listing and what --fix touches,
	// so e.g. `--unlinked --fix` can't also silently rewrite TOCs or deep
	// links.
	if c.Unlinked || c.Ambiguous || c.Broken || c.Deep || c.Toc {
		if !c.Unlinked {
			rep.Unlinked = nil
		}
		if !c.Ambiguous {
			rep.Ambiguous = nil
		}
		if !c.Broken {
			rep.Broken = nil
		}
		if !c.Deep {
			rep.DeepRelative = nil
		}
		if !c.Toc {
			rep.MissingTOC = nil
		}
	}
	return nil
}

// printLintReport writes the non-JSON lint report: one section per non-empty
// finding kind, each led by a header naming the problem and a "→ fix" line
// stating the resolution and whether `semantic lint --fix` handles it (auto) or
// it needs a human (manual). Sections are separated by a blank line. Assumes at
// least one finding (the caller handles the empty case).
func printLintReport(rep *lint.Report) {
	var sec sectionPrinter
	if len(rep.Unlinked) > 0 {
		sec.next()
		fmt.Printf("unlinked references (%d) — inline-code paths that resolve to an indexed file\n", len(rep.Unlinked))
		fmt.Println("  → fix (auto): run `semantic lint --fix` to convert them to links")
		for _, r := range rep.Unlinked {
			fmt.Printf("  %s:%d\t%s → %s\n", r.From, r.Line, r.DestWithAnchor(), r.To)
		}
	}
	if len(rep.Ambiguous) > 0 {
		sec.next()
		fmt.Printf("ambiguous references (%d) — bare basename matches more than one indexed file\n", len(rep.Ambiguous))
		fmt.Println("  → fix (manual): replace the basename with a path naming exactly one file")
		for _, r := range rep.Ambiguous {
			fmt.Printf("  %s:%d\t%s → ambiguous (%d): %s\n", r.From, r.Line, r.DestWithAnchor(), len(r.Candidates), strings.Join(r.Candidates, ", "))
		}
	}
	if len(rep.Broken) > 0 {
		sec.next()
		fmt.Printf("broken inline references (%d) — file or #section resolves to nothing\n", len(rep.Broken))
		fmt.Println("  → fix (manual): point the path/anchor at a real target, or drop the reference")
		for _, r := range rep.Broken {
			if r.To != "" { // file resolved, so the #section is the dead part
				fmt.Printf("  %s:%d\t%s → %s (no such section)\n", r.From, r.Line, r.DestWithAnchor(), r.To)
			} else {
				fmt.Printf("  %s:%d\t%s\n", r.From, r.Line, r.DestWithAnchor())
			}
		}
	}
	if len(rep.DeepRelative) > 0 {
		sec.next()
		fmt.Printf("deep relative links (%d) — climb %d+ dirs from the source file\n", len(rep.DeepRelative), lint.DeepLinkClimb)
		fmt.Println("  → fix (auto): run `semantic lint --fix` to rewrite them root-absolute")
		for _, r := range rep.DeepRelative {
			fmt.Printf("  %s:%d\t%s → %s\n", r.From, r.Line, r.DestWithAnchor(), r.Suggest)
		}
	}
	if len(rep.MissingTOC) > 0 {
		sec.next()
		fmt.Printf("missing Contents TOC (%d) — files over %d lines whose `## Contents` is absent or stale\n", len(rep.MissingTOC), toc.LineThreshold)
		fmt.Println("  → fix (auto): run `semantic lint --fix` to regenerate the TOC")
		for _, t := range rep.MissingTOC {
			fmt.Printf("  %s\t%d lines (%s)\n", t.File, t.Lines, t.Reason)
		}
	}
}

// linkRoot returns the vault's forward-slashed path within its enclosing git
// repository — the prefix a root-absolute (leading-'/') link needs to resolve
// against the repo root the way GitHub does. It walks up from the vault to the
// nearest ancestor holding a .git entry (dir or file, for worktrees) and
// returns the vault's path relative to it. Returns "" when the vault is the
// repo root, or when no repository is found — in which case the vault is
// treated as the root and suggestions stay vault-relative.
func linkRoot(vault string) string {
	abs, err := filepath.Abs(vault)
	if err != nil {
		return ""
	}
	for dir := abs; ; {
		if _, err := os.Stat(filepath.Join(dir, ".git")); err == nil {
			rel, err := filepath.Rel(dir, abs)
			if err != nil || rel == "." {
				return ""
			}
			return filepath.ToSlash(rel)
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			return "" // reached the filesystem root without finding a repo
		}
		dir = parent
	}
}

// scopePaths converts the command's file arguments to the vault-relative,
// forward-slashed rel-paths the report keys on (Ref.From / TOCFinding.File),
// resolving each through its absolute path so cwd- or repo-relative inputs (what
// a pre-commit hook passes) all land in the vault's frame. Returns nil when no
// files were given — whole-vault mode.
func scopePaths(vault string, paths []string) ([]string, error) {
	if len(paths) == 0 {
		return nil, nil
	}
	vabs, err := filepath.Abs(vault)
	if err != nil {
		return nil, err
	}
	rels := make([]string, 0, len(paths))
	for _, p := range paths {
		pabs, err := filepath.Abs(p)
		if err != nil {
			return nil, err
		}
		rel, err := filepath.Rel(vabs, pabs)
		if err != nil {
			return nil, fmt.Errorf("%s is outside the vault %s: %w", p, vault, err)
		}
		rels = append(rels, filepath.ToSlash(rel))
	}
	return rels, nil
}

// vaultPath joins vault and a rel-path, rejecting any result that would
// resolve outside vault. rel is always sourced from the index or lint report
// built from this same vault, so it should never carry a ".." component —
// this guards the file I/O below regardless, in case of a tampered index.db.
func vaultPath(vault, rel string) (string, error) {
	vabs, err := filepath.Abs(vault)
	if err != nil {
		return "", err
	}
	abs, err := filepath.Abs(filepath.Join(vault, filepath.FromSlash(rel)))
	if err != nil {
		return "", err
	}
	if abs != vabs && !strings.HasPrefix(abs, vabs+string(filepath.Separator)) {
		return "", fmt.Errorf("%s escapes vault %s", rel, vault)
	}
	return abs, nil
}

// vaultReader returns a rel-path → content reader rooted at the vault, for the
// lint TOC audit (which needs raw file content the index doesn't store).
func vaultReader(vault string) func(string) (string, error) {
	return func(rel string) (string, error) {
		abs, err := vaultPath(vault, rel)
		if err != nil {
			return "", err
		}
		b, err := os.ReadFile(abs) //nolint:gosec // G304: abs is validated to stay within vault by vaultPath
		return string(b), err
	}
}

// fixLint applies the auto-fixable findings in the report, editing each source
// file in place: unlinked inline-code references become real links (basename
// display text), deep relative links are rewritten to root-absolute paths,
// and long files' Contents TOCs are regenerated. Files are read from the
// vault root (--vault or cwd, the same base the index was built from). The
// index is not updated — reindex to refresh it.
func fixLint(vault string, rep *lint.Report, asJSON bool) error {
	byFileUnlinked := map[string][]lint.Ref{}
	for _, r := range rep.Unlinked {
		byFileUnlinked[r.From] = append(byFileUnlinked[r.From], r)
	}
	byFileDeep := map[string][]lint.Ref{}
	for _, r := range rep.DeepRelative {
		byFileDeep[r.From] = append(byFileDeep[r.From], r)
	}
	relSet := make(map[string]bool, len(byFileUnlinked)+len(byFileDeep))
	for rel := range byFileUnlinked {
		relSet[rel] = true
	}
	for rel := range byFileDeep {
		relSet[rel] = true
	}
	rels := make([]string, 0, len(relSet))
	for rel := range relSet {
		rels = append(rels, rel)
	}
	sort.Strings(rels)

	var appliedUnlinked []lint.UnlinkedFix
	var applied []lint.DeepFix
	unlinkedFilesChanged, linkFilesChanged := 0, 0
	for _, rel := range rels {
		_, err := editInPlace(vault, rel, func(content string) (string, bool, error) {
			out, unlinkedFixes := lint.ApplyUnlinkedFixes(content, byFileUnlinked[rel], linkRoot(vault))
			out, deepFixes := lint.ApplyDeepFixes(out, byFileDeep[rel])
			if len(unlinkedFixes) == 0 && len(deepFixes) == 0 {
				return content, false, nil
			}
			if len(unlinkedFixes) > 0 {
				appliedUnlinked = append(appliedUnlinked, unlinkedFixes...)
				unlinkedFilesChanged++
			}
			if len(deepFixes) > 0 {
				applied = append(applied, deepFixes...)
				linkFilesChanged++
			}
			return out, true, nil
		})
		if err != nil {
			return err
		}
	}

	var tocFixed []string
	for _, t := range rep.MissingTOC {
		changed, err := editInPlace(vault, t.File, func(content string) (string, bool, error) {
			out, changed, rerr := toc.Rewrite(content)
			if rerr != nil {
				return content, false, nil // no TOC to tabulate — leave the file
			}
			return out, changed, nil
		})
		if err != nil {
			return err
		}
		if changed {
			tocFixed = append(tocFixed, t.File)
		}
	}

	if asJSON {
		return printJSON(struct {
			UnlinkedLinks []lint.UnlinkedFix `json:"unlinked_links"`
			DeepLinks     []lint.DeepFix     `json:"deep_links"`
			TOCs          []string           `json:"tocs"`
		}{appliedUnlinked, applied, tocFixed})
	}

	if len(appliedUnlinked) == 0 && len(applied) == 0 && len(tocFixed) == 0 {
		fmt.Println("no auto-fixable findings")
		return nil
	}
	fixedUnlinked, fixedDeep := 0, 0
	for _, f := range appliedUnlinked {
		fixedUnlinked += f.Count
	}
	for _, f := range applied {
		fixedDeep += f.Count
	}

	var sec sectionPrinter
	if len(appliedUnlinked) > 0 {
		sec.next()
		fmt.Printf("linked %d unlinked reference(s) across %d file(s):\n", fixedUnlinked, unlinkedFilesChanged)
		for _, f := range appliedUnlinked {
			fmt.Printf("  %s\t%s → [`%s`](%s)%s\n", f.From, f.Target, f.Target, f.Link, timesSuffix(f.Count))
		}
	}
	if len(applied) > 0 {
		sec.next()
		fmt.Printf("rewrote %d deep relative link(s) across %d file(s):\n", fixedDeep, linkFilesChanged)
		for _, f := range applied {
			fmt.Printf("  %s\t%s → %s%s\n", f.From, f.OldTarget, f.NewTarget, timesSuffix(f.Count))
		}
	}
	if len(tocFixed) > 0 {
		sec.next()
		fmt.Printf("regenerated Contents TOC in %d file(s):\n", len(tocFixed))
		for _, f := range tocFixed {
			fmt.Printf("  %s\n", f)
		}
	}
	// Findings the fix pass can't resolve: ambiguous and broken always need a
	// human, plus any unlinked ref whose line didn't match verbatim so the
	// auto-fix skipped it. Name the count per category so a --fix run doesn't
	// read as "all clear" when manual work remains.
	var manual []string
	if rem := max(len(rep.Unlinked)-fixedUnlinked, 0); rem > 0 {
		manual = append(manual, fmt.Sprintf("%d unlinked", rem))
	}
	if len(rep.Ambiguous) > 0 {
		manual = append(manual, fmt.Sprintf("%d ambiguous", len(rep.Ambiguous)))
	}
	if len(rep.Broken) > 0 {
		manual = append(manual, fmt.Sprintf("%d broken", len(rep.Broken)))
	}
	if len(manual) > 0 {
		fmt.Printf("\nstill needs manual attention: %s — run `semantic lint` for details\n", strings.Join(manual, ", "))
	}
	fmt.Println("\nreindex to refresh the graph: semantic index")
	return nil
}

// StatusCmd reports index + model health.
type StatusCmd struct{}

func (c *StatusCmd) Run(g *Globals) error {
	if err := embed.Check(); err != nil {
		fmt.Printf("model:   not ready — %v\n", err)
	} else {
		fmt.Printf("model:   ready (%s)\n", embed.ModelCacheDir())
	}

	dbp := g.dbPath()
	if _, err := os.Stat(dbp); err != nil {
		fmt.Printf("index:   %s (not created yet — run: semantic index)\n", dbp)
		return nil
	}
	fmt.Printf("index:   %s\n", dbp)

	st, err := index.Open(dbp)
	if err != nil {
		return err
	}
	defer func() { _ = st.Close() }()

	stats, err := st.Stats()
	if err != nil {
		return err
	}
	fmt.Printf("files:   %d\nchunks:  %d\n", stats.Files, stats.Chunks)
	if !stats.LastIndexed.IsZero() {
		fmt.Printf("indexed: %s\n", stats.LastIndexed.Format(time.RFC3339))
	}
	return nil
}

// breadcrumb renders a stored heading path ("# H1 > ## H2 > ### self") as a
// clean display trail ("H1 › H2 › self"), dropping the leading markdown
// markers from each segment. Empty in, empty out (title/body chunks).
func breadcrumb(headingPath string) string {
	if headingPath == "" {
		return ""
	}
	parts := strings.Split(headingPath, " > ")
	for i, p := range parts {
		parts[i] = strings.TrimLeft(p, "# ")
	}
	return strings.Join(parts, " › ")
}

// indentBlock prefixes every line of text with indent, preserving the block's
// internal line breaks so a multi-line chunk renders as its whole content
// rather than a single collapsed row. Trailing whitespace is trimmed first so
// the block ends cleanly.
func indentBlock(text, indent string) string {
	lines := strings.Split(strings.TrimRight(text, " \t\r\n"), "\n")
	for i, ln := range lines {
		lines[i] = indent + strings.TrimRight(ln, " \t\r")
	}
	return strings.Join(lines, "\n")
}

// sectionPrinter emits a blank separator line before every section after the
// first, so a multi-section report reads as blank-line-separated blocks
// without each section tracking whether it's first.
type sectionPrinter struct{ printed bool }

func (s *sectionPrinter) next() {
	if s.printed {
		fmt.Println()
	}
	s.printed = true
}

// timesSuffix renders a " (N×)" occurrence-count suffix, or "" for a single
// occurrence.
func timesSuffix(count int) string {
	if count > 1 {
		return fmt.Sprintf(" (%d×)", count)
	}
	return ""
}

// printListOr prints one line per item (via line), or emptyMsg when there are
// none — the shape every graph list subcommand shares.
func printListOr[T any](items []T, emptyMsg string, line func(T) string) {
	if len(items) == 0 {
		fmt.Println(emptyMsg)
		return
	}
	for _, it := range items {
		fmt.Println(line(it))
	}
}

// editInPlace reads the vault file at rel, runs transform on its content, and
// writes the result back (preserving mode) only when transform reports it
// changed. Returns whether the file was rewritten.
func editInPlace(vault, rel string, transform func(content string) (string, bool, error)) (bool, error) {
	abs, err := vaultPath(vault, rel)
	if err != nil {
		return false, err
	}
	info, err := os.Stat(abs)
	if err != nil {
		return false, fmt.Errorf("stat %s: %w", rel, err)
	}
	content, err := os.ReadFile(abs) //nolint:gosec // G304: abs is validated to stay within vault by vaultPath
	if err != nil {
		return false, fmt.Errorf("read %s: %w", rel, err)
	}
	out, changed, err := transform(string(content))
	if err != nil {
		return false, err
	}
	if !changed {
		return false, nil
	}
	if err := os.WriteFile(abs, []byte(out), info.Mode()); err != nil { //nolint:gosec // G703: abs is validated to stay within vault by vaultPath
		return false, fmt.Errorf("write %s: %w", rel, err)
	}
	return true, nil
}

// CLI is the root command tree. Globals is embedded so its flags are
// available on every subcommand.
type CLI struct {
	Globals

	Init    InitCmd          `cmd:"" help:"Download the embedding model + ONNX runtime (~160MB, one-time)."`
	Index   IndexCmd         `cmd:"" help:"Index/reindex markdown + code under the vault (--vault, default cwd)."`
	Search  SearchCmd        `cmd:"" help:"Semantic search over the index."`
	Langs   LangsCmd         `cmd:"" help:"List the languages --lang accepts."`
	Dupes   DupesCmd         `cmd:"" help:"Report near-duplicate chunks (redundant docs/guidance)."`
	Graph   GraphCmd         `cmd:"" help:"Inspect the document link graph (orphans, broken links, backlinks)."`
	Lint    LintCmd          `cmd:"" help:"Flag link hygiene: inline-code paths that should be links, and deep relative links."`
	Models  ModelsCmd        `cmd:"" help:"List the embedding models --model accepts."`
	Status  StatusCmd        `cmd:"" help:"Show index + model health."`
	Version kong.VersionFlag `help:"Print version and exit."`
}

// versionString describes the running binary. The build task stamps a calendar
// version and a commit; the mise `go:` backend and a bare `go install` do not,
// so the module version and revision recorded in the build info stand in. A
// module fetched by version carries no revision, so no commit is printed.
func versionString() string {
	v, rev := version, sha
	if v == "dev" {
		if info, ok := debug.ReadBuildInfo(); ok {
			if mod := info.Main.Version; mod != "" && mod != "(devel)" {
				v = mod
			}
			for _, setting := range info.Settings {
				if setting.Key == "vcs.revision" && setting.Value != "" {
					rev = setting.Value[:min(8, len(setting.Value))]
				}
			}
		}
	}
	if rev == "" {
		return v
	}
	return v + " (" + rev + ")"
}

func main() {
	var cli CLI
	ctx := kong.Parse(
		&cli,
		kong.Name("semantic"),
		kong.Description("Semantic search over a directory of markdown and source code, backed by a local index. Run 'semantic langs' for the languages it parses."),
		kong.UsageOnError(),
		kong.Vars{"version": versionString()},
	)
	// Before any command runs, because the choice decides which weights load,
	// which cache directory is read, and which representation the index is
	// stamped with. A name that does not resolve has to stop here rather than
	// quietly fall through to the default and embed with something the user
	// did not ask for.
	if err := embed.Select(cli.Model); err != nil {
		fmt.Fprintln(os.Stderr, "semantic:", err)
		os.Exit(1)
	}

	// A model download is over a hundred megabytes and the only sign of life
	// was one line before it started. Installed here rather than in each
	// command because any of them may trigger a fetch.
	status = newStatusLine()
	embed.SetProgress(func(name string, done, total int64) {
		if total > 0 {
			status.set(fmt.Sprintf("  ↓ %s  %s / %s  (%d%%)",
				name, humanBytes(done), humanBytes(total), done*100/total), false)
			return
		}
		status.set(fmt.Sprintf("  ↓ %s  %s", name, humanBytes(done)), false)
	})
	err := ctx.Run(&cli.Globals)
	status.done()
	if err != nil {
		fmt.Fprintln(os.Stderr, "semantic:", err)
		os.Exit(1)
	}
}
