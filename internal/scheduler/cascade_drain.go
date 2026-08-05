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
	"log/slog"
	"time"

	core "github.com/flowbyte-com/mpm-core"
)

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
	materializer *core.CascadeMaterializer
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
		opts.BatchSize = 10
	}
	return &CascadeDrainHandler{
		dm:        dm,
		logger:    logger,
		budget:    opts.Budget,
		batchSize: opts.BatchSize,
		materializer: dm.NewCascadeMaterializer(core.CascadeMaterializerOptions{
			MaxCascadeDepth: 3,
			MaxRetries:      3,
			WakeDelay:       1 * time.Second,
		}),
	}
}

// Budget returns the configured per-tick budget.
func (h *CascadeDrainHandler) Budget() time.Duration { return h.budget }

// BatchSize returns the configured per-iteration claim size.
func (h *CascadeDrainHandler) BatchSize() int { return h.batchSize }
