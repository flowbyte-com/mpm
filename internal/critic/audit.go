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
type Finding struct {
	Tool     string                 // mpm tool name, e.g. "save_lesson"
	Reason   string                 // human-readable description, used in logs
	Payload  map[string]interface{} // tool-specific args
	Priority int                    // 0=low (informational), 1=medium, 2=high
}

// CLIRunner abstracts the mpm CLI invocation. Production uses ExecCLI
// (real subprocess); tests can substitute a fake.
type CLIRunner interface {
	Call(ctx context.Context, tool string, payload map[string]interface{}) error
}

// ExecCLI invokes `mpm call <tool> --payload <json>` as a subprocess.
// Errors include the tool name and any stderr output for diagnostics.
type ExecCLI struct {
	MPMPath string        // path to the mpm binary (default: "mpm")
	Timeout time.Duration // per-call timeout (default: 30s)
}

// Call runs the mpm call synchronously and returns an error on non-zero
// exit or stderr output.
func (c *ExecCLI) Call(ctx context.Context, tool string, payload map[string]interface{}) error {
	mpm := c.MPMPath
	if mpm == "" {
		mpm = "mpm"
	}
	jsonPayload, err := json.Marshal(payload)
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
	db    *sql.DB
	log   *slog.Logger
	cycle int
	hunts []Hunt
	cli   CLIRunner
}

// New returns an Audit ready to Run. The hunts list is initialized with
// the three primary hunters; poison pill is gated on cycle % 5 inside
// Run, not registered as a separate hunt.
func New(dbPath string, log *slog.Logger) (*Audit, error) {
	if log == nil {
		log = slog.Default()
	}
	db, err := sql.Open("sqlite3", dbPath+"?_busy_timeout=5000&_journal_mode=WAL&_synchronous=NORMAL")
	if err != nil {
		return nil, fmt.Errorf("open db: %w", err)
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

// Close releases the database connection.
func (a *Audit) Close() error {
	if a.db == nil {
		return nil
	}
	return a.db.Close()
}

// Run executes one full audit cycle. Each hunt runs sequentially;
// findings are batched and emitted via the CLI in priority order.
// The cycle counter is incremented at the end.
func (a *Audit) Run(ctx context.Context) error {
	a.cycle++
	cycleStart := time.Now()
	a.log.Info("critic audit cycle starting", "cycle", a.cycle)

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
		if err := a.cli.Call(ctx, f.Tool, f.Payload); err != nil {
			a.log.Error("emit finding failed",
				"tool", f.Tool,
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
		"duration", time.Since(cycleStart).String())
	return nil
}

// DB returns the audit's database handle. Hunts use this for direct
// queries against MPM tables. The handle should not be used to write —
// all writes go through cli.Call.
func (a *Audit) DB() *sql.DB { return a.db }

// Cycle returns the current cycle number. Useful in test assertions and
// for hunts that need to be cycle-aware.
func (a *Audit) Cycle() int { return a.cycle }
