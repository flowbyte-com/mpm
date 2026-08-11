#!/usr/bin/env bash
# deploy.sh — refresh the live mpm binaries and restart dependent services.
#
# Usage:  ./deploy.sh
#
# Pipeline (no sudo; canonical install is always user-writable):
#   1. make install                       — rebuild + sync to $HOME/.mpm/bin/
#   2. openclaw gateway restart           — gateway respawns mpm-mcp with new binary
#   3. systemctl --user restart mpm-scheduler  — scheduler is independent of gateway
#   4. Verify live mpm-mcp PID matches the just-built sha
#
# Why this script exists: the live MCP server is started once at gateway
# boot and only picks up a new binary on gateway restart. mpm-scheduler
# runs under user-level systemd — independent of the gateway — so a
# scheduler-touching deploy needs its own restart. Both steps are tightly
# coupled to the build. Running them as one command eliminates the
# "built but not installed" / "installed but not restarted" failure modes.
#
# Canonical install: $HOME/.mpm/bin/ (no /usr/local copy, no XDG split,
# no PATH ordering). See INSTALL.md and CONTRIBUTING.md for the alpha
# install contract.

set -euo pipefail

# Restore a sane PATH in case the caller is in a stripped-down environment
# (sudo, systemd-run, non-interactive subshell). The canonical binary
# location is $HOME/.mpm/bin which may or may not be on PATH for the
# process that invoked deploy.sh; we invoke it by absolute path.
export PATH="$HOME/.mpm/bin:$HOME/.local/bin:/usr/local/bin:/usr/bin:/bin:$PATH"

REPO_ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
cd "$REPO_ROOT"

INSTALL=1
for arg in "$@"; do
  case "$arg" in
    --no-install) INSTALL=0 ;;
    -h|--help)
      echo "Usage: $0 [--no-install]"
      echo "  --no-install   build only; skip install + service restart"
      exit 0
      ;;
  esac
done

# 1. Build + sync to canonical ─────────────────────────────────────────
echo "==> Building mpm binaries and syncing to \$HOME/.mpm/bin/..."
make install

CANONICAL_BIN="$HOME/.mpm/bin"
if [[ ! -x "$CANONICAL_BIN/mpm-mcp" ]]; then
  echo "!! $CANONICAL_BIN/mpm-mcp missing or non-executable after make install" >&2
  echo "   check the build output above for errors" >&2
  exit 1
fi

BUILD_SHA=$(sha256sum "$CANONICAL_BIN/mpm-mcp" | awk '{print $1}' | cut -c1-12)
echo "    canonical mpm-mcp sha=${BUILD_SHA} (at $CANONICAL_BIN)"

if [[ $INSTALL -eq 0 ]]; then
  echo "==> --no-install set, skipping gateway/scheduler restart"
  exit 0
fi

# 2. Restart gateway (so mpm-mcp picks up the new binary) ─────────────
echo "==> Restarting OpenClaw gateway..."
openclaw gateway restart

# 3. Restart mpm-scheduler (independent daemon) ───────────────────────
# mpm-scheduler runs under user-level systemd — NOT a child of the
# gateway. The gateway restart doesn't propagate here, so we need a
# separate restart for any deploy that touches scheduler code
# (ProcessScheduledTasks, HandlerFuncs, etc). Caught the 2026-07-24 case
# where the install landed but the scheduler kept running stale code.
# Bake it in.
SCHED_ACTIVE_BEFORE=$(systemctl --user is-active mpm-scheduler 2>/dev/null || echo "unknown")
if [[ "$SCHED_ACTIVE_BEFORE" == "active" ]]; then
  echo "==> Restarting user-level mpm-scheduler (independent of gateway)..."
  systemctl --user restart mpm-scheduler
else
  echo "==> mpm-scheduler not active (state: $SCHED_ACTIVE_BEFORE) — no restart needed"
fi

# 4. Verify live mpm-mcp is the new binary ─────────────────────────────
sleep 1
LIVE_PID=$(pgrep -f "$CANONICAL_BIN/mpm-mcp" | head -1 || true)
if [[ -n "$LIVE_PID" ]]; then
  LIVE_SHA=$(sha256sum "$CANONICAL_BIN/mpm-mcp" | awk '{print $1}' | cut -c1-12)
  echo "==> Deployment complete."
  echo "    live mpm-mcp PID=${LIVE_PID} sha=${LIVE_SHA}"
  if [[ "$LIVE_SHA" == "$BUILD_SHA" ]]; then
    echo "    build sha matches live sha — unified wakes view is live."
  else
    echo "!! sha mismatch: built ${BUILD_SHA} vs live ${LIVE_SHA}" >&2
    exit 1
  fi
else
  echo "!! no live mpm-mcp process found after restart" >&2
  echo "   check: pgrep -f mpm-mcp, openclaw gateway status" >&2
  exit 1
fi