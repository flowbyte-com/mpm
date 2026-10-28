"""Makefile target reader.

Documentation tells readers to run `make <target>`. A target that no longer
exists is an objective defect — the instruction cannot be followed — and it is
one of the quietest ways documentation rots, because the reader blames
themselves before they blame the docs.
"""

from __future__ import annotations

import re
from pathlib import Path

_TARGET_RE = re.compile(r"^([A-Za-z0-9_][A-Za-z0-9_.-]*)\s*:(?!=)", re.M)


def targets(makefile: Path) -> set[str]:
    """Every target the Makefile declares.

    Deliberately does not resolve includes or expand variables: a doc that
    names a target reachable *only* through an include is unusual enough that
    a false positive is cheaper than the machinery to avoid it — and the
    exemption list exists for exactly that case.
    """
    try:
        text = makefile.read_text(encoding="utf-8")
    except OSError:
        return set()
    return {m.group(1) for m in _TARGET_RE.finditer(text) if not m.group(1).startswith(".")}


def is_phony_declared(makefile: Path) -> set[str]:
    try:
        text = makefile.read_text(encoding="utf-8")
    except OSError:
        return set()
    declared: set[str] = set()
    for line in text.splitlines():
        if line.startswith(".PHONY:"):
            declared.update(line.split(":", 1)[1].split())
    return declared
