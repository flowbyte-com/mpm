"""
test_install_update_path.py — End-to-end regression for the
git-pull-then-update-path contract.

Demonstrates:
    stale installed managed block (Pi / Claude Code / OpenCode)
        ↓
    ./install.sh --reconcile  (the documented update entry point)
        ↓
    canonical block installed
    surrounding user content preserved
    second invocation is a no-op (no backup created)

Run with: python3 -m unittest tests.test_install_update_path -v

This is an integration test. It does NOT touch the real home
directory. It uses a tmp HOME redirect so the per-host installers
target a sandbox under tmp_path.
"""

from __future__ import annotations

import os
import shutil
import subprocess
import sys
import unittest
from pathlib import Path


REPO_ROOT = Path(__file__).resolve().parent.parent.parent
INSTALL_SH = REPO_ROOT / "install.sh"
RENDER_SH = REPO_ROOT / "agent_installation" / "scripts" / "render_managed_blocks.py"
CHECK_INSTALLED = REPO_ROOT / "agent_installation" / "scripts" / "check_installed_managed_blocks.py"
CANONICAL_SOURCE = REPO_ROOT / "agent_installation" / "MPM_AGENT_INTEGRATION_SNIPPETS.md"

# Per-host target paths expressed as relative-to-HOME strings (the
# same keys check_installed_managed_blocks.HOST_INSTALL_TARGETS uses).
# Each host's BEGIN/END markers are host-specific (the installer only
# recognizes its own markers when extracting a managed block), so the
# stale fixture must use the canonical host-specific markers for the
# installer to detect the existing block and replace it in-place.
HOST_TARGETS = {
    "pi":          ".pi/agent/AGENTS.md",
    "claude_code": ".claude/CLAUDE.md",
    "opencode":    ".config/opencode/AGENTS.md",
    # openclaw is intentionally absent on this machine; not exercised
    # by this regression.
}
HOST_MARKERS = {
    "pi":          (
        "<!-- BEGIN MPM-MANAGED SECTION:pi-instructions -->",
        "<!-- END MPM-MANAGED SECTION:pi-instructions -->",
    ),
    "claude_code": (
        "<!-- BEGIN MPM-MANAGED SECTION:claude-code-instructions -->",
        "<!-- END MPM-MANAGED SECTION:claude-code-instructions -->",
    ),
    "opencode":    (
        "<!-- BEGIN MPM-MANAGED SECTION:opencode-instructions -->",
        "<!-- END MPM-MANAGED SECTION:opencode-instructions -->",
    ),
}


def _run(cmd: list[str], *, env: dict, timeout: int = 120) -> subprocess.CompletedProcess:
    """Run a subprocess and raise on non-zero exit."""
    return subprocess.run(cmd, capture_output=True, text=True, env=env, timeout=timeout, check=True)


def _canonical_block_for(adapter_name: str) -> str:
    """Render the canonical managed block for the given adapter.

    Uses the same renderer the diagnostic script uses, so the test
    asserts against the actual production byte stream.
    """
    import importlib
    spec = importlib.util.spec_from_file_location(
        "_render_under_test", RENDER_SH,
    )
    render = importlib.util.module_from_spec(spec)
    spec.loader.exec_module(render)
    canonical_text = CANONICAL_SOURCE.read_text(encoding="utf-8")
    canonical_block = render.extract_canonical_block(canonical_text)
    adapter = next(a for a in render.ADAPTERS if a["name"] == adapter_name)
    rendered = render.render_for_host(canonical_block, adapter["tool_prefix"])
    snippet = render.compose_snippet(rendered, adapter["header"], adapter["footer"])
    return f'{adapter["copy_paste_outer_begin"]}\n{snippet}{adapter["copy_paste_outer_end"]}\n'


def _make_stale_target(host: str, home: Path) -> None:
    """Seed a stale installed file under `home` for the given host.

    The stale body uses the host-specific managed-section markers
    (matching what each per-host installer would have written) but
    with placeholder content. This is the realistic stale shape —
    earlier versions of MPM wrote these exact markers, just with
    a different body. Each per-host installer detects its own
    markers and replaces the body in place.
    """
    rel = HOST_TARGETS[host]
    target = home / rel
    target.parent.mkdir(parents=True, exist_ok=True)
    user_above = f"# user notes above managed block ({host})\nkeep me safe.\n"
    user_below = f"\n# user notes below managed block ({host})\nalso keep me safe.\n"
    begin, end = HOST_MARKERS[host]
    stale_body = (
        f"{begin}\n"
        f"## STALE managed block from an older canonical contract ({host})\n"
        f"- this body is intentionally outdated\n"
        f"- install should replace it with current canonical content\n"
        f"{end}\n"
    )
    target.write_text(user_above + stale_body + user_below)


def _read_managed_section(path: Path) -> str | None:
    """Extract the contents between the managed-section markers, or None.

    Returns the substring starting at the BEGIN marker line and
    ending at (and including) the trailing newline of the END marker
    line, so it byte-matches the canonical renderer output shape.
    """
    text = path.read_text(encoding="utf-8")
    begin_prefix = "<!-- BEGIN MPM-MANAGED SECTION"
    end_prefix = "<!-- END MPM-MANAGED SECTION"
    if begin_prefix not in text or end_prefix not in text:
        return None
    b = text.find(begin_prefix)
    e = text.find(end_prefix, b + len(begin_prefix))
    if e == -1:
        return None
    # Include the END marker line (and its trailing newline if any).
    end_line_end = text.find("\n", e)
    if end_line_end == -1:
        end_line_end = len(text)
    else:
        end_line_end += 1
    return text[b:end_line_end]


class UpdatePathRegression(unittest.TestCase):
    """End-to-end: stale installed managed blocks → ./install.sh --reconcile
    → canonical blocks installed, user content preserved, no-op on re-run.
    """

    def setUp(self):
        # Sandbox HOME: per-host installers all derive their target
        # paths from $HOME. Using a fresh tmp dir isolates the test
        # from the operator's real install.
        self.tmpdir = Path(__import__("tempfile").mkdtemp(prefix="mpm-update-path-"))
        self.home = self.tmpdir / "home"
        self.home.mkdir(parents=True, exist_ok=True)

        # Per-host installers dispatch on real adapter dirs; the
        # adapters live in the source tree at REPO_ROOT/agent_installation/.
        # We don't move them; we just point HOME at the sandbox.
        self.env = os.environ.copy()
        self.env["HOME"] = str(self.home)

        # Seed three stale installed files (one per host with a
        # persistent managed block). The fourth host (openclaw) is
        # intentionally absent and must remain absent after the
        # reconcile pass.
        for host in ("pi", "claude_code", "opencode"):
            _make_stale_target(host, self.home)

    def tearDown(self):
        shutil.rmtree(self.tmpdir, ignore_errors=True)

    # ---------- step 1: documented update entry point reconciles ----------

    def test_install_sh_reconcile_replaces_stale_blocks(self):
        """`./install.sh --reconcile` is the documented content-only
        update entry point. It must invoke the per-host installers and
        converge installed managed blocks to the current canonical
        render, preserving user content outside the managed section.
        """
        result = _run(
            ["bash", str(INSTALL_SH), "--reconcile"],
            env=self.env, timeout=180,
        )
        # The reconcile step should not silently fail.
        self.assertEqual(
            result.returncode, 0,
            f"install.sh --reconcile failed:\nstdout={result.stdout}\nstderr={result.stderr}",
        )

        # Verify each host's installed block now matches canonical.
        for host, rel in HOST_TARGETS.items():
            target = self.home / rel
            self.assertTrue(
                target.exists(),
                f"{host}: install target missing after reconcile at {target}",
            )
            installed = _read_managed_section(target)
            self.assertIsNotNone(
                installed,
                f"{host}: no managed section in installed file",
            )
            canonical = _canonical_block_for(
                {"pi": "mpm-pi", "claude_code": "mpm-claude-code", "opencode": "mpm-opencode"}[host]
            )
            self.assertEqual(
                installed, canonical,
                f"{host}: installed block drifted from canonical render "
                f"after ./install.sh --reconcile",
            )

    def test_install_sh_reconcile_preserves_user_content(self):
        """User content above and below the managed block must survive."""
        result = _run(
            ["bash", str(INSTALL_SH), "--reconcile"],
            env=self.env, timeout=180,
        )
        self.assertEqual(result.returncode, 0, result.stderr)

        for host, rel in HOST_TARGETS.items():
            text = (self.home / rel).read_text(encoding="utf-8")
            self.assertIn(
                f"user notes above managed block ({host})", text,
                f"{host}: user content above managed block was lost",
            )
            self.assertIn(
                f"user notes below managed block ({host})", text,
                f"{host}: user content below managed block was lost",
            )

    def test_install_sh_reconcile_intentionally_absent_host_stays_absent(self):
        """OpenClaw on this machine is intentionally absent. The reconcile
        pass must NOT auto-install its managed block — that would
        violate the explicit-installation requirement.
        """
        # Make sure the openclaw target is absent before we run.
        openclaw_target = self.home / ".openclaw" / "workspace" / "SOUL.md"
        self.assertFalse(
            openclaw_target.exists(),
            "test setup invariant: openclaw target should not exist before reconcile",
        )

        result = _run(
            ["bash", str(INSTALL_SH), "--reconcile"],
            env=self.env, timeout=180,
        )
        self.assertEqual(result.returncode, 0, result.stderr)

        self.assertFalse(
            openclaw_target.exists(),
            "install.sh --reconcile must NOT auto-install the openclaw managed block",
        )

    # ---------- step 2: second invocation is a no-op ----------

    def test_reconcile_second_invocation_is_noop(self):
        """After reconcile converges the installed blocks, a second
        invocation must be a no-op: no new backups, no content drift.
        The body-byte comparison in each per-host installer ensures
        this.
        """
        # First invocation.
        r1 = _run(
            ["bash", str(INSTALL_SH), "--reconcile"],
            env=self.env, timeout=180,
        )
        self.assertEqual(r1.returncode, 0, r1.stderr)

        # Snapshot each target's mtime + bytes + the directory listing
        # for backups.
        snapshots = {}
        for host, rel in HOST_TARGETS.items():
            target = self.home / rel
            backups = sorted(
                str(p.name) for p in target.parent.glob(target.name + ".bak*")
            )
            snapshots[host] = {
                "bytes": target.read_bytes(),
                "mtime": target.stat().st_mtime_ns,
                "backups": backups,
            }

        # Second invocation.
        r2 = _run(
            ["bash", str(INSTALL_SH), "--reconcile"],
            env=self.env, timeout=180,
        )
        self.assertEqual(r2.returncode, 0, r2.stderr)

        # Each target should be byte-identical to its first-run state,
        # mtime preserved (no rewrite), no new backup created.
        for host, rel in HOST_TARGETS.items():
            target = self.home / rel
            self.assertEqual(
                target.read_bytes(), snapshots[host]["bytes"],
                f"{host}: second reconcile invocation rewrote the target bytes",
            )
            self.assertEqual(
                target.stat().st_mtime_ns, snapshots[host]["mtime"],
                f"{host}: second reconcile invocation touched the file mtime",
            )
            current_backups = sorted(
                str(p.name) for p in target.parent.glob(target.name + ".bak*")
            )
            self.assertEqual(
                current_backups, snapshots[host]["backups"],
                f"{host}: second reconcile invocation created new backups "
                f"{current_backups} (was {snapshots[host]['backups']})",
            )

    # ---------- step 3: diagnostic sees PASS after reconcile ----------

    def test_check_installed_reports_pass_after_reconcile(self):
        """After the update path reconciles, the diagnostic must
        report PASS for the reconciled hosts and ABSENT (without
        rewriting) for intentionally-absent hosts.
        """
        result = _run(
            ["bash", str(INSTALL_SH), "--reconcile"],
            env=self.env, timeout=180,
        )
        self.assertEqual(result.returncode, 0, result.stderr)

        proc = _run(
            ["python3", str(CHECK_INSTALLED), "--json"],
            env=self.env, timeout=30,
        )
        import json as _json
        payload = _json.loads(proc.stdout)
        by_host = {row["host"]: row for row in payload}

        for host in ("pi", "claude_code", "opencode"):
            self.assertEqual(
                by_host[host]["verdict"], "PASS",
                f"{host}: diagnostic verdict after reconcile is "
                f"{by_host[host]['verdict']!r}, expected PASS",
            )
            self.assertEqual(by_host[host]["file"], "present")
            self.assertEqual(by_host[host]["block"], "present")

        self.assertEqual(
            by_host["openclaw"]["verdict"], "ABSENT",
            "openclaw verdict should remain ABSENT (intentional)",
        )
        self.assertEqual(by_host["openclaw"]["file"], "absent")
        self.assertEqual(by_host["openclaw"]["block"], "absent")


if __name__ == "__main__":
    unittest.main(verbosity=2)
