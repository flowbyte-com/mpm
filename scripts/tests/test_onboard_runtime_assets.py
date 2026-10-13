#!/usr/bin/env python3
"""Runtime-asset provisioning for `mpm onboard` host inspection.

`mpm onboard` inspects installed hosts by running
agent_installation/scripts/check_installed_managed_blocks.py, which
locateHostInspector() (cmd/mpm/onboard_cmds.go:1026) resolves
repo-relative: MPM_REPO_ROOT, then up from the executable, then up from
the CWD. An installed binary at ~/.mpm/bin/mpm satisfies none of those
unless invoked from a source checkout, so `onboard` reported

    ! Host inspection  unavailable: agent_installation/scripts/ not found
                       near the binary or working directory

from every directory except /home/v/src/mpm.

The bug was a PACKAGING gap, not a resolution gap: nothing ever staged
the inspector into the runtime root, so walking up from the executable
had nothing to find. These tests pin that the runtime root carries every
file the inspector transitively needs.

The transitive set is wider than it first appears, and each omission has a
distinct and misleading symptom, which is why each is asserted separately:

  scripts/*.py            absent  -> "not found near the binary"
  reconcile.json          absent  -> FAILS CLOSED to ABSENT for every host,
                                    i.e. a false "no hosts installed"
  templates/*.snippet     absent  -> "could not construct the expected block"
  <adapter>/scripts/*.py  absent  -> "could not construct the expected block"
                                    for adapters with a SECTION_BUILDERS entry

Staging the scripts alone is therefore WORSE than staging nothing: it
converts "I could not look" into a confident wrong answer. The
half-provisioning test at the end pins exactly that.

Hermeticity: every runtime root is a fresh mktemp directory and the
source tree is read-only. Nothing resolves into the real ~/.mpm.
"""
import json
import subprocess
import sys
import tempfile
import unittest
from pathlib import Path

REPO = Path(__file__).resolve().parents[2]
SCRIPT = REPO / "scripts" / "install_runtime_assets.py"
INSPECTOR = "agent_installation/scripts/check_installed_managed_blocks.py"


def provision(runtime_root: Path) -> subprocess.CompletedProcess:
    return subprocess.run(
        [sys.executable, str(SCRIPT),
         "--source-root", str(REPO), "--runtime-root", str(runtime_root)],
        capture_output=True, text=True, cwd=str(REPO),
    )


class TestOnboardingSupportAssets(unittest.TestCase):
    """The runtime root must carry what host inspection transitively reads."""

    @classmethod
    def setUpClass(cls):
        cls._tmp = tempfile.TemporaryDirectory()
        cls.root = Path(cls._tmp.name) / "rt"
        cls.proc = provision(cls.root)
        if cls.proc.returncode != 0:
            raise AssertionError(
                f"provisioning failed: {cls.proc.returncode}\n"
                f"stdout={cls.proc.stdout[-2000:]}\nstderr={cls.proc.stderr[-2000:]}")

    @classmethod
    def tearDownClass(cls):
        cls._tmp.cleanup()

    def test_provisioning_succeeds(self):
        self.assertEqual(0, self.proc.returncode, self.proc.stderr)

    def test_inspector_script_is_staged(self):
        self.assertTrue((self.root / INSPECTOR).is_file(),
                        "the inspector itself must be staged or the executable "
                        "upward walk has nothing to find")

    def test_render_script_is_staged(self):
        # main() checks RENDER_SCRIPT.exists() and returns 2 before doing
        # any work, which surfaces as "inspector failed: exit status 2".
        self.assertTrue(
            (self.root / "agent_installation/scripts/render_managed_blocks.py").is_file())

    def test_canonical_snippets_source_is_staged(self):
        # CANONICAL_SOURCE; without it main() exits 2 identically, so this
        # and the render check are indistinguishable from the outside.
        self.assertTrue(
            (self.root / "agent_installation/MPM_AGENT_INTEGRATION_SNIPPETS.md").is_file())

    def test_every_adapter_reconcile_manifest_is_staged(self):
        # This is the fail-closed one. check_installed_managed_blocks.py:238
        # reads AGENT_INSTALLATION/<adapter>/reconcile.json and returns
        # False (ABSENT) when it is missing — a host that IS installed
        # would be reported as not installed.
        adapters = sorted(
            d.name for d in (REPO / "agent_installation").iterdir()
            if d.is_dir() and (d / "reconcile.json").is_file())
        self.assertTrue(adapters, "no adapters with reconcile.json in the repo")
        for adapter in adapters:
            staged = self.root / "agent_installation" / adapter / "reconcile.json"
            self.assertTrue(staged.is_file(),
                            f"{adapter}/reconcile.json not staged — host "
                            f"inspection fails closed to ABSENT without it")

    def test_every_adapter_template_is_staged(self):
        # Each reconcile.json references @/templates/<name>.snippet, which
        # render_managed_blocks.py resolves against the staged adapter dir.
        adapters = sorted(
            d.name for d in (REPO / "agent_installation").iterdir()
            if d.is_dir() and (d / "reconcile.json").is_file())
        found_any = False
        for adapter in adapters:
            src_dir = REPO / "agent_installation" / adapter / "templates"
            for tmpl in sorted(src_dir.glob("*.snippet")):
                found_any = True
                staged = (self.root / "agent_installation" / adapter
                          / "templates" / tmpl.name)
                self.assertTrue(staged.is_file(),
                                f"{adapter}/templates/{tmpl.name} not staged")
        self.assertTrue(found_any, "repo has no adapter templates at all")

    def test_section_builder_modules_are_staged(self):
        # SECTION_BUILDERS maps an adapter to a module imported BY FILE PATH
        # to build the expected block. Those modules import their own
        # siblings, so the whole per-adapter scripts/ dir must travel.
        builders = {"mpm-memory-openclaw": "install_openclaw_instructions"}
        for adapter, module in builders.items():
            staged = (self.root / "agent_installation" / adapter
                      / "scripts" / f"{module}.py")
            self.assertTrue(staged.is_file(),
                            f"{adapter} declares a section builder {module!r} "
                            f"but {staged.name} is not staged")


class TestHalfProvisionedInstallIsWorseThanNone(unittest.TestCase):
    """Pin the fail-closed behaviour that shapes what must be staged.

    Staging scripts/*.py WITHOUT the per-adapter reconcile.json manifests
    turns "inspection unavailable" (honest, actionable) into "no hosts
    installed" (confident, wrong). This test documents that consequence so
    a future change that stages only the scripts cannot look like progress.
    """

    def test_scripts_without_manifests_report_every_host_absent(self):
        with tempfile.TemporaryDirectory() as td:
            root = Path(td)
            for rel in ("agent_installation/scripts",
                        "agent_installation/MPM_AGENT_INTEGRATION_SNIPPETS.md"):
                src = REPO / rel
                dst = root / rel
                dst.parent.mkdir(parents=True, exist_ok=True)
                if src.is_dir():
                    dst.mkdir(parents=True, exist_ok=True)
                    for f in src.iterdir():
                        if f.is_file():
                            (dst / f.name).write_bytes(f.read_bytes())
                else:
                    dst.write_bytes(src.read_bytes())

            self.assertTrue((root / INSPECTOR).is_file(), "precondition")
            self.assertFalse(
                any((root / "agent_installation").glob("*/reconcile.json")),
                "precondition: no manifests staged")

            proc = subprocess.run(
                [sys.executable, str(root / INSPECTOR), "--json"],
                capture_output=True, text=True, cwd=str(root))
            # The inspector must not be able to assert ANY host is installed.
            out = proc.stdout.strip()
            if proc.returncode == 0 and out:
                hosts = json.loads(out)
                hosts = hosts.get("hosts", hosts) if isinstance(hosts, dict) else hosts
                for h in hosts:
                    self.assertNotEqual(
                        h.get("block"), "present",
                        "without reconcile.json the inspector must not claim "
                        "a managed block is present")


if __name__ == "__main__":
    unittest.main()
