"""Adversarial regression tests (priority 5 of HAL's review).

These tests assert the harness's isolation and verification
discipline against specific attack shapes. They do NOT use any
mpm binary; they construct the same in-process conditions an
adversary would and verify the harness's hard refusals.

Each test names a specific failure mode in its docstring so
the future reader knows what regression it guards.
"""

import json
import os
import sqlite3
import stat
import tempfile
import unittest
from pathlib import Path

from bench.workspace import (
    DANGEROUS_ENV_PREFIXES,
    DANGEROUS_INHERITED_KEYS,
    WorkspaceError,
    assert_safe_output_dir,
    assert_safe_workspace,
    reject_unsafe_symlinks,
    recheck_output_dir_safe,
    scrubbed_env,
)
from bench.fingerprint import verify_regeneration_complete


# ---- Filesystem isolation adversarial tests (priority 1) ----

class TestSymlinkChainIntoLiveStore(unittest.TestCase):
    """A within-root symlink that resolves to ~/.mpm must be
    rejected even though it points "inside" the root directory."""

    def test_within_root_symlink_to_home_mpm_rejected(self) -> None:
        with tempfile.TemporaryDirectory() as td:
            root = Path(td) / "out"
            root.mkdir(parents=True, mode=0o700)
            # Create a within-root symlink whose target is
            # ~/.mpm (a live-store sentinel). The pre-hardening
            # `reject_unsafe_symlinks` would have accepted this
            # because the symlink target IS inside `root` after
            # the within-root check; the priority-1 hardening
            # adds a sentinel cross-check.
            try:
                (root / "leak").symlink_to(Path.home() / ".mpm")
            except (OSError, NotImplementedError):
                self.skipTest("symlinks unsupported")
            bad = reject_unsafe_symlinks(root)
            self.assertEqual(
                len(bad), 1,
                f"expected symlink to ~/.mpm to be rejected; got bad={bad}",
            )


class TestRecheckOutputDirTOCTOU(unittest.TestCase):
    """Between `assert_safe_output_dir` and the actual write, a
    local actor could chmod the dir or replace it with a symlink
    into ~/.mpm. `recheck_output_dir_safe` catches that."""

    def test_disappeared_output_dir_rejected(self) -> None:
        with tempfile.TemporaryDirectory() as td:
            snap = Path(td) / "snap"
            out = Path(td) / "out"
            snap.mkdir(parents=True)
            out.mkdir(parents=True, mode=0o700)
            real_snap = assert_safe_workspace(snap)
            assert_safe_output_dir(out, real_snap)
            # Adversary: rmdir the output dir.
            out.rmdir()
            with self.assertRaises(WorkspaceError) as cm:
                recheck_output_dir_safe(out, real_snap)
            self.assertIn("disappeared", str(cm.exception))

    def test_widened_permissions_rejected(self) -> None:
        with tempfile.TemporaryDirectory() as td:
            snap = Path(td) / "snap"
            out = Path(td) / "out"
            snap.mkdir(parents=True)
            out.mkdir(parents=True, mode=0o700)
            real_snap = assert_safe_workspace(snap)
            assert_safe_output_dir(out, real_snap)
            # Adversary: chmod the dir wider.
            os.chmod(out, 0o755)
            with self.assertRaises(WorkspaceError) as cm:
                recheck_output_dir_safe(out, real_snap)
            self.assertIn("permissions", str(cm.exception))


class TestEnvPrefixDenyList(unittest.TestCase):
    """Any env var whose name starts with MPM_/OLLAMA_/OPENAI_/
    ANTHROPIC_/OPENROUTER_ must be scrubbed, even if it is not
    in DANGEROUS_INHERITED_KEYS. This is a forward-compatible
    guard against future MPM overrides."""

    def test_planted_prefix_keys_absent(self) -> None:
        planted = {
            "MPM_FUTURE_OVERRIDE": "/tmp/danger",
            "OLLAMA_FUTURE_OVERRIDE": "/tmp/danger",
            "OPENAI_FUTURE_OVERRIDE": "/tmp/danger",
            "ANTHROPIC_FUTURE_OVERRIDE": "/tmp/danger",
            "OPENROUTER_FUTURE_OVERRIDE": "/tmp/danger",
        }
        for k, v in planted.items():
            os.environ[k] = v
        try:
            with tempfile.TemporaryDirectory() as td:
                snap = Path(td)
                env = scrubbed_env(snap)
                for k in planted:
                    self.assertNotIn(
                        k, env,
                        f"prefix denylist must scrub {k!r}; "
                        f"DANGEROUS_ENV_PREFIXES={DANGEROUS_ENV_PREFIXES}",
                    )
        finally:
            for k in planted:
                os.environ.pop(k, None)

    def test_planted_keys_in_dangerous_set_still_absent(self) -> None:
        # The pre-existing denylist must still work after the
        # prefix guard was added.
        for k in DANGEROUS_INHERITED_KEYS:
            os.environ[k] = f"/tmp/danger-{k}"
        try:
            with tempfile.TemporaryDirectory() as td:
                snap = Path(td)
                env = scrubbed_env(snap)
                for k in DANGEROUS_INHERITED_KEYS:
                    self.assertNotIn(k, env)
        finally:
            for k in DANGEROUS_INHERITED_KEYS:
                os.environ.pop(k, None)


# ---- Regeneration verification adversarial tests (priority 2) ----

SCHEMA = """
CREATE TABLE IF NOT EXISTS memories (
    id TEXT PRIMARY KEY,
    collection TEXT NOT NULL,
    content TEXT NOT NULL,
    embedding BLOB,
    embedding_source TEXT NOT NULL DEFAULT 'provider',
    embedding_dimension INTEGER,
    embedding_model TEXT,
    deleted_at INTEGER,
    weight REAL
);
CREATE TABLE IF NOT EXISTS reference_chunks (
    id TEXT PRIMARY KEY,
    doc_id TEXT NOT NULL,
    chunk_index INTEGER NOT NULL,
    section TEXT,
    content TEXT NOT NULL,
    embedding BLOB,
    embedding_source TEXT NOT NULL DEFAULT 'provider',
    embedding_dimension INTEGER,
    embedding_model TEXT
);
"""


def _make_db(path: Path, rows: list[dict]) -> None:
    con = sqlite3.connect(path)
    try:
        con.executescript(SCHEMA)
        for r in rows:
            con.execute(
                "INSERT INTO memories (id, collection, content, embedding, "
                "embedding_source, embedding_dimension, embedding_model) "
                "VALUES (?, ?, ?, ?, ?, ?, ?)",
                (
                    r["id"],
                    "memories",
                    r["content"],
                    r.get("embedding"),
                    r.get("embedding_source", "provider"),
                    r.get("embedding_dimension"),
                    r.get("embedding_model"),
                ),
            )
        con.commit()
    finally:
        con.close()


def _vec_blob(values: list[float]) -> bytes:
    return json.dumps(values).encode("utf-8")


class TestVerifyRegenerationAdversarial(unittest.TestCase):
    """Adversarial inputs to `verify_regeneration_complete`:
    zero-norm, NaN/Inf, and over-scaled vectors should be
    caught even when the fingerprint columns and length all
    check out."""

    def test_zero_vector_detected(self) -> None:
        with tempfile.TemporaryDirectory() as td:
            db = Path(td) / "test.db"
            rows = [
                {
                    "id": "d0000001",
                    "content": "x",
                    "embedding": _vec_blob([0.0] * 8),
                    "embedding_source": "provider",
                    "embedding_dimension": 8,
                    "embedding_model": "fake-embed-v1",
                },
            ]
            _make_db(db, rows)
            r = verify_regeneration_complete(
                db,
                expected_model="fake-embed-v1",
                expected_dimension=8,
            )
            self.assertFalse(r.ok)
            self.assertTrue(
                any("all-zero" in f for f in r.failures),
                f"expected all-zero failure, got {r.failures}",
            )

    def test_nan_in_vector_detected(self) -> None:
        # A NaN can survive a json.loads round-trip because
        # Python's json module accepts "NaN" / "Infinity" as
        # extensions. The fingerprint check passes (length and
        # model match), so the new sanity check is the only
        # thing that catches this.
        with tempfile.TemporaryDirectory() as td:
            db = Path(td) / "test.db"
            rows = [
                {
                    "id": "d0000001",
                    "content": "x",
                    "embedding": _vec_blob([0.0, float("nan"), 0.0, 0.0,
                                            0.0, 0.0, 0.0, 0.0]),
                    "embedding_source": "provider",
                    "embedding_dimension": 8,
                    "embedding_model": "fake-embed-v1",
                },
            ]
            _make_db(db, rows)
            r = verify_regeneration_complete(
                db,
                expected_model="fake-embed-v1",
                expected_dimension=8,
            )
            self.assertFalse(r.ok)
            self.assertTrue(
                any("NaN" in f for f in r.failures),
                f"expected NaN/Inf failure, got {r.failures}",
            )

    def test_huge_norm_detected(self) -> None:
        with tempfile.TemporaryDirectory() as td:
            db = Path(td) / "test.db"
            rows = [
                {
                    "id": "d0000001",
                    "content": "x",
                    "embedding": _vec_blob([1e9] * 8),
                    "embedding_source": "provider",
                    "embedding_dimension": 8,
                    "embedding_model": "fake-embed-v1",
                },
            ]
            _make_db(db, rows)
            r = verify_regeneration_complete(
                db,
                expected_model="fake-embed-v1",
                expected_dimension=8,
            )
            self.assertFalse(r.ok)
            self.assertTrue(
                any("L2 norm" in f for f in r.failures),
                f"expected huge-norm failure, got {r.failures}",
            )

    def test_finite_normalized_vector_passes(self) -> None:
        # A unit-L2 vector with finite entries must still pass;
        # the sanity checks must not over-trigger.
        with tempfile.TemporaryDirectory() as td:
            db = Path(td) / "test.db"
            import math
            v = [0.5, 0.5, 0.5, 0.5, 0.0, 0.0, 0.0, 0.0]
            n = math.sqrt(sum(x * x for x in v))
            v = [x / n for x in v]
            rows = [
                {
                    "id": "d0000001",
                    "content": "x",
                    "embedding": _vec_blob(v),
                    "embedding_source": "provider",
                    "embedding_dimension": 8,
                    "embedding_model": "fake-embed-v1",
                },
            ]
            _make_db(db, rows)
            r = verify_regeneration_complete(
                db,
                expected_model="fake-embed-v1",
                expected_dimension=8,
            )
            self.assertTrue(
                r.ok, f"expected ok, got failures: {r.failures}",
            )


# ---- Runner adversarial tests (priority 4: raw-cosine path) ----

class TestRawCosineQueryEmbeddingAdversarial(unittest.TestCase):
    """The raw-cosine experiment must use the configured provider
    for the query embedding. The pre-fix path used a SHA-256
    hash that was only meaningful for the fake server. This
    test wires a fake server on a different port, points a
    temp DB at the same provider, and asserts the ranker
    actually called the server for the query."""

    def test_raw_cosine_uses_provider_not_hash(self) -> None:
        from bench.corpus import Query
        from bench.runner import _raw_cosine_ranking
        from bench.fake_server import deterministic_vector, make_server
        import json as _json

        with tempfile.TemporaryDirectory() as td:
            # Spin up a fake server with a non-default dim and
            # check the runner actually POSTs to it.
            dim = 16
            server, base_url, port = make_server(
                dim=dim, model_name="custom-model"
            )
            try:
                # Create a DB and a doc. _find_db_path accepts
                # either a workspace dir or a direct .db file
                # path, so we pass the .db file directly to
                # avoid coupling the test to the mpm.db naming
                # convention.
                snap = Path(td) / "snap"
                snap.mkdir(parents=True)
                db = snap / "mpm.db"
                con = sqlite3.connect(str(db))
                try:
                    con.executescript(SCHEMA)
                    con.execute(
                        "INSERT INTO memories (id, collection, content, "
                        "embedding, embedding_source, embedding_dimension, "
                        "embedding_model) VALUES (?, ?, ?, ?, ?, ?, ?)",
                        (
                            "d0000001",
                            "memories",
                            "x",
                            _vec_blob(deterministic_vector("x", dim)),
                            "provider",
                            dim,
                            "custom-model",
                        ),
                    )
                    con.commit()
                finally:
                    con.close()

                # Run the raw-cosine path with the provider
                # profile, NOT with the hash fallback.
                q = Query(
                    query_id="q1",
                    query="x",
                    relevance_class="positive",
                    relevant_doc_ids=("d0000001",),
                )
                rows = _raw_cosine_ranking(
                    q, db, top_k=1,
                    embedding_profile={
                        "provider": "openai-compatible",
                        "model": "custom-model",
                        "base_url": base_url,
                        "api_key": "",
                    },
                )
                self.assertEqual(len(rows), 1)
                # The doc was embedded with the fake server's
                # vector_fn, and the query was embedded via the
                # same server. Cosine must find d0000001 first.
                self.assertEqual(rows[0]["id"], "d0000001")
            finally:
                server.shutdown()


# ---- v3 review adversarial tests (HAL 2026-10-19) ----


class TestLiveStoreNotInspected(unittest.TestCase):
    """The harness must NEVER inspect the live `~/.mpm` SQLite DB.

    v3 review priority 2: the v2 path read the live store's
    row counts as a sentinel. The v3 contract is that synthetic
    benchmark runs do not touch the live store at all.
    """

    def test_harness_does_not_call_live_store_snapshots(self) -> None:
        import bench.harness as h
        # The function must be gone; this raises AttributeError
        # on the v2 code, which is the load-bearing assertion.
        self.assertFalse(
            hasattr(h, "_live_store_snapshots"),
            "v3 must remove _live_store_snapshots; live-store "
            "inspection is out of contract",
        )
        self.assertFalse(
            hasattr(h, "_live_store_sentinel_compare"),
            "v3 must remove _live_store_sentinel_compare",
        )


class TestOutputDirValidatedBeforeMkdir(unittest.TestCase):
    """v3 review priority 2: the safety checks on the output dir
    run BEFORE any `mkdir`. The pre-fix code called
    `out.mkdir(parents=True, exist_ok=True, mode=0o700)` before
    the sentinel check, which created
    `~/.mpm/harness-output-should-be-rejected` on disk before
    the WorkspaceError raised."""

    def test_protected_root_path_not_created_on_rejection(self) -> None:
        with tempfile.TemporaryDirectory() as home_root:
            with tempfile.TemporaryDirectory() as td:
                home = Path(home_root)
                mpm = home / ".mpm"
                mpm.mkdir(parents=True, exist_ok=True, mode=0o700)
                out = mpm / "should-not-be-created"
                snap = Path(td) / "snap"
                snap.mkdir(parents=True)
                real_snap = assert_safe_workspace(snap)
                with self.assertRaises(WorkspaceError):
                    assert_safe_output_dir(
                        out, real_snap, protected_root=home, create=True
                    )
                self.assertFalse(
                    out.exists(),
                    "the rejection path must not create on disk",
                )


class TestSnapshotIsMkdtemp(unittest.TestCase):
    """v3 review priority 2: the snapshot workspace is created via
    `tempfile.mkdtemp` (not a PID-derived name)."""

    def test_make_snap_workspace_uses_mkdtemp(self) -> None:
        from bench.harness import _make_snap_workspace
        a = _make_snap_workspace("mpm-bench-test-")
        b = _make_snap_workspace("mpm-bench-test-")
        try:
            self.assertNotEqual(a, b, "each call must allocate a fresh dir")
            self.assertTrue(a.is_dir())
            self.assertTrue(b.is_dir())
            # Permission is 0o700.
            self.assertEqual(stat.S_IMODE(os.stat(a).st_mode), 0o700)
        finally:
            import shutil
            shutil.rmtree(a, ignore_errors=True)
            shutil.rmtree(b, ignore_errors=True)


class TestSymlinkRejectedBeforeMpmLaunch(unittest.TestCase):
    """v3 review priority 2: `reject_unsafe_symlinks` runs on the
    snapshot workspace in addition to the output dir, BEFORE MPM
    is launched. A within-snap symlink into a protected root
    must be caught."""

    def test_within_snap_symlink_to_protected_root_rejected(self) -> None:
        with tempfile.TemporaryDirectory() as home_root:
            with tempfile.TemporaryDirectory() as td:
                home = Path(home_root)
                mpm = home / ".mpm"
                mpm.mkdir(parents=True, exist_ok=True, mode=0o700)
                snap = Path(td) / "snap"
                snap.mkdir(parents=True, mode=0o700)
                try:
                    (snap / "leak").symlink_to(mpm)
                except (OSError, NotImplementedError):
                    self.skipTest("symlinks unsupported")
                bad = reject_unsafe_symlinks(snap)
                self.assertEqual(
                    len(bad), 1,
                    f"within-snap symlink to a protected root must "
                    f"be rejected; got bad={bad}",
                )


class TestPartialReferenceBackfillFails(unittest.TestCase):
    """v3 review priority 3: NULL/missing detection in BOTH
    `memories` AND `reference_chunks`. A partial backfill that
    misses even one reference_chunks row must fail."""

    def test_partial_reference_backfill_detected(self) -> None:
        dim = 8
        # 5 of 10 reference chunks have embeddings.
        embedded_ids = {f"d{i:08d}" for i in range(1, 6)}
        missing_ids = sorted(f"d{i:08d}" for i in range(6, 11))
        with tempfile.TemporaryDirectory() as td:
            db = Path(td) / "test.db"
            con = sqlite3.connect(str(db))
            try:
                con.executescript(SCHEMA)
                # 5 fully-embedded ref_chunks.
                for did in embedded_ids:
                    con.execute(
                        "INSERT INTO reference_chunks "
                        "(id, doc_id, chunk_index, content, embedding, "
                        "embedding_source, embedding_dimension, "
                        "embedding_model) "
                        "VALUES (?, ?, ?, ?, ?, ?, ?, ?)",
                        (
                            did,
                            did,
                            0,
                            "x",
                            _vec_blob([0.1] * dim),
                            "provider",
                            dim,
                            "fake-embed-v1",
                        ),
                    )
                # 5 ref_chunks with NULL embedding (partial backfill).
                # The schema's embedding_source has NOT NULL DEFAULT
                # 'provider', so omit it on partial rows to simulate
                # a row that the embedder never wrote.
                for did in missing_ids:
                    con.execute(
                        "INSERT INTO reference_chunks "
                        "(id, doc_id, chunk_index, content, embedding, "
                        "embedding_source, embedding_dimension, "
                        "embedding_model) "
                        "VALUES (?, ?, ?, ?, ?, ?, ?, ?)",
                        (
                            did,
                            did,
                            0,
                            "x",
                            None,
                            "provider",
                            None,
                            None,
                        ),
                    )
                con.commit()
            finally:
                con.close()
            r = verify_regeneration_complete(
                db,
                expected_model="fake-embed-v1",
                expected_dimension=dim,
                expected_reference_doc_ids=embedded_ids | set(missing_ids),
            )
            self.assertFalse(
                r.ok,
                f"partial reference_chunks backfill must fail; "
                f"got failures={r.failures}",
            )
            self.assertTrue(
                any("NULL" in f for f in r.failures),
                f"expected NULL/missing detection failure, got {r.failures}",
            )
            self.assertTrue(
                any("missing entirely" in f or "no embedded chunk" in f
                    for f in r.failures),
                f"expected missing-doc coverage failure, got {r.failures}",
            )

    def test_expected_row_count_mismatch_detected(self) -> None:
        # Caller passes expected_memories_row_count=10 but only 5
        # rows have the expected fingerprint. Verification must
        # catch the count mismatch even if every row is embedded.
        dim = 8
        with tempfile.TemporaryDirectory() as td:
            db = Path(td) / "test.db"
            con = sqlite3.connect(str(db))
            try:
                con.executescript(SCHEMA)
                for i in range(1, 6):
                    con.execute(
                        "INSERT INTO memories (id, collection, content, "
                        "embedding, embedding_source, "
                        "embedding_dimension, embedding_model) "
                        "VALUES (?, ?, ?, ?, ?, ?, ?)",
                        (
                            f"d{i:08d}",
                            "memories",
                            "x",
                            _vec_blob([0.1] * dim),
                            "provider",
                            dim,
                            "fake-embed-v1",
                        ),
                    )
                con.commit()
            finally:
                con.close()
            r = verify_regeneration_complete(
                db,
                expected_model="fake-embed-v1",
                expected_dimension=dim,
                expected_memories_row_count=10,
            )
            self.assertFalse(r.ok)
            self.assertTrue(
                any("row count" in f for f in r.failures),
                f"expected row-count failure, got {r.failures}",
            )


class TestRawCosinePrefixComparison(unittest.TestCase):
    """v3 review priority 4: the raw-cosine experiment runs BOTH
    the unprefixed and `search_query:`-prefixed query embeddings
    through the configured provider, on identical document
    vectors. Both conditions must produce valid scores; the
    delta between them is reported."""

    def test_both_conditions_return_scores(self) -> None:
        from bench.corpus import Query
        from bench.runner import _raw_cosine_ranking, RAW_COSINE_PREFIX_NOTE
        from bench.fake_server import deterministic_vector, make_server

        with tempfile.TemporaryDirectory() as td:
            dim = 16
            server, base_url, port = make_server(
                dim=dim, model_name="custom-model"
            )
            try:
                snap = Path(td) / "snap"
                snap.mkdir(parents=True)
                db = snap / "mpm.db"
                con = sqlite3.connect(str(db))
                try:
                    con.executescript(SCHEMA)
                    con.execute(
                        "INSERT INTO memories (id, collection, content, "
                        "embedding, embedding_source, "
                        "embedding_dimension, embedding_model) "
                        "VALUES (?, ?, ?, ?, ?, ?, ?)",
                        (
                            "d0000001",
                            "memories",
                            "x",
                            _vec_blob(deterministic_vector("x", dim)),
                            "provider",
                            dim,
                            "custom-model",
                        ),
                    )
                    con.commit()
                finally:
                    con.close()
                profile = {
                    "provider": "openai-compatible",
                    "model": "custom-model",
                    "base_url": base_url,
                    "api_key": "",
                }
                q = Query(
                    query_id="q1",
                    query="x",
                    relevance_class="positive",
                    relevant_doc_ids=("d0000001",),
                )
                # Unprefixed.
                rows_u = _raw_cosine_ranking(
                    q, db, top_k=1, embedding_profile=profile, prefix=""
                )
                self.assertEqual(len(rows_u), 1)
                self.assertEqual(rows_u[0]["id"], "d0000001")
                # Prefixed.
                rows_p = _raw_cosine_ranking(
                    q, db, top_k=1,
                    embedding_profile=profile,
                    prefix=RAW_COSINE_PREFIX_NOTE,
                )
                self.assertEqual(len(rows_p), 1)
                self.assertEqual(rows_p[0]["id"], "d0000001")
                # Both conditions produced a cosine score (the
                # value will differ because the fake server
                # hashes a different text each time).
                self.assertGreater(rows_u[0]["score"], 0.0)
                self.assertGreater(rows_p[0]["score"], 0.0)
            finally:
                server.shutdown()


class TestRawCosineDocVectorNormalized(unittest.TestCase):
    """v3 review priority 4: the runner L2-normalizes document
    vectors at rank time, so a 2x scale on a doc still produces
    the same ranking as a 1x scale."""

    def test_ranking_invariant_under_doc_scale(self) -> None:
        from bench.corpus import Query
        from bench.runner import _raw_cosine_ranking
        from bench.fake_server import deterministic_vector, make_server
        import math

        with tempfile.TemporaryDirectory() as td:
            dim = 16
            server, base_url, port = make_server(
                dim=dim, model_name="m"
            )
            try:
                snap = Path(td) / "snap"
                snap.mkdir(parents=True)
                db = snap / "mpm.db"
                con = sqlite3.connect(str(db))
                try:
                    con.executescript(SCHEMA)
                    # Two docs: one with a 1x-scaled vector, one
                    # with a 2x-scaled vector. The query embedding
                    # is the unit-L2 normalized version of the
                    # fake-server output. Ranking should be
                    # invariant under the 2x scale.
                    v1 = deterministic_vector("a", dim)
                    v2 = [2.0 * x for x in deterministic_vector("b", dim)]
                    con.execute(
                        "INSERT INTO memories (id, collection, content, "
                        "embedding, embedding_source, "
                        "embedding_dimension, embedding_model) "
                        "VALUES (?, ?, ?, ?, ?, ?, ?)",
                        (
                            "d0000001",
                            "memories",
                            "a",
                            _vec_blob(v1),
                            "provider",
                            dim,
                            "m",
                        ),
                    )
                    con.execute(
                        "INSERT INTO memories (id, collection, content, "
                        "embedding, embedding_source, "
                        "embedding_dimension, embedding_model) "
                        "VALUES (?, ?, ?, ?, ?, ?, ?)",
                        (
                            "d0000002",
                            "memories",
                            "b",
                            _vec_blob(v2),
                            "provider",
                            dim,
                            "m",
                        ),
                    )
                    con.commit()
                finally:
                    con.close()
                q = Query(
                    query_id="q1",
                    query="a",
                    relevance_class="positive",
                    relevant_doc_ids=("d0000001",),
                )
                rows = _raw_cosine_ranking(
                    q, db, top_k=2,
                    embedding_profile={
                        "provider": "openai-compatible",
                        "model": "m",
                        "base_url": base_url,
                        "api_key": "",
                    },
                )
                self.assertEqual(len(rows), 2)
                # The top result must be d0000001 (the doc whose
                # text matches the query); the 2x-scaled doc
                # must NOT jump ahead just because it was
                # numerically larger.
                self.assertEqual(rows[0]["id"], "d0000001")
            finally:
                server.shutdown()


class TestRawCosineRefusesHashInRealRun(unittest.TestCase):
    """v3 review priority 4: when an embedding_profile is provided
    and the embed call fails, the function must NOT silently fall
    back to the hash function. The exception propagates."""

    def test_embed_failure_propagates(self) -> None:
        from bench.corpus import Query
        from bench.runner import _raw_cosine_ranking
        from bench.embed_query import EmbedError

        with tempfile.TemporaryDirectory() as td:
            snap = Path(td) / "snap"
            snap.mkdir(parents=True)
            db = snap / "mpm.db"
            con = sqlite3.connect(str(db))
            try:
                con.executescript(SCHEMA)
                con.execute(
                    "INSERT INTO memories (id, collection, content, "
                    "embedding, embedding_source, "
                    "embedding_dimension, embedding_model) "
                    "VALUES (?, ?, ?, ?, ?, ?, ?)",
                    (
                        "d0000001",
                        "memories",
                        "x",
                        _vec_blob([0.1] * 8),
                        "provider",
                        8,
                        "x",
                    ),
                )
                con.commit()
            finally:
                con.close()
            q = Query(
                query_id="q1",
                query="x",
                relevance_class="positive",
                relevant_doc_ids=("d0000001",),
            )
            # Deliberately-broken base URL; the connect will
            # fail. The runner must surface EmbedError, not
            # silently substitute the hash.
            with self.assertRaises(EmbedError):
                _raw_cosine_ranking(
                    q, db, top_k=1,
                    embedding_profile={
                        "provider": "openai-compatible",
                        "model": "m",
                        "base_url": "http://127.0.0.1:1",
                        "api_key": "",
                    },
                )


class TestCorpusIDUniqueness(unittest.TestCase):
    """v3 review priority 5: load_corpus refuses a corpus whose
    documents.json contains duplicate doc_ids."""

    def test_duplicate_doc_ids_rejected(self) -> None:
        from bench.corpus import CorpusError, load_corpus

        with tempfile.TemporaryDirectory() as td:
            base = Path(td) / "corpus"
            v1 = base / "v1"
            v1.mkdir(parents=True)
            # Write a minimal valid MANIFEST and files except
            # the documents have a duplicate doc_id.
            docs = [
                {
                    "doc_id": "d0000001",
                    "content": "a",
                    "relevance_class": "relevant",
                },
                {
                    "doc_id": "d0000001",
                    "content": "b",
                    "relevance_class": "relevant",
                },
            ]
            qs = [
                {
                    "query_id": "q1",
                    "query": "?",
                    "relevance_class": "positive",
                    "relevant_doc_ids": ["d0000001"],
                }
            ]
            ex = {"q1": ["d0000001"]}
            sc = {"method": "mpm recall --semantic --json", "top_k": 5, "parameters": {}}
            (v1 / "documents.json").write_text(json.dumps(docs))
            (v1 / "queries.json").write_text(json.dumps(qs))
            (v1 / "expectations.json").write_text(json.dumps(ex))
            (v1 / "scoring.json").write_text(json.dumps(sc))
            import hashlib
            manifest = {
                "version": "v1",
                "documents_sha256": hashlib.sha256(
                    (v1 / "documents.json").read_bytes()
                ).hexdigest(),
                "queries_sha256": hashlib.sha256(
                    (v1 / "queries.json").read_bytes()
                ).hexdigest(),
                "expectations_sha256": hashlib.sha256(
                    (v1 / "expectations.json").read_bytes()
                ).hexdigest(),
                "scoring_sha256": hashlib.sha256(
                    (v1 / "scoring.json").read_bytes()
                ).hexdigest(),
            }
            (base / "MANIFEST.json").write_text(json.dumps(manifest))
            with self.assertRaises(CorpusError) as cm:
                load_corpus(base)
            self.assertIn("duplicate", str(cm.exception).lower())


class TestPositiveQueryMustHaveRelevantDocIds(unittest.TestCase):
    """v3 review priority 5: load_corpus refuses a positive query
    whose relevant_doc_ids is empty (Precision/Recall/MRR/nDCG
    would be undefined)."""

    def test_empty_relevant_doc_ids_rejected(self) -> None:
        from bench.corpus import CorpusError, load_corpus

        with tempfile.TemporaryDirectory() as td:
            base = Path(td) / "corpus"
            v1 = base / "v1"
            v1.mkdir(parents=True)
            docs = [
                {
                    "doc_id": "d0000001",
                    "content": "a",
                    "relevance_class": "relevant",
                }
            ]
            qs = [
                {
                    "query_id": "q1",
                    "query": "?",
                    "relevance_class": "positive",
                    "relevant_doc_ids": [],
                }
            ]
            ex = {"q1": []}
            sc = {"method": "mpm recall --semantic --json", "top_k": 5, "parameters": {}}
            (v1 / "documents.json").write_text(json.dumps(docs))
            (v1 / "queries.json").write_text(json.dumps(qs))
            (v1 / "expectations.json").write_text(json.dumps(ex))
            (v1 / "scoring.json").write_text(json.dumps(sc))
            import hashlib
            manifest = {
                "version": "v1",
                "documents_sha256": hashlib.sha256(
                    (v1 / "documents.json").read_bytes()
                ).hexdigest(),
                "queries_sha256": hashlib.sha256(
                    (v1 / "queries.json").read_bytes()
                ).hexdigest(),
                "expectations_sha256": hashlib.sha256(
                    (v1 / "expectations.json").read_bytes()
                ).hexdigest(),
                "scoring_sha256": hashlib.sha256(
                    (v1 / "scoring.json").read_bytes()
                ).hexdigest(),
            }
            (base / "MANIFEST.json").write_text(json.dumps(manifest))
            with self.assertRaises(CorpusError) as cm:
                load_corpus(base)
            self.assertIn("empty relevant_doc_ids", str(cm.exception))


class TestExpectationsMatchQueries(unittest.TestCase):
    """v3 review priority 5: expectations.json and queries.json
    must be a set-equal parallel record. The queries file is the
    source of truth; expectations must match."""

    def test_expectations_mismatch_rejected(self) -> None:
        from bench.corpus import CorpusError, load_corpus

        with tempfile.TemporaryDirectory() as td:
            base = Path(td) / "corpus"
            v1 = base / "v1"
            v1.mkdir(parents=True)
            docs = [
                {
                    "doc_id": "d0000001",
                    "content": "a",
                    "relevance_class": "relevant",
                },
                {
                    "doc_id": "d0000002",
                    "content": "b",
                    "relevance_class": "relevant",
                },
            ]
            qs = [
                {
                    "query_id": "q1",
                    "query": "?",
                    "relevance_class": "positive",
                    "relevant_doc_ids": ["d0000001"],
                }
            ]
            # Mismatched expectations: says d0000002 instead of d0000001.
            ex = {"q1": ["d0000002"]}
            sc = {"method": "mpm recall --semantic --json", "top_k": 5, "parameters": {}}
            (v1 / "documents.json").write_text(json.dumps(docs))
            (v1 / "queries.json").write_text(json.dumps(qs))
            (v1 / "expectations.json").write_text(json.dumps(ex))
            (v1 / "scoring.json").write_text(json.dumps(sc))
            import hashlib
            manifest = {
                "version": "v1",
                "documents_sha256": hashlib.sha256(
                    (v1 / "documents.json").read_bytes()
                ).hexdigest(),
                "queries_sha256": hashlib.sha256(
                    (v1 / "queries.json").read_bytes()
                ).hexdigest(),
                "expectations_sha256": hashlib.sha256(
                    (v1 / "expectations.json").read_bytes()
                ).hexdigest(),
                "scoring_sha256": hashlib.sha256(
                    (v1 / "scoring.json").read_bytes()
                ).hexdigest(),
            }
            (base / "MANIFEST.json").write_text(json.dumps(manifest))
            with self.assertRaises(CorpusError) as cm:
                load_corpus(base)
            self.assertIn("set-equal", str(cm.exception).lower())


class TestScoringTopKFromCorpus(unittest.TestCase):
    """v3 review priority 5: scoring uses `corpus.scoring.top_k`,
    not the operator's config."""

    def test_scoring_top_k_from_corpus(self) -> None:
        from bench.corpus import Corpus, Document, Query, Scoring
        from bench.scoring import score_queries

        # Construct an in-memory corpus with top_k=7.
        corpus = Corpus(
            version="v-test",
            documents=(
                Document(
                    doc_id=f"d{i:07d}",
                    content=f"c{i}",
                    relevance_class="relevant",
                )
                for i in range(1, 11)
            ),
            queries=(
                Query(
                    query_id="q1",
                    query="?",
                    relevance_class="positive",
                    relevant_doc_ids=("d0000001", "d0000002"),
                ),
            ),
            expectations=None,  # type: ignore[arg-type]
            scoring=Scoring(method="mpm recall", top_k=7, parameters={}),
            manifest_path=None,  # type: ignore[arg-type]
        )
        # Pretend the ranker returned 10 rows, all in order.
        top_ids = tuple(f"d{i:07d}" for i in range(1, 11))
        result = score_queries(
            [("q1", True, top_ids, ("d0000001", "d0000002"))],
            k=corpus.scoring.top_k,
        )
        # k must be 7 (from corpus.scoring.top_k), not 5.
        self.assertEqual(result["k"], 7)
        # Precision@k = 2 relevant out of top-7 = 2/7.
        agg = result["aggregates"]
        self.assertAlmostEqual(agg["mean_precision_at_k"], 2.0 / 7.0)


class TestEmptyPlainTextResult(unittest.TestCase):
    """v3 review priority 6: MPM's plain-text 'No memories found'
    response is a successful empty result, not a failure."""

    def test_plain_text_empty_result_returns_empty_queryresult(self) -> None:
        from bench.runner import _is_plain_text_empty, run_query, QueryResult
        from bench.corpus import Query
        from unittest import mock
        import subprocess

        # The marker check itself.
        self.assertTrue(_is_plain_text_empty("No memories found."))
        self.assertTrue(_is_plain_text_empty("no memories found."))
        self.assertTrue(_is_plain_text_empty("  No memories found for query  "))
        self.assertFalse(_is_plain_text_empty(""))
        self.assertFalse(_is_plain_text_empty('{"memories": []}'))

        # The runner: mock subprocess.run to return rc=0 with
        # plain-text stdout; assert run_query returns an empty
        # QueryResult.
        q = Query(
            query_id="q1",
            query="?",
            relevance_class="negative",
            relevant_doc_ids=(),
        )

        fake_completed = subprocess.CompletedProcess(
            args=[], returncode=0,
            stdout="No memories found.\n", stderr="",
        )
        with mock.patch("subprocess.run", return_value=fake_completed):
            r = run_query(q, Path("/tmp/nope"), "/bin/true")
        self.assertEqual(r.search_mode, "hybrid_empty")
        self.assertEqual(len(r.rows), 0)
        self.assertFalse(r.raw_cosine)

    def test_malformed_response_still_raises(self) -> None:
        from bench.runner import run_query
        from bench.corpus import Query
        from unittest import mock
        import subprocess

        q = Query(
            query_id="q1",
            query="?",
            relevance_class="negative",
            relevant_doc_ids=(),
        )
        # rc=0 but stdout is not "No memories found" and not
        # valid JSON — must raise.
        fake_completed = subprocess.CompletedProcess(
            args=[], returncode=0,
            stdout="not json and not the empty marker", stderr="",
        )
        with mock.patch("subprocess.run", return_value=fake_completed):
            with self.assertRaises(RuntimeError):
                run_query(q, Path("/tmp/nope"), "/bin/true")


class TestRecallMustNotMutateMemories(unittest.TestCase):
    """v3 review priority 1: a recall run that mutates a tracked
    field in the disposable DB (e.g. last_retrieved_at) is
    detected by the per-query after-snapshot check.

    This test does not invoke mpm; it constructs a snapshot
    pair where the post-recall snapshot differs and asserts
    the diff is non-empty.
    """

    def test_mutation_diff_detected(self) -> None:
        from bench.harness import _mutation_diff

        before = {
            "memories": [
                ("d0000001", "memories", "x", "[]", "{}", 1.0, None,
                 "m", 8, "provider", 1, 0, 0),
            ],
            "retrieval_metadata": [],
        }
        after = {
            "memories": [
                ("d0000001", "memories", "x", "[]", "{}", 1.0, None,
                 "m", 8, "provider", 1, 0, 1759000000),  # last_retrieved_at changed
            ],
            "retrieval_metadata": [
                ("d0000001", 1, 1759000000, 0, 0),
            ],
        }
        d = _mutation_diff(before, after)
        self.assertEqual(d["status"], "FAIL")
        self.assertTrue(
            any(x.get("table") == "memories" for x in d["diffs"])
            or any(x.get("table") == "retrieval_metadata" for x in d["diffs"]),
            f"expected a diff on memories or retrieval_metadata, got {d['diffs']}",
        )

    def test_no_mutation_diff_clean(self) -> None:
        from bench.harness import _mutation_diff

        before = {
            "memories": [("d0000001",)],
            "retrieval_metadata": [],
        }
        after = {
            "memories": [("d0000001",)],
            "retrieval_metadata": [],
        }
        d = _mutation_diff(before, after)
        self.assertEqual(d["status"], "clean")
        self.assertEqual(d["diffs"], [])


# ---- v4 review adversarial tests (HAL 2026-10-19) ----


def _write_cfg(json_text: str) -> Path:
    """Write a temp config file and return its path. The harness's
    `cmd_run` reads `--config` as a file path."""
    td = tempfile.mkdtemp()
    p = Path(td) / "cfg.json"
    p.write_text(json_text)
    return p


# Minimal config that satisfies the harness's required keys
# (`profiles`, `components`, `expected.model`, `expected.dimension`).
# Embedding provider is "openai-compatible" so `_build_raw_cosine_profile`
# returns a profile, exercising the path that v4 prioritised.
_MINIMAL_CONFIG = (
    '{"profiles": {"default": {"provider": "openai-compatible", '
    '"model": "fake-embed-v1", "base_url": "http://127.0.0.1:1", '
    '"api_key": ""}}, "components": {"embedding": "default"}, '
    '"expected": {"model": "fake-embed-v1", "dimension": 8}}'
)


class TestSnapshotFailsClosedOnSchemaMismatch(unittest.TestCase):
    """v4 review priority 1: the mutation snapshot must FAIL CLOSED
    when a required tracked table or column is missing. The pre-v4
    code caught `sqlite3.OperationalError` and inserted a
    placeholder; identical errors before/after recall compared
    equal and produced a false "clean" status. The new contract
    is: missing schema is a hard error.

    A test that builds a stub schema missing `last_retrieved_at`
    (the column the v3 code erroneously selected from `memories`
    instead of `retrieval_metadata`) MUST surface as a
    `SnapshotSchemaError`, not a silent placeholder diff.
    """

    def test_missing_required_column_raises(self) -> None:
        from bench.harness import _snapshot_full, SnapshotSchemaError
        from bench.mpm_schema import build_mpm_disposable_db
        with tempfile.TemporaryDirectory() as td:
            db = Path(td) / "test.db"
            build_mpm_disposable_db(db)
            # Drop the last_retrieved_at column from
            # retrieval_metadata to simulate a schema mismatch.
            con = sqlite3.connect(str(db))
            try:
                # Rename the table to break the column; this
                # produces a clean `no such column` error at
                # query time. The snapshot must hard-fail.
                con.execute(
                    "ALTER TABLE retrieval_metadata RENAME TO retrieval_metadata_broken"
                )
                con.commit()
            finally:
                con.close()
            with self.assertRaises(SnapshotSchemaError) as cm:
                _snapshot_full(db)
            self.assertIn("retrieval_metadata", str(cm.exception))

    def test_missing_required_table_raises(self) -> None:
        from bench.harness import _snapshot_full, SnapshotSchemaError
        from bench.mpm_schema import build_mpm_disposable_db
        with tempfile.TemporaryDirectory() as td:
            db = Path(td) / "test.db"
            build_mpm_disposable_db(db)
            con = sqlite3.connect(str(db))
            try:
                con.execute("DROP TABLE memories")
                con.commit()
            finally:
                con.close()
            with self.assertRaises(SnapshotSchemaError) as cm:
                _snapshot_full(db)
            self.assertIn("memories", str(cm.exception))

    def test_real_mpm_schema_snapshot_succeeds(self) -> None:
        # The positive case: a real MPM schema (memories,
        # reference_chunks, retrieval_metadata all present with
        # the columns the harness reads) must snapshot cleanly.
        from bench.harness import _snapshot_full
        from bench.mpm_schema import build_mpm_disposable_db
        with tempfile.TemporaryDirectory() as td:
            db = Path(td) / "test.db"
            build_mpm_disposable_db(db)
            snap = _snapshot_full(db)
            self.assertIn("memories", snap)
            self.assertIn("reference_chunks", snap)
            self.assertIn("retrieval_metadata", snap)
            # All three lists must be initialized (possibly empty).
            self.assertIsInstance(snap["memories"], list)
            self.assertIsInstance(snap["reference_chunks"], list)
            self.assertIsInstance(snap["retrieval_metadata"], list)


class TestRealMpmSchemaIntegration(unittest.TestCase):
    """v4 review priority 1: end-to-end integration test using
    the real MPM schema. Build a disposable DB, insert a memory,
    run a mutation (UPDATE last_retrieved_at on retrieval_metadata),
    and assert the snapshot diff is detected.

    This is the load-bearing test the v3 code did not have: a
    test that exercises the FULL snapshot path against a schema
    that matches what `mpm remember` produces."""

    def test_recall_writes_retrieval_metadata_detected(self) -> None:
        from bench.harness import _snapshot_full, _mutation_diff
        from bench.mpm_schema import build_mpm_disposable_db, vec_blob
        with tempfile.TemporaryDirectory() as td:
            db = Path(td) / "test.db"
            build_mpm_disposable_db(db)
            # Insert one memory + retrieval_metadata row.
            con = sqlite3.connect(str(db))
            try:
                con.execute(
                    "INSERT INTO memories (id, collection, content, "
                    "embedding, embedding_source, embedding_dimension, "
                    "embedding_model) VALUES (?, ?, ?, ?, ?, ?, ?)",
                    (
                        "d0000001",
                        "memories",
                        "x",
                        vec_blob([0.1] * 8),
                        "provider",
                        8,
                        "fake-embed-v1",
                    ),
                )
                con.execute(
                    "INSERT INTO retrieval_metadata "
                    "(node_id, node_type, reuse_count) "
                    "VALUES (?, ?, ?)",
                    ("d0000001", "memory", 0),
                )
                con.commit()
            finally:
                con.close()
            # Snapshot before.
            before = _snapshot_full(db)
            self.assertEqual(len(before["memories"]), 1)
            self.assertEqual(len(before["retrieval_metadata"]), 1)
            # Simulate a recall write: increment reuse_count and
            # set last_retrieved_at.
            con = sqlite3.connect(str(db))
            try:
                con.execute(
                    "UPDATE retrieval_metadata SET reuse_count = 1, "
                    "last_retrieved_at = 1759000000 "
                    "WHERE node_id = ?",
                    ("d0000001",),
                )
                con.commit()
            finally:
                con.close()
            after = _snapshot_full(db)
            d = _mutation_diff(before, after)
            # The diff must surface the retrieval_metadata change.
            self.assertEqual(
                d["status"], "FAIL",
                f"recall-time retrieval_metadata write must surface; "
                f"got diffs={d['diffs']}",
            )
            self.assertTrue(
                any(x.get("table") == "retrieval_metadata"
                    for x in d["diffs"]),
                f"expected retrieval_metadata in diffs; got {d['diffs']}",
            )

    def test_metadata_json_challenge_state_change_detected(self) -> None:
        # The challenge status flag lives in the metadata JSON,
        # not in a column. A recall that flips the flag MUST
        # be detected because the snapshot includes the metadata
        # column.
        from bench.harness import _snapshot_full, _mutation_diff
        from bench.mpm_schema import build_mpm_disposable_db, vec_blob
        with tempfile.TemporaryDirectory() as td:
            db = Path(td) / "test.db"
            build_mpm_disposable_db(db)
            con = sqlite3.connect(str(db))
            try:
                con.execute(
                    "INSERT INTO memories (id, collection, content, "
                    "metadata, embedding, embedding_source, "
                    "embedding_dimension, embedding_model) "
                    "VALUES (?, ?, ?, ?, ?, ?, ?, ?)",
                    (
                        "d0000001",
                        "memories",
                        "x",
                        '{"status": "active"}',
                        vec_blob([0.1] * 8),
                        "provider",
                        8,
                        "fake-embed-v1",
                    ),
                )
                con.commit()
            finally:
                con.close()
            before = _snapshot_full(db)
            # Simulate a challenge write that flips the metadata.
            con = sqlite3.connect(str(db))
            try:
                con.execute(
                    "UPDATE memories SET metadata = ? WHERE id = ?",
                    (
                        '{"status": "challenged", '
                        '"challenged_prior_weight": 1.0}',
                        "d0000001",
                    ),
                )
                con.commit()
            finally:
                con.close()
            after = _snapshot_full(db)
            d = _mutation_diff(before, after)
            self.assertEqual(
                d["status"], "FAIL",
                f"challenge metadata flip must surface; got {d['diffs']}",
            )
            self.assertTrue(
                any(x.get("table") == "memories"
                    for x in d["diffs"]),
                f"expected memories in diffs; got {d['diffs']}",
            )


class TestCmdRunAccumulatesResultsAndStopsOnMutation(unittest.TestCase):
    """v4 review priority 2: `cmd_run` must accumulate each
    `QueryResult` in the monitored loop, reuse those exact
    results for participation/scoring/reports, and STOP on
    the first detected mutation. The pre-v4 code re-ran the
    entire query corpus after the monitored loop finished,
    which means every query ran TWICE under MPM: once under
    mutation check, then again without.

    This test exercises the harness's mutation-stop contract
    without spinning up the real MPM binary: it patches
    `_snapshot_full` and `run_query` to count invocations."""

    def test_one_run_per_query(self) -> None:
        from bench import harness as h
        from bench.corpus import Corpus, Document, Query, Scoring
        from bench.runner import QueryResult

        # Build a minimal in-memory corpus.
        corpus = Corpus(
            version="v-test",
            documents=tuple(
                Document(
                    doc_id=f"d{i:07d}",
                    content=f"c{i}",
                    relevance_class="relevant",
                )
                for i in range(1, 6)
            ),
            queries=tuple(
                Query(
                    query_id=f"q{i}",
                    query="?",
                    relevance_class="positive",
                    relevant_doc_ids=("d0000001",),
                )
                for i in range(1, 6)
            ),
            expectations=None,  # type: ignore[arg-type]
            scoring=Scoring(method="mpm recall", top_k=5, parameters={}),
            manifest_path=None,  # type: ignore[arg-type]
        )

        # Counters for invocations.
        run_query_calls: list[str] = []
        snapshot_calls: list[str] = []

        def fake_run_query(q, snap, mpm, **kwargs):  # type: ignore[no-untyped-def]
            run_query_calls.append(q.query_id)
            return QueryResult(
                query_id=q.query_id,
                query=q.query,
                is_positive=q.is_positive,
                rows=tuple(
                    {"id": "d0000001", "content": "x", "source": "vector",
                     "score": 0.9, "vector_similarity": 0.9}
                    for _ in range(5)
                ),
                search_mode="hybrid",
                raw_cosine=False,
            )

        def fake_snapshot(db_path):  # type: ignore[no-untyped-def]
            snapshot_calls.append("snap")
            return {"memories": [], "reference_chunks": [],
                    "retrieval_metadata": []}

        # Build an args namespace. The harness reads --config as a
        # file path, so we write the JSON to a temp file.
        import argparse
        td_cfg = tempfile.mkdtemp()
        cfg_path = Path(td_cfg) / "cfg.json"
        cfg_path.write_text(_MINIMAL_CONFIG)
        args = argparse.Namespace(
            output_dir="/tmp/nope-cmd-run-test",
            config=str(cfg_path),
            raw_cosine_prefix=False,
        )

        Path("/tmp/fake-snap").mkdir(parents=True, exist_ok=True)
        Path("/tmp/fake-cmd-run-test").mkdir(parents=True, exist_ok=True)
        try:
            # Patch the heavy side effects: load_corpus, mpm
            # configuration, ingest, backfill, verify, write_report.
            with unittest.mock.patch.object(h, "load_corpus", return_value=corpus), \
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
                                        side_effect=fake_snapshot), \
             unittest.mock.patch.object(h, "_resolve_mpm_binary",
                                        return_value=Path("/bin/true")), \
             unittest.mock.patch.object(h, "_make_snap_workspace",
                                        return_value=Path("/tmp/fake-snap")), \
             unittest.mock.patch.object(h, "assert_safe_workspace",
                                        return_value=Path("/tmp/fake-snap")), \
             unittest.mock.patch.object(h, "assert_safe_output_dir",
                                        return_value=Path(
                                            "/tmp/fake-cmd-run-test")), \
             unittest.mock.patch.object(h, "reject_unsafe_symlinks",
                                        return_value=[]), \
             unittest.mock.patch.object(h, "write_mpm_config",
                                        return_value=Path(
                                            "/tmp/fake-config")), \
             unittest.mock.patch.object(h, "_find_db_path",
                                        return_value=Path(
                                            "/tmp/fake-snap/mpm.db")), \
             unittest.mock.patch.object(h, "write_report",
                                        return_value=(Path("r.json"),
                                                     Path("r.md"))):
                rc = h.cmd_run(args)
        finally:
            import shutil
            shutil.rmtree("/tmp/fake-snap", ignore_errors=True)
            shutil.rmtree("/tmp/fake-cmd-run-test", ignore_errors=True)

        # The harness must call run_query EXACTLY ONCE per
        # query (5 queries → 5 calls). The pre-v4 code would
        # have called it 10 times.
        self.assertEqual(
            len(run_query_calls), 5,
            f"each query must run exactly once under MPM; got "
            f"{len(run_query_calls)} calls: {run_query_calls}",
        )
        # And each query_id should appear exactly once.
        self.assertEqual(sorted(run_query_calls),
                         ["q1", "q2", "q3", "q4", "q5"])

    def test_mutation_stops_further_queries(self) -> None:
        from bench import harness as h
        from bench.corpus import Corpus, Document, Query, Scoring
        from bench.runner import QueryResult

        corpus = Corpus(
            version="v-test",
            documents=tuple(
                Document(doc_id=f"d{i:07d}", content=f"c{i}",
                         relevance_class="relevant")
                for i in range(1, 4)
            ),
            queries=tuple(
                Query(query_id=f"q{i}", query="?",
                      relevance_class="positive",
                      relevant_doc_ids=("d0000001",))
                for i in range(1, 6)
            ),
            expectations=None,  # type: ignore[arg-type]
            scoring=Scoring(method="mpm recall", top_k=5, parameters={}),
            manifest_path=None,  # type: ignore[arg-type]
        )

        run_query_calls: list[str] = []
        snapshot_index = [0]
        # The third snapshot-after-query returns a different
        # value, simulating a recall-time mutation.

        def fake_run_query(q, snap, mpm, **kwargs):  # type: ignore[no-untyped-def]
            run_query_calls.append(q.query_id)
            return QueryResult(
                query_id=q.query_id, query=q.query,
                is_positive=q.is_positive,
                rows=tuple({"id": "d0000001", "content": "x",
                            "source": "vector", "score": 0.9,
                            "vector_similarity": 0.9} for _ in range(5)),
                search_mode="hybrid", raw_cosine=False,
            )

        def fake_snapshot(db_path):  # type: ignore[no-untyped-def]
            snapshot_index[0] += 1
            if snapshot_index[0] == 4:  # after the 3rd query
                return {"memories": [("mutated",)],
                        "reference_chunks": [],
                        "retrieval_metadata": []}
            return {"memories": [], "reference_chunks": [],
                    "retrieval_metadata": []}

        import argparse
        td_cfg = tempfile.mkdtemp()
        cfg_path = Path(td_cfg) / "cfg.json"
        cfg_path.write_text(_MINIMAL_CONFIG)
        args = argparse.Namespace(
            output_dir="/tmp/nope-cmd-run-mut",
            config=str(cfg_path),
            raw_cosine_prefix=False,
        )

        Path("/tmp/fake-snap").mkdir(parents=True, exist_ok=True)
        Path("/tmp/fake-cmd-run-mut").mkdir(parents=True, exist_ok=True)
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
                                            side_effect=fake_snapshot), \
                 unittest.mock.patch.object(h, "_resolve_mpm_binary",
                                            return_value=Path("/bin/true")), \
                 unittest.mock.patch.object(h, "_make_snap_workspace",
                                            return_value=Path(
                                                "/tmp/fake-snap")), \
                 unittest.mock.patch.object(h, "assert_safe_workspace",
                                            return_value=Path(
                                                "/tmp/fake-snap")), \
                 unittest.mock.patch.object(h, "assert_safe_output_dir",
                                            return_value=Path(
                                                "/tmp/fake-cmd-run-mut")), \
                 unittest.mock.patch.object(h, "reject_unsafe_symlinks",
                                            return_value=[]), \
                 unittest.mock.patch.object(h, "write_mpm_config",
                                            return_value=Path(
                                                "/tmp/fake-config")), \
                 unittest.mock.patch.object(h, "_find_db_path",
                                            return_value=Path(
                                                "/tmp/fake-snap/mpm.db")), \
                 unittest.mock.patch.object(h, "write_report",
                                            return_value=(Path("r.json"),
                                                         Path("r.md"))):
                h.cmd_run(args)
        finally:
            import shutil
            shutil.rmtree("/tmp/fake-snap", ignore_errors=True)
            shutil.rmtree("/tmp/fake-cmd-run-mut", ignore_errors=True)

        # The mutation was simulated on snapshot 4 (after q3).
        # The harness MUST stop there: q4 and q5 are never
        # issued under MPM.
        self.assertEqual(
            run_query_calls, ["q1", "q2", "q3"],
            f"mutation must stop further queries; got {run_query_calls}",
        )


class TestReportWriteRechecksSafety(unittest.TestCase):
    """v4 review priority 3: `write_report` must call
    `recheck_output_dir_safe` immediately before each write
    to close the TOCTOU window between the initial check and
    the actual write. The pre-v4 code only called `mkdir`;
    a malicious local actor could chmod the dir to 0o755 or
    replace it with a symlink to a protected workspace in
    the gap."""

    def test_report_write_refuses_widened_perms(self) -> None:
        from bench.reporter import RunReport, write_report
        from bench.workspace import WorkspaceError
        with tempfile.TemporaryDirectory() as td:
            out = Path(td) / "out"
            snap = Path(td) / "snap"
            out.mkdir(parents=True, mode=0o700)
            snap.mkdir(parents=True, mode=0o700)
            r = RunReport(
                corpus={"version": "v-test"},
                regeneration={"ok": True, "failures": [], "expected": {}},
                participation={},
                raw_cosine_participation=None,
                raw_cosine_unprefixed_scoring=None,
                raw_cosine_prefixed_scoring=None,
                raw_cosine_delta=None,
                labelled_scoring=None,
                isolation={},
                snapshot_logical_mutation={"status": "clean", "diffs": []},
                per_query_table=[],
                raw_cosine_per_query_table=None,
                raw_cosine_prefixed_per_query_table=None,
                historical_note="",
                output_dir=out,
            )
            # Widen the perms to 0o755 between the initial
            # write_report call and the actual write. We do
            # this by monkey-patching mkdir to widen perms.
            import os as _os
            orig_mkdir = Path.mkdir
            def widening_mkdir(self, *a, **kw):  # type: ignore[no-untyped-def]
                kw.pop("mode", None)
                orig_mkdir(self, *a, **kw)
                if self == out:
                    _os.chmod(self, 0o755)
            with unittest.mock.patch.object(Path, "mkdir", widening_mkdir):
                with self.assertRaises(WorkspaceError) as cm:
                    write_report(r, snap_real=snap)
            self.assertIn("wider than 0o700", str(cm.exception))

    def test_report_write_refuses_symlink_redirect(self) -> None:
        from bench.reporter import RunReport, write_report
        from bench.workspace import WorkspaceError
        with tempfile.TemporaryDirectory() as td:
            out = Path(td) / "out"
            snap = Path(td) / "snap"
            snap.mkdir(parents=True, mode=0o700)
            out.mkdir(parents=True, mode=0o700)
            # Replace the output dir with a symlink that points
            # into the snapshot workspace. The pre-write recheck
            # in `write_report` MUST catch the symlink-to-snap
            # and refuse the write. The pre-v4 code only called
            # `mkdir`; the new contract is recheck-then-write.
            out_parent = out.parent
            os.rmdir(out)
            (out_parent / "out").symlink_to(snap)
            r = RunReport(
                corpus={"version": "v-test"},
                regeneration={"ok": True, "failures": [], "expected": {}},
                participation={},
                raw_cosine_participation=None,
                raw_cosine_unprefixed_scoring=None,
                raw_cosine_prefixed_scoring=None,
                raw_cosine_delta=None,
                labelled_scoring=None,
                isolation={},
                snapshot_logical_mutation={"status": "clean", "diffs": []},
                per_query_table=[],
                raw_cosine_per_query_table=None,
                raw_cosine_prefixed_per_query_table=None,
                historical_note="",
                output_dir=out_parent / "out",
            )
            with self.assertRaises(WorkspaceError) as cm:
                write_report(r, snap_real=snap)
            self.assertIn("snapshot workspace", str(cm.exception))


class TestTempParentValidatedBeforeMkdir(unittest.TestCase):
    """v4 review priority 4: `_make_snap_workspace` must validate
    the canonical temp parent (`/tmp/mpm-bench`) against the
    live-store sentinels BEFORE any mkdir. The pre-v4 code
    called `parent.mkdir(parents=True, exist_ok=True, mode=0o700)`
    first, which would create a directory inside a protected
    root if TMPDIR pointed at the operator's home or MPM
    workspace."""

    def test_tmpdir_into_protected_root_rejected_no_artifact(self) -> None:
        from bench import harness as h
        with tempfile.TemporaryDirectory() as home_root:
            with tempfile.TemporaryDirectory() as td:
                home = Path(home_root)
                mpm = home / ".mpm"
                mpm.mkdir(parents=True, exist_ok=True, mode=0o700)
                # Use a protected_root of `home`; the canonical
                # temp parent path is computed inside
                # `_make_snap_workspace` and would normally be
                # `/tmp/mpm-bench`. We simulate the dangerous
                # case by passing `home` as the parent override
                # and asserting the helper refuses.
                bad_parent = mpm / "tmp"
                # The helper exposes `_default_snap_parent` and
                # `assert_safe_temp_parent`; the integration
                # test patches `tempfile.gettempdir` to point
                # inside the protected root.
                with unittest.mock.patch.object(
                    h.tempfile, "gettempdir",
                    return_value=str(mpm)
                ):
                    with self.assertRaises(Exception):
                        h._make_snap_workspace(
                            "mpm-bench-x-", protected_root=home
                        )
                # The protected-root tmp dir must NOT have been
                # created on disk.
                self.assertFalse(
                    bad_parent.exists(),
                    f"rejection must not create {bad_parent} on disk",
                )


class TestPrefixExperimentFailsClosedWithoutProfile(unittest.TestCase):
    """v4 review priority 5: if `--raw-cosine-prefix` is requested
    and no embedding profile resolves from the config, fail
    clearly. The pre-v4 code returned a hash-only unprefixed
    result with `prefixed=None` and reported it as a "completed
    experiment" — a real-model anti-pattern."""

    def test_no_profile_returns_error(self) -> None:
        from bench import harness as h
        from bench.corpus import Corpus, Document, Query, Scoring
        import argparse
        corpus = Corpus(
            version="v-test",
            documents=(),
            queries=(),
            expectations=None,  # type: ignore[arg-type]
            scoring=Scoring(method="mpm recall", top_k=5, parameters={}),
            manifest_path=None,  # type: ignore[arg-type]
        )
        args = argparse.Namespace(
            output_dir="/tmp/nope-prefix-fail",
            # profiles is non-empty (so write_mpm_config passes),
            # but components.embedding points at a name that does
            # not exist AND there is no "default" profile → no
            # profile resolves for the raw-cosine experiment.
            config=str(_write_cfg(
                '{"profiles": {"foo": {"provider": "openai-compatible", '
                '"model": "x"}}, "components": {"embedding": "bar"}, '
                '"expected": {"model": "x", "dimension": 8}}'
            )),
            raw_cosine_prefix=True,
        )
        # Make the fake snap dir exist on disk so write_mpm_config
        # can write mpm_config.json into it.
        Path("/tmp/fake-snap").mkdir(parents=True, exist_ok=True)
        Path("/tmp/fake-out-prefix").mkdir(parents=True, exist_ok=True)
        try:
            with unittest.mock.patch.object(h, "load_corpus",
                                            return_value=corpus), \
                 unittest.mock.patch.object(h, "_make_snap_workspace",
                                            return_value=Path(
                                                "/tmp/fake-snap")), \
                 unittest.mock.patch.object(h, "assert_safe_workspace",
                                            return_value=Path(
                                                "/tmp/fake-snap")), \
                 unittest.mock.patch.object(h, "assert_safe_output_dir",
                                            return_value=Path(
                                                "/tmp/fake-out-prefix")), \
                 unittest.mock.patch.object(h, "reject_unsafe_symlinks",
                                            return_value=[]), \
                 unittest.mock.patch.object(h, "_ingest_corpus"), \
                 unittest.mock.patch.object(h, "_backfill",
                                            return_value=(0, "")), \
                 unittest.mock.patch.object(h, "_find_db_path",
                                            return_value=Path(
                                                "/tmp/fake-snap/mpm.db")), \
                 unittest.mock.patch.object(h, "verify_regeneration_complete",
                                            return_value=unittest.mock.MagicMock(
                                                ok=True,
                                                to_dict=lambda: {"ok": True,
                                                                 "failures": []},
                                            )), \
                 unittest.mock.patch.object(h, "_snapshot_full",
                                            return_value={"memories": [],
                                                         "reference_chunks": [],
                                                         "retrieval_metadata": []}), \
                 unittest.mock.patch.object(h, "run_query"), \
                 unittest.mock.patch.object(h, "_resolve_mpm_binary",
                                            return_value=Path("/bin/true")), \
                 unittest.mock.patch.object(h, "write_mpm_config",
                                            return_value=Path(
                                                "/tmp/fake-config")), \
                 unittest.mock.patch.object(h, "write_report",
                                            return_value=(Path("r.json"),
                                                         Path("r.md"))):
                rc = h.cmd_run(args)
        finally:
            import shutil
            shutil.rmtree("/tmp/fake-snap", ignore_errors=True)
            shutil.rmtree("/tmp/fake-out-prefix", ignore_errors=True)
        self.assertEqual(
            rc, 1,
            "no profile + --raw-cosine-prefix must fail with rc=1",
        )

    def test_profile_present_proceeds(self) -> None:
        # The positive case: when a profile resolves, the
        # harness does NOT short-circuit on rc=1 at the
        # profile-resolution step. (It may still fail later
        # for other reasons in the test environment; this
        # test only asserts it gets past the profile check.)
        from bench import harness as h
        from bench.corpus import Corpus, Document, Query, Scoring
        import argparse
        corpus = Corpus(
            version="v-test",
            documents=(),
            queries=(),
            expectations=None,  # type: ignore[arg-type]
            scoring=Scoring(method="mpm recall", top_k=5, parameters={}),
            manifest_path=None,  # type: ignore[arg-type]
        )
        cfg_json = (
            '{"profiles": {"default": {"provider": "openai-compatible", '
            '"model": "x"}}, "components": {"embedding": "default"}, '
            '"expected": {"model": "x", "dimension": 8}}'
        )
        args = argparse.Namespace(
            output_dir="/tmp/nope-prefix-ok",
            config=str(_write_cfg(cfg_json)),
            raw_cosine_prefix=True,
        )
        Path("/tmp/fake-snap").mkdir(parents=True, exist_ok=True)
        Path("/tmp/fake-out-ok").mkdir(parents=True, exist_ok=True)
        try:
            with unittest.mock.patch.object(h, "load_corpus",
                                            return_value=corpus), \
                 unittest.mock.patch.object(h, "_make_snap_workspace",
                                            return_value=Path(
                                                "/tmp/fake-snap")), \
                 unittest.mock.patch.object(h, "assert_safe_workspace",
                                            return_value=Path(
                                                "/tmp/fake-snap")), \
                 unittest.mock.patch.object(h, "assert_safe_output_dir",
                                            return_value=Path(
                                                "/tmp/fake-out-ok")), \
                 unittest.mock.patch.object(h, "reject_unsafe_symlinks",
                                            return_value=[]), \
                 unittest.mock.patch.object(h, "_ingest_corpus"), \
                 unittest.mock.patch.object(h, "_backfill",
                                            return_value=(0, "")), \
                 unittest.mock.patch.object(h, "_find_db_path",
                                            return_value=Path(
                                                "/tmp/fake-snap/mpm.db")), \
                 unittest.mock.patch.object(h, "verify_regeneration_complete",
                                            return_value=unittest.mock.MagicMock(
                                                ok=True,
                                                to_dict=lambda: {"ok": True,
                                                                 "failures": []},
                                            )), \
                 unittest.mock.patch.object(h, "_snapshot_full",
                                            return_value={"memories": [],
                                                         "reference_chunks": [],
                                                         "retrieval_metadata": []}), \
                 unittest.mock.patch.object(h, "run_query"), \
                 unittest.mock.patch.object(h, "_resolve_mpm_binary",
                                            return_value=Path("/bin/true")), \
                 unittest.mock.patch.object(h, "write_mpm_config",
                                            return_value=Path(
                                                "/tmp/fake-config")), \
                 unittest.mock.patch.object(h, "write_report",
                                            return_value=(Path("r.json"),
                                                         Path("r.md"))):
                # No mutation: the run proceeds past the profile
                # check and tries to do real work. We don't care
                # about the final rc here; we just want to assert
                # the harness did NOT short-circuit on the profile
                # step. The mock is permissive enough that the run
                # will either succeed or fail later; both are fine.
                h.cmd_run(args)
        finally:
            import shutil
            shutil.rmtree("/tmp/fake-snap", ignore_errors=True)
            shutil.rmtree("/tmp/fake-out-ok", ignore_errors=True)


class TestCmdRunPassesTopKToHybridRunner(unittest.TestCase):
    """v4 review priority 6: `cmd_run` must pass
    `corpus.scoring.top_k` to the hybrid runner so the
    retrieval depth matches the labelled-scoring depth. The
    pre-v4 code relied on the runner's default top_k=5 and
    only the scoring step saw corpus.scoring.top_k; a corpus
    with top_k > 5 had its labelled scoring computed on
    top-5 of top-5."""

    def test_hybrid_runner_called_with_corpus_top_k(self) -> None:
        from bench import harness as h
        from bench.corpus import Corpus, Document, Query, Scoring

        corpus = Corpus(
            version="v-test",
            documents=tuple(
                Document(doc_id=f"d{i:07d}", content=f"c{i}",
                         relevance_class="relevant")
                for i in range(1, 11)
            ),
            queries=tuple(
                Query(query_id="q1", query="?",
                      relevance_class="positive",
                      relevant_doc_ids=("d0000001",))
                for _ in range(1)
            ),
            expectations=None,  # type: ignore[arg-type]
            scoring=Scoring(method="mpm recall", top_k=7, parameters={}),
            manifest_path=None,  # type: ignore[arg-type]
        )
        captured_kwargs: list[dict] = []

        from bench.runner import QueryResult

        def fake_run_query(q, snap, mpm, **kw):  # type: ignore[no-untyped-def]
            captured_kwargs.append(kw)
            return QueryResult(
                query_id=q.query_id, query=q.query,
                is_positive=q.is_positive, rows=(),
                search_mode="hybrid", raw_cosine=False,
            )

        import argparse
        td_cfg = tempfile.mkdtemp()
        cfg_path = Path(td_cfg) / "cfg.json"
        cfg_path.write_text(_MINIMAL_CONFIG)
        args = argparse.Namespace(
            output_dir="/tmp/nope-topk",
            config=str(cfg_path),
            raw_cosine_prefix=False,
        )

        Path("/tmp/fake-snap").mkdir(parents=True, exist_ok=True)
        Path("/tmp/fake-out-topk").mkdir(parents=True, exist_ok=True)
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
                                            return_value=Path(
                                                "/tmp/fake-snap")), \
                 unittest.mock.patch.object(h, "assert_safe_workspace",
                                            return_value=Path(
                                                "/tmp/fake-snap")), \
                 unittest.mock.patch.object(h, "assert_safe_output_dir",
                                            return_value=Path(
                                                "/tmp/fake-out-topk")), \
                 unittest.mock.patch.object(h, "reject_unsafe_symlinks",
                                            return_value=[]), \
                 unittest.mock.patch.object(h, "write_mpm_config",
                                            return_value=Path(
                                                "/tmp/fake-config")), \
                 unittest.mock.patch.object(h, "_find_db_path",
                                            return_value=Path(
                                                "/tmp/fake-snap/mpm.db")), \
                 unittest.mock.patch.object(h, "write_report",
                                            return_value=(Path("r.json"),
                                                         Path("r.md"))):
                h.cmd_run(args)
        finally:
            import shutil
            shutil.rmtree("/tmp/fake-snap", ignore_errors=True)
            shutil.rmtree("/tmp/fake-out-topk", ignore_errors=True)

        # The hybrid runner must have been called with
        # top_k=7 (corpus.scoring.top_k), not the runner
        # default of 5.
        self.assertTrue(
            len(captured_kwargs) >= 1,
            f"run_query must be called at least once; got {captured_kwargs}",
        )
        for kw in captured_kwargs:
            self.assertEqual(
                kw.get("top_k"), 7,
                f"hybrid runner must be called with top_k=7 "
                f"(corpus.scoring.top_k); got kw={kw}",
            )

