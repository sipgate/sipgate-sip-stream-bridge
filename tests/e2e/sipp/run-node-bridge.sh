#!/usr/bin/env bash
# BRIDGE_BIN wrapper to run the Node.js bridge as the system-under-test under the
# sipp e2e harness (run-sipp.sh defaults BRIDGE_BIN to the Go binary). Usage:
#
#   BRIDGE_BIN="$(pwd)/tests/e2e/sipp/run-node-bridge.sh" ./tests/e2e/sipp/run-sipp.sh
#
# exec replaces this shell so the harness's tracked PID is the node process
# (clean SIGTERM → graceful shutdown). Builds dist/ on demand.
set -euo pipefail
NODE_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")/../../../node" && pwd)"
cd "$NODE_DIR"
[ -f dist/index.js ] || node_modules/.bin/tsup src/index.ts --format esm --out-dir dist >/dev/null 2>&1
exec node dist/index.js
