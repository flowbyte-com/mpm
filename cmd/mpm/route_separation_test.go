package main

import (
	"os"
	"path/filepath"
	"testing"

	mpminternal "github.com/flowbyte-com/mpm-core"
)

// TestRoute_ManualSelectionBypassesRouter is the key routing-separation
// invariant: when active.json has a CONCRETE persona/mode (not "auto"),
// `mpm route` must NOT mutate active.json even with --apply. The router
// is consulted only in the auto branch.
//
// Regression risk: a future cleanup that "unifies" the manual and auto
// branches would silently make routing an implicit selector. This test
// pins the separation so such a refactor must consciously remove it.
func TestRoute_ManualSelectionBypassesRouter(t *testing.T) {
	workspace := t.TempDir()
	t.Setenv("MPM_WORKSPACE", workspace)
	t.Setenv("MPM_ROUTE_WORKSPACE", workspace)

	// Seed with concrete (manual) persona/modes — NOT "auto".
	persona := "venkat"
	modes := []string{"architect"}
	if err := mpminternal.SaveActiveJSON(&mpminternal.ActiveState{
		Persona: &persona,
		Modes:   &modes,
		Updated: "2026-01-01T00:00:00Z",
	}); err != nil {
		t.Fatalf("seed: %v", err)
	}

	// Build minimal definition files so the router could find them.
	for _, fn := range []struct{ dir, name, content string }{
		{"persona", "venkat", "---\nname: venkat\n---\nbody\n"},
		{"persona", "critic", "---\nname: critic\n---\nbody\n"},
		{"mode", "architect", "---\nname: architect\n---\nbody\n"},
		{"mode", "debugging", "---\nname: debugging\n---\nbody\n"},
	} {
		if err := os.MkdirAll(filepath.Join(workspace, fn.dir), 0700); err != nil {
			t.Fatalf("mkdir: %v", err)
		}
		if err := os.WriteFile(filepath.Join(workspace, fn.dir, fn.name+".md"),
			[]byte(fn.content), 0644); err != nil {
			t.Fatalf("write: %v", err)
		}
	}

	r := NewRouter()
	// Prompt that would route to "critic" — but the manual branch must
	// bypass the router entirely.
	rc := r.handleRoute([]string{"--apply", "the critic challenges assumptions"})
	if rc != 0 {
		t.Fatalf("handleRoute returned %d, want 0 (hook contract: never block)", rc)
	}

	got, err := mpminternal.LoadActiveJSON()
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	if got.PersonaString() != "venkat" {
		t.Errorf("explicit selection overwritten by route --apply: Persona=%q, want %q",
			got.PersonaString(), "venkat")
	}
	if len(got.ModesSlice()) != 1 || got.ModesSlice()[0] != "architect" {
		t.Errorf("explicit selection overwritten by route --apply: Modes=%v, want [architect]",
			got.ModesSlice())
	}
}

// TestRoute_AutoBranch_MutatesOnApply verifies the auto branch DOES
// mutate when --apply is set — pins the other side of the contract.
func TestRoute_AutoBranch_MutatesOnApply(t *testing.T) {
	workspace := t.TempDir()
	t.Setenv("MPM_WORKSPACE", workspace)
	t.Setenv("MPM_ROUTE_WORKSPACE", workspace)

	autoPersona := "auto"
	autoModes := []string{"auto"}
	if err := mpminternal.SaveActiveJSON(&mpminternal.ActiveState{
		Persona: &autoPersona,
		Modes:   &autoModes,
		Updated: "2026-01-01T00:00:00Z",
	}); err != nil {
		t.Fatalf("seed: %v", err)
	}
	for _, fn := range []struct{ dir, name, content string }{
		{"persona", "critic", "---\nname: critic\npatterns: critic\n---\nbody\n"},
		{"mode", "debugging", "---\nname: debugging\npatterns: debugging\n---\nbody\n"},
	} {
		if err := os.MkdirAll(filepath.Join(workspace, fn.dir), 0700); err != nil {
			t.Fatalf("mkdir: %v", err)
		}
		if err := os.WriteFile(filepath.Join(workspace, fn.dir, fn.name+".md"),
			[]byte(fn.content), 0644); err != nil {
			t.Fatalf("write: %v", err)
		}
	}

	r := NewRouter()
	rc := r.handleRoute([]string{"--apply", "the critic challenges assumptions"})
	if rc != 0 {
		t.Fatalf("handleRoute returned %d, want 0", rc)
	}

	got, err := mpminternal.LoadActiveJSON()
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	// After route --apply, the explicit persona should be the routed one.
	if got.PersonaString() == "auto" {
		t.Errorf("auto sentinel not replaced: Persona=%q, want concrete",
			got.PersonaString())
	}
}

// TestRoute_NoApply_DoesNotMutate pins that the no-apply path is
// purely advisory — even in the auto branch, without --apply,
// active.json must not be touched.
func TestRoute_NoApply_DoesNotMutate(t *testing.T) {
	workspace := t.TempDir()
	t.Setenv("MPM_WORKSPACE", workspace)
	t.Setenv("MPM_ROUTE_WORKSPACE", workspace)

	autoPersona := "auto"
	autoModes := []string{"auto"}
	if err := mpminternal.SaveActiveJSON(&mpminternal.ActiveState{
		Persona: &autoPersona,
		Modes:   &autoModes,
		Updated: "2026-01-01T00:00:00Z",
	}); err != nil {
		t.Fatalf("seed: %v", err)
	}
	for _, fn := range []struct{ dir, name, content string }{
		{"persona", "critic", "---\nname: critic\npatterns: critic\n---\nbody\n"},
		{"mode", "debugging", "---\nname: debugging\npatterns: debugging\n---\nbody\n"},
	} {
		if err := os.MkdirAll(filepath.Join(workspace, fn.dir), 0700); err != nil {
			t.Fatalf("mkdir: %v", err)
		}
		if err := os.WriteFile(filepath.Join(workspace, fn.dir, fn.name+".md"),
			[]byte(fn.content), 0644); err != nil {
			t.Fatalf("write: %v", err)
		}
	}

	r := NewRouter()
	rc := r.handleRoute([]string{"critic challenges assumptions"}) // no --apply
	if rc != 0 {
		t.Fatalf("handleRoute returned %d, want 0", rc)
	}

	got, err := mpminternal.LoadActiveJSON()
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	if got.PersonaString() != "auto" {
		t.Errorf("no-apply mutates Persona=%q, want still \"auto\"", got.PersonaString())
	}
}