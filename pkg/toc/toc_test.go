// Copyright (c) 2026 Reactor Technologies, Inc.
// SPDX-License-Identifier: Apache-2.0

package toc

import (
	"strings"
	"testing"
)

func TestGenerate_NestsAndSkipsTitle(t *testing.T) {
	t.Parallel()
	content := `# Title

Intro.

## Setup

### Prereqs

## Usage
`
	got := Generate(content)
	want := "- Setup\n" +
		"  - Prereqs\n" +
		"- Usage"
	if got != want {
		t.Fatalf("Generate =\n%q\nwant\n%q", got, want)
	}
}

func TestGenerate_SkipsContentsHeading(t *testing.T) {
	t.Parallel()
	content := `# Title

## Contents

- Real Section

## Real Section
`
	got := Generate(content)
	want := "- Real Section"
	if got != want {
		t.Fatalf("Generate =\n%q\nwant\n%q", got, want)
	}
}

func TestGenerate_DuplicateHeadingsRepeatVerbatim(t *testing.T) {
	t.Parallel()
	content := `# Title

## Notes

## Notes

## Notes
`
	got := Generate(content)
	want := "- Notes\n" +
		"- Notes\n" +
		"- Notes"
	if got != want {
		t.Fatalf("Generate =\n%q\nwant\n%q", got, want)
	}
}

func TestGenerate_SkipsSetextHeadings(t *testing.T) {
	t.Parallel()
	// A prose line immediately followed by `---` is a setext H2 to CommonMark,
	// but the author meant `---` as a divider. It must not land in the TOC —
	// only the real ATX headings do. (Regression: SKILL.md / PIPELINE_PATTERN.md
	// captured such prose lines as headings.)
	content := `# Title

## Real Section

Some prose that happens to sit above a divider.
---

## Another Section
`
	got := Generate(content)
	want := "- Real Section\n" +
		"- Another Section"
	if got != want {
		t.Fatalf("Generate =\n%q\nwant\n%q", got, want)
	}
}

func TestGenerate_TopLevelH2WhenNoTitle(t *testing.T) {
	t.Parallel()
	// A file whose shallowest heading is H2 (no H1 title) still tabulates,
	// nesting relative to that shallowest level.
	content := `## Alpha

### Beta

## Gamma
`
	got := Generate(content)
	want := "- Alpha\n" +
		"  - Beta\n" +
		"- Gamma"
	if got != want {
		t.Fatalf("Generate =\n%q\nwant\n%q", got, want)
	}
}

func TestGenerate_IgnoresFrontmatterComments(t *testing.T) {
	t.Parallel()
	content := `---
title: Thing
# this is a yaml comment, not a heading
tags: [a, b]
---

# Real Title

## Section
`
	got := Generate(content)
	want := "- Section"
	if got != want {
		t.Fatalf("Generate =\n%q\nwant\n%q", got, want)
	}
}

func TestGenerate_Empty(t *testing.T) {
	t.Parallel()
	if got := Generate("# Only a title\n\nText.\n"); got != "" {
		t.Fatalf("Generate = %q, want empty", got)
	}
}

func TestRewrite_ReplacesListUnderContents(t *testing.T) {
	t.Parallel()
	content := `# Title

## Contents

- stale junk

## Setup

## Usage
`
	out, changed, err := Rewrite(content)
	if err != nil {
		t.Fatal(err)
	}
	if !changed {
		t.Fatal("expected changed = true")
	}
	if strings.Contains(out, "stale junk") {
		t.Errorf("stale content survived:\n%s", out)
	}
	if strings.Contains(out, "<!--") {
		t.Errorf("no HTML comment markers should be emitted:\n%s", out)
	}
	if !strings.Contains(out, "- Setup") || !strings.Contains(out, "- Usage") {
		t.Errorf("fresh TOC missing:\n%s", out)
	}
}

func TestRewrite_CleansUpStaleLinkFormTOC(t *testing.T) {
	t.Parallel()
	// A TOC committed in the older `- [text](#anchor)` link form is stale
	// against the current plain-text outline; Rewrite must replace it, leaving
	// no link syntax behind.
	content := `# Title

## Contents

- [Setup](#setup)
  - [Prereqs](#prereqs)
- [Usage](#usage)

## Setup

### Prereqs

## Usage
`
	if a := Inspect(content); !a.HasBlock || a.UpToDate {
		t.Fatalf("link-form list should read as stale (HasBlock=%v UpToDate=%v)", a.HasBlock, a.UpToDate)
	}

	out, changed, err := Rewrite(content)
	if err != nil {
		t.Fatal(err)
	}
	if !changed {
		t.Fatal("expected changed = true for a stale link-form TOC")
	}
	if strings.Contains(out, "](#") {
		t.Errorf("old link syntax survived the rewrite:\n%s", out)
	}
	want := "## Contents\n\n- Setup\n  - Prereqs\n- Usage\n\n## Setup"
	if !strings.Contains(out, want) {
		t.Errorf("plain-text TOC not written; got:\n%s", out)
	}
	// And it's now current — a second pass is a no-op.
	if _, changed, _ := Rewrite(out); changed {
		t.Errorf("rewrite not idempotent after cleanup:\n%s", out)
	}
}

func TestRewrite_Idempotent(t *testing.T) {
	t.Parallel()
	content := `# Title

## Contents

## Setup

## Usage
`
	first, _, err := Rewrite(content)
	if err != nil {
		t.Fatal(err)
	}
	second, changed, err := Rewrite(first)
	if err != nil {
		t.Fatal(err)
	}
	if changed {
		t.Errorf("second rewrite changed content:\n%s", second)
	}
	if first != second {
		t.Errorf("rewrite not idempotent:\nfirst:\n%s\nsecond:\n%s", first, second)
	}
}

func TestRewrite_InsertsUnderContentsHeading(t *testing.T) {
	t.Parallel()
	content := `# Title

## Contents

## Setup
`
	out, changed, err := Rewrite(content)
	if err != nil {
		t.Fatal(err)
	}
	if !changed {
		t.Fatal("expected changed")
	}
	if !strings.Contains(out, "## Contents\n\n- Setup") {
		t.Errorf("list not placed under Contents heading:\n%s", out)
	}
	// Re-running is a no-op.
	if _, changed, _ := Rewrite(out); changed {
		t.Errorf("re-inserting changed content:\n%s", out)
	}
}

func TestRewrite_CreatesContentsSectionAfterIntro(t *testing.T) {
	t.Parallel()
	content := `# Title

Intro paragraph.

## Setup
`
	out, changed, err := Rewrite(content)
	if err != nil {
		t.Fatal(err)
	}
	if !changed {
		t.Fatal("expected changed")
	}
	// The Contents block lands after the lede paragraph, before the first section.
	if !strings.Contains(out, "Intro paragraph.\n\n## Contents\n\n- Setup") {
		t.Errorf("Contents not placed after the intro paragraph:\n%s", out)
	}
	if !strings.HasPrefix(out, "# Title\n\nIntro paragraph.") {
		t.Errorf("title/intro order changed:\n%s", out)
	}
	// Re-running is a no-op.
	if _, changed, _ := Rewrite(out); changed {
		t.Errorf("re-inserting changed content:\n%s", out)
	}
}

func TestRewrite_ContentsAfterTitleWhenNoIntro(t *testing.T) {
	t.Parallel()
	content := `# Title

## Setup

## Usage
`
	out, changed, err := Rewrite(content)
	if err != nil {
		t.Fatal(err)
	}
	if !changed {
		t.Fatal("expected changed")
	}
	// With no lede, the block sits right after the title.
	if !strings.HasPrefix(out, "# Title\n\n## Contents\n\n- Setup") {
		t.Errorf("Contents should follow the title directly when there's no intro:\n%s", out)
	}
}

func TestRewrite_NoHeadings(t *testing.T) {
	t.Parallel()
	if _, _, err := Rewrite("# Only a title\n\nText.\n"); err == nil {
		t.Fatal("expected error when there is nothing to tabulate")
	}
}

func TestInspect(t *testing.T) {
	t.Parallel()
	current := `# Title

## Contents

- Setup

## Setup
`
	a := Inspect(current)
	if !a.HasBlock {
		t.Error("HasBlock = false, want true")
	}
	if !a.UpToDate {
		t.Errorf("UpToDate = false, want true; list should match generated")
	}
	if a.Entries != 1 {
		t.Errorf("Entries = %d, want 1", a.Entries)
	}

	stale := `# Title

## Contents

- old

## Setup
`
	if a := Inspect(stale); a.UpToDate {
		t.Error("stale list reported UpToDate")
	}

	none := "# Title\n\n## Setup\n"
	if a := Inspect(none); a.HasBlock {
		t.Error("HasBlock = true for a file with no Contents heading")
	}
}
