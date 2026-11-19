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

**THE GATE ASKS THREE QUESTIONS OF EACH CONTROL.**

1. The named test must fail.
2. The control must break **nothing it did not declare** it would break. A
   mutation that removed the invariant *and* broke the statement would fail
   the named test for the wrong reason and take the rest of the suite with
   it, and that must not read as a pass. So each control declares the extra
   tests its removal breaks — `fence` breaks the renewal test, whose LAST
   step is a `Start` presenting the **pre-renewal** epoch, which the
   transition's fence is what refuses; `state-machine` breaks the four
   tests that depend on a transition being refused, because the path *is*
   the state machine — and a failure outside the declaration refuses the
   control.
3. The control must not have broken **more than a third of the baseline**. A
   mutation that took the suite with it did not remove an invariant, it
   broke the build, and the named test may have failed for that reason
   instead. Such a control is **inconclusive**, and an inconclusive control
   does not count as load-bearing.

The third question's denominator is the **baseline** — the number of tests
the suite runs when nothing is mutated, measured once before the controls
run. It cannot be "the tests that ran in this control's own run": a
mutation that **aborts** the suite — a panic turns a guard into a nil
dereference and the test binary dies — shrinks both sides of the
comparison, so the bound would get weaker at exactly the moment it must
be strongest. A control whose run produced fewer tests than the baseline
aborted, and is inconclusive for that reason too.

The boundary case is stated rather than left implicit: **exactly** a third
is accepted, and more than a third is refused.

The comparison is **two-sided**: a run that produced FEWER tests than the
baseline aborted the suite, and a run that produced MORE is not a run of
this suite — neither reads as a measurement. The **skips** are watched by
**identity** rather than by count, because a skipped test did not run: it
neither passed nor failed, so counting only PASS-or-FAIL would make a skip
invisible to both sides of the bound — and a count cannot tell "the same
test skipped" from "a different test skipped", of which only the first is
innocent. A control whose run skipped where the baseline did not is
inconclusive.

And the baseline itself must be **fully green**: a baseline in which any
test skipped is refused, because a skipped test cannot be part of the
denominator. (With the baseline refusing to skip, the identity comparison
reduces to "the control skipped nothing" — but it is kept in the identity
form, because the count form is the one that would fail silently.)

And the baseline is **required**: an earlier version defaulted it to zero,
which silently restored the very bound the baseline was introduced to fix,
so a caller that forgot it got the vulnerable measurement and a green exit
code. A missing baseline now fails **closed** — no denominator, no
measurement.

The third question is what stops the declaration from becoming an escape
hatch: however broad a control's `breaks` list is, a mutation that broke
the suite is refused rather than accepted. The exemptions are also
**printed** by the harness, so they are part of the published measurement
rather than hidden in its source, and the pinning test checks that every
declared exemption names a test that **exists** — a stale exemption matches
nothing, and nobody notices.

**THE DECLARED EXEMPTIONS.** Two controls have any, and both are printed by
the harness on every run:

| control | it also breaks | because |
|---|---|---|
| `fence` | `TestAPIRenewalAdvancesTheEpoch` | the renewal's LAST step is a `Start` presenting the **pre-renewal** epoch, and the transition's fence is what refuses it. The renewal itself still advances the epoch — `Renew` has its own compare-and-swap in `claim.go`, which this control does not touch |
| `state-machine` | `TestAPIAnUndeliveredAssignmentCannotBeAcknowledged`, `TestAPIInvalidTransitionsAreRefused`, `TestAPIRedeliveryIsRefusedAndTheWorkStaysVisible`, `TestAPITheApprovalMustNameTheGateAction` | the path *is* the state machine, so removing it breaks every test that depends on a transition being refused |

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

### 5.6 What the fourth review found

The fourth round confirmed that **both** of the third round's findings were
genuinely fixed — the reviewer enumerated every epoch-binding write in the
service rather than trusting the count, drove the gate with the exact
attack it had described, and patched `summarise()` to prove that the
subprocess-pinning had not blunted the oracle. It found one more defect
and four nits, and all five are fixed here.

| the finding | what it was | the fix |
|---|---|---|
| the bound's denominator was "the tests that ran" | a mutation that **aborts** the suite — a panic kills the test binary, and the other tests never run — shrinks both sides of the comparison, so the bound got **weaker** at exactly the moment it must be strongest. The reviewer demonstrated it with a stdlib-only Go probe | the denominator is now the **baseline**: the number of tests the suite runs when nothing is mutated, measured once before the controls run. A control whose run produced fewer tests than the baseline aborted, and is inconclusive |
| exactly a third was still accepted | by design, but unstated | the boundary case is now stated in the note and in the SPEC |
| the new pinning test ended with `_ = seen` | it passed vacuously if every declaration was removed | it now fails when there are none, like its sibling |
| two INCONCLUSIVE names were dead | they name tests in another package, which the `-run TestAPI` selection never runs | removed — a dead entry in an exclusion list reads as "this failure is tolerated" for something that can never happen |
| the stated reason for the `fence` exemption was **wrong** | the renewal DOES advance the epoch — `Renew` has its own compare-and-swap in `claim.go`, which the control does not touch. What breaks is the renewal test's **last** step: a `Start` presenting the pre-renewal epoch, which the transition's fence refuses | the harness's comment, the note's table and the SPEC now say what actually happens |

### 5.7 What the fifth review found

The fifth round confirmed that M15 was genuinely fixed — it **proved** the
baseline is taken on an unmutated tree by digesting the tree on every call
to `run_tests()` and comparing the first call against the pristine digest,
showed that a non-green baseline refuses to measure and restores the tree
without running a single control, and drove the abort, the timeout and the
"exactly a third" boundary itself. It found one more defect and four nits,
and all five are fixed here.

| the finding | what it was | the fix |
|---|---|---|
| the note contradicted itself about the `fence` exemption | round four fixed the harness's comment and the note's **table**, but the note's **item 2** still carried the old reason — 34 lines away, saying the opposite | the note's item 2 now says what actually breaks: the renewal test's **last** step, a `Start` presenting the pre-renewal epoch |
| the baseline was **opt-out-able** | `summarise(results, baseline=0)` silently restored the very bound the baseline was introduced to fix: the reviewer drove the abort with the default and it was **ACCEPTED** | the parameter is **required**, and a zero baseline **fails closed** — no denominator, no measurement. The self-test covers both the zero and the omitted call |
| the bound was one-sided | a run with **more** tests than the baseline was never questioned — unreachable, but unstated | the comparison is **two-sided**: a run that produced a different number of tests than the baseline is inconclusive |
| a skip was invisible to both sides | both counts were PASS-or-FAIL only, so a test that skipped — which did not run, and so neither passed nor failed — was invisible | the skips are tracked on **both** sides, and a control whose run skipped where the baseline did not is inconclusive |
| the note said "a third of the **suite**" where the SPEC said "of the **baseline**" | and the claim that the boundary was stated in the SPEC was false — "exactly a third" occurred **zero** times in `docs/SPEC.md` | both documents name the same denominator, and the SPEC now states the boundary case |

The self-test grew to **twelve** cases: the published measurement, the
control that stopped detecting, the defence-in-depth control that broke the
suite, the undeclared failure, the whole suite taken along, the aborted
suite, the skipped test, the run that produced more tests than the
baseline, the defence-in-depth control that aborted, the zero baseline, the
empty corner, and the call with no baseline at all.

### 5.8 The CLI's list-valued flags, found by the operator's own smoke test

The final end-to-end smoke test — the operator's path, from `create-root`
through the review, the handoff and the approval, against a disposable
workspace — found a defect that no unit test had: **a repeated
list-valued flag silently lost its earlier values.**

Go's `flag` package overwrites a string flag's value on every `Set`, so
the obvious way to write a root's permitted operations —

    --permitted-operation deliver --permitted-operation acknowledge \
    --permitted-operation accept …

— silently kept only the **last** one. The root was created permitting a
single operation, and the work order could not be delivered. The help
page did not mention the flag at all, so the operator had no way to know
the comma-separated form was the expected one.

The failure was in the **safe direction** — a lost value narrows what the
root permits and never widens it — but a silent narrowing is still a
defect: the operator asked for six permitted operations and got one, with
no error and no way to tell.

So the list flags **accumulate**: the comma-separated form and the
repeated form both work, and neither loses the other's values. The help
page now names the three list-valued flags and both forms.

The option registry's drift guard caught the change immediately, and
correctly: it derives the option vocabulary from the source, and it read
the name as the **first** argument of every registration — which is true
of `fs.String` and false of `fs.Var`, where the value comes first. The
guard's collector now knows the difference.

### 5.9 What the sixth review found

The sixth round confirmed that **every** one of the fifth round's findings
was genuinely fixed, and it did not take the notes' word for it: it
verified the `--- SKIP:` regex against Go's own output with a stdlib-only
probe, it enumerated the registry's action values itself, and it **mutated
the gate eight ways** to ask which mutations the self-test catches. It
found one more defect and five nits.

| the finding | what it was | the fix |
|---|---|---|
| the two-sided comparison was **published but not pinned** | the SPEC states the comparison is two-sided, and the code does it — but no self-test case exercised the *more* direction, so a regression to one-sided would pass the self-test, pass `TestControls_TheHarnessGateCanFail`, and contradict a published sentence, silently, with a green exit code. The reviewer **proved** it by mutation | the self-test gained a case for the *more* direction, and the mutant is now caught |
| the defence-in-depth branch's new checks were **unpinned** | removing `total == baseline and skipped <= baseline_skipped` left the self-test at exit 0 | the self-test gained a case: a defence-in-depth control whose run **aborted** — a different number of tests, with no failure to catch it |
| the explicit zero-baseline guard was **unpinned in isolation** | the two-sided bound already refuses a zero baseline, so the case pinned the *outcome* rather than the *guard* | the self-test gained the corner where only the guard refuses: no baseline **and** no results at all |
| the INCONCLUSIVE message was wrong for one direction | it said "the mutation ABORTED the suite" for the **more**-tests direction, which is a different explanation | the two directions now have their own messages, and they say what is actually wrong |
| the baseline's health was checked for failures but not for **skips** | a baseline in which most tests skipped would have been accepted as the denominator — and since the total counts a skip as if it had run, a control that skipped the same tests would have matched it exactly | a baseline that skips **anything** is refused: a skipped test cannot be part of the denominator |
| the self-test dispatched on a case's **display string** | the name was both the label and the switch that decided how the gate was driven, so renaming a case would have silently changed what the self-test proves | each case carries a **mode**, and the name is only the label |
| `skipped` was a **count**, not an identity | a control that swapped *which* test skipped would not have been flagged, and the defence-in-depth branch's `skipped <= baseline_skipped` would still have called it clean | the skip comparison is by **identity** on both branches: only the *same* test skipping is innocent |

**THE SELF-TEST IS NOW TWELVE CASES**, and each new gate mechanism has a
case that fails when the mechanism is removed. That was verified by
mutation rather than asserted: the one-sided mutant, the removal of the
defence-in-depth checks, the removal of the zero-baseline guard and the
removal of the skip check are each **caught** by the self-test.

**THE CLAUDE CODE DRILL WAS FLAKY.** The full gate turned up one failure
that had nothing to do with this PR: the Claude Code end-to-end drill
failed with a timeout in an environment whose harness model the `claude`
CLI rejects. The drill already has a skip gate for exactly that condition
— but the gate sat **after** the `Finish` call, so it only held when the
child happened to finish in time, and the same environment failed
intermittently. The failure path now consults the same gate. The warning
is emitted at startup, so it is already in the harness's debug buffer when
the child hangs.

**THE UNRELATED REGENERATION DELTA.** `docs/SPEC.md`'s Command Catalogue is
generated by `cmd/gen-cli` from `cmd/mpm/router.go`, and this PR's
regeneration picked up one row that had gone stale on `main`: the `export`
row now reads "Export memories to JSON (structured object with the package
loss marker)", which is what the router has said for some time. It is
correct, it is generated, and it has nothing to do with the assignment API —
but it is in the diff, so it is named here rather than left to be
discovered.

---


### 5.10 What the seventh review found

The seventh round did the thing this review had been asking for: it
**mutated the gate fifteen times** and asked which mutations the
self-test catches. It found that **six mechanisms were still unpinned**,
including the gate's own **first** question.

| the finding | what it was | the fix |
|---|---|---|
| the `named` clause was **unpinned** | it is the gate's FIRST question — "the named test must fail" — and the self-test's "did not detect its removal" case ALSO trips the "unexpected failures" clause, so removing `named` left the self-test green. A direct drive showed the mutant reporting **14/14 load-bearing at exit 0** on a run where a control detected nothing | the self-test gained a case that detects **nothing and breaks nothing**, so only the `named` clause can refuse it |
| the identity comparison could revert to a **count** | the SPEC and this note both make a point of the identity form, and the note says the count form "is the one that would fail silently" — which is exactly the regression nothing detected | the self-test gained a case where the run skipped a **different** test than the baseline did: the same count, a different identity |
| the defence-in-depth branch's **skip clause** was unpinned on its own | removing only that clause left the self-test green, and it flipped a refusal into an acceptance | the self-test gained a case where the defence-in-depth control's run skipped a test — nothing failed, so the skip clause is all that can catch it |
| `main()`'s two **baseline guards** had no test at all | the self-test drives `summarise()`, which `main()` never reaches — so removing either guard left it green. One of those guards **is** round six's N23 fix | the baseline decision is now a **function** (`baseline_verdict`), and the self-test drives it: a green baseline is accepted, a baseline that is not green is refused, and a baseline that skipped a test is refused |
| the "more tests" INCONCLUSIVE **message** was unpinned | a message, not a verdict — but it is published, and it was M17's own fix | the self-test now **captures** the gate's output and asserts the message says the run is not a run of this suite |
| the `Result` docstring still described the old count semantics | N25 changed `skipped` from a count to a list of names, and the field's own comment still said "the number of those" | the comment says what the field is |
| two stale **"13/13"** strings | the sentence both files use to argue that the gate can fail understated the number of controls that would have to stop noticing | they say 14/14 |
| the drift guard's option map named **`VarX`** | there is no `VarX` in Go's `flag` package, and the map's comment said "Both put the name first" — which the map itself exists to correct | the map is the stdlib's own set, the `*Var` family is included with the right argument position, and the comment says what is true |
| the round-six table omitted **N25** | the table is the only record of what round six changed, and it listed five of six | the sixth row is there |

**THE MUTATION MATRIX IS THE EVIDENCE.** Sixteen behaviour-changing
mutants of the gate, and every one of them is caught by the self-test —
verified by applying each to a scratch copy and running the self-test
against it. That is a stronger claim than "the self-test has many
cases", because it is a claim about the cases that do not exist: the
matrix says which mechanisms would break the gate silently.

**AND WHAT THE MATRIX DOES NOT SAY.** The matrix is not a proof that no
mutation can escape, and it says so in its own words. One mutation is
**not a behaviour change** and so cannot be caught: the `^` anchor on the
harness's **FAIL** pattern. Go reports a parent test's own result on a
top-level line, and a parent whose subtest failed is itself reported as
`--- FAIL:` — so the parent's name is in the set whether the anchor is
there or not, and removing the anchor changes nothing observable.

The **SKIP** and **PASS** anchors are both load-bearing, and both are
caught. The SKIP anchor is what stops a **passing** parent whose subtest
skipped from being counted as skipped. The PASS anchor is what stops a
**failing** parent whose subtest passed from being counted as passed —
and the eighth review was right that the note's exemption of it was
false: it held only for a *passing* parent, whose name the top-level
line already carries. The sample of Go's output now carries **both**
shapes, so the failure is caught rather than argued about.

The matrix is a list of **single-edit** mutations. A two-edit mutation
that removes a mechanism *and* the check that watches it is outside it,
and the note says so rather than claiming more.

### 5.11 What the eighth review found

The eighth round **verified round seven's fixes by mutation** — all
fifteen of the author's mutants caught, plus ten more it constructed —
and then confirmed that the new cases are genuinely load-bearing with a
**double-knockout** test: dropping `detected_nothing` *and* removing
`named` is green; dropping `swapped_skip_lb` *and* reverting the
identity comparison is green. So M2 and M3's fixes are correctly pinned.

But it found three mechanisms that round seven had **added** and left
unpinned, and one case that claimed to isolate a mechanism it did not.

| the finding | what it was | the fix |
|---|---|---|
| the `redundant_skipped` case **leaked** into every control after the one it targeted | `baseline_skips` is the loop variable every control's skip set is derived from, and the case assigned to it **inside** the loop — so thirteen bystanders inherited the change and refused for the wrong reason. The verdict was the same either way, so nothing noticed | the assignment is gone, and the case's construction is asserted: a case that isolates a target must refuse **exactly one** control |
| the "every control carries the baseline's skips" **default** was unpinned | removing it *together with* the identity comparison defeated the identity comparison entirely — an escape from two half-mutations at once | the self-test gained a case where every control skips exactly what the baseline skipped, and the run is **accepted**; remove the default and every control differs from the baseline and the case fails |
| the published-message assertion dispatched on the case's **display string** | round six's N24 was exactly that defect, round seven **re-introduced** it, and the compound — renaming the case *and* reverting the message — was green, so the wrong message could be published with a green exit code | the dispatch is on the case's **mode**, and the assertion is **counted**, so removing the assertion's dispatch fails the self-test |
| `main()` still ignored the baseline verdict | round seven's fix moved the guards into a testable function, but the **call site** was still driven by nothing: a regression that made `main()` proceed on a not-green baseline would print a green gate and exit 0 | the verdict now **raises** rather than returning, so a caller cannot forget to look at a return value but cannot ignore an exception; and the self-test asserts that `main()` calls it |
| the baseline's **denominator** was unpinned | the self-test asserted the function's boolean verdict but never its returned denominator — and a `+1` makes every control's run look short of the baseline, so every control becomes inconclusive and the gate refuses everything | the denominator is the number of tests that ran and passed, and the self-test asserts the value |
| the three output **regexes** were unpinned | they are the harness's entire contact with Go's output, and the `^` anchor is what stops a **passing** parent test being counted as skipped | the three patterns are named constants, and the self-test reads a sample of Go's real output — including an indented subtest skip and an indented subtest failure — and asserts what each pattern reads |
| the note published the matrix as a **complete proof** | and it is not complete: two mutations are not behaviour changes and cannot be caught | the note says what the matrix does not say, and names the two |
| the published case count counted **output lines** | not cases — the message assertion and the "no baseline at all" check are assertions *within* a case | the self-test prints its own case count, and the note publishes that |
| a **duplicated heading** | a stray ALL-CAPS line at the end of §5.10, left over from the renumbering | removed |

**THE SELF-TEST IS NOW TWENTY-THREE CASES** (and forty-three after the
last round), and the matrix grew with it: the round-eight mutants are
all caught too, including the three compounds — the identity comparison
with the default removed, the case renamed *and* the message reverted,
and the default removed with the defence-in-depth branch's identity
comparison.

### 5.12 What the ninth review found

The ninth round **verified round eight's fixes by mutation** and found
the fix for the leak to be the strongest result of the whole exercise:
re-introducing the leak is caught by **eleven** cases, so it is not a
one-case accident. It also confirmed the service is **byte-identical** to
the state round seven reviewed — the last two rounds touched only the
harness, the tests and the documents.

But it found three more defects, and one of them was a **false claim of
this note's own**.

| the finding | what it was | the fix |
|---|---|---|
| the note's exemption of the **PASS** anchor was **false** | the justification held only for a *passing* parent. For a **failing** parent — the standard table-driven shape — Go reports `--- FAIL: TestX` on the top level and the passing subtest's result INDENTED, so the unanchored PASS pattern counts a test that **failed**. On a realistic run the harness's own arithmetic then makes every control's total 37 against a baseline of 36: inconclusive, and one of the gate's three questions masked | the sample of Go's output now carries **both** shapes — a passing parent whose subtest skipped, and a failing parent whose subtest passed — and the PASS anchor is caught |
| `main()`'s wiring was checked by a **source search** | `inspect.getsource(main)` plus a substring, so a **comment** satisfied it, and `main()` could swallow the raise that was introduced precisely so it could not be ignored | the wiring is checked **behaviourally**: the self-test drives `main()` with a **stubbed** suite and asserts that a not-green baseline and a skipping baseline both **refuse** the measurement, while a green baseline proceeds to the controls |
| the SPEC published "each removal must make the self-test fail" | which is not true — one mutation is not a behaviour change | the SPEC now says **each removal that changes the gate's behaviour** |
| the "assertion never ran" guards were themselves unpinned | each mechanism died in two edits: remove the assertion and its guard | the two guards are **gone**, and the **count** is the one mechanism: it is derived from the counters, so an assertion that stops running changes the count, and the count is checked against the published literal |
| the published case count was printed but not asserted | a printed number nobody checks drifts | the self-test checks its own count against a published literal, and a **Go test** checks the harness's *printed* count against the same figure — so adding a case without publishing it fails the suite in two places |
| the SKIP anchor's catchability rested on an unpinned sample line | removing the line from the sample would make the anchor unobservable | the sample's load-bearing shapes are **asserted** |
| the mode dispatch was protected by a counter rather than by itself | the same shape as the guard above | the count is derived from the counter, so the dispatch is pinned by the count |

**THE SELF-TEST IS NOW FORTY-THREE CASES** (and seventy-four after the
twelfth round — §5.16), and the number is published in two places that
check each other: the harness's own literal, and the Go test that
compares the harness's printed count.

### 5.13 What the tenth review found, and where the matrix lives

The tenth review applied **sixty-five** mutations to the harness and
found that **fourteen** left the self-test green — and that four of them
were single-edit behaviour changes. It also said plainly that the PR was
ready for the merge gate: none of its findings affects the gate's
behaviour as it stands, they are about whether the gate would *notice*.

| the finding | what it was | the fix |
|---|---|---|
| the count pinned the **presence** of an assertion's dispatch, not the assertion | the counter incremented before the assertion ran, so removing an assertion's **body** while keeping the counter left the count unchanged — and the wrong message could be published with a green exit code | the count is the **LOG** (§5.14): every check records that it ran, so a check that stops running leaves the count short |
| the drive captured `main()`'s exit code and **never read it** | a refusal that exits 0 is a green answer to a measurement that was refused — and the harness's exit code **is** the gate | the exit code is checked, and a refusal must exit 1 |
| the behavioural drive was pinned by **nothing** | removing it left the self-test green, which voided the whole of round nine's F1 fix | the drive's verdict is a **function**, and the self-test proves the function **can fail** by driving it with a deliberately wrong expectation |
| the SPEC published "each removal must make the self-test fail" | still false for the four above | the SPEC says what the matrix says, and the matrix now lives where it can be exhaustive |

**THE MATRIX MOVED OUT OF THE HARNESS.** The last four holes share one
shape: a mutation that **neuters** a check cannot be caught by that
check, and the harness has no observer left — the check *is* the thing
being neutered. So the matrix is now a **Go test**:

    TestControls_TheHarnessFailsWhenAMechanismIsRemoved

It copies the harness to a temporary directory (never the tree), applies
one mutation at a time, and requires the harness's own self-test to
**FAIL** against the copy. **Twenty-seven mutations, every one caught.**
The matrix is an **independent observer**: the harness is only its
subject, so it cannot influence what the matrix requires.

That is the difference between a self-report and a measurement. The
harness can say "every mechanism has a case behind it"; only something
outside it can say "and here is the proof, one neutered mechanism at a
time".

### 5.14 The log and the predicates: how a neutered check is seen

Closing the tenth round's findings turned up one more class, and it is
worth stating separately because it is the class that **any** self-test
has.

**THE CLASS.** A check inside a program can be **neutered** by editing
that program — `if not _message_ok(out):` becomes `if False:` — and the
program cannot see it. The check *is* the thing being neutered, so
there is no observer left inside. Worse, the usual counters make it
worse: a counter that increments in the check's **success** branch still
increments when the check is neutered, so the count is unchanged and the
evidence looks intact.

**THE ANSWER HAS TWO PARTS.**

**Part one: every check is a PREDICATE, and every predicate is proved
able to fail.** The five predicates are `_equal`, `_message_ok`,
`_isolation_ok`, `_sample_ok` and `_wiring_ok`. Each is a function, so
the self-test can drive it with a **deliberately wrong value** and
require it to refuse. That is what makes a neutered predicate's *body*
visible: the probe drives the predicate directly, and a predicate that
always says yes fails its own probe.

**Part two: the count is a LOG, and the predicates write it.** Every
predicate **appends to a log the caller owns** as it runs. The self-test's
count is the log's **length** — the number of checks that were
*evaluated*, not the number of lines that were printed and not a number a
branch increments on its way past. Three things follow, and each is a
mechanism:

| what is mutated | what happens |
|---|---|
| a check **stops running** | it does not log, so the log is short, so the count changes, so the count's check fires |
| a check's **call site** is neutered | the predicate is not called, so the check does not log, so the count changes |
| a predicate's **body** is neutered | it still logs, so the count is unchanged — and the **probe** catches it |

The count's own check is pinned from **outside**: the Go test mutates
the **published literal** in a copy and requires the self-test to fail,
so a copy whose count check has been disabled passes and the Go test
says so by name.

**THE COUNT IS EIGHTY-TWO CHECKS**, published in two places that check
each other: the harness's own literal, and the Go test that compares the
harness's *printed* count against the same figure.

**THE MATRICES ARE THIRTY-EIGHT AND FOUR ENTRIES** (§5.19).

**THE FOUR SUITES HOLD SIXTY-NINE TEST FUNCTIONS**, and that figure is
computed from the tree rather than copied into it: the Go pin counts the
test functions in the four suites and requires the count to be the
published one, so a suite that lost a test cannot leave the number
behind. The three checks that
are not predicate calls — the baseline verdict, the three output
patterns and the call with no baseline at all — are counted in the same
figure, so a case added without the published figure being updated fails
the suite in two places.

**WHAT THE MATRIX STILL DOES NOT SAY.** Two things, stated rather than
claimed away. The `^` anchor on the **FAIL** pattern is not a behaviour
change — Go reports a parent whose subtest failed as `--- FAIL:` on a
top-level line, so the parent's name is in the set either way. And the
matrix is a list of **single-edit** mutations: a two-edit mutation that
removes a mechanism *and* the check that watches it is outside it.

### 5.15 What the eleventh review found: the gate that nothing ran

The eleventh review asked the question the tenth round's design had
not: **is the harness's own gate exercised by anything at all?** The
answer was **no**, and the answer was worse than it looked.

Every invocation of the harness in the repository used `--selftest`,
which returns before `main()` is ever called. **No CI workflow and no
Makefile target ran the harness at all.** So nine single-edit mutations
of `main()`'s verdict path left every test in the tree green — the
reviewer confirmed it by simulating the Go matrix's external observer
against each one, and the matrix caught **none** of them.

| the finding | what it was | the fix |
|---|---|---|
| the harness's gate was exercised by **nothing** | `--selftest` returns before `main()` is called, and no workflow or make target ran the harness | `make test-continuity-controls` exists, and CI's gate job runs it as its own step |
| `main()`'s per-control arithmetic was **inline** | `hit` and `total` were computed in the loop, and nothing but the real controls exercised them — and the real controls were not in anyone's test run | `_measure()` is a **function**, and a **probe** drives it with a known control and a known run: the control declares two tests, the run fails one, and the measurement must be the one that failed |
| `main()`'s **return value was thrown away** | the drive called `main()` and kept the exit code, so a gate that accepted whatever it was given was invisible | the drive observes the **gate's verdict**, with a stub whose control **breaks the gate** — so the gate must refuse and `main()` must return 1 |
| the **process's** exit code was unobserved | `sys.exit(main())` is at module level, so nothing in the tree could see it | `--selfcheck-gate` stubs the suite and the controls and then goes through `sys.exit(main())`, and a Go test requires a **non-zero** exit |

**TWO MATRICES, FOR TWO SUBJECTS.** The self-test's matrix covers the
**summary's** mechanisms — the gate's three questions, the patterns,
the bounds, the predicates and the log. The gate's matrix covers
**`main()`'s own path** — the measurement arithmetic, the verdict, the
exit code and the module-level exit. Each entry in the gate's matrix
must make the harness **ACCEPT** the measurement — exit 0 where it must
exit non-zero.

Thirty-eight entries in the first, four in the second, and every one of
them is caught.

**THE SELF-TEST WAS SEVENTY-ONE CHECKS AT THAT ROUND**, and the number is published
in two places that check each other: the harness's own literal, and the
Go test that compares the harness's *printed* count against the same
figure.

**WHAT THE MATRICES STILL DO NOT SAY.** Two things, stated rather than
claimed away. The `^` anchor on the **FAIL** pattern is not a behaviour
change — Go reports a parent whose subtest failed as `--- FAIL:` on a
top-level line, so the parent's name is in the set either way. And both
matrices are lists of **single-edit** mutations: a two-edit mutation
that removes a mechanism *and* the check that watches it is outside
them.

### 5.16 What the twelfth review found: a constant that carried a claim

The twelfth review closed the eleventh's findings by mutation — every
one of round eleven's nine survivors is now caught — and found three
more. All three share one shape: **a constant or a pin carrying a
published claim that was not itself checked.**

| the finding | what it was | the fix |
|---|---|---|
| the new CI pin asserted a **string** in the workflow's raw text | commenting the step out left the pin passing — CI would no longer run the harness and the pin would say nothing | the pin strips the **comments** first, the way its sibling checks in the same file do |
| the gate's third question — "more than a third of the baseline" — lived in `BROKEN_SUITE_FRACTION` and a `>` comparison | **two** single edits changed the published bound and nothing noticed: the only synthetic case near the bound had 37 failures, far past a third, so the **line itself** was never exercised | there is now a case landing a control **exactly at the line** — which must be ACCEPTED — and one **exactly one past it** — which must be REFUSED. The pair pins the constant and the comparison together |
| the note's "what the matrices still do not say" was silent about the bound | the framing read as though single-edit mutations were covered | the note says what the matrices cover and what they do not |

**AND THE FIRST ATTEMPT AT THE BOUND'S LINE DID NOT WORK.** The case
derived its boundary from `SUITE_SIZE // BROKEN_SUITE_FRACTION`, so a
change to the constant **moved the case with it** and pinned nothing —
the case was measuring the constant against itself. The line is now a
**LITERAL** — twelve of thirty-six is still a measurement, thirteen
broke the build — and a Go test checks the literal against the suite's
real size, so it cannot drift either.

That is the same lesson the log taught, one level up: **a case that
derives its boundary from the thing under test is not a case.**

**THE THREE NITS.**

* `INCONCLUSIVE` was **half-pinned**: losing `<BUILD FAILED>` was green,
  because the measurement probe carried only `<TIMEOUT>`. The probe now
  carries **both**, and the matrix covers both members.
* the gate mode's **tree-safety stubs** were unpinned. The Go test now
  runs it with a fresh `TMPDIR` and requires that directory to be
  **EMPTY** afterwards — so the mode cannot have snapshotted the tree —
  and requires the output to carry the gate's **refusal** rather than
  the pristine-copy error, so `restore()` cannot have run either.
* `SUITE_SIZE` was unpinned against the real suite size. A Go test now
  compares it with the number of `TestAPI` functions the acceptance
  suite actually has.

**THE MATRICES ARE NOW THIRTY-TWO AND FOUR ENTRIES**, and every one of
them is caught.

**WHAT THE MATRICES STILL DO NOT SAY.** Three things, stated rather
than claimed away. The `^` anchor on the **FAIL** pattern is not a
behaviour change — Go reports a parent whose subtest failed as
`--- FAIL:` on a top-level line, so the parent's name is in the set
either way. The matrices cover **single-edit** mutations only: a
two-edit mutation that removes a mechanism *and* the check that watches
it is outside them. And the matrices enumerate the mechanisms they
name — a constant or a pin that nothing checks is outside them, which
is exactly what the twelfth round found and fixed.

### 5.17 What the thirteenth review found: a question that asked nothing

The thirteenth review asked a question no earlier round had asked: **is
the gate's claim about itself correct?** — not whether the mechanisms
are pinned, but whether the gate's three questions can be made to ask
nothing.

And one of them could. The gate's **first** question is "did the NAMED
test fail?", and it was written as:

    named = len(hit) == len(control["expect"])

A control that names **no** test makes that `0 == 0`, which is **True**.
So the question asks nothing about a control that names nothing — and
**13 of the 14 load-bearing controls could have their `expect` list
emptied by one edit**, with nothing in the tree catching it: not the
self-test, not the gate mode, not either matrix, not the suite-size
test.

**THE DIRECTION IS THE WHOLE POINT.** A control whose `expect` was
emptied and whose removal broke **nothing** would be **ACCEPTED**: the
gate would report it as load-bearing when it detected nothing. That is
the dangerous direction — the gate certifying a control that certified
nothing.

The fix has three parts, and each is a mechanism:

| the part | what it does |
|---|---|
| the `named` clause now requires the control to **declare** a test | `named = (len(control["expect"]) > 0 and len(hit) == len(control["expect"]))` — a control that names nothing is never load-bearing, so the question can never be vacuous |
| the self-test's "the removal detected **nothing**" case now covers **every** control | the shape is what the question exists for, and a clause exercised against one control says nothing about the other thirteen. And a **new** case covers a control that names **no test at all** — the shape that made the clause vacuous |
| a Go test pins that every load-bearing control **names a test** | that is the structural statement the gate now relies on, and it is checked from outside |

**THE MATRICES ARE NOW THIRTY-THREE AND FOUR ENTRIES**, and every one of
them is caught — including the new entry that removes the non-vacuity
guard.

**WHAT THE MATRICES STILL DO NOT SAY.** Three things, stated rather than
claimed away. The `^` anchor on the **FAIL** pattern is not a behaviour
change — Go reports a parent whose subtest failed as `--- FAIL:` on a
top-level line, so the parent's name is in the set either way. The
matrices cover **single-edit** mutations only: a two-edit mutation that
removes a mechanism *and* the check that watches it is outside them.
And they enumerate the mechanisms they name — a constant, a pin, or a
**question** that nothing checks is outside them, which is exactly what
the twelfth and thirteenth rounds found and fixed.

### 5.18 What the fourteenth review found: the selection that was not pinned

The fourteenth review asked the central question in the form that found
round thirteen's finding, and found a new instance of the same class.

**THE HEADLINE: THE HARNESS'S SELECTION IS UNPINNED.** `TEST_PATTERN`
decides what the harness measures, and the bound's denominator is the
**measured** baseline. So the published line — "a third of the
baseline" — is only meaningful *relative to the selection*, and a
selection one edit wider moves it. The same thirteen failures:

```
--- the TestAPI selection: 13 failures (baseline = 36) ---
     INCONCLUSIVE: the mutation broke 13 of the baseline's 36 tests …
    >>> summarise() returned: False          <-- REFUSED

--- a widened selection: 13 failures (baseline = 48) ---
    OK  fence    N1 — a claimed write presents the current claim_epoch
    >>> summarise() returned: True           <-- ACCEPTED
```

**Refused at the published line, accepted one edit later.** Four
single-edit mutations of the selection survived everything — the
self-test never runs the suite, the gate mode stubs it, no matrix
entry mentions it, and the suite-size test pins `SUITE_SIZE`, which
the real gate does not use.

And the test that ought to have caught it asserted a **false premise**:
it said "SUITE_SIZE is the denominator of the harness's bound", but
`SUITE_SIZE` is used only by the synthetic fixtures — the gate's
denominator is the measured baseline. The test pinned one number and
not the **link** between it and the population the gate actually
measures, and the link was the hole.

**THE FIX.** A Go test now requires the harness's selection to select
**exactly** the acceptance suite's tests. That is the link, and it is
checked from outside:

    TestControls_TheHarnessSelectionIsTheAcceptanceSuite

**THE OTHER FINDINGS.**

| the finding | the fix |
|---|---|
| the new Go pin's floor was `seen == 0`, so one edit to a control's key **spacing** dropped it from the pin and the pin still passed | the pin's regexes are **tolerant** of the spacing, and its counts are **exact** over the harness's `CONTROLS` region — sixteen controls, fourteen of them load-bearing. A control that dropped out now makes the pin **fail** |
| §5.17 presented the class as fixed | it is not, and it now says what the round found and what it did |
| the "covers every control" claim was unpinned | the exact counts pin it |
| `PKG` was dead code | it is gone |
| `--only` was unchanged | it selects one control by name and the name must exist — already pinned by the harness itself |

**THE CLASS SO FAR.** Fourteen rounds, and the findings have one shape:
**a claim the harness carries that nothing checked.** A constant, a
pin, a question that could ask nothing, and now a selection. Each was
fixed properly and immediately, and each fix is checked from outside
the harness — because the harness cannot check itself.

### 5.19 What the fifteenth review found: a gate with no floor on what it measured

The fifteenth review asked the central question in the form that has
now found three real defects, and found a new instance of the class —
but a **sharper** one: about the gate's **SCOPE**, not its thresholds.

**THE HEADLINE: THE GATE HAS NO FLOOR ON THE NUMBER OF CONTROLS IT
MEASURED.** The verdict is "every control that **ran** was
load-bearing", which is vacuous below a floor:
`summarise([], 36, ())` returns **True** — the gate certifies **zero**
controls. And the harness's **production** path was driven by
**nothing**: both the self-test's drive and the gate mode pass
`--only` explicitly, so the loop over the controls was exercised by
nothing.

Four single-edit mutations survived everything. The reviewer drove the
harness's own `main()` in the production configuration with the suite
stubbed:

```
the PRISTINE harness                      14/14 load-bearing; 2 defence
                                         main() returned: 0
L-1: the loop runs only the FIRST        1/1  load-bearing; 0 defence
                                         main() returned: 0
L-3: the loop runs the first FOURTEEN     12/12 load-bearing; 2 defence
                                         main() returned: 0
```

The gate — which **is** the process's exit code, which **is** what CI
reads — passed having certified one control out of sixteen. The printed
figure is honest ("1/1"), which is exactly what makes it subtle.

**THE FIX HAS FOUR PARTS, AND EACH IS A MECHANISM.**

| the part | what it does |
|---|---|
| `summarise()` takes a **required** floor — the number of controls the measurement must cover | it refuses a measurement over a different number. "Every control that ran was load-bearing" is only a statement when the set of controls that ran is the set that was supposed to run |
| `main()` states the floor | every control, or the one `--only` selected |
| the **baseline** the bound is measured over must be the **published** suite size | this is the **invocation** check, and it is behavioural: the harness refuses a measurement that is not over the suite the bound is published for. A wider selection measures more, a narrower one measures less, and a missing `-run` measures the whole package — all three are now refused by the harness itself, in the production path, rather than by a source inspection |
| a **new drive** runs `main()` in the **production** configuration — no `--only` — with the suite stubbed | the loop over the controls is now exercised, and the drive requires every control to have run and the gate to have accepted the measurement |

**MEASURED, IN THE HARNESS'S OWN TERMS.** The gate, in the production
configuration with the suite stubbed:

```
the pristine harness                        exit=0  -> ACCEPTED
the loop runs only the FIRST control        exit=1  -> REFUSED
    THE HARNESS FAILED: the measurement covers 1 control(s) and it must
    cover 16 — a measurement over fewer controls certifies less than it
    appears to
the loop runs the first FOURTEEN             exit=1  -> REFUSED
one control excluded                         exit=1  -> REFUSED
--only defaults to a name                    exit=1  -> REFUSED
the published size (36)                      exit=0  -> ACCEPTED
a WIDER selection (49)                       exit=1  -> REFUSED
    THE HARNESS FAILED: the baseline ran 49 tests and the published
    suite size is 36
a NARROWER selection (1)                     exit=1  -> REFUSED
```

**AND THE TWO-EDIT COMBINATIONS ARE CAUGHT TOO** — the pairs the
matrices do not cover, because the matrices cover single edits: the
floor's check removed **and** the loop limited; the floor's check
removed **and** the production predicate neutered; the baseline's size
check removed **and** the selection widened; the non-vacuity guard
removed **and** a control's `expect` emptied. All four are caught by
the self-test.

**THE NITS, ALL CLOSED.**

| the nit | the fix |
|---|---|
| §5.18 re-classified `--only` as closed without addressing it | `--only` selects one control **by name**, and the name must exist — the harness raises otherwise. The floor is **one** when `--only` chose a control, and **every** control otherwise, so `--only` cannot widen the gate's claim: it narrows what the harness measures, and the printed figure says so |
| `publishedControls = 16` had no link to the documents' "Sixteen controls" | a Go test now requires the **design note** and the **SPEC** to state the counts the code publishes — the numbers are spelled out in words, and the pin checks the words |
| the selection pin was keyed to a **file** rather than to the suite | the pin's premise is now stated: the file is the acceptance suite, and the pin compares the harness's selection with the suite's test functions. A file that lost a test, or a suite that moved, makes the pin **fail** rather than shrink the set it compares with |

**THE CLASS SO FAR.** Fifteen rounds, and the findings have one shape:
**a claim the harness carries that nothing checked** — a constant, a
pin, a question that could ask nothing, a selection, and now a
**scope**. Each was fixed properly and immediately, and each fix is
checked from **outside** the harness, because the harness cannot check
itself.

### 5.20 What the sixteenth review found: the invocation, the workflow and the note

The first fifteen rounds looked for the class **inside** the harness:
its constants, its pins, its questions, its selection, its scope. The
sixteenth review looked for the same class **outside** it — in the
build and the workflow that *invoke* it, and in the note that
*publishes* it — and found it there. That is the class's second half,
and it is the half that matters most, because a harness that is
correct but never asked to do the work is not a measurement.

**THE SEVEN FINDINGS.**

| | the finding | the fix |
|---|---|---|
| **F1** | **the invocation was unpinned.** Nothing required the Makefile's `test-continuity-controls` target to ask for **every** control. A target that asked for one (`--only fence`) would measure one control and pass — the floor is **one** when `--only` chose a control (§5.19) — and every other gate in the tree would stay green | a Go pin requires the target to run the harness, with **no** `--only` and **no** `--selfcheck-gate` |
| **F2** | **the CI step could swallow the harness's exit status.** The harness's exit code **is** the gate (§5.2), and the workflow's step ended in `exit 0`, which reports success whatever the harness said. `\|\| true` was caught before, but only **incidentally** — as a "pipeline", so the message named the wrong problem | a Python pin refuses `\|\| true`, `exit 0` and `set +e` in the step, requires the step to run the **Makefile target** rather than the script, and refuses `--only` in the step |
| **F3** | **the note's table was not tied to the harness.** The note publishes every control and the test it must fail; nothing checked that the table described the harness, so a control could be renamed and the note would keep publishing the old pair | a Go pin requires every control's name to appear in the note, and the note's **own row** for that control to name the test the harness's `expect` requires |
| **F4** *(nit)* | **nothing ties a control's mutation to its invariant.** The note says what each control removes; nothing checks that the harness's mutator removes *that* | **partly** — see the limitation below |
| **F5** *(nit)* | **`run_tests` was exercised by no test.** It is the harness's **entire** contact with Go: it builds the command, runs it and parses the output into the four numbers the gate reasons about. Ten mutations of it survived everything | a Go pin requires the command to carry the **selection**, the **package** and the two **FTS5** flags — the three things that decide *what* is measured |
| **F6** *(nit)* | **`FILES` named doors the selection cannot observe.** The tuple lists every file a control **may** mutate — the service, the agent's door, the operator's door, PR 1's DDL. But the acceptance suite lives in ONE package, so a control that mutated `internal/core/tools/handlers_assignment.go` would remove code the suite **cannot see**: the control would measure nothing and the gate would report it as load-bearing | a Go pin requires every control's **mutator** to touch files inside `internal/core/assignment/` — the package the selection reaches |
| **F7** *(nit)* | **the pristine copy was never removed.** The harness backs the tree up into `TMPDIR` and restores it; the copy stayed behind, which is litter in the operator's temporary directory and a copy of the service's source to trip over | the harness removes the backup in its `finally`, so even an **abandoned** measurement leaves nothing behind |

**THE REVIEWER'S ATTRIBUTION OF F1 WAS WRONG, AND THE SUBSTANCE
STANDS.** The review reported that the Makefile's target already asked
for one control (`--only fence`). It did not: the target ran the
harness with no `--only`. The finding was **the absence of a pin**, not
the presence of a fence — and that is exactly the shape of the class,
because a claim nothing checks is false until something checks it, and
until then it may be true, false, or anything in between. The finding
is recorded here as it stands, with the correction.

**F3 HAD A SECOND HALF, AND IT WAS THE ONE THAT MATTERED.** The first
version of the pin required the note to **contain** the test the
harness names — anywhere in the document. That is satisfied by the
same name appearing in a different table, so a row that named the
**wrong** test was not caught: the drift mutation survived the first
form of the pin. The pin now reads the note's **own row** for the
control and compares that row's third cell with the harness's
`expect`. The mutation is caught, and the table a reader can check is
now a table the build checks.

**F4 IS NOT FULLY FIXED, AND THAT IS STATED RATHER THAN PAPERED OVER.**
What is mechanical now: the control → **test** link. What is not: the
control → **invariant** link. The note's second column says what each
control removes ("the fence, both halves"), and the harness's mutator
removes source text that a human must read to confirm it is *that*
invariant. A mutation that removed the fence's *bound epoch* instead of
its *check* would satisfy every pin in this PR — the control would
still fail the named test, and the note would still publish the pair —
and the invariant it actually broke would be a different one. The
honest statement is that **the invariant a control claims to break is
prose, and prose is not checked**. The mitigation is that the control
and its expectation are read together by a human reviewer each round,
and this is recorded here as the next thing a future round should
close, rather than as a defect that is closed.

**THE VERIFICATION IS BY MUTATION, IN BOTH LANGUAGES.** Each fix is
caught by removing it, in a scratch copy, and requiring the check to
fail:

```
the Makefile target asks for ONE control            CAUGHT
the Makefile target runs the self-check mode         CAUGHT
the harness's command drops the selection             CAUGHT
the harness's command changes the package             CAUGHT
a control mutates a file outside the suite's package CAUGHT
the note's table drifts from the harness             CAUGHT

the CI step swallows the exit status with `|| true`   CAUGHT
the CI step swallows the exit status with `; exit 0`  CAUGHT
the CI step asks for one control                      CAUGHT
the CI step calls the script directly                CAUGHT
the CI step is gone                                  CAUGHT
```

The first group is the Go pins; the second is the Python CI pin, run
in a scratch copy so the operator's workflow is never touched. The
pristine tree passes in both.

**THE COUNTS MOVE WITH THE FIXES.** The four suites now hold
**sixty-nine** test functions (§5.14's figure, computed from the tree),
the harness publishes **sixteen** controls, its self-test runs
**eighty-two** checks, and the two matrices are **thirty-eight** and
**four** entries.

**THE CLASS SO FAR — THE SECOND HALF.** Sixteen rounds. The first
fifteen found the class inside the harness; the sixteenth found it
outside — in the build, in the workflow, in the note. The shape is the
same: **a claim the harness carries that nothing checked**. A harness
cannot check its own invocation, so the invocation is checked by a Go
test that reads the Makefile; a step that can discard an exit code
cannot check itself, so the workflow is checked by a Python test that
reads it; a note that publishes a table cannot check the table, so the
table is checked against the harness. Each fix is checked from
**outside** the thing it pins, because nothing can check itself.

## 6. What PR 2 leaves to PRs 3–5

| | deferred to |
|---|---|
| the OpenCode adapter, the reviewer isolation, the session handoff | **PR 3** |
| the dispatcher, the recovery loop, the tree's aggregate budgets (`max_tasks`, `max_concurrency`, `max_resource_units`) | **PR 4** |
| the GitHub evidence contract, the read-only `check` discovery | **PR 5** |
| the first real assignment (#253, skills P8) | **PR 6** |

**MPM does not merge, deploy or change production state**, and no command
in this PR can.
