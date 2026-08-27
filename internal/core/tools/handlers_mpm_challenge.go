package tools

import (
	"fmt"

	mpminternal "github.com/flowbyte-com/mpm-core"
)

// handleMpmChallenge is the F9 top-level tool handler (2026-08-27).
//
// The challenge action (weaken a memory + create a pending theory) was
// previously reachable only via the mpm_memory dispatcher:
//   mpm call mpm_memory --payload '{"action":"challenge","params":{...}}'
//
// This top-level tool exposes it as a canonical, first-class capability.
// It wraps dm.ChallengeMemoryWithTheory directly — same behavior, same
// response shape (memory_id, theory_id, theory_status, action), so the
// existing callers see no change.
//
// Wire contract (flat at the top level, matching other top-level tools
// like mpm_blob_read / mpm_resolve):
//   memory_id   (string, required) — id of the memory to challenge
//   evidence    (string, optional) — why this memory is contested
//
// For backward compat, a nested params envelope is also accepted:
//   {"params":{"memory_id":"...","evidence":"..."}}
//
// See also: mpm_memory.challenge (legacy action; preserved for parity).
func handleMpmChallenge(dm mpminternal.CoreDB, ac mpminternal.ActiveContext, payload map[string]interface{}) (interface{}, error) {
	if payload == nil {
		return nil, fmt.Errorf("mpm_challenge: missing payload")
	}
	memoryID, evidence := extractMemoryIDAndEvidence(payload)
	if memoryID == "" {
		return nil, fmt.Errorf("memory_id is required")
	}
	return dm.ChallengeMemoryWithTheory(memoryID, evidence)
}

// extractMemoryIDAndEvidence accepts both flat top-level fields
// (canonical) and a nested params envelope (backward compat). Returns
// the resolved id and evidence; either may be empty.
func extractMemoryIDAndEvidence(payload map[string]interface{}) (string, string) {
	memoryID, _ := payload["memory_id"].(string)
	evidence, _ := payload["evidence"].(string)
	if params, ok := payload["params"].(map[string]interface{}); ok {
		if memoryID == "" {
			memoryID, _ = params["memory_id"].(string)
		}
		if evidence == "" {
			evidence, _ = params["evidence"].(string)
		}
	}
	return memoryID, evidence
}
