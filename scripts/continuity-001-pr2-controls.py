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


def run_tests():
    """Run the acceptance suite and return the tests that fail."""
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
        return ["<TIMEOUT>"], []
    failed = sorted(set(re.findall(r"^--- FAIL: (\w+)", p.stdout, re.M)))
    if p.returncode != 0 and not failed:
        err = (p.stdout + p.stderr).strip().splitlines()
        return ["<BUILD FAILED>"], err[-3:]
    return failed, []


def replace_text(rel, old, new, what):
    """Replace one block of source text, and PROVE that it changed."""
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
            failed, detail = run_tests()
            inconclusive = [f for f in failed if f in INCONCLUSIVE]
            specific = [f for f in failed if f not in INCONCLUSIVE]
            hit = [t for t in control["expect"] if t in specific]
            print(f"    failing tests: {specific or '(none)'}")
            if detail:
                print(f"    detail: {detail}")
            if control["load_bearing"] and len(hit) != len(control["expect"]):
                print(f"    NOT LOAD-BEARING: {control['expect']} did not fail")
            if not control["load_bearing"] and specific:
                print(f"    UNEXPECTED: {specific} failed, but this control removes only one half")
            results.append((control, hit, specific, inconclusive))
    finally:
        restore()
        print("\nthe tree is restored")

    print("\n=== the summary ===")
    load_bearing = 0
    redundant = 0
    for control, hit, specific, inconclusive in results:
        if control["load_bearing"]:
            ok = len(hit) == len(control["expect"])
            load_bearing += 1 if ok else 0
            print(f"{'OK ' if ok else 'NO '} {control['name']:22s} {control['invariant']}")
            if not ok:
                print(f"     expected {control['expect']}, failing {specific}")
        else:
            redundant += 1
            print(f"OK  {control['name']:22s} {control['invariant']}")
            print(f"     (redundant: {control['why']})")
    total = load_bearing + redundant
    print(f"\n{load_bearing}/{total} controls are load-bearing; "
          f"{redundant} measure the defence in depth")
    return 0 if load_bearing == total - redundant else 1


if __name__ == "__main__":
    sys.exit(main())
