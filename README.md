# MPM — Memory Persistence Module

> **⚡ MPM mpm mpm-std** — SQLite-native agent state management for AI agents.

MPM is a single binary that provides long-term memory, behavioral modes, persona management, and an epistemology engine — everything stored in one SQLite database with FTS5 full-text search. Zero external services.

MPM is the memory and reasoning layer for AI agents (OpenClaw + Hermes). It tracks not just *what* the agent knows, but *why* it decided to act and *what it believes but hasn't proven yet*.

---

## What MPM Is NOT

- **Not a daemon** — every command is a single binary invocation. The optional watcher runs as a background goroutine, not a separate process.
- **Not generic storage** — built for AI agent cognition: weighted recall, decay, epistemology, proactive hints.
- **Not a human dashboard** — machine-to-machine interface is primary; CLI is a convenience layer.

---

## Core Concepts

### The JSON Boundary (`mpm call`)

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
mpm +<id>    # Reinforce (+1, auto-clears challenge if contested)
mpm -<id>    # Weaken (-1, floor at 1)
mpm snooze <id>   # Bump relevance without LTM promotion
mpm promote <id>  # Elevate to LTM (weight=10)
mpm ops gc --shred-negative  # Shred memories with weight<0 AND proven theory exists
```

### Provenance & ISR Telemetry

Every memory stamps its origin at write time:
- `client` — `mpm_cli` or `mpm_call`
- `mpm_mode` — `native` or `watch`
- `mpm_persona` — `operator` or `watch-daemon`

`mpm ops stats` surfaces a nested Client → Model → Persona matrix with Idea Survival Rate (ISR %) per cell — the percentage of memories with weight > 1 (not decayed to floor).

---

## Epistemology Engine

MPM tracks not just *what* it knows, but *why* it knows it, *how* it decided to act, and *what it believes but hasn't proven yet*. The Epistemology Engine extends the memory model into genuine agency — reasoning that can be examined, revised, and rendered obsolete.

**The core problem it solves:** AI agents retrieve facts but lose the chain of reasoning behind them. Weeks later, 808 might redo work it already discarded, re-evaluate a decision that was already made, or miss that a hypothesis it formed was already tested and resolved. The Epistemology Engine makes reasoning explicit and persistent.

### Decision Ledger

An append-only audit trail of architectural choices. Captures the context, the choice made, and the reasoning — so weeks later, 808 can reconstruct *why* a particular approach was taken instead of blindly second-guessing itself.

```bash
mpm record_decision "CONTEXT: We needed a CSS injection mechanism that survives wp_kses filtering
CHOICE: Route all widget CSS through agentshell_register_widget → wp_options → widgets.php <head> injection
RATIONALE: WordPress strips <style> blocks from post content via wp_kses_post() even for admins. The widget init JS also needs a footer injection point. Both requirements pointed to wp_options as the store."

mpm decisions                   # Formatted decision ledger
```

### Theory Tracker

A hypothesis ledger for debugging and design. When 808 forms a causal assumption ("I think X is causing Y"), it logs the hypothesis and a concrete validation test before writing the fix. This forces the assumption to be testable, and often collapses a false hypothesis before it wastes an hour.

```bash
mpm propose_theory "HYPOTHESIS: passing --json before the positional arg causes the parse bug
VALIDATION_CRITERIA: write a unit test — invoke mpm with --json flag first vs positional-first, compare parse error rate
STATUS: pending"

mpm theories                    # List all theories with status chips
mpm theories pending           # Filter to pending only

mpm resolve_theory abc123 "confirmed: flag order matters, --json consumed before positional processing"
```

### Proactive Recall Hints

During a conversation, 808 can surface relevant decisions and theories before you know you need them. FTS5 keyword extraction detects semantic overlap with your current context and pushes a low-latency recall hint — with STATUS and RATIONALE displayed directly, not just the content.

```bash
mpm kb hint "discussing the CSS injection approach for the widget system"
# → 💡 [Recall] You decided: Route all widget CSS through agentshell_register_widget...
#    RATIONALE: WordPress strips <style> blocks from post content...

mpm kb hint "token budget handling in the CLI"
# → 💡 [Recall] Hypothesis: passing --json before the positional arg...
#    STATUS: resolved | CONCLUSION: confirmed...
```

The `proactive_recall_hint` plugin tool is wired into the OpenClaw agent loop — 808 calls it after context shifts and surfaces the most relevant epistemology memory automatically.

### Challenge Lifecycle (Autonomous Pruning)

The challenge system is a closed-loop immune response for memory integrity — the mechanism by which 808 actively questions and overturns its own outdated knowledge:

```
challenge ──────────────────────────────────────────────────────▶ [pending theory]
    │
    ├── challenge restore ──────────────────────────▶ [theory: disproven]
    │
    └── shred ──────────────────────────────────────▶ [theory: deleted]
```

**`mpm challenge <id> "<evidence>"`** — Atomic transaction:
1. Patch memory metadata: `{"status":"challenged","challenged_theory_id":"<theory_id>"}`
2. Create theory with back-link: `{"status":"pending","type":"challenge","memory_id":"<memory_id>"}`
3. Weaken memory weight by 3

**`mpm challenge restore <id>`** — Atomic transaction:
1. Resolve theory: `status → disproven`, clear `memory_id`
2. Clear memory metadata flags (RFC 7396 JSON patch)

**`mpm shred <id>`** — Atomic transaction:
1. DELETE topic_memberships WHERE memory_id = ?
2. DELETE theory (if exists)
3. DELETE memory

**`mpm ops gc --shred-negative`** — Shreds only memories with weight<0 AND a proven theory exists. Negative weight alone is never sufficient — the theory provides the evidence chain.

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

## Full Feature Reference

### Hybrid Semantic Search

BM25 full-text search combined with cosine similarity from `nomic-embed-text` embeddings (768d). BM25 unbounded scores are sigmoid-normalized. Use `--semantic` flag to enable pure embedding search.

```bash
mpm recall --semantic "为什么德国队表现这么好"
```

### Embedding Pipeline

- Auto-embed on `mpm add` and all watcher ingest paths
- `mpm ops backfill-embeddings` — batched, resume-safe backfill for existing memories
- tiktoken (cl100k_base) for token-aware chunking

### Cognitive Immune System

On hybrid search, contradiction scan evaluates top-15 candidates (≤105 pairs) via cosine similarity. State collision (sim ≥ 0.85, one challenged) triggers async challenge log to `mirror.jsonl`. Challenged memories surface with in-memory warning prepended — **never written to DB**.

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

### Challenge Lifecycle

See **Epistemology Engine → Challenge Lifecycle** for the full explanation, atomic transactions, and workflow diagram.

### Proactive Hint Engine

FTS5-triggered recall hints via `mpm hint` and `proactive_recall_hint` plugin tool. Extracts conversation keywords (tiktoken, stopword filter), finds epistemology overlaps via BM25, surfaces STATUS and RATIONALE inline — not just the content.

Quality rules: one hint per turn, score >= -3.0, 10-turn suppression window, FTS5 keyword overlap detection.

```bash
mpm kb hint "discussing the CSS injection approach"
# → 💡 [Recall] You decided: Route all widget CSS through...
#    RATIONALE: WordPress strips <style> blocks from post content...
```

### Synthesis Deduplication

Context-aware deduplication: synthesis deletes the triggering memory after LTM save, preserves oldest `created_at`, transfers topic_memberships, excludes epistemology collections, quality gate requires ≥2 candidates.

### Reference Library

PDF, EPUB, HTML, Markdown ingestion with Smart Fence chunking. `--chunk-size` flag (64–2048 tokens, default 512) via tiktoken batch encoding. Source tracking with `[Source: ...]` inline chips. Cross-reference linking on recall.

### Session Memory Context (`wake`)

`mpm wake` surfaces the last session's mode, persona, topics, and recent memories — the agent's bootstrap context on startup.

### XITL Stance Hot-Swap

`mpm ops stance assume <mode> <persona> <rationale>` switches mode/persona at runtime with no restart. The new directive prints to stdout — OpenClaw captures it and injects into session chat history, so the agent reads and adopts it on the very next turn.

`mpm ops stance synthesize <name>` generates a JIT ephemeral persona from a prompt, stored in `system_config`. `mpm ops stance promote` flushes it to a permanent `.md` disk file.

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

## OpenClaw Agent Integration

MPM is installed as an OpenClaw plugin (`openclaw/mpm-plugin/`), giving the agent **native function-calling access** to 19 MPM tools. The OpenClaw plugin calls the Go binary directly via `child_process` — no MCP intermediary.

**Available tools:**

```
query_long_term_memory   save_to_memory            save_lesson
search_lessons           list_lessons              create_topic
search_topics            link_topic                add_reference
search_references        list_references           read_directives
read_wake_context        record_decision           propose_theory
resolve_theory           proactive_recall_hint     challenge_memory
```

Setup and config: see [`openclaw/OPENCLAW.md`](openclaw/OPENCLAW.md).

---

## Hermes Agent Integration

MPM is also installed as a Hermes Agent Python plugin (`hermes-mpm-plugin/`). Both agents use the **same Go backend** — identical behavior, identical database, same `mpm call` JSON boundary. The Hermes plugin uses Python `subprocess` for the JSON boundary; the OpenClaw plugin uses Node `child_process`.

Setup: see [`hermes-mpm-plugin/install.md`](hermes-mpm-plugin/install.md).

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
│  │         └──────────┬────────────┘             │        │ │
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

---

## License

GNU AGPL v3. Part of the OpenClaw agent ecosystem.

Created by v (human) + 808 (AI).
