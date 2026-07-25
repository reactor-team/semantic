// Copyright (c) 2026 Reactor Technologies, Inc.
// SPDX-License-Identifier: Apache-2.0

package embed

import (
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

// touch creates an empty file, making any missing parent directories.
func touch(t *testing.T, path string) string {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o750); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

// These tests set environment variables, so none of them call t.Parallel: the
// process environment is shared, and a parallel sibling would read another
// test's override.

func TestCacheRoot_EnvOverride(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("SEMANTIC_CACHE_DIR", dir)
	if got := cacheRoot(); got != dir {
		t.Errorf("cacheRoot() = %q, want %q", got, dir)
	}

	// Surrounding whitespace is trimmed, and a value that is only whitespace
	// counts as unset — a shell that exports an empty variable must not
	// redirect the cache to the current directory.
	t.Setenv("SEMANTIC_CACHE_DIR", "   ")
	if got := cacheRoot(); got == "" || got == "   " {
		t.Errorf("cacheRoot() = %q, want the OS default", got)
	}
}

// The default sits under the OS cache root, one directory per artifact class,
// so a user can delete either without losing the other.
func TestCacheRoot_Default(t *testing.T) {
	t.Setenv("SEMANTIC_CACHE_DIR", "")
	base, err := os.UserCacheDir()
	if err != nil {
		t.Skipf("no OS cache dir on this machine: %v", err)
	}
	if got, want := cacheRoot(), filepath.Join(base, "semantic"); got != want {
		t.Errorf("cacheRoot() = %q, want %q", got, want)
	}
}

func TestModelCacheDir(t *testing.T) {
	root := t.TempDir()
	t.Setenv("SEMANTIC_CACHE_DIR", root)
	t.Setenv("SEMANTIC_MODEL_DIR", "")

	want := filepath.Join(root, "models", "all-minilm-l6-v2")
	if got := ModelCacheDir(); got != want {
		t.Errorf("ModelCacheDir() = %q, want %q", got, want)
	}
	if got, want := OrtCacheDir(), filepath.Join(root, "onnxruntime"); got != want {
		t.Errorf("OrtCacheDir() = %q, want %q", got, want)
	}

	// $SEMANTIC_MODEL_DIR is the narrower override and wins over the cache
	// root, so a user can point at a model they already have without moving
	// the ONNX Runtime library too.
	explicit := t.TempDir()
	t.Setenv("SEMANTIC_MODEL_DIR", explicit)
	if got := ModelCacheDir(); got != explicit {
		t.Errorf("ModelCacheDir() = %q, want %q", got, explicit)
	}
	if got, want := OrtCacheDir(), filepath.Join(root, "onnxruntime"); got != want {
		t.Errorf("SEMANTIC_MODEL_DIR moved the ORT dir too: %q, want %q", got, want)
	}
}

func TestOrtLibFilename(t *testing.T) {
	t.Parallel()
	got := OrtLibFilename()
	var want string
	switch runtime.GOOS {
	case "darwin":
		want = "libonnxruntime.dylib"
	case "windows":
		want = "onnxruntime.dll"
	default:
		want = "libonnxruntime.so"
	}
	if got != want {
		t.Errorf("OrtLibFilename() on %s = %q, want %q", runtime.GOOS, got, want)
	}
}

// The cached library is the first candidate, ahead of every system path, so a
// downloaded runtime wins over whatever the machine happens to have installed.
func TestFindOrtLib_PrefersCache(t *testing.T) {
	root := t.TempDir()
	t.Setenv("SEMANTIC_CACHE_DIR", root)
	t.Setenv("SEMANTIC_ORT_LIB", "")

	lib := touch(t, filepath.Join(root, "onnxruntime", OrtLibFilename()))
	if got := findOrtLib(); got != lib {
		t.Errorf("findOrtLib() = %q, want the cached library %q", got, lib)
	}
}

func TestFindOrtLib_ExplicitOverride(t *testing.T) {
	root := t.TempDir()
	t.Setenv("SEMANTIC_CACHE_DIR", root)

	cached := touch(t, filepath.Join(root, "onnxruntime", OrtLibFilename()))
	override := touch(t, filepath.Join(t.TempDir(), "custom-onnxruntime"))
	t.Setenv("SEMANTIC_ORT_LIB", override)
	if got := findOrtLib(); got != override {
		t.Errorf("findOrtLib() = %q, want the override %q", got, override)
	}

	// An override naming a file that isn't there falls through to the normal
	// candidates rather than failing. A stale variable in a shell profile
	// should not take embedding down when a usable library is present.
	t.Setenv("SEMANTIC_ORT_LIB", filepath.Join(t.TempDir(), "missing.dylib"))
	if got := findOrtLib(); got != cached {
		t.Errorf("findOrtLib() = %q, want the fallback %q", got, cached)
	}
}

// Check reports the first missing piece, in install order, and every message
// names the command that fixes it.
func TestCheck_ReportsFirstMissingPiece(t *testing.T) {
	root := t.TempDir()
	t.Setenv("SEMANTIC_CACHE_DIR", root)
	t.Setenv("SEMANTIC_MODEL_DIR", "")
	// Pin the runtime to a file this test owns. Without it the result would
	// depend on whether the machine happens to have onnxruntime installed
	// system-wide, and the model checks below would never be reached there.
	t.Setenv("SEMANTIC_ORT_LIB", touch(t, filepath.Join(root, "libonnxruntime.dylib")))

	modelDir := ModelCacheDir()
	if err := os.MkdirAll(modelDir, 0o750); err != nil {
		t.Fatal(err)
	}

	err := Check()
	if err == nil || !strings.Contains(err.Error(), "embedding model not found") {
		t.Fatalf("Check() with no model = %v, want the model to be reported missing", err)
	}
	if !strings.Contains(err.Error(), "semantic init") {
		t.Errorf("Check() = %q, want the message to name the fix", err)
	}

	// With the model present the tokenizer becomes the first gap. A model
	// without its tokenizer produces no vectors at all, so it is checked
	// separately rather than assumed to arrive with the weights.
	touch(t, filepath.Join(modelDir, "model.onnx"))
	err = Check()
	if err == nil || !strings.Contains(err.Error(), "tokenizer not found") {
		t.Fatalf("Check() with no tokenizer = %v, want the tokenizer to be reported missing", err)
	}

	touch(t, filepath.Join(modelDir, "tokenizer.json"))
	if err := Check(); err != nil {
		t.Errorf("Check() with every file present = %v, want nil", err)
	}
}

// Check looks for the runtime before the model, because a machine missing both
// should be told about the download that has to happen first.
func TestCheck_ReportsMissingRuntimeFirst(t *testing.T) {
	root := t.TempDir()
	t.Setenv("SEMANTIC_CACHE_DIR", root)
	t.Setenv("SEMANTIC_MODEL_DIR", "")
	t.Setenv("SEMANTIC_ORT_LIB", "")
	if findOrtLib() != "" {
		t.Skip("onnxruntime is installed system-wide on this machine")
	}
	err := Check()
	if err == nil || !strings.Contains(err.Error(), "ONNX Runtime library not found") {
		t.Fatalf("Check() with nothing installed = %v, want the runtime to be reported missing", err)
	}
}

func TestFileExists(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	if fileExists(filepath.Join(dir, "nope")) {
		t.Error("fileExists reported a missing file as present")
	}
	if !fileExists(touch(t, filepath.Join(dir, "yes"))) {
		t.Error("fileExists reported an existing file as missing")
	}
}
