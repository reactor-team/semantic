#!/bin/bash
#MISE description="[Development] Download modules and verify go.mod/go.sum are tidy"
set -euo pipefail

# go.mod's `go` directive must equal the pin in /.go-version.
#
# mise reads go.mod as well as .go-version when it resolves the toolchain, and
# it prefers go.mod. A go.mod sitting below the pin therefore selects the older
# toolchain silently. That is not a style problem: it is how CI ran on 1.26.0 while
# .go-version asked for 1.26.5, which failed govulncheck against a standard
# library whose fixes landed in 1.26.1.
#
# `go mod tidy` raises this directive to the highest version any dependency
# requires, but it preserves one set higher — so the two can be kept equal.
pinned=$(tr -d '[:space:]' < "$(git rev-parse --show-toplevel)/.go-version")
declared=$(awk '$1 == "go" { print $2; exit }' go.mod)
if [[ "$pinned" != "$declared" ]]; then
  echo "--- ⚠️  go.mod's go directive does not match the pin" >&2
  echo "      .go-version: $pinned" >&2
  echo "      go.mod:      $declared" >&2
  echo "    mise resolves the toolchain from go.mod first, so these must agree." >&2
  exit 1
fi

go mod download
go mod tidy
if ! git diff --quiet -- go.mod go.sum 2>/dev/null; then
  echo "--- ⚠️  go.mod/go.sum changed after tidy — commit the result" >&2
  git diff -- go.mod go.sum >&2 || true
  exit 1
fi
echo "--- ✅ modules tidy"
