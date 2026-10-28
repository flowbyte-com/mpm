"""Authoritative-registry reader.

The gate must not carry a second copy of the tool list — that is exactly the
drift it exists to catch. Every name and action this module reports is read out
of the Go source at check time.

Two sources matter, and they are deliberately different:

  * ``internal/core/tools/registry_list.go`` — the registered substrate
    vocabulary. `mpm call <tool>` and the full MCP surface enforce it.
  * ``cmd/mpm-mcp/tools.go`` — the *compact* MCP surface's action vocabulary,
    which is deliberately shorter and different. `mpm_context action=
    write_handoff` works on the compact surface and fails through `mpm call`,
    because the two surfaces publish different enums. A checker that knew only
    the registry would flag correct documentation as broken, so both are read
    and both are accepted where the document says which surface it means.
"""

from __future__ import annotations

import json
import re
from dataclasses import dataclass, field
from pathlib import Path

REGISTRY_REL = "internal/core/tools/registry_list.go"
COMPACT_REL = "cmd/mpm-mcp/tools.go"

_NAME_RE = re.compile(r'Name:\s*"(mpm_[a-z0-9_]+)"')
_SCHEMA_RE = re.compile(r"Schema:\s*json\.RawMessage\(`(\{.*?\})`\)", re.S)
_COMPACT_VOCAB_RE = re.compile(
    r'CompactMpmContextActionVocabulary\s*=\s*((?:"[^"]*"\s*\+?\s*)+)'
)


@dataclass
class ToolSurface:
    """Every name the substrate answers to, and the actions it accepts."""

    tools: dict[str, set[str]] = field(default_factory=dict)
    # Actions accepted by the compact MCP wrapper but absent from the
    # registered enum. Held separately so a diagnostic can explain why a
    # documented action is legitimate on one surface and not the other.
    compact_only_actions: dict[str, set[str]] = field(default_factory=dict)
    source_files: list[str] = field(default_factory=list)
    errors: list[str] = field(default_factory=list)

    def names(self) -> set[str]:
        return set(self.tools)

    def actions_for(self, tool: str) -> set[str]:
        return self.tools.get(tool, set()) | self.compact_only_actions.get(tool, set())

    def has_action(self, tool: str, action: str) -> bool:
        return action in self.actions_for(tool)

    def is_compact_only(self, tool: str, action: str) -> bool:
        return action in self.compact_only_actions.get(tool, set())


def _parse_registry(root: Path, surface: ToolSurface) -> None:
    path = root / REGISTRY_REL
    try:
        text = path.read_text(encoding="utf-8")
    except OSError as exc:  # pragma: no cover - only on a broken checkout
        surface.errors.append(f"{REGISTRY_REL}: {exc}")
        return
    surface.source_files.append(REGISTRY_REL)

    names = list(_NAME_RE.finditer(text))
    if not names:
        surface.errors.append(
            f"{REGISTRY_REL}: found no tool names — the registry parse is stale, "
            "so every tool-name check below would silently pass"
        )
        return

    for idx, match in enumerate(names):
        name = match.group(1)
        end = names[idx + 1].start() if idx + 1 < len(names) else len(text)
        block = text[match.start():end]
        actions: set[str] = set()
        schema = _SCHEMA_RE.search(block)
        if schema:
            try:
                parsed = json.loads(schema.group(1))
            except json.JSONDecodeError as exc:
                surface.errors.append(f"{REGISTRY_REL}: {name} schema is not JSON: {exc}")
                parsed = {}
            enum = (
                parsed.get("properties", {})
                .get("action", {})
                .get("enum")
            )
            if isinstance(enum, list):
                actions = {str(a) for a in enum}
        # Tools without an `action` enum (mpm_resolve, mpm_blob_read, …) take
        # no action at all. An empty set is the correct answer for them, not
        # an error — the description carries the parameter shape instead.
        surface.tools[name] = actions


def _parse_compact_surface(root: Path, surface: ToolSurface) -> None:
    """Read the compact MCP wrapper's extra action vocabulary.

    ``mpm_context`` publishes `write_handoff` / `read_handoff` on the compact
    surface. They are not in the registered enum and `mpm call` rejects them;
    both facts are true at once and the documents have to be allowed to state
    either one.
    """
    path = root / COMPACT_REL
    try:
        text = path.read_text(encoding="utf-8")
    except OSError:
        return
    if COMPACT_REL not in surface.source_files:
        surface.source_files.append(COMPACT_REL)

    vocab = _COMPACT_VOCAB_RE.search(text)
    if not vocab:
        return
    # The value is a human-readable "a | b | c." display string assembled from
    # Go string concatenation, not a slice. Join the literals, then split on
    # the separator the string itself uses.
    joined = "".join(re.findall(r'"([^"]*)"', vocab.group(1)))
    actions = {
        token.strip().rstrip(".")
        for token in joined.split("|")
        if token.strip().rstrip(".")
    }
    if actions:
        registered = surface.tools.get("mpm_context", set())
        surface.compact_only_actions["mpm_context"] = actions - registered
        # The compact wrapper is a *superset* of the registered enum for this
        # tool: `mpm_context` on the compact surface also answers to
        # read_wake_context / read_directives / route / … which the registry
        # does carry. Keep only the genuinely extra ones above; the shared
        # ones already live in the registered set.


def load(root: Path) -> ToolSurface:
    surface = ToolSurface()
    _parse_registry(root, surface)
    _parse_compact_surface(root, surface)
    return surface
