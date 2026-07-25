// Copyright (c) 2026 Reactor Technologies, Inc.
// SPDX-License-Identifier: Apache-2.0

// This file chunks HCL — Terraform, Terragrunt, Packer, and Nomad
// configuration. Infrastructure is where "why is it configured this way"
// questions are hardest to answer by grep, because the answer is usually in a
// comment above a block whose name you would have to already know.
//
// HCL gets its own walker rather than a table entry (see languages.go) because
// a block is named by its labels rather than by a field: `resource
// "aws_s3_bucket" "logs"` is one node whose identity is spread across three
// children. Joining them is what makes a chunk key match how an engineer
// refers to the resource.
package chunk

import (
	"strings"

	hclbind "github.com/tree-sitter-grammars/tree-sitter-hcl/bindings/go"
	sitter "github.com/tree-sitter/go-tree-sitter"
)

// VariantBlock is the chunk variant for one HCL block.
const VariantBlock = "block"

var hclLang = grammarCache{load: func() *sitter.Language {
	return sitter.NewLanguage(hclbind.Language())
}}

// HCL parses an HCL file and emits a chunk per block, keyed by the block type
// and its labels ("resource.aws_s3_bucket.logs"), carrying the comment above
// it and the block's own attributes.
func HCL(content string) []Chunk {
	return parseSource(content, &hclLang, func(root *sitter.Node, source []byte) []Chunk {
		w := &hclWalker{
			em:  emitter{source: source, prefix: "hcl"},
			doc: docScanner{source: source, kinds: []string{"comment"}, clean: lineCommentCleaner("#", "//")},
		}
		w.walk(root, "")
		return w.em.out
	})
}

type hclWalker struct {
	em  emitter
	doc docScanner
}

// walk descends through body wrappers and emits a chunk per block. Nested
// blocks are chunked too and qualified by their parent, so a `lifecycle_rule`
// inside a bucket is findable without colliding with every other one.
func (w *hclWalker) walk(container *sitter.Node, prefix string) {
	w.doc.reset()
	w.walkInto(container, prefix)
}

// walkInto is walk without the scope reset. A `body` is a wrapper rather than
// a declaration, and a file's comments sit beside it rather than inside it, so
// consuming the pending comment on the way in would drop the documentation of
// the first block in every file.
func (w *hclWalker) walkInto(container *sitter.Node, prefix string) {
	for i := uint(0); i < container.NamedChildCount(); i++ {
		child := container.NamedChild(i)
		if w.doc.offer(child) {
			continue
		}
		switch child.Kind() {
		case "body", "config_file":
			w.walkInto(child, prefix)
		case "block":
			w.emitBlock(child, prefix, w.doc.take(child))
		default:
			// An attribute or anything else ends the comment's reach.
			w.doc.take(child)
		}
	}
}

// emitBlock chunks one block. The body is rendered whole: an HCL block's
// attributes *are* its content, unlike a function body, and a block with its
// attributes stripped would say nothing a reader could match against.
func (w *hclWalker) emitBlock(block *sitter.Node, prefix, doc string) {
	name := w.blockName(block)
	if name == "" {
		return
	}
	qualified := prefix + name
	w.em.add("block", qualified, qualified, VariantBlock, rawSig(block.Utf8Text(w.em.source)), doc, block)

	if body := firstNamedChildOfKind(block, "body"); body != nil && w.hasNestedBlock(body) {
		w.walk(body, qualified+".")
	}
}

// blockName joins a block's type and its labels with dots, so `resource
// "aws_s3_bucket" "logs"` becomes "resource.aws_s3_bucket.logs" — the name an
// engineer uses for it in a plan, a state file, and in conversation.
func (w *hclWalker) blockName(block *sitter.Node) string {
	var parts []string
	for i := uint(0); i < block.NamedChildCount(); i++ {
		c := block.NamedChild(i)
		switch c.Kind() {
		case "identifier":
			parts = append(parts, c.Utf8Text(w.em.source))
		case "string_lit":
			parts = append(parts, unquoteHCL(c.Utf8Text(w.em.source)))
		case "body":
			return strings.Join(parts, ".")
		}
	}
	return strings.Join(parts, ".")
}

// hasNestedBlock reports whether a body declares blocks of its own, so a leaf
// block of plain attributes is not walked into for nothing.
func (w *hclWalker) hasNestedBlock(body *sitter.Node) bool {
	return firstNamedChildOfKind(body, "block") != nil
}

// unquoteHCL strips the quotes from a string label.
func unquoteHCL(s string) string {
	return strings.Trim(strings.TrimSpace(s), `"`)
}
