#!/usr/bin/env python3
"""jsonc_config.py — safe migration of a user's opencode.jsonc `plugin` array.

Why this module exists
----------------------
`opencode.jsonc` is JSONC, not JSON. Users put `//` comments and trailing
commas in it — that is the entire reason the `.jsonc` extension exists.
Reading it with `json.loads` therefore fails on a perfectly valid config,
and the previous installer treated that failure as "nothing to do" and
exited 0. The shell then reported the migration as successful, so a user
with a commented config got a silent no-op: the stale `opencode-mpm` entry
survived and the plugin loader broke.

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
   value in the comment-stripped text and splice only that span. Offsets
   are preserved by blanking comments in place, so a span found in the
   stripped text indexes the original text exactly.

Idempotency is structural: if the computed plugin list already equals the
current one, the file is not written at all. A second install is therefore
byte-identical rather than merely semantically equal.

CLI
---
    python3 jsonc_config.py <config-path> <canonical-entry>

Exit codes:
    0  migration applied, or already correct (nothing written)
    1  config could not be parsed or migrated safely — nothing written
"""

from __future__ import annotations

import json
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
            return (value_start, value_end, key_indent_start)

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

        # Comment between this entry and the next, on the same line.
        line_end = original.find("\n", entry_end)
        line_end = len(original) if line_end == -1 else line_end
        tail = original[entry_end:line_end]
        pos = tail.find("//")
        if pos != -1:
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


def _render_plugin_value(plugins: list[str], indent: str, comments: dict, newline: str = "\n") -> str:
    """Render the plugin array at the given indentation.

    Written inline (not via json.dump of the whole document) so the rest of
    the file is untouched. Each entry keeps any trailing comment it had.
    """
    inner = indent + "  "
    lines = ["["]
    # No trailing comma on the final entry: JSONC would accept one, but
    # strict-JSON consumers (and json.loads in the test suite, and other
    # tooling that reads this file) would not. A trailing comma here was a
    # regression this renderer introduced and then had to be taught out of.
    last = len(plugins) - 1
    for index, entry in enumerate(plugins):
        note = ""
        for candidate in comments.get(entry, []):
            note = "  " + candidate
            break
        comma = "" if index == last else ","
        lines.append("%s%s%s%s" % (inner, json.dumps(entry), comma, note))
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


def migrate(text: str, canonical: str) -> tuple[str, bool, str]:
    """Migrate stale `opencode-mpm` entries to `canonical`.

    Returns (new_text, changed, summary). When nothing needs to change the
    original text is returned unchanged, so a repeated install is a
    byte-identical no-op rather than a rewrite. `summary` describes what
    actually happened, so the installer does not report a migration that
    did not occur.

    Raises JsoncError if the config cannot be parsed or walked safely. The
    caller must not write anything in that case.
    """
    # Detach a BOM before any offset arithmetic, and restore it on the way
    # out. Doing this here rather than only in the CLI keeps the library
    # correct for any caller, and keeps a single owner for the behaviour.
    bom = ""
    if text.startswith(_BOM):
        bom, text = _BOM, text[1:]

    data = parse(text)
    if not isinstance(data, dict):
        raise JsoncError("config root must be a JSON object")

    newline = detect_newline(text)

    existing = data.get("plugin", [])
    # A non-list `plugin` is a hard error, not something to coerce. The
    # previous behaviour wrapped a bare string in a list and re-rendered,
    # which both invented structure the operator did not write and glued
    # the closing brace onto the array ("]}")  because the spliced span was
    # a string with no brackets to absorb the layout. Refusing is
    # non-destructive and tells the operator exactly what to fix.
    if "plugin" in data and not isinstance(existing, list):
        raise JsoncError(
            "'plugin' must be an array of strings, found %r — refusing to "
            "guess at the intended shape; rewrite it as a JSON array by "
            "hand and re-run"
            % type(existing).__name__
        )

    # Recognise the legacy path by directory component, not by raw
    # substring: a sandbox path like /tmp/mpm-opencode-debug-XYZ/...
    # contains 'mpm-opencode' as a prefix and must NOT be misread as a
    # stale entry.
    new_plugins: list[str] = []
    replaced = 0
    for entry in existing:
        is_legacy = (
            isinstance(entry, str)
            and (
                "/opencode-mpm/" in entry
                or entry.endswith("/opencode-mpm")
                or entry.endswith("opencode-mpm/dist/index.js")
            )
        )
        if is_legacy:
            new_plugins.append(canonical)
            replaced += 1
        else:
            new_plugins.append(entry)

    added = False
    if canonical not in new_plugins:
        new_plugins = [canonical] + [p for p in new_plugins if p != canonical]
        added = True

    if not replaced and not added:
        return bom + text, False, "already canonical"

    stripped = strip_jsonc(text)
    span = find_top_level_value_span(stripped, "plugin")

    if span is None:
        # No `plugin` key yet — insert one at the top of the object.
        open_brace = _find_root_open_brace(stripped)
        empty = _root_is_empty(stripped, open_brace)
        root_indent = _indent_of(text, open_brace)
        indent = root_indent + "  "
        rendered = _render_plugin_value(new_plugins, indent, {}, newline)
        block = '%s%s"plugin": %s%s' % (newline, indent, rendered, "" if empty else ",")
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
        return bom + updated, True, "added %s to a config that had no 'plugin' array" % canonical

    value_start, value_end, key_start = span
    indent = _indent_of(text, key_start)
    comments = _entry_comments(text, stripped, span)
    rendered = _render_plugin_value(new_plugins, indent, comments, newline)
    updated = text[:value_start] + rendered + text[value_end:]

    if replaced:
        summary = "replaced %d legacy 'opencode-mpm' entr%s with %s" % (
            replaced,
            "y" if replaced == 1 else "ies",
            canonical,
        )
    else:
        summary = "added %s (no legacy entry was present to replace)" % canonical
    return bom + updated, True, summary


def check_entry(text: str, canonical: str) -> bool:
    """True when the top-level `plugin` array already lists `canonical`.

    Raises JsoncError when the config cannot be parsed — an unreadable
    config must never be reported as a passing verification.
    """
    data = parse(text)
    if not isinstance(data, dict):
        raise JsoncError("config root must be a JSON object")
    entries = data.get("plugin", [])
    if isinstance(entries, str):
        entries = [entries]
    if not isinstance(entries, list):
        raise JsoncError("'plugin' is neither a string nor an array")
    return canonical in entries


def main(argv: list[str]) -> int:
    check = False
    args = list(argv[1:])
    if args and args[0] == "--check-entry":
        check = True
        args = args[1:]

    if len(args) != 2:
        print(
            "usage: jsonc_config.py [--check-entry] <config-path> <canonical-entry>",
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

    # BOM handling lives in migrate()/parse(), so the library is correct
    # for any caller and there is a single owner for the behaviour.
    if check:
        try:
            present = check_entry(original, canonical)
        except JsoncError as exc:
            print("cannot verify %s: %s" % (config_path, exc), file=sys.stderr)
            return 1
        return 0 if present else 1

    try:
        updated, changed, summary = migrate(original, canonical)
    except JsoncError as exc:
        print(
            "refusing to migrate %s: %s\n"
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
    tmp_path = config_path + ".mpm-tmp"
    try:
        with open(tmp_path, "w", encoding="utf-8", newline="") as fh:
            fh.write(updated)
        import os

        os.replace(tmp_path, config_path)
    except OSError as exc:
        try:
            import os

            os.unlink(tmp_path)
        except OSError:
            pass
        print("cannot write %s: %s" % (config_path, exc), file=sys.stderr)
        return 1

    print(summary, file=sys.stderr)
    return 0


if __name__ == "__main__":
    sys.exit(main(sys.argv))