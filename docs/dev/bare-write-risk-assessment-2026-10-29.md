# Issue #178 — the bare-write risk assessment

**Date:** 2026-10-29
**Base:** `origin/main` at `f38a36ea` (post-#177)
**Issue:** #178 — *"90 bare pool writes still lose their artifact to ordinary lock contention — the same defect class as #164, in a different call shape"*
**Scope:** investigation only. No production behaviour was changed; no site was rewritten.

## The finding in one paragraph

Issue #178's claim is **correct, and narrower than its own framing suggests.**
A bare pool write (`dm.db.Exec(...)`) has no statement-level retry, so the only
tolerance is SQLite's own `_busy_timeout=5000` from the production DSN: the
write waits five seconds for the lock, and if the contention outlasts that,
the artifact is lost. The reproduction is deterministic and is in this branch
(`internal/core/bare_write_contention_repro_test.go`).

But the substrate has already been corrected on the paths that matter most. The
memory, lesson, decision, evidence, scratchpad and capability/trust writes all
run through `ExecTracked(query, 5, …)`, and the trust-boundary store runs inside
a transaction with its own budget. The exposure is in the **long tail** — the
handoff write, the wake insert, the scheduler's `MarkFired`, the audit ledger
and the synthesis ledger — plus **39 sites where the caller is never told the
write failed at all**.

The one finding that is not "a lost artifact" is the one that would matter most:
a failed `MarkFired` leaves a wake in `fired = 0`, and the scheduler's
`QueryDueWakes` selects on `fired = 0`, so **the handler runs again**. That is a
duplicated side effect, not a lost one, and no retry budget fixes it after the
fact.

## The method

### The scan is an AST scan, and that is not incidental

The #164 audit recorded why a regex cannot do this job: a regex cannot tell a
retry budget of `0` from a `, 0)` inside a SQL string literal, and it cannot
see a statement whose SQL is a variable. Both of those shapes are present in
this tree. The scanner is therefore `go/parser` + `go/ast`, with identifiers
resolved to their string values before the SQL is classified.

The scanner ships in this branch at `internal/audit/barewritescan/main.go`:

```bash
go run ./internal/audit/barewritescan internal cmd
```

It reports, per write call site: the receiver class (`pool` / `tx` /
`guarded`), the retry budget (for guarded sites), whether the error result is
bound, whether the error is actually **propagated** to the caller, whether the
enclosing function returns an error, and whether the site is lexically inside a
`WithTx` callback.

### The three bugs in my first scanner, recorded because they recur

My first pass reported **117** "pool writes whose error is ignored". That number
was wrong, and the errors are the same class of mistake the #164 audit wrote
about — a scanner that over-reports is a scanner nobody trusts:

1. **`_, err = db.Exec(...)` was classified as "ignored".** `Exec` returns
   `(sql.Result, error)`, so in `_, err = …` the `_` binds the *result* and the
   error **is** captured. The classifier took the first `_` it saw instead of
   the second return slot. The real number is 14.
2. **"The error is bound" was conflated with "the caller is told".** Binding an
   error and then logging it without returning it is a different failure mode
   from discarding it — and it is the more common one. Adding the propagation
   column is what turned 117 into 42.
3. **The propagation check was function-level.** A function that contains one
   propagating write and one non-propagating write was classified by whichever
   it happened to contain. The classifier is now **statement-scoped**: it looks
   at the `if err != nil { … }` that immediately follows the write, which is
   the canonical shape. That was worth 3 more sites and, more importantly, it
   moved ten CLI handlers *out* of the silent-loss set — they print the error
   and exit non-zero, so the operator is told after all.

Each correction made the finding **smaller and sharper**. The final number is
39, and it is a floor: the propagation heuristic is a syntactic approximation,
and a site whose error handling spans several statements can still be
mis-classified.

### What the propagation column means

| `err` | `prop` | Meaning |
|---|---|---|
| `checked` | `propagated` | The error is bound and reaches the caller — returned, or reported to the operator. The caller can react. |
| `checked` | `swallowed` | The error is bound and then logged or dropped. **The caller is never told.** |
| `ignored` | `dropped` | The error result is discarded entirely. **The caller is never told.** |

## The protection model — what actually protects a write today

Read from `internal/core/db.go`:

| Layer | Mechanism | Owns |
|---|---|---|
| Statement (pool) | `DatabaseManager.ExecTracked(query, retries, …)` | ordinary `SQLITE_BUSY`; `retries = 0` disables it |
| Statement (in tx) | `txNode.ExecTracked(query, retries, …)` | ordinary `SQLITE_BUSY`; **returns** `SQLITE_BUSY_SNAPSHOT` immediately so `WithTx` can restart the transaction |
| Transaction | `WithTx` | begin-time busy (5 attempts); stale-snapshot restart (3 attempts) |
| Connection | `_busy_timeout=5000` in the production DSN | the five-second wait, per connection |
| Commit | `ErrTxCommitAmbiguous` | a failed COMMIT is **not** retried; the outcome is reported as unknown |

Two rules follow, and they are the ones that make the correction strategy
bounded:

- **The layer that owns a busy class owns its budget.** A site already inside a
  transaction does not need a second statement budget for the same class, and
  adding one multiplies the two budgets.
- **A statement that fails with `SQLITE_BUSY` applied nothing.** So a
  statement-level retry of an incrementing statement (`success_count =
  success_count + 1`) cannot double-apply. This is proven by reproduction, not
  by argument — see R4 below. The genuinely non-idempotent case is a retry at
  the *transaction* level, where the callback is replayed; `WithTx` already owns
  that budget.

`ErrTxCommitAmbiguous` is already well covered
(`internal/core/tx_commit_ambiguity_test.go`), and `WithTx` already restarts on
a stale snapshot (`internal/core/tx_busy_snapshot_test.go`). Both are **already
protected** and neither is in scope for correction.

## The inventory

**412 direct write call sites** across `internal/` and `cmd/` (non-test).

| Class | Count | Detail |
|---|---|---|
| **Guarded** — `ExecTracked(query, N, …)` | **81** | budget 5: 53 · budget 3: 18 · budget 2: 2 · **budget 0: 8** |
| **Transaction-owned** — `tx.Exec` inside `WithTx` | **159** | 154 propagate · 4 swallow · 1 drop |
| **Bare pool** — `dm.db.Exec` / `dm.SQLDB().Exec` / `s.DB.Exec` | **172** | 133 propagate · **25 swallow** · **14 drop** |

Of the 172 bare pool writes, **133 already propagate the error**: the artifact
is lost, but the caller is told, and can decide. Those are not silent losses.
The **39** that are silent losses are the finding.

### The 23 runtime sites where the write is lost silently

| # | Site | Table | Error | Consequence of a busy failure |
|---|---|---|---|---|
| 1 | `internal/core/synthesis_auto.go:203` `recordSynthRun` | `synth_runs` | dropped | the synthesis ledger row is lost — no record that this content was synthesized |
| 2 | `internal/core/synthesis_auto.go:224` `bumpSynthDedupCounter` | `synth_runs` | dropped | the dedup counter does not move; the watchdog signal is lost |
| 3 | `internal/core/synthesis_auto.go:245` `markMemorySynthCooldown` | `memories` | dropped | the cooldown stamp is lost — **the memory can be synthesized again** |
| 4 | `internal/core/cascade_materializer.go:786` `revertToPending` | `epistemic_cascade_outbox` | dropped | the intent stays `processing` forever — **a lost cascade obligation** |
| 5 | `internal/core/memory.go:3132` `BackfillSessionIDs` | `memories` | dropped | **the row is not updated but `updated++` still runs** — the counter over-reports |
| 6 | `internal/core/memory.go:3187` `LoadSessionsFromDir` | `sessions` | swallowed | the session row is lost; the count under-reports |
| 7 | `internal/core/web_db.go:1274` `recordReferenceInteraction` | `reference_interactions` | dropped | the admission audit row is lost (documented best-effort) |
| 8 | `internal/core/migrate_md.go:392` `PromoteRawMemoryBatch` | `raw_memories` | dropped | the staging row stays `pending` instead of `rejected` — it is retried on the next pass |
| 9 | `internal/core/migrate_md.go:427` `PromoteRawMemoryBatch` | `raw_memories` | dropped | same, for a failed save |
| 10 | `internal/core/audit.go:113` `LogAudit` | `system_audit_log` | swallowed (stderr) | **an audit-trail hole** on every tool dispatch |
| 11 | `internal/core/audit.go:193` `LogAuditForInvocation` | `system_audit_log` | swallowed (stderr) | an audit-trail hole |
| 12 | `cmd/mpm-mcp/audit_hook.go:99` `recordToolInvocation` | `tool_invocations` | swallowed (`slog.Warn`) | **the dispatch audit row is lost** — the drill scorer reads this table |
| 13 | `cmd/mpm/audit_hook.go:127` `recordToolInvocation` | `tool_invocations` | swallowed (`slog.Warn`) | same, on the CLI path |
| 14 | `internal/core/cascade_wake_reconcile.go:156` `deleteOrphanWake` | `scheduled_wakes` | swallowed (audit) | the duplicate wake survives — the per-check delivery cap is what bounds it |
| 15 | `internal/core/broadcast.go:243` `BroadcastMemory` | `shared.bcast_event_wakes` | swallowed (report) | the per-target error is recorded in the broadcast report, but the row is lost |
| 16 | `internal/core/gc_tools.go:309` `RunGC` | `ephemeral_scratchpad` | swallowed | the expired scratchpad prune does not happen; the doctor check verifies |
| 17 | `cmd/mpm/recall.go:660` `handleRecall` | `memories` | swallowed (`slog.Warn`) | the implicit reinforcement of the recalled memory is lost |
| 18 | `cmd/mpm/self_heal.go:247` `handleSelfHeal` | `lessons` | swallowed (warn) | the self-heal audit lesson is lost |
| 19 | `cmd/mpm/handlers_gc.go:275` `handleGC` | `memories` | swallowed | the runtime accrual is lost for that pass |
| 20 | `cmd/mpm/handlers_gc.go:435` `handleGC` | `memories` | swallowed (warn) | the soft-delete of a zero-weight memory does not happen |
| 21 | `cmd/mpm/handlers_gc.go:458` `handleGC` | `memories` | swallowed (warn) | the zombie cleanup does not happen |
| 22 | `cmd/mpm/handlers_epistemology.go:952` `backfillEpistemologyTopics` | `topic_memberships` | swallowed | the backfill silently skips rows |
| 23 | `cmd/mpm/ingest.go:309` `handleIngestCleanup` | `raw_memories` | swallowed | old rejected rows are not deleted |
| 24 | `cmd/mpm/ops_init_topics.go:111` `handleOpsInitTopics` | `topic_memberships` | swallowed (counted) | the backfill skips rows; the error is counted, not reported |
| 25 | `cmd/mpm/drill_cmds.go:200` `handleDrillsRun` | `drill_runs` | dropped | the drill's verdict is lost — the run stays `running` forever |

### The 15 DDL / startup sites

`internal/core/db.go:2147, 2185, 2286` (the shared-DB FTS migration),
`2926, 2974, 3177` (`initUnifiedSchema`), `3561, 3722, 3731, 3783`
(`migrateLessonsToView` / `finishOrRepairLessonsView`), `4016, 4020, 4031`
(`initFTSTables`), `8237` (`migrateWorkEvents`), and `internal/core/memory.go:354`
(`InitSQLite`'s FTS5 table creation).

These are **a different defect class** and are not swept into a retry PR — see
Group 7 below. A failed migration is loud (it warns and, at startup, aborts);
a migration is not safely retryable at the statement level, because a partial
multi-statement `Exec` (the trigger DDL is a multi-statement string) may have
applied part of itself, so a retry can compound the damage. The right treatment
for these is a *transaction*, not a retry budget.

One more is a **test fixture**: `internal/core/testhelpers.go:234`
(`OpenLegacyMemoriesDB`).

### The 5 transaction-owned sites where the error is not propagated

| Site | Table | Note |
|---|---|---|
| `cmd/mpm/handlers_gc.go:408` | `memories` | inside a `WithTx`; the error is logged, not returned |
| `internal/core/db.go:3598` | — | migration DDL inside a transaction |
| `internal/core/db.go:4734` | `memories_fts` | the FTS re-index after a metadata update |
| `internal/core/db.go:4742` | `memories_fts` | the FTS re-index after a metadata update |
| `internal/core/scheduled_tasks.go:260` | `scheduled_tasks` | the "pause the poisoned cron task" write — the error is dropped, so a task that should have been paused keeps running |

The last one is the only interesting site in this group: a poison-pill cron task
that cannot be paused will keep re-injecting wakes.

### The sites that are already protected

These are worth stating explicitly, because the issue's framing ("90 bare pool
writes") could be read as "the substrate's durable writes are unprotected". They
are not:

| Surface | Primitive | Why it is protected |
|---|---|---|
| Memory (`saveMemoryRow`) | `node.ExecTracked(…, 5)` | budget 5 |
| Lesson (`AddLesson`) | `WithTx` → `node.ExecTracked(…, 5)` + read-back | budget 5, inside a transaction |
| Decision (`RecordDecision`) | `WithTx` → `saveMemoryRow` | budget 5, inside a transaction |
| Evidence (`evidence_store.go`) | `node.ExecTracked(…, 5)` | budget 5 (fixed by #176) |
| Scratchpad flush (`tools/handlers.go`) | `dm.ExecTracked(…, 5)` | budget 5 (fixed by #176) |
| Capability / trust boundary (`internal/core/capability/*`) | `n.ExecTracked(…, 2–3)` inside a transaction | budget + transaction |
| Stale-snapshot transactions | `WithTx` restart (3 attempts) | covered by `tx_busy_snapshot_test.go` |
| Ambiguous COMMIT | `ErrTxCommitAmbiguous` | covered by `tx_commit_ambiguity_test.go` |

## Prioritized findings

### F1 — The scheduler can dispatch the same wake twice (highest severity)

`internal/scheduler/scheduler.go:393/399` — `MarkFired` — `scheduled_wakes`.

`QueryDueWakes` selects `WHERE fired = 0 AND target_time <= ?`. `Tick` runs the
system wake's handler, then calls `MarkFired` to set `fired = 1`. `MarkFired`
is a bare pool write with no retry budget.

If it fails with `SQLITE_BUSY`, `fired` stays 0 and **the next tick selects the
same wake and runs the handler again**. The handlers are `gc`, `snapshot`,
`critic_audit`, `broadcast`, `drill`, `cascade_summary`, `cascade` and `cron`.
The error *is* logged (`s.log.Error("mark fired failed", …)`), so there is a
signal — but there is no recovery, and the duplicated side effects have already
happened.

This is the only finding in the audit whose consequence is a **duplicated side
effect** rather than a lost artifact. A lost artifact can be re-created by the
agent; a duplicated broadcast or a duplicated cron wake injection cannot be
un-done.

**Reachability:** the scheduler is a first-class runtime component
(`cmd/mpm-scheduler`, systemd unit), running against the same database as
everything else.

**Correction:** `ExecTracked(query, 5, …)`. A failed statement applied nothing,
so the retry is a clean re-attempt. The `fired = 0 → 1` transition is a
single idempotent UPDATE.

### F2 — The handoff write can lose the session-continuity record

`internal/core/handoff.go:224/252` — `EndSessionV2` — `session_handoffs`.

`mpm_handoff write` → `EndSession` → `EndSessionV2` → a bare pool UPSERT or
INSERT. The error **is** propagated, and the call site already has a read-back
assertion (Defense Triad #3). But there is no retry, so a five-second contention
window loses the handoff: the summary, the commitments and the open questions
for that session are gone.

This is the substrate's session-continuity artifact — the thing the wake context
renders for the next session. It is reachable through a supported surface
(`mpm_handoff`), and the write is an UPSERT keyed on `session_id`, so a retry is
idempotent.

**Correction:** `ExecTracked(query, 5, …)`.

### F3 — A wake can be scheduled without being booked, and be scheduled twice

`internal/core/wake_tools.go:311` (`ScheduleWake`) and
`internal/core/cascade_materializer.go:850` (`markWakeScheduled`).

`scheduleCascadeWake` performs **two** non-atomic bare pool writes: insert the
wake, then flip `wake_scheduled = 1` on the outbox row. If the second fails,
the reconciler sees `wake_scheduled = 0` and re-schedules — a **duplicate
wake**.

The code already acknowledges this ("the duplication risk is bounded by the
per-check delivery cap"), so the duplicate is bounded, not unbounded. But the
window is opened by exactly the failure this issue is about, and both writes
are single idempotent statements.

`ScheduleWake` itself is reachable through `mpm_wakes schedule` and through
`mpm_wake` scheduling in the tool layer.

**Correction:** `ExecTracked(query, 5, …)` on both.

### F4 — The synthesis ledger loses entries silently, and the loss causes duplication

`internal/core/synthesis_auto.go:203/224/245` — `synth_runs`, `memories`.

These three sites **discard the error entirely**. The consequence is not
abstract: `markMemorySynthCooldown` stamps `last_synthesized_at` so the
near-miss detector does not re-merge a memory it already merged. If the stamp
is lost, the memory is synthesized **again** — a duplicate artifact, produced
by the substrate itself, with no error and no log line anywhere.

This is the clearest instance of the "failed diagnostic write is not equivalent
to a lost lesson" asymmetry, running the other way: a failed *diagnostic* write
(`recordSynthRun`) is survivable; a failed *cooldown* write is not.

**Reachability:** the automatic synthesis pass runs unattended.

**Correction:** `ExecTracked(query, 5, …)` and return the error.

### F5 — The cascade outbox can strand an intent in `processing`

`internal/core/cascade_materializer.go:786` — `revertToPending`.

The error is discarded. If the update fails, the intent stays `processing`
forever: no reconciler looks for it, no retry picks it up, and the cascade
obligation is silently never discharged.

**Correction:** return the error. The caller (the materializer's own shutdown
path) must know.

### F6 — The audit ledger has holes, and the tool-dispatch audit row can be lost

`internal/core/audit.go:113/193` (stderr signal only) and
`cmd/mpm-mcp/audit_hook.go:99`, `cmd/mpm/audit_hook.go:127` (`slog.Warn` only).

Both write to provenance tables (`system_audit_log`, `tool_invocations`) on
**every** tool dispatch. A busy failure leaves a hole. The signal exists (stderr
/ `slog.Warn`), so this is not a *silent* loss — but the write is on the
hottest path in the substrate, so it is also the site most likely to meet
contention.

`tool_invocations` is not diagnostic-only: the drill scorer reads it, and
`mpm_context recent_activity` is built from it.

**Correction:** `ExecTracked(query, 5, …)`, keeping the existing stderr/Warn
signal.

### F7 — The #176 guard cannot see `cmd/` or `internal/scheduler/`

The `retries = 0` guard (`internal/core/tx_write_budget_guard_test.go`) walks
`root := filepath.Dir(thisFile)` — that is `internal/core` and nothing else.
The scanner finds **8** guarded sites with a zero budget, of which **4 are
outside the guard's reach**:

| Site | Table | Note |
|---|---|---|
| `cmd/mpm/store_working_context.go:127` `Save` | `ephemeral_scratchpad` | budget 0, with the comment *"retries=0 → standard single attempt, no backoff loop"* |
| `cmd/mpm/store_working_context.go:150` `Delete` | `ephemeral_scratchpad` | budget 0 |
| `internal/scheduler/drill_handler.go:85` | `drill_runs` | budget 0, inside `WithTx` |
| `internal/scheduler/drill_handler.go:127` | `drill_runs` | budget 0, inside `WithTx` |

`cmd/mpm/store_working_context.go` is the **same surface** — the same table —
that #164's audit repaired on the tool side (`tools/handlers.go:5484`, budget 5).
The CLI path (`mpm continue`, `mpm work`) still carries the #164 defect class,
and the guard that is supposed to keep #164 discharged cannot see it.

The two `drill_handler` sites are inside `WithTx`, so they are protected against
the *snapshot* class by the transaction restart; they are exposed to the
*ordinary* busy class, which `WithTx` does not retry.

The remaining four zero-budget sites are in `internal/core/reference_db.go` and
are the **recorded exemption** from the #164 audit — correct as they stand.

### F8 — A backfill reports success it did not achieve

`internal/core/memory.go:3132` — `BackfillSessionIDs`.

```go
if matchedSID != "" {
    s.DB.Exec("UPDATE memories SET session_id = ? WHERE id = ?", matchedSID, memID)
    updated++
}
```

The `Exec` result is discarded **and** `updated++` runs unconditionally. The
function returns a count of updates it did not make. This is the only site in
the audit where the substrate reports a *false success* about a durable
artifact.

**Correction:** check the error; only count what was written.

## Reproductions

`internal/core/bare_write_contention_repro_test.go` — four deterministic tests
on an **isolated, file-backed WAL database** with the **production DSN**
(`SqliteWriteDSN`, so `_busy_timeout=5000` is in force), using the existing
harness from `internal/core/tx_busy_snapshot_test.go`
(`newWALTestDM` / `holdWALWriteLock` / `waitFor`).

```bash
cd internal/core && \
  CGO_CFLAGS=-DSQLITE_ENABLE_FTS5=1 CGO_LDFLAGS=-lm \
  go test -tags fts5 -run 'TestRepro_' -v .
```

| Test | What it establishes |
|---|---|
| **R1** `TestRepro_BarePoolWriteLosesArtifactUnderOrdinaryContention` | **The claim.** A bare pool `INSERT INTO memories` fails with `database is locked` and the row is never written. The test asserts the error is an ordinary busy and **not** `SQLITE_BUSY_SNAPSHOT` — the class that a statement retry clears, which is the class #178 is about. |
| **R2** `TestRepro_GuardedPoolWriteSurvivesTheSameContention` | **The negative control.** The *identical* statement through `ExecTracked(…, 5)` survives the same contention. This is what makes R1 a defect rather than a law of nature. The lock is released only *after* the retry counter has moved, so the test cannot pass by never meeting the lock. |
| **R3** `TestRepro_SwallowedWriteLosesArtifactWithNoSignal` | **The severity discriminator.** `markMemorySynthCooldown` loses the cooldown stamp, and the function has no error result — there is nothing for the caller to check, log, or retry on. |
| **R4** `TestRepro_RetryingABusyStatementDoesNotDoubleApply` | **The correction precondition.** A statement that failed with `SQLITE_BUSY` applied nothing, so retrying an incrementing `success_count = success_count + 1` applies it exactly once. This is what makes "add a retry budget to the counter sites" safe rather than dangerous. |

Observed output:

```
--- PASS: TestRepro_BarePoolWriteLosesArtifactUnderOrdinaryContention (5.09s)
    bare pool write failed with: database is locked
--- PASS: TestRepro_GuardedPoolWriteSurvivesTheSameContention (5.20s)
--- PASS: TestRepro_SwallowedWriteLosesArtifactWithNoSignal (5.10s)
--- PASS: TestRepro_RetryingABusyStatementDoesNotDoubleApply (5.20s)
```

Each test asserts on the **outcome** (the row exists / does not exist), not on
a proxy, and each contention window is entered deterministically — R2 and R4
wait for `dm.BusyRetryCount()` to move, which only happens *after* a failed
attempt, so neither can pass vacuously by never having met the lock.

## The correction strategy

### Group 1 — Release-relevant: the duplicate-dispatch and duplicate-synthesis windows

**PR 1 — the wake and scheduler state transitions.**
Files: `internal/scheduler/scheduler.go`, `internal/core/wake_tools.go`,
`internal/core/cascade_materializer.go`, `internal/core/handoff.go`.
Sites: `MarkFired` (2), `ScheduleWake` (1), `markWakeScheduled` (1),
`markMaterialized` (1), `markFailed` (1), `requeueIntent` (1), `revertToPending`
(1 — also needs to *return* its error), `EndSessionV2` (2), `MarkHandoffRead` (1).

Rationale: these are the writes whose loss produces a **duplicate** rather than
a gap, and they are the ones a supported surface reaches directly.

**PR 2 — the silent-loss sites in the substrate.**
Files: `internal/core/synthesis_auto.go`, `internal/core/audit.go`,
`internal/core/memory.go`, `internal/core/web_db.go`.
Sites: `recordSynthRun`, `bumpSynthDedupCounter`, `markMemorySynthCooldown`
(the error must also be *returned*), `LogAudit`, `LogAuditForInvocation`,
`BackfillSessionIDs` (the count must reflect what was written),
`LoadSessionsFromDir`, `recordReferenceInteraction` (keep best-effort; add a
signal).

### Group 2 — Important post-release: the guard's scope gap

**PR 3 — extend the `retries = 0` guard to the whole repository, and fix what
it finds.**
Files: `internal/core/tx_write_budget_guard_test.go`,
`cmd/mpm/store_working_context.go`, `internal/scheduler/drill_handler.go`.

The guard's root becomes the repository root (or at least `cmd/` and
`internal/scheduler/`). The four sites it then finds get the documented budget
of 5, and the `reference_db.go` exemption stays as it is — it is already
recorded, and keyed to the exact SQL, not to the file.

This PR is the one that makes the rest of the work self-checking. It should
land **early**, not late.

### Group 3 — Important post-release: the trust boundary

**PR 4 — the shared-database writes.**
Files: `internal/core/db.go`, `internal/core/arbitration.go`,
`internal/core/contradiction_verification.go`.
Sites: `RecordGlobalRule`, `PromoteToGlobal`, `RetireGlobalRule`,
`ReinforceSharedMemory`, `AdjudicateContradiction`,
`backfillSharedFTSIfEmpty`, and the broadcast insert in `broadcast.go`.

These write to the **attached** database, which carries **its own** write lock,
independent of the local one. A global rule promoted under contention is lost
with no local artifact to show for it. The operator-gated surfaces
(`confirm = true`) make these rare, which is why this is post-release rather
than release-blocking.

### Group 4 — The long tail

**PR 5 — the remaining bare pool writes**, driven by the scanner rather than by
hand: the CLI handlers, the GC passes, the retrieval-metadata writes, the
works/projections, the `system_config` writes.

Two rules for this PR:

- **One PR per component**, not one PR for the tail. A 40-site mechanical PR
  cannot be reviewed.
- **A site already inside a transaction does not get a statement budget.** The
  159 transaction-owned writes are protected by `WithTx`'s snapshot restart,
  and adding budgets there multiplies budgets.

### Group 5 — Best-effort operations where failure is acceptable

These are documented as best-effort and should be **left alone** — but their
exemption should be *recorded* at the call site, the way #176's guard records
its exemptions, so a future reader does not re-litigate them:

| Site | Why failure is acceptable |
|---|---|
| `internal/core/web_db.go:1274` `recordReferenceInteraction` | the admission audit table is explicitly not on the critical path |
| `internal/core/cascade_wake_reconcile.go:156` `deleteOrphanWake` | the per-check delivery cap filters the duplicate at delivery time |
| `internal/core/gc_tools.go:309` (scratchpad prune) | the doctor check verifies; the rows are ephemeral by definition |
| `internal/core/retrieval_metadata.go:37/65/102` | the callers already discard the error (`_ = dm.RecordRetrieval(...)`); this is observability, not state |

### Group 6 — Dead or unsupported paths

Recorded, not fixed. Fixing them would be work with no traffic behind it:

| Site | Why it is dead |
|---|---|
| `internal/core/db.go:4227` `UpdateSessionSummary` | declared on the `CoreDB` interface but has **no production caller** — only the interface and the definition |
| `internal/core/memory.go:719` `addMemoryDirect` | the fallback for `s.DM == nil` (test fixtures); the live path is `saveMemoryRow` |
| `internal/core/testhelpers.go:234` `OpenLegacyMemoriesDB` | a test fixture |

### Group 7 — DDL and migrations

The fifteen DDL/migration sites are **a different defect class** and should not
be swept into a retry PR — see the inventory section above for the reasoning.

### The safest order

1. **PR 3** (guard scope) — it stops new defects entering while the rest land,
   and it is the smallest.
2. **PR 1** (wake / scheduler / handoff) — the duplicate-dispatch window.
3. **PR 2** (silent-loss sites in the substrate).
4. **PR 4** (trust boundary).
5. **PR 5** (long tail), one component at a time.

### A permanent guard — and what shape it must have

The #164 audit's guard is the right pattern, but a **bare-write** guard cannot
be a zero-tolerance gate today: the baseline is 172 sites. It has to be a
**ratchet** — measure the current baseline, fail when it goes *up* — the same
convention `mpm-lint --gate` already uses for review classes.

The shape it must have, learned from the three bugs in the first scanner:

- it must classify the **propagation**, not just whether the error is bound;
- it must not confuse a `, 0)` inside a SQL literal with a zero budget;
- it must resolve a statement whose SQL is an identifier;
- it must distinguish a bare pool write from a transaction-owned one, so it
  does not demand a budget the transaction layer already provides.

## Release recommendation

**Nothing in this audit should block the next release candidate.**

The reasons, in order of weight:

1. **The durable paths are protected.** Memory, lesson, decision, evidence,
   scratchpad and capability/trust writes all carry budget 5. The substrate's
   core promise — an agent's write survives — holds on the paths agents
   actually use.
2. **The exposure is the long tail**, and 133 of the 172 bare pool writes
   already propagate the error, so the caller is told.
3. **The trigger is narrow.** It needs contention that outlasts a five-second
   `_busy_timeout` — a long GC, a large ingest, a migration — while the write
   is in flight.
4. **The consequences are recoverable or bounded.** A lost handoff can be
   re-written; a duplicate wake is bounded by the delivery cap; a lost audit row
   is a hole, not a corruption.

**With one carve-out, in priority order:**

- **F1 (the scheduler's double dispatch)** is the one finding whose consequence
  is a duplicated side effect rather than a gap. It is four lines of change in
  one file, and it should land before the next RC — not because it blocks, but
  because it is cheap and it is the only finding that cannot be repaired after
  the fact.
- **F4 (the synthesis cooldown)** is the same class of argument and should land
  with it: a lost cooldown produces a duplicate *artifact* that the substrate
  then serves to agents.
- **F7 (the guard's scope gap)** should land before the release too, because
  `cmd/mpm/store_working_context.go` is the same scratchpad surface that #164
  repaired on the tool side and still carries the #164 defect class. It is a
  one-line change plus a guard extension.

Everything else — F2, F3, F5, F6, F8, the trust boundary and the long tail —
belongs in the release *after* the next RC, in the order above.

## What this does not change

No production behaviour. No DDL, no schema, no new table, no new interface, no
new configuration. This branch adds exactly two things: the scanner (audit
tooling, not a gate) and the reproduction tests (evidence).

The live installation was not touched. All database access during this
investigation was read-only.

Docs-Impact: none — this is an investigation record under `docs/dev/`, not a
change to a documented behaviour.
