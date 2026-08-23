package tools

import (
	"testing"

	"github.com/flowbyte-com/mpm-core"
)

// workStatus extracts the WorkStatus value from a map as a plain string.
// The map value is internal.WorkStatus which != string, so direct
// comparison fails in Go even though the underlying string is equal.
func workStatus(m map[string]interface{}, key string) string {
	if v, ok := m[key].(internal.WorkStatus); ok {
		return string(v)
	}
	if v, ok := m[key].(string); ok {
		return v
	}
	return ""
}

// typeAssertDBM converts a CoreDB interface to *DatabaseManager.
// Used by tests that need to call methods not in the CoreDB interface
// (e.g., AddWork in test setup).
func typeAssertDBM(dm internal.CoreDB) *internal.DatabaseManager {
	return dm.(*internal.DatabaseManager)
}

func TestHandleMpmWork_Create(t *testing.T) {
	dm := newTestSharedDM(t)

	result, err := handleMpmWork(dm, internal.ActiveContext{}, map[string]interface{}{
		"action": "create",
		"params": map[string]interface{}{
			"title": "Investigate telemetry admission",
		},
	})
	if err != nil {
		t.Fatalf("handleMpmWork create: %v", err)
	}
	m := result.(map[string]interface{})
	if m["title"] != "Investigate telemetry admission" {
		t.Errorf("title = %q, want %q", m["title"], "Investigate telemetry admission")
	}
	if workStatus(m, "status") != "open" {
		t.Errorf("status = %q, want %q", workStatus(m, "status"), "open")
	}
}

func TestHandleMpmWork_List(t *testing.T) {
	dm := newTestSharedDM(t)

	typeAssertDBM(dm).AddWork("Work 1", "", "")
	typeAssertDBM(dm).AddWork("Work 2", "", "")

	result, err := handleMpmWork(dm, internal.ActiveContext{}, map[string]interface{}{
		"action": "list",
		"params": map[string]interface{}{},
	})
	if err != nil {
		t.Fatalf("handleMpmWork list: %v", err)
	}
	list := result.([]map[string]interface{})
	if len(list) != 2 {
		t.Errorf("len(list) = %d, want 2", len(list))
	}
}

func TestHandleMpmWork_Complete(t *testing.T) {
	dm := newTestSharedDM(t)

	w, _ := typeAssertDBM(dm).AddWork("Test work", "", "")

	result, err := handleMpmWork(dm, internal.ActiveContext{}, map[string]interface{}{
		"action": "complete",
		"params": map[string]interface{}{
			"work_id": w.ID,
		},
	})
	if err != nil {
		t.Fatalf("handleMpmWork complete: %v", err)
	}
	m := result.(map[string]interface{})
	if workStatus(m, "status") != "done" {
		t.Errorf("status = %q, want %q", workStatus(m, "status"), "done")
	}
}

func TestHandleMpmWork_Cancel(t *testing.T) {
	dm := newTestSharedDM(t)

	w, _ := typeAssertDBM(dm).AddWork("Cancel me", "", "")

	result, err := handleMpmWork(dm, internal.ActiveContext{}, map[string]interface{}{
		"action": "cancel",
		"params": map[string]interface{}{
			"work_id": w.ID,
		},
	})
	if err != nil {
		t.Fatalf("handleMpmWork cancel: %v", err)
	}
	m := result.(map[string]interface{})
	if workStatus(m, "status") != "cancelled" {
		t.Errorf("status = %q, want %q", workStatus(m, "status"), "cancelled")
	}
}

func TestHandleMpmWork_Update(t *testing.T) {
	dm := newTestSharedDM(t)

	w, _ := typeAssertDBM(dm).AddWork("Update me", "", "")

	result, err := handleMpmWork(dm, internal.ActiveContext{}, map[string]interface{}{
		"action": "update",
		"params": map[string]interface{}{
			"work_id": w.ID,
			"status":  "done",
		},
	})
	if err != nil {
		t.Fatalf("handleMpmWork update: %v", err)
	}
	m := result.(map[string]interface{})
	if workStatus(m, "status") != "done" {
		t.Errorf("status = %q, want %q", workStatus(m, "status"), "done")
	}
}

func TestHandleMpmWork_Show(t *testing.T) {
	dm := newTestSharedDM(t)

	w, _ := typeAssertDBM(dm).AddWork("Show me", "some content", "")

	result, err := handleMpmWork(dm, internal.ActiveContext{}, map[string]interface{}{
		"action": "show",
		"params": map[string]interface{}{
			"work_id": w.ID,
		},
	})
	if err != nil {
		t.Fatalf("handleMpmWork show: %v", err)
	}
	m := result.(map[string]interface{})
	if m["id"] != w.ID {
		t.Errorf("id = %q, want %q", m["id"], w.ID)
	}
	if m["title"] != "Show me" {
		t.Errorf("title = %q, want %q", m["title"], "Show me")
	}
	if m["content"] != "some content" {
		t.Errorf("content = %q, want %q", m["content"], "some content")
	}
}

func TestHandleMpmWork_UnknownAction(t *testing.T) {
	dm := newTestSharedDM(t)

	_, err := handleMpmWork(dm, internal.ActiveContext{}, map[string]interface{}{
		"action": "bogus",
		"params": map[string]interface{}{},
	})
	if err == nil {
		t.Fatal("expected error for unknown action, got nil")
	}
}

func TestHandleMpmWork_MissingTitle(t *testing.T) {
	dm := newTestSharedDM(t)

	_, err := handleMpmWork(dm, internal.ActiveContext{}, map[string]interface{}{
		"action": "create",
		"params": map[string]interface{}{},
	})
	if err == nil {
		t.Fatal("expected error for missing title, got nil")
	}
}

func TestHandleMpmWork_MissingWorkID(t *testing.T) {
	dm := newTestSharedDM(t)

	_, err := handleMpmWork(dm, internal.ActiveContext{}, map[string]interface{}{
		"action": "show",
		"params": map[string]interface{}{},
	})
	if err == nil {
		t.Fatal("expected error for missing work_id, got nil")
	}
}

func TestHandleMpmWork_History(t *testing.T) {
	dm := newTestSharedDM(t)

	// Create a work via the handler (so it has a created event).
	w, err := handleMpmWork(dm, internal.ActiveContext{}, map[string]interface{}{
		"action": "create",
		"params": map[string]interface{}{
			"title":   "History test work",
			"content": "initial content",
		},
	})
	if err != nil {
		t.Fatalf("create work: %v", err)
	}
	workID := w.(map[string]interface{})["id"].(string)

	// Complete the work.
	_, err = handleMpmWork(dm, internal.ActiveContext{}, map[string]interface{}{
		"action": "complete",
		"params": map[string]interface{}{
			"work_id": workID,
			"note":    "Done with this",
		},
	})
	if err != nil {
		t.Fatalf("complete work: %v", err)
	}

	// Get history.
	result, err := handleMpmWork(dm, internal.ActiveContext{}, map[string]interface{}{
		"action": "history",
		"params": map[string]interface{}{
			"work_id": workID,
		},
	})
	if err != nil {
		t.Fatalf("history: %v", err)
	}
	events := result.([]map[string]interface{})
	if len(events) != 2 {
		t.Fatalf("len(events) = %d, want 2", len(events))
	}
	// First event is created (event_index 0).
	if events[0]["event_index"].(int) != 0 {
		t.Errorf("events[0] event_index = %v, want 0", events[0]["event_index"])
	}
	// Second event is completed (event_index 1).
	if events[1]["event_index"].(int) != 1 {
		t.Errorf("events[1] event_index = %v, want 1", events[1]["event_index"])
	}
}

func TestHandleMpmWork_Note(t *testing.T) {
	dm := newTestSharedDM(t)

	w, err := handleMpmWork(dm, internal.ActiveContext{}, map[string]interface{}{
		"action": "create",
		"params": map[string]interface{}{
			"title": "Note test work",
		},
	})
	if err != nil {
		t.Fatalf("create work: %v", err)
	}
	workID := w.(map[string]interface{})["id"].(string)

	result, err := handleMpmWork(dm, internal.ActiveContext{}, map[string]interface{}{
		"action": "note",
		"params": map[string]interface{}{
			"work_id": workID,
			"note":    "This is a contextual note",
		},
	})
	if err != nil {
		t.Fatalf("note: %v", err)
	}
	event := result.(map[string]interface{})
	// Note event should have event_index 1 (0 was created).
	if event["event_index"].(int) != 1 {
		t.Errorf("event_index = %v, want 1", event["event_index"])
	}
	if event["note"] != "This is a contextual note" {
		t.Errorf("note = %q, want %q", event["note"], "This is a contextual note")
	}
}

func TestHandleMpmWork_Note_MissingNote(t *testing.T) {
	dm := newTestSharedDM(t)

	w, err := handleMpmWork(dm, internal.ActiveContext{}, map[string]interface{}{
		"action": "create",
		"params": map[string]interface{}{
			"title": "Note missing note",
		},
	})
	if err != nil {
		t.Fatalf("create work: %v", err)
	}
	workID := w.(map[string]interface{})["id"].(string)

	_, err = handleMpmWork(dm, internal.ActiveContext{}, map[string]interface{}{
		"action": "note",
		"params": map[string]interface{}{
			"work_id": workID,
			// note intentionally missing
		},
	})
	if err == nil {
		t.Fatal("expected error for missing note, got nil")
	}
}

func TestHandleMpmWork_Reopen(t *testing.T) {
	dm := newTestSharedDM(t)

	w, err := handleMpmWork(dm, internal.ActiveContext{}, map[string]interface{}{
		"action": "create",
		"params": map[string]interface{}{
			"title": "Reopen test work",
		},
	})
	if err != nil {
		t.Fatalf("create work: %v", err)
	}
	workID := w.(map[string]interface{})["id"].(string)

	// Complete it.
	_, err = handleMpmWork(dm, internal.ActiveContext{}, map[string]interface{}{
		"action": "complete",
		"params": map[string]interface{}{
			"work_id": workID,
		},
	})
	if err != nil {
		t.Fatalf("complete: %v", err)
	}

	// Reopen it.
	result, err := handleMpmWork(dm, internal.ActiveContext{}, map[string]interface{}{
		"action": "reopen",
		"params": map[string]interface{}{
			"work_id": workID,
		},
	})
	if err != nil {
		t.Fatalf("reopen: %v", err)
	}
	m := result.(map[string]interface{})
	if workStatus(m, "status") != "open" {
		t.Errorf("status = %q, want open after reopen", workStatus(m, "status"))
	}

	// History should show 3 events: created, completed, reopened.
	histResult, err := handleMpmWork(dm, internal.ActiveContext{}, map[string]interface{}{
		"action": "history",
		"params": map[string]interface{}{
			"work_id": workID,
		},
	})
	if err != nil {
		t.Fatalf("history: %v", err)
	}
	events := histResult.([]map[string]interface{})
	if len(events) != 3 {
		t.Fatalf("len(events) = %d, want 3", len(events))
	}
	// Third event should have event_index 2 (0=created, 1=completed, 2=reopened).
	if events[2]["event_index"].(int) != 2 {
		t.Errorf("events[2] event_index = %v, want 2", events[2]["event_index"])
	}
}

func TestHandleMpmWork_History_MissingWorkID(t *testing.T) {
	dm := newTestSharedDM(t)

	_, err := handleMpmWork(dm, internal.ActiveContext{}, map[string]interface{}{
		"action": "history",
		"params": map[string]interface{}{},
	})
	if err == nil {
		t.Fatal("expected error for missing work_id, got nil")
	}
}

func TestHandleMpmWork_Reopen_MissingWorkID(t *testing.T) {
	dm := newTestSharedDM(t)

	_, err := handleMpmWork(dm, internal.ActiveContext{}, map[string]interface{}{
		"action": "reopen",
		"params": map[string]interface{}{},
	})
	if err == nil {
		t.Fatal("expected error for missing work_id, got nil")
	}
}
