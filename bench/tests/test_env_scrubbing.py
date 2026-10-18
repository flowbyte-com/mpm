"""Environment-scrubbing tests (C4 gate 8).

Asserts that the harness's scrubbed_env():
  - inherits only the SAFE_ENV_KEYS,
  - sets MPM_WORKSPACE and HOME to the snapshot,
  - explicitly UNSETS MPM_SHARED_DB, MPM_DB_LOCK, and other
    dangerous overrides (a value of "" would still be picked up as
    a non-empty override at handlers_shred_database_reset.go:397,
    so the only safe thing is to leave the key absent).
"""

import os
import tempfile
import unittest
from pathlib import Path

from bench.workspace import DANGEROUS_INHERITED_KEYS, SAFE_ENV_KEYS, scrubbed_env


class TestScrubbedEnv(unittest.TestCase):
    def setUp(self) -> None:
        # Plant every dangerous key in the harness's environment so
        # we can verify the scrubbed env does not inherit them.
        for k in DANGEROUS_INHERITED_KEYS:
            os.environ[k] = f"/tmp/dangerous-{k}"
        # Plant every safe key.
        os.environ["PATH"] = "/usr/bin:/bin"
        os.environ["LANG"] = "C.UTF-8"
        os.environ["LC_ALL"] = "C.UTF-8"
        os.environ["TZ"] = "UTC"
        os.environ["TMPDIR"] = "/tmp"

    def tearDown(self) -> None:
        for k in DANGEROUS_INHERITED_KEYS:
            os.environ.pop(k, None)
        for k in ("PATH", "LANG", "LC_ALL", "TZ", "TMPDIR"):
            # Don't pop PATH etc.; they were already there. But unset
            # any test-planted extras.
            pass

    def test_inherits_only_safe_keys(self) -> None:
        with tempfile.TemporaryDirectory() as td:
            snap = Path(td)
            env = scrubbed_env(snap)
            for k in SAFE_ENV_KEYS:
                if k in os.environ:
                    self.assertIn(k, env)
            # Anything not in SAFE_ENV_KEYS and not the harness-set
            # MPM_WORKSPACE / HOME / fakehome must NOT be inherited.
            for k, v in env.items():
                self.assertTrue(
                    k in SAFE_ENV_KEYS or k in ("MPM_WORKSPACE", "HOME"),
                    f"unexpected key {k}={v} in scrubbed env",
                )

    def test_dangerous_keys_absent(self) -> None:
        with tempfile.TemporaryDirectory() as td:
            snap = Path(td)
            env = scrubbed_env(snap)
            for k in DANGEROUS_INHERITED_KEYS:
                self.assertNotIn(
                    k, env,
                    f"scrubbed_env must not pass {k!r} to subprocess; "
                    f"MPM_SHARED_DB / MPM_DB_LOCK are real env vars consulted "
                    f"by MPM (cmd/mpm/handlers_shared.go:33, "
                    f"cmd/mpm/handlers_shred_database_reset.go:397)",
                )

    def test_workspace_and_home_point_at_snap(self) -> None:
        with tempfile.TemporaryDirectory() as td:
            snap = Path(td)
            env = scrubbed_env(snap)
            self.assertEqual(env["MPM_WORKSPACE"], str(snap))
            self.assertEqual(env["HOME"], str(snap / "fakehome"))
            self.assertTrue((snap / "fakehome").is_dir())
            self.assertTrue((snap / "fakehome" / ".mpm").is_dir())

    def test_dangerous_keys_not_even_set_to_empty_string(self) -> None:
        # An empty string for MPM_DB_LOCK is still a non-empty value
        # at handlers_shred_database_reset.go:397. The scrubbed env
        # must not even set the key to ""; it must be absent.
        with tempfile.TemporaryDirectory() as td:
            snap = Path(td)
            env = scrubbed_env(snap)
            for k in DANGEROUS_INHERITED_KEYS:
                self.assertNotIn(k, env)
                self.assertFalse(k in env and env[k] == "")
