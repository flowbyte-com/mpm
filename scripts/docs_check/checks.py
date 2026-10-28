"""The individual checks.

Each check returns findings. A finding has a severity:

  ``error``  — an objective defect. CI fails.
  ``review`` — something a human should look at. CI does not fail.

The split is the whole design. Anything that requires judging intent is a
``review``, because a gate that fails on judgement calls is a gate that gets
switched off, and a switched-off gate catches nothing.
"""

from __future__ import annotations

import json
import re
import subprocess
import sys
from dataclasses import dataclass, field
from pathlib import Path

from . import exemptions, makefile, registry

# ── Findings ─────────────────────────────────────────────────────────────────


@dataclass
class Finding:
    check: str
    severity: str  # "error" | "review"
    message: str
    path: str = ""
    line: int = 0

    def render(self) -> str:
        loc = f"{self.path}:{self.line}" if self.line else self.path
        tag = "ERROR" if self.severity == "error" else "review"
        return f"  [{tag}] {loc}: {self.message}" if loc else f"  [{tag}] {self.message}"


@dataclass
class Report:
    findings: list[Finding] = field(default_factory=list)
    checks_run: int = 0
    notes: list[str] = field(default_factory=list)

    @property
    def errors(self) -> list[Finding]:
        return [f for f in self.findings if f.severity == "error"]

    @property
    def reviews(self) -> list[Finding]:
        return [f for f in self.findings if f.severity == "review"]

    def add(self, finding: Finding) -> None:
        self.findings.append(finding)

    def ok(self) -> bool:
        return not self.errors

    def to_json(self) -> str:
        return json.dumps(
            {
                "ok": self.ok(),
                "checks_run": self.checks_run,
                "error_count": len(self.errors),
                "review_count": len(self.reviews),
                "findings": [
                    {
                        "check": f.check,
                        "severity": f.severity,
                        "path": f.path,
                        "line": f.line,
                        "message": f.message,
                    }
                    for f in self.findings
                ],
            },
            indent=2,
        )


# ── Shared helpers ───────────────────────────────────────────────────────────

FENCE_RE = re.compile(r"```.*?```", re.S)
LINK_RE = re.compile(r"(?<!!)\[[^\]]*\]\(([^)\s]+)(?:\s+\"[^\"]*\")?\)")
HEADING_RE = re.compile(r"^#{1,6}\s+(.+?)\s*$", re.M)
ANCHOR_DEF_RE = re.compile(r'<a\s+(?:name|id)="([^"]+)"')

# A line that already tells the reader the thing is gone. Suppresses the
# action-name check on that line, because naming a removed action in a
# retirement note is correct documentation, not a defect.
SUPERSEDED_LINE_MARKERS: tuple[str, ...] = (
    "refused",
    "operator-only",
    "superseded",
    "no longer",
    "not supported",
    "retired",
    "removed",
    "does not exist",
    "do not reintroduce",
    "earlier revision",
    "never existed",
    "~~",
)


def markdown_files(root: Path) -> list[Path]:
    """Every Markdown file that speaks about the present.

    Verbatim snapshots are excluded wholesale: they are fixtures whose content
    is frozen on purpose, so neither a dead link nor a retired adapter name
    inside one is a defect. See ``exemptions.FROZEN_SNAPSHOT_PREFIXES``.
    """
    out = []
    for path in root.rglob("*.md"):
        if ".git" in path.parts or "node_modules" in path.parts:
            continue
        relpath = rel(root, path)
        if any(relpath.startswith(p) for p in exemptions.FROZEN_SNAPSHOT_PREFIXES):
            continue
        out.append(path)
    return sorted(out)


def read(path: Path) -> str:
    return path.read_text(encoding="utf-8", errors="replace")


def strip_fences(text: str) -> str:
    """Blank out fenced code blocks while preserving line numbers."""
    return FENCE_RE.sub(lambda m: "\n" * m.group(0).count("\n"), text)


def github_slug(heading: str) -> str:
    """Approximate GitHub's heading anchor rule.

    Lowercase, drop everything that is not a word character / space / hyphen,
    then turn spaces into hyphens. Backticks and punctuation inside headings
    are removed, which matches GitHub's behaviour for the headings this
    repository actually uses.
    """
    slug = heading.strip().lower()
    slug = re.sub(r"[^\w\s-]", "", slug, flags=re.UNICODE)
    return slug.replace(" ", "-")


def rel(root: Path, path: Path) -> str:
    try:
        return str(path.relative_to(root))
    except ValueError:  # pragma: no cover
        return str(path)


def is_historical_reference_path(relpath: str) -> bool:
    return any(relpath.startswith(p) for p in exemptions.HISTORICAL_REFERENCE_PREFIXES)


# ── Check 1: relative links resolve ──────────────────────────────────────────


def check_links(root: Path, report: Report) -> None:
    report.checks_run += 1
    for path in markdown_files(root):
        text = strip_fences(read(path))
        for match in LINK_RE.finditer(text):
            target = match.group(1)
            if target.startswith(("http://", "https://", "mailto:", "#", "tel:")):
                continue
            if "#" in target:
                target = target.split("#", 1)[0]
            target = target.split("?", 1)[0]
            if not target:
                continue
            if not (path.parent / target).resolve().exists():
                report.add(
                    Finding(
                        check="links",
                        severity="error",
                        message=f"relative link does not resolve: {match.group(1)}",
                        path=rel(root, path),
                        line=text[: match.start()].count("\n") + 1,
                    )
                )


# ── Check 2: anchors resolve ─────────────────────────────────────────────────


def check_anchors(root: Path, report: Report) -> None:
    report.checks_run += 1
    for path in markdown_files(root):
        text = strip_fences(read(path))
        for match in re.finditer(r"\]\(([^)\s]*#[^)\s]+)\)", text):
            target = match.group(1)
            if target.startswith(("http", "mailto")):
                continue
            file_part, anchor = target.split("#", 1)
            destination = (path.parent / file_part).resolve() if file_part else path
            if not destination.exists():
                continue  # reported by check_links
            dest_text = read(destination)
            slugs = {github_slug(h) for h in HEADING_RE.findall(dest_text)}
            slugs |= set(ANCHOR_DEF_RE.findall(dest_text))
            if anchor.lower() not in slugs:
                report.add(
                    Finding(
                        check="anchors",
                        severity="error",
                        message=f"anchor does not exist in target: #{anchor}",
                        path=rel(root, path),
                        line=text[: match.start()].count("\n") + 1,
                    )
                )


# ── Check 3: backticked repo paths that no longer exist ──────────────────────

_PATH_TOKEN_RE = re.compile(
    r"`(docs/[A-Za-z0-9._/-]+\.md|agent_installation/[A-Za-z0-9._/-]+\.md"
    r"|scripts/[A-Za-z0-9._/-]+\.(?:py|sh)|cmd/[A-Za-z0-9._/-]+\.go"
    r"|internal/[A-Za-z0-9._/-]+\.go)`"
)


def check_missing_paths(root: Path, report: Report) -> None:
    report.checks_run += 1
    exempt = {e.path for e in exemptions.MISSING_PATH_EXEMPTIONS}
    # Pre-computed tail index: docs frequently abbreviate a path relative to
    # the subdirectory they describe ("scripts/render_managed_blocks.py" for
    # agent_installation/scripts/render_managed_blocks.py). A unique suffix
    # match is a resolution, not a guess.
    all_paths = [
        p.relative_to(root).as_posix()
        for p in root.rglob("*")
        if p.is_file() and ".git" not in p.parts and "node_modules" not in p.parts
    ]
    for path in markdown_files(root):
        relpath = rel(root, path)
        historical = is_historical_reference_path(relpath)
        text = read(path)
        for match in _PATH_TOKEN_RE.finditer(text):
            token = match.group(1)
            if token in exempt:
                continue
            if (root / token).exists():
                continue
            if (path.parent / token).exists():
                continue
            suffix = "/" + token.lstrip("./")
            if any(known.endswith(suffix) for known in all_paths):
                continue
            if historical:
                # A historical record describes a past layout; requiring its
                # paths to still exist would falsify it.
                continue
            report.add(
                Finding(
                    check="missing-paths",
                    severity="error",
                    message=f"references a path that does not exist: {token}",
                    path=relpath,
                    line=text[: match.start()].count("\n") + 1,
                )
            )


# ── Check 4: tool names in invocation position ───────────────────────────────

_CALL_RE = re.compile(r"mpm call\s+(mpm_[a-z0-9_]+)")


def check_tool_names(root: Path, surface: registry.ToolSurface, report: Report) -> None:
    report.checks_run += 1
    exempt = {e.token for e in exemptions.TOKEN_EXEMPTIONS}
    for path in markdown_files(root):
        relpath = rel(root, path)
        if is_historical_reference_path(relpath):
            # Historical documents describe a past tool set on purpose.
            continue
        text = strip_fences(read(path))
        for match in _CALL_RE.finditer(text):
            name = match.group(1)
            if name in surface.names() or name in exempt:
                continue
            line_no = text[: match.start()].count("\n") + 1
            line = text.splitlines()[line_no - 1] if line_no - 1 < len(text.splitlines()) else ""
            if any(marker in line.lower() for marker in SUPERSEDED_LINE_MARKERS):
                continue
            report.add(
                Finding(
                    check="tool-names",
                    severity="error",
                    message=f"`mpm call {name}` — no such tool in the registry",
                    path=relpath,
                    line=line_no,
                )
            )


# ── Check 5: action names against the registered enum ───────────────────────

_ACTION_RE = re.compile(r"\b(mpm_[a-z0-9_]+)\s+action\s*[=`]\s*([a-z0-9_]+)")


def check_tool_actions(root: Path, surface: registry.ToolSurface, report: Report) -> None:
    report.checks_run += 1
    for path in markdown_files(root):
        relpath = rel(root, path)
        if is_historical_reference_path(relpath):
            continue
        text = strip_fences(read(path))
        lines = text.splitlines()
        for match in _ACTION_RE.finditer(text):
            name, action = match.group(1), match.group(2)
            if name not in surface.names():
                continue  # check_tool_names owns unknown tools
            if surface.has_action(name, action):
                continue
            line_no = text[: match.start()].count("\n") + 1
            line = lines[line_no - 1] if line_no - 1 < len(lines) else ""
            if any(marker in line.lower() for marker in SUPERSEDED_LINE_MARKERS):
                continue
            valid = sorted(surface.actions_for(name))
            report.add(
                Finding(
                    check="tool-actions",
                    severity="error",
                    message=(
                        f"`{name} action={action}` — not a registered action. "
                        f"Valid: {', '.join(valid) if valid else '(none — this tool takes no action)'}"
                    ),
                    path=relpath,
                    line=line_no,
                )
            )


# ── Check 6: `make <target>` references exist ───────────────────────────────

# Only a *command* counts, not English prose that happens to use the verb
# "make". The rule is deliberately strict: the whole line must be a make
# invocation, optionally preceded by a shell prompt and optionally followed by
# a comment. `make every build look dirty` is prose; `make test-race` is an
# instruction. Anything looser produced a dozen false positives on the first
# run, and a gate that cries wolf gets switched off.
_MAKE_LINE_RE = re.compile(r"^\s*(?:\$ )?make\s+([a-z][a-z0-9-]*)\s*(?:#.*)?$")
_FENCE_BLOCK_RE = re.compile(r"```[^\n]*\n(.*?)```", re.S)
_INLINE_CODE_RE = re.compile(r"`([^`\n]+)`")


def _code_lines(text: str) -> list[tuple[int, str]]:
    """Yield (line_number, content) for every line inside a code context.

    Fenced blocks and inline spans are scanned separately from prose, because
    a command and a sentence about a command are not the same claim.
    """
    out: list[tuple[int, str]] = []
    for block in _FENCE_BLOCK_RE.finditer(text):
        start_line = text[: block.start()].count("\n") + 2
        for offset, line in enumerate(block.group(1).splitlines()):
            out.append((start_line + offset, line))
    for span in _INLINE_CODE_RE.finditer(text):
        out.append((text[: span.start()].count("\n") + 1, span.group(1)))
    return out


def check_make_targets(root: Path, report: Report) -> None:
    report.checks_run += 1
    declared = makefile.targets(root / "Makefile")
    if not declared:
        report.add(
            Finding(
                check="make-targets",
                severity="error",
                message="could not parse any Makefile targets — this check would pass vacuously",
                path="Makefile",
            )
        )
        return
    for path in markdown_files(root):
        relpath = rel(root, path)
        if is_historical_reference_path(relpath):
            continue
        text = read(path)
        for line_no, content in _code_lines(text):
            match = _MAKE_LINE_RE.match(content)
            if not match:
                continue
            target = match.group(1)
            if target in declared:
                continue
            report.add(
                Finding(
                    check="make-targets",
                    severity="error",
                    message=f"`make {target}` — no such target in the Makefile",
                    path=relpath,
                    line=line_no,
                )
            )


# ── Check 7: build-path claims ──────────────────────────────────────────────

# `bin/mpm` is correct only in the install prefix spelling (`~/.mpm/bin/mpm`)
# or the build dir spelling (`.build/bin/mpm`).
_WRONG_BIN_RE = re.compile(r"(?<![.\w~/])bin/(mpm|mpm-mcp|mpm-scheduler|mpm-critic|mpm-telemetry|mpm-lint)\b")


def check_build_paths(root: Path, report: Report) -> None:
    report.checks_run += 1
    for path in markdown_files(root):
        relpath = rel(root, path)
        if is_historical_reference_path(relpath):
            continue
        text = strip_fences(read(path))
        for match in _WRONG_BIN_RE.finditer(text):
            line_no = text[: match.start()].count("\n") + 1
            line = text.splitlines()[line_no - 1] if line_no - 1 < len(text.splitlines()) else ""
            if ".build/bin" in line or "~/.mpm/bin" in line or "$HOME/.mpm/bin" in line:
                continue
            report.add(
                Finding(
                    check="build-paths",
                    severity="error",
                    message=(
                        f"`bin/{match.group(1)}` — `make build` writes "
                        f"{exemptions.BUILD_DIR}/; only `make install` writes "
                        f"{exemptions.INSTALL_PREFIX}/"
                    ),
                    path=relpath,
                    line=line_no,
                )
            )


# ── Check 8: managed-block parity ───────────────────────────────────────────


def check_managed_block(root: Path, report: Report) -> None:
    report.checks_run += 1
    renderer = root / exemptions.RENDERER_REL
    if not renderer.exists():
        report.add(
            Finding(
                check="managed-block",
                severity="error",
                message=f"renderer missing: {exemptions.RENDERER_REL}",
                path=exemptions.RENDERER_REL,
            )
        )
        return
    try:
        proc = subprocess.run(
            [sys.executable, str(renderer), "--check"],
            cwd=str(root / "agent_installation"),
            capture_output=True,
            text=True,
            timeout=120,
        )
    except subprocess.TimeoutExpired:  # pragma: no cover
        report.add(
            Finding(
                check="managed-block",
                severity="error",
                message="renderer --check timed out",
                path=exemptions.RENDERER_REL,
            )
        )
        return
    if proc.returncode != 0:
        detail = (proc.stderr or proc.stdout).strip().splitlines()
        report.add(
            Finding(
                check="managed-block",
                severity="error",
                message=(
                    "rendered managed block does not match the canonical source: "
                    + (detail[-1] if detail else "renderer exited non-zero")
                    + " — run `python3 agent_installation/scripts/render_managed_blocks.py`"
                ),
                path=exemptions.RENDERER_REL,
            )
        )


# ── Check 9: library status labels + index completeness ─────────────────────

_LIBRARY_STATUS_RE = re.compile(r">\s*\*\*Library status:\s*(.+?)\.\*\*")


def check_library(root: Path, report: Report) -> None:
    report.checks_run += 1
    library = root / "docs" / "library"
    if not library.is_dir():
        return
    index_path = library / "README.md"
    index_text = read(index_path) if index_path.exists() else ""
    for path in sorted(library.glob("*.md")):
        if path.name == "README.md":
            continue
        text = read(path)
        match = _LIBRARY_STATUS_RE.search(text)
        if not match:
            report.add(
                Finding(
                    check="library-status",
                    severity="error",
                    message=(
                        "library entry has no `> **Library status: …**` header. "
                        "A reference document without a status label is "
                        "indistinguishable from a claim about current behaviour."
                    ),
                    path=rel(root, path),
                    line=1,
                )
            )
            continue
        if f"({path.name})" not in index_text:
            report.add(
                Finding(
                    check="library-status",
                    severity="error",
                    message=(
                        "library entry is not listed in docs/library/README.md — "
                        "the index is the only place a reader can see the whole "
                        "set and its labels"
                    ),
                    path=rel(root, path),
                )
            )


# ── Check 10: retired hosts ─────────────────────────────────────────────────

# Two different questions, deliberately split by how objective the answer is.
#
#   * A *path* to a retired adapter directory is objective: the directory is
#     gone, so the instruction cannot be followed. Error.
#   * A *brand mention* is a judgement call about whether the sentence presents
#     the host as supported. Review, and only in the documents that speak for
#     the current product — a dated report describing 2026-09 is a record, not
#     a claim about today.
_RETIRED_ADAPTER_PATHS: tuple[str, ...] = (
    "agent_installation/mpm-claude-code",
    "agent_installation/mpm-hermes",
    "agent_installation/mpm-pi",
    "agent_installation/mpm-memory-openclaw",
    "agent_installation/pi-mpm",
    "agent_installation/mpm-auto-route",
)


def check_retired_adapter_paths(root: Path, report: Report) -> None:
    report.checks_run += 1
    for path in markdown_files(root):
        relpath = rel(root, path)
        if is_historical_reference_path(relpath):
            continue
        text = read(path)
        lines = text.splitlines()
        for line_no, line in enumerate(lines, start=1):
            for adapter in _RETIRED_ADAPTER_PATHS:
                if adapter not in line:
                    continue
                window = " ".join(lines[max(0, line_no - 2): line_no + 1]).lower()
                if any(m in window for m in exemptions.RETIREMENT_MARKERS):
                    continue
                report.add(
                    Finding(
                        check="retired-adapter-paths",
                        severity="error",
                        message=(
                            f"references `{adapter}` as if it existed. The adapter "
                            "was retired on 2026-10-27 and its directory is gone."
                        ),
                        path=relpath,
                        line=line_no,
                    )
                )


def check_retired_host_mentions(root: Path, report: Report) -> None:
    """Review-only. Brand mentions in the documents that speak for today."""
    report.checks_run += 1
    for relpath in exemptions.CURRENT_OPERATIONAL_DOCS:
        path = root / relpath
        if not path.exists():
            continue
        text = strip_fences(read(path))
        lines = text.splitlines()
        for line_no, line in enumerate(lines, start=1):
            lowered = line.lower()
            hit = next((h for h in exemptions.RETIRED_HOST_BRANDS if h in lowered), None)
            if not hit:
                continue
            window = " ".join(lines[max(0, line_no - 2): line_no + 1]).lower()
            if any(m in window for m in exemptions.RETIREMENT_MARKERS):
                continue
            report.add(
                Finding(
                    check="retired-host-mentions",
                    severity="review",
                    message=(
                        f"mentions '{hit}' in a current document. If it is still "
                        "presented as a supported integration, it is wrong: "
                        "OpenCode is the only supported host."
                    ),
                    path=relpath,
                    line=line_no,
                )
            )


# ── Check 11: stale-status phrases (review) ─────────────────────────────────


def _blank_quoted(text: str) -> str:
    """Replace the contents of code spans and double-quoted strings with
    spaces, preserving every offset so line numbers still line up.

    A document that *names* a stale phrase in order to explain it — this file
    does exactly that — is not making a claim about the system.
    """
    def blank(match: re.Match) -> str:
        return match.group(0)[0] + " " * (len(match.group(0)) - 2) + match.group(0)[-1]

    text = re.sub(r"`[^`\n]*`", blank, text)
    text = re.sub(r'"[^"\n]*"', blank, text)
    return text


def check_stale_status(root: Path, report: Report) -> None:
    report.checks_run += 1
    for path in markdown_files(root):
        relpath = rel(root, path)
        if is_historical_reference_path(relpath):
            continue
        text = _blank_quoted(strip_fences(read(path)))
        lowered = text.lower()
        for phrase in exemptions.STALE_STATUS_PHRASES:
            start = 0
            while True:
                idx = lowered.find(phrase, start)
                if idx < 0:
                    break
                start = idx + 1
                line_no = text[:idx].count("\n") + 1
                line = text.splitlines()[line_no - 1] if line_no - 1 < len(text.splitlines()) else ""
                if any(m in line.lower() for m in exemptions.RETIREMENT_MARKERS):
                    continue
                report.add(
                    Finding(
                        check="stale-status",
                        severity="review",
                        message=(
                            f"states '{phrase}' in a current document — is this "
                            "still pending, or is the sentence stale?"
                        ),
                        path=relpath,
                        line=line_no,
                    )
                )


# ── Check 12: documentation-impact dependency map (review) ──────────────────


def _git_changed(root: Path, base: str) -> tuple[set[str], str]:
    def run(args: list[str]) -> tuple[int, str]:
        try:
            proc = subprocess.run(
                ["git", *args], cwd=str(root), capture_output=True, text=True, timeout=60
            )
        except (OSError, subprocess.TimeoutExpired):  # pragma: no cover
            return 1, ""
        return proc.returncode, proc.stdout

    code, out = run(["diff", "--name-only", f"{base}...HEAD"])
    if code != 0:
        code, out = run(["diff", "--name-only", base])
    if code != 0:
        return set(), base
    _, log = run(["log", "--format=%B%n===COMMIT===", f"{base}..HEAD"])
    return set(out.split()), log


def _impact_justification(commit_log: str) -> tuple[str, str] | None:
    """Read a `Docs-Impact:` trailer from the commit range.

    Accepted forms:

        Docs-Impact: none — <reason>
        Docs-Impact: docs/SPEC.md, README.md

    The reason after ``none`` is mandatory. An empty justification is not a
    justification, and the gate says so rather than accepting the token.
    """
    for line in commit_log.splitlines():
        stripped = line.strip()
        if not stripped.lower().startswith("docs-impact:"):
            continue
        value = stripped.split(":", 1)[1].strip()
        if not value:
            continue
        if value.lower().startswith("none"):
            reason = re.sub(r"^none\s*[—:-]*\s*", "", value, flags=re.I).strip()
            if reason:
                return ("none", reason)
            continue
        return ("paths", value)
    return None


def check_docs_impact(root: Path, base: str, report: Report, changed_docs: set[str] | None = None) -> None:
    report.checks_run += 1
    changed, commit_log = _git_changed(root, base)
    if not changed:
        report.notes.append(
            f"docs-impact: could not diff against '{base}' (shallow clone or "
            "unknown ref) — dependency review skipped, not silently passed"
        )
        return

    justification = _impact_justification(commit_log)

    for dep in exemptions.DEPENDENCY_MAP:
        touched = {
            f
            for f in changed
            if any(f == c or f.startswith(c.rstrip("/") + "/") or f.startswith(c) for c in dep.code)
        }
        if not touched:
            continue
        if any(d in changed for d in dep.docs):
            continue
        if justification and justification[0] == "none":
            report.notes.append(
                f"docs-impact: {dep.why} — waived by trailer: {justification[1]}"
            )
            continue
        if justification and justification[0] == "paths":
            named = {p.strip() for p in justification[1].split(",")}
            if named & set(dep.docs):
                continue
        report.add(
            Finding(
                check="docs-impact",
                severity="review",
                message=(
                    f"{dep.why}. Changed: {', '.join(sorted(touched)[:4])}"
                    f"{' …' if len(touched) > 4 else ''}. "
                    f"Consider: {', '.join(dep.docs)}. "
                    "Discharge with a `Docs-Impact: none — <reason>` trailer if no "
                    "documentation change is warranted."
                ),
                path="",
            )
        )
