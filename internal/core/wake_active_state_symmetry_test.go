// wake_active_state_symmetry_test.go — regression coverage for
// shared-core work item 8ffa1c070b7a6793: "wake_context emits empty
// active_mode when no modes present".
//
// Bug (pre-fix):
//
//   readActiveState in wake_context.go gated mode resolution on
//   `len(active.Modes) > 0`, so an active.json that omitted the
//   `modes` field (e.g. `{"active_workspace":"default"}`) produced
//   `active_mode=""` while `active_persona` still fell back to
//   "default" through the resolver. The wake JSON therefore carried
//   an asymmetric identity pair that no host adapter could reconcile
//   without papering over the underlying shared-core defect.
//
// Contract (post-fix):
//
//   Both ResolveActiveMode and ResolveActivePersona run unconditionally
//   with the joined mode string (which is "" when the modes slice is
//   empty or absent), and both rely on the resolver's existing
//   fallback chain:
//
//     requested value exists and is valid   → use requested
//     requested is empty/unset/invalid      → try "default"
//     "default" exists                      → return "default"
//     "default" missing                     → return "" silently
//
//   That contract applies identically to mode and persona. There is
//   one authoritative resolver path; readActiveState is a thin
//   caller that does not duplicate fallback logic.
//
// Tests below pin every case in the contract matrix (one
// resolver-level matrix per kind + one wake-context integration
// matrix that asserts the actual JSON payload), plus the
// specific shared-core acceptance scenario from the bug report
// (active.json with `active_workspace:"default"` only).

package internal

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
)

// writeActiveJSON writes the supplied active.json contents to the
// test-controlled root, parent directory of the persona/mode .md
// files written by writePersonaModeFile.
func writeActiveJSON(t *testing.T, root string, contents string) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(root, "active.json"), []byte(contents), 0o600); err != nil {
		t.Fatalf("write active.json: %v", err)
	}
}

// ─── Resolver symmetry matrix ────────────────────────────────────────────

// TestResolveActive_Symmetry_EmptyRequested_FallsBackToDefault pins
// the empty-input branch on both kinds so future readers know the
// resolver is the authoritative fallback path. (Per-kind versions
// already exist; this one asserts parity.)
func TestResolveActive_Symmetry_EmptyRequested_FallsBackToDefault(t *testing.T) {
	root := t.TempDir()
	overrideMPMDir(t, root)
	writePersonaModeFile(t, root, "mode", "default")
	writePersonaModeFile(t, root, "persona", "default")

	if got := ResolveActiveMode(nil, ""); got != "default" {
		t.Errorf("ResolveActiveMode(\"\") = %q, want %q (parity with persona)", got, "default")
	}
	if got := ResolveActivePersona(nil, ""); got != "default" {
		t.Errorf("ResolveActivePersona(\"\") = %q, want %q", got, "default")
	}
}

func TestResolveActive_Symmetry_RequestedMissing_FallsBackToDefault(t *testing.T) {
	root := t.TempDir()
	overrideMPMDir(t, root)
	writePersonaModeFile(t, root, "mode", "default")
	writePersonaModeFile(t, root, "persona", "default")

	if got := ResolveActiveMode(nil, "ghost"); got != "default" {
		t.Errorf("ResolveActiveMode(\"ghost\") = %q, want %q", got, "default")
	}
	if got := ResolveActivePersona(nil, "ghost"); got != "default" {
		t.Errorf("ResolveActivePersona(\"ghost\") = %q, want %q", got, "default")
	}
}

func TestResolveActive_Symmetry_RequestedExists(t *testing.T) {
	root := t.TempDir()
	overrideMPMDir(t, root)
	writePersonaModeFile(t, root, "mode", "debugging")
	writePersonaModeFile(t, root, "mode", "default")
	writePersonaModeFile(t, root, "persona", "forensic")
	writePersonaModeFile(t, root, "persona", "default")

	if got := ResolveActiveMode(nil, "debugging"); got != "debugging" {
		t.Errorf("ResolveActiveMode(\"debugging\") = %q, want %q", got, "debugging")
	}
	if got := ResolveActivePersona(nil, "forensic"); got != "forensic" {
		t.Errorf("ResolveActivePersona(\"forensic\") = %q, want %q", got, "forensic")
	}
}

func TestResolveActive_Symmetry_BothMissing_ReturnsEmptySilently(t *testing.T) {
	root := t.TempDir()
	overrideMPMDir(t, root)
	// Neither mode nor persona files exist on disk.

	if got := ResolveActiveMode(nil, "anything"); got != "" {
		t.Errorf("ResolveActiveMode with no files = %q, want empty (no error)", got)
	}
	if got := ResolveActivePersona(nil, "anything"); got != "" {
		t.Errorf("ResolveActivePersona with no files = %q, want empty (no error)", got)
	}
}

func TestResolveActive_Symmetry_DefaultMissingForOne_OtherStillDefaults(t *testing.T) {
	root := t.TempDir()
	overrideMPMDir(t, root)
	// Only persona/default.md exists; no mode files.
	writePersonaModeFile(t, root, "persona", "default")

	if got := ResolveActiveMode(nil, ""); got != "" {
		t.Errorf("ResolveActiveMode with no mode files = %q, want empty (silent)", got)
	}
	if got := ResolveActivePersona(nil, ""); got != "default" {
		t.Errorf("ResolveActivePersona with persona/default only = %q, want %q", got, "default")
	}

	// Inverse: only mode/default.md exists.
	root2 := t.TempDir()
	overrideMPMDir(t, root2)
	writePersonaModeFile(t, root2, "mode", "default")

	if got := ResolveActiveMode(root2_dm(t), ""); got != "default" { //nolint:staticcheck
		t.Errorf("ResolveActiveMode with mode/default only = %q, want %q", got, "default")
	}
	if got := ResolveActivePersona(root2_dm(t), ""); got != "" {
		t.Errorf("ResolveActivePersona with no persona files = %q, want empty (silent)", got)
	}
}

// root2_dm is a tiny helper to satisfy the type signature expected by
// the inverse case above (ResolveActiveMode/Persona take a
// *DatabaseManager; passing nil is fine for the resolver because the
// dm is only used for audit logging, not for the fallback itself).
// Kept as a function rather than a literal nil to make the intent
// obvious to readers.
func root2_dm(t *testing.T) *DatabaseManager { //nolint:staticcheck
	t.Helper()
	return nil
}

// ─── readActiveState matrix ────────────────────────────────────────────────

// TestReadActiveState_AbsentModesField_FallsBackToDefault is the
// regression pin for the shared-core defect. Pre-fix, an active.json
// of `{"active_workspace":"default"}` produced active_mode="" while
// active_persona still resolved to "default"; the post-fix contract
// is symmetric.
func TestReadActiveState_AbsentModesField_FallsBackToDefault(t *testing.T) {
	root := t.TempDir()
	overrideMPMDir(t, root)
	writePersonaModeFile(t, root, "mode", "default")
	writePersonaModeFile(t, root, "persona", "default")
	writeActiveJSON(t, root, `{"active_workspace":"default"}`)

	dm := NewTestDM(t)
	mode, persona := readActiveState(dm)
	if mode != "default" {
		t.Errorf("regression 8ffa1c070b7a6793: active_mode=%q, want %q (no modes field in active.json must still resolve to default)", mode, "default")
	}
	if persona != "default" {
		t.Errorf("active_persona=%q, want %q", persona, "default")
	}
}

func TestReadActiveState_ExplicitModeAndPersona(t *testing.T) {
	root := t.TempDir()
	overrideMPMDir(t, root)
	writePersonaModeFile(t, root, "mode", "debugging")
	writePersonaModeFile(t, root, "mode", "default")
	writePersonaModeFile(t, root, "persona", "forensic")
	writePersonaModeFile(t, root, "persona", "default")
	writeActiveJSON(t, root, `{"persona":"forensic","modes":["debugging"]}`)

	dm := NewTestDM(t)
	mode, persona := readActiveState(dm)
	if mode != "debugging" {
		t.Errorf("active_mode=%q, want %q", mode, "debugging")
	}
	if persona != "forensic" {
		t.Errorf("active_persona=%q, want %q", persona, "forensic")
	}
}

func TestReadActiveState_InvalidRequested_FallsBackToDefault(t *testing.T) {
	root := t.TempDir()
	overrideMPMDir(t, root)
	writePersonaModeFile(t, root, "mode", "default")
	writePersonaModeFile(t, root, "persona", "default")
	writeActiveJSON(t, root, `{"persona":"nonexistent","modes":["nonexistent"]}`)

	dm := NewTestDM(t)
	mode, persona := readActiveState(dm)
	if mode != "default" {
		t.Errorf("invalid mode should fall back to default; got %q", mode)
	}
	if persona != "default" {
		t.Errorf("invalid persona should fall back to default; got %q", persona)
	}
}

func TestReadActiveState_BothDefaultsMissing_ReturnsEmptySilently(t *testing.T) {
	root := t.TempDir()
	overrideMPMDir(t, root)
	// No mode/persona files on disk at all.
	writeActiveJSON(t, root, `{}`)

	dm := NewTestDM(t)
	mode, persona := readActiveState(dm)
	if mode != "" {
		t.Errorf("missing default mode + missing default persona → mode=%q, want empty", mode)
	}
	if persona != "" {
		t.Errorf("missing default mode + missing default persona → persona=%q, want empty", persona)
	}
}

func TestReadActiveState_OneDefaultMissing_OtherStillDefaults(t *testing.T) {
	// v spec 2026-09-11 (selector hardening): explicit clear (persona="",
	// modes=[]) is now distinguishable from absent fields, and explicit
	// clear returns empty (SourceEmpty) — NOT fallback to default.
	// This test now pins the new contract for explicit-clear.
	root := t.TempDir()
	overrideMPMDir(t, root)
	// Only persona/default.md exists. Active.json carries explicit-clear
	// (persona="", modes=[]) — both fields should resolve empty, NOT to
	// default.
	writePersonaModeFile(t, root, "persona", "default")
	writeActiveJSON(t, root, `{"persona":"","modes":[]}`)

	dm := NewTestDM(t)
	mode, persona := readActiveState(dm)
	if mode != "" {
		t.Errorf("explicit clear modes → mode=%q, want empty (no default injection)", mode)
	}
	if persona != "" {
		t.Errorf("explicit clear persona → persona=%q, want empty (no default injection)", persona)
	}

	// Inverse.
	root2 := t.TempDir()
	overrideMPMDir(t, root2)
	writePersonaModeFile(t, root2, "mode", "default")
	writeActiveJSON(t, root2, `{"persona":"","modes":[]}`)

	dm2 := NewTestDM(t)
	mode2, persona2 := readActiveState(dm2)
	if mode2 != "" {
		t.Errorf("explicit clear modes → mode=%q, want empty (no default injection)", mode2)
	}
	if persona2 != "" {
		t.Errorf("explicit clear persona → persona=%q, want empty (no default injection)", persona2)
	}
}

func TestReadActiveState_NoActiveJSON_ReturnsEmptySilently(t *testing.T) {
	root := t.TempDir()
	overrideMPMDir(t, root)
	// active.json does not exist (fresh install).
	dm := NewTestDM(t)
	mode, persona := readActiveState(dm)
	if mode != "" || persona != "" {
		t.Errorf("missing active.json: mode=%q persona=%q, want empty (no error)", mode, persona)
	}
}

// ─── Wake-context JSON integration ─────────────────────────────────────────

// TestGatherWakeContext_ActiveModePersonaSymmetric pins the actual
// wire-form payload from GatherWakeContext for the regression
// scenario. This is the test the bug report asked for: the JSON
// payload must carry active_mode="default" and active_persona="default"
// when active.json is `{"active_workspace":"default"}`.
func TestGatherWakeContext_ActiveModePersonaSymmetric(t *testing.T) {
	root := t.TempDir()
	overrideMPMDir(t, root)
	writePersonaModeFile(t, root, "mode", "default")
	writePersonaModeFile(t, root, "persona", "default")
	writeActiveJSON(t, root, `{"active_workspace":"default"}`)

	dm := NewTestDM(t)
	data, err := dm.GatherWakeContextReadOnly()
	if err != nil {
		t.Fatalf("GatherWakeContextReadOnly: %v", err)
	}
	if data.ActiveMode != "default" {
		t.Errorf("regression 8ffa1c070b7a6793: ActiveMode=%q, want %q (wake JSON must carry default/default)", data.ActiveMode, "default")
	}
	if data.ActivePersona != "default" {
		t.Errorf("ActivePersona=%q, want %q", data.ActivePersona, "default")
	}

	// Also confirm the JSON wire form is symmetric — round-trip via
	// json.Marshal to make sure the field names are what downstream
	// hosts see (the host integration fix is gated on the JSON key
	// being active_mode/active_persona, not on the Go struct field).
	b, err := json.Marshal(data)
	if err != nil {
		t.Fatalf("marshal wake context: %v", err)
	}
	var wire struct {
		ActiveMode    string `json:"active_mode"`
		ActivePersona string `json:"active_persona"`
	}
	if err := json.Unmarshal(b, &wire); err != nil {
		t.Fatalf("unmarshal wake context: %v", err)
	}
	if wire.ActiveMode != "default" {
		t.Errorf("JSON wire form: active_mode=%q, want %q", wire.ActiveMode, "default")
	}
	if wire.ActivePersona != "default" {
		t.Errorf("JSON wire form: active_persona=%q, want %q", wire.ActivePersona, "default")
	}
}

func TestGatherWakeContext_BothDefaultsMissing_JSONEmptySilently(t *testing.T) {
	root := t.TempDir()
	overrideMPMDir(t, root)
	// No mode/persona files; write an active.json that requests nothing.
	writeActiveJSON(t, root, `{"persona":"","modes":[]}`)

	dm := NewTestDM(t)
	data, err := dm.GatherWakeContextReadOnly()
	if err != nil {
		t.Fatalf("GatherWakeContextReadOnly: %v", err)
	}
	if data.ActiveMode != "" {
		t.Errorf("missing default mode → ActiveMode=%q, want empty (no error)", data.ActiveMode)
	}
	if data.ActivePersona != "" {
		t.Errorf("missing default persona → ActivePersona=%q, want empty (no error)", data.ActivePersona)
	}
}

func TestGatherWakeContext_ExplicitModeAndPersona_Respected(t *testing.T) {
	root := t.TempDir()
	overrideMPMDir(t, root)
	writePersonaModeFile(t, root, "mode", "debugging")
	writePersonaModeFile(t, root, "mode", "default")
	writePersonaModeFile(t, root, "persona", "forensic")
	writePersonaModeFile(t, root, "persona", "default")
	writeActiveJSON(t, root, `{"persona":"forensic","modes":["debugging"]}`)

	dm := NewTestDM(t)
	data, err := dm.GatherWakeContextReadOnly()
	if err != nil {
		t.Fatalf("GatherWakeContextReadOnly: %v", err)
	}
	if data.ActiveMode != "debugging" {
		t.Errorf("ActiveMode=%q, want %q", data.ActiveMode, "debugging")
	}
	if data.ActivePersona != "forensic" {
		t.Errorf("ActivePersona=%q, want %q", data.ActivePersona, "forensic")
	}
}

// TestReadWakeContext_HumanFormat_DefaultDefault renders the
// human-readable format that hosts like the Claude Code and Pi
// integrations parse.
//
// 8ffa1c070b7a6793 (W-001 wake context regression) introduced the
// multi-mode format with `**Modes:**` (plural heading for the
// multi-mode collection) plus inline `[fallback]` / `[empty]` source
// tags. The test setup writes a mode and persona file at "default"
// but omits the `modes` / `persona` keys in active.json — the resolver
// therefore falls back to the default files and surfaces the source
// tag inline.
//
// The expected post-fix rendering for this case is
// `**Modes:** default [fallback]` and `**Persona:** default [fallback]`.
// Pre-fix the singular `**Mode:** default` was rendered without the
// source tag, which masked the absence of an explicit pointer and
// confused wake-rendering hosts.
func TestReadWakeContext_HumanFormat_DefaultDefault(t *testing.T) {
	root := t.TempDir()
	overrideMPMDir(t, root)
	writePersonaModeFile(t, root, "mode", "default")
	writePersonaModeFile(t, root, "persona", "default")
	writeActiveJSON(t, root, `{"active_workspace":"default"}`)

	dm := NewTestDM(t)
	rendered, err := dm.ReadWakeContext()
	if err != nil {
		t.Fatalf("ReadWakeContext: %v", err)
	}
	// Post-8ffa1c070b7a6793 format (multi-mode + source tag):
	if !wakeContains(rendered, "**Modes:** default [fallback]") {
		t.Errorf("human-format wake context missing '**Modes:** default [fallback]'; got:\n%s", rendered)
	}
	if !wakeContains(rendered, "**Persona:** default [fallback]") {
		t.Errorf("human-format wake context missing '**Persona:** default [fallback]'; got:\n%s", rendered)
	}
}

// wakeContains / wakeIndexOf are local substring helpers used by the
// wake-context regression tests. Naming avoids collision with the
// package-private `contains` helper used by shared_write_test.go.
func wakeContains(haystack, needle string) bool {
	return wakeIndexOf(haystack, needle) >= 0
}

func wakeIndexOf(haystack, needle string) int {
	n := len(needle)
	if n == 0 {
		return 0
	}
	for i := 0; i+n <= len(haystack); i++ {
		if haystack[i:i+n] == needle {
			return i
		}
	}
	return -1
}
