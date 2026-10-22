"""
test_installed_block_drift.py — Regression test for stale installed
managed blocks across all persistent-file hosts.

Background
----------

The render script `agent_installation/scripts/render_managed_blocks.py`
produces per-host template snippets from the canonical
`MPM_AGENT_INTEGRATION_SNIPPETS.md` source. Its `--check` mode asserts
byte-for-byte parity between:

  1. The rendered per-host snippet (output of the render script)
  2. The copy/paste example embedded in the canonical source

That parity check is a *renderer* invariant — it catches drift between
the canonical source and the checked-in snippets. It does NOT cover
the next hop in the chain: the snippet → installed-persistent-file
step that the per-host `install.sh` (or the `make refresh-installed`
target) performs.

History of the gap
------------------

The 2026-09-27 managed-instruction contract bump (1.0.0 -> 1.2.0)
expanded the canonical block from 7 to 11 numbered invariants. The
renderer was updated and the snippets were regenerated, but the
installed persistent files for three of the five supported hosts
(Pi, OpenCode, Claude Code) were not refreshed, because:

  - `make refresh-installed` is the only path that writes installed
    blocks, and it is invoked manually (or by the per-host
    `install.sh` on a clean install), never automatically.
  - The `mpm_integration_check.md` host-specific subsections (17.2 Pi,
    17.4 Claude, 17.5 OpenCode) verify marker presence only, not
    content parity against the canonical render.
  - `git pull` of the MPM substrate does not trigger a refresh.

The result: every host that had been installed before the contract
bump continued to load the older 7-section managed block. Operators
got no signal that their agents were running on stale behavioural
instructions.

What this test asserts
----------------------

For every persistent-file host (Pi, Claude Code, OpenCode, OpenClaw),
if the canonical install target file exists on the test machine, the
managed section between `<!-- BEGIN MPM-MANAGED SECTION:* -->` and
`<!-- END MPM-MANAGED SECTION:* -->` must byte-match what the
canonical render would produce today.

OpenClaw scope
--------------

OpenClaw's persistent managed block is a REQUIRED part of a functional
OpenClaw MPM integration, exactly as it is for the other hosts. This
test therefore includes OpenClaw whenever its integration is installed
on the machine. A machine with no OpenClaw MPM integration is still
skipped: presence of a host is a separate concern from currency of the
block, and MPM never opts a machine into a host it does not use.

Expected-block construction
---------------------------

Hosts whose installer writes bare outer markers are compared against
`outer markers + rendered snippet + outer markers`. OpenClaw's
installer instead wraps the snippet in a section banner carrying a
`<!-- generated: TS -->` line, so its expected bytes are produced by
asking that installer to build the section. The preserved timestamp is
normalized away on both sides before comparison — it is a per-install
fact, not part of the canonical contract.

Hosts whose install target is absent are *skipped* — this test
intentionally does not assert presence. The question of whether a
host is installed at all is a separate concern from the question of
whether an installed block is current.

Running
-------

Source/hermetic scope (what `make test` runs):

  cd agent_installation
  python3 -m unittest tests.test_installed_block_drift -v

This runs InstalledBlockDriftHermetic, which builds a synthetic install
tree under t.TempDir() and asserts both directions of the detector: a
matching install is accepted, a stale one is reported. It never reads
or writes the operator's real agent instruction files.

Installed-runtime scope (opt-in, read-only):

  make check-installed-drift

This is the only thing that asserts against THIS machine's live
install. It sets MPM_CHECK_INSTALLED_DRIFT=1, which enables
InstalledBlockDrift.test_all_installed_blocks_match_canonical_render.

Why the split: a source gate that fails whenever the installed
instructions predate the source cannot be passed by a correct source
change, and cannot be trusted to mean "the source is broken" when it
does fail. Canonical `make test` and `make test-race` therefore
validate the source tree, and this machine's deployment state is
checked explicitly and separately. The detection logic is not left
untested by this — only its scope is.

Reparing a drift
----------------

When this test fails, the per-host installed block has drifted from
the canonical render. To repair, run the canonical refresh path:

  make refresh-installed

This regenerates the snippets and re-runs each per-host installer in
idempotent mode; the installer takes a backup of the old block and
writes the current one in place, preserving any user content outside
the managed-section markers. It writes to real install targets, so it
is a deployment action, not a test action.
"""

from __future__ import annotations

import importlib.util
import json
import os
import re
import shutil
import sys
import tempfile
import unittest
from pathlib import Path


# Path derivation matches test_render_managed_blocks.py: derive from
# the test file location so the suite runs in CI, fresh clones, and
# alternate mounts without hardcoded paths.
AGENT_INSTALLATION = Path(__file__).resolve().parent.parent
RENDER_SCRIPT = AGENT_INSTALLATION / "scripts" / "render_managed_blocks.py"
RECONCILE_SCRIPT = AGENT_INSTALLATION / "scripts" / "reconcile_managed_blocks.py"
CANONICAL_SOURCE = AGENT_INSTALLATION / "MPM_AGENT_INTEGRATION_SNIPPETS.md"

if str(AGENT_INSTALLATION / "scripts") not in sys.path:
    sys.path.insert(0, str(AGENT_INSTALLATION / "scripts"))

import managed_block_convergence as convergence  # noqa: E402
import block_provenance  # noqa: E402

_convergence = convergence


# Adapters whose installer wraps the snippet in a section banner
# rather than emitting bare outer markers. Kept in sync with
# check_installed_managed_blocks.SECTION_BUILDERS.
SECTION_BUILDERS: dict[str, str] = {
    "mpm-memory-openclaw": "install_openclaw_instructions",
}

TS_SENTINEL = "__MPM_TS__"
_GENERATED_TS_RE = re.compile(r"<!-- generated: [^\n]* -->")


def _normalize_generated_ts(text: str) -> str:
    return _GENERATED_TS_RE.sub("<!-- generated: <TS> -->", text)


def _normalize_provenance(text: str) -> str:
    """Erase the installed block's build-provenance comment.

    The provenance (product version, source commit, applied timestamp)
    is a per-install fact about the build, not part of the behavioural
    contract. The canonical render carries none, so removing it from
    the installed side leaves exactly the behavioural text comparable.
    """
    return block_provenance.strip_provenance(text)


def _load_module(name: str, path: Path):
    spec = importlib.util.spec_from_file_location(name, path)
    assert spec and spec.loader, f"{name} must be importable from {path}"
    module = importlib.util.module_from_spec(spec)
    sys.modules[spec.name] = module
    spec.loader.exec_module(module)
    return module


def _integration_installed(adapter_name: str, home: Path) -> bool:
    """Ask the reconciler whether this adapter's integration is
    already installed under `home`.

    Delegating to the same gate the reconciler uses keeps the drift
    detector and the reconciliation pass from disagreeing about
    whether a host is in scope.
    """
    reconcile = _load_module("reconcile_managed_blocks", RECONCILE_SCRIPT)
    manifest_path = AGENT_INSTALLATION / adapter_name / "reconcile.json"
    try:
        manifest = json.loads(manifest_path.read_text(encoding="utf-8"))
    except (OSError, json.JSONDecodeError):
        return False
    installed, _why = reconcile.integration_installed(manifest, str(home))
    return installed


# Host-by-host install target map. Each entry pairs the adapter name
# (as registered in render_managed_blocks.ADAPTERS) with the canonical
# install path the per-host installer writes to. The list deliberately
# covers ONLY persistent-file hosts; runtime-injection-only hosts
# (Hermes via its session-start hook, when the persistent file is
# absent) are out of scope for this test.
#
# Path resolution follows the same conventions as the per-host
# installers themselves (mpm-pi/scripts/install_agents_instructions.py
# uses Path.home() / ".pi" / "agent" / "AGENTS.md" for user scope; the
# mpm-claude-code installer uses Path.home() / ".claude" / "CLAUDE.md";
# the mpm-opencode installer uses Path.home() / ".config" / "opencode"
# / "AGENTS.md"; the mpm-memory-openclaw installer writes to the
# resolved agent workspace SOUL.md). All five paths are $HOME-anchored
# so the test runs in any user environment.
#
# Adding a new persistent-file host: append an entry here AND register
# the adapter in render_managed_blocks.ADAPTERS.
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
        # OpenClaw resolves the SOUL.md path per-agent from
        # openclaw.json -> agents.entries.<id>.workspace. The default
        # location is $HOME/.openclaw/workspace/SOUL.md per the
        # canonical installer and INSTALL.md.
        "filename": "SOUL.md",
        "relative_to_home": ".openclaw/workspace/SOUL.md",
        "outer_begin": "<!-- BEGIN MPM-MANAGED SECTION:openclaw-instructions -->",
        "outer_end": "<!-- END MPM-MANAGED SECTION:openclaw-instructions -->",
        # OpenClaw's managed block is required once the integration is
        # installed, so it is in scope exactly when the reconciler
        # would reconcile it.
        "integration_probe": "mpm-memory-openclaw",
    },
]


def _load_render_module():
    return _load_module("render_managed_blocks", RENDER_SCRIPT)


_render = _load_render_module()


def _canonical_block_for_host(adapter_name: str) -> str:
    """Return the exact bytes this host's installer writes today.

    Most hosts write bare outer markers around the rendered snippet.
    OpenClaw's installer wraps the snippet in a section banner
    carrying a `<!-- generated: TS -->` line, so its expected bytes
    come from that installer itself; otherwise the comparison would
    test bytes the installer never produces.

    Note: the snippet is the FULL host-specific composition
    (header + rendered block + footer), not just the bare
    universal canonical block. Hosts that wrap the universal
    block with their own host-specific text (notably Pi, which
    prepends the MPM-pi load-order context and appends the
    Pi-specific load-order notes) need that wrapping for
    byte-equality."""
    canonical_text = CANONICAL_SOURCE.read_text(encoding="utf-8")
    canonical_block = _render.extract_canonical_block(canonical_text)
    adapter = next(a for a in _render.ADAPTERS if a["name"] == adapter_name)
    rendered = _render.render_for_host(canonical_block, adapter["tool_prefix"])
    snippet = _render.compose_snippet(
        rendered, adapter["header"], adapter["footer"],
    )

    module_name = SECTION_BUILDERS.get(adapter_name)
    if module_name is None:
        return f'{adapter["copy_paste_outer_begin"]}\n{snippet}{adapter["copy_paste_outer_end"]}\n'

    installer = _load_module(
        module_name,
        AGENT_INSTALLATION / adapter_name / "scripts" / f"{module_name}.py",
    )
    return installer.build_managed_section(snippet, existing_ts=TS_SENTINEL)


def _extract_installed_block(text: str, outer_begin: str, outer_end: str) -> str | None:
    """Return the slice of `text` between the outer managed-section
    markers, or None if the markers are absent or only one is present
    (corrupted target — same rule the per-host installer applies)."""
    if (outer_begin in text) != (outer_end in text):
        return None
    begin = text.find(outer_begin)
    if begin == -1:
        return None
    end = text.find(outer_end, begin + len(outer_begin))
    if end == -1:
        return None
    end_line_end = text.find("\n", end)
    if end_line_end == -1:
        end_line_end = len(text)
    else:
        end_line_end += 1
    return text[begin:end_line_end]


def detect_installed_block_drift(home: Path) -> tuple[list[str], list[str]]:
    """Return (failures, skips) for the installed managed blocks under `home`.

    Parameterised by home root so the same detection logic can be exercised
    against a synthetic install tree (hermetic) and against the operator's
    real one (the `make check-installed-drift` operational check).
    """
    failures: list[str] = []
    skips: list[str] = []

    for entry in HOST_INSTALL_TARGETS:
        install_path = home / entry["relative_to_home"]

        # A host with a declared integration gate is in scope only
        # when that integration is actually installed here. This
        # mirrors the reconciler exactly, so the drift detector
        # never flags a host this machine does not use -- and
        # never skips one it does.
        probe = entry.get("integration_probe")
        if probe and not _integration_installed(probe, home):
            skips.append(
                f"{entry['host']}: integration not installed on this "
                f"machine -- skipping (MPM does not opt a machine into "
                f"a host it does not use)"
            )
            continue

        if not install_path.is_file():
            skips.append(
                f"{entry['host']}: install target absent ({install_path}) "
                f"-- skipping (presence is a separate concern)"
            )
            continue

        try:
            expected = _canonical_block_for_host(entry["adapter"])
        except StopIteration:
            failures.append(
                f"{entry['host']}: adapter '{entry['adapter']}' not "
                f"registered in render_managed_blocks.ADAPTERS -- "
                f"HOST_INSTALL_TARGETS is out of sync"
            )
            continue

        text = install_path.read_text(encoding="utf-8")
        installed = _extract_installed_block(
            text, entry["outer_begin"], entry["outer_end"],
        )
        if installed is None:
            # Block absent (or one-sided) at an existing install
            # target. For a host whose integration is installed,
            # the missing persistent managed block IS drift: the
            # per-host installer repairs it, and `make
            # refresh-installed` now reconciles it too. Fail
            # rather than skip, because silently skipping is
            # exactly what let an installed OpenClaw run without
            # its block.
            #
            # Report the file's ACTUAL marker state rather than only
            # whether THIS host's markers matched. A file carrying
            # another host's section, two sections, or a truncated
            # one is a materially different repair from a file with
            # no MPM content at all, and "markers are absent"
            # misdescribes all of them.
            analysis = _convergence.analyze(text)
            found = (
                ", ".join(convergence.describe_markers(text)) or "none"
            )
            state = (
                f"file carries MPM markers [{found}]"
                if found != "none" else
                f"no MPM marker of any kind is present"
            )
            detail = (
                f"{entry['host']}: install target exists at "
                f"{install_path} but this host's managed section is "
                f"not present (BEGIN={entry['outer_begin'] in text}, "
                f"END={entry['outer_end'] in text}); {state} -- the "
                f"persistent managed block is required for reliable "
                f"MPM use, and the file must end up with exactly one"
            )
            if analysis.problems:
                detail += (
                    f"\n  structural problems: "
                    + "; ".join(analysis.problems)
                )
            if probe:
                failures.append(detail + "\n  repair: make refresh-installed")
            else:
                skips.append(
                    detail
                    + " -- skipping drift check (run per-host install.sh "
                      "to add the managed block; this test asserts only "
                      "byte-parity when the block is present)"
                )
            continue

        if _normalize_provenance(_normalize_generated_ts(installed)) != _normalize_provenance(_normalize_generated_ts(expected)):
            # Show the *first* divergent line plus a byte offset to
            # help the operator locate the drift. A full unified
            # diff would be too noisy for a test failure summary.
            expected_lines = expected.splitlines(keepends=True)
            installed_lines = installed.splitlines(keepends=True)
            first_diff = None
            for i, (a, b) in enumerate(zip(expected_lines, installed_lines)):
                if a != b:
                    first_diff = (i, a, b)
                    break
            if first_diff is None:
                # Length mismatch with same prefix.
                first_diff = (
                    min(len(expected_lines), len(installed_lines)),
                    expected_lines[first_diff[0]] if first_diff[0] < len(expected_lines) else "<missing in expected>\n",
                    installed_lines[first_diff[0]] if first_diff[0] < len(installed_lines) else "<missing in installed>\n",
                )
            line_no, exp, ins = first_diff
            failures.append(
                f"{entry['host']}: installed managed block at "
                f"{install_path} drifted from canonical render.\n"
                f"  expected bytes: {len(expected)}\n"
                f"  installed bytes: {len(installed)}\n"
                f"  first divergent line: {line_no + 1}\n"
                f"    expected: {exp.rstrip()[:200]}\n"
                f"    installed: {ins.rstrip()[:200]}\n"
                f"  repair: make refresh-installed"
            )

    return failures, skips


class InstalledBlockDriftHermetic(unittest.TestCase):
    """Exercise the drift DETECTION LOGIC against a synthetic install
    tree, never the operator's real one.

    Canonical `make test` must validate the source tree, so it cannot
    assert anything about what happens to be installed on the machine
    running it — that couples source correctness to deployment state
    and makes the gate unpassable for anyone whose install predates the
    current source. It also means this file must never write to a real
    agent instruction file.

    These tests therefore build a throwaway HOME under t.TempDir() and
    assert both directions of the detector: a matching install is
    accepted, a stale one is reported. The live-machine assertion lives
    in InstalledBlockDrift below and runs only under
    `make check-installed-drift`.
    """

    def setUp(self):
        self.home = Path(tempfile.mkdtemp(prefix="mpm-drift-hermetic-"))
        self.addCleanup(shutil.rmtree, self.home, ignore_errors=True)

    def _write_installed(self, entry: dict, text: str) -> Path:
        target = self.home / entry["relative_to_home"]
        target.parent.mkdir(parents=True, exist_ok=True)
        target.write_text(text, encoding="utf-8")
        return target

    def _expected_block(self, entry: dict) -> str:
        return _canonical_block_for_host(entry["adapter"])

    def test_matching_installed_blocks_are_accepted(self):
        """Write each probe-free host's CURRENT expected bytes into a
        synthetic HOME. The detector must report no drift — proving the
        detector can pass, not merely that it can fail."""
        written = 0
        for entry in HOST_INSTALL_TARGETS:
            if entry.get("integration_probe"):
                # Probe-gated hosts need a real integration layout to
                # enter scope; that is exercised by the live check, not
                # by a synthetic tree.
                continue
            self._write_installed(entry, self._expected_block(entry))
            written += 1
        self.assertGreater(written, 0, "fixture wrote no install targets")

        failures, _skips = detect_installed_block_drift(self.home)
        self.assertEqual(
            [], failures,
            "a synthetic install matching the canonical render must not "
            f"be reported as drift: {failures}",
        )

    def test_stale_installed_blocks_are_reported(self):
        """Write each probe-free host's expected bytes, then mutate the
        managed section the way an older MPM would have left it. The
        detector must report every mutated host."""
        mutated = []
        for entry in HOST_INSTALL_TARGETS:
            if entry.get("integration_probe"):
                continue
            text = self._expected_block(entry)
            # Anchor the mutation on the block's reference-discovery
            # commitment, not on the number of the responsibility it
            # sits under: a numbered anchor would silently stop
            # mutating once the contract was renumbered.
            stale = text.replace(
                "read the underlying source rather than relying solely on "
                "a search-result summary",
                "Older invariant label that the current source no longer"
                " emits",
            )
            self.assertNotEqual(
                text, stale,
                "fixture failed to make the block stale, so this test "
                "would pass without exercising the failure path",
            )
            self._write_installed(entry, stale)
            mutated.append(entry["host"])

        failures, _skips = detect_installed_block_drift(self.home)
        self.assertTrue(mutated, "fixture mutated no hosts")
        self.assertEqual(
            sorted(mutated), sorted(f.split(":")[0] for f in failures),
            "every stale host must be reported, and only stale hosts",
        )
        for f in failures:
            self.assertIn("repair: make refresh-installed", f)

    def test_missing_managed_block_at_existing_target_is_reported(self):
        """An install target that exists but carries no managed section
        must never be silently ignored.

        The severity differs by host, deliberately: a probe-gated host
        (one whose integration the reconciler has already adopted) is a
        FAILURE, because its managed block is required and a missing one
        is what let an installed OpenClaw run unmanaged. For a host with
        no integration probe there is no evidence the host is in use, so
        it is a SKIP — but only after being named and given its repair
        path. This test pins that distinction: reported either way,
        never dropped.
        """
        entry = next(e for e in HOST_INSTALL_TARGETS
                     if not e.get("integration_probe"))
        self._write_installed(entry, "# my own notes, no MPM markers\n")

        failures, skips = detect_installed_block_drift(self.home)
        reported = [f for f in failures if entry["host"] in f]
        reported += [s for s in skips if entry["host"] in s]

        self.assertTrue(
            reported,
            "an existing install target with no managed block must be "
            f"reported, not dropped; failures={failures} skips={skips}",
        )
        self.assertIn(
            "run per-host install.sh", reported[0],
            "the report must carry the repair path, so an operator who "
            "sees it knows what to do",
        )
        self.assertEqual(
            [], [f for f in failures if entry["host"] in f],
            "a host with no integration probe is not proven to be in use, "
            "so a missing block there is a skip rather than a failure",
        )

    def test_absent_install_target_is_not_a_failure(self):
        """An empty synthetic HOME has nothing installed. That is a
        presence question, not a drift question, and must not fail."""
        failures, _skips = detect_installed_block_drift(self.home)
        self.assertEqual([], failures)


class InstalledBlockDrift(unittest.TestCase):
    """For each persistent-file host whose install target exists on
    THIS machine, the installed managed section must byte-match the
    canonical render.

    Absence is *not* a failure (handled by separate presence tests).
    The per-host installer runs idempotently; the canonical refresh
    path (`make refresh-installed`) re-runs all five installers in
    sequence and the final `render_managed_blocks.py --check` asserts
    snippet parity.

    This is an OPERATIONAL check, not a source check, so it is opt-in.
    It runs only when MPM_CHECK_INSTALLED_DRIFT=1 is set, which
    `make check-installed-drift` does. Rationale: a source-tree gate
    that fails whenever the install predates the source cannot be
    passed by a correct source change, and cannot be trusted to mean
    "the source is broken" when it does fail. The detection logic
    itself is covered hermetically by InstalledBlockDriftHermetic, so
    opting this in adds the machine-specific scope without leaving the
    logic untested. This test is read-only and never writes an install
    target.
    """

    @unittest.skipUnless(
        os.environ.get("MPM_CHECK_INSTALLED_DRIFT") == "1",
        "installed-runtime drift check is opt-in; run "
        "`make check-installed-drift`",
    )
    def test_all_installed_blocks_match_canonical_render(self):
        failures, skips = detect_installed_block_drift(Path.home())
        for s in skips:
            sys.stderr.write(f"[skip] {s}\n")
        if failures:
            self.fail(
                "Installed managed blocks drifted from canonical render:\n  - "
                + "\n  - ".join(failures)
            )


if __name__ == "__main__":
    unittest.main(verbosity=2)
