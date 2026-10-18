"""
test_script_failure_semantics.py — Regression coverage for two masked
failure idioms found in the 2026-10-02 repository scripts audit.

Both are static-contract tests over the script source plus, where a
behaviour can be reproduced hermetically, a real execution against a
stub binary. Nothing here touches a live workspace, a database, a
service, or the network.

--------------------------------------------------------------------------
Defect 1 — scripts/smoke_telemetry.sh: EXIT trap aborted by errexit
--------------------------------------------------------------------------

    "$BIN" serve --quiet &
    SERVE_PID=$!
    trap 'kill $SERVE_PID 2>/dev/null; rm -rf "$TMP"' EXIT
    ...
    kill $SERVE_PID 2>/dev/null || true
    wait  $SERVE_PID 2>/dev/null || true
    echo "==> smoke_telemetry: PASS"

By the time the trap fires, `wait` has already reaped SERVE_PID, so
`kill $SERVE_PID` returns 1 ("no such process"). A failing command
inside an EXIT trap is subject to errexit, which aborts the rest of the
trap body — so `rm -rf "$TMP"` never ran (workspace leaked) and the
script's final status became kill's 1 rather than the intended 0.

Observed before the fix, from a faithful reproduction of the same
lines:

    ==> smoke_telemetry: PASS
    FINAL_STATUS=1
    LEAK CONFIRMED: /tmp/tmp.msBi8zlxIt still exists

The script printed PASS, exited non-zero, and leaked its workspace.

--------------------------------------------------------------------------
Defect 2 — scripts/verify-m3-audit-fixes.sh: D-006 stderr assertion
could not fail
--------------------------------------------------------------------------

    call_only() {
      "$MPM" call "$@" 2>/dev/null
    }
    ...
    call_only mpm_memory --payload '...' 2>"$TMP_ERR" >/dev/null
    STDERR_BYTES=$(wc -c <"$TMP_ERR")
    if [[ "$STDERR_BYTES" -eq 0 ]]; then
      assert_pass "stderr is 0 bytes for clean machine call"

A redirection inside a function body supersedes the redirection the
caller applied to the function invocation, so nothing ever reached
$TMP_ERR. STDERR_BYTES was structurally 0 and the assertion passed
unconditionally. D-006 is a regression test for a real audit finding
(machine-mode stderr leakage) — it could not detect a regression.

Proven with a stub that writes 79 bytes to stderr on every call: the
script still reported "stderr is 0 bytes for clean machine call".

The sibling scripts/verify-wishlist-fixes.sh was already correct — it
invokes "$MPM" directly rather than through a stderr-suppressing
helper — and is pinned here as a contrast case.

--------------------------------------------------------------------------
Defect 3 — scripts/deploy.sh: a scheduler restart that was never verified
--------------------------------------------------------------------------

    SCHED_ACTIVE_BEFORE=$(systemctl --user is-active mpm-scheduler ...)
    if [[ "$SCHED_ACTIVE_BEFORE" == "active" ]]; then
      systemctl --user restart mpm-scheduler
    fi
    ...
    LIVE_PID=$(pgrep -f "$CANONICAL_BIN/mpm-mcp" | head -1 || true)
    echo "==> Deployment complete."

deploy.sh exists to eliminate the "installed but not restarted" failure
mode — see its own header. It restarted mpm-scheduler and mpm-telemetry
and then verified neither. The only liveness assertion in the script
covered mpm-mcp, a different unit, spawned by the gateway rather than by
systemd.

`systemctl restart` exiting 0 means systemd accepted the job. It is not
evidence the unit stayed up: a unit that crash-loops against a newly
installed binary reports success from `restart` and then sits in
`failed`. The script would print "Deployment complete." with the
scheduler down — the same outcome the script was written to prevent.

The audit that surfaced it also confirmed the rest of the tree is
correct: every other systemctl invocation in every shell script in the
checkout uses `--user`, because mpm-scheduler is a USER-level unit. The
2026-10-13 embedding migration shipped a scratch script that did not,
and lost the scheduler that way. Those three properties are pinned
below — each with a negative control, because the first version of the
liveness rule passed against the unfixed script (see
`_is_verification`, where the read that precedes a restart is not
evidence of anything after it).
"""

from __future__ import annotations

import os
import shutil
import subprocess
import tempfile
import unittest
from pathlib import Path

REPO_ROOT = Path(__file__).resolve().parents[2]
SCRIPTS = REPO_ROOT / "scripts"
SMOKE_TELEMETRY = SCRIPTS / "smoke_telemetry.sh"
VERIFY_M3 = SCRIPTS / "verify-m3-audit-fixes.sh"
VERIFY_WISHLIST = SCRIPTS / "verify-wishlist-fixes.sh"
DEPLOY = SCRIPTS / "deploy.sh"

SCHEDULER_UNIT = "mpm-scheduler"

# Directories whose shell files are fixtures or vendored trees, not scripts
# a maintainer runs against a live host. A stub that records systemctl calls
# contains deliberately malformed invocations that are the subject under
# test; auditing them would report the harness rather than the defect.
_SKIP_DIRS = {".git", "node_modules", "testdata", "fixtures", "tests"}

# A line opening with one of these emits a message; its `systemctl` is inside
# a quoted string for the operator, not a call.
_MESSAGE_VERBS = ('log "', 'warn "', 'err "', 'note "', 'die "', 'echo "', 'printf "')


def _is_comment(line: str) -> bool:
    return line.strip().startswith("#")


def _invokes_systemctl(line: str) -> bool:
    """True when a line runs systemctl, as opposed to probing or printing it.

    `command -v systemctl` asks whether the binary exists; it names no unit
    and is scope-free by construction.
    """
    if _is_comment(line) or "systemctl" not in line:
        return False
    if "command -v systemctl" in line:
        return False
    return not any(line.strip().startswith(v) for v in _MESSAGE_VERBS)


def _is_capture(line: str, needle: str) -> bool:
    """True when `needle` sits inside a $(...) result, i.e. a read."""
    idx = line.find(needle)
    return idx >= 0 and "$(" in line[:idx]


def _repo_scripts() -> dict[str, list[str]]:
    """Every shell script in the checkout, as relpath -> lines."""
    out: dict[str, list[str]] = {}
    for path in REPO_ROOT.rglob("*.sh"):
        rel = path.relative_to(REPO_ROOT)
        if _SKIP_DIRS & set(rel.parts):
            continue
        if rel.name.endswith("_test.sh"):
            continue
        text = path.read_text().replace("\r\n", "\n")
        out[rel.as_posix()] = text.split("\n")
    return out


def _scope_violations(files: dict[str, list[str]]) -> list[tuple[str, int, str]]:
    """systemctl calls that name no manager scope."""
    bad = []
    for rel, lines in files.items():
        for n, line in enumerate(lines, 1):
            if not _invokes_systemctl(line):
                continue
            # `sudo systemctl` addresses a legacy SYSTEM-level unit at
            # /etc/systemd/system/mpm-scheduler.service, which the
            # unprivileged installer cannot remove and can only ask the
            # operator to disable by hand. Those lines are warnings about a
            # different unit, and sudo is the correct scope for it.
            if "sudo systemctl" in line:
                continue
            if "systemctl --user" not in line:
                bad.append((rel, n, line.strip()))
    return bad


def _is_lifecycle_call(line: str) -> bool:
    return "systemctl" in line and SCHEDULER_UNIT in line and (
        " restart " in line or " start " in line
    )


def _is_verification(line: str) -> bool:
    """True when a line asserts liveness rather than reading prior state.

    A probe bound to a variable does not count:
    `X=$(systemctl --user is-active "$unit")` reports the PRE-restart state,
    which is how both install.sh and deploy.sh decide whether to restart at
    all. Counting it made the first version of this rule vacuous — the
    mpm-telemetry probe on the line after the mpm-scheduler restart
    satisfied the scheduler's check, and the test passed against the
    unfixed script.
    """
    return "is-active" in line and not _is_capture(line, "is-active")


def _liveness_violations(files: dict[str, list[str]]) -> list[tuple[str, int, str]]:
    """Scripts that start or restart the scheduler and never assert it is up."""
    bad = []
    for rel, lines in files.items():
        restarts = [n for n, line in enumerate(lines) if _is_lifecycle_call(line)]
        if not restarts:
            continue
        after = restarts[-1] + 1
        if not any(_is_verification(lines[n]) for n in range(after, len(lines))):
            bad.append((rel, restarts[-1] + 1, ""))
    return bad


def _discard_violations(files: dict[str, list[str]]) -> list[tuple[str, int, str]]:
    """Scheduler lifecycle calls whose exit status is thrown away.

    Scoped to the scheduler deliberately. install.sh's mpm-telemetry start
    and daemon-reload calls legitimately end in `|| true`: telemetry is
    optional, and a .desktop autostart shim that fails on an absent
    optional unit is worse than one that starts the scheduler and moves on.
    A different service with a different contract — generalising this rule
    would have meant an allowlist full of judgement calls, which is how a
    test stops being load-bearing.
    """
    bad = []
    for rel, lines in files.items():
        for n, line in enumerate(lines, 1):
            if _is_comment(line) or not _is_lifecycle_call(line):
                continue
            for discard in ("|| true", "|| :", "||true"):
                if discard in line:
                    bad.append((rel, n, discard))
    return bad


def _run_bash(source: str) -> subprocess.CompletedProcess:
    """Run a bash fragment in a scratch dir; never touches the repo."""
    tmp = Path(tempfile.mkdtemp(prefix="mpm-script-semantics."))
    try:
        script = tmp / "case.sh"
        script.write_text(source)
        return subprocess.run(
            ["bash", str(script)],
            capture_output=True,
            text=True,
            cwd=tmp,
            timeout=60,
        )
    finally:
        shutil.rmtree(tmp, ignore_errors=True)


class SmokeTelemetryExitTrap(unittest.TestCase):
    """The trap must survive its own kill and still clean up."""

    def setUp(self) -> None:
        self.src = SMOKE_TELEMETRY.read_text()

    def test_exit_trap_kill_is_guarded(self):
        # The `|| true` is load-bearing, not defensive noise: without it the
        # reap-then-kill ordering makes the trap's first command fail and
        # errexit skips `rm -rf "$TMP"`.
        self.assertRegex(
            self.src,
            r"trap 'kill \$SERVE_PID 2>/dev/null \|\| true; rm -rf \"\$TMP\"' EXIT",
        )

    def test_reproduction_now_exits_zero_and_leaks_nothing(self):
        """Faithful reproduction of the trap/reap/wait ordering."""
        with tempfile.TemporaryDirectory(prefix="mpm-trap-repro.") as td:
            repro = Path(td) / "repro.sh"
            repro.write_text(
                "set -euo pipefail\n"
                f'TMP=$(mktemp -d -p {td})\n'
                "echo \"$TMP\" > \"$TD_TMPFILE\"\n"
                "sleep 300 &\n"
                "SERVE_PID=$!\n"
                'trap \'kill $SERVE_PID 2>/dev/null || true; rm -rf "$TMP"\' EXIT\n'
                "kill $SERVE_PID 2>/dev/null || true\n"
                "wait $SERVE_PID 2>/dev/null || true\n"
                "echo '==> smoke_telemetry: PASS'\n"
            )
            tmpfile = Path(td) / "which_tmp"
            env = dict(os.environ, TD_TMPFILE=str(tmpfile))
            r = subprocess.run(
                ["bash", str(repro)],
                capture_output=True,
                text=True,
                cwd=td,
                env=env,
                timeout=60,
            )
            self.assertIn("PASS", r.stdout)
            self.assertEqual(
                r.returncode,
                0,
                f"trap must not override the script's exit status\n{r.stdout}\n{r.stderr}",
            )
            leaked = tmpfile.read_text().strip()
            self.assertFalse(
                Path(leaked).exists(),
                f"EXIT trap did not remove the workspace: {leaked}",
            )

    def test_unguarded_trap_really_does_fail(self):
        """Negative control: without `|| true` the defect reappears.

        Guards against a vacuous test: if this ever stops failing, the
        test above has stopped proving anything.
        """
        r = _run_bash(
            "set -euo pipefail\n"
            'TMP=$(mktemp -d)\n'
            "echo \"$TMP\" > ./which_tmp\n"
            "sleep 300 &\n"
            "SERVE_PID=$!\n"
            "trap 'kill $SERVE_PID 2>/dev/null; rm -rf \"$TMP\"' EXIT\n"
            "kill $SERVE_PID 2>/dev/null || true\n"
            "wait $SERVE_PID 2>/dev/null || true\n"
            "echo PASS\n"
        )
        self.assertNotEqual(
            r.returncode, 0, "unguarded trap unexpectedly succeeded; control is void"
        )
        self.assertIn("PASS", r.stdout)


class VerifyM3StderrAssertion(unittest.TestCase):
    """D-006 must be able to observe stderr."""

    def setUp(self) -> None:
        self.src = VERIFY_M3.read_text()

    def test_d006_call_does_not_go_through_call_only(self):
        # A helper whose body ends in `2>/dev/null` swallows the caller's
        # own stderr redirection, making the byte count structurally 0.
        self.assertNotRegex(
            self.src,
            r'call_only [^\n]*2>"\$TMP_ERR"',
            "D-006 measures stderr through call_only, whose body redirects "
            "fd 2 to /dev/null and supersedes the caller's redirection",
        )

    def test_d006_measures_stderr_from_a_real_invocation(self):
        self.assertRegex(
            self.src,
            r'"\$MPM" call mpm_memory[^\n]*2>"\$TMP_ERR"',
        )

    def test_call_only_is_still_used_elsewhere(self):
        """It remains a legitimate helper for output-only call sites."""
        self.assertIn("call_only() {", self.src)
        self.assertGreater(
            len([l for l in self.src.splitlines() if "call_only " in l and "()" not in l]),
            3,
        )

    def _d006_verdict(self, stub_body: str) -> str:
        """Run the D-006 measurement against a stub binary."""
        with tempfile.TemporaryDirectory(prefix="mpm-d006.") as td:
            stub = Path(td) / "mpm"
            stub.write_text(stub_body)
            stub.chmod(0o755)
            case = Path(td) / "case.sh"
            case.write_text(
                "set -uo pipefail\n"
                f'MPM={stub}\n'
                "call_only() { \"$MPM\" call \"$@\" 2>/dev/null; }\n"
                "TMP_ERR=$(mktemp)\n"
                # This is the assertion under test, in its fixed form.
                '"$MPM" call mpm_memory --payload \'{"action":"query"}\''
                ' 2>"$TMP_ERR" >/dev/null\n'
                'STDERR_BYTES=$(wc -c <"$TMP_ERR")\n'
                "if [[ \"$STDERR_BYTES\" -eq 0 ]]; then echo PASS; "
                "else echo \"FAIL $STDERR_BYTES\"; fi\n"
            )
            r = subprocess.run(
                ["bash", str(case)], capture_output=True, text=True, timeout=60
            )
            return r.stdout.strip()

    def test_noisy_stub_is_now_detected(self):
        noisy = (
            "#!/usr/bin/env bash\n"
            "echo '{\"result\":\"ok\"}'\n"
            "echo 'human-readable chatter on stderr' >&2\n"
        )
        verdict = self._d006_verdict(noisy)
        self.assertIn("FAIL", verdict, f"stderr leak went undetected: {verdict}")
        self.assertNotIn("PASS", verdict)

    def test_silent_stub_still_passes(self):
        silent = "#!/usr/bin/env bash\necho '{\"result\":\"ok\"}'\n"
        verdict = self._d006_verdict(silent)
        self.assertIn("PASS", verdict, f"clean call falsely failed: {verdict}")


class VerifyWishlistContrast(unittest.TestCase):
    """The sibling was already correct; pin it so it stays correct."""

    def test_wishlist_measures_stderr_without_a_suppressing_helper(self):
        src = VERIFY_WISHLIST.read_text()
        self.assertNotIn("call_only", src)
        self.assertRegex(
            src,
            r'"\$MPM" call mpm_memory[^\n]*\\\n\s*2>"\$TMP_ERR"',
        )


class DeployLivenessRejectsFailedUnit(unittest.TestCase):
    """Defect 3, executed: a restart that succeeds but a service that is down.

    The static rule above proves deploy.sh contains an is-active check.
    This proves the check does something — that with `systemctl restart`
    returning 0 and `systemctl is-active` returning non-zero, the script
    aborts instead of proceeding to "Deployment complete.".

    HERMETIC BY CONSTRUCTION. Only the extracted step-5 block runs, and
    `systemctl` is a stub on PATH in a temp dir. No real unit is
    contacted, stopped, or restarted, and the block is the only thing in
    the harness that can invoke systemctl at all.
    """

    def setUp(self) -> None:
        self.src = DEPLOY.read_text()
        self.start = self.src.index("# 5. Verify the restarted units are actually live")
        self.end = self.src.index("# 6. Verify live mpm-mcp is the new binary")
        self.block = self.src[self.start : self.end]

    def _run(self, is_active_exit: int) -> tuple[subprocess.CompletedProcess, str]:
        """Run the real block against a stub systemctl; return (result, calls)."""
        with tempfile.TemporaryDirectory(prefix="mpm-deploy-liveness.") as td:
            td_path = Path(td)
            log = td_path / "calls.log"
            stub = td_path / "systemctl"
            # Restart succeeds; is-active's outcome is the variable under
            # test. That split IS the defect: a zero exit from the former
            # says nothing about the latter.
            stub.write_text(
                "#!/usr/bin/env bash\n"
                'echo "[systemctl] $*" >> "$STUB_CALLS"\n'
                'case " $* " in\n'
                f'  *" is-active "*) exit {is_active_exit} ;;\n'
                '  *) exit 0 ;;\n'
                "esac\n"
            )
            stub.chmod(0o755)
            case = td_path / "case.sh"
            case.write_text(
                "set -uo pipefail\n"
                # Fail loudly if systemctl would resolve to anything but the
                # stub. Without this a missing or non-executable stub would
                # silently fall through to /usr/bin/systemctl. The extracted
                # block only ever calls `is-active`, which is read-only, so
                # the worst case is not destructive — but "we reasoned it is
                # harmless" is exactly how a hermetic test stops being
                # hermetic two edits later.
                'STUB_DIR="' + str(td_path) + '"\n'
                'PATH="$STUB_DIR:$PATH"\n'
                "export PATH\n"
                # Resolved AFTER the stub is on PATH, or the check is
                # measuring the real /usr/bin/systemctl.
                'RESOLVED="$(command -v systemctl || true)"\n'
                'if [[ "$RESOLVED" != "$STUB_DIR/systemctl" ]]; then\n'
                '  echo "STUB_NOT_USED resolved=$RESOLVED" >&2\n'
                "  exit 99\n"
                "fi\n"
                'STUB_CALLS="' + str(log) + '"\n'
                "export STUB_CALLS\n"
                "SCHED_ACTIVE_BEFORE=active\n"
                "TELEM_ACTIVE_BEFORE=active\n"
                + self.block
                + '\necho REACHED_END_OF_BLOCK\n'
            )
            result = subprocess.run(
                ["bash", str(case)], capture_output=True, text=True, timeout=60
            )
            calls = log.read_text() if log.exists() else ""
            return result, calls

    def test_a_failed_unit_after_a_successful_restart_aborts_the_deploy(self):
        result, calls = self._run(is_active_exit=3)  # inactive / failed
        self.assertNotEqual(
            result.returncode,
            0,
            f"deploy must fail when a restarted unit is not active\n{result.stdout}",
        )
        self.assertNotIn(
            "REACHED_END_OF_BLOCK",
            result.stdout,
            "the block ran to completion despite a failed unit — the is-active "
            "assertion is not wired into the exit path",
        )
        self.assertIn("did not come back up", result.stderr)
        self.assertIn("journalctl --user -u", result.stderr, "no diagnostic offered")

    def test_a_healthy_unit_passes(self):
        """Negative control: the above must not pass for the wrong reason."""
        result, calls = self._run(is_active_exit=0)
        self.assertEqual(
            result.returncode, 0, f"healthy deploy wrongly failed\n{result.stderr}"
        )
        self.assertIn("REACHED_END_OF_BLOCK", result.stdout)

    def test_the_stub_was_actually_used(self):
        """The block must reach the stub, not a real systemctl.

        Without this, a broken PATH could make the negative test pass by
        never calling systemctl at all.
        """
        _, calls = self._run(is_active_exit=3)
        self.assertIn(
            "[systemctl] --user is-active --quiet mpm-scheduler",
            calls,
            "the stub never saw the scheduler check, so this test is not "
            "exercising the real code path",
        )

    def test_a_healthy_run_checks_every_unit_it_restarted(self):
        """Both units are asserted when both are up; fail-fast is for the bad path."""
        _, calls = self._run(is_active_exit=0)
        self.assertIn("[systemctl] --user is-active --quiet mpm-scheduler", calls)
        self.assertIn(
            "[systemctl] --user is-active --quiet mpm-telemetry",
            calls,
            "mpm-telemetry is restarted by this script and must be asserted too",
        )

    def test_it_fails_fast_on_the_first_bad_unit(self):
        """The failing scheduler must abort before telemetry is checked."""
        result, calls = self._run(is_active_exit=3)
        self.assertNotIn("mpm-telemetry", calls, "expected fail-fast on the scheduler")

    def test_the_extracted_block_is_the_real_one(self):
        """Guard against the extraction silently finding an empty string."""
        self.assertIn("systemctl --user is-active --quiet", self.block)
        self.assertIn("exit 1", self.block)
        self.assertLess(self.start, self.end)


class SchedulerScopeAndLiveness(unittest.TestCase):
    """Defect 3 — a restart the script never checks actually happened."""

    @classmethod
    def setUpClass(cls) -> None:
        cls.files = _repo_scripts()

    def test_walk_actually_found_scripts(self):
        # Without this, a broken walk makes every rule below vacuously pass —
        # the "green gate reporting on a suite it never ran" failure this
        # repository has already paid for once, documented on `make
        # test-scripts`.
        self.assertGreater(
            len(self.files), 10, f"walk found only {len(self.files)} scripts"
        )

    def test_install_sh_and_deploy_sh_are_among_them(self):
        for expected in ("install.sh", "uninstall.sh", "scripts/deploy.sh"):
            self.assertIn(expected, self.files, f"{expected} not audited")

    # -- scope -------------------------------------------------------

    def test_every_systemctl_call_names_a_manager_scope(self):
        bad = _scope_violations(self.files)
        self.assertEqual(
            [],
            bad,
            "systemctl without --user:\n"
            + "\n".join(f"  {rel}:{n}: {line}" for rel, n, line in bad)
            + "\nmpm-scheduler is a USER-level unit. Plain `systemctl` cannot "
            "see it and exits non-zero without acting; if that status is "
            "ignored the script silently does nothing.",
        )

    def test_wrong_scope_is_detected(self):
        """Negative control for the rule above."""
        buggy = {"case.sh": ["systemctl restart mpm-scheduler.service"]}
        self.assertEqual(1, len(_scope_violations(buggy)))

    def test_correct_scope_is_not_flagged(self):
        ok = {"case.sh": ["systemctl --user restart mpm-scheduler.service"]}
        self.assertEqual([], _scope_violations(ok))

    def test_legacy_sudo_lines_are_exempt(self):
        """The one deliberate exception, pinned so it cannot widen silently."""
        legacy = {"case.sh": ['  warn "    sudo systemctl stop $SERVICE_NAME"']}
        self.assertEqual([], _scope_violations(legacy))

    # -- liveness ----------------------------------------------------

    def test_restarting_the_scheduler_asserts_it_came_back(self):
        bad = _liveness_violations(self.files)
        self.assertEqual(
            [],
            bad,
            "scripts that restart mpm-scheduler without an `is-active` check "
            "afterwards:\n"
            + "\n".join(f"  {rel}:{n}" for rel, n, _ in bad)
            + "\nA zero exit code from `systemctl restart` is not liveness.",
        )

    def test_deploy_sh_verifies_what_it_restarts(self):
        """The specific fix, named — so a revert points at the line."""
        lines = self.files["scripts/deploy.sh"]
        restart = [n for n, l in enumerate(lines) if _is_lifecycle_call(l)]
        self.assertTrue(restart, "deploy.sh should still restart the scheduler")
        self.assertTrue(
            any(_is_verification(lines[n]) for n in range(restart[-1] + 1, len(lines))),
            "deploy.sh restarts mpm-scheduler and mpm-telemetry but verifies "
            "neither; it can print 'Deployment complete.' with either unit down",
        )

    def test_unverified_restart_is_detected(self):
        """Negative control — and the case that made the first rule vacuous.

        The mpm-telemetry probe below IS an `is-active`, one line after the
        scheduler restart. It reads the state before the telemetry restart
        and proves nothing about the scheduler.
        """
        unverified = {
            "case.sh": [
                'SCHED_ACTIVE_BEFORE=$(systemctl --user is-active mpm-scheduler)',
                'if [[ "$SCHED_ACTIVE_BEFORE" == "active" ]]; then',
                "  systemctl --user restart mpm-scheduler",
                "fi",
                'TELEM_ACTIVE_BEFORE=$(systemctl --user is-active mpm-telemetry)',
            ]
        }
        self.assertEqual(1, len(_liveness_violations(unverified)))

    def test_verified_restart_is_not_flagged(self):
        ok = {
            "case.sh": [
                "  systemctl --user restart mpm-scheduler",
                '  systemctl --user is-active --quiet "$unit" || exit 1',
            ]
        }
        self.assertEqual([], _liveness_violations(ok))

    # -- swallowed status --------------------------------------------

    def test_scheduler_lifecycle_calls_keep_their_exit_status(self):
        bad = _discard_violations(self.files)
        self.assertEqual(
            [],
            bad,
            "scheduler lifecycle calls discarding their exit status:\n"
            + "\n".join(f"  {rel}:{n}: {what}" for rel, n, what in bad)
            + "\nA swallowed status cannot be branched on, so a failed "
            "mpm-scheduler is indistinguishable from a successful one.",
        )

    def test_swallowed_status_is_detected(self):
        for discard in ("|| true", "|| :", "||true"):
            with self.subTest(discard=discard):
                buggy = {
                    "case.sh": [f"  systemctl --user restart mpm-scheduler {discard}"]
                }
                self.assertEqual(1, len(_discard_violations(buggy)))

    def test_optional_telemetry_tolerance_is_not_flagged(self):
        """Contrast case: optional services may legitimately be tolerated."""
        tolerated = {
            "case.sh": ["  systemctl --user start mpm-telemetry.service 2>/dev/null || true"]
        }
        self.assertEqual([], _discard_violations(tolerated))


if __name__ == "__main__":
    unittest.main()
