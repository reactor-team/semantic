// Package chunk splits a markdown file into retrievable units for
// embedding, via a heading-tree walk that emits /path·/narrow·/full
// variants with slugified keys. Notable design choices:
//
//   - API returns structured []Chunk instead of a map[string]string, so
//     the index layer stores key/heading/variant/text without re-parsing
//     a composite key.
//   - Chunk text is run through Simplify (simplify.go) before it's stored:
//     HTML comments and generated TOC blocks are dropped and links (`[](…)`
//     and Obsidian `[[…]]`) are reduced to their visible text, so embeddings
//     and snippets aren't padded with URLs, anchors, and markers. Ordinary
//     markdown is kept — the model reads it fine.
//   - A synthesized `title` chunk is emitted from frontmatter title/tags,
//     so a note that carries its name only in frontmatter (no body H1)
//     stays findable. Frontmatter is otherwise not embedded.
package chunk

import (
	"fmt"
	"path"
	"regexp"
	"strings"
	"unicode"

	"github.com/yuin/goldmark"
	"github.com/yuin/goldmark/ast"
	"github.com/yuin/goldmark/text"
	"go.yaml.in/yaml/v3"
)

// Chunk is one retrievable unit of a markdown file. Key is stable across
// runs for a given body, so re-chunking an unchanged file overwrites the
// same index rows rather than orphaning old ones.
type Chunk struct {
	Key     string // stable chunk key, e.g. "body/3/overview/narrow", "title"
	Heading string // breadcrumb "# H1 > ## H2 > ### self"; empty for body/title
	Variant string // "path" | "narrow" | "full" | "body" | "title"
	Text    string // text to embed and to display as a snippet
	Line    int    // 1-based source line the chunk starts on (heading line, or 1 for title)
}

// Variant constants for the chunk kinds this package emits.
const (
	VariantTitle  = "title"
	VariantPath   = "path"
	VariantNarrow = "narrow"
	VariantFull   = "full"
	VariantBody   = "body"
)

// Document splits frontmatter off content, synthesizes a title chunk from
// the frontmatter (when present), and chunks the body by its heading tree.
// Returns nil when there is nothing embeddable.
func Document(content string) []Chunk {
	fm, body := splitFrontmatter(content)

	var out []Chunk
	if tc, ok := titleChunk(fm); ok {
		out = append(out, tc)
	}
	// chunkBody numbers lines relative to body; body is a suffix of content,
	// so shift each body chunk past the lines frontmatter consumed.
	baseLine := strings.Count(content[:len(content)-len(body)], "\n")
	for _, c := range chunkBody(body) {
		c.Line += baseLine
		out = append(out, c)
	}
	return out
}

// frontmatterPattern matches a leading `---` fenced YAML block. The body
// is everything after the closing fence. Tolerant of CRLF and a trailing
// newline on the opening fence.
var frontmatterPattern = regexp.MustCompile(`(?s)\A---\r?\n(.*?)\r?\n---[ \t]*\r?\n?(.*)\z`)

// splitFrontmatter separates a leading YAML frontmatter block from the
// markdown body. When there is no frontmatter (or it doesn't parse), the
// frontmatter map is empty and body is the whole content — chunking a
// malformed file is better than dropping it.
func splitFrontmatter(content string) (frontmatter map[string]any, body string) {
	trimmed := strings.TrimLeft(content, "\ufeff \t\r\n")
	m := frontmatterPattern.FindStringSubmatch(trimmed)
	if m == nil {
		return map[string]any{}, content
	}
	fm := map[string]any{}
	if err := yaml.Unmarshal([]byte(m[1]), &fm); err != nil || fm == nil {
		// Malformed frontmatter: treat the original content as pure body.
		return map[string]any{}, content
	}
	return fm, m[2]
}

// titleChunk synthesizes a retrievable chunk from the frontmatter title
// (and tags, if present). Returns ok=false when there's no title to embed.
func titleChunk(fm map[string]any) (Chunk, bool) {
	title := strings.TrimSpace(scalarString(fm["title"]))
	if title == "" {
		title = strings.TrimSpace(scalarString(fm["name"]))
	}
	if title == "" {
		return Chunk{}, false
	}
	body := title
	if tags := stringList(fm["tags"]); len(tags) > 0 {
		body += "\n\ntags: " + strings.Join(tags, ", ")
	}
	return Chunk{Key: VariantTitle, Variant: VariantTitle, Text: body, Line: 1}, true
}

// scalarString renders a scalar frontmatter value as a string. Non-scalars
// (maps, sequences) render empty — callers use stringList for sequences.
func scalarString(v any) string {
	switch t := v.(type) {
	case string:
		return t
	case int, int64, float64, bool:
		return fmt.Sprintf("%v", t)
	default:
		return ""
	}
}

// stringList coerces a frontmatter value into a list of strings, handling
// both the inline (`tags: [a, b]`) and block (`tags:\n  - a`) YAML shapes,
// plus a single bare string.
func stringList(v any) []string {
	switch t := v.(type) {
	case []any:
		out := make([]string, 0, len(t))
		for _, e := range t {
			if s := strings.TrimSpace(scalarString(e)); s != "" {
				out = append(out, s)
			}
		}
		return out
	case string:
		if s := strings.TrimSpace(t); s != "" {
			return []string{s}
		}
	}
	return nil
}

// chunkBody walks a body's heading tree and emits up to three chunks per
// heading, each carrying the full breadcrumb path:
//
//   - path:   breadcrumb only, no content — a structural "where is X
//     discussed?" match.
//   - narrow: breadcrumb + this heading's direct content (up to the next
//     heading of any level) — matches the specific paragraph.
//   - full:   breadcrumb + the whole subtree (up to the next same-or-
//     shallower heading) — rolls the section up at each level so H1/H2
//     chunks carry section summaries. Skipped when it would duplicate
//     narrow (leaves).
//
// A body with no headings produces a single `body` chunk with the full
// text. An empty body produces nothing.
func chunkBody(body string) []Chunk {
	trimmed := strings.TrimSpace(body)
	if trimmed == "" {
		return nil
	}
	// lead is the byte length of the whitespace TrimSpace dropped from the
	// front; offsets into source map back to body as lead+offset, which is
	// how each chunk's source line is recovered.
	lead := len(body) - len(strings.TrimLeftFunc(body, unicode.IsSpace))
	source := []byte(trimmed)
	spans := HeadingSpans(source)

	if len(spans) == 0 {
		return []Chunk{{Key: VariantBody, Variant: VariantBody, Text: strings.TrimSpace(Simplify(trimmed)), Line: lineAt(body, lead)}}
	}

	type crumb struct {
		level int
		text  string
	}
	var out []Chunk
	var stack []crumb
	for i, h := range spans {
		htext := strings.TrimSpace(Simplify(h.Text))
		for len(stack) > 0 && stack[len(stack)-1].level >= h.Level {
			stack = stack[:len(stack)-1]
		}
		stack = append(stack, crumb{level: h.Level, text: htext})

		// A "Contents" / "Table of Contents" section is a generated navigation
		// list, not content — skip it so the TOC isn't embedded. It stays in
		// `spans` above so it still bounds the preceding section's range.
		if IsContentsHeading(htext) {
			continue
		}

		// Narrow end: the next heading of ANY level — this heading's
		// direct content only.
		narrowEnd := len(source)
		if i+1 < len(spans) {
			narrowEnd = spans[i+1].LineStart
		}
		// Full end: the next heading at level ≤ this one — the whole
		// subtree rooted here.
		fullEnd := len(source)
		for j := i + 1; j < len(spans); j++ {
			if spans[j].Level <= h.Level {
				fullEnd = spans[j].LineStart
				break
			}
		}
		narrowText := strings.TrimSpace(Simplify(string(source[h.ContentStart:narrowEnd])))
		fullText := strings.TrimSpace(Simplify(string(source[h.ContentStart:fullEnd])))

		breadcrumb := make([]string, 0, len(stack))
		for _, c := range stack {
			breadcrumb = append(breadcrumb, strings.Repeat("#", c.level)+" "+c.text)
		}
		sec := section{
			index: i,
			slug:  slugifyHeading(htext),
			path:  strings.Join(breadcrumb, " > "),
			line:  lineAt(body, lead+h.LineStart),
		}
		out = append(out, emitVariants(sec, narrowText, fullText)...)
	}
	return out
}

// section identifies one heading's chunks: its document-order index, key slug,
// breadcrumb path (used as both Key component and Heading), and source line.
type section struct {
	index int
	slug  string
	path  string
	line  int
}

// emitVariants builds the path/narrow/full chunks for one heading. narrow is
// omitted when the heading has no direct content; full is omitted when it would
// duplicate narrow — a leaf heading is its own subtree.
func emitVariants(sec section, narrowText, fullText string) []Chunk {
	out := []Chunk{{
		Key:     fmt.Sprintf("body/%d/%s/path", sec.index, sec.slug),
		Heading: sec.path,
		Variant: VariantPath,
		Text:    sec.path,
		Line:    sec.line,
	}}
	if narrowText != "" {
		out = append(out, Chunk{
			Key:     fmt.Sprintf("body/%d/%s/narrow", sec.index, sec.slug),
			Heading: sec.path,
			Variant: VariantNarrow,
			Text:    sec.path + "\n\n" + narrowText,
			Line:    sec.line,
		})
	}
	if fullText != "" && fullText != narrowText {
		out = append(out, Chunk{
			Key:     fmt.Sprintf("body/%d/%s/full", sec.index, sec.slug),
			Heading: sec.path,
			Variant: VariantFull,
			Text:    sec.path + "\n\n" + fullText,
			Line:    sec.line,
		})
	}
	return out
}

// HeadingSpan describes one heading located by HeadingSpans.
type HeadingSpan struct {
	Level        int    // heading level (1 for "#", 2 for "##", …)
	Text         string // raw heading-line text, markers stripped by goldmark, not trimmed
	LineStart    int    // byte offset of the heading line's first char
	ContentStart int    // byte offset just past the heading line, where its content begins
	ATX          bool   // true for "## Foo"; false for a setext heading underlined by ===/---
}

// HeadingSpans parses src as markdown and returns its headings in document
// order. The chunker (which walks every heading) and the TOC generator (which
// keeps only ATX headings) share this single parse rather than each
// re-deriving the AST walk.
func HeadingSpans(src []byte) []HeadingSpan {
	root := goldmark.New().Parser().Parse(text.NewReader(src))
	var out []HeadingSpan
	for n := root.FirstChild(); n != nil; n = n.NextSibling() {
		h, ok := n.(*ast.Heading)
		if !ok {
			continue
		}
		lines := h.Lines()
		if lines.Len() == 0 {
			continue
		}
		first := lines.At(0)
		last := lines.At(lines.Len() - 1)
		content := last.Stop
		if content < len(src) && src[content] == '\n' {
			content++
		}
		out = append(out, HeadingSpan{
			Level:        h.Level,
			Text:         string(src[first.Start:first.Stop]),
			LineStart:    lineStart(src, first.Start),
			ContentStart: content,
			ATX:          isATXLine(src, first.Start),
		})
	}
	return out
}

// lineStart returns the byte offset of the start of the line containing off.
func lineStart(src []byte, off int) int {
	for off > 0 && src[off-1] != '\n' {
		off--
	}
	return off
}

// isATXLine reports whether the heading whose rendered text begins at byte
// offset off is an ATX heading ("## Foo") rather than a setext heading (a text
// line underlined by === / ---). goldmark's heading segment starts after the
// "## " marker for ATX but at the text itself for setext, so the line's first
// non-space byte is '#' only in the ATX case.
func isATXLine(src []byte, off int) bool {
	start := lineStart(src, off)
	for start < off && (src[start] == ' ' || src[start] == '\t') {
		start++
	}
	return start < len(src) && src[start] == '#'
}

// lineAt returns the 1-based line number of byte offset off in s (the number
// of newlines preceding it, plus one).
func lineAt(s string, off int) int {
	return 1 + strings.Count(s[:off], "\n")
}

// headingSlugPattern matches any run of characters that isn't a lowercase
// ascii letter / digit — used to collapse freeform heading text into a
// stable chunk-key slug.
var headingSlugPattern = regexp.MustCompile(`[^a-z0-9]+`)

// anchorSlugPattern matches the characters GitHub's slugger strips from a
// heading when forming its #anchor: everything except ascii letters, digits,
// underscore, hyphen, and space. Unlike headingSlugPattern this does not
// match spaces (they become hyphens, not stripped) and does not collapse
// runs.
var anchorSlugPattern = regexp.MustCompile(`[^a-z0-9_ -]+`)

// HeadingLeaf returns the leaf heading of a chunker breadcrumb
// ("# H1 > ## H2 > ### Self" → "Self"): the last segment with its leading
// `#` markers stripped. Empty for an empty breadcrumb (body/title chunks
// carry none). Pair with AnchorSlug to recover a section's anchor slug.
func HeadingLeaf(breadcrumb string) string {
	if breadcrumb == "" {
		return ""
	}
	seg := breadcrumb
	if i := strings.LastIndex(breadcrumb, " > "); i >= 0 {
		seg = breadcrumb[i+len(" > "):]
	}
	return strings.TrimSpace(strings.TrimLeft(seg, "#"))
}

// IsContentsHeading reports whether heading text names a table of contents,
// whose list is navigation we don't embed.
func IsContentsHeading(s string) bool {
	switch strings.ToLower(strings.TrimSpace(s)) {
	case "contents", "table of contents":
		return true
	}
	return false
}

// IsMarkdown reports whether a path is a markdown file by extension
// (.md/.markdown, case-insensitive).
func IsMarkdown(p string) bool {
	switch strings.ToLower(path.Ext(p)) {
	case ".md", ".markdown":
		return true
	}
	return false
}

// slugifyHeading turns heading text into a chunk-key suffix: lowercase,
// hyphen-separated, trimmed, capped. Deterministic so a re-chunk on the
// same body overwrites the same rows rather than leaving orphans. This is a
// storage key, not a GitHub anchor — use AnchorSlug to match link fragments.
func slugifyHeading(s string) string {
	lower := strings.ToLower(s)
	slug := headingSlugPattern.ReplaceAllString(lower, "-")
	slug = strings.Trim(slug, "-")
	if len(slug) > 60 {
		slug = slug[:60]
	}
	if slug == "" {
		slug = "section"
	}
	return slug
}

// AnchorSlug converts heading text into the #fragment GitHub would generate
// for it, so a link's #section anchor can be validated against real headings.
// Faithful to github-slugger: lowercase, delete punctuation (keeping '_' and
// '-'), then turn spaces into hyphens — consecutive hyphens are preserved and
// there is no length cap. This is deliberately not slugifyHeading, which
// collapses punctuation runs to a single '-' and caps length for chunk keys;
// reusing that here mis-slugs punctuation-heavy anchors (`## GET /a/{id}`).
func AnchorSlug(s string) string {
	lower := strings.ToLower(s)
	stripped := anchorSlugPattern.ReplaceAllString(lower, "")
	return strings.ReplaceAll(stripped, " ", "-")
}
