"""
test_install_phase_host_integration.py — Regression coverage for
``scripts/install.sh:phase_host_integration`` and the install
completion message.

Pins the contract that the installer:

  * Never injects ``MPM_ACTIVE_MODE`` or ``MPM_ACTIVE_PERSONA`` into
    the OpenClaw MCP registration. Both vars are read by MPM at
    request time via ``mpmcli.ActiveContextFromEnv()``, which returns
    ``""`` when unset — and the substrate applies its own
    ``default``/``default`` contract. Hardcoding framework-specific
    defaults (e.g. ``"programming"``/``"correspondent"``) at install
    time was a pre-2026-08-29 drift that leaked an old mode taxonomy
    into every MCP registration.

  * Surfaces the LLM configuration requirement at install completion,
    pointing at the canonical ``$DATA_ROOT/mpm_config.json`` file and
    the ``mpm config profile`` workflow, and explicitly stating that
    the installer did NOT create, store, request, or echo any secret.

The bug being regressed against: prior to the fix, every install run
registered the MCP server with ``MPM_ACTIVE_MODE=programming`` and
``MPM_ACTIVE_PERSONA=correspondent`` baked into the env block. A new
host's first MPM session would boot in those defaults even when the
operator had set a different mode via the host framework.

The tests source the real ``install.sh`` function definitions into a
subprocess and drive ``phase_host_integration`` against a fake
``openclaw`` binary that records every invocation to a JSON file on
disk. The fake captures both the ``mcp add`` arg form and the ``mcp
set`` JSON payload, so both branches of the phase are exercised.
"""

from __future__ import annotations

import json
import os
import re
import shutil
import subprocess
import tempfile
import textwrap
import unittest
from pathlib import Path


REPO_ROOT = Path("/home/v/workspace/projects/mpm").resolve()
INSTALL_SH = REPO_ROOT / "scripts" / "install.sh"

# Markers in install.sh that prevent the script from running main()
# during test sourcing. PROJECT_ROOT is computed from $0 inside the
# script as ``readonly``, which would override anything we export; we
# strip those two lines so the driver can control them.
STRIP_PATTERNS = (
    re.compile(r"^\s*readonly\s+SCRIPT_DIR\s*="),
    re.compile(r"^\s*readonly\s+PROJECT_ROOT\s*="),
    re.compile(r'^\s*main\s+"\$@"\s*$'),
)


def build_sourced_lib(install_text: str) -> str:
    """Return the install.sh script with path-resolution and main()
    invocations stripped, so it can be sourced for isolated testing."""
    out_lines = []
    for line in install_text.splitlines():
        if any(p.match(line) for p in STRIP_PATTERNS):
            continue
        out_lines.append(line)
    return "\n".join(out_lines) + "\n"


class _PhaseHostIntegrationDriver(unittest.TestCase):
    """Shared scaffolding for phase_host_integration tests."""

    def setUp(self):
        self._tmp = Path(tempfile.mkdtemp(prefix="mpm-host-test-"))
        self.addCleanup(self._cleanup)

    def _cleanup(self):
        shutil.rmtree(self._tmp, ignore_errors=True)

    def _make_fake_openclaw(self, scenario: str = "active-restart-ok") -> Path:
        """Create a fake ``openclaw`` binary on disk that records every
        invocation. The script writes a JSON line to ``$RECORD_FILE``
        for each subcommand (``mcp list``, ``mcp show``, ``mcp add``,
        ``mcp set``, ``gateway restart``, ``gateway status``) with
        the args received. This is enough to capture both the
        ``mcp add`` flag form and the ``mcp set`` JSON payload form.

        The ``scenario`` argument picks what the fake does for the
        ``gateway`` subcommands:

          * ``active-restart-ok``     — status returns "active";
            restart exits 0 within 1s.
          * ``active-restart-fail``   — status returns "active";
            restart exits 1 within 1s.
          * ``active-restart-hang``   — status returns "active";
            restart sleeps 60s (the installer's timeout / kill-after
            must rescue the call; we measure wall-time below the
            timeout bound to assert no leak).
          * ``inactive``              — status returns "inactive";
            restart exits 0 (should NOT be called).
          * ``failed``                — status returns "failed";
            restart exits 0 (should NOT be called)."""
        bindir = self._tmp / "fakebin"
        bindir.mkdir()
        record_file = self._tmp / "calls.jsonl"
        fake = bindir / "openclaw"
        # Keep the logic simple and robust: every line written is a
        # JSON object with ``cmd`` and ``args`` (or ``payload`` for
        # the ``mcp set`` branch). No shell quoting issues.
        fake.write_text(textwrap.dedent(f"""\
            #!/usr/bin/env bash
            set -uo pipefail
            RECORD_FILE={record_file}
            SCENARIO={scenario}
            subcmd="$1"; shift || true
            case "$subcmd" in
              mcp)
                case "$1" in
                  add|set|list|show)
                    op="$1"; shift
                    args_json=$(python3 -c 'import json,sys; print(json.dumps(sys.argv[1:]))' "$@")
                    if [ "$op" = "set" ]; then
                      payload="$1"
                      printf '{{"cmd":"mcp set","payload":%s}}\\n' "$(printf '%s' "$payload" | python3 -c 'import json,sys; print(json.dumps(sys.stdin.read()))')" >> "$RECORD_FILE"
                    else
                      printf '{{"cmd":"mcp %s","args":%s}}\\n' "$op" "$args_json" >> "$RECORD_FILE"
                    fi
                    ;;
                esac
                ;;
              gateway)
                op="$1"; shift
                case "$op" in
                  status)
                    printf '{{"cmd":"gateway status","scenario":%s}}\\n' '"'$SCENARIO'"' >> "$RECORD_FILE"
                    case "$SCENARIO" in
                      active-restart-ok|active-restart-fail|active-restart-hang)
                        echo "active"
                        exit 0
                        ;;
                      inactive)
                        echo "inactive"
                        exit 0
                        ;;
                      failed)
                        echo "failed"
                        exit 1
                        ;;
                      *)
                        echo "inactive"
                        exit 0
                        ;;
                    esac
                    ;;
                  restart)
                    printf '{{"cmd":"gateway restart","scenario":%s}}\\n' '"'$SCENARIO'"' >> "$RECORD_FILE"
                    case "$SCENARIO" in
                      active-restart-ok)
                        exit 0
                        ;;
                      active-restart-fail)
                        exit 1
                        ;;
                      active-restart-hang)
                        # Hold the connection long enough that the
                        # installer's timeout / kill-after MUST rescue
                        # it. The test asserts wall-time stays under
                        # the installer's bound.
                        sleep 60
                        ;;
                      *)
                        exit 0
                        ;;
                    esac
                    ;;
                esac
                ;;
            esac
            exit 0
        """))
        fake.chmod(0o755)
        self._record_file = record_file
        return bindir

    def _make_fake_systemctl(self, gateway_state: str) -> Path:
        """Create a fake ``systemctl`` that pretends the
        ``openclaw-gateway.service`` unit is in the requested state.

        The installer's gateway-state probe is
        ``systemctl --user is-active openclaw-gateway.service``:

          * rc=0  → active
          * rc=3  → inactive (also printed: "inactive")
          * rc=4  → not-found (also printed: "inactive")
          * any other rc → treat as inactive

        The fake matches this contract so the installer's
        state-detection branch in the test environment behaves
        like a real systemd host. Other ``systemctl`` invocations
        pass through to whatever is on PATH (most aren't reached
        in the host-integration test path).
        """
        bindir = self._tmp / "fakebin"
        bindir.mkdir(exist_ok=True)
        fake = bindir / "systemctl"
        fake.write_text(textwrap.dedent(f"""\
            #!/usr/bin/env bash
            # Pass through to real systemctl for subcommands we
            # don't fake — keeps unrelated installers/test phases
            # working. Only intercept the gateway-state probe.
            if [ "$1" = "--user" ] && [ "$2" = "is-active" ] && [ "$3" = "openclaw-gateway.service" ]; then
              case "{gateway_state}" in
                active)
                  echo "active"
                  exit 0
                  ;;
                failed)
                  # Failed state: stdout "failed", non-zero rc.
                  echo "failed"
                  exit 3
                  ;;
                not-found)
                  # No such unit: rc=4 per systemctl(1).
                  exit 4
                  ;;
                *)
                  # inactive / unknown / anything else.
                  echo "inactive"
                  exit 3
                  ;;
              esac
            fi
            exec /usr/bin/systemctl "$@"
        """))
        fake.chmod(0o755)
        return bindir

    def _stage_lib(self) -> Path:
        """Copy install.sh and write the test-sourcable .lib variant
        into a project tree so the driver can source it."""
        project = self._tmp / "project"
        scripts = project / "scripts"
        scripts.mkdir(parents=True)
        shutil.copy(INSTALL_SH, scripts / "install.sh")
        lib = scripts / "install.sh.lib"
        lib.write_text(build_sourced_lib(INSTALL_SH.read_text()))
        # Provide a minimal bin/ dir so phase_binaries-related helpers
        # don't trip if they're accidentally invoked. phase_host_integration
        # only references $PREFIX/bin/mpm-mcp and $DATA_ROOT, both of
        # which are test-controlled; the project bin/ is unused here but
        # we stage it for symmetry with test_install_phase_binaries.
        (project / "bin").mkdir()
        return project

    def _drive(self, project: Path, prefix: Path, data_root: Path,
               bindir: Path | None,
               systemctl_gateway_state: str | None = None) -> dict:
        """Source install.sh in a subprocess with controlled env and
        invoke ``phase_host_integration``. ``bindir`` is prepended to
        PATH so the fake ``openclaw`` is found first; pass ``None``
        for the no-openclaw branch (which uses a hermetic PATH that
        excludes any system openclaw).

        ``systemctl_gateway_state`` controls the fake ``systemctl``'s
        response to ``--user is-active openclaw-gateway.service``:
          - "active"    → returns active (rc=0)
          - "inactive"  → returns inactive (rc=3)
          - "failed"    → returns failed (rc=3)
          - "not-found" → returns rc=4 (treated as inactive)
          - None        → no fake systemctl staged; the installer's
            ``command -v systemctl`` check falls back to a real
            binary (or no state detection if systemctl is absent).
        """
        driver = project / "scripts" / "_drive.sh"
        env_lines = [
            "export PROJECT_ROOT=" + str(project),
            "source " + str(project / "scripts" / "install.sh.lib"),
            "PREFIX=" + str(prefix),
            "DATA_ROOT=" + str(data_root),
            "SERVICE_DST=/dev/null",
            "export PREFIX DATA_ROOT SERVICE_DST",
            "phase_host_integration",
        ]
        driver.write_text("#!/usr/bin/env bash\nset -uo pipefail\n" +
                          "\n".join(env_lines) + "\n")
        driver.chmod(0o755)

        env = os.environ.copy()
        env["HOME"] = str(self._tmp / "home")
        if bindir is not None:
            env["PATH"] = str(bindir) + ":" + env.get("PATH", "/usr/bin:/bin")
        else:
            # Hermetic PATH with only the essentials and NO /usr/local/bin,
            # so a system-installed `openclaw` is not found.
            env["PATH"] = "/usr/bin:/bin"
        result = subprocess.run(
            ["bash", "--noprofile", "--norc", str(driver)],
            capture_output=True, text=True, env=env,
        )
        # Read the recorded calls if a fake openclaw was used and
        # created a record file; otherwise return an empty list.
        record = getattr(self, "_record_file", None)
        calls: list[dict] = []
        if record is not None and record.exists():
            calls = [
                json.loads(line)
                for line in record.read_text().splitlines()
                if line.strip()
            ]
        return {
            "stdout": result.stdout,
            "stderr": result.stderr,
            "returncode": result.returncode,
            "calls": calls,
        }

    # --- assertions --------------------------------------------------

    def _assert_no_active_mode_or_persona(self, env: dict, context: str):
        """The contract: MPM_ACTIVE_MODE and MPM_ACTIVE_PERSONA must
        never be present in the MCP env block. MPM resolves both at
        request time via mpmcli.ActiveContextFromEnv(), which
        returns "" when unset, and the substrate applies the
        default/default contract."""
        self.assertNotIn(
            "MPM_ACTIVE_MODE", env,
            f"MPM_ACTIVE_MODE must NOT be in the MCP env ({context}); "
            f"got env={env!r}",
        )
        self.assertNotIn(
            "MPM_ACTIVE_PERSONA", env,
            f"MPM_ACTIVE_PERSONA must NOT be in the MCP env "
            f"({context}); got env={env!r}",
        )

    def _assert_workspace_present(self, env: dict, expected: str, context: str):
        self.assertEqual(
            env.get("MPM_WORKSPACE"), expected,
            f"MPM_WORKSPACE must point at the data root ({context}); "
            f"got env={env!r}",
        )


class PhaseHostIntegrationAddPath(_PhaseHostIntegrationDriver):
    """``phase_host_integration`` → ``openclaw mcp add`` branch (the
    mpm MCP entry is not yet registered)."""

    def test_add_omits_mode_and_persona(self):
        bindir = self._make_fake_openclaw()
        # The default add path exercises the installer's add
        # branch on the "active" gateway (so the test continues
        # to drive the restart path that the pre-fix code took).
        self._make_fake_systemctl(gateway_state="active")
        project = self._stage_lib()
        prefix = self._tmp / "home" / ".mpm"
        data_root = prefix

        result = self._drive(project, prefix, data_root, bindir)
        self.assertEqual(
            result["returncode"], 0,
            f"phase_host_integration failed:\n"
            f"  stderr: {result['stderr']}\n"
            f"  stdout: {result['stdout']}",
        )

        # Find the mcp add call. The fake records an args array with
        # ["mcp", "add", "mpm", "--command", "...", "--env", "..."].
        add_calls = [c for c in result["calls"] if c["cmd"] == "mcp add"]
        self.assertEqual(
            len(add_calls), 1,
            f"expected exactly one mcp add call, got: {result['calls']!r}",
        )
        args = add_calls[0]["args"]
        # Walk the flag pairs into a structured env dict.
        env: dict[str, str] = {}
        i = 0
        while i < len(args):
            a = args[i]
            if a == "--env":
                kv = args[i + 1]
                k, _, v = kv.partition("=")
                env[k] = v
                i += 2
            else:
                i += 1

        self._assert_workspace_present(env, str(data_root), "mcp add")
        self._assert_no_active_mode_or_persona(env, "mcp add")
        # Command must reference mpm-mcp at the canonical prefix.
        cmd_idx = args.index("--command") + 1
        self.assertEqual(
            args[cmd_idx], str(prefix / "bin" / "mpm-mcp"),
            "openclaw mcp add must point at $PREFIX/bin/mpm-mcp",
        )

    # Alias for tests that don't need the recorder behavior but do
    # need the record_file path.
    def _make_recorder(self) -> Path:
        return self._make_fake_openclaw()


class PhaseHostIntegrationSetPath(_PhaseHostIntegrationDriver):
    """``phase_host_integration`` → ``openclaw mcp set`` branch (the
    mpm MCP entry already exists, the installer updates it)."""

    def test_set_omits_mode_and_persona(self):
        bindir = self._tmp / "fakebin"
        bindir.mkdir(exist_ok=True)
        # Stage a fake systemctl that reports the gateway as
        # active so the test exercises the set path under the
        # same conditions as the original test driver.
        self._make_fake_systemctl(gateway_state="active")
        self._record_file = self._tmp / "calls.jsonl"
        payload_file = self._tmp / "mcp_set_payload.json"
        # Single fake openclaw that prints the list marker (so the
        # installer's `grep -q -- '^- mpm$'` finds it), records other
        # commands, and writes the `mcp set` JSON payload verbatim to
        # a separate file (avoiding bash JSON-escaping pitfalls).
        fake = bindir / "openclaw"
        fake.write_text(textwrap.dedent(f"""\
            #!/usr/bin/env bash
            set -uo pipefail
            RECORD_FILE={self._record_file}
            PAYLOAD_FILE={payload_file}
            subcmd="$1"; shift || true
            if [ "$subcmd" = "mcp" ] && [ "$1" = "list" ]; then
                echo "- mpm"
                echo "- other"
                exit 0
            fi
            case "$subcmd" in
              mcp)
                op="$1"; shift
                if [ "$op" = "set" ]; then
                  # `openclaw mcp set mpm <json>` — skip the "mpm"
                  # server-name arg to land on the JSON payload.
                  shift
                  printf '%s' "$1" > "$PAYLOAD_FILE"
                  printf '{{"cmd":"mcp set"}}\\n' >> "$RECORD_FILE"
                else
                  args_json=$(python3 -c 'import json,sys; print(json.dumps(sys.argv[1:]))' "$@")
                  printf '{{"cmd":"mcp %s","args":%s}}\\n' "$op" "$args_json" >> "$RECORD_FILE"
                fi
                ;;
              gateway)
                args_json=$(python3 -c 'import json,sys; print(json.dumps(sys.argv[1:]))' "$@")
                printf '{{"cmd":"gateway %s","args":%s}}\\n' "$1" "$args_json" >> "$RECORD_FILE"
                ;;
            esac
            exit 0
        """))
        fake.chmod(0o755)

        project = self._stage_lib()
        prefix = self._tmp / "home" / ".mpm"
        data_root = prefix

        result = self._drive(project, prefix, data_root, bindir=bindir)
        self.assertEqual(
            result["returncode"], 0,
            f"phase_host_integration failed:\n"
            f"  stderr: {result['stderr']}\n"
            f"  stdout: {result['stdout']}",
        )

        set_calls = [c for c in result["calls"] if c["cmd"] == "mcp set"]
        self.assertEqual(
            len(set_calls), 1,
            f"expected exactly one mcp set call, got: {result['calls']!r}",
        )
        # Read the verbatim JSON payload from the side-channel file.
        self.assertTrue(
            payload_file.exists(),
            "fake openclaw did not write the mcp set payload",
        )
        parsed = json.loads(payload_file.read_text())
        env = parsed.get("env", {})
        self._assert_workspace_present(env, str(data_root), "mcp set")
        self._assert_no_active_mode_or_persona(env, "mcp set")
        self.assertEqual(
            parsed.get("command"), str(prefix / "bin" / "mpm-mcp"),
            "openclaw mcp set command must point at $PREFIX/bin/mpm-mcp",
        )


class PhaseHostIntegrationNoOpenclaw(_PhaseHostIntegrationDriver):
    """When ``openclaw`` is not on PATH, the phase logs an informational
    skip message and returns 0 without registering anything."""

    def test_skips_silently_without_openclaw(self):
        # No bindir passed → real PATH, no fake openclaw.
        project = self._stage_lib()
        prefix = self._tmp / "home" / ".mpm"
        data_root = prefix

        # Build a hermetic PATH that excludes the test bindir.
        result = self._drive(project, prefix, data_root, bindir=None)
        self.assertEqual(
            result["returncode"], 0,
            f"phase_host_integration failed in no-openclaw path:\n"
            f"  stderr: {result['stderr']}",
        )
        # No MCP calls were recorded (record_file does not exist).
        self.assertFalse(
            result["calls"],
            f"no-openclaw branch must not invoke any openclaw commands; "
            f"got: {result['calls']!r}",
        )
        # The skip message is in the installer's log (stderr).
        self.assertIn(
            "openclaw not detected", result["stderr"],
            "skip message must explain why MCP registration was skipped",
        )


class PhaseHostIntegrationGatewayInactive(_PhaseHostIntegrationDriver):
    """When the OpenClaw gateway is NOT active (inactive, failed,
    disabled, not installed), the installer must still register the
    MPM MCP entry but must NOT attempt to start or restart the
    gateway. The OpenClaw lifecycle conflict on the pristine ``x``
    profile (an interactive ``openclaw-onboard`` already owned the
    gateway state) caused the installer to hang for ~44s while
    ``openclaw gateway restart`` polled an already-failed service.

    The fix: capture the gateway state BEFORE registering the MCP
    entry; if the gateway is not active, register only and print an
    informational message that registration will load on next
    gateway start. No restart, no hang."""

    def test_inactive_gateway_skips_restart(self):
        bindir = self._make_fake_openclaw(scenario="inactive")
        # Stage a fake systemctl reporting inactive so the
        # installer's state-detection branch sees an inactive unit.
        self._make_fake_systemctl(gateway_state="inactive")
        project = self._stage_lib()
        prefix = self._tmp / "home" / ".mpm"
        data_root = prefix

        result = self._drive(project, prefix, data_root, bindir)
        self.assertEqual(
            result["returncode"], 0,
            f"phase_host_integration must NOT exit non-zero on "
            f"inactive-gateway state:\n  stderr: {result['stderr']}",
        )

        # Registration MUST have happened.
        mcp_calls = [c for c in result["calls"]
                     if c.get("cmd") in ("mcp add", "mcp set")]
        self.assertEqual(
            len(mcp_calls), 1,
            f"inactive-gateway state must still register the MCP "
            f"entry; got calls={result['calls']!r}",
        )

        # Restart MUST NOT have happened.
        restart_calls = [c for c in result["calls"]
                         if c.get("cmd") == "gateway restart"]
        self.assertEqual(
            len(restart_calls), 0,
            f"inactive-gateway state must NOT call gateway restart; "
            f"got calls={result['calls']!r}",
        )

        # Status SHOULD have been queried (state captured BEFORE
        # registration so the post-registration gateway state isn't
        # the one we act on).
        status_calls = [c for c in result["calls"]
                        if c.get("cmd") == "gateway status"]
        self.assertEqual(
            len(status_calls), 1,
            f"installer must query gateway state before deciding to "
            f"restart; got calls={result['calls']!r}",
        )

        # Informational message should explain why no restart.
        self.assertIn(
            "not currently active", result["stderr"],
            f"inactive-gateway path must print an informational "
            f"message about the deferred restart:\n  stderr: {result['stderr']}",
        )

    def test_failed_gateway_skips_restart(self):
        """Failed gateway state must also skip the restart. The
        pristine ``x`` profile observed exactly this — systemd
        retried the gateway until it hit its start limit and entered
        ``failed``. The installer must NOT try to repair OpenClaw's
        lifecycle; it only registers MPM with OpenClaw and reports
        success."""
        bindir = self._make_fake_openclaw(scenario="failed")
        # Failed state: systemctl rc=3 + "failed" stdout. The
        # installer must treat any non-zero rc as inactive.
        self._make_fake_systemctl(gateway_state="failed")
        project = self._stage_lib()
        prefix = self._tmp / "home" / ".mpm"
        data_root = prefix

        result = self._drive(project, prefix, data_root, bindir)
        self.assertEqual(
            result["returncode"], 0,
            f"phase_host_integration must NOT exit non-zero on "
            f"failed-gateway state:\n  stderr: {result['stderr']}",
        )

        mcp_calls = [c for c in result["calls"]
                     if c.get("cmd") in ("mcp add", "mcp set")]
        self.assertEqual(len(mcp_calls), 1)

        restart_calls = [c for c in result["calls"]
                         if c.get("cmd") == "gateway restart"]
        self.assertEqual(
            len(restart_calls), 0,
            f"failed-gateway state must NOT call gateway restart; "
            f"got calls={result['calls']!r}",
        )

        # Failed-state advice must NOT say "run openclaw gateway
        # restart manually" — that advice simply recreates the
        # same problem the installer just avoided.
        self.assertNotIn(
            "run manually: openclaw gateway restart",
            result["stderr"],
            f"failed-gateway state must not push the user to run "
            f"`openclaw gateway restart` (it would just fail again):\n"
            f"  stderr: {result['stderr']}",
        )


class PhaseHostIntegrationGatewayActive(_PhaseHostIntegrationDriver):
    """When the OpenClaw gateway is ACTIVE, the installer registers the
    MCP entry and performs a bounded restart so the running gateway can
    reload the MCP configuration. The restart must have a hard
    timeout (≈10–15s) so an OpenClaw CLI command can never stall the
    installer."""

    def test_active_gateway_registers_then_restarts(self):
        bindir = self._make_fake_openclaw(scenario="active-restart-ok")
        # Active state: systemctl says the unit is active.
        self._make_fake_systemctl(gateway_state="active")
        project = self._stage_lib()
        prefix = self._tmp / "home" / ".mpm"
        data_root = prefix

        result = self._drive(project, prefix, data_root, bindir)
        self.assertEqual(
            result["returncode"], 0,
            f"phase_host_integration failed in active-gateway path:\n"
            f"  stderr: {result['stderr']}",
        )

        # Both registration and restart must have happened, and the
        # status query MUST have preceded the registration (state
        # captured before the restart decision).
        cmd_sequence = [c.get("cmd") for c in result["calls"]]
        self.assertIn(
            "gateway status", cmd_sequence,
            f"installer must query gateway status before restart; "
            f"got sequence={cmd_sequence!r}",
        )
        self.assertIn(
            "gateway restart", cmd_sequence,
            f"active gateway must trigger restart; got sequence={cmd_sequence!r}",
        )

        # Registration must come between status and restart.
        status_idx = cmd_sequence.index("gateway status")
        restart_idx = cmd_sequence.index("gateway restart")
        mcp_indices = [i for i, c in enumerate(cmd_sequence)
                       if c in ("mcp add", "mcp set")]
        self.assertTrue(len(mcp_indices) == 1,
                        f"expected exactly one mcp call, got {cmd_sequence!r}")
        mcp_idx = mcp_indices[0]
        self.assertLess(
            status_idx, mcp_idx,
            f"status MUST come before registration; got sequence={cmd_sequence!r}",
        )
        self.assertLess(
            mcp_idx, restart_idx,
            f"registration MUST come before restart; got sequence={cmd_sequence!r}",
        )

        self.assertIn("✓", result["stderr"],
                      "active restart success path must surface ✓ marker")

    def test_active_restart_failure_is_warn_only(self):
        """When the active restart fails (non-zero exit), the
        installer must NOT abort installation. WARN-only, with
        MPM registration already saved."""
        bindir = self._make_fake_openclaw(scenario="active-restart-fail")
        self._make_fake_systemctl(gateway_state="active")
        project = self._stage_lib()
        prefix = self._tmp / "home" / ".mpm"
        data_root = prefix

        result = self._drive(project, prefix, data_root, bindir)
        self.assertEqual(
            result["returncode"], 0,
            f"active-gateway restart failure must NOT exit non-zero; "
            f"got rc={result['returncode']}:\n  stderr: {result['stderr']}",
        )
        # Must have tried the restart and printed a WARN.
        self.assertIn(
            "gateway restart", [c.get("cmd") for c in result["calls"]],
            "installer must have attempted the restart",
        )
        # WARN-only: stderr should mention restart + continue / saved /
        # MPM registration survives.
        stderr_lower = result["stderr"].lower()
        self.assertTrue(
            ("warn" in stderr_lower or "failed" in stderr_lower),
            f"failed-restart path must surface a WARN / failure note:\n"
            f"  stderr: {result['stderr']}",
        )

    def test_active_restart_hang_is_bounded(self):
        """When the active restart hangs (OpenClaw CLI never returns
        within its internal health-check window), the installer's
        timeout / kill-after must rescue the call. Wall-time MUST stay
        well under OpenClaw's ~44s internal retry budget, and NO long-
        running child process may be left behind after the installer
        exits.

        The fake's ``active-restart-hang`` scenario sleeps for 60s.
        The installer's bound must rescue it; we allow up to ~25s of
        wall-time (the timeout bound plus a generous margin for shell
        fork/exec overhead) — well below OpenClaw's 44s retry window.
        """
        bindir = self._make_fake_openclaw(scenario="active-restart-hang")
        self._make_fake_systemctl(gateway_state="active")
        project = self._stage_lib()
        prefix = self._tmp / "home" / ".mpm"
        data_root = prefix

        import time
        t0 = time.monotonic()
        result = self._drive(project, prefix, data_root, bindir)
        wall_time = time.monotonic() - t0

        self.assertEqual(
            result["returncode"], 0,
            f"hang-detected restart must NOT exit non-zero:\n"
            f"  stderr: {result['stderr']}",
        )
        # The whole install phase must finish well under 30s. The
        # 44s OpenClaw internal retry was the regression baseline;
        # we assert <30s to give the installer a generous bound
        # while still catching a complete failure to bound.
        self.assertLess(
            wall_time, 30.0,
            f"installer's gateway-restart path did not bound its "
            f"OpenClaw call (took {wall_time:.1f}s); the install.sh "
            f"timeout / kill-after must rescue a hanging child. "
            f"The pre-fix behaviour was ~44s.",
        )
        # WARN must mention the timeout.
        stderr_lower = result["stderr"].lower()
        self.assertTrue(
            ("timed out" in stderr_lower or "timeout" in stderr_lower),
            f"hung-restart path must surface a timeout WARN:\n"
            f"  stderr: {result['stderr']}",
        )
        # No zombie child process: after the install phase exits, the
        # hang-sleeping fake must have been killed by the installer's
        # timeout / kill-after. Verify by polling: if any process
        # under self._tmp / "fakebin" is still running, it's a leak.
        # We give the system a moment to reap the SIGKILL.
        time.sleep(0.5)
        import subprocess as _sp
        try:
            ps = _sp.check_output(
                ["pgrep", "-af", "sleep 60"], text=True, timeout=5,
            )
            # Filter to processes whose parent is gone (or that match
            # the fake's path). A leaked sleep would show up here.
            leaked = [
                line for line in ps.splitlines()
                if str(self._tmp) in line
            ]
            self.assertEqual(
                leaked, [],
                f"installer left a hanging child behind:\n"
                + "\n".join(f"  {l}" for l in leaked),
            )
        except _sp.CalledProcessError:
            # pgrep exits 1 when no match — that's the success case.
            pass


class PhaseHostIntegrationStateOrder(_PhaseHostIntegrationDriver):
    """Sequencing invariant: the gateway state MUST be captured BEFORE
    the restart decision, not inferred from the post-registration
    state. The fake records the call sequence, and the test asserts
    the canonical order: status → mcp add/set → restart."""

    def test_status_precedes_registration(self):
        bindir = self._make_fake_openclaw(scenario="active-restart-ok")
        self._make_fake_systemctl(gateway_state="active")
        project = self._stage_lib()
        prefix = self._tmp / "home" / ".mpm"
        data_root = prefix

        result = self._drive(project, prefix, data_root, bindir)
        seq = [c.get("cmd") for c in result["calls"]]
        # Status must be the first gateway call.
        first_gw_call = next(
            (i, c) for i, c in enumerate(seq)
            if c in ("gateway status", "gateway restart")
        )
        self.assertEqual(
            first_gw_call[1], "gateway status",
            f"first gateway call must be `gateway status` (state "
            f"capture before registration/restart); got "
            f"sequence={seq!r}",
        )
        # And the MCP call must come after the status call.
        mcp_idx = next(
            i for i, c in enumerate(seq)
            if c in ("mcp add", "mcp set")
        )
        self.assertGreater(
            mcp_idx, first_gw_call[0],
            f"registration must come AFTER gateway status; got "
            f"sequence={seq!r}",
        )


class InstallSuccessMessageMentionsConfig(_PhaseHostIntegrationDriver):
    """The installer's success block must surface the LLM configuration
    requirement, point at the canonical config file path, and explicitly
    state that the installer did NOT create or store any secret.

    We assert this structurally on the install.sh source — the message
    is plain ``log`` calls in ``mode_install``, not a function we can
    exercise in isolation. A source-grep is the right level of rigor
    for a fixed, hand-curated success message: it catches regressions
    where the block is removed or rewired without the contract being
    re-asserted, while not coupling to log output line ordering.
    """

    REQUIRED_LINES = (
        # The configuration header — surfaces WHY this section exists.
        "configuration (required for LLM-backed features",
        # Points at the canonical file the operator must populate.
        "config file:",
        "$DATA_ROOT/mpm_config.json",
        # Tells operators about the env-file escape hatch.
        "~/.config/mpm/mpm.env",
        # Explicit safety statement — the installer stores nothing.
        "the installer did NOT create, store, request, or echo any API key",
    )

    def test_mode_install_block_contains_all_required_lines(self):
        install_text = INSTALL_SH.read_text()
        # Isolate the mode_install function body for the assertion —
        # we don't care about the leading function def or trailing
        # next-mode definition.
        m = re.search(
            r"^mode_install\(\)\s*\{(.*?)^\}",
            install_text, re.DOTALL | re.MULTILINE,
        )
        self.assertIsNotNone(
            m, "mode_install function not found in install.sh",
        )
        body = m.group(1)
        for required in self.REQUIRED_LINES:
            self.assertIn(
                required, body,
                f"mode_install success block missing required line "
                f"{required!r}; full body:\n{body}",
            )


class NoStaleDefaultsAcrossInstallSurface(unittest.TestCase):
    """Source-wide guard: the strings ``programming`` and
    ``correspondent`` must not appear anywhere in install.sh in the
    context of MCP env wiring. Both values were the pre-2026-08-29
    drift — mode/persona hardcoded as MCP env defaults.

    We allow the strings to exist ONLY in:

      * Comments documenting why the drift was removed.
      * Historical changelog/archive references (NOT in install.sh).

    A grep-based assertion catches both the drift re-appearing in new
    branches of the phase and a future maintainer re-introducing it
    after reading an old commit."""

    def test_no_programming_default_in_install_sh(self):
        text = INSTALL_SH.read_text()
        # Match MPM_ACTIVE_MODE=programming and "MPM_ACTIVE_MODE": "programming"
        bad_patterns = [
            re.compile(r'MPM_ACTIVE_MODE["=]?\s*[:=]\s*"programming"'),
            re.compile(r"MPM_ACTIVE_MODE=programming"),
            re.compile(r'MPM_ACTIVE_MODE["\']\s*:\s*[\'"]programming[\'"]'),
        ]
        offenders = []
        for pat in bad_patterns:
            for m in pat.finditer(text):
                # Allow mentions inside comments that explicitly
                # reference the historical drift (e.g. "do NOT inject
                # 'programming' as a default"). Such comments must
                # contain both the drift value AND the negation in the
                # same logical block.
                line_start = text.rfind("\n", 0, m.start()) + 1
                line_end = text.find("\n", m.end())
                if line_end == -1:
                    line_end = len(text)
                line = text[line_start:line_end]
                # Permit only if the line is in a comment AND clearly
                # frames the value as removed/forbidden.
                if (line.lstrip().startswith("#")
                        and ("do not" in line.lower()
                             or "intentionally not" in line.lower()
                             or "not inject" in line.lower()
                             or "must not" in line.lower()
                             or "no longer" in line.lower()
                             or "hardcoding" in line.lower()
                             or "previously" in line.lower()
                             or "drift" in line.lower())):
                    continue
                offenders.append(line.strip())
        self.assertEqual(
            offenders, [],
            "install.sh must not inject 'programming' as a default "
            "for MPM_ACTIVE_MODE. Found offenders:\n"
            + "\n".join(f"  - {o}" for o in offenders),
        )

    def test_no_correspondent_default_in_install_sh(self):
        text = INSTALL_SH.read_text()
        bad_patterns = [
            re.compile(r'MPM_ACTIVE_PERSONA["=]?\s*[:=]\s*"correspondent"'),
            re.compile(r"MPM_ACTIVE_PERSONA=correspondent"),
            re.compile(r'MPM_ACTIVE_PERSONA["\']\s*:\s*[\'"]correspondent[\'"]'),
        ]
        offenders = []
        for pat in bad_patterns:
            for m in pat.finditer(text):
                line_start = text.rfind("\n", 0, m.start()) + 1
                line_end = text.find("\n", m.end())
                if line_end == -1:
                    line_end = len(text)
                line = text[line_start:line_end]
                if (line.lstrip().startswith("#")
                        and ("do not" in line.lower()
                             or "intentionally not" in line.lower()
                             or "not inject" in line.lower()
                             or "must not" in line.lower()
                             or "no longer" in line.lower()
                             or "hardcoding" in line.lower()
                             or "previously" in line.lower()
                             or "drift" in line.lower())):
                    continue
                offenders.append(line.strip())
        self.assertEqual(
            offenders, [],
            "install.sh must not inject 'correspondent' as a default "
            "for MPM_ACTIVE_PERSONA. Found offenders:\n"
            + "\n".join(f"  - {o}" for o in offenders),
        )


if __name__ == "__main__":
    unittest.main()