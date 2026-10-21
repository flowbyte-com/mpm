# Issue #15 — Phase B re-measurement (honest semantic-retrieval contract)

This directory is the committed machine-readable output of the Issue #15
Phase B re-measurement: the same `v2` corpus, the same embedding
profile, the same pinned invocation as the committed Phase A.3 baseline
(`../phase-a3/v2-nomic/`), re-run against the binary that contains the
Phase B contract change.

**Outcome: retrieval is bit-for-bit unchanged.** Mean MRR@10
0.9181547619047619, Recall@10 0.9875, nDCG@10 0.9269176160435514,
Precision@10 0.1075 — identical to the baseline — and **0 of 60
per-query rows differ**. Phase B changed the *semantics of the output*,
not the retrieval. No legitimate result was removed.

The full write-up — the contract, the two captured JSON envelopes, the
reasoning for rejecting the measured threshold and gap policies, the
before/after metrics and the acceptance-criterion closure table — is in
[`docs/ISSUE-15-PHASE-A-REPORT.md`](../../../docs/ISSUE-15-PHASE-A-REPORT.md)
§B0–§B9.

## What this directory preserves

- `v2-nomic/report.json` — the full report: the corpus provenance, the
  regeneration verification, the vector participation for positives and
  negatives, the raw-cosine (unprefixed and prefixed) arms and their
  delta, the labelled scoring aggregates, the 60 per-query rows, the
  isolation record and the binary provenance.
- `v2-nomic/report.md` — the human-readable rendering of the same run.
- `SHA256SUMS` — checksums for every file in this directory.

## Provenance of this run

| field | value |
|---|---|
| date | 2026-10-21 |
| model digest | `0a109f422b47e3a30ba2b10eca18548e944e8a23073ee3f3e947efcf3c45e59f` (`nomic-embed-text`, 768-dim) |
| corpus | `v2`, manifest sha256 `48fdb92d6dd28bb58e56dfba6c1e8267bf2a2450b7f990cd4ca4c97eb80c9add` |
| binary | `.build/bin/mpm`, sha256 `6bdf2f2b610285b7c08edf91b13fe3fa711fe841654d212aed776267f9d98ac6`, git head `cad636884e85636f5a25a1d728d7ea0e0efe76b4` |
| live store | not touched |

The `isolation.snapshot_workspace` and `isolation.output_dir` paths
recorded in the JSON are the disposable temp directories the run used.
The harness leaves them on disk after the run (that is the established
`bench/` convention — they are kept so a failed run can be inspected);
they hold only synthetic corpus data and are not required to read this
artefact. The same applies to the absolute paths under
`binary_provenance.path` and `corpus.manifest_path`: they record where
the run happened on the machine that produced it, and are not needed to
reproduce it.

**The binary sha256 is not reproducible across builds.** Go stamps the
VCS state into the binary, so the same source produces a different
binary depending on whether the working tree was dirty at build time.
The recorded sha256 identifies *the binary this run used*; rebuilding
and getting a different sha256 does not mean the measurement is not
reproducible. The measurement is what reproduces — exactly.

**The JSON is not byte-reproducible across runs** — those temp
directory paths carry random suffixes. Every *measurement* in it is
reproducible exactly; the file's checksum is not. `SHA256SUMS` pins
this committed copy.

## What differs from the baseline, and what does not

A full leaf-by-leaf diff of this `report.json` against
`../phase-a3/v2-nomic/report.json` finds **12 differing leaves, and no
measurement among them**. They are:

| Field | Why it differs |
|---|---|
| `corpus.manifest_path` | The run happened in a different worktree. |
| `isolation.binary_provenance.git_head`, `.mpm_version`, `.mtime_iso`, `.path`, `.resolved_path`, `.sha256`, `.size_bytes` | A different binary — this one contains the Phase B contract change. |
| `isolation.output_dir`, `output_dir` | The per-run output directory. |
| `isolation.snapshot_workspace`, `regeneration.snapshot_path` | The per-run disposable workspace. |

Unchanged: every aggregate (MRR@10, Recall@10, nDCG@10, Precision@10),
all 60 per-query rows, the raw-cosine arms and their delta, the
regeneration verification (`ok: true`), the snapshot logical-mutation
check (`clean`), and the participation record (40/40 positives
vector-participated with zero failures, 20/20 negatives
vector-participated, `correctly_empty` empty).

## Reproducing

```bash
make build
MPM_BENCH_COLLECTION=bench_v2_corpus python3 -m bench run \
  --output-dir /tmp/mpm-issue15-phaseb/v2-nomic \
  --config bench/fixtures/nomic-embed-text.json \
  --corpus-version v2 \
  --raw-cosine-prefix
```

The harness refuses to write into a protected root, so the output
directory must live outside the checkout; copy the resulting
`report.json` and `report.md` into `v2-nomic/` afterwards.

This run reproduces the committed Phase A.3 measurement
(`../phase-a3/v2-nomic/report.json`) **exactly** on all four aggregates
at depth 10, with 0 per-query differences across all 60 queries.
