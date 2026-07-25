// Copyright (c) 2026 Reactor Technologies, Inc.
// SPDX-License-Identifier: Apache-2.0

// Package toc generates a committed "## Contents" table of contents for a
// markdown file from its heading tree. Unlike a Claude Code `!command`
// substitution — which runs only at context-load time in slash-command/SKILL
// files and never as committed bytes — the output here is real markdown, so it
// is correct on GitHub, for humans, and for every agent that reads the file
// raw.
//
// The generated list is a plain-text outline — one `- Heading` line per section,
// nested by depth, with no `[text](#anchor)` links. It shows a long file's scope
// at a glance without spending a per-line anchor's worth of tokens on every
// agent that reads the file, and keeps it from registering as a swarm of
// intra-file link edges in the graph. The list lives directly under a
// `## Contents` (or `## Table of Contents`) heading — no HTML-comment markers at
// all — and Rewrite replaces everything between that heading and the next one,
// so regeneration is idempotent and touches nothing else.
package toc

import (
	"fmt"
	"regexp"
	"strings"

	"github.com/reactor-team/semantic/pkg/chunk"
)

// LineThreshold is the file length (in lines) above which a markdown file is
// expected to carry a Contents TOC — a partial read of a long file should still
// reveal its full scope.
const LineThreshold = 100

var (
	// A heading line whose text is a Contents title — the TOC is the list
	// directly beneath it.
	reContentsHeading = regexp.MustCompile(`(?im)^#{1,6}[ \t]+(?:table of contents|contents)[ \t]*$`)
	// Any ATX heading line — marks where the Contents section's list ends.
	reAnyHeading = regexp.MustCompile(`(?m)^#{1,6}[ \t]+\S`)
	// A frontmatter closing fence: a line that is exactly "---".
	reClosingFence = regexp.MustCompile(`(?m)^---[ \t]*$`)
)

// Audit describes a file's Contents-TOC health for the lint rule.
type Audit struct {
	Lines    int  // total line count
	Entries  int  // headings a TOC would list (0 = nothing to tabulate)
	HasBlock bool // a Contents / Table of Contents heading is present
	UpToDate bool // the list under that heading matches a freshly generated TOC
}

// Inspect reports a file's TOC health without modifying it.
func Inspect(content string) Audit {
	list := Generate(content)
	a := Audit{
		Lines:   len(strings.Split(content, "\n")),
		Entries: entryCount(list),
	}
	if _, body, ok := contentsSection(content); ok {
		a.HasBlock = true
		a.UpToDate = strings.TrimSpace(body) == list
	}
	return a
}

// Generate returns the markdown list a Contents TOC would hold for content:
// one plain-text `- Heading` line per heading below the top level, nested by
// depth. Only ATX headings ("## Foo") count; setext headings (text underlined
// by === / ---) are ignored, since a `---` divider under a prose line reads as a
// heading to CommonMark but rarely is one. The document title (a level-1
// heading) and any "Contents" heading are omitted. Returns "" when there is
// nothing to list.
func Generate(content string) string {
	hs := headings(stripFrontmatter(content))

	var entries []heading
	minLevel := 0
	for _, h := range hs {
		if h.level < 2 || chunk.IsContentsHeading(h.text) {
			continue
		}
		entries = append(entries, h)
		if minLevel == 0 || h.level < minLevel {
			minLevel = h.level
		}
	}

	var b strings.Builder
	for _, e := range entries {
		indent := strings.Repeat("  ", e.level-minLevel)
		fmt.Fprintf(&b, "%s- %s\n", indent, e.text)
	}
	return strings.TrimRight(b.String(), "\n")
}

// Rewrite returns content with a current Contents TOC. When a Contents heading
// exists, the list between it and the next heading is replaced; otherwise a new
// "## Contents" section is created immediately before the first section heading
// (after the title and any intro), or at the top of a heading-only file. changed
// reports whether the content differs. It errors when there are no headings to
// tabulate.
func Rewrite(content string) (out string, changed bool, err error) {
	list := Generate(content)
	if list == "" {
		return content, false, fmt.Errorf("no headings to tabulate")
	}

	if headingEnd, _, ok := contentsSection(content); ok {
		rest := content[headingEnd:]
		if next := reAnyHeading.FindStringIndex(rest); next != nil {
			out = content[:headingEnd] + "\n\n" + list + "\n\n" + rest[next[0]:]
		} else {
			out = content[:headingEnd] + "\n\n" + list + "\n"
		}
	} else {
		out = insertContents(content, list)
	}
	return out, out != content, nil
}

// contentsSection locates a Contents / Table of Contents heading and returns the
// byte offset just past its heading line, the text between that heading and the
// next heading (its current list body), and whether such a heading exists.
func contentsSection(content string) (headingEnd int, body string, ok bool) {
	loc := reContentsHeading.FindStringIndex(content)
	if loc == nil {
		return 0, "", false
	}
	headingEnd = loc[1]
	rest := content[headingEnd:]
	if next := reAnyHeading.FindStringIndex(rest); next != nil {
		return headingEnd, rest[:next[0]], true
	}
	return headingEnd, rest, true
}

// insertContents creates a new "## Contents" section immediately before the
// first section heading — which sits after the title and any intro/lede
// paragraph under it — or at the top of a file that has no lede.
func insertContents(content, list string) string {
	if off, ok := firstSectionOffset(content); ok {
		before := strings.TrimRight(content[:off], "\n")
		sep := "\n\n"
		if before == "" {
			sep = ""
		}
		return before + sep + "## Contents\n\n" + list + "\n\n" + content[off:]
	}
	return "## Contents\n\n" + list + "\n\n" + content
}

// firstSectionOffset returns the byte offset in content at which the first
// TOC-listed section heading (level ≥ 2, excluding a Contents title) begins.
// Headings are located with goldmark on the frontmatter-stripped content — so a
// `##` inside a code fence or a `#` in frontmatter can't be mistaken for one —
// then mapped back to the original by the stripped-prefix length.
func firstSectionOffset(content string) (int, bool) {
	stripped := stripFrontmatter(content)
	prefix := len(content) - len(stripped)
	for _, s := range chunk.HeadingSpans([]byte(stripped)) {
		// Setext headings aren't tabulated, so don't anchor on them.
		if !s.ATX || s.Level < 2 {
			continue
		}
		if chunk.IsContentsHeading(strings.TrimSpace(s.Text)) {
			continue
		}
		return prefix + s.LineStart, true
	}
	return 0, false
}

// entryCount counts the list items a generated TOC holds.
func entryCount(list string) int {
	if list == "" {
		return 0
	}
	return strings.Count(list, "\n") + 1
}

type heading struct {
	level int
	text  string
}

// headings returns the file's ATX headings in document order, each with its
// level and rendered text (the heading line's source text, markers stripped by
// goldmark). Setext headings — a text line underlined by === or --- — are
// skipped: authors write `---` as a divider under a prose line far more often
// than they mean a heading, and CommonMark's setext rule would otherwise pull
// that prose into the TOC.
func headings(md string) []heading {
	var out []heading
	for _, s := range chunk.HeadingSpans([]byte(md)) {
		if !s.ATX {
			continue
		}
		out = append(out, heading{level: s.Level, text: strings.TrimSpace(s.Text)})
	}
	return out
}

// stripFrontmatter drops a leading YAML frontmatter block so its lines (a `#`
// YAML comment especially) aren't parsed as headings. Content without a
// frontmatter fence is returned unchanged.
func stripFrontmatter(content string) string {
	if !strings.HasPrefix(content, "---\n") && !strings.HasPrefix(content, "---\r\n") {
		return content
	}
	rest := content[strings.IndexByte(content, '\n')+1:]
	if m := reClosingFence.FindStringIndex(rest); m != nil {
		tail := rest[m[1]:]
		return strings.TrimPrefix(tail, "\n")
	}
	return content
}
