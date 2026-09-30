#!/usr/bin/env python3
r"""
managed_block_convergence.py — Shared, host-agnostic convergence engine
for MPM managed blocks in agent instruction files.

Why this module exists
----------------------

Every MPM persistent-instruction installer must obey ONE invariant:

    A host instruction file may contain at most one effective MPM
    behavioural contract after reconciliation.

Before this module, each installer recognized only its own marker pair.
A file carrying a *foreign* host's MPM section — or a section whose END
marker was never written — matched nothing, fell through to the
"no managed block present, append" branch, and ended up with two
behavioural contracts side by side. That is the defect this engine
exists to make impossible: it is the single place that decides what
counts as an MPM-owned region, and every installer delegates to it.

Marker vocabulary
-----------------

MPM writes two distinct marker FAMILIES, and conflating them is the
historical source of the duplication bug, so they are parsed separately.

  OUTER markers  — the installer's idempotency anchor. Two spellings
                   have shipped over time:

      `MPM-MANAGED SECTION[:<host-id>]`   hyphenated, optional suffix
      `MPM-MANAGED BLOCK[:<adapter-id>]`  hyphenated, optional suffix
      `MPM MANAGED BLOCK:<adapter-id>`    SPACED + suffixed; the original
                                          Hermes anchor, still accepted

  INNER markers  — the behavioural contract itself, always the
                   UNSUFFIXED spaced form:

      `MPM MANAGED BLOCK`

The suffix is what separates the two families. A spaced marker carrying
a `:<id>` suffix is a legacy outer anchor; a spaced marker with no
suffix is the inner contract. Treating them as one pattern is how a
legacy Hermes file could swallow the contract nested inside it.

Convergence rules
-----------------

Given the target file's current text and the complete replacement
section the installer wants to write:

  no MPM region at all                    -> `insert`    (section appended)
  one balanced region, bytes match        -> `no-op`     (nothing written)
  one balanced region, own host id        -> `replace`   (stale block refreshed)
  one balanced region, foreign host id    -> `migrate`   (adopted to this host)
  one balanced region, no host id         -> `migrate`
  several balanced regions                -> `converge`  (collapse to one;
                                                         never append beside)
  one unterminated region, repairable     -> `repair`    (see below)
  anything genuinely ambiguous            -> `refuse`    (caller must not write)

`repair` is granted only when the region is UNAMBIGUOUS: exactly one
outer BEGIN, no matching outer END, and exactly one balanced inner
`MPM MANAGED BLOCK` pair following it. Then the region provably ends at
the inner END marker and everything after it is user content. That is
deterministic migration without deleting user content, so it is allowed.

Everything else ambiguous — crossed markers, overlapping regions,
multiple inner blocks, an unterminated region with no inner pair to
bound it — is refused. The caller must surface the refusal rather than
write. A refusal is a far better outcome than a second contract.

User content
------------

Bytes outside every MPM-owned region are copied through byte-for-byte.
The engine never rewrites, reflows, or normalizes them, and it never
touches a file on a `refuse`.
"""

from __future__ import annotations

import re
from dataclasses import dataclass, field
from typing import Iterable, Sequence


# ---------------------------------------------------------------------------
# Marker families
# ---------------------------------------------------------------------------

# OUTER: hyphenated (SECTION or BLOCK) with an OPTIONAL `:<id>` suffix,
# OR the spaced form which is only ever an anchor when suffixed.
_OUTER_BODY = (
    r"MPM-MANAGED\s+(?:SECTION|BLOCK)(?::[^\n>]*)?"
    r"|MPM\s+MANAGED\s+BLOCK:[^\n>]*"
)
OUTER_BEGIN_RE = re.compile(r"<!--\s*BEGIN\s+(?:" + _OUTER_BODY + r")\s*-->")
OUTER_END_RE = re.compile(r"<!--\s*END\s+(?:" + _OUTER_BODY + r")\s*-->")

# The host/adapter id carried by an outer marker, if any. Captured from
# the trailing `:<id>` when present; empty string for the unsuffixed form.
_ID_TAIL_RE = re.compile(r":([A-Za-z0-9._-]+)\s*-->")

# INNER: the spaced, UNSUFFIXED behavioural contract. Never suffixed.
INNER_BEGIN_RE = re.compile(r"<!--\s*BEGIN\s+MPM\s+MANAGED\s+BLOCK\s*-->")
INNER_END_RE = re.compile(r"<!--\s*END\s+MPM\s+MANAGED\s+BLOCK\s*-->")


# ---------------------------------------------------------------------------
# Analysis result
# ---------------------------------------------------------------------------

# Region closure states.
BALANCED = "balanced"
UNTERMINATED = "unterminated"
ORPHAN_END = "orphan-end"

# Plan outcomes.
PLAN_INSERT = "insert"
PLAN_NOOP = "no-op"
PLAN_REPLACE = "replace"
PLAN_MIGRATE = "migrate"
PLAN_CONVERGE = "converge"
PLAN_REPAIR = "repair"
PLAN_REFUSE = "refuse"

#: Outcomes that write to the file.
WRITING_PLANS = frozenset({
    PLAN_INSERT, PLAN_REPLACE, PLAN_MIGRATE, PLAN_CONVERGE, PLAN_REPAIR,
})
#: Outcomes that leave the file byte-identical.
READONLY_PLANS = frozenset({PLAN_NOOP, PLAN_REFUSE})


@dataclass(frozen=True)
class Region:
    """One MPM-owned span of the target file.

    `start` is the index of the first character of the BEGIN marker's
    line; `end` is the index just past the newline that terminates the
    closing marker's line. Everything in ``text[start:end]`` is
    MPM-owned and may be replaced; everything outside is user content.
    """

    start: int
    end: int
    closure: str          # BALANCED | UNTERMINATED | ORPHAN_END
    host_id: str          # "" when the marker carries no `:<id>` suffix
    inner_pairs: int      # balanced inner MPM MANAGED BLOCK pairs inside

    def as_text(self, text: str) -> str:
        return text[self.start:self.end]


@dataclass
class Analysis:
    """Structural read of a target file, independent of any host.

    `problems` are BLOCKING: they mean the file's MPM territory cannot
    be determined from its markers, so a write could delete user
    content. Only these cause a refusal.

    `notes` are reportable but NOT blocking: the file carries an
    anomaly worth showing an operator, but no MPM-owned region is
    ambiguous because of it. A stray END marker inside a user's prose
    about markers is the canonical example — there is no BEGIN, so
    there is no region to misjudge, and inserting a correct section
    beside it deletes nothing. Locking such a file out of
    `make refresh-installed` would be a usability regression dressed up
    as caution, so these are surfaced in diagnostics and ignored by the
    convergence decision.
    """

    regions: list[Region] = field(default_factory=list)
    inner_pairs: list[tuple[int, int]] = field(default_factory=list)
    problems: list[str] = field(default_factory=list)
    notes: list[str] = field(default_factory=list)

    @property
    def region_count(self) -> int:
        return len(self.regions)

    @property
    def is_clean(self) -> bool:
        return not self.problems


@dataclass
class Plan:
    """The decision for one reconciliation, plus its justification."""

    action: str           # one of the PLAN_* constants
    text: str             # resulting file content (unchanged input on
                         # no-op and refuse)
    reason: str           # operator-facing explanation
    analysis: Analysis


class AmbiguousState(RuntimeError):
    """Raised by `require_single_region` for callers that want an
    exception instead of a `refuse` plan."""


# ---------------------------------------------------------------------------
# Parsing
# ---------------------------------------------------------------------------

def _marker_id(marker_text: str) -> str:
    """Extract the `:<id>` suffix from a marker string, or "" if none."""
    m = _ID_TAIL_RE.search(marker_text)
    return m.group(1) if m else ""


def _line_start(text: str, index: int) -> int:
    """Index of the start of the line containing `index`, but only if
    the rest of that line is blank.

    A managed marker is usually alone on its line, and expanding to the
    line boundary makes the region self-contained. But a marker can also
    share a line with user text — `…paragraph <!-- BEGIN MPM-MANAGED
    SECTION:x -->` — and treating the whole line as MPM-owned would make
    convergence delete the user's sentence along with the block. So the
    expansion is conditional: it happens only across whitespace.
    """
    nl = text.rfind("\n", 0, index)
    start = 0 if nl == -1 else nl + 1
    return start if not text[start:index].strip() else index


def _line_end(text: str, after: int) -> int:
    """Index just past the line containing position `after`, but only
    across trailing whitespace on that line.

    Symmetric to `_line_start`: the terminating newline is consumed (so
    a repaired region leaves no orphaned blank line behind), but any
    non-blank content following the marker on the same line belongs to
    the user and is left in place.

    `after` must be the offset immediately AFTER the marker, not its
    start — otherwise the marker's own `<!-- … -->` text reads as
    trailing user content and the newline is never consumed.
    """
    nl = text.find("\n", after)
    line_stop = len(text) if nl == -1 else nl
    if text[after:line_stop].strip():
        return after
    return line_stop if nl == -1 else nl + 1


def find_inner_pairs(text: str) -> list[tuple[int, int]]:
    """Return balanced (start, end) spans of the inner behavioural
    contract, in file order.

    The end offset is PAST the `END` marker, matching the outer-marker
    convention used everywhere else in this module. A span that stopped
    at the marker's first byte would leave the marker itself outside the
    contract it closes — which is exactly how a repaired section came to
    carry an orphaned `END MPM MANAGED BLOCK` after it.

    Uses a stack so that a nested or crossed inner pair is handled
    structurally rather than by a greedy first-match regex. An inner
    BEGIN with no matching END is NOT returned here — it is reported by
    `analyze` as a problem rather than silently dropped.
    """
    events: list[tuple[int, int, int]] = []
    for m in INNER_BEGIN_RE.finditer(text):
        events.append((m.start(), m.end(), 1))
    for m in INNER_END_RE.finditer(text):
        events.append((m.start(), m.end(), -1))
    events.sort()

    stack: list[int] = []
    pairs: list[tuple[int, int]] = []
    for pos, end, delta in events:
        if delta == 1:
            stack.append(pos)
        elif stack:
            pairs.append((stack.pop(), end))
    pairs.sort()
    return pairs


def analyze(text: str) -> Analysis:
    """Structurally read `text` and report every MPM-owned region.

    The analysis is deliberately conservative: anything it cannot
    reduce to a definite, provable span is recorded in `problems` rather
    than guessed at. Callers turn a non-empty `problems` list into a
    refusal.
    """
    result = Analysis()
    result.inner_pairs = find_inner_pairs(text)

    begins = [(m.start(), m.end(), _marker_id(m.group(0)))
              for m in OUTER_BEGIN_RE.finditer(text)]
    ends = [(m.start(), m.end(), _marker_id(m.group(0)))
            for m in OUTER_END_RE.finditer(text)]

    unpaired_inner = len(INNER_BEGIN_RE.findall(text)) - len(result.inner_pairs)

    if len(begins) == len(ends):
        for (b_start, b_end, b_id), (e_start, e_end, e_id) in zip(begins, ends):
            if e_start <= b_end:
                result.problems.append(
                    f"outer END marker at offset {e_start} precedes its BEGIN "
                    f"at offset {b_start}; markers are crossed"
                )
                continue
            if b_id != e_id:
                # A section closed by a DIFFERENT host's marker is not a
                # well-formed region — it is two anchors from two hosts
                # interleaved. Treating it as one span would delete
                # whatever sits between them on the assumption that MPM
                # owns it, which is exactly the guess this module exists
                # to refuse.
                result.problems.append(
                    f"outer BEGIN at offset {b_start} carries id {b_id or '<unsuffixed>'} "
                    f"but the END at offset {e_start} carries id "
                    f"{e_id or '<unsuffixed>'}; the region is opened by one host "
                    f"and closed by another"
                )
                continue
            start = _line_start(text, b_start)
            end = _line_end(text, e_end)
            result.regions.append(Region(
                start=start, end=end, closure=BALANCED, host_id=b_id,
                inner_pairs=sum(1 for s, e in result.inner_pairs if s >= b_start and e <= e_start),
            ))
        # Overlap check: a region fully containing another means the
        # markers are not a simple sequence, so no single replacement
        # span can be trusted.
        ordered = sorted(result.regions, key=lambda r: (r.start, r.end))
        for a, b in zip(ordered, ordered[1:]):
            if b.start < a.end:
                result.problems.append(
                    f"overlapping MPM-managed regions at offsets {a.start} and "
                    f"{b.start}; the file's marker structure is not a simple "
                    f"sequence"
                )
    elif begins:
        # More BEGINs than ENDs: the trailing BEGIN(s) are unterminated.
        for extra in begins[len(ends):]:
            result.problems.append(
                f"outer BEGIN marker at offset {extra[0]} has no matching END"
            )
        for (b_start, b_end, b_id), (e_start, e_end, _e_id) in zip(
            begins, ends
        ):
            start = _line_start(text, b_start)
            end = _line_end(text, e_end)
            result.regions.append(Region(
                start=start, end=end, closure=BALANCED, host_id=b_id,
                inner_pairs=sum(1 for s, e in result.inner_pairs if s >= b_start and e <= e_start),
            ))
    else:
        for e_start, _e_end, e_id in ends:
            # No BEGIN at all, so there is no region whose extent could
            # be misjudged. Reportable, not blocking — see Analysis.notes.
            result.notes.append(
                f"outer END marker at offset {e_start}"
                + (f" (id {e_id})" if e_id else "")
                + " has no matching BEGIN; left in place as user content"
            )

    if unpaired_inner:
        result.problems.append(
            f"{unpaired_inner} inner `MPM MANAGED BLOCK` "
            f"{'marker has' if unpaired_inner == 1 else 'markers have'} no "
            f"matching {'END' if unpaired_inner == 1 else 'ENDs'}; the "
            f"behavioural contract is truncated"
        )

    result.regions.sort(key=lambda r: r.start)
    return result


# ---------------------------------------------------------------------------
# Planning
# ---------------------------------------------------------------------------

def _unterminated_extent(text: str, analysis: Analysis) -> tuple[int, int] | None:
    """Resolve the end offset of an unterminated outer region, or None.

    An unterminated region is repairable only when exactly one balanced
    inner contract pair follows its BEGIN. That pair proves where the
    MPM-owned content stops; everything after it is user content. Any
    other shape (no inner pair, or more than one) is ambiguous, because
    the true end of MPM's territory cannot be proven from markers alone.
    """
    begins = [m for m in OUTER_BEGIN_RE.finditer(text)]
    if not begins:
        return None
    last_begin = begins[-1]
    candidates = [
        (s, e) for (s, e) in analysis.inner_pairs if s > last_begin.end()
    ]
    if len(candidates) != 1:
        return None
    start = _line_start(text, last_begin.start())
    end = _line_end(text, candidates[0][1])
    return start, end


def plan_convergence(
    text: str, section: str, own_host_ids: Iterable[str] = (),
) -> Plan:
    """Decide how to bring `text` to exactly one MPM behavioural contract.

    `section` is the COMPLETE replacement the installer wants to write,
    outer markers included. `own_host_ids` names the marker ids this
    host owns; a region carrying any other id is a foreign-host block
    and is migrated rather than treated as current.

    Never raises and never writes. The caller decides what to do with a
    `refuse`; it must not write one.
    """
    own = {h for h in own_host_ids if h}
    analysis = analyze(text)

    # --- Ambiguity gate -------------------------------------------------
    # An unterminated outer region is a known, repairable shape: it is
    # dropped from `problems` once we can prove its extent. Everything
    # else in `problems` is genuinely ambiguous.
    repair_extent = None
    if analysis.problems:
        unterminated_only = all(
            "has no matching END" in p or "truncated" in p for p in analysis.problems
        )
        if unterminated_only:
            repair_extent = _unterminated_extent(text, analysis)
            if repair_extent is not None:
                analysis.problems = []
        if analysis.problems:
            return Plan(
                action=PLAN_REFUSE, text=text,
                reason=(
                    "the file's MPM markers are ambiguous; refusing to modify "
                    "rather than risk deleting user content — " + "; ".join(
                        analysis.problems
                    )
                ),
                analysis=analysis,
            )

    # --- Repair an unterminated but unambiguous region -----------------
    if repair_extent is not None:
        start, end = repair_extent
        region = Region(
            start=start, end=end, closure=UNTERMINATED, host_id="",
            inner_pairs=1,
        )
        analysis.regions = [region]
        if text[start:end].rstrip() == section.rstrip():
            return Plan(PLAN_NOOP, text,
                        "managed section is current", analysis)
        new_text = _splice(text, analysis.regions, section)
        return Plan(
            PLAN_REPAIR, new_text,
            "repaired an unterminated MPM-managed section: the missing outer "
            "END marker was the only defect, the inner behavioural contract "
            "was balanced, and all surrounding user content is preserved",
            analysis,
        )

    # --- Balanced regions ----------------------------------------------
    regions = analysis.regions

    if not regions:
        # No outer marker at all. A bare, balanced inner contract with no
        # wrapper is still an effective MPM behavioural contract and must
        # not be left beside a newly written one.
        if len(analysis.inner_pairs) == 1:
            s, e = analysis.inner_pairs[0]
            start = _line_start(text, s)
            end = _line_end(text, e)
            region = Region(start=start, end=end, closure=BALANCED,
                            host_id="", inner_pairs=1)
            analysis.regions = [region]
            new_text = _splice(text, [region], section)
            return Plan(
                PLAN_MIGRATE, new_text,
                "migrated a bare MPM behavioural contract (present without its "
                "outer managed-section wrapper) into this host's wrapper",
                analysis,
            )
        sep = "" if text.endswith("\n") or text == "" else "\n"
        new_text = text + sep + "\n" + section
        return Plan(
            PLAN_INSERT, new_text,
            "no MPM-managed region present; inserted this host's managed "
            "section",
            analysis,
        )

    if len(regions) == 1:
        region = regions[0]
        current = region.as_text(text)
        if current.rstrip() == section.rstrip():
            return Plan(PLAN_NOOP, text,
                        "managed section is byte-identical to the canonical "
                        "render", analysis)
        foreign = bool(region.host_id) and region.host_id not in own
        return Plan(
            PLAN_MIGRATE if foreign else PLAN_REPLACE,
            _splice(text, regions, section),
            (
                f"migrated a foreign-host MPM managed section "
                f"(marker id {region.host_id!r}) to this host's rendering"
                if foreign else
                "replaced the stale managed section with the current canonical "
                "render"
            ),
            analysis,
        )

    # --- Several balanced regions: collapse deterministically ---------
    ids = ", ".join(sorted({r.host_id or "<unsuffixed>" for r in regions}))
    return Plan(
        PLAN_CONVERGE, _splice(text, regions, section),
        f"collapsed {len(regions)} MPM managed sections (marker ids: {ids}) "
        f"into a single one; the contract must appear at most once per file",
        analysis,
    )


def _splice(text: str, regions: Sequence[Region], section: str) -> str:
    """Replace every region with `section`, placed at the first region.

    Gaps between regions are copied through byte-for-byte, so all user
    content — including anything between two MPM sections — survives
    untouched. Nothing is reflowed or normalized.
    """
    ordered = sorted(regions, key=lambda r: r.start)
    parts: list[str] = []
    cursor = 0
    for index, region in enumerate(ordered):
        parts.append(text[cursor:region.start])
        if index == 0:
            parts.append(section)
        cursor = region.end
    parts.append(text[cursor:])
    return "".join(parts)


def remove_regions(text: str, regions: Sequence[Region]) -> str:
    """Delete every MPM-owned region, preserving all other bytes.

    Used by uninstall. Content between two regions is preserved exactly;
    only the region spans themselves are dropped.
    """
    ordered = sorted(regions, key=lambda r: r.start)
    parts: list[str] = []
    cursor = 0
    for region in ordered:
        parts.append(text[cursor:region.start])
        cursor = region.end
    parts.append(text[cursor:])
    return "".join(parts)


# ---------------------------------------------------------------------------
# Convenience wrappers
# ---------------------------------------------------------------------------

def converge(
    text: str, section: str, own_host_ids: Iterable[str] = (),
) -> Plan:
    """Alias for `plan_convergence`, kept for readable call sites."""
    return plan_convergence(text, section, own_host_ids)


def require_single_region(text: str) -> Region:
    """Return the file's single balanced MPM region, or raise.

    For callers (such as the diagnostic's currency comparison) that
    need exactly one region and have no meaningful action to take when
    that is not the case.
    """
    analysis = analyze(text)
    if analysis.problems or len(analysis.regions) != 1:
        raise AmbiguousState(
            f"expected exactly one balanced MPM-managed region, found "
            f"{len(analysis.regions)}"
            + (" with problems: " + "; ".join(analysis.problems)
               if analysis.problems else "")
        )
    return analysis.regions[0]


def repair_extent_or_none(text: str) -> tuple[int, int] | None:
    """Public probe: is this file's marker damage deterministically
    repairable, and if so over what span?

    The diagnostic asks this so it can distinguish "the installer will
    fix this for you" (WARN + a repair command) from "the installer will
    refuse; a human must look" (ERROR). Both callers go through the same
    predicate, so the diagnostic can never mis-describe what the
    installer will actually do.
    """
    analysis = analyze(text)
    if analysis.problems and not all(
        "has no matching END" in p or "truncated" in p for p in analysis.problems
    ):
        return None
    return _unterminated_extent(text, analysis)


def describe_markers(text: str) -> list[str]:
    """Operator-facing inventory of every MPM marker present in `text`.

    Used by diagnostics so a human can see, in one place, which marker
    forms a file actually carries — including foreign and malformed
    ones — instead of inferring state from a single PASS/FAIL verdict.
    """
    out: list[str] = []
    for m in OUTER_BEGIN_RE.finditer(text):
        ident = _marker_id(m.group(0))
        out.append(f"outer BEGIN id={ident or '<unsuffixed>'}")
    for m in OUTER_END_RE.finditer(text):
        ident = _marker_id(m.group(0))
        out.append(f"outer END id={ident or '<unsuffixed>'}")
    out.extend(f"inner contract BEGIN" for _ in INNER_BEGIN_RE.finditer(text))
    out.extend(f"inner contract END" for _ in INNER_END_RE.finditer(text))
    return out
