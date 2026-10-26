#!/usr/bin/env bash
# measure/wake_context_e2e.sh — end-to-end proof for the wake-context fix.
#
# Runs an ISOLATED OpenCode host with:
#   * the mpm-opencode plugin from THIS repository (not ~/.mpm), and
#   * measure/probe/wake_context_probe.js as a witness,
# against a DISPOSABLE MPM workspace that contains a unique marker memory.
#
# PASS  = the marker appears in a system-prompt part the probe captured.
# FAIL  = the host ran, the plugin loaded, and no part carried the marker.
#
# Usage:
#   wake_context_e2e.sh <label> [plugin-dir]
#
# `plugin-dir` defaults to this repository's adapter directory. Passing a
# deliberately defective copy is how the harness is shown to be
# non-vacuous (a negative control).
#
# Safety:
#   * XDG_CONFIG/DATA/STATE/CACHE all redirected under /tmp/opencode
#   * MPM_WORKSPACE + MPM_BINARY pinned to a disposable workspace and the
#     development build; ~/.mpm and ~/.mpm-dogfood are never opened
#   * the installed plugin payload is not touched
set -uo pipefail

LABEL="${1:?usage: wake_context_e2e.sh <label> [plugin-dir]}"
PLUGIN_DIR="${2:-$HOME/src/mpm-opencode/agent_installation/mpm-opencode}"

ROOT=/tmp/opencode/wake-e2e
RUN=$ROOT/$LABEL
SCRATCH=$RUN/mpm-scratch
OUT=$RUN/out
PLUGIN_COPY=$RUN/plugins

rm -rf "$RUN"; mkdir -p "$OUT" "$SCRATCH" "$PLUGIN_COPY/probe"

START_TS=$(date +%s)

# --- isolated host environment ---------------------------------------------
export XDG_CONFIG_HOME=$RUN/config
export XDG_DATA_HOME=$RUN/data
export XDG_STATE_HOME=$RUN/state
export XDG_CACHE_HOME=$RUN/cache
export OPENCODE_CONFIG_DIR=$RUN/config/opencode
export OPENCODE_DISABLE_AUTOUPDATE=1
mkdir -p "$OPENCODE_CONFIG_DIR"

# --- disposable MPM workspace, pinned binaries ------------------------------
export MPM_WORKSPACE=$SCRATCH
export MPM_DB_PATH=$SCRATCH/src/db/mpm.db
export MPM_BINARY=$HOME/src/mpm/.build/bin/mpm

MARKER="WAKE_CTX_MARKER_${LABEL//[^A-Za-z0-9]/}_a41f"
MARKER="$(printf '%s' "$MARKER" | tr '[:lower:]' '[:upper:]')"

"$MPM_BINARY" init >/dev/null 2>&1 || true
# recentMemories() orders by created_at DESC at ONE-SECOND resolution, and
# the seeded directives land in the same second as init. Sleep so the
# marker's created_at is strictly newer and it cannot lose the LIMIT 5 tie.
sleep 2
"$MPM_BINARY" call mpm_memory --payload "$(python3 - "$MARKER" <<'PY'
import json,sys
print(json.dumps({"action":"save","params":{
  "fact": f"{sys.argv[1]} disposable isolation probe: this token is only in the wake context.",
  "collection":"memories","tags":["session-c","e2e"]}}))
PY
)" >/dev/null 2>&1
sleep 1

# Sanity: the substrate really does put the marker in the system-prompt body.
"$MPM_BINARY" call mpm_context --payload '{"action":"read_wake_context","params":{"format":"system-prompt"}}' \
  > "$OUT/wake_context_envelope.json" 2>/dev/null
ENVELOPE_HAS_MARKER=no
grep -q "$MARKER" "$OUT/wake_context_envelope.json" && ENVELOPE_HAS_MARKER=yes
if [ "$ENVELOPE_HAS_MARKER" != "yes" ]; then
  echo "HARNESS PRECONDITION FAILED: the disposable workspace's own" >&2
  echo "system-prompt envelope does not contain $MARKER, so this run cannot" >&2
  echo "test injection. Inspect $OUT/wake_context_envelope.json." >&2
  exit 2
fi

# --- witness probe ----------------------------------------------------------
cp "$HOME/src/mpm-opencode/agent_installation/mpm-opencode/measure/probe/wake_context_probe.js" \
   "$PLUGIN_COPY/probe/wake_context_probe.js"
cat > "$PLUGIN_COPY/probe/server.js" <<'EOF'
export { default } from "./wake_context_probe.js"
EOF

export WAKE_PROBE_LOG=$OUT/context.jsonl
rm -f "$WAKE_PROBE_LOG"

# Plugin order matters: the MPM plugin registers first, so its push happens
# before the witness hook runs. The probe also re-reads the arrays at
# teardown, which covers the opposite ordering.
cat > "$OPENCODE_CONFIG_DIR/opencode.jsonc" <<EOF
{
  "\$schema": "https://opencode.ai/config.json",
  "plugins": [
    "file://$PLUGIN_DIR",
    "file://$PLUGIN_COPY/probe"
  ]
}
EOF

"$HOME/.opencode/bin/opencode" debug paths > "$OUT/paths.txt" 2>&1

cd "$RUN" || exit 1
timeout 240 "$HOME/.opencode/bin/opencode" run --standalone --auto \
  -m minimax-coding-plan/MiniMax-M3.1-Flash \
  "Reply with exactly: E2E_DONE" \
  > "$OUT/agent.stdout" 2> "$OUT/agent.stderr"
RC=$?

# --- verdict ----------------------------------------------------------------
python3 - "$OUT" "$MARKER" "$ENVELOPE_HAS_MARKER" "$RC" "$LABEL" <<'PY'
import json, os, sys
out, marker, envelope_has, rc, label = sys.argv[1], sys.argv[2], sys.argv[3], sys.argv[4], sys.argv[5]
probe = os.path.join(out, "context.jsonl")
hooks, teardown_snaps, init_seen, errs = [], [], False, []
if os.path.exists(probe):
    for line in open(probe):
        try: d = json.loads(line)
        except Exception: continue
        if d.get("kind") == "context.hook": hooks.append(d)
        elif d.get("kind") == "probe.teardown": teardown_snaps = d.get("snapshots") or []
        elif d.get("kind") == "probe.init": init_seen = True
        elif d.get("kind") == "probe.error": errs.append(d)

def carries(texts):
    return any(marker in (t or "") for t in (texts or []))

immediate = any(carries(h.get("texts")) for h in hooks)
at_teardown = any(carries(s.get("texts")) for s in teardown_snaps)

print(f"label                    : {label}")
print(f"host rc                  : {rc}")
print(f"probe loaded             : {init_seen}")
print(f"context hooks observed   : {len(hooks)}")
print(f"envelope carries marker  : {envelope_has}")
print(f"marker in hook (immed.)  : {immediate}")
print(f"marker in hook (teardown): {at_teardown}")
print(f"probe errors             : {errs if errs else 'none'}")
for h in hooks:
    for t in (h.get("texts") or [])[:20]:
        if "Recent Memories" in (t or "") or marker in (t or ""):
            print(f"--- captured system part (session={h.get('sessionID')}) ---")
            print((t or "")[:600])

ok = init_seen and (immediate or at_teardown)
print(f"VERDICT                  : {'PASS' if ok else 'FAIL'}")
sys.exit(0 if ok else 1)
PY
V=$?
echo "--- agent stdout ---"; tail -n 6 "$OUT/agent.stdout"
echo "--- artifacts: $OUT ---"
exit $V
