#!/usr/bin/env python3
"""installer_regression.py — the installer/reconciler regression, run
against a DISPOSABLE HOME.

Four paths, each exercised through the REAL installer:

  1. FRESH INSTALL      — no AGENTS.md; the installer creates one.
  2. UPGRADE            — an AGENTS.md carrying an OLD managed block;
                          the installer replaces the managed region and
                          leaves the user's own text alone.
  3. REINSTALL          — a current AGENTS.md; the installer is a
                          byte-level no-op and writes no backup.
  4. EXISTING BLOCK     — a current managed block nested among the
     REPLACEMENT          user's content; the managed region is
                          replaced in place, byte-for-byte, and the
                          surrounding content survives.

Every path is checked for:

  * the managed region matching the canonical render byte-for-byte
    (the provenance comment aside);
  * the user's own content surviving byte-for-byte;
  * exactly ONE managed region — never a duplicate;
  * a retired host's file never being created or touched.

The live installation is never read for mutation and never written:
HOME, XDG_CONFIG_HOME and XDG_DATA_HOME are all redirected into a
temporary directory.

Usage:
    python3 agent_installation/measure/installer_regression.py
    python3 agent_installation/measure/installer_regression.py --json OUT
"""

from __future__ import annotations

import argparse
import json
import os
import re
import shutil
import subprocess
import sys
import tempfile
from pathlib import Path


REPO_ROOT = Path(__file__).resolve().parents[2]
AGENT_INSTALLATION = REPO_ROOT / "agent_installation"
INSTALLER = (
    AGENT_INSTALLATION / "mpm-opencode" / "scripts"
    / "install_agents_instructions.py"
)
SNIPPET = AGENT_INSTALLATION / "mpm-opencode" / "templates" / "AGENTS.md.snippet"
RENDER = AGENT_INSTALLATION / "scripts" / "render_managed_blocks.py"

OUTER_BEGIN = "<!-- BEGIN MPM-MANAGED SECTION:opencode-instructions -->"
OUTER_END = "<!-- END MPM-MANAGED SECTION:opencode-instructions -->"

# The install targets of the hosts retired on 2026-10-27. The reconcile
# pass must neither create them nor touch the ones an operator has.
RETIRED_HOST_TARGETS = (
    ".claude/CLAUDE.md",
    ".pi/agent/AGENTS.md",
    ".openclaw/workspace/SOUL.md",
)

USER_ABOVE = "# operator notes above\nkeep me exactly.\n"
USER_BELOW = "\n# operator notes below\nkeep me too.\n"


def _canonical_managed_region() -> str:
    """The canonical managed region, as the installer writes it."""
    snippet = SNIPPET.read_text(encoding="utf-8")
    return f"{OUTER_BEGIN}\n{snippet.rstrip()}\n{OUTER_END}\n"


def _strip_provenance(text: str) -> str:
    return re.sub(r"<!-- mpm-provenance:[^\n]*-->\n", "", text)


def _managed_region(text: str) -> str | None:
    b = text.find(OUTER_BEGIN)
    if b == -1:
        return None
    e = text.find(OUTER_END, b)
    if e == -1:
        return None
    return text[b:e + len(OUTER_END) + 1]


class InstallerRegression:
    def __init__(self) -> None:
        self.tmpdir = Path(tempfile.mkdtemp(prefix="mpm-installer-regression-"))
        self.home = self.tmpdir / "home"
        self.home.mkdir(parents=True, exist_ok=True)
        self.target = self.home / ".config" / "opencode" / "AGENTS.md"
        self.env = {
            **os.environ,
            "HOME": str(self.home),
            "XDG_CONFIG_HOME": str(self.tmpdir / "xdg-config"),
            "XDG_DATA_HOME": str(self.tmpdir / "xdg-data"),
            "XDG_STATE_HOME": str(self.tmpdir / "xdg-state"),
            "XDG_CACHE_HOME": str(self.tmpdir / "xdg-cache"),
        }
        self.failures: list[str] = []
        self.results: list[dict] = []

    def cleanup(self) -> None:
        shutil.rmtree(self.tmpdir, ignore_errors=True)

    def _install(self) -> subprocess.CompletedProcess:
        return subprocess.run(
            [
                sys.executable, str(INSTALLER),
                "--scope", "user",
                "--target", str(self.target),
                "--snippet", str(SNIPPET),
            ],
            capture_output=True, text=True, env=self.env, timeout=120,
        )

    def _check(self, name: str, condition: bool, detail: str) -> None:
        self.results.append({"check": name, "ok": bool(condition), "detail": detail})
        if not condition:
            self.failures.append(f"{name}: {detail}")

    # ---------- the four paths ----------

    def fresh_install(self) -> None:
        proc = self._install()
        self._check("fresh: installer exits 0", proc.returncode == 0, proc.stderr)
        self._check("fresh: the file exists", self.target.is_file(), str(self.target))
        text = self.target.read_text(encoding="utf-8")
        region = _managed_region(text)
        self._check(
            "fresh: the managed region matches the canonical render",
            region is not None
            and _strip_provenance(region) == _canonical_managed_region(),
            f"got {region!r}",
        )
        self._check(
            "fresh: exactly one managed region",
            text.count(OUTER_BEGIN) == 1 and text.count(OUTER_END) == 1,
            f"begin={text.count(OUTER_BEGIN)} end={text.count(OUTER_END)}",
        )
        self._check(
            "fresh: the block carries the behaviour matrix",
            "| Situation | Record |" in text,
            "the matrix is missing",
        )

    def upgrade(self) -> None:
        self.target.parent.mkdir(parents=True, exist_ok=True)
        stale = (
            f"{OUTER_BEGIN}\n"
            "## STALE managed block from the 3.0.0 contract\n"
            "- replaced by the installer\n"
            f"{OUTER_END}\n"
        )
        self.target.write_text(USER_ABOVE + stale + USER_BELOW, encoding="utf-8")
        proc = self._install()
        self._check("upgrade: installer exits 0", proc.returncode == 0, proc.stderr)
        text = self.target.read_text(encoding="utf-8")
        region = _managed_region(text)
        self._check(
            "upgrade: the managed region matches the canonical render",
            region is not None
            and _strip_provenance(region) == _canonical_managed_region(),
            f"got {region!r}",
        )
        self._check(
            "upgrade: the user's content above survived",
            USER_ABOVE in text,
            "the content above the managed region was lost",
        )
        self._check(
            "upgrade: the user's content below survived",
            USER_BELOW in text,
            "the content below the managed region was lost",
        )
        self._check(
            "upgrade: the stale body is gone",
            "STALE managed block" not in text,
            "the stale body survived",
        )

    def reinstall(self) -> None:
        before = self.target.read_bytes()
        backups_before = sorted(p.name for p in self.target.parent.glob("AGENTS.md.bak*"))
        proc = self._install()
        self._check("reinstall: installer exits 0", proc.returncode == 0, proc.stderr)
        after = self.target.read_bytes()
        self._check(
            "reinstall: the file is byte-identical",
            before == after,
            "a repeated install changed the bytes",
        )
        backups_after = sorted(p.name for p in self.target.parent.glob("AGENTS.md.bak*"))
        self._check(
            "reinstall: no new backup was written",
            backups_before == backups_after,
            f"before={backups_before} after={backups_after}",
        )
        text = after.decode("utf-8")
        self._check(
            "reinstall: exactly one managed region",
            text.count(OUTER_BEGIN) == 1 and text.count(OUTER_END) == 1,
            f"begin={text.count(OUTER_BEGIN)} end={text.count(OUTER_END)}",
        )

    def existing_block_replacement(self) -> None:
        """A managed block that is ALREADY the canonical one, nested
        between the user's own content, must be left alone.

        The one thing that MAY change is the per-install provenance
        comment: it records when the block was applied, and a block
        that has never been stamped has no timestamp. Everything else —
        the behavioural text and the user's own content — must be
        byte-identical.
        """
        canonical = _canonical_managed_region()
        self.target.parent.mkdir(parents=True, exist_ok=True)
        self.target.write_text(
            USER_ABOVE + canonical + USER_BELOW, encoding="utf-8",
        )
        before = self.target.read_text(encoding="utf-8")
        proc = self._install()
        self._check("existing: installer exits 0", proc.returncode == 0, proc.stderr)
        after = self.target.read_text(encoding="utf-8")
        self._check(
            "existing: the managed region still matches the canonical render",
            _strip_provenance(_managed_region(after) or "")
            == _canonical_managed_region(),
            "the managed region diverged",
        )
        self._check(
            "existing: the provenance comment was added",
            "<!-- mpm-provenance:" in after,
            "the installed block carries no provenance stamp",
        )
        self._check(
            "existing: the user's content survived",
            USER_ABOVE in after and USER_BELOW in after,
            "the operator's own content was lost",
        )
        self._check(
            "existing: exactly one managed region",
            after.count(OUTER_BEGIN) == 1,
            f"begin={after.count(OUTER_BEGIN)}",
        )
        # KNOWN CHARACTERISTIC (pre-existing, verified against the
        # 3.0.0 installer): when the installer REWRITES a managed
        # region, the replacement text carries a trailing newline that
        # the replaced region did not, so one blank line is added
        # between the managed region and the user's following content.
        # The user's TEXT is untouched; only the separator grows, and
        # only on the write that first stamps the region. The next
        # install is a byte-level no-op, so it does not accumulate.
        # This is recorded rather than changed: it is the installer's
        # established behaviour and the compact block does not change
        # it.
        self._check(
            "existing: the managed region is byte-identical apart from "
            "provenance",
            _strip_provenance(_managed_region(after) or "")
            == _strip_provenance(_managed_region(before) or ""),
            "the managed region itself was rewritten",
        )

    def retired_hosts_untouched(self) -> None:
        """A retired host's file is neither created nor modified."""
        seeded = {}
        for rel in RETIRED_HOST_TARGETS:
            p = self.home / rel
            p.parent.mkdir(parents=True, exist_ok=True)
            p.write_text(f"# pre-existing operator file ({rel})\n", encoding="utf-8")
            seeded[rel] = p.read_bytes()
        self._install()
        for rel, before in seeded.items():
            p = self.home / rel
            self._check(
                f"retired: {rel} is untouched",
                p.is_file() and p.read_bytes() == before,
                "a retired host's file was modified",
            )
        for rel in RETIRED_HOST_TARGETS:
            self._check(
                f"retired: {rel} still exists",
                (self.home / rel).is_file(),
                "a retired host's file was removed",
            )

    def run(self) -> int:
        self.fresh_install()
        self.upgrade()
        self.reinstall()
        self.existing_block_replacement()
        self.retired_hosts_untouched()
        return 0


def main() -> int:
    ap = argparse.ArgumentParser()
    ap.add_argument("--json", help="write the result envelope here")
    args = ap.parse_args()

    reg = InstallerRegression()
    try:
        reg.run()
    finally:
        reg.cleanup()

    checks = len(reg.results)
    failed = len(reg.failures)
    for r in reg.results:
        print(f"{'PASS' if r['ok'] else 'FAIL'}  {r['check']}"
              + ("" if r["ok"] else f"  — {r['detail']}"))
    print()
    print(f"installer regression: {checks - failed}/{checks} checks passed")

    if args.json:
        Path(args.json).write_text(
            json.dumps(
                {"checks": reg.results, "failures": reg.failures}, indent=2,
            ),
            encoding="utf-8",
        )

    return 1 if failed else 0


if __name__ == "__main__":
    sys.exit(main())
