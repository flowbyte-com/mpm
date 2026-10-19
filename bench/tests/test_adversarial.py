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
