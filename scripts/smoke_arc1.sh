#!/bin/bash
# smoke_arc1.sh - End-to-end verification of Arc 1 (Conflict Resolution).
#
# Seeds two memories in the shared DB with very different confidence,
# queues a contradiction between them, and runs `mpm ops
# resolve-contradictions --apply` to verify the full cycle:
#   1. Detection → contradiction_log row appears
#   2. Resolution → loser is slashed, resolution memory is created
#   3. Queue → row is marked resolved with FK to resolution memory
#
# Then seeds a close-call contradiction and verifies the arbitration
# path: a theory is proposed in shared.memories (collection='theories')
# and the queue row stays unresolved.
#
# Exits non-zero on any failure.

set -e

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
REPO_ROOT="$(cd "$SCRIPT_DIR/.." && pwd)"
TMPDIR="$(mktemp -d -t mpm-smoke-arc1.XXXXXX)"
trap "rm -rf '$TMPDIR'" EXIT

# Source Go from the standard nvm path if not already on PATH.
if ! command -v go >/dev/null 2>&1; then
    for candidate in /usr/local/go/bin /home/v/.nvm/versions/node/v24.14.1/bin; do
        if [ -x "$candidate/go" ]; then
            export PATH="$candidate:$PATH"
            break
        fi
    done
fi

export MPM_WORKSPACE="$TMPDIR/mpm"
export MPM_SHARED_DB="$TMPDIR/mpm/shared.db"
export CGO_CFLAGS="-DSQLITE_ENABLE_FTS5=1"

mkdir -p "$MPM_WORKSPACE"
touch "$MPM_SHARED_DB"

# ── 1. Build with FTS5 support (needed for the shared mem_fts triggers) ─
echo "=== [1/8] Build mpm binary with FTS5 support ==="
cd "$REPO_ROOT"
go build -tags sqlite_fts5 -o "$TMPDIR/mpm-bin" ./cmd/mpm 2>&1 | grep -v "no such module: fts5" || true
test -x "$TMPDIR/mpm-bin" || { echo "❌ binary build failed"; exit 1; }
echo "   ✓ binary built at $TMPDIR/mpm-bin"

# ── 2. Bootstrap the DM (installs schema, attaches shared DB) ──────────
echo ""
echo "=== [2/8] Bootstrap the DM ==="
"$TMPDIR/mpm-bin" status >/dev/null 2>&1 || true
test -f "$MPM_WORKSPACE/src/db/mpm.db" || { echo "❌ DM not initialized"; exit 1; }
echo "   ✓ DM initialized, shared DB attached"

# ── 3. Verify shared.contradiction_log table exists with both indexes ─
echo ""
echo "=== [3/8] Verify shared.contradiction_log schema ==="
TABLES=$(sqlite3 "$MPM_SHARED_DB" "SELECT COUNT(*) FROM sqlite_master WHERE type='table' AND name='contradiction_log'")
test "$TABLES" = "1" || { echo "❌ contradiction_log table not found"; exit 1; }
INDEXES=$(sqlite3 "$MPM_SHARED_DB" "SELECT COUNT(*) FROM sqlite_master WHERE type='index' AND tbl_name='contradiction_log'")
test "$INDEXES" -ge 3 || { echo "❌ expected ≥3 indexes on contradiction_log, got $INDEXES"; exit 1; }
echo "   ✓ contradiction_log + indexes (PK + detected_at + unresolved partial) installed"

# ── 4. Seed decisive contradiction: high-confidence vs low-confidence ─
echo ""
echo "=== [4/8] Seed decisive contradiction (mem-strong vs mem-weak) ==="
sqlite3 "$MPM_SHARED_DB" <<EOF
INSERT INTO memories (id, collection, content, retrieval_priority, importance, confidence, weight, reinforcement_count, last_accessed_at, is_global, tags)
VALUES
  ('mem-strong', 'memories', 'strong memory', 0.9, 0.9, 0.95, 5, 10, CURRENT_TIMESTAMP, 1, '[]'),
  ('mem-weak',   'memories', 'weak memory',   0.2, 0.2, 0.10, 1, 0,  NULL,                 0, '[]');

INSERT INTO evidence (id, artifact_id, artifact_type, type, source_group, notes, created_by, strength, created_at)
VALUES ('ev-1', 'mem-strong', 'memory', 'observation', 'test', 'cited by 10 sessions', 'tester', 0.5, CURRENT_TIMESTAMP);

INSERT INTO contradiction_log (memory_id_a, memory_id_b, evidence, similarity, detected_by)
VALUES ('mem-strong', 'mem-weak', '{"memory_a":"mem-strong","memory_b":"mem-weak","evidence":"test decisive","similarity":0.92}', 0.92, 'session-1');
EOF
n_queued=$(sqlite3 "$MPM_SHARED_DB" "SELECT COUNT(*) FROM contradiction_log WHERE resolved_at IS NULL")
test "$n_queued" = "1" || { echo "❌ expected 1 queued, got $n_queued"; exit 1; }
echo "   ✓ seeded 1 contradiction (decisive case)"

# ── 5. Run resolve-contradictions --apply (decisive path) ─────────────
echo ""
echo "=== [5/8] Run mpm ops resolve-contradictions --apply ==="
out=$("$TMPDIR/mpm-bin" ops resolve-contradictions --apply 2>&1)
echo "$out" | grep -q "decisive: mem-strong" || { echo "❌ expected mem-strong to win"; echo "$out"; exit 1; }
echo "$out" | grep -q "Applied 1 resolutions" || { echo "❌ expected 1 applied"; echo "$out"; exit 1; }
echo "   ✓ decisive resolution applied (mem-strong won, mem-weak slashed)"

# ── 6. Verify post-state: queue resolved, resolution memory exists ────
echo ""
echo "=== [6/8] Verify post-state of the decisive resolution ==="
resolved_at=$(sqlite3 "$MPM_SHARED_DB" "SELECT resolved_at FROM contradiction_log LIMIT 1")
test -n "$resolved_at" || { echo "❌ queue row not marked resolved"; exit 1; }
res_mem_id=$(sqlite3 "$MPM_SHARED_DB" "SELECT resolution_memory_id FROM contradiction_log LIMIT 1")
test -n "$res_mem_id" || { echo "❌ resolution_memory_id not set"; exit 1; }
n_resolutions=$(sqlite3 "$MPM_SHARED_DB" "SELECT COUNT(*) FROM memories WHERE collection='resolutions'")
test "$n_resolutions" = "1" || { echo "❌ expected 1 resolution memory, got $n_resolutions"; exit 1; }
loser_metadata=$(sqlite3 "$MPM_SHARED_DB" "SELECT metadata FROM memories WHERE id='mem-weak'")
echo "$loser_metadata" | grep -q "challenged" || { echo "❌ loser's metadata not updated"; echo "$loser_metadata"; exit 1; }
echo "   ✓ queue marked resolved, resolution memory created, loser metadata updated"

# ── 7. Verify arbitration path: close call proposes theory ────────────
echo ""
echo "=== [7/8] Verify arbitration path (close call proposes theory) ==="
sqlite3 "$MPM_SHARED_DB" <<EOF
INSERT INTO memories (id, collection, content, retrieval_priority, importance, confidence, weight, is_global, tags)
VALUES ('mem-equal-A', 'memories', 'a', 0.5, 0.5, 0.5, 1, 0, '[]'),
       ('mem-equal-B', 'memories', 'b', 0.5, 0.5, 0.5, 1, 0, '[]');

INSERT INTO contradiction_log (memory_id_a, memory_id_b, evidence, similarity, detected_by)
VALUES ('mem-equal-A', 'mem-equal-B', '{"memory_a":"mem-equal-A","memory_b":"mem-equal-B","evidence":"test close call","similarity":0.92}', 0.92, 'session-1');
EOF
out=$("$TMPDIR/mpm-bin" ops resolve-contradictions --apply 2>&1)
echo "$out" | grep -q "ARBITRATION" || { echo "❌ expected arbitration verdict"; echo "$out"; exit 1; }
n_theories=$(sqlite3 "$MPM_SHARED_DB" "SELECT COUNT(*) FROM memories WHERE collection='theories' AND id LIKE 'arbitration-%'")
test "$n_theories" = "1" || { echo "❌ expected 1 arbitration theory, got $n_theories"; exit 1; }
n_unresolved=$(sqlite3 "$MPM_SHARED_DB" "SELECT COUNT(*) FROM contradiction_log WHERE resolved_at IS NULL")
test "$n_unresolved" = "1" || { echo "❌ close call queue row should stay unresolved, got $n_unresolved resolved"; exit 1; }
echo "   ✓ arbitration theory proposed, queue row stays unresolved"

echo ""
echo -e "✅ Arc 1 smoke: conflict resolution loop closed end-to-end."
echo "   Decisive: slash + resolution memory + queue marked resolved."
echo "   Close call: arbitration theory proposed, queue stays open for human review."
# ── 8. Verify the resolution added evidence for the winner (Arc 1 closure) ─
echo ""
echo "=== [8/8] Verify resolution added evidence for the winner ==="
n_evidence=$(sqlite3 "$MPM_SHARED_DB" "SELECT COUNT(*) FROM evidence WHERE artifact_id='mem-strong' AND type='resolution_survived'")
test "$n_evidence" = "1" || { echo "❌ expected 1 evidence row for mem-strong, got $n_evidence"; exit 1; }
n_evidence_weak=$(sqlite3 "$MPM_SHARED_DB" "SELECT COUNT(*) FROM evidence WHERE artifact_id='mem-weak' AND type='resolution_survived'")
test "$n_evidence_weak" = "0" || { echo "❌ loser should not have resolution evidence, got $n_evidence_weak"; exit 1; }
evidence_strength=$(sqlite3 "$MPM_SHARED_DB" "SELECT strength FROM evidence WHERE artifact_id='mem-strong' AND type='resolution_survived'")
test "$evidence_strength" = "1.0" || { echo "❌ expected strength=1.0, got $evidence_strength"; exit 1; }
echo "   ✓ resolution evidence written for winner (strength=1.0, type=resolution_survived)"
echo "   ✓ loser has no resolution evidence (the signal is asymmetric)"

echo ""
echo -e "✅ Arc 1 + Arc 1 Closure smoke: full conflict resolution loop + evidence trail proven."
echo "   Decisive: slash + resolution memory + queue marked resolved + winner gets evidence."
echo "   Close call: arbitration theory proposed, queue row stays open for human review."
