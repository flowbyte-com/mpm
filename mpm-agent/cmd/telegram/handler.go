package main

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/mymmrac/telego"
	th "github.com/mymmrac/telego/telegohandler"
	tu "github.com/mymmrac/telego/telegoutil"

	"mpm-agent/core"
)

// telegramPersistence wraps SessionManager to implement core.Persistence.
type telegramPersistence struct {
	sm     *SessionManager
	chatID int64
}

func (p *telegramPersistence) Get() ([]map[string]interface{}, error) {
	return p.sm.Get(p.chatID)
}

func (p *telegramPersistence) Save(messages []map[string]interface{}) error {
	return p.sm.Save(p.chatID, messages)
}

func (p *telegramPersistence) SessionID() string {
	return fmt.Sprintf("telegram:%d", p.chatID)
}

// handlerIdentityResolver implements core.IdentityResolver using the Handler's config.
type handlerIdentityResolver struct {
	h *Handler
}

func (r *handlerIdentityResolver) ResolveIdentityPath(binaryDir, configured string) string {
	return core.ResolveIdentityPath(binaryDir, r.h.agentConfig.Paths.Identity)
}

func (r *handlerIdentityResolver) LoadIdentity(path string) (*core.Identity, error) {
	return core.LoadIdentity(path)
}

// chatSettings holds per-chat preferences.
type chatSettings struct {
	thinkLevel     int    // 0=off, 1=brief, 2=normal, 3=verbose
	verbose        bool   // if true, don't strip thinking blocks from responses
	toolProfile    string // active tool profile name, default "standard"
	streamProgress bool   // if true, stream tool progress to live message
}

// Handler routes incoming Telegram updates to the agent.
type Handler struct {
	bot           *telego.Bot
	cfg           *TelegramConfig
	sm            *SessionManager
	agentConfig   *core.MiniBotConfig
	mediaDir      string   // absolute path to media cache directory
	activeReplies sync.Map // chatID → true (prevents double-reply)
	chatSettings  sync.Map // chatID → *chatSettings
	liveMessage   sync.Map // chatID (int64) → *liveMessageData
	tokenTotals   sync.Map // chatID (int64) → *sessionTokenTotals
	summaryTimers sync.Map // chatID (int64) → chan struct{} (cancel pending summary)
}

// liveMessageData holds the state for a live-streaming message in one chat.
type liveMessageData struct {
	messageID int
	buffer    strings.Builder
	mu        sync.Mutex
	dirty     bool     // buffer changed since last flush
	flusher   *flusher // stop via flusher field to prevent goroutine leak

	// State machine for animated status
	status        string        // "init", "thinking", "tooling", "stuck", "done"
	step          int           // current step (for "[2/3]" display)
	totalSteps    int           // total steps known
	lastActivity  time.Time      // for stuck detection
	stuckTimeout  time.Duration // default 4s
	stuckArmed    bool          // true after first activity (arms stuck detection)
}

// statusState constants
const (
	statusInit     = "init"
	statusThinking = "thinking"
	statusTooling  = "tooling"
	statusStuck    = "stuck"
	statusDone     = "done"
)

func (e *liveMessageData) SetStatus(s string) {
	e.mu.Lock()
	defer e.mu.Unlock()
	if s == statusStuck {
		return
	}
	e.status = s
}

func (e *liveMessageData) SetProgress(step, total int) {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.step = step
	e.totalSteps = total
}

func (e *liveMessageData) Touch() {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.lastActivity = time.Now()
	e.stuckArmed = true
	if e.status == statusStuck {
		e.status = statusThinking
	}
}

func (e *liveMessageData) IsStuck() bool {
	e.mu.Lock()
	defer e.mu.Unlock()
	if !e.stuckArmed || e.stuckTimeout == 0 {
		return false
	}
	return time.Since(e.lastActivity) > e.stuckTimeout
}

func (e *liveMessageData) Render(tick int) string {
	e.mu.Lock()
	status := e.status
	step := e.step
	total := e.totalSteps
	e.mu.Unlock()

	switch status {
	case statusInit:
		return "🧠 Initializing..."
	case statusThinking:
		if tick%2 == 0 {
			return "🤖 💭"
		}
		return "🤖 💡"
	case statusTooling:
		prefix := ""
		if total > 0 {
			prefix = fmt.Sprintf("[%d/%d] ", step, total)
		}
		switch tick % 3 {
		case 0:
			return fmt.Sprintf("%s🔧 Working... ⬛️⬛️⬛️", prefix)
		case 1:
			return fmt.Sprintf("%s🔧 Working... 🟩⬛️⬛️", prefix)
		default:
			return fmt.Sprintf("%s🔧 Working... 🟩🟩⬛️", prefix)
		}
	case statusStuck:
		return "⏳ Taking longer than usual..."
	case statusDone:
		return "✍️ Formatting answer..."
	default:
		return "🧠 Working..."
	}
}

func (e *liveMessageData) Init() {
	e.mu.Lock()
	e.lastActivity = time.Now()
	e.stuckArmed = false
	e.status = statusInit
	e.stuckTimeout = 4 * time.Second
	e.step = 0
	e.totalSteps = 0
	e.mu.Unlock()
}

// sessionTokenTotals holds running token usage for one chat session.
type sessionTokenTotals struct {
	mu           sync.Mutex
	inputTokens  int
	outputTokens int
	calls        int
}

// flusher runs in background, flushes buffer edits to Telegram at ~1.5s interval.
type flusher struct {
	handler *Handler
	chatID  int64
	ticker  *time.Ticker
	stopCh  chan struct{}
	doneCh  chan struct{} // closed when goroutine fully exits
	tick    int
}

func newFlusher(h *Handler, chatID int64) *flusher {
	f := &flusher{
		handler: h,
		chatID:  chatID,
		ticker:  time.NewTicker(1500 * time.Millisecond),
		stopCh:  make(chan struct{}),
		doneCh:  make(chan struct{}),
	}
	go f.run()
	return f
}

func (f *flusher) run() {
	defer close(f.doneCh)
	for {
		select {
		case <-f.ticker.C:
			f.tick++
			f.handler.flushLiveMessage(f.chatID, f.tick)
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

func (r *telegramToolReporter) ToolStarted(chatID int64, toolName string, input map[string]interface{}) string {
	entry := r.h.getOrCreateLiveMessage(r.chatID)
	if entry == nil {
		return ""
	}
	entry.SetStatus(statusTooling)
	if stepNum, ok := input["_stepNum"].(int); ok {
		if stepTotal, ok := input["_stepTotal"].(int); ok {
			entry.SetProgress(stepNum, stepTotal)
		}
	}
	entry.Touch()
	entry.mu.Lock()
	if entry.buffer.Len() == 0 {
		entry.buffer.WriteString("🧠 Working...\n")
	}
	entry.buffer.WriteString(fmt.Sprintf("🔧 %s...\n", toolName))
	entry.dirty = true
	entry.mu.Unlock()
	return toolName
}

func (r *telegramToolReporter) ToolCompleted(chatID int64, reportID string, toolName string, summary string) {
	entry := r.h.getLiveMessage(r.chatID)
	if entry == nil {
		return
	}
	entry.Touch()
	entry.mu.Lock()
	entry.buffer.WriteString(fmt.Sprintf("  %s\n", summary))
	entry.dirty = true
	entry.mu.Unlock()
}

func (r *telegramToolReporter) SendAlert(chatID int64, message string) {
	r.h.sendText(nil, r.chatID, message)
}

// tokenUsageReporter implements core.TokenUsageReporter — tracks per-session token totals.
type tokenUsageReporter struct {
	h      *Handler
	chatID int64
}

func (r *tokenUsageReporter) ReportUsage(chatID int64, inputTokens, outputTokens int, model string) {
	totals := r.h.getOrCreateTokenTotals(r.chatID)
	totals.mu.Lock()
	totals.inputTokens += inputTokens
	totals.outputTokens += outputTokens
	totals.calls++
	totals.mu.Unlock()
}

// getOrCreateTokenTotals returns or creates the token totals for a chat session.
func (h *Handler) getOrCreateTokenTotals(chatID int64) *sessionTokenTotals {
	v, ok := h.tokenTotals.Load(chatID)
	if ok {
		return v.(*sessionTokenTotals)
	}
	data := &sessionTokenTotals{}
	h.tokenTotals.Store(chatID, data)
	return data
}

// getLiveMessage returns existing liveMessageData for chatID without creating one.
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
	sent, err := h.bot.SendMessage(context.Background(), tu.Message(tu.ID(chatID), "🤖 💭"))
	if err != nil {
		log.Printf("[telegram] live message init error: %v", err)
		return nil
	}
	data := &liveMessageData{messageID: sent.MessageID}
	data.Init()
	data.status = statusThinking
	data.flusher = newFlusher(h, chatID)
	h.liveMessage.Store(chatID, data)
	return data
}

// flushLiveMessage sends buffer to Telegram if dirty. Called by flusher goroutine.
func (h *Handler) flushLiveMessage(chatID int64, tick int) {
	v, ok := h.liveMessage.Load(chatID)
	if !ok {
		return
	}
	entry := v.(*liveMessageData)

	entry.mu.Lock()
	isStuck := entry.IsStuck()
	if isStuck && entry.status != statusStuck {
		entry.status = statusStuck
	}
	content := entry.buffer.String()
	entry.mu.Unlock()

	statusLine := entry.Render(tick)

	var fullText string
	if content != "" {
		fullText = statusLine + "\n" + content
	} else {
		fullText = statusLine
	}

	const maxLen = 4000
	if len(fullText) > maxLen {
		fullText = truncateLiveBuffer(fullText, maxLen)
	}

	_, err := h.bot.EditMessageText(context.Background(), &telego.EditMessageTextParams{
		ChatID:    tu.ID(chatID),
		MessageID: entry.messageID,
		Text:      fullText,
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
// Returns the text that was sent.
func (h *Handler) finalizeLiveMessage(chatID int64, finalText string) string {
	v, ok := h.liveMessage.Load(chatID)
	if !ok {
		return ""
	}
	entry := v.(*liveMessageData)

	entry.mu.Lock()
	flusher := entry.flusher
	entry.mu.Unlock()

	if flusher != nil {
		flusher.stop()
		<-flusher.doneCh
	}

	h.liveMessage.Delete(chatID)

	entry.mu.Lock()
	if finalText != "" {
		entry.buffer.WriteString("\n" + finalText)
	}
	text := entry.buffer.String()
	entry.mu.Unlock()

	_, err := h.bot.EditMessageText(context.Background(), &telego.EditMessageTextParams{
		ChatID:    tu.ID(chatID),
		MessageID: entry.messageID,
		Text:      text,
	})
	if err != nil {
		h.sendText(nil, chatID, text)
	}
	return text
}

// NewHandler creates a new Telegram handler.
func NewHandler(bot *telego.Bot, cfg *TelegramConfig, sm *SessionManager, agentCfg *core.MiniBotConfig) *Handler {
	h := &Handler{
		bot:         bot,
		cfg:         cfg,
		sm:          sm,
		agentConfig: agentCfg,
	}
	// Set up media cache directory
	h.mediaDir = filepath.Join(core.GetBinaryDir(), "media")
	if err := os.MkdirAll(h.mediaDir, 0700); err != nil {
		log.Printf("[telegram] warning: could not create media dir: %v", err)
	} else {
		log.Printf("[telegram] media dir: %s", h.mediaDir)
	}
	return h
}

// Handle processes an incoming Telegram message or photo.
func (h *Handler) Handle(ctx *th.Context, message telego.Message) error {
	// Handle photos: download and save to media/, inject system message
	if len(message.Photo) > 0 {
		return h.handlePhotoMessage(ctx, message)
	}

	// We only handle text messages
	if message.Text == "" {
		return nil
	}

	chatID := message.Chat.ID
	text := strings.TrimSpace(message.Text)
	if text == "" {
		return nil
	}

	// Check authorization — use FROM user ID
	if !h.isAllowed(message.From.ID) {
		return nil
	}

	// Intercept commands
	if strings.HasPrefix(text, "/") {
		handled, response := h.handleCommand(chatID, text)
		if handled {
			h.sendLongText(ctx, chatID, response)
		}
		return nil
	}

	// Serialize reply to prevent double-replies to the same chat
	if _, loaded := h.activeReplies.LoadOrStore(chatID, true); loaded {
		return nil // Already processing a reply
	}
	defer h.activeReplies.Delete(chatID)

	// Non-blocking agent reply (Tier 1): send "🧠 Thinking..." immediately
	h.agentReply(chatID, text)
	return nil
}

// isAllowed checks if a Telegram user is authorized.
func (h *Handler) isAllowed(userID int64) bool {
	if len(h.cfg.AllowedUsers) == 0 {
		return true // Allow all if no allowlist configured
	}
	for _, id := range h.cfg.AllowedUsers {
		if id == userID {
			return true
		}
	}
	return false
}

// handlePhotoMessage downloads a photo and injects a system message into the session.
func (h *Handler) handlePhotoMessage(ctx *th.Context, message telego.Message) error {
	// Check authorization
	if !h.isAllowed(message.From.ID) {
		return nil
	}

	// Get the largest photo size
	sizes := message.Photo
	largest := sizes[len(sizes)-1]

	// Get file info
	file, err := h.bot.GetFile(context.Background(), &telego.GetFileParams{FileID: largest.FileID})
	if err != nil {
		log.Printf("[telegram] GetFile error: %v", err)
		return nil
	}

	// Construct download URL
	downloadURL := fmt.Sprintf("https://api.telegram.org/file/bot%s/%s", h.bot.Token(), file.FilePath)

	// Download the file
	httpResp, err := http.DefaultClient.Get(downloadURL)
	if err != nil {
		log.Printf("[telegram] DownloadFile error: %v", err)
		return nil
	}
	defer httpResp.Body.Close()

	if httpResp.StatusCode != http.StatusOK {
		log.Printf("[telegram] download HTTP %d", httpResp.StatusCode)
		return nil
	}

	content, err := io.ReadAll(httpResp.Body)
	if err != nil {
		log.Printf("[telegram] read download body: %v", err)
		return nil
	}

	// Save to media dir
	hash := sha256.Sum256(content)
	ext := "jpg" // Telegram photos are JPEG
	filename := fmt.Sprintf("inbound_%s.%s", hex.EncodeToString(hash[:16]), ext)
	outPath := filepath.Join(h.mediaDir, filename)
	if err := os.WriteFile(outPath, content, 0600); err != nil {
		log.Printf("[telegram] WriteFile error: %v", err)
		return nil
	}

	log.Printf("[telegram] saved inbound photo to %s", outPath)

	// Inject system message into session
	chatID := message.Chat.ID
	sysMsg := fmt.Sprintf("[System: User uploaded an image saved at %s]", outPath)
	if err := h.injectSystemMessage(chatID, sysMsg); err != nil {
		log.Printf("[telegram] injectSystemMessage error: %v", err)
	}

	// If the photo has a caption, also process the caption as a text message
	if message.Caption != "" {
		text := strings.TrimSpace(message.Caption)
		if text != "" && !strings.HasPrefix(text, "/") {
			// Forward to agent for image understanding
			go func() {
				history, err := h.sm.Get(chatID)
				if err != nil || history == nil {
					return
				}
				// Agent will see the system message + caption, can call understand_image
				// The caption processing happens through the normal agent flow
			}()
		}
	}

	return nil
}

// injectSystemMessage prepends a system message to the chat session history.
func (h *Handler) injectSystemMessage(chatID int64, content string) error {
	history, err := h.sm.Get(chatID)
	if err != nil {
		return fmt.Errorf("get history: %w", err)
	}
	injected := map[string]interface{}{"role": "system", "content": content}
	if history == nil {
		history = []map[string]interface{}{injected}
	} else {
		history = append([]map[string]interface{}{injected}, history...)
	}
	if err := h.sm.Save(chatID, history); err != nil {
		return fmt.Errorf("save history: %w", err)
	}
	return nil
}

// getSettings returns per-chat settings, creating a default if needed.
func (h *Handler) getSettings(chatID int64) *chatSettings {
	v, ok := h.chatSettings.Load(chatID)
	if ok {
		return v.(*chatSettings)
	}
	s := &chatSettings{thinkLevel: 2, verbose: false, toolProfile: "standard", streamProgress: true}
	h.chatSettings.Store(chatID, s)
	return s
}

// handleCommand parses and executes a /command.
// Returns (handled, responseText). If handled=true, responseText may be a reply.
func (h *Handler) handleCommand(chatID int64, cmd string) (bool, string) {
	// Strip bot name if present: /new@mpm_808_bot -> /new
	if idx := strings.Index(cmd, "@"); idx != -1 {
		cmd = cmd[:idx]
	}

	parts := strings.Fields(cmd)
	if len(parts) == 0 {
		return false, ""
	}
	switch parts[0] {
	case "/new", "/clear":
		// Cancel pending session summary
		h.cancelSummaryTimer(chatID)
		// Clear session history for this chat
		if err := h.sm.Save(chatID, nil); err != nil {
			return true, fmt.Sprintf("Failed to clear session: %v", err)
		}
		// Clear loaded toolkits for this session
		core.ClearSessionToolkits(fmt.Sprintf("telegram:%d", chatID))
		// Clear token totals
		h.tokenTotals.Delete(chatID)
		return true, "Session cleared. Starting fresh."

	case "/reasoning":
		s := h.getSettings(chatID)
		if len(parts) == 1 {
			state := "off"
			if s.thinkLevel > 0 {
				state = "on"
			}
			return true, fmt.Sprintf("Reasoning mode: %s (think level %d)", state, s.thinkLevel)
		}
		switch parts[1] {
		case "on":
			if s.thinkLevel == 0 {
				s.thinkLevel = 2 // Default to normal when enabling
			}
			return true, fmt.Sprintf("Reasoning mode on (think level %d).", s.thinkLevel)
		case "off":
			s.thinkLevel = 0
			return true, "Reasoning mode off."
		default:
			return true, "Usage: /reasoning [on|off]"
		}

	case "/think":
		s := h.getSettings(chatID)
		if len(parts) == 1 {
			labels := []string{"off", "low", "adaptive", "med", "high"}
			level := s.thinkLevel
			if level > 4 {
				level = 4
			}
			return true, fmt.Sprintf("Think level: %s", labels[level])
		}
		level := -1
		switch strings.ToLower(parts[1]) {
		case "off":
			level = 0
		case "low":
			level = 1
		case "adaptive":
			level = 2
		case "med", "medium":
			level = 3
		case "high":
			level = 4
		default:
			if n, err := strconv.Atoi(parts[1]); err == nil {
				level = n
			}
		}
		if level < 0 || level > 4 {
			return true, "Usage: /think [off|low|adaptive|med|high]"
		}
		s.thinkLevel = level
		labels := []string{"off", "low", "adaptive", "med", "high"}
		return true, fmt.Sprintf("Think level set to %s.", labels[level])

	case "/verbose":
		s := h.getSettings(chatID)
		if len(parts) == 1 {
			state := "off"
			if s.verbose {
				state = "on"
			}
			return true, fmt.Sprintf("Verbose mode: %s", state)
		}
		switch parts[1] {
		case "on":
			s.verbose = true
			return true, "Verbose mode on — thinking blocks will be shown."
		case "off":
			s.verbose = false
			return true, "Verbose mode off — thinking blocks will be hidden."
		default:
			return true, "Usage: /verbose [on|off]"
		}

	case "/status":
		s := h.getSettings(chatID)
		labels := []string{"off", "low", "adaptive", "med", "high"}
		verb := "off"
		if s.verbose {
			verb = "on"
		}
		level := s.thinkLevel
		if level > 4 {
			level = 4
		}
		stream := "off"
		if s.streamProgress {
			stream = "on"
		}
		return true, fmt.Sprintf("Think: %s | Verbose: %s | Stream: %s", labels[level], verb, stream)

	case "/stream":
		s := h.getSettings(chatID)
		s.streamProgress = !s.streamProgress
		state := "off"
		if s.streamProgress {
			state = "on"
		}
		return true, fmt.Sprintf("Tool streaming: %s", state)

	case "/tokens":
		totals := h.getOrCreateTokenTotals(chatID)
		totals.mu.Lock()
		in, out, calls := totals.inputTokens, totals.outputTokens, totals.calls
		totals.mu.Unlock()
		total := in + out
		return true, fmt.Sprintf("📊 Session tokens: in=%d | out=%d | total=%d | calls=%d", in, out, total, calls)

	case "/summarize":
		history, _ := h.sm.Get(chatID)
		if history == nil || len(history) < 10 {
			return true, "Need at least 10 messages to summarize this session."
		}
		go h.summarizeSession(chatID, history)
		return true, "🧠 Summarizing this session... will be ready in a moment."

	case "/tools":
		profiles := make([]string, 0, len(h.agentConfig.Profiles))
		for name := range h.agentConfig.Profiles {
			profiles = append(profiles, name)
		}
		sort.Strings(profiles)

		if len(parts) == 1 {
			current := h.getSettings(chatID).toolProfile
			if current == "" {
				current = "standard"
			}
			var sb strings.Builder
			sb.WriteString(fmt.Sprintf("Current: %s\n\nAvailable profiles:\n", current))
			for _, name := range profiles {
				marker := ""
				if name == current {
					marker = " ✓"
				}
				sb.WriteString(fmt.Sprintf("  /tools %s%s\n", name, marker))
			}
			sb.WriteString("\nUse /tools <name> to switch.")
			return true, sb.String()
		}

		newProfile := parts[1]
		if _, ok := h.agentConfig.Profiles[newProfile]; !ok {
			return true, fmt.Sprintf("Unknown profile: %s", newProfile)
		}
		cs := h.getSettings(chatID)
		cs.toolProfile = newProfile
		return true, fmt.Sprintf("Tool profile: %s", newProfile)

	case "/":
		return true, "Commands:\n/new or /clear — clear session\n/think [off|low|adaptive|med|high] — set thinking level\n/reasoning [on|off] — toggle reasoning mode\n/verbose [on|off] — show/hide thinking blocks\n/tools [name] — show or switch tool profiles\n/stream [on|off] — show/hide tool progress\n/tokens — show session token usage\n/status — show current settings"

	default:
		return false, ""
	}
}

// agentReply runs the MPM agent asynchronously (non-blocking).
// Sends a single fire-and-forget typing indicator, then runs the agent.
func (h *Handler) agentReply(chatID int64, userText string) {
	// Fire-and-forget typing action
	_ = h.bot.SendChatAction(context.Background(), &telego.SendChatActionParams{
		ChatID: tu.ID(chatID),
		Action: telego.ChatActionTyping,
	})

	// Run agent in goroutine
	go h.runAgentWithTimeout(chatID, userText)
}

// runAgentWithTimeout runs the agent via SessionRunner with 90s timeout.
func (h *Handler) runAgentWithTimeout(chatID int64, userText string) (finalMsg string) {
	// Create 90s timeout context — hard kill switch
	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()

	defer func() {
		if r := recover(); r != nil {
			finalMsg = "❌ Agent crashed or was terminated"
			log.Printf("[telegram] agent panic recovered: %v", r)
		}
	}()

	// Open database
	db, err := core.OpenDBForPath(core.ResolveMiniBotDBPath())
	if err != nil {
		h.sendText(nil, chatID, fmt.Sprintf("⚠️ Error: %v", err))
		return
	}
	defer db.Close()

	// Get active tool profile
	profile := h.getSettings(chatID).toolProfile
	if profile == "" {
		profile = "standard"
	}

	// Create transport + persistence + identity
	transport := newLiveTransport(h.bot, chatID, h)
	session := &telegramPersistence{sm: h.sm, chatID: chatID}
	identityResolver := &handlerIdentityResolver{h: h}

	// Create session runner
	runner := core.NewSessionRunner(
		transport,
		fmt.Sprintf("telegram:%d", chatID),
		profile,
		db,
		session,
		identityResolver,
	)

	// Run the agent (streams output via transport)
	runner.HandleInput(ctx, userText)

	// Finalize the live message — returns the response text
	responseText := h.finalizeLiveMessage(chatID, "")

	// Silent bot fallback: if no live message was created (streaming failed),
	// send the response text directly via bot.SendMessage()
	if responseText == "" && finalMsg != "" {
		h.sendText(nil, chatID, finalMsg)
	}

	// Schedule session summary
	go h.scheduleSessionSummary(chatID, nil)

	return finalMsg
}

// editMessage edits a Telegram message with final text (removes inline keyboard).
func (h *Handler) editMessage(chatID int64, msgID int, text string) {
	h.bot.EditMessageText(context.Background(), &telego.EditMessageTextParams{
		ChatID:    tu.ID(chatID),
		MessageID: msgID,
		Text:      text,
	})
}

// handleApproval was removed — HITL approval system was removed in earlier refactor

// isOverloadedError returns true if the error is an API overloaded error (529).
func isOverloadedError(err error) bool {
	if err == nil {
		return false
	}
	errStr := err.Error()
	return strings.Contains(errStr, "529") && strings.Contains(errStr, "overloaded")
}

// selfImprove extracts lessons and creates anchors from the conversation.
// Runs after each response in a goroutine to avoid blocking the reply.
// Opens its own db connection since the caller's connection is closed on return.
func (h *Handler) selfImprove(dbPath string, chatID int64, userText, responseText string) {
	db, err := core.OpenDBForPath(dbPath)
	if err != nil {
		log.Printf("[telegram] selfImprove: open db: %v", err)
		return
	}
	defer db.Close()

	sessionID := fmt.Sprintf("telegram:%d", chatID)

	// Anchor the user's message if it's substantial (weight based on length)
	if len(userText) > 100 {
		weight := 1
		if len(userText) > 200 {
			weight = 2
		}
		facts := core.ExtractFactsFromText(userText)
		tags := core.ExtractTagsFromText(userText)
		summary := truncate(userText, 100)
		if err := core.InsertAnchor(db, facts, summary, tags, "user_message", sessionID, weight); err != nil {
			log.Printf("[telegram] InsertAnchor error: %v", err)
		} else {
			// Check topic density and trigger async condensation if threshold met
			go func() {
				density, err := core.GetTopicAnchorDensity(db)
				if err != nil || len(density) == 0 {
					return
				}
				for tag, count := range density {
					if count >= 5 {
						log.Printf("[telegram] condensation: tag=%s count=%d, triggering forge", tag, count)
						go func(t string) {
							if err := core.CondenseAnchors(db, t, &h.agentConfig.Synth); err != nil {
								log.Printf("[telegram] CondenseAnchors error: %v", err)
							} else {
								log.Printf("[telegram] condensation: tag=%s completed", t)
							}
						}(tag)
					}
				}
			}()
		}
	}

	// Extract a lesson if the exchange was informative (response has useful content)
	if len(responseText) > 100 && len(userText) > 20 {
		lessonContent := fmt.Sprintf("User asked: %s | Response covered: %s",
			truncate(userText, 100), truncate(responseText, 200))
		lesson := core.Lesson{
			ID:      core.GenerateID(),
			Content: lessonContent,
			Type:    "exchange",
			Tags:    "telegram,conversation",
		}
		if err := core.ExtractLesson(db, lesson); err != nil {
			log.Printf("[telegram] ExtractLesson error: %v", err)
		}
	}
}

// scheduleSessionSummary starts a 5-minute idle timer for session summarization.
// If a new message arrives for this chat before the timer fires, the timer is cancelled
// and a new one starts (handled by cancelOnNewMessage via summaryTimers sync.Map).
// Cancel pending summaries on /new or /clear by calling cancelSummaryTimer(chatID).
func (h *Handler) scheduleSessionSummary(chatID int64, messages []map[string]interface{}) {
	if len(messages) < 10 {
		return // Don't summarize short sessions
	}

	// Cancel any existing timer for this chat
	h.cancelSummaryTimer(chatID)

	// Create cancellation channel and store it
	cancelCh := make(chan struct{}, 1)
	h.summaryTimers.Store(chatID, cancelCh)

	// Wait 5 minutes idle, then summarize
	select {
	case <-cancelCh:
		// Cancelled — new message arrived, don't summarize
		h.summaryTimers.Delete(chatID)
		return
	case <-time.After(5 * time.Minute):
		// Idle timeout — do the summarization
		h.summaryTimers.Delete(chatID)
		h.summarizeSession(chatID, messages)
	}
}

// cancelSummaryTimer cancels any pending summary timer for a chat.
// Called when user sends a new message or /new /clear.
func (h *Handler) cancelSummaryTimer(chatID int64) {
	if v, ok := h.summaryTimers.LoadAndDelete(chatID); ok {
		if ch, ok := v.(chan struct{}); ok {
			select {
			case ch <- struct{}{}:
			default:
			}
		}
	}
}

// summarizeSession uses a lightweight model to summarize the session buffer
// and stores the result in session_summaries.
func (h *Handler) summarizeSession(chatID int64, messages []map[string]interface{}) {
	// Build a concise text representation of the session for summarization
	var sb strings.Builder
	for _, m := range messages {
		role, _ := m["role"].(string)
		content := contentOfMsg(m)
		if content == "" {
			continue
		}
		trunc := content
		if len(trunc) > 150 {
			trunc = trunc[:150] + "..."
		}
		sb.WriteString(fmt.Sprintf("%s: %s\n", role, trunc))
	}
	sessionText := sb.String()
	if sessionText == "" {
		return
	}

	// Generate a one-line summary using the same API the agent uses
	summary, err := h.generateSessionSummary(sessionText)
	if err != nil {
		log.Printf("[telegram] summarizeSession: failed: %v", err)
		return
	}

	// Extract last topic from last user message
	lastTopic := ""
	for i := len(messages) - 1; i >= 0; i-- {
		if role, _ := messages[i]["role"].(string); role == "user" {
			lastTopic = contentOfMsg(messages[i])
			if len(lastTopic) > 60 {
				lastTopic = lastTopic[:60] + "..."
			}
			break
		}
	}

	if err := h.sm.UpdateSessionSummary(chatID, summary, lastTopic); err != nil {
		log.Printf("[telegram] summarizeSession: UpdateSessionSummary error: %v", err)
	}
}

// generateSessionSummary calls the synthesis API to summarize sessionText in one sentence.
func (h *Handler) generateSessionSummary(sessionText string) (string, error) {
	cfg := &h.agentConfig.Synth
	if cfg.APIKey == "" {
		return "", fmt.Errorf("no API key")
	}

	summaryReq := struct {
		Model     string `json:"model"`
		MaxTokens int    `json:"max_tokens"`
		System    string `json:"system"`
		Messages  []struct {
			Role    string `json:"role"`
			Content string `json:"content"`
		} `json:"messages"`
	}{
		Model:     cfg.Model,
		MaxTokens: 100,
		System:    "You are a concise summarizer. Reply with exactly one sentence (max 80 chars) summarizing the conversation topic. No preamble, no quotes.",
		Messages: []struct {
			Role    string `json:"role"`
			Content string `json:"content"`
		}{
			{Role: "user", Content: "Summarize this conversation in one sentence:\n\n" + sessionText},
		},
	}

	body, err := json.Marshal(summaryReq)
	if err != nil {
		return "", err
	}

	url := strings.TrimSuffix(cfg.BaseURL, "/") + "/messages"
	req, err := http.NewRequestWithContext(context.Background(), "POST", url, bytes.NewReader(body))
	if err != nil {
		return "", err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("x-api-key", cfg.APIKey)

	client := &http.Client{Timeout: 30 * time.Second}
	resp, err := client.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()

	respBody, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("summary API error %d: %s", resp.StatusCode, string(respBody))
	}

	var result struct {
		Content []struct {
			Type string `json:"type"`
			Text string `json:"text"`
		} `json:"content"`
	}
	if err := json.Unmarshal(respBody, &result); err != nil {
		return "", err
	}
	for _, block := range result.Content {
		if block.Type == "text" && block.Text != "" {
			return block.Text, nil
		}
	}
	return "", fmt.Errorf("no text in summary response")
}

// contentOfMsg extracts a string from a message's content field.
func contentOfMsg(m map[string]interface{}) string {
	switch v := m["content"].(type) {
	case string:
		return v
	case []interface{}:
		var parts []string
		for _, part := range v {
			if pm, ok := part.(map[string]interface{}); ok {
				if t, ok := pm["type"].(string); ok && t == "text" {
					if txt, ok := pm["text"].(string); ok {
						parts = append(parts, txt)
					}
				}
			}
		}
		return strings.Join(parts, "\n")
	}
	return ""
}

func truncate(s string, maxLen int) string {
	if len(s) <= maxLen {
		return s
	}
	return s[:maxLen]
}

// sendText sends a plain text message using a background context
// so it isn't tied to the handler's long-polling context deadline.
func (h *Handler) sendText(_ *th.Context, chatID int64, text string) {
	if text == "" {
		return
	}
	log.Printf("[telegram] sending %d chars to %d", len(text), chatID)
	msg := tu.Message(tu.ID(chatID), text)
	msg.LinkPreviewOptions = &telego.LinkPreviewOptions{IsDisabled: true}
	_, err := h.bot.SendMessage(context.Background(), msg)
	if err != nil {
		log.Printf("[telegram] send error: %v", err)
	}
}

// SmartFenceChunk splits text into Telegram-safe chunks while preserving
// balanced markdown code fences. Code blocks of any size are split with
// properly balanced opening/closing fences across chunk boundaries.
func SmartFenceChunk(text string, maxLen int) []string {
	if len(text) <= maxLen {
		return []string{text}
	}

	const safetyMargin = 196 // keeps us well under 4096 even with fence overhead
	effectiveMax := maxLen - safetyMargin

	var chunks []string
	var buf strings.Builder
	var currentLang string
	inCodeBlock := false

	lines := strings.Split(text, "\n")
	for _, line := range lines {
		isFence := strings.HasPrefix(line, "```")

		if isFence {
			if !inCodeBlock {
				// Opening fence — extract language
				inCodeBlock = true
				lang := strings.TrimPrefix(strings.TrimSpace(line), "```")
				if lang != "" {
					currentLang = lang
				}
			} else {
				// Closing fence
				inCodeBlock = false
				currentLang = ""
			}
		}

		// Check if adding this line would exceed the limit
		candidate := line
		if buf.Len() > 0 && buf.Len()+1+len(candidate) > effectiveMax {
			// Need to emit current chunk
			if inCodeBlock {
				// Close the code block before emitting
				buf.WriteString("\n```")
			}
			chunks = append(chunks, buf.String())
			buf.Reset()

			if inCodeBlock {
				// Reopen the code block in the next chunk
				langPrefix := ""
				if currentLang != "" {
					langPrefix = currentLang + "\n"
				}
				buf.WriteString("```" + langPrefix + line + "\n")
				continue
			}
		}

		if buf.Len() > 0 {
			buf.WriteString("\n")
		}
		buf.WriteString(line)
	}

	// Emit final chunk
	if buf.Len() > 0 {
		if inCodeBlock {
			buf.WriteString("\n```")
		}
		chunks = append(chunks, buf.String())
	}

	if len(chunks) == 0 {
		return []string{text}
	}
	return chunks
}

// sendLongText sends a long text as one or more messages, respecting Telegram's 4096 char limit.
func (h *Handler) sendLongText(ctx *th.Context, chatID int64, text string) {
	const maxLen = 4096
	chunks := SmartFenceChunk(text, maxLen)
	for i, chunk := range chunks {
		h.sendText(ctx, chatID, chunk)
		if i < len(chunks)-1 {
			time.Sleep(150 * time.Millisecond)
		}
	}
}

// stripMediaPaths removes media file references from text so users don't see raw paths.
func stripMediaPaths(text string) string {
	lines := strings.Split(text, "\n")
	var filtered []string
	for _, line := range lines {
		trimmed := strings.TrimSpace(line)
		// Skip lines that are just a media file reference
		isMediaRef := (strings.Contains(trimmed, "/media/img_") && strings.HasSuffix(trimmed, ".jpg")) ||
			(strings.Contains(trimmed, "\\media\\img_") && strings.HasSuffix(trimmed, ".jpg")) ||
			(strings.Contains(trimmed, "/media/speech_") && strings.HasSuffix(trimmed, ".mp3")) ||
			(strings.Contains(trimmed, "\\media\\speech_") && strings.HasSuffix(trimmed, ".mp3"))
		if isMediaRef {
			continue
		}
		filtered = append(filtered, line)
	}
	return strings.TrimSpace(strings.Join(filtered, "\n"))
}

// deliverResponse parses the agent response for media file references,
// sends media first, then returns the text with paths stripped for display.
func (h *Handler) deliverResponse(ctx *th.Context, chatID int64, response string) {
	// Check for image paths (media/img_*.jpg)
	imgPattern := regexp.MustCompile(`(media[/\\]img_[a-f0-9]+\.jpg)`)
	audioPattern := regexp.MustCompile(`(media[/\\]speech_[a-f0-9]+\.mp3)`)

	imgMatches := imgPattern.FindAllStringSubmatchIndex(response, -1)
	audioMatches := audioPattern.FindAllStringSubmatchIndex(response, -1)

	// Send image if present
	for _, match := range imgMatches {
		if len(match) < 4 {
			continue
		}
		imgPath := response[match[2]:match[3]]
		if !filepath.IsAbs(imgPath) {
			imgPath = filepath.Join(h.mediaDir, filepath.Base(imgPath))
		}
		// Extract caption: text before the line containing the path
		prefixEnd := strings.LastIndex(response[:match[0]], "\n")
		if prefixEnd == -1 {
			prefixEnd = 0
		} else {
			// Don't include the newline itself
			prefixEnd++
		}
		caption := strings.TrimSpace(response[prefixEnd:match[0]])
		// Strip any other media paths from caption
		caption = stripMediaPaths(caption)
		h.sendPhotoWithCaption(ctx, chatID, imgPath, caption)
		return // one image per response for now
	}

	// Send audio if present
	for _, match := range audioMatches {
		if len(match) < 4 {
			continue
		}
		audioPath := response[match[2]:match[3]]
		if !filepath.IsAbs(audioPath) {
			audioPath = filepath.Join(h.mediaDir, filepath.Base(audioPath))
		}
		prefixEnd := strings.LastIndex(response[:match[0]], "\n")
		if prefixEnd == -1 {
			prefixEnd = 0
		} else {
			prefixEnd++
		}
		caption := strings.TrimSpace(response[prefixEnd:match[0]])
		caption = stripMediaPaths(caption)
		h.sendAudioWithCaption(ctx, chatID, audioPath, caption)
		return // one audio per response for now
	}

	// No media — send as normal text
	h.sendLongText(ctx, chatID, response)
}

// sendPhotoWithCaption sends a photo with an optional caption.
// If caption exceeds 1024 chars, sends photo first then caption as a follow-up message.
func (h *Handler) sendPhotoWithCaption(ctx *th.Context, chatID int64, photoPath string, caption string) {
	if photoPath == "" {
		return
	}
	file, err := os.Open(photoPath)
	if err != nil {
		log.Printf("[telegram] sendPhotoWithCaption: open %s: %v", photoPath, err)
		h.sendLongText(ctx, chatID, caption)
		return
	}
	defer file.Close()

	const captionMax = 1024
	useCaption := caption
	sendCaptionAfter := false
	if len(caption) > captionMax {
		useCaption = ""
		sendCaptionAfter = true
	}

	params := &telego.SendPhotoParams{
		ChatID:    tu.ID(chatID),
		Photo:     tu.FileFromReader(file, photoPath),
		Caption:   useCaption,
		ParseMode: "Markdown",
	}

	_, err = h.bot.SendPhoto(context.Background(), params)
	if err != nil {
		log.Printf("[telegram] sendPhoto error: %v", err)
		h.sendLongText(ctx, chatID, caption)
		return
	}

	log.Printf("[telegram] sent photo to %d", chatID)

	if sendCaptionAfter {
		stripped := stripMediaPaths(caption)
		h.sendLongText(ctx, chatID, stripped)
	}
}

// sendAudioWithCaption sends an audio file using sendAudio (not sendVoice) to avoid
// ffmpeg dependency for OPG encoding. Uses caption as fallback if send fails.
func (h *Handler) sendAudioWithCaption(ctx *th.Context, chatID int64, audioPath string, caption string) {
	if audioPath == "" {
		return
	}
	file, err := os.Open(audioPath)
	if err != nil {
		log.Printf("[telegram] sendAudioWithCaption: open %s: %v", audioPath, err)
		h.sendLongText(ctx, chatID, caption)
		return
	}
	defer file.Close()

	const captionMax = 1024
	useCaption := caption
	sendCaptionAfter := false
	if len(caption) > captionMax {
		useCaption = ""
		sendCaptionAfter = true
	}

	params := &telego.SendAudioParams{
		ChatID:  tu.ID(chatID),
		Audio:   tu.FileFromReader(file, audioPath),
		Caption: useCaption,
	}

	_, err = h.bot.SendAudio(context.Background(), params)
	if err != nil {
		log.Printf("[telegram] sendAudio error: %v", err)
		h.sendLongText(ctx, chatID, caption)
		return
	}

	log.Printf("[telegram] sent audio to %d", chatID)

	if sendCaptionAfter {
		stripped := stripMediaPaths(caption)
		h.sendLongText(ctx, chatID, stripped)
	}
}
