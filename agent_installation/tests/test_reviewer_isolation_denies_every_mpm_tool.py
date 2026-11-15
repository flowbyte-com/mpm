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
# simple commands; a `cd` segment's text is not matched; `$( ... )`
# substitutions are descended into.
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


def _matcher_segments(command: str):
    """Split a command into the simple commands the host matches."""
    segments, current, depth, quote = [], [], 0, None
    index = 0
    while index < len(command):
        char = command[index]
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
        if char == "(":
            depth += 1
        elif char == ")":
            depth = max(0, depth - 1)
        pair = command[index : index + 2]
        if depth == 0 and pair in ("&&", "||"):
            segments.append("".join(current))
            current = []
            index += 2
            continue
        if depth == 0 and char in ";|":
            segments.append("".join(current))
            current = []
            index += 1
            continue
        current.append(char)
        index += 1
    segments.append("".join(current))
    return segments


def _matcher_substitutions(segment: str):
    """The text inside each $( ... ) substitution."""
    index = 0
    while True:
        index = segment.find("$(", index)
        if index < 0:
            return
        depth, end = 0, index + 1
        while end < len(segment):
            if segment[end] == "(":
                depth += 1
            elif segment[end] == ")":
                depth -= 1
                if depth == 0:
                    break
            end += 1
        yield segment[index + 2 : end]
        index = end + 1


def _matcher_denies(command: str, rules) -> bool:
    """Would the host deny this command, given these (resource, effect)?"""
    for segment in _matcher_segments(command):
        candidates = [segment]
        candidates.extend(_matcher_substitutions(segment))
        for candidate in candidates:
            words = candidate.strip().split()
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
            # the name delimited by a TAB, which the shell splits on
            "mpm\t--version",
            "env mpm\t--version",
            "echo\tmpm",
            # the write route
            'mpm remember "probe"',
            'go run ./cmd/mpm remember "probe"',
        ]
        for command in must_deny:
            self.assertTrue(
                _matcher_denies(command, rules),
                f"the rule set does NOT deny {command!r}; the name reaches the "
                f"binary through a spelling the rules do not cover",
            )

        must_allow = [
            # a search for the name, unquoted, is not an invocation
            "grep -rn mpm_memory .",
            'echo "the mpm_memory tool"',
            "echo the_mpm_memory_tool",
            # a file INSIDE a directory named after the binary
            "cat cmd/mpm/main.go",
            "ls /opt/checkout/mpm/docs",
            # the routes the residual list names, which stay OPEN
            "echo $(echo mp)m",
            "M=mpm; $M --version",
            "cd cmd/mpm && go run . --version",
            # a command that mentions nothing at all
            "echo probe-alive",
            "true",
        ]
        for command in must_allow:
            self.assertFalse(
                _matcher_denies(command, rules),
                f"the rule set denies {command!r}, which the definition states "
                f"is allowed; a residual that has been closed is a claim that "
                f"has gone stale",
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
            for path_pattern in (f"*/{binary} *", f"*/{binary}/"):
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
