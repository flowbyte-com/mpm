# Issue #178 — the scheduler delivery-state redesign: an implementation-ready specification

**Date:** 2026-11-04
**Baseline:** `main` @ `f8d4b95aefe60a0880409a8905ce2c4d92d22c00` (merge of PR #218)
**Authority:** [`docs/dev/issue-178-corrections-phase-3-2026-11-03.md`](issue-178-corrections-phase-3-2026-11-03.md) §7.0, and the
[revised engineering report](https://github.com/flowbyte-com/mpm/issues/178#issuecomment-6606813378) it links.

**Status of this document:** architecture and acceptance specification. No production migration is
implemented here. The §7.1–§7.6 proposal remains superseded and must not be implemented as written.

---

## 0. What this document settles

§7.0 states eight corrections (C1–C8) that the superseded M1–M6 proposal violates, and delegates the
actual design to the revised engineering report. The report in turn leaves five decisions open and
two of its own instructions are unsafe as written. This document:

1. re-establishes current reality at `f8d4b95a`, including the corrections merged through #218;
2. answers each of §7.0's ten named conditions with evidence, not assertion;
3. **settles** the five open decisions (nullability, lease duration, lease clock authority, the
   semantics of `claimed_by`, and the rollback policy) with a stated rationale and a named invariant;
4. corrects four instructions in the revised report that cannot be implemented as written;
5. produces a stage sequence with acceptance gates, stop conditions and rollback conditions;
6. produces twelve deterministic acceptance-test specifications.

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
`wake_settlement.go:78-83` argues that refusing to settle an already-fired row would turn a
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

### 1.4 What #212–#218 already closed

These are **completed work**. This specification does not propose re-implementing them.

| PR | What it closed |
|---|---|
| #212 | the wake-ownership isolation groundwork |
| #213 | cascade materialisation idempotency (the transition, the one-theory-per-intent invariant, the read-back assertions) |
| #214 | the bare-write ratchet now sees the `ExecContext` shape |
| **#215** | **the agent's fold can no longer retire the scheduler's wakes** — `CheckPendingWakes` filters to `AgentOwnedWakeKinds` = `{notification, cascade, cron}` in every branch, including `kinds:["*"]` |
| **#216** | **the agent can no longer arm a scheduler-owned kind** — `handleScheduleWake` refuses any kind for which `WakeKindAgentOwned` is false, at the agent-facing boundary; `ResolveWake` now uses the same shared predicate |
| #217 | the release-identity probe's SIGPIPE flake |
| **#218** | **the resolver survives concurrent resolution** — the read-then-write pair moved inside `dm.WithTx`, which owns the stale-snapshot class |

The revised report's stage **B** (B1: `kinds:["*"]` must not retire system wakes; B2: `ScheduleWake`
validates the kind against an allowlist) is therefore **closed**. The report's stage B is merged.

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
  §7.3.1 predicate such a row would be re-claimable. **With `fired = 0` present in the predicate
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

### 2.7 What is **not** a window

| Candidate | Reality at the baseline |
|---|---|
| **duplicate broadcast** | `BroadcastMemory` deduplicates its fan-out by content: `shared.bcast_event_wakes.wake_id` is the PRIMARY KEY and the insert is `INSERT OR IGNORE` (`internal/core/broadcast.go:122`, `:231`, `:244`, `:518`). A repeat affects 0 rows → `status = "deduped"`. Broadcast is **already idempotent** for this failure mode. (Separately: `BroadcastHandler` cannot succeed today — it passes `--target`, which the CLI does not accept, and the wake carries no memory id. §7.0 C8 / the report's §3(c).) |
| **duplicate cascade materialisation** | closed by #213: the transition is idempotent, the intent yields one theory, and the read-back assertions report a transition that did not land. |
| **duplicate cascade wake scheduling** | `scheduleCascadeWake` (`internal/core/cascade_materializer.go:918`) flips `wake_scheduled = 1` **after** `ScheduleWake` succeeds and reports a failure to the caller when the flip does not land; the reconcile pass (`internal/core/cascade_wake_reconcile.go`) recovers the lost wake and deletes the orphan. The residual is bounded by the per-check delivery cap (`MaxCascadeWakePerCheck = 3`). |
| **duplicate `gc`** | `gc` takes a 24-hour compare-and-swap on `system_config.last_gc_at` **before** its work (`cmd/mpm/handlers_gc.go:174-190`). It is **at-most-once**; its residual risk is a **lost** GC, not a duplicated one. |
| **duplicate `snapshot`** | **not** idempotent: the label defaults to `time.Now().Unix()` (`internal/scheduler/scheduler.go:914`), so a duplicate delivery writes a *different* file and then advances `rotateSnapshots(dir, 7)` (`:938`), which can delete a snapshot the operator wanted. |

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

### 3.3 The lease — settled

**Duration.** `DeliveryLeaseSeconds = 1800` (30 minutes).

**This requires a new prerequisite the report did not state: a bounded handler runtime.** The lease
must exceed the maximum handler runtime, or a long handler is re-claimed and run concurrently. Only
one of the six system handlers has a bound today — the drill, at
`core.DefaultDrillTimeout("claude_code") = 120 s` (`internal/core/drill.go:56-63`). The other five
(`snapshot`, `critic_audit`, `gc`, `broadcast`, `cascade_summary`) run until the dispatch context is
cancelled, which is the daemon's shutdown context, not a per-handler deadline.

Therefore stage D introduces:

| Constant | Value | Meaning |
|---|---|---|
| `SystemHandlerDeadline` | `900 s` (15 min) | a per-handler deadline applied at the dispatch site, to every system handler |
| `SettlementMargin` | `300 s` (5 min) | the window in which a handler that ran to its deadline can still settle inside its lease |
| `DeliveryLeaseSeconds` | `1800 s` (30 min) | `= SystemHandlerDeadline + SettlementMargin` |

**Named invariant, unit-tested so the constants cannot drift:**

```
DeliveryLeaseSeconds >= SystemHandlerDeadline + SettlementMargin
```

**Accepted consequences, stated plainly:**

- A crashed system-kind claim is recovered after **up to 30 minutes**. For a daily `snapshot` or an
  hourly `critic_audit` this is immaterial. If the operator wants faster recovery for one kind, the
  lever is a per-kind lease — a later refinement, not a first-step requirement.
- A handler that runs longer than 15 minutes is now **cancelled**, where before it ran to completion.
  This is a behaviour change and it is why stage D is separate, and why it must ship before the lease.
- The drill's own 120 s timeout remains the binding constraint inside the 15-minute deadline.

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
3. It matches the substrate's own convention (`created_at DEFAULT`, `gc`'s CAS at
   `cmd/mpm/handlers_gc.go:182`).

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
   AND (
         delivery_state IS NULL
      OR (delivery_state = 'claimed'
          AND lease_expires_at IS NOT NULL
          AND lease_expires_at < CAST(strftime('%s','now') AS INTEGER))
        )
```

`RowsAffected == 1` ⇒ this caller owns the wake. `RowsAffected == 0` ⇒ somebody else owns it, or it
was retired; the caller skips it.

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

### 3.5 The settlement — settled

**★B. `claimed_by` is a fencing token.**

The report recommends observability-only and leaves the column's semantics undefined ("`claimed_by`'s
semantics are undefined in §7.2 — that has to be settled before it is written"). Settled as follows:

- **Value:** a fresh opaque token per claim: `<instance-uuid>:<claim-uuid>`. The instance uuid is
  generated once per daemon start; the claim uuid is fresh per claim. The string is unique per claim,
  so it fences exactly one claim, and the instance prefix keeps it attributable in a log.
- **Written** by the claim statement.
- **Checked** by the settlement statement.

**The settlement:**

```sql
UPDATE scheduled_wakes
   SET delivery_state = 'delivered',
       delivered_at   = CAST(strftime('%s','now') AS INTEGER),
       fired          = 1,
       fired_at       = ?,
       metadata       = json_set(COALESCE(metadata,'{}'), '$.last_error', ?)
 WHERE id = ?
   AND fired = 0
   AND delivery_state = 'claimed'
   AND claimed_by = ?
```

- `AND fired = 0` — new relative to today's settlement, which has no guard. The current settlement's
  docstring argues against a guard ("refusing to settle because the row already says `fired = 1`
  would turn a duplicate settlement into a lost one"). That argument is about *idempotence*; the new
  settlement is not a duplicate settlement but a **fenced transition**, and its guard is the point.
- `AND delivery_state = 'claimed'` — the transition may only follow a claim. Without it a settlement
  could move a row from `NULL` straight to `'delivered'`, and the wake would never be re-delivered.
- `AND claimed_by = ?` — the fencing term. Proved necessary in Appendix A, E4.

**A refused settlement is not a silent no-op.** The settlement returns a distinguishable
"not claimed" outcome, and `Tick` **reports** it — the same way it reports a failed settlement today
(I4). A wake whose settlement is refused stays pending and will be re-run; that is the correct
at-least-once behaviour, and it must be visible.

**`MarkWakeFired` is not modified.** The existing exported function
(`internal/core/wake_settlement.go:84`) keeps its contract — it settles any row and does not touch
`delivery_state`. A **new** function carries the fenced transition, and only the scheduler's
dispatch path calls it. This keeps the mixed-version path working: an older binary's settlement still
lands `fired = 1` (and leaves `delivery_state` untouched), which is exactly what ★A's `fired = 0`
guard is designed to absorb.

**Rows retired by another path keep `delivery_state` untouched.** A fold-retired row, a
resolver-retired row and a sweep-retired row all have `fired = 1`, `delivery_state IS NULL` and
`delivered_at IS NULL`. They were **retired, not delivered**.

**A reader must never infer "still pending" from the axis.** A row with `delivery_state = 'claimed'`
and `fired = 1` is a row that a non-axis writer retired (for example an older binary's settlement
during a rollback window). It is excluded from the claim by `fired = 0`, and no reader may assert
"claimed ⇒ pending".

### 3.6 Clock consistency — settled

Summary of the whole clock story, in one place:

| Field | Clock | Written by | Compared with |
|---|---|---|---|
| `target_time` | process (`time.Now()`) | the producer | the process clock (`target_time <= ?`) — **unchanged** |
| `fired_at` | process | five retirement paths | never compared to a DB-clock field |
| `created_at` | DB (`DEFAULT`) | SQLite | not compared |
| `claimed_at` | **DB** | the claim | not compared |
| `lease_expires_at` | **DB** | the claim | the **DB** clock, in the same statement |
| `delivered_at` | **DB** | the settlement | not compared |

The scheduler's claim statement uses the **DB clock for both** the eligibility check
(`target_time <= CAST(strftime('%s','now') AS INTEGER)`) and the lease check, so the statement has
one clock and one authority. The notification path keeps the process clock — unchanged, because
`target_time` is authored on the process clock and the two producers live on the same host.

### 3.7 Future-dated eligibility — settled

`target_time <= ?` **survives** in the claim predicate (§7.0 C5), evaluated on the DB clock inside
the claim statement. `QueryDueWakes` is unchanged and continues to use the process clock for its own
`target_time <= ?` filter; the claim re-checks eligibility on the DB clock, so a wake that becomes due
between the two is claimed on the next tick. No wake is ever claimed before its `target_time`.

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

### 3.9 The ten conditions, answered

| # | Condition | Answer |
|---|---|---|
| 1 | **Crash after claim, before execution** | The row is `delivery_state = 'claimed'` with a lease. Under the corrected predicate it is **reclaimable** once the lease expires (§3.4). Today it is **permanently lost**: `fired = 0` keeps it in `QueryDueWakes`, the scheduler runs the handler, but with no claim there is no state to distinguish "never run" from "ran and died". The lease narrows this window from *permanent loss* to *at most `DeliveryLeaseSeconds`*. **Achievable.** |
| 2 | **Crash during handler execution** | Indistinguishable from (1) at the row level — the row is `claimed` with a live lease. Recovery is the same: reclaim after the lease. The external effect may or may not have happened; only per-handler idempotency can bound that. **Partly achievable** (recovery), **not** achievable (effect-exactly-once). |
| 3 | **Crash after external effect, before settlement** | The row is `claimed`. After the lease it is reclaimed and the handler runs again — **the duplicate-execution window W1**, narrowed but not closed. The delivery axis makes the duplicate *visible* (`delivery_state` returns to `'claimed'` with a fresh `claimed_by`), which the current schema cannot do. The *effect* duplication requires per-handler idempotency (§3.10). **Partly achievable.** |
| 4 | **Lease expiration and reclaim** | The corrected predicate admits `delivery_state = 'claimed' AND lease_expires_at < now`. The superseded §7.3.1 predicate can **never** admit it — proved in Appendix A, E3, and it is exactly the §7.0 C4 "permanent-loss window". **Achievable.** |
| 5 | **Stale owners attempting settlement** | Under the singleton, unreachable in production: the scheduler holds an exclusive `flock` for its lifetime and `MarkWakeFired` has exactly one production caller. But the proposed guard does not fence a stale owner — proved in Appendix A, E4. ★B adds the `claimed_by` term so the axis is correct **by construction**, not by an invariant that lives in another process. **Achievable, and the guard is one predicate term.** |
| 6 | **Clock consistency** | The lease is evaluated entirely on the DB clock, in the same statement that writes it (§3.3). `fired_at` stays on the process clock and is never compared against a DB-clock field (§3.6). NTP-step exposure is identical for both clocks and is not claimed to be eliminated. **Settled.** |
| 7 | **Future-dated wake eligibility** | `target_time <= ?` is preserved in the claim predicate, evaluated on the DB clock inside the claim statement (§3.7). **Settled.** |
| 8 | **Rollback to an older binary** | Binary rollback is safe. The C7 hazard is closed **structurally** by ★A (`fired = 0` in the claim predicate), not by policy. Schema rollback is forbidden, and the rule is enforced by SQLite (§3.2). `mpm-update` never opens the database, so the migration lands lazily. **Settled.** |
| 9 | **Drill and broadcast non-idempotence** | `drill` is the **live** risk: it creates a fresh `uuid.NewString()` run id **and** session id per execution (`internal/scheduler/drill_handler.go:74`) and re-runs the whole harness. `broadcast` is already content-deduplicated (`shared.bcast_event_wakes.wake_id` is the PRIMARY KEY, `INSERT OR IGNORE`) — and, separately, `BroadcastHandler` cannot succeed today (it passes `--target`, which the CLI rejects, and the wake carries no memory id). The report's priority order is therefore **inverted**: drill first, broadcast second. **Achievable per handler; the handler, not the axis, is the unit of idempotency.** |
| 10 | **Duplicate cascade materialisation or dispatch** | Materialisation is closed by #213. Scheduling is bounded by the `wake_scheduled` flag, the read-back assertion and the reconcile pass, with the per-check delivery cap (`MaxCascadeWakePerCheck = 3`) as the bound. The residual is a *bounded* duplicate delivery signal, not an unbounded one. The delivery axis does not change this. **Already bounded; not a target of this redesign.** |

### 3.10 What is achievable, and what requires handler-specific idempotency

**Achievable by the delivery axis:**

- a crashed claim is **recovered** rather than lost;
- a duplicate delivery is **visible** as a distinct state, with the owner and the attempt recorded;
- exactly **one** claimant wins a contested claim;
- a **stale** claimant cannot settle;
- a wake that a **non-axis** path retired is never mistaken for a delivery;
- a mixed-version window introduces **no new** duplicate-execution window.

**Not achievable by the delivery axis, at any level of effort:**

- **exactly-once external execution.** A settlement records that a handler ran; it cannot make a
  broadcast to another agent's process or a spawned drill harness happen once. (§7.0 C8.) This
  specification makes **no** such claim, and no stage's acceptance criterion may be written as one.

**Requires handler-specific idempotency — the per-handler matrix:**

| Handler | Duplicate effect today | Idempotency already present | Stage | The guard |
|---|---|---|---|---|
| `drill` | **a second `drill_runs` row with a fresh run id and session id, and a second full harness run** (`claude_code` spawns Claude Code) | none | H | `ALTER TABLE drill_runs ADD COLUMN wake_id` + a unique index; the handler's insert **is** the claim. The run id stays a uuid — keying `drill_runs.id` by the wake id would change the id that `tool_invocations.session_id` and the drill report reference. |
| `snapshot` | a **different** backup file (the label defaults to `time.Now().Unix()`) and an advanced `rotateSnapshots(dir, 7)` that can delete a snapshot the operator wanted | none | I | a label derived from the **wake id** rather than the epoch, so a duplicate delivery writes the same file; or an explicit `wake_id` guard. Must be decided before the stage. |
| `gc` | a second GC pass | **at-most-once**: a 24-hour CAS on `system_config.last_gc_at` **taken before** the work (`cmd/mpm/handlers_gc.go:174-190`) | — | none needed; the residual risk is a **lost** GC, not a duplicated one |
| `broadcast` | a second fan-out | **already content-deduplicated** (`shared.bcast_event_wakes.wake_id` PRIMARY KEY + `INSERT OR IGNORE`) | I | the content dedupe is sufficient for the crash window; a stricter **wake-id** key is a second, independent key. `BroadcastHandler` must also be fixed — and fixing it **creates** an external effect that does not exist today, so it is an operator decision (§7). |
| `cascade_summary` | a second audit line | none needed — the handler writes a summary row and schedules nothing | — | accepted: a duplicate delivery produces a duplicate forensic line |
| `critic_audit` | a second critic cycle | `critic_audit` is **blocked** at the producer (`internal/core/wake_tools.go:247`) and at the dispatch gate (`internal/scheduler/scheduler.go:590`); no wake of that kind is reachable | — | none needed |

**The unit of idempotency is the handler, not the axis.** The acceptance criterion for the whole
series is therefore **per handler** — "one `drill_runs` row", "one CAS claim", "one rotation step" —
and never "exactly one effect in total".

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

| | |
|---|---|
| **Prerequisites** | none |
| **Files** | `internal/scheduler/wake_expiration.go` (both sweeps); `internal/scheduler/dispatch.go` (the notification claim); `internal/core/wake_tools.go` (P1's INSERT); `internal/core/wake_tools.go` (the shared `json_valid` kind-matcher, if extracted) |
| **Changes** | **C1** — add `AND fired = 0` to both sweep UPDATEs, closing W3 (the `fired_at` overwrite). **C2** — probe `json_valid(metadata)` in the notification claim's and both sweeps' kind predicates, closing W4. **C3** — route P1's INSERT through `ExecTracked` with budget 5, closing W2's lost-summary case; update the bare-write allowlist accordingly (the entry at `internal/core/bare_pool_write_ratchet_test.go:658` becomes stale and must be removed, and the ratchet test must stay green). |
| **Observable behaviour change** | a malformed-`metadata` row no longer stops notification delivery or the sweeps; a sweep no longer overwrites a concurrent settlement's `fired_at`; the cascade summary survives a busy writer |
| **Regression tests** | a characterisation test per fix: the sweep must not overwrite `fired_at` (the E2 experiment, promoted to a test); the claim and the sweeps must survive a malformed row (the E5 experiment); P1 must survive a busy writer |
| **Compatibility** | no schema change; old binaries are unaffected |
| **Rollback** | fully revertible — no schema, no state |
| **Stop condition** | stop if the sweep's read-back assertion starts rejecting a legitimately-retired row, or if the ratchet baseline changes in any way other than the expected removal |
| **Gate** | the three characterisation tests green; the existing sweep and dispatch suites green unchanged; `mpm-lint --gate` PASS |

> **Why C before A?** These are independent of the delivery axis and are worth shipping on their own
> merit: W4 silently stops notification delivery, and W3 destroys the one piece of forensic evidence a
> post-mortem has. Neither is urgent in the sense of being a data-loss emergency, and both are
> smaller than stage A. **If the operator prefers to land the migration first, the order A → C is also
> correct** — C does not depend on A, and A does not depend on C.

### Stage D — the per-handler deadline

| | |
|---|---|
| **Prerequisites** | none |
| **Files** | `internal/scheduler/scheduler.go` (`executeOne` / `Tick`) |
| **Change** | apply `SystemHandlerDeadline` (15 min) to every system handler's context at the dispatch site |
| **DB invariants** | none |
| **Observable behaviour change** | a handler that runs longer than 15 minutes is cancelled and the wake settles as failed with `last_error` — instead of running unbounded |
| **Regression tests** | a handler that blocks past the deadline is cancelled and the wake settles terminal; a handler that completes inside the deadline is unaffected; the drill's own 120 s timeout is unaffected |
| **Compatibility** | behaviour change on the dispatch path; no schema change |
| **Rollback** | fully revertible |
| **Stop condition** | stop if any production handler is observed to exceed the deadline in a live tick |
| **Gate** | the deadline tests green; the existing scheduler suite green unchanged |

> **Why D before E:** the lease duration is a function of the maximum handler runtime. Without a
> bounded runtime the lease cannot be bounded, and a lease shorter than a legitimate handler is a
> duplicate-execution generator. D is the prerequisite that makes E's lease duration a defensible
> number.

### Stage E — M3a: the claim

| | |
|---|---|
| **Prerequisites** | A (the columns exist), D (the deadline bounds the runtime) |
| **Files** | `internal/core/wake_tools.go` or a new `internal/core/wake_claim.go` (the claim function and the shared predicate builder); `internal/scheduler/scheduler.go` (`Tick` — claim before running); `internal/scheduler/system_handlers.go` (the claim token's instance uuid) |
| **DB invariants** | at most one live claim per wake; `lease_expires_at > claimed_at`; `delivery_state ∈ {NULL, 'claimed'}` for a wake that is not settled |
| **Observable behaviour change** | the scheduler claims a wake before running its handler; a crashed claim is recovered after the lease instead of being lost; a contested claim has exactly one winner |
| **Regression tests** | AT8, AT9a; a claim that is refused leaves the wake untouched; a reclaimed claim carries a **new** `claimed_by`; the notification path's `dispatched_at` behaviour is unchanged |
| **Compatibility** | `delivery_state IS NULL` rows are claimable, so a mixed-version database works unchanged; the notification path is untouched |
| **Rollback** | reverting the stage leaves the columns `NULL` on rows that were claimed — the claim is then a no-op and the behaviour is today's. **Do not roll back below this stage while rows are `claimed`** (an older binary ignores the axis and re-dispatches them — today's behaviour, and a runbook note). |
| **Stop condition** | stop if the notification path's `dispatched_at` behaviour changes, or if a tick's wall-clock cost grows materially |
| **Gate** | AT8, AT9a green; the existing scheduler suite green unchanged |

### Stage F — M3b: the settlement

| | |
|---|---|
| **Prerequisites** | E |
| **Files** | `internal/core/wake_settlement.go` (a **new** fenced settlement function; `MarkWakeFired` is unchanged); `internal/scheduler/scheduler.go` (`Tick` passes the claim token) |
| **DB invariants** | a settlement moves `claimed → delivered` and writes `delivered_at`, `fired = 1` and `fired_at` **in one statement**; a settlement that is refused changes nothing and is **reported** |
| **Observable behaviour change** | a scheduler-settled row carries `delivery_state = 'delivered'` and `delivered_at`; a refused settlement is reported by `Tick` and the wake stays pending |
| **Regression tests** | AT9b, AT10 (boundary 5); a claimed-then-settled row has `fired = 1` + `delivery_state = 'delivered'`; a fold-retired row and a sweep-retired row have `fired = 1`, `delivery_state IS NULL` and `delivered_at IS NULL`; `MarkWakeFired`'s existing contract is unchanged |
| **Compatibility** | mixed-version safe: an older binary's settlement still lands `fired = 1` |
| **Rollback** | as stage E — do not roll back while rows are `claimed` |
| **Stop condition** | stop if any retirement path outside the scheduler's dispatch is found to write `delivery_state` |
| **Gate** | the settlement tests green; `internal/scheduler/wake_settlement_test.go` green **unchanged** |

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

### Stage I — M4b: the broadcast

| | |
|---|---|
| **Prerequisites** | F; and the **operator's decision** in §7 |
| **Files** | `internal/scheduler/scheduler.go` (`BroadcastHandler`); `cmd/mpm/ops_broadcast_cmds.go`; `internal/core/broadcast.go` (the wake-id dedupe key) |
| **Observable behaviour change** | **fixing the handler creates a fan-out that does not exist today** — an external effect. This is the operator's call |
| **Regression tests** | AT11-broadcast: two deliveries ⇒ one fan-out; the content dedupe still holds; the CLI accepts the handler's argument shape |
| **Stop condition** | stop if the operator declines the behaviour change — the content dedupe already bounds the crash window, so the stage is optional |
| **Gate** | the broadcast tests green |

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
| **Prerequisites** | F, H, I |
| **Change** | add `TestWakeDelivery_ARestartBeforeSettlementLeavesTheWakeClaimed` **and only then** remove `TestWakeDelivery_ARestartBeforeSettlementLeavesTheWakePending` — **in the same PR**, with the old docstring pointing at the replacement |
| **Why not earlier** | the characterisation test's own docstring says *"If a future change closes this window, THIS TEST FAILS, and that is the point"*. M1–M5 do **not** close it — they narrow it — so removing the pin before the per-handler guards land removes it too early. |
| **Assertion** | **per handler**, never "exactly one effect in total": drill ⇒ one `drill_runs` row and one harness invocation; gc ⇒ one CAS claim; snapshot ⇒ one rotation step |
| **Residual** | W1 remains — the handler **may** run twice; only the effect is deduplicated. This is stated in the replacement test's docstring. |

---

## 5. The acceptance-test specification

All twelve are deterministic. **No test may contain a sleep or an arbitrary wait.** The two
mechanisms used are the **SQLite authorizer barrier** (already in the tree at
`internal/core/wake_resolve_contention_test.go`, which pauses a statement at `PREPARE` time and
therefore observes the arrival explicitly before doing anything else) and **subprocess re-exec with a
named crash point** (the standard Go pattern: the test re-executes itself with an environment
variable naming the crash point, signals the parent over a pipe, and the parent `SIGKILL`s it).

| # | Scenario | Deterministic mechanism | Assertion |
|---|---|---|---|
| **AT1** | **Existing database migration** | a disposable file-backed WAL DB with the **pre-M1** schema (the exact base DDL plus the `dispatched_at` and `fired_by` migrations and their sentinels), populated with representative rows; then opened by the new binary | the five columns exist; the sentinel count is 1; the column set is exactly the post-M1 set |
| **AT2** | **Fresh database creation** | a new DB through the same path | the column set is **identical** to AT1's; the sentinel count is 1; the base DDL and the migration do not conflict |
| **AT3** | **Whole-row preservation** | snapshot every pre-existing row's every pre-existing column before and after, as a whole-row string | **byte-identical** — not just `fired`; the whole row set, compared as a map keyed by id |
| **AT4** | **Zero fabricated delivery history** | rows with `fired = 1` from each of the five retirement paths, plus `kind='cron'` rows | every row has `delivery_state IS NULL` and `delivered_at IS NULL`, **including** the `fired = 1` rows and the never-deliverable cron rows |
| **AT5** | **Idempotent migration** | run the migration twice, then a third time | the column set, the row set, the index list and the sentinel count are unchanged by runs 2 and 3 |
| **AT6** | **Failed-migration rollback** | a trigger that aborts the **third** `ALTER TABLE` | the migration returns an error; the column set is **unchanged**; no sentinel is recorded; the row set is unchanged; `NewDatabaseManager` surfaces the error |
| **AT7** | **Mixed-version operation** | (a) run the **pre-M1 statements** (the old settlement, the old fold, the old resolver, the old claim) against an M1-schema DB; (b) a row settled by the old binary (`fired = 1`, `delivery_state IS NULL`) | (a) the old statements still work and leave `delivery_state` untouched; (b) the row is **not** re-claimed — this is the C7 hazard, closed by ★A |
| **AT8** | **Lease expiration and reclaim** | insert rows with `delivery_state` `'claimed'` and a `lease_expires_at` computed from the **DB clock** (`CAST(strftime('%s','now') AS INTEGER) ± 1` or `± 3600`) — no sleep | the claim admits the expired row, refuses the live-lease row, and admits the `NULL` row exactly once |
| **AT9** | **Stale-claim fencing** | (a) the authorizer barrier: two claimers paused at the `UPDATE` boundary, then both released — exactly one must see `RowsAffected == 1`; (b) owner A's lease expires, owner B re-claims, A then settles | (a) exactly one winner; (b) A's settlement affects **0 rows** and is reported, and the row is still owned by B. Under the singleton, (b) is a defence-in-depth assertion |
| **AT10** | **Process death at each dispatch boundary** | subprocess re-exec with `MPM_WAKE_CRASH_AT ∈ {after_select, after_claim, during_handler, after_effect, during_settlement}`; the child signals the parent at the crash point and the parent `SIGKILL`s it | at each boundary: the post-crash row state is asserted exactly; after `after_claim` the wake is reclaimable after the lease and **not** before; after `after_effect` the wake is re-dispatched (the duplicate is visible as a fresh `claimed_by`); after `during_settlement` the wake is still pending and re-dispatched |
| **AT11** | **Handler-specific duplicate effects** | deliver the same wake twice to each handler | **per handler**: `drill` ⇒ one `drill_runs` row with a non-NULL `wake_id` and one harness invocation; `gc` ⇒ one CAS claim; `snapshot` ⇒ one rotation step; `broadcast` ⇒ one fan-out. **Never** "exactly one effect in total" |
| **AT12** | **Preservation of agent-owned wake behaviour** | the existing suites, run **unchanged** | `internal/core/wake_ownership_test.go`, `internal/core/tools/wake_resolve_ownership_test.go`, `internal/core/tools/wake_arming_authorisation_test.go`, `internal/scheduler/wake_ownership_test.go`, `internal/scheduler/wake_settlement_test.go`, `internal/core/wake_resolve_contention_test.go` all pass **unchanged**; the fold, the resolver, `kinds:["*"]`, the notification claim and both sweeps read and write **no** new column |

**AT1–AT6 and AT8–AT9 were executed as an isolated experiment before this specification was written
(Appendix A). AT7, AT10, AT11 and AT12 are specified against mechanisms that already exist in the tree
and have not yet been executed.**

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
| drill | `internal/scheduler/drill_handler_test.go`, `internal/scheduler/drill_e2e_test.go` | extended in stage H |
| expiration sweeps | `internal/scheduler/wake_expiration_test.go` | extended in stage C |

**The `git diff --stat` discipline.** Stage A must show **no** change under `internal/scheduler/`.
Stage C must show no change under `internal/core/wake_settlement.go`. Stage E must show no change to
`internal/scheduler/dispatch.go`. These are the mechanical guards against an axis leaking into the
paths it must not touch.

---

## 7. Unresolved risks and the operator's decisions

### 7.1 The operator's decisions

| # | Decision | Recommendation | Status |
|---|---|---|---|
| 1 | Is `fired` retired, a projection, or an independent axis? | **Independent** | settled here (§3.1) |
| 2 | Is `delivery_state` scoped to the scheduler axis or does it cover notifications too? | **Scheduler-dispatch only** | settled here (§3.1) |
| 3 | Lease duration | **30 min**, with a 15-min handler deadline | settled here (§3.3) — needs the operator's agreement on the **new** deadline behaviour |
| 4 | Lease clock authority | **The DB clock**, inside the statement | settled here (§3.3) |
| 5 | Is `claimed_by` observability or a fencing token? | **A fencing token** | **settled here (§3.5) — this contradicts the report's recommendation, on the evidence of Appendix A, E4** |
| 6 | Should `mpm_wakes schedule` be able to arm `drill` / `broadcast` / `snapshot` / `gc` wakes? | **No — an allowlist** | **closed by #216** |
| 7 | Should `kinds:["*"]` be permitted to retire system-kind wakes? | **No** | **closed by #215** |
| 8 | Should `BroadcastHandler` be fixed? | **Ask.** Fixing it **creates** an external effect that does not exist today. "The broadcast handler has never worked" may be load-bearing for an operator's mental model. | **open — stage I is optional** |
| 9 | Rollback policy during the axis window | binary rollback allowed and safe; **schema rollback forbidden**; do not roll back below stage E while rows are `claimed`; `--with-db-restore` restores a pre-migration backup and re-dispatches formerly-claimed rows (today's behaviour) | settled here (§3.8) — the runbook entry is still owed |
| 10 | Is the notification path in scope? | **No** | settled here (§3.1) |
| **11** | **Should the `snapshot` label be derived from the wake id?** | **Yes** — otherwise a duplicate delivery writes a different file and advances the rotation | **open — required before stage I** |
| **12** | **Is a 15-minute per-handler deadline acceptable?** | **Yes** — it is the prerequisite that makes the lease duration defensible | **open — a behaviour change on the dispatch path** |

### 7.2 Residual risks

| # | Risk | Severity | Mitigation |
|---|---|---|---|
| R1 | **The lease does not close W1.** The handler may run twice; only the effect is deduplicated, per handler. | high | stated in every stage's gate; the series' success criterion is per-handler, never global |
| R2 | **A handler that legitimately needs more than 15 minutes is now cancelled.** | medium | the deadline is a named constant; the operator's decision 12; the drill's own 120 s is far inside it; stage D ships separately so it can be reverted alone |
| R3 | **A 30-minute crash-recovery latency** for a system-kind claim. | low | the system kinds are hourly or daily; a per-kind lease is a later refinement |
| R4 | **The `snapshot` rotation is not idempotent** and stage I's scope depends on decision 11. | medium | decision 11 is a prerequisite for stage I; until then the snapshot's duplicate cost is bounded by the 7-file rotation |
| R5 | **`mpm-update --rollback --with-db-restore` reverts the schema.** | medium | the migration is idempotent and re-adds the columns on the next open; the re-dispatch of formerly-claimed rows is today's behaviour and belongs in the runbook |
| R6 | **Two schedulers running concurrently** (a misconfigured `--lock` / `MPM_DB_LOCK` / workspace) would let two claims coexist inside the lease window. | low | the flock is the primary guard; the lease is the secondary; the owner fencing is the third. ★B makes the third real. |
| R7 | **The delivery axis is not read by anything** until stage J. | low | deliberate — the axis is written first and read only after it has been proven in production |
| R8 | **The two clocks** (`fired_at` on the process clock, `delivered_at` on the DB clock) invite a future reader to difference them. | low | documented explicitly in §3.6 and in the migration's docstring |

---

## 8. Recommendation

**READY FOR STAGE A IMPLEMENTATION.**

The architecture is implementable as specified, and the eight §7.0 corrections are all honoured:

| Correction | How this specification satisfies it |
|---|---|
| C1 — `fired` is retirement, not delivery | the axis is independent; the five writers are enumerated in §1.1; no reader of `fired` changes |
| C2 — `delivery_state` is an independent axis | §3.1; the thirteen retirement readers keep reading `fired` |
| C3 — do not backfill `delivered` from `fired` | §3.2 — no backfill, and AT4 asserts it |
| C4 — an expired claim must be reclaimable | §3.4 — the corrected predicate; the superseded one is proved unreachable (Appendix A, E3) |
| C5 — preserve `target_time` eligibility | §3.7 — `target_time <= ?` is in the claim predicate |
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

5. **Stage D — the per-handler deadline.** The lease duration cannot be bounded without it; only one
   of the six system handlers has a bounded runtime today.
6. **Stages C — the two independent defect fixes** (W3's `fired_at` overwrite, W4's malformed-metadata
   abort). Neither is in the report, and W4 silently stops notification delivery.

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
| | the **superseded** §7.3.1 predicate can **never** reclaim an expired claim | PASS |
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
| `executeOne` can fail without running the handler | §1.1; `internal/scheduler/scheduler.go:579-598` and `:508-518` |
| P1 has no retry budget | §1.1; `internal/core/wake_tools.go:119`, allowlisted at `internal/core/bare_pool_write_ratchet_test.go:658` |
| the settlement has no `fired = 0` guard, deliberately | `internal/core/wake_settlement.go:78-83` |
| `MarkWakeFired` has exactly one production caller | §1.1; `internal/scheduler/scheduler.go:429` |
| the scheduler is a flock singleton | §1.3 I6; `cmd/mpm-scheduler/main.go:118`, `internal/scheduler/scheduler.go:865` |
| `mpm-update` never opens the database | §3.8; verified against `cmd/mpm-update/` |
| the migration lands lazily, in the fatal transaction | §3.8; `internal/core/db.go:3117` and `:3135` |
| `SafeMigrations` silences errors | §3.8; `internal/core/db.go:2967-2975` |
| the drill's timeout is 120 s | §3.3; `internal/core/drill.go:56-63` |
| `gc` is at-most-once | §3.10; `cmd/mpm/handlers_gc.go:174-190` |
| broadcast is already content-deduplicated | §3.10; `internal/core/broadcast.go:122,231,244,518` |
| the drill creates a fresh run id per execution | §3.10; `internal/scheduler/drill_handler.go:74` |
| the snapshot label defaults to the epoch | §3.10; `internal/scheduler/scheduler.go:914`, rotation at `:938` |
| W3 — the sweep overwrites `fired_at` | Appendix A, E2 |
| W4 — one malformed row aborts a dispatch statement | Appendix A, E5 |
| W6 — the proposed settlement guard does not fence a stale owner | Appendix A, E4 |
| the additive migration is behaviour-neutral | Appendix A, E1 (T1–T6) |
| schema rollback is forbidden, and the rule is enforced | Appendix A, E1 (T7a) |
| the corrected lease predicate reclaims; the superseded one cannot | Appendix A, E3 |
| the #218 authorizer barrier is the deterministic contention primitive | `internal/core/wake_resolve_contention_test.go` |
