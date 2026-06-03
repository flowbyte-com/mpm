# MPM — Memory Persistence Module

SQLite-native agent state management for AI agents. Single binary, zero external dependencies.

MPM provides long-term memory, behavioral modes, and persona management. Everything is stored in a single SQLite database with FTS5 full-text search — no server, no socket IPC, no complexity.

---

## Overview

### Key Features

- **Unified SQLite database** with WAL mode for concurrent reads/writes
- **Full-text search** via SQLite FTS5 — no external search service
- **Inline importance chips** on recall results — reinforcement count, weight, LTM flag, last-accessed age, score (0–1)
- **Auto-elevation on access** — frequently recalled memories grow stronger over time via per-session deduplication
- **Weight system with feedback shortcuts** — `mpm +<id>` reinforces (+1, auto-clears challenge if contested); `mpm -<id>` weakens (-1, floor at 1 via SQL MAX). Flag collision guard protects `-v`, `-h`.
- **Cognitive immune system** — on hybrid search, contradiction scan evaluates top-15 candidates (≤105 pairs) via cosine similarity; state collision (sim ≥ 0.85, one challenged) triggers async challenge log to `mirror.jsonl`; challenged memories surface with in-memory warning prepended — never written to DB.
- **Ingestion overflow backpressure** — synthesis worker channel has 200-event buffer; overflow events beyond capacity are offloaded to `raw_memories` as `overflow_deferred` status, processed alongside normal DLQ entries with shared exponential backoff.
- **Automatic decay & archival (heartbeat-wired)** — every 5 minutes on isolated DB connection: LTM memories decay exponentially via `MAX(weight - MAX(1, CAST(weight × DecayRate AS INTEGER)), 1)`; weight=1 memories past 30-day archive threshold are soft-deleted, terminal state captured in `memory_revisions` for `--as-of` time-travel.
- **Spaced reinforcement review** — `mpm review --promoted` shows recently elevated; `mpm review --stale` surfaces forgotten LTM memories
- **Topic auto-suggestion** — on save, system suggests linking to semantically related existing topics
- **Stale memory flagging** — memories not accessed in N days flagged inline in recall results
- **Epistemological pruning loop** — `mpm challenge <id>` flags a memory as challenged, creates a linked pending theory, and injects a warning into LLM recall; `mpm challenge restore <id>` resolves the theory and clears the flag; `mpm shred <id>` cascade-deletes both memory and theory. Fully closed loop, zero manual cleanup.
- **Reference library** — PDF, EPUB, HTML, Markdown ingestion with Smart Fence chunking
- **Reference chunking control** — `--chunk-size` flag (64–2048 tokens, default 512) via tiktoken batch encoding
- **Cross-reference linking** — bounded bidirectional Memory↔Topic↔Reference cross-refs on recall and topic show
- **Session memory context** — `mpm wake` surfaces last session's mode, persona, topics, and recent memories
- **Security scanning** — 20 regex patterns for API keys, tokens, secrets
- **XITL Routing Engine** — `assume_stance`, `synthesize_stance`, `mpm ops promote` — hot-swap or generate personas with automatic mode-driven retrieval depth
- **Mode-Driven Context Retrieval** — each mode specifies `retrieval_limit` and `retrieval_threshold` controlling how many FTS5 chunks the proactive hint engine pulls. Debugging mode: tunnel vision (3 chunks, strict threshold). Research mode: wide gather (20 chunks, loose threshold). The LLM sees its own retrieval parameters as operating instructions.
- **Invisible CLI** — `cat idea.md | mpm` pipes stdin to add; bare `mpm token budget` defaults to recall; 7-command daily surface with everything else under `mpm ops`
- **Proactive recall hints** — `mpm hint "conversation text"` and the `proactive_recall_hint` plugin tool surface relevant decisions and theories during conversation based on FTS5 keyword overlap

### What MPM Is NOT

MPM is **not** a daemon. Every command (`mpm add`, `mpm recall`, `mpm watch start`) is a single binary invocation. The watcher runs as a background goroutine within the same process, or as a detached child with `--bg` for systemd integration.

---

## Epistemology Engine

MPM tracks not just *what* it knows, but *why* it knows it, *how* it decided to act, and *what it believes but hasn't proven yet*. The Epistemology Engine extends the memory model into genuine agency — reasoning that can be examined, revised, and rendered obsolete.

The core problem it solves: AI agents retrieve facts but lose the chain of reasoning behind them. Weeks later, 808 might redo work it already discarded, re-evaluate a decision that was already made, or miss that a hypothesis it formed was already tested and resolved. The Epistemology Engine makes reasoning explicit and persistent.

### Decision Ledger (`mpm record_decision`)

An append-only audit trail of architectural choices. Captures the context, the choice made, and the reasoning — so weeks later, 808 can reconstruct *why* a particular approach was taken instead of blindly second-guessing itself.

```bash
mpm record_decision "CONTEXT: We needed a CSS injection mechanism that survives wp_kses filtering
CHOICE: Route all widget CSS through agentshell_register_widget → wp_options → widgets.php <head> injection
RATIONALE: WordPress strips <style> blocks from post content via wp_kses_post() even for admins. The widget init JS also needs a footer injection point. Both requirements pointed to wp_options as the store."

mpm decisions                   # Formatted decision ledger
```

### Theory Tracker (`mpm propose_theory` / `mpm resolve_theory`)

A hypothesis ledger for debugging and design. When 808 forms a causal assumption ("I think X is causing Y"), it logs the hypothesis and a concrete validation test before writing the fix. This forces the assumption to be testable, and often collapses a false hypothesis before it wastes an hour.

```bash
mpm propose_theory "HYPOTHESIS: passing --json before the positional arg causes the parse bug
VALIDATION_CRITERIA: write a unit test — invoke mpm with --json flag first vs positional-first, compare parse error rate
STATUS: pending"

mpm theories                    # List all theories with status chips
mpm theories pending           # Filter to pending only

mpm resolve_theory abc123 "confirmed: flag order matters, --json consumed before positional processing"
```

### Proactive Recall Hints (`mpm hint`)

During a conversation, 808 can surface relevant decisions and theories before you know you need them. FTS5 keyword extraction detects semantic overlap with your current context and pushes a low-latency recall hint — with STATUS and RATIONALE displayed directly, not just the content.

```bash
mpm hint "discussing the CSS injection approach for the widget system"
# → 💡 [Recall] You decided: Route all widget CSS through agentshell_register_widget...
#    RATIONALE: WordPress strips <style> blocks from post content...

mpm hint "token budget handling in the CLI"
# → 💡 [Recall] Hypothesis: passing --json before the positional arg...
#    STATUS: resolved | CONCLUSION: confirmed...
```

The `proactive_recall_hint` plugin tool is wired into the OpenClaw agent loop — 808 calls it after context shifts and surfaces the most relevant epistemology memory automatically.

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

### Challenge Lifecycle (v1.2+)

The challenge system is a closed-loop immune response for memory integrity:

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
2. Clear memory metadata: `status + challenged_theory_id` set to `null` (key removal, RFC 7396)

**`mpm shred <id>`** — Atomic transaction:
1. DELETE topic_memberships WHERE memory_id = ?
2. DELETE theory (if exists)
3. DELETE memory

**Recall warning:** Challenged memories surface with `[Note: This memory is challenged — treat as unverified]` prepended to LLM content. Human output shows `[CHALLENGED]` chip.

### Epistemology CLI Commands

| Command | Purpose |
| --- | --- |
| `mpm record_decision <text>` | Log a decision with context, choice, rationale |
| `mpm decisions` | Formatted decision ledger |
| `mpm propose_theory <text>` | Record a hypothesis with validation criteria |
| `mpm theories [pending\|resolved\|all]` | List theories with status chips |
| `mpm resolve_theory <id> <conclusion>` | Mark theory resolved, bump weight |
| `mpm challenge <id> "<evidence>"` | Flag memory as challenged, create linked pending theory (atomic tx) |
| `mpm challenge restore <id>` | Resolve linked theory (status → disproven), clear challenged flag (atomic tx) |
| `mpm shred <id>` | Cascade-delete memory and linked theory (atomic tx) |
| `mpm hint <text> [--max <n>] [--json]` | Proactive recall from conversation context |

Topics `theories` and `decisions` are auto-created on first use. Memories in these collections are auto-linked to their topic via `topic_memberships`. The watcher auto-detects `HYPOTHESIS:` and `CHOICE:` prefixes in `.md` files and routes them to the correct collection.

---

## Architecture

### Single-Process, Shared-Database Model

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
│  │         │  Shared DBManager    │             │        │ │
│  │         │  (SQLite + WAL)      │             │        │ │
│  │         └──────────┬────────────┘             │        │ │
│  └────────────────────┼────────────────────────┘        │ │
│                        ▼                                  │ │
│              ┌─────────────────┐                         │ │
│              │   mpm.db         │  (unified database)    │ │
│              └─────────────────┘                         │ │
└─────────────────────────────────────────────────────────────┘
```

**Key points:**

1. **No socket IPC** — Commands execute in the same process
2. **No separate watcher process** — `mpm watch start` runs a background goroutine; `--bg` spawns a detached child for systemd
3. **Unified WAL pool** — All access through one `DatabaseManager` with SQLite WAL mode
4. **Worker pool** — Fixed 3-goroutine pool processes watcher events concurrently
5. **PID file** — When detached, watcher writes PID to `watch.pid` for `stop`/`status` commands

### Memory Model

| Collection | Purpose | Default Weight | Notes |
| --- | --- | --- | --- |
| `memories` | General facts, LLM-synthesized insights | 1 | Tagged for auto-topic clustering |
| `session` | Operational facts (CWD, model changes) | 1 | State-change dedup, 24h TTL |
| `decisions` | Architectural choices with rationale | 1 | Append-only audit trail |
| `theories` | Hypothesis + validation criteria | 1 | Has lifecycle: pending → proven/disproven |

**Long-term memory (LTM):** any memory with `weight >= 10`. Promoted by `mpm promote <id>` or auto-ingested `.md` files from the watcher.

**Relevance scoring:** `score = (reinforcement_count × 2) + (weight × 1.5) + recency_bonus`

### Database Schema

| Table | Purpose |
| --- | --- |
| `memories` | Core storage with FTS5-triggered embedding |
| `memory_revisions` | Append-only version history for time-travel (`--as-of`) and archival audit |
| `sessions` | Session metadata and transcripts |
| `topics` | Topic definitions |
| `topic_memberships` | Memory-to-topic links |
| `lessons` | Learned lessons (insight/warning/practice) |
| `reference_docs` | Reference document metadata |
| `reference_chunks` | Smart Fence chunks from ingested documents |
| `system_config` | Configuration snapshots (hash-verified) |
| `external_db_cursors` | Sync cursors for external DB polling |
| `raw_memories` | Staging area for ingest workflow |
| `synthesis_dlq` | Dead-letter queue for failed synthesis jobs |
| `synthesis_raw` | Overflow deferred entries pending retry |

### Schema Registry (Adapter Pattern)

MPM uses a **Schema Registry** with the Adapter pattern to support multiple external database schemas.

```
┌──────────────────────────────────────────────────────────────────┐
│                      AdapterRegistry                              │
│  ┌─────────────────┐  ┌─────────────────┐  ┌─────────────────┐ │
│  │ OpenClawAdapter │  │   MPMAdapter    │  │ FutureAdapter   │ │
│  │   (chunks)      │  │   (memories)    │  │  (obsidian...)  │ │
│  └────────┬────────┘  └────────┬────────┘  └────────┬────────┘ │
└───────────┼────────────────────┼────────────────────┼───────────┘
            ▼                    ▼                    ▼
┌──────────────────────────────────────────────────────────────────┐
│  SchemaAdapter Interface                                          │
│  ├── Name() string              → "openclaw", "mpm", etc.       │
│  ├── Detect(*sql.DB) bool      → Does this DB match?            │
│  └── FetchNew(*sql.DB, cursor) ([]Memory, string, error)          │
└──────────────────────────────────────────────────────────────────┘
```

**Adding a new adapter:** create struct implementing `SchemaAdapter`, register in `NewAdapterRegistry()`.

### Security

Content is scanned against **20 regex patterns** before any database write — API keys, JWTs, SSH keys, connection strings, password patterns. Blocked content goes to `mirror.jsonl` but never reaches the database.

---

## Modes & Personas (File-Based)

Modes and personas are `.md` files with YAML frontmatter. **No database, no compile step.** Filename is the identity.

```
mode/                    # mode/*.md files
persona/                 # persona/*.md files
active.json              # active mode/persona state (only JSON needed)
```

**Format:**

```markdown
---
name: programming
title: Programming Mode
version: 1.0
status: active
purpose: Systematic, precise, architectural
description: Write clean, type-safe code
---

# Programming Mode

## Purpose
Systematic, precise, architectural. Thinks in code structures and abstraction boundaries.

## Patterns
- Always check types before suggesting implementations
- Prefer pure functions over stateful logic

## Anti-Patterns
- Premature abstraction
- Silent error handling
```

**Mode-Driven Retrieval Parameters**

Each mode file specifies two YAML fields that govern the proactive recall engine:

| Field | Type | Purpose |
|---|---|---|
| `retrieval_limit` | int | Max FTS5 chunks to pull per hint query |
| `retrieval_threshold` | float64 | Min BM25 score to be considered relevant (lower = stricter) |

Defaults: `retrieval_limit: 5`, `retrieval_threshold: -1.0` when mode lacks the fields.

| Mode | limit | threshold | Behavior |
|---|---|---|---|
| `debugging` | 3 | -2.5 | Tunnel vision — only the most highly weighted, directly relevant facts |
| `research` | 20 | -1.0 | Wide gather — build the full picture from scattered fragments |
| `architect` | 10 | -1.5 | Structural depth — trades breadth for clarity of component edges |
| `programming` | 5 | -2.0 | Precision — clean, minimal, high-signal only |
| `standard` | 7 | -1.5 | Balanced — neither turbocharged nor constrained |

These parameters apply only to the **proactive hint engine** and are exposed to the LLM in `GetSystemPrompt()` as operating instructions. Explicit `mpm recall` is **untouched** — human commands always override.

JIT ephemeral personas can set custom `retrieval_limit` in their JSON blob. A 50-chunk window for a massive codebase migration is valid — the Go backend respects whatever limit the ephemeral persona specifies.

**Active state** (`active.json`):
```json
{
  "persona": "auto",
  "modes": ["auto"],
  "updated": "2026-05-20T11:00:00Z"
}
```

Setting `mode` or `persona` to `"auto"` enables the XITL self-routing engine. When auto is active, 808 can invoke `assume_stance` to hot-swap an existing persona, or `synthesize_stance` to generate a JIT ephemeral persona stored in `system_config`. The new directive is printed to stdout — OpenClaw captures it and injects into the session chat history, so 808 reads and adopts it on the very next turn. No restart, no polling, no core changes. Only `mpm ops promote` writes a new `.md` file to disk — no disk bloat from experimental personas.

---

## OpenClaw Plugin Integration

MPM ships as an OpenClaw plugin, giving any OpenClaw agent native function-calling access to the MPM memory layer. The plugin exposes 19 tools:

| Tool | Purpose |
| --- | --- |
| `query_long_term_memory` | Semantic recall across all collections |
| `save_to_memory` | Persist facts, lessons, decisions |
| `save_lesson` | Record learned patterns (warning/practice/insight) |
| `search_lessons` | Search lesson store |
| `list_lessons` | List all lessons |
| `create_topic` | Create topic definitions |
| `search_topics` | Search topics |
| `link_topic` | Associate memory with topic |
| `add_reference` | Ingest documents to reference library |
| `search_references` | Search reference chunks |
| `list_references` | List reference documents |
| `read_directives` | Read prime directives |
| `read_wake_context` | Read last session's context (mode, persona, topics, memories) |
| `record_decision` | Log architectural choices with rationale |
| `propose_theory` | Log hypothesis before writing fix |
| `resolve_theory` | Mark theory proven/disproven, patch metadata in-place |
| `proactive_recall_hint` | Surface relevant decisions/theories from conversation context |
| `challenge_memory` | Challenge a memory — atomic transactional flag with linked theory; auto-resolves on restore; cascade-deletes on shred |

See [`docs/OPENCLAW.md`](docs/OPENCLAW.md) for the full integration guide including plugin setup, config, verification, and troubleshooting.

---

## Installation

### Build from Source

**Requirements:**
- Go 1.18+
- C compiler (for `mattn/go-sqlite3`)
- SQLite with FTS5 support

**Build:**
```bash
cd /home/v/workspace/projects/mpm
make build
```

**Install:**
```bash
sudo cp bin/mpm /usr/local/bin/mpm
```

> **Important:** Use `make build` (passes `CGO_CFLAGS="-DSQLITE_ENABLE_FTS5=1"`). Plain `go build` silently fails FTS5 indexing, breaking `mpm recall`.

### Systemd Service

```bash
sudo cp /home/v/workspace/projects/mpm/contrib/systemd/mpm.service /etc/systemd/system/
sudo systemctl daemon-reload
sudo systemctl enable mpm
sudo systemctl start mpm
systemctl status mpm
journalctl -u mpm -f
```

The service runs `mpm watch start` (goroutine-based watcher, not detached). Systemd manages lifecycle — no `--bg` needed.

---

## Configuration

MPM reads `mpm_config.json` from the workspace root.

**Example:**
```json
{
  "aliases": {
    "s": "recall",
    "in": "add -i",
    "mem": "recall --collection memories"
  },
  "memory_dirs": ["/home/user/.openclaw/workspace/memory"],
  "sessions_dirs": ["/home/user/.openclaw/agents/main/sessions"],
  "external_dbs": [
    {
      "path": "/home/user/.openclaw/memory/main.sqlite",
      "label": "openclaw",
      "interval_seconds": 30
    }
  ],
  "web_token": "your-secret-token",
  "synth": {
    "model": "MiniMax-M2.7",
    "api_key": "YOUR_API_KEY",
    "base_url": "https://api.minimax.io/anthropic",
    "max_tokens": 1024,
    "timeout_seconds": 300
  }
}
```

### Path Resolution

| Path | Resolution |
| --- | --- |
| `MPM_WORKSPACE` env var | Explicit override |
| `mpm_config.json` (`sessions_dirs`, `memory_dirs`) | Explicit configuration |
| `~/.mpm/` | Standard home fallback |

| Data | Default Location |
| --- | --- |
| Database | `~/.mpm/mpm.db` |
| Sessions | Configured via `sessions_dirs` |
| Mirror log | `~/.mpm/mirror.jsonl` |
| Modes | `~/.mpm/mode/` |
| Personas | `~/.mpm/persona/` |

---

## Usage & Commands

### Daily Commands (7 total — everything else is `mpm ops`)

```bash
mpm <query>       Search memories (default-to-recall for bare string)
mpm add <text>    Add a new memory
mpm add -i        Interactive add — opens $EDITOR
mpm snooze <id>   Bump a memory's relevance (no LTM promotion)
mpm ls            List memories
mpm show <id>     Show memory details
mpm rm <id>       Delete a memory
mpm ops           Engine room — maintenance, diagnostics, synthesis
mpm help          Full help
```

### Pipe & Aliases

```bash
cat idea.md | mpm       Pipe stdin to add (shows preview, confirms before save)
mpm s "query"          User alias for recall (defined in mpm_config.json)
mpm in                 User alias for add -i
```

### Epistemology Engine

```bash
mpm propose_theory "HYPOTHESIS: ...\nVALIDATION_CRITERIA: ..."
mpm theories           List all theories with status chips
mpm theories pending  Filter to pending only
mpm resolve_theory <id> <conclusion>
mpm record_decision "CONTEXT: ...\nCHOICE: ...\nRATIONALE: ..."
mpm decisions          Show formatted decision ledger
```

### Engine Room (`mpm ops <command>`)

All maintenance, diagnostics, and rare commands. `mpm ops help` lists them all. All commands below also work at root level (backwards compatible).

```bash
mpm ops doctor [--fix]       Diagnostics + auto-repair
mpm ops maintain            Self-maintenance (decay, consolidate, prune)
mpm ops synthesize          LLM synthesis on all memories
mpm ops gc [--dry-run]       Memory decay sweep
mpm ops review              Spaced reinforcement review
mpm ops watch start [--bg]  Watcher daemon (start/stop/status)
mpm ops web                 Start web UI
mpm ops stats                Memory statistics
mpm ops status               System dashboard (memory counts, daemon, synthesis stats)
mpm ops prune                Prune expired memories
mpm ops export               Export all to JSON
mpm ops backup [path]        Database backup
mpm ops restore-db <path>    Restore from .sql dump
mpm ops ingest <path>        Import from external SQLite
mpm ops stance assume <mode> <persona> <rationale>  XITL: hot-swap existing persona (auto required)
mpm ops stance synthesize <name> [flags]          XITL: generate JIT ephemeral persona
mpm ops promote                                   XITL: flush ephemeral persona to disk
mpm ops mode | persona       Mode and persona operations
mpm ops topic | lesson       Topic and lesson operations
mpm ops session | memory     Session and memory operations
mpm ops reference             Reference library
mpm ops wake                  Show last session context
mpm ops gateway               Gateway control
```

### Aliases Configuration

User-defined shortcuts in `mpm_config.json`. The DWIM layer is entirely user-controlled.

```json
{
  "aliases": {
    "s": "recall",
    "in": "add -i",
    "mem": "recall --collection memories"
  }
}
```

Chain expansion: `mpm s foo bar` → `mpm recall foo bar`. `--version`, `-v`, `help` bypass alias resolution.

### All Commands (Full Reference)

| Command | Notes |
| --- | --- |
| **Daily** | |
| `mpm add <text> [--tag <tag>] [--weight <n>]` | Add memory |
| `mpm add -i` | Interactive add via $EDITOR |
| `mpm recall <query>` | FTS5 search (or bare `mpm <query>`) |
| `mpm snooze <id>` | Bump relevance |
| `mpm ls [--collection <c>] [--tag <t>] [--since <date>] [--limit <n>]` | List memories |
| `mpm show <id>` | Show one memory |
| `mpm rm <id>` | Soft delete |
| **Epistemology** | |
| `mpm propose_theory <text>` | Record hypothesis + validation criteria |
| `mpm theories [pending\|resolved\|all]` | List theories with status chips |
| `mpm resolve_theory <id> <conclusion>` | Mark resolved, bump weight |
| `mpm record_decision <text>` | Log decision (context + choice + rationale) |
| `mpm decisions` | Formatted decision ledger |
| **Memory lifecycle** | |
| `mpm promote <id>` | Elevate to LTM (weight=10) |
| `mpm reinforce <id> [n]` | +N reinforcement |
| `mpm weaken <id>` | -1 reinforcement |
| `mpm set-weight <id> <n>` | Set weight directly |
| `mpm patch-memory <id> '<json>'` | Patch metadata in-place (FTS untouched) |
| `mpm shred <id>` | Cascade-delete memory + linked theory (atomic) |
| **Engine room (`ops`)** | |
| `mpm ops doctor [--fix]` | Diagnostics |
| `mpm ops maintain [--days <n>]` | Decay + consolidate + prune |
| `mpm ops synthesize [--dry-run]` | LLM synthesis |
| `mpm ops gc [--dry-run\|--review\|--purge]` | Decay sweep |
| `mpm ops review [--promoted\|--stale]` | Spaced reinforcement |
| `mpm hint <text> [--max <n>] [--json]` | Proactive recall hint from conversation context |
| `mpm ops watch start [--bg]\|stop\|status` | Watcher daemon |
| `mpm ops web [--port <n>]` | Web UI |
| `mpm ops stats` | Memory statistics |
| `mpm ops status` | System dashboard with memory counts, daemon status, synthesis stats |
| `mpm ops prune [--older-than <n>d]` | Prune old memories |
| `mpm ops export [--jsonl]` | Export |
| `mpm ops backup [path]` | Database backup |
| `mpm ops restore-db <path>` | Restore .sql dump |
| `mpm ops ingest <path>` | Import from external SQLite |
| `mpm ops switch` | Interactive mode/persona switcher |
| **Modes & Personas** | |
| `mpm mode` | Interactive mode picker |
| `mpm mode list\|active\|remove\|clear` | Direct mode management |
| `mpm persona` | Interactive persona picker |
| `mpm persona list\|active\|set\|clear` | Direct persona management |
| `mpm prime-directives` | Display 808 rules |
| **Topics, References, Sessions, Lessons** | |
| `mpm topic create\|add\|remove\|list\|show\|promote` | Topic operations |
| `mpm reference add\|list\|search\|show\|shred [--chunk-size <n>]` | Reference library |
| `mpm session list\|search\|show` | Session operations |
| `mpm lesson add\|list\|search\|get\|shred\|stats` | Lesson operations |
| `mpm wake [--json]` | Last session context |
| **System** | |
| `mpm version\|help\|--version\|-h` | Info |
| `mpm gateway <sub>` | OpenClaw gateway control |
| `mpm ingest list-schemas` | Available import schemas |
| `mpm shred sessions\|memories\|topics\|database [-f]` | Destructive ops |

---

## Environment Variables

| Variable | Purpose |
| --- | --- |
| `MPM_WORKSPACE` | Override workspace directory |
| `MPM_FORCE` | Skip confirmation prompts |
| `MPM_INTERACTIVE` | Force interactive mode |
| `MINIMAX_API_KEY` | LLM API key (synth fallback) |
| `MINIMAX_BASE_URL` | LLM base URL (synth fallback) |

---

## Build Requirements

```bash
# Ubuntu/Debian
sudo apt install build-essential golang sqlite3 libsqlite3-dev

# macOS
brew install go sqlite

make build    # Requires CGO with FTS5
make test     # Run tests
make install  # Install to /usr/local/bin/mpm
```

---

## File Locations

```
~/.mpm/                         # Default workspace (MPM_DIR)
├── mpm.db                      # Unified SQLite database (WAL mode)
├── mpm.db-wal                  # WAL journal
├── mpm.db-shm                  # Shared memory
├── mirror.jsonl                # Security audit log
├── mode/                       # Mode .md files
├── persona/                    # Persona .md files
├── active.json                 # Active mode/persona state
├── watch.pid                   # Watcher PID (when detached)
└── mpm_config.json             # Configuration
```

---

## License

GNU AGPL v3. Part of the OpenClaw agent ecosystem.

Created by v (human) + 808 (AI).