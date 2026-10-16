"""
test_release_gate_bootstrap_wiring.py — Pin the contract that the
GitHub Actions `release-gate` job installs the mpm-opencode npm
devDependencies before invoking `make release-gate`.

Background:
  `make release-gate` -> `make test-race` -> `make test-agent-
  installation` -> `make test-agent-adapters` reaches the
  mpm-opencode TypeScript suite, which compiles its `.test.ts` files
  via the adapter-local `node_modules/.bin/tsc` and runs the
  resulting `.test.js` through the same `node --test --test-reporter=
  tap` pipeline the JS branch uses. The Makefile target fails closed
  with a FATAL message if that compiler binary is missing — it
  deliberately does NOT fall back to a globally installed tsc, and
  it does NOT auto-run `npm ci` itself. A green run on a clean
  checkout therefore requires the workflow to populate
  `agent_installation/mpm-opencode/node_modules/` before the gate
  step.

  This file pins the wiring that makes that bootstrap explicit:

    * the release-gate job uses `actions/setup-node` to pin Node 20
      (the LTS that satisfies `engines.node: ">=20"` in
      agent_installation/mpm-opencode/package.json);
    * the release-gate job runs `npm ci` against
      `agent_installation/mpm-opencode/package-lock.json` BEFORE
      `make release-gate` (the order matters: `make` invokes the
      gate, the gate would fail without the dependency, so the
      bootstrap must precede the gate);
    * the lockfile is committed (so `npm ci` is reproducible
      rather than re-resolving transitive deps);
    * cache configuration is keyed to the lockfile path (so a
      warm cache is reused on subsequent runs);
    * the `gate` and `full-suite` jobs do NOT carry an
      `npm ci` step, because their execution paths do not reach
      `test-agent-installation` (the only consumer of the TS
      compiler); adding the bootstrap there would be a redundant
      download.

Negative controls (executable reproductions of the regression class):

  * Removing the `npm ci` step from the release-gate job would
    leave the Makefile gate to fail-closed with its FATAL
    message; a CI red would still surface. But the FAILURE
    message would be confusing (it would say "no local
    TypeScript compiler" rather than "missing npm bootstrap"),
    and a future edit that ALSO removed the FATAL branch in
    the Makefile could re-introduce a silent-green shape. The
    structural guard here pins the workflow file's contents so
    a regression in the workflow gets caught at the test layer
    rather than as an opaque "0 tests" CI run.

  * Moving the `npm ci` step into a different job (e.g. the
    `gate` job) would make the bootstrap execute on every
    push/PR where it is not needed, costing ~30-60s of
    install time across the matrix without any reachability
    benefit. The scope assertion below pins the bootstrap to
    the release-gate job specifically.

Hermeticity:
  No subprocess is spawned. The workflow file is parsed as text.
  Assertions operate on the file's contents directly.
"""

from __future__ import annotations

import re
import unittest
from pathlib import Path

REPO_ROOT = Path(__file__).resolve().parent.parent.parent
WORKFLOW = REPO_ROOT / ".github" / "workflows" / "build-test.yml"
OPENCODE_PKG_JSON = REPO_ROOT / "agent_installation" / "mpm-opencode" / "package.json"
OPENCODE_PKG_LOCK = REPO_ROOT / "agent_installation" / "mpm-opencode" / "package-lock.json"


def _read_workflow() -> str:
    return WORKFLOW.read_text(encoding="utf-8")


def _job_block(text: str, job_name: str) -> str:
    """Return the body of a `job_name:` top-level mapping in the
    workflow YAML. Returns '' when the job is absent. This is
    structural, not a parsed AST — the parser stops at the next
    `  <name>:` line at the same indentation, which matches the
    workflow's two-space indent for top-level job keys.
    """
    pattern = re.compile(
        rf"^  {re.escape(job_name)}:\s*$", re.MULTILINE
    )
    match = pattern.search(text)
    if not match:
        return ""
    after = text[match.end():]
    lines: list[str] = []
    for raw_line in after.splitlines(keepends=False):
        if raw_line.startswith("    ") or raw_line == "":
            lines.append(raw_line)
            continue
        break
    return "\n".join(lines)


class TestReleaseGateBootstrap(unittest.TestCase):
    """Pin that the release-gate job installs mpm-opencode
    devDependencies before invoking the gate.
    """

    def setUp(self):
        self.text = _read_workflow()
        self.job = _job_block(self.text, "release-gate")
        self.assertTrue(
            self.job,
            "release-gate job is absent from .github/workflows/build-test.yml",
        )

    def test_release_gate_uses_setup_node(self):
        """The release-gate job must use actions/setup-node so
        `npm ci` has a pinned Node binary to run against.
        """
        self.assertRegex(
            self.job,
            r"uses:\s*actions/setup-node@v\d+",
            "release-gate must invoke actions/setup-node so npm ci "
            "has a Node binary.",
        )

    def test_release_gate_pins_node_20(self):
        """Node 20 LTS is the brief-mandated pin: it satisfies
        `engines.node: ">=20"` and is the LTS the project has
        been validated against on the ubuntu-24.04 runner.
        """
        self.assertRegex(
            self.job,
            r"node-version:\s*['\"]?20['\"]?",
            "release-gate must pin Node 20 (LTS; satisfies the "
            "package.json engines.node floor).",
        )

    def test_release_gate_runs_npm_ci_in_opencode_dir(self):
        """`npm ci` must run in agent_installation/mpm-opencode
        so the adapter's devDependencies (typescript) are
        installed.
        """
        self.assertRegex(
            self.job,
            r"working-directory:\s*agent_installation/mpm-opencode",
            "release-gate must run npm ci with "
            "working-directory: agent_installation/mpm-opencode.",
        )
        self.assertRegex(
            self.job,
            r"run:\s*npm\s+ci\b",
            "release-gate must invoke `npm ci` (idempotent, "
            "lockfile-driven install).",
        )

    def test_npm_ci_step_precedes_make_release_gate(self):
        """The bootstrap must run before the gate. A future edit
        that reorders the steps would leave the gate to fail
        with a confusing FATAL. We assert the byte offset of
        `npm ci` is less than the offset of `make release-gate`
        inside the release-gate job.
        """
        npm_ci_match = re.search(r"run:\s*npm\s+ci\b", self.job)
        gate_match = re.search(r"run:\s*make\s+release-gate", self.job)
        self.assertIsNotNone(
            npm_ci_match,
            "release-gate must contain an `npm ci` step.",
        )
        self.assertIsNotNone(
            gate_match,
            "release-gate must contain a `make release-gate` step.",
        )
        self.assertLess(
            npm_ci_match.start(),
            gate_match.start(),
            "the `npm ci` step must run before `make release-gate` "
            "so the gate has devDependencies when the Makefile "
            "test-agent-adapters TS branch needs node_modules/.bin/tsc.",
        )

    def test_cache_uses_lockfile_path(self):
        """Cache must be keyed to the actual lockfile so a
        clean-checkout rebuild under `npm ci` reuses the warm
        cache. A wrong path (e.g. the package.json) would let
        npm cache stale state and obscure dependency changes.
        """
        self.assertRegex(
            self.job,
            r"cache-dependency-path:\s*"
            r"['\"]?agent_installation/mpm-opencode/package-lock\.json['\"]?",
            "release-gate must cache by the package-lock.json path.",
        )

    def test_opencode_lockfile_is_tracked(self):
        """`npm ci` reads package-lock.json. The lockfile must be
        committed, not .gitignored — otherwise the workflow would
        error with "no lockfile found" on a clean checkout.
        """
        self.assertTrue(
            OPENCODE_PKG_LOCK.exists(),
            f"{OPENCODE_PKG_LOCK} must exist on disk (committed).",
        )
        gitignore = REPO_ROOT / "agent_installation" / "mpm-opencode" / ".gitignore"
        if gitignore.exists():
            body = gitignore.read_text(encoding="utf-8")
            for line in body.splitlines():
                stripped = line.strip()
                if not stripped or stripped.startswith("#"):
                    continue
                if "package-lock.json" in stripped:
                    self.fail(
                        f"package-lock.json is listed in {gitignore}; "
                        "`npm ci` requires the lockfile to be committed."
                    )


class TestBootstrapIsScopedToReleaseGate(unittest.TestCase):
    """The bootstrap must NOT leak into jobs that do not reach
    the TS suite. Adding it to `gate` or `full-suite` would be a
    redundant download with no reachability benefit.
    """

    def setUp(self):
        self.text = _read_workflow()

    def test_gate_job_has_no_npm_ci(self):
        job = _job_block(self.text, "gate")
        self.assertTrue(job, "gate job missing")
        self.assertNotRegex(
            job,
            r"run:\s*npm\s+ci\b",
            "gate job must not run `npm ci`; it does not reach "
            "test-agent-installation and the dependency install "
            "would be a redundant download.",
        )

    def test_full_suite_job_has_no_npm_ci(self):
        job = _job_block(self.text, "full-suite")
        self.assertTrue(job, "full-suite job missing")
        self.assertNotRegex(
            job,
            r"run:\s*npm\s+ci\b",
            "full-suite job must not run `npm ci`; it does not "
            "reach test-agent-installation.",
        )


class TestPackageMetadataConsistency(unittest.TestCase):
    """The workflow pin must agree with the package.json floor.
    A future edit that bumps `engines.node` without updating the
    workflow (or vice versa) would re-couple the gate to whatever
    the runner happens to ship.
    """

    def test_engines_node_floor_is_20(self):
        import json
        pkg = json.loads(OPENCODE_PKG_JSON.read_text(encoding="utf-8"))
        floor = pkg.get("engines", {}).get("node", "")
        self.assertTrue(
            floor.startswith(">=20"),
            f"package.json engines.node must be >=20 to match the "
            f"Node 20 LTS workflow pin; got {floor!r}.",
        )


if __name__ == "__main__":
    unittest.main()
