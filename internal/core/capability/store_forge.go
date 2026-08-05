package capability

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"strings"

	"github.com/google/uuid"

	internal "github.com/flowbyte-com/mpm-core"
)

// =============================================================================
// store_forge.go — write methods for the Forge pipeline
//
// InsertCapabilityProposal is the load-bearing atomic transaction of the
// forge: capabilities row + capability_dependencies rows + validation,
// all-or-nothing. The Store assigns the capability ID (UUID v4) so the
// caller never has to think about ID uniqueness.
//
// Initial state is always 'draft'. The validation pipeline
// (lint → validate → probation) lives in higher layers and transitions
// the row via the lifecycle methods (see store_lifecycle.go).
// =============================================================================

// SourceCodeLimits bound the forge pipeline. The spec §7.4 calls these
// "defense in depth" — bwrap is the actual security boundary, but
// keeping the source small is the fast-fail before any expensive
// lint/scan work runs.
const (
	MaxSourceBytes = 64 * 1024 // 64 KiB
	MaxSourceLines = 500
)

// Proposal is the forge input shape. It carries everything the Store
// needs to atomically insert a capability; nothing else.
type Proposal struct {
	Name           string
	Purpose        string
	SourceCode     string
	SourceLanguage string
	Tags           StringSlice

	// RequestedDomain is the domain the proposal asks to run in. Trust
	// is earned, not granted — if the requested domain exceeds what a
	// draft is allowed to ask for (sandbox only, by default), the
	// Store returns ErrDomainPolicyViolation. The lifecycle methods
	// re-validate this on every promotion.
	RequestedDomain ExecutionDomain

	// DependsOn are the upstream capability IDs the new tool needs.
	// Every entry must currently be in state='active'; otherwise the
	// Store returns ErrDependencyDead. Empty slice is fine.
	DependsOn []string

	// Lineage hooks. Both nil-able.
	//
	// AuthorTheoryID: the memory id of the theory (collection='theories')
	// that this proposal realizes. Optional.
	//
	// CreatedFromID: for revisions of an existing capability. When set,
	// the new row's created_from_id points back; the existing row's
	// superseded_by_id is set on promotion. Use exactly ONE of
	// CreatedFromID (revision) or ReplacesID (fork).
	//
	// ReplacesID: for forks. Per spec §3.6 the fork's name must differ;
	// the Store enforces name uniqueness via the UNIQUE constraint.
	AuthorTheoryID *string
	AuthorAgent    string
	CreatedFromID  *string
	ReplacesID     *string

	// InitialState is the state the new row is inserted with. Default
	// is StateDraft. Spec §3.2 step 11 lets the Forge insert directly
	// with StateValidated (when the dry-run passed) or StateLinted
	// (when the dry-run was skipped) — the lifecycle methods still
	// govern subsequent transitions. The Store rejects any value
	// outside {draft, linted, validated} — an agent can never insert
	// directly into active or beyond.
	InitialState CapabilityState

	// Probation overrides. If both zero, the Store falls back to the
	// table defaults (5 successes, 10% failure rate).
	ProbationRequiredSuccessCount int
	ProbationMaxFailureRate       float64

	// Metadata is free-form JSON attached to the capability row.
	// Optional. Stamped at proposal; mutation after that is the
	// job of the lifecycle methods (rare; mostly for `escalated`).
	Metadata CapabilityMetadata
}

// validInitialStates is the set of states a Proposal may request
// on insert. Excludes 'active' and beyond — those require a
// promotion through the lifecycle methods, never a direct insert.
var validInitialStates = map[CapabilityState]bool{
	StateDraft:     true,
	StateLinted:    true,
	StateValidated: true,
}

// validate runs the forge's pre-insertion checks. These mirror spec
// §3.2 "validation pipeline (in order, atomic)" but the Store only
// owns the parts that need DB access (name collision, dep liveness)
// plus the structural checks (size cap, domain legality).
//
// Higher-level checks (scanner, linter, embedding dedup) live in
// the Forge handler — they need the LLM and the embedding provider
// which the Store deliberately does not depend on.
func (p *Proposal) validate() error {
	if strings.TrimSpace(p.Name) == "" {
		return fmt.Errorf("capability: proposal name is required")
	}
	if strings.TrimSpace(p.Purpose) == "" {
		return fmt.Errorf("capability: proposal purpose is required")
	}
	if p.InitialState == "" {
		p.InitialState = StateDraft
	}
	if !validInitialStates[p.InitialState] {
		return fmt.Errorf("%w: initial state %q is not allowed (must be draft, linted, or validated)",
			ErrInvalidTransition, p.InitialState)
	}
	if p.SourceCode == "" {
		return fmt.Errorf("capability: proposal source_code is required")
	}
	if int64(len(p.SourceCode)) > MaxSourceBytes {
		return fmt.Errorf("%w: %d bytes (cap %d)",
			ErrSourceTooLarge, len(p.SourceCode), MaxSourceBytes)
	}
	if lines := strings.Count(p.SourceCode, "\n") + 1; lines > MaxSourceLines {
		return fmt.Errorf("%w: %d lines (cap %d)",
			ErrSourceTooLarge, lines, MaxSourceLines)
	}
	if p.SourceLanguage == "" {
		p.SourceLanguage = "bash" // matches column DEFAULT
	}
	if err := p.RequestedDomain.Validate(); err != nil {
		return fmt.Errorf("capability: proposal: %w", err)
	}
	// Drafts can only request sandbox. Trust is earned in the
	// lifecycle, not granted at proposal time. The Store enforces
	// this here; the forge handler does the same check at the
	// policy layer so users get a friendlier error message.
	if p.RequestedDomain != DomainSandbox {
		return fmt.Errorf("%w: drafts may only request sandbox",
			ErrDomainPolicyViolation)
	}
	return nil
}

// hashSource returns the hex-encoded SHA-256 of the source. This is
// the on-disk fingerprint stored in capabilities.source_hash and
// re-checked on every invocation by the Executor (spec §4.2).
func hashSource(src string) string {
	sum := sha256.Sum256([]byte(src))
	return hex.EncodeToString(sum[:])
}

// InsertCapabilityProposal atomically:
//   1. Validates the proposal (size, domain, deps liveness)
//   2. Assigns a UUID v4 to the new capability
//   3. Inserts the capabilities row in state='draft'
//   4. Inserts capability_dependencies rows for every entry in
//      Proposal.DependsOn
//
// All four steps happen inside one Store.withTx; any failure rolls
// back. Returns ErrAlreadyExists on name collision, ErrDependencyDead
// if any dep is not in state='active', ErrDomainPolicyViolation if
// the requested domain exceeds the draft allowance.
func (s *Store) InsertCapabilityProposal(p *Proposal) (string, error) {
	return s.InsertCapabilityProposalWithID(p, uuid.New().String())
}

// InsertCapabilityProposalWithID is the same as InsertCapabilityProposal
// but uses the supplied id instead of generating a UUID. Seed paths
// (CS-2: `mpm capability seed`) need a deterministic id so the
// lookup-or-insert pattern is idempotent — a fresh UUID per call
// would defeat the seed engine's "Skipped vs Drifted" triage.
//
// The id MUST be unique across the capabilities table (it's the PK).
// The seed engine uses the canonical "cap.<name>" convention from
// SeedCapability.SavedID() so the on-disk id is stable across seed
// re-runs. Operators who call this directly should pick their own
// id scheme; collisions return ErrAlreadyExists via the same UNIQUE
// detection as InsertCapabilityProposal.
//
// All other contracts (validate, deps, deps-active pre-flight, tx
// wrapping, audit fields) match InsertCapabilityProposal exactly.
func (s *Store) InsertCapabilityProposalWithID(p *Proposal, id string) (string, error) {
	if p == nil {
		return "", fmt.Errorf("capability: nil proposal")
	}
	if id == "" {
		return "", fmt.Errorf("capability: id is required")
	}
	if err := p.validate(); err != nil {
		return "", err
	}

	// Pre-flight: every depends_on_id must currently be in state='active'.
	// Done outside the transaction so we can return a clean error path
	// without polluting the audit log with a failed-write event.
	if len(p.DependsOn) > 0 {
		if err := s.checkDependenciesActive(p.DependsOn); err != nil {
			return "", err
		}
	}

	now := s.Now()
	sourceHash := hashSource(p.SourceCode)

	err := s.withTx(func(n internal.DBNode) error {
		// 1. Insert the capability row.
		if err := s.insertCapabilityRow(n, id, p, sourceHash, now); err != nil {
			return err
		}
		// 2. Insert dependency rows.
		if err := s.insertDependencyRows(n, id, p.DependsOn, now); err != nil {
			return err
		}
		return nil
	})
	if err != nil {
		return "", err
	}
	return id, nil
}

// checkDependenciesActive verifies that every id in deps currently
// exists, is not soft-deleted, and is in state='active'. Returns
// ErrDependencyDead wrapping the first dead id found (so the operator
// can see which dependency is the blocker).
func (s *Store) checkDependenciesActive(deps []string) error {
	if len(deps) == 0 {
		return nil
	}
	// Build IN clause with N placeholders.
	placeholders := make([]string, len(deps))
	args := make([]interface{}, len(deps))
	for i, d := range deps {
		placeholders[i] = "?"
		args[i] = d
	}
	q := `SELECT id FROM capabilities
	      WHERE id IN (` + strings.Join(placeholders, ",") + `)
	        AND state = 'active'
	        AND deleted_at IS NULL`

	rows, err := s.dm.QueryTracked(q, args...)
	if err != nil {
		return wrapDBError("checkDependenciesActive", err)
	}
	defer rows.Close()

	live := make(map[string]bool, len(deps))
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			return wrapDBError("checkDependenciesActive", err)
		}
		live[id] = true
	}
	if err := rows.Err(); err != nil {
		return wrapDBError("checkDependenciesActive", err)
	}

	for _, d := range deps {
		if !live[d] {
			return fmt.Errorf("%w: %s", ErrDependencyDead, d)
		}
	}
	return nil
}

// insertCapabilityRow writes the capabilities row. Helper used only
// inside InsertCapabilityProposal's transaction.
func (s *Store) insertCapabilityRow(
	n internal.DBNode,
	id string,
	p *Proposal,
	sourceHash string,
	now int64,
) error {
	// Resolve probation overrides (zero means use table default).
	reqSuccess := p.ProbationRequiredSuccessCount
	if reqSuccess <= 0 {
		reqSuccess = 5
	}
	maxFailRate := p.ProbationMaxFailureRate
	if maxFailRate <= 0 {
		maxFailRate = 0.10
	}

	q := `INSERT INTO capabilities (
		id, name, purpose, source_code, source_language, source_hash,
		state, execution_domain, state_changed_at,
		author_theory_id, author_agent, created_from_id,
		probation_required_success_count, probation_max_failure_rate,
		tags, created_at, updated_at, metadata
	) VALUES (
		?, ?, ?, ?, ?, ?,
		?, ?, ?,
		?, ?, ?,
		?, ?,
		?, ?, ?, ?
	)`

	if _, err := n.ExecTracked(q, 3,
		id, p.Name, p.Purpose, p.SourceCode, p.SourceLanguage, sourceHash,
		p.InitialState, p.RequestedDomain, now,
		p.AuthorTheoryID, p.AuthorAgent, p.CreatedFromID,
		reqSuccess, maxFailRate,
		p.Tags, now, now, p.Metadata,
	); err != nil {
		// UNIQUE violation on name. Detect via the constraint message
		// because the mattn driver does not wrap to a typed error.
		if isUniqueViolation(err, "name") {
			return fmt.Errorf("%w: %s", ErrAlreadyExists, p.Name)
		}
		return wrapDBError("insertCapabilityRow", err)
	}
	return nil
}

// insertDependencyRows writes one row per entry in deps. Helper used
// only inside InsertCapabilityProposal's transaction.
//
// Note: ON DELETE CASCADE on the FKs means dependency rows auto-vacuum
// when a referenced capability is hard-deleted. Soft-delete via
// deleted_at does not trigger CASCADE — that's by design, so retired
// rows can still appear in lineage walks.
func (s *Store) insertDependencyRows(
	n internal.DBNode,
	capabilityID string,
	deps []string,
	now int64,
) error {
	if len(deps) == 0 {
		return nil
	}
	q := `INSERT INTO capability_dependencies
	      (capability_id, depends_on_id, added_at)
	      VALUES (?, ?, ?)`
	for _, d := range deps {
		if _, err := n.ExecTracked(q, 3, capabilityID, d, now); err != nil {
			return wrapDBError("insertDependencyRows", err)
		}
	}
	return nil
}

// isUniqueViolation is a tiny helper to spot SQLite UNIQUE constraint
// failures without depending on error-type wrapping (which the mattn
// driver does not do consistently across versions). Matches the
// "UNIQUE constraint failed: <column>" message format.
func isUniqueViolation(err error, column string) bool {
	if err == nil {
		return false
	}
	msg := err.Error()
	// Common forms:
	//   "UNIQUE constraint failed: capabilities.name"
	//   "constraint failed: UNIQUE constraint failed: capabilities.name"
	return strings.Contains(msg, "UNIQUE constraint failed") &&
		strings.Contains(msg, column)
}

// (end of file — no package-level state)
