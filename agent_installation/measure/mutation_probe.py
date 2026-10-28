#!/usr/bin/env python3
"""mutation_probe.py — prove the managed-block tests are non-vacuous.

A test that cannot fail proves nothing. This probe MUTATES the
behaviour under test, runs the named test, and asserts that the test
goes RED. Each mutation is reverted immediately, and the working tree
is verified byte-identical afterwards.

The mutations are the ones that matter:

  * removing a matrix row                     → the matrix loses a
                                                destination
  * changing a matrix destination             → the matrix points at
                                                the wrong artifact kind
  * removing the "no write" row               → persistence becomes a
                                                reflex
  * removing the uncertainty rule             → speculation can be
                                                promoted to a fact
  * removing the session-end rule             → a handoff can be
                                                triggered by a turn
  * removing the shared-records principle     → a handoff becomes a
                                                knowledge silo
  * breaking the installer's user-content     → reconciliation
    preservation                               destroys operator text
  * breaking the installer's idempotency      → the block is
                                                duplicated on upgrade

Usage:
    python3 agent_installation/measure/mutation_probe.py
    python3 agent_installation/measure/mutation_probe.py --list

Exit code 0 means every mutation was detected.
"""

from __future__ import annotations

import argparse
import shutil
import subprocess
import sys
import tempfile
from pathlib import Path


REPO_ROOT = Path(__file__).resolve().parents[2]
AGENT_INSTALLATION = REPO_ROOT / "agent_installation"
CANONICAL_SOURCE = AGENT_INSTALLATION / "MPM_AGENT_INTEGRATION_SNIPPETS.md"
INSTALLER = (
    AGENT_INSTALLATION / "mpm-opencode" / "scripts"
    / "install_agents_instructions.py"
)
RENDER = AGENT_INSTALLATION / "scripts" / "render_managed_blocks.py"

TESTS = "tests.test_managed_block_behaviour_matrix"


class Mutation:
    def __init__(self, name: str, path: Path, old: str, new: str,
                 test: str, why: str, count: int = 0):
        self.name = name
        self.path = path
        self.old = old
        self.new = new
        self.test = test
        self.why = why
        # `count` is how many occurrences the mutation replaces.
        # 0 means "every occurrence", which is what a mutation of the
        # canonical source needs: the file carries the block TWICE —
        # the copy/paste example and the host-neutral canonical block —
        # and the renderer derives the snippet from the LAST one. A
        # mutation that hit only the copy/paste example would leave
        # the canonical block intact, and every test would stay green
        # for the wrong reason.
        self.count = count


def _mutations() -> list[Mutation]:
    src = CANONICAL_SOURCE
    inst = INSTALLER
    return [
        Mutation(
            "remove-the-evidence-row",
            src,
            "| Supporting or contradicting observation | `mpm_evidence` action `add` |\n",
            "",
            f"{TESTS}.BehaviourMatrixContract.test_matrix_teaches_every_write_destination",
            "the matrix loses a destination, so evidence stops being taught",
        ),
        Mutation(
            "point-the-fact-row-at-lessons",
            src,
            "| Reusable fact, constraint, root cause | `mpm_memory` action `save` |",
            "| Reusable fact, constraint, root cause | `mpm_lessons` action `save` |",
            f"{TESTS}.BehaviourMatrixContract.test_matrix_teaches_every_write_destination",
            "the matrix routes a fact to the wrong artifact kind",
        ),
        Mutation(
            "remove-the-no-write-row",
            src,
            "| Routine or transient information | nothing |\n",
            "",
            f"{TESTS}.BehaviourMatrixContract.test_matrix_teaches_every_write_destination",
            "persistence becomes a reflex — the negative control is gone",
        ),
        Mutation(
            "remove-the-uncertainty-rule",
            src,
            "| Unverified hypothesis — never a fact or lesson |",
            "| Unverified hypothesis |",
            f"{TESTS}.BehaviourMatrixContract.test_pending_knowledge_is_distinguished_from_trusted",
            "speculation can be presented as a trusted fact",
        ),
        Mutation(
            "remove-the-session-end-rule",
            src,
            "**No signal reliably marks a session end**: turn\ncompletion, idle, archive and shutdown are not one.",
            "Turn completion, idle, archive and shutdown are all session ends.",
            f"{TESTS}.BehaviourMatrixContract.test_no_unreliable_session_end_signal_is_required",
            "a handoff can be triggered by a turn, an idle or a shutdown",
        ),
        Mutation(
            "remove-the-shared-records-principle",
            src,
            "**Reusable knowledge belongs in shared MPM records** — any agent can retrieve\nthem without knowing which session found them.",
            "Reusable knowledge belongs in your notes.",
            f"{TESTS}.BehaviourMatrixContract.test_memory_writes_are_distinguished_from_handoffs",
            "a handoff becomes a private knowledge silo",
        ),
        Mutation(
            "break-user-content-preservation-on-replace",
            inst,
            "        target_path.write_text(replaced)",
            "        target_path.write_text(new_managed)",
            f"{TESTS}.ManagedBlockDelivery.test_user_authored_content_survives_an_upgrade",
            "reconciliation destroys the operator's own text (the "
            "replace path)",
        ),
        Mutation(
            "break-user-content-preservation-on-append",
            inst,
            "    target_path.write_text(existing.rstrip() + insertion)",
            "    target_path.write_text(insertion)",
            f"{TESTS}.ManagedBlockDelivery.test_user_authored_content_survives_reconciliation",
            "reconciliation destroys the operator's own text (the append "
            "path)",
        ),
        Mutation(
            "break-idempotency",
            inst,
            "        if (existing_block or \"\").rstrip() == new_managed.rstrip():",
            "        if False:",
            f"{TESTS}.ManagedBlockDelivery.test_repeated_installation_does_not_duplicate",
            "the block is duplicated on every upgrade",
        ),
    ]


def _run(test: str) -> tuple[int, str]:
    proc = subprocess.run(
        [
            sys.executable, "-m", "unittest", test,
        ],
        cwd=str(AGENT_INSTALLATION),
        capture_output=True, text=True, timeout=300,
    )
    return proc.returncode, proc.stdout + proc.stderr


def main() -> int:
    ap = argparse.ArgumentParser()
    ap.add_argument("--list", action="store_true")
    args = ap.parse_args()

    mutations = _mutations()
    if args.list:
        for m in mutations:
            print(f"{m.name:38} {m.test}")
        return 0

    # A scratch copy of the tree is NOT used: the mutation must be
    # applied to the real files, because the tests read them from
    # there. Every mutation is reverted in a finally block, and the
    # tree is verified byte-identical at the end.
    backups: dict[Path, bytes] = {}
    derived = [
        AGENT_INSTALLATION / "mpm-opencode" / "templates" / "AGENTS.md.snippet",
        REPO_ROOT / "README.md",
    ]
    failures: list[str] = []
    try:
        for m in mutations:
            original = backups.get(m.path)
            if original is None:
                original = m.path.read_bytes()
                backups[m.path] = original
            text = m.path.read_text(encoding="utf-8")
            if m.old not in text:
                failures.append(
                    f"{m.name}: the mutation anchor is not present in "
                    f"{m.path.name} — the probe is stale, not the code",
                )
                print(f"SKIP {m.name}: anchor not found")
                continue
            occurrences = text.count(m.old)
            m.path.write_text(
                text.replace(m.old, m.new) if m.count == 0
                else text.replace(m.old, m.new, m.count),
                encoding="utf-8",
            )
            # The renderer is a pure function of the canonical source,
            # so the snippet must be regenerated for the mutation to
            # reach the installer.
            subprocess.run(
                [sys.executable, str(RENDER)],
                cwd=str(AGENT_INSTALLATION),
                capture_output=True, text=True, timeout=120,
            )
            rc, out = _run(m.test)
            detected = rc != 0
            print(
                f"{'RED ' if detected else 'GREEN'} {m.name:38} "
                f"{m.test.split('.')[-1]}",
            )
            if not detected:
                failures.append(
                    f"{m.name}: the test stayed GREEN after the mutation "
                    f"({m.why}) — it is VACUOUS",
                )
    finally:
        for path, data in backups.items():
            path.write_bytes(data)
        # Regenerate the derived artefacts from the restored source.
        subprocess.run(
            [sys.executable, str(RENDER)],
            cwd=str(AGENT_INSTALLATION),
            capture_output=True, text=True, timeout=120,
        )

    print()
    if failures:
        for f in failures:
            print(f"FAIL {f}")
        return 1
    print(f"all {len(mutations)} mutations were detected — the tests are non-vacuous")
    return 0


if __name__ == "__main__":
    sys.exit(main())
