// cmd/mpm/d_provenance_parent_invocation_test.go
//
// Final-pass debt-closure test: prove that the parent_invocation_id
// linkage survives the canonical provenance path.
//
// Data model (S7 verification):
//
//   tool_invocations  (1 row per tool call)   carries invocation_id,
//                                               framework_name, session_id
//   artifact_provenance (1 row per artifact)  carries invocation_id AND
//                                               parent_invocation_id
//
// The two tables are linked by artifact_provenance.invocation_id ->
// tool_invocations.invocation_id. The parent linkage for invocation
// trees is persisted on artifact_provenance only — that is the
// authoritative location per the schema design
// (docs/archive/2026-08-08-artifact-provenance-design.md).
//
// S7.2 closed the in-memory propagation: invokeTool now reads
// MPM_PROVENANCE_INVOCATION_ID and MPM_PROVENANCE_PARENT_INVOCATION_ID
// and puts them in ActiveContext. The flow then continues:
//
//   invokeTool ActiveContext
//     -> tool handler (mpm_work create)
//     -> dm.CreateWorkWithContext
//     -> dm.provenanceFromContext(ac)        (sets ParentInvocationID)
//     -> dm.RecordArtifactProvenance(...)
//     -> artifact_provenance.parent_invocation_id
//
// Tests below exercise the in-process `invokeTool` boundary (the same
// one S5/S6/S7 used) so the env var lookup is actually triggered.
// mpm_work.create is the canonical tool that actually writes
// artifact_provenance (mpm_memory save uses the legacy MemoryStore
// path which does not write artifact_provenance today).

package main

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	internal "github.com/flowbyte-com/mpm-core"
)

func TestD_ProvenanceParentInvocation_PreservedOnArtifactRow(t *testing.T) {
	dm := internal.NewTestDM(t)
	overrideDM(t, dm)
	t.Setenv("MPM_PROVENANCE_INVOCATION_ID", "inv-child-abc")
	t.Setenv("MPM_PROVENANCE_PARENT_INVOCATION_ID", "inv-parent-xyz")

	// Create a work via the in-process invokeTool boundary. The
	// mpm_work tool's create action writes a works row plus an
	// artifact_provenance row carrying the ActiveContext.
	_, err := invokeTool("mpm_work", map[string]interface{}{
		"action": "create",
		"params": map[string]interface{}{
			"title": "parent linkage test",
		},
	})
	require.NoError(t, err)

	// The work we just wrote is the most recent works row.
	var workID string
	require.NoError(t, dm.SQLDB().QueryRow(
		`SELECT id FROM works ORDER BY created_at DESC LIMIT 1`,
	).Scan(&workID))

	// Find the artifact_provenance row for this artifact. There is
	// a UNIQUE constraint on (artifact_id, artifact_type) so at
	// most one row exists.
	var invocationID, parentInvocationID string
	err = dm.SQLDB().QueryRow(
		`SELECT invocation_id, parent_invocation_id
		 FROM artifact_provenance
		 WHERE artifact_id = ? AND artifact_type = 'work'
		 ORDER BY created_at DESC LIMIT 1`,
		workID,
	).Scan(&invocationID, &parentInvocationID)
	require.NoError(t, err, "expected artifact_provenance row for work %s", workID)

	assert.Equal(t, "inv-child-abc", invocationID,
		"invocation_id propagated from env to artifact_provenance")
	assert.Equal(t, "inv-parent-xyz", parentInvocationID,
		"parent_invocation_id propagated from env to artifact_provenance")

	t.Cleanup(func() {
		_, _ = dm.SQLDB().Exec(`DELETE FROM artifact_provenance WHERE artifact_id = ?`, workID)
	})
}

func TestD_ProvenanceParentInvocation_LinkageSurvivesThroughActiveContext(t *testing.T) {
	dm := internal.NewTestDM(t)
	overrideDM(t, dm)
	t.Setenv("MPM_PROVENANCE_INVOCATION_ID", "inv-flowtest")
	t.Setenv("MPM_PROVENANCE_PARENT_INVOCATION_ID", "inv-parent-flowtest")

	_, err := invokeTool("mpm_work", map[string]interface{}{
		"action": "create",
		"params": map[string]interface{}{
			"title": "flow test",
		},
	})
	require.NoError(t, err)

	// tool_invocations: invocation_id is persisted.
	var toolInvID string
	require.NoError(t, dm.SQLDB().QueryRow(
		`SELECT invocation_id FROM tool_invocations
		 WHERE tool_name = 'mpm_work' ORDER BY started_at DESC LIMIT 1`,
	).Scan(&toolInvID))
	assert.Equal(t, "inv-flowtest", toolInvID)

	// artifact_provenance: both invocation_id and parent_invocation_id
	// are persisted, and they link back to the tool call.
	var workID string
	require.NoError(t, dm.SQLDB().QueryRow(
		`SELECT id FROM works ORDER BY created_at DESC LIMIT 1`,
	).Scan(&workID))

	var artInvID, artParentID string
	require.NoError(t, dm.SQLDB().QueryRow(
		`SELECT invocation_id, parent_invocation_id FROM artifact_provenance
		 WHERE artifact_id = ? AND artifact_type = 'work'
		 ORDER BY created_at DESC LIMIT 1`,
		workID,
	).Scan(&artInvID, &artParentID))
	assert.Equal(t, "inv-flowtest", artInvID,
		"tool_invocations.invocation_id == artifact_provenance.invocation_id (join key works)")
	assert.Equal(t, "inv-parent-flowtest", artParentID,
		"parent_invocation_id survives the canonical flow")

	t.Cleanup(func() {
		_, _ = dm.SQLDB().Exec(`DELETE FROM artifact_provenance WHERE artifact_id = ?`, workID)
	})
}

func TestD_ProvenanceParentInvocation_EmptyValueHonoredNotInvented(t *testing.T) {
	dm := internal.NewTestDM(t)
	overrideDM(t, dm)
	t.Setenv("MPM_PROVENANCE_INVOCATION_ID", "inv-only")
	t.Setenv("MPM_PROVENANCE_PARENT_INVOCATION_ID", "")

	_, err := invokeTool("mpm_work", map[string]interface{}{
		"action": "create",
		"params": map[string]interface{}{
			"title": "no parent",
		},
	})
	require.NoError(t, err)

	var workID string
	require.NoError(t, dm.SQLDB().QueryRow(
		`SELECT id FROM works ORDER BY created_at DESC LIMIT 1`,
	).Scan(&workID))

	// The empty parent_invocation_id either doesn't produce a row
	// at all OR the row's parent_invocation_id is NULL/empty. We
	// document the actual behaviour rather than asserting a
	// specific shape — the S7.2 fix only affects the propagation
	// path, not the substrate's "skip when empty" short-circuit.
	var count int
	require.NoError(t, dm.SQLDB().QueryRow(
		`SELECT COUNT(*) FROM artifact_provenance WHERE artifact_id = ? AND artifact_type = 'work'`,
		workID,
	).Scan(&count))
	// Provenance may or may not be written for empty parent; either
	// way the linkage is correctly absent (no invented placeholder).
	if count > 0 {
		var parentID interface{}
		require.NoError(t, dm.SQLDB().QueryRow(
			`SELECT parent_invocation_id FROM artifact_provenance
			 WHERE artifact_id = ? AND artifact_type = 'work'`,
			workID,
		).Scan(&parentID))
		if s, ok := parentID.(string); ok {
			assert.Equal(t, "", s, "no invented placeholder when env unset")
		} else {
			assert.Nil(t, parentID, "no invented placeholder when env unset")
		}
	}
}
