# Embedding Model Comparison — four models, 30 labelled queries

Extends [`EMBEDDING-MODEL-BENCHMARK.md`](./EMBEDDING-MODEL-BENCHMARK.md)
(two models, 22 queries) to four models and 30 queries, adds the
mutation-cost measurement per model, and measures query-instruction
sensitivity.

**Outcome: `nomic-embed-text` is the leading retrieval candidate
(MRR 0.832, nDCG@10 0.789, paraphrase MRR 0.529 vs `all-minilm`'s
0.260). Do not migrate yet** — two preconditions are unmet, both
described in §7. The decisive finding is in §4.

## Method

Four fully isolated workspaces under `/tmp/embench/{minilm2,qwen3b,gemma,nomic}/`.
Each is a `sqlite3.Connection.backup()` snapshot of the live DB (the live DB
is in WAL mode, so a file copy is not consistent), **fully re-embedded from
scratch by its own model**, with `memories` and `reference_chunks` both
regenerated:

| workspace | model | dim | size | memories | ref chunks |
|---|---|---|---|---|---|
| `minilm2` | `all-minilm` | 384 | 45 MB | 146 | 232 |
| `qwen3b` | `qwen3-embedding:0.6b` | 1024 | 639 MB | 146 | 232 |
| `gemma` | `embeddinggemma` | 768 | 621 MB | 146 | 232 |
| `nomic` | `nomic-embed-text` | 768 | 274 MB | 146 | 232 |

Ranking policy was held constant across all four (same query set, same
ranker, same `HybridConfig`) so model differences are attributable to the
model rather than to policy.

**Snapshot correctness — a bug worth recording.** `pristine.db` is the
restore point applied before *every* query. Capturing it *before*
regeneration silently restored the live 384-dim `all-minilm` vectors into
every workspace, benchmarking three models in the wrong vector space. It
surfaced because `gemma` returned zero results for all 30 queries. The fix
was to capture `pristine.db` *after* regeneration and assert its dimension
at both build time and restore time:

```python
# build_snapshot.py — restore point is the POST-regeneration store
db_backup(f"{WS}/src/db/mpm.db", f"{WS}/pristine.db")
pc = sqlite3.connect(f"{WS}/pristine.db")
pd = pc.execute("SELECT embedding FROM memories WHERE embedding IS NOT NULL "
                "AND embedding != 'null' LIMIT 1").fetchone()[0]
assert len(json.loads(pd)) == DIM
```

`run_ext.py` repeats the assertion in `restore()`. `minilm2` was unaffected
by coincidence — its regenerated vectors *are* 384-dim `all-minilm`.

**The live store was never modified.** Read-only throughout; verified
post-run against a pre-run backup: 0 rows differ in weight, metadata, or
content; 41 challenged rows unchanged. All mutation figures below are
observed *inside* the isolated snapshots.

Query set: 22 labelled relevance queries plus 7 negatives (deliberately
off-domain — recipes, unrelated technical how-tos) and 1 near-duplicate
cluster case.

## 1. Relevance (30 queries, MPM's ranker)

| metric | all-minilm | qwen3-0.6b | gemma | nomic |
|---|---|---|---|---|
| MRR | 0.767 | 0.815 | 0.775 | **0.832** |
| R@1 | 0.652 | **0.739** | 0.696 | **0.739** |
| R@5 | 0.913 | 0.913 | 0.870 | 0.913 |
| nDCG@10 | 0.729 | 0.771 | 0.730 | **0.789** |
| exact MRR | 0.900 | 1.000 | 1.000 | 0.900 |
| **paraphrase MRR** | 0.260 | 0.517 | 0.300 | **0.529** |
| multi MRR | 1.000 | 1.000 | 0.900 | 1.000 |
| scope MRR | 1.000 | 0.583 | 1.000 | 1.000 |

Paraphrase is the separation that matters: `all-minilm` finds what you
*name* and struggles with what you *describe*. `nomic` roughly doubles it.

Scope MRR rests on a single case per model — `all-minilm`'s 1.000 vs
`qwen3`'s 0.583 is one query, not a category-level finding.

## 2. Irrelevant-query behaviour

All four return 10 results for all 7 negatives. This is structural, not
model-specific: `DefaultHybridConfig.RetrievalThreshold` is `-3.0`, which
vector-only rows (similarity ∈ [0,1]) can never reach. **There is
effectively no vector similarity floor**, so "no relevant results" is not
a reachable state on the vector leg for any model.

## 3. Similarity scale — the compatibility question

Contradiction detection compares cosine to a hardcoded `0.85`
(`hybrid_search.go`). What that constant *means* differs enormously per
model:

| | all-minilm | qwen3-0.6b | gemma | nomic |
|---|---|---|---|---|
| relevant-pair mean | 0.544 | 0.681 | 0.641 | 0.644 |
| irrelevant-pair mean | **0.063** | 0.407 | 0.270 | 0.386 |
| **separation gap** | **0.481** | 0.274 | 0.371 | 0.258 |
| scan pairs ≥ 0.85 | 262/3150 | 732/3150 | 1115/3150 | 273/3150 |
| % of scan | 8.32 | 23.24 | 35.40 | 8.67 |
| queries exposing ≥1 | 18/30 | 30/30 | 25/30 | 28/30 |

`all-minilm` is the only model whose scale makes 0.85 a usable separator —
and it is the weakest retriever of the four. **A better model makes the
fixed threshold do more damage.** This is the central finding: the
threshold is not a neutral default that any model can inherit.

## 4. The decisive finding: mutation cost rises with retrieval quality

Measured inside each isolated snapshot, before/after the 30-query run:

| | all-minilm | qwen3-0.6b | gemma | nomic |
|---|---|---|---|---|
| total weight delta | −55 | −197 | **−301** | −57 |
| memories challenged | **17** | 74 | **85** | 24 |
| queries mutating | **5/30** | 27/30 | 22/30 | 12/30 |
| weight lost on *negatives* | −23 | −74 | −132 | 0 |

Under the **current** configuration, 5 of 30 queries mutate the store,
including three that are pure negatives — `"chicken alfredo pasta recipe
with mushrooms"` cost −12 weight and 4 challenges; `"pandas dataframe
groupby sum example"` −4 and 2; `"how do I bake sourdough bread at high
altitude"` −7 and 3.

Cause: the contradiction scan (`hybrid_search.go` Phase 4) calls
`dm.ChallengeMemory` whenever cosine ≥ 0.85, with no logical-contradiction
test. Its three branches are state-collision, provenance-tier, and
equal-tier — and the equal-tier branch challenges `scanSet[i]`
*positionally*, with no inspection of what either memory claims.

**This is why §7's precondition (a) is load-bearing**: a stronger model
cannot be evaluated on ranking metrics alone while the threshold is fixed,
because its better discrimination also multiplies spurious challenges.

## 5. Cost

| | all-minilm | qwen3-0.6b | gemma | nomic |
|---|---|---|---|---|
| embed 381 texts | **11.7 s** | 456.0 s | 154.9 s | 95.6 s |
| mean query latency | **0.08 s** | 0.21 s | 0.10 s | 0.10 s |
| model size | **45 MB** | 639 MB | 621 MB | 274 MB |

Measured on 12 vCPU / 30 GB, all models local via Ollama on CPU.

## 6. Query instructions — instruction-aware results

`OllamaProvider.Embed` posts `{"model", "input"}` with **no role
distinction**: a document and a query take the identical path. For
asymmetric models that is a documented requirement being violated. Measured
directly — corpus held fixed, only the query string varied, raw cosine
top-10 (so these MRRs differ from §1, which uses MPM's ranker):

```
 minilm2  plain     MRR=0.7261 R@1=0.6087   control — no documented prefix
  qwen3b   plain     MRR=0.7754 R@1=0.6957
  qwen3b   prefixed  MRR=0.7091 R@1=0.6087   <- Instruct prefix HURTS (-0.066)
  gemma    plain     MRR=0.7261 R@1=0.6522
  gemma    prefixed  MRR=0.7696 R@1=0.7391   <- `query:` helps (+0.043)
  nomic    plain     MRR=0.7826 R@1=0.6957
  nomic    prefixed  MRR=0.8043 R@1=0.7391   <- `search_query:` helps (+0.022)
```

Three distinct regimes:

- **`nomic` / `gemma`** — task prefixes help. MPM is currently leaving
  +0.022 and +0.043 MRR unclaimed.
- **`qwen3-embedding`** — the Instruct prefix *hurts* (−0.066). An
  instructed query against plain documents is a mismatch; qwen3 wants
  matched formatting on both sides.
- **`all-minilm`** — no prefix exists. Correct as-is.

Note Ollama's HTTP API requires **bare** model names; `provider:model`
forms return 404.

## 7. Recommendation

**Leading candidate: `nomic-embed-text`** — best MRR, best nDCG@10, best
paraphrase retrieval, smallest non-minilm footprint (274 MB), and its
irrelevant mean (0.386) is low enough that negative queries cost it zero
weight. `qwen3-embedding:0.6b` is second on relevance at ~6× the embedding
cost and with a scope regression.

**Do not migrate yet.** Two preconditions:

**(a) Retrieval-side challenges must be separated from retrieval.** The
0.85 constant is fitted to `all-minilm`'s noise floor (0.063). Under
`nomic` (0.386) a fixed 0.85 fires on 12/30 queries. Adopting a stronger
model *before* the write path is separated would buy ranking gains and
inherit an unmeasured integrity loss. `nomic`'s own numbers are the
argument for doing this first, not against it.

**(b) The query-instruction split must be added** if `nomic` or `gemma` is
adopted — +0.022 and +0.043 MRR are currently unclaimed, and the
provider change is what makes those gains reachable.

After (a), re-run this benchmark and decide. Migration itself is then a
re-embed plus a threshold recalibration keyed to the model fingerprint —
not a policy change.

## Reproducing

```bash
python3 /tmp/embench/build_snapshot.py <slug>   # isolate + re-embed
python3 /tmp/embench/run_ext.py       <slug>     # 30 queries
python3 /tmp/embench/qdist.py         <slug>     # scan-set pair exposure
python3 /tmp/embench/simdist.py       <slug>     # all-pairs distribution
python3 /tmp/embench/prefix_test.py              # instruction sensitivity
```

Results: `/tmp/embench/{report,scores,qdist,simdist,prefix_test}.json`.