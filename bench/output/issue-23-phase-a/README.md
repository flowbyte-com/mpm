# Issue #23 — Nomic instruction-prefix condition matrix (Phase A)

This directory is the committed machine-readable output of the Issue #23
prefix condition matrix: the four conditions A/B/C/D described in
[`bench/README.md`](../../README.md), run on 2026-10-21 against
`nomic-embed-text` on disposable workspaces.

**Outcome: every prefix condition is at or below the unprefixed
baseline.** MRR@10 0.9182 unprefixed → 0.9146 (`search_query: ` query
prefix) → 0.9113 (both roles prefixed) → 0.8933 (`search_document: `
document prefix only). The unprefixed baseline is retained: no
production prefixing, no re-embed.

The full write-up — fingerprint, corpus provenance, per-query
regressions, held-out split, reproduction command, and the explicit
statement that this is **not** comparable with the historical MRR
0.832 — is in
[`docs/ISSUE-23-PREFIX-CALIBRATION-REPORT.md`](../../../docs/ISSUE-23-PREFIX-CALIBRATION-REPORT.md).

## What this directory preserves

- `prefix-experiment-report.json` — the full report: the model
  fingerprint (including the provider's model digest), the corpus
  provenance (manifest sha256, generator, per-file content hashes),
  the per-condition fingerprint verification, the raw-cosine and
  hybrid aggregates by depth, the per-query rows, the
  baseline comparisons, the metric definitions, the isolation record
  and the reproduction command.
- `prefix-experiment-report.md` — the human-readable rendering of the
  same run.
- `SHA256SUMS` — checksums for all three files in this directory.

## Provenance of this run

| field | value |
|---|---|
| date | 2026-10-21 |
| model digest | `0a109f422b47e3a30ba2b10eca18548e944e8a23073ee3f3e947efcf3c45e59f` |
| corpus | `v2`, manifest sha256 `48fdb92d6dd28bb58e56dfba6c1e8267bf2a2450b7f990cd4ca4c97eb80c9add` |
| binary | `.build/bin/mpm`, sha256 `cba07a5f6aa7144782a233613b31a073bd24403dc28c239fcac4cea1c4848ae1`, git head `261d1c1d09cca39d226203962dc6377ffb4598f8` |
| live store | not touched (`isolation.live_store_touched: false`) |

The `isolation.workspaces` paths recorded in the JSON are the
disposable temp directories the run used. The harness leaves them on
disk after the run (that is the established `bench/` convention — they
are kept so a failed run can be inspected); they hold only synthetic
corpus data and are not required to read this artefact. The same
applies to the absolute paths under `binary_provenance.path` and
`corpus.manifest_path`: they record where the run happened on the
machine that produced it, and are not needed to reproduce it.

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

## Reproducing

```bash
make build
MPM_BENCH_COLLECTION=bench_v2_corpus \
  python3 -m bench prefix-experiment \
    --output-dir /tmp/mpm-bench-issue23 \
    --config bench/fixtures/nomic-embed-text.json \
    --corpus-version v2 \
    --conditions A,B,C,D
```

This run reproduces the committed Issue #15 A.3 measurement
(`../phase-a3/v2-nomic/report.json`) **exactly**: condition A matches
`raw_cosine_unprefixed_scoring` and condition B matches
`raw_cosine_prefixed_scoring` with a difference of 0.0 on all four
aggregates at depth 10 and 0 per-query MRR differences across all 60
queries.
