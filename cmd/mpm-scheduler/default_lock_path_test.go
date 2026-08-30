// default_lock_path_test.go — regressions for audit finding D-004
// (originally filed in the alpha-4.1 audit as "mpm-scheduler default
// -lock does not honor $MPM_WORKSPACE").
//
// At audit time the `-lock` flag default was a string literal hardcoded
// to the production path. The fix in `defaultLockPath()` resolves the
// lock path at flag-default time using:
//
//   1. MPM_SCHEDULER_LOCK (if set) — explicit override,
//   2. MPM_WORKSPACE/scheduler.lock — workspace-aware default,
//   3. /tmp/mpm-scheduler.lock — ad-hoc fallback for manual runs.
//
// This test pins all three branches so a future refactor cannot
// regress the workspace-aware behaviour.
package main

import (
	"path/filepath"
	"strings"
	"testing"
)

// TestDefaultLockPath_HonorsMPMWorkspace pins the headline regression:
// the default lock path must be derived from MPM_WORKSPACE so a
// disposable workspace scheduler does not collide with a production
// scheduler's lock file.
func TestDefaultLockPath_HonorsMPMWorkspace(t *testing.T) {
	const ws = "/tmp/mpm-scheduler-test-workspace"
	t.Setenv("MPM_WORKSPACE", ws)
	// Ensure MPM_SCHEDULER_LOCK does NOT short-circuit.
	t.Setenv("MPM_SCHEDULER_LOCK", "")

	got := defaultLockPath()
	want := filepath.Join(ws, "scheduler.lock")
	if got != want {
		t.Errorf("defaultLockPath() = %q, want %q (workspace not honored)", got, want)
	}
}

// TestDefaultLockPath_HonorsExplicitOverride pins the precedence:
// MPM_SCHEDULER_LOCK wins over MPM_WORKSPACE so operators can force
// a different lock without changing the workspace.
func TestDefaultLockPath_HonorsExplicitOverride(t *testing.T) {
	t.Setenv("MPM_WORKSPACE", "/tmp/mpm-scheduler-test-ws-ignored")
	t.Setenv("MPM_SCHEDULER_LOCK", "/var/run/custom.lock")

	got := defaultLockPath()
	if got != "/var/run/custom.lock" {
		t.Errorf("defaultLockPath() = %q, want explicit MPM_SCHEDULER_LOCK override", got)
	}
}

// TestDefaultLockPath_FallbackWhenNoEnv pins the manual-run fallback:
// with neither env var set, the path resolves to /tmp/mpm-scheduler.lock.
// This is the documented "ad-hoc fallback" branch.
func TestDefaultLockPath_FallbackWhenNoEnv(t *testing.T) {
	t.Setenv("MPM_WORKSPACE", "")
	t.Setenv("MPM_SCHEDULER_LOCK", "")

	got := defaultLockPath()
	if got != "/tmp/mpm-scheduler.lock" {
		t.Errorf("defaultLockPath() = %q, want %q (ad-hoc fallback)", got, "/tmp/mpm-scheduler.lock")
	}
}

// TestDefaultLockPath_DisjointFromProduction is a guard-rail: even if
// MPM_WORKSPACE is unset, the default must never accidentally point at
// a production location. The audit's failure mode was the production
// path being hardcoded — this test catches any future hardcoding that
// tries to come back in.
func TestDefaultLockPath_DisjointFromProduction(t *testing.T) {
	t.Setenv("MPM_WORKSPACE", "")
	t.Setenv("MPM_SCHEDULER_LOCK", "")

	got := defaultLockPath()
	if strings.Contains(got, "/home/v/.openclaw") ||
		strings.Contains(got, "/opt/mpm") ||
		strings.Contains(got, "/var/lib/mpm") {
		t.Errorf("defaultLockPath() = %q must NOT point at a production location", got)
	}
}

// TestDefaultLockPath_DoesNotDependOnHome pins the test-isolation
// invariant: defaultLockPath must not call os.UserHomeDir() (which
// leaks the host's HOME into the test, and used to silently produce
// different defaults across machines). If the helper ever calls
// os.UserHomeDir, the audit's original defect would re-occur on
// hosts with non-standard HOME paths.
func TestDefaultLockPath_DoesNotDependOnHome(t *testing.T) {
	// Set HOME to a known sentinel; if defaultLockPath reads it, the
	// sentinel will appear in the output.
	const sentinel = "/tmp/mpm-sentinel-home-does-not-exist"
	t.Setenv("HOME", sentinel)
	t.Setenv("MPM_WORKSPACE", "")
	t.Setenv("MPM_SCHEDULER_LOCK", "")

	got := defaultLockPath()
	if strings.Contains(got, sentinel) {
		t.Errorf("defaultLockPath() leaked HOME (%s) into path %q — must not read HOME", sentinel, got)
	}
}
