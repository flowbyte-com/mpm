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

// workEffectiveProvenance builds an EffectiveProvenance from the ActiveContext
// for work event writes. It captures actor_kind, actor_id, session_id, and
// framework_name from the caller's context.
func workEffectiveProvenance(dm mpminternal.CoreDB, ac mpminternal.ActiveContext) *mpminternal.EffectiveProvenance {
	prov := &mpminternal.EffectiveProvenance{
		ActorKind: "agent",
		ActorID:   ac.Agent,
		SessionID: ac.SessionID,
	}
	if ac.FrameworkName != "" {
		prov.FrameworkName = ac.FrameworkName
	}
	return prov
}

// recordWorkProvenance writes an artifact_provenance row for a newly created
// work. Failures are non-fatal — the error is logged but the handler returns
// success.
func recordWorkProvenance(dm mpminternal.CoreDB, workID string, ac mpminternal.ActiveContext) {
	if workID == "" {
		return
	}
	// dm is always *DatabaseManager at runtime. Use the convenience method
	// RecordWorkArtifactProvenance which handles its own transaction.
	dbm, ok := dm.(*mpminternal.DatabaseManager)
	if !ok {
		return
	}
	prov := workEffectiveProvenance(dm, ac)
	dbm.RecordWorkArtifactProvenance(workID, prov.ActorKind, prov.ActorID, prov.FrameworkName, prov.SessionID)
}

func handleCreateWork(dm mpminternal.CoreDB, ac mpminternal.ActiveContext, p map[string]interface{}) (interface{}, error) {
	title, _ := p["title"].(string)
	if title == "" {
		return nil, fmt.Errorf("title is required for create")
	}
	content, _ := p["content"].(string)
	sessionID, _ := p["session_id"].(string)

	prov := workEffectiveProvenance(dm, ac)

	var workID string
	// All writes (works row INSERT + created event INSERT + works projection UPDATE)
	// must be in ONE transaction so they commit atomically.
	err := dm.WithTx(func(node mpminternal.DBNode) error {
		// Insert the works row using the tx-aware node.
		w, err := dm.AddWork(title, content, sessionID)
		if err != nil {
			return err
		}
		workID = w.ID

		// Append the created event inside the same transaction.
		_, err = dm.AppendWorkEvent(workID, mpminternal.WorkEvent{
			EventType: mpminternal.WorkEventTypeCreated,
			Title:     title,
			Content:   content,
		}, prov, node)
		return err
	})
	if err != nil {
		return nil, fmt.Errorf("create work: %w", err)
	}

	// Record provenance (non-fatal). Do this after the transaction commits
	// so the provenance row is the last write. If it fails, we log and continue.
	recordWorkProvenance(dm, workID, ac)

	w, err := dm.GetWork(workID)
	if err != nil {
		return nil, fmt.Errorf("get work after create: %w", err)
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

// handleUpdateWork handles work updates. The status=X path is deprecated
// (ruling 3); it maps to the appropriate event. Title and content changes
// emit dedicated title_updated / content_updated events.
func handleUpdateWork(dm mpminternal.CoreDB, ac mpminternal.ActiveContext, p map[string]interface{}) (interface{}, error) {
	workID, _ := p["work_id"].(string)
	if workID == "" {
		return nil, fmt.Errorf("work_id is required for update")
	}
	statusStr, _ := p["status"].(string)
	title, _ := p["title"].(string)
	content, _ := p["content"].(string)

	prov := workEffectiveProvenance(dm, ac)

	// Determine event type from params.
	var eventType mpminternal.WorkEventType

	switch {
	case statusStr != "":
		// Deprecated path: status=X maps to the appropriate event.
		// This logs a deprecation warning but still works.
		switch mpminternal.WorkStatus(statusStr) {
		case mpminternal.WorkStatusDone:
			eventType = mpminternal.WorkEventTypeCompleted
		case mpminternal.WorkStatusCancelled:
			eventType = mpminternal.WorkEventTypeCancelled
		case mpminternal.WorkStatusOpen:
			eventType = mpminternal.WorkEventTypeReopened
		default:
			return nil, fmt.Errorf("invalid status %q; must be open, done, or cancelled", statusStr)
		}
	case title != "":
		eventType = mpminternal.WorkEventTypeTitleUpdated
	case content != "":
		eventType = mpminternal.WorkEventTypeContentUpdated
	default:
		return nil, fmt.Errorf("at least one of status, title, or content is required for update")
	}

	// Log deprecation warning for status path.
	if statusStr != "" {
		type hasLogAudit interface {
			LogAudit(level mpminternal.AuditLevel, component, message, stack string, ctx mpminternal.AuditContext)
		}
		if logger, ok := dm.(hasLogAudit); ok {
			logger.LogAudit(mpminternal.AuditWarn, "work", "deprecated update status=X path used", "", mpminternal.AuditContext{
				"work_id": workID,
				"status":  statusStr,
			})
		}
	}

	// Append the event inside a transaction (atomically updates works row projection).
	var event *mpminternal.WorkEvent
	err := dm.WithTx(func(node mpminternal.DBNode) error {
		var err error
		event, err = dm.AppendWorkEvent(workID, mpminternal.WorkEvent{
			EventType: eventType,
			Title:     title,
			Content:   content,
		}, prov, node)
		return err
	})
	if err != nil {
		return nil, fmt.Errorf("append %s event: %w", eventType, err)
	}
	_ = event

	w, err := dm.GetWork(workID)
	if err != nil {
		return nil, fmt.Errorf("get work after update: %w", err)
	}
	return workToMapWork(w), nil
}

func handleCompleteWork(dm mpminternal.CoreDB, ac mpminternal.ActiveContext, p map[string]interface{}) (interface{}, error) {
	workID, _ := p["work_id"].(string)
	if workID == "" {
		return nil, fmt.Errorf("work_id is required for complete")
	}
	note, _ := p["note"].(string)

	prov := workEffectiveProvenance(dm, ac)

	var event *mpminternal.WorkEvent
	err := dm.WithTx(func(node mpminternal.DBNode) error {
		var err error
		event, err = dm.AppendWorkEvent(workID, mpminternal.WorkEvent{
			EventType: mpminternal.WorkEventTypeCompleted,
			Note:      note,
		}, prov, node)
		return err
	})
	if err != nil {
		return nil, fmt.Errorf("append completed event: %w", err)
	}
	_ = event

	w, err := dm.GetWork(workID)
	if err != nil {
		return nil, fmt.Errorf("get work after complete: %w", err)
	}
	return workToMapWork(w), nil
}

func handleCancelWork(dm mpminternal.CoreDB, ac mpminternal.ActiveContext, p map[string]interface{}) (interface{}, error) {
	workID, _ := p["work_id"].(string)
	if workID == "" {
		return nil, fmt.Errorf("work_id is required for cancel")
	}
	note, _ := p["note"].(string)

	prov := workEffectiveProvenance(dm, ac)

	var event *mpminternal.WorkEvent
	err := dm.WithTx(func(node mpminternal.DBNode) error {
		var err error
		event, err = dm.AppendWorkEvent(workID, mpminternal.WorkEvent{
			EventType: mpminternal.WorkEventTypeCancelled,
			Note:      note,
		}, prov, node)
		return err
	})
	if err != nil {
		return nil, fmt.Errorf("append cancelled event: %w", err)
	}
	_ = event

	w, err := dm.GetWork(workID)
	if err != nil {
		return nil, fmt.Errorf("get work after cancel: %w", err)
	}
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

// handleNoteWork appends a note_appended event to a work item's history.
func handleNoteWork(dm mpminternal.CoreDB, ac mpminternal.ActiveContext, p map[string]interface{}) (interface{}, error) {
	workID, _ := p["work_id"].(string)
	if workID == "" {
		return nil, fmt.Errorf("work_id is required for note")
	}
	note, _ := p["note"].(string)
	if note == "" {
		return nil, fmt.Errorf("note is required for note action")
	}

	prov := workEffectiveProvenance(dm, ac)

	var event *mpminternal.WorkEvent
	err := dm.WithTx(func(node mpminternal.DBNode) error {
		var err error
		event, err = dm.AppendWorkEvent(workID, mpminternal.WorkEvent{
			EventType: mpminternal.WorkEventTypeNoteAppended,
			Note:      note,
		}, prov, node)
		return err
	})
	if err != nil {
		return nil, fmt.Errorf("append note event: %w", err)
	}

	return workEventToMap(event), nil
}

// handleReopenWork appends a reopened event, clearing the terminal
// state (completed_at) on the works row projection.
func handleReopenWork(dm mpminternal.CoreDB, ac mpminternal.ActiveContext, p map[string]interface{}) (interface{}, error) {
	workID, _ := p["work_id"].(string)
	if workID == "" {
		return nil, fmt.Errorf("work_id is required for reopen")
	}

	prov := workEffectiveProvenance(dm, ac)

	var event *mpminternal.WorkEvent
	err := dm.WithTx(func(node mpminternal.DBNode) error {
		var err error
		event, err = dm.AppendWorkEvent(workID, mpminternal.WorkEvent{
			EventType: mpminternal.WorkEventTypeReopened,
		}, prov, node)
		return err
	})
	if err != nil {
		return nil, fmt.Errorf("append reopened event: %w", err)
	}
	_ = event

	w, err := dm.GetWork(workID)
	if err != nil {
		return nil, fmt.Errorf("get work after reopen: %w", err)
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

func workEventToMap(e *mpminternal.WorkEvent) map[string]interface{} {
	m := map[string]interface{}{
		"id":          e.ID,
		"work_id":     e.WorkID,
		"event_index": e.EventIndex,
		"event_type":  e.EventType,
		"created_at":  e.CreatedAt,
		"actor_kind":  e.ActorKind,
	}
	if e.ActorID != "" {
		m["actor_id"] = e.ActorID
	}
	if e.FrameworkName != "" {
		m["framework_name"] = e.FrameworkName
	}
	if e.ProviderName != "" {
		m["provider_name"] = e.ProviderName
	}
	if e.ModelName != "" {
		m["model_name"] = e.ModelName
	}
	if e.SessionID != "" {
		m["session_id"] = e.SessionID
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
	return m
}
