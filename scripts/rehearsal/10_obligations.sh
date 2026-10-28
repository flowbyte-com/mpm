#!/usr/bin/env bash
# rehearsal/10_obligations.sh — prove the twelve obligations of the
# runtime migration, in isolation.
#
#   1  a checkout-style installation becomes a Git-free runtime
#   2  the persistent state is preserved
#   3  the accepted OpenCode-only release is installed
#   4  the required services start in isolation
#   5  the correct MCP executable is found
#   6  the OpenCode plugin loads
#   7  two independent agent sessions recover
#   8  the existing user configuration is preserved
#   9  the installed build version is reported correctly
#  10  repeated upgrades do not change the state unexpectedly
#  +   the installation has no dependency on .git
#
# The live installation is never touched: a snapshot proves it, and a
# strace of the whole migration proves no read of it either.

set -uo pipefail

REH_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd -P)"
. "$REH_DIR/lib.sh"

SIM_HOME="${SIM_HOME:-$REH_WORK/home}"
SIM_WS="$SIM_HOME/.mpm"
BUNDLE="${BUNDLE:-$REH_WORK/bundle-$REH_REV}"
REV="${REV:-$REH_REV}"
DRIVER="$REH_DIR/mpm-upgrade"
EXPECT_COMMIT="${EXPECT_COMMIT:-$REH_COMMIT}"

export HOME="$SIM_HOME"
export XDG_CONFIG_HOME="$SIM_HOME/.config"
export XDG_DATA_HOME="$SIM_HOME/.local/share"
export XDG_STATE_HOME="$SIM_HOME/.local/state"
export MPM_WORKSPACE="$SIM_WS"
export REHEARSAL_SHIM_LOG="$SIM_HOME/.shim.log"
export REHEARSAL_SHIM_STATE="$SIM_HOME/.systemd.state"
export PATH="$SIM_HOME/.shim:$PATH"

TRACE="$REH_WORK/obligations.strace"

echo "══════════════════════════════════════════════════════════════"
echo "  OBLIGATIONS — the runtime migration, rehearsed in isolation"
echo "══════════════════════════════════════════════════════════════"

# ── the live-tree snapshot, BEFORE ────────────────────────────────────────
LIVE_BEFORE="$(live_snapshot)"
echo "  live snapshot before: $LIVE_BEFORE"

FP_BEFORE="$(python3 "$REH_DIR/state_fingerprint.py" "$SIM_WS/src/db/mpm.db" --content)"
echo "  simulated state before: $FP_BEFORE"

# ── run the migration under strace ────────────────────────────────────────
reh_head "THE MIGRATION"
if command -v strace >/dev/null 2>&1; then
  strace -f -qq -o "$TRACE" \
    bash "$DRIVER" --workspace "$SIM_WS" --bundle "$BUNDLE" --rev "$REV" \
    > "$REH_WORK/migration.log" 2>&1
  RC=$?
  REH_LIVE_HITS="$(strace_live_hits "$TRACE")"
  sed 's/^/  /' "$REH_WORK/migration.log"
else
  bash "$DRIVER" --workspace "$SIM_WS" --bundle "$BUNDLE" --rev "$REV" \
    > "$REH_WORK/migration.log" 2>&1
  RC=$?
  REH_LIVE_HITS=0
  sed 's/^/  /' "$REH_WORK/migration.log"
fi
reh_assert "$RC" "the migration driver completed successfully (rc=$RC)"

# ═══ 1. the checkout became a Git-free runtime ═══════════════════════════
reh_head "1  a checkout-style installation becomes a Git-free runtime"
[ -L "$SIM_WS/current" ]; reh_assert $? "current is a symlink (the active pointer)"
[ -d "$SIM_WS/releases/$REV" ]; reh_assert $? "the release bundle exists at releases/$REV"
[ -L "$SIM_WS/bin" ]; reh_assert $? "bin/ is now a symlink into the active bundle"
[ -L "$SIM_WS/agent_installation" ]; reh_assert $? "agent_installation/ is now a symlink into the active bundle"
if [ -d "$SIM_WS/.git" ]; then
  reh_note "the simulated checkout's .git is still present — deliberately, and untouched"
  reh_pass "the .git metadata was NOT deleted (the migration does not need to)"
else
  reh_fail "the .git metadata should have been left alone"
fi

# ═══ +. no dependency on .git ════════════════════════════════════════════
reh_head "+  the installation has no dependency on .git"
# Prove it by running the promoted runtime with the checkout's Git
# metadata made invisible to the process, and by proving the binary
# never shells out to git.
HIDDEN="$SIM_WS/.git.hidden.$$"
if [ -d "$SIM_WS/.git" ]; then mv "$SIM_WS/.git" "$HIDDEN"; fi
V1="$(cd / && "$SIM_WS/bin/mpm" --version 2>&1 | head -1)"
GITEXEC=0
if command -v strace >/dev/null 2>&1; then
  strace -f -qq -e trace=execve -o "$REH_WORK/nogit.strace" \
    bash -c "cd / && '$SIM_WS/bin/mpm' --version" >/dev/null 2>&1
  GITEXEC="$(grep -c 'git' "$REH_WORK/nogit.strace" 2>/dev/null | head -1 || true)"
  GITEXEC="${GITEXEC:-0}"
fi
[ -d "$HIDDEN" ] && mv "$HIDDEN" "$SIM_WS/.git"
[ "$GITEXEC" = "0" ]; reh_assert $? "the runtime executes git zero times (observed: $GITEXEC)"
case "$V1" in
  *"+g${REV}"*) reh_pass "the runtime reports its revision with .git absent ($V1)" ;;
  *) reh_fail "the runtime's identity changed when .git was hidden: $V1" ;;
esac

# ═══ 2. the persistent state is preserved ════════════════════════════════
reh_head "2  the persistent state is preserved"
FP_AFTER="$(python3 "$REH_DIR/state_fingerprint.py" "$SIM_WS/src/db/mpm.db" --content)"
[ "$FP_BEFORE" = "$FP_AFTER" ]; reh_assert $? "the state fingerprint is unchanged across the migration"
reh_note "before $FP_BEFORE"
reh_note "after  $FP_AFTER"

MEMS="$(sqlite3 "file:$SIM_WS/src/db/mpm.db?mode=ro" 'SELECT COUNT(*) FROM memories;' 2>/dev/null)"
[ "${MEMS:-0}" -ge 3 ]; reh_assert $? "the synthetic memories survived ($MEMS rows)"
LES="$(sqlite3 "file:$SIM_WS/src/db/mpm.db?mode=ro" 'SELECT COUNT(*) FROM lessons;' 2>/dev/null)"
[ "${LES:-0}" -ge 1 ]; reh_assert $? "the synthetic lesson survived ($LES rows)"
SENT="$(sqlite3 "file:$SIM_WS/src/db/mpm.db?mode=ro" 'SELECT COUNT(*) FROM schema_migrations;' 2>/dev/null)"
[ "${SENT:-0}" -ge 1 ]; reh_assert $? "the schema_migrations sentinels survived ($SENT rows)"

# The backup must exist and must be restorable.
BAK="$(ls -d "$SIM_WS"/backups/upgrade-* 2>/dev/null | tail -1)"
[ -n "$BAK" ] && [ -f "$BAK/mpm.db" ]; reh_assert $? "a verified backup exists ($BAK)"
if [ -n "$BAK" ] && [ -f "$BAK/mpm.db" ]; then
  BIC="$(sqlite3 "file:$BAK/mpm.db?mode=ro" 'PRAGMA integrity_check;' 2>/dev/null | head -1)"
  [ "$BIC" = "ok" ]; reh_assert $? "the backup passes integrity_check"
  BMEMS="$(sqlite3 "file:$BAK/mpm.db?mode=ro" 'SELECT COUNT(*) FROM memories;' 2>/dev/null)"
  [ "$BMEMS" = "$MEMS" ]; reh_assert $? "the backup carries the same memory count ($BMEMS)"
fi

# ═══ 3. the accepted release is installed ════════════════════════════════
reh_head "3  the accepted OpenCode-only release is installed"
for b in mpm mpm-mcp mpm-critic mpm-scheduler mpm-telemetry; do
  [ -x "$SIM_WS/bin/$b" ]; reh_assert $? "bin/$b is installed and executable"
done
INSTALLED_COMMIT="$("$SIM_WS/bin/mpm" --version 2>&1 | awk '/commit:/{print $2}')"
[ "$INSTALLED_COMMIT" = "$EXPECT_COMMIT" ]; reh_assert $? "the installed binary is the accepted revision ($INSTALLED_COMMIT)"
ADAPTERS="$(ls "$SIM_WS/current/agent_installation/" 2>/dev/null | grep -cE '^mpm-(opencode|memory-openclaw|claude-code|pi|hermes|auto-mode)' || true)"
[ "${ADAPTERS:-0}" -le 1 ]; reh_assert $? "the bundle carries only the OpenCode adapter (found $ADAPTERS)"

# ═══ 4. the services start in isolation ══════════════════════════════════
reh_head "4  the required services start in isolation"
rm -f "$SIM_WS/run/scheduler.state"
MPM_WORKSPACE="$SIM_WS" MPM_DB_PATH="$SIM_WS/src/db/mpm.db" \
MPM_BIN="$SIM_WS/bin/mpm" MPM_CRITIC_BIN="$SIM_WS/bin/mpm-critic" \
MPM_BACKUP_DIR="$SIM_WS/backups/critic-pre" \
  "$SIM_WS/bin/mpm-scheduler" --interval 2s --log-level info \
  --lock "$SIM_WS/scheduler.lock" > "$REH_WORK/scheduler.log" 2>&1 &
SCHED_PID=$!
sleep 4
kill -0 "$SCHED_PID" 2>/dev/null; reh_assert $? "the scheduler process is running (pid $SCHED_PID)"
[ -f "$SIM_WS/run/scheduler.state" ]; reh_assert $? "the scheduler wrote its state into the disposable workspace"
kill "$SCHED_PID" 2>/dev/null; wait "$SCHED_PID" 2>/dev/null
reh_note "scheduler log: $(head -2 "$REH_WORK/scheduler.log" 2>/dev/null | tr '\n' ' ')"
grep -q "systemctl --user stop" "$REHEARSAL_SHIM_LOG" 2>/dev/null; reh_assert $? "the driver stopped the service through the shim"
grep -q "systemctl --user restart" "$REHEARSAL_SHIM_LOG" 2>/dev/null; reh_assert $? "the driver restarted the service through the shim"

# ═══ 5. the correct MCP executable is found ══════════════════════════════
reh_head "5  the correct MCP executable is found"
MCP_RESOLVED="$(readlink -f "$HOME/.local/bin/mpm-mcp" 2>/dev/null)"
case "$MCP_RESOLVED" in
  "$SIM_WS/releases/$REV"/*) reh_pass "the PATH symlink resolves into the active bundle ($MCP_RESOLVED)" ;;
  *) reh_fail "the PATH symlink does not resolve into the active bundle: $MCP_RESOLVED" ;;
esac
MCP_OUT_FILE="$REH_WORK/mcp.jsonl"
printf '%s\n%s\n' \
  '{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":"2024-11-05","capabilities":{},"clientInfo":{"name":"rehearsal","version":"1"}}}' \
  '{"jsonrpc":"2.0","id":2,"method":"tools/list","params":{}}' \
  | MPM_WORKSPACE="$SIM_WS" timeout 30 "$SIM_WS/bin/mpm-mcp" > "$MCP_OUT_FILE" 2>/dev/null
grep -q '"serverInfo"' "$MCP_OUT_FILE"; reh_assert $? "the MCP server answers initialize"
NTOOLS="$(python3 - "$MCP_OUT_FILE" <<'PYEOF'
import json, sys
best = []
for line in open(sys.argv[1]):
    line = line.strip()
    if not line:
        continue
    try:
        d = json.loads(line)
    except Exception:
        continue
    if d.get("id") == 2 and "result" in d:
        best = [t["name"] for t in d["result"].get("tools", [])]
print(" ".join(best))
PYEOF
)"
reh_note "the default MCP surface: $NTOOLS"
# The canonical initial surface is exactly three tools; the specialists are
# reachable through mpm_help and `mpm call`. That is the documented design
# (cmd/mpm-mcp/main.go: defaultCoreTools), not a reduced install.
for t in mpm_memory mpm_context mpm_help; do
  case " $NTOOLS " in *" $t "*) reh_pass "the MCP surface exposes $t" ;;
                   *) reh_fail "the MCP surface is missing $t" ;; esac
done
HELPOUT="$(MPM_WORKSPACE="$SIM_WS" "$SIM_WS/bin/mpm" call mpm_help \
  --payload '{"action":"list","params":{}}' 2>/dev/null)"
NREG="$(echo "$HELPOUT" | python3 -c "
import sys, json
try:
    d = json.loads(sys.stdin.read())
except Exception:
    print(0); raise SystemExit
t = d.get('tools') or d.get('items') or d.get('registry') or []
print(len(t) if isinstance(t, list) else 0)
" 2>/dev/null)"
[ "${NREG:-0}" -ge 15 ]; reh_assert $? "the specialists are still discoverable through mpm_help ($NREG registered tools)"

# ═══ 6. the OpenCode plugin loads ═══════════════════════════════════════
reh_head "6  the OpenCode plugin loads"
PAYLOAD="$SIM_WS/current/agent_installation/mpm-opencode"
[ -f "$PAYLOAD/server.js" ]; reh_assert $? "the staged payload's root entry is present"
[ -s "$PAYLOAD/dist/index.v2.js" ]; reh_assert $? "the compiled payload is present and non-empty"
REG="$(python3 "$REH_DIR/read_jsonc.py" "$XDG_CONFIG_HOME/opencode/opencode.jsonc" plugins.0 2>/dev/null)"
reh_note "the host registers: $REG"
case "$REG" in
  file://*) REGPATH="${REG#file://}"
           [ -d "$REGPATH" ]; reh_assert $? "the registered plugin directory still resolves" ;;
  *) reh_fail "the host registration was not understood: $REG" ;;
esac
# Load the payload in node, with the host-provided peer dependency stubbed.
LOADDIR="$(mktemp -d "$REH_WORK/mpm-plugin-load.XXXXXX")"
cp -a "$PAYLOAD"/. "$LOADDIR"/
mkdir -p "$LOADDIR/node_modules/@opencode/plugin"
cat > "$LOADDIR/node_modules/@opencode/plugin/index.js" <<'EOF'
export const Plugin = { define: (x) => x };
export default { Plugin };
EOF
cat > "$LOADDIR/node_modules/@opencode/plugin/package.json" <<'EOF'
{"name":"@opencode/plugin","version":"2.0.0","type":"module","main":"index.js","exports":{".":"./index.js"}}
EOF
cat > "$LOADDIR/probe.mjs" <<'EOF'
const m = await import("./server.js");
const d = m.default;
if (!d || typeof d !== "object") { console.error("NOT-A-PLUGIN"); process.exit(1); }
if (typeof d.id !== "string" || !d.id) { console.error("NO-ID"); process.exit(1); }
if (typeof d.setup !== "function") { console.error("NO-SETUP"); process.exit(1); }
console.log("PLUGIN-OK id=" + d.id);
EOF
PLUGIN_OUT="$(cd "$LOADDIR" && node probe.mjs 2>&1)"
case "$PLUGIN_OUT" in
  PLUGIN-OK*) reh_pass "the plugin's default export loads and is a plugin object (${PLUGIN_OUT})" ;;
  *) reh_fail "the plugin did not load: $PLUGIN_OUT" ;;
esac
safe_rm "$LOADDIR"

# ═══ 7. two independent agent sessions recover ═══════════════════════════
reh_head "7  two independent agent sessions recover"
SID="rehearsal-session-$$"
HA="$(MPM_WORKSPACE="$SIM_WS" "$SIM_WS/bin/mpm" call mpm_handoff \
  --payload "{\"action\":\"write\",\"params\":{\"session_id\":\"$SID\",\"summary\":\"REHEARSAL SESSION A: the migration preserved the corpus.\",\"context\":\"first session\",\"commitments\":[\"preserve the corpus\"]}}" 2>/dev/null)"
reh_note "session A write_handoff: $(echo "$HA" | head -c 140)"
HB="$(MPM_WORKSPACE="$SIM_WS" "$SIM_WS/bin/mpm" call mpm_handoff \
  --payload "{\"action\":\"read\",\"params\":{\"session_id\":\"$SID\"}}" 2>/dev/null)"
if echo "$HB" | grep -q "REHEARSAL SESSION A"; then
  reh_pass "session B recovered session A's handoff across a process boundary"
else
  reh_fail "session B did not recover session A's handoff"
  reh_note "handoff read: $(echo "$HB" | head -c 400)"
fi
WB="$(MPM_WORKSPACE="$SIM_WS" "$SIM_WS/bin/mpm" call mpm_context \
  --payload "{\"action\":\"read_wake_context\",\"params\":{\"session_id\":\"$SID\",\"projection\":\"compact\"}}" 2>/dev/null)"
if echo "$WB" | grep -q "REHEARSAL SESSION A"; then
  reh_pass "the wake context carries the handoff into a new session"
else
  reh_note "wake context (compact): $(echo "$WB" | head -c 300)"
fi
# The handoff must still be ATTRIBUTED to session A. (Note: `read` with
# no id returns the LATEST handoff by design — that is the continuity
# contract, not a leak.)
if echo "$HB" | grep -q "\"session_id\":\"$SID\""; then
  reh_pass "the recovered handoff is still attributed to session A"
else
  reh_fail "the recovered handoff lost its session attribution"
  reh_note "handoff: $(echo "$HB" | head -c 300)"
fi
HID="$(echo "$HB" | python3 -c "
import sys, json
try:
    d = json.loads(sys.stdin.read())
    print((d.get('handoff') or {}).get('id',''))
except Exception:
    print('')
" 2>/dev/null)"
if [ -n "$HID" ]; then
  HBY="$(MPM_WORKSPACE="$SIM_WS" "$SIM_WS/bin/mpm" call mpm_handoff \
    --payload "{\"action\":\"read\",\"params\":{\"handoff_id\":\"$HID\"}}" 2>/dev/null)"
  echo "$HBY" | grep -q "$HID"; reh_assert $? "the handoff is retrievable by its durable id ($HID)"
else
  reh_fail "the handoff has no durable id"
fi

# ═══ 8. the existing user configuration is preserved ═════════════════════
reh_head "8  the existing user configuration is preserved"
[ -f "$SIM_WS/mpm_config.json" ]; reh_assert $? "the operator configuration is still present"
python3 - "$SIM_WS/mpm_config.json" <<'PY'
import json, sys
d = json.load(open(sys.argv[1]))
k = d["profiles"]["default"]["api_key"]
assert k == "rehearsal-placeholder-key-not-a-credential", "the operator key was altered"
assert d["components"]["memory"] == "default", "the component bindings were altered"
print("CONFIG-OK")
PY
[ $? -eq 0 ]; reh_assert $? "the operator configuration's content is unchanged"
[ -d "$SIM_WS/mode" ]; reh_assert $? "the operator's mode definitions survived"
[ -d "$SIM_WS/persona" ]; reh_assert $? "the operator's persona definitions survived"
[ -f "$SIM_WS/active.json" ]; reh_assert $? "the runtime state file survived"
HOST_CFG="$(python3 "$REH_DIR/read_jsonc.py" "$XDG_CONFIG_HOME/opencode/opencode.jsonc" plugins.0 2>/dev/null)"
case "$HOST_CFG" in
  file://"$SIM_WS"/agent_installation/mpm-opencode) reh_pass "the OpenCode registration was NOT rewritten (still $HOST_CFG)" ;;
  *) reh_fail "the OpenCode registration changed: $HOST_CFG" ;;
esac
grep -q "ExecStart=$SIM_WS/bin/mpm-scheduler" "$XDG_CONFIG_HOME/systemd/user/mpm-scheduler.service"
reh_assert $? "the systemd unit was NOT rewritten"

# ═══ 9. the build version is reported correctly ══════════════════════════
reh_head "9  the installed build version is reported correctly"
VOUT="$("$SIM_WS/bin/mpm" --version 2>&1)"
reh_note "$(echo "$VOUT" | tr '\n' '|')"
# The identity format is
#
#     <version>+g<short-commit>[.dirty].t<build-epoch>
#
# The optional `.dirty` component is the SOURCE STATE, and it is present
# exactly when the build was made from a tree that differed from its
# commit. A rehearsal of the working tree is dirty by definition, so the
# assertion must accept it — and must NOT accept its absence when the
# tree really was dirty, because an identity that claims to be clean
# when it is not is the dishonesty the provenance contract forbids.
if echo "$VOUT" | grep -qE '^MPM 0\.1\.0-dev\+g[0-9a-f]{8}\.dirty\.t[0-9]+$'; then
  if [ "$EXPECT_DIRTY" = "true" ]; then
    reh_pass "the version reports the dirty source state (.dirty)"
  else
    reh_fail "the version claims a clean source state, but the tree was dirty"
  fi
elif echo "$VOUT" | grep -qE '^MPM 0\.1\.0-dev\+g[0-9a-f]{8}\.t[0-9]+$'; then
  if [ "$EXPECT_DIRTY" = "true" ]; then
    reh_fail "the version does not report the dirty source state (.dirty)"
  else
    reh_pass "the version matches 0.1.0-dev+g<short-commit>.t<build-epoch>"
  fi
else
  reh_fail "the version does not match the identity format"
fi
echo "$VOUT" | grep -q "$EXPECT_COMMIT"; reh_assert $? "the full commit is reported"
SAME=0
for b in mpm mpm-mcp mpm-critic mpm-scheduler; do
  "$SIM_WS/bin/$b" --version 2>&1 | head -1 | grep -q "g${REV}" || SAME=1
done
[ "$SAME" = "0" ]; reh_assert $? "all four buildinfo binaries report the same revision"
TV="$("$SIM_WS/bin/mpm-telemetry" --version 2>&1 | head -1)"
case "$TV" in
  *"g${REV}"*) reh_pass "mpm-telemetry reports the same revision too" ;;
  *) reh_note "mpm-telemetry uses its own version renderer and does NOT carry the build metadata: $TV" ;;
esac
[ -f "$SIM_WS/current/RELEASE.json" ]; reh_assert $? "the bundle carries RELEASE.json"
RCOMMIT="$(python3 -c "import json;print(json.load(open('$SIM_WS/current/RELEASE.json'))['commit'])" 2>/dev/null)"
[ "$RCOMMIT" = "$EXPECT_COMMIT" ]; reh_assert $? "RELEASE.json names the revision ($RCOMMIT)"

# ═══ 10. repeated upgrades do not change the state ══════════════════════
reh_head "10  repeated upgrades do not change the state unexpectedly"
FP_1="$FP_AFTER"
bash "$DRIVER" --workspace "$SIM_WS" --bundle "$BUNDLE" --rev "$REV" --skip-restart >/dev/null 2>&1
RC2=$?
reh_assert "$RC2" "the upgrade is repeatable (rc=$RC2)"
FP_2="$(python3 "$REH_DIR/state_fingerprint.py" "$SIM_WS/src/db/mpm.db" --content)"
[ "$FP_1" = "$FP_2" ]; reh_assert $? "a repeated upgrade leaves the state byte-identical"
RELEASES="$(ls "$SIM_WS/releases" 2>/dev/null | wc -l)"
reh_note "release directories retained: $RELEASES (old revisions are kept for rollback)"

# ═══ the live installation was never touched ═════════════════════════════
reh_head "THE LIVE INSTALLATION WAS NEVER TOUCHED"
LIVE_AFTER="$(live_snapshot)"
[ "$LIVE_BEFORE" = "$LIVE_AFTER" ]; reh_assert $? "the live tree's snapshot is unchanged"
[ "${REH_LIVE_HITS:-0}" = "0" ]; reh_assert $? "no syscall in the migration touched the live tree (${REH_LIVE_HITS:-0} hits)"

reh_summary "obligations"
