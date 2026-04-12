package main

import (
	"fmt"
	"log"
	"os"
)

func main() {
	pid := os.Getpid()
	os.Args[0] = fmt.Sprintf("mini-bot-mcp[%d]", pid)

	log.Printf("mini-bot-mcp[%d]: Starting MCP server...", pid)

	// MCP server implementation placeholder
	// This will be implemented in a future task

	select {} // Block forever - MCP server runs continuously
}