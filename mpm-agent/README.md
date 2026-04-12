# mpm-agent

A self-improving AI agent with Telegram interface, MCP server, and Toolkit lazy-loading for MPM access.

## Binaries

| Binary | Purpose |
|--------|---------|
| `mini-bot` | CLI REPL — single-shot or interactive agent |
| `mini-bot-telegram` | Telegram bot daemon — long-polling bridge |
| `mini-bot-mcp` | MCP server — stdio JSON-RPC, serves `execute_mpm_command` |

## Quick Start

### Build

```bash
make mini-bot           # → bin/mini-bot
make mini-bot-telegram  # → bin/mini-bot-telegram
make mini-bot-mcp       # → bin/mini-bot-mcp
```

Or individually:

```bash
go build -o bin/mini-bot       ./cmd/mini-bot
go build -o bin/mini-bot-telegram ./cmd/telegram
go build -o bin/mini-bot-mcp   ./cmd/mcp
```

### Configure

Copy and edit the config:

```bash
cp mini-bot-config.json.example mini-bot-config.json
```

Edit `mini-bot-config.json`:

```json
{
  "identity": { "name": "your-bot", "version": "1.0" },
  "synth": {
    "model": "MiniMax-M2.7",
    "api_key": "your-key-here",
    "base_url": "https://api.minimax.io/anthropic"
  },
  "telegram": {
    "bot_token": "your-telegram-token",
    "allowed_users": [8507429405]
  },
  "paths": { "db": "mini-bot.db", "identity": "IDENTITY.md" }
}
```

### Run the Telegram bot

```bash
MPM_WORKSPACE=$HOME/mpm ./bin/mini-bot-telegram
```

The bot long-polls Telegram and responds to messages. Use `/help` to see commands.

### Run the MCP server

```bash
MPM_WORKSPACE=$HOME/mpm ./bin/mini-bot-mcp
```

Reads JSON-RPC requests from stdin, writes responses to stdout.

## Configuration

All settings in `mini-bot-config.json`:

| Section | Key | Description |
|---------|-----|-------------|
| `identity` | `name`, `version` | Bot identity |
| `synth` | `model`, `api_key`, `base_url`, `max_tokens`, `timeout_seconds` | LLM API settings |
| `telegram` | `bot_token`, `polling`, `allowed_users` | Telegram bot settings |
| `paths` | `db`, `identity`, `mcp_socket` | File paths relative to binary |
| `profiles` | `standard` | Base tools available to the AI |
| `toolkits` | `files`, `web`, `jq`, `mpm` | Dynamically-loadable tool collections |

**Environment variables** override config values:
- `MINIMAX_API_KEY` → `synth.api_key`
- `MPM_API_TOKEN` → `mcp.token`

## IDENTITY.md

Defines the bot's stable core identity. Place `IDENTITY.md` next to the binary.

```markdown
# MiniBot v1.0
Type: helpful assistant
Domain: general-purpose
Core traits: patient, thorough, cites sources
Boundaries: never fabricate facts, admit uncertainty
```

The bot reads this at startup and includes it in every system prompt.

## Tool System

### Toolkit Lazy-Loading

The bot uses a **Toolkit** pattern — it starts with only 4 base tools to keep token costs low, and dynamically loads more tools as needed.

**Base tools** (always available):
- `list_toolkits` — show available toolkits
- `load_toolkit("name")` — load a toolkit
- `unload_toolkit("name")` — unload a toolkit
- `execute_mpm_command` — run any MPM CLI command

**Available toolkits:**

| Toolkit | Tools |
|---------|-------|
| `files` | `read_file`, `write_file`, `ReadFileSemantic`, `ReadFileCompare` |
| `web` | `WebSynthesize` (DuckDuckGo search + synthesis) |
| `jq` | `jq` — query JSON files with jq filters |
| `mpm` | All MPM CLI commands via `execute_mpm_command` |

**Example usage:**
```
User: load_toolkit("files")
Bot: Toolkit 'files' loaded. You now have access to: read_file, write_file...

User: read a file called notes.md
Bot: [reads the file]
```

### File Path Restriction

All file tools are scoped to the `mpm-agent/` directory tree. Paths outside are rejected.

### `/tools` Command (Telegram)

In Telegram, `/tools` shows the current tool profile and available profiles. `/tools <name>` switches profile (future profiles may have different base tool sets).

## Self-Improvement

mini-bot improves itself automatically:

1. **Memory Anchoring** — important conversation moments are anchored with weight, loaded as high-priority context on every call
2. **Lesson Extraction** — informative exchanges are stored as lessons, reinforced on repeat
3. **Identity Patching** — bot can propose identity changes to `IDENTITY_PATCH.md` for human approval

## Telegram Commands

| Command | Description |
|---------|-------------|
| `/new`, `/clear` | Clear session history and toolkits |
| `/think [off\|low\|adaptive\|med\|high]` | Set thinking level |
| `/reasoning [on\|off]` | Toggle reasoning mode |
| `/verbose [on\|off]` | Show/hide thinking blocks |
| `/tools [name]` | Show or switch tool profiles |
| `/status` | Show current settings |
| `/` | Show help |

## Architecture

```
                    Telegram long-polling
                         |
mini-bot-telegram ────── agent loop ────── mini-bot (REPL)
         |                   |                     |
         |                   +--- mini-bot.db ----+
         |                   |   (memories, sessions,
         |                   |    lessons, anchors)
         |                   |
         +------- MCP -------+
                     |
              mini-bot-mcp
              (execute_mpm_command)
                     |
                     v
              MPM CLI binary
              (recall, mode, persona,
               memory, topic, etc.)
```

## Database

`mini-bot.db` — SQLite with FTS5 full-text search:

- `memories` — searchable memory store
- `sessions` — per-chat conversation history
- `lessons` — reinforced learnings
- `anchors` — high-priority memory moments
- `tools` — self-registered tools
