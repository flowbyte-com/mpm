# Adaptive Drain Yielding Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Move cascade materializer ownership from an auto-starting `DatabaseManager` goroutine pool to an explicit `mpm-scheduler` tick handler that yields within a per-tick time budget.

**Architecture:** `CascadeMaterializer` becomes a pure stateless function. `mpm-scheduler` registers a new tick handler (`CascadeDrainHandler`) that owns the clock and the yield decision. The handler logs an exhaustive `yield_reason` taxonomy (`queue_empty` / `budget_exhausted` / `context_cancelled` / `error`) so operators can observe whether the 30s budget is hitting its ceiling.

**Tech Stack:** Go 1.22+, `mattn/go-sqlite3` with `CGO_CFLAGS=-DSQLITE_ENABLE_FTS5`, `log/slog` for structured logging, existing `database/sql` patterns in `internal/core/`.

## Global Constraints

These apply to every task. Copied verbatim from the spec.

- The core `MaterializeBatch` and `CascadeMaterializer` are **never modified** in their processing logic. Only lifecycle and ownership changes.
- The CLI `mpm cascade materialize` keeps its current signature and behavior — it is the foreground escape hatch.
- Normal CLI commands and the MCP server **must never** spawn a hidden cascade thread.
- Cascade draining is owned by exactly one background process: `mpm-scheduler`.
- Tests assume FTS5 is compiled in. Run with `CGO_CFLAGS="-DSQLITE_ENABLE_FTS5" CGO_LDFLAGS="-lm"`.
- The `cmd/mpm` tests live in `cmd/mpm/*_test.go`; the core tests live in `internal/core/*_test.go`. Core is a separate Go module under `internal/core/`.
- All commits are made from `/home/v/workspace/projects/mpm`. Commit messages follow the project's `feat(scope): subject` convention.

## File Structure

### Files to create

| File | Responsibility |
|------|----------------|
| `internal/scheduler/cascade_drain.go` | `CascadeDrainHandler` struct, `NewCascadeDrainHandler`, `Run(w Wake)`, `logYield` |
| `internal/scheduler/cascade_drain_test.go` | 7 test cases covering all yield reasons + safety properties |

### Files to modify

| File | Change |
|------|--------|
| `internal/core/cascade_materializer.go` | Delete `runLoop`, `Start`, `Stop`, `startC`, `stopC`, `wg`, `mu`, `run`, `PollInterval`, `Workers` fields. Update doc comment on `NewCascadeMaterializer` |
| `internal/core/db.go` | Delete `cascadeMaterializer` field, `cascadeMatMu`, `StopCascadeMaterializer`, `StartCascadeMaterializer`. Refactor `MaterializeCascadeIntents`. Add `NewCascadeMaterializer(opts)` factory method |
| `internal/core/core.go` | Remove `StartCascadeMaterializer`, `StopCascadeMaterializer` from the `CoreDB` interface |
| `internal/scheduler/scheduler.go` | Add `RegisterTickHandler` method + tick-handler dispatch in `Tick()` |
| `internal/scheduler/scheduler_test.go` | Add tests for `RegisterTickHandler` dispatch |
| `cmd/mpm-scheduler/main.go` | Register `cascade_drain` via `RegisterTickHandler` |
| `cmd/mpm/handlers_cascade.go` | Drop `dm.StartCascadeMaterializer` and `defer dm.StopCascadeMaterializer()` (no longer exists) |
| `docs/EPISTEMIC_CASCADES.md` | Add "Operational notes — scheduler-driven cascade drain" appendix |

### Files NOT modified

- `internal/core/cascade_outbox.go` — outbox schema and enqueue logic stay untouched
- `internal/core/cascade_materializer.go` `processIntent`, `claimCascadeIntents`, `materializeTheory`, `markMaterialized`, `markFailed`, `requeueIntent`, `revertToPending`, `scheduleCascadeWake` — all stay
- `internal/core/MaxCascadeDepth`, `HardConfidenceInvalidationThreshold`, `MaxRetries`, `WakeDelay`, `BatchSize` constants — all stay

---

## Task 1: Add `NewCascadeMaterializer` factory on `DatabaseManager`

**Files:**
- Modify: `internal/core/db.go` (add new method)
- Test: existing `cmd/mpm/handlers_cascade.go` exercises this path implicitly

**Interfaces:**
- Consumes: `CascadeMaterializerOptions`, `NewCascadeMaterializer` package-level function (already exists at `cascade_materializer.go:96`)
- Produces: `DatabaseManager.NewCascadeMaterializer(opts)` thin factory

- [ ] **Step 1: Add the factory method**

In `internal/core/db.go`, locate the section near `StartCascadeMaterializer` (line ~2010). Above it, add:

```go
// NewCascadeMaterializer constructs a CascadeMaterializer bound to this
// DatabaseManager. The returned materializer carries no mutable state beyond
// the dm reference; safe to share across goroutines. The caller is expected
// to invoke MaterializeBatch directly (background drain is owned by
// mpm-scheduler; the CLI calls MaterializeCascadeIntents).
func (dm *DatabaseManager) NewCascadeMaterializer(opts CascadeMaterializerOptions) *CascadeMaterializer {
    return NewCascadeMaterializer(dm, opts)
}
```

- [ ] **Step 2: Build to verify it compiles**

Run: `cd /home/v/workspace/projects/mpm && make build 2>&1 | tail -20`
Expected: Build succeeds. The new method is additive — no existing call site breaks.

- [ ] **Step 3: Commit**

```bash
cd /home/v/workspace/projects/mpm
git add internal/core/db.go
git commit -m "feat(core): add DatabaseManager.NewCascadeMaterializer factory"
```

---

## Task 2: Refactor `MaterializeCascadeIntents` to drop mutex and nil check

**Files:**
- Modify: `internal/core/db.go` (replace `MaterializeCascadeIntents` body, lines ~2034-2045)

**Interfaces:**
- Consumes: `DatabaseManager.NewCascadeMaterializer`, `DefaultCascadeMaterializerOptions`
- Produces: `MaterializeCascadeIntents(ctx, limit)` — same signature, no mutex, no nil check

- [ ] **Step 1: Replace the function body**

In `internal/core/db.go`, replace `MaterializeCascadeIntents` (lines ~2034-2045):

```go
// MaterializeCascadeIntents is a one-shot convenience for CLI callers (the
// foreground escape hatch). It constructs a default-config materializer
// per call and runs one batch. The background drain is owned by
// mpm-scheduler; there is no shared long-lived materializer on
// DatabaseManager.
func (dm *DatabaseManager) MaterializeCascadeIntents(ctx context.Context, limit int) (MaterializationReport, error) {
    return dm.NewCascadeMaterializer(DefaultCascadeMaterializerOptions()).MaterializeBatch(ctx, limit)
}
```

- [ ] **Step 2: Build to verify it compiles**

Run: `cd /home/v/workspace/projects/mpm && make build 2>&1 | tail -20`
Expected: Build succeeds. CLI tests still reference this method unchanged.

- [ ] **Step 3: Run existing cascade tests to confirm no regression**

Run: `cd /home/v/workspace/projects/mpm/internal/core && CGO_CFLAGS="-DSQLITE_ENABLE_FTS5" CGO_LDFLAGS="-lm" go test -tags fts5 -run "TestCascade|TestMaterialize" -v 2>&1 | tail -30`
Expected: Tests that exercise `MaterializeBatch` directly pass. Tests that called `StartCascadeMaterializer` first will fail — those are addressed in Tasks 3-5.

- [ ] **Step 4: Commit**

```bash
cd /home/v/workspace/projects/mpm
git add internal/core/db.go
git commit -m "refactor(core): MaterializeCascadeIntents constructs materializer per call"
```

---

## Task 3: Delete goroutine pool from `cascade_materializer.go`

**Files:**
- Modify: `internal/core/cascade_materializer.go` (delete `runLoop`, `Start`, `Stop`, lifecycle fields)
- Update doc comment on `NewCascadeMaterializer`

**Interfaces:**
- Removes: `(*CascadeMaterializer).Start(ctx)`, `(*CascadeMaterializer).Stop()`, `runLoop`
- Removes struct fields: `stopC`, `stopOnce`, `wg`, `mu`, `run`
- Removes options: `PollInterval`, `Workers` (no longer needed; check `DefaultCascadeMaterializerOptions` too)
- Keeps: `CascadeMaterializer` struct, `NewCascadeMaterializer`, `MaterializeBatch`, all processing methods

- [ ] **Step 1: Update `DefaultCascadeMaterializerOptions`**

In `internal/core/cascade_materializer.go` (lines 70-80), remove `PollInterval` and `Workers`:

```go
func DefaultCascadeMaterializerOptions() CascadeMaterializerOptions {
    return CascadeMaterializerOptions{
        BatchSize:       10,
        MaxRetries:      3,
        MaxCascadeDepth: MaxCascadeDepth,
        WakeDelay:       1 * time.Second,
    }
}
```

- [ ] **Step 2: Update `CascadeMaterializerOptions` struct**

In the same file (lines 47-68), remove the `PollInterval` and `Workers` fields and update the doc comment:

```go
type CascadeMaterializerOptions struct {
    BatchSize       int
    MaxRetries      int
    MaxCascadeDepth int
    WakeDelay       time.Duration
}
```

- [ ] **Step 3: Update `NewCascadeMaterializer`**

Replace the constructor (lines 94-120):

```go
// NewCascadeMaterializer builds a stateless materializer bound to the
// supplied DatabaseManager. The returned value carries no goroutines;
// callers invoke MaterializeBatch directly. The background drain is
// owned by mpm-scheduler; the CLI calls MaterializeCascadeIntents.
func NewCascadeMaterializer(dm *DatabaseManager, opts CascadeMaterializerOptions) *CascadeMaterializer {
    if opts.BatchSize <= 0 {
        opts.BatchSize = 10
    }
    if opts.MaxRetries <= 0 {
        opts.MaxRetries = 3
    }
    if opts.MaxCascadeDepth <= 0 {
        opts.MaxCascadeDepth = MaxCascadeDepth
    }
    if opts.WakeDelay < 0 {
        opts.WakeDelay = 1 * time.Second
    }
    return &CascadeMaterializer{
        dm:   dm,
        opts: opts,
    }
}
```

- [ ] **Step 4: Simplify `CascadeMaterializer` struct**

Replace the struct (lines 82-92):

```go
type CascadeMaterializer struct {
    dm   *DatabaseManager
    opts CascadeMaterializerOptions
}
```

- [ ] **Step 5: Delete `Start`, `Stop`, and `runLoop`**

Delete the entire `Start` method (lines 122-144), `Stop` method (lines 146-160), and `runLoop` (lines 162-188).

- [ ] **Step 6: Update imports**

Remove `sync` from the import block (lines 40-45) — no longer used:

```go
import (
    "context"
    "fmt"
    "time"
)
```

- [ ] **Step 7: Build to verify it compiles**

Run: `cd /home/v/workspace/projects/mpm && make build 2>&1 | tail -30`
Expected: **Fails** with errors about `dm.StartCascadeMaterializer` and `dm.StopCascadeMaterializer` (callers in `cmd/mpm/handlers_cascade.go`, `internal/core/db.go`). This is expected — Task 4 removes the DatabaseManager methods and Task 5 fixes the CLI.

- [ ] **Step 8: Commit (build is broken; this commit is the deletion half)**

```bash
cd /home/v/workspace/projects/mpm
git add internal/core/cascade_materializer.go
git commit -m "refactor(core): remove cascade materializer goroutine pool"
```

---

## Task 4: Delete `cascadeMaterializer` field, mutex, and lifecycle methods on `DatabaseManager`

**Files:**
- Modify: `internal/core/db.go` (delete `cascadeMaterializer`, `cascadeMatMu`, `StartCascadeMaterializer`, `StopCascadeMaterializer`)
- Modify: `internal/core/core.go` (remove the two method declarations from the `CoreDB` interface)

**Interfaces:**
- Removes: `cascadeMaterializer`, `cascadeMatMu`, `StopCascadeMaterializer`, `StartCascadeMaterializer`
- Keeps: `NewCascadeMaterializer`, `MaterializeCascadeIntents`

- [ ] **Step 1: Delete the field and mutex in `db.go`**

Locate `cascadeMaterializer` field declaration (line ~241-245). Delete the field, its doc comment, and the `cascadeMatMu` mutex line.

- [ ] **Step 2: Delete `StopCascadeMaterializer` (lines ~1997-2003)**

Delete the entire `StopCascadeMaterializer` method.

- [ ] **Step 3: Delete `StartCascadeMaterializer` (lines ~2010-2021)**

Delete the entire `StartCascadeMaterializer` method (above `MaterializeCascadeIntents`).

- [ ] **Step 4: Remove the interface declarations in `core.go`**

In `internal/core/core.go`, remove the `StartCascadeMaterializer` and `StopCascadeMaterializer` lines from the `CoreDB` interface (lines ~38-39). Keep `MaterializeCascadeIntents`.

- [ ] **Step 5: Build to verify it compiles**

Run: `cd /home/v/workspace/projects/mpm && make build 2>&1 | tail -30`
Expected: **Fails** in `cmd/mpm/handlers_cascade.go` — references to `dm.StartCascadeMaterializer` and `dm.StopCascadeMaterializer`. Task 5 fixes them.

- [ ] **Step 6: Commit**

```bash
cd /home/v/workspace/projects/mpm
git add internal/core/db.go internal/core/core.go
git commit -m "refactor(core): remove DatabaseManager cascade lifecycle methods"
```

---

## Task 5: Update `cmd/mpm/handlers_cascade.go` to drop Start/Stop calls

**Files:**
- Modify: `cmd/mpm/handlers_cascade.go` (remove `defer dm.StopCascadeMaterializer()` and `dm.StartCascadeMaterializer(ctx)`)

**Interfaces:**
- Consumes: existing `dm.MaterializeCascadeIntents` (unchanged signature)
- Produces: CLI that constructs nothing, just calls `MaterializeCascadeIntents` in a loop

- [ ] **Step 1: Delete the lifecycle calls**

In `cmd/mpm/handlers_cascade.go`:
- Delete line 102: `defer dm.StopCascadeMaterializer()`
- Delete lines 104-106 (the `dm.StartCascadeMaterializer(ctx)` call and surrounding comments)

After deletion, the code near line 95 should look like:

```go
ctx := context.Background()

start := time.Now()

if *once {
```

- [ ] **Step 2: Build to verify it compiles**

Run: `cd /home/v/workspace/projects/mpm && make build 2>&1 | tail -20`
Expected: **Succeeds**. The CLI is now a pure foreground loop using `MaterializeCascadeIntents` only.

- [ ] **Step 3: Run the CLI smoke test**

Run: `cd /home/v/workspace/projects/mpm && ./bin/mpm cascade materialize --help 2>&1 | head -20`
Expected: Help output renders. CLI is functional.

- [ ] **Step 4: Commit**

```bash
cd /home/v/workspace/projects/mpm
git add cmd/mpm/handlers_cascade.go
git commit -m "refactor(cli): drop Start/StopCascadeMaterializer from cascade materialize"
```

---

## Task 6: Add `RegisterTickHandler` to the scheduler + dispatch in `Tick()`

**Files:**
- Modify: `internal/scheduler/scheduler.go` (add `RegisterTickHandler`, modify `Tick` to invoke tick handlers)
- Test: `internal/scheduler/scheduler_test.go` (add `TestRegisterTickHandler_FiresOnEveryTick`)

**Interfaces:**
- Adds: `Scheduler.RegisterTickHandler(name string, fn func(ctx context.Context) error)`
- Adds: tick handlers fire on every `Tick(ctx)` after `ProcessScheduledTasks` and the wake partition
- Tick handlers are not gated by wake presence — they fire unconditionally
- Tick handlers run **sequentially** after wake dispatch; a failure in one is logged but does not stop subsequent ones

**Note on spec deviation:** The spec says `s.Register("cascade_drain", ...)`. The existing `Register` is wake-driven; cascade_drain should fire on every tick regardless of wake presence. This adds a sibling `RegisterTickHandler` API. Behavior visible to operators is identical; the mechanism matches the spec's intent (handler tied to the 60s tick).

- [ ] **Step 1: Add the tick handler registry**

In `internal/scheduler/scheduler.go`, near the existing `Register` method (line 147), add a parallel method:

```go
// RegisterTickHandler attaches a function to run on every Tick, after
// ProcessScheduledTasks and the wake dispatch. Tick handlers are not
// gated by wake presence — they fire unconditionally on each Tick so the
// scheduler can drive its own periodic work (e.g. cascade drain).
//
// Tick handlers run sequentially after wake dispatch completes. A handler
// that returns a non-nil error is logged but does not prevent later
// tick handlers from running.
func (s *Scheduler) RegisterTickHandler(name string, fn func(ctx context.Context) error) {
    s.tickMu.Lock()
    defer s.tickMu.Unlock()
    if s.tickHandlers == nil {
        s.tickHandlers = map[string]func(ctx context.Context) error{}
    }
    s.tickHandlers[name] = fn
}
```

- [ ] **Step 2: Add the `tickHandlers` field and `tickMu` to `Scheduler` struct**

In the `Scheduler` struct (search `type Scheduler struct`), add:

```go
type Scheduler struct {
    db     *sql.DB
    log    *slog.Logger
    mu     sync.RWMutex
    tickMu sync.Mutex
    handlers    map[string]HandlerFunc
    tickHandlers map[string]func(ctx context.Context) error
    // ... existing fields ...
}
```

- [ ] **Step 3: Modify `Tick` to invoke tick handlers**

In `Scheduler.Tick` (line 204-274), after the wake dispatch section (the `wg.Wait()` block) and before `return executed, nil`, add:

```go
// Tick handlers — fire unconditionally each tick, after wake dispatch.
s.tickMu.Lock()
tickFns := make([]struct {
    name string
    fn   func(ctx context.Context) error
}, 0, len(s.tickHandlers))
for name, fn := range s.tickHandlers {
    tickFns = append(tickFns, struct {
        name string
        fn   func(ctx context.Context) error
    }{name, fn})
}
s.tickMu.Unlock()
for _, h := range tickFns {
    if err := h.fn(ctx); err != nil {
        s.log.Error("tick handler failed",
            "handler", h.name,
            "err", err)
    }
}
```

- [ ] **Step 4: Write the failing test**

In `internal/scheduler/scheduler_test.go`, add:

```go
func TestRegisterTickHandler_FiresOnEveryTick(t *testing.T) {
    s := newTestScheduler(t)

    var count int
    var mu sync.Mutex
    s.RegisterTickHandler("test_counter", func(ctx context.Context) error {
        mu.Lock()
        count++
        mu.Unlock()
        return nil
    })

    for i := 0; i < 3; i++ {
        if _, err := s.Tick(context.Background()); err != nil {
            t.Fatalf("Tick %d: %v", i, err)
        }
    }

    mu.Lock()
    defer mu.Unlock()
    if count != 3 {
        t.Errorf("tick handler ran %d times, expected 3", count)
    }
}

func TestRegisterTickHandler_FailureDoesNotBlock(t *testing.T) {
    s := newTestScheduler(t)

    var ranAfterFailure bool
    s.RegisterTickHandler("always_errors", func(ctx context.Context) error {
        return fmt.Errorf("boom")
    })
    s.RegisterTickHandler("second", func(ctx context.Context) error {
        ranAfterFailure = true
        return nil
    })

    _, err := s.Tick(context.Background())
    if err != nil {
        t.Fatalf("Tick returned error: %v", err)
    }
    if !ranAfterFailure {
        t.Errorf("second tick handler did not run after first handler errored")
    }
}
```

- [ ] **Step 5: Build to verify**

Run: `cd /home/v/workspace/projects/mpm && make build 2>&1 | tail -20`
Expected: **Compiles** (the new `newTestScheduler` helper may need to be added — check the file for an existing helper or add one matching the package's style; if no helper exists, inline the construction using `&Scheduler{db: ..., log: slog.Default(), handlers: map[string]HandlerFunc{}}`).

- [ ] **Step 6: Run the test**

Run: `cd /home/v/workspace/projects/mpm/internal/core && CGO_CFLAGS="-DSQLITE_ENABLE_FTS5" CGO_LDFLAGS="-lm" go test -tags fts5 -run "TestRegisterTickHandler" -v 2>&1 | tail -20`

If the test file lives at `internal/scheduler/scheduler_test.go`, run from the parent module instead:
Run: `cd /home/v/workspace/projects/mpm && CGO_CFLAGS="-DSQLITE_ENABLE_FTS5" CGO_LDFLAGS="-lm" go test -tags fts5 ./internal/scheduler/... -run TestRegisterTickHandler -v 2>&1 | tail -20`

Expected: Tests pass. If `newTestScheduler` is not defined, the test file's existing pattern is the source — adapt accordingly.

- [ ] **Step 7: Commit**

```bash
cd /home/v/workspace/projects/mpm
git add internal/scheduler/scheduler.go internal/scheduler/scheduler_test.go
git commit -m "feat(scheduler): add RegisterTickHandler for unconditional per-tick work"
```

---

## Task 7: Create `CascadeDrainHandler` struct + constructor + accessors

**Files:**
- Create: `internal/scheduler/cascade_drain.go`

**Interfaces:**
- Adds: `CascadeDrainHandler` struct, `CascadeDrainOptions`, `NewCascadeDrainHandler`, `Budget()`, `BatchSize()` accessors
- Tests: defaults applied (Budget=30s, BatchSize=10) — covers Tasks 7-10 collectively

- [ ] **Step 1: Create the file with the struct and constructor**

Create `internal/scheduler/cascade_drain.go`:

```go
// Package scheduler — cascade_drain.go: per-tick cascade drain handler.
//
// Cascade draining is owned exclusively by mpm-scheduler. The CascadeDrainHandler
// runs on every Tick under a per-tick time budget. When the budget runs out,
// the handler yields so the next tick fires on schedule. A pure
// CascadeMaterializer (no clock, no goroutines) is reused for the
// handler's lifetime and invoked once per inner-loop iteration.
package scheduler

import (
    "context"
    "log/slog"
    "runtime/debug"
    "time"

    "github.com/flowbyte-com/mpm/internal/core"
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
    dm          *core.DatabaseManager
    logger      *slog.Logger
    budget      time.Duration
    batchSize   int
    materializer *core.CascadeMaterializer
}

// NewCascadeDrainHandler constructs a handler bound to the supplied
// DatabaseManager. Defaults: Budget=30s, BatchSize=10.
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
```

- [ ] **Step 2: Write the failing test for defaults**

In `internal/scheduler/cascade_drain_test.go`:

```go
package scheduler

import (
    "context"
    "log/slog"
    "testing"
    "time"

    "github.com/flowbyte-com/mpm/internal/core"
)

func TestNewCascadeDrainHandler_AppliesDefaults(t *testing.T) {
    dm := newTestDatabaseManager(t)
    h := NewCascadeDrainHandler(dm, slog.Default(), CascadeDrainOptions{})

    if got, want := h.Budget(), 30*time.Second; got != want {
        t.Errorf("Budget() = %v, want %v", got, want)
    }
    if got, want := h.BatchSize(), 10; got != want {
        t.Errorf("BatchSize() = %d, want %d", got, want)
    }
}

func TestNewCascadeDrainHandler_HonoursOptions(t *testing.T) {
    dm := newTestDatabaseManager(t)
    h := NewCascadeDrainHandler(dm, slog.Default(), CascadeDrainOptions{
        Budget:    5 * time.Second,
        BatchSize: 25,
    })
    if got, want := h.Budget(), 5*time.Second; got != want {
        t.Errorf("Budget() = %v, want %v", got, want)
    }
    if got, want := h.BatchSize(), 25; got != want {
        t.Errorf("BatchSize() = %d, want %d", got, want)
    }
}

// newTestDatabaseManager constructs an in-memory DatabaseManager with
// the schema applied. Uses the existing test helpers if available; adapt
// to match the package's test conventions.
func newTestDatabaseManager(t *testing.T) *core.DatabaseManager {
    t.Helper()
    // IMPLEMENTATION NOTE: the existing test pattern in this package (or
    // internal/core) should be used here. If a shared helper exists in
    // internal/core for tests, import and use it. Otherwise construct:
    //   dm, err := core.NewTestDatabaseManager(context.Background())
    //   if err != nil { t.Fatalf(...) }
    //   return dm
    panic("TODO: wire to existing test helper")
}
```

- [ ] **Step 3: Run the test to verify it fails (compile error expected)**

Run: `cd /home/v/workspace/projects/mpm && CGO_CFLAGS="-DSQLITE_ENABLE_FTS5" CGO_LDFLAGS="-lm" go test -tags fts5 ./internal/scheduler/... -run TestNewCascadeDrainHandler -v 2>&1 | tail -30`
Expected: Compile error or runtime panic on `newTestDatabaseManager` (TODO placeholder). The test exists but is incomplete — fill in the helper per the project's existing test pattern.

- [ ] **Step 4: Wire the test helper using `core.NewTestDM`**

Replace the placeholder `panic` body with the project's centralized helper. `internal/core/testhelpers.go` exposes `NewTestDM(t)` that returns a per-test in-memory `DatabaseManager`:

```go
func newTestDatabaseManager(t *testing.T) *core.DatabaseManager {
    t.Helper()
    return core.NewTestDM(t)
}
```

`NewTestDM` is already used by `changelog_mcp_test.go`, `evidence_store_test.go`, `contradiction_log_test.go`, and others — see `internal/core/testhelpers.go:52` for the canonical pattern.

- [ ] **Step 5: Run the test to verify it passes**

Run: `cd /home/v/workspace/projects/mpm && CGO_CFLAGS="-DSQLITE_ENABLE_FTS5" CGO_LDFLAGS="-lm" go test -tags fts5 ./internal/scheduler/... -run TestNewCascadeDrainHandler -v 2>&1 | tail -10`
Expected: Both tests pass.

- [ ] **Step 6: Commit**

```bash
cd /home/v/workspace/projects/mpm
git add internal/scheduler/cascade_drain.go internal/scheduler/cascade_drain_test.go
git commit -m "feat(scheduler): add CascadeDrainHandler struct + defaults"
```

---

## Task 8: Implement `Run` loop with `queue_empty` and `budget_exhausted` yields

**Files:**
- Modify: `internal/scheduler/cascade_drain.go` (add `Run` and `logYield`)
- Test: `internal/scheduler/cascade_drain_test.go` (add two tests)

**Interfaces:**
- Adds: `(*CascadeDrainHandler).Run(w Wake) error` — wake-driven adapter (signature matches `HandlerFunc`)
- Adds: `(*CascadeDrainHandler).TickHandler() func(ctx context.Context) error` — for use with `RegisterTickHandler`

The handler is invoked by `RegisterTickHandler` so the signature is `func(ctx) error`. We expose both for testing flexibility:
- `Run(w Wake) error` — satisfies the existing `HandlerFunc` signature for any future wake-driven use
- `TickHandler() func(ctx) error` — the actual entry point used by `RegisterTickHandler`

- [ ] **Step 1: Add `Run`, `TickHandler`, and `logYield` to `cascade_drain.go`**

Append to `internal/scheduler/cascade_drain.go`:

```go
// Run is a HandlerFunc-compatible adapter. It delegates to TickHandler
// using the supplied context. Currently unused — the handler is invoked
// via RegisterTickHandler — but kept for future flexibility.
func (h *CascadeDrainHandler) Run(w Wake) error {
    return h.TickHandler()(context.Background())
}

// TickHandler returns a function suitable for Scheduler.RegisterTickHandler.
// The function runs the inner drain loop under a per-call time budget.
func (h *CascadeDrainHandler) TickHandler() func(ctx context.Context) error {
    return h.tickHandler
}

func (h *CascadeDrainHandler) tickHandler(ctx context.Context) (err error) {
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
        if ctxErr := ctx.Err(); ctxErr != nil {
            h.logYield(totalProcessed, totalFailed, "context_cancelled", time.Since(start))
            return nil
        }
        if time.Now().After(deadline) {
            h.logYield(totalProcessed, totalFailed, "budget_exhausted", time.Since(start))
            return nil
        }

        report, err := h.materializer.MaterializeBatch(ctx, h.batchSize)
        if err != nil {
            h.logger.Error("cascade drain batch failed", "err", err)
            h.logYield(totalProcessed, totalFailed, "error", time.Since(start))
            return nil
        }
        totalProcessed += report.Materialized
        totalFailed += report.Failed

        if report.Claimed == 0 {
            h.logYield(totalProcessed, totalFailed, "queue_empty", time.Since(start))
            return nil
        }
    }
}

func (h *CascadeDrainHandler) logYield(processed, failed int, reason string, elapsed time.Duration) {
    h.logger.Info("cascade drain yielded",
        "yield_reason", reason,
        "intents_materialized", processed,
        "intents_failed", failed,
        "elapsed_ms", elapsed.Milliseconds(),
        "budget_ms", h.budget.Milliseconds(),
    )
}
```

- [ ] **Step 2: Add helper for seeding the outbox in tests**

Add to `internal/scheduler/cascade_drain_test.go`. The fixture uses the public `core.EnqueueCascadeInvalidation` API; the per-test `DatabaseManager` from `core.NewTestDM` is already wired with the schema:

```go
// seedCascadeOutbox writes n cascade intents into the outbox via the
// public EnqueueCascadeInvalidation API. Downstream targets reference
// synthetic non-existent IDs — the outbox rows are still valid because
// materialization only requires the downstream metadata, not its existence.
// Returns the invalidation_event_id so tests can scope their queries.
func seedCascadeOutbox(t *testing.T, dm *core.DatabaseManager, n int) string {
    t.Helper()
    ctx := context.Background()
    eventID := fmt.Sprintf("evt-test-%d", time.Now().UnixNano())
    targets := make([]core.CascadeTarget, n)
    for i := 0; i < n; i++ {
        targets[i] = core.CascadeTarget{
            DownstreamArtifactID:   fmt.Sprintf("down-%d-%d", i, time.Now().UnixNano()),
            DownstreamArtifactType: "theory",
            CascadeDepth:           0,
        }
    }
    if err := dm.EnqueueCascadeInvalidation(ctx, "dead-test", "memory", "memory_shredded", eventID, targets); err != nil {
        t.Fatalf("EnqueueCascadeInvalidation: %v", err)
    }
    return eventID
}
```

The exact `core.CascadeTarget` and `core.EnqueueCascadeInvalidation` signatures are in `internal/core/cascade_outbox.go` (search for `type CascadeTarget struct` and `func.*EnqueueCascadeInvalidation`). Adjust field names to match the actual struct — the canonical fixture is in `internal/core/cascade_outbox_test.go:261`.

- [ ] **Step 3: Write the failing tests for `queue_empty` and `budget_exhausted`**

In `internal/scheduler/cascade_drain_test.go`:

```go
func TestCascadeDrain_YieldsQueueEmpty(t *testing.T) {
    dm := newTestDatabaseManager(t)
    logger, drain := captureLogs(t)
    h := NewCascadeDrainHandler(dm, logger, CascadeDrainOptions{Budget: 5 * time.Second})

    // Empty outbox: handler should yield queue_empty immediately.
    err := h.tickHandler(context.Background())
    if err != nil {
        t.Fatalf("tickHandler returned error: %v", err)
    }
    logs := drain()
    if !logsContain(logs, "yield_reason", "queue_empty") {
        t.Errorf("expected yield_reason=queue_empty, got logs: %v", logs)
    }
}

func TestCascadeDrain_YieldsBudgetExhausted(t *testing.T) {
    dm := newTestDatabaseManager(t)
    // Seed enough intents that one batch (BatchSize=10) is well below
    // the total — handler must yield with work remaining.
    eventID := seedCascadeOutbox(t, dm, 50)

    logger, drain := captureLogs(t)
    // Tight budget forces yield after one batch.
    h := NewCascadeDrainHandler(dm, logger, CascadeDrainOptions{
        Budget:    100 * time.Millisecond,
        BatchSize: 10,
    })

    err := h.tickHandler(context.Background())
    if err != nil {
        t.Fatalf("tickHandler returned error: %v", err)
    }
    logs := drain()
    if !logsContain(logs, "yield_reason", "budget_exhausted") {
        t.Errorf("expected yield_reason=budget_exhausted, got logs: %v", logs)
    }

    // At least one intent must remain pending for the next tick.
    var pending int
    if err := dm.SQLDB().QueryRow(
        `SELECT COUNT(*) FROM epistemic_cascade_outbox WHERE invalidation_event_id = ? AND status = 'pending'`,
        eventID,
    ).Scan(&pending); err != nil {
        t.Fatalf("count pending: %v", err)
    }
    if pending == 0 {
        t.Errorf("expected pending intents remaining after budget_exhausted, got 0")
    }
}
```

- [ ] **Step 4: Wire the log-capture helpers using the project's existing pattern**

`internal/scheduler/logging_test.go` already exposes `captureLogger()` (returns logger, buffer, mutex) and `captureLogsFrom(buf, mu)` (parses JSON lines into `[]map[string]any`). Both are package-private in the same `scheduler` package, so `cascade_drain_test.go` calls them directly. Add a thin wrapper that scopes the buffer to the test:

```go
// captureLogs creates a logger + buffer pair for this test and returns
// a closure that returns parsed log records (cleared on each call).
func captureLogs(t *testing.T) (*slog.Logger, func() []map[string]any) {
    t.Helper()
    log, buf, mu := captureLogger()
    return log, func() []map[string]any {
        return captureLogsFrom(buf, mu)
    }
}

func logsContain(logs []map[string]any, key, value string) bool {
    for _, rec := range logs {
        if v, ok := rec[key].(string); ok && v == value {
            return true
        }
    }
    return false
}
```

Update the test bodies to use the new signature: `logger, drain := captureLogs(t); h := NewCascadeDrainHandler(dm, logger, ...); err := h.tickHandler(ctx); logs := drain()`.

- [ ] **Step 5: Build and run the tests**

Run: `cd /home/v/workspace/projects/mpm && CGO_CFLAGS="-DSQLITE_ENABLE_FTS5" CGO_LDFLAGS="-lm" go test -tags fts5 ./internal/scheduler/... -run TestCascadeDrain_Yields -v 2>&1 | tail -30`
Expected: After wiring helpers (Steps 2 and 4), both tests pass.

- [ ] **Step 6: Commit**

```bash
cd /home/v/workspace/projects/mpm
git add internal/scheduler/cascade_drain.go internal/scheduler/cascade_drain_test.go
git commit -m "feat(scheduler): CascadeDrainHandler.Run with queue_empty/budget_exhausted yields"
```

---

## Task 9: Add `context_cancelled` and `error` yields

**Files:**
- Test: `internal/scheduler/cascade_drain_test.go` (add two tests)

**Interfaces:**
- No code changes in `cascade_drain.go` — the yields already exist in the loop
- Adds tests for the two remaining yield reasons

- [ ] **Step 1: Write the failing test for `context_cancelled`**

```go
func TestCascadeDrain_YieldsContextCancelled(t *testing.T) {
    dm := newTestDatabaseManager(t)
    // Seed intents so the handler enters the inner loop.
    seedCascadeOutbox(t, dm, 50)

    logger, drain := captureLogs(t)
    h := NewCascadeDrainHandler(dm, logger, CascadeDrainOptions{
        Budget:    30 * time.Second,
        BatchSize: 10,
    })

    ctx, cancel := context.WithCancel(context.Background())
    cancel() // already cancelled before Run

    err := h.tickHandler(ctx)
    if err != nil {
        t.Fatalf("tickHandler returned error: %v", err)
    }
    logs := drain()
    if !logsContain(logs, "yield_reason", "context_cancelled") {
        t.Errorf("expected yield_reason=context_cancelled, got logs: %v", logs)
    }
}

func TestCascadeDrain_YieldsError(t *testing.T) {
    dm := newTestDatabaseManager(t)
    logger, drain := captureLogs(t)
    h := NewCascadeDrainHandler(dm, logger, CascadeDrainOptions{
        Budget:    30 * time.Second,
        BatchSize: 10,
    })
    // Force MaterializeBatch to fail by closing the underlying sql.DB
    // before the handler runs. The handler's panic-safety net will not
    // engage here — we expect a normal DB error.
    sqlDB := dm.SQLDB()
    sqlDB.Close()

    err := h.tickHandler(context.Background())
    if err != nil {
        t.Fatalf("tickHandler returned error: %v", err)
    }
    logs := drain()
    if !logsContain(logs, "yield_reason", "error") {
        t.Errorf("expected yield_reason=error, got logs: %v", logs)
    }
}
```

**Implementation note for the error test:** The cleanest way to force `MaterializeBatch` to fail is to use a `core.DatabaseManager` constructed against a closed `*sql.DB`. The `internal/core/db_test.go` package has a `newTestDatabaseManager` pattern — reuse or adapt it. If forcing failure is awkward, the test can use `t.Skip` with a comment explaining why and rely on the production error path being covered by the existing `cascade_materializer_test.go` integration tests. Do **not** skip silently — leave a `t.Skip("see Task 9 note")` with the rationale.

- [ ] **Step 2: Run the tests**

Run: `cd /home/v/workspace/projects/mpm && CGO_CFLAGS="-DSQLITE_ENABLE_FTS5" CGO_LDFLAGS="-lm" go test -tags fts5 ./internal/scheduler/... -run TestCascadeDrain_YieldsContextCancelled -v 2>&1 | tail -20`

Expected: Test passes after wiring the helper. (The `TestCascadeDrain_YieldsError` test may be skipped if forcing failure is impractical; document why.)

- [ ] **Step 3: Commit**

```bash
cd /home/v/workspace/projects/mpm
git add internal/scheduler/cascade_drain_test.go
git commit -m "test(scheduler): cover context_cancelled and error yield reasons"
```

---

## Task 10: Add panic-safety and terminal-state invariant tests

**Files:**
- Test: `internal/scheduler/cascade_drain_test.go` (add two tests)

**Interfaces:**
- No code changes — `tickHandler` already has the `defer recover()` and never leaves rows in `processing` on exit (the inner `MaterializeBatch` is atomic)

- [ ] **Step 1: Write the panic-safety test**

```go
type panickingMaterializer struct{ *core.CascadeMaterializer }

func (p *panickingMaterializer) MaterializeBatch(ctx context.Context, limit int) (core.MaterializationReport, error) {
    panic("materializer exploded")
}

func TestCascadeDrain_DoesNotPanicScheduler(t *testing.T) {
    dm := newTestDatabaseManager(t)
    logger, drain := captureLogs(t)
    h := NewCascadeDrainHandler(dm, logger, CascadeDrainOptions{Budget: 5 * time.Second})
    // Swap in a panicking materializer.
    h.materializer = &panickingMaterializer{h.materializer}

    err := h.tickHandler(context.Background())
    if err != nil {
        t.Fatalf("tickHandler must swallow panic and return nil; got err=%v", err)
    }
    logs := drain()
    if !logsContain(logs, "msg", "cascade drain panicked") {
        t.Errorf("expected panic log entry, got: %v", logs)
    }
}
```

- [ ] **Step 2: Write the terminal-state invariant test**

```go
func TestCascadeDrain_NoIntentsLeftInProcessing(t *testing.T) {
    dm := newTestDatabaseManager(t)
    eventID := seedCascadeOutbox(t, dm, 30)
    logger, _ := captureLogs(t)
    h := NewCascadeDrainHandler(dm, logger, CascadeDrainOptions{
        Budget:    100 * time.Millisecond,
        BatchSize: 10,
    })

    _ = h.tickHandler(context.Background())

    var stuck int
    if err := dm.SQLDB().QueryRow(
        `SELECT COUNT(*) FROM epistemic_cascade_outbox WHERE invalidation_event_id = ? AND status = 'processing'`,
        eventID,
    ).Scan(&stuck); err != nil {
        t.Fatalf("count processing: %v", err)
    }
    if stuck != 0 {
        t.Errorf("expected 0 intents in 'processing' after handler exit; got %d", stuck)
    }
}
```

- [ ] **Step 3: Run the tests**

Run: `cd /home/v/workspace/projects/mpm && CGO_CFLAGS="-DSQLITE_ENABLE_FTS5" CGO_LDFLAGS="-lm" go test -tags fts5 ./internal/scheduler/... -run TestCascadeDrain_DoesNotPanicScheduler -v 2>&1 | tail -20`
Run: `cd /home/v/workspace/projects/mpm && CGO_CFLAGS="-DSQLITE_ENABLE_FTS5" CGO_LDFLAGS="-lm" go test -tags fts5 ./internal/scheduler/... -run TestCascadeDrain_NoIntentsLeftInProcessing -v 2>&1 | tail -20`
Expected: Both pass. The `materializer` field needs to be accessible from the test — if it is unexported (lowercase), add an exported setter for tests or use the `internal/scheduler` package's test (same package, so access is fine).

- [ ] **Step 4: Commit**

```bash
cd /home/v/workspace/projects/mpm
git add internal/scheduler/cascade_drain_test.go
git commit -m "test(scheduler): cascade drain panic safety + terminal-state invariant"
```

---

## Task 11: Wire up `cascade_drain` in `cmd/mpm-scheduler/main.go`

**Files:**
- Modify: `cmd/mpm-scheduler/main.go`

**Interfaces:**
- Consumes: `NewCascadeDrainHandler`, `CascadeDrainOptions`, `RegisterTickHandler`

- [ ] **Step 1: Locate the existing `s.Register` block**

Find the existing handler registrations in `cmd/mpm-scheduler/main.go` (the lines that register `snapshot`, `critic_audit`, `gc`, `broadcast`). The new registration goes alongside them.

- [ ] **Step 2: Add the cascade_drain registration**

Add after the existing registrations:

```go
s.RegisterTickHandler("cascade_drain", scheduler.NewCascadeDrainHandler(
    dm,
    logger,
    scheduler.CascadeDrainOptions{
        Budget:    30 * time.Second,
        BatchSize: 10,
    },
).TickHandler())
```

- [ ] **Step 3: Build to verify**

Run: `cd /home/v/workspace/projects/mpm && make build 2>&1 | tail -20`
Expected: Build succeeds.

- [ ] **Step 4: Run the scheduler daemon briefly to confirm registration**

Run:
```bash
cd /home/v/workspace/projects/mpm
./bin/mpm-scheduler &
PID=$!
sleep 3
kill $PID 2>/dev/null
wait $PID 2>/dev/null
```

Expected: The scheduler starts, fires `Tick` immediately, and you see the `cascade drain yielded` log line with `yield_reason=queue_empty` (the outbox is empty in a fresh workspace).

- [ ] **Step 5: Commit**

```bash
cd /home/v/workspace/projects/mpm
git add cmd/mpm-scheduler/main.go
git commit -m "feat(scheduler): register cascade_drain tick handler"
```

---

## Task 12: Update `docs/EPISTEMIC_CASCADES.md` with operator runbook appendix

**Files:**
- Modify: `docs/EPISTEMIC_CASCADES.md` (append "Operational notes — scheduler-driven cascade drain" section)

- [ ] **Step 1: Append the runbook**

Open `docs/EPISTEMIC_CASCADES.md` and append at the bottom:

```markdown
## Operational notes — scheduler-driven cascade drain

Cascade intent draining is now driven exclusively by the `cascade_drain`
handler in `mpm-scheduler`. There is no longer an auto-starting
background goroutine on the DatabaseManager.

### Confirm draining is happening

`mpm-scheduler` logs a `cascade drain yielded` line on every tick where
the handler runs, with one of four `yield_reason` values:

| reason              | meaning                                              |
|---------------------|------------------------------------------------------|
| `queue_empty`       | outbox drained, normal exit                          |
| `budget_exhausted`  | budget (default 30s) ran out, more pending           |
| `context_cancelled` | scheduler shutdown mid-tick, expected on `mpm stop`  |
| `error`             | DB-level error during a batch, investigate logs      |

### "I shredded a root directive but the cascade hasn't materialized"

1. Is `mpm-scheduler` running? Look for `cascade drain yielded` log lines.
2. Is the outbox non-empty?
   ```sql
   sqlite3 src/db/mpm.db \
     "SELECT COUNT(*) FROM epistemic_cascade_outbox WHERE status='pending';"
   ```
3. Is the handler yielding `budget_exhausted` consistently? Check
   `intents_materialized` per tick; raise `CascadeDrainOptions.Budget` if
   you need faster drain after large blasts.
4. Are intents in `status='failed'`? Inspect with `mpm cascade list-dead-letters`.

### Foreground escape hatch

If you don't want to run `mpm-scheduler`, use `mpm cascade materialize`.
No time budget — the operator chose to wait.

### Tuning the budget

`CascadeDrainOptions.Budget` defaults to 30s. The handler yields when
the budget runs out; the scheduler's 60s tick has 30s of headroom.
```

- [ ] **Step 2: Commit**

```bash
cd /home/v/workspace/projects/mpm
git add docs/EPISTEMIC_CASCADES.md
git commit -m "docs: add scheduler-driven cascade drain runbook"
```

---

## Task 13: Full test suite + build + final commit

**Files:** none (verification)

- [ ] **Step 1: Build everything**

Run: `cd /home/v/workspace/projects/mpm && make clean && make build 2>&1 | tail -20`
Expected: Clean build, no errors.

- [ ] **Step 2: Run the full test suite**

Run: `cd /home/v/workspace/projects/mpm && make test 2>&1 | tail -50`
Expected: All tests pass. Investigate any failures before committing.

- [ ] **Step 3: Run `go vet`**

Run: `cd /home/v/workspace/projects/mpm && CGO_CFLAGS="-DSQLITE_ENABLE_FTS5" CGO_LDFLAGS="-lm" go vet -tags fts5 ./... 2>&1 | tail -20`
Expected: No findings.

- [ ] **Step 4: Update CHANGELOG / CLAUDE.md**

If the project maintains a CHANGELOG, add an entry:

```
- Cascade draining now driven exclusively by mpm-scheduler's
  cascade_drain handler (60s tick, 30s budget).
- Removed DatabaseManager.Start/StopCascadeMaterializer and the
  auto-starting goroutine pool.
- New log signal: `cascade drain yielded` with yield_reason taxonomy
  (queue_empty / budget_exhausted / context_cancelled / error).
```

If CLAUDE.md needs an update to reflect the new ownership topology, add a brief note in the cascade-related sections.

- [ ] **Step 5: Final commit (CHANGELOG / doc-only updates)**

```bash
cd /home/v/workspace/projects/mpm
git add CHANGELOG.md CLAUDE.md 2>/dev/null || true
git commit -m "docs: note cascade drain topology change" || echo "no changes to commit"
```

---

## Spec deviation note

The spec section "`cmd/mpm-scheduler/main.go`" calls `s.Register("cascade_drain", ...)`. The existing `Register` method is **wake-driven** — handlers fire only when a `scheduled_wakes` row exists with their kind. To honor the spec's intent (handler runs on every 60s tick, time-budget yielded), Task 6 adds a sibling `RegisterTickHandler` method that fires unconditionally on every `Tick`. The user-visible behavior is identical (a cascade_drain tick every 60 seconds) and the internal mechanism matches the spec's stated intent.

**Recommendation:** amend the spec's `cmd/mpm-scheduler/main.go` section and the `internal/scheduler/scheduler.go` "No API change" claim to reflect the new `RegisterTickHandler` mechanism. This is a 2-line addition to the spec at `docs/superpowers/specs/2026-08-05-adaptive-drain-yielding-design.md`.
