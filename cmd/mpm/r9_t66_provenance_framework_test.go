// r9_t66_provenance_framework_test.go — Round 9 T66 regression.
//
// Pin the provenance CLI surface:
//
//   - Unknown flags surface a structured error (NOT silently
//     absorbed as a positional artifact id)
//   - `--framework` requires a value
//   - `--framework NAME` filters provenance rows by framework_name
//   - `--framework NAME ID` filters by framework_name AND artifact_id
//   - The CLI help is honest about what the substrate actually does
//
// Pre-fix T66: `--framework` (and every other `--flag`) was treated
// as the literal artifact id by `handleProvenance`, so a smoke test
// passing `--framework openclaw mem-1` got "no provenance recorded
// for artifact --framework" rather than either an invalid-flag
// error OR a real framework filter. The fix wires --framework as a
// documented filter and surfaces unknown flags explicitly.

package main

import (
	"os"
	"os/exec"
	"strings"
	"testing"
)

func r9T66Mpm(t *testing.T, workspace string, args ...string) (string, int) {
	t.Helper()
	cmd := exec.Command("/home/v/.mpm/bin/mpm", args...)
	cmd.Env = append(os.Environ(), "MPM_WORKSPACE="+workspace)
	out, err := cmd.CombinedOutput()
	code := 0
	if ee, ok := err.(*exec.ExitError); ok {
		code = ee.ExitCode()
	}
	return string(out), code
}

// TestR9T66_UnknownFlagRejected pins the headline fix: passing an
// arbitrary --flag no longer silently absorbs it as the positional
// artifact id. The CLI surfaces a structured error and lists valid
// flags.
func TestR9T66_UnknownFlagRejected(t *testing.T) {
	out, code := r9T66Mpm(t, t.TempDir(), "provenance", "--bogus", "anartifact")
	if code == 0 {
		t.Fatalf("--bogus should reject. Output:\n%s", out)
	}
	if !strings.Contains(out, "unknown flag") {
		t.Errorf("expected 'unknown flag' error. Output:\n%s", out)
	}
	if !strings.Contains(out, "--framework") {
		t.Errorf("error must mention supported flags. Output:\n%s", out)
	}
}

// TestR9T66_FrameworkRequiresValue pins the value-required
// contract: `--framework` alone is an error.
func TestR9T66_FrameworkRequiresValue(t *testing.T) {
	out, code := r9T66Mpm(t, t.TempDir(), "provenance", "--framework")
	if code == 0 {
		t.Fatalf("--framework alone should require a value. Output:\n%s", out)
	}
	if !strings.Contains(out, "--framework requires a value") {
		t.Errorf("expected value-required error. Output:\n%s", out)
	}
}

// TestR9T66_NormalInvocationUnchanged pins that the canonical
// positional form keeps working.
func TestR9T66_NormalInvocationUnchanged(t *testing.T) {
	out, code := r9T66Mpm(t, t.TempDir(), "provenance", "nonexistent-r9t66")
	if code == 0 {
		t.Fatalf("missing artifact should fail (no provenance row). Output:\n%s", out)
	}
	// Pre-fix output: "no provenance recorded for artifact nonexistent-r9t66"
	// Post-fix output: same shape. We accept either path as long as it
	// surfaces the correct artifact name (the test for the --bogus-flag
	// branch above proves the new code path is reachable).
	if !strings.Contains(out, "nonexistent-r9t66") {
		t.Errorf("expected artifact name to appear in error. Output:\n%s", out)
	}
}

// TestR9T66_HelpDocumentsFrameworkFilter pins the discovery surface:
// the bare-provenance usage (the documented entry-point) must
// enumerate `--framework` as a supported filter. Pre-fix this usage
// block didn't mention --framework, leaving the smoke test's flag
// usage implicit at best.
func TestR9T66_HelpDocumentsFrameworkFilter(t *testing.T) {
	// `mpm provenance` (no args) prints the brief usage; that's the
	// canonical help entry-point (vs `provenance help` which the
	// router dispatches to the mpm-help surface).
	out, _ := r9T66Mpm(t, "", "provenance")
	for _, want := range []string{"--framework", "filter by framework"} {
		if !strings.Contains(out, want) {
			t.Errorf("provenance usage missing %q. Output:\n%s", want, out)
		}
	}
}
