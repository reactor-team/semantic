// Copyright (c) 2026 Reactor Technologies, Inc.
// SPDX-License-Identifier: Apache-2.0

// Package index is the local index: a SQLite store of markdown files and
// their embedded chunks, plus the incremental (re)indexer that keeps it in
// sync with a directory tree. The driver is modernc.org/sqlite (pure Go —
// no cgo for the storage side; cgo is only ever pulled in by internal/embed
// for ONNX inference).
package index

import (
	"database/sql"
	"encoding/binary"
	"fmt"
	"math"
	"os"
	"path/filepath"
	"time"

	_ "modernc.org/sqlite" // registers the "sqlite" database/sql driver

	"github.com/reactor-team/semantic/pkg/chunk"
	"github.com/reactor-team/semantic/pkg/embed"
)

// schemaVersion is stamped into PRAGMA user_version. Bump it and add a
// migration branch in migrate() when the schema changes.
//
// This tracks the shape of the tables only. Whether the *rows* are still valid
// after an upgrade is a separate question, answered by the representation
// stamps in representation.go.
const schemaVersion = 5

const schemaSQL = `
CREATE TABLE files (
  id           INTEGER PRIMARY KEY,
  rel_path     TEXT    NOT NULL UNIQUE,
  mtime_ns     INTEGER NOT NULL,
  size         INTEGER NOT NULL,
  content_hash TEXT    NOT NULL,
  indexed_at   INTEGER NOT NULL
);

CREATE TABLE chunks (
  id           INTEGER PRIMARY KEY,
  file_id      INTEGER NOT NULL REFERENCES files(id) ON DELETE CASCADE,
  chunk_key    TEXT    NOT NULL,
  heading_path TEXT    NOT NULL,
  variant      TEXT    NOT NULL,
  text         TEXT    NOT NULL,
  line         INTEGER NOT NULL DEFAULT 0,
  vec          BLOB    NOT NULL,
  UNIQUE(file_id, chunk_key)
);

CREATE INDEX idx_chunks_file ON chunks(file_id);

CREATE TABLE links (
  src_file_id INTEGER NOT NULL REFERENCES files(id) ON DELETE CASCADE,
  target      TEXT    NOT NULL,
  anchor      TEXT    NOT NULL DEFAULT '',
  kind        TEXT    NOT NULL,
  line        INTEGER NOT NULL DEFAULT 0
);

CREATE INDEX idx_links_src ON links(src_file_id);

CREATE TABLE meta (
  key   TEXT PRIMARY KEY,
  value TEXT NOT NULL
);
`

// Store wraps the SQLite index database.
type Store struct {
	db *sql.DB
}

// DefaultDBPath is where the index lives for a given vault root:
// <vault>/.semantic/index.db. Per-vault, gitignorable, travels with the tree.
func DefaultDBPath(vault string) string {
	return filepath.Join(vault, ".semantic", "index.db")
}

// Open opens (creating if needed) the index database at path, creating the
// parent directory and applying the schema. Foreign keys are enabled so a
// file delete cascades to its chunks; a busy timeout absorbs brief lock
// contention. A single open connection serializes access, which is all a
// single-process CLI needs and sidesteps SQLite "database is locked".
func Open(path string) (*Store, error) {
	if dir := filepath.Dir(path); dir != "" && dir != "." {
		if err := os.MkdirAll(dir, 0o750); err != nil {
			return nil, fmt.Errorf("creating index dir: %w", err)
		}
	}
	dsn := "file:" + path + "?_pragma=foreign_keys(1)&_pragma=busy_timeout(5000)"
	db, err := sql.Open("sqlite", dsn)
	if err != nil {
		return nil, fmt.Errorf("opening index: %w", err)
	}
	db.SetMaxOpenConns(1)
	s := &Store{db: db}
	if err := s.migrate(); err != nil {
		_ = db.Close()
		return nil, err
	}
	return s, nil
}

// Close closes the underlying database.
func (s *Store) Close() error { return s.db.Close() }

func (s *Store) migrate() error {
	var v int
	if err := s.db.QueryRow("PRAGMA user_version").Scan(&v); err != nil {
		return fmt.Errorf("reading schema version: %w", err)
	}
	if v >= schemaVersion {
		return nil
	}
	// Fresh database: apply the current schema in full and stop — schemaSQL
	// already reflects every migration below.
	if v == 0 {
		if _, err := s.db.Exec(schemaSQL); err != nil {
			return fmt.Errorf("applying schema: %w", err)
		}
		return s.stampVersion()
	}
	// Incremental upgrades for an existing database. Each branch brings v up
	// one step; add the next as `if v < N`.
	if v < 2 {
		// v1→v2: chunks gained a source-line column. Existing rows default to
		// 0 (unknown) until their file is re-indexed.
		if _, err := s.db.Exec(`ALTER TABLE chunks ADD COLUMN line INTEGER NOT NULL DEFAULT 0`); err != nil {
			return fmt.Errorf("migrating to v2 (chunks.line): %w", err)
		}
	}
	if v < 3 {
		// v2→v3: added the links table (the document graph). Empty until each
		// file is re-indexed, which repopulates it from the markdown.
		if _, err := s.db.Exec(`
			CREATE TABLE links (
			  src_file_id INTEGER NOT NULL REFERENCES files(id) ON DELETE CASCADE,
			  target      TEXT    NOT NULL,
			  kind        TEXT    NOT NULL,
			  line        INTEGER NOT NULL DEFAULT 0
			);
			CREATE INDEX idx_links_src ON links(src_file_id);`); err != nil {
			return fmt.Errorf("migrating to v3 (links): %w", err)
		}
	}
	if v < 4 {
		// v3→v4: links gained a #section anchor column. Existing rows default to
		// '' (no anchor) until their file is re-indexed.
		if _, err := s.db.Exec(`ALTER TABLE links ADD COLUMN anchor TEXT NOT NULL DEFAULT ''`); err != nil {
			return fmt.Errorf("migrating to v4 (links.anchor): %w", err)
		}
	}
	if v < 5 {
		// v4→v5: added the meta table, which holds the representation stamps.
		// It is deliberately left empty here. Every migration above notes that
		// its new column stays at a default "until the file is re-indexed", but
		// the incremental reindexer skips unchanged files, so those rows in fact
		// stayed stale until someone ran --force. An empty meta table reads as
		// "built by an unknown version" on the next run, which rebuilds them.
		if _, err := s.db.Exec(`
			CREATE TABLE meta (
			  key   TEXT PRIMARY KEY,
			  value TEXT NOT NULL
			);`); err != nil {
			return fmt.Errorf("migrating to v5 (meta): %w", err)
		}
	}
	return s.stampVersion()
}

// allMeta reads the meta table into a map. A missing key and an empty value are
// indistinguishable to the caller by design: both mean "not recorded".
func (s *Store) allMeta() (map[string]string, error) {
	rows, err := s.db.Query(`SELECT key, value FROM meta`)
	if err != nil {
		return nil, fmt.Errorf("reading meta: %w", err)
	}
	defer func() { _ = rows.Close() }()

	out := map[string]string{}
	for rows.Next() {
		var k, v string
		if err := rows.Scan(&k, &v); err != nil {
			return nil, fmt.Errorf("scanning meta: %w", err)
		}
		out[k] = v
	}
	return out, rows.Err()
}

// setMeta upserts one meta key.
func (s *Store) setMeta(key, value string) error {
	_, err := s.db.Exec(`
		INSERT INTO meta (key, value) VALUES (?, ?)
		ON CONFLICT(key) DO UPDATE SET value = excluded.value`, key, value)
	if err != nil {
		return fmt.Errorf("writing meta %s: %w", key, err)
	}
	return nil
}

// replaceLinks atomically swaps one file's outbound links, leaving its chunks
// and their vectors untouched. This is the cheap half of an upgrade: when only
// the link extractor changed, re-parsing a file costs a read, while re-chunking
// it would drag the whole vault back through the embedding model.
func (s *Store) replaceLinks(fileID int64, links []linkRec) (err error) {
	tx, err := s.db.Begin()
	if err != nil {
		return err
	}
	defer func() {
		if err != nil {
			_ = tx.Rollback()
		}
	}()

	if _, err = tx.Exec(`DELETE FROM links WHERE src_file_id = ?`, fileID); err != nil {
		return fmt.Errorf("clearing links for file %d: %w", fileID, err)
	}
	err = insertEach(tx, `INSERT INTO links (src_file_id, target, anchor, kind, line) VALUES (?, ?, ?, ?, ?)`,
		links, func(stmt *sql.Stmt, l linkRec) error {
			if _, e := stmt.Exec(fileID, l.target, l.anchor, l.kind, l.line); e != nil {
				return fmt.Errorf("inserting link %q for file %d: %w", l.target, fileID, e)
			}
			return nil
		})
	if err != nil {
		return err
	}
	return tx.Commit()
}

func (s *Store) stampVersion() error {
	if _, err := s.db.Exec(fmt.Sprintf("PRAGMA user_version=%d", schemaVersion)); err != nil {
		return fmt.Errorf("stamping schema version: %w", err)
	}
	return nil
}

// fileRow is the stored metadata for one indexed file.
type fileRow struct {
	id      int64
	mtimeNS int64
	size    int64
	hash    string
}

// getFile returns the stored row for relPath. found is false when the file
// is not yet indexed.
func (s *Store) getFile(relPath string) (row fileRow, found bool, err error) {
	err = s.db.QueryRow(
		`SELECT id, mtime_ns, size, content_hash FROM files WHERE rel_path = ?`,
		relPath,
	).Scan(&row.id, &row.mtimeNS, &row.size, &row.hash)
	switch {
	case err == sql.ErrNoRows:
		return fileRow{}, false, nil
	case err != nil:
		return fileRow{}, false, err
	}
	return row, true, nil
}

// touchFile updates only the stat columns — used when a file's mtime/size
// changed but its content hash did not, so re-embedding is unnecessary.
func (s *Store) touchFile(id, mtimeNS, size, indexedAt int64) error {
	_, err := s.db.Exec(
		`UPDATE files SET mtime_ns = ?, size = ?, indexed_at = ? WHERE id = ?`,
		mtimeNS, size, indexedAt, id,
	)
	return err
}

// hasEmptyVec reports whether any of a file's chunks carry a zero-length vec —
// the marker a skip-embed pass (Reindex called with a nil Embedder) leaves
// behind in place of a real embedding. Callers use this to force a real
// chunk+embed pass on a file whose stat/hash otherwise looks unchanged.
func (s *Store) hasEmptyVec(fileID int64) (bool, error) {
	var exists bool
	err := s.db.QueryRow(
		`SELECT EXISTS(SELECT 1 FROM chunks WHERE file_id = ? AND length(vec) = 0)`,
		fileID,
	).Scan(&exists)
	return exists, err
}

// chunkVec is a chunk paired with its embedding, ready to persist.
type chunkVec struct {
	key     string
	heading string
	variant string
	text    string
	line    int
	vec     embed.Vec
}

// linkRec is one outbound link, ready to persist into the links table.
type linkRec struct {
	target string
	anchor string
	kind   string
	line   int
}

// putFile upserts the file row and atomically replaces its chunks and links.
// The whole operation is one transaction: either the file and all its chunks
// and links reflect the new content, or nothing changes.
func (s *Store) putFile(meta FileMeta, chunks []chunkVec, links []linkRec, indexedAt int64) (err error) {
	tx, err := s.db.Begin()
	if err != nil {
		return err
	}
	defer func() {
		if err != nil {
			_ = tx.Rollback()
		}
	}()

	var fileID int64
	err = tx.QueryRow(`
		INSERT INTO files (rel_path, mtime_ns, size, content_hash, indexed_at)
		VALUES (?, ?, ?, ?, ?)
		ON CONFLICT(rel_path) DO UPDATE SET
			mtime_ns     = excluded.mtime_ns,
			size         = excluded.size,
			content_hash = excluded.content_hash,
			indexed_at   = excluded.indexed_at
		RETURNING id`,
		meta.RelPath, meta.MtimeNS, meta.Size, meta.Hash, indexedAt,
	).Scan(&fileID)
	if err != nil {
		return fmt.Errorf("upserting file %s: %w", meta.RelPath, err)
	}

	if _, err = tx.Exec(`DELETE FROM chunks WHERE file_id = ?`, fileID); err != nil {
		return fmt.Errorf("clearing chunks for %s: %w", meta.RelPath, err)
	}
	err = insertEach(tx, `
		INSERT INTO chunks (file_id, chunk_key, heading_path, variant, text, line, vec)
		VALUES (?, ?, ?, ?, ?, ?, ?)`, chunks, func(stmt *sql.Stmt, c chunkVec) error {
		if _, e := stmt.Exec(fileID, c.key, c.heading, c.variant, c.text, c.line, encodeVec(c.vec)); e != nil {
			return fmt.Errorf("inserting chunk %s of %s: %w", c.key, meta.RelPath, e)
		}
		return nil
	})
	if err != nil {
		return err
	}

	if _, err = tx.Exec(`DELETE FROM links WHERE src_file_id = ?`, fileID); err != nil {
		return fmt.Errorf("clearing links for %s: %w", meta.RelPath, err)
	}
	err = insertEach(tx, `INSERT INTO links (src_file_id, target, anchor, kind, line) VALUES (?, ?, ?, ?, ?)`,
		links, func(stmt *sql.Stmt, l linkRec) error {
			if _, e := stmt.Exec(fileID, l.target, l.anchor, l.kind, l.line); e != nil {
				return fmt.Errorf("inserting link %q of %s: %w", l.target, meta.RelPath, e)
			}
			return nil
		})
	if err != nil {
		return err
	}

	err = tx.Commit()
	return err
}

// insertEach prepares insertSQL once, runs exec for every row, and closes the
// statement before returning — collapsing the identical delete-then-insert
// scaffolding putFile uses for both chunks and links. exec receives the
// prepared statement so each caller keeps its own argument binding and error
// wrapping. A nil/empty rows slice is a no-op (no statement is prepared).
func insertEach[T any](tx *sql.Tx, insertSQL string, rows []T, exec func(*sql.Stmt, T) error) error {
	if len(rows) == 0 {
		return nil
	}
	stmt, err := tx.Prepare(insertSQL)
	if err != nil {
		return err
	}
	defer func() { _ = stmt.Close() }()
	for _, r := range rows {
		if err := exec(stmt, r); err != nil {
			return err
		}
	}
	return nil
}

// deleteMissing removes every file (and, by cascade, its chunks) whose
// rel_path was not seen on disk during a walk. Returns the count removed.
func (s *Store) deleteMissing(seen map[string]bool) (int, error) {
	rows, err := s.db.Query(`SELECT rel_path FROM files`)
	if err != nil {
		return 0, err
	}
	var stale []string
	for rows.Next() {
		var rel string
		if err := rows.Scan(&rel); err != nil {
			_ = rows.Close()
			return 0, err
		}
		if !seen[rel] {
			stale = append(stale, rel)
		}
	}
	if err := rows.Err(); err != nil {
		_ = rows.Close()
		return 0, err
	}
	_ = rows.Close()

	for _, rel := range stale {
		if _, err := s.db.Exec(`DELETE FROM files WHERE rel_path = ?`, rel); err != nil {
			return 0, err
		}
	}
	return len(stale), nil
}

// ChunkRow is one chunk joined to its file, as returned to the search layer.
type ChunkRow struct {
	RelPath string
	Key     string
	Heading string
	Variant string
	Text    string
	Line    int
	Vec     embed.Vec
}

// AllChunks returns every chunk in the index, optionally restricted to
// files whose rel_path begins with pathPrefix (empty = no filter). The
// search layer cosine-ranks these in memory.
func (s *Store) AllChunks(pathPrefix string) ([]ChunkRow, error) {
	query := `
		SELECT f.rel_path, c.chunk_key, c.heading_path, c.variant, c.text, c.line, c.vec
		FROM chunks c JOIN files f ON f.id = c.file_id`
	var args []any
	if pathPrefix != "" {
		query += ` WHERE f.rel_path LIKE ? ESCAPE '\'`
		args = append(args, escapeLike(pathPrefix)+"%")
	}
	rows, err := s.db.Query(query, args...)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()

	var out []ChunkRow
	for rows.Next() {
		var r ChunkRow
		var blob []byte
		if err := rows.Scan(&r.RelPath, &r.Key, &r.Heading, &r.Variant, &r.Text, &r.Line, &blob); err != nil {
			return nil, err
		}
		r.Vec = decodeVec(blob)
		out = append(out, r)
	}
	return out, rows.Err()
}

// AllFiles returns every indexed file's rel-path, sorted. The graph layer
// uses this as the universe of link targets to resolve against.
func (s *Store) AllFiles() ([]string, error) {
	rows, err := s.db.Query(`SELECT rel_path FROM files ORDER BY rel_path`)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()

	var out []string
	for rows.Next() {
		var rel string
		if err := rows.Scan(&rel); err != nil {
			return nil, err
		}
		out = append(out, rel)
	}
	return out, rows.Err()
}

// LinkRow is one stored outbound link joined to its source file's rel-path,
// as returned to the graph layer.
type LinkRow struct {
	SrcRelPath string
	Target     string
	Anchor     string
	Kind       string
	Line       int
}

// AllLinks returns every stored link edge (source file → raw target). Targets
// are unresolved — the graph layer maps them onto AllFiles.
func (s *Store) AllLinks() ([]LinkRow, error) {
	rows, err := s.db.Query(`
		SELECT f.rel_path, l.target, l.anchor, l.kind, l.line
		FROM links l JOIN files f ON f.id = l.src_file_id
		ORDER BY f.rel_path, l.line`)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()

	var out []LinkRow
	for rows.Next() {
		var r LinkRow
		if err := rows.Scan(&r.SrcRelPath, &r.Target, &r.Anchor, &r.Kind, &r.Line); err != nil {
			return nil, err
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

// FileHeadings returns, per indexed file rel-path, the set of section-anchor
// slugs that file defines — the valid targets for a link's #fragment. Slugs
// come from the stored heading breadcrumbs via the same slugifier the chunker
// uses for keys, so no re-parse is needed. Files with no headings are absent
// from the map (any #anchor into them is unresolvable).
func (s *Store) FileHeadings() (map[string]map[string]bool, error) {
	rows, err := s.db.Query(`
		SELECT DISTINCT f.rel_path, c.heading_path
		FROM chunks c JOIN files f ON f.id = c.file_id
		WHERE c.heading_path <> ''`)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()

	out := map[string]map[string]bool{}
	for rows.Next() {
		var rel, heading string
		if err := rows.Scan(&rel, &heading); err != nil {
			return nil, err
		}
		leaf := chunk.HeadingLeaf(heading)
		if leaf == "" {
			continue
		}
		set := out[rel]
		if set == nil {
			set = map[string]bool{}
			out[rel] = set
		}
		set[chunk.AnchorSlug(leaf)] = true
	}
	return out, rows.Err()
}

// Stats is index size and recency, for `semantic status`.
type Stats struct {
	Files       int
	Chunks      int
	LastIndexed time.Time
}

// Stats reports index size and recency for `semantic status`.
func (s *Store) Stats() (Stats, error) {
	var st Stats
	if err := s.db.QueryRow(`SELECT COUNT(*) FROM files`).Scan(&st.Files); err != nil {
		return st, err
	}
	if err := s.db.QueryRow(`SELECT COUNT(*) FROM chunks`).Scan(&st.Chunks); err != nil {
		return st, err
	}
	var maxIndexed sql.NullInt64
	if err := s.db.QueryRow(`SELECT MAX(indexed_at) FROM files`).Scan(&maxIndexed); err != nil {
		return st, err
	}
	if maxIndexed.Valid {
		st.LastIndexed = time.Unix(0, maxIndexed.Int64)
	}
	return st, nil
}

// encodeVec packs a float32 vector into a little-endian byte blob.
func encodeVec(v embed.Vec) []byte {
	b := make([]byte, len(v)*4)
	for i, f := range v {
		binary.LittleEndian.PutUint32(b[i*4:], math.Float32bits(f))
	}
	return b
}

// decodeVec unpacks a little-endian byte blob back into a float32 vector.
func decodeVec(b []byte) embed.Vec {
	v := make(embed.Vec, len(b)/4)
	for i := range v {
		v[i] = math.Float32frombits(binary.LittleEndian.Uint32(b[i*4:]))
	}
	return v
}

// escapeLike escapes LIKE wildcards in a user-supplied path prefix so a `%`
// or `_` in a directory name is matched literally (paired with ESCAPE '\').
func escapeLike(s string) string {
	var b []byte
	for i := 0; i < len(s); i++ {
		switch s[i] {
		case '\\', '%', '_':
			b = append(b, '\\')
		}
		b = append(b, s[i])
	}
	return string(b)
}
