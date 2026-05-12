# Session Runner CLI Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Complete the universal agent orchestrator by building the CLI transport, CLI REPL with readline multiline, the `apply_diff` and `execute_cmd_with_timeout` tool stubs, and wiring the Telegram handler to use the transport interface.

**Architecture:** The Transport interface (`core/transport.go`) is implemented by both `TerminalTransport` (CLI) and `TelegramTransport` (Telegram). The SessionRunner (`core/session_runner.go`) is already built and handles all orchestration. Each entry point implements Transport to bridge OS/UI specifics.

**Tech Stack:** Pure Go, `github.com/chzyer/readline` for CLI input, standard `bufio`/`fmt` for terminal I/O, Go's `os/exec` for shell commands.

---

## File Structure

```
core/
  transport.go         # Already created: Transport interface
  session_runner.go    # Already created: orchestrator + RouteProfile + helpers
  tools_diff.go        # NEW: apply_diff tool
  tools_shell.go       # NEW: execute_cmd_with_timeout tool

cmd/mini-bot/
  terminal_transport.go  # NEW: Transport impl for CLI
  main.go               # MODIFY: refactor to use SessionRunner + readline

cmd/telegram/
  transport.go           # NEW: Transport impl for Telegram
  handler.go            # MODIFY: delegate to SessionRunner
```

---

## Task 1: TerminalTransport (`cmd/mini-bot/terminal_transport.go`)

**Files:**
- Create: `cmd/mini-bot/terminal_transport.go`

- [ ] **Step 1: Write the file**

```go
package main

import (
	"bufio"
	"fmt"
	"io"
	"os"
	"strings"
)

// TerminalTransport implements core.Transport for the CLI REPL.
type TerminalTransport struct {
	writer    io.Writer
	errWriter io.Writer
}

var _ core.Transport = (*TerminalTransport)(nil)

// NewTerminalTransport creates a TerminalTransport.
func NewTerminalTransport() *TerminalTransport {
	return &TerminalTransport{
		writer:    os.Stdout,
		errWriter: os.Stderr,
	}
}

// ReadMessage reads a line from stdin.
func (t *TerminalTransport) ReadMessage() (string, error) {
	reader := bufio.NewReader(os.Stdin)
	line, err := reader.ReadString('\n')
	if err != nil {
		return "", err
	}
	return strings.TrimSuffix(line, "\n"), nil
}

// WriteChunk streams output. isThinking=true writes to stderr in dim ANSI.
func (t *TerminalTransport) WriteChunk(text string, isThinking bool) error {
	if isThinking {
		// ANSI dim: code 2 (not bold), color 90 (bright black)
		fmt.Fprintf(t.errWriter, "\033[2m%s\033[0m", text)
	} else {
		fmt.Fprint(t.writer, text)
	}
	return nil
}

// RequestToolApproval prompts the user for approval.
// Default Y on Enter (empty input).
func (t *TerminalTransport) RequestToolApproval(toolName string, args string) bool {
	fmt.Fprintf(os.Stderr, "\n\033[33m⚠️  Tool: %s\033[0m\nArgs: %s\n[Y/n] ", toolName, args)
	reader := bufio.NewReader(os.Stderr)
	line, _ := reader.ReadString('\n')
	line = strings.TrimSpace(strings.ToLower(line))
	return line != "n"
}

// SendTypingIndicator writes a spinner to stderr.
func (t *TerminalTransport) SendTypingIndicator() error {
	fmt.Fprint(t.errWriter, "\033[s🤖 working...\033[u")
	return nil
}

// StopTypingIndicator clears the spinner line.
func (t *TerminalTransport) StopTypingIndicator() error {
	fmt.Fprint(t.errWriter, "\r\033[K")
	return nil
}
```

- [ ] **Step 2: Add missing import**

The file needs `strings` but the import block is missing `core` and `os`. The actual implementation above uses `os` and `strings` directly, and `core` is used only for the type assertion. Verify the file compiles.

Run: `go build ./cmd/mini-bot/... 2>&1`

---

## Task 2: apply_diff Tool (`core/tools_diff.go`)

**Files:**
- Create: `core/tools_diff.go`
- Modify: `core/tools.go` (register the tool in `init()`)

- [ ] **Step 1: Write the failing test**

```go
package core

import (
	"os"
	"testing"
)

func TestApplyDiff(t *testing.T) {
	// Create a temp file
	tmp, _ := os.CreateTemp("", "test_diff_*.txt")
	defer os.Remove(tmp.Name())
	tmp.WriteString("line1\nline2\nline3\n")
	tmp.Close()

	diff := fmt.Sprintf("--- a\n+++ b\n@@ -1,3 +1,4 @@\n line1\n+inserted\n line2\n line3\n")

	result, err := applyDiff(map[string]interface{}{
		"file": tmp.Name(),
		"diff": diff,
	})
	if err != nil {
		t.Fatalf("applyDiff failed: %v", err)
	}
	if !strings.Contains(result, "Patched") {
		t.Errorf("expected Patched in result, got: %s", result)
	}

	// Verify content
	content, _ := os.ReadFile(tmp.Name())
	if !strings.Contains(string(content), "inserted") {
		t.Errorf("expected 'inserted' in file, got: %s", string(content))
	}
}
```

Run: `go test ./core/... -run TestApplyDiff -v`
Expected: FAIL — `applyDiff` not defined

- [ ] **Step 2: Write applyDiff implementation**

```go
package core

import (
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"
)

// applyDiff applies a unified diff to a file without overwriting.
// Input: { "file": "/path", "diff": "unified diff string" }
func applyDiff(args map[string]interface{}) (string, error) {
	file, _ := args["file"].(string)
	diff, _ := args["diff"].(string)
	if file == "" || diff == "" {
		return "", fmt.Errorf("apply_diff requires 'file' and 'diff' fields")
	}

	absPath, err := filepath.Abs(file)
	if err != nil {
		return "", fmt.Errorf("apply_diff: invalid path: %w", err)
	}

	content, err := os.ReadFile(absPath)
	if err != nil {
		return "", fmt.Errorf("apply_diff: read file: %w", err)
	}

	patch, err := parseUnifiedDiff(diff)
	if err != nil {
		return "", fmt.Errorf("apply_diff: parse diff: %w", err)
	}

	newContent, err := applyUnifiedDiff(string(content), patch)
	if err != nil {
		return "", fmt.Errorf("apply_diff: apply patch: %w", err)
	}

	if string(content) != newContent {
		if err := os.WriteFile(absPath, []byte(newContent), 0644); err != nil {
			return "", fmt.Errorf("apply_diff: write file: %w", err)
		}
		return fmt.Sprintf("Patched %d hunks in %s", len(patch.Hunks), absPath), nil
	}
	return "No changes needed", nil
}

// diffPatch holds a parsed unified diff.
type diffPatch struct {
	OldFile, NewFile string
	Hunks            []diffHunk
}

type diffHunk struct {
	OldStart, OldCount, NewStart, NewCount int
	Lines []string // '+' prefix = add, '-' prefix = delete, ' ' prefix = context
}

var hunkHeaderRE = regexp.MustCompile(`@@ -(\d+)(?:,(\d+))? \+(\d+)(?:,(\d+))? @@`)

func parseUnifiedDiff(diff string) (*diffPatch, error) {
	patch := &diffPatch{}
	lines := strings.Split(diff, "\n")
	var currentHunk *diffHunk

	for i, line := range lines {
		if m := hunkHeaderRE.FindStringSubmatch(line); m != nil {
			oldStart, _ := strconv.Atoi(m[1])
			oldCount, _ := strconv.Atoi(m[2])
			if oldCount == 0 {
				oldCount = 1
			}
			newStart, _ := strconv.Atoi(m[3])
			newCount, _ := strconv.Atoi(m[4])
			if newCount == 0 {
				newCount = 1
			}
			currentHunk = &diffHunk{
				OldStart: oldStart, OldCount: oldCount,
				NewStart: newStart, NewCount: newCount,
			}
			patch.Hunks = append(patch.Hunks, *currentHunk)
			continue
		}

		if currentHunk == nil {
			// Collect file headers
			if strings.HasPrefix(line, "--- ") {
				patch.OldFile = strings.TrimPrefix(line, "--- ")
			} else if strings.HasPrefix(line, "+++ ") {
				patch.NewFile = strings.TrimPrefix(line, "+++ ")
			}
			continue
		}

		if len(line) == 0 {
			continue
		}
		switch line[0] {
		case '+', '-', ' ':
			currentHunk.Lines = append(currentHunk.Lines, line)
		}
	}
	return patch, nil
}

func applyUnifiedDiff(content string, patch *diffPatch) (string, error) {
	if len(patch.Hunks) == 0 {
		return content, nil
	}

	// Process each hunk — for simplicity, we rebuild lines array
	// and apply hunk changes. This is a simplified implementation.
	// A full production version would use a proper diff library.
	origLines := strings.Split(content, "\n")
	result := make([]string, 0, len(origLines))

	for hIdx, hunk := range patch.Hunks {
		// Calculate hunk region in original file (1-indexed → 0-indexed)
		hunkEnd := hunk.OldStart + hunk.OldCount - 1

		// Copy lines before this hunk
		for i := len(result); i < hunk.OldStart-1 && i < len(origLines); i++ {
			result = append(result, origLines[i])
		}

		// Apply hunk changes
		var offset int
		for _, l := range hunk.Lines {
			switch l[0] {
			case '+':
				result = append(result, l[1:])
				offset++
			case '-':
				// Skip old line — consume from orig
				// Skip in origLines by advancing our position tracker
				origPos := hunk.OldStart - 1 + offset
				if origPos < len(origLines) {
					// skip
				}
				offset--
			case ' ':
				// Copy context line from orig
				origPos := hunk.OldStart - 1 + offset
				if origPos < len(origLines) {
					result = append(result, origLines[origPos])
					offset++
				}
			}
		}
		if hIdx == len(patch.Hunks)-1 {
			// Copy remaining lines after last hunk
			origPos := hunkEnd + 1
			for i := origPos; i < len(origLines); i++ {
				result = append(result, origLines[i])
			}
		}
	}

	return strings.Join(result, "\n"), nil
}
```

Note: The above `applyUnifiedDiff` is a simplified stub. For production, use `github.com/skeema/knownfile` or similar.

- [ ] **Step 3: Register apply_diff in core/tools.go init()**

In `core/tools.go`, add to the `init()` function:

```go
RegisterTool("apply_diff", ToolDefinition{
    Name:        "apply_diff",
    Description: "Apply a unified diff to patch specific lines in a file without overwriting",
    InputSchema: map[string]interface{}{
        "type": "object",
        "properties": map[string]interface{}{
            "file": map[string]interface{}{"type": "string", "description": "Absolute path to file"},
            "diff": map[string]interface{}{"type": "string", "description": "Unified diff string"},
        },
        "required": []string{"file", "diff"},
    },
})
```

Also add to the `executeTool` switch case: `case "apply_diff": return applyDiff(args)`.

- [ ] **Step 4: Run test**

Run: `go test ./core/... -run TestApplyDiff -v`
Expected: PASS

- [ ] **Step 5: Commit**

```bash
git add core/tools_diff.go core/tools.go
git commit -m "feat: add apply_diff tool for unified diff patching"
```

---

## Task 3: execute_cmd_with_timeout Tool (`core/tools_shell.go`)

**Files:**
- Create: `core/tools_shell.go`
- Modify: `core/tools.go` (register the tool)

- [ ] **Step 1: Write the tool**

```go
package core

import (
	"context"
	"fmt"
	"os/exec"
	"time"
)

// executeCmdWithTimeout runs a shell command with a strict timeout (max 300s).
// Input: { "cmd": "go test ./...", "timeout_seconds": 60 }
// Output: "[elapsed] Exit: N\nstdout\nstderr"
func executeCmdWithTimeout(args map[string]interface{}) (string, error) {
	cmdStr, _ := args["cmd"].(string)
	timeoutSec, ok := args["timeout_seconds"].(float64)
	if !ok || timeoutSec <= 0 {
		timeoutSec = 60
	}
	if timeoutSec > 300 {
		return "", fmt.Errorf("execute_cmd_with_timeout: timeout cannot exceed 300 seconds (security limit)")
	}

	if cmdStr == "" {
		return "", fmt.Errorf("execute_cmd_with_timeout requires 'cmd' field")
	}

	ctx, cancel := context.WithTimeout(context.Background(), time.Duration(timeoutSec)*time.Second)
	defer cancel()

	start := time.Now()
	cmd := exec.CommandContext(ctx, "sh", "-c", cmdStr)
	cmd.Dir, _ = os.Getwd()

	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr

	err := cmd.Run()
	elapsed := time.Since(start)

	result := fmt.Sprintf("[%s] Exit: %d\n%s%s",
		elapsed.Round(time.Millisecond),
		exitCodeFromError(err),
		stderr.String(),
		stdout.String())

	if ctx.Err() == context.DeadlineExceeded {
		return result + "\n⏱ TIMEOUT", fmt.Errorf("command exceeded %v timeout", time.Duration(timeoutSec)*time.Second)
	}
	return result, err
}

func exitCodeFromError(err error) int {
	if err == nil {
		return 0
	}
	if exitErr, ok := err.(*exec.ExitError); ok {
		return exitErr.ExitCode()
	}
	return -1
}
```

Also add `"bytes"` and `"os"` to the import block of `core/tools_shell.go`. Wait, `os.Getwd` uses `os` package — already in scope since `executeTool` is in the same package. Add `"bytes"` for the buffers.

- [ ] **Step 2: Register in core/tools.go init()**

```go
RegisterTool("execute_cmd_with_timeout", ToolDefinition{
    Name:        "execute_cmd_with_timeout",
    Description: "Run a shell command with a strict timeout (max 300s)",
    InputSchema: map[string]interface{}{
        "type": "object",
        "properties": map[string]interface{}{
            "cmd": map[string]interface{}{
                "type":        "string",
                "description": "Shell command to execute",
            },
            "timeout_seconds": map[string]interface{}{
                "type":        "number",
                "description": "Timeout in seconds (max 300)",
            },
        },
        "required": []string{"cmd"},
    },
})
```

And add to `executeTool` switch: `case "execute_cmd_with_timeout": return executeCmdWithTimeout(args)`.

- [ ] **Step 3: Build to verify**

Run: `go build ./core/... 2>&1`
Expected: no errors

- [ ] **Step 4: Commit**

```bash
git add core/tools_shell.go core/tools.go
git commit -m "feat: add execute_cmd_with_timeout tool with 300s security limit"
```

---

## Task 4: CLI REPL with Readline Multiline (`cmd/mini-bot/main.go`)

**Files:**
- Modify: `cmd/mini-bot/main.go`

- [ ] **Step 1: Read current main.go**

```bash
cat cmd/mini-bot/main.go
```

- [ ] **Step 2: Refactor to use SessionRunner + readline**

Replace the current `runREPL` function and `main()` with:

```go
package main

import (
	"context"
	"fmt"
	"io"
	"os"
	"os/signal"
	"strings"
	"syscall"

	"github.com/chzyer/readline"

	"mpm-agent/core"
)

type inputMode int

const (
	singleLine inputMode = iota
	multiline
)

func runREPL(ctx context.Context, transport *TerminalTransport) error {
	// Load config and db
	cfg, _ := core.LoadMiniBotConfig(core.GetConfigPath())
	if cfg == nil {
		cfg = core.DefaultMiniBotConfig()
	}

	dbPath := core.ResolveMiniBotDBPath()
	db, err := core.OpenDBForPath(dbPath)
	if err != nil {
		return fmt.Errorf("open db: %w", err)
	}
	defer db.Close()

	// Create persistence layer for CLI (stateless per-run)
	session := &cliPersistence{sessionID: "cli:" + hostname()}
	session.db = db

	// Create identity resolver
	identityResolver := &core.IdentityResolverFunc{}

	// Create session runner
	runner := core.NewSessionRunner(
		transport,
		session.SessionID(),
		"chat", // default profile
		db,
		session,
		identityResolver,
	)

	// Set up readline
	rl, err := readline.NewEx(&readline.Config{
		Prompt:       "\033[36mmpm\033[0m> ",
		HistoryFile:  os.ExpandEnv("$HOME/.mpm-agent-history"),
		EnableMask:   true,
		UniqueEditLine: true,
	})
	if err != nil {
		return fmt.Errorf("readline init: %w", err)
	}
	defer rl.Close()

	var multilineBuf strings.Builder
	mode := singleLine

	// Handle SIGINT gracefully
	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, syscall.SIGINT)
	go func() {
		<-sigCh
		rl.Close()
		os.Exit(0)
	}()

	for {
		line, err := rl.Readline()
		if err == io.EOF {
			break
		}
		if err != nil {
			if err.Error() == " Interrupt" {
				continue
			}
			break
		}

		// Triple-quote toggle
		if strings.HasPrefix(line, "```") {
			if mode == singleLine {
				mode = multiline
				multilineBuf.Reset()
				rl.SetPrompt("\033[33m... \033[0m")
			} else {
				// Deliver multiline buffer
				input := multilineBuf.String()
				multilineBuf.Reset()
				mode = singleLine
				rl.SetPrompt("\033[36mmpm\033[0m> ")
				if err := runner.HandleInput(ctx, input); err != nil {
					fmt.Fprintf(os.Stderr, "\033[31mError: %v\033[0m\n", err)
				}
			}
			continue
		}

		if mode == multiline {
			multilineBuf.WriteString(line)
			multilineBuf.WriteByte('\n')
		} else {
			if err := runner.HandleInput(ctx, line); err != nil {
				fmt.Fprintf(os.Stderr, "\033[31mError: %v\033[0m\n", err)
			}
		}
	}
	return nil
}

// cliPersistence is a minimal Persistence implementation for CLI sessions.
type cliPersistence struct {
	sessionID string
	db        *sql.DB
}

func (p *cliPersistence) Get() ([]map[string]interface{}, error) {
	// CLI sessions are not persisted between runs
	return nil, nil
}

func (p *cliPersistence) Save(messages []map[string]interface{}) error {
	// No-op for CLI (stateless)
	return nil
}

func (p *cliPersistence) SessionID() string {
	return p.sessionID
}

func hostname() string {
	h, _ := os.Hostname()
	return h
}
```

The `main()` function simplifies to:

```go
func main() {
	ctx := context.Background()
	transport := NewTerminalTransport()
	if err := runREPL(ctx, transport); err != nil {
		fmt.Fprintf(os.Stderr, "REPL error: %v\n", err)
		os.Exit(1)
	}
}
```

Note: You need to add `"database/sql"` to the imports in `cmd/mini-bot/main.go`, and add the `"github.com/mattn/go-sqlite3"` import for the blank import (used in `core.OpenDBForPath`). Also add `core` import.

- [ ] **Step 3: Build and verify**

Run: `go build ./cmd/mini-bot/... 2>&1`
Expected: no errors. If `readline` is missing: `go get github.com/chzyer/readline`

- [ ] **Step 4: Commit**

```bash
git add cmd/mini-bot/main.go
git commit -m "refactor: wire CLI REPL to SessionRunner with readline multiline support"
```

---

## Task 5: TelegramTransport (`cmd/telegram/transport.go`)

**Files:**
- Create: `cmd/telegram/transport.go`

- [ ] **Step 1: Write the transport**

```go
package main

import (
	"context"
	"fmt"
	"strings"

	"github.com/mymmrac/telego"
	"github.com/mymmrac/telego/telegoutil"
)

// TelegramTransport implements core.Transport for the Telegram bot.
type TelegramTransport struct {
	bot    *telego.Bot
	chatID int64
}

var _ core.Transport = (*TelegramTransport)(nil)

// NewTelegramTransport creates a TelegramTransport.
func NewTelegramTransport(bot *telego.Bot, chatID int64) *TelegramTransport {
	return &TelegramTransport{bot: bot, chatID: chatID}
}

// ReadMessage is not used in Telegram (messages arrive via handler).
func (t *TelegramTransport) ReadMessage() (string, error) {
	return "", nil
}

// WriteChunk accumulates chunks and sends as a single message.
// For streaming, TelegramTransport should delegate to the Handler's live message system.
// This is a simplified implementation for now.
func (t *TelegramTransport) WriteChunk(text string, isThinking bool) error {
	// Strip thinking blocks (matches handler.go cleanResponse behavior)
	// The handler already handles this, but we ensure raw chunks are sent
	_ = isThinking
	_, err := t.bot.SendMessage(context.Background(), telegoutil.Message(
		telegoutil.ID(t.chatID),
		telegoutil.Text(text),
	))
	return err
}

// RequestToolApproval for Telegram: auto-approve for now (returns true).
func (t *TelegramTransport) RequestToolApproval(toolName string, args string) bool {
	// TODO: Implement per-chat HITL setting toggle
	return true
}

// SendTypingIndicator sends the typing action to Telegram.
func (t *TelegramTransport) SendTypingIndicator() error {
	return t.bot.SendChatAction(context.Background(), &telego.SendChatActionParams{
		ChatID: telegoutil.ID(t.chatID),
		Action: telego.ChatActionTyping,
	})
}

// StopTypingIndicator sends a cancel action.
func (t *TelegramTransport) StopTypingIndicator() error {
	return t.bot.SendChatAction(context.Background(), &telego.SendChatActionParams{
		ChatID: telelegoutil.ID(t.chatID),
		Action: telego.ChatActionTyping,
	})
}
```

Fix the typo: `telelegoutil` → `telegoutil`.

- [ ] **Step 2: Build to verify**

Run: `go build ./cmd/telegram/... 2>&1`
Expected: no errors

- [ ] **Step 3: Commit**

```bash
git add cmd/telegram/transport.go
git commit -m "feat: add TelegramTransport implementing core.Transport"
```

---

## Task 6: Wire Telegram Handler to SessionRunner

**Files:**
- Modify: `cmd/telegram/handler.go`

- [ ] **Step 1: Identify the refactor points**

The goal is to make `runAgentWithTimeout` in `handler.go` delegate to `core.SessionRunner` instead of calling `core.RunAgent` directly. The key changes:

1. In `runAgentWithTimeout`, replace the `core.RunAgent(...)` call with a `core.SessionRunner` + `TelegramTransport` setup.
2. The `TelegramTransport` should be passed to `NewSessionRunner`.
3. The `Handler` should hold a `*core.SessionRunner` per chat, or create one per message.

For simplicity (given Telegram's chat-per-session model), create a new `SessionRunner` per message using a `TelegramTransport` that writes to the live message buffer.

However, the existing `Handler` already has complex live message streaming (`liveMessageData`, `flusher`). The cleanest approach is to keep `runAgentWithTimeout` largely intact but have it call through to a shared `SessionRunner` that uses the TelegramTransport.

The key refactor: replace `core.RunAgent(...)` with a call that goes through `SessionRunner`, but the Telegram handler's live message system intercepts `WriteChunk` to do streaming edits.

- [ ] **Step 2: Create a live-message-aware TelegramTransport**

Create a `cmd/telegram/live_transport.go`:

```go
package main

import (
	"context"
	"fmt"
	"strings"
	"sync"

	"github.com/mymmrac/telego"
	"github.com/mymmrac/telego/telegoutil"
)

// liveTransport implements core.Transport for Telegram with live message streaming.
type liveTransport struct {
	bot      *telego.Bot
	chatID   int64
	liveMsg  *liveMessageData
	handler  *Handler
	mu       sync.Mutex
}

var _ core.Transport = (*liveTransport)(nil)

func newLiveTransport(bot *telego.Bot, chatID int64, h *Handler) *liveTransport {
	return &liveTransport{bot: bot, chatID: chatID, handler: h}
}

func (t *liveTransport) ReadMessage() (string, error) {
	return "", nil // Not used — messages arrive via Handle()
}

func (t *liveTransport) WriteChunk(text string, isThinking bool) error {
	// Strip thinking blocks if not verbose (matches cleanResponse)
	if !t.handler.getSettings(t.chatID).verbose {
		text = cleanResponse(text)
	}
	if text == "" {
		return nil
	}

	entry := t.handler.getOrCreateLiveMessage(t.chatID)
	if entry == nil {
		return nil
	}
	entry.mu.Lock()
	entry.buffer.WriteString(text)
	entry.dirty = true
	entry.mu.Unlock()
	return nil
}

func (t *liveTransport) RequestToolApproval(toolName string, args string) bool {
	return true // Auto-approve for Telegram
}

func (t *liveTransport) SendTypingIndicator() error {
	entry := t.handler.getOrCreateLiveMessage(t.chatID)
	if entry != nil {
		entry.SetStatus(statusThinking)
	}
	return nil
}

func (t *liveTransport) StopTypingIndicator() error {
	entry := t.handler.getLiveMessage(t.chatID)
	if entry != nil {
		entry.SetStatus(statusDone)
	}
	return nil
}
```

- [ ] **Step 3: Replace runAgentWithTimeout to use SessionRunner**

In `handler.go`, modify `runAgentWithTimeout` to create a SessionRunner with the liveTransport:

```go
// In runAgentWithTimeout, replace the RunAgent call with:
transport := newLiveTransport(h.bot, chatID, h)
session := &telegramPersistence{sm: h.sm, chatID: chatID}
identityResolver := &handlerIdentityResolver{h: h}
runner := core.NewSessionRunner(
    transport,
    fmt.Sprintf("telegram:%d", chatID),
    h.getSettings(chatID).toolProfile,
    db,
    session,
    identityResolver,
)

responseText, usedAnchors, err := runner.RunAgent(ctx, userText, history)
```

This requires defining `telegramPersistence` and `handlerIdentityResolver` in `handler.go`, and exposing `RunAgent` as a public method on `SessionRunner`. Currently `runAgentWithContext` is private — expose it as:

```go
// RunAgent is the public entry point for a single agent call.
func (r *SessionRunner) RunAgent(ctx context.Context, query string, history []map[string]interface{}) (string, []core.Anchor, error) {
    // ... existing runAgentWithContext logic ...
}
```

- [ ] **Step 4: Build and verify**

Run: `go build ./cmd/telegram/... 2>&1`
Expected: no errors. May need to add `sync` import to `handler.go`.

- [ ] **Step 5: Commit**

```bash
git add cmd/telegram/handler.go cmd/telegram/live_transport.go
git commit -m "refactor: wire Telegram handler to SessionRunner via liveTransport"
```

---

## Verification

After all tasks:
- `go build ./core/...` — no errors
- `go build ./cmd/mini-bot/...` — no errors  
- `go build ./cmd/telegram/...` — no errors
- `go test ./core/... -run TestApplyDiff -v` — PASS