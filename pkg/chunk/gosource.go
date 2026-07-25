// This file extends the chunker to Go source. Where Document walks a markdown
// heading tree, GoSource walks a Go file's syntax tree and emits one chunk per
// symbol — package doc, type, func, method, and documented const/var block.
// The retrieval surface is the *doc comment + signature*, not the function
// body: bodies are implementation, the way markdown frontmatter is attributes
// (see Document) — high-signal names and prose stay, the mechanics don't
// dilute the index.
//
// Extraction is tree-sitter, not go/parser + go/doc. The reason is uniformity,
// not capability: every other language here goes through tree-sitter, so Go on
// go/parser meant two parser stacks doing one job, and every shared concern —
// signature rendering, line numbering, breadcrumbs — needed writing twice. See
// walker.go for the machinery this shares with the rest.
//
// Error recovery is *not* the reason. go/parser recovers from a malformed
// declaration about as well as tree-sitter does, and both keep the
// declarations that parsed. The real cost of the swap is a cgo grammar in
// place of the standard library, and re-deriving go/doc's doc↔symbol
// association by hand — the latter is cheap, because a Go doc comment is just
// the unbroken comment group directly above a declaration.
package chunk

import (
	sitter "github.com/tree-sitter/go-tree-sitter"
	gobind "github.com/tree-sitter/tree-sitter-go/bindings/go"
)

// Chunker turns a file's full content into retrievable chunks. Document
// (markdown), GoSource (Go), TypeScript/TSX, Python, Protobuf, SQL, HCL, and
// Bash all satisfy it; the indexer dispatches on file extension.
type Chunker func(content string) []Chunk

// Variant constants for the Go-source chunk kinds. They sit alongside the
// markdown variants (title/path/narrow/full/body) in the same column.
const (
	VariantPackage = "package"
	VariantType    = "type"
	VariantFunc    = "func"
	VariantMethod  = "method"
	VariantValue   = "value" // a const or var block
)

var goLang = grammarCache{load: func() *sitter.Language {
	return sitter.NewLanguage(gobind.Language())
}}

// GoSource parses Go source and emits a chunk per symbol carrying its
// breadcrumb ("package foo > Server.Start"), rendered signature, and doc
// comment. Returns nil when the file doesn't parse into anything usable.
//
// Keys are name-based and the walk is in source order, so re-chunking an
// unchanged file overwrites the same rows rather than orphaning them — the
// same stability contract Document relies on.
func GoSource(content string) []Chunk {
	return parseSource(content, &goLang, func(root *sitter.Node, source []byte) []Chunk {
		w := &goWalker{
			em:  emitter{source: source, prefix: "go"},
			doc: docScanner{source: source, kinds: []string{"comment"}, clean: lineCommentCleaner("//")},
		}
		w.walk(root)
		return w.em.out
	})
}

// goWalker accumulates chunks while walking a file's syntax tree. pkg is the
// package name, which every breadcrumb needs, so the walk resolves it before
// emitting anything.
type goWalker struct {
	em  emitter
	doc docScanner
	pkg string
}

// walk makes two passes over the file's top level. The first finds the package
// name, because a breadcrumb cannot be built without it; the second emits a
// chunk per declaration, pairing each with the comment group above it.
//
// A file with no package clause is not Go — it is a fragment, or the parser
// recovered nothing usable — so it yields no chunks rather than a set of
// symbols breadcrumbed under the empty package.
func (w *goWalker) walk(root *sitter.Node) {
	for i := uint(0); i < root.NamedChildCount(); i++ {
		if c := root.NamedChild(i); c.Kind() == "package_clause" {
			if id := firstDescendantOfKind(c, "package_identifier"); id != nil {
				w.pkg = id.Utf8Text(w.em.source)
			}
			break
		}
	}
	if w.pkg == "" {
		return
	}

	seenPkg := false
	for i := uint(0); i < root.NamedChildCount(); i++ {
		child := root.NamedChild(i)
		if w.doc.offer(child) {
			continue
		}
		doc := w.doc.take(child)
		if child.Kind() == "package_clause" {
			// A second package clause is not valid Go, and emitting it would
			// collide on the "go/package" key. Keep the first.
			if !seenPkg {
				w.emitPackage(child, doc)
				seenPkg = true
			}
			continue
		}
		w.declare(child, doc)
	}
}

// declare dispatches one top-level declaration to the emitter for its kind.
// Imports carry no searchable prose, so they are skipped.
func (w *goWalker) declare(decl *sitter.Node, doc string) {
	switch decl.Kind() {
	case "function_declaration":
		w.emitFunc(decl, doc)
	case "method_declaration":
		w.emitMethod(decl, doc)
	case "type_declaration":
		w.emitTypes(decl, doc)
	case "const_declaration":
		w.emitValues(decl, doc, "const")
	case "var_declaration":
		w.emitValues(decl, doc, "var")
	}
}

// emitPackage emits the package-doc chunk. An undocumented package has nothing
// to retrieve, so it emits nothing.
func (w *goWalker) emitPackage(clause *sitter.Node, doc string) {
	if doc == "" {
		return
	}
	crumb := "package " + w.pkg
	w.em.out = append(w.em.out, Chunk{
		Key:     "go/package",
		Heading: crumb,
		Variant: VariantPackage,
		Text:    crumb + "\n\n" + doc,
		Line:    nodeLine(clause),
	})
}

// emitFunc emits a plain function: its signature with the body stripped.
// Functions are indexed whether or not they are documented — the name and
// signature alone are worth retrieving.
func (w *goWalker) emitFunc(decl *sitter.Node, doc string) {
	name := fieldTextOf(decl, w.em.source, "name")
	if name == "" {
		return
	}
	w.emit("func", name, VariantFunc, sigUpToField(decl, w.em.source, "body"), doc, decl)
}

// emitMethod emits a method keyed by its receiver type, so it reads as
// "Server.Start" — the same Type.Member shape the TypeScript chunker uses for
// a class member.
func (w *goWalker) emitMethod(decl *sitter.Node, doc string) {
	name := fieldTextOf(decl, w.em.source, "name")
	recv := w.receiverType(decl)
	if name == "" || recv == "" {
		return
	}
	w.emit("method", recv+"."+name, VariantMethod, sigUpToField(decl, w.em.source, "body"), doc, decl)
}

// emitTypes emits a chunk per type declared. A type has no body to strip — its
// fields are the signal — so the declaration is rendered whole, keeping its
// original line breaks. A grouped `type ( … )` block declares several types at
// once, and each gets its own chunk.
func (w *goWalker) emitTypes(decl *sitter.Node, doc string) {
	specs := namedChildrenOfKind(decl, "type_spec", "type_alias")
	for _, spec := range specs {
		name := fieldTextOf(spec, w.em.source, "name")
		if name == "" {
			continue
		}
		// A single-spec declaration renders as written ("type Server struct
		// {…}"). One spec of a group is rebuilt from its name and type instead
		// of sliced from the source: the spec carries no `type` keyword, and
		// gofmt aligns the names within a group, so slicing would embed that
		// padding ("type Line  struct") and make two identical declarations
		// differ by how their neighbours happened to be named.
		sig := rawSig(decl.Utf8Text(w.em.source))
		if len(specs) > 1 {
			sig = "type " + name + " " + rawSig(fieldTextOf(spec, w.em.source, "type"))
		}
		w.emit("type", name, VariantType, sig, doc, spec)
	}
}

// emitValues emits one chunk for a documented const/var declaration, keyed by
// the first name it declares. Undocumented declarations are skipped: a bare
// `var x int` is low signal and would only dilute results, unlike funcs and
// types whose names alone are worth indexing.
func (w *goWalker) emitValues(decl *sitter.Node, doc, kind string) {
	if doc == "" {
		return
	}
	specs := namedChildrenOfKind(decl, "const_spec", "var_spec")
	if len(specs) == 0 {
		return
	}
	name := fieldTextOf(specs[0], w.em.source, "name")
	// Blank-identifier declarations (`var _ Iface = (*T)(nil)` compile-time
	// assertions) have no searchable name, so they are skipped like other
	// low-signal values rather than being uniqued into "go/var/_~1".
	if name == "" || name == "_" {
		return
	}
	w.emit(kind, name, VariantValue, rawSig(decl.Utf8Text(w.em.source)), doc, decl)
}

// emit appends one chunk under this file's package breadcrumb.
func (w *goWalker) emit(keyPart, name, variant, sig, doc string, n *sitter.Node) {
	w.em.add(keyPart, name, w.pkg+" > "+name, variant, sig, doc, n)
}

// receiverType returns the base type name a method is declared on, with any
// pointer star and type arguments removed, so `func (c *Cache[K]) Get()` reads
// as "Cache.Get". Returns "" when the receiver has no resolvable type name.
func (w *goWalker) receiverType(decl *sitter.Node) string {
	recv := decl.ChildByFieldName("receiver")
	if recv == nil {
		return ""
	}
	if id := firstDescendantOfKind(recv, "type_identifier"); id != nil {
		return id.Utf8Text(w.em.source)
	}
	return ""
}
