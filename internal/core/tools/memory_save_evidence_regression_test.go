// memory_save_evidence_regression_test.go — pin the contract for the
// `evidence` parameter on mpm_memory action=save.
//
// The tool exposes one action-agnostic `params` object, and `evidence`
// is part of it because action=challenge uses it. handleSaveToMemory
// never read it, so:
//
//	{"action":"save","params":{"fact":"...","evidence":"supporting test results"}}
//
// returned success while the supplied evidence was silently discarded.
// That is the worst shape for an agent: the call looks like it recorded
// the observation, and the observation is gone.
//
// The fix must not reinterpret free text as a verified observation (that
// would let a caller mint trust by writing prose). The contract is:
// action=save does not persist evidence, so it must not accept it
// silently — it refuses with the path that does record evidence.
package tools

import (
	"strings"
	"testing"

	internal "github.com/flowbyte-com/mpm-core"
)

// countMemoriesWithContent is a direct probe: nothing about the rejection
// may be implemented by inspecting the response alone.
func countMemoriesWithContent(t *testing.T, dm *internal.DatabaseManager, content string) int {
	t.Helper()
	var n int
	if err := dm.SQLDB().QueryRow(
		`SELECT COUNT(*) FROM memories WHERE content = ?`, content,
	).Scan(&n); err != nil {
		t.Fatalf("count memories: %v", err)
	}
	return n
}

// TestSaveWithEvidence_IsRefusedNotSilentlyDiscarded is the core
// regression. A non-empty `evidence` must fail the save, name the
// supported path, and leave no memory behind.
func TestSaveWithEvidence_IsRefusedNotSilentlyDiscarded(t *testing.T) {
	dm := newTestDMForTools(t)

	for _, tc := range []struct {
		name     string
		evidence interface{}
	}{
		{"string", "supporting test results"},
		{"object", map[string]interface{}{"url": "https://example.test/run/1"}},
		{"number", 42},
	} {
		t.Run(tc.name, func(t *testing.T) {
			fact := "durable technical finding that must not half-save (" + tc.name + ")"
			_, err := handleMpmMemory(dm, internal.ActiveContext{}, map[string]interface{}{
				"action": "save",
				"params": map[string]interface{}{
					"fact":     fact,
					"evidence": tc.evidence,
				},
			})
			if err == nil {
				t.Fatal("save accepted evidence and reported success; the parameter was silently discarded")
			}
			if !strings.Contains(err.Error(), "mpm_evidence") {
				t.Errorf("refusal must name the path that records evidence, got: %v", err)
			}
			if n := countMemoriesWithContent(t, dm, fact); n != 0 {
				t.Errorf("refused save still wrote %d memory row(s)", n)
			}
		})
	}
}

// TestSaveWithEmptyEvidence_StillSaves keeps the shared-schema shape
// usable: an explicitly empty value is not data, so it must not block a
// save (canonicalisation layers often send "").
func TestSaveWithEmptyEvidence_StillSaves(t *testing.T) {
	dm := newTestDMForTools(t)
	res, err := handleMpmMemory(dm, internal.ActiveContext{}, map[string]interface{}{
		"action": "save",
		"params": map[string]interface{}{
			"fact":     "save with an empty optional evidence field",
			"evidence": "",
		},
	})
	if err != nil {
		t.Fatalf("empty evidence blocked a valid save: %v", err)
	}
	if id, _ := res.(map[string]interface{})["id"].(string); id == "" {
		t.Fatalf("save returned no id: %v", res)
	}
}

// TestChallengeWithEvidence_StillWorks guards against over-correction:
// action=challenge is the action the parameter exists for.
func TestChallengeWithEvidence_StillWorks(t *testing.T) {
	dm := newTestDMForTools(t)
	saved, err := handleMpmMemory(dm, internal.ActiveContext{}, map[string]interface{}{
		"action": "save",
		"params": map[string]interface{}{"fact": "claim that will be challenged"},
	})
	if err != nil {
		t.Fatalf("seed: %v", err)
	}
	id, _ := saved.(map[string]interface{})["id"].(string)

	if _, err := handleMpmMemory(dm, internal.ActiveContext{}, map[string]interface{}{
		"action": "challenge",
		"params": map[string]interface{}{
			"memory_id": id,
			"evidence":  "a contradicting observation",
		},
	}); err != nil {
		t.Fatalf("challenge with evidence must keep working: %v", err)
	}
}
