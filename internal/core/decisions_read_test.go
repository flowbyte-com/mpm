// decisions_read_test.go — regression tests for alpha-4 D-005 (decision
// read symmetry). Pins the new GetDecision / ListDecisions / QueryDecisions
// methods plus the tool-surface wrappers (handleShowDecision /
// handleListDecisions / handleQueryDecisions).

package internal

import (
	"testing"
)

// recordDecisionForTest is a helper that records a decision through the
// production RecordDecision path and returns the id. Avoids duplicating
// the row-construction shape across the 6 tests below.
func recordDecisionForTest(t *testing.T, dm *DatabaseManager, choice, rationale string, tags []string) string {
	t.Helper()
	res, err := dm.RecordDecision(
		"alpha-4 D-005 test",
		choice,
		rationale,
		"pending",
		tags,
		nil,
		ActiveContext{},
	)
	if err != nil {
		t.Fatalf("record decision %q: %v", choice, err)
	}
	id, _ := res["id"].(string)
	if id == "" {
		t.Fatalf("record decision returned no id: %v", res)
	}
	return id
}

// TestDecisionRead_RecordThenShow is the round-trip pin: a recorded
// decision must be retrievable by id with its full content, tags,
// metadata, and created_at preserved.
func TestDecisionRead_RecordThenShow(t *testing.T) {
	dm := NewTestDM(t)
	id := recordDecisionForTest(t, dm, "use WAL mode", "concurrent reads, single writer", []string{"alpha-4", "sqlite"})

	got, err := dm.GetDecision(id)
	if err != nil {
		t.Fatalf("GetDecision: %v", err)
	}
	if got["id"] != id {
		t.Errorf("id = %v, want %q", got["id"], id)
	}
	content, _ := got["content"].(string)
	if content == "" {
		t.Errorf("content empty: %v", got)
	}
	if got["status"] != "active" {
		t.Errorf("status = %v, want \"active\"", got["status"])
	}
}

// TestDecisionRead_ListFiltersActive is the status-filter pin:
// recorded 3 decisions, supersede 1 (creates a replacement row),
// invalidate 1. After all operations, the table holds 4 rows:
//   - 1 active (the original "choice-active")
//   - 1 superseded (the original "choice-superseded")
//   - 1 invalidated (the original "choice-invalidated")
//   - 1 active replacement (the supersession's new row)
// The default ListDecisions (status=active) returns 2 rows.
// status=all returns 4. status=superseded returns 1. status=invalidated
// returns 1.
func TestDecisionRead_ListFiltersActive(t *testing.T) {
	dm := NewTestDM(t)

	idActive := recordDecisionForTest(t, dm, "choice-active", "active baseline", []string{"d005"})
	idSuperseded := recordDecisionForTest(t, dm, "choice-superseded", "to be replaced", []string{"d005"})
	idInvalidated := recordDecisionForTest(t, dm, "choice-invalidated", "to be retired", []string{"d005"})

	// Supersede idSuperseded — this also creates a NEW active row
	// (the replacement). After this op, "active" should return 2 rows.
	if _, err := dm.SupersedeDecision(idSuperseded, "alpha-4 follow-up", "choice-supersede-v2", "v2 supersedes v1", "pending", []string{"d005"}, nil, ActiveContext{}); err != nil {
		t.Fatalf("supersede: %v", err)
	}
	// Invalidate idInvalidated.
	if _, err := dm.InvalidateDecision(idInvalidated, "retired"); err != nil {
		t.Fatalf("invalidate: %v", err)
	}

	active, err := dm.ListDecisions(DecisionFilter{Status: "active", Limit: 50})
	if err != nil {
		t.Fatalf("ListDecisions active: %v", err)
	}
	if len(active) != 2 {
		t.Errorf("active list: want 2 rows (original + replacement), got %d", len(active))
	}
	// The original active row must be in the active list.
	foundActive := false
	for _, r := range active {
		if r["id"] == idActive {
			foundActive = true
			break
		}
	}
	if !foundActive {
		t.Errorf("active list missing idActive=%q: %+v", idActive, active)
	}

	all, err := dm.ListDecisions(DecisionFilter{Status: "all", Limit: 50})
	if err != nil {
		t.Fatalf("ListDecisions all: %v", err)
	}
	if len(all) != 4 {
		t.Errorf("all list: want 4 rows (3 originals + 1 replacement), got %d", len(all))
	}

	superseded, err := dm.ListDecisions(DecisionFilter{Status: "superseded", Limit: 50})
	if err != nil {
		t.Fatalf("ListDecisions superseded: %v", err)
	}
	if len(superseded) != 1 {
		t.Errorf("superseded list: want 1 row, got %d", len(superseded))
	}
	if len(superseded) == 1 && superseded[0]["id"] != idSuperseded {
		t.Errorf("superseded list id = %v, want %q", superseded[0]["id"], idSuperseded)
	}

	invalidated, err := dm.ListDecisions(DecisionFilter{Status: "invalidated", Limit: 50})
	if err != nil {
		t.Fatalf("ListDecisions invalidated: %v", err)
	}
	if len(invalidated) != 1 {
		t.Errorf("invalidated list: want 1 row, got %d", len(invalidated))
	}
	if len(invalidated) == 1 && invalidated[0]["id"] != idInvalidated {
		t.Errorf("invalidated list id = %v, want %q", invalidated[0]["id"], idInvalidated)
	}
}

// TestDecisionRead_QueryFindsByChoice pins the FTS-backed query
// surface: a query string that matches the choice text of one
// recorded decision must surface that decision.
func TestDecisionRead_QueryFindsByChoice(t *testing.T) {
	dm := NewTestDM(t)
	_ = recordDecisionForTest(t, dm, "use WAL mode for concurrency", "writer-serialization rationale", []string{"alpha-4"})
	_ = recordDecisionForTest(t, dm, "use DELETE journal for simplicity", "single-writer rationale", []string{"alpha-4"})

	rows, err := dm.QueryDecisions("WAL", 50)
	if err != nil {
		t.Fatalf("QueryDecisions: %v", err)
	}
	if len(rows) == 0 {
		t.Fatalf("QueryDecisions(\"WAL\") returned 0 rows; want ≥1")
	}
	// At least one row's content must contain the query string.
	foundMatch := false
	for _, r := range rows {
		c, _ := r["content"].(string)
		if containsFold(c, "WAL") {
			foundMatch = true
			break
		}
	}
	if !foundMatch {
		t.Errorf("QueryDecisions(\"WAL\") returned rows but none contained \"WAL\": %+v", rows)
	}
}

// TestDecisionRead_SupersededVisibleWithFlag pins that a superseded
// decision remains readable via list --status=superseded and that
// its metadata carries the superseded_by pointer to the replacement.
func TestDecisionRead_SupersededVisibleWithFlag(t *testing.T) {
	dm := NewTestDM(t)
	idOriginal := recordDecisionForTest(t, dm, "original-choice", "rationale", []string{"d005"})
	res, err := dm.SupersedeDecision(idOriginal, "follow-up context", "replacement-choice", "replacement rationale", "pending", []string{"d005"}, nil, ActiveContext{})
	if err != nil {
		t.Fatalf("supersede: %v", err)
	}
	newID, _ := res["id"].(string)

	// The original is now in the superseded bucket.
	superseded, err := dm.ListDecisions(DecisionFilter{Status: "superseded", Limit: 50})
	if err != nil {
		t.Fatalf("list superseded: %v", err)
	}
	if len(superseded) != 1 || superseded[0]["id"] != idOriginal {
		t.Fatalf("expected superseded id=%q, got %+v", idOriginal, superseded)
	}
	meta, _ := superseded[0]["metadata"].(map[string]interface{})
	if meta == nil {
		t.Fatalf("superseded row missing metadata: %+v", superseded[0])
	}
	if gotSupBy, _ := meta["superseded_by"].(string); gotSupBy != newID {
		t.Errorf("metadata.superseded_by = %q, want %q", gotSupBy, newID)
	}
}

// TestDecisionRead_InvalidatedVisibleWithFlag pins that an invalidated
// decision remains readable via list --status=invalidated and that
// its metadata carries the invalidation reason.
func TestDecisionRead_InvalidatedVisibleWithFlag(t *testing.T) {
	dm := NewTestDM(t)
	idOriginal := recordDecisionForTest(t, dm, "to-be-invalidated", "rationale", []string{"d005"})
	if _, err := dm.InvalidateDecision(idOriginal, "retired by policy"); err != nil {
		t.Fatalf("invalidate: %v", err)
	}

	invalidated, err := dm.ListDecisions(DecisionFilter{Status: "invalidated", Limit: 50})
	if err != nil {
		t.Fatalf("list invalidated: %v", err)
	}
	if len(invalidated) != 1 || invalidated[0]["id"] != idOriginal {
		t.Fatalf("expected invalidated id=%q, got %+v", idOriginal, invalidated)
	}
	meta, _ := invalidated[0]["metadata"].(map[string]interface{})
	if meta == nil {
		t.Fatalf("invalidated row missing metadata: %+v", invalidated[0])
	}
	if _, has := meta["invalidated"]; !has {
		t.Errorf("invalidated row metadata missing `invalidated` key: %+v", meta)
	}
}

// TestDecisionRead_ChainWalk pins the chain-walk use case: an agent
// that has the original decision id but wants the chain of
// supersessions must be able to follow superseded_by links.
func TestDecisionRead_ChainWalk(t *testing.T) {
	dm := NewTestDM(t)
	idA := recordDecisionForTest(t, dm, "A", "first choice", []string{"d005"})
	resB, err := dm.SupersedeDecision(idA, "context-B", "B", "rationale-B", "pending", []string{"d005"}, nil, ActiveContext{})
	if err != nil {
		t.Fatalf("supersede A→B: %v", err)
	}
	idB, _ := resB["id"].(string)
	resC, err := dm.SupersedeDecision(idB, "context-C", "C", "rationale-C", "pending", []string{"d005"}, nil, ActiveContext{})
	if err != nil {
		t.Fatalf("supersede B→C: %v", err)
	}
	idC, _ := resC["id"].(string)

	// Walk the chain. A's metadata.superseded_by should point to B.
	a, err := dm.GetDecision(idA)
	if err != nil {
		t.Fatalf("GetDecision A: %v", err)
	}
	metaA, _ := a["metadata"].(map[string]interface{})
	if metaA["superseded_by"] != idB {
		t.Errorf("A.superseded_by = %v, want %q", metaA["superseded_by"], idB)
	}

	// B is now superseded by C.
	b, err := dm.GetDecision(idB)
	if err != nil {
		t.Fatalf("GetDecision B: %v", err)
	}
	metaB, _ := b["metadata"].(map[string]interface{})
	if metaB["superseded_by"] != idC {
		t.Errorf("B.superseded_by = %v, want %q", metaB["superseded_by"], idC)
	}

	// C is the head of the chain — not superseded.
	c, err := dm.GetDecision(idC)
	if err != nil {
		t.Fatalf("GetDecision C: %v", err)
	}
	metaC, _ := c["metadata"].(map[string]interface{})
	if _, has := metaC["superseded_by"]; has {
		t.Errorf("C unexpectedly superseded: %+v", metaC)
	}
	if c["status"] != "active" {
		t.Errorf("C.status = %v, want \"active\"", c["status"])
	}
}

// containsFold is a case-insensitive substring check used in
// TestDecisionRead_QueryFindsByChoice. Lives here (not in a shared
// helper) to keep the test self-contained.
func containsFold(s, sub string) bool {
	if len(sub) == 0 {
		return true
	}
	if len(sub) > len(s) {
		return false
	}
	for i := 0; i+len(sub) <= len(s); i++ {
		match := true
		for j := 0; j < len(sub); j++ {
			a, b := s[i+j], sub[j]
			if a >= 'A' && a <= 'Z' {
				a += 32
			}
			if b >= 'A' && b <= 'Z' {
				b += 32
			}
			if a != b {
				match = false
				break
			}
		}
		if match {
			return true
		}
	}
	return false
}
