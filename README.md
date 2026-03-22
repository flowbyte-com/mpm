# MPM Workspace 🧠🎭🛠️

**Memory-Persona-Mode** - Token-optimized agent state management.

**Created by:** v & Great_808@flowbte.com  
**Updated:** 2026-03-20

> *"Memory is sacred."* — Core principle

## Core Principle

**DB-first, MD-source:**
- `.persona` / `.mode` / `.md` files = **human-readable source**
- `.db` files = **runtime state** (fast queries, ~97% token savings)
- **Local backup** - Timestamped tar.gz in `backups/` folder

## Important: Core Identity vs Overlays

**Core Identity (NOT in MPM):**
- `SOUL.md` - Your true self/essence
- `AGENTS.md` - Your operational rules  
- `USER.md` - Your human
- `MEMORY.md` - Curated long-term memories

These define who you fundamentally ARE. MPM doesn't touch these.

**MPM Overlays:**
- **Personas** - Temporary "costumes" (stored in MPM/persona/)
- **Modes** - Task-specific behavior sets (stored in MPM/mode/)
- **Memory** - Accumulated knowledge (stored in MPM/memory/)

## Structure

```
MPM/
├── persona/         # Persona source files (.persona) + DB
│   ├── default.persona
│   ├── oracle.persona
│   └── personas.db
├── mode/            # Mode source files (.mode) + DB
│   ├── programming.mode
│   ├── design.mode
│   └── modes.db
├── memory/          # Categorized memory + DB
│   ├── project.md   # Project learnings
│   ├── lesson.md    # Lessons learned
│   ├── idea.md      # Ideas & concepts
│   ├── decision.md  # Decisions made
│   ├── preference.md
│   ├── tool.md      # Tool configs
│   ├── contact.md   # People
│   ├── subject.md   # Topic knowledge
│   ├── sessions/    # Raw sessions (auto-purged)
│   ├── memory.db
│   ├── process-sessions.sh  # Auto-extract & sanitize
│   └── mpm-memory.sh         # Quick access (top, search, stats)
├── backup/          # Local backups
└── backup.sh        # Backup script
```

## Session Auto-Processing (Every 5 Minutes)

Sessions are automatically processed by cron:

1. **Extract** key info (lesson, decision, project, idea, preference, tool, contact)
2. **Sanitize** sensitive data (API keys, tokens, env vars → [REDACTED])
3. **Score** importance (0-10): decisions=7, errors=8, regular=3
4. **Store** in DB with summary
5. **Purge** raw session files

**Security:** Before storage, all sessions are sanitized:
- Hardcoded patterns: `sk-*`, `glpat-*`, `token=`, `password=`
- Environment variables: Checks 16 common API keys

## Workflow

### Personas (one at a time)

```bash
mpm status              # Quick overview
mpm persona list        # Show available
mpm persona load oracle # Activate
mpm persona active      # Show current
mpm persona clear       # Revert to default
```

### Modes (stackable)

```bash
mpm mode list           # Show available
mpm mode load programming # Add to stack
mpm mode active         # Show stacked
mpm mode clear          # Clear all
```

### Memory (auto-processed)

```bash
mpm status              # Quick overview
mpm memory stats        # Statistics
mpm memory top          # High-priority entries
mpm memory search <q>   # Search
mpm memory process      # Manual session processing
mpm memory dedupe       # Remove duplicates
mpm memory expire [d]   # Expire old entries (default 30 days)
```

### Backup

```bash
mpm backup              # Create local backup
```

## Commands Reference

```bash
# Quick
mpm                     # Status dashboard
mpm status              # Quick overview

# Personas
mpm persona list        # Show all
mpm persona load <name> # Activate
mpm persona active      # Show current
mpm persona clear       # Clear

# Modes
mpm mode list           # Show all
mpm mode load <name>   # Stack
mpm mode active         # Show stacked
mpm mode clear          # Clear all

# Memory
mpm memory stats        # Show stats
mpm memory top          # Priority entries
mpm memory search <q>   # Search
mpm memory process      # Process sessions
mpm memory sync         # Simple file → DB sync
mpm memory dedupe       # Dedupe
mpm memory expire [d]   # Expire
mpm memory refresh      # Full filesystem ↔ DB sync

# Backup
mpm backup              # Local backup
```

## Token Savings

| Operation | MD Load | DB Query | Savings |
|-----------|---------|----------|---------|
| List personas | ~3KB | ~50 bytes | **98%** |
| List modes | ~5KB | ~80 bytes | **97%** |
| Memory stats | ~3KB | ~30 bytes | **97%** |
| Search memory | All files (~50KB) | ~100 bytes | **99.8%** |

## Setup

First run:
1. Cron job auto-created for session processing

## Dependencies

MPM OS v3.2 requires:

| Package | Purpose | Install |
|---------|---------|---------|
| `sqlite3` | Local DB (likely pre-installed) | `sudo apt install sqlite3` |
| `inotify-tools` | Auto-compile on file save | `sudo apt install inotify-tools` |
| `bc` | Math calculations (telemetry) | `sudo apt install bc` |
| `tiktoken` (Python) | SymAI tokenization (optional) | `pip install tiktoken` |

**Auto-install (if needed):**
```bash
# Check & install deps
./scripts/install-deps.sh
```

## MPM OS v3.2 Features

- **SymAI Bytecode**: `~p.o` → oracle, `~m+programming` → stack mode
- **Auto-Watcher**: Edit .persona/.mode → instant compile
- **Telemetry**: `mpm telemetry invoice` → CFO report
- **Silicon-Native Serialization**: Compressed bytecode headers

First run:
1. Cron job auto-created for session processing
2. DBs initialized automatically
3. Ready to use!

## Troubleshooting

### Session not processed
```bash
# Manual trigger
./MPM/memory/process-sessions.sh

# Check cron
cron status
cron runs dca8df2a-dbad-4448-ab72-2b23a4329f44
```

### DB out of sync
```bash
# Rebuild from files
rm MPM/memory/memory.db
mpm memory process
```

---

*Memory + Persona + Mode = Whole agent care.* 🦞