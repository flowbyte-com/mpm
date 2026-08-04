// handlers_cascade.go — mpm cascade materialize subcommand.
//
// Wires the cascade materializer into the CLI process. The materializer
// runs as a background goroutine; this handler loops until the outbox is
// drained (or a bounded limit is reached) then exits.
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
	"strings"
	"time"

	"github.com/flowbyte-com/mpm-core/usererror"
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

	// context.Background() is the long-lived context for the materializer's
	// polling loop. This matches the reviewer's Important finding: the ctx
	// must outlive the request-scoped ctx that may be cancelled.
	ctx := context.Background()

	start := time.Now()

	// Ensure the materializer is stopped on every exit path.
	// The defer fires in LIFO order; Stop is idempotent so this is safe
	// even if Start was never called or Stop was already called.
	defer dm.StopCascadeMaterializer()

	// Start the materializer with a background context so it outlives any
	// request-scoped context that may be cancelled.
	dm.StartCascadeMaterializer(ctx)

	if *once {
		// Run one batch and exit.
		report, err := dm.MaterializeCascadeIntents(ctx, 10)
		if err != nil {
			usererror.Error("materialize batch: %v", err)
			return 1
		}
		fmt.Printf("materialized=%d failed=%d pending_after=%d elapsed=%s\n",
			report.Materialized, report.Failed,
			pendingCount(), time.Since(start))
		return 0
	}

	// Default: loop until the queue is drained.
	iteration := 0
	const idlePollsToConfirmDrain = 2

	for {
		iteration++
		if *maxIterations > 0 && iteration > *maxIterations {
			nPending := pendingCount()
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
		idlePolls := 0
		for {
			nPending := pendingCount()
			nProcessing := processingCount()
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

// pendingCount returns the number of 'pending' rows in the cascade outbox.
func pendingCount() int {
	dm := getDB()
	if dm == nil {
		return -1
	}
	var n int
	_ = dm.SQLDB().QueryRow(`SELECT COUNT(*) FROM epistemic_cascade_outbox WHERE status = 'pending'`).Scan(&n)
	return n
}

// processingCount returns the number of 'processing' rows in the cascade outbox.
func processingCount() int {
	dm := getDB()
	if dm == nil {
		return -1
	}
	var n int
	_ = dm.SQLDB().QueryRow(`SELECT COUNT(*) FROM epistemic_cascade_outbox WHERE status = 'processing'`).Scan(&n)
	return n
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

	// Also print a summary of all non-pending states.
	var pending, processing, materialized, failed int
	_ = dm.SQLDB().QueryRow(`SELECT COUNT(*) FROM epistemic_cascade_outbox WHERE status = 'pending'`).Scan(&pending)
	_ = dm.SQLDB().QueryRow(`SELECT COUNT(*) FROM epistemic_cascade_outbox WHERE status = 'processing'`).Scan(&processing)
	_ = dm.SQLDB().QueryRow(`SELECT COUNT(*) FROM epistemic_cascade_outbox WHERE status = 'materialized'`).Scan(&materialized)
	_ = dm.SQLDB().QueryRow(`SELECT COUNT(*) FROM epistemic_cascade_outbox WHERE status = 'failed'`).Scan(&failed)

	fmt.Printf("\nOutbox summary — pending=%d processing=%d materialized=%d failed=%d\n",
		pending, processing, materialized, failed)
	return 0
}
