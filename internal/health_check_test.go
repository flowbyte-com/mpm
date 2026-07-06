package internal

import (
	"testing"
)

// TestHealthCheck_Basic verifies the new DatabaseManager.HealthCheck
// method returns sensible defaults: integrity ok, busy_retries zero
// on a fresh DM, and the domain-count keys are present (zero or more).
// Pins the "is everything healthy?" contract — replaces 6 separate
// lookups with a single method call.
func TestHealthCheck_Basic(t *testing.T) {
	dm := newTestDM(t)

	h, err := dm.HealthCheck()
	if err != nil {
		t.Fatalf("HealthCheck: %v", err)
	}

	if ok, _ := h["ok"].(bool); !ok {
		t.Errorf("ok: got false, want true (integrity check should pass on fresh in-memory DB); full=%+v", h)
	}
	if r, _ := h["busy_retries"].(uint64); r != 0 {
		t.Errorf("busy_retries: got %d, want 0 on fresh DM", r)
	}

	// Domain counts — must be present and >=0.
	for _, k := range []string{"memories_active", "theories_pending", "wakes_overdue", "evidence_total"} {
		if _, ok := h[k]; !ok {
			t.Errorf("key %q missing from health check", k)
		}
	}

	// Page count must be present and >0 (in-memory DB has at least one page).
	if pc, ok := h["page_count"].(int64); !ok || pc <= 0 {
		t.Errorf("page_count: got %v (type %T), want positive int64", h["page_count"], h["page_count"])
	}
}

// TestHealthCheck_AfterWritesAndWakes verifies HealthCheck reflects
// domain state — adding a memory bumps memories_active, scheduling a
// past wake bumps wakes_overdue. Pins that the digest-style counts
// actually track what they claim.
func TestHealthCheck_AfterWritesAndWakes(t *testing.T) {
	dm := newTestDM(t)

	// Insert a memory directly via SQL.
	_, err := dm.db.Exec(`
		INSERT INTO memories (id, collection, content, deleted_at, created_at, updated_at)
		VALUES ('mem-health-1', 'memories', 'health check test', NULL, '2026-07-06', '2026-07-06')
	`)
	if err != nil {
		t.Fatalf("insert memory: %v", err)
	}

	// Schedule an overdue wake.
	_, err = dm.db.Exec(`
		INSERT INTO scheduled_wakes (id, target_time, reason, fired, created_by)
		VALUES ('wk-health-1', 1000000, 'overdue wake', 0, 'test')
	`)
	if err != nil {
		t.Fatalf("insert wake: %v", err)
	}

	h, err := dm.HealthCheck()
	if err != nil {
		t.Fatalf("HealthCheck: %v", err)
	}

	if n, _ := h["memories_active"].(int64); n != 1 {
		t.Errorf("memories_active: got %d, want 1", n)
	}
	if n, _ := h["wakes_overdue"].(int64); n != 1 {
		t.Errorf("wakes_overdue: got %d, want 1 (target_time=1000000 is in the past)", n)
	}
}

// TestBusyRetryCount_StartsAtZero pins the lifetime counter is zero
// on a fresh DM. Bumping the counter requires triggering a real
// SQLITE_BUSY, which is hard to do reliably in a test without driving
// two transactions against the same DB; this test just pins the
// baseline, not the increment path.
func TestBusyRetryCount_StartsAtZero(t *testing.T) {
	dm := newTestDM(t)
	if r := dm.BusyRetryCount(); r != 0 {
		t.Errorf("BusyRetryCount on fresh DM: got %d, want 0", r)
	}
}
