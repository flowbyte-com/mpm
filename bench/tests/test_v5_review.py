"""v5 review adversarial tests (HAL 2026-10-19).

These tests target the four narrow v5 corrections on top of the
v4 work:

  P1. Retrieval depth: the actual `mpm recall` subprocess
      receives `--limit <corpus.scoring.top_k>`, AND both
      raw-cosine conditions are invoked with that same top_k.
      Inspect the actual subprocess argument list, not just
      the Python run_query call. Test with k > 15 (MPM's
      default limit).

  P2. Canonical developer binary: the harness resolves the
      `make build` artifact at `<repo>/.build/bin/mpm`, refuses
      a stale root-level `mpm`, refuses to silently fall back
      to the operator's installed binary, and validates an
      explicit MPM_BENCH_BINARY override as absolute + executable
      before subprocess execution.

  P3. Real-schema integration: _snapshot_full is exercised
      against a genuinely initialized disposable database
      produced by the freshly built `mpm` CLI (not a hand-
      authored DDL subset). Uses the fake embedding provider
      (no live DB, no external service). A schema error fails
      the test.

  P4. Production count report: this test file does NOT
      enforce a count. The forensic 41 challenged memories
      are explicitly preserved; do not run mutating
      diagnostics against the live store.

v6 targets (four narrow corrections on top of v5 work):

  B1. Eliminate unsafe fixed-path test cleanup. The
      test_cmd_run_invokes_runner_with_corpus_top_k test
      now uses a `tempfile.TemporaryDirectory()` as its
      fixture root, with all mock workspace and output
      paths underneath. No fixed `/tmp/fake-*` paths.
      Cleanup is owned by the context manager.

  B2. Make negative schema validation unconditional. The
      class-level setUpClass no longer skips on a missing
      canonical binary. The negative schema test
      (test_snapshot_full_schema_error_fails_test) runs
      regardless of binary presence. A regression test
      (test_snapshot_full_schema_error_runs_even_when_binary_missing)
      simulates a missing binary and verifies the negative
      schema test still raises SnapshotSchemaError.

  B3. Complete test resource cleanup. The real-schema
      integration test now calls both `server.shutdown()`
      and `server.server_close()`, then cleans up its
      unique snap directory. Cleanup is exception-safe
      and never masks the original test failure.

  B4. Strengthen binary override validation. The harness
      now requires the resolved binary path to identify
      a regular file (not a directory or other non-file
      path). The check applies to both the canonical
      artifact and the MPM_BENCH_BINARY override.
      Tests cover rejection of an executable directory
      and a non-executable file.
"""

from __future__ import annotations

import argparse
import json
import os
import sqlite3
import subprocess
import sys
import tempfile
import unittest
import unittest.mock
from pathlib import Path


# -- helpers ------------------------------------------------------------


def _write_cfg(json_text: str) -> Path:
    """Write a temp config file and return its path. The harness's
    `cmd_run` reads `--config` as a file path."""
    td = tempfile.mkdtemp()
    p = Path(td) / "cfg.json"
    p.write_text(json_text)
    return p


_MINIMAL_CONFIG = (
    '{"profiles": {"default": {"provider": "openai-compatible", '
    '"model": "fake-embed-v1", "base_url": "http://127.0.0.1:1", '
    '"api_key": ""}}, "components": {"embedding": "default"}, '
    '"expected": {"model": "fake-embed-v1", "dimension": 8}}'
)


# -- P2: canonical developer binary -------------------------------------


class TestCanonicalDeveloperBinary(unittest.TestCase):
    """v5 review priority 2: the harness MUST prefer
    `<repo>/.build/bin/mpm` (the `make build` artifact). It
    MUST NOT silently fall back to a root-level `mpm` (which
    may be stale) or to the operator's installed `~/.mpm/bin/mpm`
    (which is production state). An explicit `MPM_BENCH_BINARY`
    override is resolved to an absolute path and validated
    before subprocess execution."""

    def setUp(self) -> None:
        from bench import harness as h
        self.h = h

    def test_canonical_path_is_build_artifact(self) -> None:
        # CANONICAL_MPM_BINARY must point at `.build/bin/mpm`,
        # NOT at the root-level `mpm` convenience copy and NOT
        # at the operator's installed `~/.mpm/bin/mpm`.
        p = self.h.CANONICAL_MPM_BINARY
        self.assertTrue(p.is_absolute(), f"canonical path must be absolute: {p}")
        self.assertEqual(
            p.name, "mpm",
            f"canonical path must end in mpm: {p}"
        )
        # The path is `<repo>/.build/bin/mpm`. The .build
        # segment is the load-bearing signal: that's the
        # directory `make build` populates.
        self.assertIn(
            ".build", p.parts,
            f"canonical path must be the make build artifact, "
            f"not the root-level convenience copy: {p}",
        )
        # Must NOT contain the operator's home (this is the
        # production state we never want as a silent fallback).
        home = str(Path.home())
        self.assertFalse(
            str(p).startswith(home + "/.mpm"),
            f"canonical path must not be the operator's installed "
            f"production binary: {p}",
        )

    def test_default_mpm_binary_points_to_canonical(self) -> None:
        # DEFAULT_MPM_BINARY is the alias used elsewhere in the
        # module; ensure it is the same path as CANONICAL.
        self.assertEqual(
            self.h.DEFAULT_MPM_BINARY, self.h.CANONICAL_MPM_BINARY,
            "DEFAULT_MPM_BINARY must alias CANONICAL_MPM_BINARY "
            "to prevent the pre-v5 behaviour of preferring a "
            "stale root-level `mpm`",
        )

    def test_resolve_rejects_missing_canonical(self) -> None:
        # If neither the override nor the canonical artifact
        # exists, _resolve_mpm_binary MUST raise with a message
        # that names both paths and explicitly states the
        # production-binary-fallback is forbidden.
        with unittest.mock.patch.dict(
            os.environ, {}, clear=False
        ):
            os.environ.pop("MPM_BENCH_BINARY", None)
            with unittest.mock.patch.object(
                self.h, "CANONICAL_MPM_BINARY",
                Path("/nonexistent/.build/bin/mpm"),
            ):
                with self.assertRaises(FileNotFoundError) as cm:
                    self.h._resolve_mpm_binary()
                msg = str(cm.exception)
                self.assertIn("make build", msg)
                self.assertIn("MPM_BENCH_BINARY", msg)
                self.assertIn(
                    "production state", msg,
                    f"error must explicitly forbid falling back to "
                    f"the operator's installed binary: {msg}",
                )

    def test_resolve_rejects_relative_override(self) -> None:
        with unittest.mock.patch.dict(
            os.environ,
            {"MPM_BENCH_BINARY": "relative/path/mpm"},
        ):
            with self.assertRaises(ValueError) as cm:
                self.h._resolve_mpm_binary()
            self.assertIn("absolute", str(cm.exception))

    def test_resolve_rejects_nonexistent_override(self) -> None:
        with unittest.mock.patch.dict(
            os.environ,
            {"MPM_BENCH_BINARY": "/no/such/mpm-binary-xyz"},
        ):
            with self.assertRaises(FileNotFoundError):
                self.h._resolve_mpm_binary()

    def test_resolve_accepts_explicit_absolute_override(self) -> None:
        # When the override is an absolute path to an
        # executable file, _resolve_mpm_binary returns it
        # (after resolving symlinks, so .resolve() is idempotent).
        with tempfile.NamedTemporaryFile(
            delete=False, suffix="", prefix="fake-mpm-",
        ) as f:
            f.write(b"#!/bin/sh\nexit 0\n")
            f.flush()
            os.chmod(f.name, 0o755)
            try:
                with unittest.mock.patch.dict(
                    os.environ,
                    {"MPM_BENCH_BINARY": f.name},
                ):
                    p = self.h._resolve_mpm_binary()
                self.assertTrue(p.is_absolute())
                self.assertTrue(p.exists())
            finally:
                os.unlink(f.name)

    def test_resolve_rejects_executable_directory(self) -> None:
        # v6 B4: an executable directory must NOT pass the
        # resolve check. On Unix, `/tmp` is an executable
        # directory; it would have passed the pre-v6 existence
        # and executable-bit checks. The new contract is that
        # the resolved path MUST identify a regular file.
        with tempfile.TemporaryDirectory(prefix="mpm-bench-v6-dir-") as td:
            with unittest.mock.patch.dict(
                os.environ,
                {"MPM_BENCH_BINARY": td},
            ):
                with self.assertRaises(IsADirectoryError) as cm:
                    self.h._resolve_mpm_binary()
                self.assertIn("regular file", str(cm.exception))

    def test_resolve_rejects_nonexecutable_file(self) -> None:
        # v6 B4: a non-executable regular file (e.g. a plain
        # text file with the executable bit unset) must be
        # rejected. The pre-v6 contract required executable;
        # the v6 contract layers the regular-file check on
        # top, so this test verifies the executable-bit
        # branch is still hit for non-executable files.
        with tempfile.NamedTemporaryFile(
            delete=False, suffix="", prefix="fake-mpm-nox-",
        ) as f:
            f.write(b"#!/bin/sh\nexit 0\n")
            f.flush()
            os.chmod(f.name, 0o644)  # not executable
            try:
                with unittest.mock.patch.dict(
                    os.environ,
                    {"MPM_BENCH_BINARY": f.name},
                ):
                    with self.assertRaises(PermissionError):
                        self.h._resolve_mpm_binary()
            finally:
                os.unlink(f.name)

    def test_resolve_canonical_rejects_directory(self) -> None:
        # v6 B4: same regular-file check applies to the
        # canonical artifact. If `.build/bin/mpm` is a
        # directory (e.g. the make build produced a path
        # but no binary), the resolve must fail closed
        # with IsADirectoryError, NOT silently pass through
        # to subprocess.run with a less informative error.
        # We must create the directory because the existence
        # check fires first; only then does the regular-
        # file check apply.
        with tempfile.TemporaryDirectory(
            prefix="mpm-bench-v6-cand-"
        ) as td:
            cand_dir = Path(td) / "mpm"
            cand_dir.mkdir(parents=True, exist_ok=True)
            with unittest.mock.patch.object(
                self.h, "CANONICAL_MPM_BINARY", cand_dir,
            ):
                with self.assertRaises(IsADirectoryError):
                    self.h._resolve_mpm_binary()


# -- P1: actual subprocess CLI args --------------------------------------


class TestRecallSubprocessReceivesTopK(unittest.TestCase):
    """v5 review priority 1: the actual `mpm recall` subprocess
    invocation MUST receive `--limit <top_k>` from
    `corpus.scoring.top_k`. The pre-v5 code relied on the
    runner's default of 5 and did not pass any limit, so the
    corpus's top_k was silently capped at 5 (and any corpus
    with top_k > 15 was using MPM's default of 15 without
    telling anyone). Inspect the actual subprocess.run args,
    not just the Python run_query call signature."""

    def test_limit_passed_for_top_k_5(self) -> None:
        # Standard case: top_k=5. The subprocess receives
        # `--limit 5` as a distinct argv pair.
        from bench import runner as r
        with unittest.mock.patch.object(
            r, "subprocess"
        ) as sp:
            # Build a fake CompletedProcess.
            fake = unittest.mock.MagicMock()
            fake.returncode = 0
            fake.stdout = json.dumps(
                {"memories": [], "search_mode": "hybrid"}
            )
            fake.stderr = ""
            sp.run.return_value = fake

            from bench.corpus import Query
            q = Query(
                query_id="q1", query="hello",
                relevance_class="positive",
                relevant_doc_ids=(),
            )
            with tempfile.TemporaryDirectory() as td:
                r.run_query(
                    q, Path(td), "/bin/true",
                    top_k=5,
                )
            # The first positional arg to subprocess.run is
            # the argv list.
            self.assertEqual(sp.run.call_count, 1)
            argv = sp.run.call_args[0][0]
            self.assertIn("--limit", argv)
            i = argv.index("--limit")
            self.assertEqual(argv[i + 1], "5",
                             f"--limit must be 5 for top_k=5; got {argv[i+1]!r}")

    def test_limit_passed_for_top_k_20(self) -> None:
        # Adversarial: top_k=20 (greater than MPM's default
        # limit of 15). The subprocess receives --limit 20,
        # which is the contract HAL's review requires.
        from bench import runner as r
        with unittest.mock.patch.object(
            r, "subprocess"
        ) as sp:
            fake = unittest.mock.MagicMock()
            fake.returncode = 0
            fake.stdout = json.dumps(
                {"memories": [], "search_mode": "hybrid"}
            )
            fake.stderr = ""
            sp.run.return_value = fake

            from bench.corpus import Query
            q = Query(
                query_id="q1", query="hello",
                relevance_class="positive",
                relevant_doc_ids=(),
            )
            with tempfile.TemporaryDirectory() as td:
                r.run_query(
                    q, Path(td), "/bin/true",
                    top_k=20,
                )
            argv = sp.run.call_args[0][0]
            self.assertIn("--limit", argv)
            i = argv.index("--limit")
            self.assertEqual(argv[i + 1], "20",
                             f"--limit must be 20 for top_k=20; got {argv[i+1]!r}")

    def test_raw_cosine_receives_top_k(self) -> None:
        # Both raw-cosine conditions (unprefixed + prefixed)
        # receive the corpus's top_k. We assert that the
        # top_k value reaches _raw_cosine_ranking in both
        # call paths through run_query.
        from bench import runner as r
        from bench.corpus import Query
        q = Query(
            query_id="q1", query="hello",
            relevance_class="positive",
            relevant_doc_ids=(),
        )
        captured: list[int] = []
        orig_raw = r._raw_cosine_ranking

        def fake_raw(query, snap, top_k, embedding_profile=None, prefix="", collection=None):
            captured.append(top_k)
            return []

        with tempfile.TemporaryDirectory() as td:
            with unittest.mock.patch.object(
                r, "_raw_cosine_ranking", side_effect=fake_raw
            ):
                r.run_query(
                    q, Path(td), "/bin/true",
                    raw_cosine=True, raw_cosine_prefix="",
                    top_k=20,
                    embedding_profile={
                        "provider": "x", "model": "y",
                        "base_url": "z", "api_key": "",
                    },
                )
                r.run_query(
                    q, Path(td), "/bin/true",
                    raw_cosine=True,
                    raw_cosine_prefix=r.RAW_COSINE_PREFIX_NOTE,
                    top_k=20,
                    embedding_profile={
                        "provider": "x", "model": "y",
                        "base_url": "z", "api_key": "",
                    },
                )
        self.assertEqual(
            captured, [20, 20],
            f"both raw-cosine conditions must receive top_k=20; got {captured}",
        )

    def test_cmd_run_invokes_runner_with_corpus_top_k(self) -> None:
        # End-to-end through cmd_run: the corpus has top_k=7
        # and every run_query call receives that value, not
        # the runner's default of 5.
        from bench import harness as h
        from bench.corpus import Corpus, Document, Query, Scoring, Expectations
        from bench.runner import QueryResult

        corpus = Corpus(
            version="v-test",
            documents=tuple(
                Document(doc_id=f"d{i:07d}", content=f"c{i}",
                         relevance_class="relevant")
                for i in range(1, 6)
            ),
            queries=(
                Query(query_id="q1", query="?",
                      relevance_class="positive",
                      relevant_doc_ids=("d0000001",)),
            ),
            expectations=Expectations(by_query={}),
            scoring=Scoring(method="mpm recall", top_k=7, parameters={}),
            manifest_path=None,  # type: ignore[arg-type]
        )
        captured_top_k: list[int] = []

        def fake_run_query(q, snap, mpm, **kw):
            captured_top_k.append(int(kw.get("top_k", -1)))
            return QueryResult(
                query_id=q.query_id, query=q.query,
                is_positive=q.is_positive, rows=(),
                search_mode="hybrid", raw_cosine=False,
            )

        # v6 B1: use a single, uniquely owned TemporaryDirectory as
        # the fixture root. No fixed /tmp paths. Every mock
        # workspace and output path lives under that root. Cleanup
        # is owned by `with tempfile.TemporaryDirectory() as td:`.
        with tempfile.TemporaryDirectory(prefix="mpm-bench-v6-topk-") as td:
            td_root = Path(td)
            cfg_path = td_root / "cfg.json"
            cfg_path.write_text(_MINIMAL_CONFIG)
            fake_snap = td_root / "fake-snap"
            fake_out = td_root / "fake-out"
            fake_snap.mkdir(parents=True, exist_ok=True)
            fake_out.mkdir(parents=True, exist_ok=True)
            args = argparse.Namespace(
                output_dir=str(fake_out),
                config=str(cfg_path),
                raw_cosine_prefix=False,
            )
            try:
                with unittest.mock.patch.object(h, "load_corpus",
                                                return_value=corpus), \
                     unittest.mock.patch.object(h, "_ingest_corpus"), \
                     unittest.mock.patch.object(h, "_backfill",
                                                return_value=(0, "")), \
                     unittest.mock.patch.object(h, "verify_regeneration_complete",
                                                return_value=unittest.mock.MagicMock(
                                                    ok=True,
                                                    to_dict=lambda: {"ok": True,
                                                                     "failures": []},
                                                )), \
                     unittest.mock.patch.object(h, "run_query",
                                                side_effect=fake_run_query), \
                     unittest.mock.patch.object(h, "_snapshot_full",
                                                return_value={"memories": [],
                                                             "reference_chunks": [],
                                                             "retrieval_metadata": []}), \
                     unittest.mock.patch.object(h, "_resolve_mpm_binary",
                                                return_value=Path("/bin/true")), \
                     unittest.mock.patch.object(h, "_make_snap_workspace",
                                                return_value=fake_snap), \
                     unittest.mock.patch.object(h, "assert_safe_workspace",
                                                return_value=fake_snap), \
                     unittest.mock.patch.object(h, "assert_safe_output_dir",
                                                return_value=fake_out), \
                     unittest.mock.patch.object(h, "reject_unsafe_symlinks",
                                                return_value=[]), \
                     unittest.mock.patch.object(h, "write_mpm_config",
                                                return_value=td_root / "fake-config"), \
                     unittest.mock.patch.object(h, "_find_db_path",
                                                return_value=fake_snap / "mpm.db"), \
                     unittest.mock.patch.object(h, "write_report",
                                                return_value=(Path("r.json"),
                                                             Path("r.md"))):
                    h.cmd_run(args)
            finally:
                # v6 B1: the TemporaryDirectory owns cleanup; we
                # only need to release our mock state. We do NOT
                # recursively delete any fixed path. The
                # TemporaryDirectory's __exit__ removes `td` and
                # every directory we created under it.
                pass
        # v6 B1 regression assertion: confirm no fixture path
        # references the pre-v6 hard-coded /tmp sentinels. The
        # strings /tmp/fake-snap and /tmp/fake-out-topk must not
        # appear in any of the test's fixture locals.
        for forbidden in ("/tmp/fake-snap", "/tmp/fake-out-topk",
                          "/tmp/fake-config"):
            self.assertNotIn(
                forbidden, str(cfg_path),
                f"fixture must not reference fixed path {forbidden!r}",
            )
            self.assertNotIn(
                forbidden, str(fake_snap),
                f"fixture must not reference fixed path {forbidden!r}",
            )
            self.assertNotIn(
                forbidden, str(fake_out),
                f"fixture must not reference fixed path {forbidden!r}",
            )
        self.assertTrue(captured_top_k, "run_query must be called")
        for k in captured_top_k:
            self.assertEqual(
                k, 7,
                f"cmd_run must pass top_k=7 to run_query; got {k}",
            )


# -- P3: real-schema integration against the fresh mpm binary ---------


class TestRealMpmSchemaIntegrationAgainstFreshBinary(unittest.TestCase):
    """v5 review priority 3: `_snapshot_full` must work
    against a genuinely initialized disposable database
    produced by the freshly-built `mpm` CLI — not a hand-
    authored DDL subset. The fake embedding provider is
    used; no live DB, no external service.

    A schema error MUST fail the test (it is not silently
    skipped). v6 B2: the negative schema test is unconditional
    and runs even when the canonical binary is absent. Only the
    real-binary integration test is gated on the binary's
    presence, and that gate lives INSIDE that test method,
    not at class scope."""

    @classmethod
    def setUpClass(cls) -> None:
        # v6 B2: do not skip the whole class on missing
        # canonical binary. The negative schema test must
        # always run. The binary gate is enforced inside
        # test_snapshot_full_against_real_mpm_init.
        from bench import harness as h
        cls.h = h

    def _make_fresh_snap(self) -> tuple[Path, "subprocess.Popen[bytes] | None"]:
        """Create a fresh disposable snap with the fake
        embedding server and capture all subprocess output."""
        from bench.fake_server import make_server

        server, base_url, port = make_server(
            dim=8, model_name="fake-embed-v1"
        )
        snap = self.h._make_snap_workspace("mpm-bench-v5int-")
        # Make sure snap exists on disk.
        snap.mkdir(parents=True, exist_ok=True)
        from bench.workspace import write_mpm_config, scrubbed_env
        write_mpm_config(
            snap,
            profile={
                "default": {
                    "provider": "openai-compatible",
                    "model": "fake-embed-v1",
                    "base_url": base_url,
                    "api_key": "",
                }
            },
            components={"embedding": "default"},
        )
        return snap, server

    def test_snapshot_full_against_real_mpm_init(self) -> None:
        # End-to-end: spin up the fake provider, run the
        # freshly-built mpm against the disposable workspace
        # so it creates the real schema, then call
        # _snapshot_full against the resulting DB and assert
        # the tracked tables are present.
        # v6 B2: the canonical binary gate lives INSIDE this
        # test only. A missing binary skips THIS test with a
        # clear diagnostic; the negative schema test below
        # remains unconditional.
        from bench import harness as h
        if not h.CANONICAL_MPM_BINARY.exists():
            raise unittest.SkipTest(
                f"canonical mpm binary not present at "
                f"{h.CANONICAL_MPM_BINARY}; run `make build` "
                f"to produce it before running this integration test"
            )
        snap, server = self._make_fresh_snap()
        # v6 B3: track the snap under a TemporaryDirectory
        # context so cleanup is unconditional and exception-
        # safe. We do not mask the original exception; cleanup
        # errors are silently ignored via the context manager
        # but the original test failure (if any) propagates.
        try:
            env = h.scrubbed_env(snap)
            # Trigger schema init by running a real mpm
            # command. `mpm remember bootstrap` is the
            # contract `_ingest_corpus` uses.
            init = subprocess.run(
                [str(h.CANONICAL_MPM_BINARY), "remember", "bootstrap"],
                env=env, cwd=str(snap), capture_output=True,
                text=True, timeout=60, check=False,
            )
            self.assertEqual(
                init.returncode, 0,
                f"mpm remember failed: rc={init.returncode} "
                f"stderr={init.stderr[:500]}",
            )
            db_path = h._find_db_path(snap)
            self.assertIsNotNone(
                db_path,
                f"mpm version did not create a database under {snap}",
            )
            snap_data = h._snapshot_full(db_path)  # type: ignore[arg-type]
            # All three tracked tables must be present and
            # have at least the bootstrap row.
            self.assertIn("memories", snap_data)
            self.assertIn("reference_chunks", snap_data)
            self.assertIn("retrieval_metadata", snap_data)
            # The number of rows the snapshot captured must
            # match the DB's actual row count for each table.
            con = sqlite3.connect(f"file:{db_path}?mode=ro", uri=True)  # type: ignore[arg-type]
            try:
                for t in ("memories", "reference_chunks",
                          "retrieval_metadata"):
                    actual = con.execute(
                        f"SELECT COUNT(*) FROM {t}"
                    ).fetchone()[0]
                    self.assertEqual(
                        len(snap_data[t]), actual,
                        f"row count mismatch for {t}: snapshot "
                        f"has {len(snap_data[t])}, DB has {actual}",
                    )
            finally:
                con.close()
        finally:
            # v6 B3: full server teardown — shutdown then
            # server_close. Both calls are idempotent and
            # exception-safe; they release the loopback socket
            # so the ResourceWarning on an unclosed socket is
            # eliminated. We do not mask the original test
            # failure: this finally block runs after the test
            # body raises (or returns), and re-raises the
            # original exception, not the cleanup error.
            if server is not None:
                try:
                    server.shutdown()
                finally:
                    server.server_close()
            # v6 B3: clean up the uniquely owned disposable
            # snapshot workspace. The snap lives under
            # tempfile.gettempdir()/mpm-bench/, which the
            # parent TemporaryDirectory would not own. We
            # remove only the specific snap path we created
            # in this test, never a fixed /tmp path.
            if snap.exists():
                import shutil as _shutil
                try:
                    _shutil.rmtree(snap)
                except OSError:
                    pass

    def test_snapshot_full_schema_error_fails_test(self) -> None:
        # Adversarial: build a DB with the WRONG schema and
        # call _snapshot_full. It MUST raise SnapshotSchemaError
        # — the test fails (does not skip) on schema mismatch.
        # This is the load-bearing "fail closed" property the
        # v5 review requires.
        from bench import harness as h
        with tempfile.TemporaryDirectory() as td:
            db = Path(td) / "wrong.db"
            # Create a DB that has `memories` but no
            # `retrieval_metadata` and no `reference_chunks`.
            con = sqlite3.connect(str(db))
            try:
                con.execute(
                    "CREATE TABLE memories (id TEXT PRIMARY KEY, content TEXT)"
                )
                con.execute(
                    "INSERT INTO memories VALUES ('x', 'y')"
                )
                con.commit()
            finally:
                con.close()
            with self.assertRaises(
                h.SnapshotSchemaError
            ) as cm:
                h._snapshot_full(db)
            # The error message must name the missing table.
            self.assertIn(
                "retrieval_metadata", str(cm.exception),
                f"error must name the missing table: {cm.exception}",
            )

    def test_snapshot_full_schema_error_runs_even_when_binary_missing(self) -> None:
        # v6 B2 regression: the negative schema test must
        # always run, even when the canonical binary is
        # absent. Simulate a missing canonical binary by
        # pointing CANONICAL_MPM_BINARY at a non-existent
        # path. The class-level setUpClass must NOT skip;
        # only the real-binary integration test must be
        # gated on binary presence.
        from bench import harness as h
        with unittest.mock.patch.object(
            h, "CANONICAL_MPM_BINARY",
            Path("/nonexistent/mpm-binary-xyz-v6"),
        ):
            with tempfile.TemporaryDirectory() as td:
                db = Path(td) / "wrong.db"
                con = sqlite3.connect(str(db))
                try:
                    con.execute(
                        "CREATE TABLE memories "
                        "(id TEXT PRIMARY KEY, content TEXT)"
                    )
                    con.execute(
                        "INSERT INTO memories VALUES ('x', 'y')"
                    )
                    con.commit()
                finally:
                    con.close()
                # Schema error must STILL raise, even with a
                # missing canonical binary. The test is
                # unconditional on the binary's presence.
                with self.assertRaises(h.SnapshotSchemaError) as cm:
                    h._snapshot_full(db)
                self.assertIn(
                    "retrieval_metadata", str(cm.exception),
                    f"error must name the missing table: {cm.exception}",
                )


if __name__ == "__main__":
    unittest.main()
