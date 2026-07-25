// Copyright (c) 2026 Reactor Technologies, Inc.
// SPDX-License-Identifier: Apache-2.0

package index

import (
	"path/filepath"
	"strconv"
	"testing"

	"github.com/reactor-team/semantic/pkg/embed"
)

// reindex runs a pass over vault with the fake embedder and fails the test on
// error. Most cases here care about the report, not the plumbing.
func reindex(t *testing.T, s *Store, vault string, force bool) *Report {
	t.Helper()
	rep, err := s.Reindex(vault, fakeEmbed, force)
	if err != nil {
		t.Fatalf("reindex: %v", err)
	}
	return rep
}

// A settled index reports no staleness and does no work on a second pass. This
// is the baseline every other case here is a deviation from: if versioning made
// ordinary runs rebuild, it would have made the tool useless rather than safe.
func TestRepresentation_SettledIndexIsQuiet(t *testing.T) {
	t.Parallel()
	vault := t.TempDir()
	writeFile(t, vault, "a.md", "# A\n\nSome prose.\n")
	s := openTemp(t)

	reindex(t, s, vault, false)
	rep := reindex(t, s, vault, false)

	if rep.Rebuild != "" {
		t.Errorf("Rebuild = %q, want empty on a settled index", rep.Rebuild)
	}
	if rep.Added+rep.Updated+rep.Relinked != 0 {
		t.Errorf("did work on a settled index: +%d ~%d relink=%d", rep.Added, rep.Updated, rep.Relinked)
	}
	if rep.Unchanged != 1 {
		t.Errorf("Unchanged = %d, want 1", rep.Unchanged)
	}
}

// A fresh index stamps the current representation rather than reporting itself
// stale. There is nothing indexed to rebuild, and claiming otherwise would make
// every first run announce a phantom upgrade.
func TestRepresentation_FreshIndexStampsWithoutRebuilding(t *testing.T) {
	t.Parallel()
	vault := t.TempDir()
	writeFile(t, vault, "a.md", "# A\n")
	s := openTemp(t)

	rep := reindex(t, s, vault, false)
	if rep.Rebuild != "" {
		t.Errorf("Rebuild = %q, want empty for a fresh index", rep.Rebuild)
	}

	meta, err := s.allMeta()
	if err != nil {
		t.Fatal(err)
	}
	want := map[string]string{
		metaChunkVersion: strconv.Itoa(chunkVersion),
		metaLinkVersion:  strconv.Itoa(linkVersion),
		metaEmbedID:      embed.RepresentationID(),
	}
	for k, v := range want {
		if meta[k] != v {
			t.Errorf("meta[%s] = %q, want %q", k, meta[k], v)
		}
	}
}

// A chunker bump re-chunks and re-embeds every file, even though not one byte
// on disk changed. This is the whole point: content hashes cannot see that the
// same bytes would now produce different chunks.
func TestRepresentation_ChunkerBumpRebuildsUnchangedFiles(t *testing.T) {
	t.Parallel()
	vault := t.TempDir()
	writeFile(t, vault, "a.md", "# A\n\nProse.\n")
	writeFile(t, vault, "b.md", "# B\n\nMore prose.\n")
	s := openTemp(t)
	reindex(t, s, vault, false)

	// Stand in for shipping a release with a new chunker.
	if err := s.setMeta(metaChunkVersion, strconv.Itoa(chunkVersion-1)); err != nil {
		t.Fatal(err)
	}

	rep := reindex(t, s, vault, false)
	if rep.Rebuild == "" {
		t.Fatal("Rebuild is empty; want a chunker-change reason")
	}
	if rep.Updated != 2 {
		t.Errorf("Updated = %d, want 2 (every file rebuilt)", rep.Updated)
	}
	if rep.Unchanged != 0 {
		t.Errorf("Unchanged = %d, want 0", rep.Unchanged)
	}

	// And the stamp is now current, so the next run is quiet again.
	if rep := reindex(t, s, vault, false); rep.Rebuild != "" || rep.Updated != 0 {
		t.Errorf("second pass not settled: Rebuild=%q Updated=%d", rep.Rebuild, rep.Updated)
	}
}

// A model change re-embeds. Vectors from two checkpoints are not comparable, so
// leaving the old ones in place would silently corrupt every ranking.
func TestRepresentation_EmbedModelChangeRebuilds(t *testing.T) {
	t.Parallel()
	vault := t.TempDir()
	writeFile(t, vault, "a.md", "# A\n")
	s := openTemp(t)
	reindex(t, s, vault, false)

	if err := s.setMeta(metaEmbedID, "some-other-model+mean+l2+d384+s256"); err != nil {
		t.Fatal(err)
	}

	rep := reindex(t, s, vault, false)
	if rep.Updated != 1 {
		t.Errorf("Updated = %d, want 1 (re-embedded)", rep.Updated)
	}
	if rep.Rebuild == "" {
		t.Error("Rebuild is empty; want an embedding-model reason")
	}
}

// A link-extractor bump alone takes the cheap path: edges are re-parsed, chunks
// and vectors are left alone. Re-embedding a whole vault to fix link rows is
// the difference between seconds and minutes, so this distinction is the reason
// the two versions are tracked separately at all.
func TestRepresentation_LinkBumpRelinksWithoutReembedding(t *testing.T) {
	t.Parallel()
	vault := t.TempDir()
	writeFile(t, vault, "a.md", "# A\n\nSee [B](b.md).\n")
	writeFile(t, vault, "b.md", "# B\n")
	s := openTemp(t)
	reindex(t, s, vault, false)

	before, err := s.AllChunks("")
	if err != nil {
		t.Fatal(err)
	}

	if err := s.setMeta(metaLinkVersion, strconv.Itoa(linkVersion-1)); err != nil {
		t.Fatal(err)
	}

	rep := reindex(t, s, vault, false)
	if rep.Relinked != 2 {
		t.Errorf("Relinked = %d, want 2", rep.Relinked)
	}
	if rep.Updated != 0 {
		t.Errorf("Updated = %d, want 0 — a link bump must not re-embed", rep.Updated)
	}

	// The edges survive the round trip, and the vectors are untouched.
	links, err := s.AllLinks()
	if err != nil {
		t.Fatal(err)
	}
	if len(links) != 1 || links[0].Target != "b.md" {
		t.Errorf("links = %+v, want one edge to b.md", links)
	}
	after, err := s.AllChunks("")
	if err != nil {
		t.Fatal(err)
	}
	if len(before) != len(after) {
		t.Fatalf("chunk count changed: %d → %d", len(before), len(after))
	}
	for i := range before {
		if before[i].Key != after[i].Key {
			t.Errorf("chunk %d key changed: %q → %q", i, before[i].Key, after[i].Key)
		}
		if !vecEqual(before[i].Vec, after[i].Vec) {
			t.Errorf("chunk %q was re-embedded during a link-only rebuild", before[i].Key)
		}
	}
}

// A file that appeared while only links were stale still gets indexed in full.
// The relink path short-circuits on files the index already knows; a new one
// must not fall through it and land in the index without chunks.
func TestRepresentation_RelinkStillIndexesNewFiles(t *testing.T) {
	t.Parallel()
	vault := t.TempDir()
	writeFile(t, vault, "a.md", "# A\n")
	s := openTemp(t)
	reindex(t, s, vault, false)

	if err := s.setMeta(metaLinkVersion, strconv.Itoa(linkVersion-1)); err != nil {
		t.Fatal(err)
	}
	writeFile(t, vault, "new.md", "# New\n\nFresh content.\n")

	rep := reindex(t, s, vault, false)
	if rep.Added != 1 {
		t.Errorf("Added = %d, want 1", rep.Added)
	}

	chunks, err := s.AllChunks("new.md")
	if err != nil {
		t.Fatal(err)
	}
	if len(chunks) == 0 {
		t.Error("new file indexed with no chunks during a relink pass")
	}
	for _, c := range chunks {
		if len(c.Vec) == 0 {
			t.Errorf("chunk %q has a placeholder vector; it was never embedded", c.Key)
		}
	}
}

// Without an embedder there is nothing to refresh a vector with, so a model
// change is neither reported nor stamped. Stamping it would claim vectors that
// were never computed and strand the placeholders forever.
func TestRepresentation_NoEmbedderLeavesModelStampAlone(t *testing.T) {
	t.Parallel()
	vault := t.TempDir()
	writeFile(t, vault, "a.md", "# A\n")
	s := openTemp(t)

	if _, err := s.Reindex(vault, nil, false); err != nil {
		t.Fatal(err)
	}
	meta, err := s.allMeta()
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := meta[metaEmbedID]; ok {
		t.Errorf("embed stamp written by a --no-embed pass: %q", meta[metaEmbedID])
	}
	// Chunk and link stamps are honest after such a pass: both really were
	// produced by this binary.
	if meta[metaChunkVersion] != strconv.Itoa(chunkVersion) {
		t.Errorf("chunk stamp = %q, want %d", meta[metaChunkVersion], chunkVersion)
	}

	// A later run with a real embedder notices and heals the placeholders.
	why, err := s.RebuildReason(true)
	if err != nil {
		t.Fatal(err)
	}
	if why == "" {
		t.Error("RebuildReason is empty; a real embedder should see the missing stamp")
	}
	rep := reindex(t, s, vault, false)
	if rep.Updated != 1 {
		t.Errorf("Updated = %d, want 1 (placeholders healed)", rep.Updated)
	}
}

// An index written before representation versioning existed carries no stamps.
// Its rows came from an unknown version, so it rebuilds rather than being
// trusted. This is the upgrade path every existing user takes exactly once.
func TestRepresentation_UnstampedLegacyIndexRebuilds(t *testing.T) {
	t.Parallel()
	vault := t.TempDir()
	writeFile(t, vault, "a.md", "# A\n")
	s := openTemp(t)
	reindex(t, s, vault, false)

	if _, err := s.db.Exec(`DELETE FROM meta`); err != nil {
		t.Fatal(err)
	}

	why, err := s.RebuildReason(true)
	if err != nil {
		t.Fatal(err)
	}
	if why == "" {
		t.Fatal("RebuildReason is empty for an unstamped index")
	}
	if rep := reindex(t, s, vault, false); rep.Updated != 1 {
		t.Errorf("Updated = %d, want 1", rep.Updated)
	}
}

// RebuildReason must describe the run the caller is about to make. Asking with
// and without an embedder can legitimately differ, and answering for the wrong
// one would print a warning about work that never happens.
func TestRepresentation_RebuildReasonTracksEmbedderAvailability(t *testing.T) {
	t.Parallel()
	vault := t.TempDir()
	writeFile(t, vault, "a.md", "# A\n")
	s := openTemp(t)
	reindex(t, s, vault, false)

	if err := s.setMeta(metaEmbedID, "different-model"); err != nil {
		t.Fatal(err)
	}

	withEmbed, err := s.RebuildReason(true)
	if err != nil {
		t.Fatal(err)
	}
	if withEmbed == "" {
		t.Error("want a reason when an embedder is available")
	}
	withoutEmbed, err := s.RebuildReason(false)
	if err != nil {
		t.Fatal(err)
	}
	if withoutEmbed != "" {
		t.Errorf("reason = %q, want empty without an embedder", withoutEmbed)
	}
}

// The schema migration adds the meta table to an index created before it. The
// v4 database is built by hand here because the point is to exercise the
// upgrade path, which a freshly created v5 database would skip entirely.
func TestRepresentation_MigrationFromV4AddsMeta(t *testing.T) {
	t.Parallel()
	path := filepath.Join(t.TempDir(), "index.db")

	s, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.db.Exec(`DROP TABLE meta`); err != nil {
		t.Fatal(err)
	}
	if _, err := s.db.Exec(`PRAGMA user_version=4`); err != nil {
		t.Fatal(err)
	}
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}

	reopened, err := Open(path)
	if err != nil {
		t.Fatalf("reopening a v4 index: %v", err)
	}
	defer func() { _ = reopened.Close() }()

	var v int
	if err := reopened.db.QueryRow(`PRAGMA user_version`).Scan(&v); err != nil {
		t.Fatal(err)
	}
	if v != schemaVersion {
		t.Errorf("user_version = %d, want %d", v, schemaVersion)
	}
	meta, err := reopened.allMeta()
	if err != nil {
		t.Fatalf("meta table missing after migration: %v", err)
	}
	if len(meta) != 0 {
		t.Errorf("meta = %v, want empty so the next run rebuilds", meta)
	}
}

func vecEqual(a, b embed.Vec) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}
