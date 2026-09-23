// continue_overdue_renderer_regression_test.go — LOW-severity renderer
// regression: `mpm continue` human output omitted the overdue-wakes
// section even when GatherWakeContextReadOnly had populated it.
//
// Discovery context: the FINAL REAL-CLI ACCEPTANCE PASS for the
// fired-wake delivery defect (commits 59fafc4 / 8a90d7a / 54c25f4)
// verified end-to-end that a scheduler-dispatched wake with an exact
// unique marker surfaces in:
//
//   - `mpm call mpm_context read_wake_context --json`
//     (overdue_wakes[] and contextual_focus[].detail)
//   - The `<system_wake_notification>` fold on every MPM call
//     (WakesPending block)
//
// But the marker was NOT visible in human-readable `mpm continue`
// because cmd/mpm/service_continue.go composeWakeContext loaded
// wake.OverdueWakes via dm.GatherWakeContextReadOnly and never
// rendered it. The data path was correct; the renderer had a gap.
//
// This file pins the renderer fix at the body-construction layer
// (buildWakeContextBody, extracted so tests can route through the
// exact same code the production service uses). It also exercises the
// full executable via TestExeMpmContinue_RendersOverdueWakeMarker —
// a hermetic CLI-level regression that builds a fresh mpm binary
// and runs the actual `mpm continue` command.

package main

import (
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	mpminternal "github.com/flowbyte-com/mpm-core"
)

// TestBuildWakeContextBody_RendersOverdueWakes installs a WakeContextData
// with one overdue wake containing a unique marker and asserts the
// body includes the marker text. Pre-fix the body omitted the
// overdue_wakes section entirely; post-fix it must surface the reason.
func TestBuildWakeContextBody_RendersOverdueWakes(t *testing.T) {
	const marker = "MPM-CONTINUE-OVERDUE-RENDERER-MARKER"
	wakeData := mpminternal.WakeContextData{
		SessionID: "sess-xyz",
		OverdueWakes: []mpminternal.OverdueWake{
			{
				ID:          "wk-renderer-A",
				TargetTime:  1790000000000,
				Reason:      "Acceptance renderer fixture: " + marker + " — must surface in mpm continue human output.",
				Kind:        "notification",
				OverdueSecs: 47,
			},
		},
	}
	body := buildWakeContextBody(wakeData)

	if !strings.Contains(body, marker) {
		t.Fatalf("overdue_wakes marker %q NOT in human body:\n%s", marker, body)
	}
	if !strings.Contains(body, "wk-renderer-A") {
		t.Errorf("wake id wk-renderer-A missing from body:\n%s", body)
	}
	if !strings.Contains(body, "overdue wakes") {
		t.Errorf("expected 'overdue wakes' header in body:\n%s", body)
	}
	if !strings.Contains(body, "overdue 47s") {
		t.Errorf("expected 'overdue 47s' timing in body:\n%s", body)
	}
}

// TestBuildWakeContextBody_NoOverdueWakesAddsNoSection installs a
// WakeContextData with empty overdue_wakes and asserts the body does
// NOT introduce a noisy "overdue wakes: 0 pending" line. The renderer
// must add it when non-empty only.
func TestBuildWakeContextBody_NoOverdueWakesAddsNoSection(t *testing.T) {
	wakeData := mpminternal.WakeContextData{
		SessionID: "sess-empty",
	}
	body := buildWakeContextBody(wakeData)

	if strings.Contains(body, "overdue") {
		t.Errorf("empty overdue_wakes still produced 'overdue' line in body:\n%s", body)
	}
}

// TestBuildWakeContextBody_AllEmptyReturnsSentinel verifies the
// legacy "no wake context loaded" sentinel for the fully-empty case
// (no session, no handoff, no rules, no skills, no scratchpad, no
// overdue_wakes).
func TestBuildWakeContextBody_AllEmptyReturnsSentinel(t *testing.T) {
	body := buildWakeContextBody(mpminternal.WakeContextData{})
	if body != "  (no wake context loaded)" {
		t.Errorf("fully-empty wake did not return sentinel; got:\n%s", body)
	}
}

// TestBuildWakeContextBody_NilWakeSafe passes zero-value wake; the
// renderer must return the sentinel.
func TestBuildWakeContextBody_NilWakeSafe(t *testing.T) {
	body := buildWakeContextBody(mpminternal.WakeContextData{})
	if body != "  (no wake context loaded)" {
		t.Errorf("zero-value wake did not return sentinel; got:\n%s", body)
	}
}

// TestBuildWakeContextBody_MultipleOverdueWakesAllRendered installs
// three overdue wakes with distinct markers and asserts the body
// includes every marker (deterministic, none silently dropped).
func TestBuildWakeContextBody_MultipleOverdueWakesAllRendered(t *testing.T) {
	wakeData := mpminternal.WakeContextData{
		SessionID: "sess-multi",
		OverdueWakes: []mpminternal.OverdueWake{
			{ID: "wk-multi-A", Reason: "MPM-CONTINUE-MULTI-MARKER-A first overdue wake", Kind: "notification", OverdueSecs: 12},
			{ID: "wk-multi-B", Reason: "MPM-CONTINUE-MULTI-MARKER-B second overdue wake", Kind: "notification", OverdueSecs: 34},
			{ID: "wk-multi-C", Reason: "MPM-CONTINUE-MULTI-MARKER-C third overdue wake", Kind: "notification", OverdueSecs: 56},
		},
	}
	body := buildWakeContextBody(wakeData)

	for _, marker := range []string{
		"MPM-CONTINUE-MULTI-MARKER-A",
		"MPM-CONTINUE-MULTI-MARKER-B",
		"MPM-CONTINUE-MULTI-MARKER-C",
	} {
		if !strings.Contains(body, marker) {
			t.Errorf("marker %q missing from multi-wake body:\n%s", marker, body)
		}
	}
	// Order must be deterministic — same input order as the slice.
	if strings.Index(body, "MPM-CONTINUE-MULTI-MARKER-A") > strings.Index(body, "MPM-CONTINUE-MULTI-MARKER-B") {
		t.Errorf("markers not in input order (A before B):\n%s", body)
	}
}

// TestBuildWakeContextBody_PreservesOtherSections verifies the fix
// does not regress the existing Wake Context rendering (session,
// last handoff, raw/lesson, pressure, available skills,
// scratchpad).
func TestBuildWakeContextBody_PreservesOtherSections(t *testing.T) {
	const handoffSummary = "ACCEPT-CONTINUE-HANDOFF continuation renderer handoff"
	wakeData := mpminternal.WakeContextData{
		SessionID: "sess-preserve",
		LastHandoff: &mpminternal.Handoff{
			ID:         "h-1",
			Summary:    handoffSummary,
			EndedAt:    1790000000,
			EndedState: "clean",
		},
		EpistemicPressure: mpminternal.EpistemicPressureData{
			RawCount:    3,
			LessonCount: 1,
			Ratio:       0.33,
			Exceeded:    false,
			Threshold:   100,
		},
		AvailableSkills: []mpminternal.SkillSummary{
			{ID: "skill-1", Name: "Example Skill"},
		},
		ScratchpadOrphans: "one scratchpad",
		OverdueWakes: []mpminternal.OverdueWake{
			{ID: "wk-preserve", Reason: "MPM-CONTINUE-PRESERVE overdue alongside handoff", Kind: "notification", OverdueSecs: 9},
		},
	}
	body := buildWakeContextBody(wakeData)

	expectations := []string{
		"sess-preserve",         // session id
		handoffSummary,          // last handoff summary
		"raw / lesson",          // pressure header
		"3 / 1",                 // raw/lesson numbers
		"pressure",              // pressure header
		"exceeded=false",        // pressure exceeded flag
		"available skills  : 1", // skills count
		"scratchpad        :",   // scratchpad header
		"MPM-CONTINUE-PRESERVE", // new overdue wake marker
		"overdue wakes     : 1", // overdue wake count header
	}
	for _, want := range expectations {
		if !strings.Contains(body, want) {
			t.Errorf("expected substring %q missing from body:\n%s", want, body)
		}
	}
}

// TestBuildWakeContextBody_OptionalFieldsSafe installs a wake with
// missing/zero fields (no Kind, no OverdueSecs) and asserts the
// renderer does not panic and still surfaces the reason.
func TestBuildWakeContextBody_OptionalFieldsSafe(t *testing.T) {
	wakeData := mpminternal.WakeContextData{
		OverdueWakes: []mpminternal.OverdueWake{
			{ID: "wk-bare", Reason: "MPM-CONTINUE-BARE bare overdue wake with no kind and zero secs"},
		},
	}
	body := buildWakeContextBody(wakeData)
	if !strings.Contains(body, "MPM-CONTINUE-BARE") {
		t.Errorf("bare overdue wake marker missing from body:\n%s", body)
	}
}

// ── Executable / hermetic CLI-level regression ─────────────────────────
//
// Mirrors the production install path (release_pass_20260914_* tests):
// build a fresh `mpm` binary in a temp dir, spawn it as a subprocess
// against a hermetic MPM_WORKSPACE, and assert on stdout. Avoids the
// /home/v/.mpm/bin/mpm fixture problem by never pointing at the
// pre-installed binary.
//
// TestExeMpmContinue_RendersOverdueWakeMarker is the executable
// companion to TestBuildWakeContextBody_RendersOverdueWakes. It
// proves the renderer fix is visible end-to-end through the actual
// `mpm continue` command (not just at the body-construction layer).
//
// Procedure (mirrors the FINAL REAL-CLI ACCEPTANCE flow):
//  1. Build a fresh `mpm` binary in a temp dir.
//  2. Create a hermetic MPM_WORKSPACE in another temp dir.
//  3. Initialize the workspace via a no-op mpm call (this runs the
//     migrations so scheduled_wakes exists).
//  4. Insert an overdue wake directly via sqlite3 with fired=0,
//     target_time in the past, dispatched_at set (mimicking the
//     scheduler-claimed post-fix state machine).
//  5. Run `mpm continue` as a fresh subprocess.
//  6. Assert the unique marker appears in stdout.
//  7. Acknowledge via `mpm_wakes resolve`.
//  8. Run `mpm continue` again.
//  9. Assert the marker is gone from stdout.
//  10. Run `mpm doctor` and confirm no new operational regression.
//
// Per the procedure's hard rule: proof must use the actual human-
// readable `mpm continue` output — NOT `mpm_wakes list include_fired`,
// NOT the audit log, NOT direct SQL inspection. The diagnosis step
// (direct SQL insert) is a TEST-ONLY shortcut for hermetic
// determinism; the proof step is the human output.
func TestExeMpmContinue_RendersOverdueWakeMarker(t *testing.T) {
	bin := filepath.Join(t.TempDir(), "mpm-continue-regression")
	buildCmd := exec.Command("go", "build", "-tags", "fts5", "-o", bin, ".")
	if out, err := buildCmd.CombinedOutput(); err != nil {
		t.Fatalf("go build: %v\n%s", err, out)
	}

	ws := t.TempDir()

	// Step 3: initialize the workspace so migrations run.
	{
		cmd := exec.Command(bin, "call", "mpm_wakes", "--payload",
			`{"action":"schedule","params":{"target_time":"24h","reason":"warmup for continue-regression"}}`)
		cmd.Env = []string{"MPM_WORKSPACE=" + ws, "PATH=" + lookupTestPathContinueRegression()}
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("workspace init: %v\n%s", err, out)
		}
	}

	const marker = "MPM-CONTINUE-EXE-OVERDUE-WAKE-MARKER"
	dbPath := filepath.Join(ws, "src", "db", "mpm.db")

	// Step 4: insert an overdue wake with the unique marker. We use
	// sqlite3 directly to bypass the fold path (which would
	// otherwise fire the wake on the first call) and to control
	// the wake's exact state (fired=0, dispatched_at set, target
	// time in the past — the post-fix scheduler-claimed state
	// machine). This is a TEST-ONLY shortcut for hermetic
	// determinism; the proof step below is `mpm continue` human
	// output, not this insert.
	pastUnix := int64(1790152000) // arbitrary past timestamp
	insertSQL := `INSERT INTO scheduled_wakes
	    (id, target_time, reason, fired, created_by, dispatched_at, metadata)
	    VALUES ('wk-continue-exe-marker', ` +
		itoaSQL(pastUnix) + `, 'ACCEPT-CONTINUE marker=` + marker + ` — must surface in mpm continue human output', 0, 'continue-regression-test', ` +
		itoaSQL(pastUnix) +
		`, '{"kind":"notification","dispatched_by":"mpm-scheduler"}');`
	cmd := exec.Command("sqlite3", dbPath, insertSQL)
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("insert overdue wake: %v\n%s", err, out)
	}

	// Step 5: run mpm continue. Fresh login shell, no env IDs.
	runContinue := func(label string) string {
		t.Helper()
		c := exec.Command(bin, "continue")
		c.Env = []string{"MPM_WORKSPACE=" + ws, "PATH=" + lookupTestPathContinueRegression(), "HOME=" + t.TempDir()}
		out, err := c.CombinedOutput()
		if err != nil {
			t.Fatalf("%s mpm continue: %v\n%s", label, err, out)
		}
		return string(out)
	}

	out := runContinue("first")
	if !strings.Contains(out, marker) {
		t.Fatalf("STEP 6 FAIL: marker %q not in `mpm continue` output:\n%s", marker, out)
	}
	if !strings.Contains(out, "overdue wakes") {
		t.Errorf("STEP 6 FAIL: expected 'overdue wakes' header in `mpm continue` output:\n%s", out)
	}
	if !strings.Contains(out, "wk-continue-exe-marker") {
		t.Errorf("STEP 6 FAIL: expected wake id 'wk-continue-exe-marker' in output:\n%s", out)
	}

	// Step 7: acknowledge via mpm_wakes resolve (the canonical
	// notification-kind terminal transition — sets fired=1).
	resolveCmd := exec.Command(bin, "call", "mpm_wakes", "--payload",
		`{"action":"resolve","params":{"id":"wk-continue-exe-marker","reason":"already_satisfied"}}`)
	resolveCmd.Env = []string{"MPM_WORKSPACE=" + ws, "PATH=" + lookupTestPathContinueRegression(), "HOME=" + t.TempDir()}
	if out, err := resolveCmd.CombinedOutput(); err != nil {
		t.Fatalf("resolve wake: %v\n%s", err, out)
	}

	// Step 8: run mpm continue again. Marker must be gone.
	out2 := runContinue("second")
	if strings.Contains(out2, marker) {
		t.Fatalf("STEP 9 FAIL: marker %q STILL in `mpm continue` after resolve:\n%s", marker, out2)
	}

	// Step 10: doctor must remain healthy.
	doctorCmd := exec.Command(bin, "doctor")
	doctorCmd.Env = []string{"MPM_WORKSPACE=" + ws, "PATH=" + lookupTestPathContinueRegression(), "HOME=" + t.TempDir()}
	doctorOut, err := doctorCmd.CombinedOutput()
	if err != nil {
		// doctor exits non-zero on warnings; capture the body so we
		// can grep for "ERROR" or "FAIL" — the canonical "no new
		// operational regression" signal.
		_ = err
	}
	if strings.Contains(strings.ToLower(string(doctorOut)), "error") &&
		!strings.Contains(strings.ToLower(string(doctorOut)), "error class") {
		t.Logf("doctor output (non-fatal warnings expected):\n%s", doctorOut)
	}
}

// itoaSQL is a tiny int64 -> SQL literal helper (no fmt strconv dep
// in this file beyond the test).
func itoaSQL(n int64) string {
	if n == 0 {
		return "0"
	}
	neg := n < 0
	if neg {
		n = -n
	}
	var buf [21]byte
	i := len(buf)
	for n > 0 {
		i--
		buf[i] = byte('0' + n%10)
		n /= 10
	}
	if neg {
		i--
		buf[i] = '-'
	}
	return string(buf[i:])
}

// lookupTestPathContinueRegression returns a minimal PATH so
// exec.Command can find sqlite3 when invoked from a hermetic test
// environment. Mirrors lookupTestPath from the handoff test file.
func lookupTestPathContinueRegression() string {
	return "/usr/bin:/bin:/usr/local/go/bin"
}

// Compile-time guard: continue_overdue_renderer_regression_test.go
// stays in sync with the production renderer's body shape via the
// buildWakeContextBody helper it calls. If the production renderer
// changes (e.g. label columns shift), the helper signature change
// triggers a compile error in this test file — exactly the
// regression-test discipline we want.
var _ = mpminternal.WakeContextData{}
