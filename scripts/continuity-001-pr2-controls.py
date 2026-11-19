#!/usr/bin/env python3
"""continuity-001-pr2-controls.py — the negative controls for Continuity 001 PR 2.

WHAT THIS IS

    A negative control REMOVES one invariant from the SERVICE and re-runs
    the acceptance suite. A control that removes an invariant nobody's
    test notices is a control that says the suite does not test that
    invariant — which is worth knowing, and worth publishing.

    PR 1's controls (`scripts/continuity-001-controls.py`) mutated the
    DDL, because PR 1's invariants live in the database. PR 2's
    invariants live in the SERVICE — the fence, the delegation bounds,
    the operator's authority, the approval's action check, the review's
    independence — so PR 2's controls mutate the Go source that
    implements them.

WHY A SOURCE MUTATION AND NOT A HASH

    PR 1 pinned its DDL with a SHA-256, so a control that changed the
    hash proved only that the pin fires. It did NOT prove that any
    behaviour changed.

    PR 2 has no hash to mutate: the invariants are CODE. So a control
    here removes the code that enforces an invariant, and the suite must
    then FAIL. A failure that names the invariant is the measurement; a
    hash mismatch would not be.

HOW IT WORKS

    The mutation is applied to the WORKING TREE, the acceptance suite is
    run, and the tree is restored from a pristine copy. Nothing is
    committed and nothing is pushed.

    Two rules make the counts honest:

      * a mutation that does NOT change the file is REFUSED. A control
        that changes nothing measures nothing and reports a misleading
        zero.

      * the pristine copy is taken at the START of every run, never
        reused.

HOW TO RUN IT

    python3 scripts/continuity-001-pr2-controls.py            # every control
    python3 scripts/continuity-001-pr2-controls.py --only fence

    It needs the Go toolchain and the FTS5 build flags, and it runs
    the suite with them — the same flags `make test` uses.
"""

import argparse
import os
import re
import shutil
import contextlib
import io
import subprocess
import sys
from collections import namedtuple
import tempfile
from pathlib import Path

REPO = Path(__file__).resolve().parent.parent
CORE = REPO / "internal/core"
PKG = CORE / "assignment"
BACKUP = Path(tempfile.gettempdir()) / f"mpm-continuity-001-pr2-controls-{os.getpid()}"

# The files a control may mutate. The service is the API; the tool
# handler is the agent-facing door; the CLI is the operator's door.
FILES = (
    "internal/core/assignment/claim.go",
    "internal/core/assignment/create.go",
    "internal/core/assignment/lifecycle.go",
    "internal/core/assignment/review.go",
    "internal/core/assignment/statemachine.go",
    "internal/core/assignment/store.go",
    "internal/core/tools/handlers_assignment.go",
    "cmd/mpm/assignment_cmds.go",
    # PR 1's DDL. Some of PR 2's invariants are ALSO enforced by the
    # schema — the claim's epoch discipline is one — so a control that
    # removes the whole enforcement has to remove that half too.
    "internal/core/migration_agent_assignments.go",
)

TEST_PATTERN = "TestAPI"

# The failures that are NOT about the invariant under test. A build
# failure or a timeout is INCONCLUSIVE: it says nothing about whether
# the suite depends on the rule.
#
# PR 1's DDL pin used to be here, for the controls that mutate the
# DDL: it fails because the SOURCE changed, which is not a statement
# about the invariant under test. It has been REMOVED, because that test
# lives in `internal/core` and the `-run TestAPI` selection never runs
# it — so the entry was dead. A dead entry in an exclusion list is
# worse than no entry: it reads as "this failure is tolerated" for
# something that can never happen.
INCONCLUSIVE = {
    "<BUILD FAILED>",
    "<TIMEOUT>",
}


def snapshot():
    """Take the pristine copy from the CURRENT tree."""
    if BACKUP.exists():
        shutil.rmtree(BACKUP)
    BACKUP.mkdir(parents=True)
    for rel in FILES:
        dest = BACKUP / rel.replace("/", "__")
        shutil.copy2(REPO / rel, dest)
    print(f"the pristine copy is at {BACKUP}")


def restore():
    """Restore the mutated files from the pristine copy."""
    if not BACKUP.exists():
        raise SystemExit("there is no pristine copy; call snapshot() first")
    for rel in FILES:
        shutil.copy2(BACKUP / rel.replace("/", "__"), REPO / rel)


# A control that breaks more than ONE THIRD of the suite is not removing
# an invariant — it is breaking the build. The gate treats such a control
# as INCONCLUSIVE, and an inconclusive control FAILS the gate.
#
# This is what stops the `breaks` declaration from becoming an escape
# hatch: however broad a control's declaration is, a mutation that took
# the suite with it is refused rather than accepted.
#
# The comparison is `failures * 3 > total`: the third is the numerator
# because the bound is a FRACTION of the suite, and writing it this way
# keeps the direction obvious.
BROKEN_SUITE_FRACTION = 3


# The three patterns that read Go's `go test -v` output. They are the
# harness's entire contact with Go's output, so they are named and
# pinned rather than buried in a call.
#
# The `^` anchor is LOAD-BEARING. A subtest's result is INDENTED, so
# without the anchor a passing parent test whose subtest skipped would
# be counted as skipped — and a skipped test is, by the gate's own
# definition, a test that did not run. The identity comparison would
# then be comparing the wrong sets.
FAIL_RE = re.compile(r"^--- FAIL: (\w+)", re.M)
PASS_RE = re.compile(r"^--- PASS: (\w+)", re.M)
SKIP_RE = re.compile(r"^--- SKIP: (\w+)", re.M)

# GO_OUTPUT_SAMPLE is a sample of Go's real `go test -v` output, and it
# carries the two shapes that make the patterns' `^` anchor load-bearing:
#
#   * a PASSING parent whose subtest SKIPPED. Go reports the parent's
#     result on a top level line and the subtest's INDENTED, so
#     without the anchor the parent is counted as SKIPPED — and a
#     skipped test is a test that did not run.
#   * a FAILING parent whose subtest PASSED. Go reports the parent as
#     `--- FAIL:` on a top level line and the passing subtest's result
#     INDENTED, so without the anchor the parent is counted as PASSED
#     — and it FAILED.
#
# The second shape is the one that was missing: without it the PASS
# pattern's anchor looks inert, because a passing parent's own line
# already carries the name.
GO_OUTPUT_SAMPLE = (
    "=== RUN   TestAPIAlpha\n"
    "--- PASS: TestAPIAlpha (0.00s)\n"
    "=== RUN   TestAPIWithSkip\n"
    "=== RUN   TestAPIWithSkip/alpha\n"
    "    --- SKIP: TestAPIWithSkip/alpha (0.00s)\n"
    "--- PASS: TestAPIWithSkip (0.00s)\n"
    "=== RUN   TestAPIWithFailure\n"
    "=== RUN   TestAPIWithFailure/beta\n"
    "    --- PASS: TestAPIWithFailure/beta (0.00s)\n"
    "--- FAIL: TestAPIWithFailure (0.00s)\n"
    "--- SKIP: TestAPISkipped (0.00s)\n"
    "--- FAIL: TestAPIBroke (0.00s)\n"
)


def run_tests():
    """Run the acceptance suite and report what it did.

    The return value is (failed, detail, passed, skipped).

    `skipped` is the LIST OF NAMES, not a count. A skipped test did not
    run: it neither passed nor failed, so counting only PASS-or-FAIL
    makes a skip invisible to both sides of the bound — and a control
    whose mutation made a test skip has stopped the suite from saying
    anything about the invariant. The NAMES matter because a count
    cannot tell "the same test skipped" from "a different test
    skipped", and only the first is innocent.
    """
    env = dict(os.environ)
    env["CGO_CFLAGS"] = "-DSQLITE_ENABLE_FTS5=1"
    env["CGO_LDFLAGS"] = "-lm"
    try:
        p = subprocess.run(
            ["go", "test", "-tags", "fts5", "-count=1", "-v",
             "-run", TEST_PATTERN, "./assignment/..."],
            cwd=CORE, capture_output=True, text=True, env=env, timeout=3600,
        )
    except subprocess.TimeoutExpired:
        return ["<TIMEOUT>"], [], 0, []
    failed = sorted(set(FAIL_RE.findall(p.stdout)))
    passed = len(set(PASS_RE.findall(p.stdout)))
    skipped = sorted(set(SKIP_RE.findall(p.stdout)))
    if p.returncode != 0 and not failed:
        err = (p.stdout + p.stderr).strip().splitlines()
        return ["<BUILD FAILED>"], err[-3:], passed, skipped
    return failed, [], passed, skipped


def replace_text(rel, old, new, what):
    """Replace one block of source text, and PROVE that it changed."""
    # A control may only touch a file the harness backs up. A mutation
    # outside FILES would be left behind in the operator's tree when the
    # harness restored itself — a green run over a broken tree.
    if rel not in FILES:
        raise SystemExit(
            f"the control for {what} would mutate {rel}, which is not in "
            f"the harness's FILES tuple — the tree would not be restored"
        )
    path = REPO / rel
    t = path.read_text()
    if t.count(old) != 1:
        raise SystemExit(
            f"the control for {what} could not find its target in {rel} "
            f"({t.count(old)} matches, expected exactly 1)"
        )
    after = t.replace(old, new)
    if after == t:
        raise SystemExit(
            f"the control for {what} changed NOTHING — the mutation is a "
            "no-op, so the control would report a misleading zero"
        )
    path.write_text(after)


# ── the controls ──────────────────────────────────────────────────────────
#
# Each control names the invariant it removes and the test that must
# then fail.
#
# A control removes the WHOLE enforcement of its invariant. Some
# invariants are enforced in more than one place — the fence is enforced
# by a Go check AND by the WHERE clause of the writing statement, and
# the idempotency is enforced by an early lookup AND by the schema's
# unique index AND by the read-back on the losing insert. A control that
# removes only HALF of such an enforcement measures nothing, because the
# other half still refuses the write — and reporting that as "the suite
# does not depend on this invariant" would be a false conclusion.
#
# So there are two kinds of control:
#
#   * the FULL removal, which must make the named test fail; and
#   * the HALF removal, which must NOT make it fail — and which is
#     published as the measurement of the defence in depth.
#
# The `load_bearing` flag says which kind each control is, and the
# summary counts them separately.


def control_fence():
    """N1: the fence. Both halves of it: the check and the WHERE clause."""
    replace_text(
        "internal/core/assignment/lifecycle.go",
        """		if spec.presentedEpoch != current.ClaimEpoch {
			return fmt.Errorf("%w: %s is at epoch %d", ErrStaleClaim, spec.id, current.ClaimEpoch)
		}""",
        """		// THE GO HALF OF THE FENCE REMOVED BY THE CONTROL.""",
        "the fence's Go check",
    )
    replace_text(
        "internal/core/assignment/lifecycle.go",
        """			`UPDATE agent_tasks SET state = ?, updated_at = ?`+setClause+
				` WHERE id = ? AND claim_epoch = ?`,""",
        """			`UPDATE agent_tasks SET state = ?, updated_at = ?`+setClause+
				` WHERE id = ?`,""",
        "the fence's WHERE clause",
    )
    # The value the predicate binds goes with the predicate. Leaving it
    # in place would make the statement a bind error, and every
    # successful transition in the suite would fail for a reason that
    # has nothing to do with the invariant under test.
    replace_text(
        "internal/core/assignment/lifecycle.go",
        """		args = append(args, spec.id, spec.presentedEpoch)""",
        """		args = append(args, spec.id)""",
        "the fence's bound epoch",
    )


def control_fence_go_half():
    """N1: the Go half of the fence alone."""
    replace_text(
        "internal/core/assignment/lifecycle.go",
        """		if spec.presentedEpoch != current.ClaimEpoch {
			return fmt.Errorf("%w: %s is at epoch %d", ErrStaleClaim, spec.id, current.ClaimEpoch)
		}""",
        """		// THE GO HALF OF THE FENCE REMOVED BY THE CONTROL.""",
        "the fence's Go check",
    )


def control_fence_sql_half():
    """N1: the WHERE clause alone."""
    replace_text(
        "internal/core/assignment/lifecycle.go",
        """			`UPDATE agent_tasks SET state = ?, updated_at = ?`+setClause+
				` WHERE id = ? AND claim_epoch = ?`,""",
        """			`UPDATE agent_tasks SET state = ?, updated_at = ?`+setClause+
				` WHERE id = ?`,""",
        "the fence's WHERE clause",
    )
    replace_text(
        "internal/core/assignment/lifecycle.go",
        """		args = append(args, spec.id, spec.presentedEpoch)""",
        """		args = append(args, spec.id)""",
        "the fence's bound epoch",
    )


def control_fence_in_review():
    """N1 again, on the reviewer's verdict path.

    The review path carries the fence in THREE places: the check in
    RecordReviewResult, the check in transitionTx, and the WHERE clause
    of the writing statement. All three are removed, because a control
    that removes one of them measures nothing — the other two still
    refuse the stale verdict.
    """
    replace_text(
        "internal/core/assignment/review.go",
        """		if p.Epoch != reviewer.ClaimEpoch {
			return fmt.Errorf("%w: %s is at epoch %d", ErrStaleClaim, reviewer.ID, reviewer.ClaimEpoch)
		}""",
        """		// THE GO HALF OF THE FENCE REMOVED BY THE CONTROL.""",
        "the fence's Go check on the review path",
    )
    replace_text(
        "internal/core/assignment/review.go",
        """	if spec.presentedEpoch != current.ClaimEpoch {
		return fmt.Errorf("%w: %s is at epoch %d", ErrStaleClaim, spec.id, current.ClaimEpoch)
	}""",
        """	// THE GO HALF OF THE FENCE, IN THE TRANSITION, REMOVED BY THE CONTROL.""",
        "the fence's Go check in the transition",
    )
    replace_text(
        "internal/core/assignment/review.go",
        """		`UPDATE agent_tasks SET state = ?, updated_at = ?`+setClause+
			` WHERE id = ? AND claim_epoch = ?`,""",
        """		`UPDATE agent_tasks SET state = ?, updated_at = ?`+setClause+
			` WHERE id = ?`,""",
        "the fence's WHERE clause on the review path",
    )
    replace_text(
        "internal/core/assignment/review.go",
        """	args = append(args, spec.id, spec.presentedEpoch)""",
        """	args = append(args, spec.id)""",
        "the fence's bound epoch on the review path",
    )


def control_recipient_allow_list():
    """N2: the child's recipient must be among the ROOT's."""
    replace_text(
        "internal/core/assignment/create.go",
        """		// N2: the child's recipient is among the ROOT's allow-list.
		if !containsString(root.EligibleRecipients, p.Recipient) {
			return fmt.Errorf(
				"%w: %q is not among the root's eligible recipients", ErrConstraint, p.Recipient)
		}""",
        """		// N2 REMOVED BY THE CONTROL.
		_ = root""",
        "the recipient allow-list",
    )


def control_depth_budget():
    """N3: the child's depth must be within the ROOT's budget."""
    replace_text(
        "internal/core/assignment/create.go",
        """		// N3: the child's depth is within the ROOT's budget.
		childDepth := int64(parent.Depth) + 1
		if childDepth > root.MaxDepth {
			return fmt.Errorf(
				"%w: the child would be at depth %d and the root allows %d",
				ErrConstraint, childDepth, root.MaxDepth)
		}""",
        """		// N3 REMOVED BY THE CONTROL.
		childDepth := int64(parent.Depth) + 1
		_ = root""",
        "the depth budget",
    )


def control_operator_authority():
    """N6: the root is created only through the operator's authority."""
    replace_text(
        "internal/core/assignment/create.go",
        """	if auth == nil {
		return nil, false, fmt.Errorf("%w: root creation is an operator operation", ErrNotAuthorised)
	}""",
        """	if auth == nil {
		// N6 REMOVED BY THE CONTROL.
		auth = NewLocalOperatorAuthority("operator")
	}""",
        "the operator's authority",
    )


def control_approval_fence():
    """The approval presents the epoch the operator read.

    The approval is the operator's gate, and it is a fenced write like
    every other state change: the operator presents the claim epoch it
    read, and the write is `WHERE id = ? AND claim_epoch = ?` with the
    PRESENTED epoch.

    Both halves are removed — the check and the value the predicate
    binds — because removing only one of them measures nothing: the
    other still refuses the stale approval.
    """
    replace_text(
        "internal/core/assignment/lifecycle.go",
        """		if p.Epoch != current.ClaimEpoch {
			return fmt.Errorf("%w: %s is at epoch %d", ErrStaleClaim, p.ID, current.ClaimEpoch)
		}""",
        """		// THE APPROVAL'S FENCE REMOVED BY THE CONTROL.""",
        "the approval's fence",
    )
    replace_text(
        "internal/core/assignment/lifecycle.go",
        """			p.ID, p.Epoch)""",
        """			p.ID, current.ClaimEpoch)""",
        "the approval's bound epoch",
    )


def control_approval_action_check():
    """The approval must name the action the gate awaited."""
    replace_text(
        "internal/core/assignment/lifecycle.go",
        """		if current.AwaitingAction != p.Action {
			return fmt.Errorf(
				"%w: the gate awaits %q and the approval names %q",
				ErrConstraint, current.AwaitingAction, p.Action)
		}""",
        """		// THE ACTION CHECK REMOVED BY THE CONTROL.""",
        "the approval's action check",
    )


def control_reviewer_independence():
    """The reviewer must not judge work it delegated."""
    replace_text(
        "internal/core/assignment/review.go",
        """		ancestor, err := isAncestorOf(node, reviewer, subject.ID)
		if err != nil {
			return err
		}
		if ancestor {
			return fmt.Errorf(
				"%w: %s delegated %s, so it does not review its own work",
				ErrConstraint, reviewer.ParentID, reviewer.ID)
		}""",
        """		// THE INDEPENDENCE CHECK REMOVED BY THE CONTROL.""",
        "the reviewer's independence",
    )


def control_live_lease_is_stealable():
    """A live lease must not be stealable."""
    replace_text(
        "internal/core/assignment/claim.go",
        """			alive := current.LeaseExpiresAt != nil && *current.LeaseExpiresAt > now
			if alive {
				if current.Owner == p.Owner {
					return fmt.Errorf(
						"%w: %s is leased to %s until %d — renew it rather than claiming it",
						ErrAlreadyClaimed, p.ID, current.Owner, *current.LeaseExpiresAt)
				}
				return fmt.Errorf(
					"%w: %s is leased to %s until %d",
					ErrLeaseHeld, p.ID, current.Owner, *current.LeaseExpiresAt)
			}""",
        """			// THE LIVE-LEASE REFUSAL REMOVED BY THE CONTROL.""",
        "the live-lease refusal",
    )


def drop_trigger(name):
    """Remove one trigger from PR 1's DDL."""
    import re

    rel = "internal/core/migration_agent_assignments.go"
    if rel not in FILES:
        raise SystemExit(
            f"the control for {name} would mutate {rel}, which is not in "
            f"the harness's FILES tuple — the tree would not be restored"
        )
    path = REPO / rel
    t = path.read_text()
    pattern = r"CREATE TRIGGER IF NOT EXISTS " + re.escape(name) + r"\b.*?\nEND;\n"
    new, n = re.subn(pattern, "", t, flags=re.S)
    if n != 1:
        raise SystemExit(f"the control could not find the trigger {name} ({n} matches)")
    if new == t:
        raise SystemExit(f"the control for {name} changed NOTHING")
    path.write_text(new)


def control_claim_is_not_a_cas():
    """The claim must be a compare-and-swap.

    The CAS is enforced in THREE places: the Go check, the WHERE clause
    of the writing statement, and PR 1's epoch-discipline trigger. All
    three are removed, because a control that removes one of them
    measures nothing — the other two still refuse the second claim.
    """
    replace_text(
        "internal/core/assignment/claim.go",
        """		if p.ExpectedEpoch != current.ClaimEpoch {
			return fmt.Errorf("%w: %s is at epoch %d", ErrStaleClaim, p.ID, current.ClaimEpoch)
		}
		switch current.State {""",
        """		// THE GO HALF OF THE COMPARE-AND-SWAP REMOVED BY THE CONTROL.
		switch current.State {""",
        "the claim's Go half",
    )
    replace_text(
        "internal/core/assignment/claim.go",
        """			SET claim_epoch = ?, owner = ?, lease_expires_at = ?, updated_at = ?
			WHERE id = ? AND claim_epoch = ?`,""",
        """			SET claim_epoch = ?, owner = ?, lease_expires_at = ?, updated_at = ?
			WHERE id = ?`,""",
        "the claim's WHERE clause",
    )
    replace_text(
        "internal/core/assignment/claim.go",
        """			5, newEpoch, p.Owner, newLease, now, p.ID, p.ExpectedEpoch)""",
        """			5, newEpoch, p.Owner, newLease, now, p.ID)""",
        "the claim's bound epoch",
    )
    drop_trigger("trg_agent_tasks_claim_discipline")


def control_idempotency_lookup():
    """The early lookup that resolves a repeated key."""
    replace_text(
        "internal/core/assignment/create.go",
        """		existing, err := s.findExistingTx(node, p.IdempotencyKey)
		if err != nil {
			return err
		}
		if existing != nil {
			if !sameLogicalAssignment(existing, rootSpec{
				projectID: p.ProjectID, title: p.Title, objective: p.Objective,
				recipient: p.Recipient, role: role,
			}) {""",
        """		existing, err := s.findExistingTx(node, p.IdempotencyKey)
		if err != nil {
			return err
		}
		if false && existing != nil {
			if !sameLogicalAssignment(existing, rootSpec{
				projectID: p.ProjectID, title: p.Title, objective: p.Objective,
				recipient: p.Recipient, role: role,
			}) {""",
        "the idempotency lookup",
    )


def control_idempotency_readback():
    """The read-back that resolves the insert the unique index refused."""
    replace_text(
        "internal/core/assignment/create.go",
        """			if strings.Contains(err.Error(), "never reused") {
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

		if err := s.injectFault("create-root-event"); err != nil {""",
        """			// THE READ-BACK REMOVED BY THE CONTROL.
			return storageRefusal(err)
		}

		if err := s.injectFault("create-root-event"); err != nil {""",
        "the idempotency read-back",
    )


def control_state_machine():
    """The role's path must be enforced before the write."""
    replace_text(
        "internal/core/assignment/statemachine.go",
        """	for _, next := range path[from] {
		if next == to {
			return true
		}
	}
	return false""",
        """	// THE PATH REMOVED BY THE CONTROL.
	_ = path
	return true""",
        "the role's path",
    )


def control_the_event_is_not_atomic_with_the_transition():
    """The transition and its ledger entry must share a transaction."""
    replace_text(
        "internal/core/assignment/lifecycle.go",
        """		if err := s.injectFault("transition-event"); err != nil {
			return err
		}""",
        """		// THE FAULT SEAM REMOVED BY THE CONTROL, so the injected
		// failure between the two writes no longer aborts.""",
        "the atomicity of the transition and its event",
    )


CONTROLS = [
    {
        "name": "fence",
        "invariant": "N1 — a claimed write presents the current claim_epoch",
        "mutate": control_fence,
        "expect": ["TestAPIAnExpiredLeaseIsReclaimableAndThePreviousHolderIsFencedOut"],
        # The renewal test depends on the transition's fence, but not
        # on the renewal's own compare-and-swap: `Renew` has its own
        # CAS in claim.go, which this control does not touch, so the
        # renewal still advances the epoch. What breaks is the test's
        # LAST step — a `Start` presenting the PRE-renewal epoch, which
        # the transition's fence is what refuses.
        "breaks": ["TestAPIRenewalAdvancesTheEpoch"],
        "load_bearing": True,
    },
    {
        "name": "fence-go-half",
        "invariant": "N1 — the Go half of the fence, alone",
        "mutate": control_fence_go_half,
        "expect": [],
        "load_bearing": False,
        "why": "the WHERE clause still fences the write, so removing the Go half alone changes nothing",
    },
    {
        "name": "fence-sql-half",
        "invariant": "N1 — the WHERE clause, alone",
        "mutate": control_fence_sql_half,
        "expect": [],
        "load_bearing": False,
        "why": "the Go check still refuses the stale epoch, so removing the WHERE clause alone changes nothing",
    },
    {
        "name": "fence-review",
        "invariant": "N1 — the fence on the reviewer's verdict path",
        "mutate": control_fence_in_review,
        "expect": ["TestAPIAStaleReviewerIsFencedOut"],
        "load_bearing": True,
    },
    {
        "name": "recipient-allow-list",
        "invariant": "N2 — the child's recipient is among the ROOT's eligible recipients",
        "mutate": control_recipient_allow_list,
        "expect": ["TestAPIChildOutsideTheRootAllowListIsRefused"],
        "load_bearing": True,
    },
    {
        "name": "depth-budget",
        "invariant": "N3 — the child's depth is within the ROOT's max_depth",
        "mutate": control_depth_budget,
        "expect": ["TestAPIChildBeyondTheRootDepthIsRefused"],
        "load_bearing": True,
    },
    {
        "name": "operator-authority",
        "invariant": "N6 — the root is created only through the operator's authority",
        "mutate": control_operator_authority,
        "expect": ["TestAPIRootCreationRequiresTheOperatorAuthority"],
        "load_bearing": True,
    },
    {
        "name": "approval-fence",
        "invariant": "the approval presents the epoch the operator read",
        "mutate": control_approval_fence,
        "expect": ["TestAPIAnApprovalPresentingAStaleEpochIsRefused"],
        "load_bearing": True,
    },
    {
        "name": "approval-action",
        "invariant": "the approval names the action the gate awaited",
        "mutate": control_approval_action_check,
        "expect": ["TestAPITheApprovalMustNameTheGateAction"],
        "load_bearing": True,
    },
    {
        "name": "reviewer-independence",
        "invariant": "the reviewer does not judge work it delegated",
        "mutate": control_reviewer_independence,
        "expect": ["TestAPIReviewerCannotReviewItsOwnWork"],
        "load_bearing": True,
    },
    {
        "name": "live-lease",
        "invariant": "a live lease is not stealable",
        "mutate": control_live_lease_is_stealable,
        "expect": ["TestAPIALiveLeaseIsNotStealable"],
        "load_bearing": True,
    },
    {
        "name": "claim-cas",
        "invariant": "the claim is a compare-and-swap",
        "mutate": control_claim_is_not_a_cas,
        "expect": ["TestAPIClaimPresentingAStaleEpochIsRefused"],
        "load_bearing": True,
    },
    {
        "name": "idempotency-lookup",
        "invariant": "the early lookup that resolves a repeated key to its existing assignment",
        "mutate": control_idempotency_lookup,
        "expect": ["TestAPIDuplicateCreationWithADifferentAssignmentIsAConflict"],
        "load_bearing": True,
    },
    {
        "name": "idempotency-readback",
        "invariant": "the read-back that resolves the insert the unique index refused",
        "mutate": control_idempotency_readback,
        "expect": ["TestAPITheDuplicateIsResolvedWhenTheLookupCannotSeeItYet"],
        "load_bearing": True,
    },
    {
        "name": "state-machine",
        "invariant": "the role's path is enforced before the write",
        "mutate": control_state_machine,
        "expect": ["TestAPIDeliveryAcknowledgementAcceptanceAndCompletionAreDistinct"],
        # The path IS the state machine, so removing it breaks every
        # test that depends on a transition being refused: the
        # undelivered acknowledgement, the invalid transitions, the
        # redelivery, and the approval's gate.
        "breaks": [
            "TestAPIAnUndeliveredAssignmentCannotBeAcknowledged",
            "TestAPIInvalidTransitionsAreRefused",
            "TestAPIRedeliveryIsRefusedAndTheWorkStaysVisible",
            "TestAPITheApprovalMustNameTheGateAction",
        ],
        "load_bearing": True,
    },
    {
        "name": "atomicity",
        "invariant": "the transition and its ledger entry share a transaction",
        "mutate": control_the_event_is_not_atomic_with_the_transition,
        "expect": ["TestAPIAFailedTransitionRollsBackItsEvent"],
        "load_bearing": True,
    },
]


# baseline_verdict decides whether a baseline run can be the
# denominator, and returns the denominator when it can.
#
# It is a FUNCTION rather than inline code in main() so that the
# self-test can drive it. Two guards live here — the suite must be
# GREEN, and it must not have SKIPPED anything — and an inline guard has
# no case that fails when it is removed: the self-test drives
# summarise(), which main() never reaches.
#
# A skipped test did not run, so it cannot be part of the denominator —
# and because the total counts a skip as if it had run, a control whose
# run skipped the same tests would match a skipping baseline EXACTLY
# and be measured against a baseline that never measured anything.
#
# IT RAISES rather than returning a verdict. Returning made main()'s
# USE of it unpinnable: a caller can forget to look at a return value,
# but it cannot ignore an exception. A regression that made main()
# proceed on a not-green baseline would otherwise print a green gate
# and exit 0, and nothing would catch it.
def baseline_verdict(base_failed, base_passed, base_skipped, detail=None):
    if base_failed:
        print(f"THE SUITE IS NOT GREEN BEFORE THE CONTROLS RUN: {base_failed}")
        if detail:
            print(f"    {detail}")
        raise SystemExit(1)
    if base_skipped:
        print(f"THE BASELINE SKIPPED {len(base_skipped)} TESTS, so it cannot "
              f"be the denominator: {base_skipped}")
        if detail:
            print(f"    {detail}")
        raise SystemExit(1)
    # The denominator is the number of tests the suite RAN AND PASSED.
    # The two refusals above mean a baseline that failed or skipped
    # never reaches this line, so the failed and skipped terms would
    # be zero — and a term that can never be non-zero is a term
    # nothing can pin.
    return base_passed


# Result is one control's measurement. It is a named tuple because the
# fields have to travel together and a bare tuple is easy to mis-read.
#
#   total    the number of tests the control's run produced
#   skipped  the NAMES of those that did not run at all
Result = namedtuple("Result", "control hit specific inconclusive total skipped")


def main():
    ap = argparse.ArgumentParser(description=__doc__)
    ap.add_argument("--only", help="run one control by name")
    args = ap.parse_args()
    if args.only and args.only not in {c["name"] for c in CONTROLS}:
        raise SystemExit(f"there is no control named {args.only!r}")

    snapshot()
    results = []
    baseline = 0
    baseline_skipped = 0
    try:
        # THE BASELINE. The suite must be green before any control runs,
        # and the number of tests it runs is the DENOMINATOR of the
        # bound.
        #
        # It has to be the baseline rather than "the tests that ran" in
        # the control's own run: a mutation that ABORTS the suite — a
        # panic turns a guard into a nil dereference, and the test
        # binary dies — shrinks both sides of the comparison, so the
        # bound would get weaker at the moment it must be strongest.
        # A control whose run produced FEWER tests than the baseline
        # aborted, and its measurement is inconclusive.
        restore()
        print("\n=== the baseline ===")
        base_failed, base_detail, base_passed, base_skipped = run_tests()
        # The verdict RAISES on a baseline that cannot be the
        # denominator, so there is no return value to forget to check.
        baseline = baseline_verdict(base_failed, base_passed, base_skipped,
                                    base_detail)
        baseline_skipped = set(base_skipped)
        print(f"the suite runs {baseline} tests and is green "
              f"({len(base_skipped)} of them skipped)")

        for control in CONTROLS:
            name = control["name"]
            if args.only and args.only != name:
                continue
            restore()
            print(f"\n=== {name}: {control['invariant']} ===")
            control["mutate"]()
            failed, detail, passed, skipped = run_tests()
            inconclusive = [f for f in failed if f in INCONCLUSIVE]
            specific = [f for f in failed if f not in INCONCLUSIVE]
            hit = [t for t in control["expect"] if t in specific]
            # The total counts every test the run PRODUCED — the ones
            # that passed, the ones that failed, and the ones that
            # skipped. A skip counts towards the total because the
            # test was selected and did not run, which is what the
            # comparison against the baseline is watching for.
            total = len(specific) + passed + len(skipped)
            print(f"    failing tests: {specific or '(none)'}")
            if detail:
                print(f"    detail: {detail}")
            if control["load_bearing"] and len(hit) != len(control["expect"]):
                print(f"    NOT LOAD-BEARING: {control['expect']} did not fail")
            if not control["load_bearing"] and specific:
                print(f"    UNEXPECTED: {specific} failed, but this control removes only one half")
            results.append(Result(control, hit, specific, inconclusive,
                                  total, skipped))
    finally:
        restore()
        print("\nthe tree is restored")

    ok = summarise(results, baseline, baseline_skipped)
    return 0 if ok else 1


def summarise(results, baseline, baseline_skipped=()):
    """Print the summary and return whether the measurement holds.

    `baseline` is the number of tests the suite runs when nothing is
    mutated. It is the denominator of the bound, and it is what makes
    an ABORTED run visible: a control whose run produced fewer tests
    than the baseline did not measure an invariant, it stopped the
    suite.

    IT IS REQUIRED. An earlier version defaulted it to 0, which
    silently restored the very bound the baseline was introduced to
    fix — a caller that forgot the baseline got the vulnerable
    measurement and a green exit code. A missing baseline now fails
    CLOSED: no denominator, no measurement.

    The RETURN VALUE is the gate. It can fail, and it does.
    """
    print("\n=== the summary ===")
    if not baseline or baseline <= 0:
        print("THE HARNESS FAILED: there is no baseline, so the bound has no "
              "denominator. A measurement without a baseline is not a "
              "measurement, and it is refused rather than assumed.")
        return False
    load_bearing_passed = 0
    load_bearing_total = 0
    redundant_total = 0
    redundant_clean = 0
    for result in results:
        control, hit, specific, inconclusive = (
            result.control, result.hit, result.specific, result.inconclusive)
        total, skipped = result.total, result.skipped
        if control["load_bearing"]:
            load_bearing_total += 1
            named = len(hit) == len(control["expect"])
            # THE BOUND. A mutation that broke more than a third of the
            # BASELINE did not remove one invariant — it broke the
            # build, and the named test may have failed for that reason
            # instead. Such a control is INCONCLUSIVE, and an
            # inconclusive control does not count as load-bearing.
            #
            # The comparison is TWO-SIDED. A run that produced FEWER
            # tests than the baseline ABORTED the suite; a run that
            # produced MORE is not a run of this suite, and neither
            # reads as a measurement.
            denominator = baseline
            # The skip comparison is by IDENTITY, not by count: only
            # the SAME test skipping is innocent.
            broke_suite = (total != denominator or
                           set(skipped) != set(baseline_skipped) or
                           len(specific) * BROKEN_SUITE_FRACTION > denominator)
            if broke_suite:
                if total < denominator:
                    print(f"     INCONCLUSIVE: the run produced {total} tests and the "
                          f"baseline runs {denominator} — the mutation ABORTED the "
                          f"suite, so the named test may have failed for that reason "
                          f"instead")
                elif total > denominator:
                    print(f"     INCONCLUSIVE: the run produced {total} tests and the "
                          f"baseline runs {denominator} — that is NOT a run of this "
                          f"suite, so it measures nothing")
                elif set(skipped) != set(baseline_skipped):
                    print(f"     INCONCLUSIVE: the run skipped {skipped} and the "
                          f"baseline skipped {sorted(baseline_skipped)} — a test that "
                          f"does not run says nothing about the invariant")
                else:
                    print(f"     INCONCLUSIVE: the mutation broke {len(specific)} of "
                          f"the baseline's {denominator} tests — that is a broken build, "
                          f"not a removed invariant")
            # The gate asks a second question: did the mutation break
            # anything the control did not DECLARE it would break? A
            # mutation that broke the statement would fail the named
            # test for the wrong reason and take the rest of the suite
            # with it — and that must not read as a pass.
            unexpected = [f for f in specific
                          if f not in control["expect"] and f not in control.get("breaks", [])]
            ok = named and not unexpected and not broke_suite
            if ok:
                load_bearing_passed += 1
            print(f"{'OK ' if ok else 'NO '} {control['name']:22s} {control['invariant']}")
            if not named:
                print(f"     expected {control['expect']}, failing {specific}")
            if unexpected:
                print(f"     UNEXPECTED failures: {unexpected} — the control did not "
                      f"declare that its removal breaks them, so the named test may have "
                      f"failed for the wrong reason")
            # The exemptions are PUBLISHED, so a broad declaration is
            # visible in the measurement rather than hidden in this file.
            exemptions = control.get("breaks", [])
            if exemptions:
                print(f"     declared exemptions: {exemptions}")
        else:
            redundant_total += 1
            exemptions = control.get("breaks", [])
            if exemptions:
                print(f"     declared exemptions: {exemptions}")
            # A defence-in-depth control removes ONE half of an
            # enforcement, so the suite must be UNCHANGED. An
            # unexpected failure means the mutation broke more than
            # it claimed — a broken measurement, not a pass. A run that
            # produced a different number of tests, or skipped more,
            # is a broken measurement for the same reason.
            clean = (not specific and total == baseline
                     and set(skipped) == set(baseline_skipped))
            if clean:
                redundant_clean += 1
            print(f"{'OK ' if clean else 'NO '} {control['name']:22s} {control['invariant']}")
            print(f"     (redundant: {control['why']})")
            if not clean:
                if specific:
                    print(f"     UNEXPECTED failures: {specific}")
                else:
                    print(f"     INCONCLUSIVE: the run produced {total} tests against "
                          f"a baseline of {baseline} ({skipped} skipped against "
                          f"{sorted(baseline_skipped)}) — the suite must be UNCHANGED "
                          f"by this control")
    print(f"\n{load_bearing_passed}/{load_bearing_total} controls are load-bearing; "
          f"{redundant_total} measure the defence in depth")
    ok = (load_bearing_passed == load_bearing_total
          and redundant_clean == redundant_total)
    if not ok:
        print("THE HARNESS FAILED: the measurement is not established.")
    return ok


# SUITE_SIZE is the number of tests the `-run TestAPI` selection runs.
# The synthetic results only need it to compute the broken-suite bound.
SUITE_SIZE = 36


def _synthetic_results(broken_load_bearing=None, broken_redundant=None,
                       undeclared=False, broken_suite=False, aborted=False,
                       skipped=False, more_tests=False, redundant_aborted=False,
                       detected_nothing=False, redundant_skipped=False,
                       swapped_skip=False, redundant_swapped_skip=False,
                       baseline_skips=None):
    """Build the results a run would produce, for the self-test.

    `broken_load_bearing` names a load-bearing control whose named test
    did NOT fail; `broken_redundant` names a defence-in-depth control
    that DID break the suite; `undeclared` adds a failure the control
    did not declare it would break — the shape a mutation that broke
    the STATEMENT produces, where the named test fails for the wrong
    reason and takes the rest of the suite with it.
    """
    first_lb = next(c["name"] for c in CONTROLS if c["load_bearing"])
    first_rd = next(c["name"] for c in CONTROLS if not c["load_bearing"])
    # The tests the BASELINE skipped. A control that skips exactly
    # these is innocent — only the SAME test skipping is — so the
    # defence-in-depth controls carry them and stay clean.
    baseline_skips = list(baseline_skips or [])
    results = []
    for control in CONTROLS:
        if control["load_bearing"]:
            hit = list(control["expect"])
            specific = []
            if control["name"] == broken_load_bearing:
                hit = []
                specific = ["TestTheSuiteDidNotNotice"]
            if undeclared and control["name"] == first_lb:
                # The named test DID fail, and something else broke
                # too — and the control never said it would.
                specific = list(hit) + ["TestSomethingElseBroke"]
            if broken_suite and control["name"] == first_lb:
                # The EXACT attack the review described: the mutation
                # took the suite with it, AND the control DECLARES
                # every test it broke. The declaration is broad enough
                # to swallow anything — so the bound is what refuses
                # it, not the declaration.
                broken = [f"TestBroke{i}" for i in range(SUITE_SIZE)]
                specific = list(hit) + broken
                control = dict(control)
                control["breaks"] = list(control.get("breaks", [])) + broken
            total = SUITE_SIZE
            # EVERY control skips what the BASELINE skipped, unless the
            # case puts it under test. That matters: the identity
            # comparison refuses a control whose skip set differs from
            # the baseline's, so a case that changes ONE control's skips
            # must leave the others matching — otherwise every other
            # control is refused for the same reason and the case
            # proves nothing about the one under test.
            run_skipped = list(baseline_skips)
            if detected_nothing and control["name"] == first_lb:
                # The mutation stopped breaking the named test AND
                # broke nothing else. The `named` clause is the FIRST
                # of the gate's three questions, and it must refuse
                # this on its own: the "unexpected failures" clause
                # cannot, because nothing unexpected happened.
                hit = []
                specific = []
            if swapped_skip and control["name"] == first_lb:
                # The run skipped a DIFFERENT test than the baseline
                # did — the same COUNT, a different identity. The
                # comparison is by identity, so this is refused; a
                # count comparison would wave it through.
                run_skipped = ["TestSomethingElse"]
            if skipped and control["name"] == first_lb:
                # The mutation made a test SKIP. A skipped test did not
                # run: it neither passed nor failed, so it says
                # nothing about the invariant — and the baseline did
                # not skip.
                run_skipped = ["TestSomethingSkipped"]
            if more_tests and control["name"] == first_lb:
                # The run produced MORE tests than the baseline. That
                # is not a run of this suite, and the comparison is
                # two-sided, so it is refused.
                total = SUITE_SIZE + 4
            if aborted and control["name"] == first_lb:
                # The mutation PANICKED, and the test binary died: the
                # run produced three tests and the other thirty-three
                # never ran. The named test failed, and the gate must
                # still refuse it.
                specific = list(hit)
                total = 3
            results.append(Result(control, hit, specific, [], total, run_skipped))
        else:
            specific = []
            if control["name"] == broken_redundant:
                specific = ["TestTheSuiteBroke"]
            redundant_total = SUITE_SIZE
            run_redundant_skipped = list(baseline_skips)
            if redundant_swapped_skip and control["name"] == first_rd:
                # The defence-in-depth control's run skipped a
                # DIFFERENT test than the baseline did — the same
                # count, a different identity.
                run_redundant_skipped = ["TestSomethingElse"]
            if redundant_skipped and control["name"] == first_rd:
                # The defence-in-depth control's run SKIPPED a test.
                # Nothing failed, so the `not specific` clause cannot
                # catch it — the skip clause is what must.
                #
                # The baseline's skips are NOT changed here: this
                # variable is what every control's skip set is derived
                # from, and assigning to it inside the loop would leak
                # the change into every control after this one — so
                # the case would refuse thirteen bystanders for the
                # wrong reason and pin nothing.
                run_redundant_skipped = ["TestSomethingSkipped"]
            if redundant_aborted and control["name"] == first_rd:
                # The defence-in-depth control's run ABORTED: it
                # produced three tests and the rest never ran. The
                # suite must be UNCHANGED by such a control, so a
                # different number of tests is a broken measurement.
                redundant_total = 3
            results.append(Result(control, [], specific, [], redundant_total,
                                  run_redundant_skipped))
    return results


def _equal(got, want, calls):
    """Whether two values are equal.

    Every VALUE comparison in the self-test goes through here, so that
    each one can be driven with a deliberately wrong value and proved
    able to fail. A check that is inlined cannot be driven, so an
    inlined check that is neutered leaves the self-test green with
    nothing to notice it.

    It also RECORDS that it ran, in the log the caller owns. That is
    what makes a neutered CALL SITE visible: a check that stops
    running leaves the log short, and the count — which is the log's
    length — changes.
    """
    calls.append("equal")
    return got == want


def _message_ok(output, calls):
    """Whether the more-tests direction says what is actually wrong."""
    calls.append("message")
    return "NOT a run of this suite" in output


def _isolation_ok(refused, calls):
    """Whether a case refused exactly the one control it is about."""
    calls.append("isolation")
    return refused == 1


def _sample_ok(needle, sample, calls):
    """Whether the sample carries the shape the pattern needs."""
    calls.append("sample")
    return needle in sample


def _wiring_ok(proceeded, exited, ran, want_proceeded, want_exit, calls):
    """The verdict on whether main()'s wiring behaved.

    It is a FUNCTION so that the self-test can prove that it CAN fail:
    a verdict that is inlined cannot be driven with a deliberately
    wrong expectation, and so a neutered inlined verdict would leave
    the self-test green with no way to notice.
    """
    calls.append("wiring")
    # The three are evaluated SEPARATELY, not as one `and` chain: a
    # short-circuiting chain would log a different number of checks
    # depending on the values, and the count must be a DETERMINISTIC
    # number or it is not a number at all.
    ok_proceeded = _equal(proceeded, want_proceeded, calls)
    ok_exited = _equal(exited, want_exit, calls)
    ok_ran = _equal(bool(ran), want_proceeded, calls)
    return ok_proceeded and ok_exited and ok_ran


def _drive_main_with_baseline(base_failed, base_skipped, ran):
    """Call main() with a STUBBED suite, and report what it did.

    It returns (proceeded, exit_code): whether main() got past the
    baseline to the controls, and what it exited with.

    `ran` is a list the CALLER owns, and the stub control appends to
    it. That is deliberate: the record of what the stub did lives with
    the caller, so a mutation that substitutes the EXPECTATION for the
    observation — inside the driver or at the call site — cannot forge
    it. The driver can only append to a list it was handed.

    The stub replaces the suite run, so the wiring of main() is
    exercised without the cost of the suite — and, more to the point,
    without the guards being reachable some other way. The controls are
    stubbed too: a real control's mutator would edit the source.
    """
    # The stub is a plain control DICT, because the gate reads its
    # fields by key. Its mutator only records that it ran — a real
    # control's mutator would edit the source — and it appends to the
    # list the CALLER owns.
    stub = {
        "name": "stub",
        "invariant": "a stub control",
        "expect": [],
        "load_bearing": True,
        "breaks": [],
        "mutate": lambda: ran.append(True),
    }
    originals = {k: globals()[k] for k in
                 ("run_tests", "snapshot", "restore", "CONTROLS")}
    argv = sys.argv
    try:
        globals()["run_tests"] = lambda: (base_failed, [], 36,
                                          list(base_skipped))
        globals()["snapshot"] = lambda: None
        globals()["restore"] = lambda: None
        globals()["CONTROLS"] = [stub]
        sys.argv = ["continuity-001-pr2-controls.py", "--only", "stub"]
        try:
            main()
        except SystemExit as e:
            return (bool(ran), e.code)
        return (bool(ran), None)
    finally:
        for k, v in originals.items():
            globals()[k] = v
        sys.argv = argv


def selftest():
    """Prove that the gate can fail.

    The harness is EVIDENCE. Evidence that cannot fail is not evidence:
    a gate that always returned 0 would still print "14/14 controls are
    load-bearing" after every load-bearing control had stopped noticing
    anything.

    So this drives the summary with the results such a run WOULD
    produce, and requires the gate to REFUSE them.
    """
    first_lb = next(c["name"] for c in CONTROLS if c["load_bearing"])
    first_rd = next(c["name"] for c in CONTROLS if not c["load_bearing"])
    # The published number of cases. The self-test checks its own count
    # against it, and a Go test checks the harness's PRINTED count
    # against the same figure — so adding a case without publishing it
    # fails the suite in two places.
    #
    # The matrix is a list of SINGLE-EDIT mutations: a two-edit
    # mutation that removes both a mechanism and the guard that watches
    # it is not in the matrix, and the note says so rather than
    # claiming more.
    expected_self_test_cases = 60

    # Each case carries a MODE. The mode decides how the gate is
    # driven; the name is only the label. Dispatching on the label
    # would make the name load-bearing, so renaming a case would
    # silently change what the self-test proves.
    cases = [
        {"name": "every control behaved as published",
         "results": _synthetic_results(), "want": True, "mode": "normal"},
        {"name": f"the load-bearing control {first_lb} did not detect its removal",
         "results": _synthetic_results(broken_load_bearing=first_lb),
         "want": False, "mode": "normal", "isolates": True},
        {"name": f"the defence-in-depth control {first_rd} broke the suite",
         "results": _synthetic_results(broken_redundant=first_rd),
         "want": False, "mode": "normal", "isolates": True},
        {"name": f"the load-bearing control {first_lb} broke tests it did not declare",
         "results": _synthetic_results(undeclared=True),
         "want": False, "mode": "normal", "isolates": True},
        {"name": f"the load-bearing control {first_lb} took the whole suite with it",
         "results": _synthetic_results(broken_suite=True),
         "want": False, "mode": "normal", "isolates": True},
        {"name": f"the load-bearing control {first_lb} aborted the suite",
         "results": _synthetic_results(aborted=True),
         "want": False, "mode": "normal", "isolates": True},
        {"name": f"the load-bearing control {first_lb} made a test skip",
         "results": _synthetic_results(skipped=True),
         "want": False, "mode": "normal", "isolates": True},
        # M17: the comparison is TWO-SIDED, and the SPEC publishes
        # that. The "more" direction needs its own case, or a
        # regression to one-sided would pass this self-test, pass
        # `TestControls_TheHarnessGateCanFail`, and contradict a
        # published sentence — silently, with a green exit code.
        {"name": f"the load-bearing control {first_lb} produced MORE tests than the baseline",
         "results": _synthetic_results(more_tests=True),
         "want": False, "mode": "more_tests", "isolates": True},
        # N20: the defence-in-depth branch's own checks. A run that
        # produced a different number of tests is a broken
        # measurement there too, and nothing else catches it.
        {"name": f"the defence-in-depth control {first_rd} aborted the suite",
         "results": _synthetic_results(redundant_aborted=True),
         "want": False, "mode": "normal", "isolates": True},
        # N15: the baseline is REQUIRED. A caller that passes a zero
        # gets a refusal rather than the vulnerable measurement the
        # old default produced.
        {"name": "the baseline is zero",
         "results": _synthetic_results(),
         "want": False, "mode": "zero_baseline"},
        # N21: the explicit guard is load-bearing in the EMPTY
        # corner. Without it, `summarise([], 0, 0)` would accept an
        # empty measurement — no denominator, no controls, and a
        # green answer.
        {"name": "there is no baseline and no results at all",
         "results": [],
         "want": False, "mode": "zero_baseline"},
        # M2: the gate's FIRST question — the named test must fail —
        # had no case of its own. The "did not detect its removal"
        # case ALSO trips the "unexpected failures" clause, so
        # removing the `named` clause left the self-test green. This
        # case detects nothing and breaks nothing, so only `named` can
        # refuse it.
        {"name": f"the load-bearing control {first_lb} detected NOTHING at all",
         "results": _synthetic_results(detected_nothing=True),
         "want": False, "mode": "normal", "isolates": True},
        # M3(a): the comparison is by IDENTITY, not by count. The run
        # skipped a DIFFERENT test than the baseline did — the same
        # count — so a count comparison would wave it through.
        {"name": f"the load-bearing control {first_lb} skipped a DIFFERENT test than the baseline",
         "results": _synthetic_results(swapped_skip=True,
                                       baseline_skips=["TestSomethingSkipped"]),
         "want": False, "mode": "swapped_skip", "isolates": True},
        # M3(b): the same identity question on the OTHER branch — the
        # defence-in-depth branch's own comparison. The control's run
        # skipped a different test than the baseline did, the same
        # count, so a count comparison would call it clean.
        {"name": f"the defence-in-depth control {first_rd} skipped a DIFFERENT test than the baseline",
         "results": _synthetic_results(redundant_swapped_skip=True,
                                       baseline_skips=["TestSomethingSkipped"]),
         "want": False, "mode": "swapped_skip", "isolates": True},
        # M3(b): the defence-in-depth branch's skip clause, on its own.
        # Nothing failed, so the `not specific` clause cannot catch
        # it — the skip clause is what must.
        {"name": f"the defence-in-depth control {first_rd} made a test skip",
         "results": _synthetic_results(redundant_skipped=True),
         "want": False, "mode": "normal", "isolates": True},
        # M2: the DEFAULT — every control carries the baseline's skips —
        # is a mechanism too, and it was unpinned. Removing it TOGETHER
        # with the identity comparison defeated the identity comparison
        # entirely. This case pins it: every control skips exactly what
        # the baseline skipped, so the run is ACCEPTED. Remove the
        # default and every control differs from the baseline, and this
        # case fails.
        {"name": "every control skipped exactly what the baseline skipped",
         "results": _synthetic_results(baseline_skips=["TestSomethingSkipped"]),
         "want": True, "mode": "swapped_skip"},
    ]

    failures = 0

    # The LOG of the checks the self-test made. It is owned by THIS
    # frame, and every predicate appends to it as it runs.
    #
    # The self-test's case count is the log's LENGTH — so it is the
    # number of checks that were EVALUATED, not the number of lines
    # that were printed and not a number a branch increments on its way
    # past. Three things follow, and each is a mechanism:
    #
    #   * a check that STOPS RUNNING leaves the log short, so the
    #     count changes and the count's check fires;
    #   * a check that is NEUTERED at its call site does not log, so
    #     the count changes;
    #   * a predicate whose BODY is neutered still logs, so the count
    #     is unchanged — and the PROBE catches that one, because the
    #     probe drives the predicate directly with a wrong value.
    calls = []

    # M4: main()'s two baseline guards. The baseline decision is a
    # function, and these two cases are what fail when a guard is
    # removed — an inline guard has no case at all, which is how round
    # six's N23 fix went unverified.
    # M4: the verdict RAISES on a baseline that cannot be the
    # denominator, so main() cannot forget to look at it. A green
    # baseline returns the DENOMINATOR — and M5: the denominator is
    # the number of tests that ran and passed, which is what the
    # bound's denominator must be.
    try:
        good = baseline_verdict([], SUITE_SIZE, [])
        calls.append("baseline")
        if not _equal(good, SUITE_SIZE, calls):
            failures += 1
            print(f"\nSELF-TEST FAILED: a green baseline returned the denominator "
                  f"{good}, want {SUITE_SIZE}")
        else:
            print("\nSELF-TEST OK: a green baseline that skipped nothing was "
                  f"accepted, with the denominator {good}.")
    except SystemExit:
        failures += 1
        print("\nSELF-TEST FAILED: a green baseline was REFUSED")

    for label, bad in (("a baseline that is not green",
                        (["TestSomethingFailed"], SUITE_SIZE - 1, [])),
                       ("a baseline that skipped a test",
                        ([], SUITE_SIZE - 1, ["TestSomethingSkipped"]))):
        try:
            baseline_verdict(*bad)
            failures += 1
            print(f"\nSELF-TEST FAILED: {label} — the baseline was ACCEPTED")
        except SystemExit:
            calls.append("baseline")
            print(f"\nSELF-TEST OK: {label} — the baseline was refused.")

    # M4, the wiring: main() must USE the verdict. The function is
    # driven above; the CALL SITE is what makes the refusal real.
    #
    # This is checked BEHAVIOURALLY, by driving main() with a stubbed
    # suite: a source search would be satisfied by a COMMENT, and
    # main() could swallow the raise that was introduced so it could
    # not be ignored.
    for label, stub_failed, stub_skipped, want_proceeded, want_exit in (
            ("a not-green baseline", ["TestSomethingFailed"], [], False, 1),
            ("a baseline that skipped a test", [], ["TestSomethingSkipped"], False, 1),
            ("a green baseline", [], [], True, None)):
        # The record of what the stub did belongs to THIS frame: the
        # driver appends to the list it is handed, so a mutation that
        # substitutes the expectation for the observation cannot forge
        # it.
        ran = []
        proceeded, exited = _drive_main_with_baseline(stub_failed, stub_skipped,
                                                       ran)
        # ONE boolean, so that the mutation matrix can neuter the whole
        # verdict with one edit — and so that the three things it
        # checks are checked together rather than in a chain where a
        # neutered first clause hides the rest.
        #
        #   proceeded   main() got past the baseline to the controls
        #   exited      and it exited with the gate's code
        #   ran         and the STUB CONTROL'S OWN RECORD says so
        #
        # The third is the record that belongs to THIS frame: the
        # driver appends to the list it was handed, so a mutation that
        # substitutes the expectation for the observation cannot forge
        # it.
        wiring_ok = _wiring_ok(proceeded, exited, ran, want_proceeded,
                               want_exit, calls)
        if not wiring_ok:
            failures += 1
            print(f"\nSELF-TEST FAILED: main() with {label} "
                  f"{'proceeded' if proceeded else 'refused'} and exited {exited}, "
                  f"running the control {bool(ran)} — want "
                  f"{'proceeded' if want_proceeded else 'refused'}, exited "
                  f"{want_exit}, running the control {want_proceeded}")
        else:
            print(f"\nSELF-TEST OK: main() with {label} "
                  f"{'proceeded to the controls' if proceeded else 'refused the measurement'}.")

    # Every PREDICATE can FAIL — and that is proved, not assumed.
    #
    # A check that is inlined cannot be driven with a deliberately
    # wrong value, so an inlined check that is neutered leaves the
    # self-test green with nothing to notice it. Each predicate below
    # is therefore driven with a value it must refuse, and each probe
    # is a case.
    for label, predicate in (
            ("the equality predicate", lambda: _equal("a", "b", calls)),
            ("the published-message predicate", lambda: _message_ok("a run of "
             "this suite", calls)),
            ("the isolation predicate", lambda: _isolation_ok(2, calls)),
            ("the sample predicate",
             lambda: _sample_ok("nothing", "sample", calls))):
        if predicate():
            failures += 1
            print(f"\nSELF-TEST FAILED: {label} accepted a value it must "
                  f"refuse, so it cannot fail")
        else:
            print(f"\nSELF-TEST OK: {label} refuses a value it must refuse, so "
                  f"it can fail.")

    # The wiring verdict can FAIL — and that is proved, not assumed.
    # A verdict that is inlined cannot be driven with a deliberately
    # wrong expectation, so a neutered inlined verdict would leave the
    # self-test green with nothing to notice it. The function can be
    # driven, so it is.
    probe_ran = []
    probe_proceeded, probe_exited = _drive_main_with_baseline(
        ["TestSomethingFailed"], [], probe_ran)
    if _wiring_ok(probe_proceeded, probe_exited, probe_ran, True, 1, calls):
        failures += 1
        print("\nSELF-TEST FAILED: the wiring verdict accepted a deliberately "
              "wrong expectation, so it cannot fail")
    else:
        print("\nSELF-TEST OK: the wiring verdict refuses a deliberately wrong "
              "expectation, so it can fail.")

    # M6: the three patterns are the harness's entire contact with Go's
    # output. The `^` anchor is what stops a PASSING parent test whose
    # subtest skipped from being counted as skipped.
    sample = GO_OUTPUT_SAMPLE
    # The nit: the SKIP anchor's catchability rests on the sample
    # carrying an INDENTED skip, and the PASS anchor's on the sample
    # carrying a FAILING parent with an indented passing subtest. If
    # either line were removed from the sample, the anchor would become
    # unobservable — so the sample's shape is asserted.
    for label, needle in (
            ("an indented skip", "    --- SKIP: "),
            ("a failing parent with an indented passing subtest",
             "    --- PASS: TestAPIWithFailure/beta"),
            ("the failing parent's own top-level line",
             "--- FAIL: TestAPIWithFailure (0.00s)")):
        if not _sample_ok(needle, sample, calls):
            failures += 1
            print(f"\nSELF-TEST FAILED: the sample of Go's output does not carry "
                  f"{label}, so the pattern's anchor would be unobservable")
        else:
            print(f"\nSELF-TEST OK: the sample of Go's output carries {label}.")

    regex_cases = (
            ("the FAIL pattern", sorted(set(FAIL_RE.findall(sample))),
             ["TestAPIBroke", "TestAPIWithFailure"]),
            ("the PASS pattern", sorted(set(PASS_RE.findall(sample))),
             ["TestAPIAlpha", "TestAPIWithSkip"]),
            ("the SKIP pattern", sorted(set(SKIP_RE.findall(sample))),
             ["TestAPISkipped"]))
    for label, got, want in regex_cases:
        if not _equal(got, want, calls):
            failures += 1
            print(f"\nSELF-TEST FAILED: {label} read {got} from Go's output, "
                  f"want {want}")
        else:
            print(f"\nSELF-TEST OK: {label} read {got} from Go's output.")

    for case in cases:
        name, results, want = case["name"], case["results"], case["want"]
        # The output is CAPTURED, because a published MESSAGE is part of
        # what the gate says — N8 was a message that no case pinned.
        buf = io.StringIO()
        with contextlib.redirect_stdout(buf):
            if case["mode"] == "zero_baseline":
                ok = summarise(results, 0, ())
            elif case["mode"] == "swapped_skip":
                # The baseline skipped a DIFFERENT test, so the
                # identity comparison has something to compare.
                ok = summarise(results, SUITE_SIZE, ("TestSomethingSkipped",))
            else:
                ok = summarise(results, SUITE_SIZE, ())
        # N8: the "more tests" direction says what is actually wrong.
        # The verdict is unchanged either way, so the case that pins it
        # is the one that produces that direction.
        #
        # The dispatch is on the MODE, never on the name: a name is a
        # label, and round six's N24 was exactly this defect — the
        # round-seven fix reintroduced it, and the round-eight fix
        # removes it again.
        if case["mode"] == "more_tests":
            if not _message_ok(buf.getvalue(), calls):
                failures += 1
                print("\nSELF-TEST FAILED: the more-tests direction does not say "
                      "that the run is not a run of this suite")
            else:
                print("\nSELF-TEST OK: the more-tests direction says that the run "
                      "is not a run of this suite.")
        # MUT-43: a case that isolates a target must refuse EXACTLY
        # ONE control. A construction that leaked into the other
        # controls would refuse thirteen bystanders too — and the
        # VERDICT would be the same, so nothing else would notice.
        if case.get("isolates"):
            refused = buf.getvalue().count("\nNO  ")
            if not _isolation_ok(refused, calls):
                failures += 1
                print(f"\nSELF-TEST FAILED: {name} refused {refused} control(s), "
                      f"want 1 — the case must isolate the control it is about")
        verdict = "REFUSED" if not ok else "ACCEPTED"
        if not _equal(ok, want, calls):
            failures += 1
            print(f"\nSELF-TEST FAILED: {name} — the gate {verdict} it, want "
                  f"{'REFUSED' if not want else 'ACCEPTED'}")
        else:
            print(f"\nSELF-TEST OK: {name} — the gate {verdict} it.")

    # And the call WITHOUT a baseline at all is not even a measurement:
    # the parameter is required, so the call cannot be made.
    try:
        summarise(_synthetic_results())
        failures += 1
        print("\nSELF-TEST FAILED: the gate accepted a call with no baseline at all")
    except TypeError:
        calls.append("no-baseline")
        print("\nSELF-TEST OK: the gate refused a call with no baseline at all "
              "— the parameter is required, so the measurement cannot be made "
              "without one")
    # EVERY check the self-test makes is counted here, and the count
    # is the LOG's length — the number of checks that were EVALUATED,
    # not the number of lines that were printed and not a number a
    # branch increments on its way past.
    #
    # The log is the mechanism. A check that stops running leaves it
    # short; a check that is neutered at its call site does not log;
    # and the count is checked against the published literal below — so
    # removing a check fails the self-test, and a Go test pins the
    # published figure against what the harness actually prints.
    total = len(calls)
    if total != expected_self_test_cases:
        failures += 1
        print(f"\nSELF-TEST FAILED: the self-test made {total} case(s), want "
              f"{expected_self_test_cases} — an assertion stopped running, or a "
              f"case was added without the published figure being updated")
    else:
        print(f"\nSELF-TEST OK: the self-test made {total} case(s), as published.")

    # The count is checked BEFORE the verdict, so a count that does not
    # match the published figure FAILS the self-test rather than being
    # printed under a green exit code.
    if failures:
        print(f"\nTHE SELF-TEST FAILED ({failures} case(s)).")
        return 1
    print(f"\nTHE SELF-TEST PASSED: the gate can fail. {total} cases.")
    return 0


if __name__ == "__main__":
    if "--selftest" in sys.argv:
        sys.exit(selftest())
    sys.exit(main())
