// wake_cron_retention_interaction_test.go — the cron wake's interaction
// with the retention sweep.
//
// WHY THIS FILE EXISTS.
//
// The cron wake is agent-owned: the opportunistic fold delivers it (see
// internal/core/wake_cron_delivery_test.go). The scheduler's cron
// retention sweep also acts on cron rows. Two surfaces acting on one
// row is exactly where a wake can be consumed before it is delivered,
// so the interaction is pinned here.
//
// THE CONTRACT PINNED.
//
//   1. CronRetentionWindow is the cron wake's DELIVERY DEADLINE. Inside
//      it, the fold's delivery opportunity is open and the sweep must
//      not retire the row.
//   2. The sweep's retirement is a compare-and-set on fired=0. A row
//      the fold has already settled is SKIPPED, not overwritten — so a
//      wake cannot be consumed twice and a delivery cannot be undone.
//   3. The sweep never DELIVERS. It retires an undelivered row and
//      writes an audit note, so the retirement is visible to an
//      operator rather than silent.
//   4. The scheduler's ad-hoc claim never claims a cron row, so the
//      scheduler and the fold never race for the same row.
//
// Every test uses a disposable temp-file database and never touches
// the operator's.
package scheduler

import (
	"context"
	"database/sql"
	"encoding/json"
	"testing"
	"time"

	core "github.com/flowbyte-com/mpm-core"
)

// cronFoldDM returns a core DatabaseManager over the scheduler's own
// database, so the fold and the sweep act on the SAME rows — which is
// the interaction under test.
func cronFoldDM(t *testing.T, s *Scheduler) *core.DatabaseManager {
	t.Helper()
	dm := core.NewDatabaseManagerForDB(s.db)
	t.Cleanup(func() { _ = s.db.Close() })
	return dm
}

// cronWakeFired reads the fired flag of a wake row.
func cronWakeFired(t *testing.T, s *Scheduler, id string) int {
	t.Helper()
	var fired int
	if err := s.db.QueryRow(`SELECT fired FROM scheduled_wakes WHERE id = ?`, id).Scan(&fired); err != nil {
		t.Fatalf("read fired for %s: %v", id, err)
	}
	return fired
}

// cronWakeMetadataJSON reads a wake row's metadata.
func cronWakeMetadataJSON(t *testing.T, s *Scheduler, id string) string {
	t.Helper()
	var raw string
	if err := s.db.QueryRow(`SELECT metadata FROM scheduled_wakes WHERE id = ?`, id).Scan(&raw); err != nil {
		t.Fatalf("read metadata for %s: %v", id, err)
	}
	return raw
}

// TestCronRetention_FoldDeliversACronWakeInsideTheDeliveryWindow is
// the end-to-end interaction: the fold delivers the cron wake, and the
// sweep then leaves the delivered row alone.
//
// The sweep is invoked with a `now` far beyond the window so the row
// WOULD be a candidate on age alone. It must still be skipped,
// because the fold already settled it.
func TestCronRetention_FoldDeliversACronWakeInsideTheDeliveryWindow(t *testing.T) {
	s := newTestScheduler(t)
	dm := cronFoldDM(t, s)
	now := time.Now()

	id := seedCronWake(t, s, now.Add(-time.Minute), "epistemic-compaction")

	// The fold delivers the cron wake.
	due, err := dm.CheckPendingWakes(now, nil)
	if err != nil {
		t.Fatalf("fold: %v", err)
	}
	if len(due) != 1 || due[0]["id"] != id {
		t.Fatalf("fold delivered %v, want exactly the cron wake %s", due, id)
	}
	if got := cronWakeFired(t, s, id); got != 1 {
		t.Fatalf("fired = %d after the fold, want 1", got)
	}

	// The sweep, run at a time when the row is long past the window,
	// must skip it: the compare-and-set sees fired=1.
	retired, rejected, err := SweepOverdueCronWakes(context.Background(), s.db, now.Add(2*time.Hour), 100)
	if err != nil {
		t.Fatalf("sweep: %v", err)
	}
	if retired != 0 {
		t.Errorf("sweep retired %d row(s), want 0: a row the fold already delivered must not be retired again", retired)
	}
	if len(rejected) != 0 {
		t.Errorf("sweep rejected %v, want no rejections", rejected)
	}
	if got := cronWakeFired(t, s, id); got != 1 {
		t.Errorf("fired = %d after the sweep, want 1 (unchanged)", got)
	}
}

// TestCronRetention_SweepLeavesAnUndeliveredCronWakeInsideTheDeliveryWindow
// pins the delivery DEADLINE.
//
// An undelivered cron wake that is still inside the window keeps its
// delivery opportunity: the sweep must not retire it. This is the
// "retention cannot silently consume a cron wake before its intended
// delivery" guarantee.
func TestCronRetention_SweepLeavesAnUndeliveredCronWakeInsideTheDeliveryWindow(t *testing.T) {
	s := newTestScheduler(t)
	now := time.Now()

	// One minute old: well inside the one-hour window.
	id := seedCronWake(t, s, now.Add(-time.Minute), "epistemic-compaction")

	retired, _, err := SweepOverdueCronWakes(context.Background(), s.db, now, 100)
	if err != nil {
		t.Fatalf("sweep: %v", err)
	}
	if retired != 0 {
		t.Errorf("sweep retired %d row(s), want 0: a cron wake inside the delivery window must keep its delivery opportunity", retired)
	}
	if got := cronWakeFired(t, s, id); got != 0 {
		t.Errorf("fired = %d, want 0: the sweep consumed a cron wake before its delivery window closed", got)
	}
}

// TestCronRetention_SweepCannotOverwriteAFoldSettlement is the
// compare-and-set, pinned directly.
//
// The candidate SELECT runs outside the retirement transaction, so a
// row can be settled by the fold between the SELECT and the UPDATE.
// The UPDATE carries `AND fired = 0`, so the sweep skips it.
func TestCronRetention_SweepCannotOverwriteAFoldSettlement(t *testing.T) {
	s := newTestScheduler(t)
	dm := cronFoldDM(t, s)
	now := time.Now()

	// Old enough to be a sweep candidate on age.
	id := seedCronWake(t, s, now.Add(-2*CronRetentionWindow), "epistemic-compaction")

	// The fold settles it first — this is the settlement that races the
	// sweep's UPDATE.
	due, err := dm.CheckPendingWakes(now, nil)
	if err != nil {
		t.Fatalf("fold: %v", err)
	}
	if len(due) != 1 {
		t.Fatalf("fold delivered %d wakes, want 1", len(due))
	}
	firedAtBefore := cronWakeFired(t, s, id)
	metadataBefore := cronWakeMetadataJSON(t, s, id)

	retired, _, err := SweepOverdueCronWakes(context.Background(), s.db, now, 100)
	if err != nil {
		t.Fatalf("sweep: %v", err)
	}
	if retired != 0 {
		t.Errorf("sweep retired %d row(s), want 0: the compare-and-set must skip a settled row", retired)
	}
	if got := cronWakeFired(t, s, id); got != firedAtBefore {
		t.Errorf("fired = %d, want %d (the settlement must not be overwritten)", got, firedAtBefore)
	}
	if got := cronWakeMetadataJSON(t, s, id); got != metadataBefore {
		t.Errorf("metadata = %s, want %s (the sweep must not rewrite a settled row)", got, metadataBefore)
	}
}

// TestCronRetention_RetirementIsAuditable pins that a sweep is never
// SILENT.
//
// The sweep retires an undelivered row. It does not deliver it — the
// agent never receives it — so the operator must be able to see that
// it was swept rather than delivered. The audit note is the record.
func TestCronRetention_RetirementIsAuditable(t *testing.T) {
	s := newTestScheduler(t)
	now := time.Now()

	id := seedCronWake(t, s, now.Add(-2*CronRetentionWindow), "epistemic-compaction")

	retired, _, err := SweepOverdueCronWakes(context.Background(), s.db, now, 100)
	if err != nil {
		t.Fatalf("sweep: %v", err)
	}
	if retired != 1 {
		t.Fatalf("sweep retired %d row(s), want 1: an undelivered cron wake past the window must be retired", retired)
	}
	if got := cronWakeFired(t, s, id); got != 1 {
		t.Errorf("fired = %d, want 1", got)
	}

	var meta map[string]interface{}
	if err := json.Unmarshal([]byte(cronWakeMetadataJSON(t, s, id)), &meta); err != nil {
		t.Fatalf("read back metadata: %v", err)
	}
	expired, ok := meta["expired"].(map[string]interface{})
	if !ok {
		t.Fatalf("metadata must carry the expired audit note, got %v", meta)
	}
	if expired["reason"] != CronRetentionReasonValue {
		t.Errorf("expired.reason = %v, want %q", expired["reason"], CronRetentionReasonValue)
	}
	// The kind and the directive_id survive the sweep: the row is
	// retired, not rewritten into something else.
	if meta["kind"] != "cron" {
		t.Errorf("kind = %v, want cron: the sweep must not change the row's kind", meta["kind"])
	}
	if meta["directive_id"] == nil || meta["directive_id"] == "" {
		t.Errorf("directive_id must survive the sweep, got %v", meta["directive_id"])
	}
}

// TestDispatchClaim_DoesNotClaimACronWake pins that the scheduler and
// the fold never race for the same row.
//
// The scheduler's ad-hoc claim excludes cron rows by construction
// (core.WakeNotificationKindFilter). If it did not, the claim would
// stamp dispatched_at on a row the fold is about to deliver, and the
// two surfaces would contend for one wake.
func TestDispatchClaim_DoesNotClaimACronWake(t *testing.T) {
	s := newDispatchTestScheduler(t)
	now := time.Now()

	if _, err := s.db.Exec(
		`INSERT INTO scheduled_wakes (id, target_time, reason, fired, created_by, metadata)
		 VALUES ('cron-claim-probe', ?, 'cron:epistemic-compaction', 0, 'mpm-scheduler', ?)`,
		now.Add(-time.Minute).Unix(),
		`{"kind":"cron","source":"cron","task_id":"epistemic-compaction","directive_id":"mpm-seed-epistemic-compaction"}`,
	); err != nil {
		t.Fatalf("insert: %v", err)
	}

	claimed, ok, err := dispatchClaimNextAdHocWake(context.Background(), s.db, now)
	if err != nil {
		t.Fatalf("claim: %v", err)
	}
	if ok {
		t.Fatalf("the scheduler claimed %q: a cron wake belongs to the fold, not to the scheduler", claimed.ID)
	}

	var dispatchedAt sql.NullInt64
	if err := s.db.QueryRow(`SELECT dispatched_at FROM scheduled_wakes WHERE id = 'cron-claim-probe'`).Scan(&dispatchedAt); err != nil {
		t.Fatalf("read dispatched_at: %v", err)
	}
	if dispatchedAt.Valid {
		t.Errorf("dispatched_at = %v, want NULL: the scheduler must not stamp a cron wake", dispatchedAt.Int64)
	}
}
