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
  **Update (2026-10, Issue #23):** the prefix question was then
  measured directly by the harness's `prefix-experiment` subcommand,
  on a fresh synthetic corpus, with the fingerprint gate active. Every
  prefix condition came out at or below the unprefixed baseline, so
  the historical +0.022 was not reproduced and **MPM applies no
  instruction prefix on either role.** See
  [`ISSUE-23-PREFIX-CALIBRATION-REPORT.md`](./ISSUE-23-PREFIX-CALIBRATION-REPORT.md).
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

## v2 review (HAL 2026-10-19) — eight priority fixes

After the v1 implementation landed, HAL's review surfaced eight
follow-up items. Five were addressed in code; three are reported
without code changes per the scope-discipline rule (no changes
unrelated to PR #40 without approval).

### Addressed in code

1. **Filesystem isolation hardening (priority 1)**: TOCTOU recheck
   immediately before each write under the output dir
   (`bench/workspace.py:recheck_output_dir_safe`); rejection of
   within-root symlinks whose target resolves into a live-store
   sentinel (`bench/workspace.py:reject_unsafe_symlinks`); a
   forward-compatible `DANGEROUS_ENV_PREFIXES` denylist
   (`MPM_/OLLAMA_/OPENAI_/ANTHROPIC_/OPENROUTER_`) that scrubs
   any prefix match even if it is not in the explicit
   `DANGEROUS_INHERITED_KEYS` set. Covered by
   `bench/tests/test_adversarial.py::TestSymlinkChainIntoLiveStore`,
   `TestRecheckOutputDirTOCTOU`, `TestEnvPrefixDenyList`.

2. **Complete regeneration verification (priority 2)**: in addition
   to the (model, dimension, source) fingerprint and BLOB length
   check, the regeneration verifier now rejects all-zero vectors
   (silent provider failure), NaN/Inf entries, and L2 norms above
   `1e6 * expected_dimension` (a real model failure or a unit
   mismatch). Covered by
   `bench/tests/test_adversarial.py::TestVerifyRegenerationAdversarial`.

3. **Real labelled relevance scoring (priority 3)**: Precision@k,
   Recall@k, MRR, and nDCG@k are now computed against the corpus's
   `relevant_doc_ids` for both the hybrid ranker and the raw-cosine
   experiment. Aggregates are reported over positive queries only;
   negative queries are tracked separately per C5. Implemented in
   `bench/scoring.py` and wired through `bench/harness.py:cmd_run`
   and `bench/reporter.py`. Covered by
   `bench/tests/test_scoring.py` (12 cases including a hand-computed
   nDCG reference for the one-relevant-at-rank-2 case).

4. **Provider-aware raw-cosine query embedding (priority 4)**: the
   raw-cosine experiment now embeds the query with the same
   provider the documents were embedded with, via HTTP to
   `{base_url}/api/embed` (Ollama) or `{base_url}/embeddings`
   (OpenAI-compatible / OpenAI / OpenRouter). The pre-fix path used
   a SHA-256 hash that was only meaningful for the fake server.
   Implemented in `bench/embed_query.py` and wired through
   `bench/runner.py:_raw_cosine_ranking`. Backward-compat fallback
   to the hash function is preserved for hermetic tests. Covered by
   `bench/tests/test_embed_query.py` (13 cases — in-process HTTP
   server per provider) and
   `bench/tests/test_adversarial.py::TestRawCosineQueryEmbeddingAdversarial`.

5. **Adversarial regression tests (priority 5)**: 11 cases in
   `bench/tests/test_adversarial.py` covering the four priorities
   above plus the TOCTOU and prefix-denylist guards.

### CI integration

`make test-bench` is wired into both `make test` and `make test-race`
with a non-vacuity floor of 50 tests (current population 75). The
`Embedding benchmark harness (bench/tests)` step in
`.github/workflows/build-test.yml` runs the same target in the
`gate` job, so the suite is reachable from CI.

### v3 review (HAL 2026-10-19)

After the initial PR #40 went green, HAL completed a third review
that focused on source correctness, isolation, and the experiment
itself rather than on the harness skeleton. The corrections landed
in a single focused commit; this section records what changed and
why. The harness skeleton (workspace, env scrubbing, fingerprint,
runner, reporter) is unchanged in shape — only the contracts inside
those modules were tightened.

**Priority 1 — logical mutation verification (C4 / gate 7).** The
v2 implementation captured a "before" snapshot and compared once,
at the end of the run, to whatever the disposable DB looked like
*after* recall. v3 moves the snapshot to **immediately before the
first query** and captures an **after** snapshot after every query;
any diff (multiset-comparison across `memories`,
`reference_chunks`, `retrieval_metadata`, `contradiction_queue`,
`challenges`) fails the run. Tracked fields per row: id, content,
tags, metadata, weight, deleted_at, embedding model / dimension /
source / presence, has_challenge, last_retrieved_at, plus all
columns of `retrieval_metadata`, `contradiction_queue`, and
`challenges`. New tests
`TestRecallMustNotMutateMemories` / `TestMutationAfterRecallDetected`
prove the diff is detected on a controlled mutation and clean
otherwise.

**Priority 2 — production isolation.** The v2 path read
`~/.mpm/src/db/mpm.db` as a "live store" sentinel. v3 removes that
read entirely; the `live_store` field and the `_live_store_snapshots`
helper no longer exist. The snapshot workspace is now created via
`tempfile.mkdtemp` (each run gets its own private
`/tmp/mpm-bench-run-*`). Both `assert_safe_workspace` and
`assert_safe_output_dir` accept a `protected_root` parameter so
tests can simulate a home directory under `/tmp` without ever
referencing `Path.home() / ".mpm"`. The v2 mkdir-then-check ordering
leaked `~/.mpm/harness-output-should-be-rejected` on disk during
rejection; v3 runs the safety checks first and creates nothing on
rejection. The artifact that earlier code did create was
intentionally **inspected but not deleted** per HAL's instruction.
`reject_unsafe_symlinks` now also runs on the snapshot workspace
**before** MPM is launched; a within-snap symlink into a protected
root is caught and the run aborts before any subprocess spawns.
New tests: `TestLiveStoreNotInspected`,
`TestOutputDirValidatedBeforeMkdir`, `TestSnapshotIsMkdtemp`,
`TestSymlinkRejectedBeforeMpmLaunch`.

**Priority 3 — regeneration verification covers both tables.**
`verify_regeneration_complete` now checks BOTH `memories` AND
`reference_chunks` for NULL/missing vectors. New parameters
`expected_reference_doc_ids`,
`expected_memories_row_count`, and
`expected_reference_chunks_row_count` catch partial backfills even
when coverage is satisfied. The pre-v3 path would have passed
"5 of 10 reference chunks have embeddings" as long as the 5 looked
right; v3 fails it explicitly with a "partial reference backfill
detected" gate. New tests
`TestPartialReferenceBackfillFails` /
`TestExpectedReferenceRowCountMismatch` prove the failures are
observable.

**Priority 4 — raw-cosine prefix experiment is two genuine
measurements.** The v2 path used a SHA-256 hash derived from the
query text; v3 runs **two** real measurements through the
configured provider — unprefixed query and
`search_query:`-prefixed query. Both query embeddings come from the
same provider, document vectors are identical across the two
conditions, and cosine similarity is computed on L2-normalized
vectors (so a 2x scale on a doc cannot jump the ranking). The
runner now refuses to silently fall back to the hash path when
`embedding_profile` is supplied and the embed call fails; the
exception propagates. The report now shows unprefixed metrics,
prefixed metrics, and the Δ between them per
(Precision@k, Recall@k, MRR, nDCG@k). New tests
`TestRawCosinePrefixComparison`,
`TestRawCosineDocVectorNormalized`,
`TestRawCosineRefusesHashInRealRun` cover the three sub-priorities.

**Priority 5 — corpus and scoring correctness.** The committed
v1 corpus uses sequential literal IDs (`d0000001`..`d0000030`) for
human cross-referencing; the hash helper remains the documented
fallback. `load_corpus` now refuses: (a) duplicate `doc_id`s, (b)
a positive query with empty `relevant_doc_ids`, and (c) an
`expectations.json` whose keys disagree with `queries.json`'s
`relevant_doc_ids` as a set. The scoring top-k is read from
`corpus.scoring.top_k`, not from the operator's config. The MRR /
nDCG / Recall / Precision work from v2 is preserved unchanged.
New tests: `TestCorpusIDUniqueness`,
`TestPositiveQueryMustHaveRelevantDocIds`,
`TestExpectationsMatchQueries`, `TestScoringTopKFromCorpus`,
`TestExpectationsSetEqualToQueries`.

**Priority 6 — empty results are empty, not failures.** MPM
occasionally returns `rc=0` with the plain-text message
`No memories found.`. v3 treats that as a successful empty
result (`QueryResult(search_mode="hybrid_empty", rows=())`).
Malformed responses and non-zero exits still raise. New tests
`TestEmptyPlainTextResult` / `TestMalformedResponseStillRaises`
prove the boundary.

**Priority 7 — regression coverage.** The v3 adversarial suite has
15 new test methods across 14 classes (total 27 in
`test_adversarial.py`); every one of them would fail against the
v2 implementation. The full bench suite is now 94 tests (the
non-vacuity floor is 50). The bench tests do not open or modify
any path under `Path.home() / ".mpm"`; the `protected_root=`
parameter is what makes this possible.

### Reported without code change (scope discipline)

- **TestHITLRotation_AgeTrigger** (release-gate order-dependent
  failure observed in earlier CI history): re-ran in isolation
  under three independent selectors (`-run TestHITLRotation_AgeTrigger`,
  `-run TestHITL*`, `-count=3`) and the test passes every time on
  the worktree's current HEAD. The test uses `t.TempDir()` for
  isolation and an injected clock; no shared state is reachable
  across the suite. A future reproduction of the failure should
  capture the seed/race output rather than retry the same flag set.
- **Historical reproducibility**: still unverifiable from the lost
  30-query corpus. The harness ships its own synthetic corpus
  under `bench/corpus/v1/`. The 22-query qwen3 vs all-minilm
  comparison in `docs/EMBEDDING-MODEL-COMPARISON.md` remains
  unreproducible from inside this repository.
