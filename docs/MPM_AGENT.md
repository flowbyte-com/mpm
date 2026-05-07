# mpm-agent — Companion AI Agent

`mpm-agent/` is a tool-augmented LLM that interacts with the MPM database directly. Three binaries sharing `core/` package.

## Binaries

| Binary | Command | Purpose |
|--------|---------|---------|
| `bin/mpm-agent` | `make build-cli` | Standalone CLI/REPL |
| `bin/mpm-agent-mcp` | `make build-mcp` | MCP server for Claude Code |
| `bin/mpm-agent-telegram` | `make build-telegram` | Telegram bot bridge |

```bash
cd mpm-agent
make build     # All 3 binaries
make test      # Run tests
```

## MCP Server (Claude Code Integration)

The MCP server exposes all MPM tools to Claude Code via JSON-RPC 2.0 (stdio).

**Available tools**:
- Memory: `mpm_memory_search`, `mpm_memory_add`, `mpm_memory_delete`
- Session: `mpm_session_search`, `mpm_session_update`
- References/Lessons: `mpm_reference_search`, `mpm_lesson_search`, `mpm_lesson_add`
- Mode/Persona: `mpm_mode_list`, `mpm_mode_set`, `mpm_persona_list`, `mpm_persona_set`
- Directives: `mpm_directive_list`, `mpm_directive_add`
- Synthesis: `mpm_synthesize`
- General: `read_file`, `write_file`, `shell`, `web_search`, `web_fetch`

**Config in `~/.claude/settings.json`**:
```json
"mcpServers": {
  "mpm": {
    "command": "/path/to/mpm-agent/bin/mpm-agent-mcp",
    "env": {
      "MPM_WORKSPACE": "/path/to/workspace",
      "MPM_API_TOKEN": "<token>"
    }
  }
}
```

Token auth: set `api_token` in `mpm_config.json` `[synth]` section, pass as `MPM_API_TOKEN` env. Optional — skipped if unset.

## Telegram Bot

Long-polling bridge. Message @BotFather for a token. Configure in `mpm_config.json`:

```json
{
  "telegram": {
    "bot_token": "123456789:ABCdef...",
    "allowed_users": [123456789],
    "polling": true
  }
}
```

Per-chat session persistence in MPM SQLite. Long text auto-chunked (4096 char limit). Graceful SIGINT/SIGTERM shutdown.

## Identity

`IDENTITY.md` optional file — injected as system prompt section "## Persona: IDENTITY". Located at binary directory or MPM workspace root. Takes precedence over DB persona.

## Configuration

`mpm_config.json` in MPM workspace root. Auto-scaffolded on first run via `EnsureConfig()`.

**Synth fields**: `model`, `api_key`, `base_url`, `max_tokens`, `timeout_seconds`  
**Auth**: `Authorization: Bearer <api_key>` header against MiniMax Anthropic-compatible API.

## Tools Reference

| Toolkit | Tools |
|---------|-------|
| Filesystem | `read_file` (2000 char truncation), `write_file` |
| Web | `web_search` (DuckDuckGo HTML), `web_fetch` (10000 char truncation) |
| Shell | `shell` (sh -c, returns stdout+stderr) |
| MPM Read | `mpm_memory_search`, `mpm_lesson_search`, `mpm_directive_list`, `mpm_reference_search`, `mpm_mode_list`, `mpm_persona_list`, `mpm_session_search` |
| MPM Write | `mpm_memory_add`, `mpm_memory_delete`, `mpm_lesson_add`, `mpm_mode_set`, `mpm_persona_set`, `mpm_directive_add`, `mpm_synthesize`, `mpm_session_update` |

**Note**: FTS5 searches fall back to LIKE when the SQLite build lacks FTS5 support.

## Architecture

```
mpm-agent/         core/agent.go          — RunAgent, LLM client, tool loop
│                  core/config.go         — Config loading, env fallbacks
│                  core/db.go             — Direct SQLite access
├── cmd/cli/       cmd/mini-bot/main.go   — CLI/REPL entry
├── cmd/mcp/       cmd/mcp/main.go        — MCP JSON-RPC server
├── cmd/telegram/  cmd/telegram/main.go   — Telegram bridge
│                  cmd/telegram/handler.go — Message routing
│                  cmd/telegram/session.go — Chat persistence
```
