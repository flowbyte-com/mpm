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
go build -o bin/mini-bot         ./cmd/mini-bot
go build -o bin/mini-bot-telegram ./cmd/telegram
go build -o bin/mini-bot-mcp     ./cmd/mcp
```

### Configure

```bash
cp mini-bot-config.json.example mini-bot-config.json
```

Edit `mini-bot-config.json`:

```json
{
  "identity": { "name": "mini-bot", "version": "1.0" },
  "synth": {
    "model": "MiniMax-M2.7",
    "api_key": "your-key-here",
    "base_url": "https://api.minimax.io/anthropic"
  },
  "telegram": {
    "bot_token": "your-telegram-token",
    "polling": true,
    "allowed_users": [8507429405]
  },
  "self_improve": {
    "enabled": true,
    "anchor_threshold": 3,
    "lesson_complexity_threshold": 7,
    "identity_patch_approval_required": true
  },
  "paths": {
    "db": "mini-bot.db",
    "identity": "IDENTITY.md",
    "mcp_socket": "mini-bot-mcp.sock"
  }
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
| `identity` | `name`, `version`, `hot_reload` | Bot identity |
| `synth` | `model`, `api_key`, `base_url`, `max_tokens`, `timeout_seconds` | LLM API settings |
| `telegram` | `bot_token`, `polling`, `allowed_users` | Telegram bot settings |
| `self_improve` | `enabled`, `anchor_threshold`, `lesson_complexity_threshold`, `identity_patch_approval_required` | Self-improvement settings |
| `paths` | `db`, `identity`, `mcp_socket`, `workspace_root` | File paths relative to binary |
| `profiles` | `standard`, `coding` | Named tool profiles |
| `toolkits` | `mpm_core`, `files`, `web`, `jq`, `coding_tools`, `shell`, `minimax` | Dynamically-loadable tool collections |

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

## Memory Architecture

Three-layer persistent memory system:

```
Layer 1: Session History — last 50 messages per chat, merged not replaced
Layer 2: Front Cortex   — user identity, recent session summaries, top anchors
                           (~600 char hard cap, always injected into prompt)
Layer 3: Long-term      — FTS5 memories, lessons (query-activated)
```

On restart, the bot loads the front cortex and knows who you are, what you were working on, and key context from recent sessions. Cross-session continuity without full history.

## Tool System

### Toolkit Lazy-Loading

The bot starts with only base tools to keep token costs low, and dynamically loads more tools as needed.

**Base tools** (always available):
- `list_toolkits` — show available toolkits
- `load_toolkit("name")` — load a toolkit
- `unload_toolkit("name")` — unload a toolkit
- `execute_mpm_command` — run any MPM CLI command
- `update_identity_knowledge(key, value, source)` — store facts about the user

**Available toolkits:**

| Toolkit | Tools |
|---------|-------|
| `mpm_core` | `list_toolkits`, `load_toolkit`, `unload_toolkit`, `execute_mpm_command` |
| `files` | `read_file`, `write_file`, `ReadFileSemantic`, `ReadFileCompare` |
| `web` | `WebSynthesize` (DuckDuckGo search + synthesis) |
| `jq` | `jq` — query JSON files with jq filters |
| `coding_tools` | `rg`, `sg`, `repomap`, `git_status`, `git_commit`, `git_diff` |
| `minimax` | `generate_image`, `synthesize_speech`, `web_search`, `understand_image` |
| `shell` | `execute_shell` |

**Tool profiles** group tools for different use cases:

- `standard` — files, web, jq, MPM, identity knowledge
- `coding` — files, shell, git tools, MPM

All file tools are scoped to the `mpm-agent/` directory tree. Paths outside are rejected.

### `/tools` Command (Telegram)

`/tools` shows the current profile. `/tools <name>` switches profile.

## Self-Improvement

mini-bot improves itself automatically:

1. **Memory Anchoring** — important conversation moments are anchored with weight (top 10 loaded per call)
2. **Lesson Extraction** — informative exchanges stored as lessons, reinforced on repeat
3. **Identity Patching** — bot can propose identity changes to `IDENTITY_PATCH.md` for human approval
4. **Session Summarization** — after 5 minutes idle, full session is distilled into a one-line summary for cross-session continuity

## Telegram Commands

| Command | Description |
|---------|-------------|
| `/new` | Clear session history and start fresh |
| `/clear` | Clear session history (keep current session) |
| `/think <level>` | Set thinking level: off, low, adaptive, med, high |
| `/reasoning [on\|off]` | Toggle reasoning mode |
| `/verbose [on\|off]` | Show/hide thinking blocks |
| `/stream [on\|off]` | Toggle tool streaming |
| `/tokens` | Show token usage for this session |
| `/summarize` | Summarize session into a one-line overview (requires ≥10 messages) |
| `/tools [name]` | Show or switch tool profiles |
| `/status` | Show current settings |
| `/help` | Show available commands |

## Architecture

```
                    Telegram long-polling
                         |
mini-bot-telegram ────── agent loop ────── mini-bot (REPL)
         |                   |                     |
         |                   +--- mini-bot.db ----+
         |                   |   (front cortex,    |
         |                   |    sessions,        |
         |                   |    lessons, anchors)|
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

- `sessions` — per-chat conversation history (merged, sliding window of 50)
- `session_summaries` — one-line distilled summaries per session
- `lessons` — reinforced learnings from informative exchanges
- `anchors` — high-priority memory moments (top 10 loaded per call)
- `identity_knowledge` — stored user facts (name, project, preferences)
- `memories` — searchable long-term memory store
- `tools` — self-registered tools
