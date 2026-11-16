"""
test_reviewer_isolation_denies_every_mpm_tool.py — a reviewer subagent
must not be able to reach the production MPM substrate.

WHY THIS EXISTS
---------------
A review subagent runs in a child session with the parent's tool
surface. On 2026-11-15 a review subagent wrote a memory into the
operator's LIVE MPM database through the `mpm_memory` tool, against an
explicit instruction not to touch it.

A written instruction was not enough, and could not have been: the tool
was still REGISTERED for the reviewer, so "do not call it" was the only
thing standing between a review note and a write into the operator's
substrate.

The fix is the PERMISSION layer. The reviewer agent definition
(`agent_installation/mpm-opencode/templates/agents/mpm-reviewer.md`)
denies every MPM tool by name, and a denied tool is removed from the
reviewer's tool list entirely - the reviewer cannot call it and cannot
see it.

The isolation is LAYERED, and this file pins every layer:

  1. PERMISSION — every tool the REGISTRY publishes is denied by name,
     so a new tool cannot be added without a matching deny rule.
  2. PERMISSION — the MPM BINARIES are refused wherever they are
     NAMED — at the command position, as their own word, as the last
     segment of a path, and inside a quote — so the command line
     cannot reach the substrate the tool surface closed.
  3. PERMISSION — a live MPM workspace cannot be opened with the read
     tool, and the reviewer's own state changes are closed.
  4. ENVIRONMENT — a disposable sandbox (HOME, TMPDIR, MPM_WORKSPACE)
     so that a route the rules do not name still cannot write into the
     operator's runtime root.
  5. INSTALL — the definition is installed by the canonical install
     flow, and the installed copy may not drift from the template.

WHAT THIS PINS
--------------
  1. Every tool the REGISTRY publishes is denied to the reviewer. The
     registry (`internal/core/tools/registry_list.go`) is the single
     authoritative list of every MPM tool, so a NEW tool cannot be
     added without the deny list being extended - the reviewer's
     isolation cannot quietly erode.
  2. The deny list has no STALE entries: a rule for a tool that no
     longer exists is dead weight that hides a real gap.
  3. The reviewer cannot EDIT - a reviewer that could change what it
     reviews is not a reviewer - and cannot spawn an agent, ask the
     operator a question, or reach Code Mode. The capability LOADER
     is deliberately NOT in this list: the host does not gate the
     `skill` tool by any permission action, so a rule for it would
     claim a closure that does not exist. The definition says so in
     its own prose instead, and this file's own claims are checked
     against that.
  4. The reviewer cannot reach the substrate from the COMMAND LINE
     either, and cannot read a live MPM workspace with the read tool.
  5. The comparison is not vacuous: the registry must parse into a
     non-zero set of tools, the reviewer must deny a non-zero number of
     them, and the registry parse must not DROP a name.
  6. The definition is INSTALLED by the canonical flow, and the
     installed copy must not differ from the template.

THE REGISTRY IS THE AUTHORITY
------------------------------
`internal/core/tools/registry_list.go` is the same list the MCP server
and `mpm call` both iterate. The PLUGIN registers a subset of it (it
omits the tools an agent reaches through `mpm call`), so the deny list
covers the registry's whole set: the plugin's subset by permission, and
the rest by the shell rules that close the `mpm` binaries.
"""

from __future__ import annotations

import importlib.util
import re
import re
import unittest
from pathlib import Path
from pathlib import Path

REPO_ROOT = Path(__file__).resolve().parent.parent.parent
REGISTRY = REPO_ROOT / "internal" / "core" / "tools" / "registry_list.go"
REVIEWER = (
    REPO_ROOT
    / "agent_installation"
    / "mpm-opencode"
    / "templates"
    / "agents"
    / "mpm-reviewer.md"
)
INSTALLER = (
    REPO_ROOT
    / "agent_installation"
    / "mpm-opencode"
    / "scripts"
    / "install_reviewer_agent.py"
)
SANDBOX = (
    REPO_ROOT
    / "agent_installation"
    / "mpm-opencode"
    / "scripts"
    / "reviewer_sandbox.py"
)
INSTALL_SH = REPO_ROOT / "agent_installation" / "mpm-opencode" / "install.sh"
MAKEFILE = REPO_ROOT / "Makefile"

# The Makefile variables that name the MPM binaries. The reviewer's
# shell rules are checked against the Makefile's OWN list, so a new
# binary cannot be built without the deny list being extended.
MPM_BINARY_VARIABLES = (
    "BINARY_NAME",
    "MCP_BINARY",
    "SCHED_BINARY",
    "CRITIC_BINARY",
    "TELEMETRY_BINARY",
    "UPDATE_BINARY",
    "LINT_BINARY",
)


def _mpm_binaries() -> list[str]:
    """The MPM binaries the Makefile builds."""
    source = MAKEFILE.read_text(encoding="utf-8")
    binaries: list[str] = []
    for variable in MPM_BINARY_VARIABLES:
        match = re.search(
            rf"^{variable}\s*:?=\s*(\S+)\s*$", source, re.MULTILINE
        )
        if match is None:
            raise AssertionError(
                f"the Makefile does not define {variable}; the reviewer's "
                "binary deny list cannot be checked against a list that is "
                "no longer the repository's own"
            )
        binaries.append(match.group(1))
    return binaries


def _registry_names() -> list[str]:
    """Every `Name:` entry in the registry, whatever it contains."""
    source = REGISTRY.read_text(encoding="utf-8")
    return re.findall(r'Name:\s*"([^"]+)"', source)


def _registry_tools() -> set[str]:
    """The names the registry publishes.

    The character class is the FULL tool-name alphabet. A narrower class
    would silently DROP a name whose spelling falls outside it - and a
    dropped name is not compared, so no deny rule would be required for
    it, and the tool would stay reachable while the guard still passed.
    `test_the_registry_parse_drops_no_name` is the backstop.
    """
    source = REGISTRY.read_text(encoding="utf-8")
    return set(re.findall(r'Name:\s*"(mpm_[A-Za-z0-9_]+)"', source))


def _permission_rules() -> list[dict[str, str]]:
    """The reviewer's permission rules, parsed from its frontmatter.

    The frontmatter is a flat list of three-key mappings, so it is
    parsed directly rather than through a YAML dependency: the
    agent_installation suite runs on the standard library alone.
    """
    text = REVIEWER.read_text(encoding="utf-8")
    if not text.startswith("---\n"):
        raise AssertionError("the reviewer agent has no frontmatter")
    end = text.find("\n---\n", 4)
    if end < 0:
        raise AssertionError("the reviewer agent's frontmatter is unterminated")
    frontmatter = text[4:end]

    # The value is unescaped the way the host's YAML reader unescapes
    # it: a double-quoted scalar's `\"` is a literal quote, its `\\`
    # is a literal backslash and its `\t` is a literal TAB. Without
    # that, a rule whose literal contains a quote or a tab would be
    # compared with the escape still on it, and the comparison would
    # fail for the wrong reason.
    #
    # The `\t` escape is MEASURED, not assumed: the host's reader
    # expands it, and a command whose text carries a literal tab is
    # refused by the tab rules -- which is why those rules exist.
    def unescape(value: str) -> str:
        return (
            value.replace('\\"', '"')
            .replace("\\\\", "\\")
            .replace("\\t", "\t")
        )

    rules: list[dict[str, str]] = []
    current: dict[str, str] = {}
    for line in frontmatter.splitlines():
        item = re.match(r"^\s*-\s+(\w+):\s*(.+?)\s*$", line)
        if item:
            if current:
                rules.append(current)
            current = {item.group(1): unescape(item.group(2).strip('"'))}
            continue
        field = re.match(r"^\s+(\w+):\s*(.+?)\s*$", line)
        if field and current:
            current[field.group(1)] = unescape(field.group(2).strip('"'))
    if current:
        rules.append(current)
    return rules


def _denied_actions() -> set[str]:
    """The actions the reviewer denies."""
    return {
        rule["action"]
        for rule in _permission_rules()
        if rule.get("effect") == "deny"
    }


def _shell_rules():
    """Every shell rule, as (resource, effect) pairs, in file order.

    The LAST matching rule wins, exactly as the host's `findLast` does,
    so the ORDER matters and is preserved here.
    """
    rules = []
    for rule in _permission_rules():
        if rule.get("action") != "shell":
            continue
        rules.append((rule.get("resource", ""), rule.get("effect", "allow")))
    return rules


def _shell_denials() -> set[str]:
    """The shell resources the reviewer denies."""
    return {
        rule.get("resource", "")
        for rule in _permission_rules()
        if rule.get("action") == "shell" and rule.get("effect") == "deny"
    }


def _read_denials() -> set[str]:
    """The read resources the reviewer denies."""
    return {
        rule.get("resource", "")
        for rule in _permission_rules()
        if rule.get("action") == "read" and rule.get("effect") == "deny"
    }


def _load_installer():
    """The installer module, loaded by path."""
    spec = importlib.util.spec_from_file_location(
        "install_reviewer_agent", INSTALLER
    )
    if spec is None or spec.loader is None:
        raise AssertionError("the reviewer installer could not be loaded")
    module = importlib.util.module_from_spec(spec)
    spec.loader.exec_module(module)
    return module


# ── A model of the host's shell matcher ─────────────────────────────────
#
# The guard above checks that certain RULE STRINGS are present.  That
# cannot tell a rule that WORKS from a rule that is merely spelled the
# right way: the `*/mpm ?*` form was present, the guard was green, and
# `go run ./cmd/mpm` matched no rule at all and RAN the binary.
#
# So the guard also models the host's matcher and asserts the VERDICT
# for a corpus whose outcomes are all MEASURED.  The model is
# transcribed from the host binary (opencode v2.3.0):
#
#   function U2(e, r) {
#     let a = e.replaceAll("\\", "/"),
#         s = r.replaceAll("\\", "/")
#           .replace(/[.+^${}()|[\]\\]/g, "\\$&")
#           .replace(/\*/g, ".*")
#           .replace(/\?/g, ".");
#     if (s.endsWith(" .*")) s = s.slice(0, -3) + "( .*)?";
#     return new RegExp("^" + s + "$", "s").test(a);
#   }
#
# and the last matching rule wins.  The command text is split into
# simple commands on `&&`, `||`, `;`, `|`, a single `&` and a
# NEWLINE, and each segment is TRIMMED before it is matched; a `cd`
# segment's text is not matched; `$( ... )` and BACKTICK substitutions
# are descended into, as are the ones inside DOUBLE quotes.
#
# The host REFUSES a command its scanner cannot ANALYSE: a process
# substitution (`echo <(mpm --version)>` -> "invalid-redirect", and so
# is `echo <(echo hello)>`, which names nothing at all), a compound
# command (`echo (mpm --version)` -> "compound-command", and so is
# `echo (hello)`) and a REDIRECTION with an EMPTY TARGET (`echo mpm>`
# and `echo hi >`, and the second names nothing at all). Such a
# command does not run, but the reason is the scanner's own limit and
# not an MPM rule - so the model reproduces the VERDICT and names the
# right reason.
#
# A `case` STATEMENT is one construct whose HEADER - the pattern list
# included - is not matched, exactly like a loop's header. MEASURED:
# `case x in mpm) echo hi;; esac` is ALLOWED, and so is
# `case x in "mpm") echo hi;; esac`, while the BODY is matched
# (`case x in *) mpm --version;; esac` is REFUSED).
#
# A CONTEXT - a `[[ ... ]]` conditional or an arithmetic context - is
# a construct only when it IS the command. MEASURED: `[[ mpm == x ]]`
# and `(( mpm ))` are ALLOWED, while `echo $(( mpm ))` and
# `echo [[ mpm == x ]]` are REFUSED - in ARGUMENT position the text is
# ordinary text and the word shape matches it.
#
# A HERE-DOCUMENT's BODY is data for the command analysis and still
# TEXT for the rule matcher: `cat <<EOF` / `mpm` / `EOF` is ALLOWED
# (the body is not a command), `cat <<EOF` / `echo mpm` / `EOF` is
# ALLOWED ("mpm" at the end of a line is not `* mpm *`), and
# `cat <<EOF` / `echo mpm --version` / `EOF` is REFUSED (the rule
# matcher sees the body).
#
# A CONTEXT the host cannot parse is UNANALYSABLE: `[[ x; y ]]`,
# `((x;y))` and an unclosed `[[ x` are REFUSED (invalid-structure).
#
# A MALFORMED CONSTRUCT is UNANALYSABLE too, and every form of it
# names NO binary - so the refusal is about the construct: a `case`
# with no subject, an empty pattern list, no `esac` or trailing text
# after it; an `if`/`while`/`until` with an empty condition; an empty
# group - `( )` or `{ }`; an unclosed substitution - `$(` or `<(`; and
# a bare function definition - `f()`.
#
# TRAILING TEXT after a closed compound construct is refused too: an
# `if ... fi`, a brace group, a subshell and a function body that are
# followed by anything but a separator are constructs the host cannot
# analyse (`invalid-structure`).
#
# MEASURED twice and independently: the review's own round and a
# dedicated measurement round - thirteen malformed forms and their
# thirteen well-formed counterparts, three isolated repeats every one,
# every verdict STABLE and every reason text identical. The six
# reasons the host gives for this class are `compound-command` (the
# malformed `case`, the empty condition), `invalid-structure` (the
# empty group, the trailing text after an `esac`),
# `command-substitution` (the unclosed substitution) and
# `dynamic-execution` (the bare function).
#
# A `#` COMMENT is not text: the host strips it before it matches, so
# `echo hi # mpm` is ALLOWED.
#
# A PROCESS SUBSTITUTION is a COMMAND: the scanner parses its content
# and the names inside are matched. MEASURED: `echo <(mpm --version)`
# is REFUSED and `echo <(echo hello)` is ALLOWED - but an ARITHMETIC
# context is not analysed, so `(( <(mpm --version) ))` is ALLOWED.
#
# A HERE-STRING (`<<<`) is not a here-document: its target is a word,
# so `cat <<< "hi there"` is ALLOWED. A here-document inside a loop
# body or a `case` branch body is ALLOWED too: the body is data, and
# the re-split must not scan it again.
#
# A SUBSHELL at the command position is ANALYSED, and the command
# inside it is matched: `(echo hi)` is ALLOWED and `(mpm --version)`
# is REFUSED - by a RULE, not by the scanner. A `(` that follows a
# WORD is a compound command and is refused as `compound-command`
# (`echo (hello)`).
#
# The `&` of a REDIRECT OPERATOR is not a separator: `2>&1`
# duplicates a descriptor and `&>` redirects both streams, and the
# host parses each as ONE operator (`echo hi 2>&1` is ALLOWED).
#
# The model is a MODEL.  Every outcome in the corpus below was measured
# against the installed definition before it was written here, and a
# host that changes its matcher would need this model revisited.
_MATCHER_ESCAPE = re.compile(r"[.+^${}()|[\]\\]")


def _matcher_u2(value: str, pattern: str) -> bool:
    """Does the value match the pattern, the way the host does it?"""
    text = value.replace("\\", "/")
    expanded = pattern.replace("\\", "/")
    expanded = _MATCHER_ESCAPE.sub(r"\\\g<0>", expanded)
    expanded = expanded.replace("*", ".*").replace("?", ".")
    if expanded.endswith(" .*"):
        expanded = expanded[:-3] + "( .*)?"
    try:
        return re.match("^" + expanded + "$", text, re.DOTALL) is not None
    except re.error:
        return False


def _matcher_word_at(text: str, index: int, word: str) -> bool:
    """Is the WORD at `index` exactly `word`?

    A `case` may be NESTED, so the closing `esac` has to be found by
    counting `case` and `esac` words - and a word inside a longer
    word (`cases`, `mycase`) is not the keyword.
    """
    if not text.startswith(word, index):
        return False
    end = index + len(word)
    before = text[index - 1] if index else ""
    after = text[end] if end < len(text) else ""
    for char in (before, after):
        if char and (char.isalnum() or char in "_-."):
            return False
    return True


def _matcher_matching_esac(command: str, start: int) -> int:
    """The `esac` that closes the `case` whose body starts at `start`.

    A `case` may be NESTED, so the matching `esac` is the one at which
    the nesting returns to zero.  MEASURED:
    `case y in z) case w in q) echo hi;; esac;; esac` is ALLOWED.
    """
    depth = 0
    index = start
    length = len(command)
    while index < length:
        if _matcher_word_at(command, index, "case"):
            depth += 1
            index += 4
            continue
        if _matcher_word_at(command, index, "esac"):
            if depth == 0:
                return index
            depth -= 1
            index += 4
            continue
        index += 1
    return -1


def _matcher_split_branches(body: str):
    """The BRANCHES of a `case` body, split at the TOP LEVEL only.

    A nested `case` has its own `;;`, so the split counts the nesting.
    MEASURED: `case y in z) case w in q) echo hi;; esac;; esac` is
    ALLOWED - and the inner `;;` belongs to the INNER construct.
    """
    branches = []
    current = []
    depth = 0
    index = 0
    length = len(body)
    while index < length:
        if _matcher_word_at(body, index, "case"):
            depth += 1
            current.append("case")
            index += 4
            continue
        if _matcher_word_at(body, index, "esac"):
            if depth > 0:
                depth -= 1
            current.append("esac")
            index += 4
            continue
        if depth == 0:
            if body.startswith(";;&", index):
                branches.append("".join(current))
                current = []
                index += 3
                continue
            if body.startswith(";;", index) or body.startswith(";&", index):
                branches.append("".join(current))
                current = []
                index += 2
                continue
        current.append(body[index])
        index += 1
    branches.append("".join(current))
    return branches


def _matcher_empty_condition(segments, position: int, keyword: str) -> bool:
    """Is the CONDITION of this segment empty?

    MEASURED: `if ; then echo hi; fi`, `while ; do echo hi; done` and
    `until ; do echo hi; done` are all REFUSED with "compound-command",
    and none of them names a binary - so the refusal is about the
    CONSTRUCT.

    The keyword has to BE the condition's keyword, not an argument:
    MEASURED, `echo if; echo hi` is ALLOWED - so the keyword has to be
    the first word of a segment, and the next segment has to open the
    condition's BODY.

    A condition that is only a COMMENT is empty, because the comment
    is stripped before the command is parsed: MEASURED, `if` and then
    a comment and then `then echo hi; fi` is REFUSED.

    MEASURED: `if true; then echo hi; fi`, `while false; do echo hi;
    done` and `until false; do echo hi; done` are ALLOWED, and so is a
    condition on the next LINE (`if` and then `true;`), because the
    text between the keyword and the separator is not empty.
    """
    if keyword == "if":
        if _matcher_next_keyword(segments, position) != "then":
            return False
    else:
        if _matcher_next_keyword(segments, position) != "do":
            return False
    return True


def _matcher_empty_group(command: str) -> bool:
    """Does this command carry an EMPTY group?

    MEASURED: `( )` and `{ }` are both REFUSED with "invalid-structure",
    and neither names a binary at all - so the refusal is about the
    CONSTRUCT.

    The group is empty WHEREVER it appears: MEASURED, `{ { } }` and a
    brace group whose braces are on separate LINES are REFUSED too.

    The braces have to CARRY WHITESPACE: MEASURED, `{ }` is REFUSED
    and `{}` is ALLOWED - because a reserved word is followed by a
    separator, so `{}` is an ordinary word and not a group at all.

    The PARENS do not make that distinction: MEASURED, `()` and
    `( )` are both REFUSED with "invalid-structure".

    A function definition's PARAMETER LIST is not a group: `f()` is
    refused for a different reason - it is a bare function definition
    with no body - and `f() { echo hi; }` is ALLOWED.

    An empty COMMAND SUBSTITUTION is not a group either: MEASURED,
    `echo $( )` is ALLOWED.

    A PARAMETER EXPANSION is not a group either: `${}` is an ordinary
    parameter expansion, and `${x}` does not match the pattern at all.
    """
    return bool(_MATCHER_EMPTY_GROUP.search(command))


def _matcher_has_body(segments, position: int) -> bool:
    """Does the segment at `position` carry a BODY?

    A function definition's body is a brace group.  It may sit in the
    SAME segment - `f() { echo hi; }` - or on the next LINE: MEASURED,
    `f()` and then `{ echo hi; }` is ALLOWED, while `f()` on its own
    is REFUSED.
    """
    text = segments[position][0]
    found = re.search(r"[A-Za-z_][A-Za-z0-9_]*\s*\(\s*\)", text)
    if found and "{" in text[found.end() :]:
        return True
    for text, _ in segments[position + 1 :]:
        stripped = text.strip()
        if not stripped:
            continue
        return stripped.startswith("{")
    return False


def _matcher_bare_function(segment: str) -> bool:
    """Does this segment carry a FUNCTION DEFINITION?

    MEASURED: `f()` is REFUSED with "dynamic-execution", and it names
    no binary at all - so the refusal is about the CONSTRUCT.  The
    host sees the parameter list WHEREVER it stands: MEASURED,
    `echo f()` is REFUSED the same way.

    A definition with a BODY is fine: `f() { echo hi; }` is analysed,
    and the host matches the BODY (`f() { mpm --version; }` is refused
    by a RULE).
    """
    return bool(re.search(r"[A-Za-z_][A-Za-z0-9_]*\s*\(\s*\)", segment))


def _matcher_unclosed_substitution(segment: str) -> bool:
    """Is there a SUBSTITUTION the host cannot finish parsing?

    MEASURED: `$(` and `<(` are both REFUSED with "command-substitution",
    and neither names a binary - so the refusal is about the CONSTRUCT.

    A CLOSED substitution is fine: `$(echo hi)` and `<(echo hi)` are
    ALLOWED.

    A backtick inside SINGLE quotes is a literal - the host does not
    treat it as a substitution - so a quoted region is skipped.

    A `)` inside a QUOTED STRING does not close the substitution
    either: MEASURED, `echo <(echo "a)b"`, `echo $(echo "a)b"` and
    `echo <(echo 'a)b'` are all REFUSED with "command-substitution".
    """
    index = 0
    length = len(segment)
    while index < length:
        char = segment[index]
        if char == "'":
            end = segment.find("'", index + 1)
            if end < 0:
                return False
            index = end + 1
            continue
        if char == "`":
            end = segment.find("`", index + 1)
            if end < 0:
                return True
            index = end + 1
            continue
        if (
            segment.startswith("$(", index)
            or segment.startswith("<(", index)
            or segment.startswith(">(", index)
        ):
            depth = 0
            end = index + 1
            quote = None
            while end < length:
                char = segment[end]
                if quote is not None:
                    if char == quote:
                        quote = None
                    end += 1
                    continue
                if char in "\"'":
                    quote = char
                    end += 1
                    continue
                if char == "(":
                    depth += 1
                elif char == ")":
                    depth -= 1
                    if depth == 0:
                        break
                end += 1
            if end >= length:
                # Never closed.
                return True
            index = end + 1
            continue
        index += 1
    return False


def _matcher_is_boundary(char: str) -> bool:
    """Is this character a WORD BOUNDARY?

    A boundary is whitespace or an OPERATOR - the characters that end
    a word.
    """
    return not char or char.isspace() or char in ";|&()<>"


def _matcher_skip_heredoc(command: str, index: int) -> int:
    """The index just after a here-document's BODY, or -1.

    A here-document's BODY is DATA, not code: the host does not
    analyse it, so a `}` or an unbalanced quote inside it is not a
    structural defect.  MEASURED: `cat <<EOF`, then `}`, then `EOF` is
    ALLOWED - and so is `cat <<EOF`, then `echo 'unclosed`, then `EOF`.
    """
    length = len(command)
    look = index + 2
    strip_tabs = False
    if look < length and command[look] == "-":
        strip_tabs = True
        look += 1
    while look < length and command[look].isspace():
        look += 1
    if look < length and command[look] in "\"'":
        closer = command[look]
        close = command.find(closer, look + 1)
        delimiter = command[look + 1 : close] if close > 0 else ""
        look = close + 1 if close > 0 else length
    else:
        start = look
        while look < length and not command[look].isspace():
            look += 1
        delimiter = command[start:look]
    newline = command.find("\n", look)
    if newline < 0:
        return -1
    cursor = newline + 1
    while cursor < length:
        line_end = command.find("\n", cursor)
        if line_end < 0:
            line_end = length
        probe = command[cursor:line_end]
        if strip_tabs:
            probe = probe.lstrip("\t")
        if probe == delimiter:
            return cursor
        cursor = line_end + 1
    return -1


def _matcher_word_at_boundary(text: str, index: int, word: str) -> bool:
    """Is the WORD at `index` exactly `word`, at a word boundary?

    MEASURED: `case x in` is REFUSED with "compound-command" - so the
    `in` is a KEYWORD, and the keyword is recognised whether or not a
    space follows it.  A word boundary is what keeps `print` from
    reading as the keyword `in`.
    """
    if not text.startswith(word, index):
        return False
    before = text[index - 1] if index else ""
    after = text[index + len(word)] if index + len(word) < len(text) else ""
    return _matcher_is_boundary(before) and _matcher_is_boundary(after)


def _matcher_case_header(command: str, start: int):
    """The `case` construct's SUBJECT and the end of its `in` keyword.

    MEASURED: `case x in` is REFUSED with "compound-command", and
    `case in y) echo hi;; esac` is REFUSED too - so the construct is
    recognised by the KEYWORD `in`, and the subject is what stands
    between `case` and it.
    """
    index = start
    length = len(command)
    while index < length:
        if _matcher_word_at_boundary(command, index, "in"):
            return command[start:index], index + 2
        index += 1
    return None


def _matcher_pattern_list_close(command: str, start: int) -> int:
    """The `)` that closes a `case` PATTERN LIST, or -1.

    The pattern list may hold quoted patterns and substitutions, so
    the search skips both.  MEASURED: `case x in y) echo hi;; esac` is
    ALLOWED, so this `)` is not a bracket.
    """
    index = start
    length = len(command)
    while index < length:
        char = command[index]
        if char in "\"'":
            end = command.find(char, index + 1)
            if end < 0:
                return -1
            index = end + 1
            continue
        if char == "(":
            depth = 0
            end = index
            while end < length:
                if command[end] == "(":
                    depth += 1
                elif command[end] == ")":
                    depth -= 1
                    if depth == 0:
                        break
                end += 1
            if end >= length:
                return -1
            index = end + 1
            continue
        if char == ")":
            return index
        index += 1
    return -1


def _matcher_structure(command: str):
    """The STRUCTURAL defect this command carries, or None.

    The host's portable scanner PARSES the command, and it refuses
    what it cannot parse.  This is the structural model of that
    scanner: it walks the command the way the shell's grammar does,
    and it reports the first defect it finds.

    MEASURED - every form below names no binary at all, so the
    refusal is about the CONSTRUCT:

        echo 'unclosed            unterminated-quote
        echo "unclosed            unterminated-quote
        echo hi \\                unterminated-escape
        (                        command-substitution
        )                        compound-command
        { echo hi                invalid-structure
        }                        invalid-structure
        ;                        invalid-structure
        echo hi;;                invalid-structure
        echo a { echo hi; }      invalid-structure
        if true; then echo hi    compound-command   (no `fi`)
        while false; do echo hi  compound-command   (no `done`)
        for ; do echo hi         compound-command   (an empty header)
        if true; then fi         compound-command   (an empty body)
        case echo x in y) ...    compound-command   (a subject that is
                                                    a command)

    Six things the scanner knows, all MEASURED:

    - a here-document's BODY is DATA, so it is skipped entirely;
    - a quote must be CLOSED - a quote pair inside a longer word is
      fine, because the host removes the quotes and continues;
    - a `;` must be PRECEDED by a command, and the slot between two
      `;` is empty - but a TRAILING empty slot is fine, and inside a
      `case` the `;;` separates two case ITEMS;
    - a `{` opens a GROUP only when it carries whitespace after it,
      so `{}` is an ordinary word and `{ }` is an empty group;
    - a GROUP is a COMPOUND COMMAND, so it may only stand where a
      command is expected - in command position, or as the body of a
      function definition or of a `coproc` declaration;
    - a `case` construct is recognised by its KEYWORD `in` and not by
      the text " in ", so `case x in` is refused, while `case x in
      esac` - an empty pattern list closed by its own `esac` - is
      allowed.
    """
    length = len(command)
    index = 0
    # Each frame is one nesting level of the command: the top level, a
    # brace GROUP, a `case` BODY, a SUBSHELL or a SUBSTITUTION.  The
    # frame tracks whether the current COMMAND SLOT is empty, and
    # where the slot began.
    frames = [{"kind": "top", "empty": True, "seen": True, "start": 0}]
    compound = []      # the compound commands that are still open
    expect_command = True

    while index < length:
        char = command[index]

        # ── a here-document's BODY is DATA ──
        if (
            char == "<"
            and command.startswith("<<", index)
            and not command.startswith("<<<", index)
        ):
            after = _matcher_skip_heredoc(command, index)
            if after < 0:
                return "heredoc"
            index = after
            expect_command = True
            continue

        # ── an escape ──
        if char == "\\":
            if index + 1 >= length:
                return "unterminated-escape"
            index += 2
            frames[-1]["empty"] = False
            expect_command = False
            continue

        # ── a comment ──
        if char == "#" and (index == 0 or _matcher_is_boundary(command[index - 1])):
            end = command.find("\n", index)
            index = length if end < 0 else end
            continue

        # ── a NEWLINE is a separator, but it does not need a command
        #    before it: `if` and then `true;` on the next line is
        #    ALLOWED, and so is the slot after a `;` ──
        if char == "\n":
            index += 1
            frames[-1]["empty"] = True
            frames[-1]["start"] = index
            expect_command = True
            continue

        # ── whitespace ──
        if char.isspace():
            index += 1
            continue

        # ── a SEPARATOR ──
        if char in ";&|":
            if char == "&" and index + 1 < length and command[index + 1] == "&":
                index += 2
            elif char == "|" and index + 1 < length and command[index + 1] == "|":
                index += 2
            else:
                index += 1
            if frames[-1]["empty"] and frames[-1]["kind"] != "case":
                # An EMPTY command slot.  MEASURED: `;`, `;;` and
                # `echo hi;;` are REFUSED with "invalid-structure",
                # while `echo hi;` is ALLOWED - a TRAILING empty slot
                # is fine - and `case x in y) echo hi;; esac` is
                # ALLOWED, because inside a `case` the `;;` separates
                # two case ITEMS.
                #
                # The same rule explains a brace group: `{ ; }` and
                # `{ ; echo hi; }` are REFUSED because the group's
                # FIRST slot is empty, while `{ echo hi; }` is
                # ALLOWED.
                return "invalid-structure"
            frames[-1]["empty"] = True
            frames[-1]["start"] = index
            expect_command = True
            continue

        # ── a redirection operator ──
        if char in "<>":
            if char == "<" and command.startswith("<<<", index):
                # A here-STRING is ONE operator: `cat <<< "hi there"` is
                # ALLOWED, and the two `<` that are left over must not
                # be read as a here-document opener.
                index += 3
            else:
                index += 1
            frames[-1]["empty"] = False
            expect_command = True
            continue

        # ── a SUBSHELL or a SUBSTITUTION ──
        if char == "(":
            previous = command[index - 1] if index else ""
            # NOTE: `"" in "$<>"` is TRUE in Python, so the guard has
            # to test the character, not the membership.
            if previous and previous in "$<>":
                kind = "subst"
            elif previous and not previous.isspace():
                # A PARAMETER LIST, not a group: `f()` is a function
                # definition's parameter list, and it is not an empty
                # subshell.  MEASURED: `f()` is REFUSED with
                # "dynamic-execution" - a different reason - and
                # `f() { echo hi; }` is ALLOWED.
                kind = "params"
            else:
                kind = "paren"
            frames.append({"kind": kind, "empty": True, "seen": False, "start": index + 1})
            index += 1
            expect_command = True
            continue
        if char == ")":
            if len(frames) == 1:
                # An unmatched `)`.  MEASURED: `)` and `echo )` are
                # REFUSED with "compound-command".
                return "compound-command"
            frame = frames.pop()
            if frame["kind"] == "brace" or frame["kind"] == "case":
                return "invalid-structure"
            if frame["kind"] == "paren" and frame["empty"]:
                # An EMPTY subshell.  MEASURED: `()` and `( )` are
                # REFUSED with "invalid-structure", while
                # `echo $( )` is ALLOWED - a SUBSTITUTION is not a
                # group.
                return "invalid-structure"
            frames[-1]["empty"] = False
            frames[-1]["seen"] = True
            index += 1
            expect_command = True
            continue

        # ── a GROUP ──
        if char == "{":
            following = command[index + 1] if index + 1 < length else ""
            if following.isspace() and (
                index == 0 or _matcher_is_boundary(command[index - 1])
            ):
                # The braces have to CARRY WHITESPACE: MEASURED,
                # `{ }` is REFUSED and `{}` is ALLOWED, because a
                # reserved word is followed by a separator.
                if not _matcher_group_stands_where_a_command_may_stand(
                    command, frames[-1]["start"], index
                ):
                    # A GROUP is a COMPOUND COMMAND, so it may only
                    # stand where a command is expected.  MEASURED:
                    # `echo a { echo hi; }` and `true { echo hi; }`
                    # are REFUSED with "invalid-structure", while
                    # `{ echo hi; }` and
                    # `echo "a"; { echo hi; }` are ALLOWED.
                    return "invalid-structure"
                frames.append(
                    {"kind": "brace", "empty": True, "seen": False, "start": index + 1}
                )
                index += 1
                expect_command = True
                continue
            frames[-1]["empty"] = False
            index += 1
            expect_command = False
            continue
        if char == "}":
            if len(frames) > 1 and frames[-1]["kind"] == "brace":
                frame = frames.pop()
                if not frame["seen"]:
                    # An EMPTY group.  MEASURED: `{ }` is REFUSED with
                    # "invalid-structure".
                    return "invalid-structure"
                frames[-1]["empty"] = False
                frames[-1]["seen"] = True
                index += 1
                expect_command = True
                continue
            if index == 0 or _matcher_is_boundary(command[index - 1]):
                # An unmatched `}`.  MEASURED: `}` and `echo }` are
                # REFUSED with "invalid-structure".
                return "invalid-structure"
            frames[-1]["empty"] = False
            index += 1
            expect_command = False
            continue

        # ── a WORD ──
        end = index
        while end < length:
            c = command[end]
            if c.isspace() or c in ";|&()<>":
                break
            if c == "\\":
                end += 2
                continue
            if c in "\"'":
                closer = command.find(c, end + 1)
                if closer < 0:
                    # A quote that is never closed.  MEASURED:
                    # `echo 'unclosed` and `echo "unclosed` are
                    # REFUSED with "unterminated-quote".
                    return "unterminated-quote"
                end = closer + 1
                continue
            end += 1
        word = command[index:end]
        index = end
        frames[-1]["empty"] = False
        frames[-1]["seen"] = True

        # ── the `case` CONSTRUCT ──
        if expect_command and word == "case":
            header = _matcher_case_header(command, end)
            if header is not None:
                subject, in_end = header
                if _matcher_subject_is_command(subject):
                    # A SUBJECT that is a COMMAND is a construct the
                    # host cannot analyse.  MEASURED:
                    # `case echo x in y) echo hi;; esac` is REFUSED with
                    # "compound-command", and it names no binary at all.
                    return "compound-command"
                close = _matcher_pattern_list_close(command, in_end)
                if close < 0:
                    # The PATTERN LIST has no `)`.  MEASURED:
                    # `case x in esac` is ALLOWED - the construct is
                    # closed by its own `esac` - while `case x in` and
                    # `case x in y z` are REFUSED with
                    # "compound-command", because nothing closes them.
                    esac_at = _matcher_matching_esac(command, in_end)
                    if esac_at < 0 or command[in_end:esac_at].strip():
                        return "compound-command"
                    trailing = command[esac_at + 4 :].lstrip()
                    if trailing and trailing[0] not in ";&)|}\n":
                        # Only a SEPARATOR may follow the `esac`.
                        # MEASURED: `case x in esac extra` is REFUSED
                        # with "invalid-structure", and it names no
                        # binary at all.
                        return "invalid-structure"
                    index = esac_at + 4
                    expect_command = False
                    continue
                frames.append(
                    {"kind": "case", "empty": True, "seen": False, "start": close + 1}
                )
                index = close + 1
                expect_command = True
                continue

        # ── the COMPOUND COMMANDS ──
        if expect_command:
            if word == "esac":
                if len(frames) > 1 and frames[-1]["kind"] == "case":
                    frames.pop()
                    frames[-1]["empty"] = False
                expect_command = False
                continue
            if word == "if":
                compound.append({"kind": "if", "phase": "cond", "seen": False})
            elif word == "while" or word == "until":
                compound.append({"kind": "loop", "phase": "cond", "seen": False})
            elif word == "for" or word == "select":
                compound.append({"kind": "loop", "phase": "header", "seen": False})
            elif word == "then":
                if (
                    not compound
                    or compound[-1]["kind"] != "if"
                    or compound[-1]["phase"] != "cond"
                ):
                    return "compound-command"
                compound[-1]["phase"] = "body"
                compound[-1]["seen"] = False
            elif word == "do":
                if not compound or compound[-1]["kind"] != "loop":
                    return "compound-command"
                if compound[-1]["phase"] == "body":
                    return "compound-command"
                if compound[-1]["phase"] == "header" and not compound[-1]["seen"]:
                    # An EMPTY header.  MEASURED: `for ; do echo hi;
                    # done` and `select ; do echo hi; done` are REFUSED
                    # with "compound-command".
                    return "compound-command"
                compound[-1]["phase"] = "body"
                compound[-1]["seen"] = False
            elif word == "else" or word == "elif":
                if not compound or compound[-1]["kind"] != "if":
                    return "compound-command"
                compound[-1]["phase"] = "body"
                compound[-1]["seen"] = False
            elif word == "fi":
                if not compound or compound[-1]["kind"] != "if":
                    return "compound-command"
                if compound[-1]["phase"] == "body" and not compound[-1]["seen"]:
                    # An EMPTY body.  MEASURED: `if true; then fi` and
                    # `if true; then echo hi; else fi` are REFUSED with
                    # "compound-command".
                    return "compound-command"
                compound.pop()
            elif word == "done":
                if not compound or compound[-1]["kind"] != "loop":
                    return "compound-command"
                if compound[-1]["phase"] == "body" and not compound[-1]["seen"]:
                    # An EMPTY body.  MEASURED: `while false; do done`
                    # and `for i in 1; do done` are REFUSED with
                    # "compound-command".
                    return "compound-command"
                compound.pop()
            else:
                if compound and compound[-1]["phase"] in ("body", "header"):
                    compound[-1]["seen"] = True
            expect_command = False
            continue
        if compound and compound[-1]["phase"] in ("body", "header"):
            compound[-1]["seen"] = True
        expect_command = False

    if compound:
        # A compound command that is never CLOSED.  MEASURED:
        # `if true; then echo hi`, `while false; do echo hi` and
        # `for i in 1; do echo hi` are REFUSED with "compound-command".
        return "compound-command"
    if len(frames) > 1:
        kind = frames[-1]["kind"]
        if kind == "subst":
            return "command-substitution"
        return "invalid-structure"
    return None


def _matcher_group_stands_where_a_command_may_stand(
    command: str, start: int, index: int
) -> bool:
    """May this brace GROUP stand where it does?

    A GROUP is a COMPOUND COMMAND, and a compound command may only
    stand where a command is expected.  MEASURED:

        { echo hi; }                 ALLOWED  (command position)
        echo "a"; { echo hi; }       ALLOWED  (command position)
        echo a { echo hi; }          REFUSED  invalid-structure
        true { echo hi; }            REFUSED  invalid-structure
        echo 'a' { echo hi; }        REFUSED  invalid-structure
        f() { echo hi; }             ALLOWED  (a definition's body)
        coproc <name> { echo hi; }   ALLOWED  (a declaration's body)
        coproc "" { echo hi; }       REFUSED  invalid-structure
        coproc mp""m { echo hi; }    REFUSED  invalid-structure

    So the slot before the group has to be EMPTY - or a function
    definition's parameter list, or a `coproc` declaration whose name
    is a PLAIN literal word.
    """
    slot = command[start:index].strip()
    if not slot:
        return True
    if re.match(r"^(function\s+)?[A-Za-z_][A-Za-z0-9_]*\s*\(\s*\)$", slot):
        # A FUNCTION DEFINITION's body: `f() { echo hi; }` and the
        # `function` keyword form, `function f() { echo hi; }`, are both
        # ALLOWED.
        return True
    if re.match(r"^function\s+[A-Za-z_][A-Za-z0-9_]*$", slot):
        # The `function` keyword form without a parameter list.
        return True
    if re.match(r"^coproc(\s+[A-Za-z_][A-Za-z0-9_.-]*)?$", slot):
        return True
    return False


def _matcher_subject_is_command(subject: str) -> bool:
    """Is this `case` SUBJECT a command rather than a word?

    MEASURED: `case echo x in y) echo hi;; esac` is REFUSED with
    "compound-command", and it names no binary at all - so the refusal
    is about the CONSTRUCT.

    A subject that is ONE WORD is fine, and so is one that is an
    EXPANSION: `case x in y) ...`, `case "echo x" in y) ...` and
    `case $(( x )) in z) ...` are all ALLOWED.  So the test is whether
    the subject carries a space outside its quoted regions and its
    substitutions.

    The subject is STRIPPED first: the text between `case` and ` in `
    carries the spaces that SEPARATE the keyword from the subject,
    and those are not part of the subject.
    """
    subject = subject.strip()
    index = 0
    length = len(subject)
    while index < length:
        char = subject[index]
        if char in "\"'":
            end = subject.find(char, index + 1)
            if end < 0:
                return False
            index = end + 1
            continue
        if char == "$" and index + 1 < length and subject[index + 1] == "(":
            # An EXPANSION: `$( ... )` and `$(( ... ))`.
            if subject.startswith("$((", index):
                end = subject.find("))", index + 3)
                index = length if end < 0 else end + 2
                continue
            depth = 0
            end = index + 1
            while end < length:
                if subject[end] == "(":
                    depth += 1
                elif subject[end] == ")":
                    depth -= 1
                    if depth == 0:
                        break
                end += 1
            index = end + 1 if end < length else length
            continue
        if char.isspace():
            return True
        index += 1
    return False


def _matcher_pattern_text(part: str) -> str:
    """The text of one `case` PATTERN, with its quotes removed.

    A pattern that is QUOTED and EMPTY is still empty.  MEASURED:
    `case x in "" ) echo hi;; esac` is REFUSED with "invalid-structure",
    and it names no binary at all.
    """
    text = part.strip()
    while len(text) >= 2 and text[0] == text[-1] and text[0] in "\"'":
        text = text[1:-1].strip()
    return text


def _matcher_next_keyword(segments, position: int) -> str:
    """The first word of the next segment that carries one."""
    for text, _ in segments[position + 1 :]:
        words = text.split()
        if words:
            return words[0]
    return ""


def _matcher_subshell_trailing(segment: str) -> bool:
    """Does a SUBSHELL carry TRAILING TEXT after it?

    MEASURED: `( echo hi ) extra` is REFUSED with "invalid-structure",
    and it names no binary at all - so the refusal is about the
    CONSTRUCT.  `( echo hi )` is ALLOWED.

    Only a BARE WORD after the group is trailing text: another
    construct after it - `(echo hi) (echo bye)` - is a second command,
    and a shell accepts that.
    """
    text = segment.strip()
    if not text.startswith("("):
        return False
    depth = 0
    for index, char in enumerate(text):
        if char == "(":
            depth += 1
        elif char == ")":
            depth -= 1
            if depth == 0:
                rest = text[index + 1 :].strip()
                return bool(rest) and (rest[0].isalnum() or rest[0] in "_")
    return False


def _matcher_extract_cases(command: str):
    """The BODY of every `case` construct, and the command without them.

    The host treats a `case` statement as ONE construct whose HEADER
    is not matched — exactly like a loop's header — and whose BODY is
    matched.  MEASURED:

        case x in mpm) echo hi;; esac      ALLOWED (the pattern list
                                           is not a command)
        case x in "mpm") echo hi;; esac    ALLOWED (the quoted pattern
                                           is not a command either)
        case mpm in mpm) echo hi;; esac     ALLOWED (the WORD is not a
                                           command either)
        case x in *) mpm --version;; esac   REFUSED (the body is)

    The construct is extracted BEFORE the command is split, because the
    `;;` that separates the branches is a separator the host does not
    split on — the branches are part of one construct.

    A SUBJECT that carries a CONTEXT is a construct the host cannot
    analyse at all.  MEASURED:

        case [[ mpm == x ]] in y) echo hi;; esac   REFUSED
        case [[ x == y ]] in z) echo hi;; esac      REFUSED — and it
                                                      names no binary at
                                                      all, so the refusal
                                                      is about the
                                                      CONSTRUCT
        case (( x )) in z) echo hi;; esac           REFUSED
        case $(( x )) in z) echo hi;; esac          ALLOWED — a `$( … )`
                                                      substitution is not
                                                      a context
        case x in [[ y ]]) echo hi;; esac          ALLOWED — a context
                                                      in the PATTERN list
                                                      is not the subject

    The reason is the scanner's own limit ("compound-command"), so the
    model reproduces the VERDICT and names the right reason.

    A MALFORMED `case` is a construct the host cannot analyse either.
    MEASURED - every one of them names no binary at all, so the
    refusal is about the CONSTRUCT:

        case x in ) echo hi;; esac      REFUSED  (compound-command)
                                          - an empty PATTERN LIST
        case in y) echo hi;; esac       REFUSED  (compound-command)
                                          - no SUBJECT
        case x in |) echo hi;; esac      REFUSED  (compound-command)
                                          - an empty pattern in the list
        case x in y) echo hi;;          REFUSED  (compound-command)
                                          - no `esac`
        case x in y) echo hi;; esac extra REFUSED (invalid-structure)
                                          - text after the `esac`

    A `case` may be NESTED, and the well-formed nested form is
    ALLOWED: `case y in z) case w in q) echo hi;; esac;; esac`.
    """
    bodies = []
    headers = []
    opaque = []
    kept = []
    index = 0
    length = len(command)
    while index < length:
        if not command.startswith("case", index):
            kept.append(command[index])
            index += 1
            continue
        before = command[index - 1] if index else ""
        after = command[index + 4 : index + 5]
        if (before and not (before.isspace() or before in ";|&()\n")) or (
            after and not after.isspace()
        ):
            kept.append(command[index])
            index += 1
            continue
        in_at = command.find(" in ", index + 4)
        if in_at < 0:
            kept.append(command[index])
            index += 1
            continue
        subject = command[index + 4 : in_at]
        # A SUBJECT that is a COMMAND is a construct the host cannot
        # analyse, and the SCANNER reads it - MEASURED:
        # `case echo x in y) echo hi;; esac` is REFUSED with
        # "compound-command", and it names no binary at all.  So the
        # rule lives in ONE place, and the mutation sweep can revert
        # it and see the guard go red.
        if not subject.strip():
            # A `case` with NO SUBJECT is a construct the host cannot
            # analyse.  MEASURED: `case in y) echo hi;; esac` is
            # REFUSED with "compound-command", and it names no binary.
            opaque.append(command[index : in_at])
            index = in_at
            continue
        close = command.find(")", in_at + 4)
        if close < 0:
            kept.append(command[index])
            index += 1
            continue
        patterns = command[in_at + 4 : close]
        first_pattern = _matcher_pattern_text(patterns.split("|")[0])
        if not first_pattern:
            # The FIRST pattern has to be non-empty.  MEASURED:
            # `case x in ) echo hi;; esac`, `case x in |) echo hi;; esac`,
            # `case x in |y) echo hi;; esac` and `case x in "" ) echo hi;; esac`
            # are all REFUSED, and none of them names a binary.
            #
            # A LATER pattern may be empty: MEASURED,
            # `case x in y|) echo hi;; esac` and `case x in y|'') echo hi;; esac`
            # are ALLOWED.
            opaque.append(command[index : close + 1])
            index = close + 1
            continue
        end = _matcher_matching_esac(command, close + 1)
        if end < 0:
            # A `case` with NO `esac` is a construct the host cannot
            # analyse.  MEASURED: `case x in y) echo hi;;` is REFUSED
            # with "compound-command", and it names no binary.
            opaque.append(command[index : close + 1])
            index = close + 1
            continue
        after = command[end + 4 :].lstrip()
        if after and after[0] not in ";&)|}\n":
            # Only a SEPARATOR may follow the `esac`.  MEASURED:
            # `case x in y) echo hi;; esac extra` is REFUSED with
            # "invalid-structure", and it names no binary.
            opaque.append(command[index : end + 4])
            index = end + 4
            continue
        kept.append(" ")
        # The header is not matched, but the substitutions inside it
        # ARE - the host descends into a substitution anywhere.
        headers.append(command[index : close + 1])
        if _matcher_has_context(subject):
            # A SUBJECT that carries a context is a construct the
            # host cannot analyse.
            opaque.append(subject.strip())
        for chunk in _matcher_split_branches(command[close + 1 : end]):
            # A branch may carry the NEXT pattern before its `)` - but
            # a branch that IS a nested `case` construct carries none,
            # and the recursion below has to see it whole.
            if not _matcher_word_at(chunk.strip(), 0, "case"):
                cut = chunk.find(")")
                if cut >= 0:
                    chunk = chunk[cut + 1 :]
            if chunk.strip():
                bodies.append(chunk)
        index = end + 4
    return bodies, headers, opaque, "".join(kept)


def _matcher_has_context(text: str) -> bool:
    """Does this text carry a `[[ … ]]` or a `(( … ))` CONTEXT?

    A `$( … )` substitution is NOT a context: the host parses it as a
    substitution, so `case $(( x )) in z)` is allowed while
    `case (( x )) in z)` is refused.
    """
    return bool(re.search(r"(?<!\$)\[\[", text)) or bool(
        re.search(r"(?<!\$)\(\(", text)
    )


# The RECORDED DIVERGENCES: the forms the host ALLOWS and the model
# REFUSES.  Every one of them is in the SAFE direction - the model
# refuses more than the host - and every one of them was MEASURED.
#
#     x=$(( mpm ))              an assignment's value is matched differently
#     coproc mpm { echo hi; }   a coproc's name is not a command
#     eval                      the host allows a bare `eval`
#     eval "hello world"        the host allows this one too
#     eval [[ x == y ]]         and this one as well
#
# The list IS the count the documents publish, and the SPEC is
# asserted to publish THIS number - so a new divergence has to be
# added here, and the published number has to follow it.
_DIVERGENCES = [
    "x=$(( mpm ))",
    "coproc mpm { echo hi; }",
    "eval",
    'eval "hello world"',
    "eval [[ x == y ]]",
]

_NUMBER_WORDS = {
    1: "one",
    2: "two",
    3: "three",
    4: "four",
    5: "five",
    6: "six",
    7: "seven",
    8: "eight",
    9: "nine",
    10: "ten",
}

_MATCHER_COMMAND = "command"
_MATCHER_HEADER = "header"
# A construct the host's scanner cannot ANALYSE at all.
_MATCHER_OPAQUE = "opaque"

# The keywords the host gives a special meaning to.  A `(` that
# follows one of them opens a COMMAND, not an argument.
_MATCHER_KEYWORDS = (
    "!",
    "builtin",
    "case",
    "command",
    "coproc",
    "do",
    "elif",
    "else",
    "for",
    "function",
    "if",
    "in",
    "select",
    "then",
    "time",
    "until",
    "while",
)


def _matcher_mark_heredocs(command: str):
    """The command with every HERE-DOCUMENT BODY marked, and a flag.

    The host's COMMAND ANALYSIS treats a here-document's body as
    DATA - the body's lines are not commands - but its RULE MATCHER
    still sees the body TEXT.  MEASURED:

        cat <<EOF / mpm / EOF                ALLOWED  (the body is not
                                                      a command, so the
                                                      exact rule `mpm`
                                                      does not fire)
        cat <<EOF / echo mpm / EOF           ALLOWED  ("mpm" at the end
                                                      of a line is not
                                                      the rule `* mpm *`,
                                                      which needs a
                                                      trailing space)
        cat <<EOF / echo mpm --version / EOF REFUSED  (the rule matcher
                                                      sees the body text)

    So the body STAYS in the text the rules are matched against, and
    its newlines are marked so the command analysis does not split the
    command on them.  (The marker is not a space: a space would make
    "mpm" at the end of a line look like the rule `* mpm *`.)

    The TERMINATOR must start a line - leading TABS are stripped only
    for `<<-` - and a here-document whose terminator is not found is a
    construct the host cannot analyse at all ("heredoc"), so the flag
    says so.

    A here-STRING (`<<<`) is not a here-document at all: its target is
    a word.  MEASURED: `cat <<< "hi there"` is ALLOWED, and `cat <<<
    mpm` is refused by the WORD SHAPE.
    """
    marked = list(command)
    index = 0
    length = len(command)
    quote = None
    unterminated = False
    while index < length:
        char = command[index]
        if quote is not None:
            if char == quote:
                quote = None
            index += 1
            continue
        if char in "\"'":
            quote = char
            index += 1
            continue
        if command.startswith("<<<", index):
            # A here-STRING: its target is a WORD, not a here-document.
            # MEASURED: `cat <<< "hi there"` is ALLOWED - and the form
            # that names a binary (`cat <<< mpm`) is refused by the WORD
            # SHAPE, not by the scanner.
            index += 3
            continue
        if not command.startswith("<<", index):
            index += 1
            continue
        look = index + 2
        strip_tabs = False
        if look < length and command[look] == "-":
            strip_tabs = True
            look += 1
        while look < length and command[look].isspace():
            look += 1
        if look < length and command[look] in "\"'":
            closer = command[look]
            close = command.find(closer, look + 1)
            delimiter = command[look + 1 : close] if close > 0 else ""
            look = close + 1 if close > 0 else length
        else:
            start = look
            while look < length and not command[look].isspace():
                look += 1
            delimiter = command[start:look]
        newline = command.find("\n", look)
        if newline < 0:
            unterminated = True
            index = look
            continue
        body = newline + 1
        cursor = body
        end = None
        while cursor < length:
            line_end = command.find("\n", cursor)
            if line_end < 0:
                line_end = length
            probe = command[cursor:line_end]
            if strip_tabs:
                probe = probe.lstrip("\t")
            if probe == delimiter:
                end = cursor
                break
            cursor = line_end + 1
        if end is None:
            unterminated = True
            index = body
            continue
        # The newline that ENDS the command line is marked too: the
        # body belongs to the command's text, and the command
        # analysis does not split on it.
        for position in range(newline, end):
            if marked[position] == "\n":
                marked[position] = "\x00"
        index = max(index + 1, body)
        continue
    return "".join(marked), unterminated


def _matcher_segments(command: str, marked: bool = False):
    """The simple commands of a command, each with its KIND.

    A KIND is either "command" — the text is matched against the
    rules — or "header" — the text is NOT matched, but the
    substitutions inside it ARE descended into.

    The host gives a loop TWO structures, and the distinction is the
    whole point.  MEASURED:

        for i in mpm; do $i --version; done     ALLOWED  — a `for`
                                                     header is a WORD
                                                     LIST, and it is not
                                                     matched
        while mpm; do echo hi; done             REFUSED  — a `while`
                                                     CONDITION is a
                                                     COMMAND LIST, and it
                                                     IS matched
        until mpm --version; do echo hi; done   REFUSED  — the same
        for i in $(echo mpm); do echo hi; done  REFUSED  — the host
                                                     descends into a
                                                     substitution ANYWHERE,
                                                     header included

    A `case` STATEMENT is one construct whose header — the pattern
    list included — is not matched either.  It is extracted before the
    split, because the `;;` between its branches is a separator the
    host does not split on.
    """
    if not marked:
        command, unterminated = _matcher_mark_heredocs(command)
        if unterminated:
            # A here-document whose TERMINATOR is not found is a
            # construct the host cannot analyse at all ("heredoc").
            return [(command, _MATCHER_OPAQUE)]
    # `marked` says the text has ALREADY been marked: its here-document
    # bodies are known, so the scan must not run again.  MEASURED: a
    # here-document inside a loop body or a `case` branch body is
    # ALLOWED - `for i in 1; do` and `cat <<EOF` and `hi` and `EOF` and
    # `done` - and the body is RE-SPLIT, so a second scan over the
    # marked text would find no newline to end the here-document and
    # would declare it unterminated.
    case_bodies, case_headers, opaque, without_cases = _matcher_extract_cases(
        command
    )
    segments = _matcher_split(without_cases)
    for header in case_headers:
        segments.append((header, _MATCHER_HEADER))
    for subject in opaque:
        segments.append((subject, _MATCHER_OPAQUE))
    for body in case_bodies:
        segments.extend(_matcher_segments(body, marked=True))
    return _matcher_bodies(segments)


def _matcher_split(command: str):
    """The simple commands of a command, split on the separator set."""
    segments, current, depth, quote = [], [], 0, None
    index = 0
    loop = 0
    word = ""
    while index < len(command):
        char = command[index]
        word_before = word
        if quote is None and (char.isspace() or char in ";|&()"):
            # The keyword test, at a word boundary.
            if word in ("for", "while", "until", "select"):
                loop += 1
            elif word == "done" and loop > 0:
                loop -= 1
            word = ""
        elif quote is None:
            word += char
        if quote is not None:
            current.append(char)
            if char == quote:
                quote = None
            index += 1
            continue
        if char in "\"'":
            quote = char
            current.append(char)
            index += 1
            continue
        if char == "#" and not word_before:
            # The host strips a COMMENT before it matches, and the
            # newline that ends it still separates commands.
            # MEASURED: `echo hi # mpm` is ALLOWED.
            end = command.find("\n", index)
            index = len(command) if end < 0 else end
            continue
        if char == "(":
            if command.startswith("((", index):
                # An ARITHMETIC context is not a group: its `;`
                # separates arithmetic, not commands, and the host
                # parses it as one thing.
                end = command.find("))", index)
                if end >= 0:
                    current.append(command[index : end + 2])
                    index = end + 2
                    continue
            depth += 1
        elif char == ")":
            depth = max(0, depth - 1)
        pair = command[index : index + 2]
        if depth == 0 and pair in ("&&", "||"):
            segments.append(("".join(current), _MATCHER_COMMAND))
            current = []
            index += 2
            continue
        if depth == 0 and char in ";|\n":
            if loop == 0:
                segments.append(("".join(current), _MATCHER_COMMAND))
                current = []
                index += 1
                continue
            current.append(char)
            index += 1
            continue
        if depth == 0 and char == "&":
            # The `&` of a REDIRECT OPERATOR is not a separator:
            # `2>&1` duplicates a descriptor and `&>` redirects both
            # streams, and the host parses each as ONE operator.
            # MEASURED: `echo hi 2>&1` is ALLOWED, and it is the most
            # common redirection idiom there is - `cmd > log 2>&1`.
            previous = "".join(current).rstrip()
            if previous.endswith(">") or (
                index + 1 < len(command) and command[index + 1] == ">"
            ):
                current.append(char)
                index += 1
                continue
            segments.append(("".join(current), _MATCHER_COMMAND))
            current = []
            index += 1
            continue
        current.append(char)
        index += 1
    segments.append(("".join(current), _MATCHER_COMMAND))
    return segments


def _matcher_bodies(segments):
    """The BODY of every shell loop among the segments, and its header.

    A `for`/`select` header is a WORD LIST and is not matched — but
    the substitutions inside it ARE descended into.  A `while`/`until`
    CONDITION is a COMMAND LIST and IS matched.

    It also tracks the ENCLOSING CONSTRUCT, because a CONTINUATION
    keyword with nothing to continue is a construct the host cannot
    analyse.  MEASURED: `do [[ mpm == x ]]`, `do echo hi`, `then echo
    hi`, `else [[ mpm == x ]]`, `elif echo hi` and `fi echo hi` are all
    REFUSED with "compound-command" — and `do [[ x == y ]]`, which
    names nothing at all, is refused the same way, so the refusal is
    about the construct and not about the name.
    """
    out = []
    # The compound commands have to be CLOSED, their bodies may not be
    # EMPTY, a continuation keyword may not continue nothing, and a
    # closing keyword may not close nothing.  MEASURED: `if true; then
    # echo hi` (no `fi`), `while false; do echo hi` (no `done`),
    # `for ; do echo hi` (an empty header), `if true; then fi` (an empty
    # body), `if true; then echo hi; else fi`, `do echo hi` and `fi
    # echo hi` are all REFUSED with "compound-command", and none of them
    # names a binary.
    #
    # The SCANNER reads all of that, in ONE place, so the rule lives
    # in ONE place - and the mutation sweep can revert it and see the
    # guard go red.
    for position, (text, kind) in enumerate(segments):
        words = text.split()
        keyword = words[0] if words else ""
        if keyword == "if":
            # The CONDITION of an `if` may not be EMPTY.
            if (
                not text.strip()[len("if") :].strip()
                and _matcher_empty_condition(segments, position, "if")
            ):
                out.append((text, _MATCHER_OPAQUE))
                continue
        if keyword not in ("for", "while", "until", "select") or "do" not in words:
            # A bare FUNCTION DEFINITION with no body is a construct
            # the host cannot analyse.  MEASURED: `f()` is REFUSED with
            # "dynamic-execution".  A definition whose BODY follows is
            # fine: MEASURED, `f()` and then `{ echo hi; }` is ALLOWED.
            if _matcher_bare_function(text) and not _matcher_has_body(segments, position):
                out.append((text, _MATCHER_OPAQUE))
                continue
            out.append((text, kind))
            continue
        cut = words.index("do")
        # The `;` that separates the header from `do` is a separator,
        # not part of the header.
        header = " ".join(words[1:cut]).strip().rstrip(";").strip()
        body = " ".join(words[cut + 1 :])
        if body.endswith("done"):
            body = body[: -len("done")].strip()
        if header:
            if keyword in ("for", "select"):
                # A WORD LIST: not matched, but the substitutions
                # inside it are.
                out.append((header, _MATCHER_HEADER))
            else:
                # A COMMAND LIST: matched, command by command.
                for part in _matcher_segments(header, marked=True):
                    out.append(part)
        elif keyword in ("while", "until"):
            # The CONDITION of a `while`/`until` may not be EMPTY.
            # MEASURED: `while ; do echo hi; done` and
            # `until ; do echo hi; done` are REFUSED with
            # "compound-command".
            out.append((text, _MATCHER_OPAQUE))
            continue
        if body:
            for part in _matcher_segments(body, marked=True):
                out.append(part)
    return out


def _matcher_substitution_body(segment: str, index: int):
    """The body of the substitution that starts at `index`, and the index after it.

    A `)` inside a QUOTED STRING does not close the substitution: the
    shell does not expand it there, and the host's scanner knows
    that.  MEASURED: `echo <(echo "a)b"` is REFUSED with
    "command-substitution" - the substitution is never closed.
    """
    if segment[index] == "`":
        end = segment.find("`", index + 1)
        if end < 0:
            return None, index + 1
        return segment[index + 1 : end], end + 1
    depth = 0
    end = index + 1
    quote = None
    while end < len(segment):
        char = segment[end]
        if quote is not None:
            if char == quote:
                quote = None
            end += 1
            continue
        if char in "\"'":
            quote = char
            end += 1
            continue
        if char == "(":
            depth += 1
        elif char == ")":
            depth -= 1
            if depth == 0:
                break
        end += 1
    return segment[index + 2 : end], end + 1


def _matcher_substitutions(segment: str, depth: int = 0):
    """The text inside each substitution the host descends into.

    The host has a portable shell scanner, and it descends into
    `$( ... )`, into BACKTICK substitutions (`` `...` ``) and into
    DOUBLE-QUOTED strings — a substitution inside `"..."` expands in a
    POSIX shell, so the host looks inside it. It descends RECURSIVELY,
    and it re-splits a substitution's contents on the same separator
    set it uses for a command.

    All of that was MEASURED, not assumed:

        echo `mpm --version`        refused  (the backtick form)
        echo "$(mpm --version)"     refused  (double quotes expand)
        echo "`mpm --version`"      refused  (double quotes expand)
        echo 'echo `mpm --version`' allowed  (single quotes are literal)
        echo $(mpm; echo hi)        refused  (the contents are re-split)
        echo $(echo $(mpm))         refused  (the descent is recursive)

    A PROCESS SUBSTITUTION inside an ARITHMETIC context is not
    descended into: the host does not analyse one.  MEASURED:
    `(( <(mpm --version) ))` is ALLOWED, while `(( $(echo mpm
    --version) ))` is REFUSED and `[[ -n <(mpm --version) ]]` is
    REFUSED.

    A model that could not see these would UNDER-APPROXIMATE the host,
    and a model that under-approximates the host cannot argue that a
    command the definition states is open really is open.
    """
    if depth > 8:
        return []
    bodies = []
    index = 0
    length = len(segment)
    # The command-position ARITHMETIC context: the host does not
    # analyse a process substitution inside one.  MEASURED:
    # `(( <(mpm --version) ))` is ALLOWED, while `(( $(echo mpm
    # --version) ))` is REFUSED - so the arithmetic context hides a
    # PROCESS SUBSTITUTION and not a `$( ... )`.
    arithmetic = None
    found = _matcher_command_context(segment)
    if found is not None and found[2] == "((":
        arithmetic = (found[0], found[1])
    while index < length:
        char = segment[index]
        if char == "'":
            # A SINGLE-quoted string is LITERAL: nothing inside it
            # expands, and the host does not descend into it.
            end = segment.find("'", index + 1)
            index = length if end < 0 else end + 1
            continue
        if char == '"':
            # A DOUBLE-quoted string is not literal: a substitution
            # inside it still expands, so its span is scanned for
            # openers.
            index += 1
            while index < length:
                if segment[index] == "\\":
                    index += 2
                    continue
                if segment[index] == '"':
                    index += 1
                    break
                if segment.startswith("$(", index) or segment[index] == "`":
                    body, after = _matcher_substitution_body(segment, index)
                    if body is not None:
                        bodies.append(body)
                        bodies.extend(_matcher_substitutions(body, depth + 1))
                        index = after
                        continue
                index += 1
            continue
        if segment.startswith("$((", index):
            # An ARITHMETIC context is not a command substitution.
            index += 1
            continue
        if (
            segment.startswith("$(", index)
            or segment.startswith("<(", index)
            or segment.startswith(">(", index)
            or char == "`"
        ):
            if segment.startswith("<(", index) or segment.startswith(">(", index):
                if arithmetic is not None and arithmetic[0] <= index < arithmetic[1]:
                    # A process substitution inside an ARITHMETIC
                    # context: the host does not analyse one.
                    index += 2
                    continue
            body, after = _matcher_substitution_body(segment, index)
            if body is not None:
                bodies.append(body)
                bodies.extend(_matcher_substitutions(body, depth + 1))
                index = after
                continue
            index += 1
            continue
        index += 1
    return bodies


def _matcher_command_context(segment: str):
    """The CONTEXT that IS the command: its span and its opener.

    MEASURED: `[[ mpm == x ]]` and `(( mpm ))` are ALLOWED - the
    operands of a command-position context are words, not commands -
    while `echo [[ mpm == x ]]` is REFUSED, because in ARGUMENT
    position the text is ordinary text.

    Only KEYWORDS may stand between the start of the segment and the
    context: the `if`, `elif`, `else`, `then`, `while`, `until` and
    `do` keywords introduce a COMMAND LIST, and the host recognises a
    context there.
    """
    text = segment.lstrip()
    lead = len(segment) - len(text)
    index = lead
    opener = closer = None
    while index < len(segment):
        if segment.startswith("[[", index):
            opener, closer = "[[", "]]"
            break
        if segment.startswith("((", index):
            opener, closer = "((", "))"
            break
        index += 1
    if opener is None:
        return None
    if not _matcher_keyword_prefix(segment[lead:index]):
        return None
    end = segment.find(closer, index + len(opener))
    if end < 0:
        return None
    return index, end + len(closer), opener


def _matcher_mask_contexts(segment: str) -> str:
    """The segment with the CONTEXT the host does not match blanked.

    A `[[ … ]]` conditional's operands are words and patterns, not
    commands, and an arithmetic context's operands are arithmetic, not
    commands — but ONLY when the context IS the command.

    MEASURED:

        [[ mpm == x ]]       ALLOWED  — the context IS the command
        (( mpm ))            ALLOWED
        ((mpm))              ALLOWED
        [[ -n mpm ]]         ALLOWED
        [[ mpm ]]            ALLOWED
        echo $(( mpm ))      REFUSED  — ARGUMENT position: the text is
                                       ordinary text and the word
                                       shape matches it
        echo $(( mpm + 1 ))  REFUSED
        echo $(( 1 + mpm ))  REFUSED
        echo a $(( mpm )) b  REFUSED
        echo [[ mpm == x ]]  REFUSED

    So the context is blanked only when it LEADS the segment.  The
    spans are blanked with SPACES rather than removed, so a word
    boundary is never invented.
    """
    found = _matcher_command_context(segment)
    if found is None:
        return segment
    index, end, _ = found
    masked = list(segment)
    for position in range(index, end):
        masked[position] = " "
    return "".join(masked)


# The keywords that CONTINUE a construct, and the ones that CLOSE it.
# A continuation keyword with nothing to continue is a construct the
# host cannot analyse - MEASURED: `do echo hi` and `then echo hi` are
# REFUSED with "compound-command".
_MATCHER_CONTINUATION_KEYWORDS = ("do", "elif", "else", "then")
_MATCHER_CLOSING_KEYWORDS = ("done", "esac", "fi")

# The words that may stand in front of a CONTEXT.  Each of them
# introduces a COMMAND LIST or a command, so the host recognises a
# context there.  MEASURED: `if [[ mpm == x ]]; then echo hi; fi`,
# `if (( mpm )); then echo hi; fi`, `else [[ mpm == x ]]`,
# `else (( mpm ))`, `{ [[ mpm == x ]]; }`, `time [[ mpm == x ]]` and
# `! (( mpm ))` are all ALLOWED by the host.
_MATCHER_COMMAND_PREFIXES = (
    "!",
    "{",
    "coproc",
    "do",
    "elif",
    "else",
    "fi",
    "for",
    "if",
    "select",
    "then",
    "time",
    "until",
    "while",
)


def _matcher_keyword_prefix(prefix: str) -> bool:
    """Is everything before a context only KEYWORDS and separators?

    The keywords that introduce a COMMAND LIST are `if`, `elif`,
    `then`, `while`, `until` and `do`; the host parses the condition
    of an `if` as a command list and recognises a `[[ … ]]` or
    `(( … ))` context there.

    MEASURED: `if [[ mpm == x ]]; then echo hi; fi` and `if (( mpm ));
    then echo hi; fi` are ALLOWED by the host.
    """
    if not prefix.strip():
        return True
    for word in re.split(r"[\s;&|()]+", prefix):
        if not word:
            continue
        if word not in _MATCHER_COMMAND_PREFIXES:
            return False
    return True


def _matcher_broken_context(segment: str) -> bool:
    """Is this segment carrying a CONTEXT the host cannot analyse?

    MEASURED:

        [[ x; y ]]   REFUSED (invalid-structure)
        ((x;y))      REFUSED (invalid-structure)
        [[ x         REFUSED (invalid-structure) - never closed

    A `;` inside a context is not a separator the host accepts, and a
    context that is never closed is a construct it cannot finish
    parsing.
    """
    index = 0
    length = len(segment)
    quote = None
    while index < length:
        char = segment[index]
        if quote is not None:
            if char == quote:
                quote = None
            index += 1
            continue
        if char in "\"'":
            quote = char
            index += 1
            continue
        if segment.startswith("[[", index):
            closer = "]]"
            start = index + 2
        elif segment.startswith("((", index):
            closer = "))"
            start = index + 2
        else:
            index += 1
            continue
        end = segment.find(closer, start)
        if end < 0:
            return True
        if ";" in segment[start:end]:
            return True
        index = end + len(closer)
    return False


def _matcher_unanalysable(segment: str) -> bool:
    """Would the host REFUSE this segment because it cannot analyse it?

    MEASURED: `echo <(mpm --version)>` and `echo <(echo hello)>` both
    return "Portable shell scanner cannot analyze command:
    invalid-redirect" — the second contains no MPM name at all — and
    `echo (mpm --version)` and `echo (hello)` both return
    "compound-command".

    A REDIRECTION with an EMPTY TARGET is refused the same way, and
    the measurement says so: `echo mpm>` and `echo hi >` are both
    REFUSED, and the second names nothing at all — so the refusal is
    about the REDIRECTION and not about the name.

    A command the host classifies as DYNAMIC EXECUTION is refused too:
    `alias NAME=value` returns "dynamic-execution" - and so is a bare
    FUNCTION DEFINITION with no body, `f()`, which returns
    "dynamic-execution" as well.

    An EMPTY GROUP is refused: `( )` and `{ }` both return
    "invalid-structure", and neither names a binary at all.

    An UNCLOSED SUBSTITUTION is refused: `$(` and `<(` both return
    "command-substitution", and neither names a binary at all.

    A PROCESS SUBSTITUTION inside a CONTEXT is NOT such a command:
    the host parses a context's operands as words and never looks for
    the substitution, so `[[ -n <(echo hi) ]]`, `(( <(echo hi) ))` and
    `echo [[ -n <(echo hi) ]]` are ALLOWED.

    The host refuses to LOOK at such a command, so it does not run,
    whatever the rules say. The model reproduces that verdict, and the
    reason it gives is the scanner's limit rather than a rule match.
    """
    if _matcher_dynamic_execution(segment):
        return True
    if _matcher_broken_context(segment):
        return True
    if _matcher_unclosed_substitution(segment):
        return True
    # Only a SEPARATOR may follow a closed compound construct.
    # MEASURED: `if true; then echo hi; fi extra`, `{ echo hi; } extra`,
    # `( echo hi ) extra` and `f() { echo hi; } extra` are all REFUSED
    # with "invalid-structure", and none of them names a binary.
    stripped = segment.strip()
    words = stripped.split()
    if words:
        first = words[0]
        if (first in _MATCHER_CLOSING_KEYWORDS or first == "}") and stripped[
            len(first) :
        ].strip():
            return True
    if _matcher_subshell_trailing(segment):
        return True
    # A `<(` is a PROCESS SUBSTITUTION, not a refusal by itself: the
    # scanner parses its content as a command, so the names inside are
    # matched.  MEASURED:
    #
    #     echo <(mpm --version)      REFUSED  (the name matcher)
    #     echo <(echo hello)>         ALLOWED
    #     cat <(echo hi)>             ALLOWED
    #
    # The invalid-redirect refusal is the EMPTY TARGET that follows
    # it: `echo <(echo hello)>` has a `>` with no target, and that is
    # refused with "invalid-redirect" - whether or not it names a
    # binary.
    #
    # A process substitution right AFTER a command-position CONTEXT is
    # a structure the host cannot analyse.  MEASURED:
    # `[[ x == y ]] <(echo hi)` is REFUSED with "invalid-structure",
    # while `[[ -n <(echo hi) ]] && echo hi` is ALLOWED.
    found = _matcher_command_context(segment)
    if found is not None:
        index, end, _ = found
        rest = segment[end:].lstrip()
        if rest.startswith("<(") or rest.startswith(">("):
            return True
    masked = _matcher_mask_contexts(segment)
    index = 0
    while index < len(masked):
        if masked[index] == "(":
            # The `(` of a `$( ... )` substitution is not a group.
            if index > 0 and masked[index - 1] == "$":
                index += 1
                continue
            if _matcher_follows_a_word(masked, index):
                return True
        index += 1
    return _matcher_empty_redirect(segment)


def _matcher_dynamic_execution(segment: str) -> bool:
    """Is this command one the host classifies as DYNAMIC EXECUTION?

    MEASURED: `alias NAME=value` and `alias mpm=mpm` are refused with
    "dynamic-execution".  An `alias` DEFINES a command at run time, so
    the text after it is not what will run — and `eval` does the
    same.

    A BARE `alias` is ALLOWED by the host — it lists the aliases
    and defines nothing — so the model refuses an `alias` only
    when it CARRIES AN ASSIGNMENT.

    `eval` is refused on the same principle, and it is the SAFE
    direction: a model that refuses more than the host only costs a
    nuisance, while a model that refuses less would claim a route is
    closed when it is not.

    MEASURED, and it is a divergence in that safe direction: the host
    allows a bare `eval` and `eval "hello world"`, and the model
    refuses both.
    """
    words = segment.strip().split()
    if not words:
        return False
    if words[0] == "eval":
        return True
    if words[0] == "alias":
        return any("=" in word for word in words[1:])
    return False


def _matcher_follows_a_word(segment: str, index: int) -> bool:
    """Is the `(` at `index` an ARGUMENT rather than a group?

    MEASURED:

      `echo (hello)`    REFUSED as `compound-command` — the group is an
                        argument;
      `f() { … }`       ALLOWED — the group is a function definition's
                        parameter list, and the host analyses the BODY
                        (`f() { mpm --version; }` is refused by a RULE);
      `(echo hi)`       ALLOWED — the group is a SUBSHELL, and the host
                        analyses it (`(mpm --version)` is refused by a
                        rule);
      `then (echo hi)`  ALLOWED — the group is a command in an `if`
                        body.

    So a `(` is an argument only when it follows an ordinary WORD,
    with a space before it.
    """
    before = segment[:index]
    stripped = before.rstrip()
    if not stripped:
        return False
    if stripped[-1] in ";|&(\n":
        return False
    if not before[-1].isspace():
        # No space before the `(`: it is a function definition's
        # parameter list (`f()`), not an argument.
        return False
    words = stripped.split()
    if words and words[-1] in _MATCHER_KEYWORDS:
        return False
    return True


def _matcher_subshell_inner(segment: str):
    """The command inside a subshell that IS the whole segment.

    The host analyses a subshell and matches the command INSIDE it,
    which is why `(mpm --version)` is refused by a RULE rather than by
    the scanner.  MEASURED: `(echo hi)` is ALLOWED and `(mpm
    --version)` is REFUSED.
    """
    text = segment.strip()
    if not text.startswith("(") or not text.endswith(")"):
        return None
    depth = 0
    for position, char in enumerate(text):
        if char == "(":
            depth += 1
        elif char == ")":
            depth -= 1
            if depth == 0:
                return text[1:position] if position == len(text) - 1 else None
    return None


def _matcher_empty_redirect(segment: str) -> bool:
    """Does this segment carry a REDIRECTION with no target?

    MEASURED: `echo mpm>` and `echo hi >` are both REFUSED with
    "invalid-redirect", and `echo hi >` names nothing at all.

    A quoted `>` is a literal, so it does not count.
    """
    index = 0
    quote = None
    while index < len(segment):
        char = segment[index]
        if quote is not None:
            if char == quote:
                quote = None
            index += 1
            continue
        if char in "\"'":
            quote = char
            index += 1
            continue
        if char in "<>":
            look = index + 1
            if look < len(segment) and segment[look] in "&|":
                look += 1
            while look < len(segment) and segment[look].isspace():
                look += 1
            if look >= len(segment):
                return True
        index += 1
    return False


def _matcher_candidates(segment: str, kind: str = _MATCHER_COMMAND, depth: int = 0):
    """Every text the host matches against the rules.

    A "header" candidate is NOT matched itself — but the
    substitutions inside it are, because the host descends into a
    substitution ANYWHERE.
    """
    masked = _matcher_mask_contexts(segment)
    if kind == _MATCHER_COMMAND:
        yield masked
    if depth > 8:
        return
    # A SUBSHELL is analysed by the host, and the command inside it is
    # matched — so `(mpm --version)` is refused by a rule.  A CONTEXT
    # (`[[ … ]]`, `(( … ))`) is not a subshell, so it has no inner
    # command to match.
    if masked == segment:
        inner = _matcher_subshell_inner(segment)
        if inner:
            yield from _matcher_candidates(inner, _MATCHER_COMMAND, depth + 1)
    for body in _matcher_substitutions(segment, depth):
        # The body comes from text that has ALREADY been marked.
        for part, part_kind in _matcher_segments(body, marked=True):
            yield from _matcher_candidates(part, part_kind, depth + 1)


def _matcher_denies(command: str, rules) -> bool:
    """Would the host deny this command, given these (resource, effect)?"""
    if _matcher_structure(command):
        # A construct the host's scanner cannot PARSE: an unterminated
        # quote or escape, an unbalanced bracket, a quote that splits a
        # word.
        return True
    for segment, kind in _matcher_segments(command):
        if kind == _MATCHER_OPAQUE:
            # A construct the host's scanner cannot ANALYSE at all —
            # e.g. a `case` whose SUBJECT is a context — is refused
            # outright, with the scanner's own reason.
            return True
        # A command the host's scanner cannot ANALYSE is refused
        # outright — a process substitution (`invalid-redirect`), a
        # compound command (`compound-command`), a redirection with no
        # target (`invalid-redirect`) or a dynamic execution
        # (`dynamic-execution`). It does not run, and the reason is
        # the scanner's limit rather than a rule match.
        #
        # A HEADER is not a command: a `for`/`select` header is a WORD
        # LIST and a `case` header is not matched at all, so the host
        # does not analyse either as a command.  MEASURED:
        # `for ((i=0;i<1;i++)); do echo hi; done` and
        # `select ((i=0;i<1;i++)); do echo hi; done` are ALLOWED - the
        # `;` inside the arithmetic header is arithmetic and not a
        # separator the host rejects.
        if kind != _MATCHER_HEADER and _matcher_unanalysable(segment):
            return True
        for candidate in _matcher_candidates(segment, kind):
            # The host TRIMS each segment before matching it.  This
            # was MEASURED: `echo hi & systemctl` is refused, and the
            # only reason is that the trimmed segment is `systemctl`.
            candidate = candidate.strip()
            words = candidate.split()
            if words and words[0] == "cd":
                # The host does not match a `cd` segment's text.
                continue
            effect = None
            for resource, rule_effect in rules:
                if _matcher_u2(candidate, resource):
                    effect = rule_effect
            if effect == "deny":
                return True
    return False


class ReviewerIsolationTest(unittest.TestCase):
    """The reviewer agent's isolation from the production substrate."""

    # ── The registry is the authority ──────────────────────────────────

    def test_registry_parses(self) -> None:
        """The registry must parse into a non-zero set of tools."""
        tools = _registry_tools()
        self.assertGreater(
            len(tools), 0, "the registry parsed into no tools; the comparison is vacuous"
        )

    def test_the_registry_parse_drops_no_name(self) -> None:
        """A name the parse cannot capture must FAIL the test, not pass it.

        The parse extracts the tool names from the registry source. If
        its character class is narrower than the registry's own
        alphabet, a name falls out of the compared set - and a name
        that is not compared needs no deny rule, so the tool stays
        reachable while this suite is green.

        So every `Name:` entry the registry publishes is compared with
        the parsed set, and a name that was dropped fails the test.
        """
        published = {name for name in _registry_names() if name.startswith("mpm_")}
        parsed = _registry_tools()
        dropped = sorted(published - parsed)
        self.assertEqual(
            [],
            dropped,
            "the registry parse dropped these tool names, so no deny rule is "
            "required for them and they stay reachable; widen the character "
            "class in _registry_tools()",
        )

    def test_every_registry_tool_is_denied(self) -> None:
        """Every tool the registry publishes is denied to the reviewer.

        THIS IS THE LOAD-BEARING ASSERTION. A tool added to the
        registry without a matching deny rule would be reachable from a
        reviewer, which is exactly the failure this guards.
        """
        tools = _registry_tools()
        denied = _denied_actions()
        missing = sorted(tool for tool in tools if tool not in denied)
        self.assertEqual(
            [],
            missing,
            "the reviewer does not deny these registry tools; add a deny rule "
            "for each to "
            "agent_installation/mpm-opencode/templates/agents/mpm-reviewer.md",
        )

    def test_the_deny_list_has_no_stale_entries(self) -> None:
        """A deny rule for a tool that no longer exists is dead weight."""
        tools = _registry_tools()
        denied = _denied_actions()
        stale = sorted(
            action
            for action in denied
            if action.startswith("mpm_") and action not in tools
        )
        self.assertEqual(
            [],
            stale,
            "the reviewer denies MPM tools the registry no longer publishes",
        )

    # ── The reviewer's own state changes are closed ───────────────────

    def test_the_reviewer_cannot_edit(self) -> None:
        """A reviewer that could change what it reviews is not a reviewer."""
        self.assertIn("edit", _denied_actions())

    def test_the_reviewer_cannot_spawn_agents(self) -> None:
        """A reviewer must not launch a further agent without the isolation."""
        self.assertIn("subagent", _denied_actions())

    def test_no_rule_of_the_suffix_form_returns(self) -> None:
        """The SUFFIX form must not come back.

        A rule of the form `*mpm *` matches any command text whose
        last token ends in the name, so it refuses `grep -rn mpm .`
        - a search, not an invocation, and one a review needs.

        The word shape is therefore space-anchored (`* mpm *`), and
        the path shape is end-anchored (`*/mpm *`), so a name that
        merely appears inside a word is not swallowed. This test
        fails if the suffix form returns.

        The path shape's own cost - `ls /path/to/mpm` and `ls
        cmd/mpm` are refused - is measured and stated in the SPEC
        and in the definition's own prose. It is the price of
        closing the write route, and it is paid deliberately.
        """
        denials = _shell_denials()
        binaries = _mpm_binaries()
        for binary in binaries:
            for pattern in (f"*{binary} *", f"*{binary})*"):
                self.assertNotIn(
                    pattern,
                    denials,
                    f"the shell rule {pattern!r} refuses every command whose "
                    f"text ends in the {binary} name - including a search "
                    f"for it, such as `grep -rn {binary} .`. Use the "
                    f"space-anchored form '* {binary} *' and the path form "
                    f"'*/{binary} *' instead",
                )
            # The `?` form must not be PRESENT at all - not merely
            # absent as a replacement.  It is the wrong SHAPE, and the
            # host expands `*` before `?`, so it does not close the
            # bare path: `go run ./cmd/{binary}` matched no rule and ran
            # the binary.  ADDING it alongside the right form would
            # leave the guard green while the definition carried a
            # rule that means the opposite of what it appears to mean.
            self.assertNotIn(
                f"*/{binary} ?*",
                denials,
                f"the shell rule '*/{binary} ?*' is present; it is the wrong "
                f"SHAPE - the host expands `*` before `?`, so it compiles to "
                f"a pattern that needs a space AND one more character "
                f"after the name, and `go run ./cmd/{binary}` with nothing "
                f"after the name matched no rule at all and RAN the binary. "
                f"Remove it; the end-anchored '*/{binary} *' closes the route",
            )

    def test_the_rule_set_denies_what_it_claims_to_deny(self) -> None:
        """The rule set must DENY the commands it claims to deny.

        The other assertions in this file check that certain RULE
        STRINGS are present.  That cannot tell a rule that works from
        a rule that is merely spelled the right way: the `*/mpm ?*`
        form was present, every assertion here was green, and
        `go run ./cmd/mpm` matched no rule at all and RAN the binary.

        So this test MODELS the host's matcher and asserts the
        VERDICT, for a corpus whose every outcome was measured against
        the installed definition before it was written here.

        A command the definition claims to close must be denied; a
        command the definition states is OPEN must stay open.  Both
        directions matter: the second is what keeps the residual list
        honest, because a residual that the rules have since closed is
        a claim that has gone stale.
        """
        rules = _shell_rules()
        binaries = _mpm_binaries()
        self.assertGreater(
            len(binaries), 0, "the Makefile names no MPM binary; the check is vacuous"
        )

        must_deny = [
            # the command position
            "mpm --version",
            "mpm-mcp --version",
            "mpm-lint --version",
            "mpm-critic --version",
            "mpm-scheduler --version",
            "mpm-telemetry --version",
            "mpm-update --version",
            # the name as its own word, behind an ordinary prefix
            "env mpm --version",
            "sh -c 'mpm --version'",
            "xargs mpm --version",
            "echo mpm",
            "echo $(echo mpm)",
            # the name inside a quote
            '"mpm" --version',
            "'mpm' --version",
            # the name as the last segment of a path, with and without
            # more of the command, with and without a trailing slash
            "go run ./cmd/mpm --version",
            "go run ./cmd/mpm",
            "go run ./cmd/mpm/",
            "go build -o /tmp/x ./cmd/mpm",
            "/opt/mpm/bin/mpm --version",
            # the name inside a GO PACKAGE PATTERN, which puts
            # something after the name and so escaped the
            # last-segment shapes — in EVERY spelling of the pattern,
            # not only the canonical one, and with or without
            # anything in FRONT of the name
            "go run ./cmd/mpm/...",
            "go run ./cmd/mpm/... --version",
            'go run ./cmd/mpm/... remember "probe"',
            "go list ./cmd/mpm/...",
            "go run ./cmd/mpm-mcp/...",
            "go run ./cmd/mpm-lint/...",
            "go run ./cmd/mpm/./...",
            "go run ./cmd/mpm//...",
            "go run ./cmd/mpm/././...",
            "go run ./cmd/mpm/../...",
            "go run ./cmd/mpm-mcp/./...",
            "go run ./cmd/mpm-scheduler/./...",
            'go run ./cmd/mpm/./... remember "probe"', 
            "go run mpm/...",
            "go run mpm/... --version",
            # the name inside a substitution the host descends into:
            # a backtick and a process substitution as well as `$( )`
            "echo `mpm --version`",
            'echo "$(mpm --version)"',
            'echo "`mpm --version`"',
            "echo $(mpm; echo hi)",
            "echo $(echo $(mpm))",
            # a command the host cannot ANALYSE is refused outright
            "echo <(mpm --version)>",
            "echo <(echo hello)>",
            "echo (mpm --version)",
            "echo `echo mpm`",
            # a `case` STATEMENT's BODY is matched
            "case x in *) mpm --version;; esac",
            # a REDIRECTION with an EMPTY TARGET is refused by the
            # host's scanner, whether or not it names anything
            "echo mpm>",
            "echo hi >",
            # a REDIRECTION with a descriptor target and NO target at
            # all — the host refuses the second as `invalid-redirect`
            "echo hi 2>&",
            # the command INSIDE a subshell is matched
            "(mpm --version)",
            # a `while`/`until` CONDITION is a command list and IS
            # matched, while a `for`/`select` header is a word list
            # and is not
            "while mpm; do echo hi; done",
            "while mpm --version; do echo hi; done",
            "until mpm; do echo hi; done",
            "until mpm --version; do echo hi; done",
            "while echo mpm; do echo hi; done",
            "until echo mpm; do echo hi; done",
            "while mpm > /dev/null; do echo hi; done",
            "while $(mpm); do echo hi; done",
            # the host descends into a substitution ANYWHERE, a loop
            # header included
            "for i in $(echo mpm); do echo hi; done",
            "for i in `echo mpm`; do echo hi; done",
            "case x in $(echo mpm)) echo hi;; esac",
            # a FUNCTION DEFINITION is analysed, and its BODY is
            # matched
            "f() { mpm --version; }",
            # a `case` whose SUBJECT is a CONTEXT is a construct the
            # host's scanner cannot analyse at all - and the form
            # that names NO binary is refused the same way, so the
            # refusal is about the construct
            "case [[ mpm == x ]] in y) echo hi;; esac",
            "case [[ x == y ]] in z) echo hi;; esac",
            "case (( x )) in z) echo hi;; esac",
            "case [[ mpm == x ]]) echo hi;; esac",
            # a here-document's BODY is DATA for the command analysis
            # and still TEXT for the rule matcher, so a body that
            # carries the word shape is refused
            "cat <<EOF\necho mpm --version\nEOF",
            "cat <<EOF\necho $(( mpm ))\nEOF",
            "cat <<EOF\n\tmpm --version\nEOF",
            "cat <<EOF\nmpm\t--version\nEOF",
            "cat <<'EOF'\n\tmpm --version\nEOF",
            "cat <<-EOF\n\tmpm --version\n\tEOF",
            # a here-document whose TERMINATOR is not found - because
            # it is indented, or because there is none - is a
            # construct the host cannot analyse
            "cat <<EOF\nmpm --version\n  EOF",
            "cat <<-EOF\nmpm --version\n  EOF",
            "cat <<EOF\nmpm --version",
            # the name inside a process substitution is matched, in a
            # conditional and in argument position alike
            "[[ -n <(mpm --version) ]]",
            "echo [[ -n <(mpm --version) ]]",
            "echo <(mpm --version)",
            # a process substitution after a command-position context
            # is a structure the host cannot analyse
            "[[ x == y ]] <(echo hi)",
            # and the `>` that follows one has no target
            "echo <(mpm --version)>",
            "echo <(echo hello)>",
            # a CONTEXT the host cannot PARSE is unanalysable: a `;`
            # inside it, or a context that is never closed
            "[[ x; y ]]",
            "((x;y))",
            "[[ x",
            # a MALFORMED construct is a construct the host cannot
            # ANALYSE, and every one of these names NO binary at all -
            # so the refusal is about the CONSTRUCT, not about a name
            "case x in ) echo hi;; esac",
            "case in y) echo hi;; esac",
            "case x in |) echo hi;; esac",
            "case x in y) echo hi;;",
            "case x in y) echo hi;; esac extra",
            "if ; then echo hi; fi",
            "while ; do echo hi; done",
            "until ; do echo hi; done",
            "{ }",
            "( )",
            "$(",
            "<(",
            "f()",
            # TRAILING TEXT after a closed compound construct is a
            # construct the host cannot analyse - and none of these
            # names a binary
            "if true; then echo hi; fi extra",
            "{ echo hi; } extra",
            "( echo hi ) extra",
            "f() { echo hi; } extra",
            # a `case` whose pattern list holds a QUOTED EMPTY pattern
            'case x in "" ) echo hi;; esac',
            # an empty group NESTED, or spread over two LINES
            "{ { } }",
            "{\n}",
            # an `if` whose CONDITION is only a COMMENT
            "if # comment\nthen echo hi; fi",
            # a `)` inside a QUOTED STRING does not close a
            # substitution, so these are never closed at all
            'echo <(echo "a)b"',
            'echo $(echo "a)b"',
            "echo <(echo 'a)b'",
            # an empty PAREN group, with or without a space, and an
            # empty brace group in ARGUMENT position
            "()",
            "( ) ; echo hi",
            "echo hi | ( )",
            "echo { }",
            # a FUNCTION DEFINITION wherever it stands
            "echo f()",
            # a `case` whose FIRST pattern is empty
            "case x in ''|y) echo hi;; esac",
            # ── the twenty-first review: the STRUCTURAL class ──
            # an unmatched delimiter
            "}",
            "echo }",
            ")",
            "echo )",
            # an unclosed group
            "{ echo hi",
            "( echo hi",
            "f() { echo hi;",
            # an empty command slot - the `;` has to be preceded by a
            # command, and the slot between two `;` is empty
            ";",
            ";;",
            "echo hi;;",
            "echo hi;; echo bye",
            # a brace group whose FIRST slot is empty
            "{ ; }",
            "{ ; echo hi; }",
            "echo { ; }",
            # a GROUP is a compound command, so it may only stand
            # where a command is expected
            "echo a { echo hi; }",
            "echo hi { echo hi; }",
            "true { echo hi; }",
            "echo 'a' { echo hi; }",
            'echo "a" { echo hi; }',
            'echo "" { echo hi; }',
            'echo "a" ( echo hi )',
            # a quoted or empty `coproc` NAME is not a name
            'coproc "" { echo hi; }',
            'coproc mp""m { echo hi; }',
            'echo mp""m { echo hi; }',
            # an EMPTY for/select header
            "for ; do echo hi; done",
            "select ; do echo hi; done",
            "select ; do done",
            # a `case` whose SUBJECT is a command
            "case echo x in y) echo hi;; esac",
            # a compound command that is never CLOSED
            "if true; then echo hi",
            "while false; do echo hi",
            "for i in 1; do echo hi",
            # an EMPTY body
            "if true; then fi",
            "while false; do done",
            "for i in 1; do done",
            "if true; then echo hi; else fi",
            # an EMPTY body, in the NEWLINE spelling, and with a
            # separator between the body keyword and the closer
            "if true; then\nfi",
            "while false; do\ndone",
            "for i in 1; do\ndone",
            "if true; then ; fi",
            "while false; do ; done",
            # a closer with NO SEPARATOR before it
            "if true; then echo hi fi",
            # a `case` whose PATTERN LIST has no `)` and no `esac`
            "case x in",
            "case x in y z",
            # a `case` that is closed, with TEXT after it
            "case x in esac extra",
            "case x in esac ;; esac",
            # an unterminated quote or escape
            "echo 'unclosed",
            'echo "unclosed',
            "echo hi \\",
            # a bare function definition in ARGUMENT position
            "echo a()",
            "a() b()",
            # the FIRST pattern of a `case` has to be non-empty
            "case x in |y) echo hi;; esac",
            "case x in ''|y) echo hi;; esac",
            "case x in ''|'') echo hi;; esac",
            # a construct DIRECTLY AFTER a closed compound construct is
            # refused as well - only a separator may follow one
            "(echo hi) (echo bye)",
            "(echo hi) echo bye",
            "if true; then echo hi; fi (echo bye)",
            "{ echo hi; } (echo bye)",
            "case x in y) echo hi;; esac (echo bye)",
            "while true; do echo hi; done extra",
            "for i in 1; do echo hi; done extra",
            # a CONTINUATION keyword with nothing to continue is a
            # construct the host cannot analyse - and the form that
            # names NO binary is refused the same way
            "do [[ mpm == x ]]",
            # the DEPTH has to be RESTORED: a keyword after a
            # construct that was closed continues nothing
            "if true; then echo a; fi; do echo hi",
            "if true; then echo a; fi; then echo hi",
            "if true; then echo a; fi; fi",
            "if true; then echo a; fi; done",
            "while true; do echo a; done; do echo hi",
            "do echo hi",
            "do [[ x == y ]]",
            "do",
            "then echo hi",
            "else [[ mpm == x ]]",
            "elif echo hi",
            "fi echo hi",
            # a CONTEXT in ARGUMENT position is ordinary text, and
            # the word shape matches it
            "echo $(( mpm ))",
            "echo $(( mpm + 1 ))",
            "echo $(( 1 + mpm ))",
            "echo a $(( mpm )) b",
            "echo [[ mpm == x ]]",
            "if true; then echo $(( mpm )); fi",
            "f() { echo $(( mpm )); }",
            "for i in 1; do echo $(( mpm )); done",
            # an `alias` DEFINES a command at run time, and the host
            # refuses it as `dynamic-execution`
            "alias NAME=value",
            # the ANSI-C QUOTING form decodes the escape inside the
            # quoted word, so the text carries no name
            "$'\\x6dpm' --version",
            "$'\\155\\160\\155' --version",
            # the name delimited by a TAB, which the shell splits on
            "mpm\t--version",
            "env mpm\t--version",
            "echo\tmpm",
            # the write route
            'mpm remember "probe"',
            'go run ./cmd/mpm remember "probe"',        ]
        for command in must_deny:
            self.assertTrue(
                _matcher_denies(command, rules),
                f"the rule set does NOT deny {command!r}; the name reaches the "
                f"binary through a spelling the rules do not cover",
            )

        must_allow = [
            # the WELL-FORMED form of every malformed construct above
            # is allowed - so the model's class rule refuses the
            # broken forms without refusing ordinary shell
            "case x in y) echo hi;; esac",
            "case x in y|z) echo hi;; esac",
            "case y in z) case w in q) echo hi;; esac;; esac",
            "if true; then echo hi; fi",
            "while false; do echo hi; done",
            "until false; do echo hi; done",
            "{ echo hi; }",
            "( echo hi )",
            "f() { echo hi; }",
            "$(echo hi)",
            "<(echo hi)",
            "for ((i=0;i<1;i++)); do echo hi; done",
            "select ((i=0;i<1;i++)); do echo hi; done",
            # the WELL-FORMED form of each trailing-text row: the
            # construct CLOSED, and nothing after it
            "if true; then echo hi; fi",
            "{ echo hi; }",
            "( echo hi )",
            "f() { echo hi; }",
            # a pattern list that holds a NON-EMPTY pattern is fine,
            # even when one of its patterns is empty
            "case x in y|) echo hi;; esac",
            # a keyword that is an ARGUMENT is not a condition
            "echo if; echo hi",
            "echo while; echo hi",
            "echo until; echo hi",
            # a function definition whose BODY follows is fine
            "f()\n{ echo hi; }",
            # a LATER pattern of a `case` may be empty
            "case x in y|'') echo hi;; esac",
            "case x in y|'') echo hi;; esac",
            # an empty command substitution is not an empty group, and
            # neither is a parameter expansion or a `{}` word
            "echo $( )",
            "echo ${}",
            "echo {}",
            # a SEPARATOR after a closed compound construct is fine
            "case x in y) echo hi;; esac; echo bye",
            "if true; then echo hi; fi; echo bye",
            "(echo hi); (echo bye)",
            "case x in y) echo hi;; esac; (echo bye)",
            # `{}` is an ordinary WORD, and an empty command
            # substitution is not a group
            "{}",
            "x=$( )",
            # a LATER pattern of a `case` may be empty
            "case x in y|'') echo hi;; esac",
            "case x in y|''|z) echo hi;; esac",
            # ── the twenty-first review: the well-formed forms ──
            # a quote pair inside a longer word is fine - the host
            # removes the quotes and continues
            'echo mp""m',
            'echo m""pm',
            'echo a"b"c',
            'echo ""',
            "x=''",
            "x='a'",
            'echo a"b"',
            'echo "a"b',
            'echo mp"m"',
            'echo "mp"m',
            # a TRAILING empty slot is fine
            "echo hi;",
            # a `;` inside a `case` separates two case ITEMS
            "case x in y) echo hi; ;; esac",
            # `{}` is an ordinary WORD
            "{}",
            "echo {}",
            # an empty command substitution is not a group
            "echo $( )",
            "x=$( )",
            # a quoted word beside a pipe or an `&&` is fine
            'echo "a" | cat',
            'echo "a" && echo hi',
            "echo a && echo hi",
            # a quoted word inside a GROUP is fine
            '{ echo "a"; }',
            '{ echo "a"; echo hi; }',
            'echo "a" ;',
            'echo "a" > /dev/null',
            'echo "a" &',
            # a GROUP in command position is fine
            'echo "a"; { echo hi; }',
            # a here-document's BODY is DATA - the host does not
            # analyse it at all
            "cat <<EOF\n}\nEOF",
            "cat <<EOF\n)\nEOF",
            "cat <<EOF\necho 'unclosed\nEOF",
            # a `case` whose PATTERN LIST holds a substitution or a
            # quoted `)` - the list's own `)` comes after them
            "case x in $(echo y)) echo hi;; esac",
            'case x in "y)z") echo hi;; esac',
            # a `case` with an EMPTY pattern list, closed by its own
            # `esac`
            "case x in esac",
            # a body that is not empty, in the NEWLINE spelling
            "if true; then\necho hi\nfi",
            # a search for the name, unquoted, is not an invocation
            "grep -rn mpm_memory .",
            'echo "the mpm_memory tool"',
            "echo the_mpm_memory_tool",
            # a file INSIDE a directory named after the binary
            "cat cmd/mpm/main.go",
            "ls /opt/checkout/mpm/docs",
            # a REDIRECTION with a DESCRIPTOR TARGET — the `&` of
            # `>&` is part of the operator, not a separator, and
            # `cmd > log 2>&1` is the commonest idiom there is
            "echo hi 2>&1",
            "echo hi >&1",
            "echo hi >& out",
            "echo hi >&-",
            "echo hi > out.log 2>&1",
            "echo hi 2>&1 | cat",
            "make build > build.log 2>&1",
            # a SUBSHELL at the command position is ANALYSED, and it
            # names nothing
            "(echo hi)",
            "(echo hello)",
            "(echo hi) && echo yes",
            # a `for`/`select` header is a WORD LIST and is not
            # matched — the `for` counterpart of the `while` forms
            # the rule set denies
            "for mpm; do echo hi; done",
            "select mpm in a b; do echo hi; done",
            # a FUNCTION DEFINITION is not a compound command: the
            # host analyses the BODY
            "f() { echo hi; }",
            "function f() { echo hi; }",
            # a `(` after a KEYWORD opens a command, not an argument
            "if true; then (echo hi); fi",
            # a `[[ … ]]` conditional and an arithmetic context are
            # CONTEXTS - their operands are words, not commands - but
            # only when the context IS the command
            "[[ mpm == x ]]",
            "((mpm))",
            "(( mpm ))",
            "[[ -n mpm ]]",
            "[[ mpm ]]",
            "if [[ mpm == x ]]; then echo hi; fi",
            "if (( mpm )); then echo hi; fi",
            # a BARE `alias` lists the aliases and defines nothing,
            # and the host allows it
            "alias",
            # a CONTEXT is a construct in more positions than the
            # command position: the `else` branch of an `if`, a brace
            # group, a `time` prefix, a `!` prefix
            "if true; then echo a; else [[ mpm == x ]]; fi",
            "if true; then echo a; else (( mpm )); fi",
            "{ [[ mpm == x ]]; }",
            "time [[ mpm == x ]]",
            "! (( mpm ))",
            # a `case` whose PATTERN list carries a context is fine,
            # and so is a `$( ... )` substitution in the subject
            "case x in y) echo hi;; esac",
            "case x in [[ y ]]) echo hi;; esac",
            "case $(( x )) in z) echo hi;; esac",
            "for i in [[ mpm == x ]]; do echo hi; break; done",
            # a `#` COMMENT is not text the rules are matched against
            "echo hi # mpm",
            "echo hi # mpm --version",
            # a PROCESS SUBSTITUTION is a command, and the names
            # inside it are matched - but an ARITHMETIC context is not
            # analysed, so a process substitution inside one is not
            "[[ -n <(echo hi) ]]",
            "(( <(echo hi) ))",
            "echo [[ -n <(echo hi) ]]",
            "echo <(echo hello)",
            "echo hi <(echo hello)",
            "cat <(echo hi)",
            "if [[ -n <(echo hi) ]]; then echo hi; fi",
            "[[ -n <(echo hi) ]] && echo hi",
            # and an ARITHMETIC context hides it
            "(( <(mpm --version) ))",
            # a HERE-STRING's target is a WORD, not a here-document
            "cat <<< \"hi there\"",
            # a here-document inside a LOOP body or a `case` branch
            # body is ALLOWED: the body is data for the command
            # analysis, and it is not re-scanned
            "for i in 1; do\ncat <<EOF\nhi\nEOF\ndone",
            "while false; do\ncat <<EOF\nhi\nEOF\ndone",
            "case x in y)\ncat <<EOF\nhi\nEOF\n;; esac",
            # a here-document's BODY is DATA for the command analysis
            # - so the body is not a command - and the rule matcher
            # still sees its TEXT
            "cat <<EOF\nmpm\nEOF",
            "cat <<EOF\necho mpm\nEOF",
            "cat <<EOF\nmpm --version\nEOF",
            "cat <<EOF\nmpm --version\n\nEOF",
            "cat <<'EOF'\nmpm --version\nEOF",
            "cat <<-EOF\nmpm --version\nEOF",
            # the routes the residual list names, which stay OPEN
            "echo $(echo mp)m",
            # a name split by a QUOTE is the same family: the matcher
            # sees the text, and the text does not hold the name
            'mp""m --version',
            'm"pm" --version',
            "M=mpm; $M --version",
            "cd cmd/mpm && go run . --version",
            # a SHELL GLOB expands to the name before the matcher
            # sees it, so the text is not the name
            "go run ./cmd/mp?",
            "go build -o /tmp/x ./cmd/mp?",
            # a `case` STATEMENT is ONE construct whose header — the
            # pattern list included — is not matched, exactly like a
            # loop's header
            "case x in mpm) echo hi;; esac",
            'case x in "mpm") echo hi;; esac',
            "case mpm in mpm) echo hi;; esac",
            # the shell removes a BACKSLASH ESCAPE before it parses
            # the command word, and the matcher sees the raw text
            "m\\pm --version",
            "m\\\npm --version",
            # a name GLUED to punctuation rather than delimited by
            # whitespace
            "${x:-mpm} --version",
            "for i in mpm; do $i --version; done",
            "echo ${x:-mpm}",
            "echo a,mpm",
            # a command that mentions nothing at all
            "echo probe-alive",
            "true",        ]
        for command in must_allow:
            self.assertFalse(
                _matcher_denies(command, rules),
                f"the rule set denies {command!r}, which the definition states "
                f"is allowed; a residual that has been closed is a claim that "
                f"has gone stale",
            )

    def test_the_recorded_divergences_are_pinned(self) -> None:
        """The RECORDED divergences must be refused, and their COUNT pinned.

        The documents publish how many commands the host allows and the
        model refuses.  That number is a claim, and a claim that is
        not checked is a number that drifts - so the ledger lives HERE,
        every entry is asserted to be refused in the RECORDED direction
        (the host allows it, so the model refusing it is the safe
        side), and the SPEC is asserted to publish the length of this
        ledger.

        A NEW divergence therefore has two places to go: this ledger,
        and the corpus - and the documents follow both.
        """
        rules = _shell_rules()
        self.assertGreater(
            len(_DIVERGENCES), 0, "the divergence ledger is empty, so the "
            "published count is vacuous",
        )
        for command in _DIVERGENCES:
            self.assertTrue(
                _matcher_denies(command, rules),
                f"the model no longer refuses {command!r}, so it is no longer "
                f"a divergence; the recorded direction has changed and the "
                f"documents have to be updated",
            )
        spec = (
            Path(__file__).resolve().parents[2] / "docs" / "SPEC.md"
        ).read_text(encoding="utf-8")
        word = _NUMBER_WORDS.get(len(_DIVERGENCES))
        self.assertIsNotNone(
            word,
            f"the ledger holds {len(_DIVERGENCES)} divergences and there is no "
            f"number word for that in the guard",
        )
        self.assertIn(
            f"it refuses {word} commands the host allows",
            spec,
            f"docs/SPEC.md does not publish {word} recorded divergences; the "
            f"guard's ledger holds {len(_DIVERGENCES)}",
        )

    def test_the_definition_does_not_claim_the_capability_loader_is_closed(self) -> None:
        """The definition must not claim a closure it does not have.

        The host does not gate the `skill` tool by any permission
        action: measured here with eighteen candidate action names and
        with the V1-style `tools:` frontmatter, the loader stayed in
        the reviewer's catalogue and a `skill` call ran.

        A rule that does not work is not a closure, so there is no
        rule - and a definition that still CLAIMS the closure is
        making a false statement about a barrier. This test fails if
        the claim comes back.
        """
        text = REVIEWER.read_text(encoding="utf-8")
        self.assertNotIn(
            "action: skill\n",
            text,
            "the reviewer denies the `skill` action, but the host does not "
            "gate the capability loader by it; the rule claims a closure "
            "that does not exist",
        )
        body = text[text.find("\n---\n", 4):]
        self.assertNotIn(
            "you cannot load a capability",
            body,
            "the reviewer definition claims the capability loader is closed; "
            "the host does not gate it, so the claim is false",
        )

    def test_the_reviewer_asks_no_question(self) -> None:
        """The `question` action must be closed explicitly."""
        self.assertIn("question", _denied_actions())

    def test_the_reviewer_has_no_code_mode(self) -> None:
        """Code Mode reaches every tool the host publishes.

        Denying the nested tools' own actions is enough for the ones
        this definition names, but closing the bridge as well means a
        route added later is not open by default.
        """
        self.assertIn("execute", _denied_actions())

    # ── The command line is closed too ────────────────────────────────

    def test_the_shell_cannot_reach_the_mpm_binaries(self) -> None:
        """The MPM binaries are refused at the COMMAND POSITION.

        A shell rule's resource is the raw command text and the match
        is over the WHOLE value, so `mpm *` matches a command that
        BEGINS with `mpm ` and does not match a command that merely
        mentions the word. The rules are therefore anchored at the
        command position - and every MPM binary is covered, not just
        the CLI.
        """
        denials = _shell_denials()
        binaries = _mpm_binaries()
        self.assertGreater(
            len(binaries), 0, "the Makefile names no MPM binary; the check is vacuous"
        )
        for binary in binaries:
            for pattern in (binary, f"{binary} *"):
                self.assertIn(
                    pattern,
                    denials,
                    f"the shell rule {pattern!r} is missing; the reviewer could "
                    f"run the {binary} binary against the operator's workspace",
                )
            self.assertIn(
                f"*/bin/{binary}",
                denials,
                f"the shell rule '*/bin/{binary}' is missing; the reviewer could "
                f"run {binary} by its installed path",
            )

    def test_the_shell_cannot_reach_a_prefixed_binary(self) -> None:
        """A command-position rule is defeated by a ONE-WORD PREFIX.

        Measured on this host with only the command-position rules in
        force: `env mpm --version` RAN, `sh -c 'mpm --version'` RAN,
        and `env mpm remember "..."` wrote a memory into the
        substrate's live database. So every binary is also refused
        WHEREVER ITS NAME APPEARS, and the name is caught in the forms
        a shell command can put it in: as its own word, followed by a
        closing parenthesis, as the last segment of a path, and inside
        a quote.

        The first two are per binary; the quoted pair is shared,
        because every binary's name starts with `mpm`.

        The PATH shapes are the ones that close the write route, and
        their form is not obvious. The host expands `*` BEFORE `?`, so
        a rule of the form `*/mpm ?*` compiles to a pattern that needs
        a space AND one more character after the name - and MEASURED,
        `go run ./cmd/mpm` and `go run ./cmd/mpm/`, with nothing after
        the name, matched no rule at all and RAN the binary. The rule
        that closes them is the END-ANCHORED `*/mpm *`, with
        `*/mpm/` for the trailing-slash spelling. `go run ./cmd/mpm
        remember "..."` and `go build -o /tmp/x ./cmd/mpm` are the
        same live-substrate write the prefix bypass was.

        The SPACE BEFORE the name in the word shape is load-bearing. A
        rule of the form `*mpm *` also matches a PATH whose last
        segment is the name, so it refuses `ls /path/to/mpm` - the
        operator's own checkout, read-only, for no reason a review
        cares about. Requiring a space before the name drops that
        denial from the WORD shape; the PATH shapes close the route
        that matters instead, and they cost `ls /path/to/mpm` and
        `ls cmd/mpm`, which is measured and stated rather than
        assumed.
        """
        denials = _shell_denials()
        binaries = _mpm_binaries()
        for binary in binaries:
            for pattern in (f"* {binary} *", f"* {binary})*"):
                self.assertIn(
                    pattern,
                    denials,
                    f"the shell rule {pattern!r} is missing; a prefix or a "
                    f"substitution would carry the reviewer past the "
                    f"command-position rules and into the {binary} binary",
                )
            # The PATH shapes. The end-anchored form is what closes
            # `go run ./cmd/mpm` - the spelling with NOTHING after the
            # name, which the old `*/mpm ?*` form did not match, and
            # which was MEASURED to run the binary.
            for path_pattern in (
                f"*/{binary} *",
                f"*/{binary}/",
                f"*{binary}/*...*",
            ):
                self.assertIn(
                    path_pattern,
                    denials,
                    f"the shell rule {path_pattern!r} is missing; `go run "
                    f"./cmd/{binary}` names the binary as the last segment "
                    f"of a path rather than at the command position, and "
                    f"would reach the substrate",
            )
        for pattern in ('*"mpm*', "*'mpm*"):
            self.assertIn(
                pattern,
                denials,
                f"the shell rule {pattern!r} is missing; a quoted binary name "
                f"would carry the reviewer past the command-position rules",
            )

    def test_the_shell_cannot_reach_the_installers(self) -> None:
        """The installers are refused at the command position.

        `./install.sh` is the canonical "build plus systemd plus PATH
        symlinks" installer. A rule of the form `* install.sh*` needs a
        SPACE before the name, so it misses `./install.sh` - which is
        the form the installer is actually run in.

        The harm is RUNNING the installer, not READING it: a rule of
        the form `*/install.sh` also refuses `ls -la
        agent_installation/mpm-opencode/install.sh`, which is a review
        of this repository's own installer. So the path forms are
        closed at the command position and through the interpreters
        that can run a script file.
        """
        denials = _shell_denials()
        for pattern in (
            "install.sh",
            "install.sh *",
            "./install.sh",
            "./install.sh *",
            "sh install.sh *",
            "bash install.sh *",
            "sh */install.sh *",
            "bash */install.sh *",
            "uninstall.sh",
            "uninstall.sh *",
            "./uninstall.sh",
            "./uninstall.sh *",
            "sh uninstall.sh *",
            "bash uninstall.sh *",
            "sh */uninstall.sh *",
            "bash */uninstall.sh *",
            "make install*",
            "make uninstall*",
            "make deploy*",
            "make -C * install*",
            "make -C * uninstall*",
            "make -C * deploy*",
        ):
            self.assertIn(
                pattern,
                denials,
                f"the shell rule {pattern!r} is missing; the reviewer could "
                "install or uninstall the substrate",
            )

    def test_the_shell_cannot_reach_the_substrate(self) -> None:
        """The command line is the other way into the substrate.

        This is the historical assertion of this file, restated for the
        rules that exist now. It no longer names the `mpm-memory`
        pattern: no such binary exists in this repository, so the rule
        was a straw man that denied innocent commands whose text
        happened to contain the string.
        """
        denials = _shell_denials()
        for pattern in ("mpm", "mpm *"):
            self.assertIn(
                pattern,
                denials,
                f"the shell rule {pattern!r} is missing; the reviewer could "
                "reach the substrate from the command line",
            )
        for straw_man in ("*mpm-memory *", "*mpm_memory*"):
            self.assertNotIn(
                straw_man,
                denials,
                f"the shell rule {straw_man!r} denies commands that merely "
                "mention a name that is not a binary in this repository",
            )

    # ── The live workspace is closed to the read tool ─────────────────

    def test_the_reviewer_cannot_read_a_live_workspace(self) -> None:
        """A runtime root is not the reviewer's to read.

        Both spellings are required: an external path is matched as a
        canonical absolute path, and a path inside the working tree is
        matched location-relative.
        """
        denials = _read_denials()
        for pattern in ("*/.mpm/*", "*/.mpm", ".mpm/*", ".mpm"):
            self.assertIn(
                pattern,
                denials,
                f"the read rule {pattern!r} is missing; the reviewer could "
                "read the operator's live MPM workspace",
            )

    # ── The definition is installed, and may not drift ────────────────

    def test_the_installer_resolves_the_canonical_template(self) -> None:
        """The installer must ship THIS template, not a copy that can drift.

        The assertion is on the installer's RESOLVED path, not on the
        text of its source: the old check grepped the installer's
        docstring for the template path, so it passed even when the
        path-resolution code pointed somewhere else entirely.
        """
        self.assertTrue(
            INSTALLER.exists(),
            "the reviewer agent has no installer; a definition nobody installs "
            "is not an isolation",
        )
        module = _load_installer()
        resolved = Path(module.TEMPLATE).resolve()
        self.assertEqual(
            REVIEWER.resolve(),
            resolved,
            "the installer does not ship the canonical reviewer template; "
            "the installed definition could be a different file",
        )

    def test_the_canonical_install_flow_installs_the_reviewer(self) -> None:
        """The definition must be installed by the CANONICAL flow.

        A definition that only exists if an operator remembers an
        extra, undocumented script is not an isolation: a fresh install
        would have no reviewer definition at all.
        """
        self.assertTrue(
            INSTALL_SH.exists(),
            "the OpenCode installer is missing",
        )
        install_source = INSTALL_SH.read_text(encoding="utf-8")
        self.assertIn(
            "install_reviewer_agent.py",
            install_source,
            "the canonical install flow does not install the reviewer "
            "definition; a fresh install would have no isolation",
        )
        makefile_source = MAKEFILE.read_text(encoding="utf-8")
        self.assertIn(
            "install_reviewer_agent.py",
            makefile_source,
            "the Makefile does not install the reviewer definition",
        )

    def test_the_installed_copy_matches_the_template(self) -> None:
        """The INSTALLED definition may not differ from the template.

        On a host where the definition has never been installed there is
        nothing to compare, and the test says so. On a host where it IS
        installed, a difference means the isolation that is actually in
        force is not the one this repository defines - which is the
        failure this guards, and the reason the canonical install flow
        now installs the definition.
        """
        installed = Path.home() / ".config" / "opencode" / "agents" / "mpm-reviewer.md"
        if not installed.exists():
            self.skipTest(f"the reviewer definition is not installed at {installed}")
        self.assertEqual(
            REVIEWER.read_text(encoding="utf-8"),
            installed.read_text(encoding="utf-8"),
            f"the installed reviewer definition at {installed} differs from the "
            "template; the isolation in force is not the one this repository "
            "defines. Re-run agent_installation/mpm-opencode/scripts/"
            "install_reviewer_agent.py --force",
        )

    # ── The sandbox is the second layer ───────────────────────────────

    def test_the_sandbox_launcher_exists(self) -> None:
        """The disposable sandbox must exist and be usable.

        The permission layer is a barrier, not a jail: the shell runs
        with the host user's authority, and a rule is a pattern match on
        a command string. The sandbox is the layer that does not depend
        on the pattern being complete - inside it, `HOME`, `TMPDIR` and
        `MPM_WORKSPACE` point at disposable directories, so a command
        that got through cannot write into the operator's runtime root.
        """
        self.assertTrue(
            SANDBOX.exists(),
            "the reviewer sandbox launcher is missing; the permission layer "
            "is a barrier, not a jail",
        )
        self.assertIn(
            "MPM_WORKSPACE",
            SANDBOX.read_text(encoding="utf-8"),
            "the sandbox launcher does not set MPM_WORKSPACE; a sandbox that "
            "does not redirect the substrate's workspace is not a sandbox",
        )
        self.assertIn(
            "reviewer_sandbox.py",
            REVIEWER.read_text(encoding="utf-8"),
            "the reviewer definition does not mention the sandbox launcher",
        )


if __name__ == "__main__":
    unittest.main()
