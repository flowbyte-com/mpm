"""Labelled relevance scoring tests (priority 3 of HAL's review).

Asserts the standard IR metrics implemented in `bench/scoring.py`:
  - Precision@k: of top-k results, what fraction are in
    `relevant_doc_ids`?
  - Recall@k: of `relevant_doc_ids`, what fraction appear in top-k?
  - MRR: 1 / rank_of_first_relevant, 0 if none.
  - nDCG@k: DCG / IDCG, binary relevance.

The expected values are computed by hand for small fixtures and
locked in as regression guards.
"""

import unittest

from bench.scoring import ScoredQuery, score_query, score_queries


class TestScoreQuery(unittest.TestCase):
    def test_perfect_ranking(self) -> None:
        # Top-3 contains the first 3 relevant docs in the right order.
        s = score_query(
            query_id="q1",
            is_positive=True,
            top_ids=["d1", "d2", "d3", "x", "y"],
            relevant_ids=["d1", "d2", "d3", "d4", "d5"],
            k=3,
        )
        # top-3 = [d1, d2, d3]; all 3 are relevant.
        self.assertEqual(s.precision_at_k, 1.0)
        # 3 of 5 relevant docs in top-3 -> recall 3/5 = 0.6
        self.assertAlmostEqual(s.recall_at_k, 0.6, places=6)
        # First result is relevant -> MRR = 1.0
        self.assertEqual(s.mrr, 1.0)
        # All top-3 are relevant, ideal top-3 are all relevant ->
        # nDCG = 1.0
        self.assertAlmostEqual(s.ndcg_at_k, 1.0, places=6)

    def test_first_relevant_at_rank_2(self) -> None:
        s = score_query(
            query_id="q1",
            is_positive=True,
            top_ids=["x", "d1", "y", "z", "w"],
            relevant_ids=["d1", "d2"],
            k=5,
        )
        # 1 of 5 is relevant -> precision 0.2
        self.assertAlmostEqual(s.precision_at_k, 0.2, places=6)
        # 1 of 2 in top-5 -> recall 0.5
        self.assertAlmostEqual(s.recall_at_k, 0.5, places=6)
        # First relevant at rank 2 -> MRR = 1/2
        self.assertEqual(s.mrr, 0.5)
        # nDCG: top-5 = [0,1,0,0,0] (one relevant at rank 2).
        # DCG = 1/log2(3); IDCG = 1/log2(2) + 1/log2(3) = 1 + 1/log2(3).
        import math
        dcg = 1.0 / (math.log(3) / math.log(2))  # = 1 / log2(3)
        idcg = 1.0 + dcg
        expected = dcg / idcg  # ≈ 0.3869
        self.assertAlmostEqual(s.ndcg_at_k, expected, places=4)

    def test_no_relevant_in_top_k(self) -> None:
        s = score_query(
            query_id="q1",
            is_positive=True,
            top_ids=["x", "y", "z"],
            relevant_ids=["d1", "d2"],
            k=3,
        )
        self.assertEqual(s.precision_at_k, 0.0)
        self.assertEqual(s.recall_at_k, 0.0)
        self.assertEqual(s.mrr, 0.0)
        # DCG = 0; nDCG = 0/0 -> 0.0 (guard in score_query).
        self.assertEqual(s.ndcg_at_k, 0.0)

    def test_negative_query_scores_zero(self) -> None:
        # Negative queries have no graded relevance; all four metrics
        # should report 0.0 (the participation check is the
        # load-bearing one for negatives).
        s = score_query(
            query_id="q1",
            is_positive=False,
            top_ids=["x", "y", "z"],
            relevant_ids=[],
            k=3,
        )
        self.assertEqual(s.precision_at_k, 0.0)
        self.assertEqual(s.recall_at_k, 0.0)
        self.assertEqual(s.mrr, 0.0)
        self.assertEqual(s.ndcg_at_k, 0.0)

    def test_top_k_truncation(self) -> None:
        # The corpus's relevant set has 5 docs but top_k=3 should
        # only score the first 3.
        s = score_query(
            query_id="q1",
            is_positive=True,
            top_ids=["d1", "d2", "d3", "d4", "d5", "d6"],
            relevant_ids=["d1", "d2", "d3", "d4", "d5"],
            k=3,
        )
        # Only the first 3 of top_ids are scored.
        self.assertEqual(s.top_ids, ("d1", "d2", "d3"))
        # 3/3 are relevant -> precision 1.0
        self.assertEqual(s.precision_at_k, 1.0)
        # 3 of 5 relevant docs in top-3 -> recall 0.6
        self.assertAlmostEqual(s.recall_at_k, 0.6, places=6)
        # First result is relevant -> MRR = 1.0
        self.assertEqual(s.mrr, 1.0)

    def test_recall_capped_at_1(self) -> None:
        # The same relevant doc appearing in two top positions does
        # NOT double-count toward recall.
        s = score_query(
            query_id="q1",
            is_positive=True,
            top_ids=["d1", "d1", "d1"],  # degenerate ranker
            relevant_ids=["d1"],
            k=3,
        )
        # 1 unique relevant out of 1 -> recall 1.0, not 3.0.
        self.assertEqual(s.recall_at_k, 1.0)


class TestScoreQueries(unittest.TestCase):
    def test_aggregates_over_positive_only(self) -> None:
        scored = score_queries(
            [
                ("q1", True, ["d1", "d2"], ["d1", "d2"]),  # perfect
                ("q2", True, ["x", "y"], ["d1"]),          # miss
                ("q3", False, [], []),                      # negative
            ],
            k=2,
        )
        # 2 positive, 1 negative.
        self.assertEqual(scored["positive_total"], 2)
        self.assertEqual(scored["negative_total"], 1)
        agg = scored["aggregates"]
        # q1: precision=1.0, recall=1.0, mrr=1.0, ndcg=1.0
        # q2: precision=0.0, recall=0.0, mrr=0.0, ndcg=0.0
        # Mean over positive only:
        self.assertAlmostEqual(agg["mean_precision_at_k"], 0.5, places=6)
        self.assertAlmostEqual(agg["mean_recall_at_k"], 0.5, places=6)
        self.assertAlmostEqual(agg["mean_mrr"], 0.5, places=6)
        self.assertAlmostEqual(agg["mean_ndcg_at_k"], 0.5, places=6)

    def test_no_positive_queries(self) -> None:
        # All-negative corpus: aggregates default to 0.0; the
        # report still records the population.
        scored = score_queries(
            [("q1", False, [], []), ("q2", False, ["x"], [])],
            k=1,
        )
        self.assertEqual(scored["positive_total"], 0)
        self.assertEqual(scored["negative_total"], 2)
        agg = scored["aggregates"]
        self.assertEqual(agg["mean_precision_at_k"], 0.0)
        self.assertEqual(agg["mean_recall_at_k"], 0.0)
        self.assertEqual(agg["mean_mrr"], 0.0)
        self.assertEqual(agg["mean_ndcg_at_k"], 0.0)

    def test_per_query_dict_shape(self) -> None:
        scored = score_queries(
            [("q1", True, ["d1"], ["d1"])],
            k=1,
        )
        per = scored["per_query"]
        self.assertEqual(len(per), 1)
        row = per[0]
        for key in (
            "query_id", "is_positive", "top_ids",
            "relevant_ids", "precision_at_k", "recall_at_k",
            "mrr", "ndcg_at_k",
        ):
            self.assertIn(key, row)
