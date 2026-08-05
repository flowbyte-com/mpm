package scheduler

import (
	"context"
	"database/sql"
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
	if got, want := h.BatchSize(), 10; got != want {
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
