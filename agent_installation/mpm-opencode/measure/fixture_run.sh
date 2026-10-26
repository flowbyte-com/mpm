#!/usr/bin/env bash
# Session C — behavioural fixture runner.
#
# Runs a prompt against an ISOLATED OpenCode instance whose MPM plugin is
# wired to a disposable MPM workspace, then reports the MPM tool calls the
# agent actually made (from the substrate's own `tool_invocations` audit).
#
# Isolation:
#   * isolated XDG dirs  → the live ~/.config/opencode, session store and
#     ~/.local/share/opencode are never touched
#   * disposable MPM workspace → no write ever reaches ~/.mpm or
#     ~/.mpm-dogfood
#   * the installed plugin payload is READ, never modified
#
# Usage: fixture_run.sh <label> <prompt>
set -uo pipefail

LABEL="$1"; shift
PROMPT="$1"; shift || true

ROOT=/tmp/opencode/probe-c
SCRATCH=$ROOT/mpm-scratch
OUTDIR=$ROOT/out
mkdir -p "$OUTDIR"

export XDG_CONFIG_HOME=$ROOT/config
export XDG_DATA_HOME=$ROOT/data
export XDG_STATE_HOME=$ROOT/state
export XDG_CACHE_HOME=$ROOT/cache
export OPENCODE_CONFIG_DIR=$ROOT/config/opencode
export OPENCODE_DISABLE_AUTOUPDATE=1

# MPM wiring — the plugin pins MPM_WORKSPACE for every subprocess it spawns
export MPM_WORKSPACE=$SCRATCH
export MPM_DB_PATH=$SCRATCH/src/db/mpm.db
export MPM_BINARY=$HOME/src/mpm/.build/bin/mpm

WORK=$ROOT/fixture-$LABEL
rm -rf "$WORK"; mkdir -p "$WORK"

# --- record the pre-state of the substrate's tool audit ------------------
M=$MPM_BINARY
PRE=$($M call mpm_system --payload '{"action":"health_check","params":{}}' 2>/dev/null | head -c 200)

cd "$WORK"
timeout 300 ~/.opencode/bin/opencode run --standalone --auto \
  -m minimax-coding-plan/MiniMax-M3.1-Flash "$PROMPT" \
  > "$OUTDIR/fixture-$LABEL.stdout" 2> "$OUTDIR/fixture-$LABEL.stderr"
RC=$?

echo "=== fixture: $LABEL (rc=$RC) ==="
echo "--- agent stdout ---"
tail -n 25 "$OUTDIR/fixture-$LABEL.stdout"

echo "--- MPM tool calls made by the agent (from tool_invocations) ---"
python3 - "$SCRATCH" <<'PY'
import sqlite3, sys, os
db = os.path.join(sys.argv[1], "src", "db", "mpm.db")
if not os.path.exists(db):
    print("   (no database)"); raise SystemExit
c = sqlite3.connect(f"file:{db}?mode=ro", uri=True)
cols = [d[1] for d in c.execute("PRAGMA table_info(tool_invocations)")]
want = [x for x in ("id","tool_name","action","result_status","outcome_class","framework_name","framework_session_id","invocation_id","started_at","duration_ms") if x in cols]
rows = list(c.execute(f"select {','.join(want)} from tool_invocations order by started_at"))
print(f"   {len(rows)} invocation(s)")
for r in rows:
    d = dict(zip(want, r))
    print("   -", d.get("tool_name"), "/", d.get("action"), "|", d.get("result_status"),
          "|", d.get("outcome_class"), "|", d.get("framework_name"),
          "| fsid=", d.get("framework_session_id"))
PY
exit $RC
