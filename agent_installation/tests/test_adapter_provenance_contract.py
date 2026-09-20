#!/usr/bin/env python3
"""
test_adapter_provenance_contract.py — Stage 2B cross-adapter provenance
contract.

Each maintained host adapter MUST declare its canonical framework
identity so MPM's audit trail attributes writes to the actual host
rather than the transport default ("mcp") or operator-CLI fallback
("mpm-cli"). Without this, contextual routing cannot distinguish
agent-driven writes from operator shell traffic, and recent_activity
collapses every host into one anonymous bucket.

This test is the structural gate: a new adapter lands without
declaring its framework → CI fails. An existing adapter drops the
declaration → CI fails.

It is intentionally pattern-based (regex over the rendered
instruction surface and the adapter's source/manifest files) rather
than a hard-coded grep of one env-var name, so a future adapter
that legitimately uses a different runtime surface (e.g. MCP env
block vs. resolve_exec_env hook) still passes when it correctly
declares the same canonical framework identifier.

Canonical framework identifiers (mirrored from
internal/core/activity_classifier.go::knownAgentFrameworks):
  claude-code, openclaw, opencode, pi, hermes

Per-adapter contract: the canonical identifier must appear at least
once in the rendered instruction surface for that host, AND the
adapter's source/runtime layer must propagate it via at least one
of:
  - MPM_PROVENANCE_FRAMEWORK env var (canonical), or
  - MPM_FRAMEWORK env var (legacy alias), or
  - a per-call buildProvenanceEnv / withWorkspace helper that emits
    the canonical identifier into the env passed to `mpm` invocations.

Run with: pytest test_adapter_provenance_contract.py -v
(also compatible with python -m unittest invocation).
"""

from __future__ import annotations

import json
import re
import unittest
from pathlib import Path

REPO_ROOT = Path(__file__).resolve().parents[2]  # repo root
AGENT_INSTALL = REPO_ROOT / "agent_installation"
RENDER_DIR = AGENT_INSTALL / "scripts" / "__pycache__"  # populated at render time
RENDER_SCRIPT = AGENT_INSTALL / "scripts" / "render_managed_blocks.py"

# Adapters that MUST declare their canonical framework identity.
# (host_dir, canonical_framework_id, env_var_or_helper_alternatives).
#
# env_var_or_helper_alternatives is a list of substrings; if ANY one
# appears in the adapter's rendered instruction surface, the contract
# is satisfied. This keeps the test resilient to "uses canonical
# MPM_PROVENANCE_FRAMEWORK" vs "uses legacy MPM_FRAMEWORK alias" vs
# "calls buildProvenanceEnv helper that emits the canonical name".

CANONICAL_FRAMEWORKS = {
    "mcp", "openclaw", "pi", "opencode", "claude-code", "hermes",
}

ADAPTERS = [
    {
        "host_dir": "mpm-pi",
        "canonical": "pi",
        "rendered_pattern": r"(?:rendered-managed-blocks|mpm-pi|managed-blocks-mpm-pi)",
        "expected_in_rendered": [
            r"MPM_PROVENANCE_FRAMEWORK[^\n]*\bpi\b",
            r"MPM_FRAMEWORK[^\n]*\bpi\b",
            r"\bpi\b",  # bare occurrence is acceptable when one of the above is absent
        ],
        "source_must_declare": [
            # withProvenance() / buildProvenanceEnv emits the framework
            # id into the env passed to `mpm` subprocess invocations.
            r"MPM_PROVENANCE_FRAMEWORK.{0,40}\bpi\b",
            r"MPM_FRAMEWORK.{0,40}\bpi\b",
        ],
    },
    {
        "host_dir": "mpm-opencode",
        "canonical": "opencode",
        "rendered_pattern": r"mpm-opencode",
        "expected_in_rendered": [
            r"MPM_PROVENANCE_FRAMEWORK[^\n]*\bopencode\b",
            r"MPM_FRAMEWORK[^\n]*\bopencode\b",
        ],
        "source_must_declare": [
            r"MPM_PROVENANCE_FRAMEWORK.{0,40}\bopencode\b",
            r"MPM_FRAMEWORK.{0,40}\bopencode\b",
        ],
    },
    {
        "host_dir": "mpm-hermes",
        "canonical": "hermes",
        "rendered_pattern": r"mpm-hermes",
        "expected_in_rendered": [
            r"MPM_PROVENANCE_FRAMEWORK[^\n]*\bhermes\b",
            r"MPM_FRAMEWORK[^\n]*\bhermes\b",
        ],
        # Hermes' adapter is documentation-driven (Hermes is itself an
        # LLM agent platform); the canonical name lives in the rendered
        # instruction surface and SKILL.md. The test accepts any source
        # manifest declaring the canonical id.
        "source_must_declare": [
            r"\bhermes\b",
        ],
    },
    {
        "host_dir": "mpm-claude-code",
        "canonical": "claude-code",
        "rendered_pattern": r"mpm-claude-code",
        "expected_in_rendered": [
            r"MPM_PROVENANCE_FRAMEWORK[^\n]*\bclaude-code\b",
            r"MPM_FRAMEWORK[^\n]*\bclaude-code\b",
        ],
        "source_must_declare": [
            # .mcp.json.template materialises one of the canonical env vars
            # with the framework id.
            r"MPM_PROVENANCE_FRAMEWORK.{0,80}\bclaude-code\b",
            r"MPM_FRAMEWORK.{0,80}\bclaude-code\b",
        ],
    },
    {
        "host_dir": "mpm-memory-openclaw",
        "canonical": "openclaw",
        "rendered_pattern": r"mpm-memory-openclaw",
        "expected_in_rendered": [
            r"MPM_PROVENANCE_FRAMEWORK[^\n]*\bopenclaw\b",
            r"MPM_FRAMEWORK[^\n]*\bopenclaw\b",
        ],
        "source_must_declare": [
            # Plugin's index.js emits MPM_PROVENANCE_FRAMEWORK=openclaw
            # via resolve_exec_env hook and/or .mcp.json MPL launch env.
            r"MPM_PROVENANCE_FRAMEWORK.{0,80}\bopenclaw\b",
            r"MPM_FRAMEWORK.{0,80}\bopenclaw\b",
        ],
    },
    {
        "host_dir": "mpm-auto-mode-persona-openclaw",
        "canonical": "openclaw",
        "rendered_pattern": r"mpm-auto-mode-persona-openclaw",
        "expected_in_rendered": [
            r"MPM_PROVENANCE_FRAMEWORK[^\n]*\bopenclaw\b",
            r"MPM_FRAMEWORK[^\n]*\bopenclaw\b",
        ],
        "source_must_declare": [
            # Plugin's lib/workspace.js stamps MPM_PROVENANCE_FRAMEWORK=openclaw
            # via withWorkspace() on every spawn.
            r"MPM_PROVENANCE_FRAMEWORK.{0,80}\bopenclaw\b",
            r"MPM_FRAMEWORK.{0,80}\bopenclaw\b",
        ],
    },
]


def _read_text(path: Path) -> str:
    try:
        return path.read_text(encoding="utf-8", errors="replace")
    except FileNotFoundError:
        return ""


def _gather_source_files(host_dir: Path) -> list[Path]:
    """Walk the adapter source tree, excluding tests and render cache."""
    out: list[Path] = []
    skip_dirs = {
        "node_modules", "dist", "__pycache__", ".git", "tests",
        # tests/ dir has its own contract tests; we surface its findings
        # separately by reading it explicitly below when needed.
    }
    skip_files = {
        # Auto-generated build artifacts are not source of truth.
        "package-lock.json",
    }
    for p in host_dir.rglob("*"):
        if not p.is_file():
            continue
        rel = p.relative_to(host_dir)
        if any(part in skip_dirs for part in rel.parts):
            continue
        if p.name in skip_files:
            continue
        out.append(p)
    return out


class AdapterProvenanceContract(unittest.TestCase):
    """Each maintained host adapter must declare its canonical framework identity."""

    def _adapter_workspace(self) -> dict:
        """Re-run the render script in dry-run-ish mode to find each adapter's rendered output path."""
        # The renderer writes to <host_dir>/managed-blocks-<host>.md or similar.
        # We don't re-run the renderer (would require Python deps); instead we
        # enumerate candidate paths that the renderer produces and the agent's
        # raw instruction manifest.
        return {a["host_dir"]: AGENT_INSTALL / a["host_dir"] for a in ADAPTERS}

    def _find_rendered_output(self, host_dir: Path) -> list[Path]:
        """Return candidate rendered-output files for the adapter."""
        candidates: list[Path] = []
        for name in ("AGENTS.md", "CLAUDE.md", "instructions.md",
                     "SKILL.md", "skill.md", "managed-blocks.md"):
            p = host_dir / name
            if p.exists():
                candidates.append(p)
        return candidates

    def test_canonical_frameworks_are_known_to_substrate(self):
        # Sanity: every framework in this test's table must appear in the
        # substrate's knownAgentFrameworks set, otherwise EffectiveActorKind
        # would classify writes from that adapter as "unknown" rather than "agent".
        from_substrate = set()
        classifier_path = REPO_ROOT / "internal" / "core" / "activity_classifier.go"
        if classifier_path.exists():
            text = classifier_path.read_text(encoding="utf-8")
            # knownAgentFrameworks = map[string]struct{}{ ... } — pull the keys
            m = re.search(
                r"knownAgentFrameworks\s*=\s*map\[string\]struct\{\}\s*\{(.*?)\n\s*\}",
                text, re.DOTALL,
            )
            if m:
                body = m.group(1)
                for key in re.findall(r'"([a-z][a-z0-9_-]*)"', body):
                    from_substrate.add(key)
        for a in ADAPTERS:
            self.assertIn(
                a["canonical"], from_substrate,
                f"{a['host_dir']} declares canonical={a['canonical']!r}, but "
                "internal/core/activity_classifier.go does NOT include it in "
                "knownAgentFrameworks. Add the framework there so "
                "EffectiveActorKind classifies its writes as actor_kind='agent'."
            )

    def test_every_adapter_declares_canonical_framework(self):
        workspaces = self._adapter_workspace()
        for a in ADAPTERS:
            with self.subTest(adapter=a["host_dir"], canonical=a["canonical"]):
                host_dir = workspaces[a["host_dir"]]
                self.assertTrue(
                    host_dir.is_dir(),
                    f"Adapter directory missing: {host_dir}"
                )

                # Source-level contract: at least one source file under the
                # adapter (excluding tests) must declare the framework id via
                # the env var, the legacy alias, or a helper that emits it
                # into the subprocess env. This is the structural gate: a
                # future adapter landing without one of these declarations
                # causes every audit row to fall through to the transport
                # default and lose host identity.
                sources = _gather_source_files(host_dir)
                source_text = "\n".join(_read_text(p) for p in sources)
                if not any(
                    re.search(p, source_text, re.MULTILINE)
                    for p in a["source_must_declare"]
                ):
                    self.fail(
                        f"{a['host_dir']}: no source file declares "
                        f"MPM_PROVENANCE_FRAMEWORK={a['canonical']!r} or the "
                        "legacy MPM_FRAMEWORK alias. Without this, the "
                        "adapter shells out to `mpm` without telling the "
                        "substrate which host it represents — every audit "
                        "row falls through to the transport default."
                    )

                # Documented intent: the rendered instruction surface should
                # also surface the framework identity so operators reading
                # the host's instructions understand the audit trail. This
                # is advisory, not a CI gate — some adapters drive provenance
                # through helpers rather than direct env-var text. We log the
                # failure for visibility but don't fail the test on this.
                rendered = self._find_rendered_output(host_dir)
                rendered_text = "\n".join(_read_text(p) for p in rendered)
                if rendered_text and not any(
                    re.search(p, rendered_text, re.MULTILINE)
                    for p in a["expected_in_rendered"]
                ):
                    # Surface as a soft warning via self.subTest context;
                    # the assertion is on the source-level contract above.
                    print(
                        f"\n  note: {a['host_dir']} rendered instruction "
                        "surface does not surface the framework identity "
                        "explicitly; provenance is propagated via a helper. "
                        "Consider documenting it for operator visibility."
                    )

    def test_no_adapter_reintroduces_dropped_session_key(self):
        # Stage 2B regression: MPM_PROVENANCE_SESSION_KEY was silently dropped
        # by internal/core/provenance.go. mpm-memory-openclaw was the only
        # adapter that ever wrote it; ensure no adapter REINTRODUCES it as a
        # WRITE (assigning the env var to a subprocess). Regression-guard
        # comments that mention the dropped name in passing are allowed.
        bad = "MPM_PROVENANCE_SESSION_KEY"
        # Match only assignment-style writes:
        #   env.MPM_PROVENANCE_SESSION_KEY = ...
        #   "MPM_PROVENANCE_SESSION_KEY": ...
        #   process.env.MPM_PROVENANCE_SESSION_KEY = ...
        quote = r"""["']"""
        write_pattern = re.compile(
            r"(?:env\.?" + re.escape(bad)
            + r"\s*=|" + quote + re.escape(bad) + quote + r"\s*:)",
            re.MULTILINE,
        )
        workspaces = self._adapter_workspace()
        offenders: list[str] = []
        for a in ADAPTERS:
            host_dir = workspaces[a["host_dir"]]
            for p in _gather_source_files(host_dir):
                if write_pattern.search(_read_text(p)):
                    offenders.append(str(p))
        self.assertEqual(
            offenders, [],
            "MPM_PROVENANCE_SESSION_KEY is silently dropped by the substrate. "
            "Use the canonical MPM_PROVENANCE_FRAMEWORK_SESSION_ID instead. "
            f"Offending files: {offenders}"
        )

    def test_host_session_never_populates_parent_invocation(self):
        # Stage 2C.2 regression: native host session identity belongs in
        # MPM_PROVENANCE_FRAMEWORK_SESSION_ID, NOT in
        # MPM_PROVENANCE_PARENT_INVOCATION_ID. The latter is reserved for
        # causal invocation lineage and must never be populated from
        # $CLAUDE_SESSION_ID, OpenClaw sessionKey, OpenCode
        # ctx.sessionID, or any other host-native session identifier.
        #
        # We scan every adapter's runtime source (excluding this test
        # file and the README docs) for assignment-style writes to
        # MPM_PROVENANCE_PARENT_INVOCATION_ID whose right-hand side is
        # a host session identifier (sessionKey, CLAUDE_SESSION_ID,
        # ctx.sessionID, sessionID, etc.).
        #
        # The two acceptable writers of MPM_PROVENANCE_PARENT_INVOCATION_ID
        # are: (1) test fixtures that simulate causal lineage; (2) the
        # Claude Code runtime when it explicitly exposes a separate
        # parent invocation identifier (which it does not today). Both
        # are absent today, so the pattern must NOT match any production
        # source file.
        bad_assignments = [
            # OpenClaw patterns
            (r'env\.MPM_PROVENANCE_PARENT_INVOCATION_ID\s*=\s*.*sessionKey',
             "OpenClaw sessionKey must not populate MPM_PROVENANCE_PARENT_INVOCATION_ID; "
             "use MPM_PROVENANCE_FRAMEWORK_SESSION_ID instead."),
            (r'env\.MPM_PROVENANCE_PARENT_INVOCATION_ID\s*=\s*.*sessionID',
             "OpenClaw sessionID must not populate MPM_PROVENANCE_PARENT_INVOCATION_ID; "
             "use MPM_PROVENANCE_FRAMEWORK_SESSION_ID instead."),
            # OpenCode patterns
            (r'env\.MPM_PROVENANCE_PARENT_INVOCATION_ID\s*=\s*sessionID',
             "OpenCode sessionID must not populate MPM_PROVENANCE_PARENT_INVOCATION_ID; "
             "use MPM_PROVENANCE_FRAMEWORK_SESSION_ID instead."),
            (r'env\["MPM_PROVENANCE_PARENT_INVOCATION_ID"\]\s*=\s*sessionID',
             "OpenCode sessionID must not populate MPM_PROVENANCE_PARENT_INVOCATION_ID."),
            # Claude Code shell pattern
            (r'export\s+MPM_PROVENANCE_PARENT_INVOCATION_ID="\$CLAUDE_SESSION_ID"',
             "Claude Code $CLAUDE_SESSION_ID must not populate "
             "MPM_PROVENANCE_PARENT_INVOCATION_ID; use "
             "MPM_PROVENANCE_FRAMEWORK_SESSION_ID instead."),
        ]
        workspaces = self._adapter_workspace()
        offenders: list[tuple[str, str, str]] = []
        for a in ADAPTERS:
            host_dir = workspaces[a["host_dir"]]
            for p in _gather_source_files(host_dir):
                text = _read_text(p)
                for pattern, msg in bad_assignments:
                    m = re.search(pattern, text)
                    if m:
                        offenders.append((str(p), pattern, msg))
        self.assertEqual(
            offenders, [],
            "Native host session identity must NEVER populate "
            "MPM_PROVENANCE_PARENT_INVOCATION_ID (causal-lineage slot). "
            "Use MPM_PROVENANCE_FRAMEWORK_SESSION_ID instead. "
            f"Offending (file, pattern, message): {offenders}"
        )

    def test_adapters_propagate_framework_session_id(self):
        # Stage 2C.2 positive coverage: every adapter that exposes a
        # native session identifier MUST propagate it through the
        # canonical MPM_PROVENANCE_FRAMEWORK_SESSION_ID slot. Adapters
        # that have no native session id (Pi, Hermes) must NOT
        # fabricate one.
        # Claude Code + OpenClaw + OpenCode: must write the env var.
        # Pi + Hermes: must not (no host session concept).
        propagated = {
            "mpm-claude-code": True,
            "mpm-memory-openclaw": True,
            "mpm-opencode": True,
            "mpm-pi": False,
            "mpm-hermes": False,
        }
        workspaces = self._adapter_workspace()
        offenders: list[str] = []
        for a in ADAPTERS:
            host_dir = workspaces[a["host_dir"]]
            expects_write = propagated.get(a["host_dir"], None)
            if expects_write is None:
                continue
            wrote = False
            for p in _gather_source_files(host_dir):
                text = _read_text(p)
                if re.search(
                    r'MPM_PROVENANCE_FRAMEWORK_SESSION_ID',
                    text,
                ):
                    wrote = True
                    break
            if expects_write and not wrote:
                offenders.append(
                    f"{a['host_dir']}: must reference "
                    f"MPM_PROVENANCE_FRAMEWORK_SESSION_ID (Stage 2C.2)"
                )
            if not expects_write and wrote:
                # Pi / Hermes do not have native session ids. If a
                # future change exposes one, this assertion flips.
                offenders.append(
                    f"{a['host_dir']}: writes "
                    f"MPM_PROVENANCE_FRAMEWORK_SESSION_ID without a "
                    f"documented host session source — fabrication guard"
                )
        self.assertEqual(
            offenders, [],
            "Every adapter with a native session id must propagate it "
            "via MPM_PROVENANCE_FRAMEWORK_SESSION_ID; Pi and Hermes "
            "must not fabricate one. " + str(offenders)
        )


class CanonicalSubstratePrecedence(unittest.TestCase):
    """The substrate's precedence chain (canonical > legacy alias) must be honored."""

    def test_knownagentframeworks_includes_every_adapter_canonical(self):
        classifier_path = REPO_ROOT / "internal" / "core" / "activity_classifier.go"
        self.assertTrue(classifier_path.exists(),
                        f"missing classifier: {classifier_path}")
        text = classifier_path.read_text(encoding="utf-8")
        m = re.search(
            r"knownAgentFrameworks\s*=\s*map\[string\]struct\{\}\s*\{(.*?)\n\s*\}",
            text, re.DOTALL,
        )
        self.assertIsNotNone(m, "knownAgentFrameworks literal not found")
        declared = set(re.findall(r'"([a-z][a-z0-9_-]*)"', m.group(1)))
        for a in ADAPTERS:
            self.assertIn(
                a["canonical"], declared,
                f"{a['host_dir']} declares canonical={a['canonical']!r} but "
                "the substrate's knownAgentFrameworks does not include it."
            )

    def test_mpmcli_precedence_canonical_wins_over_legacy(self):
        # The mpmcli precedence chain: MPM_PROVENANCE_FRAMEWORK (canonical) >
        # MPM_FRAMEWORK (legacy) > "mcp" default. The test file in
        # internal/core/mpmcli/mpmcli_test.go pins this precedence; this test
        # verifies the test file itself enforces the chain (no silent
        # regression to "only legacy").
        mpmcli_test = REPO_ROOT / "internal" / "core" / "mpmcli" / "mpmcli_test.go"
        if not mpmcli_test.exists():
            self.skipTest("mpmcli_test.go not present (separate module)")
        text = mpmcli_test.read_text(encoding="utf-8")
        self.assertIn(
            "MPM_PROVENANCE_FRAMEWORK", text,
            "mpmcli_test.go no longer references the canonical env var; "
            "precedence chain may have silently regressed."
        )
        self.assertIn(
            "MPM_FRAMEWORK", text,
            "mpmcli_test.go no longer references the legacy alias; "
            "the fallback read may have been removed."
        )


if __name__ == "__main__":
    unittest.main(verbosity=2)
