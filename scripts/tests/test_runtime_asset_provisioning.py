"""Runtime-asset provisioning — acceptance tests.

The defect these exist to prevent
--------------------------------

`mode/*.md`, `persona/*.md`, and `drills/*.yaml` are loaded at runtime
from `$MPM_WORKSPACE`. Nothing ever copied them. They worked only
because the canonical checkout IS the runtime root at `~/.mpm`, so
tracked source happened to sit exactly where the binaries look. Move
the source checkout to `~/src/mpm` and a pure runtime root has no mode,
no persona, and no drills — a working-looking install that routes
nothing.

The reconciler under test is `scripts/install_runtime_assets.py`.

What is proven here, all hermetically
-------------------------------------

  * Scenarios A-M — the full ownership case table, one test each, so a
    regression names the specific rule it broke rather than "provisioning
    changed".

  * Pure-runtime operation — source and runtime roots are PHYSICALLY
    SEPARATE, and the runtime root contains no .git, no Go source, no
    Makefile, no scripts/, no docs/. Installed binaries must serve
    stock modes, personas, and drills with no repository tree in sight.

  * Source-pollution guards — running the binary with the intended MCP
    environment from a source checkout must not create a database or
    runtime state under the source. Plus a static guard against
    `.mcp.json` regressing to `MPM_WORKSPACE: "."`, which is precisely
    the config that made the checkout a runtime workspace.

  * Manifest fail-closed behaviour — malformed, empty, unknown-version,
    and path-traversal manifests must all refuse rather than silently
    reverting to "own nothing", which would authorize overwrites.

Safety
------

Every install test uses a fake HOME, PREFIX, and DATA_ROOT under
`tempfile.mkdtemp()`. The real `~/.mpm`, the real checkout, and the
production database are never read for writes or modified. The binary
tests run the built `mpm` from `.build/bin` with an explicit
`MPM_WORKSPACE`; they never touch the live install.
"""

from __future__ import annotations

import hashlib
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
MANIFEST_RELPATH = Path("config") / "runtime-assets.json"

STOCK_MODE = "default.md"
STOCK_PERSONA = "default.md"
STOCK_DRILL = "wake-protocol-001.yaml"


def sha256_of(path: Path) -> str:
    return hashlib.sha256(path.read_bytes()).hexdigest()


def run_reconciler(source: Path, runtime: Path, *extra: str) -> subprocess.CompletedProcess:
    """Invoke the reconciler against explicit roots. Never the real ones."""
    return subprocess.run(
        [
            sys.executable,
            str(RECONCILER),
            "--source-root",
            str(source),
            "--runtime-root",
            str(runtime),
            *extra,
        ],
        capture_output=True,
        text=True,
        env={**os.environ, "HOME": str(runtime.parent)},
    )


class ReconcilerFixture(unittest.TestCase):
    """Shared fixture: a source tree with stock definitions + a runtime root."""

    def setUp(self) -> None:
        self._tmp = tempfile.mkdtemp(prefix="mpm-runtime-assets-")
        self.addCleanup(shutil.rmtree, self._tmp, True)
        self.base = Path(self._tmp)
        self.src = self.base / "src"
        self.rt = self.base / "home" / ".mpm"
        self.src.mkdir(parents=True)
        # Seed from the real repository so the tests exercise real-shaped
        # definitions rather than synthetic stubs that parse differently.
        for sub in ("mode", "persona", "drills"):
            shutil.copytree(REPO_ROOT / sub, self.src / sub)

    # -- helpers ---------------------------------------------------------

    def manifest(self) -> dict:
        return json.loads((self.rt / MANIFEST_RELPATH).read_text())

    def record(self, rel: str) -> str | None:
        entry = self.manifest()["assets"].get(rel)
        return entry["sha256"] if entry else None

    def write_source(self, rel: str, content: str) -> None:
        p = self.src / rel
        p.parent.mkdir(parents=True, exist_ok=True)
        p.write_text(content)

    def write_runtime(self, rel: str, content: str) -> None:
        p = self.rt / rel
        p.parent.mkdir(parents=True, exist_ok=True)
        p.write_text(content)

    def read_runtime(self, rel: str) -> str:
        return (self.rt / rel).read_text()


# ---------------------------------------------------------------------------
# Scenario A — absent and never managed: provision
# ---------------------------------------------------------------------------


class TestScenarioAProvision(ReconcilerFixture):
    def test_fresh_runtime_root_gets_every_stock_definition(self):
        self.assertFalse(self.rt.exists())
        r = run_reconciler(self.src, self.rt)
        self.assertEqual(r.returncode, 0, r.stderr)

        for rel in (f"mode/{STOCK_MODE}", f"persona/{STOCK_PERSONA}", f"drills/{STOCK_DRILL}"):
            self.assertTrue((self.rt / rel).is_file(), f"{rel} was not provisioned")

    def test_provisioned_file_is_byte_identical_to_source(self):
        run_reconciler(self.src, self.rt)
        self.assertEqual(
            sha256_of(self.rt / f"mode/{STOCK_MODE}"),
            sha256_of(self.src / f"mode/{STOCK_MODE}"),
        )

    def test_source_hash_is_recorded(self):
        run_reconciler(self.src, self.rt)
        self.assertEqual(
            self.record(f"mode/{STOCK_MODE}"),
            sha256_of(self.src / f"mode/{STOCK_MODE}"),
        )

    def test_definition_files_are_operator_editable_not_root_owned_readonly(self):
        run_reconciler(self.src, self.rt)
        mode_file = self.rt / f"mode/{STOCK_MODE}"
        # Owner must be able to write: these are operator config, which
        # config/fileperms.go deliberately excludes from the 0600
        # sensitive-state tightening.
        self.assertTrue(mode_file.stat().st_mode & 0o200, "owner-write bit missing")

    def test_rerun_is_a_noop(self):
        run_reconciler(self.src, self.rt)
        mtimes = {p: p.stat().st_mtime_ns for p in self.rt.rglob("*.md")}
        r = run_reconciler(self.src, self.rt)
        self.assertEqual(r.returncode, 0, r.stderr)
        for p, m in mtimes.items():
            self.assertEqual(p.stat().st_mtime_ns, m, f"{p} was rewritten on a no-op run")


# ---------------------------------------------------------------------------
# Scenario B — present, unrecorded, byte-identical: adopt without rewriting
# ---------------------------------------------------------------------------


class TestScenarioBAdopt(ReconcilerFixture):
    def test_identical_unrecorded_file_is_adopted(self):
        self.write_runtime(f"mode/{STOCK_MODE}", (self.src / f"mode/{STOCK_MODE}").read_text())
        r = run_reconciler(self.src, self.rt)
        self.assertEqual(r.returncode, 0, r.stderr)
        self.assertIn("adopted", r.stdout)
        self.assertIsNotNone(self.record(f"mode/{STOCK_MODE}"))

    def test_adoption_does_not_rewrite_the_file(self):
        self.write_runtime(f"mode/{STOCK_MODE}", (self.src / f"mode/{STOCK_MODE}").read_text())
        target = self.rt / f"mode/{STOCK_MODE}"
        before = target.stat().st_mtime_ns
        run_reconciler(self.src, self.rt)
        self.assertEqual(target.stat().st_mtime_ns, before, "adoption rewrote the file")


# ---------------------------------------------------------------------------
# Scenario C — present, unrecorded, differs: preserve, claim nothing
# ---------------------------------------------------------------------------


class TestScenarioCPreserveOperatorOwned(ReconcilerFixture):
    def test_divergent_unrecorded_file_is_preserved(self):
        original = "---\nname: mine\n---\nmy own mode\n"
        self.write_runtime(f"mode/{STOCK_MODE}", original)
        r = run_reconciler(self.src, self.rt)
        self.assertEqual(r.returncode, 0, r.stderr)
        self.assertEqual(self.read_runtime(f"mode/{STOCK_MODE}"), original)

    def test_operator_owned_file_is_not_claimed_in_the_manifest(self):
        self.write_runtime(f"mode/{STOCK_MODE}", "---\nname: mine\n---\nmine\n")
        run_reconciler(self.src, self.rt)
        # Claiming it would arm a future CASE D refresh that overwrites it.
        self.assertIsNone(self.record(f"mode/{STOCK_MODE}"))

    def test_operator_owned_file_is_reported(self):
        self.write_runtime(f"mode/{STOCK_MODE}", "---\nname: mine\n---\nmine\n")
        r = run_reconciler(self.src, self.rt)
        self.assertIn("operator-owned", r.stdout)


# ---------------------------------------------------------------------------
# Scenario D — managed and unmodified
# ---------------------------------------------------------------------------


class TestScenarioDUpstreamRefresh(ReconcilerFixture):
    def test_unchanged_source_is_a_noop(self):
        run_reconciler(self.src, self.rt)
        before = sha256_of(self.rt / f"mode/{STOCK_MODE}")
        r = run_reconciler(self.src, self.rt)
        self.assertEqual(sha256_of(self.rt / f"mode/{STOCK_MODE}"), before)
        self.assertIn("unchanged", r.stdout)

    def test_changed_source_refreshes_an_untouched_managed_file(self):
        run_reconciler(self.src, self.rt)
        new_body = "---\nname: default\npatterns: 'new'\n---\nUPSTREAM v2\n"
        self.write_source(f"mode/{STOCK_MODE}", new_body)
        r = run_reconciler(self.src, self.rt)
        self.assertEqual(r.returncode, 0, r.stderr)
        self.assertIn("refreshed", r.stdout)
        self.assertEqual(self.read_runtime(f"mode/{STOCK_MODE}"), new_body)

    def test_manifest_tracks_the_new_source_hash_after_refresh(self):
        run_reconciler(self.src, self.rt)
        self.write_source(f"mode/{STOCK_MODE}", "---\nname: default\n---\nv2\n")
        run_reconciler(self.src, self.rt)
        self.assertEqual(
            self.record(f"mode/{STOCK_MODE}"),
            sha256_of(self.src / f"mode/{STOCK_MODE}"),
        )

    def test_refresh_happens_for_drills_too(self):
        run_reconciler(self.src, self.rt)
        self.write_source(f"drills/{STOCK_DRILL}", "id: changed\nframework: claude-code\n")
        r = run_reconciler(self.src, self.rt)
        self.assertIn("refreshed", r.stdout)
        self.assertIn("id: changed", self.read_runtime(f"drills/{STOCK_DRILL}"))


# ---------------------------------------------------------------------------
# Scenario E — managed but locally modified: preserve, report, never overwrite
# ---------------------------------------------------------------------------


class TestScenarioELocalModification(ReconcilerFixture):
    def _modify_then_upgrade(self, rel: str) -> tuple[str, subprocess.CompletedProcess]:
        run_reconciler(self.src, self.rt)
        local = "---\nname: local-edit\npatterns: 'local'\n---\nOPERATOR EDIT\n"
        self.write_runtime(rel, local)
        self.write_source(rel, "---\nname: upstream\npatterns: 'upstream'\n---\nUPSTREAM\n")
        r = run_reconciler(self.src, self.rt)
        return local, r

    def test_locally_modified_mode_survives_an_upstream_change(self):
        local, r = self._modify_then_upgrade(f"mode/{STOCK_MODE}")
        self.assertEqual(self.read_runtime(f"mode/{STOCK_MODE}"), local)
        self.assertIn("conflict", r.stdout)

    def test_locally_modified_persona_survives_an_upstream_change(self):
        local, _ = self._modify_then_upgrade(f"persona/{STOCK_PERSONA}")
        self.assertEqual(self.read_runtime(f"persona/{STOCK_PERSONA}"), local)

    def test_locally_modified_drill_survives_an_upstream_change(self):
        local, _ = self._modify_then_upgrade(f"drills/{STOCK_DRILL}")
        self.assertEqual(self.read_runtime(f"drills/{STOCK_DRILL}"), local)

    def test_conflict_is_reported_with_an_actionable_summary(self):
        _, r = self._modify_then_upgrade(f"mode/{STOCK_MODE}")
        self.assertIn("local modifications", r.stdout)

    def test_manifest_still_records_the_last_installed_hash_after_a_conflict(self):
        # The record must NOT advance to the new upstream hash: doing so
        # would make the next run believe the operator's current bytes
        # are what we installed, and a later reconciliation would then
        # overwrite them.
        run_reconciler(self.src, self.rt)
        installed = self.record(f"mode/{STOCK_MODE}")
        self.write_runtime(f"mode/{STOCK_MODE}", "local\n")
        self.write_source(f"mode/{STOCK_MODE}", "upstream\n")
        run_reconciler(self.src, self.rt)
        self.assertEqual(self.record(f"mode/{STOCK_MODE}"), installed)

    def test_a_later_rerun_keeps_preserving_the_local_edit(self):
        local, _ = self._modify_then_upgrade(f"mode/{STOCK_MODE}")
        run_reconciler(self.src, self.rt)
        run_reconciler(self.src, self.rt)
        self.assertEqual(self.read_runtime(f"mode/{STOCK_MODE}"), local)


# ---------------------------------------------------------------------------
# Scenarios F / G — source deletes, operator deletes
# ---------------------------------------------------------------------------


class TestScenarioFGone(ReconcilerFixture):
    def test_source_deleted_file_keeps_its_runtime_copy(self):
        run_reconciler(self.src, self.rt)
        self.assertTrue((self.rt / f"mode/{STOCK_MODE}").is_file())
        (self.src / f"mode/{STOCK_MODE}").unlink()
        r = run_reconciler(self.src, self.rt)
        self.assertEqual(r.returncode, 0, r.stderr)
        self.assertTrue(
            (self.rt / f"mode/{STOCK_MODE}").is_file(),
            "dropping a stock definition silently is a destructive surprise",
        )

    def test_ownership_is_released_when_source_deletes_a_file(self):
        run_reconciler(self.src, self.rt)
        (self.src / f"mode/{STOCK_MODE}").unlink()
        run_reconciler(self.src, self.rt)
        self.assertIsNone(self.record(f"mode/{STOCK_MODE}"))

    def test_operator_deleted_managed_file_is_not_resurrected(self):
        run_reconciler(self.src, self.rt)
        (self.rt / f"mode/{STOCK_MODE}").unlink()
        r = run_reconciler(self.src, self.rt)
        self.assertFalse((self.rt / f"mode/{STOCK_MODE}").exists())
        self.assertIn("kept-deleted", r.stdout)

    def test_respecting_a_deletion_persists_across_further_runs(self):
        # The deletion must not decay back into CASE A on the next
        # upgrade, which would silently re-activate a mode the operator
        # turned off.
        run_reconciler(self.src, self.rt)
        (self.rt / f"mode/{STOCK_MODE}").unlink()
        run_reconciler(self.src, self.rt)
        run_reconciler(self.src, self.rt)
        run_reconciler(self.src, self.rt)
        self.assertFalse((self.rt / f"mode/{STOCK_MODE}").exists())


# ---------------------------------------------------------------------------
# Custom (non-source) files
# ---------------------------------------------------------------------------


class TestCustomSurvival(ReconcilerFixture):
    def test_custom_mode_survives_reinstall(self):
        run_reconciler(self.src, self.rt)
        custom = "---\nname: mine\npatterns: 'custom'\n---\nMY MODE\n"
        self.write_runtime("mode/house-style.md", custom)
        run_reconciler(self.src, self.rt)
        self.assertEqual(self.read_runtime("mode/house-style.md"), custom)

    def test_custom_persona_survives_reinstall(self):
        run_reconciler(self.src, self.rt)
        custom = "---\nname: reviewer\n---\nMY PERSONA\n"
        self.write_runtime("persona/reviewer.md", custom)
        run_reconciler(self.src, self.rt)
        self.assertEqual(self.read_runtime("persona/reviewer.md"), custom)

    def test_custom_drill_survives_reinstall(self):
        run_reconciler(self.src, self.rt)
        custom = "id: house-drill\nframework: claude-code\nexpect:\n  tools_required: [x]\n"
        self.write_runtime("drills/house-drill.yaml", custom)
        run_reconciler(self.src, self.rt)
        self.assertEqual(self.read_runtime("drills/house-drill.yaml"), custom)

    def test_custom_files_are_never_claimed_in_the_manifest(self):
        run_reconciler(self.src, self.rt)
        self.write_runtime("mode/house-style.md", "x\n")
        run_reconciler(self.src, self.rt)
        self.assertIsNone(self.record("mode/house-style.md"))

    def test_a_custom_file_matching_a_future_stock_name_is_not_preempted(self):
        # A custom file the operator named before upstream shipped the
        # same name must NOT be silently replaced by the stock version.
        run_reconciler(self.src, self.rt)
        self.write_runtime("drills/house-drill.yaml", "id: mine\nframework: x\n")
        self.write_source(
            "drills/house-drill.yaml",
            "id: upstream\nframework: y\nexpect:\n  tools_required: [z]\n",
        )
        run_reconciler(self.src, self.rt)
        self.assertEqual(self.read_runtime("drills/house-drill.yaml"), "id: mine\nframework: x\n")


# ---------------------------------------------------------------------------
# Scenario M — transitional source == runtime
# ---------------------------------------------------------------------------


class TestScenarioMSameFile(ReconcilerFixture):
    def setUp(self) -> None:
        super().setUp()
        # Collapse source and runtime onto one directory, which is exactly
        # the canonical pre-migration layout where ~/.mpm is both.
        self.rt = self.src
        for sub in ("config",):
            shutil.rmtree(self.rt / sub, ignore_errors=True)

    def test_same_file_run_does_not_truncate_or_error(self):
        before = sha256_of(self.rt / f"mode/{STOCK_MODE}")
        r = run_reconciler(self.src, self.rt)
        self.assertEqual(r.returncode, 0, r.stderr)
        self.assertEqual(sha256_of(self.rt / f"mode/{STOCK_MODE}"), before)

    def test_same_file_run_is_idempotent(self):
        before = sha256_of(self.rt / f"persona/{STOCK_PERSONA}")
        run_reconciler(self.src, self.rt)
        run_reconciler(self.src, self.rt)
        self.assertEqual(sha256_of(self.rt / f"persona/{STOCK_PERSONA}"), before)

    def test_same_file_run_adopts_so_later_separation_still_works(self):
        # After adoption, relocating the checkout must not see these as
        # unknown operator files.
        run_reconciler(self.src, self.rt)
        self.assertEqual(
            self.record(f"mode/{STOCK_MODE}"), sha256_of(self.src / f"mode/{STOCK_MODE}")
        )


# ---------------------------------------------------------------------------
# Manifest fail-closed behaviour
# ---------------------------------------------------------------------------


class TestManifestFailsClosed(ReconcilerFixture):
    def _write_manifest(self, text: str) -> None:
        p = self.rt / MANIFEST_RELPATH
        p.parent.mkdir(parents=True, exist_ok=True)
        p.write_text(text)

    def _assert_refused(self) -> subprocess.CompletedProcess:
        r = run_reconciler(self.src, self.rt)
        self.assertEqual(r.returncode, 2, f"expected fail-closed exit 2, got {r.returncode}")
        return r

    def test_malformed_json_is_refused(self):
        self._write_manifest("{not json")
        self._assert_refused()

    def test_empty_manifest_is_refused(self):
        # A zero-length manifest is what a pre-atomic-write interruption
        # leaves behind. Treating it as fresh would authorize overwrites.
        self._write_manifest("")
        self._assert_refused()

    def test_unknown_version_is_refused(self):
        self._write_manifest(json.dumps({"version": 99, "assets": {}}))
        self._assert_refused()

    def test_missing_version_is_refused(self):
        self._write_manifest(json.dumps({"assets": {}}))
        self._assert_refused()

    def test_non_object_manifest_is_refused(self):
        self._write_manifest('["not", "an", "object"]')
        self._assert_refused()

    def test_malformed_entry_is_refused(self):
        self._write_manifest(json.dumps({"version": 1, "assets": {"mode/x.md": "nope"}}))
        self._assert_refused()

    def test_absolute_path_entry_is_refused(self):
        # A path-traversal entry must never steer reconciliation outside
        # the runtime root.
        self._write_manifest(
            json.dumps({"version": 1, "assets": {"/etc/passwd": {"sha256": "x"}}})
        )
        self._assert_refused()

    def test_parent_traversal_entry_is_refused(self):
        self._write_manifest(
            json.dumps({"version": 1, "assets": {"../../escape.md": {"sha256": "x"}}})
        )
        self._assert_refused()

    def test_refusal_does_not_modify_existing_definitions(self):
        run_reconciler(self.src, self.rt)
        self.write_runtime(f"mode/{STOCK_MODE}", "operator content\n")
        self._write_manifest("{corrupt")
        r = run_reconciler(self.src, self.rt)
        self.assertEqual(self.read_runtime(f"mode/{STOCK_MODE}"), "operator content\n")
        # Preservation alone is not sufficient evidence of a fail-closed
        # refusal: an installer that silently discards the manifest falls
        # through to Case C and leaves the operator's file alone anyway.
        # What must be observable is that the run REFUSED — a non-zero
        # exit and an explicit reason — so the operator learns the manifest
        # needs attention instead of believing the install is reconciled.
        self.assertEqual(r.returncode, 2, f"expected a fail-closed refusal, got {r.returncode}")
        self.assertIn("refus", r.stderr.lower(), "refusal was not explained on stderr")

    def test_manifest_records_no_absolute_source_paths(self):
        run_reconciler(self.src, self.rt)
        raw = (self.rt / MANIFEST_RELPATH).read_text()
        self.assertNotIn(str(self.src), raw)
        self.assertNotIn(str(self.base), raw)

    def test_manifest_records_no_file_contents(self):
        secret = "OPERATOR-SECRET-CONTENT-MARKER"
        run_reconciler(self.src, self.rt)
        self.write_runtime("mode/custom.md", secret)
        run_reconciler(self.src, self.rt)
        self.assertNotIn(secret, (self.rt / MANIFEST_RELPATH).read_text())


# ---------------------------------------------------------------------------
# Dry run truthfulness
# ---------------------------------------------------------------------------


class TestDryRun(ReconcilerFixture):
    def test_dry_run_writes_nothing(self):
        r = run_reconciler(self.src, self.rt, "--dry-run")
        self.assertEqual(r.returncode, 0, r.stderr)
        self.assertFalse(self.rt.exists())

    def test_dry_run_reports_the_intended_operations(self):
        r = run_reconciler(self.src, self.rt, "--dry-run")
        self.assertIn("DRY RUN", r.stdout)
        self.assertIn(f"mode/{STOCK_MODE}", r.stdout)

    def test_dry_run_on_an_existing_install_reports_no_drift(self):
        run_reconciler(self.src, self.rt)
        r = run_reconciler(self.src, self.rt, "--dry-run")
        self.assertIn("unchanged", r.stdout)
        self.assertNotIn("provisioned", r.stdout)
        self.assertNotIn("refreshed", r.stdout)

    def test_dry_run_does_not_advance_the_manifest(self):
        run_reconciler(self.src, self.rt)
        before = (self.rt / MANIFEST_RELPATH).read_text()
        self.write_source(f"mode/{STOCK_MODE}", "changed\n")
        run_reconciler(self.src, self.rt, "--dry-run")
        self.assertEqual((self.rt / MANIFEST_RELPATH).read_text(), before)


# ---------------------------------------------------------------------------
# Install-surface wiring (semantic, not grep-only)
# ---------------------------------------------------------------------------


class TestInstallSurfaceWiring(unittest.TestCase):
    def test_make_install_invokes_the_canonical_reconciler(self):
        makefile = (REPO_ROOT / "Makefile").read_text()
        # The reconciler is named once and invoked through the variable,
        # so the path cannot drift between the declaration and the call.
        self.assertIn("RUNTIME_ASSETS_SCRIPT := scripts/install_runtime_assets.py", makefile)
        self.assertIn("python3 $(RUNTIME_ASSETS_SCRIPT)", makefile)

    def test_make_install_depends_on_the_reconciler_target(self):
        makefile = (REPO_ROOT / "Makefile").read_text()
        install_line = next(
            ln for ln in makefile.splitlines() if ln.startswith("install:") and "build" in ln
        )
        self.assertIn("install-runtime-assets", install_line)

    def test_install_sh_invokes_the_same_canonical_reconciler(self):
        install_sh = (REPO_ROOT / "install.sh").read_text()
        self.assertIn("scripts/install_runtime_assets.py", install_sh)

    def test_make_build_does_not_reconcile_runtime_assets(self):
        """A build is not a deployment. This is the load-bearing invariant."""
        makefile = (REPO_ROOT / "Makefile").read_text()
        body = makefile.split("build: assert-build-dir-safe", 1)[1]
        build_recipe = body.split("\n\n", 1)[0]
        self.assertNotIn("install-runtime-assets", build_recipe)
        self.assertNotIn("install_runtime_assets", build_recipe)

    def test_test_and_release_gate_targets_do_not_reconcile(self):
        makefile = (REPO_ROOT / "Makefile").read_text()
        for target in ("\ntest:", "\ntest-race:", "\nrelease-gate:", "\ntest-scripts:"):
            self.assertIn(target, makefile)
            section = makefile.split(target, 1)[1].split("\n\n", 1)[0]
            self.assertNotIn("install-runtime-assets", section, f"{target.strip()} deploys")


# ---------------------------------------------------------------------------
# Project .mcp.json wiring
# ---------------------------------------------------------------------------


class TestProjectMCPConfig(unittest.TestCase):
    def setUp(self) -> None:
        self.cfg = json.loads((REPO_ROOT / ".mcp.json").read_text())
        self.server = self.cfg["mcpServers"]["mpm"]

    def test_workspace_is_not_the_source_checkout(self):
        # Regression guard: "." makes the source tree a runtime
        # workspace. mpmcli.ResolveWorkspace() then bootstrap-creates
        # mode/ and persona/ inside the checkout and resolves a ghost DB
        # there, distinct from the canonical one.
        self.assertNotEqual(self.server["env"]["MPM_WORKSPACE"], ".")

    def test_workspace_is_the_canonical_runtime_root(self):
        self.assertEqual(self.server["env"]["MPM_WORKSPACE"], "${HOME}/.mpm")

    def test_workspace_uses_variable_expansion_not_a_literal_tilde(self):
        # Verified empirically against the host: MCP hosts expand ${VAR}
        # in the env block but pass a literal `~` through unexpanded, and
        # a literal "~/.mpm" is a relative path.
        ws = self.server["env"]["MPM_WORKSPACE"]
        self.assertFalse(ws.startswith("~"), "literal ~ is not expanded by MCP hosts")
        self.assertTrue(ws.startswith("${HOME}"))

    def test_command_still_targets_the_developer_artifact(self):
        self.assertEqual(self.server["command"], "./.build/bin/mpm-mcp")

    def test_command_does_not_point_into_the_installed_prefix(self):
        self.assertNotIn("/.mpm/bin", self.server["command"])


# ---------------------------------------------------------------------------
# Runtime templates — the units that must keep pointing at the runtime root
# ---------------------------------------------------------------------------
#
# These parse the unit files as unit files and resolve the paths they
# declare, rather than grepping for strings. A grep would be satisfied by a
# mention of ".build" in a comment; the property that actually matters is
# that no DIRECTIVE the service executes resolves outside %h/.mpm into a
# source checkout.


RUNTIME_ROOT = "%h/.mpm"
# Directives whose values name a filesystem location the service acts on.
PATH_DIRECTIVES = (
    "WorkingDirectory",
    "ExecStart",
    "ReadWritePaths",
    "RequiresMountsFor",
)


def unit_directive_values(path: Path, key: str) -> list[str]:
    """All values for a systemd directive, honouring repeated lines.

    systemd allows `Environment=` to appear many times in a [Service]
    section. ConfigParser collapses duplicates (last one wins), which would
    silently hide every directive but the final one — so read the raw
    section instead.
    """
    values: list[str] = []
    section = ""
    for raw in path.read_text().splitlines():
        line = raw.strip()
        if line.startswith("[") and line.endswith("]"):
            section = line[1:-1]
            continue
        if section != "Service":
            continue
        k, _, v = line.partition("=")
        if k.strip() == key:
            values.append(v.strip())
    return values


class TestRuntimeTemplates(unittest.TestCase):
    def units(self):
        d = REPO_ROOT / "contrib" / "systemd"
        return sorted(d.glob("*.service.user"))

    def test_units_exist(self):
        self.assertTrue(self.units(), "no systemd user unit templates found")

    def test_every_executed_path_resolves_inside_the_runtime_root(self):
        for unit in self.units():
            for directive in PATH_DIRECTIVES:
                for value in unit_directive_values(unit, directive):
                    for raw in value.split():
                        if not raw.startswith("%h/"):
                            continue
                        self.assertTrue(
                            raw.startswith(RUNTIME_ROOT + "/") or raw == RUNTIME_ROOT,
                            f"{unit.name}: {directive}={raw} escapes the runtime root "
                            f"{RUNTIME_ROOT}; a source checkout must never be reachable here",
                        )

    def test_no_developer_build_path_reaches_a_unit(self):
        for unit in self.units():
            for value in unit_directive_values(unit, "Environment"):
                self.assertNotIn(
                    ".build",
                    value,
                    f"{unit.name}: Environment={value} references the developer build "
                    "directory, which is not a runtime location",
                )
            self.assertNotIn(
                ".build/bin",
                unit_directive_values(unit, "ExecStart")[0] if
                unit_directive_values(unit, "ExecStart") else "",
                f"{unit.name}: ExecStart runs a developer artifact",
            )

    def test_workspace_environment_points_at_the_runtime_root(self):
        for unit in self.units():
            envs = [
                e
                for value in unit_directive_values(unit, "Environment")
                for e in value.split()
            ]
            self.assertTrue(
                any(e == f"MPM_WORKSPACE={RUNTIME_ROOT}" for e in envs),
                f"{unit.name}: MPM_WORKSPACE is not pinned to {RUNTIME_ROOT}; got {envs}",
            )

    def test_service_binaries_are_installed_not_built(self):
        for unit in self.units():
            execs = unit_directive_values(unit, "ExecStart")
            self.assertTrue(execs, f"{unit.name}: no ExecStart")
            for exec_line in execs:
                exe = exec_line.split()[0]
                self.assertTrue(
                    exe.startswith(RUNTIME_ROOT + "/bin/"),
                    f"{unit.name}: ExecStart runs {exe}, which is not an installed "
                    f"binary under {RUNTIME_ROOT}/bin/",
                )

    # ── Subprocess binary pinning (scheduler PATH ambiguity) ──────────────
    #
    # The scheduler execs `mpm` for gc_run and broadcast wakes. Left as a
    # bare name, exec resolves it through PATH — and a systemd user unit's
    # PATH is the manager's, not the operator's shell's. The unit therefore
    # has to say which binary to run, the same way it already says which
    # workspace and which critic binary to use.
    #
    # These assert on the unit as a unit, so a value dropped from the
    # template fails here rather than at the first wake on a live host.

    SUBPROCESS_BIN_ENV = ("MPM_BIN", "MPM_CRITIC_BIN")

    def unit_env(self, unit):
        """Every Environment assignment in the unit, split into key/value."""
        env = {}
        for value in unit_directive_values(unit, "Environment"):
            for assignment in value.split():
                key, _, val = assignment.partition("=")
                if key:
                    env[key] = val
        return env

    def scheduler_units(self):
        """Units whose ExecStart runs the scheduler.

        Subprocess binary pinning only applies here. mpm-telemetry
        execs nothing, so demanding MPM_BIN of it would be asserting a
        property it has no reason to have.
        """
        return [
            u
            for u in self.units()
            if any(
                "mpm-scheduler" in line
                for line in unit_directive_values(u, "ExecStart")
            )
        ]

    def test_scheduler_units_exist(self):
        self.assertTrue(
            self.scheduler_units(),
            "no unit runs mpm-scheduler; the subprocess-pinning guards below "
            "would pass vacuously",
        )

    def test_scheduler_binary_env_vars_are_pinned_to_installed_binaries(self):
        for unit in self.scheduler_units():
            env = self.unit_env(unit)
            for key in self.SUBPROCESS_BIN_ENV:
                self.assertIn(
                    key,
                    env,
                    f"{unit.name}: {key} is not set. The scheduler would exec a "
                    "bare name and let the unit's PATH choose the binary, so a "
                    "stale or hostile earlier entry silently serves the wakes.",
                )
                self.assertEqual(
                    env[key],
                    f"{RUNTIME_ROOT}/bin/{'mpm' if key == 'MPM_BIN' else 'mpm-critic'}",
                    f"{unit.name}: {key}={env[key]} is not the installed binary",
                )

    def test_no_subprocess_binary_env_var_is_a_bare_name(self):
        # A bare name is the defect itself: PATH gets the vote.
        for unit in self.units():
            env = self.unit_env(unit)
            for key in self.SUBPROCESS_BIN_ENV:
                if key not in env:
                    continue
                self.assertIn(
                    "/",
                    env[key],
                    f"{unit.name}: {key}={env[key]} is a bare name; PATH would "
                    "decide which binary runs",
                )

    def test_no_directive_reaches_into_a_source_checkout(self):
        # §13 prerequisite: after the checkout moves to ~/src/mpm, nothing
        # in a unit may still resolve into wherever the source used to be.
        for unit in self.units():
            for directive in PATH_DIRECTIVES + ("Environment",):
                for value in unit_directive_values(unit, directive):
                    for raw in value.split():
                        if "=" in raw:
                            raw = raw.split("=", 1)[1]
                        for marker in (".build", "/src/mpm", "~/src"):
                            self.assertNotIn(
                                marker,
                                raw,
                                f"{unit.name}: {directive}={value} references {marker}; "
                                "a source checkout must not be a runtime dependency",
                            )


if __name__ == "__main__":
    unittest.main()