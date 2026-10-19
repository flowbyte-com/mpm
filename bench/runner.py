"""Per-query runner (C5, gate 4 raw-cosine).

The runner invokes `mpm recall --semantic --json` once per query
against the disposable workspace, parses the JSON envelope, and
joins the abbreviated IDs back to the corpus's full IDs via an
in-memory map (built at ingest time).

The runner is also the home of the raw-cosine experiment (gate 4
of HAL's implementation gates): when the harness is run with
--raw-cosine-prefix, the runner does NOT call mpm recall. It
reads the embedded query and document vectors directly from the
disposable DB, ranks them by cosine similarity, and returns the
top-k. This is independent of the hybrid ranker.

Per-query source participation (C5 / gate 4 of v2 review):
  - Positive query: must have at least one row with
    source in {"vector", "hybrid"}.
  - Negative query: any outcome is accepted; correctly-empty
    results are reported as positive signals.
"""

from __future__ import annotations

import json
import math
import sqlite3
import subprocess
from dataclasses import dataclass
from pathlib import Path
from typing import Any

from bench.corpus import Corpus, Query
from bench.workspace import scrubbed_env


@dataclass(frozen=True)
class QueryResult:
    query_id: str
    query: str
    is_positive: bool
    rows: tuple[dict[str, Any], ...]  # each: {id, content, source, score, vector_similarity}
    search_mode: str  # "hybrid" or "raw_cosine"
    raw_cosine: bool  # True if this came from the raw-cosine path

    def sources(self) -> set[str]:
        return {r["source"] for r in self.rows}

    def vector_participated(self) -> bool:
        return bool(self.sources() & {"vector", "hybrid"})


def run_query(
    query: Query,
    snap: Path,
    mpm_binary: str,
    raw_cosine: bool = False,
    top_k: int = 5,
    embedding_profile: dict[str, object] | None = None,
) -> QueryResult:
    """Run a single query against the disposable workspace and return
    the parsed result.

    If `raw_cosine` is True, this function does NOT call mpm recall.
    It reads the query and document vectors from the disposable DB
    directly, computes cosine similarity, and ranks. This is the
    raw-cosine experiment path (gate 4 of HAL's implementation gates).

    `embedding_profile` is required when `raw_cosine=True`. The raw-
    cosine experiment needs to embed the query with the same provider
    the documents were embedded with, otherwise the comparison is
    apples-to-oranges. The profile is a dict of the form written by
    `write_mpm_config`: `{"provider": ..., "model": ..., "base_url":
    ..., "api_key": ...}`.
    """
    if raw_cosine:
        if embedding_profile is None:
            raise ValueError(
                "raw_cosine=True requires an embedding_profile (provider/model/"
                "base_url/api_key) so the query can be embedded with the same "
                "provider the documents were embedded with"
            )
        rows = _raw_cosine_ranking(
            query, snap, top_k=top_k, embedding_profile=embedding_profile
        )
        return QueryResult(
            query_id=query.query_id,
            query=query.query,
            is_positive=query.is_positive,
            rows=tuple(rows),
            search_mode="raw_cosine",
            raw_cosine=True,
        )

    env = scrubbed_env(snap)
    cmd = [
        mpm_binary,
        "recall",
        "--semantic",
        "--json",
        "--collection",
        "memories",
        query.query,
    ]
    proc = subprocess.run(
        cmd,
        env=env,
        cwd=str(snap),
        capture_output=True,
        text=True,
        timeout=120,
        check=False,
    )
    if proc.returncode != 0:
        raise RuntimeError(
            f"mpm recall --semantic --json failed: rc={proc.returncode} "
            f"stderr={proc.stderr[:200]}"
        )
    try:
        envelope = json.loads(proc.stdout)
    except json.JSONDecodeError as e:
        raise RuntimeError(
            f"mpm recall returned non-JSON output: {e}; raw={proc.stdout[:200]!r}"
        ) from e
    memories = envelope.get("memories", [])
    rows: list[dict[str, Any]] = []
    for m in memories[:top_k]:
        rows.append(
            {
                "id": m.get("id", ""),
                "content": m.get("content", ""),
                "source": m.get("source", ""),
                "score": float(m.get("score", 0.0)),
                "vector_similarity": float(m.get("vector_similarity", 0.0)),
            }
        )
    return QueryResult(
        query_id=query.query_id,
        query=query.query,
        is_positive=query.is_positive,
        rows=tuple(rows),
        search_mode=envelope.get("search_mode", "hybrid"),
        raw_cosine=False,
    )


def _raw_cosine_ranking(
    query: Query,
    snap: Path,
    top_k: int,
    embedding_profile: dict[str, object] | None = None,
) -> list[dict[str, Any]]:
    """Compute raw-cosine ranking directly from the embedded vectors
    in the disposable DB. Does NOT use the hybrid ranker. This is
    gate 4 of HAL's implementation gates.

    The "query embedding" is obtained by calling the configured
    provider's embedding endpoint with the same profile the
    documents were embedded with. The "document embeddings" are the
    BLOB-stored JSON float32 vectors in the disposable DB.

    For hermetic tests the provider is the fake server, and the
    fake server's `deterministic_vector` is the same hash function
    used at document-embed time, so the answer is byte-equal to
    the pre-fix `deterministic_vector` path. For real runs against
    Ollama or any other real provider, the query vector now comes
    from the same provider the documents used, so the raw-cosine
    comparison is meaningful.

    Backward-compat: if `embedding_profile` is None, fall back to
    `deterministic_vector` (the pre-fix behaviour). This is the
    hermetic path; the harness's `--smoke` and the runner tests
    do not have an HTTP provider wired in, so they need the hash
    fallback. Real runs MUST pass `embedding_profile`.
    """
    from bench.harness import _find_db_path

    db_path = _find_db_path(snap)
    if db_path is None:
        return []

    # Discover the dimension the corpus was embedded with.
    con = sqlite3.connect(str(db_path))
    try:
        row = con.execute(
            "SELECT embedding_dimension FROM memories "
            "WHERE embedding IS NOT NULL LIMIT 1"
        ).fetchone()
        if row is None:
            return []
        dim = int(row[0])
    finally:
        con.close()

    # Compute the query embedding.
    if embedding_profile is None:
        # Hermetic / test fallback: hash the query text the same
        # way the fake server does. This is the only path that
        # gives a meaningful answer without a real provider.
        from bench.fake_server import deterministic_vector
        qvec = deterministic_vector(query.query, dim)
    else:
        from bench.embed_query import embed_query, l2_normalize
        provider = str(embedding_profile.get("provider", ""))
        model = str(embedding_profile.get("model", ""))
        base_url = str(embedding_profile.get("base_url", ""))
        api_key = str(embedding_profile.get("api_key", ""))
        qvec_raw = embed_query(
            query.query, provider, model, base_url, api_key
        )
        if len(qvec_raw) != dim:
            # The provider returned a different-dimension vector
            # than the corpus. Surface this clearly rather than
            # silently misaligning the comparison.
            return []
        qvec = l2_normalize(qvec_raw)

    con = sqlite3.connect(str(db_path))
    try:
        scored: list[tuple[float, str, list[float]]] = []
        for did, blob in con.execute(
            "SELECT id, embedding FROM memories WHERE embedding IS NOT NULL AND deleted_at IS NULL"
        ):
            # BLOBs may be returned as bytes, str, or memoryview
            # depending on sqlite3 driver config.
            if isinstance(blob, (bytes, bytearray, memoryview)):
                try:
                    text = bytes(blob).decode("utf-8")
                except UnicodeDecodeError:
                    continue
            elif isinstance(blob, str):
                text = blob
            else:
                continue
            try:
                vec = json.loads(text)
            except json.JSONDecodeError:
                continue
            if not isinstance(vec, list) or len(vec) != dim:
                continue
            dot = sum(a * b for a, b in zip(qvec, vec))
            scored.append((dot, did, vec))
    finally:
        con.close()

    scored.sort(key=lambda r: r[0], reverse=True)
    out: list[dict[str, Any]] = []
    for sim, did, _ in scored[:top_k]:
        out.append(
            {
                "id": did,
                "content": "",
                "source": "vector",
                "score": sim,
                "vector_similarity": sim,
            }
        )
    return out


def per_query_participation(
    results: list[QueryResult],
    positive_query_ids: set[str],
) -> dict[str, Any]:
    """Compute the per-query participation report (C5).

    Positive queries: at least one row with source in
    {"vector", "hybrid"}. Failure -> list in `positive_failures`.
    Negative queries: any outcome is acceptable. Empty result is
    recorded as a positive signal under `negative_outcomes.correctly_empty`.
    """
    positive_failures: list[dict[str, Any]] = []
    negative_outcomes: dict[str, list[str]] = {
        "correctly_empty": [],
        "vector_participated": [],
        "fts5_only": [],
    }
    for r in results:
        if r.is_positive:
            if not r.vector_participated():
                positive_failures.append(
                    {
                        "query_id": r.query_id,
                        "query": r.query,
                        "reason": "positive query, no vector or hybrid row",
                        "rows": list(r.rows),
                    }
                )
        else:
            if not r.rows:
                negative_outcomes["correctly_empty"].append(r.query_id)
            elif r.vector_participated():
                negative_outcomes["vector_participated"].append(r.query_id)
            else:
                negative_outcomes["fts5_only"].append(r.query_id)
    return {
        "positive_total": len(positive_query_ids),
        "positive_failures": positive_failures,
        "positive_vector_participated": len(positive_query_ids) - len(positive_failures),
        "negative_total": len(results) - len(positive_query_ids),
        "negative_outcomes": negative_outcomes,
        "raw_cosine_run": all(r.raw_cosine for r in results),
    }
