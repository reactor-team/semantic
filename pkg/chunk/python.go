// Copyright (c) 2026 Reactor Technologies, Inc.
// SPDX-License-Identifier: Apache-2.0

// This file chunks Python. Python gets a hand-written walker rather than a
// table entry (see languages.go) for one reason: its documentation lives
// *inside* the declaration, not above it. A docstring is the first statement
// of a module, class, or function body, which inverts the association every
// other language here uses and which the shared docScanner implements.
//
// Both conventions are honoured, with the docstring winning. A `#` comment
// above a def is common in real code and is worth indexing, but where a
// docstring exists it is the author's actual documentation — PEP 257 says so,
// and every Python tool reads it that way.
package chunk

import (
	"strings"

	sitter "github.com/tree-sitter/go-tree-sitter"
	pybind "github.com/tree-sitter/tree-sitter-python/bindings/go"
)

var pyLang = grammarCache{load: func() *sitter.Language {
	return sitter.NewLanguage(pybind.Language())
}}

// Python parses Python source and emits a chunk per module docstring, class,
// method, function, and documented module-level constant. The retrieval
// surface is the docstring plus the signature; bodies are not embedded.
func Python(content string) []Chunk {
	return parseSource(content, &pyLang, func(root *sitter.Node, source []byte) []Chunk {
		w := &pyWalker{
			em:  emitter{source: source, prefix: "py"},
			doc: docScanner{source: source, kinds: []string{"comment"}, clean: lineCommentCleaner("#")},
		}
		w.walkModule(root)
		return w.em.out
	})
}

type pyWalker struct {
	em  emitter
	doc docScanner
}

// walkModule emits the module docstring, then every top-level declaration.
func (w *pyWalker) walkModule(root *sitter.Node) {
	if d := docstringOf(root, w.em.source); d != "" {
		w.em.out = append(w.em.out, Chunk{
			Key:     "py/module",
			Heading: "module",
			Variant: VariantPackage,
			Text:    "module\n\n" + d,
			Line:    1,
		})
	}
	w.walk(root, "")
}

// walk emits a chunk per declaration among container's children, recursing
// into class bodies. prefix qualifies member names ("Server.") and is empty at
// module level.
func (w *pyWalker) walk(container *sitter.Node, prefix string) {
	w.doc.reset()
	var pendingValue *sitter.Node // a module-level assignment awaiting its docstring
	for i := uint(0); i < container.NamedChildCount(); i++ {
		child := container.NamedChild(i)
		if w.doc.offer(child) {
			continue
		}

		// PEP 258 attribute docstring: a bare string directly after an
		// assignment documents it. Nothing else in Python documents a
		// constant, so without this a module of named settings indexes as a
		// list of bare names.
		if pendingValue != nil && isBareString(child) {
			w.emitValue(pendingValue, prefix, stringText(child, w.em.source))
			pendingValue = nil
			w.doc.reset()
			continue
		}
		pendingValue = nil

		comment := w.doc.take(child)
		decl, decorated := unwrapDecorated(child)

		switch decl.Kind() {
		case "class_definition":
			w.emitClass(decl, decorated, prefix, comment)
		case "function_definition":
			w.emitFunc(decl, decorated, prefix, comment)
		case "expression_statement":
			if assign := firstNamedChildOfKind(decl, "assignment"); assign != nil && prefix == "" {
				pendingValue = assign
			}
		}
	}
}

// emitClass emits a class and recurses into its body for methods.
func (w *pyWalker) emitClass(decl, outer *sitter.Node, prefix, comment string) {
	name := fieldTextOf(decl, w.em.source, "name")
	if name == "" {
		return
	}
	qualified := prefix + name
	w.em.add("class", qualified, qualified, VariantClass,
		w.headerSig(decl, outer), pick(docstringOf(bodyOf(decl), w.em.source), comment), outer)

	if body := bodyOf(decl); body != nil {
		w.walk(body, qualified+".")
	}
}

// emitFunc emits a function or method. A method is qualified by its class, so
// it reads as "Server.start" — the shape every other language here uses.
func (w *pyWalker) emitFunc(decl, outer *sitter.Node, prefix, comment string) {
	name := fieldTextOf(decl, w.em.source, "name")
	if name == "" {
		return
	}
	qualified := prefix + name
	keyPart, variant := "func", VariantFunc
	if prefix != "" {
		keyPart, variant = "method", VariantMethod
	}
	w.em.add(keyPart, qualified, qualified, variant,
		w.headerSig(decl, outer), pick(docstringOf(bodyOf(decl), w.em.source), comment), outer)
}

// emitValue emits a documented module-level constant.
func (w *pyWalker) emitValue(assign *sitter.Node, prefix, doc string) {
	name := fieldTextOf(assign, w.em.source, "left")
	if name == "" || strings.HasPrefix(name, "_") {
		return
	}
	qualified := prefix + name
	w.em.add("const", qualified, qualified, VariantValue, oneLine(assign.Utf8Text(w.em.source)), doc, assign)
}

// headerSig renders a declaration's signature. When the declaration carries
// decorators the slice starts at the decorator, because `@router.post("/x")`
// is the most searchable line a route handler has — losing it would make a web
// application's endpoints unfindable by their paths.
func (w *pyWalker) headerSig(decl, outer *sitter.Node) string {
	start := outer.StartByte()
	end := decl.EndByte()
	if body := decl.ChildByFieldName("body"); body != nil {
		end = body.StartByte()
	}
	return strings.TrimRight(tidySig(string(w.em.source[start:end])), ":")
}

// unwrapDecorated returns the declaration inside a decorated_definition along
// with the outer node, so a caller can name the inner one and still render and
// line-number the whole thing. For an undecorated node both are the same.
func unwrapDecorated(n *sitter.Node) (decl, outer *sitter.Node) {
	if n.Kind() != "decorated_definition" {
		return n, n
	}
	if inner := n.ChildByFieldName("definition"); inner != nil {
		return inner, n
	}
	return n, n
}

// bodyOf returns a declaration's body block, or nil.
func bodyOf(n *sitter.Node) *sitter.Node { return n.ChildByFieldName("body") }

// docstringOf returns the docstring of a module or body block — the string
// literal that is its first statement — or "" when there is none.
func docstringOf(block *sitter.Node, source []byte) string {
	if block == nil {
		return ""
	}
	for i := uint(0); i < block.NamedChildCount(); i++ {
		c := block.NamedChild(i)
		if c.Kind() == "comment" {
			continue
		}
		if isBareString(c) {
			return stringText(c, source)
		}
		return "" // the first real statement is not a string: no docstring
	}
	return ""
}

// isBareString reports whether a statement is a lone string literal, which is
// how Python spells both a docstring and an attribute docstring.
func isBareString(n *sitter.Node) bool {
	if n.Kind() != "expression_statement" || n.NamedChildCount() != 1 {
		return false
	}
	return n.NamedChild(0).Kind() == "string"
}

// stringText returns a string literal's content with its quotes removed and
// its indentation stripped, so a triple-quoted docstring embeds as prose
// rather than as a block of leading whitespace.
func stringText(stmt *sitter.Node, source []byte) string {
	str := stmt
	if stmt.Kind() == "expression_statement" && stmt.NamedChildCount() == 1 {
		str = stmt.NamedChild(0)
	}
	content := firstNamedChildOfKind(str, "string_content")
	if content == nil {
		return ""
	}
	var lines []string
	for ln := range strings.SplitSeq(content.Utf8Text(source), "\n") {
		lines = append(lines, strings.TrimSpace(ln))
	}
	return strings.TrimSpace(strings.Join(lines, "\n"))
}

// pick returns the first non-empty of its arguments.
func pick(vals ...string) string {
	for _, v := range vals {
		if strings.TrimSpace(v) != "" {
			return v
		}
	}
	return ""
}
