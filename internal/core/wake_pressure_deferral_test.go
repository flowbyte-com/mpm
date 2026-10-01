// wake_pressure_deferral_test.go — Phase 8 of the compact-refusal
// lifecycle.
//
// The wake/context trigger is the last place the deferral design could
// have been defeated. Everything below the trigger terminates: a
// refused batch leaves the pool, the drain returns, the process ends.
// If the TRIGGER still keys off raw_count, none of that matters — the
// scheduled reflex keeps firing on a substrate where compaction cannot
// succeed, forever, and the loop this design removes comes back one
// layer up.
//
// So these tests are mostly about a substrate that should be quiet.

package internal

import (
	"strconv"
	"testing"
)

// A fully-deferred substrate must not demand compaction. This is the
// headline property: raw_count is high, the model has declined
// everything, and there is no action an agent could take that would
// produce a lesson.
func TestWakePressure_DeferredOnlySubstrateDoesNotDemandCompaction(t *testing.T) {
	dm := NewTestDM(t)
	// Far above the default threshold of 100.
	seedMixed(t, dm, 0, 300, 0)

	got := dm.gatherEpistemicPressure()

	if got.RawCount != 300 {
		t.Fatalf("raw_count = %d, want 300", got.RawCount)
	}
	if got.DeferredCount != 300 {
		t.Errorf("deferred_count = %d, want 300", got.DeferredCount)
	}
	if got.ActionablePending != 0 {
		t.Errorf("actionable_pending = %d, want 0", got.ActionablePending)
	}
	if got.Exceeded {
		t.Error("exceeded = true on a fully-deferred substrate — the scheduled compaction reflex will fire forever and never produce a lesson")
	}
}

// The inverse: a backlog the model has NOT seen must still trigger.
// Moving the trigger to actionable_pending must not disarm it.
func TestWakePressure_ActionableBacklogStillDemandsCompaction(t *testing.T) {
	dm := NewTestDM(t)
	seedMixed(t, dm, 300, 0, 0)

	got := dm.gatherEpistemicPressure()

	if got.Exceeded != true {
		t.Errorf("exceeded = false with 300 actionable rows over a threshold of %d", got.Threshold)
	}
	if got.ActionablePending != 300 {
		t.Errorf("actionable_pending = %d, want 300", got.ActionablePending)
	}
}

// The case that separates the two: a substrate that is mostly deferred
// but has a real backlog hiding underneath. raw_count is unchanged by
// this test, so a trigger reading raw_count would pass the first
// assertion and fail the second — which is exactly the shape of bug
// this file exists to catch.
func TestWakePressure_TriggerIgnoresDeferredRowsInAMixedSubstrate(t *testing.T) {
	dm := NewTestDM(t)
	// 250 deferred, 5 actionable. raw_count=255, well over the
	// threshold; actionable=5, well under it.
	seedMixed(t, dm, 5, 250, 0)

	got := dm.gatherEpistemicPressure()

	if got.RawCount != 255 {
		t.Fatalf("raw_count = %d, want 255", got.RawCount)
	}
	if got.Exceeded {
		t.Error("exceeded = true — 250 of the 255 rows are ones the model already declined")
	}
	// Bump the actionable backlog past the threshold.
	if _, err := dm.SQLDB().Exec(`
		UPDATE memories SET metadata = NULL
		WHERE json_extract(metadata, '$.compaction_deferred_at') IS NOT NULL`); err != nil {
		t.Fatalf("un-defer: %v", err)
	}
	got = dm.gatherEpistemicPressure()
	if !got.Exceeded {
		t.Errorf("exceeded = false after un-deferring 250 rows; actionable_pending = %d", got.ActionablePending)
	}
}

// The threshold comparison itself is preserved verbatim — only the
// operand changed. An off-by-one introduced here would silently change
// when compaction triggers across every substrate, so the boundary is
// pinned on both sides, on the actionable count AND on the raw count
// they used to be.
func TestWakePressure_ThresholdBoundary(t *testing.T) {
	cases := []struct {
		name                 string
		actionable, deferred int
		wantExceeded         bool
	}{
		{"actionable just under", 9, 100, false},
		{"actionable exactly at", 10, 0, false},
		{"actionable just over", 11, 0, true},
		// The decisive pair: identical raw_count, opposite verdict.
		{"raw 110 all deferred", 0, 110, false},
		{"raw 110 all actionable", 110, 0, true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			d := NewTestDM(t)
			setPressureThreshold(t, d, 10)
			seedMixed(t, d, tc.actionable, tc.deferred, 0)

			got := d.gatherEpistemicPressure()

			if got.Threshold != 10 {
				t.Errorf("Threshold = %d, want 10", got.Threshold)
			}
			if got.RawCount != tc.actionable+tc.deferred {
				t.Errorf("raw_count = %d, want %d", got.RawCount, tc.actionable+tc.deferred)
			}
			if got.Exceeded != tc.wantExceeded {
				t.Errorf("exceeded = %v, want %v (actionable=%d deferred=%d threshold=%d)",
					got.Exceeded, tc.wantExceeded, got.ActionablePending, got.DeferredCount, got.Threshold)
			}
		})
	}
}

// ── helpers ──────────────────────────────────────────────────────────

func setPressureThreshold(t *testing.T, d *DatabaseManager, n int) {
	t.Helper()
	if _, err := d.SQLDB().Exec(`
		INSERT INTO system_config (key, raw_json, content_hash) VALUES ('compaction', ?, '')
		ON CONFLICT(key) DO UPDATE SET
		  raw_json = excluded.raw_json,
		  updated_at = CAST(strftime('%s','now') AS INTEGER)`,
		`{"raw_threshold":`+strconv.Itoa(n)+`}`); err != nil {
		t.Fatalf("set threshold: %v", err)
	}
}
