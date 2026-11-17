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
// THE FIX, IN TWO PARTS
// --------------------
//
// (a) THE GUARD MUST YIELD A WRITABLE OBJECT. json_valid() never
// raises, so the first guard was safe against the RAISE. But it was
// only safe against the raise: it kept every PARSEABLE document,
// including the ones that are not objects ('null', '42', '"str"',
// 'true', '[]'). SQLite's json_set() on a non-object document returns
// the document UNCHANGED, silently dropping the key:
//
//	json_set('null',  '$.k','v') -> null
//	json_set('42',    '$.k','v') -> 42
//	json_set('"str"', '$.k','v') -> "str"
//	json_set('true',  '$.k','v') -> true
//	json_set('[]',    '$.k','v') -> []
//
// The write "landed" (one row affected) and carried NO audit note, so
// the sweep's own read-back assertion failed and the whole batch was
// rolled back. The guard now yields an OBJECT:
//
//	CASE WHEN json_valid(metadata)
//	     THEN CASE WHEN json_type(metadata) = 'object' THEN metadata ELSE '{}' END
//	     ELSE '{}' END
//
// json_type() does raise on malformed input, but it is only reached
// inside the THEN branch of json_valid(metadata), and CASE evaluates
// only the branch it selects. That is a property of CASE itself, not
// of the evaluation order of an expression list — the dependence the
// guard exists to eliminate.
//
// (b) THE ADOPTED CORRUPTION POLICY. A row whose metadata is PRESENT,
// NON-EMPTY and NOT a parseable JSON document is CORRUPT. A corrupt
// row is owned by NO partition:
//
//   - it is never claimed (a truncated '{"kind": "snapshot"' would
//     otherwise be delivered to the agent as an ordinary notification);
//   - it is never folded (the fold retires what it delivers);
//   - it is never retired by a retention sweep (a truncated
//     '{"kind": "cron"' would otherwise be retired with the
//     NOTIFICATION reason — a wrong audit trail);
//   - it is never resolved by an agent (the resolver refuses it with
//     the status "corrupt_metadata");
//   - its ORIGINAL BYTES ARE PRESERVED — they are the only evidence of
//     what the row was.
//
// Every surface that meets one rejects it INDIVIDUALLY, with a
// diagnostic that carries those bytes, and carries on with the healthy
// rows. One corrupt row never blocks, delays or rolls back work on any
// other row.
//
// THE DIAGNOSTIC CHANNELS
// -----------------------
//   - the retention sweeps RETURN the rejections they made — the record
//     carries the wake id, the original bytes and the surface that
//     rejected it;
//   - the tick handlers LOG each rejection, once per distinct row —
//     the notification sweep runs on every tick, so a corrupt row that
//     is never repaired must not produce a log line per minute;
//   - the wakes_corrupt_metadata health counter counts every corrupt
//     row at any time.
//
// HOW THESE TESTS ARE WRITTEN
// ---------------------------
// Every test uses a disposable database (the package's own test
// harness) and states the expected behaviour of each metadata shape
// explicitly: malformed, NULL, empty string, untagged, the valid
// non-object shapes, and the valid kinds. Nothing here depends on
// timing.

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
	"fmt"
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
// After the fix the healthy wake behind the corrupt one IS delivered,
// and the corrupt row is NOT — it is owned by no partition, and its
// bytes are preserved.
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
	if claimed != 1 {
		t.Fatalf("claimed %d wakes, want 1: the healthy wake behind the corrupt one must be delivered", claimed)
	}

	// The healthy row carries the dispatch audit trail — and the
	// stamp LANDED (a write that reports success but drops the key is
	// the failure mode the writable guard exists to remove).
	var dispatched sql.NullInt64
	if err := s.db.QueryRow(`SELECT dispatched_at FROM scheduled_wakes WHERE id = ?`, "wk-healthy").Scan(&dispatched); err != nil {
		t.Fatalf("read dispatched_at wk-healthy: %v", err)
	}
	if !dispatched.Valid {
		t.Errorf("wk-healthy: never claimed (dispatched_at is NULL)")
	}
	if got := wakeMetadata(t, s.db, "wk-healthy"); !strings.Contains(got, `"dispatched_by":"mpm-scheduler"`) {
		t.Errorf("wk-healthy: metadata=%q, want the dispatch audit trail", got)
	}
	if got := wakeMetadata(t, s.db, "wk-healthy"); !strings.Contains(got, `"kind":"notification"`) {
		t.Errorf("wk-healthy: metadata=%q, want the original kind preserved", got)
	}

	// The CORRUPT ROW IS NOT CLAIMED, and its bytes are preserved.
	var corruptDispatched sql.NullInt64
	if err := s.db.QueryRow(`SELECT dispatched_at FROM scheduled_wakes WHERE id = ?`, "wk-malformed").Scan(&corruptDispatched); err != nil {
		t.Fatalf("read dispatched_at wk-malformed: %v", err)
	}
	if corruptDispatched.Valid {
		t.Errorf("wk-malformed: was claimed (dispatched_at=%d): a corrupt row is owned by no partition",
			corruptDispatched.Int64)
	}
	if got := wakeMetadata(t, s.db, "wk-malformed"); got != `{"kind": "notification"` {
		t.Errorf("wk-malformed: metadata=%q, want the original bytes preserved", got)
	}
}

// TestClaimHandlesEveryMetadataShape walks the claim over every
// metadata shape the column can hold.
//
// The shapes that matter:
//
//   - malformed JSON      -> NOT claimable (owned by no partition)
//   - NULL                -> untagged, claimable
//   - empty string        -> untagged, claimable
//   - '{}'                -> untagged, claimable
//   - the valid non-object shapes ('null', '42', '"str"', 'true',
//     '[]') -> untagged, claimable — and the dispatch stamp must
//     LAND on them, which is the regression the writable guard exists
//     for
//   - valid notification  -> claimable
//   - valid system kind   -> NOT claimable (ownership preserved)
func TestClaimHandlesEveryMetadataShape(t *testing.T) {
	cases := []struct {
		name         string
		metadata     any
		wantClaimed  bool
		wantMetadata string
	}{
		{"malformed JSON", `{"kind": "notification"`, false, ""},
		{"malformed, not an object", `notification`, false, ""},
		{"NULL", nil, true, ""},
		{"empty string", "", true, ""},
		{"empty object", `{}`, true, ""},
		{"null literal", `null`, true, ""},
		{"number literal", `42`, true, ""},
		{"string literal", `"str"`, true, ""},
		{"boolean literal", `true`, true, ""},
		{"empty array", `[]`, true, ""},
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

			if !c.wantClaimed {
				return
			}
			// THE AUDIT STAMP MUST LAND. The claim has no read-back
			// of its own — RETURNING confirms the row, not the
			// stamp — so the stamp is asserted here, for every
			// shape. On the old guard the stamp was silently DROPPED
			// for every non-object document.
			got := wakeMetadata(t, s.db, "wk-shape")
			if !strings.Contains(got, `"dispatched_by":"mpm-scheduler"`) {
				t.Errorf("the dispatch stamp did not land (metadata %q -> %q)", c.metadata, got)
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
//
// After the fix the healthy row behind the corrupt one IS retired, and
// the corrupt row is REJECTED — not retired, with its bytes
// preserved and a diagnostic returned.
func TestNotificationSweepRetiresRowsDespiteMalformedMetadata(t *testing.T) {
	s := newTestScheduler(t)
	now := time.Now()
	old := now.Add(-30 * 24 * time.Hour)

	// Corrupt, and OLDER than the healthy row.
	seedWakeWithMetadata(t, s, "wk-sweep-malformed", old.Add(-time.Hour), `{"kind": "notification"`)
	seedWakeWithMetadata(t, s, "wk-sweep-healthy", old, `{"kind":"notification"}`)
	// A system-kind row: the notification sweep must NOT touch it.
	seedWakeWithMetadata(t, s, "wk-sweep-system", old.Add(-2*time.Hour), `{"kind":"snapshot"}`)

	retired, rejected, err := SweepOverdueNotificationWakes(context.Background(), s.db, now)
	if err != nil {
		t.Fatalf("a malformed metadata row must not abort the retention sweep: %v", err)
	}
	if retired != 1 {
		t.Fatalf("retired %d wakes, want 1: the healthy row behind the corrupt one must be retired", retired)
	}

	// THE DIAGNOSTIC. The corrupt row was rejected, and the rejection
	// names the row and carries its original bytes.
	if len(rejected) != 1 {
		t.Fatalf("the sweep reported %d rejection(s), want 1: %+v", len(rejected), rejected)
	}
	r := rejected[0]
	if r.WakeID != "wk-sweep-malformed" {
		t.Errorf("the rejection names %q, want %q", r.WakeID, "wk-sweep-malformed")
	}
	if r.Metadata != `{"kind": "notification"` {
		t.Errorf("the rejection's metadata = %q, want the original bytes", r.Metadata)
	}
	if r.Surface != mpminternal.WakeSurfaceNotificationExpiration {
		t.Errorf("the rejection's surface = %q, want %q", r.Surface, mpminternal.WakeSurfaceNotificationExpiration)
	}

	// The corrupt row was NOT retired and NOT rewritten.
	var fired int
	if err := s.db.QueryRow(`SELECT fired FROM scheduled_wakes WHERE id = ?`, "wk-sweep-malformed").Scan(&fired); err != nil {
		t.Fatalf("read fired: %v", err)
	}
	if fired != 0 {
		t.Errorf("wk-sweep-malformed: fired=%d, want 0: a corrupt row is owned by no partition", fired)
	}
	if got := wakeMetadata(t, s.db, "wk-sweep-malformed"); got != `{"kind": "notification"` {
		t.Errorf("wk-sweep-malformed: metadata=%q, want the original bytes preserved", got)
	}

	// The healthy row WAS retired, with the sweep's audit note.
	meta := wakeMetadata(t, s.db, "wk-sweep-healthy")
	if reason := extractMetadataString(meta, ExpirationMetadataKey+".reason"); reason != ExpirationReasonValue {
		t.Errorf("wk-sweep-healthy: %s.reason=%q, want %q (metadata: %s)",
			ExpirationMetadataKey, reason, ExpirationReasonValue, meta)
	}

	// The system-kind row is untouched.
	if err := s.db.QueryRow(`SELECT fired FROM scheduled_wakes WHERE id = ?`, "wk-sweep-system").Scan(&fired); err != nil {
		t.Fatalf("read fired: %v", err)
	}
	if fired != 0 {
		t.Errorf("wk-sweep-system: fired=%d, want 0: the kind boundary must be preserved", fired)
	}
}

// TestNotificationSweepRejectsACorruptCronRow pins the PARTITION
// SHIFT the policy forbids.
//
// A truncated '{"kind": "cron"' reads as untagged, so the notification
// sweep's candidate filter matches it. Before the policy decision it
// was retired by the NOTIFICATION sweep with the notification reason —
// a wrong audit trail — while the cron sweep, whose filter is
// kind='cron', never touched it. The row's owner was misreported and
// the bytes that named the owner were destroyed.
func TestNotificationSweepRejectsACorruptCronRow(t *testing.T) {
	s := newTestScheduler(t)
	now := time.Now()
	old := now.Add(-30 * 24 * time.Hour)

	// The cron row is CORRUPT and OLDER, so the sweep reaches it
	// first.
	seedWakeWithMetadata(t, s, "wk-was-cron", old.Add(-time.Hour), `{"kind": "cron"`)
	seedWakeWithMetadata(t, s, "wk-notification", old, `{"kind":"notification"}`)

	retired, rejected, err := SweepOverdueNotificationWakes(context.Background(), s.db, now)
	if err != nil {
		t.Fatalf("the sweep must not abort on a corrupt cron row: %v", err)
	}
	if retired != 1 {
		t.Fatalf("retired %d wakes, want 1: only the healthy notification row", retired)
	}
	if len(rejected) != 1 || rejected[0].WakeID != "wk-was-cron" {
		t.Fatalf("the sweep must report the corrupt cron row as rejected, got %+v", rejected)
	}

	// The row is untouched, and its bytes — the evidence that it was a
	// CRON wake — survive.
	var fired int
	var meta string
	if err := s.db.QueryRow(
		`SELECT fired, metadata FROM scheduled_wakes WHERE id = 'wk-was-cron'`).Scan(&fired, &meta); err != nil {
		t.Fatalf("read the corrupt cron row: %v", err)
	}
	if fired != 0 {
		t.Errorf("wk-was-cron: fired=%d, want 0", fired)
	}
	if meta != `{"kind": "cron"` {
		t.Errorf("wk-was-cron: metadata=%q, want the original bytes preserved", meta)
	}
	if reason := extractMetadataString(meta, ExpirationMetadataKey+".reason"); reason != "" {
		t.Errorf("wk-was-cron: the sweep stamped its audit note on a corrupt row (reason %q)", reason)
	}

	// And the CRON sweep does not touch it either — a corrupt row
	// reads as untagged, so it is not a cron candidate.
	cronRetired, cronRejected, err := SweepOverdueCronWakes(context.Background(), s.db, now, 10)
	if err != nil {
		t.Fatalf("the cron sweep must not abort on the corrupt row: %v", err)
	}
	if cronRetired != 0 {
		t.Errorf("the cron sweep retired %d row(s), want 0: a corrupt row is not a cron candidate", cronRetired)
	}
	if len(cronRejected) != 0 {
		t.Errorf("the cron sweep reported %d rejection(s), want 0: it never meets a corrupt row", len(cronRejected))
	}
}

// TestNotificationSweepHandlesEveryMetadataShape walks the sweep over
// every metadata shape.
//
// The valid non-object shapes are here because they are the ones that
// broke the OLD guard: json_set() returned them unchanged, the write
// "landed" without an audit note, and the sweep's own read-back
// assertion then failed — aborting the whole batch.
func TestNotificationSweepHandlesEveryMetadataShape(t *testing.T) {
	cases := []struct {
		name         string
		metadata     any
		wantRetired  int
		wantRejected int
	}{
		{"malformed JSON", `{"kind": "notification"`, 0, 1},
		{"NULL", nil, 1, 0},
		{"empty string", "", 1, 0},
		{"empty object", `{}`, 1, 0},
		{"null literal", `null`, 1, 0},
		{"number literal", `42`, 1, 0},
		{"string literal", `"str"`, 1, 0},
		{"boolean literal", `true`, 1, 0},
		{"empty array", `[]`, 1, 0},
		{"valid notification", `{"kind":"notification"}`, 1, 0},
		{"valid system kind", `{"kind":"snapshot"}`, 0, 0},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			s := newTestScheduler(t)
			now := time.Now()
			seedWakeWithMetadata(t, s, "wk-shape", now.Add(-30*24*time.Hour), c.metadata)

			retired, rejected, err := SweepOverdueNotificationWakes(context.Background(), s.db, now)
			if err != nil {
				t.Fatalf("metadata shape %q must not abort the sweep: %v", c.metadata, err)
			}
			if retired != c.wantRetired {
				t.Errorf("retired %d wakes, want %d (metadata %q)", retired, c.wantRetired, c.metadata)
			}
			if len(rejected) != c.wantRejected {
				t.Errorf("reported %d rejection(s), want %d (metadata %q)", len(rejected), c.wantRejected, c.metadata)
			}
			if c.wantRetired == 0 {
				return
			}
			// THE AUDIT NOTE MUST LAND. On the old guard the note
			// was silently dropped for every non-object document, and
			// the sweep's read-back assertion then failed.
			meta := wakeMetadata(t, s.db, "wk-shape")
			if reason := extractMetadataString(meta, ExpirationMetadataKey+".reason"); reason != ExpirationReasonValue {
				t.Errorf("the audit note did not land (metadata %q -> %q)", c.metadata, meta)
			}
		})
	}
}

// TestNotificationSweepReportsNoRejectionWithoutCorruption is the
// NEGATIVE CONTROL for the rejection channel.
//
// A sweep that always returned a rejection would satisfy every other
// test in this file. With no corrupt row in the table, the rejection
// list must be EMPTY.
func TestNotificationSweepReportsNoRejectionWithoutCorruption(t *testing.T) {
	s := newTestScheduler(t)
	now := time.Now()
	old := now.Add(-30 * 24 * time.Hour)

	seedWakeWithMetadata(t, s, "wk-healthy-1", old, `{"kind":"notification"}`)
	seedWakeWithMetadata(t, s, "wk-healthy-2", old.Add(-time.Minute), `{"kind":"notification"}`)

	retired, rejected, err := SweepOverdueNotificationWakes(context.Background(), s.db, now)
	if err != nil {
		t.Fatalf("the sweep failed: %v", err)
	}
	if retired != 2 {
		t.Errorf("retired %d wakes, want 2", retired)
	}
	if len(rejected) != 0 {
		t.Errorf("the sweep reported %d rejection(s) with no corrupt row present: %+v", len(rejected), rejected)
	}
}

// TestNotificationSweepCorruptRowsDoNotStarveTheHealthyOnes pins the
// CAPACITY property.
//
// The sweep retires at most NotificationRetentionLimit rows per call.
// A corrupt row is never retired, so if the corrupt rows occupied the
// candidate slots, the healthy rows behind them would never be
// retired ”' + "\u2014" + ”' the same "one bad row stops the sweep" class, one level
// up. The corrupt rows are fetched by their own bounded query, so they
// never consume the retirement capacity.
func TestNotificationSweepCorruptRowsDoNotStarveTheHealthyOnes(t *testing.T) {
	s := newTestScheduler(t)
	now := time.Now()
	old := now.Add(-30 * 24 * time.Hour)

	// MORE corrupt rows than the sweep's per-call limit, all OLDER
	// than the healthy row.
	for i := 0; i < NotificationRetentionLimit+5; i++ {
		seedWakeWithMetadata(t, s, fmt.Sprintf("wk-corrupt-%03d", i),
			old.Add(-time.Duration(i+1)*time.Second), `{"kind": "cron"`)
	}
	seedWakeWithMetadata(t, s, "wk-healthy", old, `{"kind":"notification"}`)

	retired, rejected, err := SweepOverdueNotificationWakes(context.Background(), s.db, now)
	if err != nil {
		t.Fatalf("the sweep failed: %v", err)
	}
	if retired != 1 {
		t.Fatalf("retired %d wakes, want 1: the corrupt rows must not starve the healthy one", retired)
	}
	if len(rejected) == 0 {
		t.Errorf("the sweep reported no rejection: the corrupt rows must still be reported")
	}
	var fired int
	if err := s.db.QueryRow(`SELECT fired FROM scheduled_wakes WHERE id = 'wk-healthy'`).Scan(&fired); err != nil {
		t.Fatalf("read fired: %v", err)
	}
	if fired != 1 {
		t.Errorf("wk-healthy: fired=%d, want 1", fired)
	}
}

// BATCH AMPLIFICATION class.
//
// The sweep runs one transaction for the whole batch, so a per-row
// failure used to abort the batch and undo every retirement it had
// already made. The row-level work now runs inside a SAVEPOINT, so a
// failure on ONE row rolls that row back and leaves the rest.
//
// The failure is INDEPENDENT: a trigger raises on exactly one row, so
// the sweep's own statements are untouched and the test measures the
// isolation, not a mock.
func TestNotificationSweepIsolatesOneRowFailureFromTheRest(t *testing.T) {
	s := newTestScheduler(t)
	now := time.Now()
	old := now.Add(-30 * 24 * time.Hour)

	// The failing row is OLDER, so the sweep reaches it FIRST.
	seedWakeWithMetadata(t, s, "wk-boom", old.Add(-time.Hour), `{"kind":"notification"}`)
	seedWakeWithMetadata(t, s, "wk-healthy", old, `{"kind":"notification"}`)

	_, err := s.db.Exec(`
		CREATE TRIGGER wake_sweep_boom BEFORE UPDATE ON scheduled_wakes
		WHEN NEW.id = 'wk-boom'
		BEGIN
			SELECT RAISE(ABORT, 'injected per-row failure');
		END`)
	if err != nil {
		t.Fatalf("install the trigger: %v", err)
	}

	retired, rejected, sweepErr := SweepOverdueNotificationWakes(context.Background(), s.db, now)
	if sweepErr == nil {
		t.Fatalf("the sweep must report the per-row failure")
	}
	if len(rejected) != 0 {
		t.Errorf("the sweep reported %d corruption rejection(s), want 0: the failure was not a corruption", len(rejected))
	}
	// THE OTHER ROW'S WORK SURVIVED.
	if retired != 1 {
		t.Errorf("retired %d wakes, want 1: the healthy row's retirement must not be rolled back by the other row's failure", retired)
	}
	var fired int
	if err := s.db.QueryRow(`SELECT fired FROM scheduled_wakes WHERE id = 'wk-healthy'`).Scan(&fired); err != nil {
		t.Fatalf("read fired: %v", err)
	}
	if fired != 1 {
		t.Errorf("wk-healthy: fired=%d, want 1: the successful work was rolled back", fired)
	}
	// And the failing row was NOT retired.
	if err := s.db.QueryRow(`SELECT fired FROM scheduled_wakes WHERE id = 'wk-boom'`).Scan(&fired); err != nil {
		t.Fatalf("read fired: %v", err)
	}
	if fired != 0 {
		t.Errorf("wk-boom: fired=%d, want 0: the failed row must not be retired", fired)
	}
}

// ── the cron retention sweep ──────────────────────────────────────────────

// TestCronSweepRetiresRowsDespiteMalformedMetadata is the W4
// regression for the 1-hour cron sweep.
//
// The corrupt row is untagged, so it is NOT a cron candidate — the
// cron sweep must leave it to the notification sweep. But it must NOT
// abort the sweep either: the healthy cron row behind it must still be
// retired.
func TestCronSweepRetiresRowsDespiteMalformedMetadata(t *testing.T) {
	s := newTestScheduler(t)
	now := time.Now()
	old := now.Add(-3 * time.Hour)

	seedWakeWithMetadata(t, s, "wk-cron-malformed", old.Add(-time.Hour), `{"kind": "cron"`)
	seedWakeWithMetadata(t, s, "wk-cron-healthy", old, `{"kind":"cron"}`)

	retired, rejected, err := SweepOverdueCronWakes(context.Background(), s.db, now, 10)
	if err != nil {
		t.Fatalf("a malformed metadata row must not abort the cron sweep: %v", err)
	}
	if retired != 1 {
		t.Fatalf("retired %d wakes, want 1: only the healthy cron row is a candidate", retired)
	}
	if len(rejected) != 0 {
		t.Errorf("the cron sweep reported %d rejection(s), want 0: a corrupt row is not a cron candidate", len(rejected))
	}

	// The corrupt row is untagged, so the cron sweep did NOT retire it,
	// did NOT stamp its audit note onto it, and left its bytes alone.
	var fired int
	if err := s.db.QueryRow(`SELECT fired FROM scheduled_wakes WHERE id = ?`, "wk-cron-malformed").Scan(&fired); err != nil {
		t.Fatalf("read fired: %v", err)
	}
	if fired != 0 {
		t.Errorf("wk-cron-malformed: fired=%d, want 0: an untagged row is not a cron candidate", fired)
	}
	if got := wakeMetadata(t, s.db, "wk-cron-malformed"); got != `{"kind": "cron"` {
		t.Errorf("wk-cron-malformed: metadata=%q, want the original bytes preserved", got)
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
		t.Fatalf("the deadline probe found nothing: the healthy wake behind the corrupt one must be found")
	}
	// THE CORRUPT ROW DOES NOT ARM THE TIMER. The probe must match the
	// claim's WHERE clause exactly, and the claim does not take a
	// corrupt row. A corrupt row whose target_time is in the past would
	// arm the timer at zero delay while the dispatcher drains nothing
	// — the CPU wedge this filter exists to prevent.
	if earliest.Unix() != target.Add(time.Second).Unix() {
		t.Errorf("earliest deadline = %d, want %d (the corrupt row must not arm the timer)",
			earliest.Unix(), target.Add(time.Second).Unix())
	}
}

// TestComputeEarliestDeadlineIgnoresACorruptRowAlone is the wedge
// guard.
//
// With ONLY a corrupt row pending, the probe must report NO deadline.
// A past timestamp would be clamped to a zero delay, the dispatcher
// would drain nothing, and the cycle would repeat at zero delay — the
// 100% CPU wedge the notification filter exists to prevent.
func TestComputeEarliestDeadlineIgnoresACorruptRowAlone(t *testing.T) {
	s := newTestScheduler(t)
	now := time.Now()

	seedWakeWithMetadata(t, s, "wk-deadline-corrupt", now.Add(-time.Hour), `{"kind": "notification"`)

	earliest, err := s.computeEarliestDeadline(context.Background(), now)
	if err != nil {
		t.Fatalf("a malformed metadata row must not abort the deadline probe: %v", err)
	}
	if !earliest.IsZero() {
		t.Errorf("earliest deadline = %v, want the zero time: a corrupt row must not arm the timer", earliest)
	}
}

// ── the settlement ────────────────────────────────────────────────────────

// TestSettlementSurvivesMalformedMetadata covers the wake settlement.
//
// The settlement writes metadata through json_set(), which raises on
// malformed JSON — so the settlement of a corrupt row failed. The
// wake stayed fired=0.
//
// NOTE ON THE RATIONALE. The independent review established that this
// path was NOT reachable in production — the scheduler only settles a
// wake whose GO-SIDE kind has a registered handler, and a wake whose
// metadata Go cannot parse reads as "notification", which has no
// handler. The change is therefore DEFENSIVE HARDENING, not the fix of
// a live defect.
//
// What the test pins is the contract: a corrupt row SETTLES (it does
// not stay pending forever), and its original bytes are PRESERVED —
// they are the only evidence of what the row was.
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
	if got := wakeMetadata(t, s.db, "wk-settle-malformed"); got != `{"kind": "snapshot"` {
		t.Errorf("metadata=%q, want the original bytes preserved", got)
	}
}

// TestSettlementWritesTheErrorNoteOnAHealthyRow pins the other half
// of the settlement's contract: the last_error stamp must LAND.
//
// A write that reports success but drops the key is the failure mode
// the writable guard exists to remove, and this path had no read-back
// of its own.
func TestSettlementWritesTheErrorNoteOnAHealthyRow(t *testing.T) {
	now := time.Now()

	cases := []struct {
		name     string
		metadata any
	}{
		{"object", `{"kind":"snapshot"}`},
		{"null literal", `null`},
		{"number literal", `42`},
		{"string literal", `"str"`},
		{"boolean literal", `true`},
		{"empty array", `[]`},
		{"empty object", `{}`},
		{"NULL", nil},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			s := newTestScheduler(t)
			seedWakeWithMetadata(t, s, "wk-settle-shape", now.Add(-time.Minute), c.metadata)
			if err := mpminternal.MarkWakeFired(s.db, "wk-settle-shape", "boom"); err != nil {
				t.Fatalf("the settlement failed: %v", err)
			}
			got := wakeMetadata(t, s.db, "wk-settle-shape")
			if !strings.Contains(got, `"last_error":"boom"`) {
				t.Errorf("the error note did not land (metadata %q -> %q)", c.metadata, got)
			}
		})
	}
}

// TestClaimDoesNotDeliverATruncatedSnapshotRow pins the second half of
// the PARTITION SHIFT.
//
// A truncated '{"kind": "snapshot"' reads as untagged, so the
// notification filter matches it. Before the policy decision it was
// claimed and DELIVERED to the agent as an ordinary notification: its
// snapshot handler never ran, and never could.
func TestClaimDoesNotDeliverATruncatedSnapshotRow(t *testing.T) {
	s := newTestScheduler(t)
	now := time.Now()

	// The corrupt row is OLDER, so the claim reaches it FIRST.
	seedWakeWithMetadata(t, s, "wk-was-snapshot", now.Add(-2*time.Minute), `{"kind": "snapshot"`)
	seedWakeWithMetadata(t, s, "wk-notification", now.Add(-time.Minute), `{"kind":"notification"}`)

	claimed, err := dispatchDrainAdHocWakes(context.Background(), s.db, now, 10)
	if err != nil {
		t.Fatalf("a corrupt metadata row must not abort the claim: %v", err)
	}
	if claimed != 1 {
		t.Fatalf("claimed %d wakes, want 1: the truncated snapshot row must not be delivered", claimed)
	}

	var dispatched sql.NullInt64
	if err := s.db.QueryRow(`SELECT dispatched_at FROM scheduled_wakes WHERE id = 'wk-was-snapshot'`).Scan(&dispatched); err != nil {
		t.Fatalf("read dispatched_at: %v", err)
	}
	if dispatched.Valid {
		t.Errorf("the truncated snapshot row was claimed (dispatched_at=%d)", dispatched.Int64)
	}
	if got := wakeMetadata(t, s.db, "wk-was-snapshot"); got != `{"kind": "snapshot"` {
		t.Errorf("metadata=%q, want the original bytes preserved", got)
	}
}
