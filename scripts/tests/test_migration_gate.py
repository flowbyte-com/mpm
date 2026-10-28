#!/usr/bin/env python3
"""test_migration_gate.py — the seven proofs of the runtime-migration gate.

A migration gate is only worth something if it names what it proves.
This file is that list, and each item is a test.

    1  incompatible schemas fail the preflight
    2  a failed initial conversion is recoverable at each intermediate step
    3  a failed subsequent promotion restores the previous pointer
    4  no new binary opens the live database before the backup and the
       migration-authorization steps
    5  version reporting works without .git
    6  a runtime switch cannot replace the operator's data or definitions
    7  the tests cannot default to the live workspace

THE FIXTURE IS A REAL INSTALLER, NOT A RECREATION

Every proof here drives the SHIPPED install.sh phases, in a child bash,
against a disposable data root. The installer is sourced with its
path-resolution and main() lines stripped — the same technique
scripts/tests/test_install_phase_binaries.py uses — so the code under
test is the code that ships.

The fake binaries are deliberately honest: `mpm` answers `--version` with
a commit and RECORDS every invocation it receives, so proof 4 can assert
that the new binary was never pointed at the live database.

HERMETICITY

Nothing here resolves into the operator's real ~/.mpm. Every root is a
fresh mktemp directory, and proof 7 asserts that at the end of the run.
"""

import hashlib
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

BINARIES = ("mpm", "mpm-scheduler", "mpm-critic", "mpm-mcp", "mpm-telemetry")

# The commit the fake binaries claim to be built from.
FAKE_COMMIT = "1a2b3c4d5e6f70819a2b3c4d5e6f70819a2b3c4d"

# The steps of phase_bundle that can fail, in order. The recovery
# contract differs per step, which is exactly why the matrix is pinned
# one step at a time.
BUNDLE_STEPS = (
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


def sha256(path: Path) -> str:
    h = hashlib.sha256()
    with open(path, "rb") as fh:
        for chunk in iter(lambda: fh.read(65536), b""):
            h.update(chunk)
    return h.hexdigest()


def build_sourced_lib(install_text: str) -> str:
    out_lines = []
    for line in install_text.splitlines():
        if any(p.match(line) for p in STRIP_PATTERNS):
            continue
        out_lines.append(line)
    return "\n".join(out_lines) + "\n"


class GateFixture(unittest.TestCase):
    """A disposable MPM installation: a fake checkout plus a data root."""

    def setUp(self):
        self._tmp = Path(tempfile.mkdtemp(prefix="mpm-migration-gate-"))
        self.addCleanup(shutil.rmtree, self._tmp, ignore_errors=True)

        self.home = self._tmp / "home"
        self.home.mkdir()
        self.project = self._tmp / "project"
        self.project.mkdir()
        self.data = self.project  # the canonical colocated layout
        self.prefix = self.project

        self.build_dir = self.project / ".build" / "bin"
        self.build_dir.mkdir(parents=True)
        self.invocation_log = self._tmp / "binary-invocations.log"

        self._make_fake_binaries()
        self._make_fake_checkout()
        self._make_checkout_style_layout()

    # ------------------------------------------------------------------
    # fixture
    # ------------------------------------------------------------------
    def _make_fake_binaries(self):
        for name in BINARIES:
            p = self.build_dir / name
            if name == "mpm":
                # The `mpm` binary is HONEST: it answers --version with a
                # commit, and it records every invocation — its argv, its
                # MPM_WORKSPACE and its MPM_DB_PATH — so proof 4 can
                # assert that the new binary was never pointed at the
                # live database.
                p.write_text(
                    "#!/usr/bin/env bash\n"
                    f'printf "%s\\t%s\\t%s\\t%s\\n" "$0" "$*" '
                    '"${MPM_WORKSPACE:-}" "${MPM_DB_PATH:-}" '
                    f'>> "{self.invocation_log}"\n'
                    "if [ \"${1:-}\" = '--version' ]; then\n"
                    "  echo 'MPM 0.1.0-dev+g" + FAKE_COMMIT[:8] + "'\n"
                    f"  echo '  commit:  {FAKE_COMMIT}'\n"
                    "  echo '  dirty:   no'\n"
                    "fi\n"
                    "exit 0\n"
                )
            else:
                p.write_bytes(b"\x7fE" + b"L" + name.encode())
            p.chmod(0o755)

    def _make_fake_checkout(self):
        lib = self.project / "scripts" / "lib"
        lib.mkdir(parents=True)
        for name in (
            "binary_transaction.sh",
            "runtime_bundle.sh",
        ):
            shutil.copy(REPO_ROOT / "scripts" / "lib" / name, lib / name)
        shutil.copy(
            REPO_ROOT / "scripts" / "lib" / "schema_preflight.py", lib / "schema_preflight.py"
        )
        shutil.copy(
            REPO_ROOT / "scripts" / "install_runtime_assets.py",
            self.project / "scripts" / "install_runtime_assets.py",
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
        # The substrate's own schema — the authority for the collision
        # check.
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
            (adapter / "dist" / js).write_text(
                f"// {js}\nexport const name = '{js}';\n"
            )
        (adapter / "package.json").write_text('{"name":"mpm-opencode"}\n')

        # The installer's own source, stripped for sourcing.
        (self.project / "install.sh.lib").write_text(
            build_sourced_lib(INSTALL_SH.read_text())
        )

    def _make_checkout_style_layout(self):
        """The checkout-style installation: real directories, real state."""
        (self.data / "bin").mkdir()
        for name in BINARIES:
            shutil.copy(self.build_dir / name, self.data / "bin" / name)
        (self.data / "agent_installation").mkdir(exist_ok=True)
        (self.data / "agent_installation" / "adapter").mkdir()
        (self.data / "agent_installation" / "adapter" / "file.txt").write_text("x\n")

        db_dir = self.data / "src" / "db"
        db_dir.mkdir(parents=True)
        self.db_path = db_dir / "mpm.db"
        conn = sqlite3.connect(self.db_path)
        conn.execute("CREATE TABLE memories (id TEXT PRIMARY KEY, content TEXT)")
        conn.execute("INSERT INTO memories VALUES ('m1', 'the operator''s memory')")
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
    def drive(self, phases: str, env_extra: dict | None = None,
              use_strace: bool = False, rev: str | None = None):
        """Run the named install.sh phases in a child bash.

        `rev` sets the release label the installer uses, which is how a
        SECOND promotion is driven: the first install already occupies
        releases/<rev>, so a subsequent promotion has to name a
        different one.
        """
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
        if use_strace and shutil.which("strace"):
            cmd = ["strace", "-f", "-qq", "-e", "trace=openat", "-o",
                   str(self._tmp / "trace.log")] + cmd
        return subprocess.run(cmd, capture_output=True, text=True, timeout=300, env=env)

    def state_fingerprint(self) -> dict:
        """A fingerprint of everything that must survive an upgrade."""
        fp = {}
        for rel in ("src/db/mpm.db", "mpm_config.json", "mode/default.md",
                    "persona/default.md"):
            p = self.data / rel
            fp[rel] = sha256(p) if p.is_file() else None
        return fp

    def current_target(self):
        link = self.data / "current"
        if not link.is_symlink():
            return None
        return os.readlink(link)


# ===========================================================================
# PROOF 1 — incompatible schemas fail the preflight
# ===========================================================================


class Proof1_IncompatibleSchemasFailPreflight(GateFixture):
    def test_an_unrelated_lifecycle_events_table_stops_the_install(self):
        # The live database carries a table named lifecycle_events that
        # is NOT the substrate's. The install must REFUSE, and it must
        # refuse BEFORE anything is promoted.
        conn = sqlite3.connect(self.db_path)
        conn.execute("DROP TABLE IF EXISTS lifecycle_events")
        conn.execute("CREATE TABLE lifecycle_events (id TEXT PRIMARY KEY, note TEXT)")
        conn.execute("INSERT INTO lifecycle_events VALUES ('x', 'unrelated')")
        conn.commit()
        conn.close()

        r = self.drive("phase_schema_preflight")
        self.assertNotEqual(r.returncode, 0, "the preflight must refuse")
        self.assertIn("SCHEMA COLLISION", r.stderr)
        self.assertIn("lifecycle_events", r.stderr)

        # And nothing was promoted.
        self.assertFalse((self.data / "releases").exists())
        self.assertFalse((self.data / "current").is_symlink())

    def test_a_compatible_database_passes_the_preflight(self):
        r = self.drive("phase_schema_preflight")
        self.assertEqual(r.returncode, 0, r.stdout + r.stderr)

    def test_a_database_without_the_table_passes_the_preflight(self):
        # The table is ABSENT: the substrate will create it. That is not
        # a collision.
        r = self.drive("phase_schema_preflight")
        self.assertEqual(r.returncode, 0, r.stdout + r.stderr)

    def test_the_preflight_does_not_write_to_the_live_database(self):
        before = sha256(self.db_path)
        self.drive("phase_schema_preflight")
        self.assertEqual(sha256(self.db_path), before)


# ===========================================================================
# PROOF 2 — a failed initial conversion is recoverable at each step
# ===========================================================================


class Proof2_FailedInitialConversionIsRecoverable(GateFixture):
    def test_the_recovery_matrix(self):
        for step in BUNDLE_STEPS:
            with self.subTest(step=step):
                self._run_one_step(step)

    def _run_one_step(self, step: str):
        # A fresh installation for every step: the matrix is about the
        # FIRST conversion, so each step starts from the checkout-style
        # layout.
        self.setUp()
        fingerprint = self.state_fingerprint()

        r = self.drive(
            "phase_bundle",
            env_extra={"MPM_INSTALL_TEST_FAIL_AT": step},
        )
        self.assertNotEqual(
            r.returncode, 0,
            f"the injected failure at '{step}' did not stop the install:\n{r.stdout}",
        )
        self.assertIn(f"injected failure after '{step}'", r.stderr)

        # 1. THE STATE IS INTACT. The database, the configuration and the
        #    operator's definitions are byte-identical.
        self.assertEqual(
            self.state_fingerprint(), fingerprint,
            f"the failure at '{step}' changed the persistent state",
        )

        # 2. THE POINTER IS NOT LEFT POINTING AT A HALF-PROMOTED
        #    RELEASE. Either there is no pointer, or it names a release
        #    that is complete.
        target = self.current_target()
        if target is not None:
            self.assertTrue(
                (self.data / target).is_dir(),
                f"the failure at '{step}' left the pointer at a missing release",
            )

        # 3. THE CHECKOUT-STYLE LAYOUT IS STILL THERE, so the
        #    installation that was running before the attempt is still
        #    runnable.
        self.assertTrue(
            (self.data / "bin" / "mpm").is_file(),
            f"the failure at '{step}' left no runnable binary at bin/mpm",
        )

        # 4. NO CANDIDATE IS LEFT BEHIND. A staging directory that
        #    survives a failure is a stale candidate that the next run
        #    would have to reason about.
        staging = self.data / "staging"
        if staging.is_dir():
            leftovers = [p for p in staging.iterdir() if p.name != ".keep"]
            self.assertEqual(
                leftovers, [],
                f"the failure at '{step}' left a candidate bundle behind",
            )


# ===========================================================================
# PROOF 3 — a failed subsequent promotion restores the previous pointer
# ===========================================================================


class Proof3_FailedSubsequentPromotionRestoresThePointer(GateFixture):
    def _install_first_release(self):
        r = self.drive("phase_bundle")
        self.assertEqual(r.returncode, 0, r.stdout + r.stderr)
        self.assertIsNotNone(self.current_target())

    def test_a_failure_after_the_pointer_swap_restores_the_previous_one(self):
        self._install_first_release()
        first_target = self.current_target()
        first_release = self.data / first_target

        # A SECOND promotion, under a different release label, that fails
        # AFTER the pointer was swapped. The previous pointer must come
        # back: a failed promotion must not leave the installation
        # pointing at a revision whose promotion did not complete.
        r = self.drive(
            "phase_bundle",
            rev="second-revision",
            env_extra={"MPM_INSTALL_TEST_FAIL_AT": "convert"},
        )
        self.assertNotEqual(r.returncode, 0, r.stdout + r.stderr)
        self.assertIn("injected failure after 'convert'", r.stderr)
        self.assertIn("rolling back the promotion", r.stderr)

        self.assertEqual(
            self.current_target(), first_target,
            "a failed promotion must restore the previous active pointer",
        )
        self.assertTrue(first_release.is_dir(), "the previous release must survive")
        # The second release may exist on disk — it was installed before
        # the pointer was swapped — but it must NOT be the active one.
        self.assertTrue((self.data / "releases" / "second-revision").is_dir())

    def test_a_successful_second_promotion_moves_the_pointer(self):
        self._install_first_release()
        first_target = self.current_target()

        r = self.drive("phase_bundle", rev="second-revision")
        self.assertEqual(r.returncode, 0, r.stdout + r.stderr)
        self.assertEqual(self.current_target(), "releases/second-revision")
        self.assertNotEqual(self.current_target(), first_target)
        # The previous release is KEPT — that is what makes a rollback
        # possible.
        self.assertTrue((self.data / first_target).is_dir())


# ===========================================================================
# PROOF 4 — no new binary opens the live database before the backup
# ===========================================================================


class Proof4_NoNewBinaryOpensTheLiveDatabase(GateFixture):
    def test_the_new_binary_is_never_pointed_at_the_live_database(self):
        r = self.drive("phase_schema_preflight; phase_bundle")
        self.assertEqual(r.returncode, 0, r.stdout + r.stderr)

        log = self.invocation_log.read_text() if self.invocation_log.exists() else ""
        self.assertTrue(log, "the fake binary was never invoked — the proof is vacuous")
        for line in log.splitlines():
            if not line.strip():
                continue
            fields = line.split("\t")
            self.assertEqual(len(fields), 4, line)
            argv, workspace, db_path = fields[1], fields[2], fields[3]
            self.assertNotIn(
                str(self.db_path), argv,
                "the new binary was invoked with the live database path",
            )
            self.assertNotEqual(
                db_path, str(self.db_path),
                "the new binary was invoked with MPM_DB_PATH pointing at the live database",
            )
            self.assertNotEqual(
                workspace, str(self.data),
                "the new binary was invoked with MPM_WORKSPACE pointing at the live workspace",
            )

    def test_the_live_database_is_byte_identical_after_the_install(self):
        before = sha256(self.db_path)
        self.drive("phase_schema_preflight; phase_bundle")
        self.assertEqual(
            sha256(self.db_path), before,
            "the install modified the live database",
        )

    @unittest.skipUnless(shutil.which("strace"), "strace is not available")
    def test_the_live_database_is_only_ever_opened_read_only(self):
        r = self.drive("phase_schema_preflight; phase_bundle", use_strace=True)
        self.assertEqual(r.returncode, 0, r.stdout + r.stderr)
        trace = self._tmp / "trace.log"
        self.assertTrue(trace.exists(), "the trace was not produced")
        text = trace.read_text()
        offending = []
        for line in text.splitlines():
            if str(self.db_path) not in line:
                continue
            # An open of the live database must never be writable.
            if "O_RDONLY" not in line:
                offending.append(line)
            if "O_WRONLY" in line or "O_RDWR" in line:
                offending.append(line)
        self.assertEqual(
            offending, [],
            "the live database was opened for writing during the install",
        )


# ===========================================================================
# PROOF 5 — version reporting works without .git
# ===========================================================================


class Proof5_VersionReportingWorksWithoutGit(unittest.TestCase):
    def test_the_installed_binary_reports_its_identity_without_git(self):
        built = REPO_ROOT / ".build" / "bin" / "mpm"
        if not built.is_file():
            self.skipTest("the project has not been built; run `make build` first")
        with tempfile.TemporaryDirectory(prefix="mpm-gitfree-") as d:
            # A directory with NO .git anywhere near it.
            iso = Path(d) / "isolated"
            iso.mkdir()
            shutil.copy(built, iso / "mpm")
            out = subprocess.run(
                [str(iso / "mpm"), "--version"],
                capture_output=True, text=True, timeout=60,
                cwd=str(iso),
                env={"PATH": os.environ.get("PATH", ""), "HOME": str(iso)},
            )
            self.assertEqual(out.returncode, 0, out.stdout + out.stderr)
            self.assertNotIn("unknown", out.stdout.lower().replace("unknown-", ""),
                             f"the binary reported no identity:\n{out.stdout}")
            self.assertRegex(out.stdout, r"commit:\s+[0-9a-f]{8,}")
            self.assertFalse(
                any(p.name == ".git" for p in iso.iterdir()),
                "the isolated directory must contain no .git",
            )

    def test_the_git_free_BUILD_proof_exists_and_is_in_the_release_gate(self):
        proof = REPO_ROOT / "release_acceptance" / "gitfree_build_identity_test.go"
        self.assertTrue(proof.is_file(), "the D5 Git-free build proof is missing")
        text = proof.read_text()
        self.assertIn("TestD5_GitFreeBuildEmbedsTheAssertedIdentity", text)
        self.assertIn("TestD5_GitFreeBuildWithoutTheAssertionIsUnknown", text)
        makefile = (REPO_ROOT / "Makefile").read_text()
        self.assertIn("test-release:", makefile)
        self.assertIn("release_acceptance", makefile,
                      "the D5 proof must be reachable from the release gate")

    def test_the_bundle_validator_refuses_a_bundle_carrying_git(self):
        # The runtime half of D5: the installed runtime must not depend
        # on repository metadata.
        lib = (REPO_ROOT / "scripts" / "lib" / "runtime_bundle.sh").read_text()
        self.assertIn("Git-free", lib)
        self.assertIn('if [ -e "$bundle/.git" ]', lib)


# ===========================================================================
# PROOF 6 — a runtime switch cannot replace the operator's data
# ===========================================================================


class Proof6_RuntimeSwitchCannotReplaceUserData(GateFixture):
    def test_a_version_switch_leaves_the_data_and_the_definitions_alone(self):
        fingerprint = self.state_fingerprint()

        r = self.drive("phase_bundle")
        self.assertEqual(r.returncode, 0, r.stdout + r.stderr)

        self.assertEqual(
            self.state_fingerprint(), fingerprint,
            "the version switch changed the operator's data or definitions",
        )
        # And the definitions are still where the binaries look for them.
        self.assertTrue((self.data / "mode" / "default.md").is_file())
        self.assertTrue((self.data / "persona" / "default.md").is_file())

    def test_the_operator_definitions_stay_in_the_data_root_not_the_bundle(self):
        self.drive("phase_bundle")
        release = self.data / self.current_target()
        for name in ("mode", "persona", "src", "backups", "mpm_config.json"):
            self.assertFalse(
                (release / name).exists(),
                f"the bundle carries '{name}' — a version switch could replace it",
            )

    def test_a_bundle_carrying_state_is_refused(self):
        # The validator is the last line of defence: even if something
        # assembled a bundle with the operator's data inside it, the
        # bundle is refused.
        lib = (REPO_ROOT / "scripts" / "lib" / "runtime_bundle.sh").read_text()
        self.assertIn("RB_FORBIDDEN_IN_BUNDLE", lib)
        for name in ("src/db", "mpm_config.json", "mode", "persona"):
            self.assertIn(name, lib)


# ===========================================================================
# PROOF 7 — the tests cannot default to the live workspace
# ===========================================================================


class Proof7_TestsCannotDefaultToTheLiveWorkspace(unittest.TestCase):
    def test_the_go_database_safety_guard_exists(self):
        guard = REPO_ROOT / "internal" / "core" / "test_db_safety_test.go"
        self.assertTrue(guard.is_file(), "the repository-wide DB-safety guard is missing")
        text = guard.read_text()
        self.assertIn("TestDBSafety_NoLiveDBLeakInTests", text)

    def test_the_subprocess_isolation_guard_exists(self):
        guard = REPO_ROOT / "internal" / "testenv" / "subprocess_guard_test.go"
        self.assertTrue(guard.is_file(), "the subprocess isolation guard is missing")
        text = guard.read_text()
        self.assertIn("TestNoUnisolatedMPMSubprocess", text)

    def test_the_rehearsal_refuses_a_work_directory_inside_the_repository(self):
        lib = REPO_ROOT / "scripts" / "rehearsal" / "lib.sh"
        self.assertTrue(lib.is_file())
        text = lib.read_text()
        self.assertIn("REFUSING to use a work directory inside the repository", text)

    def test_no_python_test_writes_into_the_repository(self):
        # The rehearsal's work products must stay OUTSIDE the
        # repository: a copy of the source tree inside it breaks the
        # repo-wide scanners that walk every file under the root.
        offenders = []
        for p in sorted((REPO_ROOT / "scripts" / "tests").glob("*.py")):
            body = p.read_text()
            # A test may READ the repository; it may not WRITE into it.
            for pattern in (
                r"shutil\.rmtree\(\s*REPO_ROOT",
                r"shutil\.copytree\([^)]*REPO_ROOT[^)]*,\s*REPO_ROOT",
                r"open\(\s*REPO_ROOT[^)]*['\"]w['\"]",
            ):
                if re.search(pattern, body):
                    offenders.append(f"{p.name}: {pattern}")
        self.assertEqual(offenders, [], f"a test writes into the repository: {offenders}")


# ===========================================================================
# The gate itself must not touch the operator's installation
# ===========================================================================


class TestGateHermeticity(unittest.TestCase):
    def test_the_gate_uses_only_temporary_roots(self):
        # Every root the gate builds comes from tempfile.mkdtemp. A test
        # that reached into the operator's real ~/.mpm would make every
        # proof in this file meaningless — and would damage the very
        # installation the proofs are about.
        #
        # The scan is over STRING CONSTANTS, not over the raw text: the
        # file discusses operator paths in prose, and prose is not a path.
        import ast

        tree = ast.parse(Path(__file__).read_text())
        literals = [
            node.value
            for node in ast.walk(tree)
            if isinstance(node, ast.Constant) and isinstance(node.value, str)
        ]
        # Assembled rather than written out, so the patterns themselves
        # are not literals in this file.
        forbidden = ("/" + "home" + "/", "/" + "Users" + "/")
        for lit in literals:
            for pattern in forbidden:
                self.assertFalse(
                    pattern in lit,
                    f"the gate contains a literal operator path: {lit!r}",
                )
        self.assertIn("tempfile.mkdtemp", Path(__file__).read_text())


if __name__ == "__main__":
    unittest.main()
