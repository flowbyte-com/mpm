// Package scheduler — cascade_drain.go: per-tick cascade drain handler.
//
// Cascade draining is owned exclusively by mpm-scheduler. The CascadeDrainHandler
// runs on every Tick under a per-tick time budget. When the budget runs out,
// the handler yields so the next tick fires on schedule. A pure
// CascadeMaterializer (no clock, no goroutines) is reused for the
// handler's lifetime and invoked once per inner-loop iteration.
//
// See cascade_drain.go's Run/TickHandler/logYield (Task 8) for the actual
// inner loop and yield-reason taxonomy.
package scheduler

import (
	"context"
	"log/slog"
	"runtime/debug"
	"time"

	core "github.com/flowbyte-com/mpm-core"
)

// CascadeDrainBudgetKey is the system_config key for the per-tick
// intent budget. Default 50.
const CascadeDrainBudgetKey = "cascade_drain.max_intents_per_tick"

// CascadeDrainOptions configures the cascade drain handler.
type CascadeDrainOptions struct {
	// Budget is the maximum wall-clock time the handler may spend
	// processing cascade intents per tick. Default 30s.
	Budget time.Duration
	// BatchSize is the per-iteration claim size passed to MaterializeBatch.
	// Default 10.
	BatchSize int
}

// CascadeDrainHandler drains the cascade outbox within a per-tick budget.
type CascadeDrainHandler struct {
	dm           *core.DatabaseManager
	logger       *slog.Logger
	budget       time.Duration
	batchSize    int
	materializer materializer   // interface; production uses concrete impl
	impl         *core.CascadeMaterializer // backing concrete; accessed via materializer()
}

// materializer is the surface CascadeDrainHandler needs from the
// materializer. Defined locally so tests can inject panicking or
// otherwise-malformed implementations.
type materializer interface {
	MaterializeBatch(ctx context.Context, limit int) (core.MaterializationReport, error)
}

// concrete returns the concrete *core.CascadeMaterializer so tests can
// wrap it with embed-and-override. The returned value is the same one the
// handler uses internally.
func (h *CascadeDrainHandler) concrete() *core.CascadeMaterializer {
	return h.impl
}

// NewCascadeDrainHandler constructs a handler bound to the supplied
// DatabaseManager. Defaults: Budget=30s, BatchSize=10.
//
// The materializer is constructed once here and reused for the handler's
// lifetime. It carries no mutable state beyond the dm reference, so this
// is safe across goroutines (though the handler is intended to be called
// sequentially by the scheduler tick).
func NewCascadeDrainHandler(dm *core.DatabaseManager, logger *slog.Logger, opts CascadeDrainOptions) *CascadeDrainHandler {
	if opts.Budget <= 0 {
		opts.Budget = 30 * time.Second
	}
	if opts.BatchSize <= 0 {
		opts.BatchSize = dm.GetConfigInt(CascadeDrainBudgetKey, 50)
	}
	if opts.BatchSize < 1 {
		opts.BatchSize = 1
	}
	mat := dm.NewCascadeMaterializer(core.CascadeMaterializerOptions{
		MaxCascadeDepth: 3,
		MaxRetries:      3,
		WakeDelay:       1 * time.Second,
	})
	return &CascadeDrainHandler{
		dm:        dm,
		logger:    logger,
		budget:    opts.Budget,
		batchSize: opts.BatchSize,
		impl:      mat,
		materializer: mat, // satisfies materializer interface
	}
}

// Budget returns the configured per-tick budget.
func (h *CascadeDrainHandler) Budget() time.Duration { return h.budget }

// BatchSize returns the configured per-iteration claim size.
func (h *CascadeDrainHandler) BatchSize() int { return h.batchSize }

// Run is a HandlerFunc-compatible adapter. Currently unused — the handler
// is registered via Scheduler.RegisterTickHandler — but kept for future
// flexibility.
//
// F-3: forwards the dispatch ctx so HandlerFunc-style invocations honour
// cancellation the same way TickHandler invocations do.
func (h *CascadeDrainHandler) Run(ctx context.Context, w Wake) error {
	return h.tickHandler(ctx)
}

// TickHandler returns a function suitable for Scheduler.RegisterTickHandler.
// The function runs the inner drain loop under a per-call time budget.
func (h *CascadeDrainHandler) TickHandler() func(ctx context.Context) error {
	return h.tickHandler
}

// tickHandler drains the cascade outbox within a per-call time budget.
// On every iteration it checks (in order): context cancellation, deadline
// exhaustion, batch result. The first time any of those triggers, it
// emits a single "cascade drain yielded" log line with the yield_reason
// and returns nil.
//
// A panic in the materializer is caught and logged; the scheduler
// must never die from a cascade drain failure.
func (h *CascadeDrainHandler) tickHandler(ctx context.Context) (err error) {
	defer func() {
		if r := recover(); r != nil {
			err = nil // never propagate; keep scheduler alive
			h.logger.Error("cascade drain panicked",
				"panic", r,
				"stack", string(debug.Stack()),
			)
		}
	}()

	start := time.Now()
	deadline := start.Add(h.budget)
	var totalProcessed, totalFailed int

	for {
		if ctxErr := ctx.Err(); ctxErr != nil {
			h.logYield(ctx, totalProcessed, totalFailed, "context_cancelled", time.Since(start))
			return nil
		}
		if time.Now().After(deadline) {
			h.logYield(ctx, totalProcessed, totalFailed, "budget_exhausted", time.Since(start))
			return nil
		}

		report, err := h.materializer.MaterializeBatch(ctx, h.batchSize)
		if err != nil {
			h.dm.LogAudit(core.AuditWarn, "cascade", "cascade drain batch failed", "", core.AuditContext{
				"yield_reason": "error",
				"err":          err.Error(),
			})
			h.logger.Error("cascade drain batch failed", "err", err)
			h.logYield(ctx, totalProcessed, totalFailed, "error", time.Since(start))
			return nil
		}
		totalProcessed += report.Materialized
		totalFailed += report.Failed

		// Only emit audit row for batches that did actual work.
		if report.Claimed > 0 {
			h.dm.LogAudit(core.AuditInfo, "cascade", "cascade drain batch", "", core.AuditContext{
				"claimed":              report.Claimed,
				"processed":            report.Processed,
				"materialized":         report.Materialized,
				"failed":               report.Failed,
				"skipped":              report.Skipped,
				"suppressed":           report.Suppressed,
				"batch_size":           h.batchSize,
				"budget_remaining_ms":  time.Until(deadline).Milliseconds(),
			})
		}

		if report.Claimed == 0 {
			h.logYield(ctx, totalProcessed, totalFailed, "queue_empty", time.Since(start))
			return nil
		}
	}
}

// logYield emits the structured log line that records the handler's exit
// AND, on state transitions, inserts a cascade_summary wake row that the
// scheduler's CascadeSummaryHandler will log on the next tick.
//
// yield_reason is the operationally-important field: queue_empty means
// normal exit, budget_exhausted means the per-tick ceiling was hit,
// context_cancelled means the scheduler shut down, error means a DB-level
// failure during a batch.
//
// State-transition policy (issue #5): insert a wake row whenever the
// tick produced non-zero materializations or failures — those are
// always worth surfacing. Steady-state zero ticks (queue_empty with no
// failures, etc.) are deduped by exact metric match against the prior
// cascade_summary wake row: only insert if materialized/failed/
// pending_after/elapsed_ms differ from the most recent. This keeps
// scheduled_wakes from filling with identical zero-rows on every idle
// tick while preserving the forensic trail when something changes.
func (h *CascadeDrainHandler) logYield(ctx context.Context, processed, failed int, reason string, elapsed time.Duration) {
	h.logger.Info("cascade drain yielded",
		"yield_reason", reason,
		"intents_materialized", processed,
		"intents_failed", failed,
		"elapsed_ms", elapsed.Milliseconds(),
		"budget_ms", h.budget.Milliseconds(),
	)

	// Compute pending_after from the outbox — cheap (indexed COUNT).
	// On DB error, fall back to 0 so the wake still gets a row written.
	pendingAfter := 0
	if h.dm != nil && h.dm.SQLDB() != nil {
		var n int
		if err := h.dm.SQLDB().QueryRowContext(ctx,
			`SELECT COUNT(*) FROM epistemic_cascade_outbox WHERE status = 'pending'`).Scan(&n); err != nil {
			h.logger.Warn("cascade drain: pending_after query failed; defaulting to 0", "err", err)
		} else {
			pendingAfter = n
		}
	}

	insert := false
	if processed > 0 || failed > 0 {
		// Any non-zero materialization or failure is a state transition —
		// always emit. The user's intent (issue #5): "drained OR failed>0".
		// queue_empty with no failures IS the steady state; see the else
		// branch for the dedupe.
		insert = true
	} else {
		// Steady-state zero tick — dedupe by comparing the four metrics
		// to the most recent cascade_summary wake. If they're identical,
		// there's nothing new for an operator to look at; skip the row.
		prevM, prevF, prevPa, prevMs, ok, err := h.dm.LastCascadeSummaryMetrics(ctx)
		if err != nil {
			h.logger.Warn("cascade drain: dedupe lookup failed; inserting wake anyway", "err", err)
			insert = true
		} else if !ok {
			// No prior wake exists; first tick of a fresh scheduler run.
			insert = true
		} else if prevM != processed || prevF != failed || prevPa != pendingAfter || prevMs != elapsed.Milliseconds() {
			insert = true
		}
	}

	if !insert {
		return
	}
	if err := h.dm.ScheduleCascadeSummaryWake(ctx, processed, failed, pendingAfter, elapsed); err != nil {
		// Don't propagate — the wake row is a forensic affordance, not
		// a correctness mechanism. Log and move on.
		h.logger.Warn("cascade drain: failed to insert cascade_summary wake", "err", err)
	}
}
