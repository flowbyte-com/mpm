package internal

import (
	"database/sql"
	"fmt"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestSchema_NewColumnsOnMemories(t *testing.T) {
	store := newTestStore(t)
	defer store.DB.Close()

	cols := getTableColumns(t, store.DB.DB, "memories")
	assert.Contains(t, cols, "retrieval_priority")
	assert.Contains(t, cols, "importance")
	assert.Contains(t, cols, "confidence")
}

func TestSchema_NewColumnsOnLessons(t *testing.T) {
	store := newTestStore(t)
	defer store.DB.Close()

	cols := getTableColumns(t, store.DB.DB, "lessons")
	assert.Contains(t, cols, "retrieval_priority")
	assert.Contains(t, cols, "importance")
	assert.Contains(t, cols, "confidence")
}

func TestSchema_EvidenceTable(t *testing.T) {
	store := newTestStore(t)
	defer store.DB.Close()

	cols := getTableColumns(t, store.DB.DB, "evidence")
	for _, want := range []string{"id", "artifact_id", "artifact_type", "type", "source_group", "strength", "independence_factor", "created_by", "created_at", "expires_at", "notes"} {
		assert.Contains(t, cols, want, "evidence table missing column %q", want)
	}
}

func TestSchema_ConfidenceHistoryTable(t *testing.T) {
	store := newTestStore(t)
	defer store.DB.Close()

	cols := getTableColumns(t, store.DB.DB, "confidence_history")
	for _, want := range []string{"id", "artifact_id", "artifact_type", "confidence", "computed_at", "evidence_count", "trigger"} {
		assert.Contains(t, cols, want, "confidence_history table missing column %q", want)
	}
}

func TestSchema_ArtifactsView(t *testing.T) {
	store := newTestStore(t)
	defer store.DB.Close()

	// View exists and is queryable. Insert one memory to make the query non-empty.
	_, err := store.DB.Exec(`INSERT INTO memories (id, collection, content) VALUES ('m1', 'memories', 'test')`)
	require.NoError(t, err)

	row := store.DB.QueryRow(`SELECT type, id FROM artifacts WHERE id = 'm1'`)
	var typ, id string
	require.NoError(t, row.Scan(&typ, &id))
	assert.Equal(t, "memory", typ)
	assert.Equal(t, "m1", id)
}

func TestSchema_LegacyWeightView(t *testing.T) {
	store := newTestStore(t)
	defer store.DB.Close()

	// legacy_weight = max(0.01, (retrieval_priority + importance) / 2)
	_, err := store.DB.Exec(`INSERT INTO memories (id, collection, content, retrieval_priority, importance) VALUES ('m1', 'memories', 'test', 0.6, 0.8)`)
	require.NoError(t, err)

	var w float64
	require.NoError(t, store.DB.QueryRow(`SELECT legacy_weight FROM legacy_weight WHERE id = 'm1'`).Scan(&w))
	assert.InDelta(t, 0.7, w, 1e-9) // (0.6 + 0.8) / 2

	// Test floor: very low priorities still produce a value >= 0.01.
	_, err = store.DB.Exec(`INSERT INTO memories (id, collection, content, retrieval_priority, importance) VALUES ('m2', 'memories', 'test', 0.0, 0.0)`)
	require.NoError(t, err)
	require.NoError(t, store.DB.QueryRow(`SELECT legacy_weight FROM legacy_weight WHERE id = 'm2'`).Scan(&w))
	assert.GreaterOrEqual(t, w, 0.01)
}

// newTestStore creates a fresh store on a temp DB. Centralized so schema
// tests don't all reinvent the same boilerplate.
func newTestStore(t *testing.T) *MemoryStore {
	t.Helper()
	tmpDir := t.TempDir()
	store := NewMemoryStore("")
	store.SQLiteDBPath = filepath.Join(tmpDir, "test.db")
	require.NoError(t, store.InitSQLite())
	return store
}

// getTableColumns returns the column names of a table.
func getTableColumns(t *testing.T, db *sql.DB, name string) []string {
	t.Helper()
	rows, err := db.Query(fmt.Sprintf("PRAGMA table_info(%s)", name))
	require.NoError(t, err)
	defer rows.Close()
	var out []string
	for rows.Next() {
		var cid, cname, ctype string
		var notnull, pk int
		// dflt_value is NULL for non-defaulted columns; using sql.NullString
		// keeps the scan from tripping on those rows (the previous
		// interface{} target hits a Go 1.26 driver-internal
		// NULL-to-int conversion failure on tables that have any
		// TEXT-default column like epistemic_cascade_outbox.status='pending').
		var dflt sql.NullString
		require.NoError(t, rows.Scan(&cid, &cname, &ctype, &notnull, &dflt, &pk))
		out = append(out, cname)
	}
	return out
}
