"""Contract tests for scripts/build-provenance.sh.

The build provenance is the ONE convention that stamps the product
version and the source commit into every MPM binary. It has to answer
four questions identically for a developer build, a CI build and the
build install.sh performs:

  1. what version is this?
  2. which commit was it built from?
  3. was the tree dirty?
  4. is this a formally tagged release?

These tests build throwaway git repositories, copy the real script into
them (so the script's "repo root is where I live" resolution is
exercised, not a special test-only code path) and assert the answers.

Nothing here touches the live checkout, the live install or the live
database: every repository is created under a temporary directory.
"""

import os
import shutil
import subprocess
import tempfile
import unittest

REPO_ROOT = os.path.dirname(os.path.dirname(os.path.dirname(os.path.abspath(__file__))))
SCRIPT_REL = os.path.join("scripts", "build-provenance.sh")
SCRIPT_SRC = os.path.join(REPO_ROOT, SCRIPT_REL)

# The linker package path the stamp must target. Every MPM binary links
# this package, so `-X` against it is what actually reaches the binary.
BUILDINFO_PKG = "github.com/flowbyte-com/mpm/internal/buildinfo"


def run_script(script_path, *args, cwd=None):
    """Run the provenance script and return its stdout stripped."""
    proc = subprocess.run(
        [script_path, *args],
        cwd=cwd,
        stdout=subprocess.PIPE,
        stderr=subprocess.PIPE,
        text=True,
        check=False,
    )
    if proc.returncode != 0:
        raise AssertionError(
            "build-provenance.sh %s failed (%d): %s"
            % (" ".join(args), proc.returncode, proc.stderr)
        )
    return proc.stdout.strip()


def git(repo, *args):
    subprocess.run(
        ["git", *args],
        cwd=repo,
        stdout=subprocess.PIPE,
        stderr=subprocess.PIPE,
        text=True,
        check=True,
    )


class ProvenanceRepo:
    """A throwaway git repository with the real script installed in it."""

    def __init__(self, version="0.1.0-dev"):
        self.root = tempfile.mkdtemp(prefix="mpm-provenance-")
        self.repo = os.path.join(self.root, "repo")
        os.makedirs(os.path.join(self.repo, "scripts"))
        shutil.copy(SCRIPT_SRC, os.path.join(self.repo, SCRIPT_REL))
        self.script = os.path.join(self.repo, SCRIPT_REL)
        os.chmod(self.script, 0o755)

        git(self.repo, "init", "-q", "-b", "main")
        git(self.repo, "config", "user.email", "provenance@example.invalid")
        git(self.repo, "config", "user.name", "Provenance Test")
        self.write("VERSION", version + "\n")
        self.write("main.go", "package main\n")
        self.commit("initial")

    def write(self, relpath, content):
        path = os.path.join(self.repo, relpath)
        os.makedirs(os.path.dirname(path), exist_ok=True)
        with open(path, "w", encoding="utf-8") as handle:
            handle.write(content)

    def commit(self, message):
        git(self.repo, "add", "-A")
        git(self.repo, "commit", "-q", "-m", message)

    def head_sha(self):
        return run_script(self.script, "commit")

    def cleanup(self):
        shutil.rmtree(self.root, ignore_errors=True)


class TestVersion(unittest.TestCase):
    def setUp(self):
        self.prov = ProvenanceRepo(version="1.2.3")
        self.addCleanup(self.prov.cleanup)

    def test_version_comes_from_the_version_file(self):
        self.assertEqual(run_script(self.prov.script, "version"), "1.2.3")

    def test_version_file_is_the_single_authoritative_source(self):
        # Changing the VERSION file changes the reported version; nothing
        # else does.
        self.prov.write("VERSION", "2.0.0\n")
        self.assertEqual(run_script(self.prov.script, "version"), "2.0.0")

    def test_missing_version_file_falls_back_to_dev(self):
        os.remove(os.path.join(self.prov.repo, "VERSION"))
        self.assertEqual(run_script(self.prov.script, "version"), "dev")

    def test_version_file_trailing_whitespace_is_stripped(self):
        self.prov.write("VERSION", "  3.4.5  \n\n")
        self.assertEqual(run_script(self.prov.script, "version"), "3.4.5")


class TestCommit(unittest.TestCase):
    def setUp(self):
        self.prov = ProvenanceRepo()
        self.addCleanup(self.prov.cleanup)

    def test_commit_is_the_full_sha(self):
        sha = run_script(self.prov.script, "commit")
        self.assertEqual(len(sha), 40)
        self.assertTrue(all(c in "0123456789abcdef" for c in sha))

    def test_short_commit_is_eight_characters(self):
        short = run_script(self.prov.script, "short-commit")
        self.assertEqual(len(short), 8)
        self.assertTrue(short.startswith(run_script(self.prov.script, "commit")[:8]))

    def test_commit_follows_the_checkout(self):
        first = run_script(self.prov.script, "commit")
        self.prov.write("main.go", "package main\n\n// changed\n")
        self.prov.commit("second")
        second = run_script(self.prov.script, "commit")
        self.assertNotEqual(first, second)


class TestDirty(unittest.TestCase):
    def setUp(self):
        self.prov = ProvenanceRepo()
        self.addCleanup(self.prov.cleanup)

    def test_clean_tree_is_not_dirty(self):
        self.assertEqual(run_script(self.prov.script, "dirty"), "false")

    def test_modified_tracked_file_is_dirty(self):
        self.prov.write("main.go", "package main\n\n// edited\n")
        self.assertEqual(run_script(self.prov.script, "dirty"), "true")

    def test_staged_but_uncommitted_file_is_dirty(self):
        self.prov.write("main.go", "package main\n\n// staged\n")
        git(self.prov.repo, "add", "main.go")
        self.assertEqual(run_script(self.prov.script, "dirty"), "true")

    def test_untracked_go_source_is_dirty(self):
        # An untracked .go file would be compiled into the binary, so the
        # build genuinely differs from the commit.
        self.prov.write("extra.go", "package main\n")
        self.assertEqual(run_script(self.prov.script, "dirty"), "true")

    def test_untracked_non_source_file_is_not_dirty(self):
        # A forensic artefact or a scratch file is not compiled into the
        # binary, so it must not make every build look dirty.
        self.prov.write("notes.tmp", "scratch\n")
        self.assertEqual(run_script(self.prov.script, "dirty"), "false")

    def test_untracked_test_file_is_not_dirty(self):
        # Test files are not compiled into the shipped binary.
        self.prov.write("main_test.go", "package main\n")
        self.assertEqual(run_script(self.prov.script, "dirty"), "false")

    def test_gitignored_file_is_not_dirty(self):
        self.prov.write(".gitignore", ".build/\n")
        self.prov.commit("ignore")
        self.prov.write(".build/bin/mpm", "binary\n")
        self.assertEqual(run_script(self.prov.script, "dirty"), "false")


class TestBuildType(unittest.TestCase):
    def test_ordinary_build_is_development(self):
        prov = ProvenanceRepo()
        self.addCleanup(prov.cleanup)
        self.assertEqual(run_script(prov.script, "build-type"), "development")

    def test_tagged_stable_version_is_a_release(self):
        prov = ProvenanceRepo(version="1.2.3")
        self.addCleanup(prov.cleanup)
        git(prov.repo, "tag", "v1.2.3")
        self.assertEqual(run_script(prov.script, "build-type"), "release")

    def test_tag_without_v_prefix_is_a_release(self):
        prov = ProvenanceRepo(version="1.2.3")
        self.addCleanup(prov.cleanup)
        git(prov.repo, "tag", "1.2.3")
        self.assertEqual(run_script(prov.script, "build-type"), "release")

    def test_prerelease_tag_is_not_a_release(self):
        # 0.1.0-dev is a development pre-release: a tag on it is not a
        # formal release.
        prov = ProvenanceRepo(version="0.1.0-dev")
        self.addCleanup(prov.cleanup)
        git(prov.repo, "tag", "v0.1.0-dev")
        self.assertEqual(run_script(prov.script, "build-type"), "development")

    def test_archive_tags_are_not_release_tags(self):
        # The repository keeps archive/* preservation tags. They are not
        # semantic-version release tags and must never make a build look
        # like a formal release.
        prov = ProvenanceRepo(version="1.2.3")
        self.addCleanup(prov.cleanup)
        git(prov.repo, "tag", "archive/pr29head")
        self.assertEqual(run_script(prov.script, "build-type"), "development")

    def test_tag_on_an_older_commit_is_not_a_release(self):
        prov = ProvenanceRepo(version="1.2.3")
        self.addCleanup(prov.cleanup)
        git(prov.repo, "tag", "v1.2.3")
        prov.write("main.go", "package main\n\n// later\n")
        prov.commit("after the tag")
        self.assertEqual(run_script(prov.script, "build-type"), "development")

    def test_mismatched_tag_is_not_a_release(self):
        prov = ProvenanceRepo(version="1.2.3")
        self.addCleanup(prov.cleanup)
        git(prov.repo, "tag", "v9.9.9")
        self.assertEqual(run_script(prov.script, "build-type"), "development")


class TestLdflags(unittest.TestCase):
    def setUp(self):
        self.prov = ProvenanceRepo(version="1.2.3")
        self.addCleanup(self.prov.cleanup)

    def test_ldflags_stamp_every_provenance_field(self):
        ldflags = run_script(self.prov.script, "ldflags")
        sha = run_script(self.prov.script, "commit")
        short = run_script(self.prov.script, "short-commit")
        self.assertIn("-X %s.Version=1.2.3" % BUILDINFO_PKG, ldflags)
        self.assertIn("-X %s.Commit=%s" % (BUILDINFO_PKG, sha), ldflags)
        self.assertIn("-X %s.ShortCommit=%s" % (BUILDINFO_PKG, short), ldflags)
        self.assertIn("-X %s.Dirty=false" % BUILDINFO_PKG, ldflags)
        self.assertIn("-X %s.BuildType=development" % BUILDINFO_PKG, ldflags)

    def test_ldflags_carry_no_shell_metacharacters(self):
        # The value is spliced into a make recipe inside double quotes.
        ldflags = run_script(self.prov.script, "ldflags")
        for bad in (";", "|", "&", "$(", "`", "'", '"'):
            self.assertNotIn(bad, ldflags)


class TestNoGitCheckout(unittest.TestCase):
    def test_missing_git_metadata_is_reported_as_unknown(self):
        root = tempfile.mkdtemp(prefix="mpm-provenance-nogit-")
        self.addCleanup(lambda: shutil.rmtree(root, ignore_errors=True))
        os.makedirs(os.path.join(root, "scripts"))
        script = os.path.join(root, SCRIPT_REL)
        shutil.copy(SCRIPT_SRC, script)
        os.chmod(script, 0o755)
        with open(os.path.join(root, "VERSION"), "w", encoding="utf-8") as handle:
            handle.write("1.2.3\n")

        self.assertEqual(run_script(script, "version"), "1.2.3")
        self.assertEqual(run_script(script, "commit"), "unknown")
        self.assertEqual(run_script(script, "short-commit"), "unknown")
        self.assertEqual(run_script(script, "dirty"), "unknown")
        self.assertEqual(run_script(script, "build-type"), "development")


if __name__ == "__main__":
    unittest.main()
