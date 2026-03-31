# Configuration

**Last Updated:** 2026-03-31

The `config` package provides path resolution for MPM using a cascading priority system.

## Core Principle: Output is Always Internal

**All database storage is at `mpm/src/db/mpm_memory.db`.** This path is not configurable.

The configurable paths (`memory_dir`, `sessions_dir`) are **input/watch directories only** — directories the fsnotify daemon monitors.

## Path Resolution

### `GetWorkspace() string`
Returns the base workspace directory:
1. `MPM_WORKSPACE` environment variable (highest)
2. `os.Executable()` + parent directories (executable-relative)
3. `os.Getwd()` (current working directory fallback)

### `GetMPMDir() string`
Returns the MPM project root: `<workspace>/projects/mpm/`

### `GetDBPath(filename string) string`
Returns a path under `mpm/src/db/`: e.g., `GetDBPath("mpm_memory.db")` → `<workspace>/projects/mpm/src/db/mpm_memory.db`

### `GetSessionsPath() string`
Returns the configured `sessions_dir` (input/watch directory):
- Config `sessions_dir` if set
- Default: `~/.openclaw/agents/main/sessions`

### `ResolveEnvPath(path string) string`
Expands `~` to home directory; resolves relative paths against workspace.

## Configuration File

`mpm/projects/mpm/mpm_config.json`:

```json
{
  "workspace": "~/.openclaw/workspace",
  "memory_dir": "~/.openclaw/workspace/memory",
  "sessions_dir": "~/.openclaw/agents/main/sessions"
}
```

### Keys

| Key | Purpose | Default |
|-----|---------|---------|
| `memory_dir` | Watch directory for `.md` files | `~/.openclaw/workspace/memory` |
| `sessions_dir` | Watch directory for `.jsonl` files | `~/.openclaw/agents/main/sessions` |
| `workspace` | MPM workspace root | Auto-detected |

### Priority
1. Config file `memory_dir`/`sessions_dir`
2. Environment variables (`MPM_MEMORY_DIR`, `MPM_SESSIONS_DIR`)
3. OpenClaw defaults

## Output Paths (Immutable)

| File | Always At |
|------|-----------|
| SQLite DB | `mpm/src/db/mpm_memory.db` |
| JSONL Mirror | `mpm/src/db/mirror.jsonl` |

## Environment Variables

```bash
export MPM_WORKSPACE="/custom/workspace"
export MPM_MEMORY_DIR="/custom/memory/watch"   # Watch input only
export MPM_SESSIONS_DIR="/custom/sessions/watch" # Watch input only
```
