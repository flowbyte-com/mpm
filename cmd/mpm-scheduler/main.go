// mpm-scheduler: universal wake executor for MPM.
//
// Reads scheduled_wakes on a configurable interval (default 60s), routes
// each due wake through a HandlerFunc registered at startup. Replaces
// the OS-level cron entry plus any bespoke per-task daemons.
//
// Adding a new system wake kind: write a HandlerFunc, register it in
// main() below. That's it. Central dispatch never needs editing.
//
// Architecture: see internal/scheduler/scheduler.go doc + decision
// 27d7b3c18199e098.

package main

import (
	"context"
	"flag"
	"fmt"
	"log/slog"
	"os"
	"os/signal"
	"path/filepath"
	"syscall"
	"time"

	mpmcore "github.com/flowbyte-com/mpm-core"
	mpmcore_config "github.com/flowbyte-com/mpm-core/config"

	"github.com/flowbyte-com/mpm/internal/buildinfo"
	"github.com/flowbyte-com/mpm/internal/scheduler"
)

func main() {
	// The version surface is answered FIRST, before the umask sweep,
	// the flag parsing, the permission gate, the singleton lock and
	// the database open. Asking the daemon what it is must never
	// start it.
	if buildinfo.ArgvRequestsVersion(os.Args[1:]) {
		fmt.Print(buildinfo.FormatHuman(buildinfo.Current("mpm-scheduler")))
		return
	}

	// 2026-09-18 hardening pass: process umask 0077 BEFORE any DB
	// open or file creation. See internal/core/config/umask.go.
	mpmcore_config.EnforcePrivateUmask()

	var (
		interval = flag.Duration("interval", 60*time.Second,
			"System-maintenance cadence (default 60s). Bounds the cross-process wake-dispatch latency to min(interval, time_to_next_deadline). Min 1s.")
		dbPath = flag.String("db", "",
			"Path to mpm.db. It must have the canonical <workspace>/src/db/mpm.db shape; "+
				"the workspace is derived from it, and a path without that shape is "+
				"refused rather than silently rewritten into another database. "+
				"Default: $MPM_WORKSPACE/src/db/mpm.db")
		lockPath = flag.String("lock", defaultLockPath(),
			"flock path for singleton enforcement (default $MPM_WORKSPACE/scheduler.lock)")
		logLevel = flag.String("log-level", "info",
			"Log level: debug, info, warn, error")
		heartbeat = flag.Uint64("heartbeat", 100,
			"Emit a heartbeat log line every N ticks (0 = disabled). "+
				"At default 60s interval, 100 ticks = ~100 min. "+
				"Surfaces daemon liveness without journal grep gymnastics.")
	)
	flag.Parse()

	logger := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{
		Level: parseLevel(*logLevel),
	}))

	// D4 (2026-10-28): resolve the workspace and the database BEFORE
	// anything touches the filesystem.
	//
	// Every path below hangs off the workspace — the perms gate, the
	// singleton lock, the database itself — so they must all agree on
	// which workspace this process is serving. The old code derived the
	// workspace from an explicit --db with Dir(Dir(Dir(path))), which
	// ASSUMED the path had the canonical <root>/src/db/mpm.db shape and
	// never checked it: `--db /tmp/other.db` silently became "open
	// /src/db/mpm.db", a different database, with nothing said at
	// startup or ever afterwards.
	//
	// config.ResolveTarget is THE resolver: one place, one set of rules.
	// A path without the canonical shape is refused, and an MPM_DB_PATH
	// that disagrees with the workspace is a conflict, not a selection.
	workspace, resolvedDB, err := mpmcore_config.ResolveTarget(*dbPath)
	if err != nil {
		logger.Error("database path resolution failed", "err", err)
		os.Exit(1)
	}

	// Security gate: auto-heal or refuse to start if the runtime dir
	// perms are wider than 0700. Runs before any DB connection.
	if err := mpmcore_config.AssertUserDirPerms0700(workspace); err != nil {
		logger.Error("security gate failed", "err", err)
		os.Exit(1)
	}

	// File-perms sweep: tighten existing data-plane files to 0600.
	// Non-fatal — failures are logged via the configured slog handler.
	if _, err := mpmcore_config.TightenFilePerms0600(workspace); err != nil {
		logger.Warn("file perms sweep failed to start", "err", err)
	}

	// Singleton enforcement. Two scheduler instances must not race on the
	// same wake batch — flock prevents that at the kernel level.
	//
	// The lock must live with the workspace it protects. The --lock
	// default is resolved from MPM_WORKSPACE, but an explicit --db can
	// select a different workspace; when it does and the operator named
	// no lock, the lock follows the RESOLVED workspace. Otherwise a
	// scheduler pointed at a disposable workspace would take the live
	// installation's singleton lock — and the live daemon would refuse
	// to start beside it.
	lockFilePath := *lockPath
	if *dbPath != "" && !flagWasSet("lock") {
		lockFilePath = filepath.Join(workspace, "scheduler.lock")
	}
	lockFile, err := scheduler.AcquireLock(lockFilePath)
	if err != nil {
		logger.Error("lock acquire failed", "path", lockFilePath, "err", err)
		os.Exit(1)
	}
	defer func() {
		if err := lockFile.Close(); err != nil {
			logger.Error("lock release failed", "err", err)
		}
	}()
	logger.Info("singleton lock acquired", "path", lockFilePath)

	// Open the MPM database via the canonical DatabaseManager. F-007
	// fix: the scheduler does NOT own the *sql.DB handle. Construction
	// site enforces the singleton invariant; sqlopen_owner_test will
	// fail if any future code introduces a separate handle.
	//
	// The workspace was resolved above by config.ResolveTarget, so the
	// database this opens is exactly the one that was resolved — not a
	// re-derivation of it.
	dm, err := mpmcore.NewDatabaseManager(workspace)
	if err != nil {
		logger.Error("database open failed", "project_root", workspace, "err", err)
		os.Exit(1)
	}
	defer func() {
		if err := dm.Close(); err != nil {
			logger.Error("database close failed", "err", err)
		}
	}()

	s, err := scheduler.New(dm.SQLDB(), logger)
	if err != nil {
		logger.Error("scheduler init failed", "err", err)
		os.Exit(1)
	}
	s.SetHeartbeat(*heartbeat)
	logger.Info("scheduler heartbeat configured", "every_n_ticks", *heartbeat)
	defer func() {
		if err := s.Close(); err != nil {
			logger.Error("scheduler close failed", "err", err)
		}
	}()

	// ── Handler registry ────────────────────────────────────────────────
	//
	// Adding a new system kind = register one line here. No central
	// dispatch edit, no recompile of unrelated code paths.
	//
	// snapshot         → atomic SQLite .backup (replaces cron + shell script)
	// critic_audit     → Critic audit cycle (lands once internal/critic ships)
	// gc               → MPM garbage collection sweep
	// broadcast        → active dissemination fan-out to receiving agents
	// cascade_drain    → per-tick cascade outbox drain (30s budget)
	// cascade_summary  → forensic log line for each non-steady-state drain tick
	// drill            → behavioural drill execution (synthetic engine
	//                    self-test or real-framework harness like Claude
	//                    Code). Dispatches by spec.Framework field;
	//                    unknown frameworks surface as status='error'
	//                    rows in drill_runs, never as silent failures.
	// openclaw_ingest  → per-tick file watcher at /home/v/.mpm/run/ingest.md.
	//                    Drains the mpm-memory-openclaw plugin's
	//                    memory-flush output into scheduled_wakes (kind
	//                    ephemeral_compaction_ready). Lives behind
	//                    safety rails (path allowlist, symlink refuse,
	//                    64KB cap, *.rejected quarantine, atomic
	//                    rename, source attribution, world-readable
	//                    refuse) — see internal/scheduler/ingest.go.
	// (anything else)  → notification kind, passes through to opportunistic fold
	s.Register("snapshot", scheduler.SnapshotHandler)
	// The critic-audit handler is bound to the RESOLVED workspace and
	// database, so the critic audits the same substrate this scheduler
	// dispatches against — even when --db selected a workspace other
	// than the environment's.
	s.Register("critic_audit", scheduler.NewCriticAuditHandler(workspace, resolvedDB))
	s.Register("gc", scheduler.GCHandler)
	s.Register("broadcast", scheduler.BroadcastHandler)
	s.Register("cascade_summary", scheduler.NewCascadeSummaryHandler(logger))
	s.Register("drill", scheduler.DrillHandler)
	s.RegisterTickHandler("cascade_drain", scheduler.NewCascadeDrainHandler(
		dm,
		logger,
		scheduler.CascadeDrainOptions{
			Budget:    30 * time.Second,
			BatchSize: 10,
		},
	).TickHandler())
	s.RegisterTickHandler("openclaw_ingest", scheduler.NewIngestHandler(dm, logger).TickHandler())
	// confidence_decay → bounded decay recompute for memories-backed
	//                    artifacts (cadence-gated; see the handler).
	// cascade_wake_reconcile → per-tick recovery pass for wake
	// bookings lost to crash windows. Reads rows where status=
	// 'materialized' AND wake_scheduled=0 and re-schedules the wake.
	// Audit H-3 (post-M3, 2026-08-31). The handler is idempotent
	// (LIMIT 100, flag-flip semantics) so re-running is safe.
	s.RegisterTickHandler("cascade_wake_reconcile",
		scheduler.NewCascadeWakeReconcileHandler(dm, logger).TickHandler())
	// confidence_decay → bounded time-based confidence recompute. The
	// substrate models decay; without this, a stored confidence is a
	// snapshot that never moves until an unrelated evidence write
	// happens to touch the artifact. Cadence and batch size are
	// configurable (confidence.decay.*) and the pass is safe to re-run.
	s.RegisterTickHandler("confidence_decay",
		scheduler.NewConfidenceDecayHandler(dm, logger).TickHandler())

	ctx, cancel := signal.NotifyContext(context.Background(),
		syscall.SIGINT, syscall.SIGTERM)
	defer cancel()

	logger.Info("mpm-scheduler starting",
		"interval", interval.String(),
		"workspace", workspace,
		"db", resolvedDB,
		"pid", os.Getpid())

	if err := s.Run(ctx, *interval); err != nil {
		logger.Error("scheduler run failed", "err", err)
		os.Exit(1)
	}
	logger.Info("mpm-scheduler stopped cleanly")
}

// flagWasSet reports whether the operator named the flag explicitly on
// the command line.
//
// It matters for the singleton lock: the flag's DEFAULT is a path, so
// "the operator named it" and "the default was used" are different
// situations, and only the first one may override the default.
func flagWasSet(name string) bool {
	set := false
	flag.Visit(func(f *flag.Flag) {
		if f.Name == name {
			set = true
		}
	})
	return set
}

func parseLevel(s string) slog.Level {
	switch s {
	case "debug":
		return slog.LevelDebug
	case "warn":
		return slog.LevelWarn
	case "error":
		return slog.LevelError
	default:
		return slog.LevelInfo
	}
}

func defaultLockPath() string {
	if env := os.Getenv("MPM_SCHEDULER_LOCK"); env != "" {
		return env
	}
	// Legacy fallback (HOME/.mpm/scheduler.lock) was removed with the
	// migration to system-level paths. /var/lib/mpm is the canonical
	// runtime location; /tmp is the ad-hoc fallback for manual runs.
	if ws := os.Getenv("MPM_WORKSPACE"); ws != "" {
		return filepath.Join(ws, "scheduler.lock")
	}
	return "/tmp/mpm-scheduler.lock"
}
