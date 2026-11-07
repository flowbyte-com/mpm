# Issue #178 — the second remediation slice: the durable user-state path

**Date:** 2026-11-07  
**Base:** `main` @ `056ce449` (PR #233 merged)  
**Branch:** `fix/issue-178-bare-write-slice2`  
**Issue:** #178 — *"90 bare pool writes still lose their artifact to ordinary
lock contention — the same defect class as #164, in a different call shape"*  
**Predecessor:** [`issue-178-bare-write-audit-and-slice-1-2026-11-06.md`](issue-178-bare-write-audit-and-slice-1-2026-11-06.md)  
**Inventory:** [`bare-write-inventory-2026-11-07.md`](bare-write-inventory-2026-11-07.md)

**Issue #178 is not closed by this work.** The scheduler's
duplicate-delivery crash window remains open by design (see
[`issue-178-corrections-phase-3-2026-11-03.md`](issue-178-corrections-phase-3-2026-11-03.md)
§5), and 46 ordinary contended writes remain in the inventory (§9). This is
the **second** remediation slice, not the remediation.

What this slice does:

- refreshes the inventory on the current tree (§2);
- reconciles the first slice's audit against the current code, and finds
  that **three ordinary writes were filed as one-shot migrations** — the
  misfiling is the reason they were invisible (§3);
- converts **eight** durable user-state writes — a session's summary, its
  handoffs, its work items and its topic links — onto the substrate's
  budgeted write path (§4, §5);
- makes the `migration-once` class **falsifiable**, so the misfiling that
  hid those three writes cannot recur silently (§6);
- reproduces every one of the eight failures against isolated SQLite
  databases and shows the red-to-green transition (§7);
- demonstrates thirteen mutations of the guard, all caught (§8).

---

## 1. The scope

The mission: continue eliminating ordinary SQLite writes that lose durable
state under lock contention, and publish a focused PR with red-to-green
evidence.

The constraint that shapes the slice: **PR #232 owns the cascade and
wake-delivery surfaces**, and the scheduler's delivery-state
implementation is off limits. This slice touches neither.

The constraint that shapes it most: **do not indiscriminately convert every
bare write**. The classification of intentional nonblocking maintenance and
of one-time migrations is preserved — §2.3 shows exactly what was preserved
and §6 shows how the class was made checkable rather than abandoned.

---

## 2. The refresh

### 2.1 The instrument

The standing guard **is** the inventory. `internal/core/bare_pool_write_ratchet_test.go`
walks the tree with an AST scanner, classifies every direct pool `Exec` /
`ExecContext` whose SQL carries a write verb, and compares the result
against `barePoolAllowlist` as a multiset keyed by (file, normalized SQL).
There is no second inventory to reconcile against, and the first slice's
record already established that the scanner's blind spots are known and
bounded.

Measured on `main` @ `056ce449`:

| | Before this slice | After this slice |
|---|---|---|
| allowlist entries | 128 | **120** |
| bare pool write occurrences | 141 | **133** |
| files with at least one | 53 | 53 |
| `reasonPropagated` occurrences | 79 | 74 |
| `reasonMigrationOnce` occurrences | 23 | **20** |
| `reasonStartupDDL` occurrences | 27 | 27 |
| `reasonBestEffort` occurrences | 8 | 8 |
| `reasonSharedPrimitive` occurrences | 2 | 2 |
| `reasonConnectionScoped` occurrences | 1 | 1 |
| `reasonTestFixture` occurrences | 1 | 1 |

The eight removed occurrences are the eight converted sites (§4).

### 2.2 The reconciliation

The first slice's audit recorded 49 remaining ordinary contended writes.
Re-measured on the current tree, the ordinary class of the inventory
document is **46** occurrences. The arithmetic:

| | Occurrences |
|---|---|
| The inventory document's `ordinary` class at `653e4633` (the first slice's base) | 71 |
| − the first slice's conversions (the memory write path) | −20 |
| − this slice's conversions of ordinary writes | −5 |
| − this slice's conversions of **misfiled** writes (three, §3) | −3 |
| **= the `ordinary` class on `main` @ `056ce449`** | **46** |

And the class totals on the current tree: **46 `ordinary`**, **49
`migration`**, **38 `maintenance`**, **0 `tx-owned`** — 133 occurrences
across 53 files.

### 2.3 What was preserved

Nothing in the `migration`, `maintenance` or `tx-owned` classes was
converted. Specifically preserved:

- the **27 startup-DDL** sites (schema and FTS setup);
- the **20 one-shot migrations** that remain (backfills, seeds, sentinel
  inserts, the raw-memory promotion, the lessons FTS sentinel, the two
  rebuilds, the created-at enforcement);
- the **8 best-effort** sites (the retention sweeps, the WAL checkpoint, the
  incremental vacuum);
- the **2 shared-primitive** sites (whose pool path already carries a
  budget — see the first slice's record §4.2);
- the **1 connection-scoped** `ATTACH` (whose semantics are per-connection,
  and where a pool budget would be wrong — see the first slice's record
  §5.1.1);
- the **1 test-fixture** site.

The `migration` class **lost three members** — and that is the finding,
not an inconsistency: §3 explains what they were.

---

## 3. The finding: three ordinary writes filed as one-shot migrations

### 3.1 What the misfiling hid

Reconciling the first slice's inventory against the current allowlist
surfaced three sites whose `Class` and `Why` said *one-shot migration* but
whose enclosing functions said something else entirely:

| Site | Filed as | Enclosing function | What it actually is |
|---|---|---|---|
| `internal/core/db.go` — `UPDATE sessions SET summary = ? WHERE session_id = ?` | `migration` / `reasonMigrationOnce` | `UpdateSessionSummary` | the ordinary write of a session's summary |
| `internal/core/db.go` — `INSERT OR IGNORE INTO topic_memberships (…)` | `migration` / `reasonMigrationOnce` | `AddMemoryToTopic` | the ordinary write that links a memory to a topic |
| `internal/core/db.go` — `DELETE FROM topic_memberships WHERE memory_id = ? AND topic_id = ?` | `migration` / `reasonMigrationOnce` | `RemoveMemoryFromTopic` | the ordinary write that unlinks a memory from a topic |

These are not one-shot migrations. They are routine user-path writes that
run whenever a caller asks for them — `AddMemoryToTopic` has four
production callers, `RemoveMemoryFromTopic` one.

**Both records carried the misfiling.** The inventory document's `Class`
column said `migration`, and the guard's `Why` column said
`reasonMigrationOnce`. The first slice's reconciliation read the document's
`ordinary` class as the source of truth for the ordinary accounting, so
these three were counted as *migrations* and never entered the ordinary
candidate list — which is exactly why the first slice's "what remains"
named `internal/core/db.go`'s session and work state but **not** the topic
membership writes.

### 3.2 Why no mechanical guard could see it

The ratchet is syntactic. It sees a call, a receiver and a verb. The
**classification** — is this write a one-shot migration or an ordinary
user-state write? — is a human judgement, and a wrong judgement is
invisible to every count-based guard. The `migration-once` class was
defined by what the write *does* ("idempotent or detection-gated, so a
retry or a rerun is safe"), never by *where it lives*.

That gap is fixable, and §6 fixes it. It is worth being precise about the
scope of the fix: the guard cannot verify that a migration is idempotent.
It can verify that a migration lives on a migration path — which is the
part that was actually wrong here.

---

## 4. The slice: eight sites, two files, four tables

### 4.1 The criterion

The first slice established the criterion (its §3.1): convert an ordinary
write whose loss discards **user or agent state**, and leave alone every
write whose loss costs a re-run rather than a state.

This slice narrows it to a coherent surface: **the durable state of a
session, its handoffs, its work items and its topic links**. Four tables,
two files, eight sites.

### 4.2 The ranking

The mission asked for a ranking by **consequence of lost state**,
**reproducibility** and **dependency risk**. The eight, in the order they
were taken:

| # | Site | Consequence of the loss | Reproducibility | Dependency risk |
|---|---|---|---|---|
| 1 | `DeleteHandoff` (`handoff.go`) | **A shred that reports success but leaves the row.** The operator believes the handoff is gone; it is still on disk. | certain — the DELETE is the whole operation | none — a pool write, outside any transaction |
| 2 | `MarkHandoffRead` (`handoff.go`) | **A handoff stays unread.** The next session does not consume the previous session's continuity record, and the read marker is gone. | certain | none |
| 3 | `DeriveWorkVerification` (`db.go`) | **A work item's verification stays stale** — a work that was contradicted can still read as `verified`. Six production callers. | certain | none — but it is called from *after* a `WithTx` block, so it is a pool write on purpose |
| 4 | `AddMemoryToTopic` (`db.go`) | **A topic link is lost.** The user's organisation of their own corpus silently disagrees with what they asked for. | certain | none |
| 5 | `RemoveMemoryFromTopic` (`db.go`) | **A topic link is not removed.** The same disagreement, in the other direction. | certain | none |
| 6 | `updateWorkStatus` (`db.go`) | **A lifecycle transition is lost** — a completed work stays open, or a cancelled one stays done. | certain | none — the transition is validated in a separate read *outside* the statement |
| 7 | `AddWork` (`db.go`) | **A work item never comes into existence.** | certain | none — the id is generated before the call |
| 8 | `UpdateSessionSummary` (`db.go`) | **A session's summary is not written.** | certain | none |

### 4.3 The three honest caveats

**`UpdateSessionSummary` has no production caller.** It is declared on the
public `CoreDB` interface (`internal/core/core.go:246`) and defined at
`internal/core/db.go:4360`, and nothing calls it — not the tools, not the
CLI, not a test. Its consequence today is nil.

**It also cannot succeed.** The `sessions` table has no `summary` column.
The schema (`internal/core/schema.go:40`) declares
`id, session_id, content, content_hash, created_at, source_path, metadata,
embedding`, there is no `ALTER TABLE sessions` anywhere in the tree, and
the call fails with `no such column: summary`. This was confirmed against
a live disposable database before the conversion was made.

That is a **schema** defect, not a durability defect, and it is deliberately
**not** fixed here: adding a column is a schema change with its own
migration, its own compatibility surface and its own blast radius, and it
belongs to a different change. The conversion is still correct — a bare
pool write of a durable user-state column is the defect this issue is
about, and the write should be durable whenever the column exists — and
the reproduction (§7) adds the column to its disposable database so the
production call can be exercised end to end. The missing column is filed
as its own follow-up (§10).

**`AddWork` and `updateWorkStatus` also have no production caller.** The
production paths are the event-sourced twins — `CreateWorkWithContext`,
`CompleteWorkWithContext`, `CancelWorkWithContext`, `UpdateWorkWithContext`
— which write through `addWorkTx` (already budgeted, at `db.go:6742`) and
through `AppendWorkEvent`. `AddWork`, `UpdateWork`, `CompleteWork` and
`CancelWork` are the direct APIs on the `CoreDB` interface, and only tests
call them today.

This is stated plainly because it changes the ranking: of the eight, **five
have live production callers** (`DeleteHandoff`, `MarkHandoffRead`,
`DeriveWorkVerification`, `AddMemoryToTopic`, `RemoveMemoryFromTopic`) and
**three are interface surface with no caller** (`UpdateSessionSummary`,
`AddWork`, `updateWorkStatus`). The three were still converted: they are
public surface, they are ordinary contended writes, and the whole point of
the ratchet is that the class is discharged site by site rather than by
argument about which callers happen to exist today.

### 4.4 What was deliberately NOT converted

| Site | Why not |
|---|---|
| `EndSessionV2`'s handoff INSERT (`handoff.go:224`, `:253`) | already budgeted (`dm.ExecTracked(…, 5, …)`) |
| `addWorkTx` (`db.go:6742`) | already budgeted — and the transactional twin of `AddWork` |
| `UpdateWorkWithContext`'s `UPDATE works SET status = ?, title = ?, …` (`db.go:8180` region) | already budgeted |
| `migrateWorkEvents`' backfill (`db.go:8394`) | a one-shot migration |
| `PruneHandoffs`' retention sweep (`handoff.go:592`) | best-effort maintenance |
| `WipeRecord`'s / `DeleteTopic`'s `topic_memberships` DELETEs | inside a transaction — transaction-owned |
| `AppendWorkEvent`'s work-event writes | inside a transaction |
| the scheduler's delivery-state writes | **owned by PR #232** |
| `SaveSystemConfig`, `DeleteSystemConfig`, `CreateTopic` (`db.go`) | named for slice 3 (§9) |

---

## 5. The conversions

Every conversion is the same shape as the first slice's: `dm.db.Exec(…)`
becomes `dm.ExecTracked(…, 5, …)`, the substrate's documented
statement-level budget. `ExecTracked` writes through the **same pool**
(`execTracked(dm.db, …)`), so the connection ownership does not change; it
adds the retry, the busy-retry counter and the watchdog instrumentation.

### 5.1 Retry safety, site by site

A retry must not be able to double-apply a write. Each site's argument:

| Site | Statement | Why the retry is safe |
|---|---|---|
| `UpdateSessionSummary` | `UPDATE sessions SET summary = ? WHERE session_id = ?` | writes an absolute value; a retry lands the same value |
| `AddMemoryToTopic` | `INSERT OR IGNORE INTO topic_memberships (…)` | duplicate-tolerant by construction — `OR IGNORE`; and `now` is computed **before** the call, so the retry carries the same timestamp |
| `RemoveMemoryFromTopic` | `DELETE FROM topic_memberships WHERE memory_id = ? AND topic_id = ?` | idempotent — a retry deletes nothing more. `RowsAffected` stays truthful: a `SQLITE_BUSY` attempt removed no row, so the count that comes back with the successful attempt is the count actually removed |
| `MarkHandoffRead` | `UPDATE session_handoffs SET read_at = ?, read_by = ? WHERE id = ? AND read_at IS NULL` | guarded by `read_at IS NULL`, so it is idempotent; `now` is computed **before** the call |
| `DeleteHandoff` | `DELETE FROM session_handoffs WHERE id = ?` | idempotent; the returned row count is truthful for the same reason as above |
| `AddWork` | `INSERT INTO works (…)` | the **id is generated before the call**, so a retry re-uses it rather than inserting a second work item — the same argument `addWorkTx` already makes for its transactional twin |
| `updateWorkStatus` | `UPDATE works SET status = ?, updated_at = ?, completed_at = ? WHERE id = ?` | writes absolute values, not increments; the state-machine validation happens in a separate read **outside** the statement, so a retry re-runs the same already-validated transition |
| `DeriveWorkVerification` | `UPDATE works SET verification = ? WHERE id = ?` | writes an absolute derived value; a retry lands the value the derivation computed |

### 5.2 Error propagation

Each site's error surface is **unchanged**: the same wrapping, the same
`%w` chains, the same sentinel wrapping. `ExecTracked` returns the driver's
error unwrapped (`execOutcome.Err` is the driver's error), so the
`isBusyError` branches downstream still classify it, and the wrapping call
sites still produce their established messages:

| Site | Error surface, before and after |
|---|---|
| `UpdateSessionSummary` | `return err` (unwrapped) — unchanged |
| `AddMemoryToTopic` | `return err` (unwrapped) — unchanged |
| `RemoveMemoryFromTopic` | `fmt.Errorf("remove membership: %w", err)` — unchanged |
| `MarkHandoffRead` | `fmt.Errorf("MarkHandoffRead: %w", err)` — unchanged |
| `DeleteHandoff` | `fmt.Errorf("DeleteHandoff: %w", err)`, plus the existing `LogAudit` on failure — unchanged |
| `AddWork` | `fmt.Errorf("insert work: %w", err)`, plus the existing read-back assertion — unchanged |
| `updateWorkStatus` | `fmt.Errorf("update work status: %w", err)` — unchanged |
| `DeriveWorkVerification` | `fmt.Errorf("derive verification: persist: %w", err)` — unchanged |

`DeleteHandoff`'s `RowsAffected` and `RemoveMemoryFromTopic`'s `rows > 0`
are both preserved, and both remain truthful under a retry (§5.1).

### 5.3 Transaction semantics

None of the eight sites is lexically inside a `WithTx` callback, and none
is on a `tx` handle — all eight were `dm.db.Exec`, i.e. pool writes
outside any transaction. Each was checked against its callers:

- `DeriveWorkVerification` is called from `CompleteWorkWithContext`,
  `CancelWorkWithContext`, `ReopenWorkWithContext`, `ResolveWorkContradiction`
  and `UpdateWorkWithContext` — in **every** case *after* the `WithTx`
  block has closed, which is why the derived verification is a separate
  write from the event that caused it;
- `AddWork` and `updateWorkStatus` are the **direct** APIs; their
  transactional twins (`addWorkTx`, `AppendWorkEvent`) are separate
  functions and were already budgeted;
- the handoff and topic-membership sites are called from thin handlers
  (`internal/core/tools/handlers.go`, `cmd/mpm/handlers_epistemology.go`,
  `internal/core/epistemology_tools.go`) with no transaction in scope.

The conversion does not change connection ownership: `dm.db.Exec` and
`dm.ExecTracked` both write through `dm.db`, the substrate's single shared
pool. `internal/core/db.go` is not in PR #232's owned file set, and no
file PR #232 owns was modified.

---

## 6. The guard: making `migration-once` falsifiable

### 6.1 What was added

The `migration-once` class is a claim about **when** a write runs: once, on
a migration path, never on a user path. The class is now pinned to the
place the write lives.

**`barePoolEntry` gained a `Func` field** — the name of the function the
call sits in. It is **required** for the `migration-once` class and
optional elsewhere. The scanner resolves it (a new `Func` field on
`barePoolSite`, resolved by `funcDeclRanges` / `enclosingFunc`).

**A new guard, `TestBarePoolMigrationOnceEntriesNameAMigrationFunction`,**
asserts, for every `migration-once` entry:

1. the entry names a function, and the live site is still **in that
   function** — a one-shot migration that *moved* is caught; and
2. the function name **or** the file path carries a **migration verb** from
   a closed vocabulary — `migrat`, `backfill`, `seed`, `rebuild`,
   `enforce`, `prune`, `sentinel`.

The vocabulary is deliberately short and deliberately **excludes** the
verbs a user path uses — `mark`, `promote`, `update` — because those
names are shared by ordinary surfaces (`MarkHandoffRead`, `PromoteMemory`,
`UpdateMemory`) and would let a user-path write pass as a migration.

All twenty surviving `migration-once` entries satisfy the rule, and they
satisfy it for the right reason: `migrateWorkEvents`, `backfillSharedFTSIfEmpty`,
`BackfillContentHash`, `BackfillLTMFlags`, `insertSeedRow`, `reviveSeedRow`,
`recordSentinel`, `recordLessonsFTSSentinel`, `PromoteRawMemoryBatch`,
`RebuildMemoriesColumnAffinity`, `EnforceMemoriesCreatedAtNotNull`,
`updateMemoryEmbedding`, `runReferenceBackfill`, `PruneCascadeOutbox`,
`takeBackup`, `markSyntheticTheories`, `handleMigrate`.

The three misfiled sites would **fail** the rule: `UpdateSessionSummary`,
`AddMemoryToTopic` and `RemoveMemoryFromTopic` carry no migration verb,
and `internal/core/db.go` carries none either. The rule catches exactly
the defect this slice found.

### 6.2 The approximation, stated honestly

The vocabulary check is a proxy. It can be fooled by a user-path function
whose name happens to carry a migration verb (`PromoteMemory`), and it
cannot see whether the migration is genuinely idempotent. What it *does*
guarantee is that the classification names a function, that the function is
where the write actually is, and that the classification was re-examined
whenever the code moved. The residual hole is closed by review, not by the
guard — and the guard's job is to make the review **visible**.

The approximation is load-bearing in one direction only: a migration whose
verb is not in the vocabulary is **over-reported** (it must be reviewed and
the vocabulary extended, a reviewable act), never under-reported.

### 6.3 The negative controls for the eight converted shapes

`TestBarePoolAllowlistDetectsReintroducedSlice2Writes` pins, for each of
the eight converted shapes: the shape is **not** on the allowlist under any
reason, and the classifier still **sees** it as a bare pool write — so a
live reintroduction fails the completeness guard by shape rather than by
luck. This follows the convention the first slice established
(`TestBarePoolAllowlistDetectsReintroducedWrite`, which pins the pre-fix
handoff UPSERT).

### 6.4 The deletions

The eight allowlist entries were **deleted**, which is how a conversion is
recorded:

| File | SQL | Was |
|---|---|---|
| `internal/core/db.go` | `UPDATE sessions SET summary = ? WHERE session_id = ?` | `reasonMigrationOnce` (misfiled) |
| `internal/core/db.go` | `INSERT OR IGNORE INTO topic_memberships (…)` | `reasonMigrationOnce` (misfiled) |
| `internal/core/db.go` | `DELETE FROM topic_memberships WHERE memory_id = ? AND topic_id = ?` | `reasonMigrationOnce` (misfiled) |
| `internal/core/handoff.go` | `UPDATE session_handoffs SET read_at = ?, read_by = ? WHERE id = ? AND read_at IS NULL` | `reasonPropagated` |
| `internal/core/handoff.go` | `DELETE FROM session_handoffs WHERE id = ?` | `reasonPropagated` |
| `internal/core/db.go` | `INSERT INTO works (…)` | `reasonPropagated` |
| `internal/core/db.go` | `UPDATE works SET status = ?, updated_at = ?, completed_at = ? WHERE id = ?` | `reasonPropagated` |
| `internal/core/db.go` | `UPDATE works SET verification = ? WHERE id = ?` | `reasonPropagated` |

---

## 7. The contention reproduction

### 7.1 The harness

The first slice's harness was extended, not duplicated:
`internal/core/bare_pool_write_contention_test.go` keeps its
`contentionDSN` (a temporary directory, a temporary SQLite file, a 100 ms
busy window) and its `holdWriteLock` (a `BEGIN IMMEDIATE` on a **second,
independent** connection to the same file).

Two refinements were needed.

**The busy window.** `dm.InitSchema()` sets the **production** busy window
on the pool's connections (`PRAGMA busy_timeout = 5000`,
`internal/core/db.go:2934`), so the disposable pool's 100 ms window was
being overwritten. The test now pins the pool to one connection and
restores the short window explicitly, and says why: the defect is that the
write has **no retry**, not that the window is five seconds.

**The release.** The first slice released the lock after a fixed sleep.
That is a race — the release has to land between one attempt's busy timeout
and the next attempt's, and a loaded machine can miss the window. These
tests release the lock the moment the manager has demonstrably **absorbed a
retry** (`dm.BusyRetryCount()` moves, which happens after a failed attempt
and before the backoff sleep), so the release lands inside the backoff
window where the next attempt is guaranteed to find the lock free. If the
write returns *before* any retry is observed, it took the bare path, lost
the artifact, and the test says so by name.

Every reproduction runs against a **disposable** database in a temporary
directory. No test opens the operator's `~/.mpm`, touches a running
service, or writes to any production database.

### 7.2 The reproduction

`TestConvertedDurableUserStateWritesSurviveContention` drives **each of
the eight production calls** — `UpdateSessionSummary`,
`AddMemoryToTopic`, `RemoveMemoryFromTopic`, `MarkHandoffRead`,
`DeleteHandoff`, `AddWork`, `CompleteWork` (which is `updateWorkStatus`),
`DeriveWorkVerification` — through its real `DatabaseManager` method while
a second connection holds the SQLite write lock.

The assertion has two halves, and both matter: the production call returns
no error, **and** the durable state it was supposed to write is there. A
write that reported success but left nothing behind fails the second half.

For `DeriveWorkVerification` the seeded row carries `verification =
'partial'` and the derive must write `'unverified'` — so "the column still
holds the seeded value" is distinguishable from "the write landed".

### 7.3 The red state

Before the conversions, all eight fail:

```
--- FAIL: TestConvertedDurableUserStateWritesSurviveContention (2.41s)
    --- FAIL: .../UpdateSessionSummary (0.28s)
        the production call returned WITHOUT absorbing a retry — it took the bare path and lost the artifact: database is locked
    --- FAIL: .../AddMemoryToTopic (0.25s)
        the production call returned WITHOUT absorbing a retry — it took the bare path and lost the artifact: database is locked
    --- FAIL: .../RemoveMemoryFromTopic (0.31s)
        the production call returned WITHOUT absorbing a retry — it took the bare path and lost the artifact: remove membership: database is locked
    --- FAIL: .../MarkHandoffRead (0.24s)
        the production call returned WITHOUT absorbing a retry — it took the bare path and lost the artifact: MarkHandoffRead: database is locked
    --- FAIL: .../DeleteHandoff (0.53s)
        the converted write did not survive the contention: DeleteHandoff: database is locked
    --- FAIL: .../AddWork (0.28s)
        the production call returned WITHOUT absorbing a retry — it took the bare path and lost the artifact: insert work: database is locked
    --- FAIL: .../CompleteWork (0.27s)
        the production call returned WITHOUT absorbing a retry — it took the bare path and lost the artifact: update work status: database is locked
    --- FAIL: .../DeriveWorkVerification (0.26s)
        the production call returned WITHOUT absorbing a retry — it took the bare path and lost the artifact: derive verification: persist: database is locked
```

The reproduction was also run against the **production** five-second busy
window before the short window was restored, and the failures are the same
shape — the bare write waits the full five seconds and then loses the
artifact:

```
    --- FAIL: .../UpdateSessionSummary (5.13s)
        the production call returned WITHOUT absorbing a retry — it took the bare path and lost the artifact: database is locked
```

### 7.4 The green state

After the conversions:

```
--- PASS: TestConvertedDurableUserStateWritesSurviveContention (2.70s)
    --- PASS: .../UpdateSessionSummary
    --- PASS: .../AddMemoryToTopic
    --- PASS: .../RemoveMemoryFromTopic
    --- PASS: .../MarkHandoffRead
    --- PASS: .../DeleteHandoff
    --- PASS: .../AddWork
    --- PASS: .../CompleteWork
    --- PASS: .../DeriveWorkVerification
```

---

## 8. The mutation demonstration

Thirteen mutations were applied to the tree, the guard was run, and each
file was reverted **byte-for-byte** afterwards (verified by `md5sum`).
**All thirteen were caught.**

| # | Mutation | Caught by |
|---|---|---|
| M1 | revert `UpdateSessionSummary` to a bare pool write | the completeness guard (surplus) |
| M2 | revert `MarkHandoffRead` to a bare pool write | the completeness guard |
| M3 | revert `DeleteHandoff` to a bare pool write | the completeness guard |
| M4 | revert `DeriveWorkVerification` to a bare pool write | the completeness guard |
| M5 | revert `AddWork` to a bare pool write | the completeness guard |
| M6 | revert `updateWorkStatus` to a bare pool write | the completeness guard |
| M7 | revert `AddMemoryToTopic` to a bare pool write | the completeness guard |
| M8 | revert `RemoveMemoryFromTopic` to a bare pool write | the completeness guard |
| M9 | re-add a deleted allowlist entry for a converted shape | the stale-entry guard **and** the new negative control |
| M10 | misfile a `migration-once` entry under an ordinary function name | the new migration-once guard |
| M11 | drop the `Func` from a `migration-once` entry | the new migration-once guard |
| M12 | name a `migration-once` entry after a user-path function | the new migration-once guard |
| M13 | relocate a `migration-once` write out of its migration function | the new migration-once guard |

---

## 9. What remains

**46 ordinary contended writes** remain in the inventory. The largest
concentration is `internal/core/db.go` (10), then `cmd/mpm/simple_cmds.go`
(4), `internal/core/cluster_proposals.go` (3),
`internal/core/lesson_proposals.go` (3), `internal/core/memory.go` (3),
`internal/core/skill_db.go` (3).

**The named candidates for slice 3**, in the order this slice would take
them:

1. `internal/core/db.go` — `SaveSystemConfig`, `DeleteSystemConfig`,
   `CreateTopic`: operator configuration and topic creation. The first
   slice's record named these and deliberately left them out of that
   slice; they are the natural next three, and the operator-configuration
   concern (a configuration write is a different kind of state from a
   session's state) is the thing to settle first.
2. `internal/core/db.go` — the lesson lifecycle (`DeleteLesson`,
   `RestoreLesson`, `ShredLesson`): the same "user state" argument as this
   slice's sites.
3. `internal/core/skill_db.go` and `internal/core/lesson_proposals.go`:
   proposals and skills, with the same lifecycle shape.
4. `cmd/mpm/simple_cmds.go`'s four memory writes — the CLI's own memory
   surface, which the first slice's record also named.

The **scheduler's delivery-state implementation** is not in that list and
is not this slice's business: PR #232 owns it, and the duplicate-delivery
crash window remains open by design.

---

## 10. What this does not close

- **The scheduler's duplicate-delivery crash window** (issue #178's own
  scope note). Open by design.
- **The missing `sessions.summary` column** (§4.3). `UpdateSessionSummary`
  is converted and correct, but the column it writes does not exist in the
  schema, so the call still fails with `no such column: summary`. This is a
  schema defect with its own migration and its own compatibility surface,
  and it is filed as its own follow-up rather than smuggled into a
  durability slice.
- **The 46 ordinary writes of §9.**
- **The idempotence half of the `migration-once` class.** The guard now
  checks *where* a migration lives; it does not and cannot check that the
  migration is idempotent (§6.2).
- **The scheduler's Stage B/C.** Not in scope by instruction.

---

## 11. Reproducing this work

```bash
# the guards (the ratchet, the new migration-once guard, the negative controls)
cd internal/core && go test -tags fts5 -count=1 -run 'TestBarePool' .

# the contention reproduction
cd internal/core && go test -tags fts5 -count=1 -v \
  -run 'TestConvertedDurableUserStateWritesSurviveContention' .

# the whole runtime
make test-race
```

The red state of §7.3 is reproduced by reverting any one of the eight
conversions and running the contention test — that is exactly mutation M1
through M8 in §8.

### 11.1 The validation results

| Gate | Result |
|---|---|
| `internal/core` suite (`go test -tags fts5 ./...`) | see §11.3 |
| `make test-race` (the canonical gate) | see §11.3 |
| `make test-scripts` | see §11.3 |
| `make release-gate` | see §11.3 |
| `go run ./cmd/mpm-lint --gate` | see §11.3 |
| `make docs-check` | see §11.3 |

### 11.2 One pre-existing failure, and what it is

`internal/core/build_config_invariants_test.go`'s
`TestBuildConfig_ToolsModuleIsInFullTestGates` runs `make -n test` and
fails with `exit status 2` **in a checkout that has not been built**. The
reason is visible in the dry-run output:

```
[mpm-install] ERROR: build did not produce /…/.build/bin/mpm
make[1]: *** [Makefile:1013: test-agent-installation] Error 1
```

`make -n` does not run the build, but the `test-agent-installation`
preflight still *checks* for the binary, so the check fails. This is a
build-ORDERING artefact, not a defect in the tree: the failure reproduces
identically on the **unmodified** base commit `056ce449` in a clean
worktree, and it disappears in the same worktree after `make build` — which
is what CI does before it sweeps. The same artefact was seen and explained
in the previous session's `internal/update` sweep ("developer build
artifact missing").

---

## 12. The review

This work has **not** been independently reviewed. It is published for
review, and it stops there.

The first slice's record §12 is explicit about the difference between a
self-review and an independent one, and this slice has had only the former:
the work was checked against the tree, against the mutation demonstration
and against the base commit's behaviour, by the agent that wrote it.

The review should focus on:

1. **The eight conversions** — the retry-safety arguments in §5.1, and the
   error-surface and transaction-semantics arguments in §5.2–§5.3.
2. **The `migration-once` guard** — whether the closed vocabulary in §6.1
   is the right one, and whether requiring the `Func` field on that class
   only (and not on the others) is the right scope.
3. **The scope decision** — three of the eight sites have no production
   caller today (§4.3), and the slice converted them anyway.
4. **The `sessions.summary` finding** — whether the missing column should
   be fixed in this slice after all, or filed separately as §10 proposes.
