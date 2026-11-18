# Continuity 001 — Gate A: the reuse/gap inventory and the contract

**Status:** Gate A complete (inventory + contract; the blocking decisions answered). **Tracking issue:** #258.
**Partition:** six PRs; this document accompanies **PR 1** (the durable assignment storage).
**Inspected:** `origin/main` @ `edb276de` (2026-11-17), PR #224 @ `d4ea2468`, the
installed OpenCode host (`opencode v2.3.0`), and the installed `mpm-reviewer`
agent definition.

This document is the inventory Gate A asks for. It states what already exists and
is reused, what does not exist and must be added, and the contract the added
machinery has to satisfy. The **blocking** operator decisions are at the end; the
engineering decisions are stated here with their reasons.

---

## 1. What was inspected, and the two facts that change the shape of the work

### 1.1 PR #224's Stage 1 is ALREADY IN `main`

This is the single most consequential fact of the inventory, and it was not
stated in issue #258. The project identity axis is **not** missing:

| | |
|---|---|
| commit | `d8b373fb` *feat(continuity): Stage 1 — additive Project-Scoped Continuity storage* |
| in `main`? | **yes** — `d8b373fb` is an ancestor of `origin/main` |
| tables | `projects`, `project_names`, `continuity_threads`, `continuity_checkpoints` |
| file | `internal/core/migration_continuity_project_model.go` |
| DDL provenance | transcribed **byte-for-byte** from §4.1 of PR #224 @ `d4ea2468`; SHA-256 `d0b3d7dd…70f1` |
| drift guard | `TestContinuityProjectModelDDLIsByteIdentical` (`internal/core/migration_continuity_project_model_test.go:551`) pins that hash |

So PR #224 is **not** a draft whose schema this work must design around — its
reviewed Stage 1 DDL is **shipped, hashed, and unchangeable without failing the
build**. This work **builds on it** and must not alter it.

Two invariants of that schema are inherited, not re-invented:

* **`project` is a containment boundary in the STORAGE.** `continuity_threads`
  and `continuity_checkpoints` carry a denormalised `project_id` held honest by
  **composite foreign keys** (`(thread_id, project_id)` → `continuity_threads(id,
  project_id)`; `(supersedes, thread_id)` → `continuity_checkpoints(id,
  thread_id)`), with every reference `ON DELETE RESTRICT` so a project with
  children cannot be cascade-destroyed.
* **`revision` is the optimistic-concurrency token.** `continuity_threads.revision`
  is bumped on every mutation, and a checkpoint records the revision it observed.
  This is the codebase's own fencing idiom, and the task lease below uses it.

### 1.2 The OpenCode host exposes a native session API — MPM needs no agent runtime

Measured against the running host (`opencode v2.3.0`, OpenAPI at
`/openapi.json`, 139 paths):

| operation | endpoint | use |
|---|---|---|
| create a session | `POST /api/session` | one dispatched task → one session, with `agent`, `title`, `location.directory` |
| send the assignment | `POST /api/session/{id}/prompt` | the task payload; returns a message id, the run continues asynchronously |
| observe the run | `GET /api/event` | the SSE stream; `session.idle`, `session.error`, `message.part.updated`, … (98 event types) |
| create a worktree | `POST /api/worktree` | an isolated tree for the assignment |
| wait for idle | `POST /api/experimental/session/{id}/wait` | a bounded wait rather than an unbounded poll |

**MPM implements no inference and no coding-agent loop.** It creates a session,
sends one prompt, and reconciles the run's *events* back into durable state. The
model, the tools and the agent loop stay OpenCode's.

### 1.3 Charlie's boundary is host-enforced, and the adapter must route through it

`/home/v/.config/opencode/agents/mpm-reviewer.md` is **default-deny** with exactly
one open action, `sandbox_exec`, which the plugin implements as a **bubblewrap
mount namespace** (`agent_installation/mpm-opencode/src/sandbox-exec.ts`): the
sandbox base and the repository copy are **HOST-resolved**, the command arrives
as a JSON array (never through a command-line parser), and **if the sandbox
cannot be established the command does not run** — there is no unsandboxed
fallback and no degraded mode.

Two consequences for this work:

* the adapter dispatches the reviewer **by naming the installed agent**
  (`agent: "mpm-reviewer"`), so the host's own permission verdicts apply. The
  adapter never implements a sandbox and never relaxes one;
* the findings are captured from the **host-controlled findings tree**
  (`$MPM_REVIEW_FINDINGS`), and published with the **SHA-256 of the file as
  captured**. Alpha cannot rewrite them without changing the digest — which is
  the whole of "published without allowing Alpha to rewrite them and
  misrepresent their provenance".

---

## 2. The reuse/gap matrix

### 2.1 Reused unchanged

| need | what is reused | where |
|---|---|---|
| project identity | `projects` + `project_names` (name resolution, `COLLATE NOCASE`, partial unique index over live rows) | `internal/core/migration_continuity_project_model.go:70,91` |
| the containment idiom | denormalised `project_id` held honest by composite FKs, every reference `ON DELETE RESTRICT` | same file, `:122,169` |
| the concurrency idiom | `revision` as an optimistic-concurrency token | `continuity_threads.revision` |
| the append-only ledger idiom | a per-item event table with `UNIQUE(item_id, event_index)` | `work_events` (`internal/core/schema.go:1083`) |
| the durable-outbox idiom | `status` + `attempt_count` + `next_retry_at` + a terminal error column | `epistemic_cascade_outbox` (`internal/core/schema.go:477`) |
| the audit trail | `system_audit_log` | `internal/core/schema.go:1377` |
| the evidence pointers | `evidence` + `mpm_log_to_changelog`'s strict "a commit hash must reference a real commit" contract | `internal/core/schema.go:355`; `internal/core/tools/handlers.go:3524` |
| the machine interface | one `tools.Registry` entry publishes both `mpm call <name>` and the MCP surface | `internal/core/tools/registry_list.go:23` |
| the restore allow-list | every new table is added to the dump/restore allow-list, or a restore refuses it | `internal/core/sql_dump_validator.go:150` |
| the reviewer sandbox | the installed `mpm-reviewer` definition, used **as installed** | `agent_installation/mpm-opencode/` |

### 2.2 The gaps, and what each one costs

| # | the gap | why the existing thing cannot serve | the addition |
|---|---|---|---|
| **G1** | **the assignment itself** | `works` is a *commitment*, not an *assignment*: `works.status` is `CHECK (status IN ('open','done','cancelled'))` (`schema.go:1059`) and the contract needs **ten** distinct states — `enqueued, delivered, acknowledged, accepted, in_progress, completed, review_passed, awaiting_operator, operator_approved, cancelled`. Widening that CHECK means rebuilding a **live** table in SQLite, and `works` has **no** recipient, **no** lease, **no** fencing token, **no** idempotency key, **no** dependencies and **no** project. | a new local-only table family: `agent_tasks` + `agent_task_events` |
| **G2** | **a lease with a fencing token** | nothing in the tree has one. `continuity_threads.revision` is an optimistic-concurrency counter, not a lease. | `agent_tasks.claim_epoch` — a monotonic fencing token, incremented on every claim; every owner-scoped write presents the epoch it claimed |
| **G3** | **an idempotency key** | nothing in the tree has one at the item level. | `agent_tasks.idempotency_key`, `UNIQUE` — "accidental duplicate issue dispatch does not create duplicate logical tasks" |
| **G4** | **a stable agent address** | `actor_kind` distinguishes `agent/human/import/system/unknown` — it is a **kind**, not a **who** (PR #224 §9 OD3: *"the substrate has no identity primitive"*). `broadcast.bcast_sessions.agent_id` is a **session-scoped** broadcast address with a heartbeat, not a durable address. | one `TEXT` column naming the intended recipient, plus the **capability** discipline in §4 — **not** an identity framework |
| **G5** | **a delivery channel with at-least-once semantics** | **the wake path is at-most-once and must not be promoted.** `scheduled_wakes.fired` is set to 1 in the same transaction that surfaces the row (`wake_tools.go:1125`, `UPDATE … WHERE id = ? AND fired = 0`), so a crash **after** the mark and **before** the agent acts **loses the wake permanently**. The budget is a throttle, not a loss window — but the *crash* window is real and is why the issue forbids promoting it into a reliable queue. | the assignment's own **state** and **ledger**: delivery is a **state transition with its own ledger entry** (the `delivered` event in `agent_task_events`), never a side-effect of "the row was read" |
| **G6** | **the GitHub evidence contract** | `mpm_log_to_changelog` binds a memory to a commit hash; it does not publish a manifest, and it cannot carry CI run IDs (which are known only after the push). | a committed, machine-readable manifest plus a human-readable report, and one GitHub issue comment that carries what is only knowable after the push |

### 2.3 Explicitly **not** reused, and why

* **`scheduled_wakes`** — at-most-once, with a real crash loss window (G5). The
  issue forbids promoting it, and it is not promoted here.
* **`session_handoffs`** — a *terminal* session-end record with a 90-day TTL
  (`internal/core/gc_tools.go:117`, `PruneHandoffs(90)`), keyed `UNIQUE` on
  `session_id`. It is a bridge between two sessions of the **same** agent; it is
  not a queue, and the absence of a handoff is not proof that work did not happen.
* **`works` / `work_events`** — kept as they are. A work item is a commitment; a
  task is an **assignment**. PR #224 §10 says the same about checkpoints. The
  two vocabularies stay separate, and the new ledger does not widen `works`'
  `CHECK` vocabulary.

---

## 3. The contract

### 3.1 The identity axes

| axis | lives in | survives a session? |
|---|---|---|
| **project** | `projects(id)` — already shipped | yes |
| **task** | `agent_tasks(id)` — new | yes |
| **agent address** | `agent_tasks.recipient` (a `TEXT` name) — new | yes, and **not** an identity claim |
| **session** | OpenCode's own session id, recorded **on** the task | no — it is an attribute of a delivery, not of the work |

### 3.2 The state machine

Ten states, and the separations between them are the point:

```
enqueued ──deliver──▶ delivered ──acknowledge──▶ acknowledged ──accept──▶ accepted
                                                                              │
                                                                          start
                                                                              ▼
   awaiting_operator ◀──gate── review-passed ◀──review── completed
          │
          └──(an externally verified approval is RECORDED)──▶ operator-approved

   cancelled is reachable from EVERY state, for every role — including
   operator-approved, because an operator may withdraw an approved
   assignment.
```

* **`delivered` ≠ `acknowledged`.** Delivery means the adapter handed the
  assignment to a runtime; acknowledgement means the recipient **took** it. An
  assignment that was delivered and never acknowledged is **re-deliverable**,
  not lost.
* **`acknowledged` ≠ `accepted`.** Acknowledgement is "I have it"; acceptance
  is "I will do it". Acceptance is the point at which the work is genuinely
  owned.
* **`completed` ≠ `review-passed` ≠ `awaiting_operator` ≠ `operator-approved`.**
  Completion is the implementer's claim; review is an **independent** party's
  verdict; `awaiting_operator` is the gate; `operator_approved` is a **recorded**
  external approval. **No agent can reach the gate, and MPM cannot grant the
  approval.** An implementer cannot skip its review to reach the gate.

The transition path is **role-aware**, and the database knows it: a reviewer
ends at `review_passed` (its verdict passed) or at `completed` (the review is
done and it asked for changes), and a reviewer never reaches the operator's
gate.


### 3.3 The lease and the fencing token

`agent_tasks.claim_epoch` is a monotonically increasing integer, incremented on
every claim.

* A claim is `UPDATE … SET claim_epoch = claim_epoch + 1, owner = ?, … WHERE id =
  ? AND state IN (…) AND (lease_expires_at IS NULL OR lease_expires_at < ?)`.
  The increment and the ownership write are **one statement**, so two claimants
  cannot both win.
* Every owner-scoped write (`acknowledge`, `accept`, `complete`, `report`) carries
  the epoch the caller claimed. A write whose epoch is **not** the row's current
  `claim_epoch` is **refused** — that is the fencing of a stale owner, and it is
  what makes a recovered-after-crash agent's late write harmless.
* A lease that expires without a transition is **recoverable**: the next claim
  takes it and bumps the epoch, which invalidates the previous holder.

### 3.4 At-least-once, and idempotent effects

The delivery is **at-least-once** and the completion effect is **idempotent**:

* a dispatch may deliver twice (the adapter crashed after the session was
  created but before the state was written). The duplicate is **harmless**
  because the task's transition is guarded by its state, and the completion
  effect is guarded by the **idempotency key** — a second completion of the same
  task is a no-op that reports the first result.
* **Exactly-once execution is not claimed anywhere.** The issue forbids it, and
  the substrate cannot promise it.

### 3.5 What is **not** promised

* no exactly-once execution;
* no absence of a wake/lease is proof that work did not happen;
* the reviewer findings' provenance is a **host-controlled capture with a
  digest**, not a cryptographic attestation;
* MPM does not implement inference, and does not own a coding-agent loop;
* **no cryptographic or OS-enforced operator separation** — while a worker
  agent runs with the operator's credentials and host privileges, no mechanism
  is inaccessible to it. The privileged operations are kept outside the
  automation surface instead of being fenced.

---

## 4. The authority model — the operator's decision

MPM has **no identity primitive**: `actor_kind` is a kind, not a who, and PR #224
OD3 established the workspace as a **single-operator trust domain** in which
*"recorded caller attribution is explicitly not verified identity"*.

The operator's decision (2026-11-18) is **external authority, internal
attribution**:

1. MPM records task assignments, agent attribution, review results, approval
   requests and state transitions. **These records do not themselves confer
   operator authority.**
2. Agents may perform explicitly scoped development and review work, and may
   produce branches, draft PRs, test evidence and proposed decisions.
3. **Operator-only actions — merges, production deployment, security
   exceptions and live substrate modifications — remain outside the
   Continuity 001 automation surface entirely.**
4. An operator-only task transitions to **`awaiting_operator`**. No
   agent-authored message, metadata field, database row or GitHub manifest can
   independently satisfy this gate.
5. An approval request must identify the **exact action and target revision**.
   A change to the target commit invalidates any approval tied to the earlier
   revision.
6. MPM may subsequently **record** externally verified approvals, with their
   source, revision and evidence reference. It must not manufacture or
   authenticate those approvals itself.
7. Future automated operator-gated actions require a separate trusted
   approval mechanism, credentials inaccessible to worker agents, and
   enforcement at the action boundary. That is a **distinct security
   milestone**.
8. **No general identity provider, role system, signing infrastructure or new
   authentication framework enters Continuity 001.**

What this means for the storage (PR 1):

* the state vocabulary carries **`awaiting_operator`** — the automation's
  terminal state — and `operator_approved` is a **RECORD** of an approval
  verified outside MPM, carrying its source, the exact revision it covers and
  a pointer to the evidence;
* `awaiting_action`, `approval_action`, `approval_revision`,
  `approval_source` and `approval_evidence` are the columns that carry it;
* the actor fields on the ledger are **attribution, not identity**.

And what the operator's decision explicitly does **not** claim:

> *Do not claim cryptographic or OS-enforced operator separation while Alpha
> retains access to the operator's credentials and host privileges.*

The design note states this in the same words.

### 4.1 Assignment creation — the operator's second decision

The operator's decision is **operator-authorised roots with bounded
agent-to-agent delegation**:

1. **Only an explicit operator-controlled entry point may activate a root
   assignment.** Agent-authored messages, GitHub comments and MPM records are
   **not** operator authorisation.
2. Agents may request or create **child** assignments, but only within an
   already-authorised root workflow and its immutable constraints.
3. Every child references its root and parent, **inherits their
   restrictions**, and carries a bounded objective, an intended recipient and
   an idempotency key.
4. The dispatcher enforces limits on **delegation depth, task count,
   concurrency, resource consumption, permitted operations and eligible
   recipients**. Descendants **share the root's cumulative budgets**.
5. Agents may propose unrelated tasks, but proposals are **inert** until
   authorised under a new or explicitly expanded root.
6. Independent review assignments may be generated automatically by an
   approved workflow. **A reviewer cannot approve its own work** or escalate
   privileges through delegation.
7. Receipt, acceptance, execution, review approval and operator approval
   remain distinct transitions.
8. No child may authorise merges, deployment, production-data access, security
   exceptions or expanded permissions.
9. Recorded attribution is never represented as authentication.
10. **No OS-enforced delegation security is claimed** while worker agents can
    access the same credentials and host resources as the dispatcher.

What this means for the storage (PR 1): the **delegation tree** and the
**inherited budgets** are not optional. A root assignment carries the budget
columns; a child carries **none**, so a child cannot widen what it was given
— there is no column in which to widen it. Inheritance is structural.

## 5. The evidence contract

The evidence must be reconstructible by a separate reviewer **without an agent
transcript**, and it must not contain a self-referential commit hash.

**Two parts, because the constraint forces two parts:**

1. **A committed manifest** (in the repository, on the branch):
   `docs/evidence/<task-id>/manifest.json` (machine-readable) and
   `docs/evidence/<task-id>/report.md` (human-readable). It carries what is
   knowable **at commit time**: the task id, the project, the issue/PR
   references, the **base** SHA, the **work head** SHA, the **reviewed** SHA, the
   findings file's SHA-256, the review verdict, the disposition, and the
   outstanding operator gates.

2. **One GitHub issue comment** (posted after the push, when the CI run ids
   exist): the **exact CI run ids**, the **exact reviewed SHA**, and the link to
   the manifest at the exact commit.

The manifest **cannot** contain the SHA of the commit that contains it — that
would be self-referential. The convention is therefore: *the manifest names the
head SHA of the work it describes; the commit that adds the manifest is found
with `git log --diff-filter=A -- <manifest path>`.* The reader resolves the
evidence's own commit from the tree, which is checkable and not asserted.

---

## 6. The six-PR partition — the operator's decision

The operator partitioned the deliverable into **six independently reviewable
PRs**, each with a bounded diff, focused acceptance tests, exact-SHA CI and an
independent review. PR #258 stays the parent tracking issue, and the six PRs
are associated with it.

| PR | scope |
|---|---|
| **1** | Durable assignment schema and persistence invariants — **this PR** |
| 2 | MPM assignment/delegation API and CLI |
| 3 | Native OpenCode adapter with reviewer isolation |
| 4 | Bounded dispatcher, recovery and reconciliation |
| 5 | GitHub evidence contract and read-only `check` discovery |
| 6 | First real assignment: implement issue #253 (skills P8) using the completed mechanism |

The operator's constraints on the partition:

* **PR 6 does not begin until PRs 1–5 have passed an end-to-end recovery and
  independent-review gate.**
* The mechanism is tested against **synthetic assignments and disposable
  state** before it is used for production engineering work.
* The **dispatcher stays disabled** until an explicit activation gate.
* An operator-controlled root-authorisation mechanism must be **genuinely
  inaccessible to worker agents**; attribution strings and database records
  alone do not establish authority.
* No automatic merge, no deployment, no production MPM data, no modification
  of the operator's checkout.
* **Do not implement the entire architecture in one branch.** PR 1 is PR 1.

The minimal additional machinery, spread across PRs 2–5:

| piece | shape | PR |
|---|---|---|
| the OpenCode adapter | a small Go package speaking the host's HTTP API (`/api/session`, `/api/session/{id}/prompt`, `/api/event`, `/api/worktree`) | 3 |
| the dispatcher | a **bounded, operator- or agent-triggered single pass** — no daemon, no ticker | 4 |
| the assignment/delegation API | one `tools.Registry` entry → both `mpm call <tool>` and the MCP surface | 2 |
| the evidence writer | a manifest + report writer, and the findings digest capture | 5 |

**Explicitly not built, in any of the six PRs:** no model router, no HAL coding
loop, no framework replacement, no autonomous merging, no background daemon, no
UI, no general agent-identity framework.

## 7. The blocking decisions, and how they were answered

| # | decision | the operator's answer |
|---|---|---|
| **BD1** | How is the operator's authority expressed, given that MPM has no identity primitive? | **External authority, internal attribution.** MPM records; it does not grant. `awaiting_operator` is the gate; `operator_approved` is a recorded external approval. No identity framework enters Continuity 001. |
| **BD2** | Who may create an assignment? | **Operator-authorised roots with bounded agent-to-agent delegation.** A root needs the operator's entry point; children are bounded by the root's immutable constraints and share its budgets. |
| **BD3** | How is the deliverable split? | **Six independently reviewable PRs.** PR 1 only, now. |

Everything else in this document is an engineering decision with a stated
reason, and none of it blocks the build.
