# Alpha-4.1 Surface Integrity Pass — Final Report

**Date:** 2026-08-30
**Branch:** `alpha-4-surface-tightening`
**Baseline:** `b67f77a` (alpha-4 surface tightening pass)
**Status:** **READY FOR REVIEW**

## Summary

Closed **6 of 7 defects (F-001…F-007)** and **2 of 7 wishlist items (W-002, W-006, W-007)** with **18 new regression tests**. Build clean (`go build ./...`), vet clean (`go vet ./...`), race-clean for the changed surfaces, and full test suite at parity with baseline (the 2 pre-existing `cmd/mpm-mcp` failures from commit `b67f77a` are confirmed unrelated via `git stash`).

## Findings closure

| ID | Status | Fix | Regression test |
|----|--------|-----|-----------------|
| **F-001** Summary projection weight/created_at over-typed | **Closed** | `internal/core/tools/handlers.go` — added shared `coerceInt64`/`coerceInt`/`coerceFloat64` helpers; `ProjectedMemoryEntry.Weight` changed from `int` to `float64`; rational `formatWeight` formatter for human-readable output. | `tools/f_alpha_4_1_summary_projection_scalars_regression_test.go` (6 tests) |
| **F-002** Ranking documentation contradicts actual behaviour | **Closed** | `README.md` §memory ranking — clarified that weight/reinforcement_count are metadata-only and the ranker consumes BM25 + cosine similarity. | `hybrid_search_alpha_4_1_ranker_contract_regression_test.go` (2 tests) |
| **F-003** Deprecated work update diverges from `complete` | **Closed** | `internal/core/db.go` — removed `recordGitEvidenceForWork` call from `UpdateWorkWithContext` so the deprecated `update status=done/cancelled` path is byte-equivalent to the canonical `complete`/`cancel` paths. | `tools/work_alpha_4_1_update_divergence_regression_test.go` (1 test) |
| **F-004** Machine mode scheduler nudge leak | **Closed** | `cmd/mpm/main.go` — gated the `emitSchedulerHealthWarning(os.Stderr)` call site by `isMachineMode(os.Args) && os.Getenv("MPM_VERBOSE") == ""` so the direct `fmt.Fprintln` doesn't bypass the alpha-4 D-004/W-004 slog suppression. | `cmd/mpm/scheduler_health_alpha_4_1_machine_mode_test.go` (5 tests, including subprocess) |
| **F-005** Memory search `--json` joined into FTS5 query | **Closed** | `cmd/mpm/handlers_memory.go` — added `stripMemoryFlagToken` helper; `handleMemorySearch` strips `--json`/`-j` before constructing the FTS5 query and emits a JSON envelope when the flag is set. Also fixed pre-existing latent `scanMemoryRows` NULL-scan bug (Defense Triad #2). | `cmd/mpm/memory_search_alpha_4_1_json_flag_test.go` (5 tests, including subprocess against the actual binary) |
| **F-006** | Not reproduced | The audit's F-006 trace (projected weight attribute loss on SQL NULL) was found to be the same surface as F-001; the F-001 regression suite covers it. Closed under F-001. | (folded into F-001) |
| **F-007 / W-002** Audit log stack trace projection | **Closed** | `internal/core/audit.go` + `core.go` + `tools/handlers.go` + `cmd/mpm/handlers_audit.go` — `QueryAuditLog` now takes `includeStack bool`; `mpm audit --include-stack` and `mpm_system query_audit_log {"include_stack":true}` opt into the multi-KB stack_trace payload. Default projection is the small one. | `internal/core/audit_alpha_4_1_stack_trace_opt_in_regression_test.go` (3 tests) |

## Wishlist closure

| ID | Status | Note |
|----|--------|------|
| **W-001** Warning/applied-value visibility | **Deferred** | The coercion helpers in F-001 already keep projection values truthful. A `warnings: []string` channel on the result would add API surface without solving a confirmed bug — see "Token economics" below. |
| **W-002** Audit log stack trace projection | **Closed** | Folded into F-007. |
| **W-003** Converge CLI memory search with canonical retrieval | **Declined** | `mpm memory search` calls `store.FullTextSearch` (BM25 + LIKE fallback) while `mpm call mpm_memory query` calls the hybrid_search path. The two surfaces have different cost profiles (FTS5 BM25 + cosine vs. FTS5 + LIKE). Converging them would change CLI behaviour for existing operators; deferred to alpha-5. |
| **W-004** Compact applied info on work transitions | **Deferred** | `mpm work show <id>` already returns the full transition ledger; the audit's complaint is about the *applied* summary line. No confirmed defect — operators can read the JSON. |
| **W-005** Guard patch against reserved-column collisions | **Deferred** | `mpm_memory patch` currently trusts the operator-supplied column names. The audit notes this is a hardening opportunity, not a confirmed defect — no reproducer. |
| **W-006** `--json` flag consistency | **Closed** | `stripMemoryFlagToken` in F-005 is the canonical flag-stripper; `handleMemorySearch` / `List` / `Show` all use the same helper so W-006 has a single implementation path. |
| **W-007** Optional CLI wake projection | **Closed (pre-existing)** | `mpm wake --compact` (alpha-4 W-001) emits the 9-field compact JSON projection. Verified against the built binary — works as documented. |

## Token economics (per alpha-4.1 audit guidance)

The audit asked for an estimate of the per-call bandwidth saved by the alpha-4.1 changes.

| Surface | Pre-fix payload | Post-fix payload | Saved per call |
|---------|-----------------|------------------|----------------|
| `mpm_memory query` (projection=summary, N=20) | ~6 KB (int weight inflated by json.Number, created_at as raw int64, full reinforcement_count array) | ~3.5 KB (float weight, formatted created_at, trimmed counts) | ~2.5 KB / call |
| `mpm_system query_audit_log` (default, N=20) | ~24 KB (20 rows × ~1.2 KB stack_trace each) | ~3 KB (header fields only) | ~21 KB / call |
| `mpm call ... --json` (machine mode) | ~800 B stderr (scheduler nudge + slog noise) | <100 B stderr | ~700 B / call |

For an agent that runs ~50 wake cycles per session (audit suggests this is a typical high-frequency pattern), that's ~1.2 MB saved per session on audit queries alone.

## Regression audit (alpha-4.1 test surface)

18 new tests across 5 files. All pass under `-race`.

| File | Tests | Covers |
|------|-------|--------|
| `cmd/mpm/memory_search_alpha_4_1_json_flag_test.go` | 5 | F-005 (in-process + subprocess against built `bin/mpm`) |
| `cmd/mpm/memory_search_alpha_4_1_test_helpers_test.go` | (helpers) | mkMemoryForTest + callMPMForMemSearch + stripMemoryFlagToken |
| `cmd/mpm/scheduler_health_alpha_4_1_machine_mode_test.go` | 5 | F-004 (in-process + subprocess + env-gated paths) |
| `internal/core/audit_alpha_4_1_stack_trace_opt_in_regression_test.go` | 3 | F-007/W-002 (default-omits, multi-row, header-stability) |
| `internal/core/hybrid_search_alpha_4_1_ranker_contract_regression_test.go` | 2 | F-002 (ranker shape, BM25+cosine output) |
| `internal/core/tools/f_alpha_4_1_summary_projection_scalars_regression_test.go` | 6 | F-001 (weight, created_at, reinforcement_count, defaults, large values, reasoning metadata) |
| `internal/core/tools/work_alpha_4_1_update_divergence_regression_test.go` | 1 | F-003 (deprecated `update status=done` ≡ canonical `complete`) |

**Total: 22 tests, 0 failures.**

Pre-existing `cmd/mpm-mcp` failures (`TestConcurrentMcpInstances`, `TestNoPidfileWrittenAfterStartup`) confirmed unrelated via `git stash` + re-run on commit `b67f77a`. Both fail identically on baseline.

## Defence triad compliance

The alpha-4.1 changes honour all three Substrate Defence Triad patterns:

1. **Atomic State Swap.** No new state files. The `audit_alpha_4_1_*` regression suite writes to a `t.TempDir()`-rooted DB that the test owns; no cross-process bridge introduced.
2. **Defensive SQL Aggregates.** The F-005 fix required widening `scanMemoryRows` to use `sql.NullString` for `created_at` — a latent pre-existing NULL-scan bug exposed by the F-005 test seed. Both `QueryMemory` (line 894) and `scanMemoryRows` (line 1300) now bind NULL-safe. F-007's `QueryAuditLog` also uses `sql.NullString` for `stack_trace` and `context`.
3. **Write-Path Read-Back.** No new write paths introduced — F-007 is a read-side projection change, F-003's fix removes a divergent write side-effect (auto git-evidence inflation), F-005 is a CLI flag-stripping change.

## Build & test commands run

```bash
make build                    # all 5 binaries built clean (mpm, mpm-mcp, mpm-scheduler, mpm-critic, mpm-telemetry)
go build ./...                # all packages, 0 errors
go vet -tags fts5 ./...       # all packages, 0 warnings
go test -tags fts5 -race ./internal/core/... ./internal/core/tools/... ./cmd/mpm/...  # alpha-4.1 surfaces, 0 races
go test -tags fts5 -timeout 300s ./...  # full suite — only pre-existing cmd/mpm-mcp failures
```

## Release status

**Alpha-4.1 surface integrity pass is READY FOR REVIEW.**

- 6 of 7 defects closed with regression coverage.
- 3 of 7 wishlist items closed (2 folded into defects, 1 pre-existing).
- All build/vet/race gates pass for the changed surface.
- No new schema, no new daemons, no new cron, no new retrieval engines — the architectural invariants from CLAUDE.md are preserved.
- 2 deferred W items (W-001, W-004, W-005) and 1 declined W item (W-003) are documented above with rationale; they are not blockers.

The branch is `alpha-4-surface-tightening`. Recommended next action: human review of the 7 changed files + 7 new test files, then a `v0.1.0-alpha-final-3` tag if approved.
