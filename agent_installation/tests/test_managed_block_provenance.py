"""Contract tests for build provenance in installed MPM managed blocks.

An installed managed behavioural block must identify:

  * the MPM product version that wrote it;
  * the exact source commit it was built from;
  * the behavioural protocol version of the contract itself (a version
    INDEPENDENT of the product version);
  * the UTC moment the block was last applied or updated.

And it must do so idempotently: running the installer again with the
same version, commit and instructions must be a genuine no-op — the
same bytes, the same timestamp, no new backup.

These tests drive the REAL per-host installers against the REAL
checked-in snippets, in a temporary HOME, with the build provenance
supplied through the environment exactly as install.sh supplies it.
Nothing here touches the live installation, the live agent
configuration or the live database.
"""

from __future__ import annotations

import os
import re
import shutil
import subprocess
import sys
import tempfile
import unittest
from pathlib import Path

AGENT_INSTALLATION = Path(__file__).resolve().parent.parent
REPO_ROOT = AGENT_INSTALLATION.parent
SCRIPTS = AGENT_INSTALLATION / "scripts"

if str(SCRIPTS) not in sys.path:
    sys.path.insert(0, str(SCRIPTS))

import block_provenance  # noqa: E402


# The build provenance the tests install with. Distinctive values, so a
# test can tell an installed stamp from a placeholder.
TEST_VERSION = "9.9.9-provenance-test"
TEST_COMMIT = "abcdef1234567890abcdef1234567890abcdef12"

# A sentinel "applied" timestamp. The provenance timestamp has
# second-level resolution, so a test cannot tell "refreshed" from "not
# refreshed" by running twice inside one second. Writing a known old
# timestamp into the installed block first makes the refresh
# observable without depending on the clock.
OLD_APPLIED = "2000-01-01T00:00:00Z"


def _adapter_argv(adapter: str, script: Path, target: Path, snippet: Path, home: Path) -> list[str]:
    """The command line for one adapter's installer.

    Each adapter has its own installer with its own arguments; this
    keeps the per-adapter difference in one place.
    """
    if adapter == "mpm-claude-code":
        return [str(script), "--scope", "user", "--home", str(home),
                "--target", str(target), "--snippet", str(snippet)]
    if adapter == "mpm-opencode":
        return [str(script), "--scope", "user", "--target", str(target),
                "--snippet", str(snippet)]
    if adapter == "mpm-pi":
        return [str(script), "--scope", "user", "--target", str(target),
                "--snippet", str(snippet)]
    if adapter == "mpm-hermes":
        return [str(script), "--target", str(target), "--snippet", str(snippet)]
    if adapter == "mpm-memory-openclaw":
        return [str(script), "--home", str(home), "--target", str(target),
                "--snippet", str(snippet)]
    raise AssertionError(f"unknown adapter {adapter!r}")


# The persistent-file hosts, and the snippet each one installs.
ADAPTERS = [
    ("mpm-claude-code", "install_claude_instructions", "CLAUDE.md.snippet"),
    ("mpm-opencode", "install_agents_instructions", "AGENTS.md.snippet"),
    ("mpm-pi", "install_agents_instructions", "AGENTS.md.snippet"),
    ("mpm-hermes", "install_hermes_instructions", "hermes.md.snippet"),
    ("mpm-memory-openclaw", "install_openclaw_instructions", "SOUL.md.snippet"),
]


def run_installer(adapter: str, installer: str, target: Path, snippet: Path,
                  home: Path, env: dict | None = None, cwd: Path | None = None) -> subprocess.CompletedProcess:
    """Run one adapter's installer against `target`."""
    script = AGENT_INSTALLATION / adapter / "scripts" / f"{installer}.py"
    argv = _adapter_argv(adapter, script, target, snippet, home)
    environment = dict(os.environ)
    environment.update(env or {})
    return subprocess.run(
        [sys.executable, *argv],
        stdout=subprocess.PIPE,
        stderr=subprocess.PIPE,
        text=True,
        env=environment,
        cwd=str(cwd) if cwd else None,
        check=False,
    )


def provenance_of(text: str) -> dict[str, str]:
    """The parsed provenance comment, or an empty dict when absent."""
    return block_provenance.parse_provenance(text) or {}


def set_applied_timestamp(path: Path, value: str) -> None:
    """Rewrite the installed block's `applied` timestamp."""
    text = path.read_text(encoding="utf-8")
    patched = re.sub(
        r"(<!-- mpm-provenance:[^\n]*?applied=)[^\s]+",
        rf"\g<1>{value}",
        text,
    )
    path.write_text(patched, encoding="utf-8")


class ProvenanceTestCase(unittest.TestCase):
    """Shared fixtures: a temporary HOME and the canonical snippet."""

    def setUp(self) -> None:
        self.tmp = Path(tempfile.mkdtemp(prefix="mpm-block-provenance-"))
        self.addCleanup(lambda: shutil.rmtree(self.tmp, ignore_errors=True))
        self.home = self.tmp / "home"
        self.home.mkdir(parents=True, exist_ok=True)
        self.env = {
            "MPM_BUILD_VERSION": TEST_VERSION,
            "MPM_BUILD_COMMIT": TEST_COMMIT,
        }

    def snippet_for(self, adapter: str, name: str) -> Path:
        return AGENT_INSTALLATION / adapter / "templates" / name

    def target_for(self, adapter: str) -> Path:
        suffix = ".hermes.md" if adapter == "mpm-hermes" else ".md"
        return self.tmp / f"{adapter}{suffix}"


class TestFreshInstall(ProvenanceTestCase):
    """Case 1 — a fresh managed-block installation carries the metadata."""

    def test_fresh_install_records_the_provenance(self) -> None:
        for adapter, installer, snippet_name in ADAPTERS:
            with self.subTest(adapter=adapter):
                target = self.target_for(adapter)
                result = run_installer(
                    adapter, installer, target,
                    self.snippet_for(adapter, snippet_name), self.home, self.env,
                )
                self.assertEqual(result.returncode, 0,
                                 f"{adapter} installer failed: {result.stderr}")

                text = target.read_text(encoding="utf-8")
                fields = provenance_of(text)
                self.assertTrue(fields, f"{adapter} installed no provenance comment")
                self.assertEqual(fields.get("version"), TEST_VERSION)
                self.assertEqual(fields.get("commit"), TEST_COMMIT)
                self.assertEqual(fields.get("protocol"), block_provenance.read_protocol_version())

    def test_the_provenance_sits_immediately_after_the_inner_marker(self) -> None:
        for adapter, installer, snippet_name in ADAPTERS:
            with self.subTest(adapter=adapter):
                target = self.target_for(adapter)
                run_installer(
                    adapter, installer, target,
                    self.snippet_for(adapter, snippet_name), self.home, self.env,
                )
                text = target.read_text(encoding="utf-8")
                lines = text.splitlines()
                for index, line in enumerate(lines):
                    if line.strip() == block_provenance.INNER_BEGIN:
                        self.assertTrue(
                            lines[index + 1].startswith(block_provenance.PROVENANCE_PREFIX),
                            f"{adapter}: the provenance comment is not immediately "
                            f"after the managed-block marker:\n{text}",
                        )
                        break
                else:
                    self.fail(f"{adapter}: no managed-block marker in the installed file")

    def test_the_timestamp_is_utc(self) -> None:
        target = self.target_for("mpm-claude-code")
        run_installer(
            "mpm-claude-code", "install_claude_instructions", target,
            self.snippet_for("mpm-claude-code", "CLAUDE.md.snippet"), self.home, self.env,
        )
        applied = provenance_of(target.read_text(encoding="utf-8")).get("applied", "")
        self.assertRegex(applied, r"^\d{4}-\d{2}-\d{2}T\d{2}:\d{2}:\d{2}Z$",
                         "the applied timestamp must be an ISO-8601 UTC instant")


class TestLegacyUpgrade(ProvenanceTestCase):
    """Case 2 — an unstamped legacy block is upgraded safely."""

    def test_an_unstamped_block_is_stamped_on_the_next_reconciliation(self) -> None:
        for adapter, installer, snippet_name in ADAPTERS:
            with self.subTest(adapter=adapter):
                target = self.target_for(adapter)
                snippet = self.snippet_for(adapter, snippet_name)

                # A legacy installation: the snippet's own text, with no
                # provenance comment anywhere.
                target.write_text(snippet.read_text(encoding="utf-8"), encoding="utf-8")
                self.assertEqual(provenance_of(target.read_text(encoding="utf-8")), {},
                                 "the legacy fixture must not carry a provenance comment")

                result = run_installer(adapter, installer, target, snippet, self.home, self.env)
                self.assertEqual(result.returncode, 0, result.stderr)

                fields = provenance_of(target.read_text(encoding="utf-8"))
                self.assertTrue(fields, f"{adapter} did not upgrade the unstamped block")
                self.assertEqual(fields.get("version"), TEST_VERSION)

    def test_the_behavioural_text_is_not_changed_by_the_upgrade(self) -> None:
        adapter, installer, snippet_name = "mpm-claude-code", "install_claude_instructions", "CLAUDE.md.snippet"
        target = self.target_for(adapter)
        snippet = self.snippet_for(adapter, snippet_name)

        target.write_text(snippet.read_text(encoding="utf-8"), encoding="utf-8")
        run_installer(adapter, installer, target, snippet, self.home, self.env)

        installed = block_provenance.strip_provenance(target.read_text(encoding="utf-8"))
        canonical = block_provenance.strip_provenance(snippet.read_text(encoding="utf-8"))
        self.assertIn(canonical.strip(), installed,
                      "the upgrade must not alter the canonical behavioural text")


class TestVersionAndCommitChange(ProvenanceTestCase):
    """Case 3 — a new product version or source commit refreshes the metadata."""

    def test_a_new_product_version_refreshes_the_metadata(self) -> None:
        adapter, installer, snippet_name = "mpm-claude-code", "install_claude_instructions", "CLAUDE.md.snippet"
        target = self.target_for(adapter)
        snippet = self.snippet_for(adapter, snippet_name)

        run_installer(adapter, installer, target, snippet, self.home, self.env)
        first = provenance_of(target.read_text(encoding="utf-8"))
        self.assertEqual(first.get("version"), TEST_VERSION)
        set_applied_timestamp(target, OLD_APPLIED)

        second_env = dict(self.env)
        second_env["MPM_BUILD_VERSION"] = "9.9.10-provenance-test"
        run_installer(adapter, installer, target, snippet, self.home, second_env)

        second = provenance_of(target.read_text(encoding="utf-8"))
        self.assertEqual(second.get("version"), "9.9.10-provenance-test")
        self.assertNotEqual(
            second.get("applied"), OLD_APPLIED,
            "a changed product version must refresh the applied timestamp",
        )

    def test_a_new_source_commit_refreshes_the_metadata(self) -> None:
        adapter, installer, snippet_name = "mpm-claude-code", "install_claude_instructions", "CLAUDE.md.snippet"
        target = self.target_for(adapter)
        snippet = self.snippet_for(adapter, snippet_name)

        run_installer(adapter, installer, target, snippet, self.home, self.env)
        set_applied_timestamp(target, OLD_APPLIED)

        second_env = dict(self.env)
        second_env["MPM_BUILD_COMMIT"] = "0123456789abcdef0123456789abcdef01234567"
        run_installer(adapter, installer, target, snippet, self.home, second_env)

        second = provenance_of(target.read_text(encoding="utf-8"))
        self.assertEqual(second.get("commit"), "0123456789abcdef0123456789abcdef01234567")
        self.assertNotEqual(
            second.get("applied"), OLD_APPLIED,
            "a changed source commit must refresh the applied timestamp",
        )

    def test_changed_behavioural_text_refreshes_the_metadata(self) -> None:
        """A changed behavioural block refreshes the timestamp too."""
        adapter, installer, snippet_name = "mpm-claude-code", "install_claude_instructions", "CLAUDE.md.snippet"
        target = self.target_for(adapter)
        snippet = self.snippet_for(adapter, snippet_name)

        run_installer(adapter, installer, target, snippet, self.home, self.env)
        set_applied_timestamp(target, OLD_APPLIED)

        # A copy of the snippet whose behavioural text differs.
        changed = self.tmp / "CLAUDE.changed.snippet"
        changed.write_text(
            snippet.read_text(encoding="utf-8") + "\nan extra behavioural line.\n",
            encoding="utf-8",
        )
        run_installer(adapter, installer, target, changed, self.home, self.env)

        second = provenance_of(target.read_text(encoding="utf-8"))
        self.assertNotEqual(
            second.get("applied"), OLD_APPLIED,
            "a changed behavioural block must refresh the applied timestamp",
        )
        self.assertIn("an extra behavioural line", target.read_text(encoding="utf-8"))


class TestProtocolVersionChange(ProvenanceTestCase):
    """Case 4 — a changed behavioural protocol version refreshes the metadata."""

    def _shadow_tree(self, protocol_version: str) -> Path:
        """A copy of the agent_installation tree with a different protocol version.

        The behavioural protocol version is declared by the canonical
        source and read from it at install time, so exercising a change
        means changing that declaration. The copy keeps the real
        installer code in the loop.
        """
        shadow = self.tmp / f"shadow-{protocol_version}"
        shadow.mkdir(parents=True, exist_ok=True)
        shutil.copytree(AGENT_INSTALLATION / "scripts", shadow / "scripts")
        canonical = AGENT_INSTALLATION / "MPM_AGENT_INTEGRATION_SNIPPETS.md"
        text = canonical.read_text(encoding="utf-8")
        text = re.sub(
            r"(<!--\s*mpm_agent_integration_version:\s*)[0-9][^\s]*(\s*-->)",
            rf"\g<1>{protocol_version}\g<2>",
            text,
            count=1,
        )
        (shadow / "MPM_AGENT_INTEGRATION_SNIPPETS.md").write_text(text, encoding="utf-8")
        for adapter, installer, _ in ADAPTERS:
            source = AGENT_INSTALLATION / adapter / "scripts"
            destination = shadow / adapter / "scripts"
            shutil.copytree(source, destination, dirs_exist_ok=True)
        return shadow

    def test_a_changed_protocol_version_refreshes_the_metadata(self) -> None:
        adapter, installer, _ = "mpm-claude-code", "install_claude_instructions", None
        target = self.target_for(adapter)

        shadow_a = self._shadow_tree("2.1.0")
        script_a = shadow_a / adapter / "scripts" / f"{installer}.py"
        snippet_a = self.snippet_for(adapter, "CLAUDE.md.snippet")
        argv_a = _adapter_argv(adapter, script_a, target, snippet_a, self.home)
        env = dict(os.environ)
        env.update(self.env)
        subprocess.run([sys.executable, *argv_a], check=True, env=env,
                       stdout=subprocess.PIPE, stderr=subprocess.PIPE, text=True)
        first = provenance_of(target.read_text(encoding="utf-8"))
        self.assertEqual(first.get("protocol"), "2.1.0")
        set_applied_timestamp(target, OLD_APPLIED)

        shadow_b = self._shadow_tree("2.2.0")
        script_b = shadow_b / adapter / "scripts" / f"{installer}.py"
        argv_b = _adapter_argv(adapter, script_b, target, snippet_a, self.home)
        subprocess.run([sys.executable, *argv_b], check=True, env=env,
                       stdout=subprocess.PIPE, stderr=subprocess.PIPE, text=True)
        second = provenance_of(target.read_text(encoding="utf-8"))

        self.assertEqual(second.get("protocol"), "2.2.0")
        self.assertNotEqual(
            second.get("applied"), OLD_APPLIED,
            "a changed behavioural protocol version must refresh the applied timestamp",
        )

    def test_the_protocol_version_is_independent_of_the_product_version(self) -> None:
        protocol = block_provenance.read_protocol_version()
        self.assertTrue(re.match(r"^\d+\.\d+\.\d+", protocol),
                        f"the behavioural protocol version must be a version, got {protocol!r}")
        self.assertNotEqual(
            protocol, block_provenance.source_version(),
            "the behavioural protocol version must not be the product version",
        )


class TestIdempotency(ProvenanceTestCase):
    """Case 5 — reconciling again with the same source is a genuine no-op."""

    def test_a_second_reconciliation_changes_nothing(self) -> None:
        for adapter, installer, snippet_name in ADAPTERS:
            with self.subTest(adapter=adapter):
                target = self.target_for(adapter)
                snippet = self.snippet_for(adapter, snippet_name)

                run_installer(adapter, installer, target, snippet, self.home, self.env)
                first_bytes = target.read_bytes()
                first_fields = provenance_of(first_bytes.decode("utf-8"))

                result = run_installer(adapter, installer, target, snippet, self.home, self.env)
                self.assertEqual(result.returncode, 0, result.stderr)

                second_bytes = target.read_bytes()
                second_fields = provenance_of(second_bytes.decode("utf-8"))

                self.assertEqual(first_bytes, second_bytes,
                                 f"{adapter}: a repeated reconciliation changed the file")
                self.assertEqual(first_fields.get("applied"), second_fields.get("applied"),
                                 f"{adapter}: a repeated reconciliation moved the timestamp")

    def test_a_repeated_reconciliation_takes_no_new_backup(self) -> None:
        adapter, installer, snippet_name = "mpm-claude-code", "install_claude_instructions", "CLAUDE.md.snippet"
        target = self.target_for(adapter)
        snippet = self.snippet_for(adapter, snippet_name)

        run_installer(adapter, installer, target, snippet, self.home, self.env)
        backups_after_first = sorted(p.name for p in target.parent.glob(target.name + ".bak*"))

        run_installer(adapter, installer, target, snippet, self.home, self.env)
        backups_after_second = sorted(p.name for p in target.parent.glob(target.name + ".bak*"))

        self.assertEqual(backups_after_first, backups_after_second,
                         "a no-op reconciliation must not take a new backup")


class TestUserContentPreserved(ProvenanceTestCase):
    """Case 6 — user content outside the markers survives."""

    def test_user_content_around_the_managed_block_is_preserved(self) -> None:
        for adapter, installer, snippet_name in ADAPTERS:
            with self.subTest(adapter=adapter):
                target = self.target_for(adapter)
                snippet = self.snippet_for(adapter, snippet_name)

                before = "# My own notes\n\nKeep this.\n"
                after = "\n## My own section\n\nKeep this too.\n"
                target.write_text(before + snippet.read_text(encoding="utf-8") + after,
                                  encoding="utf-8")

                run_installer(adapter, installer, target, snippet, self.home, self.env)

                text = target.read_text(encoding="utf-8")
                self.assertIn(before.strip(), text)
                self.assertIn(after.strip(), text)


class TestCrossAdapterConsistency(ProvenanceTestCase):
    """Case 7 — every supported host writes the same metadata the same way."""

    def test_every_adapter_records_the_same_metadata(self) -> None:
        seen = []
        for adapter, installer, snippet_name in ADAPTERS:
            target = self.target_for(adapter)
            run_installer(
                adapter, installer, target,
                self.snippet_for(adapter, snippet_name), self.home, self.env,
            )
            fields = provenance_of(target.read_text(encoding="utf-8"))
            self.assertTrue(fields, f"{adapter} recorded no provenance")
            seen.append((adapter, fields.get("version"), fields.get("commit"),
                         fields.get("protocol")))

        self.assertEqual(len({entry[1:] for entry in seen}), 1,
                         f"the adapters disagree about the metadata: {seen}")

    def test_every_adapter_uses_the_same_comment_shape(self) -> None:
        for adapter, installer, snippet_name in ADAPTERS:
            target = self.target_for(adapter)
            run_installer(
                adapter, installer, target,
                self.snippet_for(adapter, snippet_name), self.home, self.env,
            )
            text = target.read_text(encoding="utf-8")
            comments = [
                line for line in text.splitlines()
                if line.startswith(block_provenance.PROVENANCE_PREFIX)
            ]
            self.assertEqual(len(comments), 1,
                             f"{adapter} wrote {len(comments)} provenance comments")
            for key in ("version", "commit", "protocol", "applied"):
                self.assertIn(f"{key}=", comments[0],
                              f"{adapter}: the provenance comment has no {key} field")


class TestUnknownFallback(ProvenanceTestCase):
    """Case 8 — "unknown" is a statement of absence, never a claim.

    An adapter installer can be run on its own — outside install.sh,
    outside a build — and then it can establish neither the product
    version nor the source commit. The provenance must then say
    "unknown". What it must NOT do is carry a value it cannot
    support: not an invented version, and not a stale one presented
    as though this run had verified it.

    The two halves of the contract are not the same rule, and the
    difference is the whole point:

      - When the behavioural text is UNCHANGED, the block is not being
        rewritten. The version an earlier, informed run established is
        still true of these bytes, so it is kept. Erasing it would
        destroy information for no gain.

      - When the behavioural text HAS changed, this run is the one
        writing the block, and it cannot say what wrote the block it
        is replacing. Carrying the old value forward would be a claim
        about bytes that no longer exist — the kind of quiet false
        attribution that provenance exists to prevent. An honest
        "unknown" beats a stale claim.
    """

    # An environment in which the build provenance cannot be
    # established: the variables are present but empty, exactly as a
    # bare `install.sh` run outside a build would leave them.
    NO_BUILD_ENV = {"MPM_BUILD_VERSION": "", "MPM_BUILD_COMMIT": ""}

    def test_a_run_that_cannot_establish_the_version_says_unknown(self) -> None:
        adapter, installer, snippet_name = "mpm-claude-code", "install_claude_instructions", "CLAUDE.md.snippet"
        target = self.target_for(adapter)
        snippet = self.snippet_for(adapter, snippet_name)

        run_installer(adapter, installer, target, snippet, self.home, self.NO_BUILD_ENV)

        fields = provenance_of(target.read_text(encoding="utf-8"))
        self.assertEqual(
            fields.get("version"), block_provenance.UNKNOWN,
            "a run that cannot establish the product version must say so; "
            "claiming one it cannot support is a false provenance claim",
        )
        self.assertEqual(
            fields.get("commit"), block_provenance.UNKNOWN,
            "a run that cannot establish the source commit must say so",
        )
        # The behavioural protocol version is declared by the canonical
        # source, not by the build, so it is establishable either way.
        self.assertEqual(
            fields.get("protocol"), block_provenance.read_protocol_version(),
            "the behavioural protocol version does not depend on the build "
            "and must remain establishable",
        )

    def test_a_no_op_run_keeps_the_version_an_earlier_run_established(self) -> None:
        adapter, installer, snippet_name = "mpm-claude-code", "install_claude_instructions", "CLAUDE.md.snippet"
        target = self.target_for(adapter)
        snippet = self.snippet_for(adapter, snippet_name)

        run_installer(adapter, installer, target, snippet, self.home, self.env)
        set_applied_timestamp(target, OLD_APPLIED)

        # A second run that cannot establish the version, against
        # UNCHANGED behavioural text.
        run_installer(adapter, installer, target, snippet, self.home, self.NO_BUILD_ENV)

        fields = provenance_of(target.read_text(encoding="utf-8"))
        self.assertEqual(
            fields.get("version"), TEST_VERSION,
            "an unestablished value must not erase a version an earlier "
            "informed run established; the block's bytes are unchanged",
        )
        self.assertEqual(
            fields.get("applied"), OLD_APPLIED,
            "a reconciliation that changes nothing must preserve the "
            "existing timestamp",
        )

    def test_a_changed_block_does_not_keep_a_stale_version_claim(self) -> None:
        adapter, installer, snippet_name = "mpm-claude-code", "install_claude_instructions", "CLAUDE.md.snippet"
        target = self.target_for(adapter)
        snippet = self.snippet_for(adapter, snippet_name)

        run_installer(adapter, installer, target, snippet, self.home, self.env)

        # A copy of the snippet whose behavioural text differs, written
        # by a run that cannot establish the version.
        changed = self.tmp / "CLAUDE.changed.snippet"
        changed.write_text(
            snippet.read_text(encoding="utf-8") + "\nan extra behavioural line.\n",
            encoding="utf-8",
        )
        run_installer(adapter, installer, target, changed, self.home, self.NO_BUILD_ENV)

        fields = provenance_of(target.read_text(encoding="utf-8"))
        self.assertNotEqual(
            fields.get("version"), TEST_VERSION,
            "a run that rewrites the block and cannot establish the version "
            "must not present the previous run's version as this block's "
            "provenance",
        )
        self.assertEqual(
            fields.get("version"), block_provenance.UNKNOWN,
            "a run that rewrites the block and cannot establish the version "
            "must say so",
        )


class TestCanonicalRendererUnaffected(ProvenanceTestCase):
    """Case 9 — the canonical renderer and its byte-equivalence gates are intact."""

    def test_the_renderer_reports_no_drift(self) -> None:
        result = subprocess.run(
            [sys.executable, str(SCRIPTS / "render_managed_blocks.py"), "--check"],
            cwd=str(REPO_ROOT),
            stdout=subprocess.PIPE,
            stderr=subprocess.PIPE,
            text=True,
            check=False,
        )
        self.assertEqual(result.returncode, 0,
                         f"the renderer reports drift:\n{result.stdout}\n{result.stderr}")

    def test_the_canonical_source_carries_no_provenance(self) -> None:
        text = (AGENT_INSTALLATION / "MPM_AGENT_INTEGRATION_SNIPPETS.md").read_text(encoding="utf-8")
        self.assertNotIn(block_provenance.PROVENANCE_PREFIX, text,
                         "the canonical source must not carry per-install metadata")

    def test_the_checked_in_snippets_carry_no_provenance(self) -> None:
        for adapter, _, snippet_name in ADAPTERS:
            snippet = AGENT_INSTALLATION / adapter / "templates" / snippet_name
            text = snippet.read_text(encoding="utf-8")
            self.assertNotIn(
                block_provenance.PROVENANCE_PREFIX, text,
                f"{adapter}'s checked-in snippet must not carry per-install metadata",
            )


if __name__ == "__main__":
    unittest.main()
