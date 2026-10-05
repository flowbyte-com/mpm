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
	"errors"
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

// criticCycleKey is the system_config key owning the durable Critic
// cycle. mpm-critic runs as a one-shot process per scheduled audit
// (see CriticAuditHandler), so an in-memory counter on Audit restarts
// at zero on every invocation and the every-fifth-cycle Poison Pill
// can never fire. The counter therefore lives in system_config, which
// already exists as MPM's canonical bounded key/value state surface.
const criticCycleKey = "critic_cycle"

// ensureCycleStateTable creates the system_config table if absent.
//
// Production always initializes this through DatabaseManager, so this
// is normally a no-op. It exists so the Critic's durable state does
// not depend on which migration path opened the database, and so a
// critic pointed at a bare SQLite file behaves identically. The DDL is
// byte-identical to the canonical definition in internal/core/schema.go
// so the two cannot drift in shape.
const ensureCycleStateTable = `
CREATE TABLE IF NOT EXISTS system_config (
	key TEXT PRIMARY KEY,
	raw_json TEXT NOT NULL,
	content_hash TEXT NOT NULL,
	updated_at INTEGER DEFAULT (CAST(strftime('%s','now') AS INTEGER)),
	config_snapshot JSON
)`

// claimDurableCycle atomically advances the Critic cycle and returns the
// value this caller owns.
//
// CRASH SEMANTICS: a cycle counts a CLAIMED AUDIT ATTEMPT, not a
// successfully completed audit. The claim happens at the top of Run,
// before any hunt executes, so a process that crashes mid-audit has
// still consumed its cycle number. This is deliberate:
//
//   - uniqueness stays trivially atomic — one claim, one owner;
//   - a crash cannot replay the every-fifth-cycle Poison Pill on
//     retry, which would double-inject a manufactured counter-theory.
//
// The cost is that a crash burns a cycle, so a persistently crashing
// critic can skip a Poison Pill boundary. That is the safer failure:
// a skipped cycle is a missed injection, whereas a replayed one
// re-injects a counter-theory that must then be arbitrated.
//
// ATOMICITY: a single INSERT ... ON CONFLICT DO UPDATE ... RETURNING
// statement. SQLite executes one statement as one implicit transaction,
// so two concurrent callers serialize on the write lock and observe
// each other's committed value. They claim N and N+1, never N and N.
// This does not rely on the scheduler's singleton enforcement, which
// does not apply because mpm-critic is also a standalone binary that an
// operator can launch by hand.
//
// MALFORMED STATE: the DO UPDATE carries a WHERE guard requiring
// $.cycle to be a JSON integer that is non-negative. If the stored
// state is corrupt or missing that field, the guard is false, no row
// is written, and RETURNING yields no row — the claim fails closed
// rather than silently resetting the counter and re-issuing cycle
// numbers that may already have driven a Poison Pill.
//
// Fail-closed matrix for the stored $.cycle value:
//
//	missing / JSON null   -> no claim, run errors   (json_type NULL)
//	string ("5")          -> no claim, run errors   (json_type 'text')
//	real (5.5)            -> no claim, run errors   (json_type 'real')
//	negative (-1)         -> no claim, run errors   (guard rejects < 0)
//
// The guard is part of the statement rather than a post-read check so
// a rejected claim LEAVES THE ROW UNTOUCHED. A post-hoc check would
// still have let a negative value increment (-1 -> 0) before being
// rejected, silently repairing corrupt state as a side effect of
// trying to detect it.
//
// Stored cycle 0 IS valid and is treated as bootstrap: the first claim
// over it yields 1. That is the natural pre-first-cycle value and has
// no legacy producer — pre-tranche the counter was in-memory only, so
// no database ever persisted a 0 — but accepting it keeps a
// hand-seeded or partially-initialized row from wedging the Critic.
const claimDurableCycleStmt = `
INSERT INTO system_config (key, raw_json, content_hash)
VALUES ('` + criticCycleKey + `', '{"cycle":1}', '')
ON CONFLICT(key) DO UPDATE SET
	raw_json = json_set(system_config.raw_json, '$.cycle',
	                    json_extract(system_config.raw_json, '$.cycle') + 1),
	updated_at = CAST(strftime('%s','now') AS INTEGER)
WHERE json_type(system_config.raw_json, '$.cycle') = 'integer'
  AND json_extract(system_config.raw_json, '$.cycle') >= 0
RETURNING json_extract(raw_json, '$.cycle')`

// claimDurableCycle advances and returns the durable cycle.
func (a *Audit) claimDurableCycle(ctx context.Context) (int, error) {
	if _, err := a.db.ExecContext(ctx, ensureCycleStateTable); err != nil {
		return 0, fmt.Errorf("critic: ensure state table: %w", err)
	}

	var cycle int
	err := a.db.QueryRowContext(ctx, claimDurableCycleStmt).Scan(&cycle)
	if errors.Is(err, sql.ErrNoRows) {
		return 0, fmt.Errorf(
			"critic: durable cycle state under key %q is malformed (expected {\"cycle\":<integer>}); "+
				"refusing to reset the counter because that could silently resurrect cycle numbers "+
				"that already drove a poison pill. Inspect or repair the row manually",
			criticCycleKey)
	}
	if err != nil {
		return 0, fmt.Errorf("critic: claim durable cycle: %w", err)
	}
	if cycle <= 0 {
		return 0, fmt.Errorf("critic: durable cycle claim returned non-positive cycle %d", cycle)
	}
	return cycle, nil
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
//
// cycle is intentionally left at zero here. The real cycle number is
// claimed from durable database state at the start of each Run(), so
// constructing an Audit does not consume a cycle and a process that
// starts but never runs one does not skip a Poison Pill boundary.
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
//
// The cycle number is CLAIMED from durable state at the start of the
// run, not incremented in memory. mpm-critic is a one-shot process per
// scheduled audit, so an in-memory counter would restart at 1 on every
// invocation and the every-fifth-cycle Poison Pill could never fire in
// production. See claimDurableCycle for the atomicity and crash
// semantics.
//
// Every hunt in a given run observes the same claimed value via
// Cycle(); hunts never re-read or advance durable state themselves.
func (a *Audit) Run(ctx context.Context) error {
	cycle, err := a.claimDurableCycle(ctx)
	if err != nil {
		return err
	}
	a.cycle = cycle
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
// reads against MPM tables.
//
// WRITE POLICY — two distinct classes, deliberately not conflated:
//
//  1. Findings / epistemic content (memories, theories, lessons) go
//     through cli.Call. The Critic is decoupled from mpm-core's
//     internal API surface, and `mpm call` is the supported write
//     path for domain content.
//
//  2. Bounded internal Critic CONTROL STATE — currently only the
//     `critic_cycle` record claimed by claimDurableCycle — is written
//     directly with transactional SQL on this handle.
//
// Class 2 is narrow and intentional. The cycle counter must be
// readable and advanceable by the one-shot mpm-critic process itself,
// before and independently of any finding emission; routing it through
// the CLI would make cycle identity depend on the CLI being installed,
// on PATH, and on a live daemon, which is exactly the coupling the
// counter exists to escape. It is a single bounded row owned solely by
// the Critic, not domain content, and it is claimed atomically so
// concurrent processes cannot duplicate a cycle.
//
// Any NEW write to this handle must justify itself against this split:
// domain content does not belong here.
func (a *Audit) DB() *sql.DB { return a.db }

// Cycle returns the durable cycle number claimed by the current run,
// or 0 before the first Run(). It is stable for the whole run: every
// hunt observes the same value, and no hunt advances it. Useful in
// test assertions and for hunts that need to be cycle-aware.
func (a *Audit) Cycle() int { return a.cycle }

// CycleStart returns the wall-clock at which the current (or most recent)
// audit cycle began. Hunts use this as the reference for settling-period
// calculations so the period doesn't tick against absolute time during a
// daemon outage — it ticks only when a cycle is actively running.
func (a *Audit) CycleStart() time.Time { return a.cycleStart }
