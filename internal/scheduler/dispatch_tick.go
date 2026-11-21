// dispatch_tick.go — the scheduler's opt-in dispatch tick.
//
// # WHAT THIS IS
//
// The scheduler may run ONE bounded dispatch pass on a tick. It is the
// SAME engine the operator's CLI runs — same configuration, same
// selection, same fences — so a tree behaves identically whether the
// operator asked for the pass or the scheduler found time for one.
//
// # WHAT THIS IS NOT
//
// It is not a daemon, and it is not a new one. The scheduler already
// exists and keeps its own cadence; this adds a handler to it, and the
// handler is INERT unless an operator has enabled the dispatcher.
//
// # THE INERTNESS IS THE POINT
//
// An installed scheduler must never dispatch on its own. The handler
// reads the operator's configuration file on every tick:
//
//   - the file is MISSING -> the dispatcher is off -> the handler does
//     nothing at all, and it does not even open a runtime;
//   - the file is PRESENT and says enabled -> the handler runs one
//     bounded pass;
//   - the file is PRESENT and cannot be understood -> the handler logs
//     the fault and dispatches nothing. A broken configuration is not a
//     reason to guess.
//
// # WHY THE FAULT IS LOGGED AND NOT RETURNED
//
// The scheduler logs a tick handler's error and carries on to the next
// handler, so a broken dispatch configuration cannot stop the rest of
// the scheduler's work. The fault is still VISIBLE: it is in the log.

package scheduler

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"

	core "github.com/flowbyte-com/mpm-core"
	"github.com/flowbyte-com/mpm-core/assignment"
	"github.com/flowbyte-com/mpm-core/config"
	"github.com/flowbyte-com/mpm-core/dispatch"
)

// DispatchConfigEnvVar names the dispatcher's configuration file.
//
// It exists so an operator can point the scheduler at a configuration
// file that is NOT the default one — for instance while testing an
// enablement before letting the default file go live. It is an
// operator-set variable: nothing in MPM sets it.
const DispatchConfigEnvVar = "MPM_DISPATCH_CONFIG"

// dispatchConfigFileName is the configuration file's name inside the MPM
// directory.
const dispatchConfigFileName = "dispatch.json"

// DispatchTickHandler runs one bounded dispatch pass on a scheduler tick.
type DispatchTickHandler struct {
	dm     *core.DatabaseManager
	logger *slog.Logger
	// configPath is where the configuration is read from. It is read on
	// every tick, so an operator can enable or disable the dispatcher
	// without restarting the scheduler.
	configPath string
}

// NewDispatchTickHandler constructs the handler.
func NewDispatchTickHandler(dm *core.DatabaseManager, logger *slog.Logger) *DispatchTickHandler {
	if logger == nil {
		logger = slog.New(slog.NewTextHandler(os.Stderr, nil))
	}
	return &DispatchTickHandler{
		dm:         dm,
		logger:     logger,
		configPath: dispatchConfigPath(),
	}
}

// dispatchConfigPath is where the handler reads the configuration.
//
// The environment variable wins, so an operator can test an enablement
// against a file that is not the default one. Otherwise it is the
// default file inside the MPM directory.
func dispatchConfigPath() string {
	if p := os.Getenv(DispatchConfigEnvVar); p != "" {
		return p
	}
	return filepath.Join(config.GetMPMDir(), dispatchConfigFileName)
}

// TickHandler returns the function the scheduler registers.
//
// The returned function never returns an error: a dispatch fault is
// logged, and the scheduler's other handlers are unaffected. It returns
// an error only for the cases where an error carries information the
// scheduler should log at a different level — and today there are none.
func (h *DispatchTickHandler) TickHandler() func(ctx context.Context) error {
	return func(ctx context.Context) error {
		if err := h.run(ctx); err != nil {
			h.logger.Error("the dispatch tick failed",
				"reason", err.Error())
		}
		return nil
	}
}

// run performs ONE bounded dispatch pass, if the dispatcher is enabled.
func (h *DispatchTickHandler) run(ctx context.Context) error {
	cfg, err := dispatch.LoadConfig(h.configPath)
	if err != nil {
		return fmt.Errorf("the dispatcher configuration at %s is not usable: %w",
			h.configPath, err)
	}
	if !cfg.Enabled {
		// OFF MEANS OFF. The handler returns before it builds
		// anything: no runtime, no session, no row read.
		return nil
	}

	factory, err := dispatch.NewOpenCodeFactory(cfg)
	if err != nil {
		return err
	}
	defer factory.Close()

	engine, err := dispatch.NewEngine(assignment.NewStore(h.dm), cfg, factory.Factory())
	if err != nil {
		return err
	}
	report, err := engine.RunOnce(ctx)
	if err != nil {
		return err
	}
	// EVERY COUNT IS LOGGED. A count the log omits is a count the
	// operator cannot find in the scheduler's own log — and the
	// scheduler's log is where an operator looks when they did not run
	// the pass themselves.
	h.logger.Info("the dispatch pass finished",
		"dispatched", report.Dispatched,
		"completed", report.Completed,
		"failed", report.Failed,
		"refused", report.Refused,
		"deferred", report.Deferred,
		"ambiguous", report.Ambiguous)
	return nil
}
