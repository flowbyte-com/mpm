package assignment

import (
	"database/sql"
	"errors"
	"fmt"
	"strings"

	internal "github.com/flowbyte-com/mpm-core"
)

// The PROJECT identity.
//
// A project is the permanent stub the project model defines: an identity
// that outlives every thread, checkpoint and session. The row already
// exists in the schema; this file adds the ONE missing operation — the
// operator's supported way to create it — and nothing else.
//
// # WHY THIS IS OPERATOR-ONLY
//
// A project is the containment boundary every assignment names, so an
// agent-facing creation path would let an agent mint a project (and then
// a root inside it). The operation takes an OperatorAuthority, and it is
// absent from the tool registry entirely: there is no action to dispatch
// to.
//
// # WHAT THIS IS NOT
//
// It is not a project-management API. It does not rename, list, delete or
// resolve projects, and it does not touch `project_names`. The pilot and
// `create-root` address a project by its id; the name-resolution domain
// is a separate design that this change does not anticipate.

// ProjectIDPrefix is the prefix the project model gives every identity.
//
// The schema's comment is the source: 'cpj-' followed by 16 lowercase
// hexadecimal characters. Enforcing it here keeps a CLI-created identity
// the same shape as one the model defines, rather than letting the
// operator's command become a second, looser constructor.
const ProjectIDPrefix = "cpj-"

// Project is one row of the projects table, projected.
type Project struct {
	// ID is the project's identity: `cpj-` and 16 lowercase hex.
	ID string `json:"id"`
	// CreatedAt and UpdatedAt are unix seconds.
	CreatedAt int64 `json:"created_at"`
	UpdatedAt int64 `json:"updated_at"`
	// CreatedBy is the WRITER IDENTITY, not an owner. It is attribution
	// recorded from the operator's own command.
	CreatedBy string `json:"created_by,omitempty"`
	// DeletedAt is non-nil for a withdrawn identity.
	DeletedAt *int64 `json:"deleted_at,omitempty"`
	// DeletedBy is the writer identity of the withdrawal.
	DeletedBy string `json:"deleted_by,omitempty"`
}

// ProjectParams is the input of CreateProject.
//
// The request names one thing, the identity, because that is the whole of
// a project's own state. Everything else the schema carries is either a
// timestamp or a writer identity the service supplies.
type ProjectParams struct {
	// ID is the project identity to create.
	ID string
}

// CreateProject creates a project identity.
//
// # IDEMPOTENCY
//
// The request is the identity itself, so a repeated request for a LIVE
// project is the same request: it returns the existing row and reports
// created=false. No second row is written, and no field is rewritten.
//
// A WITHDRAWN identity is a CONFLICT, not an idempotent hit. The row is
// history (`deleted_at` is set), and creating it again would either
// resurrect an identity the model says is gone or collide with the
// history. The operation refuses, and the operator must choose another
// id.
//
// # CONCURRENCY
//
// Creation runs in one transaction. Two concurrent creators race on the
// primary key; the loser reads the winner's row back and reports
// created=false. The result never claims to have created a project that
// another writer created first.
//
// The returned bool is true when THIS call created the row.
func (s *Store) CreateProject(auth OperatorAuthority, p ProjectParams) (*Project, bool, error) {
	// THE AUTHORITY IS THE CALL BOUNDARY.
	//
	// It is not authentication: MPM is a single-operator trust domain, and
	// the actor string is attribution. What it does is make the operator
	// path explicit and keep the operation off the agent surface.
	if auth == nil {
		return nil, false, fmt.Errorf("%w: project creation is an operator operation", ErrNotAuthorised)
	}
	actor := auth.Actor()

	// THE IDENTITY IS EXACT. No trimming: a caller that typed padding
	// asked for a different string, and silently accepting the trimmed
	// form would make the identity the service records differ from the
	// one the caller sent.
	id := p.ID
	if err := requireText("project_id", id); err != nil {
		return nil, false, err
	}
	if !validProjectID(id) {
		return nil, false, fmt.Errorf(
			"%w: the project id %q is not %q followed by 16 lowercase hexadecimal characters",
			ErrConstraint, id, ProjectIDPrefix)
	}

	var out *Project
	created := false
	err := s.db.WithTx(func(node internal.DBNode) error {
		existing, err := getProjectTx(node, id)
		if err != nil {
			return err
		}
		if existing != nil {
			if existing.DeletedAt != nil {
				return fmt.Errorf(
					"%w: the project %q was withdrawn; an identity is not reused",
					ErrConstraint, id)
			}
			out = existing
			return nil
		}

		if err := s.injectFault("create-project"); err != nil {
			return err
		}

		now := s.Now()
		if _, err := node.ExecTracked(`
			INSERT INTO projects (id, created_at, updated_at, created_by)
			VALUES (?, ?, ?, ?)`,
			5,
			id, now, now, actor.Address,
		); err != nil {
			// THE RACE LOSER READS THE WINNER BACK. A primary-key
			// collision is not a failure of the request: the request
			// was "make this identity exist", and it does.
			if strings.Contains(err.Error(), "UNIQUE constraint failed: projects.id") {
				winner, ferr := getProjectTx(node, id)
				if ferr != nil {
					return ferr
				}
				if winner != nil {
					if winner.DeletedAt != nil {
						return fmt.Errorf(
							"%w: the project %q was withdrawn; an identity is not reused",
							ErrConstraint, id)
					}
					out = winner
					return nil
				}
			}
			return storageRefusal(err)
		}

		row, err := getProjectTx(node, id)
		if err != nil {
			return err
		}
		if row == nil {
			return fmt.Errorf("assignment: the project %q was written but cannot be read back", id)
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

// validProjectID reports whether an id is the project model's shape.
//
// It is deliberately strict: lowercase hexadecimal only, so `cpj-ABC…`
// and `cpj-…-suffix` are different strings that cannot be mistaken for
// the same identity by a resolver that normalises case later.
func validProjectID(id string) bool {
	if !strings.HasPrefix(id, ProjectIDPrefix) {
		return false
	}
	rest := strings.TrimPrefix(id, ProjectIDPrefix)
	if len(rest) != 16 {
		return false
	}
	for _, c := range rest {
		if (c < '0' || c > '9') && (c < 'a' || c > 'f') {
			return false
		}
	}
	return true
}

// getProjectTx reads one project row.
//
// The columns are the schema's; `created_by` and the deletion fields are
// nullable, so they are scanned into Null* values and projected as the
// zero value / nil rather than failing the read.
func getProjectTx(node internal.DBNode, id string) (*Project, error) {
	var p Project
	var createdBy, deletedBy sql.NullString
	var deletedAt sql.NullInt64
	err := node.QueryRowTracked(`
		SELECT id, created_at, updated_at, created_by, deleted_at, deleted_by
		FROM projects WHERE id = ?`, id).
		Scan(&p.ID, &p.CreatedAt, &p.UpdatedAt, &createdBy, &deletedAt, &deletedBy)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("assignment: read the project %q: %w", id, err)
	}
	p.CreatedBy = createdBy.String
	p.DeletedBy = deletedBy.String
	if deletedAt.Valid {
		at := deletedAt.Int64
		p.DeletedAt = &at
	}
	return &p, nil
}
