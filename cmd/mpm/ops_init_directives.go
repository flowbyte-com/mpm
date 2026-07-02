// ops_init_directives.go — `mpm ops init directives` command.
//
// Seeds the Baseline Cognitive Bootstrap: the reference prime
// directives that close MPM's advanced cognitive loops (wake context,
// cluster triage). Idempotent — existing rows are detected and
// preserved. Operator's local edits to a seeded directive are
// detected (content hash mismatch) and surfaced in the report; the
// edit is never silently overwritten.
//
// This is the explicit alternative to auto-seeding at install. The
// "Truth is external" and "no auto-noise" principles forbid silent
// state mutation, so the operator runs this command when they want
// the baseline directives seeded. Re-runs are safe no-ops.
//
// Architecture:
//
//	registry:  internal/seed/directives.go (SeedDirectives slice)
//	engine:    internal/seed/engine.go   (ApplyDirectives + insertSeedRow)
//	cli:       this file                 (handleOpsInitDirectives)
//	router:    cmd/mpm/router.go         (case "init": "directives")
package main

import (
	"fmt"
	"strings"

	mpminternal "mpm/internal"
	"mpm/internal/seed"
)

func handleOpsInitDirectives(args []string) int {
	// Parse flags. The command currently takes no required args; future
	// could add --dry-run, --force, --prune (remove directives no
	// longer in the registry). For now: bare command, prints summary.
	dm, err := mpminternal.NewDatabaseManager("")
	if err != nil {
		printError("open database: %v", err)
		return 1
	}
	defer dm.Close()

	summary, err := seed.ApplyDirectives(dm)
	if err != nil {
		printError("seed directives: %v", err)
		return 1
	}

	printSeedSummary(summary)
	return 0
}

func printSeedSummary(s seed.SeedSummary) {
	fmt.Println("Baseline Cognitive Bootstrap — seed report")
	fmt.Println(strings.Repeat("─", 60))

	if len(s.Created) > 0 {
		fmt.Printf("\n  ✓ Created (%d):\n", len(s.Created))
		for _, id := range s.Created {
			fmt.Printf("    + %s\n", id)
		}
	}

	if len(s.Skipped) > 0 {
		fmt.Printf("\n  · Skipped (%d) — already seeded, content matches:\n", len(s.Skipped))
		for _, id := range s.Skipped {
			fmt.Printf("    = %s\n", id)
		}
	}

	if len(s.Updated) > 0 {
		fmt.Printf("\n  ⚠ Drifted (%d) — local content differs from seed (preserved):\n", len(s.Updated))
		for _, id := range s.Updated {
			fmt.Printf("    ! %s\n", id)
		}
		fmt.Println("    (Local edits to a seeded directive are intentionally preserved.")
		fmt.Println("     To re-sync, delete the local row and re-run.)")
	}

	total := len(s.Created) + len(s.Skipped) + len(s.Updated)
	if total == 0 {
		fmt.Println("\n  No directives in registry. (Empty seed.Directives slice.)")
	} else {
		fmt.Printf("\n  %d total: %d created, %d skipped, %d drifted.\n",
			total, len(s.Created), len(s.Skipped), len(s.Updated))
	}

	if len(s.Created) > 0 {
		fmt.Println("\nThese directives are now active. The agent will read them on")
		fmt.Println("the next session via `mpm call read_directives`.")
	}
}