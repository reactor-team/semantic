#!/bin/bash
# Copyright (c) 2026 Reactor Technologies, Inc.
# SPDX-License-Identifier: Apache-2.0
#MISE description="[Build] go install semantic into GOPATH/bin (global CLI)"
set -euo pipefail

# Installs semantic onto $PATH (GOPATH/bin, or $GOBIN). Stamps version/sha
# like build.sh so the globally installed binary reports its provenance.
git_sha="$(git rev-parse --short=8 HEAD)"
release="${RELEASE:-v1.$(date -u +%Y%m%d).0-g${git_sha}}"

# Force GOBIN to the real GOPATH/bin so the binary lands on the machine's
# global PATH. Without this, a mise-managed Go sets GOBIN to its own
# per-version install dir (~/.local/share/mise/installs/go/<ver>/bin),
# which is NOT on PATH unless mise shims are active — so `semantic` wouldn't
# be globally reachable. GOPATH/bin is the stable, always-on-PATH location.
dest="$(go env GOPATH)/bin"

echo "--- 📦 Installing semantic ${release} → ${dest}"
CGO_ENABLED=1 GOBIN="${dest}" go install \
  -ldflags "-X main.version=${release} -X main.sha=${git_sha}" \
  ./cmd/semantic
echo "--- ✅ Installed to ${dest}/semantic"
