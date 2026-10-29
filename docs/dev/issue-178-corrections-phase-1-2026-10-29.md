# Issue #178 — reliability corrections, phase 1

**Date:** 2026-10-29
**Base:** `origin/main` at `a0c3521a` (post-#174)
**Audit branch (unchanged):** `audit/issue-178-bare-write-risk` @ `74eac183`
**Issue:** #178 — *"90 bare pool writes still lose their artifact to ordinary lock contention"*

This document records **phase 1** of the corrections: three independently
reviewable changes, the delivery-state design that phase 1 deliberately does
**not** implement, and an honest account of what remains exposed.

**Issue #178 is not closed by this work.** The issue tracks a wider inventory;
the exposure that survives phase 1 is enumerated below.

---

## 1. What phase 1 delivers

| # | Branch | Original commit | On `main` | What it corrects |
|---|---|---|---|---|
| PR 1 | `fix/issue-178-guard-scope` | `f98b655a` | `36b16c2a` | The write-budget guard could not see `cmd/` or `internal/scheduler/`. It now covers the whole runtime, and the four zero-budget writes it finds are repaired. |
| PR 2 | `fix/issue-178-scheduler-markfired` | `77bcf3a0` | `a51cce78` | The scheduler's wake settlement was a bare pool write. A lost settlement is not a lost artifact — it is a **duplicated side effect**. |
| PR 3 | `fix/issue-178-synthesis-cooldown` | `3528f568` | `0eed7477` | The synthesis's cooldown and ledger writes discarded their errors. A lost cooldown is not a lost artifact — it is a **duplicated conclusion**. |

The branches are **stacked** in dependency order (PR 1 → PR 2 → PR 3). PR 1 and
PR 2 both touch `internal/scheduler/drill_handler.go`, and PR 2 and PR 3 both
touch `internal/core/db.go`'s retry policy, so a stack keeps each PR
independently reviewable and independently revertible.

Each PR is independently correct: PR 2 does not depend on PR 1's behaviour and
PR 3 does not depend on PR 2's. The stack is a convenience, not a dependency.

### How phase 1 reached `main`

The four PRs were stacked on **each other** rather than on `main` — PR 1's base
was `main`, PR 2's base was PR 1's branch, PR 3's base was PR 2's branch, and
this record's base was PR 3's branch. GitHub merges a PR into its *base
branch*, so PR 2 landed on `fix/issue-178-guard-scope` and not on `main`.

Squash-merging a stack this way has one hazard that has to be handled
deliberately: once PR 1's **squash** is on `main`, PR 2's diff against `main`
no longer shows only PR 2 — it shows PR 2 *and* PR 1 again, because PR 1's
squash commit is a different object from PR 1's original commit. The
integration therefore **re-based each branch onto the current `main`** before
merging it, so that each squash contained only its own delta, and verified that
isolation by diffing each branch against `main` before every merge.

The final content is what it was before the re-basing: PR 2's and PR 3's trees
are byte-identical to the trees their original branch heads produced, and the
combined result is the same tree the original stack produced.

**One artefact of the process is worth naming.** PR 2's squash on `main`
(`a51cce78`) carries an auto-generated commit body that also quotes PR 1's
commit message, because GitHub generated the body from the whole branch
history and PR 1's *original* commit is not an ancestor of `main` — only PR 1's
*squash* is. The **content** of the commit is exactly PR 2's four files, and
its tree is byte-identical to the tree PR 2's original head produced; only the
prose of the body over-describes. GitHub's commit-message API refuses the
signed commit and the branch ruleset forbids a force-push, so the body could
not be corrected in place. PR 3's and this record's squashes name only their
own changes, because those branches were cherry-picked onto `main` rather
than merged.

---

## 2. PR 1 — the write-budget guard must see the whole runtime

### The defect

`internal/core/tx_write_budget_guard_test.go`, introduced by #176, walked
`internal/core` and nothing else. That was the substrate as it was understood
then; it is not the runtime.

Four zero-budget writes live outside the scope it covers:

| Site | Table | Why it is the #164 defect class |
|---|---|---|
| `cmd/mpm/store_working_context.go:127` `Save` | `ephemeral_scratchpad` | The **same table** #164's audit repaired on the tool side (`internal/core/tools/handlers.go`, budget 5). The comment read *"retries=0 → standard single attempt, no backoff loop"*, which is #164's misreading verbatim. |
| `cmd/mpm/store_working_context.go:150` `Delete` | `ephemeral_scratchpad` | Same. |
| `internal/scheduler/drill_handler.go:85` | `drill_runs` | Inside `WithTx`. The transaction owns the **stale-snapshot** class; it does not own the **ordinary busy** class, so these statements had no tolerance for it. |
| `internal/scheduler/drill_handler.go:127` | `drill_runs` | Same. |

All four now carry the documented budget of 5. **No budget is multiplied**: the
statement layer owns the ordinary busy class, the transaction layer owns the
snapshot class, and they are different classes.

### What the guard became

The scan is now a pure function of `(path, source)` plus the exemption table.
That buys three things the old shape could not have had:

1. **A negative control.** The guard carries its own synthetic control over
   the shapes that have hidden a zero-budget write: a literal zero, a zero
   behind an identifier, SQL in a variable, SQL built by concatenation, a write
   outside `internal/core` — and the shapes that must **not** be flagged: budget
   5, a read, a `", 0)"` inside a SQL literal, and the exempted statement.

   The control is synthetic on purpose. A mutation of the real tree proves the
   guard fires once, on one file, on one day. The synthetic control proves it
   fires on the **shape**, and keeps proving it as the tree changes.

2. **A resolved budget, not just a resolved SQL.** A site that passes its
   budget through an identifier is resolved to the integer that identifier
   holds, so `ExecTracked(q, noRetry)` is caught the same way `ExecTracked(q, 0)`
   is. The resolver checks integers **first**: a numeric literal's fingerprint
   is its own text, so a string-first order would record `const noRetry = 0` as
   the SQL `"0"` and lose the budget — the exact hole this resolution exists to
   close.

3. **An exemption that cannot rot.** Each exemption names the **outer owner**
   of the busy class and the reason a budget there would multiply the owner's
   budget. The guard verifies the owner still exists, still tests for a busy
   error, and still re-drives the operation in a loop; it verifies each fragment
   still matches **exactly one** live statement, so an exemption can never
   quietly cover a write nobody named.

   The one exemption in the tree (`internal/core/reference_db.go`, owned by
   `IngestReference`'s five-attempt ordinary-busy loop) passes all three checks.

A **reach test** was added: the scan must reach `cmd/mpm`, `internal/scheduler`
**and** `internal/core`. A silent regression to the old scope fails that test
rather than leaving the guard green.

### Non-vacuity, on the real tree

Setting `cmd/mpm/store_working_context.go`'s upsert back to `0` fails the guard
and names the site:

```
cmd/mpm/store_working_context.go:143 (budget 0)
```

### The scope boundary the PR respects

The guard does **not** require every write to have budget 5. Retry ownership
depends on transaction and operation semantics: a statement inside a
transaction that owns the same busy class carries no second budget, and the
recorded exemption exists precisely to keep that distinction explicit. The
guard's own exemption liveness check is what stops that distinction from
becoming a blanket.

---

## 3. PR 2 — the scheduler's wake settlement

### The defect

`QueryDueWakes` selects `WHERE fired = 0 AND target_time <= ?`. `Tick` runs the
wake's handler and then calls `MarkFired` to set `fired = 1`. `MarkFired` was a
bare pool write with no statement-level retry budget.

The consequence is not a lost artifact. It is a **duplicated side effect**:

- the settlement is the **only** record that a wake was delivered;
- an ordinary lock contention loses it;
- the wake stays `fired = 0`;
- the due-wake query selects it again on the next tick;
- and the handler **runs a second time**.

A broadcast fans out to other agents. A drill spawns a harness. A cron task
injects a wake. Those side effects cannot be un-done, and the scheduler's own
contract — *"failures surface but do not block other wakes"* — means the second
run happens in the normal course of the tick.

### The correction

- `core.MarkWakeFired` (new, `internal/core/wake_settlement.go`) performs the
  settlement with the substrate's statement-level budget of 5. It lives in the
  core module because `scheduled_wakes` is a core table and core already owns
  `ScheduleWake` / `ResolveWake` / `CheckPendingWakes`; the scheduler was
  duplicating that ownership against a raw pool.
- The pool-level retry policy is extracted into `core.execTracked`, and
  `DatabaseManager.ExecTracked` now delegates to it. There is **one**
  implementation of that policy at the pool level, so the settlement cannot
  drift from it. The transaction-level policy is deliberately unchanged and
  still lives in `txNode.ExecTracked`: a transaction can meet
  `SQLITE_BUSY_SNAPSHOT`, and that class belongs to the **transaction**
  (`WithTx` restarts it with a fresh snapshot), not to the statement. **No
  budget is multiplied.**
- `Scheduler.MarkFired` delegates to `core.MarkWakeFired`.
- `Tick` now **collects** settlement failures and returns an error naming them,
  and logs *"wake settlement failed — this wake will be EXECUTED AGAIN on the
  next tick"*. A settlement failure used to be logged and forgotten, which is
  how a repeating duplicate execution hides.

### Delivery semantics — stated honestly

This does **not** establish exactly-once delivery, and the code does not claim
it:

| Failure point | Semantics |
|---|---|
| Before execution | The wake stays pending and is retried on the next tick. At-least-once. |
| During execution (handler fails) | The wake is settled as **terminal**, with the error recorded in `last_error`. **At-most-once** for the failure. This is the pre-existing contract, preserved. |
| After a successful execution, during the settlement | The settlement waits out the contention (budget 5). If it still fails, the wake stays pending, the handler runs again on the next tick, and `Tick` **reports** it. |
| A crash between the handler completing and the settlement landing | The wake stays `fired = 0` and the next process runs the handler again. **At-least-once across a crash.** |

The last row is the residual phase 1 does **not** close, and it is pinned by a
characterisation test
(`TestWakeDelivery_ARestartBeforeSettlementLeavesTheWakePending`) so it cannot
be forgotten: if a future change closes that window, **that test fails**, and it
must fail deliberately.

What the table says, in one line: **at-least-once before execution,
at-most-once for a failed handler, at-least-once across a crash.** The
settlement's retry budget buys the *settlement*; it does not buy the
*delivery*. There is no exactly-once claim anywhere in the code or in this
document.

### The handler-idempotency audit

The duplicate-execution question is only as safe as the handlers it can
re-run. This is the audit:

| Wake kind | Handler | Idempotent? | Durable deduplication? | Consequence of a duplicate |
|---|---|---|---|---|
| `gc` | `GCHandler` | **Yes** | **Yes** — the GC claims its own `last_gc_at` slot with an atomic compare-and-swap (`cmd/mpm/handlers_gc.go:145`); the second run's claim fails and it skips | None. A dry run is read-only and claims nothing. |
| `snapshot` | `SnapshotHandler` | Nearly | No | A second backup file. Rotation-bounded, low impact. |
| `critic_audit` | `NewCriticAuditHandler` | **No** | No | A second critic run. **Not reachable automatically** — the P2b gate blocks the kind, so this needs a deliberate schedule. |
| `broadcast` | `BroadcastHandler` | **No** | No | **A duplicate fan-out to other agents.** Harmful. **Not reachable automatically** — see the reachability analysis below. |
| `drill` | `DrillHandler` | **No** | No | **A second harness run.** Expensive, and it produces a second `drill_runs` verdict. **Not reachable automatically** — see the reachability analysis below. |
| `cascade_summary` | `NewCascadeSummaryHandler` | **No** | No | Duplicate log lines. Low impact. |
| `cascade` | — | — | — | Not registered in `cmd/mpm-scheduler/main.go`; the core cascade materializer owns it. |

**Two of the seven are not idempotent and are harmful when duplicated**:
`broadcast` and `drill`. Both are reachable through a supported schedule. The
P2b gate covers `critic_audit`; nothing covers `broadcast` or `drill`.

That is the argument for the delivery-state redesign in §3.1: a retry budget
protects the *settlement*; it cannot protect a *broadcast* that has already
fanned out.

#### Are `broadcast` and `drill` reachable automatically?

The table above says what a duplicate *would* cost. It does not say whether the
duplicate can happen **without anyone asking for it** — and that distinction
matters for how the residual should be read: an operator-triggered duplicate is
a consequence of a decision someone made; an automatic duplicate is a defect
that fires on its own.

**The determination: neither kind can be created by any automatic producer.**
A `broadcast` or `drill` wake can only exist if somebody explicitly scheduled
one with that kind. The evidence is the producer inventory — every path that
writes a `scheduled_wakes` row authors a *different* kind:

| Producer | Kind it authors | Source |
|---|---|---|
| `ScheduleWake` — the default, when the caller supplies no `kind` | `notification` | `internal/core/wake_tools.go:291-292` (the producer-side default, so *every* caller gets it) |
| The cascade reconciler | `cascade` | `internal/core/cascade_wake_reconcile.go:89` |
| The cascade materializer | `cascade` | `internal/core/cascade_materializer.go:819` |
| The openclaw ingest path | `notification` (the default — it sets no `kind`) | `internal/scheduler/ingest.go:230-236` |
| The mpm-reconcile stale-foundation path | `notification` (the default — it sets no `kind`) | `internal/core/wake_tools.go:957-965` |
| The cron injector | `cron` | `internal/core/scheduled_tasks.go:241` |
| The cascade summary notifier | `cascade_summary` | `internal/core/wake_tools.go:107` |

Those are **all** the `ScheduleWake` call sites in the runtime, plus the two
direct `INSERT INTO scheduled_wakes` writers (the cron injector and the
cascade summary notifier). None of them authors `broadcast` or `drill`.

The **only** path that can author those kinds is the public scheduling tool
(`mpm_wake schedule`, `internal/core/tools/handlers.go:4975-5022`), and it
authors whatever `kind` the caller puts in `metadata`. So:

- **In the initial release configuration, `broadcast` and `drill` do not
  execute automatically.** They are inert until somebody schedules one
  explicitly.
- **They become reachable the moment one is explicitly scheduled** — and from
  that moment the crash window in §3 applies to them in full: a duplicate
  broadcast fans out twice; a duplicate drill runs a second harness and writes
  a second `drill_runs` verdict.
- The live installation at the time of writing holds **7 wakes and zero
  `broadcast`/`drill` wakes** (1 `cascade_summary`, 6 `cron`, all already
  fired), so the live configuration carries no exposure of this class today.

**The honest statement of the residual, then:** the crash window is *open*, not
merely *hard to reach*. What phase 1 buys is that the exposure is confined to
kinds an operator must deliberately schedule. It is **not** a claim that
delivery is exactly-once, and it is **not** a claim that a scheduled
`broadcast` or `drill` is protected — those are exactly the two kinds the
delivery-state redesign in §3.1 exists for.

There is also **no gate** that would keep a future automatic producer from
starting to author these kinds. The P2b gate covers `critic_audit` only
(`core.WakeKindBlocked`), so a new producer that wrote `kind=broadcast` would
be dispatched, and it would inherit the duplicate window with nothing to
catch it. That is a second reason the redesign is needed even though neither
kind fires today.

### 3.1 The design blocker — durable claim + lease

**Phase 1 stops here.** A correct fix for the crash window needs a
delivery-state redesign, and forcing an unsafe architectural change into this
release is exactly what the assignment forbids.

**The requirement.** The scheduler must be able to answer, durably and without
a race: *"has this wake been delivered?"* — and it must be able to answer
*"is anyone delivering it right now?"* so a crashed delivery can be recovered
without a duplicate.

**The proposal.**

1. **A durable claim, taken before the handler runs.** Add a
   `delivery_state` column to `scheduled_wakes` (`pending` → `claimed` →
   `delivered`), and claim the wake in a transaction that also records the
   claiming instance and a **lease expiry**. The claim is a conditional
   UPDATE (`WHERE id = ? AND delivery_state = 'pending'`) so two claimants
   cannot both win.
2. **A lease, so a crashed claim is recoverable.** The claim carries
   `lease_expires_at`. A claim whose lease has expired may be re-claimed. This
   is what makes the recovery path *bounded* rather than *permanent*: a crashed
   scheduler's wakes come back after the lease, not never.
3. **The settlement becomes the completion of the claim** (`claimed` →
   `delivered`), not the transition out of `pending`.
4. **Every handler becomes idempotent or carries its own deduplication.**
   `broadcast` and `drill` must gain a delivery key (the wake id) that their
   side effects record, so a re-delivery is a no-op. `gc` already has this;
   `snapshot` and `cascade_summary` need only cheap guards.

**What this buys.** Delivery becomes **at-least-once with idempotent effects**
— the standard, achievable semantics — instead of at-least-once with
un-guarded effects. The crash window closes: a crash after the handler
completes but before the settlement leaves the wake `claimed` with an expired
lease; the next process re-claims it and re-runs the handler, and because the
handler is idempotent, the re-run is a no-op.

**What it costs.** A schema migration on `scheduled_wakes`, a change to the
scheduler's tick loop, a lease-duration policy, and per-handler idempotency
work. It is a **release-sized** change, not a phase-1-sized one.

**Why phase 1 does not do it.** The assignment's constraint is explicit: *"If a
correct solution requires a broader delivery-state redesign, stop at a tested
reproduction and reviewable design recommendation. Do not force an unsafe
architectural change into this release."* Phase 1 delivers the tested
reproduction, the correction that closes the **contention** window (the
reachable one), the honest statement of the **crash** window, and this design.
It does not pretend the crash window is closed.

**One thing phase 1 does that a redesign must preserve.** The correction moves
the settlement into the core module and gives it a budget. A redesign must not
lose that: the settlement is still a durable write, and it still needs the
statement-level budget for ordinary contention.

---

## 4. PR 3 — the synthesis's cooldown and its ledger

### The defect

A synthesis merges near-miss memories into one. The merge has six durable
consequences:

1. the merged memory exists;
2. its `created_at` is the oldest source's;
3. the sources' topic memberships are transferred to it;
4. the sources are soft-deleted;
5. the `synth_runs` ledger records the content hash; and
6. the merged memory is stamped with `last_synthesized_at` — the **cooldown**.

Steps 5 and 6 are the **dedup state**: they are what makes *"this content has
already been synthesized"* a durable fact rather than a hope. They were written
with a bare pool call and their errors were **discarded**.

A failed cooldown write therefore produced exactly the state the cooldown
exists to prevent:

- the merged memory is live;
- it has **no** `last_synthesized_at`;
- the next ingest's `DetectNearMiss` does not filter it out, so it is merged a
  **second** time.

A second merge is not a second copy of a file. It is a **second artifact
asserting the same conclusion**, over evidence that has already been
soft-deleted: duplicated evidence, and confidence counted twice. The substrate
produces it itself, with no error and no log line anywhere.

### The correction

The six writes are one **fact**, so they are one **transaction**
(`commitSynthesisMerge`). Either the merge and its dedup state both land, or
neither does:

- a **crash** before the commit leaves nothing behind — the sources are still
  live, the merge is still pending, and the next ingest redoes it cleanly;
- an **ambiguous commit** leaves the artifact and its cooldown in the same fate
  — if the merged memory is there it is stamped, so it is not merged again; if
  it is not there, nothing happened.

Either way there is no duplicated evidence and no inflated confidence.

The previous implementation also treated the `created_at` preservation, the
topic transfer and the soft-deletes as **best-effort**: a failure there left
the merged memory live beside its own sources. Those are inside the
transaction too, so a partial merge is no longer reachable.

The three helpers (`recordSynthRun`, `bumpSynthDedupCounter`,
`markMemorySynthCooldown`) now **return** their errors, carry the substrate's
statement-level budget of 5, and every call site checks them.

### The finding this change had to make — and it is about SQLite, not about MPM

**The merge transaction's first statement must be a write.** This is not a
style preference; it is what makes the transaction survive contention.

**And it is a THIRD case, distinct from the one the substrate already knows.**
The substrate already records that a DEFERRED transaction that reads before it
writes can fail with `SQLITE_BUSY_SNAPSHOT` — the snapshot class, which
`WithTx` restarts with a fresh transaction. That is **not** the case measured
here. Measured on this substrate's connection settings (WAL,
`_busy_timeout=5000`), with a competitor holding the WAL write lock:

| Case | Shape | Elapsed | Extended code |
|---|---|---|---|
| A | pool write, no transaction | 5.005s | 5 — ordinary `SQLITE_BUSY` |
| B | transaction, **first statement a write** | 5.005s | 5 — ordinary `SQLITE_BUSY` |
| C | transaction, **read then write** | **0s** | **5 — ordinary `SQLITE_BUSY`** |
| D | transaction, first statement a *no-op* write | 5.005s | 5 — ordinary `SQLITE_BUSY` |
| E | transaction, read then write, competitor **committed** after our snapshot | 1ms | **517 — `SQLITE_BUSY_SNAPSHOT`** |

Case **E** is the substrate's known class: the competitor committed, our
snapshot went stale, and `WithTx` restarts the transaction. Case **C** is
different in two ways that matter:

- the competitor still holds the write lock **uncommitted**, so this is the
  **ordinary busy** class, not the snapshot class; and
- it comes back as extended code **5**, not 517, in **zero** milliseconds —
  the busy handler is not invoked at all, because the connection already holds
  a read lock and waiting could deadlock against the other connection.

So the substrate's model — *"ordinary `SQLITE_BUSY` is a wait and is clearable
by retrying the statement"* — is **true at the pool level (A) and true for a
write-first transaction (B, D), and false for a transaction that reads first
(C)**. And because case C is plain `SQLITE_BUSY`, `WithTx` does **not** restart
it either: it only restarts on `SQLITE_BUSY_SNAPSHOT`.

The consequence for the merge is the same as the consequence for any
transaction that reads before it writes: the statement-level retry cannot clear
it — re-running the statement in place cannot succeed, because the
transaction's read lock is what blocks the upgrade — and neither can the
transaction restart.

The old implementation was immune because every one of its writes went to the
**pool** (case A), where the busy handler is always invoked. A naive "wrap it
in a transaction" would have been a **regression** — the merge would have
become *less* resilient to contention than the code it replaced.

The merged memory's own creation reads first (the substrate's F19 identity
probe), so it **cannot** be the transaction's first statement. The
soft-delete of the sources leads instead: it is a write, it takes the write
lock at the transaction's first statement, and every statement after it runs
uncontended.

> **This is a substrate-level observation, not a synthesis-level one.** The
> same shape exists anywhere a transaction reads before it writes. It is
> recorded here because phase 1 found it; it is **not** corrected at the
> substrate level in phase 1 (see §5).
>
> **It is also a refinement of a lesson the substrate already holds.** The
> substrate already knows the *snapshot* sibling of this case (case E in the
> table above): a DEFERRED transaction that reads before it writes can fail
> with `SQLITE_BUSY_SNAPSHOT`, and the remedy is to abandon the transaction and
> re-take it, never to retry the statement in place. That case is handled
> (`WithTx` restarts on it, and `internal/core/tx_busy_snapshot_test.go`
> covers it). **Case C is the ordinary-busy sibling**, and it is *not* handled:
> it returns plain `SQLITE_BUSY` in zero milliseconds, so the statement-level
> budget cannot clear it and the transaction restart does not fire.

### Tests — 12, all non-vacuous

Every test runs against a **disposable, file-backed WAL database** built by
`NewDatabaseManager`, which uses the production connection settings. One test
**pins that claim** (journal mode, absolute path, busy timeout), so the file
cannot be quietly changed to something that behaves differently under
contention.

**The reproduction** injects the fault deterministically — a trigger that
refuses the cooldown UPDATE — and asserts that the merge fails, the merged
memory does not exist, the sources are still live, and the ledger knows nothing
about the content. A second test injects the fault at the soft-delete and
asserts the same: no partial state.

**Non-vacuity, against the previous implementation.** With
`commitSynthesisMerge` reverted to the pre-#178 shape (pool writes, discarded
errors), **four** tests fail:

```
--- FAIL: TestSynthesisMerge_AFailedCooldownWriteLeavesNoMergedMemory
        the merge must fail when the cooldown write fails; it returned "0d1eb86efda5f42c"
--- FAIL: TestSynthesisMerge_ARestartBeforeTheCommitLeavesNoPartialState
        the merge must fail when one of its writes fails
--- FAIL: TestSynthesisMerge_AFailedMergeIsObservable
        the merge must fail when the ledger write fails
--- FAIL: TestSynthesisMerge_ACancelledContextWritesNothing
        a cancelled merge must fail
```

The rest of the file covers the properties the correction must **preserve**:

- the cooldown suppresses an immediate re-merge — driven from the **detector's**
  side, not the writer's;
- the cooldown does **not** suppress legitimate future synthesis: with the
  cooldown disabled the merged memory is a candidate again, and the ledger's
  fast-skip is keyed on the content hash, not on the merged memory;
- the merge **survives ordinary contention** that outlasts the busy timeout
  and lands **complete** (artifact, cooldown, ledger, soft-deletes);
- the merge leaves exactly **one** live artifact asserting the conclusion, with
  the ordinary initial confidence — not doubled, not auto-trusted;
- a re-delivered merge does not produce a second artifact;
- a cancelled merge writes nothing.

One test proves the **causal link** rather than asserting it: a merged memory
**without** a cooldown stamp **is** a near-miss candidate, and the same memory
**with** the stamp is not. That is the mechanism by which a discarded cooldown
write becomes a duplicate synthesis.

---

## 5. What remains exposed

Measured with the same AST scanner the phase-0 audit used
(`internal/audit/barewritescan`, re-derived on both trees).

| Class | `origin/main` (`a0c3521a`) | After phase 1 | Delta |
|---|---|---|---|
| **Guarded** — `ExecTracked(query, N, …)` | 81 | 87 | +6 (the merge's node writes) |
| ↳ budget 0 | 8 | **4** | **−4** — the four sites PR 1 repaired. The remaining four are the **recorded** `reference_db.go` exemptions. |
| ↳ budget 5 | 53 | 57 | +4 (the same four sites) |
| ↳ budget not machine-readable | 0 | 6 | +6 — the merge's writes go through an interface, so the *audit* scanner cannot read their budget. The **PR 1 guard** resolves them through the identifier and does see them. |
| **Transaction-owned** — `tx.Exec` inside `WithTx` | 159 | 159 | unchanged |
| ↳ error not propagated | 5 | 5 | unchanged |
| **Bare pool** — `dm.db.Exec` / `dm.SQLDB().Exec` | 172 | **167** | **−5** (the synthesis's six pool writes removed, one pool write added back by the merge's structure) |
| ↳ error **not propagated** (silent loss) | **39** | **36** | **−3** — the three synthesis sites. |
| ↳ error propagated | 133 | 131 | −2 |

### The 36 silent-loss sites that remain

**Runtime (not DDL) — 20 sites.** These are the phase-2 work.

| Site | Table | Consequence of a busy failure |
|---|---|---|
| `internal/core/cascade_materializer.go:786` `revertToPending` | `epistemic_cascade_outbox` | **The intent stays `processing` forever** — a lost cascade obligation. (Audit F5.) |
| `internal/core/memory.go:3132` `BackfillSessionIDs` | `memories` | **The row is not updated but `updated++` still runs** — the function reports a false success. (Audit F8.) |
| `cmd/mpm/drill_cmds.go:200` `handleDrillsRun` | `drill_runs` | The drill's verdict is lost — the run stays `running` forever. |
| `internal/core/migrate_md.go:392`, `:427` | `raw_memories` | The staging row stays `pending` instead of `rejected` — retried on the next pass. |
| `internal/core/web_db.go:1274` `recordReferenceInteraction` | `reference_interactions` | The admission audit row is lost. |
| `internal/core/gc_tools.go:309` `RunGC` | `ephemeral_scratchpad` | The expired scratchpad prune does not happen. |
| `internal/core/memory.go:3187` `LoadSessionsFromDir` | `sessions` | The session row is lost; the count under-reports. |
| `internal/core/broadcast.go:243` `BroadcastMemory` | `shared` | The broadcast row is lost. |
| `internal/core/cascade_wake_reconcile.go:156` `deleteOrphanWake` | `scheduled_wakes` | The duplicate wake survives. |
| `cmd/mpm-mcp/audit_hook.go:99`, `cmd/mpm/audit_hook.go:127` | `tool_invocations` | **The dispatch audit row is lost** — the drill scorer reads this table, and `mpm_context recent_activity` is built from it. |
| `cmd/mpm/handlers_gc.go:275`, `:435`, `:458` | `memories` | The runtime accrual / soft-delete / zombie cleanup does not happen. |
| `cmd/mpm/handlers_epistemology.go:952`, `cmd/mpm/ops_init_topics.go:111` | `topic_memberships` | The backfill skips rows. |
| `cmd/mpm/ingest.go:309` | `raw_memories` | Old rejected rows are not deleted. |
| `cmd/mpm/recall.go:660` | `memories` | The implicit reinforcement of the recalled memory is lost. |
| `cmd/mpm/self_heal.go:247` | `lessons` | The self-heal audit lesson is lost. |

**DDL / startup — 15 sites** (`initUnifiedSchema`, `initFTSTables`,
`migrateLessonsToView`, `finishOrRepairLessonsView`, `migrateWorkEvents`, the
shared-DB FTS migration, `InitSQLite`'s FTS5 table creation). These are a
**different defect class** and are not swept into a retry PR: a multi-statement
DDL `Exec` may have applied part of itself, so a statement-level retry can
compound the damage. The right treatment is a **transaction**, not a budget.

**One test fixture** — `internal/core/testhelpers.go:234`
(`OpenLegacyMemoriesDB`).

### The 5 transaction-owned sites that remain

`cmd/mpm/handlers_gc.go:408`, `internal/core/db.go:3598` (migration DDL),
`internal/core/db.go:4754/4779/4787` (the FTS re-index after a metadata
update).

**One of these is the audit's F3:** `internal/core/scheduled_tasks.go:260` —
the *"pause the poisoned cron task"* write drops its error, so a task that
should have been paused keeps running and keeps re-injecting wakes. It is
counted among the transaction-owned sites and is **not** corrected in phase 1.

### Two exposures phase 1 did not touch, on purpose

1. **A transaction that reads before it writes turns ordinary contention into
   an immediate failure** (§4, case C). Phase 1 works around it *inside the
   merge*, by ordering the merge's writes. The substrate-level form — `WithTx`
   restarts a transaction on a stale snapshot but **not** on an ordinary busy
   error, so a transaction that read first has no remedy — is **not**
   corrected, because `WithTx` is used by everything and changing it is a
   substrate-wide semantic change, not a phase-1 correction. The finding is
   recorded here so the next assignment can size it.

   It is a **refinement** of a lesson the substrate already holds (the
   *snapshot* sibling, case E), not a contradiction of it. The new part is
   that the ordinary-busy sibling returns plain `SQLITE_BUSY` in zero
   milliseconds, so neither the statement-level budget nor the transaction
   restart clears it.

2. **The handoff write (audit F2)** — `internal/core/handoff.go` — can lose the
   session-continuity record to a five-second contention window. It is a
   single idempotent UPSERT and a one-line fix, but it was not in phase 1's
   scope, and it should be the first item of phase 2.

---

## 6. Release recommendation

### Must precede the next release candidate

**Nothing in phase 1 blocks the next release candidate.** Phase 1's three
changes are strictly reliability *improvements* with no documented behaviour
change, and each carries a deterministic reproduction, a passing regression,
and non-vacuity evidence against the previous implementation.

The one item that **should** land before the next RC is **PR 1** — not
because of what it repairs (four zero-budget writes), but because it is the
change that makes the rest of the work self-checking. Without it, the next
zero-budget write anywhere in the runtime passes review.

**If the next RC is a long-lived or production-facing release**, one more item
is worth pulling forward from phase 2, because it is a one-line fix with a
high blast radius: **`internal/core/handoff.go` (audit F2)**. The handoff is
the substrate's session-continuity artifact; losing it to a five-second
contention window loses the summary, the commitments and the open questions
for that session.

### What can wait

- **Phase 2 — the remaining silent-loss sites (§5).** 20 runtime sites and 15
  DDL sites. None of them is a duplicate-producing write, and 14 of the 20
  runtime sites are best-effort telemetry or backfill rows whose loss is
  recoverable by the next pass. The two that are not recoverable — the
  cascade outbox's stranded intent (F5) and the backfill's false success (F8)
  — are the right first two.
- **The delivery-state redesign (§3.1).** It is a schema migration and a
  per-handler change, and it must not be rushed into a release. But it should
  be **scheduled**, because until it lands, `broadcast` and `drill` wakes can
  be duplicated by a crash with nothing to catch it. The reachability
  analysis in §3 narrows *who* can be exposed — neither kind is produced
  automatically, so today the exposure is confined to kinds an operator must
  deliberately schedule — but it does not close the window, and no gate would
  stop a future automatic producer from starting to author those kinds.
- **The shared-database writes (audit Group 3)** — `arbitration.go`,
  `resolve.go`, `broadcast.go`'s shared inserts. These write to the **attached**
  database, which carries its own write lock independent of the local one.
  Operator-gated (`confirm = true`), so they are rare — which is exactly why
  they are post-release rather than release-blocking.
- **The long tail** — the CLI handlers, the GC passes, the retrieval-metadata
  writes, the works/projections, the `system_config` writes.
- **The substrate-level "transaction that reads before it writes" finding
  (§5).** It is real, it is now measured, and it is a substrate-sized change.

### The issue stays open

**#178 is not closed by phase 1.** It tracks a wider inventory, and the
exposure above is the honest remainder: 167 bare pool writes (36 of them
silent), 5 transaction-owned writes whose error is not propagated, and 15 DDL
sites that want a transaction rather than a retry budget.

---

## 7. Verification

Run on the combined tree (PR 1 → PR 2 → PR 3), against `origin/main` at
`a0c3521a`.

| Gate | Result |
|---|---|
| `make test` (root + `internal/core` + tools + testenv + scripts + bench + agent-installation) | **PASS.** Every Go package `ok`; `test-scripts` 682 tests OK; `test-bench` 217 tests; `test-agent-installation` 473 tests OK. |
| `make test-race` (the canonical CI gate) | **PASS** — exit 0, 139 packages `ok`, **0 failures**, **no `DATA RACE`**. |
| `make test-release` | **PASS** — 36 tests collected (floor 20), `ok`, exit 0. |
| `make release-gate` (`test-race` + `test-release`) | **PASS.** |
| `mpm-lint --gate` | **PASS** — all classes. `ctx-plain-missing` improved 820 → 817; `ctx-in-scope-missing` stays 0. |
| `make docs-check` | **PASS** — 13 checks, 3 review items (discharged below), **0 errors**. |
| CI (`build-and-test`, four PRs) | **PASS** — all four runs green. |

### 7.1 The CI runs

| PR | Branch | Run | Result |
|---|---|---|---|
| #179 | `fix/issue-178-guard-scope` | [`40896414707`](https://github.com/flowbyte-com/mpm/actions/runs/40896414707) | **success** (29m13s) |
| #180 | `fix/issue-178-scheduler-markfired` | [`40896454459`](https://github.com/flowbyte-com/mpm/actions/runs/40896454459) | **success** (20m18s) |
| #181 | `fix/issue-178-synthesis-cooldown` | [`40896491859`](https://github.com/flowbyte-com/mpm/actions/runs/40896491859) | **success** (28m59s) |
| #182 | `docs/issue-178-phase-1-record` | [`40896538473`](https://github.com/flowbyte-com/mpm/actions/runs/40896538473) | **success** (21m08s) |

**The original CI runs do not prove the integrated tree.** Each of those four
runs tested a *stacked branch*, not the tree that reaches `main`. The
integration therefore re-ran every gate on the integrated tree, and re-ran CI
on each integration PR; §7.5 records that.

The integration PRs — the ones that actually carried the content onto `main` —
were CI'd separately, because their content was carried onto `main` by a
different branch than the one the original PR used:

| Integration PR | Carries | Base |
|---|---|---|
| #183 | PR 2 — the wake settlement | `main` |
| #184 | PR 3 — the synthesis merge | `main` |
| #186 | this record | `main` |

Each was green (3/3) before it was merged.

### 7.2 One environmental failure, and why it is not one of ours

The first local `make test-race` run failed on
`cmd/mpm/probe` — `TestLive_MiniMaxAnthropicSurfaceAuthenticatesWithBearer`.
That test calls the **live MiniMax API** and is gated on `MINIMAX_API_KEY`
being present. On this host the key **is** present, so the test ran against the
real provider and the provider returned a thinking-only response.

It is **not** caused by these changes, and the proof is direct: the same test
fails on **pristine `origin/main`** (`a0c3521a`, extracted to a separate tree)
with a different failure mode — a 12-second transport timeout against
`api.minimax.io`. In CI the key is absent, so the test skips; all four CI runs
above are green.

The race gate was re-run with `MINIMAX_API_KEY` and `OPENAI_API_KEY` unset —
reproducing CI's environment exactly — and passed with **exit 0**.

#### The finding underneath it: the gate is not hermetic by default

The environmental failure is a symptom of a **test-hygiene defect**, and the
defect is worth naming precisely rather than working around by unsetting an
environment variable.

**The finding:** the gate's hermeticity is a property of *the operator's
environment*, not of the test. `TestLive_MiniMaxAnthropicSurfaceAuthenticatesWithBearer`
skips **only** when `MINIMAX_API_KEY` is absent; it runs — making a real,
authenticated, paid request to `api.minimax.io` — whenever the credential is
present. A host that has the credential exported therefore cannot run
`make test` or `make test-race` hermetically: the ordinary gates reach the live
provider.

Two things follow, and they are different:

- **The ordinary gates are not hermetic by default on a credentialed host.**
  That is the defect. "Routine tests should be hermetic by default, with live
  provider verification requiring explicit opt-in" is not currently true.
- **The live test is not the only path that can reach a provider**, but it is
  the only one that does so *unguarded*: an inventory of every test file that
  names a real provider host shows that all the others either point at a local
  `httptest` server or route the real hostname to loopback. The MiniMax test is
  the single exception.

The live test is also **not** hermetic in the ordinary sense of a test whose
outcome depends on the machine it runs on: the provider's response shape
decides whether it passes, so it can fail for reasons that have nothing to do
with the code under test — which is exactly what happened here.

**The correction is a separate, focused change and is NOT part of phase 1.**
The three corrections above are reliability changes to the substrate; a
test-hygiene change is a different concern and mixing them would blur both.
The correction is to gate the live verification on an **explicit opt-in** (a
deliberate environment variable) *in addition to* the credential, so that the
credential's presence alone is no longer sufficient to make an ordinary gate
reach the network — and to add a guard that keeps that true.

The gap is recorded in a dedicated issue so it is not lost. Until it is
closed, the operational rule is: **unset `MINIMAX_API_KEY` (and the other
provider credentials) when running the ordinary gates on a host where the
credential is present**, which is what CI does by construction.

### 7.3 Documentation impact assessment

`make docs-check` reports **0 errors** and **3 review items**. Each is
discharged here, and each commit carries the matching `Docs-Impact:` trailer.

| Review item | Changed | Assessment |
|---|---|---|
| *storage schema or confidence/evidence model changed* | `internal/core/db.go` | **No schema change.** The diff extracts the pool-level retry policy into `core.execTracked` and `DatabaseManager.ExecTracked` delegates to it — the same policy, the same budget, the same instrumentation, in the same package. No `CREATE TABLE`, no `ALTER`, no column, and the confidence/evidence model is untouched: the merged memory still gets the ordinary `InitialConfidence` for its collection (asserted by a test). **No documentation change is warranted.** |
| *scheduler or critic safety gates changed* | `internal/scheduler/drill_handler.go`, `internal/scheduler/scheduler.go`, `internal/scheduler/wake_settlement_test.go` | **No safety gate changed.** The P2b dispatch gate (`core.WakeKindBlocked`) is untouched and `internal/scheduler/scheduler_blocked_wake_test.go` still passes unchanged: a blocked wake is still settled `fired = 1` with the reason in `last_error`. What changed is the settlement's **retry budget** and the fact that a failed settlement is now **reported** rather than logged-and-forgotten. The scheduler's delivery semantics are documented in §3 of this record and in the code. **No documented behaviour changes.** |
| *CLI surface changed* | `cmd/mpm/store_working_context.go` | **No CLI surface changed.** The command, its flags, its output and its exit codes are identical. The diff changes the retry budget of two internal writes from `0` to `5` and the comment above them. **No documentation change is warranted.** |

The three commits each carry:

```
Docs-Impact: none — <reason>
```

which is the repo's convention (the trailer precedes the `Co-authored-by:`
line, as it does in #169 and #176).

**This document is the one documentation artefact phase 1 adds.** It is a
`docs/dev/` coordination record, which `docs/DOCUMENTATION-MAP.md` assigns to
its authors, so the documentation map does not need an entry for it.

### 7.4 What was NOT verified, and why

- **The live `~/.mpm` installation was not exercised.** The assignment
  forbids modifying it, and no scheduler or critic activity was triggered.
  Every test ran against a disposable, file-backed WAL database.
- **The `internal/core/tools` module and the `release_acceptance` module were
  run as part of the gates** (`test-tools`, `test-tools-race`, `test-release`)
  and pass unchanged; neither is touched by these three corrections.
- **No multi-process scheduler test.** The delivery-state analysis rests on
  the singleton invariant (flock on a PID file), which is asserted by
  `TestWakeDelivery_TheSchedulerIsStillASingleton`. A two-scheduler test would
  require two live schedulers against one database, which the assignment
  forbids.

### 7.5 The integration verification

The four PRs were reviewed independently and then integrated as **four separate
squashes onto `main`**, each carrying only its own delta. The isolation was
verified by diff, before every merge, and the resulting trees were compared
against the trees the original branch heads produced.

| Content | Original branch head's tree | Tree on `main` | Identical? |
|---|---|---|---|
| PR 1 — the guard scope | `f98b655a` | `90dfe23b` | yes |
| PR 2 — the wake settlement | `5d70e077` | `5d70e077` | **yes** |
| PR 3 — the synthesis merge | `fb72aff1` | `fb72aff1` | **yes** |

The three non-trivial integrations were:

- **PR 2.** Its branch's base was PR 1's branch, so GitHub merged it there
  rather than onto `main`. It was carried onto `main` by a dedicated
  integration PR whose diff against `main` was exactly PR 2's four files
  (`internal/core/db.go`, `internal/core/wake_settlement.go`,
  `internal/scheduler/scheduler.go`, `internal/scheduler/wake_settlement_test.go`).
- **PR 3.** It was re-based onto the then-current `main` and verified to carry
  only its own three files
  (`internal/core/synthesis_auto.go`, `internal/core/synthesis_auto_test.go`,
  `internal/core/synthesis_merge_transaction_test.go`) before being squashed.
- **This record.** Same technique, carrying one file.

**The residual-risk inventory was re-derived, not copied.** The phase-0 scanner
(`internal/audit/barewritescan`) was run against the pristine baseline
(`a0c3521a`) and against the combined phase-1 tree, and the numbers in §5
reproduce exactly:

| Class | Baseline `a0c3521a` | After phase 1 | Δ |
|---|---|---|---|
| Bare pool writes | 172 | **167** | −5 |
| ↳ error not propagated | 39 | **36** | −3 |
| Transaction-owned writes | 159 | 159 | unchanged |
| ↳ error not propagated | 5 | 5 | unchanged |
| `ExecTracked` writes | 81 | 87 | +6 |
| ↳ budget 0 | 8 | **4** | −4 |

The four remaining budget-0 writes are the recorded `reference_db.go`
exemptions, and each is inside `IngestReference`'s genuine five-attempt
ordinary-busy loop.

**The SQLite finding in §4 was re-measured independently**, on this substrate's
own connection settings (WAL, `_busy_timeout=5000`), with a competitor holding
the WAL write lock:

| Case | Elapsed | Extended code |
|---|---|---|
| Pool write, no transaction | 5.008s | 5 — ordinary `SQLITE_BUSY` |
| Transaction, first statement a **write** | 5.006s | 5 — ordinary `SQLITE_BUSY` |
| Transaction, **read then write** | **0s** | 5 — ordinary `SQLITE_BUSY` |

and, for the third case, five in-place retries of the same statement all
returned in 0s with the same error. So the statement-level budget cannot clear
that class, and `WithTx` does not restart it either (it restarts only on
`SQLITE_BUSY_SNAPSHOT`, extended code 517).

**The three error classes stay distinct.** `IsBusyError` is true for both 5 and
517; `IsBusySnapshotError` is true only for 517; and `ErrTxCommitAmbiguous` is
neither, so an ambiguous COMMIT is never re-driven.

**Each correction was verified by mutation, not by inspection.** The mutations
and the tests that caught them:

| Mutation | Test that failed |
|---|---|
| PR 1: a zero-budget write outside `internal/core` | `TestEveryRuntimeWriteHasAStatementLevelRetryBudget` |
| PR 1: the budget passed as an identifier holding 0 | the same |
| PR 1: the exemption's outer owner stops testing for a busy error | `TestWriteBudgetExemptionsAreNarrowJustifiedAndLive` |
| PR 1: the exemption's fragment broadened to match 3 statements | the same |
| PR 1: the budget resolver stops resolving | `TestWriteBudgetGuardFlagsRepresentativeUnsafeSites` |
| PR 2: the settlement reverts to the bare pool write | `TestWakeSettlement_ABareWriteLosesTheWakeAndTheGuardedOneSurvives` |
| PR 2: the settlement failure is swallowed again | `TestWakeDelivery_ASettlementFailureIsReportedAndLeavesTheWakePending` |
| PR 3: the cooldown write moves out of the transaction | `TestSynthesisMerge_AFailedCooldownWriteLeavesNoMergedMemory` |
| PR 3: the ledger write moves out of the transaction | `TestSynthesisMerge_ASuccessfulMergeCommitsTheArtifactAndItsDedupState` |
| PR 3: the merged memory's creation leads the transaction (a read before a write) | `TestSynthesisMerge_SurvivesOrdinaryLockContention` |
| PR 3: the cooldown stamp is dropped | `TestSynthesisMerge_TheCooldownSuppressesAnImmediateReMerge` |
| PR 3: the cancelled-context check is removed | `TestSynthesisMerge_ACancelledContextWritesNothing` |

The PR 3 read-then-write mutation is the one that reproduces the §4 finding
directly: it fails with *"the merge must survive ordinary contention: save the
synthesized memory: database is locked"*, in 3.18s rather than the 6.11s the
unmutated test takes — which is the zero-millisecond failure the finding
describes, arriving faster because the statement-level budget burns its
backoff and still cannot clear the class.

### 7.6 The gates on the integrated tree

Re-run on the integrated tree after all four squashes:

| Gate | Result |
|---|---|
| `make build` | **PASS** — exit 0. |
| `make release-gate` (`test-race` + `test-release`) | **PASS** — exit 0, 0 failures, no `DATA RACE`. |
| `make rehearsal` | **PASS** — 135 tests, 0 failures. |
| `make docs-check` | **PASS** — 13 checks, 0 errors. |
| `mpm-lint --gate` | **PASS** — all classes. |
| CI on the final `main` | **PASS** — 3/3 green. |
| Mutation + fault-injection regressions | **PASS** — every mutation above is caught by a named test. |

The race gate was run with the provider credentials unset, reproducing CI's
environment exactly, for the reason given in §7.2.

---

## 8. The durable record

| Where | What |
|---|---|
| `audit/issue-178-bare-write-risk` @ `74eac183` | The phase-0 audit: the inventory, the scanner, the reproductions R1–R4, and findings F1–F8. **Unchanged by phase 1.** |
| `docs/dev/bare-write-risk-assessment-2026-10-29.md` | The phase-0 audit document. |
| This document | The phase-1 corrections, the design blocker, the remaining exposure, and the release recommendation. |
| `internal/core/tx_write_budget_guard_test.go` | The extended guard — the standing proof that #164's audit stays discharged across the **whole** runtime. |
| `internal/core/wake_settlement.go` | The core-owned wake settlement. |
| `internal/scheduler/wake_settlement_test.go` | The settlement's reproduction, its regression, and the delivery-semantics table. |
| `internal/core/synthesis_merge_transaction_test.go` | The merge's reproduction, its regression, and the causal-link proof. |
