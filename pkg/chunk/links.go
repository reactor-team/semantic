package chunk

import (
	"path"
	"regexp"
	"strings"
	"unicode"

	"github.com/yuin/goldmark"
	"github.com/yuin/goldmark/ast"
	"github.com/yuin/goldmark/text"
)

// Link kind constants — the syntax an edge was written in.
const (
	LinkMarkdown = "md"   // [text](dest)
	LinkWiki     = "wiki" // [[target]] / [[target|alias]] / ![[embed]]
	LinkCode     = "code" // `path/to/doc.md` written as inline code, not a link
)

// Link is one outbound reference from a document. Target is the raw
// destination as written (a relative path or a wikilink target); resolving it
// to an actual indexed file happens in the graph layer, which has the whole
// file set. Anchor is the #section fragment, if any, kept apart from Target so
// the graph/lint layers can validate it against the target file's headings.
// External URLs (http, mailto, …) and pure #anchors are not emitted — only
// candidate vault-internal references become edges. LinkCode references are
// doc or source-code paths written as inline code; they aren't real edges
// (the graph drops them) but the lint layer flags them as pointers that could
// be links.
type Link struct {
	Target string // raw destination, #anchor/?query/|alias stripped
	Anchor string // #section fragment, without the leading '#'; empty if none
	Line   int    // 1-based source line the link appears on
	Kind   string // LinkMarkdown | LinkWiki | LinkCode
}

// wikilinkPattern matches Obsidian-style [[target]] and ![[embed]]; the
// capture is everything between the brackets (target, optional #heading and
// |alias, stripped later). goldmark doesn't parse wikilinks, so they're found
// by scanning the source.
var wikilinkPattern = regexp.MustCompile(`\[\[([^\[\]]+)\]\]`)

// urlSchemePattern matches an external link scheme (http:, mailto:, …), used
// to drop links that point outside the vault.
var urlSchemePattern = regexp.MustCompile(`^[a-zA-Z][a-zA-Z0-9+.-]*:`)

// jsxHrefPattern matches the href of a JSX element (`<Card href="/x" />`).
// Docs sites written in MDX put a large share of their navigation in
// components rather than in prose links, and goldmark has no node for one —
// an unknown tag arrives as raw HTML — so these are found by scanning the
// source, exactly as wikilinks are. A non-participating group is -1 in the
// match, so the caller takes whichever matched.
//
// A braced expression counts only when it holds one whole literal and nothing
// else: `{"/x"}` is the same link written in JSX's expression form, whereas
// `{base + "/x"}` and “ {`/x/${id}`} “ name a fragment of a path computed at
// render time. Matching those would emit a target no page has, which reads as
// a broken link rather than as the unresolvable expression it is — so the
// template form excludes `$` and `{`, and the closing brace must follow the
// literal directly.
var jsxHrefPattern = regexp.MustCompile(
	`href\s*=\s*(?:` +
		`"([^"]*)"|'([^']*)'` + // attribute form
		"|\\{\\s*(?:\"([^\"]*)\"|'([^']*)'|`([^`${]*)`)\\s*\\}" + // expression form
		`)`)

// ignorePattern matches an ESLint-style suppression directive in either comment
// syntax: `semantic-ignore` drops references on the same line, `-next-line` the
// following line, `-file` the whole file. An optional trailing reason
// (`<!-- semantic-ignore: template placeholder -->`) is allowed, and runs to
// the closing delimiter rather than the first `>`, so it can name the very thing
// being suppressed (`<lang>`, `Foo<T>`) without silently voiding the directive.
//
// Both forms are accepted for every file type rather than gated on IsMDX. MDX
// has no HTML comments, so the `<!-- -->` form cannot suppress anything in an
// .mdx file and the `{/* */}` form is the only escape hatch there; accepting
// both everywhere costs nothing and means a directive never silently does
// nothing because it was written in the other flavour's syntax.
var ignorePattern = regexp.MustCompile(`(?:<!--\s*semantic-ignore(-next-line|-file)?\b.*?-->)|(?:\{/\*\s*semantic-ignore(-next-line|-file)?\b.*?\*/\})`)

// Links extracts outbound document references from markdown content. Inline
// `[text](dest)` links come from the goldmark AST, so links written inside
// code blocks are naturally ignored; `[[wikilinks]]` come from a source scan
// (goldmark leaves them as literal text) that skips matches inside code
// blocks. Inline-code spans that look like doc or source-code paths
// (`docs/foo.md`, `internal/foo/bar.go`) are also emitted as LinkCode — not
// edges, but candidates the lint layer surfaces.
// In an MDX file, a JSX element's href (`<Card href="/x" />`) is also emitted,
// as an ordinary LinkMarkdown edge — it is a link, just written as an
// attribute. name selects that behaviour by extension; a plain .md file yields
// exactly the edges it did before, so an HTML `<a href>` in one is still left
// alone.
// Frontmatter is stripped first so line numbers map to the file. A
// `semantic-ignore` directive suppresses references on its line (see
// applyIgnores).
func Links(name, content string) []Link {
	_, body := splitFrontmatter(content)
	baseLine := strings.Count(content[:len(content)-len(body)], "\n")

	trimmed := strings.TrimSpace(body)
	if trimmed == "" {
		return nil
	}
	// lead mirrors chunkBody: offsets into source map back to body as
	// lead+offset, and the file line is baseLine + the body-relative line.
	lead := len(body) - len(strings.TrimLeftFunc(body, unicode.IsSpace))
	source := []byte(trimmed)
	lineOf := func(off int) int { return baseLine + lineAt(body, lead+off) }

	md := goldmark.New()
	root := md.Parser().Parse(text.NewReader(source))

	var out []Link
	var codeRanges [][2]int
	var spanRanges [][2]int // inline-code spans, kept apart from codeRanges: they exclude a wikilink, not an ignore directive
	linkDepth := 0          // >0 while walking a real [text](dest) link's label — its code spans are already linked, not bare mentions
	_ = ast.Walk(root, func(n ast.Node, entering bool) (ast.WalkStatus, error) {
		if _, ok := n.(*ast.Link); ok {
			if entering {
				linkDepth++
			} else {
				linkDepth--
			}
		}
		if !entering {
			return ast.WalkContinue, nil
		}
		switch node := n.(type) {
		case *ast.FencedCodeBlock, *ast.CodeBlock:
			if lines := n.Lines(); lines != nil && lines.Len() > 0 {
				codeRanges = append(codeRanges, [2]int{lines.At(0).Start, lines.At(lines.Len() - 1).Stop})
			}
		case *ast.Link:
			if tgt, anchor, ok := internalTarget(string(node.Destination)); ok {
				out = append(out, Link{Target: tgt, Anchor: anchor, Kind: LinkMarkdown, Line: lineOf(nodeOffset(n))})
			}
		case *ast.CodeSpan:
			if r, ok := codeSpanRange(node); ok {
				spanRanges = append(spanRanges, r)
			}
			if linkDepth > 0 {
				return ast.WalkContinue, nil
			}
			if tgt, anchor, ok := codePathTarget(codeSpanText(node, source)); ok {
				out = append(out, Link{Target: tgt, Anchor: anchor, Kind: LinkCode, Line: lineOf(nodeOffset(n))})
			}
		}
		return ast.WalkContinue, nil
	})

	// Wikilinks are found by scanning the raw source, because goldmark has no
	// node for them. That bypasses everything the AST already knows about where
	// code sits, so both kinds of code have to be excluded by offset: fenced
	// blocks, and inline spans. Without the latter, prose that documents the
	// syntax — "`[[wikilink]]` edges" — yields an edge to a note called
	// "wikilink", which is then reported as broken.
	for _, m := range wikilinkPattern.FindAllSubmatchIndex(source, -1) {
		if offsetInRanges(m[0], codeRanges) || offsetInRanges(m[0], spanRanges) {
			continue
		}
		if tgt, anchor := wikiTarget(string(source[m[2]:m[3]])); tgt != "" {
			out = append(out, Link{Target: tgt, Anchor: anchor, Kind: LinkWiki, Line: lineOf(m[0])})
		}
	}
	// JSX hrefs, MDX only, and excluded by offset from both kinds of code for
	// the same reason wikilinks are: a page documenting the syntax must not
	// yield an edge to whatever its example points at.
	if IsMDX(name) {
		for _, m := range jsxHrefPattern.FindAllSubmatchIndex(source, -1) {
			if offsetInRanges(m[0], codeRanges) || offsetInRanges(m[0], spanRanges) {
				continue
			}
			dest, ok := submatch(source, m, 1, 2, 3, 4, 5)
			if !ok {
				continue
			}
			if tgt, anchor, ok := internalTarget(dest); ok {
				out = append(out, Link{Target: tgt, Anchor: anchor, Kind: LinkMarkdown, Line: lineOf(m[0])})
			}
		}
	}

	return applyIgnores(out, source, codeRanges, lineOf)
}

// submatch returns the text of the first participating group among groups, for
// a pattern whose alternatives each capture the same value in a group of their
// own — the two quote styles of a JSX href, the two comment syntaxes of an
// ignore directive. Reports false when none of them matched, which for an
// optional group is itself the answer rather than an error.
func submatch(source []byte, m []int, groups ...int) (string, bool) {
	for _, g := range groups {
		if lo, hi := m[2*g], m[2*g+1]; lo >= 0 {
			return string(source[lo:hi]), true
		}
	}
	return "", false
}

// IgnoresFile reports whether markdown content opts out of linting entirely
// with a `semantic-ignore-file` directive, in either comment syntax. Checks
// that read whole files rather than references — the Contents TOC audit —
// consult this so a file-level directive means what it says. A directive
// inside a code block doesn't count, so documenting the syntax can't suppress
// the file that documents it.
func IgnoresFile(content string) bool {
	_, body := splitFrontmatter(content)
	source := []byte(strings.TrimSpace(body))
	if len(source) == 0 {
		return false
	}
	var codeRanges [][2]int
	root := goldmark.New().Parser().Parse(text.NewReader(source))
	_ = ast.Walk(root, func(n ast.Node, entering bool) (ast.WalkStatus, error) {
		if !entering {
			return ast.WalkContinue, nil
		}
		switch n.(type) {
		case *ast.FencedCodeBlock, *ast.CodeBlock:
			if lines := n.Lines(); lines != nil && lines.Len() > 0 {
				codeRanges = append(codeRanges, [2]int{lines.At(0).Start, lines.At(lines.Len() - 1).Stop})
			}
		}
		return ast.WalkContinue, nil
	})
	for _, m := range ignorePattern.FindAllSubmatchIndex(source, -1) {
		if offsetInRanges(m[0], codeRanges) {
			continue
		}
		if suffix, ok := submatch(source, m, 1, 2); ok && suffix == "-file" {
			return true
		}
	}
	return false
}

// applyIgnores drops references suppressed by semantic-ignore directives: a
// whole-file directive clears everything, otherwise same-line and next-line
// directives remove references on the marked line. Directives inside code
// blocks are themselves ignored so a documented example can't suppress.
func applyIgnores(links []Link, source []byte, codeRanges [][2]int, lineOf func(int) int) []Link {
	var ignoreFile bool
	ignoreLines := map[int]bool{}
	for _, m := range ignorePattern.FindAllSubmatchIndex(source, -1) {
		if offsetInRanges(m[0], codeRanges) {
			continue
		}
		line := lineOf(m[0])
		suffix, _ := submatch(source, m, 1, 2)
		switch suffix {
		case "-file":
			ignoreFile = true
		case "-next-line":
			ignoreLines[line+1] = true
		default: // no suffix → same line
			ignoreLines[line] = true
		}
	}
	if ignoreFile {
		return nil
	}
	if len(ignoreLines) == 0 {
		return links
	}
	kept := links[:0]
	for _, l := range links {
		if !ignoreLines[l.Line] {
			kept = append(kept, l)
		}
	}
	return kept
}

// internalTarget normalizes a markdown link destination and reports whether
// it's a vault-internal reference worth tracking. External schemes and
// protocol-relative URLs are dropped, as are pure #anchors; the path and its
// #section fragment are returned separately (any ?query is discarded).
func internalTarget(dest string) (target, anchor string, ok bool) {
	dest = strings.TrimSpace(dest)
	if dest == "" || strings.HasPrefix(dest, "#") || strings.HasPrefix(dest, "//") {
		return "", "", false
	}
	if urlSchemePattern.MatchString(dest) {
		return "", "", false
	}
	dest, anchor = splitAnchor(dest)
	dest = strings.TrimSpace(dest)
	if dest == "" {
		return "", "", false
	}
	return dest, anchor, true
}

// splitAnchor separates a raw target into its path and #section fragment,
// discarding any ?query. "foo.md#bar?x=1" → ("foo.md", "bar"). The fragment
// is returned without its leading '#'.
func splitAnchor(s string) (p, anchor string) {
	p = s
	if i := strings.IndexByte(p, '#'); i >= 0 {
		anchor = p[i+1:]
		p = p[:i]
		if j := strings.IndexByte(anchor, '?'); j >= 0 {
			anchor = anchor[:j]
		}
	}
	if j := strings.IndexByte(p, '?'); j >= 0 {
		p = p[:j]
	}
	return strings.TrimSpace(p), strings.TrimSpace(anchor)
}

// wikiTarget extracts the link target and #heading from a wikilink's inner
// text, dropping a |alias. Target is empty when only an alias/heading is
// present (e.g. a same-file [[#heading]]).
func wikiTarget(inner string) (target, anchor string) {
	if i := strings.IndexByte(inner, '|'); i >= 0 {
		inner = inner[:i]
	}
	if i := strings.IndexByte(inner, '#'); i >= 0 {
		anchor = strings.TrimSpace(inner[i+1:])
		inner = inner[:i]
	}
	return strings.TrimSpace(inner), anchor
}

// codeSpanText concatenates the text segments of an inline-code span into its
// literal content.
func codeSpanText(n ast.Node, source []byte) string {
	var b strings.Builder
	for c := n.FirstChild(); c != nil; c = c.NextSibling() {
		if t, ok := c.(*ast.Text); ok {
			b.Write(t.Segment.Value(source))
		}
	}
	return b.String()
}

// codePathTarget reports whether an inline-code span looks like a doc- or
// source-path reference — a single token ending in a markdown or Go
// extension, e.g. `docs/design.md`, `SKILL.md`, or `internal/graph/graph.go`
// — and returns its path and #section fragment separately (any ?query
// discarded). Spans with whitespace (prose or commands like `make echo`),
// URLs, and other extensions are rejected, keeping the lint to references
// that plausibly want to be links.
func codePathTarget(s string) (target, anchor string, ok bool) {
	// A path written as inline code is a single token; prose or a shell
	// command (`make echo`) has interior whitespace — reject it before the
	// shared normalization, which doesn't screen for it.
	if strings.ContainsAny(strings.TrimSpace(s), " \t\n") {
		return "", "", false
	}
	target, anchor, ok = internalTarget(s)
	if !ok {
		return "", "", false
	}
	ext := path.Ext(target)
	switch strings.ToLower(ext) {
	case ".md", ".markdown", ".mdx", ".go":
	default:
		return "", "", false
	}
	// A span that is nothing but the extension (`` `.go` ``) names a file type,
	// not a file — the shape a docs table listing extensions uses. Requiring a
	// stem keeps those out of the report without a per-line directive.
	if path.Base(target) == ext {
		return "", "", false
	}
	return target, anchor, true
}

// nodeOffset returns the source offset of a link's first text segment, used
// for its line number. Falls back to 0 (→ first body line) for a link with no
// text label, e.g. `[](dest)`.
func nodeOffset(n ast.Node) int {
	for c := n.FirstChild(); c != nil; c = c.NextSibling() {
		if t, ok := c.(*ast.Text); ok {
			return t.Segment.Start
		}
	}
	return 0
}

// codeSpanRange returns the byte range an inline-code span covers, excluding
// the backticks, as [start, stop). A CodeSpan holds no segment of its own, so
// the range spans its text children.
func codeSpanRange(n ast.Node) ([2]int, bool) {
	start, stop := -1, -1
	for c := n.FirstChild(); c != nil; c = c.NextSibling() {
		t, ok := c.(*ast.Text)
		if !ok {
			continue
		}
		if start < 0 {
			start = t.Segment.Start
		}
		stop = t.Segment.Stop
	}
	if start < 0 {
		return [2]int{}, false
	}
	return [2]int{start, stop}, true
}

// offsetInRanges reports whether off falls within any [start,stop) range.
func offsetInRanges(off int, ranges [][2]int) bool {
	for _, r := range ranges {
		if off >= r[0] && off < r[1] {
			return true
		}
	}
	return false
}
