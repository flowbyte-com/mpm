"""Fingerprint tests (C1, gate 1 of impl gates).

Builds disposable DBs with the right schema and asserts that
verify_regeneration_complete correctly identifies:
  - clean backfill (all rows have correct fingerprint AND decoded length)
  - mismatched fingerprint on populated rows
  - NULL fingerprint columns
  - missing rows entirely (gate 1: missing/NULL detection)
  - undecodable BLOBs
  - decoded length != declared dimension
"""

import json
import sqlite3
import tempfile
import unittest
from pathlib import Path

from bench.fingerprint import fingerprint_of_snapshot, verify_regeneration_complete


SCHEMA = """
CREATE TABLE IF NOT EXISTS memories (
    id TEXT PRIMARY KEY,
    collection TEXT NOT NULL,
    content TEXT NOT NULL,
    embedding BLOB,
    embedding_source TEXT NOT NULL DEFAULT 'provider',
    embedding_dimension INTEGER,
    embedding_model TEXT,
    deleted_at INTEGER,
    weight REAL
);
CREATE TABLE IF NOT EXISTS reference_chunks (
    id TEXT PRIMARY KEY,
    doc_id TEXT NOT NULL,
    chunk_index INTEGER NOT NULL,
    section TEXT,
    content TEXT NOT NULL,
    embedding BLOB,
    embedding_source TEXT NOT NULL DEFAULT 'provider',
    embedding_dimension INTEGER,
    embedding_model TEXT
);
"""

EXPECTED_MODEL = "fake-embed-v1"
EXPECTED_DIM = 8


def _vec_blob(values: list[float]) -> bytes:
    return json.dumps(values).encode("utf-8")


def _make_db(path: Path, rows: list[dict]) -> None:
    con = sqlite3.connect(path)
    try:
        con.executescript(SCHEMA)
        for r in rows:
            con.execute(
                "INSERT INTO memories (id, collection, content, embedding, "
                "embedding_source, embedding_dimension, embedding_model) "
                "VALUES (?, ?, ?, ?, ?, ?, ?)",
                (
                    r["id"],
                    "memories",
                    r["content"],
                    r.get("embedding"),
                    r.get("embedding_source", "provider"),
                    r.get("embedding_dimension"),
                    r.get("embedding_model"),
                ),
            )
        con.commit()
    finally:
        con.close()


class TestVerifyRegenerationComplete(unittest.TestCase):
    def test_clean_backfill_passes(self) -> None:
        with tempfile.TemporaryDirectory() as td:
            db = Path(td) / "test.db"
            rows = [
                {
                    "id": "d0000001",
                    "content": "x",
                    "embedding": _vec_blob([0.1] * EXPECTED_DIM),
                    "embedding_source": "provider",
                    "embedding_dimension": EXPECTED_DIM,
                    "embedding_model": EXPECTED_MODEL,
                },
                {
                    "id": "d0000002",
                    "content": "y",
                    "embedding": _vec_blob([0.2] * EXPECTED_DIM),
                    "embedding_source": "provider",
                    "embedding_dimension": EXPECTED_DIM,
                    "embedding_model": EXPECTED_MODEL,
                },
            ]
            _make_db(db, rows)
            result = verify_regeneration_complete(
                db,
                expected_model=EXPECTED_MODEL,
                expected_dimension=EXPECTED_DIM,
            )
            self.assertTrue(result.ok, f"expected ok, got failures: {result.failures}")

    def test_missing_doc_id_detected(self) -> None:
        # gate 1: missing rows entirely must be detected.
        with tempfile.TemporaryDirectory() as td:
            db = Path(td) / "test.db"
            rows = [
                {
                    "id": "d0000001",
                    "content": "x",
                    "embedding": _vec_blob([0.1] * EXPECTED_DIM),
                    "embedding_source": "provider",
                    "embedding_dimension": EXPECTED_DIM,
                    "embedding_model": EXPECTED_MODEL,
                },
            ]
            _make_db(db, rows)
            result = verify_regeneration_complete(
                db,
                expected_model=EXPECTED_MODEL,
                expected_dimension=EXPECTED_DIM,
                expected_doc_ids={"d0000001", "d0000002"},
            )
            self.assertFalse(result.ok)
            self.assertTrue(
                any("missing" in f for f in result.failures),
                f"expected 'missing' in failures, got {result.failures}",
            )

    def test_mismatched_model_detected(self) -> None:
        with tempfile.TemporaryDirectory() as td:
            db = Path(td) / "test.db"
            rows = [
                {
                    "id": "d0000001",
                    "content": "x",
                    "embedding": _vec_blob([0.1] * EXPECTED_DIM),
                    "embedding_source": "provider",
                    "embedding_dimension": EXPECTED_DIM,
                    "embedding_model": "different-model",
                },
            ]
            _make_db(db, rows)
            result = verify_regeneration_complete(
                db,
                expected_model=EXPECTED_MODEL,
                expected_dimension=EXPECTED_DIM,
            )
            self.assertFalse(result.ok)
            self.assertTrue(
                any("non-matching fingerprint" in f for f in result.failures),
                f"expected mismatch failure, got {result.failures}",
            )

    def test_null_embedding_model_detected(self) -> None:
        with tempfile.TemporaryDirectory() as td:
            db = Path(td) / "test.db"
            rows = [
                {
                    "id": "d0000001",
                    "content": "x",
                    "embedding": _vec_blob([0.1] * EXPECTED_DIM),
                    "embedding_source": "provider",
                    "embedding_dimension": EXPECTED_DIM,
                    "embedding_model": None,
                },
            ]
            _make_db(db, rows)
            result = verify_regeneration_complete(
                db,
                expected_model=EXPECTED_MODEL,
                expected_dimension=EXPECTED_DIM,
            )
            self.assertFalse(result.ok)
            self.assertTrue(
                any("NULL fingerprint" in f for f in result.failures),
                f"expected NULL fingerprint failure, got {result.failures}",
            )

    def test_decoded_length_mismatch_detected(self) -> None:
        with tempfile.TemporaryDirectory() as td:
            db = Path(td) / "test.db"
            rows = [
                {
                    "id": "d0000001",
                    "content": "x",
                    "embedding": _vec_blob([0.1] * 4),  # 4 floats
                    "embedding_source": "provider",
                    "embedding_dimension": EXPECTED_DIM,  # declared as 8
                    "embedding_model": EXPECTED_MODEL,
                },
            ]
            _make_db(db, rows)
            result = verify_regeneration_complete(
                db,
                expected_model=EXPECTED_MODEL,
                expected_dimension=EXPECTED_DIM,
            )
            self.assertFalse(result.ok)
            self.assertTrue(
                any("decoded length" in f for f in result.failures),
                f"expected decoded-length failure, got {result.failures}",
            )

    def test_undecodable_blob_detected(self) -> None:
        with tempfile.TemporaryDirectory() as td:
            db = Path(td) / "test.db"
            rows = [
                {
                    "id": "d0000001",
                    "content": "x",
                    "embedding": b"not-json",
                    "embedding_source": "provider",
                    "embedding_dimension": EXPECTED_DIM,
                    "embedding_model": EXPECTED_MODEL,
                },
            ]
            _make_db(db, rows)
            result = verify_regeneration_complete(
                db,
                expected_model=EXPECTED_MODEL,
                expected_dimension=EXPECTED_DIM,
            )
            self.assertFalse(result.ok)
            self.assertTrue(
                any("undecodable" in f for f in result.failures),
                f"expected undecodable failure, got {result.failures}",
            )


class TestFingerprintOfSnapshot(unittest.TestCase):
    def test_read_only_opens_without_writing(self) -> None:
        with tempfile.TemporaryDirectory() as td:
            db = Path(td) / "test.db"
            rows = [
                {
                    "id": "d0000001",
                    "content": "x",
                    "embedding": _vec_blob([0.1] * EXPECTED_DIM),
                    "embedding_source": "provider",
                    "embedding_dimension": EXPECTED_DIM,
                    "embedding_model": EXPECTED_MODEL,
                },
            ]
            _make_db(db, rows)
            before_size = db.stat().st_size
            fp = fingerprint_of_snapshot(db)
            after_size = db.stat().st_size
            self.assertEqual(before_size, after_size)
            self.assertEqual(len(fp["memories"]), 1)
            self.assertEqual(fp["memories"][0][0], EXPECTED_MODEL)
