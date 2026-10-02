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
"""

from __future__ import annotations

import subprocess
import tempfile
import unittest
from pathlib import Path

REPO_ROOT = Path(__file__).resolve().parents[2]
SCRIPT = REPO_ROOT / "scripts" / "git-history-check"


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
        self.tmp = Path(tempfile.mkdtemp(prefix="mpm-history-check."))
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


if __name__ == "__main__":
    unittest.main()
