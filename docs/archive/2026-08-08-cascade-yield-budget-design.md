# Cascade Yield Budget — Design Spec

**Date:** 2026-08-08
**Status:** Alpha implementation
**Issue:** https://github.com/flowbyte-com/mpm/issues/6
**Scope:** Hardening the existing `CascadeDrainHandler` against scheduler starvation

---

## Summary

Add a runtime-configurable per-tick intent budget to the cascade drain handler, and write every `MaterializationReport` to the audit log so operators can see drain progress without parsing wake metadata.

The existing `CascadeDrainHandler` (shipped in #5) already prevents scheduler starvation via a 30s wall-clock budget. Issue #6 asks for **two additional hardening layers**:

1. **Configurable per-tick intent budget** — `system_config.cascade_drain.max_intents_per_tick` (default 50) overrides the hardcoded `BatchSize: 10` at handler construction time. Operators can tune without recompiling.
2. **`MaterializationReport` to audit log** — every `MaterializeBatch` call returns a `MaterializationReport`; the handler writes a row to `system_audit_log` so `mpm audit list --component cascade` surfaces the per-tick drain trail.

Out of scope (deliberately deferred from issue #6):

- **Single-batch-per-tick semantics.** Issue #6's "calls `MaterializeBatch(ctx, max(1, budget))` once" is a stricter contract than the current loop. The current loop already grants per-tick isolation via the 30s wall-clock budget — switching to one batch per tick would slow small backlog drains (a 5-intent queue would take 5 minutes instead of one tick). Retain the loop.
- **Adaptive stair-step.** Issue #6 calls this "optional refinement." The current loop is *already* an adaptive shape — it drains until queue empty or budget hits — so the stair-step is implicit.

---

## Architectural invariants

1. **Fail-safe defaults.** If `system_config.cascade_drain.max_intents_per_tick` is absent or malformed, the handler falls back to a documented default (50). The handler must never fail to start due to misconfiguration.
2. **Audit log is forensically non-load-bearing.** A `LogAudit` failure must not abort the cascade drain. The audit row is for operator visibility; the cascade drain is for correctness.
3. **Budget isolation.** The handler's per-tick intent budget is independent of the per-tick time budget. The time budget caps wall-clock duration; the intent budget caps work performed per batch. Both are tunable.

---

## Design

### 1. Configurable budget

Read at handler construction time from `dm.GetConfigInt("cascade_drain.max_intents_per_tick", 50)`. Storing the value at construction (not reading per tick) matches the existing `BatchSize` semantics and avoids DB queries on the scheduler hot path.

```go
// internal/scheduler/cascade_drain.go
func NewCascadeDrainHandler(dm *core.DatabaseManager, logger *slog.Logger, opts CascadeDrainOptions) *CascadeDrainHandler {
    if opts.Budget <= 0 {
        opts.Budget = 30 * time.Second
    }
    if opts.BatchSize <= 0 {
        // Issue #6: read from system_config (default 50), overridable via
        // MPM_CASCADE_DRAIN_MAX_INTENTS_PER_TICK env var.
        opts.BatchSize = dm.GetConfigInt(CascadeDrainBudgetKey, 50)
    }
    if opts.BatchSize < 1 {
        opts.BatchSize = 1
    }
    // ... existing materializer construction
}
```

Tests can override by writing to `system_config` directly via `SetConfigInt` or by setting the `MPM_CASCADE_DRAIN_MAX_INTENTS_PER_TICK` env var.

### 2. Audit log integration

Every `MaterializeBatch` call returns a `MaterializationReport`. The handler emits one audit row per call. The `materializer` interface gains nothing — the handler still calls `MaterializeBatch`, but the *caller* (the handler) wraps each call in a `LogAudit`.

```go
// In tickHandler, after each MaterializeBatch call:
report, err := h.materializer.MaterializeBatch(ctx, h.batchSize)
if err != nil {
    h.dm.LogAudit(core.AuditWarn, "cascade", "cascade drain batch failed", "", core.AuditContext{
        "yield_reason": "error",
        "err": err.Error(),
    })
    // ... existing yield path
}
totalProcessed += report.Materialized
totalFailed += report.Failed

// Log the report (per-tick forensic trail).
h.dm.LogAudit(core.AuditInfo, "cascade", "cascade drain batch", "", core.AuditContext{
    "claimed":         report.Claimed,
    "processed":       report.Processed,
    "materialized":    report.Materialized,
    "failed":          report.Failed,
    "skipped":         report.Skipped,
    "suppressed":      report.Suppressed,
    "batch_size":      h.batchSize,
    "budget_remaining_ms": time.Until(deadline).Milliseconds(),
})
```

The audit row is the **per-batch** forensic path. The existing `cascade_summary` wake row is the **per-tick** summary path. Both stay.

### 3. CLI subcommand untouched

`mpm cascade materialize` keeps its unbounded loop. The CLI is an operator escape hatch — operators invoke it explicitly when they want the outbox empty *now*. The scheduler is the only path that needs the yield budget.

---

## Where

- **`internal/scheduler/cascade_drain.go`** — read budget from `system_config`; emit `LogAudit` per batch.
- **`internal/scheduler/cascade_drain_test.go`** — new tests for system_config read + audit log row.
- **`cmd/mpm-scheduler/main.go`** — no change required (the handler reads config internally).

---

## Test plan

1. **Budget reads from system_config** — set `cascade_drain.max_intents_per_tick=25`, construct handler, assert `BatchSize() == 25`.
2. **Budget falls back to default** — fresh DM with no config row, assert `BatchSize() == 50`.
3. **Budget respects env var** — `MPM_CASCADE_DRAIN_MAX_INTENTS_PER_TICK=10` on a fresh DM, assert `BatchSize() == 10`.
4. **Budget floor at 1** — set config value to 0, assert `BatchSize() == 1` (forced minimum; the issue's `max(1, budget)` semantic).
5. **Audit row per batch** — drain three materialized batches, assert three `system_audit_log` rows with `component='cascade'` and `materialized` field present.
6. **Audit row on failure** — inject a failing materializer, assert one `system_audit_log` row with `level='warn'` and `err` field.

## Implementation estimate

~50 lines of code changes in `cascade_drain.go` (budget read + audit log per batch + constants/key). ~80 lines of tests. Single binary recompile. No schema migration.

## Priority

Day-2 follow-up. The CLI subcommand is the alpha-release contract. The scheduler integration already ships (issue #5) and prevents starvation via the 30s time budget; this hardening adds operator-tunable intent budget and audit visibility.
