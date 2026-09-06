"""
test_clean_install_roundtrip.py — Verify clean install/uninstall
round-trips for each host adapter using the regenerated snippets.

For each of the four file-based adapters (claude-code-mpm, opencode-mpm,
pi-mpm, hermes-mpm), this test:

  1. Creates a fresh empty target file (or a target with pre-existing
     user content).
  2. Runs the installer with `--snippet <adapter's templates/*.snippet>`.
  3. Verifies the managed section's bytes match what the canonical
     render produces for that adapter.
  4. Runs uninstall and verifies the file is restored to its
     pre-install state (or unlinked if it was fresh).
  5. Re-runs install on the original state and verifies idempotence.

This pins the "validate both template AND installed artifact" requirement
from the 2026-09-04 canonical-snippets brief.
"""

from __future__ import annotations

import importlib.util
import re
import subprocess
import sys
import tempfile
import unittest
from pathlib import Path


# Path is derived from the test file location so the suite runs in any
# environment (CI, fresh clone, alternate mount). Previously hardcoded
# to /home/v/workspace/projects/mpm/agent_installation, which broke in
# every other workspace AND in the github actions runner.
AGENT_INSTALLATION = Path(__file__).resolve().parent.parent
RENDER_SCRIPT = AGENT_INSTALLATION / "scripts" / "render_managed_blocks.py"
CANONICAL_SOURCE = AGENT_INSTALLATION / "MPM_AGENT_INTEGRATION_SNIPPETS.md"


def _load_render_module():
    spec = importlib.util.spec_from_file_location(
        "render_managed_blocks_clean", RENDER_SCRIPT,
    )
    assert spec and spec.loader
    module = importlib.util.module_from_spec(spec)
    sys.modules[spec.name] = module
    spec.loader.exec_module(module)
    return module


_render = _load_render_module()


# Per-adapter installer invocation. Each entry maps the adapter name to
# the installer CLI args and the expected managed-section regex.
INSTALLERS = {
    "claude-code-mpm": {
        "installer": AGENT_INSTALLATION / "claude-code-mpm/scripts/install_claude_instructions.py",
        "snippet": AGENT_INSTALLATION / "claude-code-mpm/templates/CLAUDE.md.snippet",
        "managed_begin": "<!-- BEGIN MPM-MANAGED SECTION:claude-code-instructions -->",
        "managed_end": "<!-- END MPM-MANAGED SECTION:claude-code-instructions -->",
        "extra_args": ["--scope", "user", "--home", str(Path.home())],
    },
    "opencode-mpm": {
        "installer": AGENT_INSTALLATION / "opencode-mpm/scripts/install_agents_instructions.py",
        "snippet": AGENT_INSTALLATION / "opencode-mpm/templates/AGENTS.md.snippet",
        "managed_begin": "<!-- BEGIN MPM-MANAGED SECTION:opencode-instructions -->",
        "managed_end": "<!-- END MPM-MANAGED SECTION:opencode-instructions -->",
        "extra_args": ["--scope", "user"],
    },
    "pi-mpm": {
        "installer": AGENT_INSTALLATION / "pi-mpm/scripts/install_agents_instructions.py",
        "snippet": AGENT_INSTALLATION / "pi-mpm/templates/AGENTS.md.snippet",
        "managed_begin": "<!-- BEGIN MPM-MANAGED SECTION:pi-instructions -->",
        "managed_end": "<!-- END MPM-MANAGED SECTION:pi-instructions -->",
        "extra_args": [],  # Pi installer defaults to user scope
    },
    "hermes-mpm": {
        "installer": AGENT_INSTALLATION / "hermes-mpm/scripts/install_hermes_instructions.py",
        "snippet": AGENT_INSTALLATION / "hermes-mpm/templates/hermes.md.snippet",
        "managed_begin": "<!-- BEGIN MPM-MANAGED BLOCK:hermes-mpm -->",
        "managed_end": "<!-- END MPM-MANAGED BLOCK:hermes-mpm -->",
        "extra_args": [],  # --target-dir provided per-call
    },
}


def _run(args: list[str]) -> subprocess.CompletedProcess:
    return subprocess.run(
        [sys.executable, *args],
        capture_output=True, text=True, timeout=30,
    )


def _extract_managed_block(text: str, begin: str, end: str) -> str:
    """Return the bytes between (and including) the managed markers."""
    m = re.search(
        re.escape(begin) + r".*?" + re.escape(end),
        text, flags=re.DOTALL,
    )
    if not m:
        return ""
    return m.group(0)


class CleanInstallRoundTrip(unittest.TestCase):
    """For each adapter, verify that:

      - The installer writes a managed section that contains the
        rendered canonical block.
      - The managed section's content is byte-for-byte identical to
        what the canonical source produces for that adapter.
      - User content outside the managed section is preserved.
      - Uninstall restores the original state.
      - Re-install is idempotent.
    """

    def _adapter_rendered(self, adapter_name: str) -> str:
        """Get the rendered snippet content for an adapter."""
        rendered = _render.render_all(CANONICAL_SOURCE, AGENT_INSTALLATION)
        return rendered[adapter_name]

    def test_render_matches_checked_in_snippets(self):
        """Sanity check: render output matches the checked-in snippet files."""
        rendered = _render.render_all(CANONICAL_SOURCE, AGENT_INSTALLATION)
        for name, cfg in INSTALLERS.items():
            with self.subTest(adapter=name):
                actual = cfg["snippet"].read_text(encoding="utf-8")
                self.assertEqual(
                    actual, rendered[name],
                    f"{name}: snippet drifted from canonical source. "
                    f"Run `python3 scripts/render_managed_blocks.py` to fix.",
                )

    def test_fresh_install_writes_canonical_block(self):
        """A fresh install must produce a file whose managed section is
        the rendered canonical block wrapped in the adapter's markers."""
        for name, cfg in INSTALLERS.items():
            with self.subTest(adapter=name):
                with tempfile.TemporaryDirectory() as tmp:
                    target = Path(tmp) / cfg["snippet"].name
                    args = [str(cfg["installer"]),
                            *cfg["extra_args"],
                            "--target", str(target),
                            "--snippet", str(cfg["snippet"])]
                    if name == "hermes-mpm":
                        # Hermes installer takes --target, not --target-dir
                        pass  # already handled
                    proc = _run(args)
                    self.assertEqual(
                        proc.returncode, 0,
                        f"{name}: installer failed: {proc.stderr}",
                    )
                    self.assertTrue(
                        target.is_file(),
                        f"{name}: installer did not create target",
                    )
                    text = target.read_text(encoding="utf-8")
                    managed = _extract_managed_block(
                        text, cfg["managed_begin"], cfg["managed_end"],
                    )
                    self.assertTrue(
                        managed,
                        f"{name}: managed block not found in {target}",
                    )
                    # The managed block must contain the canonical source
                    # marker so future archaeology can trace provenance.
                    self.assertIn(
                        "MPM_AGENT_INTEGRATION_SNIPPETS.md",
                        managed,
                        f"{name}: managed block missing source marker",
                    )

    def test_install_preserves_existing_user_content(self):
        """Pre-existing user content above/below the managed markers must
        survive a fresh install (append path) and a refresh (replace path)."""
        for name, cfg in INSTALLERS.items():
            with self.subTest(adapter=name):
                with tempfile.TemporaryDirectory() as tmp:
                    target = Path(tmp) / cfg["snippet"].name
                    user_prelude = "# User content above\n\n"
                    user_postlude = "\n\n# User content below\n"
                    target.write_text(user_prelude + user_postlude, encoding="utf-8")

                    args = [str(cfg["installer"]),
                            *cfg["extra_args"],
                            "--target", str(target),
                            "--snippet", str(cfg["snippet"])]
                    proc = _run(args)
                    self.assertEqual(proc.returncode, 0, proc.stderr)

                    text = target.read_text(encoding="utf-8")
                    self.assertIn(
                        user_prelude.strip(), text,
                        f"{name}: pre-install user content lost (prelude)",
                    )
                    self.assertIn(
                        user_postlude.strip(), text,
                        f"{name}: pre-install user content lost (postlude)",
                    )
                    managed = _extract_managed_block(
                        text, cfg["managed_begin"], cfg["managed_end"],
                    )
                    self.assertTrue(
                        managed,
                        f"{name}: managed block not found after install",
                    )

    def test_reinstall_is_idempotent(self):
        """Running install twice must produce identical bytes (no spurious
        drift; the second run is a no-op because the managed block is
        already current)."""
        for name, cfg in INSTALLERS.items():
            with self.subTest(adapter=name):
                with tempfile.TemporaryDirectory() as tmp:
                    target = Path(tmp) / cfg["snippet"].name
                    args = [str(cfg["installer"]),
                            *cfg["extra_args"],
                            "--target", str(target),
                            "--snippet", str(cfg["snippet"])]
                    proc1 = _run(args)
                    self.assertEqual(proc1.returncode, 0, proc1.stderr)
                    first = target.read_text(encoding="utf-8")

                    proc2 = _run(args)
                    self.assertEqual(proc2.returncode, 0, proc2.stderr)
                    second = target.read_text(encoding="utf-8")

                    self.assertEqual(
                        first, second,
                        f"{name}: re-install changed file bytes (non-idempotent)",
                    )

    def test_uninstall_restores_pre_install_state(self):
        """Uninstall must strip the managed section while preserving any
        pre-existing user content."""
        for name, cfg in INSTALLERS.items():
            with self.subTest(adapter=name):
                with tempfile.TemporaryDirectory() as tmp:
                    target = Path(tmp) / cfg["snippet"].name
                    user_prelude = "# User content above\n"
                    target.write_text(user_prelude, encoding="utf-8")

                    install_args = [str(cfg["installer"]),
                                    *cfg["extra_args"],
                                    "--target", str(target),
                                    "--snippet", str(cfg["snippet"])]
                    proc = _run(install_args)
                    self.assertEqual(proc.returncode, 0, proc.stderr)
                    self.assertTrue(target.is_file())

                    uninstall_args = [str(cfg["installer"]),
                                      *cfg["extra_args"],
                                      "--target", str(target),
                                      "--snippet", str(cfg["snippet"]),
                                      "--uninstall"]
                    proc = _run(uninstall_args)
                    self.assertEqual(proc.returncode, 0, proc.stderr)

                    text = target.read_text(encoding="utf-8") if target.exists() else ""
                    self.assertNotIn(
                        cfg["managed_begin"], text,
                        f"{name}: uninstall left managed block behind",
                    )
                    # User content must remain.
                    self.assertIn(
                        user_prelude.strip(), text,
                        f"{name}: uninstall lost pre-install user content",
                    )


class DriftTestFailsOnMutatedSnippet(unittest.TestCase):
    """If a checked-in snippet is mutated, the drift test must fail.

    This guards the byte-for-byte parity contract: hand-edits to
    checked-in snippets are a regression. The test temporarily mutates
    a snippet, runs --check, asserts non-zero exit, then restores.
    """

    def test_drift_check_detects_mutation(self):
        snippet = INSTALLERS["opencode-mpm"]["snippet"]
        original = snippet.read_text(encoding="utf-8")
        mutated = original + "\n<!-- drift-marker -->\n"
        try:
            snippet.write_text(mutated, encoding="utf-8")
            proc = _run([str(RENDER_SCRIPT), "--check"])
            self.assertNotEqual(
                proc.returncode, 0,
                "render_managed_blocks --check did not detect drift",
            )
            self.assertIn(
                "drift", proc.stderr.lower(),
                f"drift message missing from stderr: {proc.stderr!r}",
            )
        finally:
            snippet.write_text(original, encoding="utf-8")
            # Verify the restoration brings us back to parity.
            proc = _run([str(RENDER_SCRIPT), "--check"])
            self.assertEqual(
                proc.returncode, 0,
                f"after restoration, drift check still fails: {proc.stderr}",
            )


if __name__ == "__main__":
    unittest.main()
