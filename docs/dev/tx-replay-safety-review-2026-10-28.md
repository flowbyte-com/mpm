# Safety review — transaction-callback replay (issue #158)

**Date:** 2026-10-28
**Reviewer:** Session B
**Subject:** PR #163 — `fix(core): a stale WAL snapshot must restart the transaction, not retry the statement`
**Verdict:** **PASS.** No callback duplicates a non-transactional side effect when the
transaction is restarted. Two budget defects were found and are addressed in
[#164](https://github.com/flowbyte-com/mpm/issues/164) (separate PR).

**Status note.** This review was written as the pre-merge gate for #163. #163 was
merged at 17:09:49Z with its first commit; the review's own test commit landed on
the branch afterwards and is therefore **not** part of the merge. The review's
evidence and its findings are recorded here, and the tests travel with this
document.

---

## 1. What is being reviewed

Issue #158 fixed a stale-WAL-snapshot failure. `WithTx` opens a **deferred**
transaction; it takes a read snapshot at its first read and acquires the write lock
at its first write. If another connection commits in between, the write cannot
upgrade and SQLite returns `SQLITE_BUSY_SNAPSHOT` (extended code 517) — an error
that does **not** invoke the busy handler, so `busy_timeout` never applies to it.

The fix makes `WithTx` **restart** the transaction on that error: the abandoned
transaction is rolled back and **the callback is re-run** against a fresh one.

Re-running a callback is the whole risk. The question this review answers is:

> Can every affected `WithTx` callback be retried without duplicating
> non-transactional side effects, emitting premature external actions, or
> violating idempotence? And what happens to cancellation and nested retry budgets?

Nothing in the live runtime, the live database, the scheduler or the OpenCode
installation was touched.

---

## 2. Method

Three independent lines of evidence, all against **file-backed WAL databases**
opened with the **production DSN** (`_foreign_keys=1&_busy_timeout=5000`) — WAL is
where `SQLITE_BUSY_SNAPSHOT` exists, and an in-memory shared-cache database cannot
produce it at all.

| # | Line of evidence | What it establishes |
|---|---|---|
| 1 | **Static inventory** of all 26 `WithTx` call sites and every helper reachable from them | which callbacks can be replayed at all, and what they touch outside the transaction |
| 2 | **Empirical table diff** — the same production write run twice, once uncontended and once with a real transaction restart forced, then every table's row count compared | that a restart changes nothing durable |
| 3 | **Mutation testing** — a deliberately injected out-of-transaction side effect, on the DB side and on the filesystem side | that the diff harness in (2) would actually catch a violation |

Every test asserts the **mechanism** that produced its result, so none of them can
pass vacuously: the forced-restart tests require a `busy_snapshot` watchdog record
to exist, and the write-first proof requires the competing write to be *refused*.

---

## 3. Finding 1 — most callbacks cannot be replayed at all (structural immunity)

This is the load-bearing structural fact, and it was verified empirically rather
than assumed.

In WAL mode a connection that holds the **write lock** blocks every other writer.
So:

* a callback whose **first** database statement is a **READ** takes a snapshot,
  and a competing commit can invalidate it → **replayable**;
* a callback whose **first** database statement is a **WRITE** acquires the write
  lock immediately, and no other connection can commit until it finishes → the
  snapshot can never go stale → **structurally immune**.

`TestWriteFirstTransactionCannotExperienceAStaleSnapshot` proves the second half
directly: a write-first transaction holds the lock, a competing connection's write
is **refused** (`database is locked`), so the WAL cannot advance, so no snapshot
can be invalidated.

The 26 `WithTx` call sites divide as follows.

### Replayable — the callback reads before it writes (13 of 26)

| Site | What the callback does |
|---|---|
| `internal/core/tools/handlers.go:5531` | `promote scratchpad`: `SELECT` the scratchpad → `INSERT` the memory → `DELETE` the scratchpad |
| `internal/core/epistemology_tools.go:196` | `propose theory`: dedup probe → `INSERT` the theory |
| `internal/core/epistemology_tools.go:490` | `record decision`: dedup probe → `INSERT` the decision |
| `internal/core/changelog_mcp.go:287` | changelog entry: dedup probe → `INSERT` the memory + one evidence row per confirmation/contradiction |
| `internal/core/confidence_maintenance.go:249` | confidence decay: `artifactExists` → recompute → `UPDATE` the watermark |
| `internal/core/cascade_provenance.go:104` | citation: (only when `source_type` is empty) a bare-pool type-resolution read → `INSERT` the citation |
| `internal/core/db.go:7153, 7181, 7241, 7284, 7615, 7635, 7803` | work transitions: `SELECT MAX(event_index)` → `INSERT` the work event → `UPDATE` the works projection |

### Structurally immune — the callback writes first (13 of 26)

`internal/core/reference_db.go:83, 205` · `internal/core/epistemology_tools.go:334,
573, 668` · `internal/core/work_purge.go:485` · `internal/core/evidence_store.go:479`
· `internal/core/gc_tools.go:375` · `internal/core/db.go:3759, 7110` ·
`internal/core/capability/store.go:64` (the wrapper; both of its callbacks write
first) · `internal/scheduler/drill_handler.go:84, 126`

**These are not replayed, so they cannot double-write.** That is a structural
property of the lock, not a property of the code's discipline — which is the right
kind of guarantee.

---

## 4. Finding 2 — no callback performs an out-of-transaction side effect

### 4.1 The static picture

Every callback body was scanned for: bare-pool writes, bare-pool reads, audit
emissions, `append`, goroutine spawns, and filesystem or environment writes.

* **Goroutine spawns inside a callback: 0.** (`ChallengeMemoryAsync`, the one
  fire-and-forget goroutine in the write paths, is invoked *after* `tx.Commit()`
  and outside any `WithTx`.)
* **Filesystem writes inside a callback: 0.** The mirror appends
  (`appendMirrorLine`) live in the `MemoryStore` path, which is not transactional,
  and in the lesson path, which is also not transactional.
* **Bare-pool writes inside a callback: only `dm.LogAudit` /
  `dm.LogAuditForInvocation`**, and every one of them sits on a path that either
  aborts the callback or runs after the transaction already holds the write lock.
  See §4.3.
* **`AppendWorkEvent`'s 13 bare-pool statements are all inside `else` branches
  guarded by `node == nil`.** When a node is supplied — i.e. when the caller is
  inside a transaction — every write goes through `node.ExecTracked`. This was
  checked mechanically, not by eye.

### 4.2 The empirical picture

`TestWithTxReplay_ProducesNoDuplicatedSideEffects` runs four real production
write paths — **decision**, **theory**, **changelog**, **work event** — twice:

1. in a **control** database with no contention, recording every table's row count
   and a census of every file in the database's directory;
2. in a second database with a **real transaction restart forced**, recording the
   same.

It then asserts:

* a `busy_snapshot` watchdog record exists — so the restart is **proven**, not
  assumed; and
* the two table-count maps are **identical**, and the two file censuses show the
  same external side effects.

**Result: identical.** Across every table — `memories`, `lessons_base`,
`artifact_provenance`, `epistemic_provenance`, `evidence`, `work_events`, `works`,
`system_audit_log`, and every FTS shadow table — and across the filesystem.

`TestReplayReachesNoAuditRowOutsideTheTransaction` states the narrowest form of
the property: `system_audit_log` is written through the **bare pool**, so its rows
are never rolled back with a transaction. It is the one sink where a duplicated
side effect would be invisible everywhere else. Its row count after a forced
restart is **identical** to the control's.

### 4.3 The one sharp edge — and why it is self-limiting

The audit writer uses the bare pool. A bare-pool write **commits**, and a commit
advances the WAL. If that happened *inside* a callback that had already taken its
read snapshot, it would invalidate the callback's **own** snapshot — the next write
would return `SQLITE_BUSY_SNAPSHOT`, the transaction would restart, the audit
would fire again, and the loop would run to the budget's end.

This was confirmed experimentally while building the mutation tests: an injected
bare-pool audit on the memory write path made the **control** run fail with
`database is locked` after exhausting the restart budget.

So the hazard is not "a duplicated side effect" — it is "the transaction breaks
itself". That is a much better failure mode, but it is a sharp edge worth
recording: **an out-of-transaction write placed after a callback's first read
will break that callback.** Today no callback does this:

* the three pre-`INSERT` audits in `saveMemoryRow` fire only on validation,
  sensitive-content and poison-content rejections — all of which **return an
  error**, so the callback is never replayed;
* the `begin failed` audit fires only when `nodeUnwrapTx(node) == nil`, i.e.
  *outside* any transaction;
* the `record failed` audits fire after the write lock is already held, so the
  bare-pool write is refused rather than duplicated.

**Recommendation for future work:** if an audit emit is ever added to the middle
of a callback, move it *before* the callback's first read, or route it through the
transaction. It is a two-line change today and a debugging afternoon later.

### 4.4 Mutation testing — the harness would catch a violation

A harness that passes is only evidence if it can fail. Two mutations were injected
into the production write path and the review's own tests were re-run.

**Mutation A — an out-of-transaction DB write** (a bare-pool `system_audit_log`
insert, placed before the callback's read snapshot so it duplicates cleanly):

```
tx_replay_safety_test.go:334: the restart changed the durable state:
      system_audit_log: control=0 replayed=1
```

**Mutation B — an external action** (a file append inside the callback, outside
the transaction):

```
tx_replay_safety_test.go:464: the restart produced a different external side effect:
      control:  [mutation.log (+13 bytes)]
      replayed: [mutation.log (+26 bytes)]
```

Exactly double, as a duplicated external action must be. Both mutations were
reverted; the committed code is unmodified.

**A note on Mutation A's control count.** The control run shows `0` rather than
`1`, because in the *control* run the audit write is attempted while the
transaction already holds the write lock (the memory `INSERT` precedes it) — the
bare-pool write is refused after the busy timeout. The point of the mutation was
the *difference*, and the difference was detected. The committed test does not
depend on this behaviour.

---

## 5. Finding 3 — cancellation

`WithTx` has no `context.Context` parameter. The cancellation discipline therefore
lives in the callers:

* `WithTx` never restarts a callback whose error satisfies
  `errors.Is(err, context.Canceled)` or `errors.Is(err, context.DeadlineExceeded)`
  — covered by `TestWithTx_CancelledCallbackIsNotRetried` in PR #163.
* `internal/capability`'s `withRetry` checks `ctx.Err()` before every attempt and
  inside every backoff sleep, so a cancelled caller does not open a new
  transaction.

**The residual gap, stated plainly:** if a callback fails with a *busy* error while
the caller's context is **already** cancelled, `WithTx` will still restart it — up
to three times — because the busy error does not carry the cancellation. The
restarted callback fails fast on the cancelled context, so the cost is bounded, but
the callback body can execute more than once after cancellation.

Closing that gap properly means giving `WithTx` a context. That is an API change
across every caller, and it is **not** worth it in a release candidate for a
bounded, already-fast failure. It is recorded here as a known limitation rather
than silently fixed.

---

## 6. Finding 4 — the nested retry budgets multiply (12 transactions)

This is a real defect and it is **not** introduced by #163 — #163 made it visible
by adding an inner budget where there was previously none.

`internal/capability`'s `RecordInvocation` retries the **whole transaction**:

```go
err := s.withRetry(ctx, func() error {          // budget: 4 attempts
    return s.withTx(func(n internal.DBNode) error {   // → dm.WithTx
        return s.writer.Write(ctx, n, payload)
    })
})
```

`withTx` is `dm.WithTx`, which now restarts a stale snapshot up to
`TxSnapshotMaxAttempts` (3) times. `withRetry` treats a busy error as retryable —
and a snapshot conflict **is** a busy error. So the budgets multiply:

> **4 outer attempts × 3 inner attempts = 12 transaction attempts and 12 callback
> invocations.**

`TestWithRetry_MultipliesTheSnapshotBudget` measures this exactly against a
real WAL database, by making every attempt conflict and counting the
`busy_snapshot` watchdog records:

```
callback invocations=12, transaction attempts=12 (inner budget 3, outer budget 4)
```

The same multiplication exists in two other places:

| Site | Outer budget | Inner budget | Worst case |
|---|---|---|---|
| `internal/core/capability/store_invocation.go:305` | 4 | 3 | **12** transactions |
| `internal/core/capability/executor_fracture.go:226` | 4 | 3 | **12** transactions |
| `internal/core/reference_ingest.go:266` | 5 | 3 | **15** transactions |

`reference_ingest.go` is the most interesting case, because its comment says the
retry lives there *precisely because* of the stale snapshot:

> *"WithTx deliberately does not retry callbacks (see its contract), and its
> per-statement retry cannot help once a read snapshot is stale, so the retry lives
> here — re-resolving the identity each attempt so the loser of the race sees the
> winner's row and applies an update to it instead of failing."*

**That comment is now stale, and the retry it describes is now redundant.** After
#163, `WithTx` takes a fresh snapshot on each of its restarts, so the outer loop
is buying a fourth fresh snapshot at the price of multiplying the whole budget.
(The ingestion path itself is idempotent — it resolves a deterministic id and
re-resolves it per attempt — so the multiplication there costs latency, not
correctness.)

**Recommendation:** the layer that owns a busy class should own its budget. The
snapshot class belongs to `WithTx`; the outer loops should keep their budget for
the class they were written for (ordinary contention that the statement-level
retry could not clear) and stop re-driving the snapshot class. That is the
correction delivered in #164.

---

## 7. Finding 5 — the outer retry can re-drive an ambiguous commit (idempotence)

**Pre-existing, not introduced by #163 — but in scope for #164.**

`WithTx` does not retry a failed commit, and its comment explains why:

> *"A commit failure is NOT retried here. 'cannot commit — SQLITE_BUSY' is
> ambiguous about whether the write landed, so re-running the callback could
> double-write."*

That reasoning is correct, and it is honoured **inside** `WithTx`. It is **not**
honoured outside it. `withRetry` retries on **any** busy error, so a commit
failure that surfaces as `database is locked` re-drives the entire callback — with
a **fresh UUID** and a **fresh counter increment**.

For the capability Store that means, in the worst case:

* **two** `capability_invocations` rows for one invocation (the ids are
  `uuid.NewString()`, so the second is not a primary-key collision and the
  `ON CONFLICT` semantics do not apply), and
* a **double-incremented** `success_count` / `failure_count` on the capability.

The same shape applies to `reference_ingest.go`'s loop and to the fracture
cascade's loop.

The fix is narrow: `WithTx` should mark a failed commit so that the outer loops
can recognise it and refuse to re-drive the callback. #164 delivers that.

**How likely is this?** A WAL `COMMIT` can fail with `SQLITE_BUSY` when the
commit's checkpoint cannot complete because another connection is still reading
the WAL. It is rare — it did not occur in the live-runtime audit — but it is a
*silent* corruption when it does, because the counters are the substrate's
probation input.

---

## 8. What this review does **not** cover

* **The `BEGIN IMMEDIATE` design.** Deferred transactions remain the substrate's
  model; the review says nothing about whether that should change. The design
  question is unchanged from PR #163 and remains deferred.
* **Non-`WithTx` write paths.** `AddLesson` (a bare, unretried `db.Exec`) and
  `addEvidenceInTx` (`retries=0`) are outside this review's scope — they cannot
  be replayed, but they **lose their artifact** under ordinary contention. That is
  issue #164, and it is addressed in a separate PR.
* **The `AppendWorkEvent` path with `node == nil`.** It writes through
  `dm.db.Exec` with no retry budget (`evidence_store.go:507` calls it that way).
  Same class as #164; fixed there.
* **Multi-process behaviour.** Every measurement here is within one process
  against one database file. A second *process* writing the same file is the same
  SQLite-level story (the lock and the WAL are shared), but it was not measured.

---

## 9. Verdict and recommendation

**PASS — PR #163 may merge.**

* Every affected `WithTx` callback is replay-safe: **no** duplicate durable row,
  **no** duplicate external action, **no** idempotence violation — established
  statically, empirically, and by mutation.
* Thirteen of the twenty-six call sites cannot be replayed at all, by a
  structural property of the WAL write lock rather than by code discipline.
* Cancellation is honoured for every error that carries it; the residual gap
  (§5) is bounded and is a known limitation, not a blocker.

**Do not treat the two budget defects as blockers for #163.** They are pre-existing
or newly-visible, they are bounded, and they are addressed in a separate, narrowly
scoped PR:

| Defect | Where | Severity | Fix |
|---|---|---|---|
| Multiplied retry budgets (12–15 transactions) | capability store, fracture cascade, reference ingest | Medium — latency and cost, no corruption | #164: the owning layer keeps the budget |
| Outer retry re-drives an ambiguous commit | capability store, fracture cascade | **High** — a duplicate telemetry row and a double-incremented counter, silently | #164: mark the commit failure so it is not re-driven |
| `AddLesson` loses its lesson under ordinary contention | `internal/core/db.go` | Medium — durable knowledge lost | #164: transactional + retried |
| `addEvidenceInTx` (`retries=0`) loses its evidence | `internal/core/evidence_store.go` | Medium — durable knowledge lost | #164: real retry budget |

**Recommendation on whether #164 should block the next release: NO, with one
exception.** The two write-loss defects are ordinary contention, not the
snapshot class; the live-runtime audit measured the substrate's contention rate at
two events in six days across 14,521 invocations. The commit-retry defect is the
one item that argues for urgency, because its failure is silent and it corrupts a
counter that the probation logic reads. If #164 can land before the release
candidate is cut, land it; if not, it should be the first change after it.

---

## 10. Evidence index

| Artefact | What it shows |
|---|---|
| `internal/core/tx_replay_safety_test.go` | the table diff, the file census, the write-first structural proof, the audit-log assertion |
| `internal/core/tx_busy_snapshot_test.go` (in #163) | the error class, the removed futile retry, the bounded restart, cancellation, dedup convergence |
| `internal/core/capability/tx_retry_budget_test.go` (in the #164 PR) | the budget measurement — 12 transaction attempts before the correction, 3 after |
| `docs/dev/live-runtime-health-audit-2026-10-28.md` | the live-runtime evidence the fix is based on |

---

## 11. One unrelated finding: `main` did not build

While preparing the #164 branch, `go build ./...` on `main` failed:

```
cmd/mpm-critic/main.go:27:2: "path/filepath" imported and not used
```

The import was left behind by #155 (`00d526d9`, "ONE resolver for the database
path"). Because CI runs `go build -tags fts5 ./...` and `make build`, **all three
CI jobs on `main` were red** — the run at `db6c64ae` failed in `Build + core
tests`, `Full test suite` and `Release gate` alike.

This was reported to Session A and **fixed by #166** (`3cc709d3`, "drop the import
the D4 resolver made unused"), which landed before the #164 branch was rebased.
The #164 PR therefore does not carry the fix.

It is recorded here because it is a useful reminder: an unused import is not
cosmetic — it takes the whole branch's CI red, and it does so silently, since
nothing in the review's own scope would have surfaced it.
