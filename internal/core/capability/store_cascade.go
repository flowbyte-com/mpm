package capability

import (
	"database/sql"
	"errors"
	"fmt"
	"strings"

	"github.com/google/uuid"

	internal "github.com/flowbyte-com/mpm-core"
)

// =============================================================================
// store_cascade.go — ExecuteRollbackWithShatter (spec §5.2)
//
// This is the load-bearing cascade transaction. It owns the entire 7-step
// rollback flow as one atomic unit, and handles the "both broken"
// escalation (§5.2.1) as a sentinel-returned separate transaction.
//
// CTEs in this file are the cycle-safe descendants of the spec's
// §5.2 inline SQL (see store_query_test.go for cycle-detection tests).
// Both walkers are:
//
//   * Depth-capped (32 for ancestors, 5 for downstream) so corrupt
//     lineage chains cannot infinite-loop
//   * Path-tracked (path string + instr() check) so diamond graphs
//     do not produce duplicate shatter events
//
// The downstream walker's `path` is captured into the
// capability_events.metadata.shatter_path field so operators reading
// the audit log can see exactly how the blast radius reached the
// shattered node.
// =============================================================================

// ancestorsCTE is the cycle-safe lineage walker from spec §5.2 step 1,
// corrected for cycle protection. Returns the id, state of the closest
// *true* ancestor (depth > 0; excludes the revision itself) regardless
// of state, or no rows if the revision has no created_from_id chain.
//
// The caller decides what to do with the returned state:
//   * state = 'retired'  → predecessor is healthy; do the revive UPDATE
//   * state = anything else (fractured, rolled_back, ...) → both broken
//   * no row             → revision was a fresh capability; skip revive
//
// Bind: ? = revision_id
const ancestorsCTE = `
WITH RECURSIVE ancestors(
    id, state, created_from_id, depth, path
) AS (
    SELECT id, state, created_from_id, 0, ',' || id || ','
    FROM capabilities
    WHERE id = ? AND deleted_at IS NULL

    UNION ALL

    SELECT c.id, c.state, c.created_from_id, a.depth + 1,
           a.path || c.id || ','
    FROM capabilities c
    JOIN ancestors a ON c.id = a.created_from_id
    WHERE a.depth < 32
      AND instr(a.path, ',' || c.id || ',') = 0
)
SELECT id, state FROM ancestors
WHERE depth > 0
ORDER BY depth ASC
LIMIT 1
`

// downstreamCTE is the cycle-safe downstream walker from spec §5.2
// step 4, with depth cap (5) and path-string revisit guard against
// diamond dependencies. Returns (capability_id, depth, path, from_state)
// for every transitively dependent capability.
//
// Bind: ? = revision_id
const downstreamCTE = `
WITH RECURSIVE downstream(
    capability_id, depth, path, from_state
) AS (
    SELECT cd.capability_id, 0, ',' || cd.capability_id || ',', c.state
    FROM capability_dependencies cd
    JOIN capabilities c ON c.id = cd.capability_id
    WHERE cd.depends_on_id = ? AND c.deleted_at IS NULL

    UNION ALL

    SELECT cd.capability_id, d.depth + 1,
           d.path || cd.capability_id || ',', c.state
    FROM capability_dependencies cd
    JOIN downstream d ON cd.depends_on_id = d.capability_id
    JOIN capabilities c ON c.id = cd.capability_id
    WHERE d.depth < 5
      AND instr(d.path, ',' || cd.capability_id || ',') = 0
      AND c.deleted_at IS NULL
)
SELECT capability_id, depth, path, from_state FROM downstream
`

// ancestorInfo bundles the (id, state) of the closest lineage ancestor.
type ancestorInfo struct {
	id    string
	state CapabilityState
}

// findPredecessor walks the lineage and returns the closest ancestor of
// revisionID along with its state. Read-only; does not lock anything.
// Safe to call outside the rollback transaction for pre-flight checks.
//
// Returns ErrNotFound if the revision has no created_from_id chain at
// all (it was a fresh capability, never revised). Returns the closest
// ancestor regardless of state — callers check state to decide between
// revive (retired) and escalation (anything else).
func (s *Store) findPredecessor(revisionID string) (ancestorInfo, error) {
	if strings.TrimSpace(revisionID) == "" {
		return ancestorInfo{}, fmt.Errorf("capability: findPredecessor: revisionID is required")
	}
	var id, state string
	if err := s.dm.QueryRowTracked(ancestorsCTE, revisionID).Scan(&id, &state); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return ancestorInfo{}, ErrNotFound
		}
		return ancestorInfo{}, wrapDBError("findPredecessor", err)
	}
	if id == "" {
		return ancestorInfo{}, ErrNotFound
	}
	return ancestorInfo{id: id, state: CapabilityState(state)}, nil
}

// ExecuteRollbackWithShatter is the §5.2 transaction. It atomically:
//   1. Walks lineage to find the predecessor (pre-flight, no tx)
//   2. Demotes the failed revision to rolled_back
//   3. Revives the predecessor (if found and still retired)
//   4. Shatters downstream capabilities to needs_revision
//   5. Writes the operator/agent wake
//
// If step 3 finds the predecessor is NOT in state='retired' (i.e. it
// is fractured or otherwise broken), the entire transaction is rolled
// back and a SEPARATE escalation transaction (per §5.2.1) runs to
// mark the revision with metadata.escalated=true and write the
// high-priority operator wake.
//
// Returns the number of downstream capabilities shattered so the caller
// can surface it in CLI output. Returns ErrBothBroken when the
// escalation path runs.
func (s *Store) ExecuteRollbackWithShatter(
	revisionID, reason, tolerance string,
) (downstreamShattered int, err error) {
	// Pre-flight: find the closest ancestor regardless of state.
	// We need the id to know who to escalate against, AND the state
	// to decide whether to attempt revival.
	predecessor, predecessorErr := s.findPredecessor(revisionID)
	hasPredecessor := predecessorErr == nil
	if predecessorErr != nil && !errors.Is(predecessorErr, ErrNotFound) {
		return 0, predecessorErr
	}

	// Main transaction: steps 2-5.
	mainErr := s.withTx(func(n internal.DBNode) error {
		// 2. Demote revision to rolled_back.
		c, err := s.getCapabilityTx(n, revisionID)
		if err != nil {
			return err
		}
		// Spec §1.3: active → rolled_back is the legal entry. Refuse
		// if the revision is not active (cannot roll back a draft).
		if c.State != StateActive {
			return fmt.Errorf("%w: rollback requires state=active, got %s",
				ErrInvalidTransition, c.State)
		}
		now := s.Now()

		meta := EventMetadata{
			"tolerance_breached": tolerance,
			"observation_window": true,
		}
		if hasPredecessor {
			meta["predecessor_id"] = predecessor.id
		}

		// UPDATE revision.
		if _, err := n.ExecTracked(
			`UPDATE capabilities
			 SET state = 'rolled_back', state_changed_at = ?, updated_at = ?
			 WHERE id = ? AND state = 'active'`,
			3, now, now, revisionID,
		); err != nil {
			return wrapDBError("ExecuteRollbackWithShatter:demote", err)
		}
		// INSERT rollback event for revision.
		if err := s.insertTransitionEventTx(n, EventRollback, revisionID,
			StateActive, StateRolledBack, "scheduler", reason, meta); err != nil {
			return err
		}

		// 3. Revive predecessor — ONLY if it's still retired.
		// If the predecessor exists but is in any other state (fractured,
		// rolled_back, etc.), signal "both broken" so the outer wrapper
		// runs the escalation transaction.
		if hasPredecessor {
			if predecessor.state != StateRetired {
				return ErrBothBroken
			}
			res, err := n.ExecTracked(
				`UPDATE capabilities
				 SET state = 'active', state_changed_at = ?, updated_at = ?,
				     superseded_by_id = NULL
				 WHERE id = ? AND state = 'retired'`,
				3, now, now, predecessor.id,
			)
			if err != nil {
				return wrapDBError("ExecuteRollbackWithShatter:revive", err)
			}
			rowsAffected, _ := res.RowsAffected()
			if rowsAffected == 0 {
				// Race: predecessor state changed between pre-flight
				// and tx (someone else retired or fractured it).
				return ErrBothBroken
			}
			// INSERT promotion event for predecessor (rollback_revive).
			if err := s.insertTransitionEventTx(n, EventPromotion, predecessor.id,
				StateRetired, StateActive, "scheduler", "rollback_revive", nil); err != nil {
				return err
			}
		}

		// 4. Downstream shatter.
		shattered, err := s.shatterDownstreamTx(n, revisionID, now)
		if err != nil {
			return err
		}
		downstreamShattered = shattered

		// 5. Wake the agent / operator.
		return s.scheduleRollbackWakeTx(n, revisionID, reason, downstreamShattered, false)
	})

	if errors.Is(mainErr, ErrBothBroken) {
		// Rollback was already performed by WithTx; run the
		// §5.2.1 escalation in a separate transaction.
		var predecessorID string
		if hasPredecessor {
			predecessorID = predecessor.id
		}
		escErr := s.escalateRollbackTx(revisionID, predecessorID, reason)
		if escErr != nil {
			return 0, escErr
		}
		return 0, ErrBothBroken
	}
	if mainErr != nil {
		return 0, mainErr
	}
	return downstreamShattered, nil
}

// shatterDownstreamTx runs step 4 of §5.2: marks every transitively
// dependent capability as needs_revision and writes a dependency_shatter
// event for each one (with shatter_path in metadata so operators can
// see the dependency chain in the audit log).
//
// Diamond dependencies (one node reachable via multiple paths) are
// deduped in Go — the CTE produces one row per path, but each
// capability must shatter exactly once. First-occurrence-wins for the
// shatter_path (the shortest path; cycle guards prevent revisits
// within a single walk).
//
// Returns the number of capabilities shattered.
func (s *Store) shatterDownstreamTx(
	n internal.DBNode,
	revisionID string,
	now int64,
) (int, error) {
	// Run the downstream CTE to materialize the set of shattered nodes.
	rows, err := n.QueryTracked(downstreamCTE, revisionID)
	if err != nil {
		return 0, wrapDBError("shatterDownstreamTx:query", err)
	}
	type shatteredNode struct {
		id        string
		depth     int
		path      string
		fromState string
	}
	// Dedup by capability_id (first occurrence wins for path/depth).
	seen := make(map[string]bool)
	var nodes []shatteredNode
	for rows.Next() {
		var node shatteredNode
		if err := rows.Scan(&node.id, &node.depth, &node.path, &node.fromState); err != nil {
			rows.Close()
			return 0, wrapDBError("shatterDownstreamTx:scan", err)
		}
		if seen[node.id] {
			continue
		}
		seen[node.id] = true
		nodes = append(nodes, node)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return 0, wrapDBError("shatterDownstreamTx:rows", err)
	}
	if len(nodes) == 0 {
		return 0, nil
	}

	// UPDATE all shattered capabilities in one statement.
	// Use a parameterised IN clause — never string-interpolate ids.
	placeholders := make([]string, len(nodes))
	args := make([]interface{}, 0, len(nodes))
	for i, node := range nodes {
		placeholders[i] = "?"
		args = append(args, node.id)
	}
	updateQ := `UPDATE capabilities
	            SET state = 'needs_revision', state_changed_at = ?, updated_at = ?
	            WHERE id IN (` + strings.Join(placeholders, ",") + `)
	              AND deleted_at IS NULL`
	args = append([]interface{}{now, now}, args...)
	if _, err := n.ExecTracked(updateQ, 3, args...); err != nil {
		return 0, wrapDBError("shatterDownstreamTx:update", err)
	}

	// INSERT one dependency_shatter event per shattered node, with
	// the path in metadata so operators can read the cascade route.
	for _, node := range nodes {
		meta := EventMetadata{
			"cascade_depth": node.depth,
			"shatter_path":  node.path,
		}
		fromState := CapabilityState(node.fromState)
		if err := s.insertTransitionEventTx(n, EventDependencyShatter,
			node.id, fromState, StateNeedsRevision, "scheduler",
			"upstream_rolled_back", meta); err != nil {
			return 0, err
		}
		// Stamp related_id on the dependency_shatter event so audit
		// log readers can pivot to the rolling-back capability.
		// SQLite disallows ORDER BY in UPDATE; use a subquery to find
		// the most-recent un-stamped shatter event for this node.
		if _, err := n.ExecTracked(
			`UPDATE capability_events SET related_id = ?
			 WHERE id = (
			   SELECT id FROM capability_events
			   WHERE capability_id = ? AND event_type = 'dependency_shatter'
			     AND related_id IS NULL
			   ORDER BY occurred_at DESC LIMIT 1
			 )`,
			3, revisionID, node.id,
		); err != nil {
			return 0, wrapDBError("shatterDownstreamTx:stamp_related", err)
		}
	}
	return len(nodes), nil
}

// scheduleRollbackWakeTx writes a scheduled_wakes row that fires the
// agent (or operator) immediately. Uses s.Now() as target_time and
// stamps a UUID id + created_by for the schema's NOT NULL constraints.
func (s *Store) scheduleRollbackWakeTx(
	n internal.DBNode,
	revisionID, reason string,
	downstreamCount int,
	escalated bool,
) error {
	reasonTag := "capability_rollback"
	if escalated {
		reasonTag = "capability_rollback_escalated"
	}
	meta := EventMetadata{
		"skill_id":            revisionID,
		"reason":              reason,
		"downstream_shattered": downstreamCount,
	}
	if escalated {
		meta["escalated"] = true
	}

	q := `INSERT INTO scheduled_wakes
	      (id, target_time, reason, created_by, metadata)
	      VALUES (?, ?, ?, ?, ?)`
	_, err := n.ExecTracked(q, 3,
		uuid.New().String(), s.Now(), reasonTag,
		"capability_scheduler", meta,
	)
	return wrapDBError("scheduleRollbackWakeTx", err)
}

// escalateRollbackTx is the §5.2.1 "both broken" escalation path.
// It runs in a SEPARATE transaction after the main rollback tx has
// rolled back, marks the revision as escalated, and writes a
// high-priority operator wake.
func (s *Store) escalateRollbackTx(
	revisionID, predecessorID, reason string,
) error {
	return s.withTx(func(n internal.DBNode) error {
		// Confirm the revision is still in state='active' (the main
		// rollback rolled back, so the state change did NOT commit).
		c, err := s.getCapabilityTx(n, revisionID)
		if err != nil {
			return err
		}
		if c.State != StateActive {
			return fmt.Errorf("capability: escalateRollbackTx: revision is in state %s, expected active", c.State)
		}

		now := s.Now()

		// Mark revision as rolled_back with escalated=true in metadata.
		// The rolled_back state preserves lineage; the metadata flag
		// distinguishes "rolled back successfully" from "escalated".
		if _, err := n.ExecTracked(
			`UPDATE capabilities
			 SET state = 'rolled_back', state_changed_at = ?, updated_at = ?,
			     metadata = json_set(COALESCE(metadata, '{}'), '$.escalated', json('true'))
			 WHERE id = ? AND state = 'active'`,
			3, now, now, revisionID,
		); err != nil {
			return wrapDBError("escalateRollbackTx:demote", err)
		}

		// Rollback event with escalated metadata.
		meta := EventMetadata{
			"escalated":       true,
			"predecessor_id":  predecessorID,
			"predecessor_unavailable": true,
		}
		if err := s.insertTransitionEventTx(n, EventRollback, revisionID,
			StateActive, StateRolledBack, "scheduler",
			"predecessor_unavailable", meta); err != nil {
			return err
		}

		// High-priority operator wake.
		return s.scheduleRollbackWakeTx(n, revisionID, reason, 0, true)
	})
}
