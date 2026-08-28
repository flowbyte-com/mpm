"""
test_template_provenance_env.py — Regression coverage for the Claude Code
MPM framework-identification env contract.

The canonical wire contract for framework identification on the
mpm-mcp subprocess is **MPM_FRAMEWORK**, not MPM_PROVENANCE_FRAMEWORK.

See:
  - internal/core/mpmcli/mpmcli.go: ActiveContextFromEnv reads MPM_FRAMEWORK
    and populates ActiveContext.FrameworkName
  - internal/core/mpmcli/mpmcli_test.go: TestActiveContextFromEnv_MPM_FRAMEWORK
    pins the directive-scope transport contract
  - cmd/mpm-mcp/audit_hook.go: mpm-mcp's recordToolInvocation uses
    ac.FrameworkName (overridable via MPM_FRAMEWORK)
  - README.md §1827: "How a framework identifies itself" — explicitly
    documents MPM_FRAMEWORK=<id> as the canonical wire value.

Before this fix: only 2 of 5477 artifact_provenance rows attributed to
claude-code framework because the template omitted MPM_FRAMEWORK. Recorded
2026-08-28 during theory-backlog triage session.

This test pins the contract so the regression cannot recur.
"""

from __future__ import annotations

import json
import os
import subprocess
import tempfile
import unittest
from pathlib import Path

PLUGIN_DIR = Path("/home/v/.mpm/agent_installation/claude-code-mpm")
TEMPLATE = PLUGIN_DIR / ".mcp.json.template"
MATERIALIZED = Path.home() / ".claude" / ".mcp.json"
HOME_RESOLVED = "/home/v"


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
        # Per README §1827 — MPM_FRAMEWORK is the canonical framework-id var.
        self.assertEqual(
            self.env.get("MPM_FRAMEWORK"),
            "claude-code",
            "MPM_FRAMEWORK must be 'claude-code' (the documented wire value)",
        )

    def test_workspace_env_var_preserved(self):
        self.assertEqual(
            self.env.get("MPM_WORKSPACE"),
            "/home/v/.mpm",
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
        # Regression guard against the FIRST draft of this fix which used
        # MPM_PROVENANCE_FRAMEWORK instead of MPM_FRAMEWORK. The former is
        # not consumed by mpmcli.ActiveContextFromEnv (the canonical reader);
        # setting it would not propagate to mpm-mcp's audit_hook.
        self.assertNotIn(
            "MPM_PROVENANCE_FRAMEWORK",
            self.env,
            "MPM_PROVENANCE_FRAMEWORK is NOT the canonical framework-id var; "
            "use MPM_FRAMEWORK per README §1827. Setting the wrong var makes the "
            "fix a no-op.",
        )
        self.assertNotIn(
            "MPM_PROVENANCE_ACTOR_KIND",
            self.env,
            "MPM_PROVENANCE_ACTOR_KIND has no documentary backing in the legacy "
            "wire contract; mpm-mcp's audit_hook hardcodes actor_kind='agent'. "
            "Don't introduce it here.",
        )


class MaterializedConfigInSync(unittest.TestCase):
    """The installed ~/.claude/.mcp.json must mirror the template's env block."""

    def setUp(self):
        if not MATERIALIZED.exists():
            self.skipTest(
                f"Materialized config absent: {MATERIALIZED}. "
                f"Run 'install.sh' first."
            )
        data = json.loads(MATERIALIZED.read_text())
        self.env = data["mcpServers"]["mpm"]["env"]

    def test_installed_config_has_claude_code_framework(self):
        self.assertEqual(
            self.env.get("MPM_FRAMEWORK"),
            "claude-code",
            f"Materialized MPM_FRAMEWORK={self.env.get('MPM_FRAMEWORK')!r}; "
            "claude-code MCP calls will not be attributed correctly without this.",
        )

    def test_installed_config_no_static_model(self):
        for forbidden in ("MPM_PROVENANCE_MODEL", "MPM_FRAMEWORK_MODEL"):
            self.assertNotIn(forbidden, self.env)

    def test_installed_config_no_static_invocation_id(self):
        for forbidden in ("MPM_PROVENANCE_INVOCATION_ID", "MPM_FRAMEWORK_INVOCATION_ID"):
            self.assertNotIn(forbidden, self.env)

    def test_installed_config_does_not_contain_redundant_provenance(self):
        # Belt-and-braces: ensure earlier wrong-var-name drafts never shipped
        # into the materialized config.
        self.assertNotIn("MPM_PROVENANCE_FRAMEWORK", self.env)
        self.assertNotIn("MPM_PROVENANCE_ACTOR_KIND", self.env)


class InstallMaterializationIsIdempotent(unittest.TestCase):
    """install.sh is idempotent: running the materialization produces identical env block content."""

    def test_double_materialize_yields_stable_env(self):
        # We don't run install.sh end-to-end (its sanity probe requires the
        # canonical MPM binary at $HOME/.mpm/bin/mpm-mcp), but we exercise the
        # materialization logic in-process twice and verify the env block is
        # identical across runs. That proves the template+materialize pipeline
        # is deterministic regardless of invocation count.
        first = self._materialize()
        second = self._materialize()
        self.assertEqual(first["env"], second["env"])

    def test_installed_matches_template_after_install(self):
        # If install.sh has been run, ~/.claude/.mcp.json should reflect the
        # template's canonical framework-id var.
        if not MATERIALIZED.exists():
            self.skipTest(f"Materialized config absent: {MATERIALIZED}")
        tpl_env = _resolved_template()["mcpServers"]["mpm"]["env"]
        live_env = json.loads(MATERIALIZED.read_text())["mcpServers"]["mpm"]["env"]
        for key in ("MPM_WORKSPACE", "MPM_FRAMEWORK"):
            self.assertEqual(live_env.get(key), tpl_env.get(key),
                             f"{key} drift between template and materialized config")

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


if __name__ == "__main__":
    unittest.main(verbosity=2)
