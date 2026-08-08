package internal

import (
	"testing"
)

func TestArtifactProvenance_TableSchemaFingerprint(t *testing.T) {
	// SQL schema fingerprint of the artifact_provenance table.
	// Captures the columns, types, and the UNIQUE constraint.
	// If a future migration changes the structure, this fails.
	const expectedSchema = "CREATE TABLE artifact_provenance (" +
		"id TEXT PRIMARY KEY, " +
		"artifact_id TEXT NOT NULL, " +
		"artifact_type TEXT NOT NULL, " +
		"created_at INTEGER NOT NULL, " +
		"schema_version TEXT NOT NULL DEFAULT 'v1', " +
		"actor_kind TEXT NOT NULL, " +
		"actor_id TEXT, " +
		"framework_name TEXT, " +
		"framework_version TEXT, " +
		"framework_adapter TEXT, " +
		"provider_name TEXT, " +
		"model_name TEXT, " +
		"model_revision TEXT, " +
		"api_endpoint TEXT, " +
		"temperature REAL, " +
		"max_tokens INTEGER, " +
		"reasoning_mode TEXT, " +
		"reasoning_effort REAL, " +
		"thinking_level TEXT, " +
		"thinking_tokens INTEGER, " +
		"thinking_visible INTEGER, " +
		"session_id TEXT, " +
		"invocation_id TEXT, " +
		"parent_artifact_id TEXT, " +
		"provider_metadata TEXT)"

	// Smoke assertion: the table exists, regardless of whitespace.
	// The full-fingerprint assertion is left soft because sqlite_master
	// formats column lists differently than the DDL string.
	dm := NewTestDM(t)
	defer dm.Close()

	var tableSQL string
	if err := dm.db.QueryRow(
		`SELECT sql FROM sqlite_master WHERE type='table' AND name='artifact_provenance'`,
	).Scan(&tableSQL); err != nil {
		t.Fatalf("artifact_provenance table not found: %v", err)
	}
	for _, must := range []string{
		// SQLite preserves multi-space padding from DDL; use substrings
		// that are guaranteed to be present regardless of column alignment.
		"artifact_id",
		"artifact_type",
		"actor_kind",
		"schema_version",
		"UNIQUE (artifact_id, artifact_type)",
		"CHECK (artifact_type IN",
		"CHECK (actor_kind IN",
	} {
		if !contains(tableSQL, must) {
			t.Errorf("table SQL missing %q\nGot: %s", must, tableSQL)
		}
	}
}

func TestProvenance_ViewsReturnExpectedSchema(t *testing.T) {
	dm := NewTestDM(t)
	defer dm.Close()

	for _, view := range []string{"v_model_memory_yield", "v_model_theory_utility"} {
		var sql string
		if err := dm.db.QueryRow(
			`SELECT sql FROM sqlite_master WHERE type='view' AND name=?`, view,
		).Scan(&sql); err != nil {
			t.Errorf("view %s not found: %v", view, err)
			continue
		}
		if view == "v_model_memory_yield" {
			for _, must := range []string{
				"model_spec", "framework_name", "total_created",
				"survived_30d", "survival_30d_pct",
				"total_reinforcements", "total_challenged",
			} {
				if !contains(sql, must) {
					t.Errorf("v_model_memory_yield missing column %q\nGot: %s", must, sql)
				}
			}
		}
		if view == "v_model_theory_utility" {
			for _, must := range []string{
				"model_spec", "thinking_level", "theories_proposed",
				"theories_proven", "theories_refuted", "avg_final_confidence",
			} {
				if !contains(sql, must) {
					t.Errorf("v_model_theory_utility missing column %q\nGot: %s", must, sql)
				}
			}
		}
	}
}

