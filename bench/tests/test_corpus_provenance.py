"""Corpus provenance tests (C3, gate 6).

Asserts that the manifest's content hashes match the on-disk files
and that the corpus loads cleanly. Also asserts the explicit
historical-loss statement is present in the manifest.
"""

import json
import tempfile
import unittest
from pathlib import Path

from bench.corpus import CorpusError, load_corpus, stable_doc_id


REPO_ROOT = Path(__file__).resolve().parent.parent.parent
CORPUS_DIR = REPO_ROOT / "bench" / "corpus"


class TestLoadCorpus(unittest.TestCase):
    def test_real_corpus_loads(self) -> None:
        corpus = load_corpus(CORPUS_DIR)
        self.assertEqual(corpus.version, "v1")
        self.assertEqual(len(corpus.documents), 30)
        self.assertEqual(len(corpus.queries), 20)
        self.assertEqual(len(corpus.positive_queries), 10)
        self.assertEqual(len(corpus.negative_queries), 10)

    def test_manifest_hashes_match_files(self) -> None:
        # load_corpus() itself verifies the hashes; if we got here,
        # the hashes match. This test is the smoke for that.
        load_corpus(CORPUS_DIR)

    def test_historical_loss_note_in_manifest(self) -> None:
        # gate 6: the manifest must contain an explicit statement
        # that the historical corpus cannot be recovered.
        manifest = json.loads((CORPUS_DIR / "MANIFEST.json").read_text())
        note = manifest.get("note", "").lower()
        self.assertIn("synthetic", note)
        self.assertIn("not a recovery", note)

    def test_tampered_corpus_refused(self) -> None:
        # Copy the corpus to a temp dir, modify a file, expect refusal.
        with tempfile.TemporaryDirectory() as td:
            import shutil
            tmp_corpus = Path(td) / "corpus"
            shutil.copytree(CORPUS_DIR, tmp_corpus)
            # Tamper with a corpus file.
            docs = tmp_corpus / "v1" / "documents.json"
            data = json.loads(docs.read_text())
            data[0]["content"] = "tampered content"
            docs.write_text(json.dumps(data))
            with self.assertRaises(CorpusError):
                load_corpus(tmp_corpus)

    def test_stable_doc_id_is_8_chars(self) -> None:
        # gate 3: deterministic, unambiguous mapping between corpus
        # IDs and MPM's abbreviated result IDs. shortID() truncates
        # to 8 chars (cmd/mpm/handlers_cascade.go:331-333).
        self.assertEqual(len(stable_doc_id("any text")), 8)
        # Same content always produces the same ID.
        self.assertEqual(stable_doc_id("x"), stable_doc_id("x"))
        # Different content produces different IDs.
        self.assertNotEqual(stable_doc_id("a"), stable_doc_id("b"))

    def test_query_relevance_classes(self) -> None:
        corpus = load_corpus(CORPUS_DIR)
        for q in corpus.positive_queries:
            self.assertTrue(q.is_positive)
            self.assertGreater(len(q.relevant_doc_ids), 0)
        for q in corpus.negative_queries:
            self.assertFalse(q.is_positive)
            self.assertEqual(len(q.relevant_doc_ids), 0)
