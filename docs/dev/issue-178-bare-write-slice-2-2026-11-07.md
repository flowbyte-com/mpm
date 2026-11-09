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
- demonstrates thirteen mutations of the guard, all caught (§8);
- and — after **C's independent review** (§12) — **removes the file-path
  escape hatch** from that guard, reclassifies the four entries that
  depended on it by their actual execution semantics, and encodes C's
  M17 mutation as a permanent negative control (§6.1, §6.2, §8.1,
  §12.2).

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

### 5.2.1 The one place the budget is paid twice — `DeleteHandoff`'s failure path

`DeleteHandoff` is the only one of the eight with a **second** write on its
failure path: when the DELETE fails it calls `dm.LogAudit(AuditWarn,
"handoff", "DeleteHandoff failed: …")`, and `LogAudit` writes an audit row
through `dm.ExecTracked(..., 5, ...)` of its own. So a failed shred spends
the statement-level budget **twice**.

Three things about that, all measured rather than argued:

**The original error survives.** `LogAudit` returns nothing; on its own
failure it prints to stderr and returns. It cannot mask or replace the
caller's error, and `DeleteHandoff` returns
`fmt.Errorf("DeleteHandoff: %w", err)` with the DELETE's own error. Both
trees return exactly `DeleteHandoff: database is locked` for the same
contention.

**The shape is pre-existing.** `internal/core/audit.go` is **not touched by
this slice**, and `LogAudit` was already budgeted at the base commit
(`dm.ExecTracked(…, 5, …)`, `internal/core/audit.go:113`). The base tree
therefore already spends one budget on a failure path; this slice adds the
second.

**The cost is latency, and it is the cost of the whole policy.** Measured
against a held write lock with the harness's 100 ms busy window:

| Tree | `DeleteHandoff`'s failure path |
|---|---|
| the base `056ce449` (bare DELETE) | **3.811 s** — the DELETE fails in ~100 ms, `LogAudit` spends the rest |
| this tree (budgeted DELETE) | **7.417 s** — the DELETE spends a budget, `LogAudit` spends another |

Both return `DeleteHandoff: database is locked` with `rows=0`. The base's
3.811 s is almost entirely the audit's own budget; this slice's 7.417 s is
two budgets. With the **production** 5000 ms busy window the same shape is
~5 s + ~33 s before and ~33 s + ~33 s after.

That is the trade this issue asks for, made visible: a failed shred now
takes longer to fail, and in exchange a shred that only lost to contention
**lands**. The audit row — the record that the shred failed — becomes more
likely to be written, not less, because it now has the same tolerance as
the operation it describes.

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
2. the entry satisfies `classifyMigrationOnce` — **the FUNCTION NAME**
   carries a **migration verb** from a closed vocabulary (`migrat`,
   `backfill`, `seed`, `rebuild`, `enforce`, `prune`, `sentinel`), **or**
   the exact `(file, function)` pair is **explicitly enumerated** in
   `barePoolMigrationExceptions` with a stated reason.

> **CORRECTED after C's independent review (finding F1).** The rule as
> first shipped accepted an entry when the function name **or the file
> path** carried a migration verb. The file-path arm was an unchecked
> escape hatch: it granted migration status to **every** write in a
> verb-named file, so a **new** one-shot migration in such a file was
> accepted **silently** — every guard test green. C's mutation M17
> demonstrated exactly that. **The file path is not evidence.** A path is
> a property of the FILE; the class is a claim about the WRITE. The rule
> is now `classifyMigrationOnce`, and the file path appears nowhere in it.

The vocabulary is deliberately short and deliberately **excludes** the
verbs a user path uses — `mark`, `promote`, `update` — because those
names are shared by ordinary surfaces (`MarkHandoffRead`, `PromoteMemory`,
`UpdateMemory`) and would let a user-path write pass as a migration.

**The exclusion has a consequence, and it is not a gap.** Two genuine
migrations carry exactly those verbs in their **function names** —
`markSyntheticTheories` and `PromoteRawMemoryBatch`. Before the
correction they passed on the **file-path arm**, which is why this
record's earlier claim that all twenty entries "satisfy it for the right
reason" was **false for four of the twenty**. They now hold **explicit
enumerated exceptions**, which is the honest place for a migration whose
name the vocabulary cannot name.

#### The twenty entries, and how each is now classified

Sixteen satisfy the **primary rule** — the function name carries a
migration verb:

`runReferenceBackfill`, `PruneCascadeOutbox`, `backfillSharedFTSIfEmpty`,
`migrateWorkEvents` (×2), `recordSentinel`, `recordLessonsFTSSentinel`,
`BackfillContentHash`, `BackfillLTMFlags`,
`RebuildMemoriesColumnAffinity` (×2),
`EnforceMemoriesCreatedAtNotNull` (×2), `insertSeedRow`, `reviveSeedRow`,
`handleMigrate`.

Four hold an **enumerated exception**, each classified by its **actual
execution semantics** — when it runs, what gates it, what a rerun does —
not by its name:

| Entry | What it actually is | Why it needs an exception |
|---|---|---|
| `cmd/mpm/backfill_embeddings.go` — `updateMemoryEmbedding` | the **per-row update** inside the operator-driven `mpm backfill-embeddings` **command**. It runs only when an operator runs the backfill, it writes an **absolute** embedding value, and a rerun converges on the same value. Never on an agent's path. | its name names the *row*, not the *operation* — it carries no migration verb |
| `internal/core/embedding_migration.go` — `markSyntheticTheories` | **step 3** of `RunMigration`, whose whole orchestrator **short-circuits on the `embedding_migration_log` sentinel row**, so the step runs at most once per database. It writes the absolute value `synthetic = 1`. | its verb `mark` is shared with the user path `MarkHandoffRead`, so it cannot be in the vocabulary |
| `internal/core/embedding_migration.go` — `takeBackup` | **step 1** of the same sentinel-gated `RunMigration`: the pre-migration `VACUUM INTO` backup. It writes a **timestamped file**, not a row, so a rerun cannot corrupt anything. | its verb `backup` is not a migration verb |
| `internal/core/migrate_md.go` — `PromoteRawMemoryBatch` | the operator-driven raw-memory promotion (`mpm migrate`). It is **not** one-shot in the "runs once" sense, but it **is** detection-gated in the sense the class means: each row is promoted at most once because the promotion stamps `status='approved'`, so a rerun only picks up newly pending rows. It writes absolute values. | its verb `promote` is shared with the user path `PromoteMemory`, so it cannot be in the vocabulary |

The exception list is **closed and enumerated**: it is a map of exact
`(file, function)` pairs to reasons, not a pattern. A pair that is not in
it gets nothing — which is what makes a **second** new write in
`internal/core/migrate_md.go` (or in `embedding_migration.go`, or in
`backfill_embeddings.go`) **rejected**, even though two of those files
hold grants. The guard additionally asserts that every grant states a
reason, is used by exactly one live entry, and is **not redundant** (an
entry whose function name already carries a vocabulary verb must not also
hold a grant, or the grant would be a second, invisible way in).

The three misfiled sites would **fail** the rule: `UpdateSessionSummary`,
`AddMemoryToTopic` and `RemoveMemoryFromTopic` carry no migration verb,
and their file `internal/core/db.go` carries none either — and the file
path is no longer evidence in any case. The rule catches exactly the
defect this slice found, and it now also catches the shape C found.

### 6.2 The approximation, stated honestly

> **CORRECTED after C's independent review (finding F1).** This section
> previously claimed that the approximation was "load-bearing in one
> direction only: a migration whose verb is not in the vocabulary is
> **over-reported** … **never under-reported**". **That sentence was
> false**, and C demonstrated the counterexample: the file-path arm
> under-reported, silently, in exactly the direction the claim denied.

What the guard now demonstrably enforces:

- **A new write cannot inherit migration status from its file's path.**
  The path is not consulted. A new one-shot migration in a verb-named
  file is rejected unless its exact `(file, function)` pair is
  enumerated — and a grant covers its pair only, so a *second* new write
  in the same file is rejected too.
- **Every migration-once entry names its function, and the live site must
  still be in it.** A relocation is caught.
- **Every entry is either verb-named or explicitly enumerated**, and the
  enumeration is closed, reasoned, single-use and non-redundant.

What the guard still does **not** enforce, stated plainly:

- **It cannot verify that a migration is idempotent.** The class's
  operational meaning is "a retry or a rerun is safe", and that is a
  property of the statement's semantics, which no name-based rule can
  check.
- **A user-path function whose OWN NAME carries a vocabulary verb can
  still be misfiled.** `PromoteMemory` would satisfy the primary rule.
  The rule cannot tell a user path from a migration path by name alone.
  What has changed is *where* that judgement lives: it is now made in
  **one enumerated, visible list** with a stated reason, rather than in a
  rule's second arm, and the recorded `Func` means a misfiling has to
  name the live function the guard then checks against the code.
- **A grant is still a human judgement.** Adding one is a reviewable act
  in the guard's own source, and it is visible in a four-entry list — but
  it is not mechanically derived.

So the corrected statement of the direction is: **the rule over-reports
rather than under-reports for every write that is not enumerated** (a
genuine migration whose function name lacks a vocabulary verb must be
enumerated, which is a reviewable act), and **the residual
under-reporting surface is the enumerated list itself** — four entries,
each with a stated reason, each checkable against the code.

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

### 8.1 The five mutations C's review added

C's independent review (§12.2) ran its own mutation battery and found
one the table above missed. **M17 was NOT caught**: the rule's file-path
arm accepted a new one-shot migration in a verb-named file, silently, with
every guard test green. That was finding F1, and it is merge-blocking.

The remediation (§6.1, §6.2) removes the file-path arm, replaces it with
an **enumerated exception list** keyed by exact `(file, function)` pairs,
and adds a permanent negative control for the M17 shape. The battery below
was re-run against the corrected guard in a **disposable worktree**, with
the M17 mutation as its baseline. **All eleven were caught, none missed.**

| # | Mutation | Caught by |
|---|---|---|
| **M17** | **a NEW one-shot migration whose function name carries no vocabulary verb, in a file whose path carries one** | **the migration-once guard — by name** |
| M18 | a **SECOND** unapproved write in the same verb-named file | the migration-once guard (a grant covers its pair only) |
| M19 | a grant that names a pair with **no live entry** (a dead grant) | the migration-once guard's dead-grant check |
| M20 | a **redundant** grant whose function name already carries a verb | the migration-once guard's redundant-grant check |
| M21 | a grant that **states no reason** | the migration-once guard's reason check |
| M1 | revert `UpdateSessionSummary` to a bare pool write | the completeness guard (surplus) |
| M9 | re-add a deleted allowlist entry for a converted shape | the stale-entry guard **and** the negative control |
| M10 | misfile a `migration-once` entry under an ordinary function name | the migration-once guard |
| M11 | drop the `Func` from a `migration-once` entry | the migration-once guard |
| M12 | name a `migration-once` entry after a user-path function | the migration-once guard |
| M13 | relocate a `migration-once` write out of its migration function | the migration-once guard |

M17's red state, reproduced on the reviewed head `9c1eb4f3` **before** the
correction — every guard test green, which is the defect:

```
--- PASS: TestBarePoolWriteAllowlistIsComplete
--- PASS: TestBarePoolAllowlistHasNoStaleEntries
--- PASS: TestBarePoolAllowlistIdentitiesAreUnique
--- PASS: TestBarePoolMultisetIdentitiesDetected
--- PASS: TestBarePoolAllowlistDetectsReintroducedSlice2Writes
--- PASS: TestBarePoolMigrationOnceEntriesNameAMigrationFunction
--- PASS: TestBarePoolAllowlistDetectsReintroducedWrite
--- PASS: TestBarePoolAllowlistDetectsReintroducedExecContextWrite
--- PASS: TestBarePoolAllowlistDetectsUnapprovedAttach
```

and its green state **after** the correction — caught by name, with the
reason stated:

```
--- FAIL: TestBarePoolMigrationOnceEntriesNameAMigrationFunction
    migration-once entries whose classification is not checkable:
      internal/core/migrate_md.go: function "zzOneShotRepair" carries no migration verb,
      and the pair is not in the enumerated exception list (barePoolMigrationExceptions) —
      a migration-once write must either carry a migration verb in its FUNCTION NAME, or be
      enumerated explicitly with its reason. The file path is NOT evidence:
      UPDATE raw_memories SET status = 'repaired' WHERE id = ?
```

The M17 shape is now **asserted in the repository**, permanently, by
`TestBarePoolMigrationOnceRejectsANewWriteInAVerbNamedFile`. Before this
remediation the guard's sensitivity was asserted nowhere, which is how the
hole survived a review.

### 8.2 What the mutation demonstration still is not

**It is a manual exercise, not a test.** The mutations M1–M21 are applied,
the guard is run, and the files are reverted byte-for-byte; nothing in the
suite asserts the guard's own sensitivity to most of them. The **M17
shape** is now the exception — it is encoded as a permanent test — and
that is the one whose absence let the hole through. Encoding the rest is
C's finding F5, which is **out of scope for this remediation** and remains
open (§12.3).

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
- **C's four residual findings** — F2 (the general statement of the
  failure-path latency, and the wake-context exposure), F3 (`UpdateSessionSummary`
  is broken three ways, not one), F4 (the green subtest exercises a schema
  that does not exist in production) and F5 (the mutation demonstration is
  still a manual exercise for every mutation except the M17 shape). They
  are out of scope for this remediation by instruction, and they are
  **documented, not dismissed** (§12.1, §12.3).

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
| `internal/core` suite (`go test -tags fts5 ./...`) | ✅ |
| `internal/core` suite under `-race` | ✅ `ok github.com/flowbyte-com/mpm-core 445.093s` on the slice's head; **433.544 s** on the F1 remediation's head (`ec0af1d0`) |
| `make test-race` (the canonical gate) | ✅ exit 0 — it includes `make test-scripts`, `make test-bench` and `make test-agent-installation` |
| `make test-scripts` | ✅ 889 tests (floor 50) |
| `make test-bench` | ✅ 217 tests (floor 50) |
| `make test-agent-installation` | ✅ 481 tests (floor 300) |
| `make test-release` | ✅ `ok github.com/flowbyte-com/mpm/release_acceptance 12.115s` / `12.358s` (36 tests, floor 20) |
| `make release-gate` (`test-race` + `test-release`) | ✅ **exit 0** on the re-run and on the F1 remediation's head; the first attempt is §11.2 |
| `go run ./cmd/mpm-lint --gate` | ✅ PASS (go, mutex, sql, fd, imports thresholds all at or under their ceilings) |
| `make docs-check` | ✅ 13 checks, 0 review items, 0 errors |
| the CI on this branch | ✅ 3/3 on the F1 remediation's head on the **re-run** (run `42396294587`); the **first** attempt at that same head hit the documented 10-minute per-package cliff and failed — see §11.2.1. The slice's own heads were 3/3 on every attempt. The Go tree is byte-identical across the slice's heads; the F1 remediation's head changes only the guard (a test file) and this record |

### 11.1.1 The F1 remediation's own gates

| Gate | Result |
|---|---|
| the guard suite (`go test -tags fts5 -run 'TestBarePool' .`) | ✅ 14 tests — including the new `TestBarePoolMigrationOnceRejectsANewWriteInAVerbNamedFile` |
| `go vet -tags fts5 .` (`internal/core`) | ✅ exit 0 |
| `make release-gate` on `ec0af1d0` | ✅ **exit 0** — `internal/core` under `-race` at **433.544 s**, `cmd/mpm` at 468.298 s, `internal/core/tools` at 72.060 s, `release_acceptance` at 12.358 s |
| `make docs-check` | ✅ 13 checks, 0 review items, 0 errors |
| `go run ./cmd/mpm-lint --gate` | ✅ PASS |
| the inventory ↔ live-scanner reconciliation | ✅ **exact, both directions** — 133 occurrences / 53 files; 0 live sites without a document row; 0 document rows without a live site; 46 `ordinary` / 49 `migration` / 38 `maintenance` / 0 `tx-owned` |

### 11.2 One pre-existing failure, and one transient timeout

**The build-ordering artefact.**
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

**The transient timeout.** The FIRST `make release-gate` attempt on this
tree failed — and it is worth reporting rather than hiding, because the
failure mode is a timeout, not an assertion:

```
panic: test timed out after 10m0s
	running tests:
		TestWakeContext_PreviousSession_CompatibilityMatrix (1s)
FAIL	github.com/flowbyte-com/mpm-core	600.051s
```

The Makefile's `test-race` recipe runs the `internal/core` module with
**no `-timeout`**, so it inherits Go's **10-minute per-package** default.
The suite's -race duration on this machine is ~7m20s, which leaves about
2m40s of headroom — and a loaded machine can eat it.

The tree is not the cause, and the evidence is a controlled measurement
rather than an assertion:

| Tree | `internal/core` under `-race` |
|---|---|
| the **unmodified** base `056ce449` | `ok … 437.974s` |
| **this tree** | `ok … 445.093s` |

a **+7.1 s (+1.6 %)** difference, against a run-to-run spread on this
tree alone of 441.014 s → 445.093 s. The change adds a ~2.7 s test and
eight conversions that add a counter increment; it does not move the
suite. The first `make test-race` attempt on this exact tree also passed
(end to end, exit 0), and the CI's *Full test suite* job — which runs with
`-timeout 20m` — is green on the PR head.

The re-run of `make release-gate` on a quiet machine is **green end to
end, exit 0** (§11.1), with the `internal/core` package at 438.373 s.

**What a reader should take from this.** The `internal/core` -race suite
sits at roughly 73 % of Go's default per-package budget on a 12-core
machine. That margin is a property of the Makefile, not of this slice, but
it is thin enough that a loaded runner can turn it red — the same class of
thing as the `internal/update` flake recorded in `docs/dev/session-b-update-manager.md`.
Adding `-timeout 20m` to the `test-race` recipe for the `internal/core`
module would remove the cliff. That is a Makefile change, outside this
slice's scope, and it is left as a recommendation rather than smuggled in.

### 11.2.1 A third failure — one I caused, and the fix

During the F1 remediation, a `make test-race` run failed:

```
--- FAIL: TestBuildProvenance_TheCanonicalScriptStampIsEmbeddedInTheBinary
    the canonically stamped binary does not report this checkout's HEAD commit
    "ec0af1d05b437c67f2784525b976278e45356c2a":
      MPM 0.1.0-dev+g9c1eb4f3.dirty.t1794069844
        commit:  9c1eb4f38cfd89e7798d59a68735cba5b5c77024
```

**This was my own doing, and the failure is precise about it.** The
`test-race` recipe builds first, stamping the binary with the HEAD at
build time (`9c1eb4f3`); I then **committed while the run was in flight**,
moving HEAD to `ec0af1d0`; and `cmd/mpm`'s provenance test later compared
the already-built binary against the **current** HEAD and found them
different. The test is an invariant — *the binary must report this
checkout's HEAD* — and it was correct to fail.

The fix is procedural, not a code change: **the tree must be stable for
the duration of a gate run.** The run was repeated with no commits during
it, and it passed end to end (§11.1.1).

It is recorded here because the record's §11.2 exists to report failures
rather than hide them, and this one is mine.

### 11.2.2 The cliff, reached on the CI runner too

**The first CI attempt on the F1 remediation's head `f0b4ff77` FAILED**,
and the failure is the same timeout as §11.2 — not an assertion:

```
panic: test timed out after 10m0s
	running tests:
		TestPurge_ForcedDeletesTheFullSet (0s)
FAIL	github.com/flowbyte-com/mpm-core	600.076s
```

**The same commit was then re-run, on the same runner pool, and it
passed** — run `42396294587`, 3/3 green, with the `internal/core` -race
package at **394.934 s**.

| The same commit `f0b4ff77`, on the CI | `internal/core` under `-race` |
|---|---|
| the first attempt | **600.076 s** — the cap, hit |
| the re-run | **394.934 s** — 66 % of the cap |

That is a **>50 % run-to-run spread on identical code**, so the cliff is
the runner's load and the Makefile's missing `-timeout`, not the change.
The test that was running when the alarm fired — `TestPurge_ForcedDeletesTheFullSet`
— is a work-purge test that touches none of this slice's code.

The local measurement on the same head puts `internal/core` at **451.913 s**
(§11.1.1), i.e. **75 % of the cap on a 12-core machine**, while the CI
runner is materially slower and noisier.

**The recommendation in §11.2 is therefore not advisory any more — it is
the fix for a cliff that has now been reached on the CI runner itself.**
The `test-race` recipe already sets `-timeout $(CMD_TEST_TIMEOUT)` (20 m
by default) for `./cmd/...`; the `internal/core` step omits it and
inherits Go's 10-minute per-package default:

```make
cd internal/core && … $(GO) test -race -tags fts5 -v ./...
```

Adding `-timeout $(CMD_TEST_TIMEOUT)` to that one line would remove the
cliff. It is a **Makefile change**, it is **outside this slice's scope**,
and it is deliberately **not** made here — this remediation is narrow, and
the record recommends the change rather than smuggling it in. It is
escalated as MPM work.

---

## 12. The review

**This work HAS now been independently reviewed.** C reviewed the branch
independently, verified the eight conversions and the red-to-green
reproduction against the base rather than taking them on trust, and
returned **REQUEST CHANGES — narrowly, on F1**.

C's verdict on the substance:

> "the **eight conversions are correct** — the defect reproduces with the
> real production method at the base (the write is lost, no retry
> absorbed) and is fixed at the head (a retry is absorbed, the write
> lands); the **red state reproduces** on the PR's own test; **retry
> safety holds** for all eight; the **transaction semantics hold** for
> all eight; and the **inventory is exact** in both directions, with the
> arithmetic reconciling against the base's own previous revision."

### 12.1 C's findings, and what happened to each

| # | Severity | Finding | Disposition |
|---|---|---|---|
| **F1** | **MEDIUM — blocking** | The `migration-once` guard **under-reported** in the direction §6.2 said was impossible, because the **file-path half** of the disjunction was unchecked. C's mutation **M17** — a new one-shot migration with a non-vocabulary function name in a verb-named file — was accepted **silently**, every guard test green. §6.1's "they satisfy it for the right reason" was false for **4 of 20** entries, two of which carry the verbs the same section says the vocabulary excludes (`mark`, `promote`). | **REMEDIATED in this revision** — §6.1, §6.2, §8.1. The file-path arm is **removed**; the four are reclassified by their actual execution semantics and hold **enumerated exceptions**; the M17 shape is a **permanent negative control**. |
| **F2** | MEDIUM | The new budget makes a **failed** write cost ≈33 s at the production busy window (measured 5.015 s → 33.151 s), and failure paths that also write an audit row cost **two** budgets (66.3 s measured). The record stated this for `DeleteHandoff` only; the same shape is on the **wake-context path** (`wake_context.go:652` → `MarkHandoffRead` → `LogAudit`), measured 38.154 s → 66.275 s — an agent's session-start call. No latency contract is violated, but there is **no cancellation**. | **OPEN — documentation** (§12.3). Out of scope for this remediation by instruction; the general shape is not yet stated in the record. |
| **F3** | LOW | `UpdateSessionSummary` is dead **and** broken three ways, not one: no `summary` column; **not an upsert** (an `UPDATE` — a call for an unknown `session_id` returns `nil` and writes nothing); and `session_id` is **not unique** (one call updated two rows). | **OPEN — follow-up.** The filed follow-up should be *"fix or remove the function"*, not *"add a column"*. Recorded as MPM work `abf57027de5654de`; to be updated. |
| **F4** | LOW | The green `UpdateSessionSummary` case exercises a schema that does not exist in production (the harness adds the column). "end to end" overstates it. | **OPEN — wording** (§12.3). One sentence fixes it; not done in this remediation by instruction (F2–F5 out of scope). |
| **F5** | INFO | The mutation demonstration is a **one-time manual exercise**, not a test. Nothing in the suite asserts the guard's own sensitivity, which is how F1's class went unnoticed. | **PARTLY ADDRESSED.** The **M17 shape specifically** is now a permanent test (§8.1) — it is the one whose absence let the hole through. Encoding the remaining mutations as tests remains open. |
| — | INFO | PR #234 and PR #232 merge cleanly — zero conflicts — and the merged tree passes the relevant `internal/core` suites. The file sets do not overlap. | Noted; unchanged. |

### 12.2 The F1 remediation, in detail

**The defect.** The rule shipped as: a `migration-once` entry is acceptable
if the function name **or the file path** carries a migration verb. The
file-path arm granted migration status to **every** write in a verb-named
file. The class is a claim about the **write**; a path is a property of
the **file**. So the arm was an unchecked escape hatch, and it was the
escape hatch through which a *new* ordinary write could acquire migration
status without anybody noticing.

**The fix, in four parts.**

1. **The file path is not evidence.** The rule is now
   `classifyMigrationOnce`: the **function name** must carry a migration
   verb, **or** the exact `(file, function)` pair must be enumerated.
   `hasMigrationVerb(e.File)` appears nowhere in the rule.

2. **The four file-path-dependent entries were reviewed by their actual
   execution semantics** and reclassified in §6.1's table — when each
   runs, what gates it, what a rerun does. All four are genuine
   migrations; none of them can be identified by the mechanical rule, so
   all four hold an **enumerated exception** with a stated reason.

3. **The exception list is closed, enumerated and single-use.** It is a
   map of exact `(file, function)` pairs to reasons — not a pattern. The
   guard asserts every grant states a reason, is used by exactly one live
   entry, and is **not redundant** (an entry whose function name already
   carries a vocabulary verb must not also hold a grant).

4. **The M17 shape is a permanent negative control.**
   `TestBarePoolMigrationOnceRejectsANewWriteInAVerbNamedFile` asserts
   the M17 entry is rejected, that the same shape is rejected in each of
   the other verb-named files, that a verb-free file is still rejected,
   that an entry naming no function is rejected — **and** that the four
   genuine exception-holders and three verb-named migrations are still
   accepted. The correction bounds the class; it does not ban it.

**The production diff needs no change.** `internal/core/db.go` and
`internal/core/handoff.go` are **byte-for-byte identical** to the head C
reviewed — `git diff 9c1eb4f3 HEAD -- internal/core/db.go internal/core/handoff.go`
is **empty**. The eight approved conversions are untouched, and no retry
budget or transaction behaviour was modified.

**What F1 did NOT find.** C's verdict was explicitly that the eight
conversions "can merge as they stand", and that "the **production diff**
(`db.go`, `handoff.go`) needs no change". The remediation is confined to
the guard and to this record.

### 12.3 What is still open after this remediation

- **F2** — the general statement of the failure-path latency, and the
  wake-context exposure (`internal/core/wake_context.go:652`), is not yet
  in the record. §5.2.1 states the shape for `DeleteHandoff` only.
- **F3** — the filed follow-up for `UpdateSessionSummary` says "add a
  column"; C's measurement shows the function is not an upsert and
  `session_id` is not unique, so the correct resolution is "fix or remove
  the function". The follow-up needs updating.
- **F4** — §4.3's phrase "so the production call can be exercised end to
  end" overstates what the green subtest proves.
- **F5** — the mutation demonstration remains a manual exercise for every
  mutation except the M17 shape.
- **The residual judgement surface of the guard itself** — a user-path
  function whose own name carries a vocabulary verb (`PromoteMemory`) can
  still be misfiled, and the guard cannot verify that a migration is
  idempotent. §6.2 states both.

### 12.4 The substrate's own review, and what it cannot be

The substrate's `mpm_request_review` was run against the **production
diff itself**, with a prompt that asked for verdicts on nine specific
questions rather than for a summary. The first run — against the
production diff alone — **declined to give verdicts**, on the ground that
the artifact did not establish the retry/ID behaviour, the guard
implementation, the live-caller inventory or the five-live/three-uncalled
split. That was a correct refusal: the artifact was incomplete. The full
evidence was then assembled (production diff + guard diff + the per-site
analysis + the caller inventory + the reproduction + the mutation
demonstration) and the review was re-run against it.

Its verdicts:

| Question | Verdict |
|---|---|
| 1. Double-apply — can a retry apply any of the eight twice? | **PASS** for all eight; no argument contradicted by the statements |
| 2. Row-count truth | **PASS** — a busy attempt removes no row, so the count that comes back is the count actually removed |
| 3. Error surface | **CONCERN** — the `LogAudit`/`ExecTracked` interaction on the failure path was not settled by the artifact |
| 4. Transaction semantics | **PASS** — the analysis holds |
| 5. Connection ownership | **PASS** — same pool; and no converted statement depends on connection-scoped state (unlike the `ATTACH` case) |
| 6. The missing column | **CONCERN** — defensible as remediation, but the slice cannot claim an end-to-end production benefit for that site |
| 7. The classification fix | **CONCERN** — materially more falsifiable, but bypasses remain |
| 8. Scope honesty | **PASS** — converting the uncalled public surface is reasonable, and the slice does not misrepresent the impact |
| 9. What is missing | **CONCERN** — the missing column, and the unsettled `LogAudit` interaction |

**Overall: CONCERN** — "the eight conversions and retry safety are sound,
but the missing sessions.summary schema change remains and the LogAudit
retry interaction and classification bypasses are not fully resolved by the
artifacts."

### 12.5 What was done with the substrate review's concerns

**Concern 3 — the `LogAudit` interaction — is settled, by measurement.**
§5.2.1: `LogAudit` was **already** budgeted at the base commit and
`internal/core/audit.go` is not touched by this slice; `LogAudit` returns
nothing and cannot mask the caller's error (both trees return exactly
`DeleteHandoff: database is locked`); and the cost is measured — the
failure path goes from 3.811 s to 7.417 s under the harness's 100 ms busy
window, which is the price of the budget and buys a shred that lands.

**Concerns 6 and 9 — the missing column — were already disclosed** (§4.3,
§10), are filed as their own follow-up (MPM work `abf57027de5654de`), and
the review agrees the framing is honest rather than a way of making the
numbers look good.

**Concern 7 — the classification bypasses — were already stated in §6.2,
and the false claim in it has since been corrected.** The substrate
review's assessment was: "useful and over-reporting is safer than silent
under-reporting, but it does not completely close the classification
hole." **C's independent review then showed that the "never
under-reported" claim was false**, and this revision removes the arm that
under-reported (§12.2). What remains is stated in §6.2: the guard cannot
verify idempotence, and a user-path function whose own name carries a
vocabulary verb can still be misfiled — a judgement that now lives in one
enumerated, visible list rather than in a rule's second arm.

### 12.6 What the substrate's review is NOT

The substrate's `mpm_request_review` is a **self-review**: the work was
checked against the tree, against the mutation demonstration, against the
base commit's behaviour and by the substrate's own review components — all
by the agent that wrote it.

**C's review is the independent one, and it is the one that counted.** It
found a defect the self-review missed, and this revision is its
remediation. The re-review C is asked for is therefore a review of the
**remediation**, not of the whole slice:

1. **The rule change** — is "the function name, or an enumerated
   exception" the right rule, and is the file path correctly excluded from
   it? (§6.1, §12.2)
2. **The four classifications** — does the table in §6.1 classify each of
   the four by its actual execution semantics, and is each exception's
   stated reason accurate? (§6.1)
3. **The exception mechanism's narrowness** — is a closed list of exact
   `(file, function)` pairs with reasons, single-use and non-redundant,
   narrow enough to close the hole, or does it reintroduce one?
   (§6.1, §8.1)
4. **The four residual findings** — F2, F3, F4 and F5, which this
   remediation deliberately does **not** address (§12.3). Are any of them
   actually merge-blocking?
5. **The unchanged production diff** — `internal/core/db.go` and
   `internal/core/handoff.go` are byte-for-byte identical to the head C
   reviewed, so the eight conversions need no re-review beyond
   confirmation that they are untouched.
