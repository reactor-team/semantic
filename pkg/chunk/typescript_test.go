package chunk

import (
	"strings"
	"testing"
)

// sampleTS exercises every symbol kind TypeScript emits: a documented const, an
// interface, a type alias, an enum, a class with a constructor / method / getter
// / setter / arrow-property method, a plain function, an arrow-function const,
// an undocumented plain const (skipped), and a namespace.
const sampleTS = `/** MaxSize is the largest a widget may be. */
export const MaxSize = 100;

const internal = 3;

/** Widgeter is implemented by widgets. */
export interface Widgeter {
  widget(): boolean;
  readonly size: number;
}

/** ID identifies a widget. */
export type ID = string | number;

/** Color enumerates widget colors. */
export enum Color { Red, Green, Blue }

/** Server serves widgets. */
export class Server extends Base implements Widgeter {
  private addr: string;
  /** Construct a Server. */
  constructor(addr: string) { this.addr = addr; }
  /** start begins serving, resolving when stopped. */
  async start(ctx: Context): Promise<void> { return; }
  widget(): boolean { return true; }
  get size(): number { return 1; }
  set size(v: number) {}
  /** onTick is a class-property arrow method. */
  onTick = (n: number): void => {};
}

/** ping is a free function. */
export function ping(): boolean { return true; }

/** doubled doubles its argument. */
export const doubled = (x: number): number => x * 2;

/** Outer groups helpers. */
export namespace Outer {
  /** inner is nested. */
  export function inner(): void {}
}
`

func TestTypeScript_EmitsSymbolChunks(t *testing.T) {
	t.Parallel()
	got := TypeScript(sampleTS)
	if len(got) == 0 {
		t.Fatal("TypeScript returned no chunks")
	}

	cases := []struct {
		key         string
		wantVariant string
		wantHeading string
		wantInText  []string // substrings the embedded text must contain
	}{
		{"ts/const/MaxSize", VariantValue, "MaxSize", []string{"const MaxSize = 100", "largest a widget"}},
		{"ts/interface/Widgeter", VariantInterface, "Widgeter", []string{"interface Widgeter", "widget(): boolean", "implemented by widgets"}},
		{"ts/type/ID", VariantType, "ID", []string{"type ID = string | number", "identifies a widget"}},
		{"ts/enum/Color", VariantEnum, "Color", []string{"enum Color", "Red", "enumerates widget colors"}},
		{"ts/class/Server", VariantClass, "Server", []string{"class Server extends Base implements Widgeter", "serves widgets"}},
		{"ts/method/Server.constructor", VariantMethod, "Server.constructor", []string{"constructor(addr: string)", "Construct a Server"}},
		{"ts/method/Server.start", VariantMethod, "Server.start", []string{"async start(ctx: Context): Promise<void>", "begins serving"}},
		{"ts/method/Server.onTick", VariantMethod, "Server.onTick", []string{"onTick = (n: number): void =>", "arrow method"}},
		{"ts/func/ping", VariantFunc, "ping", []string{"function ping(): boolean", "free function"}},
		{"ts/func/doubled", VariantFunc, "doubled", []string{"const doubled = (x: number): number =>", "doubles its argument"}},
		{"ts/func/Outer.inner", VariantFunc, "Outer.inner", []string{"function inner(): void", "nested"}},
	}
	for _, tc := range cases {
		c := find(got, tc.key)
		if c.Key == "" {
			t.Errorf("%s: no chunk emitted (keys: %v)", tc.key, keysOf(got))
			continue
		}
		if c.Variant != tc.wantVariant {
			t.Errorf("%s: variant = %q, want %q", tc.key, c.Variant, tc.wantVariant)
		}
		if c.Heading != tc.wantHeading {
			t.Errorf("%s: heading = %q, want %q", tc.key, c.Heading, tc.wantHeading)
		}
		if !strings.HasPrefix(c.Text, tc.wantHeading) {
			t.Errorf("%s: text should lead with the breadcrumb, got %q", tc.key, c.Text)
		}
		for _, sub := range tc.wantInText {
			if !strings.Contains(c.Text, sub) {
				t.Errorf("%s: text missing %q\n---\n%s", tc.key, sub, c.Text)
			}
		}
		if c.Line < 1 {
			t.Errorf("%s: line = %d, want >= 1", tc.key, c.Line)
		}
	}

	// Method bodies are not embedded — only the signature.
	if c := find(got, "ts/method/Server.start"); strings.Contains(c.Text, "return;") {
		t.Errorf("method chunk should not contain the body: %q", c.Text)
	}
	// Undocumented plain consts are skipped as low signal.
	if c := find(got, "ts/const/internal"); c.Key != "" {
		t.Errorf("undocumented const should be skipped, got chunk %q", c.Key)
	}
	// Plain fields are not indexed (only arrow-property methods among fields are).
	if c := find(got, "ts/method/Server.addr"); c.Key != "" {
		t.Errorf("plain field should not become a chunk, got %q", c.Key)
	}
	// A getter and setter share a name; the second gets a suffixed key rather
	// than colliding on UNIQUE(file_id, chunk_key).
	if c := find(got, "ts/method/Server.size"); c.Key == "" {
		t.Errorf("getter chunk missing (keys: %v)", keysOf(got))
	}
	if c := find(got, "ts/method/Server.size#2"); c.Key == "" {
		t.Errorf("setter should be keyed distinctly as ts/method/Server.size#2 (keys: %v)", keysOf(got))
	}
}

func TestTypeScript_TSXAndEmpty(t *testing.T) {
	t.Parallel()
	// The TSX grammar parses JSX-bearing source the plain grammar would reject.
	const tsx = `/** Button renders a button. */
export function Button(props: Props) {
  return <button onClick={props.onClick}>{props.label}</button>;
}`
	got := TSX(tsx)
	c := find(got, "ts/func/Button")
	if c.Key == "" {
		t.Fatalf("TSX did not emit the Button function (keys: %v)", keysOf(got))
	}
	if !strings.Contains(c.Text, "function Button(props: Props)") {
		t.Errorf("Button signature missing: %q", c.Text)
	}
	if strings.Contains(c.Text, "<button") {
		t.Errorf("JSX body should not be embedded: %q", c.Text)
	}

	if got := TypeScript(""); got != nil {
		t.Errorf("empty content should yield nil, got %v", keysOf(got))
	}
	if got := TypeScript("// just a comment, no symbols\n"); len(got) != 0 {
		t.Errorf("source with no symbols should yield no chunks, got %v", keysOf(got))
	}
}

// A decorator sits between a symbol's JSDoc and the symbol itself. The walker
// must skip the decorator without dropping the pending doc, or decorator-heavy
// code (NestJS, Angular, MobX) loses exactly the prose the index retrieves.
func TestTypeScript_DecoratorKeepsDoc(t *testing.T) {
	t.Parallel()
	const src = `/** Widget is a decorated, non-exported class. */
@Component({ selector: "widget" })
class Widget {
  /** fetch loads a record by id. */
  @action
  fetch(id: string): Promise<void> { return; }
}`
	got := TypeScript(src)

	if c := find(got, "ts/class/Widget"); !strings.Contains(c.Text, "decorated, non-exported class") {
		t.Errorf("decorated top-level class lost its doc: %q (keys: %v)", c.Text, keysOf(got))
	}
	c := find(got, "ts/method/Widget.fetch")
	if c.Key == "" {
		t.Fatalf("decorated method missing (keys: %v)", keysOf(got))
	}
	if !strings.Contains(c.Text, "loads a record by id") {
		t.Errorf("decorated method lost its doc: %q", c.Text)
	}
	if !strings.Contains(c.Text, "fetch(id: string): Promise<void>") {
		t.Errorf("decorated method signature missing: %q", c.Text)
	}
}

// CommonJS export assignments are the .js/.cjs analogue of an `export`: a
// function value indexes as a function, a documented value as a value, and a
// `module.exports = { … }` object indexes its inline functions while leaving
// shorthand re-exports (already indexed at their declaration) alone.
func TestTypeScript_CommonJSExports(t *testing.T) {
	t.Parallel()
	const members = `/** add sums two numbers. */
exports.add = function(a, b) { return a + b; };
module.exports.mul = (a, b) => a * b;
/** VERSION is the module version. */
exports.VERSION = "1.2.3";
exports.undocumented = 42;`
	got := TSX(members)

	cases := []struct {
		key         string
		wantVariant string
		wantInText  []string
	}{
		{"ts/func/add", VariantFunc, []string{"exports.add = function(a, b)", "sums two numbers"}},
		{"ts/func/mul", VariantFunc, []string{"module.exports.mul = (a, b) =>"}},
		{"ts/value/VERSION", VariantValue, []string{`exports.VERSION = "1.2.3"`, "module version"}},
	}
	for _, tc := range cases {
		c := find(got, tc.key)
		if c.Key == "" {
			t.Errorf("%s: no chunk emitted (keys: %v)", tc.key, keysOf(got))
			continue
		}
		if c.Variant != tc.wantVariant {
			t.Errorf("%s: variant = %q, want %q", tc.key, c.Variant, tc.wantVariant)
		}
		for _, sub := range tc.wantInText {
			if !strings.Contains(c.Text, sub) {
				t.Errorf("%s: text missing %q\n---\n%s", tc.key, sub, c.Text)
			}
		}
	}
	// A function body is never embedded, even reached through an assignment.
	if c := find(got, "ts/func/add"); strings.Contains(c.Text, "return a + b") {
		t.Errorf("assigned-function chunk should not contain the body: %q", c.Text)
	}
	// An undocumented non-function export is low signal, like an undocumented const.
	if c := find(got, "ts/value/undocumented"); c.Key != "" {
		t.Errorf("undocumented value export should be skipped, got %q", c.Key)
	}

	const object = `function sub(a, b) { return a - b; }
module.exports = { sub, inline: (a, b) => a / b };`
	got = TSX(object)
	if c := find(got, "ts/func/inline"); c.Key == "" || !strings.Contains(c.Text, "inline: (a, b) =>") {
		t.Errorf("inline object-export function missing or malformed: %q (keys: %v)", c.Text, keysOf(got))
	}
	// `sub` is indexed once, at its declaration; the shorthand re-export must not
	// emit a duplicate keyed as sub#2.
	if c := find(got, "ts/func/sub"); c.Key == "" {
		t.Errorf("declared function sub missing (keys: %v)", keysOf(got))
	}
	if c := find(got, "ts/func/sub#2"); c.Key != "" {
		t.Errorf("shorthand re-export should not duplicate sub, got %q", c.Key)
	}
}
