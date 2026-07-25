// Copyright (c) 2026 Reactor Technologies, Inc.
// SPDX-License-Identifier: Apache-2.0

package chunk

import (
	"strings"
	"testing"
)

// find returns the first chunk whose key has the given suffix, or the zero
// Chunk. Used to assert on a specific variant without pinning the heading
// index.
func find(chunks []Chunk, keySuffix string) Chunk {
	for _, c := range chunks {
		if strings.HasSuffix(c.Key, keySuffix) {
			return c
		}
	}
	return Chunk{}
}

func findContaining(chunks []Chunk, keySub, variant string) Chunk {
	for _, c := range chunks {
		if strings.Contains(c.Key, keySub) && c.Variant == variant {
			return c
		}
	}
	return Chunk{}
}

func keysOf(chunks []Chunk) []string {
	out := make([]string, 0, len(chunks))
	for _, c := range chunks {
		out = append(out, c.Key)
	}
	return out
}

func TestChunkBody_SingleHeading(t *testing.T) {
	t.Parallel()
	got := chunkBody("# Alice\n\nShe joined Acme in 2023.\n")

	narrow := find(got, "/narrow")
	if narrow.Key == "" {
		t.Fatalf("expected a /narrow chunk; got keys %v", keysOf(got))
	}
	if !strings.HasPrefix(narrow.Text, "# Alice\n\n") {
		t.Errorf("narrow chunk should open with breadcrumb `# Alice`; got %q", narrow.Text)
	}
	if !strings.Contains(narrow.Text, "She joined Acme in 2023.") {
		t.Errorf("narrow chunk should contain content; got %q", narrow.Text)
	}
	if narrow.Heading != "# Alice" {
		t.Errorf("narrow heading breadcrumb = %q, want `# Alice`", narrow.Heading)
	}
}

func TestChunkBody_Nested(t *testing.T) {
	t.Parallel()
	body := `# Community Perception

## content

Intro paragraph.

### Google Reviews

- mold: 12
- responsive: 8

### Facebook

Complaints.
`
	got := chunkBody(body)

	if h1 := find(got, "body/0/community-perception/path"); !strings.Contains(h1.Text, "# Community Perception") {
		t.Fatalf("H1 path chunk missing/incorrect; keys %v", keysOf(got))
	}

	gr := findContaining(got, "google-reviews", VariantNarrow)
	if gr.Key == "" {
		t.Fatalf("missing narrow chunk for Google Reviews; keys %v", keysOf(got))
	}
	if !strings.Contains(gr.Text, "# Community Perception > ## content > ### Google Reviews") {
		t.Errorf("breadcrumb missing ancestors; got %q", gr.Text)
	}
	if !strings.Contains(gr.Text, "mold: 12") {
		t.Errorf("narrow chunk should include section content; got %q", gr.Text)
	}

	contentNarrow := findContaining(got, "/content/", VariantNarrow)
	contentFull := findContaining(got, "/content/", VariantFull)
	if !strings.Contains(contentNarrow.Text, "Intro paragraph.") {
		t.Errorf("content /narrow should have the intro paragraph; got %q", contentNarrow.Text)
	}
	if strings.Contains(contentNarrow.Text, "mold: 12") {
		t.Errorf("content /narrow should NOT pull in child-section content; got %q", contentNarrow.Text)
	}
	if !strings.Contains(contentFull.Text, "mold: 12") {
		t.Errorf("content /full should roll up the Google Reviews child; got %q", contentFull.Text)
	}
	if !strings.Contains(contentFull.Text, "Complaints.") {
		t.Errorf("content /full should roll up Facebook too; got %q", contentFull.Text)
	}
}

func TestChunkBody_NoHeadings(t *testing.T) {
	t.Parallel()
	got := chunkBody("Just prose without any headings.\n\nSecond paragraph.\n")
	if len(got) != 1 {
		t.Fatalf("no-heading body should produce one chunk; got %d (%v)", len(got), keysOf(got))
	}
	if got[0].Key != VariantBody || !strings.Contains(got[0].Text, "Second paragraph.") {
		t.Errorf("expected `body` chunk with full prose; got %+v", got[0])
	}
}

func TestChunkBody_Empty(t *testing.T) {
	t.Parallel()
	if got := chunkBody(""); len(got) != 0 {
		t.Errorf("empty body should produce no chunks; got %v", keysOf(got))
	}
}

func TestDocument_LineNumbers(t *testing.T) {
	t.Parallel()
	// Frontmatter occupies lines 1-3, blank line 4, "# Alice" on line 5,
	// "## Notes" on line 9. The title chunk points at the top of the file.
	content := "---\ntitle: Alice\n---\n\n# Alice\n\nIntro.\n\n## Notes\n\nDetails.\n"
	got := Document(content)

	if title := find(got, VariantTitle); title.Line != 1 {
		t.Errorf("title chunk line = %d, want 1", title.Line)
	}
	if h1 := findContaining(got, "/alice/", VariantPath); h1.Line != 5 {
		t.Errorf("`# Alice` chunk line = %d, want 5 (keys %v)", h1.Line, keysOf(got))
	}
	if notes := findContaining(got, "/notes/", VariantNarrow); notes.Line != 9 {
		t.Errorf("`## Notes` chunk line = %d, want 9 (keys %v)", notes.Line, keysOf(got))
	}
}

func TestChunkBody_LineNumbersNoFrontmatter(t *testing.T) {
	t.Parallel()
	// Leading blank line, so "# Top" is on line 2 and "## Sub" on line 5.
	got := chunkBody("\n# Top\n\nText.\n\n## Sub\n\nMore.\n")
	if top := findContaining(got, "/top/", VariantPath); top.Line != 2 {
		t.Errorf("`# Top` line = %d, want 2 (keys %v)", top.Line, keysOf(got))
	}
	if sub := findContaining(got, "/sub/", VariantNarrow); sub.Line != 6 {
		t.Errorf("`## Sub` line = %d, want 6 (keys %v)", sub.Line, keysOf(got))
	}
}

func TestSlugifyHeading(t *testing.T) {
	t.Parallel()
	// slugifyHeading is the chunk-key generator: punctuation runs collapse to
	// '-', trimmed, capped. NOT a GitHub anchor (see TestAnchorSlug).
	cases := map[string]string{
		"":                              "section",
		"content":                       "content",
		"Google Reviews: 3.2 / 5 · 198": "google-reviews-3-2-5-198",
		"  trim me  ":                   "trim-me",
		"################":              "section",
		"Alice & Bob":                   "alice-bob",
	}
	for in, want := range cases {
		if got := slugifyHeading(in); got != want {
			t.Errorf("slugifyHeading(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestAnchorSlug(t *testing.T) {
	t.Parallel()
	// AnchorSlug must match GitHub's github-slugger: delete punctuation
	// (keeping '_'), map only spaces to '-', preserve consecutive hyphens, no
	// length cap. These are exactly where it must diverge from slugifyHeading.
	longHeading := strings.Repeat("a", 70)
	cases := map[string]string{
		"":                    "",
		"Getting Started":     "getting-started",
		"foo_bar":             "foo_bar",  // underscore kept
		"__init__":            "__init__", // underscores kept, not stripped
		"Alice & Bob":         "alice--bob",
		"Google Reviews: 3.2": "google-reviews-32",
		// Punctuation is deleted, not hyphenated: '/', '{', '}' vanish; the
		// em-dash's surrounding spaces both become hyphens (a double hyphen).
		"PUT /users/{user_id}/accounts/{account_id} — Move User to Account": "put-usersuser_idaccountsaccount_id--move-user-to-account",
		longHeading: longHeading, // no 60-char cap
	}
	for in, want := range cases {
		if got := AnchorSlug(in); got != want {
			t.Errorf("AnchorSlug(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestHeadingLeaf(t *testing.T) {
	t.Parallel()
	cases := map[string]string{
		"":                         "",
		"# Title":                  "Title",
		"# H1 > ## H2 > ### Self":  "Self",
		"# A > ## Getting Started": "Getting Started",
		"### Deep":                 "Deep",
	}
	for in, want := range cases {
		if got := HeadingLeaf(in); got != want {
			t.Errorf("HeadingLeaf(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestSplitFrontmatter(t *testing.T) {
	t.Parallel()
	t.Run("present", func(t *testing.T) {
		t.Parallel()
		fm, body := splitFrontmatter("---\ntitle: Alice\nrole: CTO\n---\n\n# Alice\n\nBody.\n")
		if fm["title"] != "Alice" || fm["role"] != "CTO" {
			t.Errorf("frontmatter not parsed: %v", fm)
		}
		if strings.Contains(body, "title:") {
			t.Errorf("body should not contain frontmatter; got %q", body)
		}
		if !strings.Contains(body, "# Alice") {
			t.Errorf("body should retain markdown; got %q", body)
		}
	})
	t.Run("absent", func(t *testing.T) {
		t.Parallel()
		content := "# Alice\n\nNo frontmatter here.\n"
		fm, body := splitFrontmatter(content)
		if len(fm) != 0 {
			t.Errorf("expected empty frontmatter; got %v", fm)
		}
		if body != content {
			t.Errorf("body should equal content; got %q", body)
		}
	})
	t.Run("malformed falls back to body", func(t *testing.T) {
		t.Parallel()
		// Opening fence, invalid YAML inside: treat the whole thing as body
		// rather than dropping the file.
		content := "---\n: : not: valid: yaml\n---\n\nBody.\n"
		fm, body := splitFrontmatter(content)
		if len(fm) != 0 {
			t.Errorf("malformed frontmatter should yield empty map; got %v", fm)
		}
		if body != content {
			t.Errorf("malformed frontmatter should keep original as body; got %q", body)
		}
	})
}

func TestDocument_TitleSynthesis(t *testing.T) {
	t.Parallel()
	t.Run("title and tags", func(t *testing.T) {
		t.Parallel()
		got := Document("---\ntitle: Quarterly Plan\ntags: [planning, q3]\n---\n\nBody prose.\n")
		title := find(got, VariantTitle)
		if title.Variant != VariantTitle {
			t.Fatalf("expected a title chunk; got keys %v", keysOf(got))
		}
		if !strings.Contains(title.Text, "Quarterly Plan") {
			t.Errorf("title chunk should carry the title; got %q", title.Text)
		}
		if !strings.Contains(title.Text, "planning") || !strings.Contains(title.Text, "q3") {
			t.Errorf("title chunk should carry tags; got %q", title.Text)
		}
	})
	t.Run("name fallback", func(t *testing.T) {
		t.Parallel()
		got := Document("---\nname: Alice\n---\n\nBody.\n")
		if title := find(got, VariantTitle); !strings.Contains(title.Text, "Alice") {
			t.Errorf("expected title synthesized from name; got %+v", title)
		}
	})
	t.Run("no title yields no title chunk", func(t *testing.T) {
		t.Parallel()
		got := Document("---\nrole: CTO\n---\n\n# Heading\n\nBody.\n")
		if title := find(got, VariantTitle); title.Variant == VariantTitle {
			t.Errorf("no frontmatter title should mean no title chunk; got %+v", title)
		}
	})
}

func TestDocument_FrontmatterNotEmbeddedAsFields(t *testing.T) {
	t.Parallel()
	// Only body/* and (synthesized) title chunks — arbitrary frontmatter
	// fields like `role` must never become their own chunk.
	content := "---\nname: Alice\nrole: CTO\n---\n\n# Alice\n\n## notes\n\nShe joined Acme.\n"
	got := Document(content)
	if len(got) == 0 {
		t.Fatal("expected chunks")
	}
	for _, c := range got {
		if !strings.HasPrefix(c.Key, "body/") && c.Variant != VariantTitle {
			t.Errorf("unexpected chunk %q (frontmatter fields should not embed)", c.Key)
		}
		if strings.Contains(c.Text, "CTO") {
			t.Errorf("frontmatter field value leaked into chunk %q: %q", c.Key, c.Text)
		}
	}
}
