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
| **N2** | a child's recipient is among the root's `eligible_recipients` | `Delegate` validates it; the service never changes the root's list, so no later operation can invalidate it |
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

### 3.2a Who may delegate — and what that question cannot be answered

PR 2 does **not** require the delegator to hold the parent assignment,
and the reason is worth stating rather than leaving implicit.

MPM has no identity to check. It records who a caller *says* it is, and
§2.1 says why that is not authentication: an agent under the operator's
account can read the operator's files and run the operator's binaries.
So a rule like "only the holder of the parent may delegate it" could
not be enforced honestly — the service could only check that the caller
knows the parent's *current* epoch, which anyone who has read the
assignment also knows. That check would be theatre: it would LOOK like
a possession check and enforce nothing.

What PR 2 **does** enforce is the two bounds that are structural:

* the child's recipient is among the **root's** eligible recipients
  (N2), and
* the child's depth is within the **root's** `max_depth` (N3).

What bounds the **number** of children in a tree is the root's
`max_tasks`, and PR 2 does **not** enforce it — **PR 4** does, together
with the other aggregate budgets. Until then, the tree's size is bounded
by the depth and by nothing else, and that is stated here rather than
implied.

### 3.3 The operations

| operation | transition | who |
|---|---|---|
| `CreateRoot` | — (a new root) | operator only |
| `Delegate` | — (a new child) | agent, bounded by the root |
| `Deliver` | `enqueued → delivered` | runtime |
| `Acknowledge` | `delivered → acknowledged` | recipient |
| `Accept` | `acknowledged → accepted` | recipient |
| `Refuse` | any live state → `cancelled` | recipient |
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

**WHERE THE PRESENTED EPOCH IS NOT A LEASE HOLDER'S.** Two writes present
an epoch that is not the caller's claim about a lease it holds, and both
are stated here rather than left to be discovered:

* **The approval.** The operator is not a lease holder. It presents the
  epoch it *read* — `mpm assignment show` — and the approval lands only
  on the revision of the assignment the operator actually read. See §3.7.
* **The reviewer's write on the SUBJECT.** The subject is moved because
  the review passed, and the reviewer holds no epoch on the subject: it
  is a different assignment, in a different row. That write binds the
  subject's **own** current epoch, read in the same transaction, so the
  clause says "the subject did not change under me" rather than fencing
  a stale writer. The reviewer's **own** row is fenced properly — the
  reviewer presents its epoch, and that is the fence the stale-reviewer
  test observes.

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

**THE OTHER DIRECTION IS NOT UNCHECKED — IT IS UNREACHABLE.** The check
walks one way, so it is worth saying what the other way does. A reviewer
cannot review work it delegated itself, and the reason is the target:
the reviewer's target is fixed by the delegation that **created** it, and
a child's id is minted when the child is created. A reviewer cannot
delegate a child and then name it, because at the moment the reviewer is
created the child does not exist and its id cannot be known — the ids are
eight random bytes. The delegation that creates a reviewer therefore
cannot point it at that reviewer's own child, and the payload is
immutable afterwards (it is not in the transition's settable-column
whitelist). So the reachable cases are exactly: the subject is an
ancestor of the reviewer, which is **refused**, and the subject is
elsewhere in the tree, which is an **independent** review.

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

**THE APPROVAL IS A FENCED WRITE.** The operator presents the claim epoch
it read, and the write is `WHERE id = ? AND claim_epoch = ?` with that
value — the same rule every other state change follows, and the one
place where following it mattered most, because the approval **is** the
gate. An approval that does not match the assignment as it stands does
not land, and it leaves nothing behind: no state change and no ledger
entry.

The operator learns the epoch from `mpm assignment show`. An approval
that names the wrong epoch is refused with the epoch the assignment is
actually at, so the refusal tells the operator what to re-read.

**AN APPROVAL THAT NAMES NO EPOCH IS REFUSED SEPARATELY.** The two are
different mistakes: a wrong epoch is a claim about a revision that is no
longer there, and a missing epoch is no claim at all. The refusal for the
second names the epoch the assignment is at, so the operator can re-read
and retry — and it says which of the two happened.

The gate is always at epoch 1 or more, because `Start` requires a claim
and the gate is only reachable through `in_progress`. That is the
**service's** rule, not the schema's: the schema's transition trigger
does not require a claim for `in_progress`. So a row that reached the
gate without a claim would be a row the service did not write.

**MPM records an approval it did not manufacture.** The revision and the
evidence pointer are the operator's claims about what was authorised, and
the row says so. A change to the head revision after the approval leaves
the approval **stale**, and staleness is observable rather than implied.

**What "observable" means here.** The row carries the revision the
approval named, so a reader can compare it with the head revision and
see that they differ. MPM does **not** make that comparison itself: it
does not know what the head revision is, and asking it to would mean
asking it to authenticate the approval — which is the thing it must not
do. The comparison belongs to whoever reads the row, and PR 5's read-only
`check` is where it becomes a first-class answer.

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
                                 [--payload '{"review_target":"at-…"}']
mpm assignment delegate           --parent-id --title --objective --recipient
                                 [--role] --idempotency-key [--resource-units]
                                 [--issue-ref --pr-ref --base-sha --head-sha
                                 --reviewed-sha --findings-sha256 --evidence-ref]
                                 [--payload '{"review_target":"at-…"}']
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
                                 --epoch <n>
mpm assignment cancel             --id [--epoch <n>] [--note]
mpm assignment note               --id --note
```

`--json` is accepted by every verb and emits the service's own projection.

`create-root` and `record-approval` are **operator verbs**: they are not in
the registry, so no MCP action and no `mpm call` action reaches them.

**`--payload` CARRIES THE REVIEWER'S TARGET.** The reviewer's target is
fixed by the delegation that creates it, and that delegation is the only
place that can name it. Without a `--payload` flag the operator's own
front door could not create a reviewer at all — the authority split
would be inverted for the one part of the tree that must be created
deliberately. The flag takes a JSON object; a malformed one is an
**error**, not a silent empty object, because a caller that meant to name
a review target and spelled it wrong must not end up with an assignment
that reviews nothing.


### 4.2 The MCP / `mpm call` surface

One tool, `mpm_assignment`, with an `action` parameter — the shape the
registry already uses for `mpm_work` and `mpm_memory`. The actions are the
ones an agent may safely perform:

`inbox`, `show`, `history`, `tree`, `deliver`, `acknowledge`, `accept`,
`refuse`, `claim`, `renew`, `start`, `complete`, `delegate`, `cancel`,
`note`, `record-review`, `handoff` — **seventeen**.

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
| 7 | delivery, acknowledgement, acceptance and completion remain distinct, and a redelivery does not lose the work | `TestAPIDeliveryAcknowledgementAcceptanceAndCompletionAreDistinct`, `TestAPIAnUndeliveredAssignmentCannotBeAcknowledged`, `TestAPIRedeliveryIsRefusedAndTheWorkStaysVisible` |
| 8 | restarting the process preserves assignments, events, claims and recoverable work | `TestAPIRestartPreservesAssignmentsEventsAndClaims` |
| 9 | invalid state transitions, forged approval fields and attempts to bypass review are refused, and the approval is a fenced write — a stale epoch and an omitted epoch are refused **separately** | `TestAPIInvalidTransitionsAreRefused`, `TestAPIForgedApprovalFieldsAreRefused`, `TestAPITheApprovalMustNameTheGateAction`, `TestAPIAnApprovalPresentingAStaleEpochIsRefused` |
| 10 | the independent review moves the subject, records the verdict on both ledgers, refuses a reviewer that judges its own work, and **cannot be re-pointed** at another subject | `TestAPIReviewVerdictMovesTheSubject`, `TestAPIChangesRequestedLeavesTheSubjectAndRecordsANote`, `TestAPIReviewerCannotReviewItsOwnWork`, `TestAPIReviewOfAnIncompleteSubjectIsRefused`, `TestAPIAStaleReviewerIsFencedOut`, `TestAPIReviewersTargetIsFixedByItsDelegation` |

Plus the reads (`TestAPIInboxListsTheAddressedWork`), the authority boundary
(`internal/core/tools/assignment_operator_boundary_regression_test.go`), the
operator's own CLI path (`cmd/mpm/assignment_cmds_test.go`) and the control
harness's own pinning (`internal/core/assignment/controls_pinning_test.go`).

**`TestAPIEveryTransitionWritesExactlyOneEvent` COVERS EVERY EVENT TYPE THE
SERVICE WRITES.** That is **twelve** of the vocabulary's thirteen — the
thirteenth, `lease_expired`, is reserved and never emitted (see the event
vocabulary in `types.go`). All twelve are exercised, each on its own fixture
so that the count is about ONE transition rather than about a chain of them.
A test that covered six of them and was called "every transition" would
publish a claim the test does not make — and the event types it left out are
exactly the ones the operator's gate is made of.

### 5.2 The negative controls

`scripts/continuity-001-pr2-controls.py` removes one invariant from the
**service** and re-runs the suite. PR 1's controls mutated the DDL, because
PR 1's invariants live in the database; PR 2's invariants are code, so a
control has to remove the code and observe that the suite notices.

**Sixteen controls: fourteen are load-bearing, and two measure the defence
in depth.**

| control | what it removes | the test that must then fail |
|---|---|---|
| `fence` | the fence, both halves | `TestAPIAnExpiredLeaseIsReclaimableAndThePreviousHolderIsFencedOut` |
| `fence-review` | the fence on the review path, all three halves | `TestAPIAStaleReviewerIsFencedOut` |
| `recipient-allow-list` | N2 | `TestAPIChildOutsideTheRootAllowListIsRefused` |
| `depth-budget` | N3 | `TestAPIChildBeyondTheRootDepthIsRefused` |
| `operator-authority` | N6 | `TestAPIRootCreationRequiresTheOperatorAuthority` |
| `approval-fence` | the approval's fence, both halves | `TestAPIAnApprovalPresentingAStaleEpochIsRefused` |
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

**THE GATE ASKS TWO QUESTIONS OF EACH CONTROL.** Not only must the named
test fail — the control must also break **nothing it did not declare** it
would break. A mutation that removed the invariant *and* broke the
statement would fail the named test for the wrong reason and take the rest
of the suite with it, and that must not read as a pass. So each control
declares the extra tests its removal breaks (`fence` breaks the renewal
test, `state-machine` breaks the four tests that depend on a transition
being refused), and a failure outside the declaration refuses the control.

The last two are the **defence in depth**, published rather than hidden:

* removing the Go half of the fence changes nothing, because the `WHERE`
  clause still fences the write; and
* removing the `WHERE` clause changes nothing, because the Go check still
  refuses the stale epoch.

So the fence is enforced **twice**, in two independent places, and neither
alone is load-bearing. That is the property worth having, and it is why the
full removal is the control that must fail.

**THE HARNESS'S GATE IS A GATE.** The harness's exit code decides whether
the measurement holds, and it can fail: a load-bearing control that did not
detect its removal, and a defence-in-depth control that broke the suite,
are both refused. The harness carries its own `--selftest`, which drives its
summary with the results such a run *would* produce and requires the gate to
refuse them, and `TestControls_TheHarnessGateCanFail` runs that self-test.
A harness whose exit code cannot fail would still print "14/14 controls are
load-bearing" after every load-bearing control had stopped noticing
anything, and the published count would be unbacked by anything that could
contradict it.

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

### 5.4 What the independent review found

The first independent review found **no hole** in the four deferred
invariants — N1, N2, N3 and N6 hold as implemented, the authority boundary
is structural rather than flag-based, and the fence binds the *presented*
epoch on every path the PR claims it fences. It found nine defects in the
**evidence artefacts** and in the **published contract's accuracy**, and all
nine are fixed here.

| the finding | what it was | what changed |
|---|---|---|
| the harness's exit code was a tautology | `load_bearing == total - redundant` reduces to `load_bearing == load_bearing`, so the harness returned 0 whatever it observed | the gate compares the pass count against the number of controls it ran, and a defence-in-depth control that breaks the suite is a failed measurement |
| four mutators corrupted the statement they edited | they removed `AND claim_epoch = ?` and left the bound argument behind, so the write became a bind error instead of an unfenced write | each mutator that removes the predicate removes the value it binds too, so `fence-sql-half` now measures the clause alone |
| the approval's `WHERE` bound the row's **current** epoch | the same tautology the transition had, in the one write the note claimed was fenced | the approval presents the epoch the operator read, and binds that |
| N2's "every operation re-validates it" did not exist | the root's allow-list is read in exactly one place | the note says what is true: the list is immutable, so the delegation's check cannot be invalidated later |
| `TestAssignment_TheAgentSurfaceStillDoesItsWork` proved the opposite of its name | it delegated to a nonexistent parent and required an error, so a handler that refused everything would pass | it now delegates and records a review verdict **successfully** through the agent-facing handler |
| the boundary test's header framed a registry boundary as a capability boundary | it omitted the limitation the note and the SPEC both state plainly | the header now says what the boundary is and what it is not |
| the operator's CLI could not express the review mechanism | the reviewer's target is fixed by the delegation's payload, and the CLI had no `--payload` | `--payload` carries a JSON object, and a malformed one is an error |
| the operations table published a narrower `Refuse` than the code implements | the note said `acknowledged → cancelled`; the code allows any live state, deliberately | the table matches the code |
| the agent-facing action list named fifteen of the seventeen | `tree` and `handoff` were missing | the list names all seventeen |

Two nits were also fixed: the `lease_expired` event type is now documented
as **reserved** (the schema's vocabulary is frozen at thirteen, and a reclaim
is recorded as `claimed` with `reclaimed: true` rather than as a second
event), and `ApprovalParams.Actor` — a field nothing read — was removed
rather than left as a field that silently does nothing.

### 5.5 What the second review found, and what the full gate found

The second independent review confirmed that **all nine** of the first
round's findings were genuinely fixed — the mutators were applied and
counted by hand, the gate was driven directly, and the central claim was
re-checked against the whole registry. It found three more defects and
five nits, and all eight are fixed here.

| the finding | what it was | what changed |
|---|---|---|
| the gate did not enforce "no unexpected failures" for a load-bearing control | it asked only whether the named test failed, so a mutation that removed the invariant **and** broke the statement would read as a pass | each control now **declares** the extra tests its removal breaks, and the gate refuses anything outside the declaration |
| the M3 fix introduced a self-contradicting sentence | a heading said "the two places where the caller presents no epoch" and the next clause said the caller **does** present one | both documents now say "where the presented epoch is not a lease holder's" — which is what the code does |
| the CLI's stale-epoch test never reached the fence | the fixture was at `enqueued`, and the state check precedes the fence, so the refusal was the state check's | the fixture is now AT the gate, and the test asserts the refusal is the fence's and that nothing moved |
| "every event type" was still not true | the service writes **twelve** of the vocabulary's thirteen (`lease_expired` is reserved), and `created` was not asserted by that test | the test now asserts the creation's event too, and the wording says twelve |
| the `FILES` tuple had no mechanical link to the controls' targets | a control mutating a file outside it would leave the tree mutated and un-restored | the mutators now **refuse** a path that is not in `FILES` |
| "the root's list is immutable" read as a schema claim | the schema does not freeze `eligible_recipients`; the **service** never changes it | the wording says "the service never changes the root's list" |
| an omitted `--epoch` was silently 0 | the operator's omission and a stale epoch produced the same kind of refusal | the two are refused **separately**, and the refusal names the epoch the assignment is at |
| a dead `if` with an empty body survived in the boundary test | its comment read as if it filtered something, and it did nothing | removed, with the now-unused import |

**THE FULL TEST GATE FOUND THREE MORE GAPS.** The `make test` gate had
never reached them, because the `cmd/mpm` package failed first and the
chain stopped. They are all consequences of adding a tool to the
registry, and all three are fixed:

* **The reviewer's deny list.** The reviewer definition is
  default-deny, and the test that pins it requires every registry tool to
  be denied **by name** — "a rule that does not name a route is not a
  closure". `mpm_assignment` is now in that list. This is PR 3's
  territory, but the contract is enforced here, and a red gate is a red
  gate.
* **The tool-reference stability contract.** Every registered tool needs
  a row, or an agent reading the contract cannot discover its actions.
  `mpm_assignment` now has one, listing all seventeen actions.
* **The contract's reader could not name a hyphenated action.** The
  registry publishes `record-review`, and the reader's character class
  had no hyphen — so such a tool was **undocumentable**: the row would
  have to omit the action, and the equality check would then fail
  forever. The class now includes the hyphen. The guard's intent is
  unchanged; only its ability to read the registry's vocabulary is
  fixed.
* **The DB-safety guard.** The assignment's restart test called
  `NewDatabaseManager("")`, which falls through to
  `config.GetMPMDir()` — the live workspace. The guard is a static scan,
  so it cannot see the `MPM_WORKSPACE` the test sets. The test now
  passes the path explicitly, which genuinely redirects the database.

**THE UNRELATED REGENERATION DELTA.** `docs/SPEC.md`'s Command Catalogue is
generated by `cmd/gen-cli` from `cmd/mpm/router.go`, and this PR's
regeneration picked up one row that had gone stale on `main`: the `export`
row now reads "Export memories to JSON (structured object with the package
loss marker)", which is what the router has said for some time. It is
correct, it is generated, and it has nothing to do with the assignment API —
but it is in the diff, so it is named here rather than left to be
discovered.

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
