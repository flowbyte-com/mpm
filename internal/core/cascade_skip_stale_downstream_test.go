// cascade_skip_stale_downstream_test.go — coverage for the
// downstream-liveness guard the cascade materializer applies before
// materializing a theory.
//
// The guard closes lesson 06f57b1d (cascade theories firing on
// already-superseded targets): before persisting a theory and its
// cascade wake, the materializer must verify the downstream artifact
// is still alive. A superseded or hard-deleted downstream means the
// review the cascade theory would solicit is moot — the operator
// who performed the supersede already owns the new artifact.
//
// Defence-in-depth contract:
//
//   - Downstream present + not superseded         → materialize
//   - Downstream superseded                       → skip (mark failed)
//   - Downstream hard-deleted (not in memories)   → skip (mark failed)
//   - Downstream metadata malformed               → materialize
//     (defensive — never block on a parsing edge case)
//
// The skip path uses the existing 'failed' terminal state with a
// terminal_error that names the specific reason, so the audit row
// (and PruneCascadeOutbox after retention) carry the diagnostic.
package internal

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// markDownstreamSupersede flips an existing memories row into the
// superseded terminal state — metadata.superseded_by, the
// "superseded" tag, and the dropped confidence. Mirrors the
// production SupersedeDecision contract so the liveness guard
// sees the same shape it sees in production.
func markDownstreamSupersede(t *testing.T, dm *DatabaseManager, id, supersededBy string) {
	t.Helper()
	patchJSON, _ := json.Marshal(map[string]interface{}{
		"superseded":    true,
		"superseded_by": supersededBy,
	})
	_, err := dm.db.Exec(`
		UPDATE memories
		SET metadata = json_patch(COALESCE(metadata,'{}'), ?),
		    tags = CASE
		        WHEN tags IS NULL OR tags = '' OR tags = '[]' OR tags = 'null' OR NOT json_valid(tags)
		            THEN json_array(?, ?)
		        ELSE json_insert(
		            json_insert(tags, '$[' || json_array_length(tags) || ']', ?),
		            '$[' || (json_array_length(tags) + 1) || ']', ?
		        )
		    END,
		    deleted_at = NULL
		WHERE id = ?
	`, string(patchJSON),
		"superseded", "superseded-by:"+supersededBy,
		"superseded", "superseded-by:"+supersededBy,
		id)
	require.NoError(t, err)
}

// TestMaterializer_SkipsWhenDownstreamSuperseded pins the headline
// fix: a cascade intent whose downstream was already superseded
// before the materializer runs MUST NOT produce a theory or a
// wake. The outbox row is moved to terminal 'failed' state with a
// terminal_error that names the reason.
func TestMaterializer_SkipsWhenDownstreamSuperseded(t *testing.T) {
	fx := newCascadeMaterializerFixture(t)
	// Mark the decision downstream as superseded BEFORE the cascade
	// materializer picks up the intent. The cascade intent still
	// points at fx.decID; the guard must reject it.
	markDownstreamSupersede(t, fx.dm, fx.decID, "dec-succ-"+GenerateID())

	intentID := fx.enqueueIntent(t, fx.memID, "memory", fx.decID, "decision", 0, "foundation invalidated")

	report, err := fx.mat.MaterializeBatch(context.Background(), 10)
	require.NoError(t, err)
	require.Equal(t, 1, report.Claimed)
	require.Equal(t, 1, report.Processed, "intent must be processed (not unclaimed)")
	// 0 materialized, 0 failed (in the canonical counters) — the
	// skip path returns resultSuppressed which feeds the
	// Suppressed counter, NOT Failed.
	assert.Equal(t, 0, report.Materialized,
		"no theory may be created for a superseded downstream")
	assert.Equal(t, 0, report.Failed,
		"downstream-not-live is a suppression, not a dead-letter failure")
	assert.Equal(t, 1, report.Suppressed,
		"downstream-not-live is reported as suppressed")

	// Outbox row must be in terminal 'failed' state with the
	// canonical terminal_error message naming the supersede reason.
	var status, termErr string
	require.NoError(t, fx.dm.db.QueryRow(
		`SELECT status, COALESCE(terminal_error,'') FROM epistemic_cascade_outbox WHERE id = ?`,
		intentID,
	).Scan(&status, &termErr))
	assert.Equal(t, "failed", status, "superseded-downstream intent must be marked failed")
	assert.True(t, strings.Contains(termErr, "downstream superseded"),
		"terminal_error must name the reason (got %q)", termErr)

	// No theory row may exist for this intent. The fixture
	// already created one theory (the cascade test fixture's
	// own theory); the count must remain 1, not 2.
	var theoryCount int
	require.NoError(t, fx.dm.db.QueryRow(
		`SELECT COUNT(*) FROM memories WHERE collection = 'theories' AND json_extract(metadata,'$.cascade') = 1 AND json_extract(metadata,'$.downstream_artifact_id') = ?`,
		fx.decID,
	).Scan(&theoryCount))
	assert.Equal(t, 0, theoryCount,
		"no cascade theory may be created for a superseded downstream")

	// No wake may have been scheduled for this intent.
	var wakeCount int
	require.NoError(t, fx.dm.db.QueryRow(
		`SELECT COUNT(*) FROM scheduled_wakes WHERE theory_id IS NULL OR json_extract(metadata,'$.invalidation_event_id') IN (SELECT invalidation_event_id FROM epistemic_cascade_outbox WHERE id = ?)`,
		intentID,
	).Scan(&wakeCount))
	// Note: the intent-level event_id is unique; the cascade
	// materializer never wrote a wake so no row carries that
	// event id. We assert this below by direct event lookup.
	var eventID string
	require.NoError(t, fx.dm.db.QueryRow(
		`SELECT invalidation_event_id FROM epistemic_cascade_outbox WHERE id = ?`,
		intentID,
	).Scan(&eventID))
	require.NoError(t, fx.dm.db.QueryRow(
		`SELECT COUNT(*) FROM scheduled_wakes WHERE json_extract(metadata,'$.invalidation_event_id') = ?`,
		eventID,
	).Scan(&wakeCount))
	assert.Equal(t, 0, wakeCount,
		"no wake may be scheduled for a superseded-downstream intent")
}

// TestMaterializer_SkipsWhenDownstreamHardDeleted pins the
// downstream-not-found branch. If the memories row is gone (e.g.
// explicit shred), the cascade materializer must skip
// materialization. Same suppression semantics as the superseded
// branch.
func TestMaterializer_SkipsWhenDownstreamHardDeleted(t *testing.T) {
	fx := newCascadeMaterializerFixture(t)

	// Hard-delete the downstream decision BEFORE the cascade
	// materializer runs. deleted_at IS NOT NULL is the
	// canonical soft-delete; the production shred path also
	// removes the row entirely via DELETE. We use deleted_at
	// here so the test surface matches the existing fixture
	// (and matches what a future-shred leaves behind in the
	// short window before PruneCascadeOutbox fires).
	_, err := fx.dm.db.Exec(
		`UPDATE memories SET deleted_at = CAST(strftime('%s','now') AS INTEGER) WHERE id = ?`,
		fx.decID,
	)
	require.NoError(t, err)

	intentID := fx.enqueueIntent(t, fx.memID, "memory", fx.decID, "decision", 0, "foundation invalidated")

	report, err := fx.mat.MaterializeBatch(context.Background(), 10)
	require.NoError(t, err)
	require.Equal(t, 1, report.Claimed)
	assert.Equal(t, 0, report.Materialized)
	assert.Equal(t, 1, report.Suppressed,
		"hard-deleted downstream must surface as suppressed (NOT failed)")

	// Outbox terminal state: failed with "downstream not found".
	var status, termErr string
	require.NoError(t, fx.dm.db.QueryRow(
		`SELECT status, COALESCE(terminal_error,'') FROM epistemic_cascade_outbox WHERE id = ?`,
		intentID,
	).Scan(&status, &termErr))
	assert.Equal(t, "failed", status)
	assert.True(t, strings.Contains(termErr, "downstream not found"),
		"terminal_error must name the reason (got %q)", termErr)
}

// TestMaterializer_MaterializesWhenDownstreamLive pins the
// positive case: a normal live downstream still produces a
// theory + wake. The fix MUST NOT regress the happy path.
func TestMaterializer_MaterializesWhenDownstreamLive(t *testing.T) {
	fx := newCascadeMaterializerFixture(t)

	intentID := fx.enqueueIntent(t, fx.memID, "memory", fx.decID, "decision", 0, "foundation invalidated")

	report, err := fx.mat.MaterializeBatch(context.Background(), 10)
	require.NoError(t, err)
	require.Equal(t, 1, report.Claimed)
	assert.Equal(t, 1, report.Materialized,
		"live downstream must continue to materialize theories")
	assert.Equal(t, 0, report.Suppressed)

	var status string
	require.NoError(t, fx.dm.db.QueryRow(
		`SELECT status FROM epistemic_cascade_outbox WHERE id = ?`, intentID,
	).Scan(&status))
	assert.Equal(t, "materialized", status)
}

// TestMaterializer_MaterializesWhenDownstreamTagsSuperseded pins
// the second live-ness detector (legacy tag-only marker). The
// metadata.superseded_by path and the tags "superseded" path both
// qualify; this test pins the tag-only branch separately.
func TestMaterializer_MaterializesWhenDownstreamTagsSuperseded(t *testing.T) {
	fx := newCascadeMaterializerFixture(t)
	// Set only the tags array — leave metadata.superseded_by
	// empty. The defensive union (metadata OR tags) must catch
	// the tag marker.
	_, err := fx.dm.db.Exec(`
		UPDATE memories
		SET tags = json_array('superseded', 'superseded-by:some-other-id')
		WHERE id = ?
	`, fx.decID)
	require.NoError(t, err)

	intentID := fx.enqueueIntent(t, fx.memID, "memory", fx.decID, "decision", 0, "foundation invalidated")
	report, err := fx.mat.MaterializeBatch(context.Background(), 10)
	require.NoError(t, err)
	assert.Equal(t, 0, report.Materialized, "tag-superseded downstream must be skipped")
	assert.Equal(t, 1, report.Suppressed)

	var status, termErr string
	require.NoError(t, fx.dm.db.QueryRow(
		`SELECT status, COALESCE(terminal_error,'') FROM epistemic_cascade_outbox WHERE id = ?`,
		intentID,
	).Scan(&status, &termErr))
	assert.Equal(t, "failed", status)
	assert.True(t, strings.Contains(termErr, "superseded"),
		"terminal_error must surface the superseded marker (got %q)", termErr)
}