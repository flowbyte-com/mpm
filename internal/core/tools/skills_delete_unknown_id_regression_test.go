// skills_delete_unknown_id_regression_test.go — Residual pass §I-C.9.
//
// The 2026-09-05 audit found mpm_skills.delete returned success:true
// for an unknown skill id (silent no-op via the previous ShredSkill
// shape). The caller could not distinguish "deleted live skill" from
// "asked about an id that never existed" — a violation of the
// mutation-must-not-silently-noop invariant.
//
// Pre-fix reproduction (live CLI):
//
//   $ mpm call mpm_skills --payload '{"action":"delete","params":{"skill_id":"skill:nonexistent-v1.0.0"}}'
//     # wanted: error indicating not-found
//     # actual: {"success":true,"skill_id":"skill:nonexistent-v1.0.0"}
//
// Canonical contract (per the residual-pass brief):
//
//   skill_id omitted/empty   → ERROR: skill_id is required
//   skill_id = live row      → success, row soft-deleted (deleted_at set)
//   skill_id = never existed → ERROR: delete_skill: skill_id ... not found
//   skill_id = soft-deleted  → ERROR: not-found (caller sees no live row)
//
// The fix:
//   1. ShredSkill returns internal.ErrSkillNotFound when 0 rows are
//      affected (whether the row never existed or was already
//      soft-deleted — both are "no live row at this id" from the
//      caller's perspective).
//   2. handleDeleteSkill translates ErrSkillNotFound to a clear
//      not-found error envelope.

package tools

import (
	"database/sql"
	"strings"
	"testing"

	mpminternal "github.com/flowbyte-com/mpm-core"
)

// TestSkillsDelete_LiveSkillSucceeds pins the positive path: a
// live skill row is soft-deleted (deleted_at set) and the handler
// returns success. The skill is no longer visible to read/list
// paths.
func TestSkillsDelete_LiveSkillSucceeds(t *testing.T) {
	dm := newTestSharedDM(t)

	// Save a live skill.
	_, err := handleMpmSkills(dm, mpminternal.ActiveContext{}, map[string]interface{}{
		"action": "save",
		"params": map[string]interface{}{
			"name":    "live-skill",
			"version": "1.0.0",
			"content": "---\nname: live-skill\nversion: 1.0.0\n---\n\nbody",
		},
	})
	if err != nil {
		t.Fatalf("seed live skill: %v", err)
	}

	// Delete it.
	res, err := handleMpmSkills(dm, mpminternal.ActiveContext{}, map[string]interface{}{
		"action": "delete",
		"params": map[string]interface{}{
			"skill_id": "skill:live-skill-v1.0.0",
		},
	})
	if err != nil {
		t.Fatalf("delete live skill must succeed: %v", err)
	}
	m, ok := res.(map[string]interface{})
	if !ok {
		t.Fatalf("expected map, got %T", res)
	}
	if success, _ := m["success"].(bool); !success {
		t.Errorf("expected success=true on live delete, got %v", m["success"])
	}

	// Verify the row is soft-deleted (deleted_at set).
	var deletedAt sql.NullInt64
	if err := dm.SQLDB().QueryRow(
		`SELECT deleted_at FROM memories WHERE id = ?`, "skill:live-skill-v1.0.0",
	).Scan(&deletedAt); err != nil {
		t.Fatalf("scan deleted_at: %v", err)
	}
	if !deletedAt.Valid {
		t.Errorf("expected deleted_at set on soft-delete; got NULL")
	}
}

// TestSkillsDelete_NeverExistedRejected pins the headline §I-C.9
// invariant: an unknown skill id must error rather than silently
// succeed.
func TestSkillsDelete_NeverExistedRejected(t *testing.T) {
	dm := newTestSharedDM(t)

	_, err := handleMpmSkills(dm, mpminternal.ActiveContext{}, map[string]interface{}{
		"action": "delete",
		"params": map[string]interface{}{
			"skill_id": "skill:nonexistent-v1.0.0",
		},
	})
	if err == nil {
		t.Fatal("delete of unknown id must error")
	}
	if !strings.Contains(err.Error(), "not found") {
		t.Errorf("error must mention 'not found', got: %v", err)
	}
}

// TestSkillsDelete_AlreadySoftDeletedRejected pins the idempotency
// boundary: a second delete of an already-soft-deleted skill
// surfaces the same not-found. From the caller's perspective
// "delete a live skill" has no live work to do.
func TestSkillsDelete_AlreadySoftDeletedRejected(t *testing.T) {
	dm := newTestSharedDM(t)

	// Seed + first delete.
	_, err := handleMpmSkills(dm, mpminternal.ActiveContext{}, map[string]interface{}{
		"action": "save",
		"params": map[string]interface{}{
			"name":    "twice-skill",
			"version": "1.0.0",
			"content": "---\nname: twice-skill\nversion: 1.0.0\n---\n\nbody",
		},
	})
	if err != nil {
		t.Fatalf("seed: %v", err)
	}
	_, err = handleMpmSkills(dm, mpminternal.ActiveContext{}, map[string]interface{}{
		"action": "delete",
		"params": map[string]interface{}{
			"skill_id": "skill:twice-skill-v1.0.0",
		},
	})
	if err != nil {
		t.Fatalf("first delete must succeed: %v", err)
	}

	// Second delete — must error with not-found (no live row to delete).
	_, err = handleMpmSkills(dm, mpminternal.ActiveContext{}, map[string]interface{}{
		"action": "delete",
		"params": map[string]interface{}{
			"skill_id": "skill:twice-skill-v1.0.0",
		},
	})
	if err == nil {
		t.Fatal("second delete of soft-deleted skill must error")
	}
	if !strings.Contains(err.Error(), "not found") {
		t.Errorf("error must mention 'not found', got: %v", err)
	}
}

// TestSkillsDelete_EmptySkillIDRejected pins the existing
// pre-check: an empty/missing skill_id errors before any DB call.
func TestSkillsDelete_EmptySkillIDRejected(t *testing.T) {
	dm := newTestSharedDM(t)

	for _, empty := range []interface{}{nil, ""} {
		t.Run("empty", func(t *testing.T) {
			params := map[string]interface{}{"action": "delete", "params": map[string]interface{}{}}
			if empty != nil {
				params["params"].(map[string]interface{})["skill_id"] = empty
			}
			_, err := handleMpmSkills(dm, mpminternal.ActiveContext{}, params)
			if err == nil {
				t.Fatal("empty skill_id must error")
			}
		})
	}
}

// TestSkillsDelete_DoesNotMutateOtherSkills pins: a not-found
// delete does not affect any other skill row. Seed two distinct
// skills, attempt to delete a third nonexistent id, both seeds
// must remain intact and visible to list/read.
func TestSkillsDelete_DoesNotMutateOtherSkills(t *testing.T) {
	dm := newTestSharedDM(t)

	for _, name := range []string{"alpha-skill", "beta-skill"} {
		_, err := handleMpmSkills(dm, mpminternal.ActiveContext{}, map[string]interface{}{
			"action": "save",
			"params": map[string]interface{}{
				"name":    name,
				"version": "1.0.0",
				"content": "---\nname: " + name + "\nversion: 1.0.0\n---\n\nbody",
			},
		})
		if err != nil {
			t.Fatalf("seed %s: %v", name, err)
		}
	}

	// Bogus delete.
	_, err := handleMpmSkills(dm, mpminternal.ActiveContext{}, map[string]interface{}{
		"action": "delete",
		"params": map[string]interface{}{
			"skill_id": "skill:nonexistent-v1.0.0",
		},
	})
	if err == nil {
		t.Fatal("bogus delete must error")
	}

	// Both real skills still live (deleted_at NULL).
	for _, name := range []string{"alpha-skill", "beta-skill"} {
		var deletedAt sql.NullInt64
		err := dm.SQLDB().QueryRow(
			`SELECT deleted_at FROM memories WHERE id = ?`, "skill:"+name+"-v1.0.0",
		).Scan(&deletedAt)
		if err != nil {
			t.Fatalf("query %s: %v", name, err)
		}
		if deletedAt.Valid {
			t.Errorf("skill %s: deleted_at must remain NULL after bogus delete", name)
		}
	}
}
