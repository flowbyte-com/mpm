package main

import (
	"context"
	"fmt"
	"log"
	"regexp"
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
	thinkLevel int  // 0=off, 1=brief, 2=normal, 3=verbose
	verbose    bool // if true, don't strip thinking blocks from responses
}

// Handler routes incoming Telegram updates to the agent.
type Handler struct {
	bot          *telego.Bot
	cfg          *TelegramConfig
	sm           *SessionManager
	agentConfig  *core.MiniBotConfig
	activeReplies sync.Map   // chatID → true (prevents double-reply)
	chatSettings  sync.Map   // chatID → *chatSettings
}

// NewHandler creates a new Telegram handler.
func NewHandler(bot *telego.Bot, cfg *TelegramConfig, sm *SessionManager, agentCfg *core.MiniBotConfig) *Handler {
	return &Handler{
		bot:         bot,
		cfg:         cfg,
		sm:          sm,
		agentConfig: agentCfg,
	}
}

// Handle processes an incoming Telegram message.
func (h *Handler) Handle(ctx *th.Context, message telego.Message) error {
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

	// Send "typing" indicator while agent works
	h.startTyping(ctx, chatID)

	// Run agent reply
	response, err := h.agentReply(chatID, text)
	if err != nil {
		h.sendText(ctx, chatID, fmt.Sprintf("Error: %v", err))
		return err
	}

	if response == "" {
		response = "(no response)"
	}

	// Strip thinking blocks only if verbose mode is off
	s := h.getSettings(chatID)
	if !s.verbose {
		response = cleanResponse(response)
	}

	// Send response
	h.sendLongText(ctx, chatID, response)
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

// getSettings returns per-chat settings, creating a default if needed.
func (h *Handler) getSettings(chatID int64) *chatSettings {
	v, ok := h.chatSettings.Load(chatID)
	if ok {
		return v.(*chatSettings)
	}
	s := &chatSettings{thinkLevel: 2, verbose: false}
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
		return true, fmt.Sprintf("Think: %s | Verbose: %s", labels[level], verb)

	case "/":
		return true, "Commands:\n/new or /clear — clear session\n/think [off|low|adaptive|med|high] — set thinking level\n/reasoning [on|off] — toggle reasoning mode\n/verbose [on|off] — show/hide thinking blocks\n/status — show current settings"

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

// agentReply runs the MPM agent for the given chat and user message.
func (h *Handler) agentReply(chatID int64, userText string) (string, error) {
	log.Printf("[telegram] agentReply start: %q", userText)

	// Load existing history
	t0 := time.Now()
	history, err := h.sm.Get(chatID)
	log.Printf("[telegram] history loaded in %v: %d msgs", time.Since(t0), len(history))
	if err != nil {
		return "", fmt.Errorf("load history: %w", err)
	}
	if history == nil {
		history = []map[string]interface{}{}
	}

	// Open database
	binaryDir := core.GetBinaryDir()
	db, err := core.OpenDBForPath(core.ResolveMiniBotDBPath())
	if err != nil {
		return "", fmt.Errorf("open db: %w", err)
	}
	defer db.Close()

	// Call RunAgent with correct signature: (query, db, binaryDir)
	responseText, err := core.RunAgent(userText, db, binaryDir)
	if err != nil {
		return "", fmt.Errorf("agent error: %w", err)
	}

	// Append user + assistant messages to history and persist
	updatedHistory := append(history,
		map[string]interface{}{"role": "user", "content": userText},
		map[string]interface{}{"role": "assistant", "content": responseText},
	)
	if err := h.sm.Save(chatID, updatedHistory); err != nil {
		log.Printf("[telegram] failed to save session for chat %d: %v", chatID, err)
		// Non-fatal: don't fail the reply
	}

	return responseText, nil
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
