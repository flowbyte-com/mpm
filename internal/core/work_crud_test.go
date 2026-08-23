package internal

import (
	"testing"
)

func TestAddWork(t *testing.T) {
	dm := NewTestDM(t)
	defer dm.Close()

	work, err := dm.AddWork("Investigate telemetry admission", "", "")
	if err != nil {
		t.Fatalf("AddWork: %v", err)
	}
	if work.Title != "Investigate telemetry admission" {
		t.Errorf("Title = %q, want %q", work.Title, "Investigate telemetry admission")
	}
	if work.Status != WorkStatusOpen {
		t.Errorf("Status = %v, want WorkStatusOpen", work.Status)
	}
	if work.ID == "" {
		t.Error("ID should not be empty")
	}
	if work.CreatedAt == 0 {
		t.Error("CreatedAt should be set")
	}
}

func TestAddWork_WithContent(t *testing.T) {
	dm := NewTestDM(t)
	defer dm.Close()

	work, err := dm.AddWork("Fix the bug", "The telemetry collector crashes on SIGTERM", "")
	if err != nil {
		t.Fatalf("AddWork: %v", err)
	}
	if work.Content != "The telemetry collector crashes on SIGTERM" {
		t.Errorf("Content = %q, want %q", work.Content, "The telemetry collector crashes on SIGTERM")
	}
}

func TestAddWork_SessionID(t *testing.T) {
	dm := NewTestDM(t)
	defer dm.Close()

	work, err := dm.AddWork("Test work", "", "session-abc-123")
	if err != nil {
		t.Fatalf("AddWork: %v", err)
	}
	if work.SessionID != "session-abc-123" {
		t.Errorf("SessionID = %q, want %q", work.SessionID, "session-abc-123")
	}
}

func TestGetWork(t *testing.T) {
	dm := NewTestDM(t)
	defer dm.Close()

	created, _ := dm.AddWork("Test work", "", "")
	retrieved, err := dm.GetWork(created.ID)
	if err != nil {
		t.Fatalf("GetWork: %v", err)
	}
	if retrieved.Title != created.Title {
		t.Errorf("Title = %q, want %q", retrieved.Title, created.Title)
	}
}

func TestGetWork_NotFound(t *testing.T) {
	dm := NewTestDM(t)
	defer dm.Close()

	_, err := dm.GetWork("does-not-exist")
	if err == nil {
		t.Error("GetWork should error for missing ID")
	}
}

func TestListWorks(t *testing.T) {
	dm := NewTestDM(t)
	defer dm.Close()

	dm.AddWork("Work 1", "", "")
	dm.AddWork("Work 2", "", "")

	works, err := dm.ListWorks()
	if err != nil {
		t.Fatalf("ListWorks: %v", err)
	}
	if len(works) != 2 {
		t.Errorf("len(works) = %d, want 2", len(works))
	}
}

func TestListWorks_OnlyOpen(t *testing.T) {
	dm := NewTestDM(t)
	defer dm.Close()

	w1, _ := dm.AddWork("Open work", "", "")
	dm.AddWork("Done work", "", "")
	dm.CompleteWork(w1.ID)

	works, err := dm.ListWorks()
	if err != nil {
		t.Fatalf("ListWorks: %v", err)
	}
	if len(works) != 1 {
		t.Errorf("len(works) = %d, want 1 (done work filtered out)", len(works))
	}
	if works[0].Title != "Done work" {
		t.Errorf("Title = %q, want 'Done work' (added after Open work was completed, appears first due to ORDER BY created_at DESC)", works[0].Title)
	}
}

func TestCompleteWork(t *testing.T) {
	dm := NewTestDM(t)
	defer dm.Close()

	work, _ := dm.AddWork("Test work", "", "")
	completed, err := dm.CompleteWork(work.ID)
	if err != nil {
		t.Fatalf("CompleteWork: %v", err)
	}
	if completed.Status != WorkStatusDone {
		t.Errorf("Status = %v, want WorkStatusDone", completed.Status)
	}
	if completed.CompletedAt == nil {
		t.Error("CompletedAt should be set")
	}
}

func TestCancelWork(t *testing.T) {
	dm := NewTestDM(t)
	defer dm.Close()

	work, _ := dm.AddWork("Test work", "", "")
	cancelled, err := dm.CancelWork(work.ID)
	if err != nil {
		t.Fatalf("CancelWork: %v", err)
	}
	if cancelled.Status != WorkStatusCancelled {
		t.Errorf("Status = %v, want WorkStatusCancelled", cancelled.Status)
	}
}

func TestReopenWork(t *testing.T) {
	dm := NewTestDM(t)
	defer dm.Close()

	work, _ := dm.AddWork("Test work", "", "")
	dm.CompleteWork(work.ID)

	reopened, err := dm.UpdateWork(work.ID, WorkStatusOpen)
	if err != nil {
		t.Fatalf("UpdateWork: %v", err)
	}
	if reopened.Status != WorkStatusOpen {
		t.Errorf("Status = %v, want WorkStatusOpen", reopened.Status)
	}
	if reopened.CompletedAt != nil {
		t.Error("CompletedAt should be cleared on re-open")
	}
}
