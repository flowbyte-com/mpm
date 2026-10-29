// ops_init_directives.go — `mpm ops init directives` command.
//
// Seeds the Baseline Cognitive Bootstrap: the reference prime
// directives that close MPM's advanced cognitive loops (wake context,
// cluster triage). Idempotent — existing rows are detected and
// preserved. Operator's local edits to a seeded directive are
// detected (content hash mismatch) and surfaced in the report; the
// edit is never silently overwritten.
//
// This command RE-RUNS the baseline seed; it is not how the baseline
// gets there. NewDatabaseManager applies the registry whenever the
// database is created, so a fresh install already carries the five
// reference directives before this command is ever run — and a fresh
// install therefore reports "0 created, 5 skipped".
//
// The "Truth is external" and "no auto-noise" principles apply to what
// the SEED contains (MPM never invents the operator's own directives),
// not to the presence of the shipped baseline. The command remains the
// documented recovery path after an accidental shred, and the way to
// seed the shared database of a multi-agent workspace.
//
// Architecture:
//
//	registry:  internal/core/seed/directives.go (SeedDirectives slice)
//	engine:    internal/core/seed/engine.go   (ApplyDirectives + insertSeedRow)
//	cli:       this file                      (handleOpsInitDirectives)
//	router:    cmd/mpm/router.go              (case "init": "directives")
package main

import (
	"fmt"
	"os"
	"strings"

	"github.com/flowbyte-com/mpm-core/seed"
)

func handleOpsInitDirectives(args []string) int {
	// Parse flags.
	//
	// There is exactly one, and it exists to make the content of a
	// candidate EXPLICIT (issue #59, operator constraint 4). A cleanup
	// report is a log: it lands in a terminal scrollback and in CI
	// output, and the directive text is the operator's own policy.
	// Printing it by default would put policy text into logs the
	// operator did not ask for.
	//
	// There is deliberately still no --prune flag. Adding one would be
	// introducing an automatic cleanup mechanism to close a policy
	// question, which the question explicitly forbids.
	showContent := false
	for _, arg := range args {
		switch arg {
		case "--show-content":
			showContent = true
		case "--help", "-h":
			fmt.Println("Usage: mpm ops init directives [--show-content]")
			fmt.Println()
			fmt.Println("  --show-content  Include a preview of each orphaned directive's")
			fmt.Println("                  content. The default report does NOT print content:")
			fmt.Println("                  a report is a log, and the directive text is the")
			fmt.Println("                  operator's own policy.")
			return 0
		default:
			fmt.Fprintf(os.Stderr, "unknown flag: %s\n", arg)
			fmt.Fprintln(os.Stderr, "Usage: mpm ops init directives [--show-content]")
			return 1
		}
	}

	dm := getDB()
	if dm == nil {
		return 1
	}

	summary, err := seed.ApplyDirectives(dm)
	if err != nil {
		printError("seed directives: %v", err)
		return 1
	}

	printSeedSummary(summary)

	// Registry-membership report (issue #59, Option B). A directive that
	// has LEFT the registry is eligible for explicit cleanup — and MPM
	// never deletes it on its own. Reporting the candidates here is what
	// makes the eligibility real rather than theoretical.
	//
	// This runs only when the operator asks for it, not at boot: detection
	// is cheap and harmless, but a boot-time report nobody asked for is
	// noise, and the policy is that cleanup is an EXPLICIT act.
	orphans, err := seed.OrphanedDirectives(dm)
	if err != nil {
		printError("read orphaned directives: %v", err)
		return 1
	}
	printOrphanedDirectives(orphans, showContent)

	return 0
}

// printOrphanedDirectives renders the registry-orphaned directive report.
//
// The wording is load-bearing. It must not read as "these will be removed"
// (nothing removes them) and must not read as "these are broken" (they are
// not). They are rows that used to be standing policy and no longer are,
// and the operator decides what happens to them.
//
// The CONTENT is not printed unless the operator asked for it with
// --show-content. The report is a log, and the directive text is the
// operator's own policy — so the default line carries what is needed to
// decide WHETHER TO LOOK, and the looking is a separate, explicit act.
func printOrphanedDirectives(orphans []seed.OrphanedDirective, showContent bool) {
	if len(orphans) == 0 {
		return
	}
	fmt.Printf("\n  ⚠ Orphaned directives (%d) — in the baseline once, no longer in the registry:\n", len(orphans))
	for _, o := range orphans {
		if showContent {
			fmt.Printf("    ! %s\n", seed.FormatOrphanedDirectiveWithContent(o))
			continue
		}
		fmt.Printf("    ! %s\n", seed.FormatOrphanedDirective(o))
	}
	fmt.Println("    (These rows were written by the baseline seeder but their registry")
	fmt.Println("     entry has since been removed, so they are no longer standing policy.")
	fmt.Println("     They are ELIGIBLE for cleanup — nothing deletes them for you.")
	if !showContent {
		fmt.Println("     Their content is NOT shown here; run with --show-content to inspect")
		fmt.Println("     a candidate's text before deciding what to do with it.")
	}
	fmt.Println("     To remove one, delete the row explicitly; to keep it, leave it alone.)")
}

// printSeedSummary is the directives-flavoured back-compat wrapper.
// New code should call printSeedReport with the appropriate banner + label.
func printSeedSummary(s seed.SeedSummary) {
	printSeedReport("Baseline Cognitive Bootstrap — seed report", s, "directives")
}

// printSeedReport renders a SeedSummary with caller-supplied banner
// and label. The label drives the "no rows in registry" empty-state
// sentence and the post-seed activity hint, so each seed command
// points the operator at the right next call.
func printSeedReport(banner string, s seed.SeedSummary, label string) {
	fmt.Println(banner)
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
		fmt.Println("    (Local edits to a seeded entry are intentionally preserved.")
		fmt.Println("     To re-sync, delete the local row and re-run.)")
	}

	if len(s.Revived) > 0 {
		fmt.Printf("\n  ↻ Revived (%d) — restored from a soft-deleted state, local content preserved:\n", len(s.Revived))
		for _, id := range s.Revived {
			fmt.Printf("    ↻ %s\n", id)
		}
		fmt.Println("    (The local edit survived the restore and the entry is live again.")
		fmt.Println("     To re-sync with the shipped baseline, delete the local row and re-run.)")
	}

	total := len(s.Created) + len(s.Skipped) + len(s.Updated) + len(s.Revived)
	if total == 0 {
		fmt.Printf("\n  No entries in registry. (Empty seed.%s slice.)\n", label)
	} else {
		fmt.Printf("\n  %d total: %d created, %d skipped, %d drifted, %d revived.\n",
			total, len(s.Created), len(s.Skipped), len(s.Updated), len(s.Revived))
	}

	if len(s.Created) > 0 || len(s.Revived) > 0 {
		switch label {
		case "directives":
			fmt.Println("\nThese directives are now active. The agent will read them on")
			fmt.Println("the next session via `mpm call read_directives`.")
		case "skills":
			fmt.Println("\nThese skills are now active. The agent will surface them via")
			fmt.Println("proactive_recall_hint when conversation keywords overlap with")
			fmt.Println("a skill's when_to_use field.")
		}
		return
	}

	// Directives only: on a fresh install the baseline was already seeded
	// when the database was created, so this command legitimately creates
	// nothing. "0 created, 5 skipped" is the correct outcome there, not a
	// failed bootstrap — say so, because an unexplained zero reads as a
	// defect to a first-time operator.
	//
	// Skills are NOT auto-seeded (ApplySkills is only reachable from this
	// CLI), so the "already there" explanation must not leak into the
	// skills report.
	if label == "directives" && len(s.Skipped) > 0 {
		fmt.Println("\nAlready seeded — the baseline is applied automatically when the")
		fmt.Println("database is created, so this command had nothing left to create.")
		fmt.Println("Nothing was changed; to re-seed, delete the rows first.")
	}
}
