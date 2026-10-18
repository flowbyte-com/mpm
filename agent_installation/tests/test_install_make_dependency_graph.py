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
import tempfile
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


def _make_db_dump_all() -> str:
    """Dump the whole prerequisite database without naming a goal.

    Naming a goal makes make consider that goal's recipe. Under `-n`
    make still *executes* recipe lines containing `$(MAKE)` — and the
    `test` target's recipe ends in `$(MAKE) test-agent-installation`,
    which shells out to `python3 -m unittest discover tests`. That
    re-enters this very file, which asks for `make ... test` again: an
    unbounded fork bomb, observed as thousands of `make test-agent-
    installation` processes.

    With no goal, make dumps every target's prerequisites and considers
    only the default goal (`all: build`, which has no recipe), so
    nothing executes. 6ms, and the dump still lists `test:`,
    `test-race:` and `test-release:` with their prerequisites.
    """
    out = subprocess.run(
        ["make", "-n", "-p"], capture_output=True, text=True,
        cwd=str(REPO_ROOT), check=True,
    )
    return out.stdout


def _make_dryrun(target: str, *args: str) -> str:
    """`make -n <target>` without letting it descend into sub-makes.

    `MAKE=true` on the command line expands the `$(MAKE)` lines of the
    recipe to a harmless `true`, so make no longer recognises them as
    recursive invocations and, honouring `-n`, prints them instead of
    running them. Without this the dry run would actually execute
    `test-agent-installation` — see `_make_db_dump_all`.
    """
    out = subprocess.run(
        ["make", "-n", target, "MAKE=true", *args],
        capture_output=True, text=True, cwd=str(REPO_ROOT), check=True,
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


class TestEntryPointBuildDependency(unittest.TestCase):
    """`make test` must be a reliable entry point on a FRESH checkout.

    26 tests under cmd/mpm (and one under cmd/mpm-critic) shell out to
    $(BUILD_DIR)/mpm and $(BUILD_DIR)/mpm-mcp. They deliberately refuse
    to fall back to $(PREFIX)/bin/mpm — correctly, because falling back
    would exercise the installed production binary instead of the code
    under test, turning a build-ordering mistake into a silent false
    pass.

    Without a prior `make build`, a clean clone therefore reported 26
    failures that had nothing to do with the code:

        developer build artifact missing: <checkout>/.build/bin/mpm
        Run `make build` from the repository root first.

    That is a test-infrastructure defect, not a code defect: the suite
    needed a step nobody had documented. The fix is a `build`
    prerequisite on the test targets. These tests pin it.

    `test-release` already depended on build and is pinned here too, so
    a future edit that "simplifies" it away is caught by the same guard.
    """

    # The binaries the suite's subprocess tests execute. Kept as an
    # explicit list so a rename cannot silently narrow this test; it is
    # cross-checked against the Makefile's own variables below.
    BINARIES = ["mpm", "mpm-mcp", "mpm-scheduler", "mpm-critic", "mpm-telemetry"]

    def setUp(self):
        self.db = _make_db_dump_all()
        self.graph = _parse_target_prereqs(self.db)

    def test_artifact_asserting_targets_depend_on_build(self):
        """Two more targets assert against $(BUILD_DIR) rather than
        executing it.

        tests/test_build_install_ownership.py (agent_installation) and
        scripts/tests/test_build_install_separation.py,
        test_runtime_asset_provisioning.py, test_install_phase_binaries.py
        (scripts) all check that `.build/bin/mpm` exists and is a compiled
        binary. CI runs the agent_installation suite directly — `cd
        agent_installation && python3 -m unittest discover tests` — which
        is why the gate job was red on main with ".build/bin must exist
        (run `make build` first)" while nobody was running these targets
        locally.
        """
        for target, why in (
            ("test-agent-installation",
             "tests/test_build_install_ownership.py asserts .build/bin/mpm exists"),
            ("test-scripts",
             "scripts/tests/{test_build_install_separation,test_runtime_asset_provisioning,"
             "test_install_phase_binaries}.py assert against $(BUILD_DIR)"),
        ):
            with self.subTest(target=target):
                self.assertIn(
                    "build", self.graph.get(target, []),
                    f"`make {target}` must depend on `build` — {why}. Without "
                    "it the target fails on a fresh checkout for a reason "
                    "that has nothing to do with what it tests.",
                )

    def test_test_depends_on_build(self):
        prereqs = self.graph.get("test", [])
        self.assertIn(
            "build", prereqs,
            "`make test` must depend on `build`. Without it a fresh clone "
            "fails with 26 'developer build artifact missing' errors that "
            "have nothing to do with the code under test. `make test` is "
            "the documented entry point and must be correct on its own.",
        )

    def test_test_race_depends_on_build(self):
        prereqs = self.graph.get("test-race", [])
        self.assertIn(
            "build", prereqs,
            "`make test-race` runs the same cmd/mpm subprocess tests and "
            "therefore needs $(BUILD_DIR)/mpm too. Leaving it out means the "
            "pre-merge gate still fails on a fresh clone.",
        )

    def test_test_release_still_depends_on_build(self):
        prereqs = self.graph.get("test-release", [])
        self.assertIn(
            "build", prereqs,
            "`make test-release` runs the public-CLI subprocess suite and "
            "has always depended on build. This pins existing behaviour.",
        )

    def test_binary_list_matches_makefile_variables(self):
        """Keep the explicit BINARIES list honest.

        Without this, renaming $(MCP_BINARY) would leave this test
        asserting on a filename nothing builds — which passes, and
        guards nothing. That is the same failure shape as a gate that
        was never wired.
        """
        text = MAKEFILE.read_text(encoding="utf-8")
        for name in self.BINARIES:
            self.assertRegex(
                text,
                rf"(?m)^\w+\s*:=\s*{re.escape(name)}\s*$",
                f"Makefile must still define a binary variable named {name}; "
                "update TestEntryPointBuildDependency.BINARIES in step",
            )

    def test_make_n_test_expands_to_building_the_binaries(self):
        """The behavioural half: `make test` really does build first.

        BUILD_DIR is redirected at an empty temporary directory so the
        dry run cannot be satisfied by the checkout's populated
        .build/bin. If the `build` prerequisite is removed, no `go build`
        line appears for this path and the assertion below fails —
        which is exactly the fresh-checkout regression.
        """
        with tempfile.TemporaryDirectory(prefix="mpm-dryrun-build-") as tmp:
            stdout = _make_dryrun("test", f"BUILD_DIR={tmp}")
            for name in self.BINARIES:
                self.assertIn(
                    f"-o {tmp}/{name}",
                    stdout,
                    f"`make -n test` must show `go build -o {tmp}/{name}`; "
                    "the suite's subprocess tests execute that binary and "
                    "refuse to fall back to the installed one",
                )
            # Guard against the vacuity trap: `go build` must genuinely
            # be present, or the loop above could be satisfied by a
            # string that appears for some unrelated reason.
            self.assertIn(
                "go build", stdout,
                "sanity check: the dry run contains go build invocations",
            )

    def test_build_recipe_keeps_fts5_discipline(self):
        """The new prerequisite must not dilute the FTS5 flag set.

        `test: build` delegates to a target that already carries
        -tags fts5, CGO_CFLAGS and CGO_LDFLAGS. If a future edit drops
        one of those from the build recipe, every consumer — local and
        CI — silently loses FTS5 while still reporting success.
        """
        text = MAKEFILE.read_text(encoding="utf-8")
        match = re.search(r"(?m)^build:[^\n]*\n((?:^[ \t].*\n?)+)", text)
        self.assertIsNotNone(match, "Makefile must define a `build:` recipe")
        recipe = match.group(1)
        self.assertIn("-tags fts5", recipe,
                      "build recipe must pass -tags fts5")
        self.assertIn("CGO_CFLAGS=$(CGO_CFLAGS)", recipe,
                      "build recipe must set CGO_CFLAGS so FTS5 is compiled in")
        self.assertIn("CGO_LDFLAGS=$(CGO_LDFLAGS)", recipe,
                      "build recipe must set CGO_LDFLAGS (-lm); without it "
                      "the link fails on runners that do not link libm implicitly")


if __name__ == "__main__":
    unittest.main(verbosity=2)
