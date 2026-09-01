// ops_init_tasks.go — `mpm ops init tasks` command.
//
// Seeds the Baseline Cognitive Bootstrap for scheduled tasks: the
// canonical recurring workflows a fresh MPM install should arrive
// with (currently: `epistemic-compaction`, the agent-owned nightly
// compact reflex). Idempotent — existing rows are detected by
// stable id and preserved verbatim. Operator's custom cron / name
// / directive_id / status on a row with the same stable id are
// NEVER overwritten; the seed is a starting configuration, not a
// sync target.
//
// This is the explicit alternative to auto-seeding at install. The
// "Truth is external" and "no auto-noise" principles forbid silent
// state mutation, so the operator runs this command when they want
// the baseline tasks seeded. NewDatabaseManager (every production
// constructor) already calls the apply loop at boot, so a fresh
// install gets the task automatically; this command is for manual
// re-init (e.g. after an accidental `mpm tasks delete`) or shared-DB
// seeding.
//
// Architecture:
//
//	registry:  internal/core/seed/scheduled_tasks.go (SeedScheduledTasks)
//	engine:    internal/core/db.go                 (seedBaselineScheduledTasks)
//	cli:       this file                           (handleOpsInitTasks)
//	router:    cmd/mpm/router.go                   (case "init": "tasks")
package main

import (
	"fmt"
	"strings"

	"github.com/flowbyte-com/mpm-core/seed"
)

func handleOpsInitTasks(args []string) int {
	// Parse flags. The command currently takes no required args; future
	// could add --dry-run, --force, --prune (remove tasks no longer
	// in the registry). For now: bare command, prints summary.
	dm := getDB()
	if dm == nil {
		return 1
	}

	summary, err := dm.SeedBaselineScheduledTasks()
	if err != nil {
		printError("seed tasks: %v", err)
		return 1
	}

	printTasksSeedSummary(summary)
	return 0
}

// printTasksSeedSummary renders a SeedTaskSummary. The Created /
// Skipped / Missing buckets are explained inline so operators know
// what "Missing" means (the directive wasn't seeded yet — run
// `mpm ops init directives` first and re-run this).
func printTasksSeedSummary(s seed.SeedTaskSummary) {
	fmt.Println("Baseline Scheduled Tasks — seed report")
	fmt.Println(strings.Repeat("─", 60))

	if len(s.Created) > 0 {
		fmt.Printf("\n  ✓ Created (%d):\n", len(s.Created))
		for _, id := range s.Created {
			fmt.Printf("    + %s\n", id)
		}
	}

	if len(s.Skipped) > 0 {
		fmt.Printf("\n  · Skipped (%d) — already present, operator customization preserved:\n", len(s.Skipped))
		for _, id := range s.Skipped {
			fmt.Printf("    = %s\n", id)
		}
	}

	if len(s.Missing) > 0 {
		fmt.Printf("\n  ⚠ Missing directive (%d) — task skipped, run `mpm ops init directives` first and re-run:\n", len(s.Missing))
		for _, id := range s.Missing {
			fmt.Printf("    ! %s\n", id)
		}
	}

	total := len(s.Created) + len(s.Skipped) + len(s.Missing)
	if total == 0 {
		fmt.Printf("\n  No entries in registry. (Empty seed.SeedScheduledTasks slice.)\n")
	} else {
		fmt.Printf("\n  %d total: %d created, %d skipped, %d missing.\n",
			total, len(s.Created), len(s.Skipped), len(s.Missing))
	}

	if len(s.Created) > 0 {
		fmt.Println("\nThese tasks are now active. The agent will be woken at the next")
		fmt.Println("scheduled cron occurrence; the wake's metadata.directive_id tells the")
		fmt.Println("agent which directive to read. To inspect or change:")
		fmt.Println("  mpm tasks list")
		fmt.Println("  mpm tasks upsert <id> <name> <cron_expr> <directive_id> [status]")
	}
}
