# Continuity 001, PR 1 — the durable assignment storage

**Status:** implemented (PR 1 of the six-PR partition; tracking issue #258).
**Design of record for the storage:** `internal/core/migration_agent_assignments.go`.
**DDL pin:** SHA-256 `b1e606b7cc3e10ea50b9d0676f25c6f694562c0d09f7cba0359d112720cb9709`, asserted by
`TestAgentAssignmentDDLIsPinned`.

This note is the design the storage implements. The **published** behaviour is
`docs/SPEC.md`; this note is the reasoning, the reuse decisions and the
deferrals.

---

## 1. What PR 1 is, and what it is not

PR 1 is **storage**. It creates two tables, seven indexes and thirty
triggers, and it makes the contract's hardest invariants true *in the
database*, so a wrong write is refused by the engine rather than by the
convention of whichever code path happened to make it.

PR 1 contains **no** API, **no** CLI, **no** OpenCode adapter, **no**
dispatcher and **no** evidence writer. Those are PRs 2–5. Nothing an agent
can call reaches this storage yet — the tables are written only by the
migration.

---

## 2. What is reused, and what is deliberately not

### 2.1 Reused: the project identity axis

PR #224's **Stage 1 is already in `main`** — commit `d8b373fb`, with the DDL
pinned by hash. `projects`, `project_names`, `continuity_threads` and
`continuity_checkpoints` exist; this PR **references** `projects(id)` and
**does not duplicate** the project model or alter its reviewed DDL.

The assignment inherits Stage 1's two structural idioms:

* **the containment boundary.** The project id is denormalised onto the
  assignment and held honest by a **composite** foreign key — `(root_id,
  project_id)` and `(parent_id, project_id)` both reference
  `agent_tasks(id, project_id)`. A child therefore cannot name a root of
  another project, and cannot name a parent of another project. The boundary
  is a property of the storage, not of the code that respects it.
* **the optimistic-concurrency token.** `continuity_threads.revision` is the
  codebase's own fencing idiom; `agent_tasks.claim_epoch` is the assignment's
  version of it, and it is described in §4.

Every reference is `ON DELETE RESTRICT`, so a project with assignments cannot
be cascade-destroyed and the ledger cannot be destroyed with it.

### 2.2 Reused: the ledger and outbox shapes

`agent_task_events` is the same shape as `work_events` — an event index
`UNIQUE` per assignment, so the ledger is ordered. The two vocabularies stay
**separate**: a work item is a commitment, an assignment is an assignment.

### 2.3 Not reused: `works`

`works.status` is `CHECK (status IN ('open','done','cancelled'))` and the
contract needs **ten** states. Widening that CHECK means rebuilding a live
table in SQLite, and `works` has **no** recipient, **no** lease, **no**
fencing token, **no** idempotency key, **no** dependencies and **no** project.
The vocabulary stays separate.

### 2.4 Not reused: `scheduled_wakes`

The wake path is **at-most-once**: `scheduled_wakes.fired` is set to 1 in the
same transaction that surfaces the row, so a crash after the mark and before
the agent acts **loses the wake permanently**. That is the documented loss
window, and it is why the wake path is **not** promoted into a delivery queue
here.

The assignment's delivery is therefore a **state transition with its own
ledger entry** — the `delivered` event in `agent_task_events` — never a side
effect of the row having been read. An assignment that was delivered and
never acknowledged is **re-deliverable**, not lost.

### 2.5 Not reused: `session_handoffs`

A handoff is a *terminal* session-end record with a 90-day TTL, keyed `UNIQUE`
on `session_id`. It bridges two sessions of the same agent; it is not a queue,
and the absence of a handoff is not proof that work did not happen.

---

## 3. The contract the storage enforces

### 3.1 The ten states, and the separations

| state | meaning |
|---|---|
| `enqueued` | created; nobody has been handed it |
| `delivered` | handed to a runtime — a session exists |
| `acknowledged` | the recipient **took** it |
| `accepted` | the recipient **will do** it |
| `in_progress` | the work is happening |
| `completed` | the implementer's claim that the work is done |
| `review_passed` | the independent review of **this** assignment passed |
| `awaiting_operator` | the operator's gate — the automation's terminal state |
| `operator_approved` | an **externally verified** approval was **recorded** |
| `cancelled` | withdrawn |

Delivery is not acceptance, acceptance is not execution, execution is not
completion, completion is not review, and review is not approval. A state
machine that collapsed any two of them would make the ledger lie about what
happened.

`operator_approved` is a **record, not a grant**. MPM records an approval
that was verified **outside** MPM — its source, the exact revision it covers
and a pointer to the evidence — and it never manufactures or authenticates
one. The approval names the exact action (`approval_action`) and the exact
revision (`approval_revision`); a change to the head revision after the
approval was recorded leaves the approval **stale**, and staleness is
observable rather than implied.

All four of the approval's fields are **required** on the transition into
`operator_approved`: the action, the revision, the source and the evidence
pointer. An approval with no pointer behind it is a claim about what was
authorised with nothing to check it against, and the pointer is what a
later reader follows.

And the transition must **supply** them — they cannot be written earlier.
The same holds for the action the gate awaits: it is set by the transition
into the gate and by nothing else. Without that, a row could be given the
whole gate and approval record in one early write and then walk the path
without naming anything, and the record would not be attributable to the
event that claims it.

**And they are born empty.** The rule holds on the INSERT path as well: an
assignment is created with no gate and no approval, and a row created
carrying them would be **permanently wedged** — the immutability rules
would refuse every attempt to supply the value the transition needs, the
"must supply" rules would refuse a transition that re-stated it, and a row
with a ledger entry could no longer be deleted. So the assignment could
neither complete its path nor be removed.

### 3.2 The role-aware state machine

The transition path depends on the assignment's `role`, and the trigger knows
the role:

* an **implementer** goes `enqueued → delivered → acknowledged → accepted →
  in_progress → completed → review_passed → awaiting_operator →
  operator_approved`. It **cannot** skip its independent review: `completed →
  awaiting_operator` is refused;
* a **reviewer** goes `enqueued → delivered → acknowledged → accepted →
  in_progress`, and ends at either `review_passed` (the verdict was passing)
  or `completed` (the review is done and it asked for changes). A reviewer
  never reaches the operator's gate: the reviewer approves nothing.

`cancelled` is the **universal** withdrawal. It is legal from **every** state
except itself, for **every** role — including `operator_approved`, because
an operator may withdraw an approved assignment, and including a reviewer's
`completed`. It is stated **once**, as its own clause in the trigger, so that
"universal" is a property of the trigger rather than a promise that every
branch remembered to include it.

The gate is a **hand-off, not a claim**. `awaiting_operator` is the
automation's terminal state: the work is finished and the operator holds the
decision, so an assignment at the gate cannot be claimed. A lease there
would be a lease on a pause — it would expire against nobody, and it would
advance the epoch of a row that the state machine will not let anything
else work on. The two states the state machine calls terminal are refused
for the same reason, and the refusal covers both sides of the statement: a
claim that arrives with the cancellation in the same write is a claim on a
row that is being withdrawn.

`operator_approved` and `cancelled` are otherwise terminal.

### 3.3 The delegation tree and the inherited budgets

A **root** assignment (`depth = 0`) is the operator-authorised one. It is its
own root (`root_id = id`), it has no parent, and it **carries the budgets**:
`max_depth`, `max_tasks`, `max_concurrency`, `max_resource_units`,
`permitted_operations` and `eligible_recipients`.

A **child** (`depth > 0`) is an agent's delegation. It names its root and its
parent, and it carries **none** of the root's budgets — there is no column in
which a child could widen `max_depth`, `max_tasks`, `max_concurrency`,
`max_resource_units`, `permitted_operations` or `eligible_recipients`.
Inheritance of those is **structural**.

One column is deliberately outside that set: `resource_units`, which is the
child's **own** consumption against the root's cumulative budget. A child
therefore does carry a resource figure, and it may name any figure it likes —
the budget it must respect is the **root's** `max_resource_units`, and
enforcing that is the dispatcher's job (§5, N4), not the schema's. No
**budget limit** is carried by a child; a **consumption figure** is.

A root with an **empty** `permitted_operations` is refused, and so is a root
with an **empty** `eligible_recipients`: a root that may do nothing, or that
may address nobody, is a silent misconfiguration rather than a safe default.
A root's own `recipient` must be among its `eligible_recipients` — a root
addressed to an agent the tree may not target is the same misconfiguration.
And every element of both lists must be a **non-null, non-empty string**: an
object, an array, a boolean, a number, a null and `""` are all refused.

The tree is a **tree**, and that means four things:

* the root must be a **root** of the same project — a child may not cite a
  non-root as its budget root;
* the parent must be in the **same tree** — the same `root_id`;
* the parent's depth must be **exactly one less** — and that is what
  **excludes a cycle**: following the parent chain must strictly *decrease*
  the depth, so the chain cannot come back to where it started;
* nobody is their **own parent**.

The first three hold on the INSERT path and on the UPDATE path.

**And the depth is immutable.** That is what makes the depth condition
*sound*, and it was the subject of the third review's first blocking
finding. A BEFORE trigger sees only the row being written, so the condition
"the parent's depth is exactly one less than the child's" can be satisfied
by two statements that each change a depth — the second statement sees the
first one's new depth. Two children could then be pointed at each other
with mutually inconsistent depths, and an ancestor walk over that "tree"
would never terminate. A depth that **never changes** makes the chain
strictly decreasing by construction.

The immutability also closes the promotion hole: a child cannot be promoted
to a root, because a root's depth is zero and the depth never changes.
Without it, both tree-shape triggers — which carry `WHEN NEW.depth > 0` —
skipped the write entirely, and the promoted child's own descendants were
left with a parent whose root and depth no longer matched theirs, with no
trigger re-checking a row it did not write.

### 3.4 The idempotency key

`idempotency_key` is `UNIQUE` across the table, so an accidental duplicate
dispatch cannot create a second logical assignment. The API (PR 2) turns the
conflict into "return the existing assignment", which is what makes a
duplicate dispatch **idempotent** rather than merely **refused** — and that
is what the **unique index** is for: a trigger can refuse a duplicate, but
only an index can FIND one without refusing the write.

**It is not what lets the API resolve one with `ON CONFLICT`.** The
before-insert trigger fires *before* the conflict resolution, so the
conflicting row is still there and `INSERT ... ON CONFLICT DO NOTHING`
**aborts** with the trigger's message instead of resolving the duplicate.
PR 2 must therefore **select** the existing assignment after the refusal,
not rely on `ON CONFLICT` — and the design note says so, because the
promise was checked by execution rather than by reading.

The key's **type** is part of its identity: a `BLOB` and a `TEXT` with the
identical bytes are different values to SQLite, and a UNIQUE index holds
both — so the column is guarded with `typeof(...) = 'text'`.

And the key is **immutable**. A key that could be changed would free the
old one, and re-dispatching the same logical assignment with it would
succeed — leaving two logical assignments for one dispatch, which is the
duplicate suppression the rule exists for, defeated at the storage level.

### 3.5 The gate's record is fixed, whatever happens next

Once the gate is reached, the record of what was asked for and of what was
approved is **evidence**, and evidence is not rewritten — not to a different
value and not to nothing.

The scope is the **record**, not the row's current state. A row that has
carried the gate's action keeps it, whatever the row's state is afterwards —
including after the assignment is cancelled. The sixth review found the
rule scoped to the state, so the protection evaporated on cancellation: the
cancellation itself was refused, and the statement *after* it was not.

### 3.6 Nothing is replaced, and nothing is reused

`INSERT OR REPLACE` is DELETE + INSERT, so it defeats every rule that is
enforced by a `BEFORE UPDATE` or a `BEFORE DELETE` trigger: the update
triggers never fire, and SQLite does not fire the delete triggers for
REPLACE's implicit delete unless the `recursive_triggers` pragma is on —
which MPM does not enable, because the DSN sets `foreign_keys`,
`journal_mode`, `busy_timeout`, `synchronous` and `cache_size`, and nothing
else.

The storage therefore refuses an **INSERT** whose id or whose idempotency
key already exists. SQLite runs the insert trigger **before** the conflict
resolution, so the conflicting row is still there and the replacement is
refused as a whole: the existing row survives with its role, its state, its
lease, its claim epoch and its approval record intact.

`UPDATE OR REPLACE` is the other half of the same hole: it resolves a
uniqueness conflict by **deleting** the conflicting row, and the delete
triggers do not fire for that implicit delete either. Without a guard, a
**claimed** assignment could be destroyed by an UPDATE that merely set a
colliding idempotency key: its id was freed, and a stale token could become
current again — which is exactly the hole the first review closed, reached
by a different statement.

And a **collision** is not the only way to free an id. A **rename** frees
it without any conflict at all: `UPDATE agent_tasks SET id = 'other'`
moves the row, the old id becomes free, and a fresh row can take it, come
back at epoch 0, and be claimed at epoch 1 — so the previous holder's
stale token (`WHERE id = ? AND claim_epoch = ?`) matches the **new** row.

So the rule is not "no collision" but **immutability**: neither the id nor
the idempotency key changes. That closes the hole from both sides — the
rename is refused, and a collision cannot arise, because the only two
columns a uniqueness conflict can be resolved on are the two that no
longer move.

The ledger is guarded the same way, on its id and on its `(task, index)`
pair, so a ledger entry cannot be destroyed by a replacement in either
form.

---

## 4. The lease, the epoch and the fence

`claim_epoch` is the fencing token. It advances by **exactly one** on every
claim, and the claim sets both the owner and a lease.

The trigger enforces:

* the epoch advances by exactly one — it cannot be skipped or reset;
* the owner changes **only** on a claim — an owner cannot be reassigned
  without the row ceasing to be a lease;
* only a **live** assignment can be claimed — `operator_approved` and
  `cancelled` are terminal, so a lease taken on either is a lease on
  nothing, and it would advance the epoch of a row that will never be
  worked on again. Both sides of the statement are checked, so a claim
  that arrives with the cancellation in the same statement is refused
  too;
* the lease changes **only on a claim** — a claimed row keeps its lease,
  and an unclaimed row cannot be given one. The lease belongs to the
  claim, and the next claim replaces it;
* a claim leaves the row with an **owner** and a **lease** — and the
  lease is always **new**. The owner is required to be present, but the
  schema cannot require it to have *changed*: a claim that renews the same
  holder's lease leaves the owner unchanged, and a trigger cannot tell that
  from a takeover that forgot to write the new owner. So the rule is
  stated as it is enforced — the owner is present, and only a claim
  changes it — and **PR 2's claim always writes the owner**, so a takeover
  is explicit. The lease has no such ambiguity: it belongs to the claim,
  so a claim that inherited the last claim's window would have no window
  at all;
* a task is born **unclaimed** — epoch 0, no owner, no lease.

The trigger deliberately does **not** enforce the fence. A trigger cannot see
the epoch a caller *presented* — it sees the row before and the row after — so
the fence is the `WHERE` clause of the writing statement:

```sql
UPDATE agent_tasks SET state = 'acknowledged', owner = ?
 WHERE id = ? AND claim_epoch = ?
```

That comparison is atomic with the write, so a stale owner's write matches no
row. The trigger's job is to make sure the epoch cannot be **forged** — it
cannot be skipped, reset, or advanced without a claim shape.

The epoch's **type** is load-bearing: SQLite's INTEGER affinity does not make
a number an INTEGER, and at REAL `1e30` the expression `epoch + 1` equals
`epoch` — so the owner could change without the fence advancing. Every
numeric column is guarded with `typeof(...) = 'integer'`.

A lease that expires without a transition is **recoverable**: the next claim
takes it and advances the epoch, which invalidates the previous holder.

A **claimed** assignment cannot be **deleted** — it is cancelled. That rule
is stated in terms of the epoch because it is what closes the reuse hole: a
task that was ever claimed holds an epoch, and a task that holds an epoch
cannot be deleted, so its id cannot be reused — and a reused id would restart
the epoch at zero, which is exactly how a stale token could become current
again. An assignment that has a **ledger** cannot be deleted either, for the
same reason from the other end: the ledger is evidence, and evidence must not
be orphaned. An **unclaimed** task with no ledger may still be removed: it
never issued a token and nothing refers to it.

---

## 5. The invariants the database enforces, and the ones it does not

Thirty-four properties are enforced by the schema itself rather than by
convention.

| # | invariant | enforced by |
|---|---|---|
| I1 | an assignment is created `enqueued` | the initial-state trigger |
| I2 | an assignment is created **unclaimed** (epoch 0, no owner, no lease) | the unclaimed-at-creation trigger |
| I3 | an assignment's id and its idempotency key are **never reused** | the id-not-reused trigger |
| I4 | the delegation tree is a **tree** — the root is a root, the parent is in the same tree at depth one less, and nobody is their own parent | the tree-shape triggers (INSERT **and** UPDATE) |
| I5 | the depth is **immutable** | the depth-immutability trigger |
| I6 | a root's own recipient is among its eligible recipients | the root-recipient triggers |
| I7 | every permitted operation is a non-null, non-empty string | the permitted-operations triggers |
| I8 | every eligible recipient is a non-null, non-empty string | the eligible-recipients triggers |
| I9 | the state machine is role-aware | the state-transition trigger |
| I10 | `cancelled` is the universal withdrawal | the state-transition trigger |
| I11 | the operator's gate **names its action** | the state-transition trigger |
| I12 | a recorded approval names its action, its revision, its source and its evidence pointer | the state-transition trigger |
| I13 | the approval names the **same** action the gate awaited | the state-transition trigger |
| I14 | the gate's action is **immutable** once the gate is reached — and the immutability does not expire with the row's state | the state-transition trigger + the awaiting-action trigger |
| I15 | a recorded approval is **immutable** — it is evidence, and the immutability does not expire with the row's state | the state-transition trigger + the approval trigger |
| I16 | the claim epoch advances by exactly one | the claim-discipline trigger |
| I17 | a claim leaves the row with an owner, and sets a **new** lease | the claim-owner and claim-lease triggers |
| I18 | only a claim changes the owner | the owner trigger |
| I19 | the role is **immutable** | the role-immutability trigger |
| I20 | a claimed assignment, or one that has a ledger, cannot be **deleted** by a DELETE statement | the not-deletable trigger |
| I21 | the ledger's event index is **monotonic** | the monotonic trigger |
| I22 | a ledger entry's id and its index are **never reused** | the ledger's id-not-reused trigger |
| I23 | the ledger is **append-only** | the ledger's no-UPDATE and no-DELETE triggers |
| I24 | the ledger's `from_state` and `to_state` come from the ten-state vocabulary | the column `CHECK`s |
| I25 | every identifier, key, number, pointer and JSON column has its **type** — a `TEXT` column cannot hold a `BLOB`, and a numeric column cannot hold a `REAL` or a numeric string | the `typeof()` guards |
| I26 | a **root** carries the budgets; a **child** carries none of them | the root/child `CHECK` |
| I27 | the delegation tree is contained in **one project** | the composite foreign keys |
| I28 | the id is **immutable** | the id-immutability trigger |
| I29 | the idempotency key is **immutable** | the key-immutability trigger |
| I30 | only a **live** assignment can be claimed | the claim-needs-a-live-assignment trigger |
| I31 | the lease changes **only on a claim**, and a claim sets a **new** one | the lease-changes-only-on-a-claim trigger |
| I32 | the gate's action is **set only by** the transition into the gate | the state-transition trigger + the awaiting-action trigger |
| I33 | the approval's fields are **set only by** the transition that records it | the state-transition trigger + the approval trigger |
| I34 | the gate's record and the approval are **born empty** | the born-empty trigger |

The invariants that are **not** in the database. Each one is deferred for
its own reason, and the reason is **not** the same for all of them: a
`CHECK` constraint cannot contain a subquery, and a trigger cannot see a
caller's presented value — but three of the six are subqueries a trigger
*could* write, and they are deferred because the rule belongs to the API's
**authorisation**, not to the storage's shape.

| # | invariant | enforced by | lands in |
|---|---|---|---|
| N1 | the target of a write presents the **current** claim epoch (the fence) | the `WHERE` clause of the writing statement | PR 2 |
| N2 | a **child's** recipient is among the root's `eligible_recipients` | the API's authorisation — a trigger could write the subquery, but the rule is about who may be addressed, and that is the API's business | PR 2 |
| N3 | a child's depth is within the root's `max_depth` | the API's authorisation — the same subquery a trigger could write, deferred for the same reason | PR 2 |
| N4 | the tree's task count, concurrency and **resource consumption** are within the root's budgets | the dispatcher | PR 4 |
| N5 | a reviewer's findings are captured by a host-controlled handoff and published with their digest | the adapter | PR 3, PR 5 |
| N6 | an assignment is only created from an operator-authorised root | the API's authorisation — nothing in the row says who authorised it | PR 2 |

Those are named here rather than left implicit, because "the schema enforces
it" and "the API enforces it" are different claims and only one of them is
true of each row.

### 5.1 The ledger is monotonic, not gap-free

The event index is `UNIQUE` per assignment and a trigger refuses an index
that is not **strictly greater** than the current maximum. The ledger is
therefore **ordered and monotonic**. It is **not** gap-free: a writer that
reserves an index and then dies leaves a hole, and the ledger records what
happened rather than promising what did not.

The ledger is **append-only**, and that is enforced: triggers refuse an
`UPDATE` and a `DELETE` on it. A provenance ledger that can be rewritten is
not a provenance ledger.

The ledger's `event_type` vocabulary carries the transitions plus three
observations that are not transitions: `claimed` (a lease was taken),
`lease_expired` (a lease lapsed and the assignment became re-claimable) and
`note` (a free-text annotation that changes no state). Two of those names
differ from the state they record — `created` for `enqueued` and `started`
for `in_progress` — because the event says **what happened** and the state
says **where it is**. `from_state` and `to_state` carry the ten-state
vocabulary unchanged.

### 5.2 The two vocabularies that cannot be widened

`agent_tasks.role` and `agent_task_events.event_type` are closed `CHECK`
vocabularies, and `agent_tasks.state` is a third. Widening a `CHECK` on a
live table means rebuilding that table in SQLite. That is the same argument
that keeps `works.status` separate from this contract — and it applies here
too, with the difference that `agent_tasks` will hold **live leases**.

Nothing in the design note, the Gate A inventory or `docs/SPEC.md` §6.14
requires a third role; only `implementer` and `reviewer` are ever named. But
a later PR that needs a third role is a **migration that rebuilds a live
table**, and it should know that before it designs the row, not after.

---

## 6. The migration's discipline

The DDL is owned by a **migration**, not by a schema slice:

* `BaseTables` is the slice `attachShared` propagates into the cross-workspace
  `shared` schema, and an assignment belongs inside **one** workspace;
* the migration runs inside the **fatal migration transaction** on every open,
  so a fresh database and an upgraded one take the same code path and the same
  DDL text — there is no second definition to drift;
* the sentinel row is written **last**, inside the same transaction, so a
  failure injected at the sentinel write rolls the DDL back with it and leaves
  no partial schema;
* it is idempotent via the `agent_assignments_v1` sentinel.

---

## 7. What the seven independent reviews found

The change was reviewed independently seven times, in the installed
sandboxed reviewer (`mpm-reviewer`, default-deny, `sandbox_exec` only). The
first returned **eleven blocking and thirteen non-blocking** findings, the
second **two blocking and fourteen non-blocking**, the third **five blocking
and thirteen non-blocking**, the fourth **one blocking and ten non-blocking**,
the fifth **two blocking and eight non-blocking**, the sixth **no blocking
findings and five minor and three nit findings**, and the seventh **one
blocking, three minor and two nit findings**. Every blocking finding was a
real defect, and every one is closed.

The sixth review was the one that returned **no** blocking finding. Its
minor findings were not dismissed: three of them were real defects and two
were claims in this note that were simply wrong. They are closed below.

The seventh review then found a blocking defect in the **fix** for the
sixth — which is the reason a review round is not the same thing as a
sign-off.

### 7.1 The first review's eleven blocking findings

1. `completed → awaiting_operator` skipped the implementer's required
   independent review;
2. the transition path used `OLD.role`, but `role` was **mutable**, so a
   relabel selected a different path — and a role-only write evaded the path
   check entirely;
3. the ledger permitted **gaps**, and permitted `UPDATE` and `DELETE`, while
   the contract called it append-only;
4. the child branch of the root/child `CHECK` constrained
   `permitted_operations` but **not** `eligible_recipients`, so a child could
   store its own expanded allow-list;
5. the same-project composite keys did not establish a **tree**: a child could
   parent itself, could cite a non-root as its budget root, and its parent
   and root need not belong to the same tree;
6. `TEXT PRIMARY KEY` permits **NULL** in SQLite, and a NULL id made the root
   equality UNKNOWN, so a depth-zero row pointing at someone else's root was
   accepted;
7. the claim discipline checked `UPDATE`s only, so a task could start at
   epoch 87 with an owner and no lease;
8. INTEGER affinity does not make a number an INTEGER, so at REAL `1e30`
   `epoch + 1` equals `epoch` and the owner could change without the fence
   advancing;
9. an unreferenced task could be **deleted and re-inserted**, resetting its
   epoch so that a stale token could become current again;
10. the numeric budget comparisons accepted **nonnumeric text**, so a
    "bounded" root need not have had a numeric limit;
11. a `BLOB` idempotency key and a `TEXT` one with the identical bytes
    coexisted in the unique index, defeating the duplicate suppression.

All eight of the underlying SQLite facts were verified empirically before any
of them was acted on — a `TEXT PRIMARY KEY` does accept a NULL, a NULL
operand in a `CHECK` is not a violation, INTEGER affinity keeps a REAL, a
TEXT budget passes `> 0`, a BLOB and a TEXT key coexist in a UNIQUE index, a
self-referencing composite FK accepts the self-parent row, `'[null]'` passes
`json_array_length > 0`, and a `CHECK` cannot contain a subquery.

### 7.2 The second review's two blocking findings

1. **the delegation tree could be made into a CYCLE.** The tree-shape
   triggers closed the three named cases, but nothing excluded an ancestor
   cycle: two `UPDATE`s pointing two children at each other satisfied every
   stated condition. The fix was the parent's **depth** — the chain must
   strictly decrease — which excludes a cycle and enforces the depth
   consistency at the same time;
2. **a recorded approval could be REWRITTEN, not merely cleared.** The
   trigger fired only on a NULL or an empty value, so the revision could be
   re-pointed at a different one — which defeats the staleness comparison
   against the head revision. The trigger now fires on **any** change.

### 7.3 The third review's five blocking findings

1. **the depth condition did NOT exclude a cycle.** The second review's fix
   was right about the intent and wrong about the mechanism: a BEFORE trigger
   sees only the row being written, so two statements that each change a
   depth could each satisfy the condition against the other's new depth, and
   the parent chain would never terminate. The fix is the depth's
   **immutability**;
2. **a child could be promoted to a root.** Both tree-shape triggers carry
   `WHEN NEW.depth > 0`, so a write that set the depth to zero was checked by
   neither of them, and the promoted child's descendants were left with a
   parent whose root and depth no longer matched theirs. The depth's
   immutability closes it;
3. **every UPDATE- and DELETE-triggered rule was defeated by
   `INSERT OR REPLACE`.** REPLACE is DELETE + INSERT: the UPDATE triggers
   never fire, and SQLite does not fire the DELETE triggers for REPLACE's
   implicit delete unless the `recursive_triggers` pragma is on — which MPM
   does not set. So a claimed task could be re-inserted with its epoch reset
   to zero, and a ledger entry could be destroyed outright. The defence is a
   BEFORE INSERT trigger that refuses an INSERT whose id or idempotency key
   already exists: SQLite runs the trigger **before** the conflict
   resolution, so the conflicting row is still there and the REPLACE is
   refused as a whole;
4. **the gate's record was rewritable by the statement that LEAVES the gate.**
   The trigger fired only `WHEN OLD.state = NEW.state`, so the very statement
   that moved the assignment out of the gate could rewrite or clear the
   awaited action. The rule now lives inside the state machine's trigger,
   where the path is checked first;
5. **`approval_evidence` and `approved_at` were rewritable and clearable**
   while the approval stood. The trigger covered three of the five approval
   columns. All five are covered now — an approval whose evidence pointer
   can be swapped is not evidence.

### 7.4 The third review's non-blocking findings

Three were unenumerated holes, and they are now either enforced or named: the
permitted-operations guard tested `typeof(value)`, which is always TEXT for a
non-scalar element, so an **object** or an **array** passed as a permitted
operation — the discriminator is json_each's `type` column; a child's depth
was unrelated to its parent's; and a root's own recipient need not have been
among its eligible recipients, while an empty eligible list was accepted and
an empty permitted list was refused.

The rest were about the tests, the documents and the harness: the "the path is
checked first" claim was false, because two separate triggers competed with
the state machine — they now fire only while the state is unchanged, so a
write that moves the state is governed by the state machine's rules alone;
the ledger's
`from_state` and `to_state` carried no vocabulary; the `typeof()` guards did
not cover the timestamps, the evidence pointers, the gate's record, the JSON
columns or the ledger's attribution; the containment probe's comment claimed
to isolate one foreign key when it violated both; the negative-control counts
were unverifiable, because the harness that produced them was not part of the
change — it is now, at `scripts/continuity-001-controls.py`; the trigger
count was stale in two places; the AT8 "real dump" carried a row that would
not even execute, and the test passed anyway because the validator accepts
text without executing it — the restore is now proved through the **command**,
in `cmd/mpm/handlers_restore_assignment_test.go`; and the combined "relabel
and transition in one statement" case was not covered by any test.

The third review also made two honest observations that this note now carries
as prose rather than as silence: a child **does** carry a `resource_units`
figure (§3.3), and the two closed vocabularies are a limitation that a later
PR must plan for (§5.2).

---

### 7.5 The fourth review's one blocking finding

**`UPDATE OR REPLACE` deleted a CLAIMED assignment, freed its id and reset
its epoch.** The third fix closed the INSERT half of the replacement hole
and left the UPDATE half open: an UPDATE OR REPLACE resolves a uniqueness
conflict by deleting the conflicting row, and the delete triggers do not
fire for that implicit delete either. So a claimed assignment could be
destroyed by an UPDATE that merely set a colliding idempotency key, its id
freed, and a stale token made current again — the first review's stale-token
hole, reached by a different statement. The suite was green because it
covered only the INSERT form.

The fix is a `BEFORE UPDATE` trigger that refuses a change that would
collide with another row's id or idempotency key.

### 7.6 The fourth review's non-blocking findings

* **the "an illegal transition is always reported as an illegal transition"
  claim was false.** SQLite's order for firing SEPARATE triggers on the
  same event is not specified, and it is measured to be the reverse of
  creation order. A write that both moves the state illegally and changes
  the role is therefore refused — the invariant holds — but the message may
  be the role's rather than the transition's. The claim now says what is
  true: the write is always **refused**, and the message is whichever rule
  fired first.
* **the note and the SPEC enumerated different property lists.** The note's
  list omitted the root/child budget `CHECK` and the composite containment
  foreign keys, both of which the note itself measures with a control. They
  are I26 and I27 now, and both documents carry the same list.
* **the in-DDL rule labels contradicted the header's rule list.** They agree
  now, and the header's counts were checked against the triggers that
  actually exist.
* **the harness's import-time guard was dead code, and a tautology.** It
  compared the file with itself. It now takes the DDL's digest when the
  module is loaded — above the control list, which is where the mutation
  happened — and `snapshot()` refuses to run if the file has changed since.
* **the harness had no `try`/`finally`**, so a failure left the working
  tree mutated. The tree is now restored on every path.
* **the harness hard-coded `HOME`** and **`rmtree`d a fixed path.** It now
  inherits the environment and takes its pristine copy in a fresh
  temporary directory.
* **the AT8 dump was not the shape `sqlite3 .dump` produces.** That is
  true, and it is now proved both ways: the first command-level test uses
  the order that proves the rows satisfy the schema (indexes and triggers
  first, then the rows), and a second one uses the order the backup
  command actually writes (the data before the indexes, with the foreign
  keys turned off for the duration). Both restore. The fourth review
  reported that the dump order fails under the handler's conditions — and
  that is true only with the foreign keys ON; the real dump opens with
  `PRAGMA foreign_keys=OFF;`, and a real `sqlite3 .dump` of a database
  carrying these tables was executed and restored.
* **"an integer stays an integer" was inaccurate**: `quote()` renders every
  value as TEXT, and the column's affinity is what restores the type.

### 7.7 The fifth review's two blocking findings

**1. An assignment's id was MUTABLE, so the id WAS reused and the fence
WAS broken.** The fourth review's fix refused a uniqueness *collision* on
the update path, and the fifth review found the hole behind it: a row could
be **renamed**, which frees the old id with no collision at all. The
fifth review executed the whole chain — rename a claimed leaf child, insert
a second assignment with the freed id, claim it at epoch 1, and the
previous holder's `UPDATE ... WHERE id = ? AND claim_epoch = 1` **matched
the new row**. The first review's "a stale token could become current
again", reached by a third statement.

The fix is **immutability**: the id does not change, and neither does the
idempotency key. The rename is refused, and a collision cannot arise,
because the only two columns a uniqueness conflict can be resolved on are
the two that no longer move.

**2. The command-level restore test's trigger inventory was one short.** It
named twenty-five of the twenty-six triggers the DDL created, so the dump
it built restored a database **without** the fourth review's update-path
guard — and the assertions, which only looked for the *listed* names, could
not see the omission. The test passed while the restored schema was weaker
than the approved one.

The inventory is now the whole set, and the test **compares it against the
live schema**, so a trigger the DDL creates and the list does not name
fails the test rather than being restored away.

### 7.8 The sixth review's findings

The sixth review returned **no blocking finding**: it could not find a way
to destroy, duplicate, rename or re-key a row, to forge a claim, to break
the tree, to bypass the state machine, or to resurrect a stale token on a
different assignment. Its five minor and three nit findings were about
precision and about two narrow gaps between the contract and the DDL, and
all of them are closed.

**The two gaps between the contract and the DDL** are the substantive ones:

* **the gate's record was protected only while the row was AT the gate.**
  The immutability triggers were scoped to the row's current state, so the
  protection evaporated on cancellation: the cancellation itself was
  refused, and the statement *after* it was not — so a cancelled
  assignment's awaited action could be rewritten, and its approval could
  be rewritten too. The scope is now the **record**, not the state;
* **the records did not have to be SET by their transitions.** The field
  rules required the fields to be *present* at the transition, not to be
  written by it, so a row could be given the whole gate and approval
  record in one early write and then walk the path without naming
  anything — and the record was not attributable to the event that claims
  it. The records are now set only by their transitions.

**The three that were wrong in this note** were:

* the published **DDL pin** was stale — the note carried the previous
  revision's hash, so the design of record and the code disagreed about
  the value that prevents the DDL from drifting;
* the published **control counts** were one too high in both numbers;
* the blanket justification for the six deferred invariants was wrong for
  three of them: N2, N3 and N6 contain no caller's presented value and
  **are** expressible as a trigger. They are deferred because the rule
  belongs to the API's authorisation, not to the storage's shape, and the
  note now says that.

**The two that were the same bug class in a different place** were:

* the command-level restore test compared its **trigger** inventory
  against the live schema but not its **table** or **index** inventory —
  and the dump is built from the named inventories, so an object the DDL
  creates and a list does not name was silently left out of the dump. All
  three inventories are compared now;
* the harness counted a control whose mutation **broke the build** as one
  invariant-specific failure. It is reported as INCONCLUSIVE now.

**The three nits** were about wording and one open boundary:

* the phrase "never reused" overstated the rule for an assignment that was
  never claimed and has no ledger: that one may be removed, which frees
  its id and its key again. The rule is about a second assignment taking a
  live one's identity;
* the claim rule refused the two states the state machine calls terminal
  but not the **gate**, which is described as the automation's terminal
  state elsewhere. That was an open question, and it is decided now: the
  gate is a **hand-off to the operator**, not a claim, and the refusal
  covers both sides of the statement.

### 7.9 The seventh review's findings

The seventh review returned **one blocking finding** — and it was in the
sixth review's own fix, which is the reason this campaign needed a seventh
round rather than a sixth.

**The blocking finding: the sixth review's fix was on the UPDATE path
only.** The rule that the gate's record and the approval are written only
by their own transitions was enforced by the state machine and by the two
standalone triggers — all of them UPDATE triggers. There was no
before-insert trigger, so an assignment could be **created** already
carrying the awaited action, or a complete forged approval.

That is worse than a missing rule, and the review was right to call it
blocking: the row is **permanently wedged**. The immutability rules refuse
every attempt to supply the value the transition needs; the "must supply"
rules refuse a transition that re-states the value it already has; and once
the row has a ledger entry it can no longer be deleted, so the assignment
can neither complete its path nor be removed. Two of the thirty-four
published properties were false, and so was the SPEC's wording.

The fix is a before-insert trigger refusing a row whose awaited action or
whose approval is non-null: the records are **born empty**.

The three minor findings were:

* **"a claim sets an owner and a lease" was not what the DDL enforced.**
  The two clauses required the owner and the lease to be *present* on a
  claim, not to be *set* by it, so a re-claim could carry the previous
  holder's lease forward — an expired one, even. The lease is now required
  to be **new** on a claim. The owner is stated as it is enforced: a claim
  leaves the row with an owner, and only a claim changes it. The schema
  cannot require the owner to have *changed*, because a renewal by the same
  holder leaves it unchanged and a trigger cannot tell that from a takeover
  that forgot to write the new owner — so **PR 2's claim always writes the
  owner**, and the note says so instead of claiming the database does it;
* **both command-level restore tests could pass vacuously.** Every
  assertion they made was also true of the database as it was *before* the
  restore, because the dump is built from the live schema and the rows are
  seeded before it: a handler that validated the dump, returned 0 and wrote
  nothing would have satisfied every one of them. Both tests now write a
  **marker** row after the dump is built, and require it to be **absent**
  from the restored database — and the first also requires the file to have
  been **replaced**, not written in place;
* **§8's opening sentence was wrong.** It said "each invariant was
  removed", but the controls and the invariants do not correspond one for
  one: the ledger's state vocabulary has no control, and two controls
  correspond to no invariant. The sentence names the exceptions now.

The two nits were:

* the DDL's header listed twenty-three rules and did not include the one
  the sixth review's fix added, and two of the in-file rule labels were out
  of numeric order. The list is twenty-four rules and the labels ascend;
* I20's wording — "cannot be deleted" — held only for the DELETE
  statement. A `DROP TABLE` destroys every row with no trigger firing, and
  `PRAGMA writable_schema` can remove the triggers themselves. The
  invariant is now stated with its scope, because a database file is not
  the threat model and the claim was wider than the guarantee.

### 7.10 The fifth review's non-blocking findings

* **the idempotency key was mutable**, and the acceptance suite even
  blessed the move ("an ordinary key change still works"). A key that could
  be changed would free the old one, and re-dispatching the same logical
  assignment with it would succeed — two logical assignments for one
  dispatch. The key is immutable now, and the suite asserts the refusal.
* **"a trigger can refuse a duplicate, but only an index can resolve one
  with `ON CONFLICT`" was false against the shipped DDL.** The before-insert
  trigger fires *before* the conflict resolution, so `ON CONFLICT DO NOTHING`
  aborts with the trigger's message instead of resolving the duplicate.
  The index is what lets the API **find** one without refusing the write;
  PR 2 must **select** the existing assignment after the refusal. §3.4 now
  says that.
* **the claim discipline never looked at the state**, so a cancelled or an
  approved assignment could be claimed — a lease on nothing, advancing the
  epoch of a row that will never be worked on again. Only a live assignment
  can be claimed now, and both sides of the statement are checked.
* **the lease was covered by no trigger at all**, so a claimed assignment
  could have its lease cleared, leaving a row that holds an epoch and an
  owner but no lease. The lease changes only on a claim now.
* **the transition into `operator_approved` did not require the evidence
  pointer**, although the note and the SPEC both describe the recorded
  approval as carrying one. All four of the approval's fields are required
  now.
* **the headings counted three reviews** while §7.4 and §7.5 documented the
  fourth. They count five now, in both the note and the DDL's own header.
* **a control whose mutation broke the build was counted as ONE failing
  test and ONE invariant-specific failure** — a number that reads like "the
  suite depends on this rule" when the truth is that nothing was measured.
  Such a control is reported as **INCONCLUSIVE** now.
* **the harness's import-time digest guard covered only the DDL**, and
  `db.go` is the other file a control mutates. It covers both now.

## 8. The negative controls

Each invariant that a control can remove was removed and the suite re-run.
The controls and the invariants do **not** correspond one for one, and the
exceptions are named rather than glossed: the ledger's `from_state` and
`to_state` vocabulary has no control (it is a column `CHECK`, and the
state vocabulary's own control removes the assignment's `CHECK` beside it),
and two controls correspond to no invariant — the unique idempotency index
alone, and the control that removes the whole migration.

The counts below are the **total** number of tests that fail; the second
column is the number that fail for a reason **other** than the DDL having
changed.

The **structural** failures are the ones that follow from the DDL's *text*
changing, whatever invariant is under test: the hash pin, the four migration
probes that enumerate the objects by name (AT1, AT2, AT6-recovery and AT8),
the object inventory, and the three control tests that drop an object by
name. They are listed in the harness so that the second column can be read as
"the tests that fail because the **behaviour** changed".

**Thirty-four** controls, and **thirty-three** of them have at least one
invariant-specific failure. The counts are **measured**, not asserted:
`scripts/continuity-001-controls.py` runs every control and prints them, and
it refuses to measure anything at all unless the **baseline is green** — a
control run against a broken tree reports nothing rather than a number.

| control | tests that fail | invariant-specific |
|---|---|---|
| the state-machine trigger is removed | 15 | 8 |
| the initial-state trigger is removed | 7 | 1 |
| the unclaimed-at-creation trigger is removed | 7 | 1 |
| the id-not-reused trigger is removed | 8 | 1 |
| the born-empty trigger is removed | 7 | 1 |
| the id-immutability trigger is removed | 8 | 2 |
| the key-immutability trigger is removed | 8 | 2 |
| the claim-needs-a-live-assignment trigger is removed | 8 | 2 |
| the lease-changes-only-on-a-claim trigger is removed | 8 | 2 |
| the tree-shape trigger is removed | 7 | 1 |
| the tree-shape-on-UPDATE trigger is removed | 7 | 1 |
| the depth-immutability trigger is removed | 8 | 2 |
| the role-immutability trigger is removed | 7 | 1 |
| the root-recipient-is-eligible trigger is removed | 7 | 1 |
| the root-recipient-is-eligible-on-UPDATE trigger is removed | 7 | 1 |
| the permitted-operations INSERT trigger is removed | 9 | 3 |
| the permitted-operations UPDATE trigger is removed | 8 | 2 |
| the eligible-recipients INSERT trigger is removed | 7 | 1 |
| the eligible-recipients UPDATE trigger is removed | 7 | 1 |
| the not-deletable trigger is removed | 8 | 2 |
| the awaiting-action-is-immutable trigger is removed | 10 | 4 |
| the approval-is-immutable trigger is removed | 11 | 5 |
| the ledger's id-not-reused trigger is removed | 7 | 1 |
| the ledger's monotonic-index trigger is removed | 7 | 1 |
| the ledger's no-UPDATE trigger is removed | 7 | 1 |
| the ledger's no-DELETE trigger is removed | 7 | 1 |
| **all four** claim-discipline triggers are removed | 8 | 1 |
| the unique idempotency index is removed | 6 | **0** |
| the idempotency rule's index **and** its trigger are removed | 10 | 3 |
| the composite containment foreign keys are removed | 2 | 1 |
| the root/child budget `CHECK` is removed | 2 | 1 |
| the state vocabulary `CHECK` is removed | 2 | 1 |
| the `typeof()` type guards are removed | 6 | 5 |
| the whole migration is not called | 53 | 47 |

Four of these are worth reading closely, because they are the places where the
counts are easy to misread:

* the **unique idempotency index alone** has **zero** invariant-specific
  failures. That is honest, and it is a deliberate redundancy: the
  id-not-reused trigger refuses a duplicate key on its own, so the index is
  not independently load-bearing. The index is still needed — it is what lets
  PR 2 **find** a duplicate without refusing the write — so the control that
  measures the **rule** removes both layers;
* the **composite containment foreign keys** are load-bearing for the
  **delete** path (a root with a child cannot be deleted), while the
  **tree-shape trigger** is load-bearing for the **insert and update** paths
  (a child cannot name a foreign root, a non-root, a parent from another
  tree, or a cycle). Removing the foreign keys alone changes the delete
  behaviour and nothing else;
* the **state vocabulary `CHECK`** sits *behind* the triggers — the
  initial-state trigger refuses every state that is not `enqueued` — so the
  test pins the vocabulary **statically** as well, and the control fails it;
* the **`typeof()` guards** cover five different defects — a REAL epoch that
  does not advance, a BLOB idempotency key that shadows a TEXT one, a BLOB in
  an evidence pointer, a BLOB in the gate's action, and a TEXT timestamp that
  sorts after every integer — so five tests are specific to them.

The harness itself was hardened four times, and each hardening was
necessary:

* a control that mutates **nothing** reports a count of zero that reads like
  "the suite does not depend on this invariant" — the most dangerous
  possible outcome — so the harness now **refuses** a mutation that does not
  change the file;
* the type-guard control used to delete the guard outright, which left
  `CHECK ()` behind and broke the schema; it now **neuters** each comparison
  instead, so the control measures the guard and not the schema;
* a mutator written as a plain function and invoked while the control list
  was being **built** mutated the tree at import time — before the pristine
  copy was taken — so every measurement was made against a corrupted tree.
  The mutators are now closures, and the harness takes the DDL's digest
  when it loads — above the control list — and refuses to run if the file
  has changed since;
* a failure left the working tree mutated, because there was no
  `try`/`finally`; the tree is now restored on **every** path, including a
  failure and a Ctrl-C. The harness also inherits the environment rather
  than hard-coding `HOME`, and it takes its pristine copy in a fresh
  temporary directory rather than removing a fixed path;
* a control whose mutation broke the build was counted as ONE failing test
  and ONE invariant-specific failure — a number that reads like "the suite
  depends on this rule" when the truth is that nothing was measured. Such a
  control is reported as **INCONCLUSIVE** now, with a zero count;
* the import-time digest guard covered only the DDL, and `db.go` is the
  other file a control mutates. It covers **both** now — and the guard
  earned its keep: a stale harness from an earlier run had left a mutation
  in the tree, and the **baseline** caught it and refused to measure
  anything.

**Running it mutates the checkout.** The harness edits the working tree in
place and restores it afterwards, so it must be run on a clean tree and
never on one with uncommitted work in the two files it touches
(`internal/core/migration_agent_assignments.go` and `internal/core/db.go`).
It needs the Go toolchain, and it runs the suite with the same FTS5 build
flags `make test` uses.

---

## 9. What is NOT in PR 1

* **No API, no CLI, no tool.** PR 2.
* **No OpenCode adapter.** PR 3.
* **No dispatcher, no activation gate.** PR 4.
* **No GitHub evidence contract, no `check` surface.** PR 5.
* **No first real assignment.** PR 6.

And the limitations that are **not** fixed by any of them, because they are
properties of the host rather than of the code:

> While a worker agent runs with the operator's credentials and host
> privileges, **no mechanism is inaccessible to it**. This PR does not claim
> otherwise. The authority boundary that PR 2 builds is a *capability* the
> worker is not handed, and a worker that can read the operator's files can
> read the capability — which is why the operator's decision keeps every
> privileged operation (merges, deployment, production data, security
> exceptions) **outside** the automation surface entirely, and why the
> dispatcher stays behind an explicit activation gate.
