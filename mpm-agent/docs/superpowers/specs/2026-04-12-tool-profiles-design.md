# Tool Profiles + Toolkit Lazy-Loading Design — mini-bot Telegram

## Overview

Add a tool profile system to mini-bot Telegram. Profiles are named collections of **base tools** (framework operations). The actual implementation tools are organized into **Toolkits**, which are dynamically loaded by the AI at runtime via `load_toolkit()` calls.

This is the **Toolkit Lazy-Loading** pattern — the AI only sees the tools it has loaded, keeping token costs low.

---

## Base Tools (Always Available)

These 4 tools are always in context, regardless of toolkit state:

| Tool | Args | Description |
|------|------|-------------|
| `list_toolkits` | — | List available toolkits and their load status |
| `load_toolkit` | `name` | Load a toolkit to unlock its tools |
| `unload_toolkit` | `name` | Unload a toolkit to free context space |
| `execute_mpm_command` | `command` | Execute an MPM CLI command (e.g. `recall hello` runs `mpm recall hello`) |

---

## Available Toolkits

Toolkits are collections of tools loaded on demand. The AI calls `load_toolkit("name")` to activate one.

| Toolkit | Tools | Description |
|---------|-------|-------------|
| `files` | `read_file`, `write_file`, `ReadFileSemantic`, `ReadFileCompare` | File operations scoped to `mpm-agent/` tree |
| `web` | `WebSynthesize` | DuckDuckGo search + synthesis with citations |
| `jq` | `jq` | Query JSON files with jq filters (*.json, *.jsonl only) |
| `mpm` | *(via execute_mpm_command)* | All MPM CLI commands accessible |

### File Path Restriction

All file tools are scoped to the `mpm-agent/` directory tree. Absolute paths and `..` escapes outside the sandbox are rejected.

---

## Profile Switching

**`/tools`** command (Telegram):

```
Current: standard

Available profiles:
  /tools standard ✓
  /tools mcp-admin*
  /tools research*

Use /tools <name> to switch.
```

Profiles are stored per-chat in `chatSettings`. The AI also manages toolkit loading dynamically via `load_toolkit()` calls — no `/tools` command needed for that.

---

## Architecture

### Tool Registry

`core/tools.go`:
```go
type ToolDefinition struct {
    Name        string
    Description string
    InputSchema map[string]interface{}
}

var toolRegistry = make(map[string]ToolDefinition)  // all registered tools
var LoadedToolkits = make(map[string]map[string]bool) // sessionID → toolkit name → true
```

### Config Structure

`core/config.go`:
```go
type MiniBotConfig struct {
    ...
    Profiles  map[string][]string  // profile name → base tool names
    Toolkits  map[string][]string  // toolkit name → tool names
}
```

Default config:
```go
Profiles: map[string][]string{
    "standard": {"list_toolkits", "load_toolkit", "unload_toolkit", "execute_mpm_command"},
},
Toolkits: map[string][]string{
    "files": {"read_file", "write_file", "ReadFileSemantic", "ReadFileCompare"},
    "web":  {"WebSynthesize"},
    "jq":   {"jq"},
    "mpm":  {},
},
```

### Toolkit Interception in Tool Loop

`RunAgent` in `core/agent.go` intercepts `load_toolkit` and `unload_toolkit` locally — **no API call is made**. These are framework operations:

```
LLM calls load_toolkit("files")
         ↓
Agent loop intercepts (recognizes as framework tool)
         ↓
Updates LoadedToolkits[sessionID]["files"] = true
         ↓
Appends "[load_toolkit result]" to messages
         ↓
Re-calls API with updated tool list (now includes files tools)
         ↓
LLM uses read_file, write_file, etc.
```

`buildToolListWithLoaded()` merges:
- `execute_mpm_command` (always)
- Base tools from profile (`list_toolkits`, `load_toolkit`, `unload_toolkit`)
- Tools from loaded toolkits (deduped)

### AI Tool Loop

`RunAgent` in `core/agent.go`:
1. Build system prompt
2. Build tool list: base + dynamically loaded toolkit tools
3. Call API with tools
4. If API returns a tool call:
   - If `load_toolkit`/`unload_toolkit` → update state locally, re-call
   - If `execute_mpm_command` → exec `mpm <args>`, return output
   - If other → execute via `executeTool`
5. Append result, re-call API
6. Repeat until no more tool calls (max 5 iterations)

### Telegram Session Cleanup

`/new`, `/clear` clears both session history AND `LoadedToolkits` for that chat (via `ClearSessionToolkits`).

---

## Files to Modify

- `core/tools.go` — ToolDefinition registry, LoadedToolkits map, executeTool
- `core/config.go` — Profiles + Toolkits in MiniBotConfig
- `core/agent.go` — RunAgent with toolkit interception, buildToolListWithLoaded
- `cmd/telegram/handler.go` — sessionID passed to RunAgent, /tools command, /new clears toolkits
