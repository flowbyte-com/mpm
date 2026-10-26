// provenance_env_isolation_test.go — keep the mpmcli package's
// environment-sensitive tests independent of the shell that runs them.
//
// ActiveContextFromEnv reads the caller's identity from the environment:
// MPM_PROVENANCE_FRAMEWORK (canonical), MPM_FRAMEWORK (legacy fallback),
// MPM_ACTIVE_MODE, MPM_ACTIVE_PERSONA and MPM_SESSION_ID. A developer
// shell, an agent harness or a CI job may export any of those — this
// repository's own OpenCode environment exports
// MPM_PROVENANCE_FRAMEWORK=opencode — and a test that asserts a DEFAULT
// then fails against a correct binary.
//
// This is a test-environment defect, not a product defect: the same test
// passes on a clean machine, and CI (which exports nothing) has always
// been green while local runs inside an agent session were red.
//
// PR #89 established the same discipline for cmd/mpm
// (cmd/mpm/provenance_env_isolation_test.go). That helper cannot be
// imported here: cmd/mpm and internal/core are separate Go modules, so
// this package carries its own copy.
//
// Tests that assert a default, or that supply their own identity, call
// clearAmbientProvenanceEnv first so the starting state is defined by the
// test rather than by whoever launched it.
package mpmcli

import "testing"

// ambientProvenanceEnvVars is the COMPLETE set of environment variables
// that the production code in this package (and the config resolver it
// delegates to) reads.
//
// It is deliberately a single list rather than a set assembled per test:
// the regression test below asserts that this list and the production
// readers cannot drift apart, so a variable added to the product without
// being added here fails the build instead of silently reintroducing
// ambient-environment sensitivity.
var ambientProvenanceEnvVars = []string{
	// Caller identity — read by ActiveContextFromEnv.
	"MPM_PROVENANCE_FRAMEWORK",
	"MPM_FRAMEWORK",
	"MPM_ACTIVE_MODE",
	"MPM_ACTIVE_PERSONA",
	"MPM_SESSION_ID",
	// Runtime root — read by config.GetWorkspace, which
	// mpmcli.ResolveWorkspace delegates to.
	"MPM_WORKSPACE",
}

// clearAmbientProvenanceEnv pins the caller-identity environment to
// "no host identity supplied" for the duration of one test.
//
// It deliberately does not touch MPM_PROVENANCE_INVOCATION_ID: tests
// that assert an explicit correlation id set that themselves, after
// calling this helper. t.Setenv restores every value at test cleanup.
func clearAmbientProvenanceEnv(t *testing.T) {
	t.Helper()
	for _, key := range ambientProvenanceEnvVars {
		t.Setenv(key, "")
	}
}

// TestClearAmbientProvenanceEnv_CoversEveryReader is the regression test
// for the isolation itself: the list of variables the helper clears must
// cover every variable the production code in this package reads.
//
// If a new MPM_* variable is added to ActiveContextFromEnv or to the
// workspace resolver and not to ambientProvenanceEnvVars, this test
// fails — which is the point. A helper that silently under-covers is
// worse than no helper, because it looks like isolation while leaving a
// hole for the next ambient variable.
func TestClearAmbientProvenanceEnv_CoversEveryReader(t *testing.T) {
	// Every variable the production code reads. Kept as a literal list so
	// that adding a reader without updating this list is a compile-visible
	// omission rather than a silent behaviour change.
	readers := []string{
		"MPM_PROVENANCE_FRAMEWORK",
		"MPM_FRAMEWORK",
		"MPM_ACTIVE_MODE",
		"MPM_ACTIVE_PERSONA",
		"MPM_SESSION_ID",
		"MPM_WORKSPACE",
	}

	covered := make(map[string]bool, len(ambientProvenanceEnvVars))
	for _, key := range ambientProvenanceEnvVars {
		covered[key] = true
	}
	for _, key := range readers {
		if !covered[key] {
			t.Errorf("clearAmbientProvenanceEnv does not clear %q; a test that "+
				"asserts a default would inherit it from the ambient shell",
				key)
		}
	}
	for _, key := range ambientProvenanceEnvVars {
		if !contains(readers, key) {
			t.Errorf("ambientProvenanceEnvVars lists %q but no production code "+
				"in this package reads it; the list has drifted", key)
		}
	}
}

// TestClearAmbientProvenanceEnv_ActuallyClears proves the helper works,
// rather than merely compiling. It pollutes every variable with a
// recognisable value, calls the helper, and asserts that the production
// reader no longer sees any of them.
//
// Without this, a helper that misspelled one variable name would pass
// every other test in the package and still leave the suite ambient.
func TestClearAmbientProvenanceEnv_ActuallyClears(t *testing.T) {
	for _, key := range ambientProvenanceEnvVars {
		t.Setenv(key, "ambient-"+key)
	}
	// A pre-flight check: the pollution really is visible before the
	// helper runs, so a later "not visible" cannot be vacuously true.
	t.Setenv("MPM_PROVENANCE_FRAMEWORK", "ambient-pollution")
	if got := ActiveContextFromEnv().FrameworkName; got != "ambient-pollution" {
		t.Fatalf("pre-flight: FrameworkName = %q, want %q; the pollution did "+
			"not take effect, so the rest of this test proves nothing", got,
			"ambient-pollution")
	}

	clearAmbientProvenanceEnv(t)

	ac := ActiveContextFromEnv()
	if ac.FrameworkName != "mcp" {
		t.Errorf("FrameworkName = %q after isolation, want the %q default",
			ac.FrameworkName, "mcp")
	}
	if ac.Mode != "" || ac.Persona != "" || ac.SessionID != "" {
		t.Errorf("ActiveContext = %+v after isolation, want empty identity", ac)
	}
}

// TestActiveContextFromEnv_IgnoresAmbientShell proves the PRODUCT
// behaviour the isolation protects: the ambient shell must not be able to
// change what a test observes.
//
// This test is the one that would have caught the original defect at the
// product level: it sets the canonical variable to a value the caller
// does not control and asserts the package behaves as documented.
func TestActiveContextFromEnv_IgnoresAmbientShell(t *testing.T) {
	// Whatever the launching shell exported must not leak in.
	t.Setenv("MPM_PROVENANCE_FRAMEWORK", "ambient-pollution")
	t.Setenv("MPM_FRAMEWORK", "ambient-pollution")
	t.Setenv("MPM_ACTIVE_MODE", "ambient-pollution")
	t.Setenv("MPM_ACTIVE_PERSONA", "ambient-pollution")
	t.Setenv("MPM_SESSION_ID", "ambient-pollution")

	clearAmbientProvenanceEnv(t)

	ac := ActiveContextFromEnv()
	if ac.FrameworkName != "mcp" {
		t.Errorf("FrameworkName = %q, want %q", ac.FrameworkName, "mcp")
	}
	if ac.Mode != "" || ac.Persona != "" || ac.SessionID != "" {
		t.Errorf("ActiveContext = %+v, want empty identity", ac)
	}
}

func contains(haystack []string, needle string) bool {
	for _, s := range haystack {
		if s == needle {
			return true
		}
	}
	return false
}
