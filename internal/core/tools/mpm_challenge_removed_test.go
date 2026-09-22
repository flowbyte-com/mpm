// mpm_challenge_removed_test.go — pins the retirement of the standalone
// mpm_challenge tool.
//
// The onboarding-mcp-native audit
// (docs/onboarding-mcp-native-audit-2026-09-05.md Part C) found
// `mpm_challenge` and `mpm_memory.challenge` calling identical code
// paths with identical wire contracts. Per the follow-up, the
// standalone tool was retired 2026-09-05; the canonical challenge
// surface is now the `mpm_memory` action=`challenge` (and the matching
// `restore_challenge` action for the restore side).
//
// These tests pin the retirement so the tool cannot silently reappear
// in the registry, and ensure the F7-1 invariant (challenge ↔ restore)
// is preserved through `mpm_memory` actions only.

package tools

import (
	"testing"

	mpminternal "github.com/flowbyte-com/mpm-core"
)

// TestRegistry_mpm_challenge_Retired asserts the standalone tool has been
// removed from the Registry. The canonical challenge path is now
// `mpm_memory action=challenge`; see f7_1_challenge_restore_surface_test.go.
func TestRegistry_mpm_challenge_Retired(t *testing.T) {
	for _, tool := range Registry {
		if tool.Name == "mpm_challenge" {
			t.Errorf("Registry must not contain the retired mpm_challenge tool " +
				"(duplicates mpm_memory action=challenge with identical wire contract " +
				"— see docs/onboarding-mcp-native-audit-2026-09-05.md Part C)")
		}
	}
}

// TestRegistry_CurrentToolCount pins the size of the surface so a
// future tool add/remove can't drift silently. As of the mpm_challenge
// retirement (2026-09-05), the registry holds exactly 21 substrate
// Registry entries (plus `mpm_help` discovery closure = 22 MCP
// registrations when `MPM_EXPOSE_ALL_TOOLS=1`):
//
//   - 14 domain tools (mpm_memory, mpm_theories, mpm_decisions,
//     mpm_lessons, mpm_topics, mpm_references, mpm_evidence,
//     mpm_confidence, mpm_context, mpm_skills, mpm_wakes, mpm_handoff,
//     mpm_scratchpad, mpm_system)
//   - 4 pointer/dedicated tools (mpm_work, mpm_resolve, mpm_blob_read,
//     mpm_blob_search)
//   - 1 retrieval diagnostic (mpm_retrieval_diagnose)
//   - 2 non-MPM standalones (mpm_log_to_changelog, mpm_request_review)
//
// Change this constant deliberately, with a corresponding update to
// generator/docs that hard-code the count (e.g. specification tool listing,
// agent_installation INSTALL.md `expect: 21`, AUTO_AGENT_INSTALL.md).
func TestRegistry_CurrentToolCount(t *testing.T) {
	const want = 21
	if got := len(Registry); got != want {
		t.Fatalf("Registry has %d tools, want %d. If you added or removed a tool, "+
			"update this test deliberately and re-run "+
			"`go run ./cmd/gen-readme` so the specification's auto-generated tool listing stays in sync.",
			got, want)
	}
}

// TestF7_1_ChallengeAction_StillReachable pins that the canonical
// challenge + restore_challenge actions under mpm_memory still work
// after the standalone tool's retirement. Without this, removing
// `mpm_challenge` could silently break the challenge → restore cycle.
func TestF7_1_ChallengeAction_StillReachable(t *testing.T) {
	dm := f6NewDM(t)
	id, err := dm.SaveMemory(
		"memories", "F7-1 survivor: challenge via action still works",
		"", []string{"f7-1-survivor"}, nil, nil, false, 7)
	if err != nil {
		t.Fatalf("seed: %v", err)
	}

	// Action: challenge — the canonical surface post-retirement.
	challengeRes, err := handleMpmMemory(dm, mpminternal.ActiveContext{}, map[string]interface{}{
		"action": "challenge",
		"params": map[string]interface{}{
			"memory_id": id, "evidence": "post-retirement regression",
		},
	})
	if err != nil {
		t.Fatalf("mpm_memory action=challenge must succeed: %v", err)
	}
	if got, ok := challengeRes.(map[string]interface{})["memory_id"]; !ok || got != id {
		t.Errorf("challenge result missing memory_id=%s: %v", id, challengeRes)
	}

	// Action: restore_challenge — the inverse path.
	restoreRes, err := handleMpmMemory(dm, mpminternal.ActiveContext{}, map[string]interface{}{
		"action": "restore_challenge",
		"params": map[string]interface{}{"memory_id": id},
	})
	if err != nil {
		t.Fatalf("mpm_memory action=restore_challenge must succeed: %v", err)
	}
	if got, ok := restoreRes.(map[string]interface{})["action"]; !ok || got != "restored" {
		t.Errorf("restore_challenge result missing action=restored: %v", restoreRes)
	}
}
