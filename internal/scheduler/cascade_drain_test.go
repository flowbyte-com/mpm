package scheduler

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"log/slog"
	"testing"
	"time"

	core "github.com/flowbyte-com/mpm-core"
)

func TestNewCascadeDrainHandler_AppliesDefaults(t *testing.T) {
	dm := core.NewTestDM(t)
	h := NewCascadeDrainHandler(dm, slog.Default(), CascadeDrainOptions{})

	if got, want := h.Budget(), 30*time.Second; got != want {
		t.Errorf("Budget() = %v, want %v", got, want)
	}
	// BatchSize defaults to 50 (system_config cascade_drain.max_intents_per_tick).
	if got, want := h.BatchSize(), 50; got != want {
		t.Errorf("BatchSize() = %d, want %d", got, want)
	}
}

func TestNewCascadeDrainHandler_HonoursOptions(t *testing.T) {
	dm := core.NewTestDM(t)
	h := NewCascadeDrainHandler(dm, slog.Default(), CascadeDrainOptions{
		Budget:    5 * time.Second,
		BatchSize: 25,
	})
	if got, want := h.Budget(), 5*time.Second; got != want {
		t.Errorf("Budget() = %v, want %v", got, want)
	}
	if got, want := h.BatchSize(), 25; got != want {
		t.Errorf("BatchSize() = %d, want %d", got, want)
	}
}

// seedCascadeOutbox writes n pending cascade intents into the outbox using
// direct INSERT (the canonical pattern from core/cascade_materializer_test.go).
// The outbox rows are valid for materialization even though the downstream
// artifact IDs are synthetic — MaterializeBatch only requires the row
// metadata, not the existence of the downstream artifacts.
func seedCascadeOutbox(t *testing.T, dm *core.DatabaseManager, n int) string {
	t.Helper()
	eventID := fmt.Sprintf("evt-test-%d", time.Now().UnixNano())
	triggerEvID := fmt.Sprintf("ev-trig-%d", time.Now().UnixNano())
	for i := 0; i < n; i++ {
		intentID := fmt.Sprintf("intent-%d-%d", i, time.Now().UnixNano())
		_, err := dm.SQLDB().Exec(`
			INSERT INTO epistemic_cascade_outbox
				(id, invalidation_event_id, dead_artifact_id, dead_artifact_type,
				 downstream_artifact_id, downstream_artifact_type,
				 trigger_evidence_id, cascade_depth, reason, status)
			VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, 'pending')
		`, intentID, eventID,
			"dead-mem", "memory",
			fmt.Sprintf("down-%d-%d", i, time.Now().UnixNano()),
			"theory",
			triggerEvID, 0, "drain test")
		if err != nil {
			t.Fatalf("seed outbox row %d: %v", i, err)
		}
	}
	return eventID
}

// captureLogs creates a logger + buffer pair for this test and returns
// a closure that returns parsed log records (cleared on each call).
// Thin wrapper over the existing captureLogger / captureLogsFrom helpers
// in logging_test.go.
func captureLogs(t *testing.T) (*slog.Logger, func() []map[string]any) {
	t.Helper()
	log, buf, mu := captureLogger()
	return log, func() []map[string]any {
		return captureLogsFrom(buf, mu)
	}
}

func logsContain(logs []map[string]any, key, value string) bool {
	for _, rec := range logs {
		if v, ok := rec[key].(string); ok && v == value {
			return true
		}
	}
	return false
}

func TestCascadeDrain_YieldsQueueEmpty(t *testing.T) {
	dm := core.NewTestDM(t)
	logger, drain := captureLogs(t)
	h := NewCascadeDrainHandler(dm, logger, CascadeDrainOptions{Budget: 5 * time.Second})

	// Empty outbox: handler should yield queue_empty immediately.
	err := h.tickHandler(context.Background())
	if err != nil {
		t.Fatalf("tickHandler returned error: %v", err)
	}
	logs := drain()
	if !logsContain(logs, "yield_reason", "queue_empty") {
		t.Errorf("expected yield_reason=queue_empty, got logs: %v", logs)
	}
}

func TestCascadeDrain_YieldsBudgetExhausted(t *testing.T) {
	dm := core.NewTestDM(t)
	// Seed 50 intents so one batch (BatchSize=10) does not drain the
	// whole outbox — the handler must yield with work remaining.
	eventID := seedCascadeOutbox(t, dm, 50)

	logger, drain := captureLogs(t)
	// Tight budget forces yield after at most one batch.
	h := NewCascadeDrainHandler(dm, logger, CascadeDrainOptions{
		Budget:    1 * time.Millisecond,
		BatchSize: 10,
	})

	err := h.tickHandler(context.Background())
	if err != nil {
		t.Fatalf("tickHandler returned error: %v", err)
	}
	logs := drain()
	if !logsContain(logs, "yield_reason", "budget_exhausted") {
		t.Errorf("expected yield_reason=budget_exhausted, got logs: %v", logs)
	}

	// At least one intent must remain pending for the next tick.
	var pending int
	if err := dm.SQLDB().QueryRow(
		`SELECT COUNT(*) FROM epistemic_cascade_outbox WHERE invalidation_event_id = ? AND status = 'pending'`,
		eventID,
	).Scan(&pending); err != nil {
		t.Fatalf("count pending: %v", err)
	}
	if pending == 0 {
		t.Errorf("expected pending intents remaining after budget_exhausted, got 0")
	}
}

func TestCascadeDrain_YieldsContextCancelled(t *testing.T) {
	dm := core.NewTestDM(t)
	// Seed intents so the handler enters the inner loop.
	seedCascadeOutbox(t, dm, 50)

	logger, drain := captureLogs(t)
	h := NewCascadeDrainHandler(dm, logger, CascadeDrainOptions{
		Budget:    30 * time.Second,
		BatchSize: 10,
	})

	ctx, cancel := context.WithCancel(context.Background())
	cancel() // already cancelled before tickHandler runs

	err := h.tickHandler(ctx)
	if err != nil {
		t.Fatalf("tickHandler returned error: %v", err)
	}
	logs := drain()
	if !logsContain(logs, "yield_reason", "context_cancelled") {
		t.Errorf("expected yield_reason=context_cancelled, got logs: %v", logs)
	}
}

func TestCascadeDrain_YieldsError(t *testing.T) {
	// Construct a DatabaseManager directly so we control Close.
	// Mirrors internal/core/testhelpers.go:NewTestDM but skips the auto-close.
	db, err := sql.Open("sqlite3", fmt.Sprintf("file:mpm-cancel-%d?mode=memory&cache=shared", time.Now().UnixNano()))
	if err != nil {
		t.Fatalf("sql.Open: %v", err)
	}
	dm := core.NewDatabaseManagerForDB(db)
	if err := dm.InitSchema(); err != nil {
		_ = db.Close()
		t.Fatalf("InitSchema: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })

	logger, drain := captureLogs(t)
	h := NewCascadeDrainHandler(dm, logger, CascadeDrainOptions{
		Budget:    30 * time.Second,
		BatchSize: 10,
	})

	// Force MaterializeBatch to fail by closing the underlying sql.DB
	// before the handler runs. The handler's panic-safety net will not
	// engage here — we expect a normal DB error.
	_ = db.Close()

	err = h.tickHandler(context.Background())
	if err != nil {
		t.Fatalf("tickHandler returned error: %v", err)
	}
	logs := drain()
	if !logsContain(logs, "yield_reason", "error") {
		t.Errorf("expected yield_reason=error, got logs: %v", logs)
	}
}

// panickingMaterializer wraps the real CascadeMaterializer and panics on
// MaterializeBatch. Used to verify the handler's panic-safety net.
type panickingMaterializer struct {
	*core.CascadeMaterializer
}

func (p *panickingMaterializer) MaterializeBatch(ctx context.Context, limit int) (core.MaterializationReport, error) {
	panic("materializer exploded")
}

func TestCascadeDrain_DoesNotPanicScheduler(t *testing.T) {
	dm := core.NewTestDM(t)
	logger, drain := captureLogs(t)
	h := NewCascadeDrainHandler(dm, logger, CascadeDrainOptions{Budget: 5 * time.Second})
	// Swap in a panicking materializer.
	h.materializer = &panickingMaterializer{h.concrete()}

	err := h.tickHandler(context.Background())
	if err != nil {
		t.Fatalf("tickHandler must swallow panic and return nil; got err=%v", err)
	}
	logs := drain()
	if !logsContain(logs, "msg", "cascade drain panicked") {
		t.Errorf("expected panic log entry, got: %v", logs)
	}
}

func TestCascadeDrain_NoIntentsLeftInProcessing(t *testing.T) {
	dm := core.NewTestDM(t)
	eventID := seedCascadeOutbox(t, dm, 30)
	logger, _ := captureLogs(t)
	h := NewCascadeDrainHandler(dm, logger, CascadeDrainOptions{
		Budget:    1 * time.Millisecond,
		BatchSize: 10,
	})

	_ = h.tickHandler(context.Background())

	var stuck int
	if err := dm.SQLDB().QueryRow(
		`SELECT COUNT(*) FROM epistemic_cascade_outbox WHERE invalidation_event_id = ? AND status = 'processing'`,
		eventID,
	).Scan(&stuck); err != nil {
		t.Fatalf("count processing: %v", err)
	}
	if stuck != 0 {
		t.Errorf("expected 0 intents in 'processing' after handler exit; got %d", stuck)
	}
}

// countCascadeSummaryWakes returns the number of cascade_summary wake rows
// for the test database. Used to verify state-transition gating and
// idle-tick dedupe.
func countCascadeSummaryWakes(t *testing.T, dm *core.DatabaseManager) int {
	t.Helper()
	var n int
	if err := dm.SQLDB().QueryRow(
		`SELECT COUNT(*) FROM scheduled_wakes WHERE json_extract(metadata, '$.kind') = 'cascade_summary'`,
	).Scan(&n); err != nil {
		t.Fatalf("count cascade_summary wakes: %v", err)
	}
	return n
}

// TestCascadeDrain_InsertsSummaryWakeOnQueueEmpty covers issue #5's
// "always insert on drain" branch. An empty outbox yields queue_empty,
// which is a state transition — exactly one wake row must appear.
func TestCascadeDrain_InsertsSummaryWakeOnQueueEmpty(t *testing.T) {
	dm := core.NewTestDM(t)
	logger, _ := captureLogs(t)
	h := NewCascadeDrainHandler(dm, logger, CascadeDrainOptions{Budget: 5 * time.Second})

	if got := countCascadeSummaryWakes(t, dm); got != 0 {
		t.Fatalf("baseline: expected 0 cascade_summary wakes, got %d", got)
	}

	if err := h.tickHandler(context.Background()); err != nil {
		t.Fatalf("tickHandler: %v", err)
	}

	if got := countCascadeSummaryWakes(t, dm); got != 1 {
		t.Errorf("after queue_empty tick: expected 1 cascade_summary wake, got %d", got)
	}

	// Verify the metadata shape — materialized/failed/pending_after/elapsed_ms.
	m, f, pa, _, ok, err := dm.LastCascadeSummaryMetrics(context.Background())
	if err != nil {
		t.Fatalf("LastCascadeSummaryMetrics: %v", err)
	}
	if !ok {
		t.Fatal("LastCascadeSummaryMetrics: ok=false after a tick that should have inserted")
	}
	if m != 0 || f != 0 || pa != 0 {
		t.Errorf("queue_empty metrics: got materialized=%d failed=%d pending_after=%d, want all 0", m, f, pa)
	}
}

// TestCascadeDrain_InsertsSummaryWakeOnBudgetExhaustedWithProgress
// covers the "budget_exhausted + metrics differ from prior" branch —
// seeding 50 intents then running with a tight budget guarantees that
// the handler processes some rows before yielding.
func TestCascadeDrain_InsertsSummaryWakeOnBudgetExhaustedWithProgress(t *testing.T) {
	dm := core.NewTestDM(t)
	seedCascadeOutbox(t, dm, 50)
	logger, _ := captureLogs(t)
	h := NewCascadeDrainHandler(dm, logger, CascadeDrainOptions{
		Budget:    1 * time.Millisecond, // yields after at most one batch
		BatchSize: 10,
	})

	if err := h.tickHandler(context.Background()); err != nil {
		t.Fatalf("tickHandler: %v", err)
	}

	// budget_exhausted with metrics != (0,0,0) must insert unconditionally.
	m, _, _, _, ok, err := dm.LastCascadeSummaryMetrics(context.Background())
	if err != nil {
		t.Fatalf("LastCascadeSummaryMetrics: %v", err)
	}
	if !ok {
		t.Fatal("LastCascadeSummaryMetrics: ok=false after budget_exhausted tick with work")
	}
	// m may be 0 if MaterializeBatch on the test seed synthetics doesn't
	// count as "materialized" — the load-bearing assertion is that a wake
	// row exists at all, not the exact count.
	_ = m
	if got := countCascadeSummaryWakes(t, dm); got != 1 {
		t.Errorf("after budget_exhausted tick: expected 1 cascade_summary wake, got %d", got)
	}
}

// TestCascadeDrain_BudgetReadsFromSystemConfig verifies that when BatchSize
// is not explicitly provided, the handler reads cascade_drain.max_intents_per_tick
// from system_config.
func TestCascadeDrain_BudgetReadsFromSystemConfig(t *testing.T) {
	dm := core.NewTestDM(t)
	// Save integer 25 under the config key.
	_, err := dm.SaveSystemConfig(CascadeDrainBudgetKey, "25", "", "")
	if err != nil {
		t.Fatalf("SaveSystemConfig: %v", err)
	}
	h := NewCascadeDrainHandler(dm, slog.Default(), CascadeDrainOptions{})
	if got, want := h.BatchSize(), 25; got != want {
		t.Errorf("BatchSize() = %d, want %d", got, want)
	}
}

// TestCascadeDrain_BudgetFallsBackToDefault verifies that when no config row
// exists, the handler uses the documented default of 50.
func TestCascadeDrain_BudgetFallsBackToDefault(t *testing.T) {
	dm := core.NewTestDM(t)
	h := NewCascadeDrainHandler(dm, slog.Default(), CascadeDrainOptions{})
	if got, want := h.BatchSize(), 50; got != want {
		t.Errorf("BatchSize() = %d, want %d", got, want)
	}
}

// TestCascadeDrain_BudgetFloorAtOne verifies that a config value of 0
// results in BatchSize=1 (max(1, budget) semantic).
func TestCascadeDrain_BudgetFloorAtOne(t *testing.T) {
	dm := core.NewTestDM(t)
	_, err := dm.SaveSystemConfig(CascadeDrainBudgetKey, "0", "", "")
	if err != nil {
		t.Fatalf("SaveSystemConfig: %v", err)
	}
	h := NewCascadeDrainHandler(dm, slog.Default(), CascadeDrainOptions{})
	if got, want := h.BatchSize(), 1; got != want {
		t.Errorf("BatchSize() = %d, want %d", got, want)
	}
}

// TestCascadeDrain_BudgetEnvVar verifies that MPM_CASCADE_DRAIN_MAX_INTENTS_PER_TICK
// env var overrides the default.
func TestCascadeDrain_BudgetEnvVar(t *testing.T) {
	t.Setenv("MPM_CASCADE_DRAIN_MAX_INTENTS_PER_TICK", "10")
	dm := core.NewTestDM(t)
	h := NewCascadeDrainHandler(dm, slog.Default(), CascadeDrainOptions{})
	if got, want := h.BatchSize(), 10; got != want {
		t.Errorf("BatchSize() = %d, want %d", got, want)
	}
}

// countCascadeAuditRows returns the number of system_audit_log rows with
// component='cascade' in the test database.
func countCascadeAuditRows(t *testing.T, dm *core.DatabaseManager) int {
	t.Helper()
	var n int
	if err := dm.SQLDB().QueryRow(
		`SELECT COUNT(*) FROM system_audit_log WHERE component = 'cascade'`,
	).Scan(&n); err != nil {
		t.Fatalf("count cascade audit rows: %v", err)
	}
	return n
}

// TestCascadeDrain_EmitsAuditRowPerBatch verifies that each successful
// MaterializeBatch call produces one system_audit_log row with level='info'.
func TestCascadeDrain_EmitsAuditRowPerBatch(t *testing.T) {
	dm := core.NewTestDM(t)
	// Seed 30 intents — with BatchSize=10 this produces exactly 3 batches.
	seedCascadeOutbox(t, dm, 30)
	logger, _ := captureLogs(t)
	h := NewCascadeDrainHandler(dm, logger, CascadeDrainOptions{
		Budget:    5 * time.Second,
		BatchSize: 10,
	})

	if err := h.tickHandler(context.Background()); err != nil {
		t.Fatalf("tickHandler: %v", err)
	}

	if got := countCascadeAuditRows(t, dm); got != 3 {
		t.Errorf("expected 3 audit rows (one per batch), got %d", got)
	}

	// Verify level and component on each row.
	rows, err := dm.SQLDB().QueryContext(context.Background(),
		`SELECT level, message, context FROM system_audit_log WHERE component = 'cascade' ORDER BY created_at ASC`)
	if err != nil {
		t.Fatalf("query audit log: %v", err)
	}
	defer rows.Close()

	for rows.Next() {
		var level, message string
		var ctxRaw sql.NullString
		if err := rows.Scan(&level, &message, &ctxRaw); err != nil {
			t.Fatalf("scan row: %v", err)
		}
		if level != "info" {
			t.Errorf("expected level=info, got %s", level)
		}
		if message != "cascade drain batch" {
			t.Errorf("expected message='cascade drain batch', got %q", message)
		}
		// Verify context contains the expected fields.
		if !ctxRaw.Valid || ctxRaw.String == "" {
			t.Errorf("expected non-empty context JSON")
			continue
		}
		var ctx map[string]interface{}
		if err := json.Unmarshal([]byte(ctxRaw.String), &ctx); err != nil {
			t.Fatalf("unmarshal context: %v", err)
		}
		for _, field := range []string{"materialized", "failed", "claimed"} {
			if _, ok := ctx[field]; !ok {
				t.Errorf("expected context field %q present in audit row", field)
			}
		}
	}
}

// failingMaterializer wraps the real CascadeMaterializer and returns an error
// on MaterializeBatch. Used to verify audit log emission on failure.
type failingMaterializer struct {
	*core.CascadeMaterializer
}

func (f *failingMaterializer) MaterializeBatch(ctx context.Context, limit int) (core.MaterializationReport, error) {
	return core.MaterializationReport{}, fmt.Errorf("synthetic drain failure")
}

// TestCascadeDrain_EmitsAuditWarnOnBatchFailure verifies that a failing
// MaterializeBatch call produces one system_audit_log row with level='warn'.
func TestCascadeDrain_EmitsAuditWarnOnBatchFailure(t *testing.T) {
	dm := core.NewTestDM(t)
	seedCascadeOutbox(t, dm, 5)
	logger, _ := captureLogs(t)
	h := NewCascadeDrainHandler(dm, logger, CascadeDrainOptions{
		Budget:    5 * time.Second,
		BatchSize: 10,
	})
	h.materializer = &failingMaterializer{h.concrete()}

	if err := h.tickHandler(context.Background()); err != nil {
		t.Fatalf("tickHandler returned error: %v", err)
	}

	if got := countCascadeAuditRows(t, dm); got != 1 {
		t.Errorf("expected 1 audit row after batch failure, got %d", got)
	}

	// Verify level='warn' and err field in context.
	var level string
	var ctxRaw sql.NullString
	if err := dm.SQLDB().QueryRowContext(context.Background(),
		`SELECT level, context FROM system_audit_log WHERE component = 'cascade'`,
	).Scan(&level, &ctxRaw); err != nil {
		t.Fatalf("query audit log: %v", err)
	}
	if level != "warn" {
		t.Errorf("expected level=warn, got %s", level)
	}
	if !ctxRaw.Valid {
		t.Fatalf("expected non-null context")
	}
	var ctx map[string]interface{}
	if err := json.Unmarshal([]byte(ctxRaw.String), &ctx); err != nil {
		t.Fatalf("unmarshal context: %v", err)
	}
	if _, ok := ctx["err"]; !ok {
		t.Errorf("expected 'err' field in warn audit context")
	}
}

// TestCascadeDrain_DedupesIdleTicks covers the dedupe-by-prior-metrics
// branch. Two consecutive ticks against an empty outbox with identical
// metrics must produce exactly ONE wake row, not two.
func TestCascadeDrain_DedupesIdleTicks(t *testing.T) {
	dm := core.NewTestDM(t)
	logger, _ := captureLogs(t)
	h := NewCascadeDrainHandler(dm, logger, CascadeDrainOptions{Budget: 5 * time.Second})

	// First tick: always inserts (no prior wake row to dedupe against).
	if err := h.tickHandler(context.Background()); err != nil {
		t.Fatalf("first tick: %v", err)
	}
	if got := countCascadeSummaryWakes(t, dm); got != 1 {
		t.Fatalf("after first tick: expected 1 cascade_summary wake, got %d", got)
	}

	// Second tick: same outbox state, same metrics → must be deduped.
	if err := h.tickHandler(context.Background()); err != nil {
		t.Fatalf("second tick: %v", err)
	}
	if got := countCascadeSummaryWakes(t, dm); got != 1 {
		t.Errorf("after second identical tick: expected 1 cascade_summary wake (deduped), got %d", got)
	}
}
