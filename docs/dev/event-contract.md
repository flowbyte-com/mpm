# E — Shared event contract: proposal v0.1 (for review)

> **STATUS: PROPOSAL. NOT AGREED.**
> Sessions B and C must both explicitly acknowledge a versioned contract
> before substantial implementation begins. This document is the input to
> that acknowledgement, not the acknowledgement itself.
>
> Author: Session A. Date: 2026-10-26.

## 1. The existing substrate this builds on — no new framework

The brief asks that a new generic telemetry framework **not** be created
without demonstrating that the existing infrastructure cannot satisfy the
requirements. It can. MPM already has a real, durable, correlated event
substrate:

### 1.1 `tool_invocations` — the primary event table

`internal/core/schema.go:1315`. Already carries, per event:

| Column | Meaning |
|---|---|
| `id` | TEXT primary key — **stable event identity** |
| `session_id` | the logical session |
| `tool_name`, `action` | what was invoked |
| `invocation_id` | **correlation id** across the whole call chain |
| `actor_kind` | `agent` / `human` / `unknown` / `system` |
| `framework_name` | `opencode`, `mcp`, `mpm-cli`, … |
| `framework_session_id` | **host-owned native session id** |
| `mpm_session_id` | MPM's own session id |
| `payload_hash` | hash of the request payload — no raw payload retained |
| `result_status` | `success` / `error` (CHECK-constrained) |
| `started_at`, `completed_at`, `duration_ms` | timing |
| `error_message` | bounded failure detail |
| `outcome_class`, `outcome_code` | closed enum + bounded subtype |

`outcome_class` is already a **closed enum** (`internal/core/tool_outcome.go`):
`ok`, `validation`, `not_found`, `conflict`, `degraded`, and the rest. Degraded
outcomes still record `result_status = 'success'` — degradation is queryable
without lying about success. **This is exactly the classification vocabulary
the contract needs, and it already exists.**

### 1.2 `system_audit_log` — the substrate's own event log

`internal/core/schema.go:1032`, with four optional correlation columns whose
documented semantics are directly relevant:

> `framework_session_id` — host-owned native session ID; NULL when host
> provides none (Pi, Hermes without hooks). **NEVER synthesized**.

That sentence is a contract clause that already exists and must be honoured by
the new contract: **an absent host session id stays absent.**

### 1.3 `mpm_evidence` — the evidence store

The store for observations that are not yet trusted knowledge. The contract
routes raw lifecycle events here rather than into `memories`/`lessons`/
`decisions`.

### 1.4 What this means for the proposal

**The contract does not define a new event table, a new wire format, or a new
ingestion pipeline.** It defines:

1. which **existing** columns and enums the adapter populates;
2. the **identity, ordering and deduplication rules** for lifecycle events
   that do not originate from an MPM tool call (session start, task
   boundary, compaction, handoff);
3. the **trust boundary** between raw observation and trusted knowledge;
4. the **privacy and retention** rules.

The only genuinely new storage, if any, is a lifecycle-event table — and §5
explains why it can be avoided.

## 2. What the OpenCode plugin actually exposes today

Verified by reading `agent_installation/mpm-opencode/src/`:

| Fact | Source |
|---|---|
| The plugin's typed context surface is `PluginContext` with exactly **three** members: `tool.transform`, `session.hook`, `shell.hook` — plus an index signature `[key: string]: unknown`. | `src/opencode-sdk.d.ts:81–98` |
| The plugin currently registers **two** hooks: `ctx.session.hook("context", …)` and `ctx.shell.hook("create.before", …)`. | `src/index.v2.ts:595, 611` |
| `ctx.session.hook("context", …)` fires with an event carrying `sessionID`, and the plugin pushes text into `event.system`. It is used to inject the wake context once per session, cached by `sessionID`. | `src/index.v2.ts:594–603` |
| `ctx.shell.hook("create.before", …)` **carries no `sessionID`** — the plugin's own comment says so explicitly. | `src/index.v2.ts:608–610` |
| `ToolContext` provides `sessionID?`, `model?.id?`, `messageID?`. | `src/opencode-sdk.d.ts:111–116` |
| The plugin already exports `MPM_PROVENANCE_FRAMEWORK_SESSION_ID` from `ctx.sessionID` on the tool-execute path. | `src/index.v2.ts:217–229` |
| The plugin's `setup(ctx)` receives a `PluginContext` whose index signature means **the real SDK may expose more hooks than the hand-written `.d.ts` declares**. | `src/opencode-sdk.d.ts:96` |

**This is the single most important fact in this document.** The `.d.ts` in
the repository is a *hand-written subset* of the real `@opencode-ai/plugin`
types. It is therefore **not evidence about what the installed plugin API can
observe** — only about what the adapter currently uses.

> **This is Session C's first task, and it must precede contract
> finalisation:** enumerate the hooks the *installed* OpenCode plugin API
> actually provides (from the real `@opencode-ai/plugin` package in
> `node_modules`, and from the host's own event stream), and report which of
> the candidate events in §3 are directly observable, which are inferrable,
> and which are not observable at all.

The contract below is written so that it **degrades safely** when a hook is
absent.

## 3. Candidate lifecycle events

Candidates, not instructions. Each is classified by how it would be obtained.

| Event | How it could be obtained | Notes |
|---|---|---|
| **session.started** | a hook, if one exists | `sessionID` is the identity anchor |
| **session.resumed** | inferred: an observed `sessionID` with a gap since its last event | inference, not observation — must be marked as such |
| **context.changed** | `ctx.session.hook("context")` — **already used** | fires per session today |
| **tool.outcome** | **already recorded** by `tool_invocations` | no new work; reuse |
| **task.boundary** | inferrable from the `mpm_work` lifecycle (`create` / `complete` / `cancel`) | `mpm_work` already exists and is already recorded |
| **compaction.started / .completed** | **unknown** — depends on §2's API investigation | highest-risk candidate |
| **handoff.written / session.completed** | `mpm_handoff` `write` / `read` — **already exists** | reuse |
| **critic.proposal** | `internal/critic` — **already exists** | reuse; see §6 |

**Six of the nine candidates are already implemented in MPM.** The genuinely
new work is: session start/resume, compaction, and possibly a task-boundary
signal that is not already implied by `mpm_work`.

## 4. The contract, v0.1

### 4.1 Schema version

Every lifecycle event carries `"schema_version": "mpm-event/0.1"`. A consumer
that does not recognise the version **ignores the event** and records that it
ignored it. There is no silent partial parsing.

### 4.2 Stable event identity and deduplication

| Field | Rule |
|---|---|
| `event_id` | `sha256(schema_version ‖ host ‖ framework_session_id ‖ host_event_id ‖ event_type)`, hex, 64 chars. |
| `host_event_id` | the host's own identifier for the event, if it has one. **If the host does not provide one, the event is not deduplicable and `event_id` falls back to `sha256(… ‖ observed_at ‖ event_type ‖ payload_hash)` — which is best-effort, and the event must set `dedup: "best_effort"`.** |
| Deduplication | Insert is `INSERT … ON CONFLICT(event_id) DO NOTHING`. A repeat is a no-op, never an error and never a second row. |
| `payload_hash` | hash of the canonicalised payload. Two events with the same hash and the same type are the same observation. |

**Duplicates must not silently accumulate.** This is a hard requirement of the
dogfooding environment (§ `dogfooding-runbook.md` §2).

### 4.3 Session and logical project identity

| Identifier | Source | Stability |
|---|---|---|
| `framework_session_id` | the host's native session id (OpenCode's `sessionID`). **Never synthesized** — if absent, it is `NULL`, exactly as `system_audit_log` already documents. | stable for the life of one host session |
| `mpm_session_id` | MPM's own `MPM_SESSION_ID` | stable for the life of one MPM session |
| `logical_project_id` | **new concept — see below** | stable across worktrees and restarts |
| `workspace_id` | the resolved MPM workspace path (`config.GetMPMDir()`) | stable for a workspace |

**`logical_project_id` — the multi-worktree requirement.** MPM has **no
per-project partitioning in the schema**. The workspace is the isolation
boundary (`config.GetMPMDir()` → `MPM_WORKSPACE` or `$HOME/.mpm`), and the
database lives at `<workspace>/src/db/mpm.db`.

So "multiple Git worktrees for one repository must map to the same logical
project identity" cannot be satisfied by the database today — there is no
project column. Two honest options:

- **(a) Derive it from the repository, not the working directory.**
  `logical_project_id = sha256(git remote URL ‖ canonical repo root identity)`
  — i.e. all three worktrees of `~/src/mpm` resolve to the same id, and a
  *different* repository resolves to a different one. Implementation: read
  `git config --get remote.origin.url` from the workspace root. Cheap, stable
  across worktrees and restarts, and independent of the cwd.
- **(b) Treat the workspace as the project.** Simple, already implemented, but
  it means one project per workspace — which is fine for the dogfooding
  environment (one workspace, three sessions, one project) and *is* the
  isolation mechanism for unrelated projects.

**Recommendation: implement (a) as a stored, derived column, keep (b) as the
isolation mechanism.** (a) gives the stable identity the brief requires;
(b) continues to prevent unrelated projects from sharing a database. A
project id **without** workspace isolation would be a scoping illusion — it
would be a label on a shared table, not a partition.

### 4.4 Timestamps and ordering

- `observed_at` — integer unix seconds, UTC, taken **at the adapter**, at the
  moment of observation. Never derived from a payload field.
- `ingested_at` — integer unix seconds, set by the substrate on insert.
- **Ordering is by `observed_at`, tie-broken by `event_id`.** Events can and
  will arrive out of order (a hook fires before the tool call that caused it
  completes). `ingested_at` is **not** an ordering key: it reflects when the
  substrate saw the event, not when it happened.
- A **late** event (observed_at older than the last event for that session)
  is accepted and stored. It is not rejected and not re-ordered in place.
  Consumers that need a sequence use `(observed_at, event_id)`.

### 4.5 Event type

A closed, versioned vocabulary. Proposed:

```
session.started        session.resumed       session.completed
context.changed        task.boundary         compaction.started
compaction.completed   handoff.written       critic.proposal
tool.outcome           (already recorded — see §1.1)
```

Adding a type is a **minor version bump** and is additive. Removing or
redefining a type is a **major version bump** and requires a migration note.

### 4.6 Event source and adapter provenance

Every event carries:

| Field | Value |
|---|---|
| `source` | the emitter: `opencode-plugin` |
| `host` | `opencode` |
| `host_version` | the host's version, if the API exposes it |
| `adapter_version` | the adapter's own version — the SHA of the plugin build |
| `framework_name` | `opencode` |

The pair (`source`, `adapter_version`) is what makes a stale or rewritten
adapter distinguishable in the record.

### 4.7 Task or operation correlation

- `invocation_id` — the correlation id MPM already assigns to a tool
  dispatch. Present when the event was caused by an MPM tool call.
- `task_id` — the MPM `mpm_work` artifact id, when the event corresponds to a
  tracked piece of work.
- Both are **nullable**. A lifecycle event with no MPM tool call behind it
  (a compaction, a session start) legitimately has no `invocation_id`.

### 4.8 Redacted, bounded evidence payload

- The payload is **bounded**: a hard byte ceiling (proposed: 4096 bytes) and
  a hard field count.
- It is **redacted by construction**: the adapter emits a *classification*,
  not raw content. Proposed fields are shapes, not text:
  `tool_name`, `action`, `result_status`, `outcome_class`, `duration_ms`,
  `changed_paths_count`, `diff_stat_summary`, `message_kind`.
- Free text, file contents, prompts, and command output are **excluded by
  default**. Including them requires an explicit, per-event opt-in that
  records *that* it happened.
- Where a pointer is needed, the event references an existing MPM artifact
  (`mpm://memory/<id>`) rather than embedding content.

### 4.9 Outcome or status

`result_status ∈ {success, error}` — the same CHECK-constrained vocabulary
`tool_invocations` already uses — plus `outcome_class` from the **existing**
`internal.ToolOutcomeClass` enum. **No new outcome vocabulary is introduced.**
For lifecycle events that are not calls, `result_status` is `success` and
`outcome_class` is `ok`.

### 4.10 Trust classification

Every event carries `trust: "observation"`. It is **never** `trusted`.

The promotion path is explicit and already exists:

```
raw event  --(B's critic, with evidence attached)-->  proposal
proposal   --(explicit review: mpm_request_review / challenge)-->  memory | lesson | decision
```

**Event capture must not automatically promote observations into trusted
memory, lessons or decisions.** The event stream is evidence; it becomes
knowledge only through the existing review path.

### 4.11 Retention and privacy

- Events are **operational observations**, not knowledge. They are retained
  on a **shorter** schedule than memories.
- The dogfooding environment's default: events older than 30 days may be
  pruned; memories are not subject to the same pruning.
- A payload marked `redacted: true` carries no recoverable content by
  construction.
- No event may contain a credential, a token, or an absolute path outside the
  workspace.

## 5. Storage — and why no new table is required *yet*

### Option 1 — reuse `tool_invocations` for everything

**Rejected.** `tool_invocations` is schema-constrained to *tool invocations*:
`tool_name` and `action` are `NOT NULL`, and `result_status` is
CHECK-constrained to `success`/`error`. A `session.started` event is not a
tool invocation. Forcing it in would corrupt the table's meaning.

### Option 2 — a new `lifecycle_events` table

The honest answer if lifecycle events are to be stored durably. It would
need: `event_id` PK, `schema_version`, `event_type`, `observed_at`,
`ingested_at`, `framework_name`, `framework_session_id`, `mpm_session_id`,
`logical_project_id`, `invocation_id`, `task_id`, `source`,
`adapter_version`, `trust`, `payload_hash`, `payload` (bounded JSON),
`dedup`.

**Cost:** one `SafeMigrations` entry in `internal/core/schema.go` — which is
the **single highest-collision file in the sprint** (two PRs already conflict
there). One new index. One new set of contract tests.

### Option 3 — **do not store lifecycle events at all, yet**

**Recommended for the first phase.**

The evidence for most of the candidate events already exists in
`tool_invocations`, `system_audit_log`, `mpm_work` and `mpm_handoff`. The
genuinely new observations — session start, compaction — are exactly the ones
that are **not yet confirmed to be observable at all** (§2).

Storing a table for events whose source does not exist yet would be building
the wrong schema. **Phase 1: Session C determines what is observable; Session
B determines what the existing substrate already captures; the two reports
together decide whether `lifecycle_events` is needed.**

If it is needed, it is added **as one PR with its contract tests**, not
incrementally.

## 6. How C emits and B ingests

| Step | Owner | Detail |
|---|---|---|
| **Observe** | C | In the adapter, at the hook, with the host's own identifiers. C does **not** decide what is worth remembering. |
| **Classify** | C | Map the host event onto the closed vocabulary in §4.5 and the existing `outcome_class` enum. An unmappable event is recorded as `session.completed` with a note, or dropped with a counter incremented. **It is never guessed into a neighbouring type.** |
| **Redact** | C | Emit a classification, not content (§4.8). The ceiling is enforced in the adapter. |
| **Hand over** | C | Either write to the substrate through the existing `mpm call` path (so `tool_invocations` records it like any other invocation) or hand the batch to B. **Preferred: the former** — it reuses the existing write path, its audit trail and its concurrency handling. |
| **Validate** | B | Check the schema version, the vocabulary, the bounds, the identity. Reject (and count) anything malformed. **Do not repair silently.** |
| **Deduplicate** | B | `ON CONFLICT DO NOTHING` on `event_id`. |
| **Classify trust** | B | Every event is `trust: "observation"`. B may *propose*; B may not auto-promote. |
| **Propose** | B | A typed, evidence-backed proposal (see the critic's existing proposal path in `internal/critic/`), carrying the `event_id`s it rests on. |
| **Promote** | **A / the agent** | Through the existing review path. **No automatic promotion.** |

## 7. Behaviour under repetition, disorder and absence

| Situation | Required behaviour |
|---|---|
| The same event arrives twice | Second insert is a no-op. No error, no second row, no "duplicate" warning in the agent's context. |
| Events arrive out of order | Accepted and stored. Ordered by `(observed_at, event_id)` at read time. `ingested_at` is never used as an ordering key. |
| A hook is absent in the installed OpenCode version | The events that depend on it are **not emitted**. No synthetic default, no placeholder event. The absence is *discoverable* — the adapter reports which hooks it successfully registered. |
| The host provides no session id | `framework_session_id` is `NULL`. **Never synthesized.** The event is still storable and still deduplicable via `payload_hash`. |
| The host's event id changes between runs | `event_id` changes, so the event is new. This is correct: it *is* a new observation. The `payload_hash` still lets a consumer detect "the same thing happened again". |
| An event arrives with an unknown `schema_version` | Ignored and counted. Not parsed partially. |
| An event exceeds the payload ceiling | Rejected and counted. **Never truncated into a valid-looking event.** |

## 8. Contract tests — required

The contract is only real if it is pinned. Required tests, all of which
belong in the PR that introduces the corresponding code:

| Test | Pins |
|---|---|
| `TestEventIdentity_StableAcrossWorktrees` | the same logical project resolves to the same `logical_project_id` from three different worktrees |
| `TestEventDedup_IdempotentOnRepeat` | inserting the same `event_id` twice yields one row and no error |
| `TestEventOrdering_UsesObservedAtNotIngestedAt` | out-of-order arrival is stored, and reads order by `(observed_at, event_id)` |
| `TestEventPayload_BoundedAndRedacted` | the payload ceiling is enforced; a payload containing a secret-shaped string is refused |
| `TestEventTrust_NeverPromotedAutomatically` | a captured event creates no memory, lesson or decision row |
| `TestEventAbsentHook_ProducesNoEvent` | a hook that did not register yields no event and no placeholder |
| `TestEventSchemaVersion_UnknownIsIgnored` | an unrecognised version is ignored and counted |
| `TestEventContract_CLIMcpParity` | the CLI and MCP paths produce the same event for the same input |
| `TestEventOutcome_UsesExistingToolOutcomeClass` | `outcome_class` values are a subset of the existing enum — no new vocabulary |

## 9. Open questions for B and C

1. **Which hooks does the installed OpenCode plugin API actually expose?**
   (§2 — Session C, before the contract is finalised.)
2. Is `logical_project_id` derived from the git remote acceptable to both
   sessions, or does B need a different derivation?
3. Should `session.resumed` be emitted at all, given it is an *inference*
   rather than an observation? (Recommendation: no — inference belongs in
   B's critic, where it can be labelled as inference.)
4. What is the right payload ceiling? 4096 bytes is a proposal, not a
   measurement.
5. Does the existing `mpm_work` lifecycle already cover "task boundary", or
   does a distinct signal help?
6. Where should the `lifecycle_events` table live if it is needed — and can
   it be added in one PR together with its contract tests, to avoid
   colliding with the two PRs already touching `internal/core/schema.go`?

## 10. Agreement block

> To be completed by Sessions B and C before substantial implementation
> begins. **Session A's acceptance is not sufficient.**

```
Session B — name: ....................  contract version accepted: mpm-event/0.1
  date: ............  objections: ............................................

Session C — name: ....................  contract version accepted: mpm-event/0.1
  date: ............  objections: ............................................
```

**Until this block is filled in, Sessions B and C may audit and investigate,
but may not implement against the contract.**
