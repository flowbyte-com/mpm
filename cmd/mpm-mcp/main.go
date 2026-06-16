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

// router is initialised once at server boot — patterns and anti-patterns
// from all mode/*.md and persona/*.md files are compiled to regex at that
// point, so Evaluate() is pure string matching with zero parsing overhead.

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

	// Build the router once — pre-compiles all mode/persona patterns at boot.
	router, err := internal.NewRouter(workspace)
	if err != nil {
		log.Fatalf("mpm-mcp: build router: %v", err)
	}

	s := server.NewMCPServer("mpm-mcp", "0.1.0")
	RegisterAllTools(s, dm, ac, router)

	if err := server.ServeStdio(s); err != nil {
		log.Fatalf("mpm-mcp: serve stdio: %v", err)
	}
}
