// handlers_cascade.go — mpm cascade materialize subcommand.
//
// Foreground escape hatch for operators who don't want to run mpm-scheduler.
// The handler calls dm.MaterializeCascadeIntents directly in a loop until
// the outbox is drained (or --max-iterations is hit). Background draining
// is owned exclusively by mpm-scheduler — this CLI never spawns a hidden
// cascade thread.
//
// Exit codes:
//   0 = drained (queue empty)
//   1 = runtime error
//   2 = timeout (--max-iterations exceeded)
package main

import (
	"context"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/flowbyte-com/mpm-core/usererror"
	"github.com/flowbyte-com/mpm/internal/scheduler"
)

// handleCascade is the top-level handler for `mpm cascade`.
func handleCascade(args []string) int {
	if len(args) < 2 {
		printCascadeHelp()
		return 1
	}

	subcommand := args[1]

	switch subcommand {
	case "--help", "-h", "help":
		printCascadeHelp()
		return 0
	case "materialize":
		return handleCascadeMaterialize(args[2:])
	case "list-dead-letters":
		return handleListDeadLetters(args[2:])
	default:
		usererror.Error("Unknown cascade subcommand: %s", subcommand)
		printCascadeHelp()
		return 1
	}
}

func printCascadeHelp() {
	fmt.Print(`mpm cascade — Epistemic cascade operations

Usage: mpm cascade <subcommand> [flags]

Subcommands:
    materialize        Materialize pending cascade intents into theories
    list-dead-letters  List failed (dead-letter) cascade intents

Flags for materialize:
    --once              Run one batch and exit
    --max-iterations N  Bound iterations (0=unbounded, exits 2 on timeout)
    --poll-interval T   Sleep between empty-queue polls (default 5s, min 1s)
`)
}

// handleCascadeMaterialize runs the cascade materializer.
func handleCascadeMaterialize(args []string) int {
	fs := flag.NewFlagSet("mpm cascade materialize", flag.ContinueOnError)
	fs.SetOutput(os.Stdout)

	once := fs.Bool("once", false, "Run one batch and exit")
	maxIterations := fs.Int("max-iterations", 0, "Bound iterations (0=unbounded)")
	pollInterval := fs.Duration("poll-interval", 5*time.Second, "Sleep interval between polls")

	if err := fs.Parse(args); err != nil {
		if err == flag.ErrHelp {
			return 0
		}
		usererror.Error("parse flags: %v", err)
		return 1
	}

	if *pollInterval < time.Second {
		usererror.Error("--poll-interval must be at least 1s")
		return 1
	}

	dm := getDBConcrete()
	if dm == nil {
		usererror.Error("database not available")
		return 1
	}

	// Defensive flock against overlapping CLI invocations (issue #4).
	// SQLite WAL + BEGIN IMMEDIATE already prevents data corruption, but a
	// second concurrent `mpm cascade materialize` would burn CPU on empty
	// batches. The lock fails fast (LOCK_EX|LOCK_NB) so the loser exits
	// with a clean error instead of blocking on busy_timeout.
	//
	// list-dead-letters is read-only and explicitly does NOT acquire this
	// lock — operators must be able to inspect the outbox while a drain
	// is running.
	lockPath := os.Getenv("MPM_CASCADE_LOCK")
	if lockPath == "" {
		if ws := os.Getenv("MPM_WORKSPACE"); ws != "" {
			lockPath = filepath.Join(ws, "cascade.lock")
		} else {
			lockPath = "/tmp/mpm-cascade.lock"
		}
	}
	lockFile, err := scheduler.AcquireLock(lockPath)
	if err != nil {
		usererror.Error("mpm cascade materialize: another invocation is in progress (%v)", err)
		return 1
	}
	defer func() { _ = lockFile.Close() }()

	// Context for the synchronous MaterializeCascadeIntents calls below.
	// This is the foreground escape hatch — no background goroutine, no
	// lifecycle management. The CLI process owns one call sequence.
	ctx := context.Background()

	start := time.Now()

	if *once {
		// Run one batch and exit.
		report, err := dm.MaterializeCascadeIntents(ctx, 10)
		if err != nil {
			usererror.Error("materialize batch: %v", err)
			return 1
		}
		nPending, perr := pendingCount()
		if perr != nil {
			usererror.Error("count pending: %v", perr)
			return 1
		}
		fmt.Printf("materialized=%d failed=%d pending_after=%d elapsed=%s\n",
			report.Materialized, report.Failed,
			nPending, time.Since(start))
		return 0
	}

	// Default: loop until the queue is drained.
	iteration := 0
	const idlePollsToConfirmDrain = 2

	for {
		iteration++
		if *maxIterations > 0 && iteration > *maxIterations {
			nPending, perr := pendingCount()
			if perr != nil {
				usererror.Error("count pending: %v", perr)
				return 1
			}
			fmt.Printf("materialized=%d failed=%d pending_after=%d elapsed=%s\n",
				0, 0, nPending, time.Since(start))
			return 2 // timeout
		}

		// Run one batch.
		report, err := dm.MaterializeCascadeIntents(ctx, 10)
		if err != nil {
			usererror.Error("materialize batch: %v", err)
			return 1
		}

		// If the batch produced no forward progress, poll until empty.
		// A query failure here must NOT be treated as "drained" — bail out
		// so the operator can investigate rather than silently exit with
		// the queue still containing pending/processing rows.
		idlePolls := 0
		for {
			nPending, perr := pendingCount()
			if perr != nil {
				usererror.Error("count pending during drain: %v", perr)
				return 1
			}
			nProcessing, perr := processingCount()
			if perr != nil {
				usererror.Error("count processing during drain: %v", perr)
				return 1
			}
			if nPending == 0 && nProcessing == 0 {
				idlePolls++
				if idlePolls >= idlePollsToConfirmDrain {
					// Queue is confirmed empty.
					fmt.Printf("materialized=%d failed=%d pending_after=0 elapsed=%s\n",
						report.Materialized, report.Failed, time.Since(start))
					return 0
				}
				// Still confirm with a second poll.
				time.Sleep(*pollInterval)
				continue
			}
			idlePolls = 0
			break
		}

		// If we processed something, loop immediately. If nothing happened,
		// sleep before the next poll.
		if report.Processed == 0 {
			time.Sleep(*pollInterval)
		}
	}
}

// pendingCount returns the number of 'pending' rows in the cascade outbox,
// or an error if the query fails. Returning 0 on error would cause the
// materialize loop to falsely confirm the queue is drained, silently
// leaving pending rows unprocessed.
func pendingCount() (int, error) {
	if dbManager == nil {
		return 0, fmt.Errorf("database not available")
	}
	var n int
	if err := dbManager.SQLDB().QueryRow(`SELECT COUNT(*) FROM epistemic_cascade_outbox WHERE status = 'pending'`).Scan(&n); err != nil {
		return 0, fmt.Errorf("count pending cascade outbox rows: %w", err)
	}
	return n, nil
}

// processingCount returns the number of 'processing' rows in the cascade outbox,
// or an error if the query fails.
func processingCount() (int, error) {
	if dbManager == nil {
		return 0, fmt.Errorf("database not available")
	}
	var n int
	if err := dbManager.SQLDB().QueryRow(`SELECT COUNT(*) FROM epistemic_cascade_outbox WHERE status = 'processing'`).Scan(&n); err != nil {
		return 0, fmt.Errorf("count processing cascade outbox rows: %w", err)
	}
	return n, nil
}

// handleListDeadLetters runs `mpm cascade list-dead-letters`.
// It queries the outbox for status='failed' rows and prints them.
func handleListDeadLetters(args []string) int {
	dm := getDB()
	if dm == nil {
		usererror.Error("database not available")
		return 1
	}

	rows, err := dm.SQLDB().Query(`
		SELECT id, invalidation_event_id, dead_artifact_id, dead_artifact_type,
		       downstream_artifact_id, downstream_artifact_type,
		       cascade_depth, reason, status, terminal_error,
		       attempt_count, created_at, updated_at
		FROM epistemic_cascade_outbox
		WHERE status = 'failed'
		ORDER BY updated_at DESC
		LIMIT 100
	`)
	if err != nil {
		usererror.Error("query dead-letter intents: %v", err)
		return 1
	}
	defer rows.Close()

	count := 0
	fmt.Println("CASCADE DEAD-LETTER INTENTS (status=failed)")
	fmt.Println(strings.Repeat("-", 120))

	header := fmt.Sprintf("%-36s %-10s %-10s %-12s %-12s %-6s %-8s %s",
		"ID", "EVENT_ID", "DEAD_TYPE", "DOWN_TYPE", "DOWN_ID", "DEPTH", "ATTEMPTS", "TERMINAL_ERROR")
	fmt.Println(header)
	fmt.Println(strings.Repeat("-", 120))

	for rows.Next() {
		var (
			id, eventID, deadArtifactID, deadArtifactType string
			downstreamArtifactID, downstreamArtifactType  string
			cascadeDepth                                  int
			reason, status, terminalError                string
			attemptCount                                 int
			createdAt, updatedAt                         int64
		)
		if err := rows.Scan(&id, &eventID, &deadArtifactID, &deadArtifactType,
			&downstreamArtifactID, &downstreamArtifactType,
			&cascadeDepth, &reason, &status, &terminalError,
			&attemptCount, &createdAt, &updatedAt); err != nil {
			usererror.Error("scan dead-letter row: %v", err)
			return 1
		}
		createdTime := time.Unix(createdAt, 0).UTC().Format(time.RFC3339)
		updatedTime := time.Unix(updatedAt, 0).UTC().Format(time.RFC3339)
		shortID := id
		if len(shortID) > 8 {
			shortID = shortID[:8]
		}
		shortDown := downstreamArtifactID
		if len(shortDown) > 10 {
			shortDown = shortDown[:10]
		}
		fmt.Printf("%-36s %-10s %-10s %-12s %-12s %-6d %-8d %s\n",
			shortID, eventID[:8], deadArtifactType, downstreamArtifactType,
			shortDown, cascadeDepth, attemptCount, terminalError)
		_ = createdTime
		_ = updatedTime
		_ = reason
		_ = status
		count++
	}

	if count == 0 {
		fmt.Println("No dead-letter intents (status=failed).")
	} else {
		fmt.Printf("\nTotal dead-letter intents: %d\n", count)
	}

	// Also print a summary of all non-pending states. Best-effort counts;
	// each one logs a warning and defaults to 0 on failure.
	var pending, processing, materialized, failed int
	if err := dm.SQLDB().QueryRow(`SELECT COUNT(*) FROM epistemic_cascade_outbox WHERE status = 'pending'`).Scan(&pending); err != nil {
		usererror.Warn("handleListDeadLetters: failed to count pending cascade outbox rows, defaulting to 0: %v", err)
	}
	if err := dm.SQLDB().QueryRow(`SELECT COUNT(*) FROM epistemic_cascade_outbox WHERE status = 'processing'`).Scan(&processing); err != nil {
		usererror.Warn("handleListDeadLetters: failed to count processing cascade outbox rows, defaulting to 0: %v", err)
	}
	if err := dm.SQLDB().QueryRow(`SELECT COUNT(*) FROM epistemic_cascade_outbox WHERE status = 'materialized'`).Scan(&materialized); err != nil {
		usererror.Warn("handleListDeadLetters: failed to count materialized cascade outbox rows, defaulting to 0: %v", err)
	}
	if err := dm.SQLDB().QueryRow(`SELECT COUNT(*) FROM epistemic_cascade_outbox WHERE status = 'failed'`).Scan(&failed); err != nil {
		usererror.Warn("handleListDeadLetters: failed to count failed cascade outbox rows, defaulting to 0: %v", err)
	}

	fmt.Printf("\nOutbox summary — pending=%d processing=%d materialized=%d failed=%d\n",
		pending, processing, materialized, failed)
	return 0
}
