"""Runtime-package provisioning contract for the OpenClaw adapters.

The source/runtime split means the source checkout (``~/src/mpm``) and the
runtime root (``~/.mpm``) are separate trees. Nothing in this repository
populates ``<runtime-root>/agent_installation/<plugin>/`` from Git, because
Git no longer owns that path — it used to, when ``~/.mpm`` *was* the
checkout.

The adapters therefore have to provision a runtime package themselves and
link THAT, rather than linking their own source directory. Two failure
shapes matter and both are pinned here:

  A. Post-split world: source checkout lives outside the runtime root and
     the runtime tree was never provisioned. A normal install must produce
     a complete package, and that package must keep working when the
     source checkout disappears.

  B. The exact broken state observed on the live host: the runtime plugin
     directory exists but holds only empty directories and ``__pycache__``
     residue, with ``index.js`` / ``package.json`` / ``openclaw.plugin.json``
     all absent. It looks installed and loads nothing. A normal reinstall
     must repair it.

Every test runs against a temporary runtime prefix. The real ``~/.mpm``,
the real OpenClaw config, and the production database are never touched.
"""

from __future__ import annotations

import json
import os
import re
import shutil
import subprocess
import tempfile
import unittest
from pathlib import Path

REPO_ROOT = Path(__file__).resolve().parents[2]
AGENT_DIR = REPO_ROOT / "agent_installation"
STAGE_LIB = AGENT_DIR / "scripts" / "stage_runtime_package.sh"

# The two OpenClaw adapters, with the runtime manifest each one stages.
OPENCLAW_PLUGINS: dict[str, tuple[Path, list[str]]] = {
    "mpm-memory-openclaw": (
        AGENT_DIR / "mpm-memory-openclaw",
        [
            "index.js",
            "openclaw.plugin.json",
            "package.json",
            "README.md",
            ".mcp.json",
            "lib/workspace.js",
            "lib/mcp-client.js",
            "lib/memory-transport.js",
        ],
    ),
    "mpm-auto-mode-persona-openclaw": (
        AGENT_DIR / "mpm-auto-mode-persona-openclaw",
        [
            "index.js",
            "openclaw.plugin.json",
            "package.json",
            "README.md",
            "lib/workspace.js",
        ],
    ),
}

# Never shipped to the runtime root, whatever else changes.
DEV_RESIDUE = ("tests", "__pycache__", "install.sh", "reconcile.json")


def run_stage(src: Path, runtime_root: Path, plugin_id: str,
              manifest: list[str]) -> subprocess.CompletedProcess:
    """Drive the staging library directly, with a hermetic HOME."""
    home = runtime_root / "home"
    home.mkdir(parents=True, exist_ok=True)
    cmd = [
        "bash", "-c",
        'set -euo pipefail; . "$1"; stage_runtime_package "$2" "$3" "$4" "${@:5}"',
        "bash", str(STAGE_LIB), str(src), str(runtime_root), plugin_id, *manifest,
    ]
    env = dict(os.environ)
    env["HOME"] = str(home)
    return subprocess.run(cmd, capture_output=True, text=True, env=env)


def staging_stdout(res: subprocess.CompletedProcess) -> str:
    return res.stdout.strip().splitlines()[-1].strip() if res.stdout.strip() else ""


class StagingHarness(unittest.TestCase):
    def setUp(self) -> None:
        self.tmp = Path(tempfile.mkdtemp(prefix="mpm-runtime-pkg-"))
        self.addCleanup(shutil.rmtree, self.tmp, ignore_errors=True)
        self.runtime = self.tmp / "rt"

    def stage(self, plugin_id: str) -> subprocess.CompletedProcess:
        src, manifest = OPENCLAW_PLUGINS[plugin_id]
        return run_stage(src, self.runtime, plugin_id, manifest)


class TestManifestMatchesInstaller(unittest.TestCase):
    """The fixture manifest must not drift from what the installer stages.

    OPENCLAW_PLUGINS above is a hand-copied mirror of each plugin's
    RUNTIME_PACKAGE_MANIFEST in its install.sh. Nothing enforced that,
    so adding a runtime file to the installer alone left the real
    installer correct and every staging test failing on an unresolved
    import — the JS suite stayed green and only the Go gate caught it.
    """

    def test_fixture_manifest_matches_installer_manifest(self) -> None:
        for plugin_id, (src, manifest) in OPENCLAW_PLUGINS.items():
            with self.subTest(plugin=plugin_id):
                install_sh = (src / "install.sh").read_text(encoding="utf-8")
                block = re.search(
                    r"RUNTIME_PACKAGE_MANIFEST=\(\s*(.*?)\n\)", install_sh, re.S
                )
                self.assertIsNotNone(
                    block, f"{plugin_id}/install.sh has no RUNTIME_PACKAGE_MANIFEST"
                )
                declared = [
                    line.strip()
                    for line in block.group(1).splitlines()
                    if line.strip()
                ]
                self.assertEqual(
                    sorted(declared),
                    sorted(manifest),
                    f"{plugin_id}: test manifest is out of sync with install.sh",
                )


class TestRuntimeRootDerivation(StagingHarness):
    """The runtime root must come from the resolved binary, not $HOME/.mpm.

    Hardcoding ~/.mpm would make a relocated or hermetic install stage into
    the operator's real runtime — the exact class of bug this tranche exists
    to remove.
    """

    def _root_from(self, bin_path: str) -> str:
        cmd = [
            "bash", "-c",
            'set -euo pipefail; . "$1"; mpm_runtime_root_from_bin "$2"',
            "bash", str(STAGE_LIB), bin_path,
        ]
        return subprocess.run(
            cmd, capture_output=True, text=True,
            env={**os.environ, "HOME": str(self.tmp)},
        ).stdout.strip()

    def test_prefix_from_bin_layout(self) -> None:
        self.assertEqual(self._root_from("/opt/elsewhere/bin/mpm"), "/opt/elsewhere")

    def test_nested_prefix(self) -> None:
        self.assertEqual(self._root_from("/srv/mpm/inst/bin/mpm"), "/srv/mpm/inst")

    def test_symlinked_binary_resolves_to_real_root(self) -> None:
        real = self.tmp / "real" / "bin" / "mpm"
        real.parent.mkdir(parents=True)
        real.write_text("#!/bin/sh\n")
        real.chmod(0o755)
        link = self.tmp / "fakehome" / ".local" / "bin" / "mpm"
        link.parent.mkdir(parents=True)
        link.symlink_to(real)
        self.assertEqual(self._root_from(str(link)), str(self.tmp / "real"))

    def test_missing_binary_is_an_error_not_the_filesystem_root(self) -> None:
        out = self._root_from("")
        self.assertNotEqual(out, "/")
        self.assertEqual(out, "")


class TestPostSplitWorld(StagingHarness):
    """Regression A: source outside the runtime root, runtime unprovisioned."""

    def test_fresh_install_provisions_complete_package(self) -> None:
        for plugin_id in OPENCLAW_PLUGINS:
            with self.subTest(plugin=plugin_id):
                res = self.stage(plugin_id)
                self.assertEqual(res.returncode, 0, res.stderr)
                pkg = Path(staging_stdout(res))
                self.assertEqual(
                    pkg, self.runtime / "agent_installation" / plugin_id
                )
                self.assertTrue(pkg.is_dir())

                manifest = json.loads((pkg / "openclaw.plugin.json").read_text())
                self.assertEqual(manifest["id"], plugin_id)
                self.assertEqual(manifest["id"], plugin_id)

                pkgjson = json.loads((pkg / "package.json").read_text())
                self.assertTrue((pkg / pkgjson["main"]).is_file())

    def test_source_checkout_can_disappear_after_install(self) -> None:
        """The linked package must not depend on the checkout existing.

        This is the whole point of the split. If anything in the staged
        package reaches back into the source tree, deleting the checkout
        breaks the live integration.
        """
        src, manifest = OPENCLAW_PLUGINS["mpm-memory-openclaw"]
        copied = self.tmp / "src-copy"
        shutil.copytree(src, copied)
        res = run_stage(copied, self.runtime, "mpm-memory-openclaw", manifest)
        self.assertEqual(res.returncode, 0, res.stderr)
        pkg = Path(staging_stdout(res))

        shutil.rmtree(copied)
        self.assertFalse(copied.exists())

        for rel in manifest:
            self.assertTrue((pkg / rel).is_file(), f"{rel} missing after source removal")
        # No staged file may name the source tree it came from.
        for path in pkg.rglob("*"):
            if path.is_file() and path.suffix in {".js", ".json", ".md"}:
                text = path.read_text(encoding="utf-8", errors="replace")
                self.assertNotIn(
                    str(src), text,
                    f"{path} references the source checkout",
                )

    def test_no_development_residue_ships(self) -> None:
        for plugin_id in OPENCLAW_PLUGINS:
            with self.subTest(plugin=plugin_id):
                res = self.stage(plugin_id)
                pkg = Path(staging_stdout(res))
                names = {p.name for p in pkg.rglob("*")}
                for residue in ("tests", "__pycache__", "install.sh", "reconcile.json"):
                    self.assertNotIn(residue, names)
                self.assertEqual(
                    [p for p in pkg.rglob("*.pyc")], [],
                    "bytecode cache shipped to the runtime package",
                )

    def test_internal_imports_resolve_inside_the_package(self) -> None:
        for plugin_id in OPENCLAW_PLUGINS:
            with self.subTest(plugin=plugin_id):
                res = self.stage(plugin_id)
                pkg = Path(staging_stdout(res))
                entry = pkg / "index.js"
                self.assertIn('from "./lib/workspace.js"', entry.read_text())
                self.assertTrue((pkg / "lib" / "workspace.js").is_file())


class TestRepairsBrokenRuntimeState(StagingHarness):
    """Regression B: the exact incomplete-directory state observed live."""

    def _make_broken(self, plugin_id: str) -> Path:
        """Recreate the live signature: directories and caches only."""
        pkg = self.runtime / "agent_installation" / plugin_id
        (pkg / "lib").mkdir(parents=True)
        (pkg / "scripts" / "__pycache__").mkdir(parents=True)
        (pkg / "templates").mkdir(parents=True)
        (pkg / "tests" / "__pycache__").mkdir(parents=True)
        (pkg / "scripts" / "__pycache__" / "install_openclaw_instructions.cpython-314.pyc").write_text("")
        (pkg / "tests" / "__pycache__" / "test_install_openclaw_instructions.cpython-314.pyc").write_text("")
        return pkg

    def test_reinstall_repairs_the_exact_broken_state(self) -> None:
        for plugin_id in OPENCLAW_PLUGINS:
            with self.subTest(plugin=plugin_id):
                pkg = self._make_broken(plugin_id)
                # Precondition: broken, and visibly so.
                for missing in ("index.js", "package.json", "openclaw.plugin.json"):
                    self.assertFalse((pkg / missing).exists())

                res = self.stage(plugin_id)
                self.assertEqual(res.returncode, 0, res.stderr)

                for required in ("index.js", "package.json", "openclaw.plugin.json"):
                    self.assertTrue((pkg / required).is_file(), required)
                data = json.loads((pkg / "openclaw.plugin.json").read_text())
                self.assertEqual(data["id"], plugin_id)
                self.assertEqual(
                    [p for p in pkg.rglob("*.pyc")], [],
                    "stale bytecode survived the repair",
                )

    def test_repair_sweeps_empty_directories_of_residue(self) -> None:
        pkg = self._make_broken("mpm-memory-openclaw")
        self.stage("mpm-memory-openclaw")
        self.assertFalse((pkg / "scripts" / "__pycache__").exists())
        self.assertFalse((pkg / "tests" / "__pycache__").exists())

    def test_operator_file_is_preserved_across_reprovisioning(self) -> None:
        """Ownership: MPM replaces its own files and nothing else.

        An operator who dropped a local override into the runtime package
        must not have it silently deleted by the next install.
        """
        pkg = self._make_broken("mpm-memory-openclaw")
        mine = pkg / "OPERATOR-NOTES.md"
        mine.write_text("operator-owned\n")

        self.stage("mpm-memory-openclaw")

        self.assertTrue(mine.is_file(), "operator file was deleted by the installer")
        self.assertEqual(mine.read_text(), "operator-owned\n")

    def test_repeated_install_is_idempotent(self) -> None:
        first = self.stage("mpm-memory-openclaw")
        pkg = Path(staging_stdout(first))
        snap = {p.relative_to(pkg): p.read_bytes()
                for p in pkg.rglob("*") if p.is_file()}

        second = self.stage("mpm-memory-openclaw")
        self.assertEqual(staging_stdout(second), str(pkg))
        again = {p.relative_to(pkg): p.read_bytes()
                 for p in pkg.rglob("*") if p.is_file()}
        self.assertEqual(snap, again)

    def test_converges_when_source_changes(self) -> None:
        src, manifest = OPENCLAW_PLUGINS["mpm-memory-openclaw"]
        res = self.stage("mpm-memory-openclaw")
        pkg = Path(staging_stdout(res))
        self.assertNotIn("STAGED-MARKER", (pkg / "index.js").read_text())

        mutated = self.tmp / "mutated"
        shutil.copytree(src, mutated)
        target = mutated / "index.js"
        target.write_text(target.read_text() + "\n// STAGED-MARKER\n")

        run_stage(mutated, self.runtime, "mpm-memory-openclaw", manifest)
        self.assertIn("STAGED-MARKER", (pkg / "index.js").read_text())


class TestStagingFailsClosed(StagingHarness):
    """A package that cannot be validated must never be exposed."""

    def test_missing_manifest_entry_in_source_aborts(self) -> None:
        src, manifest = OPENCLAW_PLUGINS["mpm-memory-openclaw"]
        res = run_stage(src, self.runtime, "mpm-memory-openclaw",
                        manifest + ["lib/does_not_exist.js"])
        self.assertNotEqual(res.returncode, 0)
        self.assertIn("manifest entry not found in source", res.stderr)
        self.assertFalse((self.runtime / "agent_installation").exists())

    def test_id_mismatch_aborts(self) -> None:
        src, manifest = OPENCLAW_PLUGINS["mpm-memory-openclaw"]
        res = run_stage(src, self.runtime, "some-other-plugin", manifest)
        self.assertNotEqual(res.returncode, 0)
        self.assertIn("plugin id mismatch", res.stderr)

    def test_unresolved_internal_import_aborts(self) -> None:
        src, manifest = OPENCLAW_PLUGINS["mpm-memory-openclaw"]
        without_lib = [m for m in manifest if not m.startswith("lib/")]
        res = run_stage(src, self.runtime, "mpm-memory-openclaw", without_lib)
        self.assertNotEqual(res.returncode, 0)
        self.assertIn("unresolved internal imports", res.stderr)
        self.assertIn("./lib/workspace.js", res.stderr)

    def test_failed_restage_leaves_previous_package_intact(self) -> None:
        """The swap is all-or-nothing; a bad run must not destroy a good install."""
        src, manifest = OPENCLAW_PLUGINS["mpm-memory-openclaw"]
        good = self.stage("mpm-memory-openclaw")
        pkg = Path(staging_stdout(good))
        before = {p.relative_to(pkg): p.read_bytes()
                  for p in pkg.rglob("*") if p.is_file()}

        broken_src = self.tmp / "broken-src"
        shutil.copytree(src, broken_src)
        (broken_src / "index.js").unlink()  # manifest entry vanishes

        bad = run_stage(broken_src, self.runtime, "mpm-memory-openclaw", manifest)
        self.assertNotEqual(bad.returncode, 0)

        self.assertTrue((pkg / "index.js").is_file())
        after = {p.relative_to(pkg): p.read_bytes()
                 for p in pkg.rglob("*") if p.is_file()}
        self.assertEqual(before, after, "a failed restage mutated the live package")

    def test_empty_manifest_aborts(self) -> None:
        src, _ = OPENCLAW_PLUGINS["mpm-memory-openclaw"]
        res = run_stage(src, self.runtime, "mpm-memory-openclaw", [])
        self.assertNotEqual(res.returncode, 0)


class TestMissingStagingLibraryIsReportable(unittest.TestCase):
    """A missing sibling scripts/ directory must be diagnosed, not silent."""

    def setUp(self) -> None:
        self.tmp = Path(tempfile.mkdtemp(prefix="mpm-detached-"))
        self.addCleanup(shutil.rmtree, self.tmp, ignore_errors=True)

    def test_detached_adapter_names_the_missing_library(self) -> None:
        """Under `set -e`, a failing command substitution inside an
        assignment aborts the script before the surrounding guard can
        print anything. The installer therefore exited 1 with an empty
        log and no clue what was wrong. Pin the reportability, not just
        the exit code.
        """
        for plugin_id, (src, _) in OPENCLAW_PLUGINS.items():
            with self.subTest(plugin=plugin_id):
                detached = self.tmp / f"detached-{plugin_id}"
                detached.mkdir()
                shutil.copytree(src, detached / plugin_id)

                home = self.tmp / f"home-detached-{plugin_id}"
                home.mkdir(parents=True, exist_ok=True)
                rt = self.tmp / f"rt-detached-{plugin_id}"
                (rt / "bin").mkdir(parents=True)
                stub = rt / "bin" / "mpm"
                stub.write_text('#!/bin/sh\n[ "$1" = "--version" ] && exit 0\nexit 0\n')
                stub.chmod(0o755)

                env = {
                    **os.environ,
                    "HOME": str(home),
                    "PATH": f"{rt / 'bin'}:{os.environ.get('PATH', '')}",
                }
                res = subprocess.run(
                    ["bash", str(detached / plugin_id / "install.sh")],
                    capture_output=True, text=True, env=env, cwd=str(self.tmp),
                )
                self.assertNotEqual(res.returncode, 0)
                self.assertIn(
                    "staging library not found", res.stderr,
                    "a missing provisioning library must be named, not silent",
                )


class TestAdaptersLinkTheRuntimePackage(unittest.TestCase):
    """Static contract: the adapters must link the runtime path, not source.

    Guards against a future edit reintroducing `plugins install "$SCRIPT_DIR"`
    or the persona adapter's original bare `plugins install .`, either of
    which re-couples the live integration to the checkout or to the
    caller's working directory.
    """

    def test_memory_adapter_stages_and_links_runtime_path(self) -> None:
        text = (AGENT_DIR / "mpm-memory-openclaw" / "install.sh").read_text()
        self.assertIn("stage_runtime_package", text)
        self.assertIn(
            'openclaw plugins install "$PLUGIN_PKG_DIR"', text,
            "memory adapter must link the provisioned runtime package",
        )
        self.assertNotIn(
            'openclaw plugins install "$SCRIPT_DIR"', text,
            "memory adapter must not link the source checkout",
        )

    def test_persona_adapter_stages_and_links_runtime_path(self) -> None:
        text = (AGENT_DIR / "mpm-auto-mode-persona-openclaw" / "install.sh").read_text()
        self.assertIn("stage_runtime_package", text)
        self.assertIn(
            'openclaw plugins install "$PLUGIN_PKG_DIR"', text,
            "persona adapter must link the provisioned runtime package",
        )
        # Only executable lines count. The file legitimately *mentions*
        # the old bare-dot form in a comment explaining why it was
        # replaced; what must not come back is a live invocation.
        code = "\n".join(
            line for line in text.splitlines()
            if not line.lstrip().startswith("#")
        )
        self.assertNotIn(
            "openclaw plugins install .",
            code,
            "persona adapter must not link the caller's CWD",
        )

    def test_adapters_source_the_shared_staging_library(self) -> None:
        for plugin_id, (src, _) in OPENCLAW_PLUGINS.items():
            with self.subTest(plugin=plugin_id):
                text = (src / "install.sh").read_text()
                self.assertIn("stage_runtime_package.sh", text)

    def test_legacy_id_ownership_accepts_our_runtime_package(self) -> None:
        """A legacy-id record at OUR path is ours, not a foreign plugin.

        The conflict arm previously compared only against the legacy
        former location. A record pointing at the runtime package we
        provision today — our own record from a prior run — was
        therefore misread as an unrelated plugin, and the installer
        refused to proceed on its own idempotent re-run.
        """
        text = (AGENT_DIR / "mpm-memory-openclaw" / "install.sh").read_text()
        self.assertIn(
            'paths_match "$legacy_inspect_root" "$OUR_REAL"', text,
            "legacy-id ownership must also accept our own runtime package path",
        )
        self.assertIn(
            'paths_match "$rp" "$OUR_REAL"', text,
            "registry-path ownership must also accept our own runtime package",
        )
        # ...and the conflict protection must survive that widening.
        self.assertIn('legacy_ownership="conflict"', text)

    def test_staging_library_is_shipped_in_the_checkout(self) -> None:
        """The library must exist in source or no adapter can provision."""
        self.assertTrue(STAGE_LIB.is_file())
        import subprocess as sp
        tracked = sp.run(
            ["git", "ls-files", "agent_installation/scripts/stage_runtime_package.sh"],
            cwd=REPO_ROOT, capture_output=True, text=True,
        ).stdout.strip()
        self.assertTrue(
            tracked, "stage_runtime_package.sh exists but is not tracked by git"
        )


if __name__ == "__main__":
    unittest.main()