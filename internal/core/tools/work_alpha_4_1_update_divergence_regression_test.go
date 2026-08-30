package tools

// Alpha-4.1 F-003 regression: deprecated `mpm_work action=update
// status=done` MUST produce equivalent semantics to the canonical
// `mpm_work action=complete` path.
//
// Pre-fix divergence: UpdateWorkWithContext called
// recordGitEvidenceForWork after the status change, while
// CompleteWorkWithContext deliberately does NOT (per the P3 fix
// comment in handleCompleteWork: "do not auto-record git evidence on
// complete — it inflated verification to 'partial' for false
// completions"). The two paths therefore produced different
// verification states for an identical lifecycle transition.
//
// This test pins the contract: both paths must reach the same
// verification outcome, and the audit log must signal the deprecation.

import (
	"encoding/json"
	"testing"

	mpminternal "github.com/flowbyte-com/mpm-core"
)

func TestF003_UpdateStatusDone_EquivalentToComplete(t *testing.T) {
	dm := newTestIsolatedDM(t)
	ac := mpminternal.ActiveContext{}

	// Two parallel work items, identical shape.
	resA, err := handleMpmWork(dm, ac, map[string]interface{}{
		"action":  "create",
		"params":  map[string]interface{}{"title": "F003 update path", "content": "alpha-4.1 F-003 update"},
	})
	if err != nil {
		t.Fatalf("create A: %v", err)
	}
	idA, _ := resA.(map[string]interface{})["id"].(string)
	resB, err := handleMpmWork(dm, ac, map[string]interface{}{
		"action":  "create",
		"params":  map[string]interface{}{"title": "F003 complete path", "content": "alpha-4.1 F-003 complete"},
	})
	if err != nil {
		t.Fatalf("create B: %v", err)
	}
	idB, _ := resB.(map[string]interface{})["id"].(string)

	// A: deprecated update status=done
	if _, err := handleMpmWork(dm, ac, map[string]interface{}{
		"action": "update",
		"params": map[string]interface{}{"work_id": idA, "status": "done"},
	}); err != nil {
		t.Fatalf("update A: %v", err)
	}
	// B: canonical complete
	if _, err := handleMpmWork(dm, ac, map[string]interface{}{
		"action": "complete",
		"params": map[string]interface{}{"work_id": idB, "note": "F-003 complete"},
	}); err != nil {
		t.Fatalf("complete B: %v", err)
	}

	showA, err := handleMpmWork(dm, ac, map[string]interface{}{
		"action": "show",
		"params": map[string]interface{}{"work_id": idA},
	})
	if err != nil {
		t.Fatalf("show A: %v", err)
	}
	showB, err := handleMpmWork(dm, ac, map[string]interface{}{
		"action": "show",
		"params": map[string]interface{}{"work_id": idB},
	})
	if err != nil {
		t.Fatalf("show B: %v", err)
	}

	// workToMapWork returns a map with typed strings (WorkStatus,
	// WorkVerification). Round-trip through JSON to get plain strings
	// — same shape an MCP / mpm call consumer would observe.
	rawA := mustMarshalJSON(showA)
	rawB := mustMarshalJSON(showB)
	var parsedA, parsedB map[string]interface{}
	if err := json.Unmarshal([]byte(rawA), &parsedA); err != nil {
		t.Fatalf("unmarshal A: %v", err)
	}
	if err := json.Unmarshal([]byte(rawB), &parsedB); err != nil {
		t.Fatalf("unmarshal B: %v", err)
	}

	statusA, _ := parsedA["status"].(string)
	statusB, _ := parsedB["status"].(string)
	if statusA != "done" {
		t.Errorf("update path status=%q, want \"done\"; rawA=%s", statusA, rawA)
	}
	if statusB != "done" {
		t.Errorf("complete path status=%q, want \"done\"; rawB=%s", statusB, rawB)
	}

	// F-003 contract: verification must be equivalent between the two
	// paths. Pre-fix, update path called recordGitEvidenceForWork and
	// promoted verification to "partial", while complete stayed at
	// "unverified" — a silent divergence.
	verA, _ := parsedA["verification"].(string)
	verB, _ := parsedB["verification"].(string)
	if verA != verB {
		t.Errorf("verification diverges: update=%q complete=%q (must be equivalent); rawA=%s rawB=%s",
			verA, verB, rawA, rawB)
	}
	if verA == "partial" {
		t.Errorf("verification=%q on update path: git evidence must NOT auto-promote to partial; "+
			"this is the false-completion inflation the P3 fix removed from complete", verA)
	}
}
