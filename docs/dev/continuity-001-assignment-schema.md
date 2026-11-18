# Continuity 001, PR 1 — the durable assignment storage

**Status:** implemented (PR 1 of the six-PR partition; tracking issue #258).
**Design of record for the storage:** `internal/core/migration_agent_assignments.go`.
**DDL pin:** SHA-256 `c079375d4a222fcc5f9d56cfdc0a5bb5a8058dd26806f81608b23ef93405d0fe`, asserted by
`TestAgentAssignmentDDLIsPinned`.

This note is the design the storage implements. The **published** behaviour is
`docs/SPEC.md`; this note is the reasoning, the reuse decisions and the
deferrals.

---

## 1. What PR 1 is, and what it is not

PR 1 is **storage**. It creates two tables, seven indexes and twenty
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

`operator_approved` and `cancelled` are otherwise terminal.

### 3.3 The delegation tree and the inherited budgets

A **root** assignment (`depth = 0`) is the operator-authorised one. It is its
own root (`root_id = id`), it has no parent, and it **carries the budgets**:
`max_depth`, `max_tasks`, `max_concurrency`, `max_resource_units`,
`permitted_operations` and `eligible_recipients`.

A **child** (`depth > 0`) is an agent's delegation. It names its root and its
parent, and it carries **no budgets at all** — including no allow-list of its
own.

That is the important part, and it is **structural**: there is no column in
which a child could widen what it was given. Inheritance is a property of the
schema, not a rule the code is trusted to apply. One CHECK constraint
enforces both halves — a root must carry the budgets and must name at least
one permitted operation; a child must carry none.

A root with an **empty** `permitted_operations` is refused, and so is a root
with an **empty** `eligible_recipients`: a root that may do nothing, or that
may address nobody, is a silent misconfiguration rather than a safe default.
A root's own `recipient` must be among its `eligible_recipients` — a root
addressed to an agent the tree may not target is the same misconfiguration.

The tree is a **tree**, and that means four things:

* the root must be a **root** of the same project — a child may not cite a
  non-root as its budget root;
* the parent must be in the **same tree** — the same `root_id`;
* the parent's depth must be **exactly one less** — and that is what
  **excludes a cycle**: following the parent chain must strictly *decrease*
  the depth, so the chain cannot come back to where it started;
* nobody is their **own parent**.

The first three hold on the INSERT path and on the UPDATE path. Without the
depth condition, two UPDATEs could point two children at each other and every
other condition would hold — a "tree" with a loop in it, and an ancestor walk
over it that never terminates.

### 3.4 The idempotency key

`idempotency_key` is `UNIQUE` across the table, so an accidental duplicate
dispatch cannot create a second logical assignment. The API (PR 2) turns the
conflict into "return the existing assignment", which is what makes a
duplicate dispatch **idempotent** rather than merely **refused**.

The key's **type** is part of its identity: a `BLOB` and a `TEXT` with the
identical bytes are different values to SQLite, and a UNIQUE index holds
both — so the column is guarded with `typeof(...) = 'text'`.

---

## 4. The lease, the epoch and the fence

`claim_epoch` is the fencing token. It advances by **exactly one** on every
claim, and the claim sets both the owner and a lease.

The trigger enforces:

* the epoch advances by exactly one — it cannot be skipped or reset;
* the owner changes **only** on a claim — an owner cannot be reassigned
  without the row ceasing to be a lease;
* a claim sets an owner and a lease;
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
again. An **unclaimed** task may still be removed: it never issued a token.

---

## 5. The invariants the database enforces, and the ones it does not

Twenty-one properties are enforced by the schema itself rather than by
convention.

| # | invariant | enforced by |
|---|---|---|
| I1 | an assignment is created `enqueued` | the initial-state trigger |
| I2 | an assignment is created **unclaimed** (epoch 0, no owner, no lease) | the unclaimed-at-creation trigger |
| I3 | the state machine is role-aware | the state-transition trigger |
| I4 | `cancelled` is the universal withdrawal | the state-transition trigger |
| I5 | the role is **immutable** | the role-immutability trigger |
| I6 | the operator's gate **names its action** | the state-transition trigger |
| I7 | a recorded approval names its action, revision and source | the state-transition trigger |
| I8 | the gate's action is **immutable** once the gate is reached | the awaiting-action trigger |
| I9 | a recorded approval is **immutable** — it is evidence | the approval trigger |
| I10 | the claim epoch advances by exactly one | the claim-discipline trigger |
| I11 | only a claim changes the owner | the owner trigger |
| I12 | a claim sets an owner and a lease | the claim-owner and claim-lease triggers |
| I13 | a root carries the budgets; a child carries none | the root/child `CHECK` |
| I14 | a root must name at least one permitted operation, every permitted operation is a non-null string, and a root's own recipient is among its eligible recipients | the root/child `CHECK` + the permitted-operations and root-recipient triggers |
| I15 | the delegation tree is a **tree** — the root is a root, the parent is in the same tree at depth one less, and nobody is their own parent | the tree-shape triggers (INSERT **and** UPDATE) |
| I16 | the tree is contained in **one project** | the composite foreign keys + the tree-shape triggers |
| I17 | the idempotency key identifies one logical assignment, and it is `TEXT` | the unique index + the `typeof()` guards |
| I18 | the ledger is ordered, **monotonic**, append-only, and cannot outlive its assignment; the ledger's `from_state` and `to_state` come from the ten-state vocabulary | the ledger triggers, the `UNIQUE` constraint, the vocabulary `CHECK`s, the foreign keys and the `ON DELETE RESTRICT`s |
| I19 | nothing with children can be deleted, and a claimed assignment cannot be deleted | the `ON DELETE RESTRICT`s and the not-deletable trigger |
| I20 | every identifier, key and number has its **type** — a `TEXT` column cannot hold a `BLOB`, and a numeric column cannot hold a `REAL` or a numeric string | the `typeof()` guards |
| I21 | a lease implies an owner | the column `CHECK` |

The invariants that are **not** in the database, because a `CHECK` constraint
cannot contain a subquery and a trigger cannot see a caller's presented value:

| # | invariant | enforced by | lands in |
|---|---|---|---|
| N1 | the target of a write presents the **current** claim epoch (the fence) | the `WHERE` clause of the writing statement | PR 2 |
| N2 | a **child's** recipient is among the root's `eligible_recipients` | the API | PR 2 |
| N3 | a child's depth is within the root's `max_depth` | the API | PR 2 |
| N4 | the tree's task count, concurrency and resource consumption are within the root's budgets | the dispatcher | PR 4 |
| N5 | a reviewer's findings are captured by a host-controlled handoff and published with their digest | the adapter | PR 3, PR 5 |
| N6 | an assignment is only created from an operator-authorised root | the API's authorisation | PR 2 |

Those are named here rather than left implicit, because "the schema enforces
it" and "the API enforces it" are different claims and only one of them is
true of each row.

**One limit worth stating plainly:** I18's "the ledger cannot outlive its
assignment" is enforced by a **foreign key**, so it holds **while foreign
keys are enforced**. With the pragma off, an unclaimed task could be deleted
and its ledger entries would dangle. A **claimed** task is still protected,
because the not-deletable trigger does not depend on the pragma.

### 5.1 The ledger is monotonic, not gap-free

The event index is `UNIQUE` per assignment and a trigger refuses an index
that is not **strictly greater** than the current maximum. The ledger is
therefore **ordered and monotonic**. It is **not** gap-free: a writer that
reserves an index and then dies leaves a hole, and the ledger records what
happened rather than promising what did not.

The ledger is **append-only**, and that is enforced: triggers refuse an
`UPDATE` and a `DELETE` on it. A provenance ledger that can be rewritten is
not a provenance ledger.

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

## 7. What the two independent reviews found

The first version of this PR was reviewed independently, in the installed
sandboxed reviewer (`mpm-reviewer`, default-deny, `sandbox_exec` only), and
came back with **eleven blocking findings and thirteen non-blocking ones**.
Every blocking finding was a real defect, and every one was closed.

The first review's eleven blocking findings were:

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

The second review, of the revision that closed those eleven, returned **two
blocking findings and fourteen non-blocking ones**, and both blocking ones
were real:

1. **the delegation tree could be made into a CYCLE.** The tree-shape
   triggers closed the three named cases, but nothing excluded an ancestor
   cycle: two `UPDATE`s pointing two children at each other satisfied every
   stated condition. The fix is the parent's **depth** — the chain must
   strictly decrease — which excludes a cycle and enforces the depth
   consistency at the same time;
2. **a recorded approval could be REWRITTEN, not merely cleared.** The
   trigger fired only on a NULL or an empty value, so the revision could be
   re-pointed at a different one — which defeats the staleness comparison
   against the head revision. The trigger now fires on **any** change.

The second review's non-blocking findings were about the tests, the
documents and three more unenumerated holes, and they are closed too: the
permitted-operations guard tested `typeof(value)`, which is always TEXT for a
non-scalar element, so an **object** or an **array** passed as a permitted
operation — the discriminator is json_each's `type` column; the awaited action
could be rewritten at the gate; the "the path is checked first" claim was
false, because two separate triggers competed with the state machine — they
now fire only while the state is unchanged, so an illegal transition is
always reported as an illegal transition; a child's **depth** was unrelated
to its parent's; a root's own recipient need not be among its eligible
recipients, and an empty eligible list was accepted while an empty
permitted list was refused; the ledger's `from_state` and `to_state` carried
no vocabulary; the `typeof()` guards did not cover the evidence pointers and
the gate's record; the containment probe's comment claimed to isolate one
foreign key when it violated both; the negative-control counts were
understated, and the harness had three controls that mutated **nothing**
because they were wired to a function that was never called; the trigger
count was stale in two places; the AT8 "real dump" carried neither the
indexes nor the triggers and used a placeholder row that would not execute;
and the combined "relabel and transition in one statement" case was not
covered by any test.

---

## 8. The negative controls

Each invariant was removed and the suite re-run. The counts below are the
**total** number of tests that fail; the second column is the number that
fail for a reason **other** than the DDL having changed.

The **structural** failures are the ones that follow from the DDL's *text*
changing, whatever invariant is under test: the hash pin, the four
migration probes that enumerate the objects by name (AT1, AT2, AT6-recovery
and AT8), the object inventory, and the three control tests that drop an
object by name. They are listed here so that the second column can be read
as "the tests that fail because the **behaviour** changed".

| control | tests that fail | invariant-specific |
|---|---|---|
| the state-machine trigger is removed | 11 | 4 |
| the initial-state trigger is removed | 7 | 1 |
| the unclaimed-at-creation trigger is removed | 7 | 1 |
| the tree-shape trigger is removed | 7 | 1 |
| the tree-shape-on-UPDATE trigger is removed | 7 | 1 |
| the role-immutability trigger is removed | 7 | 1 |
| the permitted-operations INSERT trigger is removed | 8 | 2 |
| the not-deletable trigger is removed | 7 | 1 |
| the awaiting-action-is-immutable trigger is removed | 8 | 2 |
| the approval-is-immutable trigger is removed | 8 | 2 |
| the root-recipient-is-eligible trigger is removed | 7 | 1 |
| the root-recipient-is-eligible-on-UPDATE trigger is removed | 7 | 1 |
| the ledger's monotonic-index trigger is removed | 7 | 1 |
| the ledger's no-UPDATE trigger is removed | 7 | 1 |
| the ledger's no-DELETE trigger is removed | 7 | 1 |
| **all four** claim-discipline triggers are removed | 8 | 1 |
| the unique idempotency index is removed | 7 | 1 |
| the composite containment foreign keys are removed | 2 | 1 |
| the root/child budget `CHECK` is removed | 2 | 1 |
| the state vocabulary `CHECK` is removed | 2 | 1 |
| the `typeof()` type guards are removed | 4 | 3 |
| the whole migration is not called | 31 | 26 |

Three of these are worth reading closely, because they are the places where
the counts are easy to misread:

* the **composite containment foreign keys** are load-bearing for the
  **delete** path (a root with a child cannot be deleted), while the
  **tree-shape trigger** is load-bearing for the **insert and update** paths
  (a child cannot name a foreign root, a non-root, a parent from another
  tree, or a cycle). Removing the foreign keys alone changes the delete
  behaviour and nothing else, which is why one test is specific to them;
* the **state vocabulary `CHECK`** sits *behind* the triggers — the
  initial-state trigger refuses every state that is not `enqueued` — so the
  test pins the vocabulary **statically** as well, and the control fails it;
* the **`typeof()` guards** cover three different defects — a REAL epoch
  that does not advance, a BLOB idempotency key that shadows a TEXT one, and
  a BLOB in an evidence pointer — so three tests are specific to them.

The harness itself was hardened twice, and both hardenings were necessary:

* a control that mutates **nothing** reports a count of zero that reads like
  "the suite does not depend on this invariant" — the most dangerous
  possible outcome — so the harness now **refuses** a mutation that does
  not change the file;
* the type-guard control used to delete the guard outright, which left
  `CHECK ()` behind and broke the schema; it now **neuters** each
  comparison instead, so the control measures the guard and not the schema.

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
