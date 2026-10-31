#!/usr/bin/env python3
"""test_rehearsal_seeding.py — the rehearsal's synthetic corpus.

THE DEFECT THIS FILE EXISTS FOR
-------------------------------
The rehearsal seeds a synthetic corpus into the simulation, so the
migration's promise ("the persistent state is preserved") is proved
against real rows rather than against an empty database.

One of those seeds was written like this:

    "$SIM_WS/bin/mpm" call mpm_lessons --payload '{"action":"save",...}'

and it SILENTLY stopped working. The lesson trust boundary (PR #189)
made `mpm_lessons action=save` OPERATOR-ONLY — a lesson is trusted on
arrival, the lesson surface has no pending state, so the agent-reachable
write is refused at the execution point. The refusal was invisible,
because every seeding call redirects its output to /dev/null, and the
rehearsal only noticed one suite later, as:

    FAIL the synthetic lesson survived (0 rows)

which says nothing about the cause. **The migration proof was silently
not being exercised at all**, on the very candidate that was deployed.

THE TWO PROPERTIES PINNED HERE
------------------------------
1. The seeding uses the OPERATOR path (`mpm lesson add`), which creates a
   TRUSTED lesson — the thing the migration's preservation claim is
   about. (`mpm_lessons action=propose` would create a PENDING PROPOSAL
   in a different table, and the proof would be about the wrong table.)
2. The seeding is LOUD: a seed that is refused stops the rehearsal, so
   this class of defect cannot hide behind a redirected stream again.

And the refused action must not come back.
"""

import re
import unittest
from pathlib import Path

REPO_ROOT = Path(__file__).resolve().parent.parent.parent
SIMULATE = REPO_ROOT / "scripts" / "rehearsal" / "00_simulate.sh"

# The action that PR #189 refused at the execution point.
REFUSED_ACTION = "mpm_lessons"


class TestTheRehearsalSeeding(unittest.TestCase):
    def setUp(self):
        self.text = SIMULATE.read_text()

    def test_the_refused_lesson_action_is_not_used(self):
        # The refusal is not an error the seeding can tolerate: it
        # means the trusted lesson was never created, and the migration
        # proof is not being exercised.
        #
        # COMMENTS ARE NOT CALLERS.  The file quotes the old call in a
        # comment, because that is the only place a reader learns WHY
        # the operator path is used — so the scan skips comment lines.
        offenders = []
        for line in self.text.splitlines():
            if line.lstrip().startswith("#"):
                continue
            if REFUSED_ACTION in line and re.search(r'"action"\s*:\s*"save"', line):
                offenders.append(line)
        self.assertEqual(
            offenders, [],
            "the rehearsal must not seed a lesson through the refused "
            f"agent-reachable action:\n" + "\n".join(offenders),
        )

    def test_the_trusted_lesson_is_seeded_through_the_operator_path(self):
        self.assertIn(
            "mpm\" lesson add", self.text,
            "the trusted lesson must be seeded with `mpm lesson add`, the "
            "operator path, which creates a TRUSTED lesson",
        )

    def test_the_lesson_seed_is_loud(self):
        # A seed that fails must stop the rehearsal. Otherwise the
        # failure surfaces one suite later as an unrelated symptom.
        self.assertIn(
            "seed_lesson()", self.text,
            "the lesson seeding must be a function that checks its result",
        )
        self.assertRegex(
            self.text,
            r"seed_lesson\(\)\s*\{[^}]*if\s+!",
            "the lesson seeding must check the exit status of the write",
        )
        self.assertRegex(
            self.text,
            r"seed_lesson\(\)\s*\{[^}]*exit 1",
            "a refused seed must stop the rehearsal, not be swallowed",
        )

    def test_the_synthetic_lesson_is_still_asserted(self):
        # The rehearsal must still PROVE the lesson survived: the fix
        # must not have quietly removed the proof.
        obligations = (REPO_ROOT / "scripts" / "rehearsal" / "10_obligations.sh").read_text()
        self.assertIn(
            "the synthetic lesson survived", obligations,
            "the migration's preservation of the lesson surface must still "
            "be asserted",
        )


if __name__ == "__main__":
    unittest.main()
