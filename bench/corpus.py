"""Corpus loading and manifest verification (C3).

The corpus is self-contained: documents, queries, expectations and
scoring are bound together in a versioned manifest. Labels are scored
against the corpus's own documents, never against live-memory content.

The historical 30-query benchmark corpus and its hand-labelled
relevance judgments were not preserved as artifacts (the reproduction
sections in `docs/EMBEDDING-MODEL-COMPARISON.md:250-258` and
`docs/EMBEDDING-MODEL-BENCHMARK.md:166-175` describe what was lost).
This corpus is a small synthetic labelled set, not a recovery of the
historical corpus. The manifest's `note` field carries that statement.
"""

from __future__ import annotations

import hashlib
import json
from dataclasses import dataclass
from pathlib import Path
from typing import Any


class CorpusError(RuntimeError):
    """Raised when a corpus fails provenance verification."""


@dataclass(frozen=True)
class Document:
    doc_id: str
    content: str
    relevance_class: str  # "relevant" | "partially-relevant" | "not-relevant"

    @staticmethod
    def from_dict(d: dict[str, Any]) -> "Document":
        for required in ("doc_id", "content", "relevance_class"):
            if required not in d:
                raise CorpusError(f"document missing required field {required!r}: {d}")
        return Document(
            doc_id=d["doc_id"],
            content=d["content"],
            relevance_class=d["relevance_class"],
        )


@dataclass(frozen=True)
class Query:
    query_id: str
    query: str
    relevance_class: str  # "positive" | "negative"
    relevant_doc_ids: tuple[str, ...]

    @property
    def is_positive(self) -> bool:
        return self.relevance_class == "positive"

    @staticmethod
    def from_dict(d: dict[str, Any]) -> "Query":
        for required in ("query_id", "query", "relevance_class", "relevant_doc_ids"):
            if required not in d:
                raise CorpusError(f"query missing required field {required!r}: {d}")
        return Query(
            query_id=d["query_id"],
            query=d["query"],
            relevance_class=d["relevance_class"],
            relevant_doc_ids=tuple(d["relevant_doc_ids"]),
        )


@dataclass(frozen=True)
class Expectations:
    by_query: dict[str, tuple[str, ...]]

    @staticmethod
    def from_dict(d: dict[str, Any]) -> "Expectations":
        return Expectations(
            by_query={k: tuple(v) for k, v in d.items()},
        )


@dataclass(frozen=True)
class Scoring:
    method: str
    top_k: int
    parameters: dict[str, Any]

    @staticmethod
    def from_dict(d: dict[str, Any]) -> "Scoring":
        return Scoring(
            method=d.get("method", "mpm recall --semantic"),
            top_k=int(d.get("top_k", 5)),
            parameters=dict(d.get("parameters", {})),
        )


@dataclass(frozen=True)
class Corpus:
    version: str
    documents: tuple[Document, ...]
    queries: tuple[Query, ...]
    expectations: Expectations
    scoring: Scoring
    manifest_path: Path

    @property
    def positive_queries(self) -> tuple[Query, ...]:
        return tuple(q for q in self.queries if q.is_positive)

    @property
    def negative_queries(self) -> tuple[Query, ...]:
        return tuple(q for q in self.queries if not q.is_positive)

    def doc_by_id(self, doc_id: str) -> Document | None:
        for d in self.documents:
            if d.doc_id == doc_id:
                return d
        return None


def _sha256_bytes(b: bytes) -> str:
    return hashlib.sha256(b).hexdigest()


def _sha256_file(p: Path) -> str:
    return _sha256_bytes(p.read_bytes())


def load_corpus(corpus_dir: Path) -> Corpus:
    """Load the versioned corpus from disk and verify the manifest's
    content hashes match the on-disk files. Refuses to start if any
    file has been modified since the manifest was written.

    The corpus_dir is expected to be `bench/corpus/` and to contain:
      - MANIFEST.json
      - v1/documents.json
      - v1/queries.json
      - v1/expectations.json
      - v1/scoring.json
    """
    if not corpus_dir.is_dir():
        raise CorpusError(f"corpus dir not found: {corpus_dir}")

    manifest_path = corpus_dir / "MANIFEST.json"
    if not manifest_path.is_file():
        raise CorpusError(f"MANIFEST.json not found in {corpus_dir}")

    manifest = json.loads(manifest_path.read_text())
    version = manifest.get("version")
    if not version:
        raise CorpusError(f"MANIFEST.json missing 'version': {manifest_path}")

    version_dir = corpus_dir / version
    if not version_dir.is_dir():
        raise CorpusError(f"corpus version dir not found: {version_dir}")

    files = ("documents.json", "queries.json", "expectations.json", "scoring.json")
    paths: dict[str, Path] = {f: version_dir / f for f in files}
    for name, p in paths.items():
        if not p.is_file():
            raise CorpusError(f"corpus file missing: {p}")

    # Verify content hashes.
    for name, p in paths.items():
        actual = _sha256_file(p)
        expected_key = f"{name.rsplit('.', 1)[0]}_sha256"
        expected = manifest.get(expected_key)
        if expected is None:
            raise CorpusError(
                f"MANIFEST.json missing field {expected_key!r} for {name}"
            )
        if actual != expected:
            raise CorpusError(
                f"corpus file {p} has sha256 {actual} but manifest records {expected}; "
                f"refusing to start (manifest/file binding broken)"
            )

    documents = tuple(
        Document.from_dict(d)
        for d in json.loads(paths["documents.json"].read_text())
    )
    queries = tuple(
        Query.from_dict(q)
        for q in json.loads(paths["queries.json"].read_text())
    )
    expectations = Expectations.from_dict(
        json.loads(paths["expectations.json"].read_text())
    )
    scoring = Scoring.from_dict(json.loads(paths["scoring.json"].read_text()))

    # Cross-check: every relevant_doc_id in queries must exist in documents.
    doc_ids = {d.doc_id for d in documents}
    if len(doc_ids) != len(documents):
        # Duplicate doc_ids in the committed corpus. Refuse to start;
        # a non-unique key would make `INSERT OR REPLACE` collisions
        # silently collapse two distinct documents into one row.
        dupes: list[str] = []
        seen: set[str] = set()
        for d in documents:
            if d.doc_id in seen and d.doc_id not in dupes:
                dupes.append(d.doc_id)
            seen.add(d.doc_id)
        raise CorpusError(
            f"corpus documents.json contains duplicate doc_ids: {dupes}"
        )
    for q in queries:
        for did in q.relevant_doc_ids:
            if did not in doc_ids:
                raise CorpusError(
                    f"query {q.query_id} references unknown doc_id {did!r}"
                )
        # Positive queries must have at least one labelled relevant
        # doc; otherwise Precision/Recall/MRR/nDCG are undefined
        # and the scoring aggregates divide by zero.
        if q.is_positive and not q.relevant_doc_ids:
            raise CorpusError(
                f"positive query {q.query_id} has empty relevant_doc_ids; "
                f"labelled relevance scoring requires a non-empty label set"
            )

    # Cross-check: expectations keys must match query IDs AND the
    # values must be a set-equal to the query's relevant_doc_ids.
    # `queries.json` is the source of truth (it is the contract
    # the ranker is scored against); `expectations.json` is a
    # parallel audit that the corpus is internally consistent.
    expected_keys = {q.query_id for q in queries}
    actual_keys = set(expectations.by_query.keys())
    if expected_keys != actual_keys:
        missing = expected_keys - actual_keys
        extra = actual_keys - expected_keys
        raise CorpusError(
            f"expectations key mismatch: missing={sorted(missing)} extra={sorted(extra)}"
        )
    queries_by_id = {q.query_id: q for q in queries}
    for qid, exp_ids in expectations.by_query.items():
        q = queries_by_id[qid]
        if set(exp_ids) != set(q.relevant_doc_ids):
            raise CorpusError(
                f"expectations[{qid}] {sorted(exp_ids)} != "
                f"queries[{qid}].relevant_doc_ids {sorted(q.relevant_doc_ids)}; "
                f"the two files must be a set-equal parallel record"
            )

    if len(documents) < 30:
        # Soft check; not a hard fail. The design says "N>=30 documents".
        # The synthetic v1 ships 30, but we don't hard-fail if a future
        # version has fewer (the per-query logic still works).
        pass

    return Corpus(
        version=version,
        documents=documents,
        queries=queries,
        expectations=expectations,
        scoring=scoring,
        manifest_path=manifest_path,
    )


def stable_doc_id(content: str) -> str:
    """Derive an 8-character ID from a document's content. This is the
    format that survives MPM's shortID() 8-char truncation. The mapping
    is identity for any ID <= 8 chars; we choose exactly 8 chars to
    guarantee the abbreviated ID equals the corpus ID byte-for-byte.

    See `cmd/mpm/handlers_cascade.go:331-333` for shortID.

    Note: the committed v1 corpus uses sequential literal IDs
    (`d0000001`..`d0000030`) so a human reader can cross-reference
    `relevant_doc_ids` in `queries.json` against the document order
    in `documents.json` without recomputing hashes. The harness
    neither generates nor enforces sequential IDs; the helper
    above is the documented fallback for any future corpus
    version that does not include explicit IDs in the JSON.
    """
    return hashlib.sha256(content.encode("utf-8")).hexdigest()[:8]


def corpus_summary(corpus: Corpus) -> dict[str, Any]:
    """Render a small summary dict for the report."""
    return {
        "version": corpus.version,
        "documents_count": len(corpus.documents),
        "queries_count": len(corpus.queries),
        "positive_queries_count": len(corpus.positive_queries),
        "negative_queries_count": len(corpus.negative_queries),
        "scoring_method": corpus.scoring.method,
        "manifest_path": str(corpus.manifest_path),
    }
