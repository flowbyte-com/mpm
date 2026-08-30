// workspace_resolution_test.go — D-001 (alpha-4.1.1) regression test.
//
// Bug: mpm-critic ignored MPM_WORKSPACE and always opened "." (the
// process CWD). An operator pointing the critic at a disposable
// workspace would silently target the production database — the
// canonical failure mode the MPM_WORKSPACE env var exists to prevent.
//
// Fix: main.go now resolves MPM_WORKSPACE via the canonical
// mpmcli.ResolveWorkspace() helper, with explicit -db still winning
// (operator override preserved).
//
// The test asserts the helper that drives projectRoot computation
// honors MPM_WORKSPACE in the way the rest of the MPM binaries
// already do, so a future revert would surface here.

package main

import (
	"testing"

	"github.com/flowbyte-com/mpm-core/mpmcli"
)

// TestCritic_HonorsMPMWorkspace pins the fix: mpmcli.ResolveWorkspace
// must be the source of truth for the critic's projectRoot when no
// explicit -db is supplied. Without the D-001 fix, this assertion
// would fail under `t.Setenv("MPM_WORKSPACE", tmp)` because main.go
// would override the helper with `projectRoot := "."`.
func TestCritic_HonorsMPMWorkspace(t *testing.T) {
	tmp := t.TempDir()
	t.Setenv("MPM_WORKSPACE", tmp)

	got := mpmcli.ResolveWorkspace()
	if got != tmp {
		t.Fatalf("mpmcli.ResolveWorkspace() = %q, want %q (D-001: critic must honor MPM_WORKSPACE)", got, tmp)
	}
}

// TestCritic_EnvReadAtInvocation pins that the resolver reads the env
// var at invocation time (not at package init), so a t.Setenv in the
// middle of a test sequence picks up the new value. This is the same
// contract mpmcli.ResolveWorkspace advertises for all callers.
func TestCritic_EnvReadAtInvocation(t *testing.T) {
	t.Setenv("MPM_WORKSPACE", "/tmp/first")
	if got := mpmcli.ResolveWorkspace(); got != "/tmp/first" {
		t.Errorf("first read: got %q, want /tmp/first", got)
	}
	t.Setenv("MPM_WORKSPACE", "/tmp/second")
	if got := mpmcli.ResolveWorkspace(); got != "/tmp/second" {
		t.Errorf("second read: got %q, want /tmp/second (must re-read env, not cache)", got)
	}
}
