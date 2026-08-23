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
