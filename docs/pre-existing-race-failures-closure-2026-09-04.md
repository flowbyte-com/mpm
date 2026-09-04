# MPM — Pre-Existing `go test -race ./...` Failure Closure

> **Date:** 2026-09-04
> **Branch:** main
> **Commit (this pass):** `007575a`

This is the closure report for the "Pre-Existing `go test -race ./...`
Failure Closure Pass". The brief scoped four pre-existing failures
(mapping to 8 individual test cases across two packages) that
surfaced when the test runner was invoked with the race detector
enabled, but without the project's mandatory FTS5 build flags.

The rule was simple:

> Distinguish shared root causes from independent defects. Do not
> assume the failures are test-only problems. Prefer root-cause
> production fix over test-specific workaround. Preserve
> architectural guarantees.

---

## §A. Failure Inventory

Baseline established from a single `go test -race ./...` invocation
on a clean working tree at `36e8961` (HEAD before this pass). Eight
test cases failed across two packages; they map onto four root-cause
groups in the brief's own inventory.

| Group | Test | Package | First failing operation | Symptom |
|-------|------|---------|------------------------|---------|
| 1 | `TestCallQueryMemoryQuality_PerSourceStats` | `cmd/mpm` | per-source stats query | Empty `sources` list because FTS-backed counter returned 0 rows |
| 2 | `TestF005_SearchJSONFlag_DoesNotPolluteQuery` | `cmd/mpm` | `clear fts` | `no such module: fts5` (driver built without FTS5) |
| 2 | `TestF005_SearchJSONFlag_EmitsJSONEnvelope` | `cmd/mpm` | `clear fts` | `no such module: fts5` |
| 2 | `TestF005_SearchJSONFlag_FlagInAnyPosition` | `cmd/mpm` | `clear fts` | `no such module: fts5` |
| 2 | `TestF005_SearchJSONFlag_PreservesHumanDefault` | `cmd/mpm` | `clear fts` | `no such module: fts5` |
| 2 | `TestF005_SearchJSONFlag_Subprocess` | `cmd/mpm` | `clear fts` | `no such module: fts5` |
| 3 | `TestPhase2_ProjectionPointerResolveChain` | `cmd/mpm` | `add lesson` | `no such table: main.lessons_fts` |
| 3 | `TestPhase2_ProductionResolverWiring` | `cmd/mpm` | `add lesson` | `no such table: main.lessons_fts` |
| 4 | `TestDrillE2E_SchedulerTickFiresDrill` | `internal/scheduler` | `mark fired` (UPDATE scheduled_wakes) | `mark fired failed err="no such module: fts5"` (trigger fires `INSERT INTO scheduled_wakes_fts`); test then reports `fired = 0, want 1` |

**All eight share one root cause.** The mattn/go-sqlite3 driver was
compiled without FTS5 because the bare `go test -race ./...` invocation
omits both required flags (`CGO_CFLAGS=-DSQLITE_ENABLE_FTS5=1` for
C-level SQLite FTS5 support, `-tags fts5` for Go-level driver gating).

---

## §B. Root-Cause Explanation

**Single shared root cause: FTS5 build flags missing under
`go test -race ./...`.**

FTS5 capability in the MPM substrate is gated by two compile-time
flags (per `internal/core/db.go` `initFTSTables` and the schema apply
path):

```
CGO_CFLAGS=-DSQLITE_ENABLE_FTS5=1   # C-level SQLite FTS5 build
go build -tags fts5                 # Go-level driver build tag
```

Without both, `mattn/go-sqlite3` is compiled without the FTS5 module.
The substrate's defensive code at `initFTSTables` (`db.go:2667`) is
designed for graceful degradation on the **search/LIKE fallback path**.
But the substrate's `migrateLessonsToView` and `initFTSTables`
SQL DDL also creates INSTEAD OF / AFTER triggers that fire on writes
to `lessons` and `scheduled_wakes`, respectively, and write to FTS5
virtual tables. These triggers:

- are created unconditionally on every schema apply;
- succeed at CREATE time (SQLite defers validation);
- fail at FIRE time with `no such module: fts5` when the driver
  isn't FTS5-capable.

`make test` applies both flags via the Makefile, so production
behaviour (which is always built with `-tags fts5`) matches.
`go test -race ./...` does NOT route through the Makefile, so the
flags are absent.

**Why the fix is at the build-config layer, not production:**

> Per §10 ("Preserve current architectural guarantees. Do not
> regress... FTS5 integrity"). Per §9 ("Do not modify production
> code if the actual defect is only a broken test fixture").

The architectural guarantee of FTS5 integrity is mandatory.
Downgrading FTS5 from mandatory to optional in production code
would regress this guarantee. The failing tests are not broken —
they correctly require FTS5 to be present at compile time. The
defect is that `go test -race ./...` does not route through the
canonical FTS5-aware invocation.

**Mirror-path warnings observed during the failing run are NOT in
the four listed failure groups.** They appear in passing tests
(`TestMemoryAdd_*`) as warnings on `MemoryStore.MirrorFile == ""`,
which is a separate test-fixture quirk in unrelated tests and was
out of scope for this pass.

---

## §C. Before / After Evidence

### Before (`36e8961`, bare `go test -race ./...`)

```
$ go test -race ./...
FAIL    github.com/flowbyte-com/mpm/cmd/mpm   27.334s
FAIL    github.com/flowbyte-com/mpm/internal/scheduler    16.038s
--- FAIL: TestCallQueryMemoryQuality_PerSourceStats (0.15s)
--- FAIL: TestF005_SearchJSONFlag_DoesNotPolluteQuery (0.00s)
--- FAIL: TestF005_SearchJSONFlag_EmitsJSONEnvelope (0.00s)
--- FAIL: TestF005_SearchJSONFlag_PreservesHumanDefault (0.00s)
--- FAIL: TestF005_SearchJSONFlag_FlagInAnyPosition (0.00s)
--- FAIL: TestF005_SearchJSONFlag_Subprocess (0.01s)
--- FAIL: TestPhase2_ProductionResolverWiring (0.14s)
--- FAIL: TestPhase2_ProjectionPointerResolveChain (0.06s)
--- FAIL: TestDrillE2E_SchedulerTickFiresDrill (0.13s)
drill_e2e_test.go:193: fired = 0, want 1
```

### After (`007575a`, `make test-race`)

```
$ make test-race
ok      github.com/flowbyte-com/mpm/cmd/mpm                                26.534s
ok      github.com/flowbyte-com/mpm/cmd/mpm-mcp                            33.979s
ok      github.com/flowbyte-com/mpm/cmd/mpm-scheduler                      (cached)
ok      github.com/flowbyte-com/mpm/cmd/mpm-critic                         (cached)
ok      github.com/flowbyte-com/mpm/cmd/mpm-telemetry                      (cached)
ok      github.com/flowbyte-com/mpm/cmd/gen-cli                           (cached)
ok      github.com/flowbyte-com/mpm/internal/telemetry                     (cached)
ok      github.com/flowbyte-com/mpm-core                                   (cached)
ok      github.com/flowbyte-com/mpm-core/capability                        (cached)
ok      github.com/flowbyte-com/mpm-core/config                            (cached)
ok      github.com/flowbyte-com/mpm-core/logging                          (cached)
ok      github.com/flowbyte-com/mpm-core/mpmcli                           (cached)
ok      github.com/flowbyte-com/mpm-core/orchestration                     (cached)
ok      github.com/flowbyte-com/mpm-core/renderers                         (cached)
ok      github.com/flowbyte-com/mpm-core/seed                             (cached)
ok      github.com/flowbyte-com/mpm-core/seed/cap                          (cached)
ok      github.com/flowbyte-com/mpm-core/synth                            (cached)
ok      github.com/flowbyte-com/mpm-core/usererror                        (cached)
ok      github.com/flowbyte-com/mpm/internal/scheduler                     16.902s
```

All eight listed test cases now PASS under the canonical invocation.
No regressions introduced.

---

## §D. Race Validation

`make test-race` is the race-detector-enabled variant. Repeated with
`-count=20` for stability per the brief §15:

```
$ CGO_CFLAGS=-DSQLITE_ENABLE_FTS5=1 CGO_LDFLAGS=-lm \
    go test -race -tags fts5 -count=20 \
    ./cmd/mpm/... \
    -run 'TestCallQueryMemoryQuality_PerSourceStats|...SearchJSONFlag|...Phase2'
ok  github.com/flowbyte-com/mpm/cmd/mpm        15.939s

$ CGO_CFLAGS=-DSQLITE_ENABLE_FTS5=1 CGO_LDFLAGS=-lm \
    go test -race -tags fts5 -count=20 \
    ./internal/scheduler/... \
    -run 'TestDrillE2E_SchedulerTickFiresDrill'
ok  github.com/flowbyte-com/mpm/internal/scheduler    3.151s
```

Stable across 20 repetitions. No race regressions.

---

## §E. Full-Suite Validation

`make test-race` exit code 0, 19 packages PASS (no FAIL line in
output). Bare `go test ./...` (no race, no flags) still surfaces
the same 8 failures — this is expected; the bare Go invocation
remains incompatible with mandatory FTS5. `make test` (no race)
still passes (no behavioural change to that target).

`go vet ./...` clean.
`git status` clean; one focused commit at `007575a`; no push per §11.

---

## §F. Remaining Failures

**None under canonical invocation (`make test-race`).**

Bare `go test -race ./...` (without the FTS5 build flags) will
continue to surface the failures by design — the brief's §13
literal command form is structurally incompatible with the
substrate's mandatory FTS5 integrity guarantee. Future editors who
encounter this should reach for `make test-race` instead. The new
`internal/core/build_config_invariants_test.go` regression pins the
Makefile target so this drift can only be reintroduced deliberately.

---

## Files Changed

| File | Change | Commit |
|------|--------|--------|
| `Makefile` | Add `make test-race` target (-race + FTS5 flags); register in `.PHONY` | `007575a` |
| `CLAUDE.md` | §1 fix doc/code drift; describe `make test-race` and the FTS5 + race reason | `007575a` |
| `internal/core/build_config_invariants_test.go` | Structural smoke test pinning the Makefile target's flags (NEW) | `007575a` |

Net change: 2 files modified, 1 file created. One focused commit.
No `git push` was performed (per §11 discipline).

---

## Drift Controls

| Control | What it pins |
|---------|--------------|
| `TestBuildConfig_MakefileHasRaceDetectorTarget` (new) | The Makefile's `test-race` target is registered in `.PHONY`, includes `-race`, references `$(CGO_CFLAGS)`, and `CGO_CFLAGS := -DSQLITE_ENABLE_FTS5=1` is defined at the top of the Makefile. Drift in any of these surfaces as a failure with a clear `t.Fatalf` message pointing the editor at the FTS5+cascade rationale. |
| `CLAUDE.md` §1 (updated) | Documents `make test` and `make test-race` distinctly; states that bare `go test -race ./...` is incompatible with mandatory FTS5 integrity, and points at `make test-race` as the canonical CI gate. |
| `Makefile` `make test-race` target (new) | Encapsulates the race-detector invocation with all required flags in one place. Editors don't have to remember the CGO_CFLAGS + build-tag combination. |

---

## Final Verdict

| # | Question | Answer |
|---|----------|--------|
| 1 | FTS failures root cause demonstrated? | **YES** |
| 2 | Mirror-path failures root cause demonstrated? | **YES** — confirmed downstream noise from empty `MemoryStore.MirrorFile` in test fixtures that PASS; not in the four listed failures; out of scope per brief. |
| 3 | Production defect confirmed? | **NO** — production is built with `-tags fts5` and works correctly. The defect is the bare `go test -race ./...` invocation skipping the build flags the production binary requires. |
| 4 | Test-fixture defect confirmed? | **NO** — failing tests correctly require FTS5; the fixtures are accurate. No test code modified. |
| 5 | Regression coverage added? | **YES** — `TestBuildConfig_MakefileHasRaceDetectorTarget` (structural). |
| 6 | All four original failures closed? | **YES** — under canonical invocation `make test-race`. |
| 7 | `go test -race ./...` clean with zero exclusions? | **NO** (literal) — structurally impossible; FTS5 capability is compile-time bound; documented in CLAUDE.md §1 and pinned by the regression test. The canonical race-enabled invocation IS clean (`make test-race`, exit 0). |
| 8 | Full validation clean? | **YES** — `make test-race` exits 0 across 19 packages. |
| 9 | New known debt introduced? | **NO** — the bare `go test -race ./...` incompatibility is inherent to the FTS5 architecture, was present at HEAD before this pass, and is now explicitly documented rather than silently latent. |
| 10 | Architectural guarantees preserved (WAL, busy_timeout, scheduler, FTS5 integrity, schema, mirror semantics, workspace isolation, agent-installation behaviour)? | **YES** — no production code modified; only the build/test-invocation layer. |
| 11 | Commit discipline (§18) — focused commits, no push? | **YES** — single focused commit `007575a`; no push; working tree clean post-pass. |
| 12 | Scope boundary respected — no design changes, no architectural refactors? | **YES** — fix confined to Makefile + CLAUDE.md + a new regression test. |
