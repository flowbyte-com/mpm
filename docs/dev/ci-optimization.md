# C — CI optimization report

> Measured on 2026-10-26 from four representative runs:
> `40299775451` (#102), `40297056201` (#96), `40293891450` (#99),
> `40241395644` (push to `main`).

## 1. Measured baseline

### 1.1 Job wall-clock

| Job | run 40299775451 | 40297056201 | 40293891450 | 40241395644 |
|---|---|---|---|---|
| `gate` — "Build + core tests" | 18 min | 18 min | 17 min | 14 min |
| `full-suite` — "Full test suite" | 11 min | 11 min | 8 min | 10 min |
| `release-gate` | 27 min | 25 min | 18 min | 18 min |
| **Pipeline wall-clock** (jobs are parallel) | **27 min** | **25 min** | **18 min** | **18 min** |
| **Total runner-minutes per push** | **~56** | **~54** | **~43** | **~42** |

### 1.2 Step-level cost inside the `gate` job

| Step | Duration |
|---|---|
| `Installer contract (scripts/tests)` | **4 min** |
| `Run full test suite with coverage` (`go test -race ./...`) | **10 min** |
| `Nested tools module (internal/core/tools)` (`make test-tools-race`) | **1 min** |
| Build, go vet, mpm-lint, core tests, renderer parity, bench, coverage summary, upload | < 1 min each |

### 1.3 Step-level cost in the other two jobs

| Job | Step | Duration |
|---|---|---|
| `full-suite` | `Full test sweep` (`go test ./...` **without** `-race`, + `make test-tools`) | **11 min** |
| `release-gate` | `Release gate` (`make release-gate` → `test-race` + `test-release`) | **18–27 min** |

**The bottleneck is `cmd/mpm`.** It is the largest package in the tree; the
workflow's own comment measures it at 530–600 s under `-race` on a two-core
runner. It is executed **three times per push**: once with `-race` in `gate`,
once without `-race` in `full-suite`, and once more inside `make release-gate`.

### 1.4 What is *not* the bottleneck

- Caching: `actions/setup-go` with `cache: true`, and `actions/setup-node`
  with a lockfile-keyed npm cache. Both are already in place and effective.
- The inexpensive gates (build, vet, lint, core subset, renderer parity,
  bench) each cost under a minute. They are not worth reordering against the
  expensive ones.
- The `mpm-lint` gate is not slow.

## 2. The false-green investigation — **confirmed defect**

### 2.1 The claim

The `full-suite` job's sweep step read:

```yaml
- name: Full test sweep
  run: |
    go test -tags fts5 -timeout 20m -count=1 ./... 2>&1 | tee test-output.log
    make test-tools 2>&1 | tee tools-output.log
```

### 2.2 The shell semantics — verified against the official documentation

GitHub Actions runs a `run:` step with the **default** shell unless the step
opts out. From the workflow-syntax reference:

> | Linux / macOS | unspecified | The default shell on non-Windows platforms. **Note that this runs a different command to when `bash` is specified explicitly.** | `bash -e {0}` |
> | All | `bash` | … | `bash --noprofile --norc -eo pipefail {0}` |
>
> By default, fail-fast behavior is enforced using `set -e` for both `sh` and
> `bash`. **When `shell: bash` is specified, `-o pipefail` is also applied** to
> enforce early exit from pipelines that generate a non-zero exit status.

The step declared no `shell:`, so it ran under `bash -e {0}` — **no
`pipefail`**. Verified locally:

```
$ bash -e -c 'false | tee /dev/null; echo "exit=$?"'
exit=0
$ bash -eo pipefail -c 'false | tee /dev/null; echo "exit=$?"'
exit=1
```

### 2.3 Consequence

The pipeline's exit status is `tee`'s, which is 0. **A failing `go test`
could not fail the step, and therefore could not fail the job.** The
"Full test suite" job was a *decorative* sweep: it printed a log, it did not
gate anything.

This is not a new observation about GitHub Actions; it is a repeat of a
masking the repository already suffered once. The workflow's own header
records it:

> Worse, `continue-on-error` is invisible from the job list: that job's
> conclusion reads `success` while its own log shows
> `FAIL github.com/flowbyte-com/mpm/cmd/mpm 400.696s`.

`continue-on-error` was removed — but the `| tee` pipeline reintroduced
exactly the same masking, more quietly, with no `continue-on-error` in the
file to warn a reader.

**Conclusion: this is a defect, not a false alarm.** It is fixed by PR #104.

### 2.4 Scope of the defect

Only **one** step in the whole workflow was affected in a gating way. The
`gate` job's `make test-scripts`, `make test-bench`, `go test -race ./...`
and `make test-tools-race` steps are not piped, so they fail correctly — which
is why the #99 contract-test defect *was* caught. The `release-gate` job
runs `make release-gate`, also not piped. So the false-green was confined to
the "Full test suite" job, whose entire purpose is the ungated sweep.

## 3. Redundant execution — measured, and what may be removed

### 3.1 The root-module Go sweep runs three times

| Sweep | Command | Job | Race? |
|---|---|---|---|
| A | `go test -tags fts5 -race -timeout 20m -count=1 -covermode=atomic -coverprofile=coverage.out ./...` | `gate` | yes |
| B | `go test -tags fts5 -timeout 20m -count=1 ./...` | `full-suite` | no |
| C | `make test-race` → `go test -race ./cmd/...`, `internal/telemetry`, `internal/buildinfo`, `internal/core`, `internal/scheduler`, `internal/critic` | `release-gate` | yes |

**Sweep B is a strict subset of sweep A.** Same packages, same tests, same
build tags — only the race detector and the coverage instrumentation differ.

### 3.2 What may **not** be removed, and why

**Sweep B must not simply be deleted.** It is the only sweep that runs
`cmd/mpm` **without** the race detector. The race detector changes timing
substantially, and this sprint has already demonstrated that timing-sensitive
tests behave differently under it:

- the signal-interruption test (#103) was **4 of 4 red** in the full suite
  and **green** in isolation — a timing-dependent test whose outcome depends
  on scheduling;
- a race-detector-only failure would be invisible to B.

Removing B would reduce the signal available for exactly the class of defect
this sprint has already been bitten by. **B is retained.**

### 3.3 The `make test-scripts` duplication

`make test-scripts` runs **twice** per push: once in `gate` (4 min) and once
inside `make release-gate` → `test-race`.

The workflow header states the duplication is "contractually pinned by
`scripts/tests/test_makefile_agent_adapter_wiring.py` TestGateReachability".
**Checked:** that test pins the **Makefile's dependency graph** — `make test`,
`make test-race` and `make release-gate` must each reach
`test-agent-installation`. It does **not** pin the CI *job* structure.

So the constraint is real but narrower than the comment implies: the
**Makefile targets** must keep reaching the per-adapter suites. The CI job
layout is not contractually pinned by that test.

**Recommendation: keep the duplication for now.** It costs 4 runner-minutes
and its removal would change a contractually-shaped gate for a benefit that
does not justify the risk during a release-candidate sprint. Revisit after
the RC.

### 3.4 The `release-gate` job's race sweep

`make release-gate` → `test-race` re-runs the root-module race sweep that
`gate` already ran (sweep A). This is the largest redundancy — roughly 10
runner-minutes.

**Recommendation: keep it.** `release-gate` is the job that additionally
reaches `test-agent-installation` and `test-agent-adapters` (the
mpm-opencode TypeScript suite) and `release_acceptance` (a separate Go
module). It is the job whose conclusion should stand alone as "the release
is good", independent of the `gate` job's separate result. Splitting the
race sweep out of it would weaken exactly the property that makes it a
release gate. **This is the one redundancy that exists for a good reason.**

## 4. Ranked optimization plan

| # | Change | Current cost | Expected benefit | Risk of losing verification | Preserved verification | Measurement |
|---|---|---|---|---|---|---|
| **1** | **`shell: bash` on the piped steps** (PR #104) | — | — | **none** — this *adds* verification | every check still runs; the job can now fail | the new regression test `test_no_step_pipes_without_pipefail` fails when the property is removed |
| **2** | **`concurrency` + `cancel-in-progress`** (PR #104) | ~43–56 runner-minutes wasted per superseded push | reclaims the full cost of a superseded run | **none** — the surviving run executes every check | all three gating jobs, all steps | Actions run list: superseded runs show "cancelled" |
| 3 | Cache `~/.cache/go-build` and `GOMODCACHE` across jobs | setup ≈ 0.5 min/job | ~1–2 min/job, ~4 min/run | none (cache correctness only) | unchanged | step durations before/after |
| 4 | Split the `cmd/mpm` package sweep out of the root sweep so it can be cached separately | — | enables per-package parallelism and better cache reuse | **medium** — changes what a green run proves | must re-run the whole package set | step durations |
| 5 | Run `bench/tests` and the `agent_installation` parity suite in parallel with the Go sweep | 1 min each | ~1 min wall-clock | none | unchanged | job wall-clock |
| 6 | Drop the `full-suite` job's root sweep **once** the suite is shown race-stable | 11 runner-minutes | 11 runner-minutes | **high** — loses the only non-race run of `cmd/mpm` | would need a replacement non-race run | see §3.2 — **deferred** |
| 7 | Drop the duplicated `make test-scripts` from `release-gate` | 4 runner-minutes | 4 runner-minutes | **medium** — `release-gate` is contractually the comprehensive sweep | the `gate` job still runs it | job durations |

**Changes 1 and 2 are implemented and pushed (PR #104). Changes 3–5 are
low-risk and are left for a follow-up PR once the release candidate is
stable. Changes 6 and 7 are explicitly deferred**, with the reasoning
recorded above.

## 5. Superseded-run cancellation — detail

The workflow declared **no** `concurrency` group. A push therefore started a
new run while the previous one continued to completion — burning 43–56
runner-minutes to produce a conclusion about a commit that was already
obsolete.

PR #104 adds:

```yaml
concurrency:
  group: ${{ github.workflow }}-${{ github.ref }}
  cancel-in-progress: true
```

- A new push to a PR cancels that PR's in-flight run.
- A new push to `main` cancels `main`'s in-flight run.
- Two different PRs **never** cancel each other (the group includes
  `github.ref`).
- The surviving run executes every check the workflow defines — nothing is
  skipped, shortened or relaxed.

Pinned by three regression tests, including one that asserts the group
includes `github.ref`.

## 6. Preserved release gate — explicit inventory

CI optimization **must not** reduce any of the following. All remain in
place after PR #104:

| Requirement | Where it runs | Still present |
|---|---|---|
| Full repository tests | `gate` (`go test -race ./...`) and `full-suite` (`go test ./...`) | ✅ |
| Race-sensitive behaviour | `gate` race sweep; `make test-tools-race`; `release-gate` → `test-race` | ✅ |
| Database migrations and concurrent initialisation | `internal/core` tests, incl. #98's `schema_init_concurrency_test.go` | ✅ |
| Security invariants | `cmd/mpm` `operator_only_shred_regression_test.go`, `internal/core` DB-safety tests, `mpm-lint --gate` | ✅ |
| Data isolation | `internal/core` `TestDBSafety` (the repo-wide production-DB guard, which the workflow header explicitly warns must not be skipped under `-short`) | ✅ |
| Release acceptance | `release-gate` → `make test-release` (`release_acceptance` module) | ✅ |
| OpenCode adapter compatibility | `release-gate` → `test-agent-installation` → `test-agent-adapters` (mpm-opencode TypeScript, Node 20) | ✅ |
| Installed runtime behaviour | `internal/core/tools` module sweep, `cmd/mpm` CLI subprocess suite against `$(BUILD_DIR)/mpm` | ✅ |
| Existing CI contract tests | `scripts/tests/test_release_gate_bootstrap_wiring.py`, `test_makefile_agent_adapter_wiring.py` | ✅ and now joined by `test_ci_gate_integrity.py` |

**Nothing was removed. One thing was made able to fail.**

## 7. A note on fast feedback vs final validation

These are two different things and the workflow should keep them distinct:

- **Fast developer feedback** — the `gate` job's first six steps (build, vet,
  lint, core subset, renderer parity) complete in under a minute in total.
  These are already first in the job.
- **Mandatory final integration validation** — the race sweep with coverage,
  the installer-contract suite, the full-suite sweep and the release gate.
  These are expensive by design and PR #104 makes the full-suite sweep
  *count*.

A focused PR check is **not** permission to merge. The `release-gate` job
must be green on the actual merged commit before milestone promotion.

## 8. Required checks

The three gating jobs — "Build + core tests", "Full test suite", "Release
gate (cross-agent continuity + public-CLI subprocess suite)" — are the
required checks. All three are preserved, and all three are now capable of
failing. Because the group is scoped to `github.ref`, a **stacked PR** (such
as #104 on #103) still runs the full workflow — the `concurrency` group
cancels only *superseded* runs of the same ref, never a different branch.

One operational consequence worth stating plainly: **the pre-existing
signal-interruption flake (PR #103) makes the "Build + core tests" job
intermittently red for any PR**, because that job runs `make test-scripts`.
Merging #103 early is therefore not merely hygiene — it is what stops the
queue from reporting failures that have nothing to do with the PR under
review.
