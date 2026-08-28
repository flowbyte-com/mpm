// Package critic implements MPM's adversarial audit cycle.
//
// The Critic is a heuristic-driven, default-skeptical auditor that runs on
// a daily schedule (via mpm-scheduler). It pulls organic findings from
// MPM state — survival asymmetries, stale memories, weak theories — and
// every fifth cycle injects a manufactured counter-theory (Poison Pill)
// to exercise the inverse defense path.
//
// The Critic is not an LLM. It applies rules to numbers. "Strict linter
// for 808's logic, zero banter" was the design constraint. Each hunt
// reads the database, applies a rule, and emits a Finding; the audit
// orchestrator hands the finding off to the mpm CLI for write to MPM.
//
// Architecture reference: decision 90d1bccc0bbbfa79 (Critic MVP),
// 27d7b3c18199e098 (universal scheduler), lesson 52d3a18c9de1fb87
// (test-first methodology).
package critic

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"log/slog"
	"os/exec"
	"time"

	_ "github.com/mattn/go-sqlite3"
)

// Hunt is the unit of work in an audit cycle. Each hunt reads the
// database, applies a rule, and emits zero-or-more Findings.
type Hunt interface {
	Name() string
	Run(ctx context.Context, a *Audit) ([]Finding, error)
}

// Finding is a single piece of evidence the audit cycle wants to
// persist. The orchestrator calls cli.Call(Tool, Payload) to write it
// via the mpm CLI. The CLI is used (not direct mpm-core calls) so the
// Critic is decoupled from mpm-core's internal API surface.
//
// Tool and Action map to the current `mpm call` envelope: the call is
// `mpm call <Tool> --payload '{"action":"<Action>","params":<Payload>}'`.
// Keeping both fields on the Finding (rather than just the legacy
// flat-tool-name string) means a future rename from `mpm_memory` to
// `mpm_mind` or from `challenge` to `flag_stale` can't silently strand
// the critic again — TestCriticCanPublishFinding pins the contract.
type Finding struct {
	Tool     string                 // mpm tool name, e.g. "mpm_memory"
	Action   string                 // tool action, e.g. "challenge"
	Reason   string                 // human-readable description, used in logs
	Payload  map[string]interface{} // tool-specific params
	Priority int                    // 0=low (informational), 1=medium, 2=high
}

// CLIRunner abstracts the mpm CLI invocation. Production uses ExecCLI
// (real subprocess); tests can substitute a fake.
type CLIRunner interface {
	Call(ctx context.Context, tool, action string, payload map[string]interface{}) error
}

// ExecCLI invokes `mpm call <tool> --payload <json>` as a subprocess.
// Errors include the tool name and any stderr output for diagnostics.
type ExecCLI struct {
	MPMPath string        // path to the mpm binary (default: "mpm")
	Timeout time.Duration // per-call timeout (default: 30s)
}

// Call runs the mpm call synchronously and returns an error on non-zero
// exit or stderr output.
//
// The `action` and `params` arguments are wrapped into the current
// `mpm call` envelope:
//
//	mpm call <tool> --payload '{"action":"<action>","params":<params>}'
//
// Callers (hunts) populate Tool + Action on the Finding; this wrapper
// ensures the published payload matches what the live tool expects.
func (c *ExecCLI) Call(ctx context.Context, tool, action string, payload map[string]interface{}) error {
	mpm := c.MPMPath
	if mpm == "" {
		mpm = "mpm"
	}
	envelope := map[string]interface{}{
		"action": action,
		"params": payload,
	}
	jsonPayload, err := json.Marshal(envelope)
	if err != nil {
		return fmt.Errorf("marshal payload: %w", err)
	}
	args := []string{"call", tool, "--payload", string(jsonPayload)}

	cctx, cancel := context.WithTimeout(ctx, c.Timeout)
	defer cancel()

	cmd := exec.CommandContext(cctx, mpm, args...)
	out, err := cmd.CombinedOutput()
	if err != nil {
		return fmt.Errorf("mpm call %s: %s: %w", tool, string(out), err)
	}
	return nil
}

// Audit is the orchestrator. It owns the DB connection, the hunt
// registry, the cycle counter, and the CLI runner.
type Audit struct {
	db         *sql.DB
	log        *slog.Logger
	cycle      int
	cycleStart time.Time // captured at the start of each Run(); hunts read this for settling-period comparisons
	hunts      []Hunt
	cli        CLIRunner
}

// New returns an Audit bound to the given *sql.DB. The caller owns the
// database lifecycle (typically a *DatabaseManager from mpm-core) and
// is responsible for closing it.
//
// Architectural note: critic.New does NOT open or own the database.
// Connection lifecycle lives at the construction site so the
// DatabaseManager remains the singleton owner of *sql.DB. This is the
// F-007 fix — critic participates in DatabaseManager's connection
// management rather than bypassing it.
func New(db *sql.DB, log *slog.Logger) (*Audit, error) {
	if db == nil {
		return nil, fmt.Errorf("db is required (caller must construct via DatabaseManager)")
	}
	if log == nil {
		log = slog.Default()
	}
	a := &Audit{
		db:    db,
		log:   log,
		cycle: 0,
		cli:   &ExecCLI{Timeout: 30 * time.Second},
		hunts: []Hunt{
			&SurvivalAsymmetryHunt{},
			&StaleMemoryHunt{MaxAge: 30 * 24 * time.Hour},
			&WeakTheoryHunt{MaxConfidence: 0.6},
		},
	}
	return a, nil
}

// Close is a no-op on the audit's bound *sql.DB. The DatabaseManager
// owns connection lifecycle; close it at the construction site.
func (a *Audit) Close() error {
	return nil
}

// Run executes one full audit cycle. Each hunt runs sequentially;
// findings are batched and emitted via the CLI in priority order.
// The cycle counter is incremented at the end.
func (a *Audit) Run(ctx context.Context) error {
	a.cycle++
	a.cycleStart = time.Now()
	a.log.Info("critic audit cycle starting", "cycle", a.cycle, "cycle_start", a.cycleStart)

	var allFindings []Finding
	for _, h := range a.hunts {
		findings, err := h.Run(ctx, a)
		if err != nil {
			a.log.Error("hunt failed", "hunt", h.Name(), "cycle", a.cycle, "err", err)
			continue
		}
		allFindings = append(allFindings, findings...)
	}

	// Poison Pill: every 5th cycle, manufacture a counter-theory to test
	// the defense path. Skipped on cycle 0 (first run after init).
	if a.cycle > 0 && a.cycle%5 == 0 {
		findings, err := (&PoisonPillHunt{Cycle: a.cycle}).Run(ctx, a)
		if err != nil {
			a.log.Error("poison pill failed", "cycle", a.cycle, "err", err)
		} else {
			allFindings = append(allFindings, findings...)
		}
	}

	// Emit findings via CLI. Failures are logged but do not stop the cycle.
	emitted := 0
	for _, f := range allFindings {
		if err := a.cli.Call(ctx, f.Tool, f.Action, f.Payload); err != nil {
			a.log.Error("emit finding failed",
				"tool", f.Tool,
				"action", f.Action,
				"reason", f.Reason,
				"err", err)
			continue
		}
		emitted++
	}

	a.log.Info("critic audit cycle complete",
		"cycle", a.cycle,
		"hunts_run", len(a.hunts),
		"findings_total", len(allFindings),
		"findings_emitted", emitted,
		"findings_failed", len(allFindings)-emitted,
		"duration_ms", time.Since(a.cycleStart).Milliseconds())
	return nil
}

// DB returns the audit's database handle. Hunts use this for direct
// queries against MPM tables. The handle should not be used to write —
// all writes go through cli.Call.
func (a *Audit) DB() *sql.DB { return a.db }

// Cycle returns the current cycle number. Useful in test assertions and
// for hunts that need to be cycle-aware.
func (a *Audit) Cycle() int { return a.cycle }

// CycleStart returns the wall-clock at which the current (or most recent)
// audit cycle began. Hunts use this as the reference for settling-period
// calculations so the period doesn't tick against absolute time during a
// daemon outage — it ticks only when a cycle is actively running.
func (a *Audit) CycleStart() time.Time { return a.cycleStart }
