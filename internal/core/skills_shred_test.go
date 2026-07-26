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

// TestShredSkill_NotFoundIsSilent covers the idempotent path: shredding
// an unknown id is a no-op (zero rows affected), not an error. The
// caller already has to know the skill exists to want to delete it,
// and the soft-delete machinery is a delete-by-mark, not a contract
// that the row was previously there.
func TestShredSkill_NotFoundIsSilent(t *testing.T) {
	dm := NewTestDM(t)
	if err := dm.ShredSkill("skill:nope-v9.9.9"); err != nil {
		t.Errorf("ShredSkill on missing id should succeed silently, got: %v", err)
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
