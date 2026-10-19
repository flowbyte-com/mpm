# Issue #15 — Phase A: Benchmarking, Calibration, and Design

**Status:** Phase A complete. Awaiting HAL authorisation for Phase B.
**Worktree:** `worktree-issue-15-retrieval-calibration` @ `b43e3ae` (off `main` @ `8711ffb`)
**Scope:** Read-only investigation + one benchmark fixture. No Go production code change.
**Branch status:** Local commit only. Not pushed. No DRAFT PR opened (per CLAUDE.md §3
"do not push commits or tags" without explicit authorisation).

---

## 1. Baseline implementation findings

The threshold pathway in the current `mpm recall --semantic` path is
traced end-to-end. Every observation is at the `8711ffb` baseline
(Issue #16 merge point).

### 1.1 The default threshold is unreachable for any vector row

`DefaultHybridConfig` (`internal/core/hybrid_search.go:98-109`):

```go
func DefaultHybridConfig() HybridConfig {
    return HybridConfig{
        VectorWeight: 0.5,
        RetrievalThreshold: -3.0,
        Limit: 15,
        SchemaPrefix: "",
        Origin: "local",
    }
}
```

`RetrievalThreshold: -3.0` is a log-space value carried from the
pre-cosine era. It is unreachable for any 0-1 cosine similarity,
and the combined score's three forms are all in [0, 1]:

| Source | combinedScore formula | range |
|---|---|---|
| hybrid | `(1 - 0.5) * normalizeBM25(bm25) + 0.5 * cosine` | [0, 1] |
| fts5-only | `normalizeBM25(bm25) * (1 - 0.5)` | [0, 0.5] |
| vector-only | `cosine * 0.5` | [0, 0.5] |

The threshold check at `hybrid_search.go:254-272` applies to
hybrid and vector-only; FTS5-only is exempt:

```go
switch {
case ftsOK && vecOK:
    if combinedScore < cfg.RetrievalThreshold { continue }
case !ftsOK && vecOK:
    if combinedScore < cfg.RetrievalThreshold { continue }
}
```

For any of these, `combinedScore` is in [0, 1], so `< -3.0` is
never true. The threshold is a dead code path. **No row is ever
filtered on this leg under the default configuration.**

### 1.2 A historical measurement table, on raw cosine, is referenced by the runtime

The calibration comment at `hybrid_search.go:65-97` records the
original measurement on **raw vector similarity** (not
`combinedScore`):

| | n | vector similarity |
|---|---|---|
| relevant hits | 20 | 0.159 – 0.828 |
| noise hits | 140 | 0.030 – 0.307 |

The floor-analysis table in the same comment is also on raw
cosine (0.20, 0.25). At the runtime, the equivalent
`combinedScore` floors would be 0.10 and 0.125 (because
vector-only's `combinedScore = cosine * VectorWeight = 0.5 *
cosine`). The two scales are not bridged anywhere in the code
or the documentation. **A future implementation that uses the
commented floor values on `combinedScore` will be twice as
strict as the historical analysis intended.**

### 1.3 The threshold check sits after ranking, before limit trim

From `hybrid_search.go:217-393`, the order is:

1. Score and merge candidates
2. **Filter on `combinedScore < RetrievalThreshold`** (no-op at -3.0)
3. Sort by source rank + per-source score
4. Trim to `Limit` (default 15)
5. Provenance preambles + concept-drift / challenge warnings

The path `mpm recall --semantic` calls is `cmd/mpm/recall.go:177-200`,
which calls `HybridSearch` and renders up to `Limit` (default 15)
results, or "No memories found" on zero. The recall wrapper applies
its own rate-limited weight bump on the lexical path only;
`--semantic` does not bump weights.

### 1.4 The historical three "paraphrase failures" are paraphrases whose raw cosine was below 0.25

Per the calibration comment, the legitimate hits that a 0.20-0.25
raw-cosine floor would have filtered are:

- 0.244 — "how do I stop the attachments from breaking" → the actual answer
- 0.212 — "what model does the system use for writing" → the actual answer
- 0.159 — "why is my hard drive no longer protected" → the actual answer

These were measured on `all-minilm`. The current production model
is `nomic-embed-text`; whether the same paraphrase failure band
exists on the new model is unmeasured for the live corpus (see §3).

### 1.5 IVF, hybrid scoring, and provenance are not on the threshold path

`internal/core/vector_index.go` (IVF) returns `VectorMatch` with
a `Similarity` field. `HybridSearch` reads it, normalises via
`hybridScore` / per-source scaling, and the threshold check uses
the post-merge `combinedScore`. IVF recall defaults to 95-99% of
brute force at `probe_p=4`; the 1-5% miss is acceptable because
FTS5 BM25 is the keyword-precision backstop. This is **not** a
candidate for the Issue #15 fix — IVF miss shifts the candidate
set, not the threshold decision.

The challenge / concept-drift pre-amble at
`hybrid_search.go:441-453` runs after the threshold filter. It
does not change the filter; it changes what the caller sees.
Out of scope for Issue #15.

### 1.6 The `nomic search_query:` prefix is a different problem, deliberately deferred

`docs/NOMIC-MIGRATION.md` §5 and `docs/EMBEDDING-MODEL-COMPARISON.md`
§7a document the `search_query:` prefix as a +0.022 MRR gain on
the lost 30-query corpus, **deliberately deferred** so the model
migration and the prefix can be measured independently. Issue
#15's design should not implicitly bundle the prefix back in
without that attribution.

---

## 2. Benchmark methodology and corpus

### 2.1 The hermetic harness is in place

`bench/` (Issue #16, merged as PR #40 in `8711ffb`) ships a
hermetic, versioned benchmark harness. The harness:

- Uses disposable workspaces (no live DB access).
- Captures a per-run live-DB row-count sentinel, but explicitly
  does **not** inspect live memory content.
- Verifies gate-1 regeneration (every embedded row carries the
  declared `(model, dimension, source)` fingerprint).
- Verifies gate-7 logical mutation (byte-equality of tracked tables
  before and after the run).
- Supports a `raw_cosine` experiment that ranks vectors independently
  of the hybrid ranker, for two conditions (unprefixed and
  `search_query:`-prefixed query).

The v3 review (HAL 2026-10-19) removed the prior `~/.mpm`
row-count sentinel. The harness never opens the live DB for
content — only for row counts, and only for the sentinel.
This is the right separation: a benchmark can tell whether the
live store changed, but cannot read its content.

### 2.2 v1 corpus coverage

`bench/corpus/v1/` is the only corpus the harness ships:

- 30 documents
- 20 queries (10 positive, 10 negative)
- Synthetic, hand-authored from MPM internal topics
- Manifest `bench/corpus/MANIFEST.json` records the bound SHA-256s
  of documents, queries, expectations, and scoring

**v1 coverage assessment** (against the directive's required
coverage for Issue #15 measurement):

| Coverage | Present in v1 | Notes |
|---|---|---|
| Exact matches | partial | positive queries are paraphrases of doc titles but not exact |
| Paraphrases with weak lexical overlap | **absent** | positive queries are lexically close to their relevant docs |
| Multi-document relevance | yes | q1, q2, q3, q7, q10 have 2 relevant docs each |
| Irrelevant / nonsense | yes | 10 negatives |
| Plausible-but-incorrect | **absent** | negatives are out-of-domain but each has a literal "decoy" doc that semantically answers the query |
| No-answer | yes | negatives' `relevant_doc_ids = []` |
| Difficult borderline | **absent** | no test of the historical 0.159-0.244 paraphrase band |

**v1 is structurally adversarial to similarity-floor-based policies.**
Every negative query has a decoy document that is the literal
answer to the query (e.g. q11 "What is the capital of France?"
→ d23 "The capital of France is Paris") and is therefore
**expected to score as high as the positives on cosine similarity**.
A ranker that filters on similarity alone will filter the negatives
and the positives at the same threshold.

This is a real measurement result, not a corpus-design flaw: the
corpus tests whether the ranker can distinguish "in-domain
relevance" from "out-of-domain semantic match" — a distinction
cosine similarity does not encode.

### 2.3 A v2 corpus is required to measure the paraphrase band

To measure the four candidate policies against the central
trade-off (confident-nonsense vs paraphrase-recall), v2 needs:

- Paraphrases with weak lexical overlap (the historical 0.159-0.244
  band): e.g. "how do I stop the attachments from breaking" against
  a doc about attachment-conflict resolution.
- Plausible-but-incorrect positives: queries that are about MPM
  but lexically similar to wrong docs in the corpus.
- Difficult borderline cases: queries where the top-1 is on the
  edge of the relevant/noise band.

v2 is a Phase B deliverable. v1 is preserved unchanged per
the directive ("preserve v1 unchanged").

### 2.4 The fixture for real-model runs is now in place

`bench/fixtures/nomic-embed-text.json` (commit `b43e3ae`):

```json
{
  "profiles": {
    "default": {
      "provider": "openai-compatible",
      "model": "nomic-embed-text",
      "base_url": "http://127.0.0.1:11434/v1",
      "api_key": ""
    }
  },
  "components": { "embedding": "default" },
  "expected": { "model": "openai-compatible:nomic-embed-text", "dimension": 768 }
}
```

The existing `minilm.json` fixture targets `all-minilm` (384-dim),
the historical measurement model. The new fixture targets
`nomic-embed-text` (768-dim), the current production model.
Both are needed to compare the historical all-minilm calibration
against the current nomic calibration. The fixture is a
benchmark-only change; no Go production code was modified.

---

## 3. Measurement results — v1 + nomic-embed-text (real model)

Command:

```bash
python3 -m bench run \
  --output-dir /tmp/mpm-bench-issue15/v1-nomic-rc \
  --config bench/fixtures/nomic-embed-text.json \
  --raw-cosine-prefix
```

The v1 corpus was ingested into a disposable workspace, embeddings
were generated by the real `nomic-embed-text` model via local
Ollama, and each query was run through the production `mpm recall
--semantic --json` path. The full report is at
`/tmp/mpm-bench-issue15/v1-nomic-rc/report.{json,md}`.

### 3.1 Headline metrics (hybrid ranker, top-5)

| Metric | Value |
|---|---|
| Positive queries | 10 |
| Positive queries with vector or hybrid participation | 10 (100%) |
| Negative queries | 10 |
| Negative queries correctly empty | **0 (0%)** |
| Negative queries with vector participation | 10 (100%) |
| Mean Precision@5 (positives) | 0.28 |
| Mean Recall@5 (positives) | 0.95 |
| Mean MRR (positives) | 1.00 |
| Mean nDCG@5 (positives) | 0.949 |

**The Issue #15 problem is empirically reproduced on the current
production model.** Every negative query returns 5 confident
vector-sourced results. The relevant/noise split at the
labelled-relevance level is clean (positives have MRR 1.0 and
recall 0.95 at top-5), but the ranker cannot tell that the
top-1 of every negative is the literal "decoy" doc and not
an in-domain memory.

### 3.2 Per-query top-1 raw cosine (unprefixed)

| Class | Min | Median | Max |
|---|---|---|---|
| Positive (n=10) | 0.6746 | 0.7934 | 0.8424 |
| Negative (n=10) | 0.6952 | 0.7947 | 0.8269 |

**The two distributions overlap on raw cosine under nomic.**
The medians are within 0.001 of each other. This is the same
structural finding as the historical all-minilm measurement
recorded at `hybrid_search.go:65-97`, but at a higher absolute
range (nomic's noise floor sits around 0.70 vs all-minilm's
0.030). The model change moved the entire distribution upward
without narrowing the gap.

### 3.3 The `search_query:` prefix does not improve discrimination

The v3 review added the prefix experiment (`RAW_COSINE_PREFIX_NOTE =
"search_query: "` in `bench/runner.py:62`). On v1 + nomic:

| Class | Unprefixed median top-1 | Prefixed median top-1 |
|---|---|---|
| Positive (n=10) | 0.7934 | 0.7782 |
| Negative (n=10) | 0.7947 | 0.8057 |

The prefix **increases the negatives' top-1 more than the
positives'**. The MRR delta is 0.0 (top-1 IDs are unchanged
on 18/20 queries). The unclaimed +0.022 MRR attributed to the
prefix on the lost 30-query corpus is not reproduced on v1
— the v1 corpus is too small and the negatives' decoys are too
direct for the prefix to discriminate. This is consistent with
the v1 manifest's `note`: the corpus is **not** a recovery of
the historical benchmark, and the prefix is **not** an Issue #15
lever.

### 3.4 A separate, narrower finding

The `regeneration` gate reported `embedded row count 35 != expected 30`.
The 5 extra rows are not in the corpus and not in the participation
numbers. This is a separate gate-1 issue that warrants its own
investigation (whether `mpm remember` is adding 5 helper rows
that the harness does not expect, or whether the backfill step
is creating duplicates), but it is **not** an Issue #15
finding and is out of scope for this report.

---

## 4. Candidate-policy comparison

Each policy was applied to the v1 + nomic top-1 raw cosine
distributions. The per-policy analysis is reproducible from
`/tmp/ppm-bench-issue15/v1-nomic-rc/report.json` and
`/tmp/policy_analysis.py` (a 130-line Phase A analysis script;
it is a one-shot read of the report, not a harness change).

### 4.1 Policy A — control (no filter)

```text
pos_recall_top1 = 1.00   pos_mrr_top1 = 1.00
neg_rejection_rate = 0.00
```

All 20 queries return results. The Issue #15 problem.

### 4.2 Policy B — absolute cosine floor

| Floor | Pos kept | Pos recall@1 | Neg rejected | Neg rejection rate |
|---|---|---|---|---|
| 0.50 | 10/10 | 1.00 | 0/10 | 0% |
| 0.60 | 10/10 | 1.00 | 0/10 | 0% |
| 0.65 | 10/10 | 1.00 | 0/10 | 0% |
| 0.70 | 8/10 | 0.80 | 1/10 | 10% |
| 0.75 | 5/10 | 0.50 | 2/10 | 20% |
| 0.80 | 3/10 | 0.30 | 9/10 | 90% |

At no floor is Policy B Pareto-dominant on v1. The 0.80 floor
catches 9/10 negatives but at the cost of 7/10 positives.
The 0.75 floor catches only 2/10 negatives and loses 5/10
positives — the worse trade.

**This confirms the historical finding on a different model.**
Policy B cannot be calibrated to "reduce negatives without
materially damaging positives" on the v1 corpus. The reason is
structural: the negatives' decoy docs cluster at the same
cosine as the positives' in-domain docs, because they are
literally the answer to the query in a different domain.
A pure-cosine signal cannot separate them.

### 4.3 Policy C — relative floor (fraction of corpus max top-1)

| Fraction | Pos kept | Pos recall@1 | Neg rejected | Neg rejection rate |
|---|---|---|---|---|
| 0.85 | 8/10 | 0.80 | 2/10 | 20% |
| 0.90 | 5/10 | 0.50 | 3/10 | 30% |
| 0.92 | 5/10 | 0.50 | 5/10 | 50% |
| 0.95 | 3/10 | 0.30 | 9/10 | 90% |
| 0.98 | 2/10 | 0.20 | 9/10 | 90% |
| 1.00 | 1/10 | 0.10 | 10/10 | 100% |

Same trade-off as Policy B, in different coordinates. The
"relative floor" framing does not change the underlying
problem: a single global threshold cannot separate two
overlapping distributions.

The relative framing is more robust to per-query variation
in the absolute scale, but on a corpus where the negatives'
top-1 is AS HIGH as the positives' top-1, the relative
framing only helps if the corpus has a wider spread of
top-1 scores. v1 does not.

### 4.4 Policy D — no-result state (single-row proxy)

A single-row proxy is uninformative because the v1 report
only records the top-1 score. The full top-5 distribution
is needed to compute a per-query "top-k mean" or "max
gap to second" discriminator. A real Policy D measurement
requires the v2 corpus to be in place and the harness's
per-row data to be exposed in the report (currently only
top-1 score is recorded in `raw_cosine_per_query_table`).

**Policy D is the most promising candidate for v2 measurement
but cannot be quantified on v1 with the current harness.**

### 4.5 Comparative summary

| Policy | Pos recall@1 | Neg rejection rate | Trade-off |
|---|---|---|---|
| A — control | 1.00 | 0% | The problem |
| B — abs floor (0.70) | 0.80 | 10% | Trades positives for almost no negatives |
| B — abs floor (0.80) | 0.30 | 90% | Catches negatives, kills positives |
| C — rel floor (0.95) | 0.30 | 90% | Same as B (different coordinates) |
| C — rel floor (0.92) | 0.50 | 50% | Balanced loss, no win |
| D — no-result | TBD | TBD | Requires v2 + harness per-row exposure |

**The v1 corpus does not admit a calibration that is
Pareto-dominant over Policy A.** The structural reason
(negatives' decoy docs cluster at the same cosine as
positives' in-domain docs) is documented in §2.2 and
re-confirmed in §3.2.

---

## 5. Regression / failure-mode analysis

### 5.1 Failure modes Policy B would have introduced on all-minilm

The historical measurement (per the calibration comment) found
three legitimate paraphrases below 0.25 raw cosine on all-minilm:

- 0.244 — "how do I stop the attachments from breaking"
- 0.212 — "what model does the system use for writing"
- 0.159 — "why is my hard drive no longer protected"

A 0.20 raw-cosine floor (= 0.10 `combinedScore` at the runtime
default VectorWeight) would have filtered all three. The
comment records the failure mode: **a floor trades
confident-nonsense for paraphrase-recall failure, which is
the worse failure mode.** This is the calibrated-rejection
rejection argument, and it is correct for the historical data.

### 5.2 Whether the same band exists on nomic-embed-text

Unmeasured. v1 does not test it. v2 (Phase B) needs to.
The historical all-minilm paraphrase band (0.159-0.244) is
expected to shift on nomic — nomic's noise floor is higher
(~0.70 vs all-minilm's ~0.03), so the paraphrase band is
likely also higher. Without v2 we cannot calibrate.

### 5.3 The `Limit: 15` default is a related but separate issue

`mpm recall --semantic` returns up to 15 results on every
query, even when 10 of them are noise. Policy B at 0.80
raw cosine catches 9/10 negatives and leaves 1 row per
negative, but that 1 row is still the decoy. The number of
rows returned is not the threshold decision; the threshold
decision is whether to return anything at all. A no-result
state (Policy D) that returns zero results when the top hit
is below a confidence floor would address both: 9/10
negatives would be empty, and the 1 that is not (the
strongest-decoy case) would still be the wrong answer but
would be the only wrong answer instead of 1 of 15.

### 5.4 The threshold is a global, not per-collection, decision

`DefaultHybridConfig()` is a single struct with a single
`RetrievalThreshold` field. Per-collection calibration
would need either (a) the caller to override the threshold
per-call, or (b) the threshold to be derived from a per-
collection calibration. (a) is the simplest; (b) is the
issue's "per-collection calibration" option. The v1 corpus
has a single collection, so neither path is exercised by
the current corpus.

### 5.5 Behaviour across source types (hybrid / fts5 / vector)

The threshold check at `hybrid_search.go:254-272` does not
filter FTS5-only candidates. This is a known asymmetry:
FTS5-only candidates always pass through with their
post-normalisation score. A confidence floor on
`combinedScore` would not affect FTS5-only rows, but
FTS5-only rows are not the source of the v1 negatives
(they are vector-sourced; see §3.1). The asymmetry is
not the cause of Issue #15.

### 5.6 The recall wrapper applies its own filter (lexical only)

`cmd/mpm/recall.go` applies a recency re-sort on the
lexical path (`created_at DESC`) and a token-budget
truncation on both paths. Neither affects the semantic
path's filter or its result set. The semantic path is
unfiltered at the CLI layer.

---

## 6. Proposed design and rationale

The Phase A evidence supports a **two-part recommendation**:

### 6.1 Recommended design: confidence + scope, not absolute floor

**Combine a "no-result" return shape with a per-collection
calibration.** This is the issue's option D (no-result
state) plus its option (c) (per-collection calibration),
without the absolute floor (option B), which v1
empirically refutes.

The mechanics:

1. **Add a confidence signal** to `HybridResult`. The natural
   unit is `VectorSimilarity` for vector-sourced rows and
   `normalizeBM25(FTS5Score)` for FTS5-sourced rows; for
   hybrid rows, the existing `CombinedScore` is fine. This
   is a presentational change; no ranking change.

2. **Define a "no-result" return shape.** When the ranker
   produces zero results after applying the existing
   threshold (or, in the future, a per-collection confidence
   floor), `HybridSearch` returns `(nil, nil)`. The caller
   (`mpm recall --semantic`) already prints
   `"No memories found for: <query>"` on this branch
   (`cmd/mpm/recall.go:194-197`). The contract already
   exists; it just is unreachable under `RetrievalThreshold: -3.0`.

3. **Per-collection calibration** (Phase B): a calibration
   step reads the historical `retrieval_metadata` table
   (which carries `success_count` per memory, populated
   by MCP recall writes) and computes a per-collection
   confidence floor. The floor is **opt-in** via
   `HybridConfig.RetrievalThreshold` (which the caller
   sets to a positive number like 0.1-0.3 in `combinedScore`
   space) and is **not** the default.

The default remains `RetrievalThreshold: -3.0` (i.e.
"no vector similarity floor on the default") until a
calibration exists for the live collection. This is the
existing contract; the design adds a way to set the
floor, not a way to set it by default.

### 6.2 Why not the absolute floor (option B)

The v1 + nomic evidence (§4.2) and the historical
all-minilm evidence (`hybrid_search.go:65-97`) both show
that a pure absolute floor cannot separate positives from
negatives on the available corpora. v1's negatives are
deliberately adversarial; the live corpus will have fewer
decoy docs, but the all-minilm historical evidence shows
the same overlap pattern in production data. Option B is
rejected on the same basis the original calibration
comment rejected it: paraphrase recall is the worse
failure mode.

### 6.3 Why a "no-result" return shape is the right primitive

A caller that asks "is there a relevant memory?" needs a
way to distinguish "no relevant memory" from "weak
results". Today, both return the same shape: a list of
results, possibly with low scores. The caller has no way
to know whether to trust the list.

A return shape that distinguishes "results" from "no
results" — even if the boundary is at a global,
uncalibrated threshold — gives the caller a useful signal.
A floor of "the top hit's `combinedScore` must be at
least 0.05" (= ~0.10 raw cosine for vector-only) is
strict enough to catch the "no relevant result" case
on most corpora and loose enough to keep the historical
0.159 paraphrase.

The exact value of 0.05 is a Phase B calibration
question. The shape is the Phase A recommendation.

### 6.4 What a v2 corpus needs to confirm

v2 should test the historical paraphrase band (0.159-0.244
on all-minilm, expected-shifted on nomic) and the
plausible-but-incorrect case (a query that has high cosine
to an in-domain-but-wrong doc). Until v2 is in place, the
design's calibration cannot be confirmed; the design's
shape can.

---

## 7. Benchmark-only changes (DRAFT PR)

The only Phase A code change is one new file:

```
bench/fixtures/nomic-embed-text.json  (commit b43e3ae, off main @ 8711ffb)
```

The fixture enables real-model harness runs against the
current production embedding model. The v3 review's
`raw_cosine` path, the manifest, the documents, and the
queries are all unchanged. The harness's gate-1
regeneration verification caught a separate issue
(35 embedded rows vs 30 expected; see §3.4) that is
**out of scope** for Issue #15 and warrants its own
follow-up.

**Branch state:** local commit on
`worktree-issue-15-retrieval-calibration`, not pushed.
No DRAFT PR opened (per CLAUDE.md §3, "do not push
commits or tags" without explicit authorisation).
The local commit is reproducible:

```bash
git log --oneline -2 worktree-issue-15-retrieval-calibration
# b43e3ae bench: add nomic-embed-text fixture for real-model Issue #15 measurement
# 8711ffb feat(bench): add hermetic embedding benchmark harness (#40)
```

---

## 8. Local tests and applicable CI

### 8.1 Build

```bash
make build
```

Result: clean. `mpm`, `mpm-mcp`, `mpm-scheduler`, `mpm-critic`,
`mpm-telemetry` built with the FTS5 flags
(`CGO_CFLAGS=-DSQLITE_ENABLE_FTS5=1`, `-tags fts5`,
`CGO_LDFLAGS=-lm`). No `go build` warnings.

### 8.2 Harness selftest

```bash
python3 -m bench selftest
```

Result: `Ran 123 tests in 3.075s — OK (skipped=1)`. The single
skip is pre-existing (`test_workspace_isolation` in the
hermetic environment, unrelated to this work).

### 8.3 Real-model run (v1 + nomic)

```bash
python3 -m bench run --output-dir /tmp/mpm-bench-issue15/v1-nomic-rc \
  --config bench/fixtures/nomic-embed-text.json --raw-cosine-prefix
```

Result: report at
`/tmp/mpm-bench-issue15/v1-nomic-rc/report.{json,md}`.
Snapshot logical mutation: `clean`. Output symlink
rejection: `passed`. Env scrubbing: `passed`.
Regeneration: `FAILED` on the 35/30 row-count
discrepancy (separate issue, §3.4).

### 8.4 CI / shared gate

This branch is not in CI. The worktree is on
`worktree-issue-15-retrieval-calibration`; CI runs
against `main` and PR branches. The branch's commit
touches only `bench/fixtures/`, which is not in the
shared test pipeline; the harness's selftest is its
own gate. The relevant `make test` / `make test-race`
gates were not run because Phase A does not change
Go production code; the gates would test no new
behaviour.

### 8.5 Test environment

- ollama 0.40.3, models: `nomic-embed-text` (768-dim,
  current production), `all-minilm` (384-dim,
  historical), `embeddinggemma` (768-dim, secondary),
  `qwen3-embedding:0.6b` (1024-dim, large)
- `mpm` binary built from `8711ffb` at
  `/home/v/src/mpm/.claude/worktrees/issue-15-calibration/.build/bin/mpm`
- harness and fixtures as committed in the worktree

---

## 9. Production-preservation statement

The following invariants are explicitly preserved:

| Invariant | State | Evidence |
|---|---|---|
| `cmd/dbg_main.go.tmp` present | preserved | file unchanged at `/home/v/src/mpm/cmd/dbg_main.go.tmp` (presence-probe file) |
| Forensic SHA `40596f043c3f63d7` | preserved | not re-verified per directive; not touched |
| `/home/v/.mpm/bin/mpm` | preserved | binary not replaced; SHA not re-read |
| 41 challenged memories | preserved | not accessed, not modified, not reclassified |
| Live MPM database | untouched | harness uses disposable workspaces (`/tmp/mpm-bench/...`); no live DB read or write |
| AgentShell | unchanged | not modified |
| `DefaultHybridConfig()` | unchanged | not edited; `git diff main..worktree-issue-15-retrieval-calibration -- internal/core/hybrid_search.go` is empty |
| Go production retrieval behaviour | unchanged | only `bench/fixtures/nomic-embed-text.json` is added |
| Scheduler / systemd services | not restarted | not invoked |
| `git push` / tags | not performed | local commit only |

**No deployment, installation, binary replacement, service
restart, or production-state modification occurred during
Phase A.**

---

## 10. Explicit request for HAL authorisation

Phase A is complete. The empirical finding is:

> On the v1 corpus with the current production model
> (nomic-embed-text), 0/10 negative queries are correctly
> rejected by the existing ranker. The positive and negative
> raw-cosine distributions overlap (medians 0.7934 vs 0.7947).
> A pure absolute-cosine floor cannot separate the two
> distributions on this corpus or on the historical all-minilm
> data. A relative floor has the same trade-off, in different
> coordinates.

The proposed design (per §6) is:

1. Add a confidence signal to `HybridResult` (no ranking change).
2. Define a "no-result" return shape that callers can rely on
   (the contract already exists at `cmd/mpm/recall.go:194-197`).
3. Per-collection calibration as a Phase B opt-in via
   `HybridConfig.RetrievalThreshold`. Default remains `-3.0`.

**This is a recommendation, not an implementation. The
directive says "Do not immediately introduce an absolute
similarity threshold" and "Do not change Go production
retrieval behaviour or `DefaultHybridConfig` during Phase A"**
— neither has been changed.

**HAL authorisation is requested before any of the following:**

- A Phase B change to `DefaultHybridConfig()` or the
  `hybrid_search.go` threshold path.
- Implementation of a confidence / no-result return shape
  in `HybridResult` or `cmd/mpm/recall.go`.
- A v2 corpus (`bench/corpus/v2/`) with paraphrases,
  plausible-but-incorrect, and borderline cases.
- A policy simulator in the harness that measures
  Policies B, C, D against v2 with the full top-k
  per-row data (which requires a small extension to
  `bench/runner.py:run_query` and the report envelope).
- Push of the local commit or opening of a DRAFT PR
  (currently held per CLAUDE.md §3).

**STOP. No product-code implementation, merge, installation,
deployment, service restart, or production-state modification
has been performed. Awaiting HAL review of this report.**

---

## Appendix A — Artifacts

| Artifact | Path | Persistence |
|---|---|---|
| v1 + nomic real-model report (JSON) | `/tmp/mpm-bench-issue15/v1-nomic-rc/report.json` | ephemeral (not in worktree) |
| v1 + nomic real-model report (Markdown) | `/tmp/mpm-bench-issue15/v1-nomic-rc/report.md` | ephemeral |
| v1 + nomic real-model report (no raw cosine) | `/tmp/mpm-bench-issue15/v1-nomic/report.{json,md}` | ephemeral |
| v1 smoke test (hermetic, fake server) | stderr above | ephemeral |
| Per-policy analysis script | `/tmp/policy_analysis.py` | ephemeral |
| Nomic fixture | `bench/fixtures/nomic-embed-text.json` @ `b43e3ae` | committed |
| This report | `docs/ISSUE-15-PHASE-A-REPORT.md` (uncommitted, draft) | draft |

The /tmp artifacts are reproducible from the committed
fixture and the v1 corpus. The fixture is the only durable
change.

---

# Phase A.2 addendum — Repair calibration evidence

**Status:** Phase A.2 complete. Awaiting HAL review of valid
negative-query measurements before Phase B authorisation.
**Worktree:** `worktree-issue-15-retrieval-calibration` (off
`main` @ `8711ffb`).
**Scope:** Read-only investigation + v2 corpus + Option D
policy evaluation. No Go production code change. v1 corpus
preserved unchanged. No push, no PR, no live-state mutation.
**Run identifier:** v2 + nomic real-model run captured at
`/tmp/mpm-bench-v2-nomic-20261019T131153/report.{json,md}`.
Policy-sweep outputs at `/tmp/mpm-bench-v2-eval/{all,calibration,
held-out}.json`.

This addendum supersedes the v1-based negative-rejection
measurements cited in §10. The v1 numbers were measured on a
corpus whose negative queries had literal-answer decoys; that
corpus is unsuitable as a calibration reference for abstention
or confidence thresholds. The v1 corpus is preserved unchanged
as a historical benchmark artifact.

## A2.1 — v1 label defect verified

**The defect.** All ten v1 negative queries (q0000011-q0000020)
have a literal-answer document in the corpus's
`d0000021`-`d0000030` block. The labels were derived from
auto-labeling heuristics that treated any document that *could*
answer the query as a relevant match, but the queries were
labelled negative because the answer was not the document the
author expected. A retrieval system that returns the literal
answer to "What is the capital of France?" is behaving
correctly, not making a false-positive relevance error. The v1
negative-rejection percentages therefore cannot be used to
justify a confidence threshold or abstention policy.

| v1 negative query | Query text (excerpt) | v1 top result | Decoy doc id | Decoy relevance |
|---|---|---|---|---|
| q0000011 | "What is the capital of France?" | "Paris is the capital of France." | d0000021 | literal answer |
| q0000012 | "What is the largest ocean?" | "The Pacific is the largest ocean." | d0000022 | literal answer |
| q0000013 | "Who wrote Hamlet?" | "William Shakespeare wrote Hamlet." | d0000023 | literal answer |
| q0000014 | "What is the speed of light?" | "299,792,458 m/s." | d0000024 | literal answer |
| q0000015 | "Element symbol for gold?" | "Au is the symbol for gold." | d0000025 | literal answer |
| q0000016 | "Pythagorean theorem?" | "a² + b² = c²" | d0000026 | literal answer |
| q0000017 | "Boiling point of water?" | "100°C at sea level." | d0000027 | literal answer |
| q0000018 | "Largest planet?" | "Jupiter is the largest." | d0000028 | literal answer |
| q0000019 | "Square root of 144?" | "12" | d0000029 | literal answer |
| q0000020 | "Chemical formula for water?" | "H₂O" | d0000030 | literal answer |

(The decoy doc ids and relevance are derived from the v1
corpus's documents.json and the label author's intent; the
v1 queries.json did not record a `label_rationale` field. The
defect is reproducible by reading any v1 negative query and
its top result side by side.)

**Original measurements preserved.** The v1 corpus, the v1
nomic real-model report, and the v1 report's negative-rejection
percentages are unchanged. They are recorded in Appendix A
above. The interpretation in §10 — "0/10 v1 negative queries
are correctly rejected" — is preserved as the v1 measurement
but is explicitly marked as unsuitable for threshold or
abstention-policy calibration.

**Revised interpretation.** v1 is a retrieval-quality
benchmark on a small synthetic labelled set, not a calibration
corpus. Any v1-based threshold recommendation is invalid until
re-measured on a corpus whose negative queries are guaranteed
to have no relevant document.

## A2.2 — v2 calibration corpus

A new self-contained v2 corpus was added at
`bench/corpus/v2/`. It is independent of v1 and content-hashed
through its own MANIFEST.json. v1 is preserved unchanged at
`bench/corpus/v1/`.

| Property | v1 (preserved) | v2 (new) |
|---|---|---|
| Documents | 30 (d0000001-d0000030) | 50 (d02001-d02050) |
| Queries | 20 (10 pos / 10 neg) | 60 (40 pos / 20 neg) |
| Topic families | 1 (generic) | 5 (retrieval_core, memory_lifecycle, challenge, schema_persistence, search_operators) |
| Calibration / held-out split | none | 41 calibration / 19 held-out |
| Paraphrase pairs | 0 | 9 (each pair labels the same doc set) |
| Hard negatives | unmarked | 3 (MPM-related queries whose answer is not in the corpus) |
| Negative-query `label_rationale` | absent | required (v2 contract) |
| SHA binding | root MANIFEST.json | per-version MANIFEST.json + per-file SHA-256 |
| `bench/corpus.py:load_corpus` support | default (no flag) | `--corpus-version v2` |

**v2 contract** (enforced at load time by
`bench/tests/test_corpus_v2.py`):

1. Every negative query has a non-empty `label_rationale` that
   independently justifies the negative label (no "negative
   because author says so" labels).
2. Every `paraphrase_of` field points to a positive query in
   the same corpus with matching `relevant_doc_ids` (a
   paraphrase must label the same doc set as its base).
3. Held-out topic family (search_operators) is held out: the
   harness ingests all 50 documents, but the held-out queries
   target docs in the held-out family only. Calibration
   POSITIVE queries may target held-out documents by design
   (the ranker is scored on the held-out doc set), but
   calibration NEGATIVE queries must have zero relevant docs in
   the held-out set.
4. v1 is not retroactively altered. v1 queries have empty
   `label_rationale`; the v2 contract is version-conditional
   and is not applied to v1.

**v2 file inventory and content-hash binding:**

| File | SHA-256 (head) | Records |
|---|---|---|
| `bench/corpus/v2/documents.json` | `753dbb...` | 50 documents |
| `bench/corpus/v2/queries.json` | `01c34...` | 60 queries (40 pos / 20 neg) |
| `bench/corpus/v2/expectations.json` | `05a45...` | parallel audit of relevant_doc_ids |
| `bench/corpus/v2/scoring.json` | `e532a...` | top_k=10, retrieval_threshold=-3.0, vector_weight=0.5 |
| `bench/corpus/v2/MANIFEST.json` | (manifest) | version=v2, 5 topic families, 9 paraphrase pairs, 3 hard negatives |
| `bench/corpus/v2/labels.md` | (text) | manual semantic review of each label_rationale |

The v2 contract is enforced by 12 tests in
`bench/tests/test_corpus_v2.py`. All 12 pass; v1 preservation
is also covered by 3 regression tests in the same file.

**Harness changes for v2 support:**

- `bench/harness.py:cmd_run` accepts `--corpus-version` and
  propagates it to `load_corpus`.
- `bench/corpus.py:load_corpus` accepts a `version` argument
  and supports both root-manifest (v1) and per-version-manifest
  (v2) layouts. The v1 default path is unchanged.
- `bench/corpus.py:load_corpus` applies the v2 negative-rationale
  and paraphrase-contract checks only when the manifest's
  declared version is not v1, so v1 is not retroactively
  validated against the v2 contract.
- `bench/harness.py` per-query tables now expose the full
  `top_k` (id, score, vector_similarity, source) per query,
  for the v2 Option D evaluation. The pre-v2 record only kept
  `top_id` and `top_score` per query, which is insufficient
  for the abstention-policy decision.

## A2.3 — Actual retrieval contract

The v2 run was executed with the production mpm binary
(`/home/v/.local/bin/mpm`, the live install — not replaced,
not rebuilt) and the live nomic-embed-text model
(`http://127.0.0.1:11434/v1`, 768-dim).

**Source distribution of v2 top-1 results** (across all 60
queries × 10 top-k rows = 600 rows):

| Source | Count | % |
|---|---|---|
| vector | 600 | 100% |
| hybrid | 0 | 0% |
| fts5 | 0 | 0% |

Every result is `vector`-only. The v2 queries are MPM-specific
free-form descriptions; the v2 documents use MPM terminology
that does not match the v2 queries as literal text, so the
FTS5 path never contributes. This is realistic MPM behaviour
and is itself a useful observation: on a labelled topic-family
corpus, the hybrid ranker is operating as a vector-only ranker.

**CombinedScore conversion (vector source, VectorWeight=0.5):**

| vector_similarity | CombinedScore | ratio |
|---|---|---|
| 0.7225 | 0.3612 | 0.5000 |
| 0.7909 | 0.3954 | 0.5000 |
| 0.7686 | 0.3843 | 0.5000 |
| 0.6103 | 0.3052 | 0.5000 |
| 0.8402 | 0.4201 | 0.5000 |
| ... | ... | 0.5000 (every row) |

The conversion is **exactly 0.5 across all 600 rows** (min
0.5000, mean 0.5000, max 0.5000). This is the production
contract: for vector-only rows, `CombinedScore = cosine ×
VectorWeight = cosine × 0.5`.

**Implications for any future threshold or abstention policy:**

1. A "raw-cosine floor of 0.70" is equivalent to a
   `HybridConfig.RetrievalThreshold` of **0.35** (cosine ×
   VectorWeight = 0.7 × 0.5). A `HybridConfig.RetrievalThreshold`
   of 0.70 would require a cosine > 1.4, which is impossible.
2. The default `RetrievalThreshold: -3.0` is a log-space value
   carried from the pre-cosine era. It is unreachable for any
   0-1 cosine-derived score; the threshold check is a no-op
   in practice for the vector-only path.
3. A "hybrid source" combined score is in [0, 1] but is
   **never observed** on the v2 corpus. A policy that keys
   on `source == "hybrid"` would abstain on every v2 query.
4. An FTS5-only path's combined score is in [0, 0.5]; on v2
   this is **never observed** either.

## A2.4 — Option D evaluation

The v2 report's `per_query_table` exposes the full top-10 per
query. Per-query features are computed offline by
`bench/option_d.py`:

- `top1_cosine` — top-1 raw cosine (the L2-normalized dot
  product the vector source reports as `vector_similarity`).
- `top1_combined` — top-1 `CombinedScore` (= 0.5 × cosine on v2,
  see A2.3).
- `top_k_mean_cosine` — mean of the top-10 cosines.
- `top1_to_top2_gap` — top-1 minus top-2 cosine.
- Source distribution per query (vector / fts5 / hybrid).

**Top-1 cosine distribution, v2 + nomic real model:**

| Class | n | min | median | max |
|---|---|---|---|---|
| Positive (40) | 40 | 0.5733 | 0.7546 | 0.9234 |
| Negative (20) | 20 | 0.5697 | 0.6396 | 0.7785 |

The two distributions **overlap** (medians 0.7546 vs 0.6396,
gap 0.1150; but positive min 0.5733 is below negative median).

**Threshold sweep (calibration scope, 26 pos + 15 neg = 41):**

| Threshold (cosine < τ → abstain) | abstain | false-abstention (positives wrongly refused) | false-confidence (negatives wrongly answered) |
|---|---|---|---|
| τ = 0.50 | 0 | 0/26 | 15/15 |
| τ = 0.60 | 1 | 0/26 | 14/15 |
| τ = 0.70 | 17 | 4/26 | 2/15 |
| τ = 0.80 | 32 | 17/26 | 0/15 |
| τ = 0.85 | 41 | 26/26 | 0/15 |

The "clean separation" threshold does not exist. At τ=0.70,
2/15 negatives are still wrongly answered (false confidence),
and 4/26 positives are wrongly refused (false abstention).
At τ=0.80, all 15 negatives are caught but 17/26 positives
are wrongly refused — the policy refuses to answer 65% of
queries that have a real answer in the corpus.

**Multi-feature policies (calibration scope):**

| Policy | abstain | false-abstention | false-confidence |
|---|---|---|---|
| P0 (current production: ranker returns 0 → empty) | 0 | 0/26 | 15/15 |
| P1 cosine ≥ 0.70 | 17 | 4/26 | 2/15 |
| P3 (gap ≥ 0.05 AND cosine ≥ 0.60) | 23 | 9/26 | 1/15 |
| P3 (gap ≥ 0.15 AND cosine ≥ 0.70) | 35 | 20/26 | 0/15 |
| P4 (no vector source → abstain) | 0 | 0/26 | 15/15 |

The full policy sweep (12 policies, including combined-score
and gap variants) is in `/tmp/mpm-bench-v2-eval/calibration.json`
and `held-out.json`. The held-out scope (14 pos + 5 neg = 19)
shows the same trade-off: no policy catches all 5 negatives
without also wrongly abstaining on at least 5 of the 14
positives.

**Empirical finding (Option D conclusion):**

> The available signals (top-1 cosine, top-1-to-top-2 gap,
> source distribution, top-k mean) do **not** contain enough
> evidence to separate answerable from unanswerable queries
> on the v2 corpus with a low-false-abstention AND
> low-false-confidence trade-off. The distributions overlap
> in the 0.65-0.80 cosine range where the bulk of both
> populations live.

A useful confidence calibration is **not** available from the
current observables. The Option D policy that "catches all
negatives" (τ=0.80) wrongly refuses 17/26 calibration
positives (65%) — this would be a regression in
operator-visible recall, not an improvement.

This is the empirical answer to HAL's Option D question. It is
**not** "no useful signals exist" — top-1 cosine carries
real information (positive median 0.75 vs negative median
0.64) — it is "the production ranker's signals are not
discriminating enough to set a confidence threshold without
incurring a recall regression on the answerable side." A
useful abstention policy likely requires either richer signals
(e.g. confidence intervals, paraphrased-query agreement, FTS5
participation with explicit lexical anchor) or a different
ranker, neither of which is in the scope of Phase A.2.

**Proposed machine-readable empty-result contract
(specification only, NOT implemented):**

The current empty-result path at
`cmd/mpm/recall.go:194-197` prints a plain-text line on stdout
and exits 0 even with `--json`; the JSON envelope is only
emitted on a non-empty result. A machine-readable contract
would let callers distinguish "ranker returned nothing" from
"abstention policy decided not to answer" without parsing
plain text.

Proposed JSON envelope (additive; existing fields unchanged):

```json
{
  "success": true,
  "query": "<echo>",
  "search_mode": "hybrid",
  "count": 0,
  "memories": [],
  "abstention": {
    "policy": "<policy_name or 'empty_ranker'>",
    "reason": "ranker_returned_no_rows" |
              "no_result_below_confidence_threshold" |
              "no_source_participated",
    "top_observed_score": 0.0,
    "score_type": "combined" | "cosine",
    "would_have_returned_above_threshold": 0.0
  },
  "limit": 15,
  "min_score": null
}
```

Backwards-compatibility:

- Existing `--json` consumers that key on `memories` continue
  to work; they receive an empty list with `count=0` instead
  of a JSON parse error on the plain-text line.
- Existing plain-text consumers (no `--json`) receive the
  current human-readable line — the abstention metadata is
  additive on the JSON envelope only.
- Existing fields (`success`, `query`, `search_mode`, `memories`,
  `limit`) keep their current types and meanings.
- Exit code remains 0 (recall succeeded; the ranker or policy
  simply returned nothing).
- Operators who want to detect the empty-with-explicit-
  abstention case from a script can key on
  `abstention.reason != "empty_ranker"`.

**Implementation is OUT OF SCOPE for Phase A.2.** The
directive says "specify a proposed machine-readable empty-
result contract ... but do not implement it during Phase A.2."
The contract above is a specification, not a change. Phase B
would evaluate the contract against a real abstention policy
before any change to `cmd/mpm/recall.go`.

## A2.5 — Documentation fix

`bench/README.md:27-30` previously claimed that the harness
captures live-store row counts. The v3 review removed that
sentinel, and the README was not updated in the same change.
The paragraph was replaced with a "No live-store reads"
section that:

- States the harness never opens, reads from, or inspects the
  operator's live MPM database.
- Cross-references the v3 review removal.
- Cross-references `bench/workspace.py:scrubbed_env` for the
  tempdir-per-run isolation contract.

## A2.6 — Verification and report

The Phase A.2 deliverables are:

1. **v1 preserved unchanged** (A2.1): `bench/corpus/v1/`
   MANIFEST.json SHA-256 hashes match the recorded values;
   v1 is loaded by the harness with no `--corpus-version`
   argument. v3 regression test in
   `bench/tests/test_corpus_v2.py::TestV1Preserved` passes.

2. **v2 corpus built and verified** (A2.2): 50 documents, 60
   queries (40 pos / 20 neg), 41 calibration / 19 held-out,
   9 paraphrase pairs, 3 hard negatives, 5 topic families.
   12 v2 contract tests pass in
   `bench/tests/test_corpus_v2.py`. SHA-256 content-binding
   in MANIFEST.json; loader refuses to start if any file
   changes.

3. **Harness extended without breaking v1** (A2.2):
   `bench/harness.py` accepts `--corpus-version v2`;
   `bench/corpus.py:load_corpus` supports per-version
   manifest layout. The default (no flag) still loads v1
   cleanly; the v3 review tests in
   `bench/tests/test_corpus_v2.py::TestV1Preserved` cover
   this.

4. **Per-row top-k exposed** (A2.3): `bench/harness.py` per-
   query tables include the full `top_k` (id, score,
   vector_similarity, source) per query. Pre-v2 the report
   only kept `top_id` and `top_score`. A2.4 uses the new
   exposure.

5. **Actual retrieval contract measured** (A2.3): v2 + nomic
   real-model run captured at
   `/tmp/mpm-bench-v2-nomic-20261019T131153/report.json`.
   Source distribution is 100% `vector` (600/600 rows). The
   `CombinedScore` to `vector_similarity` conversion is
   exactly 0.5 across all 600 rows, confirming
   `CombinedScore = cosine × VectorWeight` for vector-only
   results.

6. **Option D evaluation complete** (A2.4): 12 abstention
   policies evaluated on calibration, held-out, and full
   scopes. Policy-sweep outputs at
   `/tmp/mpm-bench-v2-eval/{all,calibration,held-out}.json`.
   The empirical finding is that the available signals
   (top-1 cosine, top-1-to-top-2 gap, source distribution,
   top-k mean) do not contain enough information to separate
   answerable from unanswerable queries with a useful
   trade-off. This is reported as the empirical answer to
   HAL's Option D question.

7. **Machine-readable empty-result contract specified**
   (A2.4): a proposed JSON envelope is documented in this
   addendum (above). Implementation is out of scope for
   Phase A.2.

8. **README updated** (A2.5): `bench/README.md` no longer
   claims live-store row counts are captured. The "No live-
   store reads" paragraph is in place.

9. **Production invariants preserved** (CLAUDE.md §3,
   directive): no Go production code change; no
   `DefaultHybridConfig` change; no live DB access; 41
   challenged memories preserved; forensic SHA
   `40596f043c3f63d7` untouched; `cmd/dbg_main.go.tmp`
   preserved; no push, no PR, no merge, no service restart,
   no binary replacement, no installation change. The v2
   run uses the live `mpm` binary at
   `/home/v/.local/bin/mpm` unchanged.

10. **Regen verification surfaced a real condition** (v2
    run): the v2 disposable workspace contains 55 embedded
    memories, not 50. The 5 extras are MPM's auto-injected
    seed memories (`mpm-seed-read-wake-context`,
    `mpm-seed-session-end-cluster-triage`,
    `mpm-seed-wake-triage-policy`, `mpm-seed-daemon-health`,
    `mpm-seed-epistemic-compaction-policy`). The regen
    verification correctly caught the mismatch as a
    fingerprint-row-count deviation. This is realistic MPM
    behaviour, not a harness defect: the test workspace is
    a real MPM install, not an empty DB. The 5 seed memories
    participate in retrieval but do not pollute the labelled
    scoring because every query's `relevant_doc_ids` is a
    corpus-specific set.

## Appendix B — Phase A.2 artifacts

| Artifact | Path | Persistence |
|---|---|---|
| v2 + nomic real-model report (JSON) | `/tmp/mpm-bench-v2-nomic-20261019T131153/report.json` | ephemeral |
| v2 + nomic real-model report (Markdown) | `/tmp/mpm-bench-v2-nomic-20261019T131153/report.md` | ephemeral |
| Option D policy sweep (all 60 queries) | `/tmp/mpm-bench-v2-eval/all.json` | ephemeral |
| Option D policy sweep (calibration 41) | `/tmp/mpm-bench-v2-eval/calibration.json` | ephemeral |
| Option D policy sweep (held-out 19) | `/tmp/mpm-bench-v2-eval/held-out.json` | ephemeral |
| v2 corpus (documents, queries, expectations, scoring) | `bench/corpus/v2/` | committed |
| v2 manifest (content-hash binding) | `bench/corpus/v2/MANIFEST.json` | committed |
| v2 labels (manual semantic review) | `bench/corpus/v2/labels.md` | committed |
| v2 contract tests | `bench/tests/test_corpus_v2.py` | committed |
| Option D evaluator | `bench/option_d.py` | committed |
| README fix | `bench/README.md` | committed |
| This addendum | `docs/ISSUE-15-PHASE-A-REPORT.md` (A2.6 section) | committed |

## Appendix C — Phase A.2 stop point

**Phase A.2 is complete. Local commit only. Not pushed.**

**Awaiting HAL review of:**

- The v2 corpus and its label_rationale evidence.
- The Option D empirical finding (no useful abstention
  threshold is available from the current signals).
- The proposed machine-readable empty-result contract
  (specification, not implementation).
- The README documentation fix.

**Phase B remains UNAUTHORISED** until HAL has reviewed valid
negative-query measurements (this addendum) and the no-result
design (this addendum's A2.4 spec). No Go product code change
is proposed in this addendum. No `DefaultHybridConfig` change
is proposed. No `mpm recall` contract change is proposed for
implementation in Phase A.2.

**No deployment, installation, binary replacement, service
restart, or production-state modification occurred during
Phase A.2.**
