# Embedding Benchmark Harness

This is the user-facing documentation for the embedding benchmark
harness introduced in Issue #16. It complements the design document
([`EMBEDDING-BENCHMARK-HARNESS-DESIGN.md`](./EMBEDDING-BENCHMARK-HARNESS-DESIGN.md)),
which captures the design rationale and HAL review history.

## Scope

The harness measures how the MPM hybrid ranker behaves under
different embedding configurations on a small, self-contained
synthetic corpus. It is **not** a recovery of the historical
30-query benchmark; the historical corpus and its hand-labelled
relevance judgments were not preserved. The harness ships its own
synthetic corpus under `bench/corpus/v1/` with a content-hashed
manifest that records this fact.

## Why a new harness

The historical benchmark (`docs/EMBEDDING-MODEL-BENCHMARK.md`,
`docs/EMBEDDING-MODEL-COMPARISON.md`) had several properties the
team wanted to keep and several that needed to be tightened:

Properties to keep:
- Two embedding models compared on the same corpus and queries
  (the 22-query qwen3 vs all-minilm comparison; the 30-query
  nomic vs others).
- Per-query measurement, not aggregate.

Properties to tighten (HAL review 2026-10-18):
- The historical benchmark's "all rows re-embedded" assertion
  was based on subprocess exit 0. The new harness verifies every
  embedded row independently (`handleBackfillEmbeddings` always
  returns 0; see `cmd/mpm/backfill_embeddings.go:139`).
- The historical benchmark's "vector participated" assertion
  was aggregate. The new harness is per-positive-query.
- The historical benchmark used a disabled provider in the
  smoke test, expecting a hash fallback. MPM does not support
  that fallback. The new harness uses a fake embeddings server
  for hermetic tests.
- The historical benchmark's reproducibility section referred
  to scripts under `/tmp/embench/` that no longer exist. The
  new harness updates both reproduction sections to point at
  the new tool and retain the explicit statement that the
  historical results cannot currently be reproduced from the
  lost corpus.

## How to run

### Self-tests (no mpm binary, no live DB)

```bash
python -m bench selftest
```

This runs the unit tests in `bench/tests/`. None of them touch
the live database or the network. Suitable for CI.

### Smoke test (fake server, disposable DB)

```bash
CGO_CFLAGS=-DSQLITE_ENABLE_FTS5=1 CGO_LDFLAGS=-lm \
    go build -tags fts5 -o mpm ./cmd/mpm
python -m bench smoke
```

The smoke test:
1. Starts a local fake OpenAI-compatible embeddings server
   (`bench/fake_server.py`) that returns deterministic
   content-hash-derived vectors.
2. Creates a fresh disposable workspace under
   `/tmp/mpm-bench/smoke-<pid>/`.
3. Writes an `mpm_config.json` pointing the embedding profile at
   the fake server.
4. Ingests the synthetic corpus (`bench/corpus/v1/documents.json`)
   via `mpm ingest`.
5. Runs `mpm ops backfill-embeddings --include all --force-reembed`.
6. Verifies the result independently with gate-1 checks.
7. Runs the queries and checks per-query participation.
8. Prints a one-line pass/fail per gate.

The smoke test does **not** call a real provider, does **not**
open the live database, and does **not** make external network
calls.

### Real run

```bash
python -m bench run --output-dir /tmp/mpm-bench-run-001 \
    --config bench/fixtures/minilm.json
```

The output dir is required. The harness creates it with
permissions `0o700` and refuses paths that resolve into the
live store, into the snapshot workspace, or have wider-than-`0o700`
permissions. The fixture file is a small JSON describing the
embedding profile, model, dimension, and base URL.

To also run the raw-cosine experiment (independent of the hybrid
ranker):

```bash
python -m bench run --output-dir /tmp/mpm-bench-run-001 \
    --config bench/fixtures/minilm.json \
    --raw-cosine-prefix
```

## Report

Each real run produces two artefacts in `--output-dir`:

- `report.md` — human-readable Markdown report
- `report.json` — machine-readable report

The report includes:
- The corpus version, document/positive/negative counts, scoring
  method.
- The regeneration verification status (gate 1).
- The snapshot logical mutation status (gate 7).
- The live-store sentinel status (gate 7).
- The vector participation results (C5 / gate 4 of v2 review).
- A per-query table for both the hybrid ranker and (if enabled)
  the raw-cosine experiment.

## What this harness does NOT claim

- It does **not** claim to reproduce the historical +0.022 MRR
  figure attributed to `search_query:` prefixing on the lost
  30-query corpus. The historical corpus was not preserved as
  an artifact; the harness ships a small synthetic set instead.
- It does **not** measure the production MPM database. All
  measurements are against disposable workspaces under `/tmp/`.
- It does **not** alter retrieval, embedding, or contradiction-apply
  algorithms. It only measures how the existing ranker behaves
  against a known corpus.

## Implementation gates (Issue #16)

This harness satisfies all seven implementation gates from
HAL's 2026-10-18 authorization:

1. **Gate 1** — regeneration verification detects missing/NULL
   embeddings and decoded vector length:
   `bench/fingerprint.py:verify_regeneration_complete` and
   `bench/tests/test_fingerprint.py`.
2. **Gate 2** — hermetic smoke test with no real network:
   `bench/fake_server.py`, `bench/tests/test_runner_smoke.py`.
3. **Gate 3** — fresh disposable DB with synthetic corpus only,
   deterministic IDs: `bench/harness.py:cmd_smoke`,
   `bench/corpus.py:stable_doc_id`.
4. **Gate 4** — raw-cosine experiment independent of hybrid
   ranker: `bench/runner.py:_raw_cosine_ranking` and
   `bench/tests/test_runner_smoke.py`.
5. **Gate 5** — valid Python module entry point:
   `bench/__main__.py`, `python -m bench selftest`.
6. **Gate 6** — both reproduction sections updated; the
   `note` field in `bench/corpus/MANIFEST.json` records the
   historical-loss statement.
7. **Gate 7** — C4 isolation; logical mutation checks; tests
   do not inspect or copy the live DB:
   `bench/workspace.py`, `bench/tests/test_workspace_isolation.py`,
   `bench/tests/test_env_scrubbing.py`.
