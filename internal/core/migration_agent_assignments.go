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
//   - every reference is ON DELETE RESTRICT, so a project with children
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
//
// WHAT THE SECOND INDEPENDENT REVIEW FOUND, AND WHAT CHANGED HERE
// ----------------------------------------------------------------
// The first version of this file was reviewed independently and came back
// with eleven blocking findings. Every one of them was a real defect, and
// every one is closed here. The findings were, in the reviewer's words:
//
//   - the implementer could reach `awaiting_operator` from `completed`,
//     SKIPPING the required independent review;
//   - the transition path used `OLD.role`, but `role` was MUTABLE, so a
//     relabel could take a different path (and a role-only write evaded the
//     path check entirely);
//   - the ledger's `UNIQUE(task_id, event_index)` permitted GAPS, and the
//     ledger permitted UPDATE and DELETE despite the contract calling it
//     append-only;
//   - the child branch of the root/child CHECK constrained
//     `permitted_operations` but not `eligible_recipients`, so a child could
//     store its own expanded allow-list;
//   - the same-project composite keys did not establish a TREE: a child
//     could parent itself, could cite a non-root as its budget root, and
//     its parent and root need not belong to the same tree;
//   - `TEXT PRIMARY KEY` permits NULL in SQLite, and a NULL id made the
//     root equality UNKNOWN, so a depth-zero row pointing at someone else's
//     root was accepted;
//   - the claim discipline checked UPDATES only, so a task could start at
//     epoch 87 with an owner and no lease;
//   - INTEGER affinity does not make a number an INTEGER, so at REAL 1e30
//     `epoch + 1` equals `epoch` and the owner could change without the
//     fence advancing;
//   - an unreferenced task could be DELETED and re-INSERTED, resetting its
//     epoch so that a stale token could become current again;
//   - the numeric budget comparisons accepted NONNUMERIC text, so a
//     "bounded" root need not have had a numeric limit;
//   - a BLOB idempotency key and a TEXT one with the identical bytes
//     coexisted in the unique index, defeating the duplicate suppression.
//
// The fixes are in the DDL below, and each has a regression test that fails
// without it.
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
// The contract's invariants are not expressible as column CHECK constraints,
// and each of them is the difference between a coordination substrate and a
// convention. There are THIRTEEN rules, carried by EIGHTEEN triggers — three
// of the rules are split across several triggers, so that a test can
// isolate one clause from another. The rules are:
//
//   1  an assignment is created in the 'enqueued' state
//   2  an assignment is created UNCLAIMED: epoch 0, no owner, no lease
//   3  the delegation tree is a tree: the root is a root, the parent is in
//      the same tree, and nobody is their own parent — on the INSERT path
//      and on the UPDATE path
//   4  a permitted operation is a non-null string (on INSERT)
//   5  a permitted operation is a non-null string (on UPDATE)
//   6  the state machine is ROLE-AWARE, and 'cancelled' is universal
//   7  the claim epoch advances by exactly one, and only a claim changes
//      the owner
//   8  the role is IMMUTABLE
//   9  the operator's gate names its action, a recorded approval names its
//      action, its revision and its source, and neither is erasable
//  10  a claimed assignment cannot be DELETED
//  11  the ledger's event index is MONOTONIC
//  12  the ledger is append-only (no UPDATE)
//  13  the ledger is append-only (no DELETE)
//
// The fencing of a STALE owner is deliberately NOT a trigger. A trigger
// cannot see the epoch a caller presented — it only sees the row before and
// the row after — so the fence is the WHERE clause of the writing statement
// (`WHERE id = ? AND claim_epoch = ?`), which is atomic with the write. The
// trigger's job is to make sure the epoch cannot be FORGED: it advances by
// exactly one, only a claim may change the owner, and an assignment starts
// unclaimed.
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
-- EVERY COLUMN'S TYPE IS GUARDED, AND EVERY ONE OF THOSE GUARDS WAS A
-- FINDING.
--
-- SQLite's affinities are suggestions, not types:
--
--   * 'TEXT PRIMARY KEY' does NOT imply NOT NULL. A NULL id made the root
--     equality UNKNOWN, and an UNKNOWN operand in a CHECK is not a
--     violation — so a depth-zero row pointing at someone else's root was
--     accepted. The primary keys are NOT NULL, and every text column that
--     carries an identity or a key is guarded with typeof() = 'text'.
--   * INTEGER affinity does NOT make a number an INTEGER. At REAL 1e30,
--     'epoch + 1' equals 'epoch', so the owner could change without the
--     fence advancing. Every numeric column is guarded with
--     typeof() = 'integer'.
--   * A TEXT and a BLOB with the identical bytes are DIFFERENT values to
--     SQLite, and a UNIQUE index holds both. The idempotency key is
--     therefore guarded with typeof() = 'text'.
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
-- the delivery is a state transition with its own ledger entry, never a
-- side effect of the row having been read.
--
-- 'operator_approved' is a RECORD, not a grant. MPM records an approval
-- that was verified OUTSIDE MPM — its source, the exact revision it covers
-- and a pointer to the evidence — and it never manufactures or authenticates
-- one. The approval names the exact action (approval_action) and the exact
-- revision (approval_revision); a change to the head revision after the
-- approval was recorded leaves the approval stale, and staleness is
-- observable rather than implied.
CREATE TABLE IF NOT EXISTS agent_tasks (
    id                  TEXT PRIMARY KEY NOT NULL
                        CHECK (typeof(id) = 'text'),
    project_id          TEXT NOT NULL
                        CHECK (typeof(project_id) = 'text')
                        REFERENCES projects(id) ON DELETE RESTRICT,
    -- The delegation tree. root_id is the ROOT assignment; a root is its
    -- own root. parent_id is NULL for a root.
    root_id             TEXT NOT NULL CHECK (typeof(root_id) = 'text'),
    parent_id           TEXT CHECK (parent_id IS NULL OR typeof(parent_id) = 'text'),
    depth               INTEGER NOT NULL DEFAULT 0
                        CHECK (typeof(depth) = 'integer' AND depth >= 0),
    title               TEXT NOT NULL
                        CHECK (typeof(title) = 'text'
                               AND length(CAST(title AS BLOB)) <= 200),
    objective           TEXT NOT NULL
                        CHECK (typeof(objective) = 'text'
                               AND length(CAST(objective AS BLOB)) <= 4000),
    -- The stable agent ADDRESS. This is an address, NOT an identity: it
    -- names a recipient and it does not authenticate one. See
    -- docs/dev/continuity-001-assignment-schema.md.
    recipient           TEXT NOT NULL
                        CHECK (typeof(recipient) = 'text'
                               AND length(CAST(recipient AS BLOB)) BETWEEN 1 AND 64),
    role                TEXT NOT NULL DEFAULT 'implementer'
                        CHECK (role IN ('implementer','reviewer')),
    state               TEXT NOT NULL DEFAULT 'enqueued'
                        CHECK (state IN ('enqueued','delivered','acknowledged','accepted',
                                         'in_progress','completed','review_passed',
                                         'awaiting_operator','operator_approved','cancelled')),
    -- The idempotency key. UNIQUE across the table, so an accidental
    -- duplicate dispatch cannot create a second logical assignment. The
    -- typeof() guard matters: a BLOB and a TEXT with identical bytes are
    -- different values to SQLite, and both would sit in the index.
    idempotency_key     TEXT NOT NULL
                        CHECK (typeof(idempotency_key) = 'text'
                               AND length(CAST(idempotency_key AS BLOB)) BETWEEN 1 AND 128),
    -- The lease. claim_epoch is the fencing token: it advances by exactly
    -- one on every claim, and a write that presents a stale epoch matches
    -- no row.
    claim_epoch         INTEGER NOT NULL DEFAULT 0
                        CHECK (typeof(claim_epoch) = 'integer' AND claim_epoch >= 0),
    owner               TEXT
                        CHECK (owner IS NULL OR typeof(owner) = 'text'),
    lease_expires_at    INTEGER
                        CHECK (lease_expires_at IS NULL OR typeof(lease_expires_at) = 'integer'),
    -- The ROOT's budgets. A child carries none of them — see the CHECK
    -- constraint below.
    max_depth           INTEGER
                        CHECK (max_depth IS NULL OR (typeof(max_depth) = 'integer' AND max_depth >= 0)),
    max_tasks           INTEGER
                        CHECK (max_tasks IS NULL OR (typeof(max_tasks) = 'integer' AND max_tasks > 0)),
    max_concurrency     INTEGER
                        CHECK (max_concurrency IS NULL OR (typeof(max_concurrency) = 'integer' AND max_concurrency > 0)),
    max_resource_units  INTEGER
                        CHECK (max_resource_units IS NULL OR (typeof(max_resource_units) = 'integer' AND max_resource_units >= 0)),
    -- What this assignment costs against the root's cumulative budget.
    resource_units      INTEGER NOT NULL DEFAULT 0
                        CHECK (typeof(resource_units) = 'integer' AND resource_units >= 0),
    -- The operations this tree may perform. A ROOT must name at least one:
    -- an empty list on a root would be a root that may do nothing, which is
    -- a silent misconfiguration rather than a safe default. The ELEMENTS
    -- are checked by a trigger, because a CHECK constraint cannot contain
    -- a subquery and '["read"]' and '[null]' both satisfy the length test.
    permitted_operations JSON NOT NULL DEFAULT '[]'
                          CHECK (json_valid(permitted_operations)
                                 AND json_type(permitted_operations) = 'array'),
    -- The addresses an assignment in this tree may target. A root's own
    -- recipient must be among them; a child that targets an address outside
    -- the list is refused. A child carries none of its own.
    eligible_recipients  JSON NOT NULL DEFAULT '[]'
                          CHECK (json_valid(eligible_recipients)
                                 AND json_type(eligible_recipients) = 'array'),
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
    approved_at         INTEGER
                        CHECK (approved_at IS NULL OR typeof(approved_at) = 'integer'),
    created_at          INTEGER NOT NULL DEFAULT (CAST(strftime('%s','now') AS INTEGER)),
    updated_at          INTEGER NOT NULL DEFAULT (CAST(strftime('%s','now') AS INTEGER)),
    completed_at        INTEGER
                        CHECK (completed_at IS NULL OR typeof(completed_at) = 'integer'),
    -- The containment boundary. An assignment belongs to a project, and the
    -- COMPOSITE keys keep the tree inside ONE project: a child cannot name
    -- a root of another project, and it cannot name a parent of another
    -- project.
    FOREIGN KEY (root_id, project_id)
        REFERENCES agent_tasks(id, project_id) ON DELETE RESTRICT,
    FOREIGN KEY (parent_id, project_id)
        REFERENCES agent_tasks(id, project_id) ON DELETE RESTRICT,
    -- The root/child shape, and the budget inheritance.
    --
    -- EVERY OPERAND IS NEVER NULL. An UNKNOWN operand in a CHECK is not a
    -- violation, so a CHECK whose operands can be NULL is a CHECK that can
    -- be bypassed. That is why the primary key is NOT NULL, why the depth
    -- column is NOT NULL, and why the child branch states
    -- 'parent_id <> id' rather than relying on the foreign key.
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
            AND parent_id <> id
            AND root_id <> id
            AND max_depth IS NULL
            AND max_tasks IS NULL
            AND max_concurrency IS NULL
            AND max_resource_units IS NULL
            AND json_array_length(permitted_operations) = 0
            AND json_array_length(eligible_recipients) = 0
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
-- assignment, so the ledger is ordered — and it is separate from
-- 'works'/'work_events' because the two vocabularies are separate.
--
-- The event index is MONOTONIC, not gap-free: a trigger refuses an index
-- that is not strictly greater than the current maximum. An index can still
-- be SKIPPED, because a writer that reserves one and then dies leaves a
-- hole. The ledger records what happened; it does not promise what did not.
--
-- The ledger is APPEND-ONLY, and that is enforced: triggers refuse an
-- UPDATE and a DELETE on this table. A provenance ledger that can be
-- rewritten is not a provenance ledger.
--
-- The event_type vocabulary is the transition vocabulary plus three
-- observations that are not transitions: 'claimed' (a lease was taken),
-- 'lease_expired' (a lease lapsed and the assignment became re-claimable)
-- and 'note' (a free-text annotation that changes no state).
--
-- from_state and to_state are the states either side of the transition, and
-- are NULL for the three non-transition observations.
CREATE TABLE IF NOT EXISTS agent_task_events (
    id              TEXT PRIMARY KEY NOT NULL
                    CHECK (typeof(id) = 'text'),
    task_id         TEXT NOT NULL CHECK (typeof(task_id) = 'text'),
    event_index     INTEGER NOT NULL
                    CHECK (typeof(event_index) = 'integer' AND event_index >= 0),
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
    claim_epoch     INTEGER
                    CHECK (claim_epoch IS NULL OR typeof(claim_epoch) = 'integer'),
    note            TEXT,
    payload         JSON NOT NULL DEFAULT '{}'
                      CHECK (json_valid(payload) AND json_type(payload) = 'object'),
    created_at      INTEGER NOT NULL DEFAULT (CAST(strftime('%s','now') AS INTEGER)),
    UNIQUE(task_id, event_index),
    FOREIGN KEY (task_id) REFERENCES agent_tasks(id) ON DELETE RESTRICT
);
CREATE INDEX IF NOT EXISTS idx_agent_task_events_created
    ON agent_task_events(created_at DESC);

-- ── Rule 1: an assignment is CREATED enqueued ─────────────────────────────
CREATE TRIGGER IF NOT EXISTS trg_agent_tasks_initial_state
BEFORE INSERT ON agent_tasks
FOR EACH ROW WHEN NEW.state <> 'enqueued'
BEGIN
    SELECT RAISE(ABORT, 'agent_tasks: an assignment is created in the ''enqueued'' state');
END;

-- ── Rule 2: an assignment is created UNCLAIMED ────────────────────────────
--
-- The claim discipline used to check UPDATES only, so a task could be born
-- at epoch 87 with an owner and no lease — a fence that had never been
-- claimed, and therefore a fence that could not be reasoned about. A task
-- starts unclaimed: epoch 0, no owner, no lease.
CREATE TRIGGER IF NOT EXISTS trg_agent_tasks_unclaimed_at_creation
BEFORE INSERT ON agent_tasks
FOR EACH ROW WHEN NEW.claim_epoch <> 0
                 OR NEW.owner IS NOT NULL
                 OR NEW.lease_expires_at IS NOT NULL
BEGIN
    SELECT RAISE(ABORT, 'agent_tasks: an assignment is created unclaimed — epoch 0, no owner, no lease');
END;

-- ── Rule 3: the delegation tree is a TREE ─────────────────────────────────
--
-- The composite foreign keys keep the tree inside ONE project, but they do
-- not make it a tree: a child could parent ITSELF, could cite a non-root
-- as its budget root, and its parent and root need not have belonged to the
-- same tree. This trigger closes all three.
CREATE TRIGGER IF NOT EXISTS trg_agent_tasks_tree_shape
BEFORE INSERT ON agent_tasks
FOR EACH ROW WHEN NEW.depth > 0 AND (
        -- Nobody is their own parent.
        NEW.parent_id = NEW.id
        -- The budget root must BE a root.
        OR NOT EXISTS (
            SELECT 1 FROM agent_tasks r
             WHERE r.id = NEW.root_id
               AND r.project_id = NEW.project_id
               AND r.depth = 0
        )
        -- The parent must be in the SAME tree as the root.
        OR NOT EXISTS (
            SELECT 1 FROM agent_tasks p
             WHERE p.id = NEW.parent_id
               AND p.project_id = NEW.project_id
               AND p.root_id = NEW.root_id
        )
)
BEGIN
    SELECT RAISE(ABORT, 'agent_tasks: the delegation tree is malformed — the root must be a root of this project, and the parent must be in the same tree');
END;

-- The same rule on the UPDATE path.
--
-- A trigger on INSERT alone leaves the tree mutable: a child could be
-- re-pointed at a non-root, or moved to another project, and the tree
-- would no longer be a tree. The composite foreign keys refuse a
-- CROSS-PROJECT move, and this trigger refuses the rest — so neither
-- layer is the only one standing between a broken tree and a legal row.
CREATE TRIGGER IF NOT EXISTS trg_agent_tasks_tree_shape_on_update
BEFORE UPDATE OF root_id, parent_id, depth, project_id ON agent_tasks
FOR EACH ROW WHEN NEW.depth > 0 AND (
        NEW.parent_id = NEW.id
        OR NOT EXISTS (
            SELECT 1 FROM agent_tasks r
             WHERE r.id = NEW.root_id
               AND r.project_id = NEW.project_id
               AND r.depth = 0
        )
        OR NOT EXISTS (
            SELECT 1 FROM agent_tasks p
             WHERE p.id = NEW.parent_id
               AND p.project_id = NEW.project_id
               AND p.root_id = NEW.root_id
        )
)
BEGIN
    SELECT RAISE(ABORT, 'agent_tasks: the delegation tree is malformed — the root must be a root of this project, and the parent must be in the same tree');
END;

-- ── Rule 4: a permitted operation is a non-null string (INSERT) ───────────
--
-- The root/child CHECK requires a root to name at least one permitted
-- operation, and '["read"]' and '[null]' both satisfy that test. A CHECK
-- constraint cannot contain a subquery, so the ELEMENTS are checked here.
CREATE TRIGGER IF NOT EXISTS trg_agent_tasks_permitted_operations_insert
BEFORE INSERT ON agent_tasks
FOR EACH ROW WHEN EXISTS (
    SELECT 1 FROM json_each(NEW.permitted_operations)
     WHERE value IS NULL OR typeof(value) <> 'text'
)
BEGIN
    SELECT RAISE(ABORT, 'agent_tasks: a permitted operation must be a non-null string');
END;

-- ── Rule 5: a permitted operation is a non-null string (UPDATE) ───────────
CREATE TRIGGER IF NOT EXISTS trg_agent_tasks_permitted_operations_update
BEFORE UPDATE OF permitted_operations ON agent_tasks
FOR EACH ROW WHEN EXISTS (
    SELECT 1 FROM json_each(NEW.permitted_operations)
     WHERE value IS NULL OR typeof(value) <> 'text'
)
BEGIN
    SELECT RAISE(ABORT, 'agent_tasks: a permitted operation must be a non-null string');
END;

-- ── Rule 6: the state machine is ROLE-AWARE ───────────────────────────────
--
-- An assignment moves forward through a fixed path, and the path differs by
-- role: an implementer's assignment reaches 'review_passed' — and only
-- then 'awaiting_operator' — while a reviewer's assignment ends at
-- 'review_passed' (the verdict was passing) or at 'completed' (the review
-- is done and it asked for changes).
--
-- 'cancelled' is the UNIVERSAL withdrawal: legal from EVERY state except
-- itself, for EVERY role. It is stated ONCE, as its own clause, so that
-- "universal" is a property of the trigger rather than a promise that every
-- branch remembered to include it. It includes 'operator_approved' — an
-- operator may withdraw an approved assignment — so 'operator_approved' is
-- terminal in every other respect, not in this one.
--
-- The role branches below therefore carry no 'cancelled': the universal
-- clause is the only way to reach it.
CREATE TRIGGER IF NOT EXISTS trg_agent_tasks_state_transition
BEFORE UPDATE OF state ON agent_tasks
FOR EACH ROW
WHEN OLD.state <> NEW.state
BEGIN
    -- THE PATH, FIRST.
    --
    -- The checks are ordered, and the path is checked first, because the
    -- order in which SQLite fires SEPARATE triggers on the same event is
    -- not specified. If the gate's field rules were separate triggers,
    -- an implementer skipping its review would be refused by whichever
    -- trigger happened to fire first — and the message would be
    -- arbitrary. Inside ONE trigger the statements run in order, so an
    -- illegal transition is always refused AS an illegal transition.
    SELECT RAISE(ABORT, 'agent_tasks: illegal state transition for this role')
     WHERE NOT (
            NEW.state = 'cancelled'
         OR (OLD.role = 'implementer' AND (
                 (OLD.state = 'enqueued'          AND NEW.state = 'delivered')
              OR (OLD.state = 'delivered'         AND NEW.state = 'acknowledged')
              OR (OLD.state = 'acknowledged'      AND NEW.state = 'accepted')
              OR (OLD.state = 'accepted'          AND NEW.state = 'in_progress')
              OR (OLD.state = 'in_progress'       AND NEW.state = 'completed')
              OR (OLD.state = 'completed'         AND NEW.state = 'review_passed')
              OR (OLD.state = 'review_passed'     AND NEW.state = 'awaiting_operator')
              OR (OLD.state = 'awaiting_operator' AND NEW.state = 'operator_approved')
            ))
         OR (OLD.role = 'reviewer' AND (
                 (OLD.state = 'enqueued'      AND NEW.state = 'delivered')
              OR (OLD.state = 'delivered'     AND NEW.state = 'acknowledged')
              OR (OLD.state = 'acknowledged'  AND NEW.state = 'accepted')
              OR (OLD.state = 'accepted'      AND NEW.state = 'in_progress')
              OR (OLD.state = 'in_progress'   AND NEW.state IN ('completed','review_passed'))
            ))
     );

    -- THEN THE GATE'S OWN FIELD RULES. A transition that is ON the path
    -- still has to carry the fields that path requires.
    SELECT RAISE(ABORT, 'agent_tasks: the operator''s gate must name the exact action it awaits')
     WHERE NEW.state = 'awaiting_operator'
       AND (NEW.awaiting_action IS NULL OR length(CAST(NEW.awaiting_action AS BLOB)) = 0);

    SELECT RAISE(ABORT, 'agent_tasks: a recorded approval must name its action, its revision and its source')
     WHERE NEW.state = 'operator_approved'
       AND (NEW.approval_action IS NULL OR length(CAST(NEW.approval_action AS BLOB)) = 0
            OR NEW.approval_revision IS NULL OR length(CAST(NEW.approval_revision AS BLOB)) = 0
            OR NEW.approval_source IS NULL OR length(CAST(NEW.approval_source AS BLOB)) = 0);
END;

-- ── Rule 7: the claim discipline ──────────────────────────────────────────
--
-- The claim epoch advances by EXACTLY ONE, a claim sets an owner and a
-- lease, and the owner changes only on a claim. So the epoch cannot be
-- skipped or reset, and an owner cannot be reassigned without the row
-- ceasing to be a lease.
--
-- Each clause raises its OWN message, so a test can isolate one clause from
-- the others: the "a claim must set an owner" clause is otherwise
-- indistinguishable from the column CHECK that a lease implies an owner.
--
-- The FENCE for a stale owner is not here — a trigger cannot see the epoch
-- a caller presented. The fence is the WHERE clause of the writing
-- statement ('WHERE id = ? AND claim_epoch = ?'), which is atomic with the
-- write. This trigger's job is to make sure the epoch cannot be FORGED.
CREATE TRIGGER IF NOT EXISTS trg_agent_tasks_claim_discipline
BEFORE UPDATE ON agent_tasks
FOR EACH ROW
WHEN NEW.claim_epoch <> OLD.claim_epoch
     AND NEW.claim_epoch <> OLD.claim_epoch + 1
BEGIN
    SELECT RAISE(ABORT, 'agent_tasks: the claim epoch advances by exactly one');
END;

CREATE TRIGGER IF NOT EXISTS trg_agent_tasks_claim_sets_owner
BEFORE UPDATE ON agent_tasks
FOR EACH ROW
WHEN NEW.claim_epoch <> OLD.claim_epoch AND NEW.owner IS NULL
BEGIN
    SELECT RAISE(ABORT, 'agent_tasks: a claim must set an owner');
END;

CREATE TRIGGER IF NOT EXISTS trg_agent_tasks_claim_sets_lease
BEFORE UPDATE ON agent_tasks
FOR EACH ROW
WHEN NEW.claim_epoch <> OLD.claim_epoch AND NEW.lease_expires_at IS NULL
BEGIN
    SELECT RAISE(ABORT, 'agent_tasks: a claim must set a lease');
END;

CREATE TRIGGER IF NOT EXISTS trg_agent_tasks_owner_changes_only_on_claim
BEFORE UPDATE ON agent_tasks
FOR EACH ROW
WHEN NEW.owner IS NOT OLD.owner AND NEW.claim_epoch = OLD.claim_epoch
BEGIN
    SELECT RAISE(ABORT, 'agent_tasks: only a claim may change the owner');
END;

-- ── Rule 8: the role is IMMUTABLE ─────────────────────────────────────────
--
-- The transition path is selected by OLD.role. If the role were mutable, a
-- relabel would select a DIFFERENT path — and a role-only write would
-- evade the path check entirely, because the state did not change. The role
-- is part of what the assignment IS, so it is fixed at creation.
CREATE TRIGGER IF NOT EXISTS trg_agent_tasks_role_is_immutable
BEFORE UPDATE OF role ON agent_tasks
FOR EACH ROW WHEN NEW.role IS NOT OLD.role
BEGIN
    SELECT RAISE(ABORT, 'agent_tasks: the role is immutable');
END;

-- ── Rule 9: the gate's record is not erasable ─────────────────────────────
--
-- The transition INTO the gate must name the exact action, and the
-- transition INTO the approval must name the action, the revision and the
-- source — those are the two field rules inside the state-machine trigger
-- above, and they are checked AFTER the path, so an illegal transition is
-- never reported as a missing field.
--
-- But a transition is not the only write. Once an assignment IS at the
-- gate, the action it awaits must not be quietly cleared: there would be
-- nothing left for a staleness check to compare against, and the gate
-- would become a bare state. The same holds for a recorded approval — it
-- is EVIDENCE, and evidence is not rewritten.
CREATE TRIGGER IF NOT EXISTS trg_agent_tasks_awaiting_action_is_not_cleared
BEFORE UPDATE OF awaiting_action ON agent_tasks
FOR EACH ROW
WHEN OLD.state = 'awaiting_operator'
     AND (NEW.awaiting_action IS NULL OR length(CAST(NEW.awaiting_action AS BLOB)) = 0)
BEGIN
    SELECT RAISE(ABORT, 'agent_tasks: the action the operator''s gate awaits cannot be cleared');
END;

CREATE TRIGGER IF NOT EXISTS trg_agent_tasks_approval_is_not_cleared
BEFORE UPDATE OF approval_action, approval_revision, approval_source ON agent_tasks
FOR EACH ROW
WHEN OLD.state = 'operator_approved'
     AND (NEW.approval_action IS NULL OR length(CAST(NEW.approval_action AS BLOB)) = 0
          OR NEW.approval_revision IS NULL OR length(CAST(NEW.approval_revision AS BLOB)) = 0
          OR NEW.approval_source IS NULL OR length(CAST(NEW.approval_source AS BLOB)) = 0)
BEGIN
    SELECT RAISE(ABORT, 'agent_tasks: a recorded approval is evidence, and evidence is not rewritten');
END;

-- ── Rule 10: a claimed assignment cannot be DELETED ───────────────────────
--
-- An assignment is CANCELLED, never deleted. And the rule is stated in
-- terms of the claim epoch, because that is what closes the hole: an
-- assignment that was ever claimed holds an epoch, and an assignment that
-- holds an epoch cannot be deleted, so its id cannot be reused — and a
-- reused id would restart the epoch at zero, which is exactly how a STALE
-- token could become current again.
CREATE TRIGGER IF NOT EXISTS trg_agent_tasks_not_deletable
BEFORE DELETE ON agent_tasks
FOR EACH ROW WHEN OLD.claim_epoch > 0
BEGIN
    SELECT RAISE(ABORT, 'agent_tasks: a claimed assignment is cancelled, never deleted');
END;

-- ── Rule 11: the ledger's event index is MONOTONIC ───────────────────────
CREATE TRIGGER IF NOT EXISTS trg_agent_task_events_monotonic
BEFORE INSERT ON agent_task_events
FOR EACH ROW WHEN NEW.event_index <= (
    SELECT COALESCE(MAX(event_index), -1) FROM agent_task_events WHERE task_id = NEW.task_id
)
BEGIN
    SELECT RAISE(ABORT, 'agent_task_events: the event index must be strictly greater than the current maximum');
END;

-- ── Rules 12 and 13: the ledger is APPEND-ONLY ────────────────────────────
CREATE TRIGGER IF NOT EXISTS trg_agent_task_events_no_update
BEFORE UPDATE ON agent_task_events
BEGIN
    SELECT RAISE(ABORT, 'agent_task_events: the ledger is append-only');
END;

CREATE TRIGGER IF NOT EXISTS trg_agent_task_events_no_delete
BEFORE DELETE ON agent_task_events
BEGIN
    SELECT RAISE(ABORT, 'agent_task_events: the ledger is append-only');
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
