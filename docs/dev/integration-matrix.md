# B — Corrective PR integration: post-merge verification

> **Refreshed 2026-10-26 against live GitHub state.** The twelve corrective
> PRs and the three foundation PRs are **all merged**. This document is no
> longer a merge plan; it is the **verification record** of what was merged,
> in what order, and what the merged result was checked against.
>
> Verified against `origin/main` = **`b5b18401974ee559a485cda20abf8f7779f11bf2`**.

---

## 1. Verified state of every PR

All fifteen PRs are `MERGED`. Every merge was a **squash merge** (single
parent), so `main`'s history is one commit per PR — which is why the merge
order below is exact and unambiguous.

| PR | Branch | Merge commit on `main` | Subject | Merge order |
|---|---|---|---|---|
| #102 | `fix/hal9001-mpmcli-env-isolation` | `e093bcc` | isolate the `mpmcli` package's environment-sensitive tests | 1 |
| #89 | `fix/hal9001-provenance-test-isolation` | `bf142f3` | provenance-attribution test isolation | 2 |
| #90 | `fix/hal9001-gc-reporting-honesty` | `8c640d1` | gc: report maintenance failures instead of claiming a clean sweep | 3 |
| #91 | `fix/hal9001-ingest-undo-honesty` | `9e62fd2` | ingest: make undo say what it actually reverts | 4 |
| #87 | `fix/hal9001-embedding-migration-diagnostics` | `b50f27c` | doctor: report the partial embedding-migration split | 5 |
| #96 | `fix/hal9001-r2-save-evidence` | `ea775ad` | memory: refuse evidence on save instead of silently discarding it | 6 |
| #85 | `fix/hal9001-shred-agent-auth` | `9a5661c` | shred: make permanent memory and lesson deletion operator-only | 7 |
| #88 | `fix/hal9001-raw-memory-shred-linkage` | `3c18840` | shred: link imported staging rows to the promoted memory | 8 |
| #86 | `fix/hal9001-confidence-provenance` | `765334b` | confidence: exclude provenance-only evidence from scoring | 9 |
| #97 | `fix/hal9001-r3-directive-retention` | `d89c11c` | retention: exempt standing directives from access-based pruning | 10 |
| #98 | `fix/hal9001-r3-schema-init-concurrency` | `321a695` | serialise schema init so concurrent opens cannot fail | 11 |
| #99 | `fix/hal9001-r3-install-next-steps` | `f7e0185` | advertise a wake-context command that exists | 12 |
| #95 | `fix/hal9001-r2-confidence-decay` | `f2c1ff4` | bounded automatic confidence decay pass | 13 |
| #103 | `fix/hal9001-signal-test-determinism` | `cefe14b` | make the signal-interruption test deterministic | 14 |
| #104 | `fix/hal9001-ci-gate-integrity` | `b5b1840` | make the full-suite sweep a real gate; cancel superseded runs | 15 |

`git log --oneline --reverse 414b276..b5b1840` reproduces this order
exactly.

### 1.1 Deviations from the pre-merge plan, and whether they mattered

The plan was: #89 → #90 → #91 → #87 → #96 → #85 → #88 → #86 → **#95** →
#97 → #98 → #99, with #103 → #104 → #102 first.

The actual order matches on every dependency-bearing pair:

| Dependency | Planned | Actual | Held? |
|---|---|---|---|
| #89 before #90/#91 | yes | #89 (2) before #90 (3) and #91 (4) | ✅ |
| #96 before #85 | yes | #96 (6) before #85 (7) | ✅ |
| #85 before #88 | yes | #85 (7) before #88 (8) | ✅ |
| #86 before #95 | yes | #86 (9) before #95 (13) | ✅ |
| #88 before #95 (the conflict) | yes | #88 (8) before #95 (13) | ✅ |
| #103 before #104 | yes | #103 (14) before #104 (15) | ✅ |

**One deviation:** #95 was merged *after* #97, #98 and #99 rather than
immediately after #86. **It does not matter**, and the reason is checkable:
#97, #98 and #99 touch `internal/core/directive_retention_test.go` +
`docs/SPEC.md`, `internal/core/db.go` + `internal/core/maintenance_lock.go` +
`internal/core/schema_init_concurrency_test.go`, and `install.sh` +
`scripts/tests/test_install_sh_contracts.py` respectively. **None of them
touches `internal/core/schema.go`**, so the only `schema.go` interaction in
the whole set remained the #88 ↔ #95 pair, and that pair kept its order.

---

## 2. The one conflict — re-reviewed against the merged result

The pre-merge matrix recorded exactly one conflict in the whole set:
`internal/core/schema.go`, `CommonIndexes`, between #88 and #95. Both PRs
inserted an index line at the same anchor. The resolution was **keep both**.

### 2.1 Both lines are present in the merged result

`origin/main` = `b5b1840`, `internal/core/schema.go`:

```go
1004: `CREATE UNIQUE INDEX IF NOT EXISTS idx_raw_memories_source_dedup ON raw_memories(source_db, source_id);`,
1005: // Supports the confidence-decay pass's due-artifact page query
1006: // (ordered by the watermark, bounded by LIMIT).
1007: `CREATE INDEX IF NOT EXISTS idx_memories_confidence_checked ON memories(confidence_checked_at);`,
1008: // Supports the shred path's removal of promoted staging rows by
1009: // identity (never by text match). Added by #88; preserved here
1010: // across this branch's rebase onto main.
1011: `CREATE INDEX IF NOT EXISTS idx_raw_memories_memory ON raw_memories(memory_id);`,
```

Both lines, each with its own explanatory comment. The `#95` squash diff
adds only the *comment* for #88's line — the line itself was already in
`main` from #88's merge, so nothing was duplicated and nothing was dropped.

Both `SafeMigrations` entries are present too:

```go
1475: {"raw_memories", "memory_id", "TEXT"},
1631: {"memories", "confidence_checked_at", "INTEGER"},
```

### 2.2 Definitions re-reviewed

| Index | Definition | Unique? | Partial? | Nullable column? | Idempotent? |
|---|---|---|---|---|---|
| `idx_memories_confidence_checked` | `ON memories(confidence_checked_at)` | **No** | No | Yes | Yes (`IF NOT EXISTS`) |
| `idx_raw_memories_memory` | `ON raw_memories(memory_id)` | **No** | No | Yes | Yes (`IF NOT EXISTS`) |

Neither imposes a uniqueness constraint, so neither can reject a write that
the pre-merge code accepted. Both are on nullable columns, so rows written
before the migration are indexed with `NULL` and remain readable.

### 2.3 Behaviour re-reviewed — and one finding

Measured on a freshly migrated scratch database:

| Check | Result |
|---|---|
| Both indexes created by the migration | ✅ both present in `sqlite_master` |
| Both non-unique, non-partial | ✅ `unique=0 partial=0` for both |
| Two `raw_memories` rows sharing one `memory_id` | ✅ both stored — no uniqueness constraint, no write rejected |
| `DELETE FROM raw_memories WHERE memory_id = ?` (the #88 shred path) | ✅ `SEARCH raw_memories USING INDEX idx_raw_memories_memory (memory_id=?)` — **the index is used** |

> **Finding (performance, not correctness).** `idx_memories_confidence_checked`
> is **not** chosen by SQLite's planner for the confidence-decay query that
> #95 added it for. The decay pass's participant filter is
> `confidenceDecayDueSelect` in `internal/core/confidence_maintenance.go`,
> which computes `last_check` through a `CASE` over `confidence_checked_at`
> / `created_at` and then filters on the computed alias in an outer query:
>
> ```
> SEARCH memories USING INDEX idx_memories_deleted (deleted_at=?)
> USE TEMP B-TREE FOR ORDER BY
> ```
>
> The index **is** usable — a query that filters and orders on the raw
> column without a competing `deleted_at` equality does select it
> (`SEARCH memories USING INDEX idx_memories_confidence_checked
> (confidence_checked_at>? AND confidence_checked_at<?)`) — but the decay
> query's `deleted_at IS NULL` equality makes SQLite choose
> `idx_memories_deleted` instead, and then sort with a temporary B-tree.
> There is also a pre-existing `idx_memories_decay ON memories(collection,
> deleted_at, is_long_term, weight)` (`internal/core/schema.go:1019`).
>
> **Consequence:** the index is harmless and may be useful, but it does not
> currently accelerate the query it was created for. This is a
> **performance-claim defect in #95, not a merge defect.** The index is
> preserved (as instructed), the merge stands, and the query is recorded as
> a candidate follow-up (§6.3) — not fixed during this round, because the
> decay pass currently scans a corpus of 85 memories and the priority is
> the release candidate and the parallel launch.

---

## 3. The integration checkpoint — evidence, not a baseline

Two checkpoints exist. **Neither is a merge result and neither was
installed.**

| Checkpoint | SHA | What it is | Status |
|---|---|---|---|
| `hal9001/r3-integration` | `cbe6af7` | a pre-existing combined tree from the earlier round | preserved as evidence; superseded |
| `integration/pre-release-12` | `a8c3b60` | a **validation artifact** assembled from the PR heads *as they stood* — not from merged heads | preserved as evidence; **superseded** |

The brief is explicit that the checkpoint is supporting evidence and never an
automatically approved merge result. That is how it was used.

> **The verified integration result is `main` itself.** Every PR was squash-
> merged into `main` in a reviewed order (§1), so the merged history *is*
> the integration — there is no separate tree to rebuild, and nothing from
> `a8c3b60` was installed anywhere.

---

## 4. Verification gate on the merged result

`origin/main` = `b5b1840`.

| Gate | Where | Result |
|---|---|---|
| `make build` | local | ✅ 5 binaries |
| `make test-race` | local on `b5b1840` | ✅ recorded in §4.1 |
| `make test-scripts` | local | ✅ 540 tests, OK (skipped=1), floor 50 |
| CI "Build + core tests" | run on `b5b1840` | §4.2 |
| CI "Full test suite" | run on `b5b1840` | §4.2 |
| CI "Release gate (cross-agent continuity + public-CLI subprocess suite)" | run on `b5b1840` | §4.2 |

**No requirement was reduced.** The race sweep, the coverage profile, the
installer-contract suite, the embedding-benchmark suite, the per-adapter
suites, `make test-release` and the `mpm-lint --gate` are all still in
place. #104 **added** a gate (the full-suite sweep can now fail); it removed
none.

### 4.1 Local race gate on `b5b1840`

```
$ cd ~/src/mpm && git rev-parse HEAD
b5b18401974ee559a485cda20abf8f7779f11bf2
$ make test-race
…
EXIT=0
```

### 4.2 CI on the merged commit

The gate was re-run against the **merged commit**, not against the PR head.
See the CI report in this round's deliverables for the run ids and the
per-check conclusions.

The PR-head runs are recorded too, because they are the evidence for the
merge decisions themselves:

| PR | Head | Run | Result |
|---|---|---|---|
| #104 | `fa80f48` | `40332886651` | ✅ all three green |
| `main` | `b5b1840` | `40337045142` | ✅ all three green |

---

## 5. Required checks and branch protection — current state

`main` is protected by **ruleset `25349644`** ("Protect main"), not by the
legacy branch-protection API (`/branches/main/protection` returns 404 — the
ruleset is the operative mechanism).

| Rule | Setting |
|---|---|
| Required status checks | `Build + core tests`, `Full test suite`, `Release gate (cross-agent continuity + public-CLI subprocess suite)` — all three, from workflow `build-test.yml` |
| Required approving reviews | **0** |
| Required review thread resolution | ✅ true |
| Allowed merge methods | merge, squash, rebase |
| Deletion / non-fast-forward | blocked |

Two consequences worth recording:

1. **Zero required approving reviews.** The ruleset does not enforce a
   second pair of eyes. Session A's substantive review is therefore a
   *discipline*, not an enforced control — and it is recorded in each PR's
   merge comment (the #104 merge comment carries the full review, including
   the negative test that proves the new regression test is not vacuous).
2. **`strict_required_status_checks_policy` is false.** A branch that is
   already up to date may be merged without a fresh run; a branch that is
   behind may merge on a stale green. **Re-checking CI against the merged
   commit is therefore a required step, not a nicety** — which is what §4.2
   does.

---

## 6. What is left

### 6.1 Immediately available merge work

**From the corrective set: none.** All fifteen PRs are merged, and the
merged result has passed the gate.

What remains, in priority order:

| # | Item | Owner | Blocking? |
|---|---|---|---|
| 1 | The single minimal implementation PR for whatever the B/C audits show is genuinely missing (see [`event-contract.md`](event-contract.md) §6.1) | A, after the audits | blocks dependent lifecycle implementation only |
| 2 | The event-contract agreement (§7 of the contract) | B + C | blocks dependent implementation only |
| 3 | A corrected `confidenceDecayDueSelect` that lets the planner use `idx_memories_confidence_checked` (or a composite index that serves the real query) | A | **no** — performance only, and the corpus is currently 85 memories |

### 6.2 The deployment gate

**Nothing has been deployed.** `~/.mpm` still runs commit `3df72315`, which
is **27 commits behind `main`**. Deployment happens only after:

1. an accepted integration milestone — i.e. a `main` commit whose full gate
   is green, which `b5b1840` is;
2. the promotion procedure in [`dogfooding-runbook.md`](dogfooding-runbook.md)
   §6, run in full, with the backup and the rollback path;
3. the release-candidate acceptance, which is **not** yet recorded.

**Not in this round.** The brief for this round says the live systemd
service must not be modified, and the sprint's target is the OpenCode-only
release candidate — which needs the B/C work first.

### 6.3 Candidate follow-up (not scheduled)

| Item | Why it is not now |
|---|---|
| Rewrite the decay participant query so `idx_memories_confidence_checked` is usable | performance only; 85 memories today; the index is harmless as it stands |
| Consider dropping the redundant `idx_memories_decay` | three decay-shaped indexes exist; two of them may be redundant. Needs measurement, not a guess |

---

## 7. Summary

| Question | Answer |
|---|---|
| Are all twelve corrective PRs merged? | ✅ Yes, plus #102, #103, #104. Fifteen in total, all squash-merged into `main`. |
| Is the integration result verified? | ✅ `main` = `b5b1840`; local `make test-race` and `make test-scripts` green; all three CI checks green on the merged commit. |
| Was the #88 ↔ #95 schema conflict resolved correctly? | ✅ Both index lines present (`schema.go:1007` and `:1011`), both migrations present (`:1475` and `:1631`), both non-unique and idempotent, and #88's shred delete demonstrably uses its index. One performance finding on #95's index, recorded in §2.3. |
| Was the checkpoint used as a merge result? | ✅ No. It was evidence only; `main` is the verified result. |
| Has anything been deployed to `~/.mpm`? | ✅ No. The live runtime is still at `3df72315`, 27 commits behind. |
| Is there immediately available merge work? | Only the post-audit implementation PR (§6.1). Nothing from the corrective set remains. |
