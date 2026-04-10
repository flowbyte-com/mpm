# MPM — Memory-Persona-Mode Manager

**MPM** is a SQLite-native agent state management system for OpenClaw agents. It provides persistent memory, session management, topic organization, mode/persona configurations, a reference document library, and learned lessons — all indexed with FTS5 full-text search.

Built with Go. No external dependencies. Single binary.

---

## What It Does

MPM gives your AI agent long-term memory and persistent identity:

| Capability | What It Means |
|------------|----------------|
| **Memory** | Searchable facts distilled from sessions |
| **Sessions** | Full transcript archive |
| **Topics** | Hierarchical knowledge organization |
| **Modes** | Stackable behavioral contexts (debug, research, ship...) |
| **Personas** | Identity switching without prompt engineering |
| **References** | RAG-ready document library (PDF, EPUB, markdown) |
| **Lessons** | "Don't do X" / "Do Y" wisdom from experience |
| **Watch Daemon** | Auto-ingest from filesystem or external DB |

---

## Compatibility

- **OpenClaw agents** — Primary use case. MPM is the memory layer for OpenClaw.
- **Any AI agent** — Works standalone via CLI. Can ingest from any JSONL session format.
- **Platform** — Linux, macOS (Unix-like systems with fsnotify support)
- **Database** — SQLite with FTS5 (built into Go's sqlite3 driver)
- **Requirements** — Go 1.18+, ~50MB disk space

---

## Use Cases

- **Long-running agentic workflows** — Memory persists across sessions
- **Multi-project agents** — Separate databases per project via `MPM_WORKSPACE`
- **Shared team memory** — Reference library shared across agents
- **Personal AI coding assistant** — Your code patterns, preferences, lessons learned
- **Agent-to-agent memory transfer** — External DB polling syncs from OpenClaw

---

## Quick Start

```bash
# Build
make build

# Start daemon (starts both main daemon + watch daemon)
./bin/mpm start

# Check status
./bin/mpm status

# Add a memory
./bin/mpm memory add "Use sqlite3 Vacuum after bulk deletes"

# Search memories
./bin/mpm recall sqlite

# Stop
./bin/mpm stop
```

---

## Installation

See [INSTALL.md](INSTALL.md) for full installation instructions including:

- Building from source
- Installing system-wide (`make install`)
- Shell configuration (PATH)
- OpenClaw workspace setup
- Configuration

---

## Documentation

| Doc | What It Covers |
|-----|----------------|
| **[INSTALL.md](INSTALL.md)** | Installation, build, shell setup, config |
| **[docs/GETTING_STARTED.md](docs/GETTING_STARTED.md)** | First-time setup, daemon management |
| **[docs/COMMANDS.md](docs/COMMANDS.md)** | Full CLI reference |
| **[docs/ARCHITECTURE.md](docs/ARCHITECTURE.md)** | How MPM works internally |
| **[docs/WATCH.md](docs/WATCH.md)** | Watch daemon, auto-ingestion, external DB |
| **[docs/SYNTH.md](docs/SYNTH.md)** | LLM session synthesis pipeline |
| **[docs/PATH_CONFIG.md](docs/PATH_CONFIG.md)** | Path resolution, MPM_WORKSPACE |
| **[mode/README.md](mode/README.md)** | Creating and using modes |
| **[persona/README.md](persona/README.md)** | Creating and using personas |

---

## Key Commands

```bash
# Daemon
mpm start              # Start daemon + watch daemon
mpm stop               # Stop gracefully
mpm status             # Status dashboard

# Memory
mpm memory add "<text>"           # Add a memory
mpm recall <query>                 # FTS5 search
mpm memory list                    # List recent

# Sessions
mpm session list                   # List sessions
mpm synthesize <uuid>              # LLM extract facts from session

# Topics
mpm topic add <name>              # Create topic
mpm topic list                    # List all

# Modes & Personas
mpm mode                           # Interactive mode picker (multi-select)
mpm persona                        # Interactive persona picker (single-select)

# Lessons
mpm lesson add "Don't do X" --type warning    # Add warning
mpm lesson list                              # List all
mpm lesson search debugging                   # Find lessons

# Reference Library
mpm reference add ./docs/guide.pdf   # Ingest PDF/EPUB/markdown
mpm reference search <query>          # Search content
```

---

## Architecture

```
CLI (mpm) ────── Unix socket ──────> Main Daemon ──> SQLite (mpm.db)
                                          │
                                          └──> Watch Daemon (fsnotify)
                                                     ├── memory/*.md  → ingest as memory
                                                     ├── sessions/*.lock → derive session
                                                     └── external DBs → poll every 30s
```

- **Main daemon** — CLI commands, SQLite, session synthesis
- **Watch daemon** — Filesystem monitoring, auto-ingestion (runs independently)

---

## Security

All content is scanned against 17 regex patterns before writing. Blocked: API keys (OpenAI, GitHub, AWS, Stripe, Slack), JWTs, private keys, SSH keys, database URLs, `password=`/`secret=` patterns.

Blocked content is logged to `mirror.jsonl` but never stored in the database.

See [docs/security/security.md](docs/security/security.md) for full threat model.

---

## Database

Location: `src/db/mpm.db` (SQLite with FTS5 indexes)

Key tables: `memories`, `sessions`, `topics`, `topic_memberships`, `modes`, `personas`, `reference_docs`, `reference_chunks`, `lessons`

Path resolution: `MPM_WORKSPACE` env var → executable-relative → CWD fallback.

---

## License

Part of the OpenClaw agent ecosystem.

**Created by:** v (human) + Great_808 (AI)

---

**Status:** Production Ready
