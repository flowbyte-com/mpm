#!/usr/bin/env python3
"""test_version_probe_producer_status.py — the release-identity probe must
detect a producer that FAILS, whatever the caller's shell is configured to do.

THE DEFECT
----------
`rb_reported_commit` (scripts/lib/runtime_bundle.sh) asks a binary for its
own build identity:

    "$binary" --version 2>/dev/null | awk '/commit:/{ if (count == 0) { print $2; count = 1 } }'

The probe is a PIPELINE, and a pipeline has no status of its own. Without
`pipefail` its status is the LAST command's — the parser's — and with
`pipefail` it is the rightmost NONZERO status, which is the producer's only
when the producer failed AND the parser did not.

So whether a FAILING producer was noticed depended on the CALLER's shell
configuration, not on the probe:

  * `scripts/assemble-release-bundle.sh` runs with `set -u` only. The
    pipeline's status was the parser's 0, so a binary that printed a valid
    commit and THEN exited nonzero had that commit ACCEPTED, and the
    bundle was published;
  * a helper invoked inside `if ! ...` has `errexit` SUSPENDED for its
    whole body. So even under `pipefail`, `rb_validate_bundle` — which is
    called exactly that way, from install.sh and from the assembly script —
    carried on with the commit the failing producer had printed, and
    accepted the bundle.

The Go mirror of the same five checks (internal/update/verify.go) has
always failed closed: `exec.Command(...).Output()` returns an error when
the exit status is nonzero, and the bundle is refused. The shell side now
agrees with it.

THE FIX
-------
The two statuses are read AT THE PIPELINE BOUNDARY, inside a subshell
whose shell options are the helper's own, so the verdict does not depend
on the caller's `pipefail` and the caller's `errexit` is neither relied on
nor turned off. A caller must then CHECK the status and not only the
value, and `rb_validate_bundle` does exactly that.

WHAT THIS SUITE PINS
--------------------
  * a failing producer is detected in EVERY calling context and under
    EVERY shell option set — the verdict is a property of the probe;
  * a successful producer is still accepted, and the identity is still
    the FIRST commit line;
  * a producer that reports no commit, a malformed commit, or nothing at
    all still yields NO identity (and the validator still refuses);
  * a PARSER failure is detected too;
  * the failure is never reported as 141 — a broken pipe is not what
    happened;
  * the verdict is IDENTICAL under both supported awk behaviours, and the
    probe still asks its parser to read to EOF (the property the
    SIGPIPE suite in test_version_probe_sigpipe.py depends on).

HERMETICITY
-----------
Every root is a fresh mktemp directory. HOME, DATA_ROOT, PREFIX and
BUILD_DIR all point inside it, so nothing resolves into the real ~/.mpm and
no installer ever runs against a live installation. The assembly script is
driven through its OWN validate block, extracted from its own source, so
this suite cannot drift away from the script it covers.
"""

import hashlib
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
ASSEMBLY_SH = REPO_ROOT / "scripts" / "assemble-release-bundle.sh"

FAKE_COMMIT = "1a2b3c4d5e6f70819a2b3c4d5e6f70819a2b3c4d"
SECOND_COMMIT = "9f8e7d6c5b4a30291f8e7d6c5b4a3029189f8e7d"

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
# install.sh into a sourceable library.
STRIP_PATTERNS = (
    re.compile(r"^\s*readonly\s+SCRIPT_DIR\s*="),
    re.compile(r"^\s*readonly\s+PROJECT_ROOT\s*="),
    re.compile(r'^\s*main\s+"\$@"\s*$'),
)

# --------------------------------------------------------------------------
# The awk double.
# --------------------------------------------------------------------------
# It models the probe program — print the Nth whitespace-separated field of
# the FIRST line matching /regex/ — and it logs the program it modelled to
# $AWK_DOUBLE_LOG, so a test can prove the double really modelled the
# probe instead of silently delegating to the real awk.
#
# $AWK_DOUBLE_EXIT_STATUS makes the double fail, so the parser-failure
# path can be exercised without depending on the host's awk.
AWK_DOUBLE = """#!/usr/bin/env python3
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

for line in sys.stdin:
    if pattern.search(line):
        fields = line.split()
        if len(fields) > index:
            sys.stdout.write(fields[index] + "\\n")
            sys.stdout.flush()

# The modelled program has no `exit`, so the double reads to EOF — the
# behaviour of mawk and nawk. The double therefore models the reading-to-
# EOF awk, and the test asserts the probe's program never asks for the
# other behaviour (which is what would let an awk close the pipe early).
sys.exit(int(os.environ.get("AWK_DOUBLE_EXIT_STATUS", "0")))
"""

# --------------------------------------------------------------------------
# The producers under test.
# --------------------------------------------------------------------------

# Reports the commit and succeeds.
GOOD_PRODUCER = """\
#!/usr/bin/env bash
if [ "${1:-}" = '--version' ]; then
  echo 'MPM 0.1.0-dev+g%SHORT%'
  echo '  commit:  %COMMIT%'
fi
exit 0
"""

# Reports the commit and THEN FAILS. This is the shape the fix exists
# for: the identity is on stdout, but the producer did not succeed.
FAILING_PRODUCER = """\
#!/usr/bin/env bash
if [ "${1:-}" = '--version' ]; then
  echo 'MPM 0.1.0-dev+g%SHORT%'
  echo '  commit:  %COMMIT%'
  exit 7
fi
exit 0
"""

# Reports TWO commit lines. The first one is the identity.
MULTI_COMMIT_PRODUCER = """\
#!/usr/bin/env bash
if [ "${1:-}" = '--version' ]; then
  echo '  commit:  %COMMIT%'
  echo '  commit:  0000000000000000000000000000000000000000'
fi
exit 0
"""

# A "commit:" label with no value.
MALFORMED_PRODUCER = """\
#!/usr/bin/env bash
if [ "${1:-}" = '--version' ]; then
  echo 'MPM ???'
  echo '  commit:'
  echo '  nonsense'
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

# Says nothing whatsoever.
EMPTY_PRODUCER = """\
#!/usr/bin/env bash
if [ "${1:-}" = '--version' ]; then
  :
fi
exit 0
"""

# The shell option sets the probe must be immune to.
OPTION_SETS = (
    ("set -u", "-u"),
    ("set -eu", "-eu"),
    ("set -euo pipefail", "-euo pipefail"),
)


def add_host_assets(bundle: Path) -> None:
    """Give the bundle the release-scoped host assets, declared.

    A bundle that omits them installs an installation whose own
    managed block points at files that do not exist, so the validator
    refuses one. The fixture therefore has to be a WELL-FORMED bundle,
    and "well-formed" now includes the declaration the validator reads
    (config/runtime-assets.json) and the payload declaration that
    travels beside the assets.
    """
    assets = {
        "agent_installation/MPM_AGENT_INTEGRATION_SNIPPETS.md": "# canonical\n",
        "agent_installation/mpm-agent-protocol.md": "# protocol\n",
        "agent_installation/scripts/block_provenance.py": "# shared\n",
    }
    declaration = {}
    for rel, body in assets.items():
        p = bundle / rel
        p.parent.mkdir(parents=True, exist_ok=True)
        p.write_text(body)
        digest = hashlib.sha256(body.encode("utf-8")).hexdigest()
        declaration[rel] = {"sha256": digest}
    payload_decl = bundle / "agent_installation" / "runtime_payloads.json"
    payload_decl.parent.mkdir(parents=True, exist_ok=True)
    payload_decl.write_text(
        json.dumps({"version": 1, "payloads": []}, indent=2) + "\n")
    manifest = bundle / "config" / "runtime-assets.json"
    manifest.parent.mkdir(parents=True, exist_ok=True)
    manifest.write_text(
        json.dumps({"version": 1, "assets": declaration}, indent=2,
                   sort_keys=True) + "\n")
    # The schema preflight. A bundle without it degrades the
    # schema-collision check to a no-op, so the validator refuses one:
    # "well-formed" now includes it.
    schema = bundle / "schema" / "lifecycle_events.sql"
    schema.parent.mkdir(parents=True, exist_ok=True)
    schema.write_text(
        "CREATE TABLE IF NOT EXISTS lifecycle_events (\n"
        "\tevent_id TEXT PRIMARY KEY NOT NULL\n"
        ");\n")


class ProbeFixture(unittest.TestCase):
    """A disposable installation, a fake build tree and the awk double."""

    def setUp(self):
        self._tmp = Path(tempfile.mkdtemp(prefix="mpm-producer-status-"))
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

    def _producer(self, template, where=None):
        body = (template
                .replace("%SHORT%", FAKE_COMMIT[:8])
                .replace("%COMMIT%", FAKE_COMMIT))
        p = (where or self.build_dir) / "mpm"
        p.write_text(body)
        p.chmod(0o755)
        return p

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
    def _run(self, script, extra=None, timeout=300):
        driver = self._tmp / "_drive.sh"
        driver.write_text(script)
        driver.chmod(0o755)
        return subprocess.run(
            ["bash", str(driver)], capture_output=True, text=True,
            timeout=timeout, env=self._env(extra))

    def drive_helper(self, binary, options, guarded=False):
        """Run the REAL rb_reported_commit under a given shell option set.

        `guarded` calls it from inside a function invoked in an `if !`
        condition — the shape that suspends `errexit` for the whole body,
        which is how rb_validate_bundle is called everywhere.
        """
        if guarded:
            body = textwrap.dedent(f"""\
                rc=0
                probe() {{ v="$(rb_reported_commit "{binary}")"; }}
                if ! probe "{binary}"; then rc=1; else rc=0; fi
                printf 'rc=%s\\nvalue=%s\\n' "$rc" "${{v:-}}"
                """)
        else:
            body = textwrap.dedent(f"""\
                rc=0
                v="$(rb_reported_commit "{binary}")" || rc=$?
                printf 'rc=%s\\nvalue=%s\\n' "$rc" "${{v:-}}"
                """)
        script = textwrap.dedent(f"""\
            {options}
            . "{BUNDLE_LIB}"
            DATA_ROOT="{self.data}"
            HOME="{self.home}"
            export DATA_ROOT HOME
            {body}
            """)
        r = self._run(script)
        rc = None
        value = None
        for line in r.stdout.splitlines():
            if line.startswith("rc="):
                rc = int(line[3:])
            elif line.startswith("value="):
                value = line[6:]
        self.assertIsNotNone(rc, f"the driver produced no verdict\n{r.stdout}\n{r.stderr}")
        return rc, value, r

    def drive_validator(self, bundle, options, guarded=False):
        """Run the REAL rb_validate_bundle, guarded or not.

        The driver's verdict is read from its own output, not from its
        exit status: an unguarded call under `set -u` alone does NOT
        stop the caller, so a driver that simply fell through to the
        next statement would report the opposite of what happened.
        """
        if guarded:
            body = textwrap.dedent(f"""\
                if ! rb_validate_bundle "{bundle}" "{REV}"; then
                  verdict=REFUSED
                else
                  verdict=ACCEPTED
                fi
                printf 'verdict=%s\\n' "$verdict"
                """)
        else:
            body = textwrap.dedent(f"""\
                rc=0
                rb_validate_bundle "{bundle}" "{REV}" || rc=$?
                if [ "$rc" -eq 0 ]; then verdict=ACCEPTED; else verdict=REFUSED; fi
                printf 'verdict=%s\\nrc=%s\\n' "$verdict" "$rc"
                """)
        script = textwrap.dedent(f"""\
            {options}
            . "{BUNDLE_LIB}"
            DATA_ROOT="{self.data}"
            HOME="{self.home}"
            export DATA_ROOT HOME
            {body}
            """)
        r = self._run(script)
        return r

    @staticmethod
    def verdict_of(r):
        """The ACCEPTED/REFUSED verdict a driver reported."""
        for line in r.stdout.splitlines():
            if line.startswith("verdict="):
                return line[8:]
        raise AssertionError(
            f"the driver reported no verdict\nstdout:\n{r.stdout}\n"
            f"stderr:\n{r.stderr}")

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

    def drive_assembly_validate(self, bundle):
        """Run the release assembly script's OWN validate block.

        The block is extracted from the script's own source and executed
        under the script's own shell options, so this suite cannot drift
        away from the script it covers. If the script's shape changes and
        the block can no longer be found, the test FAILS rather than
        passing vacuously.
        """
        src = ASSEMBLY_SH.read_text()
        block = re.search(
            r"^if ! rb_validate_bundle .*?^fi$", src, re.S | re.M)
        self.assertIsNotNone(
            block,
            "the release assembly script no longer contains the validate "
            "block this suite drives — the script's shape changed and the "
            "suite must be updated to follow it")
        opts = re.search(r"^set (\S+)$", src, re.M)
        self.assertIsNotNone(
            opts,
            "the release assembly script no longer declares its shell "
            "options — the suite must be updated to follow it")
        work = self._tmp / "asm-work"
        work.mkdir(exist_ok=True)
        script = textwrap.dedent(f"""\
            set {opts.group(1)}
            . "{BUNDLE_LIB}"
            DATA_ROOT="{self.data}"
            HOME="{self.home}"
            CANDIDATE="{bundle}"
            TAG="{REV}"
            WORK="{work}"
            export DATA_ROOT HOME CANDIDATE TAG WORK
            {block.group(0)}
            echo "PUBLISHED"
            """)
        return self._run(script)

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
            "built_at": "2026-11-04T00:00:00Z",
            "bundle_layout": "versioned-bundle/1",
        }))
        add_host_assets(bundle)
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

    def assert_the_probe_asks_the_parser_to_read_to_eof(self):
        """The probe's program must not ask the parser to stop early.

        This is the property that makes the probe safe under BOTH
        supported awk behaviours: an awk that honours `exit` closes the
        pipe at the match and kills a producer that still has output to
        write, while mawk and nawk read to EOF. A program with no `exit`
        is safe under both — and it is what the SIGPIPE suite in
        test_version_probe_sigpipe.py depends on.
        """
        modelled = self.awk_log.read_text() if self.awk_log.is_file() else ""
        self.assertNotIn(
            "exit", modelled,
            "the probe's awk program asks the parser to exit at the match "
            "— an awk that honours `exit` would close the pipe while the "
            "producer may still have output to write, and the producer "
            "would take SIGPIPE")


class TestProducerStatusIsTheHelpersBusiness(ProbeFixture):
    """The probe's verdict must not depend on the caller's shell."""

    def test_a_failing_producer_is_detected_under_every_option_set(self):
        # THE POINT. The producer prints a valid commit and then exits 7.
        # Before the fix the probe reported the commit and returned 0
        # whenever the caller had no `pipefail` — so the identity of a
        # binary that FAILED was accepted.
        binary = self._producer(FAILING_PRODUCER)
        for options, _ in OPTION_SETS:
            for guarded in (False, True):
                with self.subTest(options=options, guarded=guarded):
                    rc, value, r = self.drive_helper(binary, options, guarded)
                    self.assertNotEqual(
                        rc, 0,
                        "the probe accepted a producer that exited 7 — the "
                        "verdict depends on the caller's shell, not on the "
                        f"probe\noptions: {options} guarded: {guarded}\n"
                        f"stdout:\n{r.stdout}\nstderr:\n{r.stderr}")
                    self.assertNotEqual(
                        rc, 141,
                        "the probe reported a broken pipe (141) — the "
                        "producer failed, and that is not a broken pipe")
                    self.assertEqual(
                        value, "",
                        "the probe printed an identity for a producer that "
                        "failed — a failed probe must yield no identity")

    def test_a_failing_producer_is_refused_by_the_validator(self):
        # rb_validate_bundle is called inside `if ! ...` everywhere, and
        # errexit is SUSPENDED for the body of a function invoked in a
        # condition context — so the failed assignment did not stop the
        # validation, and the bundle was accepted.
        bundle = self.make_bundle(producer=FAILING_PRODUCER)
        for options, _ in OPTION_SETS:
            for guarded in (False, True):
                with self.subTest(options=options, guarded=guarded):
                    r = self.drive_validator(bundle, options, guarded)
                    self.assertEqual(
                        self.verdict_of(r), "REFUSED",
                        "the bundle validator accepted a bundle whose mpm "
                        "binary failed to report its commit\n"
                        f"options: {options} guarded: {guarded}\n"
                        f"stdout:\n{r.stdout}\nstderr:\n{r.stderr}")
                    self.assertNotEqual(
                        r.returncode, 141,
                        "the validator reported a broken pipe (141) — the "
                        f"producer failed, and that is not a broken pipe\n"
                        f"stdout:\n{r.stdout}\nstderr:\n{r.stderr}")

    def test_the_release_assembly_script_refuses_a_failing_producer(self):
        # The assembly script is the publisher: it runs with `set -u`
        # only, so before the fix it published a bundle whose binary
        # failed to report its own identity.
        bundle = self.make_bundle(producer=FAILING_PRODUCER)
        r = self.drive_assembly_validate(bundle)
        self.assertNotIn(
            "PUBLISHED", r.stdout,
            "the release assembly script published a bundle whose mpm "
            f"binary failed to report its commit\nstdout:\n{r.stdout}\n"
            f"stderr:\n{r.stderr}")
        self.assertNotEqual(
            r.returncode, 141,
            "the assembly script reported a broken pipe (141) — the "
            f"producer failed, and that is not a broken pipe\n"
            f"stdout:\n{r.stdout}\nstderr:\n{r.stderr}")

    def test_the_installer_refuses_a_failing_producer(self):
        # The installer's own probe. It must stop with a diagnostic, and
        # it must NOT reach the step after the identity is taken.
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
        self.assertIn(
            "failed to report its build commit", r.stderr,
            "the installer did not say why it stopped\n"
            f"stdout:\n{r.stdout}\nstderr:\n{r.stderr}")


class TestSuccessfulProducersStillWork(ProbeFixture):
    """The correction must not cost a single working case."""

    def test_a_successful_producer_is_still_accepted(self):
        binary = self._producer(GOOD_PRODUCER)
        for options, _ in OPTION_SETS:
            for guarded in (False, True):
                with self.subTest(options=options, guarded=guarded):
                    rc, value, r = self.drive_helper(binary, options, guarded)
                    self.assertEqual(rc, 0, r.stdout + r.stderr)
                    self.assertEqual(value, FAKE_COMMIT, r.stdout + r.stderr)

    def test_the_first_matching_commit_is_still_the_one_reported(self):
        binary = self._producer(MULTI_COMMIT_PRODUCER)
        rc, value, r = self.drive_helper(binary, "set -euo pipefail")
        self.assertEqual(rc, 0, r.stdout + r.stderr)
        self.assertEqual(
            value, FAKE_COMMIT,
            "the probe reported a commit other than the first matching "
            f"one\nstdout:\n{r.stdout}\nstderr:\n{r.stderr}")

    def test_a_producer_that_reports_nothing_yields_no_identity(self):
        # A producer that succeeds but reports no commit is NOT a failure:
        # the probe succeeds and reports nothing, and the CALLER decides
        # that an empty identity is not acceptable. The distinction
        # matters — the installer falls back to the provenance script
        # for an empty identity, and refuses outright for a failed probe.
        for name, template in (("no commit", NO_COMMIT_PRODUCER),
                               ("malformed", MALFORMED_PRODUCER),
                               ("empty", EMPTY_PRODUCER)):
            with self.subTest(producer=name):
                binary = self._producer(template)
                rc, value, r = self.drive_helper(binary, "set -euo pipefail")
                self.assertEqual(rc, 0, r.stdout + r.stderr)
                self.assertEqual(value, "", r.stdout + r.stderr)

    def test_a_bundle_whose_binary_reports_nothing_is_still_refused(self):
        for name, template in (("no commit", NO_COMMIT_PRODUCER),
                               ("malformed", MALFORMED_PRODUCER),
                               ("empty", EMPTY_PRODUCER)):
            with self.subTest(producer=name):
                bundle = self.make_bundle(producer=template,
                                          name=f"empty-{name.replace(' ', '-')}")
                r = self.drive_validator(bundle, "set -euo pipefail")
                self.assertEqual(
                    self.verdict_of(r), "REFUSED", r.stdout + r.stderr)
                self.assertIn(
                    "did not report a build commit", r.stderr,
                    r.stdout + r.stderr)

    def test_a_well_formed_bundle_still_validates(self):
        bundle = self.make_bundle(producer=GOOD_PRODUCER)
        r = self.drive_validator(bundle, "set -euo pipefail")
        self.assertEqual(self.verdict_of(r), "ACCEPTED", r.stdout + r.stderr)

    def test_a_mixed_bundle_is_still_refused(self):
        bundle = self.make_bundle(commit=FAKE_COMMIT,
                                  producer=GOOD_PRODUCER)
        (bundle / "bin" / "mpm").write_text(
            GOOD_PRODUCER
            .replace("%SHORT%", SECOND_COMMIT[:8])
            .replace("%COMMIT%", SECOND_COMMIT))
        (bundle / "bin" / "mpm").chmod(0o755)
        r = self.drive_validator(bundle, "set -euo pipefail")
        self.assertEqual(self.verdict_of(r), "REFUSED", r.stdout + r.stderr)
        self.assertIn("mixture", r.stderr, r.stdout + r.stderr)

    def test_the_installer_still_reports_the_binarys_own_identity(self):
        self._producer(GOOD_PRODUCER)
        r = self.drive_phase_bundle()
        self.assertNotEqual(r.returncode, 141, r.stdout + r.stderr)
        self.assertIn(
            f"commit:    {FAKE_COMMIT}", r.stderr, r.stdout + r.stderr)

    def test_the_installer_still_falls_back_when_no_commit_is_reported(self):
        # No commit line: the probe yields nothing, and the installer
        # falls back to the provenance script. The fallback is for an
        # EMPTY identity — never for a FAILED probe.
        self._producer(NO_COMMIT_PRODUCER)
        r = self.drive_phase_bundle()
        self.assertNotEqual(r.returncode, 141, r.stdout + r.stderr)
        self.assertNotIn(f"commit:    {FAKE_COMMIT}", r.stderr)
        self.assertIn("commit:    unknown", r.stderr, r.stdout + r.stderr)


class TestParserFailureIsDetected(ProbeFixture):
    """The parser's own failure must be detected too."""

    def test_a_parser_that_fails_is_reported(self):
        binary = self._producer(GOOD_PRODUCER)
        r = self._run(textwrap.dedent(f"""\
            set -euo pipefail
            . "{BUNDLE_LIB}"
            rc=0
            v="$(rb_reported_commit "{binary}")" || rc=$?
            printf 'rc=%s\\nvalue=%s\\n' "$rc" "${{v:-}}"
            """), extra={"AWK_DOUBLE_EXIT_STATUS": "3"})
        self.assertIn("rc=3", r.stdout, r.stdout + r.stderr)
        self.assertNotIn("value=" + FAKE_COMMIT, r.stdout, r.stdout + r.stderr)


class TestBothSupportedAwkBehaviours(ProbeFixture):
    """The verdict must not depend on which awk parses the output."""

    def test_the_verdict_is_identical_under_both_awk_behaviours(self):
        # With the double on PATH the probe's program is modelled by the
        # double (which reads to EOF, like mawk and nawk). Without it the
        # HOST's real awk does the parsing — gawk or busybox awk on a
        # host that has them, mawk on a host that has not. The verdict
        # must be the same either way.
        binary = self._producer(FAILING_PRODUCER)

        with_double, value_with, _ = self.drive_helper(
            binary, "set -euo pipefail")
        self.assert_double_modelled_the_probe()
        self.assert_the_probe_asks_the_parser_to_read_to_eof()

        # Now without the double: the real awk parses.
        env_path = os.environ["PATH"]
        try:
            os.environ["PATH"] = env_path
            driver = self._tmp / "_drive_real_awk.sh"
            driver.write_text(textwrap.dedent(f"""\
                set -euo pipefail
                . "{BUNDLE_LIB}"
                rc=0
                v="$(rb_reported_commit "{binary}")" || rc=$?
                printf 'rc=%s\\nvalue=%s\\n' "$rc" "${{v:-}}"
                """))
            driver.chmod(0o755)
            env = self._env()
            env["PATH"] = env_path  # NO double: the host's awk
            r = subprocess.run(["bash", str(driver)], capture_output=True,
                               text=True, timeout=300, env=env)
        finally:
            os.environ["PATH"] = env_path

        rc_real = None
        value_real = None
        for line in r.stdout.splitlines():
            if line.startswith("rc="):
                rc_real = int(line[3:])
            elif line.startswith("value="):
                value_real = line[6:]
        self.assertIsNotNone(rc_real, f"{r.stdout}\n{r.stderr}")
        self.assertEqual(
            (rc_real, value_real), (with_double, value_with),
            "the probe's verdict differs between the modelled awk and the "
            f"host's real awk\nstdout:\n{r.stdout}\nstderr:\n{r.stderr}")


if __name__ == "__main__":
    unittest.main()
