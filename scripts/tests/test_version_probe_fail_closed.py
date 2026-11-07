#!/usr/bin/env python3
"""test_version_probe_fail_closed.py — a version probe must fail closed.

THE DEFECT
----------
Issue #221. A version probe written as a shell pipeline has NO status of
its own:

    V1="$(cd / && "$SIM_WS/bin/mpm" --version 2>&1 | head -1)"
    VOUT="$("$SIM_WS/bin/mpm" --version 2>&1)"
    INSTALLED_COMMIT="$("$SIM_WS/bin/mpm" --version 2>&1 | awk '/commit:/{print $2}')"
    RUNTIME_REV="$("$MPM_BIN" version 2>/dev/null | awk '/^  commit:/ {print $2; exit}')"

Without `pipefail` the pipeline's status is the LAST command's — the
parser's — and with `pipefail` it is the rightmost NONZERO status, which
is the producer's only when the producer failed AND the parser did not.
So whether a FAILING producer was noticed depended on the caller's
shell configuration, and the VALUE was all that was asserted.

The consequence is the thing this issue is about: a binary that printed
a valid revision and THEN failed satisfied the obligation. "The
installed binary is the accepted revision" could be true of a binary
that did not work.

Two of the shapes also had the SIGPIPE hazard issue #217 removed from
the release-identity probe: `head -1` and an awk `exit` close the pipe
at the match, and a producer that still has output to write is killed
with SIGPIPE (exit 141).

THE FIX
-------
Every version probe now goes through ONE of the release-identity
helpers in scripts/lib/runtime_bundle.sh, whose contract is the same
one PR #220 gave `rb_reported_commit`:

  * 0 — the producer exited 0. The value is on stdout (EMPTY if the
    producer reported nothing).
  * the PRODUCER's own status — the producer failed. NOTHING is on
    stdout: a failed producer yields no identity.

  * rb_reported_commit    — the COMMIT the binary reports.
  * rb_version_output     — the binary's whole version output.
  * rb_version_first_line — the binary's build identity (the first line).

And the callers CAPTURE THE STATUS with the value, so a failed probe
cannot be mistaken for an answer.

WHAT THIS SUITE PINS
--------------------
  * the six producer shapes — the expected revision and success; the
    expected revision and failure; the WRONG revision and success;
    malformed output; empty output; a producer that keeps writing after
    the revision; and a producer killed by a closed pipe;
  * the helper contract for each of them, under EVERY shell option set
    the rehearsal and the installer use;
  * that the rehearsal's own version ASSERTION fails when the producer
    failed, and passes when the producer succeeded;
  * that the dogfood baseline records a MEASUREMENT GAP — and exits 2 —
    when the binary's version cannot be read;
  * and a STATIC ratchet: no script in the issue's scope may capture a
    version probe's output without capturing its status, because that is
    the shape that lets a failed binary satisfy an obligation.

HERMETICITY
-----------
Every fixture is a fresh mktemp directory. HOME, REH_WORK and the
scratch workspace all point inside it, so nothing resolves into the
real ~/.mpm and no production service is touched. The helpers are
driven from the REAL repository sources — the suite cannot drift away
from the code it covers.
"""

from __future__ import annotations

import json
import os
import re
import shutil
import subprocess
import tempfile
import textwrap
import time
import unittest
from pathlib import Path

REPO_ROOT = Path(__file__).resolve().parent.parent.parent
BUNDLE_LIB = REPO_ROOT / "scripts" / "lib" / "runtime_bundle.sh"
REHEARSAL_LIB = REPO_ROOT / "scripts" / "rehearsal" / "lib.sh"
DOGFOOD = REPO_ROOT / "scripts" / "dogfood_baseline.sh"

FAKE_COMMIT = "06c029ba11ce18c7f10aa60b0f0497ece2edda7c"
FAKE_SHORT = FAKE_COMMIT[:8]
OTHER_COMMIT = "deadbeef11ce18c7f10aa60b0f0497ece2edda7c"

# The shell option sets the probe must be immune to. The rehearsal
# runs `set -uo pipefail`; the installer runs `set -u`, `set -eu` and
# `set -euo pipefail`; and a helper invoked inside `if ! ...` has
# `errexit` SUSPENDED for its whole body.
OPTION_SETS = (
    ("set -u", "-u"),
    ("set -eu", "-eu"),
    ("set -euo pipefail", "-euo pipefail"),
)

# --------------------------------------------------------------------------
# The producers under test.
#
# Every one of them answers `--version` and nothing else, so a probe
# that asks for anything else sees the same behaviour.
# --------------------------------------------------------------------------

# Reports the expected revision and SUCCEEDS.
GOOD = """\
#!/usr/bin/env bash
echo 'MPM 0.1.0-dev+g%(short)s.t1792679720'
echo '  commit:  %(commit)s'
exit 0
"""

# Reports the expected revision and THEN FAILS. This is the shape the
# issue is about: the identity is on stdout, but the producer did not
# succeed, so it has NOT reported an identity.
FAILING = """\
#!/usr/bin/env bash
echo 'MPM 0.1.0-dev+g%(short)s.t1792679720'
echo '  commit:  %(commit)s'
exit 7
"""

# Reports the WRONG revision and succeeds.
WRONG = """\
#!/usr/bin/env bash
echo 'MPM 0.1.0-dev+gdeadbeef.t1792679720'
echo '  commit:  %(other)s'
exit 0
"""

# Reports something malformed: a "commit:" label with no value, and an
# identity that is not an identity.
MALFORMED = """\
#!/usr/bin/env bash
echo 'MPM ???'
echo '  commit:'
echo '  nonsense'
exit 0
"""

# Reports nothing at all.
EMPTY = """\
#!/usr/bin/env bash
exit 0
"""

# Reports the revision and then KEEPS WRITING. The revision must still
# be found, and the producer must not be killed: this is the shape a
# parser that stops early turns into a SIGPIPE.
VERBOSE = """\
#!/usr/bin/env bash
echo 'MPM 0.1.0-dev+g%(short)s.t1792679720'
echo '  commit:  %(commit)s'
i=0
while [ "$i" -lt 400 ]; do
  echo "  padding line $i ..............................................."
  i=$((i + 1))
done
exit 0
"""

# Terminates because of a closed pipe. This is the status a producer
# killed by SIGPIPE reports (128 + 13), and it is what a probe that
# stops at the first line PRODUCES. The probe must never be the cause:
# it reads to EOF, so the pipe is never closed early.
SIGPIPE = """\
#!/usr/bin/env bash
echo 'MPM 0.1.0-dev+g%(short)s.t1792679720'
echo '  commit:  %(commit)s'
kill -PIPE $$
sleep 5
"""


def _render(template: str) -> str:
    return (
        template
        .replace("%(short)s", FAKE_SHORT)
        .replace("%(commit)s", FAKE_COMMIT)
        .replace("%(other)s", OTHER_COMMIT)
    )


class ProbeFixture(unittest.TestCase):
    """A disposable tree, a fake producer, and drivers for the helpers."""

    def setUp(self) -> None:
        self._tmp = Path(tempfile.mkdtemp(prefix="mpm-version-probe-"))
        self.addCleanup(shutil.rmtree, self._tmp, ignore_errors=True)

        self.home = self._tmp / "home"
        self.home.mkdir()
        self.bin_dir = self._tmp / "bin"
        self.bin_dir.mkdir()
        self.reh_work = self._tmp / "rehearsal-work"
        self.reh_work.mkdir()

    # ------------------------------------------------------------------
    # fixtures
    # ------------------------------------------------------------------
    def producer(self, template: str) -> Path:
        p = self.bin_dir / "mpm"
        p.write_text(_render(template))
        p.chmod(0o755)
        return p

    def env(self, extra: dict | None = None) -> dict:
        env = dict(os.environ)
        env["HOME"] = str(self.home)
        env["REH_WORK"] = str(self.reh_work)
        env.pop("MPM_WORKSPACE", None)
        env.pop("MPM_DB_PATH", None)
        if extra:
            env.update({k: str(v) for k, v in extra.items()})
        return env

    def run_bash(self, script: str, extra: dict | None = None, timeout: int = 120):
        driver = self._tmp / "_drive.sh"
        driver.write_text(script)
        driver.chmod(0o755)
        return subprocess.run(
            ["bash", str(driver)],
            capture_output=True,
            text=True,
            timeout=timeout,
            env=self.env(extra),
        )

    # ------------------------------------------------------------------
    # drivers
    # ------------------------------------------------------------------
    def probe(self, binary: Path, helper: str, options: str = "-u") -> tuple[int, str]:
        """Drive one release-identity helper and return (status, stdout).

        The helper is the REAL one, sourced from the REAL library.
        """
        script = textwrap.dedent(f"""\
            {options}
            . "{BUNDLE_LIB}"
            rc=0
            v="$({helper} "{binary}")" || rc=$?
            printf 'rc=%s\\n' "$rc"
            printf '<<VALUE>>\\n%s\\n<<END>>\\n' "${{v:-}}"
            """)
        r = self.run_bash(script)
        status = None
        value = None
        collecting = False
        for line in r.stdout.splitlines():
            if line.startswith("rc="):
                status = int(line[3:])
            elif line == "<<VALUE>>":
                collecting = True
                value = ""
            elif line == "<<END>>":
                collecting = False
            elif collecting:
                value += line + "\n"
        if status is None:
            self.fail(
                f"the helper produced no verdict\n--- stdout ---\n{r.stdout}\n"
                f"--- stderr ---\n{r.stderr}"
            )
        return status, (value or "").rstrip("\n")

    def rehearsal_assertion(self, binary: Path, needle: str) -> tuple[int, str]:
        """Drive the rehearsal's own version assertion.

        The assertion is the REAL one, from the REAL rehearsal library.
        """
        script = textwrap.dedent(f"""\
            REH_DIR="{REHEARSAL_LIB.parent}"
            REH_ROOT="{REHEARSAL_LIB.parent}"
            . "{REHEARSAL_LIB}"
            rc=0
            reh_version_line "the runtime reports the right revision" "{binary}" "{needle}" || rc=$?
            printf 'rc=%s\\n' "$rc"
            printf 'passed=%s failed=%s\\n' "$REH_PASS" "$REH_FAIL"
            """)
        r = self.run_bash(script)
        status = None
        counts = ""
        for line in r.stdout.splitlines():
            if line.startswith("rc="):
                status = int(line[3:])
            elif line.startswith("passed="):
                counts = line
        if status is None:
            self.fail(
                f"the rehearsal assertion produced no verdict\n--- stdout ---\n"
                f"{r.stdout}\n--- stderr ---\n{r.stderr}"
            )
        return status, counts


class TestTheHelpersFailClosed(ProbeFixture):
    """The helper contract, for every producer shape."""

    def test_a_successful_producer_is_accepted(self) -> None:
        """Shape 1: the expected revision, and the producer succeeds."""
        binary = self.producer(GOOD)
        for options, _ in OPTION_SETS:
            with self.subTest(options=options):
                status, value = self.probe(binary, "rb_version_output", options)
                self.assertEqual(status, 0)
                self.assertIn(FAKE_COMMIT, value)
                self.assertIn(FAKE_SHORT, value)

                status, value = self.probe(binary, "rb_version_first_line", options)
                self.assertEqual(status, 0)
                self.assertEqual(value, f"MPM 0.1.0-dev+g{FAKE_SHORT}.t1792679720")

                status, value = self.probe(binary, "rb_reported_commit", options)
                self.assertEqual(status, 0)
                self.assertEqual(value, FAKE_COMMIT)

    def test_a_failing_producer_is_refused(self) -> None:
        """Shape 2: the expected revision, and the producer FAILS.

        This is the shape the issue is about. The revision is on stdout,
        but the producer did not succeed, so it has NOT reported a
        revision and the probe must yield NOTHING.
        """
        binary = self.producer(FAILING)
        for options, _ in OPTION_SETS:
            with self.subTest(options=options):
                status, value = self.probe(binary, "rb_version_output", options)
                self.assertEqual(status, 7, "the producer's own status must be the verdict")
                self.assertEqual(value, "", "a failed producer must yield no output")

                status, value = self.probe(binary, "rb_version_first_line", options)
                self.assertEqual(status, 7)
                self.assertEqual(value, "")

                status, value = self.probe(binary, "rb_reported_commit", options)
                self.assertEqual(status, 7)
                self.assertEqual(value, "")

    def test_a_wrong_revision_is_reported_as_the_wrong_revision(self) -> None:
        """Shape 3: the WRONG revision, and the producer succeeds.

        The probe reports what the binary said — it does not decide
        whether the revision is the RIGHT one. The CALLER decides, and
        the caller can only do that because the probe succeeded.
        """
        binary = self.producer(WRONG)
        status, value = self.probe(binary, "rb_version_output")
        self.assertEqual(status, 0)
        self.assertNotIn(FAKE_COMMIT, value)
        self.assertIn(OTHER_COMMIT, value)

        status, value = self.probe(binary, "rb_reported_commit")
        self.assertEqual(status, 0)
        self.assertEqual(value, OTHER_COMMIT)

    def test_malformed_and_empty_output_are_not_an_identity(self) -> None:
        """Shape 4: malformed output, and empty output."""
        malformed = self.producer(MALFORMED)
        status, value = self.probe(malformed, "rb_version_output")
        self.assertEqual(status, 0)
        self.assertNotIn(FAKE_COMMIT, value)
        status, value = self.probe(malformed, "rb_reported_commit")
        self.assertEqual(status, 0)
        self.assertNotIn(FAKE_COMMIT, value)

        empty = self.producer(EMPTY)
        status, value = self.probe(empty, "rb_version_output")
        self.assertEqual(status, 0)
        self.assertEqual(value.strip(), "")
        status, value = self.probe(empty, "rb_reported_commit")
        self.assertEqual(status, 0)
        self.assertEqual(value.strip(), "")

    def test_a_producer_that_keeps_writing_is_not_killed(self) -> None:
        """Shape 5: the revision, and then a great deal more output.

        The probe must read to EOF, so the producer finishes normally
        and the revision is still found. A probe that stopped at the
        match would close the pipe and kill the producer (SIGPIPE).
        """
        binary = self.producer(VERBOSE)
        status, value = self.probe(binary, "rb_version_output")
        self.assertEqual(status, 0, "a producer that kept writing must not be killed")
        self.assertIn(FAKE_COMMIT, value)
        self.assertGreater(value.count("padding line"), 100)

        status, value = self.probe(binary, "rb_version_first_line")
        self.assertEqual(status, 0)
        self.assertEqual(value, f"MPM 0.1.0-dev+g{FAKE_SHORT}.t1792679720")

    def test_a_producer_killed_by_a_closed_pipe_is_refused(self) -> None:
        """Shape 6: the producer is terminated because of a closed pipe.

        The producer prints a valid identity and is then killed by
        SIGPIPE — the status 141 a probe that stops at the first line
        PRODUCES. The probe must report that failure, not the revision
        the producer managed to print first: a killed producer has not
        reported anything.
        """
        binary = self.producer(SIGPIPE)
        for helper in ("rb_version_output", "rb_version_first_line", "rb_reported_commit"):
            with self.subTest(helper=helper):
                status, value = self.probe(binary, helper)
                self.assertEqual(
                    status, 141,
                    "a producer killed by SIGPIPE must be reported as the "
                    "failure it is, not as an identity",
                )
                self.assertEqual(
                    value, "",
                    "a producer killed by SIGPIPE must yield no identity",
                )

    def test_the_probe_never_closes_the_pipe_early(self) -> None:
        """The probe must never be the cause of a SIGPIPE.

        Issue #217's property: the probe reads to EOF, so a producer
        that keeps writing is never killed. The verdict for such a
        producer is its own 0 — never 141.
        """
        binary = self.producer(VERBOSE)
        for helper in ("rb_version_output", "rb_version_first_line", "rb_reported_commit"):
            with self.subTest(helper=helper):
                status, _ = self.probe(binary, helper, "-euo pipefail")
                self.assertNotEqual(
                    status, 141,
                    "the probe closed the pipe early and killed the producer",
                )
                self.assertEqual(status, 0)

    def test_a_failed_probe_names_the_binary_on_stderr(self) -> None:
        """A failed probe must say WHICH binary failed.

        The value is empty on a failure, so the diagnostic is the only
        thing that tells an operator reading a rehearsal log which
        binary could not report itself.
        """
        binary = self.producer(FAILING)
        for helper in ("rb_version_output", "rb_version_first_line", "rb_reported_commit"):
            with self.subTest(helper=helper):
                r = self.run_bash(textwrap.dedent(f"""\
                    . "{BUNDLE_LIB}"
                    rc=0
                    v="$({helper} "{binary}")" || rc=$?
                    printf 'rc=%s\\n' "$rc"
                    """))
                self.assertIn(str(binary), r.stderr,
                              "the diagnostic must name the binary that failed")
                self.assertIn("failed", r.stderr)

    def test_a_helper_reports_its_own_name(self) -> None:
        """A caller must be told which helper it called.

        `rb_version_first_line` is implemented on top of
        `rb_version_output`, so the inner helper's diagnostic is
        replaced with one that names the OUTER helper — otherwise the
        log names a function the caller never invoked.
        """
        binary = self.producer(FAILING)
        r = self.run_bash(textwrap.dedent(f"""\
            . "{BUNDLE_LIB}"
            rc=0
            v="$(rb_version_first_line "{binary}")" || rc=$?
            printf 'rc=%s\\n' "$rc"
            """))
        self.assertIn("rb_version_first_line", r.stderr)
        self.assertNotIn("rb_version_output", r.stderr)

    def test_a_failed_probe_yields_no_value_when_the_status_is_inverted(self) -> None:
        """The `if ! ...` context: errexit suspended, the status inverted.

        A caller that writes

            if ! v="$(rb_version_first_line "$bin")"; then ... fi

        enters the then-branch on FAILURE — that is what `!` means.
        The value must STILL be empty, so a caller that then asserts on
        it cannot be satisfied by a producer that failed. This is the
        context PR #220's contract was written for: a helper invoked
        inside `if ! ...` has `errexit` SUSPENDED for its whole body,
        so the helper must not rely on the caller's `errexit` to stop
        anything.
        """
        binary = self.producer(FAILING)
        for helper in ("rb_version_output", "rb_version_first_line", "rb_reported_commit"):
            with self.subTest(helper=helper):
                r = self.run_bash(textwrap.dedent(f"""\
                    set -euo pipefail
                    . "{BUNDLE_LIB}"
                    v="<never assigned>"
                    if ! v="$({helper} "{binary}")"; then
                      printf 'branch=taken\\n'
                    fi
                    printf 'value=[%s]\\n' "$v"
                    """))
                self.assertIn("branch=taken", r.stdout,
                              "the inverted condition must take the failure branch")
                self.assertIn("value=[]", r.stdout,
                              "a failed probe must yield no value, inverted or not")
                # The helper must actually have RUN. Without this the
                # test would pass at the base tree, where the helper
                # does not exist at all and the shell's "command not
                # found" produces the same empty value.
                self.assertIn(
                    str(binary), r.stderr,
                    "the helper did not run — the empty value proves nothing",
                )

    def test_the_probe_does_not_kill_a_producer_that_keeps_writing(self) -> None:
        """The empirical form of the same property.

        The producer is killed by SIGPIPE if — and only if — somebody
        closes the read end of its pipe. The probe must be that
        somebody's opposite: it must read everything.
        """
        binary = self.producer(SIGPIPE)
        # A producer that kills itself is the shape under test; the
        # probe's own verdict is asserted above. Here the property is
        # stated in the producer's terms: a producer that would be
        # killed by a closed pipe still completes under the probe.
        alive = self.producer(VERBOSE)
        status, value = self.probe(alive, "rb_version_output")
        self.assertEqual(status, 0)
        self.assertIn(FAKE_COMMIT, value)
        self.assertGreater(value.count("padding line"), 100,
                           "the probe must read the producer's output to EOF")


class TestTheRehearsalAssertionFailsClosed(ProbeFixture):
    """The rehearsal's own assertion must fail when the producer failed."""

    def test_a_successful_producer_satisfies_the_assertion(self) -> None:
        binary = self.producer(GOOD)
        status, counts = self.rehearsal_assertion(binary, f"g{FAKE_SHORT}")
        self.assertEqual(status, 0)
        self.assertIn("passed=1", counts)
        self.assertIn("failed=0", counts)

    def test_a_failing_producer_does_not_satisfy_the_assertion(self) -> None:
        """THE POINT.

        "the installed binary is the accepted revision" must not be
        satisfied by a binary that printed the revision and then
        failed.
        """
        binary = self.producer(FAILING)
        status, counts = self.rehearsal_assertion(binary, f"g{FAKE_SHORT}")
        self.assertEqual(status, 1, "a failed producer must not satisfy the assertion")
        self.assertIn("failed=1", counts)

    def test_a_wrong_revision_does_not_satisfy_the_assertion(self) -> None:
        binary = self.producer(WRONG)
        status, counts = self.rehearsal_assertion(binary, f"g{FAKE_SHORT}")
        self.assertEqual(status, 1)
        self.assertIn("failed=1", counts)

    def test_malformed_and_empty_output_do_not_satisfy_the_assertion(self) -> None:
        for template in (MALFORMED, EMPTY):
            with self.subTest(template=template.splitlines()[1][:40]):
                binary = self.producer(template)
                status, counts = self.rehearsal_assertion(binary, f"g{FAKE_SHORT}")
                self.assertEqual(status, 1)
                self.assertIn("failed=1", counts)

    def test_a_producer_that_keeps_writing_satisfies_the_assertion(self) -> None:
        binary = self.producer(VERBOSE)
        status, counts = self.rehearsal_assertion(binary, f"g{FAKE_SHORT}")
        self.assertEqual(status, 0)
        self.assertIn("passed=1", counts)


# --------------------------------------------------------------------------
# The dogfood baseline.
# --------------------------------------------------------------------------

def _load_sibling(module_name: str):
    """Load a sibling test module by PATH.

    The dogfood baseline needs a COMPLETE fixture substrate — every table
    it reads — or the report is INCOMPLETE for reasons that have
    nothing to do with the probe, and the exit status stops being
    evidence. The fixture builder already exists, in the suite that
    owns the baseline; it is loaded here rather than copied, so the two
    suites cannot drift apart.
    """
    import importlib.util

    path = Path(__file__).resolve().parent / f"{module_name}.py"
    spec = importlib.util.spec_from_file_location(f"_mpm_probe_{module_name}", path)
    assert spec is not None and spec.loader is not None
    module = importlib.util.module_from_spec(spec)
    spec.loader.exec_module(module)
    return module


_DOGFOOD_FIXTURE = _load_sibling("test_dogfood_baseline")


class TestTheDogfoodBaselineReportsAFailedProbe(unittest.TestCase):
    """A binary whose version cannot be read makes the report INCOMPLETE."""

    def setUp(self) -> None:
        self._tmp = Path(tempfile.mkdtemp(prefix="mpm-dogfood-probe-"))
        self.addCleanup(shutil.rmtree, self._tmp, ignore_errors=True)

        self.ws = self._tmp / "workspace"
        run_dir = self.ws / "run"
        run_dir.mkdir(parents=True, exist_ok=True)
        # A scheduler state whose pid is this process — which is alive —
        # so the baseline's scheduler checks pass and the run exercises
        # the healthy path rather than the stop path.
        (run_dir / "scheduler.state").write_text(
            json.dumps(
                {
                    "last_tick_unix": int(time.time()),
                    "process_started_unix": int(time.time()) - 60,
                    "tick_count": 7,
                    "pid": os.getpid(),
                    "last_status": "ok",
                    "last_error": "",
                }
            )
        )
        self.db = _DOGFOOD_FIXTURE.build_fixture(
            self._tmp / "substrate" / "mpm.db", proposals=True
        )
        self.host_db = _DOGFOOD_FIXTURE.build_host_fixture(
            self._tmp / "host" / "opencode.db"
        )

    def _run(self, template: str) -> subprocess.CompletedProcess:
        binary = self._tmp / "mpm"
        binary.write_text(_render(template))
        binary.chmod(0o755)
        env = dict(os.environ)
        env["MPM_WORKSPACE"] = str(self.ws)
        env.pop("MPM_DB_PATH", None)
        return subprocess.run(
            [
                "bash", str(DOGFOOD),
                "--db", str(self.db),
                "--host-db", str(self.host_db),
                "--mpm", str(binary),
                "--no-wake",
            ],
            capture_output=True, text=True, timeout=300, env=env,
        )

    def test_a_successful_probe_is_reported(self) -> None:
        """The report is COMPLETE when the probe succeeds.

        This is the control: it proves the fixture is complete, so the
        exit status in the next test is evidence about the PROBE and not
        about the fixture.
        """
        proc = self._run(GOOD)
        self.assertIn(FAKE_COMMIT, proc.stdout,
                      "the revision must be reported when the probe succeeds")
        self.assertNotIn("could not be read", proc.stdout)
        self.assertEqual(
            proc.returncode, 0,
            "the report must be COMPLETE when every measurement can be taken:\n"
            + proc.stdout[-4000:],
        )

    def test_a_failed_probe_is_a_measurement_gap(self) -> None:
        """THE POINT.

        The baseline's doctrine: a measurement that cannot be taken is
        never reported as a measurement of zero, and it makes the run
        exit 2. A binary that prints a revision and then fails has not
        reported a revision.
        """
        proc = self._run(FAILING)
        self.assertNotIn(FAKE_COMMIT, proc.stdout,
                         "a failed producer's revision must not be reported as the revision")
        self.assertIn("could not be read", proc.stdout)
        self.assertIn("MEASUREMENT GAPS", proc.stdout)
        self.assertEqual(proc.returncode, 2,
                         "an incomplete report must exit 2, not 0")


# --------------------------------------------------------------------------
# The static ratchet.
# --------------------------------------------------------------------------

# The scripts in the issue's scope.
SCOPED = (
    REPO_ROOT / "scripts" / "dogfood_baseline.sh",
    REPO_ROOT / "scripts" / "rehearsal" / "lib.sh",
    REPO_ROOT / "scripts" / "rehearsal" / "10_obligations.sh",
    REPO_ROOT / "scripts" / "rehearsal" / "20_negative.sh",
)

# A version INVOCATION: a path (quoted or bare) followed by the version
# argument.
VERSION_INVOCATION = re.compile(
    r"""(?:"[^"]*"|'[^']*'|[\w./${}-]+)\s+(?:--version|version)\b"""
)

# A helper call whose status is captured with the value.
HELPER_CALL = re.compile(r"rb_version_output|rb_version_first_line|rb_reported_commit")
STATUS_CAPTURED = re.compile(r"\|\|\s*[A-Za-z_][A-Za-z0-9_]*=\$\?")


class TestNoVersionProbeDiscardsItsProducersStatus(unittest.TestCase):
    """The ratchet: the defect shape may not come back.

    A version probe whose output is CAPTURED or PIPED must go through
    one of the release-identity helpers, and the helper's status must be
    captured with the value. A probe that captures the value and drops
    the status is the shape that lets a failed binary satisfy an
    obligation, so it may not reappear.
    """

    def test_no_scoped_script_captures_a_raw_version_probe(self) -> None:
        offenders: list[str] = []
        for path in SCOPED:
            for number, line in enumerate(path.read_text().splitlines(), start=1):
                stripped = line.strip()
                if stripped.startswith("#"):
                    continue
                if not VERSION_INVOCATION.search(line):
                    continue
                # A probe whose result is captured or piped is the
                # defect shape. An INSTRUMENTATION run — one whose
                # output is discarded and whose status is not used for
                # a verdict — is not a probe.
                if "$(" in line or "|" in line:
                    offenders.append(f"{path.relative_to(REPO_ROOT)}:{number}: {stripped}")
        self.assertEqual(
            offenders, [],
            "a version probe captures its output without the producer's status:\n  "
            + "\n  ".join(offenders),
        )

    def test_every_helper_call_captures_the_status(self) -> None:
        offenders: list[str] = []
        for path in SCOPED:
            for number, line in enumerate(path.read_text().splitlines(), start=1):
                stripped = line.strip()
                if stripped.startswith("#"):
                    continue
                if not HELPER_CALL.search(line):
                    continue
                if not STATUS_CAPTURED.search(line):
                    offenders.append(f"{path.relative_to(REPO_ROOT)}:{number}: {stripped}")
        self.assertEqual(
            offenders, [],
            "a release-identity helper is called without capturing its status:\n  "
            + "\n  ".join(offenders),
        )

    def test_the_helpers_exist_where_the_callers_expect_them(self) -> None:
        source = BUNDLE_LIB.read_text()
        for name in ("rb_version_output", "rb_version_first_line", "rb_reported_commit"):
            with self.subTest(helper=name):
                self.assertIn(f"{name}()", source)


if __name__ == "__main__":
    unittest.main()
