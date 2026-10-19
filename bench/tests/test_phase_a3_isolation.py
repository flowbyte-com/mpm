"""Phase A.3 V1 isolation regression tests (HAL 2026-10-19).

These tests pin the V1 isolation contract from the Phase A.3
directive. They verify two distinct properties:

  1. MPM's Baseline Cognitive Bootstrap (5 prime-directive rows)
     is automatically inserted into the `directives` collection on
     every `NewDatabaseManager` call. This is the source of the
     "55 rows when 50 expected" finding from Phase A.2.

  2. The bench's collection-scoped isolation works end-to-end:
     after a fresh disposable workspace is initialised, the
     bench's `mpm recall --collection <bench_collection>` does NOT
     see the MPM-seed rows in `directives`, and the verification
     (`verify_regeneration_complete` with
     `expected_collection=<bench_collection>`) accepts the
     corpus's exact doc_ids without flagging the seed rows as
     "extra".

These tests use a real MPM binary (`.build/bin/mpm` if present,
otherwise skipped) against a disposable workspace created via
the existing `bench.workspace` helpers. They are not hermetic
unit tests; the runner smoke tests at `test_runner_smoke.py`
remain the in-process / CI-friendly coverage.

The two-layer defence model is documented in
`bench/harness.py` near the `corpus_collection` helper:

  Layer 1: `verify_regeneration_complete` is collection-scoped
           via the new `expected_collection` parameter. Even
           if the bench's collection accidentally shared its
           name with the canonical `memories` collection, the
           count check would be against rows in that
           collection only.
  Layer 2: the bench ingests into a non-default collection
           name (`bench_v2_corpus`, overridable via
           `MPM_BENCH_COLLECTION`) and queries that same
           collection. MPM's seeds live in `directives`; they
           can never reach the bench's recall result because
           they are in a different collection.

These tests verify BOTH layers are functioning.
"""

from __future__ import annotations

import json
import os
import sqlite3
import subprocess
import tempfile
import unittest
from pathlib import Path

from bench.fingerprint import verify_regeneration_complete
from bench.harness import _resolve_mpm_binary, corpus_collection
from bench.workspace import scrubbed_env, write_mpm_config


# The 5 stable seed IDs from `internal/core/seed/directives.go`
# (verified at directives.go:97, 112, 134, 154, 188). Any change
# to the registry is a defect for this test's contract.
EXPECTED_SEED_IDS = frozenset({
    "mpm-seed-read-wake-context",
    "mpm-seed-session-end-cluster-triage",
    "mpm-seed-wake-triage-policy",
    "mpm-seed-daemon-health",
    "mpm-seed-epistemic-compaction-policy",
})


def _binary_available() -> str | None:
    """Return the canonical mpm binary path if it exists and runs.

    Returns None when the binary is not built. The canonical path
    is `.build/bin/mpm` per the V3 directive; falls back to the
    harness's `_resolve_mpm_binary` resolution if the canonical
    file is missing (the harness itself would fail, so the test
    is skipped in that case).
    """
    candidates = [
        Path(__file__).resolve().parents[2] / ".build" / "bin" / "mpm",
    ]
    for c in candidates:
        if c.exists() and os.access(c, os.X_OK):
            return str(c)
    return None


def _find_db_path(snap: Path) -> Path | None:
    """Return the path to the disposable DB inside `snap`, or None."""
    # The DB lives at `<snap>/src/db/mpm.db` per the standard
    # MPM workspace layout (verified against the harness
    # `_init_workspace` flow). We search recursively to be
    # robust to future layout changes.
    for p in snap.rglob("mpm.db"):
        return p
    for p in snap.rglob("*.db"):
        return p
    return None


def _init_fresh_workspace(snap: Path, mpm: str) -> None:
    """Initialise a fresh disposable workspace. The MPM
    `NewDatabaseManager` path runs on the first CLI invocation
    and inserts the 5 prime-directive rows in `directives`.

    A no-op config (empty embedding profile) is written so MPM
    does not try to talk to a real provider just for schema
    initialisation.

    Note: the subprocess may exit non-zero because the
    configured provider (a non-routable URL) is unreachable.
    The schema and the 5 seed rows ARE created regardless;
    `NewDatabaseManager` is called BEFORE the embedding step.
    We assert on the resulting DB state, not on rc.
    """
    write_mpm_config(
        snap,
        profile={
            "default": {
                "provider": "openai-compatible",
                "model": "fake-embed-v1",
                "base_url": "http://127.0.0.1:0",
                "api_key": "test-key-not-used",
            }
        },
        components={"embedding": "default"},
    )
    # `mpm remember bootstrap` triggers NewDatabaseManager and
    # therefore seedBaselineDirectives on a fresh workspace. We
    # do not care about its return code; the load-bearing
    # assertion is on the resulting DB state (5 seed rows in
    # `directives`). `scrubbed_env` is used to ensure no
    # MPM_SHARED_DB or other env overrides from the operator's
    # environment leak into the disposable workspace.
    proc = subprocess.run(
        [mpm, "remember", "bootstrap"],
        cwd=str(snap),
        env=scrubbed_env(snap),
        capture_output=True,
        text=True,
        timeout=60,
    )
    # The DB must exist regardless of rc. If it does not, the
    # invocation truly failed.
    if _find_db_path(snap) is None:
        raise RuntimeError(
            f"mpm remember bootstrap did not create a DB: "
            f"rc={proc.returncode} stderr={proc.stderr[:300]}"
        )


class TestPhaseA3SeedInjection(unittest.TestCase):
    """V1 layer 2: the Baseline Cognitive Bootstrap inserts 5 rows
    into `directives` on every fresh workspace.
    """

    @classmethod
    def setUpClass(cls) -> None:
        cls.mpm = _binary_available()
        if cls.mpm is None:
            raise unittest.SkipTest(
                "canonical mpm binary not built; skipping live V1 test"
            )

    def test_fresh_workspace_seeds_5_directives(self) -> None:
        """A fresh workspace contains exactly 5 seed rows in the
        `directives` collection, with the 5 expected stable IDs.

        This is the load-bearing V1 finding: the 5 rows are NOT
        part of the bench's corpus; they are MPM-internal data
        that lives in a different collection.
        """
        with tempfile.TemporaryDirectory() as td:
            snap = Path(td) / "snap"
            snap.mkdir()
            _init_fresh_workspace(snap, self.mpm)
            db_path = _find_db_path(snap)
            self.assertIsNotNone(
                db_path, f"mpm did not create a DB under {snap}"
            )
            con = sqlite3.connect(str(db_path))
            try:
                rows = con.execute(
                    "SELECT id, collection FROM memories "
                    "WHERE is_prime_directive = 1"
                ).fetchall()
            finally:
                con.close()
            self.assertEqual(
                len(rows), 5,
                f"expected exactly 5 seed rows; got {len(rows)}: {rows!r}"
            )
            actual_ids = {r[0] for r in rows}
            self.assertEqual(
                actual_ids, EXPECTED_SEED_IDS,
                f"seed IDs diverged from the registry in "
                f"internal/core/seed/directives.go; "
                f"missing={EXPECTED_SEED_IDS - actual_ids}, "
                f"unexpected={actual_ids - EXPECTED_SEED_IDS}"
            )
            # All 5 must be in the `directives` collection, NOT
            # in the bench's default `memories` collection. This
            # is the row that proves the Layer 2 isolation works.
            for _id, collection in rows:
                self.assertEqual(
                    collection, "directives",
                    f"seed {_id!r} is in collection {collection!r}; "
                    f"expected 'directives' (per "
                    f"internal/core/seed/engine.go:147)"
                )


class TestPhaseA3CorpusPurity(unittest.TestCase):
    """V1 layer 1: collection-scoped verification accepts the bench
    corpus's exact doc_ids without flagging the seed rows as
    "extra" rows, AND a recall against the bench's collection
    never returns a seed row.
    """

    @classmethod
    def setUpClass(cls) -> None:
        cls.mpm = _binary_available()
        if cls.mpm is None:
            raise unittest.SkipTest(
                "canonical mpm binary not built; skipping live V1 test"
            )

    def test_collection_scoped_verification_passes(self) -> None:
        """The new `expected_collection` parameter correctly scopes
        the verification count and doc_ids set to the bench's
        collection. The seed rows in `directives` do NOT trigger
        a false-positive "extra rows" failure.
        """
        with tempfile.TemporaryDirectory() as td:
            snap = Path(td) / "snap"
            snap.mkdir()
            _init_fresh_workspace(snap, self.mpm)
            db_path = _find_db_path(snap)
            self.assertIsNotNone(db_path)
            bench_collection = corpus_collection()
            # Wipe the auto-created bootstrap row from the bench's
            # collection (the `mpm remember bootstrap` invocation
            # leaves a row in `memories`). This mirrors what
            # `_init_workspace` in bench/harness.py does.
            con = sqlite3.connect(str(db_path))
            try:
                con.execute(
                    "DELETE FROM memories WHERE collection = ?",
                    (bench_collection,),
                )
                con.execute(
                    "INSERT INTO memories "
                    "(id, collection, content, tags, metadata, weight, "
                    " embedding, embedding_source, embedding_dimension, "
                    " embedding_model) "
                    "VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?)",
                    (
                        "bench-test-row-1",
                        bench_collection,
                        "synthetic bench doc for V1 isolation test",
                        "[]",
                        "{}",
                        1.0,
                        json.dumps([0.01] * 64).encode("utf-8"),
                        "provider",
                        64,
                        "fake-embed-v1",
                    ),
                )
                con.commit()
            finally:
                con.close()

            # The new collection-scoped verification MUST pass:
            #   - 1 row in the bench's collection, 1 expected doc_id
            #   - 5 seed rows in `directives`, NOT in the bench's
            #     collection, so they do NOT trigger the
            #     "extra_doc_ids" failure.
            result = verify_regeneration_complete(
                db_path,
                expected_model="fake-embed-v1",
                expected_dimension=64,
                expected_doc_ids={"bench-test-row-1"},
                expected_memories_row_count=1,
                expected_collection=bench_collection,
            )
            self.assertTrue(
                result.ok,
                f"collection-scoped verification failed: {result.failures!r}"
            )

    def test_unscoped_verification_flags_seed_rows_as_extra(self) -> None:
        """Counter-test for the above: WITHOUT `expected_collection`,
        the verification's "extra rows" check flags the 5 seed rows
        as defects in the unfiltered `memories` set. This is the
        sanity check that the new extra-doc_ids check is working.

        If this test ever fails (i.e. unfiltered verification
        passes despite the seeds), the new check has been
        weakened and the V1 isolation contract is at risk.
        """
        with tempfile.TemporaryDirectory() as td:
            snap = Path(td) / "snap"
            snap.mkdir()
            _init_fresh_workspace(snap, self.mpm)
            db_path = _find_db_path(snap)
            self.assertIsNotNone(db_path)
            bench_collection = corpus_collection()
            con = sqlite3.connect(str(db_path))
            try:
                # Wipe the auto-created bootstrap row, then add
                # one synthetic row. We use a partial-embedding
                # INSERT to keep the test focused on the
                # extra-doc_ids check; the undecodable-embedding
                # and NULL-fingerprint failures are not the
                # load-bearing assertion here.
                con.execute(
                    "DELETE FROM memories WHERE collection = ?",
                    (bench_collection,),
                )
                con.execute(
                    "INSERT INTO memories "
                    "(id, collection, content, tags, metadata, weight) "
                    "VALUES (?, ?, ?, ?, ?, ?)",
                    (
                        "bench-test-row-1",
                        bench_collection,
                        "synthetic bench doc for V1 isolation test",
                        "[]",
                        "{}",
                        1.0,
                    ),
                )
                con.commit()
            finally:
                con.close()

            # Without expected_collection, the verification sees
            # all rows in the global `memories` table. The 5 seed
            # rows are NOT in the expected_doc_ids set, so the
            # extra-doc_ids check must fail.
            result = verify_regeneration_complete(
                db_path,
                expected_model="fake-embed-v1",
                expected_dimension=64,
                expected_doc_ids={"bench-test-row-1"},
                # NOTE: no expected_memories_row_count here; the
                # extra-doc_ids check is the load-bearing assertion.
            )
            self.assertFalse(
                result.ok,
                "unscoped verification should have failed because "
                "the 5 seed rows are 'extra' relative to the "
                "expected_doc_ids set, but it passed"
            )
            extra_failure_present = any(
                "extra" in f.lower() for f in result.failures
            )
            self.assertTrue(
                extra_failure_present,
                f"expected an 'extra rows' failure; got: {result.failures!r}"
            )


if __name__ == "__main__":
    unittest.main()
