// Copyright (c) 2026 Reactor Technologies, Inc.
// SPDX-License-Identifier: Apache-2.0

package search

import (
	"reflect"
	"testing"

	"github.com/reactor-team/semantic/pkg/embed"
	"github.com/reactor-team/semantic/pkg/index"
)

// fakeSource returns canned chunks and records the prefix it was queried
// with, so tests can assert the filter is threaded through.
type fakeSource struct {
	rows       []index.ChunkRow
	gotPrefix  string
	prefixSeen bool
}

func (f *fakeSource) AllChunks(prefix string) ([]index.ChunkRow, error) {
	f.gotPrefix = prefix
	f.prefixSeen = true
	return f.rows, nil
}

// constEmbed returns a fixed query vector regardless of text — the chunk
// vectors in each test are chosen relative to it.
func constEmbed(v embed.Vec) Embedder {
	return func(string) (embed.Vec, error) { return v, nil }
}

func row(rel, key string, vec embed.Vec) index.ChunkRow {
	return index.ChunkRow{RelPath: rel, Key: key, Variant: "narrow", Text: rel + "/" + key, Vec: vec}
}

func TestQuery_RanksByCosine(t *testing.T) {
	t.Parallel()
	src := &fakeSource{rows: []index.ChunkRow{
		row("c.md", "c", embed.Vec{0, 1}),           // cos 0
		row("a.md", "a", embed.Vec{1, 0}),           // cos 1
		row("b.md", "b", embed.Vec{0.7071, 0.7071}), // cos ~0.707
	}}
	hits, err := Query(src, constEmbed(embed.Vec{1, 0}), "anything", Options{})
	if err != nil {
		t.Fatal(err)
	}
	want := []string{"a.md", "b.md", "c.md"}
	if len(hits) != len(want) {
		t.Fatalf("got %d hits, want %d", len(hits), len(want))
	}
	for i, w := range want {
		if hits[i].RelPath != w {
			t.Errorf("hit[%d] = %s, want %s (order: %v)", i, hits[i].RelPath, w, paths(hits))
		}
	}
	if hits[0].Score <= hits[1].Score || hits[1].Score <= hits[2].Score {
		t.Errorf("scores not strictly descending: %v", scores(hits))
	}
}

func TestQuery_LimitCaps(t *testing.T) {
	t.Parallel()
	src := &fakeSource{rows: []index.ChunkRow{
		row("a.md", "a", embed.Vec{1, 0}),
		row("b.md", "b", embed.Vec{0.9, 0.1}),
		row("c.md", "c", embed.Vec{0.8, 0.2}),
	}}
	hits, err := Query(src, constEmbed(embed.Vec{1, 0}), "q", Options{Limit: 2})
	if err != nil {
		t.Fatal(err)
	}
	if len(hits) != 2 {
		t.Fatalf("got %d hits, want 2", len(hits))
	}
}

func TestQuery_MinScoreFilters(t *testing.T) {
	t.Parallel()
	src := &fakeSource{rows: []index.ChunkRow{
		row("a.md", "a", embed.Vec{1, 0}),  // cos 1
		row("c.md", "c", embed.Vec{0, 1}),  // cos 0, filtered out
		row("d.md", "d", embed.Vec{-1, 0}), // cos -1, filtered out
	}}
	hits, err := Query(src, constEmbed(embed.Vec{1, 0}), "q", Options{MinScore: 0.5})
	if err != nil {
		t.Fatal(err)
	}
	if len(hits) != 1 || hits[0].RelPath != "a.md" {
		t.Fatalf("MinScore should leave only a.md; got %v", paths(hits))
	}
}

func TestQuery_CollapseByFile(t *testing.T) {
	t.Parallel()
	// Two chunks from the same file; collapse keeps the higher-scoring one.
	src := &fakeSource{rows: []index.ChunkRow{
		row("a.md", "hi", embed.Vec{1, 0}),     // cos 1
		row("a.md", "lo", embed.Vec{0.5, 0.5}), // cos ~0.707
		row("b.md", "b", embed.Vec{0.9, 0.1}),
	}}
	hits, err := Query(src, constEmbed(embed.Vec{1, 0}), "q", Options{Collapse: true})
	if err != nil {
		t.Fatal(err)
	}
	if len(hits) != 2 {
		t.Fatalf("collapse should yield one hit per file (2); got %d: %v", len(hits), paths(hits))
	}
	for _, h := range hits {
		if h.RelPath == "a.md" && h.Key != "hi" {
			t.Errorf("collapse kept the wrong a.md chunk: %s (want best-scoring 'hi')", h.Key)
		}
	}
}

func TestQuery_CollapsePromotesPathToContentSibling(t *testing.T) {
	t.Parallel()
	// The /path chunk scores highest, but it carries no body. Collapse should
	// surface its content-bearing sibling (same heading) instead, so the
	// result shows a real block. The /narrow of a different heading must NOT
	// be chosen — promotion is heading-scoped.
	const heading = "# Deploy > ## Rolling"
	src := &fakeSource{rows: []index.ChunkRow{
		{RelPath: "a.md", Key: "p", Heading: heading, Variant: "path", Text: heading, Vec: embed.Vec{1, 0}},
		{RelPath: "a.md", Key: "n", Heading: heading, Variant: "narrow", Text: heading + "\n\nbatches replace pods", Vec: embed.Vec{0.8, 0.2}},
		{RelPath: "a.md", Key: "other", Heading: "# Deploy > ## Secrets", Variant: "narrow", Text: "# Deploy > ## Secrets\n\nsops", Vec: embed.Vec{0.9, 0.1}},
	}}
	hits, err := Query(src, constEmbed(embed.Vec{1, 0}), "q", Options{Collapse: true})
	if err != nil {
		t.Fatal(err)
	}
	if len(hits) != 1 {
		t.Fatalf("collapse should yield one hit for a.md; got %d: %v", len(hits), paths(hits))
	}
	if got := hits[0]; got.Variant == "path" || got.Key != "n" {
		t.Errorf("path winner should promote to its narrow sibling 'n'; got key=%q variant=%q", got.Key, got.Variant)
	}
}

func TestQuery_CollapseKeepsPathWhenNoContentSibling(t *testing.T) {
	t.Parallel()
	// A heading with only subheadings has no body sibling; the /path hit is
	// kept as-is rather than dropped.
	src := &fakeSource{rows: []index.ChunkRow{
		{RelPath: "a.md", Key: "p", Heading: "# Top", Variant: "path", Text: "# Top", Vec: embed.Vec{1, 0}},
	}}
	hits, err := Query(src, constEmbed(embed.Vec{1, 0}), "q", Options{Collapse: true})
	if err != nil {
		t.Fatal(err)
	}
	if len(hits) != 1 || hits[0].Key != "p" {
		t.Fatalf("path-only file should keep its path hit; got %v", paths(hits))
	}
}

func TestQuery_KindFilters(t *testing.T) {
	t.Parallel()
	rows := []index.ChunkRow{
		row("docs/a.md", "a", embed.Vec{1, 0}),
		row("readme.markdown", "r", embed.Vec{1, 0}),
		row("pkg/x.go", "x", embed.Vec{1, 0}),
	}
	cases := []struct {
		kind Kind
		want []string
	}{
		{KindAny, []string{"docs/a.md", "pkg/x.go", "readme.markdown"}},
		{KindDocs, []string{"docs/a.md", "readme.markdown"}},
		{KindCode, []string{"pkg/x.go"}},
	}
	for _, tc := range cases {
		src := &fakeSource{rows: rows}
		hits, err := Query(src, constEmbed(embed.Vec{1, 0}), "q", Options{Kind: tc.kind})
		if err != nil {
			t.Fatal(err)
		}
		got := paths(hits)
		if !reflect.DeepEqual(got, tc.want) {
			t.Errorf("kind %d → %v, want %v", tc.kind, got, tc.want)
		}
	}
}

func TestQuery_LangFilters(t *testing.T) {
	t.Parallel()
	rows := []index.ChunkRow{
		row("docs/a.md", "a", embed.Vec{1, 0}),
		row("pkg/x.go", "x", embed.Vec{1, 0}),
		row("api/run.py", "p", embed.Vec{1, 0}),
		row("web/app.tsx", "t", embed.Vec{1, 0}),
		row("infra/main.tf", "h", embed.Vec{1, 0}),
		row("k8s/deploy.yaml", "y", embed.Vec{1, 0}),
	}
	cases := []struct {
		name  string
		langs []string
		want  []string
	}{
		{"empty means every language", nil, []string{"api/run.py", "docs/a.md", "infra/main.tf", "k8s/deploy.yaml", "pkg/x.go", "web/app.tsx"}},
		{"one language", []string{"python"}, []string{"api/run.py"}},
		{"several", []string{"go", "yaml"}, []string{"k8s/deploy.yaml", "pkg/x.go"}},
		// .tsx is TypeScript, not a language of its own.
		{"extension family folds into one name", []string{"typescript"}, []string{"web/app.tsx"}},
		// .tf is HCL. Filtering follows the same table the indexer used, or a
		// language would be indexable but not findable.
		{"hcl covers terraform", []string{"hcl"}, []string{"infra/main.tf"}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			src := &fakeSource{rows: rows}
			hits, err := Query(src, constEmbed(embed.Vec{1, 0}), "q", Options{Langs: tc.langs})
			if err != nil {
				t.Fatal(err)
			}
			if got := paths(hits); !reflect.DeepEqual(got, tc.want) {
				t.Errorf("langs %v → %v, want %v", tc.langs, got, tc.want)
			}
		})
	}
}

// Kind and Langs both narrow, so both must hold. Asking for docs and Python at
// once is contradictory and must return nothing rather than either half.
func TestQuery_KindAndLangBothApply(t *testing.T) {
	t.Parallel()
	src := &fakeSource{rows: []index.ChunkRow{
		row("docs/a.md", "a", embed.Vec{1, 0}),
		row("api/run.py", "p", embed.Vec{1, 0}),
	}}
	hits, err := Query(src, constEmbed(embed.Vec{1, 0}), "q", Options{Kind: KindDocs, Langs: []string{"python"}})
	if err != nil {
		t.Fatal(err)
	}
	if len(hits) != 0 {
		t.Errorf("contradictory filters returned %v, want none", paths(hits))
	}
}

func TestQuery_PassesPathPrefix(t *testing.T) {
	t.Parallel()
	src := &fakeSource{rows: []index.ChunkRow{row("docs/a.md", "a", embed.Vec{1, 0})}}
	if _, err := Query(src, constEmbed(embed.Vec{1, 0}), "q", Options{PathPrefix: "docs/"}); err != nil {
		t.Fatal(err)
	}
	if !src.prefixSeen || src.gotPrefix != "docs/" {
		t.Errorf("prefix not threaded to source: seen=%v got=%q", src.prefixSeen, src.gotPrefix)
	}
}

func TestQuery_EmptyQueryErrors(t *testing.T) {
	t.Parallel()
	src := &fakeSource{}
	if _, err := Query(src, constEmbed(embed.Vec{1, 0}), "   ", Options{}); err == nil {
		t.Error("empty query should error")
	}
}

func TestQuery_EmptyIndexNoHits(t *testing.T) {
	t.Parallel()
	src := &fakeSource{rows: nil}
	hits, err := Query(src, constEmbed(embed.Vec{1, 0}), "q", Options{})
	if err != nil {
		t.Fatalf("empty index should not error; got %v", err)
	}
	if len(hits) != 0 {
		t.Errorf("empty index should yield no hits; got %d", len(hits))
	}
}

func paths(hits []Hit) []string {
	out := make([]string, len(hits))
	for i, h := range hits {
		out[i] = h.RelPath
	}
	return out
}

func scores(hits []Hit) []float64 {
	out := make([]float64, len(hits))
	for i, h := range hits {
		out[i] = h.Score
	}
	return out
}
