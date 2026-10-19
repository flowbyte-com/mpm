"""Real labelled relevance scoring (priority 3 of HAL's review).

The corpus already binds documents, queries, and `relevant_doc_ids`
together (verified by `load_corpus` in `bench/corpus.py`). The
participation check in `bench/runner.py:per_query_participation`
is intentionally coarse: it asks "did the vector channel
participate at all". Real labelled relevance scoring asks
"how good was the ranking against the human-labelled
`relevant_doc_ids`?".

This module computes, for a top-k result set and a labelled
`relevant_doc_ids` tuple, four standard IR metrics:

  - Precision@k:    of the top-k results, what fraction are in
                    `relevant_doc_ids`?
  - Recall@k:       of `relevant_doc_ids`, what fraction appear in
                    top-k? (1.0 if every relevant doc is in top-k.)
  - MRR:            mean reciprocal rank of the first relevant
                    result. 1.0 if the first result is relevant;
                    1/2 if the second result is the first relevant
                    one; 0.0 if no relevant doc appears in top-k.
  - nDCG@k:         normalized discounted cumulative gain, binary
                    relevance (relevant=1, not=0). The DCG is
                    normalised by the IDCG of an ideal ranking
                    (all relevant docs in their best positions).

Negative queries (zero `relevant_doc_ids`) get perfect scores by
construction on the relevance-graded metrics (Precision@k = N/A
because there is nothing to be relevant to). For negative queries
the contract is "vector channel should not promote a wrong
answer" and is enforced separately by
`per_query_participation` (an empty result for a negative query
is a `correctly_empty` positive signal, not a metric failure).
This module therefore reports `0.0` for the relevance-graded
metrics on negative queries — the participation check is the
load-bearing one for negatives.

Why these four and not others:
  - Precision@k / Recall@k are the most direct "did the ranker
    put relevant docs in the top-k" measures.
  - MRR is the metric the historical benchmark used
    (docs/EMBEDDING-MODEL-COMPARISON.md:226-233) and the one
    whose +0.022 figure is most quoted; the harness must be
    able to compare apples to apples on this metric even
    though it does not reproduce the historical value.
  - nDCG@k is the most common modern IR metric; it rewards
    placing multiple relevant docs in the top-k and weights
    position, which Precision@k alone does not.

All metrics are computed from the joined-in abbreviated IDs.
The full-ID / abbreviated-ID map is held by the runner; this
module is pure and stateless.
"""

from __future__ import annotations

from dataclasses import dataclass
from typing import Iterable, Sequence


@dataclass(frozen=True)
class ScoredQuery:
    query_id: str
    is_positive: bool
    top_ids: tuple[str, ...]          # abbreviated IDs, ranked
    relevant_ids: frozenset[str]      # the labelled relevant set
    precision_at_k: float
    recall_at_k: float
    mrr: float
    ndcg_at_k: float

    def to_dict(self) -> dict[str, object]:
        return {
            "query_id": self.query_id,
            "is_positive": self.is_positive,
            "top_ids": list(self.top_ids),
            "relevant_ids": sorted(self.relevant_ids),
            "precision_at_k": self.precision_at_k,
            "recall_at_k": self.recall_at_k,
            "mrr": self.mrr,
            "ndcg_at_k": self.ndcg_at_k,
        }


def _dcg_at_k(relevances: Sequence[int], k: int) -> float:
    """Discounted cumulative gain at k. Binary relevance (0/1).
    Position discount is `1 / log2(rank+1)`; rank is 1-indexed."""
    dcg = 0.0
    for i, rel in enumerate(relevances[:k]):
        rank = i + 1
        if rel:
            # log2(rank+1) — the standard formula (rank 1 has
            # discount 1.0, rank 2 has 1/log2(3), etc.)
            dcg += 1.0 / _log2(rank + 1)
    return dcg


def _log2(x: float) -> float:
    """log2 without importing math.log2 (keeps this module
    import-cheap and matches the rest of the harness's stdlib-
    only style)."""
    # natural log / natural log of 2
    import math
    return math.log(x) / math.log(2.0)


def score_query(
    query_id: str,
    is_positive: bool,
    top_ids: Iterable[str],
    relevant_ids: Iterable[str],
    k: int,
) -> ScoredQuery:
    """Score a single query's top-k against its labelled
    `relevant_ids`. Negative queries get zero scores on the
    relevance-graded metrics (see module docstring)."""
    top = tuple(top_ids)[:k]
    rel = frozenset(relevant_ids)

    if not is_positive or not rel:
        # Negative queries have no graded relevance. Report 0.0
        # for the four metrics; the participation check is the
        # load-bearing one for negatives.
        return ScoredQuery(
            query_id=query_id,
            is_positive=is_positive,
            top_ids=top,
            relevant_ids=rel,
            precision_at_k=0.0,
            recall_at_k=0.0,
            mrr=0.0,
            ndcg_at_k=0.0,
        )

    # Build a binary relevance vector for the top-k.
    rels = [1 if tid in rel else 0 for tid in top]

    # Precision@k: fraction of top-k that are relevant.
    # 0 if k == 0 (avoid div-by-zero; top-0 is meaningless).
    precision = (sum(rels) / len(rels)) if rels else 0.0

    # Recall@k: fraction of `relevant_ids` that appear in top-k.
    # Capped at 1.0 even if the same relevant doc appears in
    # multiple top positions (a ranker that returned the same
    # doc twice is degenerate, not "double relevant").
    if not rel:
        recall = 0.0
    else:
        hit = sum(min(1, r) for r in rels)
        recall = min(1.0, hit / len(rel))

    # MRR: 1 / rank_of_first_relevant, 0 if none.
    mrr = 0.0
    for i, r in enumerate(rels):
        if r:
            mrr = 1.0 / (i + 1)
            break

    # nDCG@k: DCG / IDCG, where IDCG assumes `min(k, |rel|)`
    # relevant docs at the top positions.
    dcg = _dcg_at_k(rels, k)
    ideal_rels = [1] * min(k, len(rel))
    idcg = _dcg_at_k(ideal_rels, k)
    ndcg = (dcg / idcg) if idcg > 0 else 0.0

    return ScoredQuery(
        query_id=query_id,
        is_positive=is_positive,
        top_ids=top,
        relevant_ids=rel,
        precision_at_k=precision,
        recall_at_k=recall,
        mrr=mrr,
        ndcg_at_k=ndcg,
    )


def score_queries(
    queries: Sequence[tuple[str, bool, Sequence[str], Sequence[str]]],
    k: int,
) -> dict[str, object]:
    """Score a list of (query_id, is_positive, top_ids, relevant_ids)
    tuples. Returns the per-query ScoredQuery list and aggregate
    summaries.

    Aggregate behaviour:
      - Mean Precision/Recall/MRR/nDCG are computed over POSITIVE
        queries only (negative queries have no graded relevance).
      - A `positive_total` and `positive_scored` count is included
        so the report makes it clear the means are over which set.
    """
    scored = [
        score_query(qid, is_pos, top, rel, k)
        for (qid, is_pos, top, rel) in queries
    ]
    positive = [s for s in scored if s.is_positive]
    n_pos = len(positive)
    if n_pos == 0:
        return {
            "k": k,
            "positive_total": 0,
            "negative_total": len(scored) - n_pos,
            "per_query": [s.to_dict() for s in scored],
            "aggregates": {
                "mean_precision_at_k": 0.0,
                "mean_recall_at_k": 0.0,
                "mean_mrr": 0.0,
                "mean_ndcg_at_k": 0.0,
            },
        }
    return {
        "k": k,
        "positive_total": n_pos,
        "negative_total": len(scored) - n_pos,
        "per_query": [s.to_dict() for s in scored],
        "aggregates": {
            "mean_precision_at_k": sum(s.precision_at_k for s in positive) / n_pos,
            "mean_recall_at_k": sum(s.recall_at_k for s in positive) / n_pos,
            "mean_mrr": sum(s.mrr for s in positive) / n_pos,
            "mean_ndcg_at_k": sum(s.ndcg_at_k for s in positive) / n_pos,
        },
    }
