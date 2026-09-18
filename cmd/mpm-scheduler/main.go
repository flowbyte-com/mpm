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
	"log/slog"
	"os"
	"os/signal"
	"path/filepath"
	"syscall"
	"time"

	mpmcore "github.com/flowbyte-com/mpm-core"
	mpmcore_config "github.com/flowbyte-com/mpm-core/config"

	"github.com/flowbyte-com/mpm-core/mpmcli"
	"github.com/flowbyte-com/mpm/internal/scheduler"
)

func main() {
	// 2026-09-18 hardening pass: process umask 0077 BEFORE any DB
	// open or file creation. See internal/core/config/umask.go.
	mpmcore_config.EnforcePrivateUmask()

	var (
		interval = flag.Duration("interval", 60*time.Second,
			"System-maintenance cadence (default 60s). Bounds the cross-process wake-dispatch latency to min(interval, time_to_next_deadline). Min 1s.")
		dbPath = flag.String("db", os.Getenv("MPM_DB_PATH"),
			"Path to mpm.db (default: $MPM_DB_PATH or src/db/mpm.db via DatabaseManager)")
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

	// Security gate: auto-heal or refuse to start if the runtime dir
	// perms are wider than 0700. Runs before any DB connection.
	if err := mpmcore_config.AssertUserDirPerms0700(mpmcore_config.GetMPMDir()); err != nil {
		logger.Error("security gate failed", "err", err)
		os.Exit(1)
	}

	// File-perms sweep: tighten existing data-plane files to 0600.
	// Non-fatal — failures are logged via the configured slog handler.
	if _, err := mpmcore_config.TightenFilePerms0600(mpmcore_config.GetMPMDir()); err != nil {
		logger.Warn("file perms sweep failed to start", "err", err)
	}

	// Singleton enforcement. Two scheduler instances must not race on the
	// same wake batch — flock prevents that at the kernel level.
	lockFile, err := scheduler.AcquireLock(*lockPath)
	if err != nil {
		logger.Error("lock acquire failed", "path", *lockPath, "err", err)
		os.Exit(1)
	}
	defer func() {
		if err := lockFile.Close(); err != nil {
			logger.Error("lock release failed", "err", err)
		}
	}()
	logger.Info("singleton lock acquired", "path", *lockPath)

	// Open the MPM database via the canonical DatabaseManager. F-007
	// fix: the scheduler does NOT own the *sql.DB handle. Construction
	// site enforces the singleton invariant; sqlopen_owner_test will
	// fail if any future code introduces a separate handle.
	//
	// D-001 (alpha-4.1.1): honor MPM_WORKSPACE. Without this, a
	// scheduler launched against a disposable workspace would
	// silently target the production DB — exactly the failure mode
	// the canonical mpmcli.ResolveWorkspace helper exists to prevent.
	// Explicit -db still wins for operator overrides.
	projectRoot := mpmcli.ResolveWorkspace()
	if *dbPath != "" {
		projectRoot = filepath.Dir(filepath.Dir(filepath.Dir(*dbPath)))
	}
	dm, err := mpmcore.NewDatabaseManager(projectRoot)
	if err != nil {
		logger.Error("database open failed", "project_root", projectRoot, "err", err)
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
	s.Register("critic_audit", scheduler.CriticAuditHandler)
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
	// cascade_wake_reconcile → per-tick recovery pass for wake
	// bookings lost to crash windows. Reads rows where status=
	// 'materialized' AND wake_scheduled=0 and re-schedules the wake.
	// Audit H-3 (post-M3, 2026-08-31). The handler is idempotent
	// (LIMIT 100, flag-flip semantics) so re-running is safe.
	s.RegisterTickHandler("cascade_wake_reconcile",
		scheduler.NewCascadeWakeReconcileHandler(dm, logger).TickHandler())

	ctx, cancel := signal.NotifyContext(context.Background(),
		syscall.SIGINT, syscall.SIGTERM)
	defer cancel()

	logger.Info("mpm-scheduler starting",
		"interval", interval.String(),
		"db", *dbPath,
		"pid", os.Getpid())

	if err := s.Run(ctx, *interval); err != nil {
		logger.Error("scheduler run failed", "err", err)
		os.Exit(1)
	}
	logger.Info("mpm-scheduler stopped cleanly")
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