"""B2 + B5 + B6 regression tests for `bench/option_d.py`.

B2 (HAL 2026-10-19): the pre-fix combined-score thresholds
(-1.0, -0.5, 0.0) were unreachable for vector-only candidates
(CombinedScore = cosine * VectorWeight = cosine * 0.5, in [0,
0.5]). The new meaningful thresholds for vector-only are
+0.30, +0.35, +0.40 (equivalent to raw cosine 0.60, 0.70,
0.80). The equivalence is exact ONLY for vector-only
candidates (rows whose `top1_combined == 0.5 *
`top1_cosine`).

B5 (HAL 2026-10-19): the JSON output now exposes a `metric_kind`
field classifying the answered-subset metrics as
QUERY-LEVEL ANSWERABILITY CLASSIFICATION metrics for the
ANSWERED subset, NOT retrieval-ranking Precision@k /
Recall@k.

B6 (HAL 2026-10-19): the previous fields
`precision_on_answered` and `recall_on_answered` were
mathematically misleading — `recall_on_answered` was
`answered_positives_with_hit / answered_positives`, so a
policy that abstained on most positives still reported
recall=1.0 on its small answered set. They are replaced with
explicit, non-misleading names: `answerability_precision`
and `positive_query_coverage`. Both are `None` when their
denominator is zero.

These tests pin all three contracts without depending on the
canonical mpm binary.
"""

from __future__ import annotations

import json
import subprocess
import sys
import unittest
from pathlib import Path

from bench.option_d import (
    PerQueryFeatures,
    PolicyResult,
    evaluate_all_policies,
    evaluate_policy,
    render_policy_result,
)


def _vf(
    query_id: str,
    is_positive: bool,
    top1_cosine: float,
    top1_combined: float,
    *,
    has_vector: bool = True,
    has_hybrid: bool = False,
    has_fts5: bool = False,
    n_rows: int = 5,
    is_empty: bool = False,
) -> PerQueryFeatures:
    return PerQueryFeatures(
        query_id=query_id,
        is_positive=is_positive,
        n_rows=n_rows,
        top1_cosine=top1_cosine,
        top1_combined=top1_combined,
        top_k_mean_cosine=top1_cosine,
        top_k_mean_combined=top1_combined,
        top1_to_top2_gap=0.0,
        has_vector=has_vector,
        has_fts5=has_fts5,
        has_hybrid=has_hybrid,
        is_empty=is_empty,
        split="calibration",
    )


def _run_option_d_main(report_path: Path, output_path: Path) -> None:
    """Run the option_d CLI against a synthetic report. Returns
    the JSON payload as a dict."""
    proc = subprocess.run(
        [
            sys.executable,
            "-m",
            "bench.option_d",
            "--report",
            str(report_path),
            "--scope",
            "calibration",
            "--output",
            str(output_path),
        ],
        cwd=str(Path(__file__).resolve().parents[2]),
        capture_output=True,
        text=True,
        timeout=60,
    )
    if proc.returncode != 0:
        raise RuntimeError(
            f"bench.option_d failed: rc={proc.returncode} "
            f"stderr={proc.stderr[:500]}"
        )


class TestB2CombinedScoreEquivalence(unittest.TestCase):
    """B2: combined-score thresholds +0.30/+0.35/+0.40 are
    exactly equivalent to raw-cosine 0.60/0.70/0.80 for
    vector-only candidates. The equivalence is broken for
    hybrid or FTS5-only candidates.
    """

    def test_vector_only_threshold_equivalence(self) -> None:
        """For VECTOR-ONLY candidates, the combined-score
        threshold t (with VectorWeight=0.5) abstains on
        exactly the same set of queries as the cosine
        threshold 2t.
        """
        # Vector-only candidates: top1_combined == 0.5 * top1_cosine
        features = [
            _vf("q-p-hi",  is_positive=True,  top1_cosine=0.95, top1_combined=0.475),  # both above
            _vf("q-p-mid", is_positive=True,  top1_cosine=0.65, top1_combined=0.325),  # cosine 0.65 above 0.60; combined 0.325 above 0.30
            _vf("q-p-lo",  is_positive=True,  top1_cosine=0.55, top1_combined=0.275),  # both below
            _vf("q-p-bd",  is_positive=True,  top1_cosine=0.60, top1_combined=0.30),   # boundary — both equal, not abstained
            _vf("q-p-bd2", is_positive=True,  top1_cosine=0.59, top1_combined=0.295),  # both below
            _vf("q-n-hi",  is_positive=False, top1_cosine=0.90, top1_combined=0.45),   # both above (false-confidence)
            _vf("q-n-lo",  is_positive=False, top1_cosine=0.40, top1_combined=0.20),   # both below (correctly abstained)
        ]
        policies = {p.name: p for p in evaluate_all_policies(features)}

        # Cosine 0.60 vs Combined 0.30 (both vector-only equivalent)
        cos_060 = policies["P1_cosine_0.60"]
        com_030 = policies["P2_combined_+0.30"]
        self.assertEqual(
            cos_060.n_abstain, com_030.n_abstain,
            f"cosine 0.60 abstained {cos_060.n_abstain} queries but "
            f"combined +0.30 abstained {com_030.n_abstain} for vector-"
            f"only candidates; the equivalence should be exact"
        )
        self.assertEqual(
            cos_060.false_abstention, com_030.false_abstention,
            f"cosine 0.60 false-abstentions={cos_060.false_abstention} "
            f"!= combined +0.30 false-abstentions={com_030.false_abstention}"
        )
        self.assertEqual(
            cos_060.false_confidence, com_030.false_confidence,
            f"cosine 0.60 false-confidences={cos_060.false_confidence} "
            f"!= combined +0.30 false-confidences={com_030.false_confidence}"
        )

        # Cosine 0.70 vs Combined 0.35
        cos_070 = policies["P1_cosine_0.70"]
        com_035 = policies["P2_combined_+0.35"]
        self.assertEqual(cos_070.n_abstain, com_035.n_abstain)
        self.assertEqual(cos_070.false_abstention, com_035.false_abstention)
        self.assertEqual(cos_070.false_confidence, com_035.false_confidence)

        # Cosine 0.80 vs Combined 0.40
        cos_080 = policies["P1_cosine_0.80"]
        com_040 = policies["P2_combined_+0.40"]
        self.assertEqual(cos_080.n_abstain, com_040.n_abstain)
        self.assertEqual(cos_080.false_abstention, com_040.false_abstention)
        self.assertEqual(cos_080.false_confidence, com_040.false_confidence)

    def test_legacy_unreachable_thresholds_abstain_zero(self) -> None:
        """The pre-fix thresholds (-1.0, -0.5, 0.0) were
        documented as CombinedScore in log-space [-3.0, 0.0]
        but for vector-only candidates CombinedScore is in
        [0, 0.5]. With all vector-only rows at cosine >=0.4
        (combined >=0.2), the legacy thresholds abstained
        zero queries. The B2 fix retains the legacy names
        for backward compatibility but documents them as
        unreachable for vector-only; this test pins that.
        """
        features = [
            _vf("q-p-1", is_positive=True,  top1_cosine=0.50, top1_combined=0.25),
            _vf("q-p-2", is_positive=True,  top1_cosine=0.45, top1_combined=0.225),
            _vf("q-n-1", is_positive=False, top1_cosine=0.40, top1_combined=0.20),
        ]
        policies = {p.name: p for p in evaluate_all_policies(features)}
        for tau in (-1.0, -0.5, 0.0):
            name = f"P2_combined_{tau:+.2f}"
            self.assertEqual(
                policies[name].n_abstain, 0,
                f"legacy combined threshold {tau} abstained "
                f"{policies[name].n_abstain} queries; the B2 "
                f"contract pins this as 0 for vector-only rows"
            )

    def test_equivalence_breaks_for_hybrid_rows(self) -> None:
        """For HYBRID rows, CombinedScore includes the FTS5
        BM25 contribution and is NOT 0.5*cosine. The B2
        equivalence does not apply; we demonstrate this by
        constructing a hybrid row where the combined and
        cosine thresholds disagree.
        """
        # Hybrid row: cosine=0.55, combined=0.45 (FTS5
        # pushed the combined score up). Cosine 0.60
        # abstains; Combined 0.30 does NOT.
        features = [
            _vf("q-hybrid", is_positive=True,
                top1_cosine=0.55, top1_combined=0.45,
                has_vector=True, has_hybrid=True, has_fts5=True),
        ]
        policies = {p.name: p for p in evaluate_all_policies(features)}
        # Cosine 0.60 abstains (0.55 < 0.60).
        self.assertEqual(policies["P1_cosine_0.60"].n_abstain, 1)
        # Combined 0.30 does NOT abstain (0.45 >= 0.30).
        self.assertEqual(policies["P2_combined_+0.30"].n_abstain, 0)
        # The hybrid row breaks the equivalence — that is
        # the B2 contract: equivalence holds ONLY for
        # vector-only candidates.

    def test_combined_thresholds_present_in_sweep(self) -> None:
        """The B2 sweep must include the new combined
        thresholds (+0.30, +0.35, +0.40). If a future
        edit accidentally removes them, the test fails.
        """
        features = [_vf("q", is_positive=True, top1_cosine=0.5, top1_combined=0.25)]
        names = {p.name for p in evaluate_all_policies(features)}
        for expected in ("P2_combined_+0.30", "P2_combined_+0.35", "P2_combined_+0.40"):
            self.assertIn(
                expected, names,
                f"{expected!r} missing from the policy sweep; "
                f"B2 fix regressed"
            )


class TestB5MetricSemantics(unittest.TestCase):
    """B5: `answerability_precision` and `positive_query_coverage`
    are QUERY-LEVEL ANSWERABILITY CLASSIFICATION metrics for
    the ANSWERED subset, not retrieval Precision@k /
    Recall@k. The `metric_kind` field in the rendered JSON
    classifies them explicitly.
    """

    def test_rendered_json_has_metric_kind(self) -> None:
        """The rendered JSON output carries a `metric_kind`
        field classifying the answered-subset metrics as
        answerability classification, not retrieval
        Precision@k / Recall@k.
        """
        features = [
            _vf("q-p", is_positive=True,  top1_cosine=0.7, top1_combined=0.35),
            _vf("q-n", is_positive=False, top1_cosine=0.7, top1_combined=0.35),
        ]
        policy = evaluate_policy(
            "P1_cosine_0.70",
            "Abstain when top-1 cosine similarity < 0.70.",
            features,
            lambda f: (f.top1_cosine < 0.70, "top1_cosine<0.70"),
        )
        out = render_policy_result(policy)
        self.assertEqual(
            out["metric_kind"],
            "query_level_answerability_classification",
            "metric_kind must classify the answered-subset "
            "metrics as answerability classification, not "
            "retrieval ranking metrics"
        )
        # B6: explicit, non-misleading names. The misleading
        # `precision_on_answered` / `recall_on_answered` aliases
        # have been removed.
        self.assertIn("answerability_precision", out)
        self.assertIn("positive_query_coverage", out)
        self.assertNotIn("precision_on_answered", out)
        self.assertNotIn("recall_on_answered", out)
        self.assertNotIn("precision_at_k", out)
        self.assertNotIn("recall_at_k", out)


class TestB6QueryLevelRecall(unittest.TestCase):
    """B6 (HAL 2026-10-19): `answerability_precision` and
    `positive_query_coverage` are explicit, non-misleading
    names. `positive_query_coverage` = accepted positives /
    all positives in the scope — so abstaining on most
    positives yields a low value, NOT a misleading 1.0.

    The five regression tests below correspond exactly to the
    HAL directive's required cases:

      1. A policy that answers every positive query.
      2. A policy that abstains on some positives.
      3. A policy that abstains on all positives.
      4. A policy that answers only negative queries.
      5. A scope with no positive queries.

    These tests establish that refusing answerable queries
    reduces positive-query coverage — the core B6 invariant.
    """

    def test_policy_answers_every_positive_query(self) -> None:
        """Coverage case 1: every positive query is answered
        and every negative query is abstained. The policy is
        `abstain if cosine < 0.5`; positives have cosine
        0.9, negatives have cosine 0.05.

        Expected:
          - positive_query_coverage = 3/3 = 1.0
          - answerability_precision = 3/3 = 1.0
          - false_abstention = 0
          - false_confidence = 0
        """
        features = [
            _vf("q-p-1", is_positive=True,  top1_cosine=0.9, top1_combined=0.45),
            _vf("q-p-2", is_positive=True,  top1_cosine=0.9, top1_combined=0.45),
            _vf("q-p-3", is_positive=True,  top1_cosine=0.9, top1_combined=0.45),
            _vf("q-n-1", is_positive=False, top1_cosine=0.05, top1_combined=0.025),
            _vf("q-n-2", is_positive=False, top1_cosine=0.05, top1_combined=0.025),
        ]
        policy = evaluate_policy(
            "answers_all_positives",
            "Abstain when top-1 cosine < 0.5.",
            features,
            lambda f: (f.top1_cosine < 0.5, "top1_cosine<0.50"),
        )
        self.assertEqual(policy.n_total, 5)
        self.assertEqual(policy.n_total_positives, 3)
        self.assertEqual(policy.n_accepted, 3)
        self.assertEqual(policy.n_accepted_positives, 3)
        self.assertEqual(policy.false_abstention, 0)
        self.assertEqual(policy.false_confidence, 0)
        self.assertEqual(policy.answerability_precision, 1.0)
        self.assertEqual(policy.positive_query_coverage, 1.0)

    def test_policy_abstains_on_some_positives(self) -> None:
        """Coverage case 2: the policy abstains on some
        positives but not all. The policy is `abstain if
        cosine < 0.5`; 3 positives have cosine {0.9, 0.3,
        0.3}, so 2 of 3 positives are abstained. Negatives
        have cosine 0.05 and are also abstained.

        Expected:
          - positive_query_coverage = 1/3 (one positive answered)
          - answerability_precision = 1/1 (only positives answered)
          - false_abstention = 2 (the two abstained positives)
          - false_confidence = 0
        """
        features = [
            _vf("q-p-1", is_positive=True,  top1_cosine=0.9, top1_combined=0.45),  # answered
            _vf("q-p-2", is_positive=True,  top1_cosine=0.3, top1_combined=0.15),  # abstained
            _vf("q-p-3", is_positive=True,  top1_cosine=0.3, top1_combined=0.15),  # abstained
            _vf("q-n-1", is_positive=False, top1_cosine=0.05, top1_combined=0.025),  # abstained
            _vf("q-n-2", is_positive=False, top1_cosine=0.05, top1_combined=0.025),  # abstained
        ]
        policy = evaluate_policy(
            "abstains_on_some",
            "Abstain when top-1 cosine < 0.5.",
            features,
            lambda f: (f.top1_cosine < 0.5, "top1_cosine<0.50"),
        )
        self.assertEqual(policy.n_total, 5)
        self.assertEqual(policy.n_total_positives, 3)
        self.assertEqual(policy.n_accepted, 1)
        self.assertEqual(policy.n_accepted_positives, 1)
        self.assertEqual(policy.false_abstention, 2)
        self.assertEqual(policy.false_confidence, 0)
        self.assertAlmostEqual(policy.answerability_precision, 1.0, places=6)
        self.assertAlmostEqual(
            policy.positive_query_coverage, 1.0 / 3.0, places=6,
            msg="abstaining on some positives must reduce "
                "positive_query_coverage below 1.0; this is "
                "the B6 invariant that the pre-fix "
                "`recall_on_answered = answered/answered` "
                "computation violated"
        )

    def test_policy_abstains_on_all_positives(self) -> None:
        """Coverage case 3: the policy abstains on every
        positive query. The policy is `abstain if cosine <
        0.5`; all positives have cosine 0.3.

        Expected:
          - positive_query_coverage = 0/N = 0.0
          - answerability_precision = 0/M = 0.0 (only negatives answered)
          - false_abstention = N
          - false_confidence = M (every negative answered)
        """
        features = [
            _vf("q-p-1", is_positive=True,  top1_cosine=0.3, top1_combined=0.15),
            _vf("q-p-2", is_positive=True,  top1_cosine=0.3, top1_combined=0.15),
            _vf("q-n-1", is_positive=False, top1_cosine=0.9, top1_combined=0.45),  # answered
            _vf("q-n-2", is_positive=False, top1_cosine=0.9, top1_combined=0.45),  # answered
        ]
        policy = evaluate_policy(
            "abstains_on_all_positives",
            "Abstain when top-1 cosine < 0.5.",
            features,
            lambda f: (f.top1_cosine < 0.5, "top1_cosine<0.50"),
        )
        self.assertEqual(policy.n_total, 4)
        self.assertEqual(policy.n_total_positives, 2)
        self.assertEqual(policy.n_accepted, 2)
        self.assertEqual(policy.n_accepted_positives, 0)
        self.assertEqual(policy.false_abstention, 2)
        self.assertEqual(policy.false_confidence, 2)
        self.assertEqual(policy.answerability_precision, 0.0)
        self.assertEqual(
            policy.positive_query_coverage, 0.0,
            "abstaining on every positive query must give "
            "positive_query_coverage = 0.0; the pre-fix "
            "`recall_on_answered` was meaningless in this case"
        )

    def test_policy_answers_only_negative_queries(self) -> None:
        """Coverage case 4: every negative query is answered
        and every positive query is abstained. The policy is
        `abstain if cosine < 0.5`; positives have cosine 0.3,
        negatives have cosine 0.9.

        Expected:
          - positive_query_coverage = 0/N = 0.0
          - answerability_precision = 0/M = 0.0 (only negatives answered)
          - false_abstention = N
          - false_confidence = M
        """
        features = [
            _vf("q-p-1", is_positive=True,  top1_cosine=0.3, top1_combined=0.15),
            _vf("q-p-2", is_positive=True,  top1_cosine=0.3, top1_combined=0.15),
            _vf("q-n-1", is_positive=False, top1_cosine=0.9, top1_combined=0.45),
            _vf("q-n-2", is_positive=False, top1_cosine=0.9, top1_combined=0.45),
        ]
        policy = evaluate_policy(
            "answers_only_negatives",
            "Abstain when top-1 cosine < 0.5.",
            features,
            lambda f: (f.top1_cosine < 0.5, "top1_cosine<0.50"),
        )
        self.assertEqual(policy.n_total, 4)
        self.assertEqual(policy.n_total_positives, 2)
        self.assertEqual(policy.n_accepted, 2)
        self.assertEqual(policy.n_accepted_positives, 0)
        self.assertEqual(policy.answerability_precision, 0.0)
        self.assertEqual(
            policy.positive_query_coverage, 0.0,
            "when the policy answers only negatives, "
            "positive_query_coverage = 0.0; the pre-fix "
            "`recall_on_answered` (which was computed only on "
            "the answered set) silently dropped to 0/0 and "
            "returned None — which is why the pre-fix field "
            "was not just misleading but also semantically "
            "absent"
        )

    def test_scope_with_no_positive_queries(self) -> None:
        """Coverage case 5: the scope has no positive queries
        (only negatives). Any policy is evaluated.

        Expected:
          - n_total_positives = 0
          - positive_query_coverage = None (zero denominator)
          - answerability_precision = 0/M (no positives answered)
        """
        features = [
            _vf("q-n-1", is_positive=False, top1_cosine=0.9, top1_combined=0.45),
            _vf("q-n-2", is_positive=False, top1_cosine=0.3, top1_combined=0.15),
        ]
        policy = evaluate_policy(
            "no_positives",
            "Abstain when top-1 cosine < 0.5.",
            features,
            lambda f: (f.top1_cosine < 0.5, "top1_cosine<0.50"),
        )
        self.assertEqual(policy.n_total, 2)
        self.assertEqual(policy.n_total_positives, 0)
        # Zero denominator → explicit None (JSON null). The
        # pre-fix computation (n_accepted_positives_with_hit /
        # n_accepted_positives) silently produced
        # `0 / 0 = error` or `None`, masking the absent meaning.
        self.assertIsNone(
            policy.positive_query_coverage,
            "positive_query_coverage must be None when the "
            "scope has no positive queries; the absent-meaning "
            "must be explicit (JSON null), not 0 or 1"
        )
        self.assertEqual(policy.answerability_precision, 0.0)

    def test_refusing_answerable_queries_reduces_coverage(self) -> None:
        """B6 core invariant: as the policy abstains on more
        positives, `positive_query_coverage` strictly decreases
        — i.e. it is a real coverage measure, not the
        pre-fix `recall_on_answered = answered/answered` which
        stayed at 1.0 regardless of abstention rate.
        """
        # All positives at cosine 0.1. Policy abstains on
        # cosine < tau; lowering tau from 0.5 (above) to
        # 0.05 (just above 0.1) increases the answered set.
        features = [
            _vf(f"q-p-{i}", is_positive=True, top1_cosine=0.1, top1_combined=0.05)
            for i in range(5)
        ]

        @staticmethod
        def abstain_below(tau: float):
            def _fn(f: PerQueryFeatures):
                return (f.top1_cosine < tau, f"top1_cosine<{tau:.2f}")
            return _fn

        strict_policy = evaluate_policy(
            "strict_0.50", "Abstain when cosine < 0.50.",
            features, abstain_below(0.50),
        )
        lenient_policy = evaluate_policy(
            "lenient_0.05", "Abstain when cosine < 0.05.",
            features, abstain_below(0.05),
        )
        # The strict policy abstains on every positive
        # (cosine 0.1 < 0.50), so coverage = 0.
        self.assertEqual(strict_policy.n_accepted_positives, 0)
        self.assertEqual(strict_policy.positive_query_coverage, 0.0)
        # The lenient policy answers every positive (cosine
        # 0.1 >= 0.05), so coverage = 1.0.
        self.assertEqual(lenient_policy.n_accepted_positives, 5)
        self.assertEqual(lenient_policy.positive_query_coverage, 1.0)
        # The invariant itself: a more lenient threshold yields
        # strictly greater positive-query coverage.
        self.assertLess(
            strict_policy.positive_query_coverage,
            lenient_policy.positive_query_coverage,
            "B6 invariant: refusing answerable queries "
            "reduces positive_query_coverage"
        )

    def test_render_policy_result_exposes_new_fields(self) -> None:
        """The rendered JSON output exposes
        `answerability_precision`, `positive_query_coverage`,
        `n_accepted`, `n_total_positives`,
        `n_accepted_positives` — and does NOT expose the
        pre-fix misleading names.
        """
        features = [
            _vf("q-p", is_positive=True,  top1_cosine=0.7, top1_combined=0.35),
            _vf("q-n", is_positive=False, top1_cosine=0.3, top1_combined=0.15),
        ]
        policy = evaluate_policy(
            "P1_cosine_0.70",
            "Abstain when top-1 cosine similarity < 0.70.",
            features,
            lambda f: (f.top1_cosine < 0.70, "top1_cosine<0.70"),
        )
        out = render_policy_result(policy)
        self.assertEqual(out["n_accepted"], 1)
        self.assertEqual(out["n_total_positives"], 1)
        self.assertEqual(out["n_accepted_positives"], 1)
        self.assertEqual(out["answerability_precision"], 1.0)
        self.assertEqual(out["positive_query_coverage"], 1.0)
        # Pre-fix names must NOT be silently retained.
        self.assertNotIn("precision_on_answered", out)
        self.assertNotIn("recall_on_answered", out)


class TestOptionDCLI(unittest.TestCase):
    """End-to-end: `python -m bench.option_d --report ...` runs
    against a synthetic report.json and produces the B2/B5/B6
    output without depending on the canonical mpm binary.
    """

    def test_cli_emits_b2_thresholds_and_b5_metric_kind_and_b6_fields(self) -> None:
        import tempfile
        with tempfile.TemporaryDirectory() as td:
            td = Path(td)
            report_path = td / "report.json"
            output_path = td / "out.json"
            report = {
                "per_query_table": [
                    {
                        "query_id": "q1",
                        "class": "positive",
                        "top_k": [
                            {
                                "id": "d01",
                                "source": "vector",
                                "score": 0.65,
                                "vector_similarity": 0.65,
                            }
                        ],
                    },
                    {
                        "query_id": "q2",
                        "class": "negative",
                        "top_k": [
                            {
                                "id": "d02",
                                "source": "vector",
                                "score": 0.55,
                                "vector_similarity": 0.55,
                            }
                        ],
                    },
                ],
            }
            queries_path = td / "queries.json"
            queries_path.write_text(json.dumps([
                {"query_id": "q1", "split": "calibration"},
                {"query_id": "q2", "split": "calibration"},
            ]))
            report_path.write_text(json.dumps(report))
            _run_option_d_main(report_path, output_path)
            out = json.loads(output_path.read_text())
            names = {p["name"] for p in out["policies"]}
            for expected in ("P2_combined_+0.30", "P2_combined_+0.35", "P2_combined_+0.40"):
                self.assertIn(expected, names)
            for p in out["policies"]:
                self.assertEqual(
                    p["metric_kind"],
                    "query_level_answerability_classification",
                )
                # B6: new explicit field names emitted; old
                # misleading names NOT silently retained.
                self.assertIn("answerability_precision", p)
                self.assertIn("positive_query_coverage", p)
                self.assertIn("n_accepted", p)
                self.assertIn("n_total_positives", p)
                self.assertIn("n_accepted_positives", p)
                self.assertNotIn("precision_on_answered", p)
                self.assertNotIn("recall_on_answered", p)


if __name__ == "__main__":
    unittest.main()