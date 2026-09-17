"""
test_naming_invariants.py — Repository-level naming-convention regression
guard.

The 2026-09-17 namespace migration established the canonical convention:

    mpm-<host>                    for a single-integration host
    mpm-<capability>-<host>       for a host with multiple independent
                                  MPM integrations (OpenClaw is the
                                  current case)

The pre-2026-09-17 convention used the inverted namespace:

    <host>-mpm                   e.g. opencode-mpm, pi-mpm, hermes-mpm
    <host>-mpm-<capability>      e.g. openclaw-mpm-memory

This test pins the canonical invariant:

    1. The six canonical adapter directories exist.
    2. None of the six obsolete adapter directories remain.
    3. Active tracked files do not reference any of the six obsolete
       directory names or plugin ids — with the EXPLICIT exception of
       historical archive (docs/archive/*) and dated audit/validation
       snapshots (VALIDATION-*.md, FEATURE_DRIVEN_AUDIT_*.md,
       changelog.md). Those intentionally document the old state.

If this test fails, somebody reintroduced an old namespace reference in
active code/docs. The remediation is the same migration that produced
this test: rename directory, rename plugin id, update references.

Run with:
    python3 -m unittest agent_installation.tests.test_naming_invariants
"""

from __future__ import annotations

import os
import re
import subprocess
import unittest
from pathlib import Path

AGENT_INSTALLATION = Path(__file__).resolve().parent.parent
REPO_ROOT = AGENT_INSTALLATION.parent

# Canonical adapter directory names (the post-2026-09-17 convention).
CANONICAL_ADAPTERS = {
    "mpm-claude-code",
    "mpm-hermes",
    "mpm-opencode",
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

# Paths that are allowed to retain old names. They are explicitly
# historical record and are not active code.
HISTORICAL_ALLOWLIST_DIRS = (
    REPO_ROOT / "docs" / "archive",
)
HISTORICAL_ALLOWLIST_FILES = (
    REPO_ROOT / "changelog.md",
    REPO_ROOT / "agent_installation" / "FEATURE_DRIVEN_AUDIT_2026-09-04.md",
    REPO_ROOT / "agent_installation" / "mpm-claude-code" / "VALIDATION-2026-08-19.md",
    REPO_ROOT / "agent_installation" / "mpm-opencode" / "VALIDATION-2026-08-19.md",
    REPO_ROOT / "agent_installation" / "mpm-memory-openclaw" / "VALIDATION-2026-08-19.md",
)

# Files that intentionally mention the legacy plugin id `openclaw-mpm-memory`
# because they implement or test the one-time legacy-id reconciliation
# step in the OpenClaw memory adapter installer. These references are
# not stale — they are the canonical "this is the legacy id we are
# migrating from" declarations, and removing them would break the
# migration.
MIGRATION_AWARE_FILES = {
    # The installer itself — declares LEGACY_PLUGIN_ID and uses it to
    # probe for legacy config to migrate.
    "agent_installation/mpm-memory-openclaw/install.sh",
    # The installer tests — assert the legacy id is absent from active
    # surfaces (manifest, index.js) and that the migration step is
    # silent when the legacy id is not present.
    "agent_installation/mpm-memory-openclaw/tests/installer.test.js",
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
        """There is exactly one canonical layout — no compatibility aliases."""
        siblings = sorted(p.name for p in AGENT_INSTALLATION.iterdir() if p.is_dir())
        # Expect the 6 canonical adapters + scripts + tests
        expected_dirs = {"scripts", "tests"} | CANONICAL_ADAPTERS
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

    def test_openclaw_memory_manifest_id_is_canonical(self) -> None:
        """mpm-memory-openclaw manifest id is mpm-memory-openclaw."""
        import json
        manifest = AGENT_INSTALLATION / "mpm-memory-openclaw" / "openclaw.plugin.json"
        data = json.loads(manifest.read_text())
        self.assertEqual(
            data["id"],
            "mpm-memory-openclaw",
            "openclaw.plugin.json id must be mpm-memory-openclaw",
        )

    def test_openclaw_auto_mode_persona_manifest_id_is_canonical(self) -> None:
        """mpm-auto-mode-persona-openclaw manifest id matches."""
        import json
        manifest = (
            AGENT_INSTALLATION / "mpm-auto-mode-persona-openclaw" / "openclaw.plugin.json"
        )
        data = json.loads(manifest.read_text())
        self.assertEqual(
            data["id"],
            "mpm-auto-mode-persona-openclaw",
            "openclaw.plugin.json id must be mpm-auto-mode-persona-openclaw",
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

    def test_root_scripts_install_sh_remains_host_agnostic(self) -> None:
        """The root scripts/install.sh does not reference adapter paths or names."""
        src = (REPO_ROOT / "scripts" / "install.sh").read_text()
        for name in FORBIDDEN_NAMES + sorted(CANONICAL_ADAPTERS):
            self.assertNotIn(
                name,
                src,
                f"scripts/install.sh must not reference '{name}' — it is host-agnostic",
            )

    def test_auto_agent_install_md_references_only_canonical(self) -> None:
        """AUTO_AGENT_INSTALL.md references only the six canonical adapter paths."""
        src = (REPO_ROOT / "AUTO_AGENT_INSTALL.md").read_text()
        for name in FORBIDDEN_NAMES:
            self.assertNotIn(
                name,
                src,
                f"AUTO_AGENT_INSTALL.md must not reference obsolete '{name}'",
            )


if __name__ == "__main__":
    unittest.main()
