#!/usr/bin/env python3
"""
install_reviewer_agent.py — install the ISOLATED REVIEWER agent
definition for OpenCode.

WHY THIS EXISTS
---------------
A review subagent runs in a child session with the parent's tool
surface. On 2026-11-15 a review subagent wrote a memory into the
operator's LIVE MPM database through the `mpm_memory` tool, against
an explicit instruction not to touch it.

The fix is the PERMISSION layer: the reviewer agent definition denies
every action by default and re-opens exactly one — the host-controlled
sandboxed execution tool. A denied tool is removed from the reviewer's
tool list entirely. The definition is only an isolation if it is
actually INSTALLED, which is what this script does.

THIS INSTALL FAILS CLOSED
-------------------------
The install is a DEPLOYMENT step, and a deployment step that fails
open is worse than one that fails: it converts an error into a SILENT
DOWNGRADE of the guarantee — a reviewer that then runs with the
parent's tool surface, believing it is confined.

So this script:

  1. parses the canonical template's frontmatter and CHECKS ITS POLICY
     — the default deny, the one allowed tool, and every host-side
     capability that must be closed;
  2. refuses to install a template that does not satisfy that policy;
  3. writes the copy atomically, with restrictive permissions;
  4. verifies the INSTALLED copy byte-for-byte against the template;
  5. exits NON-ZERO, leaving the previous copy untouched, when any of
     that fails.

USAGE
-----
    python3 install_reviewer_agent.py            # install or report
    python3 install_reviewer_agent.py --force    # overwrite an existing file
    python3 install_reviewer_agent.py --dry-run  # report only
    python3 install_reviewer_agent.py --target DIR
"""

from __future__ import annotations

import argparse
import hashlib
import os
import shutil
import sys
from pathlib import Path

HERE = Path(__file__).resolve().parent
TEMPLATE = HERE.parent / "templates" / "agents" / "mpm-reviewer.md"
DEFAULT_TARGET_DIR = Path.home() / ".config" / "opencode" / "agents"
AGENT_NAME = "mpm-reviewer.md"

# The mode the installed definition carries. The definition decides what
# a reviewer may reach, so it is not world-readable and not writable by
# anyone but the operator.
FILE_MODE = 0o600
DIR_MODE = 0o700

# The host-side capabilities that must be CLOSED for a reviewer.
#
# Every one of them runs in the HOST's process, outside the sandbox:
# the sandbox confines code execution, and these are not code execution.
REQUIRED_DENIED = (
    "shell",
    "read",
    "write",
    "edit",
    "patch",
    "grep",
    "glob",
    "list",
    "external_directory",
    "execute",
    "browser",
    "skill",
    "webfetch",
    "websearch",
    "subagent",
    "question",
)

# The ONE tool a reviewer may use.
REQUIRED_ALLOWED = ("sandbox_exec",)


class InstallRefused(SystemExit):
    """The install was refused.  Carries the reason."""


# --------------------------------------------------------------------------
# The frontmatter
# --------------------------------------------------------------------------


def _split_frontmatter(text: str) -> str:
    """The YAML frontmatter of an agent definition."""
    if not text.startswith("---"):
        raise InstallRefused("the definition has no frontmatter")
    end = text.find("\n---", 3)
    if end < 0:
        raise InstallRefused("the definition's frontmatter is not closed")
    return text[3:end]


def _parse_with_pyyaml(frontmatter: str) -> list | None:
    """The permission rules, parsed with PyYAML when it is available."""
    try:
        import yaml  # type: ignore
    except Exception:
        return None
    try:
        data = yaml.safe_load(frontmatter)
    except Exception as exc:
        raise InstallRefused(f"the definition's frontmatter does not parse: {exc}")
    if not isinstance(data, dict):
        raise InstallRefused("the definition's frontmatter is not a mapping")
    rules = data.get("permissions")
    if not isinstance(rules, list):
        raise InstallRefused("the definition declares no `permissions` list")
    return rules


def _parse_builtin(frontmatter: str) -> list:
    """The permission rules, parsed by a strict reader of our own shape.

    The format is fixed and small: a `permissions:` key followed by list
    items of `action`, `resource` and `effect`.  Anything this reader
    does not understand is a REFUSAL, never a guess — an install that
    cannot read the definition must not install it.
    """
    lines = frontmatter.splitlines()
    rules: list[dict] = []
    inside = False
    current: dict | None = None
    for raw in lines:
        line = raw.rstrip()
        if not line.strip() or line.strip().startswith("#"):
            continue
        if not inside:
            if line.strip() == "permissions:":
                inside = True
            continue
        stripped = line.strip()
        if stripped.startswith("- "):
            if current is not None:
                rules.append(current)
            current = {}
            stripped = stripped[2:].strip()
            if not stripped:
                continue
        if current is None:
            raise InstallRefused(
                f"the definition's permissions are not in the expected shape: {line!r}"
            )
        if ":" not in stripped:
            raise InstallRefused(
                f"the definition's permissions are not in the expected shape: {line!r}"
            )
        key, _, value = stripped.partition(":")
        key = key.strip()
        value = value.strip()
        if value.startswith(("'", '"')) and value.endswith(("'", '"')) and len(value) >= 2:
            value = value[1:-1]
        current[key] = value
    if current is not None:
        rules.append(current)
    if not rules:
        raise InstallRefused("the definition declares no `permissions` list")
    return rules


def parse_permissions(text: str) -> list:
    """The definition's permission rules.

    PyYAML is used when it is available and the built-in reader always
    runs as well; when both can read the definition they must AGREE.
    A disagreement is a refusal: it means the definition is written in
    a shape the installer does not fully understand, and an installer
    that does not understand what it is installing must not install it.
    """
    frontmatter = _split_frontmatter(text)
    builtin = _parse_builtin(frontmatter)
    parsed = _parse_with_pyyaml(frontmatter)
    if parsed is None:
        return builtin
    normalised_builtin = sorted(
        (str(rule.get("action")), str(rule.get("resource")), str(rule.get("effect")))
        for rule in builtin
    )
    normalised_parsed = sorted(
        (str(rule.get("action")), str(rule.get("resource")), str(rule.get("effect")))
        for rule in parsed
    )
    if normalised_builtin != normalised_parsed:
        raise InstallRefused(
            "the definition's permissions read differently under the two "
            "readers; the installer does not understand what it would install"
        )
    return builtin


# --------------------------------------------------------------------------
# The policy
# --------------------------------------------------------------------------


def check_policy(rules: list) -> list:
    """The reasons the policy is NOT satisfied.  Empty means it is."""
    problems: list[str] = []
    seen = [
        (str(rule.get("action", "")), str(rule.get("resource", "")),
         str(rule.get("effect", "")))
        for rule in rules
    ]
    if not seen:
        return ["the definition declares no permission rules"]

    def allows(action: str) -> bool:
        return any(
            a == action and e == "allow" for (a, _r, e) in seen
        )

    def denies_all(action: str) -> bool:
        return any(a == action and r == "*" and e == "deny" for (a, r, e) in seen)

    # The default deny.
    if not denies_all("*"):
        problems.append(
            "the definition has no default deny (an `action: \"*\"`, "
            "`resource: \"*\"`, `effect: deny` rule)"
        )

    for action in REQUIRED_DENIED:
        if not denies_all(action):
            problems.append(f"the definition does not deny `{action}` for every resource")
        if allows(action):
            problems.append(f"the definition ALLOWS `{action}` somewhere")

    for action in REQUIRED_ALLOWED:
        if not any(a == action and r == "*" and e == "allow" for (a, r, e) in seen):
            problems.append(f"the definition does not allow `{action}`")
        if denies_all(action):
            problems.append(f"the definition denies `{action}` for every resource")

    # Every MPM tool the definition names must be denied, never allowed.
    for action in sorted({a for (a, _r, _e) in seen if a.startswith("mpm_")}):
        if not denies_all(action):
            problems.append(f"the definition does not deny the MPM tool `{action}`")
        if allows(action):
            problems.append(f"the definition ALLOWS the MPM tool `{action}`")

    return problems


def _digest(path: Path) -> str:
    return hashlib.sha256(path.read_bytes()).hexdigest()


# --------------------------------------------------------------------------
# The install
# --------------------------------------------------------------------------


def install(target_dir: Path, force: bool, dry_run: bool, template: Path) -> int:
    template = Path(template)
    if not template.exists():
        print(f"the canonical template is missing: {template}", file=sys.stderr)
        return 1

    template_text = template.read_text(encoding="utf-8")

    # 1. The template must satisfy the policy BEFORE it is installed.
    try:
        rules = parse_permissions(template_text)
    except InstallRefused as refusal:
        print(f"refusing to install: {refusal}", file=sys.stderr)
        return 1
    problems = check_policy(rules)
    if problems:
        print("refusing to install: the template does not satisfy the", file=sys.stderr)
        print("reviewer's isolation policy:", file=sys.stderr)
        for problem in problems:
            print(f"  - {problem}", file=sys.stderr)
        return 1

    target_dir = target_dir.expanduser()
    target = target_dir / AGENT_NAME

    if target.is_symlink():
        print(
            f"refusing to install: {target} is a symlink; an installer that "
            "follows a symlink can be made to write somewhere else",
            file=sys.stderr,
        )
        return 1
    if target.exists() and not force:
        print(
            f"{target} already exists; it was NOT overwritten.\n"
            "Re-run with --force to replace it.",
            file=sys.stderr,
        )
        return 1

    if dry_run:
        print(f"would install {template} -> {target}")
        print("the template satisfies the reviewer's isolation policy")
        return 0

    staging = target.with_suffix(target.suffix + ".new")
    try:
        target_dir.mkdir(parents=True, exist_ok=True)
        try:
            os.chmod(target_dir, DIR_MODE)
        except OSError:
            pass
        # Copy to a temporary name and rename, so a reader never sees a
        # half-written definition.
        shutil.copyfile(template, staging)
        os.chmod(staging, FILE_MODE)
        staging.replace(target)
        os.chmod(target, FILE_MODE)
    except OSError as exc:
        try:
            if staging.exists():
                staging.unlink()
        except OSError:
            pass
        print(f"could not install the reviewer agent: {exc}", file=sys.stderr)
        return 1

    # 4. Verify the INSTALLED copy, not the template.
    try:
        installed_digest = _digest(target)
        template_digest = _digest(template)
    except OSError as exc:
        print(
            f"the reviewer agent was written but could not be verified: {exc}",
            file=sys.stderr,
        )
        return 1
    if installed_digest != template_digest:
        print(
            f"the installed reviewer agent at {target} does not match the "
            "template; refusing to report success",
            file=sys.stderr,
        )
        return 1

    try:
        installed_rules = parse_permissions(target.read_text(encoding="utf-8"))
    except (InstallRefused, OSError) as exc:
        print(
            f"the installed reviewer agent at {target} could not be read back: {exc}",
            file=sys.stderr,
        )
        return 1
    installed_problems = check_policy(installed_rules)
    if installed_problems:
        print(
            f"the installed reviewer agent at {target} does not satisfy the "
            "isolation policy; refusing to report success",
            file=sys.stderr,
        )
        for problem in installed_problems:
            print(f"  - {problem}", file=sys.stderr)
        return 1

    print(f"installed the isolated reviewer agent: {target}")
    print(
        f"  sha256: {installed_digest}\n"
        "  the definition was read back, matched against the template and "
        "checked against the isolation policy."
    )
    print(
        "OpenCode reloads agents after a configuration change; run /reload "
        "if the host has not noticed yet.\n"
        'Launch it with:  subagent(agent="mpm-reviewer", ...)'
    )
    return 0


def main(argv: list[str] | None = None) -> int:
    parser = argparse.ArgumentParser(
        description="Install the isolated reviewer agent for OpenCode.",
    )
    parser.add_argument(
        "--target",
        type=Path,
        default=DEFAULT_TARGET_DIR,
        help="the agents directory to install into (default: %(default)s)",
    )
    parser.add_argument(
        "--force",
        action="store_true",
        help="overwrite an existing definition",
    )
    parser.add_argument(
        "--dry-run",
        action="store_true",
        help="report what would happen, and change nothing",
    )
    parser.add_argument(
        "--template",
        type=Path,
        default=TEMPLATE,
        help="the definition to install (default: the canonical template)",
    )
    args = parser.parse_args(argv)

    try:
        return install(args.target, args.force, args.dry_run, args.template)
    except InstallRefused as refusal:
        print(f"refusing to install: {refusal}", file=sys.stderr)
        return 1


if __name__ == "__main__":
    raise SystemExit(main())
