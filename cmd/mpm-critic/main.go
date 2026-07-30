// mpm-critic: standalone runner for one Critic audit cycle.
//
// Invoked by mpm-scheduler as a payload handler (kind=critic_audit).
// Opens the MPM database, runs the audit cycle (3 hunts + poison pill
// on cycle % 5), emits findings via mpm call, then exits.
//
// Kept as a separate binary so the scheduler stays a thin dispatcher.
// The critic's audit logic evolves independently of the scheduler.

package main

import (
	"context"
	"flag"
	"log/slog"
	"os"
	"os/signal"
	"path/filepath"
	"syscall"

	mpmcore "github.com/flowbyte-com/mpm-core"

	"github.com/flowbyte-com/mpm/internal/critic"
)

func main() {
	var (
		dbPath = flag.String("db", os.Getenv("MPM_DB_PATH"),
			"Path to mpm.db (default: $MPM_DB_PATH or src/db/mpm.db via DatabaseManager)")
		logLevel = flag.String("log-level", "info",
			"Log level: debug, info, warn, error")
	)
	flag.Parse()

	logger := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{
		Level: parseLevel(*logLevel),
	}))

	ctx, cancel := signal.NotifyContext(context.Background(),
		syscall.SIGINT, syscall.SIGTERM)
	defer cancel()

	// Open the MPM database via the canonical DatabaseManager (F-007).
	projectRoot := "."
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

	a, err := critic.New(dm.SQLDB(), logger)
	if err != nil {
		logger.Error("critic init failed", "err", err)
		os.Exit(1)
	}
	defer func() {
		if err := a.Close(); err != nil {
			logger.Error("critic close failed", "err", err)
		}
	}()

	logger.Info("mpm-critic starting")
	if err := a.Run(ctx); err != nil {
		logger.Error("critic run failed", "err", err)
		os.Exit(1)
	}
	logger.Info("mpm-critic complete")
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
