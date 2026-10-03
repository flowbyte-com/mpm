# MPM Technical Specification

> **License:** [AGPL-3.0](../LICENSE) — copyleft with network-use clause. See [LICENSE](../LICENSE) for the full text.

> **Looking to install?** See [`agent_installation/INSTALL.md`](../agent_installation/INSTALL.md) for per-agent wiring (Claude Code, OpenCode, Pi, Hermes, OpenClaw), or jump to [§5 Quick Start](#5-quick-start) for MPM-only install.

MPM is an observable substrate for long-lived autonomous systems. Persistent memory is just one capability. Work, provenance, decisions, theories, and execution telemetry are all governed by the same self-observing foundation. The substrate is closed under observation: every operation on MPM is itself observable through MPM's own tools.

Traditional memory systems answer one question:

> *What does the agent remember?*

MPM answers four:

* What does the agent know?
* What is it trying to accomplish?
* Who/what produced this?
* What actually happened?

Those questions lead to a different architecture.

Instead of storing conversations, MPM stores reasoning.

Instead of assigning arbitrary confidence scores, confidence is derived from evidence.

Instead of rewriting memories, beliefs evolve while history remains intact.

One persistent cognitive substrate. One SQLite database. Five cooperating binaries. No vector database. No distributed infrastructure. No web UI.

`mpm` runs continuously in the background: a hardened SQLite data plane, an autonomous 03:00 UTC diagnostic critic, and an MCP server (`mpm-mcp`) for machine-to-machine integration. Three surfaces share one substrate: the human-facing CLI (`mpm <verb>` cognitive vocabulary), the agent-facing JSON-RPC (`mpm call <tool>`), and the MCP server (`mpm-mcp`) for AI agents. A fourth sibling — `mpm-telemetry` — records raw LLM invocation economics into its own `telemetry.db` without touching the MPM substrate. See [§6.6](#66-telemetry-sidecar-mpm-telemetry).

MPM models reasoning processes — belief formation, evidence weighting, theory tracking, self-correction. It does not claim to reproduce human cognition. "Cognitive" describes *what kind of object is being persisted*, not a claim about machine minds.

> **MPM is not designed to maximize recall. It is designed to preserve intellectual progress.**

---

## Reading Guide

This is one document. It is long because MPM covers ground. Read by audience:

| If you are… | Read first | Then |
|---|---|---|
| **A curious reader** evaluating MPM | §1 — §3 | §6 (skim) |
| **A new operator** wiring MPM into an agent | §1, §5 (Quick Start) | §8 (CLI), §9 (Runtime) |
| **A contributor** reading code or writing patches | §1 — §7 | Appendices A, B, C, F |
| **An agent author** integrating via MCP or `mpm call` | §5, §6.4 (MCP), §6.5 (Shared Epistemology) | §8 (CLI parity table) |
| **Future me** returning after months away | §3 (axioms), §10 (reliability) | Appendix C (enforcement patterns) |
| **An operator on call** chasing a stuck queue or a broken skill | §9 (Runtime), §10 (Reliability) | Appendix D (cascades), Appendix E (capabilities) |

Every section is self-contained enough to read in isolation. This document is the whole contract — the architecture, the operator reference, and the implementation specifications are all here. Nothing in it defers to a document you have to go and find.

### Table of Contents

1. [What is MPM?](#1-what-is-mpm)
2. [Why this isn't a memory system](#2-why-this-isnt-a-memory-system)
3. [The Cognitive Model](#3-the-cognitive-model)
4. [Belief Lifecycle](#4-belief-lifecycle)
5. [Quick Start](#5-quick-start)
6. [System Architecture](#6-system-architecture)
   - 6.1 [Core vs Runtime](#61-core-vs-runtime)
   - 6.2 [Confidence Engine](#62-confidence-engine)
   - 6.3 [Retrieval Architecture](#63-retrieval-architecture)
   - 6.4 [MCP Integration](#64-mcp-integration)
   - 6.5 [Multi-Agent Shared Epistemology (Layers 0–4)](#65-multi-agent-shared-epistemology-layers-04)
   - 6.6 [Telemetry Sidecar (mpm-telemetry)](#66-telemetry-sidecar-mpm-telemetry)
   - 6.7 [Epistemic Compaction](#67-epistemic-compaction)
7. [Core Stability](#7-core-stability)
8. [CLI Reference](#8-cli-reference)
9. [Runtime Services](#9-runtime-services)
10. [Reliability](#10-reliability)
11. [Glossary](#11-glossary)
- [Appendix A: Shared Epistemology Implementation](#appendix-a-shared-epistemology-implementation)
- [Appendix B: Arc 2 (Active Dissemination) Implementation](#appendix-b-arc-2-active-dissemination-implementation)
- [Appendix C: Enforcement Patterns](#appendix-c-enforcement-patterns)
- [Appendix D: Epistemic Cascades — Operator Reference](#appendix-d-epistemic-cascades--operator-reference)
- [Appendix E: Capability Lifecycle — Specification](#appendix-e-capability-lifecycle--specification)
- [Appendix F: CLI & Command Architecture](#appendix-f-cli--command-architecture)
- [Community & Security](#community--security)
- [License](#license)

---

## 1. What is MPM?

MPM is a unified substrate deployed as a primary CLI (`mpm`) with companion daemon binaries (`mpm-mcp`, `mpm-scheduler`, `mpm-critic`, `mpm-telemetry`) — everything stored in one SQLite database with FTS5 full-text search. The persistent cognitive substrate is a single SQLite file; no separate vector database, no distributed infrastructure, no remote SaaS service is required. Semantic embeddings can be delegated to a locally running **Ollama** inference provider (the default model is `nomic-embed-text`); Ollama performs inference, it is not the MPM persistence layer.

It is the reasoning layer for AI agents. It tracks not just *what* the agent knows, but *why* it decided to act, *how* it chose to act, and *what it believes but hasn't proven yet*.

### What MPM is not

MPM is not a workflow engine, an orchestration framework, a planning system, an agent runtime, a distributed database, or an infinitely extensible plugin framework. Those things may be built *on top of* MPM. They are not part of MPM.

This is the project's most important self-defense against feature creep. The regret log (see §7) is the empirical record of pressure to absorb adjacent problems; the non-goals are the standing answer. **The discipline must survive success** — the temptation to make MPM absorb an adjacent problem only grows as more people use it.

At the implementation level, the exclusions are concrete:

- **Not an HTTP server.** No request/response REST surface; machine integration is stdio-only via `mpm-mcp`. The CLI is for operators, not for serving web traffic.
- **Not generic storage.** Built for AI agent cognition: weighted recall, decay, epistemology, proactive hints.
- **Three surfaces, one substrate.** The CLI is the human-facing cognitive interface (`mpm remember`, `mpm learn`, `mpm decide`, ...); `mpm call` and MCP are the agent-facing tool surfaces. All map to the same `internal/core/tools` registry.
- **Not a vector database.** A SQLite-native ANN index handles semantic recall. No Pinecone, no Qdrant, no MPM-managed embeddings service (a locally running Ollama instance may be used as the embedding inference provider — see §6.3).
- **Not a knowledge graph.** Relationships are first-class artifacts (decisions, theories, evidence, lessons), not edges in a graph store.

---

## 2. Why this isn't a memory system

Most AI memory systems focus on retrieval. An agent stores information, retrieves information, and continues operating. The problem is that real cognition involves more than facts.

Consider a software engineering agent:

- **Fact:** WordPress strips inline style tags.
- **Decision:** Store widget CSS in `wp_options`.
- **Hypothesis:** The CLI parser fails when `--json` appears before positional arguments.
- **Evidence:** Unit tests confirm the parser bug.
- **Conclusion:** Flag ordering caused the issue.

Most memory systems flatten these into generic notes. Months later the agent remembers the conclusion but not the reasoning. The result: repeated investigations, reopened decisions, contradictory conclusions, lost institutional knowledge.

### The distinction

| Capability | Traditional Memory | MPM |
|---|---|---|
| Fact Storage | ✓ | ✓ |
| Long-Term Recall | ✓ | ✓ |
| Semantic Search | ✓ | ✓ |
| Decision Tracking | ✗ | ✓ |
| Hypothesis Management | ✗ | ✓ |
| Evidence Chains | ✗ | ✓ |
| Knowledge Challenges | ✗ | ✓ |
| Reasoning Persistence | ✗ | ✓ |

MPM does not merely remember *what* was decided. It preserves the reasoning that produced the decision — the alternatives considered, the evidence weighed, the hypotheses pending, the challenges raised and resolved.

That distinction transforms memory from passive storage into persistent reasoning.

The goal is not simply remembering information. **The goal is preserving intellectual progress.**

---

## 3. The Cognitive Model

Every persistent object inside MPM exists because it answers a different cognitive question.

### 3.1 The Artifacts

| Artifact | Purpose |
|---|---|
| Memory | Something observed or learned |
| Decision | A choice and its rationale |
| Theory | A testable hypothesis |
| Work | A tracked unit of work with immutable event history |
| Reference | External material for the agent to consult |
| Lesson | Reusable knowledge |
| Skill | A reusable procedure ("how to act") |
| Evidence | Information supporting or challenging another artifact |

The distinction matters.

The artifacts interlock through **provenance edges** (`source_ids`, `trigger_evidence_id`). When a load-bearing artifact is invalidated, **epistemic cascades** (§4.8) propagate the invalidation to every downstream decision or theory that depended on it — surfacing a pending theory for each one rather than silently rewriting the artifact.

A decision is not a memory.

A theory is not a lesson.

Evidence is not a conclusion.

Each has its own lifecycle.

#### Memory

General facts, observations, and synthesized insights.

> Germany leads Group E with +6 goal differential.

Memories are weighted, searchable, reinforced, challenged, and eventually archived.

#### Decision

Choices captured together with context and rationale.

```
CONTEXT: Need CSS injection that survives wp_kses filtering.
CHOICE: Store widget CSS in wp_options.
RATIONALE: WordPress strips inline style tags.
```

This allows agents to reconstruct previous reasoning instead of re-evaluating the same problems repeatedly.

#### Theory

Hypotheses that have not yet been proven.

```
HYPOTHESIS: Flag ordering causes parser failure.
VALIDATION: Run parser tests with positional-first and flag-first inputs.
STATUS: pending
```

Theories create a structured workflow for experimentation and debugging.

#### Work

A tracked unit of work with an immutable event-sourced history. Every state change is recorded as an append-only event; the current state (`status`, `completed_at`, `updated_at`) is derived at read time via `RecomputeWorkProjection`.

```
mpm call mpm_work --payload '{"action":"create","params":{"title":"Fix the parser bug","note":"Suspect flag ordering in cli.go"}}'
mpm call mpm_work --payload '{"action":"complete","params":{"work_id":"<id>"}}'
mpm call mpm_work --payload '{"action":"history","params":{"work_id":"<id>"}}'
mpm call mpm_work --payload '{"action":"note","params":{"work_id":"<id>","note":"Confirmed: --json before positional causes parse failure"}}'
mpm call mpm_work --payload '{"action":"reopen","params":{"work_id":"<id>"}}'
mpm call mpm_work --payload '{"action":"archive","params":{"work_id":"<id>"}}'
mpm call mpm_work --payload '{"action":"unarchive","params":{"work_id":"<id>"}}'
```

The same surface over the CLI:

```
mpm work item archive   <work_id> [--note <text>]
mpm work item unarchive <work_id> [--note <text>]
mpm work item list [--status <s>] [--visibility <v>] [--limit <n>]
mpm work item purge <work_id> --reason-code <enum> [--note <text>] [--backup <path>] [--force]
```

`purge` is **CLI-only**. It is deliberately absent from the `mpm_work` and `mpm_system` MCP surfaces, because both are agent-reachable and an agent-reachable irreversible delete is a different tool with a different risk profile — the same reasoning that keeps destructive modes on `mpm gc` rather than in an MCP handler.

New agents should use `complete`, `cancel`, and `reopen` directly. The v1 `update` action with `status` param is preserved for backward compatibility but maps to the appropriate event type internally.

**Archive lifecycle.** Archiving removes a finished item from the operational surfaces without deleting anything. It is a visibility operation, expressed as a projection of two new ledger events (`archived`, `unarchived`) into `works.archived_at`; `NULL` means active. Archiving is **terminal-only** — `archive` refuses work whose `status` is still `open`, because an open commitment is a live obligation an agent is still acting on. A second `archive` is a no-op success that reports `already_archived` rather than an error or a duplicate event. `unarchive` restores visibility and nothing else: it never reopens work, so a `cancelled` item unarchived stays `cancelled` and never becomes `open`. Unarchiving a non-archived item is an error. Neither operation touches `verification`.

**Phase 2 verification model.** The `complete` action emits `WorkEventTypeClaimedComplete` — an event recording that an agent *claimed* completion, not that the work is verified. Verification is derived separately via `DeriveWorkVerification`, which aggregates evidence rows (git observations, test results, file artifacts, manual review) into one of four epistemic states:

| State | Meaning |
|-------|---------|
| `unverified` | No evidence collected (default) |
| `partial` | Audit evidence present (e.g. git commit) but no outcome evidence |
| `verified` | Outcome evidence confirms the claimed completion |
| `contradicted` | Evidence contradicts the claim |

`claimed_complete` changes the work's `status` to `done`; it does not change `verification`. The `evidence_observed` event type records that evidence was attached. The `status` column and the `verification` column are orthogonal — `status=done, verification=unverified` is a valid, expected state immediately after `complete` is called with no evidence yet attached.

**Status and visibility are separate filters.** `status` is the lifecycle axis (`open` / `done` / `cancelled` / `all`); `visibility` is the operational axis (`active` / `archived` / `all`, default `active`). Neither is derived from the other: an archived item still has whatever `status` it had, and a `done` item can be active or archived. Both compose on every listing, so the four meaningful combinations are all reachable — `status=done, visibility=archived` is "finished and filed away", and `--status all --visibility all` is the complete set.

All seven default operational surfaces exclude archived work: `mpm wake` open works, `mpm wake` completed refs, `mpm work` list, `mpm context` focus, contextual candidates, session work references, and the `ListWorks` / `ListAllWorks` core queries. Explicit by-id access is unchanged and deliberately *not* visibility-filtered — `show`, `history`, and `get` still resolve an archived item, because archive hides an item from the operational view, it does not make it unretrievable. The `mpm_work` list envelope echoes both filters it applied, so an empty result is distinguishable from a filtered one.

**Purge is logical deletion, not erasure.** `mpm work item purge` removes a work item from the active substrate. It is the administrative counterpart to `archive`: archive hides a live record, purge removes the record and its ledger. It is *not* a secure-deletion facility, and **MPM has no secure-erasure capability in v1.** Purge is logical removal at the SQLite layer, and it makes no guarantee about the bytes that may remain in `mpm.db` pages, the WAL, a rollback journal, pre-existing backups (`backups/critic-pre/` rotates at 7, so a snapshot taken before the purge retains the content until it rotates out), `mpm backup` dumps, filesystem snapshots, or content already transcribed into a handoff summary, a memory body, or the operator's own note. Every purge report — dry run and forced alike — prints that residual-exposure list rather than leaving it to the help text. There is deliberately no `privacy` reason code: v1 has no privacy-grade or forensic use case, and advertising one would promise a guarantee MPM cannot keep.

| Aspect | Behaviour |
|--------|-----------|
| Default | Dry run. Without `--force` nothing is written; the report states what a forced run would remove. |
| `--reason-code` | Required, on dry runs and forced runs alike, and constrained by a CHECK to `test_debris` \| `accidental` \| `corrupted` \| `migration_cleanup` \| `administrative` \| `other`. There is no free-text `--reason` — that name belongs to `resolve-contradiction` and carries a different meaning. |
| Preflight | Structural references only: a memory's `dependencies` array, `epistemic_provenance` rows **citing** the item, foreign `evidence` rows, and pending `epistemic_cascade_outbox` entries. Prose mentions of the id are not references. Any referrer refuses the purge and is enumerated by kind and id. |
| Citation direction | `epistemic_provenance.source_id` is the artifact being relied on; `downstream_id` is the artifact doing the relying. A row with `source_id = work` is an inbound reference and **refuses** the purge; a row with `downstream_id = work` is a citation the work itself made, is work-owned, and is **deleted with the work**. Only the inbound direction blocks. |
| Cascade | None. There is no `--cascade` flag and no code path rewrites a referring record. A refused purge changes nothing. |
| Transaction | One. Referrers are re-checked inside the transaction, `works` deletion is asserted to affect exactly one row, and success is confirmed by post-write read-back. |
| Audit | `work_purge_audit` records the work id, timestamp, reason code, operator, delete counts, and the operator's `--note`. No field is populated from the work artifact; the note is independent input and survives the purge. |
| `--backup <path>` | Explicit and opt-in; purge creates no backup of its own. The dump is written *before* the delete and only for a purge that will actually run, so a refused purge does not leave a copy of the content behind. It contains the material being purged, and the output says so. |

`tool_invocations` and the system audit history are left intact: the record of the purge is not part of what the purge removes.

#### Reference

External material ingested for the agent to consult — documentation, specs, articles, code. Reference artifacts are distinct from Memory: Memory is what the agent synthesises internally; Reference is what the agent can look up. References are stored with their source URL, section markers, and ingestion timestamp, and are searchable via the same FTS5 index as memories.

#### Lesson

Reusable knowledge that survives across tasks — best practices, warnings, patterns, and insights.

#### Skill

Procedural memory: "how to act." A skill is a markdown document with YAML frontmatter (name, version, when_to_use, domain, constraints, steps) describing a procedure the agent can run. Skills live in `collection='skills'`, are scanned by the secret/poison scanner on every write, and are surfaced via three discovery tiers (list, read, mpm_context action=proactive_recall_hint). New and revised procedures can be evaluated through the **Skills Workshop** before publication; see §9 for the full authoring and discovery surface.

#### Evidence

Information that supports or challenges another artifact. Evidence is the substrate from which confidence is derived, never an artifact-level assertion of truth.

**Calling-framework provenance.** When a non-MPM caller (Claude Code, OpenCode, a custom agent) invokes `mpm call`, MPM reads the following environment variables to record the calling framework's identity in its audit trail:

| Env var | Purpose | Default |
|---------|---------|---------|
| `MPM_PROVENANCE_FRAMEWORK` | Caller identity | `mpm-cli` |
| `MPM_PROVENANCE_MODEL` | Model name | _(empty)_ |
| `MPM_PROVENANCE_INVOCATION_ID` | This invocation's unique ID | _(auto-generated)_ |
| `MPM_PROVENANCE_PARENT_INVOCATION_ID` | Parent invocation for agent-of-agent tracing | _(empty)_ |

These populate `tool_invocations.framework_name` and `artifact_provenance.model_name`. Provenance records *how an artifact was created* — it does not assert that the artifact is correct or true.

### 3.2 Design Principles

MPM follows a small number of architectural rules. These are stated once here. The rest of the document references them; it does not re-explain them.

1. **History is immutable.** A memory records what was believed. The original artifact is never rewritten.
2. **Interpretation is mutable.** Confidence changes. Evidence accumulates. The original artifact does not.
3. **Truth is external.** Reality determines truth. MPM only maintains its current estimate.
4. **Reasoning is persistent.** Decisions, theories, and evidence are first-class objects — the same permanence as facts.
5. **The contract is stable; the implementation is not.** Formulas, storage, and algorithms may change. The properties, the guarantees, the semantics — these are what matter. The implementation can change; the meaning survives.
6. **Simplicity beats infrastructure.** SQLite provides persistence, transactions, FTS5, JSON, WAL, and indexing. Adding another service is a last resort, not a default.
7. **Keep it boring.** Boring infrastructure is reliable infrastructure. The interesting problems in MPM live in the cognitive model, not in the deployment surface.
8. **Minimize moving parts.** Every component added is a component that must be reasoned about, debugged, secured, and explained. We earn each new piece through usage evidence, not architectural enthusiasm.
9. **Sympathy for unknown unknowns.** We do not know what MPM will be used for in three years. The architecture favors reversible, conservative choices over clever, brittle ones.

### 3.3 Confidence

Confidence is not user input.

Confidence is derived.

```
confidence
  = initial(type)
  + supporting evidence
  − contradicting evidence
  − decay(time)

bounded to [0, 1]
```

The exact formula may change. The properties do not: more positive evidence never decreases confidence; decay never increases it. The formula is a tool for satisfying the properties; the properties are what the system means by "belief."

For the deeper mechanics (atomic recompute, evidence registry, trigger wiring, source-of-truth/cache split), see §6.2 and **Appendix C** (Enforcement Patterns).

> **Implementation note:** The properties are enforced by property tests in `internal/core/confidence_test.go`. The tests describe what the formula means, not what it currently is. The implementation can change; the meaning survives.

### 3.4 Reframe, not rename

MPM (Managed Persistent Memory) — a memory aid, not a memory store. The on-disk tables (`memories`, `topics`, `lessons`) keep their historical names for schema stability: renaming them would be a destructive migration across every existing database.

Where the choice is open, prefer:

- "Cognitive artifact" or "reasoning record" over "memory"
- "Belief" over "stored fact" when the artifact has confidence
- "Epistemology" over "knowledge management" when the lifecycle matters
- "Substrate" over "database" when the system is the point

### 3.5 What MPM maintains

MPM maintains two classes of durable state around an agent.

**Persistent agent state** — what the agent carries between sessions:

| Dimension | Question it answers |
|---|---|
| **Memory** | What does the agent know? |
| **Work** | What is it trying to accomplish? |
| **Reference** | What should it consult? |
| **Persona / Mode** | How should the agent behave? |
| **Directives** | What rules or instructions govern its behaviour? |

**Execution record** — what MPM observes about what happened:

| Dimension | Question it answers |
|---|---|
| **Provenance** | Who or what produced this? |
| **Evidence** | What actually happened? |

The distinction between the five state primitives matters:

- **Memory** is what the agent synthesises internally.
- **Reference** is external material ingested for consultation — distinct from Memory.
- **Persona** describes *how* the agent should present or conduct itself.
- **Mode** describes *the operating context or behavioural regime*.
- **Directive** expresses *an explicit constraint, instruction, or rule the agent should follow*.

For example:

```
Persona:   careful investigator
Mode:       debugging
Directive:  never modify production data
Reference:  database migration specification
Work:       investigate migration failure
Memory:     previous migration failures
```

Provenance and Evidence are not state the agent controls — they are the substrate's own observation of execution. Together they give MPM a causal chain from instruction through execution to observable outcome:

```
Directive
    ↓
Agent behaviour (Persona / Mode)
    ↓
Work
    ↓
Execution
    ├── Provenance  (who/what/under what config)
    └── Evidence    (git state, verification, what actually happened)
```

This is why MPM can answer questions ordinary memory systems cannot: not just "what did the agent remember?" but "what instruction governed that action, what execution produced it, and what observable evidence confirms it?"

**Architectural test:** Every MPM primitive fits one of five categories — state, instruction, context, execution, or evidence. Proposed features that don't fit one of these deserve suspicion before they become new tables.

### 3.6 The epistemic snapshot

Every memory saved through `mpm-mcp` carries a `metadata._epistemic_snapshot`
block — a system-stamped envelope that captures the epistemic environment
at the exact moment the memory was written. Five sub-blocks under one JSON
key (the leading underscore signals system-owned; agents and operators
should treat the block as substrate-internal):

| Sub-block | What it captures |
|---|---|
| `execution` | Active mode + persona + agent's self-assessment (`confidence_band`, `reasoning_depth`) |
| `provenance` | The tool observation that preceded this save, if any (within a 60-second window) |
| `context` | Literal snapshot of the agent's working goal text at save time (≤200 chars) |
| `creator` | Agent ID + session ID + runtime model |
| `validation` | Current evidence-based epistemic state (unvalidated / corroborated / contradicted), with `trigger_evidence_id` pointing at the row that flipped the state *(omitted when unvalidated)* |

Why this exists: without the snapshot, debugging a strange decision weeks
later requires walking the audit log, the evidence table, and the session
scratchpad to reconstruct "what was the agent thinking when it wrote
this?". With the snapshot, the answer is in the row itself.

**Privacy stance.** `goal_snapshot` is captured by default and may
contain sensitive context (the agent's working goal text at save time).
Operators running MPM on multi-tenant hosts should review this field.
The resolver does not redact — redaction is a v0.2 concern. Operators
who need to suppress the snapshot can call `SaveMemoryWithContext`
instead of `SaveMemoryWithContextAndSnapshot` (the legacy entry point
still works and never stamps a snapshot).

**Observation window.** Tool calls older than 60 seconds are dropped
from `provenance` — claiming "this memory was inspired by that file"
requires the tool call to have happened recently. Stale calls are
dropped, never weakened. Tunable per install via
`system_config.epistemic_snapshot.max_observation_window_ms`
(default `60000`).

**CLI vs MCP asymmetry.** The CLI (`mpm call mpm_memory --payload '{"action":"save",...}'`) does
not stamp `provenance` by default — it has no persistent tool buffer.
The MCP server, being long-lived, instruments every tool invocation
through `internal/core/tools/registry_intercept.go` and attributes saves
back to the observation that inspired them. Both paths stamp
`execution`, `context`, and `creator`.

**Schema versioning.** The block carries a `schema_version` field (currently
`1.0.0`) stamped at write time. Future format changes will bump it; readers
must skip unknown fields and refuse to deserialize a future MAJOR version.

---

## 4. Belief Lifecycle

This is the section that explains how beliefs form, get tested, get updated, and get retired. The underlying subsystem is called the **Epistemology Engine**, but the section title describes what it actually does: a lifecycle for beliefs, not an academic treatise on epistemology.

### 4.1 The cognitive sequence

The forward flow is a typical cognitive sequence — what the agent does over time, not the only possible flow:

```
Observation → Memory → Decision → Theory → Retrieval → New Observation
```

Notice what never happens. The original memory is never edited. Only confidence changes. That distinction allows historical reasoning to remain inspectable months later.

### 4.2 Evidence is cross-cutting

The cognitive sequence above shows how artifacts come into being. The relationship below shows how their truth gets refined over time. The two are different things; conflating them is what made the earlier single-diagram view misleading.

**`source_group` vocabulary (validated at write time).** Every evidence write (`mpm_evidence action=add`, `mpm evidence add`) validates `source_group` against the same registry the work verifier classifies with. Accepted values: outcome sources `filesystem`, `test`, `api_response`, `manual_review`; audit sources `git`, `ci`, `external`; action sources `tool_invocation`, `api_call`, `process`. Unknown values are rejected with an error enumerating the accepted set (discoverable via `mpm_evidence action=source_groups`) and leave no partial state. This closes the failure mode where unrecognized source groups were stored, silently ignored by the verifier, and permanently capped verification.


A piece of evidence can support or challenge **any** artifact — memory, decision, or theory — not just the most recently created one. The artifacts that have evidence attached accumulate that evidence over time. Decay reduces confidence over time. Retrieval surfaces artifacts; new observations restart the cycle.

```
                    Evidence
                       │
            ┌──────────┼──────────┐
            │          │          │
            ▼          ▼          ▼
         Memory    Decision    Theory
            │          │          │
            └──────────┼──────────┘
                       │
                       ▼
                  Confidence ◄── Decay
                       │
                       ▼
                  Retrieval
                       │
                       ▼
              New Observation
```

A memory can be challenged by a new observation. A decision can be challenged by a new test. A theory can be challenged by an independent reproduction. The evidence ledger doesn't care which kind of artifact it attaches to.

The forward flow is creation; the cross-cutting arrow is refinement. Both run continuously, and both are required for belief revision to work.

### 4.3 Decision Ledger

An append-only audit trail of architectural choices. Captures the context, the choice made, and the reasoning — so weeks later, the agent can reconstruct *why* a particular approach was taken instead of blindly second-guessing itself.

```bash
mpm record_decision "CONTEXT: We needed a CSS injection mechanism that survives wp_kses filtering
CHOICE: Route all widget CSS through agentshell_register_widget → wp_options → widgets.php <head> injection
RATIONALE: WordPress strips <style> blocks from post content via wp_kses_post() even for admins. The widget init JS also needs a footer injection point. Both requirements pointed to wp_options as the store."

mpm decisions
```

**Invalidation / supersession.** A corrected choice must be distinguishable from stale knowledge without deleting history:

```bash
# Record a replacement; the original is tagged superseded/superseded-by:<new-id>
# (HybridSearch discounts superseded rows to 0.25x score) and carries
# metadata superseded=true, superseded_by=<new-id>.
mpm call mpm_decisions --payload '{"action":"supersede","params":{"original_id":"<id>","choice":"<new choice>","rationale":"<why>"}}'

# Retire a decision with no replacement:
mpm call mpm_decisions --payload '{"action":"invalidate","params":{"decision_id":"<id>","reason":"<why>"}}'
```

Superseding an already-superseded decision is rejected — supersede the current reading instead. Every intermediate stays inspectable via its `superseded_by` pointer.

### 4.4 Theory Tracker

A hypothesis ledger for debugging and design. When the agent forms a causal assumption ("I think X is causing Y"), it logs the hypothesis and a concrete validation test before writing the fix. This forces the assumption to be testable, and often collapses a false hypothesis before it wastes an hour.

```bash
mpm propose_theory "HYPOTHESIS: passing --json before the positional arg causes the parse bug
VALIDATION_CRITERIA: write a unit test — invoke mpm with --json flag first vs positional-first, compare parse error rate
STATUS: pending"

mpm theories                    # List all theories with status chips
mpm theories pending            # Pending only

mpm resolve_theory abc123 "confirmed: flag order matters, --json consumed before positional processing"
```

### 4.5 Cognitive Immune System (Challenge Lifecycle)

Knowledge becomes obsolete. Most memory systems never address this problem. MPM introduces a challenge workflow:

```
Memory
    ↓
Challenge
    ↓
Theory
    ↓
Evidence Collection
    ↓
Confirmed / Disproven
```

When new evidence appears, a memory is challenged, a theory is created, evidence is gathered, the theory is resolved, and knowledge is updated. This is the mechanism by which the agent actively questions and overturns its own outdated knowledge.

**Challenge lifecycle (atomic transactions):**

```
challenge ─────────────────────────────────────────────────▶ [pending theory]
    │
    ├── challenge restore ──────────────────────────▶ [theory: disproven]
    │
    └── shred ─────────────────────────────────────▶ [theory: deleted]
```

- **`mpm challenge <id> "<evidence>"`** — atomic: patch memory metadata, create theory with back-link. The `mpm_memory action=challenge` tool additionally weakens the stored weight by 2 and records the pre-challenge value in metadata (`challenged_prior_weight`). A negative or inverted reduction is rejected outright — challenging knowledge can never increase its rank.
- **`mpm challenge restore <id>`** — atomic: resolve theory as disproven, clear memory flags, and restore the recorded pre-challenge weight so the memory returns to its exact prior epistemic standing.
- **`mpm shred <id>`** — atomic: cascade-delete memory + linked theory + topic memberships.
- **`mpm ops gc --shred-negative`** — shreds only memories with weight<0 AND a proven theory exists. Negative weight alone is never sufficient — the theory provides the evidence chain.

**Auto-resolution on memory save (the implicit path).** Theories can also be closed out without an explicit `mpm resolve_theory` call. When `mpm_memory action=save` writes a memory carrying the tags `theory:<id>` AND `outcome:proven` (or `outcome:disproven`), the theory is atomically resolved in the same transaction as the memory insert. The result envelope returns `theory_resolutions_applied: ["<id>"]` when this fires; the resolved theory row carries `resolved_by=mpm_memory action=save:theory_resolve_hook` as the forensic marker. This is the preferred path for closing out a theory you've just verified — the evidence and the conclusion land in the same row, and the audit trail is the row itself rather than a separate `mpm resolve_theory` invocation.

### 4.5.1 Save idempotency and bounded responses

**Duplicate saves are deterministic, not background-magic.** Identity for a memory is the tuple *(collection, SHA-256 of content, tags, stable metadata)* — write-path stamps (`created`, `timestamp`, `source`, tag mirrors) and observation telemetry (`_epistemic_snapshot`, `weight_intent`) are excluded. An identical re-save returns the **existing row's id** with `duplicate: true` instead of writing a second indistinguishable row; the same identity rule is enforced inside both low-level insert paths. Content that differs in provenance metadata (human vs model capture) remains legitimately distinct — that distinction feeds the contradiction workflows.

**Agent-facing payloads are bounded by design.** `mpm_memory save` echoes content up to 2 KB (override with `MPM_MAX_INLINE_CONTENT_BYTES`), flags larger echoes with `content_truncated: true` + `content_bytes`, and always returns an `mpm://memory/<id>` pointer; the full payload is persisted unchanged and retrievable by id or via `projection: "full"` (or `full_content: true` on the legacy `mode` alias). Memory query and show each honor the same `projection: "summary" | "full"` parameter — default `"summary"` keeps the wire echo bounded, `"full"` returns the complete stored body. Truncation is always explicit — a bounded echo can never be mistaken for complete content.

### 4.5.2 Memory lifecycle: delete vs shred, weaken floor, projection

The memory surface exposes four lifecycle verbs whose names and effects must stay distinct:

| Verb | Effect | Reversible? | Distinct from |
|---|---|---|---|
| `mpm memory delete <id>` (CLI) / `mpm_memory action=delete` (tool) | Soft-delete: sets `deleted_at`. Row stays in substrate with full content + history preserved. | **Yes** — `mpm memory restore <id>` / `mpm_memory action=restore` clears the tombstone and re-adds the FTS5 entry. | `shred` (permanent) |
| `mpm memory restore <id>` (CLI) / `mpm_memory action=restore` (tool) | Reverse a soft-delete. Idempotent on live rows (errors with "not soft-deleted"). | n/a | `mpm challenge restore` (only clears the `challenged` status flag, not the `deleted_at` tombstone) |
| `mpm memory shred <id>` (CLI) / `mpm_memory action=shred` (tool) | Permanent removal from active state. Broad sweep across topic_memberships, memory_revisions, evidence, confidence_history, artifact_provenance, synth_runs. | **No.** | `delete` (soft, reversible) |
| `mpm shred <id>` (CLI, no `memory` subcommand) | Same as `mpm memory shred` — hard delete with cascade. | **No.** | `delete` (soft, reversible) |

#### What "shred" guarantees — and what it does not

**`shred` means: remove the object from active MPM state and run the cascades defined for its type. It is not secure erasure, and MPM has no secure-erasure capability.**

The word "secure delete" is therefore not used for any `shred` surface. Two different operations in the repo carry the erasure-flavoured vocabulary, and they must not be conflated:

| Operation | Guarantee | Attempts byte overwrite? |
|---|---|---|
| `mpm … shred <id>` (per object, all types) | Hard DELETE from the active substrate + defined cascades, in one transaction. Not reachable again through MPM; not restorable. | **No.** |
| `mpm work item purge <id>` (CLI-only) | Logical purge from the active substrate. Explicitly *not* erasure (see "Purge is logical deletion, not erasure" under **Work**). | **No.** |
| `uninstall.sh --shred` | Best-effort secure overwrite of every regular file under the data roots, then removal. Self-disclosed as best-effort: SSD, CoW, snapshot, journaled and virtualised storage are not guaranteed to be physically erased. | **Yes, best-effort.** |

**Residual copies outside every `shred` guarantee.** After a per-object shred, the following may still contain the content and are deliberately *not* rewritten:

- `system_audit_log` rows (and the `epistemic_provenance` edges citing the dead id — pruned later by a periodic `gc` sweep).
- `mirror.jsonl` / `watchdog.jsonl`, including rotated `.gz` copies. A per-ID shred appends a content-free `memory_shredded` event; it does not rewrite history.
- Database backups and `mpm backup` dumps, which are separate files shred never opens.
- The SQLite WAL, and free pages in the main DB file. A deleted row's bytes remain until SQLite reuses the page. MPM issues no `PRAGMA secure_delete`, no `VACUUM`, and no `wal_checkpoint` on any shred path, so removal of the bytes is incidental to later page reuse and maintenance, never a shred guarantee.
- Filesystem snapshots, and any copy already transcribed into a handoff summary, another memory body, or an operator's own notes.

The distinction that matters operationally: **a shredded object is no longer reachable through MPM and is not restorable — it is not gone from the storage medium.**

**Weaken floor.** `mpm memory weaken <id>` / `mpm_memory action=weaken` uses the symmetric formula `weight_loss = (delta+1)/2`, decrements `reinforcement_count` by `delta`, and floors weight at **1** — repeated weaken calls can never drive weight negative or below 1. The response payload includes `weight_loss`, `reinforcement_delta`, `weight`, `reinforcement_count`, and `floor_hit: true` when the call landed at the floor.

**Projection semantics.** `projection: "summary"` (default on save/show / query) bounds the inline content echo and flags `content_truncated: true` for the larger-than-bound case. `projection: "full"` returns the complete stored body and clears the truncation markers. Projection is a wire-format choice — the stored content is **never** mutated by projection. Unknown projection values are rejected at the handler boundary with the canonical-list error `[summary, full]`.

**Save response.** The save response distinguishes persisted content from inline preview: `content` is the bounded echo, `content_bytes` is the stored size (truthful even when bounded), and `content_truncated: true` means only the *returned* representation was bounded — the stored memory is complete. Re-call `show` with `projection: "full"` (or resolve the `pointer`) for the unabridged body.

### 4.6 Proactive Recall

Traditional memory systems wait for a search query. MPM actively surfaces relevant knowledge before it is requested.

```bash
mpm kb hint "discussing the CSS injection approach for the widget system"
# → 💡 [Recall] You decided: Route all widget CSS through agentshell_register_widget...
#    RATIONALE: WordPress strips <style> blocks from post content...
```

FTS5 keyword extraction detects semantic overlap with the current context and pushes a low-latency recall hint — with STATUS and RATIONALE displayed directly, not just the content. The `mpm_context action=proactive_recall_hint` tool is wired into the OpenClaw agent loop and surfaces the most relevant epistemology memory automatically after context shifts.

### 4.7 What happens when MPM is wrong?

MPM models reasoning under uncertainty. Mistakes happen. The system is designed around explicit recovery — every bad outcome has a paper trail and a documented path back. Nothing is silently overwritten.

Six canonical failure modes and the mechanism that handles each:

**Bad evidence corrupting a decision.** A memory was anchored to evidence that turned out to be misread, fabricated, or context-dependent; the decision now rests on a false foundation. *Recovery:* `mpm challenge <id> "<why this is wrong>"` atomically creates a back-linked theory in pending status and preserves the original artifact; the tool surface (`mpm_memory action=challenge`) also weakens the stored weight by 2, recording the pre-challenge value for exact restoration. The memory is not deleted — the original artifact is preserved. A future operator can audit *why* the memory was believed, *when* it was challenged, and *what* eventually resolved the dispute.

**Premature theory confirmation.** A theory was marked confirmed with thin evidence, or new evidence has since emerged that contradicts it. *Recovery:* the challenge lifecycle is non-monotonic. A confirmed theory can be challenged again, re-entering the evidence-collection state with a fresh back-link. There is no "settled science" path — theories are revisable for the lifetime of the database.

**Conflicting shared rules.** Agent A writes `Always use snake_case for DB columns`; Agent B writes `Always use camelCase for DB columns`. *Recovery:* every shared write produces a near-miss scan against existing rules via `shared.contradiction_log`. Detection runs opportunistically inside `mpm_memory action=query` — no separate ticker. Near-misses carry a protobuf-shape score combining confidence, freshness, and reinforcement. The operator is presented with both rules and chooses via `ResolveArbitrationTheory`; MPM does not silently overwrite either side.

**Confidence decay drift.** A memory's weight decayed to a wrong value because the lifecycle sweep ran in an unexpected state, or because the underlying evidence ledger had a bug. *Recovery:* §10.1 Self-Healing Integrity Loop. Concept-drift detection quarantines affected memories (`concept_drift: true`), proposes pending theories, and survives restarts via SQLite-native dedup. The system errs on the side of *escalating to a theory* rather than silently fixing — wrong weight corrections are far more dangerous than visible ones.

**Embedding model change.** Operator upgrades from one embedding model to another; cosine similarities shift; "similar to X" links become noisy. *Recovery:* this is the hardest case. Mitigations: (1) BM25 keyword search is independent of embeddings, so the recall path still works for exact-term queries; (2) embedding backfill is a separate phase (`mpm ops backfill-embeddings`) and can be re-run on demand; (3) the hybrid score weights keyword ranking alongside semantic recall, so semantic noise degrades gracefully rather than breaking recall outright. There is no automatic re-embedding-on-model-change — this remains an operator-driven migration.

**Dependency collapse via cascade.** A foundational memory (or rule) was disproven or shredded, and downstream decisions or theories that depended on it now rest on a false foundation. A naïve system would silently leave the downstream artifacts holding outdated context. *Recovery:* §4.8 Epistemic Cascades. Invalidating a foundation atomically enqueues an `epistemic_cascade_outbox` intent for every downstream decision or theory (lessons and global rules are deliberately excluded). `mpm cascade materialize` then drains the outbox into pending theories challenging the downstream artifacts — depth-capped at 3, with a CRITICAL audit at depth 4. The cascade envelope is durable; a crash mid-materialize leaves the outbox intact for the next run.

The common pattern: every recovery path leaves a theory, a log entry, or a `concept_drift` flag. History is the system.

### 4.8 Epistemic Cascades

*The upstream-side complement to the challenge lifecycle: when a foundation collapses, every downstream artifact that depended on it gets a pending theory.*

The challenge lifecycle (§4.5) handles a single artifact at a time: one memory is challenged, one theory is created. But reasoning is a graph. A foundational memory underpins many decisions and theories; a decision rests on memories that may themselves be invalidated later. When a load-bearing artifact collapses, the downstream artifacts that *referenced it* through provenance edges (`source_ids`, `trigger_evidence_id`) become stale — but they don't know it yet.

**Epistemic cascades** solve this by propagating invalidations. When a foundation is disproven or shredded, the substrate autonomously spawns pending theories challenging every downstream decision or theory that depended on it. The downstream artifacts are not edited — they get a back-linked theory in pending status, exactly like a manual `mpm challenge`, and the operator (or the next agent) decides what to do with each one.

**What's in scope, and what isn't.**

| In scope (cascaded) | Out of scope (not cascaded) | Why |
|---|---|---|
| Decisions | Lessons | Lessons are reusable cross-task knowledge; they outlive the artifact that inspired them. Stale-flagging a lesson on a foundation collapse would be a regression — the lesson may still apply to other contexts. |
| Theories | Global rules | House rules are operator-gated and frequently cross-project. Silently invalidating a rule from a downstream artifact would bypass the `record_global_rule` operator gate. |

**The architecture: transactional outbox + bounded materializer.**

The cascade is split into two phases because the invalidation path is hot (every challenge could trigger a cascade) and the materialization path is cold (creating theories is LLM+DB work).

```
  Invalidation event                  Materialization
  ─────────────────                   ──────────────
  foundation disproven / shredded
        │
        ▼
  SELECT descendants      ◀── atomic ──▶  mpm cascade materialize
  via provenance edges                                                │
        │                                                            │
        ▼                                                            ▼
  INSERT epistemic_cascade_outbox                             claim → process → retry
  (status='pending')                                       (depth ≤ 3, audit at 4)
        │
        ▼
  return to caller in <1ms
```

The invalidation transaction writes the intent (`status='pending'`) and returns within the same atomic transaction as the trigger. Materialization is asynchronous: `cascade_drain` runs as a registered tick handler on `mpm-scheduler`, claiming and draining pending intents under a per-tick wall-clock budget. There is no hidden per-CLI background drain and no latency tax on the write path. `mpm cascade materialize` is the foreground escape hatch for one-shot drains and post-incident catch-up — see [Appendix D.2](#d2-how-the-outbox-gets-drained).

**Depth limit and the depth-4 audit.**

The cascade recurses: invalidating a downstream artifact may itself be a foundation for further artifacts. The depth is capped at **3** (`MaxCascadeDepth`) — beyond that, the substrate still records the intent but appends a CRITICAL audit entry and suppresses the spawn, preventing pathological fan-out from a single root invalidation. The audit log carries the chain so an operator can examine what was suppressed and decide whether to widen the cap.

**The wake throttle.**

When cascade intents materialize into pending theories, the change must surface to the agent's wake context. `check_wakes` caps **cascade-kind wakes at 3 per call** (`MaxCascadeWakePerCheck`) — so a 50-intent cascade materialization doesn't flood the agent on the next call. Non-cascade wakes (notification, cron, system) are interleaved normally and unaffected by the cap; the throttle is per-call, not global.

**Why a scheduler tick, not a standalone daemon.** The watch daemon was deprecated in commit `6588cb8` and hard-removed in `215fd09`. The drain now rides the scheduler you are already running for wakes and system tasks, rather than adding a second moving part — and when you are not running the scheduler at all, `mpm cascade materialize` drains the queue in the foreground and exits.

**Inspecting dead letters.**

Reasons a cascade intent might end up dead-lettered (`status='failed'`): the scanner rejected the synthesized theory, the FTS5 insert failed, the SQLite write was retried past `MaxRetries` (default 3), or the intent exceeded the depth cap. Inspect with `mpm cascade list-dead-letters` — both the failed intent's `terminal_error` and the outbox summary (`pending / processing / materialized / failed`) are surfaced for the operator.

```
mpm cascade materialize            # drain the outbox (default: until empty)
mpm cascade materialize --once     # process one batch and exit
mpm cascade list-dead-letters      # show failed intents and outbox summary
```

The outbox is durable; a crash mid-materialize leaves the row with `status='processing'` and the next materialization reclaims it via stale-recovery (`updated_at` filter), preserving `attempt_count` so a poison pill still dead-letters instead of retrying forever.

Cascades also run in a **positive** direction: when a foundation crosses the proven threshold, downstream artifacts that explicitly declared they *assume the foundation is false* are surfaced for re-evaluation too. That opt-in is explicit and never inferred. The outbox schema, the queries, every tunable constant, the polarity contract, and the troubleshooting path are in [Appendix D](#appendix-d-epistemic-cascades--operator-reference).

---

## 5. Quick Start

Five minutes from zero to first decision. Choose your depth:

- **§5.1 Try it** — CLI only, no daemons. Best for evaluating MPM or one-off scripting.
- **§5.2 Run it as a daemon** — full autonomous operation: opportunistic wake dispatch, system-kind scheduled tasks (critic audits, snapshots, GC, broadcasts), and machine-to-machine integration via MCP.
- **§5.3 First commands** — the cognitive loop in eight lines.

### 5.1 Try it (CLI only — no daemons)

```bash
git clone https://github.com/flowbyte-com/mpm ~/.mpm
cd ~/.mpm
make build           # produces bin/mpm, bin/mpm-mcp, bin/mpm-scheduler, bin/mpm-critic, bin/mpm-telemetry
```

`~/.mpm` is the canonical root: the same directory is the Git checkout and the runtime root, and everything MPM writes there at runtime is gitignored, so a normal `git pull` updates code without touching your data.

The single binary lives at `bin/mpm`. Try it without installing anything — no daemon setup, no service registration, no config files. (`make install` is optional; it verifies/syncs all five binaries to `$HOME/.mpm/bin` — the canonical install prefix. For a full systemd + OpenClaw install, run `./install.sh` — the canonical path. The companion daemons `mpm-mcp` and `mpm-scheduler` install together when you want autonomous operation — see §5.2.)

### 5.2 Run it as a daemon

This section shows the daemon + systemd setup manually, for transparency and for operators who want to customize individual steps. If you don't need that control, run `./install.sh` instead — it does most of the below (build, install to `~/.mpm/bin`, `make service-scheduler`, `systemctl --user enable --now`, the eCryptfs autostart workaround, and OpenClaw wiring) in one idempotent step. `mpm ops init directives` (see the "Seed the baseline cognitive directives" step in [§5.2](#52-run-it-as-a-daemon)) is **not** part of `install.sh` — it is a separate post-install command by design (the installer prints it as a `next steps` hint at the end). `make service-telemetry` likewise is a separate manual step. Use the manual steps below when you need to pin a specific version, point a unit at a non-canonical install path, or otherwise deviate from the canonical layout.

For autonomous operation — the scheduler dispatches system-kind wakes (critic audits, snapshots, GC, broadcasts) on a 60s ticker, and `mpm-mcp` exposes MPM to MCP hosts (Claude Code, OpenClaw) over stdio:

```bash
git clone https://github.com/flowbyte-com/mpm ~/.mpm
cd ~/.mpm
make build           # produces bin/mpm, bin/mpm-mcp, bin/mpm-scheduler, bin/mpm-critic, bin/mpm-telemetry
make install         # optional — verifies/syncs all five to $HOME/.mpm/bin (no sudo)
```

**Install the scheduler as a systemd user service:**

```bash
make service-scheduler                              # copies unit to ~/.config/systemd/user/
systemctl --user daemon-reload                      # (make service-scheduler already does this)
systemctl --user enable --now mpm-scheduler         # enable + start
systemctl --user status mpm-scheduler               # verify
journalctl --user -u mpm-scheduler -f               # follow logs

make service-telemetry                             # copies telemetry unit to ~/.config/systemd/user/
mkdir -p ~/.mpm/run
systemctl --user enable --now mpm-telemetry         # enable + start
systemctl --user status mpm-telemetry               # verify
journalctl --user -u mpm-telemetry -f              # follow logs
```

> **eCryptfs encrypted-home autostart workaround.** When `/home` is eCryptfs-encrypted
> AND `loginctl enable-linger` is set, the user manager boots at ~08:01 (before
> PAM unwraps eCryptfs on the first login at ~08:17). Unit files inside the
> encrypted tree are invisible at that point — `default.target` is reached before
> `Wants=mpm-scheduler.service` can be evaluated, so `Restart=` does not help.
> The fix is a `~/.config/autostart/mpm-post-decrypt.desktop` entry that runs
> `systemctl --user daemon-reload && systemctl --user start mpm-scheduler.service`
> on every graphical login (post-decrypt). `install.sh` detects this case
> via `mount` + `findmnt` + the `/home/.ecryptfs/$USER` marker and writes the
> autostart entry automatically; `uninstall.sh` removes it.
>
> Operators on systems without an agent wake path (cron-driven unattended tasks,
> headless deployments) can opt out by removing the autostart entry and instead
> adding `ExecStartPre=/bin/bash -c 'until mountpoint -q $HOME; do sleep 1; done'`
> to `mpm-scheduler.service` via `systemctl --user edit`.

The default unit targets the canonical layout — `WorkingDirectory=%h/.mpm`,
`ExecStart=%h/.mpm/bin/mpm-scheduler`, and `MPM_WORKSPACE=%h/.mpm`, with
`%h/.mpm` as the only writable path. To point a unit somewhere else, override
via either:

- **Drop-in** (preferred for path changes): `systemctl --user edit mpm-scheduler`
- **Env file** (preferred for DB / backup paths): write `~/.config/mpm/mpm.env` with `MPM_DB_PATH=...`, `MPM_BACKUP_DIR=...`, `MPM_CRITIC_BIN=...` — it's sourced as `EnvironmentFile=-` in the unit.

**Seed the baseline cognitive directives** (recommended once after install):

```bash
mpm ops init directives
```

MPM provides advanced cognitive machinery — wake-context surfacing, audit-cluster detection, session-end triage — but that machinery requires specific agent behaviors to close the loop. These seed directives are the **Baseline Cognitive Bootstrap**: the "batteries-included" operating manual for that machinery. Without them, the scheduler will dispatch wakes but the cognitive loop won't close. Idempotent — safe to re-run; local edits are preserved.

**Wire up the MCP host** (Claude Code, OpenClaw, etc.). `mpm-mcp` is spawned by the host as a stdio child process — no separate service to manage. Example `.mcp.json`:

```json
{
  "mcpServers": {
    "mpm": {
      "command": "/path/to/mpm/bin/mpm-mcp",
      "env": {
        "MPM_DB_PATH": "/path/to/mpm/src/db/mpm.db"
      }
    }
  }
}
```

### 5.3 First commands

```bash
# Store
mpm add Germany leads Group E with +6 goal differential

# Retrieve
mpm World Cup prediction
mpm recall --semantic "为什么德国队表现这么好"

# Decide
mpm record_decision "CONTEXT: ...
CHOICE: ...
RATIONALE: ..."

# Theorize
mpm propose_theory "HYPOTHESIS: ...
VALIDATION_CRITERIA: ...
STATUS: pending"

# Challenge
mpm challenge <id> "recent data contradicts this"

# Status
mpm status
mpm ops stats
mpm wake               # last session context

# Swap the LLM provider — e.g. OpenRouter (any model, including the free tier):
mpm config profile add router --model openai/gpt-4o-mini --base-url https://openrouter.ai/api/v1
mpm config profile set router provider openrouter
mpm config profile set router api_key                     # hidden prompt (no echo)
                                                        # or: printf '%s' "$KEY" | mpm config profile set router api_key --stdin
mpm config component set memory router                   # bind memory writes to the new profile
                                                        # (auth header + endpoint are inferred from base_url)

# Free tier is just a model suffix — `meta-llama/llama-3.3-70b-instruct:free`:
mpm config profile set router model meta-llama/llama-3.3-70b-instruct:free
```

Done. That's the cognitive loop: observe, decide, theorize, challenge, and (with the bootstrap) keep reasoning alive across sessions and vacations. The rest of this document explains how each piece works and how to operate the system at scale.

### 5.4 Customize your agent's core files

The cognitive machinery in §5.2 runs without customization. But the agent
that runs it is generic — it doesn't know who you are, what you prefer, or
how you like to work. The workspace files at `~/.openclaw/workspace/` turn
a generic assistant into one that's yours.

| File | What it shapes | When to edit |
|---|---|---|
| `IDENTITY.md` | Name, sigil, color, emoji — who the agent _is_ | During bootstrap; revisit when the persona evolves |
| `SOUL.md` | Persona, tone, boundaries, vibe — how the agent _behaves_ | During bootstrap; refine as patterns emerge |
| `AGENTS.md` | Operating instructions, memory workflow, red lines | As the workflow stabilizes — this is the agent's operating manual |
| `USER.md` | Who you are, how to address you, your preferences | Once you know what the agent should remember about you |
| `TOOLS.md` | Local tool conventions (cameras, SSH hosts, TTS voices, etc.) | When you start using tools the agent doesn't know about |
| `HEARTBEAT.md` | Periodic check tasks (or empty to skip heartbeats) | When you want scheduled checks |
| `MEMORY.md` | Curated long-term memory — durable facts, preferences, decisions | Continuously, as the agent learns things worth keeping |
| `memory/YYYY-MM-DD.md` | Daily logs of what happened | Daily, raw session notes |

**Start simple. Iterate as the agent does work.** You don't need to fill
these in upfront — they co-evolve with the agent over sessions:

1. Run the agent for a week on default files.
2. Notice where it's _generic_ (no opinions, no preferences, no knowledge of you).
3. Edit the relevant file to capture the gap.
4. Repeat.

**SOUL.md is the highest-leverage file.** Loaded every session, sets the tone.
See the [SOUL.md personality guide](https://docs.openclaw.ai/concepts/soul)
for what good ones look like. Defaults are friendly-but-generic; an
opinionated SOUL.md (with stance, no preamble, sharp entrances) is the
difference between a chatbot and an assistant that feels like a colleague.

**Three memory layers, three different jobs.** The full agent stack has:

- **Workspace files** — who the agent _is_ (OpenClaw-side, markdown in
  `~/.openclaw/workspace/`)
- **MPM prime directives** — how MPM's machinery hooks in (DB-side, rows in
  the `directives` table, seeded by `mpm ops init directives`)
- **MPM memories** — what the agent _believes_ (DB-side,
  `mpm_memory action=save` / `mpm_memory action=query`, scales to thousands of rows)

Use `MEMORY.md` for "who am I and what do I know about my user". Use MPM
directives for "how does MPM's machinery actually fire". Use MPM memories
for "what does the agent believe, why, and with what evidence". All three
required for the closed cognitive loop.

---

> **From here on, the document describes how the system is built.** §1–§5 cover what MPM is, why it isn't a memory system, the cognitive model, the belief lifecycle, and how to operate it. The remaining sections and appendices explain how it works — for readers writing patches or evaluating the architecture.

## 6. System Architecture

MPM intentionally separates persistent cognition from runtime behaviour.

### 6.1 Core vs Runtime

*The architectural boundary between what the agent knows (stable) and how it behaves (evolvable).*

```
                    MPM
                     │
        ┌────────────┴────────────┐
        │                         │
        ▼                         ▼

    CORE                    RUNTIME

    Memories                Wake Context
    Decisions               Scheduling
    Theories                Personas
    Lessons                 Modes
    Evidence                Directives
    Confidence              Routing
                            Session Handoffs
```

The distinction is important. The Core describes what the agent knows. The Runtime describes how the agent behaves. Runtime systems may evolve. The Core should change very slowly.

Mature systems often owe their longevity to having a very small, stable core. Every feature that lives in Core must earn its place through years of usage evidence, not through the effort it took to build. Features that fail to justify themselves are removed. Engineers are sentimental about code; the regret log and disciplined review break that sentiment.

#### Execution Profile Routing

The substrate's LLM consumption is mediated through a five-layer routing chain:

```
   Skill              says 'reviewer'    (portable, install-agnostic)
     ↓
   Capability         says 'critic'      (operator-bound)
     ↓
   Component          says 'review profile' (substrate function)
     ↓
   Profile            says 'gpt-4o, tmp 0.2' (provider + params)
     ↓
   Provider                                (openai / ollama / ...)
```

**Motivating principle:** different cognitive functions have different model requirements. Memory synthesis wants strong reasoning and long context; the critic wants analytical and cheap; the scheduler wants cheap and reliable. One model doesn't fit every cognitive task, so operators configure each profile to be the model that fits its role.

**Why this matters:** skills are portable across installs. A skill references the capability `reviewer`; the install decides whether `reviewer` maps to `critic` (the substrate's term) or to an operator-named component. The runtime does the lookup. No code changes when operators rename their components.

The execution-profile abstraction is the substrate's primitive for this routing:

- `Config.ProfileFor(component)` — resolves a component name to a Profile.
- `Config.CapabilityFor(name)` — resolves a capability name to a component name.

The CLI surface (`mpm config profile|component|capability`) lets operators configure the routing without writing code. The MCP surface (`mpm call mpm_request_review ...`) lets agents invoke multi-component reviews against the same routing — the substrate's first orchestration primitive.

**No hard-coded component names.** Nothing in the substrate, and nothing in a skill, may name an operator's component directly. A skill names a *capability*; the install maps that capability to a component; the component maps to a profile; the profile names the provider and model. Every layer of that chain is resolved at runtime, which is what makes a skill portable across installs that named their components differently.

The command surface built on top of this routing — the two CLI personalities, the layered Stores → Services → Formatters → Encoders → Renderers → Commands stack, and the help taxonomy — is specified in [Appendix F](#appendix-f-cli--command-architecture).

### 6.2 Confidence Engine

*How the system derives belief from evidence rather than arbitrary LLM scoring.*

The Confidence Engine computes and tracks the system's belief in each artifact. The principles are stated in §3.3. This section is the **shape** — the evidence registry, the source-of-truth split, and the operations the agent can call. The mechanics (recompute atomicity, trigger wiring, transaction boundaries) live in Appendix C.

#### Evidence Registry

| Type | Default strength | Meaning |
|---|---|---|
| `observation` | 0.4 | Direct observation of an event |
| `test` | 0.7 | Result of an automated test |
| `reproduction` | 0.85 | Independently reproduced result |
| `challenge` | −0.6 | Evidence against an artifact |
| `decision_outcome` | 0.95 | Outcome of a recorded decision |
| `external_reference` | 0.6 | Cited source from outside the agent |

Override the default with `--strength`. Artifact types are a strict enum — the LLM cannot invent categories.

Initial confidence is also fixed by artifact type:

| Artifact type | Initial |
|---|---|
| memory | 0.8 |
| theory | 0.5 |
| decision | 0.6 |
| lesson | 0.7 |

#### Source of truth + cache

The stored `confidence` column is a performance cache. The evidence ledger is the source of truth. If they disagree, the cache is wrong. `mpm ops confidence recompute` triggers a manual recompute; `mpm_confidence action=query_history` derives its view from evidence, not the cached column.

This is one of the four enforcement patterns that make MPM's guarantees stick. See **Appendix C** for the rest (property tests, AST guard rails, self-heal whitelist + escalation).

### 6.3 Retrieval Architecture

*Combines lexical and semantic signals into a single ranking — because no single signal is sufficient.*

MPM combines two retrieval signals:

- **Keyword ranking** — SQLite FTS5 with BM25.
- **Semantic similarity** — 768-dim embeddings, cosine distance.

The hybrid score is a weighted blend of BM25 and cosine similarity:

```
combined_score = vector_weight × normalized_cosine + (1 - vector_weight) × sigmoid(BM25)
```

with `vector_weight = 0.5` by default. Use `--semantic` to drop BM25 and search by embedding similarity alone.

**What the current ranker does NOT consume.** `weight`, `reinforcement_count`, and `last_accessed_at` are stored on every memory but **do not influence ranking order** today. They are surfaced in the projected payload so agents can reason about provenance ("this memory was reinforced 4 times") and in `formatRationaleForMemory` to render the human-readable `weight N · Mx ref` string, but they are not part of `combined_score`. The `DefaultRanker` returns the FTS / hybrid score unchanged — see `internal/core/hybrid_search.go::HybridSearch`. Future rankers can consult `retrieval_metadata` to blend a reuse-adjusted score; today this is aspirational.

> **If you want a memory to surface higher, write better content.** Lexical match and semantic similarity respond to the words you put in. `mpm reinforce` increases the displayed `reinforcement_count` but does not change retrieval order.

> **FTS5 tokenization contract.** The `lessons_fts` and `memories_fts` indexes use SQLite's FTS5 with the `porter unicode61` tokenizer (English stemming, ASCII case-folding). Hyphens, underscores, and dots are SPLIT — `"lazy-start"` becomes two tokens `lazy` and `start`. Queries are auto-expanded with prefix wildcards per token (`lazy* AND start*`), so the FTS5 contract is implicit-AND across all tokens. FTS5 special characters (`"`, `(`, `)`, `*`, `+`, `-`, `:`) are stripped from query input; agents querying MPM should pass natural-language query strings rather than raw FTS5 syntax. The contract is enforced in `internal/core/fts5_query.go::BuildFTS5Query` and taught in the `mpm_lessons action=query` / `mpm_memory action=query` tool descriptions so the agent doesn't have to memorise the tokenizer's quirks.

> **Implementation note:** The hybrid scoring function lives in `internal/core/hybrid_search.go`. The default embedding model is `nomic-embed-text` (768-dim), served by a **locally running Ollama instance** as the supported embedding inference provider. Ollama performs inference and returns vectors to MPM — the vectors are then persisted in the same SQLite database (alongside the memories they describe), partitioned into Voronoi cells by the shared IVF index (§6.5 Layer 1). Ollama is an inference dependency, not a separate persistence substrate or vector database; the configured embedding component can also be set to `"disabled"` (NullProvider) when semantic recall is not required.

**Memory provenance (`mpm recall --why`):** every result can be annotated with the score breakdown that retrieved it. Pass `--why` to see per-result `[why]` lines showing the FTS5 terms that matched, the cosine similarity (when vector search contributed), and the stored weight / reinforcement_count metadata. The metadata fields are surfaced for **provenance / display only** — they are not part of the ranking score, so changes in `reinforcement_count` do not move a memory up or down the list. Useful for "why did the agent pick this memory?" introspection without re-running the search.

> **Retrieval ranking determines what is surfaced. Confidence determines what is believed.** A frequently retrieved artifact is not necessarily a trusted artifact.
>
> The hybrid score above ranks by relevance; it says nothing about how strongly the system believes the surfaced artifact. Confidence is derived from the evidence ledger (§6.2) and evolves through the challenge lifecycle (§4.5). An artifact can be top of the recall list with low confidence, or low in the list with high confidence — both states are common and correct. Conflating the two axes is the most common read of retrieval output; it is also wrong.

**LTM promotion:** `weight ≥ 10` OR explicit `mpm promote` OR auto-ingested `.md` file.

#### Concept Drift Detection

Concept drift detection — autonomously identifying paradigm shifts where historically trusted knowledge is decaying under a sudden barrage of new counter-evidence — is implemented as a **synchronous** operation that runs inline with `mpm_memory action=query` (no background ticker). The detection signature:

| Signal | Threshold |
|---|---|
| Baseline | Artifact achieved high status historically (`peak_confidence >= 0.85`) |
| Velocity | ≥ 2 recent challenges or negative-strength observations within the last 7 days |
| Delta | Confidence has dropped ≥ 0.25 and sits below the distrust threshold (`< 0.60`) |
| Lifetime | ≥ 3 total lifetime negative evidence pieces (prevents one-off flukes) |

When a drifting memory triggers this signature, the engine quarantines the memory (sets `concept_drift: true`), proposes a pending theory, and survives restarts via SQLite-native dedup. Drift detection is pure-SQLite — no separate process, no separate timer, no panic-recovery surface to maintain. A drift missed last query is just as catchable next query.

### 6.4 MCP Integration

*Bridges JSON-RPC from any MCP-aware host (Claude Code, OpenClaw, Hermes — and Pi/OpenCode via their typed subprocess adapters) to the CoreDB contract — agents see tools, not SQL.*

MPM integrates directly with AI agents as a **single MCP server**. The Go binary (`bin/mpm-mcp`) is the only substrate; agents connect to it via MCP and receive the full MPM tool surface as native function calls. No plugin layer, no Node/TypeScript wrapper, no Python shim — one binary speaking MCP.

The migration from the legacy plugin model (per-agent TypeScript wrappers calling the CLI binary via `child_process`) to the current MCP server model happened in 2026-06-23. The deleted plugin folders and their associated TypeScript sources are gone. The MCP server is the only integration surface for MCP-aware clients.

For a step-by-step recipe for connecting Claude Code to MPM — including the SessionStart hook, `MPM_PROVENANCE_*` environment variables, and the `read_wake_context --format=system-prompt` injection path — see `agent_installation/mpm-claude-code/CLAUDE_CODE_INTEGRATION.md`.

#### MCP tool surface

The MCP server exposes the full MPM substrate as native function calls. Adding a new tool is a single Go function — no plugin path, no shell wrapper, no parallel documentation.

The **default model-facing surface** is the compact 3-tool set: `mpm_memory`, `mpm_context`, `mpm_help` — **intentionally fixed**, not derived. Hosts receive this on session start; the model sees a small, stable surface that covers the daily cognitive workflow (memorise / recall / skill lookup / wake refresh / handoff / scratchpad / directive management). The block below lists the **Registry entries** — the internal substrate; the count is derived dynamically from the live Registry and grows or shrinks as tools are added or removed. The full registered MCP surface includes those Registry entries plus the `mpm_help` discovery closure (registered at server init by `cmd/mpm-mcp`); the on-wire tool count is `len(Registry) + 1` and is **not** pinned in prose. The agent discovers specialist tools on demand via `mpm_help`'s `list`/`show` actions. Tools filtered out of the default `tools/list` are NOT directly callable over the compact MCP transport; reach them through `mpm call <tool> --payload '...'` (CLI fallback, every host) or by setting `MPM_EXPOSE_ALL_TOOLS=1` on the MCP env block to restore the full registered MCP surface natively.

```
<!-- tools:begin — auto-generated by `go generate ./internal/tools/`. Do not edit by hand. -->
mpm_memory               mpm_theories             mpm_decisions
mpm_lessons              mpm_topics               mpm_references
mpm_evidence             mpm_confidence           mpm_retrieval_diagnose
mpm_context              mpm_skills               mpm_wakes
mpm_handoff              mpm_scratchpad           mpm_system
mpm_log_to_changelog     mpm_request_review       mpm_resolve
mpm_blob_read            mpm_blob_search          mpm_work
<!-- tools:end — auto-generated by `go generate ./internal/tools/`. Do not edit by hand. -->
```

Wiring a new agent: add `mpm-mcp` to its MCP server config (OpenClaw: `mcp.servers.mpm` in `openclaw.json`; Claude Code: `.mcp.json`; any other MCP-aware client). The server binary is at `bin/mpm-mcp` relative to the MPM repo root.

#### Supported integrations at a glance

MPM ships first-party integration adapters for five agent hosts. Each one delivers MPM wake context before the first model turn — using whatever lifecycle mechanism the host exposes — except where noted:

| Host | Mechanism | Wake delivery |
|---|---|---|
| **Claude Code** | MCP server + SessionStart hook | Automatic (host invokes `mpm_context` action `read_wake_context` and injects the result as `hookSpecificOutput.additionalContext`) |
| **OpenClaw** | MCP server + typed `session_start` → `agent_turn_prepare` plugin | Automatic (`prependContext` on the first turn) |
| **OpenCode** | TypeScript plugin (typed tools) + `experimental.chat.system.transform` | Automatic (push into the system prompt before the first model call) |
| **Pi** | TypeScript extension (typed tools) + `pi.on("session_start")` → `pi.on("before_agent_start")` | Automatic (concatenated onto the system prompt once per session) |
| **Hermes** | MCP server (no host plugin layer) | **Manual** — Hermes has no session-start hook; the agent calls `mcp__mpm__mpm_context` action `read_wake_context` once at the start of its first turn |

The mechanism details, installation steps, and host-specific constraints live in each adapter's directory under `agent_installation/`. Compact handoff uses `mpm_context` action `write_handoff` on every host; the substrate `mpm_handoff` tool remains reachable via `mpm call mpm_handoff --payload '…'` when the MCP transport is unavailable or `MPM_EXPOSE_ALL_TOOLS=1` is set.

#### Single source of truth: `internal/core/tools/registry.go`

Both the CLI (`mpm call <tool>`) and the MCP server iterate the same registry — a package-level `[]Tool` slice in `internal/core/tools/registry_list.go`. Each entry holds:

- `Name` — the tool identifier (used by both surfaces)
- `Description` — short prose shown to MCP clients
- `Schema` — JSON-Schema (raw bytes, parseable by both surfaces)
- `Handler` — `func(dm *DatabaseManager, ac *ActiveContext, payload map[string]interface{}) (interface{}, error)`. Same function called by both surfaces.

Adding a new tool: write `handleFoo` in `internal/core/tools/handlers.go` (one function), append a `Tool{...}` entry in `internal/core/tools/registry_list.go`. Both the CLI dispatcher and the MCP server pick it up automatically.

#### Partial results on the error path

A handler may return **both** a structured result and a non-nil error. `mpm_system action=compact` does this on a mid-drain batch failure, returning the full drain result — `batches_processed`, `raw_processed`, `lessons_created`, `raw_remaining`, `actionable_pending`, `semantic_stages_spent`, `rows_deferred`, `stop_reason`, `failed_batch`, `failure_reason` — alongside the error. That result is the only way a caller learns *which* batch failed and *how much* work already committed; earlier batches are durable and must not be re-attempted. The same projection is used on the success path and the failure path, so a field is never missing precisely when a caller most needs it.

Both transports preserve it:

- **CLI** (`mpm call`): the stdout envelope merges the diagnostic fields alongside `{"success":false,"error":…}`. The envelope stays a single line of parseable JSON, and the exit code stays `1`.
- **MCP**: the tool result stays `IsError: true` and its first content block remains the `"<tool> failed: <err>"` text. The structured diagnostic is **appended as an additional content block** under a `partial_result` key.

Two invariants hold on both surfaces:

1. **A failure is never reported as a success.** `success` is forced to `false` and `error` to the Go error text *after* the partial result is merged, so a handler cannot launder a failure into a success by setting those fields in its own payload. MCP likewise never returns a successful result merely to carry metadata.
2. **No result means no change.** A handler returning `(nil, err)` — the common case — produces exactly the pre-existing envelope. Nothing is added to the response.

Because the rule lives in the transport rather than in any one handler, it applies to every action that returns a partial result, not only `compact`.

#### MCP/CLI parity: what is exposed via both surfaces

Every `mpm call <tool>` entry has a matching MCP tool spec; both call the same `CoreDB` methods. The full Registry has **21 entries** (the MCP surface is 22 — those 21 plus the `mpm_help` discovery closure registered at server init by `cmd/mpm-mcp`). Of those 21:

- **15 aggregator tools** (`{action, params}` shape): `mpm_memory`, `mpm_theories`, `mpm_decisions`, `mpm_lessons`, `mpm_topics`, `mpm_references`, `mpm_evidence`, `mpm_confidence`, `mpm_context`, `mpm_skills`, `mpm_wakes`, `mpm_handoff`, `mpm_scratchpad`, `mpm_system`, `mpm_work`. These cover the daily cognitive workflow: read/write memory and the memory feedback loop (`mpm_memory` actions `save`/`query`/`shred`/`reinforce`/`weaken`/`snooze`/`set_weight`/`patch`/`promote`/`review`/`synthesize`/`challenge`/`commit_milestone`), lessons, topics, references, theories, decisions, evidence, confidence, route + wake + directives, cross-session handoff (`mpm_handoff` actions `write`/`read`/`list`/`shred`), intra-session scratchpad (`mpm_scratchpad` actions `flush`/`read`/`discard`/`promote`), work tracking (`mpm_work` — same Fat-RPC pattern), and system maintenance + health.
- **6 standalone tools** (each with its own narrower schema): `mpm_retrieval_diagnose` (per-node BM25 diagnostic with 3-stage trace; CLI form `mpm call explain_retrieval`), `mpm_resolve` (resolves `mpm://memory/<id>` / `mpm://lesson/<id>` / `mpm://theory/<id>` / `mpm://work/<id>` URI pointers with a default 512-byte cap, caller-controllable via `opts.MaxBytes`), `mpm_blob_read` (raw blob fetch by id, 256 KiB server ceiling), `mpm_blob_search` (pointer search across the blobstore), `mpm_log_to_changelog` (self-report agent work tied to a git commit SHA), and `mpm_request_review` (concurrent multi-component review).

A handful of CLI commands are intentionally **NOT** exposed via MCP/call because they are operationally distinct (destructive, cron-friendly, or human-gated):

| CLI command | Why CLI-only |
|---|---|
| `mpm ops doctor` | Diagnostic report; agents don't need to run it, scheduled cron does |
| `mpm ops lint` | Pre-commit / CI hook; agents should never invoke regex validation |
| `mpm ops backfill-embeddings` | Bulk operation; runs on a schedule, not per-request |
| `mpm ops synthesize` (bulk) | Per-memory synthesis IS exposed as `synthesize_memory`; bulk scan is human-initiated |
| `mpm ops gc --review` / `--purge` / `--shred-negative` | Destructive mass operations; core safe `gc_run` IS exposed |
| `mpm ops changelog build` | Release engineering; not an agent task |
| `mpm ops maintain` | Maintenance wrapper; individual ops below are exposed |

If an agent needs any of these, the operator should run it explicitly. Tool calls that could damage state are intentionally kept on the human-facing CLI where the cost of a misclick is bounded by the operator's attention.

#### Output policy and pointer mechanics — the four layered thresholds

MCP tool results pass through several size caps as they travel outward; the four numbers below are deliberate, not duplicates, and they operate at different stages. A reader of one file alone (e.g. only `output_policy.go`) would otherwise see one cap and not realize three others exist.

| Threshold | Value (default) | Where it lives | What it bounds | Override |
|---|---|---|---|---|
| MCP transport spill boundary | 20 480 B | `internal/core/tools/output_policy.go` (was 10240; raised 2026-09-05) | Marshalled tool result returned via `mcpAdapter`. If larger, the result is spilled to blobstore and a `mpm://blob/<id>` pointer envelope is returned. | env `MPM_MCP_MAX_RESULT_BYTES` |
| `BoundInlineContent` field echo | 2 048 B | `internal/core/output_limits.go:23` | Truncates an inline echo (e.g. `mpm_memory save` echoing the just-saved fact) and flags `content_truncated: true` so the caller can detect the cap. | not user-overridable |
| Pointer resolver default | 512 B | `cmd/mpm-mcp/tools.go` (`resolveMemory`, `resolveLesson`, `resolveTheory`, `resolveWork`) | Default `maxBytes` for `mpm://memory/<id>`, `mpm://lesson/<id>`, `mpm://theory/<id>`, `mpm://work/<id>` resolution via `mpm_resolve`. Caller can request more via `opts.MaxBytes`. | caller-controlled per call |
| `mpm_blob_read` server ceiling | 256 KiB | `internal/core/tools/handlers.go:handleMpmBlobRead` | Hard ceiling on explicit `max_bytes` requests; a caller asking beyond it has the request capped to serverMax. | not user-overridable |
| CLI `memory search/list` text snippet | 500 chars | `cmd/mpm/handlers_memory.go:527, 677` | CLI text-mode snippet cap; only present on the CLI surface, not on the MCP response. | not user-overridable |
| `mpm_lessons`/`mpm_memory` summary projection | 256 chars | `summarize.go:SummarizeBounded(content, 256)` | Hard rune-bounded `summary` field when `projection=summary` (the default). The `pointer` field returned alongside tells callers how to fetch full content. | `projection=full` opt-in |
| Wake-context gather cap | 32 KiB | `internal/core/wake_context.go:295` (`MaxWakeContextBytes`) | Hard cap on the JSON wire form of the wake-context payload. If a payload exceeds the cap, `EnforceSizeLimit` sheds the heaviest arrays first (`available_skills`, then `recent_topics`), and the corresponding `*_Truncated` flag is set so the agent can tell a cap-induced drop from "checked, none found". Applied **before** the MCP transport spill check. | none — not env-overridable; changing requires a code change |
| `mpm_blob_search` server ceiling (matches) | 100 | `internal/core/tools/handlers.go:6119` (`serverMaxMatches`) | Hard ceiling on `max_matches` requests; a caller asking beyond it has the request capped to `serverMaxMatches`. The default of 20 is what most callers hit; the response payload is `matches × snippet-length` (~80 chars typical), so the actual response stays well under the 20 KiB spill threshold even at the 100-match ceiling. | not user-overridable |
| `mpm_blob_search` scan window (bytes) | 256 KiB | `internal/core/tools/handlers.go:6120` (`serverMaxBytes`) | **Scan window**, not response-size cap: the regex/literal search reads through this many bytes of the underlying blob per call. Default `max_bytes` is 50 KiB (50 × 1024); a caller asking beyond the 256 KiB ceiling has it capped to `serverMaxBytes`. The response itself is bounded by `max_matches × snippet-length`, so the 20 KiB spill boundary applies normally. | caller-controlled per call (within ceiling) |
| Memory query per-call `limit` ceiling | 200 | `internal/core/tools/handlers.go:6240` (`maxQueryLimit`) | Hard ceiling on the `limit` parameter for memory/lessons/theories/decisions searches via MCP. A caller asking beyond 200 has the request capped; this caps the response-count fan-out so a runaway `limit` can't trigger a massive FTS5 + vector scan. Documented default is 5 (no silent coercion; a `limit=0` request returns zero rows by design). | not user-overridable |

These are not in conflict — they are layered. A tool response above 20 KiB becomes a pointer envelope; the pointer then resolves bounded to 512 B by default; an explicit `mpm_blob_read` returns up to 256 KiB in one go (still gated by the 20 KiB envelope boundary at the response layer for MCP transport).

##### How the 20 KiB boundary was calibrated

The spill threshold was raised from 10 240 B to 20 480 B on 2026-09-05 after measuring the real spill distribution rather than guessing. The sample was 134 live blobs, tokenized with tiktoken-go (`cl100k_base`) — Anthropic's tokenizer is closed-source, and cl100k is the standard BPE reference, typically landing within ±15 % of Anthropic counts on mixed prose-and-JSON content.

| `source_tool` | n | bytes p50 | bytes p90 | bytes max | tokens p50 |
|---|---|---|---|---|---|
| `mpm_blob_read` | 39 | 20 868 | 76 641 | 236 751 | 6 672 |
| `mpm_context` | 36 | 14 503 | 15 514 | 16 012 | 4 405 |
| `mpm_handoff` | 1 | 10 407 | 10 407 | 10 407 | 2 796 |
| `mpm_lessons` | 3 | 145 963 | 145 963 | 145 963 | 37 316 |
| `mpm_memory` | 1 | 19 358 | 19 358 | 19 358 | 4 786 |
| `mpm_resolve` | 51 | 16 313 | 27 476 | 150 768 | 4 817 |
| `mpm_work` | 3 | 34 248 | 34 248 | 85 786 | 10 849 |

Against the old 10 240 B threshold: 3.7 % of spills were within 1.25×, 33.6 % within 1.5×, 73.9 % within 2×, and 6.7 % exceeded 10×.

Four conclusions shaped the current numbers:

1. **Token density on real MPM payloads is ~0.32 tokens per byte** — close to the `bytes/4` rule of thumb, slightly above it. Either is fine for rough sizing; the tokenizer is the source of truth.
2. **The old threshold sat well below the median spill.** The median spill was 1.6× the threshold, meaning the system was mostly spilling responses that had only just crossed the boundary — paying a round trip for payloads that would have been cheaper inline. Doubling the boundary keeps the pointer mechanism for genuinely large results and stops taxing moderate ones.
3. **`mpm_lessons` was the worst offender by an order of magnitude** — three blobs at ~146 KB each, ~37 K tokens per spill, from returning full lesson texts in batches. This is why `projection=summary` is the default for lessons and memory, with a 256-rune bound and a pointer alongside.
4. **`mpm_blob_read` is the only path that can chain.** Its own result can re-spill when a caller requests `max_bytes` above the threshold, producing resolver chasing rather than a single round trip. The 256 KiB server ceiling bounds the chain.

**Scanner parity.** Spilling cannot be used to dodge content scanning. The spill is a *return-side* mechanism: write-side scanning (`isSensitiveContent` + `isPoisoned`) runs at the substrate INSERT sites, before the bytes exist anywhere the spill could reach. There is no path by which input content of unknown provenance arrives at `blobstore.Put`.

**Blob file permissions.** The blobstore creates its directory with `os.MkdirAll(blobDir, 0o700)` and writes blob files at `0600`. Note that `os.MkdirAll` does not *correct* the mode of a directory that already exists, so an install whose blob directory predates that code can retain a looser mode. Blobs restored or copied by hand can likewise carry the permissions they were copied with. If you inherited a blob directory, check it:

```bash
stat -c '%a %n' ~/.mpm/blobs
find ~/.mpm/blobs -type f ! -perm 600 -printf '%m %p\n'
chmod 700 ~/.mpm/blobs && find ~/.mpm/blobs -type f -exec chmod 600 {} +
```

### 6.5 Multi-Agent Shared Epistemology (Layers 0–4)

*Federates house rules across operators via a separate DB — five layers from local cache to global arbitration, no IPC invented.*

Multiple agents on a single workstation can share a single source of truth for house rules, cross-project decisions, and durable conventions, while keeping their per-project tactical memories isolated. The substrate is SQLite `ATTACH DATABASE`. The behavior is a **five-layer stack** that turns the shared DB from passive storage into an active dissemination system.

**Why SQLite ATTACH, not Postgres, not a separate service.** The design contract is single-workstation scope. SQLite ATTACH gives the architecture without adding a server, a network boundary, or a new failure mode. The trade-off is no cross-DB transactions — accepted because rule writes are append-mostly and operator-gated.

#### Scope: `mpm.db` vs `shared.db`

These two databases solve different problems. Conflating them is the most common misread of the architecture.

- **`mpm.db`** is **host-shared** — one DB per host (`~/.mpm/src/db/mpm.db` by default, override via `MPM_WORKSPACE`). Every local agent on that host (OpenClaw, OpenCode, Claude Code, Pi, Hermes, the MCP server, the scheduler, the critic) opens the same file. There is **no inter-agent federation required**; SQLite's single-writer WAL + `busy_timeout=5s` serialises writes without coordination.
- **`shared.db`** is **project- or organisation-shared** — a *separate* file that crosses host and team boundaries. It is what a repo's `.mpm/shared.db` looks like when committed to git, or what a CI cluster mounts as a shared volume. It is **not** the same thing as `mpm.db`, and it is **not** auto-created.

```
┌─────────────────── Single host ───────────────────┐
│  OpenClaw   OpenCode   Claude Code   Pi          │
│       \       |       /       \      |          │
│        \      |      /         \     |          │
│         ────── mpm.db (host-local) ──────        │
└──────────────────────────────────────────────────┘

┌────── Cross-host / cross-team ──────┐
│  host A            host B           │
│  mpm.db            mpm.db           │
│    \                /               │
│     ── shared.db (ATTACHed) ──      │
│     (git, NFS, shared volume)       │
└─────────────────────────────────────┘
```

The host's `mpm.db` cannot be checked into git because it contains local file paths, private session handoffs, ephemeral scratchpads, and hundreds of daily write-churn rows. `shared.db` is the subset worth committing: vetted, high-value, operator-gated knowledge.

#### Why `shared.db` Exists

Three problems it solves that `mpm.db` alone cannot:

1. **Cross-Machine Collaboration.** Three developers (or five cloud worker nodes) on the same repo need an upstream knowledge base they can all read and write through the same substrate. They cannot share a single host-local `mpm.db`. They need a distributable file — `shared.db` — committed to the repo or mounted as a shared volume.
2. **Ephemeral vs Curated Knowledge Isolation.** `mpm.db` holds everything: untested hypotheses, working context, test runs, raw observations. `shared.db` holds only the subset that has earned its keep: org-wide prime directives, proven architectural theories, canonical lessons, resolution/arbitration memory. The 20× write-churn of local dev work does not pollute the 1× write-churn of curated knowledge.
3. **Read-Only Governance.** Operators can mount a company-wide or repo-wide `shared.db` as **read-only** (`MPM_SHARED_READONLY=1`). Local agents read organisational rules without having permission to pollute the shared source with hallucinated memories. Promotion to global (`promote_to_global`) is a separate operator-gated action that crosses the read-only boundary.

#### When `shared.db` Is Created

Not during standard single-user local dogfooding. The shared DB is instantiated when one of these operator actions happens:

- **Repository bootstrap** — a team lead commits `.mpm/shared.db` (gitignored or git-tracked by team convention) and sets `MPM_SHARED_DB` in the team's CI / dev-container config.
- **Promoting local knowledge upstream** — an operator runs `record_global_rule --confirm=true` against an existing local memory, which writes the first row into the shared DB and materialises the file on disk.
- **Multi-agent cluster / CI deployment** — a shared volume is provisioned and the orchestrator sets `MPM_SHARED_DB` for every node.

The default single-user install has no `shared.db`. Local agents run on `mpm.db` alone. The OpenCode smoke test's `shared_attached: false` is the canonical local-dogfooding state, not a defect.

#### How `shared.db` Is Created and Structured

There is no explicit `mpm ops init --shared` command. Creation happens implicitly on first boot:

1. Operator sets `MPM_SHARED_DB=/path/to/project/.mpm/shared.db`.
2. The next `mpm` / `mpm-mcp` / `mpm-scheduler` boot calls `attachShared(sharedPath)` in `internal/core/db.go`.
3. `attachShared` ensures the directory exists (`os.MkdirAll`, mode 0700), ATTACHes the file via `ATTACH DATABASE '<path>' AS shared` (creating it if absent — SQLite creates the file on first write), and runs the canonical `BaseTables` DDL (`CREATE TABLE IF NOT EXISTS`) plus `SafeMigrations` against the attached schema.
4. After boot, `mpm ops shared status` confirms: `MPM_SHARED_DB` value, `shared_attached: true|false`, `shared.memories` row count, `is_global=1` count, file size.

The shared DB uses the **same table schema** as the local DB (`memories`, `theories`, `evidence`, `directives`, etc.). Volatile host-only tables (`scheduled_wakes`, local `sessions`, transient `scratchpads`) are deliberately **excluded** from the shared schema — their lifecycle is host-local and would be wrong if cross-shared. Cross-DB queries are plain SQL: `SELECT … FROM shared.memories WHERE …`. No IPC, no separate service.

#### How Agents Know to Use It

The agent doesn't. The routing lives entirely inside the MPM core.

```
Agent process (mpm-mcp / mpm CLI / scheduler)
        │
        │ reads MPM_SHARED_DB at boot
        ▼
internal/core/db.go attachShared()
        │
        ├── path exists?  ─── no ──→ local-only mode (shared_attached: false)
        │     │
        │     yes
        ▼
   ATTACH as 'shared', run BaseTables + SafeMigrations
        │
        ▼
shared_attached: true
        │
        │ every read tool (mpm_memory, mpm_context, …)
        │ unions local + shared transparently
        ▼
Agent sees one merged view
```

When the agent calls `mpm_memory action=query`, `mpm_context action=read_directives`, or any read tool, the core queries both sources and merges. For directives, `ReadDirectivesForFramework` reads the local baseline (the four baseline directives seeded by `mpm ops init directives` against the *local* `mpm.db`) and overlays the shared-database directives, deduplicating by `stable_id` (shared takes precedence — it is the operator-curated truth). For memory searches, FTS5 runs across both schemas and merges relevance scores with a multiplicative **Shared Premium** (1.20× for shared results, 1.35× when the shared row also matches a `query_global_rules` scan). House rules outrank noisy local memories without being able to *invent* matches.

The agent never makes a decision to "call shared.db". It calls `mpm_memory` or `mpm_context`, and MPM serves the union. The only agent-visible surface is the `shared_attached` boolean in `health_check` responses — present so operators can verify the wiring, not so agents need to branch on it.

#### Layer 0 — Federation

Each workspace has its own `mpm.db` (per-project tactical memory). An optional shared database, identified by the `MPM_SHARED_DB` environment variable, is ATTACHed as the `shared` schema when that env var is set. There is NO default path — if `MPM_SHARED_DB` is unset, mpm runs in local-only mode with no shared schema. Cross-DB queries become plain SQL. The convention `~/.mpm/shared/shared.db` is a reasonable default for operators who want one, but it must be opted into via the env var.

The shared DB uses the **same table schema** as the local DB. Migrations apply to both DBs at startup. FTS5 sync triggers keep the shared FTS5 mirror in lockstep with the shared tables. `getDB()` is a per-process singleton, so one ATTACH, one connection, no leak surface.

`mpm_memory action=query` federates across both DBs with a multiplicative Shared Premium: `score = base × 1.20` for shared results, `score = base × 1.35` when the shared row also matches a rule. The boost only applies to rows that already pass the base relevance threshold — house rules outrank noisy local memories without being able to invent matches.

Writing to shared (`record_global_rule`, `promote_to_global`) requires `confirm=true` in the tool payload. The agent cannot autonomously extend the shared rule set — only the operator can.

#### Layer 1 — Shared Vector Search

Local FTS5 is a keyword index. For semantic recall over the shared DB, MPM runs an in-SQLite IVF (Inverted File) ANN index — no separate vector database, no CGo, no parallel files. The index partitions the 768-dim embedding space into Voronoi cells, then restricts each query to the nearest few. A circuit breaker caps worst-case work.

#### Layer 2 — Conflict Detection

When two memories disagree, the system has to notice. `shared.contradiction_log` is the detection surface. Every shared write that produces a near-miss against an existing memory is logged with a protobuf-shape score (a weighted combination of confidence, freshness, and reinforcement). Detection runs **inside `mpm_memory action=query`'s result set** — opportunistic, not polled. There is no background ticker, no separate timer.

If the two candidates land within a small margin of each other, the system **does not** auto-resolve. It writes a pending theory and waits for an operator. A clean winner can be auto-resolved; a coin-flip always escalates.

#### Layer 3 — Conflict Resolution + Arbitration Closure

When a contradiction resolves, the system doesn't just delete the loser. It writes a **resolution memory** capturing the choice and rationale, marks the queue resolved, and — as of the Arc 1 Closure epic — appends an evidence row to the winner.

**Resolution-as-evidence asymmetry.** Only the winner gets the `resolution_survived` evidence row. The loser is tagged with `operator_arbitration` and the originating theory ID in its metadata, then archived. A memory that has survived N contradictions accumulates 1.0×N supporting evidence in its confidence ledger — the system is literally rewarded for being right under pressure.

The arbitration auto-slash loop closes when the operator resolves a pending theory: the resolution memory, the queue update, the asymmetric evidence write, and the theory status flip all happen in one transaction. The loop is closed.

#### Layer 4 — Active Dissemination

Arc 1 made the shared DB write and self-heal. But receiving agents had no idea any of this happened unless they ran a search that surfaced the resolution. Arc 2 fixes that.

When an epistemic event lands (rule, resolution, arbitration), it **fans out** as a deterministic event-wake to every active session. Receiving agents see the new state on their next MPM call without going through the federated search at all. The shared DB is no longer passive; it actively pushes its own updates.

The dedup mechanism is a deterministic SHA-256 prefix used as the PRIMARY KEY of `shared.bcast_event_wakes`. `INSERT OR IGNORE` is the dedup mechanism — database-level, O(1), race-free. Re-broadcasting the same content is a no-op. A rule body text change fires a new wake; a metadata-only update does not.

Three operator commands (`record_global_rule --confirm=true`, `resolve-contradictions --apply`, `resolve_theory --winner=<id>`) trigger auto-broadcast as a strict side-effect of the explicit gate. Auto-broadcasts are **non-fatal**: a broadcast failure does NOT roll back the underlying write. The resolution is ground truth; the broadcast is the notification. Internal agent writes (without an operator gate) do NOT trigger broadcasts.

**The receive path:**

```bash
mpm call mpm_wakes --payload '{"action":"check_pending_event","params":{"session_id":"..."}}'
```

The dispatcher pulls pending event wakes after every handler returns, so the agent sees them on every MPM call without an explicit pull. New rules, resolutions, and arbitration verdicts surface inline as an `EventWakesPending` block in the response.

**Why push, not pull.** Federation is pull: an agent decides to query, the system responds. Event wakes are push: the shared DB has decided something changed, and the change is **already in the agent's wake context** on the next call. No search, no cold-start, no missed signal — even a fresh process picks up pending wakes immediately.

#### Operational notes

- Cross-DB transactions are NOT supported. Every shared write is its own atomic transaction on the shared DB.
- WAL mode on both DBs allows concurrent local writers without blocking.
- A drift missed last query is just as catchable next query. Detection and fan-out are both opportunistic.
- Cold-start does NOT replay old wakes. `mpm_memory action=query` handles history; event wakes are real-time push only.

For the DDL, fan-out algorithm, auto-broadcast hooks, tests, and smoke behind this stack, see **Appendix A** (Layers 0-3) and **Appendix B** (Layer 4).

### 6.6 Telemetry Sidecar (mpm-telemetry)

`mpm-telemetry` is a sibling binary that records **raw LLM invocation economics** — one record per `messages.create()` call (or equivalent), no derivation, no aggregation, no USD math. It is deliberately ignorant of MPM's cognitive substrate: a separate SQLite database (`$MPM_WORKSPACE/telemetry.db`), no foreign keys to `mpm.db`, no new columns in `memories`, no dependency from `mpm` or `mpm-mcp` onto this binary. The two surfaces can be deployed, evolved, or removed independently.

**What gets recorded.** Per invocation: invocation_id, parent_invocation_id, session_id, framework + version, provider + model + revision, started_at, completed_at, status (completed / failed / cancelled / timed_out), stop_reason, the five token counters (input, output, cache_read, cache_write, reasoning), duration_ms, and an opaque provider_metadata blob. NULL means "not reported" — explicitly-zero means "zero". Token values come from the LLM provider's response; MPM does not second-guess them.

**What does NOT get recorded.** Prices, USD amounts, cost projections, vendor markups, ROI metrics. Pricing lives in an external, versioned JSON catalog (`internal/telemetry/testdata/one-model.json` is the canonical fixture; operators ship their own) and is applied at **read time** by `bin/mpm-telemetry cost --pricing <file>`. The raw ledger is the source of truth; the projected cost is a derived view that can be re-run against any historical snapshot without re-ingesting frames.

**Wire format.** Frame bodies are newline-delimited JSON over a Unix socket (`$MPM_WORKSPACE/run/mpm-telemetry.sock`, override via `MPM_TELEMETRY_SOCKET`). The wire schema is versioned (`"schema_version":"v1"`). Unknown schema versions are rejected at the protocol boundary. Idempotent retries (same `invocation_id`, same payload) return `{"status":"ACCEPTED","inserted":false,...}`; conflicting duplicates (same id, different payload) return `{"status":"REJECTED","reason":"invocation_id_payload_conflict"}`. SQLite is the canonical store with WAL mode + 5s `busy_timeout` — the same single-connection-via-discipline that the MPM core uses.

**Client-side push, non-blocking.** Agents emit frames via the `telemetry_adapter.py` library (`agent_installation/mpm-claude-code/src/telemetry_adapter.py`). The adapter returns immediately; a background daemon thread drains a per-process ring buffer (capacity 1000) over the socket with a 100ms connect timeout. If the collector is absent, frames are evicted oldest-first on overflow and the agent is never blocked. Spec invariant: collector availability MUST NOT affect agent correctness.

**Subcommands.**

```
mpm-telemetry serve                       # Run the socket collector (default)
mpm-telemetry ping                        # Handshake: collector_version, protocol_version, schema_version, queue_depth, uptime_seconds
mpm-telemetry query invocation <id>       # Read one row
mpm-telemetry query session   <id>        # Aggregate rows for a session
mpm-telemetry query since     <unix-sec>  # Read rows newer than cutoff
mpm-telemetry cost --pricing <file>       # Apply external pricing at read time
mpm-telemetry observe [--since] [--threshold] [--min-invocations]
                                         # HighTokenNoArtifactHunt: anomaly detector
                                         # (observation, NOT ROI metric; human arbitrates)
```

`observe` is the cross-DB anomaly detector. It aggregates token burn per session via the telemetry ledger, then asks MPM (via `mpm call mpm_provenance --payload '{"action":"count_by_session",...}'`) how many artifacts the session produced. Sessions with high token burn and zero new artifacts surface as `type: "observation"` lessons — descriptive, not prescriptive. The cross-DB call is parameterized so the test suite injects a stub; the production shell-out is in `cmd/mpm-telemetry/observe.go`.

**Verification.** `scripts/smoke_telemetry.sh` is a hermetic end-to-end test: boots the collector in a temp dir, sends 3 synthetic frames + an idempotent retry + a conflicting duplicate + a bad-schema frame, runs `query` / `cost` / `observe --dry-run`, asserts the row count via sqlite. Exits 0 on success.

**Why a separate binary.** Billing data has different retention, export, and access-control semantics than cognitive data. Mixing them would force operators to ship a single retention policy, single access control, single export pipeline. Keeping `telemetry.db` separate means finance/ops teams can ship their own retention without negotiating with cognitive-data stakeholders.

---

### 6.7 Epistemic Compaction

*Raw memories accumulate faster than any agent reads them; compaction drains the backlog into lessons — and a refusal from the model is a valid answer, not a failure to be retried forever.*

`mpm_system action=compact` drains eligible raw memories into lessons in sequential batches of at most **50** (the LLM context safeguard). Each batch is independently synthesized, validated, and committed. `force=false` (the default) **relieves** pressure — the drain stops as soon as `raw_count <= threshold` and may leave eligible rows remaining. `force=true` **drains everything** — the threshold gate is bypassed and the drain continues until the substrate is empty or the per-invocation cap is hit. `force` does **not** widen the 50-item per-batch limit.

#### A refusal is a valid terminal outcome

The synthesis prompt explicitly sanctions declining a batch. When the model does, it returns the sentinel `{"title":"", "body":"", "tags":[]}`. That is a **considered judgement** — "these rows do not constitute a lesson" — and MPM treats it as a successful outcome with a distinct type, not as a malformed response.

The distinction is load-bearing in both directions. A *near-miss* — a populated object whose title is empty, or one with a field the schema does not declare — is an **error**, because the model tried to answer and produced something unusable. Turning those into refusals would silently defer a broken response stream instead of surfacing it. Validation is unchanged for populated lessons: title, body, and tags are all still required.

#### Deferred rows

A refused batch is **deferred**, not retried. Each row in the group gets four `compaction_deferred_*` metadata keys — `at`, `reason`, `batch`, and a truncated `sample` — and a shared `compaction_deferred_batch` id, so an operator can see which rows were offered together and why they were declined. The whole group is written in one transaction: a fault midway leaves **zero** rows annotated, never a partial group.

Deferral is compaction-scoped and nothing more:

- The rows stay fully searchable and remain in the export population. A deferral records that the model declined, not that the memories are invalid.
- Deferral state is visible to an agent at two different granularities, and the distinction is a property of the surfaces rather than a policy. Looking a row up **by id** (`mpm_memory show`, `mpm_resolve mpm://memory/<id>`) returns the full metadata document, so all four `compaction_deferred_*` keys are inspectable. **Search** results (`mpm_memory query`) project a fixed field set that does not include `metadata`, so a search row shows that a memory matched but not that it is deferred — the only signal there is the aggregate `epistemic_pressure.deferred_count` / `actionable_pending`. An agent can therefore always see *how much* is deferred, and *which rows* are deferred when it already has the id.
- `compacted_into` is **never** written for a refused batch. That field is written only inside the same transaction that inserts the lesson it points at, and a refusal produces no lesson — so the row stays eligible to be re-offered once an operator requeues it.
- Errors are never recorded as refusals. A provider failure stops the drain and reports `failed_batch` / `failure_reason`; it does not annotate anything.

#### The pressure signal counts what is still offerable

`epistemic_pressure_v` exposes two counts over the same population:

```
raw_count        = actionable_pending + deferred_count
```

`raw_count` is unchanged in name and meaning — it still counts every un-compacted memory, including deferred ones. What is new is that the *trigger* no longer reads it. The wake/context compaction demand is computed from `actionable_pending`: rows the model has already declined cannot produce a lesson no matter how many times the drain runs, so a fully-deferred substrate reports pressure but does **not** demand compaction. Without this, the scheduled reflex fires forever against a substrate where compaction cannot succeed, and the loop the deferral design removes comes back one layer up.

`raw_remaining` in the result envelope includes deferred rows, so it is not a measure of remaining *work*. Read `actionable_pending` to know whether anything is still offerable.

#### Stage budget, not batch budget

`max_batches` bounds **semantic stages** — model synthesis attempts — not batches. A successful batch costs 1 stage; a refused batch costs 3, because MPM first attempts the whole batch and then, if refused, tries each half once more before deferring the group. The ceiling is **8 stages** (default 8, hard cap 8, silently clamped). The public parameter keeps its original name because renaming it would be a breaking wire change; the *unit* is documented rather than renamed, since "batch" and "stage" are different things and conflating them is what produced the old 20/100 documentation drift.

`semantic_stages_spent` in the result reports what the budget actually bought, which is not the same number as `batches_processed` on a refusing substrate.

#### Requeue is an operator action

Returning a deferred row to the pool is `mpm compact requeue-deferred` — **CLI only, and deliberately not exposed on the MCP surface**. A deferral is a machine decision; a requeue is a *human overriding a machine decision*, and an agent able to requeue could undo a refusal on its own judgement, putting the loop back just more slowly. The `compact` action's params schema is `additionalProperties: false`, so requeue is not merely undocumented there — it is unreachable.

The operator can always see what they are overriding: `mpm compact deferred` lists the deferred rows with their reason, batch, and a content sample. See [`mpm compact`](#compact--compaction-operator-surface) for the command surface.

---

## 7. Core Stability

The following concepts define MPM. They should change rarely.

- Memories
- Decisions
- Theories
- Lessons
- Evidence
- Confidence

Everything else is considered Runtime.

The Core should evolve only when the model of cognition evolves.
Runtime should evolve whenever the operation of agents improves.

Runtime features may evolve rapidly without changing the cognitive model.

### Why this matters

A small, stable core is what makes the rest of the system safe to evolve. Every feature outside Core — wake, routing, personas, scheduling, synthesis, review, audit — can be redesigned, replaced, or removed without invalidating the agent's existing knowledge. Core changes require demonstrated usage evidence and survive only if the regret log proves the change carries weight.

### The discipline

Before adding a new feature or changing an existing one, the answer to these questions must be clear:

- What problem does this solve?
- Who has this problem?
- What is the cost of adding it?
- Will it survive the regret log?

Default to no. The threshold for adding is high. The threshold for removing is the same.

### The regret log

The agent maintains a private regret log of things it considered but didn't add, and things it added that turned out not to matter. It is the empirical record of feature-creep pressure and the source of truth for what to delete next.

---

## 8. CLI Reference

Three surfaces share one substrate (`internal/core/tools` registry):

- **Human-facing CLI** — `mpm <verb>` cognitive vocabulary (remember, learn, decide, theorize, ...). Designed for developer clarity in the terminal.
- **Agent-facing JSON-RPC** — `mpm call <tool> --payload JSON`. Designed for shell scripts and structured tool consumers.
- **MCP server** — `mpm-mcp` speaks the Model Context Protocol for AI agents. Registers every entry in the same registry.

### Daily Commands (root level)

```bash
mpm                # Readiness dashboard — verifies substrate is ready, auto-starts scheduler
mpm <query>        # FTS5 search (default when called with a bare string)
mpm add <text>     # Add a new memory
mpm add -i         # Interactive add — opens $EDITOR
mpm snooze <id>    # Bump a memory's relevance (no LTM promotion)
mpm ls             # List recent memories
mpm show <id>      # Show one memory
mpm rm <id>        # Soft delete
mpm wake           # Last session context (mode, persona, topics, recent memories)
mpm call <tool>    # Universal machine interface (JSON payload)
mpm status         # System status dashboard
mpm version        # Version info
mpm help           # Progressive-disclosure help (~22 primary commands; --all for full catalogue)

# Cognitive verb aliases (noun-verb human surface)
mpm remember <text>     # Alias for add
mpm learn <text>        # Create a lesson (cognitive verb)
mpm decide              # Record a decision
mpm theorize            # Propose a theory
mpm decision add|resolve|help   # Decision ledger subcommands
mpm theory add|resolve|help     # Theory subcommands
mpm skill add|list|show|search|workshop|help  # Skill management subcommands

# Session & context
mpm continue            # Session-resumption dashboard (working context, wake, stats)
mpm work status         # Working Context status (one-line summary)
mpm work show           # Working Context full content
mpm work clear          # Discard current Working Context
mpm work promote        # Promote Working Context to permanent memory

# Provenance & diagnostics
mpm why <id>            # Artifact provenance — evidence chain, confidence, retrieval stats
mpm doctor              # Active trust-signal diagnostics (now actively verifies configured model connectivity through real provider probes)
mpm status              # Compact dashboard — Uptime / Mode / Models / counts (Models line shows recent verified health; never probes network)
mpm info                # Installation identity — version, paths, models, skills, counts
mpm tour                # Interactive 6-step walkthrough of cognitive verbs
mpm tour --demo         # Auto-run each step with sample arguments

# Cross-component orchestration
mpm call mpm_request_review \
    --payload '{"components":["memory","critic"],"prompt":"is this consistent?","artifacts":["mem_abc123"]}'
                        # Concurrent multi-component review: each component resolves to a
                        # profile via ProfileFor; per-component model call fused into a
                        # Markdown render. Independent results — one component failing
                        # does not abort the others. (Engine: internal/core/orchestration.
                        # Renderer: internal/core/renderers. Adapter: tools/handlers.go.)

# Configuration
mpm config              # Interactive AI provider setup wizard (MiniMax, OpenAI, OpenRouter, Anthropic, Ollama, LM Studio, Custom)
mpm config show         # Print current config
mpm config get <key>    # Print one config value
mpm config set <k> <v>  # Set one config value and persist
mpm config edit         # Open runtime mpm_config.json (in $MPM_WORKSPACE) in $EDITOR
mpm config profile add|list|get|set|remove <name>  # Execution profiles (provider/model/temperature/...)
mpm config component list|get|set <component>     # Component bindings (memory → profile, etc.)
mpm config capability list|get|set <capability>   # Capability registry (operator-meaningful vocabulary)
mpm config validate                                # Structural validation (5 layers of checks; exit 0/1/2)
```

### `kb` — Knowledge Base

Entity-centric interface to all MPM collections.

```bash
# Memory
mpm kb memory list
mpm kb memory search <query>
mpm kb memory show <id>
mpm kb memory add <content>
mpm kb memory shred <id>

# Topic
mpm kb topic list
mpm kb topic add <name> [description]
mpm kb topic search <query>
mpm kb topic show <id>
mpm kb topic shred <id>

# Lesson
mpm kb lesson list
mpm kb lesson list --type warning
mpm kb lesson add <content> [--type warning|practice|insight]
mpm kb lesson search <query>
mpm kb lesson get <id>
mpm kb lesson shred <id>
mpm kb lesson stats

# Session
mpm kb session list
mpm kb session search <query>
mpm kb session show <id>
mpm kb session shred <id>

# Reference library (PDF, EPUB, MD, TXT, HTML)
mpm kb reference add <file> [--tag tag1,tag2] [--reason "..."] [--chunk-size 512]
mpm kb reference ls
mpm kb reference search <query>
mpm kb reference show <id>
mpm kb reference used                    # most-retrieved references
mpm kb reference interactions            # recent retrieval events
mpm kb reference admit [--limit N] [--dry-run]
mpm kb reference shred <id>

# Skill
mpm kb skill list
mpm kb skill search <query>
mpm kb skill show <id>
mpm kb skill add

# Epistemology
mpm kb theories [pending|resolved|all]
mpm kb decisions
mpm kb propose_theory <theoryId>
mpm kb resolve_theory <theoryId> <status>
mpm kb record_decision <decisionId>
mpm kb hint <topic>
```

### `ops` — Engine Room

Maintenance, diagnostics, synthesis, and power tools.

```bash
# Core engine
mpm ops doctor [--explain|--deep-scan]
mpm ops self-heal [--dry-run|--force|--quiet]
mpm ops maintain
mpm ops synthesize [--dry-run]
mpm ops synthesize status
mpm ops synthesize failures
mpm ops gc [--dry-run|--review|--purge|--shred-negative]
mpm blob gc [--dry-run]                   # Garbage collect expired and orphan blob files
mpm ops backfill-embeddings [--batch-size|--collection|--dry-run]
mpm ops dlq:review [review|clear|retry]
mpm ops lint [--dir <path>]... [--show-clean]
mpm ops changelog build [--since v1.0.0] [--release-version 1.1.0] [--legacy]
mpm call mpm_system --payload '{"action":"query_audit_log","params":{"level":"...","component":"...","days":7,"limit":20}}'
mpm call mpm_handoff --payload '{"action":"write","params":{"session_id":"...","summary":"...","commitments":[],"open_questions":[]}}'
mpm call mpm_handoff --payload '{"action":"read","params":{"handoff_id":"...","mark_read":true}}'
mpm call mpm_handoff --payload '{"action":"list","params":{"limit":10,"unread":false}}'
mpm call mpm_scratchpad --payload '{"action":"flush","params":{"session_id":"...","thesis":"...","supporting":"..."}}'
mpm call mpm_scratchpad --payload '{"action":"read","params":{"session_id":"..."}}'

# Diagnostics
mpm ops stats
mpm ops status
mpm ops prune [--older-than <n>d]

# Data
mpm ops export [--jsonl]
mpm ops backup [path]
mpm ops restore-db <path>
mpm ops ingest <path>

# Init (one-time bootstrap — idempotent)
mpm ops init directives       # Seed baseline cognitive directives
mpm ops init skills           # Seed baseline skill library (status, health, etc.)

# Context
mpm ops switch
mpm ops directives
mpm ops wake

# Mode & Persona
mpm ops mode [list|active|remove|clear]
mpm ops persona [list|active|set|clear]

# XITL Stance (runtime persona hot-swap — no restart)
mpm ops stance assume <mode> <persona> <rationale>
mpm ops stance synthesize <name> [flags]

# Multi-agent shared epistemology
mpm ops shared [status]
mpm ops resolve-contradictions [--apply] [--json]      # Arc 1: dry-run inspect or apply resolutions
mpm ops broadcast <memory_id>                          # Arc 2: fan-out an event wake
        [--kind=rule|resolution|arbitration|memory]    #    (auto-detected from collection if omitted)
        [--rationale="..."]                            #    (REQUIRED for kind=memory)
        [--to=agent_id,...]                             #    (targeted; otherwise full active fleet)
        [--dry-run] [--json]
mpm ops active-sessions [--json]                       # Arc 2: list the active fleet (last 24h)
mpm resolve_theory <id> <conclusion> --winner=<id>     # Arc 1 closure: arbitration auto-slash

# Entity ops
mpm ops topic|lesson|session|reference|memory|wake|gateway
```

### `debug` — Low-Level Inspection

```bash
mpm debug history <memoryId>         # Version history for a memory
mpm debug diff <memoryId> <v1> <v2>  # Unified diff between two versions
mpm debug diff-lines <text1> <text2>
mpm debug patch-memory <id> <json>
mpm debug shred <id>
mpm debug show <id>
mpm debug gc [--dry-run]
```

### `cascade` — Epistemic Cascade Materializer

CLI surface for the cascade materializer (§4.8). The materializer drains pending `epistemic_cascade_outbox` intents into pending theories. Operators schedule from `cron` or `systemd` — each invocation drains the outbox and exits; there is no long-running daemon.

```bash
mpm cascade                                              # Show help
mpm cascade materialize [--once] [--max-iterations N] [--poll-interval T]
        # Drain the outbox. Default: loop until pending=0
        # --once          : process one batch and exit
        # --max-iterations: bound iterations (0=unbounded; exit 2 on timeout)
        # --poll-interval : sleep between empty-queue polls (5s default, min 1s)
mpm cascade list-dead-letters
        # Show failed intents (status='failed') with terminal_error, plus
        # an outbox summary: pending / processing / materialized / failed
```

**Exit codes**

| Code | Meaning |
|---|---|
| 0 | Outbox drained (no pending intents) |
| 1 | Runtime error (DB unavailable, scanner rejection, etc.) |
| 2 | Timeout — `--max-iterations` exceeded before drain |

**Drain confirmation.** The materializer polls the outbox and exits only when two consecutive polls confirm `pending=0 AND processing=0`. This prevents a race where a concurrent invalidation enqueues a new intent that the single confirmation miss would not catch.

**Operator pattern.** Schedule from `cron` every 5 minutes, or run from `systemd` on a 60s timer:

```cron
*/5 * * * * $HOME/.mpm/bin/mpm cascade materialize --max-iterations 50
```

The `--max-iterations` bound is a safety valve — the operator's job is to keep the outbox at zero (or near it), not to let a single invocation spend unbounded time churning through a blast-radius cascade.

### `compact` — Compaction operator surface

Operator surface for the deferral lifecycle (§6.7). The compaction drain itself is agent-reachable through `mpm_system action=compact`; these two subcommands are the parts that are deliberately *not* — requeue in particular, because returning a declined row to the pool is a human overriding a model decision.

```bash
mpm compact                                              # Show help
mpm compact deferred [--json]                            # List deferred rows + reasons (read-only)
mpm compact requeue-deferred [limit] [--limit N] [--json]
        # Return up to <limit> deferred rows to the compaction pool.
        # Default limit 50; oldest-first; capped at 1000. No unbounded mode.
```

**Exit codes**

| Code | Meaning |
|---|---|
| 0 | Command completed (including a requeue that found nothing to do) |
| 1 | Unknown subcommand, unknown flag, or a non-numeric limit — nothing is written |
| 2 | Runtime error (DB unavailable, scanner rejection, etc.) |

`requeue-deferred` clears exactly the four `compaction_deferred_*` keys and writes nothing else: content, weight, collection, `created_at`, and every other substantive column are untouched, so a requeued row is byte-identical to one that was never deferred. The exception is `updated_at`, which the requeue stamps — the row was modified, and a stale timestamp would make a requeued row look untouched to anything reading recency. It does **not** clear `compacted_into` — a row that carries both keys was compacted at some earlier point, and re-synthesising it would duplicate a lesson. Each call writes a `system_audit_log` row naming the row ids it cleared, so repeated requeues are reconstructible. That audit row is written **in the same transaction as the requeue**: if it cannot be written, the requeue is rolled back and the command reports failure. A requeue therefore never commits without leaving a record — the same guarantee purge holds.

Requeue returns the row to the pool; it does not schedule a synthesis. A requeued row is offered again on the next drain, spending stage budget like any other row.

**Not exposed over MCP.** `requeue-deferred` has no `mpm_system` counterpart and no `mpm call` equivalent — reach the drain itself through `mpm call mpm_system --payload '{"action":"compact",...}'` if you need the agent path. The `compact` action's params schema is `additionalProperties: false`, so a requeue parameter cannot be passed even by an agent that guesses the name.

### CLI-side input/output limits

CLI commands enforce byte and char caps on input payloads and rendered output, plus row-count defaults for `--limit` flags. These are distinct from the §6.4 MCP-output caps: this table is the user-facing CLI surface, where the operator (not the agent) sees the value. Inline per-command `fs.Int("limit", N, ...)` defaults are not listed here — they're already represented in the auto-generated Command Catalogue block below and the per-subcommand description prose.

| Limit | Value | Where it lives | What it bounds | Override |
|---|---|---|---|---|
| `mpm add --file` payload cap | 100 MiB | `cmd/mpm/handlers_memory.go:23` (`memoryMaxFileBytes`) | Hard cap on the size of `--file` payload accepted by `mpm add`. A file larger than this is rejected with an explicit error rather than silently truncated. | not user-overridable |
| `mpm add` inline content cap | 1 MiB | `cmd/mpm/simple_cmds.go:97` (`maxAddBytes`) | Cap on the inline content string passed to `mpm add` (the positional/argv form, distinct from `--file`). Larger content must use `--file` instead. | not user-overridable |
| Reference attachment cap | 100 MiB | `cmd/mpm/simple_cmds.go:921` (`maxRefBytes`) | Cap on reference attachment size accepted by `mpm kb reference admit` and related subcommands. | not user-overridable |
| Route-render body cap | 9 500 chars | `cmd/mpm/route_render.go:179` (`routeOutputCap`) | Total rendered body size for `mpm ops stance` output, deliberately under Claude Code's 10 000-char hook stdout limit so the directive isn't truncated mid-injection. | not user-overridable |
| Route-render mode hard cap | 9 000 chars | `cmd/mpm/route_render.go:180` (`routeModeHardCap`) | Hard ceiling on the mode portion of a rendered route — preserves room for persona content under the 9 500-char body cap when both are present. | not user-overridable |
| `mpm ops resolve-contradictions` default `--limit` | 100 | `cmd/mpm/ops_resolve_contradictions.go:40` (`defaultResolveLimit`) | Default row count for `mpm ops resolve-contradictions --limit`. Capped at 10× the default (1 000) regardless of caller-supplied limit. | caller-controlled (capped at 1 000) |
| `mpm decisions` (no-args) default row cap | 20 | `cmd/mpm/handlers_epistemology.go:839` (`defaultDecisionsListLimit`) | Hard-bounded row count for the no-args legacy `mpm decisions` listing. Replaces an unbounded listing that scaled linearly with collection size (per pointer-indirection sweep F-S-2). Use `mpm decisions list --limit N` for explicit pagination. | not user-overridable on the no-args path; `--limit` flag for paginated callers |

<!-- cli:begin — auto-generated by `go run ./cmd/gen-cli`. Do not edit by hand. -->

### Command Catalogue (auto-generated)

Top-level commands registered in `cmd/mpm/router.go`. Subcommand surfaces (e.g. `mpm kb memory list`, `mpm ops gc`) are dispatched via `handlers_*.go` and documented manually in the subsections above. Regenerate this block with `go run ./cmd/gen-cli`.

- **`add`** — Persist knowledge (prefix content with '-- ' if it starts with '-')
- **`backup`** — Export database to SQL dump (optional path arg)
- **`blob`** — Blob storage management (gc)
- **`call`** — Universal tool boundary (JSON): mpm call <tool> [--payload <json>] [--payload-file <path>] | (stdin)
- **`capability`** — Manage capabilities (seed, lifecycle, governance)
- **`cascade`** — Materialize cascade intents
- **`challenge`** — Flag memory as obsolete (atomic theory + patch; use 'restore' subcommand to undo)
- **`compact`** — Operator commands for the compaction deferral lifecycle (deferred|requeue-deferred)
- **`config`** — Configure LLM and embedding profiles manually (wizard | show | get | set | profile | component | capability)
- **`continue`** — Resume previous session — composes working context, wake context, decisions, skills, theories
- **`debug`** — Low-level debugging tools
- **`decide`** — Record decision (cognitive verb for 'record_decision')
- **`decision`** — Decision CRUD
- **`decisions`** — List decisions
- **`directives`** — Show behavioral directives
- **`doctor`** — Run substrate diagnostics (--deep-scan for FTS/integrity audit, --explain for FTS5 query plan)
- **`drills`** — Behavioural drill execution + compatibility matrix (list|show|run|report)
- **`evidence`** — Manage evidence (add|list) — confidence foundation
- **`export`** — Export memories to JSON
- **`gc`** — Run decay sweep (--dry-run, --review, --purge)
- **`handoff`** — Manage handoffs (write|read|list|shred)
- **`help`** — Show CLI catalogue
- **`hint`** — Surface context-relevant artifacts
- **`info`** — Show install identity (version, database, models, scheduler, skills, persona, counts)
- **`ingest`** — Import from external SQLite
- **`integration`** — Cross-framework config emitters (export-mcp)
- **`kb`** — Knowledge-base operations (memory|topic|lesson|reference)
- **`learn`** — Curate lesson (cognitive verb for 'mpm lesson add')
- **`lesson`** — Manage lessons
- **`lifecycle`** — Lifecycle asymmetry notes for artifact families
- **`lint`** — Validate router frontmatter (YAML + regex compile)
- **`list-skills`** — List skills (scope: all|local|shared)
- **`ls`** — List memories
- **`maintain`** — Run self-maintenance sweep
- **`memory`** — Manage memories
- **`migrate`** — Import from markdown/JSON (alias to ingest for non-SQLite sources)
- **`mode`** — Manage modes
- **`ops`** — Engine-room operations (maintenance, diagnostics)
- **`patch-memory`** — Patch memory metadata
- **`persona`** — Manage personas
- **`promote`** — Mark as durable (exempt from decay)
- **`propose_theory`** — Propose hypothesis
- **`provenance`** — Show artifact provenance (mpm provenance help for subcommands)
- **`prune`** — Prune expired memories
- **`read-skill`** — Read skill by name (or id) and optional version
- **`recall`** _(aliases: s)_ — Recall relevant context
- **`record_decision`** — Record decision (full substrate form: context, choice, rationale)
- **`reference`** — Manage reference documents
- **`reinforce`** — Record reinforcement (provenance/display; does not affect current retrieval ranking — see §6.3)
- **`remember`** — Persist knowledge (cognitive verb for 'mpm add')
- **`resolve_theory`** — Resolve theory
- **`restore`** — Restore soft-deleted memory
- **`restore-db`** — Restore database from SQL dump
- **`review`** — Start spaced-repetition review
- **`rm`** — Delete memory
- **`route`** — Route prompt to mode/persona (Claude Code hook input)
- **`save-skill`** — Save skill from markdown file (--file, --name, --version, --force)
- **`session`** — Manage sessions
- **`set-weight`** — Set stored weight 0–100 (decay/LTM/provenance; does not affect current retrieval ranking — see §6.3)
- **`show`** — Show memory details
- **`shred`** — Hard-delete memory
- **`skill`** — Skill CRUD + workshop (form | refine)
- **`snooze`** — Suppress from recall (temporary)
- **`stats`** — Show memory statistics
- **`status`** — Show system status
- **`switch`** — Switch persona/mode interactively
- **`synthesize`** — Merge near-duplicate memories
- **`tasks`** — Manage scheduled tasks (upsert|list|delete)
- **`theories`** — List theories [pending|resolved|all]
- **`theorize`** — Propose theory (cognitive verb for 'propose_theory')
- **`theory`** — Theory CRUD
- **`topic`** — Manage topics
- **`tour`** — Walk through cognitive verbs (--demo auto-runs each step; --step N jumps)
- **`version`** — Show version + build identity
- **`wake`** — Show wake context (--json, --strict)
- **`weaken`** — Record weakening (provenance/display; does not affect current retrieval ranking — see §6.3)
- **`why`** — Show artifact provenance (evidence + confidence + retrieval)
- **`work`** — Working context scratchpad — status|show|clear|promote

<!-- cli:end — auto-generated by `go run ./cmd/gen-cli`. Do not edit by hand. -->

---

## 9. Runtime Services

The Runtime is where MPM evolves. Services documented here are intentionally decoupled from Core — they may be redesigned, replaced, or removed without invalidating existing knowledge.

Three groups organize the runtime substrate: **Scheduling** (when system work fires), **Behaviour** (what governs and holds the agent at runtime), **Infrastructure** (the substrate plumbing that the other two depend on). External interface surfaces (MCP, CLI, registry) live in §6.4 and §8.

### Scheduling

#### Scheduled Wakes (Stateless, Opportunistic by Default)

*One-off reminders that surface on the next MCP call — no separate process required for self-scheduled work.*

The agent can defer work to a future moment with `mpm call mpm_wakes --payload '{"action":"schedule",...}'` and have the reminder surface automatically on the next call. The database is the queue, the next call is the dispatcher — no scheduler process required for this path.

```
mpm call mpm_wakes --payload '{
  "action": "schedule",
  "params": {
    "reason": "Check WC2026 R32 result for Germany",
    "target_time": "24h",
    "theory_id": "7383f1572d73bc9a"
  }
}'
```

`target_time` accepts either an absolute unix-epoch integer, an RFC3339 timestamp, or a relative duration (`30s`, `5m`, `2h`, `1d`, `7d`). Resolves to absolute at insert time and is returned in `target_iso` for log clarity.

The "agent has initiative" effect: any subsequent `mpm call` (CLI or MCP) that lands after `target_time` surfaces the wake inline as a `WakesPending` block in the response. The agent sees it, evaluates the reason, optionally schedules the next round. No missed reminders even if the agent is asleep — the next call after the target picks it up, marked with `overdue_secs` for honest accounting.

Companion tools: `check_wakes`, `list_wakes`. Architecture: `scheduled_wakes` table + composite index `scheduled_wakes_due(fired, target_time)` + FTS5 virtual table for content search. `CheckPendingWakes` runs in a single transaction (idempotent across concurrent callers).

**Trade-off vs. a real-time push daemon:** MCP has no server-initiated messages over stdio, so `mpm-mcp` cannot fire a wake back to a sleeping agent. The opportunistic fold is the next-best mechanism — at-most-once-on-next-contact, not real-time. For Wimbledon R1, WC2026 group stage, and monthly Meshal reminder use cases this is sufficient. Real-time push would require an SSE transport change and is deferred.
#### Autonomous wake execution (mpm-scheduler + mpm-critic)

*System-kind wakes (snapshot, critic, GC, broadcast) execute unattended via the 60s ticker — for the tasks the agent would forget.*

For system-level actions that must run unattended regardless of user presence (pre-flight snapshots, critic audits, GC sweeps, broadcasts), `cmd/mpm-scheduler` is a companion Go daemon that consumes `scheduled_wakes` on a 60s ticker. Wakes tagged with `metadata.kind=snapshot|critic_audit|gc|broadcast` are dispatched to registered handlers and execute inline; after each handler returns, the scheduler calls MarkFired (`fired=1`) which here records the system-kind terminal transition (the handler's completion is itself the terminal event — internal to the scheduler, no separate user/agent ack is needed). Notification-kind and untagged wakes pass through to the opportunistic fold unchanged. `cmd/mpm-critic` is the standalone runner for one audit cycle — the scheduler's `critic_audit` handler shells out to it. Install via `make build`; ship under systemd as a user service for persistence. Both binaries are first-class artifacts (Go, no shell wrappers). The two-way bridge with `mpm-mcp`: `CheckPendingWakes` filters system kinds from the opportunistic fold so the two surfaces don't race for the same wake.
#### Agentic Cron (Recurring Tasks)

*Recurring workflows with fail-fast directive validation — catch typos at upsert, not silent wake drops at 3 AM.*

For work that should run **on a schedule** rather than once, `scheduled_tasks` is the registry of recurring agentic workflows. The `mpm-scheduler` daemon's 60s tick loop polls `scheduled_tasks WHERE status='active' AND next_run_at <= ?` and, for each due task, injects a standard `scheduled_wakes` row in the same transaction as the `next_run_at` rollover. A daemon crash between injection and rollover cannot double-fire. The injected wake surfaces to the agent on its next MCP call via the standard opportunistic fold.

```
mpm tasks upsert epistemic-compaction \
  "Nightly epistemic compaction" \
  "0 3 * * *" \
  mpm-seed-epistemic-compaction active

mpm tasks list
mpm tasks delete epistemic-compaction
```

Three split tools (matches the `schedule_wake` / `list_wakes` pattern — discrete beats multiplexed):

| Tool | What |
|---|---|
| `upsert_scheduled_task` | Create or update a recurring task. **Fails fast** if `directive_id` does not exist in `memories WHERE collection='directives'` — a single indexed SELECT catches the typo at upsert time rather than silently dropping the wake at 3 AM. |
| `list_scheduled_tasks` | Read all tasks ordered by `next_run_at ASC`. |
| `delete_scheduled_task` | Hard-delete. Most operators should set `status='paused'` via upsert for soft-stop. |

**Five-field cron syntax.** Standard format (`minute hour dom month dow`). `0 3 * * *` = daily 03:00 UTC, `0 0 * * 1` = weekly Monday midnight, `*/15 * * * *` = every 15 minutes. Parsed at upsert time by `github.com/robfig/cron/v3`; the daemon never parses cron on the hot path — it just reads the pre-computed `next_run_at` from the index.

**Re-upsert semantics.** Calling `upsert_scheduled_task` with an existing id recalculates `next_run_at` from now and updates the cron/directive/status. The existing row's `created_at` and `last_run_at` are preserved. Status='paused' for six months then status='active' does NOT backfill missed fires — it waits for the next cron occurrence from the unpause moment.

**Poison-pill handling.** If `CalculateNextRun` fails at rollover time (operator typo, mid-flight cron corruption), the offending task is `paused` rather than deleted, and the loop continues. Better to halt than to spin.

**Atomic transactional pattern.** The polling function wraps three operations in a single SQLite transaction:

```sql
BEGIN;
  -- 1. SELECT due tasks
  SELECT id, cron_expr, directive_id FROM scheduled_tasks
    WHERE status='active' AND next_run_at <= ?;
  -- 2. INSERT wake rows (one per due task)
  INSERT INTO scheduled_wakes (id, target_time, reason, created_by, metadata)
    VALUES (?, ?, 'cron:<task_id>', 'mpm-scheduler',
            '{"source":"cron","task_id":"<id>","directive_id":"<id>"}');
  -- 3. ROLLOVER next_run_at
  UPDATE scheduled_tasks
    SET last_run_at=?, next_run_at=?, updated_at=?
    WHERE id=?;
COMMIT;
```

The wake injected by the cron engine has `reason='cron:<task_id>'` and `metadata={source:'cron', task_id, directive_id}`. The agent sees the wake on its next call, parses `cron:` prefix to recognize the source, reads `metadata.directive_id`, looks up the directive via `mpm_context action=read_directives`, executes. **No new wake-handling code path** — the cron engine plugs into the existing wake queue.

**Companion schema** (for the database-design curious):

```sql
CREATE TABLE scheduled_tasks (
  id           TEXT PRIMARY KEY,        -- semantic slug (e.g., 'epistemic-compaction')
  name         TEXT NOT NULL,            -- human label
  cron_expr    TEXT NOT NULL,            -- '0 3 * * *'
  directive_id TEXT NOT NULL,            -- FK target: memories.id where collection='directives'
  status       TEXT CHECK (status IN ('active','paused')),
  last_run_at  DATETIME,
  next_run_at  DATETIME NOT NULL,        -- pre-computed; daemon polls on this column
  created_at   DATETIME,
  updated_at   DATETIME
);
CREATE INDEX idx_scheduled_tasks_poll ON scheduled_tasks(status, next_run_at);
```

The composite index on `(status, next_run_at)` is the daemon's hot path: a single indexed lookup, never a table scan, even with thousands of registered tasks.

**Architecture choice — why pre-compute `next_run_at`.** Three options were considered:
1. **Compute `next_run_at` at upsert, query it on the hot path** ← chosen
2. Store `cron_expr`, parse and evaluate on every tick — simple but blocks the daemon on cron parsing
3. Cache `next_run_at` but invalidate on cron change — adds a "stale read" code path

Option 1 wins because the hot path is a single indexed lookup; option 2 wastes CPU on every tick; option 3 introduces cache invalidation correctness concerns. The cost is that re-upserting a task recomputes the schedule from now (documented behavior, not a bug).

**Migration note.** The existing `scheduled_wakes` table has a dormant `recurring_rule TEXT` column that was accepted by `schedule_wake` but never honored by any daemon code path. Recurring workflows now live in `scheduled_tasks`; `recurring_rule` is preserved for backward compatibility with the `Wake.RecurringRule` struct field but documented as superseded. A future schema-version bump can drop it cleanly when no callers remain.
#### Event Wakes — Active Dissemination (Arc 2)

*Other-directed pushes (rule bodies, resolutions, arbitration verdicts) propagate via the shared DB — pull becomes push for known events.*

Local scheduled wakes (above) are **self-directed** — the agent schedules a reminder for itself. **Event wakes** are **other-directed** — when an epistemic event lands (a new house rule, a contradiction resolution, an arbitration verdict), the shared DB pushes a wake to every other active session on the fleet.

```
mpm call mpm_wakes --payload '{"action":"check_pending_event","params":{"session_id":"..."}}'
```

**Deterministic dedup.** `wake_id = sha256(memory_id + ":" + target_session + ":" + content_hash)[:12]` is the PRIMARY KEY of `shared.bcast_event_wakes`. `INSERT OR IGNORE` is the dedup mechanism — database-level, O(1), race-free, no app-level state. Re-broadcasting the same content is a no-op; a rule body text change fires a new wake; a metadata-only update (e.g., `status=challenged`) does not.

**Three-state wake table.**

| Column | Purpose |
|---|---|
| `wake_id` (PK) | Deterministic sha256 prefix → dedup |
| `target_session` | FK to `shared.bcast_sessions` |
| `source_agent` | Who broadcast (operator + agent ID) |
| `memory_id` | The artifact that triggered the wake |
| `kind` | `rule` / `resolution` / `arbitration` / `memory` |
| `content_hash` | Drift detection |
| `rationale` | The WHY (non-negotiable) |
| `created_at` | Unix epoch |
| `fired` | Boolean; flipped to 1 by `CheckPendingEventWakes` in a transaction |

**Why this is different from a pull-based `query_global_rules`.** Federation (Layer 0 in §6.5) is pull: an agent decides to query, the system responds. Event wakes are push: the shared DB has decided something changed, and the change is **already in the agent's wake context** on the next call. No search, no cold-start, no missed signal — even a fresh process picks up pending wakes immediately.

**Auto-broadcast hooks.** Three operator commands trigger automatic broadcasts as a strict side-effect of the explicit `--confirm` / `--apply` / `--winner` flag. Auto-broadcasts are **non-fatal**: a broadcast failure does NOT roll back the underlying write. The resolution is ground truth; the broadcast is the notification. See §6.5 Layer 4 for the architecture and §6.5 Layer 3 for how the underlying rule/resolution/arbitration actually lands.

**Why the deterministic ID is the entire architecture.** Without the PRIMARY KEY, dedup is an application-level check: SELECT then INSERT, with a TOCTOU race. With the PRIMARY KEY, dedup is a database-level guarantee: `INSERT OR IGNORE` is atomic, idempotent, and O(1). The whole receiver-correctness story is the deterministic ID; the rest is plumbing.

For the schema, fan-out algorithm, test matrix, and smoke behind this section, see **Appendix B**.
#### Spaced Reinforcement Review

*Surface forgotten LTM memories for re-touch — knowledge decays if not exercised.*

```bash
mpm ops review --promoted   # Show recently elevated LTM memories
mpm ops review --stale      # Surface forgotten LTM memories
```


---


### Behaviour

#### Session Memory Context (`wake`)

*The bootstrap surface — `mpm wake` returns mode, persona, topics, recent memories so the agent starts each session with full context.*

`mpm wake` surfaces the last session's mode, persona, topics, and recent memories — the agent's bootstrap context on startup.
#### Ephemeral Scratchpad (Working Context)

*Per-session Working Context with security-scanner-gated promotion — tentative thoughts survive in a protected space until they’re ready to defend.*

A per-session Working Context for hypotheses, intermediate state, and execution tracking that aren't ready for permanent memory. Single-row-per-session, 24h decay, atomic promote. The scratchpad sits between "thought I had this turn" and "memory I'm willing to defend." Convention enforced via the `flush_scratchpad` MCP tool description (survives fresh install without a database-resident skill).

**The four verbs:**

| Verb | What it does |
|---|---|
| `flush_scratchpad` | Overwrite the Working Context for a session. Idempotent on `session_id`. The convention is to overwrite (not append) to keep the context concise and avoid context crunch. |
| `read_scratchpad` | Retrieve the agent's current Working Context. Use at session start to recover state, mid-task to verify the latest checkpoint. |
| `discard_scratchpad` | Hard-delete the scratchpad without promoting. |
| `promote_scratchpad` | Atomically promote the Working Context into a permanent memory. |

**Working Context template.** When starting a multi-step task, scaffold the scratchpad using this exact markdown structure so the next agent (or your future self after a context refresh) can pick up cleanly:

```
Working Context
Goal: [What are we trying to achieve?]
Current State: [What was the last action taken?]
Completed:
  - [x] Step 1
Next Actions:
  - [ ] Step 2
Open Questions:
  - [Unknowns to resolve]
Relevant Context: [IDs of memories/skills in use]
Exit Criteria: [What constitutes completion? When do we wipe this?]
```

The `Exit Criteria` line is the discipline that prevents context crunch: without an explicit completion definition, agents keep working past the goal and accrue context needlessly. When the Exit Criteria is met, **wipe the scratchpad clean by passing an empty string** to `flush_scratchpad`. If you learned something durable during the task, `mpm_lessons action=save` before wiping.

**The atomic rollback is the entire point.** `promote_scratchpad` runs the security scanner against the synthesized memory *inside* the same transaction as the memory INSERT and the scratchpad DELETE. If the scanner rejects (poison-phrase match, sensitive content, etc.), the entire transaction aborts: the scratchpad row survives for the agent to amend, and no memory row is created. A thought that fails the scanner is **not lost** — it is preserved for revision.

**Wake-context orphan surfacing.** A scratchpad from a session that never promoted becomes an *orphan* on the next wake. `mpm_context action=read_wake_context` includes an aggregate header (Fresh / Dormant / Expired counts) and a truncated thesis preview for each orphan. The agent's options: `promote_scratchpad` (commit), `flush_scratchpad` (amend), or `discard_scratchpad` (abandon). This closes the "agent crashed mid-thought" gap — even a process kill between flush and promote doesn't lose the work.

**Decision boundary vs `mpm_memory action=save`:** the scratchpad is for *tentative* thoughts — working hypotheses, half-formed theories, intermediate conclusions that may need revision. `mpm_memory action=save` is for *committed* facts. The promote path is the gate: if you're not ready to defend a thought against the security scanner and future challenges, it belongs on the scratchpad first.

**Trade-off vs. auto-promote-on-flush:** `flush` never auto-creates a memory. The agent must explicitly `promote_scratchpad` and pass the scanner. This is the right friction: tentative thoughts should be deliberate before they become permanent knowledge. A flush that auto-promoted would silently double the agent's memory-write surface, bypassing the scanner's intent.
#### Retrieval Observability Layer & Provenance Proxy

*Per-node telemetry for adaptive retrieval — reuse and success counts credited automatically when nodes are pulled into agent working memory or cited as lesson sources.*

The `retrieval_metadata` table is a 1:1 mapping with any cognitive node (Memory, Lesson, Decision, Theory, Skill). It tracks two signals: how often a node was surfaced into agent working memory (`reuse_count`, `last_retrieved_at`), and how often it actively helped produce durable knowledge (`success_count`). The schema is observability only — search ranking, FTS sorting, and cognitive-object schemas are unchanged. Future rankers can consult `retrieval_metadata` to blend a reuse-adjusted score; today `DefaultRanker` returns the FTS score unchanged.

**Automatic instrumentation.** The agent never manages these stats explicitly. Every `mpm_context action=read_wake_context` boot, every `mpm_memory action=query` result, every `mpm_lessons action=query` result, and every `mpm_skills action=read` call increments `reuse_count` and updates `last_retrieved_at` via `RecordRetrieval` — fire-and-forget, errors swallowed so telemetry never blocks the user-facing path.

**Provenance Proxy via `mpm_lessons action=save`.** When the agent distils a lesson, the optional `source_ids` array credits each cited node with a `success_count` increment via `IncrementSuccess` (INSERT-or-UPDATE). A node that was already retrieved has its `success_count` bumped; a node cited from prior-session memory but not surfaced this turn gets a fresh row with `success_count=1, reuse_count=0`. The "Provenance Proxy" name reflects the design intent: from any successful lesson, the system can trace back to the cognitive nodes that informed it, even when those nodes were never re-read in the session where the lesson was synthesized.

```json
{
  "fact": "When SaveSkill is force-overwriting an older version, the semver comparison must run — hardcoded is_latest=true via the metadata init was the production bug.",
  "type": "warning",
  "tags": ["mpm", "skills", "semver"],
  "source_ids": ["skill:agentshell-v1.0.0", "skill:agentshell-v2.0.0"]
}
```

The handler infers `node_type` from the id prefix (`skill:`, `lesson:` / `les-`, `memory:`, `dec-`, `theory:` / `the-`) and falls back to `memory` for unknown formats. The response includes a `credited_sources` count so the agent can verify the provenance was wired.

**Diagnose with `explain_retrieval`.** The MCP tool `mpm call explain_retrieval --payload '{"query":"..."}'` runs a standard FTS search and returns a per-node diagnostic markdown block: Base FTS Match score, Reuse Count, Last Retrieved timestamp, Success Count. The retrieval ordering is identical to `mpm_memory action=query` — the `bm25()` rank is preserved. `explain_retrieval` layers observability on top; it does not alter ranking.

**Why observability first, ranking later.** The system records retrieval patterns for a month before any ranker consults them. Today, `reuse_count` and `success_count` are facts the operator can read; tomorrow, a future ranker can use the same data to promote frequently-cited memories and demote never-reused ones. The schema, the instrumentation, and the UPSERT path are the durable substrate; the ranker is the consumer that hasn't shipped yet.
#### XITL Stance Hot-Swap

*Runtime mode/persona switching without restart — the directive prints to stdout, OpenClaw injects, the agent adopts on next turn.*

`mpm ops stance assume <mode> <persona> <rationale>` switches mode/persona at runtime with no restart. The new directive prints to stdout — OpenClaw captures it and injects into session chat history, so the agent reads and adopts it on the very next turn. `mpm ops stance synthesize <name>` generates a JIT ephemeral persona from a prompt; `mpm ops stance promote` flushes it to a permanent `.md` file.
#### Security Scanning

*19 regex patterns gate every memory write — blocked content goes to the mirror log but never reaches the database.*

Content scanned against **19 regex patterns** (API keys, JWTs, SSH keys, connection strings, password patterns) before any database write. Blocked content goes to `mirror.jsonl` but never reaches the database. Coverage enforced by a static-analysis test that walks every function containing a literal `INSERT INTO memories` and verifies the function (or its caller) calls the scanner.
#### Directives (Prime Operating Principles)

*Prime operating principles stored as memories — non-negotiable behavioral rules that govern every turn.*

Directives are the agent's **prime directives** — non-negotiable behavioral principles that govern how it operates. Unlike modes (which govern retrieval parameters) and personas (which govern tone), directives are the hard rules: the things the agent must and must not do on every turn.
#### Reflex Engine — two-tier behavioral rules

| Tier | Lives in | Loaded | Examples |
|---|---|---|---|
| Prime Directives | SQLite `collection='directives'` | Always, via `mpm_context action=read_directives` | "Always log contradictions as challenge evidence, never overwrite" |
| Mode Directives | `mode/*.md` files | Only when the agent is in that mode | "Query confidence history on suspicious memories before debugging" |

Storage: `collection='directives'` (MCP path) or legacy `is_prime_directive = 1`. Both read paths query `WHERE (collection = 'directives' OR is_prime_directive = 1) AND deleted_at IS NULL`, so a directive is visible from every consumer regardless of which identifier was set.

Elevation: `mpm call mpm_memory --payload '{"action":"save","params":{"fact":"Always verify before acting","collection":"directives","tags":["prime_directive"]}}'`.

The `mpm_context action=proactive_recall_hint` engine also elevates directive-adjacent memories when the current conversation context matches their semantic territory.
#### Baseline Cognitive Bootstrap

For fresh installs, MPM ships a small set of reference directives that close the system's most important cognitive loops (wake-context reading, session-end cluster triage). Seed them once with `mpm ops init directives` — idempotent, never overwrites local edits. See §5 step "Initialize baseline directives" for context.

> **Implementation note:** The reference directives live in `internal/seed/directives.go`. The bootstrap command detects existing directives by stable ID and skips them; local edits to a seeded directive are preserved, never silently overwritten.

#### Multi-Framework Scope

Directives can target a specific agent framework, the whole substrate, or both at once. Scope lives in metadata JSON on the directive's `memories` row; evaluation is centralised in MPM core, not in agent plugins.

Grammar (flat, case-sensitive strings):

```text
scope = "global" | "framework:<id>"
```

- `"global"` — every agent framework receives this directive
- `"framework:<id>"` — only the named framework receives it. Standard ids: `openclaw`, `opencode`, `pi`, `claude-code`, `hermes`
- empty / unset — treated as `"global"` at materialisation time (legacy-row backward compat — rows seeded before scope existed match the same predicate)

How a framework identifies itself: the MCP host exports `MPM_PROVENANCE_FRAMEWORK=<id>` on the `mpm-mcp` child process env (the canonical name, same pattern as `MPM_ACTIVE_MODE`/`MPM_ACTIVE_PERSONA`). `mpmcli.ActiveContextFromEnv` reads `MPM_PROVENANCE_FRAMEWORK` first and falls back to the legacy alias `MPM_FRAMEWORK` (used by all five `agent_installation/` adapters for backward compatibility), populating `ActiveContext.FrameworkName` in either case. The MCP `handleReadDirectives` uses that to filter via `ReadDirectivesForFramework(fw)`. When both env vars are unset, `FrameworkName` defaults to `"mcp"` — existing single-MCP callers see no behaviour change.

Resolution model (additive union, evaluated by MPM):

```text
Active Directives = Global(Directives) ∪ MatchingScoped(Directives, fw)
```

Global directives always apply. Framework-scoped directives are added on top — they never replace global invariants, so a `framework:opencode` row can never accidentally swallow `mpm-seed-read-wake-context`. Within the active set, deterministic ordering by `StableID` ASC keeps the agent's wake context byte-stable across runs.

Operational example (OpenClaw):

```bash
# 1. Tag the agent framework at the MCP layer
openclaw config set mcp.servers.mpm.env.MPM_FRAMEWORK openclaw

# 2. Seed a framework-specific directive when OpenClaw tool conventions diverge
mpm call mpm_memory --payload '{
  "action":"save",
  "params":{
    "fact":"OpenClaw-specific behavioural rule",
    "collection":"directives",
    "tags":["prime_directive","openclaw"],
    "metadata":{
      "scope":"framework:openclaw",
      "stable_id":"openclaw-tool-conventions-v1"
    }
  }
}'

# 3. Verify the wiring — admin view shows every directive regardless of scope
mpm directives

# 4. Runtime agent wake context reflects only the applicable subset,
#    and the response envelope reports which scope was applied
mpm call mpm_context --payload '{"action":"read_directives","params":{}}'
# → response: { "success":true, "directives":[...], "count":N, "framework":"openclaw" }
```

##### Precedence and conflicts

1. **Additive, not replacement.** Framework-specific directives augment global directives. They never overwrite or suppress a global invariant.
2. **Tiered overlay.** With a shared DB attached, `ReadDirectivesForFramework` returns the union of the local baseline and the shared overlay, deduplicated by id. On an id collision the **shared row wins** — the shared DB is the multi-agent authority. Sorting is by ascending id, so the returned set is byte-stable across runs.
3. **Deterministic ordering.** Within an active set, directives are ordered by ascending `StableID`.
4. **Conflict boundaries.** Two directives carrying contradictory behavioural rules are an authoring defect in the seed registry, not a runtime condition to resolve. There is no override weighting and no numerical priority field — deliberately, because a priority field turns every authoring mistake into a silent precedence puzzle.

##### Tiered fallback seeding

```text
Tier 1  LOCAL bootstrap  ── always seeded at NewDatabaseManager boot
Tier 2  SHARED overlay   ── union when MPM_SHARED_DB is attached
```

The constitutional baseline directives are auto-seeded into the **local** store at boot: `NewDatabaseManager` runs `seed.ApplyDirectives` after `initUnifiedSchema`, so a standalone runtime with no attached shared DB is never directive-blind. The hermetic test constructor (`NewDatabaseManagerForDB` + `InitSchema`) deliberately does not seed, so fixtures assert against their own rows.

Seeding is idempotent, keyed on stable id:

| Row state | Outcome |
|---|---|
| Live, content matches the seed | Skipped |
| Live, content has drifted | Operator's edit preserved, flagged `Updated` |
| Absent | Inserted (`Created`) |
| Soft-deleted | **Revived** with current seed content (`Created`) |

The revive case matters: a shredded baseline still occupies the stable-id primary key, so a plain `INSERT OR IGNORE` would be silently swallowed while reporting "Created". The baseline is non-negotiable, so re-init undeletes it. That makes `mpm ops init directives` the documented recovery path after an accidental shred or a decay sweep.

##### Liveness is sentinel-agnostic

Legacy installs wrote `deleted_at = 0`; current code writes `NULL` and soft-deletes with a Unix-epoch value. Both `ReadDirectivesForFramework` and the seed dedup treat liveness as `COALESCE(deleted_at, 0) = 0`, so a legacy database surfaces its directives instead of silently reporting an empty baseline. Operators with pre-2026-08-19 databases should normalize once:

```sql
UPDATE memories SET deleted_at = NULL WHERE deleted_at = 0;
```

This also restores visibility of any other `deleted_at = 0` memories, which every current reader treats as live.

##### Authoring rules

1. **Constitutional invariants must be global.** Any rule governing storage integrity, transaction safety, write read-backs, or audit retention uses `scope = "global"`.
2. **No framework assumptions in global directives.** Write them in third-person, framework-neutral language. Never reference a specific CLI flag, JSON-RPC envelope quirk, or harness hook in a global directive.
3. **Framework directives must be additive.** Keep them to integration mechanics, local context constraints, and tool-invocation ergonomics — never behaviour that belongs to every agent.

#### Skills (Procedural Memory)

*Markdown-frontmatter procedures stored as `collection='skills'` rows — discoverable via list / read / proactive_recall_hint, shareable to shared DB with operator consent.*

Skills are the fourth cognitive collection (alongside memories, lessons, directives). Where a directive says "always log contradictions as evidence" (a *rule*), a skill says "to handle an agentshell config update, call `agentshell_get_config`, then `agentshell_set_css_var`" (a *procedure*). Skills do not run on their own — the agent reads the steps and interprets them.

##### Format

A skill is a single markdown document with YAML frontmatter. Only `name` and `version` are required; the rest are surfaced to discovery tiers.

```markdown
---
name: agentshell
version: 2.0.0
description: Configure the AgentShell WordPress theme.
when_to_use: agentshell, theme, MCP config
domain: wordpress
constraints:
  - never edit header.php directly
  - always read get_config before writing
steps:
  - call: agentshell_get_config
  - call: agentshell_set_css_var
---
# AgentShell

Full markdown body. The agent reads the body for context; the steps
in frontmatter are guidance, not a workflow engine — the agent
interprets and adapts.
```

The complete frontmatter contract: `name` (required), `version` (required semver), `description`, `when_to_use` (discovery hook), `domain`, `constraints`, `steps` (each is `{call: string, args_from?: string}`). Invalid frontmatter (missing name or version, unterminated YAML block) is rejected at save time.

##### Authoring

Three paths exist, each routed through the secret/poison scanner — no write path bypasses `ScanContentForWrite`, enforced by both `TestScannerCoverage_AllMemoriesWritersScanContent` (static AST walk) and `TestScannerCoverage_SkillsWritePaths` (runtime end-to-end check):

| Path | When to use it |
|---|---|
| `mpm save-skill --file path/to/SKILL.md` | Operator curation from terminal — you have a finished markdown file with frontmatter |
| `mpm call mpm_skills --payload '{"action":"save","params":{...}}'` | Programmatic creation by the agent — the caller already has content and version |
| `mpm skill workshop --file request.json` | Guided formation or refinement — see the Skills Workshop section below |

The first two paths assume the caller already knows the skill is worth storing. The third path is the **formation/refinement gate**: it evaluates whether the proposed procedure actually belongs in the procedural-memory layer, derives a deterministic version when refining, and only writes when the decision model + validation + duplicate checks agree.

Saving the same `(name, version)` pair requires `force=true` — silent overwrites are rejected. Saving a new version for an existing name flips the prior version's `is_latest` to `0` in the same transaction and stamps `supersedes` linkage, so older versions remain queryable but no longer advertise themselves as current.

##### Skills Workshop

The Skills Workshop is the formation/refinement gate for procedural knowledge — the missing layer between "we keep rediscovering this procedure" and "this skill is durable." It extends the existing `mpm_skills` tool with a single `workshop` action; the underlying persistence and validation architecture is unchanged. Both the CLI (`mpm skill workshop`) and the MCP aggregator (`mpm_skills` action `workshop`) call the same canonical `internal.RunWorkshop` pipeline — the CLI is a thin JSON-loading shim, not a duplicate pipeline.

The Workshop exists because *not every useful observation is a skill*. Skills compete for discovery attention, share the substrate with lessons and memories, and require maintenance. The Workshop gates publication so that:

- only a **procedure** (a "how to act" that the agent will repeat) is eligible to become a skill
- **judgment** (reasoning/choice guidance) and **knowledge** (information to recall) are rejected and routed elsewhere — to `mpm_lessons save` or `mpm_memory save`, not to the skill collection

The agent's role is to **form** or **refine**; the Workshop's role is to decide whether the proposal is skill-shaped and, if so, to write it.

**The decision model.** Each request carries a `decision_model` that scores the proposal along four axes (each integer 0..5) plus a `boundary` enum:

| Field | Range / values | Meaning |
|---|---|---|
| `reusability` | 0–5 | How often will this procedure recur across sessions or contexts? |
| `non_obviousness` | 0–5 | How hidden is this from existing documentation or skill catalog? |
| `stability` | 0–5 | How unlikely is the procedure to change materially in the near term? |
| `leverage` | 0–5 | How much downstream value does reusing this procedure unlock? |
| `boundary` | `procedure`, `judgment`, `knowledge` | What kind of knowledge is this? See below |

`boundary` is the gate that distinguishes a skill from a lesson or a memory:

- `procedure` — "how to act." Eligible for the skill layer.
- `judgment` — "how to choose." Reasoning/decision guidance; route to `mpm_lessons` or `mpm_decisions` instead.
- `knowledge` — "what to know." Information recall; route to `mpm_memory` instead.

The numeric total (`reusability + non_obviousness + stability + leverage`, range 0–20) is the *first* gate, but not the only one. The full outcome path is:

1. `boundary != procedure` → **rejected** (the workshop refuses to publish non-procedures regardless of score).
2. `total <= 3` → **rejected**.
3. `total 4..5` → **candidate** — return the proposal so the caller can decide whether to publish it via a direct `mpm_skills save`.
4. `total >= 6` → **published**, *subject to* later downgrade. The Workshop runs additional checks (when_to_use quality, duplicate detection against existing skills, frontmatter validation). Any of those checks can flip a `published` to `candidate` before the row is written.

`mpm skill workshop --help` is the exhaustive invocation reference (boundary vocabulary, change_type mapping, full JSON envelope).

**Form.** `mode: "form"` is the path for a *new* skill. The caller supplies a proposed procedure as the `proposal` (name, version, when_to_use, steps, constraints). Publication is not automatic merely because the caller requested a skill — the decision model + validation + duplicate checks decide. The three outcomes:

- **`published`** — the skill was written through the canonical skill persistence path (`SaveSkill` → secret/poison scanner → `collection='skills'` insert). `skill_id` is populated and the row appears in `<available_skills>` on subsequent wake contexts.
- **`candidate`** — the proposal was generated but did not pass every gate. The response carries a `save_payload` — the exact `params` dict to hand to `mpm_skills` action `save` if the caller wants to publish anyway. Candidates are *not* silently persisted.
- **`rejected`** — not skill-worthy. `reason` names the gate (e.g. `decision_total_below_threshold`, `non-procedure boundary: knowledge`, `refine_target_not_found`). The caller may save a memory or lesson instead.

Idempotence: a second `form` request with the same `(name, version)` and the same content hash returns the existing `skill_id` without rewriting — safe to retry on transient failures.

```bash
# Form — author a new skill
mpm skill workshop --file form.json
```
```json
// form.json
{
  "mode": "form",
  "decision_model": {
    "reusability": 2, "non_obviousness": 2, "stability": 2, "leverage": 2,
    "boundary": "procedure"
  },
  "proposal": {
    "name": "release-checklist",
    "version": "1.0.0",
    "domain": "release",
    "description": "Pre-release smoke checks for MPM changes",
    "when_to_use": "before cutting a release, smoke-checking migrations, scheduler, and CLI",
    "steps": [{"call": "run the smoke test"}],
    "constraints": []
  },
  "task_context": "Repeated pre-release sequence",
  "workflow_description": "check migrations, scheduler, CLI"
}
```

The MCP equivalent for agents:

```bash
mpm call mpm_skills --payload '{
  "action": "workshop",
  "params": { /* same fields as the form.json above */ }
}'
```

**Refine.** `mode: "refine"` is the path for changing an *existing* skill while preserving lineage. The caller supplies:

- `intent` — the name of the existing skill being refined.
- `change_type` — the kind of change, from a fixed enum. The Workshop derives the next semantic version deterministically; the caller does *not* supply the next version directly.

| `change_type` | Version effect |
|---|---|
| `correction` | patch (1.0.0 → 1.0.1) |
| `extension` | minor (1.0.0 → 1.1.0) |
| `restructuring` | minor (1.0.0 → 1.1.0) |
| `purpose_change` | major (1.0.0 → 2.0.0) |

If `proposal.version` does not match the deterministic bump, the Workshop surfaces a `version_bump_mismatch` and returns the proposal as a candidate rather than publishing a wrong-versioned row. The prior skill row stays queryable; the new version is inserted alongside it with `supersedes` linkage, and the prior row flips `is_latest=0` in the same transaction.

```bash
# Refine — bump a correction on an existing skill
mpm skill workshop --file refine.json
```
```json
// refine.json
{
  "mode": "refine",
  "intent": "release-checklist",
  "change_type": "correction",
  "decision_model": {
    "reusability": 2, "non_obviousness": 2, "stability": 2, "leverage": 2,
    "boundary": "procedure"
  },
  "proposal": {
    "name": "release-checklist",
    "version": "1.0.1",
    "description": "Pre-release smoke checks for MPM changes",
    "when_to_use": "before cutting a release, smoke-checking migrations, scheduler, and CLI",
    "steps": [{"call": "run the smoke test"}]
  }
}
```

**Where the Workshop fits in the lifecycle.** The Workshop is a layer in the existing skill lifecycle, not a separate subsystem:

```
experience / proposed procedure
            ↓
       Skills Workshop     ← gate: decision model + validation + duplicate check
            ↓
       published skill      ← saved through normal `mpm_skills` save path
            ↓
       discovery / read / proactive_recall_hint
```

The Workshop does not introduce a separate registry, persistence layer, or MCP tool. It is one action on `mpm_skills` and one CLI subcommand on `mpm skill`. Workshop-published rows immediately become eligible for subsequent wake and discovery results — the wake skill catalogue is bounded by weight, so a particular new row is not guaranteed to appear in the top N, but no separate catalog refresh is required.

##### Discovery

Three tiers, in increasing specificity:

1. **Inventory** — `mpm call mpm_skills --payload '{"action":"list","params":{"scope":"all"}}'` (CLI: `mpm list-skills`). Returns one row per name with the highest-version row's id, name, version, when_to_use, is_global, weight. Used by wake context to render an `<available_skills>` block bounded to the top 20 by weight.
2. **Read** — `mpm call mpm_skills --payload '{"action":"read","params":{"name":"agentshell"}}'` (or `"skill_id":"skill:agentshell-v2.0.0"`). Returns the full Skill struct with parsed frontmatter and body.
3. **Proactive** — `mpm_context action=proactive_recall_hint` surfaces a skill when conversation keywords overlap its `when_to_use`. Same scoring path as memories: FTS5 BM25 + cosine + Shared Premium for `is_global=1` rows. `weight` and `reinforcement_count` are surfaced in the hint metadata but do not influence the ranking order.

##### Versioning

Skill names are stable identifiers; versions are slug-suffixed in the row id. Saving `agentshell` v1.0.0 produces the row id `skill:agentshell-v1.0.0`; v2.0.0 produces `skill:agentshell-v2.0.0`. The id format `skill:<name>-v<semver>` is deterministic — re-running the save with the same args hits the same row, which is how `mpm ops init skills` detects drift (it computes `contentHash(seed)` and compares against the existing row's stored `metadata.content_hash`).

For revision via the Skills Workshop, `change_type` selects the bump deterministically (see Skills Workshop → Refine).

##### Sharing

`mpm call mpm_skills --payload '{"action":"promote_to_global","params":{"skill_id":"skill:agentshell-v2.0.0","confirm":true}}'` flips `is_global=1` on the canonical row in place. **Operator-gated**: `confirm` must be `true`; the privilege-escalation guard (`collection='skills'` filter on the existence check and UPDATE) prevents a non-skill id from being elevated through this path. The metadata patch stamps `derived_from_skill_id` and `promoted_at` for forensic tracing. Shared skills appear in the list with `scope="shared"` and get the Shared Premium boost (1.20× shared, 1.35× shared+rules) in hybrid-search scoring.

Removal is the soft-delete `mpm_skills` action `delete`: sets `deleted_at` on the row. The scanner treats `deleted_at IS NULL` as the live-row gate everywhere, so the row vanishes from every list/read/proactive path atomically without breaking foreign keys.

##### LTM by default

Skills are written with `is_long_term=1` so the existing long-term decay machinery (slow 0.01×days rate, floor at weight 1) applies — this IS the 90-day decay floor documented in the metadata's `decay_floor_days`. The floor lives in the LTM rate rather than a skill-specific sweep because (a) the same retrieval paths that already handle directives and lessons stay correct without special-casing, and (b) skill rows remain discoverable through the same query paths. The runtime test `TestSaveSkill_SetsIsLongTerm` pins the contract; `TestDecayFloor_SkillsSurviveAggressiveDecay` pins the decay behaviour.

#### Baseline Skill Library

For fresh installs, MPM ships a small set of reference skills that close the substrate's most-used procedural loops (status reporting, health diagnostics). Seed them once with:

```bash
mpm ops init skills
```

Idempotent. Local edits to a seeded skill are preserved and surfaced as drift in the report (Created / Skipped / Drifted buckets, same shape as the directives bootstrap). The reference registry lives in `internal/core/seed/skills.go`; the engine in `internal/core/seed/engine.go` routes new rows through `dm.SaveSkill(...)` so every seeded row passes through the scanner. Re-running is a no-op.

#### Capabilities (Executable Primitives)

*Stateful, executable artifacts with a lifecycle (draft → linted → validated → probation → active → degraded → fractured) — discoverable via `mpm capability <subcommand>`, sealed at the storage-layer CHECK constraint.*

Capabilities are MPM's answer to "how does an agent actually *do* something with the substrate, not just remember about it?" Where a skill says *how* (a procedure the agent interprets), a capability is the artifact that gets *invoked* — a typed source-code payload (bash / python / jq), a declared execution domain (sandbox / restricted / trusted / operator), and a telemetry trail that feeds the fracture detector. The full lifecycle — state machine, transition table, the four tables, the forge pipeline, the executor's per-domain `bwrap` contract, the fracture and rollback cascades, and the six-layer security model — is specified in [Appendix E](#appendix-e-capability-lifecycle--specification). This section is the operator-facing surface.

##### Bootstrapping the Tier 1 primitive set

Fresh installs get four read-only primitives (`list_capabilities`, `get_capability`, `capability_lineage`, `capability_health`) seeded into the `capabilities` table by:

```bash
mpm capability seed
```

Idempotent. The seed engine (`internal/core/seed/engine_capabilities.go`) uses deterministic stable IDs (`cap.<name>`) so re-runs are clean no-ops against rows whose `source_code` still matches the bundle. Operator edits to a seeded primitive surface in the `Drifted` bucket and are never silently overwritten. A custom bundle can be merged in via `--sidecar <path>`, overriding shipped entries or adding new ones — the loader at `internal/core/seed/capabilities_loader.go` merges the sidecar into the compiled registry before the apply phase. After seeding, primitives live in `state='validated'`; the forge tick promotes them to `probation` on first invocation, then `probation` → `active` is earned by hitting the success threshold stamped at proposal time.

##### Operator-domain approval

The executor's operator-domain gate (the only execution_domain that can touch host resources outside the bwrap sandbox) refuses to invoke a capability unless `metadata.operator_approved_at` is non-zero. The bootstrap sequence is:

```bash
mpm capability seed                                     # 1. install Tier 1 primitives
mpm capability grant-operator cap.<name>                # 2. stamp operator_approved_at
                                                        #    (atomic: metadata + audit event in one tx)
mpm capability grant-operator cap.<name> --actor alice  #    optional: record who approved
                                              --reason "reviewed by sec-team"  #    optional: free-form rationale
```

`Store.MarkOperatorApproved` (`internal/core/capability/operator_approval.go`) writes both the metadata stamp and a `capability_events` row with `event_type='operator_approval'` in a single transaction, so the gate's metadata can never disagree with `mpm skill audit <id>`. Two metadata fields are stamped: `operator_approved_at` (int64 unix epoch — the field the executor's `int64FromMeta` gate reads) and `operator_approved_at_rfc3339` (human-readable sibling for the audit trail). Idempotent: re-running refreshes the timestamp + actor and writes a fresh event row. Refused on `retired` and `fractured` states (surface as `ErrOperatorApprovalRefused`, distinct from `ErrNotFound`); soft-deleted rows surface as `ErrNotFound`.

##### Storage-layer guarantees

The lifecycle matrix is enforced twice: once at runtime by `CapabilityState.CanTransitionTo`, and once at the storage layer by a `CHECK (state IN (...))` constraint generated from `AllCapabilityStates()`. The two MUST stay in sync — adding a state without updating the CHECK is a forge bug, caught by static-analysis tests in the capability package. Telemetry writes (success/failure counts, latency, invocation context) live in `capability_invocations`; synthetic state-change events live in `capability_events`. The hard separation keeps the metrics aggregation pipeline free of state-change filtering, and lets `mpm skill audit <id>` render a clean human-readable timeline without joining two tables.

##### Bootstrapping is the only way in

There is no auto-seed at install. The "Truth is external" + "no auto-noise" principles forbid silent state mutation, so an operator runs `mpm capability seed` when they want the baseline online. After CS-3 the bootstrapping sequence is complete: install primitives → review → grant-operator → invoke. Re-running `seed` is safe; re-running `grant-operator` is safe; both are idempotent and never overwrite operator drift.
#### Modes & Personas (File-Based)

*File-based retrieval parameters with auto-selection — no database, no compile step, hot-reload on mtime change.*

Modes and personas are `.md` files with YAML frontmatter. **No database, no compile step.** Filename is the identity. Each mode specifies retrieval parameters:

| Mode | retrieval_limit | retrieval_threshold |
|---|---|---|
| `debugging` | 3 | −2.5 |
| `research` | 20 | −1.0 |
| `architect` | 10 | −1.5 |
| `programming` | 5 | −2.0 |
| `standard` | 7 | −1.5 |
#### Auto-Selection (`route` tool)

Zero-latency heuristic router — scores incoming prompts against all loaded modes and personas without any LLM call, network latency, or external dependency.

```bash
mpm call route --payload '{"prompt": "I need to draft a whitepaper about our Q3 architecture"}'
```

Returns:

```json
{
  "selected_modes":   ["architect", "research", "write"],
  "selected_persona": "venkat",
  "scores": { ... }
}
```

Frontmatter schema (every `mode/*.md` and `persona/*.md`):

| Key | Type | Purpose | Routing impact |
|---|---|---|---|
| `name:` | string | Identity | none |
| `title:`, `creature:`, `vibe:`, `voice:` | string | Persona identity / voice | LLM context only |
| `patterns:` | string (regex fragments, comma-separated) | **When this component speaks** | +2 per match |
| `domain_out:` | string (regex fragments, comma-separated) | **When this component refuses** | −1 per match, logged |
| `voice_guards:` | string (prose, comma-separated) | **How this component sounds** | LLM context only |

Scoring: modes use threshold filtering (multi-select); personas use max-pooling (single-select, highest score wins, no implicit `default` fallback). Hot reload: directory mtimes monitored, no server restart needed.
#### Router Frontmatter Linter (`mpm ops lint`)

Proactive defense against the silent-failure class of bugs where hand-curated frontmatter compiles but never matches. Checks: YAML parses, regex compiles with `(?i)` prefix, raw-form `domain_out:` compiles without `regexp.QuoteMeta`. Wired into the pre-commit hook. 9 unit tests pin the contract.


### Infrastructure

#### Embedding Pipeline

*Auto-embeds on save so retrieval works without manual prep — no batch-of-one-by-one friction.*

Auto-embed on `mpm add` and on one-shot ingestion via `mpm ops ingest`. `mpm ops backfill-embeddings` provides batched, resume-safe backfill for existing memories. tiktoken (`cl100k_base`) drives token-aware chunking.

**Provider architecture.** Embeddings are generated by a configured embedding provider — the supported implementation is a locally running **Ollama** instance (default model `nomic-embed-text`, 768-dim). Ollama performs inference only; the resulting vectors are persisted inside MPM's SQLite substrate (no separate vector database, no remote service). The embedding component can also be set to `"disabled"` to opt out of semantic recall entirely. Resolution precedence is documented in `internal/core/embeddings.go` (profile → OLLAMA_* env fallback → null).
#### Memory Versioning

*Append-only revisions with `--as-of` time travel so claims are auditable, not silently rewritten.*

Every memory has an append-only version history. `mpm debug history` shows all revisions with timestamps. `mpm debug diff` computes unified diffs between any two versions. Terminal state is captured in `memory_revisions` for `--as-of` time-travel. The `--as-of` flag accepts either an RFC3339 timestamp or a unix-epoch integer (seconds).
#### Topic Auto-Suggestion

*Links new memories to existing topics automatically so the graph grows without bookkeeping.*

On `mpm add`, the system automatically suggests linking to semantically related existing topics. Topics are also auto-created for epistemology collections (`decisions`, `theories`).
#### Cross-Reference Linking

*Bounded bidirectional Memory↔Topic↔Reference edges — one source of truth, not three indexes to keep in sync.*

Bounded bidirectional Memory↔Topic↔Reference cross-refs. Links are created on save and surfaced on recall.
#### Synthesis Deduplication

*Near-duplicate memories collapse into the older one with provenance preserved — quality wins over quantity.*

Context-aware deduplication: synthesis deletes the triggering memory after LTM save, preserves oldest `created_at`, transfers topic_memberships, excludes epistemology collections, quality gate requires ≥2 candidates.
#### Reference Library

*A separate shelf of long-form sources, not promoted to memory on ingest — the operator decides what to commit.*

PDF, EPUB, HTML, Markdown ingestion with token-aware chunking (`--chunk-size`, 64–2048 tokens, default 512). Sources are diff-keyed by content hash: re-ingesting an unchanged document costs zero embedding work. Embedding is a separate phase from chunk insert, so slow embed calls never block ingest. Reference docs are a *shelf*, not a memory: they live in SQLite, are indexed for full-text and semantic search, and are surfaced to agents on demand. Admission (whether a pattern from a consulted reference is worth promoting to long-term memory) is a per-consult decision, not an ingest-time gate.

## 10. Reliability

MPM is designed for long-running autonomous operation: SQLite WAL mode, dead-letter queues, synthesis isolation, retry pipelines, event replay buffers, watchdog telemetry, overflow protection, and a closed self-healing integrity loop.

> **The Reliability Model.** MPM's guarantees are not aspirational — they are encoded in four repeating patterns:
>
> - **Source-of-truth + cache.** If two values could disagree, exactly one is authoritative; the other is a derivable cache. (§6.2 example: `confidence` is a cache; the evidence ledger is the source of truth.)
> - **Property tests, not implementation tests.** Pin the *meaning*, not the *formula*. The decay function can change shape forever; "decay never increases confidence" stays true forever.
> - **AST guard rails.** Contracts that span JSON-Schema and a handler are enforced by the type system, not the prose — the schema is a strict superset of the keys the handler reads.
> - **Self-heal whitelist + escalation.** Auto-fix only what is known-safe. Everything else escalates to a pending theory for operator review.
>
> These patterns are documented in depth in **Appendix C**. Every "this is guaranteed" claim in the body of this document maps to one of them.
>
> These patterns exist because the next architectural threat is no longer bad design — it is implementation debt. Too many artifact types, too many lifecycle states, too many special cases, too much intelligence expected from the substrate. The antidote is the same as §7's: strict enums, invariants pinned by property tests, append-only records, boring storage. Default to no.

This section is the operator-facing view of the same model: what runs, when, and what to do when it fails.

### Model Connectivity Probes

`mpm` distinguishes four states for every configured model binding:

- **configured** — the profile JSON parses; provider / model / base_url are non-empty.
- **reachable** — a real HTTP transport completes against the configured endpoint.
- **working** — a real inference request (generation OR embedding) succeeds through the production provider adapter with non-empty, parseable output.
- **healthy** — `working == true` AND the result is fresh (≤ 5 min).

`configured ≠ working`. A profile with an API key and model name is not automatically healthy.

| Surface | Behaviour |
| --- | --- |
| `mpm doctor` | Performs an active real-network probe of every generative + embedding binding. Concurrent (4 fan-out), 12s per-probe timeout, persists bounded results to `system_config[model_probe_results]`. |
| `mpm` / `mpm status` | Read-only on the probe cache. Renders `Models  ✓ N/N healthy · checked Ns ago`. Stale or missing cache surfaces `? not recently verified · run 'mpm doctor'`. **Never blocks on the network.** |
| `mpm config profile set <name> <material-field>` | After each material-field save that produces a structurally complete profile, fires a single best-effort probe and prints `✓ Connection verified · …` or `✗ Verification failed · … / Configuration was saved.` |
| Disabled (`Components["embedding"] == "disabled"`) | Reported as neutral — excluded from the health denominator. |

The probe exercises MPM's real inference path via `synth.SynthClient.DoLLMRequest` and the `EmbeddingProvider` factory — there is no second HTTP code path or alternative wire library. Failed probes never roll back valid configuration: provider outages (cold local models, transient rate limits, mid-provision credentials) are legitimate intermediate states.

### Self-Healing Integrity Loop

Long-running databases accumulate drift. Soft-deleted memories leave ghost rows in FTS5 indexes. Cascade-deletes fail to clean up all related tables. Schema migrations introduce orphans. Without a closed feedback loop, this drift degrades search quality silently and compounds over time.

MPM ships an autonomous self-heal command that detects, classifies, and acts on integrity drift without any human in the loop:

```bash
mpm ops self-heal              # run with all defaults
mpm ops self-heal --dry-run    # report drift without taking action
mpm ops self-heal --force      # bypass the 24h cooldown
mpm ops self-heal --quiet      # silent on clean state (for cron)
```

The companion on-demand audit:

```bash
mpm doctor --deep-scan         # human-readable integrity report
mpm doctor --deep-scan --fix   # clean soft-delete ghosts in place
```

#### How it works

```
                              cron / operator
                                    ↓
                          mpm ops self-heal
                                    ↓
                          runDeepScanCheck
                          (3 integrity queries)
                                    ↓
                ┌───────────────────┴────────────────────┐
                ↓                                        ↓
        known-safe drift                          unknown drift
        (soft-delete ghosts)               (FTS orphans, dangling
                ↓                             topic_memberships)
        auto-fix + lesson                           ↓
        (24h cooldown,                       pending theory
         max 1000/run)                       (theories collection)
                ↓                                        ↓
        audit trail in                              ↓
        lessons table                  next agent boot reads
                ↓                     read_wake_context, sees
        transparent.                  the theory, acts on it.
        silent.                               ↓
                                      self-healing.
                                      escalates.
```

#### Three drift classes detected

| Class | Detection query | Behavior |
|---|---|---|
| Soft-delete ghosts | `memories_fts` rows whose joined `memories` row has `deleted_at IS NOT NULL` | **Auto-fix** (known-safe) |
| FTS orphans | Any `*_fts` row whose rowid is missing from the source table | **Escalate** (unknown) |
| Dangling memberships | `topic_memberships` referencing non-existent topics | **Escalate** (unknown) |

#### Four safety boundaries on auto-fix

1. **Whitelist-only** — only soft-delete ghosts are auto-fixed.
2. **Bounded blast radius** — if ghost count exceeds `SelfHealMaxFix` (default 1000), refuse to auto-fix and escalate.
3. **Rate-limited** — 24h cooldown between same-signature auto-fixes.
4. **Audit trail** — every auto-fix writes a `lessons` table entry tagged `source=self-heal`.

#### Why known-safe vs unknown

The asymmetry is deliberate. Soft-delete ghosts are the only drift class where we have a precise model of the correct state AND a proven-safe mechanism that should have produced that state. For FTS orphans and dangling memberships, we cannot tell from inside the scan whether the FTS row is the bug or the source-table row is the bug. Auto-fixing novel drift is exactly the kind of guesswork that causes data corruption. Escalate.

The cognitive loop closes through the existing wake context: when self-heal escalates unknown drift, it injects a pending theory; the next `mpm call mpm_context --payload '{"action":"read_wake_context","params":{}}'` surfaces it; the agent acts on it. **No new wake code is needed.**

#### Recommended cron

```cron
0 4 * * 0 $HOME/.mpm/bin/mpm ops self-heal || echo "MPM drift detected: $(date)" | mail -s "MPM Self-Heal Alert" v
```

### Self-Audit Log

Runtime telemetry has historically lived in flat files (`watchdog.jsonl`, `mirror.jsonl`) — readable by humans grepping logs, blind to the agent. MPM moves that telemetry into the cognitive surface so the agent can see what went wrong across sessions.

```bash
mpm call mpm_system --payload '{"action":"query_audit_log","params":{"days":1,"limit":20}}'
mpm call mpm_system --payload '{"action":"query_audit_log","params":{"level":"error","days":7}}'
mpm call mpm_system --payload '{"action":"query_audit_log","params":{"component":"relay","days":1}}'
```

The wake context surfaces a single-line summary when errors or fatals were logged in the last 24h.

#### Storage

| Column | Type | Purpose |
|---|---|---|
| `id` | TEXT PK | Unique identifier |
| `level` | TEXT | `warn` / `error` / `fatal` (CHECK constraint) |
| `component` | TEXT | `relay`, `synthesis`, `security`, `cluster` |
| `message` | TEXT | Human-readable description |
| `stack_trace` | TEXT | Auto-captured Go stack at log point (truncated to 4KB) |
| `context` | JSON | Structured 3–5 field key-value pair |
| `created_at` | DATETIME | UTC timestamp in SQLite `YYYY-MM-DD HH:MM:SS` |

Indexes: `(level, created_at)`, `(component)`, `(created_at)`. 30-day retention enforced by the gc sweep.

#### Wired subsystems

| Component | Trigger |
|---|---|
| `security` | Poison phrase or sensitive content blocked during memory write |
| `relay` | Non-trivial HTTP error during SSE broadcast |
| `synthesis` | All LLM vendors failed; event routed to DLQ |
| `cluster` | Audit-cluster proposal raised / resolved / snoozed |

#### Why a SQLite table, not more jsonl files

The audit log is queryable from the agent via the JSON boundary, surfaceable in wake context, and pruneable by the existing gc sweep — all without new infrastructure.

#### Audit cluster proposals — three-layer self-healing

Raw audit events are the *nervous system* of the substrate, but a single error doesn't tell the agent what's been breaking. MPM runs a write-side cluster detector that aggregates repeated (component, message) pairs into structured proposals, then exposes them through the wake context and a dedicated MCP tool. The architecture is intentionally three layers so the agent can act on what it sees.

| Layer | What | Where |
|---|---|---|
| Write | Atomic UPSERT into `audit_cluster_proposals` on every `LogAudit` call. Rolling 7d window. Default threshold: 3 events. | `internal/core/audit.go::upsertClusterCounter` |
| Read | Deduped view partitioned into `known` (cluster_key in any pending theory / recent decision / resolved theory) vs `unknown`. Surfaced as a tiered string in `mpm_context action=read_wake_context`. | `internal/core/cluster_proposals.go::ActiveClusters` + `internal/core/wake_context.go::auditSummaryRich` |
| Act | `mpm call list_active_clusters` returns structured JSON (cluster_key strings) so the agent can triage at session end. Three management verbs (snooze / resolve / annotate) close the loop on each proposal. | `internal/core/tools/handlers.go::handleListActiveClusters` |

Tunables centralized in `internal/core/cluster_proposals.go`: `ClusterThreshold`, `ClusterWindowDays`, and the status constants (`active` / `snoozed` / `resolved`).

**Three management verbs — the wire format enforces the distinction.** These are not interchangeable; the JSON-Schema for each is mutually exclusive (e.g. `snooze_until` is required for `snooze_cluster`, forbidden for `resolve_cluster`). The AST guard rail (Appendix C, Pattern 3) locks the contract.

| Verb | State change | Reversible? | When to use |
|---|---|---|---|
| `snooze_cluster` | `status='snoozed'`, auto-reactivates at `snooze_until` | yes (timer-based) | Known noisy signal that's distracting from real clusters right now |
| `resolve_cluster` | `status='resolved'` permanently | no (forensic-preserving) | Root cause is known and fixed; cluster will never re-surface |
| `annotate_cluster` | none — append-only audit row | n/a | Insight arrives on its own schedule; record it for forensics without changing disposition |

`annotate_cluster` is the unusual one: it **cannot** back-door a reactivation. The DM layer writes only to `system_audit_log`, never to `audit_cluster_proposals.status` / `snooze_until` / `count`. The invariant is enforced at the data layer and tested explicitly. This is the right shape for "I learned something about this cluster later" — the historical insight is preserved without altering the disposition the previous operator chose.

`snooze_until` accepts Go-relative (`24h`, `7d`, `1h30m`), ISO 8601 absolute, or a unix-epoch integer (seconds); the DB stores normalized unix-epoch seconds for filter comparison.

**Why three verbs?** Audit trail integrity. The cluster table is a forensic record of "what signals has the system seen and how were they disposed of?" — collapsing snooze and resolve into one verb loses the distinction between "I'm choosing not to see this for now" and "this is closed forever." Annotate preserves historical insight without altering disposition. The asymmetry is deliberate: there is no `reactivate_cluster` tool — resolve is final, and the path back to `active` runs through the underlying cause (which the original operator presumably fixed).

Read-side dedup uses `LIKE '%cluster_key%' ESCAPE '\'` against `memories.content` (free text in pending theories / recent decisions / resolved theories). It catches operators who paste the cluster_key into a theory's rationale without tagging a structured field. The schema has no `cluster_key` column by design — keeping the detection primitive as a side-effect of normal theory/decision writing avoids imposing a tagging convention.

```bash
mpm call list_active_clusters
# → {"known_clusters": [...], "unknown_clusters": [...], "count": {...}}

mpm call read_wake_context
# → "Audit Summary (Last 7 Days):\n- N errors, M warnings logged.\n
#    - Active Clusters (Unknown):\n  * [security] 4 events since 2026-07-02 (ID: security:...)"
```

### Supply-Chain Identity (Commit Signing)

Every commit to the MPM repo ships GPG-signed by default (`commit.gpgsign=true` is configured in `.git/config`). The signing key identity is the substrate's cryptographic commit author — not just a "developer" label. Without commit signatures, supply-chain attacks (compromised dev box, replayed commits) are undetectable at the substrate level.

A clean run of `git log --pretty=format:"%G? %h %s"` should show `G` (good signature) on every commit since signing was enabled. Anything else is a forensic event:

| Flag | Meaning | Action |
|---|---|---|
| `G` | Good signature | Trust as normal |
| `B` | Bad signature (key mismatch or tampered commit) | Do NOT merge; investigate |
| `N` | No signature | Pre-signing-era commit, or commit was forced without `--no-gpg-sign` |

**Verify a key against its expected identity** before trusting a clone:

```bash
git config --get user.signingkey                  # short key ID (e.g. 3C049C8EF8936F94)
gpg --list-keys --keyid-format=long <short-id>    # full fingerprint + uid
```

The uid on the key should match the project's documented author identity. A mismatch is either a config drift, a key rotation that wasn't documented, or an active impersonation attempt — investigate before pulling.

**Why this matters for an autonomous substrate:** the agent writes code that other agents and humans will eventually load. Combined with the four enforcement patterns above (source-of-truth + cache, property tests, AST guard rails, self-heal whitelist), commit identity closes the loop on *who* wrote the substrate, not just *what* it does.

---

## 11. Glossary

The conceptual vocabulary of MPM. Implementation-specific terms (decay, wake, LTM, recency, challenge lifecycle) are defined where they appear, not here. The glossary is for understanding the architecture, not for looking up every noun.

**Artifact.** A persistent cognitive object: Memory, Decision, Theory, Lesson, or Evidence. Each has its own lifecycle.

**Confidence.** A derived `[0, 1]` estimate of how much the system believes an artifact. Computed from supporting evidence minus contradicting evidence minus time decay. The stored `confidence` column is a cache; the evidence ledger is authoritative.

**Core.** The small, stable part of MPM: Memories, Decisions, Theories, Lessons, Evidence, Confidence. Changes here require demonstrated usage evidence.

**Decision.** A choice with explicit context, the choice made, and the rationale.

**Evidence.** A piece of information that supports or challenges another artifact. Confidence is derived from the evidence ledger.

**Memory.** A general fact, observation, or synthesized insight.

**Projection Principle.** MPM is *closed under observation*: every node, action, and lifecycle event is introspectable through the substrate itself. Maintaining that closure requires five commitments — the substrate records facts; it records facts about facts (events, invocations); it **never** records views of facts (projections, scores, summaries); every view is computed from authoritative state at read time; and commands and CLI surfaces may evolve, but truth may not. This is the principle the architecture is built on. The Projection Test below is its operational form, applied at PR review.

**Projection Test.** A design constraint applied before adding any new table, column, cache, score, or summary: if the value can be computed from authoritative state at read time, do not persist it. The burden of proof is on persistence. See CLAUDE.md.

**Runtime.** The evolvable part of MPM: wake, scheduling, personas, modes, directives, routing, audit, review. Changes here are cheap; changes to Core are not.

**Theory.** A testable hypothesis with explicit validation criteria. Theories have lifecycle states: pending → proven | disproven.

---

If you only care about *what* MPM is and *how to use it*, stop at §10. If you are extending MPM, writing tests, or reviewing invariants, continue.

---

## Appendices

## Appendix A: Shared Epistemology Implementation

**Cross-references:** §6.5 Layers 0-3, §10 (audit cluster proposals, where applicable).

This appendix is the deep dive behind §6.5. The narrative above says *what*; this section says *how*. The atomic-epic hashes in each section header let you find the original commit if a future change makes prose stale.

## A.1 Layer 0 — Federation

**Atomic epics:** `6aee1d7` (federation), `61a8418` (FTS sync + singleton), `433dd9f` (architecture split), `c4ba2aa` (singleton migration).

### The two databases

- **Local DB** — `~/.mpm/src/db/mpm.db`. Single canonical tactical-memory DB. Override the workspace root via `MPM_WORKSPACE` (the DB lives at `$MPM_WORKSPACE/src/db/mpm.db`). There is no `<workspace>` subdirectory tier — each MPM process opens one DB per workspace.
- **Shared DB** — `$MPM_SHARED_DB` if set; no default path. Cross-project house rules, operator-gated. If the env var is unset (or the file at that path is missing), mpm runs in local-only mode. The convention `~/.mpm/shared/shared.db` is suggested but not enforced.

Each MPM process attaches both via `ATTACH DATABASE '<shared_path>' AS shared`. Cross-DB queries become plain SQL: `SELECT … FROM shared.memories WHERE …`. There is no separate service, no IPC, no serialization layer.

### Shared Premium (the score boost)

```
shared rule match:                score = base × 1.20
shared rule that also matches
  a query_global_rules scan:      score = base × 1.35
local memory:                     score = base × 1.00
```

The boost is multiplicative, not additive. House rules outrank noisy local memories without being able to *invent* matches — the boost only applies to rows that already pass the base relevance threshold. A bad rule can't bury good local facts by sheer score.

### FTS5 sync triggers

Local FTS5 is unaffected. Shared FTS5 (`shared.memories_fts`) is kept in lockstep with `shared.memories` via three triggers:

```sql
CREATE TRIGGER IF NOT EXISTS shared.shared_memories_ai
AFTER INSERT ON shared.memories
BEGIN
    INSERT INTO shared.memories_fts(rowid, content, tags) VALUES (new.rowid, new.content, new.tags);
END;

CREATE TRIGGER IF NOT EXISTS shared.shared_memories_ad
AFTER DELETE ON shared.memories
BEGIN
    INSERT INTO shared.memories_fts(shared.memories_fts, rowid, content, tags) VALUES('delete', old.rowid, old.content, old.tags);
END;

CREATE TRIGGER IF NOT EXISTS shared.shared_memories_au
AFTER UPDATE ON shared.memories
BEGIN
    INSERT INTO shared.memories_fts(shared.memories_fts, rowid, content, tags) VALUES('delete', old.rowid, old.content, old.tags);
    INSERT INTO shared.memories_fts(rowid, content, tags) VALUES (new.rowid, new.content, new.tags);
END;
```

The triggers are idempotent (`IF NOT EXISTS`) and live in the shared DB itself, so a fresh process picks them up at attach time without a separate migration step. The "already exists" warnings on subsequent attaches are harmless.

### The `is_global` column

`shared.memories.is_global` marks rows that originated as shared rules:

- Local writes always set `is_global = 0` (never the local write path).
- `record_global_rule` always sets `is_global = 1`.
- `promote_to_global` flips it on an existing memory.

This is the audit trail: any `is_global = 1` row was either created by the operator (`record_global_rule --confirm=true`) or explicitly promoted (`promote_to_global`).

### Configuration

| Env var | Effect |
|---|---|
| `MPM_SHARED_DB` | Path to the shared DB. If unset or path missing, shared features are disabled (local-only mode). |
| `MPM_SHARED_READONLY` | If `1`, attach in read-only mode. Default: read+write. |

### Concurrency

- **SQLite ATTACH is per-connection.** Each MPM instance attaches its own connection.
- **WAL mode on both DBs** allows concurrent local writers without blocking.
- **Cross-DB transactions are NOT supported.** Every shared write is its own atomic transaction on the shared DB. This is fine because shared writes are append-mostly and operator-gated.
- **`getDB()` singleton** (`c4ba2aa`) means one `DatabaseManager` per process, one ATTACH, one connection, no leak surface.

### What lives in shared vs local

| Lives in shared | Lives in local |
|---|---|
| House rules (cross-project conventions) | Project-specific facts |
| Cross-project decisions | Session state |
| Durable operator rules | Tactical context |
| Resolution/arbitration memory (the audit trail of "what we decided") | Theories tied to local artifacts |
| Federated contradiction log | Workspace-specific lessons |

## A.2 Layer 1 — Shared Vector Search

**Atomic epic:** `2ff0bda` (IVF).

### Why IVF in SQLite, not a vector DB

The shared DB holds house rules. Semantic recall over them is the operator's only escape hatch from "I know the rule exists but I don't remember the exact wording." The choices were:

1. Brute-force cosine over the shared DB.
2. An external vector database (Qdrant, Milvus, etc.).
3. An in-SQLite ANN index.

MPM chose (3). The constraint was to avoid parallel infrastructure — no new service, no second storage surface to back up, no second connection to coordinate. The in-SQLite IVF satisfies that: it lives in `shared.memory_vectors_ivf`, is queried via the same `DatabaseManager` connection, and survives restarts without re-loading.

### The IVF structure

```
nlist  = 16           # number of Voronoi cells
nprobe = 4            # cells visited per query
dim    = 768          # nomic-embed-text output
```

A query:

1. Computes the embedding of the query text (768d).
2. Finds the `nprobe=4` nearest centroids (out of 16 cells) by cosine distance.
3. Returns the union of `postings[centroid]` — the actual memory IDs in those cells.
4. Brute-force cosine within the returned set, sorted descending.

Speedup vs. brute-force: ~3-4× at this corpus size, with negligible recall loss (the nprobe/nlist ratio is tuned to the embedding model + corpus size).

### The VectorMatch circuit breaker

The `VectorMatch` circuit breaker refuses to start a vector scan that would touch more than `nlist × nprobe` centroids. The breaker caps worst-case work and prevents runaway scans if the index is corrupted or the centroids are wildly imbalanced.

## A.3 Layer 2 — Conflict Detection

**Atomic epic:** `3fb2b75` (Arc 1: Conflict Resolution loop).

### The detection surface

`shared.contradiction_log` is the queue. Every shared write that produces a near-miss against an existing memory is logged with:

- `memory_id_a`, `memory_id_b` (the symmetric pair, not winner/loser)
- The provenance link that flagged the near-miss
- The detection timestamp
- The protobuf-shape score for each side
- The resolution status (`pending` / `resolved` / `arbitration_needed`)

The detection runs **inside `mpm_memory action=query`'s result set**, not as a background ticker. This is opportunistic: detection happens exactly when an agent is looking at the shared DB anyway, so there is no separate timer, no panic-recovery surface.

### Protobuf-shape scoring

```
score = 0.5·confidence + 0.3·freshness + 0.2·reinforcement
```

Bounded to `[0, 1]`. The 50/30/20 split is intentional:

- **confidence (0.5)** dominates — what the system currently believes the artifact says.
- **freshness (0.3)** catches stale knowledge that hasn't been reinforced. A rule from 2024-01-01 with weight=5 scores lower on freshness than one from 2026-07-07 with the same weight.
- **reinforcement (0.2)** is a tiebreaker for facts that get re-surfaced often.

### The close-call margin

If the two candidates in a near-miss land within `0.10` of each other on the protobuf score, the system **does not** auto-resolve. It writes a `pending` row in `shared.theories` and waits for an operator. The asymmetry is deliberate:

- A clean winner can be auto-resolved (low risk).
- A coin-flip always escalates (high risk, operator judgment required).

## A.4 Layer 3 — Conflict Resolution + Arbitration Closure

**Atomic epics:** `3fb2b75` (Arc 1), `8bfe737` (Arc 1 Closure).

### applyResolution

When a contradiction resolves (either by auto-resolve or operator action), `applyResolution` performs four operations in a single transaction:

1. **Write a resolution memory** — captures the choice and rationale as a new `shared.memories` row.
2. **Mark the queue resolved** — UPDATE the `shared.contradiction_log` row with `resolved_at` and `resolution_memory_id`.
3. **Append evidence to the winner** — INSERT a `shared.evidence` row tagged `type='resolution_survived'`, `strength=1.0`, `source_group='resolution:<queue_id>'`, `independence_factor=1.0`. Asymmetric: only the winner, not the loser.
4. **Tag the loser with arbitration metadata** — patch the loser's metadata with `status='archived'`, `operator_arbitration=true`, and (if applicable) `arbitration_theory=<id>`.

The asymmetric evidence write is the entire reward mechanism. A memory that has survived N contradictions accumulates 1.0×N supporting evidence in its confidence ledger — the system is literally rewarded for being right under pressure.

### ResolveArbitrationTheory (the operator path)

When the operator resolves an arbitration theory via `mpm resolve_theory <id> <conclusion> --winner=<memory_id>`, the system does the same four operations, but:

- The metadata flip is done via `UpdateSharedMemoryMetadata` (not `applyResolution`'s path) because arbitration theories live in `shared.memories`, not in the local theories table.
- The loser is tagged with `operator_arbitration=true` and the originating `arbitration_theory` ID in its metadata.
- The theory status flips to `resolved` (or `disproven` depending on conclusion).

### JSON-merge gotcha

SQLite's `||` operator on JSON columns is **plain string concat**, not JSON merge. The naive `metadata = COALESCE(metadata, '{}') || ?` produces `{"status":"pending"}{"status":"resolved",…}` — invalid JSON, `json_extract` returns the old value.

Fix: read existing metadata, deep-merge in Go (`UpdateSharedMemoryMetadata` helper), write the merged result. Patch keys win on collision. This is the single most important SQLite-specific lesson from the Arc 1 closure; it's reapplied in `broadcast.go` for the same reason.

### The arbitration auto-slash loop

```
operator:  mpm resolve_theory <arbitration_id> <conclusion> --winner=<memory_id>
    ↓
mpm resolve_theory handler
    ↓
ResolveArbitrationTheory(arbitrationID, winnerID, conclusion)
    ↓ (single transaction)
    ├── write resolution memory
    ├── mark queue resolved
    ├── UpdateSharedMemoryMetadata(loser) → status=archived, operator_arbitration=true, arbitration_theory=<id>
    ├── UpdateSharedMemoryMetadata(winner) → confidence bump
    ├── Append resolution_survived evidence row
    └── UpdateSharedMemoryMetadata(arbitration_theory) → status=resolved
    ↓
Auto-broadcast hook (Appendix B, Layer 4)
    ↓
INSERT wake rows for every active session except the broadcaster
```

The loop closes. The auto-broadcast hook is the final step — every other piece of state is updated atomically; the broadcast is a side-effect notification.

## A.5 End-to-end smoke

End-to-end verification (8 steps, all green):

1. Seed two contradictory rules into the shared DB.
2. Run `mpm ops resolve-contradictions` (dry-run) — see the queue.
3. Apply one with `--apply` — auto-resolves, writes resolution memory, asymmetric evidence row.
4. Verify the winner has the `resolution_survived` evidence row.
5. Verify the loser has `status='archived'` and `operator_arbitration=true` in its metadata.
6. Create a close-call contradiction.
7. Run `mpm ops resolve-contradictions` on the close-call — should write a `pending` arbitration theory, not auto-resolve.
8. Operator resolves the arbitration theory with `mpm resolve_theory <id> <conclusion> --winner=<id>` — verifies the auto-slash loop.

## A.6 What "shared" means in practice

A memory is shared if `is_global = 1`. A contradiction is shared if its `memory_id_a` / `memory_id_b` both have `is_global = 1`. A resolution memory is shared if the originating queue was shared. A theory is shared if it was created from a shared contradiction.

The asymmetry: local contradictions are NOT written to `shared.contradiction_log`. They stay in the local theories table and are surfaced via local wake context. The shared log is exclusively for cross-project house rules, where one agent's local belief might contradict another's — that's the case that needs federated resolution.

---

## Appendix B: Arc 2 (Active Dissemination) Implementation

**Cross-references:** §6.5 Layer 4, §9 "Event Wakes."

This appendix is the runtime deep dive behind Arc 2. The narrative above says *what* the layer does; this section says *how* it does it.

Arc 2 is shipped at `efec046` (2026-07-07). All smoke tests + unit tests green. The pre-implementation design rationale is preserved in the commit message; this appendix supersedes it.

## B.1 Schema (as-built)

```sql
-- Active-agent registry. Every MPM call silently bumps last_heartbeat.
CREATE TABLE IF NOT EXISTS shared.bcast_sessions (
    session_id      TEXT PRIMARY KEY,
    agent_id        TEXT NOT NULL,
    hostname        TEXT,
    first_seen_at   TEXT NOT NULL,
    last_heartbeat  TEXT NOT NULL,
    total_calls     INTEGER NOT NULL DEFAULT 0
);
CREATE INDEX IF NOT EXISTS shared.idx_bcast_sessions_heartbeat
    ON bcast_sessions(last_heartbeat);

-- The fan-out surface. PRIMARY KEY on wake_id → O(1) dedup.
CREATE TABLE IF NOT EXISTS shared.bcast_event_wakes (
    wake_id         TEXT PRIMARY KEY,    -- sha256(memory_id + ":" + target + ":" + content_hash)[:12]
    target_session  TEXT NOT NULL,
    source_agent    TEXT NOT NULL,
    memory_id       TEXT NOT NULL,
    kind            TEXT NOT NULL CHECK(kind IN ('rule','resolution','arbitration','memory')),
    content_hash    TEXT NOT NULL,
    rationale       TEXT NOT NULL,
    created_at      INTEGER NOT NULL,
    fired           INTEGER NOT NULL DEFAULT 0
);
CREATE INDEX IF NOT EXISTS shared.idx_bcast_wakes_target
    ON bcast_event_wakes(target_session, fired);
CREATE INDEX IF NOT EXISTS shared.idx_bcast_wakes_memory
    ON bcast_event_wakes(memory_id, created_at);

-- Cross-reboot identity cache.
CREATE TABLE IF NOT EXISTS shared.bcast_agents (
    agent_id            TEXT PRIMARY KEY,
    first_seen_at       TEXT NOT NULL,
    last_session_id     TEXT,
    total_heartbeats    INTEGER NOT NULL DEFAULT 0,
    total_broadcasts    INTEGER NOT NULL DEFAULT 0
);
```

**Why the `bcast_` prefix.** `internal/core/schema.go` installs a local `sessions` table into the shared DB on attach via `rewriteTablePrefix(ddl, "shared.")`. Renaming Arc 2's tables with the `bcast_` prefix avoids a silent collision (CREATE TABLE IF NOT EXISTS would have been a no-op for the second one).

## B.2 The wake_id math

```
wake_id       = sha256(memory_id + ":" + target_session + ":" + content_hash)[:12]
content_hash  = sha256(content + "|" + sorted(tags))
```

- **48 bits of entropy** (`12 hex chars × 4 bits = 48 bits`) → ~10¹⁴ unique IDs before 50% collision probability (birthday bound).
- **PRIMARY KEY, not UNIQUE INDEX** → `INSERT OR IGNORE` is the dedup mechanism at the storage layer. No app-level check, no TOCTOU race.
- **content_hash includes only (content + sorted tags).** A rule body text change → new content_hash → new wake_id → new wake fires. A metadata-only update (e.g., `status=challenged`) does NOT change the content_hash, so re-broadcasts are no-ops. This matches the rule that the receiver doesn't need to re-hear the same content because of bookkeeping churn.

## B.3 Fan-out algorithm (Go-side)

```
func BroadcastMemory(memoryID string, opts BroadcastOpts) (BroadcastReport, error):
    1. Resolve memory: SELECT content, tags FROM shared.memories WHERE id = memoryID
    2. Compute content_hash = sha256(content + "|" + sorted(tags))
    3. Discover active sessions:
         a. With opts.ToAgents: lookup agent_id → session_id pairs; refuse any offline target
         b. Without:           SELECT session_id, agent_id FROM shared.bcast_sessions
                                 WHERE last_heartbeat > now - 24h
    4. For each active target:
         a. If target.session_id == opts.SourceSessionID: continue  (self-skip)
         b. wake_id = sha256(memoryID + ":" + target.session_id + ":" + content_hash)[:12]
         c. INSERT OR IGNORE INTO shared.bcast_event_wakes (..., fired=0)
         d. If rows affected == 0: deduped++; else new++
    5. Return BroadcastReport{MemoryID, Kind, ContentHash, Targets, NewWakes, DedupedWakes}
```

## B.4 Auto-broadcast hooks

Three operator commands trigger auto-broadcast as a strict side-effect of an explicit gate. **All three are non-fatal**: a broadcast failure does NOT roll back the underlying write. The resolution is ground truth; the broadcast is the notification.

| Command | What auto-broadcasts | Implementation site |
|---|---|---|
| `mpm ops record_global_rule --confirm=true` | The new rule (kind=rule) | `internal/core/tools/handlers.go::handleRecordGlobalRule` |
| `mpm ops resolve-contradictions --apply` | The resolution memory (kind=resolution, source=arc1-resolver) | `internal/core/contradiction_log.go::ResolveOneContradiction` |
| `mpm resolve_theory <id> <conclusion> --winner=<id>` | The arbitration verdict (kind=arbitration, source=arc1-arbitrator) | `internal/core/contradiction_log.go::ResolveArbitrationTheory` |

Internal agent writes (without an operator gate) do NOT trigger broadcasts. The asymmetry is deliberate — auto-broadcasts are a notification, not a control surface.

## B.5 Receive path

```
mpm call mpm_wakes --payload '{"action":"check_pending_event","params":{"session_id":"..."}}'
```

Mirrors `check_wakes`:

```sql
WITH pending AS (
    SELECT wake_id, ... FROM shared.bcast_event_wakes
    WHERE target_session = ? AND fired = 0
    ORDER BY created_at ASC LIMIT 100
)
UPDATE shared.bcast_event_wakes SET fired = 1
WHERE wake_id IN (SELECT wake_id FROM pending)
RETURNING *;
```

The CTE-updating-read pattern is atomic — concurrent callers see disjoint wake_id sets.

**Opportunistic fold.** `cmd/mpm/call.go` calls `CheckPendingEventWakes(ac.SessionID)` after every handler returns and folds the result into the `EventWakesPending` block on the response. So every MPM call surfaces pending event wakes automatically, without an explicit pull.

### Wake open_works ordering

`read_wake_context` surfaces at most five open work items ordered by `updated_at DESC` (created_at DESC as tiebreak): freshly created work and old-but-recently-touched work rank ahead of stale history, so a fresh agent sees current work instead of the five oldest items. Closed and cancelled work never appears, and neither does archived work (see §Work, "Status and visibility are separate filters"). Both JSON and system-prompt projections consume identical underlying data; the handoff block renders once (consuming read) unless you gather read-only.

### mpm_work response envelopes

`mpm_work action=list` returns `{"success":true,"works":[...],"count":N,"status":"<applied>","visibility":"<applied>"}` and `action=history` returns `{"success":true,"work_id":"...","events":[...],"count":N}`. The list envelope echoes both filter axes it applied, because `count:0` with an unstated filter is indistinguishable from "no work exists". History events are enriched with `framework_name`/`model` resolved from authoritative provenance (`tool_invocations`, falling back to `artifact_provenance`) — absent optional metadata is omitted rather than fabricated. `works.session_id` is populated from the ActiveContext session when the caller omits it. `action=archive` returns the updated work; the idempotent case adds `already_archived:true`. `action=unarchive` returns the updated work with no `archived_at` key.

## B.6 Wake payload shape

| Field | Meaning |
|---|---|
| `wake_id` | Deterministic sha256 prefix — receiver uses for idempotency |
| `source_agent` | Who broadcast (operator + agent ID) |
| `memory_id` | What to fetch — receiver does its own `mpm query memory <id>` if it needs the full text |
| `kind` | `rule` / `resolution` / `arbitration` / `memory` |
| `content_hash` | Receiver-side drift detection |
| `rationale` | The WHY (non-negotiable) |
| `created_at` | Unix epoch |

The full memory content is NOT in the wake — signal, not transport. 100-target fan-out ≈ 100KB total, not 10MB. The receiving agent does its own `mpm query memory <id>` if it needs the full text. If we ever need a "firehose" mode, that's a separate `--include-content` flag and a separate review.

## B.7 Tests (12 unit tests, all green)

- Heartbeat lifecycle: `TestHeartbeat_FirstCallInserts` / `TestHeartbeat_SecondCallBumps` / `TestHeartbeat_ZeroHeartbeats`
- Discovery: `TestDiscoverActiveSessions_FiltersExpired` — 24h window
- Dedup: `TestBroadcastMemory_DeterministicID` — same input → same wake_id, dedupe counter
- Drift: `TestBroadcastMemory_DifferentContent` — content change → new wake_id
- Targeting: `TestBroadcastMemory_TargetedFanout` / `TestBroadcastMemory_RejectsOfflineTargets` — `--to` semantics
- Pickup: `TestCheckPendingEventWakes_MarksFired` — atomic pickup + idempotency
- Dry-run: `TestBroadcastMemory_DryRun` — populates report, writes 0 rows
- Rationale: `TestBroadcastMemory_AutoExtractResolutionRationale` — kind=resolution auto-builds rationale
- Validation: `TestBroadcastMemory_RejectsNoRationale` — kind=memory without `--rationale` refused
- Self-skip: `TestBroadcastMemory_SkipsSelf` — source session not in target list

End-to-end invariant (6 steps): heartbeat → seed → broadcast → dedup → cross-agent → pickup.

## B.8 SQLite limitations discovered

Documented in the Arc 2 implementation lesson:

1. `CREATE INDEX shared.idx ON shared.table(col)` is rejected when shared is attached ("near '.': syntax error"). Workaround: prefix INDEX NAME with `shared.`, use bare table name.
2. `CREATE TABLE IF NOT EXISTS shared.X` is ACCEPTED (the opposite of indexes — easy to confuse).
3. `FOREIGN KEY ... REFERENCES shared.X` is rejected with the same syntax error. Workaround: drop the FK, treat as advisory.
4. `sessions` and `agents` collide with the local BaseTables. The `bcast_` prefix avoids this.
5. SQLite `||` is plain string concat, not JSON merge. Already documented from Arc 1 closure — reapplied in `broadcast.go` for metadata writes.

## B.9 Operational notes

- **Cross-DB transactions are NOT supported.** Each `INSERT OR IGNORE` is its own transaction on the shared DB.
- **WAL mode on both DBs** allows concurrent local writers without blocking.
- **Heartbeat is passive.** Every MPM call writes a heartbeat via `cmd/mpm/call.go`'s pre-handler. Optional `mpm ops heartbeat` exists for tooling that doesn't run agents.
- **Cold-start does NOT replay old wakes.** `mpm_memory action=query` handles history; event wakes are real-time push only.
- **Self-skip is by session_id, not agent_id.** An agent running in multiple sessions gets the wake in each one (except the broadcasting one).

---

## Appendix C: Enforcement Patterns

**Cross-references:** §3.2 (Design Principles), §6.2 (Confidence Engine), §10 (Reliability, audit cluster proposals, self-heal).

The specification states invariants in prose. This appendix is where the enforcement patterns live. Every claim of "this is guaranteed" in MPM maps to one of the patterns below. Once you have internalized these, the architecture reads differently — every "X is enforced" statement in the body of the document is one of these four patterns in a costume.

## C.1 Pattern 1: Source-of-truth + derived cache

**The principle:** If two values could disagree, exactly one of them is authoritative. The other is a derived cache that can be regenerated.

**The canonical example — confidence.**

The `confidence` column on `memories` is a performance cache. The evidence ledger (`shared.evidence` + `local.evidence`) is the source of truth. If they disagree, the cache is wrong.

How this is enforced:

- `mpm_evidence action=add` recomputes confidence in the same transaction as the evidence INSERT. If recompute fails (e.g., CHECK constraint on `confidence_history.trigger`), the entire transaction rolls back — no orphan evidence rows, no stale confidence.
- `mpm_confidence action=recompute` triggers a manual recompute from the evidence ledger.
- `mpm_confidence action=query_history` derives its view from evidence, not the cached column.

**The invariant:** `confidence = f(evidence, decay) at most-recent recompute`.

## C.2 Pattern 2: Property tests over implementation tests

**The principle:** Where the formula drifts but the meaning is fixed, test the meaning, not the formula.

**Why this matters.** If a property test says "decay never increases confidence" and you change the decay function from `linear` to `exponential`, the property test stays green. If a unit test says `assert(decay(0) == 1.0)`, it breaks the moment you tune the curve. The properties are forever-true; the formulas are forever-drifting.

**MPM properties pinned this way:**

- More positive evidence never decreases confidence.
- Decay never increases confidence.
- Cached confidence always matches `f(evidence, decay)` at most-recent recompute.
- `mpm ops gc` never shreds a memory with `weight >= 0` (regardless of the threshold).
- The decision ledger is append-only (no UPDATE on `mpm_decisions action=save` rows after insert).

The confidence properties are pinned in `internal/core/confidence_test.go`. When the formula changes, the tests stay green and the meaning survives.

## C.3 Pattern 3: AST guard rails

**The principle:** When a contract spans a JSON-Schema and a handler, the AST enforces it. A test asserts that the JSON-Schema is a strict superset of the keys the handler reads — so the schema can never lie about what the handler will accept.

**The canonical example — audit cluster verbs.**

`snooze_cluster`, `resolve_cluster`, and `annotate_cluster` are three different verbs with three different state-transition semantics. A buggy JSON-Schema (e.g., accepting `snooze_until` on `resolve_cluster`) would let the caller invoke a snooze while thinking they're resolving. The wire format is the safety boundary.

How this is enforced:

- `TestSchemaSupersetOfHandlerPayloadReads` walks every tool's JSON-Schema and every handler's payload reads, asserting the schema is a strict superset of the handler's keys.
- Schema validation at the boundary rejects malformed payloads before they reach the handler.

**Why this matters more than the example.** A reader of the audit-cluster code can reason about each verb in isolation; the schema validation is the thing that prevents the verbs from accidentally being confused. The contract is encoded in the type system, not the prose.

## C.4 Pattern 4: Self-heal whitelist + escalation

**The principle:** Auto-fix only what's known-safe. Escalate everything else.

**The canonical example — self-heal drift classification.**

`mpm ops self-heal` runs `runDeepScanCheck` and classifies drift into three classes:

| Class | Detection query | Behavior |
|---|---|---|
| Soft-delete ghosts | `memories_fts` rows whose joined `memories` row has `deleted_at IS NOT NULL` | **Auto-fix** (known-safe) |
| FTS orphans | Any `*_fts` row whose rowid is missing from the source table | **Escalate** (unknown) |
| Dangling memberships | `topic_memberships` referencing non-existent topics | **Escalate** (unknown) |

**Four safety boundaries on auto-fix:**

1. **Whitelist-only** — only soft-delete ghosts are auto-fixed.
2. **Bounded blast radius** — if ghost count exceeds `SelfHealMaxFix` (default 1000), refuse to auto-fix and escalate.
3. **Rate-limited** — 24h cooldown between same-signature auto-fixes.
4. **Audit trail** — every auto-fix writes a `lessons` table entry tagged `source=self-heal`.

**Why the asymmetry.** Soft-delete ghosts are the only drift class where we have a precise model of the correct state AND a proven-safe mechanism that should have produced that state. For FTS orphans and dangling memberships, we cannot tell from inside the scan whether the FTS row is the bug or the source-table row is the bug. Auto-fixing novel drift is exactly the kind of guesswork that causes data corruption. Escalate.

The cognitive loop closes through the existing wake context: when self-heal escalates unknown drift, it injects a pending theory; the next `mpm call mpm_context --payload '{"action":"read_wake_context","params":{}}'` surfaces it; the agent acts on it. **No new wake code is needed.**

## C.5 When to use which pattern

| Invariant type | Use |
|---|---|
| Value is derivable from another value | Pattern 1 (source-of-truth + cache) |
| Property holds across all formulas, current and future | Pattern 2 (property tests) |
| Contract spans JSON-Schema and handler | Pattern 3 (AST guard rail) |
| Drift detected in the substrate | Pattern 4 (whitelist + escalate) |

**Pattern 1 + Pattern 2 together:** confidence is a derived cache (Pattern 1), and the formula that derives it is pinned by property tests, not unit tests (Pattern 2). The combination means: even if someone rewrites the entire confidence engine, the system stays correct.

**Pattern 3 + Pattern 4 together:** audit clusters are protected from schema confusion (Pattern 3) and self-heal is protected from guesswork (Pattern 4). Both patterns reject the "let the system figure it out" trap.

## C.6 What these patterns reject

- **Auto-fix by inference.** "If the FTS row is missing, the source table must be the bug" — sometimes true, sometimes catastrophic. Pattern 4 rejects this.
- **Lying schemas.** A JSON-Schema that accepts `snooze_until` on `resolve_cluster` would let callers confuse the two. Pattern 3 rejects this.
- **Stale caches as truth.** A `confidence` column that disagrees with the evidence ledger is, by definition, a bug. Pattern 1 rejects this.
- **Brittle unit tests on forever-drifting formulas.** A test that pins `assert(decay(0) == 1.0)` breaks the moment the formula changes. Pattern 2 rejects this.

The discipline is the same in every case: when in doubt, escalate. The agent's wake context is the escalation surface. The user's attention is the final arbiter.

---

## Appendix D: Epistemic Cascades — Operator Reference

**Cross-references:** §4.8 (the conceptual model), Appendix C.4 (self-heal whitelist + escalation).

§4.8 says *what* a cascade is and *why* it exists. This appendix is the operator surface: the two cascade directions, the outbox schema, the queries you actually run, every tunable constant, and the troubleshooting path when nothing materialises.

### D.1 Two directions, one machine

Cascades run in two directions. Both share the outbox table, the materializer, the wake machinery, and the atomicity guarantees. They differ in trigger surface, polarity contract, and the hypothesis text the materializer writes.

| | Negative cascade | Positive cascade |
|---|---|---|
| Foundation event | becomes invalid / disproven | becomes proven / crosses the confidence ceiling |
| Trigger reasons | `theory_disproven`, `memory_shredded`, `confidence_floor` | `foundation_proven`, `confidence_ceiling` |
| Discovery path | `dependencies` JSON **or** `epistemic_provenance` rows (any polarity, including NULL) | `epistemic_provenance` rows **only**, filtered to `polarity='assumes_false'` |
| Downstream eligibility | decisions and theories | decisions and theories that explicitly opted in via `polarity='assumes_false'` |
| Hypothesis framing | "foundation invalidated; review whether the downstream conclusion still holds" | "foundation proven; review whether the downstream conclusion still holds now that the foundation is established" |

The trigger surfaces are deliberately *not* symmetric. Bare `dependencies` JSON entries cannot fire a positive cascade because the JSON carries no polarity field at all — they are structurally excluded from the positive discovery path, not merely NULL-defaulted. Opt-in via JSON is impossible by construction.

Every cascade event creates one **cascade intent** per downstream artifact. The materializer converts each intent into a **pending re-evaluation theory**, written through `ProposeTheoryWithExtras` — the standard theory write path, so the scanner and FTS5 apply exactly as they do to a hand-written theory. There is no parallel write surface.

### D.2 How the outbox gets drained

```
NORMAL OPERATION
mpm-scheduler
   → cascade_drain handler (registered tick handler)
   → claim pending intents (atomic claim inside BEGIN IMMEDIATE)
   → materialize each intent into a re-evaluation theory
   → schedule cascade wakes for the agent
```

`cascade_drain` is registered as a tick handler in `cmd/mpm-scheduler/main.go`. There is no auto-starting background goroutine on the `DatabaseManager`, and no second scheduling mechanism.

`mpm cascade materialize` is the **foreground escape hatch** — the same materializer with no time budget, because the operator has chosen to wait. Use it when you don't want to run `mpm-scheduler` for a one-shot drain, or when you need to clear a backlog outside the tick cycle (post-incident catch-up, manual cleanup, CI smoke test).

| Flag | Default | Description |
|---|---|---|
| `--once` | `false` | Run one batch and exit |
| `--max-iterations N` | `0` (unbounded) | Bound the number of batches; exits code `2` on timeout |
| `--poll-interval T` | `5s` | Sleep between empty-queue polls (minimum `1s`) |

| Exit code | Meaning |
|---|---|
| `0` | Queue drained successfully |
| `1` | Runtime error |
| `2` | `--max-iterations` exceeded |

```bash
mpm cascade materialize --once             # single batch
mpm cascade materialize --max-iterations 10  # bounded, with operator oversight
mpm cascade list-dead-letters              # failed intents + outbox summary
```

### D.3 Outbox schema

```sql
CREATE TABLE epistemic_cascade_outbox (
    id                       TEXT PRIMARY KEY,
    invalidation_event_id    TEXT NOT NULL,   -- stable event ID for causal tracing
    dead_artifact_id         TEXT NOT NULL,
    dead_artifact_type       TEXT NOT NULL,   -- 'memory' | 'decision' | 'theory'
    downstream_artifact_id   TEXT NOT NULL,
    downstream_artifact_type TEXT NOT NULL,   -- 'decision' | 'theory' only
    trigger_evidence_id      TEXT,
    cascade_depth            INTEGER NOT NULL DEFAULT 0,
    reason                   TEXT NOT NULL,   -- e.g. 'memory_shredded', 'theory_disproven'
    status                   TEXT NOT NULL,   -- 'pending' | 'processing' | 'materialized' | 'failed'
    materialized_theory_id   TEXT,            -- set after successful materialization
    attempt_count            INTEGER NOT NULL DEFAULT 0,
    next_retry_at            INTEGER,         -- unix epoch seconds; NULL = eligible now
    terminal_error           TEXT,
    created_at               INTEGER NOT NULL,
    updated_at               INTEGER NOT NULL
);

-- Idempotent dedup within one invalidation event
UNIQUE(dead_artifact_id, downstream_artifact_id, invalidation_event_id)
```

Indexes:

```sql
idx_epistemic_cascade_outbox_event        ON epistemic_cascade_outbox(invalidation_event_id);
idx_epistemic_cascade_outbox_dead         ON epistemic_cascade_outbox(dead_artifact_id);
idx_epistemic_cascade_outbox_status_retry ON epistemic_cascade_outbox(status, next_retry_at);
```

### D.4 Queries you will actually run

```sql
-- Pending intents waiting to be materialized
SELECT id, dead_artifact_id, downstream_artifact_id, cascade_depth, reason
FROM epistemic_cascade_outbox
WHERE status = 'pending'
ORDER BY created_at ASC;

-- Dead letters requiring operator review
SELECT id, dead_artifact_id, downstream_artifact_id, cascade_depth,
       terminal_error, attempt_count, created_at
FROM epistemic_cascade_outbox
WHERE status = 'failed'
ORDER BY updated_at DESC;

-- Outbox summary
SELECT status, COUNT(*) FROM epistemic_cascade_outbox GROUP BY status;

-- Every materialized cascade theory
SELECT id, metadata
FROM memories
WHERE collection = 'theories'
  AND json_extract(metadata, '$.cascade') = 1;
```

`mpm cascade list-dead-letters` wraps the second query and always exits `0`, printing the summary even when there are no dead letters:

```
CASCADE DEAD-LETTER INTENTS (status=failed)
ID              EVENT_ID  DEAD_TYPE  DOWN_TYPE  DOWN_ID    DEPTH  ATTEMPTS  TERMINAL_ERROR
intent-abc123   evt-xyz   memory     decision   abc123456  4      3         depth exceeded MaxCascadeDepth

Outbox summary — pending=2 processing=0 materialized=15 failed=1
```

### D.5 What a generated cascade theory looks like

The **subject** of the generated theory is always the downstream artifact. The hypothesis and validation criteria are written about `downstream_artifact_id`; the foundation is cited as a dependency, not reviewed as the subject.

The foundation appears in two places:

1. **`dependencies` column** — a JSON array containing `[dead_artifact_id]`. This is the discoverable citation that lets the theory participate in later cascades.
2. **`metadata.cascade.*`** — `cascade: true`, `cascade_version: 1`, `dead_artifact_id`, `dead_artifact_type`, `downstream_artifact_id`, `downstream_artifact_type`, `cascade_depth`, `generated_at` (RFC3339), and `trigger_evidence_id` when the trigger carried one.

The theory also gets a row in `artifact_provenance` with `parent_artifact_id = dead_artifact_id`, so the causal chain can be walked forensically.

**Polarity is absent, not defaulted.** The materializer writes the `artifact_provenance` row but never an `epistemic_provenance` row, so a generated cascade theory has *no* polarity row at all. It is invisible to the positive-direction discovery query. That is a deliberate safety property: even if a future change introduced a positive trigger that defaulted to `assumes_true`, generated cascade theories still could not be discovered through the polarity path, because the row does not exist.

**Recursion.** A generated cascade theory *can* be picked up by a later negative cascade — its `dependencies` JSON names the foundation, and the negative discovery path queries that JSON. It *cannot* be picked up by a later positive cascade, for the reason above. Recursion is bounded by `cascade_depth`:

| Depth | Meaning |
|---|---|
| 0 | Root invalidation event |
| 1 | Direct downstream cascade theory |
| 2 | Cascade of a depth-1 cascade theory |
| 3 | Final permitted recursive cascade |
| 4+ | Suppressed; `CRITICAL` audit event; intent enters dead-letter state |

### D.6 Polarity as a safety invariant

The `epistemic_provenance.polarity` column carries a CHECK constraint restricting it to NULL, `'assumes_true'`, or `'assumes_false'`.

| Value | Behaviour for positive cascades |
|---|---|
| NULL | Inert. Pre-existing citations and every call without an explicit polarity land here. |
| `'assumes_false'` | Explicit opt-in: "this downstream assumes the source is false." When the source is proven, the dependent surfaces for re-evaluation. |
| `'assumes_true'` | Stored per the storage contract, but no trigger surface fires on it today. Reserved. |

> Polarity is **never inferred** — not from citation content, not from keyword or negation detection in natural language, not from semantic similarity, not from dependency structure. The design is explicit-only.

A downstream that negates its foundation in plain English but never passes `polarity='assumes_false'` to `RecordProvenance` will not fire a positive cascade. That is the intended behaviour, not a gap.

### D.7 Threshold-crossing detectors

Cascades fire on the **crossing event**, not on arbitrary confidence movement. A decrease that stays above the floor does nothing; an increase that stays below the ceiling does nothing.

Negative (`internal/core/evidence_store.go`):

```go
crossed := (!hasOldConf || oldConf >= HardConfidenceInvalidationThreshold) &&
    conf < HardConfidenceInvalidationThreshold
```

Positive:

```go
ceilingCrossed := (!hasOldConf || oldConf < HardConfidenceProvenThreshold) &&
    conf >= HardConfidenceProvenThreshold
```

The `!hasOldConf` clause treats a brand-new confidence record as if its prior value sat on the safe side of the boundary. Once an artifact is below the floor, further recomputes that stay below do **not** re-trigger.

### D.8 Wake delivery cap

`CheckPendingWakes` caps cascade-kind wakes at `MaxCascadeWakePerCheck = 3` per call.

> The cap throttles **delivery**, not intent creation. It never discards, suppresses, or coalesces pending intents — those live in `epistemic_cascade_outbox` and are unaffected. It only bounds how many cascade-flavoured wake rows surface to the agent in a single `check_wakes` call.

Notification and cron wakes are unaffected. Nine pending cascade wakes drain over three `check_wakes` calls: 3, 3, 3.

```sql
SELECT * FROM scheduled_wakes
WHERE metadata LIKE '%"kind":"cascade"%'
ORDER BY target_time ASC;
```

### D.9 Drain budget semantics

`cascade_drain` runs under a per-tick wall-clock budget and logs a `cascade drain yielded` line with one of four `yield_reason` values:

| `yield_reason` | Operational meaning |
|---|---|
| `queue_empty` | Outbox drained. Normal completion; handler exits until the next tick. |
| `budget_exhausted` | Budget consumed before the queue drained. Pending intents remain. Expected under load — the next tick resumes. |
| `context_cancelled` | Scheduler shut down mid-tick. Expected on daemon stop; the next start picks up where it left off. |
| `error` | Real handler or DB failure during a batch. An `AuditWarn` plus a structured `Error` line are written. Investigate. |

`budget_exhausted` is **not** a failure. If you see it consistently, either raise `CascadeDrainOptions.Budget` or look at why the backlog is large (`SELECT COUNT(*) FROM epistemic_cascade_outbox WHERE status='pending'`). The scheduler's 60s tick leaves 30s of headroom for the default drain budget.

### D.10 Retry, backoff, and atomicity

| Attempt | Backoff |
|---|---|
| 1 | 2 seconds |
| 2 | 4 seconds |
| 3 | 8 seconds |

After `MaxRetries = 3`, the intent enters dead-letter state (`status='failed'`) and a `CRITICAL` audit event records the intent ID, source and downstream IDs, attempt count, and terminal error.

| Failure | Behaviour |
|---|---|
| Outbox INSERT fails | Root mutation rolls back — invalidation and outbox can never diverge. |
| Theory creation fails | Exponential backoff; after `MaxRetries` → dead-letter + `CRITICAL` audit event. |
| Depth exceeds the limit | Intent suppressed at materialisation; dead-letter + `CRITICAL` audit event. |
| Materializer crashes mid-batch | Restart recovery: `status='processing'` rows older than the 5-minute staleness window reset to `pending` on the next claim. `attempt_count` is **preserved** across recoveries, so a poison pill that gets SIGKILL'd mid-process still accumulates retries and eventually dead-letters. |
| Wake insert fails after materialisation | `wake_scheduled` stays `0`; `ReconcileUnscheduledCascadeWakes` re-books it on the next sweep. The theory is durable; only the wake-booking is recoverable. |

Dedup is structural: `UNIQUE(dead_artifact_id, downstream_artifact_id, invalidation_event_id)` means one root invalidation cannot produce duplicate intents for the same downstream. `markMaterialized` is idempotent — re-running on an already-materialised row yields the same theory, since theory identity is `invalidation_event_id + downstream_artifact_id`.

### D.11 Dependency discovery

Negative cascades combine two edge sources when discovering downstream targets:

1. **Explicit `dependencies` JSON** — theories listing the dead artifact ID.
2. **Typed provenance citations** — `epistemic_provenance` rows created when `source_ids` are passed to `RecordDecision` or `ProposeTheory`.

Only `decision` and `theory` downstreams are eligible (`isEligibleCascadeType`). Lessons and global rules are excluded by design — see the scope table in §4.8.

Positive cascades use path 2 only. `discoverPositiveCascadeTargets` never consults the JSON path.

### D.12 Federation

With `MPM_SHARED_DB` attached, cascade intents are written atomically to both the local and shared `epistemic_cascade_outbox` tables. The local materializer processes the local outbox; the shared outbox is available to cross-agent shared-materializer instances. The `epistemic_provenance` federated read path surfaces citations from both databases.

### D.13 Configuration knobs

| Knob | Default | Notes |
|---|---|---|
| `MaxCascadeDepth` | `3` | Storage-level ceiling on `cascade_depth`. |
| `MaxCascadeWakePerCheck` | `3` | Wake delivery cap per `check_wakes` call. |
| `MaxRetries` | `3` | Retries before dead-letter. |
| `CascadeMaterializerOptions.WakeDelay` | `1s` | Delay before scheduling the cascade wake. `0` disables. |
| `CascadeMaterializerOptions.BatchSize` | `10` | Intents claimed per `MaterializeBatch` call. |
| `CascadeDrainOptions.Budget` | `30s` | Per-tick wall-clock budget for `cascade_drain`. |
| `CascadeDrainOptions.BatchSize` | `10` | Claim size per inner-loop iteration. |
| `HardConfidenceInvalidationThreshold` | `0.3` | Confidence floor for the negative trigger. |
| `HardConfidenceProvenThreshold` | `0.8` | Confidence ceiling for the positive trigger. |
| `PolarityAssumesTrue` | `"assumes_true"` | Storage contract; no trigger surface fires on it. |
| `PolarityAssumesFalse` | `"assumes_false"` | Opt-in polarity for `foundation_proven` / `confidence_ceiling`. |

All are constants in `internal/core/`; the materializer options are also settable via `NewCascadeMaterializer(dm, opts)`.

### D.14 Troubleshooting: "I shredded a root directive but nothing materialised"

1. Is `mpm-scheduler` running? Look for `cascade drain yielded` lines.
2. Is the outbox non-empty?
   ```bash
   sqlite3 src/db/mpm.db "SELECT COUNT(*) FROM epistemic_cascade_outbox WHERE status='pending';"
   ```
3. Is the handler yielding `budget_exhausted` every tick? Check `intents_materialized` per tick and raise `CascadeDrainOptions.Budget` if you need a faster drain after a large blast.
4. Are intents sitting in `status='failed'`? Inspect with `mpm cascade list-dead-letters`.

### D.15 Terminology

| Term | Meaning |
|---|---|
| foundation | The artifact whose epistemic state changed. Stored as `dead_artifact_id` even when the change is positive — the column name reflects the feature's negative-direction origin. |
| foundation invalidation | Negative-direction event: a foundation becomes invalid or disproven. |
| foundation proven | Positive-direction event: a foundation crosses the proven threshold. |
| downstream artifact | The artifact that cited or depended on the foundation and is now subject to re-evaluation. Used interchangeably with *dependent*. |
| cascade intent | One row in `epistemic_cascade_outbox`. `pending → processing → materialized \| failed`. |
| cascade theory | The pending theory materialised from an intent; identified by `cascade=true` in metadata. Also called a *re-evaluation theory*, because the downstream needs conscious re-assessment rather than automatic rewriting. |
| cascade wake | A wake row with `metadata.kind='cascade'` that surfaces the re-evaluation theory to the agent. |
| invalidation event | The causal event ID shared by every intent produced from one trigger invocation. Stable across the chain. |

---

## Appendix E: Capability Lifecycle — Specification

**Cross-references:** §9 (Capabilities — the operator-facing surface), Appendix D (the cascade machinery this subsystem reuses).

§9 describes what a capability is and how an operator bootstraps one. This appendix is the specification: the state machine, the four tables, the forge pipeline, the executor's sandbox contract, the two cascades, and the security model.

A capability is a **stateful artifact**, not an executable blob. Its lifecycle is enforced by a strict state machine in SQLite, and every transition is an auditable row in a dedicated events table. Trust is earned rather than granted: a capability proposes for `sandbox` or `restricted`, and `trusted` is reached only after track record accumulates in the lower domains. Execution is wrapped in `bwrap` per domain, and `source_hash` is verified on every invocation.

The design follows the same philosophy as the epistemic cascade: every state transition is an observable row, every cascade is an idempotent intent in an outbox, every escalation is a wake to a human or agent. **Failure is a state transition, not an exception.** No silent recovery, no magic constants, no unverified execution.

### E.1 The state machine

```
                      operator approval / auto (sandbox only)
                                   │
  ┌───────┐  lint   ┌────────┐ dry-run ┌───────────┐ probation ┌────────┐
  │ draft ├────────▶│ linted ├────────▶│ validated ├──────────▶│ active │
  └───┬───┘         └────┬───┘         └───────────┘           └───┬────┘
      │                  │                                         │
      │ update           │ dependency fracture                     │
      ▼                  ▼                                         │
  ┌───────┐        ┌────────────┐                                  │
  │ draft │        │   needs_   │◀──────────────┐                   │
  │ (rev) │        │  revision  │               │                   │
  └───────┘        └─────┬──────┘               │                   │
      ▲                  │ agent submits fix    │                   │
      └──────────────────┘                      │                   │
                                                │                   │
  ┌──────────┐ 3 fail/60s ┌───────────┐ tolerance breach            │
  │ degraded ├───────────▶│ fractured │                             │
  │          │◀───────────┤           │   ┌─────────────┐           │
  └──────────┘  recovery  └─────┬─────┘   │ rolled_back │◀──────────┘
                                │ operator└──────┬──────┘
                                ▼                ▼ lineage walk
                          ┌─────────┐      ┌──────────┐
                          │ retired │      │ ancestor │
                          └─────────┘      │ revived  │
                                           └──────────┘
```

| State | Description | Callable? | Cascade emitter? |
|---|---|---|---|
| `draft` | Initial state after `propose_skill`; awaiting forge validation | no | no |
| `linted` | Static analysis passed (shellcheck, `py_compile`, ruff) | no | no |
| `validated` | Dry-run in the `bwrap` sandbox passed; ready for the probation gate | no | no |
| `probation` | Receiving invocations to accumulate track record | yes (sandbox) | no |
| `active` | Live version; passed probation; observation window begins | yes (per domain) | yes (on fracture) |
| `degraded` | Failure rate exceeded the soft threshold; still callable | yes (with warning) | no |
| `fractured` | 3 failures in 60s; emits a cascade if `author_theory_id` is set | no | **yes** |
| `needs_revision` | Demoted from `fractured`; the agent can rewrite or abandon | no | no |
| `rolled_back` | A revision failed its observation window; lineage preserved | no | no |
| `retired` | Permanent end-of-life: superseded, abandoned, or operator-killed | no | no |

### E.2 Transition table

| From | To | Trigger | Side effects |
|---|---|---|---|
| `draft` | `linted` | forge lint pass | none |
| `linted` | `validated` | forge dry-run exit 0 in `bwrap` | none |
| `validated` | `probation` | operator approval, or automatic when `requested_domain='sandbox'` | snapshot predecessor → `metadata.baseline_at_proposal` (if a revision); set `A.superseded_by_id = B.id` |
| `probation` | `active` | `success_count >= probation_required_success_count` AND `failure_rate <= probation_max_failure_rate` | `promoted_at = now`; predecessor → `retired` |
| `active` | `degraded` | `failure_rate > soft_threshold` (default `0.20`) | none — still callable |
| `degraded` | `active` | `failure_rate < soft_threshold` for 5 consecutive invocations | none |
| `degraded` | `fractured` | 3 failures in 60s | emit cascade wake; if `author_theory_id` is set, write to `epistemic_cascade_outbox` |
| `active` | `needs_revision` | explicit operator flag | attach the operator note to `metadata.operator_notes` |
| `fractured` | `needs_revision` | automatic on detection | attach `last_failure_stderr` to `metadata.failure_trace` |
| `needs_revision` | `draft` | agent submits a fix (`propose_skill` with `created_from_id`) | old row → `retired`; revision lineage preserved |
| `active` | `rolled_back` | observation-window tolerance breach | full rollback transaction (E.7) including downstream shatter |
| `*` | `retired` | explicit operator action, or superseded by a successful promotion | none |

Any transition not in this table is a forge bug. The validator refuses it, and the storage-layer `CHECK` constraint refuses the state itself.

### E.3 Policy parameters

| Parameter | Default | Description |
|---|---|---|
| `probation_required_success_count` | `5` | Successful invocations needed to exit probation |
| `probation_max_failure_rate` | `0.10` | Failure-rate ceiling during probation |
| `soft_threshold_failure_rate` | `0.20` | Above this, `active` → `degraded` |
| `fracture_window_seconds` | `60` | Time window for the failure cluster |
| `fracture_count_threshold` | `3` | Failures within the window that trigger `fractured` |
| `observation_window_invocations` | `100` | Invocations after promotion during which rollback is possible |
| `observation_window_hours` | `24` | Time bound on the observation window |
| `success_rate_delta_tolerance` | `-0.02` | Minimum acceptable `success_rate` delta vs baseline |
| `latency_ratio_tolerance` | `2.0` | Maximum acceptable `avg_latency_ms` ratio vs baseline |
| `probation_max_hours` | `168` | Probation timeout; on expiry → `needs_revision`, reason `probation_timeout` |
| `invocation_ttl_sandbox_hours` | `24` | GC retention for sandbox-domain invocations |
| `invocation_ttl_restricted_hours` | `72` | GC retention for restricted-domain invocations |
| `invocation_ttl_trusted_hours` | `168` | GC retention for trusted-domain invocations |
| `invocation_ttl_operator_hours` | `0` | GC retention for operator-domain invocations (`0` = indefinite) |

Every parameter is stamped at proposal time into `metadata.policy_snapshot`. Runtime reads the column-stamped values, never live config — so changing a default never silently re-judges a capability that was proposed under the old policy.

### E.4 Schema

Four tables, all using INTEGER Unix-epoch seconds and the standard `created_at` / `updated_at` / `deleted_at` soft-delete pattern.

**`capabilities`** — the artifact itself.

```sql
CREATE TABLE capabilities (
    id              TEXT PRIMARY KEY,                -- ULID
    name            TEXT NOT NULL UNIQUE,            -- slug: ^[a-z][a-z0-9_]{2,63}$
    purpose         TEXT NOT NULL,                   -- 20–500 chars; dedup input
    source_code     TEXT NOT NULL,                   -- ≤64 KB, ≤500 lines
    source_language TEXT NOT NULL DEFAULT 'bash',    -- bash | python | jq
    source_hash     TEXT NOT NULL,                   -- SHA-256 at proposal time

    -- Lifecycle
    state            TEXT NOT NULL DEFAULT 'draft'
                     CHECK (state IN (
                         'draft','linted','validated','probation','active',
                         'degraded','needs_revision','fractured','rolled_back','retired'
                     )),
    execution_domain TEXT NOT NULL DEFAULT 'sandbox'
                     CHECK (execution_domain IN ('sandbox','restricted','trusted','operator')),
    state_changed_at INTEGER NOT NULL,

    -- Lineage (causal-graph participation)
    author_theory_id TEXT,                           -- optional FK → theories(id)
    author_agent     TEXT,
    created_from_id  TEXT,                           -- revision parent
    superseded_by_id TEXT,                           -- set on dedup or shadowing

    -- Empirical metrics
    success_count       INTEGER NOT NULL DEFAULT 0,
    failure_count       INTEGER NOT NULL DEFAULT 0,
    fracture_count      INTEGER NOT NULL DEFAULT 0,  -- distinct from failure_count
    last_invoked_at     INTEGER,
    last_failure_at     INTEGER,
    last_failure_stderr TEXT,                        -- surfaced in needs_revision
    avg_latency_ms      REAL NOT NULL DEFAULT 0,

    -- Probation policy (stamped at proposal; not magic constants)
    probation_required_success_count INTEGER NOT NULL DEFAULT 5,
    probation_max_failure_rate       REAL    NOT NULL DEFAULT 0.10,
    promoted_at                      INTEGER,

    -- Discovery (FTS5 + cosine dedup)
    embedding BLOB,
    tags      TEXT NOT NULL DEFAULT '[]',            -- JSON array

    created_at INTEGER NOT NULL,
    updated_at INTEGER NOT NULL,
    deleted_at INTEGER,
    metadata   TEXT NOT NULL DEFAULT '{}',

    FOREIGN KEY (author_theory_id) REFERENCES theories(id)     ON DELETE SET NULL,
    FOREIGN KEY (created_from_id)  REFERENCES capabilities(id) ON DELETE SET NULL,
    FOREIGN KEY (superseded_by_id) REFERENCES capabilities(id) ON DELETE SET NULL
);
```

The canonical "live tool" lookup — the one the executor runs — is:

```sql
SELECT * FROM capabilities
WHERE name = ? AND state = 'active' AND superseded_by_id IS NULL AND deleted_at IS NULL
LIMIT 1;
```

**`capability_invocations`** — pure execution telemetry. Synthetic events never land here.

```sql
CREATE TABLE capability_invocations (
    id                  TEXT PRIMARY KEY,
    capability_id       TEXT NOT NULL,
    invoked_at          INTEGER NOT NULL,
    exit_code           INTEGER NOT NULL,
    duration_ms         INTEGER NOT NULL,
    stderr              TEXT,           -- truncated; full text in metadata
    invocation_context  TEXT,           -- JSON: caller, sanitized args
    cascade_invalidated BOOLEAN NOT NULL DEFAULT 0,
    FOREIGN KEY (capability_id) REFERENCES capabilities(id) ON DELETE CASCADE
);
```

**`capability_dependencies`** — the junction table whose reverse lookups power every cascade walker.

```sql
CREATE TABLE capability_dependencies (
    capability_id TEXT NOT NULL,
    depends_on_id TEXT NOT NULL,
    added_at      INTEGER NOT NULL,
    PRIMARY KEY (capability_id, depends_on_id),
    FOREIGN KEY (capability_id) REFERENCES capabilities(id) ON DELETE CASCADE,
    FOREIGN KEY (depends_on_id) REFERENCES capabilities(id) ON DELETE CASCADE
);
```

The recursive walker, used by both the fracture cascade and the downstream shatter:

```sql
WITH RECURSIVE downstream AS (
    SELECT capability_id, 0 AS depth FROM capability_dependencies
    WHERE depends_on_id = ?root
    UNION ALL
    SELECT cd.capability_id, d.depth + 1
    FROM capability_dependencies cd
    JOIN downstream d ON cd.depends_on_id = d.capability_id
    WHERE d.depth < ?max_depth
)
SELECT c.id, c.name, c.state, c.execution_domain, d.depth
FROM capabilities c JOIN downstream d ON c.id = d.capability_id
WHERE c.state IN ('active','degraded','probation')
ORDER BY d.depth ASC;
```

`max_depth` defaults to `5`; deeper transitive dependencies surface as operator warnings rather than being auto-shattered.

**`capability_events`** — state transitions and synthetic events, kept strictly separate from invocations so metrics aggregation never has to filter state changes out of telemetry.

```sql
CREATE TABLE capability_events (
    id            TEXT PRIMARY KEY,
    capability_id TEXT NOT NULL,
    event_type    TEXT NOT NULL,
    occurred_at   INTEGER NOT NULL,
    actor         TEXT NOT NULL,   -- 'forge' | 'scheduler' | 'operator:<id>' | 'agent:<id>'
    from_state    TEXT,
    to_state      TEXT,
    reason        TEXT,
    related_id    TEXT,            -- predecessor on rollback; dead dep on shatter
    metadata      TEXT NOT NULL DEFAULT '{}',
    FOREIGN KEY (capability_id) REFERENCES capabilities(id) ON DELETE CASCADE
);
```

| `event_type` | When emitted | Required fields |
|---|---|---|
| `promotion` | state → `active` from probation | `from_state`, `to_state`, `metadata.policy_snapshot` |
| `demotion` | any state regression not from the observation window | `from_state`, `to_state` |
| `fracture` | state → `fractured` | `metadata.failure_cluster`, `metadata.triggering_invocations` |
| `rollback` | observation-window tolerance breach | `related_id=predecessor.id`, `metadata.tolerance_breached` |
| `dependency_shatter` | downstream shatter | `related_id=upstream.id`, `metadata.cascade_depth` |
| `retirement` | state → `retired` | `from_state`, `metadata.reason` |
| `supersede` | shadow flag set during probation | `related_id=candidate.id` |
| `source_hash_mismatch` | runtime tamper detected | `metadata.observed_hash`, `metadata.expected_hash` |
| `operator_approval` | `mpm capability grant-operator` | actor, `metadata.reason` |

### E.5 The forge

The forge turns a `propose_skill` payload into a `linted → validated → probation` capability. Three entry points share one validation sequence wrapped in a single SQLite transaction:

- `mpm skill forge` — proposal intake (operator or agent)
- `mpm skill forge-now <id>` — urgent foreground validation, operator-gated
- `skill_forge_tick` on `mpm-scheduler` — 60s background sweep

**Payload shape:**

```json
{
  "name": "git_status_porcelain",
  "purpose": "Show git working tree status in porcelain v1 format, suitable for parsing",
  "source_code": "git status --porcelain",
  "source_language": "bash",
  "requested_domain": "restricted",
  "tags": ["git", "vcs", "status"],
  "depends_on": ["cap_abc123_git_auth_v1"],
  "author_theory_id": "theo_abc123",
  "expected_io": {
    "args": "none",
    "stdout_shape": "newline-delimited porcelain v1",
    "exit_codes": {"0": "ok", "128": "not a git repo"}
  },
  "metadata": {
    "rationale": "Agent repeatedly needs working tree state before commit",
    "failure_modes": ["not in git repo", "permission denied"]
  }
}
```

| Field | Required | Validation |
|---|---|---|
| `name` | yes | slug `^[a-z][a-z0-9_]{2,63}$`; unique against `state IN ('active','probation','linted','validated')` |
| `purpose` | yes | 20–500 chars. Too short is meaningless; too long is usually hallucination. Also the dedup input. |
| `source_code` | yes | ≤64 KB and ≤500 lines — a structural cap, not a style guide |
| `source_language` | yes | `bash` \| `python` \| `jq`. Each new language means a new validator; extend cautiously. |
| `requested_domain` | yes | `sandbox` \| `restricted` \| `trusted`. **Never `operator`** — agents cannot self-propose it. |
| `tags` | no | FTS5 surface; drives `list_skills` filters |
| `depends_on` | no | every ID must be `state='active'` at proposal time |
| `author_theory_id` | no | epistemic lineage; when set, `fractured` fires the epistemic cascade |
| `replaces_id` | no | bypasses the dedup check for that one ID (E.6); forge sets `created_from_id=replaces_id` |
| `expected_io` | no | contract declaration, compared against actual behaviour at probation |
| `metadata` | no | free-form; survives round-trip |

**The pipeline, in order, atomic** — each step must pass, and failure aborts the transaction:

| # | Step | Validator | On failure |
|---|---|---|---|
| 1 | Schema validation | Go struct tags + manual checks | reject with field-level errors |
| 2 | Name uniqueness | `SELECT 1 FROM capabilities WHERE name=? AND state NOT IN ('retired','rolled_back')` | reject, or accept as a revision if `created_from_id` is given |
| 3 | Source size cap | `len(source_code) <= 65536 AND line_count <= 500` | reject |
| 4 | Scanner pass | `isSensitiveContent` + `isPoisonedCode` patterns (E.9) | reject; mirror to `mirror.jsonl` |
| 5 | Linter pass | `shellcheck -f json` (bash); `python -m py_compile` + `ruff check` (python) | reject with lint errors |
| 6 | Dependency check | every `depends_on` ID exists and is `active` | reject, naming the dead dep |
| 7 | Domain policy | `requested_domain != 'trusted'` for drafts | reject |
| 8 | Dedup check | cosine similarity of the `purpose` embedding against `state IN ('active','probation')`, threshold `0.92`; bypassed for `replaces_id` only | return a `superseded_by` pointer to the existing capability |
| 9 | Dry-run | `bwrap` execution with dummy args; expect `exit_code=0` | reject |
| 10 | Source hash | `SHA-256(source_code)` → `source_hash` | — |
| 11 | Insert | `state='validated'` after a dry-run, or `'linted'` when the dry-run was skipped | — |
| 12 | Audit | `capability_events` row, `event_type='lint_pass'` or `'dry_run_pass'` | — |

Rejections are structured, mirroring the `mirror.jsonl` blocked-content pattern:

```json
{
  "status": "rejected",
  "capability_id": null,
  "reasons": [
    {"step": "scanner", "pattern": "rm_rf_variable", "line": 3, "snippet": "rm -rf $TARGET"},
    {"step": "domain_policy", "message": "draft cannot request domain=trusted"}
  ]
}
```

**Probation mechanics.** Each invocation increments `success_count` or `failure_count`. The forge checks both conditions on every invocation, and when both hold the state moves to `active`, `promoted_at` is stamped, and a `promotion` event is written. If probation criteria fail to materialise within `probation_max_hours`, the capability auto-transitions to `needs_revision` with `reason="probation_timeout"`.

### E.6 Revisions and forks

A revision (`mpm skill update <id>`) creates a **new** row with `created_from_id = parent.id` and re-enters the pipeline from step 1. The original stays `active` until the revision reaches `probation`, at which point `original.superseded_by_id = revision.id` is set as a shadow flag. When the revision reaches `active`, the original is `retired` with `metadata.reason='superseded_by_revision'`.

A fork (`replaces_id`) exists for the case where the agent has *diagnosed* a flaw that hasn't yet surfaced as a fracture. Without the field, the dedup check would permanently block the replacement, because the new capability's purpose necessarily scores above `0.92` against the flawed predecessor. Setting `replaces_id`:

1. bypasses dedup for that one ID — every other capability is still checked;
2. sets `created_from_id = replaces_id` automatically, giving the rollback walker a lineage path back to the predecessor;
3. otherwise flows through the standard pipeline with identical shadow and revive mechanics.

| Field | Agent intent | Database mechanics |
|---|---|---|
| `created_from_id` | Continuous iteration — small improvements and bug fixes on a working concept | New row; lineage walker finds the parent |
| `replaces_id` | Explicit fork — the existing capability is known-broken and the agent walks away from its lineage | New row; dedup bypass for that ID; lineage walker still finds the parent |

The mechanics are identical; the distinction lives in the agent's intent and in the audit log, where `retirement` records `metadata.reason` as `superseded_by_revision` or `superseded_by_fork`.

### E.7 The executor

One Go function turns a `(capability, args, env)` triple into a `(exit_code, stdout, stderr, duration_ms)` result. Every invocation — agent, scheduler, or operator — flows through it, and it enforces seven things in order:

1. the capability is live (`state='active'`, `superseded_by_id IS NULL`);
2. `source_hash` matches the DB row;
3. `execution_domain` matches the wrapper selection;
4. `bwrap` for sandbox and restricted, direct exec for trusted and operator;
5. the timeout from `metadata.max_runtime_ms` (default 30 000);
6. an invocation record is written and capability metrics are updated;
7. the fracture cluster (3-in-60s) is evaluated.

**sandbox** — the default for new proposals:

```
bwrap --ro-bind /usr /usr --ro-bind /lib /lib --ro-bind /bin /bin \
      --ro-bind /etc/resolv.conf /etc/resolv.conf \
      --tmpfs /tmp --tmpfs /home \
      --unshare-net --unshare-pid --new-session --die-with-parent \
      --chdir /tmp -- <script> <args>
```

**restricted** — sandbox plus the project bind mount and any capability-declared paths:

```
bwrap --ro-bind /usr /usr --ro-bind /lib /lib --ro-bind /bin /bin \
      --bind <project_dir> <project_dir> \
      --bind <declared_path_N> <declared_path_N> \
      --tmpfs /tmp --unshare-pid --new-session --die-with-parent \
      --chdir <project_dir> -- <script> <args>
```

Network stays unshared in restricted. The proposal declares `metadata.allowed_paths` listing every writable path; any other write attempt fails inside `bwrap`, and the executor maps that failure to a fracture-eligible error.

**trusted** and **operator** execute directly with `source_hash` still enforced. The operator domain additionally requires `metadata.operator_approved_at` to be non-zero, and the call site must present an operator token enforced at the CLI/MCP layer before `Invoke` is reached.

**`source_hash` verification is mandatory before every invocation, in every domain.** If the live source code doesn't match what the DB recorded at proposal time, the capability fractures immediately, a `source_hash_mismatch` event records observed versus expected, and the operator gets a wake. There is no skip-verification mode.

| Resource | Default | Configurable via |
|---|---|---|
| Wall clock | 30s | `metadata.max_runtime_ms` |
| Memory | 512 MB | `metadata.max_memory_mb` (`bwrap --rlimit-as`) |
| Open FDs | 256 | `metadata.max_fds` (`bwrap --rlimit-nofile`) |
| Output size | 16 MB | hard cap; stdout and stderr truncate beyond it |

A timeout emits `event_type='fracture'` with `metadata.reason='timeout'`. Memory and FD exhaustion behave the same way.

### E.8 The two cascades

Both are symmetric to the epistemic cascade in Appendix D: the upstream event writes an outbox intent, and a materializer drains it.

**Fracture cascade.** When a capability transitions to `fractured`:

1. If `author_theory_id` is set, write an `epistemic_cascade_outbox` row with `dead_artifact_id=<theory>` and `reason='capability_fractured'`. The existing cascade materializer handles it from there.
2. Always walk `capability_dependencies` for downstream capabilities. Each downstream in `state IN ('active','degraded','probation')` transitions to `needs_revision`, gets a `dependency_shatter` event with `related_id=<upstream>`, and — if it has its own `author_theory_id` — its own epistemic outbox row.
3. Emit a notification wake: *"Skill '<name>' fractured. Downstream skills demoted: <list>."*

The whole thing is one transaction: outbox row, state transitions, events, and wake commit together or not at all.

**Rollback cascade.** When an active capability breaches tolerance inside its observation window (`success_rate_delta < -0.02` or `latency_ratio > 2.0`), the substrate walks lineage backward via `created_from_id` to find the closest retired ancestor and revives it. The walk is cycle-safe — a depth cap of 32 plus a path-string revisit guard — and `ORDER BY depth ASC` selects the immediate parent rather than a sibling branch that happened to retire later. For a chain A→B→C where C rolls back, B is the revival target; if B is itself fractured the walk continues to A.

If the revival UPDATE returns `rowcount=0` — the "both broken" case — the transaction rolls back and a second one marks the revision `rolled_back` with `metadata.escalated=true`, writes a `rollback` event with `reason='predecessor_unavailable'`, and schedules a highest-priority operator wake. The `escalated` flag is observable via `mpm skill show <id>` but is deliberately **not** a separate state — the state enum stays clean, and the substrate refuses to silently pick a poison.

### E.9 Security model

Defense in depth. Each layer backstops the previous; none is sufficient alone.

| Layer | Mechanism | Role | Bypassable? |
|---|---|---|---|
| 1 | Regex scanner | Fast-fail heuristic; refuses obvious cases early to save compute | **Yes** — trivially, via string concatenation, indirect expansion, or encoding. Not a security boundary. |
| 2 | Linter | Catches semantic issues regex misses (unquoted vars, injection patterns) | **Yes** — same bypass class as layer 1 |
| 3 | Dry-run in `bwrap` | Catches runtime errors before promotion | Partial — catches crashes, not intent |
| 4 | `bwrap` execution wrapper | **The actual security boundary.** Read-only binds, `--unshare-net`, no writable paths outside declared surfaces | No — structural; the kernel enforces it |
| 5 | `source_hash` verification | Defeats runtime tampering of the source on disk | No — the comparison is deterministic |
| 6 | Domain ladder | Claimed scope versus granted scope; promotion requires track record | No — enforced at promotion time |

**The regex scanner is not the security boundary.** A hallucinated `TARGET="r"; TARGET+="m"; $TARGET -rf /` passes layer 1 (the literal string never appears), passes layer 2 (the linter sees a script building a string at runtime), and reaches layer 4 — where `bwrap` refuses to write outside the bind mounts and the script dies. The system stays safe even if every regex pattern is bypassed. The scanner's job is to refuse obvious cases cheaply; `bwrap`'s job is to make arbitrary damage impossible. Conflating the two is how security reviews go wrong.

The 19-pattern secret scanner that gates every memory write is extended with `isPoisonedCode` patterns for `source_code`:

| Pattern ID | Severity |
|---|---|
| `rm_rf_variable`, `rm_rf_root` | critical |
| `curl_pipe_sh`, `wget_pipe_sh` | critical |
| `reverse_shell_bash`, `reverse_shell_nc` | critical |
| `hardcoded_aws_key`, `hardcoded_private_key` | critical (already covered by the base scanner) |
| `dd_destructive`, `mkfs_unmounted` | critical |
| `absolute_path_write_root` | high |
| `chmod_777` | medium |

Blocked proposals are mirrored to `mirror.jsonl` with the `capability_source_code` tag and never reach the database.

**The domain ladder:**

```
sandbox
  │  50 successful invocations, failure_rate ≤ 0.05, no fractures
  ▼
restricted
  │  500 successful invocations, failure_rate ≤ 0.02, no fractures in 30d
  ▼
trusted
  ▲
  │  NEVER reachable by capability proposal
  │
operator  ◀──── mpm skill elevate (operator-only)
```

| Domain | Network | Filesystem writes |
|---|---|---|
| `sandbox` | none (`--unshare-net`) | none — read-only binds plus tmpfs |
| `restricted` | none | declared paths only |
| `trusted` | host network | full filesystem (operator-trusted) |
| `operator` | host network | full filesystem (operator-trusted) |

A capability that needs network access must reach `trusted` first, and the forge's domain policy makes that structurally enforced rather than merely documented.

### E.10 Command surface

```
mpm skill propose   --payload <json>              # submit a draft
mpm skill show      <id>                          # state, metrics, lineage
mpm skill list      [--state=X] [--domain=Y]      # filterable inventory
mpm skill audit     <id> [--limit=N] [--since=T]  # full lineage diff
mpm skill update    <id> --payload <json>         # propose a revision
mpm skill rollback  <id> [--reason=X]             # explicit operator rollback
mpm skill retire    <id> [--reason=X]             # explicit operator retirement
mpm skill forge-now <id>                          # urgent foreground validation
mpm skill invoke    <id> [args...]                # direct invocation (testing/operator)
mpm skill promote   <id>                          # operator override of the probation gate
mpm skill elevate   <id>                          # operator-only domain elevation
mpm skill gc                                      # invocation-table TTL sweep
```

---

## Appendix F: CLI & Command Architecture

**Cross-references:** §6.1 (execution-profile routing), §8 (the CLI reference itself).

§8 lists the commands. This appendix is the discipline behind them: why the CLI has two personalities, what a command is allowed to contain, and the layering that keeps the human surface from drifting away from the substrate.

The governing idea: **the CLI reflects the cognitive architecture, not the storage architecture.** It is an interface and discoverability surface, not a second implementation. Almost all of it is aliases, routing, help, and thin orchestration over handlers that already exist.

### F.1 The seven principles

These override implementation convenience.

**1. Stable contracts.** MCP tool names are API contracts — `record_decision`, `save_lesson`, `save_skill`, `read_wake_context`, `flush_scratchpad`. They do not get renamed. Compatibility beats aesthetics.

**2. Intent at the CLI.** Humans express intentions; agents integrate against contracts. So the CLI exposes intentions — `mpm remember`, `mpm continue`, `mpm recall`, `mpm why`, `mpm doctor` — and dispatches internally to the same handlers the MCP surface calls.

**3. One engine, two interfaces.** The human interface is small, discoverable, intent-driven, and for daily use. The operator interface is explicit, scriptable, complete, and stable. Both dispatch to one implementation. Business logic is never duplicated between them.

**4. Progressive disclosure.** The number of commands is not the problem; discoverability is. Default help exposes roughly eight primary commands, with everything else behind `mpm help <section>` and `mpm help --all`. Help output is **not** an API — operators who want scriptable discovery use `mpm help --json`. There is deliberately no `--legacy` flat-help mode, because maintaining a mirror of the old shape locks in drift.

**5. Prefer aliases over renames.** A better name is added, never substituted. The old command stays, documented under the operator interface.

**6. Composition discipline.** Human commands compose existing queries; they never reimplement them. `mpm continue` has no queries of its own for working context, recent decisions, or loaded skills — it calls the existing `wake`, `read_scratchpad`, `query_decisions`, and `list_skills` paths. If a contributor starts reimplementing those inside a human-mode command, stop them. Duplicated query logic is how a cognitive substrate rots silently: the CLI and the substrate quietly disagree about what "the same data" means.

**7. Commands are adapters, not dependencies.** The CLI mirrors the MCP-vs-substrate separation:

```
SQLite
   ↓
Stores      ← query types wrapping SQLite. One per major domain table cluster
               (working context, wake context, memories, decisions, skills,
               theories, status). They own ONLY queries; no behaviour.
   ↓
Services    ← behavioural units. Earn their existence by owning behaviour that
               crosses stores or carries rules (validation, expiry,
               orchestration, aggregation).
   ↓
Formatters  ← pure data transformation. Reshape a model into a presentation
               shape (WorkingContext → DashboardSection). Stateless.
   ↓
Encoders    ← serialisation. JSONEncoder, YAMLEncoder. NOT rendering.
   ↓
Renderers   ← output-medium adapters. TerminalRenderer, DashboardRenderer.
               Consume formatters (or models directly) and write to the terminal.
   ↓
Commands    ← tiny adapters (~30 LOC). Compose services; hand off to formatters,
               renderers, encoders. NEVER call another command. NEVER own SQL.
               NEVER own behaviour. NEVER own presentation.
```

### F.2 Services must pay for themselves

A service that merely wraps a store — `TheoryService{repo.ListRecent()}` — is accidental complexity. Delete it and inline the store call at every call site and nothing is lost. That is the test.

Real services own real behaviour:

- **WorkingContextService** — `Load()`, `Validate(state)`, `Expire()`, `Promote()`, `Clear()`. Behaviour spanning persistence, validation, lifecycle, and policy.
- **ContinueService** — `Compose(sectionOwners...)` orchestrates five subsystems into the `continue` dashboard. Composition *is* behaviour.
- **DoctorService** — `Check()` aggregates six-plus telemetry paths into one trust signal. Aggregation *is* behaviour.

Not services — these are stores, and should be named as such: anything of the shape `type FooService struct{}; func (s *FooService) Get() { return s.repo.Get() }`.

> **Commands do not compose commands. Commands compose services.**

### F.3 Two anti-patterns this forecloses

**The Unix trap.** `exec.Command("mpm", "work", "show")` and parsing stdout *feels* clean, because from the outside every CLI command is a program. Inside Go it is not: you end up negotiating stdout, exit codes, JSON-versus-text modes, recursive CLI invocation, and duplicated formatting paths. You script yourself. Git does not implement `git status` by shelling out to `git diff` and parsing output; both call the same plumbing.

**Model/rendering coupling.** A direct `work.Render()` call looks like the fix for the above, but it welds the model to one presentation. The moment `mpm work show --json` lands, or someone wants syntax highlighting in `work show` and compact Markdown in the `continue` dashboard, model and rendering have to be untangled with no clean migration path. The presentation layer is not the model.

Formatters, encoders, and renderers are three different concerns. A formatter reshapes for a presentation context; an encoder serialises to a machine-readable form; a renderer writes a presentation-shaped thing to a terminal. **JSON is serialisation, not rendering.**

```
mpm work show                  →  workingCtxSvc.Load()
                                  workingCtxFormatter.Format(model)   → MarkdownModel
                                  terminalRenderer.Render(markdownModel)

mpm work show --json           →  workingCtxSvc.Load()
                                  jsonEncoder.Encode(model)

mpm continue                   →  [services per section]
                                  [formatters reshaping for dashboard sections]
                                  dashboardRenderer.Render(sections...)
```

The command never sees the model raw. The service never sees a renderer. The renderer never sees a store.

### F.4 Help taxonomy

Default `mpm help` groups by cognitive function rather than by subsystem:

```
MPM

Daily              Knowledge          Working Context    Maintenance
-----              ---------          ---------------    -----------
continue           memory             work               backup
remember           lesson                                restore
recall             skill                                 review
doctor             decision
why                theory

Need more?  mpm help knowledge · mpm help work · mpm help --all
```

Dozens of commands at first contact is the failure mode this avoids.

### F.5 Non-goals

The CLI surface explicitly does not: rename MCP tools, redesign storage or the SQLite schema, change routing semantics, introduce workflow automation or planning behaviour, recommend actions, remove compatibility aliases, or maintain a flat-help compatibility mode.

---

## Community & Security

* **[Security Policy](SECURITY.md)** — how to report a vulnerability privately, what to expect back, supported versions, and the credential-handling rules specific to MPM.
* **[Contributing Guide](CONTRIBUTING.md)** — architecture invariants (Projection Test, scanner chokepoint, single-connection invariant, foreign-key posture), testing gates, schema discipline, and PR expectations.

This is an alpha release (`mpm-alpha`). APIs, CLI surfaces, on-disk formats, and schema may change without notice. Pin a commit SHA if you need a specific shape to stay that way.

## License

**License:** GNU Affero General Public License v3.0 (AGPL-3.0).
MPM is free software and may be used commercially. See [LICENSE](../LICENSE) for the complete terms.
