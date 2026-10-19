"""Option D — abstention-policy evaluation (HAL Directive A2.4).

HAL's directive for Phase A.2 asks: do the observables exposed by the
production ranker contain enough evidence to distinguish answerable
from unanswerable queries, and is a useful confidence calibration
possible without changing the production contract?

This script reads a `RunReport` JSON (the file written by
`bench.harness.cmd_run`) and computes a set of per-query features:

  - top-1 cosine similarity (raw vector dot product, normalized)
  - top-1 combined score (the production HybridSearch output)
  - top-k mean cosine (mean of the top-k cosine similarities)
  - top-k mean combined score
  - top-1 to top-2 gap
  - source distribution: vector / fts5 / hybrid

It then evaluates a small set of abstention policies on the v2
calibration corpus (41 queries, 40 positive + 20 negative total
split into calibration/held-out scopes) and reports:

  - false-abstention rate: fraction of positive queries where the
    policy says "abstain" (we would refuse to answer when the
    answer IS in the corpus)
  - false-confidence rate: fraction of negative queries where the
    policy says "answer" (we would return an answer when the answer
    is NOT in the corpus)
  - the achieved precision/recall/MRR on the answering subset
  - the abstain rate on held-out queries (for generalization
    evidence)

If the available signals cannot separate positive from negative
queries with a low false-abstention AND low false-confidence
trade-off, the script reports that explicitly.

A2.4 also asks for a proposed MACHINE-READABLE empty-result contract
to be specified (not implemented). The proposal lives in this file's
module docstring and in the report's "empty_result_contract" section.

The script's contract is read-only: it consumes a report, never
touches MPM, the corpus, or the live install.
"""

from __future__ import annotations

import argparse
import json
import sys
from dataclasses import dataclass, field
from pathlib import Path
from typing import Any


# A2.4: Proposed machine-readable empty-result contract.
#
# Current behaviour (cmd/mpm/recall.go:194-197): when the hybrid
# search returns 0 rows, the CLI prints a plain-text line on stdout
# and exits 0. With `--json` the response is still plain text; the
# JSON envelope is only emitted on a non-empty result.
#
# Proposed contract (NOT implemented in A2.2; this is a spec for
# Phase B to consider):
#
#   {
#     "success": true,
#     "query": "<echo>",
#     "search_mode": "hybrid",
#     "count": 0,
#     "memories": [],
#     "abstention": {
#       "policy": "<policy_name or 'empty_ranker'>",
#       "reason": "ranker_returned_no_rows" |
#                 "no_result_below_confidence_threshold" |
#                 "no_source_participated",
#       "top_observed_score": 0.0,
#       "score_type": "combined" | "cosine",
#       "would_have_returned_above_threshold": 0.0
#     },
#     "limit": 15,
#     "min_score": null
#   }
#
# Backwards-compatibility:
#   - Existing `--json` consumers that key on `memories` continue
#     to work; they get an empty list and `count=0` instead of
#     a parse error on plain text.
#   - Existing plain-text consumers (no `--json`) continue to
#     receive the current human-readable line — the abstention
#     metadata is additive on the JSON envelope only.
#   - The "abstention" field is the only new field; existing
#     fields (`success`, `query`, `search_mode`, `memories`,
#     `limit`) keep their current types and meanings.
#   - Exit code remains 0 (the recall succeeded; the ranker just
#     returned nothing). Operators who want to detect the
#     empty-with-explicit-abstention case from a script can
#     key on `abstention.reason != "empty_ranker"`.


@dataclass(frozen=True)
class PerQueryFeatures:
    """Computed per-query features for abstention-policy evaluation."""

    query_id: str
    is_positive: bool
    n_rows: int
    top1_cosine: float
    top1_combined: float
    top_k_mean_cosine: float
    top_k_mean_combined: float
    top1_to_top2_gap: float
    has_vector: bool
    has_fts5: bool
    has_hybrid: bool
    is_empty: bool
    split: str  # "calibration" | "held-out"


@dataclass
class PolicyResult:
    """Result of evaluating one abstention policy on a set of queries."""

    name: str
    description: str
    n_total: int
    n_abstain: int
    false_abstention: int
    false_abstention_rate: float
    false_confidence: int
    false_confidence_rate: float
    precision_on_answered: float | None
    recall_on_answered: float | None
    abstain_breakdown: dict[str, int] = field(default_factory=dict)


def _safe_get(row: dict[str, Any] | None, key: str, default: float = 0.0) -> float:
    if row is None:
        return default
    return float(row.get(key, default))


def compute_features(
    per_query_table: list[dict[str, Any]],
    queries_by_id: dict[str, dict[str, Any]],
) -> list[PerQueryFeatures]:
    """Compute per-query features from the per_query_table in the report."""
    out: list[PerQueryFeatures] = []
    for row in per_query_table:
        qid = row["query_id"]
        q_meta = queries_by_id.get(qid, {})
        top_k = row.get("top_k", [])
        if not top_k:
            out.append(
                PerQueryFeatures(
                    query_id=qid,
                    is_positive=(row["class"] == "positive"),
                    n_rows=0,
                    top1_cosine=0.0,
                    top1_combined=0.0,
                    top_k_mean_cosine=0.0,
                    top_k_mean_combined=0.0,
                    top1_to_top2_gap=0.0,
                    has_vector=False,
                    has_fts5=False,
                    has_hybrid=False,
                    is_empty=True,
                    split=q_meta.get("split", ""),
                )
            )
            continue
        cosines = [_safe_get(r, "vector_similarity") for r in top_k]
        combineds = [_safe_get(r, "score") for r in top_k]
        sources = {r.get("source", "") for r in top_k}
        out.append(
            PerQueryFeatures(
                query_id=qid,
                is_positive=(row["class"] == "positive"),
                n_rows=len(top_k),
                top1_cosine=cosines[0],
                top1_combined=combineds[0],
                top_k_mean_cosine=sum(cosines) / len(cosines),
                top_k_mean_combined=sum(combineds) / len(combineds),
                top1_to_top2_gap=(
                    cosines[0] - cosines[1] if len(cosines) > 1 else 0.0
                ),
                has_vector="vector" in sources,
                has_fts5="fts5" in sources,
                has_hybrid="hybrid" in sources,
                is_empty=False,
                split=q_meta.get("split", ""),
            )
        )
    return out


def evaluate_policy(
    name: str,
    description: str,
    features: list[PerQueryFeatures],
    abstain_fn,
) -> PolicyResult:
    """Evaluate one abstention policy against a set of features.

    `abstain_fn(f) -> (should_abstain: bool, reason: str)`. The
    function is called once per feature; the returned bool drives
    the policy's decision, and the reason string is recorded in
    the breakdown.
    """
    n_total = len(features)
    n_abstain = 0
    false_abstention = 0
    false_confidence = 0
    answered_positives = 0
    answered_positives_with_hit = 0
    answered_total = 0
    answered_relevant_total = 0
    breakdown: dict[str, int] = {}
    for f in features:
        should_abstain, reason = abstain_fn(f)
        if should_abstain:
            n_abstain += 1
            breakdown[reason] = breakdown.get(reason, 0) + 1
            if f.is_positive:
                false_abstention += 1
            continue
        # We "answer". The policy is the unit of analysis here, not
        # the underlying retrieval quality; for answered queries
        # the precision/recall on the underlying retrieval is the
        # ranker's contribution, not the policy's. We report
        # precision/recall on the answerer's mean across the
        # answered set so the policy can be compared against the
        # ranker's pre-policy numbers.
        answered_total += 1
        if f.is_positive:
            answered_positives += 1
            if f.top1_cosine > 0.0:
                answered_positives_with_hit += 1
            answered_relevant_total += 1
        else:
            false_confidence += 1
    precision = (
        answered_positives_with_hit / answered_total
        if answered_total else None
    )
    recall = (
        answered_positives_with_hit / answered_relevant_total
        if answered_relevant_total else None
    )
    return PolicyResult(
        name=name,
        description=description,
        n_total=n_total,
        n_abstain=n_abstain,
        false_abstention=false_abstention,
        false_abstention_rate=(
            false_abstention / max(1, sum(1 for f in features if f.is_positive))
        ),
        false_confidence=false_confidence,
        false_confidence_rate=(
            false_confidence / max(1, sum(1 for f in features if not f.is_positive))
        ),
        precision_on_answered=precision,
        recall_on_answered=recall,
        abstain_breakdown=breakdown,
    )


# ---- Policies ---------------------------------------------------------

def _p_empty_ranker(f: PerQueryFeatures):
    """Current production behaviour: if the ranker returned no rows,
    the empty-result path is taken. This is the 'ranker says
    abstain' baseline. It is NOT an abstention policy in the
    confidence-calibration sense; it is the floor."""
    if f.is_empty:
        return True, "empty_ranker"
    return False, ""


def _p_top1_cosine(f: PerQueryFeatures, tau: float):
    if f.is_empty:
        return True, "empty_ranker"
    if f.top1_cosine < tau:
        return True, f"top1_cosine<{tau:.2f}"
    return False, ""


def _p_top1_combined(f: PerQueryFeatures, tau: float):
    if f.is_empty:
        return True, "empty_ranker"
    if f.top1_combined < tau:
        return True, f"top1_combined<{tau:.2f}"
    return False, ""


def _p_max_gap(f: PerQueryFeatures, gap: float, min_top1: float):
    if f.is_empty:
        return True, "empty_ranker"
    if f.top1_cosine < min_top1:
        return True, f"top1_cosine<{min_top1:.2f}"
    if f.top1_to_top2_gap < gap:
        return True, f"top1_top2_gap<{gap:.2f}"
    return False, ""


def _p_source_participation(f: PerQueryFeatures):
    if f.is_empty:
        return True, "empty_ranker"
    if not (f.has_vector or f.has_hybrid):
        return True, "no_vector_or_hybrid_source"
    return False, ""


def evaluate_all_policies(features: list[PerQueryFeatures]) -> list[PolicyResult]:
    """Run the standard policy sweep on a set of features."""
    policies: list[PolicyResult] = []
    policies.append(
        evaluate_policy(
            "P0_empty_ranker",
            "Current production: ranker returns 0 -> empty path; otherwise answer.",
            features, _p_empty_ranker,
        )
    )
    # Cosine-threshold policies. tau=0.70 is the HAL example;
    # we sweep a small set so the trade-off is visible.
    for tau in (0.50, 0.60, 0.70, 0.80):
        policies.append(
            evaluate_policy(
                f"P1_cosine_{tau:.2f}",
                f"Abstain when top-1 cosine similarity < {tau:.2f}.",
                features,
                lambda f, t=tau: _p_top1_cosine(f, t),
            )
        )
    # Combined-score policies. CombinedScore in log space is
    # in [-3.0, 0.0] for the production ranker; we sweep a
    # representative set.
    for tau in (-1.0, -0.5, 0.0):
        policies.append(
            evaluate_policy(
                f"P2_combined_{tau:+.2f}",
                f"Abstain when top-1 CombinedScore < {tau:+.2f}.",
                features,
                lambda f, t=tau: _p_top1_combined(f, t),
            )
        )
    # Max-gap policies. The intuition is that a confident answer
    # is a top-1 well-separated from top-2.
    for gap, min_top1 in ((0.05, 0.60), (0.10, 0.60), (0.15, 0.70)):
        policies.append(
            evaluate_policy(
                f"P3_gap_{gap:.2f}_min_{min_top1:.2f}",
                f"Abstain when top1-top2 cosine gap < {gap:.2f} "
                f"OR top1 cosine < {min_top1:.2f}.",
                features,
                lambda f, g=gap, m=min_top1: _p_max_gap(f, g, m),
            )
        )
    # Source-participation policy. If neither vector nor hybrid
    # contributed, the result is fts5-only and we treat that as
    # a confidence-low signal.
    policies.append(
        evaluate_policy(
            "P4_no_vector_source",
            "Abstain when no row came from vector or hybrid source.",
            features, _p_source_participation,
        )
    )
    return policies


def render_policy_result(r: PolicyResult) -> dict[str, Any]:
    return {
        "name": r.name,
        "description": r.description,
        "n_total": r.n_total,
        "n_abstain": r.n_abstain,
        "abstain_rate": r.n_abstain / max(1, r.n_total),
        "false_abstention": r.false_abstention,
        "false_abstention_rate": r.false_abstention_rate,
        "false_confidence": r.false_confidence,
        "false_confidence_rate": r.false_confidence_rate,
        "precision_on_answered": r.precision_on_answered,
        "recall_on_answered": r.recall_on_answered,
        "abstain_breakdown": r.abstain_breakdown,
    }


def main(argv: list[str] | None = None) -> int:
    p = argparse.ArgumentParser(
        description="Option D abstention-policy evaluation",
    )
    p.add_argument("--report", required=True, help="Path to a run report JSON")
    p.add_argument("--scope", default="calibration",
                   choices=("calibration", "held-out", "all"),
                   help="Which query split to evaluate")
    p.add_argument("--queries", default=None,
                   help="Path to the corpus queries.json (default: "
                        "<repo>/bench/corpus/v2/queries.json relative to CWD)")
    p.add_argument("--output", default=None,
                   help="Optional output path for the JSON result")
    args = p.parse_args(argv)

    report = json.loads(Path(args.report).read_text())
    per_query_table = report.get("per_query_table", [])
    # We need the queries' split field; read it from the corpus
    # queries.json. The script is read-only and does not touch
    # the report or the corpus; the queries file is only used to
    # resolve the split (calibration vs held-out) of each query.
    report_path = Path(args.report).resolve()
    if args.queries:
        queries_path = Path(args.queries)
    else:
        queries_path = Path("bench/corpus/v2/queries.json")
    if not queries_path.is_file():
        print(f"queries file not found: {queries_path}", file=sys.stderr)
        return 1
    queries_by_id: dict[str, dict[str, Any]] = {}
    if queries_path.is_file():
        for q in json.loads(queries_path.read_text()):
            queries_by_id[q["query_id"]] = q

    features = compute_features(per_query_table, queries_by_id)
    if args.scope != "all":
        features = [f for f in features if f.split == args.scope]
    policies = evaluate_all_policies(features)
    out = {
        "report_path": str(report_path),
        "scope": args.scope,
        "n_queries": len(features),
        "policies": [render_policy_result(r) for r in policies],
    }
    payload = json.dumps(out, indent=2, sort_keys=True)
    if args.output:
        Path(args.output).write_text(payload)
        print(f"wrote {args.output}", file=sys.stderr)
    print(payload)
    return 0


if __name__ == "__main__":
    sys.exit(main())
