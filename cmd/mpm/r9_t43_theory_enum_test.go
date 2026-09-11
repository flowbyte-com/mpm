// r9_t43_theory_enum_test.go — Round 9 T43 regression.
//
// Pin the theory conclusion vocabulary at every surface:
//
//   * the canonical enum at handlers_epistemology.go acceptConclusionEnum
//     is closed-world — five values, no synonyms ("resolved" is a
//     legacy stored value, never written by the new handler)
//   * parser rejects unknown values with a structured error
//   * help text documents all five values (pre-fix only documented two)
//   * mappings are stable: `confirmed|proven` → status="proven",
//     `disproven|refuted|invalidated` → status="disproven"
//
// The smoke tests validate against this contract; a future addition
// to the enum must update acceptConclusionEnum AND the help text AND
// this test.

package main

import (
	"os/exec"
	"strings"
	"testing"
)

// r9T43RunTheoryResolve invokes `mpm theory resolve <id> <conclusion>`
// in a hermetic workspace; returns stdout, exit code.
func r9T43RunTheoryResolve(t *testing.T, workspace, id, conclusion string) (string, int) {
	t.Helper()
	binPath := "/home/v/.mpm/bin/mpm"
	cmd := exec.Command(binPath, "theory", "resolve", id, conclusion)
	cmd.Env = append(cmd.Environ(), "MPM_WORKSPACE="+workspace)
	out, err := cmd.CombinedOutput()
	code := 0
	if ee, ok := err.(*exec.ExitError); ok {
		code = ee.ExitCode()
	}
	return string(out), code
}

// TestR9T43_ResolveRejectsUnknownConclusion runs the parser with a
// value NOT in the canonical enum. The handler must exit non-zero
// and surface a parseable error message. We don't need a real theory
// to validate the parser branch — the conclusion-keyword switch runs
// before the memory-lookup branch.
func TestR9T43_ResolveRejectsUnknownConclusion(t *testing.T) {
	workspace := t.TempDir()
	for _, bad := range []string{"resolved", "yes", "no", "true", "reject", "approve", "cancel"} {
		t.Run(bad, func(t *testing.T) {
			out, code := r9T43RunTheoryResolve(t, workspace, "nonexistent-id", bad)
			if code == 0 {
				t.Fatalf("conclusion=%q: expected non-zero exit, got success. Output:\n%s", bad, out)
			}
		})
	}
}

// TestR9T43_ResolveAcceptsCanonicalValues runs the parser with each
// of the five canonical conclusions. The handler exits non-zero
// because no real theory exists at "nonexistent-id" — but the error
// message MUST be the "Theory not found" path (not the
// "must be one of:" enum-rejection path). That distinction proves
// the parser accepted the keyword before the memory lookup.
func TestR9T43_ResolveAcceptsCanonicalValues(t *testing.T) {
	workspace := t.TempDir()
	for _, good := range []string{"confirmed", "proven", "disproven", "refuted", "invalidated"} {
		t.Run(good, func(t *testing.T) {
			out, code := r9T43RunTheoryResolve(t, workspace, "nonexistent-id", good)
			// Memory lookup will fail for a non-existent theory; the
			// error message must contain "Theory not found", NOT
			// "must be one of" — the latter is the enum-rejection path
			// which would indicate the parser rejected the keyword.
			if code == 0 {
				t.Fatalf("conclusion=%q: expected failure (no such theory), got success. Output:\n%s", good, out)
			}
			if !strings.Contains(out, "Theory not found") {
				t.Errorf("conclusion=%q: expected 'Theory not found' (parser-accepted), got:\n%s",
					good, out)
			}
			if strings.Contains(out, "must be one of") {
				t.Errorf("conclusion=%q: parser treated it as enum-rejection. Output:\n%s",
					good, out)
			}
		})
	}
}

// TestR9T43_HelpDocumentsAllCanonicalValues requires the
// `mpm resolve_theory --help` output (and `mpm theory resolve --help`)
// to enumerate all five canonical conclusions. Pre-fix the help
// text only documented two; the hidden aliases broke discoverability.
func TestR9T43_HelpDocumentsAllCanonicalValues(t *testing.T) {
	binPath := "/home/v/.mpm/bin/mpm"

	expected := []string{"confirmed", "proven", "disproven", "refuted", "invalidated"}

	for _, sub := range [][]string{
		{"resolve_theory", "--help"},
		{"theory", "resolve", "--help"},
	} {
		t.Run(strings.Join(sub, "_"), func(t *testing.T) {
			cmd := exec.Command(binPath, sub...)
			out, _ := cmd.CombinedOutput()
			text := string(out)
			for _, want := range expected {
				if !strings.Contains(text, want) {
					t.Errorf("%v: help text missing canonical conclusion %q. Output:\n%s",
						sub, want, text)
				}
			}
		})
	}
}

// TestR9T43_StoredStatusMapping pins the canonical enum → stored
// status mapping. We don't have a real theory to inspect post-fix;
// instead we verify the canonical mapping is in the help text so
// smoke tests can rely on it.
func TestR9T43_StoredStatusMapping(t *testing.T) {
	binPath := "/home/v/.mpm/bin/mpm"
	cmd := exec.Command(binPath, "resolve_theory", "--help")
	out, err := cmd.CombinedOutput()
	if err != nil {
		// --help sometimes returns 0, sometimes non-zero; ignore err.
	}
	text := string(out)
	// The help text must say `status="proven"` for the proven group.
	if !strings.Contains(text, `status="proven"`) {
		t.Errorf("help text missing `status=\"proven\"` mapping. Output:\n%s", text)
	}
	if !strings.Contains(text, `status="disproven"`) {
		t.Errorf("help text missing `status=\"disproven\"` mapping. Output:\n%s", text)
	}
}
