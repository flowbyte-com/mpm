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

- **Not a daemon** — every command is a single binary invocation. The optional watcher runs as a background goroutine, not a separate process.
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

MPM tracks not just *what* it knows, but *why* it knows it, *how* it decided to act, and *what it believes but hasn't proven yet*. The Epistemology Engine extends the memory model into genuine agency — reasoning that can be examined, revised, and rendered obsolete.

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
```

This is the **machine-to-machine interface**. The human-facing CLI (documented below) calls the same handlers internally.

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

**LTM promotion:** `weight ≥ 10` OR explicit `mpm promote` OR auto-ingested `.md` file.

### Cognitive Immune System (Hybrid Search)

On hybrid search, a contradiction scan evaluates top-15 candidates (≤105 pairs) via cosine similarity. State collision (sim ≥ 0.85, one challenged) triggers an async challenge log to `mirror.jsonl`. Challenged memories surface with an in-memory warning prepended — **never written to DB**.

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
mpm web            # Start web UI server
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
mpm kb reference add <file> [--tag tags]
mpm kb reference list
mpm kb reference search <query>
mpm kb reference show <id>
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
mpm ops doctor [--explain]          # Diagnostics (--explain shows FTS5 query plan)
mpm ops maintain                     # Self-maintenance: decay, consolidate, prune
mpm ops synthesize [--dry-run]       # LLM synthesis on all memories
mpm ops gc [--dry-run|--review|--purge|--shred-negative]  # Decay sweep
mpm ops backfill-embeddings [--batch-size|--collection|--dry-run]  # Embedding pipeline
mpm ops dlq:review [review|clear|retry]  # Dead letter queue — failed synth events

# Watcher
mpm ops watch start [--bg]|stop|status   # Watcher daemon (goroutine-based)

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

- Auto-embed on `mpm add` and all watcher ingest paths
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

### Watcher / Synth Isolation + DLQ

Watcher and synthesis run on isolated SQLite read connections (WAL readers are thread-safe). Synthesis events pass through a typed channel with a **200-event buffer**. **Overflow backpressure:** events beyond capacity are offloaded to `raw_memories` as `overflow_deferred`, processed alongside normal DLQ entries with shared exponential backoff. Failed synthesis events route to `synthesis_dlq` — **never dropped, never blocking**.

Multi-vendor fallback chain: **MiniMax → OpenAI → Ollama (local)**. Each vendor has independent timeout (10s). All vendors fail → DLQ enqueue. Periodic retry tick processes DLQ when vendors recover.

```bash
mpm ops dlq:review review    # Inspect DLQ
mpm ops dlq:review retry     # Process pending retries
mpm ops dlq:review clear    # Clear resolved DLQ entries
```

### Deadlock Observability

`DatabaseManager` watchdog writes to `watchdog.jsonl` (separate from `mirror.jsonl`). Slow query threshold: 100ms. Exponential backoff on lock contention. `ExecTracked`, `QueryTracked`, `QueryRowTracked` methods log all DB operations.

### Automatic Decay & Archive

Every 5 minutes: LTM memories decay exponentially via `MAX(weight - MAX(1, CAST(weight × DecayRate AS INTEGER)), 1)`. Weight=1 memories past 30-day archive threshold are soft-deleted, terminal state captured in `memory_revisions` for `--as-of` time-travel.

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

PDF, EPUB, HTML, Markdown ingestion with Smart Fence chunking. `--chunk-size` flag (64–2048 tokens, default 512) via tiktoken batch encoding. Source tracking with `[Source: ...]` inline chips. Cross-reference linking on recall.

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

**Storage:** Directives live in the SQLite database as memories with `is_prime_directive = 1`. Any memory can be elevated to directive status via the `is_prime_directive` flag. Unlike mode/persona files which are plain markdown, directives are database-persisted — enabling synthesis, reinforcement, and challenge workflows.

**Access:**

```bash
mpm ops directives          # CLI — reads from ~/.mpm/src/db/mpm.db
mpm call read_directives    # MCP tool — reads from MPM_WORKSPACE/src/db/mpm.db
```

**Important path note:** `mpm ops directives` and the MCP `read_directives` tool read from different databases. The CLI reads the canonical `~/.mpm/` install. The MCP tool reads from `MPM_WORKSPACE` (defaults to current directory). When OpenClaw runs MPM with `MPM_WORKSPACE=~/.mpm`, both paths converge on the same database.

**Elevation:** Any memory can become a directive:

```bash
mpm call save_to_memory --payload '{"fact": "Always verify before acting", "collection": "directives", "is_prime_directive": true}'
```

Directives with `is_prime_directive=1` are surfaced by `read_directives` and injected into the system prompt context on every bootstrap. The `proactive_recall_hint` engine also elevates directive-adjacent memories when the current conversation context matches their semantic territory.

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
  "selected_modes": ["architect", "research", "write"],
  "selected_persona": "whiterabbit",
  "scores": {
    "write":     { "score": 4, "triggers": ["draft", "whitepaper"] },
    "architect": { "score": 1, "triggers": ["architecture"] },
    "research":  { "score": 1, "triggers": ["need"] }
  }
}
```

**Scoring rules:**

| | Modes | Personas |
|---|---|---|
| Selection logic | Threshold filter — any score ≥1 activates (multi-select) | Max-pooling — highest score wins if ≥1 (single-select) |
| Explicit frontmatter `patterns:` | +2 per match | +2 per match |
| Body-text implicit patterns | +1 per match | +1 per match |
| Frontmatter `anti_patterns:` | -1 per match | -1 per match |

**Pattern sources** (in order of weight):
1. **`patterns:` field** in YAML frontmatter — explicit author intent, highest weight
2. **Body text** — non-stop-word content words extracted at load time, lower weight
3. **`anti_patterns:` field** — penalizes false positives (e.g., "quick and dirty" suppresses "write" mode triggered by the word "write")

**Hot reload:** The router monitors `mode/` and `persona/` directory mtimes. Any file added or edited is re-parsed and regex recompiled automatically on the next `route` call — no server restart needed.

**Calling systems** (OpenClaw, Claude Code, etc.) receive the JSON and inject `MPM_ACTIVE_MODE` / `MPM_ACTIVE_PERSONA` into the environment before the main generation call. The routing is completely stateless — no file locking, no write collisions.

### Security Scanning

Content scanned against **20 regex patterns** (API keys, JWTs, SSH keys, connection strings, password patterns) before any database write. Blocked content goes to `mirror.jsonl` but **never reaches the database**.

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

The goal is predictable behavior under sustained workloads.

---

## Use Cases

- **Software Engineering Agents** — preserve debugging investigations, architectural decisions, and implementation rationale.
- **Research Agents** — track hypotheses, evidence, and conclusions across long-running investigations.
- **Enterprise Knowledge Retention** — capture institutional reasoning that would otherwise disappear into chat logs.
- **Multi-Agent Systems** — provide a shared cognitive substrate across multiple autonomous agents.
- **Autonomous Operations** — maintain continuity across long-running workflows.

---

## Agent Integration

MPM integrates directly with AI agents. Both OpenClaw and Hermes share the same Go backend — identical behavior, identical database, the same `mpm call` JSON boundary. The Hermes plugin uses Python `subprocess`; the OpenClaw plugin uses Node `child_process`.

### OpenClaw

MPM is installed as an OpenClaw plugin (`openclaw/mpm-plugin/`), giving the agent **native function-calling access** to 19 MPM tools. The OpenClaw plugin calls the Go binary directly via `child_process` — no MCP intermediary.

```
read_wake_context        query_long_term_memory    save_to_memory
challenge_memory         save_lesson               search_lessons
list_lessons             create_topic              search_topics
link_topic               add_reference             search_references
list_references          read_directives           propose_theory
resolve_theory           record_decision           route
proactive_recall_hint
```

Setup and config: see [`openclaw/OPENCLAW.md`](openclaw/OPENCLAW.md).

### Hermes

MPM is also installed as a Hermes Agent Python plugin (`hermes-mpm-plugin/`). Setup: see [`hermes-mpm-plugin/install.md`](hermes-mpm-plugin/install.md).

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
│  │  ┌──────────────┐  ┌───────────────────┐    │        │ │
│  │  │  WorkerPool  │  │  fsnotify watcher │    │        │ │
│  │  │  (3 goros)   │  │  (goroutine)      │    │        │ │
│  │  └──────┬───────┘  └─────────┬─────────┘    │        │ │
│  │         └──────────┬─────────┘              │        │ │
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
- Watcher = background goroutine (not separate process); `--bg` spawns detached child for systemd
- Synthesis = isolated goroutine with independent panic recovery and deadline propagation
- DLQ = SQLite table; failed synth events retried every 5min via ticker
- Multi-vendor failover: MiniMax → OpenAI → Ollama → DLQ
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
| `synthesis_dlq` | Dead-letter queue for failed synthesis jobs |
| `synthesis_raw` | Overflow deferred entries pending retry |

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
├── docs/
│   ├── OPENCLAW.md                  # OpenClaw plugin integration guide
│   ├── MPM_WISHLIST.md              # Roadmap / wishlist
│   ├── archive_v1/                  # Historical specs and designs (reference)
│   │   ├── epistemology-engine-spec.md
│   │   ├── MPM_PRUNING.md
│   │   ├── MPM_PROACTIVE_REVIEW_HOOK.md
│   │   ├── MPM_SYNTHESIS_DEDUP.md
│   │   ├── BREAK_90.md              # Feature scoring + roadmap
│   │   └── ...
│   └── superpowers/                 # Deep-dive specs
├── hermes-mpm-plugin/              # Hermes Python agent plugin
│   └── install.md
├── openclaw/                        # OpenClaw plugin
│   ├── mpm-plugin/
│   └── OPENCLAW.md
└── contrib/systemd/
    └── mpm.service
```

---

## Roadmap / Wishlist

See [`docs/MPM_WISHLIST.md`](docs/MPM_WISHLIST.md) for the full running wishlist. Notable upcoming items:

- **Concept Drift Detection** — detect when older reinforced memories are being challenged by new patterns, auto-flag epoch shifts
- **Multi-Agent Shared Epistemology** — SQLite `ATTACH DATABASE` for shared global rules across agents
- **Native Event-Driven Hooks** — UNIX drop-in hooks for `on_theory_resolved`, `on_memory_synthesized`, etc.
- **Memory Encryption at Rest** — SQLCipher AES-256 for enterprise-grade at-rest encryption
- **Advanced Cognitive Analytics**
- **Governance and Compliance Extensions**

---

## License

GNU AGPL v3

Created by v (human) + 808 (AI).
