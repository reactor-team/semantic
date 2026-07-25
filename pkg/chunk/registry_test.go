package chunk

import (
	"slices"
	"testing"
)

func TestLanguageFor(t *testing.T) {
	t.Parallel()
	cases := map[string]string{
		"pkg/x.go":           "go",
		"README.md":          "markdown",
		"web/app.tsx":        "typescript",
		"web/app.js":         "javascript",
		"api/run.py":         "python",
		"infra/main.tf":      "hcl",
		"k8s/deploy.yaml":    "yaml",
		"tasks/build.sh":     "bash",
		"proto/api.proto":    "protobuf",
		"src/Main.java":      "java",
		"src/Program.cs":     "csharp",
		"src/lib.rs":         "rust",
		"src/util.hpp":       "cpp",
		"src/util.c":         "c",
		"UPPERCASE.GO":       "go", // extension matching is case-insensitive
		"notes.txt":          "",   // not indexable
		"Makefile":           "",   // no extension
		"archive.tar.gz":     "",
		"weird.name.with.md": "markdown",
	}
	for path, want := range cases {
		if got := LanguageName(path); got != want {
			t.Errorf("LanguageName(%q) = %q, want %q", path, got, want)
		}
	}
}

// Every extension the indexer handles must resolve to a name --lang accepts,
// or a file could be indexed but impossible to filter for.
func TestEveryExtensionHasAFilterableName(t *testing.T) {
	t.Parallel()
	names := LanguageNames()
	for ext, lang := range byExtension {
		if lang.Name == "" {
			t.Errorf("%s has no language name", ext)
		}
		if lang.Chunk == nil {
			t.Errorf("%s (%s) has no chunker", ext, lang.Name)
		}
		if !slices.Contains(names, lang.Name) {
			t.Errorf("%s → %q is missing from LanguageNames()", ext, lang.Name)
		}
	}
}

func TestNormalizeLanguage(t *testing.T) {
	t.Parallel()
	valid := map[string]string{
		"go":         "go",
		"Go":         "go",
		"  golang  ": "go",
		".py":        "python", // a leading dot is what people type when thinking in extensions
		"c++":        "cpp",
		"C#":         "csharp",
		"terraform":  "hcl",
		"k8s":        "yaml",
		"shell":      "bash",
		"TS":         "typescript",
	}
	for in, want := range valid {
		got, ok := NormalizeLanguage(in)
		if !ok {
			t.Errorf("NormalizeLanguage(%q) rejected a valid name", in)
			continue
		}
		if got != want {
			t.Errorf("NormalizeLanguage(%q) = %q, want %q", in, got, want)
		}
	}

	// A misspelling must be rejected. Accepting it would produce a filter that
	// matches nothing, and an empty result reads as "no such code" rather than
	// "no such language".
	for _, bad := range []string{"pyhton", "cobol", "", "  ", "gopher"} {
		if _, ok := NormalizeLanguage(bad); ok {
			t.Errorf("NormalizeLanguage(%q) accepted an unknown name", bad)
		}
	}
}

// Every alias must resolve to a real language. An alias pointing at a language
// that was renamed or dropped would be accepted and then match nothing.
func TestAliasesResolveToRealLanguages(t *testing.T) {
	t.Parallel()
	names := LanguageNames()
	for alias, target := range aliases {
		if !slices.Contains(names, target) {
			t.Errorf("alias %q points at %q, which is not a language", alias, target)
		}
		if got, ok := NormalizeLanguage(alias); !ok || got != target {
			t.Errorf("NormalizeLanguage(%q) = %q, %v; want %q, true", alias, got, ok, target)
		}
	}
}
