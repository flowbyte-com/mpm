package seed

import (
	"errors"
	"os"
	"path/filepath"
	"testing"
)

// =============================================================================
// capabilities_loader_test.go — CS-1.5 loader tests
//
// Pins the loader's contract:
//
//   * No sidecar → Merged == compiled registry, SidecarPath="".
//   * Explicit path → sidecar read; merged reflects overrides +
//     additions; SidecarPath is the absolute path.
//   * Missing explicit path → error (silent fallback would
//     surprise the operator).
//   * schema_version mismatch → ErrSchemaVersionMismatch.
//   * Sidecar entry with bad Validate() → error; nothing
//     merged.
//   * Override by stable_id replaces the compiled entry.
//   * Addition (stable_id not in compiled) is appended.
//   * MPM_WORKSPACE unset → default sidecar is NOT read
//     (operator hasn't run `mpm start`); treated as "no sidecar."
// =============================================================================

// writeSidecar writes a JSON sidecar to a temp file and
// returns the path. Cleanup is the test's responsibility
// (t.TempDir() auto-cleans).
func writeSidecar(t *testing.T, content string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "bundled_capabilities.json")
	if err := os.WriteFile(path, []byte(content), 0644); err != nil {
		t.Fatalf("write sidecar: %v", err)
	}
	return path
}

// TestLoadBundledCapabilities_NoSidecar_ReturnsCompiled:
// when no path is given and MPM_WORKSPACE is unset (the
// default test env), the loader returns the compiled
// registry unchanged and SidecarPath="".
func TestLoadBundledCapabilities_NoSidecar_ReturnsCompiled(t *testing.T) {
	// Make sure MPM_WORKSPACE isn't accidentally set in
	// the test environment.
	t.Setenv("MPM_WORKSPACE", "")

	result, err := LoadBundledCapabilities("")
	if err != nil {
		t.Fatalf("LoadBundledCapabilities: %v", err)
	}
	if result.SidecarPath != "" {
		t.Fatalf("expected SidecarPath=\"\", got %q", result.SidecarPath)
	}
	if len(result.Merged) != len(SeedCapabilities) {
		t.Fatalf("expected %d entries, got %d",
			len(SeedCapabilities), len(result.Merged))
	}
	if result.CompiledCount != len(SeedCapabilities) {
		t.Fatalf("expected CompiledCount=%d, got %d",
			len(SeedCapabilities), result.CompiledCount)
	}
	if result.SidecarOverridesCount != 0 || result.SidecarAdditionsCount != 0 {
		t.Fatalf("expected zero overrides/additions, got %d/%d",
			result.SidecarOverridesCount, result.SidecarAdditionsCount)
	}
}

// TestLoadBundledCapabilities_MissingExplicitPath_Errors:
// an explicit path that doesn't exist is an error. Silent
// fallback to the compiled registry would mask a typo.
func TestLoadBundledCapabilities_MissingExplicitPath_Errors(t *testing.T) {
	missing := filepath.Join(t.TempDir(), "does-not-exist.json")
	_, err := LoadBundledCapabilities(missing)
	if err == nil {
		t.Fatal("expected error for missing explicit path")
	}
}

// TestLoadBundledCapabilities_SchemaVersionMismatch_Errors:
// a sidecar with the wrong schema_version is rejected.
// The loader does NOT silently interpret unknown shapes.
func TestLoadBundledCapabilities_SchemaVersionMismatch_Errors(t *testing.T) {
	path := writeSidecar(t, `{
		"schema_version": "0.9",
		"capabilities": []
	}`)
	_, err := LoadBundledCapabilities(path)
	if err == nil {
		t.Fatal("expected schema_version mismatch error")
	}
	if !errors.Is(err, ErrSchemaVersionMismatch) {
		t.Fatalf("expected ErrSchemaVersionMismatch, got: %v", err)
	}
}

// TestLoadBundledCapabilities_SchemaVersionMismatch_NoMerge:
// when the version mismatches, no entries are merged even
// if the sidecar has a capabilities array. The loader
// rejects the whole document, not just the bad part.
func TestLoadBundledCapabilities_SchemaVersionMismatch_NoMerge(t *testing.T) {
	path := writeSidecar(t, `{
		"schema_version": "0.9",
		"capabilities": [
			{
				"stable_id": "cap-seed-override",
				"name": "override",
				"purpose": "would-be override",
				"source_language": "bash",
				"requested_domain": "sandbox",
				"source_code": "#!/bin/bash\necho override\n"
			}
		]
	}`)
	_, err := LoadBundledCapabilities(path)
	if err == nil {
		t.Fatal("expected error")
	}
}

// TestLoadBundledCapabilities_InvalidEntry_Aborts:
// a sidecar entry that fails Validate() aborts the entire
// load. The loader does NOT silently drop the bad entry
// and merge the rest — that would mask a broken bundle.
func TestLoadBundledCapabilities_InvalidEntry_Aborts(t *testing.T) {
	path := writeSidecar(t, `{
		"schema_version": "1.0",
		"capabilities": [
			{
				"stable_id": "cap-seed-bad",
				"name": "bad_entry",
				"purpose": "purpose",
				"source_language": "ruby",
				"requested_domain": "sandbox",
				"source_code": "#!/bin/bash\necho bad\n"
			}
		]
	}`)
	_, err := LoadBundledCapabilities(path)
	if err == nil {
		t.Fatal("expected validation error for invalid source_language")
	}
}

// TestLoadBundledCapabilities_OverrideReplacesCompiled:
// when the sidecar has an entry with stable_id matching a
// compiled entry, the sidecar replaces the compiled entry
// in the merged result.
func TestLoadBundledCapabilities_OverrideReplacesCompiled(t *testing.T) {
	path := writeSidecar(t, `{
		"schema_version": "1.0",
		"capabilities": [
			{
				"stable_id": "cap-seed-list-capabilities",
				"name": "list_capabilities",
				"purpose": "OVERRIDDEN PURPOSE.",
				"source_language": "bash",
				"requested_domain": "sandbox",
				"source_code": "#!/bin/bash\necho overridden\n",
				"tags": ["capability", "overridden"]
			}
		]
	}`)

	result, err := LoadBundledCapabilities(path)
	if err != nil {
		t.Fatalf("LoadBundledCapabilities: %v", err)
	}
	if len(result.Merged) != len(SeedCapabilities) {
		t.Fatalf("override should not change len, got %d", len(result.Merged))
	}
	if result.SidecarOverridesCount != 1 {
		t.Fatalf("expected SidecarOverridesCount=1, got %d",
			result.SidecarOverridesCount)
	}
	if result.SidecarAdditionsCount != 0 {
		t.Fatalf("expected SidecarAdditionsCount=0, got %d",
			result.SidecarAdditionsCount)
	}

	// Find the overridden entry.
	idx := findSeedCapabilityByStableID(result.Merged, "cap-seed-list-capabilities")
	if idx < 0 {
		t.Fatal("overridden entry missing from result")
	}
	if result.Merged[idx].Purpose != "OVERRIDDEN PURPOSE." {
		t.Fatalf("override didn't apply: purpose=%q",
			result.Merged[idx].Purpose)
	}
	if result.Merged[idx].SourceCode != "#!/bin/bash\necho overridden\n" {
		t.Fatalf("override didn't apply: source_code=%q",
			result.Merged[idx].SourceCode)
	}
}

// TestLoadBundledCapabilities_OverridePreservesNotes:
// when the sidecar overrides a compiled entry without
// supplying Notes, the compiled Notes are preserved.
// Lets operators patch source_code without re-typing
// the rationale.
func TestLoadBundledCapabilities_OverridePreservesNotes(t *testing.T) {
	path := writeSidecar(t, `{
		"schema_version": "1.0",
		"capabilities": [
			{
				"stable_id": "cap-seed-list-capabilities",
				"name": "list_capabilities",
				"purpose": "patched purpose",
				"source_language": "bash",
				"requested_domain": "sandbox",
				"source_code": "#!/bin/bash\necho patched\n"
			}
		]
	}`)

	result, err := LoadBundledCapabilities(path)
	if err != nil {
		t.Fatalf("LoadBundledCapabilities: %v", err)
	}
	idx := findSeedCapabilityByStableID(result.Merged, "cap-seed-list-capabilities")
	if idx < 0 {
		t.Fatal("overridden entry missing")
	}
	// Find the compiled Notes for comparison.
	var compiledNotes string
	for _, sc := range SeedCapabilities {
		if sc.StableID == "cap-seed-list-capabilities" {
			compiledNotes = sc.Notes
			break
		}
	}
	if result.Merged[idx].Notes != compiledNotes {
		t.Fatalf("Notes not preserved: got %q, want %q",
			result.Merged[idx].Notes, compiledNotes)
	}
}

// TestLoadBundledCapabilities_AdditionAppends: a sidecar
// entry with stable_id not in the compiled registry is
// appended to Merged.
func TestLoadBundledCapabilities_AdditionAppends(t *testing.T) {
	path := writeSidecar(t, `{
		"schema_version": "1.0",
		"capabilities": [
			{
				"stable_id": "cap-seed-custom-addition",
				"name": "custom_addition",
				"purpose": "An operator-added primitive.",
				"source_language": "bash",
				"requested_domain": "sandbox",
				"source_code": "#!/bin/bash\necho custom\n",
				"tags": ["capability", "custom"]
			}
		]
	}`)

	result, err := LoadBundledCapabilities(path)
	if err != nil {
		t.Fatalf("LoadBundledCapabilities: %v", err)
	}
	wantLen := len(SeedCapabilities) + 1
	if len(result.Merged) != wantLen {
		t.Fatalf("expected len=%d, got %d", wantLen, len(result.Merged))
	}
	if result.SidecarAdditionsCount != 1 {
		t.Fatalf("expected SidecarAdditionsCount=1, got %d",
			result.SidecarAdditionsCount)
	}
	if result.SidecarOverridesCount != 0 {
		t.Fatalf("expected no overrides, got %d",
			result.SidecarOverridesCount)
	}
	idx := findSeedCapabilityByStableID(result.Merged, "cap-seed-custom-addition")
	if idx < 0 {
		t.Fatal("appended entry missing")
	}
	if result.Merged[idx].Name != "custom_addition" {
		t.Fatalf("appended entry has wrong name: %q",
			result.Merged[idx].Name)
	}
}

// TestLoadBundledCapabilities_MixedOverridesAndAdditions:
// the loader handles a sidecar with both overrides and
// additions in the same document.
func TestLoadBundledCapabilities_MixedOverridesAndAdditions(t *testing.T) {
	path := writeSidecar(t, `{
		"schema_version": "1.0",
		"capabilities": [
			{
				"stable_id": "cap-seed-list-capabilities",
				"name": "list_capabilities",
				"purpose": "OVERRIDDEN",
				"source_language": "bash",
				"requested_domain": "sandbox",
				"source_code": "#!/bin/bash\necho overridden\n"
			},
			{
				"stable_id": "cap-seed-new-thing",
				"name": "new_thing",
				"purpose": "New primitive.",
				"source_language": "python",
				"requested_domain": "sandbox",
				"source_code": "print('new thing')\n"
			}
		]
	}`)

	result, err := LoadBundledCapabilities(path)
	if err != nil {
		t.Fatalf("LoadBundledCapabilities: %v", err)
	}
	wantLen := len(SeedCapabilities) + 1 // override doesn't grow len
	if len(result.Merged) != wantLen {
		t.Fatalf("expected len=%d, got %d", wantLen, len(result.Merged))
	}
	if result.SidecarOverridesCount != 1 {
		t.Fatalf("expected 1 override, got %d",
			result.SidecarOverridesCount)
	}
	if result.SidecarAdditionsCount != 1 {
		t.Fatalf("expected 1 addition, got %d",
			result.SidecarAdditionsCount)
	}
}

// TestLoadBundledCapabilities_SidecarPathReturned:
// SidecarPath is the absolute path to the loaded file.
// CLI / MCP use this for the seed report.
func TestLoadBundledCapabilities_SidecarPathReturned(t *testing.T) {
	rel := writeSidecar(t, `{
		"schema_version": "1.0",
		"capabilities": []
	}`)

	result, err := LoadBundledCapabilities(rel)
	if err != nil {
		t.Fatalf("LoadBundledCapabilities: %v", err)
	}
	if result.SidecarPath == "" {
		t.Fatal("expected non-empty SidecarPath")
	}
	// Resolve symlinks for comparison (filepath.Abs
	// returns the symlink path on macOS).
	abs, err := filepath.EvalSymlinks(result.SidecarPath)
	if err != nil {
		t.Fatalf("EvalSymlinks: %v", err)
	}
	want, err := filepath.EvalSymlinks(rel)
	if err != nil {
		t.Fatalf("EvalSymlinks (want): %v", err)
	}
	if abs != want {
		t.Fatalf("SidecarPath: got %q, want %q", abs, want)
	}
}

// TestLoadBundledCapabilities_DefaultPathReadsFromMPMWorkspace:
// when no explicit path is given AND MPM_WORKSPACE points
// at a directory containing bundled_capabilities.json, the
// loader reads it.
func TestLoadBundledCapabilities_DefaultPathReadsFromMPMWorkspace(t *testing.T) {
	ws := t.TempDir()
	sidecarPath := filepath.Join(ws, "bundled_capabilities.json")
	if err := os.WriteFile(sidecarPath, []byte(`{
		"schema_version": "1.0",
		"capabilities": [
			{
				"stable_id": "cap-seed-workspace-addition",
				"name": "workspace_addition",
				"purpose": "Loaded from MPM_WORKSPACE.",
				"source_language": "bash",
				"requested_domain": "sandbox",
				"source_code": "#!/bin/bash\necho ws\n"
			}
		]
	}`), 0644); err != nil {
		t.Fatalf("write sidecar: %v", err)
	}

	t.Setenv("MPM_WORKSPACE", ws)

	result, err := LoadBundledCapabilities("")
	if err != nil {
		t.Fatalf("LoadBundledCapabilities: %v", err)
	}
	if result.SidecarPath == "" {
		t.Fatal("expected SidecarPath populated from MPM_WORKSPACE")
	}
	if result.SidecarAdditionsCount != 1 {
		t.Fatalf("expected 1 addition from workspace sidecar, got %d",
			result.SidecarAdditionsCount)
	}
}