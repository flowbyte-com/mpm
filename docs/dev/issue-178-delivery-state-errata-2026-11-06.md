# Issue #178 — delivery-state design: errata

> **Status:** errata to
> [`issue-178-delivery-state-design-2026-11-04.md`](issue-178-delivery-state-design-2026-11-04.md)
> (PR #219).
> **Date:** 2026-11-06.
> **Raised by:** the independent review of PR #232.
> **Status of the design document:** **frozen and unedited.** This file
> carries the corrections, so the frozen artefact's SHA does not change.
>
> **This is not a redesign.** Every item below is a correction to a
> statement the frozen document makes, or a requirement the frozen
> document already implies but does not spell out. None of it changes
> the delivery axis's scope (`scheduler-dispatch only`), and none of it
> is implemented in PR #232.

## Why this file exists

The delivery-state design document is an approved artefact of PR #219.
Its statements about the cron wake were written when the agent's
opportunistic fold (`core.CheckPendingWakes`) delivered **notification
wakes only** — the `cron` and `cascade` kinds were agent-owned in the
ownership allowlist but unreachable from the default fold.

PR #232 widened the default fold to the agent-owned allowlist, so the
cron wake became the agent's delivery path. Several statements that
were accurate under the old (broken) behaviour are now literally
false. Editing the frozen document was rejected: an approved artefact
should not be rewritten underneath the readers who approved it. The
corrections live here instead, and this file is the place Stage B/C
should look for them.

## Erratum 1 — "cron rows are never deliverable at all" (LOW-2)

The frozen document says, at three places:

> `kind='cron'` rows are **never deliverable at all** (no handler is
> registered; the notification claim excludes them).

**What is still true.** The statement is scoped to the
**scheduler-dispatch** axis, which is the only axis the document
defines. A cron wake is still never claimed by the scheduler's
ad-hoc claim and is still never dispatched to a handler. That part is
unchanged, and it is now pinned by a test
(`internal/scheduler/wake_cron_retention_interaction_test.go`,
`TestDispatchClaim_DoesNotClaimACronWake`).

**What is no longer true.** Read without the axis scoping, the
sentence is false. A cron wake **is** deliverable — by the agent's
opportunistic fold, on the agent's next request — since PR #232. The
wake row carries the `directive_id` the agent has to consult, and the
fold's `<system_wake_notification>` block renders it.

**Consequence for Stage B/C.** A reader who greps that sentence and
concludes "agent delivery is out of scope for the axis" will reach
exactly the wrong conclusion, because agent delivery is now the cron
wake's ONLY delivery path. The axis's scope has to be revisited
deliberately — see Requirement 1 below.

## Requirement 1 — the axis needs a `presented` state (MEDIUM-1)

The fold's pipeline is:

```
selected → COMMIT(fired=1, fired_at) → presented to the agent
```

There are therefore **three** terminal states for a cron row, not two:

| state | `fired` / `fired_at` | metadata | distinguishable? |
|---|---|---|---|
| **LOST** — the fold settled it, then crashed before presenting it | `1` / set | no note | **no** — byte-identical to DELIVERED |
| **SWEPT** — the retention sweep retired an undelivered row | `1` / set | `expired.reason=cron_1h_retention` | yes |
| **DELIVERED** — the agent received it | `1` / set | no note | **no** — byte-identical to LOST |

The frozen document's backfill decision (§7.0 C1/C3) and its
acceptance criterion AT4 both say that rows retired by the fold keep
`delivery_state IS NULL` and `delivered_at IS NULL` — "retired, never
delivered". That is now **literally** what a fold-delivered row
carries, and it is **semantically wrong** for the cron wake: the agent
did receive it.

The axis's vocabulary is therefore backwards for the row the agent
actually received: `delivery_state IS NULL` reads as "never
delivered" on a row that was delivered.

**What Stage B/C must add.**

1. **A `presented` (or `delivered_at`) write by the fold.** The fold
   must record that it handed the wake to the agent, so that
   DELIVERED and LOST stop being the same bytes.
2. **A claim/lease on the fold's own path.** Without one, a crash
   between the fold's COMMIT and its presentation leaves a wake that
   is `fired=1` and unrecoverable — the wake is neither re-delivered
   (it is fired) nor re-injected (the next cron tick is a NEW row).
   Opportunistic delivery is currently **at-most-once and not
   crash-safe**, and that must be stated in the axis's vocabulary
   rather than left implicit.
3. **A decision on the axis's scope.** Either the axis grows to
   cover agent delivery, or the document states plainly that it does
   not — and that a fold-delivered row is deliberately recorded as
   "never delivered" on this axis. The second option is defensible,
   but it must be a decision, not an accident of the axis's original
   scope.

## Requirement 2 — the cascade reconciler needs a claim with a lease (MEDIUM-2)

The cascade materializer's booking is on the **outbox** row
(`epistemic_cascade_outbox.wake_scheduled`), not on the wake row. The
reconciler schedules the wake **first** and claims the outbox row
**second**, precisely so that a crash between the two leaves a wake
that a later pass can find (claim-first would leave the row flagged
with no wake — a lost obligation).

That order has the mirror-image defect: two overlapping reconciler
passes can both SELECT the same outbox row, both schedule a wake, and
one of them loses the claim. The loser reclaims its own wake — but
only while that wake is still **pending**. If the agent's fold settled
it first, the reclaim is skipped (the delivery record survives — see
PR #232's HIGH-1 correction) and **the same obligation has been
delivered twice**.

The wake row carries no obligation key the substrate could dedupe on
without a new column, and a "dedupe" that cancelled the duplicate
would settle a wake without delivering it — trading a duplicate
delivery for a silent loss. So the fix is not deduplication.

**What Stage B/C must add.** A claim with a **lease** on the
reconciler's own path: the reconciler claims the outbox row with a
lease, and a pass whose lease has expired can be reclaimed. That
removes the duplicate window without opening a loss window, because an
expired lease means "nobody finished this", which is recoverable.

**What is already in place (PR #232).** The duplicate is no longer
silent. The reconciler logs it explicitly when it cannot reclaim a
wake the fold already settled, and the materializer's comment no
longer claims (falsely) that the per-check delivery cap bounds the
duplication. The obligation's identity is
`(theory_id, invalidation_event_id)`: the same theory invalidated by
two different events is **two** distinct obligations, and both must
reach the agent — a dedupe on `theory_id` alone would lose one.

## Requirement 3 — the cron backlog must be bounded by the sweep's capacity

Not a delivery-axis item, but it belongs here because it decides
whether the cron wake's "delivery deadline" is meaningful.

The cron retention sweep retires at most `CronRetentionNormalLimit =
60` rows/hour normally and `CronRetentionCatchUpLimit = 180` on
catch-up. One one-minute cron task produces exactly 60 rows/hour, so
the two are balanced **at one task**. The second one-minute task
produces 120/hour against a 60/hour sweep; from the fourth task the
backlog grows even in catch-up mode (240 produced vs 180 retired).

**What must move together when a second one-minute cron task is
added:** `CronRetentionNormalLimit`, the doctor's escalation ladder
(60/240), and the fold's per-call cron budget
(`core.MaxCronWakePerCheck`). The sweep's capacity is calibrated for
one task and that calibration is stated nowhere else.

The fold's per-call delivery budget bounds what the **agent** receives
per call. It does not bound the backlog, and it does not make a
backlog that outruns the sweep deliverable: a cron row the sweep
retires before the fold reached it was never delivered. The deadline
was missed, not shortened.

## Summary of what is corrected, and where

| item | frozen document | correction |
|---|---|---|
| LOW-2 | "cron rows are never deliverable at all" (3 places) | still true for the scheduler-dispatch axis; false for agent delivery |
| MEDIUM-1 | §7.0 C1/C3, AT4 — fold-retired rows are "never delivered" | literally true, semantically wrong for a fold-delivered cron wake; needs a `presented` state and a claim/lease |
| MEDIUM-2 | not addressed | the reconciler needs a claim with a lease; the duplicate is visible, not fixed |
| — | not addressed | the sweep's capacity must be re-derived for the second one-minute cron task |

The frozen document itself is unchanged. Nothing in this file is
implemented by PR #232 beyond the corrections it makes to the
surrounding comments and to `docs/SPEC.md`.
