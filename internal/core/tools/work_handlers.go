package tools

import (
	"fmt"

	mpminternal "github.com/flowbyte-com/mpm-core"
)

// handleMpmWork dispatches work actions: create, list, show, update, complete, cancel.
func handleMpmWork(dm mpminternal.CoreDB, ac mpminternal.ActiveContext, payload map[string]interface{}) (interface{}, error) {
	params, err := extractParamsOrFail("mpm_work", payload)
	if err != nil {
		return nil, err
	}
	action, _ := payload["action"].(string)

	switch action {
	case "create":
		return handleCreateWork(dm, params)
	case "list":
		return handleListWorks(dm, params)
	case "show":
		return handleShowWork(dm, params)
	case "update":
		return handleUpdateWork(dm, params)
	case "complete":
		return handleCompleteWork(dm, params)
	case "cancel":
		return handleCancelWork(dm, params)
	default:
		return nil, fmt.Errorf("unknown action %q for mpm_work. Valid actions include create, list, show, update, complete, cancel", action)
	}
}

// typeAssertDBM converts a CoreDB to *DatabaseManager for accessing Work-specific methods
// that are on DatabaseManager but not yet on the CoreDB interface.
// This is safe within the tools package because dm is always a *DatabaseManager
// (DatabaseManager implements CoreDB, and tools always receives a *DatabaseManager).
func typeAssertDBM(dm mpminternal.CoreDB) *mpminternal.DatabaseManager {
	return dm.(*mpminternal.DatabaseManager)
}

func handleCreateWork(dm mpminternal.CoreDB, p map[string]interface{}) (interface{}, error) {
	title, _ := p["title"].(string)
	if title == "" {
		return nil, fmt.Errorf("title is required for create")
	}
	content, _ := p["content"].(string)
	sessionID, _ := p["session_id"].(string)

	w, err := typeAssertDBM(dm).AddWork(title, content, sessionID)
	if err != nil {
		return nil, err
	}
	return workToMapWork(w), nil
}

func handleListWorks(dm mpminternal.CoreDB, p map[string]interface{}) (interface{}, error) {
	works, err := typeAssertDBM(dm).ListWorks()
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
	w, err := typeAssertDBM(dm).GetWork(workID)
	if err != nil {
		return nil, err
	}
	return workToMapWork(w), nil
}

func handleUpdateWork(dm mpminternal.CoreDB, p map[string]interface{}) (interface{}, error) {
	workID, _ := p["work_id"].(string)
	if workID == "" {
		return nil, fmt.Errorf("work_id is required for update")
	}
	statusStr, _ := p["status"].(string)
	if statusStr == "" {
		return nil, fmt.Errorf("status is required for update")
	}
	w, err := typeAssertDBM(dm).UpdateWork(workID, mpminternal.WorkStatus(statusStr))
	if err != nil {
		return nil, err
	}
	return workToMapWork(w), nil
}

func handleCompleteWork(dm mpminternal.CoreDB, p map[string]interface{}) (interface{}, error) {
	workID, _ := p["work_id"].(string)
	if workID == "" {
		return nil, fmt.Errorf("work_id is required for complete")
	}
	w, err := typeAssertDBM(dm).CompleteWork(workID)
	if err != nil {
		return nil, err
	}
	return workToMapWork(w), nil
}

func handleCancelWork(dm mpminternal.CoreDB, p map[string]interface{}) (interface{}, error) {
	workID, _ := p["work_id"].(string)
	if workID == "" {
		return nil, fmt.Errorf("work_id is required for cancel")
	}
	w, err := typeAssertDBM(dm).CancelWork(workID)
	if err != nil {
		return nil, err
	}
	return workToMapWork(w), nil
}

func workToMapWork(w *mpminternal.Work) map[string]interface{} {
	m := map[string]interface{}{
		"id":         w.ID,
		"title":      w.Title,
		"status":     w.Status,
		"created_at": w.CreatedAt,
		"updated_at": w.UpdatedAt,
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
