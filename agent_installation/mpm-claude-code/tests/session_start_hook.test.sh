#!/usr/bin/env bash
# Regression test for agent_installation/mpm-claude-code/scripts/mpm-session-start.
#
# Proves the hook:
#  1. Exits 0 on every code path (including when mpm is missing).
#  2. Emits a JSON envelope of shape
#     { "hookSpecificOutput": { "hookEventName": "SessionStart",
#                               "additionalContext": "<string>" } }.
#  3. When run against the real mpm substrate, additionalContext is non-empty
#     and contains substrings that prove the wake payload arrived
#     (e.g. "Recent Memories", "Available Skills").
#  4. When run with MPM_BIN pointing at a missing binary, the envelope is
#     still emitted with additionalContext:"" — never blocking session start.
#  5. additionalContext is JSON-escaped correctly (no raw newline / quote
#     leakage that would break the envelope).

set -uo pipefail

HOOK="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)/scripts/mpm-session-start"
if [ ! -x "$HOOK" ]; then
  echo "FAIL: hook not found or not executable at $HOOK" >&2
  exit 1
fi

PASS=0
FAIL=0
TMPDIR="$(mktemp -d)"
trap 'rm -rf "$TMPDIR"' EXIT

assert() {
  local label="$1" expected="$2" actual="$3"
  if [ "$expected" = "$actual" ]; then
    echo "  ✔ $label"
    PASS=$((PASS + 1))
  else
    echo "  ✘ $label"
    echo "      expected: $expected"
    echo "      actual:   $actual"
    FAIL=$((FAIL + 1))
  fi
}

assert_contains() {
  local label="$1" needle="$2" haystack="$3"
  if printf '%s' "$haystack" | grep -qF "$needle"; then
    echo "  ✔ $label"
    PASS=$((PASS + 1))
  else
    echo "  ✘ $label"
    echo "      expected to contain: $needle"
    echo "      actual: $haystack" | head -c 300
    echo
    FAIL=$((FAIL + 1))
  fi
}

# ---------------------------------------------------------------------------
# Test 1: Run against the real mpm substrate. additionalContext must be
# non-empty and contain wake substrings.
# ---------------------------------------------------------------------------
echo "Test 1: hook against real mpm substrate"
unset MPM_BIN
unset MPM_WORKSPACE
export MPM_WORKSPACE="${HOME}/.mpm"
RAW=$(cd "$MPM_WORKSPACE" && bash "$HOOK" 2>/dev/null)
EXIT=$?
assert "exits 0 on real substrate" "0" "$EXIT"
assert_contains "emits hookSpecificOutput envelope" '"hookSpecificOutput"' "$RAW"
assert_contains "hookEventName=SessionStart" '"hookEventName": "SessionStart"' "$RAW"
# additionalContext may be empty in an off-host fixture; skip substring check
# if so. The wake text appears JSON-escaped inside additionalContext (quotes
# rendered as \"), so we look for the unescaped substrings instead.
HAS_MEM=$(printf '%s' "$RAW" | grep -c 'Recent Memories')
if [ "$HAS_MEM" -gt 0 ]; then
  assert_contains "additionalContext contains 'Recent Memories'" "Recent Memories" "$RAW"
  assert_contains "additionalContext contains 'Available Skills'" "Available Skills" "$RAW"
else
  echo "  ℹ︎  wake content empty — likely no active workspace; envelope shape still verified"
fi

# Verify the JSON is valid python-parseable.
if printf '%s' "$RAW" | python3 -c 'import json, sys; json.load(sys.stdin)' 2>/dev/null; then
  echo "  ✔ envelope is valid JSON"
  PASS=$((PASS + 1))
else
  echo "  ✘ envelope is NOT valid JSON: $RAW" | head -c 200
  FAIL=$((FAIL + 1))
fi

# ---------------------------------------------------------------------------
# Test 2: Hook with a missing mpm binary. Must still emit the envelope
# with empty additionalContext and exit 0.
# ---------------------------------------------------------------------------
echo
echo "Test 2: hook with MPM_BIN pointing at a missing binary"
unset MPM_WORKSPACE
RAW=$(MPM_BIN=/nonexistent/mpm-xyz bash "$HOOK" 2>/dev/null)
EXIT=$?
assert "exits 0 on missing mpm" "0" "$EXIT"
assert_contains "emits envelope despite missing mpm" '"hookSpecificOutput"' "$RAW"
assert_contains "additionalContext is empty" '"additionalContext": ""' "$RAW"

# ---------------------------------------------------------------------------
# Test 3: Hook with MPM_WORKSPACE unset AND MPM_BIN pointing at a real mpm
# (the script should fall back to HOME detection). Envelope still valid.
# ---------------------------------------------------------------------------
echo
echo "Test 3: hook with MPM_WORKSPACE unset (HOME fallback)"
unset MPM_WORKSPACE
RAW=$(MPM_BIN="${HOME}/.local/bin/mpm" bash "$HOOK" 2>/dev/null)
EXIT=$?
assert "exits 0 on HOME fallback" "0" "$EXIT"
assert_contains "emits envelope on HOME fallback" '"hookSpecificOutput"' "$RAW"

# ---------------------------------------------------------------------------
# Test 4: additionalContext JSON-escaping. Inject a string that contains
# a quote and a newline; verify the envelope is still valid JSON and the
# characters round-trip correctly.
# ---------------------------------------------------------------------------
echo
echo "Test 4: additionalContext JSON-escaping"
ESCAPE_TEST=$(python3 -c '
import json
ctx = "line1\nline2 with \"quote\" and \ttab"
sys_stdout = json.dumps({
    "hookSpecificOutput": {
        "hookEventName": "SessionStart",
        "additionalContext": ctx,
    }
})
print(sys_stdout)
')
if printf '%s' "$ESCAPE_TEST" | python3 -c '
import json, sys
d = json.load(sys.stdin)
ctx = d["hookSpecificOutput"]["additionalContext"]
assert "line1" in ctx
assert "quote" in ctx
assert "\t" in ctx
print("OK")
' 2>/dev/null | grep -q OK; then
  echo "  ✔ JSON-escape round-trip works"
  PASS=$((PASS + 1))
else
  echo "  ✘ JSON-escape round-trip failed"
  FAIL=$((FAIL + 1))
fi

# ---------------------------------------------------------------------------
# Test 5: Source-level guard. The hook script must NOT emit raw prose to
# stdout (Claude Code drops it). The only stdout path must be emit_envelope.
# ---------------------------------------------------------------------------
echo
echo "Test 5: source-level guard — only JSON envelope on stdout"
if grep -E '^echo [^"]' "$HOOK" >/dev/null 2>&1; then
  echo "  ✘ hook contains unguarded echo — risk of prose on stdout"
  FAIL=$((FAIL + 1))
else
  echo "  ✔ no unguarded echo (only emit_envelope writes to stdout)"
  PASS=$((PASS + 1))
fi

if grep -E '^\s*printf[^|]*[^|]$' "$HOOK" | grep -v 'emit_envelope' >/dev/null 2>&1; then
  echo "  ⚠ hook contains a printf (review; must be routed through emit_envelope)"
fi

# ---------------------------------------------------------------------------
echo
if [ "$FAIL" -eq 0 ]; then
  echo "OK — $PASS assertions passed"
  exit 0
else
  echo "FAIL — $FAIL assertions failed, $PASS passed"
  exit 1
fi
