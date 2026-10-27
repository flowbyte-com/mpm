"""
test_legacy_installation_preserved.py — a pre-existing installation
survives an update, and survives a RETIREMENT.

WHY THIS FILE EXISTS
--------------------

This is a HOST-INDEPENDENT safety property, and it is deliberately
written so that it names NO adapter directory in its assertions.

The property:

  The repository-wide update paths — the managed-block reconciler and
  the root installer's reconcile phase — must NEVER destructively
  mutate a pre-existing installation. Concretely, for a host MPM no
  longer ships:

    * the host's configuration files are byte-identical afterwards;
    * a previously installed MPM managed instruction block is still
      there, and is NOT deleted;
    * the host's plugin registration is byte-identical;
    * the user's own instruction text is byte-identical;
    * MPM's own database and persistent state are byte-identical;
    * nothing is created under the host's directory, and nothing is
      removed from it.

WHY IT IS HOST-INDEPENDENT
--------------------------

Every other safety suite in this directory is host-SPECIFIC: they name
one adapter's directory, its manifest and its installer. That is right
for the property they pin, but it has a blind spot exactly where the
risk now is.

A host-specific suite lives or dies with its adapter. If the adapter
directory is deleted — which is what "retire the legacy adapters"
means — the suite that guarded its refresh safety goes with it, and the
guarantee is left unpinned at the exact moment it becomes load-bearing.
An operator who upgrades after the removal would have no test saying
that their existing Claude Code / Pi / Hermes / OpenClaw installation
survived.

So the property is pinned HERE, against a synthetic host that has no
adapter in the repository, and against a SIMULATED RETIREMENT: a copy
of `agent_installation/` with the adapter directories removed. The
assertions do not care which adapters exist. They care that a
pre-existing installation is left alone.

THE TWO VARIANTS
----------------

  full-repo     the reconciler runs against the repository as it is,
                restricted to the adapters whose installers are cheap
                and side-effect-free to invoke in a sandbox. The
                legacy hosts' OWN managed blocks may be refreshed — that
                is the managed-block contract — but the user's own text
                around them must survive verbatim, and no pre-existing
                file may be removed.

  retired       the reconciler runs against a copy of
                `agent_installation/` with EVERY adapter directory
                removed. This is the post-removal state. Nothing may
                change at all — and above all, no orphan managed block
                may be deleted, because a retired host's installer no
                longer exists to manage it.

The second variant is the one that proves the operator's requirement.
It is not vacuous: a reconciler that carried cleanup logic for
"managed blocks whose adapter is gone" would delete the orphan blocks
in the seeded hosts, and this test would fail.

SURVIVING THE REMOVAL
---------------------

Every assertion here is written to hold both BEFORE and AFTER the
legacy adapters are removed. The adapters that exist are DISCOVERED at
run time, not hard-coded:

  * a host whose adapter is present has its managed block refreshed,
    and its user's own text is asserted to survive verbatim;
  * a host whose adapter is absent is asserted to be byte-identical;
  * the retired variant is asserted in both states.

A hard-coded adapter name in an assertion would make this file a
host-specific suite in disguise, and it would fail on the day the
adapters are removed — taking the guarantee's only test with it.

SANDBOXING
----------

Every subprocess runs against a disposable HOME. HOME redirects file
paths; it does not redirect service control, so `XDG_RUNTIME_DIR` and
`DBUS_SESSION_BUS_ADDRESS` are also pointed at empty paths, and
`systemctl`/`loginctl` are shadowed by recording stubs on PATH. The
reconciler only invokes adapter installers, and the ones exercised here
are python scripts that touch files — but the shadowing is kept so that
a future change to the reconciler cannot reach the operator's live
systemd manager from inside a test.
"""

from __future__ import annotations

import hashlib
import json
import os
import re
import shutil
import subprocess
import sys
import tempfile
import unittest
from pathlib import Path

REPO_ROOT = Path(__file__).resolve().parents[2]
AGENT_INSTALLATION = REPO_ROOT / "agent_installation"
RECONCILE = AGENT_INSTALLATION / "scripts" / "reconcile_managed_blocks.py"

# The managed-block markers, per host. Taken from
# agent_installation/scripts/check_installed_managed_blocks.py, which
# is the diagnostic that owns these strings.
MARKERS = {
    "opencode": (
        "<!-- BEGIN MPM-MANAGED SECTION:opencode-instructions -->",
        "<!-- END MPM-MANAGED SECTION:opencode-instructions -->",
    ),
    "openclaw": (
        "<!-- BEGIN MPM-MANAGED SECTION:openclaw-instructions -->",
        "<!-- END MPM-MANAGED SECTION:openclaw-instructions -->",
    ),
    "claude_code": (
        "<!-- BEGIN MPM-MANAGED SECTION:claude-code-instructions -->",
        "<!-- END MPM-MANAGED SECTION:claude-code-instructions -->",
    ),
    "pi": (
        "<!-- BEGIN MPM-MANAGED SECTION:pi-instructions -->",
        "<!-- END MPM-MANAGED SECTION:pi-instructions -->",
    ),
}

# The legacy hosts, and the adapter that manages each of them. The
# adapter name is used only to CONFIGURE the run (which adapters to
# invoke); every assertion about the host's files is adapter-agnostic.
LEGACY_HOSTS = {
    "pi": {
        "adapter": "mpm-pi",
        "rel": ".pi/agent/AGENTS.md",
        "marker": "pi",
    },
    "claude_code": {
        "adapter": "mpm-claude-code",
        "rel": ".claude/CLAUDE.md",
        "marker": "claude_code",
    },
    "openclaw": {
        "adapter": "mpm-memory-openclaw",
        "rel": ".openclaw/workspace/SOUL.md",
        "marker": "openclaw",
    },
}

# A host MPM does not ship an adapter for, at all, ever. This is the
# synthetic host that stands in for "a host we retired".
RETIRED_HOST_DIR = ".retiredhost"

# The user's own text, above and below the managed block. It is
# distinctive on purpose: an assertion that compares it verbatim fails
# if the installer drops, reorders or rewrites any of it.
USER_ABOVE = "# My own file\n\nMy own rules, above the managed block.\n\n"
USER_BELOW = "\nMy own rules, below the managed block.\nAnd one more line.\n"

# The lint's vocabulary. A legacy host's directory, and the shapes a
# deletion takes in shell and in Python.
_HOST_PATH_RE = re.compile(
    r"\.claude\b|\.openclaw\b|\.pi/|\.config/opencode\b"
)
_DELETION_RE = re.compile(
    r"\brm\s+-[a-zA-Z]*[rf]|\brmtree\b|\bunlink\b|\bos\.remove\b"
    r"|\bshutil\.rmtree\b|\.unlink\("
)


def _strip_comment(line: str) -> str:
    """Drop a shell or Python comment from a line of code."""
    stripped = line.strip()
    if stripped.startswith("#"):
        return ""
    # A Python comment, but not a '#' inside a string literal. The
    # shared scripts have no such literal today; if one appears, the
    # lint simply skips the line, which is the safe direction.
    if "#" in line:
        head = line.split("#", 1)[0]
        if head.count('"') % 2 == 0 and head.count("'") % 2 == 0:
            return head
    return line



def _managed_block(host: str, body: str) -> str:
    """Render a plausible previously-installed managed block."""
    begin, end = MARKERS[host]
    return f"{begin}\n{body}\n{end}\n"


def _fingerprint(root: Path) -> dict[str, str]:
    """sha256 of every file under root, keyed by relative path.

    A missing file and an empty file are different states, so the
    fingerprint records the file's presence as well as its bytes: a
    deletion and a truncation must both be visible.
    """
    out: dict[str, str] = {}
    if not root.exists():
        return out
    for path in sorted(root.rglob("*")):
        rel = str(path.relative_to(root))
        if path.is_dir():
            out[rel + "/"] = "<dir>"
        elif path.is_symlink():
            out[rel] = "<symlink:" + os.readlink(path) + ">"
        else:
            out[rel] = hashlib.sha256(path.read_bytes()).hexdigest()
    return out


def _discovered_adapters() -> set[str]:
    """The adapter directories that exist in the repository right now.

    Adapters with a `provision` step are excluded: they build and
    stage a plugin payload (an npm build), which is neither cheap nor
    side-effect-free in a sandbox. Adapters with `opt_in: true` or no
    installer are excluded because the reconciler skips them by design.
    """
    found: set[str] = set()
    for adapter in sorted(AGENT_INSTALLATION.glob("mpm-*")):
        manifest = adapter / "reconcile.json"
        if not manifest.is_file():
            continue
        try:
            data = json.loads(manifest.read_text(encoding="utf-8"))
        except json.JSONDecodeError:
            continue
        if data.get("provision"):
            continue
        if data.get("opt_in"):
            continue
        if not data.get("installer"):
            continue
        found.add(adapter.name)
    return found


class _Sandbox:
    """A disposable HOME with service control shadowed out."""

    def __init__(self) -> None:
        self.root = Path(tempfile.mkdtemp(prefix="mpm-legacy-preserved-"))
        self.home = self.root / "home"
        self.bin = self.root / "bin"
        self.home.mkdir(parents=True)
        self.bin.mkdir(parents=True)
        self._write_stub("systemctl")
        self._write_stub("loginctl")
        self.env = {
            "HOME": str(self.home),
            "PATH": f"{self.bin}{os.pathsep}{os.environ.get('PATH', '')}",
            "XDG_RUNTIME_DIR": str(self.root / "run"),
            "DBUS_SESSION_BUS_ADDRESS": str(self.root / "nonexistent-bus"),
            "LANG": "C",
        }
        (self.root / "run").mkdir()

    def _write_stub(self, name: str) -> None:
        stub = self.bin / name
        stub.write_text(
            "#!/bin/sh\n"
            f"echo '{name} $@' >> '{self.root / (name + '.calls')}'\n"
            "exit 0\n",
            encoding="utf-8",
        )
        stub.chmod(0o755)

    def calls(self, name: str) -> list[str]:
        path = self.root / (name + ".calls")
        if not path.exists():
            return []
        return path.read_text(encoding="utf-8").splitlines()

    def cleanup(self) -> None:
        shutil.rmtree(self.root, ignore_errors=True)


class LegacyInstallationPreserved(unittest.TestCase):
    """The property: an update and a retirement both preserve."""

    # ── the seeded pre-existing installation ──────────────────────────

    def _seed(self, sb: _Sandbox, hosts: tuple[str, ...] = ()) -> None:
        """Seed a pre-existing installation across every surface.

        `hosts` names the legacy hosts to seed. A host that is not
        seeded is one the operator does not use — the reconciler must
        not create it.

        The surfaces are the ones an operator can lose: a host's own
        configuration, a previously installed managed instruction
        block, a plugin registration, the user's own instruction text,
        and MPM's own database and persistent state.
        """
        home = sb.home

        # (1) A host MPM does NOT ship an adapter for. This is the
        #     post-removal shape, and the one the property is about.
        retired = home / RETIRED_HOST_DIR
        (retired / "workspace").mkdir(parents=True)
        (retired / "extensions" / "mpm-retiredhost").mkdir(parents=True)
        (retired / "config.json").write_text(
            json.dumps({"theme": "dark", "plugins": {"entries": {}}}, indent=2),
            encoding="utf-8",
        )
        (retired / "workspace" / "INSTRUCTIONS.md").write_text(
            USER_ABOVE
            + _managed_block(
                "opencode",
                "An OLD MPM managed block, installed by a previous version.",
            )
            + USER_BELOW,
            encoding="utf-8",
        )
        (retired / "extensions" / "mpm-retiredhost" / "plugin.json").write_text(
            json.dumps({"name": "mpm-retiredhost", "enabled": True}), encoding="utf-8"
        )

        # (2) The legacy hosts' own instruction files, each carrying a
        #     previously installed managed block and the user's own
        #     text around it.
        for host in hosts:
            spec = LEGACY_HOSTS[host]
            path = home / spec["rel"]
            path.parent.mkdir(parents=True, exist_ok=True)
            path.write_text(
                USER_ABOVE
                + _managed_block(
                    spec["marker"],
                    f"OLD MPM managed block for {host}.",
                )
                + USER_BELOW,
                encoding="utf-8",
            )

        # (3) An OpenClaw installation: its config carries a PLUGIN
        #     REGISTRATION, and its SOUL.md carries a managed block.
        #     The plugin directory is what satisfies the adapter's
        #     `reconcile_if` gate.
        if "openclaw" in hosts:
            (home / ".openclaw" / "extensions" / "mpm-memory-openclaw").mkdir(
                parents=True, exist_ok=True
            )
            (home / ".openclaw" / "openclaw.json").write_text(
                json.dumps(
                    {
                        "plugins": {
                            "entries": {
                                "mpm-memory-openclaw": {"enabled": True},
                                "someone-elses-plugin": {"enabled": False},
                            }
                        },
                        "slots": {"memory": "mpm-memory-openclaw"},
                    },
                    indent=2,
                ),
                encoding="utf-8",
            )

        # (4) A user's own OpenCode instruction file, with an unrelated
        #     plugin registration beside MPM's.
        oc = home / ".config" / "opencode"
        oc.mkdir(parents=True)
        (oc / "AGENTS.md").write_text(
            USER_ABOVE
            + _managed_block("opencode", "OLD MPM managed block for OpenCode.")
            + USER_BELOW,
            encoding="utf-8",
        )
        (oc / "opencode.jsonc").write_text(
            '{\n  // a comment the user wrote\n'
            '  "plugins": [\n    "file:///somewhere/other-plugin"\n  ]\n}\n',
            encoding="utf-8",
        )

        # (5) MPM's own persistent state.
        mpm = home / ".mpm"
        (mpm / "src" / "db").mkdir(parents=True)
        (mpm / "bin").mkdir(parents=True)
        (mpm / "src" / "db" / "mpm.db").write_bytes(b"SQLite format 3\x00FAKE-DB-BYTES")
        (mpm / "bin" / "mpm").write_bytes(b"#!/bin/sh\nexit 0\n")
        (mpm / "config.json").write_text('{"workspace": "."}\n', encoding="utf-8")

    # ── the reconciler runs ───────────────────────────────────────────

    def _reconcile(
        self,
        sb: _Sandbox,
        *,
        retired: bool,
        only: list[str] | None = None,
    ) -> subprocess.CompletedProcess:
        """Run the reconciler, optionally against a retired tree.

        `retired=True` runs a COPY of `agent_installation/` with every
        adapter directory removed. That copy is the post-removal state,
        and it is what makes the property checkable without waiting for
        the removal to happen.
        """
        if retired:
            tree = sb.root / "retired-agent-installation"
            if not tree.exists():
                shutil.copytree(
                    AGENT_INSTALLATION,
                    tree,
                    ignore=shutil.ignore_patterns(
                        "__pycache__", "node_modules", "dist", ".build"
                    ),
                )
                for child in list(tree.glob("mpm-*")):
                    if child.is_dir():
                        shutil.rmtree(child)
            script = tree / "scripts" / "reconcile_managed_blocks.py"
        else:
            script = RECONCILE

        argv = [sys.executable, str(script), "--home", str(sb.home)]
        for name in only or []:
            argv += ["--only", name]
        return subprocess.run(
            argv, capture_output=True, text=True, env=sb.env, timeout=300
        )

    def _cheap_adapters(self) -> list[str]:
        """The adapters this run may invoke, as far as they exist.

        Post-removal this is empty. The reconciler is then invoked
        with a `--only` that matches nothing, which is the "the update
        path did nothing" case the operator's requirement describes.
        """
        discovered = _discovered_adapters()
        return [spec["adapter"] for spec in LEGACY_HOSTS.values()
                if spec["adapter"] in discovered]

    # ── the retired variant: the operator's requirement ───────────────

    def test_a_retired_host_s_installation_is_untouched(self) -> None:
        """A host with no adapter keeps EVERYTHING, byte for byte.

        This is the post-removal state, and it is the operator's
        requirement stated as an assertion.
        """
        sb = _Sandbox()
        self.addCleanup(sb.cleanup)
        self._seed(sb)

        retired_dir = sb.home / RETIRED_HOST_DIR
        before = _fingerprint(retired_dir)
        self.assertTrue(before, "the seeded installation must exist")

        proc = self._reconcile(sb, retired=True)
        # A retired tree has no adapters; the reconciler says so and
        # exits cleanly. That is the expected shape, not an error.
        self.assertEqual(proc.returncode, 0, proc.stderr)

        after = _fingerprint(retired_dir)
        self.assertEqual(
            before,
            after,
            "a host with no adapter must be byte-identical after the update — "
            "no file created, removed or rewritten",
        )
        # The previously installed managed block must still be there.
        text = (retired_dir / "workspace" / "INSTRUCTIONS.md").read_text(
            encoding="utf-8"
        )
        self.assertIn(
            MARKERS["opencode"][0],
            text,
            "the previously installed managed block must NOT be deleted",
        )
        self.assertIn("My own rules, below the managed block.", text)

    def test_a_retirement_leaves_the_whole_home_byte_identical(self) -> None:
        """The strongest form: after a retirement, NOTHING changes.

        Not one file anywhere in the operator's home may be created,
        removed or rewritten — not their host configuration, not their
        instruction files, not their plugin registrations, and not
        MPM's own database.
        """
        sb = _Sandbox()
        self.addCleanup(sb.cleanup)
        self._seed(sb, hosts=tuple(LEGACY_HOSTS))

        before = _fingerprint(sb.home)

        proc = self._reconcile(sb, retired=True)
        self.assertEqual(proc.returncode, 0, proc.stderr)

        after = _fingerprint(sb.home)
        added = sorted(set(after) - set(before))
        removed = sorted(set(before) - set(after))
        changed = sorted(
            k for k in set(before) & set(after) if before[k] != after[k]
        )
        self.assertEqual(
            added, [], "a retirement must not create any file in the operator's home"
        )
        self.assertEqual(
            removed, [], "a retirement must not remove any file in the operator's home"
        )
        self.assertEqual(
            changed,
            [],
            "a retirement must not rewrite any file in the operator's home",
        )

    def test_retirement_does_not_delete_any_orphan_managed_block(self) -> None:
        """Retiring the adapters must not delete the blocks they left.

        After a removal, the legacy hosts' installers no longer exist,
        so nobody can refresh their managed blocks. Deleting them would
        be the destructive choice — it would strip an operator's MPM
        contract out of a file the operator wrote, on the grounds that
        MPM no longer supports that host. Leaving them is the safe
        choice: they are inert prose in a file the operator owns.
        """
        sb = _Sandbox()
        self.addCleanup(sb.cleanup)
        self._seed(sb, hosts=tuple(LEGACY_HOSTS))

        survivors = {
            ".claude/CLAUDE.md": "claude_code",
            ".pi/agent/AGENTS.md": "pi",
            ".openclaw/workspace/SOUL.md": "openclaw",
            f"{RETIRED_HOST_DIR}/workspace/INSTRUCTIONS.md": "opencode",
        }
        for rel, host in survivors.items():
            path = sb.home / rel
            self.assertIn(MARKERS[host][0], path.read_text(encoding="utf-8"))
            before = path.read_bytes()

            proc = self._reconcile(sb, retired=True)
            self.assertEqual(proc.returncode, 0, proc.stderr)

            self.assertEqual(
                path.read_bytes(),
                before,
                f"{rel} must be byte-identical after a retirement — an orphan "
                f"managed block must not be deleted or rewritten",
            )

    def test_the_mpm_database_and_persistent_state_are_untouched(self) -> None:
        """MPM's own state is never in scope for a host reconciliation."""
        sb = _Sandbox()
        self.addCleanup(sb.cleanup)
        self._seed(sb, hosts=tuple(LEGACY_HOSTS))

        mpm_dir = sb.home / ".mpm"
        before = _fingerprint(mpm_dir)

        proc = self._reconcile(sb, retired=True)
        self.assertEqual(proc.returncode, 0, proc.stderr)

        after = _fingerprint(mpm_dir)
        self.assertEqual(
            before,
            after,
            "MPM's database and persistent state must be byte-identical — a host "
            "reconciliation has no business touching them",
        )

    # ── the full-repo variant: an ordinary update ─────────────────────

    def test_the_full_repository_run_preserves_the_users_own_text(self) -> None:
        """With the adapters present, only the managed region may change.

        The managed-block contract is that MPM owns the region between
        the markers and nothing else. So the legacy hosts' files may be
        refreshed INSIDE the markers — that is the contract — but the
        user's own text around them must survive verbatim, and no
        pre-existing file may be removed.
        """
        sb = _Sandbox()
        self.addCleanup(sb.cleanup)
        self._seed(sb, hosts=tuple(LEGACY_HOSTS))

        adapters = self._cheap_adapters()
        before_text = {
            host: (sb.home / spec["rel"]).read_text(encoding="utf-8")
            for host, spec in LEGACY_HOSTS.items()
        }
        before_files = {
            spec["rel"]: _fingerprint(sb.home / Path(spec["rel"]).parent)
            for spec in LEGACY_HOSTS.values()
        }
        retired_before = _fingerprint(sb.home / RETIRED_HOST_DIR)
        mpm_before = _fingerprint(sb.home / ".mpm")
        plugin_reg = sb.home / ".config" / "opencode" / "opencode.jsonc"
        plugin_reg_before = plugin_reg.read_bytes()

        proc = self._reconcile(sb, retired=False, only=adapters or ["__none__"])
        self.assertEqual(proc.returncode, 0, proc.stderr)

        refreshed = 0
        for host, spec in LEGACY_HOSTS.items():
            path = sb.home / spec["rel"]
            self.assertTrue(path.exists(), f"{spec['rel']} must still exist")
            text = path.read_text(encoding="utf-8")

            if spec["adapter"] in adapters:
                # The user's own text, verbatim, above and below.
                self.assertTrue(
                    text.startswith(USER_ABOVE),
                    f"{spec['rel']}: the user's own text above the managed block "
                    f"must survive verbatim.\n--- got ---\n{text[:400]!r}",
                )
                self.assertTrue(
                    text.endswith(USER_BELOW),
                    f"{spec['rel']}: the user's own text below the managed block "
                    f"must survive verbatim.\n--- got tail ---\n{text[-300:]!r}",
                )
                self.assertIn(
                    MARKERS[spec["marker"]][0],
                    text,
                    f"{spec['rel']}: the managed block must still be present "
                    f"afterwards",
                )
                if text != before_text[host]:
                    refreshed += 1
            else:
                # The adapter is gone: this host is not managed by
                # anyone any more, so nothing may change.
                self.assertEqual(
                    text,
                    before_text[host],
                    f"{spec['rel']}: a host with no adapter must be byte-identical "
                    f"after the update",
                )

            # No pre-existing file may be removed, whatever happened.
            after_files = _fingerprint(sb.home / Path(spec["rel"]).parent)
            removed = sorted(set(before_files[spec["rel"]]) - set(after_files))
            self.assertEqual(
                removed,
                [],
                f"{spec['rel']}: the update must not remove any pre-existing file",
            )

        self.assertEqual(
            _fingerprint(sb.home / RETIRED_HOST_DIR),
            retired_before,
            "a host with no adapter must be untouched even when other adapters run",
        )
        self.assertEqual(
            _fingerprint(sb.home / ".mpm"),
            mpm_before,
            "MPM's own state must be untouched by a host reconciliation",
        )
        self.assertEqual(
            plugin_reg.read_bytes(),
            plugin_reg_before,
            "an existing plugin registration must not be rewritten by a host "
            "reconciliation that does not manage that host",
        )

        # Non-vacuity: when adapters ARE present, the reconciler must
        # actually have refreshed the managed blocks. Without this, a
        # reconciler that silently did nothing would pass every
        # assertion above.
        self.assertEqual(
            refreshed,
            len(adapters),
            f"expected {len(adapters)} managed block(s) to be refreshed, "
            f"{refreshed} were — the reconciler did not do the work this "
            f"test claims to observe",
        )

    def test_the_reconciler_never_opts_a_machine_into_a_host(self) -> None:
        """MPM must not create a host's directory for a host it does not use.

        The safety boundary: the existence of integration code in the
        repository must never cause MPM to write into a host's
        configuration. A machine with no OpenClaw integration stays
        untouched.
        """
        sb = _Sandbox()
        self.addCleanup(sb.cleanup)
        # Deliberately NO hosts seeded: the operator uses none of them.
        self._seed(sb)

        absent = [sb.home / ".openclaw", sb.home / ".hermes"]
        for path in absent:
            self.assertFalse(path.exists())

        proc = self._reconcile(sb, retired=False, only=self._cheap_adapters() or ["__none__"])
        self.assertEqual(proc.returncode, 0, proc.stderr)

        for path in absent:
            self.assertFalse(
                path.exists(),
                f"the reconciler must not create {path.name} for a host the "
                f"operator does not use",
            )

    def test_a_second_reconciliation_changes_nothing(self) -> None:
        """Reconciliation is idempotent: the second run is a byte-stable no-op."""
        sb = _Sandbox()
        self.addCleanup(sb.cleanup)
        self._seed(sb, hosts=tuple(LEGACY_HOSTS))

        adapters = self._cheap_adapters() or ["__none__"]
        self._reconcile(sb, retired=False, only=adapters)
        first = _fingerprint(sb.home)

        self._reconcile(sb, retired=False, only=adapters)
        second = _fingerprint(sb.home)

        added = sorted(set(second) - set(first))
        changed = sorted(k for k in set(first) & set(second) if first[k] != second[k])
        self.assertEqual(added, [], "a second reconciliation must create nothing")
        self.assertEqual(
            changed, [], "a second reconciliation must change nothing at all"
        )

    def test_the_reconciler_never_reaches_the_live_service_manager(self) -> None:
        """The sandbox shadows systemctl/loginctl, and the reconciler
        must not have invoked them.

        HOME redirects file paths; it does not redirect service control.
        A reconciler that reached the real user systemd manager could
        restart the operator's live scheduler from inside a test.
        """
        sb = _Sandbox()
        self.addCleanup(sb.cleanup)
        self._seed(sb, hosts=tuple(LEGACY_HOSTS))

        self._reconcile(sb, retired=False, only=self._cheap_adapters() or ["__none__"])

        self.assertEqual(
            sb.calls("systemctl"), [], "the reconciler must not call systemctl"
        )
        self.assertEqual(
            sb.calls("loginctl"), [], "the reconciler must not call loginctl"
        )

    # ── the shared scripts must not learn to clean up ─────────────────

    def test_no_shared_script_deletes_a_legacy_hosts_files(self) -> None:
        """The shared paths must never grow a cleanup step.

        The realistic way a repository removal could destroy an
        operator's existing installation is not the removal itself —
        it is a shared script growing a "now that we no longer support
        this host, tidy up its leftovers" step. A `rm -rf ~/.claude` in
        the root installer, or an `shutil.rmtree` of a legacy host's
        config directory in the reconciler, would take the operator's
        own configuration, their managed block and their plugin
        registration with it.

        This is a lint, not a proof — a sufficiently indirect deletion
        would evade it. It is here to make the destructive shape
        conspicuous at review time, and to fail the moment someone
        writes it.
        """
        shared = [
            REPO_ROOT / "install.sh",
            REPO_ROOT / "uninstall.sh",
            *sorted((AGENT_INSTALLATION / "scripts").glob("*.py")),
        ]
        offenders: list[str] = []
        for script in shared:
            if not script.is_file():
                continue
            lines = script.read_text(encoding="utf-8").splitlines()
            code = [_strip_comment(line) for line in lines]
            # Pass 1: a variable whose VALUE names a legacy host path.
            host_vars = {
                m.group(1)
                for line in code
                for m in [re.match(r"\s*([A-Za-z_][A-Za-z0-9_]*)\s*=", line)]
                if m and _HOST_PATH_RE.search(line)
            }
            # Pass 2: a deletion of that path, directly or via the
            # variable.
            for number, (raw, line) in enumerate(zip(lines, code), start=1):
                if not _DELETION_RE.search(line):
                    continue
                direct = _HOST_PATH_RE.search(line)
                indirect = any(
                    re.search(r"\$\{?" + re.escape(var) + r"\}?", line)
                    for var in host_vars
                )
                if direct or indirect:
                    offenders.append(f"{script.name}:{number}: {raw.strip()}")

        self.assertEqual(
            offenders,
            [],
            "a shared script must never delete a legacy host's files, its "
            "managed block or its plugin registration:\n  "
            + "\n  ".join(offenders),
        )


if __name__ == "__main__":
    unittest.main()
