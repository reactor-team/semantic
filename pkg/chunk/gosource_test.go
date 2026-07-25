package chunk

import (
	"strings"
	"testing"
)

// sampleGo exercises every symbol kind GoSource emits: package doc, a
// documented type, a constructor, a method, a plain func, and documented /
// undocumented value blocks.
const sampleGo = `// Package widget makes widgets.
package widget

import "context"

// MaxSize is the largest a widget may be.
const MaxSize = 100

var internal = 3

// Widgeter is implemented by widgets.
type Widgeter interface {
	Widget() bool
}

// compile-time assertion.
var _ Widgeter = (*Server)(nil)

// compile-time assertion.
var _ Widgeter = (*Server)(nil)

// Server serves widgets.
type Server struct {
	addr string
}

// NewServer builds a Server.
func NewServer(addr string) *Server { return &Server{addr: addr} }

// Start begins serving, blocking until ctx is cancelled.
func (s *Server) Start(ctx context.Context) error { return nil }

// Ping is a free function.
func Ping() bool { return true }
`

func TestGoSource_EmitsSymbolChunks(t *testing.T) {
	t.Parallel()
	got := GoSource(sampleGo)
	if len(got) == 0 {
		t.Fatal("GoSource returned no chunks")
	}

	cases := []struct {
		key         string
		wantVariant string
		wantHeading string
		wantInText  []string // substrings the embedded text must contain
	}{
		{"go/package", VariantPackage, "package widget", []string{"makes widgets"}},
		{"go/type/Server", VariantType, "widget > Server", []string{"type Server struct", "serves widgets"}},
		{"go/func/NewServer", VariantFunc, "widget > NewServer", []string{"func NewServer(addr string) *Server", "builds a Server"}},
		{"go/method/Server.Start", VariantMethod, "widget > Server.Start", []string{"func (s *Server) Start(ctx context.Context) error", "blocking until ctx"}},
		{"go/func/Ping", VariantFunc, "widget > Ping", []string{"func Ping() bool", "free function"}},
		{"go/const/MaxSize", VariantValue, "widget > MaxSize", []string{"const MaxSize = 100", "largest a widget"}},
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

	// Function bodies are not embedded — only the signature.
	if c := find(got, "go/method/Server.Start"); strings.Contains(c.Text, "return nil") {
		t.Errorf("method chunk should not contain the body: %q", c.Text)
	}
	// Undocumented value blocks are skipped.
	if c := find(got, "go/var/internal"); c.Key != "" {
		t.Errorf("undocumented var should be skipped, got chunk %q", c.Key)
	}
	// Blank-identifier declarations (compile-time assertions) are skipped —
	// they have no searchable name and several would collide on "go/var/_".
	if c := find(got, "go/var/_"); c.Key != "" {
		t.Errorf("blank-identifier var should be skipped, got chunk %q", c.Key)
	}
}

func TestGoSource_NonGoOrEmpty(t *testing.T) {
	t.Parallel()
	if got := GoSource("this is not go source {{{"); got != nil {
		// A hard parse failure with no usable AST yields nil.
		for _, c := range got {
			if c.Variant == VariantFunc || c.Variant == VariantType {
				t.Errorf("garbage input produced a real symbol chunk: %q", c.Key)
			}
		}
	}
	if got := GoSource("package empty\n"); len(got) != 0 {
		t.Errorf("package with no symbols/doc should yield no chunks, got %v", keysOf(got))
	}
}

// A generic receiver must key on the base type, or `Cache[K,V].Get` and
// `Cache.Get` would be two different symbols depending on how the receiver was
// written.
func TestGoSource_GenericReceiverKeysOnBaseType(t *testing.T) {
	t.Parallel()
	src := "package store\n\n" +
		"// Get returns the value for k.\n" +
		"func (c *Cache[K, V]) Get(k K) (V, bool) { var v V; return v, false }\n"
	got := GoSource(src)
	c := find(got, "go/method/Cache.Get")
	if c.Key == "" {
		t.Fatalf("no chunk for the generic method (keys: %v)", keysOf(got))
	}
	if c.Heading != "store > Cache.Get" {
		t.Errorf("heading = %q, want %q", c.Heading, "store > Cache.Get")
	}
}

// A grouped `type ( … )` block declares several types at once. Each needs its
// own chunk, and each must still render as a type declaration — the spec node
// alone does not carry the keyword.
func TestGoSource_GroupedTypeBlock(t *testing.T) {
	t.Parallel()
	src := "package geom\n\n" +
		"// Shapes are the supported primitives.\n" +
		"type (\n\tPoint struct{ X, Y int }\n\tLine  struct{ A, B Point }\n)\n"
	got := GoSource(src)
	for _, name := range []string{"Point", "Line"} {
		c := find(got, "go/type/"+name)
		if c.Key == "" {
			t.Errorf("no chunk for %s (keys: %v)", name, keysOf(got))
			continue
		}
		if !strings.Contains(c.Text, "type "+name+" struct") {
			t.Errorf("%s: signature should read as a type declaration, got %q", name, c.Text)
		}
	}
}

// A directive is an instruction to a tool, not prose. It must not become a
// symbol's documentation, and a value block carrying only directives counts as
// undocumented.
func TestGoSource_DirectivesAreNotDocumentation(t *testing.T) {
	t.Parallel()
	src := "package gen\n\n" +
		"//go:generate stringer -type=Kind\n" +
		"// Kind enumerates the kinds.\n" +
		"type Kind int\n\n" +
		"//nolint:gochecknoglobals\n" +
		"var registry = map[string]Kind{}\n"
	got := GoSource(src)

	c := find(got, "go/type/Kind")
	if c.Key == "" {
		t.Fatalf("no chunk for Kind (keys: %v)", keysOf(got))
	}
	if strings.Contains(c.Text, "go:generate") {
		t.Errorf("directive leaked into documentation: %q", c.Text)
	}
	if !strings.Contains(c.Text, "enumerates the kinds") {
		t.Errorf("prose alongside a directive was lost: %q", c.Text)
	}
	if c := find(got, "go/var/registry"); c.Key != "" {
		t.Error("a var documented only by a directive should be skipped")
	}
}

// Go treats a comment separated from a declaration by a blank line as a
// free-floating note, not documentation. Attaching it would put unrelated prose
// in the symbol's embedding.
func TestGoSource_BlankLineBreaksDocAssociation(t *testing.T) {
	t.Parallel()
	src := "package note\n\n" +
		"// A stray remark about the file.\n\n" +
		"func Orphan() bool { return true }\n\n" +
		"// Another stray remark.\n\n" +
		"var loose = 1\n"
	got := GoSource(src)

	c := find(got, "go/func/Orphan")
	if c.Key == "" {
		t.Fatalf("no chunk for Orphan (keys: %v)", keysOf(got))
	}
	if strings.Contains(c.Text, "stray remark") {
		t.Errorf("a detached comment was attached as documentation: %q", c.Text)
	}
	if c := find(got, "go/var/loose"); c.Key != "" {
		t.Error("a var with only a detached comment should count as undocumented")
	}
}
