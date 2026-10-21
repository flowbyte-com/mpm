"""Tests for the Issue #23 prefix condition matrix (Phase A).

Hermetic by default: no live DB, no real provider, no network. The
tests that need the `mpm` binary to prove the DOCUMENT side of the
prefix (that MPM's production embed path embeds the prefixed text
exactly once) are gated on the canonical build artifact and skip
when it is absent, exactly like the rest of the harness's CI story.

What these tests pin:

  1. The condition matrix has the shape the directive specifies
     (A/B/C/D), and conditions that need different DOCUMENT
     embeddings are grouped into different document preparations so
     vectors can never be mixed.
  2. The query prefix is applied EXACTLY once, and the text the
     provider receives is the prefix plus the query.
  3. The document prefix is applied EXACTLY once, and the text MPM's
     production embed path receives is the prefix plus the document.
  4. A corpus that already carries a prefix is refused, so a
     double-prefixed measurement cannot happen silently.
  5. Recall@1 and Recall@5 are computed from the same ranking as the
     headline MRR, at their own depths.
  6. The per-query comparison classifies improvements and
     regressions against the same-corpus baseline.
"""

from __future__ import annotations

import json
import os
import sqlite3
import tempfile
import unittest
from pathlib import Path

from bench.corpus import (
    Corpus,
    Document,
    Expectations,
    Query,
    Scoring,
)
from bench.fake_server import deterministic_vector, make_server
from bench.harness import CANONICAL_MPM_BINARY
from bench.prefix_experiment import (
    CONDITIONS,
    CONDITIONS_BY_KEY,
    DOCUMENT_PREFIX,
    QUERY_PREFIX,
    PrefixCondition,
    PrefixExperimentError,
    _assert_no_double_prefix,
    _document_preparation,
    _named_aggregates,
    compare_rankings,
    compare_to_baseline,
    provider_model_metadata,
    score_condition,
)
from bench.runner import QueryResult, run_query


# --------------------------------------------------------------------------
# helpers
# --------------------------------------------------------------------------

def _make_corpus(
    docs: list[tuple[str, str]],
    queries: list[Query],
    top_k: int = 3,
) -> Corpus:
    return Corpus(
        version="test",
        documents=tuple(
            Document(doc_id=did, content=text, relevance_class="relevant")
            for did, text in docs
        ),
        queries=tuple(queries),
        expectations=Expectations(
            by_query={q.query_id: q.relevant_doc_ids for q in queries}
        ),
        scoring=Scoring(method="test", top_k=top_k, parameters={}),
        manifest_path=Path("/nonexistent/MANIFEST.json"),
    )


def _make_db_with_vectors(path: Path, dim: int, docs: list[tuple[str, str]]) -> None:
    """Create a minimal `memories` table the raw-cosine path can read."""
    path.parent.mkdir(parents=True, exist_ok=True)
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


def _q(
    query_id: str,
    text: str,
    positive: bool = True,
    relevant: tuple[str, ...] = (),
    **kw: object,
) -> Query:
    return Query(
        query_id=query_id,
        query=text,
        relevance_class="positive" if positive else "negative",
        relevant_doc_ids=relevant,
        **kw,  # type: ignore[arg-type]
    )


def _result(
    query_id: str,
    text: str,
    positive: bool,
    ids: list[str],
    scores: list[float] | None = None,
) -> QueryResult:
    rows = tuple(
        {
            "id": did,
            "content": "",
            "source": "vector",
            "score": (scores[i] if scores else 1.0 - 0.1 * i),
            "vector_similarity": (scores[i] if scores else 1.0 - 0.1 * i),
        }
        for i, did in enumerate(ids)
    )
    return QueryResult(
        query_id=query_id,
        query=text,
        is_positive=positive,
        rows=rows,
        search_mode="raw_cosine",
        raw_cosine=True,
    )


# --------------------------------------------------------------------------
# 1. condition matrix shape
# --------------------------------------------------------------------------

class TestConditionMatrix(unittest.TestCase):
    def test_four_conditions_with_the_specified_prefixes(self) -> None:
        self.assertEqual([c.key for c in CONDITIONS], ["A", "B", "C", "D"])
        a, b, c, d = CONDITIONS
        self.assertEqual(a.document_prefix, "")
        self.assertEqual(a.query_prefix, "")
        self.assertEqual(b.document_prefix, "")
        self.assertEqual(b.query_prefix, QUERY_PREFIX)
        self.assertEqual(c.document_prefix, DOCUMENT_PREFIX)
        self.assertEqual(c.query_prefix, QUERY_PREFIX)
        self.assertEqual(d.document_prefix, DOCUMENT_PREFIX)
        self.assertEqual(d.query_prefix, "")

    def test_conditions_that_differ_in_document_embeddings_are_separated(self) -> None:
        preps = {c.key: c.document_preparation for c in CONDITIONS}
        # A and B share the unprefixed documents -> one workspace.
        self.assertEqual(preps["A"], preps["B"])
        # C and D share the prefixed documents -> a DIFFERENT workspace.
        self.assertEqual(preps["C"], preps["D"])
        # ... and that workspace is not the unprefixed one.
        self.assertNotEqual(preps["A"], preps["C"])

    def test_only_the_document_prefix_distinguishes_the_two_preparations(self) -> None:
        by_prep: dict[str, set[str]] = {}
        for c in CONDITIONS:
            by_prep.setdefault(c.document_preparation, set()).add(c.document_prefix)
        for prep, prefixes in by_prep.items():
            self.assertEqual(
                len(prefixes), 1, f"preparation {prep} has mixed document prefixes"
            )

    def test_condition_lookup_by_key(self) -> None:
        self.assertIs(CONDITIONS_BY_KEY["C"], CONDITIONS[2])
        self.assertEqual(CONDITIONS_BY_KEY["B"].query_prefix, "search_query: ")


# --------------------------------------------------------------------------
# 2. query prefix applied exactly once
# --------------------------------------------------------------------------

class TestQueryPrefixAppliedOnce(unittest.TestCase):
    def _record(self, prefix: str) -> list[str]:
        seen: list[str] = []

        def recorder(text: str, dim: int) -> list[float]:
            seen.append(text)
            return deterministic_vector(text, dim)

        server, base_url, _port = make_server(
            dim=64, model_name="fake-embed-v1", vector_fn=recorder
        )
        try:
            with tempfile.TemporaryDirectory() as td:
                snap = Path(td) / "snap"
                snap.mkdir()
                _make_db_with_vectors(
                    snap / "src" / "db" / "mpm.db",
                    64,
                    [("d1", "alpha document"), ("d2", "beta document")],
                )
                profile = {
                    "provider": "openai-compatible",
                    "model": "fake-embed-v1",
                    "base_url": base_url,
                    "api_key": "",
                }
                run_query(
                    _q("q1", "alpha query", relevant=("d1",)),
                    snap,
                    "mpm-not-invoked",
                    raw_cosine=True,
                    raw_cosine_prefix=prefix,
                    embedding_profile=profile,
                    top_k=2,
                    collection="memories",
                )
        finally:
            server.shutdown()
            server.server_close()
        return seen

    def test_prefixed_query_reaches_the_provider_once(self) -> None:
        seen = self._record(QUERY_PREFIX)
        self.assertEqual(seen, ["search_query: alpha query"])
        self.assertNotIn("search_query: search_query: alpha query", seen)

    def test_unprefixed_query_reaches_the_provider_unchanged(self) -> None:
        seen = self._record("")
        self.assertEqual(seen, ["alpha query"])

    def test_prefix_is_the_nomic_documented_form(self) -> None:
        self.assertEqual(QUERY_PREFIX, "search_query: ")
        self.assertEqual(DOCUMENT_PREFIX, "search_document: ")


# --------------------------------------------------------------------------
# 3. document prefix applied exactly once, through MPM's production path
# --------------------------------------------------------------------------

@unittest.skipUnless(
    CANONICAL_MPM_BINARY.exists(),
    "canonical mpm build artifact is absent; run `make build` first",
)
class TestDocumentPrefixAppliedOnce(unittest.TestCase):
    """The document side of the prefix, proven through MPM's own
    backfill path rather than a benchmark re-implementation."""

    def _record(self) -> tuple[list[str], object]:
        seen: list[str] = []

        def recorder(text: str, dim: int) -> list[float]:
            seen.append(text)
            return deterministic_vector(text, dim)

        server, base_url, _port = make_server(
            dim=64, model_name="fake-embed-v1", vector_fn=recorder
        )
        return seen, (server, base_url)

    def _run(self, document_prefix: str) -> list[str]:
        from bench.prefix_experiment import prepare_document_workspace

        seen, server_pair = self._record()
        server, base_url = server_pair
        try:
            corpus = _make_corpus(
                [("d1", "alpha document"), ("d2", "beta document")],
                [_q("q1", "alpha query", relevant=("d1",))],
            )
            config = {
                "profiles": {
                    "default": {
                        "provider": "openai-compatible",
                        "model": "fake-embed-v1",
                        "base_url": base_url,
                        "api_key": "",
                    }
                },
                "components": {"embedding": "default"},
                "expected": {
                    "model": "openai-compatible:fake-embed-v1",
                    "dimension": 64,
                },
            }
            ws = prepare_document_workspace(
                document_prefix, corpus, config
            )
            self.assertTrue(
                ws.verification.ok,
                f"fingerprint verification failed: {ws.verification.failures}",
            )
            # The preparation key is DERIVED from the prefix, so it can
            # never describe the wrong text.
            self.assertEqual(
                ws.preparation, _document_preparation(document_prefix)
            )
        finally:
            server.shutdown()
            server.server_close()
        return seen

    def test_prefixed_documents_reach_the_provider_exactly_once(self) -> None:
        seen = self._run(DOCUMENT_PREFIX)
        self.assertIn("search_document: alpha document", seen)
        self.assertIn("search_document: beta document", seen)
        # The double-prefixed form must NOT be present.
        self.assertNotIn(
            "search_document: search_document: alpha document", seen
        )
        self.assertNotIn("search_document: search_document: beta document", seen)
        # The unprefixed form must NOT be present either (the
        # preparation is prefixed, so the bare text is not embedded).
        self.assertNotIn("alpha document", seen)

    def test_unprefixed_documents_reach_the_provider_unchanged(self) -> None:
        seen = self._run("")
        self.assertIn("alpha document", seen)
        self.assertIn("beta document", seen)
        self.assertNotIn("search_document: alpha document", seen)


# --------------------------------------------------------------------------
# 4. double-prefix guards
# --------------------------------------------------------------------------

class TestDoublePrefixGuards(unittest.TestCase):
    def test_corpus_with_a_prefixed_document_is_refused(self) -> None:
        corpus = _make_corpus(
            [("d1", "search_document: alpha")],
            [_q("q1", "alpha query", relevant=("d1",))],
        )
        with self.assertRaises(PrefixExperimentError):
            _assert_no_double_prefix(corpus)

    def test_corpus_with_a_prefixed_query_is_refused(self) -> None:
        corpus = _make_corpus(
            [("d1", "alpha")],
            [_q("q1", "search_query: alpha query", relevant=("d1",))],
        )
        with self.assertRaises(PrefixExperimentError):
            _assert_no_double_prefix(corpus)

    def test_clean_corpus_passes(self) -> None:
        corpus = _make_corpus(
            [("d1", "alpha")],
            [_q("q1", "alpha query", relevant=("d1",))],
        )
        _assert_no_double_prefix(corpus)  # must not raise

    def test_ingest_refuses_a_document_that_already_carries_the_prefix(self) -> None:
        """`_ingest_corpus` guards the ingest path itself.

        The guard runs before the INSERT loop reaches the offending
        row, so it fires without needing a live MPM binary: the
        check is on the corpus content, not on the database.
        """
        from bench.harness import _ingest_corpus

        corpus = _make_corpus(
            [("d1", "search_document: alpha")],
            [_q("q1", "alpha query", relevant=("d1",))],
        )
        # `_ingest_corpus` bootstraps the schema through the mpm
        # binary first, so this test only asserts the guard's
        # contract when the binary is available. Without it, assert
        # that the corpus-level guard (above) catches the same case.
        if not CANONICAL_MPM_BINARY.exists():
            with self.assertRaises(PrefixExperimentError):
                _assert_no_double_prefix(corpus)
            return
        with tempfile.TemporaryDirectory() as td:
            snap = Path(td) / "snap"
            snap.mkdir()
            with self.assertRaises(RuntimeError) as ctx:
                _ingest_corpus(snap, corpus, document_prefix=DOCUMENT_PREFIX)
            self.assertIn("double-prefix", str(ctx.exception))


# --------------------------------------------------------------------------
# 5. scoring at multiple depths
# --------------------------------------------------------------------------

class TestScoringAtDepths(unittest.TestCase):
    def test_recall_at_1_and_5_come_from_the_same_ranking(self) -> None:
        corpus = _make_corpus(
            [("d1", "a"), ("d2", "b"), ("d3", "c"), ("d4", "d"), ("d5", "e"),
             ("d6", "f")],
            [_q("q1", "query", relevant=("d3", "d6"))],
            top_k=10,
        )
        # The relevant documents are at ranks 3 and 6.
        results = [
            _result(
                "q1", "query", True,
                ["d1", "d2", "d3", "d4", "d5", "d6"],
            )
        ]
        scored = score_condition(corpus, results, [1, 5, 10])
        row = scored["rows"][0]
        # Recall@1: neither relevant doc is at rank 1.
        self.assertAlmostEqual(row["recall_at_1"], 0.0)
        # Recall@5: one of the two relevant docs is in the top 5.
        self.assertAlmostEqual(row["recall_at_5"], 0.5)
        # Recall@10: both are in the top 10.
        self.assertAlmostEqual(row["recall_at_10"], 1.0)
        # MRR at the corpus depth: first relevant doc is at rank 3.
        self.assertAlmostEqual(row["mrr_at_10"], 1.0 / 3.0)
        # first_relevant_rank is reported for the per-query diff.
        self.assertEqual(row["first_relevant_rank"], 3)

    def test_recall_at_1_is_one_when_the_top_hit_is_relevant(self) -> None:
        corpus = _make_corpus(
            [("d1", "a"), ("d2", "b")],
            [_q("q1", "query", relevant=("d1",))],
            top_k=10,
        )
        results = [_result("q1", "query", True, ["d1", "d2"])]
        scored = score_condition(corpus, results, [1, 5, 10])
        row = scored["rows"][0]
        self.assertAlmostEqual(row["recall_at_1"], 1.0)
        self.assertAlmostEqual(row["mrr_at_10"], 1.0)

    def test_negative_queries_are_scored_zero_and_reported_separately(self) -> None:
        corpus = _make_corpus(
            [("d1", "a")],
            [_q("q1", "no answer here", positive=False)],
            top_k=10,
        )
        results = [_result("q1", "no answer here", False, ["d1"])]
        scored = score_condition(corpus, results, [1, 5, 10])
        row = scored["rows"][0]
        self.assertAlmostEqual(row["recall_at_1"], 0.0)
        self.assertAlmostEqual(row["mrr_at_10"], 0.0)
        self.assertEqual(scored["negative"]["n"], 1)
        self.assertEqual(scored["positive"]["n"], 0)

    def test_paraphrase_subset_is_reported(self) -> None:
        corpus = _make_corpus(
            [("d1", "a")],
            [
                _q("q1", "base query", relevant=("d1",)),
                _q("q2", "paraphrased query", relevant=("d1",),
                   paraphrase_of="q1", source="paraphrase"),
            ],
            top_k=10,
        )
        results = [
            _result("q1", "base query", True, ["d1"]),
            _result("q2", "paraphrased query", True, ["d1"]),
        ]
        scored = score_condition(corpus, results, [1, 5, 10])
        self.assertEqual(scored["paraphrase"]["n"], 1)
        self.assertAlmostEqual(scored["paraphrase"]["mean_recall_at_10"], 1.0)

    def test_hard_negatives_are_split_out(self) -> None:
        corpus = _make_corpus(
            [("d1", "a")],
            [
                _q("q1", "genuine", positive=False,
                   label_rationale="Genuine negative: not in the corpus."),
                _q("q2", "hard", positive=False,
                   label_rationale="Hard negative: lexically close to d1."),
            ],
            top_k=10,
        )
        results = [
            _result("q1", "genuine", False, ["d1"]),
            _result("q2", "hard", False, ["d1"]),
        ]
        scored = score_condition(corpus, results, [1, 5, 10])
        self.assertEqual(scored["negative"]["n"], 2)
        self.assertEqual(scored["hard_negative"]["n"], 1)
        self.assertEqual(scored["hard_negative"]["rows"][0]["query_id"], "q2")

    def test_calibration_and_held_out_splits_are_reported_separately(self) -> None:
        corpus = _make_corpus(
            [("d1", "a"), ("d2", "b")],
            [
                _q("q1", "cal query", relevant=("d1",), split="calibration"),
                _q("q2", "held query", relevant=("d2",), split="held-out"),
                _q("q3", "cal negative", positive=False,
                   split="calibration",
                   label_rationale="Genuine negative: not in the corpus."),
            ],
            top_k=10,
        )
        results = [
            _result("q1", "cal query", True, ["d1"]),
            _result("q2", "held query", True, ["d2"]),
            _result("q3", "cal negative", False, ["d1"]),
        ]
        scored = score_condition(corpus, results, [1, 5, 10])
        self.assertEqual(sorted(scored["by_split"]), ["calibration", "held-out"])
        self.assertEqual(scored["by_split"]["calibration"]["n"], 1)
        self.assertEqual(scored["by_split"]["held-out"]["n"], 1)
        self.assertAlmostEqual(
            scored["by_split"]["held-out"]["mean_recall_at_10"], 1.0
        )
        # Split counts: positives drive the split metrics; negatives are
        # counted separately and must not dilute the split mean.
        self.assertEqual(scored["by_split"]["calibration"]["n_total"], 2)
        self.assertEqual(scored["by_split"]["calibration"]["n_negative"], 1)
        self.assertEqual(scored["by_split"]["held-out"]["n_total"], 1)
        self.assertEqual(scored["by_split"]["held-out"]["n_negative"], 0)


# --------------------------------------------------------------------------
# 6. per-query comparison against the same-corpus baseline
# --------------------------------------------------------------------------

class TestCompareToBaseline(unittest.TestCase):
    def _corpus_and_results(self):
        corpus = _make_corpus(
            [("d1", "a"), ("d2", "b")],
            [
                _q("q1", "q1 text", relevant=("d1",)),
                _q("q2", "q2 text", relevant=("d1",)),
                _q("q3", "q3 text", relevant=("d1",)),
            ],
            top_k=10,
        )
        baseline = [
            # q1: relevant at rank 1 (MRR 1.0)
            _result("q1", "q1 text", True, ["d1", "d2"]),
            # q2: relevant at rank 2 (MRR 0.5)
            _result("q2", "q2 text", True, ["d2", "d1"]),
            # q3: relevant at rank 2 (MRR 0.5)
            _result("q3", "q3 text", True, ["d2", "d1"]),
        ]
        condition = [
            # q1 unchanged (MRR 1.0)
            _result("q1", "q1 text", True, ["d1", "d2"]),
            # q2 regressed (MRR 0.5 -> 0.0)
            _result("q2", "q2 text", True, ["d2", "d3"]),
            # q3 improved (MRR 0.5 -> 1.0)
            _result("q3", "q3 text", True, ["d1", "d2"]),
        ]
        return corpus, baseline, condition

    def test_improvements_regressions_and_unchanged_are_classified(self) -> None:
        corpus, baseline, condition = self._corpus_and_results()
        base_scored = score_condition(corpus, baseline, [1, 5, 10])
        cond_scored = score_condition(corpus, condition, [1, 5, 10])
        cmp = compare_to_baseline(base_scored, cond_scored, "A", "B", [1, 5, 10])
        summary = cmp["per_query_summary"]
        self.assertEqual(summary["improved"], 1)
        self.assertEqual(summary["regressed"], 1)
        self.assertEqual(summary["unchanged"], 1)
        self.assertEqual(summary["total"], 3)
        self.assertEqual(
            [r["query_id"] for r in cmp["improved_queries"]], ["q3"]
        )
        self.assertEqual(
            [r["query_id"] for r in cmp["regressed_queries"]], ["q2"]
        )

    def test_aggregate_delta_is_condition_minus_baseline(self) -> None:
        corpus, baseline, condition = self._corpus_and_results()
        base_scored = score_condition(corpus, baseline, [1, 5, 10])
        cond_scored = score_condition(corpus, condition, [1, 5, 10])
        cmp = compare_to_baseline(base_scored, cond_scored, "A", "B", [1, 5, 10])
        base_mrr = base_scored["aggregates_by_depth"]["10"]["aggregates"]["mean_mrr_at_k"]
        cond_mrr = cond_scored["aggregates_by_depth"]["10"]["aggregates"]["mean_mrr_at_k"]
        # The delta is keyed by DEPTH, so no reader has to parse a
        # depth out of a metric name.
        self.assertEqual(
            sorted(cmp["aggregate_delta"]), ["1", "10", "5"]
        )
        self.assertAlmostEqual(
            cmp["aggregate_delta"]["10"]["mean_mrr"], cond_mrr - base_mrr
        )
        # Baseline MRR: (1.0 + 0.5 + 0.5)/3 = 0.6667; condition: (1.0 + 0 + 1.0)/3
        self.assertAlmostEqual(base_mrr, 2.0 / 3.0, places=6)
        self.assertAlmostEqual(cond_mrr, 2.0 / 3.0, places=6)

    def test_changed_queries_lists_only_the_queries_that_moved(self) -> None:
        corpus, baseline, condition = self._corpus_and_results()
        base_scored = score_condition(corpus, baseline, [1, 5, 10])
        cond_scored = score_condition(corpus, condition, [1, 5, 10])
        cmp = compare_to_baseline(base_scored, cond_scored, "A", "B", [1, 5, 10])
        changed = {r["query_id"] for r in cmp["changed_queries"]}
        self.assertEqual(changed, {"q2", "q3"})
        self.assertNotIn("per_query_deltas", cmp)


# --------------------------------------------------------------------------
# 6b. ranking agreement between two arms
# --------------------------------------------------------------------------

class TestCompareRankings(unittest.TestCase):
    def _scored(self, results):
        corpus = _make_corpus(
            [("d1", "a"), ("d2", "b")],
            [
                _q("q1", "q1 text", relevant=("d1",)),
                _q("q2", "q2 text", relevant=("d1",)),
            ],
            top_k=10,
        )
        return score_condition(corpus, results, [1, 5, 10])

    def test_identical_rankings_are_reported_as_identical(self) -> None:
        results = [
            _result("q1", "q1 text", True, ["d1", "d2"]),
            _result("q2", "q2 text", True, ["d1", "d2"]),
        ]
        scored = self._scored(results)
        cmp = compare_rankings(scored, scored, "left", "right", 10)
        self.assertTrue(cmp["identical"])
        self.assertEqual(cmp["queries_compared"], 2)
        self.assertEqual(cmp["differing_top1"], 0)
        self.assertEqual(cmp["differing_mrr"], 0)
        self.assertEqual(cmp["differing_query_ids"], [])

    def test_a_differing_top1_is_reported_per_query(self) -> None:
        left = self._scored([
            _result("q1", "q1 text", True, ["d1", "d2"]),
            _result("q2", "q2 text", True, ["d1", "d2"]),
        ])
        right = self._scored([
            _result("q1", "q1 text", True, ["d1", "d2"]),
            _result("q2", "q2 text", True, ["d2", "d1"]),
        ])
        cmp = compare_rankings(left, right, "left", "right", 10)
        self.assertFalse(cmp["identical"])
        self.assertEqual(cmp["differing_top1"], 1)
        self.assertEqual(cmp["differing_mrr"], 1)
        self.assertEqual(cmp["differing_query_ids"], ["q2"])


# --------------------------------------------------------------------------
# 6c. metric naming
# --------------------------------------------------------------------------

class TestNamedAggregates(unittest.TestCase):
    def test_mrr_is_renamed_to_carry_its_depth(self) -> None:
        agg = _named_aggregates(
            {
                "mean_precision_at_k": 0.5,
                "mean_recall_at_k": 0.6,
                "mean_mrr": 0.7,
                "mean_ndcg_at_k": 0.8,
            },
            10,
        )
        self.assertNotIn("mean_mrr", agg)
        self.assertAlmostEqual(agg["mean_mrr_at_k"], 0.7)
        self.assertEqual(agg["depth"], 10)
        self.assertEqual(agg["population"], "positive queries only")


class TestDocumentPreparationDerivation(unittest.TestCase):
    def test_the_key_is_derived_from_the_prefix_alone(self) -> None:
        self.assertEqual(_document_preparation(""), "doc_unprefixed")
        self.assertEqual(
            _document_preparation(DOCUMENT_PREFIX), "doc_search_document"
        )

    def test_the_condition_property_uses_the_same_derivation(self) -> None:
        for c in CONDITIONS:
            self.assertEqual(
                c.document_preparation, _document_preparation(c.document_prefix)
            )

    def test_an_arbitrary_prefix_still_derives_a_workspace_key(self) -> None:
        cond = PrefixCondition(
            key="X",
            name="hypothetical",
            document_prefix="some_other_prefix: ",
            query_prefix="",
            description="",
        )
        # The key is a function of the prefix, not a lookup in a table
        # of the two prefixes this experiment happens to use: it names
        # the prefix it belongs to.
        self.assertEqual(
            cond.document_preparation,
            _document_preparation("some_other_prefix: "),
        )
        self.assertEqual(cond.document_preparation, "doc_some_other_prefix")


# --------------------------------------------------------------------------
# 7. provider metadata (provenance capture)
# --------------------------------------------------------------------------

class TestProviderModelMetadata(unittest.TestCase):
    def test_returns_none_for_a_non_ollama_base_url(self) -> None:
        self.assertIsNone(
            provider_model_metadata(
                {
                    "provider": "openai-compatible",
                    "model": "m",
                    "base_url": "https://api.example.com/v1",
                }
            )
        )

    def test_returns_none_without_a_profile(self) -> None:
        self.assertIsNone(provider_model_metadata(None))

    def test_digest_and_embedding_length_are_read_from_the_ollama_reply(self) -> None:
        digest = (
            "0a109f422b47e3a30ba2b10eca18548e944e8a23073ee3f3e947efcf3c45e59f"
        )
        show_reply = {
            "details": {
                "family": "nomic-bert",
                "parameter_size": "137M",
                "families": ["nomic-bert"],
            },
            "model_info": {"nomic-bert.embedding_length": 768},
            "parameters": "num_ctx 8192",
            "template": "{{ .Prompt }}",
        }
        tags_reply = {
            "models": [
                {"name": "nomic-embed-text:latest", "digest": digest},
            ]
        }
        server, base_url, _port = make_server(
            dim=768,
            model_name="nomic-embed-text",
            api_show_reply=show_reply,
            api_tags_reply=tags_reply,
        )
        try:
            meta = provider_model_metadata({
                "provider": "openai-compatible",
                "model": "nomic-embed-text",
                "base_url": base_url,
            })
        finally:
            server.shutdown()
            server.server_close()
        assert meta is not None
        self.assertEqual(meta["digest"], digest)
        self.assertEqual(meta["details"]["embedding_length"], 768)
        self.assertEqual(meta["template"], "{{ .Prompt }}")


if __name__ == "__main__":
    unittest.main()
