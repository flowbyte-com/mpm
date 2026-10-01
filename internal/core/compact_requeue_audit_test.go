// compact_requeue_audit_test.go — the R5 audit guarantee under failure.
//
// The audit row is the only durable evidence that an operator overrode
// a model refusal. R5 promises it for *every* requeue, so the contract
// has to be pinned under the condition where the audit insert is the
// thing that breaks — otherwise the guarantee is only ever observed on
// the happy path, where it cannot fail.
//
// The injection is a BEFORE INSERT trigger that aborts, which makes the
// audit insert fail deterministically without touching the requeue's own
// UPDATE. That separation is the whole point: it is the only way to ask
// what happens when the trail is the part that fails.

package internal

import (
	"context"
	"fmt"
	"strconv"
	"strings"
	"testing"
	"time"
)

// breakAuditInsert makes every subsequent system_audit_log insert fail,
// leaving the requeue path itself untouched.
func breakAuditInsert(t *testing.T, dm *DatabaseManager) {
	t.Helper()
	if _, err := dm.SQLDB().Exec(`
		CREATE TRIGGER inject_audit_failure
		BEFORE INSERT ON system_audit_log
		BEGIN
			SELECT RAISE(ABORT, 'injected audit failure');
		END`); err != nil {
		t.Fatalf("install audit failure trigger: %v", err)
	}
}

// R5: a requeue that cannot be audited must not commit.
//
// The contract is that the trail is guaranteed, not best-effort. This
// is the same contract PurgeWork holds: its audit row is written inside
// the purge transaction (internal/core/work_purge.go), so an audit
// failure rolls the purge back rather than leaving a deletion with no
// record. Requeue is the smaller-risk version of the same operation, so
// it gets the same guarantee.
//
// Under the old best-effort-after-commit contract this test failed with
// "3 row(s) were requeued with no audit row and no error" — the exact
// unaudited override R5 exists to prevent.
func TestRequeue_AuditFailureRollsBackTheMutation(t *testing.T) {
	dm := NewTestDM(t)
	ids := seedMixed(t, dm, 0, 3, 0)
	breakAuditInsert(t, dm)

	res, err := dm.RequeueDeferred(context.Background(), 10)

	if err == nil {
		t.Fatalf("requeue succeeded despite the audit insert failing: %+v", res)
	}
	if res != nil {
		t.Errorf("result = %+v, want nil alongside the error — a failed requeue must not return a partial success", res)
	}
	if !strings.Contains(err.Error(), "audit") {
		t.Errorf("error = %v, want it to name the audit failure rather than report an opaque rollback", err)
	}

	// The rollback is the actual guarantee: no row left requeued, so
	// there is no override without a record of it.
	if got := readPressure(t, dm); got.ActionablePending != 0 || got.DeferredCount != 3 {
		t.Errorf("pressure = %+v, want actionable 0 / deferred 3 — the mutation committed without its audit row", got)
	}
	for _, id := range ids {
		if m := deferralMetadata(t, dm, id); m.At == "" {
			t.Errorf("row %s lost its deferral despite the rolled-back audit", id)
		}
	}

	// The trigger must actually have fired, or this proved nothing.
	var n int
	if err := dm.SQLDB().QueryRow(
		`SELECT COUNT(*) FROM system_audit_log WHERE component = 'compact'`).Scan(&n); err != nil {
		t.Fatalf("count audit rows: %v", err)
	}
	if n != 0 {
		t.Errorf("audit rows = %d, want 0", n)
	}
}

// The success path is unchanged: an audit row lands, and it is committed
// with the mutation rather than after it.
func TestRequeue_AuditRowCommittedWithTheMutation(t *testing.T) {
	dm := NewTestDM(t)
	seedMixed(t, dm, 0, 2, 0)

	res, err := dm.RequeueDeferred(context.Background(), 10)
	if err != nil {
		t.Fatalf("requeue: %v", err)
	}
	if res.AuditID == "" {
		t.Fatal("AuditID is empty on a successful requeue — R5 guarantees a trail")
	}

	var level, component string
	if err := dm.SQLDB().QueryRow(
		`SELECT level, component FROM system_audit_log WHERE id = ?`, res.AuditID).
		Scan(&level, &component); err != nil {
		t.Fatalf("read audit row: %v", err)
	}
	if level != "info" || component != "compact" {
		t.Errorf("audit row = level %q / component %q, want info / compact", level, component)
	}
}

// A requeue that finds nothing deferred writes no audit row, so it must
// also survive an audit failure silently — there is no decision to
// record. This pins the boundary of the guarantee: it covers requeues
// that mutate, not calls that are no-ops.
func TestRequeue_AuditFailureOnNoOpIsHarmless(t *testing.T) {
	dm := NewTestDM(t)
	breakAuditInsert(t, dm)

	res, err := dm.RequeueDeferred(context.Background(), 10)
	if err != nil {
		t.Fatalf("a no-op requeue must not error when the audit table is broken: %v", err)
	}
	if res == nil {
		t.Fatal("no-op requeue returned a nil result")
	}
	if res.Requeued != 0 || len(res.RowIDs) != 0 {
		t.Errorf("requeued %d row(s) from an empty backlog, want 0", res.Requeued)
	}
}

// ── updated_at ───────────────────────────────────────────────────────

// R3's exception. Requeue stamps updated_at, because the row *was*
// modified and a stale timestamp would make a requeued row look
// untouched to anything reading recency. The stamp must be a real Unix
// epoch integer — this column is INTEGER — and it must be no earlier
// than the moment the requeue ran.
func TestRequeue_StampsUpdatedAtAsUnixEpoch(t *testing.T) {
	dm := NewTestDM(t)
	ids := seedMixed(t, dm, 1, 0, 0)
	target := ids[0]

	// Read the value as a raw driver.Value so the storage class is
	// visible. The seed helper writes RFC3339 text; the column is
	// declared INTEGER and requeue must write an epoch integer. That
	// transition is the contract, so the test has to be able to see
	// both sides of it.
	var beforeRaw interface{}
	if err := dm.SQLDB().QueryRow(
		`SELECT updated_at FROM memories WHERE id = ?`, target).Scan(&beforeRaw); err != nil {
		t.Fatalf("read updated_at before: %v", err)
	}

	deferRow(t, dm, target, "batch-1", "refusal_sentinel", "sample")

	// Deferral stamps it too, so the value at requeue time is what the
	// requeue must advance past. Sleeping one second keeps the two
	// stamps distinguishable without depending on sub-second clock
	// resolution.
	notBefore := int(time.Now().Unix())
	time.Sleep(1100 * time.Millisecond)

	if _, err := dm.RequeueDeferred(context.Background(), 10); err != nil {
		t.Fatalf("requeue: %v", err)
	}

	var raw interface{}
	if err := dm.SQLDB().QueryRow(
		`SELECT updated_at FROM memories WHERE id = ?`, target).Scan(&raw); err != nil {
		t.Fatalf("read updated_at after: %v", err)
	}

	// SQLite is dynamically typed, so read back the storage class
	// rather than assuming: a string here would mean the column is
	// being written in the wrong format, which is the failure this
	// test exists to catch.
	var after int
	switch v := raw.(type) {
	case int64:
		after = int(v)
	case int:
		after = v
	case []byte:
		parsed, convErr := strconv.Atoi(string(v))
		if convErr != nil {
			t.Fatalf("updated_at stored as text %q, want an INTEGER epoch: %v", v, convErr)
		}
		after = parsed
	default:
		t.Fatalf("updated_at has storage class %T (%v), want INTEGER epoch", raw, raw)
	}

	if after < notBefore {
		t.Errorf("updated_at = %d, want >= %d — requeue must stamp the mutation time", after, notBefore)
	}
	if fmt.Sprintf("%v", beforeRaw) == fmt.Sprintf("%d", after) {
		t.Errorf("updated_at unchanged at %d — a requeue is a modification and must be visible to recency readers", after)
	}
	t.Logf("updated_at: before=%v (%T) after=%d (%T)", beforeRaw, beforeRaw, after, raw)
}

// The stamp must not disturb anything else about the row. This is the
// conjunction the design actually promises: updated_at advances, and
// every other substantive column is exactly as it was.
func TestRequeue_UpdatedAtStampTouchesNothingElse(t *testing.T) {
	dm := NewTestDM(t)
	ids := seedMixed(t, dm, 1, 0, 0)
	target := ids[0]
	deferRow(t, dm, target, "cdb-1", "refusal_sentinel", "a sample")
	// compacted_into is a metadata key, not a column (schema.go:140).
	if _, err := dm.SQLDB().Exec(
		`UPDATE memories SET metadata = json_set(metadata, '$.unrelated', 'keepme',
		 '$.compacted_into', 'lesson-xyz') WHERE id = ?`, target); err != nil {
		t.Fatalf("seed unrelated metadata + compacted_into: %v", err)
	}

	before := snapshotRows(t, dm, []string{target})[0]

	if _, err := dm.RequeueDeferred(context.Background(), 10); err != nil {
		t.Fatalf("requeue: %v", err)
	}
	after := snapshotRows(t, dm, []string{target})[0]

	if before.content != after.content {
		t.Errorf("content changed: %q -> %q", before.content, after.content)
	}
	if before.createdAt != after.createdAt {
		t.Errorf("created_at changed: %q -> %q", before.createdAt, after.createdAt)
	}

	m := deferralMetadata(t, dm, target)
	if m.At != "" || m.Reason != "" || m.Batch != "" || m.Sample != "" {
		t.Errorf("deferral keys survived the stamp: %+v", m)
	}

	var unrelated, compactedInto string
	if err := dm.SQLDB().QueryRow(`
		SELECT COALESCE(json_extract(metadata, '$.unrelated'), ''),
		       COALESCE(json_extract(metadata, '$.compacted_into'), '')
		FROM memories WHERE id = ?`, target).Scan(&unrelated, &compactedInto); err != nil {
		t.Fatalf("read back: %v", err)
	}
	if unrelated != "keepme" {
		t.Errorf("unrelated metadata = %q, want it preserved", unrelated)
	}
	if compactedInto != "lesson-xyz" {
		t.Errorf("compacted_into = %q, want it preserved — requeue is not a compaction-lifecycle operation", compactedInto)
	}
}
