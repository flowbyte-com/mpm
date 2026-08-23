package internal

// WorkStatus represents the lifecycle state of a work item.
type WorkStatus string

const (
	WorkStatusOpen       WorkStatus = "open"
	WorkStatusDone       WorkStatus = "done"
	WorkStatusCancelled  WorkStatus = "cancelled"
)

// Work is a durable representation of an intended future action.
type Work struct {
	ID          string     `json:"id"`
	Title       string     `json:"title"`
	Content     string     `json:"content,omitempty"`
	Status      WorkStatus `json:"status"`
	CreatedAt   int64      `json:"created_at"`
	UpdatedAt   int64      `json:"updated_at"`
	CompletedAt *int64     `json:"completed_at,omitempty"`
	SessionID   string     `json:"session_id,omitempty"`
}

// WakeContextWork is the bounded projection of a work item for wake context.
type WakeContextWork struct {
	ID        string     `json:"id"`
	Title     string     `json:"title"`    // truncated to 120 chars
	Status    WorkStatus `json:"status"`
	Pointer   string     `json:"pointer"` // "mpm://work/<id>"
	CreatedAt int64      `json:"created_at"`
}
