// f_e1_snooze_units_test.go — F-E1 snooze duration unit handling.
//
// F-E1: `mpm snooze <id> --duration 1h` silently fell through to the
// default 1-day bump. The CLI only recognized `--days`, so any other
// flag (including the explicit `--duration` flag) was silently ignored
// and last_accessed_at moved forward by 1 day instead of 1 hour.
//
// The fix:
//   1. Recognize `--duration <n><unit>` where unit ∈ {m, h, d, w}
//   2. Reject unknown units with an explicit error (no silent fallback)
//   3. Reject unknown flags entirely (no silent fallback)
//   4. Cap duration at 1 year (parity with the --days cap)
//
// These tests exercise the public `mpm snooze` handler directly.
package main

import (
	"testing"
)

// TestF_E1_DurationHours is the headline regression: --duration 1h
// must advance last_accessed_at by 3600s, not 86400s.
func TestF_E1_DurationHours(t *testing.T) {
	id := fE1SeedMemory(t, "f-e1-hours")

	rc := handleSnooze([]string{"snooze", id, "--duration", "1h"})
	if rc != 0 {
		t.Fatalf("--duration 1h: got exit %d, want 0", rc)
	}

	advance := fE1ReadLastAccessedAdvance(t, id)
	// Allow ±5s slop for clock between seed and snooze.
	if advance < 3595 || advance > 3605 {
		t.Errorf("--duration 1h: last_accessed_at advanced by %ds, want ~3600s", advance)
	}
}

// TestF_E1_DurationMinutes pins minute-level granularity.
func TestF_E1_DurationMinutes(t *testing.T) {
	id := fE1SeedMemory(t, "f-e1-min")

	rc := handleSnooze([]string{"snooze", id, "--duration", "30m"})
	if rc != 0 {
		t.Fatalf("--duration 30m: got exit %d, want 0", rc)
	}

	advance := fE1ReadLastAccessedAdvance(t, id)
	if advance < 1795 || advance > 1805 {
		t.Errorf("--duration 30m: last_accessed_at advanced by %ds, want ~1800s", advance)
	}
}

// TestF_E1_DurationDays pins days (the canonical --days-equivalent).
func TestF_E1_DurationDays(t *testing.T) {
	id := fE1SeedMemory(t, "f-e1-days")

	rc := handleSnooze([]string{"snooze", id, "--duration", "2d"})
	if rc != 0 {
		t.Fatalf("--duration 2d: got exit %d, want 0", rc)
	}

	advance := fE1ReadLastAccessedAdvance(t, id)
	// 2 days = 172800s ± 5s.
	if advance < 172795 || advance > 172805 {
		t.Errorf("--duration 2d: last_accessed_at advanced by %ds, want ~172800s", advance)
	}
}

// TestF_E1_DurationWeeks pins week-level granularity.
func TestF_E1_DurationWeeks(t *testing.T) {
	id := fE1SeedMemory(t, "f-e1-weeks")

	rc := handleSnooze([]string{"snooze", id, "--duration", "1w"})
	if rc != 0 {
		t.Fatalf("--duration 1w: got exit %d, want 0", rc)
	}

	advance := fE1ReadLastAccessedAdvance(t, id)
	// 1 week = 604800s ± 5s.
	if advance < 604795 || advance > 604805 {
		t.Errorf("--duration 1w: last_accessed_at advanced by %ds, want ~604800s", advance)
	}
}

// TestF_E1_DurationUnknownUnitRejected pins the explicit-error contract:
// a typo like "1y" or "1x" must NOT silently fall back to 1 day.
func TestF_E1_DurationUnknownUnitRejected(t *testing.T) {
	id := fE1SeedMemory(t, "f-e1-bad-unit")

	// Capture state before snooze
	before := fE1ReadLastAccessed(t, id)

	rc := handleSnooze([]string{"snooze", id, "--duration", "1y"})
	if rc == 0 {
		t.Fatalf("--duration 1y: should be rejected (unknown unit), got exit 0")
	}

	// The row must NOT have been silently snoozed.
	after := fE1ReadLastAccessed(t, id)
	if after != before {
		t.Errorf("--duration 1y silently applied a snooze (before=%d, after=%d)", before, after)
	}
}

// TestF_E1_UnknownFlagRejected pins the no-silent-fallback contract:
// passing --nonsense must be rejected, not silently ignored.
func TestF_E1_UnknownFlagRejected(t *testing.T) {
	id := fE1SeedMemory(t, "f-e1-bad-flag")

	before := fE1ReadLastAccessed(t, id)

	rc := handleSnooze([]string{"snooze", id, "--nonsense", "1h"})
	if rc == 0 {
		t.Fatalf("--nonsense: should be rejected, got exit 0")
	}

	after := fE1ReadLastAccessed(t, id)
	if after != before {
		t.Errorf("--nonsense silently applied a snooze (before=%d, after=%d)", before, after)
	}
}

// TestF_E1_DurationTooLongRejected pins the 1-year cap.
func TestF_E1_DurationTooLongRejected(t *testing.T) {
	id := fE1SeedMemory(t, "f-e1-cap")

	rc := handleSnooze([]string{"snooze", id, "--duration", "54w"})
	if rc == 0 {
		t.Fatalf("--duration 54w: should be rejected (>1 year cap), got exit 0")
	}
}

// TestF_E1_DaysStillAccepted pins the backwards-compat contract:
// --days N must continue to work after the F-E1 fix.
func TestF_E1_DaysStillAccepted(t *testing.T) {
	id := fE1SeedMemory(t, "f-e1-days-backcompat")

	rc := handleSnooze([]string{"snooze", id, "--days", "5"})
	if rc != 0 {
		t.Fatalf("--days 5: got exit %d, want 0", rc)
	}

	advance := fE1ReadLastAccessedAdvance(t, id)
	// 5 days = 432000s ± 5s.
	if advance < 431995 || advance > 432005 {
		t.Errorf("--days 5: last_accessed_at advanced by %ds, want ~432000s", advance)
	}
}

// fE1SeedMemory inserts a memory with all the columns GetMemory and
// the snooze UPDATE need (created_at, last_accessed_at, weight).
func fE1SeedMemory(t *testing.T, prefix string) string {
	t.Helper()
	dm := getDBConcrete()
	if dm == nil {
		t.Skip("DB unavailable; F-E1 test requires a real connection")
	}
	id := fC4UniqueID(t, prefix)
	now := nowUnix()
	if _, err := dm.SQLDB().Exec(
		`INSERT INTO memories (id, collection, content, tags, metadata, weight, confidence, last_accessed_at, created_at)
		 VALUES (?, 'memories', 'F-E1 seed', '[]', '{}', 1, 0.8, ?, ?)`,
		id, now, now); err != nil {
		t.Fatalf("seed memory: %v", err)
	}
	return id
}

// fE1ReadLastAccessed returns the last_accessed_at value for an id.
func fE1ReadLastAccessed(t *testing.T, id string) int64 {
	t.Helper()
	dm := getDBConcrete()
	var lastAccessed *int64
	if err := dm.SQLDB().QueryRow(
		`SELECT last_accessed_at FROM memories WHERE id = ?`, id,
	).Scan(&lastAccessed); err != nil {
		t.Fatalf("read last_accessed_at: %v", err)
	}
	if lastAccessed == nil {
		return 0
	}
	return *lastAccessed
}

// fE1ReadLastAccessedAdvance returns last_accessed_at - nowUnix
// (the seconds the snooze moved the timestamp forward).
func fE1ReadLastAccessedAdvance(t *testing.T, id string) int64 {
	t.Helper()
	now := nowUnix()
	return fE1ReadLastAccessed(t, id) - now
}
