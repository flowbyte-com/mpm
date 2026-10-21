# Nomic instruction-prefix condition matrix (Issue #23 Phase A)

- Corpus: `v2` — 50 documents, 60 queries (40 positive / 20 negative), 9 paraphrases
- Evaluation depths: `[1, 5, 10]`
- Model: `nomic-embed-text` via `openai-compatible` at `http://127.0.0.1:11434/v1`
- Fingerprint key: `openai-compatible:nomic-embed-text`, dimension `768`
- Model digest: `0a109f422b47e3a30ba2b10eca18548e944e8a23073ee3f3e947efcf3c45e59f`
- Provider template: `{{ .Prompt }}`

## Summary

| Condition | MRR@10 | ΔMRR@10 vs A | R@1 | R@5 | nDCG@10 | Improved / regressed queries |
|---|---|---|---|---|---|---|
| A (baseline-unprefixed) | 0.9182 | — | 0.8375 | 0.9500 | 0.9269 | — |
| B (query-prefix-only) | 0.9146 | -0.0036 | 0.8375 | 0.9500 | 0.9184 | 0 / 1 |
| C (both-roles) | 0.9113 | -0.0069 | 0.8375 | 0.9500 | 0.9162 | 1 / 4 |
| D (document-prefix-diagnostic) | 0.8933 | -0.0248 | 0.8250 | 0.9500 | 0.9064 | 1 / 5 |

All deltas are against condition A of THIS run, on THIS corpus. No number in this report is comparable with the historical MRR 0.832, which was measured on a different (lost) corpus with a different (lost) harness. See `Limitations`.

## Condition matrix

| Condition | Document prefix | Query prefix | Workspace | Fingerprint |
|---|---|---|---|---|
| A (baseline-unprefixed) | `(none)` | `(none)` | `doc_unprefixed` | ok |
| B (query-prefix-only) | `(none)` | `search_query: ` | `doc_unprefixed` | ok |
| C (both-roles) | `search_document: ` | `search_query: ` | `doc_search_document` | ok |
| D (document-prefix-diagnostic) | `search_document: ` | `(none)` | `doc_search_document` | ok |

## Raw-cosine arm (vector channel only) — aggregates by depth

All means are over the 40 positive queries; negative queries have no graded relevance and are reported separately. `MRR@k` is the reciprocal rank of the first relevant document **within the top k** (0 if none is), so MRR@1 is the hit-rate and MRR@5 is 0 for any query whose first relevant document ranks 6th or lower. MRR@10 over a 10-row corpus is the unbounded MRR.

### Condition A — baseline-unprefixed

| Depth | P@k | R@k | MRR@k | nDCG@k |
|---|---|---|---|---|
| 1 | 0.8750 | 0.8375 | 0.8750 | 0.8750 |
| 5 | 0.2050 | 0.9500 | 0.9146 | 0.9140 |
| 10 | 0.1075 | 0.9875 | 0.9182 | 0.9269 |

- Paraphrase subset (n=9): MRR@10 0.9259, R@1 0.8889, R@5 1.0000
- Split `calibration` (n=26 positive of 41 queries): MRR@10 0.8997, R@1 0.8077, R@5 0.9231
- Split `held-out` (n=14 positive of 19 queries): MRR@10 0.9524, R@1 0.8929, R@5 1.0000
- Negative queries (n=20): mean top-1 similarity 0.6435 (median 0.6396, max 0.7785)
- Score separation (mean top-1): positive 0.7479 vs negative 0.6435
- Irrelevant / no-answer behaviour: the raw-cosine channel has no abstention path — a negative query always returns a full top-k, so its error rate is the top-1 similarity it attracts, not a refusal. The abstention behaviour is measured on the MPM hybrid arm below.

### Condition B — query-prefix-only

| Depth | P@k | R@k | MRR@k | nDCG@k |
|---|---|---|---|---|
| 1 | 0.8750 | 0.8375 | 0.8750 | 0.8750 |
| 5 | 0.2050 | 0.9500 | 0.9146 | 0.9140 |
| 10 | 0.1050 | 0.9625 | 0.9146 | 0.9184 |

- Paraphrase subset (n=9): MRR@10 0.9259, R@1 0.8889, R@5 1.0000
- Split `calibration` (n=26 positive of 41 queries): MRR@10 0.8942, R@1 0.8077, R@5 0.9231
- Split `held-out` (n=14 positive of 19 queries): MRR@10 0.9524, R@1 0.8929, R@5 1.0000
- Negative queries (n=20): mean top-1 similarity 0.6561 (median 0.6481, max 0.7936)
- Score separation (mean top-1): positive 0.7586 vs negative 0.6561
- Irrelevant / no-answer behaviour: the raw-cosine channel has no abstention path — a negative query always returns a full top-k, so its error rate is the top-1 similarity it attracts, not a refusal. The abstention behaviour is measured on the MPM hybrid arm below.

### Condition C — both-roles

| Depth | P@k | R@k | MRR@k | nDCG@k |
|---|---|---|---|---|
| 1 | 0.8750 | 0.8375 | 0.8750 | 0.8750 |
| 5 | 0.2050 | 0.9500 | 0.9113 | 0.9116 |
| 10 | 0.1050 | 0.9625 | 0.9113 | 0.9162 |

- Paraphrase subset (n=9): MRR@10 0.9167, R@1 0.8889, R@5 1.0000
- Split `calibration` (n=26 positive of 41 queries): MRR@10 0.8923, R@1 0.8077, R@5 0.9231
- Split `held-out` (n=14 positive of 19 queries): MRR@10 0.9464, R@1 0.8929, R@5 1.0000
- Negative queries (n=20): mean top-1 similarity 0.6873 (median 0.6797, max 0.7969)
- Score separation (mean top-1): positive 0.7789 vs negative 0.6873
- Irrelevant / no-answer behaviour: the raw-cosine channel has no abstention path — a negative query always returns a full top-k, so its error rate is the top-1 similarity it attracts, not a refusal. The abstention behaviour is measured on the MPM hybrid arm below.

### Condition D — document-prefix-diagnostic

| Depth | P@k | R@k | MRR@k | nDCG@k |
|---|---|---|---|---|
| 1 | 0.8500 | 0.8250 | 0.8500 | 0.8500 |
| 5 | 0.2050 | 0.9500 | 0.8933 | 0.9017 |
| 10 | 0.1050 | 0.9625 | 0.8933 | 0.9064 |

- Paraphrase subset (n=9): MRR@10 0.9111, R@1 0.8889, R@5 1.0000
- Split `calibration` (n=26 positive of 41 queries): MRR@10 0.8667, R@1 0.7885, R@5 0.9231
- Split `held-out` (n=14 positive of 19 queries): MRR@10 0.9429, R@1 0.8929, R@5 1.0000
- Negative queries (n=20): mean top-1 similarity 0.6723 (median 0.6669, max 0.7789)
- Score separation (mean top-1): positive 0.7594 vs negative 0.6723
- Irrelevant / no-answer behaviour: the raw-cosine channel has no abstention path — a negative query always returns a full top-k, so its error rate is the top-1 similarity it attracts, not a refusal. The abstention behaviour is measured on the MPM hybrid arm below.

## Comparison against the same-corpus unprefixed baseline

### B vs A

| Metric | @1 | @5 | @10 |
|---|---|---|---|
| mean_precision | +0.0000 | +0.0000 | -0.0025 |
| mean_recall | +0.0000 | +0.0000 | -0.0250 |
| mean_mrr | +0.0000 | +0.0000 | -0.0036 |
| mean_ndcg | +0.0000 | +0.0000 | -0.0085 |

- Per-query MRR: 0 improved, 1 regressed, 59 unchanged (of 60)
- Per-split MRR delta:
  - `calibration` (n=26 positive): MRR -0.0055, R@1 +0.0000, R@5 +0.0000
  - `held-out` (n=14 positive): MRR +0.0000, R@1 +0.0000, R@5 +0.0000
- Regressed queries:
  - `q02016` (positive, split=calibration): MRR 0.1429 -> 0.0000 (first-relevant rank 7 -> —)

### C vs A

| Metric | @1 | @5 | @10 |
|---|---|---|---|
| mean_precision | +0.0000 | +0.0000 | -0.0025 |
| mean_recall | +0.0000 | +0.0000 | -0.0250 |
| mean_mrr | +0.0000 | -0.0033 | -0.0069 |
| mean_ndcg | +0.0000 | -0.0024 | -0.0107 |

- Per-query MRR: 1 improved, 4 regressed, 55 unchanged (of 60)
- Per-split MRR delta:
  - `calibration` (n=26 positive): MRR -0.0074, R@1 +0.0000, R@5 +0.0000
  - `held-out` (n=14 positive): MRR -0.0060, R@1 +0.0000, R@5 +0.0000
- Regressed queries:
  - `q02004` (positive, split=calibration): MRR 1.0000 -> 0.5000 (first-relevant rank 1 -> 2)
  - `q02016` (positive, split=calibration): MRR 0.1429 -> 0.0000 (first-relevant rank 7 -> —)
  - `q02039` (positive, split=held-out): MRR 0.3333 -> 0.2500 (first-relevant rank 3 -> 4)
  - `q02040` (positive, split=calibration): MRR 0.2500 -> 0.2000 (first-relevant rank 4 -> 5)
- Improved queries:
  - `q02008` (positive, split=calibration): MRR 0.5000 -> 1.0000

### D vs A

| Metric | @1 | @5 | @10 |
|---|---|---|---|
| mean_precision | -0.0250 | +0.0000 | -0.0025 |
| mean_recall | -0.0125 | +0.0000 | -0.0250 |
| mean_mrr | -0.0250 | -0.0213 | -0.0248 |
| mean_ndcg | -0.0250 | -0.0122 | -0.0206 |

- Per-query MRR: 1 improved, 5 regressed, 54 unchanged (of 60)
- Per-split MRR delta:
  - `calibration` (n=26 positive): MRR -0.0331, R@1 -0.0192, R@5 +0.0000
  - `held-out` (n=14 positive): MRR -0.0095, R@1 +0.0000, R@5 +0.0000
- Regressed queries:
  - `q02001` (positive, split=calibration): MRR 1.0000 -> 0.3333 (first-relevant rank 1 -> 3)
  - `q02004` (positive, split=calibration): MRR 1.0000 -> 0.5000 (first-relevant rank 1 -> 2)
  - `q02016` (positive, split=calibration): MRR 0.1429 -> 0.0000 (first-relevant rank 7 -> —)
  - `q02039` (positive, split=held-out): MRR 0.3333 -> 0.2000 (first-relevant rank 3 -> 5)
  - `q02040` (positive, split=calibration): MRR 0.2500 -> 0.2000 (first-relevant rank 4 -> 5)
- Improved queries:
  - `q02008` (positive, split=calibration): MRR 0.5000 -> 1.0000

## MPM hybrid-retrieval arm (production ranker, unprefixed)

- MPM's production hybrid ranker (FTS5 + vector blend) on the unprefixed workspace. This is the production configuration; no prefix condition is expressible through it without a production change (internal/core/hybrid_search.go embeds the query verbatim).
- Snapshot logical mutation: `clean`
- Negative queries returning an empty result: 0 / 20

| Depth | P@k | R@k | MRR@k | nDCG@k |
|---|---|---|---|---|
| 1 | 0.8750 | 0.8375 | 0.8750 | 0.8750 |
| 5 | 0.2050 | 0.9500 | 0.9146 | 0.9140 |
| 10 | 0.1075 | 0.9875 | 0.9182 | 0.9269 |

- Agreement with the raw-cosine arm over 60 queries at depth 10: 0 differing top-1 documents, 0 differing first-relevant ranks, 0 differing MRRs (identical).

## Limitations

- The corpus is a 50-document / 60-query hand-authored synthetic set about MPM's own internals. Its scores are NOT comparable with the historical MRR 0.832, which was measured on a different (lost) corpus with a different (lost) harness.
- The raw-cosine arm is not the MPM hybrid ranker. MPM blends FTS5 keyword scoring with the vector channel; the raw-cosine arm measures the vector channel alone.
- The hybrid arm can only be measured in the unprefixed condition: MPM's recall path embeds the query verbatim, so a prefixed condition would require a production change.
- In the document-prefixed conditions the corpus row's stored text carries the prefix (that is how the benchmark makes MPM's production embed path produce a prefixed vector). The stored text therefore also differs from production, which is why no hybrid measurement is taken in those workspaces.
- Single model, single corpus, single run: no confidence intervals are computed. The per-query improved/regressed counts are the honest uncertainty signal.
- The raw-cosine channel has no abstention path: a negative query always receives a full top-k. The irrelevant / no-answer behaviour is therefore measured on the MPM hybrid arm, and the raw-cosine error rate for negatives is reported as the top-1 similarity a negative query attracts.
- A prefix change is a full re-embedding of the corpus in any future implementation. This experiment measures retrieval on a 50-document corpus; it does not measure the cost, the recall of already-stored vectors, or the behaviour of the live store, none of which are in scope for Phase A.

## Reproduction

```bash
MPM_BENCH_COLLECTION=bench_v2_corpus \
  python3 -m bench prefix-experiment --output-dir /tmp/mpm-bench-issue23 --config bench/fixtures/nomic-embed-text.json --corpus-version v2 --conditions A,B,C,D
```
