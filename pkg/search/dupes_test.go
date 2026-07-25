// Copyright (c) 2026 Reactor Technologies, Inc.
// SPDX-License-Identifier: Apache-2.0

package search

import (
	"testing"

	"github.com/reactor-team/semantic/pkg/chunk"
	"github.com/reactor-team/semantic/pkg/embed"
	"github.com/reactor-team/semantic/pkg/index"
)

// vrow is row() with an explicit variant, so tests can exercise the
// content-variant filter that dupe detection applies.
func vrow(rel, key, variant string, vec embed.Vec) index.ChunkRow {
	return index.ChunkRow{RelPath: rel, Key: key, Variant: variant, Text: rel + "/" + key, Vec: vec}
}

func TestDuplicates_ReportsCrossFileNearDupes(t *testing.T) {
	t.Parallel()
	src := &fakeSource{rows: []index.ChunkRow{
		vrow("a.md", "a", chunk.VariantNarrow, embed.Vec{1, 0}), // identical to b → cos 1
		vrow("b.md", "b", chunk.VariantNarrow, embed.Vec{1, 0}),
		vrow("c.md", "c", chunk.VariantNarrow, embed.Vec{0, 1}), // orthogonal → cos 0
	}}
	pairs, err := Duplicates(src, DupeOptions{MinScore: 0.9})
	if err != nil {
		t.Fatal(err)
	}
	if len(pairs) != 1 {
		t.Fatalf("want exactly the a/b pair; got %d pairs", len(pairs))
	}
	// Canonicalized: A sorts before B by rel-path.
	if pairs[0].A.RelPath != "a.md" || pairs[0].B.RelPath != "b.md" {
		t.Errorf("pair endpoints = %s,%s; want a.md,b.md", pairs[0].A.RelPath, pairs[0].B.RelPath)
	}
	if pairs[0].Score < 0.99 {
		t.Errorf("identical vectors should score ~1; got %.4f", pairs[0].Score)
	}
}

func TestDuplicates_CrossFileOnlyByDefault(t *testing.T) {
	t.Parallel()
	src := &fakeSource{rows: []index.ChunkRow{
		vrow("a.md", "one", chunk.VariantNarrow, embed.Vec{1, 0}),
		vrow("a.md", "two", chunk.VariantNarrow, embed.Vec{1, 0}), // same file as above
		vrow("b.md", "b", chunk.VariantNarrow, embed.Vec{1, 0}),
	}}

	def, err := Duplicates(src, DupeOptions{MinScore: 0.9})
	if err != nil {
		t.Fatal(err)
	}
	// a/two↔a/one is within-file and dropped; a↔b and b↔a-two survive as
	// two cross-file pairs.
	if len(def) != 2 {
		t.Fatalf("default should exclude the within-file pair; got %d", len(def))
	}
	for _, p := range def {
		if p.A.RelPath == p.B.RelPath {
			t.Errorf("within-file pair leaked into default output: %s", p.A.RelPath)
		}
	}

	within, err := Duplicates(src, DupeOptions{MinScore: 0.9, WithinFile: true})
	if err != nil {
		t.Fatal(err)
	}
	if len(within) != 3 {
		t.Fatalf("WithinFile should add the a/one↔a/two pair (3 total); got %d", len(within))
	}
}

func TestDuplicates_SkipsNavigationalVariants(t *testing.T) {
	t.Parallel()
	// path/title/full are excluded as comparison units even when identical.
	src := &fakeSource{rows: []index.ChunkRow{
		vrow("a.md", "a", chunk.VariantPath, embed.Vec{1, 0}),
		vrow("b.md", "b", chunk.VariantTitle, embed.Vec{1, 0}),
		vrow("c.md", "c", chunk.VariantFull, embed.Vec{1, 0}),
	}}
	pairs, err := Duplicates(src, DupeOptions{MinScore: 0.9})
	if err != nil {
		t.Fatal(err)
	}
	if len(pairs) != 0 {
		t.Fatalf("navigational/full variants must not form dupe pairs; got %d", len(pairs))
	}
}

func TestDuplicates_SortsByScoreDescAndCaps(t *testing.T) {
	t.Parallel()
	src := &fakeSource{rows: []index.ChunkRow{
		vrow("a.md", "a", chunk.VariantNarrow, embed.Vec{1, 0}),
		vrow("b.md", "b", chunk.VariantNarrow, embed.Vec{1, 0}),        // a↔b: cos 1
		vrow("c.md", "c", chunk.VariantNarrow, embed.Vec{0.95, 0.312}), // ~0.95 vs a,b
	}}
	pairs, err := Duplicates(src, DupeOptions{MinScore: 0.9, Limit: 2})
	if err != nil {
		t.Fatal(err)
	}
	if len(pairs) != 2 {
		t.Fatalf("Limit should cap at 2; got %d", len(pairs))
	}
	if pairs[0].Score < pairs[1].Score {
		t.Errorf("pairs not sorted by descending score: %.4f then %.4f", pairs[0].Score, pairs[1].Score)
	}
	if pairs[0].A.RelPath != "a.md" || pairs[0].B.RelPath != "b.md" {
		t.Errorf("top pair should be the identical a/b; got %s,%s", pairs[0].A.RelPath, pairs[0].B.RelPath)
	}
}
