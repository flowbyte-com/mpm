# `mpm ops maintain` reported failed maintenance as success

**Date:** 2026-11-09
**Scope:** error reporting only. No change to the decay algorithm, retry
budgets, schema, or any maintenance predicate.
**Status:** fix prepared; **stops at independent review.** No merge, no
deployment, no live-database writes.

---

## 1. The defect

`mpm ops maintain` printed

```
✅ Self-maintenance complete:
   Decayed weights:      0
   Pruned total:         0
   ...
```

and **exited 0** even when decay, pruning or consolidation had not run at all.

The chain had three links, and every one of them discarded the failure:

| layer | code | behaviour |
|---|---|---|
| operation | `MemoryStore.RunSelfMaintenance` | `if err == nil { stats.X = n }`, then `return stats, nil` — **the error is dropped and `nil` is always returned** |
| wrapper | `DatabaseManager.RunSelfMaintenance` | `if err != nil { return nil, err }` — which also **discards the partial statistics** |
| CLI | `handleMaintain` | `if err != nil { … return 1 }` — correct, but unreachable, because the layer below never returned an error |

So the CLI's exit-code contract was right and the whole report was a lie.

The six operations that could fail silently:

1. `DecayWeights`
2. `AutoPrunePolicy`
3. `ConsolidateMemories`
4. `CountMemoriesDueForReview` (the review backlog statistic)
5. the never-accessed count query
6. the low-weight count query

Each of 4–6 already logged an `AuditWarn` ("defaulting to 0"), so the failure
was *recorded*, but never *returned* — and the number shown to the operator was
a zero that looked like a real zero.

---

## 2. Additional defects found (reported, not all fixed here)

The mission asked whether the underlying operations suppress errors themselves.
They do.

### F1 — `AutoPrunePolicy` swallowed all three statement errors — **fixed**

```go
result, err := s.execTracked(`DELETE FROM memories …`, 0)
if err == nil {
    if rows, _ := result.RowsAffected(); rows > 0 {
        totalPruned += int(rows)
    }
}
```

Three arms (`expired`, `never-accessed`, `low-weight`), each
`if err == nil { … }`, and `return totalPruned, nil`. The function's `error`
return was **always nil**, so no amount of propagation above it could report a
failed prune.

**Why it is fixed here rather than reported only:** the mission's scope is
"ensure failures are reported accurately" for *decay, pruning and
consolidation*. Fixing `RunSelfMaintenance` alone would still have reported a
failed prune as success. All three arms now record their error, keep running
(a failed arm does not stop the other two), and return
`errors.Join(...)` with the partial count.

### F2 — `ConsolidateMemories` discarded the write result and inflated the count — **fixed**

```go
s.execTracked(`UPDATE memories SET deleted_at = … WHERE id = ?`, 0, cluster[k])
consolidated++
```

The result was not assigned at all, and `consolidated` was incremented
**unconditionally**. A failed soft-delete was therefore reported as a
consolidation that had happened — a second, independent way of reporting
success for work that did not occur.

Now the error is recorded and the row is **not** counted. The loop continues so
one bad row does not abandon the rest of the cluster.

### F3 — `ConsolidateMemories` cannot scan a NULL `tags` column — **not fixed, reported**

```go
var tags string
rows.Scan(&m.id, …, &tags, …)   // "converting NULL to string is unsupported"
```

Any memory whose `tags` column is SQL `NULL` aborts the whole consolidation
with a scan error. This is the "defensive SQL aggregates" invariant from
`AGENTS.md` (bind to `sql.Null*` or `COALESCE`), applied to a plain column.

Reachability: `addMemoryDirect` marshals a `[]string` and always writes JSON
(`[]` or `"null"`), never SQL NULL — so **normal saves are not affected**. The
rows at risk are ones created by direct SQL: imports (`ops ingest`),
`restore-db`, and any legacy schema state.

It is deliberately **not fixed here**: the fix (`COALESCE(tags,'[]')` or
`sql.NullString`) changes consolidation's behaviour for those rows from "aborts"
to "consolidates", which is a behaviour change that deserves its own review.
It is a one-line follow-up.

Worth noting: this defect was *invisible* before this change, because
`RunSelfMaintenance` swallowed the error and printed a 0. **The fix makes a
real latent bug visible**, which is the point of the change.

### F4 — `ConsolidateMemories` ignores a malformed embedding — **not fixed, reported**

```go
json.Unmarshal(embeddingJSON, &m.embedding)   // error discarded
```

A row whose `embedding` column is not valid JSON is silently treated as having
no embedding, so it never participates in similarity. Low impact (it degrades
consolidation, it does not corrupt anything), but it is the same class of
silent suppression. Not fixed here to keep the change to reporting.

### F5 — a failed `incremental_vacuum` is only a log line — **not fixed, reported**

`handleMaintain`'s deferred-vacuum step logs `slog.Warn` and still exits 0.
Outside the scope of this change (it is not one of the maintenance operations),
but it is the same "logged but not reported" shape.

---

## 3. The fix

Three files, 120 insertions, 43 deletions.

### 3.1 `MemoryStore.RunSelfMaintenance`

Runs every operation, records the failure by **name**, and returns
`stats` **and** `errors.Join(errs...)`:

```go
fail := func(op string, err error) {
    stats.Errors = append(stats.Errors, op)
    errs = append(errs, fmt.Errorf("%s: %w", op, err))
}

decayed, err := s.DecayWeights(DefaultDecayPolicies, 7)
stats.DecayedWeights = decayed      // partial count is real
if err != nil { fail("decay_weights", err) }
…
return stats, errors.Join(errs...)
```

The operation names are a fixed vocabulary: `decay_weights`, `auto_prune`,
`consolidate`, `review_backlog`, `never_accessed_count`,
`low_weight_count`.

**Partial statistics are preserved.** `DecayWeights`, `AutoPrunePolicy` and
`ConsolidateMemories` all already return the count they had reached when they
failed; that count is recorded rather than replaced with zero. A failed *count*
leaves its statistic at zero **and** reports the failure, so the zero is now
distinguishable from a real zero.

### 3.2 `MaintenanceStats.Errors`

A new `Errors []string` field, `json:"errors,omitempty"`. Empty means every
operation succeeded.

### 3.3 `DatabaseManager.RunSelfMaintenance`

Returns the **partial statistics together with the error** instead of
`nil, err`, and adds an `"errors"` key (only when there is a failure, so the
success payload's shape is unchanged).

### 3.4 CLI

```go
stats, err := dm.RunSelfMaintenance()
if err != nil && stats == nil { return usererror.Error(…) }   // nothing to report

if err != nil { print "⚠ Self-maintenance completed WITH ERRORS:" }
else          { print "✅ Self-maintenance complete:" }
…print the six statistics…
if err != nil {
    print "Failed operations: <names>"
    return usererror.Error("self-maintenance failed: %v", err)   // exit 1
}
```

The statistics are printed **either way**; the exit status carries the failure.
The deferred vacuum and the follow-up review suggestion are skipped on failure:
the command failed, and the operator should read the error rather than be given
a next step.

---

## 4. Semantics, stated exactly

| outcome | statistics | exit | banner |
|---|---|---|---|
| every operation succeeded | complete | **0** | `✅ Self-maintenance complete` |
| an action failed | partial (the counts that landed), failed operations named | **1** | `⚠ … WITH ERRORS` |
| a statistic query failed | that statistic is 0 **and** the failure is named | **1** | `⚠ … WITH ERRORS` |
| everything failed | all zero, all six named | **1** | `⚠ … WITH ERRORS` |

A caller that keys on the exit status (cron, a wrapper, a CI job) can no longer
mistake a partial or failed sweep for a successful one. A caller that reads the
statistics still gets every number that is true.

**Not changed:** the decay formula, the prune predicates, the consolidation
algorithm, every retry budget (`execTracked(…, 0)` and `(…, 3)` are untouched),
the schema, and the operation order.

---

## 5. Tests

All deterministic. Failures are injected with a SQLite trigger
(`BEFORE UPDATE OF <column> … RAISE(ABORT, …)`) or by removing one table —
never with timing, contention or sleeps.

### `internal/core/maintenance_error_reporting_test.go`

| test | pins |
|---|---|
| `…_SuccessReturnsNoError` | the control: a clean run returns nil, records no failures, **and actually exercises** decay/prune/counts (so the control cannot pass vacuously) |
| `…_DecayFailureIsReported` | a weight trigger fails only decay; the error names `decay_weights` and carries the cause; the prune and count statistics survive |
| `…_PruneFailureIsReported` | a `deleted_at` trigger fails prune; decay still succeeded and its count is preserved |
| `…_ConsolidateFailureIsReported` | a 60-row cluster (> `maxPerTopic` 50) with a failing soft-delete: the failure is named and `Consolidated` is **0**, not the inflated cluster size |
| `…_ReviewCountFailureIsReported` | dropping `memory_reviews` fails only the statistic; the actions still run |
| `…_EveryFailureIsListed` | dropping `memories` fails all six; every name appears in `stats.Errors` and in the error |
| `…_KeepsPartialStatsOnError` (wrapper) | the map is **not** nil on error and carries `errors` |
| `…_SuccessOmitsErrorsKey` (wrapper) | the success payload's shape is unchanged |

### `cmd/mpm/maintain_error_reporting_test.go`

Drives the **real router** and the **real handler**, and asserts the two things
an operator or a cron wrapper keys off:

| test | pins |
|---|---|
| `TestMaintain_SuccessExitsZero` | clean workspace → exit 0, success banner, no error banner |
| `TestMaintain_FailedDecayExitsNonZeroAndKeepsTheReport` | injected decay failure → **exit 1**, `WITH ERRORS`, `decay_weights` named, the cause surfaced, and the partial statistics still printed |

### Red-to-green

| tree | probe | result |
|---|---|---|
| pre-change (`origin/main` `34b93019`) | `store.RunSelfMaintenance()` after an injected decay failure | **`FAIL — a FAILED decay returned a nil error — the failure is reported as success`** |
| pre-change | `mpm ops maintain` after an injected decay failure | **`FAIL — a failed maintenance exited 0, want 1`** |
| this change | both | **PASS** — `decay_weights: weight decay failed …: injected …` |

The probe for the first row asserts only the returned error, so it compiles
against both trees; it is a behavioural red, not a compile error.

---

## 6. Gates

| gate | result |
|---|---|
| `make build` (all modules, real binary) | pass |
| `go vet -tags fts5 ./...` (root, `internal/core`, `internal/core/tools`) | pass |
| `go test -tags fts5 ./...` — `internal/core` | see §7 |
| `go test -tags fts5 ./...` — root (`cmd/mpm` included) | see §7 |
| `go test -tags fts5 ./...` — `internal/core/tools` | see §7 |
| `go test -race` on the maintenance and `cmd/mpm` packages | see §7 |
| `mpm-lint --gate` | **PASS** |
| `make docs-check` | **OK** (13 checks, 0 errors) |
| CI on the exact head | see the PR |

The targeted pre-existing suites for the touched operations —
`TestSelfMaintenance*`, `TestDecayFloor*`, `TestDirective*`, `TestRunGC*`,
`Decay`, `Consolidat*`, `Prune*` — pass unchanged.

---

## 7. What this does *not* do

- It does not change what maintenance **does**, only what it **reports**.
- It does not fix F3 (NULL `tags`), F4 (malformed embedding) or F5 (vacuum
  logging); they are reported above and are one-line follow-ups.
- It does not touch `AutoPrunePolicy`'s or `ConsolidateMemories`' predicates,
  limits or retry budgets.
- It does not make the maintenance transactional; a partially-committed
  maintenance is now *reported* as partial, not rolled back.
