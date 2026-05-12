package main

import (
	"mpm-agent/core"

	"github.com/mymmrac/telego"
)

// liveTransport implements core.Transport for Telegram with live message streaming.
type liveTransport struct {
	bot     *telego.Bot
	chatID  int64
	handler *Handler
}

var _ core.Transport = (*liveTransport)(nil)

func newLiveTransport(bot *telego.Bot, chatID int64, h *Handler) *liveTransport {
	return &liveTransport{bot: bot, chatID: chatID, handler: h}
}

func (t *liveTransport) ReadMessage() (string, error) {
	return "", nil // Not used — messages arrive via Handle()
}

func (t *liveTransport) WriteChunk(text string, isThinking bool) error {
	// Strip thinking blocks if verbose mode is off
	settings := t.handler.getSettings(t.chatID)
	if !settings.verbose {
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