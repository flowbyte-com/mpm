// Package assignment is the coordination service over MPM's durable
// assignment storage.
//
// PR #259 (Continuity 001, PR 1) created the storage: two tables, seven
// indexes and thirty triggers, enforcing thirty-four properties in the
// database itself. This package is PR 2: the business logic that writes
// those rows, the CLI and MCP surfaces that drive it, and the four
// invariants PR 1 deliberately deferred to the API (N1 the fence, N2 the
// child's recipient, N3 the child's depth, N6 the operator's root).
//
// # WHAT THIS PACKAGE IS NOT
//
// It is not an identity system. MPM runs in a single-operator trust
// domain: an agent under the operator's own account can read the
// operator's files and run the operator's binaries. Every attribution
// field this package writes — actor_address, actor_kind — records WHO
// WROTE A ROW, not who that party was. Nothing here treats attribution as
// authentication, and the two operator-only operations are kept out of the
// agent-facing registry structurally rather than by a flag.
//
// See docs/dev/continuity-001-assignment-api.md for the design of record.
package assignment

import (
	"encoding/json"
	"errors"
	"time"
)

// ── The closed vocabularies ────────────────────────────────────────────────
//
// These mirror PR 1's schema exactly. They are constants rather than
// free strings so that a typo is a compile error and so that the service
// can never write a state the state machine does not have.

// The two roles an assignment can carry.
const (
	RoleImplementer = "implementer"
	RoleReviewer    = "reviewer"
)

// The ten states.
const (
	StateEnqueued         = "enqueued"
	StateDelivered        = "delivered"
	StateAcknowledged     = "acknowledged"
	StateAccepted         = "accepted"
	StateInProgress       = "in_progress"
	StateCompleted        = "completed"
	StateReviewPassed     = "review_passed"
	StateAwaitingOperator = "awaiting_operator"
	StateOperatorApproved = "operator_approved"
	StateCancelled        = "cancelled"
)

// The thirteen ledger event types.
const (
	EventCreated          = "created"
	EventDelivered        = "delivered"
	EventAcknowledged     = "acknowledged"
	EventAccepted         = "accepted"
	EventStarted          = "started"
	EventCompleted        = "completed"
	EventReviewPassed     = "review_passed"
	EventAwaitingOperator = "awaiting_operator"
	EventOperatorApproved = "operator_approved"
	EventCancelled        = "cancelled"
	EventClaimed          = "claimed"
	// EventLeaseExpired is RESERVED.
	//
	// PR 1's schema fixes the ledger's vocabulary at thirteen event
	// types and `lease_expired` is one of them, so the constant stays
	// and the schema keeps accepting it. The service does not emit it:
	// a lease that lapses is not an event of its own, it is a CLAIM
	// that took over an expired lease, and that is recorded as
	// `claimed` with `reclaimed: true` and the previous holder in the
	// payload. Emitting a second event for the same lapse would put
	// two entries in the ledger for one thing that happened.
	EventLeaseExpired = "lease_expired"
	EventNote         = "note"
)

// The two review verdicts.
const (
	VerdictPassed           = "passed"
	VerdictChangesRequested = "changes_requested"
)

// The payload key that names the assignment a reviewer reviews.
//
// The delegation that creates the reviewer FIXES the target, so the
// reviewer cannot be re-pointed at an arbitrary subject afterwards.
const PayloadKeyReviewTarget = "review_target"

// ── The error vocabulary ───────────────────────────────────────────────────
//
// The CLI maps these to exit codes and the MCP surface reports them
// verbatim. They are deliberately few and deliberately specific: an
// operator reading "the presented claim epoch is stale" learns more than
// one reading "constraint violation".

var (
	// ErrNotFound is returned when no assignment carries the id.
	ErrNotFound = errors.New("assignment: no such assignment")

	// ErrStaleClaim is returned when the presented claim epoch is not
	// the current one. It is the fence: the write matched no row.
	ErrStaleClaim = errors.New("assignment: the presented claim epoch is stale")

	// ErrIllegalTransition is returned when the requested transition is
	// not on the role's path.
	ErrIllegalTransition = errors.New("assignment: illegal state transition")

	// ErrNotAuthorised is returned when an operation that requires the
	// operator's authority is reached without it.
	ErrNotAuthorised = errors.New("assignment: this operation needs the operator's authority")

	// ErrConstraint is returned when the request is refused by a rule
	// the API enforces: a recipient outside the root's allow-list, a
	// depth beyond the root's budget, a missing field.
	ErrConstraint = errors.New("assignment: refused by the assignment rules")

	// ErrRecordOnlyRefused is returned when a RECORD-ONLY caller asked
	// the adapter to record an observed outcome and the adapter could
	// not: the attempt is terminal, or its session was never created.
	//
	// It is NOT a refusal of the WORK. The attempt stays outstanding, the
	// work is not lost, and a later pass may dispatch the assignment
	// normally — so a caller that buckets refusals by this error must
	// treat it as "considered again", not as "will not be tried again".
	ErrRecordOnlyRefused = errors.New(
		"assignment: the outcome could not be recorded, and the caller asked for no launch")

	// ErrIdempotencyConflict is returned when the same idempotency key
	// is presented with a DIFFERENT logical assignment. Returning the
	// existing row would hand back something that does not match the
	// request, which is worse than an error.
	ErrIdempotencyConflict = errors.New("assignment: the idempotency key already names a different assignment")

	// ErrLeaseHeld is returned when a claim is attempted while another
	// holder's lease is still alive. A live lease is not stealable.
	ErrLeaseHeld = errors.New("assignment: the work is leased to another holder")

	// ErrAlreadyClaimed is returned when a renewal is attempted by
	// somebody who is not the holder, or when a claim is attempted by
	// the holder of a live lease (which is a renewal, not a claim).
	ErrAlreadyClaimed = errors.New("assignment: the lease is held and must be renewed, not claimed")

	// ErrRefused is returned by the storage itself: a trigger or a
	// CHECK constraint refused the write. It wraps the storage's own
	// message, which is the specific rule that refused it.
	ErrRefused = errors.New("assignment: refused by the storage")
)

// ── The rows ──────────────────────────────────────────────────────────────

// Assignment is one row of agent_tasks, projected.
//
// The JSON tags are the CLI's and the MCP surface's wire format.
type Assignment struct {
	ID             string `json:"id"`
	ProjectID      string `json:"project_id"`
	RootID         string `json:"root_id"`
	ParentID       string `json:"parent_id,omitempty"`
	Depth          int    `json:"depth"`
	Title          string `json:"title"`
	Objective      string `json:"objective"`
	Recipient      string `json:"recipient"`
	Role           string `json:"role"`
	State          string `json:"state"`
	IdempotencyKey string `json:"idempotency_key"`
	ClaimEpoch     int64  `json:"claim_epoch"`
	Owner          string `json:"owner,omitempty"`
	LeaseExpiresAt *int64 `json:"lease_expires_at,omitempty"`

	PermittedOperations []string `json:"permitted_operations,omitempty"`
	EligibleRecipients  []string `json:"eligible_recipients,omitempty"`

	MaxDepth         int64  `json:"max_depth,omitempty"`
	MaxTasks         int64  `json:"max_tasks,omitempty"`
	MaxConcurrency   int64  `json:"max_concurrency,omitempty"`
	MaxResourceUnits int64  `json:"max_resource_units,omitempty"`
	ResourceUnits    int64  `json:"resource_units"`
	IssueRef         string `json:"issue_ref,omitempty"`
	PRRef            string `json:"pr_ref,omitempty"`
	BaseSHA          string `json:"base_sha,omitempty"`
	HeadSHA          string `json:"head_sha,omitempty"`
	ReviewedSHA      string `json:"reviewed_sha,omitempty"`
	FindingsSHA256   string `json:"findings_sha256,omitempty"`
	EvidenceRef      string `json:"evidence_ref,omitempty"`

	AwaitingAction   string `json:"awaiting_action,omitempty"`
	ApprovalAction   string `json:"approval_action,omitempty"`
	ApprovalRevision string `json:"approval_revision,omitempty"`
	ApprovalSource   string `json:"approval_source,omitempty"`
	ApprovalEvidence string `json:"approval_evidence,omitempty"`
	ApprovedAt       *int64 `json:"approved_at,omitempty"`

	Payload map[string]interface{} `json:"payload,omitempty"`

	CreatedAt   int64  `json:"created_at"`
	UpdatedAt   int64  `json:"updated_at"`
	CompletedAt *int64 `json:"completed_at,omitempty"`
}

// Event is one row of agent_task_events, projected.
type Event struct {
	ID           string                 `json:"id"`
	TaskID       string                 `json:"task_id"`
	EventIndex   int64                  `json:"event_index"`
	EventType    string                 `json:"event_type"`
	FromState    string                 `json:"from_state,omitempty"`
	ToState      string                 `json:"to_state,omitempty"`
	ActorAddress string                 `json:"actor_address,omitempty"`
	ActorKind    string                 `json:"actor_kind,omitempty"`
	ClaimEpoch   *int64                 `json:"claim_epoch,omitempty"`
	Note         string                 `json:"note,omitempty"`
	Payload      map[string]interface{} `json:"payload,omitempty"`
	CreatedAt    int64                  `json:"created_at"`
}

// AssignmentList is a listing of assignments.
//
// It exists so the CLI and the MCP surface render the SAME shape for a
// listing: one wrapper with the rows and the count, rather than two
// shapes that drift apart.
type AssignmentList struct {
	Assignments []*Assignment `json:"assignments"`
	Count       int           `json:"count"`
}

// EventList is a listing of ledger entries.
type EventList struct {
	Events []Event `json:"events"`
	Count  int     `json:"count"`
}

// ── The parameters ────────────────────────────────────────────────────────

// Actor records who wrote a row. It is ATTRIBUTION, not identity.
//
// ActorKind is a free-form label ("agent", "operator", "runtime"). It is
// recorded, never trusted: nothing in this package branches on it to
// grant authority.
type Actor struct {
	Address string
	Kind    string
}

// CreateRootParams describes an operator-authorised root assignment.
//
// A root carries the tree's budgets; a child carries none of them. The
// schema enforces the split, so the root's parameters are the budgets and
// the child's are not.
type CreateRootParams struct {
	ProjectID      string
	Title          string
	Objective      string
	Recipient      string
	Role           string
	IdempotencyKey string

	PermittedOperations []string
	EligibleRecipients  []string

	MaxDepth         int64
	MaxTasks         int64
	MaxConcurrency   int64
	MaxResourceUnits int64
	ResourceUnits    int64

	IssueRef       string
	PRRef          string
	BaseSHA        string
	HeadSHA        string
	ReviewedSHA    string
	FindingsSHA256 string
	EvidenceRef    string

	Payload map[string]interface{}
}

// DelegateParams describes a child assignment inside an existing root.
//
// The child carries NO budgets and NO lists: it inherits the root's, and
// the schema's root/child CHECK makes that structural rather than a
// convention.
type DelegateParams struct {
	ParentID       string
	Title          string
	Objective      string
	Recipient      string
	Role           string
	IdempotencyKey string

	// Actor is the attribution of the DELEGATING party.
	//
	// It is recorded, never trusted. When it is empty the service
	// attributes the delegation to the PARENT's recipient — a
	// structural fact it knows, rather than a claim it accepts.
	Actor Actor

	ResourceUnits int64

	IssueRef       string
	PRRef          string
	BaseSHA        string
	HeadSHA        string
	ReviewedSHA    string
	FindingsSHA256 string
	EvidenceRef    string

	Payload map[string]interface{}
}

// FencedParams are the parameters of a write that presents the caller's
// claim epoch.
//
// Epoch is the CURRENT epoch of the assignment, and the write is
// `WHERE id = ? AND claim_epoch = ?`. A stale holder's write therefore
// matches no row, atomically with the write itself.
type FencedParams struct {
	ID    string
	Epoch int64
	Actor Actor
	Note  string
}

// ClaimParams describes a claim.
//
// ExpectedEpoch is the epoch the caller believes is current. The claim is
// a compare-and-swap, so two simultaneous claims cannot both succeed.
type ClaimParams struct {
	ID            string
	ExpectedEpoch int64
	Owner         string
	LeaseSeconds  int64
	Actor         Actor
}

// RenewParams describes a lease renewal.
//
// A renewal is a claim by the same holder: the epoch advances and the
// lease is new. The holder must present the epoch it holds.
type RenewParams struct {
	ID            string
	ExpectedEpoch int64
	Owner         string
	LeaseSeconds  int64
	Actor         Actor
}

// CancelParams describes a withdrawal.
type CancelParams struct {
	ID    string
	Epoch int64
	Actor Actor
	Note  string
}

// NoteParams describes an explicit ledger note.
type NoteParams struct {
	ID      string
	Actor   Actor
	Note    string
	Payload map[string]interface{}
}

// HandoffParams describes the hand-off to the operator's gate.
//
// AwaitingAction is the EXACT action the operator is being asked to
// authorise. It is written by this transition and by no other, so the
// record is attributable to the event that claims it.
type HandoffParams struct {
	ID             string
	Epoch          int64
	AwaitingAction string
	Actor          Actor
	Note           string
}

// ReviewResultParams describes a review verdict.
type ReviewResultParams struct {
	ReviewerID string
	Verdict    string
	Epoch      int64
	Actor      Actor
	Note       string
}

// ApprovalParams describes an externally verified approval.
//
// Every field is REQUIRED. An approval with no evidence pointer behind it
// is a claim about what was authorised with nothing to check it against.
//
// There is no Actor field: the attribution of an approval comes from the
// OPERATOR AUTHORITY that performs it, not from a field the caller sets.
// A caller that could choose its own attribution would be recording a
// claim in a column that is supposed to record a fact.
type ApprovalParams struct {
	ID       string
	Action   string
	Revision string
	Source   string
	Evidence string

	// Epoch is the claim epoch the operator READ when they decided to
	// approve. The write is `WHERE id = ? AND claim_epoch = ?` with
	// THIS value, so an approval made against a different revision of
	// the assignment does not land: the operator approves what they
	// read, and nothing else.
	//
	// The operator learns the epoch from `mpm assignment show`.
	Epoch int64
}

// InboxQuery selects the assignments an addressee can see.
type InboxQuery struct {
	Recipient string
	States    []string
	ProjectID string
	Limit     int
}

// The default and maximum size of an inbox listing.
//
// MaxInboxLimit is EXPORTED, because a reader that has to know whether
// it saw EVERYTHING has to know the bound it was read under: a listing
// that stopped at the limit says nothing at all about what was past it.
// The publisher of the engineering evidence needs exactly that — it
// reads one row past its own bound so it can tell a full store from a
// truncated one, and that extra row is only reachable while the store's
// own limit is above it.
const (
	defaultInboxLimit = 50
	// MaxInboxLimit is the largest number of assignments one inbox
	// listing returns. A larger Limit is clamped to it.
	MaxInboxLimit = 500
)

// ── The operator authority ────────────────────────────────────────────────

// OperatorAuthority is the explicit trusted-call boundary for the two
// operator-only operations.
//
// The interface has an UNEXPORTED method, so no type outside this package
// can satisfy it: no JSON payload and no agent-facing handler can
// fabricate one. The concrete value comes from NewLocalOperatorAuthority.
//
// THIS IS A BOUNDARY, NOT AUTHENTICATION.
//
// MPM runs in a single-operator trust domain. An agent under the
// operator's own account can run the operator's binaries, so an authority
// value cannot distinguish "the operator asked for this" from "the agent
// decided this". What it DOES do is make the operator path explicit in
// the call, and — because the two operator-only operations are absent from
// the tool registry — keep the agent-facing surface structurally unable to
// choose it.
//
// See docs/dev/continuity-001-assignment-api.md §2.
type OperatorAuthority interface {
	// operatorAuthority is unexported on purpose: it makes this
	// interface unimplementable outside this package.
	operatorAuthority()

	// Actor is the attribution recorded on the rows the operation
	// writes. It is an address and a label, never a credential.
	Actor() Actor
}

// localOperator is the only implementation of OperatorAuthority.
type localOperator struct {
	actor Actor
}

// operatorAuthority implements OperatorAuthority.
func (localOperator) operatorAuthority() {}

// Actor implements OperatorAuthority.
func (o localOperator) Actor() Actor { return o.actor }

// NewLocalOperatorAuthority returns the authority for a locally invoked
// operator command.
//
// The actor is RECORDED, not verified. It is the operator's own name, or
// whatever the caller says it is.
func NewLocalOperatorAuthority(actor string) OperatorAuthority {
	if actor == "" {
		actor = "operator"
	}
	return localOperator{actor: Actor{Address: actor, Kind: "operator"}}
}

// marshalStringList renders a string slice as the JSON array the schema
// stores, and refuses an element that is empty or not a string.
func marshalStringList(what string, list []string) (string, error) {
	if list == nil {
		list = []string{}
	}
	seen := map[string]bool{}
	out := make([]string, 0, len(list))
	for _, v := range list {
		if v == "" {
			return "", errors.New("assignment: " + what + " must not contain an empty element")
		}
		if seen[v] {
			continue
		}
		seen[v] = true
		out = append(out, v)
	}
	b, err := json.Marshal(out)
	if err != nil {
		return "", err
	}
	return string(b), nil
}

// unmarshalStringList reads the JSON array the schema stores.
func unmarshalStringList(raw string) []string {
	if raw == "" {
		return nil
	}
	var out []string
	if err := json.Unmarshal([]byte(raw), &out); err != nil {
		return nil
	}
	return out
}

// unmarshalPayload reads the JSON object the schema stores.
func unmarshalPayload(raw string) map[string]interface{} {
	if raw == "" {
		return nil
	}
	var out map[string]interface{}
	if err := json.Unmarshal([]byte(raw), &out); err != nil {
		return nil
	}
	return out
}

// nowUnix converts the service's clock reading to the unix seconds the
// schema stores.
func nowUnix(t time.Time) int64 { return t.UTC().Unix() }
