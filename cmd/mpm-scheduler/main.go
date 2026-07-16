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

	"mpm/internal/scheduler"
)

func main() {
	var (
		interval = flag.Duration("interval", 60*time.Second,
			"Ticker interval for wake polling (min 1s)")
		dbPath = flag.String("db", os.Getenv("MPM_DB_PATH"),
			"Path to mpm.db (default: $MPM_DB_PATH or src/db/mpm.db via DatabaseManager)")
		lockPath = flag.String("lock", defaultLockPath(),
			"flock path for singleton enforcement (default ~/.mpm/scheduler.lock)")
		logLevel = flag.String("log-level", "info",
			"Log level: debug, info, warn, error")
	)
	flag.Parse()

	logger := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{
		Level: parseLevel(*logLevel),
	}))

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
	projectRoot := ""
	if *dbPath != "" {
		projectRoot = filepath.Dir(filepath.Dir(filepath.Dir(*dbPath)))
	} else {
		projectRoot = "."
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
	// (anything else)  → notification kind, passes through to opportunistic fold
	s.Register("snapshot", scheduler.SnapshotHandler)
	s.Register("critic_audit", scheduler.CriticAuditHandler)
	s.Register("gc", scheduler.GCHandler)
	s.Register("broadcast", scheduler.BroadcastHandler)

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
	home, err := os.UserHomeDir()
	if err != nil {
		return "/tmp/mpm-scheduler.lock"
	}
	return filepath.Join(home, ".mpm", "scheduler.lock")
}