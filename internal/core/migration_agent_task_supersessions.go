// migration_agent_task_supersessions.go — Continuity 001 recovery: the
// immutable supersession lineage.
//
// WHAT THIS IS
// ------------
// One additive, local-only table recording operator-authorised recovery:
// when an ambiguous execution blocks a task forever, the operator names a
// replacement implementer and an independent reviewer that continue the
// work, bound to the exact ambiguous attempt. The lineage is the binding,
// not prose: scope, execution authority and read-state interpretation are
// all derived from these rows.
//
// WHY A TABLE AND NOT A PAYLOAD FIELD
// -----------------------------------
// `agent_tasks.payload` is written at creation and is not in any
// transition's settable-column whitelist, so it cannot carry a fact known
// later. The lineage is separate state with its own lifecycle and its own
// immutability rule, so it gets its own table (the same reason the
// assignment-to-session mapping got one).
//
// WHAT THE TRIGGERS ENFORCE
// -------------------------
//   - the lineage row is write-once (TR1/TR2 refuse every UPDATE/DELETE);
//   - it names an eligible original (non-root implementer, live-ish
//     state) and its project/root (TR3/TR8);
//   - it names exactly one ambiguous attempt of that original, with no
//     other ambiguous, intent, launched or running attempt on the task
//     (TR4/TR5);
//   - it names fresh, distinct, well-shaped replacement and reviewer ids
//     that are not the original or the root and admit no cycle (TR6/TR7);
//   - reserved idempotency keys belong to a lineage and match its exact
//     form; lineage-named tasks carry no other key (TR9);
//   - scoped tasks carry the sealed shape at INSERT (TR10);
//   - claims, renewals and every state transition respect ambiguity,
//     history and terminal evidence (TR11–TR20).
//
// Idempotent via the `agent_task_supersessions_v1` sentinel, written LAST
// inside the same transaction as the DDL.
package internal

import (
	"database/sql"
	"fmt"
	"time"
)

// agentTaskSupersessionTableDDL is the approved DDL for the supersession
// lineage: the table plus its supporting index.
//
// The SHA-256 of the FULL migration text (table, indexes and all twenty-one trigger statements
// triggers) is pinned by TestAgentSupersessionDDLIsPinned, so the storage
// cannot drift from docs without failing the build.
const agentTaskSupersessionTableDDL = `-- ── agent_task_supersessions ────────────────────────────────────────────
-- One row per operator-authorised recovery: the original task whose
-- ambiguous attempt blocks it forever, the attempt that is ambiguous, and
-- the replacement implementer plus independent reviewer that continue
-- the work. The lineage is written FIRST (deferred composite FKs let the
-- replacement and reviewer rows land afterwards in the same transaction),
-- and it is never updated or deleted afterwards.
CREATE TABLE IF NOT EXISTS agent_task_supersessions (
    id                  TEXT PRIMARY KEY NOT NULL
                        CHECK (typeof(id) = 'text' AND id GLOB 'atsup-*'),
    -- The project the lineage belongs to. Restrictive: the lineage, the
    -- original and the tree share it.
    project_id          TEXT NOT NULL
                        REFERENCES projects(id) ON DELETE RESTRICT,
    -- The tree the lineage belongs to. Every predicate in this feature is
    -- tree-scoped through this column.
    root_id             TEXT NOT NULL,
    -- The blocked original. Non-root implementer, enforced by TR3.
    superseded_task_id  TEXT NOT NULL,
    -- The replacement implementer, created by the same transaction.
    -- Deferred: the lineage names it before its row exists.
    replacement_task_id TEXT NOT NULL,
    -- The independent reviewer, created by the same transaction.
    -- Deferred: the lineage names it before its row exists.
    reviewer_task_id    TEXT NOT NULL,
    -- The exact ambiguous attempt that blocks the original.
    attempt_id          TEXT NOT NULL
                        REFERENCES agent_task_sessions(id) ON DELETE RESTRICT,
    -- Who authorised the recovery. Attribution, not authentication: an
    -- unexported OperatorAuthority carries it, and the tool registry has
    -- no verb that reaches it.
    authorised_by       TEXT NOT NULL
                        CHECK (typeof(authorised_by) = 'text'
                               AND length(trim(authorised_by)) BETWEEN 1 AND 200),
    -- The authority kind. The vocabulary is closed: only the operator's
    -- own front door may authorise a supersession.
    authority_kind      TEXT NOT NULL
                        CHECK (authority_kind = 'operator'),
    -- Where the authorisation is recorded. Prefix-only with a length
    -- floor, the same contract as the review evidence pointer.
    evidence_ref        TEXT NOT NULL
                        CHECK (length(evidence_ref) BETWEEN 8 AND 1024
                               AND (evidence_ref GLOB 'gh:*'
                                    OR evidence_ref GLOB 'git:*'
                                    OR evidence_ref GLOB 'file:*'
                                    OR evidence_ref GLOB 'log:*')),
    -- Why the original cannot proceed and what the replacement is for.
    justification       TEXT NOT NULL
                        CHECK (typeof(justification) = 'text'
                               AND length(trim(justification)) BETWEEN 1 AND 4096),
    created_at          INTEGER NOT NULL
                        CHECK (typeof(created_at) = 'integer'),
    -- One lineage per (original, attempt): the ambiguous attempt is named
    -- once, and a second lineage over the same attempt is a duplicate
    -- recovery, not a new one.
    UNIQUE (superseded_task_id, attempt_id),
    -- An attempt is named by at most one lineage.
    UNIQUE (attempt_id),
    -- A replacement is created by exactly one lineage.
    UNIQUE (replacement_task_id),
    -- A reviewer is created by exactly one lineage.
    UNIQUE (reviewer_task_id),
    CHECK (superseded_task_id <> replacement_task_id),
    CHECK (superseded_task_id <> reviewer_task_id),
    CHECK (replacement_task_id <> reviewer_task_id),
    -- Composite deferred references. INITIALLY DEFERRED so the lineage
    -- row, which names rows created later in the same transaction, passes
    -- its checks at COMMIT rather than at INSERT. A missing replacement
    -- or reviewer at COMMIT fails the whole transaction.
    FOREIGN KEY (superseded_task_id, project_id)
        REFERENCES agent_tasks(id, project_id) ON DELETE RESTRICT
        DEFERRABLE INITIALLY DEFERRED,
    FOREIGN KEY (root_id, project_id)
        REFERENCES agent_tasks(id, project_id) ON DELETE RESTRICT
        DEFERRABLE INITIALLY DEFERRED,
    FOREIGN KEY (replacement_task_id, project_id)
        REFERENCES agent_tasks(id, project_id) ON DELETE RESTRICT
        DEFERRABLE INITIALLY DEFERRED,
    FOREIGN KEY (reviewer_task_id, project_id)
        REFERENCES agent_tasks(id, project_id) ON DELETE RESTRICT
        DEFERRABLE INITIALLY DEFERRED
);
-- The tree-scoped read path: every predicate in this feature starts from
-- the lineage rows of one tree.
CREATE INDEX IF NOT EXISTS idx_agent_task_supersessions_root
    ON agent_task_supersessions(root_id);
`

// agentTaskSupersessionTriggerDDL holds the twenty recovery rules (TR1-TR20) in twenty-one trigger statements: TR20 has separate UPDATE and INSERT guards because SQLite does not accept a column list on INSERT triggers.
//
// TR1–TR8 guard the lineage table. TR9–TR18 guard agent_tasks. TR19–TR20
// guard agent_task_sessions. The DDL pin covers the assembled text of all
// of them together with the table.
var agentTaskSupersessionTriggerDDL = `-- ── Rule TR1: the lineage is write-once ─────────────────────────────────
-- A lineage that could be edited could be repointed at a different
-- attempt, a different original or a different replacement after the
-- recovery it authorises. There is no edit; a mistaken lineage stays as
-- the record of the mistake.
CREATE TRIGGER IF NOT EXISTS trg_agent_task_supersessions_no_update
BEFORE UPDATE ON agent_task_supersessions
FOR EACH ROW
BEGIN
    SELECT RAISE(ABORT, 'agent_task_supersessions: the lineage is write-once');
END;

-- ── Rule TR2: the lineage is not deletable ─────────────────────────────────
-- Deleting the lineage would orphan the recovery it authorises while the
-- replacement keeps running. The record outlives the work.
CREATE TRIGGER IF NOT EXISTS trg_agent_task_supersessions_no_delete
BEFORE DELETE ON agent_task_supersessions
FOR EACH ROW
BEGIN
    SELECT RAISE(ABORT, 'agent_task_supersessions: the lineage is not deletable');
END;

-- ── Rule TR3: the original is eligible ─────────────────────────────────────
-- A root cannot be superseded; only a non-root implementer in a live-ish
-- state (in_progress, completed, cancelled) names a lineage. Terminal and
-- gate states are not recoverable work.
CREATE TRIGGER IF NOT EXISTS trg_agent_task_supersessions_original_eligible
BEFORE INSERT ON agent_task_supersessions
FOR EACH ROW
WHEN NOT EXISTS (SELECT 1 FROM agent_tasks WHERE id = NEW.superseded_task_id)
  OR EXISTS (SELECT 1 FROM agent_tasks
              WHERE id = NEW.superseded_task_id
                AND (role <> 'implementer'
                     OR depth = 0
                     OR state NOT IN ('in_progress','completed','cancelled')))
BEGIN
    SELECT RAISE(ABORT, 'agent_task_supersessions: the superseded task is not an eligible non-root implementer');
END;

-- ── Rule TR8: the lineage stays inside one tree ────────────────────────────
-- The lineage, the original and the tree share one project and one root.
-- A lineage that crossed trees would authorise execution somewhere its
-- evidence does not describe.
CREATE TRIGGER IF NOT EXISTS trg_agent_task_supersessions_contained
BEFORE INSERT ON agent_task_supersessions
FOR EACH ROW
WHEN EXISTS (SELECT 1 FROM agent_tasks
              WHERE id = NEW.superseded_task_id
                AND (project_id <> NEW.project_id OR root_id <> NEW.root_id))
BEGIN
    SELECT RAISE(ABORT, 'agent_task_supersessions: the lineage crosses a project or tree boundary');
END;

-- ── Rule TR4: the named attempt is that task's ambiguity ───────────────────
-- The lineage binds one exact attempt, and it must be an ambiguous
-- attempt OF THE SUPERSEDED TASK. A missing, foreign or non-ambiguous
-- attempt names no blockage.
CREATE TRIGGER IF NOT EXISTS trg_agent_task_supersessions_attempt_is_ambiguity
BEFORE INSERT ON agent_task_supersessions
FOR EACH ROW
WHEN NOT EXISTS (SELECT 1 FROM agent_task_sessions
                  WHERE id = NEW.attempt_id
                    AND task_id = NEW.superseded_task_id
                    AND launch_state = 'ambiguous')
BEGIN
    SELECT RAISE(ABORT, 'agent_task_supersessions: the named attempt is not that task''s ambiguity');
END;

-- ── Rule TR5: exactly one ambiguity, nothing still live ────────────────────
-- The supported slice is one unresolved ambiguous attempt. Several
-- ambiguities, or an intent/launched/running attempt beside the
-- ambiguity, is a tree the operator has to reconcile first.
CREATE TRIGGER IF NOT EXISTS trg_agent_task_supersessions_single_ambiguity
BEFORE INSERT ON agent_task_supersessions
FOR EACH ROW
WHEN (SELECT COUNT(*) FROM agent_task_sessions
       WHERE task_id = NEW.superseded_task_id
         AND launch_state = 'ambiguous') > 1
  OR EXISTS (SELECT 1 FROM agent_task_sessions
              WHERE task_id = NEW.superseded_task_id
                AND launch_state IN ('intent','launched','running'))
BEGIN
    SELECT RAISE(ABORT, 'agent_task_supersessions: the original has more than one unresolved ambiguity');
END;

-- ── Rule TR6: the new identities are fresh and distinct ────────────────────
-- The replacement and reviewer are created by this same operation. An id
-- that already exists would adopt somebody else's history; equal ids
-- would collapse the three roles into fewer rows.
CREATE TRIGGER IF NOT EXISTS trg_agent_task_supersessions_fresh_identities
BEFORE INSERT ON agent_task_supersessions
FOR EACH ROW
WHEN NEW.replacement_task_id = NEW.reviewer_task_id
  OR NEW.replacement_task_id = NEW.superseded_task_id
  OR NEW.reviewer_task_id = NEW.superseded_task_id
  OR NEW.replacement_task_id = NEW.root_id
  OR NEW.reviewer_task_id = NEW.root_id
  OR EXISTS (SELECT 1 FROM agent_tasks
              WHERE id IN (NEW.replacement_task_id, NEW.reviewer_task_id))
  OR NEW.replacement_task_id NOT GLOB 'at-[0-9a-f][0-9a-f][0-9a-f][0-9a-f][0-9a-f][0-9a-f][0-9a-f][0-9a-f][0-9a-f][0-9a-f][0-9a-f][0-9a-f][0-9a-f][0-9a-f][0-9a-f][0-9a-f]'
  OR NEW.reviewer_task_id NOT GLOB 'at-[0-9a-f][0-9a-f][0-9a-f][0-9a-f][0-9a-f][0-9a-f][0-9a-f][0-9a-f][0-9a-f][0-9a-f][0-9a-f][0-9a-f][0-9a-f][0-9a-f][0-9a-f][0-9a-f]'
BEGIN
    SELECT RAISE(ABORT, 'agent_task_supersessions: the replacement and reviewer identities are not fresh and distinct');
END;

-- ── Rule TR7: no recovery cycle ─────────────────────────────────────────────
-- Following superseded→replacement edges from the replacement must never
-- reach the superseded task. A cycle would let a lineage authorise its
-- own ancestor.
CREATE TRIGGER IF NOT EXISTS trg_agent_task_supersessions_no_cycle
BEFORE INSERT ON agent_task_supersessions
FOR EACH ROW
WHEN EXISTS (
    WITH RECURSIVE chain(id) AS (
        SELECT NEW.replacement_task_id
        UNION
        SELECT l.replacement_task_id FROM agent_task_supersessions l
         JOIN chain c ON l.superseded_task_id = c.id
    )
    SELECT 1 FROM chain WHERE id = NEW.superseded_task_id
)
BEGIN
    SELECT RAISE(ABORT, 'agent_task_supersessions: the replacement would close a recovery cycle');
END;

-- ── Rule TR9: reserved keys belong to a lineage ────────────────────────────
-- 'supersede:' keys are created only by a supersession, and a task a
-- lineage names carries only its reserved key. Anything else is squatting
-- (T58) or a legacy collision (T71/T121).
CREATE TRIGGER IF NOT EXISTS trg_agent_tasks_reserved_key
BEFORE INSERT ON agent_tasks
FOR EACH ROW
WHEN (NEW.idempotency_key GLOB 'supersede:*'
      AND NOT EXISTS (
          SELECT 1 FROM agent_task_supersessions L
           WHERE (L.replacement_task_id = NEW.id
                  AND 'supersede:' || L.attempt_id = NEW.idempotency_key)
              OR (L.reviewer_task_id = NEW.id
                  AND 'supersede:' || L.attempt_id || ':review' = NEW.idempotency_key)))
  OR (NOT (NEW.idempotency_key GLOB 'supersede:*')
      AND EXISTS (
          SELECT 1 FROM agent_task_supersessions L
           WHERE L.replacement_task_id = NEW.id OR L.reviewer_task_id = NEW.id))
BEGIN
    SELECT RAISE(ABORT, 'agent_tasks: the idempotency key is reserved for supersession lineages');
END;

-- ── Rule TR10: lineage tasks carry the sealed shape ─────────────────────────
-- A supersession-created task is enqueued, unclaimed, placed as the
-- original's sibling, and — for the reviewer — pointed at the
-- replacement. Ordinary CreateRoot/Delegate rows are not evaluated.
CREATE TRIGGER IF NOT EXISTS trg_agent_tasks_superseded_shape
BEFORE INSERT ON agent_tasks
FOR EACH ROW
WHEN NEW.idempotency_key GLOB 'supersede:*'
 AND (EXISTS (
        SELECT 1 FROM agent_task_supersessions L
         WHERE L.replacement_task_id = NEW.id
           AND (NEW.role <> 'implementer'
                OR NEW.state <> 'enqueued'
                OR NEW.claim_epoch <> 0
                OR NULLIF(NEW.base_sha, '') IS NULL
                OR NEW.parent_id <> (SELECT parent_id FROM agent_tasks WHERE id = L.superseded_task_id)
                OR NEW.depth <> (SELECT depth FROM agent_tasks WHERE id = L.superseded_task_id)
                OR EXISTS (SELECT 1 FROM agent_task_sessions WHERE task_id = NEW.id))
      )
      OR EXISTS (
        SELECT 1 FROM agent_task_supersessions L
         WHERE L.reviewer_task_id = NEW.id
           AND (NEW.role <> 'reviewer'
                OR NEW.state <> 'enqueued'
                OR NEW.claim_epoch <> 0
                OR json_extract(NEW.payload, '$.review_target') IS NOT L.replacement_task_id
                OR NEW.parent_id <> (SELECT parent_id FROM agent_tasks WHERE id = L.superseded_task_id)
                OR NEW.depth <> (SELECT depth FROM agent_tasks WHERE id = L.superseded_task_id)
                OR EXISTS (SELECT 1 FROM agent_task_sessions WHERE task_id = NEW.id))
      ))
BEGIN
    SELECT RAISE(ABORT, 'agent_tasks: a supersession-created task must carry the sealed shape');
END;

-- ── Rule TR11: claims and renewals respect ambiguity, history and terminal state ──
-- The fence is in the claim's own statement, and the trigger is the
-- backstop: an ambiguous tree admits only authorised execution.
--
-- The guard speaks only for the ambiguity arm. Terminal tasks hold no
-- claim under the base claim-discipline trigger, which reports them with
-- the base message; firing here as well would make the refusal message
-- arbitrary (the base DDL documents that SQLite does not specify trigger
-- order). Terminal refusal is enforced in the service switch and by the
-- base trigger; this trigger owns the ambiguous-tree case, including a
-- reclaim on live work while ExecAuth is false.
CREATE TRIGGER IF NOT EXISTS trg_agent_tasks_claim_epoch_guard
BEFORE UPDATE OF claim_epoch ON agent_tasks
FOR EACH ROW
WHEN EXISTS (SELECT 1 FROM agent_task_sessions s
               JOIN agent_tasks t ON t.id = s.task_id
              WHERE t.root_id = NEW.root_id
                AND s.launch_state = 'ambiguous')
      AND NOT (` + execAuthFragmentFor("NEW") + `
      )
BEGIN
    SELECT RAISE(ABORT, 'agent_tasks: the claim is refused by the execution guard');
END;

-- ── Rule TR12: only authorised execution may start ──────────────────────────
-- Starting work in an ambiguous tree is a launch decision, so the same
-- authorisation that gates the launch gates the start.
--
-- The guard speaks only on the base-legal path (accepted → in_progress):
-- for any other predecessor the base state machine already reports an
-- illegal transition, and two triggers refusing the same write would
-- make the message arbitrary (the base DDL documents that SQLite does
-- not specify trigger order).
CREATE TRIGGER IF NOT EXISTS trg_agent_tasks_start_guard
BEFORE UPDATE OF state ON agent_tasks
FOR EACH ROW
WHEN NEW.state = 'in_progress' AND OLD.state = 'accepted'
` + execAuthRefusalFor("NEW", false) + `
BEGIN
    SELECT RAISE(ABORT, 'agent_tasks: the start is refused by the execution guard');
END;

-- ── Rule TR13: completions carry terminal success evidence ─────────────────
-- A runtime-managed completion needs a current-epoch completed+succeeded
-- attempt. Own ambiguity and history are refused outright.
--
-- The guard speaks only on the base-legal path (in_progress → completed
-- for an implementer): any other predecessor is the base state
-- machine's illegal transition to report.
CREATE TRIGGER IF NOT EXISTS trg_agent_tasks_complete_guard
BEFORE UPDATE OF state ON agent_tasks
FOR EACH ROW
WHEN NEW.state = 'completed' AND OLD.role = 'implementer' AND OLD.state = 'in_progress'
 AND (EXISTS (SELECT 1 FROM agent_task_sessions s
               WHERE s.task_id = NEW.id AND s.launch_state = 'ambiguous')
      OR ` + historicalFor("NEW") + `
      OR NOT EXISTS (SELECT 1 FROM agent_task_sessions s
                      WHERE s.task_id = NEW.id
                        AND s.claim_epoch = NEW.claim_epoch
                        AND s.launch_state = 'completed'
                        AND s.runtime_outcome = 'succeeded'))
BEGIN
    SELECT RAISE(ABORT, 'agent_tasks: completion needs a current-epoch completed and succeeded attempt');
END;

-- ── Rule TR14: withdraw only what is unambiguous ────────────────────────────
-- Cancelling an ambiguous or superseded task would hide the blockage the
-- recovery status reports. Withdrawal stays available for everything else,
-- including the root of a blocked tree.
CREATE TRIGGER IF NOT EXISTS trg_agent_tasks_cancel_guard
BEFORE UPDATE OF state ON agent_tasks
FOR EACH ROW
WHEN NEW.state = 'cancelled' AND OLD.state <> 'cancelled'
 AND (EXISTS (SELECT 1 FROM agent_task_sessions s
               WHERE s.task_id = NEW.id AND s.launch_state = 'ambiguous')
      OR ` + historicalFor("NEW") + `)
BEGIN
    SELECT RAISE(ABORT, 'agent_tasks: an ambiguous or superseded task cannot be withdrawn');
END;

-- ── Rule TR15: the gate follows a passed review ────────────────────────────
-- Handoff names the operator gate; it moves only from a passed review,
-- and never for an ambiguous or superseded task.
--
-- The guard speaks only on the base-legal path (review_passed →
-- awaiting_operator): any other predecessor is the base state machine's
-- illegal transition to report.
CREATE TRIGGER IF NOT EXISTS trg_agent_tasks_handoff_guard
BEFORE UPDATE OF state ON agent_tasks
FOR EACH ROW
WHEN NEW.state = 'awaiting_operator' AND OLD.state = 'review_passed'
 AND (EXISTS (SELECT 1 FROM agent_task_sessions s
               WHERE s.task_id = NEW.id AND s.launch_state = 'ambiguous')
      OR ` + historicalFor("NEW") + `)
BEGIN
    SELECT RAISE(ABORT, 'agent_tasks: the handoff needs a passed review on live work');
END;

-- ── Rule TR16: passed reviews stay on live work ────────────────────────────
-- The guard speaks only on the base-legal path (a reviewer moving from
-- in_progress): any other predecessor is the base state machine's
-- illegal transition to report.
CREATE TRIGGER IF NOT EXISTS trg_agent_tasks_review_passed_guard
BEFORE UPDATE OF state ON agent_tasks
FOR EACH ROW
WHEN NEW.state = 'review_passed' AND OLD.state = 'in_progress'
 AND (` + historicalFor("NEW") + `)
BEGIN
    SELECT RAISE(ABORT, 'agent_tasks: a passed verdict cannot land on historical work');
END;

-- ── Rule TR17: approvals stay on live work ──────────────────────────────────
-- The guard speaks only on the base-legal path (awaiting_operator →
-- operator_approved): any other predecessor is the base state machine's
-- illegal transition to report.
CREATE TRIGGER IF NOT EXISTS trg_agent_tasks_approval_guard
BEFORE UPDATE OF state ON agent_tasks
FOR EACH ROW
WHEN NEW.state = 'operator_approved' AND OLD.state = 'awaiting_operator'
 AND (EXISTS (SELECT 1 FROM agent_task_sessions s
               WHERE s.task_id = NEW.id AND s.launch_state = 'ambiguous')
      OR ` + historicalFor("NEW") + `)
BEGIN
    SELECT RAISE(ABORT, 'agent_tasks: the approval cannot land on ambiguous or historical work');
END;

-- ── Rule TR18: verdict transitions carry captured evidence ─────────────────
-- A reviewer verdict without host-captured evidence is a decision with
-- nothing behind it. Cancellation and refusal from in_progress are not
-- verdicts and are not gated here; TR14 governs ambiguity and history.
CREATE TRIGGER IF NOT EXISTS trg_agent_tasks_reviewer_evidence_guard
BEFORE UPDATE OF state ON agent_tasks
FOR EACH ROW
WHEN OLD.role = 'reviewer' AND OLD.state = 'in_progress'
 AND NEW.state IN ('completed','review_passed')
 AND (NEW.reviewed_sha IS NULL OR NEW.reviewed_sha = ''
      OR NEW.findings_sha256 IS NULL OR NEW.findings_sha256 = '')
BEGIN
    SELECT RAISE(ABORT, 'agent_tasks: a reviewer verdict needs captured evidence');
END;

-- ── Rule TR19: attempts open only for authorised execution ─────────────────
-- BeginAttempt is the final atomic admission: an attempt row for a task
-- the tree does not authorise would be a launch right by another name.
CREATE TRIGGER IF NOT EXISTS trg_agent_task_sessions_admission_guard
BEFORE INSERT ON agent_task_sessions
FOR EACH ROW
WHEN EXISTS (SELECT 1 FROM agent_tasks t WHERE t.id = NEW.task_id
` + execAuthRefusalSubselect("t") + `)
BEGIN
    SELECT RAISE(ABORT, 'agent_task_sessions: no authorised execution for this task');
END;

-- ── Rule TR20: a completed attempt succeeded ────────────────────────────────
-- A completed attempt whose outcome is missing or anything but
-- 'succeeded' is a record that claims success it does not have. Attempts
-- are born as 'intent' (BeginAttempt hardcodes it), so every production
-- path to 'completed' is an UPDATE; the INSERT guard closes the same
-- hole for direct writes, matching the restore validator's
-- completed_without_success refusal (T106).
CREATE TRIGGER IF NOT EXISTS trg_agent_task_sessions_completion_guard
BEFORE UPDATE OF launch_state, runtime_outcome ON agent_task_sessions
FOR EACH ROW
WHEN NEW.launch_state = 'completed'
 AND (NEW.runtime_outcome IS NULL OR NEW.runtime_outcome <> 'succeeded')
BEGIN
    SELECT RAISE(ABORT, 'agent_task_sessions: a completed attempt must report success');
END;

-- TR20 covers INSERT as well as UPDATE: a directly-written completed
-- attempt without success is the same false record. The restore path
-- replays genuine completed+succeeded history, which this permits, and
-- refuses anything else before the destructive window.
CREATE TRIGGER IF NOT EXISTS trg_agent_task_sessions_completion_insert_guard
BEFORE INSERT ON agent_task_sessions
FOR EACH ROW
WHEN NEW.launch_state = 'completed'
 AND (NEW.runtime_outcome IS NULL OR NEW.runtime_outcome <> 'succeeded')
BEGIN
    SELECT RAISE(ABORT, 'agent_task_sessions: a completed attempt must report success');
END;
`

// execAuthFragmentFor builds the tree-scoped execution-authorisation test
// for one task row. row is the SQL expression naming the task row whose
// authority is judged ("NEW" inside agent_tasks triggers; "t" inside the
// task subselect of the sessions trigger).
//
// The inner join alias is ax_task (never NEW, never the caller's alias):
// reusing the caller's alias would shadow the outer row and collapse the
// tree scope to the whole database. That defect would refuse authorised
// work in clean trees whenever any other tree held an ambiguity — the
// service predicate and the trigger would disagree, and the trigger
// would win.
//
// ExecAuth(X) = NOT Historical(X) AND NOT OwnAmb(X)
//
//	AND (Amb(T) empty OR (Named(T) = Amb(T) AND Scoped(X)))
func execAuthFragmentFor(row string) string {
	return `
  NOT (
    ` + historicalFor(row) + `
  )
  AND NOT EXISTS (SELECT 1 FROM agent_task_sessions s
                   WHERE s.task_id = ` + row + `.id
                     AND s.launch_state = 'ambiguous')
  AND (
    NOT EXISTS (SELECT 1 FROM agent_task_sessions s
                 JOIN agent_tasks ax_task ON ax_task.id = s.task_id
                WHERE ax_task.root_id = ` + row + `.root_id
                  AND s.launch_state = 'ambiguous')
    OR (
      NOT EXISTS (SELECT 1 FROM agent_task_sessions s
                   JOIN agent_tasks ax_task ON ax_task.id = s.task_id
                  WHERE ax_task.root_id = ` + row + `.root_id
                    AND s.launch_state = 'ambiguous'
                    AND NOT EXISTS (SELECT 1 FROM agent_task_supersessions L
                                     WHERE L.attempt_id = s.id))
      AND EXISTS (SELECT 1 FROM agent_task_supersessions L
                   WHERE L.root_id = ` + row + `.root_id
                     AND (L.replacement_task_id = ` + row + `.id
                          OR L.reviewer_task_id = ` + row + `.id)
                     AND NOT EXISTS (SELECT 1 FROM agent_task_supersessions L2
                                      WHERE L2.superseded_task_id = L.replacement_task_id))
    )
  )`
}

// execAuthRefusalFor renders the ambiguity-tree execution-authorisation
// refusal for an agent_tasks trigger. row names the task row under test
// ("NEW"). includeTerminal adds the terminal-state refusal used by the
// claim/renew guard; start/complete/cancel/handoff/verdict/approval use
// their own state predicates.
//
// The fragment refuses when an ambiguity exists in the row's tree AND the
// row is not authorised: historical, own ambiguity, or neither scoped nor
// fully named.
func execAuthRefusalFor(row string, includeTerminal bool) string {
	terminal := ""
	if includeTerminal {
		terminal = "\n OR NEW.state IN ('cancelled','awaiting_operator','operator_approved','completed','review_passed')"
	}
	return `
 AND EXISTS (SELECT 1 FROM agent_task_sessions s
               JOIN agent_tasks t ON t.id = s.task_id
              WHERE t.root_id = ` + row + `.root_id
                AND s.launch_state = 'ambiguous')` + terminal + `
 AND NOT (` + execAuthFragmentFor(row) + `
 )`
}

// execAuthRefusalSubselect renders the same refusal for a task selected by
// an outer query (the sessions INSERT guard names the task by id). The
// inner join alias is ax_task2, distinct from the caller's alias, for the
// same shadowing reason documented on execAuthFragmentFor.
func execAuthRefusalSubselect(taskAlias string) string {
	return `
  AND EXISTS (SELECT 1 FROM agent_task_sessions s
               JOIN agent_tasks ax_task2 ON ax_task2.id = s.task_id
              WHERE ax_task2.root_id = ` + taskAlias + `.root_id
                AND s.launch_state = 'ambiguous')
  AND NOT (` + execAuthFragmentFor(taskAlias) + `
  )`
}

// historicalFor renders the Historical(X) predicate: superseded itself, on
// a superseded parent chain, or the reviewer of a non-live lineage.
func historicalFor(row string) string {
	return `EXISTS (SELECT 1 FROM agent_task_supersessions L
             WHERE L.root_id = ` + row + `.root_id
               AND L.superseded_task_id = ` + row + `.id)
  OR EXISTS (WITH RECURSIVE chain(id, parent_id) AS (
               SELECT id, parent_id FROM agent_tasks WHERE id = ` + row + `.parent_id
               UNION
               SELECT t.id, t.parent_id FROM agent_tasks t
                JOIN chain c ON t.id = c.parent_id
             )
             SELECT 1 FROM agent_task_supersessions L
              WHERE L.root_id = ` + row + `.root_id
                AND L.superseded_task_id IN (SELECT id FROM chain))
  OR EXISTS (SELECT 1 FROM agent_task_supersessions L
              WHERE L.root_id = ` + row + `.root_id
                AND L.reviewer_task_id = ` + row + `.id
                AND EXISTS (SELECT 1 FROM agent_task_supersessions L2
                                 WHERE L2.superseded_task_id = L.replacement_task_id))`
}

// MigrateAgentTaskSupersessions creates the supersession lineage storage.
//
// It MUST be called inside the fatal migration transaction (db.go), for
// the same reason every other migration is: the DDL and the sentinel
// share one transaction, so a failure at any point rolls the whole thing
// back and NewDatabaseManager returns an error rather than leaving a
// half-migrated database.
//
// Idempotent via the `agent_task_supersessions_v1` sentinel.
func MigrateAgentTaskSupersessions(tx *sql.Tx) error {
	const sentinel = "agent_task_supersessions_v1"

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

	if _, err := tx.Exec(agentTaskSupersessionTableDDL); err != nil {
		return fmt.Errorf("create the supersession table: %w", err)
	}
	if _, err := tx.Exec(agentTaskSupersessionTriggerDDL); err != nil {
		return fmt.Errorf("create the supersession triggers: %w", err)
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
