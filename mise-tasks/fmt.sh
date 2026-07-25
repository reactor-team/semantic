#!/bin/bash
# Copyright (c) 2026 Reactor Technologies, Inc.
# SPDX-License-Identifier: Apache-2.0
#MISE description="[Development] Format Go code"
set -euo pipefail

go fmt ./...
