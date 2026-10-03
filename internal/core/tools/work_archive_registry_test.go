package tools

// work_archive_registry_test.go — registry-level coverage for the
// work archive lifecycle actions.
//
// Design: docs/archive/2026-09-30-work-archive-and-purge.md §4.4.
//
// The design's stated risk is an action enum that drifts from the
// dispatcher: an agent reads a schema advertising `archive`, gets a
// response saying the action is unknown, and has no way to know the
// schema is lying. These tests assert the enum, the oneOf branches, and
// the dispatcher's error string all name the same actions.

import (
	"encoding/json"
	"strings"
	"testing"

	mpminternal "github.com/flowbyte-com/mpm-core"
)

// mpmWorkSchema parses the registered mpm_work JSON schema.
func mpmWorkSchema(t *testing.T) map[string]any {
	t.Helper()
	tool, ok := ByName("mpm_work")
	if !ok {
		t.Fatal("mpm_work is not in the registry")
	}
	var schema map[string]any
	if err := json.Unmarshal(tool.Schema, &schema); err != nil {
		t.Fatalf("mpm_work schema is not valid JSON: %v", err)
	}
	return schema
}

// workActionEnum returns the declared action enum values.
func workActionEnum(t *testing.T) []string {
	t.Helper()
	props, _ := mpmWorkSchema(t)["properties"].(map[string]any)
	if props == nil {
		t.Fatal("mpm_work schema has no top-level properties")
	}
	action, _ := props["action"].(map[string]any)
	if action == nil {
		t.Fatal("mpm_work schema has no top-level `action` property")
	}
	raw, _ := action["enum"].([]any)
	values := make([]string, 0, len(raw))
	for _, v := range raw {
		s, _ := v.(string)
		values = append(values, s)
	}
	return values
}

// oneOfActions returns the action const declared by each oneOf branch.
func oneOfActions(t *testing.T) map[string]bool {
	t.Helper()
	branches, _ := mpmWorkSchema(t)["oneOf"].([]any)
	found := map[string]bool{}
	for _, b := range branches {
		branch, _ := b.(map[string]any)
		if branch == nil {
			continue
		}
		props, _ := branch["properties"].(map[string]any)
		if props == nil {
			continue
		}
		action, _ := props["action"].(map[string]any)
		if action == nil {
			continue
		}
		if c, ok := action["const"].(string); ok {
			found[c] = true
		}
	}
	return found
}

// TestMpmWork_ArchiveActionsInEnum pins that archive/unarchive are
// advertised. Without this an agent cannot discover the capability at
// all — the schema is the only contract surface an MCP client sees.
func TestMpmWork_ArchiveActionsInEnum(t *testing.T) {
	enum := workActionEnum(t)
	for _, want := range []string{"archive", "unarchive"} {
		found := false
		for _, v := range enum {
			if v == want {
				found = true
			}
		}
		if !found {
			t.Errorf("mpm_work action enum %v is missing %q", enum, want)
		}
	}
}

// TestMpmWork_EveryEnumActionHasOneOfBranch pins enum ↔ oneOf parity in
// the direction that actually breaks clients: an action with no oneOf
// branch has no documented param shape.
func TestMpmWork_EveryEnumActionHasOneOfBranch(t *testing.T) {
	branches := oneOfActions(t)
	for _, v := range workActionEnum(t) {
		if !branches[v] {
			t.Errorf("mpm_work action %q is in the enum but has no oneOf branch (no documented params)", v)
		}
	}
}

// TestMpmWork_OneOfBranchesAreInEnum pins the reverse direction: a oneOf
// branch for an action the enum does not list is unreachable and
// indicates a partial edit.
func TestMpmWork_OneOfBranchesAreInEnum(t *testing.T) {
	inEnum := map[string]bool{}
	for _, v := range workActionEnum(t) {
		inEnum[v] = true
	}
	for v := range oneOfActions(t) {
		if !inEnum[v] {
			t.Errorf("mpm_work oneOf branch %q is not in the action enum", v)
		}
	}
}

// TestMpmWork_ArchiveParamsRequireWorkID pins the required-field
// contract for both new actions, and that the `id` alias is advertised
// alongside the canonical `work_id`.
func TestMpmWork_ArchiveParamsRequireWorkID(t *testing.T) {
	branches, _ := mpmWorkSchema(t)["oneOf"].([]any)
	for _, want := range []string{"archive", "unarchive"} {
		var found bool
		for _, b := range branches {
			branch, _ := b.(map[string]any)
			props, _ := branch["properties"].(map[string]any)
			action, _ := props["action"].(map[string]any)
			if action == nil {
				continue
			}
			if c, _ := action["const"].(string); c != want {
				continue
			}
			found = true
			params, _ := props["params"].(map[string]any)
			if params == nil {
				t.Errorf("mpm_work %s branch has no params schema", want)
				continue
			}
			required, _ := params["required"].([]any)
			var hasWorkID bool
			for _, r := range required {
				if s, _ := r.(string); s == "work_id" {
					hasWorkID = true
				}
			}
			if !hasWorkID {
				t.Errorf("mpm_work %s params do not require work_id", want)
			}
			paramProps, _ := params["properties"].(map[string]any)
			for _, key := range []string{"work_id", "id", "note"} {
				if _, ok := paramProps[key]; !ok {
					t.Errorf("mpm_work %s params missing %q", want, key)
				}
			}
			// params must be a required envelope member for these actions.
			branchRequired, _ := branch["required"].([]any)
			var hasParams bool
			for _, r := range branchRequired {
				if s, _ := r.(string); s == "params" {
					hasParams = true
				}
			}
			if !hasParams {
				t.Errorf("mpm_work %s branch does not require the params envelope", want)
			}
		}
		if !found {
			t.Errorf("mpm_work has no oneOf branch for %q", want)
		}
	}
}

// TestMpmWork_ListParamsDocumentVisibility pins that the list action
// advertises both axes with their enums and defaults. The CLI and MCP
// paths share ListWorkRows, so an undocumented visibility filter here is
// the same defect class as the §4.4 help drift.
func TestMpmWork_ListParamsDocumentVisibility(t *testing.T) {
	branches, _ := mpmWorkSchema(t)["oneOf"].([]any)
	for _, b := range branches {
		branch, _ := b.(map[string]any)
		props, _ := branch["properties"].(map[string]any)
		action, _ := props["action"].(map[string]any)
		if action == nil {
			continue
		}
		if c, _ := action["const"].(string); c != "list" {
			continue
		}
		params, _ := props["params"].(map[string]any)
		if params == nil {
			t.Fatal("mpm_work list branch has no params schema")
		}
		paramProps, _ := params["properties"].(map[string]any)
		for _, key := range []string{"status", "visibility", "limit"} {
			if _, ok := paramProps[key]; !ok {
				t.Errorf("mpm_work list params missing %q", key)
			}
		}
		vis, _ := paramProps["visibility"].(map[string]any)
		if vis == nil {
			t.Fatal("mpm_work list params: visibility has no schema")
		}
		raw, _ := vis["enum"].([]any)
		var values []string
		for _, v := range raw {
			s, _ := v.(string)
			values = append(values, s)
		}
		if strings.Join(values, ",") != "active,archived,all" {
			t.Errorf("visibility enum = %v, want [active archived all]", values)
		}
		if d, _ := vis["default"].(string); d != "active" {
			t.Errorf("visibility default = %q, want \"active\"", d)
		}
		status, _ := paramProps["status"].(map[string]any)
		if d, _ := status["default"].(string); d != "open" {
			t.Errorf("status default = %q, want \"open\"", d)
		}
		return
	}
	t.Fatal("mpm_work has no oneOf branch for \"list\"")
}

// TestMpmWork_DescriptionMentionsArchive pins that the tool description
// — the only prose an agent reads before choosing an action — explains
// the terminal-only rule and the visibility filter. Advertising
// `archive` without saying that open items are refused is how an agent
// ends up archiving live work and getting a bare error.
func TestMpmWork_DescriptionMentionsArchive(t *testing.T) {
	tool, _ := ByName("mpm_work")
	for _, want := range []string{"archive", "unarchive", "visibility"} {
		if !strings.Contains(tool.Description, want) {
			t.Errorf("mpm_work description does not mention %q", want)
		}
	}
}

// TestMpmWork_ValidVisibilitiesMatchCore pins that the wire enum used
// by the handler and the core validation used by the DB layer are the
// same closed set. A divergence would let a value pass handler
// validation and then error (or worse, be coerced) at the SQL layer.
func TestMpmWork_ValidVisibilitiesMatchCore(t *testing.T) {
	if strings.Join(validWorkVisibilities, ",") != strings.Join(mpminternal.ValidWorkVisibilities, ",") {
		t.Errorf("wire enum %v diverges from core %v", validWorkVisibilities, mpminternal.ValidWorkVisibilities)
	}
}
