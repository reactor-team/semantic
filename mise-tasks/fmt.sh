#!/bin/bash
#MISE description="[Development] Format Go code"
set -euo pipefail

go fmt ./...
