"""Nomic instruction-prefix condition matrix (MPM Issue #23, Phase A).

This module is the experiment harness for Issue #23. It answers one
question with evidence rather than assertion: does applying Nomic's
documented instruction prefixes (`search_document: ` for indexed
text, `search_query: ` for queries) change retrieval quality on
MPM's labelled corpus?

Why a condition matrix and not a single A/B toggle
--------------------------------------------------

The historical "+0.022 MRR" figure attributed to prefixing was
measured on a corpus that no longer exists, on a model set that is
not the current installation, and with a harness that no longer
exists. It cannot be reproduced or refuted by re-running anything.
What CAN be measured is the *mechanics* of the prefix on a labelled
corpus, under controlled conditions, with the current model. That
requires a matrix:

    A  baseline          unprefixed document      unprefixed query
    B  query-only        unprefixed document      `search_query: `
    C  both roles        `search_document: ` doc  `search_query: `
    D  document-only     `search_document: ` doc  unprefixed query
                        (diagnostic only)

A and B share one document preparation; C and D share another. A
condition that needs a different DOCUMENT embedding therefore gets a
different disposable workspace, and vectors from differently-prepared
document conditions can never be mixed.

Where the prefix is applied
---------------------------

The prefix is applied by the BENCHMARK, never by production code:

  - Document side: the corpus row's text is prefixed at ingest time,
    so MPM's own production embed path
    (`cmd/mpm/backfill_embeddings.go` -> `cfg.Provider.Embed(mem.Content)`)
    embeds exactly the string a prefixing implementation would embed.
    The vector is therefore produced by production code, not by the
    benchmark re-implementing the provider.
  - Query side: the benchmark prepends the prefix to the query text
    before calling the provider's embedding endpoint, which is the
    same shape MPM's recall path uses (`internal/core/hybrid_search.go`).

`internal/core` is NOT modified. The module docstring of
`bench/README.md` and the report both record this.

What this module deliberately does NOT do
-----------------------------------------

  - It does not change production embedding behaviour.
  - It does not re-embed the live store.
  - It does not compare the synthetic-corpus scores against the
    historical MRR 0.832: the test populations are different and the
    comparison is meaningless. The report states the deltas against
    the SAME-corpus unprefixed baseline only.
  - It does not measure the MPM hybrid ranker under prefixed
    conditions. The production hybrid ranker embeds the query
    verbatim (`internal/core/hybrid_search.go`), so no prefix
    condition is expressible through it without a production change.
    The hybrid arm is measured once, on the unprefixed workspace, and
    reported separately from the raw-cosine arm.
"""

from __future__ import annotations

import hashlib
import json
import sys
import urllib.error
import urllib.request
from dataclasses import dataclass
from pathlib import Path
from typing import Any, Sequence

from bench.corpus import Corpus, load_corpus
from bench.fingerprint import (
    VerificationResult,
    fingerprint_of_snapshot,
    verify_regeneration_complete,
)
from bench.harness import (
    DEFAULT_CORPUS_DIR,
    _backfill,
    _binary_provenance,
    _build_raw_cosine_profile,
    _find_db_path,
    _ingest_corpus,
    _make_snap_workspace,
    _mutation_diff,
    _resolve_mpm_binary,
    _snapshot_full,
    corpus_collection,
)
from bench.runner import QueryResult, run_query
from bench.scoring import score_queries
from bench.workspace import (
    assert_safe_output_dir,
    assert_safe_workspace,
    recheck_output_dir_safe,
    reject_unsafe_symlinks,
    write_mpm_config,
)


# Nomic's documented task prefixes. `search_document: ` is prepended
# to text that is being indexed; `search_query: ` is prepended to a
# query. The names mirror the constants the existing raw-cosine
# experiment already uses (`bench/harness.py:RAW_COSINE_PREFIX`).
DOCUMENT_PREFIX = "search_document: "
QUERY_PREFIX = "search_query: "


def _document_preparation(document_prefix: str) -> str:
    """The workspace key for a document preparation.

    DERIVED from the prefix alone. A caller cannot pair the
    unprefixed key with the prefixed text (or vice versa) because
    there is no way to state the key independently of the text.

    The key names the prefix it belongs to (`doc_search_document`),
    so it stays truthful if a different prefix is ever measured.
    Two different prefixes that slug to the same key are refused by
    the orchestrator rather than silently merged.
    """
    if not document_prefix:
        return "doc_unprefixed"
    return "doc_" + _slug(document_prefix).rstrip("_")


# Condition A IS the unprefixed baseline. Every comparison this
# experiment reports is "condition vs the same-corpus unprefixed
# baseline", so A is not one condition among four -- it is the
# reference every other condition is measured against.
BASELINE_CONDITION_KEY = "A"

# The workspace key the baseline condition lives in. The hybrid arm
# (MPM's production ranker) can only be measured here, because the
# production recall path embeds the query verbatim.
BASELINE_PREPARATION = _document_preparation("")


@dataclass(frozen=True)
class PrefixCondition:
    """One cell of the condition matrix."""

    key: str
    name: str
    document_prefix: str
    query_prefix: str
    description: str

    @property
    def document_preparation(self) -> str:
        """The key of the document preparation this condition needs.

        Conditions that share a document preparation share a
        workspace (and therefore share document vectors). Conditions
        with different document preparations NEVER share a workspace.
        """
        return _document_preparation(self.document_prefix)

    def to_dict(self) -> dict[str, Any]:
        return {
            "key": self.key,
            "name": self.name,
            "document_prefix": self.document_prefix,
            "query_prefix": self.query_prefix,
            "document_preparation": self.document_preparation,
            "description": self.description,
        }


CONDITIONS: tuple[PrefixCondition, ...] = (
    PrefixCondition(
        key="A",
        name="baseline-unprefixed",
        document_prefix="",
        query_prefix="",
        description=(
            "Unprefixed document, unprefixed query. This is MPM's "
            "current production embedding behaviour, verbatim."
        ),
    ),
    PrefixCondition(
        key="B",
        name="query-prefix-only",
        document_prefix="",
        query_prefix=QUERY_PREFIX,
        description=(
            "Unprefixed document, `search_query: `-prefixed query. The "
            "cheapest possible change: only the query embedding changes."
        ),
    ),
    PrefixCondition(
        key="C",
        name="both-roles",
        document_prefix=DOCUMENT_PREFIX,
        query_prefix=QUERY_PREFIX,
        description=(
            "`search_document: `-prefixed document, `search_query: `-prefixed "
            "query. Nomic's documented usage for both roles."
        ),
    ),
    PrefixCondition(
        key="D",
        name="document-prefix-diagnostic",
        document_prefix=DOCUMENT_PREFIX,
        query_prefix="",
        description=(
            "`search_document: `-prefixed document, unprefixed query. "
            "Diagnostic: isolates the document-side effect from the "
            "query-side effect."
        ),
    ),
)

CONDITIONS_BY_KEY: dict[str, PrefixCondition] = {c.key: c for c in CONDITIONS}

# The depths the directive asks for. `corpus.scoring.top_k` is the
# corpus's own ranking depth and is always included; 1 and 5 are the
# Recall@1 / Recall@5 depths.
FIXED_RECALL_DEPTHS: tuple[int, ...] = (1, 5)

# What each metric in this report means, stated in the artifact
# itself so a reader never has to guess. `bench/scoring.py` truncates
# the ranking to k BEFORE computing MRR, which makes MRR@1 a
# hit-rate and MRR@5 "reciprocal rank if the first relevant document
# is in the top five, else 0" — neither is the unbounded MRR, so
# they are labelled with their depth everywhere they appear.
METRIC_DEFINITIONS: dict[str, str] = {
    "population": (
        "Every mean in this report is over POSITIVE queries only. "
        "Negative queries have no graded relevance, are scored 0 on all "
        "four metrics, and are reported separately in the negative-query "
        "block."
    ),
    "precision_at_k": (
        "Fraction of the k returned rows that are labelled relevant."
    ),
    "recall_at_k": (
        "Fraction of the query's labelled relevant documents that appear "
        "in the top k."
    ),
    "mrr_at_k": (
        "Reciprocal rank of the FIRST relevant document within the top k, "
        "0 when none is in the top k. The ranking is TRUNCATED to k before "
        "scoring, so MRR@1 is the hit-rate and MRR@5 is 0 for any query "
        "whose first relevant document ranks 6th or lower. MRR@10 over a "
        "10-row corpus is the unbounded MRR."
    ),
    "ndcg_at_k": (
        "Normalised discounted cumulative gain over the top k, with an "
        "ideal ranking of min(k, number of relevant documents)."
    ),
    "delta": (
        "Condition minus the same-corpus unprefixed baseline (condition A) "
        "at the same depth. No number here is comparable with the "
        "historical MRR 0.832, which was measured on a different corpus "
        "with a different harness."
    ),
}


class PrefixExperimentError(RuntimeError):
    """Raised when the experiment cannot be run safely."""


class LogicalMutationError(PrefixExperimentError):
    """Raised when the hybrid benchmark detects a logical mutation.

    A mutation means the measurement's premise is broken: the recall
    path changed state that the comparison assumed was fixed, so the
    numbers it produced are not comparable with anything.

    The experiment FAILS on a mutation. It does not publish the
    partial result it had accumulated: a broken measurement written
    out as a successful one is worse than no measurement, because the
    next reader cannot tell the two apart.
    """


@dataclass
class DocumentWorkspace:
    """A disposable workspace holding ONE document preparation."""

    preparation: str
    document_prefix: str
    snap: Path
    db_path: Path
    verification: VerificationResult
    fingerprint: dict[str, list[tuple]]
    backfill_rc: int
    backfill_output: str


def _slug(prefix: str) -> str:
    """Make a filesystem-safe slug from a prefix string."""
    out = []
    for ch in prefix.strip():
        out.append(ch if (ch.isalnum() or ch in "-_") else "_")
    return "".join(out) or "none"


def _assert_no_double_prefix(corpus: Corpus) -> None:
    """Refuse to run if any corpus text already carries a prefix.

    This is the guard against accidental double-prefixing: if a
    corpus document already contained `search_document: `, the
    ingest-time prefix would produce `search_document: search_document: `
    and the measurement would silently be of a different condition
    than the one named.

    The check is a SUBSTRING check, not a prefix check: any
    occurrence of either instruction prefix anywhere in a document
    or query refuses the run. A stricter guard costs nothing here
    because no corpus text legitimately contains either phrase.
    """
    for d in corpus.documents:
        if DOCUMENT_PREFIX in d.content or QUERY_PREFIX in d.content:
            raise PrefixExperimentError(
                f"corpus document {d.doc_id} already contains an instruction "
                f"prefix; refusing to risk a double-prefixed measurement"
            )
    for q in corpus.queries:
        if DOCUMENT_PREFIX in q.query or QUERY_PREFIX in q.query:
            raise PrefixExperimentError(
                f"corpus query {q.query_id} already contains an instruction "
                f"prefix; refusing to risk a double-prefixed measurement"
            )


def _http_json(
    url: str,
    payload: dict[str, Any] | None = None,
    timeout: int = 15,
) -> Any:
    """Best-effort JSON fetch. Returns None on any failure."""
    data = None
    headers = {"Content-Type": "application/json"}
    if payload is not None:
        data = json.dumps(payload).encode("utf-8")
    req = urllib.request.Request(
        url, data=data, method="POST" if data is not None else "GET",
        headers=headers,
    )
    try:
        with urllib.request.urlopen(req, timeout=timeout) as resp:  # noqa: S310
            raw = resp.read()
        return json.loads(raw)
    except (urllib.error.URLError, urllib.error.HTTPError, OSError,
            json.JSONDecodeError):
        return None


def provider_model_metadata(
    profile: dict[str, Any] | None,
) -> dict[str, Any] | None:
    """Best-effort model identity for an Ollama-shaped provider.

    The fixture names a model (`nomic-embed-text`) and a dimension
    (768), but the *digest* is what pins the weights. For an
    Ollama-shaped base URL the identity comes from two endpoints:

    * `POST {base}/api/show` — family, parameter size, declared
      embedding length, parameters and the prompt template;
    * `GET {base}/api/tags` — the weight digest, size and modified
      timestamp of the installed model.

    (This Ollama build does not include the digest in the `/api/show`
    reply, so the listing endpoint is the one that carries it.)

    Returns None when the provider is not Ollama-shaped or the call
    fails. This is provenance capture only; it never fails the run.
    """
    if not profile:
        return None
    base = str(profile.get("base_url", "")).rstrip("/")
    model = str(profile.get("model", ""))
    if not base or not model:
        return None
    if not base.endswith("/v1"):
        return None
    root = base[: -len("/v1")]
    url = root + "/api/show"
    reply = _http_json(url, {"model": model})
    if not isinstance(reply, dict):
        return None
    details = reply.get("details") or {}
    model_info = reply.get("model_info") or {}
    # Ollama reports the embedding length inside `model_info` (keyed
    # by architecture); fall back across shapes rather than recording
    # a null where a value exists.
    digest = reply.get("digest") or details.get("digest")
    embedding_length = details.get("embedding_length")
    if embedding_length is None:
        for key, value in model_info.items():
            if key.endswith("embedding_length"):
                embedding_length = value
                break

    tags = _http_json(root + "/api/tags")
    if isinstance(tags, dict):
        for entry in tags.get("models") or []:
            name = str(entry.get("name", ""))
            if name in (model, model + ":latest"):
                digest = digest or entry.get("digest")
                break

    return {
        "provider_endpoint": url,
        "model": model,
        "digest": digest,
        "details": {
            "family": details.get("family"),
            "parameter_size": details.get("parameter_size"),
            "embedding_length": embedding_length,
            "families": details.get("families"),
        },
        "parameters": reply.get("parameters"),
        "template": reply.get("template"),
        "license": (reply.get("license") or "")[:120] or None,
    }


def prepare_document_workspace(
    document_prefix: str,
    corpus: Corpus,
    config: dict[str, Any],
) -> DocumentWorkspace:
    """Create, populate, embed and verify ONE document preparation.

    The workspace is disposable and private. The corpus is ingested
    with the document prefix applied to the row text (so MPM's own
    production embed path embeds the prefixed string), then MPM's
    backfill runs, then the fingerprint gate verifies the result.

    The preparation key is derived from the prefix, never passed
    alongside it: a caller cannot pair the unprefixed key with the
    prefixed text.
    """
    preparation = _document_preparation(document_prefix)
    snap = assert_safe_workspace(
        _make_snap_workspace(f"mpm-bench-cond-{_slug(preparation)}-")
    )
    bad = reject_unsafe_symlinks(snap)
    if bad:
        raise PrefixExperimentError(
            f"unsafe symlinks in snapshot workspace {snap}: {bad}"
        )
    write_mpm_config(
        snap,
        profile=config["profiles"],
        components=config["components"],
    )
    _ingest_corpus(snap, corpus, document_prefix=document_prefix)
    rc, out = _backfill(snap)
    db_path = _find_db_path(snap)
    if db_path is None:
        raise PrefixExperimentError(
            f"mpm did not create a database under {snap}"
        )
    expected = config.get("expected") or {}
    verification = verify_regeneration_complete(
        db_path,
        expected_model=str(expected.get("model", "")),
        expected_dimension=int(expected.get("dimension", 0)),
        expected_doc_ids={d.doc_id for d in corpus.documents},
        expected_memories_row_count=len(corpus.documents),
        expected_collection=corpus_collection(),
    )
    return DocumentWorkspace(
        preparation=preparation,
        document_prefix=document_prefix,
        snap=snap,
        db_path=db_path,
        verification=verification,
        fingerprint=fingerprint_of_snapshot(db_path),
        backfill_rc=rc,
        backfill_output=out[-4000:],
    )


def _mean(xs: Sequence[float]) -> float:
    xs = list(xs)
    return (sum(xs) / len(xs)) if xs else 0.0


def _named_aggregates(aggregates: dict[str, Any], k: int) -> dict[str, Any]:
    """Rename `bench/scoring.py`'s aggregate keys to depth-explicit ones.

    `scoring.py` reports `mean_mrr` while its three siblings carry
    `_at_k`. In a report where the SAME metric is reported at three
    depths, an unlabelled `mean_mrr` is misleading: MRR@1 and MRR@10
    are different quantities. This renames it to `mean_mrr_at_k` so
    every metric in the artifact states its depth.
    """
    out = dict(aggregates)
    if "mean_mrr" in out:
        out["mean_mrr_at_k"] = out.pop("mean_mrr")
    out["depth"] = k
    out["population"] = "positive queries only"
    return out


def _median(xs: Sequence[float]) -> float:
    xs = sorted(xs)
    if not xs:
        return 0.0
    mid = len(xs) // 2
    if len(xs) % 2:
        return float(xs[mid])
    return (float(xs[mid - 1]) + float(xs[mid])) / 2.0


def _first_relevant_rank(top_ids: Sequence[str], relevant: frozenset[str]) -> int | None:
    """1-indexed rank of the first relevant document, or None."""
    for i, tid in enumerate(top_ids):
        if tid in relevant:
            return i + 1
    return None


def _rank_display(rank: int | None) -> str:
    """Render a rank for humans: `None` means 'not in the top k'."""
    return "—" if rank is None else str(rank)


def _aggregate(
    aggregates_by_depth: dict[str, Any],
    depth: str,
    metric: str,
) -> float:
    """One aggregate value, 0.0 when the depth or metric is absent."""
    entry = aggregates_by_depth.get(depth) or {}
    return (entry.get("aggregates") or {}).get(metric, 0.0)


def score_condition(
    corpus: Corpus,
    results: Sequence[QueryResult],
    depths: Sequence[int],
) -> dict[str, Any]:
    """Score one condition's ranking.

    `score_queries` computes one depth per call, so the condition is
    scored once per depth. Recall@1 and Recall@5 come from the k=1
    and k=5 calls; the headline MRR comes from the corpus's own
    `scoring.top_k` depth.

    Note on MRR at a depth: `score_query` truncates the ranking to
    `k` before computing MRR, so the k=1 MRR is a hit-rate and the
    k=5 MRR is "reciprocal rank if the first relevant document is in
    the top 5". The headline MRR is reported at the corpus depth and
    labelled as such.
    """
    tuples = [
        (
            q.query_id,
            q.is_positive,
            tuple(r["id"] for r in res.rows),
            q.relevant_doc_ids,
        )
        for q, res in zip(corpus.queries, results)
    ]
    by_depth: dict[str, Any] = {}
    per_query_by_depth: dict[str, dict[str, Any]] = {}
    for k in depths:
        scored = score_queries(tuples, k=k)
        by_depth[str(k)] = {
            "k": k,
            "positive_total": scored["positive_total"],
            "negative_total": scored["negative_total"],
            "aggregates": _named_aggregates(scored["aggregates"], k),
        }
        per_query_by_depth[str(k)] = {
            q["query_id"]: q for q in scored["per_query"]
        }

    paraphrase_ids = {q.query_id for q in corpus.queries if q.paraphrase_of}
    positive_ids = {q.query_id for q in corpus.positive_queries}

    rows: list[dict[str, Any]] = []
    for q, res in zip(corpus.queries, results):
        top_ids = [r["id"] for r in res.rows]
        relevant = frozenset(q.relevant_doc_ids)
        row: dict[str, Any] = {
            "query_id": q.query_id,
            "query": q.query,
            "class": "positive" if q.is_positive else "negative",
            "split": q.split,
            "topic_family": q.topic_family,
            "is_paraphrase": bool(q.paraphrase_of),
            "paraphrase_of": q.paraphrase_of,
            "n_relevant": len(relevant),
            "n_rows": len(res.rows),
            "top1_id": top_ids[0] if top_ids else "",
            "top1_score": res.rows[0]["score"] if res.rows else 0.0,
            "first_relevant_rank": _first_relevant_rank(top_ids, relevant),
            "top_ids": top_ids,
        }
        for k in depths:
            sq = per_query_by_depth[str(k)][q.query_id]
            row[f"mrr_at_{k}"] = sq["mrr"]
            row[f"recall_at_{k}"] = sq["recall_at_k"]
            row[f"ndcg_at_{k}"] = sq["ndcg_at_k"]
        rows.append(row)

    def subset_metrics(ids: set[str]) -> dict[str, Any]:
        out: dict[str, Any] = {"n": len(ids)}
        for k in depths:
            sel = [r for r in rows if r["query_id"] in ids]
            out[f"mean_recall_at_{k}"] = _mean([r[f"recall_at_{k}"] for r in sel])
            out[f"mean_mrr_at_{k}"] = _mean([r[f"mrr_at_{k}"] for r in sel])
            out[f"mean_ndcg_at_{k}"] = _mean([r[f"ndcg_at_{k}"] for r in sel])
        return out

    positive_rows = [r for r in rows if r["query_id"] in positive_ids]
    negative_rows = [r for r in rows if r["query_id"] not in positive_ids]
    hard_negative_rows = [r for r in negative_rows if "Hard negative" in _rationale(corpus, r["query_id"])]

    # The v2 corpus carries an explicit calibration / held-out split.
    # The held-out scope is a topic family the calibration scope never
    # saw, so a prefix that only helps on calibration-family topics
    # is visible here rather than hidden in the aggregate.
    #
    # Split metrics are computed over the POSITIVE queries of the
    # split only: a negative query has no relevant document, so its
    # MRR is 0 by construction and would only dilute the mean.
    by_split: dict[str, Any] = {}
    for split in sorted({q.split for q in corpus.queries if q.split}):
        split_ids = {q.query_id for q in corpus.queries if q.split == split}
        split_pos = split_ids & positive_ids
        entry = subset_metrics(split_pos)
        entry["n_total"] = len(split_ids)
        entry["n_negative"] = len(split_ids - positive_ids)
        by_split[split] = entry

    # Every subset mean in this report is over POSITIVE queries only.
    # `paraphrase_ids` is intersected with `positive_ids` so the rule
    # holds even for a corpus that adds a negative paraphrase.
    paraphrase_pos = paraphrase_ids & positive_ids
    paraphrase_block = subset_metrics(paraphrase_pos)
    paraphrase_block["n_total"] = len(paraphrase_ids)
    paraphrase_block["n_negative"] = len(paraphrase_ids - positive_ids)

    return {
        "depths": list(depths),
        "aggregates_by_depth": by_depth,
        "paraphrase": paraphrase_block,
        "positive": subset_metrics(positive_ids),
        "by_split": by_split,
        "negative": {
            "n": len(negative_rows),
            "mean_top1_similarity": _mean([r["top1_score"] for r in negative_rows]),
            "median_top1_similarity": _median([r["top1_score"] for r in negative_rows]),
            "max_top1_similarity": max((r["top1_score"] for r in negative_rows), default=0.0),
            "min_top1_similarity": min((r["top1_score"] for r in negative_rows), default=0.0),
        },
        "hard_negative": {
            "n": len(hard_negative_rows),
            "mean_top1_similarity": _mean([r["top1_score"] for r in hard_negative_rows]),
            "max_top1_similarity": max((r["top1_score"] for r in hard_negative_rows), default=0.0),
            "rows": [
                {
                    "query_id": r["query_id"],
                    "top1_id": r["top1_id"],
                    "top1_score": r["top1_score"],
                }
                for r in hard_negative_rows
            ],
        },
        "score_separation": {
            "mean_top1_similarity_positive": _mean([r["top1_score"] for r in positive_rows]),
            "mean_top1_similarity_negative": _mean([r["top1_score"] for r in negative_rows]),
        },
        "rows": rows,
    }


def _rationale(corpus: Corpus, query_id: str) -> str:
    for q in corpus.queries:
        if q.query_id == query_id:
            return q.label_rationale
    return ""


def compare_to_baseline(
    baseline: dict[str, Any],
    condition: dict[str, Any],
    baseline_key: str,
    condition_key: str,
    depths: Sequence[int],
) -> dict[str, Any]:
    """Per-query and aggregate comparison of one condition to the baseline.

    The comparison is strictly WITHIN the same corpus: the baseline
    is condition A of the same run. No number in this function is
    comparable to the historical MRR 0.832.
    """
    base_rows = {r["query_id"]: r for r in baseline["rows"]}
    cond_rows = {r["query_id"]: r for r in condition["rows"]}
    headline = str(max(depths))

    deltas: list[dict[str, Any]] = []
    improved: list[dict[str, Any]] = []
    regressed: list[dict[str, Any]] = []
    unchanged = 0
    for qid, b in base_rows.items():
        c = cond_rows.get(qid)
        if c is None:
            continue
        d_mrr = c[f"mrr_at_{headline}"] - b[f"mrr_at_{headline}"]
        entry = {
            "query_id": qid,
            "class": b["class"],
            "split": b["split"],
            "is_paraphrase": b["is_paraphrase"],
            "baseline_mrr": b[f"mrr_at_{headline}"],
            "condition_mrr": c[f"mrr_at_{headline}"],
            "delta_mrr": d_mrr,
            "baseline_first_relevant_rank": b["first_relevant_rank"],
            "condition_first_relevant_rank": c["first_relevant_rank"],
            "baseline_top1_id": b["top1_id"],
            "condition_top1_id": c["top1_id"],
            "baseline_recall_at_1": b["recall_at_1"],
            "condition_recall_at_1": c["recall_at_1"],
            "baseline_recall_at_5": b["recall_at_5"],
            "condition_recall_at_5": c["recall_at_5"],
        }
        if d_mrr > 1e-9:
            improved.append(entry)
        elif d_mrr < -1e-9:
            regressed.append(entry)
        else:
            unchanged += 1
            continue
        deltas.append(entry)

    # Aggregate deltas, keyed by depth then by metric. The depth is
    # the dict key and the metric names carry no depth of their own,
    # so no reader has to parse a depth out of a metric name.
    aggregate_delta: dict[str, Any] = {}
    for k in depths:
        base_aggs = baseline["aggregates_by_depth"][str(k)]["aggregates"]
        cond_aggs = condition["aggregates_by_depth"][str(k)]["aggregates"]
        aggregate_delta[str(k)] = {
            metric: (
                cond_aggs.get(f"{metric}_at_k", 0.0)
                - base_aggs.get(f"{metric}_at_k", 0.0)
            )
            for metric in ("mean_precision", "mean_recall", "mean_mrr", "mean_ndcg")
        }

    # Per-split delta (the corpus's own calibration / held-out split).
    split_delta: dict[str, Any] = {}
    base_splits = baseline.get("by_split") or {}
    cond_splits = condition.get("by_split") or {}
    for split in sorted(set(base_splits) & set(cond_splits)):
        entry: dict[str, Any] = {
            "n_positive": base_splits[split].get("n", 0),
            "n_total": base_splits[split].get("n_total", 0),
        }
        for k in depths:
            entry[f"delta_mean_mrr_at_{k}"] = (
                cond_splits[split].get(f"mean_mrr_at_{k}", 0.0)
                - base_splits[split].get(f"mean_mrr_at_{k}", 0.0)
            )
            entry[f"delta_mean_recall_at_{k}"] = (
                cond_splits[split].get(f"mean_recall_at_{k}", 0.0)
                - base_splits[split].get(f"mean_recall_at_{k}", 0.0)
            )
        split_delta[split] = entry

    return {
        "baseline_condition": baseline_key,
        "condition": condition_key,
        "headline_depth": int(headline),
        "aggregate_delta": aggregate_delta,
        "split_delta": split_delta,
        "per_query_summary": {
            "improved": len(improved),
            "regressed": len(regressed),
            "unchanged": unchanged,
            "total": len(improved) + len(regressed) + unchanged,
        },
        "improved_queries": improved,
        "regressed_queries": regressed,
        "changed_queries": deltas,
    }


def compare_rankings(
    left: dict[str, Any],
    right: dict[str, Any],
    left_name: str,
    right_name: str,
    depth: int,
) -> dict[str, Any]:
    """Per-query agreement between two rankings of the same corpus.

    The raw-cosine arm measures the vector channel; MPM's production
    ranker blends that channel with FTS5 keyword scoring. Comparing
    the two per query is what licenses reading the raw-cosine numbers
    as a statement about MPM's ranker rather than about one channel
    in isolation.
    """
    lrows = {r["query_id"]: r for r in left["rows"]}
    rrows = {r["query_id"]: r for r in right["rows"]}
    top1_diff: list[str] = []
    rank_diff: list[str] = []
    mrr_diff: list[str] = []
    for qid, l in lrows.items():
        r = rrows.get(qid)
        if r is None:
            continue
        if l["top1_id"] != r["top1_id"]:
            top1_diff.append(qid)
        if l["first_relevant_rank"] != r["first_relevant_rank"]:
            rank_diff.append(qid)
        if abs(l[f"mrr_at_{depth}"] - r[f"mrr_at_{depth}"]) > 1e-9:
            mrr_diff.append(qid)
    total = len(lrows)
    return {
        "left": left_name,
        "right": right_name,
        "depth": depth,
        "queries_compared": total,
        "differing_top1": len(top1_diff),
        "differing_first_relevant_rank": len(rank_diff),
        "differing_mrr": len(mrr_diff),
        "identical": (
            not top1_diff and not rank_diff and not mrr_diff
        ),
        "differing_query_ids": sorted(set(top1_diff) | set(rank_diff) | set(mrr_diff)),
    }


def _run_raw_cosine(
    corpus: Corpus,
    workspace: DocumentWorkspace,
    profile: dict[str, Any],
    query_prefix: str,
    top_k: int,
) -> list[QueryResult]:
    """Raw-cosine ranking for one condition.

    The query embedding goes to the same provider that produced the
    document vectors, with the condition's query prefix applied
    exactly once. The document vectors are read from the condition's
    own workspace, so no cross-condition mixing is possible.
    """
    return [
        run_query(
            q,
            workspace.snap,
            _resolve_mpm_binary(),
            raw_cosine=True,
            raw_cosine_prefix=query_prefix,
            embedding_profile=profile,
            top_k=top_k,
            collection=corpus_collection(),
        )
        for q in corpus.queries
    ]


def run_prefix_experiment(args: Any) -> int:
    """Run the full condition matrix and write the report.

    This is the CLI entry point. It turns the experiment's refusals
    and hard failures into a nonzero exit with a clear message, and
    it guarantees that a failed run publishes NO report: a partial
    result written out as a successful measurement is worse than no
    measurement, because the next reader cannot tell them apart.
    """
    try:
        return _run_prefix_experiment(args)
    except LogicalMutationError as exc:
        print(f"FAILED: {exc}", file=sys.stderr)
        return 1
    except PrefixExperimentError as exc:
        print(f"REFUSING: {exc}", file=sys.stderr)
        return 2


def _run_prefix_experiment(args: Any) -> int:
    """Run the full condition matrix and write the report."""
    if not getattr(args, "output_dir", None):
        print("--output-dir is required", file=sys.stderr)
        return 2
    if not getattr(args, "config", None):
        print("--config is required", file=sys.stderr)
        return 2

    corpus = load_corpus(
        DEFAULT_CORPUS_DIR,
        version=getattr(args, "corpus_version", None),
    )
    _assert_no_double_prefix(corpus)
    config = json.loads(Path(args.config).read_text())

    profile = _build_raw_cosine_profile(config)
    if profile is None:
        print(
            "REFUSING: no embedding profile resolved from the config. The "
            "prefix experiment must embed with a real provider; a hash "
            "fallback is not a measurement.",
            file=sys.stderr,
        )
        return 1

    wanted = _parse_conditions(getattr(args, "conditions", None))
    selected = [c for c in CONDITIONS if c.key in wanted]
    if not selected:
        print("REFUSING: no conditions selected", file=sys.stderr)
        return 2

    # Condition A is the baseline every comparison is measured
    # against, so a selection without it is refused BEFORE any
    # workspace is created -- an invalid selection costs nothing.
    _validate_selection(wanted)

    depths = sorted(set(FIXED_RECALL_DEPTHS) | {corpus.scoring.top_k})
    top_k = corpus.scoring.top_k

    # One workspace per DOCUMENT preparation. Conditions that need
    # different document embeddings therefore never share vectors.
    # The preparation key is DERIVED from the document prefix, so it
    # cannot be paired with the wrong text.
    workspaces: dict[str, DocumentWorkspace] = {}
    for doc_prefix in dict.fromkeys(c.document_prefix for c in selected):
        prep = _document_preparation(doc_prefix)
        print(f"[prepare] document preparation {prep!r} (prefix={doc_prefix!r})")
        ws = prepare_document_workspace(doc_prefix, corpus, config)
        if ws.preparation in workspaces:
            raise PrefixExperimentError(
                f"document preparation {ws.preparation!r} is claimed by two "
                "different document prefixes; refusing to merge workspaces"
            )
        workspaces[ws.preparation] = ws
        print(
            f"[prepare] fingerprint verification: "
            f"{'ok' if ws.verification.ok else 'FAILED'}"
        )
        for f in ws.verification.failures:
            print(f"  - {f}")

    # The output dir must be validated against EVERY workspace (it
    # may not live inside any of them) and against the live-store
    # sentinels. The first call creates it with 0o700; the later
    # calls re-verify the permissions and the realpath.
    output_dir = Path(args.output_dir).resolve()
    out_real = None
    for ws in workspaces.values():
        out_real = assert_safe_output_dir(output_dir, ws.snap.resolve())
    assert out_real is not None
    bad_out = reject_unsafe_symlinks(out_real)
    if bad_out:
        print(f"REFUSING: unsafe symlinks in output dir: {bad_out}", file=sys.stderr)
        return 1

    # Hybrid arm: the production MPM ranker, on the unprefixed
    # workspace only. The production recall path embeds the query
    # verbatim, so no prefix condition is expressible through it.
    # Condition A is required (see `_validate_selection`), so the
    # unprefixed workspace always exists and this arm always runs.
    hybrid = _run_hybrid_arm(corpus, workspaces[BASELINE_PREPARATION], depths)

    condition_reports: list[dict[str, Any]] = []
    for cond in selected:
        ws = workspaces[cond.document_preparation]
        print(f"[condition {cond.key}] {cond.name}")
        results = _run_raw_cosine(
            corpus, ws, profile, cond.query_prefix, top_k
        )
        scored = score_condition(corpus, results, depths)
        condition_reports.append(
            {
                "condition": cond.to_dict(),
                "workspace": {
                    "preparation": ws.preparation,
                    "document_prefix": ws.document_prefix,
                    "snapshot_workspace": str(ws.snap),
                    "db_path": str(ws.db_path),
                    "backfill_rc": ws.backfill_rc,
                    "backfill_output_tail": ws.backfill_output[-600:],
                    "fingerprint_verification": ws.verification.to_dict(),
                    "fingerprint_groups": {
                        k: [list(g) for g in v] for k, v in ws.fingerprint.items()
                    },
                },
                "scoring": scored,
            }
        )

    by_key = {cr["condition"]["key"]: cr for cr in condition_reports}
    comparisons: dict[str, Any] = {}
    # The baseline is condition A and only condition A. There is no
    # fallback: `_validate_selection` guaranteed it is present, and
    # substituting a prefixed condition for it would silently measure
    # the wrong thing.
    baseline_key = BASELINE_CONDITION_KEY
    if baseline_key not in by_key:
        raise PrefixExperimentError(
            f"baseline condition {baseline_key} is missing from the "
            f"completed conditions {sorted(by_key)}; refusing to compare "
            f"against anything else"
        )
    for cr in condition_reports:
        key = cr["condition"]["key"]
        if key == baseline_key:
            continue
        comparisons[key] = compare_to_baseline(
            by_key[baseline_key]["scoring"],
            cr["scoring"],
            baseline_key,
            key,
            depths,
        )

    # Does the raw-cosine arm stand in for MPM's production ranker?
    # Only if the two agree per query on the unprefixed workspace.
    # The hybrid arm always runs (condition A is required), so this
    # comparison is always available.
    hybrid_agreement: dict[str, Any] = compare_rankings(
        by_key[baseline_key]["scoring"],
        hybrid["scoring"],
        f"raw-cosine condition {baseline_key}",
        "MPM production hybrid ranker",
        top_k,
    )

    model_meta = provider_model_metadata(profile)
    manifest = json.loads(corpus.manifest_path.read_text())
    report = {
        "experiment": "MPM Issue #23 - Nomic instruction-prefix calibration",
        "generated_by": "bench.prefix_experiment",
        "metric_definitions": METRIC_DEFINITIONS,
        "reproduction": {
            "command": (
                "python3 -m bench prefix-experiment "
                f"--output-dir {output_dir} "
                f"--config {args.config} "
                f"--corpus-version {getattr(args, 'corpus_version', None) or 'v1'} "
                f"--conditions {','.join(c.key for c in selected)}"
            ),
            "collection_override": "MPM_BENCH_COLLECTION=" + corpus_collection(),
            "note": (
                "The corpus version, the conditions and the config file are "
                "all recorded above; the same invocation reproduces this run."
            ),
        },
        "corpus": {
            "version": corpus.version,
            "manifest_path": str(corpus.manifest_path),
            "manifest_sha256": hashlib.sha256(
                corpus.manifest_path.read_bytes()
            ).hexdigest(),
            "generator": manifest.get("generator"),
            "content_hashes": {
                key: value
                for key, value in manifest.items()
                if key.endswith("_sha256")
            },
            "documents_count": len(corpus.documents),
            "queries_count": len(corpus.queries),
            "positive_queries_count": len(corpus.positive_queries),
            "negative_queries_count": len(corpus.negative_queries),
            "paraphrase_queries_count": sum(
                1 for q in corpus.queries if q.paraphrase_of
            ),
            "splits": {
                split: sum(1 for q in corpus.queries if q.split == split)
                for split in sorted({q.split for q in corpus.queries if q.split})
            },
            "topic_families": sorted({
                q.topic_family for q in corpus.queries if q.topic_family
            }),
            "scoring_top_k": corpus.scoring.top_k,
            "scoring_parameters": corpus.scoring.parameters,
            "evaluation_depths": depths,
        },
        "model": {
            "profile": profile,
            "expected_model_key": (config.get("expected") or {}).get("model"),
            "expected_dimension": (config.get("expected") or {}).get("dimension"),
            "provider_metadata": model_meta,
        },
        "binary_provenance": _binary_provenance(_resolve_mpm_binary()),
        "isolation": {
            "bench_collection": corpus_collection(),
            "workspaces": {
                prep: str(ws.snap) for prep, ws in workspaces.items()
            },
            "output_dir": str(out_real),
            "live_store_touched": False,
            "note": (
                "Every workspace is a disposable private temp dir with "
                "MPM_WORKSPACE and HOME pointed at it; the harness never "
                "falls back to the operator's installed binary or live DB."
            ),
        },
        "conditions": condition_reports,
        "comparisons_vs_baseline": comparisons,
        "hybrid_arm": hybrid,
        "hybrid_vs_raw_cosine_agreement": hybrid_agreement,
        "limitations": [
            "The corpus is a 50-document / 60-query hand-authored synthetic "
            "set about MPM's own internals. Its scores are NOT comparable "
            "with the historical MRR 0.832, which was measured on a "
            "different (lost) corpus with a different (lost) harness.",
            "The raw-cosine arm is not the MPM hybrid ranker. MPM blends "
            "FTS5 keyword scoring with the vector channel; the raw-cosine "
            "arm measures the vector channel alone.",
            "The hybrid arm can only be measured in the unprefixed "
            "condition: MPM's recall path embeds the query verbatim, so a "
            "prefixed condition would require a production change.",
            "In the document-prefixed conditions the corpus row's stored "
            "text carries the prefix (that is how the benchmark makes "
            "MPM's production embed path produce a prefixed vector). The "
            "stored text therefore also differs from production, which is "
            "why no hybrid measurement is taken in those workspaces.",
            "Single model, single corpus, single run: no confidence "
            "intervals are computed. The per-query improved/regressed "
            "counts are the honest uncertainty signal.",
            "The raw-cosine channel has no abstention path: a negative "
            "query always receives a full top-k. The irrelevant / "
            "no-answer behaviour is therefore measured on the MPM hybrid "
            "arm, and the raw-cosine error rate for negatives is "
            "reported as the top-1 similarity a negative query attracts.",
            "A prefix change is a full re-embedding of the corpus in "
            "any future implementation. This experiment measures "
            "retrieval on a 50-document corpus; it does not measure the "
            "cost, the recall of already-stored vectors, or the "
            "behaviour of the live store, none of which are in scope "
            "for Phase A.",
        ],
    }

    recheck_output_dir_safe(out_real, next(iter(workspaces.values())).snap.resolve())
    json_path = out_real / "prefix-experiment-report.json"
    json_path.write_text(json.dumps(report, indent=2))
    md_path = out_real / "prefix-experiment-report.md"
    md_path.write_text(render_markdown(report))
    print(f"REPORT: {md_path}")
    print(f"REPORT: {json_path}")

    all_ok = all(
        cr["workspace"]["fingerprint_verification"]["ok"]
        for cr in condition_reports
    )
    return 0 if all_ok else 1


def _parse_conditions(spec: str | None) -> set[str]:
    if not spec:
        return {c.key for c in CONDITIONS}
    keys = {k.strip().upper() for k in spec.split(",") if k.strip()}
    unknown = keys - set(CONDITIONS_BY_KEY)
    if unknown:
        raise PrefixExperimentError(
            f"unknown condition(s): {sorted(unknown)}; "
            f"known: {sorted(CONDITIONS_BY_KEY)}"
        )
    return keys


def _validate_selection(wanted: set[str]) -> None:
    """Refuse a condition selection that has no baseline.

    Condition A (`baseline-unprefixed`) is the ONLY baseline this
    experiment has. Every comparison it reports is against the
    same-corpus unprefixed baseline, so a selection that omits A
    cannot produce a comparison at all.

    B, C and D are not baselines. They are prefixed conditions, and
    silently promoting one of them to "the baseline" would publish a
    number whose reference point is a prefix rather than the
    unprefixed text -- exactly the kind of unanchored figure this
    experiment exists to replace.

    There is deliberately no "no-baseline mode". A run without a
    baseline would produce per-condition numbers with nothing to say
    what they are relative to, and would have to be read as a
    comparison anyway.
    """
    if BASELINE_CONDITION_KEY in wanted:
        return
    raise PrefixExperimentError(
        f"condition {BASELINE_CONDITION_KEY} (baseline-unprefixed) is "
        f"required: every comparison in this experiment is against the "
        f"same-corpus unprefixed baseline, and the selection "
        f"{sorted(wanted)} does not include it. Add "
        f"{BASELINE_CONDITION_KEY} to --conditions. There is no "
        f"no-baseline mode -- a comparison without a baseline is not a "
        f"measurement, and B/C/D are prefixed conditions, not baselines."
    )


def _run_hybrid_arm(
    corpus: Corpus,
    workspace: DocumentWorkspace,
    depths: Sequence[int],
) -> dict[str, Any]:
    """Run MPM's production hybrid ranker on the unprefixed workspace.

    This is the production reference point: MPM's own ranker, MPM's
    own query embedding, MPM's own FTS5 blend. It is measured under
    the same mutation checking the rest of the harness uses.

    A detected logical mutation RAISES. The arm does not return a
    partial result: the measurements it had accumulated up to that
    point were produced against a workspace whose state had changed
    underneath it, so they cannot be compared with anything. The
    caller turns the exception into a nonzero exit and no report is
    written.
    """
    before = _snapshot_full(workspace.db_path)
    results: list[QueryResult] = []
    mutation: dict[str, Any] = {"status": "clean", "diffs": []}
    for q in corpus.queries:
        r = run_query(
            q,
            workspace.snap,
            _resolve_mpm_binary(),
            top_k=corpus.scoring.top_k,
            collection=corpus_collection(),
        )
        after = _snapshot_full(workspace.db_path)
        d = _mutation_diff(before, after)
        if d["status"] != "clean":
            print(
                f"MUTATION DETECTED after hybrid query {q.query_id}: "
                f"{d['diffs']}",
                file=sys.stderr,
            )
            raise LogicalMutationError(
                f"logical mutation detected in the hybrid arm after query "
                f"{q.query_id}: {d['diffs']}. The measurement's premise is "
                f"broken, so the experiment fails instead of publishing a "
                f"partial result."
            )
        results.append(r)

    scored = score_condition(corpus, results, depths)
    empty = [r.query_id for r in results if r.search_mode == "hybrid_empty"]
    return {
        "description": (
            "MPM's production hybrid ranker (FTS5 + vector blend) on the "
            "unprefixed workspace. This is the production configuration; no "
            "prefix condition is expressible through it without a "
            "production change (internal/core/hybrid_search.go embeds the "
            "query verbatim)."
        ),
        "workspace": str(workspace.snap),
        "snapshot_logical_mutation": mutation,
        "negative_queries_returning_empty": empty,
        "scoring": scored,
    }


def render_markdown(report: dict[str, Any]) -> str:
    """Render the human-readable report."""
    lines: list[str] = []
    add = lines.append
    add("# Nomic instruction-prefix condition matrix (Issue #23 Phase A)")
    add("")
    add(f"- Corpus: `{report['corpus']['version']}` — "
        f"{report['corpus']['documents_count']} documents, "
        f"{report['corpus']['queries_count']} queries "
        f"({report['corpus']['positive_queries_count']} positive / "
        f"{report['corpus']['negative_queries_count']} negative), "
        f"{report['corpus']['paraphrase_queries_count']} paraphrases")
    add(f"- Evaluation depths: `{report['corpus']['evaluation_depths']}`")
    model = report["model"]
    add(f"- Model: `{model['profile'].get('model')}` via "
        f"`{model['profile'].get('provider')}` at "
        f"`{model['profile'].get('base_url')}`")
    add(f"- Fingerprint key: `{model['expected_model_key']}`, dimension "
        f"`{model['expected_dimension']}`")
    pm = model.get("provider_metadata")
    if pm and pm.get("digest"):
        add(f"- Model digest: `{pm['digest']}`")
    if pm and pm.get("template") is not None:
        add(f"- Provider template: `{pm['template']}`")
    add("")

    # ---- Headline summary -------------------------------------------
    # The one table a reviewer reads first: every condition's headline
    # metrics and its delta against the same-corpus unprefixed
    # baseline. The baseline row carries no delta — it IS the delta
    # reference.
    depths = [str(d) for d in report["corpus"]["evaluation_depths"]]
    headline = max(depths, key=int)
    comparisons = report.get("comparisons_vs_baseline") or {}
    baseline_key = (
        next(iter(comparisons.values()))["baseline_condition"]
        if comparisons
        else None
    )
    add("## Summary")
    add("")
    add(f"| Condition | MRR@{headline} | ΔMRR@{headline} vs "
        f"{baseline_key or 'baseline'} | R@1 | R@5 | nDCG@{headline} | "
        f"Improved / regressed queries |")
    add("|---|---|---|---|---|---|---|")
    for cr in report["conditions"]:
        key = cr["condition"]["key"]
        aggs = cr["scoring"]["aggregates_by_depth"]
        delta = "—"
        counts = "—"
        if baseline_key and key != baseline_key and key in comparisons:
            cmp = comparisons[key]
            delta = f"{cmp['aggregate_delta'][headline].get('mean_mrr', 0.0):+.4f}"
            s = cmp["per_query_summary"]
            counts = f"{s['improved']} / {s['regressed']}"
        add(
            f"| {key} ({cr['condition']['name']}) "
            f"| {_aggregate(aggs, headline, 'mean_mrr_at_k'):.4f} | {delta} "
            f"| {_aggregate(aggs, '1', 'mean_recall_at_k'):.4f} "
            f"| {_aggregate(aggs, '5', 'mean_recall_at_k'):.4f} "
            f"| {_aggregate(aggs, headline, 'mean_ndcg_at_k'):.4f} | {counts} |"
        )
    add("")
    add("All deltas are against condition "
        f"{baseline_key or 'A'} of THIS run, on THIS corpus. No number in "
        "this report is comparable with the historical MRR 0.832, which "
        "was measured on a different (lost) corpus with a different "
        "(lost) harness. See `Limitations`.")
    add("")
    add("## Condition matrix")
    add("")
    add("| Condition | Document prefix | Query prefix | Workspace | Fingerprint |")
    add("|---|---|---|---|---|")
    for cr in report["conditions"]:
        c = cr["condition"]
        fp = cr["workspace"]["fingerprint_verification"]
        add(
            f"| {c['key']} ({c['name']}) | `{c['document_prefix'] or '(none)'}` "
            f"| `{c['query_prefix'] or '(none)'}` "
            f"| `{cr['workspace']['preparation']}` "
            f"| {'ok' if fp['ok'] else 'FAILED'} |"
        )
    add("")

    add("## Raw-cosine arm (vector channel only) — aggregates by depth")
    add("")
    add(f"All means are over the {report['corpus']['positive_queries_count']} "
        "positive queries; negative queries have no graded relevance and are "
        "reported separately. `MRR@k` is the reciprocal rank of the first "
        "relevant document **within the top k** (0 if none is), so MRR@1 is "
        "the hit-rate and MRR@5 is 0 for any query whose first relevant "
        "document ranks 6th or lower. MRR@10 over a 10-row corpus is the "
        "unbounded MRR.")
    add("")
    for cr in report["conditions"]:
        c = cr["condition"]
        add(f"### Condition {c['key']} — {c['name']}")
        add("")
        add("| Depth | P@k | R@k | MRR@k | nDCG@k |")
        add("|---|---|---|---|---|")
        for k, agg in cr["scoring"]["aggregates_by_depth"].items():
            add(
                f"| {k} | {agg['aggregates']['mean_precision_at_k']:.4f} "
                f"| {agg['aggregates']['mean_recall_at_k']:.4f} "
                f"| {agg['aggregates']['mean_mrr_at_k']:.4f} "
                f"| {agg['aggregates']['mean_ndcg_at_k']:.4f} |"
            )
        add("")
        para = cr["scoring"]["paraphrase"]
        add(f"- Paraphrase subset (n={para['n']}): "
            f"MRR@10 {para.get('mean_mrr_at_10', 0.0):.4f}, "
            f"R@1 {para.get('mean_recall_at_1', 0.0):.4f}, "
            f"R@5 {para.get('mean_recall_at_5', 0.0):.4f}")
        for split, m in (cr["scoring"].get("by_split") or {}).items():
            add(f"- Split `{split}` (n={m['n']} positive of "
                f"{m.get('n_total', m['n'])} queries): "
                f"MRR@10 {m.get('mean_mrr_at_10', 0.0):.4f}, "
                f"R@1 {m.get('mean_recall_at_1', 0.0):.4f}, "
                f"R@5 {m.get('mean_recall_at_5', 0.0):.4f}")
        neg = cr["scoring"]["negative"]
        add(f"- Negative queries (n={neg['n']}): mean top-1 similarity "
            f"{neg['mean_top1_similarity']:.4f} "
            f"(median {neg['median_top1_similarity']:.4f}, "
            f"max {neg['max_top1_similarity']:.4f})")
        sep = cr["scoring"]["score_separation"]
        add(f"- Score separation (mean top-1): positive "
            f"{sep['mean_top1_similarity_positive']:.4f} vs negative "
            f"{sep['mean_top1_similarity_negative']:.4f}")
        add("- Irrelevant / no-answer behaviour: the raw-cosine channel "
            "has no abstention path — a negative query always returns a "
            "full top-k, so its error rate is the top-1 similarity it "
            "attracts, not a refusal. The abstention behaviour is "
            "measured on the MPM hybrid arm below.")
        add("")

    if report.get("comparisons_vs_baseline"):
        add("## Comparison against the same-corpus unprefixed baseline")
        add("")
        for key, cmp in report["comparisons_vs_baseline"].items():
            add(f"### {key} vs {cmp['baseline_condition']}")
            add("")
            delta_depths = sorted(
                cmp["aggregate_delta"], key=int
            )
            add("| Metric | " + " | ".join(f"@{d}" for d in delta_depths) + " |")
            add("|---" * (1 + len(delta_depths)) + "|")
            for metric in ("mean_precision", "mean_recall", "mean_mrr",
                           "mean_ndcg"):
                cells = " | ".join(
                    f"{cmp['aggregate_delta'][d].get(metric, 0.0):+.4f}"
                    for d in delta_depths
                )
                add(f"| {metric} | {cells} |")
            s = cmp["per_query_summary"]
            add("")
            add(f"- Per-query MRR: {s['improved']} improved, "
                f"{s['regressed']} regressed, {s['unchanged']} unchanged "
                f"(of {s['total']})")
            if cmp.get("split_delta"):
                add("- Per-split MRR delta:")
                for split, sd in cmp["split_delta"].items():
                    add(f"  - `{split}` (n={sd['n_positive']} positive): "
                        f"MRR {sd.get('delta_mean_mrr_at_10', 0.0):+.4f}, "
                        f"R@1 {sd.get('delta_mean_recall_at_1', 0.0):+.4f}, "
                        f"R@5 {sd.get('delta_mean_recall_at_5', 0.0):+.4f}")
            if cmp["regressed_queries"]:
                add("- Regressed queries:")
                for r in cmp["regressed_queries"]:
                    add(f"  - `{r['query_id']}` "
                        f"({r['class']}, split={r['split']}): "
                        f"MRR {r['baseline_mrr']:.4f} -> "
                        f"{r['condition_mrr']:.4f} "
                        f"(first-relevant rank "
                        f"{_rank_display(r['baseline_first_relevant_rank'])} "
                        f"-> "
                        f"{_rank_display(r['condition_first_relevant_rank'])})")
            if cmp["improved_queries"]:
                add("- Improved queries:")
                for r in cmp["improved_queries"]:
                    add(f"  - `{r['query_id']}` "
                        f"({r['class']}, split={r['split']}): "
                        f"MRR {r['baseline_mrr']:.4f} -> "
                        f"{r['condition_mrr']:.4f}")
            add("")

    hybrid = report.get("hybrid_arm")
    if hybrid:
        add("## MPM hybrid-retrieval arm (production ranker, unprefixed)")
        add("")
        add(f"- {hybrid['description']}")
        add(f"- Snapshot logical mutation: "
            f"`{hybrid['snapshot_logical_mutation']['status']}`")
        add(f"- Negative queries returning an empty result: "
            f"{len(hybrid['negative_queries_returning_empty'])} / "
            f"{report['corpus']['negative_queries_count']}")
        add("")
        add("| Depth | P@k | R@k | MRR@k | nDCG@k |")
        add("|---|---|---|---|---|")
        for k, agg in hybrid["scoring"]["aggregates_by_depth"].items():
            add(
                f"| {k} | {agg['aggregates']['mean_precision_at_k']:.4f} "
                f"| {agg['aggregates']['mean_recall_at_k']:.4f} "
                f"| {agg['aggregates']['mean_mrr_at_k']:.4f} "
                f"| {agg['aggregates']['mean_ndcg_at_k']:.4f} |"
            )
        add("")
        agree = report.get("hybrid_vs_raw_cosine_agreement")
        if agree:
            add(
                f"- Agreement with the raw-cosine arm over "
                f"{agree['queries_compared']} queries at depth "
                f"{agree['depth']}: "
                f"{agree['differing_top1']} differing top-1 documents, "
                f"{agree['differing_first_relevant_rank']} differing "
                f"first-relevant ranks, "
                f"{agree['differing_mrr']} differing MRRs "
                f"({'identical' if agree['identical'] else 'NOT identical'})."
            )
            add("")

    add("## Limitations")
    add("")
    for lim in report["limitations"]:
        add(f"- {lim}")
    add("")
    add("## Reproduction")
    add("")
    add(f"```bash\n{report['reproduction']['collection_override']} \\\n  "
        f"{report['reproduction']['command']}\n```")
    add("")
    return "\n".join(lines)
