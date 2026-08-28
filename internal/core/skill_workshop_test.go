package internal

import (
	"context"
	"encoding/json"
	"fmt"
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
