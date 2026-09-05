// skills_save_mode_validation_regression_test.go — Pass 4 defect C.15.
//
// The 2026-09-05 audit found mpm_skills.save silently accepts
// arbitrary strings for `mode`. The registry schema declares
// `mode` with the canonical enum [form|refine] (the workshop
// action's vocabulary), but the save handler did not read the
// field at all — passing `mode: "bogus"` to save was silently
// dropped. Schema enum validation alone is not enough (per the
// brief: do not rely solely on enum), so the handler boundary
// must validate too.
//
// Pre-fix reproduction (live CLI):
//
//   $ mpm call mpm_skills --payload '{"action":"save","params":{"name":"x","version":"1.0.0","content":"---\nname:x\n---\n","mode":"bogus"}}'
//     # wanted: error mentioning the canonical mode vocabulary
//     # actual: success, skill persisted with mode silently dropped
//
// Canonical contract (per registry schema and validateInput at
// internal/core/skill_workshop.go:211):
//
//   omitted        → no mode filter (legitimate)
//   null           → no mode filter (null omission equivalent)
//   ""             → no mode filter (explicit empty matches omission)
//   "form"         → accepted (canonical workshop mode)
//   "refine"       → accepted (canonical workshop mode)
//   "bogus"        → error: must be one of [form, refine]
//   "FORM"         → error (case-sensitive enum)

package tools

import (
	"strings"
	"testing"
)

// TestSkillsSave_ValidModesAccepted pins the positive path: each
// canonical mode value passes validation.
func TestSkillsSave_ValidModesAccepted(t *testing.T) {
	dm := newTestSharedDM(t)
	for _, mode := range []string{"form", "refine"} {
		name := "test-skill-mode-" + mode
		t.Run("mode="+mode, func(t *testing.T) {
			_, err := handleMpmSkills(dm, defaultACForPatch(), map[string]interface{}{
				"action": "save",
				"params": map[string]interface{}{
					"name":    name,
					"version": "1.0.0",
					"content": "---\nname: " + name + "\nversion: 1.0.0\n---\n\nbody",
					"mode":    mode,
				},
			})
			if err != nil {
				t.Errorf("valid mode %q must not error, got: %v", mode, err)
			}
		})
	}
}

// TestSkillsSave_OmittedModeValid pins: omitting mode (key absent
// or value nil or empty string) is the legitimate "no mode" path.
// The fix must not over-correct by requiring mode.
func TestSkillsSave_OmittedModeValid(t *testing.T) {
	dm := newTestSharedDM(t)
	for label, val := range map[string]interface{}{
		"absent": nil,
		"nil":    nil,
		"empty":  "",
	} {
		t.Run(label, func(t *testing.T) {
			name := "test-omit-mode-" + label
			params := map[string]interface{}{
				"name":    name,
				"version": "1.0.0",
				"content": "---\nname: " + name + "\nversion: 1.0.0\n---\n\nbody",
			}
			if val != nil {
				params["mode"] = val
			}
			_, err := handleMpmSkills(dm, defaultACForPatch(), map[string]interface{}{
				"action": "save",
				"params": params,
			})
			if err != nil {
				t.Errorf("omitted/empty mode must not error (%s): %v", label, err)
			}
		})
	}
}

// TestSkillsSave_InvalidModeRejected pins: arbitrary mode strings
// are rejected with a clear error mentioning the canonical
// vocabulary.
func TestSkillsSave_InvalidModeRejected(t *testing.T) {
	dm := newTestSharedDM(t)
	for _, bad := range []string{"bogus", "FORM", "Refine", "create", "0", "delete"} {
		t.Run("mode="+bad, func(t *testing.T) {
			name := "test-bad-mode-" + bad
			_, err := handleMpmSkills(dm, defaultACForPatch(), map[string]interface{}{
				"action": "save",
				"params": map[string]interface{}{
					"name":    name,
					"version": "1.0.0",
					"content": "---\nname: " + name + "\nversion: 1.0.0\n---\n\nbody",
					"mode":    bad,
				},
			})
			if err == nil {
				t.Fatalf("mode=%q must error", bad)
			}
			if !strings.Contains(err.Error(), "mode") {
				t.Errorf("error must mention 'mode', got: %v", err)
			}
		})
	}
}

// TestSkillsSave_InvalidModeDoesNotPersist pins the write-path
// guarantee: an invalid mode does not create a skill row. Skills
// live in memories with collection='skills'.
func TestSkillsSave_InvalidModeDoesNotPersist(t *testing.T) {
	dm := newTestSharedDM(t)

	beforeRow := dm.SQLDB().QueryRow(`SELECT COUNT(*) FROM memories WHERE collection = ?`, "skills")
	beforeCount := 0
	if err := beforeRow.Scan(&beforeCount); err != nil {
		t.Fatalf("count skills before: %v", err)
	}

	name := "test-no-persist-mode"
	_, err := handleMpmSkills(dm, defaultACForPatch(), map[string]interface{}{
		"action": "save",
		"params": map[string]interface{}{
			"name":    name,
			"version": "1.0.0",
			"content": "---\nname: " + name + "\nversion: 1.0.0\n---\n\nbody",
			"mode":    "bogus",
		},
	})
	if err == nil {
		t.Fatalf("invalid mode must error")
	}

	afterRow := dm.SQLDB().QueryRow(`SELECT COUNT(*) FROM memories WHERE collection = ?`, "skills")
	afterCount := 0
	if err := afterRow.Scan(&afterCount); err != nil {
		t.Fatalf("count skills after: %v", err)
	}
	if afterCount != beforeCount {
		t.Errorf("rejected mode must not create a skill; before=%d, after=%d",
			beforeCount, afterCount)
	}
}
