# Issue #178 — F2: the observational-write latency remediation

**Date:** 2026-11-08
**Issue:** #178 — *"90 bare pool writes still lose their artifact to ordinary lock contention"*
**Branch:** `fix/issue-178-observational-write-latency`
**Base:** `origin/main` at `4a870c8a0d10b4567d9362df4c3dda1fbe0a01ae`
**Depends on:** PR #234 (`fix/issue-178-bare-write-slice2`, head `f769ed9b`) — **open at the time of writing**

This document records the F2 slice: the removal of the *avoidable*
observational-write latency from the MCP request path, without weakening
the durability of genuine user-state writes.

It is a sibling of the #234 slice, not a replacement for it. #234
converts bare pool writes into durable ones; F2 removes the writes that
should never have been on the caller's critical path in the first place.
The two compose, and the composition is measured below.

**The headline:** one MCP wake request under a held SQLite write lock
went from **124.47 s to 33.16 s** — a 73% reduction — and the 33.16 s
that remains is exactly one durable write, attributable to the
millisecond.

---

## 1. Phase 1 — the baseline, reproduced independently

The mission supplied a figure: **124.47 s** for one MCP wake request
under sustained SQLite write contention. That figure is reproduced here
from scratch, with this repository's own harness, over the **real MCP
stdio transport**, against a **disposable workspace**.

The harness is `cmd/mpm-mcp/mcp_contention_latency_test.go`. It does
not instrument the substrate: it holds a real SQLite write lock from a
second connection, issues a real JSON-RPC request, and reads the clock.
The substrate's own watchdog (`<db dir>/watchdog.jsonl`) supplies the
per-statement attribution afterwards, so the breakdown is **observed**,
not asserted.

| tree | wall clock | note |
|---|---|---|
| `main` + #234 | **124.47 s** | the mission's figure, reproduced to 0.01 s |
| `main` + #234 (second run) | 124.46 s | |
| `main` alone | 96.32 s | #234's durable mark-read is 28.15 s of the difference |

The watchdog's own accounting of the 124.47 s request:

| statement | cost | class |
|---|---|---|
| `UPDATE session_handoffs SET read_at …` | 33.14 s | durable user state |
| `INSERT INTO system_audit_log …` | 33.14 s | failure-path audit evidence |
| `5 × INSERT INTO retrieval_metadata …` | 25.04 s | best-effort telemetry |
| `INSERT INTO tool_invocations …` | 33.14 s | per-invocation audit evidence |
| **total** | **124.47 s** | |

Every one of those figures is arithmetically explained by the
substrate's own numbers: `ExecTracked(retries=5)` is six 5 s
busy-handler waits plus 3.1 s of exponential backoff ≈ **33.1 s**, and
a bare pool write is one 5 s busy-handler wait.

**Three of the four writes are observational.** They record what
happened; they do not make it happen. The agent spent 124 s to be told
what it already knew — and, because every one of those writes exhausted
its budget and was **lost**, it did not even get the evidence.

---

## 2. Phase 2 — the classification

The mission requires each statement to be classified, and requires that
`tool_invocations` not be assumed discardable without checking its
contract.

| write | site | class | contract |
|---|---|---|---|
| mark-read | `internal/core/handoff.go:520` (`MarkHandoffRead`) | **durable user state** | the handoff is delivered exactly once; the mark must be durable before the agent is told it was delivered |
| failure-path audit | `internal/core/audit.go` (`LogAudit`) | **essential audit evidence** | "ALWAYS-ON … we do not want a missing audit call to be the difference between 'the agent knew' and 'the agent was blind'" |
| retrieval telemetry | `internal/core/retrieval_metadata.go` (`RecordRetrieval`) | **best-effort telemetry** | its own file header: "fire-and-forget: the user-facing retrieval must not fail because telemetry write failed" |
| per-invocation audit | `cmd/mpm-mcp/audit_hook.go` (`recordToolInvocation`) | **essential audit evidence** | the drill scorer and the recent-activity feed read `tool_invocations` |

### 2.1 `tool_invocations` is not discardable — but it is deferrable

The mission is explicit: *"Do not assume `tool_invocations` can safely
be discarded or shortened without checking its actual contract."*

It cannot be discarded. The table is the substrate's record of every
tool dispatch, and it is read by the drill scorer
(`internal/core/db.go:6917`), by the recent-activity feed, and by the
citation resolver (`internal/critic/missed_opportunity.go:355` cites
`tool_invocations.id`). #231 (merged) already gave the write its
statement-level retry budget precisely so the row survives a contention
window — that durability is intentional and is preserved here.

But **when** it is written is a different question from **whether** it
is written. The row is a record of a dispatch that has already
happened and already been answered to the client. Nothing in the
caller's contract depends on it being on disk first. So it may be
written later — as long as it *is* written later, which is a stronger
guarantee than the inline behaviour it replaces.

### 2.2 The mark-read is not observational, and stays put

The mark-read is genuine user state on three independent grounds:

1. **Delivery is exactly once.** `GatherWakeContext` consumes the
   handoff; the mark is what stops the next session seeing it again.
2. **The substrate asserts it synchronously.**
   `internal/core/contextual_materialization_test.go:261` and
   `internal/core/wake_context_previous_session_test.go:386` both read
   `read_at` immediately after a wake read.
3. **#234's durability for it is approved**, and reducing that budget is
   out of scope for this slice.

It therefore stays **inline, synchronous and unchanged**, and it
accounts for every millisecond of the remaining 33.16 s.

---

## 3. Phase 3 — the correction

### 3.1 What a context deadline cannot do

The first design question was whether the observational writes could be
made non-blocking *in place*, with a bounded attempt, rather than moved
to a background worker. That would have been much simpler.

It cannot. The `mattn/go-sqlite3` driver honours a context deadline by
calling `sqlite3_interrupt`, and **SQLite's busy handler does not
consult the interrupt flag**. A bounded context therefore still waits
the full `busy_timeout`.

Measured (`/tmp/opencode/f2probe/main.go`, against a real held write
lock):

| context budget | elapsed | error |
|---|---|---|
| none | 5.007 s | `database is locked` |
| 50 ms | 5.013 s | `database is locked` |
| 250 ms | 5.006 s | `database is locked` |

Every write attempted on the caller's goroutine costs at least one
busy-handler wait. **Deferral is the only mechanism.** There is also no
second connection pool available: `internal/core/sqlopen_owner_test.go`
enforces that the `DatabaseManager` is the only owner of `*sql.DB`
connections, and that rule is not relaxed here.

### 3.2 The deferred durable-write queue

`internal/core/deferred_write.go` adds one mechanism, with a contract
that is stated in the file's own header:

1. **The caller never waits.** The obligation moves to a background
   worker; the caller's latency stops depending on how long the write
   lock is held.
2. **The obligation is retried until it lands.** A deferred write is
   not a dropped write. The worker re-invokes the *unchanged* substrate
   call — retry budget included — until it returns nil.
3. **Only the clearable class is retried.** Ordinary `SQLITE_BUSY` is a
   wait, and waiting clears it. A constraint violation or a closed
   database is not a wait; those are dropped, loudly.
4. **The queue is bounded** (256). An unbounded queue turns a write-lock
   stall into a memory leak.
5. **A failed obligation does not monopolise the queue.** A contended
   item is put back with a later next-attempt time, so the obligations
   behind it get their turn.
6. **The worker lives as long as the queue does.** It idles when there
   is nothing to do; it exits only when the queue is closed.

**This is stronger than the inline behaviour, not weaker.** The inline
writes had a bounded budget and then lost the row. The failure-path
audit row cost 33.14 s under a held lock and produced **no row at
all** — the evidence was gone. Deferred, it is retried until it lands.

### 3.3 What moved, and what did not

| write | before | after |
|---|---|---|
| retrieval telemetry on the wake path | inline, bare pool write, no budget | `recordRetrievalAsync` → deferred queue |
| failure-path audit on the wake path | inline, `ExecTracked(…, 5)` | `LogAuditDeferred` → deferred queue |
| per-invocation audit row (MCP) | inline, `ExecTracked(…, 5)` | deferred queue |
| per-invocation audit row (CLI) | inline | **unchanged** — the CLI process exits; a deferred write there would be lost |
| mark-read | inline | **unchanged** |
| every other `RecordRetrieval` site | inline | **unchanged** — they are not on the wake path, and their callers read the row back synchronously |

Two implementation details are load-bearing and are called out in the
source:

- **`LogAuditErr` exists** so the queue can learn whether the write
  landed. A function that swallows the error cannot tell it. The
  `LogAudit` wrapper keeps the original stderr reporting byte-for-byte.
- **The stack trace is captured on the calling goroutine**
  (`audit_deferred.go`). Capturing it in the worker would record the
  worker's stack, which is the same for every deferred audit and
  therefore worthless as evidence.

### 3.4 The shutdown flush

`cmd/mpm-mcp` now drains the deferred queue before the database is
closed, with a bounded 5 s budget. Without it, a host that stops the
server would drop the obligations it accepted — and closing the
database first would make every pending obligation fail with "database
is closed" and be dropped, which is exactly the evidence loss this
slice exists to remove.

---

## 4. Two defects found while validating the correction
Both were found by running the suites against the new mechanism, and
both are recorded here because they are the kind of thing that a
"looks right" implementation ships.

### 4.1 The worker died with the queue

The first implementation of the queue's worker returned from its loop
the moment the queue was empty. That killed it **permanently**:
`started` stayed true, so it was never restarted, and every obligation
enqueued afterwards was queued but never written. Any flush then waited
for an obligation that could never land.

The bug was race-dependent and invisible in isolation. It only surfaced
when an earlier test in the same package drained the queue first — a
cross-test hazard that showed up as a 30 s flush timeout in a package
whose own tests were green.

The worker now idles on an empty queue and is woken by the next
obligation. The wake-up is a **close-and-replace channel**, so a signal
that arrives while the worker is busy is not lost.

Regression test: `internal/core/deferred_write_test.go`
→ `TestDeferredWriteQueue_WorkerSurvivesAnEmptyQueue`.

**Mutation control:** re-introducing the original defect makes that
test fail with a 30 s flush timeout; restoring the fix makes it pass.

### 4.2 The wake fold lost its settlement to a concurrent writer

Deferring the per-invocation audit row introduced a second writer on
the shared pool. That exposed a latent defect in `CheckPendingWakes`:

```go
tx, err := dm.db.Begin()      // a bare deferred transaction
...
tx.Exec(`UPDATE scheduled_wakes SET fired = 1 …`)   // a raw write
```

That shape cannot survive a concurrent commit. The fold's read snapshot
goes stale (`SQLITE_BUSY_SNAPSHOT`), and the settlement `UPDATE` fails
with `mark wake fired: database is locked`. The agent silently misses
the wake it was owed.

This is **the same defect class and the same fix** as `ResolveWake`,
in the same file: `ResolveWake` was converted to `dm.WithTx` for
exactly this reason ("the pre-fix code bypassed BOTH retry layers — it
opened its own deferred transaction with `dm.db.Begin()` and wrote with
a raw `tx.Exec`"). `CheckPendingWakes` had the same shape and was
missed by that fix.

The fold now runs inside `dm.WithTx`, so the enclosing transaction
restarts it against a fresh snapshot, and its settlement `UPDATE`
carries the substrate's documented write budget (`wakeFoldWriteRetries =
5`) instead of none — a settlement write that loses its artifact to
ordinary contention is the same defect class #178 exists to close.

`internal/core/wake_tools.go` is **not** one of #234's production files
(`internal/core/db.go`, `internal/core/handoff.go`), so this does not
overlap #234.

### 4.3 A data race the closure introduced

The race detector found it, and it is the classic Go closure trap.

`mcpAdapter` hands the per-invocation write to the deferred queue as a
closure. A Go closure captures the **variable**, not its value, and the
enclosing scope keeps writing to the captured `err`:

```go
result, err := handler(dm, callAC, payload)   // err declared here
…
core.DeferDurableWrite("tool_invocations", func() error {
    return recordToolInvocation(…, err)      // captures the VARIABLE err
})
…
decision, threshold, err := outputPolicy_.Apply(ctx, jsonBytes)  // REASSIGNS err
```

The worker goroutine read `err` while the caller reassigned it: a data
race, and — worse than the race — a potentially **wrong audit row**, with
the worker recording the output-policy error instead of the handler's.

The fix is to copy every value the closure needs into a local that is
never written again, so the worker sees a **snapshot** of the dispatch
as it was. The other three deferred closures capture only parameters
that are never written after the closure is created, so they were never
exposed to this.

### 4.4 The measurement harness needed two corrections
Worth recording, because both would have made the measurement wrong in
a way that flattered the result:

- **The seed must include `mode/default.md` and `persona/default.md`.**
  Their absence makes the active-state resolver emit a diagnostic
  `AuditInfo` row — a *write on the read path* — which costs a full
  durable-write budget under contention. A production workspace has
  those files, so the harness must too.
- **The MCP tools take a `params` envelope**
  (`{"action": …, "params": {…}}`). Seeding without it silently wrote
  no handoff, and the mark-read never fired.

### 4.5 Two PRE-EXISTING defects the CI run exposed

The first CI run of this branch failed the Release gate. Both causes
were pre-existing, and both were reproduced on **pristine `main`** with
the same rate — neither was caused by this slice. They are fixed here
because they make the gate red for this PR, and both are in separate,
clearly-labelled commits a reviewer can drop if they prefer them
separately.

**A ~4% flake in the Continuity sentinel recovery test.** The test
deleted `schema_migrations` from its two snapshot maps *after* it had
already compared them:

```go
changed := continuityDiffSnapshots(before, after)   // includes schema_migrations
…
delete(before, "schema_migrations")                 // too late
delete(after, "schema_migrations")
```

The deletion was clearly meant to exclude `schema_migrations` from the
full comparison — the sentinel's row is the one permitted difference,
and it is compared separately with the sentinel stripped from both
sides. The exclusion simply happened after the comparison it was meant
to govern.

The consequence: the recovery re-applies the migration, so the sentinel
is re-recorded with a **new `applied_at`**. Whenever the recovery
landed in a different *second* than the first application, that
timestamp was reported as a changed pre-existing row. The row sets
were always identical.

| tree | flake rate before the fix | after |
|---|---|---|
| pristine `main` | 7 / 150 | — |
| this branch | 7 / 150 | **0 / 250** |

**The `internal/core` race pack ran under Go's default 10-minute
timeout.** It is the only pack in `test-race` that does not pass
`-timeout` explicitly, and it is the largest one: ~7m20s under `-race`
on a developer host, and a CI runner is ~1.5× slower. The pack died
with `panic: test timed out after 10m0s` — a **timeout, not a test
failure**, and one that takes the whole pack's results with it, so
every other test in the pack was reported as unrun.

Measured to confirm this branch did not cause it:

| tree | `internal/core` under `-race` |
|---|---|
| pristine `main` | 447 s |
| this branch | 441 s |

The fix gives the pack the project's own `CMD_TEST_TIMEOUT` budget
(20m), which the `./cmd/...` pack in the same target already had. No
test's behaviour changes — only the ceiling under which the pack may
run.

---

## 5. Phase 4 — the acceptance evidence

The suite is `cmd/mpm-mcp/mcp_contention_acceptance_test.go`. It
measures the **complete MCP request wall clock** — not an individual
statement — over the real stdio transport, against a disposable
workspace.

### 5.1 The ceiling

| bound | value | why |
|---|---|---|
| baseline | 124.47 s | the measured pre-fix figure |
| acceptance ceiling | **60 s** | a 52% reduction from the baseline, and ≈1.8× the remaining durable write (33.16 s) |
| no-contention ceiling | 15 s | an uncontended request must stay uncontended-fast |
| observational-only ceiling | 20 s | the pre-fix cost of that request was 58.20 s |

### 5.2 The results

Measured on **`main` + #234 + this branch** — the composition that the
mission's baseline describes.

| scenario | wall clock | vs. baseline |
|---|---|---|
| no contention | 0.02 s | 100% |
| lock released after 1 s | 1.05 s | 99% |
| lock released after 3 s | 3.05 s | 98% |
| lock released after 6 s | 6.05 s | 95% |
| lock released after 12 s | 12.06 s | 90% |
| lock released after 20 s | 20.07 s | 84% |
| **sustained contention** | **33.17 s** | **73%** |
| four concurrent requests | 33.16–33.18 s each | 73% |
| only observational writes | 0.02 s | 100% |
| cancellation mid-request | no corruption | — |
| failure-audit + telemetry failure paths | evidence lands | — |
| persistent state after restart | survives | — |

The recoverable-window behaviour is the important one: **the request
returns at the moment the lock is released.** The durable mark-read's
retry budget is long enough to absorb a 20 s window, so a recoverable
window costs the caller the window and nothing more.

### 5.3 Red-to-green and mutation controls

**The measurement itself.** Reverting the three observational writes to
inline — the pre-fix behaviour — on the target tree:

| scenario | pre-fix | post-fix |
|---|---|---|
| sustained contention | **124.48 s** (over the 60 s ceiling — **FAIL**) | 33.17 s (**PASS**) |
| only observational writes | **58.20 s** (over the 20 s ceiling — **FAIL**) | 0.02 s (**PASS**) |
| no contention | 0.02 s (passes both — it is the control) | 0.02 s |

The pre-fix sustained figure of 124.48 s reproduces the 124.47 s
baseline to within 0.01 s, which is the strongest available evidence
that the measurement is the same measurement.

**The queue's worker lifetime.** Re-introducing the §4.1 defect makes
`TestDeferredWriteQueue_WorkerSurvivesAnEmptyQueue` fail with a 30 s
flush timeout; restoring the fix makes it pass.

### 5.4 The evidence that is stronger, not weaker

`TestAcceptance_DeferredEvidenceLands` reproduces the failure path —
the lock is held, so the durable mark-read fails — and then asserts the
evidence is **in the database** once the lock clears:

| evidence | rows landed |
|---|---|
| the failure-path `system_audit_log` row (`component='wake_context'`, `message LIKE '%handoff read failed%'`) | 1 |
| the `retrieval_metadata` telemetry rows | 5 |
| the `tool_invocations` per-invocation row | 1 |

Pre-fix, **the failure-path audit row was lost** — the write burned a
second full durable budget recording the first failure and produced no
row. This slice makes that evidence land.

---

## 6. The remaining worst case, and why it stays

**33.16 s**, and it is one write: the durable mark-read.

Its arithmetic is the substrate's own: `ExecTracked(retries=5)` is six
5 s busy-handler waits plus 3.1 s of exponential backoff. It is
**bounded**, **attributable** (the watchdog attributes 33.16 s of the
33.17 s request to exactly that statement), and **deliberate**.

It cannot safely be removed, because:

- The handoff is delivered **exactly once**. Deferring the mark would
  let a second session see a handoff that was already delivered.
- The substrate's **contracts assert it synchronously** — see §2.2.
- Reducing its retry budget would **weaken durability** that #234
  approved, and the mission forbids it.

What the mission asks for — *"Bound remaining durable-write latency
while preserving recoverable 12–20 s lock windows"* — is satisfied:
the budget absorbs a 20 s window (the request returns at 20.07 s for a
20 s window), and it does not absorb an unbounded one (the request
returns at 33.17 s when the lock is never released).

**The one latency that could not be removed safely** is the durable
mark-read's budget, and the reason it is safe to leave is that it is
the substrate's documented write budget rather than an accident of the
path.

---

## 7. Gates

| gate | result |
|---|---|
| `go build -tags fts5 ./...` (all three modules) | pass |
| `go vet -tags fts5 ./...` (root, `internal/core`, `internal/core/tools`) | pass |
| `go test -tags fts5 ./...` — root module | pass |
| `go test -tags fts5 ./...` — `internal/core` | pass |
| `go test -tags fts5 ./...` — `internal/core/tools` | pass |
| `go test -race -tags fts5 ./...` — the three changed packages | pass |
| `mpm-lint --gate` | pass |
| `make docs-check` | pass |
| `make test-scripts`, `make test-bench`, `make test-release` | pass |

---

## 8. Relationship to #234, and what this slice does not do

**#234 is still open** (`f769ed9b`, MERGEABLE/CLEAN against `main`).
Its production files are `internal/core/db.go` and `internal/core/handoff.go`.
This branch touches **neither** — #234's eight durable-write conversions
are preserved byte-for-byte through the merge, which was verified
conflict-free.

| tree | this branch's change | result |
|---|---|---|
| `main` | mark-read is still a bare write | 5.03 s sustained (the bare write's single busy-handler wait) |
| `main` + #234 | mark-read is durable | **33.17 s sustained**, 124.47 s → 73% reduction |

The acceptance suite is written to be correct on **both** trees. The
`TestAcceptance_ObservationalCostIsOffTheCriticalPath` scenario uses a
workspace with no unread handoff, so the request performs no durable
mark-read and every write left on its path is observational. That
isolates this slice's effect from #234's, and it measures the same
thing on either tree.

**What this slice does not do:**

- It does not change the durable mark-read, or its budget.
- It does not change any `RecordRetrieval` site outside the wake path.
- It does not change the CLI's per-invocation audit write (the CLI
  process exits; a deferred write there would be lost).
- It does not reduce the global SQLite `busy_timeout`, and it does not
  reduce any existing durable-write retry budget.
- It does not close #178. The issue tracks a wider inventory.

---

## 9. Files

| file | what |
|---|---|
| `internal/core/deferred_write.go` | the deferred durable-write queue |
| `internal/core/deferred_write_test.go` | its unit tests, including the §4.1 regression test |
| `internal/core/audit.go` | `LogAudit` split into `LogAuditErr` + wrapper |
| `internal/core/audit_deferred.go` | `LogAuditDeferred` |
| `internal/core/retrieval_metadata_deferred.go` | `RecordRetrievalDeferred` |
| `internal/core/wake_context.go` | the failure-path audit is deferred |
| `internal/core/tools/retrieval_telemetry.go` | `recordRetrievalAsync` |
| `internal/core/tools/handlers.go` | the two wake-path telemetry sites |
| `internal/core/wake_tools.go` | `CheckPendingWakes` moved to `dm.WithTx` (§4.2) |
| `cmd/mpm-mcp/audit_hook.go` | `recordToolInvocation` returns its error |
| `cmd/mpm-mcp/tools.go` | the per-invocation row is deferred |
| `cmd/mpm-mcp/main.go` | the shutdown flush (§3.4) |
| `cmd/mpm-mcp/mcp_contention_latency_test.go` | the measurement harness |
| `cmd/mpm-mcp/mcp_contention_acceptance_test.go` | the acceptance suite |
| `internal/core/migration_continuity_project_model_test.go` | the §4.5 flake fix (pre-existing) |
| `Makefile` | the §4.5 timeout fix for the `internal/core` race pack (pre-existing) |
| `docs/SPEC.md` | §10, "Observational writes do not block the agent" |
