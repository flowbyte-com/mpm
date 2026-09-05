// skills_id_vocabulary_regression_test.go — Residual pass §I-C.20.
//
// The 2026-09-05 audit found mpm_skills used two different id
// vocabularies across actions:
//   - save / read    → name (+ version)
//   - delete / promote_to_global → skill_id (the composite id)
//
// This forced callers to construct the canonical id format for
// delete/promote while save/read accepted the semantic name. A
// caller asking "delete skill foo at version 1.0.0" had to know
// the id format `skill:foo-v1.0.0` instead of supplying what they
// already knew (the name and version).
//
// Pre-fix reproduction (live CLI):
//
//   $ mpm call mpm_skills --payload '{"action":"delete","params":{"name":"foo","version":"1.0.0"}}'
//     # wanted: delete the skill (or error explaining how to identify it)
//     # actual: error "skill_id is required"
//
// Canonical contract (residual-pass §I-C.20):
//
//   `skill_id` is the canonical vocabulary (composite id).
//   `name` + `version` is accepted as an alias for caller
//   ergonomics. Both supplied with conflicting values is an
//   error (no silent merge). Neither supplied is an error.
//   Same skill resource → same id, regardless of which key the
//   caller used.
//
//   save    : name+version only (it is the only path that creates
//             a new resource; no canonical id exists yet).
//   read    : skill_id  OR  name+version
//   delete  : skill_id  OR  name+version (requireVersion=true)
//   promote : skill_id  OR  name+version (requireVersion=true)

package tools

import (
	"strings"
	"testing"

	mpminternal "github.com/flowbyte-com/mpm-core"
)

// skillVocabSave seeds a skill with the given name/version so the
// downstream tests can resolve it via either id key.
func skillVocabSave(t *testing.T, dm *mpminternal.DatabaseManager, name, version string) {
	t.Helper()
	_, err := handleMpmSkills(dm, mpminternal.ActiveContext{}, map[string]interface{}{
		"action": "save",
		"params": map[string]interface{}{
			"name":    name,
			"version": version,
			"content": "---\nname: " + name + "\nversion: " + version + "\n---\n\nbody",
		},
	})
	if err != nil {
		t.Fatalf("seed %s/%s: %v", name, version, err)
	}
}

// TestSkillsRead_BothKeysAccepted pins: read works whether the
// caller supplies the canonical skill_id or the legacy
// name+version alias.
func TestSkillsRead_BothKeysAccepted(t *testing.T) {
	dm := newTestSharedDM(t)
	skillVocabSave(t, dm, "vocab-read", "1.0.0")

	// Canonical skill_id.
	res, err := handleMpmSkills(dm, mpminternal.ActiveContext{}, map[string]interface{}{
		"action":   "read",
		"params": map[string]interface{}{"skill_id": "skill:vocab-read-v1.0.0"},
	})
	if err != nil {
		t.Fatalf("read by skill_id: %v", err)
	}
	m, _ := res.(map[string]interface{})
	if m["id"] != "skill:vocab-read-v1.0.0" {
		t.Errorf("read by skill_id: id mismatch, got %v", m["id"])
	}

	// Legacy name+version alias.
	res, err = handleMpmSkills(dm, mpminternal.ActiveContext{}, map[string]interface{}{
		"action": "read",
		"params": map[string]interface{}{
			"name":    "vocab-read",
			"version": "1.0.0",
		},
	})
	if err != nil {
		t.Fatalf("read by name+version: %v", err)
	}
	m, _ = res.(map[string]interface{})
	if m["id"] != "skill:vocab-read-v1.0.0" {
		t.Errorf("read by name+version: id mismatch, got %v", m["id"])
	}
}

// TestSkillsRead_ConflictRejected pins the headline invariant:
// both keys supplied errors rather than silently merges.
func TestSkillsRead_ConflictRejected(t *testing.T) {
	dm := newTestSharedDM(t)
	skillVocabSave(t, dm, "vocab-read-conflict", "1.0.0")

	_, err := handleMpmSkills(dm, mpminternal.ActiveContext{}, map[string]interface{}{
		"action": "read",
		"params": map[string]interface{}{
			"skill_id": "skill:vocab-read-conflict-v1.0.0",
			"name":     "different-name",
			"version":  "2.0.0",
		},
	})
	if err == nil {
		t.Fatal("both keys supplied must error")
	}
	if !strings.Contains(err.Error(), "skill_id") || !strings.Contains(err.Error(), "name") {
		t.Errorf("error must mention both keys, got: %v", err)
	}
}

// TestSkillsDelete_BothKeysAccepted pins: delete works with
// either the canonical skill_id or the name+version alias.
func TestSkillsDelete_BothKeysAccepted(t *testing.T) {
	dm := newTestSharedDM(t)

	// Seed two skills — one deleted via skill_id, one via name+version.
	skillVocabSave(t, dm, "vocab-del-id", "1.0.0")
	skillVocabSave(t, dm, "vocab-del-name", "1.0.0")

	// Delete by skill_id.
	_, err := handleMpmSkills(dm, mpminternal.ActiveContext{}, map[string]interface{}{
		"action": "delete",
		"params": map[string]interface{}{"skill_id": "skill:vocab-del-id-v1.0.0"},
	})
	if err != nil {
		t.Fatalf("delete by skill_id: %v", err)
	}

	// Delete by name+version.
	_, err = handleMpmSkills(dm, mpminternal.ActiveContext{}, map[string]interface{}{
		"action": "delete",
		"params": map[string]interface{}{
			"name":    "vocab-del-name",
			"version": "1.0.0",
		},
	})
	if err != nil {
		t.Fatalf("delete by name+version: %v", err)
	}

	// Both rows soft-deleted (deleted_at set).
	for _, id := range []string{"skill:vocab-del-id-v1.0.0", "skill:vocab-del-name-v1.0.0"} {
		var deletedAt *int64
		err := dm.SQLDB().QueryRow(`SELECT deleted_at FROM memories WHERE id = ?`, id).Scan(&deletedAt)
		if err != nil {
			t.Fatalf("scan %s: %v", id, err)
		}
		if deletedAt == nil {
			t.Errorf("%s: expected soft-deleted (deleted_at != NULL)", id)
		}
	}
}

// TestSkillsDelete_RequiresVersionForNameAlias pins: the
// delete/promote paths require a version when name is supplied
// (requireVersion=true). Omitting version with name errors
// rather than silently picking a "latest".
func TestSkillsDelete_RequiresVersionForNameAlias(t *testing.T) {
	dm := newTestSharedDM(t)
	skillVocabSave(t, dm, "vocab-del-noversion", "1.0.0")

	_, err := handleMpmSkills(dm, mpminternal.ActiveContext{}, map[string]interface{}{
		"action": "delete",
		"params": map[string]interface{}{"name": "vocab-del-noversion"},
	})
	if err == nil {
		t.Fatal("name without version must error on delete")
	}
	if !strings.Contains(err.Error(), "version") {
		t.Errorf("error must mention 'version', got: %v", err)
	}
}

// TestSkillsPromote_BothKeysAccepted pins: promote_to_global
// works with either key.
func TestSkillsPromote_BothKeysAccepted(t *testing.T) {
	dm := newTestSharedDM(t)
	skillVocabSave(t, dm, "vocab-promote-id", "1.0.0")
	skillVocabSave(t, dm, "vocab-promote-name", "1.0.0")

	// promote by skill_id.
	_, err := handleMpmSkills(dm, mpminternal.ActiveContext{}, map[string]interface{}{
		"action": "promote_to_global",
		"params": map[string]interface{}{
			"skill_id": "skill:vocab-promote-id-v1.0.0",
			"confirm":  true,
		},
	})
	if err != nil {
		t.Fatalf("promote by skill_id: %v", err)
	}

	// promote by name+version.
	_, err = handleMpmSkills(dm, mpminternal.ActiveContext{}, map[string]interface{}{
		"action": "promote_to_global",
		"params": map[string]interface{}{
			"name":    "vocab-promote-name",
			"version": "1.0.0",
			"confirm": true,
		},
	})
	if err != nil {
		t.Fatalf("promote by name+version: %v", err)
	}
}

// TestSkillsAction_MissingKeyRejected pins: omitting both keys
// errors for read/delete/promote.
func TestSkillsAction_MissingKeyRejected(t *testing.T) {
	dm := newTestSharedDM(t)

	for _, action := range []string{"read", "delete", "promote_to_global"} {
		t.Run(action, func(t *testing.T) {
			params := map[string]interface{}{}
			if action == "promote_to_global" {
				params["confirm"] = true
			}
			_, err := handleMpmSkills(dm, mpminternal.ActiveContext{}, map[string]interface{}{
				"action": action,
				"params": params,
			})
			if err == nil {
				t.Fatalf("%s without id must error", action)
			}
			if !strings.Contains(err.Error(), "skill_id") &&
				!strings.Contains(err.Error(), "name") {
				t.Errorf("error must mention the id vocabulary, got: %v", err)
			}
		})
	}
}

// TestSkillsSave_VocabularyUnchanged pins: save still uses
// name+version (it is the only path that creates a new skill —
// there is no canonical id yet to refer to). Backwards compat.
func TestSkillsSave_VocabularyUnchanged(t *testing.T) {
	dm := newTestSharedDM(t)

	_, err := handleMpmSkills(dm, mpminternal.ActiveContext{}, map[string]interface{}{
		"action": "save",
		"params": map[string]interface{}{
			"name":    "vocab-save",
			"version": "1.0.0",
			"content": "---\nname: vocab-save\nversion: 1.0.0\n---\n\nbody",
		},
	})
	if err != nil {
		t.Fatalf("save: %v", err)
	}
}
