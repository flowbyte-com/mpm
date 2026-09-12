// migration_tags_json_normalize_test.go — TDD coverage for the legacy
// tags-column repair migration. Pre-fix Supersede/Invalidate wrote
// CSV-style suffixes (`["foo"],superseded`) into a JSON column, so the
// migration must detect every degenerate shape and emit canonical JSON.
package internal

import (
	"encoding/json"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestRepairLegacyTags_Cases(t *testing.T) {
	cases := []struct {
		name string
		in   string
		want string // JSON-encoded expected array
	}{
		{"empty", "", `[]`},
		{"null_literal", "null", `[]`},
		{"null_with_superseded", "null,superseded", `["superseded"]`},
		{"json_head_with_superseded", `["foo","bar"],superseded`, `["foo","bar","superseded"]`},
		{"json_head_with_supersede_by", `["a"],superseded,superseded-by:abc123`, `["a","superseded","superseded-by:abc123"]`},
		{"csv_only_superseded", "superseded", `["superseded"]`},
		{"csv_dropped_non_superseded", "a,b,c", `[]`}, // unknown tags dropped (supersede chain only)
		{"garbage", "this is not json", `[]`},
		{"array_only_unchanged", `["x","y"]`, `["x","y"]`},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := repairLegacyTags(tc.in)
			var gotSlice, wantSlice []string
			require.NoError(t, json.Unmarshal([]byte(got), &gotSlice))
			require.NoError(t, json.Unmarshal([]byte(tc.want), &wantSlice))
			assert.Equal(t, wantSlice, gotSlice, "in=%q got=%q want=%q", tc.in, got, tc.want)
		})
	}
}

func TestMigrateTagsJSONNormalize_Idempotent(t *testing.T) {
	dm := newTestDM(t)
	defer dm.Close()

	// Clear the sentinel so the migration actually runs on our hand-crafted
	// corrupted row. NewTestDM runs InitSchema which records the sentinel
	// even when there's nothing to repair.
	_, err := dm.SQLDB().Exec(`DELETE FROM schema_migrations WHERE id = ?`, tagsJSONNormalizeSentinel)
	require.NoError(t, err)

	// Insert a corrupted tags row directly.
	_, err = dm.SQLDB().Exec(`
		INSERT INTO memories (id, collection, content, tags)
		VALUES ('corrupt-mem-1', 'memories', 'test content', 'null,superseded,superseded-by:abc')
	`)
	require.NoError(t, err)

	// Run the migration twice — second run must be a no-op (sentinel gate).
	for i := 0; i < 2; i++ {
		tx, err := dm.SQLDB().Begin()
		require.NoError(t, err)
		require.NoError(t, MigrateTagsJSONNormalize(tx))
		require.NoError(t, tx.Commit())
	}

	var tags string
	err = dm.SQLDB().QueryRow(`SELECT tags FROM memories WHERE id = ?`, "corrupt-mem-1").Scan(&tags)
	require.NoError(t, err)
	var parsed []string
	require.NoError(t, json.Unmarshal([]byte(tags), &parsed))
	assert.Contains(t, parsed, "superseded")
	assert.Contains(t, parsed, "superseded-by:abc")
}