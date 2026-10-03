"""
test_no_obsolete_paths.py — Regression coverage for the lifecycle-layout
restructure (2026-09-17, second correction).

Architectural contract:

  Canonical lifecycle layout (this commit):

    install.sh             — public substrate installer
    uninstall.sh           — public substrate uninstaller
    scripts/deploy.sh      — maintainer / release tooling
    agent_installation/    — host/framework integrations (unchanged)

  Production canonical install prefix is $HOME/.mpm (== repo root when
  checked out into ~/.mpm). Therefore the in-tree canonical paths map
  directly to $HOME/.mpm/{install.sh, uninstall.sh, scripts/deploy.sh}.

  Active operational source/docs MUST NOT reference the obsolete layouts
  (each was formerly an active canonical path):

    mpm/install.sh       — formerly here; canonical is install.sh
    mpm/uninstall.sh     — formerly here; canonical is uninstall.sh
    scripts/install.sh   — formerly here; canonical is install.sh

  Adapter-local install.sh files (e.g. agent_installation/mpm-*/install.sh)
  are NOT subject to this invariant — they are their own installer.

  Historical dated documents (docs/archive/**, changelog.md, dated
  validation snapshots) are explicitly excluded — rewriting past reality
  would falsify history.
"""

from __future__ import annotations

import re
import stat
import subprocess
import unittest
from pathlib import Path


REPO_ROOT = Path(__file__).resolve().parents[2]


# Files that legitimately mention the historical path for narrative
# reasons (release notes, changelog entries, history commits).
HISTORICAL_FILES_ALLOWLIST = {
    "changelog.md",
}

# Directories explicitly out of scope.
EXCLUDED_PATH_PREFIXES = (
    "docs/archive/",
    "docs/superpowers/plans/",  # plans describe intent; may quote past
)

# Patterns that count as "the path is actually used as a path" (not a
# narrative mention). We only flag matches where the line uses the
# path as an instruction or invocation form.
#
# Forbidden active-use patterns (each was formerly an active canonical
# path in the wrong layout):
#   * mpm/install.sh / mpm/uninstall.sh — formerly the wrong nested layout;
#     production-side ~/.mpm/{install,uninstall}.sh is NOT matched because
#     the negative lookbehind `(?<!\.)` rejects a leading `.`.
#   * scripts/install.sh               — formerly the old layout.
#
# The current canonical ./install.sh / ./uninstall.sh are NOT forbidden
# here; they are enforced by the canonical-file assertions below.
FORBIDDEN_PATH_PATTERNS = [
    # Old layout: scripts/install.sh was formerly the active path.
    re.compile(r"\bscripts/install\.sh\b"),
    # Wrong nested layout: mpm/install.sh was formerly nested under mpm/;
    # canonical is now install.sh at the repo root. Production-side
    # ~/.mpm/install.sh is allowed (negative lookbehind rejects `.`).
    re.compile(r"(?<![/\w.])mpm/install\.sh\b"),     # matches bare form; was formerly the nested layout
    re.compile(r"(?<![/\w.])mpm/uninstall\.sh\b"),
]


def _list_tracked_files() -> list[str]:
    """Return relative paths of all tracked files in the repo."""
    result = subprocess.run(
        ["git", "ls-files"],
        cwd=REPO_ROOT, capture_output=True, text=True, check=True,
    )
    return [line.strip() for line in result.stdout.splitlines() if line.strip()]


def _is_active_operational(rel: str) -> bool:
    """Active operational surface — top-level docs and operational
    tooling. Adapter directories are explicitly excluded because each
    adapter has its own install.sh (the relative `./install.sh` form
    inside the adapter directory was formerly outside this invariant
    and remains so)."""
    if rel in HISTORICAL_FILES_ALLOWLIST:
        return False
    if any(rel.startswith(p) for p in EXCLUDED_PATH_PREFIXES):
        return False
    if rel.startswith("docs/archive/"):
        return False
    # Adapter directories are out of scope for the lifecycle-path
    # invariant — they have their own canonical install.sh.
    if rel.startswith("agent_installation/"):
        return False
    return True


def _is_narrative_mention(line: str) -> bool:
    """Heuristic: lines that describe migration in past tense are
    narrative and should NOT be flagged. Active-use mentions in
    instructions, code, or commands SHOULD be flagged."""
    narrative_markers = (
        "formerly",
        "moved from",
        "was at",
        "previously",
        "before the restructure",
        "rewrite of past",
        "rewriting past",
        "falsify",
    )
    lowered = line.lower()
    return any(m in lowered for m in narrative_markers)


class TestNoObsoleteInstallPaths(unittest.TestCase):
    """Active operational files must not reference forbidden paths."""

    def test_active_files_no_obsolete_paths(self):
        offenders: list[tuple[str, int, str]] = []
        for rel in _list_tracked_files():
            if not _is_active_operational(rel):
                continue
            path = REPO_ROOT / rel
            try:
                text = path.read_text(encoding="utf-8", errors="ignore")
            except (OSError, UnicodeDecodeError):
                continue
            for n, line in enumerate(text.splitlines(), 1):
                if _is_narrative_mention(line):
                    continue
                for pat in FORBIDDEN_PATH_PATTERNS:
                    if pat.search(line):
                        offenders.append((rel, n, line.strip(), pat.pattern))
                        break
        if offenders:
            msg = "\n".join(f"  {r}:{ln}: [{pat}] {txt}" for r, ln, txt, pat in offenders)
            self.fail(
                "active operational files reference forbidden lifecycle paths:\n"
                f"{msg}\n"
                "Use the canonical 'install.sh' / 'uninstall.sh' / "
                "'scripts/deploy.sh'."
            )


class TestCanonicalRepositoryRoot(unittest.TestCase):
    """The canonical repository root is ~/.mpm.

    MPM installs via `git clone … ~/.mpm`, so that directory is BOTH the
    Git checkout and the runtime root. A doc that tells a user to clone to
    somewhere else as the PRIMARY instruction reintroduces the ambiguity
    this layout exists to remove.

    Deliberately narrow. It does NOT ban:
      * mentions of an alternate checkout where it is framed as the
        supported advanced/alternate layout;
      * migration guidance naming older paths;
      * anything under docs/archive/ (historical record);
      * host-specific paths in validation snapshots and test fixtures.

    What it forbids is a *clone instruction* aimed at a non-canonical
    destination, in a current doc, outside a migration context.
    """

    # Only these are checked: current, user-facing install documentation.
    GUARDED_DOCS = (
        "README.md",
        "AUTO_AGENT_INSTALL.md",
        "docs/INSTALL.md",
        "docs/SPEC.md",
        "agent_installation/INSTALL.md",
    )

    # `git clone <url> <dest>` where dest is not ~/.mpm. A clone with no
    # explicit destination (bare `git clone <url>`) is not a violation.
    # The URL is matched as a unit so that a repo path containing '/' is
    # not mistaken for the destination argument.
    _CLONE = re.compile(
        r"git\s+clone\s+(?P<url>(?:https?://|git@)\S+|[\w./-]+)"
        r"(?:\s+(?P<dest>~/\S+|[./~$][\w./~-]*))?"
    )

    # Lines that legitimately name a non-canonical path.
    ALLOWED_CONTEXT = (
        "advanced",          # "Advanced: install.sh also supports …"
        "alternate",         # the documented alternate checkout
        "migrat",            # migration / recovery guidance
        "histor",            # historical layouts
        "formerly",
        "previously",
        "older layout",
        "not the primary",
        "supported as",
    )

    def test_primary_clone_target_is_canonical_mpm(self):
        offenders: list[str] = []
        for rel in self.GUARDED_DOCS:
            path = REPO_ROOT / rel
            if not path.is_file():
                continue
            text = path.read_text(encoding="utf-8", errors="ignore")
            for n, line in enumerate(text.splitlines(), 1):
                low = line.lower()
                if any(ctx in low for ctx in self.ALLOWED_CONTEXT):
                    continue
                m = self._CLONE.search(line)
                if not m:
                    continue
                dest = m.group("dest")
                if dest is None:
                    continue  # bare `git clone <url>` — no destination given
                # Markdown prose trails punctuation and backticks off the
                # path ("`git clone … ~/.mpm`, then install."). Strip them
                # before comparing, or a correct instruction reads as a
                # different destination.
                dest = dest.rstrip("/").strip("`\"',;()[]*")
                if dest == "~/.mpm":
                    continue
                offenders.append(f"  {rel}:{n}: {line.strip()}")
        if offenders:
            self.fail(
                "a primary 'git clone' instruction targets a non-canonical "
                "destination; ~/.mpm is the canonical repository root:\n"
                + "\n".join(offenders)
                + "\nUpdate it to `git clone https://github.com/flowbyte-com/mpm ~/.mpm`, "
                "or frame the alternate layout explicitly as advanced/alternate."
            )

    def test_canonical_install_sequence_documented(self):
        """The canonical sequence must be stated somewhere a user will find it."""
        needle = "git clone https://github.com/flowbyte-com/mpm ~/.mpm"
        found = any(
            needle in (REPO_ROOT / rel).read_text(encoding="utf-8", errors="ignore")
            for rel in self.GUARDED_DOCS
            if (REPO_ROOT / rel).is_file()
        )
        self.assertTrue(
            found,
            "the canonical install sequence "
            f"({needle!r}) is not documented in any current install doc",
        )


class TestCanonicalLifecycleFiles(unittest.TestCase):
    """Spec §1: the canonical lifecycle files exist where expected, are
    executable, and legacy locations are gone."""

    def test_install_sh_exists(self):
        self.assertTrue(
            (REPO_ROOT / "install.sh").is_file(),
            "install.sh missing at repo root",
        )

    def test_uninstall_sh_exists(self):
        self.assertTrue(
            (REPO_ROOT / "uninstall.sh").is_file(),
            "uninstall.sh missing at repo root",
        )

    def test_scripts_deploy_sh_exists(self):
        self.assertTrue(
            (REPO_ROOT / "scripts" / "deploy.sh").is_file(),
            "scripts/deploy.sh missing",
        )

    def _assert_executable(self, path: Path, label: str):
        mode = path.stat().st_mode
        self.assertTrue(mode & stat.S_IXUSR,
                        f"{label} is not executable (mode={oct(mode & 0o777)})")

    def test_install_sh_is_executable(self):
        self._assert_executable(REPO_ROOT / "install.sh", "install.sh")

    def test_uninstall_sh_is_executable(self):
        self._assert_executable(REPO_ROOT / "uninstall.sh", "uninstall.sh")

    def test_scripts_deploy_sh_is_executable(self):
        self._assert_executable(REPO_ROOT / "scripts" / "deploy.sh", "scripts/deploy.sh")

    def test_no_mpm_install_sh(self):
        self.assertFalse(
            (REPO_ROOT / "mpm" / "install.sh").exists(),
            "mpm/install.sh still exists; was formerly the nested layout; canonical is install.sh at the repo root",
        )

    def test_no_mpm_uninstall_sh(self):
        self.assertFalse(
            (REPO_ROOT / "mpm" / "uninstall.sh").exists(),
            "mpm/uninstall.sh still exists; was formerly the nested layout; canonical is uninstall.sh at the repo root",
        )

    def test_no_scripts_install_sh(self):
        self.assertFalse(
            (REPO_ROOT / "scripts" / "install.sh").exists(),
            "scripts/install.sh still exists; was formerly the active path; canonical is install.sh",
        )

    def test_no_deploy_sh_at_repo_root(self):
        self.assertFalse(
            (REPO_ROOT / "deploy.sh").exists(),
            "root-level deploy.sh still exists; should be under scripts/deploy.sh",
        )


if __name__ == "__main__":
    unittest.main()
