# Embedding Benchmark Harness (Issue #16)

This is a Python tool that runs a reproducible embedding benchmark
against the MPM CLI. It does NOT modify MPM product code. It calls
the existing `mpm` binary against disposable workspaces and verifies
the result independently.

## What it measures

- **Per-query vector participation** in the hybrid ranker:
  positive queries must have at least one vector- or hybrid-sourced
  row; negative queries are tracked separately, with a
  correctly-empty result counted as a positive signal of the
  ranker's negative-detection capability.
- **Regeneration completeness**: every embedded row in `memories`
  and `reference_chunks` must carry the exact intended (model,
  dimension, source) fingerprint AND the BLOB must decode to a
  vector whose length matches the declared dimension. Subprocess
  exit 0 is irrelevant; this is the load-bearing assertion
  (verified at `cmd/mpm/backfill_embeddings.go:139` where
  `handleBackfillEmbeddings` always returns 0).
- **Snapshot logical mutation**: per-query, the harness asserts
  that tracked tables in the disposable DB are byte-equal at the
  row level before and after the run. (Physical file SHA is not
  used; SQLite WAL mode can legitimately rewrite the same
  logical state.)
- **Live-store sentinel**: the harness captures the live DB's
  row counts at start and end. Drift is reported as
  `inconclusive`, not `failed` (concurrent changes from other
  processes are not automatically blamed on the benchmark).
- **Optional raw-cosine experiment**: when run with
  `--raw-cosine-prefix`, the harness independently ranks vectors
  by cosine similarity, without using the hybrid ranker.

## Honest naming

This harness measures the hybrid ranker on the current
synthetic corpus (`bench/corpus/v1/`). The historical +0.022
MRR figure attributed to `search_query:` prefixing on the lost
30-query corpus is **not** reproduced by this harness. To
reproduce it, the original queries and expectations would have
to be reconstructed; this harness ships a small synthetic
labelled set instead. The synthetic corpus's manifest records
this in its `note` field.

## Quick start

### Self-tests (no mpm binary, no live DB)

```bash
cd <repo-root>
python -m bench selftest
```

Or directly:

```bash
cd <repo-root>
python -m unittest discover -s bench/tests -t .
```

### Smoke test (fake embeddings server, disposable DB)

Requires the mpm binary built with FTS5 support:

```bash
cd <repo-root>
CGO_CFLAGS=-DSQLITE_ENABLE_FTS5=1 CGO_LDFLAGS=-lm go build -tags fts5 -o mpm ./cmd/mpm
python -m bench smoke
```

### Real run

```bash
cd <repo-root>
python -m bench run --output-dir /tmp/mpm-bench-run-001 --config bench/fixtures/minilm.json
python -m bench run --output-dir /tmp/mpm-bench-run-001 \
    --config bench/fixtures/minilm.json \
    --raw-cosine-prefix
```

The fixture file describes the embedding profile, model,
dimension, and base URL.

## What this harness will NOT do

- It will not touch the live MPM database.
- It will not run a benchmark against a non-disposable workspace.
- It will not claim to reproduce the historical +0.022 figure.
- It will not modify the 41 challenged memories.
- It will not install or restart MPM.
- It will not modify the repository's MPM product code.

## File layout

```
bench/
  __init__.py
  __main__.py            # entry: python -m bench ...
  harness.py             # main orchestrator
  fingerprint.py         # C1 / C1' / gate-1 verification
  workspace.py           # C2 / C4 isolation + env scrubbing
  corpus.py              # C3 corpus load + manifest verification
  runner.py              # per-query runner (hybrid + raw-cosine)
  reporter.py            # JSON + Markdown report rendering
  fake_server.py         # OpenAI-compatible fake for hermetic tests
  corpus/
    MANIFEST.json        # versioned, content-hashed manifest
    v1/
      documents.json     # 30 synthetic documents
      queries.json       # 20 queries (10 positive, 10 negative)
      expectations.json  # expected top-k per query
      scoring.json       # scoring method + parameters
  tests/
    __init__.py
    test_workspace_isolation.py
    test_env_scrubbing.py
    test_corpus_provenance.py
    test_fingerprint.py
    test_per_query_participation.py
    test_runner_smoke.py
```

## How the seven implementation gates are satisfied

| Gate | Where |
|---|---|
| 1. Regeneration verification detects missing/NULL embeddings and decoded vector length | `bench/fingerprint.py:verify_regeneration_complete` and `bench/tests/test_fingerprint.py` |
| 2. Hermetic smoke test, no disabled provider, no real network | `bench/fake_server.py`, `bench/tests/test_runner_smoke.py` |
| 3. Fresh disposable DB with synthetic corpus only, deterministic IDs | `bench/harness.py:cmd_smoke`, `bench/corpus.py:stable_doc_id` |
| 4. Raw-cosine experiment independent of hybrid ranker | `bench/runner.py:_raw_cosine_ranking` and `bench/tests/test_runner_smoke.py` |
| 5. Valid Python module entry point | `bench/__main__.py`, `python -m bench selftest` |
| 6. Both reproduction sections updated; historical-loss statement retained | `docs/EMBEDDING-MODEL-COMPARISON.md`, `docs/EMBEDDING-MODEL-BENCHMARK.md`, `bench/corpus/MANIFEST.json:note` |
| 7. C4 isolation; logical mutation; synthetic tests do not inspect/copy the live DB | `bench/workspace.py`, `bench/tests/test_workspace_isolation.py`, `bench/tests/test_env_scrubbing.py` |
