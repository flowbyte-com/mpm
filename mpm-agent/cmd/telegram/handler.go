package main

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
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

// chatSettings holds per-chat preferences.
type chatSettings struct {
	thinkLevel    int    // 0=off, 1=brief, 2=normal, 3=verbose
	verbose       bool   // if true, don't strip thinking blocks from responses
	toolProfile   string // active tool profile name, default "standard"
	streamProgress bool  // if true, stream tool progress to live message
}

// Handler routes incoming Telegram updates to the agent.
type Handler struct {
	bot          *telego.Bot
	cfg          *TelegramConfig
	sm           *SessionManager
	agentConfig  *core.MiniBotConfig
	mediaDir     string              // absolute path to media cache directory
	activeReplies sync.Map           // chatID → true (prevents double-reply)
	chatSettings  sync.Map          // chatID → *chatSettings
	liveMessage   sync.Map          // chatID (int64) → *liveMessageData
}

// liveMessageData holds the state for a live-streaming message in one chat.
type liveMessageData struct {
	messageID int
	buffer    strings.Builder
	mu        sync.Mutex
	dirty     bool          // buffer changed since last flush
	flusher   *flusher      // stop via flusher field to prevent goroutine leak
}

// flusher runs in background, flushes buffer edits to Telegram at ~1.1s interval.
type flusher struct {
	handler *Handler
	chatID  int64
	ticker  *time.Ticker
	stopCh  chan struct{}
	doneCh  chan struct{} // closed when goroutine fully exits
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
	defer close(f.doneCh)
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

func (r *telegramToolReporter) ToolStarted(chatID int64, toolName string, input map[string]interface{}) string {
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
	return toolName
}

func (r *telegramToolReporter) ToolCompleted(chatID int64, reportID string, toolName string, summary string) {
	entry := r.h.getLiveMessage(r.chatID)
	if entry == nil {
		return
	}
	entry.mu.Lock()
	entry.buffer.WriteString(fmt.Sprintf("  %s\n", summary))
	entry.dirty = true
	entry.mu.Unlock()
}

func (r *telegramToolReporter) SendAlert(chatID int64, message string) {
	r.h.sendText(nil, r.chatID, message)
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
	sent, err := h.bot.SendMessage(context.Background(), tu.Message(tu.ID(chatID), "🧠 Working..."))
	if err != nil {
		log.Printf("[telegram] live message init error: %v", err)
		return nil
	}
	data := &liveMessageData{messageID: sent.MessageID}
	data.flusher = newFlusher(h, chatID)
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
	entry.mu.Unlock()

	_, err := h.bot.EditMessageText(context.Background(), &telego.EditMessageTextParams{
		ChatID:    tu.ID(chatID),
		MessageID: entry.messageID,
		Text:      entry.buffer.String(),
	})
	if err != nil {
		h.sendText(nil, chatID, entry.buffer.String())
	}
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

// cleanResponse removes thinking blocks and extra whitespace for clean Telegram display.
func cleanResponse(text string) string {
	patterns := []string{
		`(?si)<thinking>.*?</thinking>`,
		"(?s)" + "《" + "[^》]*》",
		"(?s)" + "（" + "[^）]*）",
		"(?s)" + "。" + "[^。]*。",
	}
	for _, p := range patterns {
		re := regexp.MustCompile(p)
		text = re.ReplaceAllString(text, "")
	}
	text = regexp.MustCompile(`\n{3,}`).ReplaceAllString(text, "\n")
	return strings.TrimSpace(text)
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
		// Clear session history for this chat
		if err := h.sm.Save(chatID, nil); err != nil {
			return true, fmt.Sprintf("Failed to clear session: %v", err)
		}
		// Clear loaded toolkits for this session
		core.ClearSessionToolkits(fmt.Sprintf("telegram:%d", chatID))
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
		case "off":  level = 0
		case "low":  level = 1
		case "adaptive": level = 2
		case "med", "medium": level = 3
		case "high": level = 4
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
		return true, "Commands:\n/new or /clear — clear session\n/think [off|low|adaptive|med|high] — set thinking level\n/reasoning [on|off] — toggle reasoning mode\n/verbose [on|off] — show/hide thinking blocks\n/tools [name] — show or switch tool profiles\n/stream [on|off] — show/hide tool progress\n/status — show current settings"

	default:
		return false, ""
	}
}

// startTyping sends typing indicator and keeps it going while we process.
// Uses background context for API calls so they aren't killed by the
// long-polling context deadline.
func (h *Handler) startTyping(ctx *th.Context, chatID int64) {
	// Send initial typing action with background context
	_ = h.bot.SendChatAction(context.Background(), &telego.SendChatActionParams{
		ChatID: tu.ID(chatID),
		Action: telego.ChatActionTyping,
	})

	// Keep sending typing every 4 seconds
	go func() {
		ticker := time.NewTicker(4 * time.Second)
		defer ticker.Stop()
		for {
			select {
			case <-ticker.C:
				_ = h.bot.SendChatAction(context.Background(), &telego.SendChatActionParams{
					ChatID: tu.ID(chatID),
					Action: telego.ChatActionTyping,
				})
			case <-ctx.Done():
				return
			}
		}
	}()
}

// agentReply runs the MPM agent asynchronously (non-blocking).
// Immediately sends "🧠 Thinking..." and spawns a goroutine.
func (h *Handler) agentReply(chatID int64, userText string) {
	// Send immediate thinking indicator (non-blocking)
	sent, err := h.bot.SendMessage(context.Background(), tu.Message(tu.ID(chatID), "🧠 Thinking..."))
	if err != nil {
		log.Printf("[telegram] send thinking error: %v", err)
		return
	}
	msgID := sent.MessageID

	// Run agent in goroutine (non-blocking) — Tier 1
	go h.runAgentWithTimeout(chatID, msgID, userText)
}

// runAgentWithTimeout runs the agent with 90s timeout and 15s heartbeat (Tier 2 + 3).
func (h *Handler) runAgentWithTimeout(chatID int64, msgID int, userText string) (finalMsg string) {
	// Create 90s timeout context — Tier 2 hard kill switch
	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()
	defer h.finalizeLiveMessage(chatID, finalMsg)

	defer func() {
		if r := recover(); r != nil {
			finalMsg = "❌ Agent crashed or was terminated"
			log.Printf("[telegram] agent panic recovered: %v", r)
		}
	}()

	// Start heartbeat ticker (every 15s) — Tier 3
	ticker := time.NewTicker(15 * time.Second)
	defer ticker.Stop()
	dots := 0

	go func() {
		for {
			select {
			case <-ticker.C:
				dots = (dots + 1) % 4
				h.bot.EditMessageText(context.Background(), &telego.EditMessageTextParams{
					ChatID:    tu.ID(chatID),
					MessageID: msgID,
					Text:      "🧠" + strings.Repeat(".", dots),
				})
			case <-ctx.Done():
				return
			}
		}
	}()

	// Load existing history
	history, err := h.sm.Get(chatID)
	if err != nil || history == nil {
		history = []map[string]interface{}{}
	}

	// Open database
	binaryDir := core.GetBinaryDir()
	db, err := core.OpenDBForPath(core.ResolveMiniBotDBPath())
	if err != nil {
		h.editMessage(chatID, msgID, fmt.Sprintf("⚠️ Error: %v", err))
		return
	}
	defer db.Close()

	// Resolve identity path
	identityPath := core.ResolveIdentityPath(binaryDir, h.agentConfig.Paths.Identity)

	// Get active tool profile
	profile := h.getSettings(chatID).toolProfile
	if profile == "" {
		profile = "standard"
	}
	profileTools := h.agentConfig.Profiles[profile]
	if profileTools == nil {
		profileTools = []string{}
	}

	// Session ID for toolkit state
	sessionID := fmt.Sprintf("telegram:%d", chatID)

	// Wire tool progress reporter if streaming is enabled
	settings := h.getSettings(chatID)
	var reporter core.ToolProgressReporter
	if settings.streamProgress {
		reporter = &telegramToolReporter{h: h, chatID: chatID}
	}

	// Call RunAgent with context (ctx is the 90s deadline)
	responseText, err := core.RunAgent(ctx, userText, history, db, identityPath,
		&h.agentConfig.Synth, profileTools, sessionID, h.agentConfig.Toolkits, chatID, reporter)

	// Stop heartbeat
	ticker.Stop()

	// Handle result
	if err != nil {
		h.editMessage(chatID, msgID, fmt.Sprintf("⚠️ Error: %v", err))
		return
	}

	if responseText == "" {
		responseText = "(no response)"
	}

	// Strip thinking blocks if verbose off
	if !settings.verbose {
		responseText = cleanResponse(responseText)
	}

	// Update thinking message with final result
	h.editMessage(chatID, msgID, responseText)

	// Save to history
	updatedHistory := append(history,
		map[string]interface{}{"role": "user", "content": userText},
		map[string]interface{}{"role": "assistant", "content": responseText},
	)
	h.sm.Save(chatID, updatedHistory)

	// Self-improve
	dbPath := core.ResolveMiniBotDBPath()
	go h.selfImprove(dbPath, chatID, userText, responseText)
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
	if len(userText) > 50 {
		weight := 1
		if len(userText) > 200 {
			weight = 2
		}
		if err := core.InsertAnchor(db, userText, "user_message", sessionID, weight); err != nil {
			log.Printf("[telegram] InsertAnchor error: %v", err)
		}
	}

	// Extract a lesson if the exchange was informative (response has useful content)
	if len(responseText) > 100 && len(userText) > 20 {
		lessonContent := fmt.Sprintf("User asked: %s | Response covered: %s",
			truncate(userText, 100), truncate(responseText, 200))
		lesson := core.Lesson{
			ID:       core.GenerateID(),
			Content:  lessonContent,
			Type:     "exchange",
			Tags:     "telegram,conversation",
		}
		if err := core.ExtractLesson(db, lesson); err != nil {
			log.Printf("[telegram] ExtractLesson error: %v", err)
		}
	}
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

// sendLongText sends a long text as one or more messages, respecting Telegram's 4096 char limit.
func (h *Handler) sendLongText(ctx *th.Context, chatID int64, text string) {
	const maxLen = 4096
	if len(text) <= maxLen {
		h.sendText(ctx, chatID, text)
		return
	}

	// Split on paragraph boundaries
	paragraphs := strings.Split(text, "\n\n")
	var buf strings.Builder
	for _, para := range paragraphs {
		para = strings.TrimSpace(para)
		if para == "" {
			continue
		}
		if buf.Len()+len(para)+2 > maxLen {
			if buf.Len() > 0 {
				h.sendText(ctx, chatID, buf.String())
				time.Sleep(150 * time.Millisecond)
				buf.Reset()
			}
		}
		if buf.Len() > 0 {
			buf.WriteString("\n\n")
		}
		buf.WriteString(para)
	}
	if buf.Len() > 0 {
		h.sendText(ctx, chatID, buf.String())
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
