package chunk

import (
	"strings"
	"testing"
)

func TestSimplify(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name string
		in   string
		want string
	}{
		{
			name: "inline link keeps text drops url",
			in:   "See the [setup guide](../../docs/setup.md#install) first.",
			want: "See the setup guide first.",
		},
		{
			name: "link with title",
			in:   `Read [the docs](https://x.example "Docs Home").`,
			want: "Read the docs.",
		},
		{
			name: "image reduces to alt",
			in:   "Diagram: ![architecture overview](img/arch.png)",
			want: "Diagram: architecture overview",
		},
		{
			name: "wikilink target only",
			in:   "Related: [[design-notes]].",
			want: "Related: design-notes.",
		},
		{
			name: "wikilink with label uses label",
			in:   "Related: [[design-notes|the design]].",
			want: "Related: the design.",
		},
		{
			name: "html comment removed",
			in:   "Alpha <!-- semantic-ignore-next-line --> beta",
			want: "Alpha  beta",
		},
		{
			name: "generated toc block removed whole",
			in:   "# Title\n\n<!-- toc -->\n\n- Setup\n- Usage\n\n<!-- tocstop -->\n\n## Setup",
			want: "# Title\n\n## Setup",
		},
		{
			name: "ordinary markdown untouched",
			in:   "## Heading\n\n- a list\n- with `code` and **bold**\n\n```go\nx := 1\n```",
			want: "## Heading\n\n- a list\n- with `code` and **bold**\n\n```go\nx := 1\n```",
		},
		{
			name: "multiple links on one line",
			in:   "[a](1) then [b](2) then [c](3)",
			want: "a then b then c",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			if got := Simplify(tc.in); got != tc.want {
				t.Errorf("Simplify(%q) =\n%q\nwant\n%q", tc.in, got, tc.want)
			}
		})
	}
}

// TestDocument_StripsLinkSyntax checks that Simplify is actually wired into the
// chunker: a section's embedded text carries the link's words, not its URL.
func TestDocument_SimplifiesChunkText(t *testing.T) {
	t.Parallel()
	body := "# Guide\n\n## Setup\n\nFollow the [install steps](../../setup/install.md#go) and the notes.\n"
	got := chunkBody(body)
	narrow := findContaining(got, "/setup/", VariantNarrow)
	if narrow.Key == "" {
		t.Fatalf("no narrow chunk for Setup; keys %v", keysOf(got))
	}
	if want := "Follow the install steps and the notes."; !strings.Contains(narrow.Text, want) {
		t.Errorf("narrow text = %q, want it to contain %q", narrow.Text, want)
	}
	if strings.Contains(narrow.Text, "install.md") || strings.Contains(narrow.Text, "](") {
		t.Errorf("link URL/syntax leaked into embedded text: %q", narrow.Text)
	}
}
