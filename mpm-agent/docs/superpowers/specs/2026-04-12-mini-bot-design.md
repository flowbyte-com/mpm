# mini-bot Design Specification

**Date:** 2026-04-12
**Status:** Approved for implementation

---

## Overview

mini-bot is a self-improving AI agent with a stable core identity (`IDENTITY.md`), its own SQLite database for memories/sessions/lessons, and access to MPM tools via a shared MCP server. It is designed to avoid identity drift by keeping its core persona separate from MPM persona overlays (which act as costumes, not identity).

---

## Priority

All features are implemented in priority order:

| Priority | Feature | Rationale |
|----------|---------|-----------|
| **1** | Memory anchoring | Mark important moments in real-time, not retroactively |
| **2** | Task handoff | Multi-step execution with checkpoints for complex work, recoverable errors, visible thinking |
| **3** | File-aware tools | Semantic file understanding, not raw bytes |
| **4** | Web fetch + synthesis | Fetch + cite + synthesize, nice-to-have layering on existing web_search |

---

## 1. Naming & Process Identity

### Binaries

| Old Name | New Name | Purpose |
|----------|----------|---------|
| `mpm-agent` | `mini-bot` | CLI entry point (REPL + single-shot) |
| `mpm-agent-telegram` | `mini-bot-telegram` | Telegram bridge daemon |
| `mpm-agent-mcp` | `mini-bot-mcp` | MCP server (shared between Claude Code and mini-bot) |

### Startup Logging

Each process prints its name and PID on startup:

```
mini-bot[1234]: Starting...
mini-bot[1234]: Loaded identity: "MiniBot" v1.0
mini-bot[1234]: Database: mini-bot.db
mini-bot[1234]: MCP socket: mini-bot-mcp.sock
mini-bot[1234]: Ready
```

Process name is set via `os.Args[0] = "mini-bot"` at program start.

---

## 2. IDENTITY.md System

**Purpose:** Stable core identity that never changes at runtime. MPM personas are costumes layered on top.

### Location

`IDENTITY.md` is read from the **same directory as the running binary** at startup, and can be hot-reloaded on demand.

### Format

```markdown
# MiniBot v1.0
Type: helpful assistant
Domain: general-purpose
Core traits: patient, thorough, cites sources, asks clarifying questions when unsure
Boundaries: never fabricate facts, always admit uncertainty, no political advice
```

### Startup Behavior

1. Read `IDENTITY.md` from binary directory
2. Establish core identity as the stable foundation
3. Check MPM's `active_persona` state file (from MPM's state dir)
4. If MPM persona is set, load it as a **costume overlay** (added to system prompt as "wearing persona: X")
5. Core identity + costume = full system prompt
6. Log identity summary on startup

**Identity drift prevention:** The core identity in IDENTITY.md is the bot's true self. MPM personas never overwrite it — they are temporary costumes that can be removed. The bot always knows who it is.

### Reload Behavior

IDENTITY.md is loaded at startup and kept stable during conversations. Three reload triggers:
- **Startup only** — full load into memory
- **On-demand** — `mini-bot reload identity` command reloads from disk
- **Hot reload** — if `identity_hot_reload: true` in config, re-reads file on change (via fsnotify)

This keeps identity stable mid-conversation but allows evolution between sessions.

### Identity Forking (Branching)

mini-bot can **fork its identity** to create a variant persona for a specific task or experiment:

```
mini-bot fork <branch-name>     Create a new identity branch from current IDENTITY.md
mini-bot fork <branch-name> --from <source>   Fork from a specific branch
mini-bot switch <branch-name>    Switch active identity to a branch
mini-bot branch list             List all identity branches
mini-bot branch diff <a> <b>     Compare two branches
mini-bot branch promote <branch>   Promote branch to main (with approval)
```

**Storage:**
- `IDENTITY.md` — main branch (stable)
- `IDENTITIES/` directory — branch variants
  ```
  IDENTITIES/
  ├── main.md          ← current IDENTITY.md
  ├── analyst-v2.md    ← fork for testing
  └── critic-branch.md
  ```
- `identities.json` — branch metadata (parent, created_at, status: active|promoted|deprecated)

**Fork flow:**
1. `mini-bot fork experiment-v1` — copies current IDENTITY.md to `IDENTITIES/experiment-v1.md`
2. Bot runs with `experiment-v1` identity variant
3. If variant works better, `mini-bot branch promote experiment-v1` — merges changes into main
4. If not, `mini-bot branch delete experiment-v1` — discards variant

This gives git-branch-style identity development with approval gates for promotion.

---

## 3. Environment Isolation

mini-bot runs as **separate instances per environment** (alpha, beta, production). Each instance has its own:

- `mini-bot.db` — independent memory, sessions, lessons, anchors
- `IDENTITY.md` — instance-specific identity (can fork/promote independently)
- `mini-bot-config.json` — instance-specific config

**Why separate:** Alpha breaks → Beta catches → Production is sacred. Clean isolation means experimental self-improvement in alpha never pollutes production.

**Cross-env promotion:** Identity branches can be promoted across environments by copying `IDENTITIES/<branch>.md` files and merging.

---

## 4. Database Schema (mini-bot.db)

```sql
-- Core memories with FTS5 search
CREATE TABLE memories (
    id TEXT PRIMARY KEY,
    collection TEXT DEFAULT 'general',
    content TEXT NOT NULL,
    session_id TEXT,
    tags TEXT,           -- JSON array
    metadata TEXT,       -- JSON object
    created_at TEXT,
    deleted_at TEXT
);
CREATE VIRTUAL TABLE memories_fts USING fts5(content, tags, content=memories, content_rowid=rowid);
CREATE TRIGGER memories_ai AFTER INSERT ON memories BEGIN INSERT INTO memories_fts(rowid, content, tags) VALUES (new.rowid, new.content, new.tags); END;
CREATE TRIGGER memories_ad AFTER DELETE ON memories BEGIN INSERT INTO memories_fts(memories_fts, rowid, content, tags) VALUES (delete, old.rowid, old.content, old.tags); END;
CREATE TRIGGER memories_au AFTER UPDATE ON memories BEGIN INSERT INTO memories_fts(memories_fts, rowid, content, tags) VALUES (delete, old.rowid, old.content, old.tags); INSERT INTO memories_fts(rowid, content, tags) VALUES (new.rowid, new.content, new.tags); END;

-- Sessions for conversation history
CREATE TABLE sessions (
    id TEXT PRIMARY KEY,
    session_id TEXT,
    content TEXT,
    content_hash TEXT,
    created_at TEXT,
    source_path TEXT,
    metadata TEXT
);

-- Learned lessons
CREATE TABLE lessons (
    id TEXT PRIMARY KEY,
    content TEXT NOT NULL,
    type TEXT DEFAULT 'insight',
    tags TEXT,
    created_at TEXT,
    reinforcement_count INTEGER DEFAULT 1
);

-- Tool registry (self-generated tools)
CREATE TABLE tools (
    id TEXT PRIMARY KEY,
    name TEXT UNIQUE,
    description TEXT,
    definition TEXT,
    source TEXT DEFAULT 'self',
    created_at TEXT
);

-- Anchored moments (high-priority memories)
CREATE TABLE anchors (
    id TEXT PRIMARY KEY,
    content TEXT NOT NULL,
    context TEXT,
    weight INTEGER DEFAULT 1,
    session_id TEXT,
    created_at TEXT
);
```

---

## 4. Config File (mini-bot-config.json)

All configuration in one file, alongside the binary.

```json
{
  "identity": {
    "name": "mini-bot",
    "version": "1.0"
  },
  "synth": {
    "model": "MiniMax-M2.7",
    "api_key": "",
    "base_url": "https://api.minimax.io/anthropic",
    "max_tokens": 4096,
    "timeout_seconds": 300
  },
  "telegram": {
    "bot_token": "",
    "polling": true,
    "allowed_users": []
  },
  "mcp": {
    "token": "",
    "socket_path": "mini-bot-mcp.sock"
  },
  "self_improve": {
    "enabled": true,
    "anchor_threshold": 3,
    "lesson_complexity_threshold": 7,
    "identity_patch_approval_required": true
  },
  "identity": {
    "hot_reload": false
  },
  "paths": {
    "db": "mini-bot.db",
    "identity": "IDENTITY.md",
    "mcp_socket": "mini-bot-mcp.sock"
  }
}
```

---

## 5. Shared MCP Server

`mini-bot-mcp` is a single MCP server that serves both Claude Code and mini-bot via a Unix socket.

```
Claude Code ---> mini-bot-mcp ---> mpm.db
mini-bot     ---> mini-bot-mcp ---> mpm.db  (queued when Claude Code connected)
```

### Protocol

- JSON-RPC over Unix socket
- Token auth via `MPM_API_TOKEN` env var (same as mpm-agent)
- Existing mpm-agent-mcp tools are exposed to mini-bot

### Connection Flow (mini-bot side)

1. Connect to `mini-bot-mcp.sock`
2. Send `initialize` with token if required
3. Call `tools/call` for MPM tool access
4. On disconnect, release socket

---

## 6. Self-Improvement Loop

### Priority 1: Memory Anchoring

**Automatic, real-time marking of important moments.**

How it works:
- During conversation, track interaction "weight" (complexity, novelty, user signals)
- When weight exceeds `anchor_threshold` (default 3), anchor the moment to mini-bot.db
- Anchored memories are high-priority context on startup and session resume

```sql
INSERT INTO anchors (id, content, context, weight, session_id, created_at)
VALUES (?, ?, ?, ?, ?, ?)
```

Anchors are loaded as context on every RunAgent call.

### Priority 2: Task Handoff

**Multi-step execution with checkpoint summaries.**

New tool: `task_execute(steps)`

```json
{
  "steps": [
    {"tool": "shell", "args": {"command": "ls -la"}},
    {"tool": "read_file", "args": {"path": "/tmp/report.md"}},
    {"checkpoint": "files listed and read"}
  ]
}
```

Behavior:
- Executes steps sequentially
- Each checkpoint stores a summary in the session
- If a step fails, execution stops and returns results so far
- Full results returned at end

### Priority 3: File-Aware Tools

**Semantic file understanding, not raw bytes.**

`read_file` enhanced:
- Default: return full content (truncated at 2000 chars for safety)
- New `mode` param: `"summary"` | `"code"` | `"compare"` | `"full"`
- `summary`: key points and structure
- `code`: extract and annotate code snippets with line references
- `compare`: compare two files, highlight differences
- `full`: complete content

### Priority 4: Web Fetch + Synthesis

**Fetch URL, extract structured content, cite properly.**

New tool: `web_synthesize(query)`
- Fetches top results for query
- Extracts key facts, titles, citations
- Produces synthesized answer with sources

---

## 7. Self-Improvement Mechanisms

### Mechanism 1: Memory Anchoring (auto)

- Track interaction weight during conversation
- When threshold exceeded, write to `anchors` table
- Anchors loaded as high-priority context on every call

### Mechanism 2: Lesson Extraction (auto)

- On session end (or every N messages), if complexity > `lesson_complexity_threshold`
- Generate a lesson and write to `lessons` table
- Recall lessons on similar future tasks

### Mechanism 3: Identity Patching (approval required)

- When bot learns something fundamental, write proposed patch to `IDENTITY_PATCH.md`
- Notify user: "I've learned something that might improve my identity. Review? [y/N]"
- If approved, merge into `IDENTITY.md`

### Mechanism 4: Tool Registry Expansion

- Bot can write new tool definitions to `tools` table
- On next startup, load self-generated tools into tool set
- Marked `source: 'self'` vs `source: 'mpm'`

---

## 8. Architecture

```
mini-bot CLI REPL / mini-bot-telegram
         |
         v
   +------------------+
   |    mini-bot      |  <-- IDENTITY.md (stable core)
   |   (agent loop)   |  <-- mini-bot-config.json
   +------------------+
         |                 mini-bot.db
         |              (memories, sessions,
         |               lessons, anchors, tools)
         v
   +------------------+
   |  mini-bot-mcp   |  <-- shared via socket
   |  (JSON-RPC)     |      token auth
   +------------------+
         |
         v
   +------------------+
   |  mpm-agent-mcp  |  <-- mpm.db (MPM tools)
   +------------------+      (existing)
```

---

## 9. File Layout

All files in the binary directory (e.g., `~/mpm/bin/`):

```
mini-bot               (CLI entry point)
mini-bot-telegram      (Telegram bridge daemon)
mini-bot-mcp           (MCP server - shared)
IDENTITY.md            (bot's stable core identity)
mini-bot-config.json   (all configuration)
mini-bot.db            (SQLite: memories, sessions, lessons, anchors, tools)
mini-bot-mcp.sock      (MCP socket - created at runtime)
IDENTITY_PATCH.md      (proposed identity changes - created when needed)
```

---

## 10. Implementation Priority

1. **Memory anchoring** — core loop, immediate value
2. **Task handoff** — complex work support
3. **File-aware tools** — semantic reading
4. **Web synthesis** — knowledge layer
5. **Identity patching** — gradual self-improvement
6. **Tool registry** — earned expansion