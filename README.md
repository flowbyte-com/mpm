# MPM

> **Persistent reasoning for autonomous AI agents.**

Traditional memory systems answer one question:

> *What does the agent remember?*

MPM attempts to answer four:

* What does the agent believe?
* Why does it believe it?
* How certain is it?
* What evidence could change its mind?

Those questions lead to a different architecture.

Instead of storing conversations, MPM stores reasoning.

Instead of assigning arbitrary confidence scores, confidence is derived from evidence.

Instead of rewriting memories, beliefs evolve while history remains intact.

Everything is built on a single SQLite database.

No vector database.

No daemon.

No distributed infrastructure.

Just a persistent cognitive substrate.

---

<p align="center">

**MPM is not designed to maximize recall.**

**It is designed to preserve intellectual progress.**

</p>

---

## Reading Guide

Evaluating MPM? Read Sections 1–5.

Integrating MPM? Read Sections 4–9.

Contributing? Read the entire document.

---

## 1. What is MPM?

MPM is a single binary that provides long-term memory, behavioral modes, persona management, and an **epistemology engine** — everything stored in one SQLite database with FTS5 full-text search. Zero external services.

MPM is the memory and reasoning layer for AI agents. It tracks not just *what* the agent knows, but *why* it decided to act, *how* it chose to act, and *what it believes but hasn't proven yet*.

### What MPM is not

- **Not a daemon.** Every command is a single binary invocation. No long-running processes; periodic work happens on demand via `mpm ops maintain`.
- **Not generic storage.** Built for AI agent cognition: weighted recall, decay, epistemology, proactive hints.
- **Not a human dashboard.** Machine-to-machine interface is primary; CLI is a convenience layer.

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
| Lesson | Reusable knowledge |
| Evidence | Information supporting or challenging another artifact |

The distinction matters.

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

#### Lesson

Reusable knowledge that survives across tasks — best practices, warnings, patterns, and insights.

#### Evidence

Information that supports or challenges another artifact. Evidence is the substrate from which confidence is derived, never an artifact-level assertion of truth.

### 3.2 Design Principles

MPM follows a small number of architectural rules.

#### History is immutable

A memory records what was believed. History is never rewritten.

#### Interpretation is mutable

Confidence changes. Evidence accumulates. The original artifact does not.

#### Truth is external

Reality determines truth. MPM only maintains its current estimate.

#### Reasoning is persistent

Reasoning deserves the same permanence as facts. Decisions, theories, and evidence are first-class objects.

#### Simplicity beats infrastructure

SQLite provides persistence, transactions, FTS5, JSON, WAL, and indexing. Adding another service is a last resort, not a default.

#### Keep it boring

Boring infrastructure is reliable infrastructure. The interesting problems in MPM live in the cognitive model, not in the deployment surface.

#### Minimize moving parts

Every component added is a component that must be reasoned about, debugged, secured, and explained. We earn each new piece through usage evidence, not architectural enthusiasm.

#### Sympathy for unknown unknowns

We do not know what MPM will be used for in three years. The architecture favors reversible, conservative choices over clever, brittle ones.

### 3.3 Confidence

Confidence is not user input.

Confidence is derived.

Conceptually:

```
confidence
  = initial(type)
  + supporting evidence
  − contradicting evidence
  − decay(time)

bounded to [0, 1]
```

This is a pedagogical summary. The authoritative computation lives in `internal/confidence/`. The stored `confidence` column is a performance cache; the evidence ledger is the source of truth. If the cached value ever disagrees with the recomputed value, the cache is wrong. The evidence is authoritative.

Key concepts:

- **Knowledge and confidence are independent.** Adding contradicting evidence changes confidence but leaves the artifact content untouched. A hindsight annotation can change what the artifact *says* without retroactively changing what was *believed* about the original.
- **Confidence is the system's *current* estimate of truth** derived from *current* evidence. The artifact is historical fact.
- **Confidence only rises with new evidence.** It is allowed to decrease automatically as time passes without reinforcement.
- **More positive evidence never decreases confidence.** This property is a forever-true invariant; the formula that computes it is a forever-drifting implementation detail. Encoded as a property test in `internal/confidence/properties_test.go` — when the formula changes, the invariant survives.

See §5.2 for the implementation details (evidence type registry, atomicity, initial confidence by artifact type).

### 3.4 Belief Lifecycle

This is the section readers learn how beliefs form, get tested, get updated, and get retired. The underlying subsystem is called the **Epistemology Engine**, but the section title describes what it actually does: a lifecycle for beliefs, not an academic treatise on epistemology.

#### The cognitive loop

```
Observation
      │
      ▼
 Save Memory
      │
      ▼
Add Evidence
      │
      ▼
Recompute Confidence
      │
      ▼
  Retrieve
      │
      ▼
Challenge?
   │        │
  No       Yes
   │        │
   ▼        ▼
 Done   Create Theory
              │
              ▼
      Gather Evidence
              │
              ▼
      Resolve Theory
              │
              ▼
   Update Confidence
```

Notice what never happens. The original memory is never edited. Only confidence changes. That distinction allows historical reasoning to remain inspectable months later.

#### Decision Ledger

An append-only audit trail of architectural choices. Captures the context, the choice made, and the reasoning — so weeks later, the agent can reconstruct *why* a particular approach was taken instead of blindly second-guessing itself.

```bash
mpm record_decision "CONTEXT: We needed a CSS injection mechanism that survives wp_kses filtering
CHOICE: Route all widget CSS through agentshell_register_widget → wp_options → widgets.php <head> injection
RATIONALE: WordPress strips <style> blocks from post content via wp_kses_post() even for admins. The widget init JS also needs a footer injection point. Both requirements pointed to wp_options as the store."

mpm decisions
```

#### Theory Tracker

A hypothesis ledger for debugging and design. When the agent forms a causal assumption ("I think X is causing Y"), it logs the hypothesis and a concrete validation test before writing the fix. This forces the assumption to be testable, and often collapses a false hypothesis before it wastes an hour.

```bash
mpm propose_theory "HYPOTHESIS: passing --json before the positional arg causes the parse bug
VALIDATION_CRITERIA: write a unit test — invoke mpm with --json flag first vs positional-first, compare parse error rate
STATUS: pending"

mpm theories                    # List all theories with status chips
mpm theories pending            # Pending only

mpm resolve_theory abc123 "confirmed: flag order matters, --json consumed before positional processing"
```

#### Cognitive Immune System (Challenge Lifecycle)

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

- **`mpm challenge <id> "<evidence>"`** — atomic: patch memory metadata, create theory with back-link, weaken weight by 3.
- **`mpm challenge restore <id>`** — atomic: resolve theory as disproven, clear memory flags.
- **`mpm shred <id>`** — atomic: cascade-delete memory + linked theory + topic memberships.
- **`mpm ops gc --shred-negative`** — shreds only memories with weight<0 AND a proven theory exists. Negative weight alone is never sufficient — the theory provides the evidence chain.

#### Proactive Recall

Traditional memory systems wait for a search query. MPM actively surfaces relevant knowledge before it is requested.

```bash
mpm kb hint "discussing the CSS injection approach for the widget system"
# → 💡 [Recall] You decided: Route all widget CSS through agentshell_register_widget...
#    RATIONALE: WordPress strips <style> blocks from post content...
```

FTS5 keyword extraction detects semantic overlap with the current context and pushes a low-latency recall hint — with STATUS and RATIONALE displayed directly, not just the content. The `proactive_recall_hint` plugin tool is wired into the OpenClaw agent loop — it surfaces the most relevant epistemology memory automatically after context shifts.

---

## 4. Quick Start

Five minutes from zero to first decision.

### Install

```bash
go install github.com/yourorg/mpm/cmd/mpm@latest
```

Or build from source:

```bash
git clone https://github.com/yourorg/mpm
cd mpm
make build
```

The single binary lives at `bin/mpm`. No daemon, no service registration, no config files required to start.

### Store memory

```bash
mpm add Germany leads Group E with +6 goal differential
```

### Retrieve memory

```bash
mpm World Cup prediction
mpm recall --semantic "为什么德国队表现这么好"
```

### Record a decision

```bash
mpm record_decision "CONTEXT: We need CSS injection that survives wp_kses filtering
CHOICE: Store widget CSS in wp_options
RATIONALE: WordPress strips inline style tags from post content"
```

### Propose a theory

```bash
mpm propose_theory "HYPOTHESIS: passing --json before the positional arg causes the parse bug
VALIDATION_CRITERIA: unit test — invoke mpm with --json flag first vs positional-first
STATUS: pending"
```

### Challenge a memory

```bash
mpm challenge <id> "recent data contradicts this — see new measurements"
```

### System status

```bash
mpm status
mpm ops stats
mpm wake               # last session context
```

Done. That's the cognitive loop: observe, decide, theorize, challenge. The rest of this document explains how each piece works and how to operate the system at scale.

---

## 5. System Architecture

MPM intentionally separates persistent cognition from runtime behaviour.

### Persistence Layer

MPM intentionally standardizes on SQLite. Transactions, FTS5, JSON support, WAL mode, and portability are sufficient for the project's design goals. Additional infrastructure is added only when SQLite can no longer satisfy those goals.

### 5.1 Core vs Runtime

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

### 5.2 Evidence & Confidence Implementation

#### Evidence types

| Type | Default strength | Meaning |
|---|---|---|
| `observation` | 0.4 | Direct observation of an event |
| `test` | 0.7 | Result of an automated test |
| `reproduction` | 0.85 | Independently reproduced result |
| `challenge` | −0.6 | Evidence against an artifact |
| `decision_outcome` | 0.95 | Outcome of a recorded decision |
| `external_reference` | 0.6 | Cited source from outside the agent |

Override the default with `--strength`. Artifact types are a strict enum — the LLM cannot invent categories.

#### Initial confidence by artifact type

| Artifact type | Initial confidence |
|---|---|
| memory | 0.8 |
| theory | 0.5 |
| decision | 0.6 |
| lesson | 0.7 |

#### Atomicity guarantee

`add_evidence` and confidence-history-triggered recomputes run inside a single SQLite transaction (`WithTx` over `DBNode`). If the recompute fails (e.g. CHECK constraint violation on `confidence_history.trigger`), the evidence INSERT rolls back with it — no orphan evidence rows, no confidence column updated without a matching history row.

#### Property tests as confidence spec

The conceptual confidence equation in §3.3 is forever-drifting. The invariants it must obey are forever-true. Those invariants live in `internal/confidence/properties_test.go` — when the formula changes, the tests stay green and the meaning survives.

### 5.3 Retrieval Architecture

MPM combines:

- SQLite FTS5 (BM25 ranking)
- Semantic embeddings (cosine similarity, 768d via `nomic-embed-text`)
- Reinforcement history
- Recency scoring

**Hybrid scoring** (in `internal/hybrid_search.go`):

```
score = (reinforcement_count × 2) + (weight × 1.5) + recency_bonus
```

BM25 unbounded scores are sigmoid-normalized. Use `--semantic` for pure embedding search.

**Memory provenance (`mpm recall --why`):** every result can be annotated with the score breakdown that retrieved it. Pass `--why` to see per-result `[why]` lines showing reinforcement contribution, weight contribution, recency age, and the FTS5 terms that matched. Useful for "why did the agent pick this memory?" introspection without re-running the search.

**LTM promotion:** `weight ≥ 10` OR explicit `mpm promote` OR auto-ingested `.md` file.

#### Concept Drift Detection

Concept drift detection — autonomously identifying paradigm shifts where historically trusted knowledge is decaying under a sudden barrage of new counter-evidence — is implemented as a **synchronous** operation that runs inline with `query_long_term_memory` (no background ticker). The detection signature:

| Signal | Threshold |
|---|---|
| Baseline | Artifact achieved high status historically (`peak_confidence >= 0.85`) |
| Velocity | ≥ 2 recent challenges or negative-strength observations within the last 7 days |
| Delta | Confidence has dropped ≥ 0.25 and sits below the distrust threshold (`< 0.60`) |
| Lifetime | ≥ 3 total lifetime negative evidence pieces (prevents one-off flukes) |

When a drifting memory triggers this signature, the engine quarantines the memory (sets `concept_drift: true`), proposes a pending theory, and survives restarts via SQLite-native dedup. Drift detection is pure-SQLite — no separate process, no separate timer, no panic-recovery surface to maintain. A drift missed last query is just as catchable next query.

### 5.4 MCP Integration

MPM integrates directly with AI agents as a **single MCP server**. The Go binary (`bin/mpm-mcp`) is the only substrate; agents connect to it via MCP and receive the full MPM tool surface as native function calls. No plugin layer, no Node/TypeScript wrapper, no Python shim — one binary speaking MCP.

The migration from the legacy plugin model (per-agent TypeScript wrappers calling the CLI binary via `child_process`) to the current MCP server model happened in 2026-06-23. The deleted plugin folders and their associated TypeScript sources are gone. The MCP server is the only integration surface for MCP-aware clients.

#### MCP tool surface

The MCP server exposes the full MPM substrate as native function calls. Adding a new tool is a single Go function — no plugin path, no shell wrapper, no parallel documentation.

```
<!-- tools:begin — auto-generated by `go generate ./internal/tools/`. Do not edit by hand. -->
save_to_memory           query_long_term_memory   challenge_memory

shred_memory             reinforce_memory         weaken_memory
snooze_memory            set_memory_weight        patch_memory
promote_memory           promote_to_global

save_lesson              search_lessons           list_lessons

create_topic             search_topics            link_topic

add_reference            search_references        list_references

propose_theory           resolve_theory

record_decision

add_evidence             list_evidence

query_confidence_history query_confidence_changes query_confidence_trend
show_confidence          recompute_confidence     explain_confidence

query_audit_log

read_wake_context        read_directives          proactive_recall_hint
route

session_end              session_handoff          list_handoffs

review_memories          synthesize_memory        gc_run

query_memory_quality     log_to_changelog         query_global_rules
record_global_rule       schedule_wake            check_wakes
list_wakes
<!-- tools:end — auto-generated by `go generate ./internal/tools/`. Do not edit by hand. -->
```

Wiring a new agent: add `mpm-mcp` to its MCP server config (OpenClaw: `mcp.servers.mpm` in `openclaw.json`; Claude Code: `.mcp.json`; any other MCP-aware client). The server binary is at `bin/mpm-mcp` relative to the MPM repo root.

#### Single source of truth: `internal/tools/registry.go`

Both the CLI (`mpm call <tool>`) and the MCP server iterate the same registry — a package-level `[]Tool` slice in `internal/tools/registry_list.go`. Each entry holds:

- `Name` — the tool identifier (used by both surfaces)
- `Description` — short prose shown to MCP clients
- `Schema` — JSON-Schema (raw bytes, parseable by both surfaces)
- `Handler` — `func(dm *DatabaseManager, ac *ActiveContext, payload map[string]interface{}) (interface{}, error)`. Same function called by both surfaces.

Adding a new tool: write `handleFoo` in `internal/tools/handlers.go` (one function), append a `Tool{...}` entry in `internal/tools/registry_list.go`. Both the CLI dispatcher and the MCP server pick it up automatically.

#### MCP/CLI parity: what is exposed via both surfaces

Every `mpm call <tool>` entry has a matching MCP tool spec; both call the same `DatabaseManager` methods. The 33 tools in the surface above cover the agent's daily workflow: read/write memory, lessons, topics, references, theories, decisions, evidence, confidence, references, route, wake, directives, log_to_changelog, plus the memory feedback loop (`shred`/`reinforce`/`weaken`/`snooze`/`set-weight`/`patch`/`promote`) and workflow (`review`/`synthesize`/`gc`).

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
| `mpm web` | Long-running HTTP server; can't be a tool call |

If an agent needs any of these, the operator should run it explicitly. Tool calls that could damage state are intentionally kept on the human-facing CLI where the cost of a misclick is bounded by the operator's attention.

---

## 6. Core Stability

The following concepts define MPM. They should change rarely.

- Memories
- Decisions
- Theories
- Lessons
- Evidence
- Confidence

Everything else is considered Runtime.

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

`workspace/REGRET_LOG.md` (created 2026-06-30) is an append-only collection of things the agent considered but didn't add, and things the agent added that turned out not to matter. It is the empirical record of feature-creep pressure and the source of truth for what to delete next.

---

## 7. CLI Reference

The CLI is a convenience surface. The machine-to-machine interface is `mpm call <tool> --payload JSON`. The CLI calls the same handlers internally.

### Daily Commands (root level)

```bash
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
mpm web [--port <n>] [--allow-anonymous]  # Start web UI server (fail-closed; requires web_token)
mpm version        # Version info
mpm help           # Full help
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
mpm kb topic link <topicId> <linkTopicId>

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
mpm ops backfill-embeddings [--batch-size|--collection|--dry-run]
mpm ops dlq:review [review|clear|retry]
mpm ops lint [--dir <path>]... [--show-clean]
mpm ops changelog build [--since v1.0.0] [--release-version 1.1.0] [--legacy]
mpm call query_audit_log [--level|--component|--days|--limit]
mpm call session_end --payload '{"session_id":"...","summary":"...","commitments":[],"open_questions":[]}'
mpm call session_handoff [--unread|--mark_read]
mpm call list_handoffs [--limit|--unread]

# UI
mpm ops web [--port <n>]
mpm ops review [--promoted|--stale]

# Diagnostics
mpm ops stats
mpm ops status
mpm ops prune [--older-than <n>d]

# Data
mpm ops export [--jsonl]
mpm ops backup [path]
mpm ops restore-db <path>
mpm ops ingest <path>

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
mpm ops stance promote

# Multi-agent shared epistemology
mpm ops shared [status]

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

---

## 8. Runtime Features

The Runtime is where MPM evolves. Features in this section are intentionally decoupled from Core — they may be redesigned, replaced, or removed without invalidating existing knowledge.

### Embedding Pipeline

Auto-embed on `mpm add` and on one-shot ingestion via `mpm ops ingest`. `mpm ops backfill-embeddings` provides batched, resume-safe backfill for existing memories. tiktoken (`cl100k_base`) drives token-aware chunking.

### SSE Live Telemetry Stream

The web UI server includes a live Server-Sent Events (SSE) stream at `GET /api/stream` — no polling, no refresh. Every state-changing operation fans out the same event to all connected browser tabs.

Architecture: package-level `SSEBroker` singleton, 50-slot ring buffer for `Last-Event-ID` replay, dual injection paths (agent/CLI via `/api/internal/broadcast` relay, human/web via direct `Broker().Broadcast()`), 15s heartbeat to prevent proxy idle-kill, 256-buffered client channels (slow consumers skip, never block).

Events: `tool_exec`, `memory_saved`, `immune_slash`, `theory_proposed`, `theory_resolved`, `lesson_saved`, `ping`.

### Memory Versioning

Every memory has an append-only version history. `mpm debug history` shows all revisions with timestamps. `mpm debug diff` computes unified diffs between any two versions. Terminal state is captured in `memory_revisions` for `--as-of` time-travel.

### Topic Auto-Suggestion

On `mpm add`, the system automatically suggests linking to semantically related existing topics. Topics are also auto-created for epistemology collections (`decisions`, `theories`).

### Cross-Reference Linking

Bounded bidirectional Memory↔Topic↔Reference cross-refs. Links are created on save and surfaced on recall.

### Synthesis Deduplication

Context-aware deduplication: synthesis deletes the triggering memory after LTM save, preserves oldest `created_at`, transfers topic_memberships, excludes epistemology collections, quality gate requires ≥2 candidates.

### Reference Library

PDF, EPUB, HTML, Markdown ingestion with token-aware chunking (`--chunk-size`, 64–2048 tokens, default 512). Sources are diff-keyed by content hash: re-ingesting an unchanged document costs zero embedding work. Embedding is a separate phase from chunk insert, so slow embed calls never block ingest. Reference docs are a *shelf*, not a memory: they live in SQLite, are indexed for full-text and semantic search, and are surfaced to agents on demand. Admission (whether a pattern from a consulted reference is worth promoting to long-term memory) is a per-consult decision, not an ingest-time gate.

### Session Memory Context (`wake`)

`mpm wake` surfaces the last session's mode, persona, topics, and recent memories — the agent's bootstrap context on startup.

### Scheduled Wakes (Phase 5a — Stateless, Opportunistic)

The agent can defer work to a future moment with `mpm call schedule_wake` and have the reminder surface automatically on the next call. No cron daemon, no background ticker, no long-lived process — the database is the queue, the dispatcher is the trigger.

```
mpm call schedule_wake --payload '{
  "reason": "Check WC2026 R32 result for Germany",
  "target_time": "24h",
  "theory_id": "7383f1572d73bc9a"
}'
```

`target_time` accepts either an absolute unix epoch or a relative duration (`30s`, `5m`, `2h`, `1d`, `7d`). Resolves to absolute at insert time and is returned in `target_iso` for log clarity.

The "agent has initiative" effect: any subsequent `mpm call` (CLI or MCP) that lands after `target_time` surfaces the wake inline as a `WakesPending` block in the response. The agent sees it, evaluates the reason, optionally schedules the next round. No missed reminders even if the agent is asleep — the next call after the target picks it up, marked with `overdue_secs` for honest accounting.

Companion tools: `check_wakes`, `list_wakes`, `mpm__check_wakes`. Architecture: `scheduled_wakes` table + composite index `scheduled_wakes_due(fired, target_time)` + FTS5 virtual table for content search. `CheckPendingWakes` runs in a single transaction (idempotent across concurrent callers).

**Trade-off vs. a real-time push daemon:** MCP has no server-initiated messages over stdio, so mpm-mcp cannot fire a wake back to a sleeping agent. The opportunistic fold is the next-best mechanism — at-most-once-on-next-contact, not real-time. For Wimbledon R1, WC2026 group stage, and monthly Meshal reminder use cases this is sufficient. Real-time push would require an SSE transport change and is deferred.

### XITL Stance Hot-Swap

`mpm ops stance assume <mode> <persona> <rationale>` switches mode/persona at runtime with no restart. The new directive prints to stdout — OpenClaw captures it and injects into session chat history, so the agent reads and adopts it on the very next turn. `mpm ops stance synthesize <name>` generates a JIT ephemeral persona from a prompt; `mpm ops stance promote` flushes it to a permanent `.md` file.

### Directives (Prime Operating Principles)

Directives are the agent's **prime directives** — non-negotiable behavioral principles that govern how it operates. Unlike modes (which govern retrieval parameters) and personas (which govern tone), directives are the hard rules: the things the agent must and must not do on every turn.

#### Reflex Engine — two-tier behavioral rules

| Tier | Lives in | Loaded | Examples |
|---|---|---|---|
| Prime Directives | SQLite `collection='directives'` | Always, via `read_directives` | "Always log contradictions as challenge evidence, never overwrite" |
| Mode Directives | `mode/*.md` files | Only when the agent is in that mode | "Query confidence history on suspicious memories before debugging" |

Storage: `collection='directives'` (MCP path) or legacy `is_prime_directive = 1`. Both read paths query `WHERE (collection = 'directives' OR is_prime_directive = 1) AND deleted_at IS NULL`, so a directive is visible from every consumer regardless of which identifier was set.

Elevation: `mpm call save_to_memory --payload '{"fact": "Always verify before acting", "collection": "directives", "tags": ["prime_directive"]}'`.

The `proactive_recall_hint` engine also elevates directive-adjacent memories when the current conversation context matches their semantic territory.

### Modes & Personas (File-Based)

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

### Security Scanning

Content scanned against **20 regex patterns** (API keys, JWTs, SSH keys, connection strings, password patterns) before any database write. Blocked content goes to `mirror.jsonl` but never reaches the database. Coverage enforced by `internal/scanner_coverage_test.go` — a static-analysis test that walks every function containing a literal `INSERT INTO memories` and verifies the function (or its caller) calls the scanner.

**Auth policy:** `mpm web` defaults to **fail-closed** — if `web_token` is unset, the server refuses to start. Pass `--allow-anonymous` to opt in for trusted-LAN debugging; the server prints a loud warning and sets `X-MPM-Auth: disabled-anonymous` on every response.

### Fsnotify Reconciliation

30s startup delay + 10-min periodic sweep (25 file/sweep cap) reconciles the filesystem state. `source_path` metadata check prevents re-ingestion of already-processed files. (Residual from the deprecated watcher; consolidated into on-demand ops.)

### Spaced Reinforcement Review

```bash
mpm ops review --promoted   # Show recently elevated LTM memories
mpm ops review --stale      # Surface forgotten LTM memories
```

### Watcher Deprecation (2026-06-26)

The MPM file-watcher daemon (`mpm watch start|stop|status`, fsnotify goroutine, worker pool, 5-min decay loop) was deprecated and removed on 2026-06-26. Its original purpose — auto-ingest of files written by pre-MCP agents — is obsolete now that `save_to_memory` is a native MCP tool. Replacement matrix:

| Watcher capability | Replacement |
|---|---|
| 5-min decay sweep | `mpm ops maintain` on demand |
| External SQLite polling | `mpm ops ingest --source <path>` one-shot |
| LLM synthesis | `mpm ops synthesize [--dry-run]` on demand |
| Topic clustering | `mpm ops synthesize` (synthesis pass) |
| Filesystem auto-ingest | Obsolete — `mpm call save_to_memory` covers it |

The reusable parser library (`extractFacts`, `extractFromSessionLine`, `looksLikeFact`, `extractKeywords`, `parseSessionsSnapshot`, `readJSONLines`) was preserved in `cmd/mpm/parsers.go` for future one-shot filesystem tooling.

---

## 9. Reliability

MPM is designed for long-running autonomous operation: SQLite WAL mode, dead-letter queues, synthesis isolation, retry pipelines, event replay buffers, watchdog telemetry, overflow protection, and a closed self-healing integrity loop.

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

The cognitive loop closes through the existing wake context: when self-heal escalates unknown drift, it injects a pending theory; the next `mpm call read_wake_context` surfaces it; the agent acts on it. **No new wake code is needed.**

#### Recommended cron

```cron
0 4 * * 0 cd /home/v/workspace/projects/mpm && /usr/local/bin/mpm ops self-heal || echo "MPM drift detected: $(date)" | mail -s "MPM Self-Heal Alert" v
```

### Self-Audit Log

Runtime telemetry has historically lived in flat files (`watchdog.jsonl`, `mirror.jsonl`) — readable by humans grepping logs, blind to the agent. MPM moves that telemetry into the cognitive surface so the agent can see what went wrong across sessions.

```bash
mpm call query_audit_log --days 1 --limit 20
mpm call query_audit_log --level error --days 7
mpm call query_audit_log --component relay --days 1
```

The wake context surfaces a single-line summary when errors or fatals were logged in the last 24h.

#### Storage

| Column | Type | Purpose |
|---|---|---|
| `id` | TEXT PK | Unique identifier |
| `level` | TEXT | `warn` / `error` / `fatal` (CHECK constraint) |
| `component` | TEXT | `relay`, `synthesis`, `watcher`, `security` |
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
| `watcher` | DEPRECATED — new audit rows not expected |

#### Why a SQLite table, not more jsonl files

The audit log is queryable from the agent via the JSON boundary, surfaceable in wake context, and pruneable by the existing gc sweep — all without new infrastructure.

---

## 10. Glossary

**Artifact.** A persistent cognitive object: Memory, Decision, Theory, Lesson, or Evidence. Each has its own lifecycle.

**Challenge.** The act of marking a memory as questionable. Creates a linked theory; the memory's weight weakens atomically.

**Confidence.** A derived `[0, 1]` estimate of how much the system believes an artifact. Computed from supporting evidence minus contradicting evidence minus time decay. The stored `confidence` column is a cache; the evidence ledger is authoritative.

**Core.** The small, stable part of MPM: Memories, Decisions, Theories, Lessons, Evidence, Confidence. Changes here require demonstrated usage evidence.

**Decay.** Automatic reduction of confidence over time without reinforcement.

**Decision.** A choice with explicit context, the choice made, and the rationale.

**Evidence.** A piece of information that supports or challenges another artifact. Confidence is derived from the evidence ledger.

**LTM (Long-Term Memory).** A memory that has reached `weight ≥ 10` — high enough to be considered permanent institutional knowledge.

**Memory.** A general fact, observation, or synthesized insight.

**Recency bonus.** A retrieval scoring term that prefers recent memories for time-sensitive queries.

**Runtime.** The evolvable part of MPM: wake, scheduling, personas, modes, directives, routing, audit, review. Changes here are cheap; changes to Core are not.

**Theory.** A testable hypothesis with explicit validation criteria. Theories have lifecycle states: pending → confirmed | disproven.

**Wake.** A scheduled reminder stored in `scheduled_wakes`. Surfaces on the next MPM call after `target_time`.

---

## License

See [LICENSE](LICENSE).