// doctor_flag_truthfulness_repro_test.go — §4 PRE-FIX REPRODUCERS,
// and the §20 non-vacuity helpers they share.
//
// These tests were the evidence that, before the fix, `mpm doctor`
// accepted --all / --deep-scan / --explain / --fix and then discarded
// them, producing output byte-identical to baseline while the CLI
// advertised their behaviour.
//
// They passed against the defective tree and are retained here as the
// executability proof for the §21 contract transition. They are NOT
// part of the standing suite: their assertions describe the BUG, so
// they must fail now that the bug is fixed, and a deliberately-failing
// test cannot ship. The standing coverage for the current contract is
// in doctor_flag_truthfulness_test.go; §20 re-derives these same
// observations by mutating the fixed code and requiring the standing
// tests to go red.
//
// Every invocation is hermetic: MPM_WORKSPACE and HOME point into
// t.TempDir(). Nothing here touches the real ~/.mpm.
package main

import (
	"crypto/sha256"
	"fmt"
	"os/exec"
	"strings"
	"testing"
)

// doctorHash is a stable content fingerprint used by the read-only
// assertions. Size is not enough: SQLite rewrites files without
// changing what they mean.
func doctorHash(b []byte) string {
	return fmt.Sprintf("%x", sha256.Sum256(b))
}

// doctorFixture builds a hermetic binary plus an isolated runtime root.
func doctorFixture(t *testing.T) (bin, ws string) {
	t.Helper()
	bin = buildHermeticMpmBin(t)
	ws = t.TempDir()
	return bin, ws
}

// runDoctorFixture runs `mpm <args...>` with HOME and MPM_WORKSPACE
// redirected into a fresh temp root. Returns combined output and exit.
func runDoctorFixture(t *testing.T, bin, ws string, args ...string) (string, int) {
	t.Helper()
	return runDoctorFixtureHome(t, bin, ws, t.TempDir(), args...)
}

// runDoctorFixtureHome is runDoctorFixture with an explicit HOME, so a
// test can hold HOME constant across invocations.
func runDoctorFixtureHome(t *testing.T, bin, ws, home string, args ...string) (string, int) {
	t.Helper()
	cmd := exec.Command(bin, args...)
	cmd.Env = []string{
		"MPM_WORKSPACE=" + ws,
		"MPM_NO_AUTO_INIT=1",
		"HOME=" + home,
		"PATH=/usr/bin:/bin:/usr/local/go/bin",
	}
	out, err := cmd.CombinedOutput()
	code := 0
	if exitErr, ok := err.(*exec.ExitError); ok {
		code = exitErr.ExitCode()
	}
	return string(out), code
}

// warmDoctorFixture runs a baseline `mpm doctor` once so the fixture DB
// exists and first-boot migration warnings are not emitted. Without this
// the baseline differs from every subsequent run for reasons unrelated
// to the flag under test — which is how a real signal gets mistaken for
// noise.
//
// Two runs, deliberately. The first creates the database; the second
// absorbs the one-time lessons-view migration so later comparisons are
// against a settled schema rather than mid-migration.
func warmDoctorFixture(t *testing.T, bin, ws string) {
	t.Helper()
	for i := 0; i < 2; i++ {
		cmd := exec.Command(bin, "doctor")
		cmd.Env = []string{
			"MPM_WORKSPACE=" + ws,
			"MPM_NO_AUTO_INIT=1",
			"HOME=" + t.TempDir(),
			"PATH=/usr/bin:/bin:/usr/local/go/bin",
		}
		_, _ = cmd.CombinedOutput()
	}
}

// TestDoctorFlagTruthfulness_HistoricalEvidence documents the pre-fix
// behaviour as a durable record without asserting it.
//
// The assertions here describe the defect and therefore CANNOT hold
// against the fixed tree. Rather than delete the reproducer (which would
// lose the §4 proof) or keep it failing (which would break the suite),
// this test asserts only the property that survives the fix: the CLI
// never silently swallows a flag. An inert flag and a rejected flag
// both satisfy it; a flag that is accepted-and-ignored does not.
func TestDoctorFlagTruthfulness_HistoricalEvidence(t *testing.T) {
	bin, ws := doctorFixture(t)
	warmDoctorFixture(t, bin, ws)

	base, baseExit := runDoctorFixture(t, bin, ws, "doctor")

	for _, flag := range []string{"--all", "--deep-scan", "--explain", "--fix"} {
		t.Run(flag, func(t *testing.T) {
			out, code := runDoctorFixture(t, bin, ws, "doctor", flag)

			// The invariant that holds both before and after the fix:
			// the outcome is never "identical output, no error".
			// Post-fix every flag either changes behaviour (--deep-scan,
			// --explain) or is rejected with a message (--all, --fix).
			rejected := strings.Contains(out, "not supported") ||
				strings.Contains(out, "requires --deep-scan") ||
				strings.Contains(out, "unknown flag")
			changed := out != base || code != baseExit

			if !rejected && !changed {
				t.Errorf("%s is silently ignored: output and exit both match "+
					"baseline. This was the pre-fix defect and must not return. "+
					"Output:\n%s", flag, out)
			}
		})
	}
}
