"""The asset-group split — the ownership lifetime of a runtime asset.

The defect these exist to prevent
--------------------------------

MPM is migrating from a Git checkout to versioned, Git-free runtime
bundles. A bundle is the replaceable half of an installation; the data
root is the part that outlives it. The runtime assets split across the
two by OWNERSHIP LIFETIME:

  definitions   mode/, persona/, drills/ — the operator may edit these.
                They belong in the DATA ROOT, where they survive every
                version switch.

  host-assets   agent_installation/** — the per-host adapters, their
                reconcilers and the shared behavioural protocol. They
                change with the release and must roll back with it, so
                they belong in the BUNDLE.

Before the split, `install_runtime_assets.py` provisioned EVERYTHING
into ONE root. With a bundle in the picture, that is a defect waiting to
happen: a version switch replaces the bundle wholesale, so operator
definitions that lived in it would be replaced with the upstream copies —
and the reconciler's refusal to overwrite an operator's edit would turn
every upgrade into a conflict report for files the operator had
deliberately customised.

What is proven here, all hermetically
-------------------------------------

  * `--groups definitions` provisions the operator's definitions and
    NOTHING else.
  * `--groups host-assets` provisions the release-scoped assets and
    NOTHING else.
  * The two groups together cover the whole asset set — no asset is
    orphaned by the split.
  * The default (no `--groups`) is unchanged: everything, into one root.
  * The two groups may be provisioned into TWO SEPARATE roots, and each
    root then holds only what it owns.

Safety
------

Every test uses temporary source and runtime roots under
`tempfile.mkdtemp()`. The real `~/.mpm` and the production database are
never written to.
"""

from __future__ import annotations

import json
import os
import shutil
import subprocess
import sys
import tempfile
import unittest
from pathlib import Path

REPO_ROOT = Path(__file__).resolve().parents[2]
RECONCILER = REPO_ROOT / "scripts" / "install_runtime_assets.py"
MANIFEST_RELPATH = Path("config") / "runtime-assets.json"


def run_reconciler(source: Path, runtime: Path, *extra: str) -> subprocess.CompletedProcess:
    """Invoke the reconciler against explicit roots. Never the real ones."""
    return subprocess.run(
        [
            sys.executable,
            str(RECONCILER),
            "--source-root",
            str(source),
            "--runtime-root",
            str(runtime),
            *extra,
        ],
        capture_output=True,
        text=True,
        env={**os.environ, "HOME": str(runtime.parent)},
    )


class GroupsFixture(unittest.TestCase):
    """A source tree with one asset from each ownership group."""

    def setUp(self) -> None:
        self._tmp = tempfile.mkdtemp(prefix="mpm-asset-groups-")
        self.addCleanup(shutil.rmtree, self._tmp, True)
        self.base = Path(self._tmp)
        self.src = self.base / "src"
        self.src.mkdir(parents=True)

        # One operator-owned definition.
        mode = self.src / "mode"
        mode.mkdir()
        (mode / "default.md").write_text("---\nname: default\n---\n\nbody\n")

        # One release-scoped host asset: a per-adapter manifest.
        adapter = self.src / "agent_installation" / "mpm-opencode"
        adapter.mkdir(parents=True)
        (adapter / "reconcile.json").write_text('{"adapter": "mpm-opencode"}\n')

    def manifest_assets(self, runtime: Path) -> set[str]:
        path = runtime / MANIFEST_RELPATH
        if not path.exists():
            return set()
        return set(json.loads(path.read_text())["assets"].keys())


class TestGroupSplit(GroupsFixture):
    def test_definitions_group_provisions_only_the_operator_files(self) -> None:
        runtime = self.base / "home" / ".mpm"
        proc = run_reconciler(self.src, runtime, "--groups", "definitions")
        self.assertEqual(proc.returncode, 0, proc.stderr)

        assets = self.manifest_assets(runtime)
        self.assertIn("mode/default.md", assets)
        for rel in assets:
            self.assertFalse(
                rel.startswith("agent_installation/"),
                f"the definitions group must not own {rel}",
            )

    def test_host_assets_group_provisions_only_the_release_scoped_files(self) -> None:
        runtime = self.base / "bundle"
        proc = run_reconciler(self.src, runtime, "--groups", "host-assets")
        self.assertEqual(proc.returncode, 0, proc.stderr)

        assets = self.manifest_assets(runtime)
        self.assertIn("agent_installation/mpm-opencode/reconcile.json", assets)
        for rel in assets:
            self.assertFalse(
                rel.startswith(("mode/", "persona/", "drills/")),
                f"the host-assets group must not own {rel}",
            )

    def test_the_two_groups_together_cover_the_whole_asset_set(self) -> None:
        # No asset may be orphaned by the split: an asset that belongs to
        # neither group would silently stop being provisioned at all.
        everything = self.base / "all"
        proc = run_reconciler(self.src, everything)
        self.assertEqual(proc.returncode, 0, proc.stderr)
        all_assets = self.manifest_assets(everything)

        split = self.base / "split"
        for group in ("definitions", "host-assets"):
            proc = run_reconciler(self.src, split, "--groups", group)
            self.assertEqual(proc.returncode, 0, proc.stderr)
        split_assets = self.manifest_assets(split)

        self.assertEqual(
            all_assets,
            split_assets,
            "the split must cover exactly the same asset set as the default",
        )

    def test_the_default_is_unchanged(self) -> None:
        # No --groups provisions everything into one root: the
        # pre-split behaviour, which existing installs depend on.
        runtime = self.base / "home" / ".mpm"
        proc = run_reconciler(self.src, runtime)
        self.assertEqual(proc.returncode, 0, proc.stderr)
        assets = self.manifest_assets(runtime)
        self.assertIn("mode/default.md", assets)
        self.assertIn("agent_installation/mpm-opencode/reconcile.json", assets)

    def test_the_two_groups_may_live_in_two_separate_roots(self) -> None:
        # The real shape of the migration: the definitions go to the
        # data root, the host assets go to the bundle.
        data_root = self.base / "home" / ".mpm"
        bundle = self.base / "bundle"

        proc = run_reconciler(self.src, data_root, "--groups", "definitions")
        self.assertEqual(proc.returncode, 0, proc.stderr)
        proc = run_reconciler(self.src, bundle, "--groups", "host-assets")
        self.assertEqual(proc.returncode, 0, proc.stderr)

        self.assertIn("mode/default.md", self.manifest_assets(data_root))
        self.assertNotIn(
            "agent_installation/mpm-opencode/reconcile.json",
            self.manifest_assets(data_root),
        )
        self.assertIn(
            "agent_installation/mpm-opencode/reconcile.json",
            self.manifest_assets(bundle),
        )
        self.assertNotIn("mode/default.md", self.manifest_assets(bundle))

    def test_an_unknown_group_is_refused(self) -> None:
        runtime = self.base / "home" / ".mpm"
        proc = run_reconciler(self.src, runtime, "--groups", "nonsense")
        self.assertNotEqual(proc.returncode, 0)
        self.assertIn("nonsense", proc.stderr)


if __name__ == "__main__":
    unittest.main()
