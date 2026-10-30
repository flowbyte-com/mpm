#!/usr/bin/env bash
# rehearsal/lib.sh — shared helpers for the runtime-migration rehearsal.
#
# EVERYTHING HERE IS ABOUT ONE THING: proving the migration procedure
# WITHOUT TOUCHING THE LIVE INSTALLATION.
#
# The live artifacts are:
#   $HOME/.mpm                  the live workspace (binaries + database)
#   $HOME/.config/opencode      the live OpenCode config
#   $HOME/.config/systemd/user  the live systemd user units
#
# Three independent detectors, because one is not enough:
#
#   1. A CLASSIFIED SNAPSHOT of the live tree, before and after.  The
#      protected artifacts are compared BY BYTES; the live database is
#      compared BY CLASS, because the live scheduler legitimately writes
#      to it and a blanket size/mtime comparison is a false positive.
#      See live_gate.py.
#   2. A STRACE of every open/stat/access in the test's process tree,
#      grepped for the live paths. This catches a READ that a snapshot
#      cannot see.
#   3. A PATH SHIM that neutralises the systemd user-manager operations.
#      Starting or restarting the operator's unit IS a deployment.
#
# ── THE DESTRUCTIVE-COMMAND GUARD ─────────────────────────────────────────
#
# Every recursive delete in this harness goes through safe_rm, which
# REFUSES to remove anything that is not under the rehearsal root. This
# harness has already destroyed itself once — a recursive delete with a
# mis-composed path took the whole rehearsal tree with it — so the guard
# is not defensive decoration: it is the mechanism that makes the rest of
# the harness safe to run unattended.

LIVE_MPM="${LIVE_MPM:-$HOME/.mpm}"
LIVE_OC_CONFIG="${LIVE_OC_CONFIG:-$HOME/.config/opencode}"
LIVE_SYSTEMD_USER="${LIVE_SYSTEMD_USER:-$HOME/.config/systemd/user}"

REH_ROOT="${REH_ROOT:-$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd -P)}"

# REH_WORK is where the harness puts its WORK PRODUCTS: the source
# export, the built bundle, the simulated home, the traces and the
# logs. It is separate from REH_ROOT so that the harness-integrity
# guard (20_negative.sh) fingerprints the HARNESS — the scripts and
# the helpers — rather than the output it produces.
#
# IT IS OUTSIDE THE REPOSITORY, AND THAT IS NOT A MATTER OF TASTE.
#
# The work products include a COPY of the source tree, and this
# repository has repo-wide scanners that walk every file under the
# root: the DB-safety guard (which fails on a test file that calls a
# production-path constructor) and the deferral guard (which fails on
# a non-test file that names RequeueDeferred). Both of them found the
# COPY, and both failed, because the copy contains the very files they
# are looking for. A copy of the repository inside the repository is
# incompatible with scanning the repository.
#
# The default is the conventional cache location, which is also where
# the harness lived before it was vendored — and which survived a
# reboot, unlike /tmp.
REH_WORK="${REH_WORK:-${XDG_CACHE_HOME:-$HOME/.cache}/mpm-rehearsal}"

# The repository root, for the in-repository check below.
REH_REPO_ROOT="$(cd "$REH_DIR/../.." && pwd -P)"

# Refuse a work directory inside the repository.
#
# The check is fail-closed: the harness stops with a clear message
# rather than writing a copy of the source tree into a directory whose
# repo-wide scanners will then fail on it.
case "$REH_WORK" in
  "$REH_REPO_ROOT"/*)
    echo "lib.sh: REFUSING to use a work directory inside the repository:" >&2
    echo "  $REH_WORK" >&2
    echo "  The work products include a COPY of the source tree, and the" >&2
    echo "  repository's repo-wide scanners walk every file under the root." >&2
    echo "  Use a directory outside it (the default is \$XDG_CACHE_HOME/mpm-rehearsal)." >&2
    exit 2
    ;;
esac

# REH_REV is the revision the harness builds and rehearses against.
# It is a variable so the rehearsal can be re-run against a
# different revision without editing the scripts — the operator
# checklist requires exactly that.
REH_REV="${REH_REV:-4111c46}"

mkdir -p "$REH_WORK"

# The staged revision, recorded by setup.sh.
#
# setup.sh resolves the revision the operator asked for into the ACTUAL
# short commit SHA of the tree under test, and records it here. The
# suites read the recorded value rather than the requested one, so an
# assertion like "the runtime reports +g<short sha>" is checked against
# what was really built -- "HEAD" is a name, not an identity.
if [ -f "$REH_WORK/rehearsal-revision.env" ]; then
  . "$REH_WORK/rehearsal-revision.env"
fi

# EXPECT_DIRTY is the source state the staged build was made from.
# A rehearsal of the working tree is dirty by definition, and the
# identity the binaries carry must SAY so.
EXPECT_DIRTY="${EXPECT_DIRTY:-false}"

# safe_rm <path>... — remove only inside the rehearsal root or the work
# directory.
#
# BOTH are in scope: the harness's own files live in REH_ROOT, and the
# work products live in REH_WORK. The guard is what makes the rest of
# the harness safe to run unattended.
safe_rm() {
  local p
  for p in "$@"; do
    [ -e "$p" ] || [ -L "$p" ] || continue
    case "$p" in
      "$REH_ROOT"/*|"$REH_WORK"/*) : ;;
      *) echo "safe_rm: REFUSING to remove $p (outside REH_ROOT and REH_WORK)" >&2; return 1 ;;
    esac
    # A path that resolves to the rehearsal root itself is refused too.
    if [ "$(readlink -f "$p" 2>/dev/null || echo "$p")" = "$REH_ROOT" ]; then
      echo "safe_rm: REFUSING to remove the rehearsal root itself" >&2
      return 1
    fi
    chmod -R u+w "$p" 2>/dev/null
    rm -rf "$p"
  done
  return 0
}

# ── the live-tree isolation gate ───────────────────────────────────────────
#
# The gate is a Python helper, because it has to read the live database
# and hash trees, and because it must be testable on its own: the
# negative control in scripts/tests/test_rehearsal_live_gate.py drives
# it against a DISPOSABLE stand-in for the live tree, so the gate's own
# behaviour is proven without touching production.
#
# live_gate_snapshot <out-file>   — write a classified snapshot
# live_gate_compare <before> <after> — compare two snapshots
#                                    0 = clean, 1 = unauthorised change,
#                                    2 = could not measure
#
# The old live_snapshot() — a hash of (path, size, mtime) over a list of
# live paths, INCLUDING the live database file — was removed. It was
# invalid: the live scheduler records health checks and SQLite
# checkpoints the WAL, so the database's size and mtime move for reasons
# that have nothing to do with the rehearsal, and the suite reported a
# failure that had no cause. A false positive in an isolation gate is
# worse than no gate, because it teaches the operator to ignore it.

live_gate_snapshot() { # live_gate_snapshot <out-file>
  python3 "$REH_DIR/live_gate.py" snapshot \
    --live-mpm "$LIVE_MPM" \
    --oc-config "$LIVE_OC_CONFIG" \
    --systemd-user "$LIVE_SYSTEMD_USER" \
    --out "$1"
}

live_gate_compare() { # live_gate_compare <before> <after>
  python3 "$REH_DIR/live_gate.py" compare "$1" "$2"
}

# The live database the gate measures.  It is the live workspace's own
# database; the gate never opens it for writing.
live_db_path() {
  printf '%s\n' "$LIVE_MPM/src/db/mpm.db"
}

# ── the strace detector ───────────────────────────────────────────────────
strace_live_hits() {
  local trace="$1"
  [ -f "$trace" ] || { echo "0"; return; }
  grep -E "openat|open\(|stat|statx|access|getdents" "$trace" 2>/dev/null \
    | grep -F -e "$LIVE_MPM" -e "$LIVE_OC_CONFIG" -e "$LIVE_SYSTEMD_USER" \
    | grep -v -F "$LIVE_MPM-dogfood" \
    | grep -v -F "/rehearsal/" \
    | wc -l | tr -d ' '
}

# ── the systemd shim ──────────────────────────────────────────────────────
# The shim RECORDS the request and does not act on the real user manager.
# It keeps its own state file so that "stop then start" behaves like a
# real service for the purposes of the rehearsal.
make_systemd_shim() {
  local dir="$1"
  mkdir -p "$dir"
  cat > "$dir/systemctl" <<'SHIM'
#!/usr/bin/env bash
# systemd shim — RECORDS the request, does not act on the real user manager.
STATE="${REHEARSAL_SHIM_STATE:-/tmp/rehearsal-systemd.state}"
echo "systemctl $*" >> "${REHEARSAL_SHIM_LOG:-/dev/null}"
svc=""
act=""
for a in "$@"; do
  case "$a" in
    --user|--quiet|--no-pager|--now) ;;
    is-active|is-failed|is-enabled) act="check" ;;
    stop|start|restart|reload|enable|disable) act="$a" ;;
    *) [ -z "$svc" ] && svc="$a" ;;
  esac
done
case "$act" in
  check)
    if [ -f "$STATE" ] && grep -q "^active:yes" "$STATE"; then exit 0; else exit 3; fi ;;
  stop)
    sed -i 's/^active:.*/active:no/' "$STATE" 2>/dev/null; exit 0 ;;
  start|restart)
    echo "active:yes" > "$STATE"; exit 0 ;;
  *) exit 0 ;;
esac
SHIM
  cat > "$dir/loginctl" <<'SHIM'
#!/usr/bin/env bash
echo "loginctl $*" >> "${REHEARSAL_SHIM_LOG:-/dev/null}"
exit 0
SHIM
  chmod +x "$dir/systemctl" "$dir/loginctl"
  echo "active:no" > "${REHEARSAL_SHIM_STATE:-/tmp/rehearsal-systemd.state}"
}

# ── assertions ────────────────────────────────────────────────────────────
REH_PASS=0
REH_FAIL=0
REH_LIVE_HITS=0

reh_pass() { REH_PASS=$((REH_PASS+1)); printf '  \033[32mPASS\033[0m %s\n' "$1"; }
reh_fail() { REH_FAIL=$((REH_FAIL+1)); printf '  \033[31mFAIL\033[0m %s\n' "$1"; }
reh_head() { printf '\n\033[1m%s\033[0m\n' "$1"; }
reh_note() { printf '       %s\n' "$1"; }

reh_assert() { # reh_assert <rc> <description>
  if [ "$1" = "0" ]; then reh_pass "$2"; else reh_fail "$2"; fi
}

reh_summary() {
  printf '\n\033[1m== %s ==\033[0m\n' "${1:-summary}"
  printf '  passed: %d\n  failed: %d\n' "$REH_PASS" "$REH_FAIL"
  [ "$REH_FAIL" -eq 0 ]
}
