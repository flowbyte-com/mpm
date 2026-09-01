// Package seed (scheduled_tasks.go) — the Baseline Cognitive Bootstrap
// for scheduled tasks.
//
// Mirrors the directives registry: SeedScheduledTasks is the canonical
// list of recurring agentic workflows that a fresh MPM install should
// arrive with. The apply loop lives in internal/core (see
// DatabaseManager.seedBaselineScheduledTasks) because it depends on
// core-only types (ScheduledTask, CalculateNextRun, the dm's
// UpsertScheduledTask writer); a core -> seed import for the registry
// data and a seed -> core import for the apply logic would create a
// cycle. The split is registry-in-seed, apply-in-core.
//
// Contract (parallel to ApplyDirectives):
//
//   - StableID absent       → Created (UpsertScheduledTask via the
//                             canonical writer; next_run_at computed
//                             from the canonical cron expression)
//   - StableID present      → Skipped (no-op). Operator's custom
//                             cron_expr / name / directive_id / status
//                             are preserved verbatim. The seed NEVER
//                             overwrites an existing task — a manually
//                             paused or renamed task stays as the
//                             operator configured it.
//
// Why this exists: prior to this, a fresh install had no default
// scheduled task. The substrate reports epistemic_pressure as a hint
// to the agent, but the agent only sees the wake when something wakes
// it. The `epistemic-compaction` task is the canonical agent-side
// reflex — it wakes the agent on a daily cadence and tells the agent
// to consult the mpm-seed-epistemic-compaction-policy directive.
//
// Architecturally, this preserves the substrate / agent / scheduler
// separation:
//
//   - Substrate: measures + exposes pressure
//   - Scheduler: delivers the wake, no LLM
//   - Agent:     reads the directive, runs compact
//
// See mpm-seed-epistemic-compaction-policy for the directive content
// and the compact contract.
package seed

// SeedScheduledTask is one entry in the Baseline Cognitive Bootstrap
// for scheduled tasks. Each entry pairs a stable row id with a
// canonical cron expression, a name (for human display), and the
// directive the agent reads on wake.
//
// StableID must be human-readable and stable across versions — once
// an entry has been seeded, the same StableID is used to detect
// prior seeding. Changing StableID creates a duplicate. Changing
// CronExpr, Name, or DirectiveID after seeding is intentionally
// treated as a no-op on existing rows (operator's customization
// preserved). To upgrade the canonical expression, document the
// migration in CHANGELOG and let the operator re-upsert manually.
type SeedScheduledTask struct {
	// StableID is the scheduled_tasks row's primary key. The same
	// StableID is used to detect prior seeding on re-run. Convention:
	// the stable id IS the operator-facing task id (no mpm-seed-
	// prefix) because operators reference this id in `mpm tasks list`
	// and `mpm tasks delete`. Prefixing it would force the operator
	// to remember a non-canonical name.
	StableID string

	// Name is the human-readable label printed by `mpm tasks list`.
	// Operators may rename it locally; the seed never re-writes.
	Name string

	// CronExpr is the canonical 5-field cron expression evaluated in
	// UTC. Default cadence is daily at 03:00 UTC (low-noise window
	// for a background LLM reflection pass). Operators may re-upsert
	// with a different expression; the seed never re-writes.
	CronExpr string

	// DirectiveID is the stable id of the prime directive the agent
	// reads when the wake fires. Must reference a row that exists
	// in the memories table (collection='directives') — the canonical
	// ApplyDirectives seed MUST run before the scheduled-task seed
	// so the directive row is present. The apply loop validates this
	// before insertion and skips with a clear summary bucket if the
	// directive is missing.
	DirectiveID string

	// Status is the canonical initial status. Defaults to
	// "active" (ScheduledTaskActive). Operators may pause or
	// unpause; the seed never re-writes.
	Status string
}

// SeedScheduledTasks is the canonical registry of baseline scheduled
// tasks. Edit this slice to add or deprecate baseline tasks.
//
// Order does not affect runtime retrieval (scheduled tasks are
// fetched by next_run_at ASC), but it does affect seed output
// determinism. New entries are appended at the end; do not reorder
// existing entries — it doesn't help anything and may obscure
// blame in code review.
var SeedScheduledTasks = []SeedScheduledTask{
	{
		// The canonical epistemic-compaction reflex. Wakes the agent
		// daily at 03:00 UTC. The agent consults
		// mpm-seed-epistemic-compaction-policy and invokes
		// mpm_system.compact with force=false, max_batches=5.
		//
		// Cadence rationale (2026-09-01 investigation): the 19-day
		// empirical gap with no compact invocation caused no
		// measurable substrate harm (99% compaction ratio held,
		// query latencies nominal), so the reflex does not need to
		// be aggressive. Daily at 03:00 UTC is the documented
		// INSTALL.md cadence. Operators with high ingest rates may
		// re-upsert to every 4h; operators with low ingest may move
		// to weekly. The seed default is the conservative middle.
		//
		// The agent owns the reflex — this task only DELIVERS the
		// wake. The scheduler does not invoke compaction.
		StableID:    "epistemic-compaction",
		Name:        "Nightly epistemic compaction (agent-owned reflex)",
		CronExpr:    "0 3 * * *",
		DirectiveID: "mpm-seed-epistemic-compaction-policy",
		Status:      "active",
	},
}

// SeedTaskSummary is the human-readable report printed by
// `mpm ops init tasks` after a run. Mirrors SeedSummary's shape so
// the CLI print helper can render either without branching.
type SeedTaskSummary struct {
	Created []string // StableIDs that were inserted as new rows
	Skipped []string // StableIDs that already existed (operator customization preserved)
	Missing []string // StableIDs skipped because the named directive was not seeded yet
}

// Status enum constants — mirror the names in internal/core so the
// seed package does not need to import core for the literals. Kept
// in sync with ScheduledTaskActive / ScheduledTaskPaused in
// internal/core/scheduled_tasks.go. If a future refactor changes
// the canonical strings, update these in lockstep.
const (
	ScheduledTaskActive = "active"
	ScheduledTaskPaused = "paused"
)
