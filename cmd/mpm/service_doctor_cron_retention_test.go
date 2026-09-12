// service_doctor_cron_retention_test.go — diagnostic-contract tests for
// the cron-wake retention interpretation in `mpm doctor`.
//
// Background: a previous agent health check saw cron wakes accumulating
// in `scheduled_wakes` and incorrectly classified that as a scheduler
// failure. The retention model (wake_expiration.go: CronRetention*)
// intentionally retains ~60-120 cron rows between sweeps. A fresh
// daemon takes ~2h before its first actual retirement under the
// strict-`<` cutoff semantics. The diagnostic surface must explain that.
//
// These tests pin the interpretation: healthy vs degraded classification
// across startup-stabilization, steady, and missed-sweep conditions.
//
// Test mechanics:
//   - dm is a fresh temp DB via newTestDMForCmd
//   - scheduler.state.json fixture points at a temp file via the
//     schedulerStatePath package var override (the production
//     schedulerStatePath() resolves via MPM_WORKSPACE — important
//     that tests don't overwrite the live scheduler.state.json)
//   - Seeded cron rows are based on real time.Now() so they're
//     correctly aged at the moment the diagnostic runs
//   - "now" is real time throughout; uptime is controlled by varying
//     process_started_unix in the scheduler.state.json fixture
//
// All assertions target semantic fields (Phase, Status, EligibleBacklog,
// SecondsUntilNextExpectedSweep) rather than full rendered strings.

package main

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"testing"
	"time"

	mpmcore "github.com/flowbyte-com/mpm-core"

	"github.com/flowbyte-com/mpm/internal/scheduler"
)

type testCronRow struct {
	insertedAt time.Time
}

// seedCronRows inserts cron-kind scheduled_wakes with the given
// target_times.
func seedCronRows(t *testing.T, dm *mpmcore.DatabaseManager, rows []testCronRow) {
	t.Helper()
	for i, r := range rows {
		_, err := dm.SQLDB().Exec(`
			INSERT INTO scheduled_wakes (id, reason, target_time, fired, fired_at, created_by, metadata)
			VALUES (?, ?, ?, 0, NULL, 'test', ?)
		`, fmt.Sprintf("test-cron-%d", i),
			"test cron row",
			r.insertedAt.Unix(),
			`{"kind":"cron","source":"cron","task_id":"epistemic-compaction","directive_id":"mpm-seed-epistemic-compaction-policy"}`,
		)
		if err != nil {
			t.Fatalf("seed cron row %d: %v", i, err)
		}
	}
}

// writeFakeSchedulerState writes a scheduler.state.json fixture
// reflecting the given process_started_unix and last_tick_unix.
func writeFakeSchedulerState(t *testing.T, path string, startedAt, lastTick time.Time) {
	t.Helper()
	state := map[string]interface{}{
		"last_tick_unix":       lastTick.Unix(),
		"process_started_unix": startedAt.Unix(),
		"tick_count":           uint64(60),
		"pid":                  12345,
		"last_status":          "ok",
		"last_error":           "",
	}
	data, err := json.Marshal(state)
	if err != nil {
		t.Fatalf("marshal state: %v", err)
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	if err := os.WriteFile(path, data, 0o600); err != nil {
		t.Fatalf("write state: %v", err)
	}
}

// withFakeSchedulerState swaps the production schedulerStatePath
// for the duration of the test. Tests point it at a temp file
// (t.TempDir()/scheduler.state) so the live /home/v/.mpm/run/
// scheduler.state is NEVER overwritten by the test.
//
// Returns the temp path (pass to writeFakeSchedulerState) and a
// cleanup func that restores the production path.
func withFakeSchedulerState(t *testing.T) (string, func()) {
	t.Helper()
	statePath := filepath.Join(t.TempDir(), "run", "scheduler.state")
	prev := schedulerStatePath
	schedulerStatePath = func() string { return statePath }
	return statePath, func() { schedulerStatePath = prev }
}

// TestDoctorService_checkScheduler_FreshStartup_AllRowsYoung pins
// the fresh-startup interpretation: daemon uptime < 1h, no eligible
// rows, status must be PASS with phase=startup_stabilization.
func TestDoctorService_checkScheduler_FreshStartup_AllRowsYoung(t *testing.T) {
	dm := newTestDMForCmd(t)
	svc := NewDoctorService(dm)

	realNow := time.Now()
	startedAt := realNow.Add(-30 * time.Minute) // uptime 30m

	statePath, restoreSchedulerPath := withFakeSchedulerState(t)
	defer restoreSchedulerPath()
	writeFakeSchedulerState(t, statePath, startedAt, realNow.Add(-30*time.Second))

	// Insert 10 cron rows, all recent (last 10m). Real-now based so
	// they're correctly aged at the moment the diagnostic runs.
	var rows []testCronRow
	for i := 0; i < 10; i++ {
		rows = append(rows, testCronRow{insertedAt: realNow.Add(-time.Duration(i+1) * time.Minute)})
	}
	seedCronRows(t, dm, rows)

	check := svc.checkScheduler()
	if check.Status != "PASS" {
		t.Errorf("status = %q, want PASS (msg: %q)", check.Status, check.Message)
	}
	if check.CronRetention == nil {
		t.Fatal("CronRetention field missing from check")
	}
	if check.CronRetention.Phase != "startup_stabilization" {
		t.Errorf("phase = %q, want startup_stabilization", check.CronRetention.Phase)
	}
	if check.CronRetention.Pending != 10 {
		t.Errorf("pending = %d, want 10", check.CronRetention.Pending)
	}
	if check.CronRetention.EligibleBacklog != 0 {
		t.Errorf("eligible_backlog = %d, want 0", check.CronRetention.EligibleBacklog)
	}
	if check.CronRetention.RetentionWindowSec != int64(scheduler.CronRetentionWindow.Seconds()) {
		t.Errorf("retention_window_seconds = %d, want %d",
			check.CronRetention.RetentionWindowSec, int64(scheduler.CronRetentionWindow.Seconds()))
	}
}

// TestDoctorService_checkScheduler_StartupStabilization_EligibleBacklogOK
// pins: at uptime ~1h25m with eligible backlog nonzero, status is still
// PASS because the next expected sweep is at +2h and we haven't missed
// the calibration sweep yet.
func TestDoctorService_checkScheduler_StartupStabilization_EligibleBacklogOK(t *testing.T) {
	dm := newTestDMForCmd(t)
	svc := NewDoctorService(dm)

	realNow := time.Now()
	startedAt := realNow.Add(-85 * time.Minute) // uptime 1h25m

	statePath, restoreSchedulerPath := withFakeSchedulerState(t)
	defer restoreSchedulerPath()
	writeFakeSchedulerState(t, statePath, startedAt, realNow.Add(-30*time.Second))

	// 25 cron rows inserted 60-84m ago. Strict-< cutoff excludes the
	// row at exactly -60m (the boundary), so 24 are eligible.
	// Real-now based so they're correctly aged at test execution.
	var rows []testCronRow
	for i := 0; i < 25; i++ {
		rows = append(rows, testCronRow{insertedAt: realNow.Add(-time.Duration(60+i) * time.Minute)})
	}
	seedCronRows(t, dm, rows)

	check := svc.checkScheduler()
	if check.Status != "PASS" {
		t.Errorf("status = %q, want PASS (eligible backlog is normal during startup stabilization; msg: %q)",
			check.Status, check.Message)
	}
	if check.CronRetention.Phase != "startup_stabilization" {
		t.Errorf("phase = %q, want startup_stabilization", check.CronRetention.Phase)
	}
	if check.CronRetention.EligibleBacklog != 24 {
		t.Errorf("eligible_backlog = %d, want 24 (strict-< excludes the boundary row at exactly cutoff)",
			check.CronRetention.EligibleBacklog)
	}
	// Next expected sweep is at +2h (startedAt + 7200s). Since we set
	// startedAt = realNow - 85min, next sweep is at realNow + 35min.
	// Allow ±60s slack for tick-boundary alignment.
	wantSecsUntil := int64(35 * 60)
	diff := check.CronRetention.SecondsUntilNextExpectedSweep - wantSecsUntil
	if diff < -60 || diff > 60 {
		t.Errorf("seconds_until_next_expected_sweep = %d, want ~%d (diff=%d)",
			check.CronRetention.SecondsUntilNextExpectedSweep, wantSecsUntil, diff)
	}
}

// TestDoctorService_checkScheduler_SteadyState_Healthy pins the
// steady-state interpretation: uptime > 2h, eligible backlog within
// normal capacity, status PASS.
func TestDoctorService_checkScheduler_SteadyState_Healthy(t *testing.T) {
	dm := newTestDMForCmd(t)
	svc := NewDoctorService(dm)

	realNow := time.Now()
	startedAt := realNow.Add(-4*time.Hour - 19*time.Minute) // uptime > 2h

	statePath, restoreSchedulerPath := withFakeSchedulerState(t)
	defer restoreSchedulerPath()
	writeFakeSchedulerState(t, statePath, startedAt, realNow.Add(-30*time.Second))

	// 50 eligible rows (older than 1h) + 60 recent rows (within window).
	var rows []testCronRow
	for i := 0; i < 50; i++ {
		rows = append(rows, testCronRow{insertedAt: realNow.Add(-time.Duration(65+i) * time.Minute)})
	}
	for i := 0; i < 60; i++ {
		rows = append(rows, testCronRow{insertedAt: realNow.Add(-time.Duration(1+i) * time.Minute)})
	}
	seedCronRows(t, dm, rows)

	check := svc.checkScheduler()
	if check.Status != "PASS" {
		t.Errorf("status = %q, want PASS in steady state with backlog within capacity (msg: %q)",
			check.Status, check.Message)
	}
	if check.CronRetention.Phase != "steady" {
		t.Errorf("phase = %q, want steady", check.CronRetention.Phase)
	}
	if check.CronRetention.EligibleBacklog != 50 {
		t.Errorf("eligible_backlog = %d, want 50", check.CronRetention.EligibleBacklog)
	}
	if check.CronRetention.Pending != 110 {
		t.Errorf("pending = %d, want 110", check.CronRetention.Pending)
	}
}

// TestDoctorService_checkScheduler_SteadyState_BacklogBeyondCapacity
// pins: steady state with eligible backlog > normal cap AND past an
// expected sweep opportunity → WARN or FAIL.
//
// "Past an expected sweep opportunity" = elapsed time since last
// expected sweep > cadence AND backlog persists.
//
// The test seeds rows based on real time.Now() so they're eligible at
// the moment the diagnostic runs (the diagnostic uses real now, not
// a test-injected clock). This is intentional: the production code
// reads wall-clock time and the test must model that exactly.
func TestDoctorService_checkScheduler_SteadyState_BacklogBeyondCapacity(t *testing.T) {
	dm := newTestDMForCmd(t)
	svc := NewDoctorService(dm)

	realNow := time.Now()
	startedAt := realNow.Add(-5 * time.Hour) // uptime > 2h → steady phase

	statePath, restoreSchedulerPath := withFakeSchedulerState(t)
	defer restoreSchedulerPath()
	writeFakeSchedulerState(t, statePath, startedAt, realNow.Add(-30*time.Second))

	// Seed 250 rows all comfortably older than 1h so they are all
	// eligible at real-now.
	const seededEligible = 250
	for i := 0; i < seededEligible; i++ {
		_, err := dm.SQLDB().Exec(`
			INSERT INTO scheduled_wakes (id, reason, target_time, fired, fired_at, created_by, metadata)
			VALUES (?, ?, ?, 0, NULL, 'test', ?)
		`, fmt.Sprintf("test-cron-cap-%d", i),
			"test cron row",
			realNow.Add(-time.Duration(70+i) * time.Minute).Unix(), // > 1h old
			`{"kind":"cron","source":"cron","task_id":"epistemic-compaction","directive_id":"mpm-seed-epistemic-compaction-policy"}`,
		)
		if err != nil {
			t.Fatalf("seed row %d: %v", i, err)
		}
	}

	check := svc.checkScheduler()
	if check.Status == "PASS" {
		t.Errorf("status = PASS, want WARN or FAIL for backlog=%d (msg: %q)",
			check.CronRetention.EligibleBacklog, check.Message)
	}
	// The probe is bounded to CatchUpLimit+1 = 241; we expect
	// backlog to saturate there but at minimum should exceed the
	// normal cap of 60.
	if check.CronRetention.EligibleBacklog < 60 {
		t.Errorf("eligible_backlog = %d, want >= 60 (we seeded 250 rows all > 1h old)",
			check.CronRetention.EligibleBacklog)
	}
}

// TestDoctorService_checkScheduler_NoStateFile_FallsBackToActionableOnly
// pins: if scheduler.state.json is unreadable, the diagnostic falls
// back to the actionable-overdue count and stays informative.
func TestDoctorService_checkScheduler_NoStateFile_FallsBackToActionableOnly(t *testing.T) {
	dm := newTestDMForCmd(t)
	svc := NewDoctorService(dm)

	// Point the diagnostic at a non-existent state file. We use the
	// schedulerStatePath override so the live file is untouched.
	statePath, restore := withFakeSchedulerState(t)
	defer restore()
	_ = os.Remove(statePath) // ensure no fixture from previous run

	check := svc.checkScheduler()
	// No state file → uptime unknown → phase unknown, but no actionable
	// overdue so should still PASS with a degraded-info hint.
	if check.Status != "PASS" {
		t.Errorf("status = %q, want PASS (no actionable overdue; msg: %q)",
			check.Status, check.Message)
	}
	if check.CronRetention == nil {
		t.Fatal("CronRetention field missing")
	}
	if check.CronRetention.SchedulerUptimeSec != 0 {
		t.Errorf("scheduler_uptime_seconds = %d, want 0 when state file unreadable",
			check.CronRetention.SchedulerUptimeSec)
	}
}

// TestDoctorService_checkScheduler_ConstantsMatchCanonical pins:
// the diagnostic reuses scheduler.CronRetention* constants rather than
// duplicating magic numbers.
func TestDoctorService_checkScheduler_ConstantsMatchCanonical(t *testing.T) {
	dm := newTestDMForCmd(t)
	svc := NewDoctorService(dm)

	realNow := time.Now()
	startedAt := realNow.Add(-4 * time.Hour)
	statePath, restoreSchedulerPath := withFakeSchedulerState(t)
	defer restoreSchedulerPath()
	writeFakeSchedulerState(t, statePath, startedAt, realNow.Add(-30*time.Second))

	check := svc.checkScheduler()
	if check.CronRetention.RetentionWindowSec != int64(scheduler.CronRetentionWindow.Seconds()) {
		t.Errorf("retention_window_seconds = %d, want %d",
			check.CronRetention.RetentionWindowSec, int64(scheduler.CronRetentionWindow.Seconds()))
	}
	if check.CronRetention.SweepCadenceSec != int64(scheduler.CronRetentionCadence.Seconds()) {
		t.Errorf("sweep_cadence_seconds = %d, want %d",
			check.CronRetention.SweepCadenceSec, int64(scheduler.CronRetentionCadence.Seconds()))
	}
	if check.CronRetention.NormalLimit != scheduler.CronRetentionNormalLimit {
		t.Errorf("normal_limit = %d, want %d",
			check.CronRetention.NormalLimit, scheduler.CronRetentionNormalLimit)
	}
	if check.CronRetention.CatchUpLimit != scheduler.CronRetentionCatchUpLimit {
		t.Errorf("catchup_limit = %d, want %d",
			check.CronRetention.CatchUpLimit, scheduler.CronRetentionCatchUpLimit)
	}
}

// TestDoctorService_cronRetentionToStatus_Boundaries pins the
// verdict matrix at the documented boundary values so a future
// regression that re-introduces a dead branch (or breaks the
// capacity threshold ladder) is caught immediately.
//
// Backlog is seeded based on real time.Now() so the diagnostic —
// which reads wall-clock time — sees rows that are properly aged at
// the moment the test runs. All cases are in steady phase
// (uptime >> 2h) so the steady-state classification rules apply.
func TestDoctorService_cronRetentionToStatus_Boundaries(t *testing.T) {
	dm := newTestDMForCmd(t)
	svc := NewDoctorService(dm)

	realNow := time.Now()
	startedAt := realNow.Add(-6 * time.Hour) // uptime > 2h → steady

	statePath, restoreSchedulerPath := withFakeSchedulerState(t)
	defer restoreSchedulerPath()
	writeFakeSchedulerState(t, statePath, startedAt, realNow.Add(-30*time.Second))

	type tc struct {
		eligible      int    // rows seeded, all > 1h old
		wantStatus    string // expected diagnostic verdict
		desc          string
	}
	cases := []tc{
		{0, "PASS", "no eligible backlog (steady, post-sweep)"},
		{60, "PASS", "eligible at normal-cap boundary (just before next sweep)"},
		{61, "WARN", "eligible one row above normal cap (sustained retention debt)"},
		{100, "WARN", "eligible mid-range above normal cap"},
		{239, "WARN", "eligible just below catch-up threshold"},
		{240, "WARN", "eligible at catch-up threshold"},
		{360, "WARN", "eligible at severe-boundary (2 * CatchUpLimit)"},
		{361, "FAIL", "eligible one row above severe-boundary"},
	}

	for _, c := range cases {
		t.Run(c.desc, func(t *testing.T) {
			// Fresh DB per case so row counts are deterministic.
			fresh := newTestDMForCmd(t)
			freshSvc := NewDoctorService(fresh)

			// Seed c.eligible rows, all comfortably older than 1h so
			// they're all eligible at real-now.
			for i := 0; i < c.eligible; i++ {
				_, err := fresh.SQLDB().Exec(`
					INSERT INTO scheduled_wakes (id, reason, target_time, fired, fired_at, created_by, metadata)
					VALUES (?, ?, ?, 0, NULL, 'test', ?)
				`, fmt.Sprintf("test-boundary-%s-%d", c.desc, i),
					"test cron row",
					realNow.Add(-time.Duration(70+i) * time.Minute).Unix(), // > 1h old
					`{"kind":"cron","source":"cron","task_id":"epistemic-compaction","directive_id":"mpm-seed-epistemic-compaction-policy"}`,
				)
				if err != nil {
					t.Fatalf("seed row %d: %v", i, err)
				}
			}

			check := freshSvc.checkScheduler()
			if check.Status != c.wantStatus {
				t.Errorf("eligible=%d → status=%q, want %q (msg: %q)",
					c.eligible, check.Status, c.wantStatus, check.Message)
			}
			if check.CronRetention.EligibleBacklog != c.eligible {
				t.Errorf("eligible_backlog field = %d, want %d (probe mismatch)",
					check.CronRetention.EligibleBacklog, c.eligible)
			}
		})
		_ = svc // silence unused-var if no subtests are skipped
	}
}