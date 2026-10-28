"""Explicit, reviewable exemption registry.

Every entry here is a claim that something which *looks* wrong is deliberately
right. That is why it is a literal Python table with a justification string
rather than a regex or a heuristic: a reviewer can read the list, and adding an
entry is a visible change in the diff.

Two rules for entries:

1. **A justification is mandatory and must be checkable.** "Legacy" is not a
   justification. "This path is an illustrative fixture in a test, not a real
   document" is.
2. **Never add an entry to make a red gate green.** Add it because the gate is
   wrong about this case, and say why in the same sentence.
"""

from __future__ import annotations

from dataclasses import dataclass


@dataclass(frozen=True)
class PathExemption:
    path: str
    why: str


@dataclass(frozen=True)
class TokenExemption:
    token: str
    why: str


# ── Missing-file references ───────────────────────────────────────────────────
# Paths that appear in documentation as *illustrative examples* and never
# existed. Real documents must resolve; these must not be "fixed" by creating
# empty files.
MISSING_PATH_EXEMPTIONS: tuple[PathExemption, ...] = (
    PathExemption(
        "docs/x.md",
        "Illustrative filename in reference-identity tests; never a real document.",
    ),
    PathExemption(
        "docs/y.md",
        "Illustrative filename in reference-identity tests; never a real document.",
    ),
    PathExemption(
        "docs/notes.md",
        "Illustrative filename in the migrate.go comment; never a real document.",
    ),
    PathExemption(
        "docs/guide.md",
        "Illustrative filename in reference-identity tests; never a real document.",
    ),
    PathExemption(
        "docs/api.md",
        "Illustrative filename in the embeddings.go doc comment; never a real document.",
    ),
    PathExemption(
        "docs/menus.md",
        "Illustrative filename in a SPEC prose example about reference collisions; "
        "never a real document.",
    ),
    PathExemption(
        "docs/wordpress-menus.md",
        "Illustrative filename in reference-identity tests; never a real document.",
    ),
    PathExemption(
        "docs/../docs/menus.md",
        "The same illustrative example written with a redundant prefix.",
    ),
    PathExemption(
        "docs/../docs/wordpress-menus.md",
        "The same illustrative example written with a redundant prefix.",
    ),
    PathExemption(
        "docs/mpm-invariants.md",
        "Cited by a restored library document but never committed to this "
        "repository, so it cannot be restored. The library entry says so "
        "explicitly and repoints its one load-bearing fact at the schema.",
    ),
    PathExemption(
        "docs/archive/2026-08-28-mpm-skill-workshop-plan.md",
        "Named in the skill-workshop design as a document that was written but "
        "never committed. The library entry says so explicitly.",
    ),
)

# Verbatim snapshots of text that existed at a point in time.
#
# These are *fixtures*, not documentation. `agent_installation/measure/fixtures/`
# holds byte-for-byte copies of the 3.0.0 managed block, protocol and snippets so
# the 4.0.0 block can be compared against them. Their links point at paths that
# existed then and do not now; that is the property being preserved. Editing a
# snapshot to satisfy a link checker would destroy the only reason it is kept.
#
# Every structural check therefore skips these entirely — not just the path
# checks, because a link inside a snapshot is equally frozen.
FROZEN_SNAPSHOT_PREFIXES: tuple[str, ...] = (
    "agent_installation/measure/fixtures/",
)

# Directory prefixes whose dated/historical entries may legitimately name
# paths that no longer exist. Checked by prefix, not by file, because the
# whole point of a historical record is that it describes a past layout.
HISTORICAL_REFERENCE_PREFIXES: tuple[str, ...] = (
    "docs/library/",                       # curated reference, labelled
    "docs/dev/",                           # session and coordination records
    "docs/CLOSEOUT.md",                    # frozen ledger of a past audit pass
    "docs/ISSUE-",                         # per-issue work reports
    "changelog.md",
    "bench/output/",                       # measurement write-ups
    "agent_installation/FEATURE_DRIVEN_AUDIT_",   # dated audit
    "agent_installation/mpm-opencode/VALIDATION-",  # dated validation snapshot
)

# ── Tool-name tokens that are not tools ───────────────────────────────────────
# `mpm_foo` shapes that the gate would otherwise read as a tool invocation.
TOKEN_EXEMPTIONS: tuple[TokenExemption, ...] = (
    TokenExemption(
        "mpm_help",
        "Discovery surface registered at MCP server init, not a Registry entry. "
        "See cmd/mpm-mcp/main.go.",
    ),
    TokenExemption(
        "mpm_config",
        "Not a tool. `mpm_config.json` is the configuration filename; the token "
        "is matched only because of the shared prefix.",
    ),
    TokenExemption(
        "mpm_call",
        "Not a tool. Prefix used in prose about the `mpm call` bridge.",
    ),
    TokenExemption(
        "mpm_session",
        "Retired tool name, kept in migration code and its regression tests. "
        "cmd/mpm/template_drift_test.go guards against new uses.",
    ),
    TokenExemption(
        "mpm_auto_route",
        "Retired adapter name in historical documents; never a tool.",
    ),
    TokenExemption(
        "mpm_opencode",
        "Adapter package name, not a tool.",
    ),
    TokenExemption(
        "mpm_agent",
        "Adapter/consumer binary name discussed in architecture prose, not a tool.",
    ),
    TokenExemption(
        "mpm_bot",
        "CI bot identity, not a tool.",
    ),
)

# ── Managed-block parity ─────────────────────────────────────────────────────
# The renderer is the authority. This gate shells out to its `--check` mode so
# there is exactly one implementation of "rendered == canonical".
RENDERER_REL = "agent_installation/scripts/render_managed_blocks.py"

# ── Retired hosts ────────────────────────────────────────────────────────────
# Adapter identifiers retired on 2026-10-27.
#
# Brand names are NOT in this list on purpose. "Claude Code" is still a live
# fact about this repository — `CLAUDE.md` is a legitimate contributor
# instruction file loaded by that host, and the retirement of the *MPM adapter*
# does not retire the host. Conflating the two is how an audit deletes a file
# that is doing real work. What is retired is the adapter, so the adapter path
# is what the error-level check looks for; brand mentions are a review prompt.
RETIRED_HOST_BRANDS: tuple[str, ...] = (
    "openclaw",
    "hermes",
    "pi-mpm",
    "pi adapter",
    "claude-code adapter",
    "mpm-claude-code",
)

# A retired-host mention is accepted when the surrounding text says one of
# these. Keyword-based, deliberately conservative: a false NEGATIVE here is a
# missed review item, a false POSITIVE is a gate people turn off.
RETIREMENT_MARKERS: tuple[str, ...] = (
    "retired",
    "retire",
    "removed",
    "no longer",
    "unsupported",
    "gone",
    "former",
    "formerly",
    "was retired",
    "were retired",
    "not supported",
    "decommission",
    "migrated",
    "migration",
    "historical",
    "history",
    "legacy",
    "used to",
    "previously",
    "archive",
)

# The documents that speak for the product *today*. A brand mention here is
# worth a reviewer's attention. A dated report, a validation snapshot or a
# session record is a description of the past and is not listed.
CURRENT_OPERATIONAL_DOCS: tuple[str, ...] = (
    "README.md",
    "AGENTS.md",
    "CLAUDE.md",
    "CLI_AGENT_RULES.md",
    "mpm_integration_check.md",
    "docs/SPEC.md",
    "docs/INSTALL.md",
    "docs/CONFIGURATION.md",
    "docs/CONTRIBUTING.md",
    "docs/SECURITY.md",
    "docs/ISSUES.md",
    "agent_installation/README.md",
    "agent_installation/INSTALL.md",
    "agent_installation/mpm-agent-protocol.md",
    "agent_installation/MPM_AGENT_INTEGRATION_SNIPPETS.md",
    "agent_installation/mpm-opencode/README.md",
)

# ── Build-path claims ────────────────────────────────────────────────────────
# `make build` writes BUILD_DIR (.build/bin). Only `make install` writes the
# install prefix. A document that says otherwise sends a reader to a directory
# that does not exist.
BUILD_DIR = ".build/bin"
INSTALL_PREFIX = "~/.mpm/bin"

# ── Documentation-impact dependency map ──────────────────────────────────────
# Which authoritative documents must be *considered* when a given part of the
# repository changes. This is a review prompt, not an assertion: the gate emits
# a finding, and the finding is discharged by either changing the document or
# recording a justified "no documentation change needed".
@dataclass(frozen=True)
class Dependency:
    code: tuple[str, ...]
    docs: tuple[str, ...]
    why: str


DEPENDENCY_MAP: tuple[Dependency, ...] = (
    Dependency(
        code=(
            "internal/core/tools/registry_list.go",
            "cmd/mpm-mcp/tools.go",
            "cmd/mpm-mcp/main.go",
            "internal/core/tools/compact_surface_filter_test.go",
        ),
        docs=(
            "docs/SPEC.md",
            "AGENTS.md",
            "agent_installation/mpm-agent-protocol.md",
            "docs/library/CONTEXT_EXPOSURE.md",
        ),
        why="tool registry or MCP surface changed — what an agent is told it can call",
    ),
    Dependency(
        code=(
            "agent_installation/MPM_AGENT_INTEGRATION_SNIPPETS.md",
            "agent_installation/scripts/render_managed_blocks.py",
            "agent_installation/mpm-opencode/templates/AGENTS.md.snippet",
            "README.md",
        ),
        docs=(
            "agent_installation/README.md",
            "agent_installation/mpm-agent-protocol.md",
            "AGENTS.md",
        ),
        why="managed instruction source or renderer changed — the block every agent reads",
    ),
    Dependency(
        code=(
            "install.sh",
            "uninstall.sh",
            "scripts/deploy.sh",
            "Makefile",
        ),
        docs=(
            "docs/INSTALL.md",
            "agent_installation/INSTALL.md",
            "README.md",
            "AGENTS.md",
        ),
        why="installer, uninstaller or build entry points changed — install paths and prefixes",
    ),
    Dependency(
        code=(
            "internal/core/schema.go",
            "internal/core/db.go",
            "internal/core/evidence.go",
            "internal/core/confidence",
        ),
        docs=(
            "docs/SPEC.md",
            "docs/library/2026-06-16-confidence-evidence-foundation-design.md",
        ),
        why="storage schema or confidence/evidence model changed — the product contract",
    ),
    Dependency(
        code=(
            "internal/telemetry",
            "cmd/mpm-telemetry",
            "agent_installation/lib/telemetry_adapter.py",
        ),
        docs=(
            "docs/SPEC.md",
            "docs/SECURITY.md",
            "docs/library/2026-08-21-telemetry-binary-design.md",
        ),
        why="telemetry wire format or trust boundary changed",
    ),
    Dependency(
        code=(
            "internal/scheduler",
            "internal/critic",
            "cmd/mpm-scheduler",
            "cmd/mpm-critic",
        ),
        docs=(
            "docs/SPEC.md",
            "CLAUDE.md",
        ),
        why="scheduler or critic safety gates changed",
    ),
    Dependency(
        code=("cmd/mpm/",),
        docs=(
            "CLI_AGENT_RULES.md",
            "docs/SPEC.md",
        ),
        why="CLI surface changed — the normative CLI contract and the command reference",
    ),
)

# ── Stale-status vocabulary ──────────────────────────────────────────────────
# Phrases that mean "this document describes an intention, not the system".
# In a *current operational* document they are a review finding. In library or
# dev documents they are expected and are not reported.
STALE_STATUS_PHRASES: tuple[str, ...] = (
    "awaiting approval",
    "awaiting review",
    "pending approval",
    "pending implementation",
    "not yet implemented",
    "will be implemented",
    "coming soon",
    "design draft",
)
