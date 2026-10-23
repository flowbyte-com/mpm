"""
test_git_history_check.py — Regression coverage for the PATH_ARG regex
defect in scripts/git-history-check (2026-10-02 scripts audit).

Defect
------

  scripts/git-history-check spliced the caller's path straight into an
  extended regular expression:

      git rev-list --objects "$ref" \
          | grep -E "\\b${PATH_ARG}\\$"

  A filesystem path is not a regex, so every metacharacter in it was
  live. Two consequences, both in the false-negative direction for a
  tool whose verdict gates a history scrub:

  1. A path containing a character that makes the pattern invalid —
     e.g. `a(b)/c.txt`, where `(` opens a group — makes grep exit
     non-zero. The `if` then takes its FALSE branch and the file is
     reported NOT REACHABLE with exit 0, which the header documents
     as "the clean answer". Reproduced in a scratch repo where the
     file genuinely existed in history:

         PATH_ARG='a(b)/c.txt'
         old (grep -E)    : NOT REACHABLE   <-- false negative
         fixed (grep -F)  : REACHABLE       <-- correct

  2. A path containing `.` matched any character, so querying for
     `secret.key` also matched a file named `secretXkey`.

The sibling content branch in the same file already used `grep -qF --`
correctly; only the path branch was wrong.

Scope note
----------

  git-history-check hardcodes `cd "$PROJECT_ROOT"`, so it audits MPM's
  own history and cannot be pointed at an arbitrary repository. These
  tests therefore exercise the two `grep` forms directly rather than
  driving the script, and pin the script's source so the regressed form
  cannot return. Driving the script end-to-end against a fixture repo
  would require either changing that `cd` (out of scope here) or
  running it against the real repository's history.

Fixture lifecycle
-----------------

  The synthetic Git repository lives in a `mpm-history-check.*`
  directory under the system temp directory. Its removal is registered
  the moment the directory exists — before the repository is built — so
  a failure part-way through setup is covered too. The cleanup refuses
  to remove anything that is not such a fixture, which is what keeps it
  from ever being pointed at the real repository, and a removal that
  cannot succeed is raised rather than swallowed.
"""

from __future__ import annotations

import os
import shutil
import subprocess
import tempfile
import unittest
from pathlib import Path
from unittest import mock

REPO_ROOT = Path(__file__).resolve().parents[2]
SCRIPT = REPO_ROOT / "scripts" / "git-history-check"

# Every fixture this module creates carries this prefix. The cleanup
# refuses to remove anything that does not, which is what keeps it
# from ever being pointed at the real repository.
FIXTURE_PREFIX = "mpm-history-check."


class FixtureCleanupError(RuntimeError):
    """Raised when a test-owned fixture could not be fully removed.

    A leak must never be mistaken for a pass.
    """


def _is_own_fixture(root: Path) -> tuple[bool, str]:
    """Is `root` a fixture this module created?

    Returns (ok, reason). The checks are deliberately strict: the
    name must carry the fixture prefix and the directory must sit
    directly inside the system temp directory. Nothing else — in
    particular, never the repository — may be removed by this module.
    """
    if not root.name.startswith(FIXTURE_PREFIX):
        return False, (
            f"refusing to remove {root}: it is not a {FIXTURE_PREFIX}* fixture"
        )
    try:
        temp_root = Path(tempfile.gettempdir()).resolve()
        parent = root.parent.resolve()
    except OSError as exc:  # pragma: no cover - resolve() rarely fails
        return False, f"refusing to remove {root}: cannot resolve its path ({exc})"
    if parent != temp_root:
        return False, (
            f"refusing to remove {root}: it is not directly inside the "
            f"system temp directory {temp_root}"
        )
    return True, ""


def _remove_tree(root: Path) -> list[str]:
    """Remove a directory tree, reporting every failure.

    The permission walk comes first: a fixture may contain
    directories the current user cannot write to, and those cannot
    have their children unlinked until they are made writable again.
    """
    errors: list[str] = []
    for dirpath, dirnames, filenames in os.walk(str(root), topdown=False):
        try:
            os.chmod(dirpath, 0o700)
        except OSError as exc:
            errors.append(f"chmod {dirpath}: {exc}")
        for name in dirnames + filenames:
            entry = os.path.join(dirpath, name)
            if os.path.islink(entry):
                try:
                    os.unlink(entry)
                except OSError as exc:
                    errors.append(f"unlink symlink {entry}: {exc}")
    try:
        shutil.rmtree(root)
    except OSError as exc:
        errors.append(f"rmtree {root}: {exc}")
    if root.exists():
        errors.append(f"the fixture directory {root} still exists")
    return errors


def _remove_fixture(root: Path) -> list[str]:
    """Remove ONLY the test-owned fixture tree.

    Returns the list of errors; an empty list means the fixture is
    gone. A removal that cannot succeed is reported, never swallowed.
    """
    if not root.exists():
        return []
    ok, reason = _is_own_fixture(root)
    if not ok:
        return [reason]
    return _remove_tree(root)


def _cleanup_fixture(root: Path) -> None:
    """Teardown callable: remove the fixture or raise.

    `addCleanup` ignores return values, so the removal helper's error
    list is converted into an exception here — a leaked fixture must
    surface as a test error rather than as a silent pass.
    """
    errors = _remove_fixture(root)
    if errors:
        raise FixtureCleanupError(
            "could not remove the test fixture:\n  " + "\n  ".join(errors)
        )


def _create_fixture(case: unittest.TestCase) -> Path:
    """Create the test-owned fixture and register its removal.

    The removal is registered the moment the directory exists — before
    the fixture repository is built — so a failure part-way through
    setup still leaves the fixture covered by the teardown.
    """
    tmp = Path(tempfile.mkdtemp(prefix=FIXTURE_PREFIX))
    case.addCleanup(_cleanup_fixture, tmp)
    return tmp


class PathArgIsNotARegex(unittest.TestCase):
    """The path branch must use a fixed-string match."""

    def setUp(self) -> None:
        self.src = SCRIPT.read_text()

    def test_path_branch_does_not_splice_path_arg_into_a_regex(self):
        self.assertNotRegex(
            self.src,
            r'grep -E[^|]*\$\{PATH_ARG\}',
            "git-history-check builds an ERE from a filesystem path; a path "
            "with regex metacharacters changes the meaning of the search",
        )

    def test_path_branch_uses_fixed_string_match(self):
        self.assertRegex(self.src, r'grep -qF -- "\$PATH_ARG"')

    def test_content_branch_still_uses_fixed_string_match(self):
        self.assertRegex(self.src, r'grep -qF -- "\$CONTENT_ARG"')


class FixedStringMatchSemantics(unittest.TestCase):
    """Why -F is required, demonstrated against real git output."""

    def _objects(self, repo: Path, ref: str = "HEAD") -> str:
        r = subprocess.run(
            ["git", "rev-list", "--objects", ref],
            cwd=repo,
            capture_output=True,
            text=True,
            timeout=60,
        )
        return r.stdout

    def _grep(self, pattern: str, text: str, fixed: bool) -> bool:
        args = ["grep", "-qF", "--"] if fixed else ["grep", "-qE"]
        return (
            subprocess.run(
                [*args, pattern],
                input=text,
                capture_output=True,
                text=True,
                timeout=30,
            ).returncode
            == 0
        )

    def setUp(self) -> None:
        # Registered immediately, before the fixture repository is
        # built: a failure part-way through setup still leaves the
        # fixture covered by the teardown.
        self.tmp = _create_fixture(self)
        self.repo = self.tmp / "repo"
        self.repo.mkdir()
        env = {
            "GIT_AUTHOR_NAME": "t",
            "GIT_AUTHOR_EMAIL": "t@example.invalid",
            "GIT_COMMITTER_NAME": "t",
            "GIT_COMMITTER_EMAIL": "t@example.invalid",
            "PATH": "/usr/bin:/bin",
            "HOME": str(self.tmp),
        }
        # A path whose '(' makes the ERE invalid, plus a decoy for the '.' case.
        (self.repo / "a(b)").mkdir()
        (self.repo / "a(b)" / "c.txt").write_text("secret\n")
        # The wildcard case: the repo contains ONLY the decoy. A query for
        # "secret.key" should find nothing at all, because no such file
        # exists — the ERE form "finds" one by treating '.' as a wildcard.
        (self.repo / "secretXkey").write_text("decoy\n")
        subprocess.run(["git", "init", "-q", "-b", "main", "."], cwd=self.repo, env=env, check=True)
        subprocess.run(["git", "add", "-A"], cwd=self.repo, env=env, check=True)
        subprocess.run(["git", "commit", "-qm", "fixture"], cwd=self.repo, env=env, check=True)
        self.objects = self._objects(self.repo)

    def test_fixture_actually_contains_the_metachar_path(self):
        # Guard against a vacuous suite: if the fixture is wrong, every
        # assertion below passes for the wrong reason.
        self.assertIn("a(b)/c.txt", self.objects)
        self.assertIn("secretXkey", self.objects)
        self.assertNotIn("secret.key", self.objects)

    def test_regex_form_misses_a_path_that_exists(self):
        # The false negative this fix removes.
        self.assertFalse(
            self._grep("a(b)/c.txt", self.objects, fixed=False),
            "expected the ERE form to fail to match; fixture or grep changed",
        )

    def test_fixed_string_form_finds_a_path_that_exists(self):
        self.assertTrue(
            self._grep("a(b)/c.txt", self.objects, fixed=True),
            "grep -F must find a path whose name contains regex metacharacters",
        )

    def test_regex_form_matches_the_wrong_file_for_a_dot(self):
        # The queried path carries the dot: "secret.key". The fixture also
        # contains "secretXkey", and the ERE's '.' matches the 'X', so the
        # search reports REACHABLE for a file the caller did not ask about.
        self.assertTrue(
            self._grep("secret.key", self.objects, fixed=False),
            "expected the ERE form to match 'secretXkey' via the '.' wildcard",
        )

    def test_fixed_string_form_does_not_match_the_wrong_file(self):
        # Same query, correct behaviour: no such file exists, so no match.
        self.assertFalse(
            self._grep("secret.key", self.objects, fixed=True),
            "grep -F must not treat '.' as a wildcard",
        )

    def test_fixed_string_form_still_finds_the_exact_path(self):
        self.assertTrue(self._grep("secretXkey", self.objects, fixed=True))


class GitHistoryFixtureLifecycle(unittest.TestCase):
    """The fixture must not survive the test that created it.

    `FixedStringMatchSemantics` builds a synthetic Git repository in a
    temp directory so the fixed-string semantics can be demonstrated
    against real `git rev-list` output. That directory used to have no
    teardown at all, so every run of the module left a
    `mpm-history-check.*` tree behind. These tests pin the lifecycle:
    the fixture is created, it is covered from the moment it exists, and
    it is gone when the test ends.
    """

    def test_the_fixture_is_removed_when_the_test_case_finishes(self):
        """End to end: a real test case runs and its fixture is gone."""
        case = FixedStringMatchSemantics(
            "test_fixed_string_form_still_finds_the_exact_path"
        )
        result = unittest.TestResult()
        case.run(result)
        self.assertEqual(len(result.errors), 0, result.errors)
        self.assertEqual(len(result.failures), 0, result.failures)
        self.assertFalse(
            case.tmp.exists(), "the fixture must not survive the test"
        )

    def test_the_fixture_is_removed_when_the_setup_fails(self):
        """A failure part-way through setup must not leak the fixture.

        The cleanup is registered as soon as the directory exists, so a
        setUp that raises afterwards still has the fixture covered.
        """
        created: dict[str, Path] = {}

        class FailingSetupCase(unittest.TestCase):
            def setUp(self) -> None:
                created["tmp"] = _create_fixture(self)
                raise RuntimeError("deliberate setup failure")

            def runTest(self) -> None:
                pass

        result = unittest.TestResult()
        FailingSetupCase().run(result)

        self.assertEqual(len(result.errors), 1, result.errors)
        self.assertIn("deliberate setup failure", result.errors[0][1])
        self.assertFalse(
            created["tmp"].exists(),
            "a fixture created before a setup failure must still be removed",
        )

    def test_the_cleanup_refuses_a_path_that_is_not_a_fixture(self):
        """The cleanup only ever touches its own fixtures."""
        other = Path(tempfile.gettempdir()) / "mpm-not-a-history-fixture"
        other.mkdir(exist_ok=True)
        self.addCleanup(shutil.rmtree, other, ignore_errors=True)
        (other / "keep.txt").write_text("not a fixture\n")

        errors = _remove_fixture(other)

        self.assertTrue(errors, "a refusal must be reported")
        self.assertIn(FIXTURE_PREFIX, errors[0])
        self.assertTrue(other.exists(), "a refused path must not be removed")
        self.assertTrue((other / "keep.txt").exists())

    def test_the_cleanup_refuses_a_fixture_outside_the_temp_directory(self):
        """Even a correctly-named path is refused outside $TMPDIR."""
        holder = Path(tempfile.gettempdir()) / "mpm-history-check-holder"
        holder.mkdir(exist_ok=True)
        self.addCleanup(shutil.rmtree, holder, ignore_errors=True)
        nested = holder / (FIXTURE_PREFIX + "nested")
        nested.mkdir(exist_ok=True)

        errors = _remove_fixture(nested)

        self.assertTrue(errors, "a refusal must be reported")
        self.assertIn("system temp directory", errors[0])
        self.assertTrue(nested.exists(), "a refused path must not be removed")

    def test_a_removal_that_cannot_succeed_is_reported(self):
        """A removal that cannot succeed must be REPORTED, with its
        path — never swallowed into a silent pass."""
        fixture = Path(tempfile.mkdtemp(prefix=FIXTURE_PREFIX))
        self.addCleanup(shutil.rmtree, fixture, ignore_errors=True)
        (fixture / "file.txt").write_text("x\n")

        def failing_rmtree(path, *args, **kwargs):
            raise PermissionError(13, "Permission denied", str(path))

        with mock.patch.object(shutil, "rmtree", failing_rmtree):
            errors = _remove_fixture(fixture)

        self.assertTrue(
            errors, "a removal that cannot succeed must be reported"
        )
        self.assertTrue(
            any(str(fixture) in err for err in errors),
            f"the error must name the path that could not be removed: {errors}",
        )
        self.assertTrue(fixture.exists())

    def test_the_teardown_raises_rather_than_swallowing_a_failure(self):
        """`addCleanup` ignores return values, so the teardown has to
        raise for a leak to surface as a test error."""
        fixture = Path(tempfile.mkdtemp(prefix=FIXTURE_PREFIX))
        self.addCleanup(shutil.rmtree, fixture, ignore_errors=True)

        def failing_rmtree(path, *args, **kwargs):
            raise PermissionError(13, "Permission denied", str(path))

        with mock.patch.object(shutil, "rmtree", failing_rmtree):
            with self.assertRaises(FixtureCleanupError):
                _cleanup_fixture(fixture)


if __name__ == "__main__":
    unittest.main()
