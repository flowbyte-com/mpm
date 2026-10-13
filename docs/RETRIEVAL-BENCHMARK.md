# Retrieval Benchmark — Findings and Fixes

This records the 22-query relevance benchmark that found the ranking
defects fixed in the `fix/retrieval-benchmark-findings` branch, and the
calibration behind the changes that were *not* made.

Reproducing: the benchmark runs against a **snapshot copy** of the live
database, never `~/.mpm/src/db/mpm.db`. As of 2026-10-13 semantic
retrieval is **write-free** — both the automatic `ChallengeMemory` weight
slashes and the `EnqueueContradiction` queue inserts were removed; see
`docs/RECALL-MUTATION.md`. The CLI still applies lexical-path access
reinforcement (`weight + 0.5`, rate-limited to hourly), and the harness
still runs against a copy so that the live store is verifiable untouched
regardless. The harness digests the copy before and after and reports
`READ-ONLY: confirmed`.

## Defect 1 — BM25 sign inversion in the hybrid blend (fixed)

`internal/core/hybrid_search.go`, `hybridScore`.

SQLite's `bm25()` is **negative-is-better**: the strongest match returns
the most negative number. The code normalised it with

```go
normFTS := ftsScore / (1 + math.Abs(ftsScore))   // sign-PRESERVING
```

which is not a sigmoid (despite the comment saying it was) and maps the
*best* lexical match to the *worst* normalised value. In a 22-query
benchmark, one query ranked bm25 `-22.57` **above** bm25 `-24.36`.

Because the inverted leg dragged strong matches negative, a hybrid row
could score *below* a vector-only row (`-0.164` vs `+0.221`) while still
being ranked first by the source bucket — so the number the UI displayed
was not monotonic with the order it displayed.

**Fix:** `normalizeBM25` divides by `-1` first, flipping the sign so
better matches are positive, then squashes into (0, 1):

| bm25 | normFTS | meaning |
|---|---|---|
| −24.365 | 0.961 | strongest |
| −8.797 | 0.898 | moderate |
| −0.300 | 0.231 | weakest |

Every hybrid leg is now non-negative, so a lexical match can only ever
*add* to the blend.

## Defect 2 — scores not comparable across sources (fixed)

The `fts5`-only branch used `fts.Score * (1-vecWeight)` — **raw**
unbounded BM25 — while the hybrid branch used the *normalised* form. The
same match therefore scored `-7.32` as an fts5 row and `-0.16` as a
hybrid row. Any consumer comparing `CombinedScore` across sources (the
`RetrievalThreshold`, the shared-scope premium, the CLI display) was
comparing two different scales.

**Fix:** the fts5 branch now uses `normalizeBM25(fts.Score) * (1-vecWeight)`,
the same 0–1 scale. The `fts5` source keeps its own comparator
(`FTS5Score` ascending), so its intra-source ordering is unchanged — the
source-bucket policy is preserved exactly.

## Defect 3 — `mpm recall --why` explained a formula nothing computes (fixed)

`cmd/mpm/recall.go` printed:

```
reinforcement=N (×2 = X)  weight=N (×1.5 = Y)  recency=Z
```

with a comment attributing it to `internal/hybrid_search.go`. No
comparator in the recall path reads either field. The arithmetic lived
only in `computeScore`, a 0–1 display value for the JSON payload.

This was measured, not assumed: neutralising `weight=1` and
`reinforcement_count=0` across every row changed **0 of 8** orderings,
byte-identical.

**Fix:** `--why` now reports the signals that actually ranked the row —
BM25 score and the rank SQL gave it on the lexical path, per-channel
scores on the semantic path — and labels reinforcement/weight as
`(not used for ranking)`. It is now wired into the `--semantic` text
path too, where it was previously a silent no-op. The LIKE fallback
reports that it has no BM25 rather than inventing one.

## Defect 4 — shared-scope weight tie-break unreachable (fixed)

`internal/core/memory_tools.go`, `applySharedPremium`. The comparator
returned early whenever both rows were FTS5-only, so the `Weight` and
`Collection` tie-breaks below it never ran — precisely the local-vs-shared
comparison they existed to serve, since the shared-scope premium produces
many rows inside one BM25 band.

**Fix:** BM25 remains primary; the comparator falls through to the
tie-breaks only when the BM25 scores are genuinely equal. Relevance is
never traded for weight.

## Defect 5 — semantic noise: investigated, remedy REJECTED

`mpm recall --semantic` returns 10 confident results for a nonsense
query. The cause is that `RetrievalThreshold: -3.0` can never fire
against a 0–1 vector similarity.

**Measured** with 22 benchmark queries plus 15 dedicated nonsense
queries (real `all-minilm` embeddings, 160 result rows):

| | n | vector similarity |
|---|---|---|
| relevant hits | 20 | 0.159 – 0.828 |
| noise hits | 140 | 0.030 – 0.307 |

The distributions **overlap**: the strongest noise row (0.307) is more
similar than the weakest legitimate row (0.159).

| floor | relevant kept | noise kept | noise filtered |
|---|---|---|---|
| 0.15 | 20/20 | 22/140 | 84% |
| 0.20 | 19/20 | 5/140 | 96% |
| 0.25 | 17/20 | 3/140 | 98% |

Every legitimate hit below 0.25 is a **paraphrase** — semantic
retrieval's entire reason to exist:

- 0.244 — "how do I stop the attachments from breaking" → the actual answer
- 0.212 — "what model does the system use for writing" → the actual answer
- 0.159 — "why is my hard drive no longer protected" → the actual answer

A floor of 0.20 would suppress a real answer. Raising the default trades
the confident-nonsense problem for a paraphrase-recall problem, which is
the worse failure. **The default is unchanged.** A caller who wants a
floor should set `RetrievalThreshold` above 0 deliberately, knowing the
0.159–0.244 band contains true positives. The calibration is recorded at
`DefaultHybridConfig`.

## Defect 6 — lexical AND semantics: unchanged

`BuildFTS5Query` joins prefix-wildcard tokens with spaces, so FTS5's
implicit AND applies. The benchmark shows the cost: adding a correct
term to a query can drop recall to zero ("pi.dev upstream catalog does
not contain flash" → 0 hits; "catalog" → 2 hits). This is a documented
trade-off, and there is no independent evidence for changing it, so
lexical AND is **preserved**.

## Results after the fixes

| mode | category | MRR | R@1 | R@5 | nDCG |
|---|---|---|---|---|---|
| lexical | exact | 0.800 | 0.80 | 0.80 | 0.800 |
| lexical | multi | 0.200 | 0.20 | 0.20 | 0.200 |
| lexical | paraphrase | 0.000 | 0.00 | 0.00 | 0.000 |
| lexical | contradiction | 0.500 | 0.50 | 0.50 | 0.815 |
| semantic | exact | 0.900 | 0.80 | 1.00 | 0.926 |
| semantic | multi | 1.000 | 1.00 | 1.00 | 1.000 |
| semantic | paraphrase | 0.260 | 0.00 | 0.60 | 0.388 |
| semantic | contradiction | 0.750 | 0.50 | 1.00 | 1.381 |
| semantic | scope | 1.000 | 1.00 | 1.00 | 1.000 |

**These metrics are identical before and after.** That is the expected
result, not a null one: the `sourceRank` bucket was already ranking
hybrid above fts5 above vector, which masked the inverted score in every
measured query. Re-ranking the same fixed candidate sets under a
sign-corrected pure blend reproduced the shipped ordering on **22/22**
cases — the two mechanisms were redundant, and only the broken one was
load-bearing.

What changed is that the score is now correct on its own terms:

| query | row | bm25 | before | after |
|---|---|---|---|---|
| A3 | MiniMax-M3.1-Flash provenance | −22.57 | −0.180 | **0.777** |
| A3 | Verified against MiniMax's API | −24.36 | −0.191 | **0.769** |
| C3 | OpenClaw MPM plugin IDs | −27.60 | −0.068 | **0.897** |

The A3 inversion is resolved: −22.57 now correctly ranks above −24.36.

## Known limitations

- The benchmark corpus is one user's memory store (~20 relevant rows).
  The sign fix is provable from the BM25 contract and does not depend on
  the corpus; the threshold calibration does, and is reported as such.
- The lexical path still sorts by `created_at DESC` after the SQL has
  ordered by BM25 (`recall.go`), discarding the lexical ranking. The
  preserved `lexicalRank` is surfaced by `--why` so the divergence is
  visible. Changing the primary ordering is a policy decision, not a
  correctness fix, so it was not made here.
- `norerelevant` queries still return 10 results under `--semantic`
  (see Defect 5 for why the default was left alone).