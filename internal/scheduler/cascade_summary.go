// Package scheduler — cascade_summary.go: handler for the cascade_summary
// wake kind (issue #5).
//
// CascadeDrainHandler inserts one cascade_summary wake row per
// state-transition tick (drained, failed>0, or any non-zero change from
// the prior wake). CascadeSummaryHandler consumes those wakes: it emits
// one structured log line per wake and returns nil so the scheduler
// marks the row fired.
//
// Why a registered handler instead of an unregistered notification:
// CheckPendingWakes caps unregistered (notification-kind) wakes at 3
// per call. cascade_summary wakes would backlog if the agent-side
// surface saw them. Registering the kind routes them through
// executeOne → this handler → MarkFired, keeping the scheduled_wakes
// table clean while preserving forensic value in scheduler logs.
//
// The handler is a closure because HandlerFunc takes no logger argument.
// Pass the same logger the scheduler was constructed with — captured
// here so test code can pass a buffer-backed slog handler for capture.
package scheduler

import (
	"log/slog"
)

// NewCascadeSummaryHandler returns a HandlerFunc that emits one
// structured "cascade summary" log line per cascade_summary wake. The
// metadata fields materialized/failed/pending_after/elapsed_ms are
// surfaced as separate log fields (not a single embedded JSON blob) so
// existing log parsers / grep recipes that target `materialized=...`
// keep working.
//
// Returns nil error unconditionally — this handler is a no-op for
// side-effect purposes. The wake row already carries the data; the
// scheduler's MarkFired (driven by the nil return) is the only state
// change required.
func NewCascadeSummaryHandler(logger *slog.Logger) HandlerFunc {
	if logger == nil {
		logger = slog.Default()
	}
	return func(w Wake) error {
		// json.Unmarshal into map[string]interface{} yields float64 for
		// all numbers — coerce defensively, since a missing or
		// mis-typed field must not crash the scheduler.
		materialized, _ := w.Metadata["materialized"].(float64)
		failed, _ := w.Metadata["failed"].(float64)
		pendingAfter, _ := w.Metadata["pending_after"].(float64)
		elapsedMs, _ := w.Metadata["elapsed_ms"].(float64)

		logger.Info("cascade summary",
			"wake_id", w.ID,
			"materialized", int(materialized),
			"failed", int(failed),
			"pending_after", int(pendingAfter),
			"elapsed_ms", int(elapsedMs),
		)
		return nil
	}
}
