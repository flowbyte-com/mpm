package tools

import (
	"fmt"
	"path/filepath"
	"strings"
	"time"
	"testing"

	"mpm/internal"
)

// newTestDM opens a fresh test DM with a temp DB. Caller must Close().
func newTestSharedDM(t *testing.T) *internal.DatabaseManager {
	t.Helper()
	tmp := t.TempDir()
	t.Setenv("MPM_SHARED_DB", filepath.Join(tmp, "shared.db"))
	dm, err := internal.NewDatabaseManager("")
	if err != nil {
		t.Fatalf("NewDatabaseManager: %v", err)
	}
	t.Cleanup(func() { dm.Close() })
	return dm
}

// TestHandleRecordGlobalRule_RejectsMissingConfirm verifies the
// operator gate: the handler refuses the call when confirm=true is
// not present. Without this gate, agents could autonomously write
// house rules — the original concern from WISHLIST.md Phase 3.
func TestHandleRecordGlobalRule_RejectsMissingConfirm(t *testing.T) {
	dm := newTestSharedDM(t)

	_, err := handleRecordGlobalRule(dm, internal.ActiveContext{}, map[string]interface{}{
		"fact": "test rule",
		// confirm intentionally omitted
	})
	if err == nil {
		t.Fatal("expected error when confirm missing, got nil")
	}
	if !strings.Contains(err.Error(), "confirm=true") {
		t.Errorf("error message should mention confirm=true, got: %v", err)
	}
}

// TestHandleRecordGlobalRule_HappyPath verifies the full round-trip:
// handler accepts confirm=true, writes to shared DB, returns id.
func TestHandleRecordGlobalRule_HappyPath(t *testing.T) {
	dm := newTestSharedDM(t)

	result, err := handleRecordGlobalRule(dm, internal.ActiveContext{}, map[string]interface{}{
		"fact":       "Always quote shell variables",
		"tags":       "shell,safety",
		"weight":     10,
		"provenance": "operator-test",
		"confirm":    true,
	})
	if err != nil {
		t.Fatalf("handleRecordGlobalRule: %v", err)
	}
	m, ok := result.(map[string]interface{})
	if !ok {
		t.Fatalf("expected map, got %T", result)
	}
	if m["id"] == "" || m["id"] == nil {
		t.Errorf("expected non-empty id, got %v", m["id"])
	}
	if m["source"] != "shared" {
		t.Errorf("expected source=shared, got %v", m["source"])
	}
}

// TestHandlePromoteToGlobal_RejectsMissingConfirm verifies the same
// operator gate for promotion.
func TestHandlePromoteToGlobal_RejectsMissingConfirm(t *testing.T) {
	dm := newTestSharedDM(t)

	_, err := handlePromoteToGlobal(dm, internal.ActiveContext{}, map[string]interface{}{
		"memory_id": "any-id",
		// confirm intentionally omitted
	})
	if err == nil {
		t.Fatal("expected error when confirm missing, got nil")
	}
	if !strings.Contains(err.Error(), "confirm=true") {
		t.Errorf("error message should mention confirm=true, got: %v", err)
	}
}

// TestHandlePromoteToGlobal_HappyPath verifies promotion round-trip.
func TestHandlePromoteToGlobal_HappyPath(t *testing.T) {
	dm := newTestSharedDM(t)

	// Insert a local memory to promote. The id is unique-per-run to
	// avoid collision with prior runs (the workspace DB persists
	// across tests).
	localPromoteID := fmt.Sprintf("local-promote-%d", time.Now().UnixNano())
	_, err := dm.SQLDB().Exec(
		`INSERT INTO memories (id, collection, content, weight, deleted_at, created_at, updated_at)
		 VALUES (?, 'memories', 'a tip worth promoting', 7, NULL, '2026-06-26', '2026-06-26')`,
		localPromoteID,
	)
	if err != nil {
		t.Fatalf("insert local: %v", err)
	}

	result, err := handlePromoteToGlobal(dm, internal.ActiveContext{}, map[string]interface{}{
		"memory_id": "local-promote-001",
		"confirm":   true,
	})
	if err != nil {
		t.Fatalf("handlePromoteToGlobal: %v", err)
	}
	m := result.(map[string]interface{})
	if m["local_id"] != "local-promote-001" {
		t.Errorf("local_id mismatch: %v", m["local_id"])
	}
	if m["shared_id"] == "" || m["shared_id"] == nil {
		t.Errorf("shared_id empty: %v", m["shared_id"])
	}
}
