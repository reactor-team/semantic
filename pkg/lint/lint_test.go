package lint

import (
	"strings"
	"testing"

	"github.com/reactor-team/semantic/pkg/chunk"
	"github.com/reactor-team/semantic/pkg/index"
)

// fakeSource feeds Analyze a canned file set, link set, and per-file heading slugs.
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

func code(src, target string) index.LinkRow {
	return index.LinkRow{SrcRelPath: src, Target: target, Kind: chunk.LinkCode}
}

func TestAnalyze_ClassifiesCodeRefs(t *testing.T) {
	t.Parallel()
	src := &fakeSource{
		files: []string{"docs/design.md", ".github/plan.md", "readme.md"},
		links: []index.LinkRow{
			code(".github/plan.md", "docs/design.md"), // root-relative → resolves → unlinked
			code("docs/design.md", "readme.md"),       // bare basename → resolves → unlinked
			code(".github/plan.md", "docs/gone.md"),   // resolves to nothing → broken
			// A real markdown link must be ignored by lint entirely.
			{SrcRelPath: "docs/design.md", Target: "readme.md", Kind: chunk.LinkMarkdown},
		},
	}
	rep, err := Analyze(src, "")
	if err != nil {
		t.Fatal(err)
	}

	if len(rep.Unlinked) != 2 {
		t.Fatalf("unlinked = %+v, want 2", rep.Unlinked)
	}
	if len(rep.Broken) != 1 || rep.Broken[0].Raw != "docs/gone.md" {
		t.Fatalf("broken = %+v, want just docs/gone.md", rep.Broken)
	}

	// The resolved target rides along on an unlinked ref.
	found := false
	for _, r := range rep.Unlinked {
		if r.Raw == "docs/design.md" {
			found = true
			if r.To != "docs/design.md" {
				t.Errorf("docs/design.md resolved to %q, want docs/design.md", r.To)
			}
		}
	}
	if !found {
		t.Errorf("expected docs/design.md among unlinked, got %+v", rep.Unlinked)
	}
}

func TestAnalyze_AmbiguousBareBasename(t *testing.T) {
	t.Parallel()
	src := &fakeSource{
		files: []string{
			"services/settings/service.go",
			"services/babysitter/service.go",
			"internal/lint/lint.go",
			"readme.md",
		},
		links: []index.LinkRow{
			code("readme.md", "service.go"),                   // bare basename, 2 same-extension matches → ambiguous
			code("readme.md", "lint.go"),                      // bare basename, 1 match → still unlinked
			code("readme.md", "services/settings/service.go"), // has a directory → never ambiguous
		},
	}
	rep, err := Analyze(src, "")
	if err != nil {
		t.Fatal(err)
	}

	if len(rep.Ambiguous) != 1 || rep.Ambiguous[0].Raw != "service.go" {
		t.Fatalf("ambiguous = %+v, want just the bare service.go ref", rep.Ambiguous)
	}
	cands := rep.Ambiguous[0].Candidates
	if len(cands) != 2 {
		t.Fatalf("candidates = %v, want both service.go files", cands)
	}
	if rep.Ambiguous[0].To != "" {
		t.Errorf("ambiguous ref should carry no single resolved target; got %q", rep.Ambiguous[0].To)
	}

	if len(rep.Unlinked) != 2 {
		t.Fatalf("unlinked = %+v, want the unambiguous lint.go ref and the directory-qualified service.go ref", rep.Unlinked)
	}
}

func TestAnalyze_ValidatesAnchors(t *testing.T) {
	t.Parallel()
	codeAnchor := func(src, target, frag string) index.LinkRow {
		return index.LinkRow{SrcRelPath: src, Target: target, Anchor: frag, Kind: chunk.LinkCode}
	}
	src := &fakeSource{
		files: []string{"design.md", "readme.md"},
		links: []index.LinkRow{
			codeAnchor("readme.md", "design.md", "setup"),   // file + section resolve → unlinked
			codeAnchor("readme.md", "design.md", "missing"), // file resolves, section doesn't → broken
		},
		headings: map[string]map[string]bool{
			"design.md": {"setup": true},
		},
	}
	rep, err := Analyze(src, "")
	if err != nil {
		t.Fatal(err)
	}
	if len(rep.Unlinked) != 1 || rep.Unlinked[0].Anchor != "setup" {
		t.Fatalf("unlinked = %+v, want just design.md#setup", rep.Unlinked)
	}
	if len(rep.Broken) != 1 || rep.Broken[0].Anchor != "missing" || rep.Broken[0].To != "design.md" {
		t.Fatalf("broken = %+v, want design.md#missing (file resolved)", rep.Broken)
	}
}

func TestAnalyze_FlagsDeepRelativeLinks(t *testing.T) {
	t.Parallel()
	mdLink := func(src, target, anchor string) index.LinkRow {
		return index.LinkRow{SrcRelPath: src, Target: target, Anchor: anchor, Kind: chunk.LinkMarkdown}
	}
	src := &fakeSource{
		files: []string{"docs/guides/deep/notes.md", "docs/setup/config.md", "docs/guides/sibling/x.md"},
		links: []index.LinkRow{
			mdLink("docs/guides/deep/notes.md", "../../setup/config.md", ""),        // 2 climbs → flagged
			mdLink("docs/guides/deep/notes.md", "../../setup/config.md", "install"), // 2 climbs + anchor
			mdLink("docs/guides/deep/notes.md", "../sibling/x.md", ""),              // 1 climb → not flagged
			mdLink("docs/guides/deep/notes.md", "/docs/setup/config.md", ""),        // root-absolute → not flagged
			mdLink("a.md", "../../../outside.md", ""),                               // climbs out of tree → skipped
		},
	}
	rep, err := Analyze(src, "")
	if err != nil {
		t.Fatal(err)
	}
	if len(rep.DeepRelative) != 2 {
		t.Fatalf("deep relative = %+v, want 2 (the two ../../ links)", rep.DeepRelative)
	}
	got := map[string]string{} // raw(#anchor) → suggested root-absolute rewrite
	for _, r := range rep.DeepRelative {
		key := r.Raw
		if r.Anchor != "" {
			key += "#" + r.Anchor
		}
		got[key] = r.Suggest
	}
	if got["../../setup/config.md"] != "/docs/setup/config.md" {
		t.Errorf("deep link suggest = %q, want /docs/setup/config.md", got["../../setup/config.md"])
	}
	if got["../../setup/config.md#install"] != "/docs/setup/config.md#install" {
		t.Errorf("anchored deep link suggest = %q, want /docs/setup/config.md#install", got["../../setup/config.md#install"])
	}
}

func TestAnalyze_DeepLinkAnchoredToRepoRoot(t *testing.T) {
	t.Parallel()
	// The vault is indexed at "subproj" within the repo, so a root-absolute
	// suggestion must carry that prefix or it points at a wrong (possibly
	// same-named) file at the repo root.
	mdLink := func(src, target, anchor string) index.LinkRow {
		return index.LinkRow{SrcRelPath: src, Target: target, Anchor: anchor, Kind: chunk.LinkMarkdown}
	}
	src := &fakeSource{
		files: []string{"docs/guides/deep/notes.md", "docs/setup/config.md"},
		links: []index.LinkRow{
			mdLink("docs/guides/deep/notes.md", "../../setup/config.md", ""),
			mdLink("docs/guides/deep/notes.md", "../../setup/config.md", "install"),
		},
	}
	rep, err := Analyze(src, "subproj")
	if err != nil {
		t.Fatal(err)
	}
	if len(rep.DeepRelative) != 2 {
		t.Fatalf("deep relative = %+v, want 2", rep.DeepRelative)
	}
	for _, r := range rep.DeepRelative {
		want := "/subproj/docs/setup/config.md"
		if r.Anchor != "" {
			want += "#" + r.Anchor
		}
		if r.Suggest != want {
			t.Errorf("suggest = %q, want %q", r.Suggest, want)
		}
	}
}

func TestApplyDeepFixes(t *testing.T) {
	t.Parallel()
	// One ref per real occurrence, each with its own line — as Analyze
	// actually produces them, not deduped by destination.
	refs := []Ref{
		{From: "docs/guides/deep/notes.md", Raw: "../../setup/config.md", Suggest: "/docs/setup/config.md", Line: 1},
		{From: "docs/guides/deep/notes.md", Raw: "../../setup/config.md", Anchor: "install", Suggest: "/docs/setup/config.md#install", Line: 1},
		{From: "docs/guides/deep/notes.md", Raw: "../../setup/config.md", Suggest: "/docs/setup/config.md", Line: 2},
	}
	content := "See [config](../../setup/config.md) and [install](../../setup/config.md#install).\n" +
		"Again [c2](../../setup/config.md) here.\n" +
		"Titled [t](../../setup/config.md \"cfg\") stays untouched.\n"

	out, fixes := ApplyDeepFixes(content, refs)

	if strings.Contains(out, "](../../setup/config.md)") {
		t.Errorf("plain deep links should be rewritten; got:\n%s", out)
	}
	if !strings.Contains(out, "[config](/docs/setup/config.md)") ||
		!strings.Contains(out, "[c2](/docs/setup/config.md)") {
		t.Errorf("both plain occurrences should be rewritten; got:\n%s", out)
	}
	if !strings.Contains(out, "[install](/docs/setup/config.md#install)") {
		t.Errorf("anchored link should keep its #fragment; got:\n%s", out)
	}
	// A destination with a link title isn't a verbatim `](dest)` span, so it's
	// left alone rather than mangled.
	if !strings.Contains(out, "[t](../../setup/config.md \"cfg\")") {
		t.Errorf("titled link should be untouched; got:\n%s", out)
	}

	var plain *DeepFix
	for i := range fixes {
		if fixes[i].OldTarget == "../../setup/config.md" {
			plain = &fixes[i]
		}
	}
	if plain == nil || plain.Count != 2 {
		t.Fatalf("plain fix = %+v, want the two-occurrence rewrite", plain)
	}
}

// A code fence can show the exact same `](dest)` text as an illustrative
// example — Analyze never flags it (fenced content isn't parsed for links),
// so the fixer must not touch it either just because the text matches
// somewhere else in the file.
func TestApplyDeepFixes_IgnoresCodeFenceExample(t *testing.T) {
	t.Parallel()
	refs := []Ref{
		{From: "docs/guides/deep/notes.md", Raw: "../../setup/config.md", Suggest: "/docs/setup/config.md", Line: 1},
	}
	content := "See [config](../../setup/config.md) for the real link.\n" +
		"\n" +
		"```\n" +
		"Bad example: [config](../../setup/config.md)\n" +
		"```\n"

	out, fixes := ApplyDeepFixes(content, refs)

	lines := strings.Split(out, "\n")
	if lines[0] != "See [config](/docs/setup/config.md) for the real link." {
		t.Errorf("flagged line should be rewritten; got:\n%s", out)
	}
	if lines[3] != "Bad example: [config](../../setup/config.md)" {
		t.Errorf("code-fence example should be untouched; got:\n%s", out)
	}
	if len(fixes) != 1 || fixes[0].Count != 1 {
		t.Fatalf("fixes = %+v, want exactly 1 rewrite (the flagged line only)", fixes)
	}
}

func TestApplyUnlinkedFixes(t *testing.T) {
	t.Parallel()
	// One ref per real occurrence, each with its own line — as Analyze
	// actually produces them, not deduped by destination.
	refs := []Ref{
		{From: "readme.md", Raw: "docs/design.md", To: "docs/design.md", Line: 1},
		{From: "readme.md", Raw: "docs/design.md", To: "docs/design.md", Line: 1},
		{From: "readme.md", Raw: "SKILL.md", Anchor: "setup", To: "SKILL.md", Line: 1},
		{From: "readme.md", Raw: "internal/graph/graph.go", To: "internal/graph/graph.go", Line: 2},
	}
	content := "See `docs/design.md`, again `docs/design.md`, and `SKILL.md#setup`.\n" +
		"Also `internal/graph/graph.go` and untouched `docs/other.md`.\n"

	out, fixes := ApplyUnlinkedFixes(content, refs, "")

	if strings.Count(out, "[`docs/design.md`](/docs/design.md)") != 2 {
		t.Errorf("both occurrences should become links with the raw path as label and a root-absolute href; got:\n%s", out)
	}
	if !strings.Contains(out, "[`SKILL.md#setup`](/SKILL.md#setup)") {
		t.Errorf("anchored ref should keep its #fragment in both label and href; got:\n%s", out)
	}
	if !strings.Contains(out, "[`internal/graph/graph.go`](/internal/graph/graph.go)") {
		t.Errorf("source-code ref should become a link with the raw path as label and a root-absolute href; got:\n%s", out)
	}
	// Not passed as a ref, so left as inline code.
	if !strings.Contains(out, "`docs/other.md`") {
		t.Errorf("un-flagged ref should be untouched; got:\n%s", out)
	}

	var plain *UnlinkedFix
	for i := range fixes {
		if fixes[i].Target == "docs/design.md" {
			plain = &fixes[i]
		}
	}
	if plain == nil || plain.Count != 2 || plain.Link != "/docs/design.md" {
		t.Fatalf("plain fix = %+v, want the two-occurrence design.md rewrite", plain)
	}
}

// A bare mention and an already-linked mention of the same file can share
// identical code-span text (the link label never carries the target's
// #anchor). ApplyUnlinkedFixes must promote only the bare one, not
// double-wrap the one that's already a real link's label.
func TestApplyUnlinkedFixes_SkipsAlreadyLinkedLabel(t *testing.T) {
	t.Parallel()
	refs := []Ref{
		{From: "readme.md", Raw: "docs/RBAC.md", To: "docs/RBAC.md", Line: 1},
	}
	content := "See [`docs/RBAC.md`](/docs/RBAC.md#operational-tooling) for details, " +
		"also see `docs/RBAC.md` directly.\n"

	out, fixes := ApplyUnlinkedFixes(content, refs, "")

	if strings.Contains(out, "[[`docs/RBAC.md`]") {
		t.Fatalf("already-linked label should not be double-wrapped; got:\n%s", out)
	}
	if !strings.Contains(out, "[`docs/RBAC.md`](/docs/RBAC.md#operational-tooling)") {
		t.Errorf("existing anchored link should be untouched; got:\n%s", out)
	}
	if !strings.Contains(out, "also see [`docs/RBAC.md`](/docs/RBAC.md) directly") {
		t.Errorf("bare mention should be promoted; got:\n%s", out)
	}
	if len(fixes) != 1 || fixes[0].Count != 1 {
		t.Fatalf("fixes = %+v, want exactly 1 rewrite (the bare mention only)", fixes)
	}
}

// The same inline-code text can reappear inside a fenced example or after a
// semantic-ignore directive — Analyze never flags those, so the fixer must
// not promote them just because the bare span text matches elsewhere.
func TestApplyUnlinkedFixes_IgnoresCodeFenceExample(t *testing.T) {
	t.Parallel()
	refs := []Ref{
		{From: "readme.md", Raw: "pkg/foo.go", To: "pkg/foo.go", Line: 1},
	}
	content := "See `pkg/foo.go` for the real reference.\n" +
		"\n" +
		"```\n" +
		"Example: `pkg/foo.go`\n" +
		"```\n"

	out, fixes := ApplyUnlinkedFixes(content, refs, "")

	lines := strings.Split(out, "\n")
	if lines[0] != "See [`pkg/foo.go`](/pkg/foo.go) for the real reference." {
		t.Errorf("flagged line should be promoted; got:\n%s", out)
	}
	if lines[3] != "Example: `pkg/foo.go`" {
		t.Errorf("code-fence example should be untouched; got:\n%s", out)
	}
	if len(fixes) != 1 || fixes[0].Count != 1 {
		t.Fatalf("fixes = %+v, want exactly 1 rewrite (the flagged line only)", fixes)
	}
}

func TestAuditTOCs(t *testing.T) {
	t.Parallel()
	long := func(withTOC bool) string {
		var b strings.Builder
		b.WriteString("# Title\n\n")
		if withTOC {
			b.WriteString("## Contents\n\n- Section 1\n- Section 2\n\n")
		}
		b.WriteString("## Section 1\n\n## Section 2\n\n")
		for range 120 {
			b.WriteString("filler line\n")
		}
		return b.String()
	}
	content := map[string]string{
		"long-missing.md": long(false),                         // >100 lines, no TOC → missing
		"long-current.md": long(true),                          // >100 lines, current TOC → ok
		"short.md":        "# Short\n\n## A\n\ntext\n",         // under threshold → skipped
		"no-headings.md":  strings.Repeat("prose line\n", 150), // long but nothing to tabulate → skipped
		"notes.txt":       long(false),                         // not markdown → skipped
		// A verbatim third-party document opts out of restructuring entirely.
		"vendored.md": "<!-- semantic-ignore-file -->\n" + long(false),
		// The same directive shown as an example can't suppress its own file.
		"documents-it.md": "```\n<!-- semantic-ignore-file -->\n```\n" + long(false),
	}
	read := func(rel string) (string, error) { return content[rel], nil }

	files := []string{"long-missing.md", "long-current.md", "short.md", "no-headings.md", "notes.txt", "vendored.md", "documents-it.md"}
	got, err := AuditTOCs(files, read)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 2 {
		t.Fatalf("findings = %+v, want long-missing.md and documents-it.md", got)
	}
	for _, f := range got {
		if f.Reason != "missing" {
			t.Errorf("finding = %+v, want reason missing", f)
		}
		if f.File != "long-missing.md" && f.File != "documents-it.md" {
			t.Errorf("unexpected finding %+v", f)
		}
	}
}

func TestKeepFiles(t *testing.T) {
	t.Parallel()
	refs := []Ref{
		{From: "a.md", Line: 1},
		{From: "docs/b.md", Line: 2},
		{From: "a.md", Line: 9},
		{From: "c.md", Line: 3},
	}
	got := KeepFiles(refs, map[string]bool{"a.md": true, "docs/b.md": true})
	if len(got) != 3 {
		t.Fatalf("KeepFiles kept %d refs, want 3: %+v", len(got), got)
	}
	for _, r := range got {
		if r.From == "c.md" {
			t.Errorf("c.md was not in scope but survived: %+v", got)
		}
	}
	// An empty scope keeps nothing.
	if kept := KeepFiles(refs, nil); len(kept) != 0 {
		t.Errorf("KeepFiles with empty scope kept %d, want 0", len(kept))
	}
}

func TestAnalyze_NoCodeRefs(t *testing.T) {
	t.Parallel()
	src := &fakeSource{
		files: []string{"a.md"},
		links: []index.LinkRow{{SrcRelPath: "a.md", Target: "b.md", Kind: chunk.LinkMarkdown}},
	}
	rep, err := Analyze(src, "")
	if err != nil {
		t.Fatal(err)
	}
	if len(rep.Unlinked) != 0 || len(rep.Broken) != 0 {
		t.Errorf("want empty report, got %+v", rep)
	}
}
