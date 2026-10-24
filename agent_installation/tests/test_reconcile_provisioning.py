"""
test_reconcile_provisioning.py — the generic reconciler's PROVISIONING
contract.

Background
----------

A managed block is INSTRUCTION: it tells an agent how to reach MPM.
Some hosts additionally need a BUILT ARTIFACT before that instruction
means anything — a compiled plugin payload, for instance.

Until 2026-10-24 the root installer reached adapters ONLY through the
managed-block installer named in `reconcile.json`. For OpenCode that
installer was `scripts/install_agents_instructions.py`, which writes
`~/.config/opencode/AGENTS.md` and nothing else. The adapter's own
`install.sh` — which builds the payload, stages it and registers it —
was never invoked by ANY installation path. So a fresh install
produced a registration and a managed block, and no plugin payload:
the plugin loaded nothing, and every install reported success.

The fix is a declarative `provision` block in `reconcile.json`,
dispatched by the same generic reconciler. The root installer stays
host-agnostic: it never names an adapter or a host, and the reconciler
never learns any host-specific fact — the probe gate lives in the
manifest.

What these tests pin
--------------------

  * A `provision` block IS invoked when its gate is open.
  * It is NOT invoked when its gate is closed (MPM must not create a
    host integration in order to provision it).
  * A REQUIRED provisioning failure exits 2 — the caller must not
    report success.
  * An ADVISORY provisioning failure exits 1.
  * An adapter with no `provision` block behaves exactly as before.
  * An `opt_in` adapter is never provisioned.

Each test builds a self-contained fake `agent_installation/` tree and
runs the REAL reconciler script out of it, so the discovery path
(`AGENT_INSTALLATION_DIR.glob("mpm-*")`) is exercised rather than
mocked.
"""

from __future__ import annotations

import json
import os
import shutil
import subprocess
import sys
import tempfile
import textwrap
import unittest
from pathlib import Path


REPO_ROOT = Path(__file__).resolve().parent.parent.parent
RECONCILER = (
    REPO_ROOT / "agent_installation" / "scripts" / "reconcile_managed_blocks.py"
)


def _provision_manifest(
    *,
    required: bool = True,
    gate: dict | None = None,
    provision_installer: str = "provision.sh",
    opt_in: bool = False,
) -> dict:
    manifest = {
        "host_label": "fakehost",
        "description": "fake managed-block installer",
        "installer": "manage.sh",
        "installer_type": "bash",
        "args": [],
        "opt_in": opt_in,
    }
    if gate is not None:
        manifest["provision"] = {
            "description": "fake provisioner",
            "installer": provision_installer,
            "installer_type": "bash",
            "args": [],
            "required": required,
            "if": gate,
        }
    return manifest


class _ReconcilerHarness(unittest.TestCase):
    """Stand up a fake agent_installation/ tree and run the real
    reconciler out of it."""

    def setUp(self):
        self._tmp = Path(tempfile.mkdtemp(prefix="mpm-reconcile-provision-"))
        self.addCleanup(lambda: shutil.rmtree(self._tmp, ignore_errors=True))
        self.home = self._tmp / "home"
        self.home.mkdir()
        self.agent_installation = self._tmp / "agent_installation"
        (self.agent_installation / "scripts").mkdir(parents=True)
        shutil.copy(RECONCILER, self.agent_installation / "scripts" / RECONCILER.name)
        self.adapter = self.agent_installation / "mpm-fakehost"
        self.adapter.mkdir()
        self.calls = self._tmp / "calls.log"

    def _write_executable(self, path: Path, body: str) -> None:
        path.write_text(textwrap.dedent(body))
        path.chmod(0o755)

    def _install_fake_adapter(self, manifest: dict) -> None:
        """A managed-block installer and a provisioner that each record
        that they ran."""
        self._write_executable(
            self.adapter / "manage.sh",
            f"""\
            #!/usr/bin/env bash
            echo "manage" >> "{self.calls}"
            exit 0
            """,
        )
        self._write_executable(
            self.adapter / "provision.sh",
            f"""\
            #!/usr/bin/env bash
            echo "provision" >> "{self.calls}"
            exit 0
            """,
        )
        (self.adapter / "reconcile.json").write_text(
            json.dumps(manifest, indent=2), encoding="utf-8"
        )

    def _run(self, *, process_home: Path | None = None) -> subprocess.CompletedProcess:
        env = os.environ.copy()
        env["HOME"] = str(process_home or self.home)
        return subprocess.run(
            [
                sys.executable,
                str(self.agent_installation / "scripts" / RECONCILER.name),
                "--home",
                str(self.home),
            ],
            capture_output=True,
            text=True,
            timeout=60,
            env=env,
        )

    def _call_log(self) -> list[str]:
        if not self.calls.exists():
            return []
        return [
            line for line in self.calls.read_text().splitlines() if line.strip()
        ]


class ProvisioningDispatch(_ReconcilerHarness):
    def test_provision_block_is_invoked_when_its_gate_is_open(self):
        """The whole point: the adapter's provisioner IS run."""
        present = self.home / ".config" / "fakehost"
        present.mkdir(parents=True)
        self._install_fake_adapter(
            _provision_manifest(
                gate={"any_of": [{"path": "~/.config/fakehost", "kind": "dir"}]}
            )
        )

        result = self._run()

        self.assertEqual(result.returncode, 0, result.stdout + result.stderr)
        self.assertIn(
            "provision",
            self._call_log(),
            "the provisioner was never invoked",
        )

    def test_provisioner_runs_against_the_home_the_reconciler_was_given(self):
        """The provisioner must act on the home the reconciler is reasoning about.

        A bash provisioner reads `$HOME`. The reconciler probes paths under
        the `--home` it was given. If the two disagree, the reconciler checks
        one machine's state and then mutates ANOTHER one's configuration —
        which, for an operator running a sandboxed check, means a test
        rewrites the real user's host configuration.

        The reconciler is therefore responsible for making the child
        process see the same home it is reasoning about, whatever the
        parent's HOME happens to be.
        """
        present = self.home / ".config" / "fakehost"
        present.mkdir(parents=True)
        other_home = self._tmp / "other-home"
        other_home.mkdir()
        # The provisioner records the home it was given, by writing into it.
        self._install_fake_adapter(
            _provision_manifest(
                gate={"any_of": [{"path": "~/.config/fakehost", "kind": "dir"}]}
            )
        )
        self._write_executable(
            self.adapter / "provision.sh",
            f"""\
            #!/usr/bin/env bash
            mkdir -p "$HOME/provisioned"
            echo "provision" >> "{self.calls}"
            exit 0
            """,
        )

        result = self._run(process_home=other_home)

        self.assertEqual(result.returncode, 0, result.stdout + result.stderr)
        self.assertTrue(
            (self.home / "provisioned").is_dir(),
            "the provisioner did not act on the home the reconciler was given",
        )
        self.assertFalse(
            (other_home / "provisioned").exists(),
            "the provisioner acted on the PARENT's home instead of the "
            "reconciler's home — a sandboxed run would mutate the real "
            "operator's configuration",
        )

    def test_provision_is_skipped_when_its_gate_is_closed(self):
        """MPM must not provision a host the machine does not use."""
        self._install_fake_adapter(
            _provision_manifest(
                gate={"any_of": [{"path": "~/.config/fakehost", "kind": "dir"}]}
            )
        )

        result = self._run()

        self.assertEqual(result.returncode, 0, result.stdout + result.stderr)
        self.assertNotIn(
            "provision",
            self._call_log(),
            "the provisioner ran on a machine with no such integration",
        )
        self.assertIn("manage", self._call_log())

    def test_adapter_without_a_provision_block_is_unchanged(self):
        """No provision block → exactly the previous behaviour."""
        self._install_fake_adapter(_provision_manifest())

        result = self._run()

        self.assertEqual(result.returncode, 0, result.stdout + result.stderr)
        self.assertEqual(self._call_log(), ["manage"])

    def test_opt_in_adapter_is_not_provisioned(self):
        """An opt-in adapter is never reconciled automatically."""
        present = self.home / ".config" / "fakehost"
        present.mkdir(parents=True)
        self._install_fake_adapter(
            _provision_manifest(
                opt_in=True,
                gate={"any_of": [{"path": "~/.config/fakehost", "kind": "dir"}]},
            )
        )

        result = self._run()

        self.assertEqual(result.returncode, 0, result.stdout + result.stderr)
        self.assertEqual(self._call_log(), [])


class ProvisioningFailureSeverity(_ReconcilerHarness):
    def _stage_failing_provisioner(self, manifest: dict) -> None:
        present = self.home / ".config" / "fakehost"
        present.mkdir(parents=True)
        self._write_executable(
            self.adapter / "manage.sh",
            f"""\
            #!/usr/bin/env bash
            echo "manage" >> "{self.calls}"
            exit 0
            """,
        )
        self._write_executable(
            self.adapter / "provision.sh",
            f"""\
            #!/usr/bin/env bash
            echo "provision" >> "{self.calls}"
            echo "provisioner failed" >&2
            exit 3
            """,
        )
        (self.adapter / "reconcile.json").write_text(
            json.dumps(manifest, indent=2), encoding="utf-8"
        )

    def test_required_provision_failure_exits_2(self):
        """A required provisioning failure must NOT be reported as
        success."""
        self._stage_failing_provisioner(
            _provision_manifest(
                required=True,
                gate={"any_of": [{"path": "~/.config/fakehost", "kind": "dir"}]},
            )
        )

        result = self._run()

        self.assertEqual(
            result.returncode, 2,
            "a required provisioning failure did not exit 2:\n"
            + result.stdout + result.stderr,
        )

    def test_advisory_provision_failure_exits_1(self):
        """An advisory provisioning failure is a warning, not a
        success — but it is not fatal."""
        self._stage_failing_provisioner(
            _provision_manifest(
                required=False,
                gate={"any_of": [{"path": "~/.config/fakehost", "kind": "dir"}]},
            )
        )

        result = self._run()

        self.assertEqual(
            result.returncode, 1,
            "an advisory provisioning failure did not exit 1:\n"
            + result.stdout + result.stderr,
        )

    def test_missing_provisioner_is_a_required_failure(self):
        """A manifest that names a provisioner that is not there is a
        broken contract, not a silent skip."""
        present = self.home / ".config" / "fakehost"
        present.mkdir(parents=True)
        self._install_fake_adapter(
            _provision_manifest(
                required=True,
                provision_installer="absent.sh",
                gate={"any_of": [{"path": "~/.config/fakehost", "kind": "dir"}]},
            )
        )

        result = self._run()

        self.assertEqual(result.returncode, 2, result.stdout + result.stderr)


if __name__ == "__main__":
    unittest.main()
