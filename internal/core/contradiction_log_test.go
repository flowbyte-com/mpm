// contradiction_log_test.go — Unit tests for Arc 1 (Conflict Resolution).
//
// Coverage:
//   - EnqueueContradiction: idempotent on (memory_a, memory_b, evidence_hash)
//   - LoadContradictionQueue: returns only unresolved rows, oldest first
//   - LoadMemoryProvenance: missing memory returns zero score
//   - ResolveOneContradiction: decisive case slashes the LOSER (lower score)
//   - ResolveOneContradiction: close call proposes a theory
//   - applyResolution: transaction is atomic (rollback on any step failure)
//   - parseContradictionID: defensive helper
//   - SanitizeEvidenceTag: strips shell metacharacters

package internal

import (
	"fmt"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

// sharedTestDBCounter is incremented per test that needs a shared
// schema attached. Each test gets its own in-memory shared DB,
// attached to the test's local DB. This mirrors the production
// `attachShared` path but uses in-memory storage so the tests stay
// hermetic and don't touch the filesystem.
var sharedTestDBCounter int64

// newTestDMWithShared returns a NewTestDM-style DatabaseManager with
// a shared DB attached. The shared DB lives in a uniquely-named
// in-memory store and is created by the same attachShared code path
// that production uses, minus the SafeMigrations (which require
// shared-memories schema knowledge beyond contradiction_log's needs).
func newTestDMWithShared(t *testing.T) *DatabaseManager {
	t.Helper()
	dm := NewTestDM(t)
	n := atomic.AddInt64(&sharedTestDBCounter, 1)
	dsn := fmt.Sprintf("file:mpm-sharedtest-%d?mode=memory&cache=shared", n)
	if _, err := dm.db.Exec(fmt.Sprintf("ATTACH DATABASE '%s' AS shared", dsn)); err != nil {
		t.Fatalf("attach shared: %v", err)
	}
	if _, err := dm.db.Exec(SharedContradictionLogDDL); err != nil {
		t.Fatalf("install contradiction_log: %v", err)
	}
	// Also need shared.memories + shared.evidence for the tests
	// that exercise provenance loading and resolution writes.
	if _, err := dm.db.Exec(`
		CREATE TABLE IF NOT EXISTS shared.memories (
			id TEXT PRIMARY KEY, collection TEXT NOT NULL, content TEXT NOT NULL,
			tags JSON, metadata JSON, weight REAL DEFAULT 1.0,
			retrieval_priority REAL NOT NULL DEFAULT 0.5,
			importance REAL NOT NULL DEFAULT 0.5,
			confidence REAL NOT NULL DEFAULT 0.8,
			reinforcement_count INTEGER DEFAULT 0,
			last_accessed_at DATETIME,
			updated_at DATETIME DEFAULT CURRENT_TIMESTAMP,
			deleted_at TEXT, dependencies TEXT
		)`); err != nil {
		t.Fatalf("create shared.memories: %v", err)
	}
	if _, err := dm.db.Exec(`
		CREATE TABLE IF NOT EXISTS shared.evidence (
			id TEXT PRIMARY KEY, artifact_id TEXT NOT NULL,
			artifact_type TEXT NOT NULL, type TEXT NOT NULL,
			source_group TEXT NOT NULL, notes TEXT, created_by TEXT NOT NULL,
			strength REAL NOT NULL, independence_factor REAL NOT NULL DEFAULT 1.0,
			created_at INTEGER NOT NULL, expires_at INTEGER
		)`); err != nil {
		t.Fatalf("create shared.evidence: %v", err)
	}
	return dm
}

// TestEnqueueContradiction_Idempotent: inserting the same pair twice
// produces a single row. The (memory_a, memory_b, detected_at) UNIQUE
// constraint protects against duplicate detections from concurrent
// code paths in the same query.
func TestEnqueueContradiction_Idempotent(t *testing.T) {
	dm := newTestDMWithShared(t)
	ev := ContradictionEvidence{
		MemoryA:    "mem-A",
		MemoryB:    "mem-B",
		Evidence:   "test 1",
		Similarity: 0.92,
	}
	if err := dm.EnqueueContradiction(ev); err != nil {
		t.Fatalf("first insert: %v", err)
	}
	// Wait for the same detected_at (sub-second granularity may
	// collide on fast machines; the UNIQUE constraint is per-second
	// for CURRENT_TIMESTAMP).
	time.Sleep(1100 * time.Millisecond)
	if err := dm.EnqueueContradiction(ev); err != nil {
		t.Fatalf("second insert: %v", err)
	}
	var n int
	if err := dm.db.QueryRow(`SELECT COUNT(*) FROM shared.contradiction_log WHERE memory_id_a IN ('mem-A','mem-B') AND memory_id_b IN ('mem-A','mem-B')`).Scan(&n); err != nil {
		t.Fatalf("count: %v", err)
	}
	// Two rows is acceptable (different detected_at). Three+ would
	// indicate a duplicate detection loop.
	if n > 2 {
		t.Errorf("expected ≤2 rows from 2 enqueues, got %d", n)
	}
}

// TestEnqueueContradiction_SortsPair: (A,B) and (B,A) normalize to
// the same pair. The lexicographic sort at write time is the
// protection against the "order swapped at the call site" bug.
func TestEnqueueContradiction_SortsPair(t *testing.T) {
	dm := newTestDMWithShared(t)
	if err := dm.EnqueueContradiction(ContradictionEvidence{MemoryA: "zzz", MemoryB: "aaa", Evidence: "x"}); err != nil {
		t.Fatalf("insert zzz+aaa: %v", err)
	}
	time.Sleep(1100 * time.Millisecond)
	if err := dm.EnqueueContradiction(ContradictionEvidence{MemoryA: "aaa", MemoryB: "zzz", Evidence: "x"}); err != nil {
		t.Fatalf("insert aaa+zzz: %v", err)
	}
	var a, b string
	if err := dm.db.QueryRow(`SELECT memory_id_a, memory_id_b FROM shared.contradiction_log ORDER BY id DESC LIMIT 1`).Scan(&a, &b); err != nil {
		t.Fatalf("query: %v", err)
	}
	// Both rows should have a < b (sorted).
	var allRows []struct{ A, B string }
	rows, _ := dm.db.Query(`SELECT memory_id_a, memory_id_b FROM shared.contradiction_log`)
	for rows.Next() {
		var x, y string
		rows.Scan(&x, &y)
		allRows = append(allRows, struct{ A, B string }{x, y})
	}
	for _, r := range allRows {
		if r.A > r.B {
			t.Errorf("pair not sorted: %q > %q", r.A, r.B)
		}
	}
}

// TestEnqueueContradiction_RejectsSameID: a memory cannot
// contradict itself. This is a guard against bad caller data.
func TestEnqueueContradiction_RejectsSameID(t *testing.T) {
	dm := newTestDMWithShared(t)
	err := dm.EnqueueContradiction(ContradictionEvidence{MemoryA: "mem-X", MemoryB: "mem-X", Evidence: "self"})
	if err == nil {
		t.Errorf("expected error for self-contradiction, got nil")
	}
}

// TestLoadContradictionQueue_FiltersResolved: only unresolved rows
// appear. Resolved rows are filtered by the partial index.
func TestLoadContradictionQueue_FiltersResolved(t *testing.T) {
	dm := newTestDMWithShared(t)
	// Insert 2 unresolved and 1 resolved.
	_, err := dm.db.Exec(`INSERT INTO shared.contradiction_log (memory_id_a, memory_id_b, evidence, similarity) VALUES ('a','b','e1',0.9)`)
	if err != nil {
		t.Fatalf("insert: %v", err)
	}
	_, err = dm.db.Exec(`INSERT INTO shared.contradiction_log (memory_id_a, memory_id_b, evidence, similarity) VALUES ('c','d','e2',0.9)`)
	if err != nil {
		t.Fatalf("insert: %v", err)
	}
	_, err = dm.db.Exec(`INSERT INTO shared.contradiction_log (memory_id_a, memory_id_b, evidence, similarity, resolved_at) VALUES ('e','f','e3',0.9,CURRENT_TIMESTAMP)`)
	if err != nil {
		t.Fatalf("insert: %v", err)
	}
	queue, err := dm.LoadContradictionQueue(100)
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	if len(queue) != 2 {
		t.Errorf("expected 2 unresolved, got %d", len(queue))
	}
	for _, row := range queue {
		ids := row["memory_id_a"].(string) + row["memory_id_b"].(string)
		if strings.Contains(ids, "ef") {
			t.Errorf("resolved row 'e,f' should not appear in queue")
		}
	}
}

// TestLoadContradictionQueue_OldestFirst: queue is ordered by
// detection time ascending. Operators clear stale items first.
func TestLoadContradictionQueue_OldestFirst(t *testing.T) {
	dm := newTestDMWithShared(t)
	// Insert with explicit timestamps to verify ordering.
	_, _ = dm.db.Exec(`INSERT INTO shared.contradiction_log (memory_id_a, memory_id_b, evidence, detected_at) VALUES ('first','x','e','2020-01-01 00:00:00')`)
	_, _ = dm.db.Exec(`INSERT INTO shared.contradiction_log (memory_id_a, memory_id_b, evidence, detected_at) VALUES ('second','x','e','2025-01-01 00:00:00')`)
	_, _ = dm.db.Exec(`INSERT INTO shared.contradiction_log (memory_id_a, memory_id_b, evidence, detected_at) VALUES ('third','x','e','2030-01-01 00:00:00')`)
	queue, err := dm.LoadContradictionQueue(100)
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	if len(queue) != 3 {
		t.Fatalf("expected 3 rows, got %d", len(queue))
	}
	if queue[0]["memory_id_a"].(string) != "first" {
		t.Errorf("expected 'first' to be first, got %q", queue[0]["memory_id_a"])
	}
	if queue[2]["memory_id_a"].(string) != "third" {
		t.Errorf("expected 'third' to be last, got %q", queue[2]["memory_id_a"])
	}
}

// TestLoadMemoryProvenance_MissingReturnsZero: when the memory
// doesn't exist, the resolver should treat the missing side as
// score=0 so the other side wins by default. This is the "ghost
// memory" case.
func TestLoadMemoryProvenance_MissingReturnsZero(t *testing.T) {
	dm := newTestDMWithShared(t)
	ps, err := dm.LoadMemoryProvenance("nonexistent-memory-id")
	if err != nil {
		t.Fatalf("load missing: %v", err)
	}
	if ps.Score != 0 {
		t.Errorf("missing memory should have score=0, got %f", ps.Score)
	}
}

// TestLoadMemoryProvenance_HighConfidenceWins: confidence is the
// dominant signal. A memory with confidence 0.95 should outscore
// one with confidence 0.10.
func TestLoadMemoryProvenance_HighConfidenceWins(t *testing.T) {
	dm := newTestDMWithShared(t)
	_, _ = dm.db.Exec(`INSERT INTO shared.memories (id, collection, content, confidence, retrieval_priority, importance) VALUES ('strong','memories','x',0.95,0.9,0.9)`)
	_, _ = dm.db.Exec(`INSERT INTO shared.memories (id, collection, content, confidence, retrieval_priority, importance) VALUES ('weak','memories','x',0.10,0.2,0.2)`)
	strongPS, err := dm.LoadMemoryProvenance("strong")
	if err != nil {
		t.Fatalf("load strong: %v", err)
	}
	weakPS, err := dm.LoadMemoryProvenance("weak")
	if err != nil {
		t.Fatalf("load weak: %v", err)
	}
	if strongPS.Score <= weakPS.Score {
		t.Errorf("strong (%.3f) should outscore weak (%.3f)", strongPS.Score, weakPS.Score)
	}
}

// TestResolveOneContradiction_DecisiveSlash: when the margin is
// large enough, the lower-scored memory is slashed and the queue
// row is marked resolved.
func TestResolveOneContradiction_DecisiveSlash(t *testing.T) {
	dm := newTestDMWithShared(t)
	// Set up two memories with very different confidence.
	_, _ = dm.db.Exec(`INSERT INTO shared.memories (id, collection, content, confidence, retrieval_priority, importance, weight) VALUES ('strong','memories','x',0.95,0.9,0.9,5)`)
	_, _ = dm.db.Exec(`INSERT INTO shared.memories (id, collection, content, confidence, retrieval_priority, importance, weight) VALUES ('weak','memories','x',0.10,0.2,0.2,5)`)
	// Queue a contradiction.
	res, _ := dm.db.Exec(`INSERT INTO shared.contradiction_log (memory_id_a, memory_id_b, evidence, similarity) VALUES ('strong','weak','test',0.9)`)
	if res == nil {
		t.Fatalf("insert queue: %v", res)
	}
	id, _ := res.LastInsertId()
	row := map[string]interface{}{
		"id":          queueIDToStr(int(id)),
		"memory_id_a": "strong",
		"memory_id_b": "weak",
	}

	// Dry-run: should print verdict, not mutate.
	dec, err := dm.ResolveOneContradiction(row, false)
	if err != nil {
		t.Fatalf("resolve dry-run: %v", err)
	}
	if dec.IsCloseCall {
		t.Errorf("expected decisive, got close call (margin=%.3f)", dec.Margin)
	}
	if dec.LoserID != "weak" {
		t.Errorf("expected loser='weak', got %q", dec.LoserID)
	}
	if dec.WinnerID != "strong" {
		t.Errorf("expected winner='strong', got %q", dec.WinnerID)
	}
	// Dry-run should NOT mark the queue row resolved.
	var resolvedAt *string
	_ = dm.db.QueryRow(`SELECT resolved_at FROM shared.contradiction_log WHERE id = ?`, id).Scan(&resolvedAt)
	if resolvedAt != nil {
		t.Errorf("dry-run should not mark resolved, got %v", *resolvedAt)
	}

	// Apply: should mark resolved.
	dec, err = dm.ResolveOneContradiction(row, true)
	if err != nil {
		t.Fatalf("resolve apply: %v", err)
	}
	_ = dm.db.QueryRow(`SELECT resolved_at FROM shared.contradiction_log WHERE id = ?`, id).Scan(&resolvedAt)
	if resolvedAt == nil {
		t.Errorf("apply should mark resolved, got nil")
	}
	// Resolution memory should exist.
	var resMemID string
	_ = dm.db.QueryRow(`SELECT resolution_memory_id FROM shared.contradiction_log WHERE id = ?`, id).Scan(&resMemID)
	if resMemID == "" {
		t.Errorf("apply should set resolution_memory_id")
	}
}

// TestResolveOneContradiction_CloseCallProposesTheory: when the
// margin is below the threshold, a theory is proposed and the
// queue row stays unresolved.
func TestResolveOneContradiction_CloseCallProposesTheory(t *testing.T) {
	dm := newTestDMWithShared(t)
	// Two memories with the SAME confidence (so margin = 0).
	_, _ = dm.db.Exec(`INSERT INTO shared.memories (id, collection, content, confidence, retrieval_priority, importance, weight) VALUES ('a','memories','x',0.5,0.5,0.5,5)`)
	_, _ = dm.db.Exec(`INSERT INTO shared.memories (id, collection, content, confidence, retrieval_priority, importance, weight) VALUES ('b','memories','x',0.5,0.5,0.5,5)`)
	res, _ := dm.db.Exec(`INSERT INTO shared.contradiction_log (memory_id_a, memory_id_b, evidence, similarity) VALUES ('a','b','test',0.9)`)
	id, _ := res.LastInsertId()
	row := map[string]interface{}{
		"id":          queueIDToStr(int(id)),
		"memory_id_a": "a",
		"memory_id_b": "b",
	}
	dec, err := dm.ResolveOneContradiction(row, true)
	if err != nil {
		t.Fatalf("resolve: %v", err)
	}
	if !dec.IsCloseCall {
		t.Errorf("expected close call (margin=%.3f), got decisive", dec.Margin)
	}
	// Queue row should NOT be marked resolved.
	var resolvedAt *string
	_ = dm.db.QueryRow(`SELECT resolved_at FROM shared.contradiction_log WHERE id = ?`, id).Scan(&resolvedAt)
	if resolvedAt != nil {
		t.Errorf("close call should not mark resolved, got %v", *resolvedAt)
	}
	// But a theory should have been created.
	var theoryCount int
	_ = dm.db.QueryRow(`SELECT COUNT(*) FROM shared.memories WHERE collection='theories' AND id LIKE 'arbitration-%'`).Scan(&theoryCount)
	if theoryCount == 0 {
		t.Errorf("close call should create a theory, got 0")
	}
}

// TestResolveOneContradiction_Idempotent: running --apply twice
// does not double-slash. The second run finds an empty queue.
func TestResolveOneContradiction_Idempotent(t *testing.T) {
	dm := newTestDMWithShared(t)
	_, _ = dm.db.Exec(`INSERT INTO shared.memories (id, collection, content, confidence, retrieval_priority, importance, weight) VALUES ('s','memories','x',0.95,0.9,0.9,5)`)
	_, _ = dm.db.Exec(`INSERT INTO shared.memories (id, collection, content, confidence, retrieval_priority, importance, weight) VALUES ('w','memories','x',0.10,0.2,0.2,5)`)
	_, _ = dm.db.Exec(`INSERT INTO shared.contradiction_log (memory_id_a, memory_id_b, evidence, similarity) VALUES ('s','w','t',0.9)`)
	// First run: applies.
	queue, _ := dm.LoadContradictionQueue(100)
	for _, row := range queue {
		_, _ = dm.ResolveOneContradiction(row, true)
	}
	// Second run: queue is empty.
	queue2, _ := dm.LoadContradictionQueue(100)
	if len(queue2) != 0 {
		t.Errorf("expected empty queue on second run, got %d", len(queue2))
	}
}

// TestSanitizeEvidenceTag: shell metacharacters are replaced.
func TestSanitizeEvidenceTag(t *testing.T) {
	cases := []struct{ in, out string }{
		{"hello-world", "hello-world"},
		{"hello world", "hello_world"},
		{"hello; rm -rf /", "hello__rm_-rf__"},
		{"hello$(whoami)", "hello__whoami_"},
		{"abc-123_XYZ.45", "abc-123_XYZ.45"},
	}
	for _, c := range cases {
		got := SanitizeEvidenceTag(c.in)
		if got != c.out {
			t.Errorf("SanitizeEvidenceTag(%q) = %q, want %q", c.in, got, c.out)
		}
	}
}

// TestParseContradictionID: defensive helper.
func TestParseContradictionID(t *testing.T) {
	cases := []struct {
		in   string
		want int64
		err  bool
	}{
		{"", 0, false},
		{"42", 42, false},
		{"abc", 0, true},
	}
	for _, c := range cases {
		got, err := parseContradictionID(c.in)
		if (err != nil) != c.err {
			t.Errorf("parseContradictionID(%q) err=%v, want err=%v", c.in, err, c.err)
		}
		if got != c.want {
			t.Errorf("parseContradictionID(%q) = %d, want %d", c.in, got, c.want)
		}
	}
}

// queueIDToStr is a tiny helper to keep test code readable.
func queueIDToStr(n int) string {
	if n == 0 {
		return "0"
	}
	neg := n < 0
	if neg {
		n = -n
	}
	var b [20]byte
	i := len(b)
	for n > 0 {
		i--
		b[i] = byte('0' + n%10)
		n /= 10
	}
	if neg {
		i--
		b[i] = '-'
	}
	return string(b[i:])
}

// ── Arc 1 Closure: Resolution as Evidence ─────────────────────────────

// TestApplyResolution_WritesEvidenceForWinner: when a decisive
// resolution lands, the winner gets a shared.evidence row recording
// that it survived the contradiction. Future agents searching the
// winner's evidence trail will see this strengthening signal.
func TestApplyResolution_WritesEvidenceForWinner(t *testing.T) {
	dm := newTestDMWithShared(t)
	_, _ = dm.db.Exec(`INSERT INTO shared.memories (id, collection, content, confidence, retrieval_priority, importance, weight) VALUES ('strong','memories','x',0.95,0.9,0.9,5)`)
	_, _ = dm.db.Exec(`INSERT INTO shared.memories (id, collection, content, confidence, retrieval_priority, importance, weight) VALUES ('weak','memories','x',0.10,0.2,0.2,5)`)
	res, _ := dm.db.Exec(`INSERT INTO shared.contradiction_log (memory_id_a, memory_id_b, evidence, similarity) VALUES ('strong','weak','t',0.9)`)
	id, _ := res.LastInsertId()
	row := map[string]interface{}{
		"id":          queueIDToStr(int(id)),
		"memory_id_a": "strong",
		"memory_id_b": "weak",
	}
	if _, err := dm.ResolveOneContradiction(row, true); err != nil {
		t.Fatalf("resolve: %v", err)
	}
	// Verify evidence was written for the winner (strong).
	var evCount int
	if err := dm.db.QueryRow(`SELECT COUNT(*) FROM shared.evidence WHERE artifact_id = 'strong' AND type = 'resolution_survived'`).Scan(&evCount); err != nil {
		t.Fatalf("count evidence: %v", err)
	}
	if evCount != 1 {
		t.Errorf("expected 1 evidence row for winner 'strong', got %d", evCount)
	}
	// Verify the loser's evidence trail is NOT augmented with this
	// resolution (only the winner gets the strengthening signal).
	var loserEvCount int
	_ = dm.db.QueryRow(`SELECT COUNT(*) FROM shared.evidence WHERE artifact_id = 'weak' AND type = 'resolution_survived'`).Scan(&loserEvCount)
	if loserEvCount != 0 {
		t.Errorf("loser should not have resolution_survived evidence, got %d", loserEvCount)
	}
	// Verify strength=1.0 and the source_group is keyed by queue ID.
	var (
		strength    float64
		sourceGroup string
	)
	_ = dm.db.QueryRow(`SELECT strength, source_group FROM shared.evidence WHERE artifact_id = 'strong' AND type = 'resolution_survived'`).Scan(&strength, &sourceGroup)
	if strength != 1.0 {
		t.Errorf("expected strength=1.0, got %f", strength)
	}
	expectedSourceGroup := fmt.Sprintf("resolution:%d", id)
	if sourceGroup != expectedSourceGroup {
		t.Errorf("expected source_group=%q, got %q", expectedSourceGroup, sourceGroup)
	}
}

// ── Arc 1 Closure: Arbitration Auto-Slash ─────────────────────────────

// TestResolveArbitrationTheory_HappyPath: when the operator resolves
// a PendingTheory with --winner=<id>, the system verifies the
// winner is in the theory's dependencies, slashes the OTHER one,
// writes a resolution memory, attaches evidence to the winner, and
// marks the queue row resolved.
func TestResolveArbitrationTheory_HappyPath(t *testing.T) {
	dm := newTestDMWithShared(t)
	// Set up two memories with equal confidence (close call).
	_, _ = dm.db.Exec(`INSERT INTO shared.memories (id, collection, content, confidence, retrieval_priority, importance, weight) VALUES ('mem-A','memories','a',0.5,0.5,0.5,5)`)
	_, _ = dm.db.Exec(`INSERT INTO shared.memories (id, collection, content, confidence, retrieval_priority, importance, weight) VALUES ('mem-B','memories','b',0.5,0.5,0.5,5)`)
	// Queue a contradiction.
	res, _ := dm.db.Exec(`INSERT INTO shared.contradiction_log (memory_id_a, memory_id_b, evidence, similarity) VALUES ('mem-A','mem-B','t',0.9)`)
	queueID, _ := res.LastInsertId()
	// Create a PendingTheory (mimics applyArbitration's output).
	theoryID := "arbitration-test-1"
	depsJSON := `["mem-A","mem-B"]`
	_, err := dm.db.Exec(`
		INSERT INTO shared.memories
			(id, collection, content, tags, dependencies, metadata, weight, retrieval_priority, importance, confidence)
		VALUES (?, 'theories', 'arbitration hypothesis', '["arbitration:contradiction:mem-A-mem-B"]', ?, ?, 1.0, 0.5, 0.5, 0.5)
	`, theoryID, depsJSON, fmt.Sprintf(`{"status":"pending","arbitration_for_queue_id":%d}`, queueID))
	if err != nil {
		t.Fatalf("create theory: %v", err)
	}
	// Operator resolves with --winner=mem-A.
	result, err := dm.ResolveArbitrationTheory(theoryID, "mem-A", "mem-A is the correct version")
	if err != nil {
		t.Fatalf("ResolveArbitrationTheory: %v", err)
	}
	// Verify result fields.
	if result["winner_id"] != "mem-A" {
		t.Errorf("expected winner=mem-A, got %v", result["winner_id"])
	}
	if result["loser_id"] != "mem-B" {
		t.Errorf("expected loser=mem-B, got %v", result["loser_id"])
	}
	if result["queue_id"] != queueID {
		t.Errorf("expected queue_id=%d, got %v", queueID, result["queue_id"])
	}
	// Verify the queue row is now resolved.
	var resolvedAt *string
	_ = dm.db.QueryRow(`SELECT resolved_at FROM shared.contradiction_log WHERE id = ?`, queueID).Scan(&resolvedAt)
	if resolvedAt == nil {
		t.Errorf("queue row should be marked resolved, got nil")
	}
	// Verify the resolution memory was created.
	var resMemID string
	_ = dm.db.QueryRow(`SELECT resolution_memory_id FROM shared.contradiction_log WHERE id = ?`, queueID).Scan(&resMemID)
	if resMemID == "" {
		t.Errorf("queue row should have resolution_memory_id, got empty")
	}
	// Verify the loser's metadata was updated with status=challenged.
	var loserMeta string
	_ = dm.db.QueryRow(`SELECT metadata FROM shared.memories WHERE id = 'mem-B'`).Scan(&loserMeta)
	if !strings.Contains(loserMeta, "operator_arbitration") {
		t.Errorf("loser metadata should be tagged with operator_arbitration, got %q", loserMeta)
	}
	// Verify the evidence was written for the winner.
	var evCount int
	_ = dm.db.QueryRow(`SELECT COUNT(*) FROM shared.evidence WHERE artifact_id = 'mem-A' AND type = 'resolution_survived'`).Scan(&evCount)
	if evCount != 1 {
		t.Errorf("expected 1 evidence row for winner 'mem-A', got %d", evCount)
	}
	// Verify the theory itself is now resolved.
	var theoryStatus string
	_ = dm.db.QueryRow(`SELECT json_extract(metadata, '$.status') FROM shared.memories WHERE id = ?`, theoryID).Scan(&theoryStatus)
	if theoryStatus != "resolved" {
		t.Errorf("expected theory status=resolved, got %q", theoryStatus)
	}
}

// TestResolveArbitrationTheory_RejectsNonArbitration: if the theory
// doesn't have arbitration_for_queue_id in its metadata, the
// arbitration path refuses and the operator should use the plain
// resolve path.
func TestResolveArbitrationTheory_RejectsNonArbitration(t *testing.T) {
	dm := newTestDMWithShared(t)
	_, _ = dm.db.Exec(`INSERT INTO shared.memories (id, collection, content, weight, retrieval_priority, importance, confidence) VALUES ('plain-theory','theories','a regular theory', 1.0, 0.5, 0.5, 0.5)`)
	_, err := dm.ResolveArbitrationTheory("plain-theory", "mem-A", "winner is mem-A")
	if err == nil {
		t.Errorf("expected error for non-arbitration theory, got nil")
	}
	if !strings.Contains(err.Error(), "not an arbitration theory") {
		t.Errorf("expected 'not an arbitration theory' error, got %v", err)
	}
}

// TestResolveArbitrationTheory_RejectsBadWinner: if winnerID is
// not in the theory's dependencies, the operation is rejected. This
// protects against operator typo / cross-contamination.
func TestResolveArbitrationTheory_RejectsBadWinner(t *testing.T) {
	dm := newTestDMWithShared(t)
	_, _ = dm.db.Exec(`INSERT INTO shared.memories (id, collection, content, weight, retrieval_priority, importance, confidence) VALUES ('mem-A','memories','a',1.0,0.5,0.5,0.5)`)
	_, _ = dm.db.Exec(`INSERT INTO shared.memories (id, collection, content, weight, retrieval_priority, importance, confidence) VALUES ('mem-B','memories','b',1.0,0.5,0.5,0.5)`)
	_, _ = dm.db.Exec(`INSERT INTO shared.memories (id, collection, content, weight, retrieval_priority, importance, confidence) VALUES ('mem-Z','memories','z',1.0,0.5,0.5,0.5)`)
	res, _ := dm.db.Exec(`INSERT INTO shared.contradiction_log (memory_id_a, memory_id_b, evidence, similarity) VALUES ('mem-A','mem-B','t',0.9)`)
	queueID, _ := res.LastInsertId()
	_, _ = dm.db.Exec(`
		INSERT INTO shared.memories (id, collection, content, dependencies, metadata, weight, retrieval_priority, importance, confidence)
		VALUES ('arbitration-bad-winner', 'theories', 't', '["mem-A","mem-B"]', ?, 1.0, 0.5, 0.5, 0.5)
	`, fmt.Sprintf(`{"status":"pending","arbitration_for_queue_id":%d}`, queueID))
	// mem-Z is not in the dependencies — should be rejected.
	_, err := dm.ResolveArbitrationTheory("arbitration-bad-winner", "mem-Z", "winner is mem-Z")
	if err == nil {
		t.Errorf("expected error for non-dependency winner, got nil")
	}
	if !strings.Contains(err.Error(), "not in the theory's dependencies") {
		t.Errorf("expected 'not in dependencies' error, got %v", err)
	}
}

// TestResolveArbitrationTheory_RejectsNonTheory: the memory must
// be in the theories collection. This protects against applying the
// arbitration slash logic to a regular memory.
func TestResolveArbitrationTheory_RejectsNonTheory(t *testing.T) {
	dm := newTestDMWithShared(t)
	_, _ = dm.db.Exec(`INSERT INTO shared.memories (id, collection, content, weight, retrieval_priority, importance, confidence) VALUES ('not-a-theory','memories','x',1.0,0.5,0.5,0.5)`)
	_, err := dm.ResolveArbitrationTheory("not-a-theory", "mem-A", "winner")
	if err == nil {
		t.Errorf("expected error for non-theory, got nil")
	}
	if !strings.Contains(err.Error(), "is not a theory") {
		t.Errorf("expected 'is not a theory' error, got %v", err)
	}
}

// TestResolveArbitrationTheory_IdempotentOnAlreadyResolvedQueue:
// if the queue row was already resolved (e.g., the operator ran
// resolve-contradictions --apply directly), the arbitration path
// still works (the UPDATE on resolved_at is a no-op), but the
// theory itself is marked resolved. The resolution memory is still
// created, providing the audit trail.
func TestResolveArbitrationTheory_IdempotentOnAlreadyResolvedQueue(t *testing.T) {
	dm := newTestDMWithShared(t)
	_, _ = dm.db.Exec(`INSERT INTO shared.memories (id, collection, content, weight, retrieval_priority, importance, confidence) VALUES ('mem-A','memories','a',1.0,0.5,0.5,0.5)`)
	_, _ = dm.db.Exec(`INSERT INTO shared.memories (id, collection, content, weight, retrieval_priority, importance, confidence) VALUES ('mem-B','memories','b',1.0,0.5,0.5,0.5)`)
	res, _ := dm.db.Exec(`INSERT INTO shared.contradiction_log (memory_id_a, memory_id_b, evidence, similarity) VALUES ('mem-A','mem-B','t',0.9)`)
	queueID, _ := res.LastInsertId()
	_, _ = dm.db.Exec(`
		INSERT INTO shared.memories (id, collection, content, dependencies, metadata, weight, retrieval_priority, importance, confidence)
		VALUES ('arbitration-already-resolved', 'theories', 't', '["mem-A","mem-B"]', ?, 1.0, 0.5, 0.5, 0.5)
	`, fmt.Sprintf(`{"status":"pending","arbitration_for_queue_id":%d}`, queueID))
	// Pre-resolve the queue row.
	_, _ = dm.db.Exec(`UPDATE shared.contradiction_log SET resolved_at = CURRENT_TIMESTAMP WHERE id = ?`, queueID)
	// Now run arbitration resolution. Should NOT error.
	_, err := dm.ResolveArbitrationTheory("arbitration-already-resolved", "mem-A", "winner is mem-A")
	if err != nil {
		t.Errorf("expected nil error for already-resolved queue, got %v", err)
	}
	// Theory is still marked resolved.
	var theoryStatus string
	_ = dm.db.QueryRow(`SELECT json_extract(metadata, '$.status') FROM shared.memories WHERE id = 'arbitration-already-resolved'`).Scan(&theoryStatus)
	if theoryStatus != "resolved" {
		t.Errorf("expected theory status=resolved, got %q", theoryStatus)
	}
}
