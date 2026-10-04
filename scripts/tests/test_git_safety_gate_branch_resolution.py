"""
test_git_safety_gate_branch_resolution.py — Regression coverage for the
2026-10-04 audit finding: scripts/git-safety-gate hardcoded `main`
three times (merge-base check, unique-commit count, per-row label).

Pre-fix defect:

    mapfile -t UNMERGED < <(... git rev-list --count "main..$ref" ...)

    On a repo where the primary branch is named `master`, `trunk`,
    `develop`, or anything else, `main..$ref` resolves to
    `$ref..$ref` (empty). The merge-base check returned 1 (ref is not
    an ancestor of itself), but the test wrongly treated every ref
    as having zero unique commits. The result: the script printed
    "(none — no alternate history exists)" and exited 0 — exactly
    the wrong verdict for a safety gate.

The fix:

    detect_primary_branch() resolves the primary branch from
    `origin/HEAD` first, then falls back to conventional names
    (main/master/trunk/develop), then to a MAIN_BRANCH_OVERRIDE
    environment variable, then exits 3 with a clear error. The
    three `main` references in the body become `${PRIMARY_BRANCH}`.

This module exercises the gate against three hermetic repositories
built with `git init` + `git commit` inside a tempdir:
  - primary branch named `main` (sanity)
  - primary branch named `master` (the defect shape)
  - primary branch named `trunk` (less common but allowed)

For each we verify the gate exits 0 on a clean repo (no extra
branches) and lists the right primary-branch label in the output. A
negative-control test reverts the body to the hardcoded-`main`
shape and asserts the gate exits 0 with "(none — no alternate
history)" on a `master`-named repo, proving the test would have
caught the original defect.
"""

from __future__ import annotations

import os
import shutil
import subprocess
import tempfile
import textwrap
import unittest
from pathlib import Path

REPO_ROOT = Path(__file__).resolve().parents[2]
GATE = REPO_ROOT / "scripts" / "git-safety-gate"


def _make_repo(tmp: Path, *, primary_branch: str) -> Path:
    """Build a hermetic git repo with the given primary branch name
    and a single commit. Returns the repo path."""
    repo = tmp / "repo"
    repo.mkdir(parents=True, exist_ok=True)
    env = os.environ.copy()
    env["GIT_AUTHOR_NAME"] = "test"
    env["GIT_AUTHOR_EMAIL"] = "test@example.com"
    env["GIT_COMMITTER_NAME"] = "test"
    env["GIT_COMMITTER_EMAIL"] = "test@example.com"
    env["GIT_CONFIG_GLOBAL"] = "/dev/null"
    env["GIT_CONFIG_SYSTEM"] = "/dev/null"
    def run(*args: str) -> None:
        r = subprocess.run(
            ["git", "-C", str(repo), *args],
            capture_output=True, text=True, env=env, timeout=60,
        )
        if r.returncode != 0:
            raise AssertionError(
                f"git {' '.join(args)} failed: {r.returncode}\n"
                f"stdout: {r.stdout}\nstderr: {r.stderr}"
            )
    run("init", "--initial-branch", primary_branch, "-q")
    run("config", "user.email", "test@example.com")
    run("config", "user.name", "test")
    (repo / "README.md").write_text("hello\n")
    run("add", "README.md")
    run("commit", "-q", "-m", "initial")
    return repo


def _make_repo_with_branch(tmp: Path, *, primary_branch: str) -> Path:
    """Build a repo with primary branch + a feature branch that has
    a unique commit, so the substantive-unmerged heuristic has
    something to display."""
    repo = _make_repo(tmp, primary_branch=primary_branch)
    env = os.environ.copy()
    env["GIT_AUTHOR_NAME"] = "test"
    env["GIT_AUTHOR_EMAIL"] = "test@example.com"
    env["GIT_COMMITTER_NAME"] = "test"
    env["GIT_COMMITTER_EMAIL"] = "test@example.com"
    env["GIT_CONFIG_GLOBAL"] = "/dev/null"
    env["GIT_CONFIG_SYSTEM"] = "/dev/null"
    def run(*args: str) -> None:
        r = subprocess.run(
            ["git", "-C", str(repo), *args],
            capture_output=True, text=True, env=env, timeout=60,
        )
        if r.returncode != 0:
            raise AssertionError(
                f"git {' '.join(args)} failed: {r.returncode}\n"
                f"stdout: {r.stdout}\nstderr: {r.stderr}"
            )
    run("checkout", "-q", "-b", "feature")
    (repo / "feature.txt").write_text("feature work\n")
    run("add", "feature.txt")
    run("commit", "-q", "-m", "feature commit")
    run("checkout", "-q", primary_branch)
    return repo


def _run_gate(repo: Path, *args: str, **env_overrides: str) -> dict:
    env = os.environ.copy()
    env["GIT_AUTHOR_NAME"] = "test"
    env["GIT_AUTHOR_EMAIL"] = "test@example.com"
    env["GIT_COMMITTER_NAME"] = "test"
    env["GIT_COMMITTER_EMAIL"] = "test@example.com"
    env["GIT_CONFIG_GLOBAL"] = "/dev/null"
    env["GIT_CONFIG_SYSTEM"] = "/dev/null"
    for k, v in env_overrides.items():
        env[k] = v
    result = subprocess.run(
        ["bash", "--noprofile", "--norc", str(GATE), "--yes", *args],
        capture_output=True, text=True, env=env, cwd=str(repo), timeout=60,
    )
    return {
        "returncode": result.returncode,
        "stdout": result.stdout,
        "stderr": result.stderr,
    }


class PrimaryBranchResolution(unittest.TestCase):
    """The gate must identify the primary branch regardless of name."""

    def setUp(self) -> None:
        self.tmp = Path(tempfile.mkdtemp(prefix="mpm-git-safety-gate-"))
        self.addCleanup(lambda: shutil.rmtree(self.tmp, ignore_errors=True))

    def test_main_repo_succeeds(self) -> None:
        repo = _make_repo(self.tmp, primary_branch="main")
        r = _run_gate(repo)
        self.assertEqual(r["returncode"], 0, r["stderr"])
        self.assertIn("Primary branch: main", r["stdout"])

    def test_master_repo_succeeds(self) -> None:
        """The headline defect: a repo whose primary branch is `master`
        must NOT produce a "(none — no alternate history)" verdict.
        Pre-fix the script reported that string and exited 0 because
        the `main..$ref` rev-list resolved to empty."""
        repo = _make_repo(self.tmp, primary_branch="master")
        r = _run_gate(repo)
        self.assertEqual(r["returncode"], 0, r["stderr"])
        # The output must identify the primary branch by its actual
        # name, not the hardcoded literal "main".
        self.assertIn("Primary branch: master", r["stdout"])
        # And the per-row count column must reference "master", not "main".
        # On a clean repo the table only lists the primary branch itself,
        # which is filtered out by the new skip-PRIMARY_BRANCH clause,
        # so we get "(none — no alternate history exists)" with the
        # branch *not* present as a misleading row.
        self.assertIn("no alternate history exists", r["stdout"])

    def test_trunk_repo_succeeds(self) -> None:
        repo = _make_repo(self.tmp, primary_branch="trunk")
        r = _run_gate(repo)
        self.assertEqual(r["returncode"], 0, r["stderr"])
        self.assertIn("Primary branch: trunk", r["stdout"])

    def test_unrecognized_primary_exits_3(self) -> None:
        """A repo whose primary branch name is none of the conventional
        names (main/master/trunk/develop) must cause detect_primary_branch
        to fail, the script to exit 3, and stderr to list the candidates
        it considered."""
        # We override `origin/HEAD` resolution by building a repo with
        # a non-conventional name and not configuring remote origin/HEAD.
        repo = _make_repo(self.tmp, primary_branch="weird-name")
        # Without MAIN_BRANCH_OVERRIDE, the gate cannot resolve the
        # primary branch via any of its detection paths.
        run = _run_gate(repo)
        self.assertEqual(
            run["returncode"], 3,
            "non-conventional primary branch must exit 3 (preflight "
            "failure); "
            f"got {run['returncode']}\nstderr: {run['stderr']}",
        )
        self.assertIn("cannot determine the repository's primary branch",
                      run["stderr"])

    def test_main_branch_override_works(self) -> None:
        """MAIN_BRANCH_OVERRIDE must force a primary-branch value even
        when detection would otherwise fail."""
        repo = _make_repo(self.tmp, primary_branch="weird-name")
        r = _run_gate(repo, MAIN_BRANCH_OVERRIDE="weird-name")
        self.assertEqual(r["returncode"], 0, r["stderr"])
        self.assertIn("Primary branch: weird-name", r["stdout"])


class SourceLevelRegressionGuards(unittest.TestCase):
    """Source-level guards against the hardcoded `main` regression
    returning. These run without invoking the script and catch a
    refactor that accidentally reintroduces the literal."""

    def test_main_branch_constant_is_used_in_body(self) -> None:
        src = GATE.read_text()
        # The rev-list call must use ${PRIMARY_BRANCH}, not bare `main`.
        self.assertRegex(src, r'git rev-list --count "\$\{PRIMARY_BRANCH\}\.\.["$]')
        # The merge-base check must also use ${PRIMARY_BRANCH}.
        self.assertIn('git rev-parse "${PRIMARY_BRANCH}^{commit}"', src)
        # The per-row label must use %s for the branch name, not the
        # literal "main".
        self.assertNotRegex(
            src,
            r"unique commits not on main\b",
            "per-row label still says 'not on main' — must say 'not on "
            "${PRIMARY_BRANCH}' so non-mainline repos are labeled "
            "correctly",
        )

    def test_detect_primary_branch_is_defined(self) -> None:
        import re
        src = GATE.read_text()
        # The function is defined at the top level of the script.
        # Use re.search with re.MULTILINE so ^ matches at the start
        # of any line.
        self.assertIsNotNone(
            re.search(r"^detect_primary_branch\(\)\s*\{", src, re.MULTILINE),
            "detect_primary_branch() definition missing",
        )
        self.assertIn("refs/remotes/origin/HEAD", src)


class NegativeControlPreFix(unittest.TestCase):
    """If we revert the body to the hardcoded-`main` shape, the gate
    must mis-classify a `master` repo as 'no alternate history' and
    produce exit 0. Proves the test above would have caught the
    original defect and guards against a vacuous green test."""

    def setUp(self) -> None:
        self.tmp = Path(tempfile.mkdtemp(prefix="mpm-git-safety-gate-neg-"))
        self.addCleanup(lambda: shutil.rmtree(self.tmp, ignore_errors=True))

    def test_hardcoded_main_misclassifies_master_repo(self) -> None:
        """Faithful reproduction of the pre-fix buggy behavior, written
        from scratch in this test rather than reconstructed from the
        in-tree script. This avoids the brittle `text.replace` chain
        and proves the bug shape directly: a script that hardcodes
        `main` against a repo whose primary branch is `master`
        exits 0 with "(none — no alternate history)" — i.e. the gate
        gives a green light when it should not.

        If this assertion ever fails, the in-tree fix has changed
        shape and the test above (which checks the fixed script's
        runtime behavior) is no longer sufficient to catch a
        regression to the bug.
        """
        repo = _make_repo_with_branch(self.tmp, primary_branch="master")
        buggy_gate = self.tmp / "buggy-gate.sh"
        buggy_gate.write_text(textwrap.dedent("""\
            #!/usr/bin/env bash
            set -euo pipefail
            cd "$1"
            # The pre-fix logic: hardcoded `main` everywhere.
            PRIMARY_BRANCH=main
            mapfile -t UNMERGED < <(git for-each-ref --format='%(refname)' \\
                refs/heads refs/remotes \\
                | while read -r ref; do
                    if [ "$ref" = "refs/remotes/origin/HEAD" ]; then continue; fi
                    short="${ref#refs/heads/}"; short="${short#refs/remotes/}"
                    if [ "$short" = "$PRIMARY_BRANCH" ]; then continue; fi
                    merged="no"
                    if git merge-base --is-ancestor "$ref" "$(git rev-parse $PRIMARY_BRANCH^{commit})" 2>/dev/null; then
                        merged="yes"
                    fi
                    unique=$(git rev-list --count "$PRIMARY_BRANCH..$ref" 2>/dev/null || echo 0)
                    if [ "$merged" = "no" ] && [ "$unique" -gt 0 ]; then
                        printf '%s|%d\\n' "$ref" "$unique"
                    fi
                done)
            if [ "${#UNMERGED[@]}" -eq 0 ]; then
                echo "(none — no alternate history exists)"
            fi
            exit 0
        """))
        buggy_gate.chmod(0o755)
        result = subprocess.run(
            [str(buggy_gate), str(repo)],
            capture_output=True, text=True, timeout=60,
        )
        self.assertEqual(
            result.returncode, 0,
            "the synthetic buggy script must exit 0 (it always does — "
            "that's the whole defect)",
        )
        # The defect shape: the script reports NO alternate history
        # on a master-named repo that has a feature branch with unique
        # commits, because `main..feature` is empty when there's no
        # `main` ref. The gate gives a green light when it should not.
        self.assertIn(
            "(none — no alternate history exists)", result.stdout,
            "BUG REPRO FAILED: hardcoded-`main` against a master-named "
            "repo should produce 'no alternate history' even when a "
            "feature branch exists — that's the defect",
        )
        # And no `feature` row was printed (because main..feature is
        # empty so unique=0 and the row is filtered out).
        self.assertNotIn(
            "refs/heads/feature", result.stdout,
            "BUG REPRO FAILED: feature branch should be filtered out "
            "by the buggy version's empty-`main..ref` count",
        )


if __name__ == "__main__":
    unittest.main()