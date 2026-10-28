"""Entry point for the documentation freshness gate.

Usage (from anywhere inside the repository):

    python3 scripts/docs_check                 # run every check
    python3 scripts/docs_check --json          # machine-readable report
    python3 scripts/docs_check --base origin/main
    python3 scripts/docs_check --check links --check anchors

Exit status is 0 when there are no ``error`` findings, 1 otherwise. ``review``
findings never change the exit status.

The gate never opens a database, reads MPM state, contacts a service, or uses
credentials. It reads the working tree and, for the docs-impact check, `git
diff`/`git log` of the current branch.
"""

from __future__ import annotations

import argparse
import sys
from pathlib import Path

if __package__ in (None, ""):  # invoked as a script, not as a module
    sys.path.insert(0, str(Path(__file__).resolve().parent.parent))
    from docs_check import checks, registry  # type: ignore
else:
    from . import checks, registry

REPO_ROOT = Path(__file__).resolve().parents[2]

ALL_CHECKS = (
    "links",
    "anchors",
    "missing-paths",
    "tool-names",
    "tool-actions",
    "make-targets",
    "build-paths",
    "managed-block",
    "library-status",
    "retired-adapter-paths",
    "retired-host-mentions",
    "stale-status",
    "docs-impact",
)


def run(
    root: Path = REPO_ROOT,
    selected: set[str] | None = None,
    base: str = "origin/main",
) -> checks.Report:
    """Run the gate. Importable so the tests can drive it against fixtures."""
    chosen = selected or set(ALL_CHECKS)
    report = checks.Report()
    surface = registry.load(root)
    # The registry is only an authority for the two checks that consult it. On
    # a fixture that carries no Go source, a parse failure would otherwise be
    # reported as a defect of the fixture rather than of the check selection.
    if chosen & {"tool-names", "tool-actions"}:
        for error in surface.errors:
            report.add(
                checks.Finding(
                    check="registry",
                    severity="error",
                    message=f"registry parse problem: {error}",
                )
            )

    def want(name: str) -> bool:
        return name in chosen

    if want("links"):
        checks.check_links(root, report)
    if want("anchors"):
        checks.check_anchors(root, report)
    if want("missing-paths"):
        checks.check_missing_paths(root, report)
    if want("tool-names"):
        checks.check_tool_names(root, surface, report)
    if want("tool-actions"):
        checks.check_tool_actions(root, surface, report)
    if want("make-targets"):
        checks.check_make_targets(root, report)
    if want("build-paths"):
        checks.check_build_paths(root, report)
    if want("managed-block"):
        checks.check_managed_block(root, report)
    if want("library-status"):
        checks.check_library(root, report)
    if want("retired-adapter-paths"):
        checks.check_retired_adapter_paths(root, report)
    if want("retired-host-mentions"):
        checks.check_retired_host_mentions(root, report)
    if want("stale-status"):
        checks.check_stale_status(root, report)
    if want("docs-impact"):
        checks.check_docs_impact(root, base, report)
    return report


def main(argv: list[str] | None = None) -> int:
    parser = argparse.ArgumentParser(
        prog="docs-check",
        description="Validate MPM's documentation against the repository.",
    )
    parser.add_argument(
        "--check",
        action="append",
        choices=ALL_CHECKS,
        help="run only the named check(s); repeatable",
    )
    parser.add_argument(
        "--base",
        default="origin/main",
        help="git ref to diff against for the docs-impact review (default: origin/main)",
    )
    parser.add_argument("--json", action="store_true", help="emit a JSON report")
    parser.add_argument(
        "--quiet", action="store_true", help="only print findings, not the summary"
    )
    args = parser.parse_args(argv)

    selected = set(args.check) if args.check else None
    report = run(selected=selected, base=args.base)

    if args.json:
        print(report.to_json())
    elif not args.quiet:
        if report.errors:
            print(f"docs-check: {len(report.errors)} error(s)")
            for finding in report.errors:
                print(finding.render())
        if report.reviews:
            print(f"\ndocs-check: {len(report.reviews)} review item(s)")
            for finding in report.reviews:
                print(finding.render())
        for note in report.notes:
            print(f"  [note] {note}")
        if report.ok():
            print(
                f"\ndocs-check: OK — {report.checks_run} checks, "
                f"{len(report.reviews)} review item(s), 0 errors"
            )
        else:
            print(
                f"\ndocs-check: FAILED — {report.checks_run} checks, "
                f"{len(report.errors)} error(s), {len(report.reviews)} review item(s)"
            )
    return 0 if report.ok() else 1


if __name__ == "__main__":
    sys.exit(main())
