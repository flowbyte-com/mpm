// review_age_calculation_test.go — alpha-4.1.2 D-009 regression test.
//
// Audit finding: the auditor flagged that `mpm review --stale` rendered
// every memory as "106751d ago" (≈292 years) for newly-created rows.
// The pre-fix code's toTime only handled time.Time and string shapes;
// integer Unix-epoch timestamps (the canonical shape after the 2026-08
// timestamps_unified_v1 migration) fell through to the zero-time default,
// which `time.Since` rendered as the year-1-to-now delta.
//
// Fix (alpha-4.1.1): toTime gained `int`, `int64`, and `float64` branches
// that route through time.Unix so the relative-age display reads sensibly.
//
// This test pins the post-fix contract: toTime returns sensible time.Time
// values for every timestamp shape the substrate may produce.

package main

import (
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
)

func TestToTime_Int_UnixEpoch(t *testing.T) {
	now := time.Now().Unix()
	got := toTime(int(now))
	assert.WithinDuration(t, time.Unix(now, 0), got, time.Second,
		"int timestamp must round-trip through time.Unix")
}

func TestToTime_Int64_UnixEpoch(t *testing.T) {
	now := time.Now().Unix()
	got := toTime(int64(now))
	assert.WithinDuration(t, time.Unix(now, 0), got, time.Second,
		"int64 timestamp must round-trip through time.Unix")
}

func TestToTime_Float64_UnixEpoch(t *testing.T) {
	now := float64(time.Now().Unix())
	got := toTime(now)
	assert.WithinDuration(t, time.Unix(int64(now), 0), got, time.Second,
		"float64 timestamp must round-trip through time.Unix")
}

func TestToTime_TimeTime_PassesThrough(t *testing.T) {
	want := time.Date(2026, 8, 30, 12, 0, 0, 0, time.UTC)
	got := toTime(want)
	assert.True(t, got.Equal(want), "time.Time must pass through unchanged")
}

func TestToTime_StringRFC3339_Parses(t *testing.T) {
	want := time.Date(2026, 8, 30, 12, 0, 0, 0, time.UTC)
	got := toTime(want.Format(time.RFC3339))
	assert.True(t, got.Equal(want), "RFC3339 string must parse correctly")
}

// TestToTime_NegativeOrZero_ReturnsZeroTime pins the guard: a 0 or
// negative timestamp must NOT be passed to time.Unix (which would
// produce a date far in the past and trip the 106751d display bug
// the auditor reported).
func TestToTime_Zero_ReturnsZeroTime(t *testing.T) {
	got := toTime(int(0))
	assert.True(t, got.IsZero(), "int 0 must yield zero time, not year-1 epoch")
}

func TestToTime_Negative_ReturnsZeroTime(t *testing.T) {
	got := toTime(int64(-1))
	assert.True(t, got.IsZero(), "negative int64 must yield zero time")
}

// TestToTime_UnknownType_ReturnsZeroTime pins that an unsupported type
// falls back to zero time rather than panicking. (The original bug
// also affected nil, but toTime's nil interface falls into `default`.)
func TestToTime_Nil_ReturnsZeroTime(t *testing.T) {
	got := toTime(nil)
	assert.True(t, got.IsZero(), "nil must yield zero time")
}

func TestToTime_UnsupportedType_ReturnsZeroTime(t *testing.T) {
	type weird struct{ x int }
	got := toTime(weird{x: 42})
	assert.True(t, got.IsZero(), "unsupported type must yield zero time")
}

// TestToTime_FreshMemory_DoesNotShowYear1Delta pins the auditor's
// reported bug at the integration level: feeding toTime the
// current-time Unix-epoch int must NOT produce a result that, when
// fed to time.Since, yields the 106751d "year 1 to now" delta.
//
// Pre-fix this test would fail because toTime fell through to the
// zero-time default for int input. Post-fix, the result is "now"
// and time.Since is well under 1 second.
func TestToTime_FreshMemory_NoYearOneDelta(t *testing.T) {
	now := time.Now().Unix()
	got := toTime(now)
	delta := time.Since(got)
	assert.Less(t, delta, 5*time.Second,
		"a fresh-memory int timestamp must not produce a year-1 delta "+
			"(got %v since rendering)", delta)
}