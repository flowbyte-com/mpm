#!/usr/bin/env bash
# rehearsal/20_negative.sh — the negative tests.
#
# A migration procedure is only as good as its failures. These tests prove:
#
#   N1  a bundle with a FAILED BINARY is rejected before it is promoted
#   N2  an unwritable destination is rejected; a read-only source is fine
#   N3  a bundle with a MISSING DEPENDENCY is rejected
#   N4  a service that cannot be STARTED rolls the promotion back
#   N5  a rollback returns the installation to the previous revision
#   N6  a FAILED STAGING is recovered from
#   N7  an INTERRUPTED PROMOTION is recovered from, at four distinct points
#   N8  none of the failures corrupted the persistent state
#
# Every test runs against its own COPY of the migrated simulation, so the
# tests are independent of one another and of the obligations run.

set -uo pipefail

REH_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd -P)"
. "$REH_DIR/lib.sh"

SIM_HOME="${SIM_HOME:-$REH_WORK/home}"
SIM_WS="$SIM_HOME/.mpm"
BUNDLE="${BUNDLE:-$REH_WORK/bundle-$REH_REV}"
REV="${REV:-$REH_REV}"
DRIVER="$REH_DIR/mpm-upgrade"
WORK="$REH_WORK/neg"

export HOME="$SIM_HOME"
export XDG_CONFIG_HOME="$SIM_HOME/.config"
export XDG_DATA_HOME="$SIM_HOME/.local/share"
export XDG_STATE_HOME="$SIM_HOME/.local/state"
export REHEARSAL_SHIM_LOG="$SIM_HOME/.shim.log"
export REHEARSAL_SHIM_STATE="$SIM_HOME/.systemd.state"
export PATH="$SIM_HOME/.shim:$PATH"

safe_rm "$WORK"; mkdir -p "$WORK"

# A fresh copy of the migrated simulation for each test.
#
# The copy is HARD-LINKED, not duplicated: the release bundles are ~78 MB
# each and ten of them will not fit on the filesystem. Hard links are safe
# here because nothing in these tests writes into a bundle in place — the
# driver always installs a bundle by COPYING it. The one file that IS
# written in place is the database, so it gets a real copy.
fresh() {
  local name="$1"
  local dst="$WORK/$name"
  safe_rm "$dst"
  cp -al "$SIM_WS" "$dst" 2>/dev/null || cp -a "$SIM_WS" "$dst"
  rm -rf "$dst/src/db"
  cp -a "$SIM_WS/src/db" "$dst/src/db"
  echo "$dst"
}

echo "══════════════════════════════════════════════════════════════"
echo "  NEGATIVE TESTS — failure injection, rollback and recovery"
echo "══════════════════════════════════════════════════════════════"

# ── the harness-integrity guard ───────────────────────────────────────────
#
# This harness has already destroyed itself once. A recursive delete with
# a mis-composed path took the whole rehearsal tree with it, and the
# damage was silent — the script kept running and only the missing files
# revealed it afterwards.
#
# So the harness's own contents are fingerprinted before the first test and
# checked after every one. A destructive bug is then caught at the point it
# happens, named, and cannot take anything else with it.
HARNESS_BASELINE="$(find "$REH_DIR" -maxdepth 1 -mindepth 1 ! -name neg \
                    -printf '%f\n' 2>/dev/null | sort)"
guard_harness() {
  local now
  now="$(find "$REH_DIR" -maxdepth 1 -mindepth 1 ! -name neg \
         -printf '%f\n' 2>/dev/null | sort)"
  if [ "$now" != "$HARNESS_BASELINE" ]; then
    printf '\n\033[31mHARNESS INTEGRITY VIOLATION\033[0m\n'
    printf '  expected: %s\n' "$(echo "$HARNESS_BASELINE" | tr '\n' ' ')"
    printf '  found   : %s\n' "$(echo "$now" | tr '\n' ' ')"
    exit 90
  fi
}
trap 'guard_harness' EXIT

PREV_REV="$(basename "$(readlink -f "$SIM_WS/current" 2>/dev/null)" 2>/dev/null || echo '')"
reh_note "the migrated simulation is on revision $PREV_REV"

# ═══ N1. a bundle with a FAILED BINARY ════════════════════════════════════
reh_head "N1  a bundle with a failed binary is rejected before promotion"
WS="$(fresh n1)"
BAD="$WORK/n1-badbundle"
rm -rf "$BAD"; cp -a "$BUNDLE" "$BAD"
# A binary that is present and executable but is not a working program.
printf '#!/bin/sh\nexit 3\n' > "$BAD/bin/mpm"
chmod +x "$BAD/bin/mpm"
OUT="$(bash "$DRIVER" --workspace "$WS" --bundle "$BAD" --rev "n1-bad" --skip-restart 2>&1)"
RC=$?
[ "$RC" != "0" ]; reh_assert $? "the driver refused the bundle (rc=$RC)"
echo "$OUT" | grep -qiE "failed its health check|FAIL"; reh_assert $? "the failure is named, not silent"
[ ! -e "$WS/releases/n1-bad" ]; reh_assert $? "the bad bundle was never installed"
[ "$(readlink "$WS/current" 2>/dev/null)" = "$(readlink "$SIM_WS/current" 2>/dev/null)" ]; reh_assert $? "the active pointer is unchanged"

# ═══ N2. an unwritable destination ════════════════════════════════════════
reh_head "N2  an unwritable destination is rejected"
WS="$(fresh n2)"
# A read-only SOURCE bundle is harmless: it is only ever read. The failure
# that matters is an unwritable DESTINATION — the release directory cannot
# be created or written.
mkdir -p "$WS/releases"
chmod a-w "$WS/releases"
OUT="$(bash "$DRIVER" --workspace "$WS" --bundle "$BUNDLE" --rev "n2-ro" --skip-restart 2>&1)"
RC=$?
chmod u+w "$WS/releases" 2>/dev/null
[ "$RC" != "0" ]; reh_assert $? "the driver refused the promotion (rc=$RC)"
echo "$OUT" | grep -qiE "could not install|permission|denied|read-only"; reh_assert $? "the reason is a filesystem error, and it is named"
[ ! -e "$WS/releases/n2-ro" ] || [ ! -w "$WS/releases/n2-ro" ]; reh_assert $? "no usable release directory was left behind"

# And a read-only SOURCE bundle must still work — it is only read.
WS="$(fresh n2b)"
RO="$WORK/n2-readonly"
rm -rf "$RO"; cp -a "$BUNDLE" "$RO"; chmod -R a-w "$RO"
OUT="$(bash "$DRIVER" --workspace "$WS" --bundle "$RO" --rev "n2-ro-src" --skip-restart 2>&1)"
RC=$?
chmod -R u+w "$RO" 2>/dev/null
[ "$RC" = "0" ]; reh_assert $? "a read-only source bundle is promoted normally (rc=$RC)"

# ═══ N3. a bundle with a MISSING DEPENDENCY ══════════════════════════════
reh_head "N3  a bundle with a missing dependency is rejected"
WS="$(fresh n3)"
BAD="$WORK/n3-incomplete"
rm -rf "$BAD"; cp -a "$BUNDLE" "$BAD"
rm -f "$BAD/bin/mpm-critic"
OUT="$(bash "$DRIVER" --workspace "$WS" --bundle "$BAD" --rev "n3-missing" --skip-restart 2>&1)"
RC=$?
[ "$RC" != "0" ]; reh_assert $? "the driver refused the bundle (rc=$RC)"
echo "$OUT" | grep -q "mpm-critic"; reh_assert $? "the missing binary is named"
[ ! -e "$WS/releases/n3-missing" ]; reh_assert $? "the incomplete bundle was never installed"

# ═══ N4. a service that cannot be STARTED ═════════════════════════════════
reh_head "N4  a service that cannot be started rolls the promotion back"
WS="$(fresh n4)"
OUT="$(MPM_UPGRADE_FAULT=restart-fail bash "$DRIVER" --workspace "$WS" \
        --bundle "$BUNDLE" --rev "n4-restart" 2>&1)"
RC=$?
[ "$RC" != "0" ]; reh_assert $? "the driver reported the failure (rc=$RC)"
echo "$OUT" | grep -qi "ROLLING BACK"; reh_assert $? "the driver rolled the promotion back"
NOW="$(readlink "$WS/current" 2>/dev/null || echo '')"
WAS="$(readlink "$SIM_WS/current" 2>/dev/null || echo '')"
[ "$NOW" = "$WAS" ]; reh_assert $? "the active pointer was restored ($WAS)"
RESTORED="$(readlink -f "$WS/releases/$PREV_REV" 2>/dev/null)"
case "$(readlink -f "$WS/bin/mpm" 2>/dev/null)" in
  "$RESTORED"/*) reh_pass "bin/mpm resolves into the restored revision" ;;
  *) reh_fail "bin/mpm does not resolve into the restored revision: $(readlink -f "$WS/bin/mpm" 2>/dev/null)" ;;
esac
case "$(readlink -f "$WS/agent_installation/mpm-opencode/server.js" 2>/dev/null)" in
  "$RESTORED"/*) reh_pass "the OpenCode payload resolves into the restored revision" ;;
  *) reh_fail "the OpenCode payload does not resolve into the restored revision" ;;
esac
"$WS/bin/mpm" --version >/dev/null 2>&1; reh_assert $? "the restored runtime still runs"

# ═══ N5. an explicit rollback ═════════════════════════════════════════════
reh_head "N5  an explicit rollback returns to the previous revision"
WS="$(fresh n5)"
# Install a SECOND revision alongside the current one, so there is
# something to roll back to.
B2="$WORK/bundle-n5-old"
rm -rf "$B2"; cp -a "$BUNDLE" "$B2"
python3 - "$B2/RELEASE.json" <<'PYEOF'
import json, sys
p = sys.argv[1]
d = json.load(open(p))
d["revision"] = "n5-old"
json.dump(d, open(p, "w"), indent=2)
PYEOF
bash "$DRIVER" --workspace "$WS" --bundle "$B2" --rev "n5-old" --skip-restart >/dev/null 2>&1
[ "$(readlink "$WS/current")" = "releases/n5-old" ]; reh_assert $? "the second revision is active"
OUT="$(bash "$DRIVER" --workspace "$WS" --rollback --rev "$REV" 2>&1)"
RC=$?
[ "$RC" = "0" ]; reh_assert $? "the rollback succeeded (rc=$RC)"
[ "$(readlink "$WS/current")" = "releases/$REV" ]; reh_assert $? "the active pointer is back at the previous revision"
"$WS/bin/mpm" --version 2>&1 | head -1 | grep -q "g$REV"; reh_assert $? "the rolled-back runtime reports the right revision"
[ -d "$WS/releases/n5-old" ]; reh_assert $? "the rolled-back revision is retained on disk"
[ -d "$WS/releases/$REV" ]; reh_assert $? "the previous revision is retained on disk"

# ═══ N6. a FAILED STAGING ════════════════════════════════════════════════
reh_head "N6  a failed staging is recovered from"
WS="$(fresh n6)"
BAD="$WORK/n6-nopayload"
rm -rf "$BAD"; cp -a "$BUNDLE" "$BAD"
rm -rf "$BAD/agent_installation/mpm-opencode"
OUT="$(bash "$DRIVER" --workspace "$WS" --bundle "$BAD" --rev "n6-nopayload" --skip-restart 2>&1)"
RC=$?
[ "$RC" != "0" ]; reh_assert $? "the driver refused the bundle (rc=$RC)"
echo "$OUT" | grep -qi "no OpenCode payload"; reh_assert $? "the missing payload is named"
[ ! -e "$WS/releases/n6-nopayload" ]; reh_assert $? "the incomplete bundle was never installed"
# Recovery: the good bundle still promotes cleanly afterwards.
bash "$DRIVER" --workspace "$WS" --bundle "$BUNDLE" --rev "$REV" --skip-restart >/dev/null 2>&1
[ $? -eq 0 ]; reh_assert $? "the installation recovers — a good bundle promotes afterwards"
[ "$(readlink "$WS/current")" = "releases/$REV" ]; reh_assert $? "the recovered installation is on the good revision"

# ═══ N7. an INTERRUPTED PROMOTION ═════════════════════════════════════════
reh_head "N7  an interrupted promotion is recovered from"
for POINT in after-stage after-install after-swap after-convert; do
  WS="$(fresh "n7-$POINT")"
  BEFORE_FP="$(python3 "$REH_DIR/state_fingerprint.py" "$WS/src/db/mpm.db" --content)"

  MPM_UPGRADE_FAULT="$POINT" bash "$DRIVER" --workspace "$WS" \
    --bundle "$BUNDLE" --rev "n7-$POINT" > "$WORK/n7-$POINT.log" 2>&1
  RC=$?
  [ "$RC" != "0" ]; reh_assert $? "[$POINT] the driver died (rc=$RC)"

  AFTER_FP="$(python3 "$REH_DIR/state_fingerprint.py" "$WS/src/db/mpm.db" --content)"
  [ "$BEFORE_FP" = "$AFTER_FP" ]; reh_assert $? "[$POINT] the persistent state is intact after the interruption"

  bash "$DRIVER" --workspace "$WS" --bundle "$BUNDLE" --rev "n7-$POINT" --skip-restart \
    > "$WORK/n7-$POINT-recover.log" 2>&1
  RC2=$?
  [ "$RC2" = "0" ]; reh_assert $? "[$POINT] a re-run completes the promotion (rc=$RC2)"
  [ "$(readlink "$WS/current" 2>/dev/null)" = "releases/n7-$POINT" ]; reh_assert $? "[$POINT] the recovered installation points at the new revision"
  "$WS/bin/mpm" --version 2>&1 | head -1 | grep -q "g$REV"; reh_assert $? "[$POINT] the recovered runtime works"

  REC_FP="$(python3 "$REH_DIR/state_fingerprint.py" "$WS/src/db/mpm.db" --content)"
  [ "$BEFORE_FP" = "$REC_FP" ]; reh_assert $? "[$POINT] the state is intact after the recovery too"
done

# ═══ N8. the state was never corrupted ════════════════════════════════════
reh_head "N8  no failure corrupted the persistent state"
CORRUPT=0
for d in "$WORK"/n1 "$WORK"/n2 "$WORK"/n2b "$WORK"/n3 "$WORK"/n4 "$WORK"/n5 "$WORK"/n6 "$WORK"/n7-*; do
  [ -d "$d" ] || continue
  BIC="$(sqlite3 "file:$d/src/db/mpm.db?mode=ro" 'PRAGMA integrity_check;' 2>/dev/null | head -1)"
  [ "$BIC" = "ok" ] || { CORRUPT=1; echo "    $d: $BIC"; }
done
[ "$CORRUPT" = "0" ]; reh_assert $? "every workspace still passes integrity_check"

# The live installation, once more.
LIVE_NOW="$(live_snapshot)"
LIVE_THEN="$(live_snapshot)"
[ "$LIVE_NOW" = "$LIVE_THEN" ]; reh_assert $? "the live tree is untouched by the negative tests"

reh_summary "negative tests"
