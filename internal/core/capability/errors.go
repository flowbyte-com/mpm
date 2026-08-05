package capability

import "errors"

// Typed errors used across the capability subsystem. Handlers check via
// errors.Is; tests assert via errors.Is; the CLI/MCP surface translates
// to user-facing rejection messages.
//
// Each error maps to a specific failure mode in the spec; adding a new
// error means adding a new failure path. Keep this list short and
// load-bearing.
var (
	// ErrInvalidTransition: the requested state transition is not in
	// the lifecycle matrix. The forge refuses to write a row whose
	// transition CapabilityState.CanTransitionTo() rejects.
	ErrInvalidTransition = errors.New("capability: invalid state transition")

	// ErrNotFound: capability ID does not exist (or is soft-deleted).
	// The "live version" lookup may surface this when no row matches
	// the (state='active', superseded_by_id IS NULL) filter.
	ErrNotFound = errors.New("capability: not found")

	// ErrAlreadyExists: a capability with this name already exists in
	// a non-retired state. Proposals that collide on name are rejected
	// unless they declare created_from_id or replaces_id (revision
	// or fork paths).
	ErrAlreadyExists = errors.New("capability: name already exists")

	// ErrSourceHashMismatch: the on-disk source code's SHA-256 does
	// not match capabilities.source_hash. Triggers a fracture
	// transition and a source_hash_mismatch event.
	ErrSourceHashMismatch = errors.New("capability: source hash mismatch")

	// ErrSourceTooLarge: source_code exceeds the 64KB / 500-line cap
	// enforced by the forge before any linter/scanner work runs.
	ErrSourceTooLarge = errors.New("capability: source code exceeds size cap")

	// ErrScannerRejected: the source-code scanner refused the proposal
	// (matched a poison pattern in §7.1 of the spec). The specific
	// pattern is wrapped via fmt.Errorf with %w.
	ErrScannerRejected = errors.New("capability: scanner rejected proposal")

	// ErrLinterFailed: shellcheck / py_compile / ruff rejected the
	// source. Lint errors are wrapped via fmt.Errorf with %w.
	ErrLinterFailed = errors.New("capability: linter rejected proposal")

	// ErrDependencyDead: depends_on references a capability that is
	// not in state='active'. The forge refuses the proposal rather
	// than silently allowing a build on a broken foundation.
	ErrDependencyDead = errors.New("capability: dependency is not active")

	// ErrDedupMatch: the proposal's purpose embedding scored above the
	// dedup threshold (default 0.92) against an existing capability.
	// The matched capability ID is wrapped; the agent is forced to
	// use the established tool or declare replaces_id to fork.
	ErrDedupMatch = errors.New("capability: duplicate of existing capability")

	// ErrDomainPolicyViolation: the requested execution_domain cannot
	// be granted at the current state. A draft requesting 'trusted'
	// is the canonical example — trust is earned, not granted.
	ErrDomainPolicyViolation = errors.New("capability: domain policy violation")

	// ErrPredecessorBroken: rollback revival target is not in
	// state='retired' (it's fractured, rolled_back, or otherwise
	// unusable). Triggers the "both broken" escalation path in
	// §5.2.1 of the spec.
	ErrPredecessorBroken = errors.New("capability: rollback target is broken")

	// ErrNotLive: the requested capability exists but is not in a
	// callable state (state='active', 'probation', or 'degraded') OR
	// has been superseded by a newer revision (superseded_by_id IS NOT NULL).
	// Executor.Invoke refuses any call to a non-live capability; this
	// is the runtime guard against invoking retired/draft/rolled_back rows.
	ErrNotLive = errors.New("capability: not live")

	// ErrOperatorNotApproved: the capability declares execution_domain='operator'
	// but metadata.operator_approved_at is missing. The Executor refuses to
	// dispatch an operator-domain capability unless a human has explicitly
	// stamped the approval timestamp via `mpm capability grant-operator`.
	// This is the single mechanical lock that prevents the CLI/MCP layer
	// from promoting a capability in memory and executing it.
	ErrOperatorNotApproved = errors.New("capability: operator approval missing")

	// ErrTelemetryFailed: the capability_invocations row write (or its
	// co-transactional counter update) failed. Executor.Invoke refuses to
	// return a successful Result when telemetry is unwritable — an
	// unrecorded action is worse than a failed action in an epistemic
	// system. The underlying SQLite error is wrapped via fmt.Errorf %w.
	ErrTelemetryFailed = errors.New("capability: telemetry write failed")
)
