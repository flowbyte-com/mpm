package assignment

import (
	"errors"
	"fmt"
	"strings"

	internal "github.com/flowbyte-com/mpm-core"
)

// The operator-authorised recovery supersession.
//
// A tree with an unresolved ambiguous attempt is a hard execution barrier:
// no new launch may start merely because a lease expired or a new claim
// epoch exists. Supersession is the ONLY way forward, and it is an
// operator-only, lineage-first transaction: the lineage row names the
// blocked original, the exact ambiguous attempt, and the two fresh
// assignments that continue the work, before either of them exists.
//
// NOTHING about the original changes. Its state, epoch, owner, lease and
// ledger stay exactly as they are; its ambiguity stays recorded. The
// replacement and the reviewer are NEW assignments: they carry their own
// identities, they start unclaimed at epoch zero, and they inherit the
// objective only through the operator's explicit parameters — not by
// copying the original's row.

// Supersession is one row of agent_task_supersessions, projected.
type Supersession struct {
	ID                string `json:"id"`
	ProjectID         string `json:"project_id"`
	RootID            string `json:"root_id"`
	SupersededTaskID  string `json:"superseded_task_id"`
	ReplacementTaskID string `json:"replacement_task_id"`
	ReviewerTaskID    string `json:"reviewer_task_id"`
	AttemptID         string `json:"attempt_id"`
	AuthorisedBy      string `json:"authorised_by"`
	AuthorityKind     string `json:"authority_kind"`
	EvidenceRef       string `json:"evidence_ref"`
	Justification     string `json:"justification"`
	CreatedAt         int64  `json:"created_at"`
}

// supersessionColumns is the column list every supersession SELECT uses,
// in the order scanSupersession expects.
const supersessionColumns = `id, project_id, root_id, superseded_task_id,
	replacement_task_id, reviewer_task_id, attempt_id,
	authorised_by, authority_kind, evidence_ref, justification, created_at`

// SupersedeTaskParams describes one of the two fresh assignments a
// supersession creates.
type SupersedeTaskParams struct {
	// Title, Objective and Recipient are the operator's explicit
	// authorisation. They are not inherited and not compared against the
	// original: the lineage is the binding, not the prose.
	Title     string
	Objective string
	Recipient string
	// ResourceUnits is the new assignment's declared reservation against
	// the root's cumulative budget.
	ResourceUnits int64
	// BaseSHA is the exact revision the replacement implements. It is
	// required for the replacement and must be empty for the reviewer.
	BaseSHA string
}

// SupersedeParams describes an operator-authorised recovery.
type SupersedeParams struct {
	// SupersededID is the blocked original. It must be a non-root
	// implementer with exactly one ambiguous attempt and no
	// intent/launched/running attempt.
	SupersededID string
	// AttemptID is the exact ambiguous attempt the lineage names.
	AttemptID string
	// Epoch is the claim epoch the operator read. The write is fenced on
	// it, so a supersession authorised against a moved claim does not
	// land.
	Epoch int64
	// Replacement and Reviewer are the two fresh assignments. The
	// reviewer examines the replacement's work, never the original's.
	Replacement SupersedeTaskParams
	Reviewer    SupersedeTaskParams
	// EvidenceRef names where the authorisation is recorded. Prefix-only
	// with a length floor, the same contract as the review evidence
	// pointer. Presence is not proof the original inference did not
	// happen.
	EvidenceRef string
	// Justification says why the original cannot proceed and what the
	// replacement is for. It is part of the idempotency comparison.
	Justification string
	// RequireQuiescence extends the guarded write with the live-claim
	// predicate: no unfinished task in the tree may hold a live lease.
	// The pilot always sets it; a caller that does not is explicitly
	// accepting concurrent live work elsewhere in the tree.
	RequireQuiescence bool
}

// SupersedeResult is what one supersession produced.
type SupersedeResult struct {
	Lineage     *Supersession `json:"lineage"`
	Replacement *Assignment   `json:"replacement"`
	Reviewer    *Assignment   `json:"reviewer"`
	// Created is false when the request repeated an identical
	// authorisation: the existing lineage and assignments are returned.
	Created bool `json:"created"`
}

// supersedeIDs derives the immutable reserved idempotency keys for the
// two fresh assignments from the named attempt.
func supersedeIDs(attemptID string) (replacement, reviewer string) {
	return "supersede:" + attemptID, "supersede:" + attemptID + ":review"
}

// Supersede records an operator-authorised recovery and creates the
// replacement and reviewer that continue the work.
//
// It is an operator operation: it requires an OperatorAuthority, and it
// is absent from the tool registry. The guarded lineage write is the
// decisive authorisation: exactly one affected row commits the whole
// transaction, and zero rows abort it without writing anything.
func (s *Store) Supersede(auth OperatorAuthority, p SupersedeParams) (*SupersedeResult, error) {
	if auth == nil {
		return nil, fmt.Errorf("%w: supersession is an operator operation", ErrNotAuthorised)
	}
	actor := auth.Actor()
	if err := validateSupersedeParams(p); err != nil {
		return nil, err
	}

	// Idempotency FIRST, on committed state: a repeated authorisation
	// returns the existing lineage rather than creating a second one.
	if existing, err := s.findSupersessionTx(p.SupersededID, p.AttemptID); err != nil {
		return nil, err
	} else if existing != nil {
		return s.matchSupersession(existing, p, actor)
	}
	// A reserved key occupied by a row no lineage names is a legacy
	// collision: the rows are never rewritten or adopted.
	if err := s.checkReservedKeyCollision(p.AttemptID); err != nil {
		return nil, err
	}

	var out *SupersedeResult
	var created bool
	err := s.db.WithTx(func(node internal.DBNode) error {
		// The replacement and reviewer identities are minted BEFORE
		// the lineage: TR6 requires the lineage to name fresh,
		// distinct, well-formed ids, TR1 forbids ever updating them,
		// and the deferred composite FKs check them at COMMIT. The
		// lineage names rows created later in the same transaction,
		// so all three ids are fixed before any of them is written.
		replacementID, err := newID("at-")
		if err != nil {
			return err
		}
		reviewerID, err := newID("at-")
		if err != nil {
			return err
		}
		// The guarded lineage insert is the decisive authorisation: one
		// row or nothing. Its WHERE clause re-checks every preflight
		// condition atomically with the write, so a claim, a state move
		// or a new attempt between preflight and commit aborts the
		// whole transaction instead of authorising stale work.
		id, err := s.insertSupersessionTx(node, p, actor, replacementID, reviewerID)
		if err != nil {
			return err
		}
		_ = id
		// Fault point between the lineage and the children (T38): the
		// whole transaction must roll back, leaving no lineage, no
		// replacement, no reviewer and no events.
		if err := s.injectFault("supersede-lineage"); err != nil {
			return err
		}
		replacement, err := s.insertSupersededChildTx(node, p, replacementID, true)
		if err != nil {
			return err
		}
		reviewer, err := s.insertSupersededChildTx(node, p, reviewerID, false)
		if err != nil {
			return err
		}
		now := s.Now()
		if _, err := appendEvent(node, replacement.ID, EventCreated, "", StateEnqueued, actor, nil,
			"the replacement was created by an authorised supersession", nil, now); err != nil {
			return err
		}
		if _, err := appendEvent(node, reviewer.ID, EventCreated, "", StateEnqueued, actor, nil,
			"the reviewer was created by an authorised supersession", nil, now); err != nil {
			return err
		}
		lin, err := getSupersessionTx(node, id)
		if err != nil {
			return err
		}
		out = &SupersedeResult{Lineage: lin, Replacement: replacement, Reviewer: reviewer}
		created = true
		return nil
	})
	if err != nil {
		// A concurrent supersession won the UNIQUE race: re-read and
		// compare rather than reporting a duplicate.
		if isUniqueViolation(err) {
			if existing, rerr := s.findSupersessionTx(p.SupersededID, p.AttemptID); rerr != nil {
				return nil, rerr
			} else if existing != nil {
				return s.matchSupersession(existing, p, actor)
			}
		}
		return nil, err
	}
	out.Created = created
	return out, nil
}

// validateSupersedeParams checks the inputs that need no transaction.
func validateSupersedeParams(p SupersedeParams) error {
	if err := requireText("superseded task", p.SupersededID); err != nil {
		return err
	}
	if err := requireText("attempt", p.AttemptID); err != nil {
		return err
	}
	if err := validateSupersedeTask(p.Replacement, true); err != nil {
		return err
	}
	if err := validateSupersedeTask(p.Reviewer, false); err != nil {
		return err
	}
	if strings.TrimSpace(p.EvidenceRef) == "" {
		return fmt.Errorf("%w: the supersession must name where its authorisation is recorded", ErrConstraint)
	}
	if l := len(p.EvidenceRef); l < 8 || l > 1024 {
		return fmt.Errorf("%w: the evidence reference must be 8 to 1024 characters", ErrConstraint)
	}
	okPrefix := false
	for _, prefix := range []string{"gh:", "git:", "file:", "log:"} {
		if strings.HasPrefix(p.EvidenceRef, prefix) {
			okPrefix = true
			break
		}
	}
	if !okPrefix {
		return fmt.Errorf("%w: the evidence reference must start with gh:, git:, file: or log:", ErrConstraint)
	}
	if trimmed := strings.TrimSpace(p.Justification); len(trimmed) < 1 || len(p.Justification) > 4096 {
		return fmt.Errorf("%w: the justification must be 1 to 4096 characters", ErrConstraint)
	}
	return nil
}

// validateSupersedeTask checks one fresh assignment's authorisation.
func validateSupersedeTask(t SupersedeTaskParams, isReplacement bool) error {
	if err := requireText("title", t.Title); err != nil {
		return err
	}
	if err := requireText("objective", t.Objective); err != nil {
		return err
	}
	if err := requireText("recipient", t.Recipient); err != nil {
		return err
	}
	if t.ResourceUnits < 0 {
		return fmt.Errorf("%w: resource units must not be negative", ErrConstraint)
	}
	if isReplacement {
		if strings.TrimSpace(t.BaseSHA) == "" {
			return fmt.Errorf("%w: the replacement must name the base revision it implements", ErrConstraint)
		}
	} else if strings.TrimSpace(t.BaseSHA) != "" {
		return fmt.Errorf("%w: the reviewer carries no base revision", ErrConstraint)
	}
	return nil
}

// ── lineage reads ───────────────────────────────────────────────────────

// getSupersessionTx reads one lineage row inside a transaction.
func getSupersessionTx(node internal.DBNode, id string) (*Supersession, error) {
	row := node.QueryRowTracked(`SELECT `+supersessionColumns+` FROM agent_task_supersessions WHERE id = ?`, id)
	return scanSupersession(row.Scan)
}

func scanSupersession(scan func(dest ...interface{}) error) (*Supersession, error) {
	var l Supersession
	if err := scan(
		&l.ID, &l.ProjectID, &l.RootID, &l.SupersededTaskID,
		&l.ReplacementTaskID, &l.ReviewerTaskID, &l.AttemptID,
		&l.AuthorisedBy, &l.AuthorityKind, &l.EvidenceRef, &l.Justification,
		&l.CreatedAt,
	); err != nil {
		return nil, fmt.Errorf("assignment: read the supersession: %w", err)
	}
	return &l, nil
}

// findSupersessionTx returns the lineage naming (superseded, attempt), or
// nil. The pair is UNIQUE, so at most one row can match.
func (s *Store) findSupersessionTx(supersededID, attemptID string) (*Supersession, error) {
	var out *Supersession
	err := s.db.WithTx(func(node internal.DBNode) error {
		row := node.QueryRowTracked(`SELECT `+supersessionColumns+` FROM agent_task_supersessions WHERE superseded_task_id = ? AND attempt_id = ?`, supersededID, attemptID)
		l, err := scanSupersession(row.Scan)
		if err != nil {
			if strings.Contains(err.Error(), "no rows") {
				return nil
			}
			return err
		}
		out = l
		return nil
	})
	if err != nil {
		return nil, err
	}
	return out, nil
}

// lineagesInTree lists every lineage in one tree, oldest first.
func (s *Store) lineagesInTree(rootID string) ([]*Supersession, error) {
	rows, err := s.db.SQLDB().Query(
		`SELECT `+supersessionColumns+` FROM agent_task_supersessions WHERE root_id = ? ORDER BY created_at ASC, id ASC`, rootID)
	if err != nil {
		return nil, fmt.Errorf("assignment: read the tree's lineages: %w", err)
	}
	defer rows.Close()
	var out []*Supersession
	for rows.Next() {
		l, err := scanSupersession(rows.Scan)
		if err != nil {
			return nil, err
		}
		out = append(out, l)
	}
	return out, rows.Err()
}

// matchSupersession compares a repeated authorisation against the stored
// lineage. Identical parameters return the existing lineage and both
// assignments; any material difference is a conflict naming the field.
func (s *Store) matchSupersession(existing *Supersession, p SupersedeParams, actor Actor) (*SupersedeResult, error) {
	var differ string
	switch {
	case existing.AuthorisedBy != actor.Address:
		differ = "authorised_by"
	case existing.EvidenceRef != p.EvidenceRef:
		differ = "evidence_ref"
	case existing.Justification != p.Justification:
		differ = "justification"
	}
	if differ == "" {
		replacement, err := s.Get(existing.ReplacementTaskID)
		if err != nil {
			return nil, err
		}
		if d := compareSupersedeTask(replacement, p.Replacement); d != "" {
			differ = "replacement." + d
		} else {
			reviewer, err := s.Get(existing.ReviewerTaskID)
			if err != nil {
				return nil, err
			}
			if d := compareSupersedeTask(reviewer, p.Reviewer); d != "" {
				differ = "reviewer." + d
			} else if replacement.ProjectID != existing.ProjectID || replacement.RootID != existing.RootID {
				differ = "project_id or root_id"
			}
		}
	}
	if differ != "" {
		return nil, fmt.Errorf("%w: %s", ErrSupersessionConflict, differ)
	}
	replacement, err := s.Get(existing.ReplacementTaskID)
	if err != nil {
		return nil, err
	}
	reviewer, err := s.Get(existing.ReviewerTaskID)
	if err != nil {
		return nil, err
	}
	return &SupersedeResult{Lineage: existing, Replacement: replacement, Reviewer: reviewer, Created: false}, nil
}

// compareSupersedeTask names the first authorisation field on which the
// stored assignment disagrees with the request.
func compareSupersedeTask(a *Assignment, t SupersedeTaskParams) string {
	switch {
	case a.Title != t.Title:
		return "title"
	case a.Objective != t.Objective:
		return "objective"
	case a.Recipient != t.Recipient:
		return "recipient"
	case a.ResourceUnits != t.ResourceUnits:
		return "resource_units"
	}
	if a.Role == RoleImplementer && a.BaseSHA != t.BaseSHA {
		return "base_sha"
	}
	return ""
}

// checkReservedKeyCollision refuses when a reserved key is already taken
// by a row no lineage names. The rows are never rewritten or adopted.
func (s *Store) checkReservedKeyCollision(attemptID string) error {
	replacementKey, reviewerKey := supersedeIDs(attemptID)
	var n int
	if err := s.db.SQLDB().QueryRow(
		`SELECT COUNT(*) FROM agent_tasks WHERE idempotency_key IN (?, ?)`, replacementKey, reviewerKey).Scan(&n); err != nil {
		return fmt.Errorf("assignment: check the reserved keys: %w", err)
	}
	if n == 0 {
		return nil
	}
	// A row carrying the key might belong to an existing lineage (a
	// concurrent supersession that committed first); that case is
	// resolved by the idempotency lookup, not here.
	var m int
	if err := s.db.SQLDB().QueryRow(
		`SELECT COUNT(*) FROM agent_task_supersessions WHERE attempt_id = ?`, attemptID).Scan(&m); err != nil {
		return fmt.Errorf("assignment: check the existing lineage: %w", err)
	}
	if m > 0 {
		return nil
	}
	return fmt.Errorf("%w: supersede:%s", ErrReservedKeyCollision, attemptID)
}

// isUniqueViolation reports whether the error is a UNIQUE constraint
// failure — the concurrent-supersession signal the idempotency path
// resolves by re-reading.
func isUniqueViolation(err error) bool {
	return err != nil && strings.Contains(err.Error(), "UNIQUE constraint failed")
}

// ── the guarded lineage write ───────────────────────────────────────────

// insertSupersessionTx performs the decisive authorisation: one INSERT …
// SELECT that binds the lineage to the blocked original and its exact
// ambiguous attempt. The WHERE clause re-checks every preflight condition
// atomically with the write, so a claim, a state move or a new attempt
// between preflight and commit aborts the transaction instead of
// authorising stale work. Zero affected rows aborts the whole operation.
func (s *Store) insertSupersessionTx(node internal.DBNode, p SupersedeParams, actor Actor, replacementID, reviewerID string) (string, error) {
	now := s.Now()
	id, err := newID("atsup-")
	if err != nil {
		return "", err
	}

	// The quiescence predicates join the same statement when the operator
	// requires quiescence: no unfinished task in the tree may hold a live
	// lease, and no intent/launched/running attempt may be outstanding
	// anywhere in the tree.
	quiescence := ""
	args := []interface{}{
		id, replacementID, reviewerID, actor.Address, p.EvidenceRef, p.Justification, now,
		p.SupersededID, p.Epoch, now, p.AttemptID,
	}
	if p.RequireQuiescence {
		quiescence = `
		  AND NOT EXISTS (
			SELECT 1 FROM agent_tasks o
			 WHERE o.root_id = t.root_id AND o.id <> t.id
			   AND o.claim_epoch > 0 AND o.lease_expires_at IS NOT NULL
			   AND o.lease_expires_at > ?
			   AND o.state IN ('enqueued','delivered','acknowledged','accepted','in_progress'))
		  AND NOT EXISTS (
			SELECT 1 FROM agent_task_sessions s2
			  JOIN agent_tasks t2 ON t2.id = s2.task_id
			 WHERE t2.root_id = t.root_id AND s2.id <> a.id
			   AND s2.launch_state IN ('intent','launched','running'))`
		args = append(args, now)
	}
	res, err := node.ExecTracked(`
		INSERT INTO agent_task_supersessions (
			id, project_id, root_id, superseded_task_id,
			replacement_task_id, reviewer_task_id, attempt_id,
			authorised_by, authority_kind, evidence_ref, justification, created_at
		)
		SELECT ?, t.project_id, t.root_id, t.id, ?, ?, a.id,
			?, 'operator', ?, ?, ?
		  FROM agent_tasks t
		  JOIN agent_task_sessions a ON a.task_id = t.id
		 WHERE t.id = ?
		   AND t.claim_epoch = ?
		   AND t.state IN ('in_progress','completed','cancelled')
		   AND t.role = 'implementer'
		   AND t.depth >= 1
		   AND (t.lease_expires_at IS NULL OR t.lease_expires_at <= ?)
		   AND a.id = ?
		   AND a.launch_state = 'ambiguous'
		   AND NOT EXISTS (
			SELECT 1 FROM agent_task_sessions x
			 WHERE x.task_id = t.id AND x.id <> a.id
			   AND x.launch_state IN ('ambiguous','intent','launched','running'))
	`+quiescence, 5, args...)
	if err != nil {
		return "", storageRefusal(err)
	}
	affected, err := res.RowsAffected()
	if err != nil {
		return "", fmt.Errorf("assignment: read the affected rows: %w", err)
	}
	if affected != 1 {
		return "", s.classifySupersessionRefusal(node, p, now)
	}
	return id, nil
}

// classifySupersessionRefusal names the failed condition after a guarded
// write matches zero rows. The write itself is already refused; this read
// exists so the operator learns which precondition moved.
func (s *Store) classifySupersessionRefusal(node internal.DBNode, p SupersedeParams, now int64) error {
	task, err := getAssignmentTx(node, p.SupersededID)
	if err != nil {
		return err
	}
	if task.ClaimEpoch != p.Epoch {
		return fmt.Errorf("%w: %s is at epoch %d", ErrStaleClaim, task.ID, task.ClaimEpoch)
	}
	// A4 exact boundaries, refused before any write.
	if task.Role == RoleReviewer {
		return fmt.Errorf("%w: %s is a reviewer", ErrReviewerOriginal, task.ID)
	}
	if task.Depth < 1 {
		return fmt.Errorf("%w: %s is the root", ErrRootSupersession, task.ID)
	}
	if task.Role != RoleImplementer {
		return fmt.Errorf("%w: %s cannot be superseded", ErrConstraint, task.ID)
	}
	switch task.State {
	case StateInProgress, StateCompleted, StateCancelled:
	case StateOperatorApproved:
		return fmt.Errorf("%w: %s is %s", ErrLegacyApprovedAmbiguity, task.ID, task.State)
	default:
		return fmt.Errorf("%w: %s is %s and cannot be superseded", ErrConstraint, task.ID, task.State)
	}
	if task.LeaseExpiresAt != nil && *task.LeaseExpiresAt > now {
		return fmt.Errorf("%w: %s holds a live lease until %d", ErrLeaseHeld, task.ID, *task.LeaseExpiresAt)
	}
	att, err := getAttemptTx(node, p.AttemptID)
	if err != nil {
		return err
	}
	if att == nil || att.TaskID != task.ID {
		return fmt.Errorf("%w: the named attempt is not %s's", ErrConstraint, task.ID)
	}
	if att.LaunchState != SessionAmbiguous {
		return fmt.Errorf("%w: the named attempt %s is %s, not ambiguous", ErrConstraint, att.ID, att.LaunchState)
	}
	// An attempt conflict: another ambiguity refuses with the A4 code;
	// a live intent/launched/running attempt needs operator reconciliation
	// first.
	var ambOthers int
	if err := node.QueryRowTracked(`SELECT COUNT(*) FROM agent_task_sessions
		 WHERE task_id = ? AND id <> ? AND launch_state = 'ambiguous'`,
		task.ID, att.ID).Scan(&ambOthers); err != nil {
		return fmt.Errorf("assignment: read the conflicting attempts: %w", err)
	}
	if ambOthers > 0 {
		return fmt.Errorf("%w: %s", ErrMultipleAmbiguousAttempts, task.ID)
	}
	var liveOthers int
	if err := node.QueryRowTracked(`SELECT COUNT(*) FROM agent_task_sessions
		 WHERE task_id = ? AND id <> ? AND launch_state IN ('intent','launched','running')`,
		task.ID, att.ID).Scan(&liveOthers); err != nil {
		return fmt.Errorf("assignment: read the live attempts: %w", err)
	}
	if liveOthers > 0 {
		return fmt.Errorf("%w: %s carries a live attempt; reconcile it before superseding", ErrConstraint, task.ID)
	}
	if p.RequireQuiescence {
		var live int
		if err := node.QueryRowTracked(`SELECT COUNT(*) FROM agent_tasks o
			 WHERE o.root_id = ? AND o.id <> ?
			   AND o.claim_epoch > 0 AND o.lease_expires_at IS NOT NULL
			   AND o.lease_expires_at > ?
			   AND o.state IN ('enqueued','delivered','acknowledged','accepted','in_progress')`,
			task.RootID, task.ID, now).Scan(&live); err != nil {
			return fmt.Errorf("assignment: read the live claims: %w", err)
		}
		if live > 0 {
			return fmt.Errorf("%w: the tree of %s holds live claims; quiescence is required", ErrConstraint, task.ID)
		}
		var liveAttempts int
		if err := node.QueryRowTracked(`SELECT COUNT(*) FROM agent_task_sessions s2
			  JOIN agent_tasks t2 ON t2.id = s2.task_id
			 WHERE t2.root_id = ? AND s2.id <> ?
			   AND s2.launch_state IN ('intent','launched','running')`,
			task.RootID, att.ID).Scan(&liveAttempts); err != nil {
			return fmt.Errorf("assignment: read the live attempts: %w", err)
		}
		if liveAttempts > 0 {
			return fmt.Errorf("%w: the tree of %s holds live attempts; quiescence is required", ErrConstraint, task.ID)
		}
	}
	return fmt.Errorf("%w: the supersession preconditions of %s moved while the write was in flight", ErrConstraint, task.ID)
}

// insertSupersededChildTx inserts one fresh assignment under the
// supersession: the replacement (implementer) or the reviewer. The caller
// minted the id BEFORE the lineage, so the lineage already names it;
// TR6, the deferred FKs and TR9 all check the same value. The reserved
// idempotency key binds the row to its lineage; TR9 refuses any other key
// shape, and TR10 enforces the sealed shape. The tree's budget conditions
// ride in the same INSERT, so an exhausted tree refuses atomically with
// nothing written.
func (s *Store) insertSupersededChildTx(node internal.DBNode, p SupersedeParams, id string, isReplacement bool) (*Assignment, error) {
	superseded, err := getAssignmentTx(node, p.SupersededID)
	if err != nil {
		return nil, err
	}
	replacementKey, reviewerKey := supersedeIDs(p.AttemptID)
	now := s.Now()
	if isReplacement {
		if _, err := node.ExecTracked(`
			INSERT INTO agent_tasks (
				id, project_id, root_id, parent_id, depth, title, objective,
				recipient, role, state, idempotency_key, resource_units, base_sha,
				created_at, updated_at
			)
			SELECT ?, ?, ?, ?, ?, ?, ?, ?, 'implementer', 'enqueued', ?, ?, ?, ?, ?
			WHERE `+taskBudgetCondition+`
			  AND `+resourceBudgetCondition,
			5,
			id, superseded.ProjectID, superseded.RootID, superseded.ParentID, superseded.Depth,
			p.Replacement.Title, p.Replacement.Objective, p.Replacement.Recipient,
			replacementKey, p.Replacement.ResourceUnits, p.Replacement.BaseSHA,
			now, now,
			// taskBudgetCondition
			superseded.RootID, superseded.RootID,
			// resourceBudgetCondition
			superseded.RootID, p.Replacement.ResourceUnits, superseded.RootID); err != nil {
			return nil, storageRefusal(err)
		}
		if err := s.injectFault("supersede-replacement"); err != nil {
			return nil, err
		}
		got, err := getAssignmentTx(node, id)
		if err != nil {
			// A missing row means the budget-conditioned INSERT
			// matched zero rows: the tree cannot take two more
			// assignments. Name the refusing budget rather than
			// reporting a missing row.
			if errors.Is(err, ErrNotFound) {
				return nil, budgetRefusalForSupersede(node, superseded.RootID, now, p.Replacement.ResourceUnits, "the replacement")
			}
			return nil, err
		}
		return got, nil
	}
	// The review target is the replacement the lineage names: read it
	// from the lineage row written earlier in this same transaction,
	// never from a caller-supplied id.
	var replacementID string
	if err := node.QueryRowTracked(
		`SELECT replacement_task_id FROM agent_task_supersessions WHERE attempt_id = ?`,
		p.AttemptID).Scan(&replacementID); err != nil {
		return nil, fmt.Errorf("assignment: read the supersession replacement: %w", err)
	}
	payload, err := marshalPayload(map[string]interface{}{PayloadKeyReviewTarget: replacementID})
	if err != nil {
		return nil, err
	}
	if _, err := node.ExecTracked(`
		INSERT INTO agent_tasks (
			id, project_id, root_id, parent_id, depth, title, objective,
			recipient, role, state, idempotency_key, resource_units, payload,
			created_at, updated_at
		)
		SELECT ?, ?, ?, ?, ?, ?, ?, ?, 'reviewer', 'enqueued', ?, ?, ?, ?, ?
		WHERE `+taskBudgetCondition+`
		  AND `+resourceBudgetCondition,
		5,
		id, superseded.ProjectID, superseded.RootID, superseded.ParentID, superseded.Depth,
		p.Reviewer.Title, p.Reviewer.Objective, p.Reviewer.Recipient,
		reviewerKey, p.Reviewer.ResourceUnits, payload,
		now, now,
		// taskBudgetCondition
		superseded.RootID, superseded.RootID,
		// resourceBudgetCondition
		superseded.RootID, p.Reviewer.ResourceUnits, superseded.RootID); err != nil {
		return nil, storageRefusal(err)
	}
	if err := s.injectFault("supersede-reviewer"); err != nil {
		return nil, err
	}
	got, err := getAssignmentTx(node, id)
	if err != nil {
		if errors.Is(err, ErrNotFound) {
			return nil, budgetRefusalForSupersede(node, superseded.RootID, now, p.Reviewer.ResourceUnits, "the reviewer")
		}
		return nil, err
	}
	return got, nil
}

// budgetRefusalForSupersede names the budget that refused a supersession
// child. A zero-row INSERT is the only symptom, so the figures are read
// back for the message.
func budgetRefusalForSupersede(node internal.DBNode, rootID string, now int64, units int64, who string) error {
	budget, err := readBudgetSnapshot(node, rootID, now)
	if err != nil {
		return err
	}
	if budget.Tasks >= budget.MaxTasks {
		return budgetRefusal("max_tasks", budget.MaxTasks, budget.Tasks,
			"and this supersession would add two more assignments")
	}
	return budgetRefusal("max_resource_units", budget.MaxUnits, budget.Units,
		fmt.Sprintf("and %s costs %d", who, units))
}

// ── the shared execution-authorisation predicate ─────────────────────────

// ExecAuth is one evaluation of the tree-scoped execution rule:
//
//	ExecAuth(X) = ¬Historical(X) ∧ ¬OwnAmb(X)
//	              ∧ (Amb(T) empty ∨ (Named(T) = Amb(T) ∧ Scoped(X)))
//
// Amb(T) is the set of ambiguous attempts in the tree. Named(T) is the
// subset named by a lineage. Scoped(X) says a live lineage names X as
// its replacement or reviewer. Historical(X) covers a superseded task,
// anything below one, and the reviewer of a non-live lineage.
type execAuth struct {
	historical  bool
	ownAmb      bool
	ownAmbNamed bool
	ambCount    int
	namedEqAmb  bool
	scoped      bool
}

// authorised reports the predicate.
func (e execAuth) authorised() bool {
	return !e.historical && !e.ownAmb && (e.ambCount == 0 || (e.namedEqAmb && e.scoped))
}

// execAuthFor evaluates the predicate for one task. It reads committed
// state only and writes nothing.
func (s *Store) execAuthFor(task *Assignment) (execAuth, error) {
	var out execAuth
	hist, err := s.isHistorical(task)
	if err != nil {
		return out, err
	}
	out.historical = hist
	var own, ownNamed int
	if err := s.db.SQLDB().QueryRow(
		`SELECT COUNT(*), COALESCE(SUM(CASE WHEN EXISTS
			(SELECT 1 FROM agent_task_supersessions L WHERE L.attempt_id = s.id)
			THEN 1 ELSE 0 END), 0)
		   FROM agent_task_sessions s WHERE s.task_id = ? AND s.launch_state = 'ambiguous'`,
		task.ID).Scan(&own, &ownNamed); err != nil {
		return out, fmt.Errorf("assignment: read the ambiguous attempts: %w", err)
	}
	out.ownAmb = own > 0
	out.ownAmbNamed = ownNamed == own
	var total, unnamed int
	if err := s.db.SQLDB().QueryRow(
		`SELECT COUNT(*), COALESCE(SUM(CASE WHEN NOT EXISTS
			(SELECT 1 FROM agent_task_supersessions L WHERE L.attempt_id = s.id)
			THEN 1 ELSE 0 END), 0)
		   FROM agent_task_sessions s JOIN agent_tasks t ON t.id = s.task_id
		  WHERE t.root_id = ? AND s.launch_state = 'ambiguous'`,
		task.RootID).Scan(&total, &unnamed); err != nil {
		return out, fmt.Errorf("assignment: read the tree ambiguity: %w", err)
	}
	out.ambCount = total
	out.namedEqAmb = unnamed == 0
	var scoped int
	if err := s.db.SQLDB().QueryRow(
		`SELECT COUNT(*) FROM agent_task_supersessions L
		  WHERE L.root_id = ? AND (L.replacement_task_id = ? OR L.reviewer_task_id = ?)
		    AND NOT EXISTS (SELECT 1 FROM agent_task_supersessions L2
		                     WHERE L2.superseded_task_id = L.replacement_task_id)`,
		task.RootID, task.ID, task.ID).Scan(&scoped); err != nil {
		return out, fmt.Errorf("assignment: read the lineage scope: %w", err)
	}
	out.scoped = scoped > 0
	return out, nil
}

// isHistorical reports whether the task is superseded itself, sits below
// a superseded task, or is the reviewer of a non-live lineage.
func (s *Store) isHistorical(task *Assignment) (bool, error) {
	var n int
	if err := s.db.SQLDB().QueryRow(
		`SELECT COUNT(*) FROM agent_task_supersessions WHERE superseded_task_id = ? AND root_id = ?`,
		task.ID, task.RootID).Scan(&n); err != nil {
		return false, fmt.Errorf("assignment: read the supersession lineage: %w", err)
	}
	if n > 0 {
		return true, nil
	}
	// Walk the parent chain in Go: trees are shallow and the walk
	// terminates because depth strictly decreases.
	current := task.ParentID
	for current != "" {
		var sup int
		if err := s.db.SQLDB().QueryRow(
			`SELECT COUNT(*) FROM agent_task_supersessions WHERE superseded_task_id = ? AND root_id = ?`,
			current, task.RootID).Scan(&sup); err != nil {
			return false, fmt.Errorf("assignment: read the supersession lineage: %w", err)
		}
		if sup > 0 {
			return true, nil
		}
		var parent string
		var depth int
		if err := s.db.SQLDB().QueryRow(
			`SELECT COALESCE(parent_id,''), depth FROM agent_tasks WHERE id = ?`, current).Scan(&parent, &depth); err != nil {
			return false, fmt.Errorf("assignment: walk the delegation chain: %w", err)
		}
		_ = depth
		current = parent
	}
	// A reviewer of a non-live lineage is stale: its review already
	// happened under a superseded authorisation.
	if task.Role == RoleReviewer {
		var stale int
		if err := s.db.SQLDB().QueryRow(
			`SELECT COUNT(*) FROM agent_task_supersessions L
			  WHERE L.reviewer_task_id = ? AND L.root_id = ?
			    AND EXISTS (SELECT 1 FROM agent_task_supersessions L2
			                 WHERE L2.superseded_task_id = L.replacement_task_id)`,
			task.ID, task.RootID).Scan(&stale); err != nil {
			return false, fmt.Errorf("assignment: read the reviewer lineage: %w", err)
		}
		if stale > 0 {
			return true, nil
		}
	}
	return false, nil
}

// EffectiveState reports the execution-aware state of one assignment.
//
// Historical tasks read `superseded`. An ambiguous task whose own
// ambiguity no lineage names reads `recovery_required`. An authorised
// task in a live lineage reads `authorised`; an ambiguous tree that does
// not authorise the task reads `blocked`. Anything else reads its stored
// state.
func (s *Store) EffectiveState(id string) (string, error) {
	task, err := s.Get(id)
	if err != nil {
		return "", err
	}
	auth, err := s.execAuthFor(task)
	if err != nil {
		return "", err
	}
	if auth.historical {
		return "superseded", nil
	}
	if auth.ownAmb && !auth.ownAmbNamed {
		return "recovery_required", nil
	}
	if auth.authorised() {
		if auth.scoped {
			return "authorised", nil
		}
	}
	if auth.ambCount > 0 {
		return "blocked", nil
	}
	return task.State, nil
}

// ── tree and recovery status ─────────────────────────────────────────────

// TreeStatus is the derived completion picture of one delegation tree.
// It is computed, never stored: the root row is never rewritten to
// reflect derived completion.
type TreeStatus struct {
	RootID   string          `json:"root_id"`
	Tree     string          `json:"tree_status"`
	Recovery string          `json:"recovery_status"`
	Lineages []*Supersession `json:"lineages,omitempty"`
}

// TreeStatusOf computes the spec's tree_status and recovery_status arms
// for one tree.
func (s *Store) TreeStatus(rootID string) (*TreeStatus, error) {
	tasks, err := s.ListByRoot(rootID)
	if err != nil {
		return nil, err
	}
	byID := map[string]*Assignment{}
	childrenOf := map[string][]*Assignment{}
	for _, t := range tasks {
		byID[t.ID] = t
		childrenOf[t.ParentID] = append(childrenOf[t.ParentID], t)
	}
	lineages, err := s.lineagesInTree(rootID)
	if err != nil {
		return nil, err
	}
	// Attempts grouped by task, terminal state included: TerminalSuccess
	// needs completed+succeeded at the CURRENT epoch, and the ambiguity
	// arms need every ambiguous row, not just the outstanding ones.
	attemptsByTask := map[string][]*SessionAttempt{}
	for _, t := range tasks {
		atts, err := s.AttemptsFor(t.ID)
		if err != nil {
			return nil, err
		}
		attemptsByTask[t.ID] = atts
	}
	named := map[string]bool{}
	for _, l := range lineages {
		named[l.AttemptID] = true
	}
	live := map[string]bool{}
	superseded := map[string]bool{}
	for _, l := range lineages {
		superseded[l.SupersededTaskID] = true
	}
	for _, l := range lineages {
		stillLive := true
		for _, l2 := range lineages {
			if l2.SupersededTaskID == l.ReplacementTaskID {
				stillLive = false
				break
			}
		}
		if stillLive {
			live[l.SupersededTaskID] = true
		}
	}
	_ = live

	// Ambiguous attempts in the tree, and whether every one is named.
	ambUnnamed := false
	ambAny := false
	for _, atts := range attemptsByTask {
		for _, a := range atts {
			if a.LaunchState != SessionAmbiguous {
				continue
			}
			ambAny = true
			if !named[a.ID] {
				ambUnnamed = true
			}
		}
	}
	namedEqAmb := !ambUnnamed

	isHistorical := func(id string) bool {
		t := byID[id]
		if t == nil {
			return false
		}
		if superseded[id] {
			return true
		}
		// An ancestor superseded: walk the parent chain.
		cur := t.ParentID
		for cur != "" {
			if superseded[cur] {
				return true
			}
			p := byID[cur]
			if p == nil {
				break
			}
			cur = p.ParentID
		}
		// A reviewer of a non-live lineage is stale.
		if t.Role == RoleReviewer {
			for _, l := range lineages {
				if l.ReviewerTaskID != id {
					continue
				}
				stillLive := true
				for _, l2 := range lineages {
					if l2.SupersededTaskID == l.ReplacementTaskID {
						stillLive = false
						break
					}
				}
				if !stillLive {
					return true
				}
			}
		}
		return false
	}
	terminalSuccess := func(t *Assignment) bool {
		if isHistorical(t.ID) {
			return false
		}
		for _, a := range attemptsByTask[t.ID] {
			if a.LaunchState == SessionAmbiguous {
				return false
			}
		}
		want := StateOperatorApproved
		if t.Role == RoleReviewer {
			want = StateReviewPassed
			if t.ReviewedSHA == "" || t.FindingsSHA256 == "" {
				return false
			}
		}
		if t.State != want {
			return false
		}
		for _, a := range attemptsByTask[t.ID] {
			if a.ClaimEpoch == t.ClaimEpoch && a.LaunchState == SessionCompleted &&
				a.RuntimeOutcome == "succeeded" {
				return true
			}
		}
		return false
	}

	out := &TreeStatus{RootID: rootID, Lineages: lineages}
	// recovery_status arms, in priority order.
	switch {
	case len(lineages) == 0:
		out.Recovery = "none"
	default:
		// Arm 2: a scoped reviewer has its own ambiguous attempt.
		reviewerAmb := false
		for _, l := range lineages {
			if !live[l.SupersededTaskID] {
				continue
			}
			for _, a := range attemptsByTask[l.ReviewerTaskID] {
				if a.LaunchState == SessionAmbiguous {
					reviewerAmb = true
				}
			}
		}
		if reviewerAmb {
			out.Recovery = "reviewer_ambiguous"
			break
		}
		// Arm 3: an ambiguous attempt not named by a lineage, excluding
		// a scoped reviewer's own attempt.
		unnamed := false
		for _, atts := range attemptsByTask {
			for _, a := range atts {
				if a.LaunchState != SessionAmbiguous || named[a.ID] {
					continue
				}
				scopedReviewer := false
				for _, l := range lineages {
					if l.ReviewerTaskID == a.TaskID && live[l.SupersededTaskID] {
						scopedReviewer = true
					}
				}
				if !scopedReviewer {
					unnamed = true
				}
			}
		}
		if unnamed {
			out.Recovery = "blocked_unresolved_ambiguity"
			break
		}
		// Arm 4: the reviewer of a live lineage ended completed.
		rework := false
		for _, l := range lineages {
			if !live[l.SupersededTaskID] {
				continue
			}
			for _, t := range tasks {
				if t.ID == l.ReviewerTaskID && t.State == StateCompleted {
					rework = true
				}
			}
		}
		if rework {
			out.Recovery = "rework_required"
			break
		}
		// Arm 5: a scoped task is cancelled and no earlier arm applies.
		failed := false
		for _, t := range tasks {
			if t.State != StateCancelled || isHistorical(t.ID) {
				continue
			}
			for _, l := range lineages {
				if l.ReplacementTaskID == t.ID || l.ReviewerTaskID == t.ID {
					failed = true
				}
			}
		}
		if failed {
			out.Recovery = "failed"
			break
		}
		// Arms 6-7: live lineage progress.
		allDone := true
		for _, l := range lineages {
			if !live[l.SupersededTaskID] {
				continue
			}
			rep, ok1 := byID[l.ReplacementTaskID]
			rev, ok2 := byID[l.ReviewerTaskID]
			if !ok1 || !ok2 || !terminalSuccess(rep) || !terminalSuccess(rev) {
				allDone = false
			}
		}
		if allDone {
			out.Recovery = "complete"
		} else {
			out.Recovery = "in_progress"
		}
	}

	// tree_status arms, in priority order.
	root, _ := byID[rootID]
	switch {
	case root != nil && root.State == StateCancelled:
		out.Tree = "withdrawn"
	case ambAny && !namedEqAmb:
		out.Tree = "blocked_unresolved_ambiguity"
	default:
		rework := false
		for _, l := range lineages {
			if !live[l.SupersededTaskID] {
				continue
			}
			for _, t := range tasks {
				if t.ID == l.ReviewerTaskID && t.State == StateCompleted {
					rework = true
				}
			}
		}
		if rework {
			out.Tree = "rework_required"
			break
		}
		// Obligations: non-historical tasks with no non-historical child.
		var obligations []*Assignment
		for _, t := range tasks {
			if isHistorical(t.ID) {
				continue
			}
			leaf := true
			for _, c := range childrenOf[t.ID] {
				if !isHistorical(c.ID) {
					leaf = false
					break
				}
			}
			if leaf {
				obligations = append(obligations, t)
			}
		}
		complete := len(obligations) > 0
		for _, t := range obligations {
			if !terminalSuccess(t) {
				complete = false
				break
			}
		}
		if complete && (out.Recovery == "none" || out.Recovery == "complete") {
			out.Tree = "complete"
		} else {
			out.Tree = "incomplete"
		}
	}
	return out, nil
}

// ── shared execution-authorisation enforcement ───────────────────────────

// execBlocked reports whether an execution step is barred for the task,
// and why. Historical tasks are frozen: supersession never re-enables
// them, and no new launch, claim, renewal, start, completion, verdict or
// approval may touch them. Tasks in an ambiguous tree run only when the
// shared predicate authorises them. Nil means the step may proceed; the
// trigger remains the atomic backstop.
func (s *Store) execBlocked(task *Assignment) error {
	auth, err := s.execAuthFor(task)
	if err != nil {
		return err
	}
	if auth.historical {
		return fmt.Errorf("%w: %s was superseded by a recovery lineage and is frozen",
			ErrSuperseded, task.ID)
	}
	if !auth.authorised() {
		return fmt.Errorf("%w: %s is in an ambiguous tree without an authorising lineage",
			ErrAmbiguousAttempt, task.ID)
	}
	return nil
}

// execBlockedHistorical is the narrower gate for steps that remain
// legal on unscoped tasks in an ambiguous tree (completion of admitted
// work, withdrawal, the operator's gate): only supersession and the
// task's own ambiguity block them.
func (s *Store) execBlockedHistorical(task *Assignment) error {
	auth, err := s.execAuthFor(task)
	if err != nil {
		return err
	}
	if auth.historical {
		return fmt.Errorf("%w: %s was superseded by a recovery lineage and is frozen",
			ErrSuperseded, task.ID)
	}
	if auth.ownAmb {
		return fmt.Errorf("%w: %s carries an unresolved ambiguous attempt",
			ErrAmbiguousAttempt, task.ID)
	}
	return nil
}
