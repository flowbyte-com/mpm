# Embedding Model Benchmark — `all-minilm` vs `qwen3-embedding:0.6b`

Compares the installed embedding model (`ollama:all-minilm`, 384-dim) against
`ollama:qwen3-embedding:0.6b` (1024-dim) on identical relevance queries.

**Outcome: a trade-off, not a clear improvement. Recommendation: keep
`all-minilm`.** qwen3 wins on paraphrase recall and loses on scope precision,
mutation safety, latency, storage, and operational risk. The reasoning is
below; the single most important finding is in §5.

## Method

Two fully isolated workspaces under `/tmp/embench/{minilm,qwen3}/`, each a
SQLite `.backup` snapshot of the live database (database-consistent, not a
file copy — the live DB is in WAL mode).

Each workspace was **re-embedded from scratch** by its own model, so no query
could ever be scored against vectors from the other model:

| workspace | memories | reference chunks | dimension | `embedding_model` |
|---|---|---|---|---|
| `minilm` | 149 | 232 | 384 | `ollama:all-minilm` |
| `qwen3` | 149 | 232 | 1024 | `ollama:qwen3-embedding:0.6b` |

Every stored vector length was asserted equal to the model's probed
dimension: **0 dimension mismatches in either workspace**, and
`memories.embedding_dimension` was populated (the live store leaves it NULL
for all 149 rows). Restores between queries came from each workspace's own
pristine template, never from the live DB — restoring from live would have
silently reintroduced all-minilm vectors into the qwen3 snapshot.

**The live store was never modified.** Post-run: integrity `ok`, 149 memories,
232 reference embeddings, `embedding_dimension` still NULL for all memories,
`embedding_model` still `ollama:all-minilm`. The live embedding model was not
replaced.

Harness validation: the minilm run reproduced the previously documented
all-minilm baseline **exactly** (exact 0.900 / multi 1.000 / paraphrase 0.260
/ contradiction 0.750 / scope 1.000), confirming the harness measures what the
earlier benchmark measured.

## 1. Relevance metrics (22 queries, identical corpus)

| category | metric | all-minilm | qwen3-0.6b | change |
|---|---|---|---|---|
| exact | MRR | 0.900 | **1.000** | +0.100 |
| exact | R@1 | 0.80 | **1.00** | +0.20 |
| multi | MRR | 1.000 | 1.000 | — |
| paraphrase | MRR | 0.260 | **0.517** | **+0.257** |
| paraphrase | R@1 | 0.00 | **0.40** | **+0.40** |
| paraphrase | R@5 | 0.60 | **0.80** | +0.20 |
| scope | MRR | **1.000** | 0.583 | **−0.417** |
| contradiction | MRR | 0.750 | 0.750 | — |

The paraphrase gain is real and is the case semantic retrieval exists for.
B1 ("how do I stop the attachments from breaking") moved rank 2 → 3 but with
a much higher absolute similarity (0.252 → 0.541); B2 moved rank 2 → 1; B5
moved rank 5 → 1; B3 moved rank 10 → 4.

The scope regression is equally real: F1 ("MPM repository location managed
instruction architecture") fell from **rank 1 to rank 6**.

## 2. Cost

| | all-minilm | qwen3-0.6b |
|---|---|---|
| model size on disk | 45 MB | **639 MB** (14×) |
| full re-embed (149 mem + 232 chunks) | 13 s | **345 s** (26×) |
| mean query embedding latency | 0.08 s | 0.12 s (1.5×) |
| mean `recall --semantic` wall time | 0.11 s | 0.16 s (1.5×) |
| snapshot DB size | 12 MB | 16 MB |

Query latency is fine in both cases — retrieval itself is vector math, not
model inference, and 0.16 s is imperceptible. The cost that matters is the
**26× re-embed time**, which is paid on every model change and every
backfill.

## 3. Does MPM support 1024 dimensions?

**Yes, structurally.** No dimension is hardcoded anywhere. `ActiveEmbeddingDimension`
(`internal/core/reference_db.go:431`) detects dimension by sampling a real
embedding and caches it per model name, so 1024 flows through storage,
indexing and retrieval without code changes. Dimension equality is enforced
at comparison time by `validEmbeddingPair` (`db.go:4176`), which rejects
mismatched and non-finite pairs rather than comparing over a shared prefix.

The one gap is the model fingerprint itself — see §5.

## 4. Query/document formatting

Qwen3 is a Matryoshka model that officially recommends task instructions on
the **query** side (documents are embedded bare). MPM calls Ollama's
`/api/embed` with a bare `input` string (`internal/core/embeddings.go:146`),
so **no instruction prefix is applied**.

This matters, and it cuts both ways:

| | relevant | noise |
|---|---|---|
| qwen3 bare input | 0.678 | 0.356 |
| qwen3 with query instruction | 0.576 | 0.198 |

The instruction lowers *both* scores but suppresses noise more. Ollama does
not apply it automatically for `/api/embed` with a plain string. Adding it
would require a code change to the Ollama provider and is **out of scope for
this comparison** — the numbers above are bare-input, which is what MPM would
actually produce today.

## 5. The decisive finding: qwen3 triggers spurious memory challenges

Running the benchmark against the qwen3 snapshot, **4 memories had their
weight decayed** and `auto_capture` evidence rows written:

```
06e5021c28c75f3a  weight 5.0 → 2.0
459b4fa746c4de2b  weight 5.0 → 3.0
56737933768bb273  weight 5.0 → 3.0
```

`all-minilm` caused **zero**. This is `ChallengeMemory()` firing from
`HybridSearch` at cosine ≥ 0.85 (`docs/RECALL-MUTATION.md`) — and it is
**not** a benchmark artifact, because the qwen3 workspace had already been
restored to pristine state before each query.

The cause is visible in the similarity distribution:

| | relevant mean | noise mean | gap |
|---|---|---|---|
| all-minilm | 0.454 | 0.147 | **0.307** |
| qwen3-0.6b | 0.662 | 0.471 | **0.191** |

qwen3 compresses the distance between relevant and irrelevant content.
That improves top-1 precision (a 0.55 floor keeps 16/18 relevant with 0/3
noise, versus all-minilm's 8/18) but it also **raises the baseline**, pushing
unrelated memory pairs toward the 0.85 challenge threshold. `ChallengeMemory`
has no env-var escape hatch, so this happens on ordinary `mpm recall
--semantic` — a read the user believes is observational.

Switching models would therefore change not just retrieval quality but
**retrieval side effects**: correct memories silently losing weight, with
evidence rows attributing the loss to a call that only read them. That is a
data-integrity regression, not a quality trade-off, and it is not something
a threshold can fix without also suppressing the paraphrase recall that makes
qwen3 attractive in the first place.

## 6. Recommendation

**Keep `ollama:all-minilm`. Do not migrate the live store to Qwen3.**

The paraphrase improvement (+0.257 MRR) is genuine but is bought with:

1. **Silent weight decay on 4 of 149 memories during a 22-query read-only
   benchmark** — unacceptable given `ChallengeMemory` has no opt-out (§5).
2. A **scope-precision regression** (1.000 → 0.583, F1 rank 1 → 6).
3. 14× model size and 26× re-embed time.
4. A noise floor (0.471) that sits above paraphrase-level relevance for
   `all-minilm` (0.159–0.244) — meaning the calibration in
   `RETRIEVAL-BENCHMARK.md` §Defect 5 does not transfer.

If qwen3 is revisited, the preconditions are: (a) a `ChallengeMemory`
similarity threshold calibrated per model rather than fixed at 0.85, (b) the
query-instruction prefix added to the Ollama provider, and (c) re-running the
§5 mutation check to show zero spurious challenges. Until all three hold,
switching trades a measurable relevance gain for an unmeasured integrity loss.

## Reproducing

The historical 22-query benchmark scripts under `/tmp/embench/`
are no longer present in the repository. The corpus and
hand-labelled relevance judgments that drove the historical run
were not preserved as artifacts; the reproduction recipe
above is therefore **not** runnable against the current
repository state. The four memories that lost weight during
the qwen3 pass (noted in the §5 mutation check) cannot be
re-identified from the lost corpus.

The current harness (Issue #16) does not reproduce the historical
22-query qwen3-vs-all-minilm comparison numbers. To reproduce
them, the original 22-query corpus and its labels would have
to be reconstructed; the new harness ships a small synthetic
labelled set under `bench/corpus/v1/` instead. The manifest at
`bench/corpus/MANIFEST.json` records this in its `note` field.

What can be reproduced today, against the current repository:

```bash
# Self-tests: no mpm binary, no live DB, no network.
python -m bench selftest

# Smoke test: fake embeddings server, disposable DB, FTS5 mpm build.
CGO_CFLAGS=-DSQLITE_ENABLE_FTS5=1 CGO_LDFLAGS=-lm \
    go build -tags fts5 -o mpm ./cmd/mpm
python -m bench smoke
```

See [`EMBEDDING-BENCHMARK-HARNESS.md`](./EMBEDDING-BENCHMARK-HARNESS.md)
and [`EMBEDDING-BENCHMARK-HARNESS-DESIGN.md`](./EMBEDDING-BENCHMARK-HARNESS-DESIGN.md)
for the user-facing and design documentation.
