# MPM + OpenClaw Integration

Integrating MPM as an OpenClaw plugin gives any OpenClaw agent direct access to the MPM memory layer via two native function-calling tools: `query_long_term_memory` and `save_to_memory`.

This is distinct from the mpm-agent MCP approach (see `docs/archive_v1/MPM_AGENT.md`). The OpenClaw plugin calls the Go binary directly via `child_process` — no MCP intermediary.

---

## Agent Configuration (`AGENTS.md`)

The plugin provides the tools. The agent configuration controls when and how they are used. Set these directives in the agent's `AGENTS.md` (or workspace bootstrap file) to enforce the memory discipline:

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

## Memory

- **Running context:** MPM SQLite — sessions, lessons, modes, personas
- **Immediate:** Use your `save_to_memory` tool to capture learnings in real-time.

When you learn something worth keeping: add it to MPM immediately via your native tools. Text survives restarts. MPM is the source of truth for agent state.

## Session Startup

On every session start, before responding to the user:
```bash
bash /home/v/workspace/scripts/mpm-startup-recall.sh
```
Then read `memory/startup-context.md` — recent memories, lessons, and LTM fragments. Files stay authoritative for identity. MPM holds everything else.
```

These directives ensure the LLM routes via the tool schema (matching `query_long_term_memory` / `save_to_memory` directly) rather than falling back to shell exec.

---

## What Gets Installed

| File | Location |
|------|----------|
| Plugin source | `~/.openclaw/workspace/projects/mpm-plugin/` |
| Bundled entry | `~/.openclaw/workspace/projects/mpm-plugin/dist/index.js` |
| Plugin config | `~/.openclaw/openclaw.json` |

---

## Prerequisites

- MPM binary built with FTS5 support
- OpenClaw Gateway `>= 2026.3.24-beta.2`
- Node.js (runtime for OpenClaw)

---

## Step 1 — Build MPM

```bash
cd /home/v/workspace/projects/mpm
PATH=/usr/local/go/bin:$PATH make build
# Produces: bin/mpm
```

Keep the system binary in sync if you use a different path in the plugin config:
```bash
cp /home/v/workspace/projects/mpm/bin/mpm /usr/local/bin/mpm
```

---

## Step 2 — Install the Plugin

If the plugin directory doesn't exist yet, create it:

```bash
mkdir -p ~/.openclaw/workspace/projects/mpm-plugin/src
```

### `package.json`

```json
{
  "name": "@myorg/openclaw-mpm",
  "version": "1.0.0",
  "type": "module",
  "main": "./dist/index.js",
  "openclaw": {
    "extensions": ["./dist/index.js"],
    "compat": {
      "pluginApi": ">=2026.3.24-beta.2",
      "minGatewayVersion": "2026.3.24-beta.2"
    },
    "build": {
      "openclawVersion": "2026.3.24-beta.2",
      "pluginSdkVersion": "2026.3.24-beta.2"
    }
  }
}
```

### `openclaw.plugin.json`

```json
{
  "id": "mpm",
  "name": "MPM (Memory Persistence Module)",
  "description": "Long-term memory tool for 808 — query and save memories via the agent's own SQLite-backed persistence layer.",
  "version": "1.0.0",
  "contracts": {
    "tools": ["query_long_term_memory", "save_to_memory"]
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

### `src/index.js` (source)

The plugin calls the Go binary directly via `child_process.spawn`. See `dist/index.js` for the compiled output. Key points:

- Uses OpenClaw plugin SDK: `import { definePluginEntry } from "openclaw/dist/plugin-sdk/plugin-entry.js"`
- Two tools registered: `query_long_term_memory`, `save_to_memory`
- Parameter schemas are plain JSON Schema (compatible with all LLM runtimes)
- Binary path and workspace passed through plugin config or env vars (`MPM_BINARY`, `MPM_WORKSPACE`)

**Important:** The dangerous code scanner blocks `child_process` by default. Build with:
```bash
openclaw plugins install --dangerously-force-unsafe-install
```
Or whitelist the plugin directory in the scanner config.

---

## Step 3 — Configure `openclaw.json`

Add to `plugins.entries`, `plugins.load.paths`, and `tools.allow`:

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
        "/home/v/.openclaw/workspace/projects/mpm-plugin"
      ]
    }
  },
  "tools": {
    "profile": "coding"
  }
}
```

---

## Step 4 — Verify

### Check plugin is loaded

```bash
openclaw plugins list
# Should show: mpm | enabled | openclaw | ~/workspace/projects/mpm-plugin/dist/index.js
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
Tool names: ['query_long_term_memory', 'save_to_memory']
Status: loaded
Activated: True
```

### Restart Gateway

Config changes to plugin tool exposure require a Gateway restart:
```bash
openclaw gateway restart
```

---

## Usage

Once loaded, the agent can call:

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

---

## Troubleshooting

**Tools don't appear in agent schema / "No callable tools remain" error**

Check your `openclaw.json` config. If you are using an explicit `tools.allow` array, it will block dynamic plugin hydration and crash the subagents.

The Fix: Remove the `"allow"` array entirely from your `"tools"` block to let the Gateway dynamically inject the loaded plugin tools into your active profile. If you must use an allowlist for security, ensure you prefix the tools with the plugin namespace (e.g., `"mpm.query_long_term_memory"`, `"mpm.save_to_memory"`).

**"No such module: fts5" on memory operations**

Rebuild MPM with FTS5 enabled:
```bash
cd /home/v/workspace/projects/mpm
PATH=/usr/local/go/bin:$PATH make build
```

**DB trigger constraint errors**

Stale debug triggers sometimes accumulate. Check:
```bash
sqlite3 mpm.db ".triggers"
```
Drop any `test_ai2` or `dbg_ai2` triggers.

**Plugin registered but child_process calls fail**

The dangerous code scanner may block `child_process`. Use `--dangerously-force-unsafe-install` flag when installing, or configure scanner allowlist for the plugin directory.
