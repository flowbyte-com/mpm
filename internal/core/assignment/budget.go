package assignment

import (
	"fmt"

	internal "github.com/flowbyte-com/mpm-core"
)

// The AGGREGATE BUDGETS.
//
// PR 1 stored three budgets on the root and deliberately deferred their
// enforcement to this phase:
//
//	max_tasks          the number of assignments in the tree
//	max_concurrency    the number of assignments in the tree holding a LIVE
//	                   claim at the same time
//	max_resource_units the sum of the tree's DECLARED resource_units
//
// # WHAT resource_units MEANS
//
// `resource_units` is a DECLARED RESERVATION, fixed once when the
// assignment is created. The schema says it plainly: "What this assignment
// costs against the root's cumulative budget."
//
// It is NOT a measurement of provider spend. MPM does not meter tokens, and
// the sidecar's audit records request and response SIZES, not cost. So this
// budget is a ceiling on the work the OPERATOR AUTHORISED, and it must not
// be described as a verified monetary or token-spend ceiling.
//
// # A RETRY DOES NOT CHARGE AGAIN
//
// A retry is a new ATTEMPT on the same assignment row, and the reservation
// lives on the row. Charging per attempt would let a failing task bill the
// root repeatedly for one logical task; charging per row charges it once.
//
// # ATOMICITY, AND WHY IT IS NOT OPTIONAL
//
// A budget check that reads the count and then writes is a check that can
// be crossed: another dispatcher, or a direct caller of the assignment API,
// can add a task between the two operations, and both writers then believe
// they were inside the budget.
//
// So the count and the write are ONE STATEMENT. The INSERT carries the
// budget condition in its WHERE clause and the UPDATE carries it in its
// own, so the database evaluates the count against the same snapshot that
// the write lands on. A writer whose snapshot goes stale in between is
// refused by SQLite (SQLITE_BUSY_SNAPSHOT) and the transaction is retried
// against a fresh snapshot, which re-evaluates the count.
//
// That is why the checks live HERE, in the assignment service, and not in
// the dispatcher: a dispatcher-side check protects only the dispatcher.

// ErrBudgetExhausted says a tree's aggregate budget is spent.
//
// It is a distinct error rather than a generic constraint refusal because
// the caller has to be able to tell "this work may not be created" from
// "this work is malformed" — the first is a budget outcome that a
// dispatcher reports and retries later, the second is a bug.
var ErrBudgetExhausted = fmt.Errorf(
	"assignment: the tree's aggregate budget is exhausted")

// budgetRefusal names the budget that refused, what it allows and what the
// tree currently holds.
//
// The numbers are part of the message on purpose: an operator reading a
// dispatch report has to be able to see WHY the tree is full without
// opening the database.
//
// The detail adds what the WRITE would have added, when the refusal is
// about a write that has not happened yet. "The tree holds 90" and "the
// tree would hold 100" are different facts, and a refusal that mixes
// them is a refusal an operator cannot act on.
func budgetRefusal(budget string, allowed, used int64, detail string) error {
	msg := fmt.Sprintf("%s allows %d and the tree holds %d", budget, allowed, used)
	if detail != "" {
		msg += ", " + detail
	}
	return fmt.Errorf("%w: %s", ErrBudgetExhausted, msg)
}

// The three budget conditions, as SQL fragments.
//
// Each is a boolean expression over the tree, evaluated INSIDE the writing
// statement. The arguments are the root id, the row's own declared
// resource_units, and — for the concurrency condition — the moment the
// lease is judged alive.
//
// max_tasks counts the ROOT itself: the root's tree is the set of rows
// whose root_id names it, and the root is one of them (a root's root_id is
// its own id). So max_tasks is the TOTAL number of assignments in the
// tree, not the number of children.

// taskBudgetCondition is the max_tasks condition for an INSERT.
const taskBudgetCondition = `
	(SELECT COUNT(*) FROM agent_tasks WHERE root_id = ?)
		< (SELECT COALESCE(max_tasks, 0) FROM agent_tasks WHERE id = ?)`

// resourceBudgetCondition is the max_resource_units condition for an
// INSERT.
//
// The sum is taken over the tree and the new row's own declared units are
// added, so the condition is exactly "the tree after this insert is still
// inside the budget".
const resourceBudgetCondition = `
	(SELECT COALESCE(SUM(resource_units), 0) FROM agent_tasks WHERE root_id = ?)
		+ ? <= (SELECT COALESCE(max_resource_units, 0) FROM agent_tasks WHERE id = ?)`

// concurrencyBudgetCondition is the max_concurrency condition for a claim.
//
// A claim is LIVE when it holds a claim epoch, its lease has not expired,
// AND the work is UNFINISHED. The state filter matters: a finished
// assignment keeps the lease it was given (the lease is not released on
// completion), so counting leases alone would let the tree fill with
// finished work and refuse the next real task.
//
// The unfinished states are listed rather than excluded, so a state this
// package adds later does not silently start counting.
//
// The assignment being claimed is excluded: it is either unclaimed (no
// epoch) or being reclaimed (its lease has expired), so it is not one of
// the live claims — and excluding it says so explicitly rather than
// relying on that reasoning holding at every call site.
const concurrencyBudgetCondition = `
	(SELECT COUNT(*)
	   FROM agent_tasks t
	  WHERE t.root_id = (SELECT root_id FROM agent_tasks WHERE id = ?)
	    AND t.claim_epoch > 0
	    AND t.lease_expires_at IS NOT NULL
	    AND t.lease_expires_at > ?
	    AND t.state IN ('enqueued', 'delivered', 'acknowledged', 'accepted', 'in_progress')
	    AND t.id <> ?)
		< (SELECT COALESCE(max_concurrency, 0) FROM agent_tasks WHERE id = ?)`

// liveClaimPredicate is the same liveness rule, as a fragment, for the
// read paths that report the figures.
//
// It is deliberately ONE expression, shared by the write condition and the
// read paths, so the number an operator is shown is the number the write
// was judged against.
const liveClaimPredicate = `
	t.claim_epoch > 0
	AND t.lease_expires_at IS NOT NULL
	AND t.lease_expires_at > ?
	AND t.state IN ('enqueued', 'delivered', 'acknowledged', 'accepted', 'in_progress')`

// The budget figures a refusal message needs.
type budgetSnapshot struct {
	Tasks         int64
	MaxTasks      int64
	Units         int64
	MaxUnits      int64
	LiveClaims    int64
	MaxConcurrent int64
}

// readBudgetSnapshot reads the tree's budget figures for a MESSAGE.
func readBudgetSnapshot(node internal.DBNode, rootID string, now int64) (budgetSnapshot, error) {
	var b budgetSnapshot
	row := node.QueryRowTracked(`
		SELECT
			(SELECT COUNT(*) FROM agent_tasks WHERE root_id = ?),
			(SELECT COALESCE(max_tasks, 0) FROM agent_tasks WHERE id = ?),
			(SELECT COALESCE(SUM(resource_units), 0) FROM agent_tasks WHERE root_id = ?),
			(SELECT COALESCE(max_resource_units, 0) FROM agent_tasks WHERE id = ?),
			(SELECT COUNT(*) FROM agent_tasks t
			  WHERE t.root_id = ? AND `+liveClaimPredicate+`),
			(SELECT COALESCE(max_concurrency, 0) FROM agent_tasks WHERE id = ?)`,
		rootID, rootID, rootID, rootID, rootID, now, rootID)
	if err := row.Scan(&b.Tasks, &b.MaxTasks, &b.Units, &b.MaxUnits,
		&b.LiveClaims, &b.MaxConcurrent); err != nil {
		return budgetSnapshot{}, fmt.Errorf("assignment: read the tree's budgets: %w", err)
	}
	return b, nil
}

// BudgetStatus is the operator-visible budget picture of one tree.
//
// It is a READ, and it is what a status report shows. It never decides
// anything: the decisions are the writing statements' own WHERE clauses.
type BudgetStatus struct {
	RootID string `json:"root_id"`
	// Tasks is the number of assignments in the tree, the root included.
	Tasks int64 `json:"tasks"`
	// MaxTasks is the root's ceiling on that number.
	MaxTasks int64 `json:"max_tasks"`
	// Units is the tree's summed DECLARED resource_units.
	Units int64 `json:"units"`
	// MaxUnits is the root's ceiling on that sum.
	MaxUnits int64 `json:"max_units"`
	// LiveClaims is the number of assignments in the tree holding a live
	// lease right now.
	LiveClaims int64 `json:"live_claims"`
	// MaxConcurrent is the root's ceiling on simultaneous claims.
	MaxConcurrent int64 `json:"max_concurrency"`
}

// BudgetStatus reads the budget picture of one tree.
func (s *Store) BudgetStatus(rootID string) (BudgetStatus, error) {
	if err := requireText("root_id", rootID); err != nil {
		return BudgetStatus{}, err
	}
	now := s.Now()
	rows, err := s.db.SQLDB().Query(`
		SELECT
			(SELECT COUNT(*) FROM agent_tasks WHERE root_id = ?),
			(SELECT COALESCE(max_tasks, 0) FROM agent_tasks WHERE id = ?),
			(SELECT COALESCE(SUM(resource_units), 0) FROM agent_tasks WHERE root_id = ?),
			(SELECT COALESCE(max_resource_units, 0) FROM agent_tasks WHERE id = ?),
			(SELECT COUNT(*) FROM agent_tasks t
			  WHERE t.root_id = ? AND `+liveClaimPredicate+`),
			(SELECT COALESCE(max_concurrency, 0) FROM agent_tasks WHERE id = ?)`,
		rootID, rootID, rootID, rootID, rootID, now, rootID)
	if err != nil {
		return BudgetStatus{}, fmt.Errorf("assignment: read the tree's budgets: %w", err)
	}
	defer rows.Close()
	var out BudgetStatus
	if !rows.Next() {
		return BudgetStatus{}, fmt.Errorf("assignment: read the tree's budgets: %w", ErrNotFound)
	}
	out.RootID = rootID
	if err := rows.Scan(&out.Tasks, &out.MaxTasks, &out.Units,
		&out.MaxUnits, &out.LiveClaims, &out.MaxConcurrent); err != nil {
		return BudgetStatus{}, fmt.Errorf("assignment: read the tree's budgets: %w", err)
	}
	return out, rows.Err()
}
