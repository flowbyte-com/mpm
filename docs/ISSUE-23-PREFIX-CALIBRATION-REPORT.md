# Issue #23 — Nomic instruction-prefix calibration report

**Scope.** Measure whether Nomic's documented task prefixes
(`search_document: ` for indexed text, `search_query: ` for queries)
change MPM's retrieval quality, and record an evidence-based decision.

**Status.** Measured. **Decision: retain the unprefixed baseline.** No
production prefixing is implemented, no re-embed is performed, no live
database is read or written. The evidence is a fingerprint-verified,
four-condition measurement (A/B/C/D) on disposable workspaces, with the
machine-readable artefact committed at
`bench/output/issue-23-phase-a/`.

**Issue #23's second acceptance criterion is WAIVED, not passed.** The
run did not happen on the post-migration (live) store, because live
access is prohibited for this work and because no further live-store
measurement is authorised. The measurement is explicitly *not* comparable
with the historical MRR 0.832. §9.1 states this in full, records the
waiver and its rationale, and does not claim the criterion as satisfied.

**HAL accepted the measured result on 2026-10-21: Option C — retain the
unprefixed baseline.** The corrected harness is in draft PR #43; the
merge SHA and the issue closure follow that review, not this document.

**Baseline.** The measurement was taken at `main` =
`261d1c1d09cca39d226203962dc6377ffb4598f8`, which is this branch's base.
`main` has since advanced to `4bbcaf65` through the independent,
separately approved asset-removal PR (#44). That change is disjoint from
this work and is not part of this branch.

**Branch.** `research/issue-23-nomic-prefix-calibration`.

---

## 1. The question

Nomic's model card documents two task prefixes. MPM embeds documents and
queries through the same provider call with no role distinction
(`internal/core/embeddings.go`), so neither prefix is applied today. A
historical benchmark on a now-lost corpus attributed **+0.022 MRR** to a
`search_query: ` prefix, and that figure has been carried forward in
[`EMBEDDING-MODEL-COMPARISON.md`](./EMBEDDING-MODEL-COMPARISON.md) §6 and
[`NOMIC-MIGRATION.md`](./NOMIC-MIGRATION.md) §5 as an unclaimed gain.

Issue #23 asks for the measurement to be made, or the issue closed with
reasoning. This report is the measurement.

**The historical +0.022 was not reproduced.** Every prefix condition came
out at or below the unprefixed baseline.

---

## 2. Model fingerprint

Recorded from the provider at run time, not from documentation. The
`digest` is the Ollama content hash of the exact model file; the
`template` is the model's own prompt template, which matters because it
shows Ollama applies **no** automatic prefix of its own.

| field | value |
|---|---|
| provider | `openai-compatible` (Ollama's OpenAI-compatible endpoint) |
| base URL | `http://127.0.0.1:11434/v1` |
| model | `nomic-embed-text` |
| **digest** | `0a109f422b47e3a30ba2b10eca18548e944e8a23073ee3f3e947efcf3c45e59f` |
| family | `nomic-bert` |
| parameters | `137M` (F16) |
| embedding length | `768` |
| context | `num_ctx 8192` |
| template | `{{ .Prompt }}` |
| MPM fingerprint key | `openai-compatible:nomic-embed-text` |

The digest is read from Ollama's `GET /api/tags` and the embedding length
from `model_info["nomic-bert.embedding_length"]` in `POST /api/show`;
this Ollama build's `/api/show` reply carries no digest field of its own.

Any future implementation would have to be keyed to this fingerprint, so
that a non-nomic profile is unaffected.

---

## 3. Corpus provenance

The corpus is **synthetic** and hand-authored. It is not the historical
30-query corpus (that one is gone) and not the live store.

| field | value |
|---|---|
| corpus version | `v2` |
| generator | `hand-authored-from-topics@2026-10-19` |
| `MANIFEST.json` sha256 | `48fdb92d6dd28bb58e56dfba6c1e8267bf2a2450b7f990cd4ca4c97eb80c9add` |
| `documents.json` sha256 | `753dbb458c237ec101cf3f1d4763b31ae7e140fd00c26c80e6d93a1f99d32a95` |
| `queries.json` sha256 | `01c3444794d69c079a9a6c896cc149fb5b647a1e31b982360051d7f48da8cfa1` |
| `expectations.json` sha256 | `05a45f528348cb779c8792d0f0a3655e3030d0ccaacf7a2182b3972a3e774425` |
| `scoring.json` sha256 | `e532ad942241fbebcb08c420ce145d4eb3878787b684728b6ab093f605ecef8b` |
| documents | 50 |
| queries | 60 (40 positive / 20 negative) |
| paraphrases | 9 |
| splits | calibration 41 (26 positive / 15 negative), held-out 19 (14 positive / 5 negative) |
| hard negatives | 4 |
| topic families | `all`, `challenge`, `memory_lifecycle`, `retrieval_core`, `schema_persistence`, `search_operators` |
| ranking depth | 10 |

The **held-out** split is a distinct topic family (`search_operators`),
not a random sample of the calibration queries, so the held-out numbers
below are a genuine out-of-sample check rather than a re-read of the
tuning set.

---

## 4. Method

Four conditions, one per cell of the matrix:

| Condition | Document prefix | Query prefix | Workspace |
|---|---|---|---|
| **A** `baseline-unprefixed` | (none) | (none) | `doc_unprefixed` |
| **B** `query-prefix-only` | (none) | `search_query: ` | `doc_unprefixed` |
| **C** `both-roles` | `search_document: ` | `search_query: ` | `doc_search_document` |
| **D** `document-prefix-diagnostic` | `search_document: ` | (none) | `doc_search_document` |

Rules the harness enforces:

- **One workspace per document preparation.** A, B share the unprefixed
  workspace; C, D share the prefixed one. Vectors from differently
  prepared documents can never be mixed. The preparation key is *derived*
  from the prefix, so a caller cannot pair the unprefixed key with
  prefixed text.
- **The document prefix is applied at ingest**, so MPM's own production
  embed path (`cmd/mpm/backfill_embeddings.go` →
  `cfg.Provider.Embed(mem.Content)`) produces the vector. The benchmark
  never re-implements the provider.
- **The query prefix is applied before the provider call**, the same shape
  MPM's recall path uses.
- **A corpus that already carries a prefix is refused**, so a
  double-prefixed measurement cannot happen silently.
- **Every workspace gets a fresh fingerprint verification.**
- **`internal/core` is not modified.** No production embedding behaviour
  changes. The prefix is applied benchmark-side only.

**Two arms.** The *raw-cosine arm* measures the vector channel alone and is
the only arm that can express a query-side prefix condition: MPM's recall
path embeds the query verbatim (`internal/core/hybrid_search.go:175`), so
no prefix condition is expressible through the production hybrid ranker
without a production change. The *hybrid arm* runs MPM's production ranker
on the unprefixed workspace, and its per-query agreement with the
raw-cosine arm is reported (§6).

**Isolation.** Every workspace is a disposable private temp directory with
`MPM_WORKSPACE` and `HOME` pointed at it and the environment scrubbed. The
harness never opens, copies or reads the operator's live MPM database; the
report records `isolation.live_store_touched: false`.

**Metric semantics** (also carried in the artefact's `metric_definitions`):

- Every mean is over **positive queries only**. Negative queries have no
  graded relevance, score 0 on all four metrics by construction, and are
  reported separately.
- The ranking is **truncated to k before scoring**, so `MRR@1` is the
  hit-rate and `MRR@5` is 0 for any query whose first relevant document
  ranks 6th or lower. `MRR@10` over a 10-row corpus is the unbounded MRR.
- Every delta is **condition minus the same-corpus unprefixed baseline**
  at the same depth. No number here is comparable with the historical
  MRR 0.832.

---

## 5. Results

### 5.1 Headline (raw-cosine arm, depth 10)

| Condition | MRR@10 | ΔMRR@10 vs A | R@1 | R@5 | nDCG@10 | improved / regressed queries |
|---|---|---|---|---|---|---|
| A `baseline-unprefixed` | **0.9182** | — | 0.8375 | 0.9500 | 0.9269 | — |
| B `query-prefix-only` | 0.9146 | **−0.0036** | 0.8375 | 0.9500 | 0.9184 | 0 / 1 |
| C `both-roles` | 0.9113 | **−0.0069** | 0.8375 | 0.9500 | 0.9162 | 1 / 4 |
| D `document-prefix-diagnostic` | 0.8933 | **−0.0248** | **0.8250** | 0.9500 | 0.9064 | 1 / 5 |

Every prefix condition is at or below the baseline. D is the only
condition that damages Recall@1 (0.8375 → 0.8250).

### 5.2 Per-query regressions and improvements

Every query that moved, out of 60 (40 positive + 20 negative):

| Condition | Query | Split | Baseline MRR | Condition MRR | First-relevant rank |
|---|---|---|---|---|---|
| B | `q02016` | calibration | 0.1429 | 0.0000 | 7 → — (not in top 10) |
| C | `q02004` | calibration | 1.0000 | 0.5000 | 1 → 2 |
| C | `q02016` | calibration | 0.1429 | 0.0000 | 7 → — |
| C | `q02039` | held-out | 0.3333 | 0.2500 | 3 → 4 |
| C | `q02040` | calibration | 0.2500 | 0.2000 | 4 → 5 |
| D | `q02001` | calibration | 1.0000 | 0.3333 | 1 → 3 |
| D | `q02004` | calibration | 1.0000 | 0.5000 | 1 → 2 |
| D | `q02016` | calibration | 0.1429 | 0.0000 | 7 → — |
| D | `q02039` | held-out | 0.3333 | 0.2000 | 3 → 5 |
| D | `q02040` | calibration | 0.2500 | 0.2000 | 4 → 5 |

Improved:

| Condition | Query | Split | Baseline MRR | Condition MRR |
|---|---|---|---|---|
| C | `q02008` | calibration | 0.5000 | 1.0000 |
| D | `q02008` | calibration | 0.5000 | 1.0000 |

Net per-query movement: B 0 up / 1 down, C 1 up / 4 down, D 1 up / 5
down. `q02016` is the shared failure: the prefixed embedding pushes the
only relevant document out of the top 10 entirely.

### 5.3 Held-out results

The held-out split (19 queries, 14 positive) is a separate topic family.
MRR@10 over the held-out positive queries:

| Condition | MRR@10 | Δ vs A | R@1 | R@5 | nDCG@10 |
|---|---|---|---|---|---|
| A | 0.9524 | — | 0.8929 | 1.0000 | 0.9586 |
| B | 0.9524 | +0.0000 | 0.8929 | 1.0000 | 0.9586 |
| C | 0.9464 | −0.0060 | 0.8929 | 1.0000 | 0.9593 |
| D | 0.9429 | −0.0095 | 0.8929 | 1.0000 | 0.9562 |

Calibration split (41 queries, 26 positive) for comparison: A 0.8997,
B 0.8942 (−0.0055), C 0.8923 (−0.0074), D 0.8667 (−0.0331).

The held-out split confirms the calibration split's direction: B is
flat, C and D are below baseline. The effect is not an artefact of the
tuning queries.

### 5.4 Why the prefix hurts

Prefixing adds a common component to every vector. Similarity between
*any* two prefixed vectors rises, so positives and negatives inflate
together and the margin between them narrows:

| Condition | mean top-1 similarity, positive | negative | separation | hard-negative top-1 |
|---|---|---|---|---|
| A | 0.7479 | 0.6435 | **0.1044** | 0.7014 |
| B | 0.7586 | 0.6561 | 0.1024 | 0.7175 |
| C | 0.7789 | 0.6873 | 0.0916 | 0.7455 |
| D | 0.7594 | 0.6723 | 0.0871 | 0.7228 |

Separation shrinks monotonically (0.1044 → 0.1024 → 0.0916 → 0.0871) and
hard-negative similarity rises (A 0.7014 → B 0.7175 → C 0.7455; D 0.7228 —
above the unprefixed baseline in every prefixed condition). The prefix
buys higher absolute similarity at the cost of discriminability, and
discriminability is what ranking needs.

### 5.5 Paraphrase subset

| Condition | MRR@10 | R@1 | R@5 |
|---|---|---|---|
| A | 0.9259 | 0.8889 | 1.0000 |
| B | 0.9259 | 0.8889 | 1.0000 |
| C | 0.9167 | 0.8889 | 1.0000 |
| D | 0.9111 | 0.8889 | 1.0000 |

The paraphrase subset — the separation that mattered most when `nomic`
was chosen over `all-minilm` — does not benefit from any prefix.

---

## 6. Does the raw-cosine arm stand in for MPM's ranker?

The hybrid arm runs MPM's production ranker (FTS5 + vector blend) on the
unprefixed workspace. Snapshot logical mutation: `clean`. Negative
queries returning an empty result: 0 / 20 (the `RetrievalThreshold =
-3.0` no-op described in
[`EMBEDDING-MODEL-COMPARISON.md`](./EMBEDDING-MODEL-COMPARISON.md) §2).

Its aggregates are identical to condition A's raw-cosine aggregates at
every depth, and the per-query agreement is **exact**:

| | value |
|---|---|
| queries compared | 60 |
| differing top-1 documents | 0 |
| differing first-relevant ranks | 0 |
| differing MRRs | 0 |
| identical | **yes** |

So on this corpus the raw-cosine arm *is* MPM's ranker, query by query.
That is what licenses reading §5's numbers as a statement about MPM's
retrieval rather than about one channel in isolation.

**Independent validation.** The same harness, unchanged, reproduces the
committed Issue #15 A.3 measurement
(`bench/output/phase-a3/v2-nomic/report.json`) **exactly**: condition A
matches `raw_cosine_unprefixed_scoring` and condition B matches
`raw_cosine_prefixed_scoring` with a difference of 0.0 on all four
aggregates at depth 10, and 0 per-query MRR differences across all 60
queries. Two independent runs of the same measurement agreeing to the
last decimal is the strongest available evidence that the harness is
deterministic and that the numbers above are not run noise.

The condition matrix itself was re-run three times during Phase B — once
per code revision that touched the harness, plus the final run against
the final binary. All three produced the same four aggregates, the same
per-query regressions and the same hybrid/raw-cosine agreement.

---

## 7. Fingerprint verification

Every condition's workspace passed the regeneration gate
(`bench/fingerprint.py:verify_regeneration_complete`) with zero failures:

- every embedded row carries the expected
  `(openai-compatible:nomic-embed-text, 768, provider)` fingerprint;
- every stored BLOB decodes to a 768-float vector;
- no zero-norm vector, no NaN, no Inf;
- every corpus document id is present, and there are no extra rows in
  the evaluation collection `bench_v2_corpus` (exactly the 50 corpus
  rows);
- no reference chunk is left unembedded.

The report's `fingerprint_groups` summary is **not** collection-scoped
and therefore counts 56 embedded memory rows. The decomposition is
50 corpus documents + 5 Baseline Cognitive Bootstrap prime-directive
rows (collection `directives`) + 1 bootstrap memory left by the
schema-initialising `mpm remember bootstrap` call (collection
`memories`). All six are outside the evaluation collection
`bench_v2_corpus`, and the raw-cosine ranking path is collection-scoped
and refuses to run without a collection, so none of them can enter a
ranking.

---

## 8. Reproduction

```bash
# Canonical FTS5 build.
make build

# The four-condition matrix. Requires a local Ollama serving
# `nomic-embed-text` with digest
# 0a109f422b47e3a30ba2b10eca18548e944e8a23073ee3f3e947efcf3c45e59f.
MPM_BENCH_COLLECTION=bench_v2_corpus \
  python3 -m bench prefix-experiment \
    --output-dir /tmp/mpm-bench-issue23 \
    --config bench/fixtures/nomic-embed-text.json \
    --corpus-version v2 \
    --conditions A,B,C,D
```

The run writes `prefix-experiment-report.json` and
`prefix-experiment-report.md`. The committed copy of that run is at
`bench/output/issue-23-phase-a/`, with a `SHA256SUMS` file.

The binary used by the committed run, recorded in the artefact:

| field | value |
|---|---|
| path | `.build/bin/mpm` (canonical build artifact) |
| sha256 | `cba07a5f6aa7144782a233613b31a073bd24403dc28c239fcac4cea1c4848ae1` |
| `mpm --version` | `MPM mpm archive/pr29head-16-g261d1c1` |
| git head | `261d1c1d09cca39d226203962dc6377ffb4598f8` |
| build | `CGO_CFLAGS=-DSQLITE_ENABLE_FTS5=1 CGO_LDFLAGS=-lm go build -tags fts5` |

The binary sha256 identifies the binary this run used; it is **not**
reproducible across builds, because Go stamps the VCS state (including
whether the working tree was dirty) into the binary. Rebuilding and
seeing a different sha256 is not evidence that the measurement is not
reproducible — the *measurement* is what reproduces, exactly (§6).

The harness self-tests (no binary, no network, no live DB) run with:

```bash
python3 -m bench selftest
```

---

## 9. Limitation: this is not the post-migration-store benchmark

**Stated plainly: this measurement does not reproduce the original
post-migration-store benchmark, and it does not establish comparability
with the historical MRR 0.832.**

- The corpus is a 50-document synthetic set about MPM's own internals.
  The historical 0.832 was measured on the operator's live store with a
  different, lost harness (`/tmp/embench/`). The two are not on the same
  scale and the numbers must not be placed side by side.
- The historical 30-query benchmark scripts are gone; the corpus and
  hand-labelled judgments that drove them were never preserved as
  artefacts. That measurement is not runnable against the current
  repository.
- The raw-cosine arm measures the vector channel. MPM's production ranker
  blends FTS5 keyword scoring with it. On this corpus the two agree
  query-by-query (§6), but that agreement is an observation about this
  corpus, not a general property.
- One model, one corpus, one run. No confidence intervals are computed;
  the per-query improved/regressed counts are the honest uncertainty
  signal, and with 0–5 moved queries per condition the differences are
  small in absolute terms.

### 9.1 Reconciliation with Issue #23's acceptance criterion

Issue #23's second criterion reads, in full:

> **If measured**, the benchmark runs on the post-migration store with
> the harness's fingerprint assertions active, and the result reported
> alongside the 0.832 baseline rather than the pre-migration 0.783.

**This criterion is WAIVED. It is not satisfied, and it is not marked as
passed.** The measurement was deliberately *not* run against the
post-migration (live) store, because live-database access is prohibited
for this work and because a benchmark that runs against the operator's
store must not be the thing that decides a production change.

What *was* satisfied: the fingerprint assertions were active for every
workspace and every condition; the result is reported as a delta against
a **same-corpus baseline**, which is the only comparison that is
well-founded here.

What was not satisfied: the run did not happen on the post-migration
store, and the result is therefore **not** reported alongside 0.832.

The rationale for closing Issue #23 on this basis:

1. The criterion's *purpose* is to stop a prefix claim from being
   attached to an incomparable number — "report alongside 0.832 rather
   than 0.783" exists so that a reader is not misled about what moved.
   This measurement claims **no improvement at all**. There is nothing
   to attach to 0.832, and the artefact says so in its own `Limitations`
   block: no number in it is comparable with 0.832. The obligation the
   criterion creates about *where* the run happens is unmet, and point
   4 records that openly rather than papering over it.
2. The criterion is **conditional on "if measured"**. By measuring, the
   criterion was triggered, and it is therefore not satisfied in its
   letter. What Issue #23 actually needed was a decision with evidence
   behind it. That evidence is stronger than the number the criterion
   was written to protect: four conditions, a fingerprint gate, a
   per-query breakdown, an out-of-sample split, and an independent exact
   reproduction of a previously committed measurement.
3. The decision is **conservative and reversible**. "Retain the
   unprefixed baseline" is the no-change outcome. Nothing is implemented,
   nothing is re-embedded, no vector in the live store is touched, and
   the measurement is committed so that a future run can re-litigate it.
4. **The criterion is waived, and no further live-store measurement is
   authorised.** HAL's decision of 2026-10-21 accepts the measured
   result and records this criterion as **waived** — explicitly not
   passed. The waiver is deliberate and bounded. The criterion's letter
   would have required a post-migration-store run; that access is not
   authorised for this work and will not be sought. Copying the live
   database was never an acceptable route either — that is prohibited
   independently of this issue — and even a hypothetical authorised run
   would still have produced a *delta* rather than a number comparable
   with 0.832, because the historical corpus and labels are gone. The
   obligation is therefore recorded as **waived**, not quietly treated
   as done.

**What a reader may and may not take from this report.** The decision
rests on a same-corpus comparison whose every number is reproduced by
the committed artefact in `bench/output/issue-23-phase-a/`. The
criterion that would have placed the historical 0.832 next to these
numbers was waived, so no such juxtaposition exists and none is
implied. **No number in this report or in the artefact is comparable
with the historical MRR 0.832.**

---

## 10. Decision and consequences

**Decision: retain the unprefixed baseline.** MPM continues to embed
documents and queries verbatim, with no `search_query: ` and no
`search_document: ` prefix, on either role.

Consequences, all of them "do not":

- **No production change.** `internal/core` is untouched. The prefix was
  applied benchmark-side only.
- **No re-embed.** The live store's vectors are unchanged. A prefix
  implementation would have been a re-embed under the
  [`NOMIC-MIGRATION.md`](./NOMIC-MIGRATION.md) §4 backup procedure, not
  a config change.
- **No live-database access.** The measurement ran entirely on disposable
  workspaces with synthetic fixtures.
- **No change to the 0.85 contradiction threshold**, to the
  `RetrievalThreshold = -3.0` no-op, or to any other retrieval
  behaviour.

Follow-ups this measurement *does* open, none of which are part of this
change:

- The `gemma` prefix result (+0.043, historical) has not been
  re-measured and is equally unverified.
- The `qwen3-embedding` query-instruction prefix (historically −0.066)
  has never been measured by MPM's own harness.
- The marginal retrieval gains from prefixing are, if anything, negative
  on this corpus — but the corpus is 50 synthetic documents, so the
  finding bounds the *decision* (do not prefix) rather than settling
  the *science* (prefixing never helps).

---

## 11. Post-review corrections (HAL, 2026-10-21)

HAL reviewed the draft at `00bb776` and asked for two corrections to the
harness. **Neither changes a measurement.** Both change how the harness
*fails*, which is the property that makes a measurement trustworthy.

### 11.1 A detected logical mutation now fails the complete experiment

The hybrid benchmark checks the disposable workspace for tracked-state
changes between queries. Before the correction, a detected mutation broke
out of the query loop, scored the partial result, wrote it into the
report and exited 0 — a broken measurement published as a successful one.
A reader of that report could not tell the two apart.

Now the mutation raises. The run exits nonzero and **no report is
written at all**: there is no partial artefact for a later reader to
mistake for a measurement.

The committed measurement was taken with a clean mutation status
(`hybrid_arm.snapshot_logical_mutation.status == "clean"`), so this
correction does not alter it.

### 11.2 Condition A is now required, and the baseline is A by identity

Every comparison this experiment reports is "condition vs the same-corpus
unprefixed baseline". Before the correction, a selection that omitted A
fell back to the first selected condition as the baseline — so a run of
`--conditions B,C,D` would have published deltas against the *prefixed*
condition B and labelled them baseline deltas.

Now a selection without A is refused with a clear error **before any
workspace is created**, so an invalid selection costs nothing and touches
nothing. The baseline is condition A by identity, not by position: the
selection order no longer decides what a comparison is measured against.
B, C and D are prefixed conditions and are never substituted for the
unprefixed baseline.

There is deliberately no "no-baseline mode". A run without a baseline
would produce per-condition numbers with nothing to say what they are
relative to, and would have to be read as a comparison anyway.

### 11.3 The measurement is unchanged

The corrected harness was re-run against the same corpus, the same model
(digest `0a109f42…5e59f`) and the same four conditions. The result is
identical to the committed artefact:

| compared | result |
|---|---|
| all four conditions' aggregates at every depth | identical |
| all 60 per-query rows (MRR, Recall, nDCG, first-relevant rank, top-1 id) | identical |
| negative / positive / paraphrase / hard-negative subsets | identical |
| calibration / held-out split metrics | identical |
| `comparisons_vs_baseline` (including `baseline_condition: "A"` on every entry) | identical |
| hybrid arm, its mutation status, and the hybrid-vs-raw-cosine agreement | identical (0 differing top-1 documents, 0 differing MRRs) |
| the report's top-level structure | unchanged |
| the rendered Markdown report | **byte-identical** — sha256 `489071f3f2bf9742f57f6c2eabfaa4313a977deff981cdbb7212e08832b842f3`, the committed checksum |

The committed artefact in `bench/output/issue-23-phase-a/` is therefore
still *the* measurement, and the corrections did not alter it. The
corrections are covered by new hermetic regression tests: 9 new tests in
`bench/tests/test_prefix_experiment.py` (40 in the file, up from 31).

