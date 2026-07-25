#!/bin/bash
# Copyright (c) 2026 Reactor Technologies, Inc.
# SPDX-License-Identifier: Apache-2.0
#MISE description="[Development] Lint Go, shell, and GitHub Actions"
set -euo pipefail

# All three linters are mise-pinned and on the task PATH, so they are called
# without mise exec. They run in one task rather than three because the shell
# scripts and the workflows are as much a part of the build as the Go is: a
# broken task script or a retired runner label fails just as loudly, and only
# much later.
echo "==> golangci-lint"
golangci-lint run ./...

# -x follows `source`d files so a shared helper is analysed too.
echo "==> shellcheck"
shellcheck -x mise-tasks/*.sh

# actionlint checks workflow syntax, expression types, and runner labels — the
# last of which GitHub retires without warning.
echo "==> actionlint"
actionlint
