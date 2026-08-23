package internal

import "testing"

func TestWorkStatusValues(t *testing.T) {
	if WorkStatusOpen != "open" {
		t.Errorf("WorkStatusOpen = %q, want 'open'", WorkStatusOpen)
	}
	if WorkStatusDone != "done" {
		t.Errorf("WorkStatusDone = %q, want 'done'", WorkStatusDone)
	}
	if WorkStatusCancelled != "cancelled" {
		t.Errorf("WorkStatusCancelled = %q, want 'cancelled'", WorkStatusCancelled)
	}
}

func TestWorkStruct(t *testing.T) {
	w := Work{
		ID:      "test-work-001",
		Title:   "Investigate telemetry admission",
		Content: "",
		Status:  WorkStatusOpen,
	}
	if w.Title != "Investigate telemetry admission" {
		t.Errorf("Title = %q, want 'Investigate telemetry admission'", w.Title)
	}
	if w.Status != WorkStatusOpen {
		t.Errorf("Status = %v, want WorkStatusOpen", w.Status)
	}
}

func TestWorkStatusConstants(t *testing.T) {
	if string(WorkStatusOpen) != "open" {
		t.Errorf("WorkStatusOpen string = %q, want 'open'", string(WorkStatusOpen))
	}
	if string(WorkStatusDone) != "done" {
		t.Errorf("WorkStatusDone string = %q, want 'done'", string(WorkStatusDone))
	}
	if string(WorkStatusCancelled) != "cancelled" {
		t.Errorf("WorkStatusCancelled string = %q, want 'cancelled'", string(WorkStatusCancelled))
	}
}
