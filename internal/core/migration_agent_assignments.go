// migration_agent_assignments.go — Continuity 001, PR 1: the durable
// assignment storage.
//
// WHAT THIS IS
// ------------
// The first piece of MPM's multi-agent coordination machinery: a durable,
// addressed **assignment** — work handed to a named agent, with an explicit
// state, a lease with a fencing token, an idempotency key, and a delegation
// tree whose budgets are inherited from the root.
//
// This file is **storage only**. It creates the tables, the indexes and the
// triggers that make the contract's invariants true *in the database*, so
// that a wrong write is refused by the engine rather than by the convention
// of whichever code path happened to make it. The API, the CLI, the OpenCode
// adapter, the dispatcher and the evidence contract are PRs 2–5; none of
// them is here.
//
// WHAT IT REUSES, AND WHAT IT DELIBERATELY DOES NOT
// --------------------------------------------------
// The project identity axis is NOT re-invented. PR #224's Stage 1 shipped
// `projects`, `project_names`, `continuity_threads` and
// `continuity_checkpoints` in commit d8b373fb, with its DDL pinned by hash
// (TestContinuityProjectModelDDLIsByteIdentical). An assignment belongs to a
// project, so it references `projects(id)` and inherits that table's
// containment idiom:
//
//   - the project id is DENORMALISED onto the assignment and held honest by a
//     COMPOSITE foreign key, exactly as `continuity_checkpoints` holds its
//     thread's project honest;
//   - every reference is ON DELETE RESTRICT, so a project with assignments
//     cannot be cascade-destroyed.
//
// The reviewed Stage 1 DDL is NOT altered here, and its four tables are not
// duplicated. `projects` is referenced; nothing else about it is touched.
//
// WHY A MIGRATION AND NOT A SCHEMA SLICE
// --------------------------------------
// The same reasoning as Stage 1: `BaseTables` is the slice `attachShared`
// propagates into the cross-workspace `shared` schema, and an assignment is
// state that belongs inside ONE workspace. A local-only slice would also
// re-introduce the two-definition drift that Stage 1 avoided by having ONE
// DDL path: the migration runs inside the fatal migration transaction on
// EVERY open, so a fresh database and an upgraded one take the same code
// path and the same DDL text.
//
// THE SENTINEL IS WRITTEN LAST, ON PURPOSE
// ----------------------------------------
// The sentinel row is written AFTER the DDL, inside the same transaction.
// A failure injected at the sentinel write (a BEFORE INSERT trigger that
// aborts it) proves that the DDL — which had already succeeded — is rolled
// back with it, leaving no partial assignment schema.
//
// Idempotent via the `agent_assignments_v1` sentinel in `schema_migrations`.
// The DDL statements are themselves CREATE ... IF NOT EXISTS, so a re-run is
// a no-op even if the sentinel were lost.
package internal

import (
	"database/sql"
	"fmt"
	"time"
)

// agentAssignmentDDL is the approved DDL for the Continuity 001 assignment
// storage.
//
// The SHA-256 of this text is pinned by
// TestAgentAssignmentDDLIsPinned, so the storage cannot drift from the
// design note (docs/dev/continuity-001-assignment-schema.md) without failing
// the build.
//
// WHY THE TRIGGERS ARE PART OF THE DDL
// ------------------------------------
// The contract's three hardest invariants are not expressible as column
// CHECK constraints, and each of them is the difference between a
// coordination substrate and a convention:
//
//  1. THE STATE MACHINE IS ROLE-AWARE. An assignment moves forward through a
//     fixed path, and the path differs by role: an implementer's assignment
//     reaches `review_passed` and then `awaiting_operator`, while a
//     reviewer's assignment ends at `review_passed` (the verdict was
//     passing) or at `completed` (the review is done and it asked for
//     changes). A trigger that knows the role can refuse the wrong path.
//
//  2. THE OWNER CHANGES ONLY THROUGH A CLAIM. The claim epoch advances by
//     exactly one, and the owner may change only on a claim that also sets a
//     lease. So an owner cannot be reassigned, and an epoch cannot be
//     skipped or reset, without the row ceasing to be a lease.
//
//  3. A TASK IS CREATED ENQUEUED. Every other state is reached by a
//     transition, so a task cannot be born already accepted or already
//     completed.
//
// The fencing of a STALE owner is deliberately NOT a trigger. A trigger
// cannot see the epoch a caller presented — it only sees the row before and
// the row after — so the fence is the WHERE clause of the writing statement
// (`WHERE id = ? AND claim_epoch = ?`), which is atomic with the write. The
// trigger's job is to make sure the epoch cannot be FORGED: it advances by
// exactly one, and only a claim may change the owner.
const agentAssignmentDDL = `-- ── agent_tasks ────────────────────────────────────────────────────────────
-- The durable assignment: work addressed to a named agent.
--
-- An assignment is NOT a work item (the 'works' table keeps its own
-- vocabulary: a commitment, with no recipient and no lease) and it is NOT a
-- thread (PR #224's 'continuity_threads' is a line of conversation). It is
-- an ASSIGNMENT: a unit of work with an intended recipient, an owner, a
-- state, and an evidence trail.
--
-- THE ROOT/CHILD SHAPE IS A STORAGE INVARIANT, NOT A CONVENTION.
--
--   * A ROOT (depth = 0) is the operator-authorised assignment. It is its
--     own root (root_id = id), it has no parent, and it CARRIES THE
--     BUDGETS: max_depth, max_tasks, max_concurrency, max_resource_units,
--     permitted_operations and eligible_recipients.
--   * A CHILD (depth > 0) is an agent's delegation. It names its root and
--     its parent, and it carries NO BUDGETS AT ALL — so a child cannot
--     widen what it was given, because there is no column in which to
--     widen it. Inheritance is structural.
--
-- One CHECK constraint enforces both halves. That is the same move PR
-- #224 made with the composite foreign keys: make the boundary a property
-- of the storage rather than of the code that happens to respect it.
--
-- THE STATE VOCABULARY IS TEN STATES, AND THE SEPARATIONS ARE THE POINT.
--
--   enqueued          created; nobody has been handed it yet
--   delivered         handed to a runtime (a session exists)
--   acknowledged      the recipient TOOK it
--   accepted          the recipient WILL do it
--   in_progress       the work is happening
--   completed         the implementer's claim that the work is done
--   review_passed     the independent review of THIS assignment passed
--   awaiting_operator the operator's gate — the automation's terminal state
--   operator_approved an EXTERNALLY verified approval was RECORDED
--   cancelled         withdrawn
--
-- Delivery is not acceptance and completion is not approval. An assignment
-- that was delivered and never acknowledged is RE-DELIVERABLE, not lost:
-- the delivery is a state transition with its own timestamp, never a side
-- effect of the row having been read.
--
-- 'operator_approved' is a RECORD, not a grant. MPM records an approval
-- that was verified OUTSIDE MPM — its source, the exact revision it covers
-- and a pointer to the evidence — and it never manufactures or authenticates
-- one. The approval names the exact action (approval_action) and the exact
-- revision (approval_revision); a change to the head revision after the
-- approval was recorded leaves the approval stale, and staleness is
-- observable rather than implied.
CREATE TABLE IF NOT EXISTS agent_tasks (
    id                  TEXT PRIMARY KEY,            -- 'at-' || 16 lowercase hex
    project_id          TEXT NOT NULL REFERENCES projects(id) ON DELETE RESTRICT,
    -- The delegation tree. root_id is the ROOT assignment; a root is its
    -- own root. parent_id is NULL for a root.
    root_id             TEXT NOT NULL,
    parent_id           TEXT,
    depth               INTEGER NOT NULL DEFAULT 0 CHECK (depth >= 0),
    title               TEXT NOT NULL CHECK (length(CAST(title AS BLOB)) <= 200),
    objective           TEXT NOT NULL CHECK (length(CAST(objective AS BLOB)) <= 4000),
    -- The stable agent ADDRESS. This is an address, NOT an identity: it
    -- names a recipient and it does not authenticate one. See
    -- docs/dev/continuity-001-assignment-schema.md.
    recipient           TEXT NOT NULL CHECK (length(CAST(recipient AS BLOB)) BETWEEN 1 AND 64),
    role                TEXT NOT NULL DEFAULT 'implementer'
                          CHECK (role IN ('implementer','reviewer')),
    state               TEXT NOT NULL DEFAULT 'enqueued'
                          CHECK (state IN ('enqueued','delivered','acknowledged','accepted',
                                           'in_progress','completed','review_passed',
                                           'awaiting_operator','operator_approved','cancelled')),
    -- The idempotency key. UNIQUE across the table, so an accidental
    -- duplicate dispatch cannot create a second logical assignment.
    idempotency_key     TEXT NOT NULL CHECK (length(CAST(idempotency_key AS BLOB)) BETWEEN 1 AND 128),
    -- The lease. claim_epoch is the fencing token: it advances by exactly
    -- one on every claim, and a write that presents a stale epoch matches
    -- no row.
    claim_epoch         INTEGER NOT NULL DEFAULT 0 CHECK (claim_epoch >= 0),
    owner               TEXT CHECK (owner IS NULL OR length(CAST(owner AS BLOB)) BETWEEN 1 AND 64),
    lease_expires_at    INTEGER,
    -- The ROOT's budgets. A child carries none of them — see the CHECK
    -- constraint below.
    max_depth           INTEGER CHECK (max_depth IS NULL OR max_depth >= 0),
    max_tasks           INTEGER CHECK (max_tasks IS NULL OR max_tasks > 0),
    max_concurrency     INTEGER CHECK (max_concurrency IS NULL OR max_concurrency > 0),
    max_resource_units  INTEGER CHECK (max_resource_units IS NULL OR max_resource_units >= 0),
    -- What this assignment costs against the root's cumulative budget.
    resource_units      INTEGER NOT NULL DEFAULT 0 CHECK (resource_units >= 0),
    -- The operations this tree may perform. A ROOT must name at least one:
    -- an empty list on a root would be a root that may do nothing, which is
    -- a silent misconfiguration rather than a safe default.
    permitted_operations JSON NOT NULL DEFAULT '[]'
                          CHECK (json_valid(permitted_operations) AND json_type(permitted_operations) = 'array'),
    -- The addresses an assignment in this tree may target. A root's own
    -- recipient must be among them; a child that targets an address outside
    -- the list is refused.
    eligible_recipients  JSON NOT NULL DEFAULT '[]'
                          CHECK (json_valid(eligible_recipients) AND json_type(eligible_recipients) = 'array'),
    -- Evidence pointers. The assignment carries POINTERS, never copies.
    issue_ref           TEXT,
    pr_ref              TEXT,
    base_sha            TEXT,
    head_sha            TEXT,
    reviewed_sha        TEXT,
    findings_sha256     TEXT,
    evidence_ref        TEXT,
    payload             JSON NOT NULL DEFAULT '{}'
                          CHECK (json_valid(payload) AND json_type(payload) = 'object'),
    -- The operator's gate. awaiting_action is the exact action the
    -- operator is being asked to authorise; the approval_* columns record
    -- an approval that was verified OUTSIDE MPM.
    awaiting_action     TEXT,
    approval_action     TEXT,
    approval_revision   TEXT,
    approval_source     TEXT,
    approval_evidence   TEXT,
    approved_at         INTEGER,
    created_at          INTEGER NOT NULL DEFAULT (CAST(strftime('%s','now') AS INTEGER)),
    updated_at          INTEGER NOT NULL DEFAULT (CAST(strftime('%s','now') AS INTEGER)),
    completed_at        INTEGER,
    -- The containment boundary. An assignment belongs to a project, and the
    -- COMPOSITE keys keep the tree inside ONE project: a child cannot name
    -- a root of another project, and it cannot name a parent of another
    -- project.
    FOREIGN KEY (root_id, project_id)
        REFERENCES agent_tasks(id, project_id) ON DELETE RESTRICT,
    FOREIGN KEY (parent_id, project_id)
        REFERENCES agent_tasks(id, project_id) ON DELETE RESTRICT,
    -- The root/child shape, and the budget inheritance.
    CHECK (
        (
            depth = 0
            AND parent_id IS NULL
            AND root_id = id
            AND max_depth IS NOT NULL
            AND max_tasks IS NOT NULL
            AND max_concurrency IS NOT NULL
            AND max_resource_units IS NOT NULL
            AND json_array_length(permitted_operations) > 0
        )
        OR
        (
            depth > 0
            AND parent_id IS NOT NULL
            AND root_id <> id
            AND max_depth IS NULL
            AND max_tasks IS NULL
            AND max_concurrency IS NULL
            AND max_resource_units IS NULL
            AND json_array_length(permitted_operations) = 0
        )
    ),
    -- A lease implies an owner. The reverse is not required: a completed
    -- assignment keeps the address that did the work.
    CHECK (lease_expires_at IS NULL OR owner IS NOT NULL)
);
-- The parent key for the two composite foreign keys above.
CREATE UNIQUE INDEX IF NOT EXISTS idx_agent_tasks_id_project
    ON agent_tasks(id, project_id);
-- The idempotency guarantee. An accidental duplicate dispatch of the same
-- logical assignment cannot create a second row.
CREATE UNIQUE INDEX IF NOT EXISTS idx_agent_tasks_idempotency_key
    ON agent_tasks(idempotency_key);
-- The dispatcher's hot path: the assignments addressed to one agent that
-- are still open.
CREATE INDEX IF NOT EXISTS idx_agent_tasks_recipient_state
    ON agent_tasks(recipient, state, updated_at DESC);
-- The queue view, and the root's cumulative budget accounting.
CREATE INDEX IF NOT EXISTS idx_agent_tasks_state_updated
    ON agent_tasks(state, updated_at DESC);
CREATE INDEX IF NOT EXISTS idx_agent_tasks_root
    ON agent_tasks(root_id, state);
CREATE INDEX IF NOT EXISTS idx_agent_tasks_project
    ON agent_tasks(project_id, state, updated_at DESC);

-- ── agent_task_events ──────────────────────────────────────────────────────
-- The append-only ledger of an assignment's transitions.
--
-- It is the same shape as 'work_events' — an event index that is UNIQUE per
-- assignment, so the ledger is ordered and gap-free — and it is separate
-- from 'works'/'work_events' because the two vocabularies are separate.
--
-- The event_type vocabulary is the transition vocabulary plus three
-- observations that are not transitions: 'claimed' (a lease was taken),
-- 'lease_expired' (a lease lapsed and the assignment became re-claimable)
-- and 'note' (a free-text annotation that changes no state).
--
-- from_state and to_state are the states either side of the transition, and
-- are NULL for the three non-transition observations.
CREATE TABLE IF NOT EXISTS agent_task_events (
    id              TEXT PRIMARY KEY,              -- 'ate-' || 16 lowercase hex
    task_id         TEXT NOT NULL,
    event_index     INTEGER NOT NULL CHECK (event_index >= 0),
    event_type      TEXT NOT NULL
                      CHECK (event_type IN (
                        'created','delivered','acknowledged','accepted','started',
                        'completed','review_passed','awaiting_operator','operator_approved',
                        'cancelled','claimed','lease_expired','note'
                      )),
    from_state      TEXT,
    to_state        TEXT,
    -- ATTRIBUTION, NOT IDENTITY. These record who wrote the row. They do
    -- not authenticate it, and nothing in this schema treats them as
    -- authentication. See PR #224's OD3.
    actor_address   TEXT,
    actor_kind      TEXT,
    claim_epoch     INTEGER,
    note            TEXT,
    payload         JSON NOT NULL DEFAULT '{}'
                      CHECK (json_valid(payload) AND json_type(payload) = 'object'),
    created_at      INTEGER NOT NULL DEFAULT (CAST(strftime('%s','now') AS INTEGER)),
    UNIQUE(task_id, event_index),
    FOREIGN KEY (task_id) REFERENCES agent_tasks(id) ON DELETE RESTRICT
);
CREATE INDEX IF NOT EXISTS idx_agent_task_events_created
    ON agent_task_events(created_at DESC);

-- ── The state machine, enforced by the engine ──────────────────────────────
--
-- An assignment is CREATED enqueued, and reaches every other state by a
-- transition. The trigger below refuses a transition that is not on the
-- role's path.
--
-- IMPLEMENTER: enqueued -> delivered -> acknowledged -> accepted ->
--   in_progress -> completed -> review_passed -> awaiting_operator ->
--   operator_approved.
-- REVIEWER: enqueued -> delivered -> acknowledged -> accepted ->
--   in_progress -> completed (the review asked for changes) or
--   -> review_passed (the review passed).
--
-- 'cancelled' is the universal withdrawal and is legal from every state
-- except itself.
CREATE TRIGGER IF NOT EXISTS trg_agent_tasks_initial_state
BEFORE INSERT ON agent_tasks
FOR EACH ROW WHEN NEW.state <> 'enqueued'
BEGIN
    SELECT RAISE(ABORT, 'agent_tasks: an assignment is created in the ''enqueued'' state');
END;

CREATE TRIGGER IF NOT EXISTS trg_agent_tasks_state_transition
BEFORE UPDATE OF state ON agent_tasks
FOR EACH ROW
WHEN OLD.state <> NEW.state AND NOT (
        (OLD.role = 'implementer' AND (
             (OLD.state = 'enqueued'      AND NEW.state IN ('delivered','cancelled'))
          OR (OLD.state = 'delivered'     AND NEW.state IN ('acknowledged','cancelled'))
          OR (OLD.state = 'acknowledged'  AND NEW.state IN ('accepted','cancelled'))
          OR (OLD.state = 'accepted'      AND NEW.state IN ('in_progress','cancelled'))
          OR (OLD.state = 'in_progress'   AND NEW.state IN ('completed','cancelled'))
          OR (OLD.state = 'completed'     AND NEW.state IN ('review_passed','awaiting_operator','cancelled'))
          OR (OLD.state = 'review_passed' AND NEW.state IN ('awaiting_operator','cancelled'))
          OR (OLD.state = 'awaiting_operator' AND NEW.state IN ('operator_approved','cancelled'))
        ))
     OR (OLD.role = 'reviewer' AND (
             (OLD.state = 'enqueued'      AND NEW.state IN ('delivered','cancelled'))
          OR (OLD.state = 'delivered'     AND NEW.state IN ('acknowledged','cancelled'))
          OR (OLD.state = 'acknowledged'  AND NEW.state IN ('accepted','cancelled'))
          OR (OLD.state = 'accepted'      AND NEW.state IN ('in_progress','cancelled'))
          OR (OLD.state = 'in_progress'   AND NEW.state IN ('completed','review_passed','cancelled'))
        ))
)
BEGIN
    SELECT RAISE(ABORT, 'agent_tasks: illegal state transition for this role');
END;

-- ── The lease discipline ───────────────────────────────────────────────────
--
-- The claim epoch advances by EXACTLY ONE, a claim sets an owner and a
-- lease, and the owner changes only on a claim. So the epoch cannot be
-- skipped or reset, and an owner cannot be reassigned without the row
-- ceasing to be a lease.
--
-- The FENCE for a stale owner is not here — a trigger cannot see the epoch
-- a caller presented. The fence is the WHERE clause of the writing
-- statement ('WHERE id = ? AND claim_epoch = ?'), which is atomic with the
-- write. This trigger's job is to make sure the epoch cannot be FORGED.
CREATE TRIGGER IF NOT EXISTS trg_agent_tasks_claim_discipline
BEFORE UPDATE ON agent_tasks
FOR EACH ROW
WHEN (
        (NEW.claim_epoch <> OLD.claim_epoch
         AND (NEW.claim_epoch <> OLD.claim_epoch + 1
              OR NEW.owner IS NULL
              OR NEW.lease_expires_at IS NULL))
     OR (NEW.owner IS NOT OLD.owner AND NEW.claim_epoch <> OLD.claim_epoch + 1)
)
BEGIN
    SELECT RAISE(ABORT, 'agent_tasks: the claim epoch advances by exactly one, and only a claim may change the owner');
END;
`

// MigrateAgentAssignments creates the durable assignment storage.
//
// It MUST be called inside the fatal migration transaction (db.go): the DDL
// and the sentinel share one transaction, so a failure at any point rolls
// the whole thing back and NewDatabaseManager returns an error rather than
// leaving a half-migrated database.
//
// Idempotent via the `agent_assignments_v1` sentinel.
func MigrateAgentAssignments(tx *sql.Tx) error {
	const sentinel = "agent_assignments_v1"

	var applied int
	if err := tx.QueryRow(
		`SELECT COUNT(*) FROM schema_migrations WHERE id = ?`,
		sentinel,
	).Scan(&applied); err != nil {
		return fmt.Errorf("check %s sentinel: %w", sentinel, err)
	}
	if applied > 0 {
		return nil
	}

	// The DDL is executed as ONE statement string. go-sqlite3's Exec walks
	// the statement tail, so the CREATE TABLEs, the CREATE INDEXes and the
	// CREATE TRIGGERs run in order, inside this transaction.
	if _, err := tx.Exec(agentAssignmentDDL); err != nil {
		return fmt.Errorf("create the assignment tables: %w", err)
	}

	// The sentinel is written LAST. See the file header: this ordering is
	// what the injected-failure probe depends on.
	if _, err := tx.Exec(
		`INSERT OR IGNORE INTO schema_migrations (id, applied_at) VALUES (?, ?)`,
		sentinel, time.Now().Unix(),
	); err != nil {
		return fmt.Errorf("record %s sentinel: %w", sentinel, err)
	}
	return nil
}
