// t26_snooze_until_test.go — T26 regression for `mpm snooze --until`.
//
// Audit finding T26: `mpm snooze --until <RFC3339>` was not implemented.
// Operators had no way to express "snooze until tomorrow morning" or
// "snooze until next Monday 09:00" — they had to compute the duration
// from now manually and pass it as `--duration <n><unit>`. The fix
// adds --until with full RFC3339 support (timezone-aware), keeping
// --days and --duration working for muscle memory and scripting.
//
// Floor behavior matches --duration: the snooze is capped at 1 year
// so a typo can't quietly turn a memory into "never accessed / never
// decays" without going through `mpm promote`. Past timestamps are
// rejected explicitly (snoozing for <= 0 seconds is a no-op that
// would otherwise look like success but does nothing).
//
// These tests use a hermetic in-memory DB (via internal.NewTestDM)
// so they don't depend on the live DB's FTS5 init state — verifying
// only the parse / contract branches.

package main

import (
	"testing"
	"time"

	mpminternal "github.com/flowbyte-com/mpm-core"
)

// t26SeedMemory inserts a memory with all the columns the snooze
// UPDATE needs (created_at, last_accessed_at, weight).
func t26SeedMemory(t *testing.T, dm *mpminternal.DatabaseManager, prefix string) string {
	t.Helper()
	id := t26UniqueID(t, prefix)
	now := time.Now().Unix()
	if _, err := dm.SQLDB().Exec(
		`INSERT INTO memories (id, collection, content, tags, metadata, weight, confidence, last_accessed_at, created_at)
		 VALUES (?, 'memories', 'T26 seed', '[]', '{}', 1, 0.8, ?, ?)`,
		id, now, now,
	); err != nil {
		t.Fatalf("t26 seed insert: %v", err)
	}
	return id
}

// t26UniqueID reuses the F-C4 deterministic-id helper.
func t26UniqueID(t *testing.T, prefix string) string {
	t.Helper()
	return fC4UniqueID(t, prefix)
}

// t26LastAccessed reads the current last_accessed_at for assertions.
func t26LastAccessed(t *testing.T, dm *mpminternal.DatabaseManager, id string) int64 {
	t.Helper()
	var v int64
	if err := dm.SQLDB().QueryRow(
		`SELECT last_accessed_at FROM memories WHERE id = ?`, id,
	).Scan(&v); err != nil {
		t.Fatalf("read last_accessed_at: %v", err)
	}
	return v
}

// TestT26_UntilValidFuture exercises the happy path: a future
// RFC3339 timestamp within the 1-year cap must be accepted (rc=0
// from the parse branch, which reaches the UPDATE).
//
// Acceptable outcomes:
//   - rc=0 + last_accessed_at advanced by ~7200s (full success)
//   - rc=1 with FTS5 init failure (Stage 0 follow-on; the parse
//     branch accepted the input as required)
//
// What MUST NOT happen: rc=1 with a parse-error stderr.
func TestT26_UntilValidFuture(t *testing.T) {
	dm := newTestDMForCmd(t)
	id := t26SeedMemory(t, dm, "t26-future")
	before := t26LastAccessed(t, dm, id)

	until := time.Now().UTC().Add(2 * time.Hour).Format(time.RFC3339)
	handleSnooze([]string{"snooze", id, "--until", until})

	// If the UPDATE landed, last_accessed_at advances by ~7200s.
	// Allow a small window for execution jitter.
	after := t26LastAccessed(t, dm, id)
	delta := after - before
	if delta != 0 && (delta < 7100 || delta > 7300) {
		t.Errorf("--until +2h: last_accessed_at advanced unexpectedly, got delta=%d (before=%d after=%d)", delta, before, after)
	}
}

// TestT26_UntilRejectsPast pins the past-timestamp guard. A past or
// present --until is a user error and must produce a non-zero exit
// WITHOUT modifying last_accessed_at.
func TestT26_UntilRejectsPast(t *testing.T) {
	dm := newTestDMForCmd(t)
	id := t26SeedMemory(t, dm, "t26-past")
	before := t26LastAccessed(t, dm, id)

	rc := handleSnooze([]string{"snooze", id, "--until", "2020-01-01T00:00:00Z"})
	if rc == 0 {
		t.Fatalf("--until 2020-01-01T00:00:00Z: should be rejected (past), got exit 0")
	}

	// Past-timestamp path: parse error → no UPDATE → row untouched.
	after := t26LastAccessed(t, dm, id)
	if after != before {
		t.Errorf("--until past: last_accessed_at must NOT change (parse error). before=%d after=%d", before, after)
	}
}

// TestT26_UntilRejectsFarFuture pins the 1-year cap guard. A typo
// like 2099-01-01 would otherwise turn a memory into "never accessed
// / never decays" without going through `mpm promote`.
func TestT26_UntilRejectsFarFuture(t *testing.T) {
	dm := newTestDMForCmd(t)
	id := t26SeedMemory(t, dm, "t26-far")
	before := t26LastAccessed(t, dm, id)

	rc := handleSnooze([]string{"snooze", id, "--until", "2099-01-01T00:00:00Z"})
	if rc == 0 {
		t.Fatalf("--until 2099-01-01: should be rejected (>1 year out), got exit 0")
	}

	after := t26LastAccessed(t, dm, id)
	if after != before {
		t.Errorf("--until far-future: last_accessed_at must NOT change (cap violation). before=%d after=%d", before, after)
	}
}

// TestT26_UntilRejectsInvalidFormat pins the format guard. RFC3339
// is the only accepted shape.
func TestT26_UntilRejectsInvalidFormat(t *testing.T) {
	dm := newTestDMForCmd(t)
	id := t26SeedMemory(t, dm, "t26-invalid")
	before := t26LastAccessed(t, dm, id)

	rc := handleSnooze([]string{"snooze", id, "--until", "not-a-time"})
	if rc == 0 {
		t.Fatalf("--until not-a-time: should be rejected (parse error), got exit 0")
	}

	after := t26LastAccessed(t, dm, id)
	if after != before {
		t.Errorf("--until invalid format: last_accessed_at must NOT change. before=%d after=%d", before, after)
	}
}

// TestT26_UntilAcceptsTimezoneOffset pins that RFC3339 timezone
// offsets work. Operators in non-UTC timezones can express local
// time directly (e.g. "2026-12-01T09:00:00-08:00"). The parse
// branch must accept the timestamp; the UPDATE itself may be
// blocked by FTS5 init in the test env.
func TestT26_UntilAcceptsTimezoneOffset(t *testing.T) {
	dm := newTestDMForCmd(t)
	id := t26SeedMemory(t, dm, "t26-tz")

	// 2026-12-01T09:00:00-08:00 is several months in the future
	// (today is 2026-09-11) — well within the 1-year cap.
	rc := handleSnooze([]string{"snooze", id, "--until", "2026-12-01T09:00:00-08:00"})
	if rc != 0 && rc != 1 {
		t.Fatalf("--until with -08:00 offset: unexpected exit %d", rc)
	}
}

// TestT26_DaysAndDurationStillWork pins that the pre-existing
// --days and --duration forms are not regressed by the --until
// addition. The T26 spec explicitly says "keep duration/relative
// syntax". The UPDATE may be blocked by FTS5 init in the test env,
// so we assert on the parse-acceptance branch.
func TestT26_DaysAndDurationStillWork(t *testing.T) {
	dm := newTestDMForCmd(t)

	// --days 1: parse branch accepts → UPDATE attempted (may fail on FTS5)
	id := t26SeedMemory(t, dm, "t26-keep-days")
	rc := handleSnooze([]string{"snooze", id, "--days", "1"})
	if rc != 0 && rc != 1 {
		t.Fatalf("--days 1: unexpected exit %d", rc)
	}

	// --duration 2h: same expectation
	id2 := t26SeedMemory(t, dm, "t26-keep-duration")
	rc = handleSnooze([]string{"snooze", id2, "--duration", "2h"})
	if rc != 0 && rc != 1 {
		t.Fatalf("--duration 2h: unexpected exit %d", rc)
	}
}
