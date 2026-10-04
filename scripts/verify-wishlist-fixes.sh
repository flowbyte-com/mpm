#!/usr/bin/env bash
# verify-wishlist-fixes.sh — Smoke test for the 2026-08-31 wishlist fixes.
#
# Covers: W-003, W-007, W-009, W-010, W-011, W-013, W-015.
# Run from repo root after `make build`.
#
# Exit code: 0 if all assertions pass, 1 if any fail.
#
# MPM and DB resolution (see #T-2026-10-04 verify-DB-targeting audit):
#   The W-011 test path performs a write (rm --force + UPDATE memories
#   SET deleted_at = NULL) against the production database. Running
#   this script from the wrong directory previously let it silently
#   target a non-default path; sqlite3's empty-on-missing-file made
#   that look like a green run. We now resolve both paths from
#   BASH_SOURCE[0] and refuse to run if either is missing.

set -uo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
REPO_ROOT="$(cd "${SCRIPT_DIR}/.." && pwd)"

MPM="${MPM:-${REPO_ROOT}/bin/mpm}"
DB="${DB:-${REPO_ROOT}/src/db/mpm.db}"

# Pre-flight: refuse to run if MPM or DB cannot be located. The W-011
# path mutates the database (rm --force on a prime-directive memory,
# then UPDATE to restore), so a wrong-target DB means a wrong-target
# write. Bail out cleanly instead.
if [ ! -x "${MPM}" ]; then
  echo "ERR: MPM binary not found or not executable at ${MPM}" >&2
  echo "  hint: run \`make build\` first, or set MPM=/absolute/path/to/mpm" >&2
  exit 2
fi
if [ ! -f "${DB}" ]; then
  echo "ERR: MPM database not found at ${DB}" >&2
  echo "  hint: run \`make build\` first, or set DB=/absolute/path/to/mpm.db" >&2
  exit 2
fi
if ! command -v sqlite3 >/dev/null 2>&1; then
  echo "ERR: sqlite3 not on PATH; required for direct DB assertions" >&2
  exit 2
fi

PASS=0
FAIL=0

assert_pass() {
  echo "  ✓ $*"
  PASS=$((PASS + 1))
}

assert_fail() {
  echo "  ✗ $*"
  FAIL=$((FAIL + 1))
}

section() {
  echo
  echo "=== $* ==="
}

# -----------------------------------------------------------------------------
section "W-007: machine-mode stderr silence"

# Capture stderr into a tmpfile so we can byte-count it.
TMP_ERR=$(mktemp)
TMP_OUT=$(mktemp)
"$MPM" call mpm_memory --payload '{"action":"query","params":{"query":"hello","limit":1}}' \
  2>"$TMP_ERR" 1>"$TMP_OUT" >/dev/null
STDERR_BYTES=$(wc -c <"$TMP_ERR")
rm -f "$TMP_ERR" "$TMP_OUT"

if [[ "$STDERR_BYTES" -eq 0 ]]; then
  assert_pass "stderr is 0 bytes (was 300-800 bytes in audit)"
else
  assert_fail "stderr leaked $STDERR_BYTES bytes"
fi

# -----------------------------------------------------------------------------
section "W-009: limit=-1 returns hard error"

OUT=$("$MPM" call mpm_memory \
  --payload '{"action":"query","params":{"query":"hello","limit":-1}}' 2>/dev/null)
# The error message wraps the field name in backticks. Match the literal
# needle (no command-substitution risk because we're inside single quotes
# at the grep call site).
if printf '%s\n' "$OUT" | grep -qF '`limit` must be'; then
  assert_pass "limit=-1 returned: $OUT"
else
  assert_fail "limit=-1 should error; got: $OUT"
fi

# -----------------------------------------------------------------------------
section "W-010: invalid work status enum returns hard error"

OUT=$("$MPM" call mpm_work \
  --payload '{"action":"list","params":{"status":"openx"}}' 2>/dev/null)
if printf '%s\n' "$OUT" | grep -qF 'must be one of'; then
  assert_pass "status=openx returned: $OUT"
else
  assert_fail "status=openx should error; got: $OUT"
fi

# -----------------------------------------------------------------------------
section "W-011: prime directive armor for mpm rm"

# Find any prime-directive memory to test against.
PRIME_ID=$(sqlite3 "$DB" \
  "SELECT id FROM memories WHERE is_prime_directive=1 AND deleted_at IS NULL LIMIT 1;" 2>/dev/null)
if [[ -z "$PRIME_ID" ]]; then
  assert_fail "no prime-directive memory in DB — cannot test"
else
  # Without --force: must refuse.
  REFUSED=$("$MPM" rm "$PRIME_ID" 2>&1 | grep -c "refusing to delete" || true)
  if [[ "$REFUSED" -gt 0 ]]; then
    assert_pass "rm without --force refuses deletion"
  else
    assert_fail "rm without --force did not refuse"
  fi

  # With --force: must succeed and delete.
  "$MPM" rm "$PRIME_ID" --force >/dev/null 2>&1
  STILL_LIVE=$(sqlite3 "$DB" \
    "SELECT deleted_at IS NULL FROM memories WHERE id='$PRIME_ID';" 2>/dev/null)
  if [[ "$STILL_LIVE" == "0" ]]; then
    assert_pass "rm with --force deleted prime-directive memory"
  else
    assert_fail "rm with --force did not delete (deleted_at IS NULL=$STILL_LIVE)"
  fi

  # Restore the memory so the test is idempotent.
  sqlite3 "$DB" "UPDATE memories SET deleted_at = NULL WHERE id='$PRIME_ID';"
fi

# -----------------------------------------------------------------------------
section "W-013: tags returned as JSON list, not string"

CANARY="WISH-VERIFY-$(date +%s)"
SAVE_OUT=$("$MPM" call mpm_memory \
  --payload "{\"action\":\"save\",\"params\":{\"fact\":\"$CANARY\",\"tags\":[\"verify\",\"w013\"]}}" \
  2>/dev/null)
NEW_ID=$(echo "$SAVE_OUT" | python3 -c "import json,sys; print(json.load(sys.stdin).get('id',''))" 2>/dev/null)

if [[ -z "$NEW_ID" ]]; then
  assert_fail "could not save test memory: $SAVE_OUT"
else
  TAGS_TYPE=$("$MPM" call mpm_memory \
    --payload "{\"action\":\"query\",\"params\":{\"query\":\"$CANARY\",\"limit\":1}}" \
    2>/dev/null | python3 -c "
import json,sys
r = json.load(sys.stdin)
mems = r.get('memories', [])
m = next((m for m in mems if m.get('id') == '$NEW_ID'), None)
if m is None:
    print('MISS')
else:
    print(type(m.get('tags')).__name__)
" 2>/dev/null)

  if [[ "$TAGS_TYPE" == "list" ]]; then
    assert_pass "tags is a JSON list in query response"
  else
    assert_fail "tags is $TAGS_TYPE, expected list"
  fi

  "$MPM" shred "$NEW_ID" >/dev/null 2>&1 || true
fi

# -----------------------------------------------------------------------------
section "W-003: show returns exact memory record"

SAVE_OUT=$("$MPM" call mpm_memory \
  --payload "{\"action\":\"save\",\"params\":{\"fact\":\"WISH-W003-CANARY\",\"tags\":[\"w003\"]}}" \
  2>/dev/null)
NEW_ID=$(echo "$SAVE_OUT" | python3 -c "import json,sys; print(json.load(sys.stdin).get('id',''))" 2>/dev/null)

if [[ -z "$NEW_ID" ]]; then
  assert_fail "could not save test memory: $SAVE_OUT"
else
  SHOW=$("$MPM" call mpm_memory \
    --payload "{\"action\":\"show\",\"params\":{\"id\":\"$NEW_ID\"}}" \
    2>/dev/null)
  RESULT=$(echo "$SHOW" | python3 -c "
import json,sys
r = json.load(sys.stdin)
m = r.get('memory', r)
ok = (
    m.get('id') == '$NEW_ID' and
    m.get('content') == 'WISH-W003-CANARY' and
    isinstance(m.get('tags'), list)
)
print('PASS' if ok else 'FAIL')
" 2>/dev/null)

  if [[ "$RESULT" == "PASS" ]]; then
    assert_pass "show returned exact record (id, content, tags)"
  else
    assert_fail "show returned unexpected payload: $SHOW"
  fi

  "$MPM" shred "$NEW_ID" >/dev/null 2>&1 || true
fi

# -----------------------------------------------------------------------------
section "W-015: show_confidence and explain_confidence share a source of truth"

# Both surfaces must call ExplainConfidence. Verify by calling each and
# checking that the discrepancy is bounded by time-decay precision
# (sub-millisecond calls should agree to ~1e-12).
SHOW=$("$MPM" call mpm_confidence \
  --payload '{"action":"show","params":{"artifact_id":"mpm-seed-read-wake-context"}}' \
  2>/dev/null)
EXPLAIN=$("$MPM" call mpm_confidence \
  --payload '{"action":"explain","params":{"artifact_id":"mpm-seed-read-wake-context"}}' \
  2>/dev/null)

RESULT=$(SHOW="$SHOW" EXPLAIN="$EXPLAIN" python3 -c "
import json, os
s = json.loads(os.environ['SHOW'])
e = json.loads(os.environ['EXPLAIN'])
sc = s.get('confidence')
ec = e.get('explanation', {}).get('confidence')
# Both should be present and agree to within 1e-6 (decay-clock drift).
if sc is None or ec is None:
    print('MISS')
elif abs(sc - ec) < 1e-6:
    print(f'PASS ({sc:.6f} ~ {ec:.6f})')
else:
    print(f'DRIFT ({sc:.6f} vs {ec:.6f}, diff {abs(sc-ec):.2e})')
")

if [[ "$RESULT" == PASS* ]]; then
  assert_pass "show and explain return same confidence ($RESULT)"
else
  assert_fail "show/explain diverge: $RESULT"
fi

# -----------------------------------------------------------------------------
echo
echo "============================================================"
echo "Results: $PASS passed, $FAIL failed"
echo "============================================================"

if [[ "$FAIL" -gt 0 ]]; then
  exit 1
fi
