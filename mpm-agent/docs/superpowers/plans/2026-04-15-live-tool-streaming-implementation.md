# Live Tool Streaming — Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Stream tool progress from the agent loop to a live Telegram message (edit-in-place), with risk alerts for dangerous operations.

**Architecture:** Add a `ToolProgressReporter` interface to `core/agent.go`, implement it in `cmd/telegram/handler.go` with a background flusher goroutine, wrap all tool execution with reporter calls, wire it up in `runAgentWithTimeout`, add `/stream` toggle.

**Tech Stack:** Go (telego Telegram library), `sync.Map`, `strings.Builder`, `regexp` for risk detection.

---

## File Map

| File | Change |
|------|--------|
| `core/agent.go` | MODIFY — add reporter interface, update `RunAgent` sig, wrap tool execution, add `summarize()` and `isRiskyOperation()` |
| `cmd/telegram/handler.go` | MODIFY — implement `ToolProgressReporter`, add flusher/liveMessage types, update `runAgentWithTimeout`, add `/stream` |

---

## Task 1: Add `ToolProgressReporter` interface and reporter param to `RunAgent`

**Files:**
- Modify: `core/agent.go:215`

- [ ] **Step 1: Add the `ToolProgressReporter` interface and `summarize` function near the top of `agent.go`**

After the existing type definitions (around line 190), add:

```go
// ToolProgressReporter receives tool execution events for streaming to UI.
// Nil reporter means no streaming — fully backward-compatible.
type ToolProgressReporter interface {
    // ToolStarted is called before executing a tool. Returns a reportID
    // used to correlate ToolStarted/ToolCompleted calls.
    ToolStarted(chatID int64, toolName string, input map[string]interface{}) string
    // ToolCompleted is called after a tool finishes with a one-line summary.
    ToolCompleted(chatID int64, reportID string, toolName string, summary string)
    // SendAlert posts a standalone risk warning to the chat (bypasses streaming).
    SendAlert(chatID int64, message string)
}

// summarize produces a one-line summary of a tool result.
func summarize(toolName string, result string, err error) string {
    if err != nil {
        return fmt.Sprintf("✗ %s: %v", toolName, err)
    }
    switch toolName {
    case "read_file":
        // result is raw file content
        lines := strings.Count(result, "\n") + 1
        return fmt.Sprintf("✓ read %d lines", lines)
    case "write_file":
        // result is "Wrote N bytes to <path>" from executeTool
        return "✓ wrote"
    case "execute_shell":
        // result is stdout
        lines := strings.Count(result, "\n") + 1
        if err != nil {
            return fmt.Sprintf("✗ exit 1: %v", truncate(result, 80))
        }
        return fmt.Sprintf("✓ %d lines output", lines)
    }
    // Default: first line of result, truncated
    return truncate(result, 60)
}

func truncate(s string, max int) string {
    if len(s) <= max {
        return s
    }
    return s[:max] + "..."
}
```

- [ ] **Step 2: Update `RunAgent` signature — add `chatID` and `reporter` params**

In `core/agent.go:215`, change:

```go
func RunAgent(ctx context.Context, query string, history []map[string]interface{}, db *sql.DB, identityPath string, cfg *SynthConfig, toolProfile []string, sessionID string, toolkitMap map[string][]string) (string, error)
```

To:

```go
func RunAgent(ctx context.Context, query string, history []map[string]interface{}, db *sql.DB, identityPath string, cfg *SynthConfig, toolProfile []string, sessionID string, toolkitMap map[string][]string, chatID int64, reporter ToolProgressReporter) (string, error)
```

- [ ] **Step 3: Wrap tool execution with reporter calls**

In the tool execution section of `RunAgent` (around line 302), after `case "execute_mpm_command"` and in the `default` case that calls `executeTool`, wrap the execution:

In the `default` case (around line 303):
```go
default:
    // Local tools from core/tools.go
    reportID := ""
    if reporter != nil {
        reportID = reporter.ToolStarted(chatID, tc.Name, tc.Input)
    }
    result, err = executeTool(tc.Name, tc.Input, sessionID)
    if reporter != nil {
        summary := summarize(tc.Name, result, err)
        reporter.ToolCompleted(chatID, reportID, tc.Name, summary)
        if isRiskyOperation(tc.Name, tc.Input, result, err) {
            reporter.SendAlert(chatID, riskWarning(tc.Name, tc.Input, result))
        }
    }
```

Similarly wrap the `case "execute_mpm_command"` section (around line 299):
```go
case "execute_mpm_command":
    reportID := ""
    if reporter != nil {
        reportID = reporter.ToolStarted(chatID, "execute_mpm_command", tc.Input)
    }
    cmd, _ := tc.Input["command"].(string)
    result, err = mpmExec(cmd)
    if reporter != nil {
        summary := summarize("execute_mpm_command", result, err)
        reporter.ToolCompleted(chatID, reportID, "execute_mpm_command", summary)
    }
```

- [ ] **Step 4: Add `isRiskyOperation` and `riskWarning` functions**

Add these near the bottom of `agent.go` (after `RunAgent`):

```go
var riskyCommandPatterns = []*regexp.Regexp{
    regexp.MustCompile(`(?i)\brm\s+-(rf|r)\b`),
    regexp.MustCompile(`(?i)git\s+push\s+.*--force`),
    regexp.MustCompile(`(?i)\bdd\b.*\bof=`),
    regexp.MustCompile(`(?i)(mkfs|shred|wipe)\s`),
    regexp.MustCompile(`(?i)(chmod|chown)\s+777`),
    regexp.MustCompile(`(?i)sudo\s+rm\s+`),
    regexp.MustCompile(`(?i):\(\)\{.*:\|.*:\}`), // fork bomb
}

var riskyWritePaths = []*regexp.Regexp{
    regexp.MustCompile(`(?i)(^|/)(\.ssh|aws|credentials|secrets|env)($|/)`),
    regexp.MustCompile(`(?i)/etc/|/sys/|/proc/`),
}

func isRiskyOperation(toolName string, input map[string]interface{}, result string, err error) bool {
    switch toolName {
    case "execute_shell":
        cmd, _ := input["command"].(string)
        for _, re := range riskyCommandPatterns {
            if re.MatchString(cmd) {
                return true
            }
        }
    case "write_file":
        path, _ := input["path"].(string)
        for _, re := range riskyWritePaths {
            if re.MatchString(path) {
                return true
            }
        }
    }
    return false
}

func riskWarning(toolName string, input map[string]interface{}, result string) string {
    switch toolName {
    case "execute_shell":
        cmd, _ := input["command"].(string)
        return fmt.Sprintf("⚠️ DANGEROUS: execute_shell running `%s`", truncate(cmd, 100))
    case "write_file":
        path, _ := input["path"].(string)
        return fmt.Sprintf("⚠️ RISKY WRITE: writing to %s", path)
    }
    return fmt.Sprintf("⚠️ RISKY: %s", toolName)
}
```

- [ ] **Step 5: Commit**

```bash
cd /home/v/.openclaw/workspace/projects/mpm/mpm-agent
git add core/agent.go
git commit -m "feat(agent): add ToolProgressReporter interface and risk detection"
```

---

## Task 2: Implement `ToolProgressReporter` in handler.go

**Files:**
- Modify: `cmd/telegram/handler.go` (add types, flusher, reporter impl, new methods)
- Modify: `cmd/telegram/handler.go:37-52` (chatSettings struct)
- Modify: `cmd/telegram/handler.go:392-495` (runAgentWithTimeout)
- Modify: `cmd/telegram/handler.go:220-243` (handleCommand switch)
- Modify: `cmd/telegram/handler.go:381-386` (/help text)

- [ ] **Step 1: Add streaming types and fields to Handler struct**

Add after the `mediaDir` field initialization in `NewHandler` (around line 71):

```go
// liveMessage tracks the live-edit message per chat
liveMessage sync.Map // chatID (int64) → *liveMessageData
```

Add after the handler struct definition (around line 52):

```go
// liveMessageData holds the state for a live-streaming message in one chat.
type liveMessageData struct {
    messageID int64
    buffer   strings.Builder
    mu       sync.Mutex
    dirty    bool           // buffer changed since last flush
    flusher  *flusher       // stop via flusher field to prevent goroutine leak
}

// flusher runs in background, flushes buffer edits to Telegram at ~1.1s interval.
// Does NOT block the agent loop — Telegram API calls happen in this goroutine.
type flusher struct {
    handler *Handler
    chatID  int64
    ticker  *time.Ticker
    stopCh  chan struct{}
    doneCh  chan struct{} // closed when goroutine fully exits (defer runs after in-flight HTTP)
}

func newFlusher(h *Handler, chatID int64) *flusher {
    f := &flusher{
        handler: h,
        chatID:  chatID,
        ticker:  time.NewTicker(1100 * time.Millisecond),
        stopCh:  make(chan struct{}),
        doneCh:  make(chan struct{}),
    }
    go f.run()
    return f
}

func (f *flusher) run() {
    defer close(f.doneCh) // goroutine fully exited only after defer runs (HTTP response included)
    for {
        select {
        case <-f.ticker.C:
            f.handler.flushLiveMessage(f.chatID)
        case <-f.stopCh:
            return
        }
    }
}

func (f *flusher) stop() {
    close(f.stopCh)
    f.ticker.Stop()
}

// telegramToolReporter implements core.ToolProgressReporter for the Telegram handler.
type telegramToolReporter struct {
    h      *Handler
    chatID int64
}

func (r *telegramToolReporter) ToolStarted(toolName string, input map[string]interface{}) string {
    entry := r.h.getOrCreateLiveMessage(r.chatID)
    if entry == nil {
        return ""
    }
    entry.mu.Lock()
    if entry.buffer.Len() == 0 {
        entry.buffer.WriteString("🧠 Working...\n")
    }
    entry.buffer.WriteString(fmt.Sprintf("🔧 %s...\n", toolName))
    entry.dirty = true
    entry.mu.Unlock()
    return toolName // reportID = toolName
}

func (r *telegramToolReporter) ToolCompleted(reportID string, toolName string, summary string) {
    entry := r.h.getLiveMessage(r.chatID)
    if entry == nil {
        return
    }
    entry.mu.Lock()
    // Append the summary in-place (replaces the "🔧 tool..." placeholder)
    // Simple approach: append after the last "🔧 tool..." line — just append, flusher will sync
    entry.buffer.WriteString(fmt.Sprintf("  %s\n", summary))
    entry.dirty = true
    entry.mu.Unlock()
    // DO NOT call flush here — flusher goroutine handles Telegram API calls
}

func (r *telegramToolReporter) SendAlert(message string) {
    // Standalone message — sent immediately, bypasses flusher
    r.h.sendText(nil, r.chatID, message)
}

// getLiveMessage returns existing liveMessageData for chatID without creating one.
// Returns nil if no live message is in progress.
func (h *Handler) getLiveMessage(chatID int64) *liveMessageData {
    v, ok := h.liveMessage.Load(chatID)
    if !ok {
        return nil
    }
    return v.(*liveMessageData)
}

// getOrCreateLiveMessage returns existing liveMessageData or creates one + starts flusher.
func (h *Handler) getOrCreateLiveMessage(chatID int64) *liveMessageData {
    v, ok := h.liveMessage.Load(chatID)
    if ok {
        return v.(*liveMessageData)
    }
    sent, err := h.bot.SendMessage(context.Background(), tu.Message(tu.ID(chatID), "🧠 Working..."))
    if err != nil {
        log.Printf("[telegram] live message init error: %v", err)
        return nil
    }
    data := &liveMessageData{messageID: sent.MessageID}
    data.flusher = newFlusher(h, chatID) // flusher stored on data — not orphaned
    h.liveMessage.Store(chatID, data)
    return data
}

// flushLiveMessage sends buffer to Telegram if dirty. Called by flusher goroutine.
func (h *Handler) flushLiveMessage(chatID int64) {
    v, ok := h.liveMessage.Load(chatID)
    if !ok {
        return
    }
    entry := v.(*liveMessageData)
    entry.mu.Lock()
    if !entry.dirty {
        entry.mu.Unlock()
        return
    }
    content := entry.buffer.String()
    entry.mu.Unlock()

    const maxLen = 4000
    if len(content) > maxLen {
        content = truncateLiveBuffer(content, maxLen)
    }

    _, err := h.bot.EditMessageText(context.Background(), &telego.EditMessageTextParams{
        ChatID:    tu.ID(chatID),
        MessageID: entry.messageID,
        Text:      content,
    })
    if err != nil {
        log.Printf("[telegram] live message edit error: %v", err)
        return
    }
    entry.mu.Lock()
    entry.dirty = false
    entry.mu.Unlock()
}

// truncateLiveBuffer keeps header + last 12 lines when approaching 4096 limit.
func truncateLiveBuffer(content string, maxLen int) string {
    lines := strings.Split(content, "\n")
    keepFirst := 3
    keepLast := 12
    if len(lines) <= keepFirst+keepLast+2 {
        return content[:maxLen]
    }
    omitted := len(lines) - keepFirst - keepLast
    return strings.Join(lines[:keepFirst], "\n") +
        fmt.Sprintf("\n... [%d older steps truncated] ...\n", omitted) +
        strings.Join(lines[len(lines)-keepLast:], "\n")
}

// finalizeLiveMessage stops the flusher, waits for it to fully exit, then does one final edit.
func (h *Handler) finalizeLiveMessage(chatID int64, finalText string) {
    v, ok := h.liveMessage.Load(chatID)
    if !ok {
        return
    }
    entry := v.(*liveMessageData)

    // Stop flusher first — wait for goroutine to fully exit (including in-flight HTTP)
    entry.mu.Lock()
    flusher := entry.flusher
    entry.mu.Unlock()

    if flusher != nil {
        flusher.stop()
        <-flusher.doneCh // block until goroutine's defer close(f.doneCh) fires
    }

    // Remove from map BEFORE final edit — flusher goroutine is guaranteed to have exited
    h.liveMessage.Delete(chatID)

    // Append final text
    entry.mu.Lock()
    if finalText != "" {
        entry.buffer.WriteString("\n" + finalText)
    }
    entry.mu.Unlock()

    // Final edit — one-shot, no flusher needed
    _, err := h.bot.EditMessageText(context.Background(), &telego.EditMessageTextParams{
        ChatID:    tu.ID(chatID),
        MessageID: entry.messageID,
        Text:      entry.buffer.String(),
    })
    if err != nil {
        // Fallback: send as new message
        h.sendText(nil, chatID, entry.buffer.String())
    }
}
```

- [ ] **Step 2: Update `runAgentWithTimeout` to pass reporter and handle streaming**

In `runAgentWithTimeout` (around line 423), after building `profileTools` and before the `RunAgent` call:

First, add `streamProgress` to `chatSettings` struct (find `type chatSettings struct` and add the field):

```go
type chatSettings struct {
    thinkLevel    int    // 0=off, 1=brief, 2=normal, 3=verbose
    verbose       bool   // if true, don't strip thinking blocks from responses
    toolProfile   string // active tool profile name, default "standard"
    streamProgress bool  // if true, stream tool progress to live message
}
```

In `getSettings` (around line 215), add `streamProgress: true` to the default:

```go
s := &chatSettings{thinkLevel: 2, verbose: false, toolProfile: "standard", streamProgress: true}
```

Then in `runAgentWithTimeout`, replace the `RunAgent` call section:

Change:
```go
responseText, err := core.RunAgent(ctx, userText, history, db, identityPath,
    &h.agentConfig.Synth, profileTools, sessionID, h.agentConfig.Toolkits)
```

To:
```go
s := h.getSettings(chatID)
var reporter core.ToolProgressReporter
if s.streamProgress {
    reporter = &telegramToolReporter{h: h, chatID: chatID}
}
responseText, err := core.RunAgent(ctx, userText, history, db, identityPath,
    &h.agentConfig.Synth, profileTools, sessionID, h.agentConfig.Toolkits, chatID, reporter)
```

Then update the defer block in `runAgentWithTimeout` (around line 493):

Change:
```go
dbPath := core.ResolveMiniBotDBPath()
go h.selfImprove(dbPath, chatID, userText, responseText)
```

To:
```go
// finalizeLiveMessage stops flusher and does final edit (after defer runs)
defer h.finalizeLiveMessage(chatID, "")

dbPath := core.ResolveMiniBotDBPath()
go h.selfImprove(dbPath, chatID, userText, responseText)
```

Note: `finalizeLiveMessage` will be called when `runAgentWithTimeout` returns — both on normal completion and via the panic defer below. Ensure the defer order puts `finalizeLiveMessage` before `cancel()` in the existing defer chain.

Update the existing panic defer (around line 495):

```go
defer cancel()
defer func() {
    if r := recover(); r != nil {
        // Panic defer handles finalize separately
        h.finalizeLiveMessage(chatID, "❌ Agent crashed or was terminated")
        log.Printf("[telegram] agent panic recovered: %v", r)
    }
}()
```

Note: the `defer h.finalizeLiveMessage(chatID, "")` above handles normal completion. The panic defer handles crash case with a message.

- [ ] **Step 3: Add `/stream` command to `handleCommand` switch**

In `handleCommand` (around line 232), add to the switch:

```go
case "/stream":
    s := h.getSettings(chatID)
    s.streamProgress = !s.streamProgress
    state := "off"
    if s.streamProgress {
        state = "on"
    }
    return true, fmt.Sprintf("Tool streaming: %s", state)
```

- [ ] **Step 4: Update `/help` text to include `/stream`**

In the `/help` case (around line 381), add `/stream` to the help text:

```go
case "/help":
    return true, "Commands:\n/new or /clear — clear session\n/think [off|low|adaptive|med|high] — set thinking level\n/reasoning [on|off] — toggle reasoning mode\n/verbose [on|off] — show/hide thinking blocks\n/stream [on|off] — show/hide tool progress\n/tools [name] — show or switch tool profiles\n/status — show current settings\n/help — show this help"
```

- [ ] **Step 5: Commit**

```bash
cd /home/v/.openclaw/workspace/projects/mpm/mpm-agent
git add cmd/telegram/handler.go
git commit -m "feat(telegram): stream tool progress to live message with risk alerts"
```

---

## Task 3: Verify build and run tests

**Files:**
- Build: entire `mpm-agent/` project

- [ ] **Step 1: Build to check for compile errors**

```bash
cd /home/v/.openclaw/workspace/projects/mpm/mpm-agent
CGO_CFLAGS="-DSQLITE_ENABLE_FTS5" CGO_LDFLAGS="-lm" go build ./...
```

Expected: No errors. If there are import issues (e.g., `fmt` not used in `agent.go`), add the missing import.

- [ ] **Step 2: Run existing tests**

```bash
CGO_CFLAGS="-DSQLITE_ENABLE_FTS5" CGO_LDFLAGS="-lm" go test ./core/... -v -count=1
```

Expected: All tests pass.

- [ ] **Step 3: Commit**

```bash
git add -A
git commit -m "fix: build verification for tool streaming feature"
```

---

## Self-Review Checklist

- [ ] `RunAgent` now takes `chatID int64` and `reporter ToolProgressReporter` — search for all call sites (should be `handler.go` only)
- [ ] `agent.go` compiles: `go build` passes with no unused import errors
- [ ] `handler.go` compiles: `go build` passes
- [ ] All existing tests still pass
- [ ] Flusher goroutine is stored on `liveMessageData.flusher` — not orphaned
- [ ] `finalizeLiveMessage` calls `flusher.stop()` then `<-flusher.doneCh` before editing
- [ ] `finalizeLiveMessage` does map `Delete` after waiting for doneCh
- [ ] `truncateLiveBuffer` handles the 4096 limit
- [ ] `/stream` toggle works and defaults to `true`