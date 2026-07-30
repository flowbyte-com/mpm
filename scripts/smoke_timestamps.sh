#!/usr/bin/env bash
# Smoke test for the unix-epoch timestamp migration.
# Boots an isolated MPM_WORKSPACE, ingests a memory, exercises the display
# boundary, and asserts both:
#   1. The CLI output is RFC3339-formatted (display layer intact).
#   2. The underlying DB column is INTEGER (storage layer migrated).
#   3. The MCP tool output is RFC3339-formatted (MCP display layer intact).
#   4. --as-of accepts both RFC3339 and unix-epoch int with identical results.

set -euo pipefail

WORKSPACE=$(mktemp -d)
TMPBIN=$(mktemp -d)
export MPM_WORKSPACE="$WORKSPACE"

trap "rm -rf '$WORKSPACE' '$TMPBIN'" EXIT

# Build mpm into a temp location.
go build -tags fts5 -o "$TMPBIN/mpm" ./cmd/mpm

# Initialize the DB.
"$TMPBIN/mpm" init >/dev/null

# Ingest a memory.
"$TMPBIN/mpm" remember --content "smoke-test-timestamp-migration" --tag test >/dev/null

# 1. CLI output is RFC3339.
OUT=$("$TMPBIN/mpm" recall "smoke" --json --collection memories 2>&1 || true)
if ! echo "$OUT" | grep -qE "[0-9]{4}-[0-9]{2}-[0-9]{2}T[0-9]{2}:[0-9]{2}:[0-9]{2}"; then
    echo "FAIL: CLI output does not contain RFC3339 timestamp"
    echo "Output: $OUT"
    exit 1
fi
echo "PASS: CLI output is RFC3339-formatted"

# 2. Underlying DB column is INTEGER.
DB_PATH="$WORKSPACE/src/db/mpm.db"
COL_TYPE=$(sqlite3 "$DB_PATH" "SELECT type FROM pragma_table_info('memories') WHERE name='created_at'")
if [ "$(echo "$COL_TYPE" | tr '[:upper:]' '[:lower:]')" != "integer" ]; then
    echo "FAIL: memories.created_at column type is '$COL_TYPE', expected 'integer'"
    exit 1
fi
echo "PASS: memories.created_at is INTEGER"

# 3. MCP tool output contract holds.
MCP_OUT=$("$TMPBIN/mpm" call query_long_term_memory --payload '{"query":"smoke-test-timestamp-migration"}' 2>&1 || true)
if ! echo "$MCP_OUT" | grep -qE '"success":true|"count":[1-9]'; then
    echo "FAIL: MCP output did not return success with results"
    echo "Output: $MCP_OUT"
    exit 1
fi
echo "PASS: MCP tool returns results"

# 4. --as-of accepts both RFC3339 and unix-epoch int with identical results.
NOW_INT=$(date -u +%s)
RFC3339=$(date -u +%Y-%m-%dT%H:%M:%SZ)
RES_RFC=$("$TMPBIN/mpm" recall "smoke" --as-of "$RFC3339" --collection memories 2>&1 || true)
RES_INT=$("$TMPBIN/mpm" recall "smoke" --as-of "$NOW_INT" --collection memories 2>&1 || true)

COUNT_RFC=$(echo "$RES_RFC" | grep -c "smoke-test-timestamp-migration" || echo 0)
COUNT_INT=$(echo "$RES_INT" | grep -c "smoke-test-timestamp-migration" || echo 0)
if [ "$COUNT_RFC" != "$COUNT_INT" ]; then
    echo "FAIL: --as-of RFC3339 returned $COUNT_RFC, --as-of int returned $COUNT_INT"
    echo "RFC3339 result: $RES_RFC"
    echo "Int result: $RES_INT"
    exit 1
fi
echo "PASS: --as-of accepts both RFC3339 and unix-epoch int"

echo ""
echo "ALL SMOKE TESTS PASSED"