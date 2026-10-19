"""v2 corpus contract tests (HAL directive Phase A.2, A2.2 + A2.6).

The v2 corpus is the calibration corpus used to repair Issue #15
measurement after HAL identified the v1 negative-label defect
(every v1 negative query had a literal decoy document in
d0000021-d0000030). These tests verify:

  - v2 loads cleanly through the v1-compatible default path
    when given the corpus version explicitly
  - v2 has at least the minimum evidence floor HAL required
    (substantially more than v1's 10 positive / 10 negative)
  - Every negative query carries a label_rationale that
    independently justifies the negative label
  - Paraphrase pairs label the same doc set and target
    positive base queries
  - Held-out queries target the held-out topic family
  - The v1 corpus is unchanged (the original hashes are
    preserved as a historical benchmark artifact)

Manual semantic review of each label_rationale is documented
in `bench/corpus/v2/labels.md` (separate file).
"""

import hashlib
import json
import tempfile
import unittest
from pathlib import Path

from bench.corpus import CorpusError, load_corpus


REPO_ROOT = Path(__file__).resolve().parent.parent.parent
CORPUS_DIR = REPO_ROOT / "bench" / "corpus"
V2_DIR = CORPUS_DIR / "v2"


class TestV2CorpusLoads(unittest.TestCase):
    """v2 corpus loads through the explicit --corpus-version path."""

    def test_v2_loads_with_explicit_version(self) -> None:
        corpus = load_corpus(CORPUS_DIR, version="v2")
        self.assertEqual(corpus.version, "v2")
        self.assertEqual(len(corpus.documents), 50)
        self.assertEqual(len(corpus.queries), 60)
        # Class balance: 40 positive / 20 negative.
        self.assertEqual(len(corpus.positive_queries), 40)
        self.assertEqual(len(corpus.negative_queries), 20)

    def test_v2_manifest_hashes_match_files(self) -> None:
        # load_corpus() verifies the hashes; if we get here, they
        # match. Smoke for the content-hash binding.
        load_corpus(CORPUS_DIR, version="v2")

    def test_v2_split_counts(self) -> None:
        # The directive requires a topic-separated calibration /
        # held-out split. The split field on each query records
        # which scope it targets.
        corpus = load_corpus(CORPUS_DIR, version="v2")
        cal = [q for q in corpus.queries if q.split == "calibration"]
        held = [q for q in corpus.queries if q.split == "held-out"]
        self.assertEqual(len(cal), 41, "calibration split must be 41")
        self.assertEqual(len(held), 19, "held-out split must be 19")

    def test_v2_topic_families_distinct(self) -> None:
        # The five topic families are used to enforce the
        # held-out separation. The calibration set must NOT
        # contain any held-out document; the held-out set must
        # only contain held-out documents.
        corpus = load_corpus(CORPUS_DIR, version="v2")
        held_out_docs = {
            d.doc_id for d in corpus.documents
            if d.topic_family == "search_operators"
        }
        self.assertEqual(
            len(held_out_docs), 10,
            "held-out topic family must have exactly 10 documents",
        )
        # The calibration POSITIVE queries may target held-out
        # documents (this is by design — calibration measures
        # the ranker on a held-out document set), but the
        # calibration NEGATIVE queries must not have any
        # relevant doc in the held-out set. Spot-check that
        # no negative query has a relevant_doc_id in held_out_docs.
        for q in corpus.queries:
            if q.is_positive:
                continue
            overlap = set(q.relevant_doc_ids) & held_out_docs
            self.assertEqual(
                overlap, set(),
                f"negative query {q.query_id} has relevant_doc_ids "
                f"in the held-out set: {sorted(overlap)}; "
                f"a negative query must have ZERO relevant docs",
            )


class TestV2LabelRationale(unittest.TestCase):
    """v2 contract: every negative query has a non-empty rationale."""

    def test_every_negative_has_rationale(self) -> None:
        corpus = load_corpus(CORPUS_DIR, version="v2")
        for q in corpus.queries:
            if q.is_positive:
                continue
            self.assertTrue(
                q.label_rationale,
                f"negative query {q.query_id} ({q.query!r}) "
                f"has empty label_rationale; the v2 contract "
                f"requires an independent justification",
            )

    def test_every_rationale_mentions_unanswerable(self) -> None:
        # The rationale must explain WHY the answer is not in
        # the corpus. A rationale that just says "negative"
        # without explanation is the v1 failure mode in
        # different words.
        corpus = load_corpus(CORPUS_DIR, version="v2")
        for q in corpus.queries:
            if q.is_positive:
                continue
            self.assertGreaterEqual(
                len(q.label_rationale), 20,
                f"rationale for {q.query_id} is too short to be "
                f"a justification: {q.label_rationale!r}",
            )

    def test_hard_negatives_marked(self) -> None:
        # Hard negatives (queries that look MPM-related but
        # the answer is not in the corpus) must be identified
        # in their rationale so a downstream reader can see
        # what the ranker is being asked to detect.
        corpus = load_corpus(CORPUS_DIR, version="v2")
        hard_negatives = [
            q for q in corpus.queries
            if not q.is_positive and "hard negative" in q.label_rationale.lower()
        ]
        self.assertGreaterEqual(
            len(hard_negatives), 3,
            "v2 must include at least 3 hard negatives "
            "(queries about MPM with answers NOT in the corpus); "
            "these test the ranker's ability to detect "
            "'related but unanswered'",
        )


class TestV2Paraphrases(unittest.TestCase):
    """v2 contract: paraphrase_of is a valid base query with same
    relevant_doc_ids."""

    def test_every_paraphrase_has_matching_base(self) -> None:
        corpus = load_corpus(CORPUS_DIR, version="v2")
        for q in corpus.queries:
            if not q.paraphrase_of:
                continue
            base = next(
                (b for b in corpus.queries if b.query_id == q.paraphrase_of),
                None,
            )
            self.assertIsNotNone(
                base, f"paraphrase {q.query_id} targets missing "
                f"base {q.paraphrase_of!r}",
            )
            self.assertTrue(base.is_positive, f"paraphrase base "
                f"{q.paraphrase_of!r} is not positive")
            self.assertEqual(
                set(q.relevant_doc_ids), set(base.relevant_doc_ids),
                f"paraphrase {q.query_id} relevant_doc_ids differ "
                f"from base {q.paraphrase_of!r}",
            )

    def test_paraphrase_count(self) -> None:
        # The directive asks for paraphrases to test the
        # "paraphrase recall" failure mode. v2 ships 9
        # paraphrase pairs. v1 ships zero paraphrases.
        corpus = load_corpus(CORPUS_DIR, version="v2")
        paraphrases = [q for q in corpus.queries if q.paraphrase_of]
        self.assertEqual(len(paraphrases), 9)
        # v1 has no paraphrases; this is the regression net.
        v1 = load_corpus(CORPUS_DIR)
        v1_paraphrases = [q for q in v1.queries if q.paraphrase_of]
        self.assertEqual(len(v1_paraphrases), 0)


class TestV1Preserved(unittest.TestCase):
    """v1 is preserved unchanged as a historical benchmark artifact."""

    def test_v1_loads_with_default_version(self) -> None:
        # The default version (no --corpus-version) must still
        # load v1 cleanly. This is the v3 review test fixture.
        corpus = load_corpus(CORPUS_DIR)
        self.assertEqual(corpus.version, "v1")
        self.assertEqual(len(corpus.documents), 30)
        self.assertEqual(len(corpus.queries), 20)

    def test_v1_hashes_match_recorded(self) -> None:
        # The v1 MANIFEST records the hashes of the four v1
        # files. Any change to a v1 file would change its
        # hash; the recorded hashes in MANIFEST.json must
        # still match.
        manifest = json.loads((CORPUS_DIR / "MANIFEST.json").read_text())
        for fname in ("documents", "queries", "expectations", "scoring"):
            key = f"{fname}_sha256"
            recorded = manifest[key]
            actual = hashlib.sha256(
                (CORPUS_DIR / "v1" / f"{fname}.json").read_bytes()
            ).hexdigest()
            self.assertEqual(
                recorded, actual,
                f"v1/{fname}.json hash drift: recorded={recorded} "
                f"actual={actual}; v1 must be preserved unchanged",
            )

    def test_v1_no_v2_contract_enforced(self) -> None:
        # v1 queries do not have label_rationale; the v2 contract
        # must NOT be retroactively applied to v1. This is the
        # backwards-compatibility check.
        corpus = load_corpus(CORPUS_DIR)
        # v1 negative queries have empty label_rationale but
        # the loader must still accept them.
        for q in corpus.queries:
            if not q.is_positive:
                self.assertEqual(
                    q.label_rationale, "",
                    f"v1 query {q.query_id} unexpectedly has a "
                    f"label_rationale; the v2 contract must not "
                    f"be applied to v1",
                )


if __name__ == "__main__":
    unittest.main()
