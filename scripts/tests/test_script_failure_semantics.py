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


if __name__ == "__main__":
    unittest.main()
