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
