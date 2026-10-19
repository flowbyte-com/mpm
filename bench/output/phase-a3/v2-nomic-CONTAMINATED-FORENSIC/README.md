# Phase A.3 raw-cosine contamination — forensic evidence

This directory contains the original `bench/output/phase-a3/v2-nomic/`
report data preserved as forensic evidence of the B1 raw-cosine
collection contamination identified during the Phase A.3 HAL review
(2026-10-19).

## Status: INVALID for collection-scoped comparison

**DO NOT use these numbers for Phase A/B conclusions.** The
hybrid-ranker's per-query table is collection-scoped (via
`mpm recall --collection bench_v2_corpus`), but the raw-cosine
experiment's dimension-discovery and candidate-vector SQL queries
were unfiltered, so MPM's Baseline Cognitive Bootstrap prime-directive
rows in `directives` leaked into the raw-cosine top-k:

- 7 `mpm-seed-*` rows in the 600-slot raw-cosine unprefixed top-k
  (4 unique seed IDs: `mpm-seed-epistemic-compaction-policy`,
  `mpm-seed-read-wake-context`, `mpm-seed-session-end-cluster-triage`,
  `mpm-seed-wake-triage-policy`).
- 5 `mpm-seed-*` rows in the 600-slot `search_query:`-prefixed top-k.
- 0 `mpm-seed-*` rows in the hybrid ranker's per-query table.

The seed rows had no `embedding` column set, so they had not been
embedded; they were nevertheless returned by the raw-cosine query
because the SQL was unfiltered and the existing corpus row count
included them. Removing them changes the top-k composition for any
query whose unfiltered top-10 was within a few cosine points of a
seed row.

## What this directory preserves

- `report.json` — the original `bench/output/phase-a3/v2-nomic/report.json`
  from the Phase A.3 run (commit `687eb4f`). Contains both the
  (still valid) hybrid ranker per-query table and the (INVALID) raw-
  cosine per-query tables.
- `report.md` — the original human-readable report.
- `SHA256SUMS` (in the parent `phase-a3/` directory) — updated to
  reference the new paths.

## Replacement

The corrected raw-cosine measurements are written by re-running
`python -m bench run --config fixtures/nomic-embed-text.json
--output-dir <new-dir> --raw-cosine-prefix` against a disposable
snapshot under `/tmp/mpm-bench/`. See
`bench/output/phase-a3/v2-nomic/report.json` for the corrected
measurements, and `docs/ISSUE-15-PHASE-A-REPORT.md` for the
post-correction Phase A.3 addendum.
