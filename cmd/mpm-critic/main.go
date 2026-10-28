// mpm-critic: standalone runner for one Critic audit cycle.
//
// Invoked by mpm-scheduler as a payload handler (kind=critic_audit).
// Opens the MPM database, runs the audit cycle (4 hunts + poison pill
// on cycle % 5), emits findings via mpm call, then exits.
//
// With --telemetry-hunt, also runs the HighTokenNoArtifactHunt from
// the telemetry store against the same audit cycle. That hunt is a
// READ-ONLY DIAGNOSTIC: it computes the observation and REPORTS it,
// and it writes nothing to the substrate. See runTelemetryHunt for
// the adjudication that made it read-only.
//
// Kept as a separate binary so the scheduler stays a thin dispatcher.
// The critic's audit logic evolves independently of the scheduler.

package main

import (
	"context"
	"database/sql"
	"errors"
	"flag"
	"fmt"
	"log/slog"
	"os"
	"os/signal"
	"syscall"
	"time"

	mpmcore "github.com/flowbyte-com/mpm-core"

	"github.com/flowbyte-com/mpm-core/config"

	"github.com/flowbyte-com/mpm/internal/buildinfo"
	"github.com/flowbyte-com/mpm/internal/critic"
	"github.com/flowbyte-com/mpm/internal/telemetry"
)

func main() {
	// The version surface is answered FIRST, before the umask sweep,
	// the flag parsing and the database open. Asking the critic what
	// it is must never run a critique.
	if buildinfo.ArgvRequestsVersion(os.Args[1:]) {
		fmt.Print(buildinfo.FormatHuman(buildinfo.Current("mpm-critic")))
		return
	}

	// 2026-09-18 hardening pass: process umask 0077 BEFORE any DB
	// open or file creation.
	config.EnforcePrivateUmask()

	var (
		dbPath = flag.String("db", "",
			"Path to mpm.db. It must have the canonical <workspace>/src/db/mpm.db shape; "+
				"the workspace is derived from it, and a path without that shape is "+
				"refused rather than silently rewritten into another database. "+
				"Default: $MPM_WORKSPACE/src/db/mpm.db")
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
	//
	// D-001 (alpha-4.1.1): honor MPM_WORKSPACE so a disposable test
	// workspace cannot accidentally target production.
	//
	// D4 (2026-10-28): the workspace and the database come from ONE
	// resolver — config.ResolveTarget. The old code derived the
	// workspace from an explicit -db with Dir(Dir(Dir(path))), which
	// ASSUMED the path had the canonical <root>/src/db/mpm.db shape
	// and never checked it: `-db /tmp/other.db` silently became "open
	// /src/db/mpm.db" — a different database, with nothing said at
	// startup or ever afterwards.
	//
	// The operator's documented way to point the critic at a recovered
	// database outside the canonical layout is a canonical workspace
	// whose src/db/mpm.db is a symlink to it; the resolver recognises
	// that by inode and accepts it.
	workspace, resolvedDB, err := config.ResolveTarget(*dbPath)
	if err != nil {
		logger.Error("database path resolution failed", "err", err)
		os.Exit(1)
	}
	logger.Info("mpm-critic target resolved", "workspace", workspace, "db", resolvedDB)
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

	// Optionally run the telemetry HighTokenNoArtifactHunt in the same
	// cycle. It is READ-ONLY: it reports the observation and writes
	// nothing.
	if *telemetryHunt {
		findings, err := runTelemetryHunt(ctx, logger, workspace, dm.SQLDB(), a.Cycle())
		if err != nil {
			logger.Error("telemetry hunt failed", "err", err)
			os.Exit(1)
		}
		logger.Info("mpm-critic telemetry hunt complete", "observations", len(findings))
	}

	logger.Info("mpm-critic complete")
}

// runTelemetryHunt runs the HighTokenNoArtifactHunt and REPORTS it.
//
// IT IS READ-ONLY AGAINST THE SUBSTRATE. It writes nothing — not a
// lesson, not a theory, not an artifact, not a wake.
//
// WHY (Session A adjudication, 2026-10-27 — issue #138).
//
// This path used to end in `mpm call mpm_lessons --payload {...}`, which
// writes a TRUSTED lesson: `lessons_base` has no status column, so a row
// there is trusted on arrival. That is exactly the M5 trust boundary the
// rest of the critic now respects — after P3b, every hunt that asserts
// something emits a PENDING proposal with typed, resolvable citations,
// and no hunt targets the trusted lesson surface.
//
// The telemetry hunt cannot make that move, and the reason is structural
// rather than a matter of effort:
//
//   1. THERE IS NO RESOLVABLE EVIDENCE SOURCE. P2's citation vocabulary
//      is memory / theory / decision / lesson / work / tool_invocation,
//      and every one of them resolves INSIDE mpm.db. The observation's
//      own evidence — the token counts — lives in telemetry.db, a
//      separate database with no foreign keys and no cross-DB
//      reference. There is no citation type that can address it.
//   2. THE OBSERVATION IS ABOUT AN ABSENCE. The hunt fires when a
//      session burned many tokens and recorded NO artifacts. So the
//      artifact the observation is about does not exist, and there is
//      nothing in mpm.db to cite. Citing an unrelated artifact would be
//      inventing a citation; citing one of the session's tool_invocations
//      would cite a row that carries none of the token counts, and so
//      would change the claim from "this session produced nothing" to
//      "this session did something".
//
// So the observation is reported and not stored. When a resolvable
// evidence source exists — a citation type that can address a
// telemetry-ledger row, or a hunt observation that names an mpm.db
// artifact that actually carries the claim — the write path can be
// reconsidered. Until then there is none.
//
// ONE MORE THING THIS FIXES. The count used to come from
// `mpm call mpm_provenance --payload {"action":"count_by_session"}`.
// **No such tool exists** — it is not in the registry, and never was.
// So the hunt failed on every qualifying session and
// `mpm-critic --telemetry-hunt` exited 1. The count is now computed
// IN-PROCESS from the substrate's own attribution (the same rule the
// missed-opportunity hunt applies), so no subprocess is involved at
// all — and with no subprocess, this path has no way to write.
//
// ORDERING, for whoever repairs the count next: the trust boundary is
// closed FIRST. Repairing the count against a substrate that still
// accepted a trusted-lesson write would ARM the write, not fix it.
//
// ONE MORE DEFECT THIS FIXES. The hunt looked for the telemetry store
// at "<projectRoot>/telemetry.db" — the path docs/SPEC.md §6.6 names.
// The COLLECTOR has never written there: it writes to
// "<workspace>/src/db/telemetry.db" (the same "src/db/" layout as
// mpm.db). So the hunt was reading a path nothing writes to.
//
// It did not fail loudly, because `telemetry.Open` applies the DDL and
// therefore CREATES an empty store at the wrong path — the hunt then
// found nothing and reported zero observations, every cycle, forever.
// A diagnostic that can never fire is worse than no diagnostic: it
// looks like a clean bill of health.
//
// The path now comes from the shared resolver (internal/telemetry),
// the store is opened READ-ONLY so the hunt cannot materialise a
// store, and a missing store is "no observations" rather than a
// failure.
func runTelemetryHunt(
	ctx context.Context,
	log *slog.Logger,
	projectRoot string,
	db *sql.DB,
	cycle int,
) ([]telemetry.Finding, error) {
	telemetryDB, err := telemetry.ResolveStorePath(projectRoot)
	if err != nil {
		return nil, fmt.Errorf("resolve telemetry store path: %w", err)
	}
	store, err := telemetry.OpenReadOnly(telemetryDB)
	if err != nil {
		if errors.Is(err, telemetry.ErrStoreNotFound) {
			// No ledger yet: no observations. Not a failure — a fresh
			// workspace has not collected anything.
			log.Info("no telemetry store; the telemetry hunt found no observations",
				"path", telemetryDB)
			return nil, nil
		}
		return nil, fmt.Errorf("open telemetry store: %w", err)
	}
	defer store.Close()

	// The artifact count, computed in-process against the substrate the
	// critic already holds. This is the same attribution rule the
	// missed-opportunity hunt applies, so the two cannot drift.
	countFn := func(ctx context.Context, sessionID string) (int, error) {
		return critic.SessionArtifactCount(ctx, db, sessionID)
	}

	sevenDaysAgo := telemetry.HuntConfig{
		HighTokenThreshold: 100000,
		MinInvocations:     1,
		Since:              time.Now().Add(-7 * 24 * time.Hour).Unix(), // 7-day lookback
	}

	findings, err := telemetry.Hunt(ctx, store, sevenDaysAgo, countFn)
	if err != nil {
		return nil, fmt.Errorf("telemetry Hunt: %w", err)
	}

	// REPORT, do not store.
	//
	// The cycle tag is kept in the report so an operator can correlate
	// an observation with the critic cycle that saw it — the same
	// correlation the old lesson tag provided, without the write.
	cycleTag := fmt.Sprintf("critic_cycle_%d", cycle)
	for _, f := range findings {
		log.Info("telemetry observation (read-only diagnostic; nothing written)",
			"session", f.SessionID,
			"invocations", f.InvocationCount,
			"input_tokens", f.TotalInputTokens,
			"output_tokens", f.TotalOutputTokens,
			"tokens", f.TotalInputTokens+f.TotalOutputTokens,
			"cycle_tag", cycleTag,
			"reason", f.Reason,
		)
	}
	return findings, nil
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
