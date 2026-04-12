package main

import (
	"context"
	"fmt"
	"log"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/mymmrac/telego"
	th "github.com/mymmrac/telego/telegohandler"

	"mpm-agent/core"
)

// Default commands registered on startup
var defaultCommands = []telego.BotCommand{
	{Command: "new", Description: "Clear session and start fresh"},
	{Command: "clear", Description: "Clear session history"},
	{Command: "think", Description: "Set thinking level: low, med, high, adaptive"},
	{Command: "reasoning", Description: "Toggle reasoning mode: on/off"},
	{Command: "verbose", Description: "Toggle verbose mode: on/off"},
	{Command: "status", Description: "Show current settings"},
}

func main() {
	pid := os.Getpid()
	os.Args[0] = fmt.Sprintf("mini-bot-telegram[%d]", pid)

	log.SetFlags(log.LstdFlags | log.Lshortfile)
	log.Printf("mini-bot-telegram[%d]: Starting...", pid)

	// Load Telegram config
	cfg, err := LoadTelegramConfig()
	if err != nil {
		log.Fatalf("mini-bot-telegram[%d]: Telegram config error: %v", pid, err)
	}
	log.Printf("mini-bot-telegram[%d]: Allowed users: %d", pid, len(cfg.AllowedUsers))

	// Load agent config
	agentCfg, err := core.LoadMiniBotConfig(core.GetConfigPath())
	if err != nil {
		log.Fatalf("mini-bot-telegram[%d]: Agent config error: %v", pid, err)
	}
	log.Printf("mini-bot-telegram[%d]: Model: %s @ %s", pid, agentCfg.Synth.Model, agentCfg.Synth.BaseURL)

	// Load identity
	binaryDir := core.GetBinaryDir()
	identity, _ := core.LoadIdentity(binaryDir)
	if identity != nil {
		log.Printf("mini-bot-telegram[%d]: Identity: %s v%s", pid, identity.Name, identity.Version)
	}

	// Open session database
	sm, err := LoadSessionManager(core.ResolveMiniBotDBPath())
	if err != nil {
		log.Fatalf("mini-bot-telegram[%d]: Session manager error: %v", pid, err)
	}
	defer sm.Close()
	log.Printf("mini-bot-telegram[%d]: Session manager ready", pid)

	// Anti-Starvation Transport
	customTransport := &http.Transport{
		ForceAttemptHTTP2:   false,
		MaxConnsPerHost:     100,
		MaxIdleConnsPerHost: 100,
		MaxIdleConns:         100,
		IdleConnTimeout:      90 * time.Second,
		DisableKeepAlives:   true,
	}
	customClient := &http.Client{Transport: customTransport}

	// Initialize Telegram Bot
	bot, err := telego.NewBot(cfg.BotToken, telego.WithHTTPClient(customClient))
	if err != nil {
		log.Fatalf("mini-bot-telegram[%d]: Bot init error: %v", pid, err)
	}
	log.Printf("mini-bot-telegram[%d]: Logged in as: @%s", pid, bot.Username())

	// Register commands with Telegram globally
	err = bot.SetMyCommands(context.Background(), &telego.SetMyCommandsParams{
		Commands: defaultCommands,
	})
	if err != nil {
		log.Printf("mini-bot-telegram[%d]: Warning: could not set commands: %v", pid, err)
	} else {
		log.Printf("mini-bot-telegram[%d]: Commands registered: new, clear, think, reasoning, verbose, status", pid)
	}

	// Create handler
	handler := NewHandler(bot, cfg, sm, agentCfg)

	if !cfg.Polling {
		log.Printf("mini-bot-telegram[%d]: Polling disabled", pid)
		os.Exit(0)
	}

	// Start long polling
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	updates, err := bot.UpdatesViaLongPolling(ctx, &telego.GetUpdatesParams{
		Timeout: 60,
	})
	if err != nil {
		log.Fatalf("mini-bot-telegram[%d]: Long polling failed: %v", pid, err)
	}

	// Create bot handler
	bh, err := th.NewBotHandler(bot, updates)
	if err != nil {
		log.Fatalf("mini-bot-telegram[%d]: BotHandler init failed: %v", pid, err)
	}

	// Handle text messages
	bh.HandleMessage(handler.Handle, th.AnyMessageWithText())

	// Graceful shutdown
	sigChan := make(chan os.Signal, 1)
	signal.Notify(sigChan, syscall.SIGINT, syscall.SIGTERM)

	go func() {
		<-sigChan
		log.Printf("mini-bot-telegram[%d]: Shutting down...", pid)
		cancel()
		bh.Stop()
		sm.Close()
		os.Exit(0)
	}()

	log.Printf("mini-bot-telegram[%d]: Message handler started", pid)
	if err := bh.Start(); err != nil {
		log.Printf("mini-bot-telegram[%d]: BotHandler error: %v", pid, err)
	}
}