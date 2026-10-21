"""Issue #15 contract tests (hermetic — no live DB, no network, no binary).

The Issue #15 remedy changes the *semantics* of the semantic-recall
output, not the retrieval itself:

  - `mpm recall --semantic --json` always emits a valid JSON envelope,
    including when the ranker produced nothing.
  - The envelope carries an explicit `retrieval_status`:
    "no_candidates" or "candidates_unverified".
  - The human-readable path says "no candidates", not "no memories".
  - An embedding-provider failure is an error, not an empty result.

The harness has to keep reading the same thing it always read. The
committed Phase A.3 report labels an empty semantic result
`search_mode: "hybrid_empty"`, and that label must survive the contract
change — otherwise the baseline artefact stops being reproducible and a
metric difference would be reported that is really a labelling
difference.

These tests pin the harness side of that: the explicit field is read
when it is present, the legacy plain-text sniffing still works for a
pre-Issue-15 binary, and the label is identical either way.
"""

import json
import unittest
from unittest import mock

from bench.corpus import Query
from bench.runner import (
    NO_CANDIDATES_STATUS,
    _is_plain_text_empty,
    _search_mode_for_envelope,
    run_query,
)


class TestEmptyResultMarkers(unittest.TestCase):
    """Both empty-result markers must be recognised."""

    def test_legacy_marker_still_recognised(self) -> None:
        self.assertTrue(_is_plain_text_empty("No memories found."))
        self.assertTrue(_is_plain_text_empty("  No memories found for query  "))

    def test_semantic_candidate_marker_recognised(self) -> None:
        self.assertTrue(_is_plain_text_empty("No candidates returned for: q"))
        self.assertTrue(_is_plain_text_empty("  no candidates returned for: q  "))

    def test_json_output_is_not_a_plain_text_empty(self) -> None:
        self.assertFalse(_is_plain_text_empty(""))
        self.assertFalse(_is_plain_text_empty('{"memories": []}'))


class TestSearchModeClassification(unittest.TestCase):
    """The harness label must be the same before and after the change."""

    def test_explicit_no_candidates_status_maps_to_hybrid_empty(self) -> None:
        envelope = {
            "query": "q",
            "search_mode": "hybrid",
            "count": 0,
            "retrieval_status": NO_CANDIDATES_STATUS,
            "memories": [],
        }
        self.assertEqual("hybrid_empty", _search_mode_for_envelope(envelope))

    def test_unverified_candidates_keep_the_hybrid_label(self) -> None:
        envelope = {
            "query": "q",
            "search_mode": "hybrid",
            "count": 2,
            "retrieval_status": "candidates_unverified",
            "memories": [{"id": "a"}, {"id": "b"}],
        }
        self.assertEqual("hybrid", _search_mode_for_envelope(envelope))

    def test_envelope_without_the_field_keeps_the_legacy_behaviour(self) -> None:
        # A pre-Issue-15 binary emits no retrieval_status at all.
        envelope = {"query": "q", "search_mode": "hybrid", "memories": []}
        self.assertEqual("hybrid", _search_mode_for_envelope(envelope))


class TestRunnerReadsTheExplicitStatus(unittest.TestCase):
    """run_query must classify an empty result the same way either way."""

    def _query(self) -> Query:
        return Query(
            query_id="q1",
            query="?",
            relevance_class="negative",
            relevant_doc_ids=(),
        )

    def _run_with_stdout(self, stdout: str):
        import subprocess
        from pathlib import Path

        fake = subprocess.CompletedProcess(
            args=[], returncode=0, stdout=stdout, stderr=""
        )
        with mock.patch("subprocess.run", return_value=fake):
            return run_query(self._query(), Path("/tmp/nope"), "/bin/true", top_k=10)

    def test_json_envelope_with_no_candidates_is_an_empty_hybrid_result(self) -> None:
        stdout = json.dumps(
            {
                "query": "?",
                "search_mode": "hybrid",
                "count": 0,
                "retrieval_status": NO_CANDIDATES_STATUS,
                "memories": [],
            }
        )
        result = self._run_with_stdout(stdout)
        self.assertEqual("hybrid_empty", result.search_mode)
        self.assertEqual((), result.rows)

    def test_legacy_plain_text_empty_is_the_same_label(self) -> None:
        result = self._run_with_stdout("No memories found for: ?\n")
        self.assertEqual("hybrid_empty", result.search_mode)
        self.assertEqual((), result.rows)

    def test_semantic_candidate_marker_is_the_same_label(self) -> None:
        result = self._run_with_stdout("No candidates returned for: ?\n")
        self.assertEqual("hybrid_empty", result.search_mode)
        self.assertEqual((), result.rows)

    def test_populated_envelope_keeps_its_rows(self) -> None:
        stdout = json.dumps(
            {
                "query": "?",
                "search_mode": "hybrid",
                "count": 1,
                "retrieval_status": "candidates_unverified",
                "memories": [
                    {
                        "id": "abc",
                        "content": "c",
                        "source": "vector",
                        "score": 0.5,
                        "vector_similarity": 1.0,
                    }
                ],
            }
        )
        result = self._run_with_stdout(stdout)
        self.assertEqual("hybrid", result.search_mode)
        self.assertEqual(1, len(result.rows))
        self.assertEqual("abc", result.rows[0]["id"])


if __name__ == "__main__":
    unittest.main()
