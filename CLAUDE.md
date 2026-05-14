# CLAUDE.md

This file provides guidance to Claude Code (claude.ai/code) when working with code in this repository.

## Project Overview

This is the **MPM** monorepo — Memory Persistence Module — Agent-owned SQLite brain. No server, no daemon, just persistence.:

| Directory | Language | Description |
|-----------|----------|-------------|
| `mpm/` | Go | SQLite-native agent state management (memory layer) |
| `mpm/mpm-agent/` | Go | Self-improving AI agent (CLI REPL, Telegram bot, MCP server) |

## Building & Testing

**MPM (primary):**
```bash
cd /home/v/workspace/projects/mpm
make build    # Build to bin/mpm
make test     # Run tests
make install  # Install to /usr/local/bin/mpm
```

**Single test:** `go test -v ./internal/... -run TestFunctionName`

**mpm-agent:**
```bash
cd /home/v/workspace/projects/mpm/mpm-agent
make build BIN=bin/mini-bot        # Build specific binary
make build                         # Build all 3 binaries
CGO_CFLAGS="-DSQLITE_ENABLE_FTS5" CGO_LDFLAGS="-lm" go test ./core/...
```

FTS5 must be enabled via `CGO_CFLAGS="-DSQLITE_ENABLE_FTS5"`.

## MPM Architecture

**Single-process, shared-database model.** No socket IPC, no separate watcher process. Commands execute directly in the same process. The file watcher runs as a background goroutine within the same binary (or as a detached child with `--bg` for systemd).

```
┌─────────────────────────────────────────────┐
│                   mpm binary                  │
│  ┌──────────┐   ┌──────────┐   ┌──────────┐ │
│  │   CLI    │──▶│  Router  │──▶│ Handlers │ │
│  └──────────┘   └──────────┘   └────┬─────┘ │
│                                      │       │
│  Background: WorkerPool (3 goros) + fsnotify │
│                  watcher             │       │
│                         ┌────────────▼────┐ │
│                         │  DBManager      │ │
│                         │  (SQLite + WAL) │ │
│                         └────────────┬────┘ │
│              ┌───────────────────────┘      │
│              ▼                               │
│         src/db/mpm.db                        │
└─────────────────────────────────────────────┘
```

**Key architectural points:**
- All access via one `DatabaseManager` instance sharing a single SQLite connection with WAL mode
- Fixed-size goroutine pool (default 3 workers) processes file watcher events concurrently
- PID file (`watch.pid`) for inter-process communication with detached mode

**Memory model:** Everything is a **memory**. Collections distinguish types:
- `memories` — general facts/knowledge
- `session` — session-derived facts
- LTM (long-term memory) = weight ≥ 10, promoted via `mpm promote` or auto-ingested `.md` files

**Relevance scoring:** `score = (reinforcement_count × 2) + (weight × 1.5) + recency_bonus`

**Path resolution:** `MPM_WORKSPACE` env var → `~/.mpm/` → current working directory

## mpm-agent Architecture

Three entry points: `mini-bot` (CLI REPL), `mini-bot-telegram` (Telegram daemon), `mini-bot-mcp` (MCP server).

**Identity-first system prompt** (`BuildSystemPromptWithIdentity` in `core/agent.go`):
Priority: IDENTITY.md → anchored memories → memories → directives → references

**Toolkit lazy-loading:**
- Tool profiles (`standard`, `coding`) define base tools
- Toolkits (`files`, `web`, `mpm`, `shell`, etc.) load dynamically via `load_toolkit()`
- Four base tools always registered: `list_toolkits`, `load_toolkit`, `unload_toolkit`, `execute_mpm_command`

**Self-improvement system** (`core/selfimprove.go`):
1. Memory anchoring — user messages >50 chars anchor as high-priority context
2. Lesson extraction — exchanges create lessons with reinforcement tracking
3. Identity patching — bot proposes changes to `IDENTITY_PATCH.md`, human approves

**Identity branching** (`core/identity_fork.go`): Full fork/promote system for identity versions.

## Database

**MPM:** Single `src/db/mpm.db` (WAL mode). Tables: `memories`, `sessions`, `topics`, `modes`, `personas`, `lessons`, `reference_docs`, `reference_chunks`.

**mpm-agent:** `mini-bot.db` shared by session manager and agent. Schema defined in both `core/db.go:InitMiniBotDB` and `cmd/telegram/session.go:initSessionDB` (must remain in sync).

## Security

All content scanned against 20 regex patterns (API keys, JWTs, SSH keys, `password=`, etc.) before database writes. Blocked content logged to `mirror.jsonl`, never the database.

## Gotchas

- `mpm watch start --bg` spawns a detached child — parent exits immediately; child blocks on `select{}` until signaled
- `cleanResponse()` in Telegram handler strips thinking blocks (`<thinking>`, `《》`, `（）》`) — other entry points see raw blocks
- `selfImprove()` in Telegram handler opens its own db connection (goroutine, runs after response)
- `mini-bot.db` is opened independently by session manager and agent — SQLite handles concurrent reads; writes retry with exponential backoff
- Both `core/db.go` and `cmd/telegram/session.go` define the full schema — must stay in sync
