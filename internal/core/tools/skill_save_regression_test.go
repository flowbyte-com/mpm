// skill_save_regression_test.go — regression tests for alpha-4 W-005
// (lower-friction skill save).
//
// Pins:
//  1. handleSaveSkill aggregates ALL validation errors instead of
//     failing on the first one (was: only the first error was
//     surfaced).
//  2. Errors are returned as a `{success:false, errors:[...]}` payload,
//     NOT as a Go error — so the wire envelope remains parseable.
//  3. The body-shortcut path (body instead of content) synthesises
//     frontmatter and round-trips through ParseSkillFrontmatter.
//  4. content AND body supplied → "either content or body, not both"
//     in the errors array (the wrong path was silently picking one).

package tools

import (
	"encoding/json"
	"strings"
	"testing"

	mpminternal "github.com/flowbyte-com/mpm-core"
)

// TestHandleSaveSkill_AggregatesAllErrors is the alpha-4 W-005 pin
// for the failure-shape contract: missing name AND missing version
// must BOTH appear in the errors array, not just the first one.
func TestHandleSaveSkill_AggregatesAllErrors(t *testing.T) {
	dm := newTestIsolatedDM(t)

	res, err := handleMpmSkills(dm, mpminternal.ActiveContext{}, map[string]interface{}{
		"action": "save",
		"params": map[string]interface{}{
			// Empty payload — all required fields missing.
		},
	})
	if err != nil {
		t.Fatalf("expected success:false payload (NOT Go error), got err: %v", err)
	}

	raw := mustMarshalJSON(res)
	var envelope struct {
		Success bool     `json:"success"`
		Errors  []string `json:"errors"`
	}
	if err := json.Unmarshal([]byte(raw), &envelope); err != nil {
		t.Fatalf("unmarshal: %v (raw=%s)", err, raw)
	}
	if envelope.Success {
		t.Fatalf("success = true on empty payload; payload=%s", raw)
	}

	// All three must-aggregate errors must be present.
	wantSubstrings := []string{"missing name", "missing version", "missing content or body"}
	for _, want := range wantSubstrings {
		found := false
		for _, e := range envelope.Errors {
			if strings.Contains(e, want) {
				found = true
				break
			}
		}
		if !found {
			t.Errorf("errors array missing %q; got %+v (raw=%s)", want, envelope.Errors, raw)
		}
	}
}

// TestHandleSaveSkill_BothContentAndBodyRejected pins the rejection
// of the "send both" anti-pattern. The previous behaviour was to
// silently pick content (whichever was first), which trains agents
// to believe their body was never persisted.
func TestHandleSaveSkill_BothContentAndBodyRejected(t *testing.T) {
	dm := newTestIsolatedDM(t)

	res, err := handleMpmSkills(dm, mpminternal.ActiveContext{}, map[string]interface{}{
		"action": "save",
		"params": map[string]interface{}{
			"name":    "w005-both",
			"version": "1.0.0",
			"content": "---\nname: w005-both\nversion: 1.0.0\n---\n\nfrom content",
			"body":    "from body",
		},
	})
	if err != nil {
		t.Fatalf("expected success:false payload, got err: %v", err)
	}

	raw := mustMarshalJSON(res)
	var envelope struct {
		Success bool     `json:"success"`
		Errors  []string `json:"errors"`
	}
	if err := json.Unmarshal([]byte(raw), &envelope); err != nil {
		t.Fatalf("unmarshal: %v (raw=%s)", err, raw)
	}
	if envelope.Success {
		t.Fatalf("success = true on both-set; payload=%s", raw)
	}

	found := false
	for _, e := range envelope.Errors {
		if strings.Contains(e, "either content or body, not both") {
			found = true
			break
		}
	}
	if !found {
		t.Errorf("errors array missing the both-set rejection; got %+v", envelope.Errors)
	}
}

// TestHandleSaveSkill_BodyShortcutSucceeds is the alpha-4 W-005 pin
// for the structured-args shortcut: an agent can save a skill by
// passing just `name`, `version`, and `body` without hand-rolling
// frontmatter. The synthesised content must parse through
// ParseSkillFrontmatter on read-back.
func TestHandleSaveSkill_BodyShortcutSucceeds(t *testing.T) {
	dm := newTestIsolatedDM(t)

	res, err := handleMpmSkills(dm, mpminternal.ActiveContext{}, map[string]interface{}{
		"action": "save",
		"params": map[string]interface{}{
			"name":    "w005-shortcut",
			"version": "1.0.0",
			"body":    "# Body\n\nPlain markdown body without frontmatter.",
		},
	})
	if err != nil {
		t.Fatalf("body-shortcut save: %v", err)
	}

	raw := mustMarshalJSON(res)
	var envelope struct {
		Success bool   `json:"success"`
		ID      string `json:"id"`
		Name    string `json:"name"`
		Version string `json:"version"`
	}
	if err := json.Unmarshal([]byte(raw), &envelope); err != nil {
		t.Fatalf("unmarshal: %v (raw=%s)", err, raw)
	}
	if !envelope.Success {
		t.Fatalf("success = false on valid body-shortcut; payload=%s", raw)
	}
	if envelope.Name != "w005-shortcut" || envelope.Version != "1.0.0" {
		t.Errorf("echo mismatch: name=%q version=%q (raw=%s)", envelope.Name, envelope.Version, raw)
	}
	if envelope.ID == "" {
		t.Errorf("id missing from response; payload=%s", raw)
	}

	// Round-trip: read the skill back and confirm body survived.
	read, err := handleMpmSkills(dm, mpminternal.ActiveContext{}, map[string]interface{}{
		"action": "read",
		"params": map[string]interface{}{"name": "w005-shortcut"},
	})
	if err != nil {
		t.Fatalf("read-back: %v", err)
	}
	readRaw := mustMarshalJSON(read)
	if !strings.Contains(readRaw, "Plain markdown body without frontmatter") {
		t.Errorf("read-back dropped body; payload=%s", readRaw)
	}
}

// TestHandleSaveSkill_ContentPathStillWorks is the regression pin
// for the existing content path: passing full markdown with embedded
// frontmatter must still persist, and the existing test surface
// (workshop → save_payload → save) must keep working.
func TestHandleSaveSkill_ContentPathStillWorks(t *testing.T) {
	dm := newTestIsolatedDM(t)

	res, err := handleMpmSkills(dm, mpminternal.ActiveContext{}, map[string]interface{}{
		"action": "save",
		"params": map[string]interface{}{
			"name":    "w005-content-path",
			"version": "1.2.3",
			"content": "---\nname: w005-content-path\nversion: 1.2.3\ndescription: explicit content path\n---\n\n# Existing path\n",
		},
	})
	if err != nil {
		t.Fatalf("content-path save: %v", err)
	}

	raw := mustMarshalJSON(res)
	if !strings.Contains(raw, `"success":true`) {
		t.Errorf("content path did not succeed; payload=%s", raw)
	}
}
