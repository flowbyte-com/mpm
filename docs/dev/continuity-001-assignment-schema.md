# Continuity 001, PR 1 — the durable assignment storage

**Status:** implemented (PR 1 of the six-PR partition; tracking issue #258).
**Design of record for the storage:** `internal/core/migration_agent_assignments.go`.
**DDL pin:** SHA-256 `8abfcbf69cc08f5937eed70c65a28586a37deea53624239122e65dfb46c3f0ac`, asserted by
`TestAgentAssignmentDDLIsPinned`.

This note is the design the storage implements. The **published** behaviour is
`docs/SPEC.md`; this note is the reasoning, the reuse decisions and the
deferrals.

---

## 1. What PR 1 is, and what it is not

PR 1 is **storage**. It creates two tables, their indexes and their triggers,
and it makes the contract's hardest invariants true *in the database*, so a
wrong write is refused by the engine rather than by the convention of whichever
code path happened to make it.

PR 1 contains **no** API, **no** CLI, **no** OpenCode adapter, **no**
dispatcher and **no** evidence writer. Those are PRs 2–5. Nothing an agent can
call reaches this storage yet — the tables are written only by the migration.

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
`UNIQUE` per assignment, so the ledger is ordered and gap-free. The two
vocabularies stay **separate**: a work item is a commitment, an assignment is
an assignment.

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
timestamp** (`delivered`), never a side effect of the row having been read.
An assignment that was delivered and never acknowledged is **re-deliverable**,
not lost.

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

`operator_approved` is a **record, not a grant**. MPM records an approval that
was verified **outside** MPM — its source, the exact revision it covers, and a
pointer to the evidence — and it never manufactures or authenticates one. The
approval names the exact action (`approval_action`) and the exact revision
(`approval_revision`); a change to the head revision after the approval was
recorded leaves the approval **stale**, and staleness is observable rather
than implied.

### 3.2 The role-aware state machine

The transition path depends on the assignment's `role`, and the trigger knows
the role:

* an **implementer** goes `enqueued → delivered → acknowledged → accepted →
  in_progress → completed → review_passed → awaiting_operator →
  operator_approved`;
* a **reviewer** goes `enqueued → delivered → acknowledged → accepted →
  in_progress`, and ends at either `review_passed` (the verdict was passing)
  or `completed` (the review is done and it asked for changes). A reviewer
  never reaches the operator's gate: the reviewer approves nothing.

`cancelled` is the universal withdrawal and is legal from every state except
itself. `operator_approved` and `cancelled` are terminal.

### 3.3 The delegation tree and the inherited budgets

A **root** assignment (`depth = 0`) is the operator-authorised one. It is its
own root (`root_id = id`), it has no parent, and it **carries the budgets**:
`max_depth`, `max_tasks`, `max_concurrency`, `max_resource_units`,
`permitted_operations` and `eligible_recipients`.

A **child** (`depth > 0`) is an agent's delegation. It names its root and its
parent, and it carries **no budgets at all**.

That is the important part, and it is **structural**: there is no column in
which a child could widen what it was given. Inheritance is a property of the
schema, not a rule the code is trusted to apply. One CHECK constraint
enforces both halves — a root must carry the budgets and must name at least
one permitted operation; a child must carry none.

A root with an **empty** `permitted_operations` is refused. A root that may
do nothing is a silent misconfiguration, not a safe default.

### 3.4 The idempotency key

`idempotency_key` is `UNIQUE` across the table, so an accidental duplicate
dispatch cannot create a second logical assignment. The API (PR 2) turns the
conflict into "return the existing assignment", which is what makes a
duplicate dispatch **idempotent** rather than merely **refused**.

---

## 4. The lease, the epoch and the fence

`claim_epoch` is the fencing token. It advances by **exactly one** on every
claim, and the claim sets both the owner and a lease.

The trigger enforces:

* the epoch advances by exactly one — it cannot be skipped or reset;
* the owner changes **only** on a claim — an owner cannot be reassigned
  without the row ceasing to be a lease;
* a claim sets an owner and a lease.

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

A lease that expires without a transition is **recoverable**: the next claim
takes it and advances the epoch, which invalidates the previous holder.

---

## 5. The invariants the database enforces, and the ones it does not

| # | invariant | enforced by |
|---|---|---|
| I1 | an assignment is created `enqueued` | the initial-state trigger |
| I2 | the state machine is role-aware | the state-transition trigger |
| I3 | `cancelled` and `operator_approved` are terminal | the state-transition trigger |
| I4 | the claim epoch advances by exactly one | the claim-discipline trigger |
| I5 | only a claim changes the owner | the claim-discipline trigger |
| I6 | a claim sets an owner and a lease | the claim-discipline trigger |
| I7 | a root carries the budgets; a child carries none | the root/child `CHECK` |
| I8 | a root must name at least one permitted operation | the root/child `CHECK` |
| I9 | the delegation tree is contained in one project | the composite foreign keys |
| I10 | an assignment belongs to an existing project | the `projects(id)` foreign key |
| I11 | an idempotency key identifies one logical assignment | the unique index |
| I12 | the ledger is ordered and gap-free | `UNIQUE(task_id, event_index)` |
| I13 | the ledger cannot outlive its assignment | the foreign key |
| I14 | nothing with children can be deleted | `ON DELETE RESTRICT` |
| I15 | the state vocabulary is closed | the column `CHECK`, behind the triggers |

The invariants that are **not** in the database, because a `CHECK` constraint
cannot contain a subquery and a trigger cannot see a caller's presented value:

| # | invariant | enforced by | lands in |
|---|---|---|---|
| N1 | the target of a write presents the **current** claim epoch (the fence) | the `WHERE` clause of the writing statement | PR 2 |
| N2 | a child's `recipient` is among the root's `eligible_recipients` | the API | PR 2 |
| N3 | a child's depth is within the root's `max_depth` | the API | PR 2 |
| N4 | the tree's task count, concurrency and resource consumption are within the root's budgets | the dispatcher | PR 4 |
| N5 | a reviewer's findings are captured by a host-controlled handoff and published with their digest | the adapter | PR 3, PR 5 |
| N6 | an assignment is only created from an operator-authorised root | the API's authorisation | PR 2 |

Those are named here rather than left implicit, because "the schema enforces
it" and "the API enforces it" are different claims and only one of them is
true of each row.

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

## 7. What is NOT in PR 1

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
