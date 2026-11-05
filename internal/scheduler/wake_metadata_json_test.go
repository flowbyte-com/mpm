// Regression tests for the malformed-`scheduled_wakes.metadata` defect
// (issue #178, defect W4).
//
// THE DEFECT
// ----------
// SQLite's JSON functions RAISE on malformed JSON:
//
//	json_extract('{not json', '$.kind')  ->  error: malformed JSON
//	json_set('{not json', '$.a', 1)      ->  error: malformed JSON
//
// A scheduled_wakes row whose metadata is malformed therefore poisoned
// every query that read or wrote that column. Three production paths
// read the column's `kind`:
//
//   - the scheduler's notification claim (internal/scheduler/dispatch.go)
//   - the 7-day notification retention sweep (wake_expiration.go)
//   - the 1-hour cron retention sweep (wake_expiration.go)
//
// Each of them used an OR-chain of json_extract() calls to express
// "untagged OR notification". The chain only works if SQLite
// short-circuits the OR before it reaches json_extract — and the
// order of the operands of an OR is NOT specified. The query planner
// may reorder them. So the guard was a guard only by accident.
//
// The consequence: ONE corrupt row could abort the claim and BOTH
// retention sweeps. The notification queue stopped being drained, and
// overdue wakes stopped being retired, for as long as the corrupt row
// survived.
//
// THE FIX
// -------
// The kind is now read through core.WakeNotificationKindFilter, which
// extracts it from an always-valid JSON document:
//
//	COALESCE(json_extract(
//	  CASE WHEN json_valid(metadata) THEN metadata ELSE '{}' END,
//	  '$.kind'), '')
//
// json_valid() never raises, so the CASE always yields a document the
// JSON functions can parse. The expression has no dependence on SQL
// evaluation order whatsoever.
//
// A malformed document reads as UNTAGGED — the same reading a NULL or
// empty metadata already had — so the corrupt row is still processed
// (as a notification) and the healthy rows behind it are unaffected.
//
// THE WRITES
// ----------
// The writes had the same exposure. json_set() raises on malformed
// JSON too, so the claim's `metadata = json_set(...)` and the sweeps'
// `metadata = json_set(...)` could not write their audit trail onto a
// corrupt row — and a sweep that cannot write its audit note fails its
// own read-back assertion. Both now write through
// core.WakeMetadataJSON, which normalises a corrupt document to the
// empty object, exactly as a NULL or empty metadata was already
// normalised.
//
// HOW THESE TESTS ARE WRITTEN
// ---------------------------
// Every test uses a disposable database (the package's own test
// harness) and states the expected behaviour of each metadata shape
// explicitly: malformed, NULL, empty string, untagged, and valid.
// Nothing here depends on timing.

package scheduler

import (
	"context"
	"database/sql"
	"strings"
	"testing"
	"time"

	mpminternal "github.com/flowbyte-com/mpm-core"
)

// seedWakeWithMetadata inserts a wake with an ARBITRARY metadata
// value, including NULL. It exists because the defect is about the
// metadata column's content, so the test must be able to put
// anything in it.
func seedWakeWithMetadata(t *testing.T, s *Scheduler, id string, targetTime time.Time, metadata any) {
	t.Helper()
	_, err := s.db.Exec(
		`INSERT INTO scheduled_wakes (id, target_time, reason, theory_id, recurring_rule, fired, created_by, metadata)
		 VALUES (?, ?, ?, '', '', 0, 'test', ?)`,
		id, targetTime.Unix(), "test-wake "+id, metadata,
	)
	if err != nil {
		t.Fatalf("seed wake %s: %v", id, err)
	}
}

// wakeMetadata reads a wake's metadata back.
func wakeMetadata(t *testing.T, db *sql.DB, id string) string {
	t.Helper()
	var m sql.NullString
	if err := db.QueryRow(`SELECT metadata FROM scheduled_wakes WHERE id = ?`, id).Scan(&m); err != nil {
		t.Fatalf("read metadata %s: %v", id, err)
	}
	if !m.Valid {
		return ""
	}
	return m.String
}

// wakeFiredAt reads a wake's retirement timestamp.
func wakeFiredAt(t *testing.T, db *sql.DB, id string) sql.NullInt64 {
	t.Helper()
	var at sql.NullInt64
	if err := db.QueryRow(`SELECT fired_at FROM scheduled_wakes WHERE id = ?`, id).Scan(&at); err != nil {
		t.Fatalf("read fired_at %s: %v", id, err)
	}
	return at
}

// ── the notification claim ────────────────────────────────────────────────

// TestClaimClaimsHealthyWakeBehindMalformedMetadata is the W4
// regression for the claim.
//
// The corrupt row is the OLDER of the two, so the claim reaches it
// first. Before the fix the claim statement raised "malformed JSON"
// and the drain aborted — the healthy wake behind it was never
// delivered.
//
// After the fix the corrupt row is claimed as an untagged (i.e.
// notification) wake, and the healthy wake behind it is claimed too.
func TestClaimClaimsHealthyWakeBehindMalformedMetadata(t *testing.T) {
	s := newTestScheduler(t)
	now := time.Now()

	// Malformed: the JSON object is never closed.
	seedWakeWithMetadata(t, s, "wk-malformed", now.Add(-2*time.Minute), `{"kind": "notification"`)
	// Healthy, and BEHIND the corrupt row.
	seedWakeWithMetadata(t, s, "wk-healthy", now.Add(-1*time.Minute), `{"kind":"notification"}`)

	claimed, err := dispatchDrainAdHocWakes(context.Background(), s.db, now, 10)
	if err != nil {
		t.Fatalf("a malformed metadata row must not abort the claim: %v", err)
	}
	if claimed != 2 {
		t.Fatalf("claimed %d wakes, want 2: the healthy wake behind the corrupt one must be delivered", claimed)
	}

	// Both rows carry the dispatch audit trail.
	for _, id := range []string{"wk-malformed", "wk-healthy"} {
		var dispatched sql.NullInt64
		if err := s.db.QueryRow(`SELECT dispatched_at FROM scheduled_wakes WHERE id = ?`, id).Scan(&dispatched); err != nil {
			t.Fatalf("read dispatched_at %s: %v", id, err)
		}
		if !dispatched.Valid {
			t.Errorf("%s: never claimed (dispatched_at is NULL)", id)
		}
	}

	// The corrupt row is NORMALISED to the empty object, not left as
	// garbage: the claim writes its audit trail through a document the
	// JSON functions can parse. The healthy row keeps its kind.
	if got := wakeMetadata(t, s.db, "wk-malformed"); !strings.Contains(got, `"dispatched_by":"mpm-scheduler"`) {
		t.Errorf("wk-malformed: metadata=%q, want the dispatch audit trail", got)
	}
	if got := wakeMetadata(t, s.db, "wk-healthy"); !strings.Contains(got, `"kind":"notification"`) {
		t.Errorf("wk-healthy: metadata=%q, want the original kind preserved", got)
	}
}

// TestClaimHandlesEveryMetadataShape walks the claim over every
// metadata shape the column can hold.
//
// The shapes that matter:
//
//   - malformed JSON      -> untagged, still claimable
//   - NULL                -> untagged, still claimable
//   - empty string        -> untagged, still claimable
//   - '{}'                -> untagged, still claimable
//   - valid notification  -> claimable
//   - valid system kind   -> NOT claimable (ownership preserved)
func TestClaimHandlesEveryMetadataShape(t *testing.T) {
	cases := []struct {
		name         string
		metadata     any
		wantClaimed  bool
		wantMetadata string
	}{
		{"malformed JSON", `{"kind": "notification"`, true, ""},
		{"malformed, not an object", `notification`, true, ""},
		{"NULL", nil, true, ""},
		{"empty string", "", true, ""},
		{"empty object", `{}`, true, ""},
		{"valid notification", `{"kind":"notification"}`, true, ""},
		{"valid system kind", `{"kind":"snapshot"}`, false, ""},
		{"valid cascade kind", `{"kind":"cascade"}`, false, ""},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			s := newTestScheduler(t)
			now := time.Now()
			seedWakeWithMetadata(t, s, "wk-shape", now.Add(-time.Minute), c.metadata)

			claimed, err := dispatchDrainAdHocWakes(context.Background(), s.db, now, 10)
			if err != nil {
				t.Fatalf("metadata shape %q must not abort the claim: %v", c.metadata, err)
			}
			if c.wantClaimed && claimed != 1 {
				t.Errorf("claimed %d wakes, want 1 (metadata %q)", claimed, c.metadata)
			}
			if !c.wantClaimed && claimed != 0 {
				t.Errorf("claimed %d wakes, want 0 (metadata %q): wake ownership must be preserved", claimed, c.metadata)
			}
		})
	}
}

// ── the notification retention sweep ──────────────────────────────────────

// TestNotificationSweepRetiresRowsDespiteMalformedMetadata is the W4
// regression for the 7-day sweep.
//
// The corrupt row is older, so the candidate SELECT reaches it first.
// Before the fix the SELECT raised and the sweep returned an error —
// so NOT ONE overdue wake was retired, including the healthy one
// behind the corrupt row.
func TestNotificationSweepRetiresRowsDespiteMalformedMetadata(t *testing.T) {
	s := newTestScheduler(t)
	now := time.Now()
	old := now.Add(-30 * 24 * time.Hour)

	// Corrupt, and OLDER than the healthy row.
	seedWakeWithMetadata(t, s, "wk-sweep-malformed", old.Add(-time.Hour), `{"kind": "notification"`)
	seedWakeWithMetadata(t, s, "wk-sweep-healthy", old, `{"kind":"notification"}`)
	// A system-kind row: the notification sweep must NOT touch it.
	seedWakeWithMetadata(t, s, "wk-sweep-system", old.Add(-2*time.Hour), `{"kind":"snapshot"}`)

	retired, err := SweepOverdueNotificationWakes(context.Background(), s.db, now)
	if err != nil {
		t.Fatalf("a malformed metadata row must not abort the retention sweep: %v", err)
	}
	if retired != 2 {
		t.Fatalf("retired %d wakes, want 2: the healthy row behind the corrupt one must be retired", retired)
	}

	// The corrupt row was retired too — it reads as untagged, i.e. as
	// a notification, and it carries the sweep's audit note.
	meta := wakeMetadata(t, s.db, "wk-sweep-malformed")
	if reason := extractMetadataString(meta, ExpirationMetadataKey+".reason"); reason != ExpirationReasonValue {
		t.Errorf("wk-sweep-malformed: %s.reason=%q, want %q (metadata: %s)",
			ExpirationMetadataKey, reason, ExpirationReasonValue, meta)
	}

	// The system-kind row is untouched.
	var fired int
	if err := s.db.QueryRow(`SELECT fired FROM scheduled_wakes WHERE id = ?`, "wk-sweep-system").Scan(&fired); err != nil {
		t.Fatalf("read fired: %v", err)
	}
	if fired != 0 {
		t.Errorf("wk-sweep-system: fired=%d, want 0: the kind boundary must be preserved", fired)
	}
}

// TestNotificationSweepHandlesEveryMetadataShape walks the sweep over
// every metadata shape.
func TestNotificationSweepHandlesEveryMetadataShape(t *testing.T) {
	cases := []struct {
		name        string
		metadata    any
		wantRetired int
	}{
		{"malformed JSON", `{"kind": "notification"`, 1},
		{"NULL", nil, 1},
		{"empty string", "", 1},
		{"empty object", `{}`, 1},
		{"valid notification", `{"kind":"notification"}`, 1},
		{"valid system kind", `{"kind":"snapshot"}`, 0},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			s := newTestScheduler(t)
			now := time.Now()
			seedWakeWithMetadata(t, s, "wk-shape", now.Add(-30*24*time.Hour), c.metadata)

			retired, err := SweepOverdueNotificationWakes(context.Background(), s.db, now)
			if err != nil {
				t.Fatalf("metadata shape %q must not abort the sweep: %v", c.metadata, err)
			}
			if retired != c.wantRetired {
				t.Errorf("retired %d wakes, want %d (metadata %q)", retired, c.wantRetired, c.metadata)
			}
		})
	}
}

// ── the cron retention sweep ──────────────────────────────────────────────

// TestCronSweepRetiresRowsDespiteMalformedMetadata is the W4
// regression for the 1-hour cron sweep.
//
// The corrupt row is untagged, so it is NOT a cron candidate — the
// cron sweep must leave it to the notification sweep. But it must
// NOT abort the sweep either: the healthy cron row behind it must
// still be retired.
func TestCronSweepRetiresRowsDespiteMalformedMetadata(t *testing.T) {
	s := newTestScheduler(t)
	now := time.Now()
	old := now.Add(-3 * time.Hour)

	seedWakeWithMetadata(t, s, "wk-cron-malformed", old.Add(-time.Hour), `{"kind": "cron"`)
	seedWakeWithMetadata(t, s, "wk-cron-healthy", old, `{"kind":"cron"}`)

	retired, err := SweepOverdueCronWakes(context.Background(), s.db, now, 10)
	if err != nil {
		t.Fatalf("a malformed metadata row must not abort the cron sweep: %v", err)
	}
	if retired != 1 {
		t.Fatalf("retired %d wakes, want 1: only the healthy cron row is a candidate", retired)
	}

	// The corrupt row is untagged, so the cron sweep did NOT retire it
	// and did not stamp its audit note onto it.
	var fired int
	if err := s.db.QueryRow(`SELECT fired FROM scheduled_wakes WHERE id = ?`, "wk-cron-malformed").Scan(&fired); err != nil {
		t.Fatalf("read fired: %v", err)
	}
	if fired != 0 {
		t.Errorf("wk-cron-malformed: fired=%d, want 0: an untagged row is not a cron candidate", fired)
	}

	// The healthy cron row WAS retired, with the cron audit note.
	meta := wakeMetadata(t, s.db, "wk-cron-healthy")
	if reason := extractMetadataString(meta, ExpirationMetadataKey+".reason"); reason != CronRetentionReasonValue {
		t.Errorf("wk-cron-healthy: %s.reason=%q, want %q (metadata: %s)",
			ExpirationMetadataKey, reason, CronRetentionReasonValue, meta)
	}
}

// TestCronBacklogProbeSurvivesMalformedMetadata covers the probe that
// sizes the cron sweep's cap.
//
// The probe counts eligible rows. Before the fix a corrupt row
// aborted it, the probe returned "could not determine", and the
// scheduler fell back to the normal cap — silently losing catch-up
// mode for as long as the corrupt row survived.
func TestCronBacklogProbeSurvivesMalformedMetadata(t *testing.T) {
	s := newTestScheduler(t)
	now := time.Now()
	old := now.Add(-3 * time.Hour)

	seedWakeWithMetadata(t, s, "wk-probe-malformed", old.Add(-time.Hour), `{"kind": "cron"`)
	seedWakeWithMetadata(t, s, "wk-probe-healthy", old, `{"kind":"cron"}`)

	// The cutoff must be LATER than both rows' target_time: the probe
	// counts rows that are already past the cutoff.
	n, err := cronRetentionProbeBacklog(context.Background(), s.db, now.Add(-2*time.Hour).Unix())
	if err != nil {
		t.Fatalf("a malformed metadata row must not abort the backlog probe: %v", err)
	}
	if n != 1 {
		t.Errorf("probe counted %d eligible rows, want 1: only the healthy cron row is eligible", n)
	}
}

// ── the deadline probe ────────────────────────────────────────────────────

// TestComputeEarliestDeadlineSurvivesMalformedMetadata covers the
// query that arms the scheduler's timer.
//
// Before the fix a corrupt row aborted this query; the error was
// logged and swallowed by scheduleNextDeadline, which then fell back
// to the plain interval. The scheduler stopped waking on time for
// notification wakes.
func TestComputeEarliestDeadlineSurvivesMalformedMetadata(t *testing.T) {
	s := newTestScheduler(t)
	now := time.Now()
	target := now.Add(-time.Minute).Truncate(time.Second)

	seedWakeWithMetadata(t, s, "wk-deadline-malformed", target, `{"kind": "notification"`)
	// A healthy wake BEHIND the corrupt one.
	seedWakeWithMetadata(t, s, "wk-deadline-healthy", target.Add(time.Second), `{"kind":"notification"}`)

	earliest, err := s.computeEarliestDeadline(context.Background(), now)
	if err != nil {
		t.Fatalf("a malformed metadata row must not abort the deadline probe: %v", err)
	}
	if earliest.IsZero() {
		t.Fatalf("the deadline probe found nothing: the corrupt row must read as a notification")
	}
	if earliest.Unix() != target.Unix() {
		t.Errorf("earliest deadline = %d, want %d (the corrupt row reads as untagged)", earliest.Unix(), target.Unix())
	}
}

// ── the settlement ────────────────────────────────────────────────────────

// TestSettlementSurvivesMalformedMetadata covers the wake settlement.
//
// json_set() raises on malformed JSON, so the settlement of a corrupt
// row failed. The wake stayed fired=0, its handler ran AGAIN on the
// next tick, and it did so forever.
func TestSettlementSurvivesMalformedMetadata(t *testing.T) {
	s := newTestScheduler(t)
	now := time.Now()
	seedWakeWithMetadata(t, s, "wk-settle-malformed", now.Add(-time.Minute), `{"kind": "snapshot"`)

	if err := mpminternal.MarkWakeFired(s.db, "wk-settle-malformed", ""); err != nil {
		t.Fatalf("a malformed metadata row must not abort the settlement: %v", err)
	}
	var fired int
	if err := s.db.QueryRow(`SELECT fired FROM scheduled_wakes WHERE id = ?`, "wk-settle-malformed").Scan(&fired); err != nil {
		t.Fatalf("read fired: %v", err)
	}
	if fired != 1 {
		t.Errorf("fired=%d, want 1: the corrupt wake must settle", fired)
	}
}
