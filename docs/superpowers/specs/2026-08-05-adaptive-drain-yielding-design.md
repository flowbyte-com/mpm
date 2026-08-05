# Adaptive Drain Yielding Design

**Date:** 2026-08-05
**Status:** Approved design; implementation pending
**Scope:** Cascade materializer ownership and `mpm-scheduler` integration

## Problem

The cascade materializer today runs as an auto-starting background goroutine pool owned by `DatabaseManager` (two workers, 5-second poll, `BatchSize=10`). A massive root invalidation event — for example, shredding a foundational root directive — can produce hundreds of cascade intents at once. The current pool drains them in the background, but two concerns remain:

1. **Lifecycle coupling.** The `DatabaseManager` owns a long-lived goroutine pool with start/stop methods and a `cascadeMatMu` mutex. The pool is started implicitly by `mpm start` and `mpm-agent`, meaning the cascade outbox is silently consumed in any process that opens the database. There is no single, observable owner.

2. **Scheduler immunity is not guaranteed.** If a future caller invokes `MaterializeCascadeIntents(ctx, huge_number)` from within a `mpm-scheduler` tick (for example, by adding a `cascade_drain` handler), the call could block the scheduler's 60-second tick and delay `snapshot`, `critic_audit`, `gc`, and `broadcast` from firing on schedule. Even with parallel goroutine execution inside the tick, the tick's `Wait` does not return until every handler completes; a runaway drain would delay the next tick.

The user-visible behavior we want: cascade draining is owned by exactly one background process (`mpm-scheduler`), runs under a per-tick time budget, yields cleanly when the budget runs out, and never starves other scheduler subsystems.

## Goals and non-goals

### Goals

- Cascade draining has exactly one background owner: the `cascade_drain` handler in `mpm-scheduler`.
- The handler yields within a per-tick time budget (default 30s of the 60s tick).
- Yield reason is observable via structured log fields, with four exhaustive values.
- The core `CascadeMaterializer` and `MaterializeBatch` remain pure, stateless functions — no internal clocks, no mid-batch rollback.
- The CLI `mpm cascade materialize` continues to work as the explicit foreground escape hatch, with no behavior change.
- No normal CLI command or MCP server spawns a hidden cascade thread.

### Non-goals

- Distributed locking across multiple `mpm-scheduler` instances (out of scope: assume a single scheduler; multi-instance coordination is a future spec).
- Adaptive budget tuning based on observed load (deferrable; static 30s default is sufficient).
- Changing the cascade outbox schema or state machine.
- Modifying `MaterializeBatch` to honor a context deadline mid-batch.

## Architecture

The cascade materializer becomes a **stateless function** invoked by exactly one background caller (the scheduler handler) and one foreground caller (the CLI). There is no shared, long-lived instance on `DatabaseManager`. The handler owns the clock; the core owns the state machine.

```
┌─ mpm-scheduler tick fires (every 60s) ──────────────────────┐
│                                                             │
│  CascadeDrainHandler.Run(ctx):                              │
│    start := time.Now()                                      │
│    for time.Since(start) < Budget:                          │
│      if ctx.Err() != nil:        yield("context_cancelled") │
│      if time.Now().After(deadline): yield("budget_exhausted")│
│      report, err = materializer.MaterializeBatch(ctx, 10)  │
│      if err != nil:              yield("error")            │
│      if report.Claimed == 0:     yield("queue_empty")       │
│                                                             │
│  snapshot / critic_audit / gc / broadcast   (parallel)      │
└─────────────────────────────────────────────────────────────┘

┌─ mpm cascade materialize (foreground escape hatch) ─────────┐
│                                                             │
│  for {                                                      │
│    report = dm.MaterializeCascadeIntents(ctx, 10)           │
│    if report.Claimed == 0: break                            │
│  }                                                          │
│                                                             │
│  No time budget. Operator chose to wait.                    │
└─────────────────────────────────────────────────────────────┘
```

## Component boundaries

### `internal/core/cascade_materializer.go` — net reduction

**Deleted:**

- `CascadeMaterializer.runLoop` (the polling goroutine)
- `CascadeMaterializer.start()` / `stop()` lifecycle methods
- `CascadeMaterializer.startC`, `stopC` channels
- `CascadeMaterializer.workers` goroutine handles
- `CascadeMaterializer.pollInterval` field
- Any `go cm.runLoop(ctx)` site (currently only inside `CascadeMaterializer.start()`; deleted alongside the start method)

**Kept (unchanged):**

- `CascadeMaterializer` struct + `NewCascadeMaterializer(dm, opts)`
- `MaterializeBatch(ctx, limit)` — pure function, no clock
- `processIntent`, `claimCascadeIntents`, `materializeTheory`, depth guard, retry/backoff, dead-letter, audit events, wake scheduling
- `DefaultCascadeMaterializerOptions`, `CascadeMaterializerOptions`, all knobs (`MaxCascadeDepth`, `MaxRetries`, `WakeDelay`, `BatchSize`)
- The stale-recovery path (`status='processing'` rows > 5min revert to `pending`); still useful as crash-safety on the CLI path even though the background path no longer needs it

### `internal/core/db.go` — minimal change

**Deleted:**

- `DatabaseManager.cascadeMaterializer` field
- `DatabaseManager.cascadeMatMu` mutex
- `DatabaseManager.StartCascadeMaterializer(ctx)` method
- `DatabaseManager.StopCascadeMaterializer()` method
- Any constructor / `init` site that wires them up

**Modified:**

- `MaterializeCascadeIntents(ctx context.Context, limit int)` loses the mutex dance and the nil check. It becomes a one-shot convenience wrapper that constructs a default-config materializer and calls `MaterializeBatch`:

  ```go
  // MaterializeCascadeIntents is a one-shot convenience for CLI callers
  // (the foreground escape hatch). The background drain is owned by
  // mpm-scheduler; there is no shared long-lived materializer on
  // DatabaseManager.
  func (dm *DatabaseManager) MaterializeCascadeIntents(ctx context.Context, limit int) (MaterializationReport, error) {
      return dm.NewCascadeMaterializer(DefaultCascadeMaterializerOptions()).MaterializeBatch(ctx, limit)
  }
  ```

**Added:**

- `DatabaseManager.NewCascadeMaterializer(opts)` — a thin factory that delegates to `NewCascadeMaterializer(dm, opts)`. The seam exists for future cross-cutting concerns (tracing, metrics, alternate storage backends); today it is one line.

### `internal/scheduler/cascade_drain.go` (new file)

The `CascadeDrainHandler` is a struct holding a `DatabaseManager`, a logger, options, and one `CascadeMaterializer` for the scheduler's lifetime. Its `Run(ctx)` method owns the clock and the loop; see **Handler internals** below for the exact implementation.

### `internal/scheduler/scheduler.go`

No API change. `Register` already accepts any function matching the `Handler` signature. The new handler plugs in alongside `snapshot`, `critic_audit`, `gc`, `broadcast`.

### `cmd/mpm-scheduler/main.go`

One new `s.Register` call:

```go
s.Register("cascade_drain", scheduler.NewCascadeDrainHandler(dm, logger, scheduler.CascadeDrainOptions{
    Budget:    30 * time.Second,
    BatchSize: 10,
}).Run)
```

### `cmd/mpm/handlers_cascade.go`

Unchanged on the surface. `mpm cascade materialize` still calls `MaterializeCascadeIntents(ctx, 10)` in a blocking loop until empty. After the core change, the loop's only effect is calling the pure `MaterializeBatch` via the convenience wrapper.

## Handler internals

### Construction

```go
type CascadeDrainHandler struct {
    dm           *core.DatabaseManager
    logger       *slog.Logger
    budget       time.Duration
    batchSize    int
    materializer *core.CascadeMaterializer
}

type CascadeDrainOptions struct {
    Budget    time.Duration  // default 30s
    BatchSize int            // default 10
}

func NewCascadeDrainHandler(dm *core.DatabaseManager, logger *slog.Logger, opts CascadeDrainOptions) *CascadeDrainHandler {
    if opts.Budget <= 0 {
        opts.Budget = 30 * time.Second
    }
    if opts.BatchSize <= 0 {
        opts.BatchSize = 10
    }
    return &CascadeDrainHandler{
        dm: dm, logger: logger,
        budget:    opts.Budget,
        batchSize: opts.BatchSize,
        materializer: dm.NewCascadeMaterializer(core.CascadeMaterializerOptions{
            MaxCascadeDepth: 3,
            MaxRetries:      3,
            WakeDelay:       1 * time.Second,
        }),
    }
}
// Budget and BatchSize are exported accessors used by tests and any future
// observability surface that needs to read the configured values without
// holding the options struct.
func (h *CascadeDrainHandler) Budget() time.Duration    { return h.budget }
func (h *CascadeDrainHandler) BatchSize() int           { return h.batchSize }
```

The handler holds **one materializer for its lifetime** because `MaterializeBatch` is stateless; sharing across ticks is safe and avoids per-tick allocation.

### Run loop

```go
func (h *CascadeDrainHandler) Run(ctx context.Context) (err error) {
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
        if err := ctx.Err(); err != nil {
            h.logYield(ctx, totalProcessed, totalFailed, "context_cancelled", time.Since(start))
            return nil
        }
        if time.Now().After(deadline) {
            h.logYield(ctx, totalProcessed, totalFailed, "budget_exhausted", time.Since(start))
            return nil
        }

        report, err := h.materializer.MaterializeBatch(ctx, h.batchSize)
        if err != nil {
            h.logger.Error("cascade drain batch failed", "err", err)
            h.logYield(ctx, totalProcessed, totalFailed, "error", time.Since(start))
            return nil
        }
        totalProcessed += report.Materialized
        totalFailed += report.Failed

        if report.Claimed == 0 {
            h.logYield(ctx, totalProcessed, totalFailed, "queue_empty", time.Since(start))
            return nil
        }
    }
}

func (h *CascadeDrainHandler) logYield(ctx context.Context, processed, failed int, reason string, elapsed time.Duration) {
    h.logger.Info("cascade drain yielded",
        "yield_reason", reason,
        "intents_materialized", processed,
        "intents_failed", failed,
        "elapsed_ms", elapsed.Milliseconds(),
        "budget_ms", h.budget.Milliseconds(),
    )
}
```

### Why the budget is checked *between* calls, not mid-call

A single `MaterializeBatch(ctx, 10)` call is bounded by the underlying SQLite write loop and the existing `busy_timeout`. With `BatchSize=10` and a normal per-intent cost (sub-millisecond SQLite writes plus one `ProposeTheoryWithExtras`), the call typically completes well under one second. Letting it run to completion preserves the outbox state machine's invariants:

- Every intent a batch claimed lands in `status='materialized'` or `status='failed'` (terminal) before the next batch is claimed.
- `attempt_count` increments deterministically per failure.
- There are no half-claimed rows for the next tick to clean up.

If a single batch overruns the budget by 4 seconds inside a 60-second tick, the next tick is delayed by 4 seconds. That is harmless — the headroom between 30s budget and 60s tick absorbs it.

### Yield reason taxonomy

Four values, exhaustive:

| reason              | meaning                                              | operator signal                       |
|---------------------|------------------------------------------------------|---------------------------------------|
| `queue_empty`       | outbox drained, normal exit                          | healthy                               |
| `budget_exhausted`  | 30s ran out, more pending                            | tune budget or investigate latency    |
| `context_cancelled` | scheduler shutdown mid-tick                          | expected on `mpm stop`                |
| `error`             | `MaterializeBatch` returned a non-nil error          | DB issue; investigate logs            |

## Error handling

| Failure                                            | Handler behavior                                     | Rationale                                    |
|----------------------------------------------------|------------------------------------------------------|----------------------------------------------|
| `MaterializeBatch` returns error                   | Log error, `yield_reason="error"`, return `nil`      | Don't panic the scheduler; let next tick retry |
| Intent processing inside `MaterializeBatch` fails  | Handled by `MaterializeBatch` retry/dead-letter path | Core's job; handler sees it as `report.Failed` |
| Budget exceeded mid-`MaterializeBatch` call        | Let the call finish; budget is checked between calls | A multi-second overrun is harmless            |
| Scheduler context cancelled                        | First check in loop catches it; `yield_reason="context_cancelled"` | Clean shutdown                              |
| Handler panics                                     | `defer recover()` returns `nil` and logs the panic with stack | Scheduler immunity                          |
| `mpm start` (no scheduler) with cascades pending   | They sit in `status='pending'` until CLI runs or scheduler starts | Same as today for non-auto-start paths        |

## Testing

### Test migration

Two categories of existing tests are affected:

**Category 1 — tests that drove the goroutine pool** become direct calls:

```go
mat := dm.NewCascadeMaterializer(core.DefaultCascadeMaterializerOptions())
for {
    r, err := mat.MaterializeBatch(ctx, 10)
    require.NoError(t, err)
    if r.Claimed == 0 { break }
}
```

**Category 2 — tests that asserted materializer lifecycle** (start/stop/idempotency) are deleted or repurposed. There is no longer a start/stop to assert; the factory method is the only entry point.

### New handler tests (`internal/scheduler/cascade_drain_test.go`)

- `TestCascadeDrain_YieldsQueueEmpty` — seed outbox, call `Run`, assert `yield_reason="queue_empty"` and all intents materialized.
- `TestCascadeDrain_YieldsBudgetExhausted` — seed 100 intents, set budget to 100ms, assert `yield_reason="budget_exhausted"`, `intents_materialized > 0`, and at least one intent remains `status='pending'` for the next tick.
- `TestCascadeDrain_HonoursContextCancellation` — cancel mid-loop, assert `yield_reason="context_cancelled"`.
- `TestCascadeDrain_DoesNotPanicOnDBError` — close DB mid-loop (or use a fault-injecting fake), assert handler returns `nil` and the parent scheduler stays alive.
- `TestCascadeDrain_DoesNotMutateClaimedIntentsOnBudgetExit` — verify all processed intents land in terminal state; none stuck in `status='processing'`.
- `TestCascadeDrain_BudgetDefaultsApplied` — `NewCascadeDrainHandler(dm, logger, CascadeDrainOptions{})` produces a handler with `budget == 30s` and `batchSize == 10`. Asserts on the handler fields directly (export getters `Budget()` and `BatchSize()` are added for this purpose; they are also useful for observability and future tests).
- `TestCascadeDrain_LogsYieldReason` — capture slog output, assert `cascade drain yielded` line is emitted with the expected fields.

## Backward compatibility

### CLI

`mpm cascade materialize` works identically. Signature of `MaterializeCascadeIntents` does not change. The only observable difference: previously, if `StartCascadeMaterializer` was never called, the CLI silently returned an empty report; now it always processes. This is strictly better and not a breaking change for any caller that was not relying on the silent no-op.

### Background draining

Operators who relied on the auto-starting background pool will see a behavioral change: cascades now drain only when `mpm-scheduler` is running. This is documented in the operator runbook (below) and the changelog.

### Migration path

For operators running `mpm start` without `mpm-scheduler`:

- Before this change: cascades drained automatically in the background.
- After this change: cascades sit in `status='pending'` until `mpm-scheduler` runs or an operator invokes `mpm cascade materialize`.
- Recommended migration: ensure `mpm-scheduler` is running as part of `mpm start`. The `mpm start` command already wires up the scheduler; verify the operator's deployment.

## Operator runbook

The full runbook lives in `docs/EPISTEMIC_CASCADES.md` (operator guide) under a new "Operational notes — scheduler-driven cascade drain" appendix. Summary:

### How to confirm draining is happening

- `mpm-scheduler` logs a `cascade drain yielded` line on every tick where the handler runs, with one of four `yield_reason` values.

### "I shredded a root directive but the cascade hasn't materialized"

1. Is `mpm-scheduler` running? Check for `cascade drain yielded` log lines.
2. Is the outbox non-empty?
   ```sql
   sqlite3 src/db/mpm.db \
     "SELECT COUNT(*) FROM epistemic_cascade_outbox WHERE status='pending';"
   ```
3. Is the handler yielding `budget_exhausted` consistently? If so, check `intents_materialized` per tick and consider raising `CascadeDrainOptions.Budget`.
4. Are intents in `status='failed'` (dead-letter)? Inspect with `mpm cascade list-dead-letters`.

### "I don't want to run `mpm-scheduler`"

Run the foreground escape hatch: `mpm cascade materialize`. No time budget; the operator chose to wait.

### Tuning the budget

`CascadeDrainOptions.Budget` defaults to 30s. The handler yields when the budget runs out; the scheduler's 60s tick has 30s of headroom. Lower the budget if you need faster snapshot/critic_audit response; raise it if you want faster drain after a large cascade blast.

## Open questions

None at design time. Two are deferred to follow-up work:

1. **Adaptive budget tuning** — could auto-adjust the budget based on observed per-batch latency. Out of scope; static 30s default is sufficient.
2. **Multi-scheduler coordination** — if multiple `mpm-scheduler` instances ever run, both would call `MaterializeBatch` and serialize on `BEGIN IMMEDIATE`. Not a correctness issue, but wasteful. Out of scope; document the assumption that exactly one scheduler runs.

## Rollout

1. Land the core deletion + factory method + handler + tests in one PR.
2. Update `docs/EPISTEMIC_CASCADES.md` operator guide with the runbook appendix.
3. Update `CHANGELOG` / CLAUDE.md with the topology change.
4. Verify on staging with a synthetic root shred: confirm the `cascade drain yielded` log appears with `yield_reason="budget_exhausted"` when the outbox has more intents than fit in one tick.
