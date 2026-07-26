// skills_security_test.go — Task 10 of the skills-layer plan.
//
// PromoteSkillToGlobal is the third operator-gated shared-DB promotion
// path (alongside record_global_rule and promote_to_global). The
// confirm=true gate mirrors the other two: agents cannot promote a
// skill to global without explicit operator consent. The metadata
// patch stamps derived_from_skill_id so the shared row is traceable
// back to the originating skill id.
//
// Note on the `contains` helper: schema_guard_test.go in package `tools`
// defines `func contains(s []interface{}, v string) bool` for
// JSON-Schema required-array assertions. That helper lives in a
// separate test package and isn't accessible here; we use
// strings.Contains directly for the metadata substring assertion, which
// is what the spec's intent actually requires.

package internal

import (
	"strings"
	"testing"
)

func TestPromoteSkillToGlobal_RequiresConfirm(t *testing.T) {
	dm := NewTestDM(t)
	insertRawSkill(t, dm, "skill:agentshell-v1.0.0", "agentshell", "1.0.0",
		"---\nname: agentshell\nversion: 1.0.0\n---\nbody")

	if err := dm.PromoteSkillToGlobal("skill:agentshell-v1.0.0", false); err == nil {
		t.Fatalf("PromoteSkillToGlobal with confirm=false should reject, got nil error")
	}
}

func TestPromoteSkillToGlobal_PromotesWithConfirm(t *testing.T) {
	dm := NewTestDM(t)
	insertRawSkill(t, dm, "skill:agentshell-v1.0.0", "agentshell", "1.0.0",
		"---\nname: agentshell\nversion: 1.0.0\n---\nbody")

	if err := dm.PromoteSkillToGlobal("skill:agentshell-v1.0.0", true); err != nil {
		t.Fatalf("PromoteSkillToGlobal with confirm=true: %v", err)
	}

	// Verify is_global was flipped.
	var isGlobal int
	if err := dm.db.QueryRow(`SELECT is_global FROM memories WHERE id = ?`, "skill:agentshell-v1.0.0").Scan(&isGlobal); err != nil {
		t.Fatalf("read is_global: %v", err)
	}
	if isGlobal != 1 {
		t.Errorf("is_global = %d, want 1", isGlobal)
	}

	// Verify the lineage metadata was stamped.
	var meta string
	if err := dm.db.QueryRow(`SELECT metadata FROM memories WHERE id = ?`, "skill:agentshell-v1.0.0").Scan(&meta); err != nil {
		t.Fatalf("read metadata: %v", err)
	}
	if !strings.Contains(meta, "derived_from_skill_id") {
		t.Errorf("metadata missing 'derived_from_skill_id' substring, got %q", meta)
	}
}

// TestPromoteSkillToGlobal_RejectsNonSkillID is the privilege-escalation
// guard: the collection='skills' filter must prevent an arbitrary memory
// id (here a decision) from being elevated to is_global=1 through the
// skill-promotion path. Without the guard this promotion would succeed
// and the audit log would record a non-skill row as a "skill" promotion.
func TestPromoteSkillToGlobal_RejectsNonSkillID(t *testing.T) {
	dm := NewTestDM(t)

	// A non-skill row: a decision memory.
	if _, err := dm.db.Exec(`
		INSERT INTO memories (id, collection, content, tags, metadata, weight, is_global, deleted_at)
		VALUES ('mem-decision-1', 'decisions', 'not a skill', '[]', '{}', 5, 0, NULL)
	`); err != nil {
		t.Fatalf("seed decision: %v", err)
	}

	if err := dm.PromoteSkillToGlobal("mem-decision-1", true); err == nil {
		t.Fatalf("PromoteSkillToGlobal on a non-skill id should reject, got nil error")
	}

	// The row must remain local — the guard must not have flipped is_global.
	var isGlobal int
	if err := dm.db.QueryRow(`SELECT is_global FROM memories WHERE id = ?`, "mem-decision-1").Scan(&isGlobal); err != nil {
		t.Fatalf("read is_global: %v", err)
	}
	if isGlobal != 0 {
		t.Errorf("is_global = %d, want 0 (non-skill row must not be promoted)", isGlobal)
	}
}
