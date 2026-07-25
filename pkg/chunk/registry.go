// This file is the one place a file extension is mapped to a language. Both
// the indexer (which needs the chunker) and search (which needs the language
// name for `--lang`) read it, so the set of indexed extensions and the set of
// filterable languages cannot drift apart — a new language becomes searchable
// by name the moment it becomes indexable.
package chunk

import (
	"path/filepath"
	"slices"
	"sort"
	"strings"
)

// Language is one indexable language: the name a user types after `--lang`,
// and the chunker that splits its files.
type Language struct {
	Name  string
	Chunk Chunker
}

// byExtension maps a lowercased file extension to its language.
//
// The .ts family uses the plain TypeScript grammar (which reads `<T>` as a
// type parameter); the JSX-bearing and JavaScript extensions use the TSX
// grammar, a superset that also parses ordinary JS. Header files go to the C++
// chunker, whose grammar is a superset of C's — a `.h` is as likely to be C++
// as C, and misreading a class as a syntax error loses more than the reverse.
var byExtension = map[string]Language{
	".md":       {"markdown", Document},
	".markdown": {"markdown", Document},
	".go":       {"go", GoSource},
	".ts":       {"typescript", TypeScript},
	".mts":      {"typescript", TypeScript},
	".cts":      {"typescript", TypeScript},
	".tsx":      {"typescript", TSX},
	".js":       {"javascript", TSX},
	".jsx":      {"javascript", TSX},
	".mjs":      {"javascript", TSX},
	".cjs":      {"javascript", TSX},
	".py":       {"python", Python},
	".pyi":      {"python", Python},
	".java":     {"java", Java},
	".cs":       {"csharp", CSharp},
	".rs":       {"rust", Rust},
	".c":        {"c", C},
	".cc":       {"cpp", CPP},
	".cpp":      {"cpp", CPP},
	".cxx":      {"cpp", CPP},
	".h":        {"cpp", CPP},
	".hpp":      {"cpp", CPP},
	".hh":       {"cpp", CPP},
	".rb":       {"ruby", Ruby},
	".php":      {"php", PHP},
	".scala":    {"scala", Scala},
	".sc":       {"scala", Scala},
	".lua":      {"lua", Lua},
	".proto":    {"protobuf", Protobuf},
	".tf":       {"hcl", HCL},
	".tfvars":   {"hcl", HCL},
	".hcl":      {"hcl", HCL},
	".yaml":     {"yaml", YAML},
	".yml":      {"yaml", YAML},
	".sh":       {"bash", Bash},
	".bash":     {"bash", Bash},
}

// aliases accepts the names people actually type. `--lang c++` has to work
// because that is the language's name, even though it cannot be an extension.
var aliases = map[string]string{
	"c++":        "cpp",
	"cplusplus":  "cpp",
	"c#":         "csharp",
	"cs":         "csharp",
	"golang":     "go",
	"js":         "javascript",
	"ts":         "typescript",
	"tsx":        "typescript",
	"py":         "python",
	"rs":         "rust",
	"rb":         "ruby",
	"md":         "markdown",
	"proto":      "protobuf",
	"terraform":  "hcl",
	"tf":         "hcl",
	"yml":        "yaml",
	"sh":         "bash",
	"shell":      "bash",
	"kubernetes": "yaml",
	"k8s":        "yaml",
}

// LanguageFor returns the language for a path, and whether the path is
// indexable at all.
func LanguageFor(path string) (Language, bool) {
	l, ok := byExtension[strings.ToLower(filepath.Ext(path))]
	return l, ok
}

// LanguageName returns a path's language name, or "" when it is not indexable.
func LanguageName(path string) string {
	l, ok := LanguageFor(path)
	if !ok {
		return ""
	}
	return l.Name
}

// LanguageNames returns every filterable language name, sorted — the list
// `--lang` accepts and the help text prints.
func LanguageNames() []string {
	seen := map[string]bool{}
	var out []string
	for _, l := range byExtension {
		if !seen[l.Name] {
			seen[l.Name] = true
			out = append(out, l.Name)
		}
	}
	sort.Strings(out)
	return out
}

// NormalizeLanguage resolves an alias to a canonical language name and reports
// whether it names a language this build can index. A misspelled `--lang` must
// be an error rather than a filter that silently matches nothing.
func NormalizeLanguage(name string) (string, bool) {
	n := strings.ToLower(strings.TrimSpace(name))
	n = strings.TrimPrefix(n, ".")
	if canonical, ok := aliases[n]; ok {
		n = canonical
	}
	return n, slices.Contains(LanguageNames(), n)
}
