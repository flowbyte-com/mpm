"""
test_naming_invariants.py — Repository-level naming-convention regression
guard.

The 2026-09-17 namespace migration established the canonical convention:

    mpm-<host>                    for a single-integration host
    mpm-<capability>-<host>       for a host with multiple independent
                                  MPM integrations (OpenClaw was the
                                  retired case)

The pre-2026-09-17 convention used the inverted namespace:

    <host>-mpm                   e.g. opencode-mpm, pi-mpm, hermes-mpm
    <host>-mpm-<capability>      e.g. openclaw-mpm-memory

This test pins the canonical invariant:

    1. The canonical adapter directories exist.
    2. None of the six obsolete adapter directories remain.
    3. Active tracked files do not reference any of the six obsolete
       directory names or plugin ids — with the EXPLICIT exception of
       historical archive (docs/archive/*) and dated audit/validation
       snapshots (VALIDATION-*.md, FEATURE_DRIVEN_AUDIT_*.md,
       changelog.md). Those intentionally document the old state.

After the 2026-10-27 OpenCode-only adapter removal, OpenCode is the
only supported host, so mpm-opencode is the only canonical adapter
directory. The adapter RETIREMENT is not the same as the NAMESPACE
migration: the obsolete-name invariant below still holds, and the
historical allowlist still names the retired adapters' own documents
where they survive.

If this test fails, somebody reintroduced an old namespace reference in
active code/docs. The remediation is the same migration that produced
this test: rename directory, rename plugin id, update references.

Run with:
    python3 -m unittest agent_installation.tests.test_naming_invariants
"""

from __future__ import annotations

import os
import re
import json
import subprocess
import unittest
from pathlib import Path

AGENT_INSTALLATION = Path(__file__).resolve().parent.parent
REPO_ROOT = AGENT_INSTALLATION.parent

# Canonical adapter directory names (the post-2026-09-17 convention).
# OpenCode is the only supported host since the 2026-10-27 adapter
# removal, so mpm-opencode is the only adapter directory that remains.
CANONICAL_ADAPTERS = {
    "mpm-opencode",
}

# Adapter directories retired on 2026-10-27 (Session A's adjudication on
# PR #126). They are recorded here so the retirement is pinned rather
# than merely implied by their absence.
RETIRED_ADAPTERS = {
    "mpm-claude-code",
    "mpm-hermes",
    "mpm-pi",
    "mpm-memory-openclaw",
    "mpm-auto-mode-persona-openclaw",
}

# Obsolete adapter directory names (the pre-2026-09-17 convention).
OBSOLETE_ADAPTERS = {
    "claude-code-mpm",
    "hermes-mpm",
    "opencode-mpm",
    "pi-mpm",
    "openclaw-mpm-memory",
    "openclaw-mpm-auto-mode-persona",
}

# All forbidden substrings in active code/docs. These are matched
# word-boundary-style: any of these substrings anywhere in an active
# tracked file is a regression.
FORBIDDEN_NAMES = sorted(OBSOLETE_ADAPTERS)

# The subset of the obsolete names that were ADAPTER DIRECTORY names
# (the pre-2026-09-17 convention renamed the directories themselves).
# The remaining obsolete names are legacy PLUGIN ids that a migration
# or uninstall path may legitimately name.
OBSOLETE_ADAPTER_DIR_NAMES = {
    "claude-code-mpm",
    "hermes-mmp",
    "opencode-mpm",
    "pi-mpm",
}

# Paths that are allowed to retain old names. They are explicitly
# historical record and are not active code.
#
# `docs/library/` replaced `docs/archive/` on 2026-10-28. It holds the
# same kind of material — designs, audits and incident records — with a
# status label on every entry saying whether it describes today or the
# past. A historical design document that names the adapter which existed
# when it was written is correct; rewriting it to use today's vocabulary
# would falsify the record and hide which adapter the design was about.
#
# `scripts/docs_check/` is the freshness gate. It carries the retired
# adapter names as the *pattern list* it searches for, so a current
# document that reintroduces one fails. That is the opposite of a stale
# reference: it is the rule that keeps them out.
HISTORICAL_ALLOWLIST_DIRS = (
    REPO_ROOT / "docs" / "archive",
    REPO_ROOT / "docs" / "library",
    REPO_ROOT / "scripts" / "docs_check",
)
HISTORICAL_ALLOWLIST_FILES = (
    REPO_ROOT / "changelog.md",
    REPO_ROOT / "agent_installation" / "FEATURE_DRIVEN_AUDIT_2026-09-04.md",
    REPO_ROOT / "agent_installation" / "mpm-opencode" / "VALIDATION-2026-08-19.md",
)

# Files that intentionally mention legacy namespace strings because
# they implement or test the one-time migration away from those names.
# These references are NOT stale — they are the canonical "this is the
# legacy namespace we are migrating FROM" declarations. Removing them
# would break the migration logic they pin.
#
# This exemption is NARROW: it covers only files whose job is to handle
# the migration (the installer's legacy-id probe + the migration tests).
# It does NOT broadly exempt the entire adapter directory — new code,
# prose, or runtime identifiers must use the canonical mpm-* namespace.
MIGRATION_AWARE_FILES = {
    # INSTALL.md OpenClaw section — documents the legacy plugin ids so
    # operators with pre-2026-09-17 installs know what to remove.
    "agent_installation/INSTALL.md",
    # mpm_integration_check.md — the §17.6 OpenClaw diagnostic surfaces
    # the legacy plugin ids as examples that should NOT appear in active
    # artefacts. The legacy names are the test inputs.
    "mpm_integration_check.md",
    # The OpenCode adapter installer — uses 'opencode-mpm' in comments
    # and migration-detection logic to recognise and migrate the legacy
    # namespace from `opencode-mpm` to `mpm-opencode`.
    "agent_installation/mpm-opencode/install.sh",
    # The OpenCode adapter install tests — assert the migration
    # behavior on the legacy path.
    "agent_installation/mpm-opencode/tests/test_opencode_install.py",
    # The JSONC migration helper — carries the canonical
    # "this is what we are migrating FROM" declarations for the legacy
    # OpenCode plugin id (recognised by directory component, and written
    # into fixtures). Same migration role as install.sh, which is the
    # code that used to hold this logic inline.
    "agent_installation/mpm-opencode/scripts/jsonc_config.py",
    # The JSONC migration unit tests — pin detection and replacement of
    # the legacy entry. The legacy id is the test input.
    "agent_installation/mpm-opencode/tests/test_jsonc_config.py",
    # The OpenCode resolver tests — they enumerate the obsolete
    # 'opencode-mpm' name in test descriptions and assertions
    # specifically BECAUSE they pin that the active source no longer
    # references it (B9 invariant). Removing the references would
    # remove the test of the invariant.
    "agent_installation/mpm-opencode/tests/test_resolve_mpm_binary.test.ts",
    # The managed-block convergence tests — managed_block_convergence
    # is the single owner of "what is an MPM-owned region", and it must
    # recognize every marker form MPM has ever written, including
    # pre-2026-09-17 host ids. The legacy names are the parser's test
    # INPUTS:
    #   test_recognizes_hermes_legacy_spaced_anchor feeds the legacy
    #     spaced anchor `MPM MANAGED BLOCK:hermes-mpm` and asserts the
    #     analyzer reports host_id "hermes-mpm".
    #   test_recognizes_hermes_hyphenated_block_anchor is its direct
    #     pair and already covers the canonical `mpm-hermes` form.
    # Changing the legacy input to the canonical id would collapse the
    # pair into a duplicate and delete the migration-path coverage
    # 19770265 added. Same category as test_pi_settings_installer.py
    # and test_opencode_install.py above.
    "agent_installation/tests/test_managed_block_convergence.py",
    # This very test file — the forbidden-name lists and the legacy
    # documentation must enumerate the old names. Excluding it would
    # be self-defeating (the test would ban itself).
    "agent_installation/tests/test_naming_invariants.py",
}


def _git_ls_files() -> list[str]:
    """Return all tracked files relative to REPO_ROOT."""
    out = subprocess.check_output(
        ["git", "-C", str(REPO_ROOT), "ls-files", "-z"],
        text=True,
    )
    return [p for p in out.split("\0") if p]


def _is_historical(rel_path: str) -> bool:
    """True if `rel_path` is allowed to retain obsolete names."""
    p = Path(rel_path)
    for allowed_dir in HISTORICAL_ALLOWLIST_DIRS:
        try:
            p.relative_to(allowed_dir.relative_to(REPO_ROOT))
            return True
        except ValueError:
            continue
    for allowed_file in HISTORICAL_ALLOWLIST_FILES:
        if p == allowed_file.relative_to(REPO_ROOT):
            return True
    # Migration-aware files are allowed to mention the legacy id as
    # the canonical "this is what we are migrating FROM" declaration.
    if rel_path in MIGRATION_AWARE_FILES:
        return True
    return False


class TestNamingInvariants(unittest.TestCase):
    """Pins the post-2026-09-17 canonical mpm-* namespace."""

    # ---- Layout invariants -------------------------------------------

    def test_canonical_adapter_directories_all_exist(self) -> None:
        """All six canonical adapter directories exist on disk."""
        for name in sorted(CANONICAL_ADAPTERS):
            d = AGENT_INSTALLATION / name
            self.assertTrue(
                d.is_dir(),
                f"canonical adapter directory missing: agent_installation/{name}/",
            )

    def test_obsolete_adapter_directories_do_not_exist(self) -> None:
        """None of the six obsolete adapter directories remain."""
        for name in sorted(OBSOLETE_ADAPTERS):
            d = AGENT_INSTALLATION / name
            self.assertFalse(
                d.exists(),
                f"obsolete adapter directory still on disk: agent_installation/{name}/ — "
                f"this means the migration was reverted or a copy was reintroduced",
            )

    def test_no_dual_layout(self) -> None:
        """There is exactly one canonical layout — no compatibility aliases.

        `lib/` holds the host-INDEPENDENT pieces of MPM's agent
        installation (the telemetry adapter lives there, so retiring a
        host cannot take it with it). It is part of the canonical
        layout, not a compatibility alias.

        `measure/` holds the verification harnesses that are not part of
        the installation machinery — the mutation probe that proves the
        managed-block tests are non-vacuous, and the installer
        regression driver. It is canonical too: a harness that MUTATES
        the canonical source needs its own home rather than living
        beside the scripts it mutates.
        """
        siblings = sorted(p.name for p in AGENT_INSTALLATION.iterdir() if p.is_dir())
        # The canonical adapters + scripts + tests + measure + the
        # host-independent lib
        expected_dirs = {"scripts", "tests", "lib", "measure"} | CANONICAL_ADAPTERS
        self.assertEqual(
            set(siblings),
            expected_dirs,
            "agent_installation/ has unexpected top-level directories: "
            f"got {sorted(siblings)}",
        )

    # ---- Reference invariants ----------------------------------------

    def test_no_obsolete_names_in_active_tracked_files(self) -> None:
        """Active tracked files contain zero references to obsolete names.

        The historical allowlist (docs/archive/*, dated audit/validation
        snapshots, changelog) is permitted to retain them — those
        document past state by design.
        """
        offenders: list[tuple[str, str, int, str]] = []
        for rel in _git_ls_files():
            if _is_historical(rel):
                continue
            # Skip binary blobs / package-lock.json (regenerated by npm).
            if rel.endswith("package-lock.json"):
                continue
            abs_path = REPO_ROOT / rel
            try:
                with open(abs_path, "r", encoding="utf-8", errors="replace") as f:
                    for lineno, line in enumerate(f, start=1):
                        for name in FORBIDDEN_NAMES:
                            if name in line:
                                offenders.append((rel, name, lineno, line.rstrip()))
                                break
            except (OSError, UnicodeError):
                # Binary or unreadable — skip.
                continue

        if offenders:
            sample = "\n".join(
                f"  {rel}:{lineno} [{name}] {line[:120]}"
                for rel, name, lineno, line in offenders[:20]
            )
            self.fail(
                "active tracked files still reference obsolete names "
                f"({len(offenders)} occurrences):\n{sample}\n\n"
                "Either the migration is incomplete or these are intentional — "
                "add to HISTORICAL_ALLOWLIST_FILES if intentional."
            )

    # ---- Plugin identity invariants ----------------------------------

    def test_retired_adapter_directories_are_absent(self) -> None:
        """The adapters retired on 2026-10-27 must not come back.

        Claude Code, Pi, Hermes and the two OpenClaw integrations are
        no longer supported hosts. Their directories are gone from the
        repository; this pins that, so a stray copy (or a re-added
        package) cannot quietly become a second source of truth for
        the shared registries.
        """
        for name in sorted(RETIRED_ADAPTERS):
            d = AGENT_INSTALLATION / name
            self.assertFalse(
                d.exists(),
                f"retired adapter directory is back on disk: agent_installation/{name}/",
            )

    def test_no_active_adapter_ships_a_retired_plugin_id(self) -> None:
        """A surviving adapter must not claim a retired plugin id.

        The retired adapters' plugin ids (`mpm-memory-openclaw`,
        `mpm-auto-mode-persona-openclaw`) are historical provenance
        identifiers: they stay in the historical record, but no live
        manifest may present itself as one of them. A resurrected
        OpenClaw package would trip this.
        """
        retired_ids = {
            "mpm-memory-openclaw",
            "mpm-auto-mode-persona-openclaw",
        }
        offenders = []
        for adapter in sorted(AGENT_INSTALLATION.glob("mpm-*")):
            if not adapter.is_dir():
                continue
            for manifest in adapter.rglob("*.plugin.json"):
                try:
                    data = json.loads(manifest.read_text(encoding="utf-8"))
                except (OSError, ValueError):
                    continue
                if data.get("id") in retired_ids:
                    offenders.append(f"{manifest}: {data.get('id')}")
        self.assertEqual(
            offenders,
            [],
            "a live adapter manifest claims a retired plugin id: "
            + "; ".join(offenders),
        )

    def test_opencode_plugin_id_is_canonical(self) -> None:
        """OpenCode plugin id is mpm-opencode (in src/index.ts)."""
        idx = (AGENT_INSTALLATION / "mpm-opencode" / "src" / "index.ts").read_text()
        m = re.search(r'id:\s*"([^"]+)"', idx)
        self.assertIsNotNone(m, "could not locate OpenCode plugin id literal")
        self.assertEqual(
            m.group(1),
            "mpm-opencode",
            "OpenCode plugin id must be mpm-opencode",
        )

    # ---- Boundary invariants -----------------------------------------

    def test_root_install_sh_remains_host_agnostic(self) -> None:
        """The root install.sh does not reference adapter paths or names.

        2026-09-17 commit 7682fa21 moved install.sh into mpm/ for one
        release; that move was reverted in ab628144 (canonical layout
        puts install.sh at the repo root). The test path was updated
        to mpm/install.sh in 7682fa21 but the matching revert was
        missed — the test has been silently erroring with
        FileNotFoundError ever since. Fix the path to the actual
        location of the canonical installer.
        """
        src = (REPO_ROOT / "install.sh").read_text()
        for name in FORBIDDEN_NAMES + sorted(CANONICAL_ADAPTERS):
            self.assertNotIn(
                name,
                src,
                f"install.sh must not reference '{name}' — it is host-agnostic",
            )

    def test_canonical_install_doc_references_only_canonical(self) -> None:
        """The canonical installation doc names only canonical adapter paths.

        AUTO_AGENT_INSTALL.md was retired (it described a multi-host
        dispatch that no longer reflects the release). The canonical
        installation authority is now agent_installation/INSTALL.md, and
        it must carry the same "no obsolete adapter directory" invariant
        the retired file carried.

        The legacy *plugin ids* (`openclaw-mpm-memory`,
        `openclaw-mpm-auto-mode-persona`) remain permitted here: the
        OpenClaw uninstall section documents them so the operator
        recognises a pre-2026-09-17 registration. That file is already
        exempted as MIGRATION_AWARE.
        """
        src = (REPO_ROOT / "agent_installation" / "INSTALL.md").read_text()
        for name in OBSOLETE_ADAPTER_DIR_NAMES:
            self.assertNotIn(
                name,
                src,
                f"agent_installation/INSTALL.md must not reference obsolete "
                f"adapter directory '{name}'",
            )


if __name__ == "__main__":
    unittest.main()
