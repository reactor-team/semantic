// Copyright (c) 2026 Reactor Technologies, Inc.
// SPDX-License-Identifier: Apache-2.0

// This file chunks shell scripts. Build tasks, deploy scripts, and CI helpers
// accumulate operational knowledge that exists nowhere else — the comment
// above a function is often the only documentation an operational procedure
// has.
//
// Bash gets its own walker rather than a table entry (see languages.go)
// because of what it *excludes*. A script is mostly top-level commands, not
// declarations, and indexing those would bury the useful chunks. Only
// functions and documented variables are emitted.
package chunk

import (
	"strings"

	sitter "github.com/tree-sitter/go-tree-sitter"
	bashbind "github.com/tree-sitter/tree-sitter-bash/bindings/go"
)

var bashLang = grammarCache{load: func() *sitter.Language {
	return sitter.NewLanguage(bashbind.Language())
}}

// Bash parses a shell script and emits a chunk per function and per documented
// variable assignment, plus the script's header comment when it has one.
func Bash(content string) []Chunk {
	return parseSource(content, &bashLang, func(root *sitter.Node, source []byte) []Chunk {
		w := &bashWalker{
			em:  emitter{source: source, prefix: "sh"},
			doc: docScanner{source: source, kinds: []string{"comment"}, clean: bashCommentCleaner},
		}
		w.walk(root)
		return w.em.out
	})
}

type bashWalker struct {
	em  emitter
	doc docScanner
}

func (w *bashWalker) walk(root *sitter.Node) {
	w.emitHeader(root)
	for i := uint(0); i < root.NamedChildCount(); i++ {
		child := root.NamedChild(i)
		if w.doc.offer(child) {
			continue
		}
		doc := w.doc.take(child)

		switch child.Kind() {
		case "function_definition":
			w.emitFunc(child, doc)
		case "variable_assignment":
			w.emitVar(child, doc)
		}
	}
}

// emitHeader chunks the comment block at the top of a script, which describes
// the script rather than whatever follows it — that is what makes "the script
// that rotates the certs" findable.
//
// It reads the leading run of comments directly, rather than taking whatever
// the walk's first declaration happens to be documented by. Those are usually
// different comments: a script's header is separated from the first function
// by a blank line, so the header documents nothing and the first function's
// own comment would otherwise be mislabelled as the header.
func (w *bashWalker) emitHeader(root *sitter.Node) {
	var lines []string
	var last *sitter.Node
	for i := uint(0); i < root.NamedChildCount(); i++ {
		c := root.NamedChild(i)
		if c.Kind() != "comment" {
			break
		}
		// Stop at a gap. Past a blank line the comments belong to the first
		// declaration, not to the script.
		if last != nil && !linesAdjacent(last, c) {
			break
		}
		last = c
		lines = append(lines, bashCommentCleaner(c.Utf8Text(w.em.source))...)
	}
	text := strings.TrimSpace(strings.Join(lines, "\n"))
	if text == "" {
		return
	}
	w.em.out = append(w.em.out, Chunk{
		Key:     "sh/header",
		Heading: "script",
		Variant: VariantPackage,
		Text:    "script\n\n" + text,
		Line:    1,
	})
}

func (w *bashWalker) emitFunc(decl *sitter.Node, doc string) {
	name := fieldTextOf(decl, w.em.source, "name")
	if name == "" {
		return
	}
	w.em.add("func", name, name, VariantFunc, name+"()", doc, decl)
}

// emitVar chunks a variable only when it is documented. An undocumented
// assignment is a local, a loop counter, or a temporary path — indexing every
// one would swamp the useful chunks in a script that is mostly assignments.
func (w *bashWalker) emitVar(assign *sitter.Node, doc string) {
	if doc == "" {
		return
	}
	name := fieldTextOf(assign, w.em.source, "name")
	if name == "" {
		return
	}
	w.em.add("var", name, name, VariantValue, oneLine(assign.Utf8Text(w.em.source)), doc, assign)
}

// bashCommentCleaner strips `#` and drops the shebang, which names an
// interpreter rather than documenting anything.
func bashCommentCleaner(raw string) []string {
	if strings.HasPrefix(raw, "#!") {
		return nil
	}
	return lineCommentCleaner("#")(raw)
}
