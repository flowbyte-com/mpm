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


class TestAgentProtocolIsReachable(unittest.TestCase):
    """The managed block advertises a runtime path for the full
    behavioural protocol. That path must resolve on an installed
    machine, or the block is pointing at nothing.

    This was a live defect: every rendered host block carried the header

        <!-- Full behavioural protocol:
             ~/.mpm/agent_installation/mpm-agent-protocol.md -->

    but no staging manifest ever listed the file, so the advertised
    path did not exist on any installed machine. An agent that followed
    the pointer it was explicitly invited to follow found nothing.
    """

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

    PROTOCOL_REL = "agent_installation/mpm-agent-protocol.md"

    def test_protocol_is_staged_at_the_advertised_path(self):
        staged = self.root / self.PROTOCOL_REL
        self.assertTrue(
            staged.is_file(),
            f"{self.PROTOCOL_REL} was not staged; every managed block "
            f"advertises this path, so it must resolve after install")

    def test_staged_protocol_is_the_repo_canonical_file(self):
        """Not a second copy. The staged bytes must be the repo file's
        bytes, so there is exactly one canonical protocol."""
        staged = (self.root / self.PROTOCOL_REL).read_bytes()
        source = (REPO / self.PROTOCOL_REL).read_bytes()
        self.assertEqual(
            source, staged,
            "the staged protocol must be the canonical repo file, not a "
            "divergent copy")

    def test_every_rendered_host_block_advertises_a_path_that_is_staged(self):
        """The header a host actually loads must name a file this
        provisioning pass placed. Guards the pairing: staging the file
        without the block pointing at it, or the reverse, both leave the
        contract broken."""
        rendered = []
        for adapter in sorted(
            d for d in (REPO / "agent_installation").iterdir()
            if d.is_dir() and (d / "templates").is_dir()
        ):
            for snippet in sorted((adapter / "templates").glob("*.snippet")):
                rendered.append((adapter.name, snippet.name,
                                 snippet.read_text(encoding="utf-8")))

        self.assertTrue(rendered, "no rendered host snippets found")
        for adapter, name, text in rendered:
            self.assertIn(
                "mpm-agent-protocol.md", text,
                f"{adapter}/{name} does not reference the protocol at all")
            for line in text.splitlines():
                if "Full behavioural protocol:" in line:
                    path = line.split("protocol:", 1)[1].strip().rstrip("-->").strip()
                    rel = path.replace("~/.mpm/", "").lstrip("/")
                    self.assertTrue(
                        (self.root / rel).is_file(),
                        f"{adapter}/{name} advertises {path!r}, which this "
                        f"provisioning pass did not create")

    def test_repeated_provisioning_is_idempotent(self):
        """Re-running the provisioner must not rewrite or duplicate the
        staged protocol — a second pass over a settled runtime root is a
        no-op."""
        sys.path.insert(0, str(REPO / "scripts"))
        try:
            import importlib.util
            spec = importlib.util.spec_from_file_location(
                "install_runtime_assets_probe", SCRIPT)
            module = importlib.util.module_from_spec(spec)
            spec.loader.exec_module(module)

            result = module.reconcile(REPO, self.root)
            changed = result.changed()
            self.assertEqual(
                [], changed,
                f"a second reconcile over a settled runtime root changed "
                f"{changed}; provisioning must be idempotent")
        finally:
            sys.path.pop(0)

    def test_provisioning_preserves_unrelated_operator_content(self):
        """Staging is additive. An operator file the provisioner does
        not own must survive a pass untouched."""
        owned = self.root / self.PROTOCOL_REL
        self.assertTrue(owned.is_file(), "precondition: provision ran")

        unrelated = self.root / "mode" / "my-custom-mode.md"
        unrelated.parent.mkdir(parents=True, exist_ok=True)
        unrelated.write_text("# my own mode, not source-managed\n",
                             encoding="utf-8")
        marker = self.root / "agent_installation" / "my-notes.md"
        marker.write_text("operator scratch\n", encoding="utf-8")

        proc = provision(self.root)
        self.assertEqual(0, proc.returncode, proc.stderr)

        self.assertEqual(
            "# my own mode, not source-managed\n",
            unrelated.read_text(encoding="utf-8"),
            "an operator-owned file under a source-managed directory must "
            "not be clobbered by provisioning")
        self.assertEqual(
            "operator scratch\n", marker.read_text(encoding="utf-8"),
            "provisioning must not remove files it does not manage")


if __name__ == "__main__":
    unittest.main()
