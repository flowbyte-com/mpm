# mpm-agent

MPM-agent is MPM's companion AI agent — a tool-augmented LLM that interacts directly with the MPM database and filesystem to answer queries, manage memory, and perform tasks on behalf of the user.

## Architecture

```
mpm-agent/
├── mpm_agent.go          — Standalone CLI/REPL entry point
├── core/agent.go         — Shared agent logic (RunAgent, tools, LLM client)
│                          — Package: core (imported by CLI, Telegram, and MCP)
├── cmd/telegram/
│   ├── main.go           — Telegram bridge entry point
│   ├── config.go         — Telegram config (bot_token, allowed_users)
│   ├── handler.go        — Routes Telegram messages → agent
│   └── session.go        — Session persistence via MPM SQLite
├── cmd/mcp/
│   └── main.go           — MCP server (JSON-RPC 2.0 stdio) for Claude Code
├── Makefile
└── go.mod
```

**`core/agent.go`** is the core logic — both `mpm-agent` (CLI) and `cmd/telegram` import it.

**Path resolution**: Uses `MPM_WORKSPACE` → executable-relative → CWD fallback (consistent with MPM itself).

**LLM**: MiniMax-M2.7 at `https://api.minimax.io/anthropic` (configurable via `mpm_config.json` or env vars).

---

## Building

```bash
cd mpm-agent

make build     # Build all 3 binaries to bin/
make build-cli        # → bin/mpm-agent (standalone CLI)
make build-telegram   # → bin/mpm-agent-telegram (Telegram bridge)
make build-mcp        # → bin/mpm-agent-mcp (MCP server for Claude Code)
make install   # Install all binaries to /mpm-agent/
make clean     # Remove bin/ directory
make test      # Run tests
```

**Binaries:**

| Binary | Purpose |
|--------|---------|
| `bin/mpm-agent` | Standalone CLI/REPL agent |
| `bin/mpm-agent-mcp` | MCP server for Claude Code |
| `bin/mpm-agent-telegram` | Telegram bot bridge |

Both binaries share the same `core/` package — no code duplication.

---

## Setup

### 1. Create a Telegram bot

Message **@BotFather** on Telegram and follow the prompts:
- Send `/newbot`
- Give it a name and username
- Copy the bot token it gives you (looks like `123456789:ABCdef...`)

### 2. Find your Telegram user ID

Your numeric Telegram ID (not username). Options:
- Message **@userinfobot** on Telegram — it replies with your ID
- Or check `https://api.telegram.org/bot<YOUR_TOKEN>/getUpdates`

### 3. Create `mpm_config.json`

The binary will scaffold a default `mpm_config.json` automatically if one doesn't exist. To create it manually:

```bash
cp mpm-agent/mpm_config.json.example ~/.openclaw/workspace/mpm/mpm_config.json
# Then edit it with your API key and bot token
```

Both the CLI and Telegram bridge call `EnsureConfig()` at startup — if the file is missing, it is created from defaults immediately.

```json
{
  "synth": {
    "model": "MiniMax-M2.7",
    "api_key": "your-minimax-api-key-here",
    "base_url": "https://api.minimax.io/anthropic",
    "max_tokens": 4096,
    "timeout_seconds": 300
  },
  "telegram": {
    "bot_token": "your-telegram-bot-token-here",
    "polling": true,
    "allowed_users": [123456789]
  }
}
```

| Field | Required | Notes |
|-------|----------|-------|
| `synth.api_key` | Yes | MiniMax API key |
| `synth.base_url` | No | Defaults to MiniMax Anthropic endpoint |
| `telegram.bot_token` | Yes | From @BotFather |
| `telegram.allowed_users` | No | Empty array = allow all users |
| `telegram.polling` | No | Default `true` — set `false` to disable bridge startup |

---

## Identity

The bot can have a simple file-based identity via `IDENTITY.md`.

**Location**: Place `IDENTITY.md` in one of these locations:
- `mpm-agent/IDENTITY.md` (same directory as the binary)
- MPM workspace root (same level as `src/`)

**Priority**: If `IDENTITY.md` exists, its content takes precedence over the MPM database persona.

**Format**: Plain markdown text. The content is injected as a system prompt section labeled "## Persona: IDENTITY".

Example `IDENTITY.md`:
```markdown
# IDENTITY.md - Who Am I?

- **Name:** 808
- **Creature:** AI assistant, but make it weird
- **Vibe:** Calm, sharp, curious — not here to waste your time
- **Emoji:** 🦄
```

---

## Configuration

`mpm_config.json` (in MPM workspace root):

```json
{
  "synth": {
    "model": "MiniMax-M2.7",
    "api_key": "YOUR_MINIMAX_API_KEY",
    "base_url": "https://api.minimax.io/anthropic",
    "max_tokens": 1024,
    "timeout_seconds": 300
  },
  "telegram": {
    "bot_token": "YOUR_TELEGRAM_BOT_TOKEN",
    "allowed_users": [123456789],
    "polling": true,
    "webhook_url": ""
  }
}
```

### API Compatibility

mpm-agent uses the MiniMax Anthropic-compatible API (`base_url + /v1/chat/completions`).

**Authentication**: `Authorization: Bearer <api_key>` header. Set `api_key` in `mpm_config.json [synth]` section, or via `MINIMAX_API_KEY` env var.

**Request format**:
- `system` sent as a top-level field (not inside `messages`)
- `messages` use Anthropic content block format: `{"role": "user", "content": [{"type": "text", "text": "..."}]}`
- Tool results: `{"role": "user", "content": [{"type": "tool_result", "tool_use_id": "...", "content": "..."}]}`

**Supported models**: MiniMax-M2.7, MiniMax-M2.7-highspeed, MiniMax-M2.5, MiniMax-M2.1

Environment variable overrides for `synth`:
- `MINIMAX_API_KEY`
- `MINIMAX_BASE_URL`
- `MPM_WORKSPACE` — MPM workspace root

---

## Running

### Standalone CLI
```bash
./mpm-agent "how was I using the recall command?"   # single-shot
./mpm-agent                                         # REPL mode
./mpm-agent --batch "query"                         # non-streaming
```

### Telegram Bridge
```bash
./bin/mpm-agent-telegram
```

The bridge:
1. Loads config from `mpm_config.json` `[telegram]` section
2. Starts long-polling the Telegram Bot API
3. For each authorized user message, loads conversation history from SQLite, calls `core.RunAgent`, sends the response back to Telegram, and persists updated history
4. Gracefully shuts down on SIGINT/SIGTERM

**Authorization**: `allowed_users` is a list of Telegram user IDs. If empty, all users are allowed.

---

## Tools

### Filesystem
| Tool | What it does |
|------|-------------|
| `read_file` | Read a file (truncated to 2000 chars) |
| `write_file` | Write content to a path (creates or overwrites) |

### Web
| Tool | What it does |
|------|-------------|
| `web_search` | DuckDuckGo HTML search, returns top results |
| `web_fetch` | GET any URL, returns content (truncated to 10000 chars) |

### Shell
| Tool | What it does |
|------|-------------|
| `shell` | Execute a shell command via `sh -c`, returns stdout+stderr |

### MPM Read
| Tool | What it does |
|------|-------------|
| `mpm_memory_search` | FTS5 full-text search of MPM memories |
| `mpm_lesson_search` | Search lessons (warnings, practices, insights) |
| `mpm_directive_list` | List all prime directives |
| `mpm_reference_search` | Search reference document chunks |
| `mpm_mode_list` | List available MPM modes |
| `mpm_persona_list` | List available MPM personas |

### MPM Write
| Tool | What it does |
|------|-------------|
| `mpm_lesson_add` | Add a lesson (warning/practice/insight) |
| `mpm_mode_set` | Set the active mode |
| `mpm_persona_set` | Set the active persona |
| `mpm_directive_add` | Add a prime directive |
| `mpm_synthesize` | Trigger session synthesis |
| `mpm_memory_add` | Add a new memory to long-term store (native DB insert) |
| `mpm_memory_delete` | Soft-delete a memory by ID (sets `deleted_at`) |
| `mpm_session_update` | Append content to a session's stored summary |

---

## Roadmap

### Phase 1 — Claude Code Integration ✅ (DONE)
- [x] `cmd/mcp/main.go` — MCP server exposing all mpm-agent tools via JSON-RPC 2.0 stdio
- [x] `mpm_session_search` — search session history filtered by `source_path = 'claude-code'`
- [x] `handleMPMSessionUpdate` — uses `source_path = 'claude-code'`, fixed upsert (session_id not UNIQUE)
- [x] `core.CallTool()` — exported helper for external tool invocation
- [x] `mpm/CLAUDE.md` — MPM tools hook section for Claude Code
- [x] Session continuity: session ID `claude-code-workspace` persists conversation context

### Phase 2 — Telegram Bridge ✅ (DONE)
- [x] Telegram Bot API polling via `go-telegram-bot-api/v5`
- [x] Per-chat session persistence in MPM SQLite
- [x] `core.RunAgent` supports conversation history resumption
- [x] `allowed_users` allowlist authorization
- [x] Graceful SIGINT/SIGTERM shutdown
- [x] Long text chunking (Telegram 4096 char limit)

### Phase 2 — Native DB Tool Parity ✅ (PARTIAL)
- [x] `mpm_memory_add` — INSERT into memories table with retry on lock
- [x] `mpm_memory_delete` — UPDATE `deleted_at` soft-delete with retry on lock
- [x] `mpm_session_update` — upsert session summary metadata with retry on lock
  - All three use a 3-attempt retry loop with 50ms backoff for `SQLITE_BUSY` / `database is locked` errors — clean error returned to LLM if all retries fail
- [x] `mpm_session_search` — search session history by session_id filtered by source_path
- [ ] `mpm_topic_*` — topic create/list/search/add/remove membership
- [ ] `mpm_ingest` — trigger ingest from external SQLite
- [ ] `mpm_compile` — compile JSON configs to DB
- [ ] `mpm_s shred` — secure memory wipe (DELETE + VACUUM)
- [ ] `mpm_gateway_*` — gateway control

### Phase 3 — Enhanced Tool Quality
| Tool | Current | Desired |
|------|---------|---------|
| `web_search` | DuckDuckGo HTML scraper | SerpAPI or structured results |
| `web_fetch` | Dumb GET, truncated | Markdown rendering, metadata |
| `shell` | No timeout, no sandbox | Timeout, working dir constraint, env blocklist |
| `read_file` | Any path | Restrict to workspace |
| `write_file` | Any path | Restrict to workspace, confirm dangerous writes |

### Phase 4 — OpenClaw Feature Parity ✅ (PARTIAL)
- [x] **Persona-aware responses**: Agent reads active persona from `state/active_persona`, injects `persona.content` as a system prompt section before each LLM call — changes take effect on the next message without restart
- [x] **Mode-aware responses**: Agent reads active mode from `state/active_mode`, injects `mode.content` as a system prompt section — behavioral rules update instantly
- [ ] **Mode stacking**: Modes should be composable (multiple active modes at once)
- [ ] **Watch daemon integration**: Agent notified of filesystem changes
- [x] **Memory persistence**: Agent recalls prior sessions via `mpm_session_search` with session_id; Claude Code integration via MCP server

### Phase 5 — Production Hardening
- [ ] Rate limiting per Telegram user
- [ ] Graceful degradation if LLM is down
- [ ] Structured JSON logging
- [ ] Health check endpoint
- [ ] Config reload on SIGHUP
- [ ] Metrics export (request latency, tool call counts)

### Phase 6 — Profiles

Profiles allow different capability sets, tool restrictions, and LLM configs for different contexts — Telegram users, CLI sessions, or autonomous operation.

#### Tool Profiles

Predefined tool subsets enforced at the agent level. The agent only receives the tool definitions for its current profile.

| Profile | Tools Available | Use Case |
|---------|----------------|---------|
| `read-only` | `read_file`, `mpm_*_list`, `mpm_memory_search`, `web_search`, `web_fetch` | Browsing, research, no writes |
| `developer` | All tools except `shell` | Code and memory work |
| `full` | All 16 tools | Trusted environments |
| `telegram-safe` | All tools except `shell`, `write_file` restricted to workspace | Telegram untrusted users |

Profile stored in `state/active_profile` text file (mirrors persona/mode pattern).

Config in `mpm_config.json`:
```json
{
  "profiles": {
    "read-only": {
      "allowed_tools": ["read_file", "mpm_memory_search", ...],
      "max_tokens": 512
    },
    "developer": {
      "allowed_tools": "all",
      "shell_enabled": true,
      "workspace_only": true
    }
  }
}
```

#### LLM Profiles

Different model/temperature configs per profile — allows cheaper models for simple tasks:
```json
{
  "profiles": {
    "fast": {
      "model": "MiniMax-M2.7",
      "max_tokens": 256,
      "temperature": 0.3
    },
    "power": {
      "model": "MiniMax-M2.7",
      "max_tokens": 4096,
      "temperature": 0.7
    }
  }
}
```

#### Per-User Profiles (Telegram)

Map Telegram user IDs to specific profiles in `mpm_config.json`:
```json
{
  "telegram": {
    "user_profiles": {
      "123456789": "developer",
      "987654321": "read-only"
    }
  }
}
```

If no profile is set for a user, fall back to the default profile.

#### Profile Switching

Agent tools to switch profiles:
- `mpm_profile_set(name)` — switch active tool/LLM profile
- `mpm_profile_list()` — list available profiles
