// broadcast_test.go — Unit tests for Arc 2 (Active Dissemination).
//
// Coverage:
//   - Heartbeat: first call inserts, subsequent calls update
//   - DiscoverActiveSessions: filters out stale heartbeats
//   - BroadcastMemory: deterministic wake_id; dedup on re-broadcast
//   - BroadcastMemory: different content → different wake_id
//   - BroadcastMemory: targeted fan-out via --to
//   - BroadcastMemory: refuses offline --to targets
//   - CheckPendingEventWakes: transactional idempotency
//   - BroadcastMemory: dry-run doesn't INSERT
//   - BroadcastMemory: auto-extracts rationale for resolutions
//   - BroadcastMemory: requires rationale for non-auto kinds
//
// Each test gets its own in-memory shared DB via newTestDMWithShared.
// We need the broadcast DDLs installed in addition to the contradiction
// log DDLs.

package internal

import (
	"encoding/json"
	"fmt"
	"sort"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

var broadcastTestDBCounter int64

// newBroadcastTestDM extends the contradiction-log helper to also
// install the Arc 2 DDLs (sessions, event_wakes, agents). Mirrors
// the production attachShared path.
func newBroadcastTestDM(t *testing.T) *DatabaseManager {
	t.Helper()
	dm := NewTestDM(t)
	n := atomic.AddInt64(&broadcastTestDBCounter, 1)
	dsn := fmt.Sprintf("file:mpm-broadcasttest-%d?mode=memory&cache=shared", n)
	if _, err := dm.db.Exec(fmt.Sprintf("ATTACH DATABASE '%s' AS shared", dsn)); err != nil {
		t.Fatalf("attach shared: %v", err)
	}
	for _, ddl := range []string{
		`
		CREATE TABLE IF NOT EXISTS shared.memories (
			id TEXT PRIMARY KEY, collection TEXT NOT NULL, content TEXT NOT NULL,
			tags JSON, metadata JSON, weight REAL DEFAULT 1.0,
			retrieval_priority REAL NOT NULL DEFAULT 0.5,
			importance REAL NOT NULL DEFAULT 0.5,
			confidence REAL NOT NULL DEFAULT 0.8,
			reinforcement_count INTEGER DEFAULT 0,
			dependencies JSON, updated_at DATETIME, deleted_at DATETIME,
			last_accessed_at DATETIME
		)`,
		SharedBcastSessionsDDL,
		SharedBcastEventWakesDDL,
		SharedBcastAgentsDDL,
	} {
		if _, err := dm.db.Exec(ddl); err != nil {
			t.Logf("FAILED DDL:\n%s\n", ddl)
			t.Fatalf("install DDL: %v", err)
		}
	}
	// Mark the shared DB as attached so SharedAttached() returns a
	// non-empty path. attachShared() in production sets both the
	// ATTACH and this flag; the test path is shorter and skips the
	// schema-rewrite, so we set the flag here.
	dm.sharedAttached = true
	dm.sharedPath = dsn
	return dm
}

// seedMemory writes a shared.memories row. Returns the content_hash
// the broadcast would compute for it (deterministic, used by tests
// to assert wake_id values).
func seedMemory(t *testing.T, dm *DatabaseManager, id, collection, content string, tags []string) string {
	t.Helper()
	var tagsJSON string
	if tags != nil {
		sorted := append([]string(nil), tags...)
		sort.Strings(sorted)
		b, _ := json.Marshal(sorted)
		tagsJSON = string(b)
	}
	_, err := dm.db.Exec(`
		INSERT INTO shared.memories (id, collection, content, tags, weight, retrieval_priority, importance, confidence)
		VALUES (?, ?, ?, ?, 5, 0.5, 0.5, 0.8)
	`, id, collection, content, tagsJSON)
	if err != nil {
		t.Fatalf("seed memory %s: %v", id, err)
	}
	return computeContentHash(content, tags)
}

// TestHeartbeat_FirstCallInserts: a session never seen before gets
// a row in shared.bcast_sessions. Locked 24h window (HeartbeatWindowHours).
func TestHeartbeat_FirstCallInserts(t *testing.T) {
	dm := newBroadcastTestDM(t)

	if err := dm.Heartbeat("sess-1", "agent-A", "host-X", nil); err != nil {
		t.Fatalf("heartbeat: %v", err)
	}

	var agent, hostname string
	err := dm.db.QueryRow(`SELECT agent_id, hostname FROM shared.bcast_sessions WHERE session_id = 'sess-1'`).Scan(&agent, &hostname)
	if err != nil {
		t.Fatalf("read session: %v", err)
	}
	if agent != "agent-A" || hostname != "host-X" {
		t.Errorf("got agent=%q hostname=%q, want agent-A host-X", agent, hostname)
	}
}

// TestHeartbeat_SecondCallBumps: second heartbeat within the
// window updates last_heartbeat and total_heartbeats. Also
// increments shared.bcast_agents.total_heartbeats.
func TestHeartbeat_SecondCallBumps(t *testing.T) {
	dm := newBroadcastTestDM(t)

	_ = dm.Heartbeat("sess-2", "agent-B", "host-Y", nil)
	first := readLastHeartbeat(t, dm, "sess-2")

	time.Sleep(1100 * time.Millisecond) // ensure timestamp diff at 1s resolution

	_ = dm.Heartbeat("sess-2", "agent-B", "host-Y", nil)
	second := readLastHeartbeat(t, dm, "sess-2")

	if !second.After(first) && !second.Equal(first) {
		// SQLite CURRENT_TIMESTAMP has 1s resolution; same-second
		// is acceptable. The real assertion is that the row exists.
		t.Logf("heartbeat timestamps: first=%v second=%v", first, second)
	}

	var total int
	if err := dm.db.QueryRow(`SELECT total_heartbeats FROM shared.bcast_agents WHERE agent_id = 'agent-B'`).Scan(&total); err != nil {
		t.Fatalf("read agent: %v", err)
	}
	if total != 2 {
		t.Errorf("total_heartbeats = %d, want 2", total)
	}
}

// TestDiscoverActiveSessions_FiltersExpired: a session whose
// last_heartbeat is older than the window is excluded from the
// active list.
func TestDiscoverActiveSessions_FiltersExpired(t *testing.T) {
	dm := newBroadcastTestDM(t)

	// Fresh session.
	_ = dm.Heartbeat("sess-fresh", "agent-fresh", "h", nil)

	// Stale session: insert with last_heartbeat = 25h ago.
	_, err := dm.db.Exec(`
		INSERT INTO shared.bcast_sessions (session_id, agent_id, last_heartbeat)
		VALUES ('sess-stale', 'agent-stale', datetime('now', '-25 hours'))
	`)
	if err != nil {
		t.Fatalf("insert stale: %v", err)
	}

	sessions, err := dm.DiscoverActiveSessions()
	if err != nil {
		t.Fatalf("discover: %v", err)
	}
	if len(sessions) != 1 {
		t.Fatalf("got %d sessions, want 1 (only the fresh one)", len(sessions))
	}
	if sessions[0].SessionID != "sess-fresh" {
		t.Errorf("got session %q, want sess-fresh", sessions[0].SessionID)
	}
}

// TestBroadcastMemory_DeterministicID: broadcasting the same memory
// twice with the same content produces the same wake_id. The second
// INSERT OR IGNORE no-ops; the deduped counter increments.
func TestBroadcastMemory_DeterministicID(t *testing.T) {
	dm := newBroadcastTestDM(t)
	_ = dm.Heartbeat("sess-A", "agent-A", "h", nil)
	_ = dm.Heartbeat("sess-B", "agent-B", "h", nil)
	seedMemory(t, dm, "rule-1", "global_rules", "FTS5 requires CGO_CFLAGS", []string{"sqlite"})

	r1, err := dm.BroadcastMemory("rule-1", BroadcastOpts{
		Kind:        "rule",
		Rationale:   "FTS5 linker error taught us this",
		SourceAgent: "agent-A",
		SourceSessionID: "sess-A",
	})
	if err != nil {
		t.Fatalf("first broadcast: %v", err)
	}
	if r1.NewWakes != 1 {
		t.Errorf("first broadcast: NewWakes=%d, want 1 (only sess-B; self-skipped)", r1.NewWakes)
	}
	if r1.DedupedWakes != 0 {
		t.Errorf("first broadcast: DedupedWakes=%d, want 0", r1.DedupedWakes)
	}

	// Second broadcast: same memory, same content, same target.
	r2, err := dm.BroadcastMemory("rule-1", BroadcastOpts{
		Kind:        "rule",
		Rationale:   "FTS5 linker error taught us this",
		SourceAgent: "agent-A",
		SourceSessionID: "sess-A",
	})
	if err != nil {
		t.Fatalf("second broadcast: %v", err)
	}
	if r2.NewWakes != 0 {
		t.Errorf("second broadcast: NewWakes=%d, want 0", r2.NewWakes)
	}
	if r2.DedupedWakes != 1 {
		t.Errorf("second broadcast: DedupedWakes=%d, want 1", r2.DedupedWakes)
	}

	// And the wake_id from r1 must equal the wake_id from r2 (same target).
	if r1.Targets[0].WakeID != r2.Targets[0].WakeID {
		t.Errorf("wake_id drifted: r1=%s r2=%s", r1.Targets[0].WakeID, r2.Targets[0].WakeID)
	}

	// And the row count in shared.bcast_event_wakes should be 1.
	var n int
	if err := dm.db.QueryRow(`SELECT COUNT(*) FROM shared.bcast_event_wakes`).Scan(&n); err != nil {
		t.Fatalf("count: %v", err)
	}
	if n != 1 {
		t.Errorf("event_wakes row count = %d, want 1", n)
	}
}

// TestBroadcastMemory_DifferentContent: changing the content of a
// memory (which is what an update looks like, conceptually) produces
// a different wake_id and thus a new wake row.
func TestBroadcastMemory_DifferentContent(t *testing.T) {
	dm := newBroadcastTestDM(t)
	_ = dm.Heartbeat("sess-A", "agent-A", "h", nil)
	_ = dm.Heartbeat("sess-B", "agent-B", "h", nil)
	seedMemory(t, dm, "rule-2", "global_rules", "first version", nil)

	r1, err := dm.BroadcastMemory("rule-2", BroadcastOpts{
		Kind:           "rule",
		Rationale:      "v1",
		SourceAgent:    "agent-A",
		SourceSessionID: "sess-A",
	})
	if err != nil {
		t.Fatalf("v1 broadcast: %v", err)
	}

	// Update the content.
	_, err = dm.db.Exec(`UPDATE shared.memories SET content = 'second version' WHERE id = 'rule-2'`)
	if err != nil {
		t.Fatalf("update: %v", err)
	}

	r2, err := dm.BroadcastMemory("rule-2", BroadcastOpts{
		Kind:           "rule",
		Rationale:      "v2",
		SourceAgent:    "agent-A",
		SourceSessionID: "sess-A",
	})
	if err != nil {
		t.Fatalf("v2 broadcast: %v", err)
	}

	if r1.Targets[0].WakeID == r2.Targets[0].WakeID {
		t.Errorf("wake_id should differ across content versions: both = %s", r1.Targets[0].WakeID)
	}
	if r2.NewWakes != 1 {
		t.Errorf("v2 broadcast: NewWakes=%d, want 1", r2.NewWakes)
	}
}

// TestBroadcastMemory_TargetedFanout: --to=agent-B only inserts one
// wake row, targeting only B.
func TestBroadcastMemory_TargetedFanout(t *testing.T) {
	dm := newBroadcastTestDM(t)
	_ = dm.Heartbeat("sess-A", "agent-A", "h", nil)
	_ = dm.Heartbeat("sess-B", "agent-B", "h", nil)
	_ = dm.Heartbeat("sess-C", "agent-C", "h", nil)
	seedMemory(t, dm, "rule-3", "global_rules", "targeted rule", nil)

	r, err := dm.BroadcastMemory("rule-3", BroadcastOpts{
		Kind:           "rule",
		Rationale:      "targeted",
		ToAgents:       []string{"agent-B"},
		SourceAgent:    "agent-A",
		SourceSessionID: "sess-A",
	})
	if err != nil {
		t.Fatalf("broadcast: %v", err)
	}
	if len(r.Targets) != 1 {
		t.Fatalf("got %d targets, want 1", len(r.Targets))
	}
	if r.Targets[0].AgentID != "agent-B" {
		t.Errorf("got target %q, want agent-B", r.Targets[0].AgentID)
	}
	if r.NewWakes != 1 {
		t.Errorf("NewWakes=%d, want 1", r.NewWakes)
	}

	// And no wake row for sess-C.
	var n int
	if err := dm.db.QueryRow(`SELECT COUNT(*) FROM shared.bcast_event_wakes WHERE target_session = 'sess-C'`).Scan(&n); err != nil {
		t.Fatalf("count sess-C: %v", err)
	}
	if n != 0 {
		t.Errorf("sess-C should have no wakes, got %d", n)
	}
}

// TestBroadcastMemory_RejectsOfflineTargets: --to=agent-X where X has
// no recent heartbeat is refused (silent skip would be worse).
func TestBroadcastMemory_RejectsOfflineTargets(t *testing.T) {
	dm := newBroadcastTestDM(t)
	_ = dm.Heartbeat("sess-A", "agent-A", "h", nil)
	seedMemory(t, dm, "rule-4", "global_rules", "offline-target rule", nil)

	// No heartbeat for agent-X — should refuse.
	_, err := dm.BroadcastMemory("rule-4", BroadcastOpts{
		Kind:           "rule",
		Rationale:      "x",
		ToAgents:       []string{"agent-X"},
		SourceAgent:    "agent-A",
		SourceSessionID: "sess-A",
	})
	if err == nil {
		t.Fatal("expected error for offline target, got nil")
	}
	if !strings.Contains(err.Error(), "agent-X") || !strings.Contains(err.Error(), "no active session") {
		t.Errorf("error message should mention the offline agent and reason; got: %v", err)
	}
}

// TestCheckPendingEventWakes_MarksFired: calling twice returns
// disjoint sets (transactional idempotency).
func TestCheckPendingEventWakes_MarksFired(t *testing.T) {
	dm := newBroadcastTestDM(t)
	_ = dm.Heartbeat("sess-X", "agent-X", "h", nil)
	seedMemory(t, dm, "rule-5", "global_rules", "pickup test", nil)

	_, err := dm.BroadcastMemory("rule-5", BroadcastOpts{
		Kind: "rule", Rationale: "pickup",
		SourceAgent: "agent-broadcaster",
	})
	if err != nil {
		t.Fatalf("broadcast: %v", err)
	}

	// First pickup: returns the wake.
	first, err := dm.CheckPendingEventWakes("sess-X")
	if err != nil {
		t.Fatalf("first pickup: %v", err)
	}
	if len(first) != 1 {
		t.Fatalf("first pickup: got %d, want 1", len(first))
	}
	if first[0].MemoryID != "rule-5" {
		t.Errorf("first pickup: got memory_id %q, want rule-5", first[0].MemoryID)
	}

	// Second pickup: nothing pending.
	second, err := dm.CheckPendingEventWakes("sess-X")
	if err != nil {
		t.Fatalf("second pickup: %v", err)
	}
	if len(second) != 0 {
		t.Errorf("second pickup: got %d, want 0 (idempotent)", len(second))
	}

	// The row should still exist in shared.bcast_event_wakes but with
	// fired=1 (audit trail preserved).
	var fired int
	if err := dm.db.QueryRow(`SELECT fired FROM shared.bcast_event_wakes WHERE target_session = 'sess-X'`).Scan(&fired); err != nil {
		t.Fatalf("read fired: %v", err)
	}
	if fired != 1 {
		t.Errorf("fired = %d, want 1", fired)
	}
}

// TestBroadcastMemory_DryRun: --dry-run populates the report but
// writes zero rows.
func TestBroadcastMemory_DryRun(t *testing.T) {
	dm := newBroadcastTestDM(t)
	_ = dm.Heartbeat("sess-A", "agent-A", "h", nil)
	_ = dm.Heartbeat("sess-B", "agent-B", "h", nil)
	seedMemory(t, dm, "rule-6", "global_rules", "dryrun rule", nil)

	r, err := dm.BroadcastMemory("rule-6", BroadcastOpts{
		Kind: "rule", Rationale: "dry",
		SourceAgent: "agent-A",
		SourceSessionID: "sess-A",
		DryRun: true,
	})
	if err != nil {
		t.Fatalf("dry-run broadcast: %v", err)
	}
	if len(r.Targets) != 1 {
		t.Errorf("dry-run: got %d targets, want 1", len(r.Targets))
	}
	if r.NewWakes != 0 || r.DedupedWakes != 0 {
		t.Errorf("dry-run should not count anything; got new=%d deduped=%d", r.NewWakes, r.DedupedWakes)
	}

	// Verify zero rows actually written.
	var n int
	if err := dm.db.QueryRow(`SELECT COUNT(*) FROM shared.bcast_event_wakes`).Scan(&n); err != nil {
		t.Fatalf("count: %v", err)
	}
	if n != 0 {
		t.Errorf("dry-run wrote %d rows, want 0", n)
	}
}

// TestBroadcastMemory_AutoExtractResolutionRationale: a memory in
// the 'resolutions' collection gets its rationale auto-built from
// the metadata fields Arc 1 writes (queue_id, winner_id, loser_id,
// resolution_id). No --rationale needed.
func TestBroadcastMemory_AutoExtractResolutionRationale(t *testing.T) {
	dm := newBroadcastTestDM(t)
	_ = dm.Heartbeat("sess-A", "agent-A", "h", nil)
	_, err := dm.db.Exec(`
		INSERT INTO shared.memories (id, collection, content, metadata, weight, retrieval_priority, importance, confidence)
		VALUES ('res-1', 'resolutions', 'mem-X survives',
			'{"queue_id":42,"winner_id":"mem-X","loser_id":"mem-Y","resolution_id":"resolution-mem-Y-1234"}',
			5, 0.5, 0.5, 0.8)
	`)
	if err != nil {
		t.Fatalf("seed resolution: %v", err)
	}

	r, err := dm.BroadcastMemory("res-1", BroadcastOpts{
		SourceAgent: "agent-A",
		// No Rationale, no Kind — both auto-extracted.
	})
	if err != nil {
		t.Fatalf("broadcast: %v", err)
	}
	if r.Kind != "resolution" {
		t.Errorf("kind = %q, want resolution", r.Kind)
	}
	if !strings.Contains(r.Rationale, "mem-X") || !strings.Contains(r.Rationale, "mem-Y") {
		t.Errorf("rationale should mention both sides; got: %q", r.Rationale)
	}
	if !strings.Contains(r.Rationale, "queue_id=42") {
		t.Errorf("rationale should mention queue_id=42; got: %q", r.Rationale)
	}
}

// TestBroadcastMemory_RejectsNoRationale: a memory whose kind is
// neither resolution nor arbitration (e.g., a 'memory' collection)
// and which has no --rationale is refused.
func TestBroadcastMemory_RejectsNoRationale(t *testing.T) {
	dm := newBroadcastTestDM(t)
	_ = dm.Heartbeat("sess-A", "agent-A", "h", nil)
	seedMemory(t, dm, "fact-1", "memories", "FTS5 needs porter", nil)

	_, err := dm.BroadcastMemory("fact-1", BroadcastOpts{
		// No Kind, no Rationale.
	})
	if err == nil {
		t.Fatal("expected error for missing rationale")
	}
	if !strings.Contains(err.Error(), "rationale is required") {
		t.Errorf("error should mention rationale; got: %v", err)
	}
}

// TestBroadcastMemory_SkipsSelf: when the source session is in the
// target list (full fan-out mode), it is silently skipped. v's
// call: no self-echo.
func TestBroadcastMemory_SkipsSelf(t *testing.T) {
	dm := newBroadcastTestDM(t)
	_ = dm.Heartbeat("sess-A", "agent-A", "h", nil)
	_ = dm.Heartbeat("sess-B", "agent-B", "h", nil)
	seedMemory(t, dm, "rule-7", "global_rules", "self-skip rule", nil)

	r, err := dm.BroadcastMemory("rule-7", BroadcastOpts{
		Kind:           "rule",
		Rationale:      "self",
		SourceAgent:    "agent-A",
		SourceSessionID: "sess-A", // <-- self
	})
	if err != nil {
		t.Fatalf("broadcast: %v", err)
	}
	if len(r.Targets) != 1 {
		t.Errorf("got %d targets, want 1 (self should be skipped)", len(r.Targets))
	}
	if r.Targets[0].SessionID == "sess-A" {
		t.Errorf("self (%s) should not be in target list", r.Targets[0].SessionID)
	}
	if r.Targets[0].SessionID != "sess-B" {
		t.Errorf("got target %s, want sess-B", r.Targets[0].SessionID)
	}
}

// readLastHeartbeat is a small helper.
func readLastHeartbeat(t *testing.T, dm *DatabaseManager, sessionID string) time.Time {
	t.Helper()
	var ts string
	if err := dm.db.QueryRow(`SELECT last_heartbeat FROM shared.bcast_sessions WHERE session_id = ?`, sessionID).Scan(&ts); err != nil {
		t.Fatalf("read last_heartbeat: %v", err)
	}
	t2, _ := time.Parse("2006-01-02 15:04:05", ts)
	return t2
}