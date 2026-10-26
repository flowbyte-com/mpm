// provenance_env_isolation_test.go — keep provenance-attribution tests
// independent of the shell that runs them.
//
// `mpm call` (and therefore any in-process test that drives handleCall)
// reads the caller's identity from MPM_PROVENANCE_* / MPM_FRAMEWORK. A
// developer shell, an agent harness, or a CI job may export those (this
// repository's own OpenCode environment exports
// MPM_PROVENANCE_FRAMEWORK=opencode), and a test that asserts the
// DEFAULT is "mpm-cli" then fails against a correct binary. That is a
// test-environment defect, not a product defect: the same test passes
// on a clean machine.
//
// Tests that assert a default, or that supply their own identity, call
// clearAmbientProvenanceEnv first so the starting state is defined by
// the test rather than by whoever launched it.
package main

import "testing"

// clearAmbientProvenanceEnv pins the caller-identity environment to
// "no host identity supplied" for the duration of one test.
//
// It deliberately does not touch MPM_PROVENANCE_INVOCATION_ID: tests
// that assert an explicit correlation id set that themselves, after
// calling this helper. t.Setenv restores every value at test cleanup.
func clearAmbientProvenanceEnv(t *testing.T) {
	t.Helper()
	for _, key := range []string{
		"MPM_PROVENANCE_FRAMEWORK",
		"MPM_FRAMEWORK",
		"MPM_PROVENANCE_MODEL",
		"MPM_PROVENANCE_FRAMEWORK_SESSION_ID",
		"MPM_PROVENANCE_PARENT_INVOCATION_ID",
	} {
		t.Setenv(key, "")
	}
}
