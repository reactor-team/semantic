#!/bin/bash
#MISE description="[Compliance] Fail if a dependency is copyleft or the license files are damaged"
set -euo pipefail

# This is the license-defensibility gate. semantic ships under Apache-2.0, and
# that stays true only while two things hold: every dependency is permissively
# licensed, and every source file still declares its own grant. A one-time audit
# rots the moment someone runs `go get`, so both are checked here and in CI.
#
# Reading a license from text is a fuzzy match, not a lookup, so an
# unclassifiable dependency is treated as a failure rather than a pass. That is
# the whole point: "we could not tell" is exactly the case a compliance gate
# exists to surface.

GO_LICENSES_VERSION="v1.6.0"

# Confidence the text classifier must reach before it names a license.
# go-licenses defaults to 0.90, which rejects the Go-project-style BSD variant
# that the modernc.org modules carry — their LICENSE files are plainly BSD-2,
# just worded far enough from the canonical template to miss the cut. 0.85
# admits them while staying well clear of a coin flip. Raising this is safe;
# lowering it past ~0.8 starts guessing.
CONFIDENCE="0.85"

# Everything Apache-2.0 can absorb without adding obligations to a downstream
# user. The four excluded types are the reason this gate exists:
#   forbidden  — AGPL, SSPL and friends: incompatible, full stop.
#   restricted — GPL/LGPL: strong copyleft, reaches the whole work.
#   reciprocal — MPL/EPL: file-level copyleft; survivable, but it is a decision
#                a human makes deliberately, not one a `go get` makes silently.
#   unknown    — unclassifiable. See the note above.
DISALLOWED="forbidden,restricted,reciprocal,unknown"

REPORT_ONLY=0
if [[ "${1:-}" == "--report" ]]; then
  REPORT_ONLY=1
fi

# go-licenses walks the real build graph, so it needs the same cgo setting the
# rest of the build uses or it cannot resolve the onnxruntime and tree-sitter
# packages at all.
export CGO_ENABLED=1

# go-licenses resolves the build graph by shelling out to `go list`, and it is
# sensitive to which toolchain answers. Against a Go other than the pinned one it
# does not fail cleanly — it reports "does not have module info" for several
# dozen *stdlib* packages and exits non-zero, which reads like a licensing
# failure and is not one. Catch the mismatch here so the message is the real
# problem.
pinned="$(tr -d '[:space:]' < .go-version)"
actual="$(go env GOVERSION 2>/dev/null | sed 's/^go//')"
if [[ -n "$pinned" && "$actual" != "$pinned" ]]; then
  cat >&2 <<EOF
This check needs the pinned Go toolchain.

  pinned (.go-version): $pinned
  on PATH:              ${actual:-none}

Run it as \`mise run licenses\`, which selects the pinned toolchain. Invoking
this script directly picks up whatever Go is on PATH, and go-licenses misreports
the mismatch as dozens of stdlib "module info" errors.
EOF
  exit 1
fi

BIN_DIR="${XDG_CACHE_HOME:-$HOME/.cache}/semantic-tools/bin"
mkdir -p "$BIN_DIR"

if [[ ! -x "$BIN_DIR/go-licenses" ]]; then
  echo "==> installing go-licenses $GO_LICENSES_VERSION"
  GOBIN="$BIN_DIR" go install "github.com/google/go-licenses@$GO_LICENSES_VERSION"
fi

if [[ "$REPORT_ONLY" == "1" ]]; then
  echo "==> dependency license inventory"
  # Warnings about non-Go (cgo) sources are expected and not actionable: the C
  # files inside onnxruntime_go and go-tree-sitter are covered by their module's
  # own license, which is what the classifier already read.
  "$BIN_DIR/go-licenses" csv ./... --confidence_threshold="$CONFIDENCE" 2>/dev/null | sort
  exit 0
fi

echo "==> checking dependency licenses (disallowed: $DISALLOWED)"
# Drop the klog warnings about non-Go sources inside onnxruntime_go,
# go-tree-sitter, and x/sys. They fire on every run, say only that cgo files
# cannot themselves be walked for further imports, and bury a real failure.
# Errors (E-prefixed) and the tool's own exit code both survive the filter.
if ! "$BIN_DIR/go-licenses" check ./... \
  --disallowed_types="$DISALLOWED" \
  --confidence_threshold="$CONFIDENCE" \
  2> >(grep -vE '^(W[0-9]|/.*/pkg/mod/)' >&2); then
  cat >&2 <<'EOF'

A dependency carries a license this project cannot absorb, or one that could not
be classified at all. Do not silence this by widening DISALLOWED.

  - Copyleft dependency: find a permissive replacement. If there genuinely is
    none, that is a decision for the maintainers, not for a build fix.
  - Unclassifiable: read the module's LICENSE yourself. If it is plainly
    permissive and merely worded oddly, nudge CONFIDENCE in this script and say
    so in the commit message.

Run `mise run licenses --report` for the full inventory.
EOF
  exit 1
fi
echo "    all dependencies permissive"

echo "==> checking the license files"
# No source file carries its own header: the root LICENSE and NOTICE cover the
# whole tree, matching the convention in the org's other public repositories.
# That only holds while the pair is actually present and still says what it
# should, so the per-file check is replaced by an integrity check on the two.
for f in LICENSE NOTICE; do
  if [[ ! -s "$f" ]]; then
    echo "    missing or empty: $f" >&2
    exit 1
  fi
done
if ! grep -q "Apache License" LICENSE; then
  echo "    LICENSE is no longer the Apache License text" >&2
  exit 1
fi
if ! grep -q "Reactor Technologies" NOTICE; then
  echo "    NOTICE lost its attribution" >&2
  exit 1
fi
echo "    LICENSE and NOTICE intact"
