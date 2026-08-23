package internal

// WorkEventType is the vocabulary of valid event types.
type WorkEventType string

const (
	WorkEventTypeCreated        WorkEventType = "created"
	WorkEventTypeNoteAppended  WorkEventType = "note_appended"
	WorkEventTypeCompleted     WorkEventType = "completed"
	WorkEventTypeCancelled     WorkEventType = "cancelled"
	WorkEventTypeReopened     WorkEventType = "reopened"
	WorkEventTypeTitleUpdated  WorkEventType = "title_updated"
	WorkEventTypeContentUpdated WorkEventType = "content_updated"
)

// WorkEvent is an immutable record of one state transition in a Work item's lifetime.
type WorkEvent struct {
	ID                 string         `json:"id"`
	WorkID             string         `json:"work_id"`
	EventIndex         int            `json:"event_index"`
	EventType          WorkEventType  `json:"event_type"`
	CreatedAt          int64          `json:"created_at"` // Unix epoch seconds

	// Provenance context at time of event (same schema as EffectiveProvenance)
	ActorKind          string         `json:"actor_kind"`
	ActorID            string         `json:"actor_id,omitempty"`
	FrameworkName      string         `json:"framework_name,omitempty"`
	FrameworkVersion   string         `json:"framework_version,omitempty"`
	ProviderName       string         `json:"provider_name,omitempty"`
	ModelName          string         `json:"model_name,omitempty"`
	ModelRevision      string         `json:"model_revision,omitempty"`
	SessionID          string         `json:"session_id,omitempty"`
	InvocationID       string         `json:"invocation_id,omitempty"`
	ParentInvocationID string         `json:"parent_invocation_id,omitempty"`

	// Event-specific payload
	Note   string `json:"note,omitempty"`
	Title  string `json:"title,omitempty"`
	Content string `json:"content,omitempty"`
}
