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

// TestDashboard_AttentionAbsentOnCleanInstall pins the
// negative invariant: when no actionable Doctor warnings
// exist, the dashboard's Attention line must NOT appear
// (clean install = no attention items). Pre-fix this could
// have regressed to "always show Attention even when empty".
//
// The test uses an empty workspace (no seeds) so no memories
// are review-eligible.
func TestDashboard_AttentionAbsentOnCleanInstall(t *testing.T) {
	bin := buildAttentionBin(t)
	ws := t.TempDir()

	dashOut := mustRunAttention(t, bin, ws)

	// Clean install may still carry some default seed rows
	// that the substrate creates on first run. We tolerate
	// those by checking the Attention line state in
	// relation to actual review backlog state.
	if strings.Contains(dashOut, "⚠  Review backlog") {
		t.Skip("clean install fixture has review backlog from baseline seeds; skipping negative-invariant check")
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
