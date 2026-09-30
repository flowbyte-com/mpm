"""
test_readme_managed_block_parity.py — README managed-block parity.

The root README documents the managed behavioural block so humans and
agents onboarding to MPM can see it without reading the installer tree.
That copy must never become a second, hand-maintained source of truth:
if someone edits the README block directly, it silently diverges from
the canonical source and every host keeps loading the real contract
while the README teaches something else.

These tests pin that the README's managed-block example is GENERATED
from the same canonical source as every adapter snippet:

  1. The README copy is byte-equivalent to the host-neutral canonical
     render.
  2. `render_managed_blocks.py --check` detects README drift.
  3. A write/regeneration updates the README deterministically.
  4. The markers and fence around the README example stay structurally
     valid.
  5. No host-specific tool prefix leaks into the README copy.
  6. Human-authored README content outside the managed section
     survives regeneration byte-for-byte.
"""

from __future__ import annotations

import importlib.util
import shutil
import subprocess
import sys
import tempfile
import unittest
from pathlib import Path


AGENT_INSTALLATION = Path(__file__).resolve().parent.parent
REPO_ROOT = AGENT_INSTALLATION.parent
RENDER_SCRIPT = AGENT_INSTALLATION / "scripts" / "render_managed_blocks.py"
CANONICAL_SOURCE = AGENT_INSTALLATION / "MPM_AGENT_INTEGRATION_SNIPPETS.md"
README = REPO_ROOT / "README.md"


def _load_render_module():
    spec = importlib.util.spec_from_file_location(
        "render_managed_blocks", RENDER_SCRIPT,
    )
    assert spec and spec.loader, "render script must be importable"
    module = importlib.util.module_from_spec(spec)
    sys.modules[spec.name] = module
    spec.loader.exec_module(module)
    return module


_render = _load_render_module()


def _run_renderer(*args: str, repo_root: Path | None = None) -> subprocess.CompletedProcess:
    argv = [sys.executable, str(RENDER_SCRIPT), *args]
    if repo_root is not None:
        argv += ["--repo-root", str(repo_root)]
    return subprocess.run(
        argv, capture_output=True, text=True, timeout=60,
    )


class ReadmeManagedBlockParity(unittest.TestCase):
    """The checked-in README must already be in parity with the
    canonical source."""

    def setUp(self):
        self.tmpdir = Path(tempfile.mkdtemp(prefix="readme-parity-"))

    def tearDown(self):
        shutil.rmtree(self.tmpdir, ignore_errors=True)

    # ---- 1. byte-equivalence with the host-neutral canonical render ----

    def test_readme_block_is_byte_equivalent_to_canonical_render(self):
        expected = _render.render_readme_block(CANONICAL_SOURCE)
        actual = _render._split_readme(README.read_text(encoding="utf-8"))
        self.assertIsNotNone(
            actual, "README.md is missing the managed-block markers",
        )
        _, owned, _ = actual
        self.assertEqual(
            owned, expected,
            "README's managed-block example has drifted from the canonical "
            "render; run `python3 agent_installation/scripts/"
            "render_managed_blocks.py` to regenerate",
        )

    def test_readme_block_content_is_the_host_neutral_canonical_block(self):
        """The README copy is the bare canonical block: the same
        `<!-- BEGIN MPM MANAGED BLOCK -->` payload every host derives
        from, with no host header or footer."""
        canonical_block = _render.extract_canonical_block(
            CANONICAL_SOURCE.read_text(encoding="utf-8"),
        )
        _, owned, _ = _render._split_readme(
            README.read_text(encoding="utf-8"),
        )
        self.assertIn(
            canonical_block.rstrip("\n"), owned,
            "the README must embed the bare canonical block verbatim",
        )

    # ---- 2. --check detects drift ----

    def test_check_passes_on_the_checked_in_readme(self):
        proc = _run_renderer("--check")
        self.assertEqual(
            proc.returncode, 0,
            f"render --check failed on the checked-in tree; "
            f"stdout={proc.stdout}\nstderr={proc.stderr}",
        )

    def test_check_detects_readme_drift(self):
        """Editing the README block by hand must fail --check."""
        sandbox = self.tmpdir / "repo"
        sandbox.mkdir(parents=True, exist_ok=True)
        readme_copy = sandbox / "README.md"
        text = README.read_text(encoding="utf-8")
        before, owned, after = _render._split_readme(text)
        # Hand-edit the managed block the way a well-meaning contributor
        # would: replace the canonical heading.
        tampered = owned.replace(
            "## MPM behavioural contract",
            "## MPM behaviour (hand-edited, will drift)",
        )
        self.assertNotEqual(
            tampered, owned, "tamper fixture did not modify the block",
        )
        readme_copy.write_text(before + tampered + after, encoding="utf-8")

        proc = _run_renderer("--check", repo_root=sandbox)
        self.assertEqual(
            proc.returncode, 1,
            "render --check must fail when the README block has drifted",
        )
        self.assertIn(
            "README DRIFT", proc.stderr,
            f"the drift report should name the README; stderr={proc.stderr}",
        )

    def test_check_detects_missing_markers(self):
        """A README with no managed markers is drift, not a crash."""
        sandbox = self.tmpdir / "repo_nomarkers"
        sandbox.mkdir(parents=True, exist_ok=True)
        (sandbox / "README.md").write_text(
            "# MPM\n\nno managed section here\n", encoding="utf-8",
        )
        proc = _run_renderer("--check", repo_root=sandbox)
        self.assertEqual(proc.returncode, 1, proc.stdout)
        self.assertIn(
            "managed-block markers", proc.stderr,
            f"the drift report should explain the missing markers; "
            f"stderr={proc.stderr}",
        )

    # ---- 3. regeneration is deterministic ----

    def test_write_is_deterministic_and_idempotent(self):
        """Two consecutive regenerations produce identical bytes, and
        the second is a no-op."""
        sandbox = self.tmpdir / "repo_write"
        sandbox.mkdir(parents=True, exist_ok=True)
        target = sandbox / "README.md"
        target.write_text(README.read_text(encoding="utf-8"), encoding="utf-8")

        first = _run_renderer("--only", "mpm-opencode", repo_root=sandbox)
        self.assertEqual(first.returncode, 0, first.stderr)
        after_first = target.read_bytes()

        second = _run_renderer("--only", "mpm-opencode", repo_root=sandbox)
        self.assertEqual(second.returncode, 0, second.stderr)
        after_second = target.read_bytes()

        self.assertEqual(
            after_first, after_second,
            "regenerating the README block twice must be byte-identical",
        )

    def test_write_restores_a_tampered_block(self):
        sandbox = self.tmpdir / "repo_restore"
        sandbox.mkdir(parents=True, exist_ok=True)
        target = sandbox / "README.md"
        text = README.read_text(encoding="utf-8")
        before, owned, after = _render._split_readme(text)
        tampered = owned.replace("MPM behavioural contract", "MPM wrong heading")
        target.write_text(before + tampered + after, encoding="utf-8")

        proc = _run_renderer("--only", "mpm-opencode", repo_root=sandbox)
        self.assertEqual(proc.returncode, 0, proc.stderr)
        self.assertEqual(
            target.read_text(encoding="utf-8"),
            text,
            "regeneration must restore the README to the canonical block",
        )

    # ---- 4. markers / fences stay structurally valid ----

    def test_markers_and_fence_are_structurally_valid(self):
        text = README.read_text(encoding="utf-8")
        begin = _render.README_BEGIN_MARKER
        end = _render.README_END_MARKER
        self.assertEqual(
            text.count(begin), 1,
            "exactly one README managed-block BEGIN marker must be present",
        )
        self.assertEqual(
            text.count(end), 1,
            "exactly one README managed-block END marker must be present",
        )
        self.assertLess(
            text.find(begin), text.find(end),
            "the BEGIN marker must precede the END marker",
        )
        owned = _render._split_readme(text)[1]
        # The example is a fenced ```markdown block, and the canonical
        # managed block contains no fence of its own, so exactly two
        # fences delimit it.
        self.assertEqual(
            owned.count("```"),
            2,
            "the README example must be delimited by exactly one fence pair",
        )
        lines = owned.splitlines()
        fence_idx = next(i for i, ln in enumerate(lines) if ln.startswith("```"))
        self.assertEqual(
            lines[fence_idx], "```markdown",
            "the opening fence must declare the markdown language",
        )
        # The closing fence is the last fence, and the managed section's
        # own END marker closes the span after it.
        last_fence = max(i for i, ln in enumerate(lines) if ln.startswith("```"))
        self.assertEqual(lines[last_fence], "```", "the closing fence must be bare")
        self.assertTrue(
            owned.rstrip().endswith(_render.README_END_MARKER),
            "the managed END marker must close the README section",
        )

    def test_canonical_block_has_no_inner_fence(self):
        """The outer ``` fence is only unambiguous because the
        canonical block carries no fence of its own."""
        block = _render.extract_canonical_block(
            CANONICAL_SOURCE.read_text(encoding="utf-8"),
        )
        self.assertNotIn(
            "```", block,
            "the canonical managed block must not contain a code fence",
        )

    # ---- 5. no host-specific prefix in the README copy ----

    def test_readme_copy_has_no_host_specific_tool_prefix(self):
        owned = _render._split_readme(
            README.read_text(encoding="utf-8"),
        )[1]
        for prefix in ("mpm__", "mcp__"):
            self.assertNotIn(
                prefix, owned,
                f"the README copy is host-neutral and must not contain the "
                f"{prefix!r} host transport prefix",
            )

    def test_readme_copy_differs_from_every_prefixed_host_render(self):
        """The README copy is the bare render, so it must not equal any
        prefixed host's render."""
        canonical = _render.extract_canonical_block(
            CANONICAL_SOURCE.read_text(encoding="utf-8"),
        )
        owned = _render._split_readme(
            README.read_text(encoding="utf-8"),
        )[1]
        for adapter in _render.ADAPTERS:
            if not adapter["tool_prefix"]:
                continue
            prefixed = _render.render_for_host(
                canonical, adapter["tool_prefix"],
            )
            self.assertNotIn(
                prefixed.rstrip("\n"), owned,
                f"README must not embed the {adapter['name']} render",
            )

    # ---- 6. human-authored content survives regeneration ----

    def test_human_authored_content_survives_regeneration(self):
        """Content outside the managed markers is never rewritten."""
        sentinel_before = "<!-- human-authored: before the managed section -->"
        sentinel_after = "<!-- human-authored: after the managed section -->"
        sandbox = self.tmpdir / "repo_human"
        sandbox.mkdir(parents=True, exist_ok=True)
        target = sandbox / "README.md"

        text = README.read_text(encoding="utf-8")
        before, owned, after = _render._split_readme(text)
        target.write_text(
            f"{sentinel_before}\n{before}{owned}{after}\n{sentinel_after}\n",
            encoding="utf-8",
        )
        # Drift the block so the regeneration has real work to do.
        tampered = owned.replace("MPM behavioural contract", "MPM stale heading")
        target.write_text(
            f"{sentinel_before}\n{before}{tampered}{after}\n{sentinel_after}\n",
            encoding="utf-8",
        )

        proc = _run_renderer("--only", "mpm-opencode", repo_root=sandbox)
        self.assertEqual(proc.returncode, 0, proc.stderr)

        result = target.read_text(encoding="utf-8")
        self.assertIn(
            sentinel_before, result,
            "content above the managed section must survive regeneration",
        )
        self.assertIn(
            sentinel_after, result,
            "content below the managed section must survive regeneration",
        )
        self.assertIn(
            "MPM behavioural contract", result,
            "the block itself must be restored to canonical",
        )
        self.assertNotIn("MPM stale heading", result)

    def test_write_refuses_when_markers_are_missing(self):
        """A README with no markers is a hard error, not a silent
        append — the renderer must never guess where its section goes."""
        sandbox = self.tmpdir / "repo_broken"
        sandbox.mkdir(parents=True, exist_ok=True)
        target = sandbox / "README.md"
        target.write_text("# MPM\n\nno markers\n", encoding="utf-8")
        proc = _run_renderer("--only", "mpm-opencode", repo_root=sandbox)
        self.assertNotEqual(proc.returncode, 0, proc.stdout)
        self.assertEqual(
            target.read_text(encoding="utf-8"),
            "# MPM\n\nno markers\n",
            "a markerless README must be left untouched",
        )


if __name__ == "__main__":
    unittest.main(verbosity=2)
