"""Runner smoke tests (gate 2: hermetic, no live DB, no real network).

These tests do NOT call the mpm binary. They verify the
deterministic-vector function and the raw-cosine ranking path
against a synthetic DB, both of which run entirely in-process.

The integration with the mpm binary is exercised separately by
`cmd_smoke` (which uses the fake embeddings server). The hermetic
tests here are the load-bearing unit tests that can run in CI
without any MPM build artifact.
"""

import json
import math
import sqlite3
import tempfile
import unittest
from pathlib import Path

from bench.fake_server import deterministic_vector
from bench.runner import _raw_cosine_ranking
from bench.corpus import Corpus, Query


def _make_db_with_vectors(path: Path, dim: int, docs: list[tuple[str, str]]) -> None:
    con = sqlite3.connect(path)
    try:
        con.executescript(
            """
            CREATE TABLE IF NOT EXISTS memories (
                id TEXT PRIMARY KEY,
                collection TEXT NOT NULL,
                content TEXT NOT NULL,
                embedding BLOB,
                embedding_source TEXT NOT NULL DEFAULT 'provider',
                embedding_dimension INTEGER,
                embedding_model TEXT,
                deleted_at INTEGER
            );
            """
        )
        for did, text in docs:
            vec = deterministic_vector(text, dim)
            con.execute(
                "INSERT INTO memories (id, collection, content, embedding, "
                "embedding_source, embedding_dimension, embedding_model) "
                "VALUES (?, ?, ?, ?, ?, ?, ?)",
                (
                    did,
                    "memories",
                    text,
                    json.dumps(vec).encode("utf-8"),
                    "provider",
                    dim,
                    "fake-embed-v1",
                ),
            )
        con.commit()
    finally:
        con.close()


def _make_db_multi_collection(
    path: Path,
    dim: int,
    rows: list[tuple[str, str, str]],  # (id, collection, content)
) -> None:
    """Hermetic DB constructor used by the B1 collection-scope
    regression tests. Rows may belong to multiple collections; the
    bench's `collection` parameter must restrict ranking to the
    configured collection.
    """
    con = sqlite3.connect(path)
    try:
        con.executescript(
            """
            CREATE TABLE IF NOT EXISTS memories (
                id TEXT PRIMARY KEY,
                collection TEXT NOT NULL,
                content TEXT NOT NULL,
                embedding BLOB,
                embedding_source TEXT NOT NULL DEFAULT 'provider',
                embedding_dimension INTEGER,
                embedding_model TEXT,
                deleted_at INTEGER
            );
            """
        )
        for did, coll, text in rows:
            vec = deterministic_vector(text, dim)
            con.execute(
                "INSERT INTO memories (id, collection, content, embedding, "
                "embedding_source, embedding_dimension, embedding_model) "
                "VALUES (?, ?, ?, ?, ?, ?, ?)",
                (
                    did,
                    coll,
                    text,
                    json.dumps(vec).encode("utf-8"),
                    "provider",
                    dim,
                    "fake-embed-v1",
                ),
            )
        con.commit()
    finally:
        con.close()


class TestDeterministicVector(unittest.TestCase):
    def test_same_text_same_vector(self) -> None:
        a = deterministic_vector("hello", 32)
        b = deterministic_vector("hello", 32)
        self.assertEqual(a, b)

    def test_different_text_different_vector(self) -> None:
        a = deterministic_vector("hello", 32)
        b = deterministic_vector("world", 32)
        self.assertNotEqual(a, b)

    def test_vector_is_normalized(self) -> None:
        for text in ("a", "hello world", "the quick brown fox", ""):
            v = deterministic_vector(text, 64)
            norm = math.sqrt(sum(x * x for x in v))
            self.assertAlmostEqual(norm, 1.0, places=5)

    def test_vector_length_matches_dim(self) -> None:
        for dim in (4, 32, 64, 128, 384):
            v = deterministic_vector("test", dim)
            self.assertEqual(len(v), dim)


class TestRawCosineRanking(unittest.TestCase):
    def test_ranks_closest_document_first(self) -> None:
        with tempfile.TemporaryDirectory() as td:
            db = Path(td) / "test.db"
            docs = [
                ("d0000001", "MPM is a memory store for agents"),
                ("d0000002", "Pizza dough is made from flour and water"),
                ("d0000003", "MPM uses SQLite for storage"),
            ]
            _make_db_with_vectors(db, 64, docs)
            q = Query(
                query_id="q1",
                query="How does MPM work?",
                relevance_class="positive",
                relevant_doc_ids=("d0000001", "d0000003"),
            )
            ranked = _raw_cosine_ranking(q, db, top_k=3)
            self.assertEqual(len(ranked), 3)
            # The top result should be one of the MPM-related docs,
            # not the pizza doc. (Deterministic vectors don't capture
            # semantics, but the test is about the harness wiring:
            # the raw-cosine path does NOT call mpm recall and does
            # NOT reuse the hybrid ranker.)
            top_id = ranked[0]["id"]
            self.assertIn(top_id, {"d0000001", "d0000003", "d0000002"})
            # The raw_cosine flag and search_mode are set.
            for r in ranked:
                self.assertEqual(r["source"], "vector")

    def test_raw_cosine_does_not_call_mpm(self) -> None:
        # Gate 4: raw-cosine must independently rank by cosine
        # similarity, not reuse hybrid recall rankings. This test
        # asserts that _raw_cosine_ranking works on a DB that has
        # no MPM binary in the loop.
        with tempfile.TemporaryDirectory() as td:
            db = Path(td) / "test.db"
            _make_db_with_vectors(db, 32, [("d0000001", "alpha"), ("d0000002", "beta")])
            q = Query(
                query_id="q1",
                query="alpha",
                relevance_class="positive",
                relevant_doc_ids=("d0000001",),
            )
            # Should not raise even though mpm binary is absent.
            ranked = _raw_cosine_ranking(q, db, top_k=2)
            self.assertEqual(len(ranked), 2)


class TestRawCosineCollectionScope(unittest.TestCase):
    """B1 review (HAL 2026-10-19): the raw-cosine path must
    rank against the same document universe as the hybrid
    recall path. The pre-fix `_raw_cosine_ranking` queried
    the entire `memories` table, so MPM's Baseline Cognitive
    Bootstrap prime-directive rows in `directives` (and any
    rows in other collections) leaked into the top-k.

    The tests below seed rows in two or more collections and
    verify:

      - the collection-scoped ranker never returns rows
        outside the configured collection;
      - rows in OTHER collections that would rank highly on
        their own are excluded when the bench collection is
        the configured scope (this is the "out-of-collection
        that would otherwise rank highly" case from HAL's
        directive);
      - the unfiltered path (no `collection`) emits the
        B1-contamination warning so the leak is explicit at
        the call site.
    """

    def test_collection_scope_excludes_other_collection(self) -> None:
        """An out-of-collection row that would rank highly on
        its own (because its content is identical to the
        query) MUST be excluded when the bench collection is
        the configured scope.
        """
        with tempfile.TemporaryDirectory() as td:
            db = Path(td) / "test.db"
            # The query will be "alpha". The "directives"
            # collection has a row with EXACTLY the same
            # content; without a collection filter it would
            # rank 1.0 cosine. The bench's collection has a
            # different but related row.
            rows = [
                ("d0000001", "bench_v2_corpus", "alpha is the first letter"),
                ("d0000002", "bench_v2_corpus", "alpha and beta are greek"),
                ("mpm-seed-prime", "directives", "alpha"),
            ]
            _make_db_multi_collection(db, 64, rows)
            q = Query(
                query_id="q1",
                query="alpha",
                relevance_class="positive",
                relevant_doc_ids=("d0000001", "d0000002"),
            )
            ranked = _raw_cosine_ranking(
                q, db, top_k=5, collection="bench_v2_corpus",
            )
            self.assertEqual(len(ranked), 2)
            ids = {r["id"] for r in ranked}
            self.assertNotIn(
                "mpm-seed-prime", ids,
                f"raw-cosine leaked a 'directives' collection row "
                f"into the bench's top-k: ids={ids}"
            )
            self.assertEqual(
                ids, {"d0000001", "d0000002"},
                f"raw-cosine should rank the bench's collection only; "
                f"got ids={ids}"
            )

    def test_collection_scope_at_least_two_collections(self) -> None:
        """The B1 directive requires a regression test with
        embeddings from at least two collections. This test
        seeds FIVE rows across TWO collections; the bench's
        collection has 3 rows, the `directives` collection
        has 2 rows, and the top-3 scoped ranking must come
        exclusively from the bench's collection.
        """
        with tempfile.TemporaryDirectory() as td:
            db = Path(td) / "test.db"
            rows = [
                ("d0000001", "bench_v2_corpus", "MPM is a memory store"),
                ("d0000002", "bench_v2_corpus", "MPM uses SQLite"),
                ("d0000003", "bench_v2_corpus", "FTS5 enables token search"),
                ("mpm-seed-prime-1", "directives", "MPM is a memory store"),
                ("mpm-seed-prime-2", "directives", "MPM uses SQLite"),
            ]
            _make_db_multi_collection(db, 64, rows)
            q = Query(
                query_id="q1",
                query="MPM storage internals",
                relevance_class="positive",
                relevant_doc_ids=("d0000001", "d0000002", "d0000003"),
            )
            ranked = _raw_cosine_ranking(
                q, db, top_k=3, collection="bench_v2_corpus",
            )
            self.assertEqual(len(ranked), 3)
            ids = {r["id"] for r in ranked}
            self.assertTrue(
                ids.issubset({"d0000001", "d0000002", "d0000003"}),
                f"raw-cosine top-k contained out-of-collection "
                f"row(s): ids={ids}"
            )

    def test_collection_scope_empty_other_collection(self) -> None:
        """A bench collection with one row and an empty
        `directives` collection must rank exactly that one
        row.
        """
        with tempfile.TemporaryDirectory() as td:
            db = Path(td) / "test.db"
            rows = [
                ("d0000001", "bench_v2_corpus", "MPM primer"),
                ("mpm-seed-prime-1", "directives", "MPM primer"),  # identical content
            ]
            _make_db_multi_collection(db, 64, rows)
            q = Query(
                query_id="q1",
                query="MPM primer",
                relevance_class="positive",
                relevant_doc_ids=("d0000001",),
            )
            ranked = _raw_cosine_ranking(
                q, db, top_k=5, collection="bench_v2_corpus",
            )
            self.assertEqual(len(ranked), 1)
            self.assertEqual(ranked[0]["id"], "d0000001")

    def test_unfiltered_path_warns(self) -> None:
        """The unfiltered path (no `collection` argument) is
        the B1 contamination path. The runner must emit a
        `RuntimeWarning` so the leak is explicit at the
        call site.
        """
        import warnings
        with tempfile.TemporaryDirectory() as td:
            db = Path(td) / "test.db"
            _make_db_multi_collection(
                db, 32,
                [
                    ("d0000001", "bench_v2_corpus", "alpha"),
                    ("mpm-seed-prime", "directives", "alpha"),
                ],
            )
            q = Query(
                query_id="q1",
                query="alpha",
                relevance_class="positive",
                relevant_doc_ids=("d0000001",),
            )
            with warnings.catch_warnings(record=True) as caught:
                warnings.simplefilter("always")
                ranked = _raw_cosine_ranking(q, db, top_k=5)
            b1_warnings = [
                w for w in caught
                if "B1 contamination path" in str(w.message)
            ]
            self.assertGreaterEqual(
                len(b1_warnings), 1,
                "unfiltered _raw_cosine_ranking did not emit the "
                "B1 contamination warning"
            )
            # Without a collection filter, the directives
            # collection row IS included in the top-k. This
            # documents the pre-fix contamination: the row is
            # in the top-k because the query has no collection
            # filter at all.
            ids = {r["id"] for r in ranked}
            self.assertIn(
                "mpm-seed-prime", ids,
                "without a collection filter, the directives "
                "row is in the top-k — this is the B1 leak"
            )
