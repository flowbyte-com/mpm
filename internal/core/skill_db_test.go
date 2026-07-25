// skill_db_test.go — ReadSkill and ListSkills read-path tests.
//
// The fixtures seed raw rows directly into the memories table with
// collection='skills'. The SUT must:
//   - Look up by exact id (skill:<name>-v<version>).
//   - Fall back to name resolution (latest version wins) when no exact
//     id matches.
//   - Return an error for unknown names.
//   - Deduplicate by name on list, keeping the highest version.
//   - Honour the scope filter (local/shared/all).
//
// Tests run against a hermetic in-memory DM via NewTestDM(t); we do NOT
// use NewDatabaseManager directly because that helper derives its path
// from MPM_WORKSPACE / config and would either fail or pollute the
// workspace database.

package internal

import (
	"testing"
)

func TestSaveSkill_NewSkill(t *testing.T) {
	dm := NewTestDM(t)

	content := "---\nname: agentshell\nversion: 2.0.0\nwhen_to_use: agentshell\n---\nbody"
	id, err := dm.SaveSkill("agentshell", "2.0.0", content, "test-agent", false)
	if err != nil {
		t.Fatalf("SaveSkill: %v", err)
	}
	if id != "skill:agentshell-v2.0.0" {
		t.Errorf("id = %q, want skill:agentshell-v2.0.0", id)
	}

	// Verify the row exists and is marked is_latest.
	got, err := dm.ReadSkill("agentshell", "")
	if err != nil {
		t.Fatalf("ReadSkill: %v", err)
	}
	if got.Version != "2.0.0" {
		t.Errorf("version = %q, want 2.0.0", got.Version)
	}
	if !got.IsLatest {
		t.Errorf("IsLatest = false, want true")
	}
}

func TestSaveSkill_VersionBumpFlipsIsLatest(t *testing.T) {
	dm := NewTestDM(t)

	id1, err := dm.SaveSkill("agentshell", "1.0.0",
		"---\nname: agentshell\nversion: 1.0.0\n---\nbody v1", "test-agent", false)
	if err != nil {
		t.Fatalf("save v1: %v", err)
	}
	id2, err := dm.SaveSkill("agentshell", "2.0.0",
		"---\nname: agentshell\nversion: 2.0.0\n---\nbody v2", "test-agent", false)
	if err != nil {
		t.Fatalf("save v2: %v", err)
	}

	// ReadSkill(name) should resolve to v2.0.0.
	latest, err := dm.ReadSkill("agentshell", "")
	if err != nil {
		t.Fatalf("ReadSkill: %v", err)
	}
	if latest.ID != id2 {
		t.Errorf("latest = %s, want %s", latest.ID, id2)
	}
	if !latest.IsLatest {
		t.Errorf("latest IsLatest = false, want true")
	}

	// Old version should still be queryable by exact id and marked not-latest.
	old, err := dm.ReadSkill(id1, "")
	if err != nil {
		t.Fatalf("ReadSkill v1: %v", err)
	}
	if old.Version != "1.0.0" {
		t.Errorf("v1 version = %q, want 1.0.0", old.Version)
	}
	if old.IsLatest {
		t.Errorf("v1 IsLatest = true, want false after v2.0.0 bump")
	}
}

func TestSaveSkill_DuplicateVersionRejected(t *testing.T) {
	dm := NewTestDM(t)

	_, err := dm.SaveSkill("agentshell", "1.0.0",
		"---\nname: agentshell\nversion: 1.0.0\n---\nbody", "test-agent", false)
	if err != nil {
		t.Fatalf("first save: %v", err)
	}
	_, err = dm.SaveSkill("agentshell", "1.0.0",
		"---\nname: agentshell\nversion: 1.0.0\n---\nbody v2", "test-agent", false)
	if err == nil {
		t.Fatal("expected error for duplicate version without force")
	}
}

func TestSaveSkill_DuplicateVersionWithForce(t *testing.T) {
	dm := NewTestDM(t)

	id1, err := dm.SaveSkill("agentshell", "1.0.0",
		"---\nname: agentshell\nversion: 1.0.0\n---\nORIGINAL", "test-agent", false)
	if err != nil {
		t.Fatalf("first save: %v", err)
	}
	id2, err := dm.SaveSkill("agentshell", "1.0.0",
		"---\nname: agentshell\nversion: 1.0.0\n---\nOVERWRITTEN", "test-agent", true)
	if err != nil {
		t.Fatalf("force save: %v", err)
	}
	if id1 != id2 {
		t.Errorf("force should overwrite same id, got %s vs %s", id1, id2)
	}

	// Read back to confirm in-place overwrite. The body should be the
	// full markdown after the closing fence, with a leading newline
	// stripped (matching ParseSkillFrontmatter's contract).
	over, err := dm.ReadSkill(id1, "")
	if err != nil {
		t.Fatalf("ReadSkill after force: %v", err)
	}
	if over.Version != "1.0.0" {
		t.Errorf("version = %q, want 1.0.0", over.Version)
	}
	if over.Body != "OVERWRITTEN" {
		t.Errorf("body = %q, want OVERWRITTEN (force overwrite should replace body)", over.Body)
	}
}

func TestSaveSkill_ForcePreservesWeight(t *testing.T) {
	dm := NewTestDM(t)

	id, err := dm.SaveSkill("agentshell", "1.0.0",
		"---\nname: agentshell\nversion: 1.0.0\n---\nbody", "test-agent", false)
	if err != nil {
		t.Fatalf("first save: %v", err)
	}

	// Simulate prior reinforcement: a battle-tested skill that has been
	// useful 8 times and accumulated weight=13. Force-overwrite must
	// preserve both columns — weight encodes retrieval trajectory, and
	// resetting it to the seed default of 5 would silently bury the
	// skill under newer-looking alternatives.
	_, err = dm.db.Exec(`UPDATE memories SET weight = 13, reinforcement_count = 8 WHERE id = ?`, id)
	if err != nil {
		t.Fatalf("set weight/reinforcement: %v", err)
	}

	_, err = dm.SaveSkill("agentshell", "1.0.0",
		"---\nname: agentshell\nversion: 1.0.0\n---\nbody updated", "test-agent", true)
	if err != nil {
		t.Fatalf("force save: %v", err)
	}

	// Re-read directly from the DB to assert the columns survived.
	var weight, reinforcement int
	err = dm.db.QueryRow(`SELECT weight, reinforcement_count FROM memories WHERE id = ?`, id).Scan(&weight, &reinforcement)
	if err != nil {
		t.Fatalf("re-read columns: %v", err)
	}
	if weight != 13 {
		t.Errorf("weight after force-overwrite = %d, want 13 (preserved)", weight)
	}
	if reinforcement != 8 {
		t.Errorf("reinforcement_count after force-overwrite = %d, want 8 (preserved)", reinforcement)
	}
}

func insertRawSkill(t *testing.T, dm *DatabaseManager, id, name, version, content string) {
	t.Helper()
	_, err := dm.db.Exec(`
		INSERT INTO memories (id, collection, content, is_prime_directive, tags, metadata, weight, is_global, deleted_at)
		VALUES (?, 'skills', ?, 0, '["skill"]', '{}', 5, 0, NULL)
	`, id, content)
	if err != nil {
		t.Fatalf("insert skill %s: %v", id, err)
	}
}

func TestReadSkill_ByExactID(t *testing.T) {
	dm := NewTestDM(t)

	insertRawSkill(t, dm, "skill:agentshell-v2.0.0", "agentshell", "2.0.0",
		"---\nname: agentshell\nversion: 2.0.0\nwhen_to_use: agentshell, theme\n---\nbody")

	skill, err := dm.ReadSkill("skill:agentshell-v2.0.0", "")
	if err != nil {
		t.Fatalf("ReadSkill: %v", err)
	}
	if skill.Name != "agentshell" {
		t.Errorf("name = %q, want agentshell", skill.Name)
	}
	if skill.Version != "2.0.0" {
		t.Errorf("version = %q, want 2.0.0", skill.Version)
	}
	if skill.Body == "" {
		t.Error("body should be populated")
	}
}

func TestReadSkill_IsLatestFromMetadata(t *testing.T) {
	dm := NewTestDM(t)

	insertRawSkill(t, dm, "skill:agentshell-v2.0.0", "agentshell", "2.0.0",
		"---\nname: agentshell\nversion: 2.0.0\n---\nbody")
	_, err := dm.db.Exec(`UPDATE memories SET metadata = '{"is_latest":true}' WHERE id = 'skill:agentshell-v2.0.0'`)
	if err != nil {
		t.Fatalf("set is_latest metadata: %v", err)
	}

	skill, err := dm.ReadSkill("skill:agentshell-v2.0.0", "")
	if err != nil {
		t.Fatalf("ReadSkill: %v", err)
	}
	if !skill.IsLatest {
		t.Error("IsLatest = false, want true from metadata.is_latest")
	}
}

func TestReadSkill_ContentHash(t *testing.T) {
	dm := NewTestDM(t)

	insertRawSkill(t, dm, "skill:agentshell-v2.0.0", "agentshell", "2.0.0",
		"---\nname: agentshell\nversion: 2.0.0\n---\nbody")
	const wantHash = "abc123def456"
	_, err := dm.db.Exec(`UPDATE memories SET metadata = ? WHERE id = 'skill:agentshell-v2.0.0'`,
		`{"is_latest":true,"content_hash":"`+wantHash+`"}`)
	if err != nil {
		t.Fatalf("set content_hash metadata: %v", err)
	}

	skill, err := dm.ReadSkill("skill:agentshell-v2.0.0", "")
	if err != nil {
		t.Fatalf("ReadSkill: %v", err)
	}
	if skill.ContentHash != wantHash {
		t.Errorf("ContentHash = %q, want %q", skill.ContentHash, wantHash)
	}
}

func TestReadSkill_ByNameLatestVersion(t *testing.T) {
	dm := NewTestDM(t)

	insertRawSkill(t, dm, "skill:agentshell-v1.0.0", "agentshell", "1.0.0",
		"---\nname: agentshell\nversion: 1.0.0\n---\nbody v1")
	insertRawSkill(t, dm, "skill:agentshell-v2.0.0", "agentshell", "2.0.0",
		"---\nname: agentshell\nversion: 2.0.0\n---\nbody v2")

	skill, err := dm.ReadSkill("agentshell", "")
	if err != nil {
		t.Fatalf("ReadSkill: %v", err)
	}
	if skill.Version != "2.0.0" {
		t.Errorf("version = %q, want 2.0.0 (latest)", skill.Version)
	}
}

func TestReadSkill_NotFound(t *testing.T) {
	dm := NewTestDM(t)

	_, err := dm.ReadSkill("nonexistent", "")
	if err == nil {
		t.Fatal("expected error for missing skill")
	}
}

func TestListSkills_LatestOnly(t *testing.T) {
	dm := NewTestDM(t)

	insertRawSkill(t, dm, "skill:agentshell-v1.0.0", "agentshell", "1.0.0",
		"---\nname: agentshell\nversion: 1.0.0\n---\nbody")
	insertRawSkill(t, dm, "skill:agentshell-v2.0.0", "agentshell", "2.0.0",
		"---\nname: agentshell\nversion: 2.0.0\n---\nbody")
	insertRawSkill(t, dm, "skill:wp-deploy-v1.0.0", "wp-deploy", "1.0.0",
		"---\nname: wp-deploy\nversion: 1.0.0\n---\nbody")

	skills, err := dm.ListSkills("all")
	if err != nil {
		t.Fatalf("ListSkills: %v", err)
	}
	if len(skills) != 2 {
		t.Errorf("got %d skills, want 2 (latest only)", len(skills))
	}
}

func TestListSkills_EmptyDB(t *testing.T) {
	dm := NewTestDM(t)

	skills, err := dm.ListSkills("all")
	if err != nil {
		t.Fatalf("ListSkills: %v", err)
	}
	if len(skills) != 0 {
		t.Errorf("got %d, want 0", len(skills))
	}
}

func TestListSkills_LocalScopeExcludesGlobal(t *testing.T) {
	dm := NewTestDM(t)

	insertRawSkill(t, dm, "skill:local-v1.0.0", "local", "1.0.0",
		"---\nname: local\nversion: 1.0.0\n---\nbody")
	_, err := dm.db.Exec(`UPDATE memories SET is_global = 1 WHERE id = 'skill:local-v1.0.0'`)
	if err != nil {
		t.Fatalf("mark global: %v", err)
	}

	local, err := dm.ListSkills("local")
	if err != nil {
		t.Fatalf("ListSkills local: %v", err)
	}
	if len(local) != 0 {
		t.Errorf("local scope should exclude global, got %d", len(local))
	}
}

func TestListSkills_SharedScopeIncludesGlobal(t *testing.T) {
	dm := NewTestDM(t)

	insertRawSkill(t, dm, "skill:local-v1.0.0", "local", "1.0.0",
		"---\nname: local\nversion: 1.0.0\n---\nbody")
	insertRawSkill(t, dm, "skill:global-v1.0.0", "global", "1.0.0",
		"---\nname: global\nversion: 1.0.0\n---\nbody")
	_, err := dm.db.Exec(`UPDATE memories SET is_global = 1 WHERE id = 'skill:global-v1.0.0'`)
	if err != nil {
		t.Fatalf("mark global: %v", err)
	}

	shared, err := dm.ListSkills("shared")
	if err != nil {
		t.Fatalf("ListSkills shared: %v", err)
	}
	if len(shared) != 1 {
		t.Fatalf("shared scope got %d skills, want 1", len(shared))
	}
	if shared[0].ID != "skill:global-v1.0.0" {
		t.Errorf("shared skill = %q, want global skill", shared[0].ID)
	}
}

func TestListSkills_EmptyScopeDefaultsToAll(t *testing.T) {
	dm := NewTestDM(t)

	insertRawSkill(t, dm, "skill:local-v1.0.0", "local", "1.0.0",
		"---\nname: local\nversion: 1.0.0\n---\nbody")
	insertRawSkill(t, dm, "skill:global-v1.0.0", "global", "1.0.0",
		"---\nname: global\nversion: 1.0.0\n---\nbody")
	_, err := dm.db.Exec(`UPDATE memories SET is_global = 1 WHERE id = 'skill:global-v1.0.0'`)
	if err != nil {
		t.Fatalf("mark global: %v", err)
	}

	all, err := dm.ListSkills("all")
	if err != nil {
		t.Fatalf("ListSkills all: %v", err)
	}
	defaulted, err := dm.ListSkills("")
	if err != nil {
		t.Fatalf("ListSkills empty scope: %v", err)
	}
	if len(defaulted) != len(all) {
		t.Fatalf("empty scope got %d skills, all got %d", len(defaulted), len(all))
	}
	for i := range all {
		if defaulted[i] != all[i] {
			t.Errorf("skill %d differs: empty scope = %+v, all = %+v", i, defaulted[i], all[i])
		}
	}
}
