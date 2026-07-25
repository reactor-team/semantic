// Copyright (c) 2026 Reactor Technologies, Inc.
// SPDX-License-Identifier: Apache-2.0

// This file chunks YAML. YAML needed a granularity decision the other
// languages did not, because it has no declarations — only nesting — and the
// obvious readings are both wrong.
//
// Chunking every key floods the index: a Kubernetes manifest has hundreds, and
// `spec.template.spec.containers.0.image` retrieves nothing a person would
// search for. Chunking whole files is the opposite failure — a multi-document
// manifest becomes one blob whose embedding averages a Deployment, a Service,
// and a ConfigMap into nothing in particular.
//
// So the unit is the **document**, plus its **top-level keys**. A document is
// what a YAML file actually declares (one object, one release, one workflow),
// and top-level keys are the sections a person names out loud — `metadata`,
// `spec`, `data`, `jobs`. Both are bounded, and both match how the files are
// discussed.
//
// A document that declares `kind` and `metadata.name` is identified by them,
// because "the api-gateway Deployment" is what someone searches for, not "the
// third document in deployment.yaml".
package chunk

import (
	"strconv"
	"strings"

	yamlbind "github.com/tree-sitter-grammars/tree-sitter-yaml/bindings/go"
	sitter "github.com/tree-sitter/go-tree-sitter"
)

// Variant constants for the YAML chunk kinds.
const (
	VariantDocument = "document"
	VariantSection  = "section"
)

var yamlLang = grammarCache{load: func() *sitter.Language {
	return sitter.NewLanguage(yamlbind.Language())
}}

// YAML parses a YAML file and emits a chunk per document and per top-level key
// within it. Comments above a document or a key are carried as its
// documentation.
func YAML(content string) []Chunk {
	return parseSource(content, &yamlLang, func(root *sitter.Node, source []byte) []Chunk {
		w := &yamlWalker{
			em:  emitter{source: source, prefix: "yaml"},
			doc: docScanner{source: source, kinds: []string{"comment"}, clean: lineCommentCleaner("#")},
		}
		w.walk(root)
		return w.em.out
	})
}

type yamlWalker struct {
	em  emitter
	doc docScanner
}

func (w *yamlWalker) walk(root *sitter.Node) {
	index := 0
	for i := uint(0); i < root.NamedChildCount(); i++ {
		child := root.NamedChild(i)
		if w.doc.offer(child) {
			continue
		}
		doc := w.doc.take(child)
		if child.Kind() != "document" {
			continue
		}
		w.emitDocument(child, index, doc)
		index++
	}
}

// emitDocument chunks one document and each of its top-level keys.
func (w *yamlWalker) emitDocument(docNode *sitter.Node, index int, doc string) {
	mapping := firstDescendantOfKind(docNode, "block_mapping")
	name, identified := w.documentName(mapping, index)

	// A positional name keeps keys unique but says nothing, so it stays out of
	// the breadcrumb — search already shows the file path beside every result,
	// and "doc0 > serviceAccount" only pushes the useful half to the right.
	crumb := name
	if !identified {
		crumb = ""
	}

	// The `---` separator belongs to the node but is punctuation, not content.
	body := strings.TrimPrefix(strings.TrimSpace(docNode.Utf8Text(w.em.source)), "---")
	w.em.add("doc", name, pick(crumb, "document"), VariantDocument, rawSig(body), doc, docNode)
	if mapping == nil {
		return
	}

	// Comments inside a document belong to the keys below them, and the
	// scanner's pending group must not leak across from the document header.
	w.doc.reset()
	for i := uint(0); i < mapping.NamedChildCount(); i++ {
		pair := mapping.NamedChild(i)
		if w.doc.offer(pair) {
			continue
		}
		keyDoc := w.doc.take(pair)
		if pair.Kind() != "block_mapping_pair" {
			continue
		}
		w.emitKey(pair, name, crumb, keyDoc)
	}
}

// emitKey chunks one top-level key. Scalar keys are skipped: `apiVersion:
// apps/v1` is already inside the document chunk, and on its own it retrieves
// nothing. Only a key with structure under it earns a chunk.
func (w *yamlWalker) emitKey(pair *sitter.Node, docName, crumbPrefix, doc string) {
	key := w.scalarText(pair.ChildByFieldName("key"))
	value := pair.ChildByFieldName("value")
	if key == "" || value == nil || firstDescendantOfKind(value, "block_mapping") == nil &&
		firstDescendantOfKind(value, "block_sequence") == nil {
		return
	}
	crumb := key
	if crumbPrefix != "" {
		crumb = crumbPrefix + " > " + key
	}
	w.em.add("key", docName+"."+key, crumb, VariantSection,
		rawSig(pair.Utf8Text(w.em.source)), doc, pair)
}

// documentName identifies a document by its `kind` and `metadata.name` when it
// declares both — the Kubernetes convention, and also close enough for Helm
// charts and GitHub Actions workflows, which use `name` at the top level.
//
// The second return reports whether the name came from the document's own
// content. A false means the name is only a position, which is enough to keep
// keys unique but not worth showing to a reader.
func (w *yamlWalker) documentName(mapping *sitter.Node, index int) (string, bool) {
	if mapping == nil {
		return "doc" + strconv.Itoa(index), false
	}
	top := w.topLevel(mapping)
	kind, name := top["kind"], top["name"]
	if name == "" {
		name = w.nestedName(mapping)
	}
	switch {
	case kind != "" && name != "":
		return kind + "/" + name, true
	case kind != "":
		return kind, true
	case name != "":
		return name, true
	}
	return "doc" + strconv.Itoa(index), false
}

// topLevel returns the document's scalar top-level keys and values.
func (w *yamlWalker) topLevel(mapping *sitter.Node) map[string]string {
	out := map[string]string{}
	for i := uint(0); i < mapping.NamedChildCount(); i++ {
		pair := mapping.NamedChild(i)
		if pair.Kind() != "block_mapping_pair" {
			continue
		}
		if k := w.scalarText(pair.ChildByFieldName("key")); k != "" {
			out[k] = w.scalarText(pair.ChildByFieldName("value"))
		}
	}
	return out
}

// nestedName returns `metadata.name`, where a Kubernetes object carries its
// identity.
func (w *yamlWalker) nestedName(mapping *sitter.Node) string {
	for i := uint(0); i < mapping.NamedChildCount(); i++ {
		pair := mapping.NamedChild(i)
		if pair.Kind() != "block_mapping_pair" || w.scalarText(pair.ChildByFieldName("key")) != "metadata" {
			continue
		}
		inner := firstDescendantOfKind(pair, "block_mapping")
		if inner == nil {
			return ""
		}
		return w.topLevel(inner)["name"]
	}
	return ""
}

// scalarText returns a node's text when it is a plain scalar, else "". A
// mapping or sequence has no scalar text, and treating one as a name would
// produce a key containing the whole subtree.
func (w *yamlWalker) scalarText(n *sitter.Node) string {
	if n == nil {
		return ""
	}
	if firstDescendantOfKind(n, "block_mapping") != nil || firstDescendantOfKind(n, "block_sequence") != nil {
		return ""
	}
	return strings.Trim(strings.TrimSpace(n.Utf8Text(w.em.source)), `"'`)
}
