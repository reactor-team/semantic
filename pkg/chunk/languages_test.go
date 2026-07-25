package chunk

import (
	"strings"
	"testing"
)

// Each case pins the contract a language's chunker holds: which symbols it
// finds, how it keys them, and that the documentation reaches the embedded
// text. Node kinds were read off each grammar rather than guessed, so these
// tests are also what catches a grammar upgrade renaming one — the failure
// mode is a silently missing symbol, which nothing else would surface.
type langCase struct {
	name    string
	chunk   Chunker
	source  string
	want    []wantChunk
	absent  []string // keys that must not be emitted
	minimum int      // total chunks expected, 0 to skip the check
}

type wantChunk struct {
	key     string
	variant string
	inText  []string
}

func TestLanguages(t *testing.T) {
	t.Parallel()
	for _, tc := range languageCases() {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			got := tc.chunk(tc.source)
			if tc.minimum > 0 && len(got) < tc.minimum {
				t.Errorf("got %d chunks, want at least %d: %v", len(got), tc.minimum, keysOf(got))
			}
			for _, w := range tc.want {
				c := find(got, w.key)
				if c.Key == "" {
					t.Errorf("%s: not emitted (keys: %v)", w.key, keysOf(got))
					continue
				}
				if w.variant != "" && c.Variant != w.variant {
					t.Errorf("%s: variant = %q, want %q", w.key, c.Variant, w.variant)
				}
				if !strings.HasPrefix(c.Text, c.Heading) {
					t.Errorf("%s: text must lead with the breadcrumb %q, got %q", w.key, c.Heading, c.Text)
				}
				if c.Line < 1 {
					t.Errorf("%s: line = %d, want >= 1", w.key, c.Line)
				}
				for _, sub := range w.inText {
					if !strings.Contains(c.Text, sub) {
						t.Errorf("%s: text missing %q\n---\n%s", w.key, sub, c.Text)
					}
				}
			}
			for _, key := range tc.absent {
				if c := find(got, key); c.Key != "" {
					t.Errorf("%s: should not be emitted", key)
				}
			}
		})
	}
}

func languageCases() []langCase {
	return []langCase{{
		name:  "java",
		chunk: Java,
		source: `package com.x;
/** Serves things. */
public class Server implements Runnable {
  /** Start it. */
  public boolean start(int a) { return true; }
}
/** An enum. */
enum Kind { A, B }
`,
		want: []wantChunk{
			{"java/class/Server", VariantClass, []string{"public class Server implements Runnable", "Serves things."}},
			{"java/method/Server.start", VariantMethod, []string{"public boolean start(int a)", "Start it."}},
			{"java/enum/Kind", VariantEnum, []string{"enum Kind", "An enum."}},
		},
		// A class holding methods renders as its header; the body would repeat
		// the method chunks.
		absent: []string{"java/class/Server.start"},
	}, {
		name:  "rust",
		chunk: Rust,
		source: `/// Serves things.
pub struct Server { addr: String }
/// Does it.
pub fn start(a: u32) -> bool { true }
impl Server {
    /// Runs.
    pub fn run(&self) {}
}
/// A trait.
pub trait Go { fn go(&self); }
`,
		want: []wantChunk{
			// A struct with no methods keeps its fields — they are the signal.
			{"rs/struct/Server", VariantStruct, []string{"pub struct Server { addr: String }", "Serves things."}},
			{"rs/func/start", VariantFunc, []string{"pub fn start(a: u32) -> bool", "Does it."}},
			// An impl block names no new symbol; it qualifies the methods it holds.
			{"rs/func/Server.run", VariantFunc, []string{"pub fn run(&self)", "Runs."}},
			{"rs/trait/Go", VariantTrait, []string{"pub trait Go", "A trait."}},
		},
		absent: []string{"rs/struct/Server.addr"},
	}, {
		name:  "cpp",
		chunk: CPP,
		source: `// A class.
class Server {
public:
  // Start it.
  bool start(int a);
};
// A function.
int ping(int b) { return b; }
struct P { int x; };
`,
		want: []wantChunk{
			{"cpp/class/Server", VariantClass, []string{"class Server", "A class."}},
			{"cpp/method/Server.start", VariantMethod, []string{"bool start(int a)", "Start it."}},
			{"cpp/func/ping", VariantFunc, []string{"int ping(int b)", "A function."}},
			// A data-only struct keeps its fields. Telling it from a class with
			// methods needs the name resolver, not the node kind: C++ spells a
			// data member and a method with the same node.
			{"cpp/struct/P", VariantStruct, []string{"int x"}},
		},
	}, {
		name:  "c",
		chunk: C,
		source: `// A function.
int ping(int b) { return b; }
// A struct.
struct P { int x; };
`,
		want: []wantChunk{
			{"c/func/ping", VariantFunc, []string{"int ping(int b)", "A function."}},
			{"c/struct/P", VariantStruct, []string{"int x", "A struct."}},
		},
	}, {
		name:  "ruby",
		chunk: Ruby,
		source: `# Serves things.
class Server
  # Start it.
  def start(a)
    true
  end
end
`,
		want: []wantChunk{
			{"rb/class/Server", VariantClass, []string{"class Server", "Serves things."}},
			{"rb/method/Server.start", VariantMethod, []string{"def start(a)", "Start it."}},
		},
	}, {
		name:  "php",
		chunk: PHP,
		source: `<?php
/** Serves things. */
class Server {
  /** Start it. */
  public function start(int $a): bool { return true; }
}
/** Pings. */
function ping(): string { return "x"; }
`,
		want: []wantChunk{
			{"php/class/Server", VariantClass, []string{"class Server", "Serves things."}},
			{"php/method/Server.start", VariantMethod, []string{"public function start(int $a): bool", "Start it."}},
			{"php/func/ping", VariantFunc, []string{"function ping(): string", "Pings."}},
		},
	}, {
		name:  "scala",
		chunk: Scala,
		source: `/** Serves things. */
class Server(addr: String) {
  /** Start it. */
  def start(a: Int): Boolean = true
}
/** An object. */
object Util { def ping: String = "x" }
`,
		want: []wantChunk{
			{"scala/class/Server", VariantClass, []string{"class Server(addr: String)", "Serves things."}},
			{"scala/func/Server.start", VariantFunc, []string{"def start(a: Int): Boolean", "Start it."}},
			{"scala/object/Util", VariantModule, []string{"object Util", "An object."}},
		},
	}, {
		name:  "csharp",
		chunk: CSharp,
		source: `/// <summary>Serves things.</summary>
public class Server {
  /// <summary>Start.</summary>
  public bool Start(int a) { return true; }
}
public interface IThing { void Go(); }
public enum Kind { A }
`,
		want: []wantChunk{
			{"cs/class/Server", VariantClass, []string{"public class Server", "Serves things."}},
			{"cs/method/Server.Start", VariantMethod, []string{"public bool Start(int a)", "Start."}},
			{"cs/interface/IThing", VariantInterface, []string{"public interface IThing"}},
			{"cs/enum/Kind", VariantEnum, []string{"enum Kind"}},
		},
	}, {
		name:  "lua",
		chunk: Lua,
		source: `-- Serves things.
function Server.start(a)
  return true
end
-- A plain one.
local function ping() end
`,
		want: []wantChunk{
			// A dotted path is the name as written; reducing it to "start"
			// would collide with every other module's start.
			{"lua/func/Server.start", VariantFunc, []string{"function Server.start(a)", "Serves things."}},
			{"lua/func/ping", VariantFunc, []string{"A plain one."}},
		},
	}}
}

// A Ruby class writes its first inner comment before its body field, so
// slicing the signature on the field alone pulls a method's documentation into
// the class. Guard the fix.
func TestRuby_ClassSignatureExcludesInnerComment(t *testing.T) {
	t.Parallel()
	got := Ruby("# Serves things.\nclass Server\n  # Start it.\n  def start(a)\n    true\n  end\nend\n")
	c := find(got, "rb/class/Server")
	if c.Key == "" {
		t.Fatalf("no class chunk (keys: %v)", keysOf(got))
	}
	if strings.Contains(c.Text, "Start it.") {
		t.Errorf("a method's comment leaked into the class signature:\n%s", c.Text)
	}
}

// Scala leaves a trailing `=` when a body is sliced off. It carries no meaning
// into an embedding and makes two renderings of one signature differ.
func TestScala_SignatureHasNoDanglingEquals(t *testing.T) {
	t.Parallel()
	got := Scala("/** Doc. */\ndef ping: String = \"x\"\n")
	for _, c := range got {
		if strings.Contains(c.Text, "String =") {
			t.Errorf("dangling '=' left in signature: %q", c.Text)
		}
	}
}

// Rust treats `///` and `//!` as documentation and a plain `//` as an
// implementation note. Its own tooling draws that line, so this does too.
func TestRust_PlainCommentIsNotDocumentation(t *testing.T) {
	t.Parallel()
	got := Rust("// Just a note to myself.\npub fn start() {}\n")
	c := find(got, "rs/func/start")
	if c.Key == "" {
		t.Fatalf("no chunk for start (keys: %v)", keysOf(got))
	}
	if strings.Contains(c.Text, "note to myself") {
		t.Errorf("a plain // comment was treated as documentation: %q", c.Text)
	}
}

// Every chunker must survive input that is not its language without panicking
// and without inventing symbols. Files get misnamed, and a crash in the
// indexer would take the whole run down.
func TestLanguages_GarbageInputIsSafe(t *testing.T) {
	t.Parallel()
	garbage := []string{"", "   \n\n", "\x00\x01\x02", "}{)(][", strings.Repeat("nope ", 500)}
	chunkers := map[string]Chunker{
		"java": Java, "rust": Rust, "c": C, "cpp": CPP, "ruby": Ruby,
		"php": PHP, "scala": Scala, "python": Python, "protobuf": Protobuf,
		"hcl": HCL, "bash": Bash, "yaml": YAML, "go": GoSource,
	}
	for name, fn := range chunkers {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			for _, src := range garbage {
				for _, c := range fn(src) {
					if c.Key == "" {
						t.Errorf("emitted a chunk with an empty key for %q", src)
					}
				}
			}
		})
	}
}

// Two symbols in one file must never share a key: the store enforces
// uniqueness, so a collision drops a symbol instead of reporting one.
func TestLanguages_KeysAreUniqueWithinAFile(t *testing.T) {
	t.Parallel()
	cases := map[string]struct {
		fn  Chunker
		src string
	}{
		// C++ overloads share a name.
		"cpp": {CPP, "int ping(int a) { return a; }\nint ping(char b) { return 0; }\n"},
		// Two HCL blocks of the same type and label.
		"hcl": {HCL, "module \"vpc\" {\n a = 1\n}\nmodule \"vpc\" {\n b = 2\n}\n"},
		// Two YAML documents with no identifying kind or name.
		"yaml": {YAML, "a:\n  b: 1\n---\nc:\n  d: 2\n"},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			seen := map[string]bool{}
			for _, c := range tc.fn(tc.src) {
				if seen[c.Key] {
					t.Errorf("duplicate key %q", c.Key)
				}
				seen[c.Key] = true
			}
		})
	}
}
