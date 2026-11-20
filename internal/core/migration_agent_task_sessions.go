// migration_agent_task_sessions.go — Continuity 001, PR 3: the durable
// mapping between an MPM assignment and a NATIVE OpenCode session.
//
// WHAT THIS IS
// ------------
// PR 1 shipped the assignment storage. PR 2 shipped the coordination
// service. Neither of them can say WHICH OpenCode session is executing an
// assignment, because an assignment is a durable row and a session is a
// runtime object with an id the runtime mints.
//
// This file adds exactly that mapping — one additive, local-only table —
// and nothing else. It does not alter PR 1's hash-pinned DDL, it does not
// repurpose the approval or provenance columns as session storage, and it
// does not widen the closed `agent_task_events` vocabulary.
//
// WHY A TABLE AND NOT A PAYLOAD FIELD
// -----------------------------------
// `agent_tasks.payload` is written at creation and is not in any
// transition's settable-column whitelist, so it cannot carry a fact that
// becomes known later. `evidence_ref` is a pointer the operator supplies
// about a revision, not a runtime handle. Neither can be a session id
// without turning a provenance field into a cache, which is what the
// Projection Test forbids.
//
// WHY THE ID IS MINTED BEFORE THE LAUNCH
// --------------------------------------
// OpenCode v2.3.2's `session.create` is NOT idempotent: every call mints a
// new session. But `opencode run --session <id>` creates the session with
// the CALLER's id, and continues it if it already exists. So the adapter
// mints the id, writes THIS row first, and only then launches. An
// interruption between the two is recoverable by the id in this row —
// which is the entire reason the row exists.
//
// The row therefore REQUIRES `native_session_id`. A launch intent without
// the id it intends to use would not be recoverable, so the column is NOT
// NULL and the service refuses to create a row without one.
//
// WHAT THE TRIGGERS ENFORCE
// -------------------------
//   - an attempt belongs to a claim that is CURRENT when the attempt
//     starts — you cannot open an attempt against an epoch the assignment
//     does not hold, so a stale worker cannot even record its intent;
//   - `native_session_id` and `claim_epoch` are IMMUTABLE after insert —
//     the id is what recovery reconciles on, and rewriting it would make
//     the recovery record lie;
//   - `launch_state` only moves FORWARD along its own graph, so a
//     completed attempt cannot be reopened as `intent`;
//   - an attempt number is unique per task and strictly increasing, so a
//     retry is visibly a retry;
//   - a native session id maps to at most ONE attempt, so two assignments
//     cannot claim the same runtime object.
//
// The row is APPEND-ONLY in the sense that matters: the identity is
// write-once, and the state moves forward. It is not deletable while the
// assignment exists, because it is the recovery record.
//
// Idempotent via the `agent_task_sessions_v1` sentinel, written LAST inside
// the same transaction as the DDL.
package internal

import (
	"database/sql"
	"fmt"
	"time"
)

// agentTaskSessionDDL is the approved DDL for the assignment-to-session
// mapping.
//
// The SHA-256 of this text is pinned by
// TestAgentTaskSessionDDLIsPinned, so the storage cannot drift from
// docs/dev/continuity-001-opencode-adapter.md without failing the build.
const agentTaskSessionDDL = `-- ── agent_task_sessions ───────────────────────────────────────────────────
-- One row per ATTEMPT to execute one assignment in one native runtime
-- session. The row is written BEFORE the runtime is launched, and it
-- carries the session id the launch will use, so an interruption between
-- the write and the launch is recoverable rather than ambiguous.
CREATE TABLE IF NOT EXISTS agent_task_sessions (
    id                TEXT PRIMARY KEY NOT NULL
                      CHECK (typeof(id) = 'text'),
    -- The assignment this attempt executes. ON DELETE RESTRICT: an
    -- assignment that has been executed cannot be deleted out from under
    -- its execution record.
    task_id           TEXT NOT NULL
                      CHECK (typeof(task_id) = 'text')
                      REFERENCES agent_tasks(id) ON DELETE RESTRICT,
    -- Which attempt this is for that task. Strictly increasing, and
    -- unique per task (index below), so a retry is visibly a retry and
    -- two concurrent launches cannot both be "the" attempt.
    attempt           INTEGER NOT NULL
                      CHECK (typeof(attempt) = 'integer' AND attempt >= 1),
    -- The claim epoch the attempt was opened under. IMMUTABLE (trigger
    -- below): this is what fences the attempt's result writes, so a
    -- rewrite would make a stale worker's write match the current row.
    claim_epoch       INTEGER NOT NULL
                      CHECK (typeof(claim_epoch) = 'integer' AND claim_epoch >= 1),
    -- The runtime this attempt uses. Closed vocabulary: an unknown
    -- runtime is a row nothing can reconcile.
    runtime           TEXT NOT NULL
                      CHECK (runtime IN ('opencode')),
    -- The agent definition the attempt intended to use. The adapter
    -- verifies it against an allow-list BEFORE the row is written, so
    -- this column records a decision already made rather than a wish.
    intended_agent    TEXT NOT NULL
                      CHECK (typeof(intended_agent) = 'text'
                             AND length(CAST(intended_agent AS BLOB)) BETWEEN 1 AND 64),
    -- THE CALLER-MINTED NATIVE SESSION ID. Present from the FIRST write,
    -- because the id is what recovery reconciles on. IMMUTABLE.
    native_session_id TEXT NOT NULL
                      CHECK (typeof(native_session_id) = 'text'
                             AND length(CAST(native_session_id AS BLOB)) BETWEEN 1 AND 128),
    -- The adapter's own correlation reference for this launch, so a log
    -- line or an evidence pointer can be tied back to this attempt.
    correlation_ref   TEXT NOT NULL
                      CHECK (typeof(correlation_ref) = 'text'
                             AND length(CAST(correlation_ref AS BLOB)) BETWEEN 1 AND 128),
    -- The workspace identity (a stable label) and the absolute path the
    -- runtime was launched in. The adapter verifies the runtime's own
    -- reported location against workspace_path before accepting any
    -- result, so a session that ran somewhere else is detected rather
    -- than trusted.
    workspace_id      TEXT NOT NULL
                      CHECK (typeof(workspace_id) = 'text'
                             AND length(CAST(workspace_id AS BLOB)) BETWEEN 1 AND 128),
    workspace_path    TEXT NOT NULL
                      CHECK (typeof(workspace_path) = 'text'
                             AND length(CAST(workspace_path AS BLOB)) BETWEEN 1 AND 1024),
    -- The launch/reconciliation state. Closed vocabulary, and it only
    -- moves FORWARD (trigger below).
    --
    --   intent      the row exists; the runtime has NOT been launched
    --   launched    the runtime process was started
    --   running     the runtime reports the session is working
    --   completed   the runtime reported a real completion
    --   failed      the runtime reported a failure (or vanished)
    --   ambiguous   the runtime could not tell us whether work happened
    --   cancelled   an operator cancelled the attempt
    launch_state      TEXT NOT NULL
                      CHECK (launch_state IN ('intent','launched','running',
                                              'completed','failed','ambiguous',
                                              'cancelled')),
    -- The runtime's own reported outcome, when it gave one. NULL means
    -- "the runtime did not say", which is different from "it said
    -- failure" — the adapter records the difference rather than folding
    -- it into a boolean.
    runtime_outcome   TEXT
                      CHECK (runtime_outcome IS NULL
                             OR runtime_outcome IN ('succeeded','failed')),
    -- A bounded, credential-free note about the last error or ambiguity.
    -- Never a transcript, never a model output, never a secret.
    last_error        TEXT
                      CHECK (last_error IS NULL OR typeof(last_error) = 'text'),
    created_at        INTEGER NOT NULL
                      CHECK (typeof(created_at) = 'integer'),
    updated_at        INTEGER NOT NULL
                      CHECK (typeof(updated_at) = 'integer')
);
-- One attempt number per task.
CREATE UNIQUE INDEX IF NOT EXISTS idx_agent_task_sessions_task_attempt
    ON agent_task_sessions(task_id, attempt);
-- A native session belongs to at most ONE attempt. This is what stops two
-- assignments from claiming the same runtime object, and it is also what
-- makes "the session already exists" a recoverable fact rather than a
-- coincidence.
CREATE UNIQUE INDEX IF NOT EXISTS idx_agent_task_sessions_native
    ON agent_task_sessions(runtime, native_session_id);
-- SINGLE-FLIGHT ADMISSION.
--
-- At most ONE non-terminal attempt per task PER CLAIM EPOCH, enforced by the
-- engine. Two concurrent invocations both read "no live attempt" outside a
-- transaction and both try to write their intent; only one INSERT can
-- succeed, so exactly one obtains permission to launch. A service-level check
-- cannot do this — the read and the write are not one critical section.
--
-- SCOPED TO THE EPOCH on purpose. A worker whose lease was reclaimed leaves a
-- stale intent behind, and an index on (task_id) alone would then block every
-- future attempt of that assignment forever. Two writers under DIFFERENT
-- epochs are already refused by the reconciler, which can inspect the old
-- session; this index is the atomic half, and it only needs to cover the
-- epoch the caller claims.
CREATE UNIQUE INDEX IF NOT EXISTS idx_agent_task_sessions_one_live
    ON agent_task_sessions(task_id, claim_epoch)
    WHERE launch_state IN ('intent','launched','running');
-- The reconciler's hot path: the live attempt for a task.
CREATE INDEX IF NOT EXISTS idx_agent_task_sessions_task_state
    ON agent_task_sessions(task_id, launch_state, attempt DESC);
-- An attempt's result is written by whoever holds the claim, so the fence
-- index carries the epoch.
CREATE INDEX IF NOT EXISTS idx_agent_task_sessions_epoch
    ON agent_task_sessions(claim_epoch);

-- ── Rule 1: an attempt belongs to a claim that is CURRENT ────────────────
-- You cannot open an attempt against an epoch the assignment does not
-- hold. A stale worker's intent write matches no claim and is refused
-- here, which is the earliest possible fence.
CREATE TRIGGER IF NOT EXISTS trg_agent_task_sessions_epoch_is_current
BEFORE INSERT ON agent_task_sessions
FOR EACH ROW
WHEN NOT EXISTS (
    SELECT 1 FROM agent_tasks
     WHERE id = NEW.task_id AND claim_epoch = NEW.claim_epoch
)
BEGIN
    SELECT RAISE(ABORT, 'agent_task_sessions: the attempt epoch is not the assignment''s current claim');
END;

-- ── Rule 2: the identity is WRITE-ONCE ───────────────────────────────────
-- The epoch and the native session id are what recovery and fencing
-- reconcile on. Rewriting either makes the recovery record lie.
CREATE TRIGGER IF NOT EXISTS trg_agent_task_sessions_id_is_not_reused
BEFORE INSERT ON agent_task_sessions
FOR EACH ROW
WHEN EXISTS (SELECT 1 FROM agent_task_sessions WHERE id = NEW.id)
  OR EXISTS (SELECT 1 FROM agent_task_sessions
              WHERE runtime = NEW.runtime
                AND native_session_id = NEW.native_session_id)
BEGIN
    -- AN INSERT TRIGGER FIRES FOR INSERT OR REPLACE TOO.
    --
    -- PR 1's fifth review found this exact hole on agent_tasks: REPLACE
    -- resolves its conflict by DELETING the conflicting row, and SQLite
    -- does not fire DELETE triggers for that implicit delete when
    -- recursive_triggers is OFF (it is OFF by default, and this project
    -- never sets it). So a REPLACE would delete an attempt — defeating
    -- the not-deletable rule — and re-insert it — defeating write-once
    -- and the forward-only state graph — in ONE statement. Refusing the
    -- insert is what closes all three at once.
    --
    -- BOTH UNIQUENESS CONFLICTS ARE GUARDED. A REPLACE whose conflict
    -- target is the (runtime, native_session_id) index would otherwise
    -- delete a DIFFERENT attempt's row and take its identity with it.
    SELECT RAISE(ABORT, 'agent_task_sessions: the attempt id is not reusable');
END;

CREATE TRIGGER IF NOT EXISTS trg_agent_task_sessions_identity_is_immutable
BEFORE UPDATE OF id, task_id, claim_epoch, native_session_id, runtime, attempt,
                 intended_agent, correlation_ref, workspace_id, workspace_path,
                 created_at
ON agent_task_sessions
FOR EACH ROW
WHEN NEW.id IS NOT OLD.id
  OR NEW.task_id IS NOT OLD.task_id
  OR NEW.claim_epoch IS NOT OLD.claim_epoch
  OR NEW.native_session_id IS NOT OLD.native_session_id
  OR NEW.runtime IS NOT OLD.runtime
  OR NEW.attempt IS NOT OLD.attempt
  OR NEW.intended_agent IS NOT OLD.intended_agent
  OR NEW.correlation_ref IS NOT OLD.correlation_ref
  OR NEW.workspace_id IS NOT OLD.workspace_id
  OR NEW.workspace_path IS NOT OLD.workspace_path
  OR NEW.created_at IS NOT OLD.created_at
BEGIN
    SELECT RAISE(ABORT, 'agent_task_sessions: the attempt identity is write-once');
END;

-- ── Rule 3: the launch state moves FORWARD only ──────────────────────────
-- A completed attempt cannot be reopened as an intent, and a failed one
-- cannot be relabelled as a success. The graph is stated in one place so
-- an added state is an obvious edit rather than a silent widening.
CREATE TRIGGER IF NOT EXISTS trg_agent_task_sessions_state_moves_forward
BEFORE UPDATE OF launch_state ON agent_task_sessions
FOR EACH ROW
WHEN OLD.launch_state <> NEW.launch_state
 AND NOT (
        (OLD.launch_state = 'intent'    AND NEW.launch_state IN ('launched','completed','failed','ambiguous','cancelled'))
     OR (OLD.launch_state = 'launched'  AND NEW.launch_state IN ('running','completed','failed','ambiguous','cancelled'))
     OR (OLD.launch_state = 'running'   AND NEW.launch_state IN ('completed','failed','ambiguous','cancelled'))
 )
BEGIN
    SELECT RAISE(ABORT, 'agent_task_sessions: the launch state may only move forward');
END;

-- ── Rule 4: an attempt number is one more than the last ──────────────────
-- The unique index says two attempts cannot share a number. This says a
-- task's attempts are CONSECUTIVE, so the record cannot show attempt 1 and
-- attempt 7 with a silent gap where attempts 2..6 would be.
CREATE TRIGGER IF NOT EXISTS trg_agent_task_sessions_attempt_is_next
BEFORE INSERT ON agent_task_sessions
FOR EACH ROW
WHEN NEW.attempt <> (
    SELECT COALESCE(MAX(attempt), 0) + 1
      FROM agent_task_sessions
     WHERE task_id = NEW.task_id
)
BEGIN
    SELECT RAISE(ABORT, 'agent_task_sessions: each attempt must follow the last');
END;

-- ── Rule 5: the session record outlives the attempt ─────────────────────
-- Deleting the recovery record would make an executed assignment look
-- unexecuted. It is not deletable while its assignment exists.
CREATE TRIGGER IF NOT EXISTS trg_agent_task_sessions_not_deletable
BEFORE DELETE ON agent_task_sessions
FOR EACH ROW
BEGIN
    SELECT RAISE(ABORT, 'agent_task_sessions: the execution record is not deletable');
END;
`

// MigrateAgentTaskSessions creates the assignment-to-session mapping.
//
// It MUST be called inside the fatal migration transaction (db.go), for
// the same reason every other migration is: the DDL and the sentinel share
// one transaction, so a failure at any point rolls the whole thing back and
// NewDatabaseManager returns an error rather than leaving a half-migrated
// database.
//
// Idempotent via the `agent_task_sessions_v1` sentinel.
func MigrateAgentTaskSessions(tx *sql.Tx) error {
	const sentinel = "agent_task_sessions_v1"

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

	if _, err := tx.Exec(agentTaskSessionDDL); err != nil {
		return fmt.Errorf("create the task session table: %w", err)
	}

	// The sentinel is written LAST, inside the same transaction.
	if _, err := tx.Exec(
		`INSERT OR IGNORE INTO schema_migrations (id, applied_at) VALUES (?, ?)`,
		sentinel, time.Now().Unix(),
	); err != nil {
		return fmt.Errorf("record %s sentinel: %w", sentinel, err)
	}
	return nil
}
