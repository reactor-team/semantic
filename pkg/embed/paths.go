package embed

import (
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strings"
)

// Check reports whether the ONNX runtime library and model files are all
// present on disk, without initializing the runtime. Returns nil when
// embedding is ready, or an error naming the first missing piece — suitable
// for `semantic status`. Get performs the same checks lazily.
func Check() error {
	if findOrtLib() == "" {
		return fmt.Errorf("ONNX Runtime library not found — run: semantic init")
	}
	modelDir := ModelCacheDir()
	if !fileExists(filepath.Join(modelDir, "model.onnx")) {
		return fmt.Errorf("embedding model not found — run: semantic init")
	}
	if !fileExists(filepath.Join(modelDir, "tokenizer.json")) {
		return fmt.Errorf("tokenizer not found — run: semantic init")
	}
	return nil
}

// cacheRoot is the home for regeneratable artifacts — the ONNX Runtime
// shared library and the embedding model files. On a fresh machine these
// can always be re-downloaded, so they belong under the OS-conventional
// cache root:
//
//	darwin  → ~/Library/Caches/semantic
//	linux   → $XDG_CACHE_HOME/semantic (or ~/.cache/semantic)
//	windows → %LocalAppData%\semantic
//
// $SEMANTIC_CACHE_DIR overrides everything (used by tests and by users who
// want a non-default location). Returns "" only when the cache directory
// can't be resolved (extremely rare — missing home directory).
func cacheRoot() string {
	if v := strings.TrimSpace(os.Getenv("SEMANTIC_CACHE_DIR")); v != "" {
		return v
	}
	base, err := os.UserCacheDir()
	if err != nil || base == "" {
		return ""
	}
	return filepath.Join(base, "semantic")
}

// ModelCacheDir returns the directory where model files are cached.
// $SEMANTIC_MODEL_DIR overrides.
func ModelCacheDir() string {
	if v := strings.TrimSpace(os.Getenv("SEMANTIC_MODEL_DIR")); v != "" {
		return v
	}
	return filepath.Join(cacheRoot(), "models", "all-minilm-l6-v2")
}

// OrtCacheDir returns the directory where the ONNX Runtime library is cached.
//
// The version is part of the path. The binding is compiled against one release
// of the C API and will not load a library from another, so a single
// unversioned path would make an upgrade find the old file already present,
// skip the download, and fail at the first embed with "Error setting ORT API
// base". Keying by version makes the upgrade fetch what it needs and leaves
// the superseded library sitting harmlessly beside it.
func OrtCacheDir() string {
	return filepath.Join(cacheRoot(), "onnxruntime", OrtVersion)
}

// OrtLibFilename returns the platform-appropriate shared library filename.
func OrtLibFilename() string {
	switch runtime.GOOS {
	case "darwin":
		return "libonnxruntime.dylib"
	case "windows":
		return "onnxruntime.dll"
	default:
		return "libonnxruntime.so"
	}
}

// findOrtLib returns the first existing path from the candidate list.
// $SEMANTIC_ORT_LIB is an explicit override checked first.
func findOrtLib() string {
	if v := strings.TrimSpace(os.Getenv("SEMANTIC_ORT_LIB")); v != "" {
		if fileExists(v) {
			return v
		}
	}
	candidates := []string{
		filepath.Join(OrtCacheDir(), OrtLibFilename()), // auto-downloaded
	}
	switch runtime.GOOS {
	case "darwin":
		candidates = append(candidates,
			"/opt/homebrew/lib/libonnxruntime.dylib", // Homebrew ARM64
			"/usr/local/lib/libonnxruntime.dylib",    // Homebrew x86_64
		)
	case "windows":
		// No standard system location; the cache dir is the only place we
		// look. A user wanting a system-wide install can drop
		// onnxruntime.dll alongside semantic.exe — LoadLibrary finds it.
	default:
		candidates = append(candidates,
			"/usr/lib/libonnxruntime.so",                  // Linux system
			"/usr/lib/x86_64-linux-gnu/libonnxruntime.so", // Debian/Ubuntu
			"/usr/local/lib/libonnxruntime.so",            // Linux local
		)
	}
	for _, p := range candidates {
		if fileExists(p) {
			return p
		}
	}
	return ""
}

func fileExists(p string) bool {
	_, err := os.Stat(p) //nolint:gosec // G703: p is a hardcoded candidate path or $SEMANTIC_ORT_LIB (trusted local env), not user input
	return err == nil
}
