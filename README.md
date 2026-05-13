You are completely right. I truncated the "Usage & Commands" and environment variable sections with a placeholder to save space, but that defeats the purpose of giving you a file you can just copy and paste cleanly. My apologies.

Here is the **complete, full-length `README.md**`. It includes every single CLI command, flag, and configuration section from your original file, seamlessly merged with all the v2.0 architectural updates (FTS5 builds, deduplication, new synth logic, and the new watcher trigger).

Copy and paste this entirely:

---

```markdown
# MPM — Memory-Persona-Mode Manager

SQLite-native agent state management for AI agents. Single binary, zero external dependencies.

MPM provides long-term memory, behavioral modes, and persona management for AI agents. Everything is stored in a single SQLite database with full-text search — no server process, no socket IPC, no complexity.

---

## Overview

MPM is your agent's **persistent memory layer**. It stores facts, lessons, topics, and references in a unified SQLite database (`mpm.db`), with automatic file watching for seamless memory ingestion from your workflow.

### Key Features

- **Unified SQLite database** with WAL mode for concurrent reads/writes
- **Full-text search** via SQLite FTS5 — no external search service needed
- **Automatic file watching** — `.md` files become LTM memories; finished sessions are swept, synthesized, and safely archived
- **Topic clustering** — when 3+ long-term memories share an LLM-generated tag, a topic is auto-created
- **Reference library** — ingest PDFs, EPUBs, and documents with automatic Smart Fence chunking
- **Security scanning** — all content checked against 20 sensitive-data regex patterns before storage
- **Modes & Personas** — multi-select behavioral modes and single-select identity profiles
- **Spaced reinforcement** — memories that are accessed/used grow stronger over time

### What MPM Is NOT

MPM is **not** a daemon you run separately. There is no `mpm-server` process. Every command — `mpm add`, `mpm recall`, `mpm watch start` — is a single invocation of the `mpm` binary. The file watcher runs as a background goroutine within the same process (or as a detached child for systemd-style lifecycle management).

---

## Architecture

### Single-Process, Shared-Database Model

In MPM v2.0, the architecture was radically simplified:

```text
┌─────────────────────────────────────────────────────────────┐
│                         mpm binary                          │
│                                                             │
│  ┌─────────────┐   ┌─────────────┐   ┌─────────────────┐  │
│  │  CLI input  │──▶│   Router    │──▶│    Handler      │  │
│  └─────────────┘   └─────────────┘   └────────┬────────┘  │
│                                                 │           │
│  ┌─────────────────────────────────────────────┼────────┐ │
│  │            Background subsystem              │        │ │
│  │  ┌──────────────┐  ┌───────────────────┐    │        │ │
│  │  │  WorkerPool  │  │  fsnotify watcher │    │        │ │
│  │  │  (3 goros)   │  │  (goroutine)      │    │        │ │
│  │  └──────┬───────┘  └─────────┬─────────┘    │        │ │
│  │         │                    │              │        │ │
│  │         └──────────┬─────────┘              │        │ │
│  │                    ▼                        │        │ │
│  │         ┌─────────────────────┐             │        │ │
│  │         │  Shared DBManager    │             │        │ │
│  │         │  (SQLite + WAL)      │             │        │ │
│  │         └──────────┬────────────┘             │        │ │
│  └────────────────────┼────────────────────────┘        │ │
│                        ▼                                  │ │
│              ┌─────────────────┐                         │ │
│              │   src/db/mpm.db  │  (unified database)    │ │
│              └─────────────────┘                         │ │
└─────────────────────────────────────────────────────────────┘

```

**Key architectural points:**

1. **No socket IPC** — Commands execute directly in the same process. The old Unix socket daemon (`net` imports, `watch.sock`) was completely removed.
2. **No separate watcher process** — By default, `mpm watch start` launches the file watcher as a background goroutine within the same binary. The `--bg` flag spawns a detached child process for systemd integration.
3. **Unified WAL pool** — All access goes through one `DatabaseManager` instance sharing a single SQLite connection with WAL mode enabled.
4. **Worker pool** — A fixed-size goroutine pool (default 3 workers) processes file watcher events concurrently, sharing the database connection without blocking the main event loop.
5. **PID file** — When running detached (`--bg`), the watcher writes its PID to `watch.pid` for inter-process communication with `stop` and `status` commands.

### Memory Model

Everything is a **memory**. Collections distinguish types:

| Collection | Purpose | Default Weight | Notes |
| --- | --- | --- | --- |
| `memories` | General facts, knowledge, and LLM-synthesized insights | 1 | Tagged for auto-topic clustering |
| `session` | Operational facts (CWD, model changes) from `.jsonl` | 1 | State-change deduplicated with 24h auto-expiry (TTL) |

**Long-term memory (LTM)** is any memory with `weight >= 10`. LTM is promoted by:

* `mpm promote <id>` — explicit promotion (weight=10)
* File watcher processing `.md` files — auto-ingested as LTM (weight=10)

**Relevance scoring:**

```text
score = (reinforcement_count × 2) + (weight × 1.5) + recency_bonus

```

### Database Schema

The unified `mpm.db` contains:

| Table | Purpose |
| --- | --- |
| `memories` | Core storage (content, tags, metadata, FTS5-triggered embedding) |
| `sessions` | Session metadata and transcripts |
| `topics` | Topic definitions |
| `topic_memberships` | Memory-to-topic links |
| `modes` | Behavioral mode configurations |
| `personas` | Persona profiles |
| `lessons` | Learned lessons (insight/warning/practice) |
| `reference_docs` | Reference document metadata |
| `reference_chunks` | Smart Fence chunks from ingested documents |
| `system_config` | Configuration snapshots (hash-verified) |
| `external_db_cursors` | Sync cursors for external DB polling |
| `raw_memories` | Staging area for ingest workflow |

### Security

All content is scanned against **20 regex patterns** before any database write:

* API keys (OpenAI, Anthropic, GitHub, AWS, Stripe, Slack)
* JWTs, bearer tokens, SSH keys, private keys
* Database connection strings (`mysql://`, `postgres://`, etc.)
* Password/secret patterns (`password=`, `secret=`, `api_key=`)

Blocked content is logged to `mirror.jsonl` but never reaches the database.

Toxic phrase detection (`toxicphrases.txt`) blocks prompt injection patterns via case-insensitive substring match.

---

## Installation

### Build from Source

**Requirements:**

* Go 1.18+
* C compiler (for `mattn/go-sqlite3`)
* SQLite compiled with FTS5 support

**Crucial Note:** You *must* build using the Makefile (which passes `CGO_CFLAGS="-DSQLITE_ENABLE_FTS5=1"`). A plain `go build` will silently fail FTS5 indexing, breaking the `mpm recall` commands.

```bash
cd /home/v/workspace/projects/mpm
make build

```

This produces `bin/mpm`. The binary is self-contained.

**Install system-wide:**

```bash
sudo cp bin/mpm /usr/local/bin/mpm

```

### Systemd Service (Reboot Persistence)

For automatic restart on reboot, install the systemd unit:

```bash
# 1. Copy the unit file
sudo cp /home/v/workspace/projects/mpm/contrib/systemd/mpm.service /etc/systemd/system/

# 2. Reload systemd
sudo systemctl daemon-reload

# 3. Enable and start
sudo systemctl enable mpm
sudo systemctl start mpm

# 4. Check status
systemctl status mpm
journalctl -u mpm -f

```

The service runs `mpm watch start` (goroutine-based watcher, not detached). Systemd manages the process lifecycle — no `--bg` flag needed.

### Path Resolution

MPM resolves paths in this priority order:

| Path | Resolution |
| --- | --- |
| `MPM_WORKSPACE` env var | Explicit override |
| `mpm_config.json` (`sessions_dirs`, `memory_dirs`) | Explicit configuration |
| `~/.mpm/` | Standard home directory fallback |
| Current working directory | Last resort |

| Data | Default Location |
| --- | --- |
| Database | `~/.mpm/src/db/mpm.db` |
| Sessions | Configured via `sessions_dirs` in `mpm_config.json` |
| Mirror log | `~/.mpm/src/db/mirror.jsonl` |
| Modes | `~/.mpm/mode/` |
| Personas | `~/.mpm/persona/` |

### Configuration

MPM reads `mpm_config.json` from the workspace root. Create it manually or let MPM auto-scaffold it on first run.

**Example `mpm_config.json`:**

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
    "base_url": "[https://api.minimax.io/anthropic](https://api.minimax.io/anthropic)",
    "max_tokens": 1024,
    "timeout_seconds": 300
  }
}

```

---

## Usage & Commands

### Core Memory

```bash
# Add a memory
mpm add "Remember to use gRPC for internal service communication"
mpm add "The payment service requires JWT validation" --tag security --weight 5
mpm add "Weekly review every Friday 3pm" --ttl 7d

# List memories
mpm ls                          # Recent 20 memories
mpm ls --collection memories    # Filter by collection
mpm ls --collection session     # View operational state-changes
mpm ls --tag important          # Filter by tag
mpm ls --since 2026-01-01      # Since date
mpm ls --limit 50               # Custom limit

# Show and delete
mpm show abc123                 # Show memory details
mpm rm abc123                   # Soft delete (sets deleted_at)
mpm shred abc123                # Secure delete (DELETE + deferred VACUUM)

```

### Search & Recall

```bash
# Full-text search (FTS5 + LIKE fallback)
mpm recall sqlite
mpm recall "JWT authentication"
mpm recall "payment" --limit 10

```

### Memory Importance

```bash
# Promote to LTM (weight=10, clears TTL)
mpm promote abc123

# Reinforce or weaken
mpm reinforce abc123           # +1 reinforcement (default)
mpm reinforce abc123 3         # +3
mpm weaken abc123              # -1
mpm set-weight abc123 7        # Set directly (0-100)

```

### Stats & Maintenance

```bash
# Statistics
mpm stats                      # Memory counts, tag distribution, reinforcement

# Prune old/expired memories
mpm prune                      # Default: older than 90 days
mpm prune --older-than 30d     # Custom TTL

# Export
mpm export                     # Export all memories to JSON
mpm export --format csv        # CSV format
mpm export --collection memories --since 2026-01-01

# Self-maintenance (decay, consolidate, prune)
mpm maintain                   # Run maintenance cycle
mpm maintain --review          # Preview without applying
mpm maintain --days 30         # Prune memories not accessed in 30 days

```

### Modes & Personas

```bash
# Interactive TUI selection (multi-select for modes, single-select for persona)
mpm mode                       # Pick modes, auto-compiles
mpm persona                    # Pick persona, auto-compiles

# Direct management
mpm mode list                  # Show available modes
mpm mode active                # Show currently active modes
mpm mode add developer         # Add a mode
mpm mode remove developer      # Remove a mode
mpm mode clear                 # Clear all active modes

mpm persona list               # Show available personas
mpm persona active             # Show active persona
mpm persona set 808            # Set active persona
mpm persona clear              # Clear active persona

# Prime directives (808 rules)
mpm prime-directives           # Display all prime directives

```

### Topics

```bash
mpm topic create "golang patterns" --desc "Go idioms and patterns"
mpm topic add abc123 "golang patterns"   # Add memory to topic
mpm topic remove abc123 "golang patterns" # Remove from topic
mpm topic list                            # List all topics
mpm topic show def456                     # Show topic details
mpm topic promote def456                  # Promote topic to memory

```

### Reference Library

Ingest documents and search within them. Supports PDF, EPUB, HTML, Markdown, and plain text.

```bash
# Add a reference document
mpm reference add book.pdf
mpm reference add manual.md --tag golang
mpm reference add research.epub

# List, search, show
mpm reference list              # All references
mpm reference search "performance"  # Search content
mpm reference show abc123      # Full document with chunks

# Delete
mpm reference shred abc123

```

### Sessions

```bash
mpm session list                # Recent sessions
mpm session search "project"   # Search sessions
mpm session show abc123        # Session details
mpm session add "Notes from the architecture review"

```

### Lessons

Lessons store learned wisdom — insights, warnings, and best practices.

```bash
mpm lesson add "Always validate JWT expiration" --type warning --tags security
mpm lesson add "Use connection pooling for external APIs" --type practice --tags golang
mpm lesson add "gRPC streaming is better for high-throughput data pipelines" --type insight

mpm lesson list                 # All lessons
mpm lesson list --type warning  # Filter by type
mpm lesson search "security"
mpm lesson get abc123
mpm lesson shred abc123
mpm lesson stats               # Statistics by type

```

### File Watcher (Race-Condition Safe)

The watcher monitors directories for automatic memory ingestion. It operates entirely on concurrent goroutines.

* **`.md` files** → ingested as LTM memories, then deleted.
* **Orphan Sweep (Next-Session Trigger)** → To prevent race conditions with active agent sessions, the watcher triggers *only* when a new session lock (`.jsonl.lock`) is created. It scans for any older `.jsonl` files without matching locks, triggers `mpm synthesize <uuid>`, and then safely renames the raw logs to an `archive/` directory to prevent data loss.
* **`workspace.json`/`config.json**` → snapshot stored in `system_config` table.

```bash
# Lifecycle (goroutine-based, within the CLI process)
mpm watch start                # Start the watcher
mpm watch stop                 # Stop gracefully
mpm watch status               # Check if running

# Detached mode (for systemd integration)
mpm watch start --bg           # Spawns detached child, parent exits

# Path management
mpm watch add-path /path/to/memory --type memory
mpm watch add-path /path/to/sessions --type sessions
mpm watch remove-path /path/to/memory --type memory
mpm watch list-paths           # Show all configured paths

```

### Web UI

```bash
mpm web         # Start web UI at http://localhost:18792

```

Requires `web_token` in `mpm_config.json` for authentication.

### System Diagnostics

```bash
mpm doctor              # Run diagnostics (6 categories)
mpm doctor --fix         # Apply automatic repairs

```

Checks: system info, environment variables, workspace structure, database integrity, network connectivity, external dependencies.

### Synthesis (LLM Tagging)

Generate memory summaries from session data using an LLM. The AI extracts facts and returns a structured JSON array, applying 1-3 lowercase tags per memory to enable automatic topic clustering.

```bash
mpm synthesize <session-uuid>

```

Configure the LLM provider in `mpm_config.json` `[synth]` section.

### Interactive Menu

```bash
mpm menu         # Launch the transient interactive mode/persona picker

```

### Other Commands

```bash
mpm version               # Show version
mpm help                  # Show help
mpm ingest <path>         # Import from external SQLite (e.g., OpenClaw)
mpm ingest list-schemas   # Show available tables
mpm compile mode          # Force recompile modes
mpm compile persona       # Force recompile personas
mpm shred sessions -f     # Delete all sessions (requires --force)
mpm shred memories -f     # Delete all memories
mpm shred topics -f      # Delete all topics
mpm shred database -f    # Wipe and recreate database

```

---

## Environment Variables

| Variable | Purpose |
| --- | --- |
| `MPM_WORKSPACE` | Override workspace directory |
| `MPM_FORCE` | Skip confirmation prompts |
| `MPM_INTERACTIVE` | Force interactive mode |
| `MPM_SELECT` | Internal: PTY selector subprocess |
| `MINIMAX_API_KEY` | LLM API key (synth fallback) |
| `MINIMAX_BASE_URL` | LLM base URL (synth fallback) |
| `MPM_WEBHOOK_URL` | Heartbeat webhook target |

---

## Build Requirements

```bash
# Ubuntu/Debian
sudo apt install build-essential golang sqlite3 libsqlite3-dev

# macOS
brew install go sqlite

# Build
make build    # Requires CGO for mattn/go-sqlite3 with FTS5
make test     # Run tests
make install  # Install to /usr/local/bin/mpm

```

The build requires `CGO_CFLAGS="-DSQLITE_ENABLE_FTS5=1"` for full-text search support.

---

## File Locations Summary

```
~/.mpm/                         # Default workspace (MPM_DIR)
├── src/db/
│   ├── mpm.db                  # Unified SQLite database (WAL mode)
│   ├── mpm.db-wal              # WAL journal
│   ├── mpm.db-shm              # Shared memory
│   └── mirror.jsonl             # Security audit log
├── mode/                        # Mode JSON files
├── persona/                     # Persona JSON files
├── watch.pid                    # Watcher PID (when detached)
└── mpm_config.json              # Configuration

```

---

## License

GNU AGPL v3. Part of the OpenClaw agent ecosystem.

Created by v (human) + Great_808 (AI).

```

```
