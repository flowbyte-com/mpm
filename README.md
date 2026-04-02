# SymAI MPM (Memory-Persona-Mode) v6.0.0

DB-first agent state management with SQLite-only. Manage personas, modes, memory, and reference library with secure embedded storage.

## Documentation

**Full documentation:** [`docs/`](docs/)  
**Quick Start:** [`docs/getting-started/quick-start.md`](docs/getting-started/quick-start.md)

### Key Features

- **Personas** - Define *who* the assistant is (default, corporate, creative, etc.)
- **Modes** - Configure *how* it behaves (stackable for layered behavior)
- **Memory** - Semantic knowledge base with SQLite vector search
- **References** - Document library with chunking and vector search
- **FTS5 Search** - Full-text search with `snippet()` highlighting
- **Shred Protocol** - True hard delete with `DELETE` + `VACUUM`

### Quick Reference

| Task | Command |
|------|---------|
| Show status | `mpm status` |
| Select persona | `mpm persona set <name>` |
| Add mode | `mpm mode add <name>` |
| Search | `mpm memory search "query"` |
| Watch directories | `mpm watch [--v] [--dry-run]` |
| Add reference | `mpm reference add /path/to/file.pdf` |
| Save session | `mpm ss` |


## Architecture

```
symai/projects/mpm/
├── mpm                            # Compiled binary (Golang)
├── src/db/                        # SQLite databases (consolidated)
│   ├── mpm.db              # Main database (memories, sessions, topics)
│   ├── init.sql                   # Database initialization script
│   └── schema.sql                 # Database schema
├── mode/                          # Mode configurations (JSON)
├── persona/                       # Persona configurations (JSON)
├── src/                           # Source code (hidden from end users)
│   ├── cmd/                       # CLI entry points
│   ├── internal/                  # Core library
│   │   ├── config/                # Path resolution (portable)
│   │   └── ...
│   └── go.mod                     # Go module definition
└── docs/                          # Documentation
```

## Quick Start

### 1. Add to Your Shell Configuration

Add these lines to `~/.bashrc` (or `~/.zshrc`):

```bash
# MPM - Memory-Persona-Mode Manager
export PATH="$HOME/.openclaw/workspace/flowbyte/mpm:$PATH"
export MPM_WORKSPACE="$HOME/.openclaw/workspace"
```

Then reload:
```bash
source ~/.bashrc
```

### 2. Check Status
```bash
mpm status             # Full dashboard
mpm --help             # All commands
```

## Documentation Structure

| Section | Description |
|---------|-------------|
| **getting-started/** | Installation and quick start |
| **commands/** | CLI command reference |
| **advanced/** | FTS5 search, shred protocol, native ingestion |
| **security/** | Threat model and security implementation |

## Installation

### Enable via OpenClaw
```bash
openclaw skills enable mpm
```

### Manual Config
Add to `~/.openclaw/openclaw.json`:
```json
"skills": {
  "entries": {
    "mpm": { "enabled": true }
  }
}
```

### Verify
```bash
mpm status
```

## Setup

See [`docs/getting-started/quick-start.md`](docs/getting-started/quick-start.md) for detailed setup instructions.

### Customizing Paths

Edit `mpm_config.json` in the MPM project directory:

```bash
nano $HOME/.openclaw/workspace/flowbyte/mpm/mpm_config.json
```

```json
{
  "workspace": "$HOME/.openclaw/workspace",
  "memory_dir": "$HOME/.openclaw/workspace/memory",
  "sessions_dir": "$HOME/.openclaw/agents/main/sessions"
}
```

**Changes take effect immediately** - no rebuild or restart needed!

## Credits

**Created by:**
- **v** (human developer)
- **Great_808** (The Great 808 - AI agent)

**First release:** 2026-03-20

**Core Principle:** *"Memory is sacred."*  
Treat what you remember with care. Curate, don't hoard.

## See Also

- **[docs/](docs/)** - Full documentation
- **[src/](src/)** - Implementation details
- **[SET-UP.md](SET-UP.md)** - Detailed setup guide
- **[STRUCTURE.md](STRUCTURE.md)** - Project structure

## License

Part of the OpenClaw agent ecosystem.

---

**Status:** ✅ Production Ready  
**Last Updated:** 2026-03-30

### New in v6.0.1 (2026-03-30):

**Watch Daemon (fsnotify-based file watcher):**
- `mpm watch [--v] [--dry-run] [--once]` - Monitor directories for auto-ingestion
- Route A (.md): Create/Write → sanitize → ingest as LTM → delete
- Route B (.lock): Remove → process matching .jsonl → extract facts → delete
- Startup sweep processes existing files on daemon start

**New in v6.0.0 (2026-03-27):

**FTS5 Search (Full-Text Search with Highlighting):**
- `mpm session search "docker"` - Search sessions with highlighted results
- `mpm topic search "ai"` - Search topics with `snippet()` highlighting
- `mpm memory search "kubernetes"` - Search memories with FTS5

**Shred Protocol (Hard Delete with VACUUM):**
- `mpm session shred <id>` - Hard delete session (irreversible + VACUUM)
- `mpm topic shred <id>` - Hard delete topic (cascading + VACUUM)
- `mpm memory shred <id>` - Hard delete memory (VACUUM)

**Database Schema:**
- FTS5 virtual tables added to all three tiers
- Sync triggers to keep FTS5 in sync
- `snippet()` function for search result highlighting

**Native Document Ingestion:**
- PDF parsing: `github.com/ledongthuc/pdf` (pure Go)
- EPUB parsing: `archive/zip` + `golang.org/x/net/html`
- No external tools (Calibre, Python pdfplumber, etc.) needed
