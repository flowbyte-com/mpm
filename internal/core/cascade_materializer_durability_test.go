// cascade_materializer_durability_test.go — read-back assertion coverage
// for H-2 (post-M3 audit, 2026-08-31).
//
// The fix: markMaterialized now does a post-UPDATE SELECT to verify the
// row actually flipped to status='materialized'. This test exercises the
// happy path and the silent-failure scenarios that motivated the read-back.
//
// Tests:
//   - TestMarkMaterialized_HappyPath: status flips, theory_id matches.
//   - TestMarkMaterialized_UnknownIntent: row missing, error surfaces.
//   - TestMarkMaterialized_DBClosedMidCall: connection error surfaces.
package internal

import (
	"testing"

	"github.com/stretchr/testify/require"
)

// TestMarkMaterialized_HappyPath pins the new read-back behavior: after
// UPDATE, the row's status and materialized_theory_id must both match
// the input. This is the load-bearing assertion the audit H-2 lacked.
func TestMarkMaterialized_HappyPath(t *testing.T) {
	dm := hermeticDatabaseManager(t)
	defer dm.Close()

	intentID := insertCascadeIntent(t, dm, "mem-pending-h2")

	mat := NewCascadeMaterializer(dm, DefaultCascadeMaterializerOptions())
	err := mat.markMaterialized(intentID, "theory-xyz")
	require.NoError(t, err, "markMaterialized should succeed on a valid intent")

	var status, theoryID string
	scanErr := dm.db.QueryRow(
		`SELECT status, materialized_theory_id FROM epistemic_cascade_outbox WHERE id = ?`,
		intentID,
	).Scan(&status, &theoryID)
	require.NoError(t, scanErr)
	require.Equal(t, "materialized", status, "status should be 'materialized' post read-back")
	require.Equal(t, "theory-xyz", theoryID, "materialized_theory_id should match the input")
}

// TestMarkMaterialized_UnknownIntent surfaces a structured error when
// the row doesn't exist. Without the read-back, a silent UPDATE that
// affected zero rows would have returned nil and the caller would
// proceed to schedule a wake for a theory with no outbox row to back
// it up.
func TestMarkMaterialized_UnknownIntent(t *testing.T) {
	dm := hermeticDatabaseManager(t)
	defer dm.Close()

	mat := NewCascadeMaterializer(dm, DefaultCascadeMaterializerOptions())
	err := mat.markMaterialized("intent-that-does-not-exist", "theory-orphan")
	require.Error(t, err, "markMaterialized on a missing intent must surface error")
	require.Contains(t, err.Error(), "read-back",
		"error should indicate the read-back assertion failed")
}

// TestMarkMaterialized_DBClosedMidCall covers the connection-loss
// scenario: the UPDATE fails (driver returns connection error), and
// markMaterialized must propagate that error rather than swallow it.
func TestMarkMaterialized_DBClosedMidCall(t *testing.T) {
	dm := hermeticDatabaseManager(t)
	intentID := insertCascadeIntent(t, dm, "mem-pending-h2-closed")

	mat := NewCascadeMaterializer(dm, DefaultCascadeMaterializerOptions())
	require.NoError(t, dm.db.Close(), "test setup: closing the underlying db")

	err := mat.markMaterialized(intentID, "theory-abc")
	require.Error(t, err, "markMaterialized after db close must propagate the driver error")
}

// insertCascadeIntent inserts a minimal valid outbox row with the
// given memory id and returns the generated outbox id. Used by the
// durability tests; isolated here so the test file is self-contained.
func insertCascadeIntent(t *testing.T, dm *DatabaseManager, deadArtifactID string) string {
	t.Helper()
	id := "out-" + GenerateID()
	now := int64(1700000000)
	_, err := dm.db.Exec(`
		INSERT INTO epistemic_cascade_outbox (
			id, invalidation_event_id, dead_artifact_id, dead_artifact_type,
			downstream_artifact_id, downstream_artifact_type,
			cascade_depth, status, attempt_count, created_at, updated_at
		) VALUES (?, ?, ?, ?, ?, ?, ?, 'pending', 0, ?, ?)
	`, id, "inv-"+deadArtifactID, deadArtifactID, "memory",
		"ds-"+deadArtifactID, "memory", 1, now, now)
	require.NoError(t, err)
	return id
}

