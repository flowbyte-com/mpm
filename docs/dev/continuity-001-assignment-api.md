# Continuity 001, PR 2 — the assignment API, delegation and CLI

**Status:** design of record for PR #260 (PR 2 of six), issue #258.
**Prerequisite:** PR #259 (merged as `dfd10fb2`), whose storage this PR
builds on without altering it.

---

## 1. What PR 2 is, and what it is not

PR 1 was **storage**: two tables, seven indexes and thirty triggers, created
empty. PR 2 is the **coordination interface** over that storage — a service
that creates, delegates, delivers, claims, fences, completes and cancels
assignments, plus a CLI that drives it and an MCP surface that exposes only
the operations an agent may safely perform.

PR 2 does **not** execute anything. There is no OpenCode adapter (PR 3), no
dispatcher or recovery loop (PR 4), and no GitHub evidence surface (PR 5).
MPM coordinates and remembers; it does not run the work.

**The four invariants PR 1 deliberately deferred to PR 2** are the spine of
this design:

| | invariant | where it is enforced here |
|---|---|---|
| **N1** | a claimed write presents the current `claim_epoch`, checked atomically | every state-changing operation takes the epoch and writes `WHERE id = ? AND claim_epoch = ?` |
| **N2** | a child's recipient is among the root's `eligible_recipients` | `Delegate` validates it, and every operation that touches a child re-validates it |
| **N3** | a child's depth is within the root's `max_depth` | `Delegate` validates it |
| **N6** | a root is created only through an operator-controlled entry point | root creation is **absent from the tool registry**; only the CLI verb reaches it |

---

## 2. The authority boundary, stated honestly

### 2.1 What MPM cannot do

MPM cannot authenticate anybody. The workspace is a **single-operator trust
domain** (PR #224's OD3): an agent running under the operator's own Linux
account can read the operator's files, run the operator's binaries and
write to the operator's database. There is no identity provider here, and
this PR does not add one.

So `actor_kind`, `actor_address` and every other attribution field are
**attribution, not identity**. They record who wrote a row. They do not
establish who that party was, and no rule in this PR treats them as
authentication. PR 1's ledger says the same thing in its own comment:
*"ATTRIBUTION, NOT IDENTITY."*

### 2.2 What is enforced anyway

Two things are enforced **structurally**, not by a flag:

1. **Root creation is not in the tool registry.** The registry is the single
   source of truth shared by `mpm call <tool>` and the MCP server
   (`internal/core/tools/registry.go`). A root can be created only by the
   dedicated CLI verb `mpm assignment create-root`, which calls the core
   primitive directly. No MCP action and no `mpm call` action can create a
   root, because **no such action exists**.
2. **Operator approval is not in the tool registry either.** It is reachable
   only through `mpm assignment record-approval`.

This is the shape MPM already uses for irreversible deletion: the shared
registry handler refuses the `shred` action on every agent-facing surface,
and the dedicated CLI verb calls the `DatabaseManager` primitive directly
(`internal/core/tools/operator_only_shred_regression_test.go`). PR 2 makes
the same boundary **stronger** by removing the action from the registry
altogether rather than refusing it at call time.

**A confirmation flag is not a credential.** No `--confirm`, no
`--operator-approved`, no `--evidence-ref` and no client-supplied
`actor_kind` unlocks anything. A boolean the agent itself sets cannot
distinguish "the operator asked for this" from "the agent decided this",
and this PR says so in the code rather than in a comment alone.

### 2.3 The explicit trusted-call boundary

The service's operator-only operations take an `OperatorAuthority`, an
interface with an **unexported method**. No type outside the package can
satisfy it, so no JSON payload and no agent-facing handler can fabricate
one. The concrete value is produced by `NewLocalOperatorAuthority(actor)`.

This is a **boundary, not authentication**. It makes the operator path
*explicit in the call* — a caller must consciously choose it — and it makes
the agent-facing path *structurally incapable* of choosing it. It does not
stop a process with shell access to the same account from running the CLI
verb, and this document does not claim that it does.

**Autonomous activation stays disabled.** Nothing in this PR activates a
root by itself; the dispatcher that would is PR 4, and it is behind an
explicit activation gate.

---

## 3. The service

### 3.1 Where it lives

`internal/core/assignment/` — a new package inside the `mpm-core` module,
following the `internal/core/capability` precedent: a `Store` over the
shared `DatabaseManager` with an **injected clock**.

Both surfaces call the same service, so there is exactly one implementation
of every transition rule:

* the **CLI** (`cmd/mpm/assignment_cmds.go`) calls the service directly;
* the **MCP/`mpm call` surface** (`internal/core/tools/handlers_assignment.go`)
  calls the same service.

### 3.2 The clock

`Store.Now()` reads an injected `Clock`. Production uses `time.Now().UTC()`;
tests inject a frozen or fast-forwarded clock. **No test sleeps.** Lease
expiry, reclaim and the approval's recorded moment are all observed through
the injected clock, so the semantics are deterministic rather than
timing-dependent.

### 3.3 The operations

| operation | transition | who |
|---|---|---|
| `CreateRoot` | — (a new root) | operator only |
| `Delegate` | — (a new child) | agent, bounded by the root |
| `Deliver` | `enqueued → delivered` | runtime |
| `Acknowledge` | `delivered → acknowledged` | recipient |
| `Accept` | `acknowledged → accepted` | recipient |
| `Refuse` | `acknowledged → cancelled` | recipient |
| `Claim` | — (a fenced lease) | worker |
| `Renew` | — (a new epoch, a new lease) | the holder |
| `Start` | `accepted → in_progress` | the holder |
| `Complete` | `in_progress → completed` | the holder |
| `RecordReviewResult` | see §3.6 | reviewer |
| `HandoffToOperator` | `review_passed → awaiting_operator` | the holder |
| `RecordApproval` | `awaiting_operator → operator_approved` | operator only |
| `Cancel` | any → `cancelled` | the holder, or the operator |
| `Note` | — (a ledger note) | anyone with access |

Reads: `Get`, `Inbox` (addressed to a recipient), `History` (the ledger),
`ListByRoot` (the tree).

#### Delivery is at-least-once, and never exactly-once

The delivery transition is refused a second time — the state machine has
one `delivered` state, and a second delivery would be a second state
change. That is not the delivery being lost:

* the assignment **stays in the recipient's inbox** until somebody
  acknowledges it, so a second runtime addressed to the same recipient
  picks the work up rather than finding it gone;
* the refused delivery writes **nothing** — no state change and no
  ledger entry; and
* the recovery is to **continue from the current state**: the recipient
  acknowledges what was delivered and the path continues.

So the guarantee is at-least-once **over the recipient**, not over the
runtime. A runtime that crashes after delivering does not lose the work
and does not need the delivery to be replayed; the work is recovered by
acknowledging it.

The test that pins this is `TestAPIRedeliveryIsRefusedAndTheWorkStaysVisible`
— a test that only checked the refusal would pass with a delivery that
dropped the work on the floor.

### 3.4 The fence

Every state-changing operation presents the caller's **current claim
epoch**, and the write is `WHERE id = ? AND claim_epoch = ?`. A stale
holder's write therefore matches **no row**, atomically with the write
itself. That is N1, and it is the fence PR 1 deliberately left to the
`WHERE` clause — a trigger cannot see the epoch a caller *presented*.

A **claim** is a compare-and-swap: the caller presents the epoch it
believes is current, and the write is
`SET claim_epoch = claim_epoch + 1 … WHERE id = ? AND claim_epoch = ?`.
Two simultaneous claims therefore cannot both succeed: the first takes
epoch *n* → *n+1*, and the second's `WHERE` no longer matches.

The epoch is not a secret. It fences a **stale writer**, not a **hostile
one** — see §2.1.

### 3.5 Claims, leases and reclaim

The semantics are stated precisely, because "the lease expired" is otherwise
a phrase with two readings:

* an assignment with `claim_epoch = 0` is **unclaimed**: no owner, no lease;
* an assignment with `claim_epoch > 0` is **claimed**: it has an owner and
  a lease;
* a lease is **alive** while `lease_expires_at > now`.

| the caller's situation | `Claim` | `Renew` |
|---|---|---|
| unclaimed | **grants** the claim: epoch + 1, owner set, a new lease | refused — there is nothing to renew |
| claimed, lease alive | **refused** — the work is leased | refused — a live lease needs no renewal |
| claimed, lease expired | **reclaims**: epoch + 1, a new owner, a new lease | refused — the holder is stale |
| claimed by **me**, lease alive | refused — use `Renew` | **renews**: epoch + 1, the same owner, a new lease |

So a lease is **never stolen from a live holder**, and a lease that has
expired **is** reclaimable — and the reclaim fences the previous holder out,
because the epoch has advanced.

**A renewal advances the epoch.** PR 1's rule is that the lease changes
only when the claim epoch advances, and that a claim sets a **new** lease.
A renewal that kept the epoch could not move the lease at all. So `Renew`
is a claim by the same owner: the epoch advances, the old epoch is stale,
and the holder must use its new epoch from then on.

`Start` requires a claim (`claim_epoch > 0`): an assignment cannot be
*in progress* without a lease. `Complete` requires the presented epoch to
match, so a holder whose lease was reclaimed cannot overwrite the new
holder's result.

### 3.6 The review

PR 1's state machine makes the **subject's own row** carry the verdict: an
implementer travels `completed → review_passed`. Somebody must perform that
transition, and the only party with the standing to do it is the
**independent reviewer**.

The reviewer is a separate assignment with `role = 'reviewer'`. Its
`payload` names the assignment it reviews:

```json
{"review_target": "at-0123456789abcdef"}
```

The delegation that created the reviewer therefore **fixes** the target —
the reviewer cannot be pointed at an arbitrary subject afterwards.

`RecordReviewResult(reviewer, verdict)`:

| verdict | the subject | the reviewer | events |
|---|---|---|---|
| `passed` | `completed → review_passed` | `in_progress → review_passed` | one on each — two transitions, two events |
| `changes_requested` | unchanged (stays `completed`) | `in_progress → completed` | the reviewer's `completed`, plus a `note` on the subject so the verdict is not invisible on the work it judges |

The subject must be an implementer in the **same tree**, and it must not be
an **ancestor** of the reviewer. That is the independence check: a reviewer
delegated *by* the work it reviews is refused, because the subject is on
the reviewer's parent chain. The walk terminates because the depth is
immutable and therefore strictly decreasing.

**THE REWORK IS A NEW ASSIGNMENT.** A `changes_requested` verdict leaves the
subject `completed`, because PR 1's state machine has no path back from
`completed` — it is a forward path, and PR 2 does not widen it. The rework
is therefore a **child delegated from the subject**: a fresh assignment that
walks the path from the beginning and is reviewed in its own turn. That is
the honest shape of "the review asked for changes" — the work that was asked
for is a new unit of work, and the old one stands as the record of what was
tried.

**The reviewer's ledger is the record of the review**, and the subject's
ledger shows the verdict as a state change (when it passed) or a note
(when it did not). The handover to the operator's gate is a **separate,
explicit** operation (`HandoffToOperator`), so that reaching the gate is a
decision rather than a side effect of the verdict.

### 3.7 The gate and the approval

`HandoffToOperator` moves `review_passed → awaiting_operator` and **must
supply** `awaiting_action` — the exact action the operator is being asked
to authorise. PR 1's trigger refuses a transition that does not change the
field, so the value is attributable to the transition.

`RecordApproval` moves `awaiting_operator → operator_approved` and must
supply the action, the revision, the source **and** the evidence pointer.
The approval must name the **same action** the gate awaited, and PR 1's
trigger refuses it otherwise.

**MPM records an approval it did not manufacture.** The revision and the
evidence pointer are the operator's claims about what was authorised, and
the row says so. A change to the head revision after the approval leaves
the approval **stale**, and staleness is observable rather than implied.

### 3.8 Idempotency

The idempotency key is `UNIQUE`, and PR 1 found that the before-insert
trigger fires **before** the conflict resolution — so `ON CONFLICT DO
NOTHING` **aborts** rather than resolving the duplicate. PR 2 therefore
**selects** rather than relying on `ON CONFLICT`:

1. `SELECT` by idempotency key. If a row exists, return it.
2. Otherwise `INSERT`. If the insert is refused by the not-reused trigger,
   `SELECT` again and return the row that won the race.

A duplicate request therefore resolves to the **existing logical
assignment** — including when two requests race, because the unique index
makes exactly one of them win. The returned value says whether the caller
created the row or found it.

**A conflict is not silently ignored.** If the same key is presented with a
*different* subject — a different project, parent, recipient or role — the
service reports a conflict rather than handing back a row that does not
match the request. Silently returning the wrong logical assignment is
worse than an error.

### 3.9 One event per transition, atomically

Every state transition appends **exactly one** event to the transitioned
assignment's ledger, in the **same transaction** as the state change. There
is no window in which the row moved and the ledger did not, and no window
in which the ledger moved and the row did not.

The event index is `MAX(event_index) + 1`, which is strictly greater than
the current maximum — PR 1's monotonicity rule.

The one documented exception is `RecordReviewResult` with the verdict
`changes_requested`: the reviewer transitions (one event) and the subject —
which does **not** transition — receives a `note`, so that a failed review
is not invisible on the work it judges.

### 3.10 What PR 2 does **not** enforce

PR 1 deferred the tree's **aggregate** budgets to PR 4, and PR 2 leaves
them there:

* `max_tasks` — the tree's total task count;
* `max_concurrency` — the tree's concurrent claims;
* `max_resource_units` — the tree's cumulative resource consumption.

PR 2 records `resource_units` per assignment and **does not** sum it, and
it must not claim the protection is operational. **PR 4 enforces these.**

---

## 4. The surfaces

### 4.1 The CLI

`mpm assignment <verb>` — one namespace, small verbs, JSON where it helps.

Every verb accepts every flag and **ignores** the ones that do not apply to
it, rather than each verb declaring its own set: a flag set that differs per
verb is a flag set that surprises. A list-valued flag is comma-separated
(`--eligible-recipient alpha,beta`), which is the CLI's existing convention
for a list (`mpm handoff write --tag a,b,c`).

```
mpm assignment create-root       --project-id --title --objective --recipient
                                 --idempotency-key
                                 --permitted-operation read,write
                                 --eligible-recipient alpha,beta
                                 --max-depth --max-tasks --max-concurrency
                                 --max-resource-units [--resource-units]
                                 [--issue-ref --pr-ref --base-sha --head-sha
                                 --reviewed-sha --findings-sha256 --evidence-ref]
mpm assignment delegate           --parent-id --title --objective --recipient
                                 [--role] --idempotency-key [--resource-units]
                                 [--issue-ref --pr-ref --base-sha --head-sha
                                 --reviewed-sha --findings-sha256 --evidence-ref]
mpm assignment inbox              [--recipient] [--state enqueued,delivered]
                                 [--project-id] [--limit]
mpm assignment show               --id
mpm assignment history            --id
mpm assignment tree               --root-id
mpm assignment deliver            --id [--epoch <n>] [--note]
mpm assignment acknowledge        --id [--epoch <n>] [--note]
mpm assignment accept             --id [--epoch <n>] [--note]
mpm assignment refuse             --id [--epoch <n>] [--note]
mpm assignment claim              --id --owner <addr> --lease-seconds <n>
                                 [--epoch <n>]
mpm assignment renew              --id --owner <addr> --lease-seconds <n>
                                 [--epoch <n>]
mpm assignment start              --id [--epoch <n>] [--note]
mpm assignment complete           --id [--epoch <n>] [--note]
mpm assignment record-review      --id --verdict passed|changes_requested
                                 [--epoch <n>] [--note]
mpm assignment handoff            --id --awaiting-action "<exact action>"
                                 [--epoch <n>] [--note]
mpm assignment record-approval    --id --action --revision --source --evidence
mpm assignment cancel             --id [--epoch <n>] [--note]
mpm assignment note               --id --note
```

`--json` is accepted by every verb and emits the service's own projection.

`create-root` and `record-approval` are **operator verbs**: they are not in
the registry, so no MCP action and no `mpm call` action reaches them.


### 4.2 The MCP / `mpm call` surface

One tool, `mpm_assignment`, with an `action` parameter — the shape the
registry already uses for `mpm_work` and `mpm_memory`. The actions are the
ones an agent may safely perform:

`inbox`, `show`, `history`, `deliver`, `acknowledge`, `accept`, `refuse`,
`claim`, `renew`, `start`, `complete`, `delegate`, `cancel`, `note`,
`record-review`.

**`create-root` and `record-approval` are absent.** A test enumerates the
registry and fails if any action can create a root or record an approval.

---

## 5. The acceptance tests

The suite proves behaviour, not source text. The negative controls **break
behaviour** and observe that the suite notices — they never merely notice
that a hash or a string changed.

### 5.1 The ten properties, and the tests that carry them

| # | property | the tests |
|---|---|---|
| 1 | a valid operator-created root succeeds; an agent-facing root-creation attempt is refused | `TestAPIRootCreationRequiresTheOperatorAuthority`, `TestAPIRootCreationSucceeds`, `TestAPIRootCreationValidatesItsInputs` |
| 2 | a bounded child is created; an invalid recipient, excess depth and a terminal parent are refused **without partial writes** | `TestAPIBoundedChildCreation`, `TestAPIChildOutsideTheRootAllowListIsRefused`, `TestAPIChildBeyondTheRootDepthIsRefused`, `TestAPIDelegationFromATerminalParentIsRefused` |
| 3 | duplicate creation is idempotent, **including competing concurrent requests** and the schedule in which the duplicate is not yet visible | `TestAPIDuplicateCreationIsIdempotent`, `TestAPIDuplicateCreationWithADifferentAssignmentIsAConflict`, `TestAPITheDuplicateIsResolvedWhenTheLookupCannotSeeItYet`, `TestAPIConcurrentDuplicateCreationResolvesToOneAssignment` |
| 4 | two simultaneous claims cannot both become the valid owner, and a claim presenting a stale epoch is refused as stale | `TestAPITwoSimultaneousClaimsProduceOneOwner`, `TestAPIClaimPresentingAStaleEpochIsRefused` |
| 5 | an expired lease can be reclaimed, and the previous holder's epoch is fenced out | `TestAPIAnExpiredLeaseIsReclaimableAndThePreviousHolderIsFencedOut`, `TestAPIALiveLeaseIsNotStealable`, `TestAPIRenewalAdvancesTheEpoch`, `TestAPIOnlyTheHolderMayRenew`, `TestAPITerminalAssignmentsAreNotClaimable` |
| 6 | every transition writes exactly one corresponding durable event atomically, **including injected failure and rollback** | `TestAPIEveryTransitionWritesExactlyOneEvent`, `TestAPIAFailedTransitionRollsBackItsEvent`, `TestAPIAFailedCreationRollsBackItsEvent` |
| 7 | delivery, acknowledgement, acceptance and completion remain distinct | `TestAPIDeliveryAcknowledgementAcceptanceAndCompletionAreDistinct`, `TestAPIAnUndeliveredAssignmentCannotBeAcknowledged` |
| 8 | restarting the process preserves assignments, events, claims and recoverable work | `TestAPIRestartPreservesAssignmentsEventsAndClaims` |
| 9 | invalid state transitions, forged approval fields and attempts to bypass review are refused | `TestAPIInvalidTransitionsAreRefused`, `TestAPIForgedApprovalFieldsAreRefused`, `TestAPITheApprovalMustNameTheGateAction` |
| 10 | the independent review moves the subject, records the verdict on both ledgers, and refuses a reviewer that judges its own work | `TestAPIReviewVerdictMovesTheSubject`, `TestAPIChangesRequestedLeavesTheSubjectAndRecordsANote`, `TestAPIReviewerCannotReviewItsOwnWork`, `TestAPIReviewOfAnIncompleteSubjectIsRefused`, `TestAPIAStaleReviewerIsFencedOut` |

Plus the reads (`TestAPIInboxListsTheAddressedWork`), the authority boundary
(`internal/core/tools/assignment_operator_boundary_regression_test.go`) and
the control harness's own pinning
(`internal/core/assignment/controls_pinning_test.go`).

### 5.2 The negative controls

`scripts/continuity-001-pr2-controls.py` removes one invariant from the
**service** and re-runs the suite. PR 1's controls mutated the DDL, because
PR 1's invariants live in the database; PR 2's invariants are code, so a
control has to remove the code and observe that the suite notices.

**Fifteen controls: thirteen are load-bearing, and two measure the defence
in depth.**

| control | what it removes | the test that must then fail |
|---|---|---|
| `fence` | the fence, both halves | `TestAPIAnExpiredLeaseIsReclaimableAndThePreviousHolderIsFencedOut` |
| `fence-review` | the fence on the review path, all three halves | `TestAPIAStaleReviewerIsFencedOut` |
| `recipient-allow-list` | N2 | `TestAPIChildOutsideTheRootAllowListIsRefused` |
| `depth-budget` | N3 | `TestAPIChildBeyondTheRootDepthIsRefused` |
| `operator-authority` | N6 | `TestAPIRootCreationRequiresTheOperatorAuthority` |
| `approval-action` | the approval's action check | `TestAPITheApprovalMustNameTheGateAction` |
| `reviewer-independence` | the review's independence check | `TestAPIReviewerCannotReviewItsOwnWork` |
| `live-lease` | the live-lease refusal | `TestAPIALiveLeaseIsNotStealable` |
| `claim-cas` | the claim's compare-and-swap, all three halves | `TestAPIClaimPresentingAStaleEpochIsRefused` |
| `idempotency-lookup` | the early lookup | `TestAPIDuplicateCreationWithADifferentAssignmentIsAConflict` |
| `idempotency-readback` | the read-back after the unique index refused the insert | `TestAPITheDuplicateIsResolvedWhenTheLookupCannotSeeItYet` |
| `state-machine` | the role's path | `TestAPIDeliveryAcknowledgementAcceptanceAndCompletionAreDistinct` |
| `atomicity` | the fault seam between the transition and its event | `TestAPIAFailedTransitionRollsBackItsEvent` |
| `fence-go-half` | **only** the Go half of the fence | *none — and that is the measurement* |
| `fence-sql-half` | **only** the `WHERE` clause | *none — and that is the measurement* |

The last two are the **defence in depth**, published rather than hidden:

* removing the Go half of the fence changes nothing, because the `WHERE`
  clause still fences the write; and
* removing the `WHERE` clause changes nothing, because the Go check still
  refuses the stale epoch.

So the fence is enforced **twice**, in two independent places, and neither
alone is load-bearing. That is the property worth having, and it is why the
full removal is the control that must fail.

### 5.3 What the controls found

The controls are not decoration: the first run of the harness found two
defects in PR 2's own code and tests.

* **The `WHERE` clause was a tautology.** The transition's write was
  `WHERE id = ? AND claim_epoch = ?` — but it bound the row's **current**
  epoch, read inside the same transaction, so the clause always matched. It
  fenced nothing. The clause now binds the **presented** epoch, which is
  what makes it a fence at all; the Go check alone had been carrying the
  invariant.
* **Two tests were proving the state machine and calling it the fence.**
  The reclaim test presented a stale epoch to an assignment still in
  `enqueued`, so the state machine refused it before the fence was ever
  consulted. Both fence tests now put the assignment in a state where the
  transition is **legal**, so the refusal they observe is the fence's.

A control that removes an invariant nobody's test notices is a control that
says the suite does not test that invariant. Both of these were invisible
before the harness ran.

---


## 6. What PR 2 leaves to PRs 3–5

| | deferred to |
|---|---|
| the OpenCode adapter, the reviewer isolation, the session handoff | **PR 3** |
| the dispatcher, the recovery loop, the tree's aggregate budgets (`max_tasks`, `max_concurrency`, `max_resource_units`) | **PR 4** |
| the GitHub evidence contract, the read-only `check` discovery | **PR 5** |
| the first real assignment (#253, skills P8) | **PR 6** |

**MPM does not merge, deploy or change production state**, and no command
in this PR can.
