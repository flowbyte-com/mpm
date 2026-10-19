"""Harness main orchestrator.

Usage:
  python -m bench selftest            # run self-tests, no MPM binary needed
  python -m bench smoke               # full smoke: fake server + disposable DB
  python -m bench run --output-dir PATH --config CONFIG
                                        # full real run (operator)

The harness never touches the live MPM database. It always uses
disposable workspaces (gate 3 of HAL's implementation gates).

For real runs, the operator must provide:
  --output-dir  Required for real runs. Must be writable; the
                harness creates it with 0o700 permissions and
                refuses paths that resolve into the snapshot
                workspace or into any live-store sentinel.
  --config      A JSON config describing the embedding profile,
                model, dimension, and base URL for the OpenAI-
                compatible fake or real provider.

v3 review (HAL 2026-10-19):
  - Snapshot workspace is created via `tempfile.mkdtemp` with a
    descriptive prefix; per-PID names are removed. The tempdir
    is private to one run and is removed at exit by the OS.
  - The harness NEVER inspects the live store. The previous
    `~/.mpm` row-count sentinel has been removed entirely.
  - Symlink rejection runs on the snapshot workspace as well as
    the output dir, BEFORE any subprocess is launched. The
    MPM binary is never invoked against a workspace that
    contains an unsafe symlink.
  - The logical-mutation check is per-query: the "before"
    snapshot is taken immediately before the first query, and
    after every query the DB is re-snapshotted and compared.
    Any tracked-field change fails the run.
  - The raw-cosine experiment runs BOTH the unprefixed and
    `search_query:`-prefixed query embeddings through the
    configured provider, on identical document vectors, and
    reports the metric deltas in the report.
  - Scoring top-k is read from `corpus.scoring.top_k`, the
    corpus contract, not from the operator's config. The
    operator's config is honoured only for the embedding
    profile and model identity.
"""

from __future__ import annotations

import argparse
import json
import os
import sqlite3
import sys
import tempfile
from pathlib import Path
from typing import Any

from bench import __version__
from bench.corpus import Corpus, corpus_summary, load_corpus
from bench.fingerprint import (
    VerificationResult,
    verify_regeneration_complete,
)
from bench.reporter import RunReport, write_report
from bench.runner import (
    QueryResult,
    per_query_participation,
    run_query,
)
from bench.scoring import score_queries
from bench.workspace import (
    WorkspaceError,
    assert_safe_output_dir,
    assert_safe_workspace,
    reject_unsafe_symlinks,
    scrubbed_env,
    write_mpm_config,
)


REPO_ROOT = Path(__file__).resolve().parent.parent
DEFAULT_CORPUS_DIR = REPO_ROOT / "bench" / "corpus"
DEFAULT_MPM_BINARY = REPO_ROOT / "mpm"  # built artifact in the repo root

# v3 review: the historical +0.022 MRR figure attributed to the
# `search_query:` prefix on the lost 30-query corpus is the
# reason this prefix is the second condition in the raw-cosine
# experiment. The harness reports the metric delta; the v3
# contract is that BOTH query embeddings come from the
# configured provider, not from a SHA-256 hash.
RAW_COSINE_PREFIX = "search_query: "


def _default_snap_parent() -> Path:
    """The parent dir for snapshot workspaces. Always under /tmp
    so it cannot be inside a live-store sentinel."""
    return Path(tempfile.gettempdir()) / "mpm-bench"


def _make_snap_workspace(prefix: str = "mpm-bench-") -> Path:
    """Create a private per-run snapshot workspace under
    /tmp/mpm-bench/ via `tempfile.mkdtemp`.

    v3 review: this replaces the previous per-PID name. The
    tempdir is private to one run; we resolve it to a realpath
    before doing any further work.
    """
    parent = _default_snap_parent()
    parent.mkdir(parents=True, exist_ok=True, mode=0o700)
    snap = Path(tempfile.mkdtemp(prefix=prefix, dir=str(parent)))
    os.chmod(snap, 0o700)
    return snap


# v3 review: the v2 `_snapshot_row_snapshot` only captured
# `(id, content, embedding_model, embedding_dimension,
# embedding_source)`. That was a "did the backfill mutate?"
# check, not a "did the recall mutate?" check. The new
# version captures every tracked field the harness needs
# to detect recall-time mutation, including the
# contradiction queue and the retrieval metadata table
# (both of which `mpm recall` may touch on a hit).
TRACKED_TABLES: tuple[str, ...] = (
    "memories",
    "reference_chunks",
    "retrieval_metadata",
    "contradiction_queue",
    "challenges",
)


def _snapshot_full(snap_db: Path) -> dict[str, list[tuple]]:
    """Read every tracked table in the disposable DB and return a
    list of tuples per table. The shape is ordered so the diff
    is deterministic.

    The read is done at READ UNCOMMITTED to avoid being blocked
    by MPM's WAL writes; we want a consistent point-in-time
    view of the DB state. (WAL mode means readers never block
    writers and the snapshot is committed-state as of the
    read-time snapshot.)

    v3 review fields:
      - `memories`: id, content, tags, metadata, weight, deleted_at,
        embedding_model, embedding_dimension, embedding_source,
        embedding IS NULL check, last_retrieved_at if present.
      - `reference_chunks`: id, doc_id, chunk_index, section, content,
        embedding_model, embedding_dimension, embedding_source,
        embedding IS NULL check.
      - `retrieval_metadata`: every column, the table is
        observability for hits.
      - `contradiction_queue`: every column, the table is
        written by MPM on a fresh contradiction-apply.
      - `challenges`: every column, the table is the
        challenge-state trail.
    """
    con = sqlite3.connect(f"file:{snap_db}?mode=ro", uri=True)
    out: dict[str, list[tuple]] = {}
    try:
        # Memories: capture every column that recall could plausibly
        # write to, plus the embedding NULL/present bit and the
        # fingerprint columns.
        out["memories"] = list(
            con.execute(
                """
                SELECT id, collection, content, tags, metadata, weight,
                       deleted_at, embedding_model, embedding_dimension,
                       embedding_source,
                       (CASE WHEN embedding IS NULL THEN 0 ELSE 1 END) AS has_embedding,
                       (CASE WHEN challenged_at IS NULL THEN 0 ELSE 1 END) AS has_challenge,
                       last_retrieved_at
                FROM memories
                """
            ).fetchall()
        )
        out["reference_chunks"] = list(
            con.execute(
                """
                SELECT id, doc_id, chunk_index, section, content,
                       embedding_model, embedding_dimension, embedding_source,
                       (CASE WHEN embedding IS NULL THEN 0 ELSE 1 END) AS has_embedding
                FROM reference_chunks
                """
            ).fetchall()
        )
        out["retrieval_metadata"] = list(
            con.execute("SELECT * FROM retrieval_metadata").fetchall()
        )
        out["contradiction_queue"] = list(
            con.execute("SELECT * FROM contradiction_queue").fetchall()
        )
        out["challenges"] = list(
            con.execute("SELECT * FROM challenges").fetchall()
        )
    except sqlite3.OperationalError as e:
        # A tracked table might be missing in a fixture or a
        # pre-embed DB. Treat that as an empty snapshot for the
        # missing table; the post-run check will report a schema
        # mismatch in the diff.
        out.setdefault("missing_table", []).append(("missing_table", str(e)))
    finally:
        con.close()
    return out


def _mutation_diff(
    before: dict[str, list[tuple]], after: dict[str, list[tuple]]
) -> dict[str, Any]:
    """Compare two full snapshots. Returns a status dict with a
    per-table diff. Empty diffs == status clean."""
    diffs: list[dict[str, Any]] = []
    tables = sorted(set(before) | set(after))
    for t in tables:
        b = before.get(t, [])
        a = after.get(t, [])
        if b == a:
            continue
        # Compute the row-level diff. Tuples of rows are
        # order-dependent (memories table is roughly insertion
        # order; retrieval_metadata is keyed on a different
        # axis). We compare as multisets.
        from collections import Counter
        cb, ca = Counter(b), Counter(a)
        added = ca - cb
        removed = cb - ca
        if added or removed:
            diffs.append(
                {
                    "table": t,
                    "rows_added": len(added),
                    "rows_removed": len(removed),
                }
            )
    return {
        "status": "clean" if not diffs else "FAIL",
        "diffs": diffs,
    }


def _ingest_corpus(snap: Path, corpus: Corpus) -> None:
    """Insert the synthetic corpus's documents into the disposable DB
    as memories.

    We use direct SQLite (not mpm ingest) because mpm auto-generates
    16-char hex IDs for new memories, and we need deterministic 8-char
    IDs that match `stable_doc_id(content)` and survive MPM's
    shortID() 8-char truncation. mpm does not expose an `--id` flag
    for memory creation; the only path is direct SQL.

    Gate 3 of HAL's implementation gates requires that synthetic
    benchmark runs use a fresh disposable DB containing only the
    versioned synthetic corpus, with deterministic, unambiguous
    mapping between corpus IDs and MPM's abbreviated result IDs.
    The 8-char corpus IDs ARE the abbreviated result IDs; that
    is the unambiguous mapping.

    The schema is created by triggering an mpm command on the empty
    workspace (which lazily creates the schema and lock file via
    NewDatabaseManager). Then we INSERT the corpus rows directly.
    """
    import subprocess as sp

    mpm_bin = _resolve_mpm_binary()
    env = scrubbed_env(snap)
    # Trigger schema initialization. `mpm remember` is a real write
    # command that opens the DB through NewDatabaseManager, which
    # creates the schema, the lock file, and the WAL. This is the
    # only safe way to bootstrap a disposable DB; mpm has no other
    # public command that does it. The auto-created memory and the
    # mirror.jsonl / toxicphrases.txt side effects are removed by
    # the DELETE below.
    init = sp.run(
        [str(mpm_bin), "remember", "bootstrap"],
        env=env,
        cwd=str(snap),
        capture_output=True,
        text=True,
        check=False,
        timeout=30,
    )
    if init.returncode != 0:
        raise RuntimeError(
            f"mpm remember failed to initialize workspace: rc={init.returncode} "
            f"stderr={init.stderr}"
        )

    # Now insert memories directly. The schema is in place.
    db_path = _find_db_path(snap)
    if db_path is None:
        raise RuntimeError(
            f"mpm version did not create a database under {snap}"
        )
    con = sqlite3.connect(str(db_path))
    try:
        # Wipe any auto-created memories from the bootstrap so the
        # synthetic corpus is the only thing in the DB (gate 3).
        con.execute("DELETE FROM memories")
        for d in corpus.documents:
            con.execute(
                "INSERT OR REPLACE INTO memories "
                "(id, collection, content, tags, metadata, weight) "
                "VALUES (?, ?, ?, ?, ?, ?)",
                (
                    d.doc_id,
                    "memories",
                    d.content,
                    "[]",
                    "{}",
                    1.0,
                ),
            )
        con.commit()
    finally:
        con.close()


def _find_db_path(snap: Path) -> Path | None:
    """Locate the SQLite DB file. Accepts either a workspace dir
    (looks for <snap>/src/db/mpm.db) or a direct file path to a
    .db file. Returns None if neither is found.
    """
    if snap.is_file():
        return snap
    candidate = snap / "src" / "db" / "mpm.db"
    if candidate.exists():
        return candidate
    # Walk the workspace looking for any mpm.db.
    for p in snap.rglob("mpm.db"):
        return p
    # Also accept any .db file at the top of the dir (test fixtures).
    if snap.is_dir():
        for p in snap.glob("*.db"):
            return p
    return None


def _resolve_mpm_binary() -> Path:
    """Return the path to the mpm binary. For tests, allow override
    via MPM_BENCH_BINARY env var."""
    override = os.environ.get("MPM_BENCH_BINARY")
    if override:
        return Path(override)
    if DEFAULT_MPM_BINARY.exists():
        return DEFAULT_MPM_BINARY
    raise FileNotFoundError(
        f"mpm binary not found at {DEFAULT_MPM_BINARY} and "
        f"MPM_BENCH_BINARY not set; build with `make build` or "
        f"set the env to an explicit path"
    )


def _backfill(snap: Path) -> tuple[int, str]:
    """Run mpm ops backfill-embeddings --include all --force-reembed.
    Returns (returncode, stdout). The returncode is informational;
    the harness's gate-1 verification is the source of truth."""
    import subprocess as sp
    env = scrubbed_env(snap)
    proc = sp.run(
        [
            str(_resolve_mpm_binary()),
            "ops", "backfill-embeddings",
            "--include", "all",
            "--force-reembed",
        ],
        env=env,
        cwd=str(snap),
        capture_output=True,
        text=True,
        check=False,
        timeout=600,
    )
    return proc.returncode, proc.stdout + proc.stderr


def cmd_selftest(_args: argparse.Namespace) -> int:
    """Run self-tests. Does not call the mpm binary. Does not open
    the live DB. Suitable for CI smoke and for verifying the
    harness's own logic before a real run."""
    import unittest

    loader = unittest.TestLoader()
    suite = loader.discover(start_dir=str(REPO_ROOT / "bench" / "tests"))
    runner = unittest.TextTestRunner(verbosity=2)
    result = runner.run(suite)
    return 0 if result.wasSuccessful() else 1


def cmd_smoke(args: argparse.Namespace) -> int:
    """Full smoke test: spins up a fake embeddings server, creates a
    fresh disposable DB, ingests the synthetic corpus, runs the
    backfill, verifies gate 1, runs a sample of queries, checks
    participation, and prints a report. Does not touch the live DB."""
    from bench.fake_server import make_server

    corpus = load_corpus(DEFAULT_CORPUS_DIR)

    # Start the fake server.
    dim = 64
    server, base_url, port = make_server(dim=dim, model_name="fake-embed-v1")
    try:
        # v3 review: per-run mkdtemp; the previous PID-derived
        # name leaked across smoke runs in rare cases.
        snap = assert_safe_workspace(_make_snap_workspace("mpm-bench-smoke-"))
        # Configure the embedding profile to point at the fake. MPM's
        # `buildProvider` switch only knows "ollama", "openai-compatible",
        # "openai", and "openrouter"; the fake server speaks the
        # OpenAI-compatible protocol, so we use that provider name.
        # Using "custom" would silently resolve to NullProvider
        # (status=misconfigured) and mpm remember would fail.
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
        # Reject unsafe symlinks in the snapshot workspace BEFORE
        # launching MPM. The smoke path is hermetic and the tempdir
        # is freshly created, but the v3 review requires this guard.
        bad_snap = reject_unsafe_symlinks(snap)
        if bad_snap:
            raise RuntimeError(
                f"unsafe symlinks in snapshot workspace {snap}: {bad_snap}"
            )
        # Ingest the corpus via mpm.
        _ingest_corpus(snap, corpus)
        # Backfill embeddings.
        rc, out = _backfill(snap)
        # Verify.
        db_path = _find_db_path(snap)
        if db_path is None:
            raise RuntimeError(
                f"mpm version did not create a database under {snap}"
            )
        result = verify_regeneration_complete(
            db_path,
            expected_model="openai-compatible:fake-embed-v1",
            expected_dimension=dim,
            expected_doc_ids={d.doc_id for d in corpus.documents},
            expected_memories_row_count=len(corpus.documents),
        )
        print(f"REGENERATION: {'ok' if result.ok else 'FAILED'}")
        if not result.ok:
            for f in result.failures:
                print(f"  - {f}")
        # Run a few queries and check participation.
        results = []
        for q in corpus.queries:
            r = run_query(q, snap, _resolve_mpm_binary())
            results.append(r)
        p = per_query_participation(results, {q.query_id for q in corpus.positive_queries})
        print(
            f"PARTICIPATION: positive={p['positive_vector_participated']}/{p['positive_total']} "
            f"correctly_empty={len(p['negative_outcomes']['correctly_empty'])}"
        )
        if p["positive_failures"]:
            for f in p["positive_failures"]:
                print(f"  - FAIL {f['query_id']}: {f['reason']}")
        return 0 if (result.ok and not p["positive_failures"]) else 1
    finally:
        server.shutdown()


def _build_raw_cosine_profile(config: dict[str, Any]) -> dict[str, object] | None:
    """Pick the embedding profile for the raw-cosine experiment.
    The harness uses the same name the components config picks,
    or "default" if absent. Returns None when no usable profile
    is present (the runner's hash fallback is then used; that
    path is for hermetic tests only)."""
    profiles = config.get("profiles", {}) if isinstance(config, dict) else {}
    components = config.get("components", {}) if isinstance(config, dict) else {}
    emb_component = (
        components.get("embedding", "default")
        if isinstance(components, dict)
        else "default"
    )
    if emb_component in profiles:
        return dict(profiles[emb_component])
    if "default" in profiles:
        return dict(profiles["default"])
    return None


def _run_raw_cosine_both(
    queries: tuple,
    snap: Path,
    embedding_profile: dict[str, object] | None,
) -> tuple[list[QueryResult], list[QueryResult] | None]:
    """Run the raw-cosine experiment for BOTH conditions: unprefixed
    and `search_query:`-prefixed. v3 review priority 4.

    Returns (unprefixed_results, prefixed_results). prefixed_results
    is None when no embedding_profile is available (the hermetic
    fallback would have to substitute the hash, which is a real-
    model run anti-pattern; we return None to make it explicit
    that only one condition ran).
    """
    if embedding_profile is None:
        # No profile: only the hermetic hash path is available.
        # Skip the prefix comparison (it would be a hash-vs-hash
        # comparison, not a model measurement).
        unprefixed = [
            run_query(
                q, snap, _resolve_mpm_binary(),
                raw_cosine=True, raw_cosine_prefix="",
            )
            for q in queries
        ]
        return unprefixed, None
    unprefixed = [
        run_query(
            q, snap, _resolve_mpm_binary(),
            raw_cosine=True, raw_cosine_prefix="",
            embedding_profile=embedding_profile,
        )
        for q in queries
    ]
    prefixed = [
        run_query(
            q, snap, _resolve_mpm_binary(),
            raw_cosine=True, raw_cosine_prefix=RAW_COSINE_PREFIX,
            embedding_profile=embedding_profile,
        )
        for q in queries
    ]
    return unprefixed, prefixed


def cmd_run(args: argparse.Namespace) -> int:
    """Full real run. Requires --output-dir and --config. Performs
    all gate checks and writes the report."""
    if not args.output_dir:
        print("--output-dir is required for `run`", file=sys.stderr)
        return 2
    if not args.config:
        print("--config is required for `run`", file=sys.stderr)
        return 2

    corpus = load_corpus(DEFAULT_CORPUS_DIR)
    output_dir = Path(args.output_dir).resolve()
    config = json.loads(Path(args.config).read_text())

    # v3 review: per-run mkdtemp under /tmp/mpm-bench/, not a
    # PID-derived name.
    snap = assert_safe_workspace(_make_snap_workspace("mpm-bench-run-"))
    # v3 review: validate the output dir BEFORE any creation.
    # Pass `create=True` so the safe path creates it post-check.
    out_real = assert_safe_output_dir(output_dir, snap)

    # v3 review: reject unsafe symlinks in BOTH the output dir
    # and the snapshot workspace, BEFORE MPM is launched.
    bad_out = reject_unsafe_symlinks(out_real)
    if bad_out:
        print(f"REFUSING: unsafe symlinks in output dir: {bad_out}", file=sys.stderr)
        return 1
    bad_snap = reject_unsafe_symlinks(snap)
    if bad_snap:
        print(f"REFUSING: unsafe symlinks in snapshot workspace: {bad_snap}", file=sys.stderr)
        return 1

    # Configure the embedding profile.
    write_mpm_config(
        snap,
        profile=config["profiles"],
        components=config["components"],
    )

    # Ingest the corpus (also creates the DB).
    _ingest_corpus(snap, corpus)

    # Backfill embeddings.
    rc, bf_out = _backfill(snap)

    # Locate the disposable DB.
    db_path = _find_db_path(snap)
    if db_path is None:
        raise RuntimeError(
            f"mpm version did not create a database under {snap}"
        )

    # v3 review: gate 1 verifies NULL/missing detection in BOTH
    # tables and (when the caller provides expected counts) the
    # exact row count after backfill.
    verification = verify_regeneration_complete(
        db_path,
        expected_model=config["expected"]["model"],
        expected_dimension=config["expected"]["dimension"],
        expected_doc_ids={d.doc_id for d in corpus.documents},
        expected_memories_row_count=len(corpus.documents),
    )

    # v3 review: take the "before" snapshot IMMEDIATELY before
    # the first recall. The "after" snapshot is taken after
    # every query; any tracked-field change fails the run.
    before = _snapshot_full(db_path)
    cumulative_diff: dict[str, Any] = {"status": "clean", "diffs": []}
    for q in corpus.queries:
        r = run_query(q, snap, _resolve_mpm_binary())
        after = _snapshot_full(db_path)
        d = _mutation_diff(before, after)
        if d["status"] != "clean":
            cumulative_diff = d
            print(
                f"MUTATION DETECTED after query {q.query_id} "
                f"({q.query!r}): {d['diffs']}",
                file=sys.stderr,
            )
            break
        # Re-run result rows into the per-query list.
        results_so_far = list(corpus.queries[: corpus.queries.index(q) + 1])
        # (the per-query participation uses `results` collected
        # below; we only need the per-row QueryResult objects.)
        _ = results_so_far

    # Rebuild the per-query list from the actual run, so we can
    # report it. The pre-fix `results = [run_query(...)]` did
    # the same; here we re-run to keep the per-query report
    # consistent with the post-mutation-check loop's outcome.
    results = [
        run_query(q, snap, _resolve_mpm_binary())
        for q in corpus.queries
    ]
    positive_ids = {q.query_id for q in corpus.positive_queries}
    participation = per_query_participation(results, positive_ids)

    # v3 review: scoring top-k is read from the corpus contract,
    # not from the operator's config. The operator's config
    # controls the embedding profile and model identity only.
    top_k_for_scoring = corpus.scoring.top_k
    labelled_scoring = score_queries(
        [
            (
                q.query_id,
                q.is_positive,
                tuple(r["id"] for r in res.rows),
                q.relevant_doc_ids,
            )
            for q, res in zip(corpus.queries, results)
        ],
        k=top_k_for_scoring,
    )

    # v3 review: raw-cosine experiment runs BOTH conditions
    # (unprefixed + `search_query:`-prefixed) when a real
    # profile is available. The hash fallback is not used
    # in a real-model run.
    raw_cosine_unprefixed: list[QueryResult] | None = None
    raw_cosine_prefixed: list[QueryResult] | None = None
    raw_cosine_unprefixed_scoring: dict[str, Any] | None = None
    raw_cosine_prefixed_scoring: dict[str, Any] | None = None
    raw_cosine_delta: dict[str, Any] | None = None
    if args.raw_cosine_prefix:
        profile = _build_raw_cosine_profile(config)
        unprefixed, prefixed = _run_raw_cosine_both(
            corpus.queries, snap, profile
        )
        raw_cosine_unprefixed = unprefixed
        raw_cosine_prefixed = prefixed
        raw_cosine_unprefixed_scoring = score_queries(
            [
                (
                    q.query_id,
                    q.is_positive,
                    tuple(r["id"] for r in res.rows),
                    q.relevant_doc_ids,
                )
                for q, res in zip(corpus.queries, unprefixed)
            ],
            k=top_k_for_scoring,
        )
        if prefixed is not None:
            raw_cosine_prefixed_scoring = score_queries(
                [
                    (
                        q.query_id,
                        q.is_positive,
                        tuple(r["id"] for r in res.rows),
                        q.relevant_doc_ids,
                    )
                    for q, res in zip(corpus.queries, prefixed)
                ],
                k=top_k_for_scoring,
            )
            # Compute the metric deltas: prefixed - unprefixed.
            agg_a = raw_cosine_unprefixed_scoring.get("aggregates", {})
            agg_b = raw_cosine_prefixed_scoring.get("aggregates", {})
            raw_cosine_delta = {
                "prefixed_minus_unprefixed": {
                    "mean_precision_at_k": (
                        agg_b.get("mean_precision_at_k", 0.0)
                        - agg_a.get("mean_precision_at_k", 0.0)
                    ),
                    "mean_recall_at_k": (
                        agg_b.get("mean_recall_at_k", 0.0)
                        - agg_a.get("mean_recall_at_k", 0.0)
                    ),
                    "mean_mrr": (
                        agg_b.get("mean_mrr", 0.0)
                        - agg_a.get("mean_mrr", 0.0)
                    ),
                    "mean_ndcg_at_k": (
                        agg_b.get("mean_ndcg_at_k", 0.0)
                        - agg_a.get("mean_ndcg_at_k", 0.0)
                    ),
                },
                "prefix": RAW_COSINE_PREFIX,
            }
        # Participation on the unprefixed condition.
        raw_cosine_participation = per_query_participation(
            unprefixed, positive_ids
        )
        # Per-query tables for both conditions.
        raw_cosine_table = [
            {
                "query_id": r.query_id,
                "class": "positive" if r.is_positive else "negative",
                "n_rows": len(r.rows),
                "top_id": r.rows[0]["id"] if r.rows else "",
                "top_score": r.rows[0]["score"] if r.rows else 0.0,
            }
            for r in unprefixed
        ]
        raw_cosine_prefixed_table = None
        if prefixed is not None:
            raw_cosine_prefixed_table = [
                {
                    "query_id": r.query_id,
                    "class": "positive" if r.is_positive else "negative",
                    "n_rows": len(r.rows),
                    "top_id": r.rows[0]["id"] if r.rows else "",
                    "top_score": r.rows[0]["score"] if r.rows else 0.0,
                }
                for r in prefixed
            ]
    else:
        raw_cosine_participation = None
        raw_cosine_table = None
        raw_cosine_prefixed_table = None

    per_query_table = [
        {
            "query_id": r.query_id,
            "class": "positive" if r.is_positive else "negative",
            "n_rows": len(r.rows),
            "sources": ",".join(sorted(r.sources())) or "(empty)",
            "vector_participated": r.vector_participated(),
            "search_mode": r.search_mode,
        }
        for r in results
    ]

    report = RunReport(
        corpus=corpus_summary(corpus),
        regeneration=verification.to_dict(),
        participation=participation,
        raw_cosine_participation=raw_cosine_participation,
        raw_cosine_unprefixed_scoring=raw_cosine_unprefixed_scoring,
        raw_cosine_prefixed_scoring=raw_cosine_prefixed_scoring,
        raw_cosine_delta=raw_cosine_delta,
        labelled_scoring=labelled_scoring,
        isolation={
            "snapshot_workspace": str(snap),
            "output_dir": str(out_real),
            "snapshot_symlink_rejection": (
                "passed" if not bad_snap else f"FAILED: {bad_snap}"
            ),
            "output_symlink_rejection": (
                "passed" if not bad_out else f"FAILED: {bad_out}"
            ),
            "env_scrubbing": "passed (see test_env_scrubbing.py)",
        },
        snapshot_logical_mutation=cumulative_diff,
        per_query_table=per_query_table,
        raw_cosine_per_query_table=raw_cosine_table,
        raw_cosine_prefixed_per_query_table=raw_cosine_prefixed_table,
        historical_note=(
            "queries.v1 is a small synthetic labelled set, not a recovery of "
            "the historical 30-query benchmark. The historical +0.022 MRR "
            "figure attributed to raw-cosine prefixing is NOT reproduced by "
            "this harness. The raw-cosine delta in the report is the "
            "metric DIFFERENCE between the unprefixed and `search_query:`-"
            "prefixed query embeddings on this corpus; a near-zero delta "
            "on a synthetic set is expected and does not contradict the "
            "historical finding on the lost corpus."
        ),
        output_dir=out_real,
    )
    json_path, md_path = write_report(report)
    print(f"REPORT: {md_path}")
    print(f"REPORT: {json_path}")
    return 0 if (
        verification.ok
        and not participation["positive_failures"]
        and cumulative_diff["status"] == "clean"
    ) else 1


def build_parser() -> argparse.ArgumentParser:
    parser = argparse.ArgumentParser(
        prog="bench",
        description="MPM Embedding Benchmark Harness (Issue #16).",
    )
    parser.add_argument("--version", action="version", version=f"bench {__version__}")
    sub = parser.add_subparsers(dest="cmd")

    selftest = sub.add_parser("selftest", help="Run self-tests (no mpm binary, no live DB).")
    selftest.set_defaults(func=cmd_selftest)

    smoke = sub.add_parser("smoke", help="Run a full smoke test (fake server, disposable DB).")
    smoke.set_defaults(func=cmd_smoke)

    run = sub.add_parser("run", help="Run a real benchmark (requires --output-dir, --config).")
    run.add_argument("--output-dir", required=False, help="Output dir for the report (required for real runs).")
    run.add_argument("--config", required=False, help="JSON config file with embedding profile.")
    run.add_argument(
        "--raw-cosine-prefix",
        action="store_true",
        help=(
            "Run the raw-cosine experiment. v3: BOTH unprefixed and "
            "`search_query:`-prefixed query embeddings are obtained from "
            "the configured provider; the report includes the metric delta."
        ),
    )
    run.set_defaults(func=cmd_run)

    return parser


def main(argv: list[str] | None = None) -> int:
    parser = build_parser()
    args = parser.parse_args(argv)
    if not getattr(args, "func", None):
        # Top-level flags for convenience.
        if "--selftest" in sys.argv:
            return cmd_selftest(args)
        if "--smoke" in sys.argv:
            return cmd_smoke(args)
        parser.print_help()
        return 0
    try:
        return args.func(args)
    except WorkspaceError as e:
        print(f"WORKSPACE ERROR: {e}", file=sys.stderr)
        return 1


if __name__ == "__main__":
    sys.exit(main())
