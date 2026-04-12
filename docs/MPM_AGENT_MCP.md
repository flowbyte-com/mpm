# MPM Agent MCP Server

## Overview

The MPM Agent MCP server exposes MPM memory/session/search tools to Claude Code as MCP tools. It bridges Claude Code and the MPM SQLite database, allowing Claude to search and store memories, manage sessions, and access reference documents.

## Binary

```
mpm-agent/bin/mpm-agent-mcp
```

Built from `mpm-agent/cmd/mcp/` with `make mcp`.

## Claude Code Configuration

In `~/.claude/settings.json`:

```json
"mcpServers": {
  "mpm": {
    "command": "/home/v/.openclaw/workspace/flowbyte/mpm/mpm-agent/bin/mpm-agent-mcp",
    "env": {
      "MPM_WORKSPACE": "/home/v/.openclaw/workspace/flowbyte/mpm",
      "MPM_API_TOKEN": "<token from mpm_config.json>"
    }
  }
}
```

Claude Code auto-connects to configured MCP servers on startup.

## Authentication

The MCP server supports token-based authentication:

1. Add `api_token` to `synth` section in `mpm_config.json`:
```json
{
  "synth": {
    "model": "MiniMax-M2.7",
    "api_key": "...",
    "base_url": "https://api.minimax.io",
    "api_token": "your-secure-token-here"
  }
}
```

2. Set the same token in Claude Code settings:
```json
"env": {
  "MPM_API_TOKEN": "your-secure-token-here"
}
```

3. Rebuild the MCP server after changing the config:
```bash
cd mpm-agent
go build -o bin/mpm-agent-mcp ./cmd/mcp/
```

If no `api_token` is set in config, the MCP server runs without authentication.

## Path Resolution

The MCP server resolves `src/db/mpm.db` using the same logic as MPM itself:

1. `MPM_WORKSPACE` env var if set and path ends in `mpm` → `MPM_WORKSPACE/src/db/mpm.db`
2. Otherwise `MPM_WORKSPACE + "/mpm/src/db/mpm.db"`
3. Executable-relative traversal (up to 4 dirs up looking for `mpm/`)
4. CWD fallback

**IMPORTANT**: `MPM_WORKSPACE` must point to the directory *containing* the `mpm` subdirectory (i.e., the workspace root), not the `mpm` source directory itself.

## Available Tools

### Memory Tools

| Tool | Description |
|------|-------------|
| `mpm_memory_search` | Search memories via FTS5 (falls back to LIKE) |
| `mpm_memory_add` | Add a memory to long-term store |
| `mpm_memory_delete` | Soft-delete a memory |

### Session Tools

| Tool | Description |
|------|-------------|
| `mpm_session_search` | Search session history for context |
| `mpm_session_update` | Update/summarize a session |

### Reference & Lessons

| Tool | Description |
|------|-------------|
| `mpm_reference_search` | Search reference documents |
| `mpm_lesson_search` | Search lessons |
| `mpm_lesson_add` | Add a lesson |

### Mode & Persona

| Tool | Description |
|------|-------------|
| `mpm_mode_list` | List available modes |
| `mpm_mode_set` | Set active mode |
| `mpm_persona_list` | List available personas |
| `mpm_persona_set` | Set active persona |

### Directives

| Tool | Description |
|------|-------------|
| `mpm_directive_list` | List prime directives |
| `mpm_directive_add` | Add a prime directive |

### Utility

| Tool | Description |
|------|-------------|
| `mpm_synthesize` | Trigger session synthesis |

### General Tools

| Tool | Description |
|------|-------------|
| `read_file` | Read file contents |
| `write_file` | Write file contents |
| `shell` | Execute shell command |
| `web_search` | Search the web |
| `web_fetch` | Fetch URL content |

## Database Schema

The MCP server uses MPM's SQLite database at `src/db/mpm.db`:

- `memories` - long-term memory storage
- `sessions` - conversation session history
- `references` - reference documents
- `lessons` - learned lessons
- `modes` - mode configurations
- `personas` - persona configurations

## FTS5 Limitation

This SQLite build (`go-sqlite3` / mattn/go-sqlite3) was compiled **without FTS5 support**. Searches attempt FTS5 first and fall back to LIKE automatically.

## Rebuilding

```bash
cd mpm-agent
make mcp       # Build MCP server
make telegram  # Build Telegram bridge
make build     # Build standalone agent CLI
```

Rebuild after any change to `cmd/mcp/` or `core/agent.go`.
