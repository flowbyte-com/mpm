# Tool Profiles Design — mini-bot Telegram

## Overview

Add a tool profile system to mini-bot Telegram. Profiles are named collections of tools. The Standard profile is always available; additional profiles build on top of it.

## Profile: Standard

### File Tools (scoped to `mpm-agent/` tree)

| Tool | Args | Description |
|------|------|-------------|
| `read_file` | `path` | Read file contents |
| `write_file` | `path`, `content` | Write content to file |
| `ReadFileSemantic` | `path`, `mode` | Read with semantic mode: `summary`, `code`, `compare` |
| `ReadFileCompare` | `pathA`, `pathB` | Diff two files |

Path restriction: all file paths resolved relative to `mpm-agent/` directory. Absolute paths and `..` escapes are rejected.

### Web Tool

| Tool | Args | Description |
|------|------|-------------|
| `WebSynthesize` | `query` | DuckDuckGo search + synthesis with citations |

### Shell/JQ Tool

| Tool | Args | Description |
|------|------|-------------|
| `jq` | `filter`, `file` | Run `jq` filter on JSON file. Restricted to `*.json` and `*.jsonl` files in `mpm-agent/` |

### MPM Commands (via `mpm` binary)

These invoke `mpm <cmd> [args]` via shell and return stdout.

| Tool | MPM Equivalent | Description |
|------|----------------|-------------|
| `mpm_recall` | `mpm recall <q>` | Search memories for context |
| `mpm_topics` | `mpm topics` | List all topic names |
| `mpm_mode` | `mpm mode [name]` | Show current or set mode |
| `mpm_persona` | `mpm persona [name]` | Show current or set persona |
| `mpm_memory` | `mpm memory <args>` | Memory operations |
| `mpm_topic` | `mpm topic <args>` | Topic management |
| `mpm_session` | `mpm session <args>` | Session operations |
| `mpm_lesson` | `mpm lesson <args>` | Lesson operations |
| `mpm_reference` | `mpm reference <args>` | Reference library |
| `mpm_version` | `mpm version` | Show version info |
| `mpm_doctor` | `mpm doctor` | Run diagnostics |
| `mpm_compile` | `mpm compile` | Compile project |
| `mpm_shred` | `mpm shred` | Secure memory wipe |
| `mpm_ingest` | `mpm ingest <args>` | Import external memories |
| `mpm_stats` | `mpm stats` | Show MPM statistics |

**Path constraint**: `mpm` binary must be in `PATH` or resolvable via `MPM_WORKSPACE`.

## Profile Switching

**`/tools`** command shows an inline keyboard with available profiles:

```
[Standard]  [MCP-Admin*]  [Research*]
```

Profiles marked `*` are future extensions (out of scope for this spec).

Selecting a profile:
1. Stores the active profile name in `chatSettings` for that chat
2. Returns confirmation: "Tool profile: Standard"
3. The AI agent loop uses the profile's tool list for tool calls

**`/tools <name>`** — directly switch to named profile.

**`/tools`** (no args) — show inline menu.

## Architecture

### Tool Registry

`core/tools.go` expanded with:
- `ToolDefinition` struct: `{name, description, inputSchema}`
- `RegisterTool(name, def)` — add tool to registry
- `GetTool(name)` — fetch tool definition
- `ListTools()` — all registered tools
- `ListToolsByProfile(profile)` — tools in a profile

Profile definitions in `core/config.go`:
```go
type ToolProfile struct {
    Name  string
    Tools []string  // tool names
}

type MiniBotConfig struct {
    ...
    Profiles map[string][]string `json:"profiles"`  // profile name → tool names
}
```

Default profiles in `DefaultMiniBotConfig()`:
```go
Profiles: map[string][]string{
    "standard": {
        "read_file", "write_file", "ReadFileSemantic", "ReadFileCompare",
        "WebSynthesize", "jq",
        "mpm_recall", "mpm_topics", "mpm_mode", "mpm_persona",
        "mpm_memory", "mpm_topic", "mpm_session", "mpm_lesson",
        "mpm_reference", "mpm_version", "mpm_doctor", "mpm_compile",
        "mpm_shred", "mpm_ingest", "mpm_stats",
    },
}
```

### Tool Execution

`executeTool(tool string, args map[string]interface{}) (string, error)` expanded:
- MPM tools: exec `mpm <cmd> <args...>` via `exec.Command`, return stdout/stderr
- File tools: current implementation + path restriction to `mpm-agent/`
- `jq`: validate file extension, run `jq` command
- `WebSynthesize`: existing implementation

### AI Tool Loop

`RunAgent` in `agent.go` updated:
1. Build system prompt (unchanged)
2. Call API with available tools from active profile in tool schema
3. If API returns a tool call, execute via `executeTool`
4. Collect result, re-call API with result
5. Repeat until no more tool calls (max 5 iterations to prevent loops)
6. Return final text response

### Telegram `/tools` Command

In `handler.go`, `handleCommand`:
- `/tools` — send inline keyboard with profile buttons
- `/tools standard` — switch to standard profile, confirm

Inline keyboard sent via `telego.EditMessageText` with inline keyboard markup.

## Implementation Order

1. Define `ToolDefinition` struct and registry in `core/tools.go`
2. Add profile config to `MiniBotConfig` in `core/config.go`
3. Expand `executeTool` with MPM commands, `jq`, path restriction
4. Wire tool registry into `RunAgent` tool loop
5. Add `/tools` command to handler
6. Register Standard profile as default

## Files to Modify

- `core/tools.go` — registry + `executeTool` expansion
- `core/config.go` — profile definitions in config
- `core/agent.go` — tool loop in `RunAgent`
- `cmd/telegram/handler.go` — `/tools` command
- `cmd/telegram/config.go` — Telegram config (no change needed)
