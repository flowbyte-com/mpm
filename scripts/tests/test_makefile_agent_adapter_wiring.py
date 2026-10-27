"""
test_makefile_agent_adapter_wiring.py — Regression coverage for the
gate wiring of the per-adapter suites under agent_installation/mpm-*/tests/.

Background:
  Before this wiring existed, the per-adapter suites (one Python suite
  per adapter under agent_installation/mpm-*/tests/, plus two Node
  suites) were reachable from NO local gate — not from `make test`, not
  from `make test-race`, not from `make release-gate`, and not from the
  CI workflow. The only Python suite the project treated as a merge gate
  (the renderer-parity suite under agent_installation/tests/) was
  reachable from CI; the per-adapter contract tests were invisible to
  every gate the developer runs locally.

Contract pinned by this test:
  1. `test-agent-adapters` exists as a Makefile target in the root
     Makefile and runs both the AGENT_ADAPTERS Python families and
     AGENT_ADAPTERS_JS Node families, with non-empty lists and
     non-zero floors. The TypeScript family (AGENT_ADAPTERS_TS) is
     also pinned here: its members must ship a `tsconfig.tests.json`,
     the gate must compile them via the local `node_modules/.bin/tsc`
     (not a globally installed compiler), and a missing tsconfig,
     missing tests directory, or empty pass summary is a hard refusal
     rather than a silent green. The sibling file
     `test_makefile_typescript_test_wiring.py` carries the executable
     negative controls (omitting mpm-opencode from AGENT_ADAPTERS_TS,
     `tsc --version` reading from the wrong path, etc.).
  2. The adapter lists are checked AGAINST THE ADAPTERS ON DISK, not
     against a hard-coded count. A hard-coded count silently rots the
     moment an adapter is added or retired; a set comparison either
     matches reality or fails loudly.
  3. The retired JavaScript adapter family is checked as a DECLARED
     empty state: the list is empty, the reason is recorded, the
     recipe says so out loud, and the recipe refuses to continue if a
     JavaScript suite ever appears without being wired in. It is
     deliberately NOT a "floor of 0" (a gate that can never fail) and
     NOT a silently deleted branch.
  4. `test-agent-installation`'s recipe in the root Makefile invokes
     `test-agent-adapters` via a `$(MAKE) test-agent-adapters` line
     (the standard Makefile recursive-make form). This is the
     foundational wiring: the per-adapter suites inherit every gate
     that already depends on `test-agent-installation`.
  5. `make test`, `make test-race`, and `make release-gate` all
     reach `test-agent-installation` via their dependency graph
     (verified by `make -n` and grep). This is what makes the
     promotion meaningful — if any of these gates stops depending on
     `test-agent-installation`, the per-adapter suites stop being
     exercised locally and the regression class this test exists
     to prevent re-appears.
  6. `test-agent-installation` returns a non-zero exit if either
     part of its umbrella fails (per-adapter suite or renderer-
     parity suite). The wiring must not silently mask the
     per-adapter exit status with the renderer-parity exit status,
     or vice-versa.

This is a structural test, not a snapshot test: it asserts the
specific surface that makes the wiring load-bearing, not the entire
Makefile. A snapshot test would couple to the file's overall
content; a structural test fails only when the load-bearing surface
changes.

Hermeticity:
  All assertions read the repository's own Makefile or run `make -n`
  against it. No subprocess is spawned that mutates host state. No
  test runs the real `test-agent-adapters` recipe (that would re-run
  the per-adapter suites on each invocation of the gate-wiring test).
  `make -n` is the dry-run form and emits no side effects.
"""

from __future__ import annotations

import re
import shutil
import subprocess
import unittest
from pathlib import Path

REPO_ROOT = Path(__file__).resolve().parent.parent.parent
MAKEFILE = REPO_ROOT / "Makefile"
AGENT_INSTALLATION_DIR = REPO_ROOT / "agent_installation"


def _read_makefile() -> str:
    return MAKEFILE.read_text(encoding="utf-8")


def _make_var(text: str, name: str) -> str | None:
    """Return the value of a `NAME := value` Make assignment.

    The match is ANCHORED to the assignment's own line. The previous
    implementation used `\\s*` after the `:=`, and `\\s` matches a
    NEWLINE — so an empty assignment (`AGENT_ADAPTERS_JS :=`) made the
    regex run on into the following comment line and capture THAT as
    the value. The list then looked non-empty, and the "the JS list is
    not empty" assertion passed against a comment. That was a measured
    false green; this parser cannot reproduce it.

    Returns None when the variable is not assigned at all.
    """
    match = re.search(
        rf"^{re.escape(name)}[ \t]*:=[ \t]*(?P<value>[^\n]*)$",
        text,
        re.MULTILINE,
    )
    if not match:
        return None
    return match.group("value").strip()


def _adapters_on_disk() -> set[str]:
    """Adapter directories that ship a Python-testable `tests/` tree.

    Derived from the filesystem, not from a hard-coded list, so the
    Makefile's adapter lists can be checked against reality.
    """
    if not AGENT_INSTALLATION_DIR.is_dir():
        return set()
    found = set()
    for child in AGENT_INSTALLATION_DIR.iterdir():
        if not child.is_dir() or not child.name.startswith("mpm-"):
            continue
        if (child / "tests").is_dir():
            found.add(child.name)
    return found


def _js_suite_adapters_on_disk() -> set[str]:
    """Adapter directories that ship a `tests/*.test.js` suite."""
    found = set()
    if not AGENT_INSTALLATION_DIR.is_dir():
        return found
    for child in AGENT_INSTALLATION_DIR.iterdir():
        if not child.is_dir() or not child.name.startswith("mpm-"):
            continue
        tests = child / "tests"
        if not tests.is_dir():
            continue
        if any(tests.glob("*.test.js")):
            found.add(child.name)
    return found


def _make_dry(target: str) -> str:
    """Run `make -n <target>` from the repo root and return stdout.

    `make -n` is the dry-run form: it prints the commands a real run
    would execute but never executes them. We rely on this to inspect
    the dependency graph without re-running the per-adapter suites.
    """
    make = shutil.which("make") or "/usr/bin/make"
    cp = subprocess.run(
        [make, "-n", target],
        cwd=str(REPO_ROOT),
        capture_output=True,
        text=True,
        check=False,
        timeout=60,
    )
    return cp.stdout


def _extract_recipe(makefile_text: str, target: str) -> str:
    """Return the recipe lines for `target`, i.e. every line that
    starts with a TAB (a Makefile recipe line) or ends with a single
    backslash (a continuation that began with a TAB on the previous
    line) between the target's colon line and the next
    non-recipe line. This is structural: it returns text, not a
    parsed AST, so a Makefile parser change cannot break the
    assertion.

    Continuation rules: a line whose last non-newline character is
    `\\` joins with the next line. Both lines are recipe lines, so
    both must be collected. Empty lines and non-TAB, non-bs lines
    end the recipe — except for the very first line, which is the
    empty remainder between the matched target's `:` and the
    trailing newline of that target-declaration line.
    """
    pattern = re.compile(
        rf"^{re.escape(target)}\s*:.*$",
        re.MULTILINE,
    )
    match = pattern.search(makefile_text)
    if not match:
        return ""
    after = makefile_text[match.end():]
    lines: list[str] = []
    first = True
    for raw_line in after.splitlines(keepends=False):
        if first:
            # The first line in `after` is whatever sits between
            # the matched target's `:` and the next newline —
            # usually empty. It is NOT a recipe line; drop it.
            first = False
            continue
        if raw_line.startswith("\t") or raw_line.endswith("\\"):
            lines.append(raw_line)
            continue
        # First non-empty, non-indented, non-continuation line ends
        # the recipe.
        break
    return "\n".join(lines)


class TestAdapterLists(unittest.TestCase):
    """The per-adapter targets must reference the adapter families that
    actually exist, and every family that exists must be wired in.
    """

    def test_make_var_parser_does_not_capture_the_next_comment_line(self):
        """The anchor fix must be a real fix.

        The previous parser used `\\s*` after the `:=`, and `\\s`
        matches a NEWLINE. An empty assignment therefore captured the
        FOLLOWING COMMENT LINE as its value — a measured false green
        that made an empty adapter list look non-empty. This test
        pins the corrected behaviour directly, on a synthetic
        Makefile, so the parser cannot silently regress.
        """
        synthetic = (
            "AGENT_ADAPTERS_JS :=\n"
            "# Adapters that additionally ship a `node --test` suite.\n"
        )
        self.assertEqual(
            _make_var(synthetic, "AGENT_ADAPTERS_JS"),
            "",
            "an empty Make assignment must parse as an EMPTY list, "
            "never as the comment line that follows it.",
        )

    def test_agent_adapters_python_list_matches_the_adapters_on_disk(self):
        """AGENT_ADAPTERS must list exactly the adapter directories
        that ship a `tests/` tree.

        Checked as a SET against the filesystem, not as a count: a
        count silently rots when an adapter is added or retired,
        while a set comparison either matches reality or fails loudly.
        """
        text = _read_makefile()
        value = _make_var(text, "AGENT_ADAPTERS")
        self.assertIsNotNone(
            value,
            "AGENT_ADAPTERS must be defined as a Make variable.",
        )
        listed = set(value.split())
        on_disk = _adapters_on_disk()
        self.assertTrue(
            on_disk,
            "the repository is expected to ship at least one adapter "
            "with a tests/ tree; found none.",
        )
        self.assertEqual(
            listed,
            on_disk,
            "AGENT_ADAPTERS must list exactly the adapters that ship a "
            f"tests/ tree. listed={sorted(listed)} on_disk={sorted(on_disk)}. "
            "An adapter that is not listed is SILENTLY SKIPPED by the "
            "gate; a listed adapter that does not exist is a packaging "
            "error.",
        )

    def test_agent_adapters_js_list_is_a_declared_empty_state(self):
        """The JavaScript adapter family was retired with its adapters.

        The empty state must be EXPLICIT, not accidental:
          - the list is empty;
          - the reason is recorded next to it;
          - the recipe says the fact out loud instead of passing in
            silence;
          - the recipe refuses to continue if a JavaScript suite
            appears without being wired in.

        It is deliberately NOT a "floor of 0" (a gate that can never
        fail) and NOT a silently deleted branch.
        """
        text = _read_makefile()
        value = _make_var(text, "AGENT_ADAPTERS_JS")
        self.assertIsNotNone(
            value,
            "AGENT_ADAPTERS_JS must still be DECLARED (empty is a "
            "decision; an absent variable is an accident).",
        )
        self.assertEqual(
            value.split(),
            [],
            "AGENT_ADAPTERS_JS is expected to be EMPTY — the "
            "JavaScript adapter family was retired with its adapters.",
        )
        reason = _make_var(text, "AGENT_ADAPTERS_JS_EMPTY_REASON")
        self.assertIsNotNone(reason, "the empty state must carry a reason.")
        self.assertTrue(
            reason.strip(),
            "AGENT_ADAPTERS_JS_EMPTY_REASON must not be blank.",
        )

    def test_agent_adapters_js_list_covers_every_js_suite_on_disk(self):
        """No adapter may ship a `tests/*.test.js` suite without being
        wired into AGENT_ADAPTERS_JS.

        This is the check that keeps the declared empty state honest:
        if a JavaScript adapter is added back without touching the
        list, its suite would never run and the gate would still be
        green.
        """
        js_on_disk = _js_suite_adapters_on_disk()
        listed = set(_make_var(_read_makefile(), "AGENT_ADAPTERS_JS").split())
        self.assertEqual(
            js_on_disk - listed,
            set(),
            "these adapters ship a tests/*.test.js suite but are not "
            "listed in AGENT_ADAPTERS_JS; their suite would never run.",
        )

    def test_agent_adapters_ts_list_includes_mpm_opencode(self):
        """The TypeScript family must list mpm-opencode (the only
        adapter today whose test suite is written in TypeScript). A
        recipe that compiles to disk but skips the gate because it
        did not list mpm-opencode under AGENT_ADAPTERS_TS is a
        silent green — the regression we are pinning.
        """
        text = _read_makefile()
        names = (_make_var(text, "AGENT_ADAPTERS_TS") or "").split()
        self.assertIn(
            "mpm-opencode",
            names,
            "AGENT_ADAPTERS_TS must list mpm-opencode so its "
            "TypeScript suite runs through the canonical "
            f"test-agent-adapters gate. Got: {names!r}",
        )

    def test_per_language_floors_present(self):
        """The Python and TypeScript floors must be real numbers.

        The JavaScript floor must be EMPTY, because there is no
        JavaScript family left to measure: a floor of 0 would be a
        gate that can never fail, and a floor of 100 against a
        population of 0 would be a gate that always fails. The recipe
        refuses to run a JavaScript family whose floor is unset, so
        the empty floor cannot drift into a silent pass.
        """
        text = _read_makefile()
        self.assertRegex(
            text,
            r"(?m)^AGENT_ADAPTER_TEST_MIN_PY_TESTS[ \t]*:=[ \t]*\d+$",
        )
        self.assertRegex(
            text,
            r"(?m)^AGENT_ADAPTER_TEST_MIN_TS_TESTS[ \t]*:=[ \t]*\d+$",
            "AGENT_ADAPTER_TEST_MIN_TS_TESTS must be defined so "
            "the TypeScript pass-count floor trips on a "
            "silently-zero suite.",
        )
        js_floor = _make_var(text, "AGENT_ADAPTER_TEST_MIN_JS_TESTS")
        self.assertIsNotNone(
            js_floor,
            "AGENT_ADAPTER_TEST_MIN_JS_TESTS must still be DECLARED "
            "(empty is a decision; an absent variable is an accident).",
        )
        self.assertEqual(
            js_floor,
            "",
            "AGENT_ADAPTER_TEST_MIN_JS_TESTS must be EMPTY while "
            "AGENT_ADAPTERS_JS is empty: there is no measured "
            "population to floor.",
        )


class TestAdapterTargetExists(unittest.TestCase):
    """`test-agent-adapters` must be a real target in the root Makefile
    so that `$(MAKE) test-agent-adapters` resolves.
    """

    def test_target_declared(self):
        text = _read_makefile()
        self.assertRegex(
            text,
            r"(?m)^test-agent-adapters\s*:",
            "Makefile must declare `test-agent-adapters:` target.",
        )

    def test_recipe_invokes_both_families(self):
        """The recipe must walk both AGENT_ADAPTERS (Python) and
        AGENT_ADAPTERS_JS (JS). A recipe that drops either family
        silently stops exercising that adapter layer — the exact
        failure mode this test exists to prevent.
        """
        recipe = _extract_recipe(_read_makefile(), "test-agent-adapters")
        self.assertIn("$(AGENT_ADAPTERS)", recipe,
                      "test-agent-adapters must iterate AGENT_ADAPTERS (Python).")
        self.assertIn("$(AGENT_ADAPTERS_JS)", recipe,
                      "test-agent-adapters must iterate AGENT_ADAPTERS_JS (JS).")

    def test_recipe_declares_the_empty_js_state_loudly(self):
        """The recipe must SAY that the JavaScript family is gone.

        A branch that simply disappears leaves a reader — and a future
        maintainer — unable to tell the difference between "no
        JavaScript adapter exists" and "the JavaScript gate was
        accidentally removed". The empty state must be printed, with
        the recorded reason.
        """
        recipe = _extract_recipe(_read_makefile(), "test-agent-adapters")
        self.assertIn(
            "no JavaScript adapter family remains",
            recipe,
            "test-agent-adapters must print the declared empty "
            "JavaScript state instead of passing in silence.",
        )
        self.assertIn(
            "AGENT_ADAPTERS_JS_EMPTY_REASON",
            recipe,
            "test-agent-adapters must print the recorded reason for "
            "the empty JavaScript state.",
        )

    def test_recipe_refuses_an_unwired_js_suite(self):
        """A JavaScript suite that is not wired into AGENT_ADAPTERS_JS
        must be a hard refusal.

        This is what keeps the declared empty state honest: a new
        JavaScript adapter added without touching the list would
        otherwise have its suite silently skipped.
        """
        recipe = _extract_recipe(_read_makefile(), "test-agent-adapters")
        self.assertIn(
            "JavaScript adapter suites exist but AGENT_ADAPTERS_JS is empty",
            recipe,
            "test-agent-adapters must FATAL when a JavaScript suite "
            "exists that AGENT_ADAPTERS_JS does not list.",
        )
        self.assertIn(
            "tests/*.test.js",
            recipe,
            "the unwired-suite guard must look for tests/*.test.js "
            "under the adapter directories.",
        )

    def test_recipe_refuses_a_js_family_without_a_floor(self):
        """A JavaScript family with NO pass-count floor is a silent
        gate: the suite could vanish and the recipe would still be
        green. The recipe must refuse that combination outright.
        """
        recipe = _extract_recipe(_read_makefile(), "test-agent-adapters")
        self.assertIn(
            "AGENT_ADAPTERS_JS is non-empty but AGENT_ADAPTER_TEST_MIN_JS_TESTS is unset",
            recipe,
            "test-agent-adapters must FATAL when a JavaScript family "
            "is wired in without a measured pass-count floor.",
        )

    def test_recipe_invokes_ts_family_with_local_tsc(self):
        """The recipe must walk AGENT_ADAPTERS_TS and compile each
        member via the LOCAL `node_modules/.bin/tsc`. Falling back
        to a globally installed compiler would couple the test output
        to whatever the host happens to ship and break the
        reproducible-build contract the canonical gate is meant to
        preserve.
        """
        recipe = _extract_recipe(_read_makefile(), "test-agent-adapters")
        self.assertIn("$(AGENT_ADAPTERS_TS)", recipe,
                      "test-agent-adapters must iterate AGENT_ADAPTERS_TS (TS).")
        self.assertIn(
            "tsconfig.tests.json",
            recipe,
            "test-agent-adapters must pin a per-adapter tsconfig.tests.json "
            "as the compile input — that is the deterministic input that "
            "every other adapter's compile paths already use.",
        )
        self.assertIn(
            "node_modules/.bin/tsc",
            recipe,
            "test-agent-adapters must invoke the LOCAL tsc binary at "
            "agent_installation/<d>/node_modules/.bin/tsc. The gate "
            "deliberately does not fall back to a globally installed "
            "tsc (which is unpinned here).",
        )

    def test_recipe_fails_on_missing_node(self):
        """A host without `node` on PATH must be a hard refusal, not
        a silent skip. The script became unreachable in the first
        place because silent-skipping hid the JS family from the
        gate; this assertion prevents that regression.
        """
        recipe = _extract_recipe(_read_makefile(), "test-agent-adapters")
        self.assertRegex(
            recipe,
            r"command -v node",
            "test-agent-adapters must probe for `node` on PATH and fail closed.",
        )

    def test_recipe_fails_on_missing_python3(self):
        recipe = _extract_recipe(_read_makefile(), "test-agent-adapters")
        self.assertRegex(
            recipe,
            r"command -v python3",
            "test-agent-adapters must probe for `python3` on PATH and fail closed.",
        )

    def test_recipe_fails_closed_on_ts_compile_problems(self):
        """The TS branch must fail closed on the four realistic
        failure shapes: missing tests directory, missing
        `tsconfig.tests.json`, missing local tsc, and an empty
        pass summary after a successful compile. None of them is a
        silent zero — every one of them is an explicit FATAL.
        """
        recipe = _extract_recipe(_read_makefile(), "test-agent-adapters")
        # Missing tsconfig.tests.json — listed, but the adapter forgot
        # to ship the per-adapter input.
        self.assertIn(
            "tsconfig.tests.json is missing",
            recipe,
            "test-agent-adapters must FATAL on a missing "
            "tsconfig.tests.json so an entry drift surfaces "
            "as a green-gate regression rather than a silent skip.",
        )
        # Missing local tsc — refrained from falling back to a
        # globally installed compiler.
        self.assertIn(
            "no local TypeScript compiler",
            recipe,
            "test-agent-adapters must FATAL on a missing local "
            "tsc binary and refuse to fall back to a globally "
            "installed (unpinned) tsc.",
        )
        # Empty pass summary — `node --test` exited 0 without a
        # `# pass` line. The JS branch already pins this;
        # the TS branch must too.
        self.assertRegex(
            recipe,
            r"TS:.*passing tests",
            "test-agent-adapters must print a TS: <count> "
            "passing-tests line so the gate is auditable.",
        )

    def test_session_start_hook_excluded_from_comment(self):
        """The live `mpm-claude-code/tests/session_start_hook.test.sh`
        check must remain outside the hermetic gate. It depends on
        this machine's actual install (MPM_WORKSPACE=$HOME/.mpm and
        $HOME/.local/bin/mpm), so it cannot be part of an automated
        local gate.
        """
        text = _read_makefile()
        self.assertIn(
            "session_start_hook.test.sh",
            text,
            "Makefile must mention session_start_hook.test.sh so its "
            "exclusion is visible to a future reader.",
        )
        # The reference must sit inside a `#` comment block, not as
        # a recipe-line token that would make the suite executable
        # via the Makefile. Walk back from the reference: the
        # closest preceding non-blank line must be a `#` comment,
        # otherwise the suite has been promoted from "documented
        # exclusion" to "executed surface" without anyone noticing.
        idx = text.find("session_start_hook.test.sh")
        all_lines = text.splitlines()
        ref_line_idx = text[:idx].count("\n")
        # Walk backwards from ref_line_idx - 1 until a non-blank
        # line is found.
        for back in range(ref_line_idx - 1, -1, -1):
            line = all_lines[back]
            if not line.strip():
                continue
            self.assertTrue(
                line.lstrip().startswith("#"),
                "session_start_hook.test.sh reference must sit inside a "
                "`#` comment block. The closest preceding non-blank "
                f"line ({back + 1}: {line!r}) is not a comment, which "
                "means the exclusion has been promoted from prose to "
                "an executable recipe surface — exactly the silent "
                "regression this assertion exists to prevent.",
            )
            break


class TestUmbrellaWiring(unittest.TestCase):
    """`test-agent-installation` is the umbrella: it must chain into
    `test-agent-adapters` so that every existing gate that already
    depends on `test-agent-installation` picks up the per-adapter
    suites for free.
    """

    def test_umbrella_invokes_test_agent_adapters(self):
        """The structural surface: `test-agent-installation`'s recipe
        must contain a `$(MAKE) test-agent-adapters` line. The recipe
        must invoke the per-adapter suites via `$(MAKE)`, not by
        re-implementing the loop inline, so the chain stays through
        the canonical target.
        """
        recipe = _extract_recipe(_read_makefile(), "test-agent-installation")
        self.assertRegex(
            recipe,
            r"\$\(MAKE\)\s+test-agent-adapters",
            "test-agent-installation must invoke "
            "`$(MAKE) test-agent-adapters` so the per-adapter "
            "suites inherit every gate that already depends on "
            "test-agent-installation.",
        )

    def test_umbrella_runs_before_cd(self):
        """The recursive `$(MAKE)` must run BEFORE the `cd
        agent_installation &&` line. Each line in a Makefile recipe
        runs in its own subshell, but a `cd` chained to a single
        multi-line recipe via `\\` continuations DOES persist into
        subsequent lines of the same recipe. If it runs after the
        `cd`, the recursive make looks for `Makefile` inside
        `agent_installation/` and fails with `No rule to make
        target 'test-agent-adapters'`.
        """
        recipe = _extract_recipe(_read_makefile(), "test-agent-installation")
        cd_idx = recipe.find("cd agent_installation")
        make_idx = recipe.find("$(MAKE) test-agent-adapters")
        self.assertNotEqual(cd_idx, -1,
                            "test-agent-installation must cd into "
                            "agent_installation/ for the renderer "
                            "parity suite.")
        self.assertNotEqual(make_idx, -1,
                            "test-agent-installation must invoke "
                            "$(MAKE) test-agent-adapters.")
        self.assertLess(
            make_idx, cd_idx,
            "test-agent-adapters must be invoked BEFORE the `cd "
            "agent_installation` line, otherwise the recursive make "
            "fails with `No rule to make target 'test-agent-adapters'`.",
        )

    def test_umbrella_propagates_either_failure(self):
        """The umbrella must surface a non-zero exit if EITHER
        component failed. A regression that swallowed the per-
        adapter suite's exit status (or the renderer-parity exit
        status) would silently let one half of the umbrella decay
        while the other half stayed green.
        """
        recipe = _extract_recipe(_read_makefile(), "test-agent-installation")
        # The per-adapter suite's exit must be captured into a
        # variable and propagated. Capture+propagate looks like
        # `ad_status=$?` + `exit $ad_status` at the end of the
        # recipe. In the Makefile source the variable name appears
        # as `$$ad_status` (Makefile escapes `$` to `$$` for the
        # shell), but `_extract_recipe` returns the raw text, so
        # the pattern below matches `$$ad_status` literally.
        self.assertRegex(
            recipe,
            r"ad_status=",
            "test-agent-installation must capture test-agent-adapters' "
            "exit status into a shell variable before the renderer "
            "parity run can overwrite $?.",
        )
        self.assertRegex(
            recipe,
            r"exit \$\$ad_status",
            "test-agent-installation must exit with the captured "
            "ad_status at the end so a per-adapter failure surfaces "
            "even when the renderer-parity suite passed.",
        )


class TestGateReachability(unittest.TestCase):
    """Every higher-level gate (`make test`, `make test-race`,
    `make release-gate`) must reach `test-agent-installation` via
    its dependency graph. This is what makes the promotion
    meaningful — if any of these gates stops depending on
    `test-agent-installation`, the per-adapter suites stop being
    exercised by the developer-facing gates.
    """

    def _gate_reaches(self, gate: str, leaf: str) -> bool:
        """Return True if `make -n <gate>` shows `make <leaf>` being
        invoked (directly or transitively) somewhere in the trace.
        """
        trace = _make_dry(gate)
        # The dry-run output contains lines like:
        #   /usr/bin/make test-agent-installation
        # (the recipe of the calling target) and indented
        #   make test-agent-adapters; \
        # (a sub-make invocation). `make -n` emits the resolved
        # absolute path for the recursive make (because that's how
        # it was found via `command -v make`), so the regex must
        # accept either a bare `make` or a `/path/to/make` form.
        # We only care that the gate eventually invokes
        # `make <leaf>`; the trace shows every sub-make that the
        # real run would execute.
        pattern = re.compile(
            rf"(?m)^(?:\s*)(?:\S*/)?make\s+{re.escape(leaf)}\b"
        )
        return bool(pattern.search(trace))

    def test_make_test_reaches_test_agent_installation(self):
        self.assertTrue(
            self._gate_reaches("test", "test-agent-installation"),
            "`make test` must reach test-agent-installation via "
            "its dependency graph; otherwise the per-adapter "
            "suites stop being exercised by the local dev gate.",
        )

    def test_make_test_reaches_test_agent_adapters(self):
        self.assertTrue(
            self._gate_reaches("test", "test-agent-adapters"),
            "`make test` must reach test-agent-adapters (via "
            "test-agent-installation) so the per-adapter suites "
            "are exercised by the local dev gate.",
        )

    def test_make_test_race_reaches_test_agent_installation(self):
        self.assertTrue(
            self._gate_reaches("test-race", "test-agent-installation"),
            "`make test-race` must reach test-agent-installation.",
        )

    def test_make_test_race_reaches_test_agent_adapters(self):
        self.assertTrue(
            self._gate_reaches("test-race", "test-agent-adapters"),
            "`make test-race` must reach test-agent-adapters.",
        )

    def test_make_release_gate_reaches_test_agent_installation(self):
        self.assertTrue(
            self._gate_reaches("release-gate", "test-agent-installation"),
            "`make release-gate` must reach test-agent-installation.",
        )

    def test_make_release_gate_reaches_test_agent_adapters(self):
        self.assertTrue(
            self._gate_reaches("release-gate", "test-agent-adapters"),
            "`make release-gate` must reach test-agent-adapters.",
        )

    def test_umbrella_does_not_double_invoke_adapter_target(self):
        """`test-agent-installation`'s recipe must invoke
        `$(MAKE) test-agent-adapters` exactly once. If a future
        edit accidentally adds a second invocation, the per-adapter
        suites run twice (≈50s duplicated) and the gate stays
        green — but slower. The exact-count assertion catches this
        before it lands.
        """
        recipe = _extract_recipe(_read_makefile(), "test-agent-installation")
        count = len(re.findall(r"\$\(MAKE\)\s+test-agent-adapters", recipe))
        self.assertEqual(
            count, 1,
            f"`$(MAKE) test-agent-adapters` must appear exactly once "
            f"in test-agent-installation; found {count}.",
        )


if __name__ == "__main__":
    unittest.main()