// Copyright (c) 2026 Reactor Technologies, Inc.
// SPDX-License-Identifier: Apache-2.0

package index

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"time"

	ignore "github.com/sabhiram/go-gitignore"

	"github.com/reactor-team/semantic/pkg/chunk"
	"github.com/reactor-team/semantic/pkg/embed"
)

// FileMeta is the on-disk identity of a markdown file: its path relative to
// the vault root (forward-slashed) plus the stat + content-hash used to
// decide whether it needs re-embedding.
type FileMeta struct {
	RelPath string
	MtimeNS int64
	Size    int64
	Hash    string
}

// Embedder turns chunk text into a vector. Injected so the indexer is
// testable without the ONNX runtime; production passes embed.Get. A nil
// Embedder opts a Reindex call into skip-embed mode: chunks are stored with
// a zero-length placeholder vec instead, and a later Reindex with a real
// Embedder detects and heals them regardless of whether the file's stat or
// content hash otherwise looks unchanged.
type Embedder func(text string) (embed.Vec, error)

// Report summarizes a Reindex run.
type Report struct {
	Added     int // files newly indexed
	Updated   int // files whose content changed and were re-embedded
	Unchanged int // files skipped (stat match, or hash match after a touch)
	Relinked  int // files whose links were re-extracted without re-embedding
	Deleted   int // files removed from the index (gone from disk)
	Files     int // total files in the index afterward
	Chunks    int // total chunks in the index afterward
	Duration  time.Duration

	// Rebuild explains why this run redid work the content hashes said was
	// unnecessary — an upgrade that changed the chunker, the link extractor, or
	// the embedding model. Empty on an ordinary incremental run. Callers should
	// surface it: an automatic rebuild is a long pause on a command that is
	// normally instant, and silence makes it look like a hang.
	Rebuild string
}

// alwaysSkipDirs are directory names never descended into, whether or not a
// .gitignore is present: version-control internals, our own index dir, and
// vendored deps. `.git` and `.semantic` are listed explicitly because git's
// ignore rules don't cover them (git ignores .git implicitly, and .semantic may
// be untracked); node_modules is skipped so a dependency-heavy tree without a
// .gitignore stays cheap.
var alwaysSkipDirs = map[string]bool{
	".git":         true,
	".semantic":    true,
	"node_modules": true,
}

// ignoreLayer is one directory's .gitignore, active for its own subtree
// during the walk. dir is that directory's forward-slashed path relative to
// the walk root ("." for the root .gitignore).
type ignoreLayer struct {
	dir string
	ig  *ignore.GitIgnore
}

// layerApplies reports whether target (a rel path, forward-slashed) falls
// within dir's subtree, so dir's .gitignore is in scope for it.
func layerApplies(dir, target string) bool {
	return dir == "." || target == dir || strings.HasPrefix(target, dir+"/")
}

// relToLayer returns target's path relative to a layer anchored at dir (both
// rel to the walk root), which is what that layer's own .gitignore patterns
// are written against.
func relToLayer(dir, target string) string {
	if dir == "." {
		return target
	}
	return strings.TrimPrefix(target, dir+"/")
}

// popStaleLayers drops layers whose directory we've walked out of: WalkDir is
// depth-first, so once a sibling outside a pushed layer's subtree is
// reached, that layer no longer applies. parentRel is the rel path of the
// directory containing the entry about to be checked.
func popStaleLayers(layers []ignoreLayer, parentRel string) []ignoreLayer {
	for len(layers) > 0 && !layerApplies(layers[len(layers)-1].dir, parentRel) {
		layers = layers[:len(layers)-1]
	}
	return layers
}

// dirSkipped reports whether a directory is pruned from the walk: an
// always-skip name, or a path any active .gitignore layer ignores. rel is
// the dir's forward-slashed path relative to the walk root. With no
// .gitignore anywhere in scope, only the always-skip set prunes — we trust
// git as the sole ignore source rather than second-guessing with a
// heuristic.
func dirSkipped(layers []ignoreLayer, rel, name string) bool {
	if alwaysSkipDirs[name] {
		return true
	}
	for _, l := range layers {
		if !layerApplies(l.dir, rel) {
			continue
		}
		sub := relToLayer(l.dir, rel)
		// A dir-only pattern like `build/` matches with the trailing slash.
		if l.ig.MatchesPath(sub) || l.ig.MatchesPath(sub+"/") {
			return true
		}
	}
	return false
}

// fileIgnored reports whether a git-ignored file should be skipped, checked
// against every active .gitignore layer (root plus any nested ones whose
// subtree rel falls within).
func fileIgnored(layers []ignoreLayer, rel string) bool {
	for _, l := range layers {
		if layerApplies(l.dir, rel) && l.ig.MatchesPath(relToLayer(l.dir, rel)) {
			return true
		}
	}
	return false
}

// loadNestedGitignore compiles a non-root directory's own .gitignore into a
// matcher scoped to it, or nil if it has none. Unlike loadGitignore, this
// never merges .git/info/exclude — that repo-wide file is only meaningful at
// the git root, already handled by the initial load before the walk starts.
func loadNestedGitignore(dirAbs string) *ignore.GitIgnore {
	lines := readLines(filepath.Join(dirAbs, ".gitignore"))
	if lines == nil {
		return nil
	}
	return ignore.CompileIgnoreLines(lines...)
}

// loadGitignore compiles the root .gitignore (plus .git/info/exclude when
// present) into a matcher, or returns nil when the tree has no .gitignore —
// the signal that we're not in a git-managed tree and should fall back to the
// dotdir heuristic. Patterns are matched relative to root, which is where a
// top-level .gitignore is anchored. Nested .gitignore files elsewhere in the
// tree are picked up per-directory during the walk (see ignoreLayer),
// keeping each one scoped to its own subtree the way git itself resolves
// them, rather than folding every pattern into one root-anchored matcher.
func loadGitignore(root string) *ignore.GitIgnore {
	gitignorePath := filepath.Join(root, ".gitignore")
	if _, err := os.Stat(gitignorePath); err != nil {
		return nil
	}
	lines := readLines(gitignorePath)
	lines = append(lines, readLines(filepath.Join(root, ".git", "info", "exclude"))...)
	return ignore.CompileIgnoreLines(lines...)
}

// readLines returns the file's lines, or nil if it can't be read — a missing
// optional exclude file is not an error.
func readLines(path string) []string {
	b, err := os.ReadFile(path) //nolint:gosec // G304: path is root plus a fixed suffix (.gitignore / .git/info/exclude), not user input
	if err != nil {
		return nil
	}
	return strings.Split(string(b), "\n")
}

// chunkerFor returns the chunker for a filename's extension, or nil if the
// file isn't indexable. The extension table lives in pkg/chunk, which is also
// where search reads language names from, so the set of indexed extensions and
// the set of `--lang` values cannot drift apart.
func chunkerFor(name string) chunk.Chunker {
	l, ok := chunk.LanguageFor(name)
	if !ok {
		return nil
	}
	return l.Chunk
}

// extractLinks returns a file's outbound document links, or nil for file
// types that don't carry a doc graph (only markdown does today). The graph is
// a markdown concept — Go source references resolve through the compiler, not
// through links.
func extractLinks(name, content string) []chunk.Link {
	switch strings.ToLower(filepath.Ext(name)) {
	case ".md", ".markdown":
		return chunk.Links(content)
	}
	return nil
}

// Reindex walks root, incrementally syncing the index to the indexable files
// under it (markdown and source code — see chunkers): new/changed files are
// chunked and embedded, touched-but-identical files are cheaply restatted,
// and files gone from disk are removed. embedFn supplies vectors (embed.Get
// in production); pass nil to skip embedding entirely (see Embedder).
//
// The change test is two-tier: a (mtime, size) match skips the file without
// reading it; otherwise the file is read and its sha256 compared, so a
// touch that didn't change bytes costs a read but no embedding. force bypasses
// both checks, re-chunking, re-embedding, and re-extracting links for every
// file regardless of whether its content changed — the escape hatch for when
// the chunker or link extractor itself changes behavior, since content-hash
// diffing has no way to know an unchanged file would now produce different
// chunks or links.
func (s *Store) Reindex(root string, embedFn Embedder, force bool) (*Report, error) {
	var layers []ignoreLayer
	if rootIg := loadGitignore(root); rootIg != nil {
		layers = append(layers, ignoreLayer{dir: ".", ig: rootIg})
	}

	start := time.Now()
	rep := &Report{}

	// Before trusting a single content hash, check whether this binary still
	// agrees with the index about how content becomes rows. A mismatch means the
	// hashes are answering the wrong question — they say "unchanged" about files
	// that would now chunk, link, or embed differently.
	st, err := s.detectStaleness(embedFn != nil)
	if err != nil {
		return nil, err
	}
	rep.Rebuild = st.reason()

	pass := reindexPass{
		embedFn:    embedFn,
		force:      force || st.rebuildAll(),
		relinkOnly: st.relinkOnly(),
		rep:        rep,
	}
	seen := map[string]bool{}

	walkErr := filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		rel, err := filepath.Rel(root, path)
		if err != nil {
			return err
		}
		rel = filepath.ToSlash(rel)

		if path != root {
			layers = popStaleLayers(layers, filepath.ToSlash(filepath.Dir(rel)))
		}

		if d.IsDir() {
			if path == root {
				return nil
			}
			if dirSkipped(layers, rel, d.Name()) {
				return fs.SkipDir
			}
			if nested := loadNestedGitignore(path); nested != nil {
				layers = append(layers, ignoreLayer{dir: rel, ig: nested})
			}
			return nil
		}
		chunkFn := chunkerFor(d.Name())
		if chunkFn == nil {
			return nil
		}
		if fileIgnored(layers, rel) {
			return nil
		}
		seen[rel] = true
		return s.indexFile(path, rel, d, chunkFn, pass)
	})
	if walkErr != nil {
		return nil, walkErr
	}

	deleted, err := s.deleteMissing(seen)
	if err != nil {
		return nil, err
	}
	rep.Deleted = deleted

	// The index now matches this binary, so record that. Stamping only after the
	// walk means an interrupted run leaves the old stamps in place and the next
	// run retries the rebuild, rather than claiming work that never finished.
	if err := s.stampRepresentation(embedFn != nil); err != nil {
		return nil, err
	}

	stats, err := s.Stats()
	if err != nil {
		return nil, err
	}
	rep.Files, rep.Chunks = stats.Files, stats.Chunks
	rep.Duration = time.Since(start)
	return rep, nil
}

// reindexPass carries the walk-invariant inputs indexFile needs beyond the
// per-file path and entry: how to embed (nil in skip-embed mode), whether to
// force a re-index, and where to tally the outcome.
type reindexPass struct {
	embedFn Embedder
	force   bool
	// relinkOnly re-extracts every file's links while leaving its chunks and
	// vectors alone. Set when an upgrade changed link extraction and nothing
	// else. Never set together with force, which already redoes links.
	relinkOnly bool
	rep        *Report
}

// indexFile syncs one indexable file into the store, updating rep with the
// outcome. It applies Reindex's two-tier change test: a (mtime, size) match
// skips without reading; otherwise the content is read and its hash compared,
// so a touch that didn't change bytes refreshes the stat but doesn't re-embed.
// A prior skip-embed pass may have left placeholder vecs; canSkip forces those
// to be re-embedded once a real embedder is supplied.
func (s *Store) indexFile(path, rel string, d fs.DirEntry, chunkFn chunk.Chunker, pass reindexPass) error {
	embedFn, force, rep := pass.embedFn, pass.force, pass.rep
	info, err := d.Info()
	if err != nil {
		return err
	}
	mtimeNS := info.ModTime().UnixNano()
	size := info.Size()

	existing, found, err := s.getFile(rel)
	if err != nil {
		return err
	}

	// Link extraction changed, but chunking and embedding did not. Re-parse the
	// file for its edges and leave its chunks and vectors in place. This costs a
	// read per file where a full rebuild would cost an embed per chunk. A file
	// the index has never seen falls through to be indexed normally.
	if pass.relinkOnly && found {
		content, err := os.ReadFile(path) //nolint:gosec // G304: path comes from filepath.WalkDir over root, not user input
		if err != nil {
			return err
		}
		if err := s.replaceLinks(existing.id, buildLinkRecs(extractLinks(d.Name(), string(content)))); err != nil {
			return err
		}
		rep.Relinked++
		return nil
	}

	// Fast path: stat unchanged → content unchanged, no read.
	if !force && found && existing.mtimeNS == mtimeNS && existing.size == size {
		skip, err := s.canSkip(existing.id, embedFn)
		if err != nil {
			return err
		}
		if skip {
			rep.Unchanged++
			return nil
		}
	}

	content, err := os.ReadFile(path) //nolint:gosec // G304: path comes from filepath.WalkDir over root, not user input
	if err != nil {
		return err
	}
	hash := sha256hex(content)
	now := time.Now().UnixNano()

	// Touched but byte-identical (e.g. rewritten with same content): refresh
	// the stat so the fast path catches it next time and skip the embed.
	if !force && found && existing.hash == hash {
		skip, err := s.canSkip(existing.id, embedFn)
		if err != nil {
			return err
		}
		if skip {
			if err := s.touchFile(existing.id, mtimeNS, size, now); err != nil {
				return err
			}
			rep.Unchanged++
			return nil
		}
	}

	// New or changed: chunk, embed, extract links, persist atomically.
	vecs, err := buildVecs(chunkFn(string(content)), rel, embedFn)
	if err != nil {
		return err
	}
	recs := buildLinkRecs(extractLinks(d.Name(), string(content)))
	if err := s.putFile(FileMeta{RelPath: rel, MtimeNS: mtimeNS, Size: size, Hash: hash}, vecs, recs, now); err != nil {
		return err
	}
	if found {
		rep.Updated++
	} else {
		rep.Added++
	}
	return nil
}

// canSkip reports whether an unchanged file can be skipped without re-embedding.
// In skip-embed mode (nil embedFn) it always can; with a real embedder it can
// only if the file's chunks aren't still carrying zero-length placeholder vecs
// from an earlier skip-embed pass — those must be healed.
func (s *Store) canSkip(id int64, embedFn Embedder) (bool, error) {
	if embedFn == nil {
		return true, nil
	}
	empty, err := s.hasEmptyVec(id)
	return !empty, err
}

// buildVecs embeds each chunk into a chunkVec. A nil embedFn (skip-embed mode)
// leaves every vec as the zero-length placeholder Reindex heals on a later pass.
func buildVecs(chunks []chunk.Chunk, rel string, embedFn Embedder) ([]chunkVec, error) {
	vecs := make([]chunkVec, 0, len(chunks))
	for _, c := range chunks {
		var v embed.Vec
		if embedFn != nil {
			var err error
			if v, err = embedFn(c.Text); err != nil {
				return nil, fmt.Errorf("embedding %s (%s): %w", rel, c.Key, err)
			}
		}
		vecs = append(vecs, chunkVec{
			key:     c.Key,
			heading: c.Heading,
			variant: c.Variant,
			text:    c.Text,
			line:    c.Line,
			vec:     v,
		})
	}
	return vecs, nil
}

// buildLinkRecs converts extracted links into their storage records.
func buildLinkRecs(links []chunk.Link) []linkRec {
	recs := make([]linkRec, 0, len(links))
	for _, l := range links {
		recs = append(recs, linkRec{target: l.Target, anchor: l.Anchor, kind: l.Kind, line: l.Line})
	}
	return recs
}

func sha256hex(b []byte) string {
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:])
}
