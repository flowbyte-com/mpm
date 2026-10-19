"""FTS5 / hybrid control set regression test (Phase A.3 V2).

The control set in `bench/fts5_control.py` is a small set of
exact-token probes that pin FTS5 / hybrid correctness. This test
exercises the control set end-to-end against the canonical
binary (the same binary the calibration runs use) and verifies:

  1. FTS5 is compiled in the binary (memories_fts shadow table
     exists in the disposable DB).
  2. The disposable corpus's documents are present in the FTS5
     index (direct FTS5 MATCH query returns rows).
  3. Exact-token probes surface at least one row with source in
     {"fts5", "hybrid"} via the production `mpm recall` CLI.
  4. The target_doc_ids in the control set are actually present
     in the FTS5 result (i.e. the lexical channel is functional
     AND the right documents are in the index).

The test is hermetic at the FTS5 level (it queries the SQLite
shadow table directly) AND it integrates with the production
hybrid ranker (it also runs `mpm recall` and asserts on the
source field). Both layers must pass.
"""

from __future__ import annotations

import json
import os
import sqlite3
import subprocess
import tempfile
import unittest
from pathlib import Path

from bench.corpus import Corpus, load_corpus
from bench.fake_server import make_server
from bench.fts5_control import (
    CONTROL_SET_VERSION,
    FTS5_CONTROL_QUERIES,
    fts5_control_corpus_manifest,
)
from bench.harness import (
    _find_db_path,
    _ingest_corpus,
    _resolve_mpm_binary,
    corpus_collection,
)
from bench.runner import run_query
from bench.workspace import scrubbed_env, write_mpm_config


def _binary_available() -> str | None:
    """Return the canonical mpm binary path if it exists."""
    candidates = [
        Path(__file__).resolve().parents[2] / ".build" / "bin" / "mpm",
    ]
    for c in candidates:
        if c.exists() and os.access(c, os.X_OK):
            return str(c)
    return None


def _init_workspace(snap: Path) -> None:
    """Initialise a fresh disposable workspace. Mirrors what
    bench.harness.cmd_smoke does: trigger schema creation
    via `mpm remember bootstrap` (which also seeds the 5
    prime-directive rows in `directives` — see
    test_phase_a3_isolation.py for that contract)."""
    mpm_bin = _resolve_mpm_binary()
    proc = subprocess.run(
        [str(mpm_bin), "remember", "bootstrap"],
        cwd=str(snap),
        env=scrubbed_env(snap),
        capture_output=True,
        text=True,
        timeout=30,
    )
    if _find_db_path(snap) is None:
        raise RuntimeError(
            f"mpm remember bootstrap did not create a DB: "
            f"rc={proc.returncode} stderr={proc.stderr[:300]}"
        )


def _ingest_v2_corpus(snap: Path) -> Corpus:
    """Load the v2 corpus and ingest it into the disposable workspace."""
    corpus = load_corpus(Path("bench/corpus"), version="v2")
    _ingest_corpus(snap, corpus)
    return corpus


class TestFTS5ControlSet(unittest.TestCase):
    """V2: FTS5 / hybrid control set against the real binary."""

    @classmethod
    def setUpClass(cls) -> None:
        cls.mpm = _binary_available()
        if cls.mpm is None:
            raise unittest.SkipTest(
                "canonical mpm binary not built; skipping live V2 test"
            )

    def test_fts5_shadow_table_present(self) -> None:
        """FTS5 is compiled in the binary; the memories_fts shadow
        table exists in the disposable DB."""
        with tempfile.TemporaryDirectory() as td:
            snap = Path(td) / "snap"
            snap.mkdir()
            _init_workspace(snap)
            db_path = _find_db_path(snap)
            self.assertIsNotNone(db_path)
            con = sqlite3.connect(str(db_path))
            try:
                rows = con.execute(
                    "SELECT name FROM sqlite_master "
                    "WHERE type='table' AND name='memories_fts'"
                ).fetchall()
            finally:
                con.close()
            self.assertEqual(
                len(rows), 1,
                f"memories_fts shadow table not found; FTS5 is not "
                f"compiled in the binary. rows={rows!r}"
            )

    def test_disposable_corpus_in_fts5_index(self) -> None:
        """After ingesting the v2 corpus, the FTS5 index contains
        the corpus's documents. A direct MATCH query for a
        distinctive token returns at least one row.

        The FTS5 schema (verified at
        internal/core/fts_recovery_test.go:435) is:

          CREATE VIRTUAL TABLE memories_fts USING fts5(
              content, collection, session_id UNINDEXED, tags,
              tokenize='porter unicode61'
          )

        The `id` is implicit (rowid); the FTS5 MATCH returns
        rowids. We then JOIN against `memories` to confirm the
        matched row is one of the bench's corpus docs (and not,
        e.g. a MPM-seed row in `directives`).
        """
        with tempfile.TemporaryDirectory() as td:
            snap = Path(td) / "snap"
            snap.mkdir()
            _init_workspace(snap)
            _ingest_v2_corpus(snap)
            db_path = _find_db_path(snap)
            self.assertIsNotNone(db_path)
            con = sqlite3.connect(str(db_path))
            try:
                # FTS5 MATCH on a token that appears in many v2
                # documents. Use the FTS5 query syntax: a bare
                # token is a single-term match.
                rows = con.execute(
                    "SELECT m.id FROM memories_fts f "
                    "JOIN memories m ON m.rowid = f.rowid "
                    "WHERE memories_fts MATCH ? "
                    "AND m.deleted_at IS NULL",
                    ("FTS5",),
                ).fetchall()
                fts_count = con.execute(
                    "SELECT COUNT(*) FROM memories_fts "
                    "WHERE memories_fts MATCH ?",
                    ("FTS5",),
                ).fetchone()[0]
            finally:
                con.close()
            self.assertGreater(
                fts_count, 0,
                "FTS5 MATCH on 'FTS5' returned 0 rows; the v2 "
                "corpus is not present in the FTS5 index"
            )
            # Sanity: at least one of the FTS5-matched rows must
            # be in the bench's collection (not a MPM-seed row
            # in `directives`). This is the V1 isolation
            # contract from `test_phase_a3_isolation.py` applied
            # at the FTS5 index level.
            bench_collection = corpus_collection()
            con = sqlite3.connect(str(db_path))
            try:
                coll_count = con.execute(
                    "SELECT COUNT(*) FROM memories_fts f "
                    "JOIN memories m ON m.rowid = f.rowid "
                    "WHERE memories_fts MATCH ? "
                    "AND m.deleted_at IS NULL "
                    "AND m.collection = ?",
                    ("FTS5", bench_collection),
                ).fetchone()[0]
            finally:
                con.close()
            self.assertGreater(
                coll_count, 0,
                f"FTS5 returned {fts_count} rows for 'FTS5' but "
                f"0 in the bench's collection {bench_collection!r}; "
                f"the bench's corpus is not in the FTS5 index"
            )

    def test_control_manifest_sha256_stable(self) -> None:
        """The control set's content-hash is stable. If the
        control set is accidentally modified, this test fails
        (forcing an explicit update of CONTROL_SET_SHA256)."""
        manifest = fts5_control_corpus_manifest()
        # Re-compute the hash from the in-memory list and
        # confirm it matches the manifest's reported hash.
        from bench.fts5_control import FTS5_CONTROL_QUERIES
        payload = json.dumps(
            [
                {
                    "query_id": q.query_id,
                    "query": q.query,
                    "target_doc_ids": list(q.target_doc_ids),
                }
                for q in FTS5_CONTROL_QUERIES
            ],
            sort_keys=True,
        ).encode("utf-8")
        import hashlib
        actual = hashlib.sha256(payload).hexdigest()
        self.assertEqual(
            manifest["expected_sha256"], actual,
            "control set manifest sha256 mismatch; the test "
            "would have caught a silent edit"
        )
        self.assertEqual(
            manifest["version"], CONTROL_SET_VERSION,
            "control set version string mismatch"
        )
        self.assertEqual(
            manifest["n_queries"], len(FTS5_CONTROL_QUERIES)
        )

    def test_exact_token_probes_surface_fts5_or_hybrid_source(self) -> None:
        """For each exact-token probe, the production `mpm recall`
        returns at least one row with source in {fts5, hybrid}.

        This is the load-bearing assertion: the FTS5 channel is
        working in the binary the calibration uses.
        """
        # This test is more expensive: it needs the real
        # `mpm recall` to return results. We boot the fake
        # embeddings server so the vector channel can also
        # participate (and produce hybrid results).
        server, base_url, port = make_server(dim=64, model_name="fake-embed-v1")
        try:
            with tempfile.TemporaryDirectory() as td:
                snap = Path(td) / "snap"
                snap.mkdir()
                # Configure the workspace with the fake server
                # provider so vector embedding succeeds.
                write_mpm_config(
                    snap,
                    profile={
                        "default": {
                            "provider": "openai-compatible",
                            "model": "fake-embed-v1",
                            "base_url": base_url,
                            "api_key": "",
                        }
                    },
                    components={"embedding": "default"},
                )
                _init_workspace(snap)
                _ingest_v2_corpus(snap)
                # Override the corpus collection env so the
                # recall subprocess queries the right collection.
                env = os.environ.copy()
                env["MPM_BENCH_COLLECTION"] = corpus_collection()
                env.update(scrubbed_env(snap))
                # Run each probe and check the source distribution.
                for fc in FTS5_CONTROL_QUERIES:
                    proc = subprocess.run(
                        [
                            self.mpm,
                            "recall",
                            "--semantic",
                            "--json",
                            "--collection",
                            corpus_collection(),
                            "--limit", "10",
                            fc.query,
                        ],
                        cwd=str(snap),
                        env=env,
                        capture_output=True,
                        text=True,
                        timeout=60,
                    )
                    if proc.returncode != 0:
                        self.fail(
                            f"mpm recall failed for probe "
                            f"{fc.query_id!r} ({fc.query!r}): "
                            f"rc={proc.returncode} "
                            f"stderr={proc.stderr[:300]}"
                        )
                    try:
                        envelope = json.loads(proc.stdout)
                    except json.JSONDecodeError:
                        # Plain-text empty result: this probe
                        # produced no FTS5 candidates, which is
                        # a failure for an exact-token probe.
                        self.fail(
                            f"probe {fc.query_id!r} returned "
                            f"plain-text empty: {proc.stdout!r}"
                        )
                    rows = envelope.get("memories", [])
                    sources = {r.get("source", "") for r in rows}
                    has_fts5 = bool(sources & {"fts5", "hybrid"})
                    if not has_fts5:
                        self.fail(
                            f"probe {fc.query_id!r} ({fc.query!r}) "
                            f"returned 0 rows with source in "
                            f"{{fts5, hybrid}}; sources={sources!r}. "
                            f"This means the FTS5 channel did not "
                            f"participate in the ranker, even "
                            f"though the exact token should match."
                        )
                    # At least one target_doc_id should appear in
                    # the top-10. This is the "right documents in
                    # the index" assertion.
                    top_ids = {r.get("id", "") for r in rows}
                    targets_found = [
                        t for t in fc.target_doc_ids
                        if t in top_ids
                    ]
                    self.assertGreater(
                        len(targets_found), 0,
                        f"probe {fc.query_id!r} ({fc.query!r}) "
                        f"surfaces no target_doc_id; targets="
                        f"{fc.target_doc_ids!r}, top_ids={top_ids!r}"
                    )
        finally:
            server.shutdown()


if __name__ == "__main__":
    unittest.main()
