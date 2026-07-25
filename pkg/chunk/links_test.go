// Copyright (c) 2026 Reactor Technologies, Inc.
// SPDX-License-Identifier: Apache-2.0

package chunk

import "testing"

func TestLinks_ExtractsAndFilters(t *testing.T) {
	t.Parallel()
	content := "# Title\n" + // line 1
		"\n" + // line 2
		"See [the design](design.md) and [[Other Note]].\n" + // line 3
		"A [deep ref](../up/ref.md#section) and [alias|x](notes/todo).\n" + // line 4
		"External [site](https://example.com), mail [me](mailto:a@b.c), anchor [top](#top).\n" + // line 5
		"\n" +
		"```go\n" + // fenced code — links inside are ignored
		"x := \"[not a link](nope.md)\"\n" +
		"```\n" +
		"\n" +
		"Transclusion ![[Embedded#heading|alias]].\n"

	got := Links(content)

	type key struct {
		target string
		kind   string
	}
	seen := map[key]int{} // → line
	for _, l := range got {
		seen[key{l.Target, l.Kind}] = l.Line
	}

	want := []struct {
		target string
		kind   string
		line   int
	}{
		{"design.md", LinkMarkdown, 3},
		{"Other Note", LinkWiki, 3},
		{"../up/ref.md", LinkMarkdown, 4}, // #section stripped
		{"notes/todo", LinkMarkdown, 4},
		{"Embedded", LinkWiki, 11}, // #heading and |alias stripped
	}
	for _, w := range want {
		line, ok := seen[key{w.target, w.kind}]
		if !ok {
			t.Errorf("missing link %q (%s); got %v", w.target, w.kind, got)
			continue
		}
		if line != w.line {
			t.Errorf("link %q on line %d, want %d", w.target, line, w.line)
		}
	}

	// External, mailto, pure-anchor, and in-code links must not appear.
	for _, bad := range []key{
		{"https://example.com", LinkMarkdown},
		{"mailto:a@b.c", LinkMarkdown},
		{"#top", LinkMarkdown},
		{"nope.md", LinkMarkdown},
	} {
		if _, ok := seen[bad]; ok {
			t.Errorf("should not have extracted %q (%s)", bad.target, bad.kind)
		}
	}
}

func TestLinks_Anchors(t *testing.T) {
	t.Parallel()
	content := "# T\n" + // 1
		"\n" + // 2
		"A [md](design.md#setup) link and a bare [x](design.md).\n" + // 3
		"A wiki [[Other Note#Getting Started|alias]] link.\n" + // 4
		"An inline `docs/api.md#endpoints` ref and `SKILL.md?v=2` (query only).\n" // 5

	type ta struct {
		target, anchor, kind string
	}
	got := map[ta]bool{}
	for _, l := range Links(content) {
		got[ta{l.Target, l.Anchor, l.Kind}] = true
	}

	for _, w := range []ta{
		{"design.md", "setup", LinkMarkdown},        // #anchor split from path
		{"design.md", "", LinkMarkdown},             // no anchor
		{"Other Note", "Getting Started", LinkWiki}, // wiki #heading kept, |alias dropped
		{"docs/api.md", "endpoints", LinkCode},      // inline-code #anchor kept
		{"SKILL.md", "", LinkCode},                  // ?query dropped, no anchor
	} {
		if !got[w] {
			t.Errorf("missing link %+v; got %v", w, got)
		}
	}
}

// A bare extension names a file type, not a file. Docs that tabulate the
// extensions a tool handles write them as inline code, and promoting those to
// links would point at nothing.
func TestLinks_BareExtensionIsNotAPath(t *testing.T) {
	t.Parallel()
	content := "# T\n\nHandles `.go` and `.md`, but `main.go` and `.hidden.md` are files.\n"

	var code []string
	for _, l := range Links(content) {
		if l.Kind == LinkCode {
			code = append(code, l.Target)
		}
	}

	want := map[string]bool{"main.go": true, ".hidden.md": true}
	for _, tgt := range code {
		if !want[tgt] {
			t.Errorf("extracted %q as a path reference; a bare extension is a file type", tgt)
		}
		delete(want, tgt)
	}
	for tgt := range want {
		t.Errorf("missing path reference %q", tgt)
	}
}

func TestLinks_FrontmatterLineOffset(t *testing.T) {
	t.Parallel()
	content := "---\ntitle: T\ntags: [a]\n---\n\nBody links [x](y.md).\n"
	got := Links(content)
	if len(got) != 1 {
		t.Fatalf("want 1 link, got %d: %v", len(got), got)
	}
	// Frontmatter is 4 lines + blank; the link sits on line 6 of the file.
	if got[0].Target != "y.md" || got[0].Line != 6 {
		t.Errorf("got %+v, want y.md on line 6", got[0])
	}
}

func TestLinks_CodePaths(t *testing.T) {
	t.Parallel()
	content := "# T\n" + // 1
		"\n" + // 2
		"See `docs/design.md` and `SKILL.md#setup`.\n" + // 3
		"Run `make echo` and note `foo.txt`.\n" + // 4
		"\n" + // 5
		"```\n" + // 6
		"`inside/code.md`\n" + // 7 — fenced, raw text, not a code span
		"```\n" // 8

	seen := map[string]int{} // code target → line
	for _, l := range Links(content) {
		if l.Kind == LinkCode {
			seen[l.Target] = l.Line
		}
	}

	if seen["docs/design.md"] != 3 {
		t.Errorf("want docs/design.md code ref on line 3; got %v", seen)
	}
	if seen["SKILL.md"] != 3 { // #setup stripped
		t.Errorf("want SKILL.md code ref (anchor stripped) on line 3; got %v", seen)
	}
	// A command with a space, a non-doc extension, and a path inside a fenced
	// block are all not doc-path code refs.
	for _, bad := range []string{"make echo", "foo.txt", "inside/code.md"} {
		if _, ok := seen[bad]; ok {
			t.Errorf("should not have emitted code ref %q", bad)
		}
	}
}

func TestLinks_CodePaths_GoSource(t *testing.T) {
	t.Parallel()
	content := "# T\n" + // 1
		"\n" + // 2
		"See `internal/graph/graph.go` for the resolver.\n" // 3

	seen := map[string]int{}
	for _, l := range Links(content) {
		if l.Kind == LinkCode {
			seen[l.Target] = l.Line
		}
	}
	if seen["internal/graph/graph.go"] != 3 {
		t.Errorf("want internal/graph/graph.go code ref on line 3; got %v", seen)
	}
}

// A doc-path code span that's already a real link's label (the form
// `lint --fix` writes: [`path`](/path)) must not also surface as a bare
// LinkCode mention — else --fix's own output re-flags as unlinked and a
// second --fix run corrupts it into a nested link.
func TestLinks_CodePaths_NotInsideRealLink(t *testing.T) {
	t.Parallel()
	content := "See [`internal/graph/graph.go`](/internal/graph/graph.go) for the resolver.\n"

	links := Links(content)
	for _, l := range links {
		if l.Kind == LinkCode {
			t.Errorf("code span inside a real link's label should not also be a LinkCode ref; got %+v", l)
		}
	}
	found := false
	for _, l := range links {
		if l.Kind == LinkMarkdown && l.Target == "/internal/graph/graph.go" {
			found = true
		}
	}
	if !found {
		t.Errorf("want the real markdown link itself preserved; got %+v", links)
	}
}

func TestLinks_IgnoreDirectives(t *testing.T) {
	t.Parallel()
	content := "# T\n" + // 1
		"\n" + // 2
		"Kept [a](a.md) and `keep/me.md`.\n" + // 3 — both survive
		"Inline ignore `skip/here.md`. <!-- semantic-ignore -->\n" + // 4 — suppressed
		"<!-- semantic-ignore-next-line: template -->\n" + // 5
		"Placeholder `docs/<Feature>.md` and [b](b.md).\n" + // 6 — both suppressed
		// The reason names the placeholder it suppresses, so it carries a `>`.
		// The directive still runs to `-->`, not to that first `>`.
		"Angled `pkg/<lang>.go`. <!-- semantic-ignore: <lang> is a placeholder -->\n" // 7 — suppressed

	seen := map[string]bool{}
	for _, l := range Links(content) {
		seen[l.Target] = true
	}

	for _, want := range []string{"a.md", "keep/me.md"} {
		if !seen[want] {
			t.Errorf("expected %q to survive; got %v", want, seen)
		}
	}
	for _, gone := range []string{"skip/here.md", "docs/<Feature>.md", "b.md", "pkg/<lang>.go"} {
		if seen[gone] {
			t.Errorf("expected %q to be suppressed by semantic-ignore", gone)
		}
	}
}

func TestLinks_IgnoreFile(t *testing.T) {
	t.Parallel()
	content := "<!-- semantic-ignore-file -->\n\nAll [a](a.md) and `b/c.md` gone.\n"
	if got := Links(content); got != nil {
		t.Errorf("semantic-ignore-file should suppress everything; got %v", got)
	}
}

func TestLinks_None(t *testing.T) {
	t.Parallel()
	if got := Links("# Just a heading\n\nProse with no links.\n"); got != nil {
		t.Errorf("want nil, got %v", got)
	}
}
