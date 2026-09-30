"""
test_refresh_installed_openclaw_safety.py — OpenClaw reconciliation
safety contract.

Two distinct properties are pinned here, and they are NOT the same
property:

  SAFETY BOUNDARY (must never regress)
    MPM must not install or opt a machine into OpenClaw merely
    because OpenClaw integration code exists in the repository. A
    machine with no OpenClaw MPM integration stays untouched by
    every documented update path.

  MANAGED-BLOCK CONTRACT (the current contract)
    Once the OpenClaw MPM integration IS already installed, the
    persistent managed block is a REQUIRED part of that integration.
    A missing or stale block is drift that reconciliation repairs;
    a current block is a byte-stable no-op. Dynamic wake injection
    supplements the block, it does not replace it.

The old version of this file pinned the superseded assumption that
"OpenClaw is intentionally absent" and that no path may ever write
SOUL.md. That assumption was invalidated by direct runtime evidence:
without the managed block an OpenClaw agent can have MPM tools
exposed and still fail to use MPM, and with the block it discovers
and uses MPM reliably.

Covers:
  - make refresh-installed (the standalone reconciliation target)
  - make install (which depends on refresh-installed)
  - ./install.sh --reconcile (content-only update mode)
  - ./install.sh (full install, including the reconciliation phase)

All tests use a sandboxed HOME so the real machine state — including
the real installed SOUL.md — is never touched.

Two integration MODES are covered, because OpenClaw supports both and
a machine legitimately running either one must have its block repaired:

  native plugin   mpm-memory-openclaw installed as an OpenClaw plugin
                  -> typed mpm_memory_* tools + runtime wake injection
  CLI fallback    no MPM plugin, but the agent reaches MPM through
                  `mpm call <tool>` over its shell tool

The CLI-fallback mode is the one a live investigation surfaced on the
real machine, and the one a plugin-only detector misses entirely. It is
a supported mode, not a degraded one: the managed block is what tells
the agent the `mpm call` fallback exists, so on a CLI-fallback host a
missing block means the agent never learns MPM is reachable at all.
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


REPO_ROOT = Path(__file__).resolve().parent.parent.parent
AGENT_INSTALLATION = REPO_ROOT / "agent_installation"
MAKEFILE = REPO_ROOT / "Makefile"
INSTALL_SH = REPO_ROOT / "install.sh"
RECONCILE = AGENT_INSTALLATION / "scripts" / "reconcile_managed_blocks.py"
CHECK_INSTALLED = AGENT_INSTALLATION / "scripts" / "check_installed_managed_blocks.py"


def _sandbox_home() -> tuple[Path, dict]:
    """Create an isolated HOME and return (tmpdir, env)."""
    tmp = Path(tempfile.mkdtemp(prefix="mpm-openclaw-safety-"))
    home = tmp / "home"
    home.mkdir(parents=True, exist_ok=True)
    env = os.environ.copy()
    env["HOME"] = str(home)
    return tmp, env


def _cleanup(tmp: Path) -> None:
    shutil.rmtree(tmp, ignore_errors=True)


def _run(cmd: list[str], *, env: dict, timeout: int = 240) -> subprocess.CompletedProcess:
    return subprocess.run(
        cmd, capture_output=True, text=True, env=env, timeout=timeout,
    )


def _openclaw_target(home: Path) -> Path:
    """The canonical OpenClaw install target (per
    check_installed_managed_blocks.HOST_INSTALL_TARGETS)."""
    return home / ".openclaw" / "workspace" / "SOUL.md"


def _install_integration(home: Path) -> None:
    """Simulate NATIVE PLUGIN mode: mpm-memory-openclaw installed.

    Mirrors what `mpm-memory-openclaw/install.sh` produces: a
    registered plugin entry in the host config. The reconciler's
    `reconcile_if` probes key off exactly this.
    """
    extensions = home / ".openclaw" / "extensions" / "mpm-memory-openclaw"
    extensions.mkdir(parents=True, exist_ok=True)
    config = home / ".openclaw" / "openclaw.json"
    config.parent.mkdir(parents=True, exist_ok=True)
    config.write_text(json.dumps({
        "plugins": {"entries": {"mpm-memory-openclaw": {"enabled": True}}},
    }), encoding="utf-8")


def _install_cli_fallback_integration(home: Path) -> None:
    """Simulate CLI-FALLBACK mode — the real machine's shape.

    No MPM plugin anywhere. Instead, exactly the two facts the live
    investigation established:

      reachability  the mpm binary is installed where an OpenClaw
                    agent's shell tool can find it
      intent        the operator already wired MPM into THIS
                    OpenClaw workspace's persistent instruction
                    surface (AGENTS.md / MEMORY.md), which is where
                    the agent is told the `mpm call` fallback exists

    Note SOUL.md itself is left without a managed block — that is the
    drift this mode must still be able to repair.
    """
    (home / ".mpm" / "bin").mkdir(parents=True, exist_ok=True)
    (home / ".mpm" / "bin" / "mpm").write_text("#!/bin/sh\n", encoding="utf-8")

    workspace = home / ".openclaw" / "workspace"
    workspace.mkdir(parents=True, exist_ok=True)
    (workspace / "AGENTS.md").write_text(
        "# AGENTS.md — Workspace\n\n"
        "## MPM — Memory Persistence Module (Mandatory)\n\n"
        "MPM is your native SQLite-backed persistence layer.\n"
        "MPM is the source of truth for agent state.\n",
        encoding="utf-8",
    )
    (workspace / "MEMORY.md").write_text(
        "# MEMORY.md\n\n"
        "**On every session start:** call `read_wake_context`.\n"
        "**MPM interface:** all operations use `mpm call <tool> --payload JSON`.\n",
        encoding="utf-8",
    )

    config = home / ".openclaw" / "openclaw.json"
    config.parent.mkdir(parents=True, exist_ok=True)
    config.write_text(json.dumps({
        "agents": {
            "defaults": {"workspace": str(workspace)},
            "entries": {"main": {"workspace": str(workspace)}},
        },
        "plugins": {"entries": {"anthropic": {"enabled": True}}},
    }), encoding="utf-8")


def _install_unrelated_openclaw(home: Path) -> None:
    """An OpenClaw install that has never been integrated with MPM.

    MPM IS installed on this machine (so reachability alone is
    satisfied) and OpenClaw IS installed with a real workspace — but
    the workspace has never heard of MPM. Reconciliation must leave it
    alone. This is the false-positive the conjunction gate exists to
    prevent.
    """
    (home / ".mpm" / "bin").mkdir(parents=True, exist_ok=True)
    (home / ".mpm" / "bin" / "mpm").write_text("#!/bin/sh\n", encoding="utf-8")
    workspace = home / ".openclaw" / "workspace"
    workspace.mkdir(parents=True, exist_ok=True)
    (workspace / "SOUL.md").write_text(
        "# SOUL.md\n\n***A persona that predates MPM entirely.***\n",
        encoding="utf-8",
    )
    (workspace / "AGENTS.md").write_text(
        "# AGENTS.md — Workspace\n\nThis folder is home. Nothing else.\n",
        encoding="utf-8",
    )
    config = home / ".openclaw" / "openclaw.json"
    config.parent.mkdir(parents=True, exist_ok=True)
    config.write_text(json.dumps({
        "agents": {
            "defaults": {"workspace": str(workspace)},
            "entries": {"main": {"workspace": str(workspace)}},
        },
        "plugins": {"entries": {"anthropic": {"enabled": True}}},
    }), encoding="utf-8")


def _reconcile(home: Path, *extra: str) -> subprocess.CompletedProcess:
    return _run(
        [sys.executable, str(RECONCILE), "--home", str(home), *extra],
        env=os.environ.copy(),
    )


# ---------------------------------------------------------------------------
# 1. Safety boundary: a machine without OpenClaw is never opted in.
# ---------------------------------------------------------------------------

class OpenClawNotOptedIn(unittest.TestCase):
    """The safety boundary that motivated the original file, kept
    intact: MPM must not install OpenClaw on a machine that does not
    use it."""

    def setUp(self):
        self.tmpdir, self.env = _sandbox_home()
        self.home = self.tmpdir / "home"
        oc = _openclaw_target(self.home)
        if oc.exists():
            oc.unlink()

    def tearDown(self):
        _cleanup(self.tmpdir)

    def _assert_openclaw_untouched(self, oc: Path, label: str, stderr: str):
        self.assertFalse(
            oc.exists(),
            f"{label}: must not create {oc} — MPM must not opt a machine "
            f"into a host it does not already use. stderr={stderr[:500]!r}",
        )
        if oc.parent.exists():
            remaining = list(oc.parent.iterdir())
            self.assertEqual(
                remaining, [],
                f"{label}: openclaw parent dir should be empty, found {remaining}",
            )

    def test_reconcile_does_not_install_openclaw_without_integration(self):
        """The generic reconciler must leave a machine with no
        OpenClaw MPM integration completely alone."""
        result = _reconcile(self.home)
        self.assertEqual(
            result.returncode, 0,
            f"reconcile failed:\nstdout={result.stdout}\nstderr={result.stderr}",
        )
        self.assertIn(
            "integration not installed", result.stdout,
            "the reconciler must report the integration gate as closed",
        )
        self._assert_openclaw_untouched(
            _openclaw_target(self.home), "reconcile_managed_blocks.py",
            result.stderr,
        )

    def test_make_refresh_installed_does_not_install_openclaw(self):
        result = _run(
            ["make", "-C", str(REPO_ROOT), "refresh-installed"],
            env=self.env,
        )
        self.assertEqual(
            result.returncode, 0,
            f"make refresh-installed failed:\nstdout={result.stdout}\n"
            f"stderr={result.stderr}",
        )
        self._assert_openclaw_untouched(
            _openclaw_target(self.home),
            "make refresh-installed",
            result.stderr,
        )

    def test_make_install_does_not_install_openclaw(self):
        """make install is the documented update entry point."""
        result = _run(
            ["make", "-n", "-C", str(REPO_ROOT), "install"],
            env=self.env,
        )
        self.assertEqual(
            result.returncode, 0,
            f"make -n install failed:\nstdout={result.stdout}\n"
            f"stderr={result.stderr}",
        )
        # The dependency chain must include refresh-installed.
        self.assertIn(
            "refreshing host installed artifacts from canonical source",
            result.stdout,
            "make -n install must include refresh-installed steps in its "
            "dependency chain, or the documented `git pull && make install` "
            "flow cannot guarantee managed-block reconciliation.",
        )
        self._assert_openclaw_untouched(
            _openclaw_target(self.home), "make -n install", result.stderr,
        )

    def test_install_sh_reconcile_does_not_install_openclaw(self):
        result = _run(
            ["bash", str(INSTALL_SH), "--reconcile"],
            env=self.env, timeout=180,
        )
        self.assertEqual(
            result.returncode, 0,
            f"install.sh --reconcile failed:\nstdout={result.stdout}\n"
            f"stderr={result.stderr}",
        )
        self._assert_openclaw_untouched(
            _openclaw_target(self.home), "install.sh --reconcile", result.stderr,
        )

    def test_install_sh_does_not_install_openclaw(self):
        result = _run(
            ["bash", str(INSTALL_SH)],
            env=self.env, timeout=180,
        )
        self._assert_openclaw_untouched(
            _openclaw_target(self.home), "install.sh (full)", result.stderr,
        )


# ---------------------------------------------------------------------------
# 2. Managed-block contract: an installed integration is repaired.
# ---------------------------------------------------------------------------

class OpenClawManagedBlockRepaired(unittest.TestCase):
    """Once the integration is installed, the persistent managed block
    is required and reconciliation converges it."""

    def setUp(self):
        self.tmpdir, self.env = _sandbox_home()
        self.home = self.tmpdir / "home"
        _install_integration(self.home)
        self.target = _openclaw_target(self.home)

    def tearDown(self):
        _cleanup(self.tmpdir)

    def test_missing_block_is_installed(self):
        """Integration installed + block missing -> block is installed."""
        self.assertFalse(self.target.exists())
        result = _reconcile(self.home, "--only", "mpm-memory-openclaw")
        self.assertEqual(
            result.returncode, 0,
            f"reconcile failed:\nstdout={result.stdout}\nstderr={result.stderr}",
        )
        self.assertTrue(
            self.target.is_file(),
            "reconciliation must install the managed block when the "
            "OpenClaw MPM integration is already present",
        )
        text = self.target.read_text(encoding="utf-8")
        self.assertIn(
            "<!-- BEGIN MPM-MANAGED SECTION:openclaw-instructions -->", text,
        )

    def test_stale_block_is_refreshed(self):
        """Integration installed + block stale -> block is refreshed to
        the current canonical render."""
        self.target.parent.mkdir(parents=True, exist_ok=True)
        self.target.write_text(
            "persona line above\n\n"
            "<!-- BEGIN MPM-MANAGED SECTION:openclaw-instructions -->\n"
            "stale body from an old contract\n"
            "<!-- END MPM-MANAGED SECTION:openclaw-instructions -->\n"
            "user line below\n",
            encoding="utf-8",
        )
        result = _reconcile(self.home, "--only", "mpm-memory-openclaw")
        self.assertEqual(
            result.returncode, 0,
            f"reconcile failed:\nstdout={result.stdout}\nstderr={result.stderr}",
        )
        text = self.target.read_text(encoding="utf-8")
        self.assertNotIn("stale body from an old contract", text)
        self.assertIn("MPM behavioural contract", text)

    def test_user_content_outside_block_preserved_byte_for_byte(self):
        """Content above and below the managed markers must survive a
        refresh byte-for-byte."""
        above = "# SOUL.md\n\npersona: 808\n\nuser notes line one\nuser notes line two\n"
        below = "\ntrailing user content\nsecond trailing line\n"
        self.target.parent.mkdir(parents=True, exist_ok=True)
        _reconcile(self.home, "--only", "mpm-memory-openclaw")
        self.assertTrue(self.target.is_file())
        # Inject user content around the freshly installed block, then
        # force a refresh.
        text = self.target.read_text(encoding="utf-8")
        begin = "<!-- BEGIN MPM-MANAGED SECTION:openclaw-instructions -->"
        end = "<!-- END MPM-MANAGED SECTION:openclaw-instructions -->"
        b = text.find(begin)
        e = text.find(end) + len(end) + 1
        self.target.write_text(above + text[b:e] + below, encoding="utf-8")

        # Drift the block so the second run has real work to do.
        stale = self.target.read_text(encoding="utf-8").replace(
            "MPM behavioural contract", "MPM stale heading",
        )
        self.target.write_text(stale, encoding="utf-8")

        result = _reconcile(self.home, "--only", "mpm-memory-openclaw")
        self.assertEqual(
            result.returncode, 0,
            f"reconcile failed:\nstdout={result.stdout}\nstderr={result.stderr}",
        )
        after = self.target.read_text(encoding="utf-8")
        self.assertTrue(
            after.startswith(above),
            f"content above the managed block must be preserved verbatim.\n"
            f"--- got ---\n{after[:400]!r}",
        )
        self.assertTrue(
            after.endswith(below),
            f"content below the managed block must be preserved verbatim.\n"
            f"--- got tail ---\n{after[-200:]!r}",
        )
        self.assertIn("MPM behavioural contract", after)

    def test_second_reconciliation_is_a_noop(self):
        """Reconciliation is idempotent: a second run leaves the file
        byte-identical."""
        _reconcile(self.home, "--only", "mpm-memory-openclaw")
        first = self.target.read_bytes()
        result = _reconcile(self.home, "--only", "mpm-memory-openclaw")
        self.assertEqual(result.returncode, 0, result.stderr)
        second = self.target.read_bytes()
        self.assertEqual(
            first, second,
            "a second reconciliation must be a byte-stable no-op",
        )


# ---------------------------------------------------------------------------
# 3. Diagnostics report the new contract.
# ---------------------------------------------------------------------------

class OpenClawDiagnostics(unittest.TestCase):
    """Installed-block diagnostics must not report an installed OpenClaw
    with a missing block as a healthy intentional state."""

    def setUp(self):
        self.tmpdir, _ = _sandbox_home()
        self.home = self.tmpdir / "home"
        self.target = _openclaw_target(self.home)

    def tearDown(self):
        _cleanup(self.tmpdir)

    def _check(self) -> subprocess.CompletedProcess:
        return _run(
            [sys.executable, str(CHECK_INSTALLED), "--home", str(self.home), "--json"],
            env=os.environ.copy(),
        )

    def _openclaw_row(self, proc: subprocess.CompletedProcess) -> dict:
        rows = json.loads(proc.stdout)
        return next(r for r in rows if r["host"] == "openclaw")

    def test_pass_when_block_current(self):
        _install_integration(self.home)
        _reconcile(self.home, "--only", "mpm-memory-openclaw")
        row = self._openclaw_row(self._check())
        self.assertEqual(row["verdict"], "PASS", row["detail"])

    def test_warn_when_block_missing_on_installed_integration(self):
        _install_integration(self.home)
        self.target.parent.mkdir(parents=True, exist_ok=True)
        self.target.write_text("persona only, no block\n", encoding="utf-8")
        proc = self._check()
        row = self._openclaw_row(proc)
        self.assertEqual(
            row["verdict"], "WARN",
            "an installed OpenClaw integration with a missing managed block "
            "is drift, not a healthy intentional state",
        )
        self.assertEqual(row["file"], "present")
        self.assertEqual(row["block"], "absent")

    def test_warn_when_block_stale_on_installed_integration(self):
        _install_integration(self.home)
        self.target.parent.mkdir(parents=True, exist_ok=True)
        self.target.write_text(
            "<!-- BEGIN MPM-MANAGED SECTION:openclaw-instructions -->\n"
            "stale\n"
            "<!-- END MPM-MANAGED SECTION:openclaw-instructions -->\n",
            encoding="utf-8",
        )
        row = self._openclaw_row(self._check())
        self.assertEqual(row["verdict"], "WARN", row["detail"])

    def test_absent_when_integration_not_installed(self):
        """A machine that does not use OpenClaw reports ABSENT, which is
        informational and not a failure."""
        self.target.parent.mkdir(parents=True, exist_ok=True)
        self.target.write_text("persona only, no block\n", encoding="utf-8")
        proc = self._check()
        row = self._openclaw_row(proc)
        self.assertEqual(
            row["verdict"], "ABSENT",
            "OpenClaw integration is not installed here, so this is "
            "informational",
        )
        self.assertEqual(
            proc.returncode, 0,
            f"ABSENT must remain informational (exit 0); stdout={proc.stdout}",
        )


# ---------------------------------------------------------------------------
# 5. CLI-fallback mode — the real machine's shape.
# ---------------------------------------------------------------------------

class OpenClawCliFallbackMode(unittest.TestCase):
    """The regression that matters most.

    A live investigation of the real machine established that its
    OpenClaw reaches MPM with NO mpm-memory-openclaw plugin installed
    at all: no `~/.openclaw/extensions/`, no `plugins.entries.*`, no
    `plugins.slots.memory`. The agent reaches MPM through `mpm call
    <tool>` over its shell tool, and the managed block is what tells
    it that fallback exists.

    A plugin-only detector classifies that machine as "integration not
    installed" and therefore never repairs the block on the exact
    machine whose runtime behaviour proved the block is required.

    These tests pin both halves of the corrected rule: the real shape
    IS reconciled, and an unrelated OpenClaw is still left alone.
    """

    def setUp(self):
        self.tmpdir, _ = _sandbox_home()
        self.home = self.tmpdir / "home"
        self.target = _openclaw_target(self.home)

    def tearDown(self):
        _cleanup(self.tmpdir)

    def _check(self) -> subprocess.CompletedProcess:
        return _run(
            [sys.executable, str(CHECK_INSTALLED), "--home", str(self.home), "--json"],
            env=os.environ.copy(),
        )

    def _openclaw_row(self, proc: subprocess.CompletedProcess) -> dict:
        rows = json.loads(proc.stdout)
        return next(r for r in rows if r["host"] == "openclaw")

    def test_no_plugin_present_in_this_fixture(self):
        """Guards the fixture itself: if a future edit adds a plugin
        entry, this class stops testing CLI-fallback mode and would
        pass for the wrong reason."""
        _install_cli_fallback_integration(self.home)
        self.assertFalse((self.home / ".openclaw" / "extensions").exists())
        config = json.loads(
            (self.home / ".openclaw" / "openclaw.json").read_text(encoding="utf-8")
        )
        self.assertNotIn("mpm-memory-openclaw", config["plugins"]["entries"])

    def test_missing_block_on_cli_fallback_host_is_repaired(self):
        """THE regression. Real-machine shape + missing managed block
        -> reconciliation writes the block."""
        _install_cli_fallback_integration(self.home)
        self.assertFalse(
            self.target.exists(),
            "fixture precondition: SOUL.md starts with no managed block",
        )

        result = _reconcile(self.home, "--only", "mpm-memory-openclaw")

        self.assertEqual(
            result.returncode, 0,
            f"reconcile must succeed on a CLI-fallback host. "
            f"stdout={result.stdout!r} stderr={result.stderr!r}",
        )
        self.assertIn("openclaw", result.stdout)
        self.assertNotIn(
            "SKIP", result.stdout,
            "a CLI-fallback host must NOT be skipped as 'integration not "
            f"installed'. stdout={result.stdout!r}",
        )
        self.assertTrue(
            self.target.exists(),
            "the managed block must be written on a CLI-fallback host",
        )
        self.assertIn(
            "MPM MANAGED BLOCK", self.target.read_text(encoding="utf-8"),
            "written file must contain the canonical managed block",
        )

    def test_block_converges_and_second_run_is_a_no_op(self):
        """Repair converges; re-running changes nothing."""
        _install_cli_fallback_integration(self.home)
        _reconcile(self.home, "--only", "mpm-memory-openclaw")
        first = self.target.read_bytes()

        _reconcile(self.home, "--only", "mpm-memory-openclaw")
        second = self.target.read_bytes()

        self.assertEqual(
            first, second,
            "a second reconcile on a converged CLI-fallback host must be "
            "byte-stable",
        )

    def test_unrelated_openclaw_is_still_untouched(self):
        """MPM is installed AND OpenClaw is installed, but this
        OpenClaw was never integrated. Reachability alone must not opt
        it in."""
        _install_unrelated_openclaw(self.home)
        before = self.target.read_bytes()

        result = _reconcile(self.home, "--only", "mpm-memory-openclaw")

        self.assertIn(
            "integration not installed", result.stdout,
            f"an unrelated OpenClaw must be skipped. stdout={result.stdout!r}",
        )
        self.assertEqual(
            before, self.target.read_bytes(),
            "an unrelated OpenClaw's SOUL.md must be byte-for-byte untouched",
        )

    def test_diagnostic_agrees_with_reconciliation(self):
        """Reconciliation and the diagnostic must not disagree.

        Both derive from the reconciler's `integration_installed`, so a
        divergence here means one of them stopped sharing the gate.
        """
        _install_cli_fallback_integration(self.home)
        self.target.parent.mkdir(parents=True, exist_ok=True)
        self.target.write_text("persona only, no block\n", encoding="utf-8")

        row = self._openclaw_row(self._check())
        self.assertEqual(
            row["verdict"], "WARN",
            "diagnostic must treat a CLI-fallback host with a missing block "
            f"as drift, not as an unused host. detail={row['detail']!r}",
        )
        self.assertEqual(row["file"], "present")
        self.assertEqual(row["block"], "absent")

    def test_diagnostic_reports_absent_for_unrelated_openclaw(self):
        """The other half of the consistency property."""
        _install_unrelated_openclaw(self.home)
        proc = self._check()
        row = self._openclaw_row(proc)
        self.assertEqual(
            row["verdict"], "ABSENT",
            f"an unrelated OpenClaw is informational, not drift. "
            f"detail={row['detail']!r}",
        )
        self.assertEqual(proc.returncode, 0)

    def test_diagnostic_reports_pass_after_repair(self):
        """Once repaired, a CLI-fallback host reads PASS."""
        _install_cli_fallback_integration(self.home)
        _reconcile(self.home, "--only", "mpm-memory-openclaw")
        row = self._openclaw_row(self._check())
        self.assertEqual(
            row["verdict"], "PASS",
            f"a repaired CLI-fallback host must read PASS. detail={row['detail']!r}",
        )


if __name__ == "__main__":
    unittest.main(verbosity=2)
