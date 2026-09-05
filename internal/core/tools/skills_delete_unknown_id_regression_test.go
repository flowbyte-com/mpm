// skills_delete_unknown_id_regression_test.go — Residual pass §I-C.9
// (reverted).
//
// The 2026-09-05 audit originally classified the silent-no-op
// mpm_skills.delete behavior as a defect. The remediation commit
// (45e616f) flipped it to error-on-unknown-id. After surfacing the
// deliberate-design precedent — commit 0583bea explicitly designed
// mpm_handoff.shred as idempotent on unknown id with structured
// shredded=false / rows_deleted=0 / success=true feedback and a
// regression test pinning the contract — the C.9 fix was reverted
// in the §N verdict cycle.
//
// mpm_skills.delete is a SOFT delete (the row stays in the DB with
// deleted_at set; recoverable for forensics), and it now matches
// the project-wide soft-delete policy in
// docs/tool-behavioral-contract.md "Not-found semantics for soft
// deletes". A stale-id race against another agent's earlier delete
// is a benign collision, not a user-facing error.
//
// Canonical contract (per the project-wide soft-delete policy):
//
//	skill_id omitted/empty   → ERROR: skill_id is required
//	skill_id = live row      → success, row soft-deleted (deleted_at set)
//	skill_id = never existed → success, no-op
//	skill_id = soft-deleted  → success, no-op (already in terminal state)

package tools

import (
	"database/sql"
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

// TestSkillsDelete_NeverExistedIsSilentNoOp pins the project-wide
// soft-delete policy: a delete against an id that has no live row
// returns success (matching mpm_handoff.shred's structured no-op
// envelope). See docs/tool-behavioral-contract.md and the precedent
// at TestHandoff_DeleteHandoff_Idempotent (handoff_test.go:230).
func TestSkillsDelete_NeverExistedIsSilentNoOp(t *testing.T) {
	dm := newTestSharedDM(t)

	res, err := handleMpmSkills(dm, mpminternal.ActiveContext{}, map[string]interface{}{
		"action": "delete",
		"params": map[string]interface{}{
			"skill_id": "skill:nonexistent-v1.0.0",
		},
	})
	if err != nil {
		t.Fatalf("delete of unknown id must be a silent no-op; got: %v", err)
	}
	m, ok := res.(map[string]interface{})
	if !ok {
		t.Fatalf("expected map, got %T", res)
	}
	if success, _ := m["success"].(bool); !success {
		t.Errorf("unknown-id delete: expected success=true; got %v", m["success"])
	}
}

// TestSkillsDelete_AlreadySoftDeletedIsSilentNoOp pins the
// idempotency boundary: a second delete of an already-soft-deleted
// skill returns the same success envelope. From the caller's
// perspective the row is already in terminal state and there is
// nothing to do — the soft-delete policy treats this as a benign
// collision, not an error.
func TestSkillsDelete_AlreadySoftDeletedIsSilentNoOp(t *testing.T) {
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

	// Second delete — silent no-op (already in terminal state).
	_, err = handleMpmSkills(dm, mpminternal.ActiveContext{}, map[string]interface{}{
		"action": "delete",
		"params": map[string]interface{}{
			"skill_id": "skill:twice-skill-v1.0.0",
		},
	})
	if err != nil {
		t.Fatalf("second delete of soft-deleted skill must be a silent no-op; got: %v", err)
	}
}

// TestSkillsDelete_EmptySkillIDRejected pins the existing
// pre-check: an empty/missing skill_id errors before any DB call.
// This is the only error path on the soft-delete boundary — see
// docs/tool-behavioral-contract.md.
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

// TestSkillsDelete_DoesNotMutateOtherSkills pins: a soft-delete
// no-op (unknown id) does not affect any other skill row. Seed
// two distinct skills, attempt to delete a third nonexistent id,
// both seeds must remain intact and visible to list/read.
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

	// Bogus delete — silent no-op.
	_, err := handleMpmSkills(dm, mpminternal.ActiveContext{}, map[string]interface{}{
		"action": "delete",
		"params": map[string]interface{}{
			"skill_id": "skill:nonexistent-v1.0.0",
		},
	})
	if err != nil {
		t.Fatalf("bogus delete must be a silent no-op; got: %v", err)
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