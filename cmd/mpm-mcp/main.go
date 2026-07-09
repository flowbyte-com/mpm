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
	"fmt"
	"log"
	"log/slog"
	"os"
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

	// Acquire the single-instance lock before opening the database.
	// If another mpm-mcp already holds the lock, we are an orphan
	// from a previous gateway cycle and must yield the slot — exit
	// 0 silently so the gateway does not interpret this as a crash.
	pidfilePath := PidfilePath()
	if err := AcquirePidfile(pidfilePath); err != nil {
		if errors.Is(err, ErrOrphan) {
			slog.Info("mpm-mcp: another live instance holds the lock; exiting", "err", err)
			os.Exit(0)
		}
		log.Fatalf("mpm-mcp: pidfile: %v", err)
	}
	// Release the lock on any exit path: graceful shutdown via
	// signal, panic, or normal return. The PID check inside
	// ReleasePidfile means we only ever remove a file we own.
	pidfileAcquired = true
	defer ReleasePidfileOnExit(pidfilePath)

	slog.Info("mpm-mcp starting", "pidfile", pidfilePath)
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

// pidfileAcquired is the package-level latch set by main() once
// AcquirePidfile succeeds. ReleasePidfileOnExit reads it to decide
// whether the defer chain actually holds a lock to release — this
// prevents a defer that runs before the acquire from trying to
// remove a pidfile we never wrote.
var pidfileAcquired bool

// ReleasePidfileOnExit is the deferred helper called on every exit
// path. It is a no-op if AcquirePidfile did not succeed, and on
// success it removes the pidfile only if it still points at us.
func ReleasePidfileOnExit(path string) {
	if !pidfileAcquired {
		return
	}
	if err := ReleasePidfile(path); err != nil {
		fmt.Fprintf(os.Stderr, "mpm-mcp: release pidfile: %v\n", err)
	}
}
