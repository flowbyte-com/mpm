// release_pass_20260914_handoff_semantics_test.go — Regression
// coverage for the 2026-09-14 final-last-mile handoff semantics
// investigation.
//
// The previous release-pass report claimed "Dashboard Last
// Session = latest unread handoff; Wake context = same semantic."
// Built-binary production smoke disproved this: with all 3
// production handoffs marked read, the dashboard correctly
// shows "No previous handoff/session available" while
// `mpm ops wake` still renders the latest handoff (regardless
// of read state).
//
// Investigation: `mpm ops wake` is the CLI browse surface.
// It calls `dm.GetLatestHandoff()` (latest regardless of read)
// so the operator can review the previous session's commitments
// and unresolved questions even after wake-context consumed the
// handoff. The substrate's `mpm call read_wake_context` path
// (the agent-facing consumption path) marks the handoff as read
// and only surfaces unread ones. These are two intentional
// semantics:
//   - mpm call read_wake_context  → consume semantics
//                                    (latest unread, marks read)
//   - mpm ops wake (CLI)           → browse semantics
//                                    (latest regardless of read)
//
// The dashboard "Last Session" mirrors the wake-context
// consumption semantic: it shows the operator "what should I
// look at next?" (an unread handoff). The CLI's `mpm ops wake`
// answers a different question: "what was the previous session
// about?" (history).
//
// This test pins both behaviors. Pre-fix the report conflated
// them; the regressions make the distinction explicit and
// implementation-backed.

package main

import (
	stdlibexec "os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// TestHandoffSemantics_DashboardVsOpsWake is the headline
// regression for the intentional distinction. The fixture
// contains exactly one handoff which is then marked read.
// Pre-fix this assertion failed because the dashboard's
// Last Session was reported to use the same semantic as
// `mpm ops wake`, but the built-binary shows the dashboard
// correctly says "No previous handoff/session available"
// while `mpm ops wake` still surfaces the handoff for
// browsing.
func TestHandoffSemantics_DashboardVsOpsWake(t *testing.T) {
	bin := buildHandoffSemanticsBin(t)
	ws := t.TempDir()

	// Seed a single handoff.
	seedHandoff(t, bin, ws, "session alpha — first handoff")
	// Mark it read.
	markAllHandoffsRead(t, bin, ws)

	// Sanity: handoff list --unread returns zero.
	unreadOut := mustRunHandoffBin(t, bin, ws, "handoff", "list", "--unread")
	if !strings.Contains(unreadOut, "No handoffs") {
		t.Fatalf("after marking all read, --unread must report No handoffs; got:\n%s", unreadOut)
	}

	// Dashboard Last Session must say "No previous handoff" —
	// mirrors the wake-context consume semantic.
	dashOut := mustRunHandoffBin(t, bin, ws)
	if !strings.Contains(dashOut, "No previous handoff") {
		t.Fatalf("dashboard Last Session must reflect the wake-context consume semantic (latest unread); got:\n%s", dashOut)
	}
	if strings.Contains(dashOut, "session alpha") {
		t.Fatalf("dashboard must NOT surface a read handoff under Last Session; got:\n%s", dashOut)
	}

	// `mpm ops wake` must still surface the handoff — the CLI
	// browse semantic intentionally shows the latest regardless
	// of read state.
	wakeOut := mustRunHandoffBin(t, bin, ws, "ops", "wake")
	if !strings.Contains(wakeOut, "session alpha") {
		t.Fatalf("`mpm ops wake` must surface the latest handoff regardless of read state (browse semantic); got:\n%s", wakeOut)
	}
}

// TestHandoffSemantics_DashboardReflectsWakeConsume pins the
// dashboard's behavior against the substrate's wake-context
// consume path. With an unread handoff present, the dashboard
// must surface it (the operator's "what should I look at next"
// question); after marking read, the dashboard must NOT surface
// it (the consumption path has been satisfied).
func TestHandoffSemantics_DashboardReflectsWakeConsume(t *testing.T) {
	bin := buildHandoffSemanticsBin(t)
	ws := t.TempDir()

	// Seed two handoffs.
	seedHandoff(t, bin, ws, "first session")
	seedHandoff(t, bin, ws, "second session")

	// With both unread, the dashboard must surface at least one
	// of them (the latest unread).
	dashOut := mustRunHandoffBin(t, bin, ws)
	hasFirst := strings.Contains(dashOut, "first session")
	hasSecond := strings.Contains(dashOut, "second session")
	if !hasFirst && !hasSecond {
		t.Fatalf("with unread handoffs present, dashboard Last Session must surface at least one; got:\n%s", dashOut)
	}

	// Mark all read — dashboard must clear Last Session.
	markAllHandoffsRead(t, bin, ws)

	dashOut2 := mustRunHandoffBin(t, bin, ws)
	if !strings.Contains(dashOut2, "No previous handoff") {
		t.Fatalf("after marking all handoffs read, dashboard Last Session must clear; got:\n%s", dashOut2)
	}
	if strings.Contains(dashOut2, "first session") || strings.Contains(dashOut2, "second session") {
		t.Fatalf("dashboard must NOT surface read handoffs under Last Session; got:\n%s", dashOut2)
	}
}

// TestHandoffSemantics_OpsWakeUsesLatestRegardlessOfRead pins
// the CLI browse semantic: `mpm ops wake` continues to surface
// the latest handoff even when it has been marked read. This
// is the intentional architecture (handlers_session.go:142-148).
func TestHandoffSemantics_OpsWakeUsesLatestRegardlessOfRead(t *testing.T) {
	bin := buildHandoffSemanticsBin(t)
	ws := t.TempDir()

	seedHandoff(t, bin, ws, "alpha content")
	seedHandoff(t, bin, ws, "beta content")
	markAllHandoffsRead(t, bin, ws)

	wakeOut := mustRunHandoffBin(t, bin, ws, "ops", "wake")
	if !strings.Contains(wakeOut, "beta content") {
		t.Fatalf("`mpm ops wake` must surface latest handoff (browse semantic) regardless of read state; got:\n%s", wakeOut)
	}
}

// buildHandoffSemanticsBin builds a fresh mpm binary in a temp
// dir for the handoff-semantics tests.
func buildHandoffSemanticsBin(t *testing.T) string {
	t.Helper()
	bin := filepath.Join(t.TempDir(), "mpm-test")
	cmd := stdlibexec.Command("go", "build", "-tags", "fts5", "-o", bin, ".")
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("go build: %v\n%s", err, out)
	}
	return bin
}

// seedHandoff writes a handoff via the substrate write action
// and returns the generated id.
func seedHandoff(t *testing.T, bin, ws, summary string) string {
	t.Helper()
	payload := `{"action":"write","params":{"summary":"` + summary + `"}}`
	cmd := stdlibexec.Command(bin, "call", "mpm_handoff", "--payload", payload)
	cmd.Env = []string{"MPM_WORKSPACE=" + ws, "PATH=" + lookupTestPath()}
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("seed handoff: %v\n%s", err, out)
	}
	return string(out)
}

// markAllHandoffsRead reads each handoff with mark_read=true.
// Uses the latest-unread read path so the read marker
// propagates to all rows.
func markAllHandoffsRead(t *testing.T, bin, ws string) {
	t.Helper()
	for {
		payload := `{"action":"read","params":{"unread":true,"mark_read":true}}`
		cmd := stdlibexec.Command(bin, "call", "mpm_handoff", "--payload", payload)
		cmd.Env = []string{"MPM_WORKSPACE=" + ws, "PATH=" + lookupTestPath()}
		out, err := cmd.CombinedOutput()
		if err != nil {
			t.Fatalf("mark read iteration: %v\n%s", err, out)
		}
		s := stripLogNoise(string(out))
		if !strings.Contains(s, `"success":true`) {
			t.Fatalf("mark read returned non-success: %s", s)
		}
		// Stop when no more unread handoffs.
		if strings.Contains(s, `"handoff":null`) || !strings.Contains(s, `"id":"`) {
			return
		}
	}
}

// mustRunHandoffBin invokes the binary in the hermetic
// workspace. Stderr is captured but does not fail the call —
// ops wake exits non-zero in some configurations and the
// fixture state matters, not the exit code.
func mustRunHandoffBin(t *testing.T, bin, ws string, args ...string) string {
	t.Helper()
	cmd := stdlibexec.Command(bin, args...)
	cmd.Env = []string{"MPM_WORKSPACE=" + ws, "PATH=" + lookupTestPath(), "QUIET=1"}
	out, err := cmd.CombinedOutput()
	if err != nil {
		// Don't fail — see comment above.
		_ = err
	}
	return stripLogNoise(string(out))
}
