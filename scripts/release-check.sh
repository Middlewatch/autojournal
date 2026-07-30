#!/usr/bin/env bash
set -euo pipefail

ROOT=$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)
cd "$ROOT"

./scripts/verify.sh
./scripts/build-adapter-binaries.sh
./scripts/verify-package.sh
