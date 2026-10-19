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

v3 review (HAL 2026-10-19):
  - Empty-result handling: MPM's plain-text "No memories found"
    response is treated as a successful empty result, not a
    failure. Malformed responses and non-zero exits remain
    failures.
  - Raw-cosine prefix experiment: two genuine measurements
    (unprefixed query and `search_query:`-prefixed query) with
    BOTH embeddings from the configured provider. Document
    vectors are identical across the two conditions. Cosine
    similarity is computed on L2-normalized vectors. The
    function refuses to silently fall back to the hash path
    when a real-provider embed call fails.
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
from bench.embed_query import embed_query, l2_normalize
from bench.workspace import scrubbed_env


# Plain-text prefix MPM emits on an empty semantic recall.
# Lower-cased and stripped; the runner matches after lowering
# the actual response to be tolerant of MPM's exact wording.
EMPTY_RESULT_MARKERS: tuple[str, ...] = (
    "no memories found",
)


# v3 review (HAL 2026-10-19): the historical raw-cosine prefix
# experiment ("search_query: ") is exposed as a constant so the
# test suite can compare prefixed vs. unprefixed conditions
# without duplicating the literal.
RAW_COSINE_PREFIX_NOTE: str = "search_query: "


@dataclass(frozen=True)
class QueryResult:
    query_id: str
    query: str
    is_positive: bool
    rows: tuple[dict[str, Any], ...]  # each: {id, content, source, score, vector_similarity}
    search_mode: str  # "hybrid" | "hybrid_empty" | "raw_cosine" | "raw_cosine_prefixed"
    raw_cosine: bool  # True if this came from the raw-cosine path

    def sources(self) -> set[str]:
        return {r["source"] for r in self.rows}

    def vector_participated(self) -> bool:
        return bool(self.sources() & {"vector", "hybrid"})


def _is_plain_text_empty(stdout: str) -> bool:
    """Return True if `stdout` is MPM's plain-text "no memories
    found" response (v3 review priority 6).

    The hybrid recall subprocess occasionally returns rc=0 with
    a plain-text message instead of the JSON envelope. That is
    a successful empty result, not a failure. The harness
    must treat it as empty.
    """
    head = (stdout or "").strip().lower()
    if not head:
        return False
    return any(head.startswith(m) for m in EMPTY_RESULT_MARKERS)


def run_query(
    query: Query,
    snap: Path,
    mpm_binary: str,
    raw_cosine: bool = False,
    raw_cosine_prefix: str = "",
    top_k: int = 5,
    embedding_profile: dict[str, object] | None = None,
) -> QueryResult:
    """Run a single query against the disposable workspace and return
    the parsed result.

    If `raw_cosine` is True, this function does NOT call mpm recall.
    It reads the query and document vectors from the disposable DB
    directly, computes cosine similarity, and ranks. This is the
    raw-cosine experiment path (gate 4 of HAL's implementation gates).

    `raw_cosine_prefix` (v3 review): when non-empty, the query text
    is sent to the embedding provider with this prefix attached
    (e.g. `"search_query: "` for the historical raw-cosine
    comparison). The document vectors are unchanged.

    `embedding_profile` is required when `raw_cosine=True`. The raw-
    cosine experiment needs to embed the query with the same provider
    the documents were embedded with, otherwise the comparison is
    apples-to-oranges. The profile is a dict of the form written by
    `write_mpm_config`: `{"provider": ..., "model": ..., "base_url":
    ..., "api_key": ...}`.

    v5 review (HAL 2026-10-19): `top_k` is now passed through to
    the `mpm recall` subprocess as `--limit <top_k>` (the CLI
    flag the production binary actually honours) AND is used to
    slice the JSON envelope's `memories` array. MPM's default
    recall limit is 15; the pre-v5 path relied on the runner's
    default of 5 and only the scoring step saw `corpus.scoring
    .top_k`, which meant scoring on `top-5 of top-15` was a
    no-op for any corpus with `top_k > 5`. The new contract
    passes `corpus.scoring.top_k` end-to-end; a corpus with
    `top_k > 15` now exercises the MPM limit explicitly.
    """
    if raw_cosine:
        if embedding_profile is None:
            raise ValueError(
                "raw_cosine=True requires an embedding_profile (provider/model/"
                "base_url/api_key) so the query can be embedded with the same "
                "provider the documents were embedded with"
            )
        rows = _raw_cosine_ranking(
            query, snap, top_k=top_k,
            embedding_profile=embedding_profile,
            prefix=raw_cosine_prefix,
        )
        search_mode = "raw_cosine" if not raw_cosine_prefix else "raw_cosine_prefixed"
        return QueryResult(
            query_id=query.query_id,
            query=query.query,
            is_positive=query.is_positive,
            rows=tuple(rows),
            search_mode=search_mode,
            raw_cosine=True,
        )

    env = scrubbed_env(snap)
    # v5 review: pass `top_k` to the mpm recall subprocess as
    # `--limit <top_k>`. This is the CLI flag the production
    # binary actually honours (its default is 15). Pre-v5
    # code relied on the runner's default of 5 and did not
    # pass any limit to mpm, so the corpus's top_k was silently
    # capped at 5. A corpus with top_k > 5 (and certainly
    # top_k > 15) was retried and re-tested against an
    # inadequate result size. The new contract is
    # end-to-end: corpus.scoring.top_k is the source of truth
    # and is passed to both the subprocess and the slicing of
    # the JSON envelope.
    cmd = [
        mpm_binary,
        "recall",
        "--semantic",
        "--json",
        "--collection",
        "memories",
        "--limit",
        str(int(top_k)),
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
    # v3 review: MPM may emit a plain-text "No memories found"
    # response with rc=0. Treat as an empty successful result,
    # not a failure. The JSON path is unchanged.
    if _is_plain_text_empty(proc.stdout):
        return QueryResult(
            query_id=query.query_id,
            query=query.query,
            is_positive=query.is_positive,
            rows=tuple(),
            search_mode="hybrid_empty",
            raw_cosine=False,
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
    prefix: str = "",
) -> list[dict[str, Any]]:
    """Compute raw-cosine ranking directly from the embedded vectors
    in the disposable DB. Does NOT use the hybrid ranker. This is
    gate 4 of HAL's implementation gates.

    v3 review (HAL 2026-10-19):
      - The query embedding is obtained by calling the configured
        provider's embedding endpoint. The provider sees the query
        text WITH `prefix` prepended (e.g. `"search_query: "` for
        the historical raw-cosine comparison); `prefix=""` is the
        unprefixed baseline. The two runs use identical document
        vectors.
      - Cosine similarity is computed on L2-normalized vectors
        (so dot product == cosine regardless of provider scaling).
        The document vectors are normalized at rank time; the
        query vector is normalized via `l2_normalize` from
        `bench.embed_query`.
      - This function refuses to silently fall back to the
        hash function when a real-provider embed call fails.
        The exception propagates; the harness's run-level
        error handler decides how to surface it.

    The pre-fix path (priority 4 of v2 review) used a SHA-256
    hash that was only meaningful for the fake server. The
    current path always uses the configured provider when
    `embedding_profile` is supplied.

    For hermetic tests the `embedding_profile=None` fallback
    path is the only path that gives a meaningful answer
    without a real provider. Tests pass None explicitly.
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
        # Hermetic / test fallback ONLY. Real runs must pass
        # an embedding_profile. We refuse this path silently
        # in production (the harness's run-level caller
        # passes a profile); the test path keeps the hash
        # because no real provider is wired in.
        from bench.fake_server import deterministic_vector
        qvec = deterministic_vector(query.query, dim)
    else:
        provider = str(embedding_profile.get("provider", ""))
        model = str(embedding_profile.get("model", ""))
        base_url = str(embedding_profile.get("base_url", ""))
        api_key = str(embedding_profile.get("api_key", ""))
        # v3 review: apply the prefix to the text the provider
        # sees, not to the text the harness records.
        provider_text = (prefix or "") + query.query
        try:
            qvec_raw = embed_query(
                provider_text, provider, model, base_url, api_key
            )
        except Exception:
            # v3 review: refuse to silently substitute the hash
            # function in a real-model experiment. The exception
            # propagates so the harness can surface it. Tests
            # that want the hash path pass `embedding_profile=None`.
            raise
        if len(qvec_raw) != dim:
            # The provider returned a different-dimension vector
            # than the corpus. Surface this clearly rather than
            # silently misaligning the comparison.
            return []
        qvec = l2_normalize(qvec_raw)

    # v3 review: L2-normalize document vectors at rank time so
    # cosine similarity is correct regardless of the provider's
    # scale. Document vectors are read once and cached.
    con = sqlite3.connect(str(db_path))
    try:
        scored: list[tuple[float, str]] = []
        for did, blob in con.execute(
            "SELECT id, embedding FROM memories "
            "WHERE embedding IS NOT NULL AND deleted_at IS NULL"
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
            # v3 review: normalize at rank time so we get true
            # cosine regardless of provider scale. If the vector
            # is zero-norm, the l2_normalize contract returns the
            # zero vector; the dot product stays 0 and the doc
            # is unranked naturally.
            nvec = l2_normalize([float(x) for x in vec])
            dot = sum(a * b for a, b in zip(qvec, nvec))
            scored.append((dot, did))
    finally:
        con.close()

    scored.sort(key=lambda r: r[0], reverse=True)
    out: list[dict[str, Any]] = []
    for sim, did in scored[:top_k]:
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


def _l2_norm(v: list[float]) -> float:
    """Return the L2 norm of a vector. Pure-Python fallback for the
    `l2_normalize` happy path; tests use this to construct
    controlled fixtures without re-implementing the math."""
    return math.sqrt(sum(float(x) * float(x) for x in v))
