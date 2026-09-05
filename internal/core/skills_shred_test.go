// skills_shred_test.go — Task 12 of the skills-layer plan.
//
// ShredSkill is the soft-delete path. The row stays in the DB for
// forensics (deleted_at set), and ReadSkill/ListSkills exclude it via
// their deleted_at IS NULL filter.

package internal

import (
	"testing"
)

func TestShredSkill_HappyPath(t *testing.T) {
	dm := NewTestDM(t)
	insertRawSkill(t, dm, "skill:agentshell-v1.0.0", "agentshell", "1.0.0",
		"---\nname: agentshell\nversion: 1.0.0\n---\nbody")

	if err := dm.ShredSkill("skill:agentshell-v1.0.0"); err != nil {
		t.Fatalf("ShredSkill: %v", err)
	}

	if _, err := dm.ReadSkill("skill:agentshell-v1.0.0", ""); err == nil {
		t.Fatal("shredded skill should not be readable")
	}

	var deletedAt *string
	if err := dm.db.QueryRow(
		`SELECT deleted_at FROM memories WHERE id = ?`, "skill:agentshell-v1.0.0",
	).Scan(&deletedAt); err != nil {
		t.Fatalf("query: %v", err)
	}
	if deletedAt == nil {
		t.Error("deleted_at should be set after shred")
	}
}

// TestShredSkill_UnknownIdIsSilentNoOp pins the contract that
// shredding an id that has no live row is a successful no-op
// (matching the project-wide soft-delete idempotency policy; see
// docs/tool-behavioral-contract.md "Not-found semantics for soft
// deletes" and the precedent at mpm_handoff.shred —
// TestHandoff_DeleteHandoff_Idempotent at handoff_test.go:230).
// The §I-C.9 fix that flipped this to error-on-unknown was reverted
// once the deliberate-design precedent surfaced (commit 0583bea
// designed handoff shred as idempotent and verified the contract
// end-to-end). Forensics: the soft-delete machinery still works
// the same way on live rows (deleted_at mark, recoverable from
// the row) — the unknown-id path just doesn't mutate anything
// because there is no live row to mutate.
func TestShredSkill_UnknownIdIsSilentNoOp(t *testing.T) {
	dm := NewTestDM(t)
	if err := dm.ShredSkill("skill:nope-v9.9.9"); err != nil {
		t.Errorf("ShredSkill on unknown id must be a silent no-op; got: %v", err)
	}
}

// TestShredSkill_DoesNotAffectOtherVersions pins the WHERE clause
// filter: shredding one version must not flip is_latest=false on the
// remaining versions of the same name (that's SaveSkill's job, not
// ShredSkill's).
func TestShredSkill_DoesNotAffectOtherVersions(t *testing.T) {
	dm := NewTestDM(t)
	insertRawSkill(t, dm, "skill:agentshell-v1.0.0", "agentshell", "1.0.0",
		"---\nname: agentshell\nversion: 1.0.0\n---\nbody")
	insertRawSkill(t, dm, "skill:agentshell-v2.0.0", "agentshell", "2.0.0",
		"---\nname: agentshell\nversion: 2.0.0\n---\nbody")

	if err := dm.ShredSkill("skill:agentshell-v1.0.0"); err != nil {
		t.Fatalf("ShredSkill: %v", err)
	}

	// The 2.0.0 row must remain queryable and still be latest.
	if _, err := dm.ReadSkill("skill:agentshell-v2.0.0", ""); err != nil {
		t.Errorf("v2.0.0 should survive; ReadSkill: %v", err)
	}
	if _, err := dm.ReadSkill("agentshell", ""); err != nil {
		t.Errorf("name lookup should resolve to v2.0.0; ReadSkill: %v", err)
	}
}
