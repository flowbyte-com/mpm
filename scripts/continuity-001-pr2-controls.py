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
import subprocess
import sys
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
# The DDL pin is in this set for the controls that mutate the DDL: it
# fails because the SOURCE changed, which is not a statement about the
# invariant under test.
INCONCLUSIVE = {
    "<BUILD FAILED>",
    "<TIMEOUT>",
    "TestAgentAssignmentDDLIsPinned",
    "TestAgentAssignmentObjectInventoryIsStable",
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


def run_tests():
    """Run the acceptance suite and return the tests that fail, and how many ran."""
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
        return ["<TIMEOUT>"], [], 0
    failed = sorted(set(re.findall(r"^--- FAIL: (\w+)", p.stdout, re.M)))
    passed = len(set(re.findall(r"^--- PASS: (\w+)", p.stdout, re.M)))
    if p.returncode != 0 and not failed:
        err = (p.stdout + p.stderr).strip().splitlines()
        return ["<BUILD FAILED>"], err[-3:], passed
    return failed, [], passed


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
        # The renewal test depends on the fence too: a renewal that
        # did not advance the epoch would not fence the old holder
        # out. Removing the fence breaks both.
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


def main():
    ap = argparse.ArgumentParser(description=__doc__)
    ap.add_argument("--only", help="run one control by name")
    args = ap.parse_args()
    if args.only and args.only not in {c["name"] for c in CONTROLS}:
        raise SystemExit(f"there is no control named {args.only!r}")

    snapshot()
    results = []
    try:
        for control in CONTROLS:
            name = control["name"]
            if args.only and args.only != name:
                continue
            restore()
            print(f"\n=== {name}: {control['invariant']} ===")
            control["mutate"]()
            failed, detail, passed = run_tests()
            inconclusive = [f for f in failed if f in INCONCLUSIVE]
            specific = [f for f in failed if f not in INCONCLUSIVE]
            hit = [t for t in control["expect"] if t in specific]
            total = len(specific) + passed
            print(f"    failing tests: {specific or '(none)'}")
            if detail:
                print(f"    detail: {detail}")
            if control["load_bearing"] and len(hit) != len(control["expect"]):
                print(f"    NOT LOAD-BEARING: {control['expect']} did not fail")
            if not control["load_bearing"] and specific:
                print(f"    UNEXPECTED: {specific} failed, but this control removes only one half")
            results.append((control, hit, specific, inconclusive, total))
    finally:
        restore()
        print("\nthe tree is restored")

    ok = summarise(results)
    return 0 if ok else 1


def summarise(results):
    """Print the summary and return whether the measurement holds.

    The RETURN VALUE is the gate. It can fail, and it does.
    """
    print("\n=== the summary ===")
    load_bearing_passed = 0
    load_bearing_total = 0
    redundant_total = 0
    redundant_clean = 0
    for control, hit, specific, inconclusive, total in results:
        if control["load_bearing"]:
            load_bearing_total += 1
            named = len(hit) == len(control["expect"])
            # The BOUND. A mutation that broke more than a third of the
            # suite did not remove one invariant — it broke the build,
            # and the named test may have failed for that reason
            # instead. Such a control is INCONCLUSIVE, and an
            # inconclusive control does not count as load-bearing.
            broke_suite = (total > 0 and
                           len(specific) * BROKEN_SUITE_FRACTION > total)
            if broke_suite:
                print(f"     INCONCLUSIVE: the mutation broke {len(specific)} of the "
                      f"suite's {total} tests — that is a broken build, not a "
                      f"removed invariant")
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
            # it claimed — a broken measurement, not a pass.
            clean = not specific
            if clean:
                redundant_clean += 1
            print(f"{'OK ' if clean else 'NO '} {control['name']:22s} {control['invariant']}")
            print(f"     (redundant: {control['why']})")
            if not clean:
                print(f"     UNEXPECTED failures: {specific}")
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
                       undeclared=False, broken_suite=False):
    """Build the results a run would produce, for the self-test.

    `broken_load_bearing` names a load-bearing control whose named test
    did NOT fail; `broken_redundant` names a defence-in-depth control
    that DID break the suite; `undeclared` adds a failure the control
    did not declare it would break — the shape a mutation that broke
    the STATEMENT produces, where the named test fails for the wrong
    reason and takes the rest of the suite with it.
    """
    first_lb = next(c["name"] for c in CONTROLS if c["load_bearing"])
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
            results.append((control, hit, specific, [], SUITE_SIZE))
        else:
            specific = []
            if control["name"] == broken_redundant:
                specific = ["TestTheSuiteBroke"]
            results.append((control, [], specific, [], SUITE_SIZE))
    return results


def selftest():
    """Prove that the gate can fail.

    The harness is EVIDENCE. Evidence that cannot fail is not evidence:
    a gate that always returned 0 would still print "13/13 controls are
    load-bearing" after every load-bearing control had stopped noticing
    anything.

    So this drives the summary with the results such a run WOULD
    produce, and requires the gate to REFUSE them.
    """
    first_lb = next(c["name"] for c in CONTROLS if c["load_bearing"])
    first_rd = next(c["name"] for c in CONTROLS if not c["load_bearing"])

    cases = [
        ("every control behaved as published",
         _synthetic_results(), True),
        (f"the load-bearing control {first_lb} did not detect its removal",
         _synthetic_results(broken_load_bearing=first_lb), False),
        (f"the defence-in-depth control {first_rd} broke the suite",
         _synthetic_results(broken_redundant=first_rd), False),
        (f"the load-bearing control {first_lb} broke tests it did not declare",
         _synthetic_results(undeclared=True), False),
        (f"the load-bearing control {first_lb} took the whole suite with it",
         _synthetic_results(broken_suite=True), False),
    ]

    failures = 0
    for name, results, want in cases:
        ok = summarise(results)
        verdict = "REFUSED" if not ok else "ACCEPTED"
        if ok != want:
            failures += 1
            print(f"\nSELF-TEST FAILED: {name} — the gate {verdict} it, want "
                  f"{'REFUSED' if not want else 'ACCEPTED'}")
        else:
            print(f"\nSELF-TEST OK: {name} — the gate {verdict} it.")
    if failures:
        print(f"\nTHE SELF-TEST FAILED ({failures} case(s)).")
        return 1
    print("\nTHE SELF-TEST PASSED: the gate can fail.")
    return 0


if __name__ == "__main__":
    if "--selftest" in sys.argv:
        sys.exit(selftest())
    sys.exit(main())
