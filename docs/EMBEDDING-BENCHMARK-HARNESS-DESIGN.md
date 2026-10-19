# Issue #16 — Embedding Benchmark Harness (revised design)

> **Status:** design draft v3, awaiting HAL approval.
> **Supersedes:** v1 sketch (rejected 2026-10-18, 7 corrections) and
> v2 (rejected 2026-10-18, 4 final corrections).
> **Hard scope boundary:** no implementation, no commit, no push, no
> live benchmark, no production-data change, no install modification
> until HAL approves this document.

## 0. Why this design exists

Issue #16 asks for a reproducible benchmark harness that compares two
embedding configurations (`qwen3-embedding` vs `all-minilm`) on the
same corpus, with the same queries, with the same relevance labels,
and reports per-query source participation rather than aggregate
metrics. The historical implementation was lost (its reproduction
section is documented in `docs/EMBEDDING-MODEL-COMPARISON.md` and
`docs/EMBEDDING-MODEL-BENCHMARK.md` but the runnable scripts are
gone). This design does not promise to recover that implementation;
it designs a new harness that follows the verification discipline
established by the post-#28 observational contract.

The seven corrections from HAL's review are addressed inline below
under headings C1–C7. Every "verified" claim in the plan is anchored
to a file:line in the repository.

---

## C1 — Verified MPM interfaces (HAL point 1)

The v1 sketch proposed three MPM interfaces that do not exist as
described. The corrected design uses only what the code actually
provides:

| v1 sketch claim                                | Reality (verified)                                                                                                                                                                                                 | What the harness does instead |
|------------------------------------------------|--------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------|-------------------------------|
| `mpm recall --json` returns `rows[]` with full IDs | `cmd/mpm/recall.go:1014` — actual shape: `{"query":..., "memories":[...], "search_mode":"hybrid"}`. IDs are abbreviated via `shortID()` to 8 chars (`cmd/mpm/handlers_cascade.go:331-333`). | The harness parses the canonical envelope, treats abbreviated IDs as opaque, and joins them back to full IDs via an in-memory map keyed at ingest time. |
| `mpm embedding fingerprint` CLI command exists | `cmd/mpm/router.go` — the actual command list is `version, wake, gc, blob, tasks, backup, restore, restore-db, help, doctor, recall, ingest, cascade, migrate, work, continue, stats, prune, export, integration, maintain, review, why, lint`. **No embedding subcommand exists.** | The harness reads `embedding_model`, `embedding_dimension`, `embedding_source` columns directly from the snapshot's `memories` and `reference_chunks` tables via a `mode=ro` SQLite URI connection. No product-code change. |
| `mpm migrate-embeddings` is the supported regeneration command | `cmd/mpm/migrate_embeddings.go` — this is the one-shot remediation command for the v0.1-final era. It is NOT the current supported regeneration path. | The v3 design replaces this with the canonical `mpm ops backfill-embeddings --include all --force-reembed` (see C1′ below). |

Fingerprint verification mechanism (no new product code):

```python
# Pseudocode — uses stdlib sqlite3 with mode=ro URI.
import sqlite3, json, hashlib
def fingerprint_of_snapshot(snapshot_path: Path) -> dict:
    con = sqlite3.connect(f"file:{snapshot_path}?mode=ro", uri=True)
    try:
        # memories.embedding_model / embedding_dimension / embedding_source
        # added by migration {"memories","embedding_model","TEXT"} at
        # internal/core/schema.go:1569.
        mem_rows = con.execute("""
            SELECT embedding_model, embedding_dimension, embedding_source, COUNT(*)
            FROM memories
            WHERE embedding IS NOT NULL AND deleted_at IS NULL
            GROUP BY 1,2,3
        """).fetchall()
        # reference_chunks.* has the same columns since 2026-09-28
        # (internal/core/schema.go:786-794, with a fingerprint comment
        # block at 1469-1476).
        ref_rows = con.execute("""
            SELECT embedding_model, embedding_dimension, embedding_source, COUNT(*)
            FROM reference_chunks
            WHERE embedding IS NOT NULL
            GROUP BY 1,2,3
        """).fetchall()
    finally:
        con.close()
    return {"memories": mem_rows, "reference_chunks": ref_rows}
```

The harness asserts a single (model, dimension, source) group for
both tables after every regeneration and every pristine restore. A
mismatch is a hard fail.

---

## C2 — Workspace configuration (HAL point 2)

Each isolated workspace is configured by writing a `mpm_config.json`
into the snapshot workspace and setting `MPM_WORKSPACE` for
subprocess calls. The config path and resolution priority are
documented at `internal/core/config/config.go:529-597`:

```
Priority: MPM_WORKSPACE env → $HOME/.mpm → CWD
Config file: <workspace>/mpm_config.json
```

Verified config shape (from `internal/core/config/config_simplification_test.go`):

```json
{
  "profiles": {
    "default":   {"provider": "openai", "model": "gpt-4o"},
    "embedding": {"provider": "ollama", "model": "all-minilm",
                  "base_url": "http://localhost:11434"}
  },
  "components": {
    "embedding": "embedding"
  }
}
```

The harness writes this file with the target profile (e.g. `all-minilm`
or `qwen3-embedding`) before each subprocess invocation, then calls:

```bash
env MPM_WORKSPACE=<snapshot_workspace> HOME=<snapshot_workspace>/fakehome \
    <MPM_BINARY> recall --semantic --json --collection memories "<query>"
```

`HOME` is redirected to a per-snapshot fake-home so the MPM subprocess
never resolves to `~/.mpm` even if `MPM_WORKSPACE` is somehow lost.
The harness also pre-creates `<snapshot_workspace>/fakehome/.mpm` to
defeat the CWD fallback (`config.go:574-597` shows CWD is the final
fallback after both env and `$HOME/.mpm`).

The fingerprint check is run **after** `mpm ops backfill-embeddings
--include all --force-reembed` completes and **after** every pristine
restore, with a single-line pass/fail output:
`FINGERPRINT: ok (model=<m>, dim=<d>, source=provider)`.

## C1′ — Regeneration command and independent completion verification (HAL point 1 of v2 review)

**The supported regeneration command is:**

```bash
mpm ops backfill-embeddings --include all --force-reembed
```

Verified at `cmd/mpm/router.go:1056-1057` (dispatch to
`handleBackfillEmbeddings`) and `cmd/mpm/backfill_embeddings.go:38-140`
(flag parsing, two-phase pass, exit-code behaviour).

**Critical implementation property (verified in source):**

`handleBackfillEmbeddings` **always returns 0 at line 139** even when
individual embedding failures occur. The memory pass tracks
`processed, failed` counters (`backfill_embeddings.go:168`) and
prints a failure message at line 112 without changing the exit
code. The reference pass at line 136 calls `runReferenceBackfill`
and **ignores its return value entirely**. Per-row failures (network
blips, transient provider errors, dimension mismatches) are reported
on stdout but the process exits 0.

**Therefore: a subprocess exit code of 0 is NOT evidence that
regeneration completed. The harness MUST verify independently.**

The verification protocol after every regeneration invocation:

```python
def verify_regeneration_complete(
    snapshot_path: Path,
    expected_model: str,
    expected_dimension: int,
    expected_source: str = "provider",
) -> VerificationResult:
    """Open the snapshot read-only and assert every embedded row in both
    memories and reference_chunks has the exact intended (model, dimension,
    source). Subprocess exit 0 is irrelevant."""
    con = sqlite3.connect(f"file:{snapshot_path}?mode=ro", uri=True)
    try:
        # memories.embedding_model, embedding_dimension, embedding_source
        # are real columns (internal/core/schema.go:1552-1569).
        mem_mismatch = con.execute("""
            SELECT COUNT(*) FROM memories
            WHERE embedding IS NOT NULL
              AND deleted_at IS NULL
              AND NOT (embedding_model = ?
                       AND embedding_dimension = ?
                       AND embedding_source = ?)
        """, (expected_model, expected_dimension, expected_source)).fetchone()[0]
        mem_null = con.execute("""
            SELECT COUNT(*) FROM memories
            WHERE embedding IS NOT NULL
              AND deleted_at IS NULL
              AND (embedding_model IS NULL
                   OR embedding_dimension IS NULL
                   OR embedding_source IS NULL)
        """).fetchone()[0]
        # reference_chunks.* mirror the same columns
        # (internal/core/schema.go:786-794, 1469-1476).
        ref_mismatch = con.execute("""
            SELECT COUNT(*) FROM reference_chunks
            WHERE embedding IS NOT NULL
              AND NOT (embedding_model = ?
                       AND embedding_dimension = ?
                       AND embedding_source = ?)
        """, (expected_model, expected_dimension, expected_source)).fetchone()[0]
        ref_null = con.execute("""
            SELECT COUNT(*) FROM reference_chunks
            WHERE embedding IS NOT NULL
              AND (embedding_model IS NULL
                   OR embedding_dimension IS NULL
                   OR embedding_source IS NULL)
        """).fetchone()[0]
    finally:
        con.close()
    failures = []
    if mem_mismatch:
        failures.append(f"memories: {mem_mismatch} rows with non-matching fingerprint")
    if mem_null:
        failures.append(f"memories: {mem_null} rows with NULL fingerprint")
    if ref_mismatch:
        failures.append(f"reference_chunks: {ref_mismatch} rows with non-matching fingerprint")
    if ref_null:
        failures.append(f"reference_chunks: {ref_null} rows with NULL fingerprint")
    return VerificationResult(
        ok=not failures,
        failures=failures,
        expected=(expected_model, expected_dimension, expected_source),
    )
```

The harness invokes `backfill-embeddings`, captures its stdout (which
contains per-row failure messages), then runs
`verify_regeneration_complete` against the snapshot. **The harness
exits non-zero on any mismatch or NULL, regardless of subprocess
exit code.** The verification result is recorded in the run report
as `REGENERATION: ok` or `REGENERATION: FAILED (<reasons>)`. This is
the load-bearing assertion: a "successful" backfill that left 4 rows
with a stale fingerprint (as in the historical qwen3 benchmark
documented in `docs/EMBEDDING-MODEL-BENCHMARK.md:166-175`) is treated
as a failed run, not a successful one.

---

## C3 — Corpus provenance (HAL point 3)

**Honest statement of what is recoverable:** the historical
30-query benchmark corpus and its hand-labelled relevance judgments
were not preserved as artifacts. The doc sections
`docs/EMBEDDING-MODEL-COMPARISON.md:250-258` and
`docs/EMBEDDING-MODEL-BENCHMARK.md:166-175` describe the lost
reproduction but the actual `queries.json` and `expectations.json`
files are gone. The harness does **not** claim to reproduce that
corpus. Instead it ships:

1. **A self-contained labelled corpus** under `bench/corpus/v1/`
   with FOUR artefacts, not two:

   - `documents.json` — the document set the queries are scored
     against. Each entry has a **stable ID** (content-derived
     `sha256(content)[:16]`, never reused across versions), the full
     `content` string, optional `metadata` (source, date, tags), and
     the relevance class (`relevant` / `partially-relevant` /
     `not-relevant`). The set has N≥30 documents spanning the topics
     the queries cover.
   - `queries.json` — N≤20 queries. Each entry has a `query` text,
     a `relevant_doc_ids` list (stable IDs from `documents.json`),
     and a `relevance_class` (`positive` / `negative`).
   - `expectations.json` — the expected top-k for each query, as
     `{query_id: [doc_id_1, doc_id_2, ...]}`, used for MRR / nDCG
     scoring. Keys match `queries.json` IDs.
   - `scoring.json` — the scoring method (`mpm recall --semantic`
     for the hybrid ranker, or the explicit raw-cosine path), with
     its parameters (`top_k`, `score_aggregation`, etc.) recorded
     so the report can reproduce the exact measurement.

   All four files are content-hashed and bound together in the
   manifest (see below). The corpus is generated by:

   a. Reading K=5 seed topics from the live install (read-only,
      not the live store's content).
   b. For each seed topic, hand-authoring 6 documents (2 relevant,
      2 partially-relevant, 2 not-relevant) and 2 queries
      (1 positive, 1 negative).
   c. Assigning stable IDs as `sha256(content)[:16]`.
   d. Recording `expectations.json` and `scoring.json` alongside.

   **Important:** the labels in `documents.json` are **scored against
   the same `documents.json` content**, not against unrelated
   live-memory content. The harness ingests `documents.json` into
   the disposable snapshot workspace, runs the embedding
   regeneration (C1′), and then scores each query's returned
   `memories` rows against the `relevant_doc_ids` in `queries.json`.
   No live-memory content participates in the relevance judgement.

2. **A versioned-and-hashed manifest** (`bench/corpus/MANIFEST.json`)
   that records, for each version:

   ```json
   {
     "version": "v1",
     "documents_sha256": "<hex>",
     "queries_sha256": "<hex>",
     "expectations_sha256": "<hex>",
     "scoring_sha256": "<hex>",
     "generator": "hand-authored-from-topics@<date>",
     "seed_topics": ["...", "..."],
     "documents_count": 30,
     "queries_count": 20,
     "positive_queries_count": 10,
     "negative_queries_count": 10,
     "scoring_method": "mpm recall --semantic",
     "note": "synthetic self-contained set; documents, queries, expectations and scoring are bound together. Not a recovery of the lost historical corpus."
   }
   ```

   The manifest's `note` field is load-bearing: an operator who
   later reads the corpus must see, in the manifest itself, that
   the corpus is self-contained and that its labels are bound to
   the corpus's own documents rather than to live-memory content.

3. **An honest provenance section** in the harness report:
   "queries.v1 is a small synthetic labelled set, not a recovery of
   the historical 30-query benchmark. Any +0.022 figure attributed
   to the historical run is not reproduced by this harness."

The harness refuses to start if the manifest hash and the file
hashes do not match. A future operator who wants a real historical
reproduction would need to reconstruct the corpus by other means;
this harness does not pretend to do so.

---

## C4 — Live-store isolation hardening (HAL point 4)

Four isolation layers, each independently verified by the harness:

1. **MPM_WORKSPACE resolution** — the harness resolves the snapshot
   workspace through `os.path.realpath` (Python `os.path.realpath`,
   resolves symlinks) and refuses to start if the realpath is inside
   `~/.mpm`, `$HOME`, or the repository worktree. Test:
   `test_workspace_realpath_rejects_alias_into_live_store`.

2. **Output-dir explicitness** — the harness CLI takes
   `--output-dir <path>` as a required argument for any "real run"
   (the `--smoke` and `--selftest` modes do not require it). The
   output dir is `os.path.realpath`-checked to be outside the source
   workspace and outside any of the snapshot workspaces.

3. **Symlink/path-alias rejection** — the harness walks the output
   dir and the snapshot workspace before each run and asserts that
   no symlink points at a path outside the disposable set. Test:
   `test_symlink_into_source_workspace_rejected`.

4. **Private output permissions** — output dirs are created with
   `os.makedirs(path, mode=0o700)`. Existing dirs whose permissions
   are wider than 0o700 are rejected with a clear error.

5. **Subprocess env-isolation** — every `subprocess.run` call to the
   MPM binary sets `env={...minimal..., "MPM_WORKSPACE": snap,
   "HOME": snap+"/fakehome", "PATH": os.environ["PATH"]}`. The
   subprocess inherits no other environment variables. Test:
   `test_subprocess_inherits_no_live_env`.

6. **Mutation measurement in disposable snapshots** — the harness
   uses **logical row-level checks**, not physical-file SHA. The
   per-query snapshot mutation assertion is:

   ```python
   def assert_snapshot_logically_unchanged(
       before: set[tuple], after: set[tuple],
       tracked_tables: list[str],
   ) -> LogicalMutationResult:
       """Compare row-level content of tracked tables.
       tracked_tables includes the synthetic corpus's own documents.
       External (live) source mutations are reported separately
       and never automatically blamed on the benchmark."""
   ```

   Specifically the harness:

   a. Before the run, snapshots a `(table, rowid, *cols)` view of
      every tracked table in the disposable snapshot.
   b. After the run, re-snapshots the same view.
   c. Asserts set equality of the row sets.
   d. Reports `SNAPSHOT LOGICAL MUTATION: clean` on equality,
      or `SNAPSHOT LOGICAL MUTATION: <n> rows changed in
      <table>` on inequality.

   **Why logical, not physical:** SQLite WAL mode can legitimately
   rewrite the same logical state with a different on-disk byte
   pattern (WAL checkpoints, fts5 segment rebuilds, pragma
   side-effects of a read-only connection). A byte-level SHA
   difference is not a logical mutation. The physical SHA
   remains a smoke-test for the file-existence invariant (the
   snapshot file is still where the harness put it and is not
   truncated or replaced) but is not the load-bearing
   no-mutation assertion.

7. **Live-store sentinel** — the harness also snapshots the live
   store's row counts of the same tracked tables at start and at
   end. The counts must match.

   **Concurrent live changes are inconclusive, not failure.** If
   the live store's row count drifts between the harness's start
   and end measurements, the harness reports
   `LIVE STORE: inconclusive (drift detected in <table>)` rather
   than `LIVE STORE: failed`. The drift is recorded in the report
   so an operator can audit it. The harness does **not** treat
   the drift as evidence that the benchmark mutated the live
   store — it may have been mutated by an unrelated process
   (another agent session, a scheduled ingest, etc.). The
   benchmark run itself is still considered valid; the live
   sentinel is reported as inconclusive.

8. **Environment scrubbing for subprocess calls** — every
   `subprocess.run` call to the MPM binary sets the following
   env explicitly (no inheritance beyond a minimal allow-list):

   ```python
   SAFE_ENV_KEYS = {"PATH", "LANG", "LC_ALL", "TZ", "TMPDIR"}
   # Verified: MPM_SHARED_DB and MPM_DB_LOCK are real env vars
   # consulted by MPM subprocesses
   # (cmd/mpm/handlers_shared.go:33, cmd/mpm/handlers_shred_database_reset.go:397).
   # They are explicitly UNSET, not just overridden.
   def scrubbed_env(snapshot_workspace: Path) -> dict:
       env = {k: os.environ[k] for k in SAFE_ENV_KEYS if k in os.environ}
       env["MPM_WORKSPACE"] = str(snapshot_workspace)
       env["HOME"] = str(snapshot_workspace / "fakehome")
       # Explicitly clear the dangerous overrides. We do NOT
       # rely on overriding their values; we remove them.
       # (Setting MPM_DB_LOCK to "" would still be picked up
       # by the MPM subprocess as a non-empty value at
       # handlers_shred_database_reset.go:397.)
       return env
   ```

   The subprocess inherits only `PATH`, `LANG`, `LC_ALL`, `TZ`,
   `TMPDIR` from the harness's environment, plus the two
   workspace assignments. `MPM_SHARED_DB`, `MPM_DB_LOCK`,
   `MPM_WORKSPACE` (when not the snapshot's), `MPM_DB_PATH`,
   `OLLAMA_ENDPOINT`, `OLLAMA_MODEL`, and any other MPM- or
   provider-specific variable are scrubbed unless the harness
   itself sets them. A test
   (`test_subprocess_inherits_no_dangerous_env`) walks the
   subprocess environment and asserts that none of the
   dangerous keys is set.

---

## C5 — Per-query vector participation (HAL points 5 and 4 of v2 review)

The v1 sketch used an 80% aggregate threshold ("at least 80% of
queries have at least one vector-sourced row"). The corrected design
asserts the contract per-query **and tracks positive and negative
queries separately**.

**Important distinction (v2 review, HAL point 4):**

- An **expected-positive** query (the corpus's `queries.json` marks
  it as `relevance_class: "positive"`, i.e. it has at least one
  `relevant_doc_ids` entry) **must** have at least one returned
  row with `source ∈ {"vector","hybrid"}`. This is the contract
  the hybrid ranker is being asked to honour.

- An **expected-negative** query (`relevance_class: "negative"`,
  zero `relevant_doc_ids`) **may correctly return an empty
  result set**. A correctly-empty result is the desired outcome
  for a negative query — the vector channel correctly judged that
  no document in the corpus matches the query. A correctly-empty
  result is **not** a participation failure.

```python
def per_query_vector_participation(
    query_results: list[dict],  # one entry per query, each a list of {source: ...}
    positive_query_ids: set[str],
) -> dict:
    """For each positive query, require at least one row with source in
    {vector, hybrid}. For each negative query, accept any outcome
    (including a correctly-empty result set) as long as the run was not
    silently bypassed. Empty result sets for negative queries are
    recorded as 'correctly_empty' (a positive signal), not as failures."""
    positive_failures = []
    negative_outcomes = {"correctly_empty": [], "vector_participated": [], "fts5_only": []}
    for q in query_results:
        sources = {row["source"] for row in q["rows"]}
        vector_participated = bool(sources & {"vector", "hybrid"})
        is_positive = q["query_id"] in positive_query_ids
        if is_positive:
            if not vector_participated:
                positive_failures.append({
                    "query_id": q["query_id"],
                    "query": q["query"],
                    "reason": "positive query, no vector or hybrid row",
                    "rows": q["rows"],
                })
        else:
            # Negative query. Track outcome but do not fail.
            if not q["rows"]:
                negative_outcomes["correctly_empty"].append(q["query_id"])
            elif vector_participated:
                negative_outcomes["vector_participated"].append(q["query_id"])
            else:
                negative_outcomes["fts5_only"].append(q["query_id"])
    return {
        "positive_total": len(positive_query_ids),
        "positive_failures": positive_failures,
        "positive_vector_participated": len(positive_query_ids) - len(positive_failures),
        "negative_total": len(query_results) - len(positive_query_ids),
        "negative_outcomes": negative_outcomes,
    }
```

The harness exits non-zero if `positive_failures` is non-empty.
The result is recorded in the run report as a per-query table
separated by positive/negative class. Empty result sets for
negative queries are recorded as `correctly_empty` (a positive
signal) and reported alongside the participation numbers. Empty
result sets for positive queries are recorded as participation
failures.

This matches the historical vector-source assertion
(`docs/EMBEDDING-MODEL-COMPARISON.md:226-233`): "the runner now
asserts the fingerprint before the first query and asserts that
returned rows are `vector`/`hybrid`-sourced after the last". The
historical runner asserted it after-the-last in aggregate; this
harness asserts it per-positive-query and exits non-zero on a
single violation, while tracking negative-query outcomes
separately so a correctly-empty negative result is reported as a
success of the ranker's negative-detection capability.

---

## C6 — Package launch and test framework (HAL point 6)

The v1 sketch proposed `pytest`. The corrected design uses
`unittest` because:

1. The repository's existing benchmarks (e.g. the 22-query benchmark
   in `docs/EMBEDDING-MODEL-BENCHMARK.md`) are described as
   `unittest`-style test cases. The new harness should match.
2. `unittest` is in the stdlib; the harness needs no third-party
   Python deps. This is a property the historical
   `mpm recall --semantic` snapshot script also relied on
   (`docs/RETRIEVAL-BENCHMARK.md:5-7`: snapshot is a stdlib
   `sqlite3.Connection.backup()` call).
3. The launch directory is the `bench/` subdir of the issue-16
   worktree, not the repository root. The harness is invoked as
   `python -m bench.harness ...` from `bench/`, with `bench/__init__.py`
   and `bench/__main__.py` providing the module entry point.

Test layout (all in `bench/tests/`):

```
bench/
  __init__.py
  __main__.py        # entry: python -m bench ...
  harness.py         # main runner
  fingerprint.py     # C1 fingerprint check + C1′ regeneration verification
  workspace.py       # C2 / C4 workspace + isolation + env scrubbing
  corpus.py          # C3 corpus load + manifest check (4-file binding)
  runner.py          # per-query runner (calls mpm recall)
  reporter.py        # output rendering
  corpus/
    v1/
      documents.json
      queries.json
      expectations.json
      scoring.json
    MANIFEST.json    # binds documents+queries+expectations+scoring
  tests/
    __init__.py
    test_fingerprint.py
    test_regeneration_verification.py   # C1′ independent verify
    test_workspace_isolation.py
    test_env_scrubbing.py               # MPM_SHARED_DB / MPM_DB_LOCK unset
    test_corpus_provenance.py
    test_per_query_participation.py
    test_runner_smoke.py       # uses a real disposable DB; no live call
```

All tests use `unittest.TestCase`. The runner smoke test uses a
disposable DB built by `mpm ops backfill-embeddings
--include all --force-reembed` against a temp workspace; it does
not call any live provider (it points the embedding profile at
`disabled`, falling back to hash embeddings that the snapshot
script preserves). The smoke test does **not** rely on
`handleBackfillEmbeddings` exit 0; it relies on the C1′
independent fingerprint verification.

---

## C7 — Raw-cosine prefix vs hybrid ranker (HAL point 7)

The +0.022 MRR improvement attributed to the `search_query:` prefix
(`docs/EMBEDDING-MODEL-COMPARISON.md:250-258`,
`docs/NOMIC-MIGRATION.md:~108`) was a **raw-cosine** measurement.
The hybrid ranker combines fts5 and vector scores with a weight;
the prefix's effect on the ranker is a separate question.

The harness separates the two measurements:

1. **Hybrid ranker measurement** (the primary report): for each
   query, run `mpm recall --semantic --json` and record the rank
   and source of every returned row. The vector-sourced-row
   participation check (C5) is part of this report.

2. **Raw-cosine prefix experiment** (a separate, optional report):
   the harness exposes a `--raw-cosine-prefix` flag that, when set,
   embeds the query with the prefix `search_query: ` and re-runs
   the same per-query measurement, comparing ranks against the
   no-prefix run. The report is rendered as a separate section
   under "Raw-cosine prefix experiment" and is **not** included in
   the primary report's summary.

3. **Honest naming**: the harness never claims to reproduce the
   historical +0.022 figure. The harness's README explicitly
   states: "This harness measures the hybrid ranker on the current
   installed corpus. The historical +0.022 figure attributed to
   raw-cosine prefixing on the lost 30-query corpus is **not**
   reproduced by this harness. To reproduce it, recover the
   original queries and expectations and re-run with
   `--raw-cosine-prefix`."

---

## Deliverable layout (post-approval)

After HAL approves this design, the implementation will produce:

```
bench/
  __init__.py
  __main__.py
  harness.py
  fingerprint.py
  workspace.py
  corpus.py
  runner.py
  reporter.py
  README.md                  # operational notes + C7 honest naming
  corpus/
    MANIFEST.json
    v1/
      queries.json
      expectations.json
  tests/
    __init__.py
    test_fingerprint.py
    test_workspace_isolation.py
    test_corpus_provenance.py
    test_per_query_participation.py
    test_runner_smoke.py
docs/EMBEDDING-BENCHMARK-HARNESS.md   # user-facing harness doc
```

No new Go code. No MPM product code changes. The harness is a
Python tool that calls the existing `mpm` binary against disposable
workspaces.

---

## Stop point

This document is the v3 design. **Stop here for HAL review.** No
implementation, no commit, no push, no live benchmark, no
production-data change, no install modification until HAL approves
this design. The v3 addresses HAL's four final corrections:

1. **C1′** — regeneration uses `mpm ops backfill-embeddings --include
   all --force-reembed`; the harness independently verifies every
   embedded row in both `memories` and `reference_chunks` has the
   exact intended fingerprint, because `handleBackfillEmbeddings`
   returns 0 even on per-row failures.
2. **C3 (v2)** — the synthetic corpus is self-contained: it ships
   `documents.json` with stable IDs derived from content, plus
   `queries.json`, `expectations.json`, `scoring.json`, and a
   manifest that binds all four. Labels are scored against the
   corpus's own documents, not against unrelated live-memory
   content.
3. **C4 (v2)** — physical file integrity is separated from logical
   mutation detection; per-query snapshot mutation is asserted via
   row-level checks; live-store drift is reported as `inconclusive`
   rather than `failed`; `MPM_SHARED_DB` and `MPM_DB_LOCK` (real env
   vars at `cmd/mpm/handlers_shared.go:33` and
   `cmd/mpm/handlers_shred_database_reset.go:397`) are explicitly
   unset before every subprocess call.
4. **C5 (v2)** — vector participation is required for positive
   queries, while negative queries are tracked separately and a
   correctly-empty result for a negative query is reported as a
   positive signal.

---

## v2 review follow-ups (HAL 2026-10-19)

After the v3 implementation landed, HAL's review surfaced eight
follow-up items. Five were addressed in code; three are reported
without code change per the scope-discipline rule (no changes
unrelated to PR #40 without approval).

### Filesystem isolation hardening (priority 1)

C4 in v3 covered realpath rejection, output-dir explicitness,
symlink rejection, and private output permissions. v4 (this
section) closes the TOCTOU window between the original safety
check and the actual write under the output dir, and rejects
within-root symlinks whose target resolves into a live-store
sentinel.

New module surface in `bench/workspace.py`:

- `_path_is_under(child, parent)`: realpath-resolved containment
  check.
- `_assert_outside_live_store_sentinels(real, role)`: refuses if
  `real` is the same as or inside any `LIVE_STORE_SENTINELS`
  (`~/.mpm`, `~`).
- `recheck_output_dir_safe(out, snap_real)`: re-runs the realpath
  + permission check immediately before each write. The harness
  MUST call this immediately before any write under `out`.
- `reject_unsafe_symlinks` now also rejects within-root symlinks
  whose target resolves into a live-store sentinel. The pre-fix
  walk accepted such symlinks because the within-root check
  passed.
- `DANGEROUS_ENV_PREFIXES` (`MPM_/OLLAMA_/OPENAI_/ANTHROPIC_/
  OPENROUTER_`) added to `scrubbed_env`. Forward-compatible
  against future MPM/OLLAMA/OPENAI/ANTHROPIC/OPENROUTIC
  overrides.

Covered by `bench/tests/test_adversarial.py::TestSymlinkChainIntoLiveStore`,
`TestRecheckOutputDirTOCTOU`, `TestEnvPrefixDenyList`.

### Complete regeneration verification (priority 2)

C1′ in v3 covered (model, dimension, source) fingerprint and
BLOB length checks. v4 adds three more checks on the decoded
vector itself:

- **All-zero vector**: a `provider` row whose decoded BLOB is
  all zeros is a silent failure; the ranker will see a
  directionless vector. JSON round-trip accepts it.
- **NaN/Inf**: a NaN can survive `json.loads` on some Python
  versions because the `json` module accepts the strings `NaN`
  and `Infinity` as extensions. The fingerprint check passes
  (length and model match), so the new sanity check is the only
  thing that catches this.
- **Huge L2 norm**: a stored vector whose L2 norm is far from
  `expected_dimension` in magnitude (e.g. `1e6`) is a real
  model failure or a unit mismatch.

Threshold: `math.sqrt(norm_sq) > 1e6 * expected_dimension`.
NaN check: `not isinstance(x, (int, float)) or x != x or x in
(float("inf"), float("-inf"))`.

Covered by `bench/tests/test_adversarial.py::TestVerifyRegenerationAdversarial`
(zero, NaN, huge-norm + a positive case that must still pass).

### Real labelled relevance scoring (priority 3)

v3's C5 covered per-query vector participation but did NOT score
the ranker against the corpus's `relevant_doc_ids` — the only
labelled measure was "did the vector channel participate?",
which conflates a strong negative-query defence with a weak
positive-query match. v4 adds Precision@k, Recall@k, MRR, and
nDCG@k against the corpus's labels for both the hybrid ranker
and the raw-cosine experiment.

Implementation: `bench/scoring.py` with `score_query` and
`score_queries`. Aggregates are reported over positive queries
only; negative queries are excluded from the aggregate because
the labelled metrics assume a non-empty relevance set.

- **Precision@k** = `(# of relevant docs in top-k) / k`. For
  positive queries, `relevant_ids` is non-empty.
- **Recall@k** = `(# of unique relevant docs in top-k) /
  |relevant_ids|`. Capped at 1.0 even if the same relevant doc
  appears multiple times.
- **MRR** = `1 / rank_of_first_relevant`, or 0.0 if no
  relevant doc is in the result set.
- **nDCG@k** = `DCG@k / IDCG@k` with binary relevance and
  `discount = 1 / log2(rank + 1)`. Rank 1 → 1.0; rank 2 →
  `1 / log2(3) ≈ 0.6309`.

Covered by `bench/tests/test_scoring.py` (12 cases including
the hand-computed nDCG reference).

### Provider-aware raw-cosine query embedding (priority 4)

v3's C7 (raw-cosine experiment) used a SHA-256 hash for the
query vector (`deterministic_vector(query_text, dim)`) that
was only meaningful for the fake server. Real runs against
Ollama or any other real provider would compare the
document-vector from the real provider against a query-vector
from a hash, which is apples-to-oranges.

v4 embeds the query with the same provider the documents
were embedded with, via HTTP to the appropriate endpoint:

- **Ollama**: POST `{base_url}/api/embed` with
  `{"model": "<model>", "input": "<query>"}` → reads
  `{"embeddings": [[...]]}`. Modern endpoint; the deprecated
  `/api/embeddings` is intentionally not used.
- **OpenAI-compatible / OpenAI / OpenRouter**: POST
  `{base_url}/embeddings` with `{"input": "<query>",
  "model": "<model>"}` → reads
  `{"data": [{"embedding": [...]}]}`.

Implementation: `bench/embed_query.py` with `embed_query` and
`l2_normalize`. The profile is resolved by
`bench/harness.py:cmd_run` from
`config["profiles"][components["embedding"]]` or
`config["profiles"]["default"]`. If neither is present, the
runner falls back to the hash function (matches the hermetic
test path).

Vector-length check: if the provider returns a different-
dimension vector than the corpus, the runner returns an
empty ranking rather than silently misaligning the
comparison.

Covered by `bench/tests/test_embed_query.py` (13 cases — one
in-process HTTP server per provider shape) and
`bench/tests/test_adversarial.py::TestRawCosineQueryEmbeddingAdversarial`
(the test wires a fake server on a different port, points a
temp DB at the same provider, and asserts the ranker actually
called the server for the query).

### Adversarial regression tests (priority 5)

11 cases in `bench/tests/test_adversarial.py` covering
priorities 1, 2, 4 and the prefix denylist. These tests do NOT
use any mpm binary; they construct the same in-process
conditions an adversary would and verify the harness's hard
refusals. Each test names a specific failure mode in its
docstring so the future reader knows what regression it
guards.

### CI integration

`make test-bench` is wired into both `make test` and
`make test-race` with a non-vacuity floor of 50 tests
(current population 75). The
`Embedding benchmark harness (bench/tests)` step in
`.github/workflows/build-test.yml` runs the same target in
the `gate` job, so the suite is reachable from CI.

### Reported without code change (scope discipline)

- **TestHITLRotation_AgeTrigger** (release-gate order-dependent
  failure observed in earlier CI history): re-ran in isolation
  under three independent selectors
  (`-run TestHITLRotation_AgeTrigger`,
  `-run TestHITL*`, `-count=3`) and the test passes every
  time on the worktree's current HEAD. The test uses
  `t.TempDir()` for isolation and an injected clock; no shared
  state is reachable across the suite. A future reproduction
  of the failure should capture the seed/race output rather
  than retry the same flag set.
- **Historical reproducibility**: still unverifiable from the
  lost 30-query corpus. The harness ships its own synthetic
  corpus under `bench/corpus/v1/`. The 22-query qwen3 vs
  all-minilm comparison in `docs/EMBEDDING-MODEL-COMPARISON.md`
  remains unreproducible from inside this repository.
- **The `~/.mpm/harness-output-should-be-rejected` directory**
  exists because `bench/tests/test_workspace_isolation.py:67-74`
  creates the path as the rejected output target. The directory
  is empty (no leakage) and correctly reflects the rejection
  path. Per HAL's instruction, the directory is left in place.
