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

    def _make_fake_openclaw(self) -> Path:
        """Create a fake ``openclaw`` binary on disk that records every
        invocation. The script writes a JSON line to ``$RECORD_FILE``
        for each subcommand (``mcp list``, ``mcp show``, ``mcp add``,
        ``mcp set``, ``gateway restart``) with the args received. This
        is enough to capture both the ``mcp add`` flag form and the
        ``mcp set`` JSON payload form."""
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
            # Skip argv[0] (program name), join the rest as a JSON array.
            args_json=$(python3 -c 'import json,sys; print(json.dumps(sys.argv[1:]))' "$@")
            subcmd="$1"; shift || true
            case "$subcmd" in
              mcp)
                case "$1" in
                  add|set|list|show)
                    op="$1"; shift
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
                printf '{{"cmd":"gateway %s","args":%s}}\\n' "$1" "$args_json" >> "$RECORD_FILE"
                ;;
            esac
            exit 0
        """))
        fake.chmod(0o755)
        self._record_file = record_file
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
               bindir: Path | None) -> dict:
        """Source install.sh in a subprocess with controlled env and
        invoke ``phase_host_integration``. ``bindir`` is prepended to
        PATH so the fake ``openclaw`` is found first; pass ``None``
        for the no-openclaw branch (which uses a hermetic PATH that
        excludes any system openclaw)."""
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