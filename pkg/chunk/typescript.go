// This file extends the chunker to TypeScript, the same way gosource.go
// extends it to Go: one chunk per documented symbol — function, class (+ its
// methods), interface, type alias, enum, and documented top-level const/var.
// The retrieval surface is the *JSDoc comment + signature*, not the body:
// function/method bodies are stripped exactly as Go bodies are, so
// high-signal names, types, and prose stay while the mechanics don't dilute
// the index.
//
// TypeScript has no stdlib parser, so we lean on
// tree-sitter (already a viable cgo dependency, since onnxruntime_go forces
// CGO_ENABLED=1). The .ts grammar and the .tsx grammar differ only in how
// they resolve the `<...>` ambiguity (type assertions/generics vs JSX), so
// each is wrapped once and dispatched by extension in the indexer.
package chunk

import (
	"regexp"
	"slices"
	"strconv"
	"strings"
	"sync"

	sitter "github.com/tree-sitter/go-tree-sitter"
	tsbind "github.com/tree-sitter/tree-sitter-typescript/bindings/go"
)

// Variant constants for the TypeScript chunk kinds without a Go analogue.
// Functions, methods, and const/var reuse the func/method/value variants from
// gosource.go, and a type alias reuses VariantType — they mean the same thing
// across languages, so search filters and displays stay uniform. VariantFile
// is the file's own documentation, which VariantPackage is for Go; it is not
// VariantModule, which names a declared module or namespace in the languages
// that have one.
const (
	VariantClass     = "class"
	VariantInterface = "interface"
	VariantEnum      = "enum"
	VariantFile      = "file"
)

// tsValueSigMax caps how much of a plain const/var initializer is inlined into
// its signature. A `const x = 3` carries its value; a `const cfg = {…200 lines}`
// would only bloat the chunk, so past this length the initializer is dropped
// and the signature keeps just the name and type annotation.
const tsValueSigMax = 80

// The two grammars are immutable once wrapped and safe to share across the
// per-file parsers the indexer creates, so each is built exactly once.
var (
	tsGrammarOnce  sync.Once
	tsxGrammarOnce sync.Once
	cachedTSLang   *sitter.Language
	cachedTSXLang  *sitter.Language
)

func typescriptGrammar() *sitter.Language {
	tsGrammarOnce.Do(func() { cachedTSLang = sitter.NewLanguage(tsbind.LanguageTypescript()) })
	return cachedTSLang
}

func tsxGrammar() *sitter.Language {
	tsxGrammarOnce.Do(func() { cachedTSXLang = sitter.NewLanguage(tsbind.LanguageTSX()) })
	return cachedTSXLang
}

// TypeScript chunks .ts/.mts/.cts source with the plain TypeScript grammar,
// which reads `<T>` as a type assertion / type parameter rather than JSX.
func TypeScript(content string) []Chunk { return tsSource(content, typescriptGrammar()) }

// TSX chunks .tsx and JavaScript (.js/.jsx/.mjs/.cjs) source with the TSX
// grammar, which reads `<Foo>` as a JSX element. It is a superset that also
// parses ordinary, JSX-free JavaScript, so the whole JS family routes here; the
// symbol kinds it emits (function, class, arrow-const, …) are the ones JS shares
// with TypeScript, minus the type-only interface/type-alias/enum forms.
func TSX(content string) []Chunk { return tsSource(content, tsxGrammar()) }

// tsSource parses source with the given grammar and walks its top level,
// emitting a chunk per symbol. Returns nil when there is nothing embeddable —
// the same contract GoSource and Document hold. tree-sitter is error-tolerant,
// so a file with syntax errors still yields the symbols it could parse.
func tsSource(content string, lang *sitter.Language) []Chunk {
	if strings.TrimSpace(content) == "" {
		return nil
	}
	parser := sitter.NewParser()
	defer parser.Close()
	if err := parser.SetLanguage(lang); err != nil {
		return nil
	}
	source := []byte(content)
	tree := parser.Parse(source, nil)
	if tree == nil {
		return nil
	}
	defer tree.Close()

	w := &tsWalker{source: source, seen: map[string]bool{}}
	w.fileDoc(tree.RootNode())
	w.walk(tree.RootNode(), "")
	return w.out
}

// tsWalker accumulates chunks while walking a file's syntax tree. seen keys the
// chunks already emitted so duplicate keys (function overloads, get/set pairs)
// get a numeric suffix instead of colliding on the store's UNIQUE(file_id,
// chunk_key) constraint — the TypeScript analogue of GoSource skipping the
// several `var _` blank-identifier declarations that would share one key.
type tsWalker struct {
	source []byte
	seen   map[string]bool
	out    []Chunk
}

// walk iterates the declarations directly under a container (the file root, or
// a namespace body), attaching each declaration's immediately-preceding JSDoc
// comment. prefix qualifies names nested in a namespace ("Outer.").
func (w *tsWalker) walk(container *sitter.Node, prefix string) {
	var doc string
	for i := uint(0); i < container.NamedChildCount(); i++ {
		child := container.NamedChild(i)
		if child.Kind() == "comment" {
			// Only the comment directly before a declaration documents it; a
			// non-comment sibling resets the pending doc below.
			doc = jsDoc(child, w.source)
			continue
		}
		if child.Kind() == "decorator" {
			// A decorator on a non-exported declaration (`/** … */ @dec class Foo`)
			// is a sibling of the comment; skip it without dropping the doc. An
			// exported declaration nests the decorator inside export_statement, so
			// this only fires for the unexported case.
			continue
		}
		// `export`/`export default` wrap the real declaration; re-exports and
		// bare `export default <expr>` carry no declaration to index.
		decl := child
		if child.Kind() == "export_statement" {
			d := child.ChildByFieldName("declaration")
			if d == nil {
				doc = ""
				continue
			}
			decl = d
		}
		w.declare(decl, prefix, doc)
		doc = ""
	}
}

// fileDoc emits a leading JSDoc block that documents no declaration as the
// file's own chunk, the way GoSource emits a package doc.
//
// A file whose first statement is an import, or whose only export is a
// re-export, leaves its leading block with nothing below it to own. The walk
// then drops that block, which is how a file's own prose — the `@module` and
// `@fileoverview` convention, and every design note written above the imports
// — stayed out of the index. It is the highest-signal prose in such a file:
// the symbols below it carry signatures, and only this says what the file is
// for.
func (w *tsWalker) fileDoc(root *sitter.Node) {
	start := afterPrologue(root)
	first := root.NamedChild(start)
	if first == nil || first.Kind() != "comment" {
		return
	}
	doc := jsDoc(first, w.source)
	if doc == "" || documentsNext(root.NamedChild(start+1)) {
		return
	}
	crumb := VariantFile
	w.out = append(w.out, Chunk{
		Key:     "ts/" + VariantFile,
		Heading: crumb,
		Variant: VariantFile,
		Text:    crumb + "\n\n" + doc,
		Line:    nodeLine(first),
	})
}

// afterPrologue returns the index of the first child past a shebang and any
// directive prologue, so `"use client"` above the doc block does not hide it.
func afterPrologue(root *sitter.Node) uint {
	index := uint(0)
	for ; index < root.NamedChildCount(); index++ {
		if !prologue(root.NamedChild(index)) {
			break
		}
	}
	return index
}

// prologue reports whether n is a shebang or a bare string statement, the two
// things a file may carry above its own documentation.
func prologue(n *sitter.Node) bool {
	if n.Kind() == "hash_bang_line" {
		return true
	}
	if n.Kind() != "expression_statement" || n.NamedChildCount() != 1 {
		return false
	}
	return n.NamedChild(0).Kind() == "string"
}

// documentsNext reports whether the node after a leading comment is a
// declaration the walk hands that comment to. An import, a re-export carrying
// no declaration, a directive, a second comment block, and the end of the file
// all leave the comment unowned, which makes it documentation about the file.
func documentsNext(next *sitter.Node) bool {
	if next == nil {
		return false
	}
	switch next.Kind() {
	case "comment", "import_statement":
		return false
	case "export_statement":
		return next.ChildByFieldName("declaration") != nil
	}
	return !prologue(next)
}

// declare dispatches one declaration to the emitter for its kind.
func (w *tsWalker) declare(decl *sitter.Node, prefix, doc string) {
	switch decl.Kind() {
	case "function_declaration", "generator_function_declaration":
		w.emitFunc(decl, prefix, doc)
	case "class_declaration", "abstract_class_declaration":
		w.emitClass(decl, prefix, doc)
	case "interface_declaration":
		w.emitTypeLike(decl, prefix, doc, "ts/interface/", VariantInterface)
	case "type_alias_declaration":
		w.emitTypeLike(decl, prefix, doc, "ts/type/", VariantType)
	case "enum_declaration":
		w.emitTypeLike(decl, prefix, doc, "ts/enum/", VariantEnum)
	case "lexical_declaration", "variable_declaration":
		w.emitValues(decl, prefix, doc)
	case "internal_module", "module":
		w.emitNamespace(decl, prefix)
	case "expression_statement":
		// A non-exported `namespace Foo {}` parses as an expression statement
		// wrapping the module; an exported one arrives as a declaration above.
		if m := firstNamedChildOfKind(decl, "internal_module", "module"); m != nil {
			w.emitNamespace(m, prefix)
			return
		}
		// CommonJS export assignments (`exports.foo = …`, `module.exports.foo = …`,
		// `module.exports = { … }`) are the .js/.cjs analogue of an `export`.
		if a := firstNamedChildOfKind(decl, "assignment_expression"); a != nil {
			w.emitCommonJSExport(a, prefix, doc)
		}
	}
}

// emitFunc emits a top-level function chunk (its signature, body stripped).
func (w *tsWalker) emitFunc(decl *sitter.Node, prefix, doc string) {
	name := w.fieldText(decl, "name")
	if name == "" {
		return // anonymous `export default function () {}`: nothing to key on.
	}
	qual := prefix + name
	w.emit("ts/func/"+qual, qual, VariantFunc, joinDoc(qual, w.sigBeforeBody(decl), doc), decl)
}

// emitClass emits the class header (its extends/implements clause, member
// bodies excluded) plus one chunk per member — the same split GoSource makes
// between a type and its methods.
func (w *tsWalker) emitClass(decl *sitter.Node, prefix, doc string) {
	name := w.fieldText(decl, "name")
	if name == "" {
		return
	}
	qual := prefix + name
	w.emit("ts/class/"+qual, qual, VariantClass, joinDoc(qual, w.sigBeforeBody(decl), doc), decl)
	if body := decl.ChildByFieldName("body"); body != nil {
		w.emitMembers(body, qual)
	}
}

// emitMembers emits a chunk per method of a class body, keyed by receiver so a
// method reads as "Server.start". Plain fields are skipped as low signal (like
// Go struct fields), except a field bound to an arrow/function expression — the
// class-property method form (`handleClick = () => {…}`) common in React — which
// is indexed as a method.
func (w *tsWalker) emitMembers(body *sitter.Node, recv string) {
	var doc string
	for i := uint(0); i < body.NamedChildCount(); i++ {
		m := body.NamedChild(i)
		switch m.Kind() {
		case "comment":
			doc = jsDoc(m, w.source)
			continue
		case "decorator":
			// A decorator sits between the JSDoc and the member it annotates
			// (`/** … */ @bound foo()`); skip it without dropping the pending doc.
			continue
		case "method_definition", "method_signature", "abstract_method_signature":
			w.emitMethod(m, recv, doc, w.sigBeforeBody(m))
		case "public_field_definition":
			if v := m.ChildByFieldName("value"); v != nil && isFuncValue(v) {
				w.emitMethod(m, recv, doc, w.funcValueSig(m, v))
			}
		}
		doc = ""
	}
}

// emitMethod emits one method chunk. get/set accessors and overload signatures
// share a name; uniqueKey (via emit) keeps their keys distinct.
func (w *tsWalker) emitMethod(m *sitter.Node, recv, doc, sig string) {
	name := w.fieldText(m, "name")
	if name == "" {
		return
	}
	qual := recv + "." + name
	w.emit("ts/method/"+qual, qual, VariantMethod, joinDoc(qual, sig, doc), m)
}

// emitTypeLike emits an interface / type alias / enum. These carry no bodies to
// strip — the whole declaration is signal — so the full source is rendered,
// keeping its original line breaks rather than collapsing to one line.
func (w *tsWalker) emitTypeLike(decl *sitter.Node, prefix, doc, keyPrefix, variant string) {
	name := w.fieldText(decl, "name")
	if name == "" {
		return
	}
	qual := prefix + name
	w.emit(keyPrefix+qual, qual, variant, joinDoc(qual, rawSig(decl.Utf8Text(w.source)), doc), decl)
}

// emitValues emits a chunk per declarator in a const/let/var statement. A
// declarator bound to a function is treated as a function (indexed regardless
// of doc, like every other function); any other initializer is a plain value,
// indexed only when documented — a bare `const x = 3` is low signal, the same
// reason GoSource skips undocumented var blocks.
func (w *tsWalker) emitValues(decl *sitter.Node, prefix, doc string) {
	kw := w.declKeyword(decl)
	for i := uint(0); i < decl.NamedChildCount(); i++ {
		vd := decl.NamedChild(i)
		if vd.Kind() != "variable_declarator" {
			continue
		}
		name := vd.ChildByFieldName("name")
		if name == nil || name.Kind() != "identifier" {
			continue // destructuring pattern: no single searchable name.
		}
		qual := prefix + name.Utf8Text(w.source)
		if v := vd.ChildByFieldName("value"); v != nil && isFuncValue(v) {
			w.emit("ts/func/"+qual, qual, VariantFunc, joinDoc(qual, w.declaratorFuncSig(kw, vd, v), doc), vd)
			continue
		}
		if strings.TrimSpace(doc) == "" {
			continue
		}
		w.emit("ts/"+kw+"/"+qual, qual, VariantValue, joinDoc(qual, w.valueSig(kw, vd), doc), vd)
	}
}

// emitNamespace recurses into a namespace/module body, qualifying the names it
// finds ("Outer.inner"), so symbols declared inside stay searchable.
func (w *tsWalker) emitNamespace(mod *sitter.Node, prefix string) {
	name := w.fieldText(mod, "name")
	body := mod.ChildByFieldName("body")
	if name == "" || body == nil {
		return
	}
	w.walk(body, prefix+name+".")
}

// emitCommonJSExport indexes a CommonJS export assignment — the .js/.cjs analogue
// of an `export`. `exports.foo = …` and `module.exports.foo = …` name a single
// export; `module.exports = { … }` names each property. A function value indexes
// as a function (like every other function); any other value indexes only when
// documented, the same rule emitValues applies to a plain const.
func (w *tsWalker) emitCommonJSExport(assign *sitter.Node, prefix, doc string) {
	lhs := assign.ChildByFieldName("left")
	rhs := assign.ChildByFieldName("right")
	if lhs == nil || rhs == nil || lhs.Kind() != "member_expression" {
		return
	}
	obj := lhs.ChildByFieldName("object")
	prop := w.fieldText(lhs, "property")
	if obj == nil || prop == "" {
		return
	}
	// `module.exports = { … }`: index each property, not the object itself.
	if prop == "exports" && obj.Kind() == "identifier" && obj.Utf8Text(w.source) == "module" {
		w.emitExportsObject(rhs, prefix)
		return
	}
	// `exports.NAME = …` or `module.exports.NAME = …`.
	if !w.isExportsTarget(obj) {
		return
	}
	qual := prefix + prop
	if isFuncValue(rhs) {
		w.emit("ts/func/"+qual, qual, VariantFunc, joinDoc(qual, w.funcValueSig(lhs, rhs), doc), assign)
		return
	}
	if strings.TrimSpace(doc) == "" {
		return
	}
	w.emit("ts/value/"+qual, qual, VariantValue, joinDoc(qual, w.assignValueSig(lhs, rhs), doc), assign)
}

// isExportsTarget reports whether a member-expression object is the CommonJS
// exports object — `exports` or `module.exports`.
func (w *tsWalker) isExportsTarget(n *sitter.Node) bool {
	switch n.Kind() {
	case "identifier":
		return n.Utf8Text(w.source) == "exports"
	case "member_expression":
		return w.fieldText(n, "object") == "module" && w.fieldText(n, "property") == "exports"
	}
	return false
}

// emitExportsObject indexes the function properties of a `module.exports = { … }`
// object. A shorthand property (`{ foo }`) re-exports a symbol already indexed at
// its declaration, so it is skipped rather than duplicated; a property's own
// leading JSDoc documents it, the module-level comment does not.
func (w *tsWalker) emitExportsObject(obj *sitter.Node, prefix string) {
	if obj.Kind() != "object" {
		return
	}
	var doc string
	for i := uint(0); i < obj.NamedChildCount(); i++ {
		p := obj.NamedChild(i)
		switch p.Kind() {
		case "comment":
			doc = jsDoc(p, w.source)
			continue
		case "pair":
			key := p.ChildByFieldName("key")
			val := p.ChildByFieldName("value")
			if key != nil && val != nil && isFuncValue(val) {
				qual := prefix + key.Utf8Text(w.source)
				w.emit("ts/func/"+qual, qual, VariantFunc, joinDoc(qual, w.funcValueSig(key, val), doc), p)
			}
		}
		doc = ""
	}
}

// assignValueSig renders a documented CommonJS value export — the assignment
// target and its value — dropping the value past tsValueSigMax so a large literal
// does not bloat the chunk (the name still carries the signal).
func (w *tsWalker) assignValueSig(lhs, rhs *sitter.Node) string {
	head := tidySig(string(w.source[lhs.StartByte():rhs.StartByte()])) // "exports.NAME ="
	val := oneLine(rhs.Utf8Text(w.source))
	if len(val) > tsValueSigMax {
		return strings.TrimRight(head, "= ")
	}
	return head + " " + val
}

// emit appends one chunk with a collision-free key. Callers assemble the body
// with joinDoc, so a TypeScript chunk carries the same breadcrumb-led layout as
// a Go one and search display strips the leading breadcrumb identically.
func (w *tsWalker) emit(key, crumb, variant, text string, n *sitter.Node) {
	w.out = append(w.out, Chunk{
		Key:     w.uniqueKey(key),
		Heading: crumb,
		Variant: variant,
		Text:    text,
		Line:    nodeLine(n),
	})
}

// uniqueKey returns key unchanged the first time it is seen, then key#2, key#3,
// … for repeats — keeping overloads and get/set pairs off each other's rows.
func (w *tsWalker) uniqueKey(key string) string {
	if !w.seen[key] {
		w.seen[key] = true
		return key
	}
	for i := 2; ; i++ {
		alt := key + "#" + strconv.Itoa(i)
		if !w.seen[alt] {
			w.seen[alt] = true
			return alt
		}
	}
}

// sigBeforeBody renders a declaration's signature by slicing from its start to
// the start of its `body` (a statement block for funcs/methods, the class body
// for classes), collapsing whitespace to one line. A declaration with no body
// (an abstract method or overload signature) renders whole.
func (w *tsWalker) sigBeforeBody(decl *sitter.Node) string {
	end := decl.EndByte()
	if body := decl.ChildByFieldName("body"); body != nil {
		end = body.StartByte()
	}
	return tidySig(string(w.source[decl.StartByte():end]))
}

// funcValueSig renders the signature of a class field bound to a function
// (`handleClick = (e) => {…}`), from the field's start to the function body.
func (w *tsWalker) funcValueSig(field, value *sitter.Node) string {
	end := value.EndByte()
	if body := value.ChildByFieldName("body"); body != nil {
		end = body.StartByte()
	}
	return tidySig(string(w.source[field.StartByte():end]))
}

// declaratorFuncSig renders `const foo = (…) =>` for a function-valued
// declarator, prepending the declaration keyword the declarator itself lacks.
func (w *tsWalker) declaratorFuncSig(kw string, vd, value *sitter.Node) string {
	end := value.EndByte()
	if body := value.ChildByFieldName("body"); body != nil {
		end = body.StartByte()
	}
	return tidySig(kw + " " + string(w.source[vd.StartByte():end]))
}

// valueSig renders a plain value declarator: "const NAME[: Type] = value",
// dropping the initializer past tsValueSigMax so a large literal doesn't bloat
// the chunk (the name and type still carry the signal).
func (w *tsWalker) valueSig(kw string, vd *sitter.Node) string {
	value := vd.ChildByFieldName("value")
	if value == nil {
		return tidySig(kw + " " + vd.Utf8Text(w.source))
	}
	head := tidySig(kw + " " + string(w.source[vd.StartByte():value.StartByte()]))
	val := oneLine(value.Utf8Text(w.source))
	if len(val) > tsValueSigMax {
		return strings.TrimRight(head, "= ")
	}
	return head + " " + val
}

// declKeyword reports the declaration keyword: "var" for a variable_declaration,
// otherwise the const/let token leading a lexical_declaration.
func (w *tsWalker) declKeyword(decl *sitter.Node) string {
	if decl.Kind() == "variable_declaration" {
		return "var"
	}
	for i := uint(0); i < decl.ChildCount(); i++ {
		if c := decl.Child(i); !c.IsNamed() {
			if t := c.Utf8Text(w.source); t == "const" || t == "let" {
				return t
			}
		}
	}
	return "const"
}

// fieldText returns the text of a named field child, or "" when absent.
func (w *tsWalker) fieldText(n *sitter.Node, field string) string {
	c := n.ChildByFieldName(field)
	if c == nil {
		return ""
	}
	return c.Utf8Text(w.source)
}

// isFuncValue reports whether an initializer is a function, so a const bound to
// one is chunked as a function rather than a value.
func isFuncValue(n *sitter.Node) bool {
	switch n.Kind() {
	case "arrow_function", "function", "function_expression", "generator_function":
		return true
	}
	return false
}

// jsDoc returns the cleaned prose of a `/** … */` block comment, or "" for any
// other comment (`//` and plain `/* */` are not doc comments, the way go/doc
// only associates the comment group directly above a symbol).
func jsDoc(comment *sitter.Node, source []byte) string {
	raw := comment.Utf8Text(source)
	if !strings.HasPrefix(raw, "/**") {
		return ""
	}
	return cleanJSDoc(raw)
}

// cleanJSDoc strips the `/** */` fence and the leading `*` gutter from each
// line, leaving the prose (including any @param/@returns tags, which are
// retrieval signal).
func cleanJSDoc(raw string) string {
	raw = strings.TrimSuffix(strings.TrimPrefix(raw, "/**"), "*/")
	lines := strings.Split(raw, "\n")
	for i, ln := range lines {
		ln = strings.TrimSpace(ln)
		ln = strings.TrimPrefix(ln, "*")
		lines[i] = strings.TrimSpace(ln)
	}
	return strings.TrimSpace(strings.Join(lines, "\n"))
}

// nodeLine is the 1-based source line a node starts on.
func nodeLine(n *sitter.Node) int {
	return int(n.StartPosition().Row) + 1 //nolint:gosec // G115: a source line number always fits in int.
}

// firstNamedChildOfKind returns the first named child matching any of kinds, or
// nil.
func firstNamedChildOfKind(n *sitter.Node, kinds ...string) *sitter.Node {
	for i := uint(0); i < n.NamedChildCount(); i++ {
		if c := n.NamedChild(i); slices.Contains(kinds, c.Kind()) {
			return c
		}
	}
	return nil
}

// tsWhitespace collapses runs of whitespace (including newlines) to a single
// space, used to render multi-line signatures on one line.
var tsWhitespace = regexp.MustCompile(`\s+`)

// oneLine collapses internal whitespace and trims the ends.
func oneLine(s string) string {
	return strings.TrimSpace(tsWhitespace.ReplaceAllString(s, " "))
}

// tidySig one-lines a signature and trims the trailing `{`/space left when the
// slice stops at a body brace.
func tidySig(s string) string {
	return strings.TrimRight(oneLine(s), "{ ")
}

// rawSig trims surrounding whitespace and a trailing statement semicolon while
// preserving internal line breaks, for declarations rendered whole.
func rawSig(s string) string {
	return strings.TrimRight(strings.TrimSpace(s), ";")
}
