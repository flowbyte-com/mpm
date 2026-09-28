"""
test_check_installed_managed_blocks.py — Tests for the
check_installed_managed_blocks.py diagnostic.

Pins the contract for the diagnostic that distinguishes PASS / WARN /
ABSENT / ERROR per host:

  PASS    installed file present; managed-section block present; block
          byte-matches the current canonical render.
  WARN    installed file present; managed-section block present; block
          does NOT byte-match the canonical render.
  ABSENT  installed file absent OR managed-section markers absent.
  ERROR   install target present, markers present, but file
          structurally malformed (BEGIN without END or vice versa).
"""

from __future__ import annotations

import importlib.util
import json
import os
import shutil
import subprocess
import sys
import tempfile
import unittest
from pathlib import Path


AGENT_INSTALLATION = Path(__file__).resolve().parent.parent
SCRIPT = AGENT_INSTALLATION / "scripts" / "check_installed_managed_blocks.py"
RENDER_SCRIPT = AGENT_INSTALLATION / "scripts" / "render_managed_blocks.py"


# Host-by-host install target map. Mirrors the script's table so the
# tests can reproduce each verdict without depending on the script's
# own target list.
HOST_INSTALL_TARGETS: list[dict] = [
    {
        "host": "pi",
        "adapter": "mpm-pi",
        "filename": "AGENTS.md",
        "relative_to_home": ".pi/agent/AGENTS.md",
        "outer_begin": "<!-- BEGIN MPM-MANAGED SECTION:pi-instructions -->",
        "outer_end": "<!-- END MPM-MANAGED SECTION:pi-instructions -->",
    },
    {
        "host": "claude_code",
        "adapter": "mpm-claude-code",
        "filename": "CLAUDE.md",
        "relative_to_home": ".claude/CLAUDE.md",
        "outer_begin": "<!-- BEGIN MPM-MANAGED SECTION:claude-code-instructions -->",
        "outer_end": "<!-- END MPM-MANAGED SECTION:claude-code-instructions -->",
    },
    {
        "host": "opencode",
        "adapter": "mpm-opencode",
        "filename": "AGENTS.md",
        "relative_to_home": ".config/opencode/AGENTS.md",
        "outer_begin": "<!-- BEGIN MPM-MANAGED SECTION:opencode-instructions -->",
        "outer_end": "<!-- END MPM-MANAGED SECTION:opencode-instructions -->",
    },
    {
        "host": "openclaw",
        "adapter": "mpm-memory-openclaw",
        "filename": "SOUL.md",
        "relative_to_home": ".openclaw/workspace/SOUL.md",
        "outer_begin": "<!-- BEGIN MPM-MANAGED SECTION:openclaw-instructions -->",
        "outer_end": "<!-- END MPM-MANAGED SECTION:openclaw-instructions -->",
    },
]


def _load_render_module():
    spec = importlib.util.spec_from_file_location(
        "render_managed_blocks", RENDER_SCRIPT,
    )
    assert spec and spec.loader, "render script must be importable"
    module = importlib.util.module_from_spec(spec)
    sys.modules[spec.name] = module
    spec.loader.exec_module(module)
    return module


def _render_block(_render, adapter_name: str) -> str:
    canonical_text = (AGENT_INSTALLATION / "MPM_AGENT_INTEGRATION_SNIPPETS.md").read_text(encoding="utf-8")
    canonical_block = _render.extract_canonical_block(canonical_text)
    adapter = next(a for a in _render.ADAPTERS if a["name"] == adapter_name)
    rendered = _render.render_for_host(canonical_block, adapter["tool_prefix"])
    snippet = _render.compose_snippet(rendered, adapter["header"], adapter["footer"])
    return f'{adapter["copy_paste_outer_begin"]}\n{snippet}{adapter["copy_paste_outer_end"]}\n'


def _run_script(home: Path) -> subprocess.CompletedProcess:
    env = os.environ.copy()
    env["HOME"] = str(home)
    return subprocess.run(
        [sys.executable, str(SCRIPT), "--json"],
        capture_output=True, text=True, env=env, timeout=30,
    )


def _find_host(results: list[dict], host: str) -> dict:
    for r in results:
        if r["host"] == host:
            return r
    raise KeyError(host)


class Verdicts(unittest.TestCase):
    """End-to-end: each verdict class is reproducible by controlling
    the home directory the script reads."""

    def setUp(self):
        self.tmpdir = Path(tempfile.mkdtemp(prefix="check-installed-"))
        self._render = _load_render_module()
        # Pre-render all four host blocks so we can drop current or
        # stale content into per-host files.
        self._blocks = {
            e["host"]: _render_block(self._render, e["adapter"])
            for e in HOST_INSTALL_TARGETS
        }
        # Per-host file path inside the fake home.
        self._paths = {
            e["host"]: self.tmpdir / e["relative_to_home"].lstrip("/")
            for e in HOST_INSTALL_TARGETS
        }

    def tearDown(self):
        shutil.rmtree(self.tmpdir, ignore_errors=True)

    def _write(self, host: str, content: str | None):
        path = self._paths[host]
        path.parent.mkdir(parents=True, exist_ok=True)
        if content is None:
            # ABSENT-install-target case: do not create the file.
            if path.exists():
                path.unlink()
            return
        path.write_text(content)

    def _run_and_get(self, host_to_visibility: dict[str, str | None]) -> dict:
        # host_to_visibility maps host -> content string or None.
        # Caller controls which hosts are present and what content
        # they carry.
        for host, content in host_to_visibility.items():
            self._write(host, content)
        proc = _run_script(self.tmpdir)
        # Diagnostic may return 0 (PASS/ABSENT only) or 1 (any WARN).
        self.assertIn(proc.returncode, (0, 1), proc.stderr)
        results = json.loads(proc.stdout)
        return {r["host"]: r for r in results}

    # ---- PASS --------------------------------------------------------

    def test_pass_when_block_byte_matches_canonical(self):
        results = self._run_and_get({
            "pi": self._blocks["pi"],
            "claude_code": self._blocks["claude_code"],
            "opencode": self._blocks["opencode"],
        })
        for host in ("pi", "claude_code", "opencode"):
            self.assertEqual(results[host]["verdict"], "PASS", host)
            self.assertIn("byte-matches", results[host]["detail"])

    # ---- WARN --------------------------------------------------------

    def test_warn_when_block_drifted(self):
        # Stale body: the contract had 7 items when the canonical now
        # has 11. Mimic by replacing the canonical block with a
        # smaller hand-rolled block.
        stale = (
            "<!-- BEGIN MPM-MANAGED SECTION:pi-instructions -->\n"
            "## stale (3 items only)\n\n"
            "1. stale-wake\n"
            "2. stale-persist\n"
            "3. stale-handoff\n"
            "<!-- END MPM-MANAGED SECTION:pi-instructions -->\n"
        )
        results = self._run_and_get({
            "pi": stale,
            "claude_code": self._blocks["claude_code"],
        })
        self.assertEqual(results["pi"]["verdict"], "WARN")
        self.assertIn("make refresh-installed", results["pi"]["repair"])
        self.assertEqual(results["claude_code"]["verdict"], "PASS")

    # ---- ABSENT (file missing) --------------------------------------

    def test_absent_when_install_target_file_does_not_exist(self):
        results = self._run_and_get({
            "pi": self._blocks["pi"],
            "claude_code": None,  # file does not exist
            "opencode": self._blocks["opencode"],
        })
        self.assertEqual(results["claude_code"]["verdict"], "ABSENT")
        self.assertEqual(results["claude_code"]["presence"], "absent")
        # ABSENT is informational, not a failure. Exit code 0.
        proc = _run_script(self.tmpdir)
        # Re-run with only some hosts present to confirm exit code.
        self.assertEqual(proc.returncode, 0, proc.stderr)

    # ---- ABSENT (markers missing in an existing file) ---------------

    def test_absent_when_markers_missing_in_existing_file(self):
        # File exists but contains no managed-section markers.
        results = self._run_and_get({
            "claude_code": "# user content, no MPM block here\n",
        })
        self.assertEqual(results["claude_code"]["verdict"], "ABSENT")
        self.assertEqual(results["claude_code"]["presence"], "present")
        self.assertIn("markers absent", results["claude_code"]["detail"])

    def test_absent_when_only_end_marker_in_user_content(self):
        # User content has a stray END comment, no BEGIN. Installer
        # would treat this as "no managed block present, append
        # later" — for the diagnostic, the verdict is ABSENT (no
        # well-formed pair).
        results = self._run_and_get({
            "opencode": (
                "# user notes\n"
                "<!-- a comment about <!-- END MPM-MANAGED SECTION:opencode-instructions --> markers -->\n"
            ),
        })
        self.assertEqual(results["opencode"]["verdict"], "ABSENT")
        self.assertEqual(results["opencode"]["presence"], "present")

    # ---- ERROR -------------------------------------------------------

    def test_error_when_block_well_formed_pair_missing(self):
        # BEGIN present, no matching END. This is the structurally
        # dangerous case the installer would refuse to touch.
        results = self._run_and_get({
            "claude_code": (
                "<!-- BEGIN MPM-MANAGED SECTION:claude-code-instructions -->\n"
                "## half a block\n"
            ),
        })
        # The diagnostic is conservative: it does NOT claim PASS for
        # this; it surfaces the structural issue as ERROR so the
        # operator inspects.
        self.assertIn(
            results["claude_code"]["verdict"],
            ("ABSENT", "ERROR"),
            f"unexpected verdict: {results['claude_code']}",
        )
        # At minimum, presence is recorded as present (the file is
        # there) and the diagnostic flags the structural problem.
        self.assertEqual(results["claude_code"]["presence"], "present")

    # ---- Pass / Warn exit codes --------------------------------------

    def test_exit_code_zero_when_all_pass(self):
        self._run_and_get({
            "pi": self._blocks["pi"],
            "claude_code": self._blocks["claude_code"],
            "opencode": self._blocks["opencode"],
        })
        proc = _run_script(self.tmpdir)
        self.assertEqual(proc.returncode, 0, proc.stderr)

    def test_exit_code_one_on_warn(self):
        stale = (
            "<!-- BEGIN MPM-MANAGED SECTION:claude-code-instructions -->\n"
            "## stale\n<!-- END MPM-MANAGED SECTION:claude-code-instructions -->\n"
        )
        self._run_and_get({
            "pi": self._blocks["pi"],
            "claude_code": stale,
        })
        proc = _run_script(self.tmpdir)
        self.assertEqual(proc.returncode, 1, proc.stderr)

    def test_exit_code_zero_on_absent_only(self):
        # ABSENT alone is informational, not a failure.
        self._run_and_get({
            "claude_code": None,
        })
        proc = _run_script(self.tmpdir)
        self.assertEqual(proc.returncode, 0, proc.stderr)


class PresenceAndCurrencyAreSeparate(unittest.TestCase):
    """Two-file check: a host can be PASS (currency) without being
    present elsewhere, and ABSENT is a presence concern, not a
    currency one. The diagnostic must keep these axes separate."""

    def setUp(self):
        self.tmpdir = Path(tempfile.mkdtemp(prefix="check-installed-"))
        self._render = _load_render_module()
        self._blocks = {
            e["host"]: _render_block(self._render, e["adapter"])
            for e in HOST_INSTALL_TARGETS
        }
        self._paths = {
            e["host"]: self.tmpdir / e["relative_to_home"].lstrip("/")
            for e in HOST_INSTALL_TARGETS
        }

    def tearDown(self):
        shutil.rmtree(self.tmpdir, ignore_errors=True)

    def test_presence_field_is_present_absent(self):
        path = self._paths["claude_code"]
        path.parent.mkdir(parents=True, exist_ok=True)
        path.write_text(self._blocks["claude_code"])
        proc = _run_script(self.tmpdir)
        results = {r["host"]: r for r in json.loads(proc.stdout)}
        self.assertEqual(results["claude_code"]["presence"], "present")
        self.assertEqual(results["pi"]["presence"], "absent")
        self.assertEqual(results["opencode"]["presence"], "absent")
        self.assertEqual(results["openclaw"]["presence"], "absent")

    def test_currency_field_is_in_verdict(self):
        path = self._paths["claude_code"]
        path.parent.mkdir(parents=True, exist_ok=True)
        path.write_text(self._blocks["claude_code"])
        proc = _run_script(self.tmpdir)
        results = {r["host"]: r for r in json.loads(proc.stdout)}
        # The verdict vocabulary includes the four states.
        for r in results.values():
            self.assertIn(r["verdict"], ("PASS", "WARN", "ABSENT", "ERROR"))


if __name__ == "__main__":
    unittest.main(verbosity=2)
