#!/usr/bin/env python3
"""test_version_probe_sigpipe.py — the release-identity probes must not
die of SIGPIPE.

THE DEFECT
----------
The installer's release identity comes from the binary being installed:

    commit="$("$(build_dir)/mpm" --version 2>/dev/null | awk '/commit:/{print $2; exit}')"

and the bundle validator asks the same question of a bundle's binary:

    reported="$("$bundle/bin/mpm" --version 2>/dev/null | awk '/commit:/{print $2; exit}')"

Both are pipelines whose CONSUMER exits at the match. Three properties
combine into a failure mode that has no diagnostic whatsoever:

  1. the consumer exits at the match, so it closes the read end of the
     pipe while the producer may still have output to write;
  2. `set -o pipefail` (install.sh:94) makes the pipeline's status the
     producer's SIGPIPE status — 141;
  3. the statement is a bare assignment under `set -e`, so the caller's
     exit status becomes 141. The installer dies without saying why,
     long before it can name the phase it was in.

The producer is killed only if it writes AFTER the consumer has closed
the pipe. `mpm --version` writes its 172 bytes in ONE atomic write and
exits, so today it cannot be killed — but that is a property of the
current producer, not of the pipeline, and it is not a property the
probe may rely on. Any producer that writes a second time (a wrapper
script, a version surface that grew a second section, a test fixture) is
killed, and the installation dies with 141.

This suite pins the property the probe must have: the identity it
extracts must not depend on the producer surviving an early pipe close.

THE awk DOUBLE
--------------
The suite installs a small `awk` double first on PATH. It models the
probe program the installer uses — print the second whitespace-separated
field of the FIRST line matching /commit:/ — and it models an awk that
HONOURS `exit` by closing its stdin, which is what gawk and busybox awk
do. (mawk and nawk keep reading until EOF, so they never close the
pipe and the failure cannot be provoked with them; the double makes it
deterministic on every host.)

The double appends every program it models to $AWK_DOUBLE_LOG, so a
test can prove the double really modelled the probe instead of
silently delegating to the real awk.

HERMETICITY
-----------
Every root is a fresh mktemp directory. HOME, DATA_ROOT, PREFIX and
BUILD_DIR all point inside it, so nothing resolves into the real
~/.mpm and no installer ever runs against a live installation.
"""

import json
import os
import re
import shutil
import subprocess
import tempfile
import textwrap
import unittest
from pathlib import Path

REPO_ROOT = Path(__file__).resolve().parent.parent.parent
INSTALL_SH = REPO_ROOT / "install.sh"
BUNDLE_LIB = REPO_ROOT / "scripts" / "lib" / "runtime_bundle.sh"
PROMOTION_LIB = REPO_ROOT / "scripts" / "lib" / "binary_transaction.sh"

FAKE_COMMIT = "1a2b3c4d5e6f70819a2b3c4d5e6f70819a2b3c4d"
OTHER_COMMIT = "9f8e7d6c5b4a30291f8e7d6c5b4a3029189f8e7d"

REV = "v0.1.0"

BUNDLE_BINARIES = (
    "mpm",
    "mpm-scheduler",
    "mpm-critic",
    "mpm-mcp",
    "mpm-telemetry",
    "mpm-update",
)

# The three lines the updater-bootstrap suite strips when it turns
# install.sh into a sourceable library. The same three, for the same
# reason: the library must not pin SCRIPT_DIR/PROJECT_ROOT, and it must
# not run main.
STRIP_PATTERNS = (
    re.compile(r"^\s*readonly\s+SCRIPT_DIR\s*="),
    re.compile(r"^\s*readonly\s+PROJECT_ROOT\s*="),
    re.compile(r'^\s*main\s+"\$@"\s*$'),
)

# --------------------------------------------------------------------------
# The awk double.
# --------------------------------------------------------------------------
AWK_DOUBLE = """#!/usr/bin/env python3
# A test double for `awk`.
#
# It models the probe program the installer uses -- print the Nth
# whitespace-separated field of the FIRST line matching /regex/ -- and it
# models an awk that HONOURS `exit` by closing its stdin, which is what
# gawk and busybox awk do. (mawk and nawk keep reading until EOF, so they
# never close the pipe; the double makes the failure deterministic on
# every host.)
#
# Any program it models is appended to $AWK_DOUBLE_LOG. A program it does
# NOT model is delegated to the real awk, so the double never breaks an
# unrelated tool -- but then the log stays empty and the test that
# asserts on it fails, rather than passing vacuously.
import os
import re
import sys

prog = " ".join(sys.argv[1:])
m = re.search(r"/([^/]*)/\\{", prog)
fld = re.search(r"print\\s+\\$(\\d+)", prog)
if not (m and fld):
    os.execv("/usr/bin/awk", ["awk"] + sys.argv[1:])

log = os.environ.get("AWK_DOUBLE_LOG")
if log:
    with open(log, "a") as fh:
        fh.write(prog + "\\n")

pattern = re.compile(m.group(1))
index = int(fld.group(1)) - 1
exits = re.search(r"\\bexit\\b", prog) is not None

for line in sys.stdin:
    if pattern.search(line):
        fields = line.split()
        if len(fields) > index:
            sys.stdout.write(fields[index] + "\\n")
            sys.stdout.flush()
        if exits:
            # Honour `exit` the way gawk and busybox awk do: close the
            # read end of the pipe. A producer with output left now
            # takes SIGPIPE.
            try:
                os.close(0)
            except OSError:
                pass
            sys.exit(0)
sys.exit(0)
"""

# --------------------------------------------------------------------------
# The producers under test.
# --------------------------------------------------------------------------

# Writes the commit line and then writes AGAIN, after the consumer has
# matched and closed the pipe. This is the shape that is killed by
# SIGPIPE.
SLOW_PRODUCER = """\
#!/usr/bin/env bash
if [ "${1:-}" = '--version' ]; then
  echo 'MPM 0.1.0-dev+g%SHORT%'
  echo '  commit:  %COMMIT%'
  sleep 0.3
  echo '  dirty:   no'
fi
exit 0
"""

# The real producer's shape: ONE write, then exit. `mpm --version`
# emits its whole 172-byte output in a single write(2).
SINGLE_WRITE_PRODUCER = """\
#!/usr/bin/env bash
if [ "${1:-}" = '--version' ]; then
  printf 'MPM 0.1.0-dev+g%SHORT%\\n  commit:  %COMMIT%\\n  dirty:   no\\n'
fi
exit 0
"""

# Reports no commit at all.
NO_COMMIT_PRODUCER = """\
#!/usr/bin/env bash
if [ "${1:-}" = '--version' ]; then
  echo 'MPM 0.1.0-dev'
  echo '  binary:  mpm'
fi
exit 0
"""

# Malformed: a "commit:" label with no value.
MALFORMED_PRODUCER = """\
#!/usr/bin/env bash
if [ "${1:-}" = '--version' ]; then
  echo 'MPM ???'
  echo '  commit:'
  echo '  nonsense'
fi
exit 0
"""

# Reports the commit and then fails.
FAILING_PRODUCER = """\
#!/usr/bin/env bash
if [ "${1:-}" = '--version' ]; then
  echo '  commit:  %COMMIT%'
  exit 7
fi
exit 0
"""


class ProbeFixture(unittest.TestCase):
    """A disposable installation, a fake build tree and the awk double."""

    def setUp(self):
        self._tmp = Path(tempfile.mkdtemp(prefix="mpm-version-probe-"))
        self.addCleanup(shutil.rmtree, self._tmp, ignore_errors=True)

        self.home = self._tmp / "home"
        self.home.mkdir()
        self.project = self._tmp / "project"
        self.project.mkdir()
        self.data = self.project
        self.prefix = self.project
        self.build_dir = self.project / ".build" / "bin"
        self.build_dir.mkdir(parents=True)

        self.bin_dir = self._tmp / "bin"
        self.bin_dir.mkdir()
        self.awk_log = self._tmp / "awk-double.log"

        self._write_awk_double()
        self._write_checkout()

    # ------------------------------------------------------------------
    # fixture
    # ------------------------------------------------------------------
    def _write_awk_double(self):
        p = self.bin_dir / "awk"
        p.write_text(AWK_DOUBLE)
        p.chmod(0o755)

    def _write_checkout(self):
        lib = self.project / "scripts" / "lib"
        lib.mkdir(parents=True)
        shutil.copy(PROMOTION_LIB, lib / "binary_transaction.sh")
        shutil.copy(BUNDLE_LIB, lib / "runtime_bundle.sh")

        stripped = []
        for line in INSTALL_SH.read_text().splitlines():
            if any(p.match(line) for p in STRIP_PATTERNS):
                continue
            stripped.append(line)
        (self.project / "install.sh.lib").write_text(
            "\n".join(stripped) + "\n")

    def _producer(self, template):
        """The fake `mpm` the installer probes for its release identity."""
        body = (template
                .replace("%SHORT%", FAKE_COMMIT[:8])
                .replace("%COMMIT%", FAKE_COMMIT))
        p = self.build_dir / "mpm"
        p.write_text(body)
        p.chmod(0o755)

    def _env(self, extra=None):
        env = dict(os.environ)
        env["HOME"] = str(self.home)
        env["PATH"] = f"{self.bin_dir}:{env['PATH']}"
        env["AWK_DOUBLE_LOG"] = str(self.awk_log)
        env.pop("MPM_WORKSPACE", None)
        env.pop("MPM_DB_PATH", None)
        if extra:
            env.update({k: str(v) for k, v in extra.items()})
        return env

    # ------------------------------------------------------------------
    # drivers
    # ------------------------------------------------------------------
    def drive_phase_bundle(self, extra=None):
        """Run the REAL phase_bundle out of the REAL install.sh.

        The fault injector stops the phase at the `assemble` step, which
        is the first step AFTER the release identity is taken. The
        injected failure is exit status 3, so a run that reaches it has
        proved it got past the probe.
        """
        driver = self.project / "_drive.sh"
        driver.write_text(textwrap.dedent(f"""\
            #!/usr/bin/env bash
            set -uo pipefail
            export PROJECT_ROOT="{self.project}"
            source "{self.project}/install.sh.lib"
            BUILD_DIR="{self.build_dir}"
            PREFIX="{self.prefix}"
            DATA_ROOT="{self.data}"
            SERVICE_DST=/dev/null
            export BUILD_DIR PREFIX DATA_ROOT SERVICE_DST
            phase_bundle
            """))
        driver.chmod(0o755)
        env = self._env(extra)
        env.setdefault("MPM_INSTALL_TEST_FAIL_AT", "assemble")
        return subprocess.run(
            ["bash", str(driver)], capture_output=True, text=True,
            timeout=300, env=env)

    def drive_validator(self, bundle, expected_rev=REV):
        """Run the REAL rb_validate_bundle out of the REAL library.

        The function is called DIRECTLY, with `set -e` live, exactly as
        a caller that does not guard it would call it. That matters: a
        probe that dies of SIGPIPE must abort the caller, and the
        caller's exit status is the only place that shows. So the status
        is observed from OUTSIDE the driver, not echoed from inside it
        — a `|| rc=$?` would suspend `set -e` inside the function and
        hide the very failure this suite is about.
        """
        driver = self._tmp / "_drive_validator.sh"
        driver.write_text(textwrap.dedent(f"""\
            #!/usr/bin/env bash
            set -euo pipefail
            . "{BUNDLE_LIB}"
            DATA_ROOT="{self.data}"
            HOME="{self.home}"
            export DATA_ROOT HOME
            rb_validate_bundle "{bundle}" "{expected_rev}"
            """))
        driver.chmod(0o755)
        return subprocess.run(
            ["bash", str(driver)], capture_output=True, text=True,
            timeout=300, env=self._env())

    def make_bundle(self, commit=FAKE_COMMIT, producer=None, name="candidate"):
        bundle = self._tmp / name
        (bundle / "bin").mkdir(parents=True, exist_ok=True)
        for binary in BUNDLE_BINARIES:
            p = bundle / "bin" / binary
            if binary == "mpm" and producer is not None:
                p.write_text(producer
                             .replace("%SHORT%", FAKE_COMMIT[:8])
                             .replace("%COMMIT%", commit))
            else:
                p.write_text("#!/usr/bin/env bash\nexit 0\n")
            p.chmod(0o755)
        (bundle / "RELEASE.json").write_text(json.dumps({
            "revision": REV,
            "commit": commit,
            "short_commit": commit[:8],
            "build_epoch": 1793000000,
            "version": "0.1.0-dev",
            "built_at": "2026-10-28T00:00:00Z",
            "bundle_layout": "versioned-bundle/1",
        }))
        return bundle

    # ------------------------------------------------------------------
    # assertions
    # ------------------------------------------------------------------
    def assert_double_modelled_the_probe(self):
        """The awk double must have modelled the probe, not delegated.

        Without this the suite could pass vacuously: a probe the double
        does not understand is delegated to the real awk, which behaves
        well, and the failure would never be provoked.
        """
        modelled = self.awk_log.read_text() if self.awk_log.is_file() else ""
        self.assertIn(
            "commit:", modelled,
            "the awk double never modelled the installer's commit probe — "
            "the probe's awk program changed shape, so this suite is no "
            "longer exercising it")
        self.assertIn(
            "print $2", modelled,
            "the awk double modelled a program that does not print $2 — "
            "the probe's awk program changed shape")

    def assert_not_sigpipe(self, result, what):
        self.assertNotEqual(
            result.returncode, 141,
            f"{what} died of SIGPIPE (exit 141): the probe's consumer "
            f"closed the pipe while the producer still had output to "
            f"write.\nstdout:\n{result.stdout}\nstderr:\n{result.stderr}")


class TestInstallerProbe(ProbeFixture):
    """The installer's release identity must survive a prompt-closing
    consumer, and must still be the binary's own identity."""

    def test_the_probe_survives_a_producer_that_writes_again(self):
        # THE POINT. The producer writes the commit line and then writes
        # again, after the consumer has matched. The consumer closes the
        # pipe at the match, so the producer takes SIGPIPE, and under
        # pipefail the pipeline's status is 141. Because the probe is a
        # bare assignment under `set -e`, that 141 becomes the
        # installer's exit status — with no diagnostic at all.
        self._producer(SLOW_PRODUCER)
        r = self.drive_phase_bundle()

        self.assert_not_sigpipe(r, "the installer")
        self.assert_double_modelled_the_probe()
        # The phase got past the probe and into the injected failure.
        self.assertIn(
            "injected failure after 'assemble'", r.stderr,
            "the phase never reached the step after the release identity "
            f"was taken\nstdout:\n{r.stdout}\nstderr:\n{r.stderr}")
        self.assertEqual(r.returncode, 3, r.stdout + r.stderr)
        # The identity is still the binary's own.
        self.assertIn(f"commit:    {FAKE_COMMIT}", r.stderr, r.stdout + r.stderr)

    def test_the_probe_reports_the_binarys_own_identity(self):
        # The producer writes once, the way the real `mpm --version`
        # does. The identity must be the binary's own, unchanged.
        self._producer(SINGLE_WRITE_PRODUCER)
        r = self.drive_phase_bundle()

        self.assert_not_sigpipe(r, "the installer")
        self.assertIn(f"commit:    {FAKE_COMMIT}", r.stderr, r.stdout + r.stderr)

    def test_a_binary_that_reports_no_commit_is_not_silently_trusted(self):
        # No commit line: the probe yields nothing, and the installer
        # must not invent one. It falls back to the provenance script,
        # which is absent here, so the revision is 'unknown'.
        self._producer(NO_COMMIT_PRODUCER)
        r = self.drive_phase_bundle()

        self.assert_not_sigpipe(r, "the installer")
        self.assertNotIn(f"commit:    {FAKE_COMMIT}", r.stderr)
        self.assertIn("commit:    unknown", r.stderr, r.stdout + r.stderr)

    def test_a_malformed_version_output_is_not_silently_trusted(self):
        self._producer(MALFORMED_PRODUCER)
        r = self.drive_phase_bundle()

        self.assert_not_sigpipe(r, "the installer")
        self.assertIn("commit:    unknown", r.stderr, r.stdout + r.stderr)

    def test_a_binary_that_fails_stops_the_installation(self):
        # A producer that exits non-zero must fail the probe, and the
        # installation must stop — fail closed. The status is the
        # producer's, not a silent success and not 141.
        self._producer(FAILING_PRODUCER)
        r = self.drive_phase_bundle()

        self.assertNotEqual(r.returncode, 0, r.stdout + r.stderr)
        self.assertNotEqual(
            r.returncode, 141,
            "a failing producer must fail the probe, not be mistaken for "
            f"a broken pipe\nstdout:\n{r.stdout}\nstderr:\n{r.stderr}")
        self.assertNotIn(
            "injected failure after 'assemble'", r.stderr,
            "the phase carried on after the probe failed")


class TestBundleValidatorProbe(ProbeFixture):
    """The bundle validator asks the same question of the bundle's own
    binary, and must reach the same answer."""

    def test_the_probe_survives_a_producer_that_writes_again(self):
        bundle = self.make_bundle(producer=SLOW_PRODUCER)
        r = self.drive_validator(bundle)

        self.assert_not_sigpipe(r, "the bundle validator")
        self.assert_double_modelled_the_probe()
        self.assertEqual(r.returncode, 0, r.stdout + r.stderr)

    def test_a_well_formed_bundle_still_validates(self):
        bundle = self.make_bundle(producer=SINGLE_WRITE_PRODUCER)
        r = self.drive_validator(bundle)

        self.assertEqual(r.returncode, 0, r.stdout + r.stderr)

    def test_a_bundle_whose_binary_reports_no_commit_is_refused(self):
        bundle = self.make_bundle(producer=NO_COMMIT_PRODUCER)
        r = self.drive_validator(bundle)

        self.assertEqual(r.returncode, 1, r.stdout + r.stderr)
        self.assertIn("did not report a build commit", r.stderr)

    def test_a_bundle_whose_binary_reports_malformed_output_is_refused(self):
        bundle = self.make_bundle(producer=MALFORMED_PRODUCER)
        r = self.drive_validator(bundle)

        self.assertEqual(r.returncode, 1, r.stdout + r.stderr)
        self.assertIn("did not report a build commit", r.stderr)

    def test_a_bundle_whose_binary_fails_is_refused(self):
        # The producer's own status is what fails the probe, and the
        # validation stops. The point is that it is the PRODUCER's
        # failure and not a broken pipe: a broken pipe would report 141
        # and say nothing about what actually went wrong.
        bundle = self.make_bundle(producer=FAILING_PRODUCER)
        r = self.drive_validator(bundle)

        self.assertNotEqual(r.returncode, 0, r.stdout + r.stderr)
        self.assertNotEqual(
            r.returncode, 141,
            "a failing producer must fail the probe, not be mistaken for "
            f"a broken pipe\nstdout:\n{r.stdout}\nstderr:\n{r.stderr}")

    def test_a_mixed_bundle_is_still_refused(self):
        # The validator's job: a bundle whose binaries disagree with
        # its RELEASE.json must never be installed.
        bundle = self.make_bundle(commit=FAKE_COMMIT,
                                  producer=SINGLE_WRITE_PRODUCER)
        (bundle / "bin" / "mpm").write_text(
            SINGLE_WRITE_PRODUCER
            .replace("%SHORT%", OTHER_COMMIT[:8])
            .replace("%COMMIT%", OTHER_COMMIT))
        (bundle / "bin" / "mpm").chmod(0o755)
        r = self.drive_validator(bundle)

        self.assertEqual(r.returncode, 1, r.stdout + r.stderr)
        self.assertIn("mixture", r.stderr)

    def test_a_corrupt_bundle_is_still_refused(self):
        # A bundle that carries the operator's state, or that is missing
        # a binary, must not pass validation.
        for state in ("src/db", "mpm_config.json"):
            with self.subTest(state=state):
                bundle = self.make_bundle(producer=SINGLE_WRITE_PRODUCER,
                                          name=f"corrupt-{state.replace('/', '-')}")
                target = bundle / state
                if "." in state.split("/")[-1]:
                    target.write_text("{}")
                else:
                    target.mkdir(parents=True, exist_ok=True)
                r = self.drive_validator(bundle)
                self.assertEqual(r.returncode, 1, r.stdout + r.stderr)
                self.assertIn("persistent state", r.stderr)

        bundle = self.make_bundle(producer=SINGLE_WRITE_PRODUCER,
                                  name="corrupt-missing-binary")
        (bundle / "bin" / "mpm-update").unlink()
        r = self.drive_validator(bundle)
        self.assertEqual(r.returncode, 1, r.stdout + r.stderr)
        self.assertIn("mpm-update", r.stderr)


if __name__ == "__main__":
    unittest.main()
