# Live Tool Progress Streaming — Telegram Agent

**Date:** 2026-04-15
**Status:** Approved

## Problem

When using `mpm-agent-telegram` with the `coding` tool profile, the agent runs silently — no visibility into which tools are executing or what they're touching. This is risky: destructive commands or file writes could be happening without the user's knowledge.

## Solution

Stream tool progress to the user via Telegram with two mechanisms:

1. **Live edit message** — single message, updated in place as each tool completes
2. **Risk alerts** — separate warning ping for high-risk operations

This gives visibility without flooding the chat.

---

## Architecture

### Components

**`ToolProgressReporter`** — interface implemented by the Telegram handler, passed into the agent loop:

```go
type ToolProgressReporter interface {
    // ToolStarted is called before executing a tool
    ToolStarted(toolName string, input map[string]interface{}) (reportID string)
    // ToolCompleted updates the live message with the result
    ToolCompleted(reportID string, toolName string, summary string)
    // SendAlert posts a risk warning to the chat
    SendAlert(chatID int64, message string)
}
```

**Edit-in-place message** — tracks `messageID` per chat, updated on each tool completion.

**Risk detection** — `IsRiskyTool(toolName string, input map[string]interface{}) bool` function, evaluates against a configurable pattern list.

**Update data flow:**

```
agentReply(chatID)
  → start typing indicator
  → runAgentWithTimeout(chatID, ...)
      → reporter = &telegramToolReporter{h, chatID}
      → RunAgent(..., reporter)
          → ToolStarted(toolName) → appends "🔧 tool_name..." to buffer
          → execute tool
          → ToolCompleted(toolName, summary) → appends " tool_name: summary"
          → if risky: SendAlert() → separate warning message
          → if error: SendAlert() → warning with error
      → defer: finalizeLiveMessage(chatID, "") — stops flusher
  → sendLongText(response) — response below the live message
```

Note: `finalizeLiveMessage` stops the flusher and does one final edit. The agent's response is sent separately as `sendLongText` — it appears below the live message, which shows the tool execution log.

---

## Implementation

### 1. `core/agent.go` — Add reporter parameter to `RunAgent`

Add a `ToolProgressReporter` parameter to `RunAgent`. If nil, no streaming (backward-compatible with MCP entry point).

```go
func RunAgent(ctx context.Context, query string, history []map[string]interface{}, db *sql.DB,
    identityPath string, cfg *SynthConfig, toolProfile []string, sessionID string,
    toolkitMap map[string][]string, reporter ToolProgressReporter) (string, error)
```

Wrap tool execution in:

```go
if reporter != nil {
    reportID := reporter.ToolStarted(tc.Name, tc.Input)
    // ... execute tool ...
    reporter.ToolCompleted(reportID, tc.Name, summarize(toolName, result, err))
    if isRiskyOperation(tc.Name, tc.Input, result, err) {
        reporter.SendAlert(chatID, riskWarning(tc.Name, tc.Input))
    }
}
```

`summarize()` produces one-line summaries:
- `read_file`: `"✓ /path/file.go — N lines"`
- `write_file`: `"✓ Wrote /path/file.go — N bytes"`
- `execute_shell`: `"✓ exit 0 — N lines output"` or `"✗ exit 1 — error"`
- `git`: `"✓ git branch — 3 branches"`
- Error: `"✗ tool_name: <brief error>`

### 2. `cmd/telegram/handler.go` — Implement `ToolProgressReporter`

```go
type Handler struct {
    // ... existing fields ...
    liveMessage sync.Map // chatID → liveMessageData
}

type liveMessageData struct {
	messageID int64
	buffer    strings.Builder
	mu        sync.Mutex
	dirty     bool           // true if buffer changed since last flush
	lastSent  string         // last successfully sent content
	flusher   *flusher       // stored here so finalizeLiveMessage can stop it
}

// flusher runs in background, flushes edits to Telegram at ~1/sec rate
// Does NOT block the agent loop — only throttles Telegram API calls.
type flusher struct {
	handler *Handler
	chatID  int64
	ticker  *time.Ticker
	stopCh  chan struct{}
	doneCh  chan struct{} // closed when goroutine fully exits (including in-flight HTTP)
}

func newFlusher(h *Handler, chatID int64) *flusher {
	f := &flusher{handler: h, chatID: chatID, ticker: time.NewTicker(1100 * time.Millisecond), stopCh: make(chan struct{}), doneCh: make(chan struct{})}
	go f.run()
	return f
}

func (f *flusher) run() {
	defer close(f.doneCh) // goroutine fully exited only after defer runs (HTTP call included)
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
```

Implement the interface:

```go
func (h *Handler) ToolStarted(toolName string, input map[string]interface{}) string {
    entry := h.getOrCreateLiveMessage(chatID)
    entry.mu.Lock()
    entry.buffer.WriteString(fmt.Sprintf("\n🔧 %s...", toolName))
    entry.dirty = true
    entry.mu.Unlock()
    return toolName // reportID is the tool name
}

func (h *Handler) ToolCompleted(reportID string, toolName string, summary string) {
    entry := h.getLiveMessage(chatID)
    if entry == nil {
        return
    }
    entry.mu.Lock()
    entry.buffer.WriteString(fmt.Sprintf(" %s\n", summary))
    entry.dirty = true
    entry.mu.Unlock()
    // DO NOT call flush here — flusher goroutine handles Telegram API calls
}

// getLiveMessage returns existing entry without creating one (for ToolCompleted after ToolStarted)
func (h *Handler) getLiveMessage(chatID int64) *liveMessageData {
    v, ok := h.liveMessage.Load(chatID)
    if !ok {
        return nil
    }
    return v.(*liveMessageData)
}

func (h *Handler) SendAlert(chatID int64, message string) {
    // Standalone message — bypasses flusher, sent immediately
    h.sendText(nil, chatID, message)
}

// getOrCreateLiveMessage returns the liveMessageData for a chat, creating one if needed.
// Must be called from the agent goroutine (not the flusher goroutine).
func (h *Handler) getOrCreateLiveMessage(chatID int64) *liveMessageData {
    v, ok := h.liveMessage.Load(chatID)
    if ok {
        return v.(*liveMessageData)
    }
    // Send initial message and store its message ID
    sent, err := h.bot.SendMessage(context.Background(), tu.Message(tu.ID(chatID), "🧠 Working..."))
    if err != nil {
        log.Printf("[telegram] live message init error: %v", err)
        return nil
    }
    data := &liveMessageData{messageID: sent.MessageID}
    data.flusher = newFlusher(h, chatID) // flusher stored on data, not orphaned
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

    // Truncate if approaching 4096
    const maxLen = 4000
    if len(content) > maxLen {
        // Keep header + last 10 entries
        content = truncateBuffer(content, maxLen)
    }

    _, err := h.bot.EditMessageText(context.Background(), &telego.EditMessageTextParams{
        ChatID:    tu.ID(chatID),
        MessageID: entry.messageID,
        Text:      content,
    })
    if err != nil {
        // Non-fatal — log and continue
        log.Printf("[telegram] live message edit error: %v", err)
        return
    }
    entry.mu.Lock()
    entry.lastSent = content
    entry.dirty = false
    entry.mu.Unlock()
}
```

**Truncation strategy for 4096 limit:**

```go
func truncateBuffer(content string, maxLen int) string {
    lines := strings.Split(content, "\n")
    // Keep first 3 lines (header), last 12 lines
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
```

**Panic/crash safety:**

In `runAgentWithTimeout`, add:

```go
func (h *Handler) runAgentWithTimeout(chatID int64, userText string, cancel context.CancelFunc) {
    defer cancel()
    defer func() {
        if r := recover(); r != nil {
            h.finalizeLiveMessage(chatID, "❌ Agent crashed or was terminated")
            log.Printf("[telegram] agent panic recovered: %v", r)
        } else {
            // Normal completion: stop flusher, replace live message with final response
            h.finalizeLiveMessage(chatID, "")
        }
    }()
    // ... existing code ...
}
```

`finalizeLiveMessage` stops the flusher and sets a final state:

```go
func (h *Handler) finalizeLiveMessage(chatID int64, finalText string) {
    // Load first (not LoadAndDelete) — we need the flusher before removing from map
    v, ok := h.liveMessage.Load(chatID)
    if !ok {
        return
    }
    entry := v.(*liveMessageData)
    // Stop flusher BEFORE removing from map — flusher goroutine reads from map
    entry.mu.Lock()
    flusher := entry.flusher
    entry.mu.Unlock()
    if flusher != nil {
        flusher.stop()          // closes stopCh — flusher's select loop exits
        <-flusher.doneCh        // waits for run() to fully exit (defer + any in-flight HTTP done)
    }
    // Now it is 100% safe to do the final edit — no in-flight flush can race
    // Now safe to remove from map — flusher goroutine will exit
    h.liveMessage.Delete(chatID)
    // Append final text and send one last edit
    entry.mu.Lock()
    entry.buffer.WriteString("\n" + finalText)
    entry.mu.Unlock()
    // Final edit (bypasses flusher, no dirty tracking needed)
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

Key invariant: the flusher goroutine is stopped BEFORE the entry is removed from the `sync.Map`. This guarantees the goroutine will exit cleanly — it won't try to access the map after deletion because it has already exited.

### 3. Call sites — wire reporter into `RunAgent`

In `runAgentWithTimeout`, create reporter from handler and pass to `RunAgent`:

```go
reporter := &telegramToolReporter{h: h, chatID: chatID}
responseText, err := core.RunAgent(..., reporter)
```

MCP entry point (`cmd/mcp/main.go`) passes `nil` reporter — no streaming needed there.

### 4. Risk Detection

Pattern-based risk detection in `core/agent.go`:

```go
// Risky tool + dangerous pattern = alert
var riskyPatterns = []*regexp.Regexp{
    regexp.MustCompile(`(?i)\brm\s+-(rf|r)\b`),
    regexp.MustCompile(`(?i)git\s+push\s+.*--force`),
    regexp.MustCompile(`(?i)dd\s+.*of=`),
    regexp.MustCompile(`(?i)(mkfs|shred|wipe)\s`),
    regexp.MustCompile(`(?i)(chmod|chown)\s+777`),
}

// Risky paths
var riskyPaths = []*regexp.Regexp{
    regexp.MustCompile(`^\.?(ssh|aws|env|secrets|credentials)`),
    regexp.MustCompile(`(?i)/etc/|/sys/|^/root/`),
}

func isRiskyOperation(toolName string, input map[string]interface{}, result string, err error) bool {
    // Check: dangerous commands in shell execution
    // Check: writes to risky paths
    // Check: errors in sensitive operations
    return false
}
```

Alerts are sent as standalone Telegram messages (not edits) so they're noticed:

```
⚠️ DANGEROUS: execute_shell running `rm -rf ./node_modules/.cache`
```

### 5. Chat Settings — Add `streamProgress` toggle

Add `streamProgress` to `chatSettings` struct (default `true`). Allow toggle via `/stream` command:

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

Update `/help` to include `/stream`.

---

## What Gets Streamed

Only tool calls that go through the agent loop. Framework tools (`load_toolkit`, `unload_toolkit`, `list_toolkits`) are intercepted and not sent to the API — no streaming needed for them.

---

## Edge Cases

- **No tool calls**: live message shows "🧠 Working..." only, finalized on completion (flusher stops cleanly)
- **Many rapid tools**: edits throttled to ~1/second via background goroutine (agent loop never blocks on Telegram API)
- **Telegram edit fails**: log error, flusher continues — next tick retries
- **Risky tool where result is safe**: still alert — presence of command matters more than outcome
- **MCP callers**: `nil` reporter → full backward compat, no streaming
- **Flusher race with finalize**: flusher goroutine reads from map only while entry is present; `stop()` + map `Delete()` in `finalizeLiveMessage` guarantees goroutine exits before removal

---

## Testing

- `runAgentWithTimeout` with coding profile → live message appears and updates
- Risky shell command detected → alert fires
- `/stream off` → no live message
- `/verbose on` + `/think on` together → both visible alongside tool progress
- MCP entry point still works (reporter = nil)