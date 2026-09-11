package internal

import (
	"encoding/json"
	"strings"
	"testing"
)

// TestWakeContext_ActiveModeSource_ExplicitSelected pins the new
// source indicator in the wake payload: an explicit selection with
// valid file → SourceExplicit.
func TestWakeContext_ActiveModeSource_ExplicitSelected(t *testing.T) {
	root := t.TempDir()
	overrideMPMDir(t, root)
	writePersonaModeFile(t, root, "mode", "debugging")
	writePersonaModeFile(t, root, "mode", "default")

	dm := NewTestDM(t)
	c := "debugging"
	active := &ActiveState{Modes: &[]string{c}}
	if err := SaveActiveJSON(active); err != nil {
		t.Fatalf("seed: %v", err)
	}

	data, err := dm.GatherWakeContext()
	if err != nil {
		t.Fatalf("gather: %v", err)
	}
	if data.ActiveModeSource != SourceExplicit {
		t.Errorf("ActiveModeSource=%q, want %q", data.ActiveModeSource, SourceExplicit)
	}
	if data.ActiveMode != "debugging" {
		t.Errorf("ActiveMode=%q, want %q (singular = first)", data.ActiveMode, "debugging")
	}
	if len(data.ActiveModes) != 1 || data.ActiveModes[0] != "debugging" {
		t.Errorf("ActiveModes=%v, want [debugging]", data.ActiveModes)
	}
}

// TestWakeContext_ActiveModeSource_AbsentBootstrap pins: when no
// active.json exists, both modes should resolve to default fallback.
func TestWakeContext_ActiveModeSource_AbsentBootstrap(t *testing.T) {
	root := t.TempDir()
	overrideMPMDir(t, root)
	writePersonaModeFile(t, root, "mode", "default")

	dm := NewTestDM(t)
	data, err := dm.GatherWakeContext()
	if err != nil {
		t.Fatalf("gather: %v", err)
	}
	if data.ActiveModeSource != SourceFallback {
		t.Errorf("ActiveModeSource=%q, want %q", data.ActiveModeSource, SourceFallback)
	}
	if data.ActiveMode != "default" {
		t.Errorf("ActiveMode=%q, want %q", data.ActiveMode, "default")
	}
}

// TestWakeContext_ActivePersonaSource_ExplicitClear pins the
// critical user spec invariant: explicit clear (persona="") must
// produce SourceEmpty (NOT default fallback).
func TestWakeContext_ActivePersonaSource_ExplicitClear(t *testing.T) {
	root := t.TempDir()
	overrideMPMDir(t, root)
	writePersonaModeFile(t, root, "persona", "default")

	dm := NewTestDM(t)
	empty := ""
	active := &ActiveState{Persona: &empty}
	if err := SaveActiveJSON(active); err != nil {
		t.Fatalf("seed: %v", err)
	}

	data, err := dm.GatherWakeContext()
	if err != nil {
		t.Fatalf("gather: %v", err)
	}
	if data.ActivePersonaSource != SourceEmpty {
		t.Errorf("explicit clear: ActivePersonaSource=%q, want %q", data.ActivePersonaSource, SourceEmpty)
	}
	if data.ActivePersona != "" {
		t.Errorf("explicit clear: ActivePersona=%q, want \"\"", data.ActivePersona)
	}
}

// TestWakeContext_ActivePersonaSource_StaleFallback: stored persona
// file gone → default fallback (not explicit, not empty).
func TestWakeContext_ActivePersonaSource_StaleFallback(t *testing.T) {
	root := t.TempDir()
	overrideMPMDir(t, root)
	writePersonaModeFile(t, root, "persona", "default") // only default exists

	dm := NewTestDM(t)
	stale := "ghost"
	active := &ActiveState{Persona: &stale}
	if err := SaveActiveJSON(active); err != nil {
		t.Fatalf("seed: %v", err)
	}

	data, err := dm.GatherWakeContext()
	if err != nil {
		t.Fatalf("gather: %v", err)
	}
	if data.ActivePersonaSource != SourceFallback {
		t.Errorf("stale: ActivePersonaSource=%q, want %q", data.ActivePersonaSource, SourceFallback)
	}
	if data.ActivePersona != "default" {
		t.Errorf("stale: ActivePersona=%q, want %q", data.ActivePersona, "default")
	}
}

// TestWakeContext_FormatRendersMultiMode verifies the human-readable
// renderer correctly enumerates multiple modes with source tags.
func TestWakeContext_FormatRendersMultiMode(t *testing.T) {
	root := t.TempDir()
	overrideMPMDir(t, root)
	writePersonaModeFile(t, root, "mode", "debugging")
	writePersonaModeFile(t, root, "mode", "forensic")
	writePersonaModeFile(t, root, "mode", "default")
	writePersonaModeFile(t, root, "persona", "default")

	dm := NewTestDM(t)
	sel := []string{"debugging", "forensic"}
	active := &ActiveState{Modes: &sel}
	if err := SaveActiveJSON(active); err != nil {
		t.Fatalf("seed: %v", err)
	}

	data, err := dm.GatherWakeContext()
	if err != nil {
		t.Fatalf("gather: %v", err)
	}
	rendered := formatWakeContext(data)
	if !strings.Contains(rendered, "debugging") || !strings.Contains(rendered, "forensic") {
		t.Errorf("rendered missing modes: %s", rendered)
	}
	if !strings.Contains(rendered, "**Modes:**") {
		t.Errorf("rendered missing Modes label: %s", rendered)
	}
}

// TestWakeContext_JSONWireForm_HasNewFields pins the JSON wire form
// carries the new source + plural-mode fields.
func TestWakeContext_JSONWireForm_HasNewFields(t *testing.T) {
	root := t.TempDir()
	overrideMPMDir(t, root)
	writePersonaModeFile(t, root, "mode", "debugging")
	writePersonaModeFile(t, root, "mode", "forensic")
	writePersonaModeFile(t, root, "mode", "default")
	writePersonaModeFile(t, root, "persona", "default")

	dm := NewTestDM(t)
	sel := []string{"debugging", "forensic"}
	active := &ActiveState{Modes: &sel}
	if err := SaveActiveJSON(active); err != nil {
		t.Fatalf("seed: %v", err)
	}

	data, err := dm.GatherWakeContext()
	if err != nil {
		t.Fatalf("gather: %v", err)
	}
	b, err := EnforceSizeLimit(&data)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	var wire map[string]interface{}
	if err := json.Unmarshal(b, &wire); err != nil {
		t.Fatalf("parse: %v", err)
	}

	// Legacy singular back-compat: first mode only.
	if wire["active_mode"] != "debugging" {
		t.Errorf("wire active_mode=%v, want \"debugging\" (first)", wire["active_mode"])
	}
	// Canonical plural: full collection.
	modesArr, _ := wire["active_modes"].([]interface{})
	if len(modesArr) != 2 {
		t.Errorf("wire active_modes=%v, want [debugging forensic]", wire["active_modes"])
	}
	// Source indicator.
	if wire["active_mode_source"] != SourceExplicit {
		t.Errorf("wire active_mode_source=%v, want %q", wire["active_mode_source"], SourceExplicit)
	}
}