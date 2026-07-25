#!/bin/bash
#MISE description="[Compliance] Scan dependencies for known vulnerabilities"
set -euo pipefail

# govulncheck reports only vulnerabilities whose affected symbol is actually
# reachable from this module's code, so a CVE in an unused corner of a
# dependency does not fail the build. That precision is what makes it safe to
# gate on: a finding here means a call path exists, not merely that a scanner
# matched a version number.
#
# It complements `mise run licenses`. That task asks whether a dependency is
# legally safe to ship; this one asks whether it is currently safe to run.

GOVULNCHECK_VERSION="v1.1.4"

# onnxruntime_go links via cgo, so the build graph does not resolve without it.
export CGO_ENABLED=1

BIN_DIR="${XDG_CACHE_HOME:-$HOME/.cache}/semantic-tools/bin"
mkdir -p "$BIN_DIR"

if [[ ! -x "$BIN_DIR/govulncheck" ]]; then
  echo "==> installing govulncheck $GOVULNCHECK_VERSION"
  GOBIN="$BIN_DIR" go install "golang.org/x/vuln/cmd/govulncheck@$GOVULNCHECK_VERSION"
fi

echo "==> scanning for reachable vulnerabilities"
# This reaches the network: the vulnerability database lives at vuln.go.dev.
# It is the only task in this repo that does; semantic itself reaches the network
# only once, in `semantic init`.
"$BIN_DIR/govulncheck" ./...
