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
	"io"
	"log"
	"log/slog"
	"os"
	"os/signal"
	"path/filepath"
	"syscall"
	"time"

	"github.com/mark3labs/mcp-go/server"

	"github.com/flowbyte-com/mpm/internal/blobstore"
	"github.com/flowbyte-com/mpm-core/tools"
	"github.com/flowbyte-com/mpm-core"
	"github.com/flowbyte-com/mpm-core/logging"
	"github.com/flowbyte-com/mpm-core/mpmcli"
)

// router is initialised once at server boot — patterns and anti-patterns
// from all mode/*.md and persona/*.md files are compiled to regex at that
// point, so Evaluate() is pure string matching with zero parsing overhead.

func main() {
	// Alpha-4 D-004/W-004: mpm-mcp is a stdio MCP server — its
	// stdout/stderr stream is the protocol boundary. Discard operational
	// INFO by default so the protocol stays parseable. Operators can
	// restore diagnostics with MPM_VERBOSE=1.
	if os.Getenv("MPM_VERBOSE") == "" {
		logging.SetupWithWriter(io.Discard)
	} else {
		logging.Setup()
	}

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

	// Integration invariant: every mpm-mcp boot must check the live
	// db_path against a host-pinned expected path before serving any
	// tool call. This catches the "orphaned database" failure mode where
	// an agent quietly talks to a forgotten scratch db (left over from a
	// prior host migration, a stale CI runner, or a permissive
	// `mpm init --allow-empty`).
	//
	// Set MPM_REQUIRED_DB_PATH to a canonical absolute path to activate.
	// When unset the check is skipped (so ad-hoc `mpm-mcp` calls from
	// the test harness still work); setting it to ""+non-empty-string
	// disables the gate explicitly. Compare on EvalSymlinks-resolved
	// paths so symlink-aliasing (e.g. canonical → project-source)
	// doesn't false-positive.
	if required := os.Getenv("MPM_REQUIRED_DB_PATH"); required != "" {
		got, herr := filepath.EvalSymlinks(dm.DBPath())
		if herr != nil {
			log.Fatalf("mpm-mcp: resolve live db_path: %v", herr)
		}
		want, herr := filepath.EvalSymlinks(required)
		if herr != nil {
			log.Fatalf("mpm-mcp: resolve MPM_REQUIRED_DB_PATH: %v", herr)
		}
		if got != want {
			log.Fatalf(
				"mpm-mcp: DB path invariant violated — refusing to boot.\n"+
					"  expected: %s\n"+
					"  actual:   %s\n"+
					"This usually means two mpm installs on the same host, or a\n"+
					"  stale scratch db that pre-dates the canonical mpm install.\n"+
					"Either set MPM_REQUIRED_DB_PATH correctly, unset it for ad-hoc\n"+
					"  boots, or run `mpm status` to see which workspace is current.",
				want, got,
			)
		}
		slog.Info("mpm-mcp: DB path invariant satisfied", "db_path", got)
	}

	ac := mpmcli.ActiveContextFromEnv()

	// D-013: bootstrap mode/ and persona/ directories if absent.
	// The CLI tolerates a fresh workspace (mode/ is optional for CLI
	// commands), but mpm-mcp used to fail silently when either
	// directory was missing because NewRouter's loadComponents helper
	// surfaces os.ReadDir errors verbatim. Bootstrap empty dirs so a
	// fresh workspace doesn't produce an opaque fatal on first MCP
	// boot — operators can drop .md files in later to activate modes.
	for _, subdir := range []string{"mode", "persona"} {
		dir := filepath.Join(workspace, subdir)
		if err := os.MkdirAll(dir, 0o700); err != nil {
			log.Fatalf("mpm-mcp: bootstrap %s/: %v", subdir, err)
		}
	}

	// Build the router once — pre-compiles all mode/persona patterns at boot.
	router, err := internal.NewRouter(workspace)
	if err != nil {
		log.Fatalf("mpm-mcp: build router: %v", err)
	}

	// Build blob store.
	blobDir := filepath.Join(workspace, "blobs")
	ttl := 24 * time.Hour
	if envTTL := os.Getenv("MPM_BLOB_TTL"); envTTL != "" {
		if d, derr := time.ParseDuration(envTTL); derr == nil && d > 0 {
			ttl = d
		}
	}
	blobStore, err := blobstore.NewFilesystemBackend(dm.SQLDB(), blobDir, ttl)
	if err != nil {
		log.Fatalf("mpm-mcp: build blob store: %v", err)
	}

	// Output policy for MCP result bounding.
	outputPolicy := tools.DefaultOutputPolicy()

	s := server.NewMCPServer("mpm-mcp", "0.1.0",
		// Defense-in-depth: a short protocol summary in the initialize
		// response. Hosts that read the .instructions field (per the
		// MCP spec wording "Optional instructions for the client")
		// will surface it to the model as a behavioural primer.
		//
		// The full MPM behavioural contract lives in the host's
		// managed instruction file (agent_installation/
		// MPM_AGENT_INTEGRATION_SNIPPETS.md) because that delivery
		// channel is portable and provably-reachable on every
		// supported host. This instructions string is intentionally
		// short — a primer, not a substitute — and it's free to
		// include because no host is required to surface it.
		//
		// Per the onboarding-mcp-native audit
		// (docs/onboarding-mcp-native-audit-2026-09-05.md Part A),
		// at least Hermes ignores .instructions at the client side
		// (verified at /home/v/.hermes/hermes-agent/tools/mcp_tool.py:
		// only .capabilities is read from initialize_result); other
		// hosts are unknown from source/docs. So this is additive /
		// inert on every host we know about, and a small bonus if
		// any future host does surface it.
		server.WithInstructions(
			"MPM behavioural primer — full contract lives in the host's MPM-managed instruction file if installed, otherwise in $HOME/.mpm/agent_installation/mpm-agent-protocol.md. "+
				"Wake on session start: call mpm_context read_wake_context. "+
				"Persist during work (not only at the end): use mpm_memory save, mpm_decisions record, mpm_lessons save, mpm_topics create, mpm_references add. "+
				"Handoff before session closure (mpm_handoff write; summary is the only required field; mid-session acks like ok/thanks/ty/ack are NOT session-closing): "+
				"do not write a handoff in response to a chat ack. "+
				"Session closure does NOT auto-complete work items — call mpm_work action=complete with the work_id explicitly when work is done. "+
				"If MCP transport fails, fall back to `mpm call <tool> --payload '{\"action\":\"<op>\",\"params\":{...}}'` — same substrate, same provenance.",
		),
	)
	RegisterAllTools(s, dm, ac, router, blobStore, outputPolicy)

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

