package tools

import (
	"database/sql"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

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

// TestHandleMpmWork_ProvenanceOnCreate verifies that creating a work via
// handleCreateWork results in an artifact_provenance row with artifact_type='work'.
// This is the integration test for Task 4 provenance integration.
func TestHandleMpmWork_ProvenanceOnCreate(t *testing.T) {
	dm := newTestSharedDM(t)

	result, err := handleMpmWork(dm, internal.ActiveContext{Agent: "test-agent", SessionID: "test-session"}, map[string]interface{}{
		"action": "create",
		"params": map[string]interface{}{
			"title":   "Provenance test work",
			"content": "testing artifact_provenance integration",
		},
	})
	if err != nil {
		t.Fatalf("handleMpmWork create: %v", err)
	}
	workID := result.(map[string]interface{})["id"].(string)

	// Query artifact_provenance directly for this work.
	var provCount int
	if err := dm.SQLDB().QueryRow(
		`SELECT COUNT(*) FROM artifact_provenance WHERE artifact_id = ? AND artifact_type = 'work'`,
		workID,
	).Scan(&provCount); err != nil {
		t.Fatalf("query artifact_provenance: %v", err)
	}
	if provCount == 0 {
		t.Fatal("expected a row in artifact_provenance for newly created work")
	}

	// Verify actor_kind is captured (should be 'agent' from ActiveContext).
	var actorKind string
	if err := dm.SQLDB().QueryRow(
		`SELECT actor_kind FROM artifact_provenance WHERE artifact_id = ? AND artifact_type = 'work'`,
		workID,
	).Scan(&actorKind); err != nil {
		t.Fatalf("query actor_kind: %v", err)
	}
	if actorKind != "agent" {
		t.Errorf("actor_kind = %q, want %q", actorKind, "agent")
	}
}

// ---------------------------------------------------------------------------
// Task 5 integration tests (Section 14 adversarial tests)
// ---------------------------------------------------------------------------
// Coverage of Tasks 1-3 and 5 from the task brief:
// - Task 1 (history returns events in order):        covered by TestHandleMpmWork_History
// - Task 2 (note appends note_appended event):       covered by TestHandleMpmWork_Note
// - Task 3 (reopen clears completed_at, appends):   covered by TestHandleMpmWork_Reopen
// - Task 5 (provenance row at creation):              covered by TestHandleMpmWork_ProvenanceOnCreate
// Only the adversarial concurrent-append test (Task 4) is new below.

// TestHandleMpmWork_ConcurrentAppends_OneWinsConstraintError is the adversarial
// integration test from Section 14. Two independent *sql.DB connections both
// call AppendWorkEvent for the same work_id simultaneously. The UNIQUE(work_id,
// event_index) constraint ensures exactly one succeeds.
//
// We use two separate *sql.DB instances with a shared-cache in-memory database
// because Go's sql.DB pool serializes access — two connections bypass that and
// produce genuine SQLite-level contention.
//
// The in-memory shared-cache approach produces a "table is locked" error on the
// SELECT (SQLITE_LOCKED) when the second goroutine arrives while the first holds
// the write lock. The file-based approach (WAL) can allow both goroutines'
// SELECTs to succeed concurrently, computing the same event_index; the second
// INSERT then hits the UNIQUE constraint. Both are valid adversarial shapes.
// The test accepts both: the invariant is that exactly one succeeds.
func TestHandleMpmWork_ConcurrentAppends_OneWinsConstraintError(t *testing.T) {
	// Shared-cache in-memory DB so both connections see the same database.
	// The file: prefix with mode=memory&cache=shared creates a shared
	// in-memory database accessible by multiple connections.
	dsn := fmt.Sprintf("file:mpm-concurrent-%d?mode=memory&cache=shared", time.Now().UnixNano())

	db1, err := sql.Open("sqlite3", dsn)
	if err != nil {
		t.Fatalf("open db1: %v", err)
	}
	defer db1.Close()
	db2, err := sql.Open("sqlite3", dsn)
	if err != nil {
		t.Fatalf("open db2: %v", err)
	}
	defer db2.Close()

	// Establish the shared DB before InitSchema runs.
	if _, err := db1.Exec("SELECT 1"); err != nil {
		t.Fatalf("establish shared db: %v", err)
	}

	dm1 := internal.NewDatabaseManagerForDB(db1)
	if err := dm1.InitSchema(); err != nil {
		t.Fatalf("init schema on db1: %v", err)
	}
	dm2 := internal.NewDatabaseManagerForDB(db2)
	if err := dm2.InitSchema(); err != nil {
		t.Fatalf("init schema on db2: %v", err)
	}

	// Create a work via dm1 so it has event_index=0 (created).
	w, err := handleMpmWork(dm1, internal.ActiveContext{}, map[string]interface{}{
		"action": "create",
		"params": map[string]interface{}{
			"title": "Concurrent append target",
		},
	})
	if err != nil {
		t.Fatalf("create work: %v", err)
	}
	workID := w.(map[string]interface{})["id"].(string)

	// Rendezvous so both goroutines truly start simultaneously.
	startCh := make(chan struct{})
	errCh := make(chan error, 2)
	var wg sync.WaitGroup
	wg.Add(2)

	for _, dm := range []*internal.DatabaseManager{dm1, dm2} {
		go func(dbm *internal.DatabaseManager) {
			defer wg.Done()
			<-startCh // block until released; both goroutines release together

			err := dbm.WithTx(func(node internal.DBNode) error {
				_, err := dbm.AppendWorkEvent(workID, internal.WorkEvent{
					EventType: internal.WorkEventTypeNoteAppended,
					Note:      "concurrent note attempt",
				}, nil, node)
				return err
			})
			errCh <- err
		}(dm)
	}

	close(startCh) // fire both simultaneously
	wg.Wait()
	close(errCh)

	var errs []error
	for e := range errCh {
		if e != nil {
			errs = append(errs, e)
		}
	}

	// Exactly one must fail; the other must succeed.
	if len(errs) == 0 {
		t.Fatal("expected exactly one goroutine to fail, but both succeeded")
	}
	if len(errs) == 2 {
		t.Fatal("expected exactly one goroutine to succeed, but both failed")
	}

	// The failure must be a SQLite adversarial error: either UNIQUE (both
	// computed same index and second INSERT was evaluated) or LOCKED (second
	// arrived while first held the write lock). Both are correct outcomes;
	// both prove the DB serializes concurrent writers.
	if len(errs) == 1 {
		errStr := errs[0].Error()
		isCorrectErr := strings.Contains(errStr, "UNIQUE") ||
			strings.Contains(errStr, "constraint") ||
			strings.Contains(errStr, "locked")
		if !isCorrectErr {
			t.Errorf("expected UNIQUE/locked/constraint error, got: %v", errs[0])
		}
	}

	// Verify exactly 2 events exist (created + one winner's note_appended).
	events, err := dm1.GetWorkEvents(workID)
	if err != nil {
		t.Fatalf("GetWorkEvents: %v", err)
	}
	if len(events) != 2 {
		t.Errorf("expected exactly 2 events (created + one note_appended), got %d", len(events))
	}
	if len(events) == 2 && events[1].EventType != internal.WorkEventTypeNoteAppended {
		t.Errorf("surviving event type = %q, want %q", events[1].EventType, internal.WorkEventTypeNoteAppended)
	}
}
