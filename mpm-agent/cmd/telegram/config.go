package main

import (
	"fmt"

	"mpm-agent/core"
)

// TelegramConfig holds the Telegram bot configuration.
type TelegramConfig struct {
	BotToken     string  `json:"bot_token"`
	AllowedUsers []int64 `json:"allowed_users"` // Telegram user IDs allowed to interact
	Polling      bool    `json:"polling"`
	WebhookURL   string  `json:"webhook_url"`
}

// LoadTelegramConfig reads mini-bot-config.json and extracts the telegram block.
func LoadTelegramConfig() (*TelegramConfig, error) {
	cfg, err := core.LoadMiniBotConfig(core.GetConfigPath())
	if err != nil {
		return nil, fmt.Errorf("load config: %w", err)
	}
	if cfg.Telegram.BotToken == "" {
		return nil, fmt.Errorf("telegram.bot_token is not set in %s", core.GetConfigPath())
	}
	return &TelegramConfig{
		BotToken:     cfg.Telegram.BotToken,
		AllowedUsers: cfg.Telegram.AllowedUsers,
		Polling:      cfg.Telegram.Polling,
	}, nil
}
