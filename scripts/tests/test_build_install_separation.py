"""Build/install separation — acceptance tests.

The defect these exist to prevent:

    canonical checkout:        ~/.mpm
    `make build` output:       ~/.mpm/bin/*      (pre-fix)
    systemd ExecStart:         ~/.mpm/bin/mpm-scheduler

so `make build` was, by construction, an overwrite of the live installed
binaries — i.e. an ordinary build WAS a deployment.

After the split, `make build` writes `.build/bin/` (a scratch
subdirectory of the checkout) and `make install` is the only thing that
writes `$(PREFIX)/bin/`.

Two properties are proven here, both from the outside:

  * TestBuildInstallIsolation.test_fake_prefix_install_promotes_all_five
    (§11) — with a fake HOME and a fake PREFIX, `make build` produces
    developer artifacts and leaves PREFIX/bin untouched; `make install
    PREFIX=<fake>` then promotes all five, and each installed binary is
    byte-identical to its developer artifact.

  * TestLiveInstallSentinel.test_build_does_not_touch_the_live_install
    (§12, the central acceptance test) — the exact bug, reproduced
    hermetically: PREFIX == repo root, with sentinel files standing in
    for the installed binaries. `make build` must leave every sentinel
    BYTE-IDENTICAL. Only the explicit `make install` may replace them.

Everything runs in temp directories. The real checkout, the real
~/.mpm, and the production database are never touched, and no systemd
unit is read, written, or reloaded.
"""

from __future__ import annotations

import hashlib
import os
import shutil
import subprocess
import tempfile
import unittest
from pathlib import Path


REPO_ROOT = Path(__file__).resolve().parents[2]

ALL_BINARIES = (
    "mpm",
    "mpm-mcp",
    "mpm-scheduler",
    "mpm-critic",
    "mpm-telemetry",
)

# Distinct from any real content so "unchanged" means "the sentinel is
# still here, byte for byte", not "some file exists".
SENTINEL = b"MAGIC-SENTINEL-not-a-real-binary\x00"


# Where the Go toolchain keeps its module cache and build cache on the
# OPERATOR's real machine. Computed at import time from `go env` so the
# tests follow whatever Go the developer has installed, not whatever
# the test happened to inherit. Both values are then ALWAYS passed
# explicitly into the sandbox env — see _SandboxedCheckout.env().
def _resolve_real_go_cache() -> tuple[str | None, str | None]:
    """Return (GOMODCACHE, GOCACHE) as the toolchain would resolve them
    with the test's own environment, or (None, None) if `go` is absent.

    The values are used as the only acceptable cache locations for the
    sandbox subprocesses. A real developer cache is what the build is
    allowed to read/write; the sandbox HOME is forbidden territory.
    """
    try:
        mod = subprocess.run(
            ["go", "env", "GOMODCACHE"],
            capture_output=True, text=True, check=True,
        ).stdout.strip()
        bld = subprocess.run(
            ["go", "env", "GOCACHE"],
            capture_output=True, text=True, check=True,
        ).stdout.strip()
    except (FileNotFoundError, subprocess.CalledProcessError):
        return (None, None)
    return (mod or None, bld or None)


_REAL_GOMODCACHE, _REAL_GOCACHE = _resolve_real_go_cache()


def _sha256(path: Path) -> str:
    h = hashlib.sha256()
    with path.open("rb") as f:
        for chunk in iter(lambda: f.read(1 << 20), b""):
            h.update(chunk)
    return h.hexdigest()


def _copy_tracked_tree(dest: Path) -> None:
    """Copy every git-tracked file from the repo into `dest`.

    `git ls-files` bounds the copy to tracked content, so ignored build
    output or local runtime state on a developer's machine is never
    imported. The working tree is copied rather than `git archive HEAD`
    so the .gitignore and Makefile actually on disk are what get tested.
    """
    tracked = subprocess.run(
        ["git", "ls-files", "-z"],
        cwd=REPO_ROOT,
        capture_output=True,
        check=True,
    ).stdout.decode()
    for rel in tracked.split("\0"):
        if not rel:
            continue
        src = REPO_ROOT / rel
        if not src.is_file():
            continue
        out = dest / rel
        out.parent.mkdir(parents=True, exist_ok=True)
        shutil.copy2(src, out)


def _writable_remove(root: Path) -> None:
    """Remove `root` reliably, including subtrees whose directories
    are mode 0555 — the layout the Go toolchain creates inside its
    module cache. `shutil.rmtree` cannot unlink a child of a 0555
    directory without first making that directory writable; we do
    that here, then delegate to `rmtree` with full error reporting
    so silent accumulation is impossible.

    Symlink safety (the contract that prevents this helper from
    touching anything outside the test-owned root):
      * Refuses to remove a symlinked `root`. The intended use is a
        `tempfile.mkdtemp` result, which is always a real directory;
        following a symlink here could write or delete outside the
        sandbox.
      * During the walk, any symlink encountered is `os.unlink`-ed
        before chmod runs. `os.chmod` follows symlinks by default,
        so chmod'ing a symlink would change the target's mode —
        potentially a file the operator owns. Unlinking the symlink
        removes only the symlink itself, leaving the target intact.
        `os.walk` is also asked `followlinks=False` (the default,
        made explicit) so it does not descend into any symlinked
        directory. By the time `shutil.rmtree` runs, every symlink
        has been removed, so rmtree's own symlink behaviour is no
        longer in scope.
      * Only real directories are chmod'd, never files. `os.unlink`
        only requires write permission on the parent directory, so
        file mode bits are not on the critical path and need not be
        touched.

    Errors are not swallowed: if anything still cannot be removed
    after the walk, the helper raises so the caller sees the failure
    rather than letting a sandbox accumulate. That is the contract
    that prevents the ~98 MB-per-run leak this module exists to fix.
    """
    if root.is_symlink():
        raise ValueError(
            f"refusing to remove symlink root: {root}; sandbox root "
            "must be a real directory so cleanup cannot escape it"
        )
    if not root.exists():
        return
    # Walk bottom-up so a child directory is processed before its
    # parent. followlinks=False (the default, stated explicitly) is
    # what stops os.walk from descending into a symlinked directory
    # under the sandbox.
    for dirpath, dirnames, filenames in os.walk(str(root), topdown=False, followlinks=False):
        # Make this directory writable so its children can be unlinked
        # by the trailing rmtree — the actual reason this helper exists
        # is that the Go module cache leaves these directories at 0o555.
        os.chmod(dirpath, 0o700)
        # Unlink every symlink in dirpath. We must do this BEFORE any
        # chmod could run on a symlink entry: chmod would follow the
        # link and modify the target. unlink removes only the symlink.
        for name in dirnames + filenames:
            entry = os.path.join(dirpath, name)
            if os.path.islink(entry):
                try:
                    os.unlink(entry)
                except FileNotFoundError:
                    # Race: another walker or test step removed it
                    # between islink() and unlink(). Treat as already
                    # gone; rmtree will not see it.
                    pass
    shutil.rmtree(root)


class _SandboxedCheckout:
    """An isolated checkout with an isolated HOME and an isolated cache.

    HOME is not merely a convenience here. The Makefile resolves
    `PREFIX ?= $(HOME)/.mpm`, install.sh resolves `DATA_ROOT` the same
    way, and several MPM runtime paths are anchored at HOME — so
    pinning HOME is what keeps a test run from reading or writing the
    operator's real install, database, and toolchain state.

    GOCACHE and GOMODCACHE are also re-pointed. They are SHARED
    developer caches (content-addressed, deduplicated, and safe to
    read/write from any project), and forcing each test to repopulate
    them inside the sandbox would (a) re-download the entire module
    graph on every run, blowing the test's timeout, and (b) drop
    mode-0555 module directories under the sandbox that the cleanup
    helper cannot delete without first chmodding them. The shared
    caches are always passed EXPLICITLY (not only when the parent
    environment already exported them), so a developer machine that
    never set GOMODCACHE still gets the toolchain pointed at the
    operator's real cache, not at a derivation from the sandboxed
    HOME.
    """

    def __init__(self) -> None:
        self.root = Path(tempfile.mkdtemp(prefix="mpm-buildsep-"))
        self.checkout = self.root / "repo"
        self.home = self.root / "home"
        self.home.mkdir(parents=True)
        _copy_tracked_tree(self.checkout)

    def env(self, **extra: str) -> dict[str, str]:
        env = dict(os.environ)
        env["HOME"] = str(self.home)
        env["MPM_WORKSPACE"] = str(self.home / ".mpm")
        # Always pin Go's caches to their real-machine locations so the
        # toolchain cannot derive them from the redirected HOME. An
        # empty string would re-enable the default; an explicit value
        # is the contract `go help environment` documents.
        if _REAL_GOMODCACHE is not None:
            env["GOMODCACHE"] = _REAL_GOMODCACHE
        if _REAL_GOCACHE is not None:
            env["GOCACHE"] = _REAL_GOCACHE
        # Make GOPATH consistent: the toolchain reads it for layout
        # decisions even when GOMODCACHE is set, so we anchor it at
        # the operator's real GOPATH (the parent of pkg/mod). It is
        # never written to during these tests.
        real_gopath = str(Path(_REAL_GOMODCACHE).parent) if _REAL_GOMODCACHE else None
        if real_gopath:
            env["GOPATH"] = real_gopath
        env.update(extra)
        return env

    def make(self, *args: str, env: dict[str, str] | None = None):
        return subprocess.run(
            ["make", *args],
            cwd=self.checkout,
            env=env or self.env(),
            capture_output=True,
            text=True,
        )

    def cleanup(self) -> None:
        _writable_remove(self.root)


class TestBuildInstallIsolation(unittest.TestCase):
    """§11 — promotion is explicit, complete, and byte-faithful."""

    @classmethod
    def setUpClass(cls) -> None:
        cls.sandbox = _SandboxedCheckout()
        cls.build = cls.sandbox.make("build")

    @classmethod
    def tearDownClass(cls) -> None:
        cls.sandbox.cleanup()

    def test_build_produces_all_five_developer_artifacts(self):
        self.assertEqual(
            self.build.returncode,
            0,
            "`make build` must succeed in an isolated checkout.\n"
            + self.build.stdout + self.build.stderr,
        )
        build_dir = self.sandbox.checkout / ".build" / "bin"
        for name in ALL_BINARIES:
            self.assertTrue(
                (build_dir / name).is_file(),
                f"make build did not produce {name} in {build_dir}",
            )

    def test_build_leaves_the_install_prefix_untouched(self):
        """`make build` must not write PREFIX/bin, even when it exists.

        Seeding a pre-existing PREFIX/bin is what makes this
        non-vacuous: an empty directory would satisfy "nothing was
        written" trivially, and a bug that overwrote the files would
        still pass an emptiness check.
        """
        prefix = self.sandbox.root / "fake-prefix"
        (prefix / "bin").mkdir(parents=True)
        sentinel = prefix / "bin" / "mpm-scheduler"
        sentinel.write_bytes(SENTINEL)

        result = self.sandbox.make("build", env=self.sandbox.env(PREFIX=str(prefix)))
        self.assertEqual(
            result.returncode, 0, result.stdout + result.stderr
        )
        self.assertEqual(
            sentinel.read_bytes(),
            SENTINEL,
            "`make build` overwrote an installed binary in $PREFIX/bin. "
            "Building must never promote.",
        )

    def test_install_promotes_all_five_byte_identically(self):
        prefix = self.sandbox.root / "promote-prefix"
        result = self.sandbox.make(
            "install", "PREFIX=" + str(prefix), env=self.sandbox.env()
        )
        self.assertEqual(
            result.returncode,
            0,
            "`make install PREFIX=<fake>` must succeed.\n"
            + result.stdout + result.stderr,
        )
        for name in ALL_BINARIES:
            developer = self.sandbox.checkout / ".build" / "bin" / name
            installed = prefix / "bin" / name
            self.assertTrue(
                installed.is_file(),
                f"make install did not promote {name} to {installed}",
            )
            self.assertEqual(
                _sha256(developer),
                _sha256(installed),
                f"installed {name} is not byte-identical to the developer "
                "artifact; promotion must not rewrite or strip the binary",
            )


class TestLiveInstallSentinel(unittest.TestCase):
    """§12 — the central acceptance test: the exact bug, hermetically."""

    def setUp(self):
        self.sandbox = _SandboxedCheckout()
        self.addCleanup(self.sandbox.cleanup)
        # PREFIX IS the checkout root — the canonical ~/.mpm co-location
        # that made `make build` a live deployment.
        self.prefix = self.sandbox.checkout
        self.install_bin = self.prefix / "bin"
        self.install_bin.mkdir(parents=True)
        for name in ALL_BINARIES:
            (self.install_bin / name).write_bytes(SENTINEL + name.encode())

    def test_build_does_not_touch_the_live_install(self):
        before = {
            name: (self.install_bin / name).read_bytes() for name in ALL_BINARIES
        }
        mtimes = {
            name: (self.install_bin / name).stat().st_mtime_ns
            for name in ALL_BINARIES
        }

        result = self.sandbox.make("build", env=self.sandbox.env(PREFIX=str(self.prefix)))
        self.assertEqual(
            result.returncode,
            0,
            "`make build` must succeed with PREFIX == repo root.\n"
            + result.stdout + result.stderr,
        )

        for name in ALL_BINARIES:
            path = self.install_bin / name
            self.assertTrue(path.is_file(), f"{path} disappeared during make build")
            self.assertEqual(
                path.read_bytes(),
                before[name],
                f"`make build` MODIFIED the installed binary {name}. With "
                "PREFIX == repo root this is exactly the defect under test: "
                "an ordinary build overwrote what the running services "
                "execute.",
            )
            self.assertEqual(
                path.stat().st_mtime_ns,
                mtimes[name],
                f"`make build` rewrote {name} even though the bytes are "
                "unchanged; it must not touch the install path at all",
            )

    def test_explicit_install_replaces_the_sentinels(self):
        """The other half: `make install` — and only it — may promote."""
        self.sandbox.make("build", env=self.sandbox.env(PREFIX=str(self.prefix)))

        result = self.sandbox.make(
            "install", "PREFIX=" + str(self.prefix), env=self.sandbox.env()
        )
        self.assertEqual(
            result.returncode,
            0,
            "`make install` must succeed with PREFIX == repo root.\n"
            + result.stdout + result.stderr,
        )

        for name in ALL_BINARIES:
            installed = self.install_bin / name
            developer = self.prefix / ".build" / "bin" / name
            self.assertNotEqual(
                installed.read_bytes(),
                SENTINEL + name.encode(),
                f"`make install` left the {name} sentinel in place; explicit "
                "promotion must replace it",
            )
            self.assertEqual(
                _sha256(installed),
                _sha256(developer),
                f"promoted {name} differs from the developer artifact",
            )

    def test_clean_does_not_delete_the_installed_binaries(self):
        self.sandbox.make("build", env=self.sandbox.env(PREFIX=str(self.prefix)))
        result = self.sandbox.make(
            "clean", env=self.sandbox.env(PREFIX=str(self.prefix))
        )
        self.assertEqual(result.returncode, 0, result.stdout + result.stderr)
        for name in ALL_BINARIES:
            self.assertTrue(
                (self.install_bin / name).is_file(),
                f"`make clean` deleted the installed {name}. The pre-fix "
                "clean ran `rm -rf bin`, which is the live install in the "
                "canonical layout.",
            )


class TestSandboxCleanup(unittest.TestCase):
    """Regression: the helper the suite relies on for cleanup must
    actually delete what it created, including subtrees with Go-style
    read-only mode bits. Issue #27 was rooted in `shutil.rmtree(..., \
    ignore_errors=True)` silently leaving ~98 MB of module-cache
    directories under /tmp on every run.

    These tests are deliberately hermetic: they do NOT shell out to
    `go build` (which would re-introduce the variable they exist to
    pin down), and they do NOT touch the real GOMODCACHE.
    """

    def test_cleanup_removes_a_subtree_with_0555_dirs_and_0444_files(self):
        # Layout: root/ro/0444_file, root/ro/0555_subdir/0444_inner.
        # Both are the shape Go leaves behind when it caches a module.
        root = Path(tempfile.mkdtemp(prefix="mpm-buildsep-test-ro-"))
        ro = root / "ro"
        ro.mkdir()
        (ro / "outer.txt").write_text("x")
        os.chmod(ro / "outer.txt", 0o444)
        # The inner dir must be created, the inner file written, and only
        # then chmod'd to the read-only state. Chmod'ing the parent first
        # would block the file write that is meant to live under it —
        # which is a different bug from the one this test exists to pin
        # down, and would mask the helper's real behaviour.
        ro_sub = ro / "subdir"
        ro_sub.mkdir()
        (ro_sub / "inner.txt").write_text("y")
        os.chmod(ro_sub, 0o555)
        os.chmod(ro_sub / "inner.txt", 0o444)
        # Sanity: a vanilla rmtree with default mode-bit handling would
        # leave this behind, which is the whole point of the fix.
        self.assertTrue(ro_sub.is_dir())

        _writable_remove(root)

        self.assertFalse(
            root.exists(),
            f"_writable_remove left {root} behind; the per-run cleanup "
            "is silently failing like the pre-fix code did",
        )

    def test_cleanup_runs_even_after_the_caller_raises(self):
        # Contract: cleanup registered through addCleanup MUST run, and
        # the residue MUST be gone, even when the test body raises.
        #
        # unittest's addCleanup runs in LIFO order when the test method
        # returns — AFTER any `try/except` in the body has finished, and
        # AFTER the body's own `self.assertFalse` would have. Asserting
        # post-cleanup in the body therefore does not work: the body
        # would always see the directory still present. The right way
        # is to register the assertion AS a cleanup itself, after the
        # real cleanup, so the assertion runs LAST — once the helper
        # has had its turn.
        #
        # If `_writable_remove` were the broken `rmtree(ignore_errors=True)`,
        # the assertion cleanup would fail with the directory still
        # present, the original 98 MB leak would re-emerge, and the
        # suite would be reporting a leak as a test failure.
        root = Path(tempfile.mkdtemp(prefix="mpm-buildsep-test-fail-"))
        # Pre-poison the directory with 0555/0444 entries that
        # rmtree(ignore_errors=True) would silently leave behind. Order
        # matters: write the file, then chmod the parent — chmod'ing
        # the parent first would block the file's own write.
        sub = root / "ro"
        sub.mkdir()
        (sub / "inner.txt").write_text("x")
        os.chmod(sub, 0o555)
        os.chmod(sub / "inner.txt", 0o444)

        # Real failure in the test body. We catch it so the test method
        # can return normally and the cleanups get a chance to run;
        # re-raising it would prevent the LIFO chain from completing.
        try:
            raise AssertionError("simulated test failure")
        except AssertionError:
            pass

        # Register the assertion FIRST, the rmtree SECOND — LIFO means
        # the rmtree runs first, the assertion runs last. If the helper
        # is broken, the assertion sees the directory still present and
        # fails loudly instead of leaking silently.
        #
        # The argument to assertFalse must be a callable that re-reads
        # the filesystem at cleanup time, not `root.exists()` evaluated
        # now. addCleanup captures arguments at registration time, and
        # the directory still exists at that point — the captured True
        # would make the assertion fail even when the cleanup was fine.
        def _check_gone():
            self.assertFalse(
                root.exists(),
                f"addCleanup did not run _writable_remove on {root}; a "
                "failing test will leak the entire sandbox",
            )
        self.addCleanup(_check_gone)
        self.addCleanup(_writable_remove, root)

    def test_cleanup_does_not_touch_paths_outside_the_sandbox(self):
        # A real operator file. If the cleanup helper ever walks ABOVE
        # the root it is given, it would chmod this file. We assert
        # mode bits are unchanged after running cleanup on an unrelated
        # sibling directory.
        sentinel = Path(tempfile.mkdtemp(prefix="mpm-buildsep-sentinel-"))
        (sentinel / "realfile").write_text("do not chmod me")
        os.chmod(sentinel / "realfile", 0o600)
        before_mode = (sentinel / "realfile").stat().st_mode & 0o777
        self.addCleanup(lambda: _writable_remove(sentinel) if sentinel.exists() else None)

        # Build an unrelated, fully-detached sandbox and clean THAT up.
        victim = Path(tempfile.mkdtemp(prefix="mpm-buildsep-victim-"))
        (victim / "x").write_text("x")
        _writable_remove(victim)

        after_mode = (sentinel / "realfile").stat().st_mode & 0o777
        self.assertEqual(
            before_mode, after_mode,
            "_writable_remove changed mode bits of a file outside the "
            "root it was asked to remove; cleanup is overreaching",
        )

    def test_cleanup_does_not_follow_symlinks(self):
        # External operator-owned paths. _writable_remove must not
        # chmod or unlink anything outside the sandbox even when the
        # sandbox contains symlinks that point at the operator's
        # files. os.chmod follows symlinks by default, so a symlinked
        # entry would otherwise be chmod'd at the target — see the
        # probe that established this in the #27 follow-up review.
        #
        # The external targets are the assertion: their contents,
        # mode bits, mtimes, and (for the file) their inode must all
        # be unchanged after the helper runs. If the helper ever
        # followed a symlink, the target's state would be different.
        external_dir = Path(tempfile.mkdtemp(prefix="mpm-buildsep-ext-dir-"))
        inner = external_dir / "inner.txt"
        inner.write_text("untouched inner content")
        os.chmod(inner, 0o644)
        os.chmod(external_dir, 0o755)
        inner_content = inner.read_text()
        inner_mode = inner.stat().st_mode & 0o777
        inner_mtime = inner.stat().st_mtime_ns
        dir_mode = external_dir.stat().st_mode & 0o777
        dir_mtime = external_dir.stat().st_mtime_ns
        self.addCleanup(shutil.rmtree, external_dir, ignore_errors=True)

        external_file_dir = Path(tempfile.mkdtemp(prefix="mpm-buildsep-ext-filedir-"))
        external_file = external_file_dir / "data"
        external_file.write_text("untouched external content")
        os.chmod(external_file, 0o644)
        file_content = external_file.read_text()
        file_mode = external_file.stat().st_mode & 0o777
        file_inode = external_file.stat().st_ino
        file_mtime = external_file.stat().st_mtime_ns
        self.addCleanup(shutil.rmtree, external_file_dir, ignore_errors=True)

        # Sandbox contains three symlinks — one to the external file,
        # one to the external directory, one dangling — plus a real
        # 0555 directory and a real 0444 file so the original defect-A
        # cleanup path is exercised in the same call.
        sandbox = Path(tempfile.mkdtemp(prefix="mpm-buildsep-symlink-"))
        (sandbox / "link-to-file").symlink_to(external_file)
        (sandbox / "link-to-dir").symlink_to(external_dir)
        (sandbox / "dangling").symlink_to(
            "/nonexistent/path/that/does/not/exist"
        )
        ro = sandbox / "ro"
        ro.mkdir()
        (ro / "x").write_text("x")
        os.chmod(ro / "x", 0o444)
        os.chmod(ro, 0o555)

        _writable_remove(sandbox)

        # The sandbox itself is gone.
        self.assertFalse(
            sandbox.exists(),
            f"sandbox {sandbox} was not removed; the symlink handling "
            "broke the original 0555-dir cleanup path",
        )
        # The external file: content, mode, mtime, and inode all
        # unchanged. The inode check in particular catches a
        # write-through that replaced the file in place.
        self.assertTrue(external_file.exists(), "external file was deleted")
        self.assertEqual(
            external_file.read_text(), file_content,
            "external file content was modified; a symlink was followed",
        )
        self.assertEqual(
            external_file.stat().st_mode & 0o777, file_mode,
            "external file mode was changed; a symlink was followed",
        )
        self.assertEqual(
            external_file.stat().st_ino, file_inode,
            "external file inode changed; a symlink was followed",
        )
        self.assertEqual(
            external_file.stat().st_mtime_ns, file_mtime,
            "external file mtime was changed; chmod ran on the target",
        )
        # The external directory and its contents are unchanged.
        self.assertTrue(external_dir.exists(), "external dir was deleted")
        self.assertTrue(
            inner.exists(),
            "external dir contents were deleted; a directory symlink was followed",
        )
        self.assertEqual(
            inner.read_text(), inner_content,
            "external dir content changed; chmod or unlink escaped the sandbox",
        )
        self.assertEqual(
            inner.stat().st_mode & 0o777, inner_mode,
            "external dir content mode changed; a symlink was followed",
        )
        self.assertEqual(
            inner.stat().st_mtime_ns, inner_mtime,
            "external dir content mtime changed; chmod ran on the target",
        )
        self.assertEqual(
            external_dir.stat().st_mode & 0o777, dir_mode,
            "external dir mode changed; chmod ran on the target",
        )
        self.assertEqual(
            external_dir.stat().st_mtime_ns, dir_mtime,
            "external dir mtime changed; chmod ran on the target",
        )

    def test_cleanup_rejects_a_symlinked_root(self):
        # The sandbox root itself is a symlink. The helper must refuse
        # to follow it; doing otherwise could chmod the target
        # directory or, on Python versions where shutil.rmtree followed
        # directory symlinks, recursively remove the target's contents.
        real = Path(tempfile.mkdtemp(prefix="mpm-buildsep-real-root-"))
        (real / "data.txt").write_text("untouched real-root content")
        self.addCleanup(shutil.rmtree, real, ignore_errors=True)
        link_parent = Path(tempfile.mkdtemp(prefix="mpm-buildsep-link-parent-"))
        self.addCleanup(shutil.rmtree, link_parent, ignore_errors=True)
        link = link_parent / "link-to-real"
        link.symlink_to(real)

        with self.assertRaises(ValueError) as ctx:
            _writable_remove(link)
        # The error message has to make the refusal visible to a future
        # caller; a silent return would be just as bad as a follow.
        self.assertIn(
            "symlink", str(ctx.exception).lower(),
            f"_writable_remove raised but the message does not name the "
            f"problem: {ctx.exception!r}",
        )
        # And the target is still there, untouched.
        self.assertTrue(
            (real / "data.txt").exists(),
            "real-root contents were deleted despite the symlink rejection",
        )
        self.assertEqual(
            (real / "data.txt").read_text(), "untouched real-root content",
            "real-root content changed despite the symlink rejection",
        )


class TestModuleCacheIsolation(unittest.TestCase):
    """Regression: `make build` inside the sandbox must use the
    operator's real Go caches, not ones derived from the redirected
    HOME. Issue #27's first symptom was that ~98 MB of module-cache
    data was being downloaded INSIDE the sandbox on every run, because
    the original `env()` only re-pointed GOMODCACHE when the parent
    environment had already set it.
    """

    @unittest.skipUnless(
        shutil.which("go") is not None,
        "`go` not on PATH; module-cache isolation is a no-op without it",
    )
    def test_sandbox_env_pins_gomodcache_to_real_location(self):
        sandbox = _SandboxedCheckout()
        self.addCleanup(sandbox.cleanup)
        env = sandbox.env()
        # GOMODCACHE MUST be set explicitly, and to a path OUTSIDE the
        # sandbox. If it were empty or pointing inside the sandbox, the
        # toolchain would derive it from HOME — the defect under test.
        self.assertIn("GOMODCACHE", env, "env() did not set GOMODCACHE")
        self.assertNotIn(
            str(sandbox.home), env["GOMODCACHE"],
            f"GOMODCACHE resolves inside the sandbox HOME "
            f"({env['GOMODCACHE']}); the toolchain will populate a "
            "throwaway module cache that the cleanup helper has to "
            "chmod before removing",
        )
        self.assertEqual(env["GOMODCACHE"], _REAL_GOMODCACHE)

    @unittest.skipUnless(
        shutil.which("go") is not None,
        "`go` not on PATH; module-cache isolation is a no-op without it",
    )
    def test_sandbox_env_pins_gocache_to_real_location(self):
        sandbox = _SandboxedCheckout()
        self.addCleanup(sandbox.cleanup)
        env = sandbox.env()
        self.assertIn("GOCACHE", env)
        self.assertNotIn(
            str(sandbox.home), env["GOCACHE"],
            f"GOCACHE resolves inside the sandbox HOME "
            f"({env['GOCACHE']}); the toolchain will populate a "
            "throwaway build cache that has the same 0555-dir shape "
            "the issue is meant to keep out of the sandbox",
        )
        self.assertEqual(env["GOCACHE"], _REAL_GOCACHE)

    def test_no_module_cache_appears_under_sandbox_after_make_build(self):
        # The strongest end-to-end assertion: a full `make build` inside
        # the sandbox must not leave a Go module cache under the
        # sandbox HOME. If it does, the cache-isolation fix is not
        # working and the leak is back.
        sandbox = _SandboxedCheckout()
        self.addCleanup(sandbox.cleanup)
        # `make build` is heavy; skip on hosts where Go is not on PATH
        # because we cannot reach the path that would actually trigger
        # the defect.
        if shutil.which("go") is None:
            self.skipTest("`go` not on PATH")
        try:
            _copy_tracked_tree(sandbox.checkout)
        except Exception:
            # _copy_tracked_tree already ran in __init__, but if it
            # somehow didn't, fall through to the build which will
            # fail loudly.
            pass
        result = sandbox.make("build")
        self.assertEqual(
            result.returncode, 0,
            f"`make build` in the sandbox failed:\n{result.stdout}\n{result.stderr}",
        )
        mod_path = sandbox.home / "go" / "pkg" / "mod"
        bld_path = sandbox.home / ".cache" / "go-build"
        self.assertFalse(
            mod_path.exists(),
            f"Go module cache was created under the sandbox HOME at "
            f"{mod_path}; GOMODCACHE was not actually pinned to the "
            "operator's real cache",
        )
        self.assertFalse(
            bld_path.exists(),
            f"Go build cache was created under the sandbox HOME at "
            f"{bld_path}; GOCACHE was not actually pinned to the "
            "operator's real cache",
        )


if __name__ == "__main__":
    unittest.main()