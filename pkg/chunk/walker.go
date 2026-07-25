// Copyright (c) 2026 Reactor Technologies, Inc.
// SPDX-License-Identifier: Apache-2.0

// This file holds the machinery every tree-sitter chunker shares, so adding a
// language is a walk plus an emit rather than a parser.
//
// The split is deliberate: everything here is language-independent — loading a
// grammar once, associating a comment group with the declaration below it,
// assembling a chunk's breadcrumb-led text, and keeping keys unique. What
// stays in each language's file is the part that genuinely differs: which node
// kinds are declarations, where a name lives, and what counts as a signature.
//
// This is not a configuration DSL. A per-language spec table was the other
// option and it collapses the moment a language has a quirk — Python keeps its
// doc *inside* the body, HCL names a block with labels rather than a field,
// Bash has no doc convention at all. A shared toolkit absorbs those; a schema
// grows a flag for each one.
package chunk

import (
	"slices"
	"strconv"
	"strings"
	"sync"

	sitter "github.com/tree-sitter/go-tree-sitter"
)

// grammarCache lazily wraps a tree-sitter grammar. The wrapped Language is
// immutable and safe to share across the per-file parsers the indexer creates,
// so each grammar is built exactly once per process rather than per file.
type grammarCache struct {
	once sync.Once
	load func() *sitter.Language
	lang *sitter.Language
}

func (g *grammarCache) get() *sitter.Language {
	g.once.Do(func() { g.lang = g.load() })
	return g.lang
}

// parseSource parses content with a grammar and hands the root node to walk.
// It owns the parser and tree lifetimes — both hold C memory, so both must be
// closed on every path, which is the kind of thing a caller forgets exactly
// once. Returns nil when the content is empty or the grammar cannot be set.
func parseSource(content string, g *grammarCache, walk func(root *sitter.Node, source []byte) []Chunk) []Chunk {
	if strings.TrimSpace(content) == "" {
		return nil
	}
	parser := sitter.NewParser()
	defer parser.Close()
	if err := parser.SetLanguage(g.get()); err != nil {
		return nil
	}
	source := []byte(content)
	tree := parser.Parse(source, nil)
	if tree == nil {
		return nil
	}
	defer tree.Close()
	return walk(tree.RootNode(), source)
}

// docScanner associates a comment group with the declaration it documents.
//
// The rule is adjacency, which is what every language in this index uses: a
// run of comment lines ending on the line directly above a declaration is that
// declaration's documentation. A blank line breaks the association, because a
// detached comment is a note about the file rather than about the symbol, and
// attaching it would put unrelated prose into the symbol's embedding.
type docScanner struct {
	source []byte
	kinds  []string                  // the grammar's comment node kinds
	clean  func(raw string) []string // strips markers, drops directives
	group  []*sitter.Node
}

// offer reports whether n is a comment and, if so, accumulates it. A blank line
// between two comments starts a new group.
func (d *docScanner) offer(n *sitter.Node) bool {
	if !slices.Contains(d.kinds, n.Kind()) {
		return false
	}
	if len(d.group) > 0 && !linesAdjacent(d.group[len(d.group)-1], n) {
		d.group = nil
	}
	d.group = append(d.group, n)
	return true
}

// take returns the documentation for next and clears the pending group. The
// group is consumed either way: a comment that turned out not to be adjacent
// documents nothing, and must not carry over to the declaration after it.
func (d *docScanner) take(next *sitter.Node) string {
	group := d.group
	d.group = nil
	if len(group) == 0 || !linesAdjacent(group[len(group)-1], next) {
		return ""
	}
	var lines []string
	for _, c := range group {
		lines = append(lines, d.clean(c.Utf8Text(d.source))...)
	}
	return strings.TrimSpace(strings.Join(lines, "\n"))
}

// reset drops any pending comment group, for a walker entering a new scope.
func (d *docScanner) reset() { d.group = nil }

// linesAdjacent reports whether b follows a with no blank line between them.
//
// The test allows b to start on a's end row as well as the one after it,
// because grammars disagree about whether a line comment owns its terminating
// newline. Rust's does, so a `///` line reports as ending on the line below
// itself; Go's does not. A rule of "at most one line apart" is true under both
// readings, and a strict +1 silently dropped every Rust doc comment.
func linesAdjacent(a, b *sitter.Node) bool {
	end, start := a.EndPosition().Row, b.StartPosition().Row
	return start >= end && start-end <= 1
}

// emitter accumulates a file's chunks. It owns the two invariants every
// chunker must hold: a chunk's text leads with its breadcrumb (search display
// strips it, since the header already shows it), and no two chunks in a file
// share a key — the store enforces uniqueness, so a collision would drop a
// symbol rather than report one.
type emitter struct {
	source []byte
	prefix string // key namespace: "go", "py", "proto", …
	out    []Chunk
	seen   map[string]int
}

// add appends one chunk. keyPart is the kind segment of the key ("func",
// "method"), name is the qualified symbol name, and crumb is the breadcrumb
// shown as the heading and repeated as the text's first line.
func (e *emitter) add(keyPart, name, crumb, variant, sig, doc string, n *sitter.Node) {
	e.out = append(e.out, Chunk{
		Key:     e.uniqueKey(e.prefix + "/" + keyPart + "/" + name),
		Heading: crumb,
		Variant: variant,
		Text:    joinDoc(crumb, sig, doc),
		Line:    nodeLine(n),
	})
}

// uniqueKey returns key, suffixed if this file already used it. Overloads,
// get/set pairs, and repeated config blocks legitimately share a name; the
// index needs them distinct, and a stable numeric suffix keeps re-chunking an
// unchanged file overwriting the same rows.
func (e *emitter) uniqueKey(key string) string {
	if e.seen == nil {
		e.seen = map[string]int{}
	}
	n := e.seen[key]
	e.seen[key]++
	if n == 0 {
		return key
	}
	return key + "~" + strconv.Itoa(n)
}

// namedChildrenOfKind returns every named child matching any of kinds, in
// source order.
func namedChildrenOfKind(n *sitter.Node, kinds ...string) []*sitter.Node {
	var out []*sitter.Node
	for i := uint(0); i < n.NamedChildCount(); i++ {
		if c := n.NamedChild(i); slices.Contains(kinds, c.Kind()) {
			out = append(out, c)
		}
	}
	return out
}

// firstDescendantOfKind walks the subtree breadth-first and returns the first
// node of the given kind. Used to reach a name through whatever wrappers a
// language puts around it — a pointer and type arguments on a Go receiver, a
// decorator on a Python def.
func firstDescendantOfKind(n *sitter.Node, kind string) *sitter.Node {
	queue := []*sitter.Node{n}
	for len(queue) > 0 {
		cur := queue[0]
		queue = queue[1:]
		if cur.Kind() == kind {
			return cur
		}
		for i := uint(0); i < cur.NamedChildCount(); i++ {
			queue = append(queue, cur.NamedChild(i))
		}
	}
	return nil
}

// fieldTextOf returns the text of a named field child, or "" when absent.
func fieldTextOf(n *sitter.Node, source []byte, field string) string {
	c := n.ChildByFieldName(field)
	if c == nil {
		return ""
	}
	return c.Utf8Text(source)
}

// sigUpToField renders a declaration's signature by slicing from its start to
// the start of the named field, collapsing whitespace onto one line. Used to
// drop a body: the signature is the retrieval surface, the body is
// implementation. A declaration missing the field renders whole.
func sigUpToField(n *sitter.Node, source []byte, field string) string {
	end := n.EndByte()
	if f := n.ChildByFieldName(field); f != nil {
		end = f.StartByte()
	}
	return tidySig(string(source[n.StartByte():end]))
}

// lineCommentCleaner builds a clean function for a language whose comments are
// a marker followed by prose (`//`, `#`, `--`). Directives are dropped: a
// `//go:generate` or a `# type: ignore` instructs a tool and is not
// documentation, and embedding it dilutes the symbol's prose.
func lineCommentCleaner(markers ...string) func(raw string) []string {
	return func(raw string) []string {
		for _, m := range markers {
			rest, ok := strings.CutPrefix(raw, m)
			if !ok {
				continue
			}
			if isDirective(rest) {
				return nil
			}
			return []string{strings.TrimSpace(rest)}
		}
		return blockCommentLines(raw)
	}
}

// blockCommentLines strips `/* … */` fencing and returns the prose lines.
// Returns nil for anything that is not a block comment, so a cleaner can fall
// through to it safely.
func blockCommentLines(raw string) []string {
	rest, ok := strings.CutPrefix(raw, "/*")
	if !ok {
		return nil
	}
	rest = strings.TrimSuffix(rest, "*/")
	var out []string
	for ln := range strings.SplitSeq(rest, "\n") {
		out = append(out, strings.TrimSpace(strings.TrimPrefix(strings.TrimSpace(ln), "*")))
	}
	return out
}

// isDirective reports whether a comment's text (already past the marker) is a
// tool directive rather than prose. The shape is `name:argument` with no space
// before the colon and none directly after the marker — "go:build",
// "nolint:gosec", "type: ignore" is prose by this rule but "type:ignore" is not.
func isDirective(rest string) bool {
	if rest == "" || rest[0] == ' ' || rest[0] == '\t' {
		return false
	}
	colon := strings.IndexByte(rest, ':')
	if colon <= 0 || colon == len(rest)-1 {
		return false
	}
	for i := range colon {
		if c := rest[i]; ('a' > c || c > 'z') && ('0' > c || c > '9') {
			return false
		}
	}
	return rest[colon+1] != ' '
}

// joinDoc assembles a chunk body as breadcrumb, then rendered signature, then
// doc prose, each separated by a blank line. Empty parts are dropped so a
// symbol with only one of the two still renders cleanly. The breadcrumb leads
// so search display can strip it — it is already repeated in the result header.
func joinDoc(crumb, sig, docText string) string {
	parts := []string{crumb}
	if s := strings.TrimSpace(sig); s != "" {
		parts = append(parts, s)
	}
	if d := strings.TrimSpace(docText); d != "" {
		parts = append(parts, d)
	}
	return strings.Join(parts, "\n\n")
}
