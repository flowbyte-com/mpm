package main

import (
	"encoding/json"
	"fmt"
	"os"

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
	configPath := core.GetConfigPath()
	data, err := os.ReadFile(configPath)
	if err != nil {
		return nil, fmt.Errorf("cannot read config at %s: %w", configPath, err)
	}
	var raw struct {
		Telegram *TelegramConfig `json:"telegram"`
	}
	if err := json.Unmarshal(data, &raw); err != nil {
		return nil, fmt.Errorf("parse config: %w", err)
	}
	if raw.Telegram == nil {
		return nil, fmt.Errorf("no [telegram] section found in %s", configPath)
	}
	if raw.Telegram.BotToken == "" {
		return nil, fmt.Errorf("telegram.bot_token is not set in %s", configPath)
	}
	return raw.Telegram, nil
}