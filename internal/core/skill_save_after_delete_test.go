package internal

import (
	"strings"
	"testing"
)

// TestSkill_SaveAfterDelete_Resurrects (2026-09-05 audit P1) is the
// regression for the documented save-after-delete lifecycle:
//
//	save  → live row exists (id owned)
//	delete → soft-delete (deleted_at set; id still owned by tombstone)
//	save  → resurrect (force=true) OR error (force=false)
//
// Pre-fix behaviour: the existence check at skill_db.go:345 was
//
//	SELECT id FROM memories WHERE id = ? AND deleted_at IS NULL
//
// which hides the tombstone. The new-row INSERT path at line 540 then
// collided with the tombstone's PK because the tombstone still owns
// the id — UNIQUE constraint violation on `memories.id`.
//
// The intended contract (derived from SaveSkill's documented semantics
// and the existing TestSaveSkill_DuplicateVersionWithForce surface):
//   - save + force=true overwrites a same-(name,version) row in place.
//   - save + force=false against an existing id returns
//     "already exists" — even if the row is tombstoned, because the
//     id is still owned.
//   - the tombstone is the audit trail; the resurrection (force=true)
//     replaces it with a live row carrying the new content.
//
// After the fix:
//   - existence check includes tombstones (id is owned regardless of
//     deleted_at).
//   - force=true overwrites live OR tombstoned rows (UPDATE…WHERE id = ?
//     with no `deleted_at IS NULL` guard, also resetting deleted_at).
//   - force=false returns "already exists" for live OR tombstoned ids.
func TestSkill_SaveAfterDelete_Resurrects(t *testing.T) {
	dm := NewTestDM(t)

	content := "---\nname: agentshell\nversion: 1.0.0\n---\nORIGINAL BODY"

	id, err := dm.SaveSkill("agentshell", "1.0.0", content, "test-agent", false)
	if err != nil {
		t.Fatalf("first save: %v", err)
	}

	if err := dm.ShredSkill(id); err != nil {
		t.Fatalf("shred: %v", err)
	}

	// After delete, ReadSkill must NOT find it (tombstones hidden by
	// deleted_at IS NULL in the read path).
	if _, err := dm.ReadSkill(id, ""); err == nil {
		t.Fatalf("expected ReadSkill to fail after delete, got success")
	}

	// Save with force=true: must succeed and resurrect the row.
	resurrectedContent := "---\nname: agentshell\nversion: 1.0.0\n---\nRESURRECTED BODY"
	id2, err := dm.SaveSkill("agentshell", "1.0.0", resurrectedContent, "test-agent", true)
	if err != nil {
		t.Fatalf("force save after delete: %v", err)
	}
	if id2 != id {
		t.Errorf("force save after delete: id mismatch %q vs %q", id2, id)
	}

	// ReadSkill must now return the resurrected body.
	over, err := dm.ReadSkill(id, "")
	if err != nil {
		t.Fatalf("ReadSkill after resurrection: %v", err)
	}
	if !strings.Contains(over.Body, "RESURRECTED BODY") {
		t.Errorf("resurrected body = %q, want contains RESURRECTED BODY", over.Body)
	}
}

// TestSkill_SaveAfterDelete_WithoutForce_Errors documents the same-id
// contract: a deleted skill still owns its id, so re-saving without
// force is a contract violation. The error message must clearly
// signal "already exists" so callers know to retry with force=true.
func TestSkill_SaveAfterDelete_WithoutForce_Errors(t *testing.T) {
	dm := NewTestDM(t)

	content := "---\nname: agentshell\nversion: 1.0.0\n---\nORIGINAL"

	id, err := dm.SaveSkill("agentshell", "1.0.0", content, "test-agent", false)
	if err != nil {
		t.Fatalf("first save: %v", err)
	}

	if err := dm.ShredSkill(id); err != nil {
		t.Fatalf("shred: %v", err)
	}

	// Save without force against the same (name, version): must error
	// (the tombstone still owns the id, so the existence check should
	// see it and return "already exists").
	_, err = dm.SaveSkill("agentshell", "1.0.0", content, "test-agent", false)
	if err == nil {
		t.Fatalf("expected 'already exists' error on save-after-delete without force, got nil")
	}
	if !strings.Contains(err.Error(), "already exists") {
		t.Errorf("expected 'already exists' error, got: %v", err)
	}

	// Tombstone must still be present (we did not accidentally
	// resurrect it). ReadSkill must still fail.
	if _, err := dm.ReadSkill(id, ""); err == nil {
		t.Errorf("tombstone should remain hidden from read path; got success")
	}
}

// TestSkill_SaveSave_OverwriteWithForce documents the existing
// documented contract (force=true in-place overwrite). The audit
// regression suite must not break this pre-existing behaviour.
func TestSkill_SaveSave_OverwriteWithForce(t *testing.T) {
	dm := NewTestDM(t)

	content1 := "---\nname: agentshell\nversion: 1.0.0\n---\nORIGINAL"
	id1, err := dm.SaveSkill("agentshell", "1.0.0", content1, "test-agent", false)
	if err != nil {
		t.Fatalf("first save: %v", err)
	}

	content2 := "---\nname: agentshell\nversion: 1.0.0\n---\nOVERWRITTEN"
	id2, err := dm.SaveSkill("agentshell", "1.0.0", content2, "test-agent", true)
	if err != nil {
		t.Fatalf("force overwrite: %v", err)
	}
	if id1 != id2 {
		t.Errorf("force should reuse same id, got %q vs %q", id1, id2)
	}

	over, err := dm.ReadSkill(id1, "")
	if err != nil {
		t.Fatalf("ReadSkill: %v", err)
	}
	if !strings.Contains(over.Body, "OVERWRITTEN") {
		t.Errorf("force overwrite body = %q, want contains OVERWRITTEN", over.Body)
	}
}

// TestSkill_SaveSave_WithoutForce_Errors: documented existing behaviour.
// Saving the same (name, version) twice without force is a contract
// violation; the caller must explicitly opt in to overwrite via
// force=true.
func TestSkill_SaveSave_WithoutForce_Errors(t *testing.T) {
	dm := NewTestDM(t)

	content := "---\nname: agentshell\nversion: 1.0.0\n---\nbody"
	if _, err := dm.SaveSkill("agentshell", "1.0.0", content, "test-agent", false); err != nil {
		t.Fatalf("first save: %v", err)
	}

	_, err := dm.SaveSkill("agentshell", "1.0.0", content, "test-agent", false)
	if err == nil {
		t.Fatalf("expected 'already exists' on duplicate save without force, got nil")
	}
	if !strings.Contains(err.Error(), "already exists") {
		t.Errorf("expected 'already exists' error, got: %v", err)
	}
}
