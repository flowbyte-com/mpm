// capability_seed.go — `mpm capability seed` command.
//
// Seeds the Tier 1 read-only capability primitive set into the
// capabilities table. Idempotent — existing rows are detected
// and preserved. Operator's local edits to a seeded capability
// are detected (source_code mismatch) and surfaced in the
// Drifted bucket; the edit is never silently overwritten.
//
// This is the explicit alternative to auto-seeding at install.
// The "Truth is external" and "no auto-noise" principles forbid
// silent state mutation, so the operator runs this command when
// they want the baseline capabilities seeded. Re-runs are safe
// no-ops.
//
// Sidecar support: if the operator has placed a custom
// `bundled_capabilities.json` in $MPM_WORKSPACE (or passes an
// explicit path via --sidecar), the loader merges it with the
// compiled-in SeedCapabilities registry before applying. This
// lets operators ship custom primitives or patch shipped
// primitives without recompiling mpm.
//
// Architecture:
//
//	registry:  internal/core/seed/capabilities.go (SeedCapabilities slice)
//	loader:    internal/core/seed/capabilities_loader.go (LoadBundledCapabilities)
//	engine:    internal/core/seed/engine_capabilities.go (ApplyCapabilitiesFromBundle)
//	cli:       this file (handleCapabilitySeed)
//	router:    cmd/mpm/router.go (case "capability": "seed")
package main

import (
	"fmt"
	"strings"

	"github.com/flowbyte-com/mpm-core/capability"
	"github.com/flowbyte-com/mpm-core/seed"
)

// handleCapabilitySeed is the entry point for `mpm capability seed`.
//
// Flags:
//
//	--sidecar <path>   Path to a custom bundled_capabilities.json.
//	                   Default: $MPM_WORKSPACE/bundled_capabilities.json
//	                   (omitted entirely if neither exists).
//	--dry-run          Parse + merge the bundle, but skip the
//	                   database write. Prints the report with
//	                   zero Created rows.
func handleCapabilitySeed(args []string) int {
	var sidecarPath string
	for i := 0; i < len(args); i++ {
		switch args[i] {
		case "-h", "--help", "help":
			printCapabilitySeedHelp()
			return 0
		case "--sidecar":
			if i+1 >= len(args) {
				printError("--sidecar requires a path argument")
				return 1
			}
			sidecarPath = args[i+1]
			i++
		default:
			printError("mpm capability seed: unknown flag %q", args[i])
			fmt.Println("Try: mpm capability seed --help")
			return 1
		}
	}

	dm := getDBConcrete()
	if dm == nil {
		return 1
	}

	// 1. Load the bundled capabilities (compiled + sidecar).
	//    LoadBundledCapabilities returns a copy of SeedCapabilities
	//    when no sidecar is configured, so the apply phase always
	//    has a non-nil bundle.
	bundle, err := seed.LoadBundledCapabilities(sidecarPath)
	if err != nil {
		printError("load bundled capabilities: %v", err)
		return 1
	}

	// 2. Apply the merged bundle. ApplyCapabilitiesFromBundle
	//    walks bundle.Merged (NOT the compiled SeedCapabilities
	//    directly) so the sidecar's overrides + additions are
	//    honored.
	store := capability.NewStore(dm)
	summary, err := seed.ApplyCapabilitiesFromBundle(store, bundle.Merged)
	if err != nil {
		printError("seed capabilities: %v", err)
		return 1
	}

	// 3. Print the report.
	printCapabilitySeedReport(bundle, summary)
	return 0
}

// printCapabilitySeedReport renders the seed result with the
// sidecar audit info at the top so the operator can see what
// was actually applied. Mirrors the directives/skills report
// shape (Created / Skipped / Drifted buckets) so the surface
// is consistent across all seed commands.
func printCapabilitySeedReport(bundle seed.LoadBundledCapabilitiesResult, s seed.SeedSummary) {
	fmt.Println("Capability Seed — Tier 1 read-only primitives")
	fmt.Println(strings.Repeat("─", 60))

	if bundle.SidecarPath != "" {
		fmt.Printf("\n  Sidecar: %s\n", bundle.SidecarPath)
		fmt.Printf("    Compiled: %d, Overrides: %d, Additions: %d\n",
			bundle.CompiledCount,
			bundle.SidecarOverridesCount,
			bundle.SidecarAdditionsCount)
	} else {
		fmt.Printf("\n  Bundle: compiled-in SeedCapabilities (no sidecar)\n")
		fmt.Printf("    Compiled: %d\n", bundle.CompiledCount)
	}

	if len(s.Created) > 0 {
		fmt.Printf("\n  ✓ Created (%d):\n", len(s.Created))
		for _, id := range s.Created {
			fmt.Printf("    + %s\n", id)
		}
	}

	if len(s.Skipped) > 0 {
		fmt.Printf("\n  · Skipped (%d) — already seeded, source_code matches:\n", len(s.Skipped))
		for _, id := range s.Skipped {
			fmt.Printf("    = %s\n", id)
		}
	}

	if len(s.Drifted) > 0 {
		fmt.Printf("\n  ⚠ Drifted (%d) — local source_code differs from seed (preserved):\n", len(s.Drifted))
		for _, id := range s.Drifted {
			fmt.Printf("    ! %s\n", id)
		}
		fmt.Println("    (Local edits to a seeded capability's source_code are intentionally preserved.")
		fmt.Println("     To re-sync, delete the local row and re-run.)")
	}

	total := len(s.Created) + len(s.Skipped) + len(s.Drifted)
	if total == 0 {
		fmt.Printf("\n  No entries in bundle. (Empty SeedCapabilities + sidecar.)\n")
	} else {
		fmt.Printf("\n  %d total: %d created, %d skipped, %d drifted.\n",
			total, len(s.Created), len(s.Skipped), len(s.Drifted))
	}

	if len(s.Created) > 0 {
		fmt.Println("\nThese capabilities are now in 'validated' state. The forge tick will")
		fmt.Println("promote each to 'probation' on first invocation; 'probation' → 'active'")
		fmt.Println("is earned by reaching the invocation-success threshold.")
		fmt.Println("\nInspect at any time with:")
		fmt.Println("  mpm help capability list")
	}
}

// printCapabilitySeedHelp is the per-subcommand help. Shown
// when the operator passes --help or asks for unknown flag help.
func printCapabilitySeedHelp() {
	fmt.Println()
	fmt.Println("mpm capability seed — install / refresh the Tier 1 capability set")
	fmt.Println()
	fmt.Println("Usage: mpm capability seed [--sidecar <path>]")
	fmt.Println()
	fmt.Println("Flags:")
	fmt.Println("  --sidecar <path>   Path to a custom bundled_capabilities.json.")
	fmt.Println("                     Default: $MPM_WORKSPACE/bundled_capabilities.json,")
	fmt.Println("                     or omit entirely if no sidecar is configured.")
	fmt.Println()
	fmt.Println("The seed is idempotent. Re-running is a clean no-op when the database")
	fmt.Println("matches the bundle. Operator edits to a seeded source_code surface in")
	fmt.Println("the Drifted bucket and are never overwritten.")
	fmt.Println()
}
