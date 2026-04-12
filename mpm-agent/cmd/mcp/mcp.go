package main

import (
	"fmt"
	"log"
	"os"

	_ "github.com/mattn/go-sqlite3"

	"mpm-agent/core"
)

func main() {
	pid := os.Getpid()
	os.Args[0] = fmt.Sprintf("mini-bot-mcp[%d]", pid)

	// Load mini-bot config for token auth
	configPath := core.GetConfigPath()
	cfg, err := core.LoadMiniBotConfig(configPath)
	if err != nil {
		log.Printf("mini-bot-mcp[%d]: warning: could not load config: %v", pid, err)
	}

	// Token auth: use cfg.MCP.Token, fall back to MPM_API_TOKEN env var
	expectedToken := ""
	if cfg != nil {
		expectedToken = cfg.MCP.Token
	}
	if expectedToken == "" {
		expectedToken = os.Getenv("MPM_API_TOKEN")
	}

	log.Printf("mini-bot-mcp[%d]: Starting (token auth: %v)", pid, expectedToken != "")

	// Open mpm.db (shared MCP serves MPM tools)
	dbPath := core.ResolveMiniBotDBPath()
	db, err := core.OpenDBForPath(dbPath)
	if err != nil {
		log.Fatalf("mini-bot-mcp[%d]: open db: %v", pid, err)
	}
	defer db.Close()

	log.Printf("mini-bot-mcp[%d]: Ready on socket", pid)

	// JSON-RPC loop placeholder
	_ = expectedToken
	_ = db

	select {} // Block forever - MCP server runs continuously
}