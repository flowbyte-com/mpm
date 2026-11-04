# Issue #178 — reliability corrections, phase 3

> ## ⚠ §§7.1–7.6 (the M1–M6 delivery-state plan) are SUPERSEDED
>
> **Do not implement M1–M6 as written.** An independent engineering review
> found the proposal in §7 is not implementation-safe. One of its steps — the
> claim predicate in §7.3.1 — would convert the open duplicate-execution
> window into a **permanent-loss** window, which is strictly worse than the
> residual it replaces.
>
> | | |
> |---|---|
> | **Superseded** | §§7.1, 7.2, 7.3, 7.4, 7.5 and 7.6 — the whole delivery-state redesign and its M1–M6 migration plan. |
> | **Not superseded** | §§1–6, the cascade materialization correction and its regression tests, which stand on their own review. |
> | **What replaces it** | The corrected requirements in **§7.0**, and the revised engineering report: <https://github.com/flowbyte-com/mpm/issues/178#issuecomment-6606813378> |
>
> §§7.1–7.6 are retained **verbatim** below as the historical record of what was
> proposed and why. They are kept so the reasoning — and the review that
> corrected it — stays legible. **They are not a specification.**

**Date:** 2026-11-03
**Base:** `origin/main` at `77738cfd1663f2aee5d27172c916afe618bf4644`
**Issue:** #178 — *"90 bare pool writes still lose their artifact to ordinary lock contention"*

The base line records the tree every measurement in this document was taken
against. `main` has since advanced (PR #212 merged as `6e47ba99`); that merge
is test infrastructure and touches no file this document measures, so no
finding here is affected by it.

Phases 1 and 2 are recorded in
[`issue-178-corrections-phase-1-2026-10-29.md`](issue-178-corrections-phase-1-2026-10-29.md).
This document records **phase 3**: a fresh reconciliation of the inventory
against the current `main`, the one bounded correction phase 3 makes, and an
honest account of what is still exposed — including the scheduler's
duplicate-delivery window, which phase 3 deliberately does **not** close.

**Issue #178 is not closed by this work.** The issue tracks a wider inventory,
and the highest-impact residual is architectural. Section 7.0 records the
corrected requirements for the delivery-state work that has to precede
closure; the superseded proposal it replaces is retained below for the record
only.

---

## 1. The reconciliation

### 1.1 What was already fixed

Every correction named in phases 1 and 2 is present in `main` at `77738cfd`. The
named audit findings, and where each one now lives:

| Finding | Phase | State at `77738cfd` |
|---|---|---|
| F1 — the scheduler's `MarkFired` settlement was a bare pool write | 1 (#183) | **Fixed.** `core.MarkWakeFired` (`internal/core/wake_settlement.go`) writes through `ExecTracked(…, 5, …)` and reads the row back. |
| F2 — the handoff upsert discarded its error | 1 | **Fixed.** `internal/core/handoff.go` routes both upserts through `ExecTracked(…, 5, …)` and returns the error. |
| F3 — the poisoned-cron-task pause discarded its error | 1 | **Fixed.** `internal/core/scheduled_tasks.go` returns the pause failure; the task keeps running and re-injecting wakes only if the write is *reported*. |
| F4 — the synthesis's cooldown and ledger discarded their errors | 1 (#184) | **Fixed.** The merge and its cooldown now have the same fate: one transaction, one budget. |
| F5 — `revertToPending` could strand an intent in `processing` | 1 | **Fixed.** It carries budget 5 and returns its error. |
| F7 — the write-budget guard could not see `cmd/` or `internal/scheduler/` | 1 (#179) | **Fixed.** The guard scans the whole runtime; the four zero-budget writes it found were repaired. |
| F8 — `BackfillSessionIDs` reported a count it had not written | 1 | **Fixed.** The counter is read back from the database. |
| The durable tail (memory, lesson, decision, evidence, scratchpad, capability store) | 2 (#200–#205) | **Fixed.** All on `ExecTracked` with a budget of 5. |
| The best-effort tail | 2 (#201) | **Fixed.** Best-effort writes are retried; a failure is logged, not lost silently. |
| The startup-DDL tail | 2 (#204) | **Fixed.** Migrations and DDL have their own documented treatment. |
| The bare-write ratchet | 2 (#202/#203) | **Present.** `internal/core/bare_pool_write_ratchet_test.go` — 119 identities / 121 occurrences, each with a recorded reason. |

### 1.2 The counts, re-measured

The phase-0 audit's AST scanner (`internal/audit/barewritescan`, from the
unmerged branch `audit/issue-178-bare-write-risk` @ `74eac183`) was rebuilt and
run against three trees. **These numbers were measured, not quoted.**

| Class | `a0c3521a` (phase-0 baseline) | `f957607f` | `77738cfd` (current `main`) | phase 3 branch |
|---|---|---|---|---|
| **Bare pool** writes | 172 | 172 | **127** | **124** |
| ↳ error not propagated | 39 | 39 | **1** | **1** |
| **Transaction-owned** writes | 159 | 159 | **159** | **159** |
| ↳ error not propagated | 5 | 5 | **4** | **4** |
| **Guarded** (`ExecTracked`) | 81 | 81 | **123** | **126** |
| ↳ budget 0 | 8 | 8 | **4** | **4** |
| ↳ budget 5 | 53 | 53 | **93** | **96** |
| ↳ budget 3 | 18 | 18 | 18 | 18 |
| ↳ budget 2 | 2 | 2 | 2 | 2 |
| ↳ budget not machine-readable | 0 | 0 | 6 | 6 |
| **Total classified sites** | 412 | 412 | **409** | **409** |

The bare-pool count falls by 45 from the baseline: the durable tail moved onto
`ExecTracked`, and the remaining sites were classified rather than removed.
**A lower bare-write count is not, by itself, a stronger correctness
guarantee** — that is why the classification below is the deliverable and the
count is only a receipt.

**These counts are LOWER BOUNDS, and the gap is itself a finding.** Both
scanners read the SQL from a call's *first* argument. `ExecContext(ctx, query,
…)` puts the query in the **second** argument, so every `ExecContext` write is
invisible to them. A dedicated sweep found **12 bare-pool `ExecContext` sites**
that the scanners cannot see (5 more are transaction-owned). **The true
bare-pool count at `main` is therefore 127 + 12 = 139, not 127.** Section 5
enumerates them and phase 3's second PR closes the detector hole.

### 1.3 The scanner and the ratchet disagree by exactly three — and the ratchet is right

The phase-0 scanner reports 127 bare pool writes at `main`; the merged ratchet
allows 124 occurrences (at `main`) across 122 identities. The three-site gap is
`internal/core/db.go:3853`, `:3861`, `:3864`, inside `finishOrRepairLessonsView`.

The phase-0 scanner classifies a write as transaction-owned by looking for the
substring `tx` in the receiver's name. `node.Tx().Exec(...)` does not contain
`tx`, so the scanner mis-classifies the three `node.Tx()` writes as pool
writes. The merged ratchet classifies a `*ast.CallExpr` receiver whose selector
is `SQLDB` as the pool and everything else as transaction-owned, so it gets
these right. **The ratchet is strictly more accurate than the phase-0 scanner
on this tree**, and the phase-0 scanner was never merged, so there is no
divergence to reconcile in `main`.

### 1.4 The ratchet detects a newly introduced unsafe call — verified live

Both guards were confirmed non-vacuous by mutation, in the disposable clone:

- A bare `dm.db.Exec("UPDATE skills SET last_used_at = ? WHERE id = ?", …)`
  injected into `internal/core/skill_db.go` makes
  `TestBarePoolWriteAllowlistIsComplete` fail, naming the file, the line, and
  the normalized SQL.
- A zero-budget `dm.ExecTracked(…, 0, …)` injected into
  `cmd/mpm/drill_cmds.go` — **outside** `internal/core` — makes
  `TestEveryRuntimeWriteHasAStatementLevelRetryBudget` fail. That is the #179
  scope fix, live.

Both mutations were reverted; the tree was verified clean afterwards.

### 1.5 The classification of what remains

| Class | Sites at `main` | Disposition |
|---|---|---|
| **1. Safe, intentionally transaction-owned** | 155 of 159, plus 5 of the 17 invisible `ExecContext` sites | Owned by an open transaction; the transaction's own commit/rollback decides the outcome. |
| **2. Migrations or DDL requiring different treatment** | 17 (`migration-once`) + 12 (`startup-ddl`) in the ratchet; the 4 budget-0 `reference_db.go` sites | Different treatment by construction — a migration runs once under a schema lock, and a DDL statement cannot be retried as a row write. |
| **3. Best-effort diagnostic writes** | 6 | Diagnostic; a failure is logged and the operation continues. |
| **4. Ordinary pool writes that propagate errors** | 126 of the 127 the scanner sees, **plus 11 of the 12 invisible `ExecContext` sites** | The error reaches the caller. The write can still be *lost* to contention — this is the class the ratchet exists to keep from growing. |
| **5. Writes that silently discard errors** | **1** the scanner sees, **plus 1 of the 12 invisible `ExecContext` sites** | `internal/core/testhelpers.go:234` — a test fixture, not runtime. The invisible one is `internal/blobstore/fs.go:697`, whose failure is benign (the next sweep retries) but which was being dropped; phase 3's second PR reports it. |
| **6. Writes whose failure can create duplicated side effects or permanently stranded state** | **1 at `main`** — `internal/core/cascade_materializer.go` (the three outbox transitions) | **This is the defect phase 3 corrects.** |

The 4 transaction-owned writes whose error is not propagated are unchanged by
phases 1–3 and remain:

- `cmd/mpm/handlers_gc.go:437`
- `internal/core/db.go:3649` (a `CREATE` inside a migration transaction)
- `internal/core/db.go:4791` and `:4799` (the `memories_fts` DELETE/INSERT pair
  inside `updateMemoryMetadataTx`)

They are transaction-owned: the enclosing transaction's outcome decides whether
they land, and the FTS shadow is rebuilt by `fts_recovery` if it drifts. They
are recorded, not fixed, and they are not in the "class 6" band.

---

## 2. The defect phase 3 corrects

`internal/core/cascade_materializer.go` had **three bare pool writes** with no
statement-level retry budget, all of them state transitions on the cascade
outbox:

| Function | Statement | What it records |
|---|---|---|
| `markMaterialized` | `SET status = 'materialized', materialized_theory_id = ?` | that the intent reached its terminal state |
| `markFailed` | `SET status = 'failed', terminal_error = ?` | that the intent reached its dead-letter state |
| `requeueIntent` | `SET status = 'pending', attempt_count = ?, next_retry_at = ?` | the **retry counter** |

### 2.1 The mechanism, step by step

1. The materializer claims a batch of intents by transitioning them
   `pending → processing` inside one transaction (`claimCascadeIntents`).
2. It materializes each intent: writes the cascade theory, then transitions the
   outbox row.
3. A bare pool write has **no statement-level retry**, so its only tolerance is
   SQLite's own `_busy_timeout=5000`. If the contention outlasts that, the
   transition is **lost** — the row stays `processing`.
4. The restart reaper in `claimCascadeIntents` resets `processing` rows older
   than 300 s back to `pending`. So the intent is **not permanently stranded**:
   it is revived, and the materialization runs **again**.

That last step is where the damage is done.

### 2.2 A re-materialization was NOT idempotent — and the code said it was

`markMaterialized`'s own docstring claimed:

> *"the caller can leave the row in `processing` for restart-recovery to
> re-materialize (idempotently — the theory is identified by the
> `invalidation_event_id` + `downstream_artifact_id`, so a re-run produces the
> same theory)"*

**That claim was false, and it was falsified empirically before this change
was written.** The theory's cascade metadata carried a wall-clock stamp:

```go
"generated_at": time.Now().UTC().Format(time.RFC3339),
```

The save layer's F19 identity probe (`saveMemoryRow` →
`MemoryIdentityHash(contentHash, tags, metadata)` → `findLiveDuplicateNode`) is
the mechanism that is *supposed* to collapse an identical live save onto the
existing row. It computes the hash over the content hash, the tags, and the
**stable** metadata — and `generated_at` is not on the volatile-key exclusion
list (`_epistemic_snapshot, weight_intent, created, timestamp, source, tags`).
So every run produced a different identity hash, the probe never fired, and a
re-run created a **second theory row**.

Measured, with a disposable file-backed WAL database and the production DSN:

```
FIRST  theory id = "e8b32e0a19b2d711"
SECOND theory id = "cd081e5ab98d58f6"
cascade theory ROWS in the table = 2
```

The consequence is a duplicated artifact: two rows asserting the same
conclusion about the same downstream artifact, of which the **first is
orphaned** — no outbox row points at it — and it is still promotable by an
operator. The outbox's `UNIQUE (dead_artifact_id, downstream_artifact_id,
invalidation_event_id)` deduplicates **intents**, not theories; it does not
help here.

The same defect applies to `requeueIntent`: `attempt_count` is written **only**
there on the retry path. A lost requeue means the counter does not advance, the
next attempt computes the same `newAttempt`, and the dead-letter cap can never
be reached for that intent — the poison pill retries forever.

---

## 3. The correction

### 3.1 The metadata becomes a function of the intent, not of the run

`internal/core/cascade_materializer.go` — `materializeTheory`:

```go
"generated_at": time.Unix(intent.CreatedAt, 0).UTC().Format(time.RFC3339),
```

The intent's creation time is written once, when the invalidation was enqueued,
and identifies the cascade. The theory row's own `created_at` column still
records when the theory was materialised, so no information is lost — and the
F19 identity probe now fires on a re-run, returning the **existing** theory id
instead of writing a second row.

### 3.2 The three transitions carry the substrate's budget of 5

`markMaterialized`, `markFailed` and `requeueIntent` now write through
`cm.dm.ExecTracked(…, 5, …)`, the same budget their siblings `revertToPending`
and `markWakeScheduled` already carry. The writes are `SET`s, not increments, so
a retry applies each one exactly once.

### 3.3 A transition that does not land is reported, not just logged

Before this change, a failed transition was visible only as an audit line, and
the batch report returned a clean-looking summary while an intent sat in
`processing`.

- `processIntent` and `handleMaterializeError` now return `(processResult, error)`.
  The second value is a **state-transition failure**: the classification
  succeeded, but the intent's next state was not recorded.
- `MaterializationReport` gains a `TransitionFailed` counter.
- `MaterializeBatch` increments it and logs the failure.

The batch does **not** abort. The theory exists, so a wake for it is still
useful; the intent stays `processing`, where the restart reaper revives it. The
obligation survives, the delay is bounded, and the operator can see it.

---

## 4. The regressions

`internal/core/cascade_materialization_idempotency_test.go` — five tests, all
deterministic, all on disposable file-backed WAL databases with the production
DSN (`busy_timeout=5000`, `journal_mode=WAL`).

| Test | Property |
|---|---|
| `TestCascadeMaterialization_ReRunReusesTheSameTheory` | A re-materialization reuses the existing theory and leaves exactly one cascade theory row. |
| `TestCascadeTransition_ABareWriteLosesTheTransitionUnderContention` | **Control.** The identical statement through the bare path, under the identical contention, must fail with **ordinary `SQLITE_BUSY` (extended code 5)** and must not land. |
| `TestCascadeTransition_SurvivesOrdinaryContention` | `markMaterialized` survives that contention and lands exactly once. |
| `TestCascadeTransition_RequeueSurvivesOrdinaryContention` | `requeueIntent` survives it, and `attempt_count` advances **exactly once** — a retry does not double-apply. |
| `TestCascadeTransition_ReportsAFailureThatDidNotLand` | A transition that genuinely cannot land is **returned** to the caller. |
| `TestCascadeBatch_ReportCountsATransitionThatDidNotLand` | The batch report counts it in `TransitionFailed`. |

Three properties of the harness are worth naming, because they are what make
the tests evidence rather than assertions:

- **Non-vacuity.** The control test proves the contention actually bites: it
  asserts the bare write **fails** with ordinary busy. Without it, the survival
  tests could pass because contention never occurred.
- **Busy-class discrimination.** The control asserts extended code **5**
  (ordinary `SQLITE_BUSY` — a *wait*, clearable by a statement retry) and
  explicitly rejects **517** (`SQLITE_BUSY_SNAPSHOT` — not clearable by a
  statement retry, and a transaction's problem, not a statement's). The three
  transitions are **pool-level** statements, which cannot produce a snapshot
  conflict at all: a snapshot conflict needs an open read transaction.
- **Ambiguous commits.** The substrate's `ErrTxCommitAmbiguous` marker belongs
  to `WithTx`, because a transaction has a separate COMMIT step whose failure
  leaves the outcome unknown. These three transitions are **autocommit pool
  statements**, so there is no separate commit to be ambiguous about — and
  their statements are `SET`s, not increments, so a retry after any failure
  applies the same value again rather than compounding it. `markMaterialized`
  additionally carries the Defense-Triad read-back assertion, so a transition
  that did not land is *detected*, not assumed.
- **No statement retry inside a transaction.** The three transitions are
  pool-level by design. Nothing here adds a retry to a transaction, so no
  operation layer's transaction budget is multiplied — the layering contract
  pinned by #205 is untouched.
- **Cancellation and bounded retry are unchanged.** The budget is the
  substrate's existing statement-level policy (100/200/400/800/1600 ms), not
  a new one, and `ExecTracked` is bounded by construction.
- **The retry budget engages.** The survival tests wait for
  `dm.BusyRetryCount() > 0` before releasing the lock, so the test fails
  loudly if the write never met contention rather than passing vacuously.

### 4.1 Red before, green after

The behavioural fix was reverted **in place** (the signature changes were kept
so the test file still compiled) and the suite re-run:

```
RED  (pre-fix behaviour restored)
  TestCascadeMaterialization_ReRunReusesTheSameTheory            FAIL  two theory ids
  TestCascadeTransition_ABareWriteLosesTheTransitionUnderContention  PASS  (the control)
  TestCascadeTransition_SurvivesOrdinaryContention                FAIL  "database is locked"
  TestCascadeTransition_RequeueSurvivesOrdinaryContention        FAIL  "database is locked"
  TestCascadeTransition_ReportsAFailureThatDidNotLand            FAIL  no error returned
  TestCascadeBatch_ReportCountsATransitionThatDidNotLand         FAIL  TransitionFailed=0

GREEN (fix restored)
  all six PASS
```

The control test passing in **both** runs is the point: the contention is real
in both, and only the fix's behaviour differs.

**One flaw in the test was found and fixed during this work.** The first
version of the idempotency test passed in the RED run — RFC3339 has *second*
resolution, so two runs inside the same second coincided and the defect hid
itself. The test now sleeps 1.1 s, because the reaper's window is five
minutes, so a real re-materialization **always** crosses a second boundary.

---

## 5. The other finding: a detection hole in both scanners

The audit's conclusion depends on its scanners seeing every write. **They do
not.**

`dm.db.ExecContext(ctx, query, args…)` puts the SQL in `Args[1]` — `Args[0]` is
the context. Both the phase-0 scanner and the merged ratchet read `Args[0]`.

The two miss it differently, and the phase-0 scanner's miss is the more
dangerous because it **looks** like coverage:

- The **phase-0 scanner** matches the *method name* `ExecContext` and then
  reads `Args[0]`. `Args[0]` is the context — an identifier with no literal
  binding — so the resolved SQL is empty, no write verb matches, and the site
  is skipped as a non-write. A reader of the scanner would reasonably
  believe the shape was handled.
- The **merged ratchet** matches the *method name* `Exec` only, so the shape is
  never visited at all.

A dedicated sweep of every `Exec*` call in the tree found **17 write sites
that neither scanner can see**:

| Site | Receiver | Consequence of a lost write |
|---|---|---|
| `internal/core/wake_tools.go:117` | pool | The cascade-summary wake is lost. The summary is a *report*, and the next tick produces another one. |
| `internal/core/cascade_wake_reconcile.go:108` | pool | The **atomic claim** of a wake. A failure rolls the wake back rather than stranding it. |
| `internal/core/compact.go:1019` | pool | The compaction ledger entry (`compaction.last_run`). The reader treats a missing row as "no pressure" — the safe default. |
| `internal/core/reference_db.go:349`, `:702` | pool | An embedding for one reference chunk. The chunk is re-embeddable; the counter is incremented only on success. |
| `internal/core/active_uptime.go:223` | pool | A settling baseline. `INSERT … ON CONFLICT DO NOTHING` — idempotent by construction. |
| `internal/core/similarity_candidates_store.go:1258` | pool (pinned `*sql.Conn`) | A `shared.contradiction_log` entry, written `INSERT OR IGNORE`. The error **is** propagated; the write is simply not retried. |
| `internal/blobstore/fs.go:268` | pool | The blob's metadata row. The orphan file is removed on failure. |
| `internal/blobstore/fs.go:397`, `:577` | pool | A blob row deletion. `Delete` is explicitly idempotent and returns the error. |
| `internal/blobstore/fs.go:697` | pool | An orphan row deletion in the GC sweep. **The one site of the 12 whose error was discarded** — the next sweep retries it, so the consequence is a delay, not a stranded artifact. Phase 3's second PR reports the failure instead of dropping it. |
| `cmd/mpm/backfill_embeddings.go:289` | pool | A bulk embedding clear. The backfill is re-runnable. |
| `internal/core/backfill_snapshots.go:363` | transaction | Transaction-owned. |
| `internal/core/compact.go:911` | transaction | Transaction-owned. |
| `internal/core/compact_requeue.go:265` | transaction | Transaction-owned. |
| `internal/core/migration_memories_affinity_rebuild.go:560` | transaction | Transaction-owned (a migration). |
| `internal/core/migration_memories_created_at_not_null.go:443` | transaction | Transaction-owned (a migration). |

**None of the 12 bare-pool sites has a stranded-or-duplicated consequence.**
This is a **detection** hole, not a live-defect hole — but a detector that
cannot see a write shape cannot keep that shape honest, and the shape is
one line away from every future caller.

The correction is a second, separate PR: the ratchet is extended to the
`ExecContext` shape, the 12 sites are baselined with a recorded reason each,
and the one site that discarded its error is corrected so the baseline is
truthful.

**What happens to the phase-0 scanner itself.** It lives on the unmerged
branch `audit/issue-178-bare-write-risk` @ `74eac183` and was never merged
into `main`, so there is nothing in `main` to fix. It should be corrected or
retired **on that branch**, so the historical inventory can be re-derived
correctly if anyone needs to reproduce it. The **merged ratchet is now the
enforcement mechanism** — it is in `main`, it runs on every commit, and
phase 3's second PR makes it see the shape the scanner missed.

---

## 6. The scheduler's duplicate-delivery crash window

### 6.1 It remains open. It is a design property, not a defect.

The window:

1. the scheduler selects a due wake (`WHERE fired = 0 AND target_time <= ?`);
2. it runs the wake's **handler**;
3. it settles the wake (`fired = 1`);
4. **the process dies between (2) and (3)**.

The wake is still `fired = 0`. The next process selects it again and **runs the
handler a second time**. No retry budget can close this: the settlement is not
what failed — the *process* died.

This is pinned deliberately, so it cannot be forgotten:

```go
// internal/scheduler/wake_settlement_test.go
func TestWakeDelivery_ARestartBeforeSettlementLeavesTheWakePending(t *testing.T)
```

The test asserts the *documented residual*: the wake stays pending and the
handler runs again. It is a characterisation test, and it passes on `main`
today. Phase 1 recorded the same conclusion; phase 3 re-confirmed it by
reading the settlement path and the producer inventory, and by running the
whole scheduler delivery suite (7 tests, all green).

### 6.2 The idempotency guarantees have NOT changed

| Handler | Idempotency | Changed since phase 1? |
|---|---|---|
| `gc` | Its own CAS claim — a second run finds the work already claimed and exits. | No. |
| `broadcast` | **None.** It shells out to `mpm ops broadcast`, which fans out to other agents. | No. |
| `drill` | **None.** It spawns a harness. | No. |
| `snapshot` | Cheap guards only. | No. |
| `cascade_summary` | Cheap guards only. | No. |

**How the two dangerous kinds get their wakes.** Neither `broadcast` nor
`drill` is authored by an automatic producer: a sweep of the tree finds no
producer that writes `"kind":"broadcast"` or `"kind":"drill"` into a wake's
metadata. Both are authored by an **explicit operator or agent action**
(`mpm ops broadcast`, `mpm drill run`). So the window needs a *requested*
wake to be duplicated — it is not a self-inflicted loop. That lowers the
frequency; it does not lower the consequence, because the fan-out to other
agents and the spawned harness have already happened and cannot be undone.

A wake whose kind has **no registered handler** (for example `cron`, which
the scheduler does not register) is settled with an error rather than
dispatched, so it cannot become a repeated side effect either.

And the **producer** side is unchanged in the ways that matter: the wake kinds
are still authored by the same producers, and the phase-1 blocking gate
(`WakeKindBlocked`, enforced in `core.ScheduleWake` at the DB layer, with the
consumer-side default `notification` in `scheduler.Wake.Kind()`) still applies
to **every producer that goes through `core.ScheduleWake`**.

**Three producers insert into `scheduled_wakes` directly and therefore bypass
that gate** — a fact worth stating rather than glossing, because the gate's
reach is narrower than "every producer":

| Producer | `kind` in its metadata | Consumer-side kind |
|---|---|---|
| `internal/core/scheduled_tasks.go:247` (the cron task's wake) | `CronWakeKind`, explicit | `CronWakeKind` |
| `internal/core/wake_tools.go:118` (`ScheduleCascadeSummaryWake`) | `"cascade_summary"`, explicit | `cascade_summary` |
| `internal/core/capability/store_cascade.go:380` (`scheduleRollbackWakeTx`) | **none** | `"notification"` (the consumer-side default) |

The third was **added since phase 1** and was audited here. It authors a
`scheduled_wakes` row with no `kind`, so it resolves to `"notification"` — the
same default as every other kind-less producer. It is **not** an automatic
producer of the non-idempotent kinds, so it does not widen the window.

### 6.3 Why phase 3 does not close it

Closing it needs durable claims, leases, acknowledgements, or per-handler
deduplication — a delivery-state redesign across the scheduler, the core, and
every handler. The mission explicitly asks for a **proposal** in that case,
not an unreviewed change. Phase 1 recorded the first sketch; **§7.0** records
the corrected requirements an implementation has to meet, after an
independent review found the plan this section originally proposed was not
implementation-safe.

---

## 7. The delivery-state redesign — SUPERSEDED proposal, and the corrected requirements

### 7.0 The corrected delivery-state requirements (this section supersedes §§7.1–7.6)

**§§7.1–7.6 below are superseded. Do not implement M1–M6 as written.** This
subsection states what a delivery-state redesign must get right; it does not
restate the redesign itself. The full design — the schema, the migration
mechanics, the regression tests for the first step, and the corrected PR
sequence — is in the revised engineering report:

> **Revised engineering report (independent review, 2026-11-03):**
> <https://github.com/flowbyte-com/mpm/issues/178#issuecomment-6606813378>
>
> That report is the authoritative source for the delivery-state work. What
> follows is the list of corrections it established, each of which §§7.1–7.6
> violate. The report also carries a handler-by-handler idempotency matrix,
> the failure-window analysis, and the migration and rollback contract.

**The eight corrections.**

**C1 — `fired` is retirement, not delivery.** It records "no longer pending
for normal delivery", never "the handler ran". `fired = 1` is written by five
paths across the codebase: the scheduler settlement
(`internal/core/wake_settlement.go:96,103`), the agent fold
(`internal/core/wake_tools.go:671`), explicit resolution
(`internal/core/wake_tools.go:478`) and the two retirement sweeps
(`internal/scheduler/wake_expiration.go:182,501`). **Four of the five never run
a handler.** §7.5's M1 backfill would therefore assert deliveries that did not
happen.

**C2 — `delivery_state` must be an independent axis.** Not a projection of
`fired`, and not a replacement for it. §7.2 declares `fired` a projection of
`delivery_state = 'delivered'`; that conflates two different questions — "has
the scheduler run this wake's handler?" and "is this wake still pending?" —
and it is not implementable, because it would require *every* writer of
`fired`, including the fold and the sweeps, to also write `delivery_state`.
Only three of the sixteen `fired` reader sites ask a dispatch question; the
other thirteen ask a retirement question and must keep reading `fired`.

**C3 — Do not backfill `delivered` from `fired`.** There is no column on a
`scheduled_wakes` row that records "the scheduler's handler ran":
`fired_by` is written only by the resolver, `dispatched_at` only by the
notification claim, and `metadata.last_error` only by the settlement — and it
is set to `NULL` on a *successful* settlement
(`internal/core/wake_settlement.go:97-99`). A backfill would fabricate a
delivery and would become load-bearing the moment any reader trusts the
column. `kind = 'cron'` rows are never deliverable at all: no handler is
registered for them, and the notification claim excludes them.

**C4 — An expired claim must actually be reclaimable.** §7.3.1's predicate is

```sql
WHERE id = ? AND delivery_state = 'pending'
  AND (lease_expires_at IS NULL OR lease_expires_at < ?)
```

A crashed claim leaves `delivery_state = 'claimed'`, so the predicate never
matches it and the lease clause is dead code. The lease has to be reachable:
`delivery_state = 'pending' OR (delivery_state = 'claimed' AND
lease_expires_at < ?)`. §7.3.2 states the opposite intent — *"a claim whose
`lease_expires_at` has passed may be re-claimed"* — so intent and SQL
disagree. **As written, the design turns the open duplicate-execution window
into a permanent-loss window: the wake is claimed, the process dies, and
nothing ever delivers it again.** That is strictly worse than today's
residual.

**C5 — Preserve `target_time` eligibility.** §7.3.4 replaces the due-wake
predicate with `WHERE delivery_state = 'pending' AND (lease_expires_at IS NULL
OR lease_expires_at < ?)` — dropping `target_time <= ?` entirely. Every wake,
including ones scheduled for next week, becomes due. The due-time predicate is
not part of the delivery-state axis and must survive it.

**C6 — Keep notification delivery separate from scheduler-owned delivery.**
The notification path has its own claim (`internal/scheduler/dispatch.go:78`),
its own audit trail (`dispatched_at`) and its own retirement semantics that
the agent fold depends on. It is not the source of the duplicate-execution
risk. Merging the two axes is what makes §7.5's M2 — *"teach every reader of
`fired` to read `delivery_state` instead"* — incoherent: it would touch
sixteen reader sites and break thirteen of them.

**C7 — Account for rollback to older binaries.** `mpm-update --rollback`
restores the runtime and deliberately leaves the database alone; the updater
never opens it. An older binary's settlement writes `fired = 1` and **not**
`delivery_state = 'delivered'`. Under §7.3's claim predicate that row is still
`delivery_state = 'pending'` — i.e. re-claimable and re-executed. A migration
window followed by a rollback would therefore *create* a duplicate-execution
window that does not exist today. The migration must be strictly additive and
NULL-defaulted, and the rollback policy must be written down.

**C8 — Do not promise exactly-once external effects through SQLite
settlement alone.** A settlement records that a handler ran; it cannot make
the handler's external effect happen once. A broadcast fans out to other
agents' processes and a drill spawns a harness — neither is inside the
transaction. §7.4's per-handler guards are what bound a duplicate's cost, and
§7.4's premise needs correcting on two handlers: `broadcast` already
deduplicates its fan-out by content (`shared.bcast_event_wakes.wake_id` is the
primary key and the insert is `INSERT OR IGNORE`), and `gc` takes its own
24-hour compare-and-swap cooldown **before** its work
(`cmd/mpm/handlers_gc.go:174`), so it is at-most-once and its residual risk
is a *lost* GC rather than a duplicated one.

**What a first implementation must not do.** The superseded plan's M1
backfills `delivery_state` from `fired`, and its M2 switches the readers.
Both are unsafe, for the reasons in C1–C3 and C6. A first step must be
**strictly additive and behaviour-neutral**: new columns with no backfill, no
reader and no writer touched, and a migration that is idempotent, forward-only
— schema rollback is not permitted — and applied by the first new-binary
process that opens the database, never by the updater, which does not open it.

**The full requirements, the schema, the migration mechanics, the regression
tests for the first step, and the corrected PR sequence are in the revised
engineering report.** This document does not restate them: the report is the
specification, and §7.0 is the list of ways the superseded plan fails it.

*Everything below this line is the superseded proposal, retained verbatim as
the historical record of what was proposed and why. It is **not** a
specification and **must not be implemented as written** — read §7.0 first.*

---

### 7.1 The target semantics

> **SUPERSEDED — see §7.0. Not to be implemented as written.**

**At-least-once delivery with idempotent effects** — the standard, achievable
semantics. *Not* exactly-once: exactly-once would require the effect and the
record of the effect to be one atomic act, and a broadcast to another agent's
process cannot be.

### 7.2 The schema change

> **SUPERSEDED — see §7.0 (C2, C3). Not to be implemented as written.**
>
> In particular: the `NOT NULL DEFAULT 'pending'` column below cannot
> distinguish "not tracked" from "pending", and the projection this section
> declares for `fired` is not implementable.

Add three columns to `scheduled_wakes`:

```sql
ALTER TABLE scheduled_wakes ADD COLUMN delivery_state TEXT NOT NULL DEFAULT 'pending';
ALTER TABLE scheduled_wakes ADD COLUMN lease_expires_at INTEGER;
ALTER TABLE scheduled_wakes ADD COLUMN claimed_by TEXT;
```

`delivery_state` ∈ `pending | claimed | delivered`. The existing `fired`
column becomes a **projection** of `delivery_state = 'delivered'`, kept for
backwards compatibility with every existing reader (`mpm wake`,
`check_wakes`, `ResolveWake`).

### 7.3 The claim, the lease, and the settlement

> **SUPERSEDED — see §7.0 (C4, C5, C7). Not to be implemented as written.**
>
> In particular: the claim predicate in 7.3.1 below makes lease recovery
> unreachable, and the due-wake query in 7.3.4 below drops `target_time`.

1. **The claim is a conditional `UPDATE`** inside one short transaction:

   ```sql
   UPDATE scheduled_wakes
      SET delivery_state = 'claimed',
          lease_expires_at = ?,
          claimed_by      = ?
    WHERE id = ?
      AND delivery_state = 'pending'
      AND (lease_expires_at IS NULL OR lease_expires_at < ?)
   ```

   Two claimants cannot both win: the second sees `delivery_state != 'pending'`
   and updates zero rows.

2. **The lease makes a crashed claim recoverable.** A claim whose
   `lease_expires_at` has passed may be re-claimed. This is what makes recovery
   **bounded** rather than permanent: a crashed scheduler's wakes come back
   after the lease, not never.

3. **The settlement completes the claim** (`claimed → delivered`) and sets
   `fired = 1` in the *same* statement, so the two can never disagree.

4. **The due-wake query changes** from `WHERE fired = 0` to
   `WHERE delivery_state = 'pending' AND (lease_expires_at IS NULL OR lease_expires_at < ?)`.

### 7.4 Per-handler idempotency — the part that does the real work

> **SUPERSEDED — see §7.0 (C8). Not to be implemented as written.**
>
> In particular: the premise for `broadcast` and `gc` in the table below is
> wrong. `broadcast` already deduplicates its fan-out by content, and `gc` is
> at-most-once because its compare-and-swap cooldown is taken before the work.

The lease bounds *how often* a duplicate happens; the per-handler guard
bounds *what a duplicate costs*. Each handler needs one of:

| Handler | Guard |
|---|---|
| `broadcast` | Record the wake id in the same transaction that records the broadcast, under a unique index on `(wake_id)`. A second delivery finds the row and exits. This requires the broadcast's *own* record to be the dedupe key — the fan-out to other agents still needs a delivery key. |
| `drill` | Same: a `drill_runs` row keyed by wake id, with a unique index. A second delivery finds the run and does not spawn a second harness. |
| `gc` | Already has its CAS claim. Verify it under the new lease semantics: a re-claimed wake must re-check the CAS, not assume the claim held. |
| `snapshot`, `cascade_summary` | Cheap guards: the work is a read or a summary write, and a repeat is harmless. They need the lease, not a dedupe key. |

### 7.5 The migration plan, in testable steps

> **SUPERSEDED — see §7.0 (C1, C2, C3, C6, C7). Not to be implemented as written.**
>
> **Every row of the table below is superseded.** In particular M1's backfill
> and M2's reader switch are unsafe; see §7.0 C1–C3 and C6.

Each step is independently mergeable and independently revertible.

| Step | Change | Test that proves it |
|---|---|---|
| **M1** | Add the three columns; backfill `delivery_state` from `fired` (`delivered` where `fired = 1`, else `pending`). | A migration test on a fixture database with a mix of `fired` values. |
| **M2** | Teach every *reader* of `fired` to read `delivery_state` instead. No behaviour change yet. | The existing wake suite must pass unchanged. |
| **M3** | Add the claim with the conditional `UPDATE` and the lease. The due-wake query switches to `delivery_state`. The settlement writes `delivery_state = 'delivered'` **and** `fired = 1` together. | A concurrency test that runs N claimants against one wake and asserts exactly one wins. A lease-expiry test that asserts a crashed claim is re-claimed after the lease. |
| **M4** | `broadcast` and `drill` gain their wake-id dedupe rows and their unique indexes. | A duplicate-delivery test for each: deliver the same wake twice and assert one broadcast record and one drill run. |
| **M5** | The scheduler's settlement path keeps its `ExecTracked(…, 5, …)` budget. The claim is a transaction, so the busy-class layering contract from #205 applies: ordinary busy surfaces to the caller, and the *scheduler tick* retries — the claim must not retry statements inside its own transaction. | The existing `internal/core/tx_busy_layering_contract_test.go` plus a new test that a busy claim is retried at the tick level, not inside the transaction. |
| **M6** | The characterisation test `TestWakeDelivery_ARestartBeforeSettlementLeavesTheWakePending` is **replaced** by its opposite: a restart before settlement leaves the wake *claimed*, and after the lease expires it is re-delivered **exactly once** in total effect. | The rewritten test. |

### 7.6 What M6 must not claim

> **SUPERSEDED — see §7.0 (C8). Not to be implemented as written.**

A re-delivery after a lease expiry is still a **re-delivery**. The guarantee
M6 buys is **one effect**, not one execution: the handler may run twice, but
the second run must be a no-op. That is the honest limit of the design, and
the test must assert the *effect* count, not the *execution* count.

---

## 8. Remaining risks, ordered by impact

| # | Risk | Impact | Status |
|---|---|---|---|
| **1** | **The scheduler's duplicate-delivery crash window.** A crash between the handler and the settlement re-runs the handler. `broadcast` fans out to other agents; `drill` spawns a harness. | **High** — the effects cannot be undone. | **Open by design.** §7.0 records the corrected requirements; the superseded plan in §§7.1–7.6 is not to be implemented. Not fixed by phase 3. |
| **2** | **The cascade outbox transitions.** A lost transition revived the intent and duplicated the theory; a lost requeue defeated the dead-letter cap. | **High** — duplicated artifacts and an unbounded retry. | **Fixed in phase 3.** |
| **3** | **The ratchet cannot see `ExecContext`.** A new bare write in that shape would not be caught by either guard. | **Medium** — a detector hole, not a live defect (all 12 existing sites are benign). | **Corrected in phase 3's second PR.** |
| **4** | **A duplicate cascade WAKE for the same theory.** After a revival, the same theory can be woken twice — the theory is now shared, but `scheduleCascadeWake` does not dedupe against the intent's `wake_scheduled` flag in that path. | **Low** — bounded by the scheduler's per-check delivery cap. | **Open and recorded.** Not fixed: the fix belongs with the delivery-state redesign, which must define per-handler dedupe anyway. |
| **5** | **4 transaction-owned writes whose error is not propagated**, and **1 bare pool write that discards its error** (a test fixture). | **Low** — enumerated in §1.5. | **Recorded.** Not fixed: each is either transaction-owned or a fixture. |
| **6** | **Pre-existing `mpm doctor` warnings**: 8 memories without an embedding fingerprint, 1 scratchpad past its `decay_at`. | **None for #178** — unrelated to write contention. | **Out of scope.** |

---

## 9. What remains before #178 can responsibly be closed

The issue's title is about the bare-write count. The count is not the goal, and
a lower count is not the evidence. What #178 actually claims is that **a
durable write is no longer lost to ordinary lock contention**. That is now true
for every site the audit can see, with two qualifications:

1. **The detection hole must be closed.** Until the ratchet sees the
   `ExecContext` shape, the claim "every durable write is protected" is true
   only of the shapes the scanners recognise. Phase 3's second PR closes it.
2. **The scheduler's delivery window must be resolved — or explicitly
   accepted.** The window is not a write-contention defect: no retry budget
   can close it, and the phase-1 correction already removed the *contention*
   path to it (a lost settlement is now retried). What remains is a
   process-death window, which needs the delivery-state redesign whose
   requirements are recorded in **§7.0**.

**The recommended path to closure:**

1. Merge phase 3's two PRs (the cascade correction and the ratchet coverage).
2. Land the delivery-state redesign as its own, separately-reviewed series
   built to the requirements in **§7.0** — *not* to M1–M6, which are superseded
   (§§7.1–7.6).
3. Re-run the audit against the resulting tree and confirm that the bare-pool
   sites are all in the classified bands of §1.5, with class 6 empty.
4. Close #178 with a citation to that audit — **not** to a bare-write count.

Until steps 1–3 are done, **#178 should stay open.** The issue's own framing
("the same defect class as #164, in a different call shape") is satisfied for
the write-contention class; the residual delivery window is a different defect
class that #178 happens to have surfaced.

---

## 10. Verification

| Gate | Result |
|---|---|
| `make test` | see the PR's CI run on the exact head |
| `make test-race` | see the PR's CI run on the exact head |
| `make test-scripts` | see the PR's CI run on the exact head |
| `make docs-check` | see the PR's CI run on the exact head |
| `mpm-lint --gate` | see the PR's CI run on the exact head |
| Release tests | see the PR's CI run on the exact head |

The full evidence, including the exact-head CI run, is on the PR.
