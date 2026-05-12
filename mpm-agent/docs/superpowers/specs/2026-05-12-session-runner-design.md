# Session Runner: Universal Agent Orchestrator

**Date:** 2026-05-12
**Status:** Approved

## Overview

Refactor `mpm-agent` from a Telegram-centric agent into a universal, IO-agnostic coding agent by abstracting the core message lifecycle into `core/session_runner.go`. Both the CLI and Telegram entry points share the same orchestrator, eliminating duplication and enabling a first-class terminal coding experience.

## Architecture

```
core/session_runner.go  ← universal orchestrator
    ├── command interception
    ├── session history management
    └── async selfImprove()
            ↑                              ↑
cmd/telegram/handler.go           cmd/mini-bot/main.go
    (Telegram transport)              (CLI transport)
                                     ├── readline multiline
                                     ├── ANSI streaming
                                     └── HITL tool approvals
```

---

## 1. Transport Interface (`core/transport.go`)

```go
// Transport abstracts all IO for session_runner.
// Each entry point (CLI, Telegram, MCP) implements this interface.
type Transport interface {
    // WriteChunk streams output. isThinking=true routes to stderr/dim.
    WriteChunk(text string, isThinking bool) error

    // RequestToolApproval blocks until user approves/rejects.
    // Returns true if approved, false if rejected.
    RequestToolApproval(toolName string, args string) bool

    // SendTypingIndicator signals "agent is thinking/working".
    SendTypingIndicator() error

    // StopTypingIndicator clears the typing signal.
    StopTypingIndicator() error
}
```

### CLI Implementation (`cmd/mini-bot/terminal_transport.go`)

- `WriteChunk` — isThinking=true → `fmt.Fprintf(os.Stderr, "\033[2m%s\033[0m", text)`; false → `fmt.Print(text)`
- `RequestToolApproval` — `fmt.Printf("\n[Y/n] Allow %s? ", toolName)` then `bufio.NewReader`. Default Y on Enter.
- `SendTypingIndicator` — print spinner to stderr

### Telegram Implementation (`cmd/telegram/transport.go`)

- `WriteChunk` — accumulate into buffer, send as needed
- `RequestToolApproval` — returns true (auto-approve for now)
- `SendTypingIndicator` — call `bot.Request(tgAPI.SendChatAction{ChatID: chatID, Action: "typing"})`

---

## 2. SessionRunner Struct (`core/session_runner.go`)

```go
type SessionRunner struct {
    transport    Transport
    sessionID    string
    profile      string  // "chat" | "coding"

    // Dependencies
    db           *sql.DB
    synthConfig  SynthConfig
    toolProfile  map[string]bool

    // Session state
    history      []Message
    sessionMgr   *SessionManager

    // Sub-systems
    selfImprover *SelfImprover
    identityPath string

    mu sync.Mutex
}
```

### Constructor

```go
func NewSessionRunner(
    transport Transport,
    sessionID string,
    initialProfile string,
    db *sql.DB,
    identityPath string,
) *SessionRunner
```

---

## 3. Command Reference

| Command | Behavior |
|---------|----------|
| `/new` | Clear history, reset session |
| `/clear` | Same as `/new` |
| `/tools <profile>` | Switch profile (`chat`/`coding`), reload toolkit |
| `/recall <topic>` | Query MPM memories about topic, inject into context |
| `/think` | Toggle think level (low/med/high) |
| `/status` | Show current profile, model, active toolkits |

---

## 4. Profile System

### Profiles

| Profile | Purpose | Toolset | LLM | Think Level |
|---------|---------|---------|-----|-------------|
| `chat` | General assistance | `files`, `web`, `mpm` | MiniMax (fast, cheap) | `med` |
| `coding` | Heavy-duty development | `coding_tools`, `apply_diff`, shell sandbox | OpenRouter (Claude 3.5 Sonnet) | `high` |

### Model Routing (`core/synth_router.go`)

```go
func RouteProfile(profile string) SynthConfig

// "chat" → MiniMax: BaseURL="https://api.minimax.io/anthropic/v1", Model="MiniMax-Text-01"
// "coding" → OpenRouter: BaseURL="https://openrouter.ai/api/v1", Model="anthropic/claude-3.5-sonnet"
```

Profile switching updates `r.synthConfig` and `r.toolProfile` in `updateProfile()`.

---

## 5. CLI Transport (`cmd/mini-bot/terminal_transport.go`)

```go
type TerminalTransport struct {
    writer     io.Writer
    errWriter  io.Writer
    approvalFn func(tool string) bool
}

func NewTerminalTransport(approvalFn func(tool string) bool) *TerminalTransport
func (t *TerminalTransport) WriteChunk(text string, isThinking bool) error
func (t *TerminalTransport) RequestToolApproval(toolName string, args string) bool
func (t *TerminalTransport) SendTypingIndicator() error
func (t *TerminalTransport) StopTypingIndicator() error
```

---

## 6. CLI REPL Loop (`cmd/mini-bot/main.go`)

- Uses `github.com/chzyer/readline` for input
- Triple-backtick (` ``` `) enters multiline mode; empty line delivers buffer
- Prompts: `mpm> ` (single line), `... ` (multiline)
- History file: `~/.mpm-agent-history`

### HITL Tool Approval

Risky tools (`execute_shell`, `write_file`, `apply_diff`, `execute_cmd_with_timeout`) trigger `transport.RequestToolApproval()` before execution. Default Y on Enter.

---

## 7. New Tool Stubs

### `apply_diff` (`core/tools_diff.go`)

Applies a unified diff to patch specific lines without overwriting whole files.

**Input:**
```json
{ "file": "/path/to/file.go", "diff": "--- a/file.go\n+++ b/file.go\n@@ -10,3 +10,4 @@\n old line\n+inserted line\n old line" }
```

**Output:** `"Patched N hunks in /path/to/file.go"`

### `execute_cmd_with_timeout` (`core/tools_shell.go`)

Runs a shell command with a strict timeout (max 300 seconds).

**Input:**
```json
{ "cmd": "go test ./... -v -count=1", "timeout_seconds": 60 }
```

**Output:**
```
[1.234s] Exit: 0
--- PASS: TestFoo (0.00s)
...
```

---

## 8. Refactoring Constraints

- `runAgentWithContext` is a **clean refactor** from `handler.go`, not a copy-paste
- Telegram-specific cruft (4096-char splitting, `<thinking>` stripping) is removed
- Raw chunks pass directly to `transport.WriteChunk()`
- `selfImprove()` opens its own `*sql.DB` connection to avoid SQLite lock contention

---

## Files to Create/Modify

| File | Action |
|------|--------|
| `core/transport.go` | Create |
| `core/session_runner.go` | Create |
| `cmd/mini-bot/terminal_transport.go` | Create |
| `cmd/telegram/transport.go` | Create |
| `core/tools_diff.go` | Create |
| `core/tools_shell.go` | Create |
| `core/synth_router.go` | Create |
| `cmd/mini-bot/main.go` | Refactor (add readline, multiline, profile switch) |
| `cmd/telegram/handler.go` | Refactor (use transport, delegate to session_runner) |
| `core/tools.go` | Register `apply_diff`, `execute_cmd_with_timeout` |
| `core/agent.go` | Extract `runAgentWithContext` logic |

---

## Out of Scope

- No external SDK integrations (Node.js, TypeScript, Pi SDK)
- No namespace sandboxing for file paths
- No complex per-profile config files
