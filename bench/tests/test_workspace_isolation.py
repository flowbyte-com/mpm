"""Workspace isolation tests (C4 gates 1-4).

These tests do NOT touch the live MPM database. They create a temp
dir for fixtures and assert the harness's isolation checks.
"""

import os
import stat
import tempfile
import unittest
from pathlib import Path
from unittest import mock

from bench.workspace import (
    LIVE_STORE_SENTINELS,
    WorkspaceError,
    assert_safe_output_dir,
    assert_safe_workspace,
    reject_unsafe_symlinks,
    write_mpm_config,
)


class TestAssertSafeWorkspace(unittest.TestCase):
    def test_tempdir_workspace_is_accepted(self) -> None:
        with tempfile.TemporaryDirectory() as td:
            snap = Path(td) / "snap"
            real = assert_safe_workspace(snap)
            self.assertTrue(real.exists())
            self.assertTrue(oct(stat.S_IMODE(os.stat(real).st_mode)) in ("0o700", "0o755"))
            # Not inside any live-store sentinel.
            for sentinel in LIVE_STORE_SENTINELS:
                with self.assertRaises((AssertionError, ValueError)):
                    real.relative_to(Path(sentinel).resolve())

    def test_symlink_into_home_rejected(self) -> None:
        # If a symlink in a temp dir points to ~/.mpm, realpath lands there.
        with tempfile.TemporaryDirectory() as td:
            link = Path(td) / "link"
            try:
                link.symlink_to(Path.home() / ".mpm")
            except (OSError, FileExistsError):
                self.skipTest("cannot create symlink in this environment")
            with self.assertRaises(WorkspaceError):
                assert_safe_workspace(link)


class TestAssertSafeOutputDir(unittest.TestCase):
    def test_tempdir_output_dir_accepted(self) -> None:
        with tempfile.TemporaryDirectory() as td:
            snap = Path(td) / "snap"
            out = Path(td) / "out"
            snap.mkdir(parents=True)
            real_snap = assert_safe_workspace(snap)
            real_out = assert_safe_output_dir(out, real_snap)
            self.assertTrue(real_out.exists())

    def test_output_dir_inside_snap_rejected(self) -> None:
        with tempfile.TemporaryDirectory() as td:
            snap = Path(td) / "snap"
            out_inside = snap / "out"
            snap.mkdir(parents=True)
            real_snap = assert_safe_workspace(snap)
            with self.assertRaises(WorkspaceError):
                assert_safe_output_dir(out_inside, real_snap)

    def test_output_dir_in_home_mpm_rejected(self) -> None:
        with tempfile.TemporaryDirectory() as td:
            snap = Path(td) / "snap"
            snap.mkdir(parents=True)
            real_snap = assert_safe_workspace(snap)
            out = Path.home() / ".mpm" / "harness-output-should-be-rejected"
            with self.assertRaises(WorkspaceError):
                assert_safe_output_dir(out, real_snap)

    def test_output_dir_wider_permissions_rejected(self) -> None:
        with tempfile.TemporaryDirectory() as td:
            snap = Path(td) / "snap"
            out = Path(td) / "out"
            out.mkdir(parents=True, mode=0o755)
            snap.mkdir(parents=True)
            real_snap = assert_safe_workspace(snap)
            with self.assertRaises(WorkspaceError):
                assert_safe_output_dir(out, real_snap)


class TestRejectUnsafeSymlinks(unittest.TestCase):
    def test_no_symlinks_returns_empty(self) -> None:
        with tempfile.TemporaryDirectory() as td:
            self.assertEqual(reject_unsafe_symlinks(Path(td)), [])

    def test_internal_symlink_accepted(self) -> None:
        with tempfile.TemporaryDirectory() as td:
            target = Path(td) / "target.txt"
            target.write_text("x")
            link = Path(td) / "link.txt"
            try:
                link.symlink_to(target)
            except (OSError, NotImplementedError):
                self.skipTest("symlinks unsupported")
            self.assertEqual(reject_unsafe_symlinks(Path(td)), [])

    def test_external_symlink_rejected(self) -> None:
        with tempfile.TemporaryDirectory() as td:
            with tempfile.TemporaryDirectory() as other:
                outside = Path(other) / "outside.txt"
                outside.write_text("x")
                link = Path(td) / "link.txt"
                try:
                    link.symlink_to(outside)
                except (OSError, NotImplementedError):
                    self.skipTest("symlinks unsupported")
                bad = reject_unsafe_symlinks(Path(td))
                self.assertEqual(len(bad), 1)
                self.assertTrue(bad[0].name == "link.txt")


class TestWriteMpmConfig(unittest.TestCase):
    def test_writes_config_with_0o600(self) -> None:
        with tempfile.TemporaryDirectory() as td:
            snap = Path(td)
            path = write_mpm_config(
                snap,
                profile={"default": {"provider": "custom", "model": "x"}},
                components={"embedding": "default"},
            )
            self.assertTrue(path.exists())
            mode = stat.S_IMODE(os.stat(path).st_mode)
            self.assertEqual(mode, 0o600)
            import json
            cfg = json.loads(path.read_text())
            self.assertIn("profiles", cfg)
            self.assertIn("components", cfg)
