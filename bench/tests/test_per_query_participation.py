"""Per-query vector participation tests (C5 / gate 4 of v2 review).

Asserts the contract:
  - Positive queries: must have at least one row with
    source in {"vector", "hybrid"}. Failure -> positive_failures.
  - Negative queries: any outcome is accepted; correctly-empty
    results are recorded as positive signals.
"""

import unittest

from bench.runner import QueryResult, per_query_participation


def _r(qid: str, is_pos: bool, sources: list[str]) -> QueryResult:
    rows = [
        {"id": "x", "content": "", "source": s, "score": 0.0, "vector_similarity": 0.0}
        for s in sources
    ]
    return QueryResult(
        query_id=qid,
        query="",
        is_positive=is_pos,
        rows=tuple(rows),
        search_mode="hybrid",
        raw_cosine=False,
    )


class TestPerQueryParticipation(unittest.TestCase):
    def test_positive_with_vector_succeeds(self) -> None:
        results = [_r("q1", True, ["vector"])]
        p = per_query_participation(results, {"q1"})
        self.assertEqual(p["positive_vector_participated"], 1)
        self.assertEqual(p["positive_failures"], [])

    def test_positive_with_hybrid_succeeds(self) -> None:
        results = [_r("q1", True, ["hybrid", "fts5"])]
        p = per_query_participation(results, {"q1"})
        self.assertEqual(p["positive_vector_participated"], 1)
        self.assertEqual(p["positive_failures"], [])

    def test_positive_with_only_fts5_fails(self) -> None:
        results = [_r("q1", True, ["fts5"])]
        p = per_query_participation(results, {"q1"})
        self.assertEqual(p["positive_vector_participated"], 0)
        self.assertEqual(len(p["positive_failures"]), 1)
        self.assertIn("no vector or hybrid row", p["positive_failures"][0]["reason"])

    def test_positive_with_empty_rows_fails(self) -> None:
        results = [_r("q1", True, [])]
        p = per_query_participation(results, {"q1"})
        self.assertEqual(p["positive_vector_participated"], 0)
        self.assertEqual(len(p["positive_failures"]), 1)

    def test_negative_with_empty_rows_is_positive_signal(self) -> None:
        results = [_r("q1", False, [])]
        p = per_query_participation(results, set())
        self.assertEqual(p["negative_total"], 1)
        self.assertIn("q1", p["negative_outcomes"]["correctly_empty"])

    def test_negative_with_vector_participated_tracked(self) -> None:
        results = [_r("q1", False, ["vector"])]
        p = per_query_participation(results, set())
        self.assertIn("q1", p["negative_outcomes"]["vector_participated"])

    def test_negative_with_fts5_only_tracked(self) -> None:
        results = [_r("q1", False, ["fts5"])]
        p = per_query_participation(results, set())
        self.assertIn("q1", p["negative_outcomes"]["fts5_only"])

    def test_mixed_positive_and_negative(self) -> None:
        results = [
            _r("q1", True, ["vector"]),
            _r("q2", True, ["fts5"]),  # failure
            _r("q3", False, []),  # correctly empty
            _r("q4", False, ["vector"]),  # participated
        ]
        p = per_query_participation(results, {"q1", "q2"})
        self.assertEqual(p["positive_total"], 2)
        self.assertEqual(p["positive_vector_participated"], 1)
        self.assertEqual(len(p["positive_failures"]), 1)
        self.assertEqual(p["negative_total"], 2)
        self.assertIn("q3", p["negative_outcomes"]["correctly_empty"])
        self.assertIn("q4", p["negative_outcomes"]["vector_participated"])
