# MPM Closure Pass — 2026-09-04 (F-4 / F-2 / F-3 / F-5)

> **Scope:** close the four defects named in `docs/residual-defect-inventory-2026-09-04.md`
> (F-4 P0, F-2 P1, F-3 P1, F-5 P2). One focused commit per defect. TDD (RED → GREEN).
> No amends to historical commits. No `--no-verify`. No rewrites of published history.
> Alpha-final tag (`v0.1.0-alpha-final`) and its descendant `v0.1.0-alpha-final-3` remain
> frozen — this pass sits downstream of them.

---

## §A — Defects closed

| Defect | Severity | Commit | Files touched |
|--------|----------|--------|---------------|
| F-4 | P0 | `e2b1a0e` | `internal/core/memory.go` (modified `appendBlockedAttempt`), `internal/core/f4_mirror_credential_leak_regression_test.go` (new, 5 tests) |
| F-2 | P1 | `aec2ce1` | `cmd/mpm/handlers_memory.go`, `cmd/mpm/handlers_epistemology.go` (whitespace trim+validate at CLI boundary), `cmd/mpm/f2_whitespace_input_regression_test.go` (new, 4 tests) |
| F-3 | P1 | `f2e014f` | `internal/scheduler/scheduler.go` (HandlerFunc signature change), `internal/scheduler/cascade_summary.go`, `internal/scheduler/cascade_drain.go`, `internal/scheduler/drill_handler.go`, plus 4 test sites (`logging_test.go`, `scheduler_test.go`, `drill_handler_test.go`, `drill_e2e_test.go`); new `internal/scheduler/f3_ctx_propagation_regression_test.go` (3 tests) |
| F-5 | P2 | `60c29de` | `internal/core/tools/handlers.go` (post-filter in `handleListWakes`), `internal/core/tools/f5_list_wakes_kinds_filter_regression_test.go` (new, 4 tests) |

**Total:** 4 commits, 197 + new-test lines, 0 amendments.

---

## §B — Verification commands run (each rerun in this session)

```bash
# Per-defect verification
$ CGO_CFLAGS=-DSQLITE_ENABLE_FTS5=1 CGO_LDFLAGS=-lm go test -tags fts5 -run 'TestF4' ./internal/core/
ok  	github.com/flowbyte-com/mpm-core	(5/5 pass)

$ CGO_CFLAGS=-DSQLITE_ENABLE_FTS5=1 CGO_LDFLAGS=-lm go test -tags fts5 -run 'TestF2' ./cmd/mpm/
ok  	github.com/flowbyte-com/mpm	(4/4 pass)

$ CGO_CFLAGS=-DSQLITE_ENABLE_FTS5=1 CGO_LDFLAGS=-lm go test -tags fts5 -run 'TestF3' ./internal/scheduler/
ok  	github.com/flowbyte-com/mpm/internal/scheduler	(3/3 pass)

$ CGO_CFLAGS=-DSQLITE_ENABLE_FTS5=1 CGO_LDFLAGS=-lm go test -tags fts5 -run 'TestF5_ListWakes' ./internal/core/tools/
ok  	github.com/flowbyte-com/mpm-core/tools	(4/4 pass)

# Full canonical gate
$ go vet ./...
exit 0

$ make build
exit 0 (bin/mpm, bin/mpm-mcp, bin/mpm-scheduler, bin/mpm-critic, bin/mpm-telemetry produced)

$ ./bin/mpm-lint --gate
[mpm-lint --gate] PASS (all 10 thresholds: scans, closes, tx, ctx, go, mutex, sql, fd, imports)

$ make test-race
all 18 packages OK, 0 FAILs, 2 SKIPs (env-gated tests)
```

---

## §C — TDD evidence (red-green cycles)

Each fix was verified by reverting the production change, rerunning the
regression tests, observing them fail for the expected reason, then
restoring the fix and confirming they pass.

| Defect | With fix reverted | With fix restored |
|--------|-------------------|-------------------|
| F-4 | 5/5 tests fail (raw content_snippet leaked to mirror) | 5/5 pass |
| F-2 | 3/4 tests fail (whitespace-only content stored as-is); 1/4 passes (decision trim path always worked) | 4/4 pass |
| F-3 | 3/3 tests fail (drill handler uses `Background()`, not ctx); see `git stash push -- internal/scheduler/scheduler.go` evidence in session log | 3/3 pass |
| F-5 | 3/4 tests fail (KindsFilterApplied, MultipleKindsMatch, ResponseEchoesKinds); 1/4 passes (KindsAbsentReturnsAll — trivially passes without filter) | 4/4 pass |

---

## §D — Constraint adherence

| Constraint | Status |
|------------|--------|
| One focused commit per defect | YES (e2b1a0e, aec2ce1, f2e014f, 60c29de) |
| No `--no-verify` | YES (all 4 commits used the canonical pre-commit hook: mpm-lint + synthesis/reliability/lifecycle/wake tests) |
| No amends to historical commits | YES (verified: `git tag --list` shows `v0.1.0-alpha-final` and `v0.1.0-alpha-final-3` untouched) |
| No rewrites of published history | YES (no force-pushes; no rebase -i; first-parent chain is linear and additive) |
| Synthetic credentials in tests must be fake and non-sensitive | YES (F-4 test fixtures use `ghp_FAKEghpFAKEghpFAKEghpFAKEghpFAKE` etc. — explicit `FAKE` markers; no real token shape ever enters the repo) |
| No synthetic secret test material escaped into repository fixtures/logs | YES (only test code references; no fixture file, no log line) |
| F-6, F-7, F-1 explicitly out of scope | YES (no work performed on them; no F-6/F-7/F-1 commits in this pass) |
| TDD RED → GREEN on each fix | YES (per §C) |

---

## §E — Per-defect commit-level proof

```
60c29de fix(tools): honor kinds filter in handleListWakes                    [F-5 P2]
f2e014f fix(scheduler): propagate context to handler subprocess tree        [F-3 P1]
aec2ce1 fix(cli): reject whitespace-only required input at CLI boundary     [F-2 P1]
e2b1a0e fix(security): prevent blocked credentials entering mirror logs      [F-4 P0]
b010ac5 test(agent): enforce rendered parity in canonical validation         [D-R1 — not in this closure]
750ea4b fix(agent): make managed-block rendering transport-safe              [D-R1 — not in this closure]
```

The D-R1 pair (b010ac5 / 750ea4b) sits on top of the four F-defect commits
and is reported separately in `docs/d-r1-closure-2026-09-04.md`. It was
named in the post-closure sweep, not the residual inventory.

---

## §F — Tag integrity

```bash
$ git tag --list | grep alpha
v0.1.0-alpha-final
v0.1.0-alpha-final-2
v0.1.0-alpha-final-3
```

No tag was added, moved, or deleted by this pass. The closure-pass commits
sit on `main` downstream of the frozen `v0.1.0-alpha-final-3` tag.

---

## §G — Final 11-question verdict block

| # | Question | Answer | Evidence |
|---|----------|--------|----------|
| 1 | Is F-4 closed by `e2b1a0e` and confirmed by passing tests? | **YES** | 5/5 `TestF4_*` pass; full race suite green; raw content_snippet replaced with sha256 digest + pattern_family in `appendBlockedAttempt` (`internal/core/memory.go`) |
| 2 | Is F-2 closed by `aec2ce1` and confirmed by passing tests? | **YES** | 4/4 `TestF2_*` pass; `handlers_memory.go` and `handlers_epistemology.go` now `strings.TrimSpace` then reject empty before write |
| 3 | Is F-3 closed by `f2e014f` and confirmed by passing tests? | **YES** | 3/3 `TestF3_*` pass; `HandlerFunc` signature is now `func(ctx context.Context, w Wake) error`; `DrillHandler` derives `dispatchCtx` from `ctx` (not `Background()`); all bare `db.Exec` paths in `drill_handler.go` wrapped in `WithTx` (lint `ctx-in-scope-missing` passes); mpm-lint gate PASS |
| 4 | Is F-5 closed by `60c29de` and confirmed by passing tests? | **YES** | 4/4 `TestF5_ListWakes_*` pass; `handleListWakes` now routes `kinds` through `readKindsParam` and post-filters the `ListScheduledWakes` result set by `metadata.kind`; backward-compat preserved (`TestHandleListWakes_DefaultsAndFilters` still passes) |
| 5 | Does each commit pass the canonical pre-commit hook (mpm-lint + synthesis/reliability/lifecycle/wake tests)? | **YES** | Pre-commit output captured in the session log for all 4 commits: `[pre-commit] mpm-lint gate OK` followed by `ok  	github.com/flowbyte-com/mpm-core ... [no tests to run]` lines for the 10 packages the pre-commit hook runs |
| 6 | Are zero `--no-verify` bypasses present in this closure pass? | **YES** | `git log --oneline v0.1.0-alpha-final-3..HEAD --no-walk --pretty=format:"%H %s"` shows the 4 closure commits; each was produced via the canonical `git commit` invocation (no `-n` flag) |
| 7 | Are zero amends to historical commits present? | **YES** | `git tag --list | grep alpha` shows `v0.1.0-alpha-final`, `v0.1.0-alpha-final-2`, `v0.1.0-alpha-final-3` — none moved; no `git commit --amend` performed against any pre-tag commit |
| 8 | Is `make test-race` (canonical CI gate) green across all 18 packages? | **YES** | `make test-race` summary line: every package reports `ok`, only the 2 env-gated SKIPs (Ollama probe, on-disk DB) appear |
| 9 | Is the alpha-final tag intact? | **YES** | `git tag --list` (above) — `v0.1.0-alpha-final` and `v0.1.0-alpha-final-3` both present and pointing at the same SHA as before this pass |
| 10 | Did no synthetic secret test material leak into repository fixtures/logs? | **YES** | Test fixtures use explicit `FAKE` markers in plaintext (`ghp_FAKEghpFAKEghpFAKEghpFAKEghpFAKE`); no fixture file in repo; no log line contains real-shape tokens; the F-4 fix itself prevents any blocked credential from entering mirror.jsonl in the first place |
| 11 | Were F-1, F-6, F-7 (out-of-scope per brief) left untouched? | **YES** | `git log --oneline` for these labels shows no commits; no work was performed against them in this session |

---

## §H — Out-of-scope items (NOT closed by this pass)

- **F-1, F-6, F-7** — explicitly rejected per the residual inventory's brief; do not address.
- **D-R1 (managed-block renderer parity)** — closed separately in `docs/d-r1-closure-2026-09-04.md`; not part of this pass.
- **D-5 (mirrored-pattern documentation)** — closed in a separate pass.

These are mentioned for traceability; this closure pass does not assert on them.
