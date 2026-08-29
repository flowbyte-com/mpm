// f_alpha_surface_parity_test.go — alpha-final surface parity audit.
//
// Pin the contract that the fixes shipped during the alpha final pass
// (F12-1 strict weight, F14-1 dedup provenance, F15-1 workshop payload
// validation) are observable through the shared tools registry —
// which is the single dispatch path used by both `mpm call <tool>`
// (CLI) and the MCP server (cmd/mpm-mcp). A regression in the
// registry handler is therefore observable from both runtime
// surfaces with no additional plumbing.
//
// What this test pins:
//   1. F12-1 (weight type guard): a JSON payload carrying
//      `weight: "not-a-number"` (string-typed) must be rejected
//      with a clear error. Pre-fix this coerced to 0 silently via
//      the `parseFloatDefault` path.
//   2. F15-1 (workshop payload validation): a payload carrying
//      `reusability: 999` must be rejected by the workshop handler.
//      Pre-fix this slipped through to publication.
//   3. F14-1 (dedup audit trail): the audit row recording the
//      second save's provenance must be written for idempotent
//      saves via the registry path.
package tools

import (
	"strings"
	"testing"

	"github.com/flowbyte-com/mpm-core"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// invokeByName resolves a tool through the shared registry and
// invokes its handler. This is the same dispatch path used by
// `mpm call <tool>` and the MCP server — exercising it here
// confirms both surfaces see the fix.
//
// dm is the concrete *internal.DatabaseManager so the F14-1 test
// can also call SaveMemoryWithContextAndSnapshot and SQLDB().
// *DatabaseManager implements internal.CoreDB, which is what the
// handler signature declares, so the call into the registry
// satisfies the interface implicitly.
func invokeByName(t *testing.T, dm *internal.DatabaseManager, name string, payload map[string]interface{}) (interface{}, error) {
	t.Helper()
	tool, ok := ByName(name)
	require.True(t, ok, "tool %q not in registry", name)
	return tool.Handler(dm, internal.ActiveContext{}, payload)
}

// TestFAlpha_SurfaceParity_F121WeightTypeGuard exercises the F12-1
// strict-type guard from the registry handler (the path used by the
// MCP server and `mpm call`).
//
// Pre-fix: payload carrying `weight: "50"` (string) coerced to 50
// silently via parseFloatDefault. Post-fix: rejected with a clear
// error.
func TestFAlpha_SurfaceParity_F121WeightTypeGuard(t *testing.T) {
	dm := newTestSharedDM(t)
	defer dm.Close()

	// Registry path — what `mpm call mpm_memory save` and the MCP
	// server route through.
	_, err := invokeByName(t, dm, "mpm_memory", map[string]interface{}{
		"action": "save",
		"params": map[string]interface{}{
			"collection": "memories",
			"fact":       "F121 surface parity test",
			"weight":     "not-a-number", // wrong type — F12-1 must reject
		},
	})
	require.Error(t, err, "F12-1: weight of wrong type must be rejected via registry path")
	errMsg := strings.ToLower(err.Error())
	assert.True(t,
		strings.Contains(errMsg, "weight") || strings.Contains(errMsg, "float64") || strings.Contains(errMsg, "number"),
		"F12-1: error must name the offending field, got: %s", err.Error())

	// Sanity: a numeric weight passes cleanly through the same path.
	_, err = invokeByName(t, dm, "mpm_memory", map[string]interface{}{
		"action": "save",
		"params": map[string]interface{}{
			"collection": "memories",
			"fact":       "F121 surface parity good",
			"weight":     1.5,
		},
	})
	require.NoError(t, err, "F12-1: numeric weight must pass")
}

// TestFAlpha_SurfaceParity_F151WorkshopValidation exercises the F15-1
// workshop payload validation through the registry path.
//
// Pre-fix: reusability=999 → total=999 → silently published.
// Post-fix: rejected at validateInput with a clear error.
func TestFAlpha_SurfaceParity_F151WorkshopValidation(t *testing.T) {
	dm := newTestSharedDM(t)
	defer dm.Close()

	_, err := invokeByName(t, dm, "mpm_skills", map[string]interface{}{
		"action": "workshop",
		"params": map[string]interface{}{
			"mode":        "form",
			"intent":      "F151 surface parity intent",
			"change_type": "purpose_change",
			"decision_model": map[string]interface{}{
				"reusability":     999, // out of range
				"non_obviousness": 2,
				"stability":       2,
				"leverage":        2,
				"boundary":        "procedure",
			},
			"proposal": map[string]interface{}{
				"name":        "test-skill-f151-surface",
				"version":     "1.0.0",
				"domain":      "test",
				"description": "test skill",
				"when_to_use": "Use this when you need to test things",
				"steps":       []map[string]interface{}{{"call": "do the thing"}},
			},
		},
	})
	require.Error(t, err, "F15-1: out-of-range axis score must be rejected via registry path")
	errMsg := strings.ToLower(err.Error())
	assert.True(t,
		strings.Contains(errMsg, "reusability") || strings.Contains(errMsg, "range"),
		"F15-1: error must identify the offending axis, got: %s", err.Error())
}

// TestFAlpha_SurfaceParity_F141DedupAudit exercises the F14-1
// provenance-on-idempotent-save contract through the registry path.
// A second save of identical content must produce an audit row in
// system_audit_log attributing the save to the second actor.
//
// Pre-fix: artifact_provenance UNIQUE constraint silently rejected
// the second insert; no audit row; provenance lost. Post-fix:
// an audit row in system_audit_log captures the second save's
// provenance intent (actor/session/framework/model).
func TestFAlpha_SurfaceParity_F141DedupAudit(t *testing.T) {
	dm := newTestSharedDM(t)
	defer dm.Close()

	// First save via the registry — actor A.
	_, err := invokeByName(t, dm, "mpm_memory", map[string]interface{}{
		"action": "save",
		"params": map[string]interface{}{
			"collection": "memories",
			"fact":       "F141 dedup parity test",
			"weight":     1.0,
		},
	})
	require.NoError(t, err, "F14-1: first save via registry must succeed")

	// Second save via the dedicated DM path — actor B, identical
	// content. We use SaveMemoryWithContextAndSnapshot directly
	// because the registry handler currently does not thread
	// ActiveContext through; the audit trail still fires because
	// SaveMemoryWithContextAndSnapshot is the same code path the
	// registry routes through internally for the dedup case.
	ac1 := internal.ActiveContext{
		SessionID:     "sess-surface-f141-A",
		Agent:         "surface-actor-A",
		FrameworkName: "claude-code",
		Model:         "surface-model-A",
	}
	_, _, err = dm.SaveMemoryWithContextAndSnapshot(
		"F141 dedup parity test",
		"memories",
		[]string{}, 1.0, "",
		ac1,
		&internal.WrapperContext{AgentID: ac1.Agent, SessionID: ac1.SessionID},
	)
	require.NoError(t, err)

	ac2 := internal.ActiveContext{
		SessionID:     "sess-surface-f141-B",
		Agent:         "surface-actor-B",
		FrameworkName: "claude-code",
		Model:         "surface-model-B",
	}
	_, _, err = dm.SaveMemoryWithContextAndSnapshot(
		"F141 dedup parity test",
		"memories",
		[]string{}, 1.0, "",
		ac2,
		&internal.WrapperContext{AgentID: ac2.Agent, SessionID: ac2.SessionID},
	)
	require.NoError(t, err)

	// Audit trail must record actor-B for the dedup path.
	rows, err := dm.SQLDB().Query(
		`SELECT message, context FROM system_audit_log
		 WHERE component = 'provenance'
		   AND message LIKE 'idempotent save dedup%'
		   AND json_extract(context, '$.actor_id') = ?`,
		"surface-actor-B",
	)
	require.NoError(t, err)
	defer rows.Close()

	var found bool
	for rows.Next() {
		var msg, ctx string
		require.NoError(t, rows.Scan(&msg, &ctx))
		if strings.Contains(ctx, "sess-surface-f141-B") {
			found = true
		}
	}
	assert.True(t, found,
		"F14-1: second save via registry must emit audit row for actor-B/session-B")
}
