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
	_ "embed"
	"errors"
	"io"
	"log"
	"log/slog"
	"os"
	"os/signal"
	"path/filepath"
	"syscall"
	"time"

	"github.com/mark3labs/mcp-go/mcp"
	"github.com/mark3labs/mcp-go/server"

	"github.com/flowbyte-com/mpm/internal/blobstore"
	"github.com/flowbyte-com/mpm-core/tools"
	"github.com/flowbyte-com/mpm-core"
	"github.com/flowbyte-com/mpm-core/config"
	"github.com/flowbyte-com/mpm-core/logging"
	"github.com/flowbyte-com/mpm-core/mpmcli"
)

// defaultCoreTools is the canonical initial MCP surface for MPM. Tools
// outside this set are filtered out of tools/list AND tools/call by
// the server.WithToolFilter option below; they remain registered in
// the substrate (so `mpm call <tool>` continues to work) but are not
// initially exposed to the model. The mpm_help discovery tool lets an
// agent reach specialists by name via the host shell. The full set is
// restored by setting MPM_EXPOSE_ALL_TOOLS=1.
//
// 3-tool surface (post Sept-2026 launch polish):
//   mpm_memory   persist/recall/show/shred (and reinforce/weaken/snooze/patch/promote)
//   mpm_context  wake/directives/route/handoff (write/read) — absorbs mpm_handoff
//   mpm_help     capability discovery (lists all 22 tools + per-tool reach_via_cli)
//
// Specialists reachable via mpm_help + mpm call:
//   mpm_work, mpm_theories, mpm_decisions, mpm_lessons, mpm_topics,
//   mpm_references, mpm_evidence, mpm_confidence, mpm_skills, mpm_wakes,
//   mpm_scratchpad, mpm_resolve, mpm_blob_read, mpm_blob_search,
//   mpm_retrieval_diagnose, log_to_changelog, request_review,
//   mpm_system, mpm_handoff (standalone access — preferred path is
//   through mpm_context action=write_handoff/read_handoff)
//
//  wake        → mpm_context   (read_wake_context, read_directives)
//  persist     → mpm_memory    (save, query, show)
//  skills      → mpm_skills    (list, read) + mpm_help (discovery)
//  handoff     → mpm_handoff   (write, read)
//  closure     → mpm_work      (NOT initial — discoverable)
//  source-of-truth → mpm_memory (already listed)
//  recovery    → mpm_help      (lists mpm call escape hatch)
//
// 3-tool initial surface — see the docstring above. Handoff is
// reachable via mpm_context action=write_handoff/read_handoff; the
// standalone mpm_handoff tool remains in the substrate for CLI /
// direct access but is not in the initial surface.
var defaultCoreTools = map[string]bool{
	"mpm_memory":  true,
	"mpm_context": true,
	"mpm_help":    true,
}

// coreToolFilter is the actual filter function passed to
// server.WithToolFilter. It honors the MPM_EXPOSE_ALL_TOOLS escape
// hatch for hosts that need the legacy full surface. The filter is
// applied at both tools/list and tools/call time (mcp-go enforces
// the latter); a tool that fails the filter is invisible AND
// uncallable through the MCP surface.
func coreToolFilter(_ context.Context, registered []mcp.Tool) []mcp.Tool {
	if os.Getenv("MPM_EXPOSE_ALL_TOOLS") != "" {
		return registered
	}
	out := make([]mcp.Tool, 0, len(defaultCoreTools))
	for _, t := range registered {
		if defaultCoreTools[t.Name] {
			out = append(out, t)
		}
	}
	return out
}

// instructionsPrimer is the value mpm-mcp returns in the
// `initialize.instructions` field. The file is generated from the
// canonical managed block by
// `agent_installation/scripts/render_managed_blocks.py --dump
// instructions` and committed alongside this source so the embedding
// is deterministic across builds (no path resolution at runtime).
//
// Drift detection is two-sided:
//
//   - `render_managed_blocks.py --check` byte-compares this file
//     against the renderer's output and fails if the canonical block
//     has drifted away from the embedded primer.
//   - `instructions_primer_drift_test.go` invokes the renderer at Go
//     test time and asserts the rendered string equals this constant;
//     that catches the inverse drift (someone hand-edits this file
//     and forgets to re-run the renderer).
//
// The primer is a fallback-aware pointer, not a restatement of the
// managed block's contract: on hosts that auto-inject the field AND
// also maintain a managed instruction file (Claude Code, OpenCode) it
// is one short paragraph; on hosts that surface it via an explicit
// call (Pi via pi-mcp-adapter) it is a minimal behavioural skeleton;
// on hosts that ignore the field (Hermes) it is inert.
//
//go:embed instructions_primer.txt
var instructionsPrimer string

// router is initialised once at server boot — patterns and anti-patterns
// from all mode/*.md and persona/*.md files are compiled to regex at that
// point, so Evaluate() is pure string matching with zero parsing overhead.

func main() {
	// 2026-09-18 hardening pass: process umask 0077 BEFORE any DB
	// open, log setup, or tool registration that may write to disk.
	config.EnforcePrivateUmask()

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
		// Defense-in-depth: ship a short behavioural primer in the
		// initialize response. Hosts that read .instructions will
		// surface it to the model; hosts that don't (Hermes,
		// confirmed by source inspection) treat it as inert. The
		// primer text is embedded from instructions_primer.txt
		// (see the top of this file for the drift-detection
		// contract and the audit citation
		// docs/onboarding-mcp-native-audit-2026-09-05.md Part A).
		server.WithInstructions(instructionsPrimer),
		// Initial-surface filter: expose only the default core set at
		// tools/list time. Specialists are reachable via the
		// mpm_help discovery tool + `mpm call <tool>` escape hatch.
		// MPM_EXPOSE_ALL_TOOLS=1 reverts to the legacy full surface.
		// See docs/CONTEXT_EXPOSURE.md for the architecture.
		server.WithToolFilter(coreToolFilter),
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

