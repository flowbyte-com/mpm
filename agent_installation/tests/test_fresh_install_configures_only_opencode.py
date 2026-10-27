"""
test_fresh_install_configures_only_opencode.py — a fresh install configures
exactly one host.

THE PROPERTY

On a machine that has never used MPM, the installation must configure
OpenCode and NOTHING ELSE. Concretely, after a fresh install the only
thing MPM has written under $HOME is the OpenCode managed block:

    $HOME/.config/opencode/AGENTS.md

and there is no `.claude/`, no `.pi/`, no `.openclaw/`, no `.hermes*`.

This is the property that makes the 2026-10-27 OpenCode-only adapter
removal SAFE rather than merely tidy. Retiring an adapter must not turn
into "MPM now configures a different set of hosts", and the way to know
that is to assert the resulting HOME, not to assert that the retired
directories are gone from the REPOSITORY.

THE OTHER HALF

The mirror property is that an installation the operator ALREADY has must
survive untouched. That is covered by
`test_refresh_installed_absent_adapter_safety.py` (the retargeted
refresh-safety suite) and by `test_legacy_installation_preserved.py`
(PR #133). This file covers the fresh-machine half only.

NON-VACUITY

Every assertion here is of the form "X did NOT happen". That is only
meaningful if the run DID do something. The control is the OpenCode
managed block itself: it must exist, and it must byte-match the current
canonical render — not merely be present, but be the real, current
contract. A reconciler that had stopped writing anything would fail that
assertion, and with it this whole file.

HERMETICITY

Every scenario runs against a disposable HOME in a temp directory. The
operator's real ~/.config, ~/.claude, ~/.pi, ~/.openclaw and ~/.hermes.md
are never read as inputs and never written.
"""

from __future__ import annotations

import importlib.util
import os
import shutil
import subprocess
import sys
import tempfile
import unittest
from pathlib import Path


REPO_ROOT = Path(__file__).resolve().parent.parent.parent
AGENT_INSTALLATION = REPO_ROOT / "agent_installation"
RECONCILE = AGENT_INSTALLATION / "scripts" / "reconcile_managed_blocks.py"
RENDER_SH = AGENT_INSTALLATION / "scripts" / "render_managed_blocks.py"
CANONICAL_SOURCE = AGENT_INSTALLATION / "MPM_AGENT_INTEGRATION_SNIPPETS.md"
INSTALL_SH = REPO_ROOT / "install.sh"

if str(AGENT_INSTALLATION / "scripts") not in sys.path:
    sys.path.insert(0, str(AGENT_INSTALLATION / "scripts"))

import block_provenance  # noqa: E402

# The one host that IS shipped, and where its managed block lives.
OPENCODE_ADAPTER = "mpm-opencode"
OPENCODE_TARGET = Path(".config") / "opencode" / "AGENTS.md"

# The install targets of the hosts that were retired on 2026-10-27.
# None of them may be created by a fresh install.
RETIRED_HOST_TARGETS = (
    Path(".claude") / "CLAUDE.md",
    Path(".pi") / "agent" / "AGENTS.md",
    Path(".openclaw") / "workspace" / "SOUL.md",
    Path(".hermes.md"),
)

# The directories a retired host would have needed. Their ABSENCE is a
# stronger statement than the absence of the target file: an installer
# that created the directory and then failed would have left the
# directory behind.
RETIRED_HOST_DIRS = (
    Path(".claude"),
    Path(".pi"),
    Path(".openclaw"),
)

OPENCODE_BEGIN = "<!-- BEGIN MPM-MANAGED SECTION:opencode-instructions -->"
OPENCODE_END = "<!-- END MPM-MANAGED SECTION:opencode-instructions -->"


def _canonical_block_for(adapter_name: str) -> str:
    """Render the canonical managed block for the given adapter.

    Uses the same renderer the diagnostic script uses, so the test
    asserts against the actual production byte stream.
    """
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


def _read_managed_section(path: Path) -> str | None:
    """Extract the contents between the managed-section markers.

    Returns the substring starting at the BEGIN marker line and ending
    at (and including) the trailing newline of the END marker line, so
    it byte-matches the canonical renderer output shape.

    The build-provenance comment is stripped: it names the product
    version, the source commit and the install timestamp — per-install
    metadata, not the behavioural contract. The diagnostic compares the
    same way (check_installed_managed_blocks.py), so the two gates
    cannot disagree about what "the same block" means.
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
    end_line_end = text.find("\n", e)
    if end_line_end == -1:
        end_line_end = len(text)
    else:
        end_line_end += 1
    return block_provenance.strip_provenance(text[b:end_line_end])


class _FreshHome(unittest.TestCase):
    """A disposable HOME, plus the assertions that describe what a fresh
    install may and may not have written into it."""

    def setUp(self):
        self.tmpdir = Path(tempfile.mkdtemp(prefix="mpm-fresh-install-"))
        self.addCleanup(shutil.rmtree, self.tmpdir, ignore_errors=True)
        self.home = self.tmpdir / "home"
        self.home.mkdir(parents=True, exist_ok=True)

    def env(self) -> dict:
        env = os.environ.copy()
        env["HOME"] = str(self.home)
        # Keep the workspace inside the sandbox too: the reconciler and
        # the installers must not reach the operator's runtime root.
        env["MPM_WORKSPACE"] = str(self.tmpdir / "workspace")
        (self.tmpdir / "workspace").mkdir(parents=True, exist_ok=True)
        return env

    # ---------- the assertions ----------

    def assert_only_opencode_is_configured(self) -> None:
        # THE CONTROL: the OpenCode managed block exists and is CURRENT.
        # Without this, every "nothing else was configured" assertion
        # below would be satisfied by a reconciler that did nothing.
        target = self.home / OPENCODE_TARGET
        self.assertTrue(
            target.is_file(),
            f"the fresh install did not write the OpenCode managed block at "
            f"{target}; the 'nothing else was configured' assertions below "
            f"would be vacuous",
        )
        section = _read_managed_section(target)
        self.assertIsNotNone(
            section,
            f"{target} carries no managed section; the markers are absent",
        )
        self.assertIn(OPENCODE_BEGIN, section)
        self.assertIn(OPENCODE_END, section)
        self.assertEqual(
            section,
            _canonical_block_for(OPENCODE_ADAPTER),
            f"{target} does not byte-match the current canonical render; "
            f"a fresh install must write the CURRENT contract, not a "
            f"stale one",
        )

        # No retired host was configured.
        for rel in RETIRED_HOST_TARGETS:
            self.assertFalse(
                (self.home / rel).exists(),
                f"the fresh install created {rel}; a retired host must not "
                f"be configured",
            )
        for rel in RETIRED_HOST_DIRS:
            self.assertFalse(
                (self.home / rel).exists(),
                f"the fresh install created {rel}/; a retired host's "
                f"directory must not be created",
            )

        # And the strongest statement of all: exactly one host's worth of
        # state exists under HOME.
        top_level = sorted(p.name for p in self.home.iterdir())
        self.assertEqual(
            top_level,
            [".config"],
            f"a fresh install must leave exactly one top-level entry under "
            f"HOME (the OpenCode config directory); found {top_level}",
        )


class ReconcileOnAFreshHome(_FreshHome):
    """The generic reconciler is the entry point every update path ends
    at, so it is where the property is checked first."""

    def test_reconcile_configures_only_opencode(self):
        proc = subprocess.run(
            [sys.executable, str(RECONCILE), "--home", str(self.home)],
            capture_output=True, text=True, env=self.env(), timeout=120,
        )
        self.assertEqual(
            proc.returncode, 0,
            f"reconcile failed:\nstdout={proc.stdout}\nstderr={proc.stderr}",
        )
        self.assert_only_opencode_is_configured()

    def test_reconcile_is_idempotent(self):
        """A second reconcile changes nothing: the fresh install leaves
        the host in a state the next update leaves alone."""
        first = subprocess.run(
            [sys.executable, str(RECONCILE), "--home", str(self.home)],
            capture_output=True, text=True, env=self.env(), timeout=120,
        )
        self.assertEqual(first.returncode, 0, first.stderr)
        before = (self.home / OPENCODE_TARGET).read_bytes()

        second = subprocess.run(
            [sys.executable, str(RECONCILE), "--home", str(self.home)],
            capture_output=True, text=True, env=self.env(), timeout=120,
        )
        self.assertEqual(second.returncode, 0, second.stderr)
        self.assertEqual(
            (self.home / OPENCODE_TARGET).read_bytes(), before,
            "a second reconcile must be a byte-stable no-op",
        )


class InstallShellOnAFreshHome(_FreshHome):
    """`./install.sh --reconcile` is the documented content-only update
    entry point. It is a different code path from the reconciler script
    — it decides WHEN to reconcile — so the property is checked there
    too."""

    def test_install_sh_reconcile_configures_only_opencode(self):
        proc = subprocess.run(
            ["bash", str(INSTALL_SH), "--reconcile"],
            capture_output=True, text=True, env=self.env(), timeout=300,
        )
        self.assertEqual(
            proc.returncode, 0,
            f"install.sh --reconcile failed:\nstdout={proc.stdout[-4000:]}\n"
            f"stderr={proc.stderr[-4000:]}",
        )
        self.assert_only_opencode_is_configured()


if __name__ == "__main__":
    unittest.main(verbosity=2)
