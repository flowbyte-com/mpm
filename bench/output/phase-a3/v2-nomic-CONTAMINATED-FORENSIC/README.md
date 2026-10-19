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

## B7 review (HAL 2026-10-19) — corrected embedding-state
explanation

The previous draft of this README claimed:

> "The seed rows had no `embedding` column set, so they had not been
> embedded; they were nevertheless returned by the raw-cosine query
> because the SQL was unfiltered."

This claim is **incorrect**. The pre-fix raw-cosine SQL read `WHERE
embedding IS NOT NULL AND deleted_at IS NULL`; rows lacking embeddings
could not have been returned by that query.

The four facts that ARE independently provable from code:

1. **The seed rows appeared in the raw-cosine output.** This is
   directly observable in the preserved `report.json`:
   - 7 `mpm-seed-*` rows in the unprefixed top-k.
   - 5 `mpm-seed-*` rows in the `search_query:` prefixed top-k.

2. **The pre-fix SQL required a non-NULL embedding.** The bench
   runner's candidate-vector query was:

   ```sql
   SELECT id, embedding FROM memories
   WHERE embedding IS NOT NULL AND deleted_at IS NULL
   ```

   Any row that returned from it must have had `embedding IS NOT NULL`
   at query time.

3. **The collection filter was absent in pre-fix code.** The
   candidate-vector SQL had no `AND collection = ?` clause (the
   collection parameter was added in commit `8df87bf`, this branch's
   B1 fix). With the filter added, the seed rows are excluded because
   they belong to the `directives` collection, not `bench_v2_corpus`.

4. **The corrected run excludes the seed rows.** The B1 re-run with
   the collection filter produces 0 `mpm-seed-*` rows in all three
   conditions (raw-cosine unprefixed, raw-cosine prefixed, hybrid
   ranker).

The pre-fix `insertSeedRow` writer
(`internal/core/seed/engine.go:147-168`) does not set the
`embedding` column, so the rows are inserted with `embedding =
NULL`. The `mpm ops backfill-embeddings` writer
(`cmd/mpm/backfill_embeddings.go:421`) selects rows by
`(embedding IS NULL OR embedding = 'null') AND deleted_at IS NULL`
with no exclusion on `collection`, so it WOULD backfill directive
rows if it was ever run against the live database after seed
insertion.

Therefore, for the seed rows to have appeared in the raw-cosine
output under the pre-fix SQL filter, they must have had a non-NULL
embedding at the time of the bench measurement. The exact sequence
of operations on the live database between seed insertion and the
bench measurement (which backfill pass, if any, populated the
embeddings) is not independently reconstructible from the bench-run
artifacts alone — no preserved snapshot of the live database's
`memories.embedding` column state at the time of seed insertion is
available in this repository. We state only the four provable facts
above and the inferred constraint; we do not assert a specific
backfill timing or writer.

## What this directory preserves

- `report.json` — the original `bench/output/phase-a3/v2-nomic/report.json`
  from the Phase A.3 run (commit `687eb4f`). Contains both the
  (still valid) hybrid ranker per-query table and the (INVALID) raw-
  cosine per-query tables.
- `report.md` — the original human-readable report.
- `README.md` — this file.
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