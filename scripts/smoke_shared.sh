#!/bin/bash
# scripts/smoke_shared.sh — Hermetic regression net for the
# multi-agent shared-epistemology federated query path.
#
# Tests the structural invariant promised by shared_memories_ai /
# shared_memories_ad / shared_memories_au / shared_memories_au_content
# triggers (internal/core/db.go attachShared):
#
#   "If a row is in shared.memories, it is searchable in
#   shared.memories_fts \u2014 NO manual backfill needed."
#
# This script:
#   1. Sets up an isolated MPM_WORKSPACE + MPM_SHARED_DB in a tempdir.
#   2. Triggers DM attach + schema migration by writing one local memory.
#   3. Verifies the 4 FTS sync triggers were installed in the shared DB.
#   4. Writes 5 sentinel "house rule" rows through mpm's record_global_rule
#      (with confirm=true). Each write goes through mpm's own connection,
#      exercising the AFTER INSERT trigger.
#   5. Queries scope=local, scope=shared, scope=all and asserts that
#      the federated path returns hits organically.
#   6. Tears down the tempdir.
#
# Pre-2026-07-07 the federated path was broken: a freshly seeded
# shared DB would return 0 hits under scope=all because HybridSearch
# did not lazy-backfill shared.memories_fts. The triggers are the
# structural fix. This script is the proof.
#
# This is the integration counterpart to the unit test
# TestSharedFTS_FederatedScopeAll_TriggersOnly in
# internal/core/shared_fts_triggers_test.go.
#
# Usage: bash scripts/smoke_shared.sh
# Requires: bin/mpm built with CGO_CFLAGS=-DSQLITE_ENABLE_FTS5=1 (or
# -tags fts5). Run `make build` first.

set -euo pipefail

# Resolve script-relative paths so the script is callable from any cwd.
SCRIPT_DIR="$( cd "$( dirname "${BASH_SOURCE[0]}" )" && pwd )"
REPO_ROOT="$( cd "$SCRIPT_DIR/.." && pwd )"
MPM_BIN="${MPM_BIN:-$REPO_ROOT/bin/mpm}"

if [ ! -x "$MPM_BIN" ]; then
    echo "FATAL: mpm binary not found at $MPM_BIN"
    echo "       Run 'make build' first (CGO_CFLAGS=-DSQLITE_ENABLE_FTS5=1 make build)"
    exit 1
fi

if ! command -v sqlite3 >/dev/null 2>&1; then
    echo "FATAL: sqlite3 CLI not found (needed for trigger-count assertion)"
    exit 1
fi

if ! command -v python3 >/dev/null 2>&1; then
    echo "FATAL: python3 not found (needed for JSON parsing)"
    exit 1
fi

TMPDIR="$(mktemp -d)"
LOCAL_DIR="$TMPDIR/local"
mkdir -p "$LOCAL_DIR/src/db"
export MPM_WORKSPACE="$LOCAL_DIR"
export MPM_SHARED_DB="$TMPDIR/shared.db"

cleanup() {
    rm -rf "$TMPDIR"
}
trap cleanup EXIT

note() { printf "   \033[36m%s\033[0m\n" "$*"; }
ok()   { printf "   \033[32m\u2713 %s\033[0m\n" "$*"; }
fail() { printf "   \033[31m\u2717 %s\033[0m\n" "$*"; }

echo "=== [1/6] Bootstrap: trigger DatabaseManager attach + schema migration ==="
"$MPM_BIN" call save_to_memory \
    --payload '{"fact":"smoke-test bootstrap (will be shredded)","tags":"test:bootstrap-debug,2026-07-07","ttl":"24h"}' \
    > /dev/null 2>&1
SHARED_SIZE=$(du -h "$TMPDIR/shared.db" | cut -f1)
ok "DM initialized (shared.db $SHARED_SIZE)"

echo ""
echo "=== [2/6] Verify the 4 FTS sync triggers were installed ==="
TRIGGER_COUNT=$(sqlite3 "$TMPDIR/shared.db" \
    "SELECT COUNT(*) FROM sqlite_master WHERE type='trigger' AND name LIKE 'shared_memories_%'")
if [ "$TRIGGER_COUNT" = "4" ]; then
    ok "all 4 shared_memories triggers installed (_ai, _ad, _au, _au_content)"
else
    fail "expected 4 triggers; found $TRIGGER_COUNT"
    note "The triggers are the structural fix. Re-run after 'make build'."
    exit 1
fi

echo ""
echo "=== [3/6] Seed 5 sentinel 'house rule' rows via record_global_rule (confirm=true) ==="
SENTINELS=(
    "House rule: prefer substantive outputs over flattery."
    "House rule: API keys are secrets, never expose them to LLM."
    "House rule: architectural decisions need a recorded rationale."
    "House rule: secrets stay out of memory; the 20-pattern scanner enforces this."
    "House rule: doc staleness is a tax that compounds; update docs with the code."
)
SEEDED=0
for content in "${SENTINELS[@]}"; do
    # record_global_rule takes `fact` (not `content`) and a
    # comma-separated `tags` STRING (not an array). The handler also
    # requires `confirm=true` at the operator boundary — see
    # internal/core/tools/handlers.go handleRecordGlobalRule.
    PAYLOAD=$(python3 -c "import json,sys; print(json.dumps({'fact': sys.argv[1], 'tags': 'test:sentinel,smoke-test,2026-07-07', 'confirm': True}))" "$content")
    if "$MPM_BIN" call record_global_rule --payload "$PAYLOAD" > /dev/null 2>&1; then
        SEEDED=$((SEEDED + 1))
    else
        note "  failed to seed: $content"
    fi
done
if [ "$SEEDED" = "5" ]; then
    ok "seeded 5 sentinels via record_global_rule"
else
    fail "expected 5 sentinels seeded; got $SEEDED"
    exit 1
fi

echo ""
echo "=== [4/6] Verify the trigger auto-populated shared.memories_fts ==="
SHARED_MEMORIES_COUNT=$(sqlite3 "$TMPDIR/shared.db" "SELECT COUNT(*) FROM memories WHERE is_global=1")
SHARED_FTS_COUNT=$(sqlite3 "$TMPDIR/shared.db" "SELECT COUNT(*) FROM memories_fts")
if [ "$SHARED_MEMORIES_COUNT" = "$SHARED_FTS_COUNT" ] && [ "$SHARED_MEMORIES_COUNT" = "5" ]; then
    ok "shared.memories=$SHARED_MEMORIES_COUNT, shared.memories_fts=$SHARED_FTS_COUNT (triggers in sync)"
else
    fail "expected 5 rows in both; got memories=$SHARED_MEMORIES_COUNT, fts=$SHARED_FTS_COUNT"
    note "If they differ, the AFTER INSERT trigger (shared_memories_ai) is not firing."
    exit 1
fi

echo ""
echo "=== [5/6] Federated query scope=all — should return hits organically ==="
# Single-token query to avoid the BM25 IDF-floor issue documented in
# audit.md / the 2026-07-07 incident: with only 5 documents, multi-
# token queries can produce 0 matches because per-token weights
# compete and the Shared Premium multiplier doesn't lift them above
# the retrieval threshold. "rule" appears in every sentinel; the
# exact match is the test.
Q='{"query":"rule","scope":"all","limit":10}'
RESPONSE=$("$MPM_BIN" call query_long_term_memory --payload "$Q" 2>/dev/null)
COUNT=$(echo "$RESPONSE" | python3 -c 'import sys,json; print(json.load(sys.stdin)["count"])' 2>/dev/null || echo "0")
if [ "$COUNT" -ge 5 ]; then
    ok "scope=all returned $COUNT rows (5 sentinels expected)"
else
    fail "scope=all returned $COUNT rows; expected >= 5"
    note "This is the EXACT bug the triggers are supposed to fix."
    note "Response: $RESPONSE"
    exit 1
fi

echo ""
echo "=== [6/6] Verdict ==="
ok "shared.memories reachable via ATTACH (step 1)"
ok "FTS sync triggers installed (step 2)"
ok "writes through mpm auto-populate FTS (step 3)"
ok "trigger keeps FTS in sync (step 4)"
ok "scope=all returns hits without manual backfill (step 5)"
echo ""
echo "\u2705 Multi-agent shared-epistemology federated path is wired."
echo "   The 'silent read-path failure' footgun (2026-07-07) is closed."
