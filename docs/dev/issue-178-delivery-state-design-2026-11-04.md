# Issue #178 — the scheduler delivery-state redesign: an implementation-ready specification

**Date:** 2026-11-04 · **Revised:** 2026-11-05 (independent-review remediation)
**Baseline:** `main` @ `f8d4b95aefe60a0880409a8905ce2c4d92d22c00` (merge of PR #218)
**Authority:** [`docs/dev/issue-178-corrections-phase-3-2026-11-03.md`](issue-178-corrections-phase-3-2026-11-03.md) §7.0, and the
[revised engineering report](https://github.com/flowbyte-com/mpm/issues/178#issuecomment-6606813378) it links.

**Status of this document:** architecture and acceptance specification. No production migration is
implemented here. The upstream corrections document's §7.1–§7.6 proposal remains superseded and must
not be implemented as written.

**Revision note (2026-11-05).** This revision remediates the independent correctness review of this
document ([comment 6641707069](https://github.com/flowbyte-com/mpm/pull/219#issuecomment-6641707069),
verdict *CHANGES REQUESTED* on the specification, with *Stage A approved to proceed independently*).
The **architecture is unchanged**: the independent `delivery_state` axis, ★A (`fired = 0` in the
claim predicate), ★B (`claimed_by` as a fencing token), and the C8 discipline (no acceptance criterion
written as "exactly once") are all carried forward unchanged, as the review recommended. What changed
is that the **later-stage guarantees now agree with the actual scheduler implementation**.

Six blocking findings (B1–B6), ten recommended changes (R1–R10) and five optional refinements
(O1–O5) are disposed of in **[Appendix C](#appendix-c--the-disposition-of-the-independent-review)**.
The material changes are:

| # | Change |
|---|---|
| **B1** | the **claim statement** now carries `target_time <= ?`. The predicate carried it; the statement did not. |
| **B2** | the handler-failure path now uses the **fenced** settlement, so the axis can no longer be left in the state §3.5 says only a non-axis writer can produce. The **stranded** case (the dispatch gate refused; no handler ran) is classified and given its own transition. |
| **B3** | the settlement-failure policy is **decided and stated**: a settlement that errors releases the claim, best-effort, fenced by `claimed_by = ?`; the recovery latency is stated explicitly for both the normal and the worst case. |
| **B4** | the **snapshot idempotency mechanism** is specified completely, and assigned to a stage that actually implements it. A deterministic filename is necessary and **not sufficient**. |
| **B5** | the 15-minute and 30-minute constants are **withdrawn**. They had no evidence base, and their stated basis was wrong in three places. The lease and the deadlines are now **derived per handler** from the tree, with the measurement that is still missing named as a stage. |
| **B6** | §3.9 condition 1 no longer says a crash loses the wake "permanently". It does not — the wake is re-run. What is lost is the *knowledge* that it ran, and the audit trail. |

Additionally: §1.4's PR table is corrected (**#212 is not scheduler work**), §4 records that **stage C
is already partly implemented** in two open PRs, and §3.10's handler matrix is rewritten against the
tree — including the four handlers that **cannot run at all** today.

**Stage A is untouched.** Its boundary — five nullable `ADD COLUMN`s, one partial index, the
sentinel, the fatal-migration transaction, no backfill, base DDL unchanged, schema rollback
forbidden — is the independently approved one and is not reopened, redesigned or extended here.

---

## 0. What this document settles

§7.0 states eight corrections (C1–C8) that the superseded M1–M6 proposal violates, and delegates the
actual design to the revised engineering report. The report in turn leaves five decisions open and
two of its own instructions are unsafe as written. This document:

1. re-establishes current reality at `f8d4b95a`, including the corrections merged through #218;
2. answers each of §7.0's ten named conditions with evidence, not assertion;
3. **settles** four of the five open decisions (nullability, lease clock authority, the semantics of
   `claimed_by`, and the rollback policy) with a stated rationale and a named invariant, and makes
   the fifth — the **lease duration** — an explicit **open operator decision with a derivation
   procedure** rather than a number this document asserts (§3.3.6, §7.1 decision 3);
4. corrects four instructions in the revised report that cannot be implemented as written;
5. produces a stage sequence with acceptance gates, stop conditions and rollback conditions;
6. produces **fourteen** deterministic acceptance-test specifications, subdivided so that a safe
   claim, a lease expiry, a stale settlement, a crash recovery and a duplicate external effect each
   have their own discriminating assertion (§5).

**The two corrections that matter most** are marked **★** and are new relative to the report:

- **★A — The claim predicate must include `fired = 0`.** The report's §7.3.1 predicate does not, and
  that omission is the *actual* mechanism of the C7 rollback hazard. With `fired = 0` present, a wake
  settled by an older binary is structurally excluded from re-claim, and the mixed-version hazard
  closes without a special rollback policy. §3.4.
- **★B — `claimed_by` must be a fencing token, not observability.** The report recommends
  observability-only and leaves the column's semantics undefined. The proposed settlement guard
  (`AND delivery_state = 'claimed'`) does not fence a stale owner: it was proved, in isolation, that a
  stale owner can mark a row delivered while the current owner is still running its handler. The fix
  is one predicate term. §3.5.

---

## 1. Current reality at `main` @ `f8d4b95a`

### 1.1 The wake lifecycle, end to end

`scheduled_wakes` has four INSERT producers and seven UPDATE sites.

**Producers (INSERT):**

| # | Producer | file:line | `kind` | Transaction | Retry budget |
|---|---|---|---|---|---|
| P1 | `ScheduleCascadeSummaryWake` | `internal/core/wake_tools.go:119` | `cascade_summary` | **bare pool** (`dm.db.ExecContext`) | **none** |
| P2 | `ScheduleWake` | `internal/core/wake_tools.go:430` | default `notification` | bare pool via `ExecTracked` | 5 |
| P3 | cron injector | `internal/core/scheduled_tasks.go:247` | `cron` | raw `db.Begin()` + `tx.Exec` | **none** |
| P4 | capability rollback | `internal/core/capability/store_cascade.go:380` | untagged | `WithTx` + `ExecTracked` | 3 |

P1 is a known bare pool write: it is the sole `internal/core/wake_tools.go` entry in the bare-write
allowlist (`internal/core/bare_pool_write_ratchet_test.go:658-661`, reason `propagated`, count 1).

**Retirement writers (`fired = 1`), by semantic path:**

| # | Path | file:line | Guard | Handler ran? |
|---|---|---|---|---|
| W-a | scheduler settlement | `internal/core/wake_settlement.go:96,103` | **none** | **yes** |
| W-b | agent fold (`CheckPendingWakes`) | `internal/core/wake_tools.go:909` | `AND fired = 0` | no |
| W-c | explicit resolve | `internal/core/wake_tools.go:646` | `AND fired = 0` | no |
| W-d | 7-day notification sweep | `internal/scheduler/wake_expiration.go:182` | **none** | no |
| W-e | 1-hour cron sweep | `internal/scheduler/wake_expiration.go:501` | **none** | no |

**Four of the five never run a handler.** `fired` is a *retirement* latch, not a delivery record. No
production path ever writes `fired = 0`; the only escape is
`DELETE FROM scheduled_wakes` at `internal/core/cascade_wake_reconcile.go:157`.

**Dispatch:**

- `QueryDueWakes` (`internal/scheduler/scheduler.go:380`) selects `WHERE fired = 0 AND target_time <= ?`
  — **all** kinds, system and notification, with no kind filter.
- `Tick` (`internal/scheduler/scheduler.go:434`) partitions the result in Go by `hasHandler(kind)`
  (`internal/scheduler/scheduler.go:638`) and runs only the system partition, one goroutine per wake.
- `executeOne` (`internal/scheduler/scheduler.go:579`) returns an error **without running the handler**
  for a blocked kind (`critic_audit`) or an unregistered kind; `Tick` still calls `MarkFired` on that
  error, so the row reaches `fired = 1` with `last_error` although no handler body ran.
- The notification path has its own claim, `dispatchClaimNextAdHocWake`
  (`internal/scheduler/dispatch.go:79`), an atomic `UPDATE … RETURNING` that stamps `dispatched_at`
  and `metadata.dispatched_by`. It filters to notification kinds, so the two paths never contend on a
  row.

**Settlement:** `MarkWakeFired` (`internal/core/wake_settlement.go:84`) is a single `ExecTracked`
statement (budget 5) with **no** `fired = 0` guard, by deliberate design: the docstring at
`internal/core/wake_settlement.go:78-83` argues that refusing to settle an already-fired row would turn a
duplicate settlement into a lost one. Its only production caller is the scheduler
(`internal/scheduler/scheduler.go:429` ← `:511` and `:521`).

### 1.2 Four meanings that one word has been carrying

| Concept | Where it lives today | Is it recorded? |
|---|---|---|
| **Retirement** ("no longer pending") | `fired`, `fired_at` | yes — five writers, four semantics for `fired_at`: delivery time (fold), retirement time (resolver), handler-completion time (settlement), sweep time (both sweeps) |
| **Scheduler dispatch** ("the scheduler took it") | `dispatched_at`, `metadata.dispatched_by` | notification path only; **write-only in production** — no reader |
| **Notification delivery** ("the agent saw it") | `fired` + `dispatched_at` | conflated with retirement |
| **External effect completion** ("the thing actually happened") | nowhere | **not recorded at all** |

`fired_by` is written only as the literal `'wake-resolver'` by `ResolveWake` and has **no production
reader**; its migration comment (`internal/core/migration_wake_resolver_fired_by.go:7-12`) claims
`cascade-materializer` / `cascade-reconciler` values that no code writes. `metadata.last_error` is
**write-only** — nothing reads it. The only column that records "the scheduler's handler ran" does
not exist.

### 1.3 The current invariants (each is pinned by a test at `f8d4b95a`)

| # | Invariant | Pinned by |
|---|---|---|
| I1 | `fired` is one-way: nothing sets it back to 0 | source invariant; only a `DELETE` escapes |
| I2 | The scheduler executes a wake only if a handler is registered for its kind | `TestSchedulerDispatchesADrillWakeTheAgentCheckLeftPending` |
| I3 | A wake whose handler ran but whose settlement did not land stays `fired = 0` and is re-due | `TestWakeDelivery_ARestartBeforeSettlementLeavesTheWakePending` |
| I4 | A failed settlement is **reported** by `Tick`, never silent | `TestWakeDelivery_ASettlementFailureIsReportedAndLeavesTheWakePending` |
| I5 | A failed handler settles the wake **terminal** with `last_error` | `TestWakeDelivery_AFailedHandlerSettlesTheWakeAsTerminal` |
| I6 | Only one scheduler runs at a time (`flock`) | `TestWakeDelivery_TheSchedulerIsStillASingleton` |
| I7 | The agent's fold retires only agent-owned kinds | `internal/core/wake_ownership_test.go` |
| I8 | The agent may not arm or resolve a scheduler-owned kind | `internal/core/tools/wake_arming_authorisation_test.go`, `wake_resolve_ownership_test.go` |
| I9 | A concurrent resolution of the same wake produces **at most one** audit row | `internal/core/wake_resolve_contention_test.go` |
| I10 | `target_time <= now` is part of wake eligibility | `QueryDueWakes`, `dispatch.go` |

All ten were re-verified green on the baseline before this specification was written.

### 1.4 What the merged PRs already closed

These are **completed work**. This specification does not propose re-implementing them.

| PR | What it closed |
|---|---|
| **#118** | **the blocked-kind mechanism** — the producer-side gate (`ScheduleWake` refuses `critic_audit` at `internal/core/wake_tools.go:412-418`) and the dispatch-side gate (`executeOne` refuses it at `internal/scheduler/scheduler.go:590`). §3.10's `critic_audit` row depends on this, and it is the only reason that kind is unreachable rather than merely discouraged. |
| #212 | **not scheduler work.** This entry was wrong in the previous revision: #212 is *"test(update): the development-source fixture cleans up and cannot copy itself"* — the updater's fixture suite, touching only `internal/update/`. It closed nothing on the wake path. The wake-ownership isolation groundwork is #215 and #216, below. |
| #213 | cascade materialisation idempotency (the transition, the one-theory-per-intent invariant, the read-back assertions) |
| #214 | the bare-write ratchet now sees the `ExecContext` shape |
| **#215** | **the agent's fold can no longer retire the scheduler's wakes** — `CheckPendingWakes` filters to `AgentOwnedWakeKinds` = `{notification, cascade, cron}` in every branch, including `kinds:["*"]` |
| **#216** | **the agent can no longer arm a scheduler-owned kind** — `handleScheduleWake` refuses any kind for which `WakeKindAgentOwned` is false, at the agent-facing boundary; `ResolveWake` now uses the same shared predicate |
| #217 | the release-identity probe's SIGPIPE flake |
| **#218** | **the resolver survives concurrent resolution** — the read-then-write pair moved inside `dm.WithTx`, which owns the stale-snapshot class |

**#200–#205** are the issue-178 contention series (durable writes survive contention, the best-effort
tail survives, the audit ledger survives, the bare-write ratchet, startup DDL and backfills survive
contention, and the busy-class layering contract pinned at the `WithTx` boundary). They are the
substrate's **write-path** classes, not delivery-state work, but the axis inherits them: stage G's
retry gate is the layering contract from #205, and stage C's ratchet-baseline change is measured
against #214's ratchet.

The revised report's stage **B** (B1: `kinds:["*"]` must not retire system wakes; B2: `ScheduleWake`
validates the kind against an allowlist) is therefore **closed**. The report's stage B is merged.

**Two open PRs already implement parts of stage C.** See §4, stage C.

---

## 2. Confirmed crash windows

Every window below was confirmed by reading the code at the baseline and, where marked, reproduced in
an isolated experiment (Appendix A).

### W1 — Duplicate execution after a crash between the handler and the settlement

**Status: open, and pinned as a live characterisation test.**

`TestWakeDelivery_ARestartBeforeSettlementLeavesTheWakePending`
(`internal/scheduler/wake_settlement_test.go:375`) runs the handler, omits the settlement, and asserts
that the wake is still `fired = 0` and still returned by `QueryDueWakes`. Its own comment: *"this is
the residual duplicate-execution path, and this test exists to keep it visible"*.

Two distinct triggers produce it:

- **T1-a — process death.** The handler's external effect happened; the process died before the
  settlement. The row is still pending, so the next tick runs the handler again.
- **T1-b — settlement failure.** `Tick` reports the failure (I4) and the wake stays pending; the next
  tick runs the handler again. `TestWakeDelivery_ASettlementFailureIsReportedAndLeavesTheWakePending`
  pins this.

**T1-a is irreducible by any delivery column.** A settlement records that a handler ran; it cannot
make an external effect happen once (§7.0 C8). What a delivery column *can* do is (i) make the
duplicate **visible** as a distinct state, and (ii) bound it with a lease so a crashed claim is
recovered rather than lost. The **effect** duplication is bounded only by per-handler idempotency
(§3.9).

### W2 — Lost INSERT on the two producers with no retry budget

**Status: open, independent of the delivery axis.**

P1 (`ScheduleCascadeSummaryWake`, `internal/core/wake_tools.go:119`) is a bare `dm.db.ExecContext`
with no retry budget: a `SQLITE_BUSY` from a concurrent writer loses the cascade summary row with
**no** retry and **no** audit. P3 (the cron injector, `internal/core/scheduled_tasks.go:247`) is a raw
`tx.Exec` inside a `db.Begin()` transaction with no statement-level budget; a busy there aborts the
whole cron injection.

P1 is inside the cascade-summary drain, whose *own* wake is the audit trail for the drain — so a lost
P1 row is a lost audit line for a drain that may have succeeded. P3 is inside a transaction that also
performs the `next_run_at` rollover, so the transaction is atomic: either both the wake and the
rollover land, or neither. P3's risk is therefore *delay*, not loss — but only if the busy error is
retried, which it is not. The next tick re-reads the task, finds it still due, and injects again.
**P3's residual is a poisoned cron task that fails every tick**, not a lost wake.

P1's residual is a genuinely lost audit row. It is a one-line fix (route P1 through `ExecTracked`
with budget 5) and belongs in stage C.

### W3 — The retention sweeps' unguarded UPDATE overwrites a settlement

**Status: open, confirmed by experiment (Appendix A, E2).**

Both sweeps select candidates with `WHERE fired = 0` on the **pool, outside the transaction**, and
then update inside a **separate** transaction with **no `fired = 0` guard**
(`internal/scheduler/wake_expiration.go:182` and `:501`). The window between the candidate `SELECT`
and the sweep `UPDATE` admits a concurrent settlement.

Reproduced: a wake selected as a sweep candidate, settled by the scheduler (`fired = 1`,
`fired_at = T1`), and then updated by the sweep, which **overwrote `fired_at` with T2** and stamped
its audit note over the settlement's metadata. The settlement's `fired_at` is lost. The sweep's
read-back assertion (`fired != 1`) cannot detect this, because the row *is* `fired = 1`.

This corrupts the one piece of evidence a post-mortem has: when did the handler actually finish?

### W4 — One malformed `metadata` row aborts a whole dispatch or sweep statement

**Status: open, confirmed by experiment (Appendix A, E5).**

`json_extract` **raises** on malformed JSON. The fold's kind filter probes `json_valid` first
(`internal/core/wake_tools.go:348`, added by #215). Three other predicates in the same table do not:

- the notification claim (`internal/scheduler/dispatch.go:92-101`),
- the 7-day sweep (`internal/scheduler/wake_expiration.go:117-133`),
- the 1-hour cron sweep (`internal/scheduler/wake_expiration.go:451-462`).

Reproduced: with a single row whose `metadata` is `{not json`, the notification claim raises
`malformed JSON` and **the healthy notification wake behind it is never dispatched**; both sweeps
raise on the same row. A single corrupt row — one legacy row written by an old binary, one row
hand-edited by an operator — silently stops notification delivery and both retention sweeps.

This is independent of the delivery axis, cheap to fix, and high-value.

### W5 — Mixed-version operation and rollback

**Status: open by default; closed structurally by ★A.**

`mpm-update` never opens the database (verified: no `NewDatabaseManager` / `sql.Open` in
`cmd/mpm-update/`), so the schema change lands **lazily**, when the first new-binary process opens
the database (`NewDatabaseManager` → `initUnifiedSchema` → the fatal migration transaction at
`internal/core/db.go:3117` and `:3135`).

Consequences:

- `mpm-update --rollback` restores the **runtime** and deliberately leaves the database alone. An
  older binary's settlement writes `fired = 1` and does **not** write `delivery_state = 'delivered'`.
- `mpm-update --rollback --with-db-restore --force` restores a **pre-update database backup**. If
  that backup predates the migration, the columns vanish; the migration re-runs on the next open and
  the columns return, all `NULL`. Any row that had been claimed reverts to `fired = 0` and is
  re-dispatched — today's behaviour, not a regression.
- A row settled by an older binary has `fired = 1` and `delivery_state IS NULL`. Under the report's
  report's §7.3.1 predicate such a row would be re-claimable. **With `fired = 0` present in the predicate
  (★A) it is structurally excluded.**

### W6 — The stale-owner settlement gap

**Status: a gap in the *proposed* design, not in the current code. Confirmed by experiment
(Appendix A, E4).**

The revised report proposes that the settlement writes `delivery_state = 'delivered'` guarded by
`AND delivery_state = 'claimed'`. That guard checks the **state** but not the **owner**. Reproduced:
owner A's claim expires, owner B re-claims the row, and A's stale settlement then succeeds — marking
the row delivered while B's handler is still running against it. If B then dies before its own
settlement, the wake is terminal and B's effect is lost.

Under the current singleton this is unreachable in production: the scheduler holds an exclusive
`flock` for its whole lifetime (`cmd/mpm-scheduler/main.go:118` → `internal/scheduler/scheduler.go:865`)
and `MarkWakeFired` has exactly one production caller, the scheduler. But the delivery axis should not
silently depend on an invariant that lives in another process and can be misconfigured through
`--lock`, `MPM_DB_LOCK` and workspace resolution. The fix is one predicate term (§3.5).

### W7 — The stranded wake: retired without the handler ever running

**Status: open, and a classification problem rather than a data-loss problem.**

`executeOne` (`internal/scheduler/scheduler.go:579-598`) returns an error **without running the
handler** in two cases:

| case | line | reachable in production? |
|---|---|---|
| the kind is **blocked** (`critic_audit`) | `:590-592` | **yes** — the kind is registered (`internal/scheduler/system_handlers.go:83`), so `hasHandler` is true and the wake reaches `executeOne`; the dispatch gate then refuses it |
| **no handler is registered** for the kind | `:596-598` | **no** — `Tick` partitions by `hasHandler(kind)` first (`internal/scheduler/scheduler.go:479`), and no handler is ever unregistered, so the two lookups cannot disagree |

`Tick` treats the error like any other handler failure: it calls `MarkFired` with the refusal reason
(`internal/scheduler/scheduler.go:511`), so the row reaches `fired = 1` with `last_error` set — and
**no handler body ran**. The docstring at `:586-589` says this is deliberate: *"the caller marks the
wake fired with this reason in `last_error`, so the wake is recorded as processed-and-refused (it
does NOT retry forever) and the refusal is observable rather than silent."*

So the wake is **not lost** — it is *retired*, deliberately, and the refusal is recorded. What the
schema cannot currently express is the difference between:

- a wake **retired after its handler ran and failed** (the handler's effect may or may not have
  happened), and
- a wake **retired because the handler never ran** (there was no effect to have happened).

Both reach `fired = 1` + `last_error`, and both look identical to a reader. Under the delivery axis
this distinction becomes **representable**, because the axis records whether the scheduler's handler
*completed* — and the two cases get different transitions (§3.5). The stranded case is classified
there, and it is the one case in this series where the current schema genuinely loses information
about a procedure.

**The design consequence, stated plainly:** a `critic_audit` wake, if one exists, is retired without
the audit running — and that is the intended behaviour, not a defect. The delivery axis must not
turn it into a retry loop.

### 2.7 What is **not** a window

| Candidate | Reality at the baseline |
|---|---|
| **duplicate broadcast** | `BroadcastMemory` deduplicates its fan-out by content: `shared.bcast_event_wakes.wake_id` is the PRIMARY KEY and the insert is `INSERT OR IGNORE` (`internal/core/broadcast.go:122`, `:231`, `:244`, `:518`). A repeat affects 0 rows → `status = "deduped"`. Broadcast is **already idempotent** for this failure mode. (Separately: `BroadcastHandler` cannot succeed today — it passes `--target`, which the CLI does not accept, and the wake carries no memory id. §7.0 C8 / the report's §3(c).) |
| **duplicate cascade materialisation** | closed by #213: the transition is idempotent, the intent yields one theory, and the read-back assertions report a transition that did not land. |
| **duplicate cascade wake scheduling** | `scheduleCascadeWake` (`internal/core/cascade_materializer.go:918`) flips `wake_scheduled = 1` **after** `ScheduleWake` succeeds and reports a failure to the caller when the flip does not land; the reconcile pass (`internal/core/cascade_wake_reconcile.go`) recovers the lost wake and deletes the orphan. The residual is bounded by the per-check delivery cap (`MaxCascadeWakePerCheck = 3`). |
| **duplicate `gc`** | `gc` takes a 24-hour compare-and-swap on `system_config.last_gc_at` **before** its work. The CAS that a `gc` wake would actually execute is `internal/core/gc_tools.go:81-92` — **not** `cmd/mpm/handlers_gc.go:174-190`, which is the separate `mpm gc` CLI path. On a successful claim it is **at-most-once**; its residual risk is a **lost** GC, not a duplicated one. **Two caveats the previous revision omitted:** (i) the `gc_run` action's `dry_run` is **not** read-only — three hard-delete prunes run unconditionally on a successful claim (`internal/core/gc_tools.go:114-122`: `PruneAuditLog(30)`, `PruneHandoffs(90)`, `PruneCascadeOutbox(30)`), while the `DryRun` guards begin only at `:141`; (ii) the handler **cannot run at all** today (§3.10), so this row is about the *fixed* handler, not the current one. |
| **duplicate `snapshot`** | **not** idempotent, and the previous revision's wording was wrong. The label defaults to `time.Now().Unix()` (`internal/scheduler/scheduler.go:914`), which has **one-second granularity**. So a duplicate delivery **in the same second** writes the **same** file and **overwrites** it — `sqlite3 .backup` overwrites an existing destination — which gives the file a **new mtime** and therefore changes the rotation order; and a duplicate delivery **in a later second** writes a **different** file and advances `rotateSnapshots(dir, 7)` (`:938`), which can delete a snapshot the operator wanted. Both outcomes are bad, and the first is the one a filename-only fix does not prevent. |

---

## 3. The architecture, challenged and settled

### 3.1 The axis: what `delivery_state` may and may not mean

**Settled.** `delivery_state` is an **independent axis** from `fired`, scoped to the
**scheduler-dispatch axis only**.

- It is **not** a projection of `fired` and does not replace it. `fired` remains the retirement axis
  and its thirteen retirement readers keep reading it unchanged. (§7.0 C2.)
- It does **not** cover the notification path. The notification path already has its own claim
  (`dispatched_at`), its own audit trail and its own retirement semantics, and the fold depends on
  them. (§7.0 C6.) The two axes provably never contend on a row: the notification claim filters to
  notification kinds, and the scheduler's claim filters to kinds with a registered handler.
- It carries **only** scheduler-dispatch facts. It never asserts that an external effect happened —
  only that the scheduler's handler completed and the row was settled.

### 3.2 The schema — settled

```sql
ALTER TABLE scheduled_wakes ADD COLUMN delivery_state TEXT;      -- NULL | 'claimed' | 'delivered'
ALTER TABLE scheduled_wakes ADD COLUMN lease_expires_at INTEGER;
ALTER TABLE scheduled_wakes ADD COLUMN claimed_by TEXT;
ALTER TABLE scheduled_wakes ADD COLUMN claimed_at INTEGER;
ALTER TABLE scheduled_wakes ADD COLUMN delivered_at INTEGER;

CREATE INDEX IF NOT EXISTS idx_sw_delivery_claim
  ON scheduled_wakes(delivery_state, lease_expires_at)
  WHERE delivery_state IS NOT NULL;
```

Decisions, each with its reason:

| Decision | Value | Reason |
|---|---|---|
| **Nullability** | `delivery_state` is **NULLABLE** and starts `NULL` on every row | `NULL` = "not under scheduler-dispatch tracking". It is the honest value for every notification wake, every retired wake and every wake the new binary has not yet dispatched. `NOT NULL DEFAULT 'pending'` would fabricate a tracking claim for rows that were never tracked. (§7.0 C3.) |
| **Backfill** | **none** | No column on the row records "the handler ran": `fired_by` is written only by the resolver, `dispatched_at` only by the notification claim, and `metadata.last_error` is set to `NULL` on a *successful* settlement (`internal/core/wake_settlement.go:97-99`). A backfill would fabricate a delivery for the four writers that never ran a handler, and for `kind='cron'` rows, which are **never deliverable at all** (no handler is registered; the notification claim excludes them). (§7.0 C1, C3.) |
| **The `pending` state** | **dropped** | The state machine is `NULL → 'claimed' → 'delivered'`. `'pending'` is never written by anything, so it is a dead value in the enum; the eligibility question ("is it due?") is answered by `target_time`, not by a state. Removing it is behaviour-neutral. |
| **Independent of `fired`** | yes | `fired` stays the retirement axis and is untouched by this axis. A row retired by the fold, the resolver or a sweep keeps `delivery_state IS NULL` and `delivered_at IS NULL` — it was retired, never delivered. |
| **Scope** | scheduler-dispatch only | §7.0 C6; the report's operator decisions 2 and 10. |
| **Base DDL** | **not** changed in `internal/core/schema.go` | `dispatched_at` and `fired_by` are already migration-only columns; the base DDL at `internal/core/schema.go:1359-1370` does not carry them. Adding these five to the base DDL would create a **second** definition of the columns and a drift risk. A fresh database and a migrated database then have **literally identical** schemas, produced by one code path. |
| **Index** | one partial index, `(delivery_state, lease_expires_at) WHERE delivery_state IS NOT NULL` | The reclaim predicate is `delivery_state = 'claimed' AND lease_expires_at < ?`; the partial index makes it a range scan over a set that is **empty at migration time** (no backfill), so creating it is instant regardless of table size. |
| **Forward-only** | schema rollback is forbidden | Proven, not asserted: with the partial index present, `ALTER TABLE scheduled_wakes DROP COLUMN delivery_state` is **refused by SQLite** (`error in index idx_sw_delivery_claim after drop column: no such column: delivery_state`). The rule is enforced by the schema itself. Dropping the index first does permit the rewrite — which is why the index must not be dropped. |

### 3.3 The lease — re-derived from the tree

**The previous revision asserted `SystemHandlerDeadline = 900 s` and `DeliveryLeaseSeconds = 1800 s`.
Both constants are withdrawn.** They had no evidence base, and the sentence that justified them was
wrong in three separate ways. The review's objection (B5) is upheld in full.

#### 3.3.1 What the previous justification actually got wrong

The previous text said:

> *"Only one of the six system handlers has a bound today — the drill, at
> `core.DefaultDrillTimeout("claude_code") = 120 s` … The other five (`snapshot`, `critic_audit`,
> `gc`, `broadcast`, `cascade_summary`) run until the dispatch context is cancelled."*

| the claim | the tree at `f8d4b95a` |
|---|---|
| "only one of the six has a bound" | **`cascade_summary` has a bound by construction** — it writes one `slog.Info` line and returns `nil` (`internal/scheduler/cascade_summary.go:42-59`). It is not unbounded; it is bounded by the length of a log line. |
| "the other five run until the dispatch context is cancelled" | **two of them cannot run at all** (§3.3.3), and a **third** is blocked at both ends. |
| "the drill's bound is `DefaultDrillTimeout("claude_code") = 120 s`" | **the YAML wins and it is uncapped.** `drill.TimeoutSecs` (`internal/core/drill.go:19`) takes precedence over `DefaultDrillTimeout` (`internal/core/drill.go:56-63`), and `LoadDrill` validates only `id`, `framework` and `expect.tools_required` (`internal/core/drill_loader.go:20-39`) — `timeout_secs` is neither validated nor bounded. The 120 s figure is the *default*, not the bound. |

A 15-minute deadline therefore protected **nothing that can execute**, and a 30-minute lease was
justified by recovery latencies for handlers that never receive a wake.

#### 3.3.2 Which handlers can actually execute

This is the load-bearing question, and it is answered by reading the producers, not the handler
registry. **A registered handler is not a reachable handler.** Every production `ScheduleWake` call
site in the tree was enumerated:

| producer | `metadata.kind` | has a handler? |
|---|---|---|
| `ScheduleCascadeSummaryWake` (`internal/core/wake_tools.go:105-119`), called by the cascade drain (`internal/scheduler/cascade_drain.go:257`) | `cascade_summary` | **yes** |
| the cascade materializer (`internal/core/cascade_materializer.go:931-937`) | `cascade` | no — notification partition |
| the cascade reconciler (`internal/core/cascade_wake_reconcile.go:88-95`) | `cascade` | no — notification partition |
| the cron injector (`internal/core/scheduled_tasks.go:243`) | `cron` | no — notification partition |
| the reconcile pass (`internal/core/wake_tools.go:1200-1208`) and the OpenClaw ingest (`internal/scheduler/ingest.go:230-243`) | untagged → `Wake.Kind()` returns `"notification"` (`internal/scheduler/scheduler.go:138-143`) | no — notification partition |
| the agent-facing surface (`internal/core/tools/handlers.go:5109-5121`) | `notification` only — every scheduler-owned kind is refused | no — notification partition |

**No producer in this repository arms `snapshot`, `drill`, `gc`, `broadcast` or `critic_audit`.** The
agent-facing path cannot (§1.4's #216), and no other code path creates those kinds. Combined with
§3.3.3, the handler population that can execute is:

| Handler | Registered | Producer in this tree | Can it run today? | Its own runtime bound |
|---|---|---|---|---|
| **`cascade_summary`** | yes | **yes** | **yes** | **bounded by construction** — one `slog.Info` line; **no row**, no subprocess, no filesystem access |
| `snapshot` | yes | no | would, if a wake existed | **unbounded** — `sqlite3 .backup` and `PRAGMA integrity_check`, both ∝ database size; `rotateSnapshots` **never reads `ctx`** (`internal/scheduler/scheduler.go:1122-1160`) |
| `drill` | yes | no | would, if a wake existed (it needs `metadata.path`) | the YAML's `timeout_secs`, **uncapped**; the ctx covers only `dispatchDrill` (`internal/scheduler/drill_handler.go:113-123`) |
| `critic_audit` | yes | no | **no** — blocked at the producer (`internal/core/wake_tools.go:412-418`) and at the dispatch gate (`internal/scheduler/scheduler.go:590`) | the `mpm-critic` subprocess is unbounded |
| `gc` | yes | no | **no** — the invocation cannot succeed (§3.3.3) | n/a |
| `broadcast` | yes | no | **no** — the invocation cannot succeed (§3.3.3) | n/a |

#### 3.3.3 The two handlers that cannot run at all

These are not "slow"; they are **broken invocations**, and the previous revision described them as
if they executed.

**`gc` — `mpm call gc_run` is not a callable tool name.** `gc_run` is an **action** of the
`mpm_system` registry entry (`internal/core/tools/registry_list.go:527`, `:534`, `:540`), not a tool
of its own. `mpm call` resolves a name through `tools.ByName` (`cmd/mpm/call.go:139-146`) and fails
with *"unknown tool: gc_run"*; the only call-routed capability is `mpm_help`
(`internal/core/tools/call_routed.go:52-54`). So `GCHandler`
(`internal/scheduler/scheduler.go:1032-1051`) always gets exit code 1, and the wake is retired as
**failed**. **No GC has ever run from a wake.**

**`broadcast` — the CLI does not accept `--target`.** `BroadcastHandler`
(`internal/scheduler/scheduler.go:1057-1076`) passes `--target <x>`; the CLI's switch recognises
`--dry-run`, `--json`, `--help` and treats everything else as positional
(`cmd/mpm/ops_broadcast_cmds.go:49-72`), so `--target` becomes the **memory id**, and the real
target lands in the "unknown flag" branch (`cmd/mpm/ops_broadcast_cmds.go:113-129`) → exit 1. The
wake also carries no memory id. **No broadcast has ever run from a wake.**

Both facts are load-bearing for this specification:

1. Their **idempotency** is not a delivery-axis concern today — there is no effect to duplicate.
2. **Fixing either one creates an external effect that does not exist today.** For `gc` that effect
   includes three hard-delete prunes that run even under `dry_run`
   (`internal/core/gc_tools.go:114-122`). This is the same class of operator decision as the
   broadcast's (§7.1, decision 8) and it was **missing from the previous revision**.

#### 3.3.4 The measurement instrument that already exists

`executeOne` logs `duration_ms` on **all three** of its exit paths — success, handler error and
context cancellation (`internal/scheduler/scheduler.go:611-635`):

```
s.log.Info("wake completed",  "wake_id", w.ID, "kind", w.Kind(), "duration_ms", elapsed.Milliseconds())
s.log.Error("wake completed", "wake_id", w.ID, "kind", w.Kind(), "duration_ms", elapsed.Milliseconds(), "err", err)
s.log.Error("wake cancelled",  "wake_id", w.ID, "kind", w.Kind(), "duration_ms", elapsed.Milliseconds(), "err", ctx.Err())
```

That is the pre-flight measurement, and it is **already in production** — no new instrumentation is
needed to read it. Two things must be added before it can be *used* as the basis for a deadline:

- the per-kind **maximum** must be observable, which means the log must be attributable to a kind
  (it already carries `kind`) and gathered over a **stated window**;
- the **observation window must be stated in advance** and the **result recorded**, so the deadline
  is a number with a provenance rather than an assertion.

The previous revision's stage D had a "stop if any production handler is observed to exceed the
deadline" stop condition — a post-hoc trip-wire on a number that had no provenance. The measurement
is now a **precondition** of the deadline, not a stop condition after it.

#### 3.3.5 The uncancellable tail — the part that must be bounded before any lease can be derived

A lease must cover the handler's runtime **plus** the time from the handler's completion to the
settlement landing. The second term is not derivable today, because **two handlers have work after
their cancellable region that no context can interrupt**:

| handler | cancellable region | uncancellable tail |
|---|---|---|
| `drill` | `dispatchDrill(dispatchCtx, …)` (`internal/scheduler/drill_handler.go:120`) | the scoring (`core.Score`, `:126`) and the closing `UPDATE drill_runs` (`:135-149`) — both outside the ctx. `WithTx` takes **no context** (`internal/core/db.go:1163`: `func (dm *DatabaseManager) WithTx(fn func(DBNode) error) (err error)`), so that write cannot be cancelled. |
| `snapshot` | both `sqlite3` subprocesses (`exec.CommandContext`, `internal/scheduler/scheduler.go:926`, `:932`) | `rotateSnapshots(dir, 7)` (`:938` → `:1122-1160`) — it never reads `ctx`, so once started it runs to completion. Its cost is a directory read plus a sort plus at most *n* unlinks, where *n* is the number of files present. |

**Therefore the lease cannot be derived until the tail is bounded by construction.** Stage D gains
two concrete prerequisites (making the tail cancellable, and measuring the cancellable region), and
only then can the settlement margin be computed rather than guessed.

The margin's *contention* component **is** derivable: the production DSN carries
`_busy_timeout=5000` (`internal/core/db.go:114`, `:151`), so a settlement statement waits up to five
seconds per attempt, and stage G's retry bound (an asserted, counted number) multiplies it. The
margin's *tail* component is not derivable until the two handlers above are fixed.

#### 3.3.6 The policy this document now specifies

| Constant | Value | Meaning |
|---|---|---|
| `SystemHandlerDeadline` | **not a constant** — a **per-handler registry**, `map[kind]time.Duration` | the deadline applied at the dispatch site for that kind; a kind absent from the registry gets the registry's documented **default** |
| `SettlementMargin` | **derived**, not asserted | the time from the handler's completion to the settlement landing: the cancellable tail (which stage D makes bounded) plus the settlement's own busy window (`busy_timeout × attempts`) |
| `DeliveryLeaseSeconds` | **derived**, not asserted | `max(registered deadline) + SettlementMargin`, over the kinds that can actually receive a wake |

**The registry's initial contents, and the evidence for each:**

| kind | deadline | evidence |
|---|---|---|
| `cascade_summary` | **the registry default** | bounded by construction — one log line, no I/O, no subprocess, no row. A deadline here is a trip-wire, not a bound. |
| `drill` | **derived at dispatch time** from the drill's own `timeout_secs`, **plus** the tail margin | the handler knows its own bound; the scheduler must read it (`internal/scheduler/drill_handler.go:113-117`) rather than assume the 120 s default. The registry entry is a **cap**: the effective deadline is `min(registry value, drill's own timeout + tail)`. |
| `snapshot` | **unset — no deadline may be registered yet** | no static bound exists (the cost ∝ database size) and the handler has no producer, so there is nothing to measure. The value is set from the measurement of §3.3.4 **after** a producer exists. Until then, the handler is **unguarded** — and that is recorded as risk R2, not papered over. |
| `critic_audit`, `gc`, `broadcast` | **no deadline** | the kinds cannot run (§3.3.2, §3.3.3). A deadline on an unreachable kind is untestable noise. If the operator unblocks or fixes one, the deadline must be set before it is armed. |

**Two invariants, unit-tested so the registry cannot drift:**

```
DeliveryLeaseSeconds >= max(registered deadline) + SettlementMargin
SettlementMargin      >= the largest uncancellable tail among the kinds that can receive a wake
```

**The accepted consequences, stated plainly:**

- **The lease duration is an operator decision informed by a measurement, not a number in this
  document.** Until stage D's measurement is taken, `DeliveryLeaseSeconds` is **undefined**, and
  stage E cannot be implemented. This is the honest position: the previous revision stated a number
  with no derivation, and the review's objection was that the number was wrong.
- A crashed claim is recovered after **up to the lease**, and the lease is set by the operator from
  the observed maximum handler runtime. The system kinds are hourly or daily in intent, so a
  recovery latency in the same order as the handler's own runtime is immaterial for them.
- **A handler that runs longer than its deadline is now cancelled**, where before it ran to
  completion. This remains a behaviour change on the dispatch path and it remains the operator's
  decision (§7.1, decision 12) — but it now applies **only to the kinds that can run**.

#### 3.3.7 The lease's precondition: process death, not a hung handler

**The lease recovers a claim from a dead process. It does not recover a claim from a hung handler,
because a hung handler does not let the tick finish.**

`Tick` blocks on `wg.Wait()` (`internal/scheduler/scheduler.go:535`) and `Run` calls `Tick`
**synchronously** (`internal/scheduler/scheduler.go:778`, `:809`). The daemon's context is
`signal.NotifyContext(context.Background(), …)` (`cmd/mpm-scheduler/main.go:224`) — it has **no
deadline** and is cancelled only by a signal. So a handler that ignores cancellation pins the whole
scheduler: no tick completes, and no other wake is delivered.

This is why the per-handler deadline is a **prerequisite** of the lease and not a refinement of it.
The lease's precondition is stated as an invariant, not as an aspiration:

> **Lease invariant.** Every handler that can receive a wake observes `ctx.Done()` and returns
> within its registered deadline. A handler that does not violate the lease's purpose, because the
> lease only reclaims claims held by a process that is no longer running.

The previous revision did not state this precondition, and the review was right to flag the omission
(R2). The `executeOne` docstring at `internal/scheduler/scheduler.go:572-573` — *"runs a single
wake's handler under a goroutine, with context timeout so a hung handler cannot pin a tick
indefinitely"* — is **misleading**: there is no dispatch-site timeout today, and the comment
describes the mechanism stage D introduces. Stage D must correct that comment (O1).

#### 3.3.8 Clock authority — unchanged

The clock discipline is **not** in dispute and is carried forward verbatim.

**Clock authority: the DB clock, evaluated inside the same statement as the write and the comparison.**

```sql
-- write
lease_expires_at = CAST(strftime('%s','now') AS INTEGER) + ?   -- the interval is bound; the instant is the engine's
-- compare
lease_expires_at <  CAST(strftime('%s','now') AS INTEGER)
```

Reasons:

1. One authority for the whole axis — `lease_expires_at`, `claimed_at` and `delivered_at` are all
   read from the same clock.
2. The write and the comparison derive from the **same statement-level clock reading**, so there is no
   window in which the process's cached time and the engine's evaluation disagree.
3. It matches the substrate's own convention (`created_at DEFAULT`).

**Honest limitation.** SQLite's `now` **is** the host's system clock. The DB clock does **not**
eliminate NTP-step exposure — `time.Now()` has exactly the same exposure. What it eliminates is
*cross-reader* disagreement and gives the axis a single authority. Under the singleton, the lease is
the only place in this axis where a clock matters at all.

**The two clocks are never compared to each other.** `fired_at` stays on the **process clock**
(`time.Now().Unix()`, `internal/core/wake_settlement.go:92`) — it is written by five paths and read by
the sweeps and the fold, and changing its clock source is a behaviour change to paths outside this
redesign. The new trio (`lease_expires_at`, `claimed_at`, `delivered_at`) is on the **DB clock**. This
is recorded here so a future reader does not try to difference `fired_at` against `delivered_at`.

### 3.4 The claim — settled

**★A. The claim predicate MUST include `fired = 0`.**

The report's §7.3.1 predicate is:

```sql
WHERE id = ? AND delivery_state = 'pending'
  AND (lease_expires_at IS NULL OR lease_expires_at < ?)
```

It omits `fired = 0` **and** `target_time <= ?`. Both omissions are load-bearing:

- Omitting `fired = 0` is the **mechanism** of the C7 rollback hazard: an older binary's settlement
  sets `fired = 1` and not `delivery_state = 'delivered'`, so the row looks claimable. With
  `fired = 0` present the row is structurally excluded and the hazard closes **without any special
  rollback policy**.
- Omitting `target_time <= ?` makes every wake due, including one scheduled for next week. (§7.0 C5.)

**The corrected claim predicate:**

```sql
WHERE fired = 0
  AND target_time <= CAST(strftime('%s','now') AS INTEGER)
  AND (
       delivery_state IS NULL
    OR (delivery_state = 'claimed'
        AND lease_expires_at IS NOT NULL
        AND lease_expires_at < CAST(strftime('%s','now') AS INTEGER))
      )
```

- `delivery_state IS NULL` — the row has never been dispatched under the new regime. It is claimable
  **exactly once**; the claim moves it to `'claimed'`.
- `delivery_state = 'claimed'` with an expired lease — a crashed claim, reclaimable.
- `'delivered'` — excluded (and also excluded by `fired = 0`, since the settlement writes both
  atomically). Double protection.

**The claim statement** is a per-wake guarded `UPDATE`, executed by the scheduler only for wakes
whose kind has a registered handler:

```sql
UPDATE scheduled_wakes
   SET delivery_state   = 'claimed',
       claimed_by       = ?,
       claimed_at       = CAST(strftime('%s','now') AS INTEGER),
       lease_expires_at = CAST(strftime('%s','now') AS INTEGER) + ?
 WHERE id = ?
   AND fired = 0
   AND target_time <= CAST(strftime('%s','now') AS INTEGER)
   AND (
         delivery_state IS NULL
      OR (delivery_state = 'claimed'
          AND lease_expires_at IS NOT NULL
          AND lease_expires_at < CAST(strftime('%s','now') AS INTEGER))
        )
```

> **B1 — the `target_time <= ?` term was missing from this statement.** The previous revision's
> *predicate* (§3.4, above) carried it, and §3.7 asserted that "the claim re-checks eligibility on
> the DB clock", but the **statement** that actually claims the row did **not** contain the term —
> so a future-dated wake would have been claimable by any caller of the statement. The predicate and
> the statement must be the same expression; the term is now in both, and §3.7's claim is true.

`RowsAffected == 1` ⇒ this caller owns the wake. `RowsAffected == 0` ⇒ somebody else owns it, or it
was retired, **or it is not yet due**; the caller skips it.

**The eligibility term is a second line of defence, not the only one.** `QueryDueWakes` already
filters `WHERE fired = 0 AND target_time <= ?` (`internal/scheduler/scheduler.go:380-408`), so the
scheduler only ever *offers* due wakes. The statement's own term is what makes the guarantee
**structural** rather than procedural: a caller that bypasses the query — a future tool, a repair
path, a test — still cannot claim a wake before its `target_time`.

**Why per-wake and not "claim the next wake":** the registered-kind set lives in Go
(`internal/scheduler/system_handlers.go:51-60`, `SystemHandlerKinds()`), not in SQL. `Tick` already
partitions by `hasHandler(kind)` in Go (`internal/scheduler/scheduler.go:638`). A per-wake claim keeps
the kind registry in one place and keeps the SQL free of a hard-coded kind list that would drift from
the registry. The existing "claim the next" shape (`internal/scheduler/dispatch.go:79`) is retained
**for the notification axis, untouched**.

**One predicate, two axes — a correction to the report.** The report's stage C instructs that the
system path should extend `dispatchClaimNextAdHocWake` and "also write the existing `dispatched_at` +
`metadata.dispatched_by` so the notification audit trail survives". **Rejected.** `dispatched_at` is
documented as the **notification** dispatch trail
(`internal/core/migration_scheduled_wakes_dispatched_at.go:29-32`: *"Invariant: `dispatched_at != NULL`
records that the scheduler has processed the wake at its target_time"*, in the context of
notification wakes). Writing it from the system path would make one column mean two things, and it is
unnecessary: the two axes never contend on a row (§3.1). The correct instruction is **one shared
predicate builder** — so the two axes cannot drift on the eligibility rules — with **two
axis-specific claim statements**.

**The notification path is out of scope.** Its claim, its `dispatched_at` semantics, its
`kinds` filter and the fold that depends on them are all unchanged.

### 3.5 The settlement — three outcomes, one fenced token

**★B. `claimed_by` is a fencing token.**

The report recommends observability-only and leaves the column's semantics undefined ("`claimed_by`'s
semantics are undefined in §7.2 — that has to be settled before it is written"). Settled as follows:

- **Value:** a fresh opaque token per claim: `<instance-uuid>:<claim-uuid>`. The instance uuid is
  generated once per daemon start; the claim uuid is fresh per claim. The string is unique per claim,
  so it fences exactly one claim, and the instance prefix keeps it attributable in a log.
- **Written** by the claim statement.
- **Checked** by every transition that follows the claim.

#### 3.5.1 The settlement statement

```sql
UPDATE scheduled_wakes
   SET delivery_state = 'delivered',
       delivered_at   = CAST(strftime('%s','now') AS INTEGER),
       fired          = 1,
       fired_at       = ?,
       metadata       = json_set(WakeMetadataJSON, '$.last_error', ?)
 WHERE id = ?
   AND fired = 0
   AND delivery_state = 'claimed'
   AND claimed_by = ?
```

> **The metadata write must use the guarded expression.** `json_set` **raises** on malformed JSON, so
> the unguarded `COALESCE(metadata, '{}')` form cannot settle a wake whose `metadata` is corrupt:
> the settlement errors, the wake stays `fired = 0`, and its handler runs **again on the next tick —
> forever**. That is W4's class, and it is the same defect the existing settlement has today.
> **#227** fixes it by routing the write through `WakeMetadataJSON` —
> `CASE WHEN json_valid(metadata) THEN metadata ELSE '{}' END` — and the new transitions **must** use
> the same expression. `WakeMetadataJSON` is defined in `internal/core/wake_tools.go` by **#227**, so
> **stage F depends on stage C's #227 landing.**

- `AND fired = 0` — new relative to today's settlement, which has no guard. The current settlement's
  docstring argues against a guard ("refusing to settle because the row already says `fired = 1`
  would turn a duplicate settlement into a lost one"). That argument is about *idempotence*; the new
  settlement is not a duplicate settlement but a **fenced transition**, and its guard is the point.
- `AND delivery_state = 'claimed'` — the transition may only follow a claim. Without it a settlement
  could move a row from `NULL` straight to `'delivered'`, and the wake would never be re-delivered.
- `AND claimed_by = ?` — the fencing term. Proved necessary in Appendix A, E4.

**The settlement has no lease test, and that is deliberate.** The lease governs **reclaimability**;
the fencing token governs **settleability**. If the lease expired and another claimant took the
row, `claimed_by` has changed and the stale settlement is refused — the fencing term already
subsumes the lease test, because a reclaim necessarily rewrites `claimed_by`. If the lease expired
and *nobody* reclaimed, `claimed_by` is unchanged and the settlement is **accepted** — correctly,
because the work really was done. A lease test would *reject* that settlement and turn a completed
delivery into a redelivery. This is the opposite of the argument `MarkWakeFired` makes against a
`fired = 0` guard, and the difference is exactly the fencing token.

#### 3.5.2 The three outcomes of a dispatch — and the three transitions

The previous revision described **one** transition and asserted that "a wake whose settlement is
refused stays pending and will be re-run". That is wrong, and the review's objection (B2) is upheld.
A dispatch has **three** possible outcomes, and each needs its own transition.

| # | outcome | did the handler run? | the effect may have happened? | transition |
|---|---|---|---|---|
| **S** | the handler **completed** (returned nil, or returned an error, or was cancelled by its deadline) | **yes** | maybe | the **settlement** (§3.5.1): `delivery_state = 'delivered'`, `delivered_at`, `fired = 1`, `fired_at`, `last_error` = the handler's error (empty on success) |
| **R** | the handler **never ran** — the dispatch gate refused the kind | **no** | **no** | the **refusal**: the claim is **released** and the wake is **retired** (§3.5.3) |
| **E** | the **settlement statement itself errored** (a busy DB, a rolled-back write) | yes | maybe | the **release**: the claim is **released**, best-effort (§3.5.4) |

**The failure path must use the fenced settlement, not `MarkWakeFired`.** Today, `Tick` calls
`MarkFired` when `executeOne` returns an error (`internal/scheduler/scheduler.go:511`), and
`MarkWakeFired` writes `fired = 1` **without touching `delivery_state`**
(`internal/core/wake_settlement.go:84-110`). Under the axis that produces

```
fired = 1  AND  delivery_state = 'claimed'
```

— a state §3.5.7 says only a non-axis writer (an older binary during a rollback window) can produce.
That is a contradiction inside the design, and it is the defect the review identified. After stage F
the failure path calls the **fenced settlement** with the handler's error bound to `last_error`, and
the row lands in a single coherent state:

```
fired = 1  AND  delivery_state = 'delivered'  AND  delivered_at = <now>  AND  metadata.last_error = <err>
```

`delivered` means **"the scheduler's handler completed and the row was settled"** — which is exactly
what §3.1 says the axis records. It does **not** mean "the external effect happened". A failed
handler is a *completed* handler; the error is recorded in `last_error`, and the per-handler
idempotency analysis (§3.10) is what bounds the effect. **This does not change Stage A's schema**:
the three values `NULL | 'claimed' | 'delivered'` are unchanged, and `metadata.last_error` already
exists.

**The stranded case (R) is not a failure.** The dispatch gate refuses a blocked kind
(`internal/scheduler/scheduler.go:590-592`) and returns an error **without running any handler**
(§2, W7). Retiring that wake as `delivery_state = 'delivered'` would assert that a handler
completed when none ran — the exact falsehood the axis must not carry. Retiring it as
`fired = 1` with the claim still `'claimed'` would leave the axis in the contradictory state above.
So the stranded case gets **its own transition**: the claim is released and the wake is retired
without a delivery.

#### 3.5.3 The refusal transition (R)

```sql
UPDATE scheduled_wakes
   SET delivery_state   = NULL,
       lease_expires_at = NULL,
       fired            = 1,
       fired_at         = ?,
       metadata         = json_set(WakeMetadataJSON, '$.last_error', ?)
 WHERE id = ?
   AND fired = 0
   AND delivery_state = 'claimed'
   AND claimed_by = ?
```

The row ends where a non-axis retirement ends — `fired = 1`, `delivery_state IS NULL`,
`delivered_at IS NULL` — and it says *why* in `last_error`. **`claimed_by` and `claimed_at` are
deliberately left in place**: they are the forensic record that a claim was taken and then released
without a delivery, which is precisely the distinction W7 says the current schema cannot express.
`lease_expires_at` is voided, because there is no live lease any more.

**A refusal is terminal and is not a retry loop.** That is the existing behaviour's intent
(`internal/scheduler/scheduler.go:586-589`) and it is preserved: a blocked kind is retired once, and
the axis records that the procedure did not run.

#### 3.5.4 The release transition (E) and the recovery latency

A settlement that **errors** is different from a settlement that is **refused**. A refusal means the
row is no longer ours (§3.5.5). An error means the write did not happen.

**Policy (B3, decided).** A settlement that errors **releases the claim**, so the wake is immediately
eligible again, exactly as today:

```sql
UPDATE scheduled_wakes
   SET delivery_state   = NULL,
       lease_expires_at = NULL
 WHERE id = ?
   AND delivery_state = 'claimed'
   AND claimed_by = ?
```

The release is **best-effort** and **fenced by `claimed_by = ?`**: it can only release *this*
claim. If the lease expired in the meantime and another claimant took the row, `claimed_by` has
changed and the release affects **0 rows** — it can never free a claim that is no longer ours.

`claimed_by` and `claimed_at` are **not** cleared by the release: the next claim overwrites them,
and until it does they record that this wake was claimed by this token and had its settlement
fail. The error itself is **reported** by `Tick` (§3.5.4's last paragraph) rather than written onto
the row — `metadata.last_error` is reserved for the *handler's* error, and a settlement error is a
different fact.

**The recovery latency, stated explicitly** (this is what the previous revision left silent):

| case | the wake is eligible again | latency |
|---|---|---|
| the settlement errors and the **release succeeds** | immediately, at the next tick | **one tick interval** — the daemon's default `--interval` is **60 s** (`cmd/mpm-scheduler/main.go:48`) |
| the settlement errors and the **release also fails** (the same busy writer) | only when the lease expires | **up to `DeliveryLeaseSeconds`** — which is why the lease must not be an arbitrary number (§3.3.6) |

**The normal case is unchanged from today.** Today a failed settlement leaves the wake `fired = 0`
and `Tick` reports it ("this wake will be EXECUTED AGAIN on the next tick",
`internal/scheduler/scheduler.go:512-513`); the wake is re-run on the next tick. Under the axis, the
release reproduces exactly that, and the worst case is the lease. The alternative — accepting the
lease as the only recovery path — would have been a **60 s → 30 min regression** in the common case,
and the review was right to call that out.

**The failure is still reported.** `Tick` reports it today (invariant I4) and it continues to: the
release is a *repair* of the state, not a substitute for the report.

#### 3.5.5 What a refused settlement actually means

The previous revision said: *"A wake whose settlement is refused stays pending and will be re-run;
that is the correct at-least-once behaviour, and it must be visible."* **This is wrong for the
fenced settlement.** A refused settlement (`RowsAffected == 0`) means the row no longer satisfies
the guard — so **either**:

- the row was retired by another path (`fired = 1`) — it is **terminal**, not pending; or
- another claimant owns it (`claimed_by` differs) — it is **theirs**, not pending for us; or
- the row was already settled by this same claim — idempotent, already terminal.

In **none** of those cases is the wake "pending and re-runnable" by us. The correct statement is:

> A refused settlement changes nothing and is **reported**. The wake is **not** re-run by this
> claimant; it is either already terminal or owned by another claimant, and the axis records which.

**A refused settlement is still visible.** `Tick` reports it the same way it reports a failed
settlement today, and the report distinguishes the two cases — a *refusal* (0 rows) from an *error*
(the statement failed) — because they have different consequences.

#### 3.5.6 `MarkWakeFired` is not modified — but the new binary stops calling it

The existing exported function (`internal/core/wake_settlement.go:84`) keeps its contract — it
settles any row and does not touch `delivery_state`. **New** functions carry the three transitions
above, and only the scheduler's dispatch path calls them. This keeps the mixed-version path working:
an older binary's settlement still lands `fired = 1` (and leaves `delivery_state` untouched), which
is exactly what ★A's `fired = 0` guard is designed to absorb.

After stage F, `MarkWakeFired` has **no caller in the new binary's scheduler path** — its only
production caller today is the scheduler (`internal/scheduler/scheduler.go:429` ← `:511` and `:521`).
It remains exported and unchanged, so the older binary and the existing tests are unaffected, and the
transition it does not make — `delivery_state = 'delivered'` — is the one the axis needs.

#### 3.5.7 Rows retired by another path keep `delivery_state` untouched

A fold-retired row, a resolver-retired row and a sweep-retired row all have `fired = 1`,
`delivery_state IS NULL` and `delivered_at IS NULL`. They were **retired, not delivered**.

**A reader must never infer "still pending" from the axis.** After stage F the only rows with
`delivery_state = 'claimed'` are (a) rows claimed by a live claimant, and (b) rows whose settlement
errored **and** whose release also failed — which the lease recovers. A row with
`delivery_state = 'claimed'` and `fired = 1` is therefore no longer producible by the new binary; if
one is observed, it was written by an **older binary during a rollback window**, and the axis's own
invariant — "no new binary writes `fired = 1` without also settling or releasing" — is the
detector.

### 3.6 Clock consistency — settled

Summary of the whole clock story, in one place:

| Field | Clock | Written by | Compared with |
|---|---|---|---|
| `target_time` | process (`time.Now()`) | the producer | **two** comparisons: the process clock in `QueryDueWakes` (`target_time <= ?`, **unchanged**) and the **DB** clock in the claim statement (`target_time <= CAST(strftime('%s','now') AS INTEGER)`) |
| `fired_at` | process | five retirement paths | never compared to a DB-clock field |
| `created_at` | DB (`DEFAULT`) | SQLite | not compared |
| `claimed_at` | **DB** | the claim | not compared |
| `lease_expires_at` | **DB** | the claim | the **DB** clock, in the same statement |
| `delivered_at` | **DB** | the settlement | not compared |

The scheduler's claim statement uses the **DB clock for both** the eligibility check
(`target_time <= CAST(strftime('%s','now') AS INTEGER)`) and the lease check, so the statement has
one clock and one authority. The notification path keeps the process clock — unchanged, because
`target_time` is authored on the process clock and the two producers live on the same host.

**The previous revision's table said `target_time` was compared with "the process clock —
unchanged".** That was wrong, and it was wrong in the direction that mattered: the claim statement
now compares `target_time` against the **DB** clock, so `target_time` is the one field that is
**read on two different clocks**. The row above records that explicitly rather than implying a
single comparison. The two clocks are the same host clock, so the difference is a sub-second
evaluation-order difference, not a semantic one — but a reader diffing `target_time` against
`claimed_at` must know which one they are looking at.

### 3.7 Future-dated eligibility — settled

`target_time <= ?` **survives** (§7.0 C5), and it is now in the **claim statement** as well as in
the claim predicate (§3.4) — the previous revision stated the predicate but omitted the term from
the statement that actually claims the row (B1).

`QueryDueWakes` is unchanged and continues to use the process clock for its own
`target_time <= ?` filter; the claim re-checks eligibility on the DB clock, so a wake that becomes
due between the two is claimed on the next tick. **No wake is ever claimed before its
`target_time`**, by any caller of the statement.

### 3.8 Migration lifecycle and rollback policy — settled

**Mechanics.** A new file `internal/core/migration_scheduled_wakes_delivery_state.go`, following
`internal/core/migration_scheduled_wakes_dispatched_at.go` exactly:

- a `schema_migrations` sentinel, `scheduled_wakes_delivery_state_v1`;
- `columnExists` probes (`internal/core/migration_timestamps.go:81`) so a partially-migrated database
  converges;
- the five `ALTER TABLE … ADD COLUMN` statements and the one `CREATE INDEX IF NOT EXISTS`;
- the sentinel written with `INSERT OR IGNORE`.

**Where it runs.** Inside the **fatal** migration transaction, next to `MigrateWakeResolverFiredBy`
and `MigrateScheduledWakesDispatchedAt` (`internal/core/db.go:3117` and `:3135`), so a failure rolls the
whole migration back and `NewDatabaseManager` returns an error. It is **not** added to
`SafeMigrations`: that loop is best-effort and **silences errors**
(`internal/core/db.go:2967-2975` only warns), and `dispatched_at` is deliberately not in it.

**When it lands.** Lazily, when the first new-binary process opens the database. `mpm-update` never
opens it. The activation note for the operator: run one `mpm` invocation before starting the new
scheduler if the schema is wanted in place first.

**Rollback policy — binary and schema, separately:**

| | Policy | Reason |
|---|---|---|
| **Binary rollback** | **Allowed and safe** | the columns are additive and NULL-defaulted; an older binary ignores them. An older binary's settlement writes `fired = 1` and not `delivery_state = 'delivered'`; ★A's `fired = 0` guard excludes that row from any future claim, so **the rollback window introduces no new duplicate-execution window**. |
| **Binary rollback while rows are `claimed`** | **Allowed**, and an operator-visible event | an older binary ignores `delivery_state` and re-dispatches those rows — today's behaviour. The runbook must say so. |
| **Schema rollback** | **Forbidden** | `ALTER TABLE … DROP COLUMN` is refused by SQLite while `idx_sw_delivery_claim` exists, and proceeding by dropping the index first performs the table rewrite that the rule forbids. The schema is **forward-only**. |
| **`--with-db-restore`** | **Allowed**, with a runbook note | the restored backup may predate the migration; the columns vanish and the migration re-adds them on the next open, all `NULL`. Any row that had been claimed reverts to `fired = 0` and is re-dispatched — today's behaviour. |

**The operator runbook — the three entries the axis needs.**

The previous revision said only *"the runbook entry is still owed"*. Three entries are owed, and
the review named two of them (R8, R9). They are specified here so that the runbook is a checklist
rather than a judgement call.

**(1) The do-not-touch list.** `idx_sw_delivery_claim` is on it, and the reason is specific:

> **`idx_sw_delivery_claim` must not be dropped, and no "unused index" cleanup may remove it.**
> The index's predicate is `delivery_state IS NOT NULL`, and in stage A every row is `NULL`, so the
> index matches **zero rows** and reads as dead. That is exactly what a tidy-up would propose to
> remove — and removing it is the **only** way to re-enable the schema rollback the migration
> forbids, because `ALTER TABLE … DROP COLUMN` is refused by SQLite while the index exists
> ("error in index `idx_sw_delivery_claim` after drop column: no such column: `delivery_state`").
> Dropping the index first permits the table rewrite the rule forbids. **The index is the
> enforcement mechanism for the forward-only rule, not an optimisation.**

The same applies to any index a later stage adds (stage H's `drill_runs.wake_id` unique index,
stage I-a's `snapshot_deliveries` primary key): **each one is load-bearing for a schema rule**, and
each must be on the list with its rule named.

**(2) The pre-rollback count.** Before any binary rollback that could run while the axis is live,
the operator records:

```sql
SELECT COUNT(*) FROM scheduled_wakes WHERE delivery_state = 'claimed';
```

That count is the number of rows an older binary will **re-dispatch** on its next tick, because an
older binary ignores the axis entirely. The count is the operator's measure of the rollback's
exposure, and it is recorded **before** the rollback, not discovered afterwards. If the count is
non-zero, the rollback is an **operator-visible event** and the runbook says so explicitly
(decision 9's second row).

**(3) The schema-rollback prohibition.** `ALTER TABLE scheduled_wakes DROP COLUMN …` is
**forbidden**, and the enforcement is SQLite's, not a convention. The only sanctioned "rollback" is
`mpm-update --rollback --with-db-restore`, which restores a **backup** — and which therefore
reverts the schema as a side effect (R5). The runbook must state the consequence: the restored
backup may predate the migration, the columns return all `NULL` on the next open, and every row
that had been claimed is re-dispatched — today's behaviour, and a duplicate-execution window the
operator has accepted by choosing the restore.

**(4) The `delivery_state = 'claimed'` + `fired = 1` detector.** After stage F, a row with
`fired = 1` and `delivery_state = 'claimed'` is **not producible by the new binary**. If one is
observed, it was written by an **older binary during a rollback window** — which is expected and
benign — or by a defect. The runbook's diagnostic query is:

```sql
SELECT id, kind, claimed_by, claimed_at, fired_at
  FROM scheduled_wakes
 WHERE fired = 1 AND delivery_state = 'claimed';
```

Any row it returns is a row the axis could not account for, and it is the standing detector for
B2 (AT14).

### 3.9 The ten conditions, answered

| # | Condition | Answer |
|---|---|---|
| 1 | **Crash after claim, before execution** | The row is `delivery_state = 'claimed'` with a lease. Under the corrected predicate it is **reclaimable** once the lease expires (§3.4). **The previous revision's answer here was wrong** (B6): it said the wake is "**permanently lost**" today. It is **not**. Today the wake is `fired = 0`, so `QueryDueWakes` still returns it and the **next tick runs the handler again** — that is precisely what the characterisation test `TestWakeDelivery_ARestartBeforeSettlementLeavesTheWakePending` asserts (`internal/scheduler/wake_settlement_test.go:375-397`: *"without a settlement the wake must still be pending"*), and what invariant **I3** says. What is lost today is not the wake but (a) the **knowledge** that the handler already ran — no state distinguishes "never ran" from "ran and died" — and (b) the **audit trail** of the first run. So the honest baseline is *silent duplicate execution*, not *loss*. The lease does **not** prevent the re-run either (W1 remains); what it adds is that the re-run is **visible** (a fresh `claimed_by`) and **bounded** (it happens after the lease, not on the next tick). **Achievable — as visibility and bounding, not as prevention.** |
| 2 | **Crash during handler execution** | Indistinguishable from (1) at the row level — the row is `claimed` with a live lease. Recovery is the same: reclaim after the lease. The external effect may or may not have happened; only per-handler idempotency can bound that. **Partly achievable** (recovery), **not** achievable (effect-exactly-once). |
| 3 | **Crash after external effect, before settlement** | The row is `claimed`. After the lease it is reclaimed and the handler runs again — **the duplicate-execution window W1**, narrowed but not closed. The delivery axis makes the duplicate *visible* (`delivery_state` returns to `'claimed'` with a fresh `claimed_by`), which the current schema cannot do. The *effect* duplication requires per-handler idempotency (§3.10). **Partly achievable.** |
| 4 | **Lease expiration and reclaim** | The corrected predicate admits `delivery_state = 'claimed' AND lease_expires_at < now`. The superseded **report's** §7.3.1 predicate can **never** admit it — proved in Appendix A, E3, and it is exactly the report's §7.0 C4 "permanent-loss window". **Achievable.** |
| 5 | **Stale owners attempting settlement** | Under the singleton, unreachable in production: the scheduler holds an exclusive `flock` for its lifetime and `MarkWakeFired` has exactly one production caller. But the proposed guard does not fence a stale owner — proved in Appendix A, E4. ★B adds the `claimed_by` term so the axis is correct **by construction**, not by an invariant that lives in another process. **The settlement deliberately has no lease test** — the fencing token subsumes it, because a reclaim necessarily rewrites `claimed_by`, and a lease test would *reject* a legitimate late settlement and turn a completed delivery into a redelivery (§3.5.1). **Achievable, and the guard is one predicate term.** |
| 6 | **Clock consistency** | The lease is evaluated entirely on the DB clock, in the same statement that writes it (§3.3). `fired_at` stays on the process clock and is never compared against a DB-clock field (§3.6). NTP-step exposure is identical for both clocks and is not claimed to be eliminated. **Settled.** |
| 7 | **Future-dated wake eligibility** | `target_time <= ?` is preserved **in the claim statement**, evaluated on the DB clock (§3.4, §3.7). The previous revision stated it in the predicate but omitted it from the statement (B1). **Settled.** |
| 8 | **Rollback to an older binary** | Binary rollback is safe. The C7 hazard is closed **structurally** by ★A (`fired = 0` in the claim predicate), not by policy. Schema rollback is forbidden, and the rule is enforced by SQLite (§3.2). `mpm-update` never opens the database, so the migration lands lazily. **Settled.** |
| 9 | **Drill and broadcast non-idempotence** | `drill` is the **live** risk: it creates a fresh `uuid.NewString()` run id **and** session id per execution (`internal/scheduler/drill_handler.go:74`) and re-runs the whole harness. `broadcast` is already content-deduplicated (`shared.bcast_event_wakes.wake_id` is the PRIMARY KEY, `INSERT OR IGNORE`) — and, separately, `BroadcastHandler` cannot succeed today (it passes `--target`, which the CLI rejects, and the wake carries no memory id). **Neither kind has a producer in this tree** (§3.3.2), so today neither can receive a wake at all; the risks are **latent**, not live. The report's priority order is therefore **inverted**: drill first, broadcast second. **Achievable per handler; the handler, not the axis, is the unit of idempotency.** |
| 10 | **Duplicate cascade materialisation or dispatch** | Materialisation is closed by #213. Scheduling is bounded by the `wake_scheduled` flag, the read-back assertion and the reconcile pass, with the per-check delivery cap (`MaxCascadeWakePerCheck = 3`) as the bound. The residual is a *bounded* duplicate delivery signal, not an unbounded one. The delivery axis does not change this. **Already bounded; not a target of this redesign.** |

#### 3.9.1 One case the ten conditions do not cover: the stranded wake (W7)

The ten conditions are all about **crashes**. There is one non-crash outcome that the axis must
classify, and the previous revision never classified it (B6): **the handler never ran, and the wake
was retired anyway.**

`executeOne` refuses a blocked kind **before** running any handler
(`internal/scheduler/scheduler.go:590-592`), and `Tick` retires the wake with the refusal reason
(`:511`). The wake is therefore **terminal with no delivery** — a deliberate, documented behaviour
(`:586-589`), not a defect.

**Why it matters for the axis.** Under the previous revision's single-transition design, the failure
path called the unfenced `MarkWakeFired`, so the stranded case and the failed-handler case were
**indistinguishable**: both produced `fired = 1` with `last_error`. The axis exists to record
whether the scheduler's handler completed — so it must distinguish them. It now does:

| case | handler ran? | axis state after the transition |
|---|---|---|
| the handler completed (success **or** failure) | **yes** | `delivery_state = 'delivered'`, `delivered_at` set, `fired = 1`, `last_error` = the handler's error if any |
| **the dispatch gate refused the kind** | **no** | `delivery_state = NULL`, `delivered_at = NULL`, `fired = 1`, `last_error` = the refusal reason, `claimed_by`/`claimed_at` retained as the record of the claim that was released |

**The stranded case is terminal and is not retried.** The axis must not turn a deliberate refusal
into a retry loop. It is also the one case in this series where the *current* schema genuinely
loses information about a procedure — today the row is indistinguishable from a failed handler —
so the distinction is a real gain, not a bookkeeping nicety.

**A refusal is not a claim failure.** If the **claim** itself is refused (`RowsAffected == 0`), no
handler runs and the wake is **not** retired: the wake belongs to another claimant, or is not yet
due, or was retired already. The scheduler simply skips it (§3.4).

### 3.10 What is achievable, and what requires handler-specific idempotency

**Achievable by the delivery axis:**

- a crashed claim is **recovered** rather than lost;
- a duplicate delivery is **visible** as a distinct state, with the owner and the attempt recorded;
- exactly **one** claimant wins a contested claim;
- a **stale** claimant cannot settle;
- a wake that a **non-axis** path retired is never mistaken for a delivery;
- a handler that **never ran** (the refusal case, §3.9.1) is never mistaken for a delivery;
- a mixed-version window introduces **no new** duplicate-execution window.

**Not achievable by the delivery axis, at any level of effort:**

- **exactly-once external execution.** A settlement records that a handler ran; it cannot make a
  broadcast to another agent's process or a spawned drill harness happen once. (§7.0 C8.) This
  specification makes **no** such claim, and no stage's acceptance criterion may be written as one.

**Requires handler-specific idempotency — the per-handler matrix.** The previous revision's matrix
omitted the question that decides the whole table: **can the handler run at all?** A registered
handler is not a reachable handler (§3.3.2), and four of the six cannot run today. The matrix is
rewritten with that column, with the citations corrected, and with the `cascade_summary` row
corrected — the handler writes **no row**.

| Handler | Can it run today? | Duplicate effect *if* it ran | Idempotency already present | Stage | The guard |
|---|---|---|---|---|---|
| **`cascade_summary`** | **yes** — it is the only handler with a producer (§3.3.2) | a second **log line**. The handler writes **no row** — the `cascade_summary` wake row is written by the *producer* (`ScheduleCascadeSummaryWake`, `internal/core/wake_tools.go:105-119`), and the handler only emits one `slog.Info` line and returns `nil` (`internal/scheduler/cascade_summary.go:42-59`) | **none needed** | — | **accepted**: a duplicate delivery produces a duplicate forensic log line and **no** duplicate row. The previous revision's "the handler writes a summary row" was wrong. |
| `drill` | **no** — no producer; it would need `metadata.path` | a second `drill_runs` row with a fresh run id **and** session id, and a second full harness run (`claude_code` spawns Claude Code) — `internal/scheduler/drill_handler.go:74-75` | none | **H** | `ALTER TABLE drill_runs ADD COLUMN wake_id` + a unique index; the handler's insert **is** the claim (`internal/scheduler/drill_handler.go:93-107`). The run id stays a uuid — keying `drill_runs.id` by the wake id would change the id that `tool_invocations.session_id` and the drill report reference. |
| `snapshot` | **no** — no producer | the second `.backup` **overwrites** the same file with a fresh copy and a **new mtime** (which reorders the rotation), or writes a **different** file and advances `rotateSnapshots(dir, 7)` — `internal/scheduler/scheduler.go:914`, `:926`, `:938` | none | **I** | **the complete mechanism, below** |
| `gc` | **no** — the invocation cannot succeed (§3.3.3) | a second GC pass | **at-most-once**: a 24-hour compare-and-swap on `system_config.last_gc_at` **taken before** the work — `internal/core/gc_tools.go:81-92` (**not** `cmd/mpm/handlers_gc.go:174-190`, which is the separate `mpm gc` CLI path) | — | none needed for the *duplicate*; but note the `dry_run` is **not** read-only — three hard-delete prunes run on a successful claim (`internal/core/gc_tools.go:114-122`) — and **fixing the invocation creates a GC that does not happen today**, which is an operator decision (§7.1, decision 8) |
| `broadcast` | **no** — the invocation cannot succeed (§3.3.3) | a second fan-out | **already content-deduplicated** (`shared.bcast_event_wakes.wake_id` PRIMARY KEY + `INSERT OR IGNORE`, `internal/core/broadcast.go:122`, `:231`, `:244`, `:518`) | **I** | the content dedupe is sufficient for the crash window; a stricter **wake-id** key is a second, independent key. `BroadcastHandler` must also be fixed — and fixing it **creates** an external effect that does not exist today, so it is an operator decision (§7.1, decision 8) |
| `critic_audit` | **no** — blocked at the **producer** (`internal/core/wake_tools.go:412-418`, the arming gate inside `ScheduleWake` — **not** `:247`, which is only the predicate `WakeKindBlocked`) **and** at the **dispatch gate** (`internal/scheduler/scheduler.go:590`) | a second critic cycle | the kind is **barred at both ends**; no wake of that kind is reachable | — | none needed. If the operator ever unblocks it, it needs a deadline (§3.3.6) and an idempotency guard before it is armed. |

**The unit of idempotency is the handler, not the axis.** The acceptance criterion for the whole
series is therefore **per handler** — "one `drill_runs` row", "one CAS claim", "one rotation step" —
and never "exactly one effect in total".

#### 3.10.1 The snapshot idempotency mechanism — specified completely

The previous revision's guard was *"a label derived from the **wake id** rather than the epoch, so a
duplicate delivery writes the same file; or an explicit `wake_id` guard. Must be decided before the
stage."* That is **not sufficient**, and the review's objection (B4) is upheld: a deterministic
filename makes the *name* idempotent, not the *effect*.

**Why the filename alone fails.** `sqlite3 .backup` **overwrites** an existing destination. So a
duplicate delivery that resolves to the same path rewrites the file with a **later** copy of the
database and gives it a **new mtime** — and `rotateSnapshots` sorts by mtime
(`internal/scheduler/scheduler.go:1148-1155`). The duplicate therefore **changes the rotation
order** and can cause a snapshot the operator wanted to be deleted. The second `.backup` still
happens, the integrity check still runs, and the rotation still advances. A filename is not a guard.

**The complete mechanism — five parts, all required:**

1. **A wake-id guard row is the claim.** The handler's **INSERT is the claim**, exactly as stage H
   does it for the drill:

   ```sql
   CREATE TABLE IF NOT EXISTS snapshot_deliveries (
       wake_id       TEXT PRIMARY KEY,
       snapshot_path TEXT NOT NULL,
       created_at    INTEGER NOT NULL DEFAULT (CAST(strftime('%s','now') AS INTEGER))
   );
   ```

   The handler inserts `(wake_id, snapshot_path)` **before** it touches the filesystem. A duplicate
   delivery violates the primary key, and the handler returns **without** re-running the backup, the
   integrity check, or the rotation. The insert is the claim, the same idiom, the same semantics.

2. **The filename is derived from the wake id, and only from the wake id.**
   `mpm_pre_critic_<wake-id>.db`. The epoch label has **one-second granularity** (§2.7), so it
   cannot distinguish two deliveries of the same wake from two deliveries a second apart — and the
   overwrite is exactly what makes that matter. `metadata.label` is therefore **no longer a filename
   override**: with the name derived from the wake id, a caller-supplied label could make two
   *different* wakes collide on one path, and the guard — keyed by wake id — would not prevent it.
   If the operator wants the label for readability, it is recorded in the guard row and appended to
   the wake-derived name, never used on its own.

3. **The backup is written to a temporary path and atomically renamed.** `sqlite3 .backup` writes
   directly to its destination; a crash mid-backup leaves a **partial file under the final name**,
   and the next attempt overwrites it. The handler writes `<final>.tmp` and renames — the
   substrate's canonical atomic-swap pattern (the Substrate Defense Triad's first rule, the same
   one `internal/scheduler/state.go:persistState` uses). A partial backup is never visible under the
   final name.

4. **The integrity check runs against the temporary file, before the rename.** A snapshot that is
   not sane must not become visible under the final name — the current code checks the final path
   (`internal/scheduler/scheduler.go:932`), which is correct only because the backup is written in
   place; with the temp-path shape the check moves to the temp path.

5. **The rotation runs inside the guarded region.** The rotation is part of the effect and it is
   **not** idempotent by itself — it deletes files based on mtime ordering
   (`internal/scheduler/scheduler.go:1156-1158`). It must be inside the region the guard protects,
   so a duplicate delivery does not advance it.

**The schema obligations this carries:**

- the guard table's DDL is owned by a **migration**, not by a schema slice — `attachShared`
  propagates only `BaseTables` (`internal/core/db.go:2132` and `:2160`), so a migration-owned table
  can never reach the shared database;
- the table is added to `CanonicalMPMSchema`, the restore allow-list
  (`internal/core/sql_dump_validator.go:141`), and `TestCanonicalSchemaSync`
  (`internal/core/sql_dump_canonical_schema_test.go:22`) is the static guard that keeps the two in
  step;
- the index and the table are **forward-only** in the same sense stage A is: the schema rollback
  rule (§3.8) applies to them too.

**And the acceptance criterion, per handler, never "exactly one effect in total":**

> `snapshot` ⇒ **one** backup file, **one** integrity check, **one** rotation step — and the file's
> path is derived from the wake id, so an operator can map a snapshot back to the wake that produced
> it.

---

## 4. The implementation order — stages

Each stage is independently mergeable, has its own acceptance gate, and names its stop conditions.
Stages A–B are the safe prefix; the delivery axis proper starts at E.

### Stage A — M1: the additive migration

| | |
|---|---|
| **Prerequisites** | none |
| **Files** | `internal/core/migration_scheduled_wakes_delivery_state.go` (new); `internal/core/db.go` (one call in the fatal migration transaction, beside `:3117` and `:3135`) |
| **DB invariants** | the five columns exist and are nullable; `idx_sw_delivery_claim` exists; the sentinel `scheduled_wakes_delivery_state_v1` is recorded exactly once; **no row is modified** |
| **Observable behaviour change** | **none** — no reader, no writer, no predicate changed |
| **Regression tests** | AT1–AT6 (§5) |
| **Compatibility** | old binaries ignore the columns; the schema is forward-only |
| **Rollback** | binary rollback is safe; schema rollback is forbidden |
| **Stop condition** | stop if any pre-existing row's bytes change, if the FTS shadow diverges, if the sentinel is recorded twice, or if `git diff --stat` touches `internal/scheduler/` |
| **Gate** | AT1–AT6 green; `mpm-lint --gate` PASS; `make docs-check` OK |

### Stage B — wake-ownership isolation · **MERGED**

#215 (the fold may not retire the scheduler's wakes) and #216 (the agent may not arm or resolve a
scheduler-owned kind). The report's stage B. **Closed.**

### Stage C — two independent defect fixes

> **Status: partly implemented in two open PRs.** This stage is not green-field work and must not be
> re-implemented.
>
> | fix | PR | state |
> |---|---|---|
> | **C1** — `AND fired = 0` on both sweep UPDATEs (W3) | **#226** (*"fix(wake): a retention sweep must not overwrite a concurrent settlement"*, head `5c09889`) | **OPEN** — `internal/scheduler/wake_expiration.go` +71/−10, `internal/scheduler/wake_expiration_race_test.go` +474 |
> | **C2** — the `json_valid(metadata)` probe in the notification claim and both sweeps (W4) | **#227** (*"fix(wake): a malformed metadata row must not stop healthy wake processing"*, head `d55f76c`) | **OPEN** — `internal/core/wake_tools.go`, `internal/scheduler/dispatch.go`, `internal/scheduler/wake_expiration.go`, `internal/scheduler/scheduler.go`, `internal/core/wake_settlement.go` |
> | **C3** — route P1's INSERT through `ExecTracked` with budget 5 (W2) | — | **not implemented anywhere** |
>
> **Two consequences the stage must absorb.**
>
> 1. **#226 and #227 both modify `internal/scheduler/wake_expiration.go`**, so they are not
>    independently mergeable and one must land first. #227 also touches
>    `internal/core/wake_tools.go` and `internal/scheduler/scheduler.go`, which stage E and stage F
>    will also touch — the stages are therefore sequenced by merge order, not only by dependency.
> 2. **#226 changes the sweep's read-back assertion** from "0 rows is an error" to "0 rows is a
>    skip" (`continue`), because the `AND fired = 0` guard turns a lost race into a benign no-op and
>    aborting the batch would roll back every candidate the sweep had already retired. The previous
>    revision's stop condition — *"stop if the sweep's read-back assertion starts rejecting a
>    legitimately-retired row"* — is therefore **wrong as written**: #226 makes it *stop rejecting*
>    exactly that row. The stop condition becomes: *"stop if the sweep's read-back assertion stops
>    asserting the rows the sweep itself retired"*.
>
> #227 also extracts the shared kind-matcher the previous revision described as "if extracted" —
> `WakeMetadataJSON`, `WakeKindExpr` and the canonical kind filters in `internal/core/wake_tools.go`
> — so the "if extracted" hedge can be dropped from this stage's file list.

| | |
|---|---|
| **Prerequisites** | none |
| **Files** | `internal/scheduler/wake_expiration.go` (both sweeps — **#226**); `internal/scheduler/dispatch.go` (the notification claim — **#227**); `internal/core/wake_tools.go` (the shared `json_valid` kind-matcher — **#227**, already extracted); `internal/core/wake_tools.go` (P1's INSERT — **C3, still to do**) |
| **Changes** | **C1** — add `AND fired = 0` to both sweep UPDATEs, closing W3 (the `fired_at` overwrite). **C2** — probe `json_valid(metadata)` in the notification claim's and both sweeps' kind predicates, closing W4. **C3** — route P1's INSERT through `ExecTracked` with budget 5, closing W2's lost-summary case; update the bare-write allowlist accordingly (the entry at `internal/core/bare_pool_write_ratchet_test.go:658-661` becomes stale and must be removed, and the ratchet test must stay green). |
| **Observable behaviour change** | a malformed-`metadata` row no longer stops notification delivery or the sweeps; a sweep no longer overwrites a concurrent settlement's `fired_at`; the cascade summary survives a busy writer |
| **Regression tests** | a characterisation test per fix: the sweep must not overwrite `fired_at` (#226's `wake_expiration_race_test.go`); the claim and the sweeps must survive a malformed row (#227's `wake_metadata_json_test.go`); P1 must survive a busy writer |
| **Compatibility** | no schema change; old binaries are unaffected |
| **Rollback** | fully revertible — no schema, no state |
| **Stop condition** | stop if the sweep's read-back assertion stops asserting the rows the sweep itself retired, or if the ratchet baseline changes in any way other than the expected removal |
| **Gate** | #226 and #227 merged (or their changes re-landed) and green; the C3 characterisation test green; the existing sweep and dispatch suites green unchanged; `mpm-lint --gate` PASS |

> **Why C before A?** These are independent of the delivery axis and are worth shipping on their own
> merit: W4 silently stops notification delivery, and W3 destroys the one piece of forensic evidence a
> post-mortem has. Neither is urgent in the sense of being a data-loss emergency, and both are
> smaller than stage A. **If the operator prefers to land the migration first, the order A → C is also
> correct** — C does not depend on A, and A does not depend on C.

### Stage D — the per-handler deadline, and the measurement it rests on

> **Rewritten.** The previous revision's stage D applied a universal 15-minute deadline to every
> system handler and derived the lease from it. The review's objection (B5) is upheld: the constant
> had no evidence base, and the sentence that justified it was wrong in three ways (§3.3.1). The
> deadline is now a **per-handler registry**, each value justified by the tree, and the values that
> cannot be justified are **left unset** rather than guessed.

**This stage has two halves, and the order matters.**

#### D1 — make the uncancellable tail cancellable (a prerequisite)

The lease must cover the handler's runtime **plus** the time from the handler's completion to the
settlement landing. Two handlers have work outside their cancellable region that **no context can
interrupt** (§3.3.5):

| handler | the tail | the change |
|---|---|---|
| `drill` | the scoring (`internal/scheduler/drill_handler.go:126`) and the closing `UPDATE drill_runs` (`:135-149`), both outside the dispatch ctx. `WithTx` takes **no context** (`internal/core/db.go:1163`), so the write cannot be cancelled. | the closing write moves **inside** the dispatch ctx's lifetime, and the write path takes a context (`ExecContext`), so a cancelled drill does not leave a `drill_runs` row stuck at `status='running'` |
| `snapshot` | `rotateSnapshots(dir, 7)` (`internal/scheduler/scheduler.go:938` → `:1122-1160`) — it never reads `ctx` | `rotateSnapshots` takes the ctx and checks it between the directory read and the unlinks |

**Until D1 lands, no settlement margin can be derived**, and therefore no lease can be derived.
D1 is small and it is not optional.

#### D2 — the deadline registry, and the measurement behind it

| | |
|---|---|
| **Prerequisites** | D1 |
| **Files** | `internal/scheduler/scheduler.go` (`executeOne` — apply the registry's deadline to the handler's context); a new `internal/scheduler/handler_deadlines.go` (the registry); `internal/scheduler/drill_handler.go` (report its own effective bound to the dispatcher) |
| **Change** | a per-handler deadline registry, `map[kind]time.Duration`, applied at the dispatch site. The effective deadline for a wake is `min(registry[kind], the handler's own reported bound + the tail margin)`; a kind absent from the registry gets the registry's documented **default**. **No value is invented here**: the registry's initial contents are the ones justified in §3.3.6 — `cascade_summary` takes the default (it is bounded by construction), `drill` is capped by the drill's own YAML `timeout_secs` read at dispatch time, and `snapshot` has **no registered deadline** until a producer exists and a measurement can be taken. |
| **DB invariants** | none |
| **Observable behaviour change** | a handler that runs longer than its deadline is cancelled and the wake settles as failed with `last_error` — instead of running unbounded. **The change now applies only to the kinds that can run** (§3.3.2). |
| **Regression tests** | a handler that blocks past its deadline is cancelled and the wake settles terminal; a handler that completes inside its deadline is unaffected; the drill's own `timeout_secs` is unaffected; a kind with **no** registered deadline is dispatched with the **registry default** and the log records which value was used; the drill's `timeout_secs` is honoured even when it is **larger** than the registry cap (the cap wins) and when it is **smaller** (the drill's own bound wins) |
| **Compatibility** | behaviour change on the dispatch path; no schema change |
| **Rollback** | fully revertible |
| **Stop condition** | stop if any production handler is observed to exceed its deadline in a live tick — **and** stop if the observation window is not yet complete (§D3) |
| **Gate** | the deadline tests green; the existing scheduler suite green unchanged |

#### D3 — the measurement, and what it gates

The `duration_ms` field is **already logged on every exit path** of `executeOne`
(`internal/scheduler/scheduler.go:611-635`), so no new instrumentation is needed to read it. What
D3 fixes is the **protocol**, so that the deadline has a provenance:

1. **The observation window is stated in advance** — for example "the first 30 days of the
   scheduler's logs in the production workspace" — and the window is long enough to cover at least
   one instance of the kind's normal cadence.
2. **The measurement is per kind**: the **maximum** observed `duration_ms` per `kind`, and the
   **count** of observations. A kind with **zero** observations has **no** measured bound, and no
   deadline may be registered for it on the strength of a measurement that did not happen.
3. **The result is recorded in this document** — a table of `kind → max observed duration_ms →
   observations → registered deadline` — and the registered deadline is derived from it with a stated
   factor.
4. **The measurement gates stage E.** Until the table exists, `DeliveryLeaseSeconds` is undefined
   and stage E cannot be implemented (§3.3.6). This replaces the previous revision's post-hoc stop
   condition with a pre-flight one.

**The drill's own bound is not a measurement and must not be substituted for one.** It is a
**configuration** value — YAML-supplied and uncapped (§3.3.1) — and it bounds the drill only if
the operator has set it. The registry's cap is the operator's defence against an uncapped YAML.

> **Why D before E:** the lease duration is a function of the maximum handler runtime. Without a
> bounded runtime the lease cannot be bounded, and a lease shorter than a legitimate handler is a
> duplicate-execution generator. D is the prerequisite that makes E's lease duration a defensible
> number — and it is a defensible number only once the **measurement** in D3 has been taken.

### Stage E — M3a: the claim

| | |
|---|---|
| **Prerequisites** | A (the columns exist), D (the deadline bounds the runtime) |
| **Files** | `internal/core/wake_tools.go` or a new `internal/core/wake_claim.go` (the claim function and the shared predicate builder); `internal/scheduler/scheduler.go` (`Tick` — claim before running); `internal/scheduler/system_handlers.go` (the claim token's instance uuid) |
| **DB invariants** | at most one live claim per wake; `lease_expires_at > claimed_at`; `delivery_state ∈ {NULL, 'claimed'}` for a wake that is not settled |
| **Observable behaviour change** | the scheduler claims a wake before running its handler; a crashed claim is recovered after the lease instead of being re-run on the next tick; a contested claim has exactly one winner |
| **Regression tests** | **AT8a–AT8d** (all four sub-tests) and **AT9a–AT9c**; a claim that is refused leaves the wake untouched; a reclaimed claim carries a **new** `claimed_by`; **a future-dated wake is not claimable — asserted directly on the statement** (`RowsAffected == 0` for a wake whose `target_time` is in the future), not only through the `QueryDueWakes` filter; the notification path's `dispatched_at` behaviour is unchanged |
| **Compatibility** | `delivery_state IS NULL` rows are claimable, so a mixed-version database works unchanged; the notification path is untouched |
| **Rollback** | reverting the stage leaves the columns `NULL` on rows that were claimed — the claim is then a no-op and the behaviour is today's. **Do not roll back below this stage while rows are `claimed`** (an older binary ignores the axis and re-dispatches them — today's behaviour, and a runbook note). |
| **Stop condition** | stop if the notification path's `dispatched_at` behaviour changes, or if a tick's wall-clock cost grows materially |
| **Gate** | AT8a–AT8d and AT9a–AT9c green; the existing scheduler suite green unchanged |

> **The claim statement carries `target_time <= ?`** (B1). The previous revision stated the term in
> the predicate and omitted it from the statement, so §3.7's claim that "the claim re-checks
> eligibility on the DB clock" was not true of the code it described. The statement is now the same
> expression as the predicate (§3.4), and the acceptance matrix asserts the term **directly on the
> statement** so that a future edit cannot drop it again.

### Stage F — M3b: the settlement, and the two transitions around it

| | |
|---|---|
| **Prerequisites** | E; **and #227 landed** (it defines `WakeMetadataJSON`, which the transitions' metadata write requires — §3.5.1) |
| **Files** | `internal/core/wake_settlement.go` (a **new** fenced settlement function, a **new** refusal transition and a **new** release transition; `MarkWakeFired` is unchanged but is **no longer called** by the scheduler's dispatch path); `internal/scheduler/scheduler.go` (`Tick` passes the claim token and selects the transition) |
| **DB invariants** | a settlement moves `claimed → delivered` and writes `delivered_at`, `fired = 1` and `fired_at` **in one statement**; a settlement that is **refused** changes nothing and is **reported**; a settlement that **errors** triggers a fenced **release**; a **refusal** transition moves `claimed → NULL` and retires the wake **without** a delivery |
| **Observable behaviour change** | a scheduler-settled row carries `delivery_state = 'delivered'` and `delivered_at`; a **failed handler** also settles through the fenced path, so the row carries `delivery_state = 'delivered'` with `last_error` set — **never** `fired = 1` with `delivery_state = 'claimed'`; a **refused** kind is retired with `delivery_state = NULL` and `delivered_at = NULL`; a refused settlement is reported by `Tick` and the wake is **not** re-run by this claimant |
| **Regression tests** | **AT9d** (the stale settlement), **AT10** (boundary 5), **AT13** (the three outcomes) and **AT14** (the contradictory-state detector); a claimed-then-settled row has `fired = 1` + `delivery_state = 'delivered'`; **a failed handler yields `fired = 1` + `delivery_state = 'delivered'` + `last_error`, and never `fired = 1` + `delivery_state = 'claimed'`**; **a refused kind yields `fired = 1` + `delivery_state = NULL` + `delivered_at = NULL` + `last_error`, with `claimed_by` retained**; a fold-retired row and a sweep-retired row have `fired = 1`, `delivery_state IS NULL` and `delivered_at IS NULL`; `MarkWakeFired`'s existing contract is unchanged |
| **Compatibility** | mixed-version safe: an older binary's settlement still lands `fired = 1` |
| **Rollback** | as stage E — do not roll back while rows are `claimed` |
| **Stop condition** | stop if any retirement path outside the scheduler's dispatch is found to write `delivery_state`; stop if a row can be observed with `fired = 1` and `delivery_state = 'claimed'` after the stage lands |
| **Gate** | the settlement tests green, including AT13 and AT14; `internal/scheduler/wake_settlement_test.go` green **unchanged** |

> **B2 — the failure path must be fenced.** The previous revision left the scheduler's failure path
> calling the unfenced `MarkFired` (`internal/scheduler/scheduler.go:511`), which writes `fired = 1`
> without touching `delivery_state`. Under the axis that produced the state
> `fired = 1 AND delivery_state = 'claimed'` — a state the previous revision's own §3.5 said only a
> non-axis writer could produce. This stage replaces it with the fenced settlement (§3.5.1), and the
> "stop if a row can be observed with `fired = 1` and `delivery_state = 'claimed'`" condition is the
> standing detector for the defect.

> **B3 — the settlement-failure policy is decided.** A settlement that **errors** releases the
> claim, best-effort and fenced by `claimed_by = ?`, so the wake is immediately eligible again
> exactly as today (§3.5.4). The recovery latency is stated explicitly: **one tick interval** (the
> daemon's default `--interval` is 60 s) in the normal case, and **up to the lease** in the worst
> case, where the release itself fails. The alternative — accepting the lease as the only recovery
> path — was a 60 s → 30 min regression in the common case and is not adopted.

### Stage G — M5: the tick-level retry

| | |
|---|---|
| **Prerequisites** | E, F |
| **Files** | `internal/scheduler/scheduler.go` (`Tick`) |
| **Change** | a busy claim or a busy settlement is retried at the **tick** level with an **explicit, asserted bound** — "the scheduler tick retries" is a new behaviour and `Tick` has no retry loop today, so an unbounded version is a busy-loop |
| **Regression tests** | the retry **count** is asserted; a busy claim is retried at the tick level and **not** inside the transaction; `internal/core/tx_busy_layering_contract_test.go` passes |
| **Stop condition** | stop if the retry makes a tick exceed the tick interval under contention |
| **Gate** | the retry tests green; the layering contract green |

### Stage H — M4a: the drill guard

| | |
|---|---|
| **Prerequisites** | F (the axis exists, so a duplicate delivery is detectable) |
| **Files** | `internal/core/schema.go` (or a migration) for `ALTER TABLE drill_runs ADD COLUMN wake_id` + a unique index; `internal/scheduler/drill_handler.go` |
| **DB invariants** | at most one `drill_runs` row per `wake_id` |
| **Observable behaviour change** | a duplicate delivery of a drill wake produces **one** `drill_runs` row and **one** harness invocation |
| **Regression tests** | AT11-drill: two deliveries ⇒ one row with a non-NULL `wake_id`; the invocation count is asserted; the unique index rejects a duplicate |
| **Rollback** | binary rollback is safe (the column is additive); schema rollback is forbidden |
| **Stop condition** | stop if keying by `wake_id` would change the `drill_runs.id` that `tool_invocations.session_id` and the drill report reference |
| **Gate** | the drill tests green; the existing drill suite green |

### Stage I — M4b/M4c: the snapshot guard and the broadcast

> **Restructured.** The previous revision assigned *both* the broadcast and the snapshot to stage I,
> but stage I was **the broadcast stage**, which is **optional** and gated on the operator's
> decision. AT11 nevertheless asserted *"snapshot ⇒ one rotation step"*, and **no stage implemented
> it** (B4). Stage I now carries the two guards as **independent parts**, each with its own
> precondition and its own gate, so the snapshot's guard is never optional by accident.

#### I-a — M4c: the snapshot guard

| | |
|---|---|
| **Prerequisites** | F; and the **operator's decision** in §7.1 (decision 11) |
| **Files** | a new migration file owning `CREATE TABLE IF NOT EXISTS snapshot_deliveries …` — **migration-owned, not a schema slice**, so `attachShared` (which propagates only `BaseTables`, `internal/core/db.go:2132` and `:2160`) can never propagate it into the shared database; `internal/core/sql_dump_validator.go` (`CanonicalMPMSchema`, `:141`) — the restore allow-list; `internal/scheduler/scheduler.go` (`SnapshotHandler`) |
| **DB invariants** | at most one `snapshot_deliveries` row per `wake_id`; the row's `snapshot_path` is the path the handler wrote |
| **Observable behaviour change** | a duplicate delivery of a snapshot wake produces **one** backup file, **one** integrity check and **one** rotation step. The filename becomes `mpm_pre_critic_<wake-id>.db`, so an operator can map a snapshot back to the wake that produced it, and `metadata.label` stops being a filename override |
| **Regression tests** | AT11-snapshot: two deliveries ⇒ one file, one integrity check, one rotation step; the guard row's `wake_id` is non-NULL and unique; the backup is written to a temporary path and renamed; the integrity check runs against the temporary path; the rotation does not advance on the second delivery; `TestCanonicalSchemaSync` is green with the new table |
| **Rollback** | binary rollback is safe (the table is additive); schema rollback is forbidden |
| **Stop condition** | stop if the wake-derived filename would break an operator's existing snapshot-naming script, or if the temp-path + rename shape cannot be applied to `sqlite3 .backup` |
| **Gate** | the snapshot tests green; `TestCanonicalSchemaSync` green; the existing scheduler suite green |

The mechanism itself is specified in full in **§3.10.1** — the guard row, the wake-derived
filename, the temp-path + atomic rename, the integrity check against the temp path, and the
rotation inside the guarded region. **A deterministic filename alone is explicitly not the guard.**

#### I-b — M4b: the broadcast

| | |
|---|---|
| **Prerequisites** | F; and the **operator's decision** in §7.1 (decision 8) |
| **Files** | `internal/scheduler/scheduler.go` (`BroadcastHandler`); `cmd/mpm/ops_broadcast_cmds.go`; `internal/core/broadcast.go` (the wake-id dedupe key) |
| **Observable behaviour change** | **fixing the handler creates a fan-out that does not exist today** — an external effect. This is the operator's call |
| **Regression tests** | AT11-broadcast: two deliveries ⇒ one fan-out; the content dedupe still holds; the CLI accepts the handler's argument shape |
| **Stop condition** | stop if the operator declines the behaviour change — the content dedupe already bounds the crash window, so the part is optional |
| **Gate** | the broadcast tests green |

> **The two parts are independent.** I-a may land without I-b, and I-b may be declined without
> blocking I-a. **Stage K's prerequisite is "the guards that exist"**, not "every guard" — if the
> operator declines I-b, stage K's assertion is narrowed to the handlers that have a guard, and
> that narrowing is stated in the replacement test's docstring rather than left implicit.

### Stage J — M2 (deferred): the narrowed reader migration

| | |
|---|---|
| **Prerequisites** | F, and the axis proven in production |
| **Change** | the **three** scheduler-dispatch readers read the axis; the **thirteen** retirement readers are **unchanged**. "Read `delivery_state` for the dispatch questions only" — not "instead of `fired` everywhere" |
| **Regression tests** | the thirteen retirement readers are unchanged; the existing wake suite passes unchanged |
| **Stop condition** | stop if any retirement reader would have to change |
| **Gate** | the full wake suite green unchanged |

### Stage K — M6: replace the characterisation test

| | |
|---|---|
| **Prerequisites** | F, H, and **I-a** (the snapshot guard). **I-b is optional** — if the operator declines the broadcast, this stage's assertion is narrowed to the handlers that have a guard, and the narrowing is recorded in the replacement test's docstring |
| **Change** | add `TestWakeDelivery_ARestartBeforeSettlementLeavesTheWakeClaimed` **and only then** remove `TestWakeDelivery_ARestartBeforeSettlementLeavesTheWakePending` — **in the same PR**, with the old docstring pointing at the replacement |
| **Why not earlier** | the characterisation test's own docstring says *"If a future change closes this window, THIS TEST FAILS, and that is the point"*. M1–M5 do **not** close it — they narrow it — so removing the pin before the per-handler guards land removes it too early. |
| **Assertion** | **per handler**, never "exactly one effect in total": drill ⇒ one `drill_runs` row and one harness invocation; snapshot ⇒ one backup file, one integrity check and one rotation step; `cascade_summary` ⇒ one log line and no row. A handler with **no** guard (because its part of stage I was declined, or because it cannot run) is asserted as **"the duplicate is visible in the axis"** — the row carries a fresh `claimed_by` — and the docstring says which guards are outstanding. |
| **Residual** | W1 remains — the handler **may** run twice; only the effect is deduplicated, per handler. This is stated in the replacement test's docstring. |

> **The residual is a duplicate, not a loss.** The previous revision's §3.9 condition 1 called the
> pre-axis behaviour "permanently lost", which is wrong (§3.9 condition 1, and the characterisation
> test's own assertion that the wake "must still be pending"). The replacement test must not
> reintroduce that error: the wake is **re-run**, and what the axis changes is that the re-run is
> **visible** and the *effect* is deduplicated per handler.

---

## 5. The acceptance-test specification

All twelve are deterministic. **No test may contain a sleep or an arbitrary wait.** The two
mechanisms used are the **SQLite authorizer barrier** (already in the tree at
`internal/core/wake_resolve_contention_test.go`, which pauses a statement at `PREPARE` time and
therefore observes the arrival explicitly before doing anything else) and **subprocess re-exec with a
named crash point** (the standard Go pattern: the test re-executes itself with an environment
variable naming the crash point, signals the parent over a pipe, and the parent `SIGKILL`s it).

**The matrix is subdivided so that the revised tests can tell five things apart.** A criterion that
passes for more than one of them cannot fail when the corresponding defect is present, so each of
safe claim, lease expiry, stale settlement, recovery and duplicate external effect has its own
sub-test with its own mechanism and its own discriminating assertion.

| # | Scenario | Deterministic mechanism | Assertion |
|---|---|---|---|
| **AT1** | **Existing database migration** | a disposable file-backed WAL DB with the **pre-M1** schema (the exact base DDL plus the `dispatched_at` and `fired_by` migrations and their sentinels), populated with representative rows; then opened by the new binary | the five columns exist; the sentinel count is 1; the column set is exactly the post-M1 set |
| **AT2** | **Fresh database creation** | a new DB through the same path | the column set is **identical** to AT1's; the sentinel count is 1; the base DDL and the migration do not conflict |
| **AT3** | **Whole-row preservation** | snapshot every pre-existing row's every pre-existing column before and after, as a whole-row string | **byte-identical** — not just `fired`; the whole row set, compared as a map keyed by id |
| **AT4** | **Zero fabricated delivery history** | rows with `fired = 1` from each of the five retirement paths, plus `kind='cron'` rows | every row has `delivery_state IS NULL` and `delivered_at IS NULL`, **including** the `fired = 1` rows and the never-deliverable cron rows |
| **AT5** | **Idempotent migration** | run the migration twice, then a third time | the column set, the row set, the index list and the sentinel count are unchanged by runs 2 and 3 |
| **AT6** | **Failed-migration rollback** | **a trigger that aborts the sentinel write** — `CREATE TRIGGER block_m1_sentinel BEFORE INSERT ON schema_migrations WHEN NEW.id = '<sentinel>' BEGIN SELECT RAISE(ABORT, 'injected M1 failure'); END` | the migration returns an error; the column set is **unchanged**; **the index was not created**; no sentinel is recorded; the row set is unchanged; `NewDatabaseManager` surfaces the error |
| **AT7** | **Mixed-version operation** | (a) run the **pre-M1 statements** (the old settlement, the old fold, the old resolver, the old claim) against an M1-schema DB; (b) a row settled by the old binary (`fired = 1`, `delivery_state IS NULL`) | (a) the old statements still work and leave `delivery_state` untouched; (b) the row is **not** re-claimed — this is the C7 hazard, closed by ★A |
| **AT8** | **The claim statement's own safety** — four sub-tests, each with its own discriminating assertion | each sub-test calls the **claim statement directly**, so the assertion is about the statement and not about the query that feeds it | **AT8a — a safe claim.** A due, `delivery_state IS NULL` wake is claimed exactly once (`RowsAffected == 1`); the same statement a second time affects **0 rows**; the claimed row has `delivery_state = 'claimed'`, a non-NULL `claimed_by`, and `lease_expires_at > claimed_at`. **AT8b — a future-dated wake is not claimable.** A wake whose `target_time` is in the future ⇒ `RowsAffected == 0`, **asserted on the statement itself**, not only through the `QueryDueWakes` filter. *This is the test that pins B1: with the term absent from the statement, this sub-test fails and every other sub-test still passes.* **AT8c — a retired wake is not claimable.** `fired = 1` ⇒ `RowsAffected == 0`. **AT8d — a delivered wake is not claimable.** `delivery_state = 'delivered'` ⇒ `RowsAffected == 0`. |
| **AT9** | **Lease expiry, contention and the stale settlement** — four sub-tests | the lease windows are computed from the **DB clock** (`CAST(strftime('%s','now') AS INTEGER) ± 1` or `± 3600`) — **no sleep**; the contention case uses the authorizer barrier | **AT9a — lease expiry.** A claimed row with an **expired** lease is reclaimable; a claimed row with a **live** lease is **not**; the reclaim carries a **new** `claimed_by` and a **new** `claimed_at`. **AT9b — a live lease is not expired.** The negative half of AT9a, asserted separately, so a predicate that admits every claimed row cannot pass AT9a. **AT9c — contested claim.** Two claimers paused at the `UPDATE` boundary and then both released ⇒ **exactly one** sees `RowsAffected == 1`. **AT9d — the stale settlement.** Owner A's lease expires, owner B re-claims, A then settles ⇒ A's settlement affects **0 rows**, is **reported**, and the row is still owned by B. Under the singleton this is a defence-in-depth assertion. |
| **AT10** | **Process death at each dispatch boundary** | subprocess re-exec with `MPM_WAKE_CRASH_AT ∈ {after_select, after_claim, during_handler, after_effect, during_settlement}`; the child signals the parent at the crash point and the parent `SIGKILL`s it | at each boundary: the post-crash row state is asserted **exactly**; after `after_claim` the wake is reclaimable after the lease and **not** before; after `after_effect` the wake is re-dispatched (the duplicate is visible as a **fresh** `claimed_by`); after `during_settlement` the wake's claim is **released** (`delivery_state IS NULL`) and the wake is **immediately** re-dispatched — **not** left claimed until the lease expires. *This is the test that distinguishes the settlement-**errored** case from the settlement-**refused** case, and it is the executable form of the B3 policy.* |
| **AT11** | **Duplicate external effects — one sub-test per handler** | deliver the same wake twice to each handler, and count the effect | **per handler**: `drill` ⇒ one `drill_runs` row with a non-NULL `wake_id` and one harness invocation; `snapshot` ⇒ **one** backup file, **one** integrity check and **one** rotation step, with the path derived from the wake id; `broadcast` ⇒ one fan-out; `cascade_summary` ⇒ one log line and **no** row. **Never** "exactly one effect in total". Each sub-test counts **its own** handler's effect, so a sub-test for one handler cannot pass because another handler's guard works. |
| **AT12** | **Preservation of agent-owned wake behaviour** | the existing suites, run **unchanged** | `internal/core/wake_ownership_test.go`, `internal/core/tools/wake_resolve_ownership_test.go`, `internal/core/tools/wake_arming_authorisation_test.go`, `internal/scheduler/wake_ownership_test.go`, `internal/scheduler/wake_settlement_test.go`, `internal/core/wake_resolve_contention_test.go` all pass **unchanged**; the fold, the resolver, `kinds:["*"]`, the notification claim and both sweeps read and write **no** new column |
| **AT13** | **The three dispatch outcomes are distinguishable** | three fixtures, one per outcome: a handler that returns nil; a handler that returns an error; a kind the dispatch gate refuses (`critic_audit`, which `executeOne` refuses at `internal/scheduler/scheduler.go:590`) | the success outcome ⇒ `fired = 1` + `delivery_state = 'delivered'` + `delivered_at` set; the **failure** outcome ⇒ `fired = 1` + `delivery_state = 'delivered'` + `metadata.last_error` set (**the handler ran**); the **refusal** outcome ⇒ `fired = 1` + `delivery_state IS NULL` + `delivered_at IS NULL` + `metadata.last_error` set (**the handler did not run**) + `claimed_by` retained. *This is the test that makes W7 representable, and it fails against the previous revision's single-transition design.* |
| **AT14** | **The axis is never left in the contradictory state** | run every dispatch path in AT13 and AT10 and inspect every row afterwards | **no** row is ever observed with `fired = 1` **and** `delivery_state = 'claimed'`. *This is the standing detector for B2: with the unfenced `MarkWakeFired` on the failure path, this assertion fails.* |

**AT6's mechanism was corrected in this revision.** The previous revision specified *"a trigger that
aborts the **third** `ALTER TABLE`"*. **SQLite does not offer that mechanism** — `ALTER TABLE … ADD
COLUMN` fires no triggers, so no such trigger can be installed. The executable mechanism is the one
Stage A's implementation uses: a trigger that aborts the **sentinel write**, by which time the five
`ALTER TABLE`s and the `CREATE INDEX` have already succeeded — so the rollback has to undo **real
DDL**, which is a *stronger* assertion than aborting an `ALTER TABLE` would have been. PR #225's
`TestDeliveryStateMigration_AT6_FailedMigrationRollsBack` says exactly this in its own docstring.

**Which have been executed.**

| criteria | status |
|---|---|
| **AT1–AT6** | **implemented and green** in PR #225 (Stage A): `internal/core/migration_scheduled_wakes_delivery_state_test.go` — `TestDeliveryStateMigration_AT1_ExistingDatabaseMigration` through `…_AT6_FailedMigrationRollsBack`, plus the FTS-shadow, forward-only, dump-validator, dropped-index, dropped-axis, failing-index-creation, partially-migrated and post-failure-recovery tests. |
| **AT8, AT9** | executed as an isolated experiment before this specification was written (Appendix A) and re-specified here with the sub-tests that discriminate them |
| **AT7, AT10, AT11, AT12, AT13, AT14** | specified against mechanisms that already exist in the tree and **have not yet been executed** |

---

## 6. The regression matrix

| Suite | Location | Gate |
|---|---|---|
| wake settlement characterisation | `internal/scheduler/wake_settlement_test.go` | unchanged through stage F; the restart test is replaced only in stage K |
| wake ownership (core) | `internal/core/wake_ownership_test.go` | unchanged throughout |
| wake ownership (scheduler) | `internal/scheduler/wake_ownership_test.go` | unchanged throughout |
| resolve ownership | `internal/core/tools/wake_resolve_ownership_test.go`, `internal/scheduler/wake_resolve_ownership_test.go` | unchanged throughout |
| arming authorisation | `internal/core/tools/wake_arming_authorisation_test.go` | unchanged throughout |
| resolver contention (#218) | `internal/core/wake_resolve_contention_test.go` | unchanged throughout |
| cascade materialisation (#213) | `internal/core/cascade_materialization_idempotency_test.go` | unchanged throughout |
| bare-write ratchet (#214) | `internal/core/bare_pool_write_ratchet_test.go` | the baseline must hold; stage C's expected removal is the **only** permitted change |
| transaction layering | `internal/core/tx_busy_layering_contract_test.go` | unchanged throughout |
| system handler registration | `internal/scheduler/system_handlers.go` + `internal/scheduler/wake_ownership_test.go` | the registered-kind set must not change without a stage that says so |
| scheduler dispatch | `internal/scheduler/dispatch_test.go` | unchanged — the notification claim is not touched |
| drill | `internal/scheduler/drill_handler_test.go`, `internal/scheduler/drill_e2e_test.go` | extended in stage H; **D1** also changes the drill's tail (the closing write becomes cancellable) and must keep these green |
| expiration sweeps | `internal/scheduler/wake_expiration_test.go` | extended in stage C — **#226 and #227 already extend it** |
| malformed-metadata handling | `internal/core/wake_metadata_json_test.go`, `internal/scheduler/wake_metadata_json_test.go` | **added by #227**; unchanged thereafter |
| sweep race | `internal/scheduler/wake_expiration_race_test.go` | **added by #226**; unchanged thereafter |
| the delivery-state migration | `internal/core/migration_scheduled_wakes_delivery_state_test.go` | **added by #225**; AT1–AT6 green; unchanged throughout the later stages |
| the restore allow-list | `internal/core/sql_dump_canonical_schema_test.go` (`TestCanonicalSchemaSync`) | **stage I-a** adds `snapshot_deliveries` and must add it to `CanonicalMPMSchema` in the same change |
| the handler deadline registry | a new `internal/scheduler/handler_deadlines_test.go` | **stage D** — the registry's contents, the two invariants, and the per-kind effective deadline are asserted |

**The `git diff --stat` discipline.** Stage A must show **no** change under `internal/scheduler/`.
Stage C must show no change under `internal/core/wake_settlement.go`. Stage E must show no change to
`internal/scheduler/dispatch.go`. These are the mechanical guards against an axis leaking into the
paths it must not touch.

**Two new mechanical guards, added by this revision.**

| guard | why |
|---|---|
| **Stage F: no row may be observed with `fired = 1` and `delivery_state = 'claimed'`.** | This is B2's detector. The previous revision's failure path used the unfenced `MarkWakeFired`, which produced exactly that state. It is asserted by **AT14** and it is stage F's stop condition. |
| **Stage I-a: `snapshot_deliveries` must be in `CanonicalMPMSchema`, and its DDL must be migration-owned.** | `TestCanonicalSchemaSync` (`internal/core/sql_dump_canonical_schema_test.go:22`) requires an exact two-way match between the `CREATE TABLE` statements in source and the allow-list, so a migration-owned table that is not allow-listed fails the static guard. And a migration-owned DDL is what keeps `attachShared` — which propagates only `BaseTables` (`internal/core/db.go:2132`, `:2160`) — from copying the table into the shared database. |

---

## 7. Unresolved risks and the operator's decisions

### 7.1 The operator's decisions

| # | Decision | Recommendation | Status |
|---|---|---|---|
| 1 | Is `fired` retired, a projection, or an independent axis? | **Independent** | settled here (§3.1) |
| 2 | Is `delivery_state` scoped to the scheduler axis or does it cover notifications too? | **Scheduler-dispatch only** | settled here (§3.1) |
| **3** | **Lease duration** | **Not a number in this document.** Set it from stage D's measurement: `max(registered deadline) + SettlementMargin`, where the margin covers the (now bounded) uncancellable tail and the settlement's own busy window | **OPEN — the previous revision's "30 min" is withdrawn.** Until the measurement is taken, `DeliveryLeaseSeconds` is undefined and **stage E cannot be implemented** (§3.3.6) |
| 4 | Lease clock authority | **The DB clock**, inside the statement | settled here (§3.3.8) |
| 5 | Is `claimed_by` observability or a fencing token? | **A fencing token** | **settled here (§3.5) — this contradicts the report's recommendation, on the evidence of Appendix A, E4** |
| 6 | Should `mpm_wakes schedule` be able to arm `drill` / `broadcast` / `snapshot` / `gc` wakes? | **No — an allowlist** | **closed by #216** |
| 7 | Should `kinds:["*"]` be permitted to retire system-owned wake kinds? | **No** | **closed by #215** |
| **8** | **Should `BroadcastHandler` be fixed?** | **Ask.** Fixing it **creates** an external effect that does not exist today. "The broadcast handler has never worked" may be load-bearing for an operator's mental model. | **open — stage I-b is optional** |
| **8b** | **Should `GCHandler` be fixed?** | **Ask, and with more care than the broadcast.** `mpm call gc_run` is not a callable tool name (§3.3.3), so **no GC has ever run from a wake**. Fixing it creates a GC that does not happen today — and the `dry_run` is **not** read-only: three hard-delete prunes run on a successful claim (`internal/core/gc_tools.go:114-122`). **The previous revision asked only about the broadcast**; this decision was **missing**. | **open — and it gates stage I-b's sibling, which does not exist yet** |
| 9 | Rollback policy during the axis window | binary rollback allowed and safe; **schema rollback forbidden**; do not roll back below stage E while rows are `claimed`; `--with-db-restore` restores a pre-migration backup and re-dispatches formerly-claimed rows (today's behaviour) | settled here (§3.8) — the runbook entry is still owed |
| 10 | Is the notification path in scope? | **No** | settled here (§3.1) |
| **11** | **How is the `snapshot` delivery made idempotent?** | **The complete mechanism in §3.10.1**: a wake-id guard row whose insert **is** the claim; the filename derived from the wake id (and `metadata.label` no longer a filename override); the backup written to a temp path and atomically renamed; the integrity check against the temp path; the rotation inside the guarded region | **the mechanism is settled here; the operator's remaining choice is whether stage I-a lands at all.** A deterministic filename alone is **not** sufficient and is explicitly rejected |
| **12** | **Is a per-handler deadline acceptable, and with what values?** | **Yes in principle; the values are not asserted here.** The registry is per-handler and each entry is justified in §3.3.6. `cascade_summary` takes the default (bounded by construction); `drill` is capped by its own YAML `timeout_secs`; `snapshot` has **no** registered deadline until a producer exists and a measurement can be taken | **open — a behaviour change on the dispatch path, now scoped to the kinds that can actually run** |
| **13** | **What happens when a settlement errors?** | **The claim is released**, best-effort, fenced by `claimed_by = ?`, so the wake is immediately eligible again exactly as today. Recovery latency: one tick interval (60 s) normally; up to the lease if the release also fails | **settled here (§3.5.4) — the previous revision's silence is closed** |
| **14** | **Should `metadata.label` still be a filename override for the snapshot?** | **No** — with the filename derived from the wake id, a caller-supplied label could make two *different* wakes collide on one path, and the guard (keyed by wake id) would not prevent it. The label may be recorded in the guard row for readability | **settled here (§3.10.1, part 2)** |

### 7.2 Residual risks

| # | Risk | Severity | Mitigation |
|---|---|---|---|
| R1 | **The lease does not close W1.** The handler may run twice; only the effect is deduplicated, per handler. | high | stated in every stage's gate; the series' success criterion is per-handler, never global; AT11 has one sub-test per handler so one handler's guard cannot mask another's |
| **R2** | **A handler that legitimately needs more than its deadline is now cancelled.** | medium | the deadline is **per-handler**, so only the affected kind is at risk; the drill's own bound is honoured when it is *smaller* than the registry cap; the registry cap is the operator's defence against an uncapped YAML `timeout_secs`; stage D ships separately so it can be reverted alone; the operator's decision 12 |
| **R3** | **The crash-recovery latency is undefined until the measurement is taken.** | low | it is a function of the lease, which is now derived (§3.3.6) rather than asserted; the kinds that can run are bounded by construction, so the observed maximum is small; a per-kind lease is a later refinement |
| R4 | **The `snapshot` rotation is not idempotent** | medium | the complete mechanism in §3.10.1 puts the rotation inside the guarded region; stage I-a now has a **prerequisite, a file list, a DB invariant and a gate** — it is not optional by accident, and it is independent of stage I-b |
| R5 | **`mpm-update --rollback --with-db-restore` reverts the schema.** | medium | the migration is idempotent and re-adds the columns on the next open; the re-dispatch of formerly-claimed rows is today's behaviour and belongs in the runbook |
| R6 | **Two schedulers running concurrently** (a misconfigured `--lock` / `MPM_DB_LOCK` / workspace) would let two claims coexist inside the lease window. | low | the flock is the primary guard; the lease is the secondary; the owner fencing is the third. ★B makes the third real. |
| R7 | **The delivery axis is not read by anything** until stage J. | low | deliberate — the axis is written first and read only after it has been proven in production |
| R8 | **The two clocks** (`fired_at` on the process clock, `delivered_at` on the DB clock) invite a future reader to difference them. | low | documented explicitly in §3.6 and in the migration's docstring |
| **R9** | **Two handlers have work no context can interrupt** (the drill's closing write, `rotateSnapshots`), so the settlement margin cannot be derived until they are fixed. | medium | stage D1 makes both cancellable; until then the margin is undefined, and the lease with it |
| **R10** | **Four of the six registered handlers cannot run today**, so the axis's per-handler guarantees are, for now, mostly theoretical. | medium | stated in §3.3.2 and in the matrix's "can it run today" column; the one handler that can run (`cascade_summary`) is bounded by construction; the three latent risks (`drill`, `snapshot`, `broadcast`) are the ones the guard stages address |
| **R11** | **`snapshot` has no registered deadline**, because no static bound exists and the handler has no producer. | low | the handler is unguarded today and will stay unguarded until a producer exists and D3's measurement is possible; the risk is **latent**, not live (R10) |
| **R12** | **The two open PRs #226 and #227 both modify `internal/scheduler/wake_expiration.go`**, so stage C's merge order is not free. | low | recorded in stage C; the merge order is a mechanical decision, not a design one |

---

## 8. Recommendation

**STAGE A IS IMPLEMENTED. THE LATER STAGES ARE NOT READY TO IMPLEMENT — AND ONE OF THEM CANNOT BE
UNTIL A MEASUREMENT IS TAKEN.**

Stage A (the additive migration) is approved, implemented and green in **PR #225**. It is not
reopened by this revision and its boundary is unchanged.

The later stages are **not** ready to implement today, for a reason that is new and that the
independent review forced into the open: **the lease duration cannot be derived until the handler
runtime has been measured.** The previous revision asserted a 30-minute lease and a 15-minute
deadline; neither number had a derivation, and the sentence that justified them was wrong in three
ways (§3.3.1). Stage D is therefore now a **measurement stage**, and **stage E is gated on it**.

The architecture is implementable as specified, and the eight §7.0 corrections are all honoured:

| Correction | How this specification satisfies it |
|---|---|
| C1 — `fired` is retirement, not delivery | the axis is independent; the five writers are enumerated in §1.1; no reader of `fired` changes |
| C2 — `delivery_state` is an independent axis | §3.1; the thirteen retirement readers keep reading `fired` |
| C3 — do not backfill `delivered` from `fired` | §3.2 — no backfill, and AT4 asserts it |
| C4 — an expired claim must be reclaimable | §3.4 — the corrected predicate; the superseded one is proved unreachable (Appendix A, E3) |
| C5 — preserve `target_time` eligibility | §3.7 — `target_time <= ?` is in the claim **predicate** *and* in the claim **statement** (§3.4), and AT8b asserts it directly on the statement |
| C6 — keep notification delivery separate | §3.1, §3.4 — the notification path is untouched; the two axes provably never contend |
| C7 — account for rollback to older binaries | ★A — `fired = 0` in the claim predicate closes the hazard **structurally**; the rollback policy is §3.8 |
| C8 — do not promise exactly-once external effects | §3.10 — no stage's gate is written as exactly-once; the criterion is per handler |

**Four corrections to the revised report** are made here, each on evidence:

1. **★A** — the claim predicate must include `fired = 0` (§3.4). Without it, C7's hazard is real
   and the correction is only cosmetic.
2. **★B** — `claimed_by` must be a fencing token, not observability (§3.5). The report's proposed
   guard was proved insufficient (Appendix A, E4).
3. **The `pending` state is dropped** (§3.2) — it is never written, so it is a dead value.
4. **The system path must not write `dispatched_at`** (§3.4) — that column is the notification audit
   trail, and the report's instruction would make it mean two things.

**Two additions the report did not make:**

5. **Stage D — the per-handler deadline.** The lease duration cannot be bounded without it. The
   previous revision of this document asserted a universal 15-minute deadline with no evidence
   base; the deadline is now **per-handler**, each value justified against the tree (§3.3.6), and
   the value that cannot be justified — `snapshot` — is **left unset** rather than guessed.
6. **Stages C — the two independent defect fixes** (W3's `fired_at` overwrite, W4's malformed-metadata
   abort). Neither is in the report, and W4 silently stops notification delivery. **Both are already
   implemented in open PRs** (#226 and #227, §4, stage C).

**Three additions this revision makes, on the evidence of the independent review:**

7. **The three dispatch outcomes are distinguishable** (§3.5.2, §3.9.1). The previous revision had
   one transition and a failure path that used the unfenced settlement, which produced a state the
   document itself said only a non-axis writer could produce. The **stranded** case — the handler
   never ran — was never classified at all.
8. **The settlement-failure policy is decided and its latency is stated** (§3.5.4).
9. **The snapshot's idempotency mechanism is specified completely** (§3.10.1), and assigned to a
   stage that implements it.

**The remaining risk is concentrated in one place:** the series does not close the duplicate-execution
window, and it does not claim to. It narrows it, makes it visible, bounds it with a lease, and moves
the residual onto the handlers — which is the only place it can be closed.

---

## Appendix A — the experiment

An isolated, disposable experiment (`/tmp/opencode/m1-spike`, outside the repository) was run against
a file-backed WAL database with the production DSN
(`_foreign_keys=1&_busy_timeout=5000`) and the same `go-sqlite3` driver, using the production
`scheduled_wakes` DDL and FTS shadow copied verbatim from `internal/core/schema.go:1359-1377` and
`internal/core/db.go:3948` and `:4019-4021`. It produced the evidence for AT1–AT6, AT8, AT9 and the W3/W4/W6
findings. It is not production code and it is not part of the repository.

**Results — 31 assertions, all green:**

| Group | Assertion | Result |
|---|---|---|
| **E1 — behaviour neutrality** | T1 exactly the five proposed columns are added | PASS |
| | T2 every pre-existing row is **byte-identical** after the migration | PASS |
| | T3 no backfill: every row has `delivery_state IS NULL`, **including** `fired = 1` rows | PASS |
| | T3b the other four new columns are `NULL` on every row | PASS |
| | T4 the FTS5 shadow is still consistent after the `ALTER TABLE` | PASS |
| | T1b/T1c/T1d a second run adds no column, no row; the sentinel is recorded once | PASS |
| | T5 a fresh DB has all five columns; the base DDL and the migration do not conflict | PASS |
| | T6a/T6b/T6c an injected failure is reported, the column set is **unchanged**, no sentinel | PASS |
| | T7a `DROP COLUMN` is **refused by SQLite** while the partial index exists | PASS |
| | T7b/T7c with the index dropped the rewrite proceeds; the FTS shadow survives it | PASS |
| **E2 — W3** | W3a the sweep's `UPDATE` still fires on an already-settled row | PASS |
| | W3b the sweep **overwrites** the settlement's `fired_at` | PASS |
| | W3c the sweep stamps its audit note over the settlement's metadata | PASS |
| **E3 — the lease predicate** | the **corrected** predicate admits `pending`, `claimed-live` and `claimed-expired` | PASS |
| | the **superseded report's** §7.3.1 predicate can **never** reclaim an expired claim | PASS |
| **E4 — stale-owner fencing** | E4a the expired claim **is** re-claimable | PASS |
| | E4b the proposed guard (`AND delivery_state='claimed'`) **does** admit the stale owner | PASS |
| | E4c the stale owner marked the row delivered while the current owner still held it | PASS |
| | E4d the **owner-fenced** guard (`AND claimed_by = ?`) rejects the stale owner | PASS |
| **E5 — W4** | E5a the notification claim **errors** on one malformed-`metadata` row | PASS (`malformed JSON`) |
| | E5b the 7-day sweep also errors on the same row | PASS |
| | E5c the 1-hour cron sweep also errors on the same row | PASS |
| | E5d the fold's predicate survives the same row (it probes `json_valid`) | PASS |

## Appendix B — the evidence index

| Claim | Where it is evidenced |
|---|---|
| `fired` has five writers, four of which never run a handler | §1.1; the table in §1.1 gives every file:line |
| `fired_at` has four different semantics | §1.2; `internal/core/wake_settlement.go:96,103`, `internal/core/wake_tools.go:646,909`, `internal/scheduler/wake_expiration.go:182,501` |
| `fired_by` has no production reader | §1.2; the only write is `internal/core/wake_tools.go:646` |
| `metadata.last_error` has no production reader | §1.2; the only write is `internal/core/wake_settlement.go:96-103` |
| `executeOne` can fail without running the handler | §1.1; W7; `internal/scheduler/scheduler.go:579-598` and `:508-518` |
| P1 has no retry budget | §1.1; `internal/core/wake_tools.go:119`, allowlisted at `internal/core/bare_pool_write_ratchet_test.go:658-661` |
| the settlement has no `fired = 0` guard, deliberately | §1.3; `internal/core/wake_settlement.go:78-83` |
| `MarkWakeFired` has exactly one production caller | §1.1; `internal/scheduler/scheduler.go:429` ← `:511` and `:521` |
| the scheduler is a flock singleton | §1.3 I6; `cmd/mpm-scheduler/main.go:118`, `internal/scheduler/scheduler.go:865` |
| `mpm-update` never opens the database | §3.8; verified against `cmd/mpm-update/` |
| the migration lands lazily, in the fatal transaction | §3.8; `internal/core/db.go:3117` and `:3135`; PR #225's `migration_scheduled_wakes_delivery_state.go` |
| `SafeMigrations` silences errors | §3.8; `internal/core/db.go:2967-2975` |
| the drill's timeout is **YAML-supplied and uncapped**, not 120 s | §3.3.1; `internal/core/drill.go:19`, `:56-63`, `internal/core/drill_loader.go:20-39` |
| `cascade_summary` writes **one log line and no row** | §3.10; `internal/scheduler/cascade_summary.go:42-59`; the row is written by the producer, `internal/core/wake_tools.go:105-119` |
| **`gc` is at-most-once** — the CAS is in `gc_tools.go`, not the CLI | §2.7, §3.10; `internal/core/gc_tools.go:81-92` |
| the `gc` dry-run is **not** read-only | §2.7, §3.10; `internal/core/gc_tools.go:114-122` vs the `DryRun` guards at `:141`, `:295`, `:308`, `:317` |
| **`mpm call gc_run` is not a callable tool name** | §3.3.3; `internal/core/tools/registry_list.go:527`, `:534`, `:540`; `internal/core/tools/call_routed.go:52-54`; `cmd/mpm/call.go:139-146` |
| **`mpm ops broadcast` does not accept `--target`** | §3.3.3; `cmd/mpm/ops_broadcast_cmds.go:49-72`, `:113-129` |
| **`critic_audit` is barred at the producer and at the dispatch gate** | §3.10; `internal/core/wake_tools.go:412-418` (the arming gate) and `internal/scheduler/scheduler.go:590` |
| **no producer arms `snapshot`, `drill`, `gc`, `broadcast` or `critic_audit`** | §3.3.2; every production `ScheduleWake` call site is enumerated there |
| broadcast is already content-deduplicated | §3.10; `internal/core/broadcast.go:122,231,244,518` |
| the drill creates a fresh run id per execution | §3.10; `internal/scheduler/drill_handler.go:74-75` |
| the snapshot label defaults to the epoch, **with one-second granularity** | §2.7, §3.10; `internal/scheduler/scheduler.go:914`, the backup at `:926`, the rotation at `:938` → `:1122-1160` |
| `sqlite3 .backup` **overwrites** its destination | §3.10.1; the second backup therefore rewrites the file and gives it a new mtime |
| the handler runtime is **already logged** as `duration_ms` on every exit path | §3.3.4; `internal/scheduler/scheduler.go:611-635` |
| two handlers have an **uncancellable tail** | §3.3.5; `internal/scheduler/drill_handler.go:126`, `:135-149`; `internal/core/db.go:1163`; `internal/scheduler/scheduler.go:1122-1160` |
| a hung handler **pins the whole scheduler** | §3.3.7; `internal/scheduler/scheduler.go:535`, `:778`, `:809`; `cmd/mpm-scheduler/main.go:224` |
| the daemon's default tick interval is 60 s | §3.5.4; `cmd/mpm-scheduler/main.go:48` |
| W3 — the sweep overwrites `fired_at` | Appendix A, E2; **and already fixed in open PR #226** |
| W4 — one malformed row aborts a dispatch statement | Appendix A, E5; **and already fixed in open PR #227** |
| W7 — the stranded wake: retired without the handler running | §2, W7; `internal/scheduler/scheduler.go:590-592`, `:511` |
| **the settlement's metadata write raises on malformed metadata** | §3.5.1; `json_set` raises on malformed JSON, so the write must go through `WakeMetadataJSON` — see **#227**'s change to `internal/core/wake_settlement.go` |
| W6 — the proposed settlement guard does not fence a stale owner | Appendix A, E4 |
| the additive migration is behaviour-neutral | Appendix A, E1 (T1–T6); **and implemented in PR #225** |
| schema rollback is forbidden, and the rule is enforced | Appendix A, E1 (T7a) |
| the corrected lease predicate reclaims; the superseded one cannot | Appendix A, E3 |
| the #218 authorizer barrier is the deterministic contention primitive | `internal/core/wake_resolve_contention_test.go` |
| **AT6's "abort the third ALTER TABLE" is not a mechanism SQLite offers** | §5; `ALTER TABLE … ADD COLUMN` fires no triggers — PR #225's `TestDeliveryStateMigration_AT6_FailedMigrationRollsBack` uses the sentinel-abort mechanism and says so |
---

## Appendix C — the disposition of the independent review

The review under disposition is
[#issuecomment-6641707069](https://github.com/flowbyte-com/mpm/pull/219#issuecomment-6641707069),
*Independent correctness review — PR #219 (scheduler delivery-state architecture)*, against the
previous revision of this document at head `c40f1221ab9798883e8f0de91c519fe25d7e36c3`. Its verdict
was ***CHANGES REQUESTED on the specification***, with **Stage A approved to proceed
independently**.

**Every finding was re-verified against the tree before it was disposed of.** Where the review's
finding was wrong, that is recorded too.

### C.1 The blocking findings (B1–B6)

| # | Finding | Disposition | What changed, and where |
|---|---|---|---|
| **B1** | **The claim statement omits `target_time <= ?`.** The *predicate* carries it; the statement that actually claims the row does not, so a future-dated wake was claimable by any caller of the statement — and §3.7 asserted the opposite. | **Accepted — confirmed against the tree.** | §3.4: the term is now in the statement. §3.6's clock table records that `target_time` is the one field read on **two** clocks (the process clock in `QueryDueWakes`, the DB clock in the claim), which the previous table denied. §5: **AT8b** asserts the term **directly on the statement** — the test that pins it, and the one that fails if the term is ever dropped again. |
| **B2** | **The handler-failure path uses the unfenced `MarkWakeFired`**, leaving `fired = 1` + `delivery_state = 'claimed'` — a state §3.5 itself says only a non-axis writer can produce. | **Accepted — confirmed.** `internal/scheduler/scheduler.go:511` calls `MarkFired`, and `MarkWakeFired` writes `fired = 1` without touching `delivery_state` (`internal/core/wake_settlement.go:84-110`). | §3.5.2: the failure path now uses the **fenced settlement**, so the row lands `fired = 1` + `delivery_state = 'delivered'` + `last_error`. §3.5.3 adds the **refusal** transition for the stranded case. §3.5.7 records the invariant. Stage F's stop condition is now *"stop if a row can be observed with `fired = 1` and `delivery_state = 'claimed'`"*. §5 adds **AT13** (the three outcomes are distinguishable) and **AT14** (the standing detector). |
| **B3** | **The settlement-failure recovery latency regresses** from ~60 s to up to 30 min, and the policy is not decided. | **Accepted — confirmed.** Today's tick interval is 60 s (`cmd/mpm-scheduler/main.go:48`) and a failed settlement leaves the wake pending; under the lease it would not be re-runnable for the lease. | §3.5.4: the policy is **decided** — a settlement that **errors** releases the claim, best-effort, fenced by `claimed_by = ?`. The latency is stated for both cases: **one tick interval (60 s)** normally, **up to the lease** if the release also fails. §5's **AT10** makes it executable: after `during_settlement` the claim is **released** and the wake is **immediately** re-dispatched, not left claimed. Decision 13 in §7.1. |
| **B4** | **The snapshot guard is assigned to stage I — the broadcast stage, which is optional — and no stage implements AT11's "snapshot ⇒ one rotation step".** | **Accepted — confirmed.** The previous stage I was "M4b: the broadcast", gated on the operator's decision 8 and explicitly optional; the snapshot had no stage at all, and its guard was *"a label derived from the wake id … or an explicit `wake_id` guard. Must be decided before the stage."* | §3.10.1: the mechanism is now specified **completely** — the wake-id guard row whose insert **is** the claim; the filename derived from the wake id (and `metadata.label` no longer an override); the backup written to a temp path and atomically renamed; the integrity check against the temp path; the rotation inside the guarded region. Stage I is split into **I-a** (the snapshot guard, with its own prerequisites, files, invariants and gate) and **I-b** (the broadcast, still optional). Stage K's prerequisite is now "the guards that exist", so a declined I-b narrows stage K's assertion explicitly rather than silently. |
| **B5** | **The 15-minute deadline has no evidence, and its stated basis is wrong in three places.** | **Accepted — confirmed, and then some.** The three errors: (i) `cascade_summary` is **not** unbounded — it writes one log line and returns `nil`; (ii) two of the "five" cannot run at all; (iii) the drill's bound is the **YAML's** `timeout_secs`, which is **uncapped**, not the 120 s default. | §3.3 is **re-derived**. The two constants are **withdrawn**. The deadline becomes a **per-handler registry** with each value justified against the tree (§3.3.6); `snapshot` gets **no** deadline because no static bound exists. §3.3.4 identifies the measurement instrument that **already exists** (`duration_ms` on every exit path of `executeOne`, `internal/scheduler/scheduler.go:611-635`). §3.3.5 identifies the **uncancellable tail** that blocks any derivation. Stage D becomes a **measurement** stage with D3 as an explicit **gate on stage E**. Decision 3 in §7.1 is now **open** with a derivation procedure rather than a number. |
| **B6** | **§3.9 condition 1's "permanently lost" contradicts §1.1, I3 and the code, and the stranded case is never classified.** | **Accepted — confirmed.** `TestWakeDelivery_ARestartBeforeSettlementLeavesTheWakePending` asserts the wake *"must still be pending"* (`internal/scheduler/wake_settlement_test.go:375-397`); invariant **I3** says the same. The wake is **re-run**, not lost. | §3.9 condition 1 now says **"silent duplicate execution", not "loss"**, and states what is actually lost: the *knowledge* that the handler ran, and the audit trail. §2 adds **W7** as a first-class window. §3.9.1 classifies the **stranded** case and gives it its own transition. §3.10 lists it as an achievable property ("a handler that never ran is never mistaken for a delivery"). §5's **AT13** makes it executable. Stage K's "residual" note forbids reintroducing the "lost" error. |

### C.2 The recommended changes (R1–R10)

| # | Recommendation | Disposition | Where |
|---|---|---|---|
| **R1** | State that the settlement's **absence of a lease test is deliberate** — the fencing token subsumes it. | **Accepted.** | §3.5.1: the lease governs **reclaimability**, the token governs **settleability**; a reclaim necessarily rewrites `claimed_by`, and a lease test would *reject* a legitimate late settlement and turn a completed delivery into a redelivery. Restated in §3.9 condition 5. |
| **R2** | State the lease's **process-death-only precondition**. | **Accepted.** | §3.3.7: `Tick` blocks on `wg.Wait()` and the daemon's context has **no deadline**, so a hung handler pins the whole scheduler. The **Lease invariant** is stated. The `executeOne` docstring's claim of an existing "context timeout" is called out as misleading (also **O1**). |
| **R3** | Correct the `gc` CAS citation to `internal/core/gc_tools.go:81-92`, and note the dry-run is **not** read-only (three hard-delete prunes at `:114-122`). | **Accepted.** | §2.7 and §3.10 both corrected. The prunes (`PruneAuditLog(30)`, `PruneHandoffs(90)`, `PruneCascadeOutbox(30)`) run on a successful claim, before the `DryRun` guards at `:141`. |
| **R4** | Correct the `cascade_summary` matrix row — it writes a **log line**, not a row. | **Accepted.** | §3.10: the handler writes **no row**; the `cascade_summary` wake row is written by the **producer** (`ScheduleCascadeSummaryWake`, `internal/core/wake_tools.go:105-119`), and the handler only emits one `slog.Info` line and returns `nil` (`internal/scheduler/cascade_summary.go:42-59`). A duplicate delivery therefore produces a duplicate **forensic line** and **no** duplicate row. AT11 gains a `cascade_summary` sub-test accordingly. |
| **R5** | Correct the `critic_audit` citation from `internal/core/wake_tools.go:247` to the producer gate. | **Accepted.** | §3.10: `:247` is only the predicate `WakeKindBlocked`; the **arming gate** is `internal/core/wake_tools.go:412-418`, inside `ScheduleWake`. The dispatch gate (`internal/scheduler/scheduler.go:590`) is also cited. §1.4 now names **#118**, the PR that introduced the blocked-kind mechanism, so the row has a provenance. |
| **R6** | A deterministic snapshot label makes the **rotation** idempotent but **not** the handler — the second `.backup` still overwrites, and a wake-id filename loses the timestamp. | **Accepted, and strengthened.** | §3.10.1: `sqlite3 .backup` **overwrites** its destination, so a same-second duplicate rewrites the file with a **later** copy and a **new mtime**, which **reorders** the rotation and can delete a snapshot the operator wanted. The five-part mechanism includes the guard row, the wake-derived filename, the temp-path + atomic rename, the integrity check against the temp path, and the rotation inside the guarded region. §2.7's "a *different* file" wording is corrected (also **O5**). |
| **R7** | The drill's `timeout_secs` is **YAML-controlled**, and `WithTx` writes are **uncancellable**. | **Accepted.** | §3.3.1 (the YAML wins and is uncapped) and §3.3.5 (`WithTx` takes **no context**, `internal/core/db.go:1163`, so the drill's closing `UPDATE drill_runs` cannot be cancelled). Stage **D1** makes both tails cancellable, and §3.3.5 explains why **no margin can be derived** until it does. |
| **R8** | Name `idx_sw_delivery_claim` in the runbook's **do-not-touch list** — dropping it re-enables schema rollback. | **Accepted.** | §3.8's runbook entry (1): the index is the **enforcement mechanism** for the forward-only rule, not an optimisation — its predicate matches **zero rows** in stage A, so it reads as dead, and removing it is the only way to permit the forbidden rewrite. The same rule is extended to the indexes stage H and stage I-a add. |
| **R9** | Give the rollback runbook a **pre-rollback count of `claimed` rows**. | **Accepted.** | §3.8's runbook entry (2): the exact `SELECT COUNT(*) … WHERE delivery_state = 'claimed'`, recorded **before** the rollback, as the operator's measure of the re-dispatch exposure. |
| **R10** | Read the existing `duration_ms` log as the **pre-flight measurement**, and replace stage D's post-hoc stop condition. | **Accepted.** | §3.3.4: the log line already exists on all three exit paths — no new instrumentation is needed to read it; what stage D fixes is the **protocol** (a stated observation window, per-kind maxima with observation counts, a recorded result table). Stage D's stop condition is no longer a trip-wire on an unproven number; **D3 gates stage E** instead. |

### C.3 The optional refinements (O1–O5)

| # | Refinement | Disposition | Where |
|---|---|---|---|
| **O1** | `executeOne`'s comment claims a "context timeout" that does not exist. | **Accepted.** | §3.3.7 quotes it and says it describes the mechanism stage D *introduces*. Stage D's file list includes the comment correction. |
| **O2** | `HandlerFunc`'s comment names a non-existent "per-call timeout". | **Accepted.** | §3.3.7 and stage D: `internal/scheduler/scheduler.go:148-149` says *"SIGTERM, shutdown deadline, or per-call timeout"* — there is no per-call timeout today. The comment is corrected with the deadline stage. |
| **O3** | `updateDrillRunError` uses a bare `dm.SQLDB().Exec` with **no retry budget**. | **Accepted.** | Stage D1's file list includes `internal/scheduler/drill_handler.go`: the error-stamp write is a bare pool write with no budget, so a busy writer loses the error state on the very path that records a failure. |
| **O4** | `rotateSnapshots` ignores `ctx`. | **Accepted.** | §3.3.5 identifies it as one of the two uncancellable tails, and stage **D1** changes it to take the ctx and check it between the directory read and the unlinks. |
| **O5** | §2.7's same-second duplicate wording. | **Accepted.** | §2.7: the epoch label has **one-second granularity**, so a duplicate in the same second writes the **same** file and **overwrites** it (with a new mtime, reordering the rotation), and a duplicate in a later second writes a **different** file and advances the rotation. Both are bad; the first is the one a filename-only fix does not prevent. |

### C.4 Findings from this revision that the review did not raise

These were found while verifying the review's findings, and they are recorded here so that the next
review does not have to rediscover them.

| # | Finding | Evidence | Where it is now handled |
|---|---|---|---|
| **N1** | **§1.4's PR table is wrong: #212 is not scheduler work.** It is *"test(update): the development-source fixture cleans up and cannot copy itself"* — the updater's fixture suite, touching only `internal/update/`. The wake-ownership isolation groundwork is **#215 and #216**. | `gh pr view 212 --json files` → seven files, all under `internal/update/` | §1.4: the entry is corrected, **#118** is added (the blocked-kind mechanism, which §3.10's `critic_audit` row depends on), and the #200–#205 issue-178 write-path series is named for what it is. |
| **N2** | **Stage C is already partly implemented, in two open PRs.** **#226** implements C1 (the sweeps' `fired = 0` guard); **#227** implements C2 (the `json_valid` probes) and extracts the shared kind-matcher. **C3 is implemented nowhere.** | `gh pr diff 226` — `wake_expiration.go` +71/−10; `gh pr diff 227` — `wake_tools.go` +110/−26, `dispatch.go`, `scheduler.go`, `wake_settlement.go` | §4, stage C: the status is recorded, the two PRs are named with their heads, and the stage's file list, tests and stop condition are rewritten to match. |
| **N3** | **#226 and #227 both modify `internal/scheduler/wake_expiration.go`**, so they are not independently mergeable, and stage C's merge order is not free. | both diffs touch that file | §4, stage C; risk **R12**. |
| **N4** | **#226 changes the sweep's read-back assertion** from "0 rows is an error" to "0 rows is a skip", because the `fired = 0` guard turns a lost race into a benign no-op and aborting the batch would roll back every candidate the sweep had already retired. The previous stage C's stop condition was therefore **wrong as written**. | `gh pr diff 226` — the `RowsAffected == 0` branch changes from `return retired, fmt.Errorf(...)` to `continue` | §4, stage C's stop condition is rewritten. |
| **N5** | **AT6's mechanism does not exist.** The previous §5 specified *"a trigger that aborts the **third** `ALTER TABLE`"*. **`ALTER TABLE … ADD COLUMN` fires no triggers**, so no such trigger can be installed. | PR #225's own `TestDeliveryStateMigration_AT6_FailedMigrationRollsBack` docstring says so explicitly: *"a stronger assertion than aborting the third ALTER TABLE (a mechanism SQLite does not offer: ALTER TABLE ADD COLUMN fires no triggers)"* | §5: AT6's mechanism is corrected to the **sentinel-abort** trigger that Stage A's implementation actually uses — which is also a *stronger* assertion, because the rollback must then undo real DDL. |
| **N6** | **Four of the six registered handlers have no producer in this tree**, so the axis's per-handler guarantees are, today, mostly theoretical. | every production `ScheduleWake` call site enumerated in §3.3.2 | §3.3.2; risk **R10**; the matrix's new "can it run today" column. |
| **N7** | **`gc` and `broadcast` cannot run at all** — their invocations are broken, not slow. `mpm call gc_run` is not a callable tool name, and `mpm ops broadcast` does not accept `--target`. | §3.3.3, with the full call chain | §3.3.3; §3.10's matrix; decision **8b** in §7.1 — **fixing the `gc` invocation creates a GC that does not happen today**, and its `dry_run` is not read-only. The previous revision asked about the broadcast only. |
| **N8** | **The axis's own vocabulary could not express the stranded case**, and the previous §3.5 asserted that "a refused settlement stays pending and will be re-run" — which is false for a fenced settlement (a refusal means the row is terminal or owned by another claimant). | §3.5.5 | §3.5.5: the refusal's three meanings are enumerated; §3.5.3 adds the refusal transition. |
| **N9** | **The lease cannot cover a hung handler**, because a hung handler prevents the tick from completing at all — so the lease's precondition is process death, not a stalled handler. | `internal/scheduler/scheduler.go:535` (`wg.Wait()`), `:778`, `:809`; `cmd/mpm-scheduler/main.go:224` | §3.3.7's **Lease invariant**. |
| **N10** | **The new transitions' metadata write would have inherited W4's defect.** `json_set` **raises** on malformed JSON, so the unguarded `COALESCE(metadata,'{}')` form cannot settle a wake whose `metadata` is corrupt — the settlement errors, the wake stays `fired = 0`, and its handler runs again on the next tick, **forever**. The existing settlement has this bug today; the new transitions must not copy it. | #227's diff on `internal/core/wake_settlement.go` replaces `COALESCE(metadata,'{}')` with `WakeMetadataJSON` and says so in its own comment | §3.5.1: the settlement and the refusal transition both use `WakeMetadataJSON` (`CASE WHEN json_valid(metadata) THEN metadata ELSE '{}' END`), and stage F's prerequisites now name **#227** as a dependency, because `WakeMetadataJSON` is defined by it. |

### C.5 What this revision did **not** do

- **Stage A was not reopened, redesigned or extended.** Its boundary — five nullable `ADD COLUMN`s,
  one partial index, the sentinel, the fatal-migration transaction, no backfill, base DDL unchanged,
  schema rollback forbidden — is unchanged, and PR #225 is unaffected by this document.
- **No later stage was implemented.** This document specifies stages C–K; it does not contain their
  code.
- **The three corrections the review endorsed were carried forward unchanged**: ★A (`fired = 0` in
  the claim predicate), ★B (`claimed_by` as a fencing token), and the C8 discipline (no acceptance
  criterion written as "exactly once").
- **The independent review's own verdict on Stage A** — approved to proceed independently — is
  recorded, not re-argued.
