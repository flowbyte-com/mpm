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
	"syscall"

	"mpm/internal/critic"
)

func main() {
	var (
		dbPath = flag.String("db", os.Getenv("MPM_DB_PATH"),
			"Path to mpm.db (default: $MPM_DB_PATH or src/db/mpm.db)")
		logLevel = flag.String("log-level", "info",
			"Log level: debug, info, warn, error")
	)
	flag.Parse()

	if *dbPath == "" {
		*dbPath = "src/db/mpm.db"
	}

	logger := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{
		Level: parseLevel(*logLevel),
	}))

	ctx, cancel := signal.NotifyContext(context.Background(),
		syscall.SIGINT, syscall.SIGTERM)
	defer cancel()

	a, err := critic.New(*dbPath, logger)
	if err != nil {
		logger.Error("critic init failed", "db", *dbPath, "err", err)
		os.Exit(1)
	}
	defer func() {
		if err := a.Close(); err != nil {
			logger.Error("critic close failed", "err", err)
		}
	}()

	logger.Info("mpm-critic starting", "db", *dbPath)
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
