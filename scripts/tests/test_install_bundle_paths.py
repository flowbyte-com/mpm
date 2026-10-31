#!/usr/bin/env python3
"""test_install_bundle_paths.py — the bundle layout's ONE root.

The versioned runtime-bundle layout writes its stable paths with the
RELATIVE target "current/<name>", so they resolve inside the install
prefix, while the active pointer lives at <data root>/current. The
layout therefore has ONE root, and the install prefix and the data
root must be the SAME directory.

With two different directories the conversion created a
"current/<name>" symlink that resolved against a `current` that did not
exist — a dangling symlink naming no binary and no host asset — and
`rb_convert_layout` reported success. The pointer had already been
swapped by then, so the installation was left pointing at a revision
whose stable paths resolved to nothing.

This file pins the INSTALLER's half of the contract: the unsupported
combination is refused BEFORE the installer touches the filesystem, so
it costs the operator nothing.

WHY THIS IS NOT INSIDE test_migration_gate.py
----------------------------------------------

The migration gate is a numbered set of proofs about the runtime
migration, and its count is quoted in the rehearsal's evidence record.
Adding an installer-level path proof there would change a historical
record's claim. This file stands beside it instead: it proves one
thing, about one surface, and it names why it lives here.

The library half of the same contract — the guard itself, and the
rollback that reads the record it wrote — is pinned in
test_runtime_bundle.py.

HERMETICITY
-----------

Every root is a fresh mktemp directory, HOME is redirected into it, and
nothing resolves into the operator's real ~/.mpm.
"""

import os
import re
import shutil
import subprocess
import tempfile
import textwrap
import unittest
from pathlib import Path

REPO_ROOT = Path(__file__).resolve().parent.parent.parent
INSTALL_SH = REPO_ROOT / "install.sh"

# The lines that must not be sourced: install.sh computes them from its
# own location, and it runs main() at the bottom.
STRIP_PATTERNS = (
    re.compile(r"^\s*readonly\s+PROJECT_ROOT\s*="),
    re.compile(r'^\s*main\s+"\$@"\s*$'),
)


def build_sourced_lib(install_text: str) -> str:
    out_lines = []
    for line in install_text.splitlines():
        if any(p.match(line) for p in STRIP_PATTERNS):
            continue
        out_lines.append(line)
    return "\n".join(out_lines) + "\n"


class BundlePathFixture(unittest.TestCase):
    """A disposable installation with a configurable prefix and data root."""

    def setUp(self):
        self._tmp = Path(tempfile.mkdtemp(prefix="mpm-bundle-paths-"))
        self.addCleanup(shutil.rmtree, self._tmp, ignore_errors=True)

        self.home = self._tmp / "home"
        self.home.mkdir()
        self.project = self._tmp / "project"
        (self.project / "scripts" / "lib").mkdir(parents=True)
        for name in ("runtime_bundle.sh", "binary_transaction.sh"):
            shutil.copy(
                REPO_ROOT / "scripts" / "lib" / name,
                self.project / "scripts" / "lib" / name,
            )
        (self.project / "install.sh.lib").write_text(
            build_sourced_lib(INSTALL_SH.read_text())
        )

        # The canonical installation: one root, checkout-style.
        self.root = self._tmp / "mpm"
        (self.root / "bin").mkdir(parents=True)
        (self.root / "bin" / "mpm").write_text("old binary\n")
        (self.root / "agent_installation").mkdir()
        (self.root / "agent_installation" / "adapter").mkdir()

        # The split the installer must refuse.
        self.split_prefix = self._tmp / "prefix"
        self.split_prefix.mkdir()

    def drive(self, script: str, prefix: Path, data_root: Path,
              use_bundle: bool = True) -> subprocess.CompletedProcess:
        driver = self.project / "_drive.sh"
        driver.write_text(
            textwrap.dedent(f"""\
                #!/usr/bin/env bash
                export PROJECT_ROOT="{self.project}"
                source "{self.project}/install.sh.lib"
                PREFIX="{prefix}"
                DATA_ROOT="{data_root}"
                USE_BUNDLE={"1" if use_bundle else "0"}
                export PREFIX DATA_ROOT USE_BUNDLE
                {script}
            """)
        )
        driver.chmod(0o755)
        env = dict(os.environ)
        env["HOME"] = str(self.home)
        env.pop("MPM_WORKSPACE", None)
        env.pop("MPM_DB_PATH", None)
        return subprocess.run(
            ["bash", str(driver)], capture_output=True, text=True, timeout=120, env=env
        )


class TestBundleLayoutHasOneRoot(BundlePathFixture):
    """The install prefix and the data root must be the same directory."""

    def test_the_canonical_layout_is_accepted(self):
        r = self.drive(
            "require_bundle_layout_paths; echo \"rc=$?\"",
            prefix=self.root,
            data_root=self.root,
        )
        self.assertIn("rc=0", r.stdout, r.stderr)

    def test_a_split_prefix_is_refused(self):
        r = self.drive(
            "require_bundle_layout_paths; echo \"rc=$?\"",
            prefix=self.split_prefix,
            data_root=self.root,
        )
        # The refusal EXITS: the installer stops rather than carrying on
        # with a layout it cannot produce.
        self.assertEqual(r.returncode, 1, r.stdout + r.stderr)
        self.assertIn("SAME directory", r.stderr)
        self.assertIn(str(self.split_prefix), r.stderr)
        self.assertIn(str(self.root), r.stderr)

    def test_the_default_install_path_still_accepts_a_split_prefix(self):
        # The DEFAULT install path puts the binaries in the prefix and
        # the state in the data root. That split is supported, and it
        # must keep working: only the BUNDLE layout has one root.
        r = self.drive(
            "require_bundle_layout_paths; echo \"rc=$?\"",
            prefix=self.split_prefix,
            data_root=self.root,
            use_bundle=False,
        )
        self.assertIn("rc=0", r.stdout, r.stderr)

    def test_the_bundle_phase_refuses_before_it_creates_anything(self):
        # THE GUARANTEE. An unsupported combination must cost the
        # operator nothing: no release, no pointer, no lock file, no
        # staging area — and the operator's own directories untouched.
        r = self.drive(
            "phase_bundle",
            prefix=self.split_prefix,
            data_root=self.root,
        )
        self.assertNotEqual(r.returncode, 0, "the bundle phase must refuse")
        self.assertIn("SAME directory", r.stderr)

        for rel in ("releases", "current", "staging", ".mpm-bundle.lock",
                    ".legacy-layout-paths"):
            self.assertFalse(
                (self.root / rel).exists(),
                f"the refusal must happen before {rel} is created",
            )
        self.assertTrue((self.root / "bin" / "mpm").is_file())
        self.assertFalse((self.root / "bin").is_symlink())

    def test_a_refused_bundle_phase_leaves_the_split_prefix_untouched(self):
        r = self.drive(
            "phase_bundle",
            prefix=self.split_prefix,
            data_root=self.root,
        )
        self.assertNotEqual(r.returncode, 0)
        self.assertEqual(
            list(self.split_prefix.iterdir()),
            [],
            "a refused bundle phase must not write into the prefix",
        )


if __name__ == "__main__":
    unittest.main()
