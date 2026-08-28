# Retrieval Correctness Audit — 2026-08-26

**Subject:** HybridSearch vector-only candidate preservation (post-fix verification)
**Fix commits under audit:**
- `91a0de5` — fix(hybrid_search): preserve vector-only candidates in merge
- `59f2cfe` — fix(core): populate vector-only metadata + scan-safe NullString JSON binding
**Audit target:** `internal/core/hybrid_search.go`
**Constraint:** No query expansion, no new embeddings, no model changes, no Test 17 query modifications, no F7.1/F8.1 special cases.

---

## 1. Structural Verdict

**PASS** — The HybridSearch fix at `91a0de5` is structurally correct, complete, and resistant to regression under the constraints of this audit. The follow-up at `59f2cfe` closes two silent-failure modes discovered during the audit:

1. **Vector-only rows surfaced with zero-valued display fields.** Weight, Collection, Tags, Metadata, ReinforcementCount, and CreatedAt were left at zero/empty because the FTS5 path was the only metadata source. Fixed by a batched SELECT (`loadVectorOnlyMeta`) that pre-loads the field set for every vector-only candidate before the merge step runs.
2. **`loadVectorOnlyMeta` silently dropped rows with nullable JSON columns.** Direct string scanning panicked on `NULL` for `tags`/`metadata` JSON columns, the inner `continue` swallowed the error, and `out[id]` was never populated. Fixed by `sql.NullString` / `sql.NullFloat64` for nullable scalars per the 2026-08-17 Substrate Defense Triad.

The candidate-union invariant (`Candidates(HybridSearch) ⊆ Candidates(BM25) ∪ Candidates(Vector)`) now holds in all branches: hybrid (both channels), fts5-only, and vector-only. The merge step explicitly preserves vector-only candidates via a 3-way `if ftsOK && vecOK / else if ftsOK / else` branch — no candidate is dropped after the merge.

---

## 2. Candidate-Flow Analysis

### Sources feeding the merge step

| Source | Function | Returns | Empty-when |
|---|---|---|---|
| FTS5 keyword | `searchFTS5` / `searchLike` | `[]ftsEntry` | No token overlap with corpus (BM25 implicit-AND with porter unicode61) |
| Vector | `dm.VectorMatch` (IVFSearch → brute-force fallback) | `[]VectorMatch` | `Provider.Name() == "null"` (no embedding provider) |
| Vector-only enrichment | `loadVectorOnlyMeta` (new) | `map[string]vectorOnlyMetaRow` | No vector candidates OR batched SELECT failed |

### Branch semantics (post-fix)

```go
if ftsOK && vecOK {
    // Both — full hybrid
    ftsScore = fts.Score
    vecSim = vec.similarity
    combinedScore = hybridScore(fts.Score, vecSim, cfg.VectorWeight)
    source = "hybrid"
} else if ftsOK {
    // FTS5 only
    ftsScore = fts.Score
    combinedScore = fts.Score * (1 - cfg.VectorWeight)
    source = "fts5"
} else {
    // Vector only (no FTS5 match) — preserved
    vecSim = vec.similarity
    combinedScore = vec.similarity * cfg.VectorWeight
    source = "vector"
}
```

The `else` branch is the load-bearing fix. Before commit `91a0de5` this branch held `continue;`, silently discarding every paraphrased query's vector signal.

### Invariant verified

- **Vector-only path:** `vecResults` populated, `ftsResults` empty → `else` branch fires → row enters `combined` with `source="vector"`. Verified by `TestHybridSearch_VectorOnlyCandidatePreserved` and `TestHybridSearch_VectorOnlyAtVWOne`.
- **BM25-only path:** `ftsResults` populated, `vecResults` empty → `else if ftsOK` branch fires → row enters `combined` with `source="fts5"`. Verified by `TestHybridSearch_BM25OnlyStillWorks`.
- **Dual-channel path:** both populated → `if ftsOK && vecOK` branch → `source="hybrid"`. Verified by `TestHybridSearch_BothChannelsMerge`.
- **NullProvider graceful degradation:** `provider.Name() == "null"` → `vecResults` stays empty → only FTS5 branch reachable. Verified by `TestHybridSearch_BM25OnlyStillWorks` running with no provider and the existing `TestHybridSearch_*` suite passing unchanged.

### Threshold semantics

The `RetrievalThreshold` filter (post-fix) applies uniformly:
- `hybrid`: `combinedScore < RetrievalThreshold` → drop (combined score is 0–1 from BM25+vector)
- `vector-only`: `combinedScore < RetrievalThreshold` → drop (combined score = similarity × VectorWeight, also 0–1)
- `fts5-only`: threshold NOT applied (BM25 is unbounded negative; threshold of `-3.0` would filter almost everything)

This matches the canonical `RetrievalThreshold` semantics from the original design and is verified by `TestHybridSearch_VectorOnlyThresholdFilter`.

---

## 3. Result-Integrity Audit

### Pre-fix silent-failure modes (discovered and fixed in `59f2cfe`)

| Field | Pre-fix behavior | Post-fix |
|---|---|---|
| `Weight` | Always `0.0` for vector-only rows | Populated from batched lookup |
| `Collection` | Empty string `""` | Populated from `memory_row.collection` |
| `Tags` | Empty string `""` (after NULL scan panic + silent `continue`) | Populated via `sql.NullString` |
| `Metadata` | Empty string `""` | Populated via `sql.NullString` |
| `CreatedAt` | `0` | Populated from `memory_row.created_at` |
| `ReinforcementCount` | `0` | Populated from `memory_row.reinforcement_count` |
| `IsChallenged` | `false` | Extracted from `metadata["status"] == "challenged"` via batched lookup |
| `IsConceptDrift` | `false` | Extracted from `metadata["concept_drift"] == true` via batched lookup |
| `ChallengedTheoryID` | Empty string `""` | Extracted from `metadata["challenged_theory_id"]` |
| `Rationale` | Hardcoded `"hybrid fts5+vec"` regardless of source | Source-aware via `rationaleForSource()` |

### Post-fix verification

- `TestHybridSearch_VectorOnlyFieldsPopulated` — confirms Content/Collection/Weight=7/ReinforcementCount=4/Tags/Metadata/CreatedAt all populated from batched lookup. PASS.
- `TestHybridSearch_VectorOnlyChallengeSignalPopulated` — confirms `IsChallenged=true` and `ChallengedTheoryID="abc123"` extracted from batched metadata. PASS.

### SQL scan panic (fixed in `59f2cfe`)

The original `loadVectorOnlyMeta` did:
```go
var id, content, collection, tags, metadata string
if err := rows.Scan(&id, &content, &collection, &tags, &metadata, ...); err != nil {
    continue  // SILENT — drops the row entirely
}
```

When any of `tags`/`metadata` is SQL `NULL` (very common — the `tags` column has no default and the test rows insert only metadata), the Scan panics with `"converting NULL to string is unsupported"`. The `continue` swallows the error and `out[id]` is never populated. The merge step then falls through to the field-population `else if meta, ok := vectorOnlyMeta[id]; ok { ... }` branch — but `ok` is false because the map was never populated, so all vector-only rows get zero-valued fields.

**Fix:** `sql.NullString` for nullable JSON columns and `sql.NullFloat64` for nullable scalars, with `Valid` checks converting NULL to empty string / 0.0. Aligns with the 2026-08-17 Substrate Defense Triad pattern: "For nullable scalar columns … use `sql.NullString` / `sql.NullInt64` rather than concrete types."

### Recall rationale string

The recall handler previously hardcoded:
```go
Rationale: "hybrid fts5+vec weight=...",
```

regardless of source. This was misleading — vector-only results would report themselves as hybrid. Fixed via `rationaleForSource()`:
- `vector` → `"vector-only similarity=X.XXX"`
- `fts5` → `"fts5-only bm25-weighted=X.XX"`
- `hybrid` → `"hybrid fts5+vec weight=X.XX"`

---

## 4. Ranking Audit

### Source priority (current ordering)

```
hybrid > fts5 > vector
```

Within each source:
- `hybrid`: descending by `combinedScore` (BM25-weighted + vector-weighted)
- `fts5`: ascending by BM25 (more negative = better match for FTS5)
- `vector`: descending by `cosine similarity`

This matches the canonical ordering documented in commit `91a0de5` ("Sort ranks hybrid > fts5 > vector; within each source, FTS5 sorts ascending BM25, vector sorts descending cosine, hybrid sorts descending combined").

### Ordering determinism

For queries that produce the same candidate set on repeated invocations, the ordering is deterministic because:
- `ftsResults` from `searchFTS5` is ordered by BM25 ascending (SQLite FTS5 default)
- `vecResults` from `VectorMatch` is ordered by similarity descending
- `combined` is built from a `map[string]bool` union → iteration order is non-deterministic in Go but `sort.Slice` is applied after construction with stable key `(sourceRank, primaryScore)`

`sourceRank()` helper:
```go
func sourceRank(s string) int {
    switch s {
    case "hybrid": return 3
    case "fts5":   return 2
    case "vector": return 1
    }
    return 0
}
```

### VectorWeight semantics (verified)

| VectorWeight | Behavior |
|---|---|
| `0.0` | Pure BM25 (`hybridScore = ftsScore * 1.0 + vecSim * 0.0`) |
| `0.5` | Equal blend (default) |
| `1.0` | Pure vector (`ftsScore * 0.0 + vecSim * 1.0`) |

- `TestHybridSearch_VectorOnlyAtVWOne` (VW=1.0) — PASS, vector-only candidates surface.
- `TestHybridSearch_BM25OnlyStillWorks` (VW=0.0) — PASS, FTS5-only paths rank by BM25.
- `TestHybridSearch_BothChannelsMerge` (VW=0.5) — PASS, both signals combined.

---

## 5. Regression Audit

The fix at `91a0de5` would re-regress if any future change reintroduced `continue` (or equivalent drop) in the `else` branch of the merge step. The fix at `59f2cfe` would re-regress if anyone re-introduced concrete `string` scanning on nullable JSON columns without the NullString guard.

### Regression net — proof by re-introduction

I temporarily reverted the merge fix (replaced the `else` branch with `continue`) and ran the new regression suite:

```
=== RUN   TestHybridSearch_VectorOnlyCandidatePreserved
--- FAIL: TestHybridSearch_VectorOnlyCandidatePreserved (0.09s)
=== RUN   TestHybridSearch_VectorOnlyAtVWOne
--- FAIL: TestHybridSearch_VectorOnlyAtVWOne (0.09s)
=== RUN   TestHybridSearch_VectorOnlyThresholdFilter
--- FAIL: TestHybridSearch_VectorOnlyThresholdFilter (0.07s)
=== RUN   TestHybridSearch_VectorOnlyFieldsPopulated
--- FAIL: TestHybridSearch_VectorOnlyFieldsPopulated (0.08s)
=== RUN   TestHybridSearch_VectorOnlyChallengeSignalPopulated
--- FAIL: TestHybridSearch_VectorOnlyChallengeSignalPopulated (0.07s)
```

**All 5 regression tests fail** when the bug is reintroduced. After restoring the fix, all 7 HybridSearch tests pass:

```
--- PASS: TestHybridSearch_VectorOnlyCandidatePreserved (0.09s)
--- PASS: TestHybridSearch_VectorOnlyAtVWOne (0.06s)
--- PASS: TestHybridSearch_BM25OnlyStillWorks (0.10s)
--- PASS: TestHybridSearch_BothChannelsMerge (0.07s)
--- PASS: TestHybridSearch_VectorOnlyThresholdFilter (0.10s)
--- PASS: TestHybridSearch_VectorOnlyFieldsPopulated (0.08s)
--- PASS: TestHybridSearch_VectorOnlyChallengeSignalPopulated (0.08s)
```

### Coverage gaps addressed

The original `hybrid_search_vector_only_test.go` had 2 tests. After this audit the file has 7, covering:
- Vector-only candidate preservation (the regression net for `91a0de5`)
- VW=1.0 behavior (extreme-vector path)
- BM25-only preservation (regression net for the existing path)
- Dual-channel merge (Source=hybrid)
- Threshold filter on vector-only
- Field population from batched lookup
- Challenge/concept-drift signal propagation through batched lookup

---

## 6. Test Results

### New regression suite (`internal/core/hybrid_search_vector_only_test.go`)

| Test | Verifies | Result |
|---|---|---|
| `TestHybridSearch_VectorOnlyCandidatePreserved` | Paraphrased query → vector-only row returns | PASS |
| `TestHybridSearch_VectorOnlyAtVWOne` | VW=1.0 surfaces vector-only | PASS |
| `TestHybridSearch_BM25OnlyStillWorks` | FTS5 strong hit ranks #1 under VW=0 | PASS |
| `TestHybridSearch_BothChannelsMerge` | Dual-channel = Source=hybrid, both scores populated | PASS |
| `TestHybridSearch_VectorOnlyThresholdFilter` | RetrievalThreshold filters vector-only when above similarity | PASS |
| `TestHybridSearch_VectorOnlyFieldsPopulated` | Weight/Collection/Tags/Metadata/RC/CreatedAt populated | PASS |
| `TestHybridSearch_VectorOnlyChallengeSignalPopulated` | IsChallenged + ChallengedTheoryID extracted from metadata | PASS |

### Test 17-Semantic benchmark (`internal/core/test17_semantic_benchmark_test.go`)

Real Ollama embeddings, 6 memories, 19 paraphrased queries:

```
=== Semantic Archaeology Benchmark (Test 17-Semantic) ===
Corpus: 6 memories (2 targets, 4 decoys)
Queries: 19 paraphrased

Per-query results:
  Class | Rank@BM25 | Rank@Vector | Rank@Hybrid | Query
  A     |     1     |      1      |      1      | why does restoring a challenged memory not make it trusted again
  ... (18 queries at rank 1, 1 query at rank 2)

Aggregate Recall:
  Mode    | Recall@1 | Recall@3 | Recall@5
  BM25    |   94.7%  |  100.0%  |  100.0%
  Vector  |   94.7%  |  100.0%  |  100.0%
  Hybrid  |   94.7%  |  100.0%  |  100.0%
```

The 94.7% Recall@1 corresponds to 1 of 19 paraphrased queries ranking the target at #2 (a known semantic gap where the target's content semantically overlaps with the decoy's content — not a regression). Recall@3 and Recall@5 are at 100%.

### Full test suite

| Package | Result |
|---|---|
| `github.com/flowbyte-com/mpm-core` (48.4s) | PASS |
| `core/capability` | PASS |
| `core/config` | PASS |
| `core/logging` | PASS |
| `core/mpmcli` | PASS |
| `core/orchestration` | PASS |
| `core/renderers` | PASS |
| `core/seed` | PASS |
| `core/seed/cap` | PASS |
| `core/synth` | PASS |
| `core/usererror` | PASS |
| `cmd/mpm` | PASS |
| `cmd/mpm-mcp` | PASS |
| `cmd/mpm-telemetry` | PASS |
| `internal/audit` | PASS |
| `internal/blobstore` | PASS |
| `internal/critic` | PASS |
| `internal/pointer` | PASS |
| `internal/scheduler` | PASS |
| `internal/telemetry` | PASS |

**Total: 20 packages, 0 failures.**

---

## 7. Remaining Issues

### Known limitations (not regressions, not in scope)

1. **The 1-of-19 paraphrase miss in Test 17-Semantic.** A single paraphrased query in the C-class ranks the target at #2 because its semantic embedding is closer to a decoy than the target. This is a property of the embedding model (all-MiniLM-L6-v2 with 384-dim cosine), not a HybridSearch bug. Out of scope per audit constraints (no model changes, no query modifications).

2. **The `vecResults, err = dm.VectorMatch(...)` error swallow.** The `// VecResults already sorted by similarity; ignore error and continue` comment in `hybrid_search.go:139-141` means a transient VectorMatch failure silently degrades to "no vector candidates." This is a design choice for graceful degradation (FTS5 still works) but operators should monitor `mpm call search_memories` success rates to detect persistent VectorMatch failures. Not a regression — pre-existing pattern.

3. **`loadVectorOnlyMeta` issues one query for all vector candidates.** At 5000+ vector candidates this becomes O(N) in SQL roundtrips per search. For typical workloads (≤100 vector candidates) this is fine; at 5K+ consider paginated enrichment. Out of scope — not introduced by this audit.

### No regressions introduced

- Pre-existing FTS5 path (TestHybridSearch_BM25OnlyStillWorks) — unchanged behavior.
- Pre-existing dual-channel path — unchanged behavior, both scores now correctly displayed in Source=hybrid results.
- Pre-existing NullProvider graceful degradation — unchanged.
- Pre-existing `mpm recall --semantic` CLI behavior — improved (rationale string now correct).

---

## 8. Final Release Assessment

**Status: APPROVED for alpha RC inclusion.**

The HybridSearch vector-only candidate preservation fix (`91a0de5`) is structurally correct and sufficiently hardened to become the canonical semantic-discovery implementation. The follow-up fix (`59f2cfe`) closes two silent-failure modes (zero-valued vector-only display fields and NULL-JSON-column scan panics) discovered during the audit.

### What changed

| Commit | What | Why | Risk |
|---|---|---|---|
| `91a0de5` | Vector-only candidates preserved in merge | Paraphrased queries produce zero FTS5 hits; vector signal was being discarded | LOW — new code path, no existing behavior modified |
| `59f2cfe` | (a) Batched metadata lookup for vector-only rows, (b) `NullString`/`NullFloat64` scan, (c) source-aware rationale string | (a) Display fields silently zeroed for vector-only rows; (b) row silently dropped on nullable JSON scan panic; (c) all results reported as "hybrid" regardless of source | LOW — additive enrichment + scan-safety guard, no behavior change for hybrid/fts5-only paths |

### Why this is release-ready

1. **Candidate-union invariant holds.** No branch in the merge step drops a row that came from FTS5 or VectorMatch (verified by re-introduction regression net).
2. **All 7 vector-only + hybrid regression tests pass.**
3. **Test 17-Semantic shows 94.7% Recall@1 and 100% Recall@3/5 across 19 paraphrased queries with real Ollama embeddings** — no regression vs the original `91a0de5` measurement.
4. **All 20 packages in the test suite pass** — no collateral damage.
5. **The two silent-failure modes (`continue` in the merge + Scan-panic swallowed) are now both structurally defended** — one by a 3-way branch with an explicit vector-only path, the other by NullString/NullFloat64 per the Substrate Defense Triad.

### Operator action

No action required. The fix is backward-compatible: FTS5-only callers see identical behavior, hybrid callers see identical behavior (with more accurate rationale strings), vector-only callers now correctly receive populated results where they previously received silent zero-value rows.

### Future hardening (out of scope)

- An ANN index would replace brute-force vector scan for corpora >5K rows. See WISHLIST.md.
- A mpm-lint --gate check for `*sql.DB` + `string` scan on nullable JSON columns would catch the NULL-JSON scan panic class automatically. Currently enforced by review.

---

**Audit signed off:** 2026-08-26 20:21 UTC
**Commits under audit:** `91a0de5`, `59f2cfe`
**Audit verdict:** APPROVED
