// This file chunks Protocol Buffers. A `.proto` file is the closest thing a
// service-oriented codebase has to an API reference, and the comments above a
// message, an rpc, or an enum are that reference's prose — so they are worth
// indexing even though a proto declares no behaviour.
//
// Protobuf gets its own walker rather than a table entry (see languages.go)
// because it names things by a dedicated child node — `message_name`,
// `service_name`, `rpc_name` — instead of by a `name` field, and because an
// rpc is worth qualifying by the service that declares it.
package chunk

import (
	"slices"

	protobind "github.com/coder3101/tree-sitter-proto/bindings/go"
	sitter "github.com/tree-sitter/go-tree-sitter"
)

// Variant constants for the protobuf declaration kinds.
const (
	VariantMessage = "message"
	VariantService = "service"
	VariantRPC     = "rpc"
)

var protoLang = grammarCache{load: func() *sitter.Language {
	return sitter.NewLanguage(protobind.Language())
}}

// Protobuf parses a .proto file and emits a chunk per message, service, rpc,
// and enum, each carrying the comment above it. The package declared by the
// file prefixes every breadcrumb, so a search result says which API surface it
// came from.
func Protobuf(content string) []Chunk {
	return parseSource(content, &protoLang, func(root *sitter.Node, source []byte) []Chunk {
		w := &protoWalker{
			em:  emitter{source: source, prefix: "proto"},
			doc: docScanner{source: source, kinds: []string{"comment"}, clean: lineCommentCleaner("//")},
		}
		w.walk(root, "")
		return w.em.out
	})
}

type protoWalker struct {
	em  emitter
	doc docScanner
	pkg string
}

func (w *protoWalker) walk(container *sitter.Node, prefix string) {
	w.doc.reset()
	for i := uint(0); i < container.NamedChildCount(); i++ {
		child := container.NamedChild(i)
		if w.doc.offer(child) {
			continue
		}
		doc := w.doc.take(child)
		switch child.Kind() {
		case "package":
			// Take the dotted name only. The node text is the whole statement
			// ("package reactor.v1;"), and the keyword and semicolon would end
			// up inside every breadcrumb this file produces.
			if id := firstNamedChildOfKind(child, "full_ident"); id != nil {
				w.pkg = oneLine(id.Utf8Text(w.em.source))
			}
		case "message":
			w.emit(child, "message_name", "message", VariantMessage, prefix, doc, "message_body")
		case "enum":
			w.emit(child, "enum_name", "enum", VariantEnum, prefix, doc, "")
		case "service":
			w.emit(child, "service_name", "service", VariantService, prefix, doc, "")
		case "rpc":
			w.emit(child, "rpc_name", "rpc", VariantRPC, prefix, doc, "")
		}
	}
}

// emit chunks one declaration and, when it nests others, recurses. bodyKind
// names the child holding nested declarations; a message may declare messages
// and enums inside itself, and those deserve their own chunks.
func (w *protoWalker) emit(n *sitter.Node, nameKind, keyPart, variant, prefix, doc, bodyKind string) {
	nameNode := firstNamedChildOfKind(n, nameKind)
	if nameNode == nil {
		return
	}
	name := prefix + nameNode.Utf8Text(w.em.source)

	// A message renders whole, because its fields are the schema and the schema
	// is what a reader matches against. A service renders as its header only:
	// each rpc becomes its own chunk, so including the body would embed every
	// rpc twice and drag its neighbours' comments into the service's text.
	sig := rawSig(n.Utf8Text(w.em.source))
	if n.Kind() == "service" {
		sig = w.declHeader(n, "rpc", "comment")
	}
	w.em.add(keyPart, name, w.crumb(name), variant, sig, doc, n)

	if n.Kind() == "service" {
		w.walk(n, name+".")
		return
	}
	if bodyKind != "" {
		if body := firstNamedChildOfKind(n, bodyKind); body != nil {
			w.walk(body, name+".")
		}
	}
}

// declHeader renders a declaration up to the first child of any of the given
// kinds — the protobuf grammar gives a service no body field, so its header is
// found by locating where its contents start.
func (w *protoWalker) declHeader(n *sitter.Node, until ...string) string {
	end := n.EndByte()
	for i := uint(0); i < n.NamedChildCount(); i++ {
		if c := n.NamedChild(i); slices.Contains(until, c.Kind()) {
			end = c.StartByte()
			break
		}
	}
	return trimSigTail(tidySig(string(w.em.source[n.StartByte():end])))
}

// crumb prefixes a name with the file's proto package, when it declares one.
func (w *protoWalker) crumb(name string) string {
	if w.pkg == "" {
		return name
	}
	return w.pkg + " > " + name
}
