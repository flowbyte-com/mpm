#!/usr/bin/env bash
# verify-m3-audit-fixes.sh — Smoke test for the 2026-08-31 M3 audit remediation.
#
# Covers: D-001/002/003, D-004, D-005, D-006, D-008, D-009, D-010, D-013,
# D-015/021, D-016, D-017, D-018, D-019, D-020, D-022, D-023.
# Run from repo root after `make build`.
#
# Exit code: 0 if all assertions pass, 1 if any fail.
#
# MPM and DB resolution (see #T-2026-10-04 verify-DB-targeting audit):
#   The script previously defaulted both MPM and DB to CWD-relative
#   paths (./bin/mpm, ./src/db/mpm.db). When the script is run from
#   anywhere other than the repo root, those paths silently resolve to
#   non-existent files: sqlite3 returns empty strings, the assertions
#   degenerate to PASS-or-FAIL based on whether the helper succeeded,
#   and the smoke test produces a meaningless "all green" run. We
#   resolve both paths from BASH_SOURCE[0] (this script's own
#   location) so the suite is CWD-independent. The override knobs
#   MPM= and DB= are preserved for operators who explicitly want to
#   test a non-default build.

set -uo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
REPO_ROOT="$(cd "${SCRIPT_DIR}/.." && pwd)"

MPM="${MPM:-${REPO_ROOT}/bin/mpm}"
DB="${DB:-${REPO_ROOT}/src/db/mpm.db}"

# Pre-flight: refuse to run if MPM or DB cannot be located. Without
# this, sqlite3 silently returns empty for every query and the suite
# can produce a vacuous green run on the wrong database.
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

# Helper: run `mpm call` capturing only stdout (not the human-readable logs
# that mpm may emit on stderr in some shells).
call_only() {
  "$MPM" call "$@" 2>/dev/null
}

# -----------------------------------------------------------------------------
section "D-001/002/003: theory parser supports all four documented forms"

# Long-flag form
ID_FLAGS=$(call_only mpm_theories --payload '{"action":"propose","params":{"hypothesis":"m3-test-1","validation_criteria":"some criterion"}}' | python3 -c "import json,sys; print(json.load(sys.stdin).get('id',''))" 2>/dev/null)
[[ -n "$ID_FLAGS" ]] && assert_pass "long-flag form via mpm call: id=$ID_FLAGS" || assert_fail "long-flag form"

# Bare key=value form via CLI
ID_BARE=$("$MPM" theorize 'hypothesis="m3-test-2"' 'validation="some criterion"' --json 2>&1 | grep -oE '"id":"[^"]*"' | head -1 | cut -d'"' -f4)
[[ -n "$ID_BARE" ]] && assert_pass "bare key=value form via CLI: id=$ID_BARE" || assert_fail "bare key=value form"

# Same content + tags → same id (content-hash idempotency)
ID_BARE2=$("$MPM" theorize 'hypothesis="m3-test-2"' 'validation="some criterion"' --json 2>&1 | grep -oE '"id":"[^"]*"' | head -1 | cut -d'"' -f4)
[[ "$ID_BARE" == "$ID_BARE2" ]] && assert_pass "idempotent re-save returns same id" || assert_fail "different ids on re-save: $ID_BARE vs $ID_BARE2"

# Pipe-separated positional form
ID_PIPE=$("$MPM" theorize "m3-test-3 | some criterion" --json 2>&1 | grep -oE '"id":"[^"]*"' | head -1 | cut -d'"' -f4)
[[ -n "$ID_PIPE" ]] && assert_pass "pipe-separated positional form: id=$ID_PIPE" || assert_fail "pipe form"

# Validation criteria actually stored
for tid in "$ID_FLAGS" "$ID_BARE" "$ID_PIPE"; do
  if [[ -n "$tid" ]]; then
    vc=$(sqlite3 "$DB" "SELECT json_extract(metadata,'\$.validation_criteria') FROM memories WHERE id='$tid' AND deleted_at IS NULL;" 2>/dev/null)
    if [[ "$vc" == "some criterion" ]]; then
      assert_pass "validation_criteria stored for $tid"
    else
      assert_fail "validation_criteria missing for $tid: '$vc'"
    fi
  fi
done

# -----------------------------------------------------------------------------
section "D-010: theory state vocabulary — invalid conclusion errors, valid maps"

THEORY_FOR_RESOLVE=$(call_only mpm_theories --payload '{"action":"propose","params":{"hypothesis":"d010-test-'"$(date +%s)"'","validation_criteria":"x"}}' | python3 -c "import json,sys; print(json.load(sys.stdin).get('id',''))" 2>/dev/null)

if [[ -z "$THEORY_FOR_RESOLVE" ]]; then
  assert_fail "could not create theory for D-010 test"
else
  # Invalid conclusion should error, not silently write "resolved"
  INVALID_OUT=$("$MPM" resolve_theory "$THEORY_FOR_RESOLVE" "some random text" 2>&1 | grep -v "^time=" | head -1)
  if printf '%s\n' "$INVALID_OUT" | grep -qF "conclusion must be one of"; then
    assert_pass "invalid conclusion refused: $INVALID_OUT"
  else
    assert_fail "invalid conclusion should error; got: $INVALID_OUT"
  fi

  # Valid conclusion maps to status=proven
  "$MPM" resolve_theory "$THEORY_FOR_RESOLVE" "confirmed" >/dev/null 2>&1
  RESOLVED_STATUS=$(sqlite3 "$DB" "SELECT json_extract(metadata,'\$.status') FROM memories WHERE id='$THEORY_FOR_RESOLVE';" 2>/dev/null)
  if [[ "$RESOLVED_STATUS" == "proven" ]]; then
    assert_pass "valid 'confirmed' resolves to status=proven"
  else
    assert_fail "expected proven, got '$RESOLVED_STATUS'"
  fi
fi

# -----------------------------------------------------------------------------
section "D-016: prime-directive memory deletion armor"

PRIME_ID=$(sqlite3 "$DB" "SELECT id FROM memories WHERE is_prime_directive=1 AND deleted_at IS NULL LIMIT 1;" 2>/dev/null)
if [[ -z "$PRIME_ID" ]]; then
  assert_fail "no prime-directive memory in DB — cannot test"
else
  REFUSED=$("$MPM" rm "$PRIME_ID" 2>&1 | grep -c "refusing to delete" || true)
  [[ "$REFUSED" -gt 0 ]] && assert_pass "rm without --force refuses deletion" || assert_fail "rm without --force did not refuse"

  "$MPM" rm "$PRIME_ID" --force >/dev/null 2>&1
  STILL_LIVE=$(sqlite3 "$DB" "SELECT deleted_at IS NULL FROM memories WHERE id='$PRIME_ID';" 2>/dev/null)
  if [[ "$STILL_LIVE" == "0" ]]; then
    assert_pass "rm with --force deleted prime-directive memory"
  else
    assert_fail "rm with --force did not delete"
  fi
  sqlite3 "$DB" "UPDATE memories SET deleted_at = NULL WHERE id='$PRIME_ID';"
fi

# -----------------------------------------------------------------------------
section "D-017: duplicate creation message — distinguish create vs already-exists"

CANARY="M3-D017-CANARY-$(date +%s)"
FIRST=$("$MPM" add "$CANARY" 2>&1 | tail -1)
if printf '%s\n' "$FIRST" | grep -qF "Added memory"; then
  assert_pass "first add: $FIRST"
else
  assert_fail "first add should say 'Added'; got: $FIRST"
fi

# Wait >2s so the created_at window catches the dedup hit
sleep 3
SECOND=$("$MPM" add "$CANARY" 2>&1 | tail -1)
if printf '%s\n' "$SECOND" | grep -qF "Memory already exists"; then
  assert_pass "second add: $SECOND"
else
  assert_fail "second add should say 'already exists'; got: $SECOND"
fi

# -----------------------------------------------------------------------------
section "D-019: evidence list returns wrapped envelope"

EVIDENCE_ID=$(sqlite3 "$DB" "SELECT id FROM memories WHERE deleted_at IS NULL LIMIT 1;" 2>/dev/null)
"$MPM" evidence add --artifact "$EVIDENCE_ID" --type observation --source filesystem --by "verify-m3" --note "envelope verify" >/dev/null 2>&1
LIST_OUT=$("$MPM" evidence list --artifact "$EVIDENCE_ID" 2>/dev/null | grep -o '{.*}' | head -1)
if printf '%s\n' "$LIST_OUT" | python3 -c "
import json, sys
r = json.loads(sys.stdin.read())
need = {'success', 'count', 'evidence', 'artifact_id', 'artifact_type'}
have = set(r.keys())
missing = need - have
sys.exit(1 if missing else 0)
" 2>/dev/null; then
  assert_pass "evidence list returns wrapped envelope: keys=success,count,evidence,artifact_id,artifact_type"
else
  assert_fail "evidence list missing envelope keys: $LIST_OUT"
fi

# -----------------------------------------------------------------------------
section "D-023: --note is canonical, --notes is alias"

"$MPM" evidence add --artifact "$EVIDENCE_ID" --type observation --source filesystem --by "verify-m3" --note "canonical form" >/dev/null 2>&1
CANONICAL_OK=$?
"$MPM" evidence add --artifact "$EVIDENCE_ID" --type observation --source filesystem --by "verify-m3" --notes "alias form" >/dev/null 2>&1
ALIAS_OK=$?
[[ "$CANONICAL_OK" == "0" ]] && assert_pass "--note (canonical) accepted" || assert_fail "--note rejected"
[[ "$ALIAS_OK" == "0" ]] && assert_pass "--notes (alias) accepted" || assert_fail "--notes alias rejected"

# -----------------------------------------------------------------------------
section "D-006: machine-mode stderr silence"

TMP_ERR=$(mktemp)
# Invoke "$MPM" directly, not through call_only. call_only's body ends in
# `2>/dev/null`, and a redirection inside a function body supersedes the
# inherited one — so the caller's 2>"$TMP_ERR" never received anything and
# STDERR_BYTES was structurally 0, making this assertion unable to fail.
# Verified with a stub binary that writes 79 bytes to stderr: it still
# reported "stderr is 0 bytes".
"$MPM" call mpm_memory --payload '{"action":"query","params":{"query":"hello","limit":1}}' 2>"$TMP_ERR" >/dev/null
STDERR_BYTES=$(wc -c <"$TMP_ERR")
rm -f "$TMP_ERR"
if [[ "$STDERR_BYTES" -eq 0 ]]; then
  assert_pass "stderr is 0 bytes for clean machine call"
else
  assert_fail "stderr leaked $STDERR_BYTES bytes"
fi

# -----------------------------------------------------------------------------
section "D-005: mpm_memory show returns exact record"

CANARY="M3-D005-CANARY-$(date +%s)"
NEW_ID=$(call_only mpm_memory --payload "{\"action\":\"save\",\"params\":{\"fact\":\"$CANARY\",\"tags\":[\"d005\"]}}" | python3 -c "import json,sys; print(json.load(sys.stdin).get('id',''))" 2>/dev/null)
if [[ -n "$NEW_ID" ]]; then
  SHOW=$(call_only mpm_memory --payload "{\"action\":\"show\",\"params\":{\"id\":\"$NEW_ID\"}}")
  OK=$(printf '%s\n' "$SHOW" | python3 -c "
import json,sys
r = json.load(sys.stdin)
m = r.get('memory', r)
print('PASS' if (m.get('id') == '$NEW_ID' and m.get('content') == '$CANARY') else 'FAIL')
" 2>/dev/null)
  if [[ "$OK" == "PASS" ]]; then
    assert_pass "show returned exact record"
  else
    assert_fail "show returned unexpected payload: $SHOW"
  fi
  "$MPM" shred "$NEW_ID" >/dev/null 2>&1 || true
fi

# -----------------------------------------------------------------------------
section "D-008: query limit validation"

OUT=$(call_only mpm_memory --payload '{"action":"query","params":{"query":"hello","limit":-1}}')
if printf '%s\n' "$OUT" | grep -qE '(limit must|limit.*0|0, got)'; then
  assert_pass "limit=-1 returned hard error: $OUT"
else
  assert_fail "limit=-1 should error; got: $OUT"
fi

# -----------------------------------------------------------------------------
section "D-009: invalid work status enum returns hard error"

OUT=$(call_only mpm_work --payload '{"action":"list","params":{"status":"openx"}}')
if printf '%s\n' "$OUT" | grep -qF 'must be one of'; then
  assert_pass "status=openx returned: $OUT"
else
  assert_fail "status=openx should error; got: $OUT"
fi

# -----------------------------------------------------------------------------
section "D-015/021: tags returned as JSON list, not string"

CANARY="M3-D015-CANARY-$(date +%s)"
NEW_ID=$(call_only mpm_memory --payload "{\"action\":\"save\",\"params\":{\"fact\":\"$CANARY\",\"tags\":[\"verify\",\"d015\"]}}" | python3 -c "import json,sys; print(json.load(sys.stdin).get('id',''))" 2>/dev/null)
TAGS_TYPE=$(call_only mpm_memory --payload "{\"action\":\"query\",\"params\":{\"query\":\"$CANARY\",\"limit\":1}}" | python3 -c "
import json,sys
r = json.load(sys.stdin)
mems = r.get('memories', [])
m = next((m for m in mems if m.get('id') == '$NEW_ID'), None)
if m is None: print('MISS')
else: print(type(m.get('tags')).__name__)
" 2>/dev/null)
[[ "$TAGS_TYPE" == "list" ]] && assert_pass "tags is a JSON list" || assert_fail "tags is $TAGS_TYPE"
"$MPM" shred "$NEW_ID" >/dev/null 2>&1 || true

# -----------------------------------------------------------------------------
section "D-018: confidence show and explain agree"

SHOW=$(call_only mpm_confidence --payload '{"action":"show","params":{"artifact_id":"mpm-seed-read-wake-context"}}')
EXPLAIN=$(call_only mpm_confidence --payload '{"action":"explain","params":{"artifact_id":"mpm-seed-read-wake-context"}}')
RESULT=$(SHOW="$SHOW" EXPLAIN="$EXPLAIN" python3 -c "
import json, os
s = json.loads(os.environ['SHOW'])
e = json.loads(os.environ['EXPLAIN'])
sc = s.get('confidence')
ec = e.get('explanation', {}).get('confidence')
if sc is None or ec is None: print('MISS')
elif abs(sc - ec) < 1e-6: print(f'PASS ({sc:.6f} ~ {ec:.6f})')
else: print(f'DRIFT ({sc:.6f} vs {ec:.6f})')
" 2>/dev/null)
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
