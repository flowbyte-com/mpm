# MPM — Memory-Persona-Mode Manager

**MPM** is a SQLite-native agent state management system. Everything is a **memory** — sessions, topics, lessons, references are just memories with different metadata. Simple CLI, powerful system.

Built with Go. No external dependencies. Single binary.

---

## What It Does

MPM gives your AI agent long-term memory and persistent identity:

| Capability | What It Means |
|------------|----------------|
| **Memory** | Searchable facts with reinforcement learning |
| **Sessions** | Full transcript archive (collection: session) |
| **Topics** | Knowledge organization via tags |
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

## Quick Start

```bash
# Build
make build

# Add a memory (no daemon needed!)
./bin/mpm add "Use sqlite3 Vacuum after bulk deletes"

# List memories
./bin/mpm ls

# Search
./bin/mpm recall sqlite

# Memory importance
./bin/mpm promote <id>    # Make LTM
./bin/mpm reinforce <id>   # Mark as useful
./bin/mpm weaken <id>     # Mark as less useful

# Start daemon for mode/persona changes
./bin/mpm start
./bin/mpm mode debug
./bin/mpm persona helpful
```

---

## Simplified CLI

### Core Memory Commands (no daemon needed)

```bash
mpm add <content>                    # Add memory
  --collection <name>                # Collection (default: memories)
  --tag <tag1,tag2>                  # Tags
  --weight <1-100>                  # Importance (default: 1)
  --ttl <7d,24h>                    # Time to live

mpm ls                               # List memories
  --collection <name>                 # Filter by collection
  --tag <tag>                        # Filter by tag
  --since YYYY-MM-DD                 # Since date
  --until YYYY-MM-DD                 # Until date
  --limit <n>                        # Max results

mpm show <id>                        # Show memory details
mpm rm <id>                          # Soft delete
mpm recall <query>                   # Search (FTS5)
  --since YYYY-MM-DD                 # Time range filter
  --until YYYY-MM-DD

mpm shred <id>                       # Secure delete (DELETE + VACUUM)
```

### Memory Importance

```bash
mpm promote <id>                     # Make LTM (weight=10)
mpm reinforce <id> [delta]           # Increment reinforcement (+1)
mpm weaken <id> [delta]              # Decrement reinforcement (-1)
mpm set-weight <id> <0-100>          # Set weight directly
```

### Statistics & Maintenance

```bash
mpm stats                            # Memory health dashboard
mpm prune                            # Prune expired memories
  --older-than 90d                  # By age
  --never-accessed                   # Never accessed
mpm export                           # Export to JSON/CSV
  --format json|csv
  --collection <name>
  --since YYYY-MM-DD
```

### Daemon-Required Commands (runtime agent config)

```bash
mpm mode [name]                     # Switch mode (stackable)
mpm persona [name]                   # Switch persona
mpm prime-directives                  # Show 808 directives
mpm watch                            # Watch daemon for auto-ingest
mpm web                              # Web UI
mpm dashboard                        # Real-time TUI dashboard
```

### Legacy Commands (still work, route to daemon)

```bash
mpm session ...                      # Session operations
mpm topic ...                        # Topic operations
mpm lesson ...                       # Lesson operations
mpm reference ...                    # Reference library
mpm ingest <path>                   # Import from external SQLite
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

- **Main daemon** — CLI commands, SQLite, session synthesis, mode/persona
- **Watch daemon** — Filesystem monitoring, auto-ingestion (runs independently)

---

## Memory Model

Everything is a **memory**. Collections distinguish types:

| Collection | Purpose |
|------------|---------|
| `memories` | General memories |
| `session` | Session facts |
| `lessons` | Learned lessons |

**Relevance scoring**: `(reinforcement_count * 2) + (weight * 1.5) + recency_bonus`

---

## Security

All content is scanned against 17 regex patterns before writing. Blocked: API keys (OpenAI, GitHub, AWS, Stripe, Slack), JWTs, private keys, SSH keys, database URLs, `password=`/`secret=` patterns.

Blocked content is logged to `mirror.jsonl` but never stored in the database.

---

## Database

Location: `src/db/mpm.db` (SQLite with FTS5 indexes)

Key tables: `memories`, `sessions`, `topics`, `topic_memberships`, `modes`, `personas`, `reference_docs`, `reference_chunks`, `lessons`

Path resolution: `MPM_WORKSPACE` env var → executable-relative → CWD fallback.

---

## Documentation

| Doc | What It Covers |
|-----|----------------|
| **[INSTALL.md](INSTALL.md)** | Installation, build, shell setup |
| **[docs/COMMANDS.md](docs/COMMANDS.md)** | Full CLI reference |
| **[docs/ARCHITECTURE.md](docs/ARCHITECTURE.md)** | How MPM works internally |
| **[docs/WATCH.md](docs/WATCH.md)** | Watch daemon, auto-ingestion, external DB |
| **[docs/PATH_CONFIG.md](docs/PATH_CONFIG.md)** | Path resolution, MPM_WORKSPACE |
| **[mode/README.md](mode/README.md)** | Creating and using modes |
| **[persona/README.md](persona/README.md)** | Creating and using personas |
| **[docs/security/security.md](docs/security/security.md)** | Threat model, 17 regex patterns |

---

## License

Part of the OpenClaw agent ecosystem.

**Created by:** v (human) + Great_808 (AI)

---

**Status:** Production Ready
