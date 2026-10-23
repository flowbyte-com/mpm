// ops_init_directives_test.go — regression coverage for the
// `mpm ops init directives` report.
//
// HAL-9001 Finding 5: on a fresh install the baseline directives are
// already seeded (NewDatabaseManager applies the registry when the
// database is created), so the command legitimately reports
// "0 created, 5 skipped". Before this change the report said nothing
// about what that outcome means, so a first-time operator saw a
// zero-create count with no explanation and had to assume the
// bootstrap had failed.
//
// The explanation is directives-specific on purpose: skills are NOT
// auto-seeded, so the same report must not claim "already seeded" for
// them.
package main

import (
	"io"
	"os"
	"strings"
	"testing"

	"github.com/flowbyte-com/mpm-core/seed"
)

// captureSeedReport runs printSeedReport with stdout redirected and
// returns what it printed.
func captureSeedReport(t *testing.T, s seed.SeedSummary, label string) string {
	t.Helper()

	orig := os.Stdout
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatalf("os.Pipe: %v", err)
	}
	os.Stdout = w

	done := make(chan string, 1)
	go func() {
		b, _ := io.ReadAll(r)
		done <- string(b)
	}()

	printSeedReport("banner", s, label)

	os.Stdout = orig
	_ = w.Close()
	out := <-done
	_ = r.Close()
	return out
}

func TestSeedReport_ExplainsAnAlreadySeededBaseline(t *testing.T) {
	out := captureSeedReport(t, seed.SeedSummary{
		Skipped: []string{
			"mpm-seed-read-wake-context",
			"mpm-seed-session-end-cluster-triage",
			"mpm-seed-wake-triage-policy",
			"mpm-seed-daemon-health",
			"mpm-seed-epistemic-compaction-policy",
		},
	}, "directives")

	if !strings.Contains(out, "5 skipped") {
		t.Errorf("report does not show the skipped count:\n%s", out)
	}
	if !strings.Contains(out, "Already seeded") {
		t.Errorf("report does not explain that the baseline was already applied; "+
			"a zero-create report reads as a failed bootstrap:\n%s", out)
	}
	if !strings.Contains(out, "database is created") {
		t.Errorf("report does not say WHERE the baseline comes from:\n%s", out)
	}
	if strings.Contains(out, "These directives are now active") {
		t.Errorf("report claims the directives were just activated, but nothing "+
			"was created:\n%s", out)
	}
}

func TestSeedReport_ActivatesNewlyCreatedDirectives(t *testing.T) {
	out := captureSeedReport(t, seed.SeedSummary{
		Created: []string{"mpm-seed-read-wake-context"},
	}, "directives")

	if !strings.Contains(out, "These directives are now active") {
		t.Errorf("report does not announce newly created directives:\n%s", out)
	}
	if strings.Contains(out, "Already seeded") {
		t.Errorf("report claims the baseline was already applied even though a "+
			"directive was just created:\n%s", out)
	}
}

// The skills report shares the renderer. Skills are only ever seeded
// through the CLI, so the "already seeded" explanation must not leak
// into it — it would tell an operator that something exists when it
// does not.
func TestSeedReport_SkillsReportDoesNotClaimAlreadySeeded(t *testing.T) {
	out := captureSeedReport(t, seed.SeedSummary{
		Skipped: []string{"some-skill"},
	}, "skills")

	if strings.Contains(out, "Already seeded") {
		t.Errorf("skills report claims the baseline was already applied; skills "+
			"are not auto-seeded:\n%s", out)
	}
}
