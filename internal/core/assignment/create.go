package assignment

import (
	"encoding/json"
	"errors"
	"fmt"
	"strings"

	internal "github.com/flowbyte-com/mpm-core"
)

// CreateRoot creates an operator-authorised ROOT assignment.
//
// Root creation is an OPERATOR operation. It requires an OperatorAuthority,
// and it is absent from the tool registry entirely — so no MCP action and
// no `mpm call` action can reach it. See the design note §2.
//
// A root is its own root, it has no parent, and it CARRIES the tree's
// budgets: a child carries none of them, and the schema's root/child
// CHECK makes that structural rather than a convention.
//
// IDEMPOTENCY. The key identifies ONE logical assignment. A repeated
// request returns the existing assignment and reports created=false; a
// request that describes a DIFFERENT assignment under the same key is a
// conflict, because handing back a row that does not match the request
// would be worse than an error.
//
// The returned bool is true when this call created the row.
func (s *Store) CreateRoot(auth OperatorAuthority, p CreateRootParams) (*Assignment, bool, error) {
	if auth == nil {
		return nil, false, fmt.Errorf("%w: root creation is an operator operation", ErrNotAuthorised)
	}
	actor := auth.Actor()

	if err := requireText("project_id", p.ProjectID); err != nil {
		return nil, false, err
	}
	if err := requireText("title", p.Title); err != nil {
		return nil, false, err
	}
	if err := requireText("objective", p.Objective); err != nil {
		return nil, false, err
	}
	if err := requireText("recipient", p.Recipient); err != nil {
		return nil, false, err
	}
	if err := requireText("idempotency_key", p.IdempotencyKey); err != nil {
		return nil, false, err
	}
	if len(p.IdempotencyKey) > 128 {
		return nil, false, fmt.Errorf("%w: the idempotency key is longer than 128 bytes", ErrConstraint)
	}
	if len(p.PermittedOperations) == 0 {
		return nil, false, fmt.Errorf("%w: a root must name at least one permitted operation", ErrConstraint)
	}
	if len(p.EligibleRecipients) == 0 {
		return nil, false, fmt.Errorf("%w: a root must name at least one eligible recipient", ErrConstraint)
	}
	if p.MaxDepth < 0 {
		return nil, false, fmt.Errorf("%w: max_depth must not be negative", ErrConstraint)
	}
	if p.MaxTasks <= 0 {
		return nil, false, fmt.Errorf("%w: max_tasks must be positive", ErrConstraint)
	}
	if p.MaxConcurrency <= 0 {
		return nil, false, fmt.Errorf("%w: max_concurrency must be positive", ErrConstraint)
	}
	if p.MaxResourceUnits < 0 {
		return nil, false, fmt.Errorf("%w: max_resource_units must not be negative", ErrConstraint)
	}
	if p.ResourceUnits < 0 {
		return nil, false, fmt.Errorf("%w: resource_units must not be negative", ErrConstraint)
	}
	role := p.Role
	if role == "" {
		role = RoleImplementer
	}
	if role != RoleImplementer && role != RoleReviewer {
		return nil, false, fmt.Errorf("%w: role must be %q or %q", ErrConstraint, RoleImplementer, RoleReviewer)
	}
	if !containsString(p.EligibleRecipients, p.Recipient) {
		return nil, false, fmt.Errorf(
			"%w: the root's own recipient %q is not among its eligible recipients", ErrConstraint, p.Recipient)
	}

	permittedJSON, err := marshalStringList("permitted_operations", p.PermittedOperations)
	if err != nil {
		return nil, false, fmt.Errorf("%w: %s", ErrConstraint, err)
	}
	eligibleJSON, err := marshalStringList("eligible_recipients", p.EligibleRecipients)
	if err != nil {
		return nil, false, fmt.Errorf("%w: %s", ErrConstraint, err)
	}
	payloadJSON, err := marshalPayload(p.Payload)
	if err != nil {
		return nil, false, err
	}

	var out *Assignment
	var created bool
	err = s.db.WithTx(func(node internal.DBNode) error {
		// The idempotent path FIRST: PR 1's before-insert trigger
		// refuses a duplicate key BEFORE the conflict resolution, so
		// ON CONFLICT cannot be relied on to resolve it.
		existing, err := s.findExistingTx(node, p.IdempotencyKey)
		if err != nil {
			return err
		}
		if existing != nil {
			if !sameLogicalAssignment(existing, rootSpec{
				projectID: p.ProjectID, title: p.Title, objective: p.Objective,
				recipient: p.Recipient, role: role,
			}) {
				return fmt.Errorf(
					"%w: the key %q already names %s", ErrIdempotencyConflict, p.IdempotencyKey, existing.ID)
			}
			out = existing
			return nil
		}

		id, err := newID("at-")
		if err != nil {
			return err
		}
		now := s.Now()

		// The project must exist. The schema's foreign key would
		// refuse the write anyway, but "refused by the storage:
		// FOREIGN KEY constraint failed" is a poor answer to "which
		// project does not exist?".
		exists, err := projectExistsTx(node, p.ProjectID)
		if err != nil {
			return err
		}
		if !exists {
			return fmt.Errorf("%w: the project %q does not exist", ErrConstraint, p.ProjectID)
		}

		if err := s.injectFault("create-root"); err != nil {
			return err
		}

		if _, err := node.ExecTracked(`
			INSERT INTO agent_tasks (
				id, project_id, root_id, parent_id, depth, title, objective,
				recipient, role, state, idempotency_key,
				max_depth, max_tasks, max_concurrency, max_resource_units,
				resource_units, permitted_operations, eligible_recipients,
				issue_ref, pr_ref, base_sha, head_sha, reviewed_sha,
				findings_sha256, evidence_ref, payload, created_at, updated_at
			) VALUES (?, ?, ?, NULL, 0, ?, ?, ?, ?, 'enqueued', ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
			5,
			id, p.ProjectID, id, p.Title, p.Objective, p.Recipient, role,
			p.IdempotencyKey,
			p.MaxDepth, p.MaxTasks, p.MaxConcurrency, p.MaxResourceUnits,
			p.ResourceUnits, permittedJSON, eligibleJSON,
			nullIfEmpty(p.IssueRef), nullIfEmpty(p.PRRef), nullIfEmpty(p.BaseSHA),
			nullIfEmpty(p.HeadSHA), nullIfEmpty(p.ReviewedSHA),
			nullIfEmpty(p.FindingsSHA256), nullIfEmpty(p.EvidenceRef),
			payloadJSON, now, now,
		); err != nil {
			// The unique index may have lost the race. The trigger
			// refuses the write, so the winner's row is read back
			// rather than a second assignment being created.
			if strings.Contains(err.Error(), "never reused") {
				winner, ferr := findByIdempotencyKey(node, p.IdempotencyKey)
				if ferr != nil {
					return ferr
				}
				if winner != nil {
					out = winner
					return nil
				}
			}
			return storageRefusal(err)
		}

		if err := s.injectFault("create-root-event"); err != nil {
			return err
		}
		if _, err := appendEvent(node, id, EventCreated, "", StateEnqueued, actor, nil,
			"the root assignment was created", nil, now); err != nil {
			return err
		}

		row, err := getAssignmentTx(node, id)
		if err != nil {
			return err
		}
		out = row
		created = true
		return nil
	})
	if err != nil {
		return nil, false, err
	}
	return out, created, nil
}

// Delegate creates a CHILD assignment inside an existing root.
//
// This is the bounded agent-to-agent delegation. The child:
//
//   - is at depth parent.depth + 1, and that depth must be within the
//     ROOT's max_depth (N3);
//   - is addressed to a recipient among the ROOT's eligible_recipients
//     (N2);
//   - carries NO budgets and NO lists — it inherits the root's, and the
//     schema's root/child CHECK makes that structural.
//
// The parent must be a live assignment: a cancelled, gated or approved
// assignment is not delegated from.
//
// The returned bool is true when this call created the row.
func (s *Store) Delegate(p DelegateParams) (*Assignment, bool, error) {
	if err := requireText("parent_id", p.ParentID); err != nil {
		return nil, false, err
	}
	if err := requireText("title", p.Title); err != nil {
		return nil, false, err
	}
	if err := requireText("objective", p.Objective); err != nil {
		return nil, false, err
	}
	if err := requireText("recipient", p.Recipient); err != nil {
		return nil, false, err
	}
	if err := requireText("idempotency_key", p.IdempotencyKey); err != nil {
		return nil, false, err
	}
	if len(p.IdempotencyKey) > 128 {
		return nil, false, fmt.Errorf("%w: the idempotency key is longer than 128 bytes", ErrConstraint)
	}
	if p.ResourceUnits < 0 {
		return nil, false, fmt.Errorf("%w: resource_units must not be negative", ErrConstraint)
	}
	role := p.Role
	if role == "" {
		role = RoleImplementer
	}
	if role != RoleImplementer && role != RoleReviewer {
		return nil, false, fmt.Errorf("%w: role must be %q or %q", ErrConstraint, RoleImplementer, RoleReviewer)
	}

	payloadJSON, err := marshalPayload(p.Payload)
	if err != nil {
		return nil, false, err
	}

	var out *Assignment
	var created bool
	err = s.db.WithTx(func(node internal.DBNode) error {
		existing, err := s.findExistingTx(node, p.IdempotencyKey)
		if err != nil {
			return err
		}
		if existing != nil {
			if !sameLogicalAssignment(existing, rootSpec{
				projectID: "", parentID: p.ParentID, title: p.Title,
				objective: p.Objective, recipient: p.Recipient, role: role,
			}) {
				return fmt.Errorf(
					"%w: the key %q already names %s", ErrIdempotencyConflict, p.IdempotencyKey, existing.ID)
			}
			out = existing
			return nil
		}

		parent, err := getAssignmentTx(node, p.ParentID)
		if err != nil {
			return err
		}
		switch parent.State {
		case StateCancelled, StateAwaitingOperator, StateOperatorApproved:
			return fmt.Errorf(
				"%w: %s is %s and is not delegated from", ErrConstraint, parent.ID, parent.State)
		}

		root, err := getAssignmentTx(node, parent.RootID)
		if err != nil {
			return err
		}

		// N3: the child's depth is within the ROOT's budget.
		childDepth := int64(parent.Depth) + 1
		if childDepth > root.MaxDepth {
			return fmt.Errorf(
				"%w: the child would be at depth %d and the root allows %d",
				ErrConstraint, childDepth, root.MaxDepth)
		}

		// N2: the child's recipient is among the ROOT's allow-list.
		if !containsString(root.EligibleRecipients, p.Recipient) {
			return fmt.Errorf(
				"%w: %q is not among the root's eligible recipients", ErrConstraint, p.Recipient)
		}

		id, err := newID("at-")
		if err != nil {
			return err
		}
		now := s.Now()

		if err := s.injectFault("delegate"); err != nil {
			return err
		}

		if _, err := node.ExecTracked(`
			INSERT INTO agent_tasks (
				id, project_id, root_id, parent_id, depth, title, objective,
				recipient, role, state, idempotency_key,
				resource_units, permitted_operations, eligible_recipients,
				issue_ref, pr_ref, base_sha, head_sha, reviewed_sha,
				findings_sha256, evidence_ref, payload, created_at, updated_at
			)
			SELECT ?, ?, ?, ?, ?, ?, ?, ?, ?, 'enqueued', ?, ?, '[]', '[]', ?, ?, ?, ?, ?, ?, ?, ?, ?, ?
			WHERE `+taskBudgetCondition+`
			  AND `+resourceBudgetCondition,
			5,
			id, parent.ProjectID, root.ID, parent.ID, childDepth,
			p.Title, p.Objective, p.Recipient, role, p.IdempotencyKey,
			p.ResourceUnits,
			nullIfEmpty(p.IssueRef), nullIfEmpty(p.PRRef), nullIfEmpty(p.BaseSHA),
			nullIfEmpty(p.HeadSHA), nullIfEmpty(p.ReviewedSHA),
			nullIfEmpty(p.FindingsSHA256), nullIfEmpty(p.EvidenceRef),
			payloadJSON, now, now,
			// taskBudgetCondition
			root.ID, root.ID,
			// resourceBudgetCondition
			root.ID, p.ResourceUnits, root.ID,
		); err != nil {
			if strings.Contains(err.Error(), "never reused") {
				winner, ferr := findByIdempotencyKey(node, p.IdempotencyKey)
				if ferr != nil {
					return ferr
				}
				if winner != nil {
					out = winner
					return nil
				}
			}
			return storageRefusal(err)
		}

		// THE BUDGET REFUSAL IS ZERO ROWS, NOT AN ERROR.
		//
		// The INSERT's WHERE clause carries the budget condition, so a
		// tree that is full produces NO row and NO SQLite error. That is
		// what makes the check atomic with the write — and it means the
		// refusal has to be read back, because "nothing was inserted" is
		// the only symptom.
		inserted, err := getAssignmentTx(node, id)
		if err != nil && !errors.Is(err, ErrNotFound) {
			return err
		}
		if inserted == nil {
			budget, berr := readBudgetSnapshot(node, root.ID, now)
			if berr != nil {
				return berr
			}
			if budget.Tasks >= budget.MaxTasks {
				return budgetRefusal("max_tasks", budget.MaxTasks, budget.Tasks,
					"and this assignment would be one more")
			}
			return budgetRefusal("max_resource_units", budget.MaxUnits, budget.Units,
				fmt.Sprintf("and this assignment costs %d, so the tree would hold %d",
					p.ResourceUnits, budget.Units+p.ResourceUnits))
		}

		if err := s.injectFault("delegate-event"); err != nil {
			return err
		}
		// The delegating party is attributed to the parent holder when
		// the caller states nobody: a structural fact rather than a
		// claim. It is RECORDED either way, and it grants nothing.
		delegator := p.Actor
		if delegator.Address == "" {
			delegator = Actor{Address: parent.Recipient, Kind: "agent"}
		}
		if _, err := appendEvent(node, id, EventCreated, "", StateEnqueued,
			delegator, nil,
			"the child assignment was delegated", nil, now); err != nil {
			return err
		}

		row, err := getAssignmentTx(node, id)
		if err != nil {
			return err
		}
		out = row
		created = true
		return nil
	})
	if err != nil {
		return nil, false, err
	}
	return out, created, nil
}

// ── helpers ───────────────────────────────────────────────────────────────

// rootSpec is the description an idempotency key is compared against.
type rootSpec struct {
	projectID string
	parentID  string
	title     string
	objective string
	recipient string
	role      string
}

// sameLogicalAssignment reports whether the stored row describes the same
// logical assignment the request describes.
//
// The comparison covers the identity-bearing fields, not the timestamps
// or the budgets: two requests under one key that disagree about WHO the
// work is for, or WHAT it is, are two different assignments.
func sameLogicalAssignment(a *Assignment, spec rootSpec) bool {
	if spec.projectID != "" && a.ProjectID != spec.projectID {
		return false
	}
	if spec.parentID != "" && a.ParentID != spec.parentID {
		return false
	}
	if spec.title != "" && a.Title != spec.title {
		return false
	}
	if spec.objective != "" && a.Objective != spec.objective {
		return false
	}
	if spec.recipient != "" && a.Recipient != spec.recipient {
		return false
	}
	if spec.role != "" && a.Role != spec.role {
		return false
	}
	return true
}

// containsString reports whether the list holds the value.
func containsString(list []string, value string) bool {
	for _, v := range list {
		if v == value {
			return true
		}
	}
	return false
}

// marshalPayload renders the payload object the schema stores.
func marshalPayload(payload map[string]interface{}) (string, error) {
	if payload == nil {
		return "{}", nil
	}
	b, err := json.Marshal(payload)
	if err != nil {
		return "", fmt.Errorf("assignment: encode the payload: %w", err)
	}
	return string(b), nil
}
