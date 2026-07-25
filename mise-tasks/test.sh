#!/bin/bash
#MISE description="[Testing] Run unit tests with gotestsum"
set -euo pipefail

# gotestsum is mise-pinned and on the task PATH. CGO_ENABLED=1 because the
# embed package links the ONNX runtime.
echo "--- 🧪 Running unit tests"
CGO_ENABLED=1 gotestsum --format pkgname -- ./...
