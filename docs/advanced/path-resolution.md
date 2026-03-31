# Path Resolution Guide

**Last Updated:** 2026-03-31  
**MPM Version:** 6.0.0+

## Core Principle: Strict Input/Output Separation

MPM enforces a hard separation between **input watch directories** and **output storage**:

```
INPUT (Watch Daemon)          OUTPUT (Database)
─────────────────────         ──────────────────────
.memory_dir   ──────────►    mpm/src/db/mpm_memory.db  (SQLite)
.sessions_dir ──────────►    mpm/src/db/mpm_memory.db  (sessions table)
                              mpm/src/db/mirror.jsonl   (audit mirror)
```

> **CRITICAL:** The database is **ALWAYS** created at `mpm/src/db/mpm_memory.db`. Under no circumstances does MPM create storage databases outside of `mpm/src/db/`.

## Priority System (Workspace Detection)

1. **CLI Flag** → `--workspace=/custom/path`
2. **Environment Variable** → `export MPM_WORKSPACE=/custom/path`
3. **Executable Relative** → `os.Executable()` detects workspace from binary location
4. **Current Working Directory** → `os.Getwd()` (fallback)

## Output Paths (Internal Storage — Immutable)

All processed data is **written to** the MPM internal database:

| File | Purpose |
|------|---------|
| `mpm/src/db/mpm_memory.db` | Unified SQLite database (all tables: memories, sessions, topics, modes, personas) |
| `mpm/src/db/mirror.jsonl` | Audit mirror of all stored content (JSONL, append-only) |

**The output path is never configurable.** It is always `mpm/src/db/mpm_memory.db`.

## Input Paths (Watch Directories — Configurable)

These are the directories the **fsnotify daemon watches** for new files to ingest:

| Config Key | Environment Variable | Default | Description |
|------------|---------------------|---------|-------------|
| `memory_dir` | `MPM_MEMORY_DIR` | `~/.openclaw/workspace/memory` | Directory scanned for `.md` files → ingested into LTM |
| `sessions_dir` | `MPM_SESSIONS_DIR` | `~/.openclaw/agents/main/sessions` | Directory scanned for `.jsonl` files → session facts extracted |

> These directories are **input sources only**. Processed data does NOT get written back to them.

## Configuration File

File: `mpm/projects/mpm/mpm_config.json`

```json
{
  "memory_dir": "~/.openclaw/workspace/memory",
  "sessions_dir": "~/.openclaw/agents/main/sessions",
  "workspace": "~/.openclaw/workspace"
}
```

### Priority
1. Config file `memory_dir`/`sessions_dir` (highest)
2. Environment variables (`MPM_MEMORY_DIR`, `MPM_SESSIONS_DIR`)
3. OpenClaw defaults: `~/.openclaw/workspace/memory` and `~/.openclaw/agents/main/sessions`

## CLI Commands

```bash
mpm config show              # Show current paths
mpm config set memory_dir <path>   # Set memory watch directory
mpm config set sessions_dir <path> # Set sessions watch directory
```

## Key Path Functions

| Function | Returns |
|----------|---------|
| `GetWorkspace()` | Base workspace directory |
| `GetMPMDir()` | `projects/mpm/` directory |
| `GetDBPath(filename)` | `mpm/src/db/<filename` |
| `GetMemoryPath()` | Configured `memory_dir` (watch input) |
| `GetSessionsPath()` | Configured `sessions_dir` (watch input) |
| `GetPersonaPath()` | `mpm/persona/` |
| `GetModePath()` | `mpm/mode/` |

## Portable Installation Structure

```
/workspace/
└── symai/
    └── projects/
        └── mpm/
            ├── mpm                          ← Binary
            ├── src/db/
            │   ├── mpm_memory.db            ← ALWAYS here (output)
            │   └── mirror.jsonl             ← ALWAYS here (output)
            ├── mode/                        ← Mode configs
            ├── persona/                     ← Persona configs
            └── toxicphrases.txt             ← Firewall file
```

Binary at `/opt/mpm/bin/mpm` → Workspace resolved via `os.Executable()` parent walk.
