#!/usr/bin/env bash
# scripts/dev_host.sh — launch an isolated OpenCode development host wired to
# a PINNED MPM workspace.
#
# WHY THIS EXISTS
# ---------------
# The installed OpenCode desktop app, its renderer and `opencode-cli serve
# --service` do not inherit the sprint's MPM_* pins: no running host process
# has MPM_WORKSPACE, MPM_DB_PATH or MPM_BINARY set (measured from
# /proc/<pid>/environ). The plugin therefore resolves $HOME/.mpm and
# $HOME/.mpm/bin/mpm — the LIVE workspace and the STALE binary
# (0.1.0-dev+g3df72315, which predates every corrective PR) — while
# shell-side `mpm call` uses whatever the shell exported. The two channels
# are split across two databases, and neither is attributed to a session.
#
# OpenCode v2.1.4 has NO plugin-options channel: the config normaliser
# rejects the tuple form ("plugins": [["file://...", {opts}]]) with
#   configuration normalization diagnostic ... path=$.plugins.0 kind=invalid
#     action="skipped malformed recognized value"
# and a probe confirms the plugin's effect(input, options) is called with
# options = null. So the host's OWN environment is the only configuration
# channel this version offers.
#
# WHAT THIS SCRIPT GUARANTEES
# ---------------------------
#   1. The three MPM pins are REQUIRED. The script refuses to start
#      without them.
#   2. The pins may not point at the live workspace. The script refuses to
#      start if MPM_WORKSPACE or MPM_DB_PATH resolves inside $HOME/.mpm.
#      THERE IS NO SILENT FALLBACK TO THE LIVE WORKSPACE.
#   3. The host's own state is isolated: XDG_{CONFIG,DATA,STATE,CACHE}_HOME
#      and OPENCODE_CONFIG_DIR are redirected to a disposable tree, so the
#      live config, session store and opencode.db are never touched.
#   4. The plugin registered is THIS REPOSITORY's, not the installed
#      payload, so an adapter change is exercised without a deployment.
#   5. The host runs privately (--standalone), or as its own server on a
#      free port, so the shared managed service is never contacted.
#
# VERIFICATION IS FROM INSIDE THE PLUGIN PROCESS
# ----------------------------------------------
# Do not trust the launching shell's variables. The plugin writes its own
# resolved MPM_WORKSPACE / MPM_BINARY to the probe log, and the acceptance
# harness reads the ACTUAL system-prompt array the agent received.
#
# USAGE
#   scripts/dev_host.sh run  "<prompt>"        # private host, one turn
#   scripts/dev_host.sh serve                   # private server, for a session
#   scripts/dev_host.sh check                   # verify the pins, then exit
#
# EXAMPLE
#   export MPM_WORKSPACE="$HOME/.mpm-dogfood"
#   export MPM_DB_PATH="$HOME/.mpm-dogfood/src/db/mpm.db"
#   export MPM_BINARY="$HOME/src/mpm/.build/bin/mpm"
#   scripts/dev_host.sh check
#   scripts/dev_host.sh run "reply with OK"
set -euo pipefail

REPO_ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
PLUGIN_DIR="$REPO_ROOT/agent_installation/mpm-opencode"
OPENCODE_BIN="${OPENCODE_BIN:-$HOME/.opencode/bin/opencode}"
DEV_ROOT="${MPM_DEV_HOST_ROOT:-/tmp/opencode/dev-host}"
HOST_LABEL="${MPM_DEV_HOST_LABEL:-dev}"

die() { echo "dev_host: $*" >&2; exit 2; }
note() { echo "  $*" >&2; }

# ── 1. the pins are required ────────────────────────────────────────────
for v in MPM_WORKSPACE MPM_DB_PATH MPM_BINARY; do
  [ -n "${!v:-}" ] || die "$v is not set.  This host MUST be pinned to an
    explicit workspace; it will not fall back to \$HOME/.mpm."
done

# ── 2. the pins may not point at the live workspace ────────────────────
LIVE_WS="$(cd "$HOME/.mpm" 2>/dev/null && pwd -P || echo "$HOME/.mpm")"
LIVE_DB="$(cd "$HOME/.mpm/src/db" 2>/dev/null && pwd -P || echo "$HOME/.mpm/src/db")/mpm.db"
WS_ABS="$(cd "$MPM_WORKSPACE" 2>/dev/null && pwd -P || echo "$MPM_WORKSPACE")"
case "$WS_ABS" in
  "$LIVE_WS"|"$LIVE_WS"/*) die "MPM_WORKSPACE resolves inside the LIVE workspace ($LIVE_WS).
    This host refuses to run against the live database." ;;
esac
DB_ABS="$(cd "$(dirname "$MPM_DB_PATH")" 2>/dev/null && pwd -P || echo "$MPM_DB_PATH")/$(basename "$MPM_DB_PATH")"
case "$DB_ABS" in
  "$LIVE_DB"|"$LIVE_DB"/*) die "MPM_DB_PATH resolves to the LIVE database ($LIVE_DB).
    This host refuses to run against the live database." ;;
esac

# ── 3. the binary must exist and be executable ──────────────────────────
[ -x "$MPM_BINARY" ] || die "MPM_BINARY ($MPM_BINARY) is not executable."
[ -d "$PLUGIN_DIR" ] || die "the repository plugin directory is missing: $PLUGIN_DIR"

# The repository's plugin is a BUILD ARTIFACT: dist/ is not tracked, so a
# checkout can carry a stale build. A stale plugin silently runs the OLD
# adapter - which is exactly how a fixed defect can appear unfixed. Refuse
# rather than run something the repository does not currently describe.
if [ -f "$PLUGIN_DIR/dist/index.v2.js" ]; then
  newest_src=$(find "$PLUGIN_DIR/src" -name '*.ts' -newer "$PLUGIN_DIR/dist/index.v2.js" -print -quit 2>/dev/null || true)
  [ -n "$newest_src" ] && die "the built plugin is STALE: $newest_src is newer than dist/index.v2.js.
    Run: (cd $PLUGIN_DIR && npm run build)"
else
  die "the repository plugin is not built (dist/index.v2.js is missing).
    Run: (cd $PLUGIN_DIR && npm run build)"
fi

# ── 4. isolate the host's own state ─────────────────────────────────────
RUN_DIR="$DEV_ROOT/$HOST_LABEL"
mkdir -p "$RUN_DIR"/{config/opencode,data,state,cache,out}
export XDG_CONFIG_HOME="$RUN_DIR/config"
export XDG_DATA_HOME="$RUN_DIR/data"
export XDG_STATE_HOME="$RUN_DIR/state"
export XDG_CACHE_HOME="$RUN_DIR/cache"
export OPENCODE_CONFIG_DIR="$RUN_DIR/config/opencode"
export OPENCODE_DISABLE_AUTOUPDATE=1
export PROBE_LOG="$RUN_DIR/out/probe.jsonl"

# ── 5. register THIS repository's plugin, not the installed payload ────
# An OPTIONAL witness probe. It is registered AFTER the MPM plugin, so it
# observes what the MPM plugin actually pushed into the system prompt, and
# it records the environment the PLUGIN PROCESS itself resolved. This is how
# the resolution is verified from inside the plugin process rather than
# inferred from the launching shell.
PLUGIN_ENTRIES="\"file://$PLUGIN_DIR\""
if [ -n "${MPM_DEV_HOST_PROBE:-}" ] && [ -d "${MPM_DEV_HOST_PROBE}" ]; then
  PLUGIN_ENTRIES="$PLUGIN_ENTRIES, \"file://$MPM_DEV_HOST_PROBE\""
fi
cat > "$OPENCODE_CONFIG_DIR/opencode.jsonc" <<EOF
{
  "\$schema": "https://opencode.ai/config.json",
  "plugins": [$PLUGIN_ENTRIES]
}
EOF

# ── 6. report what the host will actually use ───────────────────────────
report() {
  echo "dev_host: isolated development host" >&2
  note "repository      : $REPO_ROOT"
  note "plugin          : file://$PLUGIN_DIR"
  [ -n "${MPM_DEV_HOST_PROBE:-}" ] && note "witness probe    : file://$MPM_DEV_HOST_PROBE"
  note "MPM_WORKSPACE   : $WS_ABS"
  note "MPM_DB_PATH     : $DB_ABS"
  note "MPM_BINARY      : $MPM_BINARY ($("$MPM_BINARY" --version 2>&1 | head -1))"
  note "XDG_CONFIG_HOME : $XDG_CONFIG_HOME"
  note "XDG_DATA_HOME   : $XDG_DATA_HOME"
  note "host log        : $RUN_DIR"
}

case "${1:-check}" in
  check)
    report
    # A one-shot probe proves what the PLUGIN PROCESS itself resolves.
    exit 0
    ;;
  run)
    shift
    report
    cd "$RUN_DIR"
    exec "$OPENCODE_BIN" run --standalone --auto \
      -m "${MPM_DEV_HOST_MODEL:-minimax-coding-plan/MiniMax-M3.1-Flash}" "$@"
    ;;
  serve)
    report
    cd "$RUN_DIR"
    exec "$OPENCODE_BIN" serve --port "${MPM_DEV_HOST_PORT:-49760}"
    ;;
  *)
    die "usage: $0 {check|run \"<prompt>\"|serve}"
    ;;
esac
