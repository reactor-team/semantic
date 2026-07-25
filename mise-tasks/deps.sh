#!/bin/bash
# Copyright (c) 2026 Reactor Technologies, Inc.
# SPDX-License-Identifier: Apache-2.0
#MISE description="[Development] Download modules and verify go.mod/go.sum are tidy"
set -euo pipefail

go mod download
go mod tidy
if ! git diff --quiet -- go.mod go.sum 2>/dev/null; then
  echo "--- ⚠️  go.mod/go.sum changed after tidy — commit the result" >&2
  git diff -- go.mod go.sum >&2 || true
  exit 1
fi
echo "--- ✅ modules tidy"
