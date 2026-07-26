// This file makes an upgrade safe. The incremental reindexer decides what to
// redo by comparing content hashes, which answers "did this file change?" and
// cannot answer "would this file produce different chunks now?". When a release
// changes the chunker, the link extractor, or the embedding model, every
// content hash still matches, so a plain `semantic index` skips the whole vault
// and leaves the index holding rows built by the previous version.
//
// The fix is to version the *representation* alongside the schema and store the
// stamps in the index. A mismatch on open means the index was built by a
// different version of this logic, and the affected work is redone
// automatically — no `--force`, and no user who has to know they needed one.
//
// Schema version and representation version answer different questions and are
// deliberately separate. `PRAGMA user_version` describes the shape of the
// tables: a migration can add a column without invalidating a single stored
// vector. These stamps describe the *content* of the rows: the shape is fine,
// the values are stale.

package index

import (
	"fmt"
	"strconv"
	"strings"

	"github.com/reactor-team/semantic/pkg/embed"
)

// Representation versions. Bump one when a change makes the stored rows it
// covers wrong for an unchanged file, and add a line to CHANGELOG.md marked
// [reindex] so the rebuild is not a surprise.
//
// The two are separate because they cost different amounts to redo. Link
// extraction is a parse; re-chunking drags the whole corpus back through the
// embedding model. Bumping only what actually changed is the difference between
// a few seconds and several minutes on a large vault.
const (
	// chunkVersion covers anything that changes a chunk's key, text, variant,
	// heading path, or line — every chunker, and the markdown simplifier they
	// share. Bumping it re-chunks and re-embeds the whole vault.
	chunkVersion = 4

	// linkVersion covers link extraction only: which edges a file yields, and
	// their targets, anchors, kinds, and lines. Bumping it re-extracts links
	// and touches neither chunks nor vectors.
	linkVersion = 3
)

// Keys under which the stamps live in the meta table.
const (
	metaChunkVersion = "chunk_version"
	metaLinkVersion  = "link_version"
	metaEmbedID      = "embed_id"
)

// staleness is what a stored representation stamp mismatch implies for the run
// about to happen: which categories of stored row can no longer be trusted.
type staleness struct {
	chunks bool // chunker changed: re-chunk and re-embed
	embeds bool // embedding model changed: vectors are in a different space
	links  bool // link extractor changed: re-extract edges
	why    []string
}

// rebuildAll reports whether the run must re-chunk and re-embed every file.
// Both triggers imply the stored vectors are unusable, and a stale chunker also
// invalidates the text those vectors were built from.
func (st staleness) rebuildAll() bool { return st.chunks || st.embeds }

// relinkOnly reports whether links alone need redoing — the cheap path, where
// chunks and vectors are still valid and only the edges get rewritten.
func (st staleness) relinkOnly() bool { return st.links && !st.rebuildAll() }

// reason renders the mismatch for the user. An automatic rebuild that does not
// say why reads as an unexplained pause on a command that is usually instant.
func (st staleness) reason() string { return strings.Join(st.why, "; ") }

// detectStaleness compares the stamps in the index against this binary's. It
// reports nothing for an index with no files: there is no stale content to
// rebuild, and the caller stamps the current values once the walk is done.
//
// canEmbed says whether a real embedder is available. Without one there is no
// way to refresh a vector, so a model change is not reported — the stamp is
// also left alone, which is what makes the next embedding run notice it.
func (s *Store) detectStaleness(canEmbed bool) (staleness, error) {
	var st staleness

	stats, err := s.Stats()
	if err != nil {
		return st, err
	}
	if stats.Files == 0 {
		return st, nil
	}

	stored, err := s.allMeta()
	if err != nil {
		return st, err
	}

	// A missing stamp means the index predates representation versioning, so
	// its rows were built by an unknown version. Treat that as stale: the whole
	// point is to not trust rows we cannot account for.
	if v, ok := stored[metaChunkVersion]; !ok || v != strconv.Itoa(chunkVersion) {
		st.chunks = true
		st.why = append(st.why, fmt.Sprintf("chunker changed (%s → %d)", orUnknown(v), chunkVersion))
	}
	if v, ok := stored[metaLinkVersion]; !ok || v != strconv.Itoa(linkVersion) {
		st.links = true
		st.why = append(st.why, fmt.Sprintf("link extraction changed (%s → %d)", orUnknown(v), linkVersion))
	}
	if canEmbed {
		if v, ok := stored[metaEmbedID]; !ok || v != embed.RepresentationID() {
			st.embeds = true
			st.why = append(st.why, fmt.Sprintf("embedding model changed (%s → %s)", orUnknown(v), embed.RepresentationID()))
		}
	}
	return st, nil
}

// RebuildReason reports why the next Reindex will redo work that the content
// hashes alone would skip, or "" when it will not. Reindex reports the same
// string afterwards; this exists so a caller can say so *first*. A rebuild
// turns an ordinarily instant command into a multi-minute one, and an
// unexplained pause is indistinguishable from a hang.
//
// canEmbed must match the embedder the caller will pass to Reindex, or the
// answer describes a different run than the one about to happen.
func (s *Store) RebuildReason(canEmbed bool) (string, error) {
	st, err := s.detectStaleness(canEmbed)
	if err != nil {
		return "", err
	}
	return st.reason(), nil
}

// EmbedStamp returns the representation the index's stored vectors were built
// with, or "" when nothing has ever embedded into it.
//
// Reindex compares this itself and heals a mismatch. It is exported for the
// one caller that cannot rely on that: --no-reindex skips the healing, and
// ranking a query vector against vectors from another model returns confident
// nonsense rather than an error. A caller about to do that needs to be able to
// ask first.
func (s *Store) EmbedStamp() (string, error) {
	stored, err := s.allMeta()
	if err != nil {
		return "", err
	}
	return stored[metaEmbedID], nil
}

// stampRepresentation records this binary's stamps as the state of the index.
// Call it only after a run that actually brought the index up to them.
//
// The embedding stamp is written only when a real embedder was available.
// Recording it after a --no-embed pass would claim vectors that were never
// computed, and the placeholder rows would then never be healed.
func (s *Store) stampRepresentation(canEmbed bool) error {
	if err := s.setMeta(metaChunkVersion, strconv.Itoa(chunkVersion)); err != nil {
		return err
	}
	if err := s.setMeta(metaLinkVersion, strconv.Itoa(linkVersion)); err != nil {
		return err
	}
	if !canEmbed {
		return nil
	}
	return s.setMeta(metaEmbedID, embed.RepresentationID())
}

func orUnknown(v string) string {
	if v == "" {
		return "unknown"
	}
	return v
}
