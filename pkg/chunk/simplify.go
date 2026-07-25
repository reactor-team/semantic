// Copyright (c) 2026 Reactor Technologies, Inc.
// SPDX-License-Identifier: Apache-2.0

package chunk

import "regexp"

// The embedded/displayed text of a chunk is the raw markdown source slice.
// Some of that markup is pure token overhead with no retrieval value — HTML
// comments (including our own `<!-- toc -->` blocks and `<!-- semantic-ignore -->`
// directives) and the URL/anchor halves of links. Ordinary markdown (headings,
// lists, emphasis, code spans) is left intact: the embedding model handles it
// fine and it carries structure worth keeping.
var (
	// A generated TOC block, markers and list together — navigation, not content.
	reSimplifyTocBlock = regexp.MustCompile(`(?is)<!--\s*toc\s*-->.*?<!--\s*tocstop\s*-->`)
	// Any remaining HTML comment (directives, editorial notes).
	reSimplifyComment = regexp.MustCompile(`(?s)<!--.*?-->`)
	// Inline image `![alt](url)` → alt. Run before links (shares the `](` shape).
	reSimplifyImage = regexp.MustCompile(`!\[([^\]]*)\]\([^)]*\)`)
	// Inline link `[text](url)` (optionally with a "title") → text.
	reSimplifyLink = regexp.MustCompile(`\[([^\]]*)\]\([^)]*\)`)
	// Wikilink `[[target]]` or `[[target|label]]` → label if present, else target.
	reSimplifyWikilink = regexp.MustCompile(`\[\[([^\]|]+)(?:\|([^\]]+))?\]\]`)
	// A run of 3+ newlines left by removals → a single blank line.
	reSimplifyBlankRun = regexp.MustCompile(`\n{3,}`)
)

// Simplify reduces markdown to the text worth embedding and displaying: it
// drops generated TOC blocks and HTML comments, and renders links as just their
// visible text (`[docs](x)` → `docs`, `![alt](x)` → `alt`,
// `[[target|label]]` → `label`). Prose, code spans, lists, and emphasis are left
// as-is. Applied to chunk text at index time so embeddings and snippets aren't
// padded with URLs, anchors, and markers that carry no meaning.
func Simplify(s string) string {
	s = reSimplifyTocBlock.ReplaceAllString(s, "")
	s = reSimplifyComment.ReplaceAllString(s, "")
	s = reSimplifyImage.ReplaceAllString(s, "$1")
	s = reSimplifyLink.ReplaceAllString(s, "$1")
	s = reSimplifyWikilink.ReplaceAllStringFunc(s, func(m string) string {
		g := reSimplifyWikilink.FindStringSubmatch(m)
		if g[2] != "" {
			return g[2]
		}
		return g[1]
	})
	s = reSimplifyBlankRun.ReplaceAllString(s, "\n\n")
	return s
}
