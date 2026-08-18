// Tests for the 7-day notification-wake retention sweep
// (wake_expiration.go).
//
// The sweep retires notification-kind scheduled_wakes whose
// target_time is more than 7 days in the past by marking them
// fired=1 with an audit note in metadata. These tests pin every
// guarantee from the wake_expiration.go doc-comment:

package scheduler

import (
	"context"
	"database/sql"
	"encoding/json"
	"strconv"
	"strings"
	"testing"
	"time"
)

// readWakeFired reads the fired flag and metadata for a wake by id.
// Used to assert the post-sweep state without depending on the
// scheduler's read APIs.
func readWakeFired(t *testing.T, db *sql.DB, id string) (fired int, firedAt int64, metadata string) {
	t.Helper()
	err := db.QueryRow(
		`SELECT fired, COALESCE(fired_at, 0), COALESCE(metadata, '{}') FROM scheduled_wakes WHERE id = ?`,
		id,
	).Scan(&fired, &firedAt, &metadata)
	if err != nil {
		t.Fatalf("read wake %s: %v", id, err)
	}
	return
}

// TestSweepRetainsRecentNotificationWakes — the grace period.
//
// A notification wake overdue by less than 7 days must remain in
// the queue so the mpm-mcp opportunistic fold can still pick it up.
// This is the "no false-positive retirement" guarantee.
func TestSweepRetainsRecentNotificationWakes(t *testing.T) {
	s := newTestScheduler(t)
	now := time.Now()

	// Three recent-overdue notifications: 1 hour, 1 day, 6 days.
	// All three are well inside the 7-day window.
	for _, c := range []struct {
		id   string
		when time.Time
	}{
		{"recent-1h", now.Add(-1 * time.Hour)},
		{"recent-1d", now.Add(-24 * time.Hour)},
		{"recent-6d", now.Add(-6 * 24 * time.Hour)},
	} {
		seedWake(t, s, c.id, c.when, "notification")
	}

	// One pending (not yet due) notification — must also remain.
	seedWake(t, s, "future", now.Add(1*time.Hour), "notification")

	retired, err := SweepOverdueNotificationWakes(context.Background(), s.db, now)
	if err != nil {
		t.Fatalf("sweep: %v", err)
	}
	if retired != 0 {
		t.Errorf("sweep retired %d recent notifications, want 0", retired)
	}

	// Confirm every recent wake is still unfired and the pending
	// wake is still pending.
	for _, id := range []string{"recent-1h", "recent-1d", "recent-6d", "future"} {
		fired, _, _ := readWakeFired(t, s.db, id)
		if fired != 0 {
			t.Errorf("%s: fired=%d after sweep, want 0 (under 7d)", id, fired)
		}
	}
}

// TestSweepRetiresNotificationWakesOver7Days — the retirement path.
//
// A notification wake overdue by more than 7 days must transition
// to fired=1, fired_at=now, with the audit note in metadata. The
// row stays in scheduled_wakes (no DELETE) so the audit history is
// preserved.
func TestSweepRetiresNotificationWakesOver7Days(t *testing.T) {
	s := newTestScheduler(t)
	now := time.Now()

	// Three stale-overdue notifications: 8 days, 30 days, 90 days.
	// All three are past the 7-day window.
	for _, c := range []struct {
		id   string
		when time.Time
	}{
		{"stale-8d", now.Add(-8 * 24 * time.Hour)},
		{"stale-30d", now.Add(-30 * 24 * time.Hour)},
		{"stale-90d", now.Add(-90 * 24 * time.Hour)},
	} {
		seedWake(t, s, c.id, c.when, "notification")
	}

	retired, err := SweepOverdueNotificationWakes(context.Background(), s.db, now)
	if err != nil {
		t.Fatalf("sweep: %v", err)
	}
	if retired != 3 {
		t.Errorf("sweep retired %d wakes, want 3", retired)
	}

	// Confirm every stale wake is fired=1 with the audit note.
	for _, id := range []string{"stale-8d", "stale-30d", "stale-90d"} {
		fired, firedAt, metadata := readWakeFired(t, s.db, id)
		if fired != 1 {
			t.Errorf("%s: fired=%d after sweep, want 1", id, fired)
		}
		if firedAt != now.Unix() {
			t.Errorf("%s: fired_at=%d, want %d", id, firedAt, now.Unix())
		}
		// Audit note must be present and carry the right reason.
		reason := extractMetadataString(metadata, ExpirationMetadataKey+".reason")
		if reason != ExpirationReasonValue {
			t.Errorf("%s: %s.reason=%q, want %q (full metadata: %s)",
				id, ExpirationMetadataKey, reason, ExpirationReasonValue, metadata)
		}
		// Target-time snapshot must equal the original target.
		snapshot := extractMetadataInt(metadata, ExpirationMetadataKey+".target_time")
		if snapshot <= 0 {
			t.Errorf("%s: %s.target_time should be positive, got %d (full metadata: %s)",
				id, ExpirationMetadataKey, snapshot, metadata)
		}
	}
}

// TestSweepDropsRetiredWakesFromOverdueQuery — the integration
// guarantee the task explicitly calls out.
//
// The wakes_overdue doctor counter and the FireStaleFoundationWakes
// path both filter on `WHERE fired = 0 AND target_time < now`.
// After the sweep, retired notification wakes must drop out of
// these queries. The row remains in scheduled_wakes (audit
// history) but no longer counts as actionable.
func TestSweepDropsRetiredWakesFromOverdueQuery(t *testing.T) {
	s := newTestScheduler(t)
	now := time.Now()

	// Seed the same mix the production sees: a recent notification
	// (must remain), a stale notification (must retire), and a
	// system-kind wake (must NEVER be touched).
	seedWake(t, s, "recent-notif", now.Add(-1*time.Hour), "notification")
	seedWake(t, s, "stale-notif", now.Add(-10*24*time.Hour), "notification")
	seedWake(t, s, "stale-system", now.Add(-30*24*time.Hour), "snapshot")

	retired, err := SweepOverdueNotificationWakes(context.Background(), s.db, now)
	if err != nil {
		t.Fatalf("sweep: %v", err)
	}
	if retired != 1 {
		t.Errorf("sweep retired %d, want 1 (only the stale notification)", retired)
	}

	// Overdue-wake count: the recent notification is still overdue
	// (under 7d) AND the stale system wake is overdue. Stale
	// notification is no longer overdue because fired=1. Expect 2.
	var overdueCount int
	if err := s.db.QueryRow(
		`SELECT COUNT(*) FROM scheduled_wakes WHERE fired = 0 AND target_time < ?`,
		now.Unix(),
	).Scan(&overdueCount); err != nil {
		t.Fatalf("overdue count: %v", err)
	}
	if overdueCount != 2 {
		t.Errorf("overdue count = %d, want 2 (recent-notif + stale-system)", overdueCount)
	}

	// The system-kind wake must be untouched regardless of age.
	fired, _, _ := readWakeFired(t, s.db, "stale-system")
	if fired != 0 {
		t.Errorf("stale-system wake was fired=1 — system kinds must not be swept, got fired=%d", fired)
	}
}

// TestSweepIsIdempotent — running the sweep twice is a no-op on
// the second call. The WHERE clause filters out already-retired
// rows, so a double-sweep must retire 0 rows the second time.
func TestSweepIsIdempotent(t *testing.T) {
	s := newTestScheduler(t)
	now := time.Now()

	for _, c := range []struct {
		id   string
		when time.Time
	}{
		{"dup-8d", now.Add(-8 * 24 * time.Hour)},
		{"dup-30d", now.Add(-30 * 24 * time.Hour)},
	} {
		seedWake(t, s, c.id, c.when, "notification")
	}

	// First pass: retire both.
	first, err := SweepOverdueNotificationWakes(context.Background(), s.db, now)
	if err != nil {
		t.Fatalf("first sweep: %v", err)
	}
	if first != 2 {
		t.Fatalf("first sweep retired %d, want 2", first)
	}

	// Second pass: no candidates (fired=1 now), so 0 retirements.
	second, err := SweepOverdueNotificationWakes(context.Background(), s.db, now)
	if err != nil {
		t.Fatalf("second sweep: %v", err)
	}
	if second != 0 {
		t.Errorf("second sweep retired %d, want 0 (idempotent)", second)
	}
}

// TestSweepHandlesUntaggedWakesAsNotifications — the kind
// backward-compat guarantee.
//
// Untagged wakes (no metadata.kind) default to "notification" via
// Wake.Kind(). The sweep must treat them as notifications, so a
// stale untagged wake retires just like a stale explicit-tagged
// notification.
func TestSweepHandlesUntaggedWakesAsNotifications(t *testing.T) {
	s := newTestScheduler(t)
	now := time.Now()

	// Seed an untagged stale wake. seedWake's empty-kind path
	// produces metadata = '{}' — exactly the untagged case.
	seedWake(t, s, "untagged-stale", now.Add(-10*24*time.Hour), "")

	retired, err := SweepOverdueNotificationWakes(context.Background(), s.db, now)
	if err != nil {
		t.Fatalf("sweep: %v", err)
	}
	if retired != 1 {
		t.Errorf("sweep retired %d untagged wakes, want 1 (untagged defaults to notification)", retired)
	}
	fired, _, _ := readWakeFired(t, s.db, "untagged-stale")
	if fired != 1 {
		t.Errorf("untagged-stale fired=%d, want 1", fired)
	}
}

// TestSweepBoundedByLimit — the LIMIT 100 safety net.
//
// Even if the database has thousands of stale notification wakes,
// a single sweep call retires at most NotificationRetentionLimit
// (100). The next tick picks up the remainder. This protects the
// tick loop from stalls when a misconfigured cron generates a
// backlog.
func TestSweepBoundedByLimit(t *testing.T) {
	s := newTestScheduler(t)
	now := time.Now()

	// Seed 150 stale notification wakes.
	const seed = 150
	for i := 0; i < seed; i++ {
		id := "bounded-" + strconv.Itoa(i)
		seedWake(t, s, id, now.Add(-30*24*time.Hour), "notification")
	}

	retired, err := SweepOverdueNotificationWakes(context.Background(), s.db, now)
	if err != nil {
		t.Fatalf("sweep: %v", err)
	}
	if retired != NotificationRetentionLimit {
		t.Errorf("sweep retired %d, want %d (LIMIT %d)", retired, NotificationRetentionLimit, NotificationRetentionLimit)
	}

	// Confirm exactly 100 fired=1, 50 still fired=0.
	var firedCount, pendingCount int
	if err := s.db.QueryRow(
		`SELECT
		   SUM(CASE WHEN fired = 1 THEN 1 ELSE 0 END),
		   SUM(CASE WHEN fired = 0 THEN 1 ELSE 0 END)
		 FROM scheduled_wakes`,
	).Scan(&firedCount, &pendingCount); err != nil {
		t.Fatalf("count: %v", err)
	}
	if firedCount != NotificationRetentionLimit {
		t.Errorf("fired count = %d, want %d", firedCount, NotificationRetentionLimit)
	}
	if pendingCount != seed-NotificationRetentionLimit {
		t.Errorf("pending count = %d, want %d", pendingCount, seed-NotificationRetentionLimit)
	}

	// Second call drains the rest of the backlog.
	retired2, err := SweepOverdueNotificationWakes(context.Background(), s.db, now)
	if err != nil {
		t.Fatalf("second sweep: %v", err)
	}
	if retired2 != seed-NotificationRetentionLimit {
		t.Errorf("second sweep retired %d, want %d (drain remainder)", retired2, seed-NotificationRetentionLimit)
	}
}

// TestSweepRespectsKindBoundary — the safety net for system kinds.
//
// A wake tagged with a system kind (e.g. snapshot, critic_audit)
// must NEVER be retired, even if it's 30 days overdue. The
// opportunistic fold does not apply to system kinds; the scheduler
// dispatches them eagerly. If a system kind ever falls behind by
// 30 days, that's an operational problem the operator needs to
// see — not a notification that can be silently swept.
func TestSweepRespectsKindBoundary(t *testing.T) {
	s := newTestScheduler(t)
	now := time.Now()

	for _, kind := range []string{"snapshot", "critic_audit", "gc", "broadcast"} {
		seedWake(t, s, "system-"+kind, now.Add(-60*24*time.Hour), kind)
	}

	retired, err := SweepOverdueNotificationWakes(context.Background(), s.db, now)
	if err != nil {
		t.Fatalf("sweep: %v", err)
	}
	if retired != 0 {
		t.Errorf("sweep retired %d system-kind wakes, want 0 (system kinds are not notifications)", retired)
	}

	for _, kind := range []string{"snapshot", "critic_audit", "gc", "broadcast"} {
		fired, _, _ := readWakeFired(t, s.db, "system-"+kind)
		if fired != 0 {
			t.Errorf("system-%s fired=%d, want 0 (system kinds must be untouched)", kind, fired)
		}
	}
}

// TestSweepPreservesExistingMetadata — the metadata merge contract.
//
// The sweep must not destroy existing metadata on the wake; it
// only ADDS the expiration keys. A wake that already carries
// agent-defined fields (e.g. a flag the agent set) keeps them
// after the sweep.
func TestSweepPreservesExistingMetadata(t *testing.T) {
	s := newTestScheduler(t)
	now := time.Now()

	// Seed a wake with existing metadata.
	if _, err := s.db.Exec(`
		INSERT INTO scheduled_wakes (id, target_time, reason, theory_id, recurring_rule, fired, created_by, metadata)
		VALUES ('with-metadata', ?, 'test', '', '', 0, 'test',
		        '{"kind":"notification","agent_label":"my-agent","priority":3}')
	`, now.Add(-10*24*time.Hour).Unix()); err != nil {
		t.Fatalf("seed: %v", err)
	}

	if _, err := SweepOverdueNotificationWakes(context.Background(), s.db, now); err != nil {
		t.Fatalf("sweep: %v", err)
	}

	_, _, metadata := readWakeFired(t, s.db, "with-metadata")
	if !strings.Contains(metadata, `"agent_label":"my-agent"`) {
		t.Errorf("existing metadata was destroyed: %s", metadata)
	}
	if !strings.Contains(metadata, `"priority":3`) {
		t.Errorf("existing metadata was destroyed: %s", metadata)
	}
	// Expiration note must be present alongside the existing keys.
	if !strings.Contains(metadata, ExpirationReasonValue) {
		t.Errorf("expiration reason missing from merged metadata: %s", metadata)
	}
}

// extractMetadataInt returns the integer value at the given dotted
// path inside a JSON metadata string, or 0 if the key is missing
// or not a number. Mirror of extractMetadataString for numeric
// fields. Used by the test that asserts the target-time snapshot
// survived the sweep.
func extractMetadataInt(metadataJSON, path string) int64 {
	if metadataJSON == "" || metadataJSON == "{}" {
		return 0
	}
	var m map[string]interface{}
	if err := json.Unmarshal([]byte(metadataJSON), &m); err != nil {
		return 0
	}
	keys := strings.Split(path, ".")
	var cur interface{} = m
	for _, k := range keys {
		obj, ok := cur.(map[string]interface{})
		if !ok {
			return 0
		}
		cur, ok = obj[k]
		if !ok {
			return 0
		}
	}
	switch v := cur.(type) {
	case float64:
		return int64(v)
	case int64:
		return v
	}
	return 0
}
