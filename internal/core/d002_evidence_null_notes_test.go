// d002_evidence_null_notes_test.go — regressions for audit finding D-002.
//
// D-002: `mpm evidence list --artifact <id>` crashed with
// `converting NULL to string is unsupported` whenever any evidence row had
// NULL notes. The auto_capture pipeline writes NULL notes for every
// auto-generated evidence row, so this crashed on every fresh workspace
// after the first memory save.
//
// Root cause: ListEvidenceForArtifact scanned the `notes` TEXT NULL column
// directly into a Go `string` field. Per the substrate defense triad
// (rule #2), NULL scalar columns must bind to sql.NullString at scan time
// and be unwrapped at the Go boundary.
package internal

import (
	"encoding/json"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestD002_EvidenceListHandlesNullNotes pins the headline regression: a row
// with NULL notes must not crash and must return notes as the empty string.
func TestD002_EvidenceListHandlesNullNotes(t *testing.T) {
	dm := newTestDM(t)

	// Insert a memory and let the auto_capture path add a NULL-notes row.
	memID := "d002-mem-null-notes"
	_, err := dm.ExecTracked(
		`INSERT INTO memories (id, collection, content) VALUES (?, 'memories', 'D-002 seed')`,
		0, memID)
	require.NoError(t, err)

	// Insert a synthetic NULL-notes row directly to exercise the scan path
	// without depending on auto_capture side effects.
	_, err = dm.ExecTracked(`
		INSERT INTO evidence
			(id, artifact_id, artifact_type, type, source_group,
			 strength, independence_factor, created_by, created_at, expires_at, notes)
		VALUES
			('d002-ev-1', ?, 'memory', 'observation', 'auto_capture',
			 0.5, 1.0, 'direct', 1700000000, NULL, NULL)
	`, 0, memID)
	require.NoError(t, err)

	// The fix: this call used to panic with "converting NULL to string is
	// unsupported". It now returns a row with Notes: "".
	rows, err := ListEvidenceForArtifact(dm, memID, "memory")
	require.NoError(t, err)
	require.NotEmpty(t, rows, "expected at least one evidence row")

	// Find our synthetic row and assert Notes was unwrapped to empty string.
	var found bool
	for _, ev := range rows {
		if ev.ID == "d002-ev-1" {
			found = true
			assert.Equal(t, "", ev.Notes, "NULL notes must surface as empty string, not panic")
			assert.Equal(t, "auto_capture", ev.SourceGroup)
		}
	}
	require.True(t, found, "synthetic NULL-notes row not returned")
}

// TestD002_EvidenceListHandlesPopulatedNotes pins the positive contract:
// non-NULL notes still surface verbatim after the sql.NullString unwrap.
func TestD002_EvidenceListHandlesPopulatedNotes(t *testing.T) {
	dm := newTestDM(t)

	memID := "d002-mem-with-notes"
	_, err := dm.ExecTracked(
		`INSERT INTO memories (id, collection, content) VALUES (?, 'memories', 'D-002 positive seed')`,
		0, memID)
	require.NoError(t, err)

	const wantNotes = "Reproduced locally on 3 machines; see issue tracker #1234"
	_, err = dm.ExecTracked(`
		INSERT INTO evidence
			(id, artifact_id, artifact_type, type, source_group,
			 strength, independence_factor, created_by, created_at, expires_at, notes)
		VALUES
			('d002-ev-2', ?, 'memory', 'reproduction', 'manual_review',
			 0.9, 1.0, 'analyst-a', 1700000000, NULL, ?)
	`, 0, memID, wantNotes)
	require.NoError(t, err)

	rows, err := ListEvidenceForArtifact(dm, memID, "memory")
	require.NoError(t, err)
	require.NotEmpty(t, rows)

	var found bool
	for _, ev := range rows {
		if ev.ID == "d002-ev-2" {
			found = true
			assert.Equal(t, wantNotes, ev.Notes, "populated notes must round-trip verbatim")
		}
	}
	require.True(t, found, "evidence row with populated notes not returned")
}

// TestD002_EvidenceListMixedNullAndPopulated pins the mixed-row contract:
// a single artifact can carry some NULL-notes rows and some with notes.
// The list call must return both, with the NULL ones empty and the others
// verbatim. This is the realistic auto_capture + manual_review pattern.
func TestD002_EvidenceListMixedNullAndPopulated(t *testing.T) {
	dm := newTestDM(t)

	memID := "d002-mem-mixed"
	_, err := dm.ExecTracked(
		`INSERT INTO memories (id, collection, content) VALUES (?, 'memories', 'D-002 mixed seed')`,
		0, memID)
	require.NoError(t, err)

	// Three rows: NULL, populated, NULL.
	rows := []struct {
		id    string
		notes interface{}
	}{
		{"d002-mix-null-1", nil},
		{"d002-mix-pop-1", "investigator followup: see thread"},
		{"d002-mix-null-2", nil},
	}
	for _, r := range rows {
		_, err := dm.ExecTracked(`
			INSERT INTO evidence
				(id, artifact_id, artifact_type, type, source_group,
				 strength, independence_factor, created_by, created_at, expires_at, notes)
			VALUES
				(?, ?, 'memory', 'observation', 'auto_capture',
				 0.5, 1.0, 'direct', 1700000000, NULL, ?)
		`, 0, r.id, memID, r.notes)
		require.NoError(t, err)
	}

	evs, err := ListEvidenceForArtifact(dm, memID, "memory")
	require.NoError(t, err)
	assert.Len(t, evs, 3)

	notesByID := map[string]string{}
	for _, e := range evs {
		notesByID[e.ID] = e.Notes
	}
	assert.Equal(t, "", notesByID["d002-mix-null-1"])
	assert.Equal(t, "investigator followup: see thread", notesByID["d002-mix-pop-1"])
	assert.Equal(t, "", notesByID["d002-mix-null-2"])
}

// TestD002_EvidenceListJSONMarshalable pins the public-boundary contract:
// after the unwrap, Evidence rows must still JSON-marshal cleanly with
// Notes as a regular string field (not as null). This is the contract the
// `mpm call mpm_evidence --payload '{"action":"list"}'` consumers depend on.
func TestD002_EvidenceListJSONMarshalable(t *testing.T) {
	dm := newTestDM(t)

	memID := "d002-mem-json"
	_, err := dm.ExecTracked(
		`INSERT INTO memories (id, collection, content) VALUES (?, 'memories', 'D-002 json seed')`,
		0, memID)
	require.NoError(t, err)

	_, err = dm.ExecTracked(`
		INSERT INTO evidence
			(id, artifact_id, artifact_type, type, source_group,
			 strength, independence_factor, created_by, created_at, expires_at, notes)
		VALUES
			('d002-ev-json', ?, 'memory', 'observation', 'auto_capture',
			 0.5, 1.0, 'direct', 1700000000, NULL, NULL)
	`, 0, memID)
	require.NoError(t, err)

	evs, err := ListEvidenceForArtifact(dm, memID, "memory")
	require.NoError(t, err)
	require.NotEmpty(t, evs)

	// Marshal the first row. JSON should contain `"Notes":""` (empty string),
	// NOT omit the field entirely or panic.
	body, err := json.Marshal(evs[0])
	require.NoError(t, err)
	assert.Contains(t, string(body), `"Notes":""`,
		"evidence row must marshal Notes as empty string, not panic or null")
}
