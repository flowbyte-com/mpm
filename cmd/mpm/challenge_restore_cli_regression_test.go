// challenge_restore_cli_regression_test.go — F11 alpha-blocker regression
// at the CLI/tool surface.
//
// Audit finding F11: challenge reported "weakened" while the stored weight
// INCREASED (5 → 7) on 5/5 reproductions, and restore cleared the flags but
// preserved the inflation. These tests drive the full `mpm call mpm_memory
// action=challenge` path and the `mpm challenge restore` handler, then
// inspect persisted state directly.
package main

import (
	"encoding/json"
	"testing"

	mpminternal "github.com/flowbyte-com/mpm-core"
)

func persistedWeight(t *testing.T, dm *mpminternal.DatabaseManager, id string) (int, map[string]interface{}) {
	t.Helper()
	var weight int
	var metaStr string
	if err := dm.SQLDB().QueryRow(`SELECT COALESCE(weight,0), COALESCE(metadata,'{}') FROM memories WHERE id = ? AND deleted_at IS NULL`, id).
		Scan(&weight, &metaStr); err != nil {
		t.Fatalf("persistedWeight(%s): %v", id, err)
	}
	meta := map[string]interface{}{}
	if err := json.Unmarshal([]byte(metaStr), &meta); err != nil {
		t.Fatalf("unmarshal metadata: %v", err)
	}
	return weight, meta
}

// TestF11_CallChallenge_WeakenedNotBoosted drives the exact audit surface:
// `mpm call mpm_memory {"action":"challenge","memoryId":...,"evidence":...}`.
func TestF11_CallChallenge_WeakenedNotBoosted(t *testing.T) {
	dm := newTestDMForCmd(t)

	id, err := dm.SaveMemory("memories", "audit reproduction memory: nginx logstats parsing", "", []string{"f11"}, nil, nil, false, 5)
	if err != nil {
		t.Fatalf("seed memory: %v", err)
	}

	result, err := runHandler(dm, "mpm_memory", map[string]interface{}{
		"action": "challenge",
		"params": map[string]interface{}{
			"memoryId": id,
			"evidence": "contradicted by newer measurement",
		},
	})
	if err != nil {
		t.Fatalf("challenge call failed: %v", err)
	}
	res := result.(map[string]interface{})
	if res["action"] != "weakened" {
		t.Errorf("action = %v, want weakened", res["action"])
	}

	weight, meta := persistedWeight(t, dm, id)
	if weight >= 5 {
		t.Errorf("F11 REGRESSION: stored weight after challenge = %d, want < 5 (audit bug was 5 → 7)", weight)
	}
	if meta["status"] != "challenged" {
		t.Errorf("status = %v, want challenged", meta["status"])
	}
	theoryID, _ := meta["challenged_theory_id"].(string)
	if theoryID == "" || theoryID == "contradicted by newer measurement" {
		t.Errorf("challenged_theory_id must reference the real theory row, got %q", theoryID)
	}
}

// TestF11_RestoreRoundTrip covers the second half of the audit repro:
// restore must clear the flags AND return the pre-challenge epistemic state.
func TestF11_RestoreRoundTrip(t *testing.T) {
	dm := newTestDMForCmd(t)

	id, err := dm.SaveMemory("memories", "restore round trip target", "", []string{"f11"}, nil, nil, false, 5)
	if err != nil {
		t.Fatalf("seed memory: %v", err)
	}

	if _, err := runHandler(dm, "mpm_memory", map[string]interface{}{
		"action": "challenge",
		"params": map[string]interface{}{
			"memoryId": id,
			"evidence": "disputed",
		},
	}); err != nil {
		t.Fatalf("challenge: %v", err)
	}

	_, meta := persistedWeight(t, dm, id)
	theoryID, _ := meta["challenged_theory_id"].(string)

	// Drive the CLI restore handler (`mpm challenge restore <id>`).
	if code := runChallengeRestore(dm, id); code != 0 {
		t.Fatalf("runChallengeRestore exit = %d, want 0", code)
	}

	weight, metaAfter := persistedWeight(t, dm, id)
	if weight != 5 {
		t.Errorf("weight after restore = %d, want 5 (pre-challenge value)", weight)
	}
	for _, key := range []string{"status", "challenged_theory_id", "challenged_prior_weight"} {
		if _, ok := metaAfter[key]; ok {
			t.Errorf("metadata key %q survived restore; want removed", key)
		}
	}

	var theoryStatus string
	if err := dm.SQLDB().QueryRow(`SELECT json_extract(metadata,'$.status') FROM memories WHERE id = ?`, theoryID).Scan(&theoryStatus); err != nil {
		t.Fatalf("theory lookup: %v", err)
	}
	if theoryStatus != "disproven" {
		t.Errorf("theory status after restore = %q, want disproven", theoryStatus)
	}
}

// TestF11_ChallengeRestoreCyclesStable repeats the cycle to prove no drift.
func TestF11_ChallengeRestoreCyclesStable(t *testing.T) {
	dm := newTestDMForCmd(t)

	id, err := dm.SaveMemory("memories", "cycle stability target", "", []string{"f11"}, nil, nil, false, 8)
	if err != nil {
		t.Fatalf("seed memory: %v", err)
	}

	for cycle := 0; cycle < 3; cycle++ {
		if _, err := runHandler(dm, "mpm_memory", map[string]interface{}{
			"action": "challenge",
			"params": map[string]interface{}{
				"memoryId": id,
				"evidence": "cycle",
			},
		}); err != nil {
			t.Fatalf("cycle %d challenge: %v", cycle, err)
		}
		w, _ := persistedWeight(t, dm, id)
		if w != 6 {
			t.Fatalf("cycle %d: challenged weight = %d, want 6", cycle, w)
		}
		if code := runChallengeRestore(dm, id); code != 0 {
			t.Fatalf("cycle %d restore exit = %d", cycle, code)
		}
		w, _ = persistedWeight(t, dm, id)
		if w != 8 {
			t.Fatalf("cycle %d: restored weight = %d, want 8 (no drift)", cycle, w)
		}
	}
}
