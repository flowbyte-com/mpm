// mpm-mcp - Native Go MCP server for MPM.
//
// Exposes MPM reasoning primitives to MCP clients (Claude Code, etc.) over
// stdio. This replaces the Python wrapper at .claude/mpm-mcp/server.py and
// runs in-process against the MPM database — no subprocess, no shell, no
// Python interpreter startup cost.
//
// All 18 tools are registered in tools.go via RegisterAllTools, which both
// this server and the `mpm call <tool>` CLI share through the dm methods
// in internal/call_helpers.go.

package main

import (
	"log"
	"os"

	"github.com/mark3labs/mcp-go/server"

	"mpm/internal"
)

func main() {
	workspace := os.Getenv("MPM_WORKSPACE")
	if workspace == "" {
		workspace = "."
	}

	dm, err := internal.NewDatabaseManager(workspace)
	if err != nil {
		log.Fatalf("mpm-mcp: open database: %v", err)
	}
	defer dm.Close()

	s := server.NewMCPServer("mpm-mcp", "0.1.0")
	RegisterAllTools(s, dm)

	if err := server.ServeStdio(s); err != nil {
		log.Fatalf("mpm-mcp: serve stdio: %v", err)
	}
}
