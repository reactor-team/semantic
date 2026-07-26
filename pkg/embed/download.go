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
	"sync"
	"time"
)

// OrtVersion is the ONNX Runtime release whose C API onnxruntime_go v1.31.0
// is built against. The two are coupled: the binding compiles against one
// version of the headers and dlopens whatever this constant downloaded, so
// bumping the Go module without bumping this constant produces a binary that
// loads a library it was not compiled for. Nothing in CI catches that — the
// tests skip inference when no model is installed — so the versions move
// together, in one commit, or not at all.
const OrtVersion = "1.26.0"

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

// ensureMu serializes EnsureModel so concurrent callers fetch the model once
// between them rather than once each. Like initMu, it is a mutex and not a
// sync.Once: a download that fails on a flaky network must stay retryable
// instead of being cached as failed for the life of the process.
var ensureMu sync.Mutex

// EnsureModel downloads whatever Check reports missing, and does nothing when
// everything is already cached. It exists so a command that is about to embed
// can heal itself instead of failing with an instruction to run `init` and be
// run a second time.
//
// A checkpoint change is the case it is really for: ModelCacheDir moves with
// the checkpoint, so an upgraded binary finds nothing at the new path, on a
// machine whose owner has run `init` once already and reasonably believes the
// model is installed. The reindex that same upgrade triggers is automatic, and
// a manual step in the middle of an otherwise invisible migration is the part
// a user would experience as breakage.
//
// Get does not call this. A library that fetches a hundred-odd MB because something
// imported it is a surprise no caller asked for; the choice to spend the
// bandwidth belongs to the command, which is also the thing with somewhere to
// report progress.
//
// $SEMANTIC_NO_DOWNLOAD turns it back into a check, for a sandbox or an
// air-gapped machine where an unasked-for hundred-MB fetch is worse than the error
// it avoids. The test suite sets it so no script can spend the bandwidth by
// accident.
func EnsureModel(logf func(string, ...any)) error {
	ensureMu.Lock()
	defer ensureMu.Unlock()
	// Re-checked under the lock, not before it: a caller that queued behind a
	// download in progress finds the files there and returns without starting
	// a second one.
	err := Check()
	if err == nil {
		return nil
	}
	if strings.TrimSpace(os.Getenv("SEMANTIC_NO_DOWNLOAD")) != "" {
		return err
	}
	return DownloadAll(logf)
}

// Progress reports how far a single file's download has got. total is the
// size the server advertised, or 0 when it advertised none — a caller showing
// a percentage has to handle that, because Hugging Face redirects to a CDN
// that does not always send Content-Length.
//
// It is a package-level hook rather than a parameter threaded through five
// functions because it is presentation, and every one of those functions
// otherwise has nothing to say about how bytes are displayed. nil means the
// caller wants none, which is the default and what every test gets.
type Progress func(name string, done, total int64)

var progressFn Progress

// SetProgress installs the hook downloads report through. Pass nil to silence
// it. Not safe against a download already running; the CLI sets it once at
// startup.
func SetProgress(fn Progress) { progressFn = fn }

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
	m := Current()
	modelPath := filepath.Join(dir, "model.onnx")
	tokPath := filepath.Join(dir, "tokenizer.json")

	if fileExists(modelPath) {
		logf("  ✓ Model already cached at %s", modelPath)
	} else {
		logf("  ↓ Downloading %s model (~%d MB)...", m.Name, m.ApproxMB)
		if err := downloadFile(m.ModelURL, m.Name+" model", modelPath); err != nil {
			return fmt.Errorf("model download: %w", err)
		}
		logf("  ✓ Saved to %s", modelPath)
	}

	if fileExists(tokPath) {
		logf("  ✓ Tokenizer already cached at %s", tokPath)
	} else {
		logf("  ↓ Downloading tokenizer.json...")
		if err := downloadFile(m.TokenizerURL, "tokenizer", tokPath); err != nil {
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

// downloadFile downloads url to destPath atomically (temp file + rename),
// reporting bytes through the Progress hook as they arrive. name is what the
// hook shows the user; the URL is not, being mostly a CDN path nobody reads.
func downloadFile(url, name, destPath string) error {
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

	var src io.Reader = resp.Body
	if progressFn != nil {
		src = &progressReader{r: resp.Body, name: name, total: resp.ContentLength}
	}
	if _, err := io.Copy(tmp, src); err != nil {
		tmp.Close()
		return fmt.Errorf("writing %s: %w", url, err)
	}
	tmp.Close()
	return os.Rename(tmpPath, destPath)
}

// progressReader counts bytes on their way past and hands the running total to
// the Progress hook. Rate limiting belongs to the hook, not here: this cannot
// know how expensive the display is, and a reader that decided for itself
// would have to guess.
type progressReader struct {
	r     io.Reader
	name  string
	total int64
	done  int64
}

func (p *progressReader) Read(b []byte) (int, error) {
	n, err := p.r.Read(b)
	p.done += int64(n)
	progressFn(p.name, p.done, p.total)
	return n, err
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
