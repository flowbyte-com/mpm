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


class _SandboxedCheckout:
    """An isolated checkout with an isolated HOME.

    HOME is not merely a convenience here. The Makefile resolves
    `PREFIX ?= $(HOME)/.mpm`, install.sh resolves `DATA_ROOT` the same
    way, and the Go build cache lives under HOME — so pinning HOME is
    what keeps a test run from reading or writing the operator's real
    install, database, and toolchain state.

    GOCACHE/GOMODCACHE/GOPATH are deliberately NOT re-pointed: they are
    shared, content-addressed developer caches, and re-downloading or
    recompiling the whole dependency graph for every test would push
    these tests past any sane timeout. They are build scratch, not
    product state.
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
        for var in ("GOCACHE", "GOMODCACHE", "GOPATH"):
            value = os.environ.get(var)
            if value:
                env[var] = value
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
        shutil.rmtree(self.root, ignore_errors=True)


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
                "clean ran `rm -rf bin`, which in the canonical layout is "
                "the live install.",
            )


if __name__ == "__main__":
    unittest.main()