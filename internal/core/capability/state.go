package capability

import "fmt"

// CapabilityState is the lifecycle state of a capability. See §1.2 of
// docs/architecture/capability-lifecycle.md for semantics.
//
// Storage-layer constraint: the `state` column in the capabilities
// table has a CHECK constraint that lists every state in
// AllCapabilityStates(). The two MUST stay in sync — adding a state
// here without updating the CHECK is a forge bug.
type CapabilityState string

const (
	StateDraft         CapabilityState = "draft"
	StateLinted        CapabilityState = "linted"
	StateValidated     CapabilityState = "validated"
	StateProbation     CapabilityState = "probation"
	StateActive        CapabilityState = "active"
	StateDegraded      CapabilityState = "degraded"
	StateNeedsRevision CapabilityState = "needs_revision"
	StateFractured     CapabilityState = "fractured"
	StateRolledBack    CapabilityState = "rolled_back"
	StateRetired       CapabilityState = "retired"
)

// AllCapabilityStates returns every legal state. Used to (a) generate
// the storage-layer CHECK constraint and (b) drive validation routines
// that iterate the full matrix.
func AllCapabilityStates() []CapabilityState {
	return []CapabilityState{
		StateDraft, StateLinted, StateValidated, StateProbation,
		StateActive, StateDegraded, StateNeedsRevision,
		StateFractured, StateRolledBack, StateRetired,
	}
}

// validTransitions is the authoritative lifecycle matrix (§1.3 of the
// spec). Any transition not in this table is refused at the type layer
// before any SQL is constructed.
//
// Enforced twice:
//   1. CapabilityState.CanTransitionTo() — runtime guard in handlers
//   2. SQLite CHECK constraint on the state column — storage-layer guard
//
// Both must agree. Adding a transition requires updating BOTH.
var validTransitions = map[CapabilityState]map[CapabilityState]bool{
	StateDraft: {
		StateLinted: true,
	},
	StateLinted: {
		StateValidated: true,
	},
	StateValidated: {
		StateProbation: true,
	},
	StateProbation: {
		StateActive: true,
	},
	StateActive: {
		StateDegraded:      true,
		StateNeedsRevision: true,
		StateRolledBack:    true,
	},
	StateDegraded: {
		StateActive:    true,
		StateFractured: true,
	},
	StateFractured: {
		StateNeedsRevision: true,
	},
	StateNeedsRevision: {
		StateDraft: true,
	},
	StateRolledBack: {}, // terminal: lineage preserved, no further moves
	StateRetired:    {}, // terminal: end-of-life
}

// CanTransitionTo returns true if a transition from s to next is in
// the lifecycle matrix. This is the runtime guard for forge and cascade
// handlers — illegal transitions fail here before any SQL is constructed.
//
// Special rule from spec §1.3 (last row of the transition table):
// `* → retired` is always legal — explicit operator action OR supersede
// by successful promotion can retire any non-terminal capability.
// This is encoded here as a wildcard rather than per-state entries so
// the matrix stays focused on the normal-path transitions.
//
// An unknown from-state always returns false.
func (s CapabilityState) CanTransitionTo(next CapabilityState) bool {
	// Wildcard: any non-terminal state can transition to retired.
	// Retired itself is terminal — its entry in validTransitions is
	// empty, so it can never leave.
	if next == StateRetired {
		_, isKnown := validTransitions[s]
		return isKnown
	}
	allowed, ok := validTransitions[s]
	if !ok {
		return false
	}
	return allowed[next]
}

// Validate returns an error if s is not a recognized capability state.
// Used at storage boundaries (Scan) to reject garbage rows.
func (s CapabilityState) Validate() error {
	if _, ok := validTransitions[s]; !ok {
		return fmt.Errorf("capability: invalid state %q (must be one of %v)", s, AllCapabilityStates())
	}
	return nil
}

// IsCallable returns true if a capability in this state is allowed to be
// invoked by the runtime. Only probation, active, and degraded are
// callable — all other states are pre-flight, post-mortem, or terminal.
func (s CapabilityState) IsCallable() bool {
	switch s {
	case StateProbation, StateActive, StateDegraded:
		return true
	}
	return false
}

// IsTerminal returns true if the capability has reached end-of-life.
// Retired is permanently dead; rolled_back retains lineage for the
// audit diff but is no longer a working artifact.
func (s CapabilityState) IsTerminal() bool {
	switch s {
	case StateRetired, StateRolledBack:
		return true
	}
	return false
}

// ExecutionDomain is the trust level under which a capability runs.
// See §3.3 / §7.2 of the spec for the earned-trust ladder.
type ExecutionDomain string

const (
	DomainSandbox    ExecutionDomain = "sandbox"
	DomainRestricted ExecutionDomain = "restricted"
	DomainTrusted    ExecutionDomain = "trusted"
	DomainOperator   ExecutionDomain = "operator"
)

// AllExecutionDomains returns every legal domain.
func AllExecutionDomains() []ExecutionDomain {
	return []ExecutionDomain{DomainSandbox, DomainRestricted, DomainTrusted, DomainOperator}
}

// Validate returns an error if d is not a recognized execution domain.
func (d ExecutionDomain) Validate() error {
	for _, allowed := range AllExecutionDomains() {
		if d == allowed {
			return nil
		}
	}
	return fmt.Errorf("capability: invalid execution_domain %q (must be one of %v)", d, AllExecutionDomains())
}

// EventType discriminates rows in capability_events. See §2.4.1 of the
// spec for the event_type taxonomy and required fields.
type EventType string

const (
	EventPromotion          EventType = "promotion"
	EventDemotion           EventType = "demotion"
	EventFracture           EventType = "fracture"
	EventRollback           EventType = "rollback"
	EventDependencyShatter  EventType = "dependency_shatter"
	EventRetirement         EventType = "retirement"
	EventSupersede          EventType = "supersede"
	EventSourceHashMismatch EventType = "source_hash_mismatch"
	// EventOperatorApproval is stamped by `mpm capability grant-operator`.
	// It is the audit-trail companion to metadata.operator_approved_at
	// — the gate the executor checks at invoke-time is the metadata
	// (int64 unix epoch, see executor.go int64FromMeta), but the event
	// row preserves the human-readable RFC3339 timestamp + actor for
	// `mpm skill audit <id>` consumption. Both writes happen in the
	// same transaction (mark + event) so the audit trail can never
	// disagree with the gate.
	EventOperatorApproval EventType = "operator_approval"
)

// AllEventTypes returns every legal event_type.
func AllEventTypes() []EventType {
	return []EventType{
		EventPromotion, EventDemotion, EventFracture, EventRollback,
		EventDependencyShatter, EventRetirement, EventSupersede,
		EventSourceHashMismatch, EventOperatorApproval,
	}
}

// Validate returns an error if e is not a recognized event type.
func (e EventType) Validate() error {
	for _, allowed := range AllEventTypes() {
		if e == allowed {
			return nil
		}
	}
	return fmt.Errorf("capability: invalid event_type %q (must be one of %v)", e, AllEventTypes())
}
