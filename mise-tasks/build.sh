#!/bin/bash
# Copyright (c) 2026 Reactor Technologies, Inc.
# SPDX-License-Identifier: Apache-2.0
#MISE description="[Build] Build the semantic binary into bin/"
set -euo pipefail

# Calendar version, derived here rather than sourced from the monorepo helper
# this repo no longer sits in. Both version and sha are stamped in (sha is the
# bare commit, so a clean release build still reports its commit even when
# RELEASE carries no -g<sha>).
git_sha="$(git rev-parse --short=8 HEAD)"
release="${RELEASE:-v1.$(date -u +%Y%m%d).0-g${git_sha}}"
goos="$(go env GOOS)"
goarch="$(go env GOARCH)"
target="bin/semantic_${goos}-${goarch}"

# CGO is REQUIRED: onnxruntime_go's non-Windows path links -ldl via cgo to
# dlopen the ONNX runtime. modernc.org/sqlite is pure Go, so cgo is only for
# the embedding side.
mkdir -p bin
echo "--- 🛠️ Building semantic ${release} (${goos}/${goarch})"
CGO_ENABLED=1 GOOS="${goos}" GOARCH="${goarch}" go build \
  -ldflags "-X main.version=${release} -X main.sha=${git_sha}" \
  -o "${target}" \
  ./cmd/semantic
echo "--- ✅ Built: ${target}"
