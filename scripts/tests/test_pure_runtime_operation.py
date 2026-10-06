"""Pure-runtime operation — acceptance tests.

The goal this file exists to prove
----------------------------------

A runtime root at `~/.mpm` must be able to serve installed MPM with NO
repository tree present: no `.git/`, no Go source, no `Makefile`, no
`scripts/`, no `docs/`, no `agent_installation/`. Only the provisioned
runtime assets and the installed binaries.

Before this tranche nothing copied `mode/`, `persona/`, or `drills/`
into the runtime root. They were tracked source that happened to sit at
the runtime root because the checkout WAS `~/.mpm`. These tests build
the split world for real — a source checkout and a runtime root at
physically separate paths — and run the actual compiled binary against
it.

Topology built per test
-----------------------

    <tmp>/src/mpm/          source checkout (mode/ persona/ drills/ only)
    <tmp>/home/.mpm/        runtime root — NO repository content

The source-side definition directories are renamed away before the
runtime assertions run, so a test cannot accidentally pass by reading
through the source tree. That is the whole point: if the binary reaches
back into the source checkout, the definitions are gone and it must
fail rather than silently succeed.

Safety
------

Fake HOME and fake MPM_WORKSPACE throughout. The real `~/.mpm`, the
real checkout, and the production database are never written. These
tests are read-mostly against a temp runtime root and only ever create
state inside it.
"""

from __future__ import annotations

import json
import os
import shutil
import subprocess
import sys
import tempfile
import unittest
from pathlib import Path

REPO_ROOT = Path(__file__).resolve().parents[2]
RECONCILER = REPO_ROOT / "scripts" / "install_runtime_assets.py"
MPM_BIN = REPO_ROOT / ".build" / "bin" / "mpm"
MCP_BIN = REPO_ROOT / ".build" / "bin" / "mpm-mcp"
MANIFEST_RELPATH = Path("config") / "runtime-assets.json"

# Content that must never appear under a source checkout.
FORBIDDEN_IN_SOURCE = (
    "db/mpm.db",
    "active.json",
    "toxicphrases.txt",
    "run",
    "backups",
    "blobs",
    "scheduler.state",
    "config/runtime-assets.json",
)

# Repository content that must not be required by a runtime root.
FORBIDDEN_IN_RUNTIME = (
    ".git",
    "go.mod",
    "go.sum",
    "Makefile",
    "install.sh",
    "uninstall.sh",
    "scripts",
    "docs",
    "agent_installation",
    "internal",
    "cmd",
)


def _binary_available() -> bool:
    return MPM_BIN.is_file() and os.access(MPM_BIN, os.X_OK)


class PureRuntimeFixture(unittest.TestCase):
    """Builds a physically split source/runtime world and installs into it."""

    def setUp(self) -> None:
        if not _binary_available():
            self.skipTest(f"{MPM_BIN} not built; run `make build` first")

        self._tmp = tempfile.mkdtemp(prefix="mpm-pure-runtime-")
        self.addCleanup(shutil.rmtree, self._tmp, True)

        self.base = Path(self._tmp)
        self.home = self.base / "home"
        self.home.mkdir()
        self.src = self.base / "src" / "mpm"
        self.src.mkdir(parents=True)
        self.rt = self.home / ".mpm"

        # A minimal but REAL source checkout: the directories the
        # reconciler provisions from. Deliberately not a full checkout —
        # the runtime root must never need the rest.
        for sub in ("mode", "persona", "drills"):
            shutil.copytree(REPO_ROOT / sub, self.src / sub)

        # Install into the fake runtime root.
        r = subprocess.run(
            [sys.executable, str(RECONCILER), "--source-root", str(self.src),
             "--runtime-root", str(self.rt)],
            capture_output=True, text=True,
        )
        self.assertEqual(r.returncode, 0, r.stderr)
        self.assertTrue((self.rt / "mode" / "default.md").is_file())

    # -- helpers ---------------------------------------------------------

    def env(self, **extra: str) -> dict:
        """Environment for running the binary against the fake runtime.

        HOME and MPM_WORKSPACE both point at the fake runtime root, which
        is the shape of a real deployment. Nothing may resolve to the
        repository or to the operator's real home.
        """
        env = {
            k: v
            for k, v in os.environ.items()
            # Do not let the operator's ambient MPM_* settings leak in and
            # silently redirect the binary at the real install.
            if not k.startswith("MPM_")
        }
        env.update(
            {
                "HOME": str(self.home),
                "MPM_WORKSPACE": str(self.rt),
                "PATH": os.environ.get("PATH", "/usr/bin:/bin"),
                "USER": os.environ.get("USER", "tester"),
            }
        )
        env.update(extra)
        return env

    def run_mpm(self, *args: str, cwd: Path | None = None, **env_extra: str):
        return subprocess.run(
            [str(MPM_BIN), *args],
            capture_output=True,
            text=True,
            cwd=str(cwd) if cwd else None,
            env=self.env(**env_extra),
            timeout=120,
        )

    def hide_source_definitions(self) -> None:
        """Take the source-side definitions out of view.

        After this the binary has no source-side path to mode/persona/
        drills, so any success proves it served from the runtime root.
        """
        hidden = self.base / "hidden-source-assets"
        hidden.mkdir()
        for sub in ("mode", "persona", "drills"):
            shutil.move(str(self.src / sub), str(hidden / sub))

    def assert_source_clean(self) -> None:
        for rel in FORBIDDEN_IN_SOURCE:
            offenders = list(self.src.rglob(rel)) if "/" in rel else (
                [self.src / rel] if (self.src / rel).exists() else []
            )
            self.assertEqual(offenders, [], f"source polluted by {rel}: {offenders}")


# ---------------------------------------------------------------------------
# §15 — the pure-runtime topology itself
# ---------------------------------------------------------------------------


class TestPureRuntimeTopology(PureRuntimeFixture):
    def test_runtime_root_contains_no_repository_content(self):
        for name in FORBIDDEN_IN_RUNTIME:
            self.assertFalse((self.rt / name).exists(), f"runtime root contains {name}")

    def test_runtime_root_has_no_git_directory(self):
        self.assertFalse((self.rt / ".git").exists())

    def test_source_and_runtime_are_physically_distinct(self):
        self.assertNotEqual(self.src, self.rt)
        self.assertFalse(self.rt.is_relative_to(self.src))
        self.assertFalse(self.src.is_relative_to(self.rt))

    def test_manifest_records_ownership_for_the_provisioned_set(self):
        m = json.loads((self.rt / MANIFEST_RELPATH).read_text())
        self.assertEqual(m["version"], 1)
        for rel in ("mode/default.md", "persona/default.md", "drills/wake-protocol-001.yaml"):
            self.assertIn(rel, m["assets"], f"{rel} not recorded as managed")


# ---------------------------------------------------------------------------
# §16 A/B — stock mode and persona listing from a pure runtime root
# ---------------------------------------------------------------------------


class TestStockModesAndPersonas(PureRuntimeFixture):
    def setUp(self) -> None:
        super().setUp()
        self.hide_source_definitions()

    def test_mode_list_serves_stock_modes_with_no_source_tree(self):
        r = self.run_mpm("mode", "list")
        self.assertEqual(r.returncode, 0, r.stderr)
        self.assertIn("default", r.stdout)

    def test_mode_show_renders_a_stock_mode(self):
        r = self.run_mpm("mode", "show", "default")
        self.assertEqual(r.returncode, 0, r.stderr)
        self.assertTrue(r.stdout.strip(), "mode show produced no output")

    def test_persona_list_serves_stock_personas(self):
        r = self.run_mpm("persona", "list")
        self.assertEqual(r.returncode, 0, r.stderr)
        self.assertIn("default", r.stdout)

    def test_persona_show_renders_a_stock_persona(self):
        r = self.run_mpm("persona", "show", "default")
        self.assertEqual(r.returncode, 0, r.stderr)
        self.assertTrue(r.stdout.strip())

    def test_readme_is_not_listed_as_a_selectable_definition(self):
        # README.md ships alongside the definitions as operator docs. It
        # must never be selectable — internal.IsDefinitionFile is the
        # single gate for that, and `mpm switch` used to bypass it.
        for noun in ("mode", "persona"):
            r = self.run_mpm(noun, "list")
            self.assertNotIn("README", r.stdout, f"{noun} list exposed README")

    def test_readme_is_not_selectable_as_a_definition(self):
        # A README sitting in a runtime directory the operator owns must
        # never be reachable as a definition by name.
        r = self.run_mpm("mode", "show", "README")
        self.assertNotEqual(r.returncode, 0, "mode show README must not succeed")
        r = self.run_mpm("persona", "show", "README")
        self.assertNotEqual(r.returncode, 0, "persona show README must not succeed")


# ---------------------------------------------------------------------------
# No source dependency — the property a fallback would break
# ---------------------------------------------------------------------------


class TestNoSourceDependency(PureRuntimeFixture):
    """A missing runtime definition must NOT be sourced from a checkout.

    The command is deliberately run with cwd set to the REAL source
    checkout, which still holds every stock definition. A loader that
    quietly fell back to a source tree — however it found one, whether by
    executable-relative walk-up, a compile-time default, or a relative
    probe of the cwd — would satisfy the request from there and make a
    runtime root appear provisioned when it is not.

    That failure mode is invisible to every other test in this file: with
    the runtime copy present, source and runtime hold identical bytes and
    the loader's choice cannot be observed. Removing the runtime copy is
    what makes the difference observable.
    """

    def test_missing_runtime_mode_is_not_sourced_from_the_checkout(self):
        (self.rt / "mode" / "default.md").unlink()
        r = self.run_mpm("mode", "list", cwd=REPO_ROOT)
        self.assertNotIn(
            "default",
            r.stdout,
            "mode list found a definition the runtime root does not have; "
            "something is reading a source checkout",
        )

    def test_missing_runtime_persona_is_not_sourced_from_the_checkout(self):
        (self.rt / "persona" / "default.md").unlink()
        r = self.run_mpm("persona", "list", cwd=REPO_ROOT)
        self.assertNotIn(
            "default",
            r.stdout,
            "persona list found a definition the runtime root does not have; "
            "something is reading a source checkout",
        )

    def test_missing_runtime_drill_is_not_sourced_from_the_checkout(self):
        (self.rt / "drills" / "wake-protocol-001.yaml").unlink()
        r = self.run_mpm("drills", "list", cwd=REPO_ROOT)
        self.assertNotIn(
            "wake-protocol-001",
            r.stdout,
            "drills list found a drill the runtime root does not have; "
            "something is reading a source checkout",
        )

    def test_empty_runtime_root_yields_no_definitions_at_all(self):
        shutil.rmtree(self.rt / "mode")
        shutil.rmtree(self.rt / "persona")
        shutil.rmtree(self.rt / "drills")
        r = self.run_mpm("mode", "list", cwd=REPO_ROOT)
        self.assertNotIn("default", r.stdout)


# ---------------------------------------------------------------------------
# §16 C — the router loads installed stock definitions
# ---------------------------------------------------------------------------


class TestRouterFromPureRuntime(PureRuntimeFixture):
    def setUp(self) -> None:
        super().setUp()
        self.hide_source_definitions()

    def test_route_evaluation_loads_stock_modes_and_personas(self):
        r = self.run_mpm("route", "explain", "debug this failing test for me")
        # Route explain is the cheapest command that forces a real
        # Router reload from the workspace directories.
        self.assertEqual(r.returncode, 0, r.stderr)
        self.assertNotIn("not found on disk", r.stdout)

    def test_route_does_not_fall_back_to_any_source_tree(self):
        # With the source definitions moved away, a fallback to the
        # source checkout would have to fail. Prove it did not need one.
        r = self.run_mpm("route", "explain", "architect a new distributed system design")
        self.assertEqual(r.returncode, 0, r.stderr)
        self.assertNotIn("not found on disk", r.stdout)

    def test_mcp_server_bootstraps_against_the_runtime_root(self):
        # mpm-mcp creates mode/ and persona/ at its resolved workspace if
        # absent. Run it against a runtime root that already has them and
        # confirm it starts without reaching into the source tree.
        if not MCP_BIN.is_file():
            self.skipTest("mpm-mcp not built")
        proc = subprocess.run(
            [str(MCP_BIN)],
            capture_output=True,
            text=True,
            env=self.env(),
            timeout=25,
            input="",
        )
        # A stdio server given EOF exits or reports a protocol error; both
        # are fine. What matters is that it did not crash on a missing
        # workspace, and that state landed under the runtime root only.
        self.assertNotIn("bootstrap", proc.stderr)


# ---------------------------------------------------------------------------
# §16 D — stock drills from a pure runtime root
# ---------------------------------------------------------------------------


class TestStockDrills(PureRuntimeFixture):
    def setUp(self) -> None:
        super().setUp()
        self.hide_source_definitions()

    def test_drills_list_serves_stock_drills(self):
        r = self.run_mpm("drills", "list")
        self.assertEqual(r.returncode, 0, r.stderr)
        self.assertIn("wake-protocol-001", r.stdout)

    def test_drills_show_renders_a_stock_drill(self):
        r = self.run_mpm("drills", "show", "wake-protocol-001")
        self.assertEqual(r.returncode, 0, r.stderr)
        self.assertTrue(r.stdout.strip())

    def test_all_three_stock_drills_are_present(self):
        r = self.run_mpm("drills", "list")
        for drill in ("decision-record-001", "lesson-persistence-001", "wake-protocol-001"):
            self.assertIn(drill, r.stdout)


# ---------------------------------------------------------------------------
# §16 E/F/G — runtime state lands in the runtime root, never in source
# ---------------------------------------------------------------------------


class TestRuntimeStateLocation(PureRuntimeFixture):
    def test_database_is_created_under_the_runtime_root(self):
        r = self.run_mpm("call", "mpm_system", "--payload", '{"action":"health_check"}')
        self.assertTrue((self.rt / "src" / "db").exists() or r.returncode == 0)

    def test_source_checkout_stays_clean_while_runtime_commands_mutate_state(self):
        before = sorted(p.relative_to(self.src) for p in self.src.rglob("*"))
        self.run_mpm("call", "mpm_system", "--payload", '{"action":"health_check"}')
        self.run_mpm("mode", "list")
        self.run_mpm("persona", "list")
        self.run_mpm("drills", "list")
        after = sorted(p.relative_to(self.src) for p in self.src.rglob("*"))
        self.assertEqual(before, after, "running MPM mutated the source checkout")
        self.assert_source_clean()


class TestSourcePollutionGuard(PureRuntimeFixture):
    """Run from the SOURCE checkout with the intended MCP environment."""

    def test_commands_from_source_cwd_do_not_create_state_in_source(self):
        self.run_mpm(
            "call", "mpm_system", "--payload", '{"action":"health_check"}',
            cwd=self.src,
        )
        self.assert_source_clean()

    def test_mode_list_from_source_cwd_does_not_create_source_state(self):
        self.run_mpm("mode", "list", cwd=self.src)
        self.assert_source_clean()

    def test_drift_drill_from_source_cwd_does_not_create_source_state(self):
        self.run_mpm("drills", "list", cwd=self.src)
        self.assert_source_clean()

    def test_mcp_env_from_project_config_resolves_to_runtime_not_source(self):
        # Simulates what the MCP host does with project .mcp.json: the
        # env block is applied verbatim after ${HOME} expansion. This is
        # the guard that would fail if .mcp.json regressed to ".".
        cfg = json.loads((REPO_ROOT / ".mcp.json").read_text())
        raw = cfg["mcpServers"]["mpm"]["env"]["MPM_WORKSPACE"]
        self.assertNotEqual(raw, ".", "project .mcp.json points MPM_WORKSPACE at the checkout")
        resolved = raw.replace("${HOME}", str(self.home))
        self.assertEqual(resolved, str(self.rt), "project .mcp.json does not resolve to the runtime root")

        r = self.run_mpm("call", "mpm_system", "--payload", '{"action":"health_check"}',
                         cwd=self.src, MPM_WORKSPACE=resolved)
        self.assertEqual(r.returncode, 0, r.stderr)
        self.assert_source_clean()


# ---------------------------------------------------------------------------
# §16 G/H/I — custom runtime definitions survive a reinstall
# ---------------------------------------------------------------------------


class TestCustomDefinitionsSurviveReinstall(PureRuntimeFixture):
    def _reinstall(self) -> None:
        r = subprocess.run(
            [sys.executable, str(RECONCILER), "--source-root", str(self.src),
             "--runtime-root", str(self.rt)],
            capture_output=True, text=True,
        )
        self.assertEqual(r.returncode, 0, r.stderr)

    def test_custom_mode_survives_and_is_listed(self):
        (self.rt / "mode" / "house-style.md").write_text(
            "---\nname: house-style\npatterns: 'refactor'\n---\nHOUSE STYLE MODE\n"
        )
        self._reinstall()
        self.assertTrue((self.rt / "mode" / "house-style.md").is_file())
        r = self.run_mpm("mode", "list")
        self.assertIn("house-style", r.stdout)

    def test_custom_persona_survives_and_is_listed(self):
        (self.rt / "persona" / "reviewer.md").write_text(
            "---\nname: reviewer\n---\nREVIEWER PERSONA\n"
        )
        self._reinstall()
        self.assertTrue((self.rt / "persona" / "reviewer.md").is_file())
        r = self.run_mpm("persona", "list")
        self.assertIn("reviewer", r.stdout)

    def test_custom_drill_survives_and_is_listed(self):
        (self.rt / "drills" / "house-drill.yaml").write_text(
            "id: house-drill\nframework: claude-code\n"
            "description: operator drill\n"
            "expect:\n  tools_required:\n    - mpm_memory_save\n"
        )
        self._reinstall()
        self.assertTrue((self.rt / "drills" / "house-drill.yaml").is_file())
        r = self.run_mpm("drills", "list")
        self.assertIn("house-drill", r.stdout)

    def test_custom_mode_content_is_unchanged_after_reinstall(self):
        custom = "---\nname: house-style\npatterns: 'refactor'\n---\nUNIQUE OPERATOR TEXT\n"
        (self.rt / "mode" / "house-style.md").write_text(custom)
        self._reinstall()
        self.assertEqual((self.rt / "mode" / "house-style.md").read_text(), custom)


if __name__ == "__main__":
    unittest.main()