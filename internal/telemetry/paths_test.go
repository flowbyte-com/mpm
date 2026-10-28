package telemetry

import (
	"path/filepath"
	"strings"
	"testing"
)

// The telemetry store's path was decided TWICE — by the collector and by
// docs/SPEC.md §6.6 — and the two disagreed. The disagreement is not
// cosmetic: it made the critic's telemetry hunt inert (it read a path
// nothing writes to) and it made the spec wrong.
//
// These tests pin ONE resolver, shared by the collector, the readers
// and the critic.

// TestStorePath_IsTheCanonicalLocation pins the canonical location: the
// same "src/db/" layout the substrate uses for mpm.db.
func TestStorePath_IsTheCanonicalLocation(t *testing.T) {
	got := StorePath("/ws")
	want := filepath.Join("/ws", "src", "db", "telemetry.db")
	if got != want {
		t.Errorf("StorePath = %q, want %q", got, want)
	}
}

// TestResolveStorePath_PrefersTheOverride pins the operator override.
func TestResolveStorePath_PrefersTheOverride(t *testing.T) {
	t.Setenv("MPM_TELEMETRY_DB", "/custom/telemetry.db")
	got, err := ResolveStorePath("/ws")
	if err != nil {
		t.Fatalf("ResolveStorePath: %v", err)
	}
	if got != "/custom/telemetry.db" {
		t.Errorf("ResolveStorePath = %q, want the MPM_TELEMETRY_DB override", got)
	}
}

// TestResolveStorePath_UsesTheWorkspace pins the default.
func TestResolveStorePath_UsesTheWorkspace(t *testing.T) {
	t.Setenv("MPM_TELEMETRY_DB", "")
	got, err := ResolveStorePath("/ws")
	if err != nil {
		t.Fatalf("ResolveStorePath: %v", err)
	}
	if want := StorePath("/ws"); got != want {
		t.Errorf("ResolveStorePath = %q, want %q", got, want)
	}
}

// TestResolveStorePath_RefusesAnEmptyWorkspace pins that the result is
// never cwd-relative.
//
// A relative path resolves differently depending on where the process
// was started, so a writer and a reader can silently disagree. That is
// the ghost-store shape this codebase removed after the 2026-07-21
// incident (lesson 59fe3f8ff3e1549e).
func TestResolveStorePath_RefusesAnEmptyWorkspace(t *testing.T) {
	t.Setenv("MPM_TELEMETRY_DB", "")
	_, err := ResolveStorePath("")
	if err == nil {
		t.Fatal("ResolveStorePath succeeded with no workspace and no override")
	}
	// The message must name both ways out.
	for _, want := range []string{"MPM_WORKSPACE", "MPM_TELEMETRY_DB"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error = %v, want it to name %q", err, want)
		}
	}
}

// TestResolveStorePath_IgnoresABlankOverride pins that a blank
// environment variable is not an override.
func TestResolveStorePath_IgnoresABlankOverride(t *testing.T) {
	t.Setenv("MPM_TELEMETRY_DB", "   ")
	got, err := ResolveStorePath("/ws")
	if err != nil {
		t.Fatalf("ResolveStorePath: %v", err)
	}
	if want := StorePath("/ws"); got != want {
		t.Errorf("ResolveStorePath = %q, want %q", got, want)
	}
}

// TestDescribeStorePath_NamesTheOverride pins that an operator-facing
// message says where the path came from.
func TestDescribeStorePath_NamesTheOverride(t *testing.T) {
	t.Setenv("MPM_TELEMETRY_DB", "")
	if got := DescribeStorePath("/ws/src/db/telemetry.db"); got != "/ws/src/db/telemetry.db" {
		t.Errorf("DescribeStorePath = %q, want the bare path", got)
	}
	t.Setenv("MPM_TELEMETRY_DB", "/custom/telemetry.db")
	got := DescribeStorePath("/custom/telemetry.db")
	if !strings.Contains(got, "MPM_TELEMETRY_DB") {
		t.Errorf("DescribeStorePath = %q, want it to name the override", got)
	}
}

// TestEnvStorePathOverride_ReadsTheEnvironment pins the accessor.
func TestEnvStorePathOverride_ReadsTheEnvironment(t *testing.T) {
	t.Setenv("MPM_TELEMETRY_DB", "/custom/telemetry.db")
	if got := EnvStorePathOverride(); got != "/custom/telemetry.db" {
		t.Errorf("EnvStorePathOverride = %q", got)
	}
	t.Setenv("MPM_TELEMETRY_DB", "")
	if got := EnvStorePathOverride(); got != "" {
		t.Errorf("EnvStorePathOverride = %q, want empty", got)
	}
}

// TestStorePath_IsNotTheSpecPath is a REGRESSION guard for the defect
// this issue is about: docs/SPEC.md §6.6 named
// "$MPM_WORKSPACE/telemetry.db", which is not where the collector
// writes. If someone "restores" the spec's path here, the critic's
// hunt goes inert again.
func TestStorePath_IsNotTheSpecPath(t *testing.T) {
	specPath := filepath.Join("/ws", "telemetry.db")
	if got := StorePath("/ws"); got == specPath {
		t.Errorf("StorePath = %q, which is the SPEC's path; the collector writes to %q",
			got, filepath.Join("/ws", "src", "db", "telemetry.db"))
	}
}
