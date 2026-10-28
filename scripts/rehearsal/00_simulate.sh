#!/usr/bin/env bash
# rehearsal/00_simulate.sh — build an ISOLATED simulation of the currently
# installed runtime.
#
# The simulation reproduces the shape of the live installation:
#
#   ~/.mpm/                     a Git CHECKOUT that is also the binary
#                               prefix, the data root and the config root
#     .git/                     a real Git repository
#     bin/                      the five binaries (ELF files)
#     src/db/mpm.db             a SQLite database in WAL mode
#     mpm_config.json           operator configuration (a SYNTHETIC key)
#     mode/ persona/ drills/    operator-owned definitions
#     agent_installation/       the full adapter SOURCE tree, with the
#                               plugin payload built inside it
#   ~/.local/bin/mpm            the PATH symlink
#   ~/.config/opencode/         the host config registering the plugin
#   ~/.config/systemd/user/     the scheduler unit
#
# Everything is under a DISPOSABLE HOME. The live tree is never written to,
# and the live credentials are never copied: `mpm_config.json` here is
# synthetic, with a placeholder key.

set -uo pipefail

REH_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd -P)"
. "$REH_DIR/lib.sh"

SIM_HOME="${SIM_HOME:-$REH_WORK/home}"
SIM_WS="$SIM_HOME/.mpm"
SRC_TREE="${SRC_TREE:-$REH_WORK/src-$REH_REV}"
LIVE_BIN="${LIVE_BIN:-$HOME/.mpm/bin}"

echo "══════════════════════════════════════════════════════════════"
echo "  SIMULATION — an isolated checkout-style MPM installation"
echo "══════════════════════════════════════════════════════════════"
echo "  disposable HOME : $SIM_HOME"
echo "  simulated ws    : $SIM_WS"
echo

safe_rm "$SIM_HOME"
mkdir -p "$SIM_HOME"

# ── the HOME environment ──────────────────────────────────────────────────
export HOME="$SIM_HOME"
export XDG_CONFIG_HOME="$SIM_HOME/.config"
export XDG_DATA_HOME="$SIM_HOME/.local/share"
export XDG_STATE_HOME="$SIM_HOME/.local/state"
export MPM_WORKSPACE="$SIM_WS"
export REHEARSAL_SHIM_LOG="$SIM_HOME/.shim.log"
export REHEARSAL_SHIM_STATE="$SIM_HOME/.systemd.state"

mkdir -p "$XDG_CONFIG_HOME/opencode" "$XDG_DATA_HOME" "$XDG_STATE_HOME" \
         "$HOME/.local/bin" "$XDG_CONFIG_HOME/systemd/user"

# ── the systemd shim ──────────────────────────────────────────────────────
make_systemd_shim "$SIM_HOME/.shim"
export PATH="$SIM_HOME/.shim:$PATH"

# ── the checkout ──────────────────────────────────────────────────────────
mkdir -p "$SIM_WS"
for d in cmd internal scripts agent_installation contrib drills mode persona docs; do
  [ -d "$SRC_TREE/$d" ] && cp -a "$SRC_TREE/$d" "$SIM_WS/"
done
for f in Makefile go.mod go.sum VERSION install.sh uninstall.sh LICENSE README.md; do
  [ -f "$SRC_TREE/$f" ] && cp -a "$SRC_TREE/$f" "$SIM_WS/"
done

# The checkout's Git metadata — a REAL repository, so "is it still a
# checkout?" is a question with a real answer.
(
  cd "$SIM_WS" || exit 1
  git init -q .
  git config user.email rehearsal@example.invalid
  git config user.name "Rehearsal"
  git config commit.gpgsign false
  git add -A >/dev/null 2>&1
  git commit -q -m "simulated checkout-style installation" >/dev/null 2>&1
) && echo "  [sim] the simulated workspace is a real Git checkout ($(cd "$SIM_WS" && git rev-parse --short HEAD))"

# ── the installed binaries ────────────────────────────────────────────────
# These are the binaries the live installation is running, copied verbatim.
# The copy is READ-ONLY on the live side and happens BEFORE the strace
# detector is armed.
mkdir -p "$SIM_WS/bin"
for b in mpm mpm-mcp mpm-critic mpm-scheduler mpm-telemetry; do
  install -m 0755 "$LIVE_BIN/$b" "$SIM_WS/bin/$b"
done
echo "  [sim] installed five binaries: $(cd "$SIM_WS/bin" && "$SIM_WS/bin/mpm" --version 2>&1 | head -1)"

# ── the operator configuration — SYNTHETIC, never the live file ───────────
#
# NOTE THE SHAPE: `components` maps a component name to a PROFILE NAME, so
# its values must be strings. A malformed mpm_config.json is not a soft
# failure — config.LoadConfig() returns (nil, err) and DefaultMemoryPaths()
# dereferences the nil, so `mpm_memory save` PANICS. That is a real defect
# in the substrate (recorded in the plan) and it is also why this fixture
# has to be exactly right.
cat > "$SIM_WS/mpm_config.json" <<'EOF'
{
  "profiles": {
    "default": {
      "provider": "rehearsal",
      "model": "rehearsal-model",
      "base_url": "http://127.0.0.1:9/rehearsal",
      "api_key": "rehearsal-placeholder-key-not-a-credential",
      "timeout_seconds": 30
    },
    "embedding": {
      "name": "rehearsal-embedding",
      "provider": "rehearsal",
      "model": "rehearsal-embedding-model",
      "base_url": "http://127.0.0.1:9/rehearsal"
    }
  },
  "components": { "embedding": "embedding", "memory": "default" }
}
EOF
chmod 0600 "$SIM_WS/mpm_config.json"

# ── the persistent state ──────────────────────────────────────────────────
mkdir -p "$SIM_WS/src/db" "$SIM_WS/backups/critic-pre" "$SIM_WS/blobs" \
         "$SIM_WS/run" "$SIM_WS/logs" "$SIM_WS/config"
chmod 0700 "$SIM_WS/src/db" "$SIM_WS/backups/critic-pre" "$SIM_WS/blobs"

# The database is created by the OLD binary, so its schema is exactly what
# the currently-installed runtime expects.
"$SIM_WS/bin/mpm" call mpm_system --payload '{"action":"health_check"}' >/dev/null 2>&1
echo "  [sim] database bootstrapped by the installed binary: $(ls -la "$SIM_WS/src/db/mpm.db" 2>/dev/null | awk '{print $5}') bytes"

# SYNTHETIC content. Representative of the live shape — memories, a lesson,
# evidence, a decision — with no live content in it.
save() {
  "$SIM_WS/bin/mpm" call mpm_memory --payload "$1" >/dev/null 2>&1
}
save '{"action":"save","params":{"fact":"REHEARSAL FACT ONE: the substrate keeps its state in the data root, not in the release bundle.","collection":"memories","tags":["rehearsal","layout"]}}'
save '{"action":"save","params":{"fact":"REHEARSAL FACT TWO: promotion is an atomic symlink swap, so a reader sees the old revision or the new one, never a mixture.","collection":"memories","tags":["rehearsal","upgrade"]}}'
save '{"action":"save","params":{"fact":"REHEARSAL FACT THREE: the backup must be an online SQLite backup, because a raw copy of a WAL database omits everything still in the log.","collection":"memories","tags":["rehearsal","backup"]}}'
save '{"action":"save","params":{"fact":"REHEARSAL EVIDENCE: the rehearsal recorded a synthetic corpus so the live database was never read into a fixture.","collection":"evidence","tags":["rehearsal"]}}'

"$SIM_WS/bin/mpm" call mpm_lessons --payload '{"action":"save","params":{"fact":"REHEARSAL LESSON: a migration that cannot be rolled back must be preceded by a backup that has been verified by restoring it.","type":"warning","tags":["rehearsal"]}}' >/dev/null 2>&1

"$SIM_WS/bin/mpm" call mpm_decisions --payload '{"action":"record","params":{"choice":"REHEARSAL DECISION: keep the data root at ~/.mpm so no host configuration changes.","context":"Runtime layout migration","rationale":"The database path, the systemd unit, the PATH symlink and the MCP client wiring all name that path already.","outcome":"accepted for the rehearsal","tags":["rehearsal"]}}' >/dev/null 2>&1

# The runtime state file and the scheduler lock.
cat > "$SIM_WS/active.json" <<'EOF'
{"active_mode":"default","updated_at":"2026-10-20T00:00:00Z"}
EOF
chmod 0600 "$SIM_WS/active.json"
: > "$SIM_WS/scheduler.lock"

# ── the host registrations ────────────────────────────────────────────────
cat > "$XDG_CONFIG_HOME/opencode/opencode.jsonc" <<EOF
{
  "\$schema": "https://opencode.ai/config.json",
  "plugins": [
    "file://$SIM_WS/agent_installation/mpm-opencode"
  ]
}
EOF

cat > "$XDG_CONFIG_HOME/systemd/user/mpm-scheduler.service" <<EOF
[Unit]
Description=MPM universal wake executor (USER-level, alpha default)

[Service]
Type=simple
WorkingDirectory=$SIM_WS
ExecStart=$SIM_WS/bin/mpm-scheduler --interval 60s --log-level info --lock $SIM_WS/scheduler.lock
Environment=MPM_WORKSPACE=$SIM_WS
Environment=MPM_DB_PATH=$SIM_WS/src/db/mpm.db
Environment=MPM_BACKUP_DIR=$SIM_WS/backups/critic-pre
Environment=MPM_CRITIC_BIN=$SIM_WS/bin/mpm-critic
Environment=MPM_BIN=$SIM_WS/bin/mpm
Restart=on-failure

[Install]
WantedBy=default.target
EOF

ln -sf "$SIM_WS/bin/mpm"     "$HOME/.local/bin/mpm"
ln -sf "$SIM_WS/bin/mpm-mcp" "$HOME/.local/bin/mpm-mcp"

# The MCP client wiring.
cat > "$SIM_HOME/.claude.json" <<EOF
{
  "mcpServers": {
    "mpm": {
      "command": "$SIM_WS/bin/mpm-mcp",
      "env": { "MPM_FRAMEWORK": "claude", "MPM_WORKSPACE": "$SIM_WS" }
    }
  }
}
EOF

# ── the inventory ─────────────────────────────────────────────────────────
echo
echo "  [sim] simulated installation:"
echo "         $SIM_WS  ($(du -sh "$SIM_WS" | cut -f1))"
echo "         .git      $(du -sh "$SIM_WS/.git" | cut -f1)"
echo "         bin       $(du -sh "$SIM_WS/bin" | cut -f1)"
echo "         src/db    $(du -sh "$SIM_WS/src/db" | cut -f1)"
echo "         agent_installation $(du -sh "$SIM_WS/agent_installation" | cut -f1)"
echo
echo "  [sim] synthetic corpus:"
python3 - "$SIM_WS/src/db/mpm.db" <<'PY'
import sqlite3, sys
c = sqlite3.connect(f"file:{sys.argv[1]}?mode=ro", uri=True)
for t in ("memories","lessons","evidence","tool_invocations","schema_migrations"):
    try: print(f"         {t:<18}{c.execute(f'SELECT COUNT(*) FROM {t}').fetchone()[0]}")
    except Exception as e: print(f"         {t:<18}-- {e}")
c.close()
PY
echo
echo "  [sim] done. The disposable HOME is $SIM_HOME"
