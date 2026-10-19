"""FTS5 / hybrid control set (Phase A.3 V2).

The v2 calibration corpus's natural-language queries are designed
to test semantic recall; many produce vector-only results because
the query text does not match any document's literal tokens
(FTS5's implicit AND semantics require every token to match a
document).

This module is a separate control set: short, exact-token
queries that are designed to produce FTS5 (or hybrid) results.
Each control query has a known target doc_id; the test asserts
that the target doc_id appears in the top-k results AND that at
least one result has source in {"fts5", "hybrid"} (i.e. the
FTS5 channel contributed). The control set is measured
ALONGSIDE the calibration corpus; it is not part of the
calibration scope and does not affect calibration metrics.

The control set is intentionally small (5 queries) so it is a
sanity check, not a calibration artefact. It pins the
following:

  1. FTS5 is compiled and available in the binary (verified
     by the existence of the `memories_fts` shadow table).
  2. The disposable corpus's documents are present in the FTS5
     index (verified by a direct FTS5 MATCH query producing
     the target doc_id).
  3. The hybrid ranker reports source="fts5" or source="hybrid"
     for exact-token queries whose tokens all match at least
     one document.
  4. Natural-language queries whose tokens are too sparse or
     too distant from any document's literal tokens may produce
     zero FTS5 candidates; this is the documented implicit-AND
     behaviour and is NOT a defect.

The control set is hashable and versioned: changes to the
exact-token probes require updating CONTROL_SET_SHA256 and are
caught by the regression test.

Per HAL's V2 directive: "If vector-only output is correct for
v2, retain measurements but classify as vector-only." The
control set is the mechanism that distinguishes "FTS5 channel
works" from "v2 natural-language queries legitimately
missed FTS5".
"""

from __future__ import annotations

import hashlib
import json
from dataclasses import dataclass
from typing import Any


CONTROL_SET_VERSION = "fts5-control-v1"
# Fixed expected SHA-256 of the canonical FTS5 control contract
# (CONTROL_SET_VERSION + FTS5_CONTROL_QUERIES, sorted by query_id).
# This digest is computed by `control_set_canonical_sha256()`; any
# change to a probe's query text or target_doc_ids list will
# produce a different SHA and the regression test will fail. The
# pre-fix version of this constant was a placeholder that was
# never checked against the actual data, so accidental edits to
# FTS5_CONTROL_QUERIES passed silently. The B3 review requires
# the digest to be genuinely fixed (not derived in the same test
# that asserts it) and to cover the actual control-query
# contract including target-document identities.
CONTROL_SET_SHA256 = (
    "0e213f6fe9fa52e9c0e31a760a8ade9eff19aacf209f95d6ff28979d21c8c6be"
)


def control_set_canonical_sha256() -> str:
    """Compute the canonical SHA-256 of the FTS5 control contract.

    The contract covers CONTROL_SET_VERSION and the (query_id,
    query, target_doc_ids) tuple for every probe in
    FTS5_CONTROL_QUERIES. The probes are sorted by query_id
    before hashing so the digest is stable across dict
    insertion order. This function is the canonical source of
    truth for the expected CONTROL_SET_SHA256; if
    FTS5_CONTROL_QUERIES changes legitimately, both this
    function's output and the constant CONTROL_SET_SHA256 must
    be updated together.
    """
    payload = {
        "version": CONTROL_SET_VERSION,
        "queries": [
            {
                "query_id": q.query_id,
                "query": q.query,
                "target_doc_ids": list(q.target_doc_ids),
            }
            for q in sorted(FTS5_CONTROL_QUERIES, key=lambda x: x.query_id)
        ],
    }
    encoded = json.dumps(payload, sort_keys=True).encode("utf-8")
    return hashlib.sha256(encoded).hexdigest()


@dataclass(frozen=True)
class FTS5ControlQuery:
    """One exact-token FTS5 probe.

    `target_doc_ids` are the document IDs the FTS5 channel
    should surface. They are derived from the v2 corpus's
    documents.json (verified at corpus build time); a mismatch
    at probe time is a corpus integrity issue, not a control
    defect.
    """
    query_id: str
    query: str
    target_doc_ids: tuple[str, ...]
    rationale: str


# The 5 exact-token probes. Each query is a single distinctive
# token that appears verbatim in one or more v2 documents.
# Token selection rationale: each token is a MPM-specific
# identifier that does not appear in any natural-language
# query, so the FTS5 channel has a clean signal.
FTS5_CONTROL_QUERIES: tuple[FTS5ControlQuery, ...] = (
    FTS5ControlQuery(
        query_id="fc-01",
        query="FTS5",
        target_doc_ids=("d02001", "d02003", "d02006", "d02010",
                          "d02011", "d02014", "d02015", "d02016",
                          "d02021", "d02022", "d02024", "d02035",
                          "d02037"),
        rationale=(
            "FTS5 appears verbatim in many retrieval_core and "
            "schema_persistence documents. FTS5 should surface "
            "those docs via the lexical channel."
        ),
    ),
    FTS5ControlQuery(
        query_id="fc-02",
        query="BM25",
        target_doc_ids=("d02001", "d02006"),
        rationale=(
            "BM25 is a distinctive MPM-specific term. d02001 "
            "documents the BM25 algorithm and d02006 documents "
            "the combined score formula that includes BM25."
        ),
    ),
    FTS5ControlQuery(
        query_id="fc-03",
        query="VectorWeight",
        target_doc_ids=("d02003", "d02006"),
        rationale=(
            "VectorWeight is a MPM config field; d02003 and "
            "d02006 are the canonical documents that mention it."
        ),
    ),
    FTS5ControlQuery(
        query_id="fc-04",
        query="ULID",
        target_doc_ids=("d02005", "d02013"),
        rationale=(
            "ULID appears in d02005 (shortID truncation) and "
            "d02013 (memory insertion). Both are about MPM's "
            "ID format."
        ),
    ),
    FTS5ControlQuery(
        query_id="fc-05",
        query="WAL",
        target_doc_ids=("d02033",),
        rationale=(
            "WAL is mentioned in d02033 (SQLite WAL mode). A "
            "single-target probe tests that even narrow single-"
            "token queries surface the right document."
        ),
    ),
)


def fts5_control_corpus_manifest() -> dict[str, Any]:
    """Return a serialisable manifest for the FTS5 control set.

    The manifest is content-addressed: any change to
    FTS5_CONTROL_QUERIES produces a different SHA-256, so the
    regression test catches accidental edits.

    B3 review (HAL 2026-10-19): the expected_sha256 here is
    the same fixed digest as CONTROL_SET_SHA256, derived from
    `control_set_canonical_sha256()`. The manifest's
    `expected_sha256` field and the module-level
    `CONTROL_SET_SHA256` constant must agree, AND both must
    agree with the actual content. The pre-fix code derived
    the manifest's hash from the same in-memory list it was
    testing, so accidental edits would have compared equal to
    themselves and passed silently.
    """
    return {
        "version": CONTROL_SET_VERSION,
        "expected_sha256": CONTROL_SET_SHA256,
        "n_queries": len(FTS5_CONTROL_QUERIES),
        "queries": [
            {
                "query_id": q.query_id,
                "query": q.query,
                "target_doc_ids": list(q.target_doc_ids),
                "rationale": q.rationale,
            }
            for q in FTS5_CONTROL_QUERIES
        ],
    }
