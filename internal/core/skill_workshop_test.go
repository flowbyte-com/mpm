package internal

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"testing"
	"time"
)

func TestWorkshopClaimOrWait_FirstWriterWins(t *testing.T) {
	_ = NewTestDM(t)
	key := "test-key-" + fmt.Sprint(time.Now().UnixNano())

	// First claim — should be the writer.
	entry, isWriter, err := claimOrWait(context.Background(), key)
	if err != nil {
		t.Fatalf("first claimOrWait: %v", err)
	}
	if !isWriter {
		t.Fatal("first caller should be the writer")
	}

	// Publish the result and unblock any concurrent waiters.
	publishResult(entry, json.RawMessage(`{"outcome":"published"}`), nil)

	// Second claim with same key — entry.done is already closed,
	// so this should return immediately (isWriter=false).
	entry2, isWriter2, err := claimOrWait(context.Background(), key)
	if err != nil {
		t.Fatalf("second claimOrWait: %v", err)
	}
	if isWriter2 {
		t.Fatal("second caller should NOT be the writer")
	}
	if entry != entry2 {
		t.Fatal("second caller should observe the same entry as first")
	}

	// Verify the result was propagated.
	if string(entry2.response) != `{"outcome":"published"}` {
		t.Errorf("entry2.response = %q, want published", string(entry2.response))
	}
}

func TestValidateInput_ExceedsSizeLimits(t *testing.T) {
	huge := strings.Repeat("x", 50*1024+1) // task_context max is 50KB
	req := &WorkshopRequest{
		Mode: "form",
		Proposal: SkillProposal{Name: "foo", Version: "1.0.0"},
		TaskContext: huge,
	}
	_, _, err := validateInput(req)
	if err == nil {
		t.Fatal("expected error for oversized task_context, got nil")
	}
}

func TestValidateInput_TooManyMemoryIDs(t *testing.T) {
	ids := make([]string, 11)
	for i := range ids {
		ids[i] = fmt.Sprintf("mem-%d", i)
	}
	req := &WorkshopRequest{
		Mode: "form",
		Proposal: SkillProposal{Name: "foo", Version: "1.0.0"},
		Evidence: WorkshopEvidence{MemoryIDs: ids},
	}
	_, _, err := validateInput(req)
	if err == nil {
		t.Fatal("expected error for >10 memory_ids, got nil")
	}
}

func TestValidateInput_CleanRequest(t *testing.T) {
	req := &WorkshopRequest{
		Mode: "form",
		DecisionModel: DecisionModel{Reusability: 2, NonObviousness: 2, Stability: 2, Leverage: 2, Boundary: "procedure"},
		Proposal: SkillProposal{Name: "foo", Version: "1.0.0", WhenToUse: "doing a thing"},
		TaskContext: "small context",
	}
	_, _, err := validateInput(req)
	if err != nil {
		t.Fatalf("clean request: %v", err)
	}
}
