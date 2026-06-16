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
//
// Provenance: the MCP host (Claude Code, OpenClaw) populates child stdio
// env from the `env` block in `.mcp.json`. We read MPM_ACTIVE_MODE /
// MPM_ACTIVE_PERSONA at startup and pass them as internal.ActiveContext to
// write handlers, so every memory/decision row carries the active mode
// and persona that produced it.

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

	ac := internal.ActiveContext{
		Mode:    os.Getenv("MPM_ACTIVE_MODE"),
		Persona: os.Getenv("MPM_ACTIVE_PERSONA"),
	}

	s := server.NewMCPServer("mpm-mcp", "0.1.0")
	RegisterAllTools(s, dm, ac)

	if err := server.ServeStdio(s); err != nil {
		log.Fatalf("mpm-mcp: serve stdio: %v", err)
	}
}
