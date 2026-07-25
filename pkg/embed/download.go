// Copyright (c) 2026 Reactor Technologies, Inc.
// SPDX-License-Identifier: Apache-2.0

package embed

import (
	"archive/tar"
	"archive/zip"
	"compress/gzip"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"time"
)

// OrtVersion is the ONNX Runtime release that matches onnxruntime_go v1.27.0.
const OrtVersion = "1.24.1"

// ModelURL and TokenizerURL are where `semantic init` fetches the embedding
// model. ModelURL is the full model (~86MB); the quantized model
// (model_quantized.onnx) is ~23MB.
const (
	ModelURL     = "https://huggingface.co/Xenova/all-MiniLM-L6-v2/resolve/main/onnx/model.onnx"
	TokenizerURL = "https://huggingface.co/Xenova/all-MiniLM-L6-v2/resolve/main/tokenizer.json" //nolint:gosec // G101 false positive: a public asset URL, not a credential
)

// OrtDownloadURL returns the GitHub release URL for the ORT shared library
// archive for the current platform.
func OrtDownloadURL() string {
	switch runtime.GOOS {
	case "darwin":
		arch := "arm64"
		if runtime.GOARCH != "arm64" {
			arch = "x86_64"
		}
		return fmt.Sprintf(
			"https://github.com/microsoft/onnxruntime/releases/download/v%s/onnxruntime-osx-%s-%s.tgz",
			OrtVersion, arch, OrtVersion,
		)
	case "windows":
		arch := "x64"
		if runtime.GOARCH == "arm64" {
			arch = "arm64"
		}
		return fmt.Sprintf(
			"https://github.com/microsoft/onnxruntime/releases/download/v%s/onnxruntime-win-%s-%s.zip",
			OrtVersion, arch, OrtVersion,
		)
	default: // linux
		arch := "x64"
		if runtime.GOARCH == "arm64" {
			arch = "aarch64"
		}
		return fmt.Sprintf(
			"https://github.com/microsoft/onnxruntime/releases/download/v%s/onnxruntime-linux-%s-%s.tgz",
			OrtVersion, arch, OrtVersion,
		)
	}
}

// DownloadAll downloads the ONNX Runtime library and model files.
// logf receives progress messages.
func DownloadAll(logf func(string, ...any)) error {
	if err := DownloadOrt(logf); err != nil {
		return fmt.Errorf("ONNX Runtime: %w", err)
	}
	if err := DownloadModel(logf); err != nil {
		return fmt.Errorf("model: %w", err)
	}
	return nil
}

// DownloadOrt downloads and caches the ONNX Runtime shared library.
func DownloadOrt(logf func(string, ...any)) error {
	destDir := OrtCacheDir()
	destPath := filepath.Join(destDir, OrtLibFilename())
	if fileExists(destPath) {
		logf("  ✓ ONNX Runtime already cached at %s", destPath)
		return nil
	}
	if err := os.MkdirAll(destDir, 0o750); err != nil {
		return err
	}
	url := OrtDownloadURL()
	logf("  ↓ Downloading ONNX Runtime v%s (%s)...", OrtVersion, url)
	resp, err := httpGet(url)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if runtime.GOOS == "windows" {
		if err := extractOrtLibFromZip(resp.Body, destPath); err != nil {
			return fmt.Errorf("extracting library: %w", err)
		}
	} else {
		if err := extractOrtLib(resp.Body, destPath); err != nil {
			return fmt.Errorf("extracting library: %w", err)
		}
	}
	logf("  ✓ Saved to %s", destPath)
	return nil
}

// DownloadModel downloads and caches model.onnx and tokenizer.json.
func DownloadModel(logf func(string, ...any)) error {
	dir := ModelCacheDir()
	if err := os.MkdirAll(dir, 0o750); err != nil {
		return err
	}
	modelPath := filepath.Join(dir, "model.onnx")
	tokPath := filepath.Join(dir, "tokenizer.json")

	if fileExists(modelPath) {
		logf("  ✓ Model already cached at %s", modelPath)
	} else {
		logf("  ↓ Downloading all-MiniLM-L6-v2 model (~86 MB)...")
		if err := downloadFile(ModelURL, modelPath); err != nil {
			return fmt.Errorf("model download: %w", err)
		}
		logf("  ✓ Saved to %s", modelPath)
	}

	if fileExists(tokPath) {
		logf("  ✓ Tokenizer already cached at %s", tokPath)
	} else {
		logf("  ↓ Downloading tokenizer.json...")
		if err := downloadFile(TokenizerURL, tokPath); err != nil {
			return fmt.Errorf("tokenizer download: %w", err)
		}
		logf("  ✓ Saved to %s", tokPath)
	}
	return nil
}

// maxExtractedLibSize caps how much of a single archive entry extractOrtLib(FromZip)
// will write out, bounding a malicious or corrupt archive's decompression blast
// radius. The real ORT shared library is tens of MB; this leaves generous headroom.
const maxExtractedLibSize = 500 << 20 // 500MB

// extractOrtLib unpacks the first libonnxruntime*.{dylib,so.*} regular file
// from the .tgz stream and writes it to destPath. Symlinks are skipped;
// only the actual versioned library file is extracted.
func extractOrtLib(r io.Reader, destPath string) error {
	gz, err := gzip.NewReader(r)
	if err != nil {
		return fmt.Errorf("gzip: %w", err)
	}
	defer gz.Close()

	tr := tar.NewReader(gz)
	for {
		hdr, err := tr.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			return fmt.Errorf("tar: %w", err)
		}
		if hdr.Typeflag != tar.TypeReg {
			continue // skip dirs, symlinks, etc.
		}
		base := filepath.Base(hdr.Name)
		if !strings.HasPrefix(base, "libonnxruntime") {
			continue
		}
		// Match versioned dylib (libonnxruntime.1.24.1.dylib) or so (libonnxruntime.so.1.24.1)
		if !strings.Contains(base, ".dylib") && !strings.Contains(base, ".so") {
			continue
		}
		return writeCappedLib(tr, destPath, maxExtractedLibSize)
	}
	return fmt.Errorf("no libonnxruntime library found in archive — check ORT release URL")
}

// writeCappedLib copies src to destPath atomically (temp file + rename, 0600),
// refusing to write more than limit bytes so a corrupt or malicious archive
// entry can't blow up decompression. Callers pass maxExtractedLibSize; the cap
// is a parameter so a test can exercise the refusal without half a gigabyte.
func writeCappedLib(src io.Reader, destPath string, limit int64) error {
	tmp, err := os.CreateTemp(filepath.Dir(destPath), ".ort-download-*")
	if err != nil {
		return err
	}
	tmpPath := tmp.Name()
	n, copyErr := io.Copy(tmp, io.LimitReader(src, limit+1))
	tmp.Close()
	if copyErr != nil {
		os.Remove(tmpPath)
		return fmt.Errorf("copying library: %w", copyErr)
	}
	if n > limit {
		os.Remove(tmpPath)
		return fmt.Errorf("library entry exceeds %d bytes, aborting", limit)
	}
	if err := os.Chmod(tmpPath, 0o600); err != nil {
		os.Remove(tmpPath)
		return err
	}
	return os.Rename(tmpPath, destPath)
}

// extractOrtLibFromZip spools the zip archive to a temp file (archive/zip
// needs a ReaderAt with known size), then extracts the first onnxruntime.dll
// regular file to destPath. Used for Windows ORT distributions.
func extractOrtLibFromZip(r io.Reader, destPath string) error {
	spool, err := os.CreateTemp(filepath.Dir(destPath), ".ort-zip-*")
	if err != nil {
		return err
	}
	spoolPath := spool.Name()
	defer os.Remove(spoolPath)

	size, err := io.Copy(spool, r)
	if err != nil {
		spool.Close()
		return fmt.Errorf("spooling zip: %w", err)
	}
	if _, err := spool.Seek(0, io.SeekStart); err != nil {
		spool.Close()
		return err
	}
	zr, err := zip.NewReader(spool, size)
	if err != nil {
		spool.Close()
		return fmt.Errorf("zip: %w", err)
	}
	defer spool.Close()

	for _, f := range zr.File {
		base := filepath.Base(f.Name)
		if !strings.EqualFold(base, "onnxruntime.dll") {
			continue
		}
		if f.Mode().IsDir() {
			continue
		}
		in, err := f.Open()
		if err != nil {
			return fmt.Errorf("opening zip entry %s: %w", f.Name, err)
		}
		err = writeCappedLib(in, destPath, maxExtractedLibSize)
		in.Close()
		return err
	}
	return fmt.Errorf("onnxruntime.dll not found in zip — check ORT release URL")
}

// downloadFile downloads url to destPath atomically (temp file + rename).
func downloadFile(url, destPath string) error {
	resp, err := httpGet(url)
	if err != nil {
		return err
	}
	defer resp.Body.Close()

	tmp, err := os.CreateTemp(filepath.Dir(destPath), ".download-*")
	if err != nil {
		return err
	}
	tmpPath := tmp.Name()
	defer os.Remove(tmpPath) // no-op after successful rename

	if _, err := io.Copy(tmp, resp.Body); err != nil {
		tmp.Close()
		return fmt.Errorf("writing %s: %w", url, err)
	}
	tmp.Close()
	return os.Rename(tmpPath, destPath)
}

func httpGet(url string) (*http.Response, error) {
	client := &http.Client{Timeout: 10 * time.Minute}
	resp, err := client.Get(url)
	if err != nil {
		return nil, fmt.Errorf("GET %s: %w", url, err)
	}
	if resp.StatusCode != 200 {
		resp.Body.Close()
		return nil, fmt.Errorf("GET %s: HTTP %d", url, resp.StatusCode)
	}
	return resp, nil
}
