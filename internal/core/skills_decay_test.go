// skills_decay_test.go — Task 11 of the skills-layer plan.
//
// Skills are LTM (is_long_term=1) by default; that's the 90-day floor
// mechanism. The production decay sweep in DatabaseManager.DecayWeights
// uses a step-down formula on is_long_term=1 rows:
//
//	weight = MAX(weight - MAX(1, CAST(weight * decayFactor AS INTEGER)), 1)
//	WHERE is_long_term = 1 AND weight > 1
//
// The `AND weight > 1` floor is the contract: a skill's weight can
// never drop below 1, so even an ancient skill remains queryable.
// Tests assert (a) SaveSkill writes is_long_term=1, and (b) the
// production decay sweep applies but cannot push weight below 1.
//
// The decay tests backdate updated_at via a raw UPDATE because
// SaveSkill always writes a fresh row — we need a fixture whose
// updated_at is ancient enough that a single sweep lands on the floor.

package internal

import (
	"testing"
)

func TestSaveSkill_SetsIsLongTerm(t *testing.T) {
	dm := NewTestDM(t)

	if _, err := dm.SaveSkill("agentshell", "1.0.0",
		"---\nname: agentshell\nversion: 1.0.0\n---\nbody", "test-agent", false); err != nil {
		t.Fatalf("SaveSkill: %v", err)
	}

	var isLTM int
	if err := dm.db.QueryRow(
		`SELECT is_long_term FROM memories WHERE id = ?`,
		"skill:agentshell-v1.0.0",
	).Scan(&isLTM); err != nil {
		t.Fatalf("query: %v", err)
	}
	if isLTM != 1 {
		t.Errorf("is_long_term = %d, want 1 (skills are LTM by default)", isLTM)
	}
}

// seedAgedSkill inserts a skill whose decay-relevant columns are set to
// pre-conditions the sweep needs (weight just above the floor, old
// updated_at, no reinforcement). content uses real newlines so the
// frontmatter parser that ReadSkill calls on the way to the row is
// happy. Returns the inserted id.
func seedAgedSkill(t *testing.T, dm *DatabaseManager, name string, weight int, isLTM int) string {
	t.Helper()
	content := "---\nname: " + name + "\nversion: 1.0.0\n---\nbody"
	id := "skill:" + name + "-v1.0.0"
	if _, err := dm.SaveSkill(name, "1.0.0", content, "test-agent", false); err != nil {
		t.Fatalf("seed SaveSkill: %v", err)
	}
	if _, err := dm.db.Exec(`
		UPDATE memories
		SET weight = ?, is_long_term = ?,
		    updated_at = datetime('now', '-500 days'),
		    last_accessed_at = datetime('now', '-500 days'),
		    created_at = datetime('now', '-500 days')
		WHERE id = ?
	`, weight, isLTM, id); err != nil {
		t.Fatalf("seed backdate UPDATE: %v", err)
	}
	return id
}

// TestDecayFloor_SkillsSurviveAggressiveDecay pins the 90-day-floor
// contract: even with a high decay rate the LTM clamp (`AND weight > 1`
// in the sweep's WHERE clause) prevents weight from dropping below 1,
// so the skill stays queryable.
//
// Start at weight 2 (just above the floor) with updated_at backdated
// 500 days. After the sweep, weight must be exactly 1 — the floor.
// The ReadSkill check confirms the row survived, which means the
// floor kept it out of both the sweep and any downstream archival.
func TestDecayFloor_SkillsSurviveAggressiveDecay(t *testing.T) {
	dm := NewTestDM(t)
	id := seedAgedSkill(t, dm, "aged", 2, 1)

	policy := map[string]DecayPolicy{
		"skills": {DecayPercent: 50.0, Floor: 1},
	}
	if _, err := dm.DecayWeights(policy, 30); err != nil {
		t.Fatalf("DecayWeights: %v", err)
	}

	var weight int
	if err := dm.db.QueryRow(`SELECT weight FROM memories WHERE id = ?`, id).Scan(&weight); err != nil {
		t.Fatalf("query weight: %v", err)
	}
	if weight != 1 {
		t.Errorf("skill weight = %d, want 1 (LTM floor must clamp)", weight)
	}

	if _, err := dm.ReadSkill(id, ""); err != nil {
		t.Errorf("clamped skill should remain queryable; ReadSkill: %v", err)
	}
}

// TestDecayFloor_NonLTMRowUntouchedByDecaySweep is the contrast: a row
// in the skills collection WITHOUT is_long_term=1 is not touched by the
// production decay sweep at all. The sweep is gated on is_long_term=1,
// so a non-LTM row must keep its weight exactly.
func TestDecayFloor_NonLTMRowUntouchedByDecaySweep(t *testing.T) {
	dm := NewTestDM(t)
	id := seedAgedSkill(t, dm, "ephemeral", 5, 0)

	policy := map[string]DecayPolicy{
		"skills": {DecayPercent: 50.0, Floor: 1},
	}
	if _, err := dm.DecayWeights(policy, 30); err != nil {
		t.Fatalf("DecayWeights: %v", err)
	}

	var weight int
	if err := dm.db.QueryRow(`SELECT weight FROM memories WHERE id = ?`, id).Scan(&weight); err != nil {
		t.Fatalf("query weight: %v", err)
	}
	if weight != 5 {
		t.Errorf("non-LTM skill weight = %d, want 5 (sweep is_long_term=1 gated; non-LTM rows untouched)", weight)
	}
}
