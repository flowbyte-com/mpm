// mpm-critic: standalone runner for one Critic audit cycle.
//
// Invoked by mpm-scheduler as a payload handler (kind=critic_audit).
// Opens the MPM database, runs the audit cycle (3 hunts + poison pill
// on cycle % 5), emits findings via mpm call, then exits.
//
// With --telemetry-hunt, also runs the HighTokenNoArtifactHunt from
// telemetry store against the same audit cycle. This integrates the
// telemetry pipeline into the critic's daily cadence so findings land
// in mpm_lessons automatically rather than requiring a separate cron.
//
// Kept as a separate binary so the scheduler stays a thin dispatcher.
// The critic's audit logic evolves independently of the scheduler.

package main

import (
	"bytes"
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"log/slog"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	mpmcore "github.com/flowbyte-com/mpm-core"

	"github.com/flowbyte-com/mpm-core/config"

	"github.com/flowbyte-com/mpm-core/mpmcli"
	"github.com/flowbyte-com/mpm/internal/critic"
	"github.com/flowbyte-com/mpm/internal/telemetry"
)

func main() {
	// 2026-09-18 hardening pass: process umask 0077 BEFORE any DB
	// open or file creation.
	config.EnforcePrivateUmask()

	var (
		dbPath = flag.String("db", os.Getenv("MPM_DB_PATH"),
			"Path to mpm.db (default: $MPM_DB_PATH or src/db/mpm.db via DatabaseManager)")
		logLevel = flag.String("log-level", "info",
			"Log level: debug, info, warn, error")
		telemetryHunt = flag.Bool("telemetry-hunt", false,
			"Also run the HighTokenNoArtifactHunt from telemetry store")
	)
	flag.Parse()

	logger := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{
		Level: parseLevel(*logLevel),
	}))

	ctx, cancel := signal.NotifyContext(context.Background(),
		syscall.SIGINT, syscall.SIGTERM)
	defer cancel()

	// Open the MPM database via the canonical DatabaseManager (F-007).
	// D-001 (alpha-4.1.1): honor MPM_WORKSPACE so a disposable test
	// workspace cannot accidentally target production. Explicit -db
	// still wins — an explicit DB path overrides MPM_WORKSPACE so
	// operator overrides (e.g. running the critic against a recovered
	// DB at a non-standard location) keep working.
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

	// Run critic hunts first (SurvivalAsymmetry, StaleMemory, WeakTheory, PoisonPill).
	logger.Info("mpm-critic starting")
	if err := a.Run(ctx); err != nil {
		logger.Error("critic run failed", "err", err)
		os.Exit(1)
	}
	logger.Info("mpm-critic critic hunts complete")

	// Optionally run the telemetry HighTokenNoArtifactHunt in the same cycle.
	if *telemetryHunt {
		if err := runTelemetryHunt(ctx, logger, projectRoot, a.Cycle()); err != nil {
			logger.Error("telemetry hunt failed", "err", err)
			os.Exit(1)
		}
		logger.Info("mpm-critic telemetry hunt complete")
	}

	logger.Info("mpm-critic complete")
}

// runTelemetryHunt opens the telemetry store and runs the HighTokenNoArtifactHunt
// within the same audit cycle as the critic hunts. Findings are emitted as
// mpm_lessons (type=observation) via mpm call, consistent with how the
// critic emits its own findings.
func runTelemetryHunt(ctx context.Context, log *slog.Logger, projectRoot string, cycle int) error {
	telemetryDB := filepath.Join(projectRoot, "telemetry.db")
	store, err := telemetry.Open(telemetryDB)
	if err != nil {
		return fmt.Errorf("open telemetry store: %w", err)
	}
	defer store.Close()

	// Artifact count: shell out to mpm call mpm_provenance (cross-DB join).
	countFn := func(ctx context.Context, sessionID string) (int, error) {
		cmd := exec.CommandContext(ctx, "mpm", "call", "mpm_provenance",
			"--payload", fmt.Sprintf(`{"action":"count_by_session","params":{"session_id":"%s"}}`, sessionID))
		cmd.Env = append(os.Environ(), "MPM_WORKSPACE="+projectRoot)
		out, err := cmd.Output()
		if err != nil {
			return 0, fmt.Errorf("mpm_provenance: %w", err)
		}
		// Parse {"count": N} from stdout.
		out = bytes.TrimSpace(out)
		if len(out) == 0 {
			return 0, nil
		}
		// Find "count" field.
		var parsed struct {
			Count int `json:"count"`
		}
		if err := json.Unmarshal(out, &parsed); err != nil {
			return 0, fmt.Errorf("parse mpm_provenance output: %w", err)
		}
		return parsed.Count, nil
	}

	// Lesson save: shell out to mpm call mpm_lessons.
	lessonFn := func(ctx context.Context, payload map[string]any) error {
		b, err := json.Marshal(payload)
		if err != nil {
			return err
		}
		cmd := exec.CommandContext(ctx, "mpm", "call", "mpm_lessons", "--payload", string(b))
		cmd.Env = append(os.Environ(), "MPM_WORKSPACE="+projectRoot)
		out, err := cmd.Output()
		if err != nil {
			return fmt.Errorf("mpm_lessons: %s: %w", strings.TrimSpace(string(out)), err)
		}
		return nil
	}

	sevenDaysAgo := telemetry.HuntConfig{
		HighTokenThreshold: 100000,
		MinInvocations:     1,
		Since:              time.Now().Add(-7 * 24 * time.Hour).Unix(), // 7-day lookback
	}

	findings, err := telemetry.Hunt(ctx, store, sevenDaysAgo, countFn)
	if err != nil {
		return fmt.Errorf("telemetry Hunt: %w", err)
	}

	// The telemetry hunt runs in the same process immediately after the
	// critic hunts, so it genuinely belongs to the cycle that was just
	// claimed from durable state. Tagging it with that cycle makes the
	// telemetry findings correlatable with the critic's own cycle_N
	// tags. It is NOT wall-clock derived and never was - the previous
	// hardcoded 0 made every telemetry lesson share one bogus tag.
	cycleTag := fmt.Sprintf("critic_cycle_%d", cycle)
	for _, f := range findings {
		payload := telemetry.FindingLessonPayload(f, cycleTag)
		if err := lessonFn(ctx, payload); err != nil {
			log.Error("telemetry lesson save failed", "session", f.SessionID, "err", err)
			continue
		}
		log.Info("telemetry finding saved", "session", f.SessionID, "tokens", f.TotalInputTokens+f.TotalOutputTokens)
	}
	return nil
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
