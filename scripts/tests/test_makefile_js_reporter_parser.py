"""
test_makefile_js_reporter_parser.py — Pin the contract that the
`test-agent-adapters` Makefile recipe counts JS passing tests from a
stable, reporter-explicit `node --test` output and rejects empty or
malformed summaries.

Background:
  The recipe used to invoke `node --test 'tests/*.test.js'` without
  an explicit reporter. The default reporter is the version-dependent
  "spec" output on Node >= 22.6 (`ℹ pass N`) and the TAP output on
  Node 18/20.x (`# pass N`). The recipe's parser only understood
  the spec form, so the GitHub Actions ubuntu-24.04 runner (Node
  20.x by default) emitted `# pass 143` and the recipe reported
  "no passing-test count" while the suite had actually passed.

  CI evidence (run 38888336201, line 39264-39272):
    # tests 145
    # pass 143
    # fail 0
    # skipped 2
    [test-agent-adapters] FATAL: mpm-memory-openclaw reported no
    passing-test count.

  The fix is twofold: (1) invoke `node --test --test-reporter=tap
  ...` so the output is stable across every Node >= 18 minor, and
  (2) parse the TAP `# pass N` form. The spec form (`ℹ pass N`) is
  NOT accepted by the parser — the recipe forces `--test-reporter=tap`
  on every invocation, so a parser that also accepted the spec
  form would be re-coupled to a version-dependent default. Any
  input that does not match the TAP `# pass N` line is treated
  as malformed and fails closed.

  This test pins both halves of that contract and the fail-closed
  behaviour the brief required:

    * the recipe MUST use `--test-reporter=tap`
    * the parser MUST accept `# pass N` (TAP)
    * the parser MUST strip ANSI escapes (SGR) and carriage returns
    * the parser MUST refuse the spec form (`ℹ pass N`) so a
      future refactor that "support both formats" gets caught
    * the parser MUST refuse an empty summary, a missing line, a
      malformed line, a zero pass count that is below the floor, a
      non-zero node exit, and a zero node exit without a `# pass`
      line in the output (e.g. node died before printing anything)
    * a pre-fix shape that only matched `ℹ pass` MUST be rejected
      (negative control: this is the regression we just shipped a
      fix for; never let it come back)

Hermeticity:
  No `node` subprocess is spawned. The parse pipeline is a pure
  `sed` expression over a captured string; the test feeds in
  representative outputs (with and without ANSI, with and without
  CR, TAP and spec) and asserts on the parsed count. The recipe's
  exact `sed` command is extracted from the Makefile source so a
  change to the recipe cannot pass these tests without also
  changing the parse contract.
"""

from __future__ import annotations

import re
import subprocess
import tempfile
import textwrap
import unittest
from pathlib import Path

REPO_ROOT = Path(__file__).resolve().parent.parent.parent
MAKEFILE = REPO_ROOT / "Makefile"


def _read_makefile() -> str:
    return MAKEFILE.read_text(encoding="utf-8")


def _extract_recipe(makefile_text: str, target: str) -> str:
    """Mirror of the helper in test_makefile_agent_adapter_wiring.py.

    Returns the joined recipe lines (TAB-indented or backslash-
    continued) for the given target. Empty when the target is
    absent. This is structural, not a parsed AST.
    """
    pattern = re.compile(rf"^{re.escape(target)}\s*:.*$", re.MULTILINE)
    match = pattern.search(makefile_text)
    if not match:
        return ""
    after = makefile_text[match.end():]
    lines: list[str] = []
    first = True
    for raw_line in after.splitlines(keepends=False):
        if first:
            first = False
            continue
        if raw_line.startswith("\t") or raw_line.endswith("\\"):
            lines.append(raw_line)
            continue
        break
    return "\n".join(lines)


def _recipe_invocation_line(recipe: str) -> str:
    """Return the subshell in the recipe that calls `node --test`.

    The recipe is multi-line with backslash continuations; we join
    the lines into a single command string. The line that actually
    launches the JS suite is `out=$$(cd "agent_installation/$$d"
    && node --test 'tests/*.test.js' 2>&1)`. The parser-recipe
    substring lives directly after it on the same joined line.
    """
    joined_lines: list[str] = []
    for line in recipe.splitlines():
        joined_lines.append(line.rstrip("\\\n").strip())
    full = " ".join(joined_lines)
    # Anchor on `out=$$(cd "agent_installation/$$d" && node --test`
    # and pull everything up to and including the closing `)` of
    # the `$$(...)` subshell. `[^)]*` is too narrow (a future
    # change could add a paren-bearing arg); use a non-greedy
    # match for the closing `)` instead, anchored on the literal
    # `2>&1)` that closes the current subshell. If the invocation
    # ever grows past that, this assertion fires first and forces
    # the test to be updated alongside the recipe.
    match = re.search(
        r'out=\$\$\(cd\s+"agent_installation/\$\$d"\s+&&\s+node\s+--test.*?2>&1\)',
        full,
    )
    if not match:
        raise AssertionError(
            "test-agent-adapters recipe must invoke "
            "`node --test` inside a `cd agent_installation/$$d` "
            "subshell that ends with `2>&1)`. "
            f"Joined recipe was:\n{full}"
        )
    return match.group(0)


def _parser_command(recipe: str) -> str:
    """Return the exact `n=...` shell pipeline that extracts the
    JS pass count from the captured `$$out` variable.

    The recipe has TWO `n=$$(...)` pipelines — one for the Python
    suite (`s/^Ran ([0-9][0-9]*) tests?.*/\1/p`) and one for
    the JS suite (the SGR-stripping and `# pass` matcher). This
    helper returns the JS one, anchored on the `${ESC}` variable
    that the JS pipeline uses to strip ANSI escapes. If the recipe
    ever drops the ANSI-stripping step (e.g. switches to a Python-
    based parser) the assertion fires and the test is updated
    alongside the recipe.
    """
    joined = " ".join(
        line.rstrip("\\\n").strip() for line in recipe.splitlines()
    )
    # Anchor on the literal `ESC=$$(printf '\\033');` that
    # immediately precedes the JS `n=$$(...)` pipeline, then
    # match the `n=$$(... | tail -1)` that follows it. This is
    # unambiguous: the Python pipeline does not set ESC first.
    match = re.search(
        r"ESC=\$\$\(printf '\\033'\);\s*n=\$\$\(.*?tail -1\)",
        joined,
    )
    if not match:
        raise AssertionError(
            "test-agent-adapters JS pipeline must follow the "
            "`ESC=$$(printf '\\033')` helper and end with "
            "`tail -1)`. Joined recipe was:\n"
            f"{joined}"
        )
    # Return just the `n=$$(...)` portion so the helper output
    # is a clean shell-pipeline string for `_run_parser_on`.
    return match.group(0).split(";", 1)[1].strip()


def _run_parser_on(parser: str, captured: str) -> str:
    """Execute the recipe's `n=$$(printf ... | sed ... | sed ...)`
    pipeline against a captured output and return the parsed count.

    The Makefile uses `$$` (Makefile escape for `$`) and `${ESC}`
    for the SGR-strip helper. We translate both to their shell
    counterparts and then run the entire pipeline through a single
    `bash -c` invocation. The captured output is supplied through
    a tempfile so escape bytes, CRs, and NULs survive intact; the
    recipe's `printf '%s\n' "$$out"` reads it the same way the
    real recipe reads the in-shell `$$out` variable.
    """
    # Step 1: Makefile -> shell escaping.
    shell = parser.replace("$$", "$").replace("${ESC}", "$ESC")
    # Step 2: write the captured output to a tempfile so the
    # `printf '%s\n' "$out"` substitution below has a real file
    # to read. We then substitute the literal `"$out"` reference
    # with a real filesystem path.
    with tempfile.NamedTemporaryFile(
        "w", suffix=".txt", delete=False
    ) as tf:
        tf.write(captured)
        tmp_path = tf.name
    try:
        shell = shell.replace('"$out"', f'"$(cat {tmp_path})"')
        # The `n=$$(...)` pipeline only assigns a shell variable;
        # its value is not echoed. Append `printf '%s\n' "$n"` so
        # the wrapper's stdout carries the parsed count, exactly
        # the way the recipe's downstream `js_total=$$((... + n))`
        # sees it.
        script = (
            "set -e\n"
            "ESC=$(printf '\\033')\n"
            f"{shell}\n"
            "printf '%s\\n' \"$n\"\n"
        )
        cp = subprocess.run(
            ["bash", "-c", script],
            capture_output=True,
            text=True,
            check=False,
            timeout=30,
        )
        return cp.stdout.strip()
    finally:
        Path(tmp_path).unlink(missing_ok=True)


class TestRecipeUsesExplicitTapReporter(unittest.TestCase):
    """`node --test` defaults are version-dependent. Pin the recipe
    to the stable explicit form.
    """

    def test_recipe_invokes_node_test_with_tap_reporter(self):
        recipe = _extract_recipe(_read_makefile(), "test-agent-adapters")
        invocation = _recipe_invocation_line(recipe)
        self.assertIn(
            "--test-reporter=tap",
            invocation,
            "test-agent-adapters must invoke `node --test "
            "--test-reporter=tap` so the output is stable across "
            f"Node 18/20/22/24/26. Invocation was: {invocation}",
        )


class TestParserAcceptsTapOutput(unittest.TestCase):
    """The parser MUST recognise the TAP `# pass N` line that the
    CI runner (Node 20.x on ubuntu-24.04) emits.
    """

    def setUp(self):
        recipe = _extract_recipe(_read_makefile(), "test-agent-adapters")
        self.parser = _parser_command(recipe)

    def test_tap_summary_with_passes_and_skips_parses(self):
        """The exact CI failure shape: 145 tests, 143 pass, 0 fail,
        2 skipped, exit 0. The parser must extract 143.
        """
        captured = textwrap.dedent("""\
            TAP version 13
            1..145
            # tests 145
            # pass 143
            # fail 0
            # skipped 2
        """)
        self.assertEqual(_run_parser_on(self.parser, captured), "143")

    def test_tap_summary_zero_passes_parses(self):
        """An all-fail TAP run still emits `# pass 0`. The parser
        must surface that, not refuse to parse.
        """
        captured = textwrap.dedent("""\
            TAP version 13
            1..3
            # tests 3
            # pass 0
            # fail 3
        """)
        self.assertEqual(_run_parser_on(self.parser, captured), "0")

    def test_spec_summary_is_not_the_wire_contract(self):
        """The recipe always invokes `node --test
        --test-reporter=tap`, so the spec form (`ℹ pass N`) is not
        the wire contract. A defensive `^# pass` parser that does
        NOT match the spec form is the correct design — anything
        else couples the parser back to a version-dependent Node
        default. This test pins the design decision: the parser
        must REFUSE the spec form, so a future refactor that
        "support both formats" gets caught.

        The `test_recipe_invokes_node_test_with_tap_reporter`
        test pins the complementary contract (the invocation
        always uses `--test-reporter=tap`); together they
        eliminate the version-dependence at both ends.
        """
        captured = textwrap.dedent("""\
            ✔ first test
            ℹ tests 3
            ℹ pass 3
            ℹ fail 0
        """)
        # The parser must NOT extract anything from the spec
        # form. An empty match is the correct answer; a non-empty
        # match would mean the parser accepted a non-TAP line
        # and could therefore be tricked by a future node
        # version that changes the spec form again.
        self.assertEqual(
            _run_parser_on(self.parser, captured), "",
            "TAP-pinned parser must refuse the spec `ℹ pass` "
            "form. The recipe forces `--test-reporter=tap` on "
            "every invocation, so the spec form is not the wire "
            "contract; accepting it would couple the parser back "
            "to a version-dependent default.",
        )

    def test_ansi_wrapped_tap_summary_parses(self):
        """Some Node versions wrap the TAP summary in SGR color
        codes. The parser strips ANSI escapes before matching, so
        the wrapped form must still parse to the right count.
        """
        captured = (
            "\x1b[32m# pass 117\x1b[39m\n"
            "\x1b[32m# fail 0\x1b[39m\n"
        )
        self.assertEqual(_run_parser_on(self.parser, captured), "117")

    def test_carriage_return_in_summary_still_parses(self):
        """TAP output occasionally contains a stray CR (e.g. when
        redirected through a PTY). The parser must handle CRs
        without losing the count.
        """
        captured = "# pass 42\r\n# fail 0\r\n"
        self.assertEqual(_run_parser_on(self.parser, captured), "42")


class TestParserRefusesMalformedOutput(unittest.TestCase):
    """Fail-closed behaviour: an empty summary, a missing line, a
    malformed line, or a process that exited non-zero MUST NOT
    produce a successful parse.
    """

    def setUp(self):
        recipe = _extract_recipe(_read_makefile(), "test-agent-adapters")
        self.parser = _parser_command(recipe)

    def test_empty_output_parses_to_empty(self):
        captured = ""
        self.assertEqual(_run_parser_on(self.parser, captured), "")

    def test_only_individual_test_lines_parses_to_empty(self):
        """Many `ok N - test name` lines but no TAP/spec summary
        block. The parser must NOT pick a test count out of the
        middle of the test list.
        """
        captured = textwrap.dedent("""\
            ok 1 - first test
            ok 2 - second test
            ok 3 - third test
        """)
        self.assertEqual(_run_parser_on(self.parser, captured), "")

    def test_malformed_pass_line_parses_to_empty(self):
        """`# passes 5` (English plural) is not the wire contract.
        The parser must reject it.
        """
        captured = "# passes 5\n# fail 0\n"
        self.assertEqual(_run_parser_on(self.parser, captured), "")

    def test_tap_failure_block_parses_zero(self):
        """A failing run still has the TAP summary; the count is
        what it is, the gate-floor check (not the parser) decides
        whether the run is acceptable. The parser must report 0
        pass for an all-fail summary so the floor trips.
        """
        captured = "# tests 5\n# pass 0\n# fail 5\n"
        self.assertEqual(_run_parser_on(self.parser, captured), "0")


class TestPreFixRecipeIsRejected(unittest.TestCase):
    """Negative control. The pre-fix recipe only matched `ℹ pass`
    and silently parsed an empty match against the CI TAP output.
    Pin that exact failure shape as a separate test so a future
    edit that re-introduces a single-format parser is caught.

    We construct the broken pipeline inline (matching the pre-fix
    recipe at commit 7ffbf46) and confirm it parses empty for
    the CI TAP output that the live CI run produced. Then we
    confirm the *current* recipe (whatever the user landed) does
    NOT parse empty for the same input — that is the proof that
    the fix is in place and the regression cannot recur silently.
    """

    def setUp(self):
        # Pre-fix pipeline, exactly as it stood in commit 7ffbf46
        # before this fix. Reproduce here so the test is a true
        # negative control rather than a hand-waved description.
        self.prefix_pipeline = (
            "n=$(printf '%s\\n' \"$out\" "
            "| sed -E \"s/${ESC}\\[[0-9;]*m//g\" "
            "| sed -nE 's/^ℹ pass ([0-9][0-9]*).*/\\1/p' | tail -1)"
        )
        self.tap_capture = textwrap.dedent("""\
            # tests 145
            # pass 143
            # fail 0
            # skipped 2
        """)

    def test_prefix_pipeline_rejects_valid_tap_summary(self):
        """The pre-fix `^ℹ pass` regex is anchored to a literal
        bullet (`ℹ`) and must NOT match the TAP `# pass` line.
        It parses empty against the valid CI TAP output — which
        is the exact failure the brief pinned.
        """
        # `cat` heredoc with a backslash to keep $out safe.
        script = f"""
ESC=$(printf '\\033')
{self.prefix_pipeline.replace("printf '%s\\n' \"$out\"", "cat <<'__X__'")}
__X__
"""
        cp = subprocess.run(
            ["bash", "-c", script],
            input=self.tap_capture,
            capture_output=True,
            text=True,
            check=False,
            timeout=30,
        )
        self.assertEqual(
            cp.stdout.strip(), "",
            "Pre-fix `^ℹ pass` pipeline must parse empty against the "
            "CI TAP output; that empty result is the exact failure "
            "the brief pinned. If this assertion ever fails, the "
            "broken pipeline stopped being broken — likely because "
            "Node started emitting the bullet form by default on "
            "every supported version, which would also invalidate "
            "this test's premise. Update the test, do not delete it.",
        )

    def test_current_recipe_does_not_regress_to_prefix(self):
        """Whatever the current recipe's parser is, it must NOT
        be the pre-fix `^ℹ pass`-only pipeline against the CI
        TAP output.
        """
        recipe = _extract_recipe(_read_makefile(), "test-agent-adapters")
        parser = _parser_command(recipe)
        result = _run_parser_on(parser, self.tap_capture)
        self.assertEqual(
            result, "143",
            "Current test-agent-adapters parser must extract 143 "
            "from the CI TAP output. If it returns empty, the "
            f"pre-fix `^ℹ pass`-only pipeline is back. Parser: {parser}",
        )


class TestFailClosedBehaviourIsPreserved(unittest.TestCase):
    """The recipe's fail-closed branches: non-zero node exit, empty
    summary, below-floor count. These are documented in the recipe
    comments and the brief required they stay.
    """

    def setUp(self):
        self.recipe = _extract_recipe(_read_makefile(), "test-agent-adapters")

    def test_recipe_fails_closed_on_empty_summary(self):
        """An empty `n` (no `# pass` line) MUST trigger a FATAL
        `reported no passing-test count` and exit 1. This is the
        contract that prevents `node --test exited 0 without a
        pass line` from being treated as a pass.
        """
        self.assertIn(
            "reported no passing-test count",
            self.recipe,
            "test-agent-adapters must emit a FATAL 'reported no "
            "passing-test count' line when the parse pipeline "
            "yields an empty count.",
        )

    def test_recipe_fails_closed_on_below_floor_count(self):
        """A parse that yields 0 must trigger the floor check and
        exit 1. The contract: a count below the floor is a hard
        refusal, never a green gate.
        """
        self.assertIn(
            "JS passing tests, floor is",
            self.recipe,
            "test-agent-adapters must check the parsed JS total "
            "against the AGENT_ADAPTER_TEST_MIN_JS_TESTS floor "
            "and exit non-zero on a below-floor count.",
        )

    def test_recipe_fails_closed_on_nonzero_node_exit(self):
        """A `node --test` invocation that exits non-zero (e.g.
        a real test failure) must propagate the exit status into
        the `failed=1` accumulator and ultimately exit 1 from
        the umbrella.
        """
        self.assertIn(
            "if [ $$status -ne 0 ]; then failed=1; fi",
            self.recipe,
            "test-agent-adapters must accumulate non-zero node "
            "exits into a `failed` flag and exit non-zero at "
            "the end of the recipe.",
        )
        self.assertIn(
            "if [ $$failed -ne 0 ]; then",
            self.recipe,
            "test-agent-adapters must exit 1 when `failed` is set.",
        )


if __name__ == "__main__":
    unittest.main(verbosity=2)
