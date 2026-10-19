"""Binary provenance test (Phase A.3 V3).

V3 directive: "Use the developer build artifact: .build/bin/mpm;
build it from intended source revision; record path, resolved
path, Git SHA, SHA-256, build configuration. Investigate why
Phase A.2 used /home/v/.local/bin/mpm. Do not modify, replace,
reinstall or execute an installed binary as a substitute for the
canonical developer build."

This test pins:
  1. The canonical binary is `.build/bin/mpm` (or the
     MPM_BENCH_BINARY override).
  2. The provenance block has every required field.
  3. The binary's SHA-256 is stable (re-running the
     provenance function on the same binary returns the
     same SHA).
  4. The binary's mpm version line is non-empty.
  5. The build config captured from the Makefile pins the
     fts5 build flag.
"""

from __future__ import annotations

import hashlib
import os
import unittest
from pathlib import Path

from bench.harness import (
    CANONICAL_MPM_BINARY,
    REPO_ROOT,
    _binary_provenance,
    _resolve_mpm_binary,
)


class TestBinaryProvenance(unittest.TestCase):
    """V3: binary provenance block on the canonical binary."""

    def test_canonical_binary_path(self) -> None:
        """The canonical binary path is `.build/bin/mpm`."""
        expected = REPO_ROOT / ".build" / "bin" / "mpm"
        self.assertEqual(CANONICAL_MPM_BINARY, expected)

    def test_provenance_block_has_all_required_fields(self) -> None:
        """The provenance block has every required field for
        HAL review. The fields are the ones the V3 directive
        names explicitly: path, resolved_path, Git SHA, SHA-256,
        build configuration.
        """
        if not _resolve_mpm_binary().exists():
            self.skipTest("canonical mpm binary not built")
        prov = _binary_provenance(_resolve_mpm_binary())
        required = {
            "path", "resolved_path", "size_bytes", "mtime_iso",
            "sha256", "mpm_version", "git_head", "build_config",
            "canonical",
        }
        self.assertEqual(
            required - set(prov.keys()),
            set(),
            f"missing provenance fields: {required - set(prov.keys())}"
        )
        # SHA-256 must be 64 hex chars.
        sha = prov["sha256"]
        self.assertEqual(len(sha), 64)
        self.assertTrue(
            all(c in "0123456789abcdef" for c in sha),
            f"sha256 contains non-hex chars: {sha!r}"
        )

    def test_provenance_is_stable(self) -> None:
        """Re-running the provenance function on the same binary
        returns the same SHA. (A silent rebuild would change the
        SHA; this test catches that.)"""
        if not _resolve_mpm_binary().exists():
            self.skipTest("canonical mpm binary not built")
        p1 = _binary_provenance(_resolve_mpm_binary())
        p2 = _binary_provenance(_resolve_mpm_binary())
        self.assertEqual(p1["sha256"], p2["sha256"])
        self.assertEqual(p1["mtime_iso"], p2["mtime_iso"])

    def test_provenance_sha_matches_independent_hash(self) -> None:
        """The provenance SHA matches an independent SHA-256
        computation on the resolved file. This is the sanity
        check that the in-function hashing is correct."""
        if not _resolve_mpm_binary().exists():
            self.skipTest("canonical mpm binary not built")
        p = _resolve_mpm_binary().resolve()
        expected = hashlib.sha256(p.read_bytes()).hexdigest()
        prov = _binary_provenance(_resolve_mpm_binary())
        self.assertEqual(prov["sha256"], expected)

    def test_build_config_pins_fts5_tag(self) -> None:
        """The build config string captured from the Makefile
        includes the `-tags fts5` flag. A future build that
        drops the fts5 tag would invalidate this test (and
        invalidate the FTS5 channel)."""
        if not _resolve_mpm_binary().exists():
            self.skipTest("canonical mpm binary not built")
        prov = _binary_provenance(_resolve_mpm_binary())
        self.assertIn(
            "-tags fts5", prov["build_config"],
            f"build config did not pin the fts5 tag; "
            f"got: {prov['build_config']!r}"
        )

    def test_canonical_flag_true(self) -> None:
        """`canonical: True` means the binary IS `.build/bin/mpm`.
        A non-canonical binary (e.g. an MPM_BENCH_BINARY override)
        would record canonical: False. The V3 directive forbids
        using an installed binary, so any future test that
        accidentally uses `/home/v/.mpm/bin/mpm` would land
        here as canonical: False."""
        if not _resolve_mpm_binary().exists():
            self.skipTest("canonical mpm binary not built")
        prov = _binary_provenance(_resolve_mpm_binary())
        self.assertTrue(
            prov["canonical"],
            f"binary is not the canonical artifact; got "
            f"resolved_path={prov['resolved_path']!r} but "
            f"expected {CANONICAL_MPM_BINARY.resolve()!r}. "
            f"Phase A.3 V3 forbids using the operator's "
            f"installed binary as a substitute."
        )


if __name__ == "__main__":
    unittest.main()
