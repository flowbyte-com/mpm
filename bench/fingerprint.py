"""Fingerprint and regeneration-completion verification (C1, C1', gate 1).

Two responsibilities:

1. Fingerprint a snapshot: count (model, dimension, source) groups in
   both `memories` and `reference_chunks` and report them.

2. Verify regeneration completed fully: every embedded row in both
   tables must carry the exact intended (model, dimension, source)
   fingerprint AND the BLOB must decode to a vector whose length
   matches the declared `embedding_dimension`. This is the load-bearing
   assertion that rejects partial backfills even when the MPM
   subprocess exits 0 (per gate 1 of HAL's implementation gates).

Schema references (verified file:line):
  - memories.embedding_model, embedding_dimension, embedding_source:
    internal/core/schema.go:1552-1569
  - reference_chunks.embedding_model, embedding_dimension, embedding_source:
    internal/core/schema.go:786-794, 1469-1476
  - BLOB format (JSON-encoded []float32):
    internal/core/reference_db.go:10-19
"""

from __future__ import annotations

import json
import sqlite3
from dataclasses import dataclass
from pathlib import Path
from typing import Any


@dataclass(frozen=True)
class VerificationResult:
    ok: bool
    failures: tuple[str, ...]
    expected: tuple[str, int, str]
    snapshot_path: Path

    def to_dict(self) -> dict[str, Any]:
        return {
            "ok": self.ok,
            "failures": list(self.failures),
            "expected": {
                "model": self.expected[0],
                "dimension": self.expected[1],
                "source": self.expected[2],
            },
            "snapshot_path": str(self.snapshot_path),
        }


def fingerprint_of_snapshot(snapshot_path: Path) -> dict[str, list[tuple]]:
    """Return a fingerprint dict for the snapshot. Read-only. Does not
    modify the file.

    Returns:
        {
          "memories": [(model, dimension, source, count), ...],
          "reference_chunks": [(model, dimension, source, count), ...],
        }
    """
    con = sqlite3.connect(f"file:{snapshot_path}?mode=ro", uri=True)
    try:
        mem_rows = con.execute(
            """
            SELECT embedding_model, embedding_dimension, embedding_source, COUNT(*)
            FROM memories
            WHERE embedding IS NOT NULL AND deleted_at IS NULL
            GROUP BY 1, 2, 3
            """
        ).fetchall()
        ref_rows = con.execute(
            """
            SELECT embedding_model, embedding_dimension, embedding_source, COUNT(*)
            FROM reference_chunks
            WHERE embedding IS NOT NULL
            GROUP BY 1, 2, 3
            """
        ).fetchall()
    finally:
        con.close()
    return {"memories": mem_rows, "reference_chunks": ref_rows}


def _decode_vector(blob) -> list[float] | None:
    """Decode an embedding BLOB. Returns None on malformed input.

    The BLOB format is JSON-encoded []float32 (verified at
    internal/core/reference_db.go:10-19). Empty bytes decode to an
    empty list; the caller decides whether that is acceptable.

    Python's sqlite3 driver may return BLOBs as `bytes`, `str`, or
    `memoryview` depending on `detect_types` and the column's
    declared type. We accept any of those and coerce to text.
    """
    if blob is None:
        return None
    if isinstance(blob, (bytes, bytearray, memoryview)):
        try:
            text = bytes(blob).decode("utf-8")
        except UnicodeDecodeError:
            return None
    elif isinstance(blob, str):
        text = blob
    else:
        return None
    if not text:
        return []
    try:
        decoded = json.loads(text)
    except json.JSONDecodeError:
        return None
    if not isinstance(decoded, list):
        return None
    return decoded


def verify_regeneration_complete(
    snapshot_path: Path,
    expected_model: str,
    expected_dimension: int,
    expected_source: str = "provider",
    expected_doc_ids: set[str] | None = None,
    expected_reference_doc_ids: set[str] | None = None,
    expected_memories_row_count: int | None = None,
    expected_reference_chunks_row_count: int | None = None,
    expected_collection: str | None = None,
) -> VerificationResult:
    """Open the snapshot read-only and assert every embedded row in
    both `memories` and `reference_chunks` has the exact intended
    (model, dimension, source) AND the BLOB decodes to a vector whose
    length matches `expected_dimension` AND (if `expected_doc_ids` is
    provided) every expected doc_id has an embedded row AND (priority
    2 of HAL's review) the decoded vector is not all-zero and has a
    non-NaN, non-Inf, finite L2 norm in a sane range.

    Subprocess exit 0 is irrelevant; this function is the source of
    truth for "did regeneration complete?".

    Gate 1 of HAL's implementation gates: this must detect missing /
    NULL embeddings, not just incorrect fingerprints on populated
    rows. The expected_doc_ids check is how missing rows are caught:
    if a row is missing entirely, the doc_ids set will not be covered.

    v3 review (HAL 2026-10-19):
      - NULL/missing detection now runs on BOTH `memories` AND
        `reference_chunks`. The pre-fix code only counted
        `memories` rows with `embedding IS NULL`; reference_chunks
        were checked only for fingerprint match on rows that
        already had a non-NULL embedding. A partial reference
        backfill would therefore have passed verification.
      - `expected_reference_doc_ids`: when provided, every
        expected ref doc_id must have an embedded row. Missing
        IDs are listed in failures.
      - `expected_memories_row_count` and
        `expected_reference_chunks_row_count`: when provided,
        the actual row count in the table (with the expected
        fingerprint) must match.

    Hardening (priority 2 of HAL's review):
      - L2-norm check: reject zero vectors (a `provider` row whose
        decoded BLOB is all zeros is a silent failure; the JSON
        round-trip will accept it but the ranker will see a
        directionless vector).
      - NaN/Inf check: a NaN in a stored vector would corrupt
        cosine computations; the JSON decode accepts the
        string `"NaN"` on some Python versions but Go's strict
        JSON decoder refuses it. We surface the corruption
        rather than letting it pass.
      - L2-norm range check: a unit-L2 vector's norm is 1.0; a
        stored vector whose L2 norm is far from `expected_dimension`
        in magnitude (e.g. `1e6`) is a real model failure.

    Args:
        snapshot_path: path to the disposable DB file
        expected_model: e.g. "fake-embedding-v1" or "ollama:all-minilm"
        expected_dimension: declared vector length, e.g. 64
        expected_source: "provider" (the only valid value for a
            backfilled row; "hash" / "null" / NULL are all failures)
        expected_doc_ids: optional set of corpus doc_ids that must
            have a non-NULL embedded row. If provided, missing IDs
            are listed in failures.
        expected_reference_doc_ids: optional set of reference doc_ids
            whose chunks must all be embedded.
        expected_memories_row_count: optional expected count of
            embedded memories; if provided, the actual count must
            match exactly.
        expected_reference_chunks_row_count: optional expected count
            of embedded reference_chunks; if provided, the actual
            count must match exactly.
    """
    import math

    failures: list[str] = []
    con = sqlite3.connect(f"file:{snapshot_path}?mode=ro", uri=True)
    try:
        # Memories: check fingerprint columns.
        mem_mismatch = con.execute(
            """
            SELECT COUNT(*) FROM memories
            WHERE embedding IS NOT NULL
              AND deleted_at IS NULL
              AND NOT (embedding_model = ?
                       AND embedding_dimension = ?
                       AND embedding_source = ?)
            """,
            (expected_model, expected_dimension, expected_source),
        ).fetchone()[0]
        mem_null = con.execute(
            """
            SELECT COUNT(*) FROM memories
            WHERE embedding IS NOT NULL
              AND deleted_at IS NULL
              AND (embedding_model IS NULL
                   OR embedding_dimension IS NULL
                   OR embedding_source IS NULL)
            """
        ).fetchone()[0]
        # Memories: check that the BLOB decodes to a vector of the
        # declared length AND is a sane real-valued vector.
        mem_bad_len = 0
        mem_decoded_total = 0
        mem_undecodable = 0
        mem_zero_norm = 0
        mem_nonfinite = 0
        mem_huge_norm = 0
        for blob, dim in con.execute(
            """
            SELECT embedding, embedding_dimension FROM memories
            WHERE embedding IS NOT NULL AND deleted_at IS NULL
            """
        ):
            vec = _decode_vector(blob)
            if vec is None:
                mem_undecodable += 1
                continue
            mem_decoded_total += 1
            if dim is None or len(vec) != dim:
                mem_bad_len += 1
                continue
            # Sanity: all values finite, not all zero, norm bounded.
            if any(
                (not isinstance(x, (int, float))) or x != x or x in (float("inf"), float("-inf"))
                for x in vec
            ):
                mem_nonfinite += 1
                continue
            norm_sq = sum(float(x) * float(x) for x in vec)
            if norm_sq == 0.0:
                mem_zero_norm += 1
                continue
            if math.sqrt(norm_sq) > 1e6 * expected_dimension:
                mem_huge_norm += 1
                continue

        # Reference chunks: same shape.
        ref_mismatch = con.execute(
            """
            SELECT COUNT(*) FROM reference_chunks
            WHERE embedding IS NOT NULL
              AND NOT (embedding_model = ?
                       AND embedding_dimension = ?
                       AND embedding_source = ?)
            """,
            (expected_model, expected_dimension, expected_source),
        ).fetchone()[0]
        ref_null = con.execute(
            """
            SELECT COUNT(*) FROM reference_chunks
            WHERE embedding IS NOT NULL
              AND (embedding_model IS NULL
                   OR embedding_dimension IS NULL
                   OR embedding_source IS NULL)
            """
        ).fetchone()[0]
        ref_bad_len = 0
        ref_decoded_total = 0
        ref_undecodable = 0
        ref_zero_norm = 0
        ref_nonfinite = 0
        ref_huge_norm = 0
        for blob, dim in con.execute(
            """
            SELECT embedding, embedding_dimension FROM reference_chunks
            WHERE embedding IS NOT NULL
            """
        ):
            vec = _decode_vector(blob)
            if vec is None:
                ref_undecodable += 1
                continue
            ref_decoded_total += 1
            if dim is None or len(vec) != dim:
                ref_bad_len += 1
                continue
            if any(
                (not isinstance(x, (int, float))) or x != x or x in (float("inf"), float("-inf"))
                for x in vec
            ):
                ref_nonfinite += 1
                continue
            norm_sq = sum(float(x) * float(x) for x in vec)
            if norm_sq == 0.0:
                ref_zero_norm += 1
                continue
            if math.sqrt(norm_sq) > 1e6 * expected_dimension:
                ref_huge_norm += 1
                continue

        # v3 review: NULL/missing detection in BOTH tables.
        # The pre-fix code only checked the memories table; a
        # partial reference_chunks backfill would have passed.
        # Phase A.3 (HAL 2026-10-19): when `expected_collection`
        # is provided, the NULL/missing detection on `memories`
        # is also scoped to that collection. MPM's Baseline
        # Cognitive Bootstrap inserts 5 prime-directive rows
        # into the `directives` collection with no embedding;
        # without the collection filter, those rows would
        # always trigger a spurious missing-embedding failure
        # on a fresh workspace.
        if expected_collection is not None:
            mem_unembedded = con.execute(
                """
                SELECT COUNT(*) FROM memories
                WHERE deleted_at IS NULL
                  AND embedding IS NULL
                  AND collection = ?
                """,
                (expected_collection,),
            ).fetchone()[0]
        else:
            mem_unembedded = con.execute(
                """
                SELECT COUNT(*) FROM memories
                WHERE deleted_at IS NULL AND embedding IS NULL
                """
            ).fetchone()[0]
        ref_unembedded = con.execute(
            """
            SELECT COUNT(*) FROM reference_chunks
            WHERE embedding IS NULL
            """
        ).fetchone()[0]
        if mem_unembedded:
            failures.append(
                f"memories: {mem_unembedded} rows have NULL embedding "
                f"(gate 1: missing/NULL detection)"
            )
        if ref_unembedded:
            failures.append(
                f"reference_chunks: {ref_unembedded} rows have NULL "
                f"embedding (gate 1: missing/NULL detection)"
            )

        # Expected doc_ids coverage. If the caller passed the corpus
        # IDs, every one must have an embedded row.
        missing_doc_ids: list[str] = []
        extra_doc_ids: list[str] = []
        if expected_doc_ids is not None:
            if expected_collection is not None:
                rows = con.execute(
                    "SELECT id FROM memories "
                    "WHERE deleted_at IS NULL AND collection = ?",
                    (expected_collection,),
                ).fetchall()
            else:
                rows = con.execute(
                    "SELECT id FROM memories WHERE deleted_at IS NULL"
                ).fetchall()
            present = {r[0] for r in rows}
            missing_doc_ids = sorted(
                d for d in expected_doc_ids if d not in present
            )
            # Phase A.3: detect extra rows that are NOT in the
            # expected set. Without this check, a partial backfill
            # that left orphan rows (or MPM-seed rows in the
            # collection) would still pass the count check if the
            # count happened to match by coincidence. The
            # expected-doc-ids set is the source of truth for
            # "which rows are supposed to be in the collection";
            # anything else is a defect.
            extra_doc_ids = sorted(
                p for p in present if p not in expected_doc_ids
            )
        missing_reference_doc_ids: list[str] = []
        if expected_reference_doc_ids is not None:
            # A reference doc is considered "covered" when at least
            # one of its chunks has a non-NULL embedding. The
            # caller passes doc_ids (not chunk_ids); we map
            # chunk -> doc via the doc_id column on
            # reference_chunks.
            rows = con.execute(
                "SELECT DISTINCT doc_id FROM reference_chunks "
                "WHERE embedding IS NOT NULL"
            ).fetchall()
            covered = {r[0] for r in rows}
            missing_reference_doc_ids = sorted(
                d for d in expected_reference_doc_ids if d not in covered
            )

        # Expected row counts (v3 review): catch silent partial
        # backfills even when every expected doc is "covered"
        # by a chunk.
        #
        # Phase A.3 (HAL 2026-10-19): when `expected_collection` is
        # provided, the count is restricted to that collection. This
        # is the V1 isolation mechanism: MPM's Baseline Cognitive
        # Bootstrap seeds 5 prime-directive rows into the
        # `directives` collection at every `NewDatabaseManager` call,
        # and those rows legitimately exist alongside the bench's
        # evaluation surface. Without the collection filter, the
        # count would always be corpus_size + 5 (or corpus_size +
        # N_seed) on a fresh workspace, producing a spurious
        # partial-backfill failure.
        #
        # The collection filter does NOT weaken the assertion: it
        # scopes it correctly. The expected_doc_ids check still
        # catches missing rows; the new "no extra rows beyond the
        # expected set" check (also Phase A.3) catches rows that
        # exist in the collection but are not in the corpus. Both
        # checks are stronger, not weaker, than the unfiltered
        # count.
        collection_clause = ""
        collection_args: tuple[Any, ...] = ()
        if expected_collection is not None:
            collection_clause = "AND collection = ?"
            collection_args = (expected_collection,)
        if expected_memories_row_count is not None:
            actual_mem = con.execute(
                f"""
                SELECT COUNT(*) FROM memories
                WHERE embedding IS NOT NULL
                  AND deleted_at IS NULL
                  AND embedding_model = ?
                  AND embedding_dimension = ?
                  AND embedding_source = ?
                  {collection_clause}
                """,
                (expected_model, expected_dimension, expected_source)
                + collection_args,
            ).fetchone()[0]
            if actual_mem != expected_memories_row_count:
                failures.append(
                    f"memories: embedded row count {actual_mem} != "
                    f"expected {expected_memories_row_count} "
                    f"(gate 1: partial backfill detection)"
                )
        if expected_reference_chunks_row_count is not None:
            actual_ref = con.execute(
                """
                SELECT COUNT(*) FROM reference_chunks
                WHERE embedding IS NOT NULL
                  AND embedding_model = ?
                  AND embedding_dimension = ?
                  AND embedding_source = ?
                """,
                (expected_model, expected_dimension, expected_source),
            ).fetchone()[0]
            if actual_ref != expected_reference_chunks_row_count:
                failures.append(
                    f"reference_chunks: embedded row count {actual_ref} "
                    f"!= expected {expected_reference_chunks_row_count} "
                    f"(gate 1: partial backfill detection)"
                )
    finally:
        con.close()

    if mem_mismatch:
        failures.append(
            f"memories: {mem_mismatch} rows with non-matching fingerprint"
        )
    if mem_null:
        failures.append(
            f"memories: {mem_null} rows with NULL fingerprint columns"
        )
    if mem_bad_len:
        failures.append(
            f"memories: {mem_bad_len} rows with decoded length != declared embedding_dimension"
        )
    if mem_undecodable:
        failures.append(
            f"memories: {mem_undecodable} rows with undecodable embedding BLOB"
        )
    if mem_zero_norm:
        failures.append(
            f"memories: {mem_zero_norm} rows with all-zero vector (silent provider failure)"
        )
    if mem_nonfinite:
        failures.append(
            f"memories: {mem_nonfinite} rows with NaN/Inf in vector (priority 2)"
        )
    if mem_huge_norm:
        failures.append(
            f"memories: {mem_huge_norm} rows with L2 norm > 1e6 * dim (priority 2)"
        )
    if ref_mismatch:
        failures.append(
            f"reference_chunks: {ref_mismatch} rows with non-matching fingerprint"
        )
    if ref_null:
        failures.append(
            f"reference_chunks: {ref_null} rows with NULL fingerprint columns"
        )
    if ref_bad_len:
        failures.append(
            f"reference_chunks: {ref_bad_len} rows with decoded length != declared embedding_dimension"
        )
    if ref_undecodable:
        failures.append(
            f"reference_chunks: {ref_undecodable} rows with undecodable embedding BLOB"
        )
    if ref_zero_norm:
        failures.append(
            f"reference_chunks: {ref_zero_norm} rows with all-zero vector (silent provider failure)"
        )
    if ref_nonfinite:
        failures.append(
            f"reference_chunks: {ref_nonfinite} rows with NaN/Inf in vector (priority 2)"
        )
    if ref_huge_norm:
        failures.append(
            f"reference_chunks: {ref_huge_norm} rows with L2 norm > 1e6 * dim (priority 2)"
        )
    if missing_doc_ids:
        failures.append(
            f"memories: {len(missing_doc_ids)} expected doc_ids missing entirely: "
            f"{missing_doc_ids[:5]}{'...' if len(missing_doc_ids) > 5 else ''}"
        )
    if extra_doc_ids:
        # Phase A.3: rows in the collection that are NOT in the
        # expected set are a defect, not a feature. MPM-seed
        # prime-directive rows live in the `directives` collection
        # and never appear in the corpus collection; if they do,
        # the isolation contract is broken.
        failures.append(
            f"memories: {len(extra_doc_ids)} extra embedded rows in "
            f"collection {expected_collection!r} that are NOT in the "
            # ` extra_doc_ids`
            f"expected doc_ids set: "
            f"{extra_doc_ids[:5]}{'...' if len(extra_doc_ids) > 5 else ''}"
        )
    if missing_reference_doc_ids:
        failures.append(
            f"reference_chunks: {len(missing_reference_doc_ids)} expected "
            f"ref doc_ids have no embedded chunk: "
            f"{missing_reference_doc_ids[:5]}{'...' if len(missing_reference_doc_ids) > 5 else ''}"
        )

    return VerificationResult(
        ok=not failures,
        failures=tuple(failures),
        expected=(expected_model, expected_dimension, expected_source),
        snapshot_path=snapshot_path,
    )
