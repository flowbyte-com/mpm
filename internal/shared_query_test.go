package internal

import (
	"path/filepath"
	"testing"
)

func TestQueryGlobalRules_LocalOnlyReturnsEmpty(t *testing.T) {
	t.Setenv("MPM_SHARED_DB", "")
	dm, err := NewDatabaseManager("")
	if err != nil {
		t.Fatalf("NewDatabaseManager: %v", err)
	}
	defer dm.Close()

	got, err := dm.QueryGlobalRules("", 10)
	if err != nil {
		t.Fatalf("expected nil err in local-only mode, got: %v", err)
	}
	if got != nil {
		t.Errorf("expected nil results in local-only mode, got %v", got)
	}
}

func TestQueryGlobalRules_AttachesAndReadsRules(t *testing.T) {
	tmp := t.TempDir()
	sharedPath := filepath.Join(tmp, "shared.db")
	t.Setenv("MPM_SHARED_DB", sharedPath)
	t.Setenv("MPM_SHARED_READONLY", "")

	dm, err := NewDatabaseManager("")
	if err != nil {
		t.Fatalf("NewDatabaseManager: %v", err)
	}
	defer dm.Close()

	if dm.SharedAttached() == "" {
		t.Fatal("shared DB did not attach")
	}

	// Insert a rule. We include every column the QueryGlobalRules
	// SELECT references so NULL coercion can't trip the scan.
	_, err = dm.SQLDB().Exec(`
		INSERT INTO shared.memories
		    (id, collection, content, weight, reinforcement_count,
		     created_at, updated_at, deleted_at, is_global)
		VALUES
		    ('rule-001', 'rules', 'Never expose API keys to the LLM context',
		     10, 0, '2026-06-26', '2026-06-26', NULL, 1)
	`)
	if err != nil {
		t.Fatalf("insert shared rule: %v", err)
	}

	got, err := dm.QueryGlobalRules("", 10)
	if err != nil {
		t.Fatalf("QueryGlobalRules: %v", err)
	}
	if len(got) != 1 {
		t.Fatalf("expected 1 rule, got %d: %+v", len(got), got)
	}
	rule := got[0]
	if rule["id"] != "rule-001" {
		t.Errorf("unexpected rule id: %v", rule["id"])
	}
	if rule["is_global"] != 1 {
		t.Errorf("expected is_global=1, got %v", rule["is_global"])
	}
	if rule["source"] != "shared" {
		t.Errorf("expected source=shared, got %v", rule["source"])
	}
}

func TestQueryGlobalRules_FiltersByIsGlobal(t *testing.T) {
	tmp := t.TempDir()
	sharedPath := filepath.Join(tmp, "shared.db")
	t.Setenv("MPM_SHARED_DB", sharedPath)

	dm, err := NewDatabaseManager("")
	if err != nil {
		t.Fatalf("NewDatabaseManager: %v", err)
	}
	defer dm.Close()

	_, err = dm.SQLDB().Exec(`
		INSERT INTO shared.memories
		    (id, collection, content, weight, reinforcement_count,
		     created_at, updated_at, deleted_at, is_global)
		VALUES
		    ('local-rule', 'rules', 'project-specific note',
		     5, 0, '2026-06-26', '2026-06-26', NULL, 0),
		    ('shared-rule', 'rules', 'house rule applies to all agents',
		     10, 0, '2026-06-26', '2026-06-26', NULL, 1)
	`)
	if err != nil {
		t.Fatalf("insert: %v", err)
	}

	got, err := dm.QueryGlobalRules("", 10)
	if err != nil {
		t.Fatalf("QueryGlobalRules: %v", err)
	}
	if len(got) != 1 {
		t.Fatalf("expected 1 (only is_global=1), got %d: %+v", len(got), got)
	}
	if got[0]["id"] != "shared-rule" {
		t.Errorf("expected shared-rule, got %v", got[0]["id"])
	}
}

func TestQueryGlobalRules_LikeSearch(t *testing.T) {
	tmp := t.TempDir()
	sharedPath := filepath.Join(tmp, "shared.db")
	t.Setenv("MPM_SHARED_DB", sharedPath)

	dm, err := NewDatabaseManager("")
	if err != nil {
		t.Fatalf("NewDatabaseManager: %v", err)
	}
	defer dm.Close()

	_, err = dm.SQLDB().Exec(`
		INSERT INTO shared.memories
		    (id, collection, content, weight, reinforcement_count,
		     created_at, updated_at, deleted_at, is_global)
		VALUES
		    ('rule-a', 'rules', 'api keys must never reach the LLM',
		     10, 0, '2026-06-26', '2026-06-26', NULL, 1),
		    ('rule-b', 'rules', 'always quote shell variables in scripts',
		     5, 0, '2026-06-26', '2026-06-26', NULL, 1)
	`)
	if err != nil {
		t.Fatalf("insert: %v", err)
	}

	got, err := dm.QueryGlobalRules("api", 10)
	if err != nil {
		t.Fatalf("QueryGlobalRules: %v", err)
	}
	if len(got) != 1 {
		t.Fatalf("expected 1 LIKE hit for 'api', got %d: %+v", len(got), got)
	}
	if got[0]["id"] != "rule-a" {
		t.Errorf("expected rule-a, got %v", got[0]["id"])
	}
}

func TestQueryGlobalRules_LimitCap(t *testing.T) {
	tmp := t.TempDir()
	sharedPath := filepath.Join(tmp, "shared.db")
	t.Setenv("MPM_SHARED_DB", sharedPath)

	dm, err := NewDatabaseManager("")
	if err != nil {
		t.Fatalf("NewDatabaseManager: %v", err)
	}
	defer dm.Close()

	_, err = dm.SQLDB().Exec(`
		INSERT INTO shared.memories
		    (id, collection, content, weight, reinforcement_count,
		     created_at, updated_at, deleted_at, is_global)
		VALUES ('rule-x', 'rules', 'one rule', 5, 0, '2026-06-26', '2026-06-26', NULL, 1)
	`)
	if err != nil {
		t.Fatalf("insert: %v", err)
	}

	got, err := dm.QueryGlobalRules("", 10000)
	if err != nil {
		t.Fatalf("QueryGlobalRules: %v", err)
	}
	if len(got) != 1 {
		t.Errorf("expected 1 row (we only inserted 1), got %d", len(got))
	}
}
