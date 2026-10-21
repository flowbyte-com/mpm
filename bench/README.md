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
- **No live-store reads**: the harness never opens, reads from,
  or inspects the operator's live MPM database. (The earlier
  v2 `~/.mpm` row-count sentinel was removed in the v3 review.
  The harness is hermetic against `MPM_WORKSPACE` and resolves
  to a tempdir per run; see `bench/workspace.py:scrubbed_env`.)
- **Optional raw-cosine experiment**: when run with
  `--raw-cosine-prefix`, the harness independently ranks vectors
  by cosine similarity, without using the hybrid ranker.
- **Optional instruction-prefix condition matrix** (Issue #23 Phase
  A): `python -m bench prefix-experiment` measures the four
  prefix conditions (A/B/C/D) described below, each in its own
  disposable workspace.

## The instruction-prefix condition matrix (Issue #23)

`bench/prefix_experiment.py` measures whether Nomic's documented
instruction prefixes change retrieval quality:

| Condition | Document prefix | Query prefix |
|---|---|---|
| A `baseline-unprefixed` | (none) | (none) |
| B `query-prefix-only` | (none) | `search_query: ` |
| C `both-roles` | `search_document: ` | `search_query: ` |
| D `document-prefix-diagnostic` | `search_document: ` | (none) |

**Outcome: every prefix condition is at or below the unprefixed
baseline** (MRR@10 0.9182 unprefixed → 0.9146 / 0.9113 / 0.8933), so
the unprefixed baseline is retained: no production prefixing, no
re-embed. The evidence, including the exact model fingerprint, the
corpus provenance, the per-query regressions and the held-out split,
is in [`../docs/ISSUE-23-PREFIX-CALIBRATION-REPORT.md`](../docs/ISSUE-23-PREFIX-CALIBRATION-REPORT.md);
the committed machine-readable run is in `bench/output/issue-23-phase-a/`.

Design rules:

- Conditions that need different **document** embeddings get
  different disposable workspaces, so vectors from differently
  prepared document conditions can never be mixed. Conditions that
  share a document preparation share a workspace. The preparation
  key is DERIVED from the document prefix, so a key can never be
  paired with the wrong text.
- The **document** prefix is applied by the benchmark at ingest
  time, so MPM's own production embed path
  (`cmd/mpm/backfill_embeddings.go` -> `cfg.Provider.Embed(mem.Content)`)
  embeds the prefixed string. The benchmark never re-implements the
  provider.
- The **query** prefix is applied by the benchmark before the
  provider call, the same shape MPM's recall path uses.
- Every workspace gets a fresh fingerprint verification.
- A corpus that already carries a prefix is REFUSED, so a
  double-prefixed measurement cannot happen silently.
- `internal/core` is NOT modified. No production embedding behaviour
  changes.

The MPM hybrid ranker is measured once, on the unprefixed
workspace, and reported separately from the raw-cosine arm: MPM's
recall path embeds the query verbatim
(`internal/core/hybrid_search.go`), so no prefix condition is
expressible through it without a production change. The report
carries the per-query agreement between the two arms, so a reader
can see whether the raw-cosine numbers stand in for MPM's ranker.

```bash
MPM_BENCH_COLLECTION=bench_v2_corpus \
python -m bench prefix-experiment \
    --output-dir /tmp/mpm-bench-issue23 \
    --config bench/fixtures/nomic-embed-text.json \
    --corpus-version v2 \
    --conditions A,B,C,D
```

The report records the model fingerprint (including the provider's
model digest), the per-condition fingerprint verification, MRR /
Recall@1 / Recall@5 / nDCG per depth, the per-query
improved/regressed counts against the same-corpus baseline, the
paraphrase subset, the hard negatives, the calibration / held-out
split breakdown, the metric definitions, and the exact reproduction
command.

## Honest naming

This harness measures the hybrid ranker on the current
synthetic corpus (`bench/corpus/v1/`). The historical +0.022
MRR figure attributed to `search_query:` prefixing on the lost
30-query corpus is **not** reproduced by this harness. To
reproduce it, the original queries and expectations would have
to be reconstructed; this harness ships a small synthetic
labelled set instead. The synthetic corpus's manifest records
this in its `note` field.

The Issue #23 prefix experiment re-measured the prefix question
from scratch on the `v2` synthetic corpus and also did **not**
reproduce the +0.022. Its numbers are deltas against a same-corpus
baseline and are **not** comparable with the historical MRR 0.832.

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
  prefix_experiment.py   # Issue #23 instruction-prefix condition matrix
  fake_server.py         # OpenAI-compatible fake for hermetic tests
  corpus/
    MANIFEST.json        # versioned, content-hashed manifest
    v1/
      documents.json     # 30 synthetic documents
      queries.json       # 20 queries (10 positive, 10 negative)
      expectations.json  # expected top-k per query
      scoring.json       # scoring method + parameters
    v2/
      MANIFEST.json      # content-hashed manifest for the v2 corpus
      documents.json     # 50 synthetic documents
      queries.json       # 60 queries (40 positive, 20 negative)
      expectations.json  # expected top-k per query
      labels.md          # per-query labelling audit trail
      scoring.json       # scoring method + parameters
  output/
    phase-a3/            # committed Issue #15 A.3 measurement
    issue-23-phase-a/    # committed Issue #23 prefix condition matrix
  tests/
    __init__.py
    test_workspace_isolation.py
    test_env_scrubbing.py
    test_corpus_provenance.py
    test_fingerprint.py
    test_per_query_participation.py
    test_runner_smoke.py
    test_prefix_experiment.py  # Issue #23 condition-matrix tests
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
