#!/usr/bin/env python3
"""Regression guard: machine-specific absolute home paths in
agent_installation/** code.

Background
----------
The agent_installation adapter test suites historically hardcoded
literal author-machine paths (e.g. "/home/v/.mpm/agent_installation/...",
"/home/v/workspace/projects/mpm/agent_installation/..."). On any host
other than the original author's, those tests failed with permission
errors or non-existent-path assertions.

This guard scans executable/test fixture code under
agent_installation/** (Python, JavaScript, TypeScript, and shell files)
and rejects concrete developer-home path fragments that would defeat
the portability repair.

The guard follows the procedure's directives:
  - scans executable/test fixture code under agent_installation/**
  - rejects concrete developer-home paths such as /home/v/...
  - permits documentation, historical prose, and explicit synthetic
    test data through clear exclusions
  - does NOT ban /home/... strings indiscriminately

What the guard flags
--------------------
String literals (in code, not in comments) that contain the hardcoded
author-machine prefix `/home/v/`. The pattern is the historical
install/checkout root of the original author — any reappearance in a
runtime code path is a regression to that hardcoded assumption.

What the guard permits
----------------------
- Comments / docstrings that narrate prior defects (Category 1
  historical prose). These reference the historical path to document
  what was wrong; the surrounding code does not depend on the literal.
- Markdown files under agent_installation/**. These are documents,
  not code.
- Files explicitly whitelisted via the WHITELIST constant below,
  with rationale.
- Synthetic test data strings inside template literals or comments
  that simulate a non-canonical state for migration testing — these
  are produced by code via os.homedir() or path.join, not hardcoded
  by the test author.

Run with:
    python3 agent_installation/tests/agent_installation_machine_specific_paths_test.py
"""

from __future__ import annotations

import ast
import os
import re
import sys
import unittest
from pathlib import Path
from typing import Iterable

REPO_ROOT = Path(__file__).resolve().parents[2]

# Files whose `/home/v/` occurrences are intentional Category 1 prose.
# Each entry must have a one-line rationale. Add a file here only when
# the surrounding code does not depend on the literal at runtime.
WHITELIST: dict[str, str] = {
    # Comment-narrating-prior-fix prose. The historical path is
    # referenced to document the regression but never read at runtime.
    "agent_installation/tests/test_clean_install_roundtrip.py":
        "comment narrating prior /home/v path defect",
    "agent_installation/tests/test_cross_adapter_contract_parity.py":
        "comment narrating prior /home/v path defect",
    "agent_installation/tests/test_live_behavioural_verification.py":
        "comment narrating prior /home/v path defect",
    "agent_installation/tests/test_refresh_installed.py":
        "comment narrating prior /home/v path defect",
    "agent_installation/tests/test_render_managed_blocks.py":
        "comment narrating prior /home/v path defect (also CI runner path)",
    # The shell uninstaller documents the previous literal in its
    # header comment; the runtime path uses ${HOME}/.mpm/bin/mpm.
    "agent_installation/uninstall-openclaw.sh":
        "header comment narrating prior hardcoded /home/v literal",
    # The guard itself references the patterns it forbids, in
    # docstrings, constant declarations, and error messages. The
    # pattern values are not used as runtime paths.
    "agent_installation/tests/agent_installation_machine_specific_paths_test.py":
        "guard itself — references the bad pattern in constants/docstrings",
}

# File extensions the guard scans.
SCAN_EXTENSIONS = {".py", ".js", ".ts", ".mjs", ".sh"}

# Author-machine path fragment we forbid in code (not comments).
# The string is intentionally narrow — it matches the historical
# install/checkout root of the original author only.
BAD_PATH_FRAGMENT = "/home/v/"

# A second pattern matches the original author's repository checkout
# root (used in early install paths before the canonical install root
# was settled). The same restrictions apply.
BAD_CHECKOUT_FRAGMENT = "/home/v/workspace/projects/mpm"


def _is_comment_line(line: str, ext: str) -> bool:
    """Return True when the line is unambiguously a comment line in
    the given language's idiom. Indented continuation comments and
    in-line comments after code are NOT matched here — those are
    handled by the AST / per-language pass below."""
    stripped = line.lstrip()
    if not stripped:
        return True
    if ext == ".py" or ext == ".sh":
        return stripped.startswith("#")
    if ext in {".js", ".ts", ".mjs"}:
        return stripped.startswith("//") or stripped.startswith("/*") or stripped.startswith("*")
    return False


def _scan_python(path: Path) -> list[tuple[int, str]]:
    """Return a list of (line_no, snippet) for any /home/v/ string
    literal in non-comment positions in a Python file.

    Walks the AST and inspects string constants. Multi-line strings
    and f-strings are normalised to their textual form; we then check
    whether the resulting string contains the bad fragment.
    Comments are excluded implicitly because AST nodes for string
    constants never contain comment text — only the literal value.
    """
    src = path.read_text(encoding="utf-8", errors="replace")
    try:
        tree = ast.parse(src)
    except SyntaxError:
        # Skip unparseable files (likely generated); the guard is
        # about source code fixtures, not syntax.
        return []
    findings: list[tuple[int, str]] = []
    for node in ast.walk(tree):
        if isinstance(node, ast.Constant) and isinstance(node.value, str):
            value = node.value
            if BAD_PATH_FRAGMENT in value or BAD_CHECKOUT_FRAGMENT in value:
                findings.append((node.lineno, value[:80]))
        elif isinstance(node, (ast.JoinedStr,)):
            # f-strings — concatenate literal parts for the check.
            parts: list[str] = []
            for sub in node.values:
                if isinstance(sub, ast.Constant) and isinstance(sub.value, str):
                    parts.append(sub.value)
            joined = "".join(parts)
            if BAD_PATH_FRAGMENT in joined or BAD_CHECKOUT_FRAGMENT in joined:
                findings.append((node.lineno, joined[:80]))
    return findings


def _scan_grep(path: Path) -> list[tuple[int, str]]:
    """Grep-based scan for non-Python files (JS/TS/shell). We avoid
    AST parsing for these languages and rely on comment-stripping
    at the line level. In-line comments after code are NOT stripped —
    this is acceptable because the historical defect was always a
    bare literal on its own line (Path(...) or template literal)."""
    ext = path.suffix
    findings: list[tuple[int, str]] = []
    for lineno, line in enumerate(path.read_text(encoding="utf-8", errors="replace").splitlines(), start=1):
        if _is_comment_line(line, ext):
            continue
        if BAD_PATH_FRAGMENT in line or BAD_CHECKOUT_FRAGMENT in line:
            findings.append((lineno, line.strip()[:80]))
    return findings


def _scan_one(path: Path) -> list[tuple[int, str]]:
    if path.suffix == ".py":
        return _scan_python(path)
    return _scan_grep(path)


def _iter_scan_targets(root: Path) -> Iterable[Path]:
    for dirpath, _dirnames, filenames in os.walk(root):
        # Skip node_modules / dist — these are generated artifacts.
        dirnames = list(_dirnames)
        if "node_modules" in dirnames:
            dirnames.remove("node_modules")
        if "dist" in dirnames:
            dirnames.remove("dist")
        del _dirnames  # mark unused
        for filename in filenames:
            p = Path(dirpath) / filename
            if p.suffix in SCAN_EXTENSIONS:
                yield p


class TestAgentInstallationMachineSpecificPaths(unittest.TestCase):
    """Pins the no-machine-specific-path invariant for
    agent_installation executable/test fixture code."""

    def test_no_machine_specific_paths_in_fixtures(self):
        root = REPO_ROOT / "agent_installation"
        violations: list[str] = []
        for path in sorted(_iter_scan_targets(root)):
            rel = str(path.relative_to(REPO_ROOT))
            if rel in WHITELIST:
                continue
            for lineno, snippet in _scan_one(path):
                violations.append(
                    f"{rel}:{lineno} contains forbidden author-machine "
                    f"path fragment: {snippet!r}"
                )
        if violations:
            self.fail(
                "agent_installation contains machine-specific author-home "
                "path literals in executable/test code. Resolve via "
                "Path(__file__).resolve().parent (Python), __dirname / "
                "import.meta.url (Node/TS), os.homedir() / os.userHomeDir "
                "(runtime), or ${HOME} (shell) instead.\n"
                + "\n".join(violations)
            )


if __name__ == "__main__":
    unittest.main(verbosity=2)
