package tools

import (
	"strings"
	"testing"

	mpminternal "github.com/flowbyte-com/mpm-core"
)

// TestMpmChallenge_TopLevelTool is the F9 regression test (2026-08-27).
//
// Bug: the challenge action (weaken a memory + create a pending theory)
// was reachable only via the mpm_memory dispatcher:
//   mpm call mpm_memory --payload '{"action":"challenge","params":{"memory_id":"..."}}'
// There was no canonical top-level tool, even though the registry
// advertises itself as the source of truth for both the CLI's
// `mpm call <tool>` surface and the MCP server's ListTools handler.
//
// Contract after fix: mpm_challenge is registered as a top-level tool
// that wraps ChallengeMemoryWithTheory. Same wire contract as the
// mpm_memory.challenge action (params: memory_id, evidence).
//
// Parity test: the new top-level tool and the mpm_memory.chaction action
// produce equivalent results for the same inputs.
func TestMpmChallenge_TopLevelTool(t *testing.T) {
	dm := newTestSharedDM(t)

	probeID := mpmChallengeSeed(t, dm)

	// Top-level mpm_challenge — must succeed.
	res, err := handleMpmChallenge(dm, mpminternal.ActiveContext{}, map[string]interface{}{
		"params": map[string]interface{}{"memory_id": probeID, "evidence": "f9 top-level tool"},
	})
	if err != nil {
		t.Fatalf("mpm_challenge top-level: %v", err)
	}
	if res == nil {
		t.Fatal("mpm_challenge returned nil result")
	}
	m, ok := res.(map[string]interface{})
	if !ok {
		t.Fatalf("mpm_challenge result type %T, want map", res)
	}
	if m["theory_id"] == "" {
		t.Errorf("mpm_challenge result missing theory_id: %v", m)
	}
	if m["memory_id"] != probeID {
		t.Errorf("mpm_challenge memory_id = %v, want %s", m["memory_id"], probeID)
	}
}

// TestMpmChallenge_ParamsRequired asserts the user-facing contract:
// missing memory_id is reported with a clear error message that names
// the required field.
func TestMpmChallenge_ParamsRequired(t *testing.T) {
	dm := newTestSharedDM(t)

	_, err := handleMpmChallenge(dm, mpminternal.ActiveContext{}, map[string]interface{}{
		"params": map[string]interface{}{"evidence": "missing memory_id"},
	})
	if err == nil {
		t.Fatal("mpm_challenge with missing memory_id: expected error, got nil")
	}
	if !strings.Contains(err.Error(), "memory_id") {
		t.Errorf("error %q missing 'memory_id' guidance", err.Error())
	}
}

// TestMpmChallenge_ParityWithMpmMemoryAction asserts that the top-level
// tool and the legacy mpm_memory.challenge action produce equivalent
// results for the same input. The new tool must not be a thin alias —
// it must call ChallengeMemoryWithTheory directly, surfacing the same
// theory row.
func TestMpmChallenge_ParityWithMpmMemoryAction(t *testing.T) {
	dm := newTestSharedDM(t)

	// Create two memories — one for each invocation path.
	viaTop := mpmChallengeSeed(t, dm)
	viaAction := mpmChallengeSeed(t, dm)

	topRes, err := handleMpmChallenge(dm, mpminternal.ActiveContext{}, map[string]interface{}{
		"params": map[string]interface{}{"memory_id": viaTop, "evidence": "f9 parity top"},
	})
	if err != nil {
		t.Fatalf("top-level challenge: %v", err)
	}
	actionRes, err := handleMpmMemory(dm, mpminternal.ActiveContext{}, map[string]interface{}{
		"action": "challenge",
		"params": map[string]interface{}{"memory_id": viaAction, "evidence": "f9 parity action"},
	})
	if err != nil {
		t.Fatalf("action-based challenge: %v", err)
	}

	topM := topRes.(map[string]interface{})
	actM := actionRes.(map[string]interface{})
	if topM["theory_id"] == "" || actM["theory_id"] == "" {
		t.Fatalf("parity: missing theory_id in result(s): top=%v action=%v", topM, actM)
	}
	if topM["theory_status"] != actM["theory_status"] {
		t.Errorf("parity: theory_status differs: top=%v action=%v", topM["theory_status"], actM["theory_status"])
	}
}

// mpmChallengeSeed inserts a memory and returns its id. Helper for the
// F9 regression suite.
func mpmChallengeSeed(t *testing.T, dm mpminternal.CoreDB) string {
	t.Helper()
	res, err := handleMpmMemory(dm, mpminternal.ActiveContext{}, map[string]interface{}{
		"action": "save",
		"params": map[string]interface{}{"fact": "f9 challenge probe"},
	})
	if err != nil {
		t.Fatalf("save probe: %v", err)
	}
	id, _ := res.(map[string]interface{})["id"].(string)
	if id == "" {
		t.Fatalf("save returned empty id: %v", res)
	}
	return id
}

// TestMpmChallenge_FlatPayloadCanonical asserts the canonical wire
// shape — flat top-level fields, no params envelope — works exactly
// like the nested envelope does. This is what the registry schema
// advertises and what an MCP client will send after reading the tool
// definition. Pre-fix (extractParamsOrFail), the handler rejected flat
// payloads with "missing required field action".
func TestMpmChallenge_FlatPayloadCanonical(t *testing.T) {
	dm := newTestSharedDM(t)

	probeID := mpmChallengeSeed(t, dm)

	res, err := handleMpmChallenge(dm, mpminternal.ActiveContext{}, map[string]interface{}{
		"memory_id": probeID,
		"evidence":  "f9 flat canonical payload",
	})
	if err != nil {
		t.Fatalf("mpm_challenge flat payload: %v", err)
	}
	m, ok := res.(map[string]interface{})
	if !ok {
		t.Fatalf("mpm_challenge flat result type %T, want map", res)
	}
	if m["memory_id"] != probeID {
		t.Errorf("mpm_challenge flat memory_id = %v, want %s", m["memory_id"], probeID)
	}
	if m["theory_id"] == "" {
		t.Errorf("mpm_challenge flat result missing theory_id: %v", m)
	}
}
