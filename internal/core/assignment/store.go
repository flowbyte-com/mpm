package assignment

import (
	"crypto/rand"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	internal "github.com/flowbyte-com/mpm-core"
)

// DB is the narrow surface the Store needs.
//
// *internal.DatabaseManager and internal.CoreDB both satisfy it. The
// narrow interface keeps the service testable and keeps it from
// depending on the whole substrate.
type DB interface {
	// WithTx runs fn inside one transaction. The DDL, the state change
	// and the ledger entry share it, so a transition is atomic with its
	// event.
	WithTx(fn func(internal.DBNode) error) error
	// SQLDB is the underlying pool, used for the reads that do not
	// need a transaction.
	SQLDB() *sql.DB
}

// Clock is the time source the Store uses for every timestamp it writes.
//
// Production uses time.Now().UTC(). Tests inject a frozen or
// fast-forwarded clock, so lease expiry and reclaim are observed rather
// than waited for. No test sleeps.
type Clock func() time.Time

func realClock() time.Time { return time.Now().UTC() }

// Store is the assignment coordination service.
//
// It is safe for concurrent use: every write runs inside a transaction,
// and the fence is expressed in the WHERE clause of the writing statement
// rather than in a read-then-write pair.
type Store struct {
	db    DB
	clock Clock

	// fault is a TEST SEAM. When non-nil it is called with a stage name
	// immediately before the corresponding write, and a non-nil error
	// aborts the transaction. It exists so the suite can prove that a
	// transition and its ledger entry roll back TOGETHER.
	//
	// It is unexported, so only this package's tests can set it, and
	// production never has it non-nil.
	fault func(stage string) error
}

// NewStore returns a Store backed by db with the production clock.
func NewStore(db DB) *Store { return NewStoreWithClock(db, realClock) }

// NewStoreWithClock returns a Store with an injected clock.
func NewStoreWithClock(db DB, clock Clock) *Store {
	if clock == nil {
		clock = realClock
	}
	return &Store{db: db, clock: clock}
}

// Now is the service's current time in unix seconds.
func (s *Store) Now() int64 { return nowUnix(s.clock()) }

// ── identifiers ───────────────────────────────────────────────────────────

// newID mints an identifier in the substrate's convention: a short prefix
// and sixteen lowercase hex characters.
//
// The prefix says what the row is, so a human reading a log can tell an
// assignment from an event without a schema lookup.
func newID(prefix string) (string, error) {
	var b [8]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "", fmt.Errorf("assignment: mint an id: %w", err)
	}
	return prefix + hex.EncodeToString(b[:]), nil
}

// ── reading a row ─────────────────────────────────────────────────────────

// assignmentColumns is the column list every assignment SELECT uses, in
// the order scanAssignment expects.
const assignmentColumns = `id, project_id, root_id, COALESCE(parent_id,''), depth,
	title, objective, recipient, role, state, idempotency_key,
	claim_epoch, COALESCE(owner,''), lease_expires_at,
	permitted_operations, eligible_recipients,
	COALESCE(max_depth,0), COALESCE(max_tasks,0), COALESCE(max_concurrency,0),
	COALESCE(max_resource_units,0), resource_units,
	COALESCE(issue_ref,''), COALESCE(pr_ref,''), COALESCE(base_sha,''),
	COALESCE(head_sha,''), COALESCE(reviewed_sha,''), COALESCE(findings_sha256,''),
	COALESCE(evidence_ref,''),
	COALESCE(awaiting_action,''), COALESCE(approval_action,''),
	COALESCE(approval_revision,''), COALESCE(approval_source,''),
	COALESCE(approval_evidence,''), approved_at,
	payload, created_at, updated_at, completed_at`

// scanAssignment reads one row selected with assignmentColumns.
func scanAssignment(scan func(dest ...interface{}) error) (*Assignment, error) {
	var (
		a           Assignment
		lease       sql.NullInt64
		approvedAt  sql.NullInt64
		completedAt sql.NullInt64
		permitted   string
		eligible    string
		payload     string
	)
	if err := scan(
		&a.ID, &a.ProjectID, &a.RootID, &a.ParentID, &a.Depth,
		&a.Title, &a.Objective, &a.Recipient, &a.Role, &a.State,
		&a.IdempotencyKey, &a.ClaimEpoch, &a.Owner, &lease,
		&permitted, &eligible,
		&a.MaxDepth, &a.MaxTasks, &a.MaxConcurrency, &a.MaxResourceUnits,
		&a.ResourceUnits,
		&a.IssueRef, &a.PRRef, &a.BaseSHA, &a.HeadSHA, &a.ReviewedSHA,
		&a.FindingsSHA256, &a.EvidenceRef,
		&a.AwaitingAction, &a.ApprovalAction, &a.ApprovalRevision,
		&a.ApprovalSource, &a.ApprovalEvidence, &approvedAt,
		&payload, &a.CreatedAt, &a.UpdatedAt, &completedAt,
	); err != nil {
		return nil, err
	}
	if lease.Valid {
		v := lease.Int64
		a.LeaseExpiresAt = &v
	}
	if approvedAt.Valid {
		v := approvedAt.Int64
		a.ApprovedAt = &v
	}
	if completedAt.Valid {
		v := completedAt.Int64
		a.CompletedAt = &v
	}
	a.PermittedOperations = unmarshalStringList(permitted)
	a.EligibleRecipients = unmarshalStringList(eligible)
	a.Payload = unmarshalPayload(payload)
	return &a, nil
}

// getAssignment reads one assignment outside a transaction.
func (s *Store) getAssignment(id string) (*Assignment, error) {
	row := s.db.SQLDB().QueryRow(
		`SELECT `+assignmentColumns+` FROM agent_tasks WHERE id = ?`, id)
	a, err := scanAssignment(row.Scan)
	if err == sql.ErrNoRows {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, fmt.Errorf("assignment: read %s: %w", id, err)
	}
	return a, nil
}

// getAssignmentTx reads one assignment inside a transaction.
func getAssignmentTx(node internal.DBNode, id string) (*Assignment, error) {
	row := node.QueryRowTracked(
		`SELECT `+assignmentColumns+` FROM agent_tasks WHERE id = ?`, id)
	a, err := scanAssignment(row.Scan)
	if err == sql.ErrNoRows {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, fmt.Errorf("assignment: read %s: %w", id, err)
	}
	return a, nil
}

// findByIdempotencyKey reads the assignment that carries the key, or
// nil.
//
// PR 1 found that the before-insert trigger fires BEFORE the conflict
// resolution, so `ON CONFLICT DO NOTHING` ABORTS rather than resolving
// the duplicate. The service therefore SELECTS the existing row instead
// of relying on the conflict resolution.
func findByIdempotencyKey(node internal.DBNode, key string) (*Assignment, error) {
	row := node.QueryRowTracked(
		`SELECT `+assignmentColumns+` FROM agent_tasks WHERE idempotency_key = ?`, key)
	a, err := scanAssignment(row.Scan)
	if err == sql.ErrNoRows {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("assignment: read the idempotency key: %w", err)
	}
	return a, nil
}

// projectExistsTx reports whether the project row exists.
//
// It is a defensive read: the schema's foreign key is the real
// enforcement, and this read exists only so the refusal names the
// project that is missing.
func projectExistsTx(node internal.DBNode, projectID string) (bool, error) {
	var n int
	if err := node.QueryRowTracked(
		`SELECT COUNT(*) FROM projects WHERE id = ?`, projectID).Scan(&n); err != nil {
		return false, fmt.Errorf("assignment: read the project %q: %w", projectID, err)
	}
	return n > 0, nil
}

// ── the ledger ────────────────────────────────────────────────────────────

// appendEvent appends one ledger entry and returns it.
//
// The index is MAX(event_index)+1, which is strictly greater than the
// current maximum — PR 1's monotonicity rule. The event is written in the
// SAME transaction as the state change it records, so there is no window
// in which one landed and the other did not.
func appendEvent(
	node internal.DBNode,
	taskID string,
	eventType string,
	fromState string,
	toState string,
	actor Actor,
	claimEpoch *int64,
	note string,
	payload map[string]interface{},
	now int64,
) (*Event, error) {
	id, err := newID("ate-")
	if err != nil {
		return nil, err
	}

	var index int64
	if err := node.QueryRowTracked(
		`SELECT COALESCE(MAX(event_index), -1) + 1 FROM agent_task_events WHERE task_id = ?`,
		taskID,
	).Scan(&index); err != nil {
		return nil, fmt.Errorf("assignment: read the ledger index: %w", err)
	}

	payloadJSON := "{}"
	if payload != nil {
		b, err := json.Marshal(payload)
		if err != nil {
			return nil, fmt.Errorf("assignment: encode the event payload: %w", err)
		}
		payloadJSON = string(b)
	}

	if _, err := node.ExecTracked(`
		INSERT INTO agent_task_events (
			id, task_id, event_index, event_type, from_state, to_state,
			actor_address, actor_kind, claim_epoch, note, payload, created_at
		) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		5,
		id, taskID, index, eventType, nullIfEmpty(fromState), nullIfEmpty(toState),
		nullIfEmpty(actor.Address), nullIfEmpty(actor.Kind), claimEpoch,
		nullIfEmpty(note), payloadJSON, now,
	); err != nil {
		return nil, fmt.Errorf("assignment: append the %s event: %w", eventType, err)
	}

	return &Event{
		ID:           id,
		TaskID:       taskID,
		EventIndex:   index,
		EventType:    eventType,
		FromState:    fromState,
		ToState:      toState,
		ActorAddress: actor.Address,
		ActorKind:    actor.Kind,
		ClaimEpoch:   claimEpoch,
		Note:         note,
		Payload:      payload,
		CreatedAt:    now,
	}, nil
}

// nullIfEmpty maps the empty string to SQL NULL, so an absent
// attribution is absent rather than an empty claim.
func nullIfEmpty(s string) interface{} {
	if s == "" {
		return nil
	}
	return s
}

// ── the fault seam ────────────────────────────────────────────────────────

// injectFault calls the test seam, if one is installed.
func (s *Store) injectFault(stage string) error {
	if s.fault == nil {
		return nil
	}
	return s.fault(stage)
}

// errSkipLookup is the sentinel a fault seam returns to make the
// service SKIP the idempotency lookup.
//
// It simulates the SCHEDULE, not a broken invariant: the duplicate is
// not yet visible to the lookup, because the other party's transaction
// has not committed. That is the schedule the read-back exists to
// resolve, and it is otherwise reachable only by winning a race.
var errSkipLookup = errors.New("assignment: skip the idempotency lookup")

// findExistingTx reads the assignment that carries the key, or nil.
//
// It consults the fault seam first, so a test can reproduce the
// schedule in which the duplicate is not yet visible without having to
// win a race to do it.
func (s *Store) findExistingTx(node internal.DBNode, key string) (*Assignment, error) {
	if s.fault != nil {
		if err := s.fault("idempotency-lookup"); err != nil {
			if errors.Is(err, errSkipLookup) {
				return nil, nil
			}
			return nil, err
		}
	}
	return findByIdempotencyKey(node, key)
}

// ── validation helpers ────────────────────────────────────────────────────

// requireText refuses an empty or blank required field.
func requireText(what, value string) error {
	if strings.TrimSpace(value) == "" {
		return fmt.Errorf("%w: %s is required", ErrConstraint, what)
	}
	return nil
}

// storageRefusal classifies a storage error as a refusal by the schema.
//
// The storage's own message names the RULE that refused the write, and
// that message is more useful than anything this package could invent —
// so it is preserved.
func storageRefusal(err error) error {
	if err == nil {
		return nil
	}
	msg := err.Error()
	for _, marker := range []string{
		"agent_tasks:", "agent_task_events:", "UNIQUE constraint",
		"CHECK constraint", "FOREIGN KEY constraint", "NOT NULL constraint",
	} {
		if strings.Contains(msg, marker) {
			return fmt.Errorf("%w: %s", ErrRefused, msg)
		}
	}
	return err
}
