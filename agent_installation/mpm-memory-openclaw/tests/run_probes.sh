#!/usr/bin/env bash
# tests/run_probes.sh — §23 non-vacuity probes.
#
# Each probe deliberately breaks one property of the implementation and
# asserts that the corresponding guard goes RED. A guard that cannot be
# made to fail is not a guard. Every mutation is reverted afterwards and
# the tree is required to return to its original state.
#
# Usage: bash tests/run_probes.sh
set -uo pipefail

PKG="$(cd "$(dirname "$0")/.." && pwd -P)"
cd "$PKG"
MPM=/home/v/.mpm/bin/mpm
MCP=/home/v/.mpm/bin/mpm-mcp
export MPM_TEST_MPM_BIN=$MPM MPM_TEST_MCP_BIN=$MCP

CLIENT=lib/mcp-client.js
TRANSPORT=lib/memory-transport.js
INDEX=index.js

PASS=0; FAIL=0
snapshot="$(mktemp)"
md5sum "$CLIENT" "$TRANSPORT" "$INDEX" > "$snapshot"

# The pristine copies must survive EVERY per-probe restore. An earlier
# version of this script deleted them inside restore(), which made the
# first restore work and every later one a silent no-op — probes then
# compounded their mutations onto one tree and each "guard went red" was
# really several unrelated breaks at once. Keep the copies until the
# final EXIT check, and verify restoration by checksum rather than by
# trusting that the copy happened.
cp "$CLIENT" "$CLIENT.pristine"; cp "$TRANSPORT" "$TRANSPORT.pristine"; cp "$INDEX" "$INDEX.pristine"

restore() {
  local bad=0 f
  for f in "$CLIENT" "$TRANSPORT" "$INDEX"; do
    cp "$f.pristine" "$f"
  done
  for f in "$CLIENT" "$TRANSPORT" "$INDEX"; do
    cmp -s "$f" "$f.pristine" || { echo "  !! restore failed for $f"; bad=1; }
  done
  [ "$bad" -eq 0 ] || exit 1
}

cleanup() {
  restore
  if md5sum -c --status "$snapshot" 2>/dev/null; then
    echo "tree restored cleanly"
  else
    echo "TREE NOT RESTORED"; md5sum -c "$snapshot" || true
  fi
  rm -f "$CLIENT.pristine" "$TRANSPORT.pristine" "$INDEX.pristine" "$snapshot"
}
trap cleanup EXIT

# probe <name> <expected-substring-in-output> <test-file> <filter>
probe() {
  local name="$1" expect="$2" file="$3" filter="${4:-}"
  local out rc
  if [ -n "$filter" ]; then
    out=$(timeout 300 node --test --test-name-pattern "$filter" "$file" 2>&1); rc=$?
  else
    out=$(timeout 300 node --test "$file" 2>&1); rc=$?
  fi
  if echo "$out" | grep -qE "$expect" && [ "$rc" -ne 0 ]; then
    echo "  PASS  $name  (guard failed as expected, exit=$rc)"
    PASS=$((PASS+1))
  else
    echo "  FAIL  $name  (guard did NOT fail; exit=$rc)"
    echo "$out" | grep -E "ℹ (pass|fail)" | sed 's/^/          /'
    FAIL=$((FAIL+1))
  fi
  restore
}

echo "Probe A — make the fast path also spawn a subprocess per read"
# The structural claim is "repeated reads no longer launch one mpm process
# each". The sharpest test of that counter is a regression that still
# returns CORRECT results. Routing reads away from MCP entirely just makes
# the benchmark abort on its first unusable response, which proves nothing
# about the counter. So: keep serving over MCP, and add a per-call
# subprocess alongside it. Results stay correct and timings barely move;
# only the process counter can catch it.
python3 tests/apply_probe_a.py
if grep -q "PROBE A" "$TRANSPORT"; then
  out=$(MPM_BENCH_WS=/tmp/mpm-fastpath-probe/ws MPM_BENCH_N=12 MPM_BENCH_RUNS=1 timeout 300 node tests/bench_transport.mjs 2>&1); rc=$?
  if echo "$out" | grep -q "FAIL: the fast path still launched mpm subprocesses"; then
    echo "  PASS  A  structural guard detected the per-call subprocess (exit=$rc)"; PASS=$((PASS+1))
  else
    echo "  FAIL  A  structural guard did not detect the regression"
    echo "$out" | tail -8 | sed 's/^/          /'
    FAIL=$((FAIL+1))
  fi
else
  echo "  FAIL  A  probe mutation did not apply"
  FAIL=$((FAIL+1))
fi
restore

echo "Probe B — let concurrent first calls start two children"
# Drop the single-flight: each concurrent ensureReady() spawns its own child.
python3 - <<'PY'
import re
p="lib/mcp-client.js"; s=open(p).read()
s=s.replace("    if (this.#startPromise) return this.#startPromise;","    // PROBE B: single-flight disabled")
open(p,"w").write(s)
PY
probe "B" "concurrent first calls spawn exactly ONE child" tests/mcp_client.test.js "concurrent first calls"
# strengthen: count real children so the probe is not a tautology
restore

echo "Probe C — disable response-id correlation"
python3 - <<'PY'
p="lib/mcp-client.js"; s=open(p).read()
s=s.replace("    const entry = this.#pending.get(message.id);",
            "    const entry = this.#pending.values().next().value; // PROBE C: ignore id")
open(p,"w").write(s)
PY
probe "C" "correlate responses to the right caller" tests/mcp_client.test.js "correlate responses"
restore

echo "Probe D — skip child cleanup on shutdown"
python3 - <<'PY'
p="lib/mcp-client.js"; s=open(p).read()
s=s.replace("    const child = this.#child;\n    if (!child) {\n      this.#state = CLIENT_STATE.STOPPED;\n      return;\n    }",
            "    const child = this.#child;\n    if (child) return; // PROBE D: never reap\n    if (!child) {\n      this.#state = CLIENT_STATE.STOPPED;\n      return;\n    }")
open(p,"w").write(s)
PY
probe "D" "close\(\) reaps the child" tests/mcp_client.test.js "reaps the child"
restore

echo "Probe E — serve stale results after an out-of-band write"
python3 - <<'PY'
p="lib/memory-transport.js"; s=open(p).read()
s=s.replace("      if (result !== null) {\n        stats.mcpCalls += 1;\n        return result;\n      }",
            "      if (result !== null) {\n        stats.mcpCalls += 1;\n        if (!this._sticky) { this._sticky = result; }\n        return this._sticky; // PROBE E: never re-read\n      }")
open(p,"w").write(s)
PY
probe "E" "out-of-band write is visible" tests/memory_transport.test.js "out-of-band"
restore

echo "Probe F — drop one memory tool from registration"
python3 - <<'PY'
p="index.js"; s=open(p).read()
s=s.replace('      { names: ["mpm_memory_get"] }','      { names: ["mpm_memory_get_disabled_probe"] }')
open(p,"w").write(s)
PY
probe "F" "mpm_memory_get missing from resolved plugin tools" tests/resolver_reachability.test.js ""
restore

echo "Probe G — resolve mpm-mcp through PATH instead of the configured prefix"
python3 - <<'PY'
p="lib/mcp-client.js"; s=open(p).read()
s=s.replace('  const dir = path.dirname(bin);\n  if (dir === "" || dir === ".") {\n    // A bare name gives nothing to derive from, and searching PATH for a\n    // sibling would defeat the point of a deterministic mapping.\n    return null;\n  }',
            '  const dir = path.dirname(bin);\n  if (dir === "" || dir === ".") {\n    return "/home/v/.mpm/bin/mpm-mcp"; // PROBE G: arbitrary PATH selection\n  }')
open(p,"w").write(s)
PY
probe "G" "returns null for a bare binary name" tests/mcp_client.test.js "bare binary name"
restore

restore
echo
echo "probes: ${PASS} passed, ${FAIL} failed"
[ "$FAIL" -eq 0 ]