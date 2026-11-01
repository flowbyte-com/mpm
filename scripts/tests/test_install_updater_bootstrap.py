#!/usr/bin/env python3
"""test_install_updater_bootstrap.py — the first-installation bootstrap.

THE GAP THIS FILE CLOSES
------------------------

A versioned bundle carries a real, standalone updater at
``<data root>/mpm-update``: the executable an operator runs when the
active runtime is broken. It must be a REAL FILE, not a symlink into the
active release, because ``<data root>/bin`` IS a symlink into the active
release — a recovery tool placed there dies with the runtime it is meant
to repair.

The installer used to promote only the five substrate binaries into the
candidate bundle, so ``releases/<rev>/bin/mpm-update`` never existed and
``<data root>/mpm-update`` was never created. Nothing repaired it later
either: the update workflow provisions its stable copy only on a
successful activation, and an ALREADY-CURRENT update returns before that
step. A first installation therefore ended with no recovery path at all,
and every idempotent re-run preserved the defect.

WHAT IS PINNED HERE
-------------------

1. a first bundle-mode installation provisions ``<data root>/mpm-update``
   as a real, executable file, byte-identical to its release's copy;
2. it is NOT a symlink, and a pre-existing symlink is replaced by a real
   file rather than preserved;
3. a second installation refreshes it to the new release;
4. a promotion that fails at ANY step leaves the previous executable
   byte-identical (and, on a first install, absent);
5. a failure at the provisioning step itself rolls the active pointer
   back to the previous runtime;
6. a build without ``mpm-update`` is refused before anything is
   activated, rather than silently producing a bundle with no recovery
   tool;
7. ``rb_provision_stable_updater`` refuses a source it cannot validate
   and leaves the existing executable untouched.

HERMETICITY
-----------

Every root is a fresh mktemp directory, HOME is redirected into it, and
nothing resolves into the operator's real ``~/.mpm``. The binaries are
shell stubs: this file is about which files exist and what they contain,
not about what the runtime does.
"""

import os
import re
import shutil
import sqlite3
import subprocess
import tempfile
import textwrap
import unittest
from pathlib import Path

REPO_ROOT = Path(__file__).resolve().parent.parent.parent
INSTALL_SH = REPO_ROOT / "install.sh"

# The five substrate binaries. The updater is handled separately because
# its presence is the thing under test.
CORE_BINARIES = ("mpm", "mpm-scheduler", "mpm-critic", "mpm-mcp", "mpm-telemetry")
UPDATER = "mpm-update"

FAKE_COMMIT = "1a2b3c4d5e6f70819a2b3c4d5e6f70819a2b3c4d"

# phase_bundle steps that run BEFORE the standalone provisioning step.
# A failure at any of them must leave the recovery executable exactly as
# it was: provisioning happens last, mirroring the update workflow's rule
# that a release must prove itself before it becomes the recovery tool.
STEPS_BEFORE_UPDATER = (
    "assemble",
    "promote",
    "assets",
    "payload",
    "identity",
    "validate",
    "install",
    "activate",
    "convert",
)

STRIP_PATTERNS = (
    re.compile(r"^\s*readonly\s+SCRIPT_DIR\s*="),
    re.compile(r"^\s*readonly\s+PROJECT_ROOT\s*="),
    re.compile(r'^\s*main\s+"\$@"\s*$'),
)


def build_sourced_lib(install_text: str) -> str:
    out = []
    for line in install_text.splitlines():
        if any(p.match(line) for p in STRIP_PATTERNS):
            continue
        out.append(line)
    return "\n".join(out) + "\n"


class BootstrapFixture(unittest.TestCase):
    """A disposable MPM installation plus a fake build tree."""

    def setUp(self):
        self._tmp = Path(tempfile.mkdtemp(prefix="mpm-updater-bootstrap-"))
        self.addCleanup(shutil.rmtree, self._tmp, ignore_errors=True)

        self.home = self._tmp / "home"
        self.home.mkdir()
        self.project = self._tmp / "project"
        self.project.mkdir()
        self.data = self.project  # the canonical colocated layout
        self.prefix = self.project
        self.build_dir = self.project / ".build" / "bin"
        self.build_dir.mkdir(parents=True)

        self._make_binaries()
        self._make_checkout()
        self._make_layout()

    # ------------------------------------------------------------------
    # fixture
    # ------------------------------------------------------------------
    def _make_binaries(self, include_updater: bool = True):
        for name in CORE_BINARIES:
            p = self.build_dir / name
            if name == "mpm":
                # The `mpm` binary answers --version with the commit the
                # bundle validator cross-checks against RELEASE.json.
                p.write_text(
                    "#!/usr/bin/env bash\n"
                    "if [ \"${1:-}\" = '--version' ]; then\n"
                    f"  echo 'MPM 0.1.0-dev+g{FAKE_COMMIT[:8]}'\n"
                    f"  echo '  commit:  {FAKE_COMMIT}'\n"
                    "  echo '  dirty:   no'\n"
                    "fi\n"
                    "exit 0\n"
                )
            else:
                p.write_bytes(b"\x7fEL" + name.encode())
            p.chmod(0o755)
        p = self.build_dir / UPDATER
        if include_updater:
            p.write_text("#!/usr/bin/env bash\necho 'mpm-update fake'\n")
            p.chmod(0o755)
        elif p.exists():
            p.unlink()

    def _make_checkout(self):
        lib = self.project / "scripts" / "lib"
        lib.mkdir(parents=True)
        for name in ("binary_transaction.sh", "runtime_bundle.sh", "schema_preflight.py"):
            shutil.copy(REPO_ROOT / "scripts" / "lib" / name, lib / name)
        shutil.copy(
            REPO_ROOT / "scripts" / "install_runtime_assets.py",
            self.project / "scripts" / "install_runtime_assets.py",
        )
        shutil.copy(
            REPO_ROOT / "scripts" / "runtime_payloads.json",
            self.project / "scripts" / "runtime_payloads.json",
        )
        shutil.copy(
            REPO_ROOT / "scripts" / "build-provenance.sh",
            self.project / "scripts" / "build-provenance.sh",
        )
        (self.project / "agent_installation" / "scripts").mkdir(parents=True)
        shutil.copy(
            REPO_ROOT / "agent_installation" / "scripts" / "stage_runtime_package.sh",
            self.project / "agent_installation" / "scripts" / "stage_runtime_package.sh",
        )
        core = self.project / "internal" / "core"
        core.mkdir(parents=True)
        shutil.copy(REPO_ROOT / "internal" / "core" / "schema.go", core / "schema.go")

        # A minimal, VALID OpenCode payload: the root entry and the V2
        # loader must exist and be non-empty, and no relative import may
        # dangle.
        adapter = self.project / "agent_installation" / "mpm-opencode"
        (adapter / "dist").mkdir(parents=True)
        (adapter / "server.js").write_text(
            "export default async function () { return { name: 'mpm' }; }\n"
        )
        for js in (
            "index.v2.js",
            "wake-context.js",
            "event-envelope.js",
            "event-mapper.js",
            "event-subscriber.js",
            "workspace.js",
            "resolve-mpm-binary.js",
        ):
            (adapter / "dist" / js).write_text(f"// {js}\nexport const name = '{js}';\n")
        (adapter / "package.json").write_text('{"name":"mpm-opencode"}\n')

        (self.project / "install.sh.lib").write_text(
            build_sourced_lib(INSTALL_SH.read_text())
        )

    def _make_layout(self):
        """A checkout-style installation: real directories, real state."""
        (self.data / "bin").mkdir()
        for name in CORE_BINARIES:
            shutil.copy(self.build_dir / name, self.data / "bin" / name)
        (self.data / "agent_installation").mkdir(exist_ok=True)
        (self.data / "agent_installation" / "adapter").mkdir()

        db_dir = self.data / "src" / "db"
        db_dir.mkdir(parents=True)
        self.db_path = db_dir / "mpm.db"
        conn = sqlite3.connect(self.db_path)
        conn.execute("CREATE TABLE memories (id TEXT PRIMARY KEY, content TEXT)")
        conn.execute("INSERT INTO memories VALUES ('m1', 'operator memory')")
        conn.commit()
        conn.close()
        self.config_path = self.data / "mpm_config.json"
        self.config_path.write_text('{"profile": "synthetic"}\n')
        (self.data / "mode").mkdir()
        (self.data / "mode" / "default.md").write_text("the operator's mode\n")
        (self.data / "persona").mkdir()
        (self.data / "persona" / "default.md").write_text("the operator's persona\n")

    # ------------------------------------------------------------------
    # driver
    # ------------------------------------------------------------------
    def drive(self, phases: str = "phase_bundle", rev: str | None = None,
              env_extra: dict | None = None,
              use_strace: bool = False) -> subprocess.CompletedProcess:
        driver = self.project / "_drive.sh"
        rev_line = f'BUNDLE_REV="{rev}"' if rev else ""
        driver.write_text(
            textwrap.dedent(f"""\
                #!/usr/bin/env bash
                set -uo pipefail
                export PROJECT_ROOT="{self.project}"
                source "{self.project}/install.sh.lib"
                BUILD_DIR="{self.build_dir}"
                PREFIX="{self.prefix}"
                DATA_ROOT="{self.data}"
                SERVICE_DST=/dev/null
                export BUILD_DIR PREFIX DATA_ROOT SERVICE_DST
                {rev_line}
                {phases}
            """)
        )
        driver.chmod(0o755)

        env = dict(os.environ)
        env["HOME"] = str(self.home)
        env.pop("MPM_WORKSPACE", None)
        env.pop("MPM_DB_PATH", None)
        if env_extra:
            env.update({k: str(v) for k, v in env_extra.items()})
        cmd = ["bash", str(driver)]
        if use_strace:
            cmd = [
                "strace", "-f", "-qq",
                "-e", "trace=rename,renameat,renameat2,unlink,unlinkat",
                "-o", str(self._tmp / "syscalls.log"),
            ] + cmd
        return subprocess.run(
            cmd, capture_output=True, text=True, timeout=300, env=env
        )

    def syscall_log(self) -> str:
        p = self._tmp / "syscalls.log"
        return p.read_text(errors="replace") if p.is_file() else ""

    # ------------------------------------------------------------------
    # assertions helpers
    # ------------------------------------------------------------------
    def standalone_path(self) -> Path:
        return self.data / UPDATER

    def current_target(self) -> str | None:
        link = self.data / "current"
        return os.readlink(link) if link.is_symlink() else None

    def release_updater(self, rev: str) -> Path:
        return self.data / "releases" / rev / "bin" / UPDATER

    def assert_provisioned_real_file(self, path: Path, msg: str = ""):
        self.assertTrue(path.exists(), f"{msg}: {path} does not exist")
        self.assertFalse(path.is_symlink(), f"{msg}: {path} is a symlink")
        self.assertTrue(path.is_file(), f"{msg}: {path} is not a regular file")
        self.assertTrue(os.access(path, os.X_OK), f"{msg}: {path} is not executable")

    def state_fingerprint(self) -> dict:
        fp = {}
        for rel in ("src/db/mpm.db", "mpm_config.json", "mode/default.md",
                    "persona/default.md"):
            p = self.data / rel
            fp[rel] = p.read_bytes() if p.is_file() else None
        return fp


# ===========================================================================
# 1 — a first bundle-mode installation provisions the recovery executable
# ===========================================================================


class TestFirstInstallProvisionsRecoveryExecutable(BootstrapFixture):
    def test_the_standalone_updater_is_a_real_executable(self):
        before = self.state_fingerprint()
        r = self.drive()
        self.assertEqual(r.returncode, 0, r.stdout + r.stderr)

        path = self.standalone_path()
        self.assert_provisioned_real_file(path, "after a first bundle install")

        # The operator's data is untouched by the promotion.
        self.assertEqual(self.state_fingerprint(), before,
                         "the bootstrap changed the persistent state")

        # And the log says so, so an operator reading the install output
        # learns where the recovery tool is.
        self.assertIn("recovery", (r.stdout + r.stderr).lower())

    def test_it_is_byte_identical_to_the_release_copy(self):
        r = self.drive()
        self.assertEqual(r.returncode, 0, r.stdout + r.stderr)
        rev = self.current_target().split("/", 1)[1]

        self.assert_provisioned_real_file(self.release_updater(rev),
                                          "the release's own copy")
        self.assertEqual(
            self.standalone_path().read_bytes(),
            self.release_updater(rev).read_bytes(),
            "the standalone updater must be the release's updater, byte for byte",
        )

    def test_it_is_not_a_symlink_into_the_active_release(self):
        r = self.drive()
        self.assertEqual(r.returncode, 0, r.stdout + r.stderr)
        path = self.standalone_path()
        self.assertFalse(path.is_symlink(),
                         "the recovery executable must not be a symlink: bin/ is a "
                         "symlink into the active release, and a recovery tool that "
                         "dies with the runtime is not a recovery tool")

    def test_an_existing_symlink_is_replaced_by_a_real_file(self):
        # A wrong earlier state: <data root>/mpm-update pointing into a
        # release. Provisioning must replace the LINK, not follow it.
        bogus_target = self.data / "releases" / "old" / "bin" / UPDATER
        bogus_target.parent.mkdir(parents=True)
        bogus_target.write_text("#!/usr/bin/env bash\nexit 0\n")
        bogus_target.chmod(0o755)
        self.standalone_path().symlink_to(bogus_target)
        self.assertTrue(self.standalone_path().is_symlink())

        r = self.drive()
        self.assertEqual(r.returncode, 0, r.stdout + r.stderr)
        self.assert_provisioned_real_file(self.standalone_path(),
                                          "after replacing a symlink")


# ===========================================================================
# 2 — a subsequent installation refreshes it
# ===========================================================================


class TestSubsequentInstallRefreshesTheExecutable(BootstrapFixture):
    def test_a_second_install_replaces_it_with_the_new_release_copy(self):
        first = self.drive(rev="rev-one")
        self.assertEqual(first.returncode, 0, first.stdout + first.stderr)
        self.assertEqual(self.current_target(), "releases/rev-one")
        first_bytes = self.standalone_path().read_bytes()

        # A second release, carrying a different updater payload.
        self.build_dir.joinpath(UPDATER).write_text(
            "#!/usr/bin/env bash\necho 'mpm-update rev-two'\n"
        )
        self.build_dir.joinpath(UPDATER).chmod(0o755)
        second = self.drive(rev="rev-two")
        self.assertEqual(second.returncode, 0, second.stdout + second.stderr)

        self.assertEqual(self.current_target(), "releases/rev-two")
        self.assert_provisioned_real_file(self.standalone_path(),
                                          "after the second install")
        self.assertNotEqual(self.standalone_path().read_bytes(), first_bytes,
                            "a version switch must refresh the recovery executable")
        self.assertEqual(self.standalone_path().read_bytes(),
                         self.release_updater("rev-two").read_bytes())


# ===========================================================================
# 3 — a failure before the provisioning step changes nothing
# ===========================================================================


class TestFailureLeavesTheRecoveryExecutableIntact(BootstrapFixture):
    def test_every_step_before_provisioning_preserves_the_previous_executable(self):
        first = self.drive(rev="rev-one")
        self.assertEqual(first.returncode, 0, first.stdout + first.stderr)
        kept = self.standalone_path().read_bytes()

        # Give the second candidate a DIFFERENT updater, so a premature
        # refresh would be visible.
        self.build_dir.joinpath(UPDATER).write_text(
            "#!/usr/bin/env bash\necho 'mpm-update rev-two'\n"
        )
        self.build_dir.joinpath(UPDATER).chmod(0o755)

        for step in STEPS_BEFORE_UPDATER:
            with self.subTest(step=step):
                r = self.drive(rev=f"rev-two-{step}",
                               env_extra={"MPM_INSTALL_TEST_FAIL_AT": step})
                self.assertNotEqual(r.returncode, 0,
                                    f"the injected failure at '{step}' did not stop the install")
                self.assertIn(f"injected failure after '{step}'", r.stderr)
                self.assertEqual(
                    self.standalone_path().read_bytes(), kept,
                    f"a failure at '{step}' replaced the known-good recovery executable",
                )
                self.assert_provisioned_real_file(
                    self.standalone_path(),
                    f"after a failure at '{step}'",
                )

    def test_a_first_install_that_fails_creates_no_executable(self):
        for step in STEPS_BEFORE_UPDATER:
            with self.subTest(step=step):
                # A fresh fixture per step: this is about the FIRST
                # installation, where there is no previous tool to keep.
                self.setUp()
                r = self.drive(env_extra={"MPM_INSTALL_TEST_FAIL_AT": step})
                self.assertNotEqual(r.returncode, 0)
                self.assertFalse(
                    self.standalone_path().exists(),
                    f"a failed first install left a recovery executable behind (step '{step}')",
                )

    def test_a_failure_at_the_provisioning_step_rolls_the_pointer_back(self):
        first = self.drive(rev="rev-one")
        self.assertEqual(first.returncode, 0, first.stdout + first.stderr)
        self.assertEqual(self.current_target(), "releases/rev-one")

        r = self.drive(rev="rev-two", env_extra={"MPM_INSTALL_TEST_FAIL_AT": "updater"})
        self.assertNotEqual(r.returncode, 0)
        self.assertIn("injected failure after 'updater'", r.stderr)

        # The pointer names the runtime that was running before.
        self.assertEqual(self.current_target(), "releases/rev-one",
                         "a failure at the provisioning step must roll the pointer back")

    def test_a_failure_at_the_provisioning_step_restores_the_PREVIOUS_executable(self):
        """A rolled-back promotion must put the OLD recovery tool back.

        Asserting "an executable exists" is not enough: the whole point of
        the rollback is that the executable belongs to the runtime that is
        active again. A new-release updater left behind after the pointer
        moved back is a recovery tool for a runtime that is no longer
        running.
        """
        # The first install establishes a distinctive previous updater.
        self.build_dir.joinpath(UPDATER).write_text(
            "#!/usr/bin/env bash\necho 'mpm-update rev-one'\n"
        )
        self.build_dir.joinpath(UPDATER).chmod(0o755)
        first = self.drive(rev="rev-one")
        self.assertEqual(first.returncode, 0, first.stdout + first.stderr)
        previous_bytes = self.standalone_path().read_bytes()
        self.assertIn(b"rev-one", previous_bytes)

        # A second release carries a DIFFERENT updater, then the promotion
        # is abandoned after provisioning.
        self.build_dir.joinpath(UPDATER).write_text(
            "#!/usr/bin/env bash\necho 'mpm-update rev-two'\n"
        )
        self.build_dir.joinpath(UPDATER).chmod(0o755)
        r = self.drive(rev="rev-two", env_extra={"MPM_INSTALL_TEST_FAIL_AT": "updater"})
        self.assertNotEqual(r.returncode, 0)

        self.assertEqual(self.current_target(), "releases/rev-one")
        self.assertEqual(
            self.standalone_path().read_bytes(), previous_bytes,
            "the rolled-back promotion left the NEW release's recovery executable in place",
        )
        self.assert_provisioned_real_file(self.standalone_path(),
                                          "after restoring the previous executable")
        self.assertFalse(
            (self.data / f".{UPDATER}.previous").exists(),
            "the rollback left its preserved copy behind",
        )
        self.assertFalse(
            (self.data / f".{UPDATER}.rollback").exists(),
            "the rollback left its journal behind",
        )

    def test_a_first_install_that_rolls_back_removes_the_new_executable(self):
        """On a first install the "previous" state is "no updater at all".

        Restoring it must REMOVE the executable, not keep the one the
        abandoned promotion created. Creating a recovery tool that no
        active runtime owns is not a neutral leftover: it is a tool whose
        provenance no longer exists.
        """
        r = self.drive(env_extra={"MPM_INSTALL_TEST_FAIL_AT": "updater"})
        self.assertNotEqual(r.returncode, 0)
        self.assertFalse(
            self.standalone_path().exists(),
            "a rolled-back FIRST install left a recovery executable behind",
        )
        self.assertIsNone(self.current_target())


# ===========================================================================
# 4 — a build without the updater is refused before activation
# ===========================================================================


class TestMissingUpdaterIsRefused(BootstrapFixture):
    def test_a_build_without_the_updater_cannot_be_activated(self):
        self._make_binaries(include_updater=False)
        before = self.state_fingerprint()

        r = self.drive()
        self.assertNotEqual(r.returncode, 0,
                            "a bundle with no updater must not be activated")
        self.assertIn(UPDATER, r.stderr)

        # Nothing was promoted or activated.
        self.assertIsNone(self.current_target())
        self.assertFalse(self.standalone_path().exists())
        self.assertEqual(self.state_fingerprint(), before)
        # And no stale candidate is left behind.
        staging = self.data / "staging"
        if staging.is_dir():
            leftovers = [p for p in staging.iterdir() if p.name != ".keep"]
            self.assertEqual(leftovers, [], "a refused promotion left a candidate behind")

    def test_the_bundle_validator_names_the_missing_updater(self):
        # The validator is the layer that makes the refusal specific:
        # "missing bin/mpm-update" plus why it matters.
        bundle = self.data / "hand-built-bundle"
        (bundle / "bin").mkdir(parents=True)
        for name in CORE_BINARIES:
            p = bundle / "bin" / name
            p.write_bytes(b"x")
            p.chmod(0o755)
        (bundle / "RELEASE.json").write_text(
            '{"revision":"abc1234","commit":"' + FAKE_COMMIT + '"}\n'
        )
        # The driver sources install.sh.lib, which (re-)enables `set -e`;
        # a direct call to a function that RETURNS non-zero would abort
        # the shell before the rc could be echoed.
        r = self.drive(
            "set +e\n"
            "load_bundle_lib || exit 9\n"
            f'rb_validate_bundle "{bundle}"; echo "rc=$?"'
        )
        self.assertIn("rc=1", r.stdout, r.stdout + r.stderr)
        self.assertIn(f"missing bin/{UPDATER}", r.stderr)
        self.assertIn("recovery", r.stderr.lower())


# ===========================================================================
# 5 — the provisioning primitive itself
# ===========================================================================


class TestProvisioningPrimitive(BootstrapFixture):
    def drive_lib(self, script: str) -> subprocess.CompletedProcess:
        return self.drive(
            "set +e\nload_bundle_lib || exit 9\n" + script
        )

    def test_it_refuses_a_release_with_no_updater_and_changes_nothing(self):
        release = self.data / "releases" / "empty" / "bin"
        release.mkdir(parents=True)
        existing = self.standalone_path()
        existing.write_bytes(b"the known-good updater")
        existing.chmod(0o755)

        r = self.drive_lib(
            f'rb_provision_stable_updater "{self.data}" "{release.parent}" "empty"; echo "rc=$?"'
        )
        self.assertIn("rc=1", r.stdout, r.stdout + r.stderr)
        self.assertIn(UPDATER, r.stderr)
        self.assertEqual(existing.read_bytes(), b"the known-good updater",
                         "a refused provisioning must not touch the existing executable")
        self.assertTrue(os.access(existing, os.X_OK))

    def test_it_refuses_a_non_executable_source(self):
        release = self.data / "releases" / "bad" / "bin"
        release.mkdir(parents=True)
        src = release / UPDATER
        src.write_text("#!/usr/bin/env bash\nexit 0\n")
        src.chmod(0o644)

        r = self.drive_lib(
            f'rb_provision_stable_updater "{self.data}" "{release.parent}" "bad"; echo "rc=$?"'
        )
        self.assertIn("rc=1", r.stdout, r.stdout + r.stderr)
        self.assertIn("not executable", r.stderr)
        self.assertFalse(self.standalone_path().exists())

    # -- atomicity ---------------------------------------------------
    #
    # The recovery executable must never be absent. Moving the live file
    # aside before moving the new one in — which is what this used to do —
    # leaves a window in which the operator whose runtime just failed runs
    # a path that does not exist. The window is microseconds, so a polling
    # test would be a coin flip; the syscall trace is exact.

    def _displacements(self, dest: str) -> list[str]:
        """Syscalls that take the LIVE executable away.

        A rename FROM the destination, or an unlink OF it, is a
        displacement: the path stops existing for some interval. A rename
        TO the destination is the correct atomic replacement and is not
        reported.
        """
        bad = []
        for line in self.syscall_log().splitlines():
            m = re.search(r"\b(rename|renameat2?|unlink|unlinkat)\((.*)\)\s*=", line)
            if not m:
                continue
            syscall = m.group(1)
            strings = re.findall(r'"((?:[^"\\]|\\.)*)"', m.group(2))
            if not strings:
                continue
            # unlink/unlinkat: the path is the first string.
            # rename/renameat*: the SOURCE is the first string.
            if strings[0] == dest:
                bad.append(f"{syscall}: {line.strip()}")
        return bad

    @unittest.skipUnless(shutil.which("strace"), "strace is not available")
    def test_the_live_executable_is_never_displaced(self):
        # An existing executable, so the replacement has something to
        # preserve.
        first = self.drive(rev="rev-one")
        self.assertEqual(first.returncode, 0, first.stdout + first.stderr)

        release = self.data / "releases" / "rev-two" / "bin"
        release.mkdir(parents=True)
        src = release / UPDATER
        src.write_text("#!/usr/bin/env bash\necho 'rev-two'\n")
        src.chmod(0o755)

        r = self.drive(
            "set +e\n"
            "load_bundle_lib || exit 9\n"
            f'rb_provision_stable_updater "{self.data}" "{release.parent}" "rev-two"; echo "rc=$?"',
            use_strace=True,
        )
        self.assertIn("rc=0", r.stdout, r.stdout + r.stderr)

        dest = str(self.standalone_path())
        displaced = self._displacements(dest)
        self.assertEqual(
            displaced, [],
            "the live recovery executable was moved or unlinked rather than replaced "
            "atomically; there was an interval in which it did not exist:\n  "
            + "\n  ".join(displaced),
        )

    def test_the_previous_executable_is_preserved_before_the_swap(self):
        """Both files exist after provisioning: the replacement is a swap,
        not a displacement, and the rollback material is present."""
        first = self.drive(rev="rev-one")
        self.assertEqual(first.returncode, 0, first.stdout + first.stderr)
        old_bytes = self.standalone_path().read_bytes()

        release = self.data / "releases" / "rev-two" / "bin"
        release.mkdir(parents=True)
        src = release / UPDATER
        src.write_text("#!/usr/bin/env bash\necho 'rev-two'\n")
        src.chmod(0o755)

        r = self.drive(
            "set +e\n"
            "load_bundle_lib || exit 9\n"
            f'rb_provision_stable_updater "{self.data}" "{release.parent}" "rev-two"; echo "rc=$?"'
        )
        self.assertIn("rc=0", r.stdout, r.stdout + r.stderr)

        # The live file is the new one...
        self.assert_provisioned_real_file(self.standalone_path(), "after provisioning")
        self.assertIn(b"rev-two", self.standalone_path().read_bytes())
        # ...and the old one is still here, un-moved, as rollback material.
        backup = self.data / f".{UPDATER}.previous"
        self.assertTrue(backup.exists(), "the previous executable was not preserved")
        self.assertFalse(backup.is_symlink())
        self.assertEqual(backup.read_bytes(), old_bytes,
                         "the preserved copy is not the previous executable")
        journal = self.data / f".{UPDATER}.rollback"
        self.assertTrue(journal.is_file(), "no rollback journal was written")
        self.assertEqual(journal.read_text().strip(), "previous")

    def test_commit_clears_the_rollback_material(self):
        r = self.drive()
        self.assertEqual(r.returncode, 0, r.stdout + r.stderr)
        for name in (f".{UPDATER}.previous", f".{UPDATER}.rollback"):
            self.assertFalse((self.data / name).exists(),
                             f"a successful install left {name} behind")

    def test_it_leaves_no_staging_residue_on_success(self):
        r = self.drive()
        self.assertEqual(r.returncode, 0, r.stdout + r.stderr)
        residue = sorted(
            p.name for p in self.data.iterdir()
            if p.name.startswith(".mpm-update")
        )
        self.assertEqual(residue, [],
                         f"provisioning left staging residue in the data root: {residue}")



# ===========================================================================
# 6 — the post-install validation predicate
# ===========================================================================


class TestValidationPredicate(BootstrapFixture):
    """`install.sh --validate` must reject every unusable recovery tool.

    The predicate is exercised directly rather than through the full
    validation phase: that phase also probes systemd and the live
    database, which would make this a test of the host rather than of
    the rule.
    """

    def check(self, path: Path) -> tuple[str, str]:
        r = self.drive(
            "set +e\n"
            f'recovery_executable_problem "{path}"; echo "<<END>>"'
        )
        out = r.stdout
        self.assertIn("<<END>>", out, out + r.stderr)
        return out.split("<<END>>")[0].strip(), r.stderr

    def test_a_real_executable_is_accepted(self):
        good = self.data / "good-updater"
        good.write_text("#!/bin/sh\nexit 0\n")
        good.chmod(0o755)
        problem, _ = self.check(good)
        self.assertEqual(problem, "", f"a real executable must pass, got {problem!r}")

    def test_a_symlink_is_rejected_even_when_it_resolves(self):
        target = self.data / "releases" / "r1" / "bin" / UPDATER
        target.parent.mkdir(parents=True)
        target.write_text("#!/bin/sh\nexit 0\n")
        target.chmod(0o755)
        link = self.data / "linked-updater"
        link.symlink_to(target)
        self.assertTrue(os.access(link, os.X_OK), "fixture: the link must resolve")

        problem, _ = self.check(link)
        self.assertIn("SYMLINK", problem)

    def test_a_missing_file_is_rejected(self):
        problem, _ = self.check(self.data / "no-such-updater")
        self.assertIn("missing", problem)

    def test_a_non_executable_file_is_rejected(self):
        p = self.data / "inert-updater"
        p.write_text("#!/bin/sh\nexit 0\n")
        p.chmod(0o644)
        problem, _ = self.check(p)
        self.assertIn("not executable", problem)


if __name__ == "__main__":
    unittest.main()
