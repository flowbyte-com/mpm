package internal

// WorkStatus represents the lifecycle state of a work item.
type WorkStatus string

const (
	WorkStatusOpen       WorkStatus = "open"
	WorkStatusDone       WorkStatus = "done"
	WorkStatusCancelled  WorkStatus = "cancelled"
)

// isValidWorkTransition enforces the F-B1 state machine:
//
//	open       → done | cancelled
//	done       → open   (reopen)
//	cancelled  → open   (reopen)
//
// Same-state transitions (e.g. open → open) and out-of-order
// transitions (done → cancelled) are rejected. The hostile test
// surfaced that the substrate silently accepted these, which masked
// operator error and broke the verification lifecycle.
func isValidWorkTransition(from, to WorkStatus) bool {
	if from == to {
		return false
	}
	switch from {
	case WorkStatusOpen:
		return to == WorkStatusDone || to == WorkStatusCancelled
	case WorkStatusDone, WorkStatusCancelled:
		return to == WorkStatusOpen
	}
	return false
}

// WorkVerification represents the epistemic verification state of a work item.
// Distinguishes agent assertion from observable evidence from persisted state.
type WorkVerification string

const (
	WorkVerificationUnverified  WorkVerification = "unverified"  // default, no evidence collected
	WorkVerificationVerified    WorkVerification = "verified"    // evidence confirms completion
	WorkVerificationPartial     WorkVerification = "partial"     // some evidence, incomplete
	WorkVerificationContradicted WorkVerification = "contradicted" // evidence contradicts claim
)

// Work is a durable representation of an intended future action.
type Work struct {
	ID            string           `json:"id"`
	Title         string           `json:"title"`
	Content       string           `json:"content,omitempty"`
	Status        WorkStatus       `json:"status"`
	Verification  WorkVerification `json:"verification"`
	CreatedAt     int64            `json:"created_at"`
	UpdatedAt     int64            `json:"updated_at"`
	CompletedAt   *int64           `json:"completed_at,omitempty"`
	SessionID     string           `json:"session_id,omitempty"`
}

// WakeContextWork is the bounded projection of a work item for wake context.
type WakeContextWork struct {
	ID            string            `json:"id"`
	Title         string            `json:"title"`    // truncated to 120 chars
	Status        WorkStatus        `json:"status"`
	Verification  WorkVerification  `json:"verification"`
	Pointer       string            `json:"pointer"` // "mpm://work/<id>"
	CreatedAt     int64             `json:"created_at"`
}
