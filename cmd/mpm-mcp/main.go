// mpm-mcp - Native Go MCP server for MPM.
//
// Exposes MPM reasoning primitives to MCP clients (Claude Code, etc.) over
// stdio. This replaces the Python wrapper at .claude/mpm-mcp/server.py and
// runs in-process against the MPM database — no subprocess, no shell, no
// Python interpreter startup cost.
//
// Tools are registered in tools.go via RegisterAllTools, which both
// this server and the `mpm call <tool>` CLI share through the dm methods
// in internal/call_helpers.go. The list of s.AddTool(...) calls in
// tools.go is the canonical count of the MCP tool surface.
//
// Provenance: the MCP host (Claude Code, OpenClaw) populates child stdio
// env from the `env` block in `.mcp.json`. We read MPM_ACTIVE_MODE /
// MPM_ACTIVE_PERSONA at startup and pass them as internal.ActiveContext to
// write handlers, so every memory/decision row carries the active mode
// and persona that produced it.

package main

import (
	"context"
	"errors"
	"log"
	"log/slog"
	"os/signal"
	"syscall"

	"github.com/mark3labs/mcp-go/server"

	"github.com/flowbyte-com/mpm-core"
	"github.com/flowbyte-com/mpm-core/logging"
	"github.com/flowbyte-com/mpm-core/mpmcli"
)

// router is initialised once at server boot — patterns and anti-patterns
// from all mode/*.md and persona/*.md files are compiled to regex at that
// point, so Evaluate() is pure string matching with zero parsing overhead.

func main() {
	logging.Setup()

	// No single-instance lock. mpm-mcp is a stdio MCP server — one
	// process per MCP host (each host gets its own stdin/stdout pair).
	// Multiple hosts (OpenClaw gateway + concurrent Hermes sessions)
	// can spawn their own mpm-mcp concurrently without contention.
	//
	// Concurrent writes to mpm.db are safe via SQLite WAL mode +
	// 5s busy_timeout (see internal/core/db.go init). The previous
	// pidfile singleton was a workaround for OpenClaw's broken
	// respawn behavior (it spawned new mpm-mcp children without
	// reaping the old ones), but it broke multi-host MCP usage:
	// whichever host acquired the lock first won, the others got
	// silent "another live instance holds the lock" failures and
	// ran with no mcp__mpm tools at all.
	//
	// The old failure mode (stale OpenClaw children holding open
	// mpm.db handles) is now harmless: WAL serializes writes, OS
	// reaps processes on parent death, and the worst case is
	// N idle mpm-mcp children holding memory — not SQLITE_BUSY
	// errors.
	slog.Info("mpm-mcp starting (no pidfile singleton; stdio-per-host model)")
	workspace := mpmcli.ResolveWorkspace()

	dm, err := internal.NewDatabaseManager(workspace)
	if err != nil {
		log.Fatalf("mpm-mcp: open database: %v", err)
	}
	defer dm.Close()

	ac := mpmcli.ActiveContextFromEnv()

	// Build the router once — pre-compiles all mode/persona patterns at boot.
	router, err := internal.NewRouter(workspace)
	if err != nil {
		log.Fatalf("mpm-mcp: build router: %v", err)
	}

	s := server.NewMCPServer("mpm-mcp", "0.1.0")
	RegisterAllTools(s, dm, ac, router)

	// Translate SIGTERM/SIGINT into a context cancellation so the
	// stdio server can shut down cleanly. The defer above releases
	// the pidfile; the order matters — we want the lock removed
	// AFTER the database is closed.
	ctx, cancel := signal.NotifyContext(context.Background(), syscall.SIGTERM, syscall.SIGINT)
	defer cancel()

	if err := server.ServeStdio(s); err != nil && !errors.Is(err, context.Canceled) {
		log.Fatalf("mpm-mcp: serve stdio: %v", err)
	}
	_ = ctx
}

