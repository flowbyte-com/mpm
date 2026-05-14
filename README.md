# MPM — Memory Persistence Module

SQLite-native agent state management for AI agents. Single binary, zero external dependencies.

MPM provides long-term memory, behavioral modes, and persona management. Everything is stored in a single SQLite database with FTS5 full-text search — no server, no socket IPC, no complexity.

---

## Overview

### Key Features

- **Unified SQLite database** with WAL mode for concurrent reads/writes
- **Full-text search** via SQLite FTS5 — no external search service
- **Automatic file watching** — `.md` files ingested as LTM; sessions swept, synthesized, archived
- **Topic clustering** — 3+ LTM memories sharing a tag auto-generate a topic
- **Reference library** — PDF, EPUB, HTML, Markdown ingestion with Smart Fence chunking
- **Security scanning** — 20 regex patterns for API keys, tokens, secrets
- **Modes & Personas** — file-based (`.md`), multi-select modes, single-select persona
- **Spaced reinforcement** — accessed memories grow stronger over time

### What MPM Is NOT

MPM is **not** a daemon. Every command (`mpm add`, `mpm recall`, `mpm watch start`) is a single binary invocation. The watcher runs as a background goroutine within the same process, or as a detached child with `--bg` for systemd integration.

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

**Long-term memory (LTM):** any memory with `weight >= 10`. Promoted by `mpm promote <id>` or auto-ingested `.md` files from the watcher.

**Relevance scoring:** `score = (reinforcement_count × 2) + (weight × 1.5) + recency_bonus`

### Database Schema

| Table | Purpose |
| --- | --- |
| `memories` | Core storage with FTS5-triggered embedding |
| `sessions` | Session metadata and transcripts |
| `topics` | Topic definitions |
| `topic_memberships` | Memory-to-topic links |
| `lessons` | Learned lessons (insight/warning/practice) |
| `reference_docs` | Reference document metadata |
| `reference_chunks` | Smart Fence chunks from ingested documents |
| `system_config` | Configuration snapshots (hash-verified) |
| `external_db_cursors` | Sync cursors for external DB polling |
| `raw_memories` | Staging area for ingest workflow |

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

**Frontmatter fields:** `name`, `title`, `version`, `status`, `purpose`, `description`, plus persona-specific fields (`creature`, `vibe`, `voice`, `emoji`).

**Active state** (`active.json`):
```json
{
  "persona": "whiterabbit",
  "modes": ["programming", "research"],
  "updated": "2026-05-13T15:00:00Z"
}
```

---

## OpenClaw Plugin Integration

MPM ships as an OpenClaw plugin, giving any OpenClaw agent native function-calling access to the MPM memory layer via two tools: `query_long_term_memory` and `save_to_memory`.

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

### Core Memory

```bash
mpm add "Remember to use gRPC for internal services"
mpm add "Payment service requires JWT validation" --tag security --weight 5

mpm ls                          # Recent 20
mpm ls --collection memories    # Filter by collection
mpm ls --collection session     # Operational state-changes
mpm ls --tag important           # Filter by tag
mpm ls --since 2026-01-01       # Since date
mpm ls --limit 50               # Custom limit

mpm show abc123                 # Show details
mpm rm abc123                   # Soft delete
mpm shred abc123                # Secure delete (DELETE + deferred VACUUM)
```

### Search & Recall

```bash
mpm recall sqlite               # Full-text search (FTS5 + LIKE fallback)
mpm recall "JWT authentication"
mpm recall "payment" --limit 10
```

### Memory Importance

```bash
mpm promote abc123              # LTM (weight=10, clears TTL)
mpm reinforce abc123            # +1 reinforcement
mpm reinforce abc123 3          # +3
mpm weaken abc123               # -1
mpm set-weight abc123 7        # Set directly (0-100)
```

### Stats & Maintenance

```bash
mpm stats                       # Counts, tag distribution, reinforcement
mpm prune                       # Older than 90 days
mpm prune --older-than 30d     # Custom TTL
mpm export                      # Export all to JSON
mpm maintain                    # Run maintenance cycle (decay, consolidate, prune)
mpm maintain --days 30         # Prune memories not accessed in 30 days
```

### Modes & Personas

```bash
# Interactive TUI (multi-select modes, single-select persona)
mpm mode                        # Pick modes
mpm persona                     # Pick persona

# Direct management
mpm mode list                   # Show available modes
mpm mode active                 # Show active modes
mpm mode remove programming     # Remove a mode file
mpm mode clear                  # Clear all active modes

mpm persona list                # Show available personas
mpm persona active              # Show active persona
mpm persona set whiterabbit     # Set active persona
mpm persona clear               # Clear active persona

mpm prime-directives            # Display 808 rules
```

### Topics

```bash
mpm topic create "golang patterns" --desc "Go idioms and patterns"
mpm topic add abc123 "golang patterns"    # Add memory to topic
mpm topic remove abc123 "golang patterns"  # Remove from topic
mpm topic list                            # List all topics
mpm topic show def456                     # Show topic details
mpm topic promote def456                 # Promote topic to memory
```

### Reference Library

```bash
mpm reference add book.pdf
mpm reference add manual.md --tag golang
mpm reference list               # All references
mpm reference search "performance"
mpm reference show abc123        # Full document with chunks
mpm reference shred abc123
```

### Sessions

```bash
mpm session list                 # Recent sessions
mpm session search "project"    # Search sessions
mpm session show abc123         # Session details
```

### Lessons

```bash
mpm lesson add "Always validate JWT expiration" --type warning --tags security
mpm lesson add "Use connection pooling for external APIs" --type practice --tags golang
mpm lesson list                 # All lessons
mpm lesson list --type warning  # Filter by type
mpm lesson search "security"
mpm lesson get abc123
mpm lesson shred abc123
mpm lesson stats               # Statistics by type
```

### File Watcher

* **`.md` files** → ingested as LTM, then deleted
* **Orphan Sweep** → `.jsonl.lock` Create triggers synthesis of old `.jsonl` files without locks, then archived (not deleted)

```bash
mpm watch start                 # Start watcher (goroutine-based)
mpm watch stop                  # Stop gracefully
mpm watch status                # Check if running
mpm watch start --bg           # Detached mode (systemd)

mpm watch add-path /path/to/memory --type memory
mpm watch remove-path /path/to/memory --type memory
mpm watch list-paths            # Show all configured paths
```

### Web UI

```bash
mpm web         # Start at http://localhost:18792 (requires web_token in config)
```

### System Diagnostics

```bash
mpm doctor              # Run diagnostics (6 categories)
mpm doctor --fix        # Apply automatic repairs
```

### Synthesis (LLM Tagging)

```bash
mpm synthesize <session-uuid>   # Generate memories from session via LLM
```

Configure LLM in `mpm_config.json` `[synth]` section.

### Other Commands

```bash
mpm version               # Show version
mpm help                  # Show help
mpm ingest <path>         # Import from external SQLite
mpm ingest list-schemas   # Show available tables
mpm shred sessions -f     # Delete all sessions (requires --force)
mpm shred memories -f     # Delete all memories
mpm shred topics -f       # Delete all topics
mpm shred database -f     # Wipe and recreate database
```

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