package internal

// WorkEventType is the vocabulary of valid event types.
type WorkEventType string

const (
	WorkEventTypeCreated         WorkEventType = "created"
	WorkEventTypeNoteAppended   WorkEventType = "note_appended"
	WorkEventTypeCompleted      WorkEventType = "completed"
	WorkEventTypeCancelled      WorkEventType = "cancelled"
	WorkEventTypeReopened      WorkEventType = "reopened"
	WorkEventTypeTitleUpdated   WorkEventType = "title_updated"
	WorkEventTypeContentUpdated WorkEventType = "content_updated"
	// Phase 2: provenance/verification events
	WorkEventTypeClaimedComplete    WorkEventType = "claimed_complete"
	WorkEventTypeEvidenceObserved   WorkEventType = "evidence_observed"
	// Archive lifecycle (2026-09-30). Visibility events, NOT status
	// transitions: they leave works.status untouched, so archiving a
	// cancelled item and later unarchiving returns it to `cancelled`,
	// never to `open`.
	//
	// Design: docs/archive/2026-09-30-work-archive-and-purge.md §1.2
	WorkEventTypeArchived   WorkEventType = "archived"
	WorkEventTypeUnarchived WorkEventType = "unarchived"
)

// WorkEvent is an immutable record of one state transition in a Work item's lifetime.
//
// Provenance is NOT duplicated here. Only invocation_id (and parent) are stored;
// full execution telemetry (actor, model, provider, framework, session, etc.)
// lives in artifact_provenance and is reachable via JOIN on invocation_id.
// See forensic fix: domain-neutral Work primitive.
type WorkEvent struct {
	ID                 string        `json:"id"`
	WorkID             string        `json:"work_id"`
	EventIndex         int           `json:"event_index"`
	EventType          WorkEventType `json:"event_type"`
	CreatedAt          int64         `json:"created_at"` // Unix epoch seconds

	// Provenance linkage — join to artifact_provenance via invocation_id
	// for full execution context (actor, model, provider, session, etc.)
	InvocationID       string `json:"invocation_id,omitempty"`
	ParentInvocationID string `json:"parent_invocation_id,omitempty"`

	// Event-specific payload
	Note    string `json:"note,omitempty"`
	Title   string `json:"title,omitempty"`
	Content string `json:"content,omitempty"`

	// Instruction provenance — which directives governed this event
	DirectiveIDs []string `json:"directive_ids,omitempty"` // StableIDs active at event time
}
