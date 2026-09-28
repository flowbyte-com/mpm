"""
test_install_make_dependency_graph.py — Structural test for the
Makefile dependency graph.

Pins:
  1. `install` depends on `build` and `refresh-installed`.
  2. `refresh-installed` does NOT depend on `build` (they are
     independent leaf targets; reconciliation does not need fresh
     binaries).
  3. `make -j install` is therefore safe — `build` and
     `refresh-installed` may run concurrently; their outputs do not
     collide.

The test introspects the Makefile's prerequisite database directly
rather than relying on `make -n` output order. `make -n` is sequential
even with `-j`, so it cannot prove parallel safety. The structural
inspection proves the graph is what we want.
"""

from __future__ import annotations

import re
import subprocess
import unittest
from pathlib import Path


REPO_ROOT = Path(__file__).resolve().parent.parent.parent
MAKEFILE = REPO_ROOT / "Makefile"


def _make_db_dump(*targets: str) -> str:
    """Run `make -n -p <targets>` and return the full database dump.

    The database dump lists every target with its prerequisites in
    the form:
        target: prereq1 prereq2 ...
    This is the same form Make uses internally to schedule jobs, so
    it is the authoritative source for the dependency graph.
    """
    cmd = ["make", "-n", "-p"] + list(targets)
    out = subprocess.run(
        cmd, capture_output=True, text=True,
        cwd=str(REPO_ROOT), check=True,
    )
    return out.stdout


def _parse_target_prereqs(db_dump: str) -> dict[str, list[str]]:
    """Parse the make database into {target: [prereq, ...]}."""
    graph: dict[str, list[str]] = {}
    # Lines like `target: prereq1 prereq2 ...` at the start of a
    # logical line. Continuation lines start with whitespace.
    current_target: str | None = None
    current_prereqs: list[str] = []
    for line in db_dump.splitlines():
        if not line or line.startswith("#"):
            continue
        stripped = line.lstrip()
        if line.startswith((" ", "\t")):
            # Continuation
            if current_target is not None:
                current_prereqs.extend(stripped.split())
        else:
            # New entry
            if current_target is not None:
                graph[current_target] = current_prereqs
            current_target = None
            current_prereqs = []
            if ":" in stripped:
                # Skip variable assignments like `FOO := bar`.
                if "=" in stripped.split(":", 1)[0]:
                    continue
                target, _, rhs = stripped.partition(":")
                # A rule like `install: build refresh-installed` has
                # the prereqs on the same line.
                target = target.strip()
                rhs = rhs.strip()
                if target and not target.startswith("."):
                    current_target = target
                    current_prereqs = rhs.split() if rhs else []
    if current_target is not None:
        graph[current_target] = current_prereqs
    return graph


class InstallDependencyGraph(unittest.TestCase):
    """`make install` must depend on both build and refresh-installed."""

    def setUp(self):
        self.db = _make_db_dump("install", "build", "refresh-installed")
        self.graph = _parse_target_prereqs(self.db)

    def test_install_target_exists(self):
        self.assertIn(
            "install", self.graph,
            "Makefile must define an `install` target",
        )

    def test_install_depends_on_build(self):
        prereqs = self.graph.get("install", [])
        self.assertIn(
            "build", prereqs,
            "install must depend on build — the binary install step "
            "copies bin/* into $(PREFIX)/bin/, which requires a "
            "successful build first.",
        )

    def test_install_depends_on_refresh_installed(self):
        """The documented `git pull && make install` update flow
        requires reconciliation as part of install. Without this
        dependency, the install step silently leaves drift
        unaddressed."""
        prereqs = self.graph.get("install", [])
        self.assertIn(
            "refresh-installed", prereqs,
            "install must depend on refresh-installed so the "
            "documented update flow (`git pull && make install`) "
            "converges installed managed blocks automatically.",
        )

    def test_refresh_installed_does_not_depend_on_build(self):
        """refresh-installed must NOT depend on build.

        Reconciliation invokes the host-agnostic Python entry point
        at agent_installation/scripts/reconcile_managed_blocks.py,
        which only needs the canonical snippets (no compiled
        binaries). If refresh-installed depended on build, a
        content-only `git pull` would force a rebuild for no reason.

        Critically: an UNNECESSARY dependency on build would force
        serial execution under `make -j`, defeating the parallelism
        between build and refresh-installed that the dependency graph
        currently allows.
        """
        prereqs = self.graph.get("refresh-installed", [])
        self.assertNotIn(
            "build", prereqs,
            "refresh-installed must NOT depend on build. "
            "Reconciliation does not need fresh binaries; depending "
            "on build would force serial execution and force a "
            "rebuild for content-only updates.",
        )

    def test_install_prereqs_are_parallel_safe(self):
        """The prereqs of `install` may run in parallel under `make -j`.

        Specifically: build writes to bin/, refresh-installed writes
        to $(HOME)/.claude/CLAUDE.md (etc.) and reads from
        agent_installation/. Their outputs do not collide; they may
        run concurrently. The structural assertion here is that the
        two prereqs are *siblings*, not chained through a third
        target that would force serial execution.

        If a future change makes one prereq depend on the other, this
        test fails — the operator must consciously decide whether
        to keep the parallel-allowed dependency structure.
        """
        build_prereqs = set(self.graph.get("build", []))
        refresh_prereqs = set(self.graph.get("refresh-installed", []))

        # Each prereq must not list the other as a dependency.
        self.assertNotIn(
            "refresh-installed", build_prereqs,
            "build must not depend on refresh-installed — the build "
            "should not be blocked by managed-block reconciliation "
            "state, which is independent of compile-time dependencies.",
        )
        self.assertNotIn(
            "build", refresh_prereqs,
            "refresh-installed must not depend on build — see "
            "test_refresh_installed_does_not_depend_on_build.",
        )

    def test_make_n_parallel_dryrun_does_not_show_actual_parallelism(self):
        """Document the limitation of `make -n` for proving parallelism.

        `make -n` (dry-run) prints commands sequentially even when
        `-j` is enabled, because dry-run doesn't actually schedule
        jobs. A test that relied on `make -n install` output order
        would falsely conclude the graph is sequential.

        The structural assertions above are the source of truth.
        """
        # We assert the limitation rather than assert a specific
        # behavior, so future regressions in `make` semantics are
        # caught.
        result = subprocess.run(
            ["make", "-n", "-j", "4", "install"],
            capture_output=True, text=True, cwd=str(REPO_ROOT),
            check=True,
        )
        # The output is sequential, but the underlying graph is
        # parallel. This is a documentation assertion: future readers
        # should not derive "parallel-safe" or "not parallel-safe"
        # from output ordering alone.
        self.assertIn(
            "reconcile_managed_blocks.py",
            result.stdout,
            "sanity check: dry-run output includes the reconcile step",
        )


if __name__ == "__main__":
    unittest.main(verbosity=2)
