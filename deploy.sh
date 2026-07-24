#!/usr/bin/env bash
# deploy.sh — refresh the live mpm-mcp binary and restart the gateway.
#
# Usage:  ./deploy.sh
#
# Pipeline:
#   1. make build                       — recompiles all four binaries to bin/
#   2. sudo install bin/mpm-mcp /usr/local/bin/mpm-mcp
#                                       — replaces the running binary on disk
#   3. openclaw gateway restart         — gateway respawns mpm-mcp with the new binary
#
# Why this script exists: the live MCP server (/usr/local/bin/mpm-mcp)
# is started once at gateway boot and only picks up a new binary on
# gateway restart. The three steps are tightly coupled — partial deploys
# leave the running process on stale code. Running them as one command
# eliminates the "built but not installed" / "installed but not
# restarted" failure modes.
#
# Sudo requirement: only the install step needs it. To skip sudo and
# just refresh bin/ (for local testing), pass --no-install.

set -euo pipefail

REPO_ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
cd "$REPO_ROOT"

INSTALL=1
for arg in "$@"; do
  case "$arg" in
    --no-install) INSTALL=0 ;;
    -h|--help)
      echo "Usage: $0 [--no-install]"
      echo "  --no-install   skip the sudo install + gateway restart (build only)"
      exit 0
      ;;
  esac
done

# 1. Build ─────────────────────────────────────────────────────────────
echo "==> Building mpm binaries (this may take a moment)..."
make build

if [[ ! -x bin/mpm-mcp ]]; then
  echo "!! bin/mpm-mcp missing or non-executable after make build" >&2
  echo "   check the build output above for errors" >&2
  exit 1
fi

BUILD_SHA=$(sha256sum bin/mpm-mcp | awk '{print $1}' | cut -c1-12)
echo "    built bin/mpm-mcp sha=${BUILD_SHA}"

if [[ $INSTALL -eq 0 ]]; then
  echo "==> --no-install set, skipping install + restart"
  exit 0
fi

# 2. Install ───────────────────────────────────────────────────────────
echo "==> Installing mpm-mcp to /usr/local/bin/ (sudo required)..."
sudo install -Dm755 bin/mpm-mcp /usr/local/bin/mpm-mcp

# 3. Restart ───────────────────────────────────────────────────────────
echo "==> Restarting OpenClaw gateway (picks up new binary)..."
openclaw gateway restart

# 4. Verify ────────────────────────────────────────────────────────────
sleep 1
LIVE_PID=$(pgrep -f /usr/local/bin/mpm-mcp | head -1 || true)
if [[ -n "$LIVE_PID" ]]; then
  LIVE_SHA=$(sha256sum /usr/local/bin/mpm-mcp | awk '{print $1}' | cut -c1-12)
  echo "==> Deployment complete."
  echo "    live mpm-mcp PID=${LIVE_PID} sha=${LIVE_SHA}"
  if [[ "$LIVE_SHA" == "$BUILD_SHA" ]]; then
    echo "    build sha matches deployed sha \u2014 unified wakes view is live."
  else
    echo "!! sha mismatch: built ${BUILD_SHA} vs deployed ${LIVE_SHA}" >&2
    exit 1
  fi
else
  echo "!! no live mpm-mcp process found after restart" >&2
  echo "   check: pgrep -f mpm-mcp, journalctl -u openclaw-gateway" >&2
  exit 1
fi