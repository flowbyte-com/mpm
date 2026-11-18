#!/usr/bin/env python3
"""continuity-001-controls.py — the negative controls for Continuity 001 PR 1.

WHAT THIS IS

    A negative control REMOVES one invariant and re-runs the acceptance
    suite. A control that removes an invariant nobody's test notices is a
    control that says the suite does not test that invariant — which is
    worth knowing, and worth publishing.

    The design note (docs/dev/continuity-001-assignment-schema.md) section 8
    publishes the measured counts. This script is what produces them, so
    that the published numbers can be reproduced rather than taken on
    trust.

HOW IT WORKS

    The mutation is applied to the WORKING TREE, the acceptance suite is
    run, and the tree is restored from a pristine copy. Nothing is
    committed and nothing is pushed.

    Two rules make the counts honest:

      * a mutation that does NOT change the file is REFUSED. A control
        that changes nothing measures nothing, and reports a count of
        zero that reads like "the suite does not depend on this
        invariant" — the most dangerous possible outcome. (It happened:
        three controls were wired to a function that was never called,
        so they mutated nothing and reported a misleading zero.)

      * the pristine copy is taken at the START of every run, never
        reused. A copy that survives from an earlier revision is how a
        control run silently measures the wrong DDL.

HOW TO RUN IT

    python3 scripts/continuity-001-controls.py            # every control
    python3 scripts/continuity-001-controls.py --only tree  # one control

    It needs the Go toolchain and the FTS5 build flags, and it runs the
    suite with them — the same flags `make test` uses.
"""

import argparse
import hashlib
import os
import re
import shutil
import subprocess
import sys
import tempfile
from pathlib import Path

REPO = Path(__file__).resolve().parent.parent
CORE = REPO / "internal/core"
DDL_FILE = CORE / "migration_agent_assignments.go"
DB_FILE = CORE / "db.go"
BACKUP = Path(tempfile.gettempdir()) / f"mpm-continuity-001-controls-{os.getpid()}"

# The two files a control may mutate. The migration file is untracked on
# some revisions, so `git checkout --` cannot restore it; a copy is the
# honest mechanism.
FILES = (
    "internal/core/migration_agent_assignments.go",
    "internal/core/db.go",
)

TEST_PATTERN = "TestAgentAssignment|TestAssignment"

# The failures that follow from the DDL's TEXT changing whatever invariant
# is under test. They are listed so that the second column of the design
# note's table can be read as "the tests that fail because the BEHAVIOUR
# changed".
# The failures that are not about the invariant under test at all. A
# build failure or a timeout is INCONCLUSIVE: it says nothing about
# whether the suite depends on the rule, and counting it as an
# invariant-specific failure is exactly the misleading number this
# harness exists to avoid.
INCONCLUSIVE = {
    "<BUILD FAILED>",
    "<TIMEOUT>",
}

STRUCTURAL = {
    "TestAgentAssignmentDDLIsPinned",
    "TestAgentAssignmentAT1ExistingDatabaseMigration",
    "TestAgentAssignmentAT2FreshDatabaseSchemaEquivalence",
    "TestAgentAssignmentAT6RecoveryAfterInjectedFailure",
    "TestAgentAssignmentAT8RestoreAllowList",
    "TestAgentAssignmentObjectInventoryIsStable",
    "TestAssignmentNegativeControlsAreLoadBearing",
    "TestAssignmentIdempotencyControlIsLoadBearing",
    "TestAssignmentClaimControlIsLoadBearing",
}


# The digest of EVERY file a control may mutate, as they were when this
# module was LOADED. It is taken here, ABOVE the control list, because
# the control list is where a mutator written as a plain function would
# run — and one that ran there would change the tree before the pristine
# copy was taken, so every measurement would be made against a corrupted
# tree. That happened once.
#
# `snapshot()` compares the live files against these digests, so an
# import-time mutation is caught rather than measured. It covers BOTH
# files: the DDL and the db.go the migration is called from. The fifth
# review found that the guard covered only the DDL, so an import-time
# mutation of db.go — which the "the whole migration is not called"
# control makes — would have passed it.
_IMPORT_DIGEST = {
    rel: hashlib.sha256((REPO / rel).read_bytes()).hexdigest()
    for rel in FILES
}


def snapshot():
    """Take the pristine copy from the CURRENT tree.

    The copy goes into a FRESH temporary directory, so a run never
    removes a directory another run put there, and so the tree is left
    exactly as it was found.
    """
    for rel in FILES:
        live = hashlib.sha256((REPO / rel).read_bytes()).hexdigest()
        if live != _IMPORT_DIGEST[rel]:
            raise SystemExit(
                f"{rel} changed while this harness was loading, so a control "
                "mutated the tree before the pristine copy was taken — every "
                "measurement would be against a corrupted tree"
            )
    if BACKUP.exists():
        shutil.rmtree(BACKUP)
    BACKUP.mkdir(parents=True)
    for rel in FILES:
        shutil.copy2(REPO / rel, BACKUP / Path(rel).name)
    print(f"the pristine copy is at {BACKUP}")


def restore():
    """Restore the two mutated files from the pristine copy."""
    if not BACKUP.exists():
        raise SystemExit("there is no pristine copy; call snapshot() first")
    for rel in FILES:
        shutil.copy2(BACKUP / Path(rel).name, REPO / rel)


def run_tests():
    """Run the acceptance suite and return the tests that fail."""
    env = dict(os.environ)
    env["CGO_CFLAGS"] = "-DSQLITE_ENABLE_FTS5=1"
    env["CGO_LDFLAGS"] = "-lm"
    try:
        p = subprocess.run(
            ["go", "test", "-tags", "fts5", "-count=1", "-v", "-run", TEST_PATTERN, "."],
            cwd=CORE, capture_output=True, text=True, env=env, timeout=3600,
        )
    except subprocess.TimeoutExpired:
        return ["<TIMEOUT>"], []
    failed = sorted(set(re.findall(r"^--- FAIL: (\w+)", p.stdout, re.M)))
    if p.returncode != 0 and not failed:
        err = (p.stdout + p.stderr).strip().splitlines()
        return ["<BUILD FAILED>"], err[-3:]
    return failed, []


def _inconclusive(failed):
    """A build failure or a timeout is not an invariant-specific failure.

    The fifth review found that a control whose mutation broke the build
    was counted as ONE failing test and ONE invariant-specific failure —
    a number that reads like "the suite depends on this invariant" when
    the truth is that nothing was measured at all. So a control that
    cannot build is reported as INCONCLUSIVE, and the invariant-specific
    count for it is zero.
    """
    return [f for f in failed if f in INCONCLUSIVE]


def _write_mutation(before, after, what):
    """Write the mutation, and PROVE that it changed something."""
    if before == after:
        raise SystemExit(
            f"the control for {what} changed NOTHING — the mutation is a no-op, "
            "so the control would report a misleading zero"
        )
    DDL_FILE.write_text(after)


def drop_trigger(name):
    """Remove one trigger from the DDL."""
    def mutate():
        t = DDL_FILE.read_text()
        pattern = r"CREATE TRIGGER IF NOT EXISTS " + re.escape(name) + r"\b.*?\nEND;\n"
        new, n = re.subn(pattern, "", t, flags=re.S)
        if n != 1:
            raise SystemExit(f"the control could not find the trigger {name} ({n} matches)")
        _write_mutation(t, new, name)
    return mutate


def drop_text(block, what="the block"):
    """Remove one block of DDL text."""
    def mutate():
        t = DDL_FILE.read_text()
        if t.count(block) != 1:
            raise SystemExit(f"the control could not find {what} ({t.count(block)} matches)")
        _write_mutation(t, t.replace(block, ""), what)
    return mutate


def replace_text(old, new, what="the block"):
    """Replace one block of DDL text."""
    def mutate():
        t = DDL_FILE.read_text()
        if t.count(old) != 1:
            raise SystemExit(f"the control could not find {what} ({t.count(old)} matches)")
        _write_mutation(t, t.replace(old, new), what)
    return mutate


def drop_idempotency_index():
    """Remove the UNIQUE INDEX on the idempotency key.

    This control measures the INDEX ALONE, and the honest answer is that
    it is not independently load-bearing: the id-not-reused trigger
    refuses a duplicate key on its own, so removing the index changes
    nothing that a test notices. That is a deliberate redundancy — the
    index is what lets the API FIND a duplicate without a trigger
    refusing the write — and it is published as measured rather than
    hidden.

    It is NOT what lets the API resolve a duplicate with ON CONFLICT:
    the BEFORE INSERT trigger fires BEFORE the conflict resolution, so
    an INSERT ... ON CONFLICT DO NOTHING ABORTS with the trigger's
    message rather than resolving the duplicate. PR 2 must therefore
    SELECT the existing assignment after the refusal, not rely on
    ON CONFLICT.
    """
    return drop_text(
        "CREATE UNIQUE INDEX IF NOT EXISTS idx_agent_tasks_idempotency_key\n"
        "    ON agent_tasks(idempotency_key);\n",
        "the unique idempotency index")


def drop_the_idempotency_rule():
    """Remove BOTH layers of the idempotency rule.

    The rule is enforced by the unique index AND by the id-not-reused
    trigger. This control removes both, so it measures the RULE rather
    than one of its layers.
    """
    def mutate():
        drop_idempotency_index()()
        drop_trigger("trg_agent_tasks_id_is_not_reused")()
    return mutate


def drop_containment_fks():
    return drop_text(
        """    FOREIGN KEY (root_id, project_id)
        REFERENCES agent_tasks(id, project_id) ON DELETE RESTRICT,
    FOREIGN KEY (parent_id, project_id)
        REFERENCES agent_tasks(id, project_id) ON DELETE RESTRICT,
""",
        "the composite containment foreign keys")


def drop_root_child_check():
    """Remove the root/child budget CHECK."""
    def mutate():
        t = DDL_FILE.read_text()
        pattern = r"    CHECK \(\n        \(\n            depth = 0.*?\n        \)\n    \),\n"
        new, n = re.subn(pattern, "", t, flags=re.S)
        if n != 1:
            raise SystemExit(f"the control could not find the root/child CHECK ({n} matches)")
        _write_mutation(t, new, "the root/child CHECK")
    return mutate


def drop_state_vocabulary():
    # The COLUMN is kept; only its CHECK is removed. Removing the column
    # too would break every test for the wrong reason — the schema would
    # not exist at all — and the control would stop isolating the rule.
    return replace_text(
        """    state               TEXT NOT NULL DEFAULT 'enqueued'
                        CHECK (state IN ('enqueued','delivered','acknowledged','accepted',
                                         'in_progress','completed','review_passed',
                                         'awaiting_operator','operator_approved','cancelled')),""",
        "    state               TEXT NOT NULL DEFAULT 'enqueued',",
        "the state vocabulary CHECK")


def drop_type_guards():
    """Neuter every typeof() guard, keeping the CHECK constraints valid.

    Deleting the guard outright would leave `CHECK ()` behind, which is
    not valid SQL — the control would then measure "the schema does not
    exist" rather than "the type is not checked". So each comparison
    `typeof(x) = 'text'` becomes `1 = 1`, which is always true and keeps
    the surrounding constraint meaningful.
    """
    def mutate():
        t = DDL_FILE.read_text()
        new, n = re.subn(r"typeof\([a-z_]+\) = '(?:text|integer)'", "1 = 1", t)
        if n < 10:
            raise SystemExit(f"the type-guard control only neutered {n} guards")
        _write_mutation(t, new, "the typeof() type guards")
    return mutate


def drop_the_whole_migration():
    """Remove the migration's call from the boot path."""
    def mutate():
        t = DB_FILE.read_text()
        block = """\tif err := MigrateAgentAssignments(tx); err != nil {
\t\t_ = tx.Rollback()
\t\treturn fmt.Errorf("agent assignments migration failed: %w", err)
\t}
"""
        if t.count(block) != 1:
            raise SystemExit("the control could not find the migration call")
        after = t.replace(block, "")
        if after == t:
            raise SystemExit("the migration control changed NOTHING")
        DB_FILE.write_text(after)
    return mutate


CONTROLS = [
    ("the state-machine trigger is removed", drop_trigger("trg_agent_tasks_state_transition")),
    ("the initial-state trigger is removed", drop_trigger("trg_agent_tasks_initial_state")),
    ("the unclaimed-at-creation trigger is removed",
     drop_trigger("trg_agent_tasks_unclaimed_at_creation")),
    ("the id-not-reused trigger is removed", drop_trigger("trg_agent_tasks_id_is_not_reused")),
    ("the id-immutability trigger is removed",
     drop_trigger("trg_agent_tasks_id_is_immutable")),
    ("the key-immutability trigger is removed",
     drop_trigger("trg_agent_tasks_key_is_immutable")),
    ("the claim-needs-a-live-assignment trigger is removed",
     drop_trigger("trg_agent_tasks_claim_needs_a_live_assignment")),
    ("the lease-changes-only-on-a-claim trigger is removed",
     drop_trigger("trg_agent_tasks_lease_changes_only_on_a_claim")),
    ("the tree-shape trigger is removed", drop_trigger("trg_agent_tasks_tree_shape")),
    ("the tree-shape-on-UPDATE trigger is removed",
     drop_trigger("trg_agent_tasks_tree_shape_on_update")),
    ("the depth-immutability trigger is removed",
     drop_trigger("trg_agent_tasks_depth_is_immutable")),
    ("the role-immutability trigger is removed", drop_trigger("trg_agent_tasks_role_is_immutable")),
    ("the root-recipient-is-eligible trigger is removed",
     drop_trigger("trg_agent_tasks_root_recipient_is_eligible")),
    ("the root-recipient-is-eligible-on-UPDATE trigger is removed",
     drop_trigger("trg_agent_tasks_root_recipient_is_eligible_on_update")),
    ("the permitted-operations INSERT trigger is removed",
     drop_trigger("trg_agent_tasks_permitted_operations_insert")),
    ("the permitted-operations UPDATE trigger is removed",
     drop_trigger("trg_agent_tasks_permitted_operations_update")),
    ("the eligible-recipients INSERT trigger is removed",
     drop_trigger("trg_agent_tasks_eligible_recipients_insert")),
    ("the eligible-recipients UPDATE trigger is removed",
     drop_trigger("trg_agent_tasks_eligible_recipients_update")),
    ("the not-deletable trigger is removed", drop_trigger("trg_agent_tasks_not_deletable")),
    ("the awaiting-action-is-immutable trigger is removed",
     drop_trigger("trg_agent_tasks_awaiting_action_is_immutable")),
    ("the approval-is-immutable trigger is removed",
     drop_trigger("trg_agent_tasks_approval_is_immutable")),
    ("the ledger's id-not-reused trigger is removed",
     drop_trigger("trg_agent_task_events_id_is_not_reused")),
    ("the ledger's monotonic-index trigger is removed",
     drop_trigger("trg_agent_task_events_monotonic")),
    ("the ledger's no-UPDATE trigger is removed",
     drop_trigger("trg_agent_task_events_no_update")),
    ("the ledger's no-DELETE trigger is removed",
     drop_trigger("trg_agent_task_events_no_delete")),
    ("ALL FOUR claim-discipline triggers are removed", lambda: [
        drop_trigger(n)() for n in (
            "trg_agent_tasks_claim_discipline",
            "trg_agent_tasks_claim_sets_owner",
            "trg_agent_tasks_claim_sets_lease",
            "trg_agent_tasks_owner_changes_only_on_claim",
        )]),
    ("the unique idempotency index is removed", drop_idempotency_index()),
    ("the idempotency rule's index AND its trigger are removed",
     drop_the_idempotency_rule()),
    ("the composite containment foreign keys are removed", drop_containment_fks()),
    ("the root/child budget CHECK is removed", drop_root_child_check()),
    ("the state vocabulary CHECK is removed", drop_state_vocabulary()),
    ("the typeof() type guards are removed", drop_type_guards()),
    ("the whole migration is not called", drop_the_whole_migration()),
]


def control(name, mutate):
    print(f"\n=== CONTROL: {name} ===")
    restore()
    mutate()
    failed, notes = run_tests()
    if notes:
        print("  " + "\n  ".join(notes))
    inconclusive = _inconclusive(failed)
    if inconclusive:
        # The mutation broke the build, so nothing was measured. Saying
        # so is the honest answer; counting it as an invariant-specific
        # failure would read like "the suite depends on this rule".
        print(f"  INCONCLUSIVE: {', '.join(inconclusive)} — nothing was measured")
        print("  tests that fail: 0")
        print("  invariant-specific: 0")
        restore()
        return
    specific = [f for f in failed if f not in STRUCTURAL]
    print(f"  tests that fail: {len(failed)}")
    print(f"  invariant-specific: {len(specific)}")
    for f in failed:
        mark = "  " if f in STRUCTURAL else "* "
        print(f"    {mark}- {f}")
    restore()


def main():
    ap = argparse.ArgumentParser(description=__doc__)
    ap.add_argument("--only", default="",
                    help="run only the controls whose name contains this")
    args = ap.parse_args()

    snapshot()
    restore()

    try:
        print("=== BASELINE (nothing removed) ===")
        failed, notes = run_tests()
        print(f"  tests that fail: {len(failed)}")
        for f in failed:
            print(f"    - {f}")
        if notes:
            print("  " + "\n  ".join(notes))
        if failed:
            raise SystemExit("the baseline must be GREEN before a control means anything")

        for name, mutate in CONTROLS:
            if args.only and args.only not in name:
                continue
            control(name, mutate)
    finally:
        # The tree is restored on EVERY path, including a failure or a
        # Ctrl-C: a control run must never leave the checkout mutated.
        restore()
        if BACKUP.exists():
            shutil.rmtree(BACKUP, ignore_errors=True)
    print("\n=== the tree is restored ===")


if __name__ == "__main__":
    sys.exit(main())
