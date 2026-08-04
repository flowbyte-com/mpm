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
    materialize     Materialize pending cascade intents into theories

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
