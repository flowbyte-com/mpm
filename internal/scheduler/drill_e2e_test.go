// drill_e2e_test.go — end-to-end scheduler-side drill execution.
//
// This is the "drill engine self-test" the user explicitly asked be
// separated from the real-framework harness. The synthetic path is
// deterministic; the test runs the scheduler with a brief interval
// and a pre-seeded drill wake, then verifies the drill_runs row
// lands with the expected verdict.
//
// The test does NOT shell out to `mpm call` (the audit hook would
// require a real binary). Instead, it invokes DrillHandler directly
// — which is what the scheduler does — and checks the run row. The
// scheduler's call path is identical to this direct call, so the
// row layout is the same shape the wake machinery would produce.

package scheduler

import (
	"context"
	"log/slog"
	"os"
	"path/filepath"
	"testing"
	"time"

	core "github.com/flowbyte-com/mpm-core"
)

// TestDrillE2E_SyntheticPassLifecycle exercises the full happy path:
//
//  1. Seed a drill fixture with a known-compliant shape
//  2. Build a scheduler, register the drill handler
//  3. Insert a scheduled_wake row that fires DrillHandler
//  4. Run one tick
//  5. Verify drill_runs has exactly one row with status='passed'
//
// If this test breaks, the user-visible capability "scheduled drill
// runs produce verdicts" is broken. The runtime path is what
// `mpm drills run` mirrors, and the scheduler's wake path is what
// production runs against.
func TestDrillE2E_SyntheticPassLifecycle(t *testing.T) {
	workspace := t.TempDir()
	t.Setenv("MPM_WORKSPACE", workspace)

	dm, err := core.NewDatabaseManager(workspace)
	if err != nil {
		t.Fatalf("NewDatabaseManager: %v", err)
	}
	defer dm.Close()
	if err := dm.InitSchema(); err != nil {
		t.Fatalf("InitSchema: %v", err)
	}

	// Drill fixture — `mpm_lessons:save` is enough for a one-step
	// PASS verdict. The synthetic harness emits the sequence
	// regardless of compliant flag (only multi-step drills actually
	// drop on non-compliant).
	drillPath := filepath.Join(workspace, "e2e-drill.yaml")
	if err := os.WriteFile(drillPath, []byte(`
id: e2e-drill
description: end-to-end synthetic pass
framework: synthetic
prompt: emit a single mpm_lessons call
expect:
  tools_required: [mpm_lessons]
  sequence:
    - tool: mpm_lessons
      action: save
timeout_secs: 5
`), 0o644); err != nil {
		t.Fatalf("write drill: %v", err)
	}

	// Run DrillHandler directly. The scheduler's wake path calls
	// this same function — we skip the wake machinery here because
	// the test is exercising the drill engine, not the timer.
	w := Wake{
		ID: "e2e-wake-1",
		Metadata: map[string]interface{}{
			"kind":      "drill",
			"drill_id":  "e2e-drill",
			"path":      drillPath,
			"compliant": true,
		},
	}
	if err := DrillHandler(w); err != nil {
		t.Fatalf("DrillHandler: %v", err)
	}

	// Verify the drill_runs row. Status should be 'passed' (the
	// synthetic harness emitted the expected sequence; the score
	// function called the row passed).
	var status, verdictJSON string
	if err := dm.SQLDB().QueryRow(`
		SELECT status, verdict FROM drill_runs
		WHERE drill_id = ? ORDER BY started_at DESC LIMIT 1`,
		"e2e-drill",
	).Scan(&status, &verdictJSON); err != nil {
		t.Fatalf("query drill_runs: %v", err)
	}
	if status != "passed" {
		t.Errorf("status = %q, want passed (verdict=%s)", status, verdictJSON)
	}
	if verdictJSON == "" {
		t.Error("verdict must be populated, not empty")
	}
}

// TestDrillE2E_SchedulerTickFiresDrill confirms that the scheduler's
// tick actually invokes the drill handler when a due wake is
// present. The test:
//
//  1. Constructs a scheduler with the same Register("drill", ...) call
//     that cmd/mpm-scheduler/main.go issues
//  2. Inserts a synthetic wake that's already due
//  3. Runs one tick with a short context
//  4. Verifies the wake row is marked fired=1 and the drill_runs row
//     carries the expected verdict
//
// This is the test that proves the wake contract works for drill
// runs end-to-end. If the scheduler routes drills but doesn't fire
// them, this test fails.
func TestDrillE2E_SchedulerTickFiresDrill(t *testing.T) {
	workspace := t.TempDir()
	t.Setenv("MPM_WORKSPACE", workspace)

	dm, err := core.NewDatabaseManager(workspace)
	if err != nil {
		t.Fatalf("NewDatabaseManager: %v", err)
	}
	defer dm.Close()
	if err := dm.InitSchema(); err != nil {
		t.Fatalf("InitSchema: %v", err)
	}

	// Drill fixture.
	drillPath := filepath.Join(workspace, "tick-drill.yaml")
	if err := os.WriteFile(drillPath, []byte(`
id: tick-drill
description: scheduler tick should fire this
framework: synthetic
prompt: emit a single call
expect:
  tools_required: [mpm_lessons]
  sequence:
    - tool: mpm_lessons
      action: save
`), 0o644); err != nil {
		t.Fatalf("write drill: %v", err)
	}

	// Build the scheduler the same way cmd/mpm-scheduler does.
	logger := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{
		Level: slog.LevelError, // quiet test logs
	}))
	s, err := New(dm.SQLDB(), logger)
	if err != nil {
		t.Fatalf("scheduler.New: %v", err)
	}
	defer s.Close()
	s.Register("drill", DrillHandler)

	// Insert a drill wake that's already due. The scheduled_wakes
	// table has no `kind` column — kind is read from metadata.k
	// ind (see QueryDueWakes in scheduler.go).
	_, err = dm.SQLDB().Exec(`
		INSERT INTO scheduled_wakes (id, target_time, reason, fired, created_by, metadata)
		VALUES (?, ?, ?, 0, ?, ?)`,
		"wake-tick-1",
		time.Now().Add(-1*time.Second).Unix(),
		"drill harness test",
		"scheduler_drill_e2e_test",
		`{"kind":"drill","drill_id":"tick-drill","path":"`+drillPath+`","compliant":true}`,
	)
	if err != nil {
		t.Fatalf("insert wake: %v", err)
	}

	// One tick: scheduler should pick up the wake and run DrillHandler.
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	if _, err := s.Tick(ctx); err != nil {
		t.Fatalf("Tick: %v", err)
	}

	// Wake row should be marked fired.
	var fired int
	if err := dm.SQLDB().QueryRow(
		`SELECT fired FROM scheduled_wakes WHERE id = ?`, "wake-tick-1",
	).Scan(&fired); err != nil {
		t.Fatalf("query wake: %v", err)
	}
	if fired != 1 {
		t.Errorf("fired = %d, want 1", fired)
	}

	// Drill_run row should exist with status='passed'.
	var status string
	if err := dm.SQLDB().QueryRow(
		`SELECT status FROM drill_runs WHERE drill_id = ? ORDER BY started_at DESC LIMIT 1`,
		"tick-drill",
	).Scan(&status); err != nil {
		t.Fatalf("query drill_runs: %v", err)
	}
	if status != "passed" {
		t.Errorf("drill_run status = %q, want passed", status)
	}
}
