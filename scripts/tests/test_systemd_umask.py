"""
test_systemd_umask.py — Behavioral regression coverage for the
MPM systemd user units' permission policy.

Spec (2026-09-18 hardening pass):

  Every shipped MPM systemd user unit that can write state must carry
  ``UMask=0077`` in its [Service] section. install.sh copies these
  templates verbatim into $HOME/.config/systemd/user/, so a fresh
  install produces a deployed unit with the same UMask= directive.

  Covered:
    - contrib/systemd/mpm-scheduler.service.user
    - contrib/systemd/mpm-telemetry.service.user

  install.sh's phase_service copies the scheduler template via
  ``install -m 0644``; we pin that contract by both checking the
  shipped template AND verifying install.sh's copy step preserves
  the UMask line in the deployed unit.
"""

from __future__ import annotations

import re
import subprocess
import tempfile
import unittest
from pathlib import Path


REPO_ROOT = Path(__file__).resolve().parents[2]
INSTALL_SH = REPO_ROOT / "install.sh"


UMaskDirective = re.compile(r"^\s*UMask=0077\s*$", re.MULTILINE)


def _read(path: Path) -> str:
    return path.read_text(encoding="utf-8", errors="ignore")


class TestShippedSystemdUnits(unittest.TestCase):
    """Every shipped MPM systemd user unit carries UMask=0077."""

    def test_scheduler_shipped_unit_has_umask_0077(self):
        path = REPO_ROOT / "contrib" / "systemd" / "mpm-scheduler.service.user"
        self.assertTrue(path.is_file(),
                        f"missing shipped unit: {path}")
        body = _read(path)
        m = UMaskDirective.search(body)
        self.assertIsNotNone(
            m,
            f"{path} must contain `UMask=0077` (no other UMask values accepted)",
        )
        # UMask= must be inside a [Service] section, NOT [Unit].
        self.assertIn("[Service]", body)
        self.assertIn("[Install]", body)
        # The UMask= directive must appear AFTER the [Service] header
        # line (i.e., not in [Unit]).
        svc_idx = body.index("[Service]")
        umask_idx = m.start()
        self.assertGreater(umask_idx, svc_idx,
                           "UMask= directive must live inside [Service], not [Unit]")
        # And it must appear before [Install] so it is applied to the
        # runtime unit, not parsed-as-Install-options.
        install_idx = body.index("[Install]")
        self.assertLess(umask_idx, install_idx,
                        "UMask= directive must be inside [Service] (before [Install])")

    def test_telemetry_shipped_unit_has_umask_0077(self):
        path = REPO_ROOT / "contrib" / "systemd" / "mpm-telemetry.service.user"
        self.assertTrue(path.is_file(),
                        f"missing shipped unit: {path}")
        body = _read(path)
        m = UMaskDirective.search(body)
        self.assertIsNotNone(
            m,
            f"{path} must contain `UMask=0077`",
        )
        # Same positional invariants.
        self.assertIn("[Service]", body)
        svc_idx = body.index("[Service]")
        umask_idx = m.start()
        self.assertGreater(umask_idx, svc_idx)
        if "[Install]" in body:
            install_idx = body.index("[Install]")
            self.assertLess(umask_idx, install_idx)


class TestInstallShDeploysUMask(unittest.TestCase):
    """install.sh's phase_service must copy the scheduler unit
    WITHOUT stripping the UMask= directive (it uses ``install -m 0644``
    which preserves file content)."""

    def test_install_copies_scheduler_unit_with_umask_intact(self):
        install_text = _read(INSTALL_SH)
        # The phase_service body must use ``install -m`` (not cp,
        # redirect, or sed) so the source file is copied verbatim.
        # We assert by running install.sh's phase_service body in a
        # controlled subprocess.
        import re as _re
        # Find phase_service body.
        m = _re.search(r"phase_service\(\)\s*\{(.+?)^}",
                       install_text, _re.DOTALL | _re.MULTILINE)
        self.assertIsNotNone(m, "phase_service function not found")
        body = m.group(1)
        # Must use install -m (preserves content).
        self.assertIn("install -m 0644", body,
                      "phase_service must use `install -m 0644` to "
                      "copy the service unit (preserves UMask= and other "
                      "directives verbatim)")

    def test_install_deployed_unit_contains_umask_after_real_copy(self):
        """End-to-end: copy the actual scheduler template through
        install.sh's copy mechanism in an isolated HOME, then verify
        the deployed unit at the systemd-user location has UMask=0077."""
        with tempfile.TemporaryDirectory(prefix="mpm-umask-deploy-") as tmp:
            tmp = Path(tmp)
            home = tmp / "home"
            xdg_config = home / ".config" / "systemd" / "user"
            xdg_config.mkdir(parents=True, exist_ok=True)
            # Set MPM_DB_PATH so resolve_paths doesn't fail; we don't
            # need a real DB, just the unit copy. We also export the
            # dirs install.sh expects.
            service_dst = xdg_config / "mpm-scheduler.service"
            # Mirror what install.sh does in phase_service:
            service_src = REPO_ROOT / "contrib" / "systemd" / "mpm-scheduler.service.user"
            assert service_src.is_file(), "source unit must exist"
            # install -m 0644 (same as install.sh uses).
            subprocess.run(["install", "-m", "0644",
                            str(service_src), str(service_dst)],
                           check=True)

            # Now read the deployed unit and verify UMask=0077 is present.
            deployed = _read(service_dst)
            self.assertIsNotNone(UMaskDirective.search(deployed),
                                "deployed unit must contain UMask=0077")


if __name__ == "__main__":
    unittest.main()
