"""Runtime-package provisioning contract for the shipped adapter.

The source/runtime split means the source checkout (``~/src/mpm``) and the
runtime root (``~/.mpm``) are separate trees. Nothing in this repository
populates ``<runtime-root>/agent_installation/<plugin>/`` from Git, because
Git no longer owns that path — it used to, when ``~/.mpm`` *was* the
checkout.

The adapter therefore has to provision a runtime package itself and link
THAT, rather than linking its own source directory. Two failure shapes
matter and both are pinned here:

  A. Post-split world: source checkout lives outside the runtime root and
     the runtime tree was never provisioned. A normal install must produce
     a complete package, and that package must keep working when the
     source checkout disappears.

  B. The exact broken state observed on the live host: the runtime plugin
     directory exists but holds only empty directories and ``__pycache__``
     residue, with the payload entries all absent. It looks installed and
     loads nothing. A normal reinstall must repair it.

Every test runs against a temporary runtime prefix. The real ``~/.mpm``,
the real host configuration, and the production database are never touched.

THE SOURCE TREE IS SYNTHESISED, NOT THE REAL ONE
------------------------------------------------

The shipped adapter's ``dist/`` is a build artefact (``npm ci`` +
``tsc``) and is gitignored, so a CI job that has not built the adapter
cannot stage the real tree. The property under test here is the
*library's* behaviour given a manifest and a source tree — not the
adapter's build output — so the source tree is synthesized with exactly
the shape the manifest declares. The manifest itself is checked against
the real ``install.sh`` in ``TestManifestMatchesInstaller``, so the
synthetic tree can never drift away from what the installer stages.

After the 2026-10-27 OpenCode-only adapter removal this suite covers
OpenCode alone. The OpenClaw and Pi adapters took their own scenarios
with them; the shared library and the OpenCode payload contract stayed.
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

# The plugin id of the shipped adapter.
PLUGIN_ID = "mpm-opencode"

# The OpenCode runtime manifest — a mirror of install.sh's
# OPENCODE_RUNTIME_MANIFEST. TestManifestMatchesInstaller fails if the
# two ever drift.
OPENCODE_MANIFEST = [
    "server.js",
    "dist/index.v2.js",
    "dist/wake-context.js",
    "dist/event-envelope.js",
    "dist/event-mapper.js",
    "dist/event-subscriber.js",
    "dist/workspace.js",
    "dist/resolve-mpm-binary.js",
    "package.json",
]

# The validator the OpenCode installer selects. The library's DEFAULT
# validator is the historical OpenClaw one; the OpenCode installer
# overrides it with this hook.
OPENCODE_VALIDATOR = "mpm_validate_runtime_package_opencode"

# Never shipped to the runtime root, whatever else changes.
DEV_RESIDUE = ("tests", "__pycache__", "install.sh", "reconcile.json")


def opencode_source_tree(root: Path) -> Path:
    """A synthetic OpenCode-shaped source tree under `root`.

    Mirrors the shape the runtime manifest declares: a root-level
    ``server.js`` shim that re-exports the compiled payload, the
    ``dist/`` payload modules, and a ``package.json`` whose ``main``
    names the payload.
    """
    src = root / PLUGIN_ID
    (src / "dist").mkdir(parents=True)
    (src / "server.js").write_text(
        'export { default } from "./dist/index.v2.js";\n', encoding="utf-8"
    )
    (src / "dist" / "index.v2.js").write_text(
        'import { w } from "./workspace.js";\n'
        'import { r } from "./resolve-mpm-binary.js";\n'
        "export default { w, r };\n",
        encoding="utf-8",
    )
    for name in (
        "wake-context",
        "event-envelope",
        "event-mapper",
        "event-subscriber",
        "workspace",
        "resolve-mpm-binary",
    ):
        (src / "dist" / f"{name}.js").write_text(
            "export const x = () => {};\n", encoding="utf-8"
        )
    (src / "package.json").write_text(json.dumps({
        "name": PLUGIN_ID,
        "type": "module",
        "main": "./dist/index.v2.js",
    }), encoding="utf-8")
    # Development surface the manifest must NOT carry.
    (src / "tests").mkdir()
    (src / "install.sh").write_text("#!/bin/sh\nexit 0\n", encoding="utf-8")
    (src / "reconcile.json").write_text("{}\n", encoding="utf-8")
    return src


def run_stage(src: Path, runtime_root: Path, plugin_id: str,
              manifest: list[str], validator: str | None = None) -> subprocess.CompletedProcess:
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
    if validator is not None:
        env["MPM_RUNTIME_PACKAGE_VALIDATOR"] = validator
    return subprocess.run(cmd, capture_output=True, text=True, env=env)


def staging_stdout(res: subprocess.CompletedProcess) -> str:
    return res.stdout.strip().splitlines()[-1].strip() if res.stdout.strip() else ""


class StagingHarness(unittest.TestCase):
    def setUp(self) -> None:
        self.tmp = Path(tempfile.mkdtemp(prefix="mpm-runtime-pkg-"))
        self.addCleanup(shutil.rmtree, self.tmp, ignore_errors=True)
        self.runtime = self.tmp / "rt"
        self.src = opencode_source_tree(self.tmp)

    def stage(self, plugin_id: str = PLUGIN_ID,
              manifest: list[str] | None = None,
              validator: str = OPENCODE_VALIDATOR) -> subprocess.CompletedProcess:
        return run_stage(
            self.src, self.runtime, plugin_id,
            OPENCODE_MANIFEST if manifest is None else manifest,
            validator,
        )


class TestManifestMatchesInstaller(unittest.TestCase):
    """The fixture manifest must not drift from what the installer stages.

    OPENCODE_MANIFEST above is a hand-copied mirror of the installer's
    OPENCODE_RUNTIME_MANIFEST. Nothing enforced that, so adding a
    runtime file to the installer alone would leave the real installer
    correct and every staging test failing on an unresolved import —
    the gate stayed green and only a later run caught it.
    """

    def test_fixture_manifest_matches_installer_manifest(self) -> None:
        install_sh = (AGENT_DIR / PLUGIN_ID / "install.sh").read_text(encoding="utf-8")
        block = re.search(
            r"OPENCODE_RUNTIME_MANIFEST=\(\s*(.*?)\n\)", install_sh, re.S
        )
        self.assertIsNotNone(
            block, f"{PLUGIN_ID}/install.sh has no OPENCODE_RUNTIME_MANIFEST"
        )
        declared = [
            line.strip()
            for line in block.group(1).splitlines()
            if line.strip()
        ]
        self.assertEqual(
            sorted(declared),
            sorted(OPENCODE_MANIFEST),
            "the test manifest is out of sync with install.sh",
        )


class TestRuntimeRootDerivation(StagingHarness):
    """The runtime root must come from the resolved binary, not $HOME/.mpm.

    Hardcoding ~/.mpm would make a relocated or hermetic install stage into
    the operator's real runtime — the exact class of bug this tranche exists
    to remove.
    """

    def _root_from(self, bin_path: str) -> str:
        return self._call("mpm_runtime_root_from_bin", bin_path)

    def _bundle_from(self, bin_path: str) -> str:
        return self._call("mpm_bundle_root_from_bin", bin_path)

    def _data_from(self, bin_path: str) -> str:
        return self._call("mpm_data_root_from_bin", bin_path)

    def _call(self, fn: str, bin_path: str) -> str:
        cmd = [
            "bash", "-c",
            f'set -euo pipefail; . "$1"; {fn} "$2"',
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


class TestBundleAndDataRoots(StagingHarness):
    """The BUNDLE root and the DATA root are different things.

    The versioned-bundle layout puts a release at
    <data-root>/releases/<rev>/, so the binary resolves to
    <data-root>/releases/<rev>/bin/mpm. The release-scoped assets belong
    in the BUNDLE and roll back with the release; the persistent state
    belongs in the DATA ROOT and outlives every release.

    Conflating them is how an operator's definition ends up replaced on
    every version switch, or how a release-scoped asset survives one.
    """

    def _call(self, fn: str, bin_path: str) -> str:
        cmd = [
            "bash", "-c",
            f'set -euo pipefail; . "$1"; {fn} "$2"',
            "bash", str(STAGE_LIB), bin_path,
        ]
        return subprocess.run(
            cmd, capture_output=True, text=True,
            env={**os.environ, "HOME": str(self.tmp)},
        ).stdout.strip()

    def test_legacy_layout_the_two_roots_are_the_same(self) -> None:
        # <data-root>/bin/mpm — the pre-bundle layout. Nothing changes for
        # an existing install.
        self.assertEqual(self._call("mpm_bundle_root_from_bin", "/opt/elsewhere/bin/mpm"),
                         "/opt/elsewhere")
        self.assertEqual(self._call("mpm_data_root_from_bin", "/opt/elsewhere/bin/mpm"),
                         "/opt/elsewhere")

    def test_versioned_bundle_the_roots_differ(self) -> None:
        # <data-root>/releases/<rev>/bin/mpm
        self.assertEqual(
            self._call("mpm_bundle_root_from_bin", "/srv/mpm/releases/4111c46/bin/mpm"),
            "/srv/mpm/releases/4111c46")
        self.assertEqual(
            self._call("mpm_data_root_from_bin", "/srv/mpm/releases/4111c46/bin/mpm"),
            "/srv/mpm")

    def test_a_symlinked_binary_resolves_through_the_bundle(self) -> None:
        # ~/.mpm/bin is a symlink into current/, which is a symlink into
        # the active release. The resolved path is the BUNDLE, and the
        # data root is its grandparent.
        data = self.tmp / "mpm"
        release = data / "releases" / "4111c46" / "bin"
        release.mkdir(parents=True)
        real = release / "mpm"
        real.write_text("#!/bin/sh\n")
        real.chmod(0o755)
        link = self.tmp / "link" / "bin" / "mpm"
        link.parent.mkdir(parents=True)
        link.symlink_to(real)
        self.assertEqual(self._call("mpm_bundle_root_from_bin", str(link)),
                         str(data / "releases" / "4111c46"))
        self.assertEqual(self._call("mpm_data_root_from_bin", str(link)), str(data))

    def test_a_parent_named_releases_always_means_the_versioned_layout(self) -> None:
        # The discriminator is the parent's NAME, so a bundle root whose
        # parent is named "releases" is always treated as being in the
        # versioned layout, whatever else is true about it.
        #
        # That is deliberate: the name is the layout's own convention,
        # and a DATA ROOT is never named "releases" in any supported
        # layout. The alternative — looking for src/db or a releases/
        # directory — would make the resolver depend on the database
        # existing, and it would therefore fail exactly when it is first
        # needed, on a fresh install.
        self.assertEqual(
            self._call("mpm_data_root_from_bin", "/srv/releases/4111c46/bin/mpm"),
            "/srv")


class TestPostSplitWorld(StagingHarness):
    """Regression A: source outside the runtime root, runtime unprovisioned."""

    def test_fresh_install_provisions_complete_package(self) -> None:
        res = self.stage()
        self.assertEqual(res.returncode, 0, res.stderr)
        pkg = Path(staging_stdout(res))
        self.assertEqual(pkg, self.runtime / "agent_installation" / PLUGIN_ID)
        self.assertTrue(pkg.is_dir())

        for rel in OPENCODE_MANIFEST:
            self.assertTrue((pkg / rel).is_file(), f"{rel} missing from the staged package")

        pkgjson = json.loads((pkg / "package.json").read_text())
        self.assertTrue((pkg / pkgjson["main"]).is_file())

    def test_source_checkout_can_disappear_after_install(self) -> None:
        """The linked package must not depend on the checkout existing.

        This is the whole point of the split. If anything in the staged
        package reaches back into the source tree, deleting the checkout
        breaks the live integration.
        """
        copied = self.tmp / "src-copy"
        shutil.copytree(self.src, copied)
        res = run_stage(copied, self.runtime, PLUGIN_ID, OPENCODE_MANIFEST,
                        OPENCODE_VALIDATOR)
        self.assertEqual(res.returncode, 0, res.stderr)
        pkg = Path(staging_stdout(res))

        shutil.rmtree(copied)
        self.assertFalse(copied.exists())

        for rel in OPENCODE_MANIFEST:
            self.assertTrue((pkg / rel).is_file(), f"{rel} missing after source removal")
        # No staged file may name the source tree it came from.
        for path in pkg.rglob("*"):
            if path.is_file() and path.suffix in {".js", ".json", ".md"}:
                text = path.read_text(encoding="utf-8", errors="replace")
                self.assertNotIn(
                    str(self.src), text,
                    f"{path} references the source checkout",
                )

    def test_no_development_residue_ships(self) -> None:
        res = self.stage()
        pkg = Path(staging_stdout(res))
        names = {p.name for p in pkg.rglob("*")}
        for residue in DEV_RESIDUE:
            self.assertNotIn(residue, names)
        self.assertEqual(
            [p for p in pkg.rglob("*.pyc")], [],
            "bytecode cache shipped to the runtime package",
        )

    def test_internal_imports_resolve_inside_the_package(self) -> None:
        res = self.stage()
        pkg = Path(staging_stdout(res))
        entry = pkg / "dist" / "index.v2.js"
        self.assertIn('from "./workspace.js"', entry.read_text())
        self.assertTrue((pkg / "dist" / "workspace.js").is_file())


class TestRepairsBrokenRuntimeState(StagingHarness):
    """Regression B: the exact incomplete-directory state observed live."""

    def _make_broken(self, plugin_id: str = PLUGIN_ID) -> Path:
        """Recreate the live signature: directories and caches only."""
        pkg = self.runtime / "agent_installation" / plugin_id
        (pkg / "dist").mkdir(parents=True)
        (pkg / "scripts" / "__pycache__").mkdir(parents=True)
        (pkg / "tests" / "__pycache__").mkdir(parents=True)
        (pkg / "scripts" / "__pycache__" / "stale.cpython-314.pyc").write_text("")
        (pkg / "tests" / "__pycache__" / "stale.cpython-314.pyc").write_text("")
        return pkg

    def test_reinstall_repairs_the_exact_broken_state(self) -> None:
        pkg = self._make_broken()
        # Precondition: broken, and visibly so.
        for missing in ("server.js", "dist/index.v2.js", "package.json"):
            self.assertFalse((pkg / missing).exists())

        res = self.stage()
        self.assertEqual(res.returncode, 0, res.stderr)

        for required in ("server.js", "dist/index.v2.js", "package.json"):
            self.assertTrue((pkg / required).is_file(), required)
        self.assertEqual(
            [p for p in pkg.rglob("*.pyc")], [],
            "stale bytecode survived the repair",
        )

    def test_repair_sweeps_empty_directories_of_residue(self) -> None:
        self._make_broken()
        self.stage()
        self.assertFalse((self.runtime / "agent_installation" / PLUGIN_ID
                          / "scripts" / "__pycache__").exists())
        self.assertFalse((self.runtime / "agent_installation" / PLUGIN_ID
                          / "tests" / "__pycache__").exists())

    def test_operator_file_is_preserved_across_reprovisioning(self) -> None:
        """Ownership: MPM replaces its own files and nothing else.

        An operator who dropped a local override into the runtime package
        must not have it silently deleted by the next install.
        """
        pkg = self._make_broken()
        mine = pkg / "OPERATOR-NOTES.md"
        mine.write_text("operator-owned\n")

        self.stage()

        self.assertTrue(mine.is_file(), "operator file was deleted by the installer")
        self.assertEqual(mine.read_text(), "operator-owned\n")

    def test_repeated_install_is_idempotent(self) -> None:
        first = self.stage()
        pkg = Path(staging_stdout(first))
        snap = {p.relative_to(pkg): p.read_bytes()
                for p in pkg.rglob("*") if p.is_file()}

        second = self.stage()
        self.assertEqual(staging_stdout(second), str(pkg))
        again = {p.relative_to(pkg): p.read_bytes()
                 for p in pkg.rglob("*") if p.is_file()}
        self.assertEqual(snap, again)

    def test_converges_when_source_changes(self) -> None:
        res = self.stage()
        pkg = Path(staging_stdout(res))
        self.assertNotIn("STAGED-MARKER", (pkg / "dist" / "index.v2.js").read_text())

        mutated = self.tmp / "mutated"
        shutil.copytree(self.src, mutated)
        target = mutated / "dist" / "index.v2.js"
        target.write_text(target.read_text() + "\n// STAGED-MARKER\n")

        run_stage(mutated, self.runtime, PLUGIN_ID, OPENCODE_MANIFEST,
                  OPENCODE_VALIDATOR)
        self.assertIn("STAGED-MARKER", (pkg / "dist" / "index.v2.js").read_text())


class TestStagingFailsClosed(StagingHarness):
    """A package that cannot be validated must never be exposed."""

    def test_missing_manifest_entry_in_source_aborts(self) -> None:
        res = run_stage(
            self.src, self.runtime, PLUGIN_ID,
            OPENCODE_MANIFEST + ["dist/does_not_exist.js"],
            OPENCODE_VALIDATOR,
        )
        self.assertNotEqual(res.returncode, 0)
        self.assertIn("manifest entry not found in source", res.stderr)
        self.assertFalse((self.runtime / "agent_installation").exists())

    def test_missing_root_entry_aborts(self) -> None:
        """A package without the loader-resolvable root entry is rejected.

        OpenCode drops a directory registration without a root entry and
        says nothing — so the stage must refuse it.
        """
        without = [m for m in OPENCODE_MANIFEST if m != "server.js"]
        res = run_stage(self.src, self.runtime, PLUGIN_ID, without,
                        OPENCODE_VALIDATOR)
        self.assertNotEqual(res.returncode, 0)
        self.assertIn("missing the root entry server.js", res.stderr)

    def test_unresolved_internal_import_aborts(self) -> None:
        without = [m for m in OPENCODE_MANIFEST if m != "dist/workspace.js"]
        res = run_stage(self.src, self.runtime, PLUGIN_ID, without,
                        OPENCODE_VALIDATOR)
        self.assertNotEqual(res.returncode, 0)
        self.assertIn("unresolved internal imports", res.stderr)
        self.assertIn("./workspace.js", res.stderr)

    def test_failed_restage_leaves_previous_package_intact(self) -> None:
        """The swap is all-or-nothing; a bad run must not destroy a good install."""
        good = self.stage()
        pkg = Path(staging_stdout(good))
        before = {p.relative_to(pkg): p.read_bytes()
                  for p in pkg.rglob("*") if p.is_file()}

        broken_src = self.tmp / "broken-src"
        shutil.copytree(self.src, broken_src)
        (broken_src / "server.js").unlink()  # manifest entry vanishes

        bad = run_stage(broken_src, self.runtime, PLUGIN_ID, OPENCODE_MANIFEST,
                        OPENCODE_VALIDATOR)
        self.assertNotEqual(bad.returncode, 0)

        self.assertTrue((pkg / "server.js").is_file())
        after = {p.relative_to(pkg): p.read_bytes()
                 for p in pkg.rglob("*") if p.is_file()}
        self.assertEqual(before, after, "a failed restage mutated the live package")

    def test_empty_manifest_aborts(self) -> None:
        res = run_stage(self.src, self.runtime, PLUGIN_ID, [], OPENCODE_VALIDATOR)
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

        The adapter's build step is stubbed: a real `tsc` run would
        need the adapter's node_modules (populated by `npm ci`, which
        is a network operation and is NOT part of the CI job that runs
        this suite). The stub produces the payload files the build step
        expects, so the installer reaches the provisioning step — the
        one under test — and reports the missing library.
        """
        detached = self.tmp / f"detached-{PLUGIN_ID}"
        detached.mkdir()
        shutil.copytree(
            AGENT_DIR / PLUGIN_ID, detached / PLUGIN_ID,
            ignore=shutil.ignore_patterns("node_modules", "tests-build"),
        )
        # A stand-in for the adapter's build toolchain.
        tsc = detached / PLUGIN_ID / "node_modules" / ".bin" / "tsc"
        tsc.parent.mkdir(parents=True)
        tsc.write_text(
            "#!/usr/bin/env bash\n"
            "set -eu\n"
            "mkdir -p dist\n"
            "for f in index.v2.js wake-context.js event-envelope.js "
            "event-mapper.js event-subscriber.js workspace.js "
            "resolve-mpm-binary.js; do\n"
            "  [ -f \"dist/$f\" ] || printf 'export default {};\\n' > \"dist/$f\"\n"
            "done\n",
            encoding="utf-8",
        )
        tsc.chmod(0o755)

        home = self.tmp / "home-detached"
        home.mkdir(parents=True, exist_ok=True)
        rt = self.tmp / "rt-detached"
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
            ["bash", str(detached / PLUGIN_ID / "install.sh")],
            capture_output=True, text=True, env=env, cwd=str(self.tmp),
        )
        self.assertNotEqual(res.returncode, 0)
        self.assertIn(
            "runtime provisioning library not found", res.stderr,
            "a missing provisioning library must be named, not silent",
        )


class TestAdapterLinksTheRuntimePackage(unittest.TestCase):
    """Static contract: the adapter must link the runtime path, not source.

    Guards against a future edit registering the source checkout, which
    re-couples the live integration to a directory the operator may
    delete.
    """

    def test_the_adapter_stages_and_links_the_runtime_path(self) -> None:
        text = (AGENT_DIR / PLUGIN_ID / "install.sh").read_text()
        self.assertIn("stage_runtime_package", text)
        self.assertIn("CANONICAL_ADAPTER_DIR", text)
        self.assertIn("CANONICAL_PLUGIN_ENTRY", text)

    def test_the_adapter_sources_the_shared_staging_library(self) -> None:
        text = (AGENT_DIR / PLUGIN_ID / "install.sh").read_text()
        self.assertIn("stage_runtime_package.sh", text)

    def test_the_adapter_selects_the_opencode_validator(self) -> None:
        """The staged package must be validated by the OpenCode hook.

        The library's DEFAULT validator is the historical OpenClaw one,
        which demands an openclaw.plugin.json the OpenCode package does
        not carry — so the hook must be selected explicitly.
        """
        text = (AGENT_DIR / PLUGIN_ID / "install.sh").read_text()
        self.assertIn(
            "MPM_RUNTIME_PACKAGE_VALIDATOR=mpm_validate_runtime_package_opencode",
            text,
            "the OpenCode installer must select the OpenCode validator",
        )

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


# ---------------------------------------------------------------------------
# Adapter-payload validation (added when staging generalised past OpenClaw)
# ---------------------------------------------------------------------------

def run_validator(dir_: Path, validator: str, *args: str) -> subprocess.CompletedProcess:
    """Invoke one validator hook with a hermetic HOME."""
    cmd = [
        "bash", "-c",
        # $1 is the library, $2 the validator, $3.. its arguments.
        'set -euo pipefail; . "$1"; "$2" "${@:3}"',
        "bash", str(STAGE_LIB), validator, str(dir_), *args,
    ]
    home = dir_.parent / "home"
    home.mkdir(parents=True, exist_ok=True)
    env = dict(os.environ)
    env["HOME"] = str(home)
    return subprocess.run(cmd, capture_output=True, text=True, env=env)


def run_staged(dir_: Path, validator: str, plugin_id: str) -> subprocess.CompletedProcess:
    """Invoke the dispatcher exactly as stage_runtime_package does."""
    cmd = [
        "bash", "-c",
        'set -euo pipefail; . "$1"; MPM_RUNTIME_PACKAGE_VALIDATOR="$2" '
        'mpm_validate_staged_package "$3" "$4"',
        "bash", str(STAGE_LIB), validator, str(dir_), plugin_id,
    ]
    home = dir_.parent / "home"
    home.mkdir(parents=True, exist_ok=True)
    env = dict(os.environ)
    env["HOME"] = str(home)
    return subprocess.run(cmd, capture_output=True, text=True, env=env)


class ValidatorFixture(unittest.TestCase):
    def setUp(self) -> None:
        self.tmp = Path(tempfile.mkdtemp(prefix="mpm-validator-"))
        self.addCleanup(shutil.rmtree, self.tmp, ignore_errors=True)


class TestCoreValidation(ValidatorFixture):
    """The adapter-independent floor every runtime package must clear."""

    def test_empty_directory_is_rejected(self) -> None:
        d = self.tmp / "empty"
        d.mkdir()
        res = run_validator(d, "mpm_validate_runtime_package_core")
        self.assertNotEqual(res.returncode, 0)
        self.assertIn("is empty", res.stderr)

    def test_missing_directory_is_rejected(self) -> None:
        res = run_validator(self.tmp / "nope", "mpm_validate_runtime_package_core")
        self.assertNotEqual(res.returncode, 0)
        self.assertIn("does not exist", res.stderr)

    def test_orphan_bytecode_only_directory_is_rejected(self) -> None:
        """The exact live-host shape: a directory that looks installed."""
        d = self.tmp / "orphan"
        (d / "__pycache__").mkdir(parents=True)
        (d / "__pycache__" / "stale.cpython-314.pyc").write_text("")
        res = run_validator(d, "mpm_validate_runtime_package_core")
        self.assertNotEqual(res.returncode, 0)
        self.assertIn("residue", res.stderr)

    def test_populated_directory_passes(self) -> None:
        d = self.tmp / "ok"
        d.mkdir()
        (d / "index.ts").write_text("export default () => {}\n")
        self.assertEqual(
            run_validator(d, "mpm_validate_runtime_package_core").returncode, 0
        )


class TestOpenCodePayloadValidation(ValidatorFixture):
    """The OpenCode registration is the staged PACKAGE DIRECTORY, resolved
    by a root-level server/index module the OpenCode 2 loader looks for
    (never package.json). 'the build succeeded', 'a root entry exists', and
    'the registered path resolves' must stay separate facts: an entry-less
    directory verifies as a valid package while the loader silently drops
    it — that is exactly the 2026-10-16 load-nothing defect's shape."""

    def _pkg(self, dist: dict[str, str], *, server_js: str | None = "default_shim",
             package_json: bool = True) -> Path:
        d = self.tmp / "oc"
        (d / "dist").mkdir(parents=True)
        for name, body in dist.items():
            (d / "dist" / name).write_text(body)
        if server_js == "default_shim":
            (d / "server.js").write_text('export { default } from "./dist/index.v2.js";\n')
        elif server_js is not None:
            (d / "server.js").write_text(server_js)
        if package_json:
            (d / "package.json").write_text('{"main": "./dist/index.v2.js"}')
        return d

    def test_complete_bundle_passes(self) -> None:
        d = self._pkg({
            "index.v2.js": 'import { w } from "./workspace.js";\nexport default w;\n',
            "workspace.js": "export const w = () => {};\n",
            "resolve-mpm-binary.js": "export const r = () => {};\n",
        })
        self.assertEqual(run_staged(d, OPENCODE_VALIDATOR, PLUGIN_ID).returncode, 0)

    def test_root_entry_required(self) -> None:
        """A staged package without the root server.js entry registers and
        loads nothing, with no warning from OpenCode. This is the loader
        contract the 2026-10-16 regression was built on."""
        d = self._pkg({
            "index.v2.js": "export default {};\n",
            "workspace.js": "export const w = () => {};\n",
        }, server_js=None)
        res = run_staged(d, OPENCODE_VALIDATOR, PLUGIN_ID)
        self.assertNotEqual(res.returncode, 0)
        self.assertIn("missing the root entry server.js", res.stderr)

    def test_empty_root_entry_is_rejected(self) -> None:
        d = self._pkg({
            "index.v2.js": "export default {};\n",
            "workspace.js": "export const w = () => {};\n",
        }, server_js="")
        res = run_staged(d, OPENCODE_VALIDATOR, PLUGIN_ID)
        self.assertNotEqual(res.returncode, 0)
        self.assertIn("root entry server.js is empty", res.stderr)

    def test_missing_v2_payload_is_rejected(self) -> None:
        d = self._pkg({})
        res = run_staged(d, OPENCODE_VALIDATOR, PLUGIN_ID)
        self.assertNotEqual(res.returncode, 0)
        self.assertIn("missing dist/index.v2.js", res.stderr)

    def test_empty_v2_payload_is_rejected(self) -> None:
        """tsc can produce a zero-byte file on some failure paths; an empty
        payload registers and loads nothing."""
        d = self._pkg({"index.v2.js": ""})
        res = run_staged(d, OPENCODE_VALIDATOR, PLUGIN_ID)
        self.assertNotEqual(res.returncode, 0)
        self.assertIn("is empty", res.stderr)

    def test_unresolved_relative_import_is_rejected(self) -> None:
        d = self._pkg({"index.v2.js": 'import { w } from "./workspace.js";\n'})
        res = run_staged(d, OPENCODE_VALIDATOR, PLUGIN_ID)
        self.assertNotEqual(res.returncode, 0)
        self.assertIn("unresolved internal imports", res.stderr)
        self.assertIn("./workspace.js", res.stderr)

    def test_bare_specifier_is_not_treated_as_unresolved(self) -> None:
        """@opencode/plugin is aliased by the OpenCode 2 loader for
        directory plugins and is unresolvable in the checkout too.
        Rejecting it would fail every correct run."""
        d = self._pkg({
            "index.v2.js": 'import { Plugin } from "@opencode/plugin";\n'
                           "export default Plugin.define({ id: 'x', setup() {} });\n",
        })
        self.assertEqual(run_staged(d, OPENCODE_VALIDATOR, PLUGIN_ID).returncode, 0)


class TestValidatorDispatch(ValidatorFixture):
    def test_unknown_validator_name_fails_rather_than_passing(self) -> None:
        """A typo in an installer must degrade to a failure, not to no
        validation at all."""
        d = self.tmp / "d"
        d.mkdir()
        (d / "index.ts").write_text("export default () => {}\n")
        res = run_staged(d, "mpm_validate_does_not_exist", PLUGIN_ID)
        self.assertNotEqual(res.returncode, 0)
        self.assertIn("unknown runtime package validator", res.stderr)

    def test_the_library_default_validator_is_still_the_openclaw_shape(self) -> None:
        """The library's DEFAULT validator is the historical OpenClaw one.

        The shared library is retained as-is by the 2026-10-27 removal,
        so its default still demands an openclaw.plugin.json. The
        shipped adapter does not use the default — it selects the
        OpenCode hook explicitly (pinned above) — but the default's
        behaviour is still part of the library's contract, and a change
        to it would be a change to shared infrastructure rather than an
        adapter retirement.
        """
        d = self.tmp / "d"
        d.mkdir()
        (d / "index.ts").write_text("module.exports = {};\n")
        (d / "package.json").write_text('{"main": "index.js"}\n')
        (d / "index.js").write_text("module.exports = {};\n")
        (d / "openclaw.plugin.json").write_text('{"id": "thing"}\n')
        res = run_staged(d, "mpm_validate_runtime_package", "thing")
        self.assertEqual(res.returncode, 0, res.stderr)


class TestManifestEntriesExistInSource(unittest.TestCase):
    """A manifest naming a file tsc does not emit must fail loudly at
    stage time, not publish a package that throws on load."""

    def test_opencode_manifest_matches_tsc_output_shape(self) -> None:
        """tsc emits one .js per .ts under src/ (rootDir=src, outDir=dist),
        except for ambient declarations (.d.ts), which emit nothing. The
        manifest is therefore the compilable src/ file list plus
        package.json — anything else would be a manifest drift bug."""
        src = AGENT_DIR / PLUGIN_ID / "src"
        emitted = sorted(
            f"dist/{p.stem}.js"
            for p in src.glob("*.ts")
            if not p.name.endswith(".d.ts")
        )
        expected = sorted([
            "dist/index.js",
            "dist/index.v2.js",
            "dist/wake-context.js",
            "dist/event-envelope.js",
            "dist/event-mapper.js",
            "dist/event-subscriber.js",
            "dist/workspace.js",
            "dist/resolve-mpm-binary.js",
        ])
        self.assertEqual(emitted, expected,
                         "opencode src/ changed; the runtime manifest and "
                         "the fake-tsc test stub must both be updated")

    def test_opencode_staged_package_carries_root_entry(self) -> None:
        """server.js is not a tsc output — it is a checked-in shim — but it
        MUST be in the manifest: without it OpenCode drops the directory
        registration silently (the 2026-10-16 defect)."""
        self.assertTrue(
            (AGENT_DIR / PLUGIN_ID / "server.js").exists(),
            "server.js missing from the adapter source tree",
        )
        reexport = (AGENT_DIR / PLUGIN_ID / "server.js").read_text(
            encoding="utf-8"
        )
        self.assertIn("./dist/index.v2.js", reexport,
                      "server.js must re-export the compiled payload")


if __name__ == "__main__":
    unittest.main()
