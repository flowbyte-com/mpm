# MPM — Memory Persistence Module

> **SQLite-native memory and reasoning infrastructure for autonomous AI agents.**

MPM is a single binary that provides long-term memory, behavioral modes, persona management, and an **epistemology engine** — everything stored in one SQLite database with FTS5 full-text search. Zero external services.

MPM is the memory and reasoning layer for AI agents (OpenClaw + Hermes). It tracks not just *what* the agent knows, but *why* it decided to act, *how* it chose to act, and *what it believes but hasn't proven yet*.

---

## The Problem

Most AI memory systems focus on retrieval. An agent stores information, retrieves information, and continues operating. The problem is that real cognition involves more than facts.

Consider a software engineering agent:

- **Fact:** WordPress strips inline style tags.
- **Decision:** Store widget CSS in `wp_options`.
- **Hypothesis:** The CLI parser fails when `--json` appears before positional arguments.
- **Evidence:** Unit tests confirm the parser bug.
- **Conclusion:** Flag ordering caused the issue.

Most memory systems flatten these into generic notes. Months later the agent remembers the conclusion but not the reasoning. The result: repeated investigations, reopened decisions, contradictory conclusions, lost institutional knowledge.

## The Solution

MPM is a persistent memory and reasoning layer for AI agents. It provides:

- Long-term memory
- Decision tracking
- Hypothesis management
- Proactive recall
- Behavioral modes
- Persona management
- Knowledge self-correction

Everything is stored in a unified SQLite database. No vector database. No daemon. No external services. No distributed infrastructure. Just one binary.

## Why MPM Is Different

Most memory systems store facts. MPM stores reasoning.

| Capability | Traditional Memory | MPM |
|---|---|---|
| Fact Storage | ✓ | ✓ |
| Long-Term Recall | ✓ | ✓ |
| Semantic Search | ✓ | ✓ |
| Decision Tracking | ✗ | ✓ |
| Hypothesis Management | ✗ | ✓ |
| Evidence Chains | ✗ | ✓ |
| Knowledge Challenges | ✗ | ✓ |
| Proactive Recall | Partial | ✓ |
| Reasoning Persistence | ✗ | ✓ |

The goal is not simply remembering information. The goal is **preserving intellectual progress**.

## Vision

Most AI memory systems answer: *What does the agent remember?*

MPM attempts to answer: *What does the agent know, why does it believe it, and what evidence could prove it wrong?*

That distinction transforms memory from passive storage into persistent reasoning.

---

## What MPM Is NOT

- **Not a daemon** — every command is a single binary invocation. No long-running processes; periodic work happens on demand via `mpm ops maintain`.
- **Not generic storage** — built for AI agent cognition: weighted recall, decay, epistemology, proactive hints.
- **Not a human dashboard** — machine-to-machine interface is primary; CLI is a convenience layer.

---

## Core Concepts

### Memories

General facts, observations, and synthesized insights.

> Germany leads Group E with +6 goal differential.

Memories are weighted, searchable, reinforced, challenged, and eventually archived.

### Decisions

Choices captured together with context and rationale.

```
CONTEXT: Need CSS injection that survives wp_kses filtering.
CHOICE: Store widget CSS in wp_options.
RATIONALE: WordPress strips inline style tags.
```

This allows agents to reconstruct previous reasoning instead of re-evaluating the same problems repeatedly.

### Theories

Hypotheses that have not yet been proven.

```
HYPOTHESIS: Flag ordering causes parser failure.
VALIDATION: Run parser tests with positional-first and flag-first inputs.
STATUS: pending
```

Theories create a structured workflow for experimentation and debugging.

### Lessons

Reusable knowledge that survives across tasks — best practices, warnings, patterns, and insights.

### Sessions

Operational context (current project, active model, working directory, runtime state) so agents can resume work after interruptions.

### Collections

| Collection | Purpose | Default Weight |
|---|---|---|
| `memories` | General facts, synthesized insights | 1 |
| `session` | Operational facts (CWD, model changes) | 1 |
| `decisions` | Architectural choices with rationale | 1 |
| `theories` | Hypothesis + validation criteria | 1 |
| `lessons` | Insights, warnings, best practices | 1 |

### Weight & Reinforcement

Every memory has a `weight` (default 1.0). FTS5 BM25 score, reinforcement count, and recency all factor into relevance. LTM (long-term memory) threshold is weight ≥ 10.

**Feedback-Driven Weight Adjustment:** `mpm +<id>` reinforces (+1, auto-clears challenge if contested); `mpm -<id>` weakens (-1, floor at 1 via SQL MAX). Flag collision guard protects `-v`, `-h`.

```bash
mpm +<id>             # Reinforce (+1, auto-clears challenge if contested)
mpm -<id>             # Weaken (-1, floor at 1)
mpm snooze <id>       # Bump relevance without LTM promotion
mpm promote <id>      # Elevate to LTM (weight=10)
mpm ops gc --shred-negative  # Shred memories with weight<0 AND proven theory exists
```

### Provenance & ISR Telemetry

Every memory stamps its origin at write time:
- `client` — `mpm_cli` or `mpm_call`
- `mpm_mode` — `native` or `watch`
- `mpm_persona` — `operator` or `watch-daemon`

`mpm ops stats` surfaces a nested Client → Model → Persona matrix with **Idea Survival Rate (ISR %)** per cell — the percentage of memories with weight > 1 (not decayed to floor).

---

## The Epistemology Engine

MPM does not attempt to determine truth. MPM maintains a continuously updated estimate of confidence based on available evidence.

Truth is external. Confidence is internal. MPM manages the latter. Reality adjudicates the former.

This distinction protects the architecture from scope creep — it is a confidence estimation engine, not a truth determination engine. The Epistemology Engine extends the memory model into genuine agency: reasoning that can be examined, revised, and rendered obsolete.

It answers questions that traditional memory systems cannot:

- Not: *What does the agent know?*
- But: *Why does the agent believe it?*
- And: *Has that belief been tested?*
- And: *What evidence could invalidate it?*

### Decision Ledger

An append-only audit trail of architectural choices. Captures the context, the choice made, and the reasoning — so weeks later, the agent can reconstruct *why* a particular approach was taken instead of blindly second-guessing itself.

```bash
mpm record_decision "CONTEXT: We needed a CSS injection mechanism that survives wp_kses filtering
CHOICE: Route all widget CSS through agentshell_register_widget → wp_options → widgets.php <head> injection
RATIONALE: WordPress strips <style> blocks from post content via wp_kses_post() even for admins. The widget init JS also needs a footer injection point. Both requirements pointed to wp_options as the store."

mpm decisions                   # Formatted decision ledger
```

### Theory Tracker

A hypothesis ledger for debugging and design. When the agent forms a causal assumption ("I think X is causing Y"), it logs the hypothesis and a concrete validation test before writing the fix. This forces the assumption to be testable, and often collapses a false hypothesis before it wastes an hour.

```bash
mpm propose_theory "HYPOTHESIS: passing --json before the positional arg causes the parse bug
VALIDATION_CRITERIA: write a unit test — invoke mpm with --json flag first vs positional-first, compare parse error rate
STATUS: pending"

mpm theories                    # List all theories with status chips
mpm theories pending           # Filter to pending only

mpm resolve_theory abc123 "confirmed: flag order matters, --json consumed before positional processing"
```

### Cognitive Immune System (Challenge Lifecycle)

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
challenge ──────────────────────────────────────────────────────▶ [pending theory]
    │
    ├── challenge restore ──────────────────────────▶ [theory: disproven]
    │
    └── shred ──────────────────────────────────────▶ [theory: deleted]
```

**`mpm challenge <id> "<evidence>"`** — atomic:
1. Patch memory metadata: `{"status":"challenged","challenged_theory_id":"<theory_id>"}`
2. Create theory with back-link: `{"status":"pending","type":"challenge","memory_id":"<memory_id>"}`
3. Weaken memory weight by 3

**`mpm challenge restore <id>`** — atomic:
1. Resolve theory: `status → disproven`, clear `memory_id`
2. Clear memory metadata flags (RFC 7396 JSON patch)

**`mpm shred <id>`** — atomic:
1. `DELETE topic_memberships WHERE memory_id = ?`
2. `DELETE theory` (if exists)
3. `DELETE memory`

**`mpm ops gc --shred-negative`** — shreds only memories with weight<0 AND a proven theory exists. Negative weight alone is never sufficient — the theory provides the evidence chain.

### Proactive Recall

Traditional memory systems wait for a search query. MPM actively surfaces relevant knowledge before it is requested.

```bash
mpm kb hint "discussing the CSS injection approach for the widget system"
# → 💡 [Recall] You decided: Route all widget CSS through agentshell_register_widget...
#    RATIONALE: WordPress strips <style> blocks from post content...
```

FTS5 keyword extraction detects semantic overlap with the current context and pushes a low-latency recall hint — with STATUS and RATIONALE displayed directly, not just the content. The `proactive_recall_hint` plugin tool is wired into the OpenClaw agent loop — it surfaces the most relevant epistemology memory automatically after context shifts.

### Confidence and Evidence

Memories, theories, decisions, and lessons each carry a `confidence` value
that's derived from the evidence supporting them. The system tracks what
the artifact is, what evidence has accumulated, and how that evidence
decays over time — the three are decoupled so confidence can move without
modifying the artifact content.

#### Key concepts

- **Knowledge and confidence are independent.** Adding contradicting
  evidence changes confidence but leaves the artifact content untouched.
  A hindsight annotation can change what the artifact says without
  retroactively changing what was believed about the original.
- **Confidence is the system's *current* estimate of truth** derived from
  *current* evidence. The artifact is historical fact.
- **Confidence only rises with new evidence.** It is allowed to decrease
  automatically as time passes without reinforcement.
- **Confidence column is a performance cache, not the source of truth.**
  The stored `confidence REAL` value is a performance optimization — every
  confidence score is always `f(evidence, decay_rules)`. If the stored value
  ever disagrees with the result of `f(evidence, decay)`, that is a bug.
  The `query_confidence_history` tool is the audit trail; `explain_confidence`
  (planned) is the reasoning trace. Both derive from evidence, not from the
  stored column.

#### Commands

| Command | MCP tool | Description |
|---|---|---|
| `mpm evidence add --artifact <id> --type <t> --source <s> --by <who>` | `add_evidence` | Add a piece of evidence to an artifact |
| `mpm evidence list --artifact <id>` | `list_evidence` | List all evidence for an artifact |
| `mpm ops confidence show --artifact <id>` | `query_confidence_history` | Show current confidence + history |
| `mpm ops confidence changes --artifact <id>` | `query_confidence_changes` | Recent confidence-altering events with delta and trigger — answers "what moved and why?" (default: last 24h) |
| `mpm ops confidence trend --artifact <id>` | `query_confidence_trend` | Trajectory over window: velocity (slope/day), delta_window, trend label — answers "where is this going?" (default 30d) |
| `mpm call query_memory_quality` | `query_memory_quality` | Per-source memory statistics: volume, avg confidence, challenge rate, survival rate — answers "which models produce memories that survive?" |
| `mpm ops confidence explain --artifact <id>` | `explain_confidence` | Component breakdown: evidence strength, effective weight, decay penalty, initial odds, top contributors — the reasoning trace (distinct from history audit trail) |
| `mpm ops confidence recompute --artifact <id>` | (manual CLI only) | Trigger a manual recompute |

Evidence types: `observation` (0.4), `test` (0.7), `reproduction` (0.85),
`challenge` (-0.6), `decision_outcome` (0.95), `external_reference` (0.6).
Strength defaults to the type's registry value; override with `--strength`.

Artifact types: `memory`, `theory`, `decision`, `lesson`. The MCP tools
enforce these as a strict enum so the LLM cannot invent categories.

#### Atomicity guarantee

`add_evidence` and `query_confidence_history`-triggered recomputes run
inside a single SQLite transaction (`WithTx` over `DBNode`). If the
recompute fails (e.g. CHECK constraint violation on
`confidence_history.trigger`), the evidence INSERT rolls back with it —
no orphan evidence rows, no confidence column updated without a matching
history row.

#### Initial confidence by type

| Artifact type | Initial confidence |
|---|---|
| memory | 0.8 |
| theory | 0.5 |
| decision | 0.6 |
| lesson | 0.7 |

See the [Confidence and Evidence](#confidence-and-evidence) section above for the design rationale; the original design document from 2026-06-16 lives in git history (commit `b51066a`) but is no longer maintained as a separate doc — README is the source of truth.

### How It All Connects

| Component | Role |
|---|---|
| `propose_theory` / `record_decision` | Ingest reasoning into structured collections |
| Topic auto-link | Theories and decisions are automatically linked to their respective topics |
| `resolve_theory` | Closes the hypothesis lifecycle — status + conclusion, weight bumped |
| `theories` / `decisions` | Display formatted views with STATUS chips |
| `mpm hint` | Proactive surfacing via FTS5 keyword overlap |
| Synthesis engine | Epistemology collections are **excluded** from auto-synthesis — their lifecycle is separate |
| Watcher | Auto-detects `HYPOTHESIS:` and `CHOICE:` prefixes in `.md` files and routes to correct collection |
| Backfill | On first `mpm doctor` run, existing epistemology memories are linked to their topics |

### Content Format Conventions

| Collection | Content Format |
| --- | --- |
| `decisions` | `CONTEXT:\nCHOICE:\nRATIONALE:\n[OUTCOME:]` |
| `theories` | `HYPOTHESIS:\nVALIDATION_CRITERIA:\nSTATUS:` |
| `theories` (challenge) | `HYPOTHESIS: Memory <id> is obsolete.\nRATIONALE: <evidence>\nSTATUS: pending` |

---

## The JSON Boundary (`mpm call`)

All MPM operations are accessible via a universal machine interface. Both OpenClaw (TypeScript) and Hermes (Python) agents call MPM using the same JSON protocol — no CLI flag parsing, no split-brain architecture.

```bash
# Universal machine interface — all MPM operations via JSON
mpm call <tool> --payload JSON

# Examples:
mpm call save_to_memory --payload '{"fact": "Germany leads Group E with +6 GD", "tags": ["wc2026"]}'
mpm call query_long_term_memory --payload '{"query": "World Cup prediction"}'
mpm call propose_theory --payload '{"hypothesis": "Germany wins", "validationCriteria": "semi-final minimum"}'
mpm call resolve_theory --payload '{"theoryId": "abc123", "conclusion": "confirmed", "newStatus": "proven"}'
mpm call challenge_memory --payload '{"memoryId": "abc123", "evidence": "recent data contradicts this"}'
mpm call add_evidence --payload '{"artifact_id":"abc123","artifact_type":"memory","type":"test","source_group":"unit_test_suite","strength":0.7,"created_by":"openclaw-agent","notes":"parser fix verified"}'
mpm call list_evidence --payload '{"artifact_id":"abc123","artifact_type":"memory"}'
mpm call query_confidence_history --payload '{"artifact_id":"abc123","artifact_type":"memory","limit":10}'
```

This is the **machine-to-machine interface**. The human-facing CLI (documented below) calls the same handlers internally.

#### MCP/C parity: what's exposed via `mpm call` and the MCP server vs CLI-only

Every `mpm call <tool>` entry has a matching MCP tool spec; both call the same `DatabaseManager` methods. The 33 tools in the [MCP tool surface](#mcp-tool-surface) cover the agent's daily workflow: read/write memory, lessons, topics, references, theories, decisions, evidence, confidence, references, route, wake, directives, log_to_changelog, plus the memory feedback loop (`shred`/`reinforce`/`weaken`/`snooze`/`set-weight`/`patch`/`promote`) and workflow (`review`/`synthesize`/`gc`).

A handful of CLI commands are intentionally **NOT** exposed via MCP/call because they're operationally distinct (destructive, cron-friendly, or human-gated):

| CLI command | Why CLI-only |
|---|---|
| `mpm ops doctor` | Diagnostic report; agents don't need to run it, scheduled cron does |
| `mpm ops lint` | Pre-commit / CI hook; agents should never invoke regex validation |
| `mpm ops backfill-embeddings` | Bulk operation; runs on a schedule, not per-request |
| `mpm ops synthesize [--dry-run]` (bulk) | Per-memory synthesis IS exposed as `synthesize_memory`; bulk scan is human-initiated |
| `mpm ops gc --review` / `--purge` / `--shred-negative` | Destructive mass operations; core safe `gc_run` IS exposed |
| `mpm ops changelog build` | Release engineering; not an agent task |
| `mpm ops maintain` | Maintenance wrapper; individual ops below are exposed |
| `mpm web` | Long-running HTTP server; can't be a tool call |

If an agent needs any of these, the operator should run it explicitly. Tool calls that could damage state are intentionally kept on the human-facing CLI where the cost of a misclick is bounded by the operator's attention.

---

## Retrieval Architecture

MPM combines:

- SQLite FTS5 (BM25 ranking)
- Semantic embeddings (cosine similarity, 768d via `nomic-embed-text`)
- Reinforcement history
- Recency scoring

This enables both:

```bash
mpm "world cup prediction"
mpm recall --semantic "为什么德国队表现这么好"
```

to retrieve relevant knowledge.

**Hybrid scoring** (in `internal/hybrid_search.go`):
`score = (reinforcement_count × 2) + (weight × 1.5) + recency_bonus`

BM25 unbounded scores are sigmoid-normalized. Use `--semantic` flag to enable pure embedding search.

**Memory provenance (`mpm recall --why`):** every result can be annotated with the score breakdown that retrieved it. Pass `--why` to see per-result `[why]` lines showing reinforcement contribution, weight contribution, recency age, and the FTS5 terms that matched. Useful for "why did the agent pick this memory?" introspection without re-running the search.

**LTM promotion:** `weight ≥ 10` OR explicit `mpm promote` OR auto-ingested `.md` file.

### Cognitive Immune System (Hybrid Search)

On hybrid search, a contradiction scan evaluates top-15 candidates (≤105 pairs) via cosine similarity.

**Two-tier warning banner system (Phase 5):** Challenged or drifting memories surface with dedicated, in-memory warning banners prepended to the text content during retrieval — the underlying historical database records are never modified:
- **Manual challenges** (`status == "challenged"`): prepends `[Note: This memory is challenged — treat as unverified]`
- **Concept drift** (`concept_drift: true` in metadata): prepends `[SYSTEM WARNING: This knowledge is under active Concept Drift investigation — treat as potentially obsolete]`

**Synchronous state collision resolution (Phase 4):** State collisions (cosine similarity ≥ 0.85 between a challenged memory and an unchallenged candidate) trigger a **synchronous database patch** via `ChallengeMemory()`, which instantly degrades the unchallenged candidate's weight, logs an evidence chain, and atomically flips its status to `challenged` in a single transaction. This eliminates the async ghost loop — state transitions commit atomically on the first collision turn. Equal-tier fall-throughs are bounded to avoid double-challenging an already-resolved target.

### Concept Drift Detection

Concept drift detection — autonomously identifying paradigm shifts where historically trusted knowledge is decaying under a sudden barrage of new counter-evidence — is implemented as a **synchronous** operation that runs inline with `query_long_term_memory` (no background ticker). The detection signature is:

| Signal | Threshold |
|---|---|
| **Baseline** | Artifact achieved high status historically (`peak_confidence >= 0.85`) |
| **Velocity** | ≥ 2 recent challenges or negative-strength observations within the last 7 days |
| **Delta** | Confidence has dropped ≥ 0.25 and sits below the distrust threshold (`< 0.60`) |
| **Lifetime** | ≥ 3 total lifetime negative evidence pieces (prevents one-off flukes) |

When a drifting memory triggers this signature, the engine:
1. **Quarantines** the memory — sets `concept_drift: true` in metadata
2. **Proposes** a pending theory in the theories collection (`trigger='concept_drift'`) to alert the agent
3. **Survives restarts** — SQLite-native dedup check prevents duplicate theory generation across process restarts

The earlier background ticker (every 6 hours, `internal/idle_dream.go`, separate goroutine pool) was removed in the 2026-06-26 purge. Drift detection is now a pure-SQLite operation that piggybacks on the read path — no separate process, no separate timer, no panic-recovery surface to maintain. A drift that the agent missed last query is just as catchable next query.

---

## Quick Start

```bash
# Search memories (default — bare string → recall)
mpm World Cup prediction

# Add a fact
mpm add Germany leads Group E with +6 goal differential

# Interactive add
mpm add -i

# System status
mpm status
mpm ops stats

# Session bootstrap (last session context)
mpm wake

# Knowledge base
mpm kb memory list
mpm kb theories pending
mpm kb decisions
mpm kb hint "discussing the CSS approach"

# Engine room
mpm ops doctor
mpm ops gc
mpm ops maintain
```

---

## CLI Reference

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
mpm web [--port <n>] [--allow-anonymous]  # Start web UI server. Default: fail-closed (requires web_token in mpm_config.json). --allow-anonymous opts in to unauthenticated mode and prints a loud warning at startup.
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
mpm kb reference add <file> [--tag tag1,tag2] [--reason "why this is being added"] [--chunk-size 512]
mpm kb reference ls
mpm kb reference search <query>
mpm kb reference show <id>
mpm kb reference used                    # most-retrieved references
mpm kb reference interactions            # recent retrieval events
mpm kb reference admit [--limit N] [--dry-run]   # run admission function on candidates
mpm kb reference shred <id>

# Epistemology
mpm kb theories [pending|resolved|all]
mpm kb decisions
mpm kb propose_theory <theoryId>       # Interactive hypothesis + criteria
mpm kb resolve_theory <theoryId> <status>
mpm kb record_decision <decisionId>     # Interactive context/choice/rationale
mpm kb hint <topic>                    # Proactive recall from conversation context
```

### `ops` — Engine Room

Maintenance, diagnostics, synthesis, and power tools.

```bash
# Core engine
mpm ops doctor [--explain|--deep-scan]  # Diagnostics (--explain: FTS5 query plan, --deep-scan: integrity audit)
mpm ops self-heal [--dry-run|--force|--quiet]  # Autonomous integrity repair — cron-friendly, escalates unknown drift
mpm ops maintain                     # Self-maintenance: decay, consolidate, prune
mpm ops synthesize [--dry-run]       # LLM synthesis on all memories
mpm ops synthesize status            # Recent synthesis telemetry (last 20 watchdog events)
mpm ops synthesize failures          # Only error/skipped synthesis events (for "why are merges not happening?")
mpm ops gc [--dry-run|--review|--purge|--shred-negative]  # Decay sweep
mpm ops backfill-embeddings [--batch-size|--collection|--dry-run]  # Embedding pipeline
mpm ops dlq:review [review|clear|retry]  # Dead letter queue — failed synth events
mpm ops lint [--dir <path>]... [--show-clean]  # Validate persona/mode router frontmatter (YAML + regex compile) — wired into pre-commit hook
mpm ops changelog build [--since v1.0.0] [--release-version 1.1.0] [--legacy]  # Generate CHANGELOG.md + changelog.json from git log
mpm call query_audit_log [--level|--component|--days|--limit]  # Runtime anomaly ledger (cross-session telemetry)
mpm call session_end --payload '{"session_id":"...","summary":"...","commitments":[],"open_questions":[]}'  # End session with handoff (next wake will surface it)
mpm call session_handoff [--unread|--mark_read]  # Read latest handoff (used by wake context automatically)
mpm call list_handoffs [--limit|--unread]  # Browse handoff history

# Watcher (deprecated 2026-06-26 — see "Watcher deprecation" below)
mpm watch status                         # DEPRECATED — prints deprecation warning, exits 0
mpm ops maintain                         # On-demand decay/cleanup (replaces watcher's 5-min loop)
mpm ops ingest --source <path>           # One-shot external SQLite ingestion (replaces watcher's poll goroutine)
mpm ops synthesize [--dry-run]           # On-demand LLM synthesis (replaces watcher's synth worker)

# UI
mpm ops web [--port <n>]            # Web UI server + SSE telemetry at /api/stream
mpm ops review [--promoted|--stale]  # Spaced reinforcement review

# Diagnostics
mpm ops stats                       # Memory statistics + ISR telemetry
mpm ops status                      # System dashboard (memory counts, daemon, synthesis)
mpm ops prune [--older-than <n>d]   # Prune expired memories

# Data
mpm ops export [--jsonl]            # Export to JSON
mpm ops backup [path]               # Database backup (.sql dump)
mpm ops restore-db <path>           # Restore from .sql dump
mpm ops ingest <path>               # Import from external SQLite

# Context
mpm ops switch                      # Interactive persona/mode switcher
mpm ops directives                  # Show behavioral directives
mpm ops wake                        # Show last session context

# Mode & Persona
mpm ops mode [list|active|remove|clear]
mpm ops persona [list|active|set|clear]

# XITL Stance (runtime persona hot-swap — no restart required)
mpm ops stance assume <mode> <persona> <rationale>
mpm ops stance synthesize <name> [flags]
mpm ops stance promote              # Flush ephemeral persona to permanent disk file

# Multi-agent shared epistemology (Phase 1 — see WISHLIST.md)
mpm ops shared [status]             # Show MPM_SHARED_DB attach state, row counts, file size

# Entity ops
mpm ops topic|lesson|session|reference|memory|wake|gateway
```

### `debug` — Low-Level Inspection

```bash
mpm debug history <memoryId>         # Version history for a memory
mpm debug diff <memoryId> <v1> <v2> # Unified diff between two versions
mpm debug diff-lines <text1> <text2>  # Compute diff of two text blocks
mpm debug patch-memory <id> <json>   # Patch metadata in-place (FTS untouched)
mpm debug shred <id>                # Secure cascade-delete memory + linked theory
mpm debug show <id>                 # Show memory details
mpm debug gc [--dry-run]            # Decay sweep (dry-run for inspection)
```

---

## Feature Reference

### Embedding Pipeline

- Auto-embed on `mpm add` and on one-shot ingestion via `mpm ops ingest`
- `mpm ops backfill-embeddings` — batched, resume-safe backfill for existing memories
- tiktoken (`cl100k_base`) for token-aware chunking

### SSE Live Telemetry Stream

The web UI server includes a live Server-Sent Events (SSE) stream at `GET /api/stream` — no polling, no refresh. Every state-changing operation across both the agent/CLI path and the human/web UI path fans out the same event to all connected browser tabs simultaneously.

**Architecture:**
- `SSEBroker` — package-level singleton in `cmd/mpm/stream.go`, distinct from any HTTP server instance, so it can be called from `main()` with no server handle
- 50-slot **ring buffer** — last 50 events cached for replay on browser reconnect via `Last-Event-ID` header
- Dual injection paths:
  - **Agent/CLI path** (`call.go`): `handleCall` POSTs to `/api/internal/broadcast` relay endpoint after every tool completes. The relay is synchronous (completes before `mpm call` exits) so no events are lost to goroutine preemption.
  - **Human/web UI path** (`handlers.go`): `Broker().Broadcast()` called directly in each handler
- 15s **heartbeat ping** — prevents proxy idle-kill on long-lived SSE connections
- 256-buffered client channels — slow consumers skip events (non-blocking send), never blocking broadcast
- Deferred `unregister` on `r.Context().Done()` — goroutine leak prevention on browser disconnect

**Broadcast events:**

| Event | Trigger | Payload |
|---|---|---|
| `tool_exec` | Any `mpm call` completion | `tool`, `result` |
| `memory_saved` | `save_to_memory` / web UI add | `id`, `content`, `collection`, `tags`, `provenance` |
| `immune_slash` | `challenge_memory` | `memory_id`, `theory_id`, `action`, `theory_status` |
| `theory_proposed` | `propose_theory` | `id`, `hypothesis`, `status` |
| `theory_resolved` | `resolve_theory` | `id`, `status`, `conclusion`, `resolved_at` |
| `lesson_saved` | `save_lesson` | `id`, `type`, `fact` |
| `ping` | 15s heartbeat | `{}` |

**Browser client (`app.js`):**
- `EventSource('/api/stream')` with `Last-Event-ID` replay — on reconnect, browser sends `Last-Event-ID` and server replays all buffered events with ID > lastSeen
- `localStorage.setItem('mpm_sse_id', lastEventId)` — persisted across page reloads
- Connection status dot: orange (connecting) → green (live) → red (disconnected)
- DOM mutators: `prependMemoryCard`, `prependLessonCard`, `flashMatrixCell` — no second GET round-trip
- `refreshStats()` on theory events — silent re-fetch of `/api/status`

**CSS animations (`style.css`):**
- `@keyframes fadeSlideIn` — memory/lesson cards slide in from top on arrival
- `.slashed` — `text-decoration: line-through` + `opacity: 0.5` for challenged memories
- `[data-isr-cell]` flash transition for theory resolve/propose

**Web port discovery:** The web server writes its active port to `~/.mpm/web.port` on startup (and removes it on exit). `mpm call` reads this file to find the relay endpoint — no environment variable inheritance required between separate process invocations.

### Watcher / Synth Isolation

The watcher daemon was deprecated 2026-06-26 (see [Watcher deprecation](#watcher-deprecation-2026-06-26)). Synthesis now runs on-demand only — `mpm ops synthesize [--dry-run]` and the per-memory `mpm call synthesize_memory` MCP tool. Both use the same multi-vendor fallback chain: **MiniMax → OpenAI → Ollama (local)**. Each vendor has independent timeout (10s). All vendors fail → error returned to the caller; the caller decides whether to retry.

### Deadlock Observability

`DatabaseManager` watchdog writes to `watchdog.jsonl` (separate from `mirror.jsonl`). Slow query threshold: 100ms. Exponential backoff on lock contention. `ExecTracked`, `QueryTracked`, `QueryRowTracked` methods log all DB operations.

### Automatic Decay & Archive (deprecated 2026-06-26 — see "Watcher deprecation")

The watcher used to drive decay every 5 minutes via an idle-consolidation goroutine. With the watcher removed, decay is now **on demand**: run `mpm ops maintain` periodically (every few weeks is plenty). The decay math is unchanged — `MAX(weight - MAX(1, CAST(weight × DecayRate AS INTEGER)), 1)` — only the trigger moved. Weight=1 memories past 30-day archive threshold are soft-deleted; terminal state is captured in `memory_revisions` for `--as-of` time-travel.

### Watcher deprecation (2026-06-26)

The MPM file-watcher daemon (`mpm watch start|stop|status`, plus the internal fsnotify goroutine + worker pool + 5-min decay loop) was deprecated and removed on 2026-06-26. Its original purpose — auto-ingest of files written by pre-MCP agents — is obsolete now that `save_to_memory` is a native MCP tool.

**Removed:**
- `cmd/mpm/watch.go` (1800 lines)
- `cmd/mpm/worker.go` (WorkerPool, 399 lines)
- `cmd/mpm/watch_lifecycle_test.go`
- `contrib/systemd/mpm.service`
- All daemon-mode audit rows (the `watcher` component is now dead)

**Preserved as a reusable library** (`cmd/mpm/parsers.go`):
- `extractFacts`, `extractFromSessionLine`, `looksLikeFact`, `extractKeywords` — regex parsers
- `parseSessionsSnapshot` — sessions.json → structured map
- `readJSONLines` — JSONL line reader
- Pre-compiled regex patterns (`factPatterns`, `skipPatterns`, etc.)

A future one-shot CLI command can rebuild filesystem auto-ingest on top of this library without re-deriving the parsers.

**Replacement matrix:**

| Watcher capability | Replacement |
|---|---|
| 5-min decay sweep | `mpm ops maintain --quiet` on demand |
| External SQLite polling | `mpm ops ingest --source <path>` one-shot CLI |
| LLM synthesis | `mpm ops synthesize [--dry-run]` on demand |
| Topic clustering | `mpm ops synthesize` (synthesis pass) |
| Filesystem auto-ingest (original purpose) | Obsolete — `mpm call save_to_memory` covers it |

**Invocation guard:** `mpm watch start|stop|status|restart|add-path|remove-path|list-paths` all print a friendly deprecation warning and exit 0. Operator scripts that referenced the old daemon won't break loudly.

### Memory Versioning

Every memory has an append-only version history. `mpm debug history` shows all revisions with timestamps. `mpm debug diff` computes unified diffs between any two versions.

```bash
mpm debug history abc123
mpm debug diff abc123 v1 v2
```

### Topic Auto-Suggestion

On `mpm add`, the system automatically suggests linking to semantically related existing topics. Topics are also auto-created for epistemology collections (`decisions`, `theories`).

### Cross-Reference Linking

Bounded bidirectional Memory↔Topic↔Reference cross-refs. Links are created on save and surfaced on recall. `mpm kb topic link <topicId> <linkTopicId>` creates cross-links manually.

### Proactive Hint Engine

FTS5-triggered recall hints via `mpm hint` and `proactive_recall_hint` plugin tool. Extracts conversation keywords (tiktoken, stopword filter), finds epistemology overlaps via BM25, surfaces STATUS and RATIONALE inline — not just the content.

Quality rules: one hint per turn, score >= -3.0, 10-turn suppression window, FTS5 keyword overlap detection.

### Synthesis Deduplication

Context-aware deduplication: synthesis deletes the triggering memory after LTM save, preserves oldest `created_at`, transfers topic_memberships, excludes epistemology collections, quality gate requires ≥2 candidates.

### Reference Library

PDF, EPUB, HTML, Markdown ingestion with token-aware chunking (`--chunk-size`, 64–2048 tokens, default 512). Sources are diff-keyed by content hash: re-ingesting an unchanged document costs zero embedding work; only changed chunks get re-embedded. Embedding is a separate phase from chunk insert, so slow embed calls never block ingest. Source tracking via `[Source: ...]` inline chips. Cross-reference linking on recall. Reference docs are a *shelf*, not a memory: they live in SQLite, are indexed for full-text and semantic search, and are surfaced to agents on demand. Admission (whether a pattern from a consulted reference is worth promoting to long-term memory) is a per-consult decision, not an ingest-time gate.

### Session Memory Context (`wake`)

`mpm wake` surfaces the last session's mode, persona, topics, and recent memories — the agent's bootstrap context on startup.

### XITL Stance Hot-Swap

`mpm ops stance assume <mode> <persona> <rationale>` switches mode/persona at runtime with no restart. The new directive prints to stdout — OpenClaw captures it and injects into session chat history, so the agent reads and adopts it on the very next turn.

`mpm ops stance synthesize <name>` generates a JIT ephemeral persona from a prompt, stored in `system_config`. `mpm ops stance promote` flushes it to a permanent `.md` disk file.

### Directives (Prime Operating Principles)

Directives are the agent's **prime directives** — non-negotiable behavioral principles that govern how it operates. Unlike modes (which govern retrieval parameters) and personas (which govern tone), directives are the hard rules: the things the agent must and must not do on every turn.

Examples of directive content:

```
On every session start, call read_wake_context before responding to the user.
MPM is the single source of truth for agent state.
Never exfiltrate private data.
When in doubt, ask.
```

#### Reflex Engine — two-tier behavioral rules

MPM splits behavioral rules across two tiers to avoid crowding the system
prompt with rules that only matter in specific contexts:

| Tier | Lives in | Loaded | Examples |
|---|---|---|---|
| **Prime Directives** | SQLite `collection='directives'` | Always, via `read_directives` | "Always log contradictions as challenge evidence, never overwrite" |
| **Mode Directives** | `mode/*.md` files (e.g. `debugging.md`, `research.md`) | Only when the agent is in that mode | "Query confidence history on suspicious memories before debugging" |

Prime Directives are universal rules the agent must follow on every turn.
Mode Directives are contextual rules injected by the runtime when the
agent's mode matches — they never appear in the system prompt unless the
mode is active.

**Storage:** Directives are SQLite rows identified by either
`collection = 'directives'` (set by `save_to_memory`) or
`is_prime_directive = 1` (legacy column, set by direct SQL or older
code paths). Both read paths — the MCP `read_directives` tool and the
`mpm ops directives` CLI — query `WHERE (collection = 'directives' OR
is_prime_directive = 1) AND deleted_at IS NULL`, so a directive is
visible from every consumer regardless of which identifier was set.

**Access:**

```bash
mpm ops directives          # CLI — reads from ~/.mpm/src/db/mpm.db
mpm call read_directives    # MCP tool — reads from MPM_WORKSPACE/src/db/mpm.db
```

**Important path note:** `mpm ops directives` and the MCP `read_directives` tool read from different databases. The CLI reads the canonical `~/.mpm/` install. The MCP tool reads from `MPM_WORKSPACE` (defaults to current directory). When OpenClaw runs MPM with `MPM_WORKSPACE=~/.mpm`, both paths converge on the same database.

**Elevation:** Save into the `directives` collection. The MCP
`save_to_memory` tool ignores any `is_prime_directive` field — set
`collection: "directives"` instead, and the row will be returned by
`read_directives` automatically.

```bash
mpm call save_to_memory --payload '{"fact": "Always verify before acting", "collection": "directives", "tags": ["prime_directive"]}'
```

The `proactive_recall_hint` engine also elevates directive-adjacent memories when the current conversation context matches their semantic territory.

### Modes & Personas (File-Based)

Modes and personas are `.md` files with YAML frontmatter. **No database, no compile step.** Filename is the identity.

```bash
mode/       # mode/*.md files
persona/    # persona/*.md files
active.json # active mode/persona state
```

Each mode specifies retrieval parameters governing the proactive hint engine:

| Mode | retrieval_limit | retrieval_threshold |
|---|---|---|
| `debugging` | 3 | -2.5 |
| `research` | 20 | -1.0 |
| `architect` | 10 | -1.5 |
| `programming` | 5 | -2.0 |
| `standard` | 7 | -1.5 |

JIT ephemeral personas can override `retrieval_limit` in their JSON blob. A 50-chunk window for a massive codebase migration is valid — the Go backend respects whatever limit the ephemeral persona specifies.

#### Auto-Selection (`route` tool)

MPM includes a zero-latency heuristic router accessible via the `route` MCP tool. It scores incoming prompts against all loaded modes and personas and returns the best-matching ones — without any LLM call, network latency, or external dependency.

```bash
mpm call route --payload '{"prompt": "I need to draft a whitepaper about our Q3 architecture"}'
```

Returns:
```json
{
  "selected_modes":   ["architect", "research", "write"],
  "selected_persona": "venkat",
  "scores": {
    "write":     { "score": 4, "triggers": ["draft", "whitepaper"] },
    "architect": { "score": 4, "triggers": ["(?i)\\barchitecture\\b"] },
    "research":  { "score": 1, "triggers": ["research"] },
    "default":   { "score": 0, "triggers": [] }
  }
}
```

**Frontmatter schema** (every `mode/*.md` and `persona/*.md`):

| Key | Type | Purpose | Routing impact |
|---|---|---|---|
| `name:` | string | Identity (falls back to filename if absent) | none |
| `title:`, `creature:`, `vibe:`, `voice:` | string | Persona identity / voice — LLM context only | none |
| `patterns:` | string (regex fragments, comma-separated) | **When this component speaks** — prompt vocabulary that activates it | +2 per match (explicit frontmatter) |
| `domain_out:` | string (regex fragments, comma-separated) | **When this component refuses** — prompt vocabulary that reduces its score | -1 per match, surfaced in `PenaltiesApplied` |
| `voice_guards:` | string (prose, comma-separated) | **How this component sounds** — things it should NOT say at generation time | none (LLM context only) |

**Scoring rules:**

| | Modes | Personas |
|---|---|---|
| Selection logic | Threshold filter — any score ≥1 activates (multi-select) | Max-pooling — highest-scoring wins (single-select) |
| `patterns:` match | +2 per match | +2 per match |
| `domain_out:` match | -1 per match (logged in `penalties_applied`) | -1 per match (logged in `penalties_applied`) |
| `voice_guards:` | ignored by router entirely (LLM context only) | ignored by router entirely (LLM context only) |

**Persona selection rule (revised 2026-06-26):** A persona activates ONLY when at least one of its `patterns:` matches the input (net score ≥ 1 after `domain_out:` penalties). There is **no implicit `default` fallback** — if nothing matches, `selected_persona` is empty. The `default` persona still loads and scores like any other; it wins when its patterns match, never for free.

Rationale: the route hook is a **context-injection mechanism for matched routing targets**, not a persona-defaulting mechanism. The agent's active persona lives in `active.json` (set via `mpm mode/persona`) and the route hook must not override it. An unconditional default fallback previously injected a full `<system-reminder>` block for every short conversational prompt (`"hi"`, `"hello there"`), polluting the agent's context window with mode/persona content that wasn't relevant.

**Anti-pattern rename history** (decision `d676c0993c280e1d`): The original `anti_patterns:` field was misconfigured across all 16 components as voice-guard prose (output constraints like 'bikeshedding', 'premature optimization') rather than input filters. The -1 penalty mechanism was dormant because voice-guard phrasings almost never match prompt vocabulary. The field was split into two semantically explicit fields in 2026-06-26:
- `voice_guards:` for output constraints (LLM context, not routing)
- `domain_out:` for input filters (compiled regex, -1 per match, surfaces in `penalties_applied`)

**Hot reload:** The router monitors `mode/` and `persona/` directory mtimes. Any file added or edited is re-parsed and regex recompiled automatically on the next `route` call — no server restart needed.

**Calling systems** (OpenClaw, Claude Code, etc.) receive the JSON and inject `MPM_ACTIVE_MODE` / `MPM_ACTIVE_PERSONA` into the environment before the main generation call. The routing is completely stateless — no file locking, no write collisions.

#### Router Frontmatter Linter (`mpm ops lint`)

The router frontmatter is hand-curated and uses three classes of regex (`(?i)` patterns, `\b...\b` word boundaries, `regexp.QuoteMeta`). YAML's escape rules interact with regex escapes in subtle ways — e.g., `\b` in double-quoted YAML becomes a literal backspace character (0x08), and `\'` inside single-quoted YAML is rejected by the Go yaml parser. These classes of bug fail silently: the persona loads, the regex compiles, the prompt never matches, no diagnostic surfaces the dead wiring.

`mpm ops lint` is the proactive defense:

```bash
mpm ops lint                    # scan default dirs (~/.mpm/persona + ~/.mpm/mode)
mpm ops lint --dir <path>       # scan a custom dir
mpm ops lint --show-clean       # print OK summary even when nothing is wrong
```

Checks:
1. **YAML frontmatter parses without error** — catches the `\'` family.
2. **Regex compile with (?i) prefix** — mirrors the runtime's compilation shape.
3. **Raw-form regex compile on `domain_out:`** (no `regexp.QuoteMeta`) — catches the QuoteMeta-masked typo class where unbalanced brackets silently compile but never match.

Wired into the pre-commit hook (`.git/hooks/pre-commit`, also tracked at `scripts/pre-commit`). When `persona/` or `mode/` files are staged, the hook aborts the commit with the linter output if any issues are found. Non-router commits skip the linter for speed.

9 unit tests pin the contract (`internal/router_linter_test.go`): clean production files, broken frontmatter, broken regex in patterns, broken raw-form regex in domain_out, no-frontmatter files, multiple issues per file, voice guards NOT checked, deterministic output.

### Security Scanning

Content scanned against **20 regex patterns** (API keys, JWTs, SSH keys, connection strings, password patterns) before any database write. Blocked content goes to `mirror.jsonl` but **never reaches the database**.

**Coverage enforced by** `internal/scanner_coverage_test.go` — a static-analysis test that walks every function in `internal/` and `cmd/mpm/` containing a literal `INSERT INTO memories` statement and verifies the function (or its caller) calls the scanner. If you add a new write path, the test will tell you. New write paths should route through `DatabaseManager.SaveMemory` (which scans) or call `internal.ScanContentForWrite` before any raw INSERT.

**Auth policy:** `mpm web` defaults to **fail-closed** — if `web_token` is unset in `mpm_config.json`, the server refuses to start. The historical "fail open when no token" behavior was the single biggest gap in the original code (any reachable client could read every memory on a LAN). Pass `--allow-anonymous` to opt in to unauthenticated mode for trusted-LAN debugging; the server prints a loud warning and sets `X-MPM-Auth: disabled-anonymous` on every response so clients know.

### Fsnotify Reconciliation

30s startup delay + 10-min periodic sweep (25 file/sweep cap) reconciles the filesystem state. `source_path` metadata check prevents re-ingestion of already-processed files.

### Spaced Reinforcement Review

```bash
mpm ops review --promoted   # Show recently elevated LTM memories
mpm ops review --stale      # Surface forgotten LTM memories
```

---

## Reliability

MPM is designed for long-running autonomous operation:

- SQLite WAL mode
- Dead-letter queues
- Synthesis isolation
- Retry pipelines
- Event replay buffers
- Watchdog telemetry
- Overflow protection
- **Self-healing integrity loop** — see below

The goal is predictable behavior under sustained workloads.

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

**Three drift classes detected:**

| Class | Detection query | Behavior |
|---|---|---|
| Soft-delete ghosts | `memories_fts` rows whose joined `memories` row has `deleted_at IS NOT NULL` | **Auto-fix** (known-safe) |
| FTS orphans | Any `*_fts` row whose rowid is missing from the source table | **Escalate** (unknown) |
| Dangling memberships | `topic_memberships` referencing non-existent topics | **Escalate** (unknown) |

**Four safety boundaries on auto-fix:**

1. **Whitelist-only** — only soft-delete ghosts are auto-fixed. They are proven safe because the `memories_au` trigger is the *expected* cleanup mechanism; when it's missing or bypassed, the only effect is a stale FTS row that no longer matches any real memory.
2. **Bounded blast radius** — if ghost count exceeds `SelfHealMaxFix` (default 1000), the system refuses to auto-fix and escalates instead. A 1000-row drift is no longer "drift" — it's a broken trigger.
3. **Rate-limited** — 24h cooldown between same-signature auto-fixes, stored in `collection=projects` memory id `self-heal-state-marker`. Drift accumulation within the cooldown window is a real bug, not a routine sweep.
4. **Audit trail** — every auto-fix writes a `lessons` table entry tagged `source=self-heal` with timestamp, count, and drift signature. Operator review is one SQL query away.

**The cognitive loop closes through the existing wake context.** When self-heal escalates unknown drift, it injects a pending theory using the standard `HYPOTHESIS: ... \nVALIDATION_CRITERIA: ... \nSTATUS: pending` content format. Pending theories are stored in the `memories` table with `collection=theories`, which the next `mpm call read_wake_context` call surfaces in `recent_memories`. The next agent boot sees the structural anomaly, reads the validation criteria (which point at the exact recovery procedure), and acts on it. **No new wake code is needed** — the agent's existing self-bootstrap mechanism is the loop closure.

**Why known-safe vs. unknown.** The asymmetry is deliberate. Soft-delete ghosts are the *only* drift class where we have a precise model of what the correct state is (FTS row count must equal active memory count) AND we have a proven-safe mechanism that should have produced that state (the `memories_au` trigger). For FTS orphans and dangling memberships, we cannot tell from inside the scan whether the FTS row is the bug (orphan) or the source-table row is the bug (incorrectly deleted, should be restored). Auto-fixing novel drift is exactly the kind of guesswork that causes data corruption. Escalate, let a human — or an agent that has more context — decide.

#### Recommended cron

```cron
# Weekly integrity sweep — alert on non-zero exit
0 4 * * 0 cd /home/v/workspace/projects/mpm && /usr/local/bin/mpm ops self-heal || echo "MPM drift detected: $(date)" | mail -s "MPM Self-Heal Alert" v
```

The `--quiet` flag is unnecessary in cron; absence of output means clean. Non-zero exit means drift was found and escalated — wire to your alerting channel of choice.

#### State marker

The self-heal loop is rate-limited via a `projects` collection memory with a fixed id:

```bash
sqlite3 mpm/src/db/mpm.db "SELECT metadata FROM memories WHERE id='self-heal-state-marker'"
# {"last_run":"2026-06-23T11:14:28Z","last_action":"auto-fixed","last_fixed_count":1,"last_theory_id":"","drift_signature":"..."}
```

To force a fresh run (e.g., after manual repair): `mpm ops self-heal --force`. To reset entirely: `mpm call save_to_memory` with `id=self-heal-state-marker` and a fresh metadata payload, or use direct SQL to `DELETE FROM memories WHERE id='self-heal-state-marker'`.

### Self-Audit Log

Runtime telemetry has historically lived in flat files (`watchdog.jsonl`, `mirror.jsonl`) — readable by humans grepping logs, blind to the agent. MPM moves that telemetry into the cognitive surface so the agent can see what went wrong across sessions.

```bash
mpm call query_audit_log --days 1 --limit 20          # all events, last 24h
mpm call query_audit_log --level error --days 7       # errors only, last week
mpm call query_audit_log --component relay --days 1   # relay subsystem only
```

The wake context surfaces a single-line summary when errors or fatals were logged in the last 24h:

> **Audit note: 3 error(s) in the last 24h. Run mpm call query_audit_log to investigate.**

#### Storage

| Column | Type | Purpose |
|---|---|---|
| `id` | TEXT PK | Unique identifier |
| `level` | TEXT | `warn` / `error` / `fatal` (CHECK constraint) |
| `component` | TEXT | Subsystem name (`relay`, `synthesis`, `watcher`, `security`) |
| `message` | TEXT | Human-readable description |
| `stack_trace` | TEXT | Auto-captured Go stack at log point (truncated to 4KB) |
| `context` | JSON | Structured 3-5 field key-value pair for query |
| `created_at` | DATETIME | UTC timestamp in SQLite `YYYY-MM-DD HH:MM:SS` format |

Indexes: `(level, created_at)`, `(component)`, `(created_at)`. The 30-day retention is enforced by the gc sweep (`mpm ops gc` calls `PruneAuditLog(30)`), not by a SQLite trigger — retention is a tunable policy, not an invariant.

#### Wired Subsystems

Four runtime anomaly sources are wired into the audit ledger:

| Component | Trigger |
|---|---|
| `security` | Poison phrase or sensitive content blocked during memory write |
| `relay` | Non-trivial HTTP error during SSE broadcast (timeouts, DNS, etc. — connection-refused is silent) |
| `synthesis` | All LLM vendors failed; event routed to DLQ |
| `watcher` | DEPRECATED — the watcher daemon was removed 2026-06-26; new audit rows with this component are not expected |

The `LogAudit(level, component, message, stack, ctx)` method is safe to call from any goroutine. It captures the stack automatically if not provided, never panics on a closed DB, and fails silently with a stderr note if the insert itself fails — so callers can wire it on the error path without wrapping every site in defensive code.

#### Why a SQLite table, not more jsonl files

The audit log is queryable from the agent via the JSON boundary, surfaceable in wake context, and pruneable by the existing gc sweep — all without new infrastructure. Flat files would have required: a parser, a rotation policy, a separate retention mechanism, and a new surface for the agent. The table is the minimal substrate that closes the loop.

---

## Use Cases

- **Software Engineering Agents** — preserve debugging investigations, architectural decisions, and implementation rationale.
- **Research Agents** — track hypotheses, evidence, and conclusions across long-running investigations.
- **Enterprise Knowledge Retention** — capture institutional reasoning that would otherwise disappear into chat logs.
- **Multi-Agent Systems** — provide a shared cognitive substrate across multiple autonomous agents.
- **Autonomous Operations** — maintain continuity across long-running workflows.

---

## Agent Integration

MPM integrates directly with AI agents as a **single MCP server**. The Go binary (`bin/mpm-mcp`) is the only substrate; agents connect to it via MCP and receive the full MPM tool surface as native function calls. There is no plugin layer, no Node/TypeScript wrapper, no Python shim — just one binary speaking MCP to whatever client connects.

### Single-path-of-truth architecture

The migration from the legacy plugin model (per-agent TypeScript wrappers calling the CLI binary via `child_process`) to the current MCP server model happened in 2026-06-23. The deleted plugin folders (`agent-plugins/openclaw-mpm-plugin/`, `agent-plugins/opencode-mpm-plugin/`) and their associated TypeScript sources are gone. The MCP server is the only integration surface for MCP-aware clients; Hermes retains a small Python plugin (`agent-plugins/hermes-mpm-plugin/`) because it doesn't yet have an MCP client.

### MCP tool surface

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
record_global_rule
<!-- tools:end — auto-generated by `go generate ./internal/tools/`. Do not edit by hand. -->
```

Wiring a new agent: add `mpm-mcp` to its MCP server config (OpenClaw: `mcp.servers.mpm` in `openclaw.json`; Claude Code: `.mcp.json`; any other MCP-aware client). The server binary is at `bin/mpm-mcp` relative to the MPM repo root.

#### Single source of truth: `internal/tools/registry.go`

Both the CLI (`mpm call <tool>`) and the MCP server iterate the same registry — a package-level `[]Tool` slice in `internal/tools/registry_list.go`. Each entry holds:

- `Name` — the tool identifier (used by both surfaces)
- `Description` — short prose shown to MCP clients
- `Schema` — JSON-Schema (raw bytes, parseable by both surfaces; the MCP server passes it via `mcp.NewToolWithRawSchema`)
- `Handler` — `func(dm *DatabaseManager, ac *ActiveContext, payload map[string]interface{}) (interface{}, error)`. Same function called by both surfaces.

Adding a new tool:

1. Write `handleFoo` in `internal/tools/handlers.go` (one function).
2. Append a `Tool{Name, Description, Schema, Handler}` entry in `internal/tools/registry_list.go`.

Both the CLI dispatcher and the MCP server pick it up automatically. No edits to either `cmd/mpm/call.go` or `cmd/mpm-mcp/tools.go`.

Before this refactor (commit `8423cd8`), each tool had two independently-maintained handlers (`callFoo` in `call.go` and `handleFoo` in `tools.go`) that drifted apart — the CLI would silently keep a stale tool while the MCP server added the new one, or vice versa. Now they share one handler, and `cmd/mpm/registry_roundtrip_test.go::TestRegistry_AllToolsExecuteWithoutPanic` enforces byte-identical JSON output from both surfaces on every tool.

### Session Handoffs (Episodic Memory)

Working memory, long-term memory, audit, and self-heal. Four of the five layers a persistent agent needs. The missing one is **episodic memory** — the thread. When a session ends, the next boot shouldn't have to reconstruct what was happening from FTS-ranked recent memories. It should be told directly.

A handoff is a structured end-of-session record: summary, commitments, and open questions. The next session pulls the latest unread handoff from wake context and uses it to continue work.

```bash
mpm call session_end --payload '{
  "session_id":     "uuid-of-this-session",
  "summary":        "Shipped MPM session handoffs. End-to-end verified.",
  "state":          "clean",                                  # clean | crashed | interrupted | force_end
  "commitments":    ["Run the substrate for a week",           # things this session committed to
                      "Stop building, start using"],
  "open_questions": ["Should commitments be promoted to lessons?"]
}'

mpm call session_handoff --payload '{}'           # returns the latest handoff (read or unread)
mpm call session_handoff --payload '{"unread": true}'    # only unread
mpm call session_handoff --payload '{"mark_read": true}' # return latest and mark it read
mpm call list_handoffs --payload '{"limit": 10}'  # see recent handoffs
```

**Upsert semantics** (decision `1263af51a34b8e69`, 2026-06-26): `session_handoffs.session_id` is `UNIQUE`. A session is a living context, not an append-only ledger. If you write a handoff, the user replies, and the agent does 20 more minutes of substantive work, the final handoff should reflect the new final state — not the snapshot from 20 minutes ago. `session_end` is an UPSERT keyed on `session_id`: **last writer wins**.

- `id` is preserved across upserts (existing id returned) so external references stay stable.
- `created_at` is preserved across upserts (the row's birth time doesn't move).
- `ended_at`, `ended_state`, `summary`, `commitments`, `open_questions` move forward.
- `read_at` and `read_by` are preserved across upserts — wake consumption state survives.

The earlier "warn on UNIQUE failure" code path is gone; the schema constraint is now aligned with the semantic reality.

Wake context surfaces unread handoffs automatically and marks them read, so the same handoff is never shown twice in a row:

```
**Previous Session Handoff** (sess-abc, ended 2026-06-23 12:30 UTC, clean)
  - Summary: Shipped MPM session handoffs. End-to-end verified.
  - Commitments:
    - Run the substrate for a week
    - Stop building, start using
  - Open Questions:
    - Should commitments be promoted to lessons?
```

#### Storage

| Column | Type | Purpose |
|---|---|---|
| `id` | TEXT PK | Unique identifier |
| `session_id` | TEXT UNIQUE | Opaque session identifier (UUID is fine) |
| `ended_at` | DATETIME | UTC end-of-session timestamp |
| `ended_state` | TEXT | `clean` / `crashed` / `interrupted` / `force_end` (CHECK) |
| `summary` | TEXT | 1-3 sentence description of what was done |
| `commitments` | JSON | Array of strings: things this session committed to do |
| `open_questions` | JSON | Array of strings: things still unresolved |
| `read_at` | DATETIME | When the next session surfaced this handoff (NULL = unread) |
| `read_by` | TEXT | Token identifying the reader (e.g. "wake-context", "manual-call") |
| `created_at` | DATETIME | UTC row creation timestamp |

Indexes: `(read_at, ended_at DESC)` for the unread lookup, `(ended_at)` for the recency lookup, `(session_id)` for the unique constraint and direct lookup. The 90-day retention is enforced by `mpm ops gc` calling `PruneHandoffs(90)` — longer than audit log (30d) because handoffs are higher-signal, lower-volume bootstrap data.

#### Why a separate table, not a metadata field on the `sessions` table

The existing `sessions` table holds content snapshots (the session's transcript) and is dormant — zero rows in production. Handoffs are *bootstrap data*, not logs. Mixing them into one table would force every reader to filter by purpose, and a future feature that wants session transcripts would have to also pull in handoffs. Keeping them separate lets each feature evolve independently. The 3-index cost is negligible.

#### Cadence

The agent decides when to call `session_end`. The natural pattern is once per agent session (when v closes the chat, the agent writes a handoff and exits). There's no auto-timer — the agent always has full control over what goes into the summary, commitments, and open questions. A handoff is a deliberate act of synthesis, not a side effect.

### Hermes

Hermes Agent uses a Python plugin (`agent-plugins/hermes-mpm-plugin/`) that calls the CLI binary directly via `subprocess` (mostly the `mpm call <tool> --payload '{...}'` interface — the JSON boundary is the universal machine interface, so the plugin is a thin payload-construction layer over the same handlers MCP uses). Setup: see [`agent-plugins/hermes-mpm-plugin/install.md`](agent-plugins/hermes-mpm-plugin/install.md). The plugin-to-MCP migration was driven by OpenClaw's native MCP support; Hermes does not yet have an MCP client, so the Python wrapper remains.

---

## The Reflex Engine

The Reflex Engine is MPM's zero-latency pre-prompt router. It evaluates
each user prompt against the workspace's mode and persona files and
returns the best match *before* the LLM sees anything — compiled Go
regex, microseconds, no LLM call, no network round-trip, no daemon.

The name is deliberate: a reflex is what runs *before* deliberation. The
engine picks the mode (operational rules) and persona (voice/tone) in a
fraction of a millisecond, so the LLM receives the right context as if
it were always there — the way a writer sits down with a particular
genre's rules already internalized rather than re-reading them
mid-sentence.

All three invocation paths share the same `internal.Router` engine. See
[Auto-Selection (`route` tool)](#auto-selection-route-tool) for the full
scoring rules, anti-pattern handling, and hot-reload behavior.

### Hot reload

The engine monitors `mode/` and `persona/` directory mtimes. Edit a mode
file, save it, and the next `route` call picks it up — no server
restart, no rebuild, no deploy.

---

## Integration Patterns

The Reflex Engine is output-format-agnostic. The same `mpm route` call produces different output depending on how it's invoked:

| Invocation | Environment | Output format |
|---|---|---|
| Claude Code hook | Hook stdin/stdout | `<system-reminder>` block (plain text) |
| `mpm call route` (JSON-RPC) | OpenClaw / Hermes | `RoutingReport` JSON |
| `route` MCP tool | Any MCP client | MCP-protocol response |
| `mpm route` (CLI) | Shell pipeline | Plain text or empty |

### Hook-based integration

Any agent surface that supports a pre-prompt command hook can use `mpm route` to inject mode/persona context before the LLM generates. The hook runs synchronously; its stdout is injected into the LLM's context directly — no JSON envelope, no MCP overhead.

**Claude Code example** — add to `~/.claude/settings.json`:

```json
{
  "hooks": {
    "UserPromptSubmit": [
      {
        "hooks": [
          {
            "type": "command",
            "command": "mpm route",
            "timeout": 1,
            "statusMessage": "MPM routing…"
          }
        ]
      }
    ]
  }
}
```

Requirements:
- `mpm` must be on your `PATH` (run `which mpm` to verify). If it's not,
  the hook logs a non-blocking error and proceeds without routing.
- `MPM_ROUTE_WORKSPACE` env var (optional) — if unset, uses the current
  working directory as the MPM workspace base.

> **Note:** Claude Code hooks have a 10,000-character stdout limit. If a
> mode + persona combination would exceed the cap, the persona is
> truncated first with a marker (`[...truncated, see mode/<name>.md for
> full content]`). A single mode file exceeding 9,000 characters is
> truncated with a plain `[...truncated]` marker.

### Opt-out

- Type `/noroute` anywhere in the prompt → routing skipped for that turn
- Set `MPM_ROUTE=off` in shell env → routing skipped for the session
- A low-signal prompt (e.g. "hi") that matches no mode or persona
  produces no injected context — the LLM responds natively

### Verify it works

```bash
# Should print a <system-reminder> block
mpm route < "review this code for security issues"

# Should print nothing (no mode matched)
mpm route < "hi"

# Should print nothing (opt-out)
mpm route < "/noroute explain quantum computing"
```

---

## Architecture

```
┌─────────────────────────────────────────────────────────────┐
│                         mpm binary                          │
│  ┌─────────────┐   ┌─────────────┐   ┌─────────────────┐  │
│  │  CLI input  │──▶│   Router    │──▶│    Handler      │  │
│  └─────────────┘   └─────────────┘   └────────┬────────┘  │
│  ┌─────────────────────────────────────────────┼────────┐ │
│  │            Background subsystem              │        │ │
│  │              (none — all periodic work is   │        │ │
│  │               on-demand via mpm ops …)      │        │ │
│  │                    ▼                        │        │ │
│  │         ┌─────────────────────┐             │        │ │
│  │         │  Shared DBManager   │             │        │ │
│  │         │  (SQLite + WAL)     │             │        │ │
│  │         └──────────┬────────────┐             │        │ │
│  └────────────────────┼────────────────────────┘        │ │
│                        ▼                                  │ │
│              ┌─────────────────┐                         │ │
│              │    mpm.db       │  (unified database)     │ │
│              └─────────────────┘                         │ │
│                                                            │ │
│  ┌──────────────────────────────────────────────────┐    │
│  │              SSEBroker (stream.go)                  │    │
│  │  • Package-level singleton                         │    │
│  │  • 50-slot ring buffer (replay on reconnect)       │    │
│  │  • 15s heartbeat ticker                            │    │
│  │  • Fan-out to 256-buffered client channels         │    │
│  │  • /api/stream  +  /api/internal/broadcast         │    │
│  └──────────────────────────────────────────────────┘    │
└─────────────────────────────────────────────────────────────┘
```

- No socket IPC — commands execute in the same process
- No background goroutines — synthesis, GC, maintenance are all on-demand via `mpm call <tool>` or `mpm ops <subcommand>`
- Multi-vendor LLM failover: MiniMax → OpenAI → Ollama → error to caller (no DLQ — failures bubble up so the caller decides whether to retry)
- SSEBroker is a package-level singleton — callable from `main()` with no HTTP server handle; agent/CLI path relays via `POST /api/internal/broadcast`

### Database Schema

| Table | Purpose |
|---|---|
| `memories` | Core storage with FTS5 full-text search + embeddings |
| `memory_revisions` | Append-only version history for `--as-of` time-travel |
| `sessions` | Session metadata and transcripts |
| `topics` | Topic definitions |
| `topic_memberships` | Memory-to-topic links |
| `lessons` | Learned lessons (insight/warning/practice) |
| `reference_docs` | Reference document metadata |
| `reference_chunks` | Smart Fence chunks from ingested documents |
| `system_config` | Configuration snapshots, ephemeral personas |
| `raw_memories` | Staging area for ingest workflow |
| `evidence` | Evidence rows (with strength + decay) backing `confidence` |
| `confidence_history` | Append-only audit trail of every confidence recompute |
| `confidence_components` | Component breakdown (evidence strength, decay, prior) for `explain_confidence` |
| `memory_source_evidence_ai` | Per-source survival-rate trigger backing `query_memory_quality` |
| `session_handoffs` | End-of-session handoff records (summary, commitments, open_questions) |
| `audit_log` | Cross-session runtime anomaly ledger (`level`, `component`, `stack_trace`, `context`) |

---

## Release Notes

Every release is recorded in [`CHANGELOG.md`](CHANGELOG.md) and a structured [`changelog.json`](changelog.json) sibling. Both are generated by `mpm ops changelog build` from the git log; pass `--with-synthesis` to also join git-sourced entries with agent-sourced changelog memories into a single unified artifact.

Generate a new release:

```bash
mpm ops changelog build --since v1.0.0 --release-version 1.2.0 \
    --release-notes ./release-notes.md --legacy
```

Flags:
- `--since <ref>` — lower bound (tag, sha, or date). Default: latest tag.
- `--release-version <v>` — version string for the new release.
- `--release-notes <file>` — hand-written highlights (rendered as a blockquote at the top of the release).
- `--legacy` — emit pre-since commits as a single `## [N.legacy] - Legacy Backfill` section so no history is lost across versions.
- `--dry-run` — print to stdout, do not write files.
- `--with-synthesis` — join git-sourced entries with agent-written `#changelog` memories. Agent prose renders as a blockquote under each commit bullet; orphan memories (no matching git commit) surface in a dedicated `## [orphans]` section and a stderr warning.

The schema is the join contract: every `ChangelogEntry` carries a `commit_hash` (authoritative identity for git-sourced entries) and an `mpm_memory_ids` array (empty for git-sourced, populated by the `log_to_changelog` MCP tool for agent-sourced). The synthesis engine is a pure function over that join.

Agents self-report their work via the `log_to_changelog` MCP tool:

```json
{
  "fact": "Embedding is now a separate phase after the chunk-insert tx commits.",
  "commit_hash": "914ebf90665964d6f75a4dbc1847f13a83977820"
}
```

The tool requires a full 40-character SHA-1 (run `git rev-parse HEAD` to get the canonical form). Short hashes and refs are rejected at write time, keeping the join one-to-one: a memory exists iff the commit exists. See [CHANGELOG.md](CHANGELOG.md) for the full release history.

---

## Configuration


```json
{
  "aliases": {
    "s": "recall",
    "in": "add -i"
  },
  "memory_dirs": ["./memory"],
  "sessions_dirs": ["./sessions"],
  "external_dbs": [
    {
      "path": "~/.openclaw/memory/main.sqlite",
      "label": "openclaw",
      "interval_seconds": 30
    }
  ],
  "synth": {
    "model": "MiniMax-M2.7",
    "api_key": "***",
    "base_url": "https://api.minimax.io/anthropic",
    "max_tokens": 1024,
    "timeout_seconds": 300
  }
}
```

### Default File Locations

| Data | Path |
|---|---|
| Database | `~/.mpm/mpm.db` |
| Modes | `~/.mpm/mode/` |
| Personas | `~/.mpm/persona/` |
| Mirror log | `~/.mpm/mirror.jsonl` |
| Watchdog log | `~/.mpm/watchdog.jsonl` |

### Environment Variables

| Variable | Purpose |
|---|---|
| `MPM_WORKSPACE` | Override workspace directory |
| `MPM_FORCE` | Skip confirmation prompts |
| `MPM_INTERACTIVE` | Force interactive mode |
| `MPM_PORT` | Web server port (auto-detected if unset, written to `~/.mpm/web.port` for sibling processes) |
| `MPM_LOG` | Log level: `debug` / `info` / `warn` / `error` (default: `info`) |
| `MPM_LOG_FORMAT` | Log format: `text` / `json` (default: `text`) |
| `MPM_SHARED_DB` | Path to a shared SQLite database for cross-agent house rules (Phase 1 of multi-agent shared epistemology). If unset, mpm runs in local-only mode. See [WISHLIST.md](WISHLIST.md). |
| `MPM_SHARED_READONLY` | `1` to attach the shared DB read-only. Default: read/write. |
| `MPM_ACTIVE_MODE` | Active mode (MCP only — populates child stdio env from the `.mcp.json` `env` block) |
| `MPM_ACTIVE_PERSONA` | Active persona (MCP only — same population path) |
| `MINIMAX_API_KEY` | LLM API key (synthesis fallback) |
| `MINIMAX_BASE_URL` | LLM base URL (synthesis fallback) |

---

## Build & Install

```bash
# Build (requires CGO with FTS5)
cd /home/v/.openclaw/workspace/projects/mpm
make build    # Produces: bin/mpm

# Install binary
sudo cp bin/mpm /usr/local/bin/mpm

# Systemd service
sudo cp contrib/systemd/mpm.service /etc/systemd/system/
sudo systemctl daemon-reload
sudo systemctl enable mpm
sudo systemctl start mpm
```

> **Important:** Use `make build` (passes `CGO_CFLAGS="-DSQLITE_ENABLE_FTS5=1"`). Plain `go build` silently fails FTS5 indexing, breaking recall.

---

## Directory Structure

```
projects/mpm/
├── bin/mpm                          # Built binary
├── src/db/mpm.db                    # SQLite database (WAL mode)
├── mode/                            # Mode .md files (debugging, research, etc.)
├── persona/                         # Persona .md files
├── active.json                      # Active mode/persona state
├── agent-plugins/                 # All MPM agent integrations live here
│   └── hermes-mpm-plugin/          # Hermes Python agent plugin (CLI-bound; Hermes lacks native MCP)
│       └── install.md
└── contrib/systemd/
    └── mpm.service
```

---

## Roadmap

### Recently Shipped

- **Multi-Agent Shared Epistemology — Phase 1 (Plumbing)** — `MPM_SHARED_DB` env var, ATTACH on startup, graceful local-only fallback, `mpm ops shared status` command. Read tools (`query_global_rules`) and operator-gated writes (`record_global_rule`, `promote_to_global`) are tracked in [WISHLIST.md](WISHLIST.md) Phases 2-3.

### Upcoming

- **Native Event-Driven Hooks** — UNIX drop-in hooks for `on_theory_resolved`, `on_memory_synthesized`, etc.
- **Memory Encryption at Rest** — SQLCipher AES-256 for enterprise-grade at-rest encryption
- **Advanced Cognitive Analytics** — agent-level retention curves, confidence trajectory forecasting
- **Governance and Compliance Extensions** — redaction policies, retention windows, audit-export formats

---

## License

GNU AGPL v3

Created by v (human) + 808 (AI).
