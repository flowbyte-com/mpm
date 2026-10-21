#!/usr/bin/env python3
"""Build provenance for installed MPM managed blocks.

Every installed managed behavioural block identifies:

  * the MPM product version that wrote it;
  * the exact source commit that version was built from;
  * the behavioural protocol version of the block itself;
  * the UTC moment the block was last applied or updated.

WHY THIS IS NOT PART OF THE CANONICAL SOURCE
=============================================

The canonical behavioural text lives in
MPM_AGENT_INTEGRATION_SNIPPETS.md and is rendered into per-host
snippets. That pipeline is byte-compared against the installed blocks
by four separate gates, so anything that changes per installation
(timestamps, per-build commit SHAs) must NOT be baked into the
canonical text or the checked-in snippets — it would make every
render drift.

So the provenance is injected at INSTALL time, into the installed
block only, immediately after the inner `<!-- BEGIN MPM MANAGED BLOCK -->`
marker. The behavioural text stays byte-identical to the canonical
render; the provenance is per-install metadata layered on top, and the
drift gates normalise it away on both sides of their comparison.

WHY THE TIMESTAMP IS NOT REFRESHED ON EVERY RUN
===============================================

An installer that mints a fresh timestamp on every run is never
idempotent: two runs a second apart produce different bytes, so the
reconcile pass would rewrite the file (and take a new backup) every
time it ran, for no change. The timestamp therefore records when the
block was last APPLIED OR UPDATED: it is preserved when the version,
the commit, the protocol version and the behavioural text are all
unchanged, and refreshed whenever any of them changes.

Note the precedent this follows: the OpenClaw installer already
preserves its `<!-- generated: ... -->` timestamp across refreshes for
exactly the same reason.

WHERE THE VERSION AND COMMIT COME FROM
======================================

From the environment:

    MPM_BUILD_VERSION   the product version
    MPM_BUILD_COMMIT    the full source commit SHA

install.sh exports both before it reconciles the managed blocks, so an
installed block names the exact build that produced it. An adapter
installer run on its own — outside install.sh, outside a build — may
not be able to establish either value, and then it says so
("unknown") rather than claiming a version it cannot establish. It
does not erase an established value either: when the behavioural text
is unchanged, the value the block already carries is kept.

THE PROTOCOL VERSION IS INDEPENDENT
===================================

`mpm_agent_integration_version` is the version of the BEHAVIOURAL
CONTRACT, not of the MPM product. It is declared by the canonical
source and it moves on its own schedule; the product version must
never be used in its place, and it must never be used to decide
whether the product version changed.
"""

from __future__ import annotations

import os
import re
from datetime import datetime, timezone
from pathlib import Path

# The inner marker of the canonical behavioural block. The provenance
# comment goes immediately after it.
INNER_BEGIN = "<!-- BEGIN MPM MANAGED BLOCK -->"

# The prefix of the provenance comment. It is deliberately distinct
# from the `<!-- generated: ... -->` line some installers already
# write, and from the `<!-- source: ... -->` line that is part of the
# canonical behavioural text.
PROVENANCE_PREFIX = "<!-- mpm-provenance:"

# The canonical source, and therefore the behavioural protocol version.
CANONICAL_SOURCE = Path(__file__).resolve().parent.parent / "MPM_AGENT_INTEGRATION_SNIPPETS.md"

# The placeholder used when a value cannot be established. It is a
# statement of absence, never a claim.
UNKNOWN = "unknown"

_PROVENANCE_RE = re.compile(r"<!-- mpm-provenance:[^\n]*?-->[ \t]*\n?")
_PROTOCOL_RE = re.compile(
    r"<!--\s*mpm_agent_integration_version:\s*([0-9][^\s]*)\s*-->",
)


# ---------------------------------------------------------------------------
# Value sources
# ---------------------------------------------------------------------------


def utc_now() -> str:
    """The current UTC moment, in the format the provenance uses."""
    return datetime.now(timezone.utc).strftime("%Y-%m-%dT%H:%M:%SZ")


def source_version() -> str:
    """The MPM product version, or "unknown" when it cannot be established."""
    value = os.environ.get("MPM_BUILD_VERSION", "").strip()
    return value or UNKNOWN


def source_commit() -> str:
    """The exact source commit SHA, or "unknown" when it cannot be established."""
    value = os.environ.get("MPM_BUILD_COMMIT", "").strip()
    return UNKNOWN if value in ("", "unknown") else value


def read_protocol_version(path: Path | str | None = None) -> str:
    """The behavioural protocol version declared by the canonical source.

    This version is INDEPENDENT of the MPM product version: it versions
    the behavioural contract, not the software. It moves when the
    contract changes, not when the product is released.
    """
    target = Path(path) if path is not None else CANONICAL_SOURCE
    try:
        text = target.read_text(encoding="utf-8")
    except OSError:
        return UNKNOWN
    match = _PROTOCOL_RE.search(text)
    return match.group(1) if match else UNKNOWN


# ---------------------------------------------------------------------------
# Rendering and parsing
# ---------------------------------------------------------------------------


def format_provenance(version: str, commit: str, protocol: str, applied_at: str) -> str:
    """Render the provenance comment (without its trailing newline)."""
    return (
        f"{PROVENANCE_PREFIX} version={version} commit={commit} "
        f"protocol={protocol} applied={applied_at} -->"
    )


def parse_provenance(text: str) -> dict[str, str] | None:
    """Parse the provenance comment out of `text`, or None when absent.

    Returns the fields as a dict: version, commit, protocol, applied.
    """
    match = _PROVENANCE_RE.search(text or "")
    if not match:
        return None
    body = match.group(0)
    body = body[len(PROVENANCE_PREFIX):]
    body = body.rsplit("-->", 1)[0]
    fields: dict[str, str] = {}
    for token in body.split():
        if "=" in token:
            key, value = token.split("=", 1)
            fields[key] = value
    return fields


def strip_provenance(text: str) -> str:
    """Remove the provenance comment from `text`.

    Used to compare the BEHAVIOURAL text of two blocks without the
    per-install metadata getting in the way. The drift gates use it on
    BOTH sides of their comparison: the canonical render has no
    provenance comment, so removing the installed one leaves the
    behavioural text — which is what those gates are about — byte for
    byte comparable.
    """
    return _PROVENANCE_RE.sub("", text or "")


def insert_provenance(block: str, comment: str) -> str:
    """Insert `comment` immediately after the inner managed-block marker.

    A block with no inner marker is returned unchanged: the provenance
    belongs to the behavioural contract, and a snippet without one has
    no contract to describe.
    """
    index = block.find(INNER_BEGIN)
    if index == -1:
        return block
    insert_at = index + len(INNER_BEGIN)
    if block[insert_at:insert_at + 1] == "\n":
        insert_at += 1
    return block[:insert_at] + comment + "\n" + block[insert_at:]


# ---------------------------------------------------------------------------
# The operation the installers use
# ---------------------------------------------------------------------------


def annotate(
    block: str,
    existing: str | None = None,
    version: str | None = None,
    commit: str | None = None,
    protocol: str | None = None,
    applied_at: str | None = None,
) -> str:
    """Return `block` carrying the provenance comment.

    `block` is the canonical managed block this host's installer would
    write, WITHOUT any provenance comment. `existing` is the block
    currently installed, when there is one; it must span the same
    region, so that the two can be compared.

    `existing` is what makes the operation idempotent. When the
    version, the commit, the protocol version AND the behavioural text
    are all unchanged, the timestamp already in `existing` is
    preserved, so the result is byte-identical to what is installed
    and the installer has nothing to write.

    Whenever anything changed, a fresh timestamp is minted: the
    timestamp records when the block was last applied or updated.

    A run that cannot establish a value does not erase a value the
    block already carries. An adapter installer run on its own — with
    no build behind it — may know neither the product version nor the
    source commit, and it must not claim a version it cannot
    establish; but it must not overwrite the version an earlier,
    informed run established either. So when the behavioural text is
    unchanged, an unestablished value falls back to the installed one.
    When the text DID change, the block is being updated by this run,
    so the recorded value is the one this run can establish — "unknown"
    when it can establish none. An honest "unknown" beats a stale
    claim.
    """
    version = source_version() if version is None else version
    commit = source_commit() if commit is None else commit
    protocol = read_protocol_version() if protocol is None else protocol

    body = strip_provenance(block)

    prior = parse_provenance(existing or "")
    same_text = (
        prior is not None
        and strip_provenance(existing or "").rstrip() == body.rstrip()
    )

    if same_text:
        version = version if version != UNKNOWN else prior.get("version", UNKNOWN)
        commit = commit if commit != UNKNOWN else prior.get("commit", UNKNOWN)
        protocol = protocol if protocol != UNKNOWN else prior.get("protocol", UNKNOWN)

    applied = applied_at or utc_now()
    if (
        prior is not None
        and prior.get("version") == version
        and prior.get("commit") == commit
        and prior.get("protocol") == protocol
        and prior.get("applied")
    ):
        # The metadata is unchanged. The timestamp is preserved only
        # when the block that would be written is byte-identical to the
        # one already installed: otherwise the file is about to change,
        # and the timestamp must record that change rather than the
        # moment the previous content was applied.
        candidate = insert_provenance(
            body, format_provenance(version, commit, protocol, prior["applied"]),
        )
        if candidate == existing:
            applied = prior["applied"]

    return insert_provenance(body, format_provenance(version, commit, protocol, applied))
