// release_pass_20260914_dashboard_attention_test.go — Regression
// coverage for the 2026-09-14 final-last-mile dashboard
// Attention-rendering fix.
//
// Pre-fix the dashboard rendered only the readiness items
// (Scheduler, Database, Embedding, etc.) and never surfaced
// the actionable Doctor warnings (Wake backlog, Review
// backlog). The result: a dashboard that said "System health
// ✓ Healthy" while Doctor reported 2 warnings. The operator's
// view diverged from the substrate's.
//
// The fix adds `checkWakeBacklogReady` and `checkReviewBacklogReady`
// to ReadReadiness so the dashboard's readiness stream carries
// the same actionable-warning state Doctor surfaces. The
// attentionSummary helper then aggregates WARN-level items
// into a single Attention line.
//
// This test seeds actionable backlog in a hermetic workspace
// and asserts the dashboard visibly surfaces it. Pre-fix this
// assertion failed because the dashboard never showed the
// backlog rows OR the Attention summary.

package main

import (
	stdlibexec "os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// TestDashboard_AttentionVisibleWithBacklog is the headline
// regression. With actionable Doctor warnings present, the
// dashboard must visibly surface them — both as individual
// readiness rows (with ⚠ markers) and as a single aggregated
// Attention summary line. Pre-fix the dashboard only rendered
// the structural readiness items (Scheduler, Database, etc.)
// and the Attention line was absent, so the operator's view
// silently diverged from Doctor's.
//
// Seed strategy: the review backlog path (GetSpacedReinforcementReview)
// matches memories where (is_long_term=1 OR weight>=5) AND
// (last_accessed_at IS NULL OR < now-14d). Newly added memories
// default to last_accessed_at=NULL and weight=10 (via --weight
// 10) make them eligible. The review backlog count in the
// fixture is dynamic (depends on the install baseline + the
// seeded rows); the regression asserts the dashboard surfaces
// the SAME warning state Doctor surfaces, not a literal number.
func TestDashboard_AttentionVisibleWithBacklog(t *testing.T) {
	bin := buildAttentionBin(t)
	ws := t.TempDir()

	// Seed 3 high-weight memories. Together with whatever the
	// install baseline carries, the review backlog count will
	// be > 0 and Doctor will WARN.
	for i := 0; i < 3; i++ {
		cmd := stdlibexec.Command(bin, "add", "review probe item",
			"--weight", "10",
			"--tags", "ops-review")
		cmd.Env = []string{"MPM_WORKSPACE=" + ws, "PATH=" + lookupTestPath()}
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("seed review memory %d: %v\n%s", i, err, out)
		}
	}

	// Sanity: doctor must report the review backlog.
	doctorOut := mustRunAttention(t, bin, ws, "doctor")
	if !strings.Contains(doctorOut, "Review backlog") {
		t.Fatalf("doctor must surface Review backlog in fixture; got:\n%s", doctorOut)
	}

	// Dashboard must surface the same warning state.
	dashOut := mustRunAttention(t, bin, ws)

	// 1. The Review backlog row must appear in the readiness
	// stream with a ⚠ marker.
	if !strings.Contains(dashOut, "Review backlog") {
		t.Fatalf("dashboard must surface Review backlog row; got:\n%s", dashOut)
	}
	if !strings.Contains(dashOut, "⚠  Review backlog") {
		t.Fatalf("dashboard Review backlog row must use ⚠ marker; got:\n%s", dashOut)
	}

	// 2. The Attention summary line must aggregate the
	// actionable warnings.
	if !strings.Contains(dashOut, "Attention") {
		t.Fatalf("dashboard must surface Attention summary line; got:\n%s", dashOut)
	}
	if !strings.Contains(dashOut, "warning") {
		t.Fatalf("dashboard Attention line must mention 'warning'; got:\n%s", dashOut)
	}
}

// TestDashboard_NoSubstrateWarningsOnCleanInstall pins the substrate
// half of the negative invariant: a clean install must not surface a
// substrate readiness row in a warning state.
//
// 2026-10-23 (HAL-9001 Finding 5): this assertion did not exist
// before, because the Attention-line test it belongs to SKIPPED on
// every run — a clean install carried the five baseline bootstrap
// directives as a spaced-review backlog, which tripped the old guard.
// The directives are no longer review candidates (they are standing
// policy, and their `last_accessed_at` is never written, so that
// backlog could never be cleared), so the invariant is enforceable.
//
// This test never skips: the substrate rows it inspects are read from
// the database and do not depend on the host environment.
func TestDashboard_NoSubstrateWarningsOnCleanInstall(t *testing.T) {
	bin := buildAttentionBin(t)
	ws := t.TempDir()

	dashOut := mustRunAttention(t, bin, ws)

	for _, row := range []string{
		"LLM provider", "Database connected", "Embedding model",
		"Gateway connected", "Working Context", "Open theories",
		"Wake backlog", "Review backlog",
	} {
		if strings.Contains(dashOut, "⚠  "+row) {
			t.Errorf("clean dashboard surfaces a substrate warning on %q:\n%s", row, dashOut)
		}
	}
}

// TestDashboard_AttentionAbsentOnCleanInstall pins the
// negative invariant: when no actionable Doctor warnings
// exist, the dashboard's Attention line must NOT appear
// (clean install = no attention items). Pre-fix this could
// have regressed to "always show Attention even when empty".
//
// The test uses an empty workspace (no seeds) so no memories
// are review-eligible.
//
// 2026-10-23 (HAL-9001 Finding 5): this test previously SKIPPED on
// every run, because a clean install carried the five baseline
// bootstrap directives as a spaced-review backlog and the guard below
// tripped on them. The guard is replaced by the real assertion.
//
// The scheduler row probes the systemd USER bus, which the stripped
// environment this harness runs the binary in cannot reach. That probe
// is an environment artifact, not a substrate signal, so the
// Attention-line invariant is asserted whenever the environment
// contributes no warning of its own. The substrate half of the
// invariant lives in TestDashboard_NoSubstrateWarningsOnCleanInstall,
// which never skips.
func TestDashboard_AttentionAbsentOnCleanInstall(t *testing.T) {
	bin := buildAttentionBin(t)
	ws := t.TempDir()

	dashOut := mustRunAttention(t, bin, ws)

	// The Attention line aggregates WARN-level readiness items. When
	// the environment itself contributes one (the systemd user-bus
	// probe above), the aggregate is not a substrate signal and the
	// invariant cannot be asserted.
	if strings.Contains(dashOut, "⚠  Scheduler running") {
		t.Skip("the scheduler probe cannot reach the systemd user bus in this environment; " +
			"the Attention-line invariant is not assertable here " +
			"(see TestDashboard_NoSubstrateWarningsOnCleanInstall for the substrate half)")
	}
	if strings.Contains(dashOut, "Attention") {
		t.Fatalf("clean dashboard must NOT show Attention summary; got:\n%s", dashOut)
	}
}

// buildAttentionBin builds a fresh mpm binary in a temp dir
// for the dashboard-Attention tests.
func buildAttentionBin(t *testing.T) string {
	t.Helper()
	bin := filepath.Join(t.TempDir(), "mpm-test")
	cmd := stdlibexec.Command("go", "build", "-tags", "fts5", "-o", bin, ".")
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("go build: %v\n%s", err, out)
	}
	return bin
}

// mustRunAttention invokes the binary in the hermetic workspace
// for the Attention-rendering tests. Stderr is captured; the
// exit code is intentionally ignored (doctor may exit non-zero
// when warnings are present — that's the headline semantic).
func mustRunAttention(t *testing.T, bin, ws string, args ...string) string {
	t.Helper()
	cmd := stdlibexec.Command(bin, args...)
	cmd.Env = []string{"MPM_WORKSPACE=" + ws, "PATH=" + lookupTestPath(), "QUIET=1"}
	out, err := cmd.CombinedOutput()
	if err != nil {
		_ = err
	}
	return stripLogNoise(string(out))
}
