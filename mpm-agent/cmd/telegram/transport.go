package main

import (
	"context"

	"github.com/mymmrac/telego"
	tu "github.com/mymmrac/telego/telegoutil"

	"mpm-agent/core"
)

// TelegramTransport implements core.Transport for the Telegram entry point.
// Messages arrive via Handle() rather than ReadMessage(), so ReadMessage
// is a no-op that returns ""/nil.
type TelegramTransport struct {
	bot    *telego.Bot
	chatID int64
}

// NewTelegramTransport creates a TelegramTransport for the given bot and chat.
func NewTelegramTransport(bot *telego.Bot, chatID int64) *TelegramTransport {
	return &TelegramTransport{bot: bot, chatID: chatID}
}

// ReadMessage is not used in Telegram — messages arrive via Handle() instead.
func (t *TelegramTransport) ReadMessage() (string, error) {
	return "", nil
}

// WriteChunk sends text to the Telegram chat.
func (t *TelegramTransport) WriteChunk(text string, isThinking bool) error {
	ctx := context.Background()
	_, err := t.bot.SendMessage(ctx, tu.Message(tu.ID(t.chatID), text))
	return err
}

// RequestToolApproval auto-approves all tool requests.
func (t *TelegramTransport) RequestToolApproval(toolName string, args string) bool {
	return true
}

// SendTypingIndicator sends a typing indicator to the Telegram chat.
func (t *TelegramTransport) SendTypingIndicator() error {
	ctx := context.Background()
	return t.bot.SendChatAction(ctx, &telego.SendChatActionParams{
		ChatID: tu.ID(t.chatID),
		Action: telego.ChatActionTyping,
	})
}

// StopTypingIndicator cancels the typing indicator by sending another typing action.
// Telegram does not have a cancel action, but sending another typing action
// refreshes the displayed indicator.
func (t *TelegramTransport) StopTypingIndicator() error {
	ctx := context.Background()
	return t.bot.SendChatAction(ctx, &telego.SendChatActionParams{
		ChatID: tu.ID(t.chatID),
		Action: telego.ChatActionTyping,
	})
}

var _ core.Transport = (*TelegramTransport)(nil)
