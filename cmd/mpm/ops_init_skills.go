// ops_init_skills.go — `mpm ops init skills` command.
//
// Seeds the Baseline Skill Library: the curated set of skills that
// close MPM's most-used procedural loops (status reporting, health
// diagnostics). Idempotent — existing rows are detected and preserved.
// Operator's local edits to a seeded skill are detected (content
// mismatch) and surfaced in the report; the edit is never silently
// overwritten.
//
// This is the explicit alternative to auto-seeding at install. The
// "Truth is external" and "no auto-noise" principles forbid silent
// state mutation, so the operator runs this command when they want
// the baseline skills seeded. Re-runs are safe no-ops.
//
// Architecture:
//
//	registry:  internal/core/seed/skills.go (SeedSkills slice)
//	engine:    internal/core/seed/engine.go (ApplySkills)
//	cli:       this file                   (handleOpsInitSkills)
//	router:    cmd/mpm/router.go           (case "init": "skills")
//
// Output reuses printSeedSummary from ops_init_directives.go — same
// shape (Created / Skipped / Drifted buckets, reconciliation hint)
// keeps the two seed commands visually consistent.
package main

import (
	"github.com/flowbyte-com/mpm-core/seed"
)

func handleOpsInitSkills(args []string) int {
	// Parse flags. The command currently takes no required args; future
	// could add --dry-run, --force, --prune (remove skills no longer
	// in the registry). For now: bare command, prints summary.
	dm := getDB()
	if dm == nil {
		return 1
	}

	summary, err := seed.ApplySkills(dm)
	if err != nil {
		printError("seed skills: %v", err)
		return 1
	}

	printSkillsSeedSummary(summary)
	return 0
}

// printSkillsSeedSummary uses the skills-flavoured banner so operators
// can distinguish "what just seeded" at a glance. Field order
// matches the directives report for parity.
func printSkillsSeedSummary(s seed.SeedSummary) {
	printSeedReport("Baseline Skill Library — seed report", s, "skills")
}
