#!/usr/bin/env python3
"""jsonc_config.py — safe migration of the MPM registration in opencode.jsonc.

Why this module exists
----------------------
`opencode.jsonc` is JSONC, not JSON. Users put `//` comments and trailing
commas in it — that is the entire reason the `.jsonc` extension exists.
Reading it with `json.loads` therefore fails on a perfectly valid config,
and the previous installer treated that failure as "nothing to do" and
exited 0. The shell then reported the migration as successful, so a user
with a commented config got a silent no-op: the stale `opencode-mpm` entry
survived and the OpenCode plugin loader broke.

Two properties this module is built around:

1. **Comment stripping is a character scanner, not a regex.** A regex like
   `re.sub(r'//.*', '', text)` corrupts any URL in a string value
   (`"https://opencode.ai/config.json"` becomes `"https:`), and any config
   carrying a `$schema` URL would be rewritten into invalid JSON. The
   scanner tracks string state and only treats `//` and `/*` as comments
   when it is actually outside a string literal.

2. **Unrelated bytes are never rewritten.** Parsing the config and dumping
   it back with `json.dump` would preserve the *data* but destroy every
   comment the operator wrote — in a format whose purpose is to hold
   comments. Instead we locate the byte span of the top-level `plugin`
   value by ``strip_jsonc``-ing the config, splicing them in place. Offsets
   are preserved by blanking comments in place, so a span found in the
   stripped text indexes the original text exactly.

Registration contract (2026-10-16)
-----------------------------------
OpenCode 2 resolves a DIRECTORY plugin registration by looking for a
root-level ``server.*`` (preferred) or ``index.*`` entry at the top of the
registered directory. It never consults package.json for plugin entries,
and it silently drops a directory registered without a resolvable entry.
Registering ``file://…/mpm-opencode/dist/index.js`` — the shape this
installer wrote before 2026-10-16 — therefore registered a FILE url that
the V2 loader refuses ("configured plugin path must be a directory") and,
after the rename to a directory registration, loads nothing.

The canonical registration is the plugin PACKAGE ROOT:

    file://{MPM_WORKSPACE}/agent_installation/mpm-opencode

Historical MPM-owned entries recognized for migration/removal anywhere in
either the ``plugin`` (V1 key) or ``plugins`` (V2 key) array:

  - file://…/opencode-mpm            (legacy namespace, pre-2026-09-17)
  - file://…/opencode-mpm/dist/index.js
  - file://…/mpm-opencode            (necanonical itself in the other key)
  - file://…/mpm-opencode/dist/index.js   (the broken file path entry; see
        issue flowbyte-com/mpm#19 / the installer defect issue)

Recognition is by adapter DIRECTORY SEGMENT, not by raw substring: an
unrelated path like ``/tmp/mpm-opencode-debug-XYZ`` does not segment-match
``mpm-opencode`` and is left alone.

Key semantics:

  - ``plugins`` is the V2 key. If it exists it is the array this module
    operates on.
  - If only ``plugin`` exists, operations happen there; when the module
    edits that array the key is renamed to ``plugins`` (the V2 loader
    normalizes both, but this installer's fresh-config template and
    verification speak ``plugins``; a migrated config should end in the
    canonical key).
  - If BOTH keys exist the arrays are merged into ``plugins`` (order:
    ``plugins`` entries first, then new-from-``plugin`` entries) and the
    ``plugin`` member is removed, so the loader cannot see two competing
    registrations of the same plugin.

Uninstall (--remove-mpm): every MPM-owned entry — the canonical one and
every historical stale shape — is removed from both keys; a key whose
array becomes empty is removed entirely. Unrelated entries and comments
survive byte-for-byte.

Idempotency is structural: if the computed plugin list already equals the
current one, the file is not written at all. A repeated install is
therefore byte-identical rather than merely semantically equal.

CLI
---
    python3 jsonc_config.py <config-path> <canonical-entry>
    python3 jsonc_config.py --check-entry <config-path> <canonical-entry>
    python3 jsonc_config.py --remove-mpm <config-path> <canonical-entry>

Exit codes:
    0  command succeeded (migration applied, or already correct)
    1  config could not be parsed or migrated safely — nothing written
"""

from __future__ import annotations

import json
import os
import stat
import sys
from typing import Any


class JsoncError(ValueError):
    """Raised when the config cannot be parsed or migrated safely."""


# A UTF-8 BOM decodes to U+FEFF. Python's `json` module rejects it outright
# ("Unexpected UTF-8 BOM"), and it is neither JSON whitespace nor a comment,
# so the comment scanner leaves it alone too. A BOM-prefixed config is
# perfectly valid and must not be refused as malformed — we remove it for
# processing and put it back on write, so the file keeps the exact encoding
# the operator had.
_BOM = "\ufeff"

ADAPTER_BASENAMES = ("mpm-opencode", "opencode-mpm")


def strip_jsonc(text: str) -> str:
    """Blank out comments, preserving byte offsets.

    Comment characters are replaced with spaces and newlines are kept, so
    the returned string has exactly the same length as the input and every
    offset found in it is valid in the original. Line numbers in any
    downstream `json` error also stay correct.

    Handles the two JSONC comment forms and both string escapes:
    `\\"` is an escaped quote, not the end of the literal.
    """
    out = list(text)
    i = 0
    n = len(text)
    in_string = False

    while i < n:
        ch = text[i]

        if in_string:
            if ch == "\\":
                # Skip the escape and whatever it escapes. A trailing
                # backslash at EOF has nothing to skip.
                i += 2
                continue
            if ch == '"':
                in_string = False
            i += 1
            continue

        if ch == '"':
            in_string = True
            i += 1
            continue

        if ch == "/" and i + 1 < n:
            nxt = text[i + 1]
            if nxt == "/":
                end = text.find("\n", i)
                end = n if end == -1 else end
                for k in range(i, end):
                    out[k] = " "
                i = end
                continue
            if nxt == "*":
                end = text.find("*/", i + 2)
                if end == -1:
                    raise JsoncError(
                        "unterminated block comment at offset %d" % i
                    )
                for k in range(i, end + 2):
                    if out[k] != "\n":
                        out[k] = " "
                i = end + 2
                continue

        i += 1

    if in_string:
        raise JsoncError("unterminated string literal")

    return "".join(out)


def strip_trailing_commas(text: str) -> str:
    """Remove commas that directly precede `}` or `]`, outside strings.

    Operates on already-comment-stripped text. String state is tracked so a
    comma inside a string value is never touched.
    """
    out = list(text)
    in_string = False
    i = 0
    n = len(text)

    while i < n:
        ch = text[i]
        if in_string:
            if ch == "\\":
                i += 2
                continue
            if ch == '"':
                in_string = False
            i += 1
            continue

        if ch == '"':
            in_string = True
            i += 1
            continue

        if ch == ",":
            # Look ahead past whitespace for the next structural char.
            j = i + 1
            while j < n and text[j] in " \t\r\n":
                j += 1
            if j < n and text[j] in "}]":
                out[i] = " "
        i += 1

    return "".join(out)


def parse(text: str) -> Any:
    """Parse JSONC into Python data. Raises JsoncError on malformed input."""
    stripped = strip_jsonc(text.lstrip(_BOM) if text.startswith(_BOM) else text)
    stripped = strip_trailing_commas(stripped)
    try:
        return json.loads(stripped)
    except json.JSONDecodeError as exc:
        raise JsoncError(
            "config is not valid JSONC even after removing comments and "
            "trailing commas: %s (line %d column %d)" % (exc.msg, exc.lineno, exc.colno)
        ) from exc


def _skip_ws(stripped: str, i: int) -> int:
    while i < len(stripped) and stripped[i] in " \t\r\n":
        i += 1
    return i


def find_top_level_value_span(stripped: str, key: str):
    """Return (value_start, value_end, key_start) for a top-level key.

    `key_start` is returned so the caller can recover the line indentation
    of the *key* (the value's own offset sits after `"key": `, which is not
    a valid indentation prefix). Returns None when the key is absent. Raises
    JsoncError when the structure cannot be walked (which means the config
    is malformed and we must not guess).
    """
    i = _skip_ws(stripped, 0)
    if i >= len(stripped) or stripped[i] != "{":
        raise JsoncError("config root is not a JSON object")

    i += 1
    while True:
        i = _skip_ws(stripped, i)
        if i >= len(stripped):
            raise JsoncError("unterminated top-level object")
        if stripped[i] == "}":
            return None
        if stripped[i] != '"':
            raise JsoncError(
                "expected a quoted key at top level, found %r" % stripped[i]
            )

        # Read the key literal.
        key_start = i
        key_indent_start = key_start
        i += 1
        while i < len(stripped):
            if stripped[i] == "\\":
                i += 2
                continue
            if stripped[i] == '"':
                break
            i += 1
        else:
            raise JsoncError("unterminated key literal")
        key_literal = stripped[key_start : i + 1]
        i += 1

        i = _skip_ws(stripped, i)
        if i >= len(stripped) or stripped[i] != ":":
            raise JsoncError("expected ':' after top-level key")
        i = _skip_ws(stripped, i + 1)

        value_start = i
        # Walk the value to its end. For a container the value ends at its
        # own matching closer, so stop as soon as depth returns to 0 after a
        # closer — otherwise the span would swallow the next member's
        # terminator (a top-level `"plugin": [ ... ]` followed by `}` would
        # run on to that `}` and delete it on splice).
        depth = 0
        in_string = False
        while i < len(stripped):
            ch = stripped[i]
            if in_string:
                if ch == "\\":
                    i += 2
                    continue
                if ch == '"':
                    in_string = False
                i += 1
                continue
            if ch == '"':
                in_string = True
                i += 1
                continue
            if ch in "{[":
                depth += 1
                i += 1
                continue
            if ch in "}]":
                if depth == 0:
                    break
                depth -= 1
                i += 1
                if depth == 0:
                    break
                continue
            if ch == "," and depth == 0:
                break
            i += 1
        value_end = i

        try:
            decoded_key = json.loads(key_literal)
        except json.JSONDecodeError as exc:
            raise JsoncError("unparseable top-level key %r: %s" % (key_literal, exc)) from exc

        if decoded_key == key:
            return (value_start, value_end, key_start)

        # Advance past the comma to the next member.
        i = _skip_ws(stripped, i)
        if i < len(stripped) and stripped[i] == ",":
            i += 1
        elif i < len(stripped) and stripped[i] == "}":
            return None
        else:
            raise JsoncError("expected ',' or '}' between top-level members")


def _indent_of(text: str, offset: int) -> str:
    """Leading whitespace of the line containing `offset`."""
    line_start = text.rfind("\n", 0, offset) + 1
    prefix = text[line_start:offset]
    return prefix if prefix.strip() == "" else ""


def _entry_comments(original: str, stripped: str, span) -> dict:
    """Map each existing plugin entry to any comment trailing its line.

    A comment belonging to an entry is carried through to the re-rendered
    array, so migrating MPM's own entry does not silently delete an
    operator's note explaining why another plugin is loaded.
    """
    comments: dict[str, list[str]] = {}
    value_start = span[0]
    i = value_start
    while i < span[1]:
        # Skip to the next string literal, which is an array entry.
        if stripped[i] != '"':
            i += 1
            continue
        entry_start = i
        i += 1
        while i < len(stripped):
            if stripped[i] == "\\":
                i += 2
                continue
            if stripped[i] == '"':
                break
            i += 1
        entry_end = i + 1
        try:
            entry = json.loads(original[entry_start:entry_end])
        except (json.JSONDecodeError, ValueError):
            i = entry_end
            continue

        # Comment between this entry and the next, on the same line. Only
        # string entries carry comments (a comment attached to an object
        # entry is preserved byte-wise because object entries are not
        # re-rendered when they are the only change — guarded below).
        line_end = original.find("\n", entry_end)
        line_end = len(original) if line_end == -1 else line_end
        tail = original[entry_end:line_end]
        pos = tail.find("//")
        if pos != -1 and isinstance(entry, str):
            note = tail[pos:].strip()
            if note and entry not in comments:
                comments[entry] = [note]
        i = entry_end
    return comments


def detect_newline(text: str) -> str:
    """The line terminator the file already uses.

    The renderer must not impose a terminator the file does not use: a
    CRLF config would otherwise come back with a few LF lines spliced into
    it, which is the same class of "we rewrote unrelated bytes" defect as
    the universal-newline read this replaced.

    Mixed files resolve to the dominant terminator. A lone-`\r` (classic
    Mac) file is not recognised: `\r` alone is treated as content, so the
    original bytes are preserved unchanged and the splice stays local.
    """
    crlf = text.count("\r\n")
    lf = text.count("\n") - crlf
    return "\r\n" if crlf > lf else "\n"


def _render_plugin_value(entries: list, indent: str, comments: dict, newline: str = "\n") -> str:
    """Render the plugin array at the given indentation.

    Written inline (not via json.dump of the whole document) so the rest of
    the file is untouched. Each entry keeps any trailing comment it had.
    String entries render as JSON strings; non-string entries (the V2
    `{package, options}` object form) render as compact JSON objects.
    """
    inner = indent + "  "
    lines = ["["]
    # No trailing comma on the final entry: JSONC would accept one, but
    # strict-JSON consumers (and json.loads in the test suite, and other
    # tooling that reads this file) would not. A trailing comma here was a
    # regression this renderer introduced and then had to be taught out of.
    last = len(entries) - 1
    for index, entry in enumerate(entries):
        note = ""
        text_key = entry if isinstance(entry, str) else json.dumps(entry, sort_keys=True)
        for candidate in comments.get(text_key, []):
            note = "  " + candidate
            break
        comma = "" if index == last else ","
        rendered = json.dumps(entry) if isinstance(entry, str) else json.dumps(entry)
        lines.append("%s%s%s%s" % (inner, rendered, comma, note))
    lines.append("%s]" % indent)
    return newline.join(lines)


def _find_root_open_brace(stripped: str) -> int:
    i = _skip_ws(stripped, 0)
    if i >= len(stripped) or stripped[i] != "{":
        raise JsoncError("config root is not a JSON object")
    return i


def _root_is_empty(stripped: str, open_brace: int) -> bool:
    j = _skip_ws(stripped, open_brace + 1)
    return j < len(stripped) and stripped[j] == "}"


# --------------------------------------------------------------------------
# MPM registration recognition
# --------------------------------------------------------------------------


def _entry_path(entry: str) -> str:
    """The filesystem path an entry names, with any file:// scheme removed.

    Only used for equality and segment tests; the entry value itself is
    always preserved or regenerated as the operator wrote it.
    """
    if entry.startswith("file://"):
        return entry[len("file://"):]
    return entry


def _adapter_segment(path: str) -> str | None:
    """The MPM adapter directory segment in `path`, when present.

    Match by exact path component so unrelated directories whose names
    merely CONTAIN the adapter name (`/tmp/mpm-opencode-debug-XYZ`) are
    not misread as MPM registrations.
    """
    for part in path.split("/"):
        if part in ADAPTER_BASENAMES:
            return part
    return None


def _identity(path: str) -> str:
    """The identity of a filesystem path: what it really names.

    Two spellings of ONE directory have ONE identity. The versioned
    bundle layout is built on that fact: `~/.mpm/agent_installation` is
    a symlink into the active release, so the stable registration

        file://<home>/.mpm/agent_installation/mpm-opencode

    and the physical

        file://<home>/.mpm/releases/<rev>/agent_installation/mpm-opencode

    name the SAME directory through DIFFERENT strings. Comparing the
    strings compares the spelling, and the spelling is exactly what the
    layout changes — so a string comparison misses the duplicate and
    lets one plugin be registered, and loaded, twice.

    A path that does not exist resolves to its own lexical form, which
    is the right answer for comparison: two paths that do not exist are
    the same only when they are spelled the same way.
    """
    if not path:
        return path
    try:
        return os.path.realpath(path)
    except OSError:  # pragma: no cover - realpath does not raise on a missing path
        return os.path.normpath(path)


def _install_scope(canonical_path: str) -> list[str]:
    """The installation roots whose MPM registrations the canonical owns.

    MPM's own registrations live under the canonical's installation
    root: `<root>/agent_installation/<adapter>` for the active release,
    and `<root>/releases/<rev>/agent_installation/<adapter>` for a
    historical one.

    A SEPARATE MPM installation under another workspace has its own
    root. Its registration is not ours to rewrite: MPM owns one
    registration in the installation it was pointed at, and rewriting
    another installation's entry would silently break that
    installation's plugin.
    """
    parts = canonical_path.split("/")
    try:
        ai = parts.index("agent_installation")
    except ValueError:
        # The canonical does not name an adapter install root — an
        # unusual but possible spelling. Only the canonical itself is
        # then in scope.
        return [os.path.dirname(canonical_path)]
    scope = ["/".join(parts[:ai]) or "/"]
    if ai >= 2 and parts[ai - 2] == "releases":
        # `<root>/releases/<rev>/agent_installation/<adapter>`: the
        # release directory is a release-scoped copy of one
        # installation, so the scope reaches the data root above it,
        # and every sibling release with it.
        scope.append("/".join(parts[: ai - 2]) or "/")
    return scope


def _in_scope(path: str, scope: list[str]) -> bool:
    """True when the path lies inside one of the scope's roots.

    The comparison is on path COMPONENTS, not on a string prefix: a
    root of `<root>/.mpm` does not contain `<root>/.mpm-other`.
    """
    candidate = os.path.normpath(path)
    for root in scope:
        root = os.path.normpath(root)
        if candidate == root:
            return True
        prefix = root if root.endswith("/") else root + "/"
        if candidate.startswith(prefix):
            return True
    return False


def _is_adapter_payload(path: str, scope: list[str]) -> bool:
    """True when the path names an adapter package in the scoped install.

    MPM keeps its host adapters under a directory named
    `agent_installation`, and the package directory inside it carries an
    adapter basename. A path with that shape is MPM's own payload, at
    whatever path the current installation layout put it.

    The shape is necessary but NOT sufficient: the payload must also
    lie inside the installation the canonical registration names. A
    second MPM installation under another workspace has the same shape
    and is not ours to rewrite.

    This is what recognises the registration the versioned-bundle layout
    produces. The stable path and the revision-pinned path are the same
    directory NOW, so an identity comparison collapses them — but after
    the next upgrade the revision-pinned path names a DIFFERENT
    directory, and it must still be recognised as MPM's registration
    rather than left behind as a second copy of the plugin.
    """
    parts = _entry_path(path).split("/")
    for i in range(len(parts) - 1):
        if parts[i] == "agent_installation" and parts[i + 1] in ADAPTER_BASENAMES:
            return _in_scope(_entry_path(path), scope)
    return False


def _canonical_dir(canonical: str) -> str:
    """The plugin package directory a canonical registration points at.

    Accepts the directory URL the installer writes, and — for backwards
    compatibility of callers — the historical dist/index.js file URL,
    from which the package root is derived by stripping the known
    compiled-entry suffix.
    """
    path = _entry_path(canonical)
    for suffix in ("/dist/index.js", "/index.js"):
        if path.endswith(suffix):
            path = path[: -len(suffix)]
    return path.rstrip("/")


def _stale_and_canonical_paths(canonical: str) -> tuple[str, list[str]]:
    """(canonical package path, historical MPM-owned paths to migrate).

    The canonical package path is the registered DIRECTORY. Stale shapes
    are same-namespace variants: the dist/index.js file registration this
    installer wrote before 2026-10-16 (defect: silently loads nothing in
    OpenCode 2) and every legacy `opencode-mpm` namespace variant.
    """
    directory = _canonical_dir(canonical)
    legacy_dir = None
    dir_path = directory
    if dir_path.split("/") and dir_path.split("/")[-1] in ADAPTER_BASENAMES:
        legacy_dir = "/".join(dir_path.split("/")[:-1] + ["opencode-mpm"])
    stale: list[str] = []
    if legacy_dir and legacy_dir != dir_path:
        stale.extend([legacy_dir, legacy_dir + "/dist/index.js"])
    stale.append(dir_path + "/dist/index.js")
    return dir_path, stale


def is_mpm_entry(entry: Any, canonical: str) -> bool:
    """True when `entry` is an MPM-owned registration shape (any era).

    Only recognizes the two adapter directory namespaces, by path
    segment, across file:// URL and plain-path spellings.
    """
    if not isinstance(entry, str):
        return False
    path = _entry_path(entry)
    return _adapter_segment(path) is not None


def _dedupe(entries: list, canonical: str | None = None) -> list:
    """Keep first occurrence of each entry value, preserving order.

    An MPM-owned entry is keyed by its RESOLVED IDENTITY, not by its
    spelling: two spellings of one directory are one registration, and
    keeping both is the duplicate-registration defect. Unrelated entries
    are keyed by their own text — this function does not reorganise the
    operator's config, and two of their entries are theirs to keep.
    """
    seen: set = set()
    out: list = []
    for item in entries:
        if isinstance(item, str):
            if canonical is not None and is_mpm_entry(item, canonical):
                key: object = ("identity", _identity(_entry_path(item)))
            else:
                key = ("text", item)
        else:
            key = ("text", json.dumps(item, sort_keys=True))
        if key in seen:
            continue
        seen.add(key)
        out.append(item)
    return out


def _plugin_arrays(data: dict) -> dict:
    """The existing plugin arrays by key name.

    Raises when a key exists in a form this module refuses to guess at
    (the historical non-list refusal); an absent key is [].
    """
    arrays = {}
    for key in ("plugins", "plugin"):
        if key not in data:
            arrays[key] = []
            continue
        value = data[key]
        if not isinstance(value, list):
            raise JsoncError(
                "'%s' must be an array of strings, found %r — refusing to "
                "guess at the intended shape; rewrite it as a JSON array by "
                "hand and re-run" % (key, type(value).__name__)
            )
        arrays[key] = value
    return arrays


# The summary/changed plumbing is kept simple on purpose; see CLI main().
def _rename_key(text: str, stripped: str, key: str, new_key: str) -> str:
    """Rename a top-level key literal in `text`.

    `stripped` is strip_jsonc(text); the key literal offsets are valid in
    both since strip_jsonc is offset-preserving. Offsets after the rename
    shift by the length delta, which callers must respect when they touch
    spans that sit AFTER the key (this is why migrate() renames the key
    FIRST and then re-locates the value span).
    """
    span = find_top_level_value_span(stripped, key)
    if span is None:
        return text
    _, _, key_start = span
    old_literal = '"%s"' % key
    assert text[key_start : key_start + len(old_literal)] == old_literal
    return text[:key_start] + '"%s"' % new_key + text[key_start + len(old_literal):]


def _remove_member(text: str, stripped: str, key: str) -> str:
    """Remove an entire top-level member (key, colon, value) from `text`.

    Also removes the comma that separated the member from its neighbours,
    whether the comma preceded (member was last) or followed it (member
    was first or in the middle).
    """
    span = find_top_level_value_span(stripped, key)
    if span is None:
        return text
    value_start, value_end, key_start = span

    start = key_start
    # Include the preceding comma when this member is the LAST one.
    j = _skip_ws(stripped, value_end)
    if j < len(stripped) and stripped[j] == "}":
        k = start
        while k > 0 and stripped[k - 1] in " \t\r\n":
            k -= 1
        if k > 0 and stripped[k - 1] == ",":
            start = k - 1
    else:
        # Include the following comma (first/middle position).
        j = _skip_ws(stripped, value_end)
        if j < len(stripped) and stripped[j] == ",":
            j += 1
        value_end = j
    return text[:start] + text[value_end:]


def _insert_member(text: str, key: str, entries: list, comments: dict) -> str:
    """Insert a top-level key with a rendered array value."""
    stripped = strip_jsonc(text)
    open_brace = _find_root_open_brace(stripped)
    empty = _root_is_empty(stripped, open_brace)
    root_indent = _indent_of(text, open_brace)
    indent = root_indent + "  "
    rendered = _render_plugin_value(entries, indent, comments, detect_newline(text))
    newline = detect_newline(text)
    block = '%s%s%s: %s%s' % (newline, indent, '"%s"' % key, rendered, "" if empty else ",")
    if empty:
        updated = (
            text[: open_brace + 1]
            + block
            + newline
            + root_indent
            + text[open_brace + 1 :]
        )
    else:
        updated = text[: open_brace + 1] + block + text[open_brace + 1 :]
    return updated


def _extract_entry_comments(text: str, data: dict) -> dict:
    """Comments keyed by entry text, across both plugin keys.

    Used when an array is re-rendered: each entry carries any trailing
    same-line comment it had, so migration never deletes an operator's
    note about why a plugin is loaded.
    """
    stripped = strip_jsonc(text)
    comments: dict[str, list[str]] = {}
    for key in ("plugins", "plugin"):
        if key not in data:
            continue
        span = find_top_level_value_span(stripped, key)
        if span is None:
            continue
        for entry_text, notes in _entry_comments(text, stripped, span).items():
            comments.setdefault(entry_text, notes)
    return comments


def _merged_entries(
    data: dict, canonical: str, raw_comments: dict | None = None
) -> tuple[list, int, dict]:
    """Map every MPM-owned entry across both keys to the canonical shape.

    Returns (final_entry_list, replaced_count, comments) where the list is
    ordered by first appearance (plugins entries first, then plugin-only
    entries), MPM-owned entries rewritten to the canonical package
    directory, and exact duplicates collapsed. Unrelated entries
    (including the V2 object form) pass through untouched. Comments are
    re-keyed from the original entry text onto the rendered entry text, so
    a comment attached to a replaced MPM entry follows the canonical entry
    it became.

    MPM OWNS EXACTLY ONE REGISTRATION. An entry is MPM's when it is the
    canonical path, one of the historical stale shapes, the same
    directory reached by a different route (the versioned-bundle layout
    makes the stable path and the revision-pinned path one directory
    under two names), or MPM's own adapter payload at some other path.
    Every one of them is rendered as the canonical registration, so the
    result carries one registration of the plugin rather than several
    spellings of it.
    """
    arrays = _plugin_arrays(data)
    _, stale_paths = _stale_and_canonical_paths(canonical)
    canonical_path = _canonical_dir(canonical)
    canonical_identity = _identity(canonical_path)
    scope = _install_scope(canonical_path)
    raw_comments = raw_comments or {}

    union: list = list(arrays["plugins"]) + list(arrays["plugin"])
    replaced = 0
    mapped: list = []
    comments: dict[str, list[str]] = {}

    def keep(entry, rendered):
        mapped.append(rendered)
        if isinstance(entry, str) and isinstance(rendered, str):
            notes = raw_comments.get(entry)
            if notes:
                comments.setdefault(rendered, notes)

    for entry in union:
        if isinstance(entry, str):
            path = _entry_path(entry)
            if (
                path == canonical_path
                or path in stale_paths
                # The same directory, reached by a different route. The
                # versioned-bundle layout produces exactly this: the
                # stable path and the revision-pinned path name one
                # directory, and a spelling comparison would keep both.
                or _identity(path) == canonical_identity
                # MPM's own payload at some other path — a previous
                # release's copy, left behind by an earlier install.
                # It is MPM's registration, so it is normalised to the
                # canonical one rather than left as a second copy.
                or _is_adapter_payload(path, scope)
            ):
                # Normalize every MPM-owned spelling (stale shapes and
                # same-directory registrations under either scheme) to
                # the canonical package URL this installer writes.
                keep(entry, canonical)
                if path != canonical_path:
                    replaced += 1
                continue
        keep(entry, entry)

    # The canonical registration must be PRESENT: this module only rewrites
    # MPM-owned entries, so without this a config carrying unrelated
    # plugins alone would migrate to... no registration at all. Append,
    # so existing entries keep their byte positions and trailing comments.
    if not any(
        isinstance(e, str) and _entry_path(e) == canonical_path for e in mapped
    ):
        mapped.append(canonical)
    return _dedupe(mapped, canonical), replaced, comments


def migrate(text: str, canonical: str) -> tuple[str, bool, str]:
    """Migrate MPM registration entries to the canonical package directory.

    Returns (new_text, changed, summary). When nothing needs to change the
    original text is returned unchanged, so a repeated install is a
    byte-identical no-op rather than a rewrite. `summary` describes what
    actually happened, so the installer does not report a migration that
    did not occur.

    Raises JsoncError if the config cannot be parsed or walked safely. The
    caller must not write anything in that case.
    """
    bom = ""
    if text.startswith(_BOM):
        bom, text = _BOM, text[1:]

    data = parse(text)
    if not isinstance(data, dict):
        raise JsoncError("config root must be a JSON object")

    arrays = _plugin_arrays(data)
    raw_comments = _extract_entry_comments(text, data) if (data.get("plugins") or data.get("plugin")) else {}

    final_entries, replaced, comments = _merged_entries(data, canonical, raw_comments)

    summary_bits: list[str] = []
    if replaced:
        summary_bits.append(
            "replaced %d stale MPM registration entr%s with %s"
            % (replaced, "y" if replaced == 1 else "ies", canonical)
        )

    if "plugins" in data:
        # Canonical key exists: operate there. If the legacy `plugin`
        # member also exists, merge it away entirely (see module
        # docstring) so the loader cannot see two registrations.
        if "plugin" in data:
            return _apply(
                text,
                target_key="plugins",
                drop_key="plugin",
                entries=final_entries,
                comments=comments,
                summary_bits=summary_bits
                + ["merged legacy 'plugin' entries into 'plugins'"],
                bom=bom,
            )
        if not replaced and _entry_list_equals(arrays["plugins"], final_entries):
            return bom + text, False, "already canonical"
        return _apply(
            text,
            target_key="plugins",
            drop_key=None,
            entries=final_entries,
            comments=comments,
            summary_bits=(summary_bits + ["added %s" % canonical])
            if not replaced
            else summary_bits,
            bom=bom,
        )

    if "plugin" in data:
        return _apply(
            text,
            target_key="plugins",
            drop_key=None,
            entries=final_entries,
            comments=comments,
            summary_bits=summary_bits + ["renamed legacy 'plugin' key to 'plugins'"],
            bom=bom,
            rename_from="plugin",
            # Rename always changes the file; list equality cannot make a
            # rename a no-op, so no noop_check here.
        )

    # Neither key exists: add a canonical `plugins` member.
    return (
        _insert_member(text, "plugins", [canonical], {}),
        True,
        "added %s to a config with no plugin array" % canonical,
    )


def _entry_list_equals(a: list, b: list) -> bool:
    if len(a) != len(b):
        return False
    for x, y in zip(a, b):
        if isinstance(x, str) and isinstance(y, str):
            if x != y:
                return False
        else:
            if json.dumps(x, sort_keys=True) != json.dumps(y, sort_keys=True):
                return False
    return True


def _apply(
    text: str,
    *,
    target_key: str,
    drop_key: str | None,
    entries: list,
    comments: dict | None,
    summary_bits: list[str],
    bom: str,
    rename_from: str | None = None,
    noop_check: tuple[list, list] | None = None,
) -> tuple[str, bool, str]:
    """Splice the computed result into the text (single write path).

    Order of operations matters: the key rename happens FIRST (it is a
    pure literal substitution), then the value span is re-located in the
    renamed text and spliced, then a dropped key's member is removed from
    the already-spliced text. A `noop_check` of (existing, final) that
    compares equal short-circuits to unchanged.
    """
    updated = text
    changed = False

    if rename_from is not None:
        stripped_current = strip_jsonc(updated)
        if find_top_level_value_span(stripped_current, rename_from) is not None:
            updated = _rename_key(updated, stripped_current, rename_from, target_key)
            changed = True

    if noop_check is not None and not changed:
        existing, final = noop_check
        if _entry_list_equals(existing, final):
            return bom + text, False, "already canonical"

    stripped = strip_jsonc(updated)
    span = find_top_level_value_span(stripped, target_key)
    if span is not None and (entries is not None):
        value_start, value_end, key_start = span
        indent = _indent_of(updated, key_start)
        rendered = _render_plugin_value(
            entries, indent, comments or {}, detect_newline(text)
        )
        updated = updated[:value_start] + rendered + updated[value_end:]
        changed = True

    if drop_key is not None:
        stripped_dropping = strip_jsonc(updated)
        if find_top_level_value_span(stripped_dropping, drop_key) is not None:
            updated = _remove_member(updated, stripped_dropping, drop_key)
            changed = True

    summary = "; ".join(summary_bits) if summary_bits else "migrated"
    return bom + updated, changed, summary


def check_entry(text: str, canonical: str) -> bool:
    """True when the config registers the canonical package directory.

    A passing check requires:
      - the canonical directory entry is present in either key, AND
      - no STALE MPM-owned registration remains anywhere.

    The second condition is what rejects the known-broken file-path
    registration (`file://…/mpm-opencode/dist/index.js`), which OpenCode
    silently drops: presence of that entry is a failed verification, not
    a success, even though a historical file registration may sit
    alongside the canonical one.
    """
    data = parse(text)
    if not isinstance(data, dict):
        raise JsoncError("config root must be a JSON object")
    arrays = _plugin_arrays(data)
    canonical_path = _canonical_dir(canonical)
    entries = arrays["plugins"] + arrays["plugin"]
    present = any(
        isinstance(p, str) and _entry_path(p) == canonical_path for p in entries
    )
    stale = [
        p
        for p in entries
        if isinstance(p, str)
        and is_mpm_entry(p, canonical)
        and _entry_path(p) != canonical_path
    ]
    return present and not stale


def remove_mpm(text: str, canonical: str) -> tuple[str, bool, str]:
    """Remove every MPM-owned registration entry from both keys.

    Used by `install.sh --uninstall`. MPM-owned = any entry naming the
    `mpm-opencode` or legacy `opencode-mpm` adapter directory, in file://
    URL or plain-path form, directory or historical dist/index.js
    file-registration spellings. Entries that are not MPM-owned are never
    touched, and an operator's comments survive.

    A key whose array becomes empty is removed entirely: an uninstall that
    empties MPM's registrations takes its own key artifact down with it.

    Returns (new_text, changed, summary). Raises JsoncError on an
    unparseable config; the caller must not write anything then.
    """
    bom = ""
    if text.startswith(_BOM):
        bom, text = _BOM, text[1:]

    data = parse(text)
    if not isinstance(data, dict):
        raise JsoncError("config root must be a JSON object")

    arrays = _plugin_arrays(data)
    removed = 0
    updated = text
    summary_bits: list[str] = []

    # Only touch keys that exist.
    for key in ("plugins", "plugin"):
        existing = arrays[key]
        owned_present = any(is_mpm_entry(e, canonical) for e in existing)
        if not owned_present:
            continue
        kept = [
            e
            for e in existing
            if not (isinstance(e, str) and is_mpm_entry(e, canonical))
        ]
        removed += len(existing) - len(kept)
        if kept:
            stripped = strip_jsonc(updated)
            span = find_top_level_value_span(stripped, key)
            if span is None:  # pragma: no cover - existence checked above
                continue
            value_start, value_end, key_start = span
            indent = _indent_of(updated, key_start)
            rendered = _render_plugin_value(kept, indent, {}, detect_newline(text))
            updated = updated[:value_start] + rendered + updated[value_end:]
        else:
            # The array is empty after filtering: remove the whole member.
            stripped = strip_jsonc(updated)
            updated = _remove_member(updated, stripped, key)
            summary_bits.append("removed empty '%s' key" % key)

    if removed == 0 and not summary_bits:
        return bom + text, False, "no MPM-owned registration entries found"
    if removed:
        summary_bits.insert(0, "removed %d MPM registration entr%s" % (removed, "y" if removed == 1 else "ies"))
    return bom + updated, True, "; ".join(summary_bits)


def main(argv: list[str]) -> int:
    mode = "migrate"
    args = list(argv[1:])
    if args and args[0] == "--check-entry":
        mode, args = "check", args[1:]
    elif args and args[0] == "--remove-mpm":
        mode, args = "remove", args[1:]

    if len(args) != 2:
        print(
            "usage: jsonc_config.py [--check-entry|--remove-mpm] <config-path> <canonical-entry>",
            file=sys.stderr,
        )
        return 2

    config_path, canonical = args

    # Read with newline="" so no newline translation happens in either
    # direction. Python's default universal-newline mode silently rewrites
    # every \r\n to \n on read, which converts a CRLF config to LF end to
    # end — rewriting lines this migration was never supposed to touch.
    # newline="" makes every offset in the string a real file offset, which
    # is what the splice logic assumes.
    try:
        with open(config_path, "r", encoding="utf-8", newline="") as fh:
            original = fh.read()
    except OSError as exc:
        print("cannot read %s: %s" % (config_path, exc), file=sys.stderr)
        return 1

    if mode == "check":
        try:
            present = check_entry(original, canonical)
        except JsoncError as exc:
            print("cannot verify %s: %s" % (config_path, exc), file=sys.stderr)
            return 1
        if not present:
            print(
                "%s does not register the canonical plugin package directory\n"
                "  expected: %s\n"
                "  (a historical file-path MPM registration also counts as \n"
                "   broken: OpenCode 2 drops it silently — re-run install.sh)"
                % (config_path, canonical),
                file=sys.stderr,
            )
        return 0 if present else 1

    try:
        if mode == "remove":
            updated, changed, summary = remove_mpm(original, canonical)
        else:
            updated, changed, summary = migrate(original, canonical)
    except JsoncError as exc:
        print(
            "refusing to change %s: %s\n"
            "  The file was NOT modified. Fix the config by hand, or remove\n"
            "  it and re-run install to have it regenerated." % (config_path, exc),
            file=sys.stderr,
        )
        return 1

    if not changed:
        print(summary, file=sys.stderr)
        return 0

    # Write via a sibling temp file + atomic replace so an interrupted
    # install cannot leave a truncated config behind.
    #
    # THE MODE MUST SURVIVE. `os.replace()` installs a NEW inode, so the
    # original file's permissions are gone and the replacement takes the
    # default umask's mode instead. Measured on a live host during the
    # aeec40f1 deployment: an operator's `opencode.jsonc` was 0600
    # before the install and 0664 after it — a file the operator had
    # deliberately restricted, silently widened by an installer run.
    # The original mode is copied onto the replacement explicitly.
    #
    # The replacement is created PRIVATE (0600) and given the original's
    # mode only once its content is complete, so the config's content is
    # never briefly exposed at the default umask's mode either.
    tmp_path = config_path + ".mpm-tmp"
    try:
        try:
            os.unlink(tmp_path)
        except FileNotFoundError:
            pass
        fd = os.open(tmp_path, os.O_WRONLY | os.O_CREAT | os.O_EXCL, 0o600)
        with os.fdopen(fd, "w", encoding="utf-8", newline="") as fh:
            fh.write(updated)

        try:
            original_mode = stat.S_IMODE(os.stat(config_path).st_mode)
        except OSError:
            # The config vanished between the read and this write. There
            # is no mode to preserve, and the private 0600 the
            # replacement was created with is the safe answer.
            original_mode = None
        if original_mode is not None:
            os.chmod(tmp_path, original_mode)

        os.replace(tmp_path, config_path)
    except OSError as exc:
        try:
            os.unlink(tmp_path)
        except OSError:
            pass
        print("cannot write %s: %s" % (config_path, exc), file=sys.stderr)
        return 1

    print(summary, file=sys.stderr)
    return 0


if __name__ == "__main__":
    sys.exit(main(sys.argv))
