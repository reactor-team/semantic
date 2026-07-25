// This file chunks the languages whose declarations all have the same shape: a
// named node, optionally holding members, documented by the comment group
// directly above it. Java, C#, Rust, C, C++, Ruby, PHP, Scala, and Lua differ
// in node names and almost nothing else, so each is a table entry rather than
// a file.
//
// Go, TypeScript, and Python keep hand-written walkers. That is not an
// oversight — each has a quirk the table cannot express without growing a flag
// that only one caller sets: Go qualifies methods by a receiver buried under a
// pointer and type arguments, TypeScript has to recognise CommonJS export
// assignments and function-valued consts, and Python keeps its documentation
// *inside* the body rather than above the declaration. A table is the right
// tool for the regular cases and the wrong one for the irregular, so the line
// is drawn there deliberately.
package chunk

import (
	"slices"
	"strings"

	luabind "github.com/tree-sitter-grammars/tree-sitter-lua/bindings/go"
	sitter "github.com/tree-sitter/go-tree-sitter"
	csharpbind "github.com/tree-sitter/tree-sitter-c-sharp/bindings/go"
	cbind "github.com/tree-sitter/tree-sitter-c/bindings/go"
	cppbind "github.com/tree-sitter/tree-sitter-cpp/bindings/go"
	javabind "github.com/tree-sitter/tree-sitter-java/bindings/go"
	phpbind "github.com/tree-sitter/tree-sitter-php/bindings/go"
	rubybind "github.com/tree-sitter/tree-sitter-ruby/bindings/go"
	rustbind "github.com/tree-sitter/tree-sitter-rust/bindings/go"
	scalabind "github.com/tree-sitter/tree-sitter-scala/bindings/go"
)

// Variant constants for the declaration kinds these languages add. Class,
// interface, and enum are already declared by the TypeScript chunker and are
// reused here — a Java class and a TypeScript class are the same concept to a
// reader filtering search results, so they share a variant.
const (
	VariantStruct = "struct"
	VariantTrait  = "trait"
	VariantModule = "module"
)

// declDef describes one kind of declaration: how to recognise it, name it,
// render it, and whether it holds members worth chunking on their own.
type declDef struct {
	kinds   []string // tree-sitter node kinds that match
	variant string   // chunk variant
	keyPart string   // key segment, e.g. "func" in "java/func/ping"
	name    string   // field holding the name; empty means use nameFn
	nameFn  func(n *sitter.Node, source []byte) string
	body    string // field to slice the signature up to; empty renders whole

	// members names the field holding this declaration's members. A member is
	// chunked separately and qualified as Parent.member, and the parent then
	// renders as its header rather than whole.
	members string

	// qualifyOnly marks a node that scopes its members without being a symbol
	// itself — a Rust `impl` block names no new thing, it attaches methods to a
	// type that is already declared elsewhere.
	qualifyOnly bool
}

// langDef is one language's declaration table plus how to read its comments.
type langDef struct {
	prefix   string // key namespace: "java", "rs", "cpp", …
	grammar  *grammarCache
	comments []string // node kinds carrying comments
	clean    func(raw string) []string
	decls    []declDef
}

// chunkWith parses content and emits a chunk per declaration the table
// recognises. Unrecognised nodes are skipped rather than guessed at, so a
// grammar update that renames a node loses a symbol kind instead of emitting
// nonsense — and the per-language tests are what catch that.
func (l *langDef) chunkWith(content string) []Chunk {
	return parseSource(content, l.grammar, func(root *sitter.Node, source []byte) []Chunk {
		w := &tableWalker{
			lang: l,
			em:   emitter{source: source, prefix: l.prefix},
			doc: docScanner{
				source: source,
				kinds:  l.comments,
				clean:  l.clean,
			},
		}
		w.walk(root, "")
		return w.em.out
	})
}

type tableWalker struct {
	lang *langDef
	em   emitter
	doc  docScanner
}

// walk emits a chunk for each declaration among container's named children,
// recursing into anything that holds members. prefix qualifies member names
// ("Server." inside a class body), and is empty at the top level.
func (w *tableWalker) walk(container *sitter.Node, prefix string) {
	w.doc.reset()
	w.walkInto(container, prefix)
}

// walkInto is walk without the scope reset, so descending a wrapper carries
// the pending comment across.
//
// A wrapper must be transparent to documentation. Ruby writes a method's
// comment as a sibling of the `body_statement` that holds the method, so
// consuming the comment on the way into the wrapper — as an earlier version
// did — dropped the documentation of the first method in every class.
func (w *tableWalker) walkInto(container *sitter.Node, prefix string) {
	for i := uint(0); i < container.NamedChildCount(); i++ {
		child := container.NamedChild(i)
		if w.doc.offer(child) {
			continue
		}
		if def := w.lang.match(child); def != nil {
			w.emitDecl(child, def, prefix, w.doc.take(child))
			continue
		}
		// Some grammars wrap declarations in a list node (a Ruby class body, a
		// Rust impl's declaration_list). Descend so the wrapper does not hide
		// what is under it, and leave any pending comment for the declaration
		// inside.
		if child.NamedChildCount() > 0 && isWrapper(child.Kind()) {
			w.walkInto(child, prefix)
			continue
		}
		// Any other statement ends the comment's reach: a comment separated
		// from a declaration by real code does not document it.
		w.doc.take(child)
	}
}

// emitDecl emits one declaration and recurses into its members.
func (w *tableWalker) emitDecl(n *sitter.Node, def *declDef, prefix, doc string) {
	name := w.nameOf(n, def)
	if name == "" {
		return
	}
	qualified := prefix + name

	body := w.memberContainer(n, def)
	if def.qualifyOnly {
		// The node scopes its members but is not itself a symbol.
		if body != nil {
			w.walkMembers(n, name+".")
		}
		return
	}

	// A type whose body declares no members of its own renders whole — its
	// fields are the signal, the way a Go struct's are. One that does declare
	// members renders as its header, because the members become chunks in
	// their own right and repeating them here would embed the file twice.
	sig := rawSig(n.Utf8Text(w.em.source))
	if def.body != "" && (body != nil || def.members == "") {
		sig = w.headerSig(n, def.body)
	}
	w.em.add(def.keyPart, qualified, qualified, def.variant, sig, doc, n)

	if body != nil {
		w.walkMembers(n, qualified+".")
	}
}

// walkMembers walks a declaration's own children rather than descending
// straight to its body field.
//
// Ruby puts the first comment of a class *between* the class name and the body
// node, so a walk that jumped to the body would never see it and the first
// method in every class would lose its documentation. Starting from the
// declaration itself picks those up, and the body is reached anyway because
// walk descends through wrapper nodes.
func (w *tableWalker) walkMembers(decl *sitter.Node, prefix string) {
	w.walk(decl, prefix)
}

// headerSig renders a declaration's header: everything up to its body, and up
// to any comment that precedes the body. Ruby writes the first comment of a
// class *inside* the class node and before its body field, so slicing on the
// field alone would pull a method's documentation into the class signature.
func (w *tableWalker) headerSig(n *sitter.Node, bodyField string) string {
	end := n.EndByte()
	if f := n.ChildByFieldName(bodyField); f != nil {
		end = f.StartByte()
	}
	for i := uint(0); i < n.NamedChildCount(); i++ {
		c := n.NamedChild(i)
		if slices.Contains(w.lang.comments, c.Kind()) && c.StartByte() < end {
			end = c.StartByte()
		}
	}
	return trimSigTail(tidySig(string(w.em.source[n.StartByte():end])))
}

// trimSigTail drops the punctuation left dangling when a body is sliced off —
// Scala's `def f: Int =` and the brace of a C-family header. It carries no
// meaning into an embedding and only makes two renderings of the same
// signature differ.
func trimSigTail(sig string) string {
	return strings.TrimSpace(strings.TrimRight(strings.TrimSpace(sig), "={:"))
}

// memberContainer returns the node holding a declaration's members, or nil
// when it declares none. A class with an empty body counts as none.
func (w *tableWalker) memberContainer(n *sitter.Node, def *declDef) *sitter.Node {
	if def.members == "" {
		return nil
	}
	body := n.ChildByFieldName(def.members)
	if body == nil || !w.hasDecl(body) {
		return nil
	}
	return body
}

// hasDecl reports whether a subtree contains a declaration that will actually
// emit a chunk, so a data-only body is told apart from one holding methods.
//
// It resolves each candidate's name rather than only matching its kind. C++
// writes a data member and a method with the same node kind
// (`field_declaration`), and only the name resolver tells them apart — matching
// on kind alone made every plain struct look like it had methods, and cost it
// its fields in the rendered signature.
func (w *tableWalker) hasDecl(n *sitter.Node) bool {
	for i := uint(0); i < n.NamedChildCount(); i++ {
		c := n.NamedChild(i)
		if d := w.lang.match(c); d != nil && !d.qualifyOnly && w.nameOf(c, d) != "" {
			return true
		}
		if isWrapper(c.Kind()) && w.hasDecl(c) {
			return true
		}
	}
	return false
}

// nameOf resolves a declaration's name, by field or by the table's resolver.
func (w *tableWalker) nameOf(n *sitter.Node, def *declDef) string {
	if def.nameFn != nil {
		return def.nameFn(n, w.em.source)
	}
	return strings.TrimSpace(fieldTextOf(n, w.em.source, def.name))
}

// match returns the table entry for a node kind, or nil.
func (l *langDef) match(n *sitter.Node) *declDef {
	kind := n.Kind()
	for i := range l.decls {
		if slices.Contains(l.decls[i].kinds, kind) {
			return &l.decls[i]
		}
	}
	return nil
}

// isWrapper reports whether a node kind is a list that holds declarations
// without being one. Descending through these is what lets a Ruby class body
// or a Rust impl block yield its methods.
func isWrapper(kind string) bool {
	switch kind {
	case "declaration_list", "body_statement", "template_body", "class_body",
		"interface_body", "enum_body", "field_declaration_list", "block":
		return true
	}
	return false
}

// cDeclaratorName digs a C or C++ function's name out of its declarator. C
// declares a function as a type plus a declarator rather than a name field, so
// the identifier sits under one or more wrappers (a pointer return type adds
// another).
func cDeclaratorName(n *sitter.Node, source []byte) string {
	d := n.ChildByFieldName("declarator")
	if d == nil {
		return ""
	}
	fn := d
	if fn.Kind() != "function_declarator" {
		fn = firstDescendantOfKind(d, "function_declarator")
		if fn == nil {
			return ""
		}
	}
	inner := fn.ChildByFieldName("declarator")
	if inner == nil {
		return ""
	}
	// A qualified C++ name (`Server::start`) already reads as Type.member, so
	// it is taken whole rather than reduced to the trailing identifier.
	return strings.TrimSpace(inner.Utf8Text(source))
}

// cFieldIsFunction keeps a C++ field_declaration only when it declares a
// method. A field_declaration is also how a plain data member is written, and
// a data member is not worth its own chunk.
func cFieldIsFunction(n *sitter.Node, source []byte) string {
	if firstDescendantOfKind(n, "function_declarator") == nil {
		return ""
	}
	return cDeclaratorName(n, source)
}

// ---------------------------------------------------------------------------
// The tables. Node kinds below were read off each grammar rather than guessed;
// the per-language tests fail loudly if a grammar upgrade renames one.
// ---------------------------------------------------------------------------

var javaLang = langDef{
	prefix:   "java",
	grammar:  &grammarCache{load: func() *sitter.Language { return sitter.NewLanguage(javabind.Language()) }},
	comments: []string{"line_comment", "block_comment", "comment"},
	clean:    javadocCleaner,
	decls: []declDef{
		{kinds: []string{"class_declaration"}, variant: VariantClass, keyPart: "class", name: "name", body: "body", members: "body"},
		{kinds: []string{"interface_declaration"}, variant: VariantInterface, keyPart: "interface", name: "name", body: "body", members: "body"},
		{kinds: []string{"enum_declaration"}, variant: VariantEnum, keyPart: "enum", name: "name", body: "body", members: "body"},
		{kinds: []string{"record_declaration"}, variant: VariantClass, keyPart: "record", name: "name", body: "body", members: "body"},
		{kinds: []string{"annotation_type_declaration"}, variant: VariantInterface, keyPart: "annotation", name: "name", body: "body"},
		{kinds: []string{"method_declaration", "constructor_declaration"}, variant: VariantMethod, keyPart: "method", name: "name", body: "body"},
	},
}

var rustLang = langDef{
	prefix:   "rs",
	grammar:  &grammarCache{load: func() *sitter.Language { return sitter.NewLanguage(rustbind.Language()) }},
	comments: []string{"line_comment", "block_comment"},
	clean:    rustDocCleaner,
	decls: []declDef{
		{kinds: []string{"struct_item"}, variant: VariantStruct, keyPart: "struct", name: "name", body: "body", members: "body"},
		{kinds: []string{"enum_item"}, variant: VariantEnum, keyPart: "enum", name: "name", body: "body", members: "body"},
		{kinds: []string{"union_item"}, variant: VariantStruct, keyPart: "union", name: "name", body: "body"},
		{kinds: []string{"trait_item"}, variant: VariantTrait, keyPart: "trait", name: "name", body: "body", members: "body"},
		{kinds: []string{"mod_item"}, variant: VariantModule, keyPart: "mod", name: "name", body: "body", members: "body"},
		{kinds: []string{"type_item"}, variant: VariantType, keyPart: "type", name: "name"},
		{kinds: []string{"impl_item"}, name: "type", members: "body", qualifyOnly: true},
		{kinds: []string{"function_item", "function_signature_item"}, variant: VariantFunc, keyPart: "func", name: "name", body: "body"},
		{kinds: []string{"macro_definition"}, variant: VariantFunc, keyPart: "macro", name: "name", body: "body"},
	},
}

var cLang = langDef{
	prefix:   "c",
	grammar:  &grammarCache{load: func() *sitter.Language { return sitter.NewLanguage(cbind.Language()) }},
	comments: []string{"comment"},
	clean:    lineCommentCleaner("//"),
	decls: []declDef{
		{kinds: []string{"function_definition"}, variant: VariantFunc, keyPart: "func", nameFn: cDeclaratorName, body: "body"},
		{kinds: []string{"struct_specifier"}, variant: VariantStruct, keyPart: "struct", name: "name"},
		{kinds: []string{"union_specifier"}, variant: VariantStruct, keyPart: "union", name: "name"},
		{kinds: []string{"enum_specifier"}, variant: VariantEnum, keyPart: "enum", name: "name"},
		{kinds: []string{"type_definition"}, variant: VariantType, keyPart: "type", name: "declarator"},
	},
}

var cppLang = langDef{
	prefix:   "cpp",
	grammar:  &grammarCache{load: func() *sitter.Language { return sitter.NewLanguage(cppbind.Language()) }},
	comments: []string{"comment"},
	clean:    lineCommentCleaner("//"),
	decls: []declDef{
		{kinds: []string{"class_specifier"}, variant: VariantClass, keyPart: "class", name: "name", body: "body", members: "body"},
		{kinds: []string{"struct_specifier"}, variant: VariantStruct, keyPart: "struct", name: "name", body: "body", members: "body"},
		{kinds: []string{"union_specifier"}, variant: VariantStruct, keyPart: "union", name: "name"},
		{kinds: []string{"enum_specifier"}, variant: VariantEnum, keyPart: "enum", name: "name"},
		{kinds: []string{"namespace_definition"}, variant: VariantModule, keyPart: "namespace", name: "name", body: "body", members: "body"},
		{kinds: []string{"function_definition"}, variant: VariantFunc, keyPart: "func", nameFn: cDeclaratorName, body: "body"},
		{kinds: []string{"field_declaration"}, variant: VariantMethod, keyPart: "method", nameFn: cFieldIsFunction},
		{kinds: []string{"type_definition"}, variant: VariantType, keyPart: "type", name: "declarator"},
	},
}

var rubyLang = langDef{
	prefix:   "rb",
	grammar:  &grammarCache{load: func() *sitter.Language { return sitter.NewLanguage(rubybind.Language()) }},
	comments: []string{"comment"},
	clean:    lineCommentCleaner("#"),
	decls: []declDef{
		{kinds: []string{"class"}, variant: VariantClass, keyPart: "class", name: "name", body: "body", members: "body"},
		{kinds: []string{"module"}, variant: VariantModule, keyPart: "module", name: "name", body: "body", members: "body"},
		{kinds: []string{"singleton_class"}, variant: VariantClass, keyPart: "class", name: "value", body: "body", members: "body"},
		{kinds: []string{"method", "singleton_method"}, variant: VariantMethod, keyPart: "method", name: "name", body: "body"},
	},
}

var phpLang = langDef{
	prefix:   "php",
	grammar:  &grammarCache{load: func() *sitter.Language { return sitter.NewLanguage(phpbind.LanguagePHP()) }},
	comments: []string{"comment"},
	clean:    javadocCleaner,
	decls: []declDef{
		{kinds: []string{"class_declaration"}, variant: VariantClass, keyPart: "class", name: "name", body: "body", members: "body"},
		{kinds: []string{"interface_declaration"}, variant: VariantInterface, keyPart: "interface", name: "name", body: "body", members: "body"},
		{kinds: []string{"trait_declaration"}, variant: VariantTrait, keyPart: "trait", name: "name", body: "body", members: "body"},
		{kinds: []string{"enum_declaration"}, variant: VariantEnum, keyPart: "enum", name: "name", body: "body", members: "body"},
		{kinds: []string{"method_declaration"}, variant: VariantMethod, keyPart: "method", name: "name", body: "body"},
		{kinds: []string{"function_definition"}, variant: VariantFunc, keyPart: "func", name: "name", body: "body"},
	},
}

var csharpLang = langDef{
	prefix:   "cs",
	grammar:  &grammarCache{load: func() *sitter.Language { return sitter.NewLanguage(csharpbind.Language()) }},
	comments: []string{"comment"},
	clean:    javadocCleaner,
	decls: []declDef{
		{kinds: []string{"class_declaration"}, variant: VariantClass, keyPart: "class", name: "name", body: "body", members: "body"},
		{kinds: []string{"interface_declaration"}, variant: VariantInterface, keyPart: "interface", name: "name", body: "body", members: "body"},
		{kinds: []string{"struct_declaration"}, variant: VariantStruct, keyPart: "struct", name: "name", body: "body", members: "body"},
		{kinds: []string{"record_declaration"}, variant: VariantClass, keyPart: "record", name: "name", body: "body", members: "body"},
		{kinds: []string{"enum_declaration"}, variant: VariantEnum, keyPart: "enum", name: "name", body: "body"},
		{kinds: []string{"namespace_declaration", "file_scoped_namespace_declaration"}, variant: VariantModule, keyPart: "namespace", name: "name", body: "body", members: "body"},
		{kinds: []string{"method_declaration", "constructor_declaration"}, variant: VariantMethod, keyPart: "method", name: "name", body: "body"},
		{kinds: []string{"property_declaration"}, variant: VariantValue, keyPart: "property", name: "name", body: "accessors"},
	},
}

var luaLang = langDef{
	prefix:   "lua",
	grammar:  &grammarCache{load: func() *sitter.Language { return sitter.NewLanguage(luabind.Language()) }},
	comments: []string{"comment"},
	clean:    lineCommentCleaner("---", "--"),
	decls: []declDef{
		// A Lua function's name may be a dotted or colon path
		// (`Server.start`, `Server:start`). The node's own text already reads
		// that way, so it is used as written rather than reduced to the last
		// segment.
		{kinds: []string{"function_declaration"}, variant: VariantFunc, keyPart: "func", name: "name", body: "body"},
	},
}

var scalaLang = langDef{
	prefix:   "scala",
	grammar:  &grammarCache{load: func() *sitter.Language { return sitter.NewLanguage(scalabind.Language()) }},
	comments: []string{"comment", "block_comment"},
	clean:    javadocCleaner,
	decls: []declDef{
		{kinds: []string{"class_definition"}, variant: VariantClass, keyPart: "class", name: "name", body: "body", members: "body"},
		{kinds: []string{"object_definition"}, variant: VariantModule, keyPart: "object", name: "name", body: "body", members: "body"},
		{kinds: []string{"trait_definition"}, variant: VariantTrait, keyPart: "trait", name: "name", body: "body", members: "body"},
		{kinds: []string{"enum_definition"}, variant: VariantEnum, keyPart: "enum", name: "name", body: "body", members: "body"},
		{kinds: []string{"type_definition"}, variant: VariantType, keyPart: "type", name: "name"},
		{kinds: []string{"function_definition", "function_declaration"}, variant: VariantFunc, keyPart: "func", name: "name", body: "body"},
	},
}

// javadocCleaner strips `/** … */` and `//` markers. Javadoc, PHPDoc, and
// Scaladoc share the shape, including the leading `*` on continuation lines.
func javadocCleaner(raw string) []string {
	if lines := blockCommentLines(raw); lines != nil {
		return lines
	}
	return lineCommentCleaner("//", "#")(raw)
}

// rustDocCleaner handles Rust's doc comments. `///` documents the item below
// and `//!` documents the enclosing module; both are prose. A plain `//` is an
// implementation note, and Rust's own tooling does not treat it as
// documentation, so neither does this.
func rustDocCleaner(raw string) []string {
	for _, marker := range []string{"///", "//!"} {
		if rest, ok := strings.CutPrefix(raw, marker); ok {
			return []string{strings.TrimSpace(rest)}
		}
	}
	if lines := blockCommentLines(raw); lines != nil {
		return lines
	}
	return nil
}

// The exported entry points. Each is a Chunker, registered per extension in
// pkg/index.

// Java chunks Java source: classes, interfaces, enums, records, and methods.
func Java(content string) []Chunk { return javaLang.chunkWith(content) }

// Rust chunks Rust source: structs, enums, traits, modules, functions, and the
// methods an `impl` block attaches to a type.
func Rust(content string) []Chunk { return rustLang.chunkWith(content) }

// C chunks C source: functions, structs, unions, enums, and typedefs.
func C(content string) []Chunk { return cLang.chunkWith(content) }

// CPP chunks C++ source: classes, structs, namespaces, functions, and methods.
func CPP(content string) []Chunk { return cppLang.chunkWith(content) }

// Ruby chunks Ruby source: classes, modules, and methods.
func Ruby(content string) []Chunk { return rubyLang.chunkWith(content) }

// PHP chunks PHP source: classes, interfaces, traits, enums, functions, and
// methods.
func PHP(content string) []Chunk { return phpLang.chunkWith(content) }

// Scala chunks Scala source: classes, objects, traits, enums, and functions.
func Scala(content string) []Chunk { return scalaLang.chunkWith(content) }

// CSharp chunks C# source: namespaces, classes, interfaces, structs, records,
// enums, methods, and properties.
func CSharp(content string) []Chunk { return csharpLang.chunkWith(content) }

// Lua chunks Lua source: functions, including the dotted and colon paths a
// module or a method is declared with.
func Lua(content string) []Chunk { return luaLang.chunkWith(content) }
