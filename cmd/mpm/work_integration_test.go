package main

import (
	"testing"

	"github.com/flowbyte-com/mpm-core"

	"github.com/stretchr/testify/require"
)

// newWorkTestDM creates a hermetic DatabaseManager backed by a temp file,
// isolated from other tests.
func newWorkTestDM(t *testing.T) *internal.DatabaseManager {
	t.Helper()
	tmpDir := t.TempDir()
	t.Setenv("MPM_WORKSPACE", tmpDir)
	dm, err := internal.NewDatabaseManager("")
	if err != nil {
		t.Fatalf("NewDatabaseManager: %v", err)
	}
	if err := dm.InitSchema(); err != nil {
		dm.Close()
		t.Fatalf("InitSchema: %v", err)
	}
	t.Cleanup(func() { dm.Close() })
	return dm
}

func TestWork_CrossSessionPersistence(t *testing.T) {
	tmpDir := t.TempDir()
	t.Setenv("MPM_WORKSPACE", tmpDir)

	// Session A
	dmA, err := internal.NewDatabaseManager("")
	if err != nil {
		t.Fatalf("NewDatabaseManager: %v", err)
	}
	if err := dmA.InitSchema(); err != nil {
		dmA.Close()
		t.Fatalf("InitSchema: %v", err)
	}

	work, err := dmA.AddWork("Investigate telemetry admission", "The telemetry collector is not firing on clean deploy", "")
	if err != nil {
		dmA.Close()
		t.Fatalf("Session A AddWork: %v", err)
	}
	if work.Status != internal.WorkStatusOpen {
		t.Errorf("Session A: status = %v, want open", work.Status)
	}
	dmA.Close()

	// Session B — new DM instance, same MPM_WORKSPACE
	dmB, err := internal.NewDatabaseManager("")
	if err != nil {
		t.Fatalf("NewDatabaseManager: %v", err)
	}
	if err := dmB.InitSchema(); err != nil {
		dmB.Close()
		t.Fatalf("InitSchema: %v", err)
	}

	// Work persists
	retrieved, err := dmB.GetWork(work.ID)
	if err != nil {
		dmB.Close()
		t.Fatalf("Session B GetWork: %v", err)
	}
	if retrieved.Title != "Investigate telemetry admission" {
		t.Errorf("Session B: title = %q, want %q", retrieved.Title, "Investigate telemetry admission")
	}

	// Wake context surfaces it
	ctx, err := dmB.GatherWakeContext()
	if err != nil {
		dmB.Close()
		t.Fatalf("Session B GatherWakeContext: %v", err)
	}
	if len(ctx.OpenWorks) != 1 {
		t.Errorf("Session B: len(OpenWorks) = %d, want 1", len(ctx.OpenWorks))
	}
	if ctx.OpenWorks[0].ID != work.ID {
		t.Errorf("Session B: OpenWorks[0].ID = %q, want %q", ctx.OpenWorks[0].ID, work.ID)
	}

	// Complete from Session B
	completed, err := dmB.CompleteWork(work.ID)
	if err != nil {
		dmB.Close()
		t.Fatalf("Session B CompleteWork: %v", err)
	}
	if completed.Status != internal.WorkStatusDone {
		t.Errorf("Session B: status = %v, want done", completed.Status)
	}

	// No longer in open works
	ctx2, _ := dmB.GatherWakeContext()
	if len(ctx2.OpenWorks) != 0 {
		t.Errorf("Session B after complete: len(OpenWorks) = %d, want 0", len(ctx2.OpenWorks))
	}

	dmB.Close()
}

func TestWork_NoSilentDisappearance(t *testing.T) {
	dm := newWorkTestDM(t)

	work, err := dm.AddWork("Long-term investigation", "This might take weeks", "")
	if err != nil {
		t.Fatalf("AddWork: %v", err)
	}

	// Work should still be there (no TTL/decay on works)
	retrieved, err := dm.GetWork(work.ID)
	if err != nil {
		t.Fatalf("GetWork: %v", err)
	}
	if retrieved.Status != internal.WorkStatusOpen {
		t.Errorf("Status = %v, want open (no TTL)", retrieved.Status)
	}

	// List should still return it
	works, err := dm.ListWorks()
	if err != nil {
		t.Fatalf("ListWorks: %v", err)
	}
	found := false
	for _, w := range works {
		if w.ID == work.ID {
			found = true
			break
		}
	}
	require.True(t, found, "Work should still be in list after time passes")
}

func TestWork_CancelAndReopen(t *testing.T) {
	dm := newWorkTestDM(t)

	work, err := dm.AddWork("Maybe later", "", "")
	if err != nil {
		t.Fatalf("AddWork: %v", err)
	}

	cancelled, err := dm.CancelWork(work.ID)
	if err != nil {
		t.Fatalf("CancelWork: %v", err)
	}
	if cancelled.Status != internal.WorkStatusCancelled {
		t.Errorf("CancelWork: status = %v, want cancelled", cancelled.Status)
	}

	// Cancelled work is NOT in list
	works, err := dm.ListWorks()
	if err != nil {
		t.Fatalf("ListWorks: %v", err)
	}
	if len(works) != 0 {
		t.Errorf("ListWorks after cancel: len = %d, want 0", len(works))
	}

	// Can re-open
	reopened, err := dm.UpdateWork(work.ID, internal.WorkStatusOpen)
	if err != nil {
		t.Fatalf("UpdateWork reopen: %v", err)
	}
	if reopened.Status != internal.WorkStatusOpen {
		t.Errorf("Reopen: status = %v, want open", reopened.Status)
	}

	// Back in list
	works, err = dm.ListWorks()
	if err != nil {
		t.Fatalf("ListWorks after reopen: %v", err)
	}
	if len(works) != 1 {
		t.Errorf("ListWorks after reopen: len = %d, want 1", len(works))
	}
}
