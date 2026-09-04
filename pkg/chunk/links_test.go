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

	got := Links("note.md", content)

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
	for _, l := range Links("note.md", content) {
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
	for _, l := range Links("note.md", content) {
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
	got := Links("note.md", content)
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
	for _, l := range Links("note.md", content) {
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
	for _, l := range Links("note.md", content) {
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

	links := Links("note.md", content)
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

func TestLinks_WikilinkInsideCodeSpanIsNotAnEdge(t *testing.T) {
	t.Parallel()
	// Wikilinks are matched over raw source, so nothing about the AST's view of
	// inline code applies unless it is applied by offset. Documenting the syntax
	// is the case that exposes it: semantic's own README describes `[[wikilink]]`
	// edges, and reported a broken link to a note called "wikilink".
	content := "The `graph` command resolves `[[wikilink]]` edges at query time.\n\n" +
		"A real one is [[actual-note]] here.\n"

	var wiki []string
	for _, l := range Links("note.md", content) {
		if l.Kind == LinkWiki {
			wiki = append(wiki, l.Target)
		}
	}
	if len(wiki) != 1 || wiki[0] != "actual-note" {
		t.Errorf("wiki targets = %v, want only actual-note", wiki)
	}
}

func TestLinks_WikilinkInsideFencedBlockIsNotAnEdge(t *testing.T) {
	t.Parallel()
	// The fenced-block half of the same rule, which already held — pinned so a
	// change to how either kind of code is excluded cannot quietly drop it.
	content := "Example:\n\n```\n[[not-an-edge]]\n```\n\nBut [[real-note]] is one.\n"

	var wiki []string
	for _, l := range Links("note.md", content) {
		if l.Kind == LinkWiki {
			wiki = append(wiki, l.Target)
		}
	}
	if len(wiki) != 1 || wiki[0] != "real-note" {
		t.Errorf("wiki targets = %v, want only real-note", wiki)
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
	for _, l := range Links("note.md", content) {
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
	if got := Links("note.md", content); got != nil {
		t.Errorf("semantic-ignore-file should suppress everything; got %v", got)
	}
}

func TestLinks_None(t *testing.T) {
	t.Parallel()
	if got := Links("note.md", "# Just a heading\n\nProse with no links.\n"); got != nil {
		t.Errorf("want nil, got %v", got)
	}
}

// The JSX-href and MDX-comment cases below pin the two ways an .mdx file
// differs from a .md one at extraction time. Both are gated on the extension,
// so each has a .md counterpart asserting the old behaviour is untouched.

func TestLinks_ExtractsJSXHrefsInMDX(t *testing.T) {
	t.Parallel()
	content := "# Title\n" + // line 1
		"\n" + // line 2
		"<Card title=\"Deploy\" href=\"/deploy/overview\" />\n" + // line 3
		"<Card title=\"Quoted\" href='/deploy/quickstart' />\n" + // line 4
		"<Card title=\"External\" href=\"https://example.com\" />\n" + // line 5
		"<Card title=\"Anchor\" href=\"#top\" />\n" + // line 6
		"\n" +
		"```jsx\n" + // fenced: an example must not yield an edge
		"<Card href=\"/not/an/edge\" />\n" +
		"```\n" +
		"\n" +
		"Inline `<Card href=\"/also/not/an/edge\" />` in prose.\n" // line 12

	seen := map[string]int{} // target → line
	for _, l := range Links("page.mdx", content) {
		if l.Kind == LinkMarkdown {
			seen[l.Target] = l.Line
		}
	}

	for _, tc := range []struct {
		target string
		line   int
	}{
		{"/deploy/overview", 3},
		{"/deploy/quickstart", 4},
	} {
		if got, ok := seen[tc.target]; !ok {
			t.Errorf("href %q not extracted; got %v", tc.target, seen)
		} else if got != tc.line {
			t.Errorf("href %q on line %d, want %d", tc.target, got, tc.line)
		}
	}

	for _, gone := range []string{"https://example.com", "#top", "/not/an/edge", "/also/not/an/edge"} {
		if _, ok := seen[gone]; ok {
			t.Errorf("href %q should not be an edge; got %v", gone, seen)
		}
	}
}

// An href written as a JSX expression is the same link in JSX's other syntax,
// so `{"/x"}` is an edge. An expression that computes a path at render time is
// not: emitting the literal inside it would name a target no page has, which
// reads as a broken link rather than as an expression nothing can resolve.
func TestLinks_ExtractsJSXHrefExpressionForms(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name string
		attr string
		want string // "" = no edge
	}{
		{"double-quoted attribute", `href="/a/one"`, "/a/one"},
		{"single-quoted attribute", `href='/a/two'`, "/a/two"},
		{"space around equals", `href = "/a/three"`, "/a/three"},
		{"braced double quotes", `href={"/a/four"}`, "/a/four"},
		{"braced single quotes", `href={'/a/five'}`, "/a/five"},
		{"braced template literal", "href={`/a/six`}", "/a/six"},
		{"braced with inner spaces", "href={ `/a/seven` }", "/a/seven"},
		{"plain HTML anchor", `href="/a/eight"`, "/a/eight"},

		{"concatenation names a fragment", `href={base + "/a/nine"}`, ""},
		{"interpolation is render-time", "href={`/a/${id}`}", ""},
		{"bare identifier", `href={url}`, ""},
		{"absolute URL", `href={"https://example.com/x"}`, ""},
		{"empty", `href={""}`, ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			var got []string
			for _, l := range Links("page.mdx", "<Card "+tc.attr+" />\n") {
				got = append(got, l.Target)
			}
			if tc.want == "" {
				if len(got) != 0 {
					t.Errorf("%s: want no edge, got %v", tc.attr, got)
				}
				return
			}
			if len(got) != 1 || got[0] != tc.want {
				t.Errorf("%s: want [%s], got %v", tc.attr, tc.want, got)
			}
		})
	}
}

func TestLinks_IgnoresJSXHrefsInPlainMarkdown(t *testing.T) {
	t.Parallel()
	// The same content in a .md file yields nothing: an HTML <a href> in
	// ordinary markdown was never an edge, and .md behaviour must not move.
	content := "# Title\n\n<a href=\"/deploy/overview\">Deploy</a>\n"
	for _, l := range Links("note.md", content) {
		if l.Target == "/deploy/overview" {
			t.Errorf("href extracted from a .md file: %+v", l)
		}
	}
}

func TestLinks_MDXCommentSuppresses(t *testing.T) {
	t.Parallel()
	// MDX has no HTML comments, so {/* */} is the only escape hatch there.
	content := "# Title\n" +
		"\n" +
		"A `placeholder.mdx` reference. {/* semantic-ignore: prose placeholder */}\n" + // line 3
		"{/* semantic-ignore-next-line */}\n" + // line 4
		"Another `next-line.mdx` reference.\n" + // line 5
		"A live `real.mdx` reference.\n" // line 6

	var targets []string
	for _, l := range Links("page.mdx", content) {
		targets = append(targets, l.Target)
	}

	if len(targets) != 1 || targets[0] != "real.mdx" {
		t.Errorf("MDX ignore directives not honored: got %v, want [real.mdx]", targets)
	}
}

func TestLinks_BothCommentSyntaxesSuppress(t *testing.T) {
	t.Parallel()
	// Accepting both forms everywhere is deliberate: a directive must never
	// silently do nothing because it was written in the other flavour's syntax.
	for _, tc := range []struct {
		name      string
		file      string
		directive string
	}{
		{"html form in md", "note.md", "<!-- semantic-ignore -->"},
		{"mdx form in md", "note.md", "{/* semantic-ignore */}"},
		{"html form in mdx", "page.mdx", "<!-- semantic-ignore -->"},
		{"mdx form in mdx", "page.mdx", "{/* semantic-ignore */}"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			content := "# Title\n\nA `suppressed.md` reference. " + tc.directive + "\n"
			if got := Links(tc.file, content); len(got) != 0 {
				t.Errorf("directive %q did not suppress: got %+v", tc.directive, got)
			}
		})
	}
}

func TestIgnoresFile_BothCommentSyntaxes(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name    string
		content string
		want    bool
	}{
		{"html form", "<!-- semantic-ignore-file -->\n\n# Title\n", true},
		{"mdx form", "{/* semantic-ignore-file */}\n\n# Title\n", true},
		{"mdx form with reason", "{/* semantic-ignore-file: generated */}\n\n# Title\n", true},
		{"line-scoped directive is not file-scoped", "{/* semantic-ignore */}\n\n# Title\n", false},
		{"no directive", "# Title\n", false},
		{"inside a fence does not count", "```\n{/* semantic-ignore-file */}\n```\n", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			if got := IgnoresFile(tc.content); got != tc.want {
				t.Errorf("IgnoresFile() = %v, want %v", got, tc.want)
			}
		})
	}
}

func TestIsMarkdownAndIsMDX(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		path       string
		isMarkdown bool
		isMDX      bool
	}{
		{"a.md", true, false},
		{"a.markdown", true, false},
		{"a.mdx", true, true},
		{"a.MDX", true, true},
		{"dir/b.mdx", true, true},
		{"a.go", false, false},
		{"a.txt", false, false},
		{"mdx", false, false},
	} {
		t.Run(tc.path, func(t *testing.T) {
			t.Parallel()
			if got := IsMarkdown(tc.path); got != tc.isMarkdown {
				t.Errorf("IsMarkdown(%q) = %v, want %v", tc.path, got, tc.isMarkdown)
			}
			if got := IsMDX(tc.path); got != tc.isMDX {
				t.Errorf("IsMDX(%q) = %v, want %v", tc.path, got, tc.isMDX)
			}
		})
	}
}
