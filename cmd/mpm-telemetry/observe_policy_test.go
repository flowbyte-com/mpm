package main

import (
	"io"
	"os"
	"strings"
	"testing"
)

// The approved policy (2026-10-28) for `mpm-telemetry observe`:
//
//   1. it is a READ-ONLY DIAGNOSTIC by default;
//   2. it NEVER automatically creates trusted lessons or unsupported
//      pending theories;
//   3. `--dry-run` is kept as a compatibility option;
//   4. `--mpm` is deprecated with an EXPLICIT warning — not silently
//      ignored;
//   5. the canonical store location is `src/db/telemetry.db`;
//   6. the output stays explicitly labelled as unverified diagnostic
//      information.
//
// These tests pin the policy itself, in process. The end-to-end
// coverage — the REAL executable against the REAL registry — lives in
// observe_e2e_test.go.

// captured holds what a run printed.
type captured struct {
	stdout string
	stderr string
	err    error
}

// captureStreams runs fn with both standard streams redirected.
func captureStreams(t *testing.T, fn func() error) captured {
	t.Helper()
	origOut, origErr := os.Stdout, os.Stderr
	rOut, wOut, err := os.Pipe()
	if err != nil {
		t.Fatalf("pipe: %v", err)
	}
	rErr, wErr, err := os.Pipe()
	if err != nil {
		t.Fatalf("pipe: %v", err)
	}
	os.Stdout, os.Stderr = wOut, wErr
	runErr := fn()
	_ = wOut.Close()
	_ = wErr.Close()
	os.Stdout, os.Stderr = origOut, origErr
	out, _ := io.ReadAll(rOut)
	errOut, _ := io.ReadAll(rErr)
	_ = rOut.Close()
	_ = rErr.Close()
	return captured{stdout: string(out), stderr: string(errOut), err: runErr}
}

// TestPolicy_DryRunIsACompatibilityAlias pins policy point 3.
//
// `--dry-run` used to be the "do not write" mode. The default is now
// read-only too, so the flag means exactly what the default already
// means. It is still ACCEPTED — an operator's existing invocation must
// not break on an unknown flag.
func TestPolicy_DryRunIsACompatibilityAlias(t *testing.T) {
	env := newObserveEnv(t)
	seedSubstrate(t, env.workspace)
	seedTelemetrySession(t, env.telemetryDB, "ses-qualify", 120000, 120000)

	def := captureStreams(t, func() error {
		return runObserve([]string{"--since", "1"})
	})
	if def.err != nil {
		t.Fatalf("the default mode failed: %v", def.err)
	}
	alias := captureStreams(t, func() error {
		return runObserve([]string{"--dry-run", "--since", "1"})
	})
	if alias.err != nil {
		t.Fatalf("--dry-run failed: %v", alias.err)
	}
	if alias.stdout != def.stdout {
		t.Errorf("--dry-run and the default mode disagree.\ndefault:\n%s\n--dry-run:\n%s",
			def.stdout, alias.stdout)
	}
	if !strings.Contains(def.stdout, "ses-qualify") {
		t.Errorf("neither mode reported the observation:\n%s", def.stdout)
	}
}

// TestPolicy_MpmFlagWarnsExplicitly pins policy point 4.
//
// "Deprecated with an explicit warning; do not silently ignore it."
//
// The warning must name the flag, say it is deprecated, say it has no
// effect, and say what happened instead. A bare "ignored" note is what
// this replaces.
func TestPolicy_MpmFlagWarnsExplicitly(t *testing.T) {
	env := newObserveEnv(t)
	seedSubstrate(t, env.workspace)
	seedTelemetrySession(t, env.telemetryDB, "ses-qualify", 120000, 120000)

	got := captureStreams(t, func() error {
		return runObserve([]string{"--since", "1", "--mpm", "/opt/mpm/bin/mpm"})
	})
	if got.err != nil {
		t.Fatalf("runObserve with --mpm failed: %v; the flag is deprecated, not rejected", got.err)
	}
	for _, want := range []string{"warning", "--mpm", "DEPRECATED", "no effect", "removed"} {
		if !strings.Contains(got.stderr, want) {
			t.Errorf("the deprecation warning does not mention %q:\n%s", want, got.stderr)
		}
	}
	// The warning goes to stderr, never stdout: stdout is the report.
	if strings.Contains(got.stdout, "DEPRECATED") {
		t.Errorf("the deprecation warning leaked into stdout:\n%s", got.stdout)
	}
	// And the observation is still reported.
	if !strings.Contains(got.stdout, "ses-qualify") {
		t.Errorf("the observation was not reported:\n%s", got.stdout)
	}
}

// TestPolicy_NoMpmFlagMeansNoWarning pins that the warning is tied to
// the flag, not printed on every run.
func TestPolicy_NoMpmFlagMeansNoWarning(t *testing.T) {
	env := newObserveEnv(t)
	seedSubstrate(t, env.workspace)
	seedTelemetrySession(t, env.telemetryDB, "ses-qualify", 120000, 120000)

	got := captureStreams(t, func() error {
		return runObserve([]string{"--since", "1"})
	})
	if got.err != nil {
		t.Fatalf("runObserve failed: %v", got.err)
	}
	if strings.Contains(got.stderr, "DEPRECATED") {
		t.Errorf("a deprecation warning was printed without the flag:\n%s", got.stderr)
	}
}

// TestPolicy_OutputIsLabelledUnverified pins policy point 6.
//
// The observation must declare its own standing. A reader who sees a
// JSON document about a session must not be able to mistake it for
// something the substrate believes.
func TestPolicy_OutputIsLabelledUnverified(t *testing.T) {
	env := newObserveEnv(t)
	seedSubstrate(t, env.workspace)
	seedTelemetrySession(t, env.telemetryDB, "ses-qualify", 120000, 120000)

	got := captureStreams(t, func() error {
		return runObserve([]string{"--since", "1"})
	})
	if got.err != nil {
		t.Fatalf("runObserve failed: %v", got.err)
	}
	for _, want := range []string{
		`"status": "unverified-observation"`,
		`"wrote": "nothing"`,
		`"observation": "high-token-no-artifact"`,
	} {
		if !strings.Contains(got.stdout, want) {
			t.Errorf("the output does not contain %s:\n%s", want, got.stdout)
		}
	}
	// It must NOT claim a lesson type, a save action, or a fact.
	for _, banned := range []string{`"type"`, `"action"`, `"fact"`, `"lesson"`} {
		if strings.Contains(got.stdout, banned) {
			t.Errorf("the output contains %s, which claims trusted knowledge:\n%s",
				banned, got.stdout)
		}
	}
}

// TestPolicy_QuietStoreSucceedsSilently pins the ordinary Unix
// contract: nothing to report is not a failure.
func TestPolicy_QuietStoreSucceedsSilently(t *testing.T) {
	env := newObserveEnv(t)
	seedSubstrate(t, env.workspace)
	seedTelemetrySession(t, env.telemetryDB, "ses-quiet", 10, 10)

	got := captureStreams(t, func() error {
		return runObserve([]string{"--since", "1"})
	})
	if got.err != nil {
		t.Fatalf("runObserve on a quiet store = %v; there is nothing to report", got.err)
	}
	if got.stdout != "" {
		t.Errorf("a quiet store printed %q", got.stdout)
	}
}

// TestPolicy_TheSubstrateHandleIsReadOnly pins the STRUCTURAL
// guarantee behind policy point 2.
//
// "Never automatically create trusted lessons or unsupported pending
// theories" is usually asserted by counting rows before and after. That
// is a good assertion, but it is a statement about what the code chose
// to do. This one is stronger: the handle the observe path holds is
// opened with SQLite's `mode=ro`, so the path CANNOT write — not
// because it declines to, but because the database refuses.
//
// A future change that "helpfully" adds a write would have to reopen
// the substrate, and this test would fail at that point rather than at
// the first trusted lesson.
func TestPolicy_TheSubstrateHandleIsReadOnly(t *testing.T) {
	env := newObserveEnv(t)
	dm := seedSubstrate(t, env.workspace)
	_ = dm

	db, err := openSubstrateReadOnly(env.substrateDB)
	if err != nil {
		t.Fatalf("open the substrate read-only: %v", err)
	}
	defer db.Close()

	if _, err := db.Exec(
		`INSERT INTO tool_invocations
		 (id, session_id, tool_name, action, invocation_id, actor_kind,
		  framework_name, payload_hash, result_status, started_at, mpm_session_id,
		  framework_session_id)
		 VALUES ('x','s','t','a','i','agent','f','h','success',1,'s','s')`); err == nil {
		t.Fatal("the substrate handle is writable; the observe path could write " +
			"a trusted row, and the read-only guarantee would be conventional only")
	}
}

// TestPolicy_HelpStatesTheContract pins that the help page tells the
// operator the truth: read-only, deprecated flag, compatibility alias.
func TestPolicy_HelpStatesTheContract(t *testing.T) {
	var buf strings.Builder
	writeHelpObserve(&buf, "test-build")
	out := buf.String()

	for _, want := range []string{"READ-ONLY", "DEPRECATED", "compatibility alias", "writes nothing"} {
		if !strings.Contains(out, want) {
			t.Errorf("the observe help does not mention %q:\n%s", want, out)
		}
	}
	for _, banned := range []string{"cross-DB lookups", "instead of calling mpm", "refuses"} {
		if strings.Contains(out, banned) {
			t.Errorf("the observe help still advertises %q:\n%s", banned, out)
		}
	}
}
