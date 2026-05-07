# CLAUDE.md

This file provides guidance to Claude Code (claude.ai/code) when working with code in this repository.

## Project Overview

mpm-agent is a self-improving AI agent with three entry points: a CLI REPL, a Telegram bot daemon, and an MCP server. It integrates with MPM (Memory-Persona-Mode Manager) and uses a **Toolkit lazy-loading** pattern to keep token costs low at startup.

## Building & Testing

```bash
# Build all binaries to bin/
make build

# Build specific binary
make build BIN=bin/mini-bot
make build BIN=bin/mini-bot-telegram
make build BIN=bin/mini-bot-mcp

# Install to /usr/local/bin/
make install

# Run tests (requires FTS5)
CGO_CFLAGS="-DSQLITE_ENABLE_FTS5" CGO_LDFLAGS="-lm" go test ./core/...

# Run a single test
CGO_CFLAGS="-DSQLITE_ENABLE_FTS5" CGO_LDFLAGS="-lm" go test ./core/... -run TestFunctionName
```

FTS5 must be enabled via `CGO_CFLAGS="-DSQLITE_ENABLE_FTS5"`.

## Binaries

| Binary | Entry Point | Purpose |
|--------|-------------|---------|
| `mini-bot` | `cmd/mini-bot/main.go` | CLI REPL — single-shot or interactive |
| `mini-bot-telegram` | `cmd/telegram/telegram.go` | Telegram bot daemon — long-polling |
| `mini-bot-mcp` | `cmd/mcp/main.go` | MCP server — stdio JSON-RPC for `execute_mpm_command` |

## Architecture

```
                    Telegram long-polling
                         |
mini-bot-telegram ────── agent loop ────── mini-bot (REPL)
         |                   |                     |
         |                   +--- mini-bot.db ----+
         |                   |   (memories, sessions,
         |                   |    lessons, anchors, tools)
         |                   |
         +------- MCP -------+
                     |
              mini-bot-mcp
              (execute_mpm_command)
                     |
                     v
              MPM CLI binary
```

**Core loop** (`core/agent.go:RunAgent`): Tool loop that calls the Anthropic-compatible Synth API, executes tools, and repeats up to 20 iterations (configurable). The agent uses an **identity-first** system prompt built by `BuildSystemPromptWithIdentity` in strict priority order: IDENTITY.md → anchored memories → memories → directives → references.

## Tool System

### Tool Profiles vs Toolkits

**Tool profiles** (`profiles` in config) define base tools always available to the AI. Default profiles:
- `standard` — file operations, web search, MPM access
- `coding` — git operations, ripgrep, ast-grep, repomap

**Toolkits** (`toolkits` in config) are dynamically-loaded per session via `load_toolkit("name")`:
- `files` — `read_file`, `write_file`, `ReadFileSemantic`, `ReadFileCompare`
- `web` — `WebSynthesize` (DuckDuckGo search + synthesis)
- `jq` — jq filter on JSON files
- `mpm` — All MPM CLI commands via `execute_mpm_command`
- `minimax` — `generate_image`, `synthesize_speech`, `web_search`, `understand_image`
- `shell` — `execute_shell`

### Framework Tools

Four base tools are always registered and cannot be unloaded: `list_toolkits`, `load_toolkit`, `unload_toolkit`, `execute_mpm_command`. The first three are intercepted in the agent loop (not dispatched to the API) so toolkit loading works without round-trips.

### Tool Registration

All tools are registered in `core/tools.go` via the `init()` function on package load. Tool definitions include `Name`, `Description`, and `InputSchema` matching the Anthropic tool format.

## Self-Improvement System

Three mechanisms in `core/selfimprove.go`:

1. **Memory Anchoring** — The Telegram handler (`cmd/telegram/handler.go:selfImprove`) anchors user messages over 50 chars with weight based on length. Top 5 anchors load as high-priority context every call.

2. **Lesson Extraction** — Exchanges with response >100 chars and user text >20 chars create lessons with reinforcement tracking via `ExtractLesson`. The `lessons` table uses `ON CONFLICT(content)` to increment `reinforcement_count` on duplicate insights.

3. **Identity Patching** — Bot proposes changes to `IDENTITY_PATCH.md` via `ProposeIdentityPatch()`. Human reviews and approves; `ApplyIdentityPatch()` merges with a backup before overwriting.

Additionally, `core/identity_fork.go` implements a full **identity branching system** (`ForkIdentity`, `ListIdentityBranches`, `SwitchIdentityBranch`, `PromoteIdentityBranch`) stored in `IDENTITIES/` with `branches.json` tracking parent/child relationships and status.

## Database

`mini-bot.db` — Single SQLite database with FTS5, shared by session manager and agent. Schema is defined in two places (intentional for separation):
- `core/db.go:InitMiniBotDB` — Core initialization
- `cmd/telegram/session.go:initSessionDB` — Session manager initialization (same schema)

Tables: `memories` (with FTS5 `memories_fts`), `sessions`, `lessons`, `anchors`, `tools`. The `initSessionDB` function is the authoritative schema since it runs at startup.

Session history (50-message sliding window) is stored as JSON in `sessions.metadata`.

## Telegram Handler Flow

`cmd/telegram/handler.go` orchestrates the full message lifecycle:

1. **Authorization** — `isAllowed()` checks user against `cfg.AllowedUsers`
2. **Command interception** — `/new`, `/clear`, `/think`, `/reasoning`, `/verbose`, `/tools`, `/status`, `/help`
3. **Double-reply prevention** — `activeReplies sync.Map` (chatID → true) serializes replies per chat
4. **Async reply** — `agentReply()` starts a typing indicator goroutine then runs `runAgentWithTimeout` in a goroutine
5. **Response cleaning** — `cleanResponse()` strips thinking blocks (`<thinking>`, `《》`, `（）》`, `。。`) before Telegram display
6. **Long text** — `sendLongText()` splits on `"\n\n"` boundaries at 4096-char Telegram limit
7. **Self-improvement** — After response, `selfImprove()` runs in a goroutine with its own db connection

Per-chat settings stored in `chatSettings sync.Map`: `thinkLevel`, `verbose`, `toolProfile`.

## Configuration

All settings via `mini-bot-config.json`. Environment variables override config values:
- `MINIMAX_API_KEY` → `synth.api_key`
- `MPM_API_TOKEN` → `mcp.token`
- `MPM_WORKSPACE` → MPM workspace path

Config path resolution: binary directory first, then `mini-bot-config.json` in CWD.

**Retry config** controls exponential backoff for overloaded API errors (529):
```json
{ "retry": { "max_retries": 5, "base_delay_seconds": 1, "max_delay_seconds": 30 } }
```

## IDENTITY.md

Defines the bot's stable core identity. Resolved via `core/identity.go:ResolveIdentityPath` checking (in order): absolute path → CWD → binary directory. Format:
```markdown
# BotName v1.0
Type: helpful assistant
Domain: general-purpose
Core traits: patient, thorough, cites sources
Boundaries: never fabricate facts, admit uncertainty
```

## API Integration

The Synth API (`callSynthAPIWithTools` in `core/agent.go`) calls an **Anthropic-compatible endpoint** via MiniMax (`https://api.minimax.io/anthropic/v1/messages`). Request/response format matches the Anthropic Messages API with `system`, `messages`, and `tools` fields. Handles `type: error` top-level responses from MiniMax.

## Key Source Files

| File | Purpose |
|------|---------|
| `core/agent.go` | Agent loop, Synth API calls, system prompt builder, tool dispatch |
| `core/tools.go` | Tool registry, toolkit management, all tool implementations |
| `core/config.go` | Config loading, env var overrides, defaults |
| `core/identity.go` | IDENTITY.md parsing, path resolution, binary directory detection |
| `core/identity_fork.go` | Identity branching/versioning system |
| `core/anchors.go` | Anchor storage and retrieval |
| `core/selfimprove.go` | Lesson extraction, identity patching, self-tool registration |
| `core/db.go` | Database initialization, FTS5 schema, ID generation |
| `cmd/telegram/telegram.go` | Bot main, long-polling setup, graceful shutdown |
| `cmd/telegram/handler.go` | Message handling, session management, async agent flow |
| `cmd/telegram/session.go` | Session history persistence to SQLite |
| `cmd/telegram/config.go` | Telegram config loading |
| `cmd/mcp/main.go` | MCP stdio JSON-RPC server |

## Gotchas

- **`mini-bot.db` is shared** — Both the session manager and the agent open `mini-bot.db` independently. SQLite handles concurrent reads, but writes (session saves, anchor inserts) could contend with the agent's own db operations. The `SessionManager` retries on "database is locked" with exponential backoff.
- **Toolkit lazy-loading is session-scoped** — `LoadedToolkits` maps sessionID → toolkit names, cleared on `/new` or `/clear`.
- **File tool path restriction** — `read_file`, `write_file`, `jq`, `understand_image` accept absolute paths but no path sandboxing is currently enforced. All paths are resolved via `filepath.Abs` with no boundary check.
- **Two `initSessionDB` definitions** — Both `core/db.go` and `cmd/telegram/session.go` define the full schema. They are kept separate intentionally (core vs. cmd package), but must remain in sync.
- **`cleanResponse` is Telegram-specific** — Strips thinking block patterns before sending. If the bot is used via other entry points, thinking blocks will be visible.
- **`selfImprove` opens its own db connection** — Runs in a goroutine after response to avoid blocking the reply. Uses a separate `OpenDBForPath` call since the caller's connection is closed on return.