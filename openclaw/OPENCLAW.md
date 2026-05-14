# MPM + OpenClaw Integration

MPM (Memory Persistence Module) integrates with OpenClaw as a plugin, exposing the full Go backend capability set as native function-calling tools. The plugin calls the Go binary directly via `child_process` — no MCP intermediary.

---

## What Gets Installed

| File | Location |
|------|----------|
| Plugin source | `/home/v/workspace/projects/mpm/openclaw/mpm-plugin/src/` |
| Bundled entry | `/home/v/workspace/projects/mpm/openclaw/mpm-plugin/dist/index.js` |
| Plugin config | `/home/v/workspace/projects/mpm/openclaw/mpm-plugin/openclaw.plugin.json` |

---

## Prerequisites

- MPM binary built with FTS5 support (MPM v2.0+)
- OpenClaw Gateway `>= 2026.3.24-beta.2`
- Node.js (runtime for OpenClaw)

---

## Step 1 — Build MPM

```bash
cd /home/v/workspace/projects/mpm
PATH=/usr/local/go/bin:$PATH make build
# Produces: bin/mpm
```

Keep the system binary in sync:
```bash
cp /home/v/workspace/projects/mpm/bin/mpm /usr/local/bin/mpm
```

---

## Step 2 — Install the Plugin

The plugin lives at `/home/v/workspace/projects/mpm/openclaw/mpm-plugin/`. It is already configured; no manual creation needed.

### `openclaw.plugin.json` (already configured)

```json
{
  "id": "mpm",
  "name": "MPM (Memory Persistence Module)",
  "description": "Long-term memory tool for 808 — full SQLite-backed persistence layer with lessons, topics, references, and directives.",
  "version": "2.0.0",
  "contracts": {
    "tools": [
      "query_long_term_memory",
      "save_to_memory",
      "save_lesson",
      "search_lessons",
      "list_lessons",
      "create_topic",
      "search_topics",
      "add_reference",
      "search_references",
      "list_references",
      "read_directives"
    ]
  },
  "activation": {
    "onStartup": true
  },
  "configSchema": {
    "type": "object",
    "additionalProperties": false,
    "properties": {
      "binary": {
        "type": "string",
        "description": "Path to the MPM binary (default: /home/v/workspace/projects/mpm/bin/mpm)",
        "default": "/home/v/workspace/projects/mpm/bin/mpm"
      },
      "workspace": {
        "type": "string",
        "description": "MPM_WORKSPACE path (default: /home/v/workspace/projects/mpm)",
        "default": "/home/v/workspace/projects/mpm"
      }
    }
  }
}
```

---

## Step 3 — Configure `openclaw.json`

Add to `plugins.entries` and `plugins.load.paths`:

```json
{
  "plugins": {
    "entries": {
      "mpm": {
        "enabled": true,
        "config": {
          "binary": "/home/v/workspace/projects/mpm/bin/mpm",
          "workspace": "/home/v/workspace/projects/mpm"
        }
      }
    },
    "load": {
      "paths": [
        "/home/v/workspace/projects/mpm/openclaw/mpm-plugin"
      ]
    }
  },
  "tools": {
    "profile": "coding"
  }
}
```

**Note:** Do **not** use an explicit `tools.allow` array — it blocks dynamic plugin tool hydration and crashes subagents. Let the Gateway inject tools dynamically via the profile.

---

## Step 4 — Verify

### Check plugin is loaded

```bash
openclaw plugins list
# Should show: mpm | enabled | openclaw | .../mpm-plugin/dist/index.js
```

### Inspect runtime tools

```bash
openclaw plugins inspect mpm --runtime --json | python3 -c "
import sys, json
d = json.load(sys.stdin)
p = d['plugin']
print('Tool names:', p.get('toolNames', []))
print('Status:', p.get('status'))
print('Activated:', p.get('activated'))
"
```

Expected output:
```
Tool names: ['query_long_term_memory', 'save_to_memory', 'save_lesson', 'search_lessons',
             'list_lessons', 'create_topic', 'search_topics', 'add_reference',
             'search_references', 'list_references', 'read_directives']
Status: loaded
Activated: True
```

### Restart Gateway

Config changes to plugin tool exposure require a Gateway restart:
```bash
openclaw gateway restart
```

---

## Tool Reference (11 tools total)

### Core Memory (2 tools — original)

**`query_long_term_memory`**
```
query: "any natural language search"
limit: 5  (optional, default 5)
```

**`save_to_memory`**
```
fact: "The thing to remember"
tags: ["tag1", "tag2"]  (optional)
weight: 0.5  (optional, 0.0-1.0)
ttl: "24h"  (optional, '0' for permanent)
```

### Lessons (3 tools)

**`save_lesson`** — Store a learned insight, warning, or practice
```
fact: "What was learned or observed"
type: "warning" | "practice" | "insight"  (default: insight)
tags: ["tag1", "tag2"]  (optional)
```

**`search_lessons`** — Find lessons by natural language query
```
query: "search string"
```

**`list_lessons`** — List all stored lessons
```
type: "warning" | "practice" | "insight"  (optional filter)
```

### Topics (2 tools)

**`create_topic`** — Create a named memory cluster
```
name: "Topic name"
description: "Optional description"
```

**`search_topics`** — Find topics by name or description
```
query: "search string"
```

### References (3 tools)

**`add_reference`** — Ingest a document (PDF, EPUB, MD, TXT, HTML)
```
filepath: "/absolute/path/to/document"  (required)
title: "Optional title"  (optional)
```

**`search_references`** — Full-text search within ingested documents
```
query: "search string"
limit: 5  (optional, default 5)
```

**`list_references`** — List all ingested documents
```
{}  (no parameters)
```

### Directives (1 tool)

**`read_directives`** — Retrieve prime directives (operating principles)
```
{}  (no parameters)
```

---

## Agent Configuration (`AGENTS.md`)

Add this to the agent's `AGENTS.md` to enforce memory discipline:

```markdown
## MPM — Memory Persistence Module (Mandatory)

MPM is your native SQLite-backed persistence layer. You are directly wired into it via your tool schema. It is **not** optional context — it is your primary brain.

**CRITICAL: Mandatory Recall**
Before answering *any* question about prior work, decisions, dates, people, preferences, or todos, you MUST execute the `query_long_term_memory` tool. NEVER claim you don't know something or guess an answer without querying your memory first.
*Note: Use `memory_get` only when a result points to a specific file.*

**CRITICAL: Mandatory Save**
After any non-trivial action, lesson learned, or architectural decision, you MUST execute the `save_to_memory` tool.
- **Tags:** Help later retrieval — use them heavily.
- **Weight:** 0.5 default, higher for important absolute truths.
- **TTL '24h':** Ephemeral session-scoped facts (e.g., current task context).
- **TTL '0':** Permanent memories (user preferences, project state, architectural decisions).

**CRITICAL: Structured Memory**
For lessons, topics, references, and directives — use the dedicated tools:
- `save_lesson` for insights, warnings, and practices
- `create_topic` / `search_topics` for memory clustering
- `add_reference` / `search_references` for ingested documents
- `read_directives` for operating principles

## Session Startup

On every session start, before responding to the user:
```bash
bash /home/v/workspace/scripts/mpm-startup-recall.sh
```
Then read `memory/startup-context.md` — recent memories, lessons, and LTM fragments. Files stay authoritative for identity. MPM holds everything else.
```

---

## Troubleshooting

**Tools don't appear in agent schema / "No callable tools remain" error**

Check your `openclaw.json` config. If you are using an explicit `tools.allow` array, it will block dynamic plugin hydration and crash subagents. Remove the `"allow"` array entirely from your `"tools"` block to let the Gateway dynamically inject loaded plugin tools.

**"No such module: fts5" on memory operations**

Rebuild MPM with FTS5 enabled:
```bash
cd /home/v/workspace/projects/mpm
PATH=/usr/local/go/bin:$PATH make build
```

**DB trigger constraint errors**

Stale debug triggers sometimes accumulate. Check:
```bash
sqlite3 /home/v/workspace/projects/mpm/src/db/mpm.db ".triggers"
```
Drop any `test_ai2` or `dbg_ai2` triggers.

**Plugin registered but child_process calls fail**

The dangerous code scanner may block `child_process`. Use `--dangerously-force-unsafe-install` flag when installing, or configure scanner allowlist for the plugin directory.

**`reference ls` returns "no such table: references"**

This was a pre-existing bug — the SQL queries referenced a table named `"references"` but the actual table is `reference_docs`. Fixed in MPM v2.0 build. Rebuild if still seeing this error.