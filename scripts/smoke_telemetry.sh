#!/usr/bin/env bash
# scripts/smoke_telemetry.sh — hermetic end-to-end test for mpm-telemetry.
#
# Boots the collector in a temp dir, sends 3 synthetic frames + duplicates,
# runs ping / query / observe (dry-run) / cost. Exits 0 on success.

set -euo pipefail

REPO_ROOT="$(cd "$(dirname "$0")/.." && pwd)"
BIN="${REPO_ROOT}/bin/mpm-telemetry"

if [[ ! -x "$BIN" ]]; then
  echo "bin/mpm-telemetry missing; run 'make build' first" >&2
  exit 1
fi

TMP=$(mktemp -d)
trap 'rm -rf "$TMP"' EXIT
export MPM_WORKSPACE="$TMP"
export MPM_TELEMETRY_SOCKET="$TMP/runtime/mpm-telemetry.sock"
export MPM_TELEMETRY_DB="$TMP/telemetry.db"

echo "==> smoke_telemetry: workspace=$TMP"

# Start the collector in the background.
"$BIN" serve --quiet &
SERVE_PID=$!
trap 'kill $SERVE_PID 2>/dev/null; rm -rf "$TMP"' EXIT

# Wait for socket.
for _ in $(seq 1 50); do
  [[ -S "$MPM_TELEMETRY_SOCKET" ]] && break
  sleep 0.05
done
if [[ ! -S "$MPM_TELEMETRY_SOCKET" ]]; then
  echo "socket never came up" >&2
  exit 1
fi

echo "==> smoke_telemetry: ping"
PING_OUT=$("$BIN" ping)
echo "$PING_OUT"
for want in '"collector_version"' '"protocol_version"' '"schema_version": "v1"' '"queue_depth"'; do
  if [[ "$PING_OUT" != *"$want"* ]]; then
    echo "ping missing $want" >&2
    exit 1
  fi
done

# Use a small Python helper to write NDJSON frames + read responses.
python3 - "$MPM_TELEMETRY_SOCKET" <<'PY'
import json, socket, sys

sock_path = sys.argv[1]

def send(frame):
    s = socket.socket(socket.AF_UNIX, socket.SOCK_STREAM)
    s.settimeout(2.0)
    s.connect(sock_path)
    s.sendall((json.dumps(frame) + "\n").encode())
    buf = b""
    while not buf.endswith(b"\n"):
        chunk = s.recv(4096)
        if not chunk: break
        buf += chunk
    s.close()
    return buf.decode().strip()

base = {
    "schema_version": "v1",
    "event_type": "invocation_completed",
    "framework": "claude-code",
    "framework_version": "1.0",
    "provider": "anthropic",
    "model": "claude-fable-5",
    "started_at": 1756000000,
    "completed_at": 1756000012,
    "status": "completed",
    "stop_reason": "end_turn",
    "input_tokens": 100,
    "output_tokens": 50,
    "cache_read_tokens": None,
    "cache_write_tokens": 200,
    "reasoning_tokens": 0,
    "duration_ms": 12000,
    "provider_metadata": {},
}

frames = [
    {**base, "invocation_id": "inv_smoke_1", "session_id": "sess_smoke"},
    {**base, "invocation_id": "inv_smoke_2", "session_id": "sess_smoke",
     "parent_invocation_id": "inv_smoke_1", "status": "failed", "stop_reason": "error"},
    {**base, "invocation_id": "inv_smoke_3", "session_id": None,
     "input_tokens": 0, "cache_read_tokens": None, "cache_write_tokens": None},
]

for f in frames:
    r = send(f)
    assert '"ACCEPTED"' in r, f"expected ACCEPTED, got {r}"
    print(f"frame {f['invocation_id']}: {r}")

# Idempotent retry — response has Inserted:false but omitempty drops it from JSON.
# Only '"ACCEPTED"' is reliable in the response; inserted=false is verified by
# protocol_test.go (Task 5). The smoke verifies end-to-end plumbing only.
r = send(frames[0])
assert '"ACCEPTED"' in r, f"expected ACCEPTED on retry, got {r}"
print(f"retry {frames[0]['invocation_id']}: {r}")

# Conflicting duplicate (different payload).
conflict = {**frames[0], "input_tokens": 999}
r = send(conflict)
assert '"REJECTED"' in r and 'invocation_id_payload_conflict' in r, f"expected REJECTED conflict, got {r}"
print(f"conflict: {r}")

# Schema version rejection.
bad = {**base, "invocation_id": "inv_smoke_bad", "schema_version": "v999"}
r = send(bad)
assert '"REJECTED"' in r and 'unknown_schema_version' in r, f"expected REJECTED schema, got {r}"
print(f"bad schema: {r}")

print("OK: frame flows accepted/rejected as expected")
PY

echo "==> smoke_telemetry: query invocation inv_smoke_1"
Q=$("$BIN" query invocation inv_smoke_1)
echo "$Q"
[[ "$Q" == *'"invocation_id": "inv_smoke_1"'* ]] || { echo "query failed" >&2; exit 1; }

echo "==> smoke_telemetry: cost"
COST=$("$BIN" cost --pricing "$REPO_ROOT/internal/telemetry/testdata/one-model.json" --since 0)
echo "$COST"
[[ "$COST" == *'"total"'* ]] || { echo "cost missing total" >&2; exit 1; }

# F2 fix: use --threshold 999999999 to avoid cross-DB shell-out.
# inv_smoke_1 + inv_smoke_2 have session_id=sess_smoke with combined
# input 200 + output 100 = 300 tokens. With --threshold 999999999,
# 300 < 999999999 → Hunt produces zero findings → dry-run exits 0 cleanly
# without shelling out to mpm for countFn.
echo "==> smoke_telemetry: observe (dry-run)"
OBS=$("$BIN" observe --dry-run --since 0 --threshold 999999999)
echo "$OBS"

# Belt-and-suspenders: verify row count via sqlite3 if available.
if command -v sqlite3 >/dev/null 2>&1; then
  ROWS=$(sqlite3 "$MPM_TELEMETRY_DB" "SELECT COUNT(*) FROM telemetry_invocation")
  # 3 originals, retry was deduped, conflict was rejected, bad-schema was rejected → 3 rows
  if [[ "$ROWS" != "3" ]]; then
    echo "expected 3 rows, got $ROWS" >&2
    exit 1
  fi
  echo "==> smoke_telemetry: row count check: $ROWS rows (expected 3)"
fi

echo "==> smoke_telemetry: cleanup"
kill $SERVE_PID 2>/dev/null || true
wait $SERVE_PID 2>/dev/null || true

echo "==> smoke_telemetry: PASS"
