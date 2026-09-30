package internal

import "fmt"

// WorkStatus represents the lifecycle state of a work item.
type WorkStatus string

const (
	WorkStatusOpen       WorkStatus = "open"
	WorkStatusDone       WorkStatus = "done"
	WorkStatusCancelled  WorkStatus = "cancelled"
)

// WorkVisibility is the independent visibility axis for a work item.
//
// Visibility is orthogonal to WorkStatus: it answers "is this item in
// the operational view?", while status answers "what happened to it?".
// The two never substitute for each other.
//
// Design: docs/designs/2026-09-30-work-archive-and-purge.md §2.
type WorkVisibility string

const (
	// WorkVisibilityActive is the default: operational, non-archived work.
	WorkVisibilityActive WorkVisibility = "active"
	// WorkVisibilityArchived returns only archived work.
	WorkVisibilityArchived WorkVisibility = "archived"
	// WorkVisibilityAll returns the union of active and archived work.
	WorkVisibilityAll WorkVisibility = "all"
)

// ValidWorkVisibilities enumerates the accepted `visibility` filter
// values for work listing. Kept sorted; the single source of truth for
// validation on both the MCP and CLI paths (mirrors
// validWorkStatuses in internal/core/tools/work_handlers.go).
var ValidWorkVisibilities = []string{"active", "all", "archived"}

// NormalizeWorkVisibility maps an empty string to the default
// ("active") and validates the result. An unknown value is an error
// rather than a silent zero-row result, so a typo is never
// indistinguishable from "no items exist" (W-010 class).
func NormalizeWorkVisibility(v string) (WorkVisibility, error) {
	if v == "" {
		return WorkVisibilityActive, nil
	}
	switch WorkVisibility(v) {
	case WorkVisibilityActive, WorkVisibilityArchived, WorkVisibilityAll:
		return WorkVisibility(v), nil
	}
	return "", fmt.Errorf("unknown visibility %q (use active|archived|all)", v)
}

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
	// ArchivedAt is the derived projection of the `archived` /
	// `unarchived` event pair. nil means not archived. Omitempty
	// mirrors CompletedAt: absent on the wire means "not archived",
	// so there is exactly one source of truth.
	//
	// Design: docs/designs/2026-09-30-work-archive-and-purge.md §1.1
	ArchivedAt *int64 `json:"archived_at,omitempty"`
}

// IsArchived reports whether the work item is currently archived.
func (w *Work) IsArchived() bool { return w != nil && w.ArchivedAt != nil }

// WakeContextWork is the bounded projection of a work item for wake context.
type WakeContextWork struct {
	ID            string            `json:"id"`
	Title         string            `json:"title"`    // truncated to 120 chars
	Status        WorkStatus        `json:"status"`
	Verification  WorkVerification  `json:"verification"`
	Pointer       string            `json:"pointer"` // "mpm://work/<id>"
	CreatedAt     int64             `json:"created_at"`
}
