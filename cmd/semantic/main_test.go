// Copyright (c) 2026 Reactor Technologies, Inc.
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"path/filepath"
	"testing"
)

func TestBreadcrumb(t *testing.T) {
	t.Parallel()
	cases := map[string]string{
		"":                                 "",
		"# Databases > ## Postgres":        "Databases › Postgres",
		"# Beverages":                      "Beverages",
		"# A > ## B > ### C":               "A › B › C",
		"## C# language > ### Async/await": "C# language › Async/await",
	}
	for in, want := range cases {
		if got := breadcrumb(in); got != want {
			t.Errorf("breadcrumb(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestScopePaths(t *testing.T) {
	t.Parallel()
	// No files → whole-vault mode.
	if got, err := scopePaths("/vault", nil); err != nil || got != nil {
		t.Fatalf("scopePaths(nil) = %v, %v; want nil, nil", got, err)
	}

	// Absolute inputs under the vault normalize to forward-slashed rel-paths;
	// a nested path keeps its subdir.
	got, err := scopePaths("/vault", []string{"/vault/README.md", "/vault/docs/guide.md"})
	if err != nil {
		t.Fatal(err)
	}
	want := []string{"README.md", "docs/guide.md"}
	if len(got) != len(want) {
		t.Fatalf("scopePaths = %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("scopePaths[%d] = %q, want %q", i, got[i], want[i])
		}
	}

	// A path outside the vault climbs with "..", which is a valid rel-path — it
	// simply won't match any in-vault finding.
	rel, err := scopePaths(".", []string{filepath.Join("..", "sibling.md")})
	if err != nil {
		t.Fatal(err)
	}
	if len(rel) != 1 || rel[0] != "../sibling.md" {
		t.Errorf("scopePaths outside vault = %v, want [../sibling.md]", rel)
	}
}

// TestVersionString does not call t.Parallel: it swaps the package-level stamps,
// which a parallel sibling could observe. The build-info fallback is not
// unit-testable, because a test binary carries no module version.
func TestVersionString(t *testing.T) {
	origVersion, origSHA := version, sha
	t.Cleanup(func() { version, sha = origVersion, origSHA })

	version, sha = "v9.9.9", "deadbeef"
	if got, want := versionString(), "v9.9.9 (deadbeef)"; got != want {
		t.Errorf("versionString() = %q, want %q", got, want)
	}

	// A module fetched by version carries no revision, so none is printed.
	version, sha = "v9.9.9", ""
	if got, want := versionString(), "v9.9.9"; got != want {
		t.Errorf("versionString() = %q, want %q", got, want)
	}

	// With no stamp, the fallback reads the build info rather than printing "dev".
	version, sha = "dev", ""
	if got := versionString(); got == "" {
		t.Error("versionString() is empty with no stamp")
	}
}
