#!/usr/bin/env python3
"""read a JSONC file (JSON with // comments and trailing commas).

The naive `re.sub(r'//.*', '', text)` breaks URLs — and the OpenCode
plugin registration IS a file:// URL. A host config that cannot be read
back is a host config nobody can verify, so the parser has to be right.

Usage: read_jsonc.py <path> <key.path>
   e.g. read_jsonc.py ~/.config/opencode/opencode.jsonc plugins.0
"""
import json
import sys


def strip_jsonc(text: str) -> str:
    out = []
    i = 0
    n = len(text)
    in_string = False
    while i < n:
        c = text[i]
        if in_string:
            out.append(c)
            if c == "\\" and i + 1 < n:
                out.append(text[i + 1])
                i += 2
                continue
            if c == '"':
                in_string = False
            i += 1
            continue
        if c == '"':
            in_string = True
            out.append(c)
            i += 1
            continue
        if c == "/" and i + 1 < n and text[i + 1] == "/":
            while i < n and text[i] != "\n":
                i += 1
            continue
        if c == "/" and i + 1 < n and text[i + 1] == "*":
            i += 2
            while i + 1 < n and not (text[i] == "*" and text[i + 1] == "/"):
                i += 1
            i += 2
            continue
        out.append(c)
        i += 1
    return "".join(out)


def strip_trailing_commas(text: str) -> str:
    out = []
    in_string = False
    i = 0
    n = len(text)
    while i < n:
        c = text[i]
        if in_string:
            out.append(c)
            if c == "\\" and i + 1 < n:
                out.append(text[i + 1])
                i += 2
                continue
            if c == '"':
                in_string = False
            i += 1
            continue
        if c == '"':
            in_string = True
            out.append(c)
            i += 1
            continue
        if c == ",":
            j = i + 1
            while j < n and text[j] in " \t\r\n":
                j += 1
            if j < n and text[j] in "}]":
                i += 1
                continue
        out.append(c)
        i += 1
    return "".join(out)


def main() -> int:
    if len(sys.argv) < 2:
        print("usage: read_jsonc.py <path> [key.path]", file=sys.stderr)
        return 2
    raw = open(sys.argv[1], encoding="utf-8").read()
    raw = strip_trailing_commas(strip_jsonc(raw))
    doc = json.loads(raw)
    if len(sys.argv) < 3:
        print(json.dumps(doc))
        return 0
    cur = doc
    for part in sys.argv[2].split("."):
        if isinstance(cur, list):
            cur = cur[int(part)]
        else:
            cur = cur[part]
    print(cur if isinstance(cur, str) else json.dumps(cur))
    return 0


if __name__ == "__main__":
    sys.exit(main())
