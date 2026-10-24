"""
test_opencode_install.py — Regression coverage for mpm-opencode/install.sh.

Pins the Part-3 contract: the namespace-refresh migration
recognizes legacy 'opencode-mpm' references in OpenCode's
plugin config and migrates them to the canonical 'mpm-opencode'
path. The legacy generated directory (dist/) is cleaned up
when safe; user-installed source code is left alone.

Tested scenarios:
  A. Stale config: opencode.jsonc points at opencode-mpm path.
     After install, it points at mpm-opencode. The legacy
     generated dist/ is removed.
  B. Fresh config: no opencode.jsonc exists.
     After install, it is created with the canonical entry.
  C. Multiple stale entries + a non-stale entry are preserved.
  D. Idempotent rerun: second run leaves config unchanged.
  E. Legacy dist/ cleanup: removed only when the legacy
     directory is MPM-owned (contains the canonical src layout),
     not for arbitrary user-owned dirs.
  F. Unrelated plugin entries in opencode.jsonc are preserved.
  G. The legacy opencode-mpm directory remains for operator review
     after migration (we don't silently delete user-installed code).
"""

from __future__ import annotations

import json
import os
import hashlib
import shutil
import subprocess
import sys
import tempfile
import unittest
from pathlib import Path

# Derive the source-tree adapter directory from this test file's
# location rather than a hardcoded author-machine absolute path. The
# test lives at
#   agent_installation/mpm-opencode/tests/<file>.py
# so ADAPTER_DIR = parent.parent (= agent_installation/mpm-opencode/).
# The previous literal "/home/v/workspace/projects/mpm/agent_installation/..."
# pointed only at the original author's checkout; tests should resolve
# from the repository so they work without an installed MPM.
ADAPTER_DIR = Path(__file__).resolve().parent.parent
INSTALL_SH = ADAPTER_DIR / "install.sh"
# The shared library directory lives at agent_installation/scripts/, i.e.
# a sibling of every adapter directory. A staged adapter copy is
# therefore only faithful to the real layout if that sibling is staged
# too.
AGENT_INSTALLATION_DIR = ADAPTER_DIR.parent
SHARED_SCRIPTS_DIR = AGENT_INSTALLATION_DIR / "scripts"
# The canonical behavioural source. The shared build-provenance
# mechanism reads it to determine the behavioural protocol version, and
# an installed runtime root stages it next to the shared scripts
# directory — so a faithful sandbox stages it too. Without it the
# sandbox's managed blocks would carry protocol=unknown, and a future
# assertion about the protocol version would either fail for a reason
# that has nothing to do with the code under test, or (worse) pass
# against a value the product never produces.
CANONICAL_SOURCE = AGENT_INSTALLATION_DIR / "MPM_AGENT_INTEGRATION_SNIPPETS.md"


def _stage_sibling_scripts(adapter_dir: Path) -> None:
    """Put scripts/ where a staged adapter expects to find it.

    Real installs copy the whole repository into the runtime root, so
    agent_installation/scripts/ is always a sibling of the adapter
    directory. A hermetic sandbox that stages only the adapter would
    otherwise test an installer whose library is missing.

    The WHOLE shared directory is staged, not a hand-picked subset. The
    adapters import shared modules from it by path — the provisioning
    library, the convergence engine, the build-provenance mechanism —
    and a maintained list of "the shared files this adapter happens to
    need today" is a trap: the next shared module lands, the list is
    not updated, and the adapter dies at import time inside the sandbox
    while production works fine. That is the worst possible failure
    shape: a test-only break that hides a real one, or a real one that
    hides behind a test-only break.

    Copying the directory also keeps this fixture honest about the
    production layout instead of asserting it.
    """
    install_root = adapter_dir.parent
    scripts_dir = install_root / "scripts"
    if scripts_dir.exists():
        shutil.rmtree(scripts_dir)
    shutil.copytree(
        SHARED_SCRIPTS_DIR,
        scripts_dir,
        ignore=shutil.ignore_patterns("__pycache__"),
    )
    # The canonical behavioural source sits beside the shared scripts
    # directory in an installed runtime root, for the same reason.
    shutil.copy2(CANONICAL_SOURCE, install_root / CANONICAL_SOURCE.name)


# A tiny stub TypeScript compiler used to make build_dist() hermetic in
# tests. The real install.sh uses ./node_modules/.bin/tsc — tests stage
# the adapter into a tempdir without a populated node_modules, so we
# populate node_modules/.bin/tsc with this stub during _stage_adapter().
# Anything that calls a real `tsc` will see this stub.
FAKE_TSC_BODY = """#!/usr/bin/env bash
# fake tsc stub for mpm-opencode tests.
# Emits the same output shape real tsc does: one .js per .ts under src/,
# because install.sh's runtime manifest names those files explicitly.
set -e
# $0 may be relative or absolute; resolve it to an absolute path of the
# directory holding this stub (node_modules/.bin/), then ascend two
# levels to reach the adapter root. (The installer puts us at
# <adapter>/node_modules/.bin/tsc.)
script_dir="$(cd "$(dirname "$0")" && pwd)"
out_dir="$(cd "$script_dir/../.." && pwd)"
mkdir -p "$out_dir/dist"
cat > "$out_dir/dist/index.v2.js" <<'JS'
// stub generated by fake tsc stub for mpm-opencode tests
import { withWorkspace } from "./workspace.js";
import { resolveMpmBinary } from "./resolve-mpm-binary.js";
export default { withWorkspace, resolveMpmBinary, id: "mpm-opencode", setup() {} };
JS
cat > "$out_dir/dist/workspace.js" <<'JS'
// stub generated by fake tsc stub for mpm-opencode tests
export function withWorkspace(x) { return x; }
JS
cat > "$out_dir/dist/resolve-mpm-binary.js" <<'JS'
// stub generated by fake tsc stub for mpm-opencode tests
export function resolveMpmBinary() { return "/usr/bin/mpm"; }
JS
"""


def _run(args: list[str], home: Path, install_sh: Path | None = None) -> subprocess.CompletedProcess:
    """Run install.sh with a hermetic HOME."""
    env = os.environ.copy()
    env["HOME"] = str(home)
    return subprocess.run(
        ["bash", str(install_sh or INSTALL_SH), *args],
        capture_output=True,
        text=True,
        timeout=60,
        env=env,
    )


def _write_opencode_config(home: Path, plugins: list[str]) -> Path:
    cfg_dir = home / ".config" / "opencode"
    cfg_dir.mkdir(parents=True, exist_ok=True)
    cfg = cfg_dir / "opencode.jsonc"
    cfg.write_text(json.dumps({
        "$schema": "https://opencode.ai/config.json",
        "plugins": plugins,
    }, indent=2) + "\n")
    return cfg


def _stage_adapter(home: Path, adapter_src: Path) -> Path:
    """Stage the mpm-opencode adapter at ~/.mpm/agent_installation/.

    Returns the canonical adapter install path inside the sandbox."""
    install_root = home / ".mpm" / "agent_installation"
    canonical = install_root / "mpm-opencode"
    # Copy via rsync-style tree copy but exclude node_modules (linked)
    # and dist (regenerated by install.sh's build_dist step).
    if canonical.exists():
        shutil.rmtree(canonical)
    shutil.copytree(
        adapter_src,
        canonical,
        ignore=shutil.ignore_patterns("node_modules", "dist"),
    )
    # Populate a hermetic node_modules/.bin/tsc so build_dist() finds a
    # usable compiler. Tests don't need a real TypeScript build —
    # install.sh's contract here is "regenerate dist/index.v2.js when
    # missing/stale", and the stub satisfies that contract.
    nm = canonical / "node_modules"
    bin_dir = nm / ".bin"
    bin_dir.mkdir(parents=True, exist_ok=True)
    tsc_stub = bin_dir / "tsc"
    tsc_stub.write_text(FAKE_TSC_BODY)
    tsc_stub.chmod(0o755)
    # Pre-build dist/ via the stub so --verify (which checks for
    # dist/index.v2.js + server.js without running build_dist) sees a
    # complete canonical install state. server.js is a checked-in file, so
    # the tree copy has already carried it. Tests that explicitly want to
    # exercise the missing-dist rebuild path can delete dist/ before
    # invoking install.sh.
    subprocess.run(
        [str(tsc_stub)],
        cwd=str(canonical),
        capture_output=True,
        check=True,
    )
    _stage_sibling_scripts(canonical)
    return canonical


def _stage_legacy_dist(home: Path, with_src: bool = True) -> Path:
    """Create the legacy opencode-mpm directory with a generated dist/.

    If with_src is True, also create a minimal src/ so the directory
    looks MPM-owned (the cleanup heuristic only removes dist/ when the
    legacy directory is recognised as MPM-owned)."""
    legacy = home / ".mpm" / "agent_installation" / "opencode-mpm"
    legacy.mkdir(parents=True, exist_ok=True)
    if with_src:
        # Minimal src layout to mark this as MPM-owned.
        src_dir = legacy / "src"
        src_dir.mkdir(parents=True, exist_ok=True)
        (src_dir / "index.ts").write_text("// legacy stub\n")
    dist_dir = legacy / "dist"
    dist_dir.mkdir(parents=True, exist_ok=True)
    (dist_dir / "index.js").write_text("// legacy dist\n")
    return legacy


class OpenCodeInstallStaleConfigMigration(unittest.TestCase):
    """Stale opencode.jsonc entries are migrated to the canonical
    PACKAGE DIRECTORY by install.sh. Two historical shapes:

      * opencode-mpm namespace entries (pre-2026-09-17), and
      * the file://.../mpm-opencode/dist/index.js file registration
        written from 2026-09-17 through 2026-10-16, which OpenCode 2
        refuses outright ("configured plugin path must be a directory").
    """

    def setUp(self):
        self._tmp = Path(tempfile.mkdtemp(prefix="mpm-opencode-install-"))
        self.addCleanup(self._cleanup)
        self.home = self._tmp / "home"
        self.home.mkdir()

    def _cleanup(self):
        shutil.rmtree(self._tmp, ignore_errors=True)

    def test_broken_file_path_registration_is_migrated(self):
        """The 2026-10-16 defect shape: opencode.jsonc registers the
        dist/index.js FILE of the (correctly named) mpm-opencode package.
        install --verify used to accept it; install must replace it with
        the package directory and --verify must reject it beforehand.

        Negative control: under the OLD contract this test fails — verify
        accepted the broken shape as 'the registered path' because both
        spellings skewed the same adapter directory."""
        canonical = _stage_adapter(self.home, ADAPTER_DIR)
        cfg = _write_opencode_config(
            self.home,
            [f"file://{canonical}/dist/index.js", "npm:keep-me"],
        )

        # --verify against the broken registration must fail...
        result = _run(["--verify"], self.home)
        self.assertNotEqual(
            result.returncode, 0,
            "verify accepted a file://.../dist/index.js registration "
            "(the load-nothing shape)",
        )
        self.assertIn("does not register the canonical package directory",
                      result.stderr)

        # ...and install migrates it.
        result = _run([], self.home, install_sh=canonical / "install.sh")
        self.assertEqual(result.returncode, 0, result.stderr)
        text = cfg.read_text()
        self.assertNotIn("/dist/index.js", text,
            f"dist/index.js still registered:\n{text}")
        data = json.loads(cfg.read_text())
        self.assertEqual(
            [e for e in data["plugins"] if "mpm-opencode" in str(e)],
            [f"file://{canonical}"],
            "broken file registration not replaced with the package "
            "directory (exactly one MPM registration must remain)",
        )
        self.assertIn("npm:keep-me", data["plugins"])

        # And verify now passes.
        result = _run(["--verify"], self.home)
        self.assertEqual(result.returncode, 0, result.stderr)

    def test_verify_rejects_missing_server_entry_in_registered_package(self):
        """A registered package directory without the root server.js entry
        is dropped SILENTLY by the OpenCode loader. --verify is the only
        place that can catch it before the operator restarts a session."""
        canonical = _stage_adapter(self.home, ADAPTER_DIR)
        (canonical / "server.js").unlink()
        result = _run(["--verify"], self.home)
        self.assertNotEqual(result.returncode, 0)
        self.assertIn("no root entry server.js", result.stderr)

    def test_stale_opencode_mpm_path_is_migrated(self):        # Stale config pointing at the legacy path.
        canonical = _stage_adapter(self.home, ADAPTER_DIR)
        legacy_dist = _stage_legacy_dist(self.home)
        cfg = _write_opencode_config(
            self.home,
            [f"file://{legacy_dist}/dist/index.js"],
        )
        # Confirm the stale path.
        self.assertIn("opencode-mpm", cfg.read_text())

        result = _run(["--verify"], self.home)  # verify BEFORE install: should fail
        # Don't assert on the exact rc — the verify-before-install
        # path is informational. The important assertion is below.

        # Install.
        # Run install from the STAGED adapter copy (not from the
        # source tree) — this matches a fresh install scenario where
        # the user has copied or cloned the adapter to a new location.
        staged_install_sh = canonical / "install.sh"
        result = _run([], self.home, install_sh=staged_install_sh)
        self.assertEqual(result.returncode, 0, result.stderr)

        # Config now points at canonical mpm-opencode path; no
        # opencode-mpm reference remains.
        text = cfg.read_text()
        self.assertNotIn("opencode-mpm", text,
            f"opencode.jsonc still references legacy 'opencode-mpm':\n{text}")
        self.assertIn("mpm-opencode", text)
        self.assertIn(str(canonical), text)

        # Legacy generated dist/ was cleaned up.
        self.assertFalse(
            (legacy_dist / "dist").exists(),
            f"legacy generated dist/ not cleaned up:\n"
            f"{legacy_dist}",
        )

        # Canonical dist was built (or marked up to date).
        self.assertTrue((canonical / "dist" / "index.v2.js").exists())

    def test_no_existing_config_creates_canonical(self):
        # No opencode.jsonc exists — install must create one with the
        # canonical plugin entry.
        canonical = _stage_adapter(self.home, ADAPTER_DIR)
        cfg_path = self.home / ".config" / "opencode" / "opencode.jsonc"
        self.assertFalse(cfg_path.exists())

        staged_install_sh = canonical / "install.sh"
        result = _run([], self.home, install_sh=staged_install_sh)
        self.assertEqual(result.returncode, 0, result.stderr)

        self.assertTrue(cfg_path.exists(), "opencode.jsonc not created")
        data = json.loads(cfg_path.read_text())
        self.assertEqual(
            data["plugins"],
            [f"file://{canonical}"],
            "fresh config does not point at canonical path",
        )

    def test_unrelated_plugin_entries_are_preserved(self):
        # A stale opencode-mpm entry AND a non-stale unrelated entry
        # (different plugin id) — install must migrate only the stale
        # one and leave the unrelated one intact.
        canonical = _stage_adapter(self.home, ADAPTER_DIR)
        cfg = _write_opencode_config(self.home, [
            f"file://{self.home}/.mpm/agent_installation/opencode-mpm/dist/index.js",
            "npm:some-other-plugin@1.2.3",
        ])

        staged_install_sh = canonical / "install.sh"
        result = _run([], self.home, install_sh=staged_install_sh)
        self.assertEqual(result.returncode, 0, result.stderr)

        data = json.loads(cfg.read_text())
        # Canonical entry replaces stale one (no duplicates).
        self.assertIn(
            f"file://{canonical}",
            data["plugins"],
            "canonical plugin entry not present after install",
        )
        # The unrelated entry is preserved.
        self.assertIn(
            "npm:some-other-plugin@1.2.3",
            data["plugins"],
            "unrelated plugin entry was removed by install",
        )
        # And the stale one is gone.
        self.assertNotIn(
            "opencode-mpm",
            cfg.read_text(),
            "stale opencode-mpm entry not migrated",
        )

    def test_idempotent_rerun_preserves_config(self):
        canonical = _stage_adapter(self.home, ADAPTER_DIR)
        _stage_legacy_dist(self.home)
        _write_opencode_config(
            self.home,
            [f"file://{self.home}/.mpm/agent_installation/opencode-mpm/dist/index.js"],
        )

        staged_install_sh = canonical / "install.sh"
        # First install: migrates stale.
        r1 = _run([], self.home, install_sh=staged_install_sh)
        self.assertEqual(r1.returncode, 0, r1.stderr)
        canonical_entry = f"file://{canonical}"
        plugins1 = json.loads(
            (self.home / ".config" / "opencode" / "opencode.jsonc").read_text()
        )["plugins"]
        self.assertEqual(len(plugins1), 1)
        self.assertEqual(plugins1[0], canonical_entry)

        # Second install: idempotent — config unchanged. Must run
        # from the staged adapter (not the source tree) so the
        # hermetic node_modules/.bin/tsc stub is on the build path.
        r2 = _run([], self.home, install_sh=staged_install_sh)
        self.assertEqual(r2.returncode, 0, r2.stderr)
        plugins2 = json.loads(
            (self.home / ".config" / "opencode" / "opencode.jsonc").read_text()
        )["plugins"]
        self.assertEqual(plugins2, plugins1,
            "second install altered plugin[]")

    def test_legacy_user_source_is_left_alone(self):
        # The legacy directory is MPM-owned (has src/) but contains a
        # non-generated file (not in dist/) that looks like user
        # source. We remove ONLY dist/, not the rest of the tree.
        canonical = _stage_adapter(self.home, ADAPTER_DIR)
        legacy = _stage_legacy_dist(self.home, with_src=True)
        # Plant a non-dist file that looks like user content.
        user_file = legacy / "src" / "user-notes.md"
        user_file.write_text("# My notes\n")

        staged_install_sh = canonical / "install.sh"
        result = _run([], self.home, install_sh=staged_install_sh)
        self.assertEqual(result.returncode, 0, result.stderr)

        # The legacy directory still exists (we don't blindly remove).
        self.assertTrue(legacy.exists())
        # The generated dist/ was cleaned up.
        self.assertFalse((legacy / "dist").exists())
        # User content survives.
        self.assertTrue(user_file.exists(), "user content was removed")
        self.assertEqual(user_file.read_text(), "# My notes\n")

    def test_verify_fails_when_canonical_entry_absent(self):
        # A config holding only an unrelated plugin entry means MPM is
        # NOT registered with OpenCode. --verify must report that.
        #
        # This used to assert the opposite (returncode 0), on the
        # reasoning that the check only looks for 'opencode-mpm'
        # references. That reasoning described the gap rather than the
        # desired behaviour: verify passed with MPM entirely unregistered,
        # which is the exact failure class this installer already had.
        canonical = _stage_adapter(self.home, ADAPTER_DIR)
        _write_opencode_config(self.home, [
            "file:///some/unrelated/path/dist/index.js",
        ])
        staged_install_sh = canonical / "install.sh"
        result = _run(["--verify"], self.home, install_sh=staged_install_sh)
        self.assertEqual(result.returncode, 1,
            f"verify must fail when the canonical entry is absent; "
            f"stderr={result.stderr}")
        self.assertIn("does not register the canonical package directory",
                      result.stderr)

    def test_verify_passes_when_canonical_entry_present(self):
        canonical = _stage_adapter(self.home, ADAPTER_DIR)
        cfg = _write_opencode_config(self.home, [
            f"file://{canonical}",
            "someone-elses-plugin",
        ])
        staged_install_sh = canonical / "install.sh"
        result = _run(["--verify"], self.home, install_sh=staged_install_sh)
        self.assertEqual(result.returncode, 0,
            f"verify should pass with the canonical entry registered; "
            f"stderr={result.stderr}")
        # Verification must not mutate the user's config.
        self.assertIn("someone-elses-plugin", cfg.read_text())

    def test_legacy_dist_with_no_src_is_removed(self):
        # If the legacy directory has ONLY a generated dist/ (no src
        # tree at all), the cleanup heuristic still removes the
        # dist/. The script cannot distinguish "user accidentally
        # left a dist/ behind" from "generated dist/" — the safe
        # default is to remove.
        canonical = _stage_adapter(self.home, ADAPTER_DIR)
        legacy = _stage_legacy_dist(self.home, with_src=False)
        self.assertTrue((legacy / "dist").exists())

        staged_install_sh = canonical / "install.sh"
        result = _run([], self.home, install_sh=staged_install_sh)
        self.assertEqual(result.returncode, 0, result.stderr)

        self.assertFalse((legacy / "dist").exists(),
            "legacy dist/ not removed even though legacy has no src/")

    def test_fresh_install_with_legacy_user_installed_source_succeeds(self):
        # If the user has manually cloned mpm-opencode to a custom
        # location and installed it there, our canonical install
        # should not clobber it. But the install SH creates a fresh
        # plugin[] entry pointing at the canonical path — the user
        # would need to manually remove their custom install.
        # This test just verifies we don't FAIL or DELETE anything
        # in this scenario.
        canonical = _stage_adapter(self.home, ADAPTER_DIR)
        user_custom = self.home / "my" / "opencode-plugins" / "mpm-opencode"
        user_custom.mkdir(parents=True, exist_ok=True)
        (user_custom / "dist").mkdir(parents=True, exist_ok=True)
        (user_custom / "dist" / "index.js").write_text("// user custom\n")

        staged_install_sh = canonical / "install.sh"
        result = _run([], self.home, install_sh=staged_install_sh)
        self.assertEqual(result.returncode, 0, result.stderr)

        # User's custom dir survives.
        self.assertTrue(user_custom.exists())
        self.assertTrue((user_custom / "dist" / "index.js").exists())


class OpenCodeInstallBuildContract(unittest.TestCase):
    """install.sh must build dist/ if missing."""

    def setUp(self):
        self._tmp = Path(tempfile.mkdtemp(prefix="mpm-opencode-install-"))
        self.addCleanup(self._cleanup)
        self.home = self._tmp / "home"
        self.home.mkdir()

    def _cleanup(self):
        shutil.rmtree(self._tmp, ignore_errors=True)

    def test_install_rebuilds_missing_dist(self):
        canonical = _stage_adapter(self.home, ADAPTER_DIR)
        # Remove dist to force rebuild.
        if (canonical / "dist").exists():
            shutil.rmtree(canonical / "dist")

        result = _run([], self.home, install_sh=canonical / "install.sh")
        self.assertEqual(result.returncode, 0, result.stderr)
        self.assertTrue((canonical / "dist" / "index.v2.js").exists(),
            "install did not rebuild missing dist/")


class OpenCodeInstallUninstallContract(unittest.TestCase):
    """install.sh --uninstall must remove the canonical plugin
    entry and refresh AGENTS.md."""

    def setUp(self):
        self._tmp = Path(tempfile.mkdtemp(prefix="mpm-opencode-install-"))
        self.addCleanup(self._cleanup)
        self.home = self._tmp / "home"
        self.home.mkdir()

    def _cleanup(self):
        shutil.rmtree(self._tmp, ignore_errors=True)

    def test_uninstall_removes_canonical_entry(self):
        canonical = _stage_adapter(self.home, ADAPTER_DIR)
        cfg = _write_opencode_config(self.home, [
            f"file://{canonical}",
            "npm:keep-me",
        ])

        staged_install_sh = canonical / "install.sh"
        result = _run(["--uninstall"], self.home, install_sh=staged_install_sh)
        self.assertEqual(result.returncode, 0, result.stderr)

        data = json.loads(cfg.read_text())
        # Canonical entry removed.
        self.assertNotIn(
            f"file://{canonical}",
            data["plugins"],
        )
        # Unrelated entry preserved.
        self.assertIn("npm:keep-me", data["plugins"])

    def test_uninstall_removes_every_mpm_owned_historical_shape(self):
        """Uninstall must remove EVERY MPM-owned registration shape a
        config can carry — the canonical package directory, the broken
        dist/index.js file registration, and every legacy opencode-mpm
        variant — from both the V2 `plugins` key and the legacy `plugin`
        key, while touching no unrelated entry.

        Negative control: the OLD uninstall read only the legacy `plugin`
        key with json.loads and compared entries by exact string
        equality, so (a) a commented config aborted silently, (b) the
        `plugins` key survived untouched, and (c) any mismatch in entry
        spelling defeated the comparison."""
        canonical = _stage_adapter(self.home, ADAPTER_DIR)
        legacy_dir = (self.home / ".mpm" / "agent_installation"
                      / "opencode-mpm")
        broken = f"file://{canonical}/dist/index.js"
        legacy_file = f"file://{legacy_dir}/dist/index.js"
        legacy_dir_url = f"file://{legacy_dir}"
        cfg = _write_opencode_config(self.home, [
            broken, legacy_file, legacy_dir_url, f"file://{canonical}",
            "npm:keep-me",
        ])

        result = _run(["--uninstall"], self.home,
                      install_sh=canonical / "install.sh")
        self.assertEqual(result.returncode, 0, result.stderr)

        data = json.loads(cfg.read_text())
        for stale in (broken, legacy_file, legacy_dir_url,
                      f"file://{canonical}"):
            self.assertNotIn(stale, data["plugins"],
                             f"{stale} survived uninstall")
        self.assertEqual(
            [e for e in data["plugins"] if "opencode-mpm" in str(e)],
            [], "legacy namespace entries survived uninstall",
        )
        self.assertEqual(data["plugins"], ["npm:keep-me"],
                         "unrelated entry damaged by uninstall")

    def test_uninstall_removes_the_emptied_plugin_key(self):
        """A plugin array emptied by uninstall must be removed entirely:
        a lingering empty [] member is inert but is still orphan state
        this installer wrote."""
        canonical = _stage_adapter(self.home, ADAPTER_DIR)
        cfg = _write_opencode_config(self.home, [
            "npm:keep-me", f"file://{canonical}",
        ])
        result = _run(["--uninstall"], self.home,
                      install_sh=canonical / "install.sh")
        self.assertEqual(result.returncode, 0, result.stderr)
        text = cfg.read_text()
        self.assertNotIn(f"file://{canonical}", text)
        self.assertIn("npm:keep-me", text)

    def test_uninstall_of_mpm_only_config_removes_the_plugin_key(self):
        """When uninstall empties MPM's own key, the key goes with it —
        the config keeps only its non-MPM members ($schema)."""
        canonical = _stage_adapter(self.home, ADAPTER_DIR)
        cfg = _write_opencode_config(self.home, [f"file://{canonical}"])
        result = _run(["--uninstall"], self.home,
                      install_sh=canonical / "install.sh")
        self.assertEqual(result.returncode, 0, result.stderr)
        data = json.loads(cfg.read_text())
        self.assertNotIn("plugins", data)
        self.assertEqual(
            data.get("$schema"), "https://opencode.ai/config.json",
            "uninstall must not touch non-MPM members",
        )


class OpenCodeInstallJsonc(unittest.TestCase):
    """JSONC handling at the installer level.

    The defect: opencode.jsonc is JSONC, so it legitimately contains
    comments and trailing commas. install.sh read it with json.loads,
    caught the resulting exception, and exited 0 — so the shell reported a
    successful migration while the stale entry survived and the plugin
    loader broke. The user saw "done" and an unusable install.
    """

    def setUp(self):
        self._tmp = Path(tempfile.mkdtemp(prefix="mpm-opencode-jsonc-"))
        self.addCleanup(self._cleanup)
        self.home = self._tmp / "home"
        self.home.mkdir()

    def _cleanup(self):
        shutil.rmtree(self._tmp, ignore_errors=True)

    def _write_raw_config(self, body: str) -> Path:
        cfg_dir = self.home / ".config" / "opencode"
        cfg_dir.mkdir(parents=True, exist_ok=True)
        cfg = cfg_dir / "opencode.jsonc"
        cfg.write_text(body)
        return cfg

    def _legacy_entry(self) -> str:
        return f"file://{self.home}/.mpm/agent_installation/opencode-mpm/dist/index.js"

    def test_commented_config_is_migrated_not_skipped(self):
        """The core regression: a valid commented config must migrate."""
        canonical = _stage_adapter(self.home, ADAPTER_DIR)
        cfg = self._write_raw_config(
            "{\n"
            "  // plugins I actually use\n"
            '  "$schema": "https://opencode.ai/config.json",\n'
            '  "plugin": [\n'
            f'    "{self._legacy_entry()}",\n'
            "  ],\n"
            "}\n"
        )

        result = _run([], self.home, install_sh=canonical / "install.sh")
        self.assertEqual(result.returncode, 0, result.stderr)

        text = cfg.read_text()
        self.assertNotIn("opencode-mpm/dist", text,
                         f"stale entry survived migration:\n{text}")
        # The operator's comment and $schema URL both survive.
        self.assertIn("// plugins I actually use", text)
        self.assertIn("https://opencode.ai/config.json", text)

    def test_trailing_commas_are_tolerated(self):
        canonical = _stage_adapter(self.home, ADAPTER_DIR)
        cfg = self._write_raw_config(
            "{\n"
            '  "plugin": [\n'
            f'    "{self._legacy_entry()}",\n'
            "  ],\n"
            '  "theme": "dark",\n'
            "}\n"
        )
        result = _run([], self.home, install_sh=canonical / "install.sh")
        self.assertEqual(result.returncode, 0, result.stderr)
        self.assertNotIn("opencode-mpm/dist", cfg.read_text())

    def test_malformed_config_fails_loudly_and_is_not_modified(self):
        """A config we cannot parse must abort the install, not fake success."""
        canonical = _stage_adapter(self.home, ADAPTER_DIR)
        broken = "{\n  \"plugin\": [\n"
        cfg = self._write_raw_config(broken)

        result = _run([], self.home, install_sh=canonical / "install.sh")
        self.assertNotEqual(
            result.returncode, 0,
            "install must NOT report success on an unparseable config",
        )
        self.assertIn("migration failed", result.stderr)
        # The user's file is left exactly as they had it.
        self.assertEqual(cfg.read_text(), broken)

    def test_install_does_not_claim_done_when_migration_fails(self):
        """The false-success message must not appear on a failed install."""
        canonical = _stage_adapter(self.home, ADAPTER_DIR)
        self._write_raw_config("{ this is not json at all\n")
        result = _run([], self.home, install_sh=canonical / "install.sh")
        self.assertNotEqual(result.returncode, 0)
        self.assertNotIn("[mpm-opencode install] done", result.stderr)
        self.assertIn("install aborted", result.stderr)

    def test_repeated_install_is_byte_identical(self):
        canonical = _stage_adapter(self.home, ADAPTER_DIR)
        self._write_raw_config(
            "{\n  // note\n"
            '  "plugin": [\n'
            f'    "{self._legacy_entry()}",\n'
            '    "keep-me",  // unrelated\n'
            "  ],\n}\n"
        )
        staged = canonical / "install.sh"
        first = _run([], self.home, install_sh=staged)
        self.assertEqual(first.returncode, 0, first.stderr)
        after_first = (self.home / ".config" / "opencode" / "opencode.jsonc").read_text()

        second = _run([], self.home, install_sh=staged)
        self.assertEqual(second.returncode, 0, second.stderr)
        after_second = (self.home / ".config" / "opencode" / "opencode.jsonc").read_text()

        self.assertEqual(
            after_first, after_second,
            "a repeated install rewrote an already-correct config; "
            "comments and formatting should be untouched",
        )
        self.assertIn("// note", after_second)
        self.assertIn("// unrelated", after_second)
        self.assertIn("keep-me", after_second)

    def test_unrelated_config_preserved_through_migration(self):
        canonical = _stage_adapter(self.home, ADAPTER_DIR)
        cfg = self._write_raw_config(
            "{\n"
            '  "$schema": "https://opencode.ai/config.json",\n'
            '  "theme": "dark",\n'
            '  "model": "anthropic/claude",\n'
            '  "mcp": {\n    "some-server": {\n      "command": "npx",\n'
            '      "args": ["-y", "server"]\n    }\n  },\n'
            '  "plugin": [\n'
            f'    "{self._legacy_entry()}",\n'
            '    "npm:another-plugin"\n'
            "  ]\n}\n"
        )
        result = _run([], self.home, install_sh=canonical / "install.sh")
        self.assertEqual(result.returncode, 0, result.stderr)

        sys.path.insert(0, str(Path(__file__).resolve().parent.parent / "scripts"))
        import jsonc_config as jc

        data = jc.parse(cfg.read_text())
        self.assertEqual(data["theme"], "dark")
        self.assertEqual(data["model"], "anthropic/claude")
        self.assertEqual(data["mcp"]["some-server"]["command"], "npx")
        self.assertIn("npm:another-plugin", data["plugins"])
        self.assertNotIn("opencode-mpm/dist", cfg.read_text())


class OpenCodeInstallWorkspace(unittest.TestCase):
    """MPM_WORKSPACE must be honoured when locating adapter assets."""

    def setUp(self):
        self._tmp = Path(tempfile.mkdtemp(prefix="mpm-opencode-ws-"))
        self.addCleanup(self._cleanup)
        self.home = self._tmp / "home"
        self.home.mkdir()
        self.workspace = self._tmp / "relocated-workspace"

    def _cleanup(self):
        shutil.rmtree(self._tmp, ignore_errors=True)

    def _run(self, args, install_sh: Path, env_extra=None):
        env = os.environ.copy()
        env["HOME"] = str(self.home)
        env.pop("MPM_WORKSPACE", None)
        if env_extra:
            env.update(env_extra)
        return subprocess.run(
            ["bash", str(install_sh), *args],
            capture_output=True, text=True, timeout=60, env=env,
        )

    def _stage_into_workspace(self) -> Path:
        """Install the adapter under $MPM_WORKSPACE, not $HOME/.mpm."""
        canonical = self.workspace / "agent_installation" / "mpm-opencode"
        if canonical.exists():
            shutil.rmtree(canonical)
        shutil.copytree(
            ADAPTER_DIR, canonical,
            ignore=shutil.ignore_patterns("node_modules", "dist"),
        )
        bin_dir = canonical / "node_modules" / ".bin"
        bin_dir.mkdir(parents=True, exist_ok=True)
        tsc = bin_dir / "tsc"
        tsc.write_text(FAKE_TSC_BODY)
        tsc.chmod(0o755)
        subprocess.run([str(tsc)], cwd=str(canonical), capture_output=True, check=True)
        _stage_sibling_scripts(canonical)
        return canonical

    def test_relocated_workspace_is_honoured(self):
        canonical = self._stage_into_workspace()
        result = self._run(
            [], canonical / "install.sh",
            {"MPM_WORKSPACE": str(self.workspace)},
        )
        self.assertEqual(result.returncode, 0, result.stderr)

        cfg = self.home / ".config" / "opencode" / "opencode.jsonc"
        self.assertTrue(cfg.exists(), "opencode.jsonc not created")
        text = cfg.read_text()
        self.assertIn(str(self.workspace), text,
                      "plugin entry did not follow MPM_WORKSPACE")
        self.assertIn(str(canonical), text)
        # It must NOT point at the default $HOME/.mpm location.
        self.assertNotIn(f"{self.home}/.mpm/agent_installation", text)

    def test_unset_workspace_keeps_default_home_location(self):
        """Default behaviour must be byte-compatible with pre-change installs."""
        canonical = _stage_adapter(self.home, ADAPTER_DIR)
        result = self._run([], canonical / "install.sh", None)
        self.assertEqual(result.returncode, 0, result.stderr)
        cfg = self.home / ".config" / "opencode" / "opencode.jsonc"
        self.assertIn(f"file://{canonical}", cfg.read_text())


class OpenCodeInstallProvisioning(unittest.TestCase):
    """The registered path must hold a payload, however the install ran.

    build_dist() compiles into $SCRIPT_DIR. The registration names
    $CANONICAL_ADAPTER_DIR. When the installer runs from anywhere but
    the installed workspace those are two different directories, so the
    config used to get an entry pointing at nothing: OpenCode loads no
    plugin and reports no error.

    The installer now provisions the built payload into the runtime
    root, so running from a checkout is a supported thing to do rather
    than a trap.
    """

    def setUp(self):
        self._tmp = Path(tempfile.mkdtemp(prefix="mpm-opencode-payload-"))
        self.addCleanup(lambda: shutil.rmtree(self._tmp, ignore_errors=True))
        self.home = self._tmp / "home"
        self.home.mkdir()

    def _run(self, args, install_sh: Path, env_extra=None):
        env = os.environ.copy()
        env["HOME"] = str(self.home)
        env.pop("MPM_WORKSPACE", None)
        if env_extra:
            env.update(env_extra)
        return subprocess.run(
            ["bash", str(install_sh), *args],
            capture_output=True, text=True, timeout=60, env=env,
        )

    def _stage_buildable(self, dest: Path) -> Path:
        """A fully buildable adapter copy at an arbitrary path."""
        if dest.exists():
            shutil.rmtree(dest)
        shutil.copytree(
            ADAPTER_DIR, dest,
            ignore=shutil.ignore_patterns("node_modules", "dist"),
        )
        bin_dir = dest / "node_modules" / ".bin"
        bin_dir.mkdir(parents=True, exist_ok=True)
        tsc = bin_dir / "tsc"
        tsc.write_text(FAKE_TSC_BODY)
        tsc.chmod(0o755)
        _stage_sibling_scripts(dest)
        return dest

    def _registered_entry(self):
        cfg = self.home / ".config" / "opencode" / "opencode.jsonc"
        self.assertTrue(cfg.exists(), "opencode.jsonc was not created")
        return json.loads(cfg.read_text())["plugins"]

    # -- the deployment gap this closes --

    def test_install_from_a_checkout_registers_a_provisioned_path(self):
        """The checkout is not the runtime root, and must not be
        registered as though it were."""
        checkout = self._stage_buildable(self._tmp / "checkout" / "mpm-opencode")

        result = self._run([], checkout / "install.sh")
        self.assertEqual(result.returncode, 0, result.stderr)

        canonical = self.home / ".mpm" / "agent_installation" / "mpm-opencode"
        entry = f"file://{canonical}"
        self.assertIn(entry, self._registered_entry())
        self.assertNotIn(
            str(checkout), " ".join(self._registered_entry()),
            "the checkout path was registered instead of the runtime path",
        )

    def test_registered_path_actually_holds_the_built_payload(self):
        checkout = self._stage_buildable(self._tmp / "checkout" / "mpm-opencode")
        result = self._run([], checkout / "install.sh")
        self.assertEqual(result.returncode, 0, result.stderr)

        canonical = self.home / ".mpm" / "agent_installation" / "mpm-opencode"
        for rel in ("server.js", "dist/index.v2.js", "dist/workspace.js",
                    "dist/resolve-mpm-binary.js", "package.json"):
            self.assertTrue(
                (canonical / rel).exists(),
                f"runtime package is missing manifest entry {rel}",
            )

    def test_empty_runtime_workspace_is_provisioned_not_trusted(self):
        """An existing but empty directory is the orphan state, and used
        to satisfy any 'does the directory exist' check."""
        canonical = self.home / ".mpm" / "agent_installation" / "mpm-opencode"
        canonical.mkdir(parents=True)

        checkout = self._stage_buildable(self._tmp / "checkout" / "mpm-opencode")
        result = self._run([], checkout / "install.sh")
        self.assertEqual(result.returncode, 0, result.stderr)
        self.assertTrue((canonical / "dist" / "index.v2.js").exists())

    def test_orphan_bytecode_only_directory_is_replaced(self):
        canonical = self.home / ".mpm" / "agent_installation" / "mpm-opencode"
        cache = canonical / "__pycache__"
        cache.mkdir(parents=True)
        (cache / "stale.cpython-314.pyc").write_text("")

        checkout = self._stage_buildable(self._tmp / "checkout" / "mpm-opencode")
        result = self._run([], checkout / "install.sh")
        self.assertEqual(result.returncode, 0, result.stderr)
        self.assertTrue((canonical / "dist" / "index.v2.js").exists())
        self.assertFalse((canonical / "__pycache__").exists())

    def test_build_failure_registers_nothing(self):
        """A failed compile must not leave a registration behind."""
        checkout = self._stage_buildable(self._tmp / "checkout" / "mpm-opencode")
        # A tsc that exits non-zero: the build failure case.
        (checkout / "node_modules" / ".bin" / "tsc").write_text(
            "#!/usr/bin/env bash\necho 'boom' >&2\nexit 2\n")
        (checkout / "node_modules" / ".bin" / "tsc").chmod(0o755)

        result = self._run([], checkout / "install.sh")
        self.assertNotEqual(result.returncode, 0)
        cfg = self.home / ".config" / "opencode" / "opencode.jsonc"
        self.assertFalse(cfg.exists(),
                         "a registration was written despite a failed build")

    def test_missing_staging_library_is_reported_and_registers_nothing(self):
        checkout = self._stage_buildable(self._tmp / "checkout" / "mpm-opencode")
        (checkout.parent / "scripts" / "stage_runtime_package.sh").unlink()

        result = self._run([], checkout / "install.sh")
        self.assertEqual(result.returncode, 2)
        self.assertIn("runtime provisioning library not found",
                      result.stderr + result.stdout)
        cfg = self.home / ".config" / "opencode" / "opencode.jsonc"
        self.assertFalse(cfg.exists())

    def test_repeat_install_is_idempotent(self):
        checkout = self._stage_buildable(self._tmp / "checkout" / "mpm-opencode")
        self.assertEqual(self._run([], checkout / "install.sh").returncode, 0)
        cfg = self.home / ".config" / "opencode" / "opencode.jsonc"
        first = cfg.read_bytes()
        self.assertEqual(self._run([], checkout / "install.sh").returncode, 0)
        self.assertEqual(cfg.read_bytes(), first)

    def test_operator_file_in_runtime_package_survives_an_upgrade(self):
        canonical = self.home / ".mpm" / "agent_installation" / "mpm-opencode"
        canonical.mkdir(parents=True)
        (canonical / "OPERATOR_NOTES.md").write_text("keep me\n")

        checkout = self._stage_buildable(self._tmp / "checkout" / "mpm-opencode")
        result = self._run([], checkout / "install.sh")
        self.assertEqual(result.returncode, 0, result.stderr)
        self.assertEqual(
            (canonical / "OPERATOR_NOTES.md").read_text(), "keep me\n",
            "an operator-owned file was removed by staging",
        )

    def test_install_from_the_installed_workspace_still_works(self):
        """The pre-existing path: run from where the package already is."""
        canonical = _stage_adapter(self.home, ADAPTER_DIR)
        result = self._run([], canonical / "install.sh")
        self.assertEqual(result.returncode, 0, result.stderr)
        self.assertIn(
            f"file://{canonical}", self._registered_entry(),
        )

    def test_stale_dist_rebuild_then_register_still_works(self):
        """The pre-existing 'rebuild missing dist' contract is intact."""
        # _stage_buildable never creates dist/, so this is exactly the
        # "dist/index.v2.js missing" branch build_dist() exists to handle.
        canonical = self._stage_buildable(
            self.home / ".mpm" / "agent_installation" / "mpm-opencode"
        )
        self.assertFalse((canonical / "dist").exists())
        result = self._run([], canonical / "install.sh")
        self.assertEqual(result.returncode, 0, result.stderr)
        self.assertTrue((canonical / "dist" / "index.v2.js").exists())

    def test_relocated_workspace_provisions_into_the_relocated_root(self):
        workspace = self._tmp / "relocated"
        checkout = self._stage_buildable(self._tmp / "checkout" / "mpm-opencode")
        result = self._run(
            [], checkout / "install.sh", {"MPM_WORKSPACE": str(workspace)},
        )
        self.assertEqual(result.returncode, 0, result.stderr)
        self.assertTrue(
            (workspace / "agent_installation" / "mpm-opencode"
             / "dist" / "index.v2.js").exists(),
            "payload was not provisioned into the relocated root",
        )
        self.assertIn(
            str(workspace),
            " ".join(self._registered_entry()),
            "registration did not follow MPM_WORKSPACE",
        )


# ---------------------------------------------------------------------------
# Toolchain bootstrap
# ---------------------------------------------------------------------------
#
# A machine that has never run the OpenCode adapter has no
# node_modules/ — the directory is gitignored, so it is never in a
# checkout, and `npm ci` has never run there. Before this contract,
# build_dist() found no compiler and died with "run 'npm ci'", so the
# only way to a working OpenCode plugin was a manual, out-of-band
# compile step that the installer never performed and never verified.
#
# The installer now obtains its own build dependencies. The tests below
# pin that, and pin the diagnostics for the cases where it cannot:
# a machine with no Node, a registry that cannot be reached, and a
# compiler that fails.
#
# The stubs are hermetic: a fake `npm` on PATH stands in for the real
# one, and PATH never contains the real node/npm, so the suite does not
# depend on what the host happens to have installed.

# The PATH every test in this section runs with: coreutils and python3
# (the JSONC migration needs python3) but no node and no npm. The real
# node lives under a version-manager directory that is deliberately not
# on this PATH.
BOOTSTRAP_PATH = "/usr/local/sbin:/usr/local/bin:/usr/sbin:/usr/bin:/sbin:/bin"

# A fake `npm` that stands in for the real one. `npm ci` populates the
# adapter's devDependencies (typescript + @types/node) from the
# committed package-lock.json. The stub records every invocation so a
# test can assert that npm was NOT called when it was not needed.
FAKE_NPM_BODY = """#!/usr/bin/env bash
# fake npm for mpm-opencode toolchain-bootstrap tests.
set -u
echo "$*" >> "${MPM_TEST_NPM_LOG:?}"
if [ "${1:-}" != "ci" ]; then
  echo "fake npm: unsupported command '$*'" >&2
  exit 64
fi
if [ "${MPM_TEST_NPM_FAIL:-0}" != "0" ]; then
  echo "npm error code ENOTFOUND" >&2
  echo "npm error network request to https://registry.npmjs.org/typescript failed" >&2
  exit 1
fi
mkdir -p node_modules/.bin
if [ "${MPM_TEST_TSC_FAIL:-0}" != "0" ]; then
  cat > node_modules/.bin/tsc <<'MPM_TEST_TSC_FAIL_EOF'
#!/usr/bin/env bash
echo "tsc: compile error" >&2
exit 2
MPM_TEST_TSC_FAIL_EOF
else
  cat > node_modules/.bin/tsc <<'MPM_TEST_TSC_EOF'
@@TSC_BODY@@MPM_TEST_TSC_EOF
fi
chmod 0755 node_modules/.bin/tsc
echo "added 3 packages"
"""


class OpenCodeInstallToolchainBootstrap(unittest.TestCase):
    """The installer must obtain its own build dependencies.

    The contract under test: a normal installation produces a working
    OpenCode plugin with no manual compilation step, and when it cannot
    it says exactly what is missing instead of reporting success.
    """

    def setUp(self):
        self._tmp = Path(tempfile.mkdtemp(prefix="mpm-opencode-bootstrap-"))
        self.addCleanup(lambda: shutil.rmtree(self._tmp, ignore_errors=True))
        self.home = self._tmp / "home"
        self.home.mkdir()
        self.bindir = self._tmp / "bin"
        self.bindir.mkdir()
        self.npm_log = self._tmp / "npm-calls.log"

    def _stage_adapter_without_toolchain(self) -> Path:
        """Stage the adapter with NO node_modules and NO dist/.

        This is the state of a machine that has never run the adapter:
        node_modules/ and dist/ are gitignored, so neither is ever in a
        checkout.
        """
        canonical = self.home / ".mpm" / "agent_installation" / "mpm-opencode"
        shutil.copytree(
            ADAPTER_DIR, canonical,
            ignore=shutil.ignore_patterns("node_modules", "dist"),
        )
        _stage_sibling_scripts(canonical)
        return canonical

    def _install_fake_npm(self, *, fail: bool = False) -> None:
        body = FAKE_NPM_BODY.replace("@@TSC_BODY@@", FAKE_TSC_BODY)
        npm = self.bindir / "npm"
        npm.write_text(body)
        npm.chmod(0o755)
        # A fake `node` so the installer sees a toolchain at all; the
        # real one is never invoked (npm is a stub, tsc is a stub).
        node = self.bindir / "node"
        node.write_text("#!/usr/bin/env bash\nexit 0\n")
        node.chmod(0o755)
        self._npm_fail = fail

    def _run_install(self, canonical: Path, args=None):
        env = os.environ.copy()
        env["HOME"] = str(self.home)
        env.pop("MPM_WORKSPACE", None)
        env["PATH"] = f"{self.bindir}:{BOOTSTRAP_PATH}"
        env["MPM_TEST_NPM_LOG"] = str(self.npm_log)
        if getattr(self, "_npm_fail", False):
            env["MPM_TEST_NPM_FAIL"] = "1"
        if getattr(self, "_tsc_fail", False):
            env["MPM_TEST_TSC_FAIL"] = "1"
        return subprocess.run(
            ["bash", str(canonical / "install.sh"), *(args or [])],
            capture_output=True, text=True, timeout=120, env=env,
        )

    def _npm_calls(self) -> str:
        if not self.npm_log.exists():
            return ""
        return self.npm_log.read_text()

    # -- the deployment gap this closes --

    def test_missing_toolchain_is_bootstrapped_by_the_installer(self):
        """No node_modules, no dist/ — and the install still produces a
        usable plugin, with no manual compilation step."""
        canonical = self._stage_adapter_without_toolchain()
        self._install_fake_npm()

        result = self._run_install(canonical)

        self.assertEqual(result.returncode, 0, result.stderr)
        self.assertIn("ci", self._npm_calls(),
                      "the installer never obtained its build dependencies")
        self.assertTrue(
            (canonical / "dist" / "index.v2.js").exists(),
            "the plugin payload was not compiled",
        )
        # The plugin is usable: the registered package holds the root
        # entry AND the payload it re-exports.
        registered = self.home / ".mpm" / "agent_installation" / "mpm-opencode"
        for rel in ("server.js", "dist/index.v2.js"):
            self.assertTrue((registered / rel).exists(),
                            f"registered package is missing {rel}")

    def test_bootstrap_uses_npm_ci_not_an_offline_cache(self):
        """A fresh machine has no npm cache, so the dependency install
        must be allowed to reach the registry."""
        canonical = self._stage_adapter_without_toolchain()
        self._install_fake_npm()

        self.assertEqual(self._run_install(canonical).returncode, 0)

        calls = self._npm_calls()
        self.assertIn("ci", calls)
        self.assertNotIn("--offline", calls,
                         "the installer assumed a populated npm cache")

    def test_toolchain_is_not_reinstalled_when_the_compiler_is_present(self):
        """A repeat install must not reach the network at all."""
        canonical = self._stage_adapter_without_toolchain()
        self._install_fake_npm()
        self.assertEqual(self._run_install(canonical).returncode, 0)
        self.npm_log.unlink(missing_ok=True)

        self.assertEqual(self._run_install(canonical).returncode, 0)

        self.assertEqual(
            self._npm_calls(), "",
            "npm was invoked again although the compiler was already present",
        )

    # -- diagnostics: the failure modes must be named, not swallowed --
    #
    # Each diagnostic must name ITS OWN cause. The pre-fix installer
    # failed every one of these with the single generic message
    # "TypeScript compiler not found ... run 'npm ci'", which mentions
    # both "node" and "npm" while saying nothing about the real cause.
    # So each assertion below also requires that the generic message is
    # ABSENT — otherwise the test would pass against the very defect it
    # is meant to pin.

    def test_missing_node_is_reported_and_registers_nothing(self):
        """No Node on the machine at all."""
        canonical = self._stage_adapter_without_toolchain()
        env = os.environ.copy()
        env["HOME"] = str(self.home)
        env.pop("MPM_WORKSPACE", None)
        # No bindir on PATH: node and npm are absent.
        env["PATH"] = BOOTSTRAP_PATH
        result = subprocess.run(
            ["bash", str(canonical / "install.sh")],
            capture_output=True, text=True, timeout=120, env=env,
        )
        output = result.stderr + result.stdout

        self.assertNotEqual(result.returncode, 0)
        self.assertRegex(
            output.lower(),
            r"node",
            "the diagnostic does not name the missing prerequisite",
        )
        self.assertNotIn(
            "TypeScript compiler not found", output,
            "the diagnostic blames the compiler instead of the absent "
            "Node runtime",
        )
        cfg = self.home / ".config" / "opencode" / "opencode.jsonc"
        self.assertFalse(cfg.exists(),
                         "a registration was written without a payload")

    def test_unreachable_registry_is_reported_and_registers_nothing(self):
        """The dependency install fails (no network, no cache)."""
        canonical = self._stage_adapter_without_toolchain()
        self._install_fake_npm(fail=True)

        result = self._run_install(canonical)
        output = result.stderr + result.stdout

        self.assertNotEqual(result.returncode, 0)
        self.assertRegex(
            output.lower(),
            r"npm",
            "the diagnostic does not name the failed dependency install",
        )
        self.assertNotIn(
            "TypeScript compiler not found", output,
            "the diagnostic blames the compiler instead of the failed "
            "dependency install",
        )
        cfg = self.home / ".config" / "opencode" / "opencode.jsonc"
        self.assertFalse(cfg.exists(),
                         "a registration was written without a payload")

    def test_compile_failure_is_reported_and_registers_nothing(self):
        """The dependencies installed but the compiler fails."""
        canonical = self._stage_adapter_without_toolchain()
        self._install_fake_npm()
        # The compiler npm installs is a failing one.
        self._tsc_fail = True

        result = self._run_install(canonical)
        output = result.stderr + result.stdout

        self.assertNotEqual(result.returncode, 0)
        self.assertRegex(
            output.lower(),
            r"(compile|tsc)",
            "the diagnostic does not name the failed compilation",
        )
        self.assertNotIn(
            "TypeScript compiler not found", output,
            "the diagnostic blames a missing compiler instead of the "
            "compiler that ran and failed",
        )
        cfg = self.home / ".config" / "opencode" / "opencode.jsonc"
        self.assertFalse(cfg.exists(),
                         "a registration was written despite a failed build")


class OpenCodeProvisioningThroughTheGenericReconciler(unittest.TestCase):
    """The installation path that was broken, exercised end to end.

    Until 2026-10-24 the root installer reached adapters ONLY through
    the managed-block installer named in `reconcile.json`. For OpenCode
    that was `scripts/install_agents_instructions.py` — which writes
    ~/.config/opencode/AGENTS.md and nothing else. The adapter's own
    install.sh, which builds the payload, stages it and registers it,
    was never invoked by ANY installation path.

    The consequence on a real machine: a fresh install produced a
    registration and a managed block, and NO plugin payload. OpenCode
    resolves a directory plugin by a root-level server.*/index.* module
    and drops a directory without one SILENTLY, so the plugin loaded
    nothing while every install reported success.

    These tests drive the REAL generic reconciler against the REAL
    adapter in a hermetic HOME: no manual compilation, no manual npm
    step, and a plugin that is actually loadable afterwards.
    """

    def setUp(self):
        self._tmp = Path(tempfile.mkdtemp(prefix="mpm-opencode-reconcile-"))
        self.addCleanup(lambda: shutil.rmtree(self._tmp, ignore_errors=True))
        self.home = self._tmp / "home"
        self.home.mkdir()
        self.bindir = self._tmp / "bin"
        self.bindir.mkdir()
        self.npm_log = self._tmp / "npm-calls.log"
        # The runtime root is the same tree as the source in a live
        # install (~/.mpm IS the checkout), so the sandbox mirrors it.
        self.runtime_root = self.home / ".mpm"
        self.adapter = self.runtime_root / "agent_installation" / "mpm-opencode"
        shutil.copytree(
            ADAPTER_DIR,
            self.adapter,
            ignore=shutil.ignore_patterns("node_modules", "dist"),
        )
        _stage_sibling_scripts(self.adapter)
        # The reconciler discovers adapters as a sibling of scripts/.
        self.reconciler = (
            self.adapter.parent / "scripts" / "reconcile_managed_blocks.py"
        )
        # OpenCode is present on this machine (the gate the manifest
        # declares): ~/.config/opencode exists.
        (self.home / ".config" / "opencode").mkdir(parents=True)

    def _install_fake_toolchain(self) -> None:
        npm = self.bindir / "npm"
        npm.write_text(
            FAKE_NPM_BODY.replace("@@TSC_BODY@@", FAKE_TSC_BODY)
        )
        npm.chmod(0o755)
        node = self.bindir / "node"
        node.write_text("#!/usr/bin/env bash\nexit 0\n")
        node.chmod(0o755)

    def _run_reconciler(self):
        env = os.environ.copy()
        env["HOME"] = str(self.home)
        env.pop("MPM_WORKSPACE", None)
        env["PATH"] = f"{self.bindir}:{BOOTSTRAP_PATH}"
        env["MPM_TEST_NPM_LOG"] = str(self.npm_log)
        return subprocess.run(
            [sys.executable, str(self.reconciler), "--home", str(self.home)],
            capture_output=True, text=True, timeout=180, env=env,
        )

    def test_the_installation_path_provisions_a_usable_plugin(self):
        """No manual compilation, no manual npm step: the installation
        path produces a plugin OpenCode can actually load."""
        self._install_fake_toolchain()

        result = self._run_reconciler()

        self.assertEqual(result.returncode, 0, result.stdout + result.stderr)

        # The payload was compiled by the installer itself.
        self.assertIn(
            "ci",
            (self.npm_log.read_text() if self.npm_log.exists() else ""),
            "the installer never obtained its build dependencies",
        )
        payload = self.adapter / "dist" / "index.v2.js"
        self.assertTrue(payload.exists(), "the plugin payload was not compiled")
        self.assertTrue(payload.stat().st_size > 0, "the payload is empty")

        # The registration points at the package directory, and that
        # package holds BOTH the root entry OpenCode resolves and the
        # payload behind it. A directory without server.js is dropped
        # silently by the loader.
        registered = self.adapter
        self.assertTrue(
            (registered / "server.js").exists(),
            "the registered package has no root entry",
        )
        self.assertTrue(
            (registered / "dist" / "index.v2.js").exists(),
            "the registered package has no payload",
        )

        config = self.home / ".config" / "opencode" / "opencode.jsonc"
        self.assertTrue(config.exists(), "no registration was written")
        self.assertIn(
            f"file://{registered}",
            config.read_text(),
            "the registration does not name the package directory",
        )

    def test_the_installation_path_records_the_build_provenance(self):
        """The deployed payload is tied to a source revision."""
        self._install_fake_toolchain()

        result = self._run_reconciler()

        self.assertEqual(result.returncode, 0, result.stdout + result.stderr)
        sidecar = self.adapter / "dist" / "mpm-build-provenance.json"
        self.assertTrue(
            sidecar.exists(),
            "the deployed payload carries no build provenance",
        )
        data = json.loads(sidecar.read_text())
        self.assertIn("source_commit", data)
        self.assertIn("payload_sha256", data)
        self.assertEqual(
            data["payload_sha256"],
            hashlib.sha256(
                (self.adapter / "dist" / "index.v2.js").read_bytes()
            ).hexdigest(),
            "the recorded hash is not the deployed payload's hash",
        )

    def test_the_manifest_declares_the_adapters_own_installer(self):
        """The provision block must name the adapter's own installer —
        the one that builds, stages and registers."""
        manifest = json.loads(
            (self.adapter / "reconcile.json").read_text()
        )
        provision = manifest.get("provision")
        self.assertIsNotNone(
            provision,
            "the OpenCode manifest declares no provisioning step",
        )
        self.assertEqual(provision.get("installer"), "install.sh")
        self.assertTrue(
            (self.adapter / "install.sh").is_file(),
            "the named provisioner does not exist",
        )
        self.assertTrue(
            provision.get("required"),
            "a provisioning failure would not be reported as a failure",
        )


if __name__ == "__main__":
    unittest.main()
