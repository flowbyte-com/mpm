// embedding_schema_test.go — regression test for alpha-3.5 embedding
// provider separation schema additions: memories.embedding_source,
// memories.embedding_dimension, theories.synthetic, embedding_migration_log
// table, and three partial indexes.

package internal

import (
	"testing"
)

func TestEmbeddingSchemaExists(t *testing.T) {
	dm := NewTestDM(t)
	defer dm.Close()

	// Verify the three columns exist on the right tables.
	for _, c := range []struct{ table, column string }{
		{"memories", "embedding_source"},
		{"memories", "embedding_dimension"},
		{"memories", "synthetic"},
	} {
		var n int
		err := dm.SQLDB().QueryRow(
			`SELECT COUNT(*) FROM pragma_table_info(?) WHERE name = ?`,
			c.table, c.column,
		).Scan(&n)
		if err != nil {
			t.Fatalf("pragma_table_info %s.%s: %v", c.table, c.column, err)
		}
		if n != 1 {
			t.Errorf("missing column %s.%s", c.table, c.column)
		}
	}

	// Verify the audit log table exists.
	var name string
	err := dm.SQLDB().QueryRow(
		`SELECT name FROM sqlite_master WHERE type='table' AND name='embedding_migration_log'`,
	).Scan(&name)
	if err != nil {
		t.Fatalf("query sqlite_master: %v", err)
	}
	if name != "embedding_migration_log" {
		t.Errorf("missing embedding_migration_log table, got %q", name)
	}

	// Verify the three indexes exist.
	for _, idx := range []string{
		"idx_memories_embedding_source",
		"idx_memories_embedding_dimension",
		"idx_embedding_migration_log_memory_id",
	} {
		var n int
		err := dm.SQLDB().QueryRow(
			`SELECT COUNT(*) FROM sqlite_master WHERE type='index' AND name=?`,
			idx,
		).Scan(&n)
		if err != nil {
			t.Fatalf("query sqlite_master for index %s: %v", idx, err)
		}
		if n != 1 {
			t.Errorf("missing index %s", idx)
		}
	}
}
