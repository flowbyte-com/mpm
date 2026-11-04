# Project-Scoped Continuity — architecture

**Date:** 2026-11-04
**Baseline:** `main` @ `f8d4b95aefe60a0880409a8905ce2c4d92d22c00` (merge of PR #218)
**Status:** architecture and acceptance specification. **No production schema migration, no
implementation, no merge.**

---

## 0. What this document is

Continuity preserves an agent's current position inside a durable project context, so that another
session, model or agent can resume meaningful work without replaying a conversation.

This document answers the ten deliverables: the current-state inventory, the proposed model, an
example lifecycle, the schema and API, the CLI/MCP contract, the threat model, the compatibility
analysis, the stage sequence, the acceptance matrix, and the operator's open decisions.

**The product contract it must satisfy:**

> *Let's work on the form builder in AgentShell.*

A compatible agent must (1) recognise AgentShell as an existing project, (2) resolve its durable
identity independently of the current directory, (3) find the relevant continuity thread if one
exists, (4) retrieve a compact, evidence-backed current checkpoint, (5) continue from the next
meaningful action, and (6) create a new thread when none exists — without creating a duplicate
project.

**What this is not.** No boards, no assignments, no scheduling, no progress metrics, no workflow
management. §10 records the non-goals explicitly, because the most likely failure mode of a design
like this is feature creep into project management.

**The single most important finding of the investigation** is in §1.3: **the substrate has no
project axis at all.** Every durable artifact is scoped to a session, a workspace, or nothing. The
one `project_id` column in the tree belongs to an ingest-only observation ledger that nothing joins
against durable artifacts, and MPM's own source states the gap in as many words. Continuity therefore
requires a new data model — and the design below is the smallest one that meets the contract.

---

## 1. Current-state inventory

### 1.1 The identity axes that exist

MPM has **three session identity axes and no project axis.**

| Axis | Owner | Lifetime | Where it lives |
|---|---|---|---|
| `session_id` | the host / the CLI process | one `mpm call` | `cmd/mpm/session_runtime.go:44-52` — fresh per process |
| `mpm_session_id` | **MPM** | one interaction lifecycle, rotated explicitly | `<workspace>/active.json` |
| `framework_session_id` | the host | the host's session | env / payload |

`mpm_session_id` is the substrate's continuity axis today. Its contract
(`internal/core/session_identity.go:27-41`) is that `active.json` is the source of truth, that
acquisition is atomic across processes (flock + read-or-create), that **reads never allocate**, and
that the next genuine session gets a new id.

`BandContinuity` in the selection policy confirms the vocabulary: *"identity threads (same MPM/framework
session, handoff pickup, working scratchpad)"* (`internal/core/contextual_selection.go:55-62`). The
continuity unit today is **the session** — not a project, not a body of work.

### 1.2 The entity lifecycles that exist

| Entity | Scope | Durability | Terminal states | Fits "a project"? |
|---|---|---|---|---|
| `works` + `work_events` | a commitment | purged (`work_purge.go`, reason-coded) | `done` / `cancelled` / `archived` | **No** — a task, not an identity |
| `memories` | content | decay, retirement, prune | soft-deleted | No |
| `lessons` (a VIEW over `lessons_base`) | content | pruned | soft-deleted | No |
| `topics` | a label hierarchy | no lifecycle | deleted | No — free-form, `name UNIQUE`, no durability guarantee |
| `session_handoffs` | **one session** | 90-day TTL (`PruneHandoffs`) | `clean` / `crashed` / `interrupted` / `force_end` | No — written at session **end** |
| `ephemeral_scratchpad` | **one session** (PK) | 24 h TTL | discarded | No — explicitly volatile |
| `evidence` | a typed observation | expires | — | No |
| `lifecycle_events` | an observation ledger | append-only | — | No — ingest-only |

The three closest analogues to a continuity checkpoint are:

1. **`session_handoffs`** — the end-of-session continuity record. Its shape (`summary`,
   `commitments`, `open_questions`) is exactly what a checkpoint needs, but it is written by
   `EndSessionV2` at session end. The brief requires recovery *"without requiring a session-end
   event"*, so a handoff cannot serve.
2. **`ephemeral_scratchpad`** — the intra-session working note. It has the right *timing* (written
   at will) but the wrong *scope* (one row per session, 24 h TTL, explicitly "not ready for permanent
   memory").
3. **`works` / `work_events`** — the durable multi-session commitment ledger with an event-sourced
   history. Right *durability*, wrong *meaning*: a work item answers "is this done?", a thread
   answers "are we still here?".

### 1.3 The project axis: proven absent

**There is no project identifier attached to any durable artifact.** Verified exhaustively:

- **`works` has no project, repo, workspace or codebase column.** MPM says so itself at
  `internal/core/db.go:7199-7201`: *"Nothing in the data model could supply the missing fact. Work
  carries no project or repository field (see schema.go's `works` table), ActiveContext and
  ActiveState carry session provenance only, and the evidence table has no origin column."*
- **`memories` has no project column and no `metadata` key for one.** The only `json_extract`
  targets on `memories.metadata` anywhere in the tree are `$.kind`, `$.status`, `$.source` and
  `$.provenance.*`. `memories.source_db` identifies the **ingest pipeline** (default `'openclaw'`),
  not a project.
- **`topic_memberships` links only memories and sessions** — there is no topic→work link and no
  topic→project link.
- **`session_handoffs` is scoped to a session**, not a project or a workspace. One `mpm_session_id`
  can have many handoffs; the table lives in the workspace's own database.
- **`tool_invocations` has no cwd and no repo column** — only the three session axes, plus
  `framework_name` and `actor_kind`.
- **The only `project_id` column in the entire tree** is `lifecycle_events.project_id`
  (`internal/core/schema.go:914`). It is a **host-supplied field on the mpm-event/0.1 adapter ingest
  ledger**: a closed five-value source vocabulary (`explicit-env`, `explicit-file`, `git-remote`,
  `host-git-remote`, `unresolved`), two accepted id forms (`pj_` + 32 hex derived, or an explicit
  1–128-char id), and the rule *"project_id: never fabricated"*. **Nothing joins it to `works`,
  `memories` or `session_handoffs`.** It is an observation attribute, not a substrate identity.
- **There is no repository scoping anywhere.** No table, column, index or query filters by
  repository, repo path, git remote or codebase.

And the substrate has already ruled on the obvious shortcut. `internal/core/db.go:7181-7234`
documents the removal of automatic git evidence and encodes the rule:

> *Git evidence without an explicitly identified repository is unavailable, not inferred.*

Continuity inherits that rule. **The current directory is never a resolution input** (§2.5).

### 1.4 The integration seams that exist

| Seam | Shape | Use in this design |
|---|---|---|
| **The `mpm://` pointer convention** | `mpm://memory/<id>`, `mpm://work/<id>`, `mpm://lesson/<id>`, `mpm://theory/<id>`, `mpm://blob/<id>`; resolved by `mpm_resolve` | add `mpm://project/<id>` and `mpm://thread/<id>` |
| **The polymorphic reference idiom** | `(artifact_id, artifact_type)` with the closed vocabulary `memory\|theory\|decision\|lesson\|work` — `evidence.artifact_type` (`internal/core/schema.go:355`) | reuse it verbatim for checkpoint references |
| **The provenance vocabulary** | `ProvenanceArtifactTypes` = `memory, theory, lesson, decision, handoff, directive, work` (`internal/core/schema.go:23-25`), enforced by a CHECK on `artifact_provenance` | **not** extended in v1 (§4.5) |
| **`active.json`** | `ActiveState` — persona, modes, `mpm_session_id` | **not** extended in v1 (§4.6) |
| **The tool registry** | one slice, `mpm_<noun>` names, `action` + `params`, `oneOf` per action; both `mpm call` and the MCP server iterate it (`internal/core/tools/registry_list.go:5-22`) | one new entry, `mpm_continuity` |
| **The CLI facade pattern** | `mpm work item <sub>` is "a thin discoverable facade over `mpm call mpm_work`" (`cmd/mpm/handlers_work.go:78-84`) | `mpm continuity <sub>` |
| **The transaction layer** | `WithTx` (restart on a stale snapshot, rollback on error), `execTracked` (budget 5) | every write |
| **The CAS idiom** | `WHERE id = ? AND <state> = ?` — wake resolution, lesson promotion, handoff read marker | the thread's `revision` column (§2.8) |
| **The identity-derived id** | `evidence` derives its id from the observation identity and inserts `ON CONFLICT(id) DO NOTHING` | the checkpoint's restart-safe insert (§5, T1) |
| **The bounded-projection budgets** | `contextual_materialization`: 6000-byte detail budget, 800-byte per-item cap, 20-item hard max | the checkpoint's budgets (§2.3) |
| **The restore allow-list** | `CanonicalMPMSchema` / `RuntimeCanonicalSchema` (`internal/core/sql_dump_validator.go:141-240`) | the three new tables must be added (§6.3) |

### 1.5 The substrate's own refusals, which this design inherits

- **No ambient probing.** The ghost-database incident (`internal/core/config/config.go:633-656`)
  is why `GetWorkspace` no longer probes the CWD. The same discipline applies to project matching.
- **No inferred repository.** `db.go:7181-7234`.
- **No fabricated project id.** `lifecycle_events.go:500`.
- **Reads never allocate.** `session_identity.go:27-41`.
- **No `SetMaxOpenConns(1)`.** `db.go:1663-1679` — many connections, writes serialised by SQLite's
  file lock.
- **No exactly-once claims.** The delivery-state work (§7 of the phase-3 record) states the rule for
  external effects; the same discipline applies here: a checkpoint records what an agent *said*, and
  the thread pointer records what was *committed*. Neither is a proof that anything outside the
  database happened.

---

## 2. The proposed model

### 2.1 Why a new data model, and why exactly this one

**A new data model is required.** The case is not "the schema looks different" but four specific
structural mismatches:

1. **No existing entity has the required durability.** The brief requires that a project's identity
   *"survive indefinitely until explicitly deleted by the user"*. `works` is purged with reason
   codes; `session_handoffs` has a 90-day TTL; `ephemeral_scratchpad` has a 24-hour TTL; `topics`
   have no lifecycle at all. None can hold a permanent stub.
2. **No existing entity has a project axis** (§1.3, exhaustively verified). Extending `works` with a
   `project_id` column would attach a project identity to a *task* — and a task is deleted, completed
   and cancelled. A project must outlive every task in it.
3. **A thread cannot be a work item.** `works`' state machine is `open → done | cancelled`, with
   `done → open` and `cancelled → open` reopen transitions
   (`internal/core/work.go:53-63`). A thread's states are `active | dormant | completed | superseded`.
   The overlap on the word "completed" is a coincidence; the semantics differ: a completed work item
   is a *finished commitment*, a completed thread is a *finished line of conversation* inside a
   project that continues. Mapping one onto the other would import the forbidden workflow
   vocabulary.
4. **A checkpoint cannot be a handoff.** A handoff is written by `EndSessionV2` at session end and
   is terminal. The brief explicitly requires recovery *"without requiring a session-end event"*.

**But the design extends rather than duplicates wherever the substrate already has the right
mechanism.** The extensions are enumerated in §6.4: two pointer kinds, one new tool, one CLI facade,
one new candidate kind (deferred to Stage 4), and three entries in the restore allow-list.

### 2.2 The three tables

```
projects              — the permanent stub: an identity that outlives every thread
project_names         — the resolution domain: canonical name + aliases, one row per key
continuity_threads    — an independent line of work inside a project
continuity_checkpoints— the bounded, structured, append-only resume brief
```

Three tables is the minimum: a project without names cannot be resolved; a thread without a project
has no owner; a checkpoint without a thread has no home. A fourth table for checkpoint references
was considered and rejected — see §2.3.

### 2.3 The checkpoint: bounded, structured, reference-bearing

A checkpoint is **not** a transcript. It has exactly four sections, and the sections answer four
different questions:

| Section | Question | Holds |
|---|---|---|
| `facts` | *what is established?* | references to **established** knowledge: `memory`, `decision`, `lesson` |
| `hypotheses` | *what is unproven?* | references to `theory` |
| `state` | *where do things actually stand?* | references to `work` records (and, where the state is a durable fact rather than a task, `memory` / `decision` / `lesson`) |
| `next_actions` | *what happens next?* | short bounded action strings, each optionally pointing at a `work` record |

The separation is **enforced**, not advisory: a `theory` in `facts` is a validation error, and a
`memory` in `hypotheses` is a validation error. The reference vocabulary is the substrate's existing
`evidence.artifact_type` set — `memory | theory | decision | lesson | work`
(`internal/core/schema.go:355`) — reused verbatim rather than re-invented.

**References, not duplicated contents.** A checkpoint never copies a memory's text or a work item's
content. It points at ids; the read path materialises them through the existing per-kind read paths
and returns bounded summaries plus `mpm://` pointers. A `note` on a reference is permitted but is a
**bounded annotation** ("why this is here"), not a copy — it cannot exceed 256 bytes and cannot
restate the artifact's content.

**One orientation line.** A pure list of ids is not resumable, so a checkpoint carries a `headline`
of at most 200 bytes: one line saying where things stand. That is the only free-form prose in the
structure, and it is the part that cannot be derived from the references.

**Budgets**, grounded in the substrate's existing numbers:

| Budget | Value | Anchor |
|---|---|---|
| checkpoint payload (all four sections + headline) | **4096 bytes** | below `contextual_materialization`'s 6000-byte detail budget |
| per-reference note | **256 bytes** | below the 800-byte per-item cap |
| per-`next_action` text | **256 bytes** | as above |
| `facts` / `hypotheses` / `state` / `next_actions` counts | 12 / 6 / 12 / 8 | — |
| `resume` response (materialised) | **6000 bytes** | matches the materialization budget exactly |

**Why the references live in JSON columns, not a fourth table.** A `checkpoint_refs` table would give
an index nobody needs (a checkpoint is read whole) and would make the bounded-size guarantee a
database-level concern rather than a code-level one. The substrate already stores exactly this shape
on `session_handoffs` (`commitments` and `open_questions` are JSON arrays) and already enforces
byte budgets in code at the materialization boundary. Following both precedents keeps the guarantee
where the substrate keeps it.

**Dangling references are allowed and reported.** A checkpoint may outlive the artifact it points at.
`resume` reports a reference whose target no longer exists as `missing` rather than failing — a
checkpoint must never be unreadable because one memory was shredded.

### 2.4 Thread selection — conservative by construction

The brief requires *"Find the relevant continuity thread, if one exists."* "Relevant" must be
resolved **deterministically**, and the substrate's stated rule — *"unavailable, not inferred"* —
applies to threads as much as to repositories.

The selection ladder, applied in order:

| # | Selector | Rule | If it does not resolve to exactly one |
|---|---|---|---|
| 1 | `thread_id` | exact | error — the thread does not exist, or belongs to another project |
| 2 | `focus` | exact match on the thread's `focus` slug; then a **unique** case-insensitive match | **ambiguous** → list the candidates |
| 3 | `title` | exact; then a **unique** case-insensitive match | **ambiguous** → list the candidates |
| 4 | *(none)* | threads with `state='active'` | zero → step 5; more than one → **ambiguous** → list them |
| 5 | *(none)* | threads with `state='dormant'`, most recently updated first | zero → "no thread"; more than one → **ambiguous** |

`completed` and `superseded` threads are **never** auto-selected. A superseded thread is
history.

**"Ambiguous" is a first-class outcome, not an error.** The response lists the candidates with their
`id`, `title`, `focus`, `state` and `updated_at`, and asks the caller to disambiguate. This is the
conservative behaviour the brief asks for: when two agents are working two lines of the same
project, a bare "resume" must not silently pick one of them.

**Why `focus` exists at all.** The product contract's phrase is *"the form builder in AgentShell"* —
a recurring label, not the thread's title. Without a dedicated `focus` slug, resolving it would need
substring matching against titles, which is exactly the fuzzy inference this design refuses. With a
normalised slug (`form-builder`), the match is exact.

**`focus` is unique per project** among live, non-superseded threads, enforced by a partial unique
index. Two agents that race to open a thread for the same focus produce one thread and one clear
error naming the winner — not two near-identical threads.

### 2.5 Project matching — explicit and conservative

The brief requires the agent to *"Recognise AgentShell as an existing project"* and to *"resolve its
durable identity independently of the current directory"*, with *"no silent duplicate project
creation"*.

**Resolution** (`mpm_continuity resolve --name <name>`) is a **two-tier exact match** over the
`project_names` table:

1. **Exact** match on the stored key → that project.
2. **Case-insensitive unique** match (`COLLATE NOCASE`) → that project, with the response noting the
   case normalisation.
3. **Case-insensitive ambiguous** (two projects whose names differ only by case) → **refuse**, and
   list both.
4. **No match** → **do not create**. Return the project's absence and a bounded list of live
   project names, so the agent can choose. Creation is a separate, explicit act.

**No fuzzy matching.** No substring, no token overlap, no embedding similarity, no Levenshtein. A
project named "AgentShell" must never be matched by "agent" or "shell".

**No directory, ever.** The current working directory is not an input to resolution, not a
tie-breaker, and not a fallback. `MPM_WORKSPACE` selects the *database*, which is the substrate's
existing scoping mechanism and is not a project identity. A repository path is **not** an input in
v1 (§10, non-goal 2).

**Creation is explicit and refuses duplicates.** `mpm_continuity create --name AgentShell`:

- normalises the name (trim, collapse internal whitespace; case preserved);
- **refuses** if a live project already holds that key — exact or case-insensitively unique — and
  returns that project's id instead ("a project named AgentShell already exists: cpj_…"). This is
  the "no silent duplicate" guarantee, enforced by the caller *and* by the UNIQUE index on
  `project_names`;
- **refuses** if the name is the id of a live project (see §2.6);
- if a **tombstoned** project once held the name, the response says so and the caller decides. The
  new project gets a **new id**. The tombstone is what makes the duplication *visible* rather than
  silent.

### 2.6 Rename and alias

References are **by id**, so a rename breaks nothing mechanically. The alias table preserves
*human and agent* references by name.

`mpm_continuity rename --project cpj_x --name NewName`:

1. the new name must not already resolve to a live project (exact or case-insensitively unique) →
   refuse otherwise;
2. the old canonical key is **demoted to an alias** (one UPDATE);
3. the new name becomes the canonical key (one INSERT);
4. both happen in **one transaction**, so a resolver sees either the old key (as an alias) or the new
   one — never a gap in which the project is unreachable.

`mpm_continuity alias --project cpj_x --add <key>` / `--remove <key>`:

- an alias must not be the id of a live project, and a live project's id must not be added as an
  alias to a *different* project. This keeps the id space and the name space disjoint. Continuity's
  own ids use the `cpj_` prefix, which is disjoint from the substrate's existing `pj_` derived form
  (`lifecycle_events.project_id`), so an adapter-reported `pj_…` can be adopted as an alias without
  any possibility of collision with a generated id.

### 2.7 Deletion semantics — deliberate, and different per entity

| Entity | Effect | Reversible? |
|---|---|---|
| **project** | soft-delete: `projects.deleted_at` is set; the project is **no longer resolvable by name**; its `project_names` rows are **withdrawn** (deleted) so the name is free again | the id and the timestamps remain as a tombstone; a later create with the same name yields a **new** id, and the create response reports the tombstone |
| **thread** | soft-delete: `continuity_threads.deleted_at` is set; the thread stops being selectable; its checkpoints are retained | yes — an undelete is a single UPDATE; the `revision` still fences concurrent writers |
| **checkpoint** | **not deletable in v1** — the ledger is append-only | — |

Deleting a project **does not** delete its threads or checkpoints. They become unreachable by name
and are reachable only by id. That is deliberate: a hard purge needs a reason-code and a backup flow
like `work_purge`, and inventing a second destructive mode in the same change is how data gets lost.
A project purge is **Stage 5** and is explicitly out of scope here (§10, non-goal 6).

**Deletion requires an explicit confirmation token.** `mpm_continuity delete --project cpj_x
--confirm cpj_x`. The confirmation is the project id itself, so the operator cannot delete a project
they did not name.

### 2.8 Optimistic concurrency

The brief requires *"optimistic concurrency protection against stale agents"*. The substrate's
existing CAS idiom is a **state-column predicate** (`WHERE id = ? AND state = ?`), never a version
number — the inventory found no version-based CAS anywhere in the tree.

For a thread, a state predicate is not sufficient: the state changes rarely, but the *checkpoint
pointer* changes constantly, and two agents writing checkpoints to the same thread is the exact race
to fence. So the thread carries a **`revision` counter**:

```sql
UPDATE continuity_threads
   SET revision = revision + 1,
       last_checkpoint_id = ?,
       updated_at = ?
 WHERE id = ? AND revision = ? AND deleted_at IS NULL
```

`RowsAffected == 0` ⇒ the caller is **stale**. The response says so, and reports the current
revision and the current checkpoint's id, so the caller can re-read and rebase. The checkpoint is
**not** written.

`expected_revision` is a **required** parameter of every thread-mutating write. The agent already
holds it — `resolve` and `resume` both return it — and making it optional would make the guarantee
conditional on a client remembering to use it.

**One mechanism, not two.** A revision counter also fences state transitions, focus changes and
deletes, so there is a single stale-detection rule rather than a per-operation one. The existing
state-predicate idiom and the revision predicate compose: the WHERE clause carries both.

### 2.9 The two state machines

**Project** — deliberately minimal. A project is either **alive** or **deleted**; there is no
"completed project", because a project is an identity, not a goal. `projects.deleted_at IS NULL` is
alive. Nothing else about a project has a state.

**Thread** — a closed vocabulary with an explicit transition table, mirroring the substrate's
`isValidWorkTransition` pattern (`internal/core/work.go:53-63`):

```
active     → dormant | completed | superseded
dormant    → active  | completed | superseded
completed  → active
superseded → (terminal)
```

- **active** — the line currently being worked.
- **dormant** — paused but resumable; the state a thread is left in when work moves elsewhere.
- **completed** — the line's goal was reached. Reopenable, because a project outlives its threads.
- **superseded** — replaced by another thread; `superseded_by` points at the replacement. The thread
  is retained as history and is never auto-selected.

`deleted_at` is orthogonal to the state, exactly as `archived_at` is orthogonal to `works.status`
(`internal/core/work.go:14-22`) — deletion is a visibility projection, not a lifecycle state.

---

## 3. Example lifecycle — AgentShell and two concurrent agents

The scenario: a user says *"Let's work on the form builder in AgentShell."* Two agents are live.
Agent **A** wants the form builder; agent **B** is on the API surface.

### 3.1 The project already exists

```jsonc
// the project stub
{"id":"cpj_3f9a…","created_at":1767225600,"updated_at":1767225600,"deleted_at":null}
// the resolution domain
{"name":"AgentShell","project_id":"cpj_3f9a…","kind":"canonical"}
{"name":"agentshell","project_id":"cpj_3f9a…","kind":"alias"}
{"name":"the form builder repo","project_id":"cpj_3f9a…","kind":"alias"}
```

There is one canonical name and two aliases. All three resolve to `cpj_3f9a…`. The project has no
repository binding, no directory binding, and no ambient state — it is found by name alone, from any
directory, in any session, under any model.

### 3.2 Agent A resolves the project and finds two active threads

```jsonc
// mpm_continuity resolve --name AgentShell
{
  "success": true,
  "project": {"id":"cpj_3f9a…","name":"AgentShell","aliases":["agentshell","the form builder repo"]},
  "threads": [
    {"id":"ct_a1…","title":"Form builder rework","focus":"form-builder","state":"active","revision":7,"updated_at":1767300000},
    {"id":"ct_b1…","title":"Public API surface","focus":"api","state":"active","revision":3,"updated_at":1767290000}
  ],
  "resolved_by": "exact name match"
}
```

Both threads are active. An unqualified `resume` would be **ambiguous** — two agents, two lines.
Agent A's user named the form builder, so Agent A selects by focus:

```jsonc
// mpm_continuity resume --project cpj_3f9a… --focus form-builder
{
  "success": true,
  "project": {"id":"cpj_3f9a…","name":"AgentShell"},
  "thread":  {"id":"ct_a1…","title":"Form builder rework","focus":"form-builder","state":"active","revision":7},
  "checkpoint": {
    "id":"ck_77…","created_at":1767300000,"supersedes":"ck_76…",
    "headline":"Validation lives in form.ts; the submit path is still on the old action shape.",
    "facts": [
      {"kind":"decision","id":"d_91…","pointer":"mpm://memory/d_91…","note":"Submit stays a plain form POST"},
      {"kind":"lesson","id":"l_04…","pointer":"mpm://lesson/l_04…","note":"Do not move validation into the reducer"}
    ],
    "hypotheses": [
      {"kind":"theory","id":"m_55…","pointer":"mpm://theory/m_55…","note":"A schema-first validator will cover the 3 open field cases"}
    ],
    "state": [
      {"kind":"work","id":"w_12…","pointer":"mpm://work/w_12…","summary":"Port the validator","status":"open","verification":"unverified"}
    ],
    "next_actions": [
      {"text":"Add the three missing field cases to the validator","ref":{"kind":"work","id":"w_12…"}}
    ]
  },
  "next_action": "Add the three missing field cases to the validator",
  "bytes": 1180
}
```

The checkpoint carries **pointers**, not copies. The agent follows `mpm://work/w_12…` when it needs
the full work item, and `mpm://theory/m_55…` when it wants the hypothesis. The facts and the
hypothesis point at artifacts that carry their own evidence; the checkpoint does not duplicate their
contents.

### 3.3 Agent A writes a new checkpoint

```jsonc
// mpm_continuity checkpoint --project cpj_3f9a… --thread ct_a1… --expected-revision 7
{
  "success": true,
  "thread": {"id":"ct_a1…","revision":8},
  "checkpoint": {"id":"ck_78…","supersedes":"ck_77…"}
}
```

The checkpoint is inserted and the thread's pointer is advanced **in one transaction**. `ck_78…`
supersedes `ck_77…`; `ck_77…` is retained. Nothing is overwritten destructively.

### 3.4 Agent B, on its own thread, is unaffected

Agent B writes to `ct_b1…` with `--expected-revision 3`. The CAS is **per thread**, so the two agents
never contend. Agent B's write lands and `ct_b1…` moves to revision 4.

### 3.5 The stale-writer case — both agents on the same thread

Suppose Agent B had been pointed at `ct_a1…` as well (say, a user said "and you handle the API
*in that thread*"). Both read revision 7. Agent A writes first → revision 8. Agent B's write:

```jsonc
// mpm_continuity checkpoint --project cpj_3f9a… --thread ct_a1… --expected-revision 7
{
  "success": false,
  "error": "stale_thread",
  "detail": "thread ct_a1… is at revision 8, not 7 — another writer committed since you read it",
  "current_revision": 8,
  "current_checkpoint_id": "ck_78…",
  "hint": "re-read with `mpm_continuity resume --thread ct_a1…`, merge your change onto ck_78…, and write with --expected-revision 8"
}
```

**Agent B's checkpoint is not written.** No orphan row, no clobbered pointer. Agent B rebases. This
is the "safe checkpoint replacement" guarantee and the "optimistic concurrency protection against
stale agents" guarantee, expressed in one `WHERE` clause.

### 3.6 A crash, with no session-end event

Agent A's process dies right after writing `ck_78…`. There is no handoff — the process never
reached session end, and `EndSessionV2` never ran.

The next agent to pick up AgentShell runs `mpm_continuity resume --project cpj_3f9a… --focus
form-builder` and gets `ck_78…` — the last committed checkpoint. **Nothing was lost**, because the
checkpoint was committed by an explicit write rather than by a session-end event.

Had the crash happened *during* the write, the transaction rolls back and the thread still points at
`ck_77…` — the previous good checkpoint (§5, T1).

### 3.7 The fresh-agent success criterion

> *a fresh agent can find AgentShell, recover the correct thread and continue useful work without a
> transcript or project-management machinery.*

A fresh agent, in a new session, under a different model, in a different directory:

1. `mpm_continuity resolve --name AgentShell` → the project id, its aliases, and its threads.
2. `mpm_continuity resume --project cpj_3f9a… --focus form-builder` → the thread, the checkpoint,
   the resolved references, and the next action.
3. `mpm_resolve --uri mpm://work/w_12…` → the full work item, if it is needed.

Three calls, ~1.2 KB of checkpoint, no transcript, no board, no progress metric, no assignment.

---

## 4. Schema and API proposal

### 4.1 DDL

```sql
-- ── projects ────────────────────────────────────────────────────────────
-- The permanent stub. An identity that outlives every thread, every
-- checkpoint and every session. There is no project state machine: a
-- project is alive (deleted_at IS NULL) or deleted.
CREATE TABLE IF NOT EXISTS projects (
    id          TEXT PRIMARY KEY,                    -- 'cpj_' || 32 lowercase hex
    created_at  INTEGER NOT NULL DEFAULT (CAST(strftime('%s','now') AS INTEGER)),
    updated_at  INTEGER NOT NULL DEFAULT (CAST(strftime('%s','now') AS INTEGER)),
    deleted_at  INTEGER                              -- NULL = alive
);

-- ── project_names ───────────────────────────────────────────────────────
-- The resolution domain. Every key that resolves to a project lives
-- here, and the UNIQUE key is what makes "no silent duplicate project
-- creation" a database guarantee rather than a convention.
CREATE TABLE IF NOT EXISTS project_names (
    name        TEXT PRIMARY KEY,
    project_id  TEXT NOT NULL REFERENCES projects(id) ON DELETE CASCADE,
    kind        TEXT NOT NULL CHECK (kind IN ('canonical','alias')),
    created_at  INTEGER NOT NULL DEFAULT (CAST(strftime('%s','now') AS INTEGER))
);
CREATE INDEX IF NOT EXISTS idx_project_names_project ON project_names(project_id);

-- ── continuity_threads ──────────────────────────────────────────────────
-- An independent line of work inside a project. Several may be active
-- at once; each carries its own revision for optimistic concurrency.
CREATE TABLE IF NOT EXISTS continuity_threads (
    id                  TEXT PRIMARY KEY,            -- 'ct_' || 32 lowercase hex
    project_id          TEXT NOT NULL REFERENCES projects(id) ON DELETE CASCADE,
    title               TEXT NOT NULL,
    focus               TEXT,                        -- normalised slug, unique per project
    state               TEXT NOT NULL DEFAULT 'active'
                        CHECK (state IN ('active','dormant','completed','superseded')),
    revision            INTEGER NOT NULL DEFAULT 0, -- optimistic-concurrency token
    last_checkpoint_id  TEXT,                        -- the current resume brief
    superseded_by       TEXT REFERENCES continuity_threads(id) ON DELETE SET NULL,
    created_at          INTEGER NOT NULL DEFAULT (CAST(strftime('%s','now') AS INTEGER)),
    updated_at          INTEGER NOT NULL DEFAULT (CAST(strftime('%s','now') AS INTEGER)),
    completed_at        INTEGER,
    deleted_at          INTEGER
);
CREATE INDEX IF NOT EXISTS idx_continuity_threads_project
    ON continuity_threads(project_id, state, updated_at DESC);
-- One live thread per focus. A superseded or deleted thread releases
-- its focus, so the same focus can be re-opened later.
CREATE UNIQUE INDEX IF NOT EXISTS idx_continuity_threads_focus
    ON continuity_threads(project_id, focus)
    WHERE focus IS NOT NULL AND deleted_at IS NULL AND state != 'superseded';

-- ── continuity_checkpoints ──────────────────────────────────────────────
-- The append-only ledger of resume briefs. A checkpoint is never
-- edited; it is superseded by a later one, and the thread's
-- last_checkpoint_id names the current one.
CREATE TABLE IF NOT EXISTS continuity_checkpoints (
    id                   TEXT PRIMARY KEY,           -- 'ck_' || 32 lowercase hex
    thread_id            TEXT NOT NULL REFERENCES continuity_threads(id) ON DELETE CASCADE,
    project_id           TEXT NOT NULL REFERENCES projects(id) ON DELETE CASCADE,
    revision             INTEGER NOT NULL,          -- the thread revision this was written at
    headline             TEXT,                       -- <= 200 bytes, one orientation line
    facts                JSON NOT NULL DEFAULT '[]',
    hypotheses           JSON NOT NULL DEFAULT '[]',
    state                JSON NOT NULL DEFAULT '[]',
    next_actions         JSON NOT NULL DEFAULT '[]',
    supersedes           TEXT REFERENCES continuity_checkpoints(id) ON DELETE SET NULL,
    created_at           INTEGER NOT NULL DEFAULT (CAST(strftime('%s','now') AS INTEGER)),
    -- The writer's identity, on the row rather than in artifact_provenance
    -- (see §4.5 for why).
    mpm_session_id       TEXT,
    framework_session_id TEXT,
    actor_kind           TEXT,
    framework_name       TEXT,
    model_name           TEXT
);
CREATE INDEX IF NOT EXISTS idx_continuity_checkpoints_thread
    ON continuity_checkpoints(thread_id, created_at DESC);
```

### 4.2 The tool contract — `mpm_continuity`

One registry entry, `action` + `params`, with a `oneOf` branch per action — the shape `mpm_work`
already uses for twelve actions (`internal/core/tools/registry_list.go:791-983`).

| Action | Kind | Params | Result |
|---|---|---|---|
| `resolve` | read | `name` (optional — omit to list all live projects) | the project, its aliases, its threads |
| `create` | write | `name` | the new project, or a refusal naming the existing one |
| `rename` | write | `project`, `name` | the project with its new canonical name and the demoted alias |
| `alias` | write | `project`, `add` \| `remove` | the project's names |
| `delete` | write | `project`, `confirm` | the tombstone |
| `thread` | read+write | `project`, `op` ∈ `open` \| `list` \| `close` \| `supersede` \| `focus`, plus `title`, `focus`, `thread`, `state`, `expected_revision` | the thread(s) |
| `checkpoint` | write | `project`, `thread`, `expected_revision`, `headline`, `facts`, `hypotheses`, `state`, `next_actions` | the new checkpoint and the thread's new revision |
| `resume` | read | `project`, and one of `thread` \| `focus` \| `title` | the thread, the materialised checkpoint, the next action |

**`resolve` never creates.** A caller that wants a project created calls `create`. That separation
is the "no silent duplicate" guarantee at the API level; the UNIQUE index is the guarantee at the
storage level. Both are needed: the index catches a race, the API shape prevents the intent.

**`checkpoint` is the only action that writes a brief, and it is always explicit.** No automatic
checkpointing, no fold, no implicit write on session end. The brief's "recovery after crashes
without requiring a session-end event" is satisfied by the checkpoint being writable *at any time*,
not by a background writer (which would add ambient overhead and could write a brief nobody
sanctioned).

### 4.3 The CLI facade

`mpm continuity <sub>` — the same shape as `mpm work item` (`cmd/mpm/handlers_work.go:78-84`): a
thin, discoverable facade over `mpm call mpm_continuity --payload '{…}'`.

```
mpm continuity resolve [name]
mpm continuity create <name>
mpm continuity rename <project> <new-name>
mpm continuity alias <project> --add <key> | --remove <key>
mpm continuity delete <project> --confirm <project>
mpm continuity thread <project> --list | --focus <slug> | --title <t> | --close | --supersede <thread>
mpm continuity checkpoint <project> --thread <id> --expected-revision <n> [--headline …]
mpm continuity resume <project> [--thread <id> | --focus <slug> | --title <t>]
```

### 4.4 Pointers

`mpm://project/<id>` and `mpm://thread/<id>` join the existing five kinds. The resolver's kind
check (`internal/core/tools/handlers.go:7562`) and its tool description
(`internal/core/tools/registry_list.go:759-762`) both need extending, and `parsePointerURI`
(`:7827`) already validates the id shape — the `cpj_`/`ct_` prefixes fit it unchanged.

### 4.5 Provenance — and why the artifact vocabulary is not extended

`artifact_provenance.artifact_type` carries a **CHECK** constraint generated from the canonical
`ProvenanceArtifactTypes` slice (`internal/core/schema.go:23-25`, `:30-36`). Widening it to admit
`continuity_checkpoint` requires **rebuilding the table** — the precedent is
`migrateArtifactProvenanceWorkType`, which does exactly that dance for `'work'`.

**Decision: do not widen it in v1.** A table rebuild on a shared, append-heavy table is a
migration with real risk, and the benefit is telemetry the checkpoint row already carries. Instead,
the checkpoint row records `mpm_session_id`, `framework_session_id`, `actor_kind`, `framework_name`
and `model_name` directly. The checkpoint table is new, so those columns are free.

**Consequence, stated plainly:** a checkpoint is *not* a first-class provenance artifact. It will
not appear in `artifact_provenance`-driven lineage queries, and `evidence.artifact_type` is not
widened either — a checkpoint cannot carry its own evidence rows. Its "evidence-backed" quality
comes from the artifacts it *references*, each of which carries its own evidence. Making checkpoints
first-class provenance artifacts is an operator decision (§9, D6).

### 4.6 What is deliberately *not* extended

- **`active.json` is not extended.** The obvious place to record "the session's current project" is
  `ActiveState` — and it would make the wake context able to carry a checkpoint for free. Two
  reasons not to: (i) `saveActiveJSONAt` writes with `os.WriteFile`, **not** the tmp+rename atomic
  swap that AGENTS.md §3 requires of cross-process files, so adding a correctness-relevant pointer
  to a file that can be truncated by a crash is a bad trade; and (ii) it adds ambient state to every
  session for a capability that the product contract reaches by explicit name.
- **The wake context gains no new top-level field in v1.** The continuity brief is fetched by an
  explicit call, which costs zero tokens on every wake. The routing-pipeline integration is Stage 4
  and is conditional on the operator deciding where a session→project binding should live (§9, D3).
- **`evidence.artifact_type` is not widened** (§4.5).
- **No `works` column.** A work item is not made project-scoped. The relationship runs the other
  way: a checkpoint's `state` section *references* work items by id.

---

## 5. Crash and concurrency threat model

| # | Threat | Mechanism | Outcome |
|---|---|---|---|
| **T1** | Crash between the checkpoint INSERT and the thread pointer UPDATE | both in one `WithTx`; the callback's error or the process's death rolls the transaction back | the thread still points at the previous checkpoint; no orphan |
| **T2** | `WithTx` restarts the transaction on a stale snapshot | the checkpoint id is generated **once, before** the callback, and the insert is `ON CONFLICT(id) DO NOTHING` — the substrate's `evidence` pattern (`internal/core/evidence_store.go:92-105`) | the restart re-inserts the *same* id; no duplicate checkpoint |
| **T3** | Two agents write checkpoints to one thread concurrently | `WHERE id = ? AND revision = ?`; exactly one gets `RowsAffected == 1` | the loser is told `stale_thread` with the current revision; nothing is written |
| **T4** | Two agents create a project with the same name concurrently | `project_names.name` is the PRIMARY KEY | exactly one INSERT lands; the other gets a UNIQUE violation, which the tool turns into "a project named X already exists: cpj_…" |
| **T5** | Two agents open a thread for the same focus concurrently | the partial UNIQUE index on `(project_id, focus)` | one thread; the other gets "a thread with focus X already exists (ct_…)" |
| **T6** | A rename concurrent with a resolve | the demote + promote are one transaction | a resolver sees the old key as an alias or the new key — never a gap |
| **T7** | A thread is superseded while an agent holds it | the next write's CAS includes `revision` and the state transition bumped it | the agent's write is refused with `stale_thread`; the response names the superseding thread |
| **T8** | A project is deleted while an agent holds a thread | the delete bumps the thread revisions (or the delete cascades); the CAS fails | the write is refused; the agent is told the project is deleted |
| **T9** | A checkpoint references an artifact that is later shredded | `resume` resolves each reference independently and marks an unresolvable one `missing` | the checkpoint stays readable; the missing reference is reported, not fatal |
| **T10** | A checkpoint's payload exceeds the budget | the write path validates the section counts and byte caps and **rejects** the write | an oversized brief is never committed; the error names the section and the limit |
| **T11** | An agent writes a checkpoint with a stale `expected_revision` after its own earlier write | the CAS | refused — the agent must re-read. This is deliberate: silently accepting a write whose basis has moved is how a checkpoint loses meaning |
| **T12** | Two workspaces hold projects with the same name | projects are workspace-scoped (the database is the scope) | they are independent projects; there is no cross-workspace resolution. See §9, D1 |

**What is not claimed.** A checkpoint records what an agent asserted. It is not a proof that any
external thing happened, exactly as a settlement is not a proof that an external effect happened
(the delivery-state record's C8). The checkpoint's integrity rests on the artifacts it references —
which carry their own evidence and their own confidence — not on the checkpoint's own assertion.

---

## 6. Backward-compatibility and migration analysis

### 6.1 What changes for an existing database

| Change | Kind | Risk |
|---|---|---|
| three new tables | additive | none — `CREATE TABLE IF NOT EXISTS` on a database that has never seen them |
| `project_names.name` PRIMARY KEY | additive | none |
| two new pointer kinds in `mpm_resolve` | additive | none — two more branches in an existing switch |
| `mpm_continuity` in the registry | additive | none — one slice entry; `mpm call`, the MCP `tools/list` and `mpm_help` pick it up automatically |
| `mpm continuity` in the CLI router | additive | none |
| `CanonicalMPMSchema` + `RuntimeCanonicalSchema` | **required** | the restore allow-list must list the new tables, or `mpm restore-db` will reject a dump that contains them |
| the `contextual_candidates` kind vocabulary | additive, **Stage 4** | the vocabulary is closed; adding `continuity` extends it. Deferred so v1 changes no routing behaviour |

**Nothing existing is read differently.** No existing writer changes. No existing reader changes.
No existing predicate changes. **This is the same additive, behaviour-neutral shape that the
delivery-state Stage A was specified to have** (`docs/dev/issue-178-delivery-state-design-2026-11-04.md`),
and the same discipline applies: the migration is strictly additive, idempotent, forward-only, and
lands lazily.

### 6.2 The migration mechanics

Following `internal/core/migration_scheduled_wakes_dispatched_at.go` exactly:

- a `schema_migrations` sentinel, `continuity_project_model_v1`;
- `CREATE TABLE IF NOT EXISTS` for all three tables plus the indexes, so the migration is idempotent;
- called from the **fatal** migration transaction, beside `MigrateWakeResolverFiredBy` and
  `MigrateScheduledWakesDispatchedAt` (`internal/core/db.go:3117` and `:3135`), so a failure rolls the
  whole migration back and `NewDatabaseManager` returns an error;
- **not** in `SafeMigrations`, which is best-effort and **silences errors**
  (`internal/core/db.go:2967-2975`).

The base DDL goes in `internal/core/schema.go` as well, because a fresh database and a migrated
database must have **identical** schemas — the precedent is `work_events`, whose DDL appears both in
`BaseTables` and as a runtime migration (`internal/core/db.go:8251-8272`).

**Schema rollback is forbidden.** `ALTER TABLE … DROP COLUMN` is a table-rewrite operation on a live
table with FTS triggers attached, and the delivery-state spike showed SQLite *refuses* the drop
while a dependent index exists. Continuity's tables are forward-only.

**The updater never opens the database.** `cmd/mpm-update/` contains no `NewDatabaseManager` and no
`sql.Open`, so the migration lands lazily, when the first new-binary process opens the database. The
activation note for the operator: run one `mpm` invocation before starting anything that depends on
the new tables.

### 6.3 The restore allow-list is not optional

`internal/core/sql_dump_validator.go:141-240` holds two lists: `CanonicalMPMSchema` (tables declared
in the base DDL) and `RuntimeCanonicalSchema` (tables created by runtime migrations). Both are
**security allow-lists** for `mpm restore-db`: a dump declaring a table MPM does not create is
rejected. `RuntimeCanonicalSchema` is additionally checked against source by
`TestRuntimeCanonicalSchemaHasNoPhantomEntries`.

**Both lists must gain the three new tables**, or `mpm restore-db` will refuse a dump of a database
that Continuity created. This is the single most easily-missed compatibility item in the whole
design, and it belongs in the Stage 1 acceptance matrix.

### 6.4 The complete change surface

| Surface | Change | Stage |
|---|---|---|
| `internal/core/schema.go` | the three tables + indexes | 1 |
| `internal/core/migration_continuity_project_model.go` (new) | the sentinel migration | 1 |
| `internal/core/db.go` | one call in the fatal migration transaction | 1 |
| `internal/core/sql_dump_validator.go` | two allow-list entries | 1 |
| `internal/core/continuity.go` (new) | the model: resolve, create, rename, alias, delete, thread, checkpoint, resume | 2 |
| `internal/core/tools/handlers.go` | `handleMpmContinuity` | 2 |
| `internal/core/tools/registry_list.go` | one registry entry | 2 |
| `internal/core/tools/registry.go` / `internal/core/core.go` | `CoreDB` gains the continuity methods | 2 |
| `cmd/mpm/router.go` + `cmd/mpm/handlers_continuity.go` (new) | the CLI facade | 3 |
| `internal/core/tools/handlers.go` (`mpm_resolve`) | two pointer kinds | 3 |
| `internal/core/contextual_candidates.go` + `…_sources.go` | the `continuity` candidate kind + source | 4 |
| `internal/core/wake_context.go` | *nothing* in v1 | — |

---

## 7. Staged implementation sequence

Each stage is independently mergeable, has its own acceptance gate, and names its stop conditions.

### Stage 1 — the additive schema

| | |
|---|---|
| **Prerequisites** | none |
| **Files** | `internal/core/schema.go`; `internal/core/migration_continuity_project_model.go` (new); `internal/core/db.go`; `internal/core/sql_dump_validator.go` |
| **DB invariants** | the three tables and their indexes exist; the sentinel is recorded exactly once; **no existing table or row is modified** |
| **Observable behaviour** | **none** |
| **Regression tests** | AT1–AT6 (§8) |
| **Compatibility** | old binaries ignore the new tables; the schema is forward-only |
| **Rollback** | binary rollback is safe; schema rollback is forbidden |
| **Stop condition** | stop if any existing row changes, if the restore allow-list is not updated, or if `git diff --stat` touches a file outside the four listed |
| **Gate** | AT1–AT6 green; `mpm-lint --gate` PASS; `make docs-check` OK |

### Stage 2 — the model and the tool

| | |
|---|---|
| **Prerequisites** | Stage 1 |
| **Files** | `internal/core/continuity.go` (new); `internal/core/tools/handlers.go`; `internal/core/tools/registry_list.go`; `internal/core/core.go` (`CoreDB`) |
| **DB invariants** | a project resolves through exactly one name row; the checkpoint ledger is append-only; every thread mutation bumps `revision` |
| **Observable behaviour** | `mpm call mpm_continuity` becomes available with eight actions |
| **Regression tests** | AT7–AT12 |
| **Compatibility** | the tool is new; nothing existing changes behaviour |
| **Rollback** | fully revertible — no existing surface is touched |
| **Stop condition** | stop if any action can create a duplicate project, or if a write can land without a revision check |
| **Gate** | AT7–AT12 green; the existing tool-registry and namespace-contract tests green |

### Stage 3 — the CLI facade and the pointers

| | |
|---|---|
| **Prerequisites** | Stage 2 |
| **Files** | `cmd/mpm/router.go`; `cmd/mpm/handlers_continuity.go` (new); `internal/core/tools/handlers.go` (`mpm_resolve`) |
| **Observable behaviour** | `mpm continuity <sub>` works; `mpm_resolve` accepts two new URI kinds |
| **Regression tests** | the facade's exit codes; the two new pointer kinds; `mpm_resolve`'s existing five kinds are unchanged |
| **Rollback** | fully revertible |
| **Stop condition** | stop if `mpm_resolve`'s existing kinds change behaviour |
| **Gate** | the pointer and facade tests green |

### Stage 4 — routing-pipeline integration (**conditional**)

| | |
|---|---|
| **Prerequisites** | Stage 2; **and** the operator's decision on where a session→project binding lives (§9, D3) |
| **Files** | `internal/core/contextual_candidates.go`; `internal/core/contextual_candidates_sources.go`; `internal/core/contextual_selection.go` |
| **DB invariants** | none — the pipeline is observational |
| **Observable behaviour** | a checkpoint becomes a routing candidate with kind `continuity` and a rationale in the existing `continuity` band |
| **Regression tests** | the candidate's source counts and reason tokens; the selection's band and rationale; **no change** to the existing candidates' ordering or counts |
| **Rollback** | fully revertible |
| **Stop condition** | stop if the routing output changes for a session with **no** project — the whole point is that the capability is opt-in and ambient-free |
| **Gate** | the existing contextual-routing suites green **unchanged** |

### Stage 5 — a project purge (deferred)

Deliberately out of scope. A hard purge of a project's threads and checkpoints needs a reason-code
and a backup flow like `work_purge`. It is not in this series.

---

## 8. Acceptance-test matrix

Every test is deterministic. **No test may contain a sleep or an arbitrary wait.** Two mechanisms,
both already in the tree: the **SQLite authorizer barrier**
(`internal/core/wake_resolve_contention_test.go`, which pauses a statement at `PREPARE` time and
therefore observes the arrival explicitly before doing anything else) and **subprocess re-exec with
a named crash point** (the standard Go pattern: the test re-executes itself with an environment
variable naming the crash point, signals the parent over a pipe, and the parent `SIGKILL`s it).

| # | Scenario | Mechanism | Assertion |
|---|---|---|---|
| **AT1** | **Existing database migration** | a disposable file-backed WAL DB on the pre-Continuity schema, populated with representative rows, then opened by the new binary | the three tables and their indexes exist; the sentinel count is 1 |
| **AT2** | **Fresh database creation** | a new DB through the same path | the schema is **identical** to AT1's; the sentinel count is 1; the base DDL and the migration do not conflict |
| **AT3** | **Whole-row preservation** | snapshot every pre-existing row of every pre-existing table before and after, as whole-row strings | **byte-identical** — the whole row set, not a sample |
| **AT4** | **Zero fabricated state** | — | no existing table gains a row; no existing row gains a column value; `project_names` starts empty |
| **AT5** | **Idempotent migration** | run the migration twice, then a third time | the table list, the index list and the sentinel count are unchanged by runs 2 and 3 |
| **AT6** | **Failed-migration rollback** | a trigger that aborts the third `CREATE TABLE` | the migration returns an error; the table list is unchanged; no sentinel; `NewDatabaseManager` surfaces the error |
| **AT7** | **Restore allow-list** | a dump containing the three new tables | `mpm restore-db` **accepts** it — and `TestRuntimeCanonicalSchemaHasNoPhantomEntries` is green |
| **AT8** | **Project resolution** | a project with a canonical name and two aliases, plus a second project differing only by case | exact → resolved; alias → resolved; case-insensitive unique → resolved **with a note**; case-insensitive ambiguous → **refused**, both listed; unknown → **not created**, the live names listed |
| **AT9** | **No silent duplicate creation** | (a) create twice with the same name; (b) two concurrent creates of the same name, gated by the authorizer barrier | (a) the second is refused and names the existing project; (b) exactly one project exists and the loser is told its id |
| **AT10** | **Thread selection** | a project with two active threads, one dormant, one completed, one superseded | by id → that thread; by focus → that thread; by title → that thread; unqualified with two active → **ambiguous**, both listed; the completed and superseded threads are never auto-selected |
| **AT11** | **Optimistic concurrency** | two writers against one thread at the same revision | exactly one write lands; the other receives `stale_thread` with the current revision; **no checkpoint row exists for the loser** |
| **AT12** | **Crash at each write boundary** | subprocess re-exec with `MPM_CONTINUITY_CRASH_AT ∈ {after_checkpoint_insert, after_pointer_update, after_commit}` | after insert → the thread still points at the previous checkpoint and **no orphan checkpoint exists**; after the pointer update → the same (the transaction rolled back); after the commit → the new checkpoint is current |
| **AT13** | **Checkpoint budget** | a checkpoint at the limit, then one over it | the limit one is accepted; the over one is **rejected** with the section and the limit named; nothing is committed |
| **AT14** | **Reference resolution and dangling refs** | a checkpoint whose refs point at a memory and at a work item; then shred the memory | both refs resolve with bounded summaries and pointers; after the shred, the memory ref is reported `missing` and the checkpoint is still readable |
| **AT15** | **Rename and alias** | rename a project; then resolve by the old name, the new name, and a stale alias | all three resolve to the same project id; the old canonical name appears as an alias; a rename onto a live project's name is refused |
| **AT16** | **Deletion semantics** | delete a project with `--confirm`; then create a project with the same name | the tombstone exists; the old names no longer resolve; the new project gets a **new** id and the response mentions the tombstone; the old threads and checkpoints are retained and reachable only by id |
| **AT17** | **Crash without a session-end event** | write a checkpoint; kill the process; then resolve and resume in a fresh manager | the checkpoint is current and complete; **no handoff exists**; the resume returns the same bytes as before the crash |
| **AT18** | **Cross-session, cross-agent, cross-model** | write a checkpoint from session 1 with framework F and model M; read it from session 2 with framework G and model N | the checkpoint is byte-identical; the writer's identity is on the row and is **not** part of the brief's content |
| **AT19** | **Existing surfaces unchanged** | the existing suites, run **unchanged** | `internal/core/wake_resolve_contention_test.go`, `internal/core/wake_ownership_test.go`, `internal/scheduler/wake_settlement_test.go`, the tool-registry and namespace-contract tests, and the contextual-routing suites all pass **unchanged** |
| **AT20** | **No ambient overhead** | a session with **no** project | the wake context is **byte-identical** to a pre-Continuity wake context; no new top-level key; no additional query |

**AT1–AT7 are the Stage 1 gate. AT8–AT14 and AT19–AT20 are the Stage 2 gate. AT15–AT18 are the
Stage 3 gate.**

---

## 9. Unresolved operator decisions

| # | Decision | Recommendation | Status |
|---|---|---|---|
| **D1** | Is a project **workspace-scoped** or **cross-workspace**? | **Workspace-scoped.** A project is a user's body of work; the substrate's scoping mechanism is the database. The `shared` schema exists for cross-agent broadcast, not for user identity. | open — but the recommendation is strong, and it is the substrate's own default |
| **D2** | Is the checkpoint written **only explicitly**, or also automatically? | **Only explicitly.** An automatic writer adds ambient overhead and can write a brief nobody sanctioned. The product contract is satisfied by explicit writes. | open |
| **D3** | Where does a **session→project binding** live, if the routing pipeline is to carry a checkpoint automatically? | **Not in `active.json`** (its write is not atomic). Either a DB-backed binding, or Stage 4 is deferred entirely. | open — **blocks Stage 4** |
| **D4** | How long are **checkpoints** retained? | The project survives indefinitely; the checkpoint ledger grows. Recommend keeping the current checkpoint plus the last N superseded ones per thread, and pruning the rest. | open |
| **D5** | Is the **case-insensitive** match tier wanted at all? | **Yes**, with the ambiguity refusal. Without it, `agentshell` fails to resolve an `AgentShell` project — an avoidable frustration. With it, a near-miss can still be caught by the refusal. | open |
| **D6** | Should a **checkpoint be a first-class provenance artifact** — i.e. should `ProvenanceArtifactTypes` gain `continuity_checkpoint`? | **Deferred.** It requires rebuilding `artifact_provenance`, and the checkpoint row already carries the writer's identity. | open |
| **D7** | Should the **`focus` slug** be mandatory on a thread? | **Optional in v1.** A thread without a focus is selectable by id and title only. | open |
| **D8** | Should the **CLI facade** be `mpm continuity` or `mpm project`? | `mpm continuity`, because the tool covers both the project identity and the thread/checkpoint family. | open |
| **D9** | Should `mpm_continuity` appear on the **MCP compact surface**? | **No in v1.** The compact surface is deliberately minimal (`read_wake_context`, `write_handoff`, …). | open |
| **D10** | Should a project be able to hold a **repository binding**? | **Deferred.** It is the natural answer to "resolve independently of the current directory" for a *code* project, but it adds a whole dimension (many-to-many, remote renames, monorepos) and the contract is satisfied by names. | open |

---

## 10. Non-goals

Recorded explicitly, because the failure mode of a design like this is feature creep.

1. **Not project management.** No boards, no assignments, no owners, no priorities, no burndown, no
   "progress", no workflow states, no due dates, no scheduling. A project is an identity; a thread is
   a line of conversation; a checkpoint is a brief.
2. **Not repository discovery.** The current directory is never an input. No CWD probe, no
   parent-directory walk, no git-remote inference. The substrate's rule applies: *"Git evidence
   without an explicitly identified repository is unavailable, not inferred."*
3. **Not a transcript store.** A checkpoint is four bounded sections and one orientation line. It
   holds references, never copied contents, and never a conversation.
4. **Not a replacement for `works`.** A work item is a commitment; a checkpoint may reference one. The
   two vocabularies stay separate.
5. **Not a replacement for `session_handoffs`.** A handoff is the session-end record; a checkpoint is
   the at-will brief. They coexist; neither subsumes the other.
6. **Not a destructive capability.** No hard purge of a project's content in this series (Stage 5 is
   deferred). No automatic deletion of a thread. No cascading delete that a user did not ask for.
7. **Not automatic.** Nothing is written without an explicit call. No fold, no background writer, no
   implicit checkpoint on session end.
8. **Not cross-workspace.** A project lives in one workspace's database (D1).

---

## 11. Recommendation

**READY FOR STAGE 1 IMPLEMENTATION** — the additive schema.

The design satisfies the product contract with **three new tables, one new tool, one CLI facade, two
new pointer kinds, and no change to any existing reader, writer or predicate.**

The load-bearing decisions, and the evidence for each:

| Decision | Evidence |
|---|---|
| A **new data model** is required | §1.3 — the substrate has no project axis, exhaustively verified; and no existing entity has the required durability (§1.2) |
| **Names + aliases in one UNIQUE table** | the UNIQUE key is what makes "no silent duplicate" a database guarantee rather than a convention (§2.5, T4) |
| **Two-tier exact matching, never fuzzy** | the substrate's own rule: *"unavailable, not inferred"* (§1.5) |
| **A `focus` slug per thread** | the contract's *"the form builder"* is a recurring label, not a title; without it, resolution would need substring matching (§2.4) |
| **A `revision` counter per thread** | the inventory found **no** version-based CAS anywhere in the tree; the state-predicate idiom cannot fence a checkpoint pointer (§2.8) |
| **References, not copies, in four sections** | the brief's explicit requirement; the vocabulary is reused from `evidence.artifact_type` (§2.3) |
| **Explicit pull, no ambient state** | *"Minimal context and token overhead"*; and `active.json`'s write is not atomic (§4.6) |
| **`artifact_provenance` not widened** | widening a CHECK requires a table rebuild; the benefit is telemetry the checkpoint row already carries (§4.5) |
| **A project is not a work item** | `works` is purged and has a task lifecycle; the two vocabularies must not be conflated (§2.1) |

**The two open decisions that block the most downstream work** are **D3** (where a session→project
binding lives — it blocks Stage 4) and **D2** (explicit-only checkpoints — it determines whether the
capability ever has ambient cost).

**What this design does not claim.** It does not claim that a checkpoint is *true*. A checkpoint
records what an agent asserted, bounded and pointed at durable artifacts; its integrity rests on
those artifacts, which carry their own evidence and confidence. It does not claim exactly-once
delivery of a brief to a thread — two agents can both write, and the revision makes the loser
visible rather than silent. And it does not make the project axis universal: until D1 is answered
and Stage 4 is built, a project is reachable by name and by explicit call, and by nothing else.

---

## Appendix — the investigation, in one table

| Question | Answer | Where it is evidenced |
|---|---|---|
| Is there a project identity? | **No** — only `lifecycle_events.project_id`, an ingest-ledger attribute nothing joins against | §1.3 |
| Does `works` carry a project? | **No** — and MPM says so itself | `internal/core/db.go:7199-7201` |
| Does `memories` carry a project? | **No** — no column, no `metadata` key | §1.3 |
| Is there a "checkpoint" concept? | **No** — the word appears only for WAL checkpointing and the scratchpad | §1.2 |
| Is there a "continuity thread" concept? | **No table or column** — `BandContinuity` is a *selection band* over session identity | §1.1 |
| Is there an alias concept? | **No** | §2.6 |
| What is the continuity unit today? | **The session** — `mpm_session_id` + `session_handoffs` + `work_events` | §1.1, §1.2 |
| What is the durable-commitment mechanism? | `works` + `work_events`, event-sourced, `UNIQUE(work_id, event_index)` | §1.2 |
| What is the polymorphic reference idiom? | `(artifact_id, artifact_type)` with `memory\|theory\|decision\|lesson\|work` | `internal/core/schema.go:355` |
| What is the provenance vocabulary? | `ProvenanceArtifactTypes` — 7 values, CHECK-enforced, widening needs a table rebuild | `internal/core/schema.go:23-25` |
| What is the CAS idiom? | state-column predicates; **no** version counter anywhere | §2.8 |
| What is the transaction layer? | `WithTx` — restarts on a stale snapshot, rolls back on error; `execTracked` — budget 5 | `internal/core/db.go:1163`, `:1324` |
| What is the restart-safe insert idiom? | an identity-derived id plus `ON CONFLICT(id) DO NOTHING` | `internal/core/evidence_store.go:92-105` |
| What are the projection budgets? | 6000-byte detail budget, 800-byte per-item cap, 20-item hard max | `internal/core/contextual_materialization.go:102-107` |
| Where does the tool surface live? | one `Registry` slice; `mpm_<noun>`; `action` + `params`; `oneOf` per action | `internal/core/tools/registry_list.go:5-22` |
| What must a new table be added to? | `CanonicalMPMSchema` and `RuntimeCanonicalSchema` — the restore security allow-list | `internal/core/sql_dump_validator.go:141-240` |
| Is the current directory ever a resolution input? | **No** — and the substrate has a documented rule against it | `internal/core/db.go:7181-7234` |
