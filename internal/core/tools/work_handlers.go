package tools

import (
	"fmt"

	mpminternal "github.com/flowbyte-com/mpm-core"
)

// handleMpmWork dispatches work actions: create, list, show, update, complete, cancel,
// and the new v2 event-sourced actions: history, note, reopen.
func handleMpmWork(dm mpminternal.CoreDB, ac mpminternal.ActiveContext, payload map[string]interface{}) (interface{}, error) {
	params, err := extractParamsOrFail("mpm_work", payload)
	if err != nil {
		return nil, err
	}
	action, _ := payload["action"].(string)

	switch action {
	case "create":
		return handleCreateWork(dm, ac, params)
	case "list":
		return handleListWorks(dm, params)
	case "show":
		return handleShowWork(dm, params)
	case "update":
		return handleUpdateWork(dm, ac, params)
	case "complete":
		return handleCompleteWork(dm, ac, params)
	case "cancel":
		return handleCancelWork(dm, ac, params)
	case "history":
		return handleHistoryWork(dm, params)
	case "note":
		return handleNoteWork(dm, ac, params)
	case "reopen":
		return handleReopenWork(dm, ac, params)
	default:
		return nil, fmt.Errorf("unknown action %q for mpm_work. Valid actions include create, list, show, update, complete, cancel, history, note, reopen", action)
	}
}

func handleCreateWork(dm mpminternal.CoreDB, ac mpminternal.ActiveContext, p map[string]interface{}) (interface{}, error) {
	title, _ := p["title"].(string)
	if title == "" {
		return nil, fmt.Errorf("title is required for create")
	}
	content, _ := p["content"].(string)
	sessionID, _ := p["session_id"].(string)
	w, err := dm.CreateWorkWithContext(title, content, sessionID, ac)
	if err != nil {
		return nil, err
	}
	return workToMapWork(w), nil
}

func handleListWorks(dm mpminternal.CoreDB, p map[string]interface{}) (interface{}, error) {
	works, err := dm.ListWorks()
	if err != nil {
		return nil, err
	}
	result := make([]map[string]interface{}, len(works))
	for i, w := range works {
		result[i] = workToMapWork(w)
	}
	return result, nil
}

func handleShowWork(dm mpminternal.CoreDB, p map[string]interface{}) (interface{}, error) {
	workID, _ := p["work_id"].(string)
	if workID == "" {
		return nil, fmt.Errorf("work_id is required for show")
	}
	w, err := dm.GetWork(workID)
	if err != nil {
		return nil, err
	}
	return workToMapWork(w), nil
}

// Thin handlers: parse payload, delegate to db.go event-sourced API.
// No UPDATE works queries, no git logic, no provenance duplication.

func handleUpdateWork(dm mpminternal.CoreDB, ac mpminternal.ActiveContext, p map[string]interface{}) (interface{}, error) {
	workID, _ := p["work_id"].(string)
	statusStr, _ := p["status"].(string)
	title, _ := p["title"].(string)
	content, _ := p["content"].(string)
	w, err := dm.UpdateWorkWithContext(workID, title, content, statusStr, ac)
	if err != nil {
		return nil, err
	}
	return workToMapWork(w), nil
}

func handleCompleteWork(dm mpminternal.CoreDB, ac mpminternal.ActiveContext, p map[string]interface{}) (interface{}, error) {
	workID, _ := p["work_id"].(string)
	note, _ := p["note"].(string)
	w, err := dm.CompleteWorkWithContext(workID, note, ac)
	if err != nil {
		return nil, err
	}
	// Explicit evidence route: separate write to evidence table (not WorkEvent)
	dm.RecordGitEvidenceForWork(workID)
	return workToMapWork(w), nil
}

func handleCancelWork(dm mpminternal.CoreDB, ac mpminternal.ActiveContext, p map[string]interface{}) (interface{}, error) {
	workID, _ := p["work_id"].(string)
	note, _ := p["note"].(string)
	w, err := dm.CancelWorkWithContext(workID, note, ac)
	if err != nil {
		return nil, err
	}
	// Explicit evidence route for cancellation as well
	dm.RecordGitEvidenceForWork(workID)
	return workToMapWork(w), nil
}

// handleHistoryWork returns the full event history for a work item,
// ordered by event_index ASC.
func handleHistoryWork(dm mpminternal.CoreDB, p map[string]interface{}) (interface{}, error) {
	workID, _ := p["work_id"].(string)
	if workID == "" {
		return nil, fmt.Errorf("work_id is required for history")
	}

	events, err := dm.GetWorkEvents(workID)
	if err != nil {
		return nil, fmt.Errorf("get work events: %w", err)
	}

	result := make([]map[string]interface{}, len(events))
	for i, e := range events {
		result[i] = workEventToMap(e)
	}
	return result, nil
}

func handleNoteWork(dm mpminternal.CoreDB, ac mpminternal.ActiveContext, p map[string]interface{}) (interface{}, error) {
	workID, _ := p["work_id"].(string)
	note, _ := p["note"].(string)
	ev, err := dm.AddWorkNoteWithContext(workID, note, ac)
	if err != nil {
		return nil, err
	}
	return workEventToMap(ev), nil
}

func handleReopenWork(dm mpminternal.CoreDB, ac mpminternal.ActiveContext, p map[string]interface{}) (interface{}, error) {
	workID, _ := p["work_id"].(string)
	w, err := dm.ReopenWorkWithContext(workID, ac)
	if err != nil {
		return nil, err
	}
	return workToMapWork(w), nil
}

func workToMapWork(w *mpminternal.Work) map[string]interface{} {
	m := map[string]interface{}{
		"id":           w.ID,
		"title":        w.Title,
		"status":       w.Status,
		"verification": w.Verification,
		"created_at":   w.CreatedAt,
		"updated_at":   w.UpdatedAt,
	}
	if w.Content != "" {
		m["content"] = w.Content
	}
	if w.CompletedAt != nil {
		m["completed_at"] = *w.CompletedAt
	}
	if w.SessionID != "" {
		m["session_id"] = w.SessionID
	}
	return m
}

func workEventToMap(e *mpminternal.WorkEvent) map[string]interface{} {
	m := map[string]interface{}{
		"id":          e.ID,
		"work_id":     e.WorkID,
		"event_index": e.EventIndex,
		"event_type":  e.EventType,
		"created_at":  e.CreatedAt,
	}
	if e.InvocationID != "" {
		m["invocation_id"] = e.InvocationID
	}
	if e.ParentInvocationID != "" {
		m["parent_invocation_id"] = e.ParentInvocationID
	}
	if e.Note != "" {
		m["note"] = e.Note
	}
	if e.Title != "" {
		m["title"] = e.Title
	}
	if e.Content != "" {
		m["content"] = e.Content
	}
	if len(e.DirectiveIDs) > 0 {
		m["directive_ids"] = e.DirectiveIDs
	}
	return m
}
