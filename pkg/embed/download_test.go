// Copyright (c) 2026 Reactor Technologies, Inc.
// SPDX-License-Identifier: Apache-2.0

// These tests never reach the network. Archives are synthesized in memory and
// downloads are served by httptest, so the suite runs offline and does not
// depend on a GitHub release or a Hugging Face repository staying put.
package embed

import (
	"archive/tar"
	"archive/zip"
	"bytes"
	"compress/gzip"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

// tarEntry is one file in a synthesized archive.
type tarEntry struct {
	name string
	body string
	typ  byte
}

// makeTgz builds a gzipped tar holding the given entries.
func makeTgz(t *testing.T, entries ...tarEntry) []byte {
	t.Helper()
	var buf bytes.Buffer
	gz := gzip.NewWriter(&buf)
	tw := tar.NewWriter(gz)
	for _, e := range entries {
		typ := e.typ
		if typ == 0 {
			typ = tar.TypeReg
		}
		hdr := &tar.Header{Name: e.name, Mode: 0o644, Size: int64(len(e.body)), Typeflag: typ}
		if typ == tar.TypeSymlink {
			hdr.Size, hdr.Linkname = 0, "elsewhere"
		}
		if err := tw.WriteHeader(hdr); err != nil {
			t.Fatal(err)
		}
		if typ == tar.TypeReg {
			if _, err := tw.Write([]byte(e.body)); err != nil {
				t.Fatal(err)
			}
		}
	}
	if err := tw.Close(); err != nil {
		t.Fatal(err)
	}
	if err := gz.Close(); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}

// makeZip builds a zip archive holding the given name/body pairs.
func makeZip(t *testing.T, files map[string]string) []byte {
	t.Helper()
	var buf bytes.Buffer
	zw := zip.NewWriter(&buf)
	for name, body := range files {
		w, err := zw.Create(name)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := w.Write([]byte(body)); err != nil {
			t.Fatal(err)
		}
	}
	if err := zw.Close(); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}

// readFile returns a file's contents, failing the test if it is unreadable.
func readFile(t *testing.T, path string) string {
	t.Helper()
	b, err := os.ReadFile(path) //nolint:gosec // G304: path is a t.TempDir()-derived test path
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

// A real ORT tarball ships the library under a versioned name, beside a
// symlink and the directories that hold them. Only the regular library file
// is extracted: a symlink would be copied as its target's bytes at best, and
// as nothing at worst.
func TestExtractOrtLib(t *testing.T) {
	t.Parallel()
	archive := makeTgz(t,
		tarEntry{name: "onnxruntime-osx-arm64-1.24.1/", typ: tar.TypeDir},
		tarEntry{name: "onnxruntime-osx-arm64-1.24.1/lib/libonnxruntime.dylib", typ: tar.TypeSymlink},
		tarEntry{name: "onnxruntime-osx-arm64-1.24.1/README.md", body: "not the library"},
		tarEntry{name: "onnxruntime-osx-arm64-1.24.1/lib/libonnxruntime.1.24.1.dylib", body: "ELF-ish bytes"},
	)

	dest := filepath.Join(t.TempDir(), "libonnxruntime.dylib")
	if err := extractOrtLib(bytes.NewReader(archive), dest); err != nil {
		t.Fatalf("extractOrtLib: %v", err)
	}
	if got := readFile(t, dest); got != "ELF-ish bytes" {
		t.Errorf("extracted %q, want the library body", got)
	}

	// The extracted file is owner-only. It lands in a cache directory that
	// later gets dlopen'd, so it should not be group- or world-writable.
	info, err := os.Stat(dest)
	if err != nil {
		t.Fatal(err)
	}
	if perm := info.Mode().Perm(); perm != 0o600 {
		t.Errorf("extracted library mode = %v, want 0600", perm)
	}
}

// Linux names the library libonnxruntime.so.1.24.1 — the version trails the
// extension, so a suffix match would miss it.
func TestExtractOrtLib_LinuxNaming(t *testing.T) {
	t.Parallel()
	archive := makeTgz(t, tarEntry{name: "onnxruntime-linux-x64-1.24.1/lib/libonnxruntime.so.1.24.1", body: "so bytes"})
	dest := filepath.Join(t.TempDir(), "libonnxruntime.so")
	if err := extractOrtLib(bytes.NewReader(archive), dest); err != nil {
		t.Fatalf("extractOrtLib: %v", err)
	}
	if got := readFile(t, dest); got != "so bytes" {
		t.Errorf("extracted %q, want the library body", got)
	}
}

// An archive with no library is a changed release layout, not a corrupt
// download, and the error says so — a silent success would leave a missing
// file to be discovered much later, at the first embed.
func TestExtractOrtLib_NoLibrary(t *testing.T) {
	t.Parallel()
	cases := map[string][]tarEntry{
		"nothing that matches": {{name: "onnxruntime-osx-arm64-1.24.1/README.md", body: "docs"}},
		// A name that starts right but has no library extension: the headers
		// ship as libonnxruntime*.h and must not be mistaken for the library.
		"header file": {{name: "include/libonnxruntime_c_api.h", body: "typedef struct"}},
	}
	for name, entries := range cases {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			dest := filepath.Join(t.TempDir(), "lib")
			err := extractOrtLib(bytes.NewReader(makeTgz(t, entries...)), dest)
			if err == nil || !strings.Contains(err.Error(), "no libonnxruntime library found") {
				t.Fatalf("extractOrtLib = %v, want a missing-library error", err)
			}
			if fileExists(dest) {
				t.Error("a failed extraction left a file behind")
			}
		})
	}
}

// A stream that is not gzip fails at the first read rather than being parsed
// as a tar of garbage.
func TestExtractOrtLib_NotGzip(t *testing.T) {
	t.Parallel()
	err := extractOrtLib(strings.NewReader("<html>404</html>"), filepath.Join(t.TempDir(), "lib"))
	if err == nil || !strings.Contains(err.Error(), "gzip") {
		t.Fatalf("extractOrtLib = %v, want a gzip error", err)
	}
}

func TestExtractOrtLibFromZip(t *testing.T) {
	t.Parallel()
	archive := makeZip(t, map[string]string{
		"onnxruntime-win-x64-1.24.1/lib/onnxruntime.dll": "dll bytes",
		"onnxruntime-win-x64-1.24.1/README.md":           "docs",
	})
	dest := filepath.Join(t.TempDir(), "onnxruntime.dll")
	if err := extractOrtLibFromZip(bytes.NewReader(archive), dest); err != nil {
		t.Fatalf("extractOrtLibFromZip: %v", err)
	}
	if got := readFile(t, dest); got != "dll bytes" {
		t.Errorf("extracted %q, want the library body", got)
	}

	// The spool file the zip reader needs is temporary and must not survive.
	left, err := filepath.Glob(filepath.Join(filepath.Dir(dest), ".ort-zip-*"))
	if err != nil {
		t.Fatal(err)
	}
	if len(left) > 0 {
		t.Errorf("zip spool files left behind: %v", left)
	}
}

func TestExtractOrtLibFromZip_NoLibrary(t *testing.T) {
	t.Parallel()
	archive := makeZip(t, map[string]string{"onnxruntime-win-x64-1.24.1/README.md": "docs"})
	err := extractOrtLibFromZip(bytes.NewReader(archive), filepath.Join(t.TempDir(), "dll"))
	if err == nil || !strings.Contains(err.Error(), "onnxruntime.dll not found") {
		t.Fatalf("extractOrtLibFromZip = %v, want a missing-dll error", err)
	}
}

// The size cap bounds what a corrupt or hostile archive entry can write. The
// limit is a parameter so this runs on a few bytes rather than the 500 MB the
// callers pass.
func TestWriteCappedLib_RefusesOversizeEntry(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	dest := filepath.Join(dir, "lib")

	err := writeCappedLib(strings.NewReader("0123456789"), dest, 4)
	if err == nil || !strings.Contains(err.Error(), "exceeds 4 bytes") {
		t.Fatalf("writeCappedLib = %v, want a size-cap error", err)
	}
	if fileExists(dest) {
		t.Error("an over-cap entry was written to the destination anyway")
	}
	// The partial write is cleaned up, so a refused download does not leave
	// the cache directory filling with temp files.
	left, err := filepath.Glob(filepath.Join(dir, ".ort-download-*"))
	if err != nil {
		t.Fatal(err)
	}
	if len(left) > 0 {
		t.Errorf("temp files left behind: %v", left)
	}
}

// An entry exactly at the cap is allowed: the limit is a maximum, not a
// threshold to stay under.
func TestWriteCappedLib_AllowsExactlyTheCap(t *testing.T) {
	t.Parallel()
	dest := filepath.Join(t.TempDir(), "lib")
	if err := writeCappedLib(strings.NewReader("1234"), dest, 4); err != nil {
		t.Fatalf("writeCappedLib at exactly the cap = %v, want nil", err)
	}
	if got := readFile(t, dest); got != "1234" {
		t.Errorf("wrote %q, want %q", got, "1234")
	}
}

func TestDownloadFile(t *testing.T) {
	t.Parallel()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		fmt.Fprint(w, "model bytes")
	}))
	defer srv.Close()

	dir := t.TempDir()
	dest := filepath.Join(dir, "model.onnx")
	if err := downloadFile(srv.URL, dest); err != nil {
		t.Fatalf("downloadFile: %v", err)
	}
	if got := readFile(t, dest); got != "model bytes" {
		t.Errorf("downloaded %q, want %q", got, "model bytes")
	}
	// The download lands by rename, so a reader either sees the whole file or
	// no file — never a truncated model that would fail much later inside ORT.
	left, err := filepath.Glob(filepath.Join(dir, ".download-*"))
	if err != nil {
		t.Fatal(err)
	}
	if len(left) > 0 {
		t.Errorf("temp files left behind: %v", left)
	}
}

// A non-200 is an error rather than a file full of an error page. Hugging Face
// answers a moved asset with a 404 body that would otherwise be saved as
// model.onnx and fail unreadably at load time.
func TestDownloadFile_HTTPError(t *testing.T) {
	t.Parallel()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		http.Error(w, "gone", http.StatusNotFound)
	}))
	defer srv.Close()

	dest := filepath.Join(t.TempDir(), "model.onnx")
	err := downloadFile(srv.URL, dest)
	if err == nil || !strings.Contains(err.Error(), "HTTP 404") {
		t.Fatalf("downloadFile = %v, want an HTTP 404 error", err)
	}
	if fileExists(dest) {
		t.Error("a failed download left a file behind")
	}
}

// DownloadOrt and DownloadModel are idempotent: with everything cached they
// report what they found and make no request. `semantic init` is documented as
// safe to re-run, and a second run must not re-fetch 90 MB.
func TestDownloadAll_AlreadyCached(t *testing.T) {
	root := t.TempDir()
	t.Setenv("SEMANTIC_CACHE_DIR", root)
	t.Setenv("SEMANTIC_MODEL_DIR", "")

	touch(t, filepath.Join(OrtCacheDir(), OrtLibFilename()))
	touch(t, filepath.Join(ModelCacheDir(), "model.onnx"))
	touch(t, filepath.Join(ModelCacheDir(), "tokenizer.json"))

	var log strings.Builder
	if err := DownloadAll(func(format string, a ...any) {
		fmt.Fprintf(&log, format+"\n", a...)
	}); err != nil {
		t.Fatalf("DownloadAll: %v", err)
	}

	// Three cached artifacts, three "already cached" lines and no download.
	if got := strings.Count(log.String(), "already cached"); got != 3 {
		t.Errorf("reported %d cached artifacts, want 3\n%s", got, log.String())
	}
	if strings.Contains(log.String(), "Downloading") {
		t.Errorf("a cached install still tried to download:\n%s", log.String())
	}
}

// A failure says which of the two artifacts it was, because the two come from
// different hosts and the distinction is the first thing to check.
func TestDownloadAll_NamesTheFailingArtifact(t *testing.T) {
	root := t.TempDir()
	// A regular file where the cache directory belongs: MkdirAll fails, and
	// the wrapper's prefix is what identifies the stage.
	touch(t, filepath.Join(root, "blocked"))
	t.Setenv("SEMANTIC_CACHE_DIR", filepath.Join(root, "blocked"))
	t.Setenv("SEMANTIC_MODEL_DIR", "")

	err := DownloadAll(func(string, ...any) {})
	if err == nil || !strings.HasPrefix(err.Error(), "ONNX Runtime:") {
		t.Fatalf("DownloadAll = %v, want an error naming the ONNX Runtime stage", err)
	}
}

// The release URL is assembled per platform. This pins the shape rather than
// the exact string, so a version bump stays a one-line change in download.go.
func TestOrtDownloadURL(t *testing.T) {
	t.Parallel()
	url := OrtDownloadURL()
	if !strings.HasPrefix(url, "https://github.com/microsoft/onnxruntime/releases/download/v"+OrtVersion+"/") {
		t.Errorf("OrtDownloadURL() = %q, want a microsoft/onnxruntime release for v%s", url, OrtVersion)
	}
	if !strings.Contains(url, OrtVersion) {
		t.Errorf("OrtDownloadURL() = %q, missing the version", url)
	}

	// Windows ships a zip and every other platform a tarball, which is the
	// branch extractOrtLibFromZip exists for.
	wantExt := ".tgz"
	if runtime.GOOS == "windows" {
		wantExt = ".zip"
	}
	if !strings.HasSuffix(url, wantExt) {
		t.Errorf("OrtDownloadURL() = %q, want a %s archive on %s", url, wantExt, runtime.GOOS)
	}
}

// httpGet closes the body on a non-200 so a failed init leaks no connection.
func TestHTTPGet_ClosesBodyOnError(t *testing.T) {
	t.Parallel()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		http.Error(w, "nope", http.StatusInternalServerError)
	}))
	defer srv.Close()

	resp, err := httpGet(srv.URL)
	if err == nil {
		resp.Body.Close()
		t.Fatal("httpGet returned no error for a 500")
	}
	if resp != nil {
		t.Error("httpGet returned both a response and an error")
	}
	if !strings.Contains(err.Error(), "HTTP 500") {
		t.Errorf("httpGet error = %q, want it to name the status", err)
	}
}

func TestHTTPGet_Success(t *testing.T) {
	t.Parallel()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		fmt.Fprint(w, "ok")
	}))
	defer srv.Close()

	resp, err := httpGet(srv.URL)
	if err != nil {
		t.Fatalf("httpGet: %v", err)
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatal(err)
	}
	if string(body) != "ok" {
		t.Errorf("body = %q, want %q", body, "ok")
	}
}
