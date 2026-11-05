# Project-Scoped Continuity — architecture

**Date:** 2026-11-04 · **Revised:** 2026-11-05 (independent-review remediation)
**Baseline:** `main` @ `f8d4b95aefe60a0880409a8905ce2c4d92d22c00` (merge of PR #218)
**Status:** architecture and acceptance specification. **No production schema migration, no
implementation, no merge.**

**Revision note (2026-11-05, third pass).** This revision remediates the **final independent delta
review** of this document ([comment 6656558202](https://github.com/flowbyte-com/mpm/pull/224#issuecomment-6656558202),
verdict *APPROVED* — *"The Stage 1 architecture is technically ready"*), which verified all eight
of its priorities against the tree and the SQL and raised **two** non-blocking documentation
findings (F1–F2): §2.3's claim that a `memory` reference admits a lesson id, and AT2's rationale for
comparing by name. Both are corrected here (§12.4). The revision immediately before it remediated the
**second** independent correctness review ([comment 6655135875](https://github.com/flowbyte-com/mpm/pull/224#issuecomment-6655135875),
verdict *ACCEPT WITH REQUIRED CORRECTIONS*), which verified the first review's sixteen corrections
(A1–A16) against the tree and the SQL and raised **six** further defects (D1–D6).

The **architecture is unchanged**: permanent project identities, a name/alias resolution domain,
independent threads, bounded reference-backed checkpoints, and per-thread optimistic concurrency are
all preserved. What changed is the *specification*:

- **D1 — one required DDL correction.** The checkpoint's `supersedes` pointer was a single-column
  foreign key, so a cross-thread *and* a cross-project supersession were both representable and
  T13's stated containment did not cover it. It is now a **composite** foreign key on
  `(supersedes, thread_id)`, closing both cases, using the unique index the `last_checkpoint_id`
  foreign key already needed (§4.1).
- **D2–D6 — five documentation corrections.** The foreign-key counts; the §3 example lifecycle's
  invalid pointer ids; four stale citations and the audit's root cause; AT8's coverage; and OD3's
  conditionality.
- **D7–D12 — the review's optional refinements are taken too**, because each removes a real
  ambiguity and none changes the architecture (§12.3).
- **The evidential apparatus was strengthened.** The §4.1 DDL is executed against a disposable
  SQLite database (**84 assertions, 0 failures**), the substrate's **real** inline pointer parser
  is run over **every** `mpm://` URI this document contains, and the citation audit now runs
  **against the declared base** with a declaration-anchored criterion (§14).

- **F1–F2 — the final review's two documentation findings.** A `memory` reference admits a
  **decision** or a **theory** id, not a lesson id: a lesson is a `lessons_base` row, and
  `GetMemory` reads `memories`. And AT2 still compares **by name**, but for the honest reason —
  row order is not part of the schema, so an order-sensitive comparison is fragile even though it
  happens to pass today (§14.2 E28).

The first review's corrections (A1–A16) are preserved unchanged; §12.1 is their disposition table,
§12.2 is D1–D6's, §12.3 is D7–D12's, §12.4 is F1–F2's, and §13 is P1–P6.

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
| `session_handoffs` | **one session** | 90-day TTL (`PruneHandoffs`, `internal/core/gc_tools.go:117`) | `clean` / `crashed` / `interrupted` / `force_end` (`internal/core/schema.go:1322-1323`) | No — written at session **end** |
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
  (`internal/core/schema.go:883` for the table, `:914` for the column). It is a **host-supplied field on the mpm-event/0.1 adapter ingest
  ledger**: a closed five-value source vocabulary (`explicit-env`, `explicit-file`, `git-remote`,
  `host-git-remote`, `unresolved`), two accepted id forms (`pj_` + 32 hex derived, or an explicit
  1–128-char id), and the rule *"project_id: never fabricated"* (`internal/core/lifecycle_events.go:500`).
  **Nothing joins it to `works`, `memories` or `session_handoffs`.** It is an observation attribute,
  not a substrate identity.
- **There is no repository scoping anywhere.** No table, column, index or query filters by
  repository, repo path, git remote or codebase.

And the substrate has already ruled on the obvious shortcut. `internal/core/db.go:7181-7234`
documents the removal of automatic git evidence and encodes the rule:

> *Git evidence without an explicitly identified repository is unavailable, not inferred.*

Continuity inherits that rule. **The current directory is never a resolution input** (§2.5).

### 1.4 The integration seams that exist

| Seam | Shape | Use in this design |
|---|---|---|
| **The `mpm://` pointer convention** | `mpm://memory/<id>`, `mpm://work/<id>`, `mpm://lesson/<id>`, `mpm://theory/<id>`, `mpm://blob/<id>`; resolved by `mpm_resolve` (`internal/core/tools/handlers.go:7562`) | add `mpm://project/<id>`, `mpm://thread/<id>` and `mpm://decision/<id>` (§4.4) |
| **The pointer id charset** | `^[a-z0-9-]+$` — lowercase alphanumerics **and hyphens only**, no underscore (`internal/pointer/pointer.go:17`; the inline copy in `internal/core/tools/handlers.go:7857-7862`) | the Continuity id prefixes are `cpj-`, `ct-`, `ck-` (§4.1) — they fit unchanged |
| **The polymorphic reference idiom** | `(artifact_id, artifact_type)` with the closed vocabulary `memory\|theory\|decision\|lesson\|work` — `evidence.artifact_type` (`internal/core/schema.go:355`) | reuse it verbatim for checkpoint references (§2.3) |
| **The provenance vocabulary** | `ProvenanceArtifactTypes` = `memory, theory, lesson, decision, handoff, directive, work` (`internal/core/schema.go:23-26`), enforced by a CHECK on `artifact_provenance` | **not** extended in v1 (§4.5) |
| **`active.json`** | `ActiveState` — persona, modes, `mpm_session_id` | **not** extended in v1 (§4.6) |
| **The tool registry** | one slice, `mpm_<noun>` names, `action` + `params`, `oneOf` per action; both `mpm call` and the MCP server iterate it (`internal/core/tools/registry_list.go:5-23`) | one new entry, `mpm_continuity` |
| **The CLI facade pattern** | `mpm work item <sub>` is "a thin discoverable facade over `mpm call mpm_work`" (`cmd/mpm/handlers_work.go:78-84`) | `mpm continuity <sub>` |
| **The transaction layer** | `WithTx` (restart on a stale snapshot, rollback on error), `execTracked` (budget 5) | every write |
| **The CAS idiom** | `WHERE id = ? AND <state> = ?` — wake resolution, lesson promotion, handoff read marker | the thread's `revision` column (§2.8) |
| **The identity-derived id** | `evidence` derives its id from the observation identity and inserts `ON CONFLICT(id) DO NOTHING` (`internal/core/evidence_store.go:92-105`) | the checkpoint's restart-safe insert (§5, T2) |
| **The bounded-projection budgets** | `contextual_materialization`: 6000-byte detail budget, 800-byte per-item cap | the checkpoint's budgets (§2.3) |
| **The restore allow-list** | `CanonicalMPMSchema` / `RuntimeCanonicalSchema` (`internal/core/sql_dump_validator.go:141`, `:231`) | the **four** new tables go in `CanonicalMPMSchema` only (§6.3) |

### 1.5 The substrate's own refusals, which this design inherits

- **No ambient probing.** The ghost-database incident (`internal/core/config/config.go:633-656`)
  is why `GetWorkspace` no longer probes the CWD. The same discipline applies to project matching.
- **No inferred repository.** `internal/core/db.go:7181-7234`.
- **No fabricated project id.** `internal/core/lifecycle_events.go:500`.
- **Reads never allocate.** `internal/core/session_identity.go:27-41`.
- **No `SetMaxOpenConns(1)`.** `internal/core/db.go:1663-1679` — many connections, writes serialised by SQLite's
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
mechanism.** The extensions are enumerated in §6.4: three pointer kinds, one new tool, one CLI
facade, one new candidate kind (deferred to Stage 4), and four entries in the restore allow-list.

### 2.2 The four tables

```
projects              — the permanent stub: an identity that outlives every thread
project_names         — the resolution domain: canonical name + aliases, one row per key
continuity_threads    — an independent line of work inside a project
continuity_checkpoints— the bounded, structured, append-only resume brief
```

**Four tables, three entities.** `project_names` is not an entity; it is the *resolution domain* for
one entity, the project. The count is **four**, and it is load-bearing: `CanonicalMPMSchema` is the
restore allow-list (§6.3), so a table missing from it makes `mpm restore-db` reject a dump of a
database Continuity touched. The four names are enumerated by name in AT1 and AT8 — and AT8 now names them
too, so the claim is checkable against the matrix rather than against the prose.

Four tables is the minimum: a project without names cannot be resolved; a thread without a project
has no owner; a checkpoint without a thread has no home. A fifth table for checkpoint references
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

**The reference vocabulary and the pointer vocabulary must coincide.** Each reference kind has
exactly one pointer kind, and the read path materialises it through that kind's read path:

| Reference kind | Where the artifact lives | Pointer kind | Resolver behaviour |
|---|---|---|---|
| `memory` | `memories`, `collection='memories'` | `mpm://memory/<id>` | `GetMemory` |
| `theory` | `memories`, `collection='theories'` | `mpm://theory/<id>` | `GetMemory` **+ `collection == 'theories'`** |
| `decision` | `memories`, `collection='decisions'` | `mpm://decision/<id>` | `GetMemory` **+ `collection == 'decisions'`** |
| `lesson` | the `lessons` VIEW over `lessons_base` | `mpm://lesson/<id>` | `GetLesson` |
| `work` | `works` | `mpm://work/<id>` | the work read path |

The `decision` row is the reason the mapping is stated rather than assumed: **a decision is a
`memories` row with `collection='decisions'`** (`internal/core/citation.go:121`,
`internal/core/epistemology_tools.go:501`). `GetMemory` selects by id and does **not** filter by
collection (`internal/core/web_db.go:258`), so `mpm://memory/<decision-id>` happens to resolve the
decision today — but untyped. The substrate's own precedent is `mpm://theory/`
(`internal/core/tools/handlers.go:7660-7668`), which resolves a `memories` row through
`GetMemory` **and asserts its collection**. `mpm://decision/` is that same shape, and it is what
makes the reference vocabulary and the pointer vocabulary the same set (§4.4, §9 OD5).

**The stored reference shape is exactly three fields.** A reference is stored as:

```jsonc
{"kind": "memory | theory | decision | lesson | work", "id": "<artifact id>", "note": "<optional, ≤256 bytes>"}
```

**The `pointer` is derived, never stored.** The read path computes `mpm://<kind>/<id>` from the
two stored fields. A stored pointer could disagree with its reference kind — a `decision` reference
carrying `mpm://memory/<id>` would resolve and present a decision as a memory, silently — and a
derived pointer cannot. This is why §3.2's response shows a `pointer` on every reference while the
checkpoint *row* holds only `kind`, `id` and `note`.

**The read path asserts the kind against the artifact.** Because the pointer is derived, the only
way a reference can be mis-resolved is a `kind` that lies about the artifact it names. So `resume`
asserts the collection before materialising: a reference of kind `decision` whose id is a
`memories` row with `collection='memories'` is reported as **`kind_mismatch`**, not resolved as a
decision. The same assertion exists for `theory`. A `memory` reference names the general
`memories` collection and therefore admits a **decision** or a **theory** id — that direction is
inherited from the substrate's own untyped `mpm://memory/` and is **bounded and reported**: the
resolved artifact's own collection is returned alongside it, so a caller sees that it received a
decision when it asked for a memory. **A lesson is not in that set.** A lesson is a
`lessons_base` row, not a `memories` row: `GetLesson` reads `FROM lessons_base`
(`internal/core/db.go:5966-5972`) and `GetMemory` reads `FROM memories`
(`internal/core/web_db.go:258`), so `mpm://memory/` does **not** resolve a lesson id, and a lesson
reference that named one would fail to resolve rather than resolve untyped. A lesson has its own
kind — `mpm://lesson/<id>` — and its own read path, `GetLesson`. The typed kinds are the precise
ones, and they are the ones the `facts` section should prefer for decisions.

**References, not duplicated contents.** A checkpoint never copies a memory's text or a work item's
content. It points at ids; the read path materialises them through the existing per-kind read paths
and returns bounded summaries plus the derived `mpm://` pointers. A work item's `status`,
`verification` and `summary` in the `state` section are read **live from the work row at resume
time**, not copied into the checkpoint — so a work item that has since been reopened is reported
with its current state, not the state as it was when the brief was written. A `note` on a reference
is permitted but is a **bounded annotation** ("why this is here"), not a copy — it cannot exceed 256
bytes and cannot restate the artifact's content.

**One orientation line.** A pure list of ids is not resumable, so a checkpoint carries a `headline`
of at most 200 bytes: one line saying where things stand. That is the only free-form prose in the
structure, and it is the part that cannot be derived from the references. It is the writer's
assertion, and `resume` labels it as such.

**Budgets**, grounded in the substrate's existing numbers:

| Budget | Value | Anchor |
|---|---|---|
| checkpoint payload (all four sections + headline) | **4096 bytes** | below `contextual_materialization`'s 6000-byte detail budget (`internal/core/contextual_materialization.go:102-107`) |
| per-reference note | **256 bytes** | below the 800-byte per-item cap |
| per-`next_action` text | **256 bytes** | as above |
| `facts` / `hypotheses` / `state` / `next_actions` counts | 12 / 6 / 12 / 8 | — |
| thread `title` | **200 bytes** | new in this revision (§4.1) |
| `resume` response (materialised) | **6000 bytes** | matches the materialization budget exactly |

**The byte cap is the primary gate, not the counts.** Twelve `facts` at 256-byte notes is 3072
bytes of notes inside a 4096-byte payload, so a checkpoint can exceed the count limits while still
fitting the byte budget, and a checkpoint can exceed the byte budget while still fitting the
counts. **The write path rejects on whichever limit is exceeded, and the rejection names the
section and the limit** (AT15). The counts are a secondary guard.

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
| 2 | `focus` | exact match on the thread's `focus` slug; then a **case-insensitive** match | **ambiguous** → list the candidates |
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

**`focus` is unique per project among live, non-superseded threads, enforced by a partial unique
index** — and the enforcement is **case-insensitive**, because the column itself is declared
`COLLATE NOCASE` (§4.1). Two agents that race to open a thread for the same focus produce one
thread and one clear error naming the winner — not two near-identical threads, and not two threads
differing only by case.

**The ambiguity branch of tier 2 is unreachable among live threads.** The partial unique index admits
at most one live, non-superseded thread per `(project_id, focus)` under NOCASE collation, so two live
threads cannot differ only by case. The branch is retained in the implementation as a defensive
assertion and AT11 asserts that it is unreachable — an unreachable branch is a branch that is never
tested, so it is tested.

**Why `focus` is optional.** A thread without a focus is selectable by id and title only (§9, R7).
The partial unique index excludes `focus IS NULL`, so any number of focus-less threads may coexist.

### 2.5 Project matching — explicit and conservative

The brief requires the agent to *"Recognise AgentShell as an existing project"* and to *"resolve its
durable identity independently of the current directory"*, with *"no silent duplicate project
creation"*.

**Resolution** (`mpm_continuity resolve --name <name>`) is an **exact-or-case-insensitive match**
over the live rows of the `project_names` table:

1. **Exact** match on the stored key → that project.
2. **Case-insensitive** match → that project, with the response noting the case normalisation.
3. **Ambiguous** → **unreachable, and the storage proves it.** `project_names.name` is declared
   `COLLATE NOCASE` and a **partial unique index** admits at most one **live** holder of a name
   (§4.1). Two live projects therefore cannot differ only by case, so the case-insensitive match
   resolves to exactly one project or to none. The branch is retained as a defensive assertion;
   AT9 asserts that it is unreachable.
4. **No match** → **do not create**. Return the project's absence and a bounded list of live
   project names, so the agent can choose. Creation is a separate, explicit act.

**The list is bounded, and the bound is stated in the response.** The live-project list is capped at
**25 entries**. When the cap truncates, the response says so — *"showing 25 of 47 live projects —
narrow by name"* — because a truncated list that does not announce its truncation makes the
eleventh project unfindable, which is the failure mode the product requirement warns about.

**No fuzzy matching.** No substring, no token overlap, no embedding similarity, no Levenshtein. A
project named "AgentShell" must never be matched by "agent" or "shell".

**No directory, ever.** The current working directory is not an input to resolution, not a
tie-breaker, and not a fallback. `MPM_WORKSPACE` selects the *database*, which is the substrate's
existing scoping mechanism and is not a project identity. A repository path is **not** an input in
v1 (§10, non-goal 2).

**A name is bound to one project for as long as it is live.** The live-name unique index means a
name cannot be moved between projects by a rename, an alias, or a race. The only way a name is
released is by withdrawing it — that is, by deleting the project that holds it (§2.7). This is
strictly stronger than the original draft promised and it is what makes the "permanent project
identity" requirement true at the storage layer rather than only in the prose.

**Creation is explicit and refuses duplicates.** `mpm_continuity create --name AgentShell`:

- normalises the name (trim, collapse internal whitespace; case preserved);
- **refuses** if a live project already holds that key — exact or case-insensitively — and returns
  that project's id instead ("a project named AgentShell already exists: cpj-…"). This is the "no
  silent duplicate" guarantee, enforced by the caller *and* by the partial unique index on
  `project_names`;
- **refuses** if the name is the id of a live project (see §2.6);
- if a **tombstoned** project once held the name, the response says so and the caller decides. The
  new project gets a **new** id. The tombstone is what makes the duplication *visible* rather than
  silent.

### 2.6 Rename and alias

References are **by id**, so a rename breaks nothing mechanically. The alias table preserves
*human and agent* references by name.

`mpm_continuity rename --project cpj-x --name NewName`:

1. the new name must not be held by any live project — as a canonical name **or** as an alias,
   exact or case-insensitively → refuse otherwise, naming the holder;
2. the old canonical key is **demoted to an alias** (one UPDATE);
3. the new name becomes the canonical key (one INSERT);
4. both happen in **one transaction**, so a resolver sees either the old key (as an alias) or the new
   one — never a gap in which the project is unreachable.

The order inside the transaction is **demote first, then promote**. The "at most one canonical live
name per project" partial unique index (§4.1) is evaluated per statement, so promoting before
demoting would violate it.

**The demote-then-promote order cannot deadlock**, because SQLite serialises write transactions:
the second transaction waits, then re-reads and either succeeds or is refused. What the index does
*not* prevent is a name being *offered* while another project still holds it — that is refused, and
the refusal names the holder.

`mpm_continuity alias --project cpj-x --add <key>` / `--remove <key>`:

- an alias must not be the id of a live project, and a live project's id must not be added as an
  alias to a *different* project. This keeps the id space and the name space disjoint. Continuity's
  own ids use the `cpj-` prefix, which is disjoint from the substrate's existing `pj_` derived form
  (`lifecycle_events.project_id`), so an adapter-reported `pj_…` can be adopted as an alias without
  any possibility of collision with a generated id.
- an alias must not be a case variant of another live name of the same project — the canonical name
  `AgentShell` already resolves `agentshell`, so an `agentshell` alias would be unreachable. (The
  original draft's §3.1 example contained exactly that pair; §3 has been corrected.)

### 2.7 Deletion semantics — deliberate, and different per entity

| Entity | Effect | Reversible? |
|---|---|---|
| **project** | soft-delete: `projects.deleted_at` and `projects.deleted_by` are set; the project is **no longer resolvable by name**; its `project_names` rows are **withdrawn** (`withdrawn_at` set), so they no longer resolve but are retained as history | the id, the timestamps, the actor and the name history remain as a tombstone; a later create with the same name yields a **new** id, and the create response reports the tombstone |
| **thread** | soft-delete: `continuity_threads.deleted_at` is set; the thread stops being selectable; its checkpoints are retained | yes — an undelete is a single UPDATE; the `revision` still fences concurrent writers |
| **checkpoint** | **not deletable in v1** — the ledger is append-only | — |

Deleting a project **does not** delete its threads or checkpoints. They become unreachable by name
and are reachable only by id. That is deliberate: a hard purge needs a reason-code and a backup flow
like `work_purge`, and inventing a second destructive mode in the same change is how data gets lost.
A project purge is **Stage 5** and is explicitly out of scope here (§10, non-goal 6).

**The destructive path is refused by the schema, not merely discouraged.** Every reference in §4.1 is
`ON DELETE RESTRICT`, so while a project has names, threads or checkpoints, a hard
`DELETE FROM projects` **fails with a foreign-key violation** and the ledger survives. The
substrate enforces foreign keys structurally — `SqliteWriteDSN` puts `_foreign_keys=1` in the DSN so
every pooled connection enforces them (`internal/core/db.go:98-155`) — so this is a storage
guarantee, not a convention. A soft delete never issues a `DELETE` at all.

**The guarantee is DSN-borne, and a reader should know that.** `InitSchema` deliberately does **not**
issue a `PRAGMA foreign_keys = ON` itself (`internal/core/db.go:2914-2921`); the enforcement comes
from the `_foreign_keys=1` in the **production open paths' DSN** (`internal/core/db.go:98-155`). A
connection opened some other way — a bare `sql.Open` without that DSN parameter — would not enforce
the containment guarantees above, because SQLite defaults foreign keys **off**. This is the
substrate's documented reason for preferring the DSN over a `PRAGMA`, and it is the reason the
acceptance matrix must open its databases the way production does rather than with a bare handle
(AT1, AT7).

**The tombstone names itself.** A withdrawn `project_names` row keeps its name, its kind and its
project id, and the project row keeps `deleted_at` and `deleted_by`. So "the tombstone for
AgentShell" is a query, not a guess: the create path reports the withdrawn rows and the tombstone
they belong to. The original draft deleted the name rows, which destroyed the only record of which
name the tombstone had — and made the "the create response reports the tombstone" promise
unimplementable (§12, A9).

**The name is free again, and the history is kept.** Withdrawing a name releases it for a successor
project; the withdrawn row remains as the history. This resolves the tension the original draft left
open — *"the name is free again"* and *"the tombstone reports the name"* cannot both hold under a
single name key, and the original DDL silently chose one of them by deleting the record. Here both
hold, because the live-name uniqueness is a **partial** index over live rows only (§4.1).

**Deletion requires an explicit confirmation token, and the token is a typo guard — not
authorization.** `mpm_continuity delete --project cpj-x --confirm cpj-x`. The confirmation is the
project id itself, so the operator cannot delete a project they did not name.

**What the confirmation does NOT do.** It is not an authorization mechanism, and this document does
not claim it is one. `projects` has no owner, `resolve` hands out the id and the name of every live
project, and therefore **any agent in the workspace can delete any project**. The workspace is a
single-operator trust domain, and that is a recorded risk (§9 OD3), not a safeguard. The mitigation
is **auditability, not authorization**:

- `projects.created_by` and `projects.deleted_by` record the writer identity, using the vocabulary
  the checkpoint row already carries (`mpm_session_id`, `framework_name`, `model_name`,
  `actor_kind`);
- the soft delete writes a `system_audit_log` row — the substrate's own audit log
  (`internal/core/schema.go:1238-1268`), which already carries `mpm_session_id`, `framework_name`,
  `framework_session_id`, `invocation_id` and `event_code`. **No fifth table is introduced.**

A real actor/authorization model is *not* proposed, because the substrate has no identity primitive
to build it on: `actor_kind` distinguishes agent/human/unknown, not *who*. Adding one is a separate
decision, and the operator's answer is required before the delete action is written (§9, OD3).

### 2.8 Optimistic concurrency

The brief requires *"optimistic concurrency protection against stale agents"*. The substrate's
existing CAS idiom is a **state-column predicate** (`WHERE id = ? AND state = ?`), never a version
number — the inventory found **no version-based CAS** anywhere in the tree (see §14 for the precise
wording).

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

**The checkpoint's own `revision` column is the PRE-increment value the writer observed** — that is,
the `expected_revision` the caller supplied, not the value the thread has after the write. A
checkpoint therefore records "the thread state this brief was written against", and the pair
(checkpoint.revision, thread.revision) is what a reader compares. AT13 asserts this explicitly,
because "the revision this was written at" is ambiguous otherwise.

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

**Every id in this section is a real id shape.** The three Continuity ids are their prefix followed
by the 16 lowercase hex characters `core.GenerateID()` returns; the four artifact ids are the
substrate's own 16 bare hex. Every one of them parses with the substrate's pointer parser
(`^[a-z0-9-]+$`, `internal/pointer/pointer.go:17`) — §14.2's probe runs the **real** inline parser
over every pointer this document contains and fails if one does not parse.

### 3.1 The project already exists

```jsonc
// the project stub
{"id":"cpj-3f9a4c7b8d9e0f12","created_at":1767225600,"updated_at":1767225600,"deleted_at":null,"created_by":"mpm-cli"}
// the resolution domain
{"name":"AgentShell","project_id":"cpj-3f9a4c7b8d9e0f12","kind":"canonical","withdrawn_at":null}
{"name":"the form builder repo","project_id":"cpj-3f9a4c7b8d9e0f12","kind":"alias","withdrawn_at":null}
```

There is one canonical name and one alias. All names resolve to `cpj-3f9a4c7b8d9e0f12`. The project
has no repository binding, no directory binding, and no ambient state — it is found by name alone,
from any directory, in any session, under any model.

Note what is **not** here: there is no `agentshell` alias. `AgentShell` resolves `agentshell`
already, because the column is `COLLATE NOCASE` — and a case-variant alias would be unreachable
(and, with the live-name unique index, unrepresentable).

### 3.2 Agent A resolves the project and finds two active threads

```jsonc
// mpm_continuity resolve --name AgentShell
{
  "success": true,
  "project": {"id":"cpj-3f9a4c7b8d9e0f12","name":"AgentShell","aliases":["the form builder repo"]},
  "threads": [
    {"id":"ct-a1c4e7d903b2f5a8","title":"Form builder rework","focus":"form-builder","state":"active","revision":7,"updated_at":1767300000},
    {"id":"ct-b1c4e7d903b2f5a8","title":"Public API surface","focus":"api","state":"active","revision":3,"updated_at":1767290000}
  ],
  "resolved_by": "exact name match"
}
```

Both threads are active. An unqualified `resume` would be **ambiguous** — two agents, two lines.
Agent A's user named the form builder, so Agent A selects by focus:

```jsonc
// mpm_continuity resume --project cpj-3f9a4c7b8d9e0f12 --focus form-builder
{
  "success": true,
  "project": {"id":"cpj-3f9a4c7b8d9e0f12","name":"AgentShell"},
  "thread":  {"id":"ct-a1c4e7d903b2f5a8","title":"Form builder rework","focus":"form-builder","state":"active","revision":7},
  "checkpoint": {
    "id":"ck-77a0b3c6d9e2f4a7","created_at":1767300000,"revision":7,"supersedes":"ck-76a0b3c6d9e2f4a7",
    "headline":"Validation lives in form.ts; the submit path is still on the old action shape.",
    "facts": [
      {"kind":"decision","id":"d91f4c0a7b3e5682","pointer":"mpm://decision/d91f4c0a7b3e5682","note":"Submit stays a plain form POST"},
      {"kind":"lesson","id":"04b7e9d2c6f13a58","pointer":"mpm://lesson/04b7e9d2c6f13a58","note":"Do not move validation into the reducer"}
    ],
    "hypotheses": [
      {"kind":"theory","id":"55e8a1c4d7b20963","pointer":"mpm://theory/55e8a1c4d7b20963","note":"A schema-first validator will cover the 3 open field cases"}
    ],
    "state": [
      {"kind":"work","id":"12c6f9b3a4d7e058","pointer":"mpm://work/12c6f9b3a4d7e058","summary":"Port the validator","status":"open","verification":"unverified"}
    ],
    "next_actions": [
      {"text":"Add the three missing field cases to the validator","ref":{"kind":"work","id":"12c6f9b3a4d7e058"}}
    ]
  },
  "next_action": "Add the three missing field cases to the validator",
  "stale": {
    "checkpoint_age_seconds": 5400,
    "checkpoint_revision": 7,
    "thread_revision": 7,
    "session_ended_after_checkpoint": false,
    "session_end_state": null
  },
  "bytes": 1180
}
```

**What is stored and what is returned.** The checkpoint row stores each reference as
`{kind, id, note?}` and **nothing else** — the `pointer` in this response is *derived* by the read
path from `kind` and `id`, never stored, so a stored pointer can never disagree with its reference
kind (§2.3). The `summary`, `status` and `verification` on the `state` entry are **read live from
the work row at resume time**; they are not copied into the checkpoint. A work item that has since
been reopened or verified is reported with its *current* state, not the state as it was when the
brief was written.

The checkpoint carries **pointers**, not copies, and each pointer kind matches its reference kind
(§2.3). The agent follows `mpm://work/12c6f9b3a4d7e058` when it needs the full work item, and
`mpm://theory/55e8a1c4d7b20963` when it wants the hypothesis. The facts and the hypothesis point at
artifacts that carry their own evidence; the checkpoint does not duplicate their contents.

**The `stale` block is what makes an old brief visibly old.** `checkpoint_age_seconds` is the age of
the brief; `session_ended_after_checkpoint` says whether a session that wrote it ended afterwards.
A fresh agent that sees *"the current checkpoint is 90 minutes old"* can ask for a fresh checkpoint
rather than silently trusting a stale brief (§9, OD2; the limitations are stated there).

### 3.3 Agent A writes a new checkpoint

```jsonc
// mpm_continuity checkpoint --project cpj-3f9a4c7b8d9e0f12 --thread ct-a1c4e7d903b2f5a8 --expected-revision 7
{
  "success": true,
  "thread": {"id":"ct-a1c4e7d903b2f5a8","revision":8},
  "checkpoint": {"id":"ck-78a0b3c6d9e2f4a7","revision":7,"supersedes":"ck-77a0b3c6d9e2f4a7"}
}
```

The checkpoint is inserted and the thread's pointer is advanced **in one transaction**.
`ck-78a0b3c6d9e2f4a7` supersedes `ck-77a0b3c6d9e2f4a7` — a checkpoint of the **same thread**, which
the composite foreign key requires (§4.1, D1) — and `ck-77a0b3c6d9e2f4a7` is retained. Nothing is
overwritten destructively. Note that the checkpoint's `revision` is **7** — the pre-increment value
it was written against — while the thread's is now 8 (§2.8).

### 3.4 Agent B, on its own thread, is unaffected

Agent B writes to `ct-b1c4e7d903b2f5a8` with `--expected-revision 3`. The CAS is **per thread**, so
the two agents never contend. Agent B's write lands and `ct-b1c4e7d903b2f5a8` moves to revision 4.

### 3.5 The stale-writer case — both agents on the same thread

Suppose Agent B had been pointed at `ct-a1c4e7d903b2f5a8` as well (say, a user said "and you handle
the API *in that thread*"). Both read revision 7. Agent A writes first → revision 8. Agent B's
write:

```jsonc
// mpm_continuity checkpoint --project cpj-3f9a4c7b8d9e0f12 --thread ct-a1c4e7d903b2f5a8 --expected-revision 7
{
  "success": false,
  "error": "stale_thread",
  "detail": "thread ct-a1c4e7d903b2f5a8 is at revision 8, not 7 — another writer committed since you read it",
  "current_revision": 8,
  "current_checkpoint_id": "ck-78a0b3c6d9e2f4a7",
  "hint": "re-read with `mpm_continuity resume --thread ct-a1c4e7d903b2f5a8`, merge your change onto ck-78a0b3c6d9e2f4a7, and write with --expected-revision 8"
}
```

**Agent B's checkpoint is not written.** No orphan row, no clobbered pointer, and **no checkpoint id
is burned** — the id is generated once and the insert is rolled back with the transaction, so a retry
with the same id succeeds (§5, T2). Agent B rebases. This is the "safe checkpoint replacement"
guarantee and the "optimistic concurrency protection against stale agents" guarantee, expressed in
one `WHERE` clause.

### 3.6 A crash, with no session-end event

Agent A's process dies right after writing `ck-78a0b3c6d9e2f4a7`. There is no handoff — the process
never reached session end, and `EndSessionV2` never ran.

The next agent to pick up AgentShell runs `mpm_continuity resume --project cpj-3f9a4c7b8d9e0f12
--focus form-builder` and gets `ck-78a0b3c6d9e2f4a7` — the last committed checkpoint. **The committed
brief was not lost**, because the checkpoint was committed by an explicit write rather than by a
session-end event.

Had the crash happened *during* the write, the transaction rolls back and the thread still points at
`ck-77a0b3c6d9e2f4a7` — the previous good checkpoint (§5, T1).

**What this case does not claim.** The work Agent A did between its last checkpoint and the crash was
not captured, and no mechanism in this design captures it. The `stale` block makes the *age* of the
brief visible; it does not recover the lost window. See §9, OD2.

### 3.7 The fresh-agent success criterion

> *a fresh agent can find AgentShell, recover the correct thread and continue useful work without a
> transcript or project-management machinery.*

A fresh agent, in a new session, under a different model, in a different directory:

1. `mpm_continuity resolve --name AgentShell` → the project id, its aliases, and its threads.
2. `mpm_continuity resume --project cpj-3f9a4c7b8d9e0f12 --focus form-builder` → the thread, the
   checkpoint, the resolved references, the next action, and the staleness block.
3. `mpm_resolve --uri mpm://work/12c6f9b3a4d7e058` → the full work item, if it is needed.

Three calls, ~1.2 KB of checkpoint, no transcript, no board, no progress metric, no assignment.

**The limit of this criterion.** The three calls above assume the agent knows to make them. Nothing
in the substrate surfaces the project to an agent that has not been told to look: the wake context
gains no new field in v1 (§4.6), and the routing-pipeline integration that would surface it is
Stage 4 — conditional on the operator deciding where the session→project binding lives (§9, OD1).
This is a real gap in the operator's requirement and it is not closed by this document.

---

## 4. Schema and API proposal

### 4.1 DDL

**This DDL was executed against a disposable SQLite database (3.46.1) with foreign keys enabled.
Every negative case below was observed to be **refused** by the database, and every positive case was
observed to land. The negative set is: the case-variant name, the case-variant focus, the
cross-project checkpoint, the invalid and cross-thread `last_checkpoint_id`, the cross-project thread
supersession, the **cross-thread and cross-project `supersedes`**, the hard delete of a project with
children, the second canonical name, the malformed JSON, the non-array JSON, and the oversized
headline. The probe reads this block out of this document, so it cannot drift from it, and its
output is in §14.2.**

```sql
-- ── projects ────────────────────────────────────────────────────────────
-- The permanent stub. An identity that outlives every thread, every
-- checkpoint and every session. There is no project state machine: a
-- project is alive (deleted_at IS NULL) or deleted.
--
-- created_by / deleted_by carry the WRITER IDENTITY, not an owner:
-- the workspace is a single-operator trust domain and there is no
-- authorization model in this design (§2.7, §9 OD3).
CREATE TABLE IF NOT EXISTS projects (
    id          TEXT PRIMARY KEY,                    -- 'cpj-' || 16 lowercase hex
    created_at  INTEGER NOT NULL DEFAULT (CAST(strftime('%s','now') AS INTEGER)),
    updated_at  INTEGER NOT NULL DEFAULT (CAST(strftime('%s','now') AS INTEGER)),
    created_by  TEXT,
    deleted_at  INTEGER,                             -- NULL = alive
    deleted_by  TEXT                                -- NULL = alive
);

-- ── project_names ───────────────────────────────────────────────────────
-- The resolution domain. Every key that resolves to a project lives
-- here.
--
-- name is COLLATE NOCASE and the live-name uniqueness is a PARTIAL
-- index over live rows only. Together they make "no silent duplicate
-- project creation" a database guarantee rather than a convention,
-- and they make a name reusable after its holder is deleted while the
-- withdrawn row is retained as history.
--
-- The key is a surrogate id, not the name, precisely so that a
-- withdrawn name and its successor's live name can coexist.
CREATE TABLE IF NOT EXISTS project_names (
    id           INTEGER PRIMARY KEY,
    name         TEXT NOT NULL COLLATE NOCASE,
    project_id   TEXT NOT NULL REFERENCES projects(id) ON DELETE RESTRICT,
    kind         TEXT NOT NULL CHECK (kind IN ('canonical','alias')),
    created_at   INTEGER NOT NULL DEFAULT (CAST(strftime('%s','now') AS INTEGER)),
    withdrawn_at INTEGER                            -- NULL = live
);
CREATE INDEX IF NOT EXISTS idx_project_names_project ON project_names(project_id);
-- At most one LIVE holder of a name, case-insensitively. A withdrawn
-- row is history and does not occupy the name.
CREATE UNIQUE INDEX IF NOT EXISTS idx_project_names_live_name
    ON project_names(name COLLATE NOCASE) WHERE withdrawn_at IS NULL;
-- At most one canonical LIVE name per project.
CREATE UNIQUE INDEX IF NOT EXISTS idx_project_names_one_canonical
    ON project_names(project_id) WHERE kind = 'canonical' AND withdrawn_at IS NULL;

-- ── continuity_threads ──────────────────────────────────────────────────
-- An independent line of work inside a project. Several may be active
-- at once; each carries its own revision for optimistic concurrency.
--
-- Every reference is ON DELETE RESTRICT: a hard delete of a project
-- with children is refused by the database, so the ledger cannot be
-- destroyed by a cascade (§2.7).
--
-- The two composite foreign keys are what make "project" a containment
-- boundary in the STORAGE:
--   * (superseded_by, project_id) -> a thread may only be superseded
--     by a thread of the SAME project;
--   * (last_checkpoint_id, id) -> the resume pointer may only name a
--     checkpoint of THIS thread, and it must exist.
CREATE TABLE IF NOT EXISTS continuity_threads (
    id                  TEXT PRIMARY KEY,            -- 'ct-' || 16 lowercase hex
    project_id          TEXT NOT NULL REFERENCES projects(id) ON DELETE RESTRICT,
    title               TEXT NOT NULL CHECK (length(CAST(title AS BLOB)) <= 200),
    focus               TEXT COLLATE NOCASE,         -- normalised slug, unique per project
    state               TEXT NOT NULL DEFAULT 'active'
                        CHECK (state IN ('active','dormant','completed','superseded')),
    revision            INTEGER NOT NULL DEFAULT 0, -- optimistic-concurrency token
    last_checkpoint_id  TEXT,                        -- the current resume brief
    superseded_by       TEXT,
    created_at          INTEGER NOT NULL DEFAULT (CAST(strftime('%s','now') AS INTEGER)),
    updated_at          INTEGER NOT NULL DEFAULT (CAST(strftime('%s','now') AS INTEGER)),
    completed_at        INTEGER,
    deleted_at          INTEGER,
    FOREIGN KEY (superseded_by, project_id)
        REFERENCES continuity_threads(id, project_id) ON DELETE RESTRICT,
    FOREIGN KEY (last_checkpoint_id, id)
        REFERENCES continuity_checkpoints(id, thread_id) ON DELETE RESTRICT
);
CREATE INDEX IF NOT EXISTS idx_continuity_threads_project
    ON continuity_threads(project_id, state, updated_at DESC);
-- One live thread per focus, case-insensitively. A superseded or
-- deleted thread releases its focus, so the same focus can be
-- re-opened later.
CREATE UNIQUE INDEX IF NOT EXISTS idx_continuity_threads_focus
    ON continuity_threads(project_id, focus)
    WHERE focus IS NOT NULL AND deleted_at IS NULL AND state != 'superseded';
-- The parent key for the two composite foreign keys below.
CREATE UNIQUE INDEX IF NOT EXISTS idx_continuity_threads_id_project
    ON continuity_threads(id, project_id);

-- ── continuity_checkpoints ──────────────────────────────────────────────
-- The append-only ledger of resume briefs. A checkpoint is never
-- edited; it is superseded by a later one, and the thread's
-- last_checkpoint_id names the current one.
--
-- project_id is DENORMALISED from the thread, and the composite
-- foreign key is what keeps the denormalisation honest: a checkpoint
-- cannot name a project other than its thread's. The separate
-- projects(id) reference is the second line of defence.
--
-- The two composite foreign keys here are the other half of the
-- containment boundary:
--   * (thread_id, project_id) -> the checkpoint belongs to its thread's
--     project, and therefore to that project;
--   * (supersedes, thread_id) -> a checkpoint may only supersede
--     another checkpoint of ITS OWN thread.
CREATE TABLE IF NOT EXISTS continuity_checkpoints (
    id                   TEXT PRIMARY KEY,           -- 'ck-' || 16 lowercase hex
    thread_id            TEXT NOT NULL,
    project_id           TEXT NOT NULL,
    revision             INTEGER NOT NULL,          -- the PRE-increment thread revision observed
    headline             TEXT CHECK (length(CAST(headline AS BLOB)) <= 200),
    facts                JSON NOT NULL DEFAULT '[]'
                         CHECK (json_valid(facts) AND json_type(facts) = 'array'),
    hypotheses           JSON NOT NULL DEFAULT '[]'
                         CHECK (json_valid(hypotheses) AND json_type(hypotheses) = 'array'),
    state                JSON NOT NULL DEFAULT '[]'
                         CHECK (json_valid(state) AND json_type(state) = 'array'),
    next_actions         JSON NOT NULL DEFAULT '[]'
                         CHECK (json_valid(next_actions) AND json_type(next_actions) = 'array'),
    supersedes           TEXT,                        -- a checkpoint of THIS thread
    created_at           INTEGER NOT NULL DEFAULT (CAST(strftime('%s','now') AS INTEGER)),
    -- The writer's identity, on the row rather than in artifact_provenance
    -- (see §4.5 for why).
    mpm_session_id       TEXT,
    framework_session_id TEXT,
    actor_kind           TEXT,
    framework_name       TEXT,
    model_name           TEXT,
    FOREIGN KEY (thread_id, project_id)
        REFERENCES continuity_threads(id, project_id) ON DELETE RESTRICT,
    FOREIGN KEY (supersedes, thread_id)
        REFERENCES continuity_checkpoints(id, thread_id) ON DELETE RESTRICT,
    FOREIGN KEY (project_id) REFERENCES projects(id) ON DELETE RESTRICT
);
CREATE INDEX IF NOT EXISTS idx_continuity_checkpoints_thread
    ON continuity_checkpoints(thread_id, created_at DESC);
-- The parent key for continuity_threads' (last_checkpoint_id, id) FK
-- AND for this table's own (supersedes, thread_id) FK.
CREATE UNIQUE INDEX IF NOT EXISTS idx_continuity_checkpoints_id_thread
    ON continuity_checkpoints(id, thread_id);
```

**Notes on the DDL.**

- **The id prefixes use a hyphen, not an underscore.** The substrate's pointer id charset is
  `^[a-z0-9-]+$` (`internal/pointer/pointer.go:17`); `cpj_…` would be **rejected** by
  `parsePointerURI` and by `pointer.Parse`. `cpj-`/`ct-`/`ck-` fit unchanged. The pointer package
  is **not** modified (§12, A2; §14).
- **The ids are the substrate's own, prefixed.** Each Continuity id is one of the three prefixes
  followed by the 16 lowercase hex characters `core.GenerateID()` returns
  (`internal/core/db.go:5283`) — `cpj-3f9a4c7b8d9e0f12`, `ct-…`, `ck-…`. Reusing the house
  generator means there is exactly one id scheme in the tree, the artifact ids and the Continuity
  ids have the same shape, and nothing has to be invented (§14.3).
- **`JSON` does not validate, and its declared type has NUMERIC affinity.** SQLite's declared `JSON`
  type is not a real type: it carries NUMERIC affinity, so the literal string `'42'` is silently
  coerced to the *integer* 42, `'1.5'` to the *real* 1.5, while `'[]'`, `'{}'`, `'"str"'` and
  `'null'` stay TEXT. `CHECK (json_valid(...))` alone therefore accepts a bare scalar, and it
  accepts an object where the code layer expects an array. The four columns carry
  `CHECK (json_valid(x) AND json_type(x) = 'array')` instead, which makes the documented array
  shape **structural**: `'{}'`, `'42'`, `'1.5'` and `'"str"'` are all refused, and only a JSON
  array is stored (§13, P4; §14.2 E17).
- **`length(CAST(x AS BLOB))` is a byte count.** `length()` on TEXT counts characters, not bytes; the
  documented budgets are in bytes.
- **`supersedes` is a COMPOSITE foreign key, and it is `ON DELETE RESTRICT`.**
  `FOREIGN KEY (supersedes, thread_id) REFERENCES continuity_checkpoints(id, thread_id)` closes both
  the cross-thread and the cross-project case, because the referenced checkpoint must carry the
  *same* `thread_id` — and therefore the same project. The parent key is
  `idx_continuity_checkpoints_id_thread`, which the `last_checkpoint_id` FK already needed.
  `ON DELETE SET NULL` is **not** usable here: SQLite applies `SET NULL` to *every* column of the
  child key, so it would try to null the `NOT NULL` `thread_id` and the delete would be refused with
  a misleading `NOT NULL constraint failed` rather than a foreign-key error. `RESTRICT` refuses the
  same delete with the correct error — and refusing it is the right behaviour anyway, because the
  checkpoint ledger is append-only (§2.7): a superseded brief is history and is not deleted.
- **Seven foreign-key clauses in total: four composite, three single-column.** The composite four
  are `(superseded_by, project_id)`, `(last_checkpoint_id, id)`, `(thread_id, project_id)` and
  `(supersedes, thread_id)`; the single-column three are `project_names.project_id`,
  `continuity_threads.project_id` and `continuity_checkpoints.project_id`. §5's T13 and §11's
  summary enumerate the same four (§12, D2).
- **The four tables are created by the migration, not by the base DDL** — see §6.2. The consequence
  is that a reader of `internal/core/schema.go` will not see them, and that is deliberate: it keeps
  the DDL out of `BaseTables`, and `BaseTables` is what `attachShared` propagates into the
  cross-workspace shared schema (§6.2, §12 A12).

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
| `thread` | read+write | `project`, `op` ∈ `open` \| `list` \| `select` \| `close` \| `supersede`, plus `thread`, `title`, `focus`, `state`, `expected_revision` | the thread(s) |
| `checkpoint` | write | `project`, `thread`, `expected_revision`, `headline`, and the four sections `facts`, `hypotheses`, `state`, `next_actions` — each section an array of **references**, each reference exactly `{kind, id, note?}` (§2.3) | the new checkpoint and the thread's new revision |
| `resume` | read | `project`, and one of `thread` \| `focus` \| `title` | the thread, the materialised checkpoint, the next action, **and the `stale` block** |

**The `thread` action's five ops, and the two meanings of `focus`.** `op='list'` returns the
project's threads. `op='select'` picks **one** thread by `thread`, `focus` or `title`, using tiers 1–3
of the §2.4 ladder — the selector must be **explicit**, because the unqualified fallback (tiers 4–5)
belongs to `resume`. It is exposed separately so an agent can inspect a thread's state without
materialising its brief. `op='open'` creates a thread with a `title` and an optional `focus`.
`op='close'` and `op='supersede'` change a thread's state, and both require `expected_revision`.

**`focus` is a selector in three of those ops and a value in one.** As a *selector* (`op='select'`,
and on `resume`) it names the thread to find. As a *value* (`op='open'`) it is the normalised slug
the new thread will be reachable by. The CLI distinguishes them by which flag is present —
`--select --focus <slug>` looks a thread up, `--open --focus <slug>` sets the new thread's slug —
so a caller can never silently set a slug when it meant to select one.

**`resolve` never creates.** A caller that wants a project created calls `create`. That separation
is the "no silent duplicate" guarantee at the API level; the partial unique index is the guarantee at
the storage level. Both are needed: the index catches a race, the API shape prevents the intent.

**`checkpoint` is the only action that writes a brief, and it is always explicit.** No automatic
checkpointing, no fold, no implicit write on session end. The brief's "recovery after crashes
without requiring a session-end event" is satisfied by the checkpoint being writable *at any time*,
not by a background writer (which would add ambient overhead and could write a brief nobody
sanctioned).

**`resume` reports staleness.** It returns, alongside the brief:

| Field | Meaning |
|---|---|
| `checkpoint_age_seconds` | `now - checkpoint.created_at` |
| `checkpoint_revision` / `thread_revision` | the revision the brief was written against, and the thread's current revision — they differ when the brief is behind |
| `session_ended_after_checkpoint` | whether the session that wrote the checkpoint has a `session_handoffs` row with `ended_at > checkpoint.created_at` |
| `session_end_state` | that row's `ended_state` (`clean`/`crashed`/`interrupted`/`force_end` — the closed vocabulary of `internal/core/schema.go:1322-1323`), or `null` |

**The join column is `mpm_session_id`, and the selection rule is "the latest one".** The
`session_handoffs` table's `session_id` column is the **legacy UNIQUE** one
(`internal/core/schema.go:1321`); it is optional correlation metadata, and it is *not* what identifies an MPM
session. The canonical identity lives in the `mpm_session_id` column, which was added by a migration
(`internal/core/migration_session_handoffs_mpm_session_id.go:128`) and which the substrate's own
handoff lookup uses (`handoffColumnMPMSession = "mpm_session_id"`,
`internal/core/handoff.go:420`).

**`mpm_session_id` is not unique**, so one session can have several handoffs: *"mpm_session_id is NEVER
unique, so multiple handoffs from the same lifecycle all share the same value"*
(`internal/core/handoff.go:157-158`). The selection rule is therefore **the same one the substrate
already uses** for its own previous-handoff lookup — `ORDER BY ended_at DESC, rowid DESC LIMIT 1`.
The substrate states that tie-breaker in the comment above its own query
(`internal/core/handoff.go:378`) and implements it in the query itself
(`internal/core/handoff.go:468-469`). So `session_end_state` names **one** row: the most recently
ended handoff of the writing session. The `rowid` tiebreak makes the choice deterministic when two
handoffs share an `ended_at` — which is why the substrate's own comment says *"multiple handoffs
inserted in the same second need a secondary sort"*. A checkpoint row carries the writing session's
`mpm_session_id`, so the join is exact.

**The implementation and the limits of the session-end signal are in §9, OD2** — in particular the
90-day TTL and the "no handoff row at all" case.

**`resume` labels the headline as the writer's assertion.** The headline is the one free-form field
in the structure, and it is the only place speculation can enter. The response presents it as the
writer's claim, not as a verified fact (§2.3).

### 4.3 The CLI facade

`mpm continuity <sub>` — the same shape as `mpm work item` (`cmd/mpm/handlers_work.go:78-84`): a
thin, discoverable facade over `mpm call mpm_continuity --payload '{…}'`.

```
mpm continuity resolve [name]
mpm continuity create <name>
mpm continuity rename <project> <new-name>
mpm continuity alias <project> --add <key> | --remove <key>
mpm continuity delete <project> --confirm <project>
mpm continuity thread <project> --list
mpm continuity thread <project> --select [--thread <id> | --focus <slug> | --title <t>]
mpm continuity thread <project> --open --title <t> [--focus <slug>]
mpm continuity thread <project> --close --thread <id> --expected-revision <n>
mpm continuity thread <project> --supersede <replacement-id> --thread <id> --expected-revision <n>
mpm continuity checkpoint <project> --thread <id> --expected-revision <n> [--headline …]
mpm continuity resume <project> [--thread <id> | --focus <slug> | --title <t>]
```

**Every sub-command maps to exactly one `mpm_continuity` action** (AT25), and the mapping is
mechanical:

| Sub-command | Action | `op` / selector |
|---|---|---|
| `resolve` | `resolve` | — |
| `create` | `create` | — |
| `rename` | `rename` | — |
| `alias` | `alias` | — |
| `delete` | `delete` | — |
| `thread --list` | `thread` | `op='list'` |
| `thread --select` | `thread` | `op='select'` + the selector |
| `thread --open` | `thread` | `op='open'` |
| `thread --close` | `thread` | `op='close'` |
| `thread --supersede` | `thread` | `op='supersede'` |
| `checkpoint` | `checkpoint` | — |
| `resume` | `resume` | the selector |

**`thread --open` is the only sub-command that creates a thread, and it is explicit.** It does not
pick an existing thread: a caller that wants to continue a thread uses `--select` or `resume`.
If `--open` names a focus that a live thread already holds, the partial unique index refuses the
insert (§5, T5) and the error names the thread that holds it — the refusal is the same one the
storage gives, surfaced with a human-readable pointer to the winner.

### 4.4 Pointers

**Three pointer kinds are added**, joining the existing five:

| Kind | URI | Resolves to | Id shape |
|---|---|---|---|
| project | `mpm://project/<id>` | the project stub, its canonical name and its aliases | `cpj-` + hex |
| thread | `mpm://thread/<id>` | the thread, its state and its current checkpoint id | `ct-` + hex |
| decision | `mpm://decision/<id>` | the `memories` row with `collection='decisions'` | the substrate's own memory id |

**The id charset is not widened.** The existing parser accepts `^[a-z0-9-]+$`
(`internal/pointer/pointer.go:17`), and the inline copy in
`internal/core/tools/handlers.go:7857-7862` enforces the same set character by character. The
Continuity prefixes therefore use a **hyphen**: `cpj-`, `ct-`, `ck-`. `internal/pointer/pointer.go`
and `parsePointerURI` are **not modified** — the change surface gains a row for the *id shape*, not
for the pointer grammar (§6.4).

**The `decision` kind follows the `theory` precedent exactly.** `mpm://theory/<id>` resolves a
`memories` row through `GetMemory` and then asserts `collection == 'theories'`
(`internal/core/tools/handlers.go:7660-7668`). `mpm://decision/<id>` is the same shape with
`collection == 'decisions'`. A decision is *already* reachable through `mpm://memory/<id>` — and so
is a theory — because `GetMemory` selects by id alone
(`internal/core/web_db.go:258`); the typed kind adds the assertion, so the resolver can say *"this
id is not a decision"* instead of returning it unlabelled. The compatibility consequence is stated
in §9, OD5.

**The surfaces that must change together** — the resolver's kind check
(`internal/core/tools/handlers.go:7562`), the tool description
(`internal/core/tools/registry_list.go:759-762`), and the pointer package's own
`internal/pointer/resolve.go` — are listed in §6.4. `mpm_help` picks the registry entry up
automatically: `cmd/mpm/call.go:550` iterates `tools.Registry`, and
`cmd/mpm/call_help_test.go:74` pins that.

### 4.5 Provenance — and why the artifact vocabulary is not extended

`artifact_provenance.artifact_type` carries a **CHECK** constraint generated from the canonical
`ProvenanceArtifactTypes` slice (`internal/core/schema.go:23-26`, `:30-36`). Widening it to admit
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
first-class provenance artifacts is an operator decision (§9, R6).

### 4.6 What is deliberately *not* extended

- **`active.json` is not extended.** The obvious place to record "the session's current project" is
  `ActiveState` — and it would make the wake context able to carry a checkpoint for free. Two
  reasons not to: (i) `saveActiveJSONAt` writes with `os.WriteFile`, **not** the tmp+rename atomic
  swap that AGENTS.md §3 requires of cross-process files, so adding a correctness-relevant pointer
  to a file that can be truncated by a crash is a bad trade; and (ii) it adds ambient state to every
  session for a capability that the product contract reaches by explicit name.
- **The wake context gains no new top-level field in v1.** The continuity brief is fetched by an
  explicit call, which costs zero tokens on every wake. The routing-pipeline integration is Stage 4
  and is conditional on the operator deciding where a session→project binding should live (§9, OD1).
- **`evidence.artifact_type` is not widened** (§4.5).
- **No `works` column.** A work item is not made project-scoped. The relationship runs the other
  way: a checkpoint's `state` section *references* work items by id.
- **The shared database is not a project store.** The `shared` schema is the cross-workspace
  broadcast store; the Continuity tables are local to the workspace (§6.2, §9, P3).

---

## 5. Crash and concurrency threat model

| # | Threat | Mechanism | Outcome |
|---|---|---|---|
| **T1** | Crash between the checkpoint INSERT and the thread pointer UPDATE | both in one `WithTx`; the callback's error or the process's death rolls the transaction back | the thread still points at the previous checkpoint; no orphan |
| **T2** | `WithTx` restarts the transaction on a stale snapshot | the checkpoint id is generated **once, before** the callback, and the insert is `ON CONFLICT(id) DO NOTHING` | the restart re-inserts the *same* id; no duplicate checkpoint, and **no id is burned**. The id is the substrate's own `core.GenerateID()` — 16 lowercase hex, **not** content-derived — so the `ON CONFLICT` clause is a **belt-and-braces safety net that in practice never fires**: it is the `evidence` pattern's clause (`internal/core/evidence_store.go:92-105`), and `evidence`'s id *is* identity-derived, which is why the clause matters there. Here the restart-safety actually comes from the **transaction rollback**, which is sufficient. The clause is kept because it is free and because a future content-derived id would need it (§14.3) |
| **T3** | Two agents write checkpoints to one thread concurrently | `WHERE id = ? AND revision = ?`; exactly one gets `RowsAffected == 1` | the loser is told `stale_thread` with the current revision; nothing is written |
| **T4** | Two agents create a project with the same name concurrently | the partial UNIQUE index on live `project_names.name` (NOCASE) | exactly one INSERT lands; the other gets a UNIQUE violation, which the tool turns into "a project named X already exists: cpj-…" |
| **T5** | Two agents open a thread for the same focus concurrently | the partial UNIQUE index on `(project_id, focus)` (NOCASE) | one thread; the other gets "a thread with focus X already exists (ct-…)" |
| **T6** | A rename concurrent with a resolve | the demote + promote are one transaction, demote first | a resolver sees the old key as an alias or the new key — never a gap |
| **T7** | A thread is superseded while an agent holds it | the next write's CAS includes `revision` and the state transition bumped it | the agent's write is refused with `stale_thread`; the response names the superseding thread |
| **T8** | A project is deleted while an agent holds a thread | the soft delete bumps the thread revisions (there is **no** cascade — every reference is `ON DELETE RESTRICT`); the CAS fails | the write is refused; the agent is told the project is deleted |
| **T9** | A checkpoint references an artifact that is later shredded | `resume` resolves each reference independently and marks an unresolvable one `missing` | the checkpoint stays readable; the missing reference is reported, not fatal |
| **T10** | A checkpoint's payload exceeds the budget | the write path validates the byte cap and the section counts and **rejects** the write; the byte cap is the primary gate | an oversized brief is never committed; the error names the section and the limit, and says whether bytes or count fired |
| **T11** | An agent writes a checkpoint with a stale `expected_revision` after its own earlier write | the CAS | refused — the agent must re-read. This is deliberate: silently accepting a write whose basis has moved is how a checkpoint loses meaning |
| **T12** | Two workspaces hold projects with the same name | projects are workspace-scoped (the database is the scope) | they are independent projects; there is no cross-workspace resolution. See §9, D1 |
| **T13** | **A checkpoint or a pointer crosses a project boundary** | the **four** composite foreign keys: `(thread_id, project_id)` ties a checkpoint to its thread's project, `(last_checkpoint_id, id)` ties the resume pointer to its own thread, `(superseded_by, project_id)` ties a thread supersession to the same project, and `(supersedes, thread_id)` ties a checkpoint supersession to its own thread | **refused by the schema** — the insert or update fails with a foreign-key violation. "Project" is a containment boundary in the *storage*, not only in the code. This row was **incomplete** in the previous revision, which listed three; the fourth is D1 |
| **T14** | **Two agents rename A→B and B→A at the same time** | the live-name partial unique index: a name is bound to one project for as long as it is live, so neither target is free | both renames are **refused**, each naming the holder. The alias graph cannot form a cycle; there is no deadlock, because SQLite serialises write transactions and the demote-then-promote order is fixed within each transaction (§2.6) |
| **T15** | **A hard delete is attempted on a project with children** | every reference is `ON DELETE RESTRICT` and foreign keys are enforced structurally on every pooled connection | the `DELETE` is refused and the ledger survives. The soft-delete path never issues a `DELETE` |

**What is not claimed.** A checkpoint records what an agent asserted. It is not a proof that any
external thing happened, exactly as a settlement is not a proof that an external effect happened
(the delivery-state record's C8). The checkpoint's integrity rests on the artifacts it references —
which carry their own evidence and their own confidence — not on the checkpoint's own assertion.

**What is not claimed about the loss window.** Explicit-only checkpointing means the work between
the last checkpoint and a crash is not captured. The `stale` block makes the *age* of the brief
visible; it does not recover the window (§9, OD2).

---

## 6. Backward-compatibility and migration analysis

### 6.1 What changes for an existing database

| Change | Kind | Risk |
|---|---|---|
| **four** new tables | additive | none — `CREATE TABLE IF NOT EXISTS` on a database that has never seen them |
| the `mpm_continuity` tool in the registry | additive | none — one slice entry; `mpm call`, the MCP `tools/list` and `mpm_help` pick it up automatically |
| `mpm continuity` in the CLI router | additive | none |
| **three** new pointer kinds in `mpm_resolve` | additive | none — three more branches in an existing switch; the five existing kinds are unchanged |
| **four** entries in `CanonicalMPMSchema` | **required** | the restore allow-list must list the new tables, or `mpm restore-db` will reject a dump that contains them |
| `docs/SPEC.md` | **required** | this change adds a durable entity family and a user-facing command; the product contract must describe them (§6.4) |
| the `contextual_candidates` kind vocabulary | additive, **Stage 4** | the vocabulary is closed; adding `continuity` extends it. Deferred so v1 changes no routing behaviour |

**Nothing existing is read differently.** No existing writer changes. No existing reader changes.
No existing predicate changes. **No existing public contract changes** — in particular the pointer
id charset is *not* widened (§4.4) and `internal/pointer/pointer.go` is not touched. This is the
same additive, behaviour-neutral shape the delivery-state Stage A was specified to have
(`docs/dev/issue-178-delivery-state-design-2026-11-04.md`), and the same discipline applies: the
migration is strictly additive, idempotent, forward-only, and lands lazily.

**The base DDL in `internal/core/schema.go` is deliberately NOT changed.** This is the shape the
delivery-state Stage A implementation chose and recorded as its own unresolved issue #3
([PR #225](https://github.com/flowbyte-com/mpm/pull/225)): *"adding these to the base DDL would
create a second definition of them and a drift risk. A fresh database and a migrated database
therefore have literally identical schemas, produced by one code path."* Continuity adopts the
same shape, for the same reason, and §12 A14 records the correction.

### 6.2 The migration mechanics

The Continuity DDL lives in **one** place: the migration file
`internal/core/migration_continuity_project_model.go` (new). It is **not** in `BaseTables`, and
**not** in `WorkTables` or any other schema slice.

**Why not `BaseTables` — the shared-schema hazard.** `attachShared` (`internal/core/db.go:2094-2175`)
ATTACHes the configured shared database as the `shared` schema and then runs **`BaseTables` against
it**, twice:

```go
// db.go:2132 — the CREATE TABLE loop
for _, ddl := range BaseTables {
    if !strings.HasPrefix(ddl, "CREATE TABLE") { continue }
    sharedDDL := rewriteTablePrefix(ddl, "shared.")
    …
}
// db.go:2160 — the CREATE INDEX loop, same slice
```

So a `BaseTables` entry is not a local table — it is a table in the **cross-workspace broadcast
store**, with its foreign keys rewritten along with it. The shared schema exists for global memories
and rules, not for user identity; §9 R1 says so, and non-goal 8 says a project lives in one
workspace's database. Putting the Continuity DDL in `BaseTables` would put a project-identity table
— and a project-identity table whose `ON DELETE CASCADE` would have made it destructible — into the
store whose entire purpose is cross-workspace broadcast.

**The migration-owned path avoids that by construction, and the guard test proves it.** AT7 opens a
hermetic `NewTestSharedDM` (`internal/core/testhelpers.go:175` — a local database plus an ATTACHed
shared database, both in a temp directory) and asserts:

```sql
SELECT COUNT(*) FROM shared.sqlite_master
 WHERE type='table' AND name IN ('projects','project_names','continuity_threads','continuity_checkpoints')
-- must be 0
```

and the same four names must be present in `main.sqlite_master` with a count of 4.

**One code path.** A fresh database gets the four tables the same way an existing one does: the
migration runs in the **fatal** migration transaction on every open, so the fresh-database path and
the migrated-database path are the *same* code. There is no second DDL definition to drift.

Following `internal/core/migration_scheduled_wakes_dispatched_at.go` exactly:

- a `schema_migrations` sentinel, `continuity_project_model_v1`;
- `CREATE TABLE IF NOT EXISTS` for all four tables plus the indexes, so the migration is idempotent;
- called from the **fatal** migration transaction, beside `MigrateWakeResolverFiredBy` and
  `MigrateScheduledWakesDispatchedAt` (`internal/core/db.go:3109-3140`), so a failure rolls the whole
  migration back and `NewDatabaseManager` returns an error;
- **not** in `SafeMigrations`, which is best-effort and **silences errors**
  (`internal/core/db.go:2967-2975`).

**Schema rollback is forbidden.** `ALTER TABLE … DROP COLUMN` is a table-rewrite operation on a live
table with FTS triggers attached, and the delivery-state spike showed SQLite *refuses* the drop
while a dependent index exists. Continuity's tables are forward-only. With `ON DELETE RESTRICT` on
every reference, the schema is additionally **physically** forward-only in the same sense Stage A's
partial index made `scheduled_wakes` forward-only: a physical delete of a project with children is
**refused**, not merely discouraged (§5, T15).

**The updater never opens the database.** `cmd/mpm-update/` contains no `NewDatabaseManager` and no
`sql.Open`, so the migration lands lazily, when the first new-binary process opens the database. The
activation note for the operator: run one `mpm` invocation before starting anything that depends on
the new tables.

### 6.3 The restore allow-list is not optional

`internal/core/sql_dump_validator.go` holds two lists: `CanonicalMPMSchema` (tables **declared in Go
source**) at `:141`, and `RuntimeCanonicalSchema` (tables created by runtime migrations that are not
declared in Go source — FTS virtual tables and transient rename targets) at `:231`. Both are
**security allow-lists** for `mpm restore-db`: a dump declaring a table MPM does not create is
rejected.

**Exactly one list must change: `CanonicalMPMSchema`, which gains four entries** —
`projects`, `project_names`, `continuity_threads`, `continuity_checkpoints`.

`RuntimeCanonicalSchema` is **not** changed. The reason is mechanical, not stylistic:
`TestCanonicalSchemaSync` (`internal/core/sql_dump_canonical_schema_test.go:22-62`) scans every
non-test `*.go` file in `internal/core` for `CREATE TABLE` statements and requires an **exact
two-way match** with `CanonicalMPMSchema` — every table found in source must be listed, and every
listed table must be found in source. The Continuity DDL is a `CREATE TABLE` in Go source (the
migration file), so its four tables are found by the scanner and **must** be in
`CanonicalMPMSchema`. Putting them in `RuntimeCanonicalSchema` as well would be harmless but
redundant, and the instruction "both lists" would invite an implementer into the wrong list.

This is the single most easily-missed compatibility item in the whole design, and it belongs in the
Stage 1 acceptance matrix (AT8).

### 6.4 The complete change surface

| Surface | Change | Stage |
|---|---|---|
| `internal/core/migration_continuity_project_model.go` (new) | the four tables + indexes — **the only DDL definition** | 1 |
| `internal/core/db.go` | one call in the fatal migration transaction | 1 |
| `internal/core/sql_dump_validator.go` | **four** entries in `CanonicalMPMSchema` | 1 |
| `internal/core/continuity_schema_test.go` (new) | the shared-schema guard (AT7) and the migration's negative tests (AT6) | 1 |
| `internal/core/continuity.go` (new) | the model: resolve, create, rename, alias, delete, thread, checkpoint, resume | 2 |
| `internal/core/tools/handlers.go` | `handleMpmContinuity` | 2 |
| `internal/core/tools/registry_list.go` | one registry entry — **and** the `mpm_resolve` description edit, which lists the pointer kinds | 2, 3 |
| `internal/core/tools/registry.go` / `internal/core/core.go` | `CoreDB` gains the continuity methods | 2 |
| `cmd/mpm/router.go` + `cmd/mpm/handlers_continuity.go` (new) | the CLI facade | 3 |
| `internal/core/tools/handlers.go` (`mpm_resolve`) | **three** pointer kinds | 3 |
| `internal/pointer/resolve.go` | the pointer package's resolver switch, if it enumerates kinds | 3 |
| `internal/core/contextual_candidates.go` + `…_sources.go` | the `continuity` candidate kind + source | 4 |
| `internal/core/wake_context.go` | *nothing* in v1 | — |
| `docs/SPEC.md` | the product contract: the new durable entity family and the new command | 1–3 |

**The id shape is a change surface row.** The generated ids change shape from the draft's
`cpj_…`/`ct_…`/`ck_…` to `cpj-…`/`ct-…`/`ck-…`. Nothing outside Continuity observes those ids
today — they do not exist yet — but the shape is a **public contract** the moment `mpm://project/…`
and `mpm://thread/…` ship, so it is recorded here rather than left implicit (§4.4).

**`docs/SPEC.md` is in the change surface because this IS a product-contract change.**
`scripts/docs_check/exemptions.py:291-303` pairs `internal/core/schema.go`, `internal/core/db.go`,
`internal/core/evidence.go` and `internal/core/confidence` with `docs/SPEC.md` and the
confidence/evidence design, so a Stage 1 that touches the schema fires the dependency check. The
delivery-state Stage A discharged the identical item with a `Docs-Impact: none` trailer — **that
trailer is not available here**, because Continuity adds a durable entity family and a user-facing
command. `docs/SPEC.md` owns *"the product contract: behaviour, commands, semantics"*.

---

## 7. Staged implementation sequence

Each stage is independently mergeable, has its own acceptance gate, and names its stop conditions.
The gates below and the acceptance matrix in §8 agree; the original draft's two versions of this
split did not, and that contradiction is resolved here.

### Stage 1 — the additive schema

| | |
|---|---|
| **Prerequisites** | none |
| **Files** | `internal/core/migration_continuity_project_model.go` (new); `internal/core/db.go`; `internal/core/sql_dump_validator.go`; `internal/core/continuity_schema_test.go` (new) |
| **DB invariants** | the **four** tables and their indexes exist; the sentinel is recorded exactly once; **no existing table or row is modified**; the four tables exist in the local schema and **do not exist in the `shared` schema** |
| **Observable behaviour** | **none** |
| **Regression tests** | AT1–AT8 (§8) |
| **Compatibility** | old binaries ignore the new tables; the schema is forward-only; a hard delete of a project with children is refused |
| **Rollback** | binary rollback is safe; schema rollback is forbidden |
| **Stop condition** | stop if any existing row changes, if the restore allow-list is not updated, if any Continuity table appears in the `shared` schema, if `git diff --stat` touches a file outside the four listed, **or if the operator answers OD3 with (β)** — the explicit actor/authorization option — because (β) would need an owner column and possibly a fifth table, and this Stage 1's DDL would then have to be re-issued (§9, OD3; §12.2, D6) |
| **Gate** | AT1–AT8 green; `mpm-lint --gate` PASS; `make docs-check` OK |

### Stage 2 — the model and the tool

| | |
|---|---|
| **Prerequisites** | Stage 1 |
| **Files** | `internal/core/continuity.go` (new); `internal/core/tools/handlers.go`; `internal/core/tools/registry_list.go`; `internal/core/core.go` (`CoreDB`) |
| **DB invariants** | a project resolves through exactly one live name row; the checkpoint ledger is append-only; every thread mutation bumps `revision`; a name is bound to one live project; a checkpoint's project always equals its thread's project |
| **Observable behaviour** | `mpm call mpm_continuity` becomes available with eight actions |
| **Regression tests** | AT9–AT24 (§8) |
| **Compatibility** | the tool is new; nothing existing changes behaviour |
| **Rollback** | fully revertible — no existing surface is touched |
| **Stop condition** | stop if any action can create a duplicate project, if a write can land without a revision check, if a checkpoint can be written under a project other than its thread's, or if a project can be deleted without recording the actor |
| **Gate** | AT9–AT24 green; the existing tool-registry and namespace-contract tests green |

### Stage 3 — the CLI facade and the pointers

| | |
|---|---|
| **Prerequisites** | Stage 2 |
| **Files** | `cmd/mpm/router.go`; `cmd/mpm/handlers_continuity.go` (new); `internal/core/tools/handlers.go` (`mpm_resolve`); `internal/core/tools/registry_list.go` (the `mpm_resolve` description); `internal/pointer/resolve.go` |
| **Observable behaviour** | `mpm continuity <sub>` works; `mpm_resolve` accepts three new URI kinds |
| **Regression tests** | AT25–AT26 (§8) |
| **Rollback** | fully revertible |
| **Stop condition** | stop if `mpm_resolve`'s existing five kinds change behaviour, or if the pointer id charset is widened |
| **Gate** | the pointer and facade tests green |

### Stage 4 — routing-pipeline integration (**conditional**)

| | |
|---|---|
| **Prerequisites** | Stage 2; **and** the operator's decision on where a session→project binding lives (§9, OD1) |
| **Files** | `internal/core/contextual_candidates.go`; `internal/core/contextual_candidates_sources.go`; `internal/core/contextual_selection.go` |
| **DB invariants** | none — the pipeline is observational |
| **Observable behaviour** | a checkpoint becomes a routing candidate with kind `continuity` and a rationale in the existing `continuity` band |
| **Regression tests** | the candidate's source counts and reason tokens; the selection's band and rationale; **no change** to the existing candidates' ordering or counts |
| **Rollback** | fully revertible |
| **Stop condition** | stop if the routing output changes for a session with **no** project — the whole point is that the capability is opt-in and ambient-free |
| **Gate** | the existing contextual-routing suites green **unchanged** |

### Stage 5 — a project purge (deferred)

Deliberately out of scope. A hard purge of a project's threads and checkpoints needs a reason-code
and a backup flow like `work_purge`. It is not in this series. Note that the purge is also what
would release a withdrawn name for reuse, if the operator ever wants that (§2.7).

---

## 8. Acceptance-test matrix

Every test is deterministic. **No test may contain a sleep or an arbitrary wait.** Two mechanisms,
both already in the tree: the **SQLite authorizer barrier**
(`internal/core/wake_resolve_contention_test.go`, which pauses a statement at `PREPARE` time and
therefore observes the arrival explicitly before doing anything else) and **subprocess re-exec with
a named crash point** (the standard Go pattern: the test re-executes itself with an environment
variable naming the crash point, signals the parent over a pipe, and the parent `SIGKILL`s it).

**Every assertion below is an observable invariant** — a row count, a stored value, an error
code, a table list — never "the statement succeeded". A successful statement is not evidence of a
correct state transition; that is the defect class this matrix exists to catch.

### Stage 1 gate — the schema

| # | Scenario | Mechanism | Assertion |
|---|---|---|---|
| **AT1** | **Existing database migration** | a disposable file-backed WAL DB on the pre-Continuity schema, populated with representative rows, then opened by the new binary | the **four** tables — `projects`, `project_names`, `continuity_threads`, `continuity_checkpoints` — and their indexes exist, **by name**; the sentinel count is 1 |
| **AT2** | **Fresh database creation** | a new DB through the same path | the schema is **identical** to AT1's — compared as the **full `sqlite_master.sql` text** of every table and index, **grouped by name, not by row order**. The comparison is grouped by name because **row order is not part of the schema**: the same objects can legitimately be created in a different order for reasons outside the Continuity migration — a migration that recreates a table, an index rebuilt in place, a `CREATE TABLE IF NOT EXISTS` ordering difference between a fresh init and an upgrade — so an order-sensitive comparison is **fragile even though it happens to pass today**. Today the two ordered lists are in fact **identical**: the fatal migration transaction opens *after* every base DDL slice — the substrate states that ordering dependency in its own terms at `internal/core/db.go:3048-3056` — so the Continuity tables land last in **both** paths. §14.2 E28 measures both halves of that. A table-list comparison would not catch a column or index drift, which is why the full `sql` text is compared |
| **AT3** | **Whole-row preservation** | snapshot every pre-existing row of every pre-existing table before and after, as whole-row strings | **byte-identical** — the whole row set, not a sample |
| **AT4** | **Zero fabricated state** | — | no existing table gains a row; no existing row gains a column value; `project_names` starts **empty**; `projects` starts empty |
| **AT5** | **Idempotent migration** | run the migration twice, then a third time | the table list, the index list and the sentinel count are unchanged by runs 2 and 3 |
| **AT6** | **Failed-migration rollback** | **a trigger that aborts the migration's sentinel write** — `CREATE TRIGGER block_continuity_sentinel BEFORE INSERT ON schema_migrations WHEN NEW.id = 'continuity_project_model_v1' BEGIN SELECT RAISE(ABORT, 'injected failure'); END` — installed on a disposable DB, then opened by the new binary | the migration returns an error; `NewDatabaseManager` surfaces it; the **table list is unchanged** (all four DDL statements had already succeeded and must be undone); the **index list is unchanged**; **no sentinel**; the pre-existing row set is byte-identical |
| **AT7** | **The four tables are local only** | a hermetic `NewTestSharedDM` — a local database plus an ATTACHed shared database, both in a temp dir | the four names are present in `main.sqlite_master` with a count of 4, and the count of those names in **`shared.sqlite_master` is 0**; no `shared.projects` table exists |
| **AT8** | **Restore allow-list** | a dump containing the four new tables — `projects`, `project_names`, `continuity_threads`, `continuity_checkpoints` — named explicitly | `mpm restore-db` **accepts** it; `CanonicalMPMSchema` contains **exactly** the four new entries `projects`, `project_names`, `continuity_threads`, `continuity_checkpoints` and **no** others; `RuntimeCanonicalSchema` contains **none** of the four; `TestCanonicalSchemaSync` and `TestRuntimeCanonicalSchemaHasNoPhantomEntries` are green; and the allow-list's size grows by exactly four, so the "four tables" count is observable rather than asserted |

**Note on AT6's mechanism.** The original draft specified "a trigger that aborts the third
`CREATE TABLE`". **That mechanism does not exist**: SQLite has no DDL trigger, and it refuses
triggers on system tables (`CREATE TRIGGER … ON sqlite_master` → *"cannot create trigger on system
table"*; `AFTER CREATE TABLE` → a syntax error). The delivery-state Stage A hit the same wall and
deviated ([PR #225](https://github.com/flowbyte-com/mpm/pull/225), unresolved issue #1): it aborts
the **sentinel write**, which happens *after* the DDL. The rollback that exercises is therefore
**strictly stronger** than the one the draft described — the DDL has already succeeded and must be
undone. AT6 asserts the table list, the index list and the row set, not merely "no sentinel".

### Stage 2 gate — the model and the tool

| # | Scenario | Mechanism | Assertion |
|---|---|---|---|
| **AT9** | **Project resolution** | a project with a canonical name and one alias, plus a second project whose name differs only by case (which the schema must refuse to create) | exact → resolved; alias → resolved; case-insensitive → resolved **with a note**; the **ambiguous branch is unreachable** — the resolver must be able to prove at most one live holder of a name; unknown → **not created**, the live names listed, and the list says *"showing 25 of 47"* when it truncates |
| **AT10** | **No silent duplicate creation** | (a) create twice with the same name; (b) two concurrent creates of the same name, gated by the authorizer barrier; (c) create with a case variant of an existing name | (a) the second is refused and names the existing project; (b) exactly one project exists and the loser is told its id; (c) the case variant is **refused by the database** with a UNIQUE violation, and the stored name keeps its original case |
| **AT11** | **Thread selection** | a project with two active threads, one dormant, one completed, one superseded | `op='select'` by id → that thread; by focus → that thread; by title → that thread; `op='select'` with **no** selector → **refused** (the unqualified fallback belongs to `resume`); `resume` with no selector and two active threads → **ambiguous**, both listed; the completed and superseded threads are never auto-selected |
| **AT12** | **Focus uniqueness is case-insensitive** | two threads opened for `form-builder` and `Form-Builder` | the second is **refused by the database** with a UNIQUE violation; the case-insensitive lookup finds exactly one live thread; the "ambiguous focus" branch is unreachable among live threads; `op='open'` with an already-held focus is refused and the error names the thread that holds it |
| **AT13** | **Optimistic concurrency** | two writers against one thread at the same revision | exactly one write lands; the other receives `stale_thread` with the current revision; **no checkpoint row exists for the loser**; **the loser's generated id was not burned** — a retry with that id succeeds; the winning checkpoint's `revision` is the **pre-increment** value it was written against, and the thread's revision is one higher |
| **AT14** | **Crash at each write boundary** | subprocess re-exec with `MPM_CONTINUITY_CRASH_AT ∈ {after_checkpoint_insert, after_pointer_update, after_commit}` | after insert → the thread still points at the previous checkpoint and **no orphan checkpoint exists**; after the pointer update → the same (the transaction rolled back); after the commit → the new checkpoint is current |
| **AT15** | **Checkpoint budget** | a checkpoint at the limit, then one over it — and one that exceeds the **byte** cap while still inside the count limits | the limit one is accepted; the over one is **rejected** with the section and the limit named, and the rejection says **which limit fired** (bytes, not count); nothing is committed; a malformed `facts` document is **refused by the schema's `CHECK (json_valid(facts))`** |
| **AT16** | **Reference resolution and dangling refs** | a checkpoint whose refs point at a memory, a **decision**, a theory, a lesson and a work item; then shred the memory | every ref resolves with a bounded summary and a pointer **whose kind matches the reference kind** (`mpm://decision/…` for a decision); after the shred, the memory ref is reported `missing` and the checkpoint is still readable |
| **AT17** | **Rename and alias** | rename a project; then resolve by the old name, the new name, and a stale alias; then attempt a rename onto another project's live name and another project's live alias | the old name, the new name and the alias all resolve to the same project id; the old canonical name appears as an alias; **exactly one live canonical name** exists at every point; a rename onto a live name is **refused**, naming the holder; the demote-then-promote order leaves no gap (a concurrent resolve sees the old alias or the new canonical, never neither) |
| **AT18** | **Deletion semantics** | delete a project with `--confirm`; then create a project with the same name; then attempt a hard `DELETE` | the tombstone exists **and names itself** — the withdrawn name rows are retained and report the tombstone's id, actor and time; the old names no longer resolve; the new project gets a **new** id and the response mentions the tombstone; the old threads and checkpoints are retained and reachable only by id; the hard `DELETE` is **refused** by the schema and every row survives; `projects.deleted_by` records the actor |
| **AT19** | **Containment is refused by the schema** | **five** hostile writes: a checkpoint naming another project; a thread pointer naming another thread's checkpoint; a pointer naming a nonexistent checkpoint; a thread supersession naming another project's thread; **a checkpoint supersession naming another thread's checkpoint** | **all five are refused** with a foreign-key violation; the row counts are unchanged after each; the same-project and same-thread equivalents succeed — and the *fourth* hostile write is the supersession's own counterpart: a checkpoint superseding another checkpoint of the **same** thread succeeds |
| **AT20** | **Staleness is visible** | write a checkpoint; then read it back after the session that wrote it has ended | `resume` reports `checkpoint_age_seconds`, `checkpoint_revision`, `thread_revision` and `session_ended_after_checkpoint` with the session's `ended_state`; the values are correct against the fixture's clock |
| **AT21** | **Crash without a session-end event** | write a checkpoint; kill the process; then resolve and resume in a fresh manager | the checkpoint is current and complete; **no handoff exists**; the resume returns the same bytes as before the crash; `session_ended_after_checkpoint` is `false` |
| **AT22** | **Cross-session, cross-agent, cross-model** | write a checkpoint from session 1 with framework F and model M; read it from session 2 with framework G and model N | the checkpoint is byte-identical; the writer's identity is on the row and is **not** part of the brief's content |
| **AT23** | **Existing surfaces unchanged** | the existing suites, run **unchanged** | `internal/core/wake_resolve_contention_test.go`, `internal/core/wake_ownership_test.go`, `internal/scheduler/wake_settlement_test.go`, the tool-registry and namespace-contract tests, and the contextual-routing suites all pass **unchanged** |
| **AT24** | **No ambient overhead** | a session with **no** project | the wake context is **byte-identical** to a pre-Continuity wake context; no new top-level key; no additional query |

### Stage 3 gate — the facade and the pointers

| # | Scenario | Mechanism | Assertion |
|---|---|---|---|
| **AT25** | **The CLI facade** | `mpm continuity <sub>` for every sub-command in §4.3's table | **every** sub-command maps to **exactly one** `mpm_continuity` action with the documented params and the documented `op`; `--select --focus <slug>` and `--open --focus <slug>` reach **different** actions; the exit codes are the facade's documented ones; an unknown sub-command fails without touching the database |
| **AT26** | **The pointer kinds** | `mpm_resolve` against `mpm://project/…`, `mpm://thread/…`, `mpm://decision/…` | each resolves; `mpm://decision/<a-memory-id>` **rejects** an id whose collection is not `decisions`; the **existing five kinds are unchanged** — same projection, same bounds, same errors; the id charset is **not widened** (`mpm://project/cpj_…` with an underscore is still rejected) |

---

## 9. Unresolved operator decisions

### 9.1 The independent review's OD1–OD6

| # | Decision | Recommendation | Status | Blocks |
|---|---|---|---|---|
| **OD1** | Where does the **session→project binding** live? (the draft's R3, re-scoped) | A **DB-backed binding written by an explicit action**, read by the Stage 4 source. `active.json` stays out (its write is not atomic — `os.WriteFile`, not the tmp+rename swap). The cost is **one more table**, and it is **not** part of the four-table Stage 1 — it is a separate, later table. | **open** | **The operator's requirement, not merely Stage 4.** As specified, a project is reachable by name and by explicit call, and by nothing else — §3.7 says so plainly |
| **OD2** | Is an unbounded, invisible **loss window** acceptable? (the draft's R2, re-scoped) | **No writer** — an automatic writer must read the session transcript, and a transcript is the one thing that must not become durable state. But `resume` **must** report `checkpoint_age` and whether a session ended after the checkpoint. **The limitations of the retained handoff history are explicit:** `session_handoffs` has a **90-day TTL** (`PruneHandoffs(90)`), so the session-end signal disappears after 90 days; and a session killed without running `EndSessionV2` leaves **no handoff row at all**, so the absence of a handoff is **not** proof of continuity. The signal is best-effort and the brief's age is the durable half | **open** | The **crash-recovery claim** — the *visibility* of the loss, not its absence. Does **not** block Stage 1 |
| **OD3** | **Who may delete a project, and is it recorded?** | **The minimum viable single-operator contract, plus auditability.** Compare: (α) `--confirm <project-id>` as a naming check — the review is right that this is **not** authorization: `projects` has no owner, `resolve` hands out every live project's id and name, so any agent in the workspace can delete any project; (β) an explicit actor/authorization mechanism — the substrate has **no identity primitive** to build it on (`actor_kind` distinguishes agent/human/unknown, not *who*), so (β) is new machinery, not a correction. **Recommended:** (α) with `projects.created_by` / `deleted_by` and a `system_audit_log` row — the writer identity and the audit, **no fifth table** — and the workspace declared a **single-operator trust domain** with the residual risk stated rather than implied | **open — needs the operator's acknowledgement** | **Stage 2's delete action, and — conditionally — Stage 1's DDL.** Under the recommended (α), the Stage 1 *columns* are unaffected: `created_by` and `deleted_by` are already in the `projects` table and the audit row goes to the existing `system_audit_log`, so Stage 1's DDL needs no change. Under (β) they are **not** unaffected — an actor model needs an owner column and possibly a fifth table, so **Stage 1's DDL would change** and the §4.1 block, AT1, AT2 and the restore allow-list would all have to be re-issued. The claim is therefore *conditional on the operator's answer*, and this is the one outstanding decision that touches Stage 1 |
| **OD4** | Which **slice** holds the four tables? | **Resolved by this revision: none of the schema slices.** The DDL is owned by the **migration** (§6.2), which is neither `BaseTables` (propagated to the shared schema) nor `WorkTables` (the dual-definition `work_events` shape Stage A declined). One code path, local only, with AT7 as the guard | **resolved** | — |
| **OD5** | Is a **`decision` reference pointerable**? | **Yes — add `mpm://decision/<id>` as a third pointer kind**, with the substrate's own `mpm://theory/` shape: `GetMemory` plus a `collection == 'decisions'` assertion. **Compatibility implications:** `mpm_resolve` gains a kind (the five existing kinds are unchanged); a decision becomes reachable under **two** URIs — `mpm://memory/<id>` already resolves it today, because `GetMemory` selects by id without a collection filter, and a theory already has this same duality — so the new kind adds a **typed** address, not a new address space; the resolver can now *reject* an id that is not a decision. The alternative — dropping `decision` from the checkpoint reference vocabulary — would cost the `facts` section its ability to distinguish a decision from a memory, and §2.3's whole point is that the sections are typed | **open — recommended; does not block Stage 1** | Stage 3's pointer surface. Stage 1 is unaffected either way |
| **OD6** | Is case-insensitive uniqueness enforced in **storage** or only in the API? | **Storage.** `COLLATE NOCASE` on `project_names.name` and on `continuity_threads.focus`, plus partial unique indexes over the live rows (§4.1). The API check stays as the friendly first line | **resolved** | — |

### 9.2 The remaining open decisions

These are the original draft's open decisions, numbered **R1–R10**. The **R** prefix is
deliberate: the revision-2 review's findings use **D1–D12**, and keeping the two
vocabularies apart is what stops a reviewer's `D2` and a draft's `D2` from being read as
the same thing. The numbers are unchanged, so `R4` is the draft's `D4`.

| # | Decision | Recommendation | Status | Blocks |
|---|---|---|---|---|
| **R4** | How long are **checkpoints** retained? | The project survives indefinitely; the checkpoint ledger grows. Recommend keeping the current checkpoint plus the last N superseded ones per thread, and pruning the rest. | open | nothing yet — a pruning stage, if any, comes after the model |
| **R6** | Should a **checkpoint be a first-class provenance artifact**? | **Deferred.** It requires rebuilding `artifact_provenance`, and the checkpoint row already carries the writer's identity. | open | nothing |
| **R7** | Should the **`focus` slug** be mandatory on a thread? | **Optional in v1.** A thread without a focus is selectable by id and title only. The partial unique index excludes `focus IS NULL`, so focus-less threads may coexist. | open | nothing |
| **R8** | Should the **CLI facade** be `mpm continuity` or `mpm project`? | `mpm continuity`, because the tool covers both the project identity and the thread/checkpoint family. | open | Stage 3's naming |
| **R9** | Should `mpm_continuity` appear on the **MCP compact surface**? | **No in v1.** The compact surface is deliberately minimal (`read_wake_context`, `write_handoff`, …). | open | nothing |
| **R10** | Should a project be able to hold a **repository binding**? | **Deferred**, and the deferral carries its real prerequisite: MPM has **no repository identity primitive** (§1.3). "Add a repository binding" is not a small addition; it means first building an explicit, operator-supplied repository key. When cross-workspace identity comes, the shared database's role is a **name→id directory only** — never the storage for threads or checkpoints (§13, P3) | open | nothing |

### 9.3 Decisions this revision closes

The draft's **R1** (workspace- vs cross-workspace-scoped) is closed as **workspace-scoped** and is
now enforced structurally: the four tables exist only in the local schema (§6.2, AT7), so a
cross-workspace project is not merely unresolvable — it does not exist in the shared store.

The draft's **R2** (explicit-only checkpoints) is closed as **explicit-only**, with the visibility
condition of **OD2** attached.

The draft's **R3** (session→project binding) is **re-scoped to OD1** and is explicitly *not* part
of the four-table Stage 1.

The draft's **R5** (whether the case-insensitive match tier is wanted) is closed as **yes, in
storage** — **OD6**.

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
   deferred). No automatic deletion of a thread. **No cascading delete that a user did not ask for** —
   and, with `ON DELETE RESTRICT` on every reference, no cascade the user *can* ask for by accident
   either.
7. **Not automatic.** Nothing is written without an explicit call. No fold, no background writer, no
   implicit checkpoint on session end.
8. **Not cross-workspace.** A project lives in one workspace's database (R1/OD4). The shared schema
   is the cross-workspace **broadcast** store, and it holds no project identity (§6.2).
9. **Not an authorization system.** The deletion confirmation is a typo guard. The workspace is a
   single-operator trust domain (§2.7, §9 OD3).

---

## 11. Recommendation

**STAGE 1 ARCHITECTURE INDEPENDENTLY APPROVED** — the additive schema.

This is *not* an authorisation to implement. It is a statement that the specification's guarantees
now agree with the actual SQL, the actual codebase and the actual test plan, that the **sixteen**
confirmed defects of the first review (A1–A16) and the **six** of the second (D1–D6) have been
corrected, and that the second review's optional refinements (D7–D12) have been taken. The **final
independent delta review** has since been carried out and **APPROVED** the Stage 1 architecture
([comment 6656558202](https://github.com/flowbyte-com/mpm/pull/224#issuecomment-6656558202): *"The
Stage 1 architecture is technically ready"*), raising two non-blocking documentation findings, which
are corrected here (§12.4, F1–F2). The approval covers the **architecture**, not the operator's
open decision: Stage 1 is not thereby authorised for implementation, and remains subject to the
operator's answer on OD3 (§9) (§12, §13, §14).

**The one conditional.** Stage 1's DDL is final **if the operator accepts OD3's recommended option
(α)** — the delete confirmation as a naming check plus `created_by` / `deleted_by` and a
`system_audit_log` row, with no fifth table. Under (α) the Stage 1 columns are already right.
Under (β), the explicit actor/authorization option, Stage 1 would need an owner column and possibly
a fifth table, and §4.1, AT1, AT2 and the restore allow-list would all have to be re-issued. The
document does **not** assume the operator's answer; it states the dependency, and Stage 1's stop
condition names it (§7, §9 OD3, §12.2 D6).

The design satisfies the product contract with **four new tables, one new tool, one CLI facade,
three new pointer kinds, and no change to any existing reader, writer, predicate or public
contract.**

The load-bearing decisions, and the evidence for each:

| Decision | Evidence |
|---|---|
| A **new data model** is required | §1.3 — the substrate has no project axis, exhaustively verified; and no existing entity has the required durability (§1.2) |
| **Names + aliases in one table with a live-name unique index** | the index is what makes "no silent duplicate" a database guarantee rather than a convention (§2.5, T4) — and, because it is a *partial* index, it also keeps the withdrawn history (§2.7) |
| **Exact-or-case-insensitive matching, never fuzzy** | the substrate's own rule: *"unavailable, not inferred"* (§1.5); the ambiguity branch is *unreachable*, and that is asserted (AT9) |
| **A `focus` slug per thread** | the contract's *"the form builder"* is a recurring label, not a title; without it, resolution would need substring matching (§2.4) |
| **A `revision` counter per thread** | the inventory found **no version-based CAS** anywhere in the tree; the state-predicate idiom cannot fence a checkpoint pointer (§2.8) |
| **References, not copies, in four sections** | the brief's explicit requirement; the vocabulary is reused from `evidence.artifact_type`, and the pointer kinds are made to match it (§2.3) |
| **Explicit pull, no ambient state** | *"Minimal context and token overhead"*; and `active.json`'s write is not atomic (§4.6) |
| **`artifact_provenance` not widened** | widening a CHECK requires a table rebuild; the benefit is telemetry the checkpoint row already carries (§4.5) |
| **A project is not a work item** | `works` is purged and has a task lifecycle; the two vocabularies must not be conflated (§2.1) |
| **One DDL definition, owned by the migration** | the same shape the delivery-state Stage A chose and recorded (§6.1); it also keeps the four tables out of the shared schema (§6.2) |
| **Containment enforced by the schema, not by convention** | **four** composite foreign keys — `(thread_id, project_id)`, `(last_checkpoint_id, id)`, `(superseded_by, project_id)`, `(supersedes, thread_id)` — make a cross-project checkpoint, a cross-thread or cross-project pointer, and a cross-project supersession *unrepresentable* (§4.1, T13, D1) |

**What this design does not claim.** It does not claim that a checkpoint is *true*. A checkpoint
records what an agent asserted, bounded and pointed at durable artifacts; its integrity rests on
those artifacts, which carry their own evidence and confidence. It does not claim exactly-once
delivery of a brief to a thread — two agents can both write, and the revision makes the loser
visible rather than silent. It does not claim that the work between the last checkpoint and a crash
was captured — only that the age of the brief is visible (§9, OD2). And it does not make the
project axis universal: until OD1 is answered and Stage 4 is built, a project is reachable by name
and by explicit call, and by nothing else.

---

## 12. The review dispositions — A1–A16 and D1–D6

### 12.1 A1–A16 — the first independent review

Every finding of the [first independent review](https://github.com/flowbyte-com/mpm/pull/224#issuecomment-6652203614) (verdict *ACCEPT WITH REQUIRED CORRECTIONS*; sixteen confirmed defects A1–A16), and where this revision answers it. The second review verified all sixteen against the tree and the SQL rather than against this table, and found **fifteen resolved and verified** and one — **A13** — resolved in principle but inadequately specified; A13's remaining conditionality is now stated in **OD3** (§12.2, D6).

| # | Finding | Corrected location | Resolution | Verification evidence | Remaining risk |
|---|---|---|---|---|---|
| **A1** | The table count is wrong — it is **four**, not three | §2.2, §6.1, §6.3, §6.4, §7 Stage 1, §8 AT1/AT8, §11 | "three" → "four" in all nine places. The document now says **four tables, three entities**: `project_names` is the resolution *domain*, not an entity. AT1 and AT8 enumerate the four names by name | the review's own reproduction: the restore validator accepts none of the four today. AT8 is the gate | the count is load-bearing for the allow-list, not for the model — a future fifth table (OD1's binding) is deliberately *outside* Stage 1 |
| **A2** | The `cpj_`/`ct_`/`ck_` prefixes are **rejected by the pointer parser** | §1.4, §3.1–§3.3, §4.1, §4.4, §6.4 | the ids use a **hyphen**: `cpj-`, `ct-`, `ck-`. The pointer package is **not** modified; the id charset is **not** widened | the review's citation of `internal/pointer/pointer.go:17` and `internal/core/tools/handlers.go:7857-7862` was re-checked; §14 asserts the charset against the substrate's own regex — `cpj-…` matches, `cpj_…` does not, and `core.GenerateID()`'s 16 bare hex matches | none. The ids are new, so nothing observes the old shape |
| **A3** | `project_names.name` is case-sensitive, so "no silent duplicate" is not a database guarantee | §2.5, §4.1 | `name TEXT NOT NULL COLLATE NOCASE` plus a **partial unique index over live rows**. The case-insensitive tier and the exact tier collapse into one index lookup, and the **ambiguous branch becomes unreachable** — which is now asserted rather than assumed | §14: the case-variant INSERT is refused with a UNIQUE violation, the stored name keeps its case, and a case-insensitive lookup finds exactly one row | none for the storage. The *API* still checks first, so a friendly message precedes the constraint |
| **A4** | The `focus` unique index has the same hole | §2.4, §4.1 | `focus TEXT COLLATE NOCASE` — the **column** carries the collation, so the uniqueness index and every resolution query are case-insensitive without a per-query `COLLATE` clause | §14: the case-variant focus is refused; the case-insensitive lookup finds exactly one live thread | the column-level collation also makes `focus = 'X'` case-insensitive **everywhere**, including in queries an implementer writes later. That is the intent |
| **A5** | `ON DELETE CASCADE` contradicts §2.7 and non-goal 6, and it really deletes the ledger | §2.7, §4.1, §5 (T8, T15), §10 | `ON DELETE RESTRICT` on all four ownership references. The soft-delete path never issues a `DELETE`, so the destructive path is **refused by the schema**, not merely discouraged | §14: `DELETE FROM projects` raises a foreign-key violation and names/threads/checkpoints all survive | the schema is now physically forward-only: a legitimate purge (Stage 5) must be written explicitly, and it will have to choose its own mechanism |
| **A6** | `continuity_checkpoints.project_id` is unconstrained | §2.3, §4.1, §5 (T13), §8 (AT19) | a **composite foreign key** `(thread_id, project_id) REFERENCES continuity_threads(id, project_id)`, backed by a unique index on the parent key, plus a second `projects(id)` reference. The denormalisation is now kept honest by the schema, and the document says so | §14: the cross-project checkpoint is refused with a foreign-key violation; the same-project insert lands | the checkpoint's `project_id` remains denormalised — it is now redundant by construction, and a future Stage could derive it by join instead. Kept for the resume path's index |
| **A7** | `last_checkpoint_id` has no foreign key | §2.3, §4.1, §5 (T13), §8 (AT19) | `FOREIGN KEY (last_checkpoint_id, id) REFERENCES continuity_checkpoints(id, thread_id)` — the pointer must name an **existing** checkpoint **of that same thread** | §14: the nonexistent pointer is refused, the cross-thread pointer is refused, and the production write order (insert the checkpoint, then advance the pointer) still works | none. The composite FK is stricter than the review's single-column proposal, which is what the mission asks for ("no invalid **or cross-thread** `last_checkpoint_id`") |
| **A8** | `superseded_by` can point at a thread in another project | §2.4, §4.1, §5 (T13), §8 (AT19) | `FOREIGN KEY (superseded_by, project_id) REFERENCES continuity_threads(id, project_id)` | §14: the cross-project supersession is refused; a same-project supersession lands | none. `ON DELETE SET NULL` was **not** usable here — it would null the NOT NULL `project_id` — so the reference is `RESTRICT`, which is safe because threads are never hard-deleted |
| **A9** | The tombstone cannot name itself | §2.5, §2.7, §4.1 | the name rows are **withdrawn**, not deleted (`withdrawn_at`), and the live-name uniqueness is a **partial index over live rows**. The tombstone therefore names itself, the history is recoverable, **and** the name is free again — the three-way tension the draft resolved silently is now resolved explicitly | §14: both historical names are retained with the deletion actor; the withdrawn name no longer resolves; a successor project takes the live name while the tombstone keeps its history | the withdrawn row is retained forever unless Stage 5 purges it. That is a growth cost, and it is the price of "the name is free again" + "the history is recoverable" |
| **A10** | The DDL cannot express "exactly one canonical name per project" | §2.6, §4.1 | `CREATE UNIQUE INDEX … ON project_names(project_id) WHERE kind='canonical' AND withdrawn_at IS NULL` | §14: a second canonical name is refused; a withdrawn canonical row releases the slot; the demote-then-promote rename order leaves exactly one live canonical | the index is evaluated **per statement**, so the rename's statement order is load-bearing. §2.6 and AT17 pin it |
| **A11** | AT6's mechanism is mechanically impossible | §8 (AT6) | AT6 injects a failure by aborting the migration's **sentinel write** — the delivery-state Stage A's own deviation ([PR #225](https://github.com/flowbyte-com/mpm/pull/225), unresolved issue #1). The DDL has already succeeded when the abort fires, so the rollback exercised is **strictly stronger** | the review's reproduction: `CREATE TRIGGER … ON sqlite_master` → *"cannot create trigger on system table"*; `AFTER CREATE TABLE` → syntax error. AT6 asserts the table list, the index list **and** the row set, not merely "no sentinel" | none |
| **A12** | Putting the DDL in `internal/core/schema.go` replicates project identity into the **shared database** | §1.4, §4.1, §4.6, §6.2, §8 (AT7), §10 | the DDL is owned by the **migration**, not by any schema slice. `attachShared` propagates only `BaseTables` (`internal/core/db.go:2132` and `:2160`), so a migration-owned table never reaches the shared schema. **AT7** opens a hermetic `NewTestSharedDM` and asserts the four names are present in `main` and absent from `shared` | the review's reproduction: appending the DDL to `BaseTables` created all four tables in the shared schema, with `shared.project_names → shared.projects ON DELETE CASCADE`. The `attachShared` loops were re-read at `internal/core/db.go:2094-2175` | a future author who adds the tables to `BaseTables` for convenience would silently reintroduce it. AT7 is the standing guard, and it is a Stage 1 gate |
| **A13** | `--confirm <project-id>` is a naming check, not authorization | §2.7, §9 (OD3), §10 (non-goal 9) | the document now says plainly that the confirmation is a **typo guard** and **not** authorization, and that **any agent in the workspace can delete any project**. The mitigation is auditability: `projects.created_by` / `deleted_by`, plus a `system_audit_log` row — **no fifth table**. The two options are compared in OD3 and **the operator's acknowledgement is required** | the review's finding — `projects` has no owner column and `resolve` hands out every live project's id and name — was re-checked and is accurate | **the residual risk is real and is stated, not mitigated**: the workspace is a single-operator trust domain. A real actor/authorization model needs an identity primitive MPM does not have |
| **A14** | §6.1 and §6.2 contradict the precedent they invoke | §6.1, §6.2 | the document adopts **Stage A's shape**: the base DDL is deliberately not changed, the four tables are created by the migration, and a fresh database gets them through **the same code path**. There is **one** DDL definition, so there is no drift to guard against — and AT2 compares the **full `sqlite_master.sql` text**, not a table list | the review's citation of PR #225's unresolved issue #3 was re-read and is accurate: *"adding these to the base DDL would create a second definition of them and a drift risk"* | none. The cost is that a reader of `internal/core/schema.go` will not see the tables — which is the trade Stage A made and documented |
| **A15** | A `decision` reference has no pointer | §1.4, §2.3, §4.4, §9 (OD5), §8 (AT16, AT26) | **add `mpm://decision/<id>` as a third pointer kind**, with the substrate's own `mpm://theory/` shape (`GetMemory` + a `collection == 'decisions'` assertion). The reference vocabulary and the pointer vocabulary are now the same set | **one correction to the review's finding:** a decision is a `memories` row with `collection='decisions'` (`internal/core/citation.go:121`, `internal/core/epistemology_tools.go:501`), and `GetMemory` selects by id without a collection filter (`internal/core/web_db.go:258`) — so `mpm://memory/<decision-id>` **does** resolve the decision today. The defect is therefore not "the pointer cannot be followed" but "the pointer kind does not carry the reference kind, so the resolver cannot assert it". The `theory` branch (`internal/core/tools/handlers.go:7660-7668`) is the precedent | a decision is now reachable under two URIs (`mpm://memory/…` and `mpm://decision/…`) — exactly as a theory already is. The new kind adds a **typed** address, not a new address space. Stated in OD5 |
| **A16** | The change surface omits `docs/SPEC.md`, and the allow-list instruction is wrong about which list | §1.4, §6.1, §6.3, §6.4, §8 (AT8) | `CanonicalMPMSchema` gains **four** entries; `RuntimeCanonicalSchema` is **unchanged**. `docs/SPEC.md` is added to the change surface, and the document states why the `Docs-Impact: none` trailer is not available here | `TestCanonicalSchemaSync` (`internal/core/sql_dump_canonical_schema_test.go:22-62`) scans every non-test `*.go` in `internal/core` and requires an **exact two-way match** with `CanonicalMPMSchema`; the Continuity DDL is a `CREATE TABLE` in Go source, so the four names are found by the scanner and must be listed. The `docs_check` dependency pairing (`scripts/docs_check/exemptions.py:291-303`) was re-read | none |

### 12.2 D1–D6 — the revision-2 independent review

The second independent review ([comment 6655135875](https://github.com/flowbyte-com/mpm/pull/224#issuecomment-6655135875),
verdict *ACCEPT WITH REQUIRED CORRECTIONS*) verified A1–A16 as **15 resolved and verified, 1 (A13) resolved
in principle but inadequately specified**, and raised six remaining defects. All six are corrected
here. The **D** prefix is the review's; the original draft's open decisions are **R1–R10** (§9.2), so
the two vocabularies cannot be confused.

| # | Finding | Corrected location | Resolution | Verification evidence | Remaining risk |
|---|---|---|---|---|---|
| **D1** | **[medium · required]** The checkpoint's `supersedes` pointer had no thread or project containment — it was a single-column FK, so a cross-thread *and* a cross-project supersession were both representable, and T13's stated mechanism did not cover it | §4.1 (the DDL and its notes), §5 (T13), §8 (AT19), §3.3, §11 | `supersedes` is now a **composite** foreign key: `FOREIGN KEY (supersedes, thread_id) REFERENCES continuity_checkpoints(id, thread_id) ON DELETE RESTRICT`. The referenced unique key `idx_continuity_checkpoints_id_thread ON continuity_checkpoints(id, thread_id)` already existed — it was the parent key the `last_checkpoint_id` FK needed — so **no new index is required**. The composite closes the cross-thread case *and* the cross-project case, because the referenced checkpoint must carry the same `thread_id` and therefore the same project | §14.2 E18–E23: the same-thread supersession is accepted; the cross-thread, cross-project and nonexistent-id supersessions are all **refused** with a foreign-key violation; an `UPDATE` that repoints `supersedes` across threads is refused; no row survives a refused write; the parent index exists and is over `(id, thread_id)` | `ON DELETE SET NULL` is unusable on a composite FK — SQLite applies the action to **every** column of the child key, so it would try to null the `NOT NULL` `thread_id` and refuse the delete with a misleading `NOT NULL constraint failed`. `RESTRICT` refuses the same delete with the correct error, and refusing it is right: the checkpoint ledger is append-only (§2.7). The cost is that a superseded brief can no longer be deleted at all — which is what "append-only" means, and it is now enforced by the schema rather than only by policy |
| **D2** | **[low]** §11 miscounted the composite foreign keys — it said "four", the schema declared three composite plus four single-column, and T13 enumerated only the three | §4.1's notes, §5 (T13), §11 | the counts are now stated once and consistently: **seven foreign-key clauses — four composite, three single-column**. The composite four are `(superseded_by, project_id)`, `(last_checkpoint_id, id)`, `(thread_id, project_id)` and `(supersedes, thread_id)`; the single-column three are `project_names.project_id`, `continuity_threads.project_id` and `continuity_checkpoints.project_id`. T13 enumerates all four, and §11's row now names them | the count is read off the DDL itself; §14.2's probe executes the DDL and the FK behaviour is asserted case by case | none. Note the coincidence worth stating plainly: §11's "four" was *right for the wrong reason* — it counted the four single-column clauses' partners rather than the composite ones. After D1 there really are four composite clauses, and the claim they support is now true |
| **D3** | **[low]** §3.2's worked example violated the revision's own id rule — its four pointers carried underscore ids (`d_91…`, `l_04…`, `m_55…`, `w_12…`) that the substrate's own parser rejects, and the ellipsis character is not in the pointer charset either | §3 in full, §4.1's id note, §14.2 | every example id in the document is now a **real id shape**: the three Continuity ids are their prefix plus the 16 lowercase hex characters `core.GenerateID()` returns, and the four artifact ids are the substrate's own 16 bare hex. The DDL's id comments were changed from "32 lowercase hex" to **"16 lowercase hex"** to match, so the DDL and the examples now describe one scheme. §3 also states what is stored versus what is returned (see D8) | §14.2 E24–E27: the probe **transcribes the substrate's real inline `parsePointerURI`** (`internal/core/tools/handlers.go:7827-7864`) and runs it over **every `mpm://` URI in the document**; the four literal pointers all parse, the one pointer shown as rejected really is rejected, and every literal id matches the generated-id shape it claims | none for the examples. The **real** lesson is that a hand-written example can drift from the rule it illustrates, so the check is now mechanical rather than editorial: a pointer that does not parse fails the probe |
| **D4** | **[low]** The citation audit's headline numbers did not hold for the pointer surface — four load-bearing citations were stale, and §14.4's narrative "corrected" one of them **in the wrong direction** | §1.4, §4.4, §14.1, §14.4 | the four are corrected, in `internal/core/tools/handlers.go` and `internal/core/tools/registry_list.go`: the kind check → **line 7562**; the theory branch → **lines 7660–7668**; the inline id-charset copy → **lines 7857–7862**; the `mpm_resolve` registry description → **lines 759–762**. The stale values are written as bare line numbers so that none of them is a copyable `path:line` citation §14.4's narrative is rewritten to state the **root cause** — the previous revision's audit ran against a tree **20 commits behind** the declared base, so it validated the citations against the wrong files | §14.4's audits now run **against the declared base `f8d4b95a`**, not against whatever tree happens to be checked out, and the instruments were strengthened: a **declaration-anchored** audit (50 / 50 citations point at the symbol's *declaration*, not merely at a line containing it) and a **generic drift screen** over every citation | the root cause was a *process* defect, not a clerical one: an audit that runs against an arbitrary checkout validates nothing. The audits now take the base as a parameter and refuse to run against a tree that is not it |
| **D5** | **[low]** §2.2 over-claimed AT8 — it said the four table names are "enumerated by name in AT1 and AT8", but AT8 said only "a dump containing the four new tables" | §2.2, §8 (AT8) | AT8 now **names all four** — `projects`, `project_names`, `continuity_threads`, `continuity_checkpoints` — in its mechanism *and* in its assertion, and it additionally asserts that the allow-list grows by **exactly four** and that `RuntimeCanonicalSchema` contains **none** of the four. §2.2's claim is now true of AT8 and says so explicitly | the claim is checkable against the matrix rather than against the prose | none |
| **D6** | **[low]** OD3's "the Stage 1 *columns* are unaffected by the answer" was **conditional** — it holds only under the recommended (α); option (β) would need an owner column and possibly a fifth table, so Stage 1's DDL would change | §9 (OD3), §12.1 (A13), §7 (Stage 1's stop condition) | the claim now reads **"under the recommended (α) … unaffected"**, and OD3's *Blocks* column says that (β) would change **Stage 1's DDL** and require §4.1, AT1, AT2 and the restore allow-list to be re-issued. Stage 1's stop condition now names it: if the operator chooses (β), Stage 1 is **not** ready to implement until the schema is re-issued | the operator's answer is still **not assumed**; the document states the dependency rather than resolving it | **the one outstanding decision that touches Stage 1.** It does not block Stage 1 *if* (α) is accepted; it does block it if (β) is chosen. No operator decision is invented here |

### 12.3 The revision-2 review's optional refinements (D7–D12)

The review recorded six refinements as **optional, not required for Stage 1**. They are taken here
because each is a one-line specification change that removes a real ambiguity, and none of them
alters the architecture. The DDL-affecting one (D7) is called out separately.

| # | Refinement | Taken? | Where |
|---|---|---|---|
| **D7** | The `JSON` columns have **NUMERIC affinity**, and `json_valid` does not mean "array" — `'42'` is silently coerced to the integer 42 and `'1.5'` to the real 1.5 | **yes — a DDL change** | §4.1: the four columns now carry `CHECK (json_valid(x) AND json_type(x) = 'array'))`. §14.2 E17: `'{}'`, `'42'`, `'1.5'`, `'"str"'` and `'null'` are all **refused** on every one of the four columns; an array is stored as TEXT and is never coerced |
| **D8** | The **stored reference shape** was unspecified — which is where the residual type confusion lives: whether the pointer is stored or derived was never stated, and a `memory` reference naming a decision's id resolves and presents a decision as a memory | **yes** | §2.3 now states the stored shape exactly: `{kind, id, note?}`, with the `pointer` **derived, never stored**. §4.2's `checkpoint` write parameters carry the same contract. `resume` **asserts the reference kind against the artifact's collection** and reports a mismatch as `kind_mismatch`; the one direction that stays open (a `memory` reference naming a decision) is **bounded and reported** — the resolved artifact's own collection is returned alongside it. §3.2 shows what is stored versus what is returned, and the `state` entry's `summary`/`status`/`verification` are read **live from the work row**, not copied into the checkpoint |
| **D9** | The **checkpoint id-generation strategy** was unspecified, while T2 borrowed the `evidence` pattern's `ON CONFLICT(id) DO NOTHING` rationale — which only bites for a *content-derived* id | **yes** | §5's T2 now states that the id is the substrate's own `core.GenerateID()` — 16 lowercase hex, **not** content-derived — so the `ON CONFLICT` clause is a **belt-and-braces safety net that in practice never fires**, and the restart-safety actually comes from the transaction rollback. The clause is kept because it is free and because a future content-derived id would need it |
| **D10** | The `stale` block's **join column** was unnamed and its **multi-handoff rule** was ambiguous — `session_handoffs.session_id` is the legacy UNIQUE column, and `mpm_session_id` is not unique, so "the session that wrote the checkpoint" can match several handoffs | **yes** | §4.2 now names the join column (`mpm_session_id`, the canonical identity — `handoffColumnMPMSession`, `internal/core/handoff.go:420`; the legacy `session_id` is optional correlation metadata) and the selection rule: **the same one the substrate already uses** for its own previous-handoff lookup — `ORDER BY ended_at DESC, rowid DESC LIMIT 1` (`internal/core/handoff.go:468-469`) — so `session_end_state` names exactly one row, deterministically |
| **D11** | **AT2** should compare by **name**, not by row order | **yes — with a corrected rationale** | §8's AT2 now says "**grouped by name, not by row order**", and explains why: row order is not part of the schema, so an order-sensitive comparison is **fragile even though it happens to pass today**. The original rationale — that the fresh path creates the base tables first and the migration's four last, so the *ordered* `sqlite_master` list differs — was **wrong**: the fatal migration transaction opens after every base DDL slice, so the Continuity tables land last in **both** paths and the two ordered lists are identical today (§14.2 E28). The requirement is unchanged; only the rationale is corrected |
| **D12** | §2.7 should say that the foreign-key guarantee is **DSN-borne** — `SqliteWriteDSN` carries `_foreign_keys=1`, and `InitSchema` deliberately does not enable them itself | **yes** | §2.7 now says so, and records the consequence: a connection opened without that DSN parameter would not enforce the containment guarantees, because SQLite defaults foreign keys **off** — which is why the acceptance matrix must open its databases the way production does |

**One is deliberately not taken.** The review's §4 observation — that a `memory` reference can name a
decision's id and resolve untyped — is **partly** a specification gap and partly an inherited
property of the substrate's own `mpm://memory/`. D8 closes the specification half: the stored shape
is fixed, the pointer is derived, and the typed kinds assert their collection. The inherited half is
not closed by this document, because closing it would mean narrowing the substrate's existing
`mpm://memory/` semantics — a change to an existing public contract, which is out of scope here and is
recorded in §9 as a decision rather than made silently.

### 12.4 The final delta review — two documentation findings

The final independent delta review ([comment 6656558202](https://github.com/flowbyte-com/mpm/pull/224#issuecomment-6656558202))
**APPROVED** the Stage 1 architecture: *"The Stage 1 architecture is technically ready."* It
verified all eight of its priorities against the tree and the SQL rather than against this document's
disposition tables, and raised **two** remaining findings — both low severity, both documentation-only,
and neither blocking Stage 1. Both are corrected here. The **F** prefix is the final review's; the
first review's findings are A1–A16 and the second's are D1–D12 (§9.2).

| # | Finding | Corrected location | Resolution and evidence |
|---|---|---|---|
| **F1** | **[low]** §2.3 stated that a `memory` reference "admits a decision, a theory or a **lesson** id". A lesson is **not** a `memories` row, so `mpm://memory/` does not resolve one, and `GetMemory` cannot retrieve a row from `lessons_base` | §2.3 | the claim is corrected: a `memory` reference admits a **decision** or a **theory** id. The paragraph now says explicitly that a lesson is a `lessons_base` row, that `GetLesson` reads `FROM lessons_base` (`internal/core/db.go:5966-5972`) while `GetMemory` reads `FROM memories` (`internal/core/web_db.go:258`), that a lesson reference naming one would fail to resolve rather than resolve untyped, and that a lesson has its own kind (`mpm://lesson/<id>`) and its own read path. **The accurate decision/theory behaviour is unchanged.** Measured on a hermetic database: a lesson id has **0** rows in `memories` and **1** in `lessons_base`; `GetMemory(<lesson-id>)` → *memory not found*; `GetLesson(<lesson-id>)` → OK; `GetMemory(<decision-id>)` → OK, `collection="decisions"`; `GetMemory(<theory-id>)` → OK, `collection="theories"` |
| **F2** | **[low]** AT2's **rationale** for comparing by name was wrong: it said the fresh path creates the base tables first and the migration's four last, "so the *ordered* `sqlite_master` list differs even when the schemas are identical" | §8 (AT2), §12.3 (D11's row) | the **requirement is unchanged** — the schema is still compared as the full `sqlite_master.sql` text of every table and index, **grouped by name** — and only the rationale is corrected: row order is not part of the schema, so the same objects can legitimately be created in a different order for reasons outside the Continuity migration (a migration that recreates a table, an index rebuilt in place, a `CREATE TABLE IF NOT EXISTS` ordering difference between a fresh init and an upgrade), and an order-sensitive comparison is therefore **fragile even though it happens to pass today**. The original assertion is corrected too: the fatal migration transaction opens *after* every base DDL slice (`internal/core/db.go:3048-3056`), so the Continuity tables land last in **both** paths and the two ordered lists are **identical** today. §14.2 E28 measures both halves: an object created after the others lands **last** in the ordered list, and recreating a table with **byte-identical** sql leaves the schema identical while the **ordered** list changes — so an order-sensitive comparison fails on an identical schema, and a compare-by-name comparison still passes |

Neither finding changed the schema, an acceptance requirement, an implementation stage or an operator
decision. Both errors were in the **safe direction** — they over-stated a surface rather than
understating it — and neither touched the containment guarantees.

---

## 13. The architectural preferences (P1–P6)

These are **recommendations**, not defects. They are recorded because two of them need a decision
(OD1, OD2) and because the rest change the document's claims.

**P1 — Keep explicit-only checkpointing, but make the loss window visible.** The recommendation
stands: reject the automatic writer. A background writer needs a *source* for the brief, and the
only available source is the session transcript — which is exactly the "transcript as durable fact"
failure the brief asks about, and it would violate AT24's "no ambient overhead" as well.

But the draft's *"Nothing was lost"* is true only of the checkpointed state, and its framing is the
favourable case (the crash happens *after* the write). The unfavourable case — the agent makes forty
tool calls and dies before checkpointing — loses that work silently, and a fresh agent resumes from
a brief that may contradict what was actually done. The smallest policy that closes this **without a
writer** is to make staleness **visible at read time**, which is what §4.2's `stale` block and AT20
now do, and to **name the trigger**: the `mpm_continuity` registry entry's *"Use when:"* line and the
agent-installation snippets should state the checkpoint points — after a verified unit of work
completes, after a significant decision or plan change, at a meaningful state transition, before a
potentially disruptive operation, and before a long-running action where interruption is plausible.
That is "selective agent-triggered", implemented as guidance rather than as a mechanism, which keeps
every guarantee the explicit-only policy wants: no transcript, no ambient queries, no automatic
writing. **No transcript archiving, no automatic periodic writing, and no speculative promotion of
checkpoint content into the memory store.**

**P2 — The identity model is right; the *discovery* gap is the risk.** Workspace-scoped identity
satisfies the operator's requirement **within one workspace** — one `~/.mpm` serves many
repositories, many worktrees, many frameworks and many models — and the document is right that the
CWD must never be an input. What it does not deliver is a reason for a fresh agent to *look*.
**This is re-scoped from "blocks Stage 4" to "blocks the operator's requirement"** (OD1), and the
recommended answer is written down rather than deferred: a DB-backed session→project binding,
written by an explicit action, read by the Stage 4 source. The atomicity objection that rules out
`active.json` (a non-atomic `os.WriteFile`) does not apply to a row. The cost is one more table,
and that table is **not** part of the four-table Stage 1.

**P3 — Cross-workspace identity, when it comes.** Workspace-scoped is right and the deferral is
right, but the deferral must carry its real prerequisite: **MPM has no repository identity
primitive** (§1.3). "Add a repository binding" is not a small addition; it means first building an
explicit, operator-supplied repository key. When cross-workspace continuity is wanted, the right
shape is to use the shared database as a **name→id directory only** — never as the storage for
threads or checkpoints — so the workspace remains the unit of ownership and deletion. §6.2 and AT7
keep the shared schema free of project identity until that day.

**P4 — Budget enforcement is code-only; two cheap hardenings are available.** The draft's budgets
were not in the DDL: `headline`'s "≤ 200 bytes" was a comment, `title` was unbounded, and SQLite's
declared `JSON` type does **not** validate — the literal string `'not json at all'` stores happily
in a `JSON NOT NULL` column. Both hardenings are now in the DDL (§4.1):
`CHECK (json_valid(...))` on the four JSON columns, and byte-accurate `length(CAST(x AS BLOB))`
checks on `title` and `headline`. The code-level validation AT15 describes must be pinned regardless;
these are defence in depth, not a substitute.

**P5 — Two precision corrections to the inventory.** (a) The draft said *"no version counter
anywhere in the tree"*; `memory_revisions.version` **does** exist
(`internal/core/schema.go:255`). The substantive claim holds — a search for a version predicate in a
write finds nothing, so `memory_revisions.version` is a ledger counter, not a CAS guard — so the
wording is now *"no version-based CAS"* (§1.4, §2.8, §14). (b) The appendix attributed the
"6000-byte detail budget, 800-byte per-item cap, 20-item hard max" to
`internal/core/contextual_materialization.go:102-107`, but the 20-item ceiling is
`SelectionHardMaxItems = 20` in `internal/core/contextual_selection.go:46` — a **selection** limit,
not a materialisation limit. The budget table in §2.3 now cites only the two materialisation
numbers.

**P6 — Two small things worth adding to the tool surface.** `mpm_resolve`'s registry description
(`internal/core/tools/registry_list.go:759-762`) enumerates the pointer kinds and must be edited
alongside the resolver — §6.4 now gives that file a row that covers the description edit, not just
the new registry entry. And §6.1's claim that `mpm_help` picks the tool up automatically is true
and is now backed by the citations: `cmd/mpm/call.go:550` iterates `tools.Registry`, and
`cmd/mpm/call_help_test.go:74` pins that a shell-only agent can enumerate every capability.

---

## 14. Evidence appendix

### 14.1 What was verified in the tree

All citations re-checked at `main` @ `f8d4b95aefe60a0880409a8905ce2c4d92d22c00`.

| Claim in this document | Verified |
|---|---|
| §1.1 — three session axes, `active.json` is the MPM continuity axis | `internal/core/session_identity.go:27-41`, `cmd/mpm/session_runtime.go:44-52` ✓ |
| §1.2 — handoff 90-day TTL; scratchpad 24 h; works' lifecycle | `internal/core/gc_tools.go:117` (`PruneHandoffs(90)`), `internal/core/schema.go:1444` (`ephemeral_scratchpad`), `internal/core/work.go:53-63` ✓ |
| §1.3 — `lifecycle_events.project_id` is the only `project_id` column; "never fabricated" | `internal/core/schema.go:883` (the table), `:914` (the column), `internal/core/lifecycle_events.go:500` ✓ |
| §1.3 — no repository scoping anywhere; the git-evidence rule | `internal/core/db.go:7181-7234` ✓ (verbatim, including *"unavailable, not inferred"*) |
| §1.4 — the five pointer kinds | `internal/core/tools/handlers.go:7562`, `internal/core/tools/registry_list.go:759-762` ✓ |
| §1.4 — the pointer id charset is `^[a-z0-9-]+$` | `internal/pointer/pointer.go:17`; the inline copy at `internal/core/tools/handlers.go:7857-7862` ✓ |
| §1.4 — the polymorphic reference vocabulary | `internal/core/schema.go:355` — `memory\|theory\|decision\|lesson\|work` ✓ |
| §1.4 — `ProvenanceArtifactTypes`, seven values, CHECK-enforced | `internal/core/schema.go:23-26` ✓ |
| §1.4 — the restore allow-lists | `internal/core/sql_dump_validator.go:141` (`CanonicalMPMSchema`), `:231` (`RuntimeCanonicalSchema`) ✓ |
| §1.4 — the identity-derived id + `ON CONFLICT DO NOTHING` | `internal/core/evidence_store.go:92-105` ✓ |
| §1.4 — the materialisation budgets | `internal/core/contextual_materialization.go:102-107` (6000 / 800) ✓ |
| §2.3 — **a decision is a `memories` row with `collection='decisions'`** | `internal/core/citation.go:121`, `internal/core/epistemology_tools.go:501` ✓ |
| §2.3 — `GetMemory` does **not** filter by collection | `internal/core/web_db.go:258` ✓ |
| §4.4 — the `mpm://theory/` precedent | `internal/core/tools/handlers.go:7660-7668` ✓ |
| §2.8 — no version-based CAS anywhere | searched: no `WHERE … version =` write exists; `memory_revisions.version` (`internal/core/schema.go:255`) is a ledger counter ✓ |
| §4.1 — `core.GenerateID()` returns 16 bare hex characters | `internal/core/db.go:5283` ✓ |
| §4.1 — the substrate's DDL slices | `internal/core/schema.go`: `BaseTables:38`, `ReferenceTables:949`, `WorkTables:1050`, `ReferenceIndexes:1160`, `CommonIndexes:1172` ✓ |
| §6.2 — `attachShared` propagates `BaseTables` twice | `internal/core/db.go:2094` (the function), `:2132` (the CREATE TABLE loop), `:2160` (the CREATE INDEX loop) ✓ |
| §6.2 — the fresh-DB path executes `BaseTables`, `ReferenceTables`, `BlobsTable`, `WorkTables`, `ReferenceIndexes` | `internal/core/db.go:2927-2960` ✓ |
| §6.2 — the fatal migration call sites; `SafeMigrations` only warns | `internal/core/db.go:3109-3140`; `:2967-2975` ✓ |
| §6.3 — `TestCanonicalSchemaSync` requires an exact two-way match | `internal/core/sql_dump_canonical_schema_test.go:22-62` ✓ |
| §6.2 — the shared-DM harness exists | `internal/core/testhelpers.go:175` (`NewTestSharedDM`) ✓ |
| §7 — Stage A's shape and its sentinel-abort deviation | [PR #225](https://github.com/flowbyte-com/mpm/pull/225), unresolved issues #1 and #3 ✓ |
| §6.4 — `mpm_help` picks the registry up automatically | `cmd/mpm/call.go:550`; `cmd/mpm/call_help_test.go:74` ✓ |

### 14.2 The DDL probe — the §4.1 DDL, executed

The §4.1 DDL was executed **verbatim** against a disposable SQLite 3.46.1 database with
`PRAGMA foreign_keys = ON`. Every case below asserts an **observable** outcome. The probe is
reproducible from this document: the DDL is in §4.1 and the cases are listed here.

| # | Case | Observed |
|---|---|---|
| E1 | insert `AgentShell`, then `agentshell` | the second is **refused** — `UNIQUE constraint failed: project_names.name`; the stored name keeps its case; a case-insensitive lookup finds exactly 1 row |
| E2 | insert focus `form-builder`, then `Form-Builder` | the second is **refused** — `UNIQUE constraint failed: continuity_threads.project_id, continuity_threads.focus`; the case-insensitive lookup finds exactly 1 thread |
| E3 | `DELETE FROM projects` with one name, one thread and one checkpoint | **refused** — `FOREIGN KEY constraint failed`; names/threads/checkpoints all survive (1/1/1) |
| E4 | a checkpoint with `thread_id` of project A and `project_id` of project B | **refused** — `FOREIGN KEY constraint failed`; the same-project insert lands |
| E5 | `last_checkpoint_id` → a nonexistent id | **refused** — `FOREIGN KEY constraint failed` |
| E6 | `last_checkpoint_id` → another thread's checkpoint | **refused** — `FOREIGN KEY constraint failed` |
| E7 | the production write order (insert the checkpoint, then advance the pointer) | **succeeds** — `last_checkpoint_id = ck-1` |
| E8 | `superseded_by` → a thread of another project | **refused** — `FOREIGN KEY constraint failed`; a same-project supersession lands |
| E9 | soft-delete a project and withdraw its two names | both names are **retained** with the deletion actor; the withdrawn name no longer resolves; a successor project takes the live name while the tombstone keeps its history |
| E10 | a second canonical name for one project | **refused** — `UNIQUE constraint failed: project_names.project_id`; a withdrawn canonical row releases the slot |
| E11 | a malformed `facts` document | **refused** — `CHECK constraint failed` |
| E12 | a 201-byte `headline` | **refused** — `CHECK constraint failed` |
| E13 | rename (demote then promote, one transaction) | exactly one live canonical name survives |
| E14 | the A→B / B→A rename swap | **both refused** — `UNIQUE constraint failed: project_names.name`; a rename onto another project's live *alias* is refused too. The alias graph cannot form a cycle |
| E15 | the id charset | `cpj-…`, `ct-…`, `ck-…` all match `^[a-z0-9-]+$`; `cpj_…`, `ct_…`, `ck_…` do **not**; the substrate's own 16-hex `GenerateID()` output matches |
| E16 | the §4.1 DDL text itself | the probe **extracts** the DDL from §4.1 of this document and executes that text, so the claim "the DDL was executed verbatim" is checkable rather than asserted; the extracted statements are compared against an independently authored reference copy and must match |
| **E17** | **the JSON columns hold arrays, not coerced scalars** (D7) | on **each** of `facts`, `hypotheses`, `state`, `next_actions`: `{}`, `42`, `1.5`, `"str"` and `null` are all **refused** with `CHECK constraint failed`; an array is stored as **TEXT**; `[1,2]` stays TEXT — the NUMERIC affinity cannot coerce an array |
| **E18** | **a same-thread supersession** (D1) | **accepted** — `(ck-3, ct-1, cpj-a, supersedes ck-1)` lands, and the stored `thread_id` and `supersedes` are what was written |
| **E19** | **a cross-thread supersession** (D1) | **refused** — `FOREIGN KEY constraint failed` |
| **E20** | **a cross-project supersession** (D1) | **refused** — `FOREIGN KEY constraint failed` |
| **E21** | **a supersession of a nonexistent checkpoint** (D1) | **refused** — `FOREIGN KEY constraint failed` |
| **E22** | **an `UPDATE` that repoints `supersedes` across threads** (D1) | **refused** — the containment holds on the update path, not only on the insert path |
| **E23** | **the append-only ledger** (D1) | deleting a **superseded** checkpoint is **refused** (`FOREIGN KEY constraint failed`); a checkpoint that supersedes nothing **is** deletable; and no row survives any refused supersession |
| **E24** | **the referenced unique key** (D1) | `idx_continuity_checkpoints_id_thread` exists and its columns are exactly `(id, thread_id)` — the composite parent key the `supersedes` FK needs |
| **E25** | **every `mpm://` pointer in the document** (D3) | the probe transcribes the substrate's **real** inline `parsePointerURI` (`internal/core/tools/handlers.go:7827-7864`) and runs it over every URI the document contains: the **four literal pointers all parse**, and the one pointer the document shows as rejected **really is rejected** |
| **E26** | **every example id's shape** (D3) | each literal pointer's id is either `prefix + 16 lowercase hex` (the Continuity ids) or the substrate's own **16 bare hex** (the artifact ids) — and the document contains **no** pointer with an unjustified underscore id |
| **E27** | **the `SET NULL` alternative, measured** (D1) | SQLite applies `ON DELETE SET NULL` to **every** column of the child key, so on the composite FK it tries to null the `NOT NULL` `thread_id` and refuses the delete with `NOT NULL constraint failed: ck.thread_id` — a misleading error for the right refusal. `RESTRICT` refuses the same delete with `FOREIGN KEY constraint failed` |
| **E28** | **the ordered `sqlite_master` list** (AT2, D11) | **first**: an object created after the others lands **last** in the ordered list, and the pre-existing objects keep their relative order — so the four tables the migration creates (inside the fatal migration transaction, which opens after every base DDL slice — `internal/core/db.go:3048-3056`) land last in **both** the fresh and the migrated path, and the two ordered lists are **identical today**. **second**: recreating a table and its indexes with **byte-identical** sql leaves the object set and every object's sql unchanged while the **ordered** list changes — so an order-sensitive comparison fails on an identical schema, and a compare-**by-name** comparison still passes |

**84 assertions, 0 failures.**

**The probe reads the specification.** It does not carry its own copy of the DDL: it extracts the
§4.1 `sql` block from this file and executes that text, then asserts that the extracted statements
match a separately authored reference copy. The document and its evidence therefore cannot drift
apart silently — a change to §4.1 that breaks an invariant breaks the probe on the next run.

**Two contradictions this probe found in the draft**, both now fixed in §3 and §4.1:

1. The draft's §3.1 example held a live canonical `AgentShell` **and** a live alias `agentshell` —
   which the case-insensitive uniqueness forbids. §3.1 now uses a genuinely different alias.
2. A case-insensitive *uniqueness* index alone does not make a *lookup* case-insensitive: a plain
   `WHERE focus = 'FORM-BUILDER'` uses the column's collation, which was BINARY. The `focus`
   column therefore now **declares** `COLLATE NOCASE`, so the enforcement and the lookup agree
   without a per-query `COLLATE` clause an implementer could forget.

### 14.3 The investigation, in one table

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
| What is the pointer vocabulary? | five kinds — `blob`, `work`, `memory`, `lesson`, `theory` — and their id charset | `internal/core/tools/handlers.go:7562`; `internal/pointer/pointer.go:17` |
| **Where does a decision live?** | **a `memories` row with `collection='decisions'`** — so it is already reachable via `mpm://memory/<id>` | `internal/core/citation.go:121`; `internal/core/web_db.go:258` |
| What is the provenance vocabulary? | `ProvenanceArtifactTypes` — 7 values, CHECK-enforced, widening needs a table rebuild | `internal/core/schema.go:23-26` |
| What is the CAS idiom? | state-column predicates; **no version-based CAS** | §2.8 |
| What is the transaction layer? | `WithTx` — restarts on a stale snapshot, rolls back on error; `execTracked` — budget 5 | `internal/core/db.go:1163`, `:1324` |
| What is the restart-safe insert idiom? | an identity-derived id plus `ON CONFLICT(id) DO NOTHING` | `internal/core/evidence_store.go:92-105` |
| What are the projection budgets? | 6000-byte detail budget, 800-byte per-item cap | `internal/core/contextual_materialization.go:102-107` |
| **Where is the 20-item ceiling?** | **`SelectionHardMaxItems = 20`** — a *selection* limit, not a materialisation limit | `internal/core/contextual_selection.go:46` |
| Where does the tool surface live? | one `Registry` slice; `mpm_<noun>`; `action` + `params`; `oneOf` per action | `internal/core/tools/registry_list.go:5-23` |
| What must a new table be added to? | `CanonicalMPMSchema` — the restore security allow-list; `RuntimeCanonicalSchema` is for tables *not* declared in Go source | `internal/core/sql_dump_validator.go:141`; `internal/core/sql_dump_canonical_schema_test.go:22-62` |
| **Which slice does `attachShared` propagate?** | **`BaseTables`** — twice, for tables and indexes. A migration-owned table never reaches the shared schema | `internal/core/db.go:2094` (the function), `:2132` (the CREATE TABLE loop), `:2160` (the CREATE INDEX loop) |
| Is the current directory ever a resolution input? | **No** — and the substrate has a documented rule against it | `internal/core/db.go:7181-7234` |
| How does the substrate inject a migration failure? | a trigger that aborts the **sentinel write**, which is *after* the DDL — a strictly stronger rollback | [PR #225](https://github.com/flowbyte-com/mpm/pull/225), unresolved issue #1 |
| §4.2/D10 — the handoff join column is `mpm_session_id`, and it is **not** unique | `handoffColumnMPMSession = "mpm_session_id"`; *"mpm_session_id is NEVER unique, so multiple handoffs from the same lifecycle all share the same value"*; the substrate's own selection is `ORDER BY ended_at DESC, rowid DESC LIMIT 1` | `internal/core/handoff.go:420`, `:157-158`, `:468-469` ✓ |
| §4.2/D10 — the legacy `session_id` column is the **UNIQUE** one | `session_id TEXT UNIQUE` in the `session_handoffs` table; the `mpm_session_id` column was added by a later migration | `internal/core/schema.go:1321`; `internal/core/migration_session_handoffs_mpm_session_id.go:128` ✓ |
| §5/D9 — the checkpoint id is the substrate's own 16-hex `GenerateID()`, not content-derived | `func GenerateID()` → `hex.EncodeToString(hash[:])[:16]` | `internal/core/db.go:5283-5291` ✓ |
| §2.7/D12 — foreign keys are DSN-borne, and `InitSchema` does not enable them itself | `_foreign_keys=1` in the production DSN; `InitSchema` deliberately omits the `PRAGMA` | `internal/core/db.go:98-155`, `:2914-2921` ✓ |
| §4.4/D1 — SQLite applies `ON DELETE SET NULL` to **every** column of a composite child key | measured: on the composite FK it tries to null the `NOT NULL` `thread_id` and refuses the delete with `NOT NULL constraint failed`, whereas `RESTRICT` refuses it with `FOREIGN KEY constraint failed` | §14.2 E27 ✓ |

### 14.4 The citation audit

This document makes **145 file-and-line citations** into the substrate's source. A drifted line
number would make a claim look verified when it is not, so the citations are audited mechanically
rather than by eye — and, since the revision-2 review, against the **declared base** rather than
against whatever tree happens to be checked out.

**The root cause of the previous revision's four stale citations, stated plainly.** The previous
audit ran against a working tree that was **20 commits behind `main`** — an ancestor of the declared
base `f8d4b95a`, not the base itself. Every citation therefore resolved and every load-bearing
window was checked, but against **the wrong files**: the pointer-parser surfaces had moved by 72 to
106 lines between that tree and the base. The audit was not weak; it was aimed at the wrong target.
An audit that runs against an arbitrary checkout validates nothing, so the base is now an explicit
parameter and every audit below refuses to run against a tree that is not it.

| Audit | Criterion | Result |
|---|---|---|
| **Resolution** | the cited file exists, and the cited line range is inside it and non-blank | **145 / 145** |
| **Content** | the cited window contains the symbol or literal the sentence claims | **56 / 56** |
| **Declaration-anchored** | the cited window contains the symbol's **declaration** — the `var X =`, `func X(`, `CREATE TABLE X` form — and not merely a later mention of it | **62 / 62** |
| **Generic drift screen** | over **every** citation: a symbol the sentence talks about exists in the cited file but **not at the cited lines** | **no unexplained drift** (see the residual class below) |
| **§ cross-references** | no `§n.m` points at a heading that does not exist | **47 / 47** |
| **Internal agreement** | the stage gates agree with the acceptance matrix, every A1–A16, D1–D6, F1–F2 and P1–P6 is dispositioned, every OD/R row states its status and what it blocks, and no superseded claim survives | **180 / 180** |
| **The DDL probe** | §14.2 — the DDL executed verbatim, plus the pointer parser run over every URI in the document, plus the ordered-`sqlite_master` measurement behind AT2 | **84 / 84** |

**The four citations the revision-2 review found stale, and their corrected values.**

| the claim | the file | the previous revision cited | the correct location at `f8d4b95a` |
|---|---|---|---|
| the five pointer kinds (the kind check) | `internal/core/tools/handlers.go` | line 7490 | **line 7562** |
| the `mpm://theory/` precedent (the collection assertion) | `internal/core/tools/handlers.go` | lines 7588–7596 | **lines 7660–7668** |
| the inline id-charset copy | `internal/core/tools/handlers.go` | lines 7751–7762 | **lines 7857–7862** |
| the `mpm_resolve` registry description | `internal/core/tools/registry_list.go` | lines 755–758 | **lines 759–762** |

The stale values are written as bare line numbers on purpose: a stale citation in a copyable
`path:line` form is exactly the defect this table is about, so none of them is written in one.

The previous revision's §14.4 narrative reported the last of these as "corrected **to** lines
759–762 (not 759–762)" — it had moved the citation *in the wrong direction*, which is what a check
against the wrong tree produces. The corrected narrative is above, and the claim it makes is the one
the instruments support.

**The generic drift screen has one residual false-positive class, and it is named rather than
hidden.** The screen flags a citation when a symbol the sentence mentions exists in the cited file
but not at the cited lines. That rule is sound for a sentence that cites one file and whose subject
*is* a symbol, but a sentence whose subject is a **tool** rather than a symbol — or one that cites
several files — can be flagged when the identifier belongs to a different citation in the same
sentence. **Seven** such cases remain, and each was inspected by hand: the `session_id` axis row
(cited to `getOrMakeSessionID`, the per-process id generator), the pointer-convention row (cited to
the kind check; `mpm_resolve` is the tool's name, not the cited symbol), the
`epistemology_tools.go` row (cited to the `"decisions"` collection; `GetMemory` belongs to the
*other* citation in the same sentence), T2's row (cited to the `ON CONFLICT(id) DO NOTHING`
insert; `WithTx` is the row's subject), §2.3's `GetLesson` citation (cited to the `lessons_base`
read; `GetMemory` is a **call** at `internal/core/db.go:4619` and is declared in
`internal/core/web_db.go:258`, which is the *other* citation in the same sentence), and the two
`internal/core/db.go:3048-3056` citations (cited to the ordering-dependency statement;
`sqlite_master` occurs elsewhere in `db.go` for unrelated reasons).
**All seven citations are correct.** The declaration-anchored audit is the rigorous instrument; the
screen is a sweep that finds candidates, not a verdict.

**How a reviewer can reproduce all of it.** The DDL probe is self-contained: it reads the DDL out
of §4.1 of this file, executes it against a disposable SQLite database, and runs the substrate's
**real** inline pointer parser over every `mpm://` URI the document contains — so "the DDL was
executed verbatim" and "every pointer in this document parses" are both checkable rather than
asserted. The citation and consistency audits are disposable scripts over this file and a checkout
**at the declared base**; they are not part of the repository, because a documentation commit that
adds a checker for its own prose is a different kind of change than the one this revision is.
