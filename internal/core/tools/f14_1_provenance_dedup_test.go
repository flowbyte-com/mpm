// f14_1_provenance_dedup_test.go — F14-1 alpha-final regression.
//
// F14-1: silent provenance loss on idempotent save. The F19 idempotency
// contract returns the EXISTING memory row's id when an identical
// save is attempted (same collection + content + tags + metadata).
// But provenance is keyed on (artifact_id, artifact_type) with a UNIQUE
// constraint — so a second save to the same artifact_id silently
// fails to record its provenance row. The audit trail loses "who
// saved this and when" for the second-and-subsequent saves.
//
// The corrected contract: provenance is recorded PER SAVE ATTEMPT, not
// per artifact id. Either:
//   (a) the artifact_provenance row is keyed on (artifact_id, save_attempt_id),
//       allowing multiple rows per artifact; OR
//   (b) the dedup path surfaces the second save's provenance intent in
//       a separate audit/reinforcement table so the "second save at
//       time T by actor A" fact is preserved.
//
// This test pins the regression: a second save with a DIFFERENT
// actor/session MUST produce a visible provenance record in the
// audit ledger. Pre-fix, the second save's provenance was silently
// dropped.
package tools

import (
	"strings"
	"testing"

	mpminternal "github.com/flowbyte-com/mpm-core"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestF14_1_ProvenancePersistedOnIdempotentSave verifies that when an
// identical-content save dedups to the existing row, the SECOND save's
// provenance is still recorded somewhere in the audit trail. Pre-fix,
// the second save silently dropped provenance because the UNIQUE
// (artifact_id, artifact_type) constraint on artifact_provenance
// rejected the duplicate.
func TestF14_1_ProvenancePersistedOnIdempotentSave(t *testing.T) {
	dm := f3_3NewDM(t)

	// First save: actor A, session S1.
	ac1 := mpminternal.ActiveContext{
		SessionID:     "sess-f141-first",
		Agent:         "actor-A",
		FrameworkName: "claude-code",
		Model:         "test-model-1",
	}
	firstResult, _, err := dm.SaveMemoryWithContextAndSnapshot(
		"f14-1 dedup provenance test",
		"memories",
		[]string{"f14-1"},
		1.0,
		"",
		ac1,
		&mpminternal.WrapperContext{AgentID: ac1.Agent, SessionID: ac1.SessionID},
	)
	require.NoError(t, err)
	require.NotNil(t, firstResult)
	firstID, ok := firstResult["id"].(string)
	require.True(t, ok, "first save result must include id")
	require.NotEmpty(t, firstID)

	// Second save: actor B, session S2, IDENTICAL content/tags/metadata.
	// F19 idempotency: should return the EXISTING row's id (the
	// dedup path returns (resultMap, nil, nil) where resultMap["id"]
	// is the existing row's id and resultMap["duplicate"]=true).
	ac2 := mpminternal.ActiveContext{
		SessionID:     "sess-f141-second",
		Agent:         "actor-B",
		FrameworkName: "claude-code",
		Model:         "test-model-2",
	}
	secondResult, _, err := dm.SaveMemoryWithContextAndSnapshot(
		"f14-1 dedup provenance test", // identical content
		"memories",
		[]string{"f14-1"}, // identical tags
		1.0,                // identical weight
		"",
		ac2,
		&mpminternal.WrapperContext{AgentID: ac2.Agent, SessionID: ac2.SessionID},
	)
	require.NoError(t, err)
	require.NotNil(t, secondResult)
	secondID, ok := secondResult["id"].(string)
	require.True(t, ok, "second save result must include id")
	assert.Equal(t, firstID, secondID,
		"F19 idempotency: identical save must dedup to the existing row")
	if dup, _ := secondResult["duplicate"].(bool); !dup {
		t.Logf("note: second save did not mark duplicate=true (still returned the right id)")
	}

	// Audit-trail invariant: the second save's provenance intent
	// (actor=B, session=S2) must be observable SOMEWHERE in the
	// durable audit ledger. artifact_provenance is keyed on
	// (artifact_id, artifact_type) with a UNIQUE constraint — a direct
	// re-insert is silently rejected. The audit_log is the canonical
	// durable home for "who re-saved this and when": the dedup path
	// emits an AuditInfo row tagged 'provenance' carrying the second
	// save's actor / session / framework / model so a future forensic
	// query can reconstruct the save history.
	rows, err := dm.SQLDB().Query(
		`SELECT message, context FROM system_audit_log
		 WHERE component = 'provenance'
		   AND message LIKE 'idempotent save dedup%'
		   AND json_extract(context, '$.artifact_id') = ?`,
		firstID,
	)
	require.NoError(t, err)
	defer rows.Close()

	type auditRow struct {
		message string
		context string
	}
	var auditRows []auditRow
	for rows.Next() {
		var msg, ctx string
		require.NoError(t, rows.Scan(&msg, &ctx))
		auditRows = append(auditRows, auditRow{message: msg, context: ctx})
	}

	// Pre-fix: no audit row at all — the dedup path returned silently.
	// Post-fix: at minimum one audit row records the second save's
	// provenance intent. The audit context must include actor=B and
	// session=S2 so forensic queries can reconstruct the save chain.
	require.NotEmpty(t, auditRows,
		"idempotent save must emit an audit row recording the second save's provenance (pre-fix: silent loss)")

	foundActorB := false
	foundSessionS2 := false
	for _, r := range auditRows {
		if strings.Contains(r.context, `"actor-B"`) {
			foundActorB = true
		}
		if strings.Contains(r.context, `"sess-f141-second"`) {
			foundSessionS2 = true
		}
	}
	assert.True(t, foundActorB,
		"audit row must record actor=B for the second save. Got contexts: %+v", auditRows)
	assert.True(t, foundSessionS2,
		"audit row must record session=S2 for the second save. Got contexts: %+v", auditRows)
}

// TestF14_1_DistinctContentProducesDistinctProvenance is the
// regression-safety check: a save with DIFFERENT content must
// produce its own provenance row (the dedup path must not
// over-reach and merge distinct artifacts).
func TestF14_1_DistinctContentProducesDistinctProvenance(t *testing.T) {
	dm := f3_3NewDM(t)

	ac := mpminternal.ActiveContext{
		SessionID:     "sess-distinct",
		Agent:         "actor-distinct",
		FrameworkName: "claude-code",
		Model:         "test-model",
	}

	_, memA, err := dm.SaveMemoryWithContextAndSnapshot(
		"f14-1 distinct content A",
		"memories", []string{"f14-1-distinct"}, 1.0, "",
		ac,
		&mpminternal.WrapperContext{AgentID: ac.Agent, SessionID: ac.SessionID},
	)
	require.NoError(t, err)
	require.NotNil(t, memA)
	idA := memA.ID
	require.NotEmpty(t, idA)

	_, memB, err := dm.SaveMemoryWithContextAndSnapshot(
		"f14-1 distinct content B",
		"memories", []string{"f14-1-distinct"}, 1.0, "",
		ac,
		&mpminternal.WrapperContext{AgentID: ac.Agent, SessionID: ac.SessionID},
	)
	require.NoError(t, err)
	require.NotNil(t, memB)
	idB := memB.ID
	require.NotEmpty(t, idB)
	require.NotEqual(t, idA, idB,
		"distinct content must produce distinct memory rows; the dedup path must not over-reach")

	// Both rows must have their own provenance.
	for _, id := range []string{idA, idB} {
		var actor string
		err := dm.SQLDB().QueryRow(
			`SELECT actor_id FROM artifact_provenance
			 WHERE artifact_id = ? AND artifact_type = 'memory'`,
			id,
		).Scan(&actor)
		require.NoError(t, err, "distinct artifact %s missing provenance row", id)
		assert.Equal(t, "actor-distinct", actor)
	}
}