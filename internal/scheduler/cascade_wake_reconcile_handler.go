// Package scheduler — cascade_wake_reconcile_handler.go: per-tick
// recovery handler for cascade wakes lost to crash windows.
//
// Post-M3 audit H-3 (2026-08-31): scheduleCascadeWake was fire-and-
// forget pre-fix. A crash between markMaterialized and the wake insert
// left outbox rows in status='materialized' with no scheduled wake.
// The wake_scheduled column on the outbox provides the durable record;
// this handler scans for materialized rows with wake_scheduled=0 and
// re-schedules the wake via core.ReconcileUnscheduledCascadeWakes.
//
// Cadence: the handler runs on every scheduler tick (default 5s) but
// ReconcileUnscheduledCascadeWakes internally LIMITs to 100 rows per
// pass, so the per-tick work is bounded. The 60s recovery SLA emerges
// from the 5s tick × 100 rows/pass, which is well within tolerance for
// the loss-of-wake failure mode (a missed delivery, not a data-loss
// event).
package scheduler

import (
	"context"
	"log/slog"

	core "github.com/flowbyte-com/mpm-core"
)

// CascadeWakeReconcileHandler runs the wake-scheduling recovery pass
// on each tick. Constructed once at scheduler startup; the bound
// *core.DatabaseManager is the canonical source of truth.
type CascadeWakeReconcileHandler struct {
	dm     *core.DatabaseManager
	logger *slog.Logger
}

// NewCascadeWakeReconcileHandler constructs the handler.
func NewCascadeWakeReconcileHandler(dm *core.DatabaseManager, logger *slog.Logger) *CascadeWakeReconcileHandler {
	return &CascadeWakeReconcileHandler{dm: dm, logger: logger}
}

// TickHandler returns a function suitable for Scheduler.RegisterTickHandler.
// The closure captures the bound dm and logger so callers don't need to
// thread them through the scheduler dispatch table.
func (h *CascadeWakeReconcileHandler) TickHandler() func(ctx context.Context) error {
	return func(ctx context.Context) error {
		recovered, err := core.ReconcileUnscheduledCascadeWakes(ctx, h.dm)
		if err != nil {
			h.logger.Warn("cascade wake reconcile failed",
				"err", err.Error())
			// Return err so the scheduler logs it via its standard
			// handler-error path; the next tick will retry.
			return err
		}
		if recovered > 0 {
			h.logger.Info("cascade wake reconcile recovered",
				"count", recovered)
		}
		return nil
	}
}
