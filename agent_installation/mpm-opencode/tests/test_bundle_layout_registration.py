# test_bundle_layout_registration.py — the plugin registration under the
# versioned-bundle layout.
#
# WHAT THIS FILE PINS
# --------------------
# The versioned-bundle layout makes `~/.mpm/agent_installation` a SYMLINK
# into the active release:
#
#     ~/.mpm/agent_installation -> current/agent_installation
#                                -> releases/<rev>/agent_installation
#
# The adapter's installer computed the registration it writes into
# `opencode.jsonc` with
#
#     CANONICAL_ADAPTER_DIR="$(cd "$staged" && pwd -P)"
#
# and `pwd -P` resolves symlinks. Under the CHECKOUT-STYLE layout
# (`~/.mpm/agent_installation` a real directory) that resolved to the
# stable path and everything was fine. Under the BUNDLE layout it
# resolves to the REVISION-PINNED physical path, so the installer
# appended a second, revision-pinned entry beside the stable one that
# the config already carried.
#
# Two entries, two different strings, ONE directory: OpenCode would load
# the same plugin twice. And the revision-pinned spelling would break at
# the next upgrade.
#
# The registration must be the STABLE path:
#
#     file://<data root>/agent_installation/mpm-opencode
#
# because that is the path the versioned layout exists to provide — it
# rolls with the release, and every registration that names `~/.mpm`
# keeps naming the same place.
#
# EVERY test runs against a disposable temporary tree. The live
# installation is never touched.
import json
import os
import shutil
import subprocess
import sys
import tempfile
import unittest
from pathlib import Path

HERE = Path(__file__).resolve().parent
ADAPTER_SRC = HERE.parent
INSTALL_SH = ADAPTER_SRC / "install.sh"

sys_path_entry = str(HERE.parent / "scripts")

FAKE_TSC_BODY = """#!/bin/sh
# stub compiler: create the artefacts install.sh expects
mkdir -p dist
for f in index.v2 wake-context event-envelope event-mapper event-subscriber workspace resolve-mpm-binary; do
  printf '// stub\\n' > "dist/$f.js"
done
exit 0
"""


def _stage_sibling_scripts(canonical: Path) -> None:
    """The adapter's install.sh sources scripts/ from its sibling."""
    src = ADAPTER_SRC / "scripts"
    dst = canonical / "scripts"
    dst.mkdir(parents=True, exist_ok=True)
    for item in src.iterdir():
        if item.is_file():
            shutil.copy2(item, dst / item.name)
        elif item.is_dir() and item.name != "__pycache__":
            shutil.copytree(item, dst / item.name, dirs_exist_ok=True)


def _stage_adapter(home: Path) -> Path:
    """Stage the adapter the way the installer expects to find it."""
    install_root = home / ".mpm" / "agent_installation"
    canonical = install_root / "mpm-opencode"
    if canonical.exists():
        shutil.rmtree(canonical)
    shutil.copytree(
        ADAPTER_SRC,
        canonical,
        ignore=shutil.ignore_patterns("node_modules", "dist", "__pycache__"),
    )
    nm = canonical / "node_modules"
    bin_dir = nm / ".bin"
    bin_dir.mkdir(parents=True, exist_ok=True)
    tsc_stub = bin_dir / "tsc"
    tsc_stub.write_text(FAKE_TSC_BODY)
    tsc_stub.chmod(0o755)
    subprocess.run([str(tsc_stub)], cwd=str(canonical),
                   capture_output=True, check=True)
    _stage_sibling_scripts(canonical)
    return canonical


def _bundle_layout(home: Path) -> Path:
    """Convert ~/.mpm into the versioned-bundle layout.

    Returns the release directory. The layout is exactly what the
    installer's rb_convert_layout produces:

        <data root>/agent_installation -> current/agent_installation
        <data root>/current -> releases/<rev>
    """
    data_root = home / ".mpm"
    rev = "aeec40f1f6f5edfbd07b1fd962b54de123d996a6"
    release_dir = data_root / "releases" / rev
    release_assets = release_dir / "agent_installation"
    release_assets.mkdir(parents=True, exist_ok=True)

    # Move the staged adapter into the release bundle.
    staged = data_root / "agent_installation" / "mpm-opencode"
    if staged.exists():
        shutil.move(str(staged), str(release_assets / "mpm-opencode"))

    # `current` -> releases/<rev>   (relative, as the installer writes it)
    current = data_root / "current"
    if current.exists() or current.is_symlink():
        current.unlink()
    os.symlink("releases/" + rev, current)

    # `agent_installation` -> current/agent_installation   (relative)
    ai = data_root / "agent_installation"
    if ai.exists() and not ai.is_symlink():
        shutil.rmtree(ai)
    os.symlink("current/agent_installation", ai)
    return release_dir


def _run(args, home: Path):
    env = os.environ.copy()
    env["HOME"] = str(home)
    return subprocess.run(
        ["bash", str(INSTALL_SH), *args],
        capture_output=True, text=True, timeout=120, env=env,
    )


def _plugins(home: Path):
    cfg = home / ".config" / "opencode" / "opencode.jsonc"
    if not cfg.exists():
        return None
    text = cfg.read_text(encoding="utf-8")
    # The config is JSONC; the adapter's own parser handles it. Strip
    # comments and trailing commas the same way it does.
    sys.path.insert(0, sys_path_entry)
    import jsonc_config
    data = jsonc_config.parse(
        jsonc_config.strip_trailing_commas(jsonc_config.strip_jsonc(text)))
    return data.get("plugins", [])


class BundleLayoutRegistrationTests(unittest.TestCase):
    """Under the versioned-bundle layout, ONE stable registration."""

    def setUp(self):
        self._tmp = tempfile.TemporaryDirectory(prefix="mpm-bundle-reg-")
        self.addCleanup(self._tmp.cleanup)
        self.home = Path(self._tmp.name) / "home"
        self.home.mkdir(parents=True)
        _stage_adapter(self.home)
        self.release_dir = _bundle_layout(self.home)
        self.data_root = self.home / ".mpm"

    def test_the_installer_registers_the_stable_path(self):
        """THE DEFECT: the registration must not be revision-pinned.

        Before the fix the installer wrote the physical release path,
        because `pwd -P` resolved the layout's symlink.
        """
        result = _run([], self.home)
        self.assertEqual(result.returncode, 0,
                         f"the install must succeed:\n{result.stdout}\n{result.stderr}")
        plugins = _plugins(self.home)
        self.assertIsNotNone(plugins, "the config must exist")
        self.assertEqual(
            len(plugins), 1,
            f"there must be exactly ONE registration, got {plugins!r}")
        expected = "file://" + str(self.data_root / "agent_installation" / "mpm-opencode")
        self.assertEqual(
            plugins[0], expected,
            "the registration must be the STABLE path — the one that rolls "
            "with the release. A revision-pinned path breaks at the next "
            "upgrade and, alongside the stable entry, loads the plugin "
            "twice.")

    def test_the_registration_resolves_into_the_active_release(self):
        """The stable path must actually resolve into the bundle."""
        _run([], self.home)
        plugins = _plugins(self.home)
        self.assertEqual(len(plugins), 1)
        resolved = os.path.realpath(plugins[0][len("file://"):])
        self.assertEqual(
            resolved, str(self.release_dir / "agent_installation" / "mpm-opencode"),
            "the stable registration must resolve into the active release — "
            "that is what the layout is for")

    def test_the_installer_is_idempotent(self):
        """A second install must not add a second registration."""
        _run([], self.home)
        first = _plugins(self.home)
        _run([], self.home)
        second = _plugins(self.home)
        self.assertEqual(
            first, second,
            "a repeated install must produce the same registration, not an "
            "accumulating list")

    def test_an_already_duplicated_config_is_collapsed(self):
        """The state the buggy install left behind must be repairable.

        An operator who has already been through the buggy install must
        not have to edit opencode.jsonc by hand.
        """
        stable = "file://" + str(self.data_root / "agent_installation" / "mpm-opencode")
        revision = "file://" + str(self.release_dir / "agent_installation" / "mpm-opencode")
        cfg_dir = self.home / ".config" / "opencode"
        cfg_dir.mkdir(parents=True, exist_ok=True)
        (cfg_dir / "opencode.jsonc").write_text(
            json.dumps({"plugins": [stable, revision]}, indent=2) + "\n",
            encoding="utf-8")

        result = _run([], self.home)
        self.assertEqual(result.returncode, 0,
                         f"the install must succeed:\n{result.stdout}\n{result.stderr}")
        plugins = _plugins(self.home)
        self.assertEqual(
            plugins, [stable],
            "the two spellings of one directory must collapse to ONE stable "
            f"registration. Got: {plugins!r}")

    def test_the_config_mode_is_preserved(self):
        """The writer must not widen an operator's restricted file."""
        cfg_dir = self.home / ".config" / "opencode"
        cfg_dir.mkdir(parents=True, exist_ok=True)
        cfg = cfg_dir / "opencode.jsonc"
        cfg.write_text(json.dumps({"plugins": []}, indent=2) + "\n",
                       encoding="utf-8")
        os.chmod(cfg, 0o600)
        _run([], self.home)
        mode = os.stat(cfg).st_mode & 0o777
        self.assertEqual(
            mode, 0o600,
            "the install must preserve the config's mode. The writer installs "
            "a NEW inode through os.replace(), so the original mode has to "
            "be copied onto the replacement.")

    def test_a_brand_new_config_is_created_private(self):
        """A config the installer CREATES must not be group- or world-readable.

        The config can name provider credentials and host tokens. A file
        the operator never chose to widen must not start life readable by
        anyone else — and it must not inherit whatever the ambient umask
        happens to be, because that is a property of the shell that ran
        the installer, not a decision anyone made about this file.

        The umask is set to 0 for the run, so the ambient default would
        be 0666 (world-readable). The created file must still be 0600.
        """
        cfg_dir = self.home / ".config" / "opencode"
        cfg = cfg_dir / "opencode.jsonc"
        self.assertFalse(cfg.exists(), "the config must not exist yet")

        previous_umask = os.umask(0)
        try:
            result = _run([], self.home)
        finally:
            os.umask(previous_umask)

        self.assertEqual(result.returncode, 0,
                         f"the install must succeed:\n{result.stdout}\n{result.stderr}")
        self.assertTrue(cfg.exists(), "the installer must create the config")
        mode = os.stat(cfg).st_mode & 0o777
        self.assertEqual(
            mode, 0o600,
            "a brand-new config must be created 0600 — private to the "
            "operator — not at the ambient umask's mode and not at the "
            "installer's own choice of a wider mode")


if __name__ == "__main__":
    unittest.main(verbosity=2)
