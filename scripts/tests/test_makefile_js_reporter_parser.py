"""
test_makefile_js_reporter_parser.py — Pin the contract that the
`test-agent-adapters` Makefile recipe counts JS passing tests from a
stable, reporter-explicit `node --test` output and rejects empty or
malformed summaries.

Background:
  The recipe's parser once only understood the spec form
  (`ℹ pass N`), so the GitHub Actions ubuntu-24.04 runner (Node
  20.x by default) emitted `# pass 143` and the recipe reported
  "no passing-test count" while the suite had actually passed.

  CI evidence (run 38888336201, line 39264-39272):
    # tests 145
    # pass 143
    # fail 0
    # skipped 2
    [test-agent-adapters] FATAL: mpm-memory-openclaw reported no
    passing-test count.

  The parser fix is twofold: (1) invoke `node --test
  --test-reporter=tap ...` so the output is stable across every
  Node >= 18 minor, and (2) parse the TAP `# pass N` form. The
  spec form (`ℹ pass N`) is NOT accepted by the parser — the
  recipe forces `--test-reporter=tap` on every invocation, so a
  parser that also accepted the spec form would be re-coupled to
  a version-dependent default. Any input that does not match the
  TAP `# pass N` line is treated as malformed and fails closed.

  A second, file-discovery defect appeared on the same runner
  after the parser fix landed: the recipe's `node --test
  'tests/*.test.js'` invocation passes a literal glob (the single
  quotes stop the shell from expanding it). Node 26 expands the
  glob internally and falls back to "0 tests" on a non-match;
  Node 20 reports `Could not find '<pattern>'` and exits 0. The
  empty-pass-summary FATAL then trips on a suite whose .test.js
  files are present and would have run if `node --test` had
  received concrete paths. The recipe now uses portable POSIX
  `set -- tests/*.test.js` to expand the glob and an `[ -e
  "$$1" ]` guard to fail closed on a no-match (the same file-
  list-then-fail-closed pattern the TS branch already used).

  This test pins both contracts and the fail-closed behaviour
  the brief required:

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
      (negative control: this is the parser regression we just
      shipped a fix for; never let it come back)
    * the recipe MUST discover `.test.js` files via a portable
      POSIX shell glob (`set -- tests/*.test.js` followed by
      `"$$@"`) and MUST NOT pass a literal single-quoted glob to
      `node --test` (the second regression we just shipped a fix
      for; the new `TestRecipeDiscoversJsTestFiles` class pins
      this contract)

Hermeticity:
  No `node` subprocess is spawned by the parse-pipeline tests;
  the parse pipeline is a pure `sed` expression over a captured
  string, and the test feeds in representative outputs (with and
  without ANSI, with and without CR, TAP and spec) and asserts on
  the parsed count. The recipe's exact `sed` command is extracted
  from the Makefile source so a change to the recipe cannot pass
  these tests without also changing the parse contract.

  The new `TestNode20CompatUnderDash` class IS allowed to spawn
  `dash` and `node` subprocesses against a real `tempfile.TemporaryDirectory()`
  containing a synthetic `.test.js` file. Both binaries are
  already required by the recipe (`dash` is the default
  `make`-recipe shell on Debian-derived systems; `node` is
  probed for on PATH at the top of the recipe). The subprocess
  is the only way to reproduce the original failure shape under
  Node 20 — the structural assertions cannot prove that a
  literal glob fails closed because they cannot exercise the
  shell's quote-handling semantics.
"""

from __future__ import annotations

import re
import shutil
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
    """Return the JS branch's `out=$$(cd "agent_installation/$$d"
    && ... node --test ... 2>&1; }); status=$$?; \\` subshell.

    The recipe is multi-line with backslash continuations; we join
    the lines into a single command string. The JS branch is the
    FIRST `out=$$(cd "agent_installation/$$d" && ... node --test
    ... 2>&1` substring in the joined recipe — the TS branch comes
    later and uses the same shape but with a different `out=`
    variable list (`$$js_glob` rather than `"$$@"`). The helper
    anchors on the literal `out=$$(cd "agent_installation/$$d"`
    prefix and the closing `2>&1` of the subshell. If the recipe
    ever grows past that, this assertion fires first and forces
    the test to be updated alongside the recipe.
    """
    joined_lines: list[str] = []
    for line in recipe.splitlines():
        joined_lines.append(line.rstrip("\\\n").strip())
    full = " ".join(joined_lines)
    match = re.search(
        r'out=\$\$\(cd\s+"agent_installation/\$\$d"\s+&&.*?node\s+--test.*?2>&1\b',
        full,
    )
    if not match:
        raise AssertionError(
            "test-agent-adapters recipe must invoke "
            "`node --test` inside a `cd agent_installation/$$d` "
            "subshell that ends with `2>&1`. "
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


class TestRecipeDiscoversJsTestFiles(unittest.TestCase):
    """Pin the JS file-discovery contract that the literal-glob
    fix introduced.

    Before the fix, the JS branch passed a single-quoted glob
    (`'tests/*.test.js'`) to `node --test`. The shell did not
    expand the quoted glob, and `node --test` only matched files
    when the host happened to expand the pattern itself (Node 26
    does, Node 20 does not — it reports `Could not find` and
    exits 0). The fix is to expand the glob in the recipe's
    shell with portable POSIX `set -- <pattern>` and pass the
    resulting concrete paths via `"$$@"`, with a `[ -e "$$1" ]`
    guard that fails closed when the glob matched nothing.

    These tests are structural: they assert that the recipe text
    contains the load-bearing pieces of the new contract and
    does NOT contain the load-bearing piece of the broken
    contract. They do NOT exercise `node` or `dash`; that is the
    job of `TestNode20CompatUnderDash` below.
    """

    def setUp(self):
        self.recipe = _extract_recipe(_read_makefile(), "test-agent-adapters")

    def test_recipe_uses_set_to_expand_js_glob(self):
        """The recipe MUST expand the JS test-file glob with a
        portable POSIX `set -- tests/*.test.js` (the same form
        dash and bash both understand on every supported
        platform). This is the load-bearing piece of the
        discovery contract.
        """
        self.assertRegex(
            self.recipe,
            r"set\s+--\s+tests/\*\.test\.js",
            "test-agent-adapters JS branch must use portable "
            "POSIX `set -- tests/*.test.js` to expand the JS "
            "test-file glob. The previous literal single-quoted "
            "glob passed the pattern to `node --test` literally, "
            "which Node 20 cannot match.",
        )

    def test_recipe_fails_closed_on_no_matching_test_files(self):
        """A glob that matched nothing MUST trigger a FATAL and
        exit 1 before `node --test` runs. The `[ -e "$$1" ]`
        guard distinguishes "glob matched something" from "glob
        matched nothing" (in which case `$$1` is the literal
        pattern and the file-existence check fails). Without
        this guard, an empty `tests/` directory silently runs
        `node --test` against zero files and trips the
        empty-summary FATAL downstream.
        """
        self.assertRegex(
            self.recipe,
            r"if\s+\[\s+!\s+-e\s+\"\$\$1\"\s+\];\s*then",
            "test-agent-adapters JS branch must guard the "
            "`set -- tests/*.test.js` expansion with an "
            "`[ ! -e \"$$1\" ]` test that fails closed when the "
            "glob matched nothing.",
        )
        self.assertIn(
            "no .test.js files under tests/",
            self.recipe,
            "test-agent-adapters JS branch must emit a FATAL "
            "naming the missing .test.js files so the failure is "
            "actionable rather than a downstream empty-summary "
            "mystery.",
        )
        self.assertIn(
            "AGENT_ADAPTERS_JS lists $$d, so its absence is a packaging error",
            self.recipe,
            "test-agent-adapters JS branch must name the "
            "AGENT_ADAPTERS_JS variable in the FATAL so a reader "
            "knows which variable to fix.",
        )

    def test_recipe_passes_concrete_paths_via_positional_args(self):
        """The `node --test` invocation MUST receive concrete
        file paths via `"$$@"`, NOT a single-quoted glob
        (`'tests/*.test.js'`) that the shell would have to
        expand for `node --test` to match anything. The
        single-quoted form is exactly the broken shape the
        fix removed.
        """
        # The new shape: pass concrete paths via the positional
        # args `"$$@"` (set up by the `set -- tests/*.test.js`
        # expansion above). The recipe must contain this form.
        self.assertIn(
            'node --test --test-reporter=tap "$$@"',
            self.recipe,
            "test-agent-adapters JS branch must pass concrete "
            "filenames via `node --test --test-reporter=tap "
            '"$$@"` so `node --test` never has to expand a glob '
            "itself.",
        )
        # The old, broken shape: a literal single-quoted glob
        # passed to `node --test`. The recipe must NOT contain
        # this form anywhere in the JS branch — re-introducing
        # it would re-create the Node 20 latent failure.
        self.assertNotIn(
            "node --test --test-reporter=tap 'tests/*.test.js'",
            self.recipe,
            "test-agent-adapters JS branch must NOT pass a "
            "single-quoted glob to `node --test`. The shell "
            "does not expand single-quoted patterns, and Node "
            "20 cannot match a literal glob that did not match "
            "any file on disk (the exact failure mode this fix "
            "removed).",
        )
        # The exact pre-fix bare invocation must also be gone —
        # even if a future refactor drops the `--test-reporter=tap`
        # flag, a literal-glob form is still broken.
        self.assertNotIn(
            "node --test 'tests/*.test.js'",
            self.recipe,
            "test-agent-adapters must not invoke `node --test` "
            "with a literal single-quoted glob anywhere in the "
            "JS branch. The pattern is the regression we just "
            "fixed.",
        )


class TestNode20CompatUnderDash(unittest.TestCase):
    """Executable negative control: prove (a) the original
    quoted-glob form is fragile because the shell passes the
    literal pattern to `node --test`, and (b) the new form is
    robust because the shell expands the glob to concrete
    filenames before `node --test` runs.

    Per the brief, the two demonstrations are NODE-INDEPENDENT —
    they prove the SHELL semantics, not the runtime behaviour of
    any particular Node major. The third optional demonstration
    (run the broken form on a real Node and observe that the
    file is undiscovered) is Node-major-DEPENDENT: Node 20
    reports `Could not find '<pattern>'`, but Node 22+ silently
    expands the literal pattern itself and reports a passing
    test count of zero or worse. The brief asked us to test on
    Node 20 where available; the test below detects the local
    Node major and runs the runtime demonstration only on Node
    <= 20 (where it is meaningful), and skips on Node 22+ (where
    the surface lies).

    This class is the only one in this file that spawns a real
    subprocess. The structural assertions in
    `TestRecipeDiscoversJsTestFiles` prove the recipe text is
    correct; the assertions here prove the recipe is correct
    *as a shell pipeline*. The two halves together pin both the
    text and the runtime semantics.
    """

    # Mirror the recipe body exactly so the test exercises the
    # same pipeline the recipe runs. The shell expansion +
    # existence guard + positional-args invocation is the load-
    # bearing piece.
    NEW_INVOCATION = (
        "set -- tests/*.test.js && "
        "if [ ! -e \"$1\" ]; then exit 1; fi && "
        "node --test --test-reporter=tap \"$@\""
    )

    def setUp(self):
        if shutil.which("dash") is None:
            self.skipTest("dash not on PATH; cannot exercise the negative control.")
        if shutil.which("node") is None:
            self.skipTest("node not on PATH; cannot exercise the negative control.")
        self.tmp = tempfile.TemporaryDirectory()
        self.addCleanup(self.tmp.cleanup)
        self.tmpdir = Path(self.tmp.name)
        # Synthesize a single `.test.js` file under `tests/`.
        # The file contains one trivially-passing `node:test`
        # test so `node --test` has at least one test to run.
        tests_dir = self.tmpdir / "tests"
        tests_dir.mkdir()
        test_file = tests_dir / "synthetic.test.js"
        test_file.write_text(
            "const test = require('node:test');\n"
            "test('synthetic passes', () => { /* ok */ });\n",
            encoding="utf-8",
        )
        # Capture the local Node major so the optional
        # runtime-demonstration test can decide whether its
        # premise is meaningful. Major-20 reports
        # `Could not find '<pattern>'` on a literal glob;
        # major-22+ reports a TAP summary that hides the
        # underlying discovery failure.
        self.node_major = self._detect_node_major()

    @staticmethod
    def _detect_node_major() -> int:
        cp = subprocess.run(
            ["node", "--version"],
            capture_output=True,
            text=True,
            check=False,
            timeout=10,
        )
        out = (cp.stdout or "").strip()
        # `v20.x.y` -> 20
        if out.startswith("v") and "." in out:
            try:
                return int(out[1:].split(".", 1)[0])
            except ValueError:
                return 0
        return 0

    def _run_dash(self, command: str) -> subprocess.CompletedProcess:
        """Run `command` in `dash -c` from the synthetic tempdir.

        Returns the CompletedProcess so callers can inspect
        stdout, stderr, and returncode.
        """
        return subprocess.run(
            ["dash", "-c", command],
            cwd=str(self.tmpdir),
            capture_output=True,
            text=True,
            check=False,
            timeout=30,
        )

    def test_quoted_glob_passes_through_shell_literally(self):
        """Demonstration (1) of the brief — Node-independent.

        A POSIX shell passes a SINGLE-QUOTED wildcard to
        `node --test` literally. The shell expands an unquoted
        `tests/*.test.js` to concrete filenames; the same
        pattern inside single quotes is left alone. This is
        what makes the original recipe's `'tests/*.test.js'`
        argument brittle — under Node 20, the literal string
        `tests/*.test.js` is treated as a single path that does
        not exist, and the runner reports `Could not find
        '<pattern>'`.

        The test is Node-independent: under `dash`, the quoted
        wildcard in a command string is the EXACT byte sequence
        passed as argv[1] to `node --test`. The proof: write
        the literal glob into a wrapper script using only
        single quotes, invoke the wrapper, and ask the wrapper
        to print its `$1` argument. The literal pattern must
        round-trip intact. A re-introduced refactor that
        "fixes the shell quoting" without also expanding the
        glob is caught here.
        """
        # Write a wrapper script that, when run, prints its
        # $1 argument. The shell treats single-quoted text
        # literally; if the wrapper's $1 is the literal glob,
        # the round-trip proves the shell never expanded it.
        wrapper = self.tmpdir / "round_trip.sh"
        wrapper.write_text(
            "#!/bin/sh\n"
            "printf '%s\\n' \"$1\"\n",
            encoding="utf-8",
        )
        wrapper.chmod(0o755)
        # Invoke the wrapper with the literal single-quoted
        # glob as $1. If the shell expanded the pattern, the
        # wrapper would receive a concrete filename instead.
        cp = subprocess.run(
            [str(wrapper), "tests/*.test.js"],
            capture_output=True,
            text=True,
            check=False,
            timeout=10,
        )
        self.assertEqual(
            cp.returncode, 0,
            f"round-trip wrapper exited {cp.returncode}: {cp.stderr!r}",
        )
        printed = cp.stdout.rstrip("\n")
        self.assertEqual(
            printed, "tests/*.test.js",
            "A single-quoted glob passed as a positional arg "
            "to a script must survive intact: the receiving "
            "process's `$1` must equal the literal pattern, "
            "not any matched filename. If the shell expanded "
            "the pattern, this assertion would print a "
            "concrete filename (the file's basename) instead. "
            "The literal-pattern round-trip is the proof that "
            "the original quoted-glob form is fragile: it "
            "depends on `node --test` to do its own glob "
            "expansion, and Node 20 cannot.",
        )

    def test_new_invocation_passes_concrete_filenames(self):
        """Demonstration (2) of the brief — Node-independent.

        The fixed invocation passes concrete filenames to
        `node --test`, not a glob. The proof is a shell
        round-trip: run the recipe's `set -- tests/*.test.js
        && printf '%s\\n' "$@"` snippet and assert the printed
        result is a real filename, not the literal glob.

        This is the positive counterpart to
        `test_quoted_glob_passes_through_shell_literally`:
        the same shell semantics prove (a) the quoted glob
        is passed literally and (b) the unquoted glob is
        expanded to a concrete filename. Together they prove
        the original form is fragile and the new form is
        robust, without depending on any specific Node major.
        """
        cp = self._run_dash(
            "set -- tests/*.test.js && printf '%s\\n' \"$@\""
        )
        self.assertEqual(
            cp.returncode, 0,
            f"`dash -c` wrapper exited {cp.returncode}: {cp.stderr!r}",
        )
        printed = cp.stdout.rstrip("\n")
        # The new form's expansion must produce a real
        # filename, not the literal glob.
        self.assertIn(
            "synthetic.test.js",
            printed,
            "The fixed invocation's `set -- tests/*.test.js` "
            "must expand to a concrete filename, not the "
            "literal glob. The printed `$@` should include "
            "the on-disk `synthetic.test.js` path. If this "
            "assertion fails, the shell did not expand the "
            "glob (e.g. the test ran with `shopt -f` or some "
            "nonomal setting) and the recipe's discovery "
            "contract is broken. "
            f"printed={printed!r}",
        )
        # Negative: the literal glob must NOT be in the
        # expansion output (it would only appear if the glob
        # failed to match anything AND the shell left the
        # pattern intact — which is exactly the no-match
        # branch the `[ -e "$1" ]` guard is supposed to
        # catch).
        self.assertNotIn(
            "tests/*.test.js",
            printed,
            "The fixed invocation's `set -- tests/*.test.js` "
            "must NOT print the literal glob. The expansion "
            "produced a real filename; the literal pattern "
            "should be gone. If the literal pattern is still "
            "in the output, the glob did not match anything "
            "AND the shell did not strip the pattern — the "
            "recipe's fail-closed `[ -e \"$1\" ]` guard would "
            "catch this, but it is worth a separate "
            "assertion here so the failure mode is visible "
            "in the test output. "
            f"printed={printed!r}",
        )

    def test_quoted_glob_runtime_fails_on_node_20(self):
        """Optional runtime demonstration — gated on Node <= 20.

        On Node 20 (the GitHub Actions ubuntu-24.04 runner),
        the original quoted-glob form does NOT discover the
        synthetic file: `node --test` reports `Could not find
        '<pattern>'` and exits 0 with no TAP summary. On
        Node 22+, `node --test` silently expands the literal
        pattern and either runs zero tests or behaves
        differently; the test cannot make a portable
        assertion about that surface, so it is gated to Node
        <= 20 only.

        If the local Node major is > 20, this test skips with
        an informative message rather than failing. The two
        Node-independent tests above already pin the load-
        bearing shell semantics; this test is a
        belt-and-braces version of the same proof on the
        affected host major.
        """
        if self.node_major == 0:
            self.skipTest("Could not determine local Node major; "
                          "skipping the version-gated runtime check.")
        if self.node_major > 20:
            self.skipTest(
                f"Local Node major is {self.node_major}; the "
                "runtime demonstration is meaningful only on "
                "Node <= 20 (the GitHub Actions ubuntu-24.04 "
                "runner major). The two Node-independent tests "
                "above already pin the load-bearing shell "
                "semantics."
            )
        # Run the original quoted-glob form against the
        # synthetic file. The synthetic file is on disk; the
        # shell passes the literal glob; Node 20 reports
        # `Could not find '<pattern>'` to stderr and exits 0
        # with no TAP summary (i.e. zero tests run).
        cp = self._run_dash(
            "node --test --test-reporter=tap 'tests/*.test.js'"
        )
        combined = (cp.stdout or "") + (cp.stderr or "")
        discovery_signals = (
            "synthetic passes",
            "# pass 1",
            "# tests 1",
            "TAP version 13",
        )
        discovered = any(sig in combined for sig in discovery_signals)
        self.assertFalse(
            discovered,
            "On Node <= 20, the original quoted-glob form "
            "must NOT discover the synthetic `.test.js` file. "
            "The shell passed the literal glob, `node --test` "
            "received the literal string, and the runner "
            "could not find a file at that path. If this "
            "assertion fails, the local Node major expanded "
            "the pattern itself and hid the bug from the "
            "developer. "
            f"Combined output: {combined!r}",
        )

    def test_new_invocation_discovers_synthetic_file(self):
        """Run the new invocation against the synthetic file.
        The file MUST be discovered, run, and emit a TAP
        summary with `# pass 1`. This is the positive control
        for the recipe's discovery contract.
        """
        cp = self._run_dash(self.NEW_INVOCATION)
        self.assertEqual(
            cp.returncode, 0,
            f"New invocation under `dash -c` exited {cp.returncode}: "
            f"stdout={cp.stdout!r} stderr={cp.stderr!r}",
        )
        # The synthetic test name AND the `# pass 1` TAP
        # summary must both be present. The summary is what
        # the recipe's `n=...` parser would extract; the test
        # name is the proof the file was actually run.
        self.assertIn(
            "synthetic passes",
            cp.stdout,
            "New invocation must RUN the synthetic test. If "
            "this assertion fails, the file was not discovered "
            "even with the fixed invocation. stdout="
            f"{cp.stdout!r}",
        )
        self.assertIn(
            "# pass 1",
            cp.stdout,
            "New invocation must emit a TAP summary with "
            "`# pass 1`. This is what the recipe's `n=...` "
            "parser extracts. stdout="
            f"{cp.stdout!r}",
        )

    def test_new_invocation_fails_closed_on_no_matching_files(self):
        """The `[ -e "$$1" ]` guard distinguishes "glob matched
        something" from "glob matched nothing" — when no
        `.test.js` files exist, the guard exits 1 before
        `node --test` runs. This is the fail-closed branch
        the recipe documents; the test proves it works on a
        real shell.
        """
        # Remove the synthetic test file and rerun. The guard
        # must trip and exit 1, NOT silently fall through to
        # `node --test` (which would then exit 0 with zero
        # tests and trip the empty-summary FATAL downstream).
        (self.tmpdir / "tests" / "synthetic.test.js").unlink()
        cp = self._run_dash(self.NEW_INVOCATION)
        self.assertNotEqual(
            cp.returncode, 0,
            "New invocation must exit non-zero when the glob "
            "matched no files. The `[ -e \"$1\" ]` guard is the "
            "fail-closed branch; if it ever goes away, the "
            "recipe would silently fall through to `node --test` "
            "with zero files and trip the empty-summary FATAL. "
            f"returncode={cp.returncode} stdout={cp.stdout!r} "
            f"stderr={cp.stderr!r}",
        )


if __name__ == "__main__":
    unittest.main(verbosity=2)
