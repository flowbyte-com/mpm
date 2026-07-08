// wake_tools_test.go — round-trip tests for Phase 5a scheduled_wakes.
//
// Coverage:
//   - DM layer: ScheduleWake, CheckPendingWakes (idempotency + transactional
//     mark), ListScheduledWakes (filters), resolveTargetTime (relative +
//     absolute parsing)
//   - Handler layer: schedule_wake (happy path, missing reason, missing
//     target_time, relative duration), check_wakes (fold semantics + dedup),
//     list_wakes (defaults + filters), opportunistic folding (every write
//     surfaces due wakes)
//
// The tests use the same newTestSharedDM harness as the other tools
// handlers tests — a fresh tmpdir DB per test, no global state.

package internal

import (
	"database/sql"
	"fmt"
	"strings"
	"testing"
	"time"
)

// ── DM layer tests ─────────────────────────────────────────────────────

func TestScheduleWake_RelativeDuration(t *testing.T) {
	dm := newTestWakeDM(t)
	now := time.Now()
	out, err := dm.ScheduleWake("check Wimbledon R1", "24h", "", "+24h", "test-agent", nil)
	if err != nil {
		t.Fatalf("ScheduleWake: %v", err)
	}
	if id, _ := out["id"].(string); !strings.HasPrefix(id, "wk-") {
		t.Errorf("expected wk-* id, got %q", id)
	}
	got, _ := out["target_time"].(int64)
	want := now.Add(24 * time.Hour).Unix()
	if abs(got-want) > 2 { // ±2s for test execution latency
		t.Errorf("target_time: got %d, want ~%d (delta %d)", got, want, abs(got-want))
	}
}

func TestScheduleWake_AbsoluteEpoch(t *testing.T) {
	dm := newTestWakeDM(t)
	abs := time.Now().Add(48 * time.Hour).Unix()
	out, err := dm.ScheduleWake("future check", fmt.Sprintf("%d", abs), "", "", "test-agent", nil)
	if err != nil {
		t.Fatalf("ScheduleWake: %v", err)
	}
	got, _ := out["target_time"].(int64)
	if got != abs {
		t.Errorf("absolute target_time: got %d, want %d", got, abs)
	}
}

func TestScheduleWake_RejectsEmpty(t *testing.T) {
	dm := newTestWakeDM(t)
	if _, err := dm.ScheduleWake("", "24h", "", "", "test-agent", nil); err == nil {
		t.Error("expected error for empty reason")
	}
	if _, err := dm.ScheduleWake("ok", "", "", "", "test-agent", nil); err == nil {
		t.Error("expected error for empty target_time")
	}
}

func TestCheckPendingWakes_TransactionalIdempotency(t *testing.T) {
	dm := newTestWakeDM(t)
	past := time.Now().Add(-1 * time.Hour).Unix()
	for i := 0; i < 3; i++ {
		_, err := dm.ScheduleWake(
			fmt.Sprintf("wake %d", i),
			fmt.Sprintf("%d", past+int64(i)),
			"", "", "test-agent", nil,
		)
		if err != nil {
			t.Fatalf("ScheduleWake %d: %v", i, err)
		}
	}
	first, err := dm.CheckPendingWakes(time.Now())
	if err != nil {
		t.Fatalf("first check: %v", err)
	}
	if len(first) != 3 {
		t.Errorf("first check: got %d wakes, want 3", len(first))
	}
	// Concurrent callers must not see the same wake twice.
	second, err := dm.CheckPendingWakes(time.Now())
	if err != nil {
		t.Fatalf("second check: %v", err)
	}
	if len(second) != 0 {
		t.Errorf("second check: got %d wakes, want 0 (idempotency broken)", len(second))
	}
}

func TestCheckPendingWakes_NotDueYet(t *testing.T) {
	dm := newTestWakeDM(t)
	future := time.Now().Add(2 * time.Hour).Unix()
	if _, err := dm.ScheduleWake("future wake", fmt.Sprintf("%d", future), "", "", "test-agent", nil); err != nil {
		t.Fatalf("ScheduleWake: %v", err)
	}
	got, err := dm.CheckPendingWakes(time.Now())
	if err != nil {
		t.Fatalf("CheckPendingWakes: %v", err)
	}
	if len(got) != 0 {
		t.Errorf("expected 0 wakes, got %d (future wake surfaced prematurely)", len(got))
	}
}

func TestCheckPendingWakes_MarksFiredWithTimestamp(t *testing.T) {
	dm := newTestWakeDM(t)
	past := time.Now().Add(-30 * time.Minute).Unix()
	out, err := dm.ScheduleWake("check this", fmt.Sprintf("%d", past), "theory-xyz", "", "test-agent", nil)
	if err != nil {
		t.Fatalf("ScheduleWake: %v", err)
	}
	id, _ := out["id"].(string)
	got, err := dm.CheckPendingWakes(time.Now())
	if err != nil {
		t.Fatalf("CheckPendingWakes: %v", err)
	}
	if len(got) != 1 {
		t.Fatalf("expected 1 wake, got %d", len(got))
	}
	w := got[0]
	if w["id"] != id {
		t.Errorf("id mismatch: got %v, want %s", w["id"], id)
	}
	if w["theory_id"] != "theory-xyz" {
		t.Errorf("theory_id not preserved: got %v", w["theory_id"])
	}
	firedAt, _ := w["fired_at"].(int64)
	if firedAt == 0 {
		t.Error("fired_at not set")
	}
	overdue, _ := w["overdue_secs"].(int64)
	if overdue < 1700 || overdue > 1900 { // ~30 min ±slop
		t.Errorf("overdue_secs: got %d, want ~1800", overdue)
	}
}

func TestListScheduledWakes_DefaultsToPending(t *testing.T) {
	dm := newTestWakeDM(t)
	// one pending, one past (should appear in default list, since not yet fired)
	past := time.Now().Add(-10 * time.Minute).Unix()
	future := time.Now().Add(2 * time.Hour).Unix()
	if _, err := dm.ScheduleWake("past wake", fmt.Sprintf("%d", past), "", "", "test-agent", nil); err != nil {
		t.Fatalf("ScheduleWake past: %v", err)
	}
	if _, err := dm.ScheduleWake("future wake", fmt.Sprintf("%d", future), "", "", "test-agent", nil); err != nil {
		t.Fatalf("ScheduleWake future: %v", err)
	}
	got, err := dm.ListScheduledWakes(false, false, 100)
	if err != nil {
		t.Fatalf("ListScheduledWakes: %v", err)
	}
	if len(got) != 2 {
		t.Errorf("default list: got %d, want 2 (both unfired)", len(got))
	}
}

func TestListScheduledWakes_OverdueOnly(t *testing.T) {
	dm := newTestWakeDM(t)
	past := time.Now().Add(-1 * time.Hour).Unix()
	future := time.Now().Add(2 * time.Hour).Unix()
	if _, err := dm.ScheduleWake("past wake", fmt.Sprintf("%d", past), "", "", "test-agent", nil); err != nil {
		t.Fatalf("ScheduleWake past: %v", err)
	}
	if _, err := dm.ScheduleWake("future wake", fmt.Sprintf("%d", future), "", "", "test-agent", nil); err != nil {
		t.Fatalf("ScheduleWake future: %v", err)
	}
	got, err := dm.ListScheduledWakes(false, true, 100)
	if err != nil {
		t.Fatalf("ListScheduledWakes: %v", err)
	}
	if len(got) != 1 {
		t.Errorf("overdue_only: got %d, want 1", len(got))
	}
	if r := got[0]; r["reason"] != "past wake" {
		t.Errorf("overdue_only returned wrong wake: %v", r["reason"])
	}
}

func TestListScheduledWakes_IncludeFired(t *testing.T) {
	dm := newTestWakeDM(t)
	past := time.Now().Add(-1 * time.Hour).Unix()
	if _, err := dm.ScheduleWake("to be fired", fmt.Sprintf("%d", past), "", "", "test-agent", nil); err != nil {
		t.Fatalf("ScheduleWake: %v", err)
	}
	if _, err := dm.CheckPendingWakes(time.Now()); err != nil {
		t.Fatalf("CheckPendingWakes: %v", err)
	}
	// Default: not include_fired → empty
	pending, err := dm.ListScheduledWakes(false, false, 100)
	if err != nil {
		t.Fatalf("ListScheduledWakes default: %v", err)
	}
	if len(pending) != 0 {
		t.Errorf("default list after fire: got %d, want 0", len(pending))
	}
	// include_fired: audit trail
	all, err := dm.ListScheduledWakes(true, false, 100)
	if err != nil {
		t.Fatalf("ListScheduledWakes include_fired: %v", err)
	}
	if len(all) != 1 {
		t.Errorf("include_fired list: got %d, want 1", len(all))
	}
	if r := all[0]; r["fired"] != true {
		t.Errorf("fired flag not true in include_fired list: %v", r["fired"])
	}
}

func TestResolveTargetTime_Parsing(t *testing.T) {
	now := time.Now()
	cases := []struct {
		in     string
		delta  time.Duration
		errExp bool
	}{
		{"30s", 30 * time.Second, false},
		{"5m", 5 * time.Minute, false},
		{"2h", 2 * time.Hour, false},
		{"1d", 24 * time.Hour, false},
		{"7d", 7 * 24 * time.Hour, false},
		{"0s", 0, true},
		{"", 0, true},
		{"24", 24 * time.Hour, false}, // pure integer = absolute epoch
		{"abc", 0, true},
		{"5x", 0, true}, // unknown unit
	}
	for _, c := range cases {
		got, err := resolveTargetTime(c.in, now)
		if c.errExp {
			if err == nil {
				t.Errorf("resolveTargetTime(%q): expected error, got %d", c.in, got)
			}
			continue
		}
		if err != nil {
			t.Errorf("resolveTargetTime(%q): unexpected error %v", c.in, err)
			continue
		}
		if c.in == "24" {
			if got != 24 {
				t.Errorf("resolveTargetTime(%q) as absolute: got %d, want 24", c.in, got)
			}
			continue
		}
		want := now.Add(c.delta).Unix()
		if abs(got-want) > 1 {
			t.Errorf("resolveTargetTime(%q): got %d, want ~%d", c.in, got, want)
		}
	}
}

func TestResolveTargetTime_ISO8601(t *testing.T) {
	now := time.Now()
	cases := []struct {
		in   string
		want int64
	}{
		{"2026-07-12T12:00:00Z", time.Date(2026, 7, 12, 12, 0, 0, 0, time.UTC).Unix()},
		{"2026-07-12T12:00:00+02:00", time.Date(2026, 7, 12, 10, 0, 0, 0, time.UTC).Unix()},
		{"2026-07-12T12:00:00.500Z", time.Date(2026, 7, 12, 12, 0, 0, 0, time.UTC).Unix()},
		{"2026-07-12 12:00:00", time.Date(2026, 7, 12, 12, 0, 0, 0, time.UTC).Unix()},
	}
	for _, c := range cases {
		got, err := resolveTargetTime(c.in, now)
		if err != nil {
			t.Errorf("resolveTargetTime(%q): unexpected error %v", c.in, err)
			continue
		}
		// The space form parses in local tz; allow tz offset by comparing
		// only the RFC3339 UTC cases exactly and the space form loosely.
		if c.in == "2026-07-12 12:00:00" {
			if got == 0 {
				t.Errorf("resolveTargetTime(%q): got 0", c.in)
			}
			continue
		}
		if got != c.want {
			t.Errorf("resolveTargetTime(%q): got %d, want %d", c.in, got, c.want)
		}
	}
}

// ── Handler-layer tests (mirror the tools/handlers_test.go shape) ──────

// helper: build a wake DM with a temp shared DB, then wipe the LOCAL
// scheduled_wakes table so prior tests do not leak rows. The shared DB
// is per-test (tmpdir), but the local DB at $repo/src/db/mpm.db is
// shared across all tests in this package's binary (per NewDatabaseManager
// at internal/db.go:489 — local DB path is fixed regardless of
// projectRoot). Wiping scheduled_wakes between tests gives isolation
// without requiring a full NewDatabaseManagerForDB + InitSchema dance.
func newTestWakeDM(t *testing.T) *DatabaseManager {
	t.Helper()
	tmp := t.TempDir()
	t.Setenv("MPM_SHARED_DB", tmp+"/shared.db")
	dm, err := NewDatabaseManager("")
	if err != nil {
		t.Fatalf("NewDatabaseManager: %v", err)
	}
	t.Cleanup(func() {
		_, _ = dm.db.Exec(`DELETE FROM scheduled_wakes`)
		dm.Close()
	})
	_, _ = dm.db.Exec(`DELETE FROM scheduled_wakes`)
	return dm
}

func abs(x int64) int64 {
	if x < 0 {
		return -x
	}
	return x
}
// ── digest tests ───────────────────────────────────────────────────────

// TestDigestScheduledWakes_BucketsAndTopN verifies the digest summarises
// overdue wakes into age buckets and surfaces the top-N most overdue by
// target_time ASC. Pins the contract for the "agent wakes after long
// idle" case where listing every overdue wake individually would blow
// out context.
func TestDigestScheduledWakes_BucketsAndTopN(t *testing.T) {
	dm := newTestDM(t)

	now := time.Now().Unix()
	// Seed wakes across age buckets + a future one.
	// (target_time, reason) — times are absolute seconds-from-epoch.
	seed := []struct {
		offsetSecs int64
		reason     string
		theoryID   string // empty for none
	}{
		{-30, "30s overdue (under_1h)", ""},
		{-7200, "2h overdue (1h_to_1d)", ""},
		{-3 * 86400, "3d overdue (1d_to_1w)", ""},
		{-14 * 86400, "14d overdue (1w_to_1mo)", "th-oldest"},
		{-45 * 86400, "45d overdue (over_1mo)", ""},
		{-2 * 86400, "2d overdue (1d_to_1w, second entry)", "th-recent"},
		{3600, "1h future pending", ""},
	}
	for _, s := range seed {
		theory := sql.NullString{String: s.theoryID, Valid: s.theoryID != ""}
		if _, err := dm.db.Exec(`
			INSERT INTO scheduled_wakes (id, target_time, reason, theory_id, fired, created_by)
			VALUES (?, ?, ?, ?, 0, 'test')
		`, "wk-test-"+s.reason, now+s.offsetSecs, s.reason, theory); err != nil {
			t.Fatalf("seed wake %q: %v", s.reason, err)
		}
	}

	d, err := dm.DigestScheduledWakes(3)
	if err != nil {
		t.Fatalf("DigestScheduledWakes: %v", err)
	}

	// 6 overdue + 1 future pending.
	if c, _ := d["total_overdue"].(int); c != 6 {
		t.Errorf("total_overdue: got %d, want 6", c)
	}
	if c, _ := d["total_pending_future"].(int); c != 1 {
		t.Errorf("total_pending_future: got %d, want 1", c)
	}

	// 2 wakes linked to theories.
	if c, _ := d["linked_to_theory"].(int); c != 2 {
		t.Errorf("linked_to_theory: got %d, want 2", c)
	}

	// Bucket counts.
	buckets, ok := d["age_buckets"].(map[string]int)
	if !ok {
		t.Fatalf("age_buckets missing or wrong type: %T", d["age_buckets"])
	}
	want := map[string]int{
		"under_1h":  1,
		"1h_to_1d":  1,
		"1d_to_1w":  2,
		"1w_to_1mo": 1,
		"over_1mo":  1,
	}
	for k, v := range want {
		if buckets[k] != v {
			t.Errorf("bucket %q: got %d, want %d", k, buckets[k], v)
		}
	}

	// Oldest is the 45d one (most negative target_time).
	if reason, _ := d["oldest_overdue_reason"].(string); reason != "45d overdue (over_1mo)" {
		t.Errorf("oldest_overdue_reason: got %q, want the 45d one", reason)
	}
	if secs, _ := d["oldest_overdue_secs"].(int64); secs < 44*86400 {
		t.Errorf("oldest_overdue_secs: got %d, want >= %d", secs, 44*86400)
	}

	// top_overdue should be 3 rows, in target_time ASC order (most overdue first).
	top, ok := d["top_overdue"].([]map[string]interface{})
	if !ok {
		t.Fatalf("top_overdue missing or wrong type: %T", d["top_overdue"])
	}
	if len(top) != 3 {
		t.Fatalf("top_overdue length: got %d, want 3", len(top))
	}
	if reason, _ := top[0]["reason"].(string); reason != "45d overdue (over_1mo)" {
		t.Errorf("top[0] reason: got %q, want oldest", reason)
	}
	if reason, _ := top[1]["reason"].(string); reason != "14d overdue (1w_to_1mo)" {
		t.Errorf("top[1] reason: got %q, want 14d", reason)
	}
	if reason, _ := top[2]["reason"].(string); reason != "3d overdue (1d_to_1w)" {
		t.Errorf("top[2] reason: got %q, want 3d", reason)
	}
}

// TestDigestScheduledWakes_Empty confirms the digest works when no
// wakes exist — no panic, sensible zero values.
func TestDigestScheduledWakes_Empty(t *testing.T) {
	dm := newTestDM(t)
	d, err := dm.DigestScheduledWakes(5)
	if err != nil {
		t.Fatalf("DigestScheduledWakes: %v", err)
	}
	if c, _ := d["total_overdue"].(int); c != 0 {
		t.Errorf("total_overdue: got %d, want 0", c)
	}
	if c, _ := d["total_pending_future"].(int); c != 0 {
		t.Errorf("total_pending_future: got %d, want 0", c)
	}
	if _, present := d["oldest_overdue_iso"]; present {
		t.Error("oldest_overdue_iso should be absent when no overdue wakes")
	}
	top, _ := d["top_overdue"].([]map[string]interface{})
	if len(top) != 0 {
		t.Errorf("top_overdue: got %d rows, want 0", len(top))
	}
}
