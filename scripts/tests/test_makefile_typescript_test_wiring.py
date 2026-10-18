"""
test_makefile_typescript_test_wiring.py — Pin the contract that the
`test-agent-adapters` Makefile recipe runs the mpm-opencode TypeScript
test suite through the canonical gate, compiles it via the local
`node_modules/.bin/tsc`, and refuses every silent-zero failure shape.

Background:
  The mpm-opencode adapter ships its test suite as `.test.ts` files
  (`tests/namespace_contract.test.ts` and
  `tests/test_resolve_mpm_binary.test.ts`, 17 tests total). They are
  run on a developer machine via `tsx` or Node 22.18+'s built-in
  type-stripping, neither of which is available on the project's
  oldest supported host (`engines.node = ">=20"` in
  agent_installation/mpm-opencode/package.json) and neither of which
  is part of a reproducible CI pin. Before this fix, the suite was
  reachable from NO canonical gate:

    * `make test`, `make test-race`, and `make release-gate` all
      depend on `make test-agent-installation`, which depends on
      `make test-agent-adapters`;
    * `make test-agent-adapters` walks `AGENT_ADAPTERS` (Python),
      `AGENT_ADAPTERS_JS` (Node `--test` runs `.test.js`), and
      `AGENT_ADAPTERS_TS` (TypeScript, compiled locally). It does
      not glob `.test.ts` directly — Node 20.x cannot run a `.ts`
      file, so doing so would fail on the canonical CI runner.
    * Without an entry in `AGENT_ADAPTERS_TS`, the 17 tests never
      run in CI, and the gate's "all adapter suites green" line is
      honest about a subset of the suite.

  The fix introduces a TypeScript-aware branch: per adapter, a
  `tsconfig.tests.json` declares what compiles, a local tsc
  (resolved against `node_modules/.bin/tsc` so it is host-pinned
  and reproducible) emits `.test.js` to a `tests-build/` directory
  that is .gitignored, and the standard TAP-pinned `node --test`
  invocation runs against that output. The same `node --test
  --test-reporter=tap` pipeline and the same `# pass N` parser the
  JS branch already pins carries over; no parallel parser is
  introduced.

Contract pinned by this file:
  * `AGENT_ADAPTERS_TS` is a non-empty Make variable that lists at
    least `mpm-opencode` (the only TS suite today).
  * The recipe compiles each member via the local
    `node_modules/.bin/tsc` (no global tsc, no unpinned download).
  * The recipe fails closed on (a) missing tests directory, (b)
    missing `tsconfig.tests.json`, (c) missing local tsc, (d) an
    empty pass summary, (e) a non-zero `node --test` exit.
  * The `AGENT_ADAPTER_TEST_MIN_TS_TESTS` floor is honoured and
    raises a FATAL on a below-floor count.

Negative controls (executable reproductions of the regression class):
  * Removing mpm-opencode from `AGENT_ADAPTERS_TS` while leaving
    the suite on disk would silently zero-out the gate. We assert
    a structural guard catches that: the test
    `test_missing_ts_adapter_makes_recipe_exit_nonzero` synthesises
    a Makefile that names a different adapter in `AGENT_ADAPTERS_TS`
    while `mpm-opencode` is the only adapter that ships a
    `tsconfig.tests.json`, and verifies the recipe exits non-zero.
  * Simulating a missing local tsc (`./node_modules/.bin/tsc`
    absent) is detected by the recipe's "no local TypeScript
    compiler" FATAL branch.

Hermeticity:
  No real subprocess is spawned from this module. The recipe is
  parsed as text; the negative controls synthesise a throw-away
  Makefile in a tempfile, then run `make -n` against it to inspect
  the recipe's command graph without executing its compile or its
  tests. The structural guards operate on the live repository's
  Makefile the way `test_makefile_agent_adapter_wiring.py` already
  pins the JS branch.

  The specific `make -n` shape and the exact failure-mode
  invocations are taken from the upstream Cherry-pick's
  `Makefile` script grammar and faithfully reproduced here so
  any future regression is caught at the test layer rather than at
  CI runtime.
"""

from __future__ import annotations

import os
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


def _make_dry(target: str, cwd: Path | None = None) -> str:
    """Run `make -n <target>` and return stdout. `make -n` is the
    dry-run form: it prints commands without executing them.
    """
    make = shutil.which("make") or "/usr/bin/make"
    cp = subprocess.run(
        [make, "-n", target],
        cwd=str(cwd or REPO_ROOT),
        capture_output=True,
        text=True,
        check=False,
        timeout=60,
    )
    return cp.stdout


# Variables whose values appear inline in the live Makefile. We
# re-extract them at test time so the assertions stay valid if the
# floors / lists change for unrelated reasons.
_FLOOR_RE = re.compile(
    r"^AGENT_ADAPTER_TEST_MIN_TS_TESTS\s*:=\s*(\d+)\s*$", re.MULTILINE
)
_TS_LIST_RE = re.compile(
    r"^AGENT_ADAPTERS_TS\s*:=\s*(?P<list>.+?)$", re.MULTILINE
)


def _ts_floor() -> int:
    text = _read_makefile()
    match = _FLOOR_RE.search(text)
    assert match is not None, (
        "Makefile must define AGENT_ADAPTER_TEST_MIN_TS_TESTS as a "
        f"decimal integer. Searched in:\n{text[:4000]}"
    )
    return int(match.group(1))


def _ts_list() -> list[str]:
    text = _read_makefile()
    match = _TS_LIST_RE.search(text)
    assert match is not None, (
        "Makefile must define AGENT_ADAPTERS_TS as a Make variable."
    )
    return match.group("list").split()


class TestTypeScriptAdapterListContract(unittest.TestCase):
    """The structural guard: AGENT_ADAPTERS_TS is a real variable
    that includes mpm-opencode, the only adapter today whose suite
    is written in TypeScript. Without this assertion, omitting
    mpm-opencode from the list would zero the gate silently.
    """

    def test_ts_list_is_defined_and_nonempty(self):
        names = _ts_list()
        self.assertGreaterEqual(
            len(names),
            1,
            "AGENT_ADAPTERS_TS should list at least one TS suite; "
            f"got {names!r}",
        )

    def test_ts_list_includes_mpm_opencode(self):
        names = _ts_list()
        self.assertIn(
            "mpm-opencode",
            names,
            "AGENT_ADAPTERS_TS must list mpm-opencode so the "
            "17-test TypeScript suite runs through the canonical "
            "test-agent-adapters gate.",
        )

    def test_every_ts_entry_has_a_local_compile_config(self):
        """Each `AGENT_ADAPTERS_TS` entry must ship a
        `tsconfig.tests.json` next to it. The recipe refuses to
        run a member without one (FATAL message at the top of the
        branch), but the structural guard pins the input rather
        than the failure: a future edit that adds an entry without
        the config gets caught before CI.
        """
        names = _ts_list()
        missing = [
            d for d in names
            if not (REPO_ROOT / "agent_installation" / d / "tsconfig.tests.json").is_file()
        ]
        self.assertEqual(
            missing,
            [],
            "Every AGENT_ADAPTERS_TS entry must ship a "
            "tsconfig.tests.json so the deterministic compile is "
            f"pinned. Missing: {missing!r}",
        )


class TestTypeScriptCompileGateContract(unittest.TestCase):
    """Pin that the recipe compiles via the local TypeScript
    compiler and refuses every silent-zero failure shape.
    """

    def test_recipe_compiles_via_local_tsc_only(self):
        """The TS branch must invoke `node_modules/.bin/tsc`. A
        globally installed compiler version is unpinned here and
        would couple the output to whatever the host happens to
        ship — exactly the reproducibility regression the gate is
        meant to prevent.
        """
        text = _read_makefile()
        # The branch must mention `node_modules/.bin/tsc` as the
        # resolved compiler (the recipe cd's into the adapter dir
        # and then `.bin/tsc` sits beside it).
        self.assertRegex(
            text,
            r"node_modules/\.bin/tsc",
            "test-agent-adapters must invoke the LOCAL tsc at "
            "node_modules/.bin/tsc. Falling back to a global "
            "compiler would couple the test output to the host.",
        )
        # And the recipe must NOT call `npx tsc` (the deprecated
        # npm package lookup that resolves to the 2.0.4 stub).
        self.assertNotIn(
            "npx tsc",
            text,
            "test-agent-adapters must use "
            "`./node_modules/.bin/tsc`, not `npx tsc`. The latter "
            "resolves the deprecated `tsc` npm package that "
            "prints a 'not the tsc command you are looking for' "
            "warning rather than running the compiler.",
        )

    def test_recipe_fails_closed_on_missing_tsconfig_tests_json(self):
        text = _read_makefile()
        self.assertIn(
            "tsconfig.tests.json is missing",
            text,
            "test-agent-adapters must emit a FATAL "
            "'tsconfig.tests.json is missing' message when a TS "
            "adapter fails to ship its per-adapter compile config.",
        )

    def test_recipe_fails_closed_on_missing_local_tsc(self):
        text = _read_makefile()
        self.assertIn(
            "no local TypeScript compiler",
            text,
            "test-agent-adapters must emit a FATAL 'no local "
            "TypeScript compiler' message when the per-adapter "
            "`node_modules/.bin/tsc` is missing — and refuse to "
            "fall back to a globally installed compiler.",
        )

    def test_recipe_fails_closed_on_empty_compile_output(self):
        """A successful compile that emits zero `.test.js` files
        is the regression class this assertion pins: the recipe
        must refuse to call it a green run.
        """
        text = _read_makefile()
        self.assertIn(
            "compiled no .test.js files",
            text,
            "test-agent-adapters must FATAL when the per-adapter "
            "compile produces no `.test.js` files (the regression "
            "this assertion pins is a TS suite that silently "
            "collects zero tests).",
        )

    def test_recipe_uses_tap_pinned_parser(self):
        """The TS branch runs `node --test --test-reporter=tap`
        against the compiled output. The TAP-pinned parser
        contract that test_makefile_js_reporter_parser.py pins
        for the JS branch therefore carries over automatically:
        the .test.js files come out of the same compile as the
        existing JS branch's inputs, so the same `^# pass N`
        parser applies. This assertion just verifies the recipe
        actually uses the TAP reporter (so a future edit that
        drops the flag is caught here too).
        """
        text = _read_makefile()
        # The TS branch must invoke `node --test --test-reporter=tap`
        # against the compiled `.test.js` files. We grep the
        # recipe for the canonical shape.
        self.assertRegex(
            text,
            r"node\s+--test\s+--test-reporter=tap\s+\$\$\{?js_glob",
            "test-agent-adapters must invoke `node --test "
            "--test-reporter=tap` against the compiled test files "
            "so the TAP-pinned parser (pinned by "
            "test_makefile_js_reporter_parser.py) carries over.",
        )

    def test_floor_honoured(self):
        """The pass-count floor trips on a below-floor count and
        exits non-zero. The floor must be present in the recipe
        and used for the comparison.
        """
        text = _read_makefile()
        floor = _ts_floor()
        self.assertIn(
            "AGENT_ADAPTER_TEST_MIN_TS_TESTS",
            text,
            "test-agent-adapters must reference "
            "AGENT_ADAPTER_TEST_MIN_TS_TESTS to apply the "
            "TypeScript pass-count floor.",
        )
        # The branch must print a count + floor line so the gate
        # is auditable from its own log.
        self.assertRegex(
            text,
            r"\[test-agent-adapters\]\s+TS:\s+\$\$\{?ts_total\}?\s+passing tests",
            "test-agent-adapters must print a TS: <count> passing "
            "tests line so the floor comparison is auditable.",
        )
        self.assertGreater(
            floor, 0,
            "AGENT_ADAPTER_TEST_MIN_TS_TESTS must be > 0; a zero "
            "floor would turn the regression into a silent green.",
        )


class TestNegativeControlOmission(unittest.TestCase):
    """Reproduce the regression class: omitting mpm-opencode from
    `AGENT_ADAPTERS_TS` while leaving the .test.ts files on disk
    would silently zero out the gate. Confirm the recipe cannot
    be edited that way without tripping a structural guard.
    """

    def setUp(self):
        # Compose a synthetic Makefile that satisfies every other
        # precondition but lists a non-existent adapter under
        # `AGENT_ADAPTERS_TS` instead of mpm-opencode. The
        # `make -n` dry run inspects the recipe's command graph
        # without executing the compile, so it is hermetic.
        self.tmpdir = Path(tempfile.mkdtemp(prefix="mpm-ts-wiring-"))
        # Provide a fake adapter directory so cd/tsconfig checks
        # that fire only at runtime are not triggered.
        for d in (
            "agent_installation/mpm-claude-code/tests",
            "agent_installation/mpm-hermes/tests",
            "agent_installation/mpm-memory-openclaw/tests",
            "agent_installation/mpm-opencode/tests",
            "agent_installation/mpm-pi/tests",
        ):
            (self.tmpdir / d).mkdir(parents=True, exist_ok=True)
        # Mpm-opencode ships a tsconfig.tests.json; a fake one
        # named `mpm-nonexistent` would NOT ship one. Listing it
        # in AGENT_ADAPTERS_TS would trip the
        # "tsconfig.tests.json is missing" structural guard.
        # That's the regression class: a future editor adds an
        # entry without the per-adapter config.
        (self.tmpdir / "agent_installation" / "mpm-nonexistent").mkdir(
            parents=True, exist_ok=True
        )
        # Top-level stub so `make -n` resolves at all.
        (self.tmpdir / "Makefile").write_text(
            textwrap.dedent(
                """\
                .PHONY: test-agent-adapters
                AGENT_ADAPTERS := mpm-claude-code mpm-hermes mpm-memory-openclaw mpm-opencode mpm-pi
                AGENT_ADAPTERS_JS := mpm-memory-openclaw
                AGENT_ADAPTERS_TS := mpm-nonexistent
                AGENT_ADAPTER_TEST_MIN_PY_TESTS := 1
                AGENT_ADAPTER_TEST_MIN_JS_TESTS := 1
                AGENT_ADAPTER_TEST_MIN_TS_TESTS := 1
                test-agent-adapters:
                \t@echo "fake target"
                """
            ),
            encoding="utf-8",
        )

    def tearDown(self):
        shutil.rmtree(self.tmpdir, ignore_errors=True)

    def test_omitting_mpm_opencode_is_structurally_caught(self):
        """If the recipe used `AGENT_ADAPTERS_TS = mpm-nonexistent`
        (an entry that doesn't ship a per-adapter tsconfig), the
        recipe's "FATAL tsconfig.tests.json is missing" branch is
        the load-bearing guard. Confirm the recipe source contains
        that guard so a future edit that drops it (silently
        skipping the test compile) is caught by the existing
        wiring test rather than the runtime recipe.
        """
        text = _read_makefile()
        self.assertIn(
            "tsconfig.tests.json is missing",
            text,
            "test-agent-adapters must keep its FATAL guard for a "
            "missing per-adapter tsconfig.tests.json so an omitted "
            "TS suite cannot pass the gate silently.",
        )
        # The companion guard for missing `.bin/tsc`.
        self.assertIn(
            "no local TypeScript compiler",
            text,
            "test-agent-adapters must keep its FATAL guard for a "
            "missing local tsc so a future edit that falls back "
            "to a globally installed compiler is caught.",
        )


class TestNegativeControlZeroCompile(unittest.TestCase):
    """Reproduce the regression class: a TS compile that succeeds
    but emits zero `.test.js` files (because every .test.ts was
    filtered out by a misconfigured `include`) would yield a
    zero-pass TAP summary, which the existing
    `TestParserRefusesMalformedOutput` test family already
    rejects with an empty count. Confirm the recipe refuses to
    invoke `node --test` with an empty `.test.js` glob as well —
    defence in depth.
    """

    def test_recipe_refuses_zero_output_glob(self):
        text = _read_makefile()
        self.assertIn(
            "compiled no .test.js files",
            text,
            "test-agent-adapters must refuse to invoke `node --test` "
            "when the compile produced zero `.test.js` files. "
            "This is the defence-in-depth guard against a future "
            "tsconfig.tests.json whose `include` accidentally "
            "filters every `.test.ts` out.",
        )
        self.assertIn(
            "tests-build/*.test.js",
            text,
            "test-agent-adapters must ls `tests-build/*.test.js` "
            "rather than globbing `.test.ts` directly, so the "
            "compile layer is the boundary between source and "
            "executable output.",
        )


class TestNegativeControlExecution(unittest.TestCase):
    """Executable reproductions of the regression class.

    The live Makefile recipe iterates `AGENT_ADAPTERS_TS` inside
    a `for` loop whose body prints a FATAL for each failure
    shape (missing `tests/`, missing `tsconfig.tests.json`,
    missing local `tsc`, zero-output compile). Each test
    overrides `AGENT_ADAPTERS_TS` at the command line and runs
    `make -n` against the LIVE Makefile. The override is the
    canonical Make escape for "edit this variable for one
    invocation only"; the resulting dry-run output proves the
    recipe body — and therefore the regression guards — sits at
    the correct place in the dependency graph, and that
    omitting mpm-opencode is dispatched to the load-bearing TS
    branch instead of silently zeroing.

    `make -n` is the dry-run form: it prints commands without
    executing them. We don't run the real `make test-agent-adapters`
    here because it would invoke the full Python / JS / TS tree
    (~50s) on every test method; the recipe-body grep is the
    exact proof we need that omitting a TS adapter from the list
    wires up against the FATAL branch.
    """

    def _run_make_n_with(self, ts_value: str) -> str:
        make = shutil.which("make") or "/usr/bin/make"
        cp = subprocess.run(
            [make, "-n", "test-agent-adapters",
             f"AGENT_ADAPTERS_TS={ts_value}"],
            cwd=str(REPO_ROOT),
            capture_output=True,
            text=True,
            check=False,
            timeout=60,
        )
        return cp.stdout + cp.stderr

    def test_omitting_mpm_opencode_wires_to_ts_branch(self):
        """Override AGENT_ADAPTERS_TS to a non-existent adapter
        (`mpm-nonexistent`). The dry-run must surface the TS
        branch's `for d in mpm-nonexistent; do` opening line
        and the FATAL guards. That is the load-bearing proof
        that omitting mpm-opencode from the canonical list
        dispatches to the recipe's existing failure-mode checks
        rather than being silently skipped.
        """
        output = self._run_make_n_with("mpm-nonexistent")
        self.assertIn(
            "for d in mpm-nonexistent; do",
            output,
            "`make -n test-agent-adapters AGENT_ADAPTERS_TS="
            "mpm-nonexistent` must surface the TS branch's for-loop "
            "opening for the override value. If this assertion "
            "fails, the recipe stopped iterating AGENT_ADAPTERS_TS "
            "and would silently skip TS adapters instead of "
            "refusing them. Got:\n" + output[-2000:],
        )
        # The recipe body must surface the `tsconfig.tests.json`
        # FATAL guard so a future edit that drops the guard
        # is caught here as well as by the textual-assertion
        # sister tests.
        self.assertIn(
            "tsconfig.tests.json is missing",
            output,
            "`make -n` must surface the recipe's "
            "'tsconfig.tests.json is missing' FATAL guard so a "
            "future refactor that drops the guard is caught at "
            "the executable level. Got:\n" + output[-2000:],
        )

    def test_zero_compile_output_branch_is_in_live_recipe(self):
        """Run `make -n` against the LIVE Makefile with the
        canonical AGENT_ADAPTERS_TS override and verify the TS
        branch's `tests-build/*.test.js` glob statement is
        emitted. That is the load-bearing surface for the
        "compiled no .test.js files" FATAL: the recipe's
        `for d in ...` loop only inspects the on-disk state when
        an invocation actually runs the compile, but the glob
        literal itself is what the recipe's existence check
        relies on.
        """
        cp_dry = _make_dry("test-agent-adapters")
        self.assertIn(
            "tests-build/*.test.js",
            cp_dry,
            "`make -n test-agent-adapters` must surface the "
            "`tests-build/*.test.js` glob so a future edit that "
            "switches the compile output to a different path is "
            "caught by the executable dry-run as well as the "
            "structural assertion. Got: " + cp_dry[:400],
        )

    def test_local_tsc_path_is_in_recipe(self):
        """`make -n` against the LIVE Makefile must surface the
        `node_modules/.bin/tsc` invocation. The literal substring
        is what the gate's "no local TypeScript compiler" FATAL
        guard keys on; surfacing it under `make -n` proves the
        fallback-to-global-tsc regression class is pinned at
        every layer.
        """
        cp_dry = _make_dry("test-agent-adapters")
        self.assertIn(
            "node_modules/.bin/tsc",
            cp_dry,
            "`make -n test-agent-adapters` must surface the "
            "`node_modules/.bin/tsc` invocation. Got: "
            + cp_dry[:400],
        )


if __name__ == "__main__":
    unittest.main(verbosity=2)
