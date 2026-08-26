#!/usr/bin/env bash
set -euo pipefail

ROOT_DIR=$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)
exec "$ROOT_DIR/scripts/verify-darwin.sh" "${1:-$ROOT_DIR/cap-token-usage-tracker.dylib}" amd64
