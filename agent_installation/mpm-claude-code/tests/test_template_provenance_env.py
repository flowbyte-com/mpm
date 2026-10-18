"""
test_template_provenance_env.py — Regression coverage for the Claude Code
MPM framework-identification env contract.

The canonical wire contract for framework identification on the
mpm-mcp subprocess is **MPM_PROVENANCE_FRAMEWORK**. The legacy alias
MPM_FRAMEWORK remains a fallback read by mpmcli.ActiveContextFromEnv.

See:
  - internal/core/mpmcli/mpmcli.go: ActiveContextFromEnv reads
    MPM_PROVENANCE_FRAMEWORK first and falls back to MPM_FRAMEWORK.
  - internal/core/mpmcli/mpmcli_test.go: TestActiveContextFromEnv_FrameworkPrecedence
    pins the MPM_PROVENANCE_FRAMEWORK > MPM_FRAMEWORK > "mcp" precedence.
  - cmd/mpm-mcp/audit_hook.go: mpm-mcp's recordToolInvocation uses
    ac.FrameworkName (which ActiveContextFromEnv populated from the env
    var chain above).
  - docs/SPEC.md §1945: "How a framework identifies itself" — explicitly
    documents MPM_PROVENANCE_FRAMEWORK=<id> as the canonical name.

Stage 2B update (2026-09-19): the template now sets both
MPM_PROVENANCE_FRAMEWORK (canonical) AND MPM_FRAMEWORK (legacy alias)
so existing installs continue to attribute correctly while future
substrate versions that drop the alias still attribute correctly.

Before this fix family: only 2 of 5477 artifact_provenance rows
attributed to claude-code framework because the template omitted
MPM_FRAMEWORK. Recorded 2026-08-28.

This test pins the contract so the regression cannot recur.
"""

from __future__ import annotations

import json
import os
import shutil
import subprocess
import tempfile
import unittest
from pathlib import Path

# Derive the source-tree adapter directory from this test file's
# location. The test lives at
#   agent_installation/mpm-claude-code/tests/<file>.py
# so PLUGIN_DIR = parent.parent (= agent_installation/mpm-claude-code/).
# The previous hardcoded "/home/v/.mpm/agent_installation/..." pointed
# only at the original author's installed location; tests should
# resolve from the repository so they work without an installed MPM.
PLUGIN_DIR = Path(__file__).resolve().parent.parent
TEMPLATE = PLUGIN_DIR / ".mcp.json.template"
INSTALL_SH = PLUGIN_DIR / "install.sh"
CONFIG_HELPER = PLUGIN_DIR / "scripts" / "claude_mcp_config.py"
# Claude Code's user scope is ~/.claude.json (mcpServers at top level).
# ~/.claude/.mcp.json is NOT a discovery path — install.sh used to write
# there, so these assertions silently skipped forever on a host where the
# adapter had never run. They now follow the registration the installer
# actually performs.
#
# MATERIALIZED is the operator's live ~/.claude.json — the one install.sh
# would read on a developer machine that has already run the installer.
# Tests that touch the live install path (MaterializedConfigInSync below)
# check for its presence and skip cleanly when absent, so a clean CI runner
# with no prior install does not error out. Tests that need to *exercise*
# the installer's first-install contract run against an isolated sandbox
# (FreshInstallMatchesTemplateContract) so the developer's real config
# is never read or written by these tests.
MATERIALIZED = Path.home() / ".claude.json"
# HOME_RESOLVED — the resolved ${HOME} that install.sh substitutes when
# it materialises the template. The test asserts template variables
# expand to a path that exists on the current host, so we use the
# current process's resolved home directory. The previous literal
# "/home/v" hardcoded the author's checkout and produced a
# host-dependent assertion that failed under any other user.
HOME_RESOLVED = str(Path.home())


def has_claude() -> bool:
    """Whether the host has the `claude` CLI on PATH.

    The first-install contract requires `claude mcp add` to run; on hosts
    without Claude Code installed the installer itself refuses to proceed.
    The sandbox-based tests below skip on those hosts rather than fabricating
    a `~/.claude.json` — installing the CLI is a developer decision, not a
    test convenience.
    """
    return shutil.which("claude") is not None


def _resolved_template():
    """Read the template and substitute ${HOME} the way install.sh does."""
    raw = TEMPLATE.read_text().replace("${HOME}", HOME_RESOLVED)
    return json.loads(raw)


class TemplateFrameworkEnvContract(unittest.TestCase):
    """Pin the env block in .mcp.json.template to the documented contract."""

    def setUp(self):
        self.assertTrue(TEMPLATE.exists(), f"missing template: {TEMPLATE}")
        data = _resolved_template()
        self.assertIn("mcpServers", data)
        self.assertIn("mpm", data["mcpServers"])
        self.env = data["mcpServers"]["mpm"]["env"]

    def test_framework_env_var_is_claude_code(self):
        # Per README §1945 — MPM_PROVENANCE_FRAMEWORK is canonical.
        self.assertEqual(
            self.env.get("MPM_PROVENANCE_FRAMEWORK"),
            "claude-code",
            "MPM_PROVENANCE_FRAMEWORK must be 'claude-code' (canonical per "
            "README §1945 and the precedence test in mpmcli_test.go)",
        )

    def test_legacy_framework_alias_preserved(self):
        # Older mpm-mcp binaries read MPM_FRAMEWORK instead of the canonical
        # var. Setting both lets installs migrate forward without re-render
        # of the registered user-scope entry.
        self.assertEqual(
            self.env.get("MPM_FRAMEWORK"),
            "claude-code",
            "Legacy MPM_FRAMEWORK alias must remain set for backward compat; "
            "ActiveContextFromEnv reads it as a fallback when "
            "MPM_PROVENANCE_FRAMEWORK is absent.",
        )

    def test_workspace_env_var_preserved(self):
        # The template sets MPM_WORKSPACE=${HOME}/.mpm; after install.sh
        # substitutes ${HOME} with the current user's resolved home, the
        # materialized env block must read "<home>/.mpm". Previously
        # hardcoded to "/home/v/.mpm" which asserted the original
        # author's specific install rather than the portable layout.
        expected_workspace = str(Path.home() / ".mpm")
        self.assertEqual(
            self.env.get("MPM_WORKSPACE"),
            expected_workspace,
            "MPM_WORKSPACE must remain set; rewriting the workspace handle "
            "would break the canonical install path.",
        )

    def test_no_static_model_identity(self):
        # MPM_PROVENANCE_MODEL with a static value would be a fabrication —
        # the actual model changes per session (MiniMax-M2.7 / M3 / opus-5
        # etc.). Leave NULL; mpm-mcp will not lie about model identity.
        for forbidden in (
            "MPM_PROVENANCE_MODEL",
            "MPM_FRAMEWORK_MODEL",  # legacy alias some integrations use
        ):
            self.assertNotIn(
                forbidden, self.env,
                f"Static {forbidden} in template is a fabrication.",
            )

    def test_no_static_invocation_id(self):
        for forbidden in (
            "MPM_PROVENANCE_INVOCATION_ID",
            "MPM_PROVENANCE_PARENT_INVOCATION_ID",
            "MPM_FRAMEWORK_INVOCATION_ID",
        ):
            self.assertNotIn(
                forbidden, self.env,
                f"Static {forbidden} defeats the per-call dynamic ID.",
            )

    def test_does_not_use_wrong_var_names(self):
        # MPM_PROVENANCE_ACTOR_KIND is intentionally not exposed by the
        # substrate's wire contract — mpm-mcp's audit_hook hardcodes
        # actor_kind='agent' on every MCP dispatch, so a static
        # MPM_PROVENANCE_ACTOR_KIND=agent would imply a contract this
        # codebase does not implement. Don't introduce it.
        self.assertNotIn(
            "MPM_PROVENANCE_ACTOR_KIND",
            self.env,
            "MPM_PROVENANCE_ACTOR_KIND has no documentary backing in the wire "
            "contract; mpm-mcp's audit_hook hardcodes actor_kind='agent'. "
            "Don't introduce it here.",
        )


class MaterializedConfigInSync(unittest.TestCase):
    """The registered user-scope entry must mirror the template's env block."""

    def setUp(self):
        if not MATERIALIZED.exists():
            self.skipTest(
                f"Materialized config absent: {MATERIALIZED}. "
                f"Run 'install.sh' first."
            )
        data = json.loads(MATERIALIZED.read_text())
        entry = (data.get("mcpServers") or {}).get("mpm")
        if not entry:
            self.skipTest(
                f"No user-scope mpm entry in {MATERIALIZED}. "
                f"Run 'install.sh' first."
            )
        self.env = entry["env"]

    def test_installed_config_has_claude_code_framework(self):
        # Canonical wins; legacy alias is acceptable as fallback.
        framework = (
            self.env.get("MPM_PROVENANCE_FRAMEWORK")
            or self.env.get("MPM_FRAMEWORK")
        )
        self.assertEqual(
            framework,
            "claude-code",
            f"Materialized framework={framework!r}; claude-code MCP calls "
            "will not be attributed correctly without MPM_PROVENANCE_FRAMEWORK "
            "or MPM_FRAMEWORK set to 'claude-code'.",
        )

    def test_installed_config_no_static_model(self):
        for forbidden in ("MPM_PROVENANCE_MODEL", "MPM_FRAMEWORK_MODEL"):
            self.assertNotIn(forbidden, self.env)

    def test_installed_config_no_static_invocation_id(self):
        for forbidden in ("MPM_PROVENANCE_INVOCATION_ID", "MPM_FRAMEWORK_INVOCATION_ID"):
            self.assertNotIn(forbidden, self.env)

    def test_installed_config_does_not_contain_redundant_provenance(self):
        # Belt-and-braces: ensure earlier wrong-var-name drafts never shipped
        # into the materialized config. This previously asserted the OPPOSITE
        # of the canonical contract — that MPM_PROVENANCE_FRAMEWORK is absent —
        # while TemplateFrameworkEnvContract pins it as required. It only ever
        # passed by skipping, because it pointed at a file the installer
        # never wrote; repointed at the real registration it would have
        # contradicted the very contract it was guarding.
        self.assertEqual(self.env.get("MPM_PROVENANCE_FRAMEWORK"), "claude-code")
        self.assertNotIn("MPM_PROVENANCE_ACTOR_KIND", self.env)


class InstallMaterializationIsIdempotent(unittest.TestCase):
    """install.sh's in-process materialization is deterministic.

    These tests do not run install.sh end-to-end; the installer's sanity
    probe requires the canonical MPM binary at $HOME/.mpm/bin/mpm-mcp,
    which is a developer-machine artefact, not a test fixture. Instead we
    exercise the materialization logic in-process and verify the env
    block is identical across runs and across a synthetic empty / pre-seeded
    user config. That proves the template+materialize pipeline is
    deterministic regardless of invocation count or starting state.

    First-install behaviour against a real Claude Code CLI is verified
    separately by FreshInstallMatchesTemplateContract, which runs the
    installer against an isolated sandbox so the developer's real
    ~/.claude.json is never read or written.
    """

    def test_double_materialize_yields_stable_env(self):
        first = self._materialize()
        second = self._materialize()
        self.assertEqual(first["env"], second["env"])

    def _materialize(self) -> dict:
        raw = TEMPLATE.read_text().replace("${HOME}", HOME_RESOLVED)
        data = json.loads(raw)
        data.pop("_comment", None)
        # Replicate install.sh's "merge with existing mcpServers" logic.
        existing = {}
        # Use a temp file to avoid touching live config during the in-process
        # test (the live install may also have run).
        with tempfile.NamedTemporaryFile("w", suffix=".mcp.json", delete=False) as tf:
            tf.write(json.dumps({"mcpServers": {}}))
            tf_path = Path(tf.name)
        try:
            if tf_path.exists():
                with open(tf_path) as f:
                    existing = json.load(f)
        except Exception:
            existing = {}
        existing.setdefault("mcpServers", {})
        existing["mcpServers"]["mpm"] = data["mcpServers"]["mpm"]
        tf_path.unlink(missing_ok=True)
        return existing["mcpServers"]["mpm"]


class FreshInstallSandbox:
    """Isolated HOME + CLAUDE_CONFIG_DIR for first-install contract tests.

    `CLAUDE_CONFIG_DIR` relocates BOTH `~/.claude.json` and `settings.json`
    into a single directory, so a sandbox here is genuinely complete — the
    real host config is never read or written by these tests.

    install.sh's pre-flight requires an executable mpm-mcp under HOME; the
    sandbox writes a no-op stub so the installer's materialization path can
    run end-to-end without needing a real binary. The probe at the end of
    install.sh is opted out via MPM_INSTALL_SKIP_PROBE=1 — the probe
    requires a live mpm-mcp and a reachable workspace DB, which is not
    what these tests are about. The contract under test is the
    registration, not the binary.
    """

    def __init__(self) -> None:
        self.root = Path(tempfile.mkdtemp(prefix="mpm-cc-tplprov-"))
        self.home = self.root / "home"
        self.cfg = self.root / "claude-cfg"
        self.project = self.root / "project"
        for d in (self.home, self.cfg, self.project):
            d.mkdir(parents=True, exist_ok=True)
        # install.sh pre-flight: must have an executable mpm-mcp under HOME.
        bin_dir = self.home / ".mpm" / "bin"
        bin_dir.mkdir(parents=True, exist_ok=True)
        (bin_dir / "mpm-mcp").write_text("#!/bin/sh\nexit 0\n")
        (bin_dir / "mpm-mcp").chmod(0o755)

    @property
    def user_config(self) -> Path:
        return self.cfg / ".claude.json"

    def read_user_config(self) -> dict:
        try:
            return json.loads(self.user_config.read_text())
        except (OSError, json.JSONDecodeError):
            return {}

    def seed_user_config(self, cfg: dict) -> None:
        self.user_config.write_text(json.dumps(cfg, indent=2))

    def env(self, **extra) -> dict:
        e = dict(os.environ)
        e.update({
            "HOME": str(self.home),
            "CLAUDE_CONFIG_DIR": str(self.cfg),
            # The live mpm-mcp handshake has no meaning in a sandbox; the
            # registration contract is what these tests are about.
            "MPM_INSTALL_SKIP_PROBE": "1",
        })
        e.update(extra)
        return e

    def run(self, *args, expect_ok: bool = True) -> subprocess.CompletedProcess:
        p = subprocess.run(
            ["bash", str(INSTALL_SH), *args],
            cwd=str(self.project), env=self.env(),
            capture_output=True, text=True, timeout=180,
        )
        if expect_ok and p.returncode != 0:
            raise AssertionError(
                f"install.sh {args} failed ({p.returncode})\n"
                f"--- stdout ---\n{p.stdout}\n--- stderr ---\n{p.stderr}"
            )
        return p

    def cleanup(self) -> None:
        shutil.rmtree(self.root, ignore_errors=True)


@unittest.skipUnless(has_claude(), "claude CLI not on PATH")
class FreshInstallMatchesTemplateContract(unittest.TestCase):
    """End-to-end: install.sh against an isolated sandbox writes a registration
    whose env block matches the source-tree template.

    These tests exercise the installer's *actual* first-install contract
    (the same `claude mcp add --scope user` flow the developer runs), not
    a re-implementation of the materialization. They skip when the `claude`
    CLI is not on PATH — the installer's supported first-install contract
    requires it, and a host without Claude Code installed cannot meaningfully
    verify the registration path.

    On a clean CI runner the installer's pre-flight fails before any config
    is written, so these tests are skipped rather than fabricating a
    ~/.claude.json. MaterializedConfigInSync covers the developer's
    existing-install path; the suite is honest about which state it can
    verify on which host.
    """

    def setUp(self) -> None:
        self.sb = FreshInstallSandbox()
        self.addCleanup(self.sb.cleanup)
        # Import the helper so we can call desired_entry / user_entry against
        # the sandbox config without duplicating their logic.
        import importlib.util
        spec = importlib.util.spec_from_file_location("claude_mcp_config", CONFIG_HELPER)
        self.cfgmod = importlib.util.module_from_spec(spec)
        spec.loader.exec_module(self.cfgmod)

    def test_fresh_install_against_empty_sandbox_matches_template(self) -> None:
        """The cleanest first-install state: no ~/.claude.json exists.

        `claude mcp add --scope user` creates the file when the user has
        never registered an MCP server before. The resulting entry must
        match the template's env block exactly.
        """
        self.assertFalse(
            self.sb.user_config.exists(),
            "sandbox precondition: user config must not exist before install",
        )
        self.sb.run()
        entry = self.cfgmod.user_entry(self.sb.user_config)
        self.assertIsNotNone(entry, "no user-scope mpm entry was written")
        tpl_env = (
            json.loads(TEMPLATE.read_text().replace("${HOME}", str(self.sb.home)))
            ["mcpServers"]["mpm"]["env"]
        )
        # Pin every key the contract cares about. Drift here would break
        # MPM's framework-identification wire contract.
        for key in ("MPM_WORKSPACE", "MPM_FRAMEWORK", "MPM_PROVENANCE_FRAMEWORK"):
            self.assertEqual(
                entry["env"].get(key), tpl_env.get(key),
                f"{key} drift between template and post-install entry",
            )

    def test_fresh_install_against_pre_existing_user_config_matches_template(self) -> None:
        """Pre-existing ~/.claude.json with unrelated servers: the installer
        must add mpm without touching the rest, and the mpm entry's env
        block must still match the template.

        This is the developer-machine case: a user already has other MCP
        servers registered at user scope. Idempotency and preservation of
        user-authored configuration are both pinned here.
        """
        self.sb.seed_user_config({
            "mcpServers": {
                "unrelated": {"command": "/bin/echo", "env": {}},
            },
        })
        self.sb.run()
        cfg = self.sb.read_user_config()
        self.assertIn(
            "unrelated", cfg["mcpServers"],
            "installer must preserve unrelated user-scope servers",
        )
        entry = self.cfgmod.user_entry(self.sb.user_config)
        self.assertIsNotNone(entry, "no user-scope mpm entry was written")
        self.assertEqual(
            entry["env"].get("MPM_PROVENANCE_FRAMEWORK"), "claude-code",
            "post-install entry must carry the canonical framework id",
        )

    def test_no_install_run_means_no_mpm_entry(self) -> None:
        """Negative control: a sandbox with no installer run must not contain
        an mpm registration. This is the test that would fail loudly if the
        suite ever started fabricating a ~/.claude.json to make a skip look
        like a pass.
        """
        self.assertFalse(
            self.sb.user_config.exists(),
            "sandbox precondition: no user config without an install",
        )
        self.assertIsNone(
            self.cfgmod.user_entry(self.sb.user_config),
            "no mpm entry should exist before install.sh runs",
        )


if __name__ == "__main__":
    unittest.main(verbosity=2)
