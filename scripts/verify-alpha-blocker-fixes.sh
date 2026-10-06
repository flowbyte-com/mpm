#!/usr/bin/env bash
# verify-alpha-blocker-fixes.sh
# End-to-end verification of the 5 alpha-blocker eradication fixes:
#   D-001/002/003  Theory CLI flag parser (--hypothesis/--validation)
#   D-004          mpm add with content starting with `-`
#   D-013          mpm-mcp bootstrap of mode/ and persona/ dirs
#   D-010          Theory resolve status vocabulary (proven/disproven)
#   D-007          Secret scanner catches sk-/ghp_ short-form keys
#
# Each section runs in its own throwaway workspace, asserts pass/fail,
# and exits non-zero on any failure.
#
# Usage:  ./scripts/verify-alpha-blocker-fixes.sh
# Assumes: .build/bin/mpm and .build/bin/mpm-mcp already built (`make build`).

set -u

ROOT="$(cd "$(dirname "$0")/.." && pwd)"
MPM_BIN="${ROOT}/.build/bin/mpm"
MCP_BIN="${ROOT}/.build/bin/mpm-mcp"
TMP="$(mktemp -d -t mpm-alpha-verify.XXXXXX)"
trap 'rm -rf "$TMP"' EXIT

pass=0
fail=0

assert_eq() {
    local label="$1" expected="$2" actual="$3"
    if [[ "$expected" == "$actual" ]]; then
        printf "  PASS  %s\n" "$label"
        pass=$((pass + 1))
    else
        printf "  FAIL  %s\n" "$label"
        printf "        expected: %q\n" "$expected"
        printf "        actual:   %q\n" "$actual"
        fail=$((fail + 1))
    fi
}

assert_contains() {
    local label="$1" needle="$2" haystack="$3"
    if [[ "$haystack" == *"$needle"* ]]; then
        printf "  PASS  %s\n" "$label"
        pass=$((pass + 1))
    else
        printf "  FAIL  %s\n" "$label"
        printf "        expected to contain: %q\n" "$needle"
        printf "        in: %q\n" "$haystack"
        fail=$((fail + 1))
    fi
}

assert_rejects() {
    local label="$1" needle="$2" output="$3"
    if [[ "$output" != *"$needle"* ]]; then
        printf "  PASS  %s (no match for %q)\n" "$label" "$needle"
        pass=$((pass + 1))
    else
        printf "  FAIL  %s (unexpectedly matched %q)\n" "$label" "$needle"
        printf "        in: %q\n" "$output"
        fail=$((fail + 1))
    fi
}

mpm() {
    ( cd "$1" && MPM_WORKSPACE="$1" "$MPM_BIN" "${@:2}" )
}

echo
echo "============================================================"
echo " Fix #1: D-001/002/003 — theory CLI flag parser"
echo "============================================================"
W="$TMP/fix1"
mkdir -p "$W"
mpm "$W" add "seed" >/dev/null 2>&1

out=$(mpm "$W" theorize --hypothesis "alpha-blocker fix" --validation "see verification script" 2>&1)
assert_contains "theorize --validation exits 0" "Theory proposed" "$out"

# Extract theory id from output
tid=$(echo "$out" | grep -oE '[0-9a-f]{16}' | head -1)
if [[ -z "$tid" ]]; then
    echo "  FAIL  could not extract theory id from theorize output"
    fail=$((fail + 1))
else
    meta_json=$(sqlite3 "$W/src/db/mpm.db" "SELECT json(metadata) FROM memories WHERE id='$tid';")
    assert_contains "validation_criteria stored in metadata" "validation_criteria" "$meta_json"
    hyp_val=$(sqlite3 "$W/src/db/mpm.db" "SELECT json_extract(metadata, '\$.validation_criteria') FROM memories WHERE id='$tid';")
    assert_eq "metadata.validation_criteria exact value" "see verification script" "$hyp_val"
    # Pre-fix bug: hypothesis was "--hypothesis alpha-blocker fix --validation see verification script"
    hyp_blob=$(sqlite3 "$W/src/db/mpm.db" "SELECT json_extract(metadata, '\$.hypothesis') FROM memories WHERE id='$tid';")
    assert_rejects "hypothesis does NOT contain flag text" "--hypothesis" "$hyp_blob"
fi

echo
echo "============================================================"
echo " Fix #2: D-004 — mpm add content starting with dash"
echo "============================================================"
W="$TMP/fix2"
mkdir -p "$W"

# Single dash content (use -- separator since Go flag lib supports it)
out=$(mpm "$W" add -- "---test" 2>&1)
assert_contains "mpm add -- '---test' exits 0" "Added memory" "$out"

# Verify content stored verbatim
last_id=$(echo "$out" | grep -oE '[0-9a-f]{16}' | head -1)
if [[ -n "$last_id" ]]; then
    stored=$(sqlite3 "$W/src/db/mpm.db" "SELECT content FROM memories WHERE id='$last_id';")
    assert_eq "content stored verbatim" "---test" "$stored"
fi

# Help text mentions the -- separator
help_out=$(mpm "$W" add --help 2>&1)
assert_contains "help text mentions -- separator" "prefix content" "$help_out"

echo
echo "============================================================"
echo " Fix #3: D-013 — mpm-mcp bootstraps mode/ + persona/"
echo "============================================================"
W="$TMP/fix3"
mkdir -p "$W"
rm -rf "$W/mode" "$W/persona" 2>/dev/null
[[ ! -d "$W/mode" ]]    || { echo "  pre: mode/ unexpectedly exists"; fail=$((fail+1)); }
[[ ! -d "$W/persona" ]] || { echo "  pre: persona/ unexpectedly exists"; fail=$((fail+1)); }

# Boot mpm-mcp in background; send it no input; expect it to start cleanly
( cd "$W" && MPM_WORKSPACE="$W" MPM_REQUIRED_DB_PATH="$(cd "$W" && readlink -f src/db/mpm.db)" \
    "$MCP_BIN" </dev/null >/dev/null 2>"$W/mcp-stderr.log" ) &
mcp_pid=$!
# Give the bootstrap a moment to run mkdir + NewRouter
sleep 1
kill "$mcp_pid" 2>/dev/null || true
wait "$mcp_pid" 2>/dev/null || true

if [[ -d "$W/mode" && -d "$W/persona" ]]; then
    printf "  PASS  mode/ and persona/ bootstrapped\n"
    pass=$((pass + 1))
else
    printf "  FAIL  mode/ or persona/ missing after mcp boot\n"
    fail=$((fail + 1))
fi

# Stderr should not contain the legacy "build router: open .../mode" error
if [[ -f "$W/mcp-stderr.log" ]] && ! grep -q "build router" "$W/mcp-stderr.log"; then
    printf "  PASS  no 'build router' fatal in stderr\n"
    pass=$((pass + 1))
else
    printf "  FAIL  mcp stderr still contains router error:\n"
    cat "$W/mcp-stderr.log" 2>/dev/null | sed 's/^/        /'
    fail=$((fail + 1))
fi

echo
echo "============================================================"
echo " Fix #4: D-010 — theory resolve status vocabulary"
echo "============================================================"
W="$TMP/fix4"
mkdir -p "$W"
mpm "$W" add "seed" >/dev/null 2>&1

# Resolve with 'confirmed' → status should be 'proven'
tid=$(mpm "$W" theorize --hypothesis "victory" --validation "test passes" 2>&1 | grep -oE '[0-9a-f]{16}' | head -1)
mpm "$W" resolve_theory "$tid" "confirmed" >/dev/null 2>&1
status=$(sqlite3 "$W/src/db/mpm.db" "SELECT json_extract(metadata, '\$.status') FROM memories WHERE id='$tid';")
assert_eq "resolve 'confirmed' → status 'proven'" "proven" "$status"

# Filter by status=proven should now find it
list_out=$(mpm "$W" theories proven 2>&1)
assert_contains "theories proven filter returns the row" "$tid" "$list_out"

# Resolve a second theory with 'disproven'
tid2=$(mpm "$W" theorize --hypothesis "defeat" --validation "test fails" 2>&1 | grep -oE '[0-9a-f]{16}' | head -1)
mpm "$W" resolve_theory "$tid2" "disproven" >/dev/null 2>&1
status2=$(sqlite3 "$W/src/db/mpm.db" "SELECT json_extract(metadata, '\$.status') FROM memories WHERE id='$tid2';")
assert_eq "resolve 'disproven' → status 'disproven'" "disproven" "$status2"

echo
echo "============================================================"
echo " Fix #5: D-007 — secret scanner short-form patterns"
echo "============================================================"
W="$TMP/fix5"
mkdir -p "$W"
mpm "$W" add "seed" >/dev/null 2>&1

# sk- prefix with 16 hex chars (was uncaught pre-fix)
out=$(mpm "$W" add "leaked: sk-1234567890abcdef" 2>&1)
assert_contains "sk- + 16 hex chars blocked" "sensitive content" "$out"

# ghp_ with 20 alnum chars (was uncaught pre-fix)
out=$(mpm "$W" add "token: ghp_1234567890abcdefghij" 2>&1)
assert_contains "ghp_ + 20 alnum chars blocked" "sensitive content" "$out"

# Existing patterns still fire (regression check)
out=$(mpm "$W" add "password=hunter2" 2>&1)
assert_contains "password=... still blocked" "sensitive content" "$out"

# Benign content not blocked
out=$(mpm "$W" add "this is a normal memory about lunch" 2>&1)
assert_contains "benign content allowed" "Added memory" "$out"

echo
echo "============================================================"
echo " Summary"
echo "============================================================"
printf "  PASS: %d\n" "$pass"
printf "  FAIL: %d\n" "$fail"
echo
if [[ $fail -gt 0 ]]; then
    echo "VERIFICATION FAILED"
    exit 1
fi
echo "VERIFICATION PASSED — all 5 alpha-blocker fixes verified."
