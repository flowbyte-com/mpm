# Issue #178 — the bare-write audit, and the first remediation slice

**Date:** 2026-11-06
**Base:** `main` @ `653e4633`
**Branch:** `feat/issue-178-bare-write-slice1`
**Issue:** #178 — *"90 bare pool writes still lose their artifact to ordinary
lock contention — the same defect class as #164, in a different call shape"*

The audit was first measured on `3a3486cf` (the green `main` when the work
began; `653e4633` was then still `in_progress`). It was **re-measured on
`653e4633` after that landed**, and every number in this record is the
re-measured one: the ratchet inventory is **161** occurrences / 53 files / 148
identities on both trees, the type-resolved census is 155 / 50 on both, and
the reconciliation of §1.3 is identical on both. `3a3486cf` and
`653e4633` touch disjoint files, and the class is unchanged. (The inventory
document's own line numbers are the ones measured at `653e4633`; an earlier
revision of it carried `3a3486cf`'s line numbers under a header that named
the later base.)

**Issue #178 is not closed by this work.** The scheduler's duplicate-delivery
crash window remains open by design (see
[`issue-178-corrections-phase-3-2026-11-03.md`](issue-178-corrections-phase-3-2026-11-03.md)
§5). What this record does is: re-measure the class on the current tree with
an instrument that resolves types, publish the full inventory and its
classification, and land the **first** remediation slice — twenty-three
writes on the memory write path, every one of them an ordinary contended
write whose loss discards user or agent state — together with the permanent
guards that keep the slice honest.

---

## 1. The audit

### 1.1 Why the issue's numbers could not be used

The issue's headline is "90 bare pool writes across 24 files", with a per-file
table of line numbers. Measured against `main` @ `653e4633`, that figure is
**stale in both directions**:

- the line numbers no longer point at the statements they named (the tree
  moved), and
- the count does not match either scanner: the phase-0 audit scanner reported
  127, the merged ratchet recorded 124 occurrences over 122 identities, and
  the issue says 90.

The phase-3 record
([`issue-178-corrections-phase-3-2026-11-03.md`](issue-178-corrections-phase-3-2026-11-03.md))
already established that the numbers in circulation are **lower bounds**, and
the phase-3b record established that both scanners were blind to the
`ExecContext` shape. Both findings still hold. So the audit was redone from
scratch rather than reconciled against the issue's table.

### 1.2 The instrument

The audit instrument is a **type-resolving AST scanner**, written for this
work and deliberately different from the merged ratchet:

| | merged ratchet (`internal/core/bare_pool_write_ratchet_test.go`) | this audit's scanner |
|---|---|---|
| receiver classification | **syntactic**: `db`/`DB` selectors, `.SQLDB()` calls, and every identifier that is not literally named `tx` | **type-resolving**: a declared type, a derived type (`.Begin()` → tx, `.Conn()` → conn, `sql.Open()` → pool), a struct field's declared type (following an embedded `*sql.DB`), or — for an interface-typed parameter — the **concrete types passed at every call site** of the enclosing function |
| direction of error | fail-closed: it may **over**-count, never under-count | fail-closed **per call site**: a shared primitive called with a pool anywhere counts as a pool write |
| SQL resolution | `Args[0]` for `Exec`, `Args[1]` for `ExecContext`, resolved through a literal scope | the same, plus identifier bindings resolved through the local scope |
| write verbs | DML + DDL + `PRAGMA` + `VACUUM` + `ATTACH`/`DETACH`/`REINDEX` | DML + DDL + `PRAGMA` + `VACUUM` |

The scanner's receiver census over the whole tree, on the base commit:

```
pool 172   tx 213   conn 9   stmt 5   interface 0   other 0     TOTAL 399
```

### 1.3 The reconciliation — and the blind spot it closed

The two scanners are not supposed to agree; the ratchet over-counts by
design. Reconciling them **identity by identity** (file + normalized SQL, as
a multiset) gives an exact answer.

The ratchet is measured with the **verb list the guard now uses** — DML, DDL,
`PRAGMA`, `VACUUM`, `ATTACH`, `DETACH` and `REINDEX`. The audit's
type-resolving scanner was measured with DML, DDL, `PRAGMA` and `VACUUM`;
the three verbs added since are **connection-scoped verbs**, and the one
site they expose (`similarity_candidates_store.go:972`) is a `*sql.Conn`
receiver, which the type-resolving scanner classifies as a **connection**,
not a pool. So the scanner's count is unaffected by the extension, and the
comparison is still like-for-like:

| base commit (`653e4633`) | occurrences | files | identities |
|---|---|---|---|
| merged ratchet, syntactic, fail-closed | 161 | 53 | 148 |
| this audit's scanner, type-resolved | 155 | 50 | 142 |
| **ratchet only** (the scanner proved them not-pool) | 6 | | |
| **scanner only** (the ratchet missed them) | **0** | | |

**The 6 ratchet-only sites** are the ratchet's documented over-count, and
all six are the same shape: the receiver is a pinned `*sql.Conn`, not a
pool. Four are the `PRAGMA foreign_keys = OFF/ON` pair inside the two
table-rebuild migrations (`migration_memories_affinity_rebuild.go`,
`migration_memories_created_at_not_null.go`), one is the
`INSERT OR IGNORE INTO shared.contradiction_log` in
`similarity_candidates_store.go`, and one is the
`ATTACH DATABASE '%s' AS shared` in the same file. The ratchet is right to
count them — it fails closed — and wrong about their *kind*.

**The scanner misses nothing the ratchet sees.** That is the property that
makes the ratchet usable as the permanent guard: the audit instrument can
disagree with it, but only by counting more.

**What this table is not: reproducible from this PR.** The type-resolving
scanner is not in the tree — it was a throwaway analysis tool (see §11), and
the version that exists on the unmerged audit branch still has the three
blind spots §1.4 describes as fixed. A reader can re-derive the **ratchet's**
half of this table from the tree (§11 gives the command); the **scanner's**
half they cannot. The substantive conclusion — that the scanner sees nothing
the ratchet misses — was independently confirmed by a third scanner written
from scratch by the reviewer of 2026-11-06 (§12.3).

**The gap this exposed.** Measured with the ratchet's *pre-extension* verb
list (DML and DDL only), the same reconciliation gave 133 occurrences for
the ratchet against 129 for the scanner, and the **8 scanner-only sites**
were all one thing:

| Site | Statement |
|---|---|
| `cmd/mpm/handlers_backup.go:698` | `PRAGMA wal_checkpoint(TRUNCATE)` |
| `cmd/mpm/maint_cmds.go:385` | `PRAGMA incremental_vacuum` |
| `internal/core/db.go:663` | `PRAGMA wal_checkpoint(TRUNCATE)` |
| `internal/core/db.go:2922` | `PRAGMA journal_mode = WAL` |
| `internal/core/db.go:2923` | `PRAGMA busy_timeout = 5000` |
| `internal/core/db.go:2924` | `PRAGMA synchronous = NORMAL` |
| `internal/core/db.go:248` | `VACUUM` |
| `internal/core/embedding_migration.go:380` | `VACUUM INTO ?` |

`bareWriteVerb` stopped at the DML and DDL verbs. A bare pool `PRAGMA` or
`VACUUM` was therefore **invisible to the guard**: a detector that cannot see
a write shape cannot keep that shape honest. §5 closes that gap.

**A second gap, found by the independent review of 2026-11-06.** The verb
list had a second stopping point, and this one was not merely a gap — it was
a **false claim**. The list's comment said that `REINDEX`, `ATTACH` and
`DETACH` were deliberately not listed because "the tree contains no bare pool
write of any of them". The tree contains exactly one, and the review
reproduced the contradiction in one line of diff. §5.1 records the
correction; the review's finding is §12.3's M1.

### 1.4 Blind spots in this work's own instruments, found and fixed

All three were found by the reconciliation above and are recorded here
because a scanner with silent blind spots is exactly the failure mode this
issue is about. Each one made the scanner **under**-count:

1. **`ExecContext` read the SQL from the wrong argument.** `ExecContext(ctx,
   query, ...)` puts the query second. Fixed; the count moved 118 → 129.
2. **A shared primitive's executor resolved to "ambiguous", not "pool".**
   `weakenMemoryOn(ex memExecer, …)` is called with a pool on one call site
   and a transaction on another. Classifying it as ambiguous **under**-counts
   the pool path — the path that loses the write. The resolution is now
   per-call-site and fail-closed: if any call site passes a pool, the site
   counts as a pool write. This is what surfaced the two shared-primitive
   sites of §4.2, which the first version of the scanner missed entirely.
3. **A factory call was read as SQL.** `db, err := sql.Open("sqlite3", …)`
   was being read as the SQL string `"sqlite3"` — a call's first argument is
   not the call's SQL — which hid the pool binding, and with it the write
   behind it. The type binding now comes first, and a local bound to a
   same-package factory's result takes that factory's declared result type
   (`src, err := openSQLiteRO(path)` → `src` is a `*sql.DB`).

A **fourth** blind spot was found later, and by a different instrument: the
slice's own classification step looked only at the shared primitives the
audit had already surfaced, and missed `createLessonsViewAndTriggers`. It
was the new guard's **reach check** — which discovers every function taking
an `Exec`-bearing interface parameter — that made it visible. That is §4.3,
and it is the argument for the reach check existing at all: a guard that
checks only what its author already knew about is a guard that cannot fail
on the thing the author did not know about.

## 2. The reclassification

Every bare pool write on the base commit falls into one of four classes. The
question each class answers is: **what happens to the write when the
contention outlasts the busy timeout?**

| Class | Count (base) | What the loss means |
|---|---|---|
| **ordinary contended write** | 71 | The user's or the agent's state is not persisted. The write is lost; the caller is told, so the loss is recoverable only by redoing the work. |
| **startup migration / DDL** | 52 | Schema, a one-shot backfill, a seed, or a test-fixture write. Idempotent or detection-gated, so a rerun is safe. |
| **transaction-owned** | 0 | A write inside a transaction is owned by the transaction layer (`WithTx` restarts a stale snapshot; `txNode.ExecTracked` owns the statement budget). **No bare pool write on the base commit is lexically inside a `WithTx` callback.** |
| **intentionally nonblocking maintenance** | 38 | A ledger, a counter, a retention sweep, a recomputation, or a diagnostic row. Deliberately best-effort: the failure is logged and costs a re-run, not a state. |

**Total: 161 occurrences across 53 files** — the base commit measured with
the same verb list the guard now uses (`PRAGMA`, `VACUUM` and the
connection-scoped `ATTACH`/`DETACH`/`REINDEX` included). With the ratchet's
pre-extension list the total is 133; the 28-site difference is the detector
gap of §1.3: the 27 `PRAGMA`/`VACUUM` sites, plus the one connection-scoped
`ATTACH` that the review of 2026-11-06 surfaced.

### 2.1 The mapping, mechanically

The classification is a judgement about the **consequence** of the loss, and
it is recorded per site in
[`bare-write-inventory-2026-11-07.md`](bare-write-inventory-2026-11-07.md). It
is anchored to the merged ratchet's own reason vocabulary, so the two
cannot drift apart:

| Ratchet reason | Occurrences (base) | Class |
|---|---|---|
| `propagated` | 101 | 71 `ordinary` + 30 `maintenance` |
| `startup-ddl` | 27 | `migration` |
| `migration-once` | 23 | `migration` |
| `best-effort` | 8 | `maintenance` |
| `test-fixture` | 1 | `migration` |
| `connection-scoped` | 1 | `migration` |

The `propagated` reason answers a different question from the class: it says
the error reaches the caller, not what the loss costs. So it splits. A GC
ledger row and a memory's content both propagate their error; losing the
ledger costs a re-run, and losing the content costs the content.

`connection-scoped` is the newest reason, and it is the one whose CLASS
needs the most explanation. Its single site is the
`ATTACH DATABASE '%s' AS shared` at `similarity_candidates_store.go:972`.
It is `migration` because it is **schema**, it is **detection-gated** (a
`PRAGMA database_list` check on the same connection precedes it), and a
rerun on that connection is **safe** — which is exactly what the `migration`
class means. It is not `ordinary`, because no agent state is at stake; and
it is not `maintenance`, because its failure is fatal to the operation
rather than deliberately nonblocking. §5.1 says why a statement budget
would be *wrong* for it rather than merely redundant.

The 38 `maintenance` sites are, in full: the GC and compaction ledgers
(`last_gc_at`, `compaction.last_run`), the retrieval and cluster counters,
the settling baselines, the reference-chunk embedding refreshes (the chunks
are re-embeddable), the derived cluster assignments, the work projection
recomputation, the background weight and runtime decay passes, the audit
ledger and the best-effort forensic row, the cascade-summary wake (a report —
the next tick produces another), the wake claim (a failure rolls the wake
back rather than stranding it), the drill rehearsal status, the WAL
checkpoints, the `VACUUM`s, and the nine retention sweeps.

### 2.2 The remaining inventory

The full inventory — every bare pool write on the base commit, with its file,
line, reason and statement — is in
[`bare-write-inventory-2026-11-07.md`](bare-write-inventory-2026-11-07.md).

---

## 3. The slice: what was chosen, and what was not

### 3.1 The criterion

The slice is: **ordinary contended writes on the memory write path whose loss
discards user or agent state** — the content of a memory, its lifecycle flags,
or its importance/reinforcement state.

The memory write path was chosen because it is where an agent's durable work
actually lives: every `mpm_memory save`, every edit, every promote, every
soft-delete, every reinforcement. A lost write there is a lost memory or a
lost decision about a memory.

### 3.2 The twenty-three writes

**Twenty-three writes now carry the substrate's budget of 5**: twenty call
sites converted directly, the `MemoryStore.execTracked` **fallback** (§4.1),
and two shared-primitive pool paths (§4.2). The type-resolved pool-write
census falls by **22** (155 → 133), because the fallback was never in it —
its SQL is a parameter, so no scanner could classify it as a write at all.

| # | File | Function | Statement |

| # | File | Function | Statement |
|---|---|---|---|
| 1 | `internal/core/memory.go` | `addMemoryDirect` | `INSERT INTO memories (…)` — the content of the memory itself |
| 2 | `internal/core/memory.go` | `DeleteMemory` | `UPDATE memories SET deleted_at = …` — the soft delete |
| 3 | `internal/core/memory.go` | `UpdateMemory` | `UPDATE memories SET content = ?, tags = ?, metadata = ?, embedding = ?, …` |
| 4 | `internal/core/memory.go` | `UpdateMemory` | `UPDATE memories SET content = ?, tags = ?, metadata = ?, content_hash = ?, …` |
| 5 | `internal/core/memory.go` | `ReinforceMemory` | `UPDATE memories SET reinforcement_count = … , weight = …` |
| 6 | `internal/core/memory.go` | `WeakenMemory` | `UPDATE memories SET reinforcement_count = MAX(…) , weight = MAX(…)` |
| 7 | `internal/core/memory.go` | `SetMemoryTTL` | `UPDATE memories SET expires_at = NULL WHERE id = ?` |
| 8 | `internal/core/memory.go` | `SetMemoryTTL` | `UPDATE memories SET expires_at = ? WHERE id = ?` |
| 9 | `internal/core/memory.go` | `MemoryStore.execTracked` | the **fallback** `s.DB.Exec(query, args...)` — see §4.1 |
| 10 | `internal/core/memory_tools.go` | `SoftDeleteMemory` | `UPDATE memories SET deleted_at = … WHERE id = ? AND deleted_at IS NULL` |
| 11 | `internal/core/memory_tools.go` | `RestoreMemory` | `UPDATE memories SET deleted_at = NULL WHERE id = ? AND deleted_at IS NOT NULL` |
| 12 | `internal/core/memory_tools.go` | `SnoozeMemory` | `UPDATE memories SET weight = MIN(weight + 1, 9), last_accessed_at = …` |
| 13 | `internal/core/memory_tools.go` | `SetMemoryWeight` | `UPDATE memories SET weight = ? WHERE id = ? AND deleted_at IS NULL` |
| 14 | `internal/core/memory_tools.go` | `PromoteMemory` | `UPDATE memories SET weight = 10, is_long_term = 1 WHERE id = ? AND …` |
| 15 | `internal/core/memory_tools.go` | `WeakenMemoryTool` | `UPDATE memories SET reinforcement_count = MAX(…) , weight = MAX(…) , …` |
| 16 | `internal/core/web_db.go` | `UpdateMemory` | `UPDATE memories SET content = ?, tags = ?, metadata = ?, embedding = ?, …` |
| 17 | `internal/core/web_db.go` | `UpdateMemory` | `UPDATE memories SET content = ?, tags = ?, metadata = ?, content_hash = ?, …` |
| 18 | `internal/core/web_db.go` | `ReinforceMemory` | `UPDATE memories SET reinforcement_count = … , weight = … , last_accessed_at = …` |
| 19 | `internal/core/web_db.go` | `AdjustMemoryWeight` | `UPDATE memories SET weight = MAX(weight + ?, 1), last_accessed_at = …` |
| 20 | `internal/core/web_db.go` | `SetMemoryTTL` | `UPDATE memories SET expires_at = NULL WHERE id = ?` |
| 21 | `internal/core/web_db.go` | `SetMemoryTTL` | `UPDATE memories SET expires_at = ? WHERE id = ?` |
| 22 | `internal/core/web_db.go` | `WeakenMemory` | the **shared-primitive pool path** (`weakenMemoryOn`) — see §4.2 |
| 23 | `internal/core/review.go` | `RecordMemoryReview` | the **shared-primitive pool path** (`recordMemoryReviewOn`) — see §4.2 |

Every one of them now carries the substrate's established budget of **5**:

```go
// before
_, err := dm.db.Exec(`UPDATE memories SET weight = ? WHERE id = ? AND deleted_at IS NULL`, weight, memoryID)
// after
res, err := dm.ExecTracked(`UPDATE memories SET weight = ? WHERE id = ? AND deleted_at IS NULL`, 5, weight, memoryID)
```

and, on the `MemoryStore` side, the store's own tracked route:

```go
// before
_, err = s.DB.Exec(`INSERT INTO memories (…) VALUES (…)`, …)
// after
_, err = s.execTracked(`INSERT INTO memories (…) VALUES (…)`, 5, …)
```

### 3.3 What was deliberately NOT converted

The requirement was explicit: do not mechanically convert migration code, and
do not re-enter the transaction layers. The exemptions, with their reasons:

| Site | Class | Why it is exempt |
|---|---|---|
| `internal/core/memory.go:286` `addColumnIfNotExists` | startup DDL | `ALTER TABLE … ADD COLUMN`. Schema work, once per boot; a retry is not the right remedy (a multi-statement DDL wants a transaction). |
| `internal/core/memory.go:3346` `BackfillContentHash` | migration-once | A one-shot backfill, detection-gated on `content_hash IS NULL`. |
| `internal/core/memory.go:3368` `BackfillLTMFlags` | migration-once | A one-shot backfill, detection-gated on `is_long_term = 0`. |
| `internal/core/memory.go:3512` `PruneExpired` | best-effort maintenance | A retention sweep. The row it soft-deletes is expired; if the sweep is lost, the next sweep redoes it. |
| `internal/core/memory.go:2632` `PromoteTopicToMemory` | ordinary, but not in the slice | It writes the **`topics`** table, not memories. Its loss duplicates a topic-derived memory rather than discarding state, and the operation is non-atomic by design — the memory is inserted *before* the topic is deactivated — so converting this single UPDATE would not make it atomic. Deferred, and recorded here so the deferral is a decision rather than an oversight. |
| `internal/core/memory.go:2822` `DeleteAllByCollection` | ordinary, but unreachable | **No production caller.** The only reference in the tree is a comment in `cmd/mpm/handlers_shred.go`. Converting an unreachable API is churn, not remediation. |
| `internal/core/memory.go:2849` `DeleteAllMemories` | ordinary, but unreachable | As above. |
| `internal/core/memory_tools.go:677` `ShredMemoryWithCascade` | ordinary, but destructive-path | `DELETE FROM lessons WHERE id = ?`. A lost shred leaves the lesson in place — recoverable, and the shred path has its own audited contract (`ShredMemory`, `logShredTombstone`). Deferred to a slice that can treat the whole shred cascade together, rather than converted piecemeal here. |
| `internal/core/web_db.go:797` `PruneExpired` | best-effort maintenance | A hard-delete retention sweep. |
| `internal/core/web_db.go:1782` `PruneOlderThan` | best-effort maintenance | A hard-delete retention sweep. |
| `internal/core/web_db.go:1799` `PruneNeverAccessed` | best-effort maintenance | A hard-delete retention sweep. |

**None of the twenty-three converted writes is inside a `WithTx` callback, and
none of them is reached from inside one.** That was checked with a separate
AST pass over every call site of each converted function: no call site of
`SetMemoryWeight`, `SoftDeleteMemory`, `RestoreMemory`, `SnoozeMemory`,
`PromoteMemory`, `WeakenMemoryTool`, `UpdateMemory`, `ReinforceMemory`,
`AdjustMemoryWeight`, `SetMemoryTTL`, `WeakenMemory` or `RecordMemoryReview`
appears lexically inside a `WithTx` callback body. Converting them therefore
does not re-enter the transaction layer.

### 3.4 Behaviour preservation

- `dm.db.Exec(q, args…)` and `dm.ExecTracked(q, 5, args…)` have the same
  signature, `(sql.Result, error)`, and the same error value: `ExecTracked`
  returns the driver's error unwrapped. The `isBusyError(err)` branches that
  already existed (`web_db.go` `ReinforceMemory`, `WeakenMemoryTool`) still
  see the same classification.
- `s.DB.Exec(q, args…)` and `s.execTracked(q, 5, args…)` likewise.
- `dm.ExecTracked` adds one thing the bare call did not do: it records the
  outcome in the watchdog (`dm.logSQLOutcome`). That is instrumentation the
  manager's own tracked path has always had.
- The write goes to the **same pool** it went to before. `s.execTracked`
  routes to `s.DM.db`, and every production `MemoryStore` is built with
  `DB: &SQLiteConnection{DB: dm.db}` and `DM: dm` — the same pool. The
  `poolMemExecer` adapter carries the pool it was given.

---

## 4. The two shapes that needed more than a substitution

### 4.1 The `MemoryStore` fallback

`MemoryStore.execTracked` is the store's tracked route, and its fallback was
a bare write:

```go
func (s *MemoryStore) execTracked(query string, retries int, args ...interface{}) (sql.Result, error) {
	if s.DM != nil {
		return s.DM.ExecTracked(query, retries, args...)
	}
	return s.DB.Exec(query, args...)   // ← the budget was a property of the MANAGER, not of the statement
}
```

A store built without a `DatabaseManager` therefore **silently lost the
caller's budget**. The slice routes eight sites through this helper, so the
fallback had to carry the same budget or the guarantee would depend on how
the store happened to be constructed. The fallback now uses the same
pool-level implementation the manager wraps (`execTracked`), minus the
manager's watchdog instrumentation — there is no manager to instrument when
there is no manager.

### 4.2 The two shared primitives

`recordMemoryReviewOn` and `weakenMemoryOn` take a `memExecer` — an
interface satisfied by both `*sql.DB` and `*sql.Tx` — so that the **exact
same** validation, busy-error mapping and read-back assertion can run inside
a transaction and outside one. The comment above the interface says why:

> Both `*sql.DB` and `*sql.Tx` satisfy it, and that is the whole point: the
> EXACT same weight semantics, validation and read-back assertion can run
> inside a transaction and outside one.

That sharing is right for the **semantics** and wrong for the **retry
policy**: on the pool path the executor was a bare `*sql.DB`, and a bare pool
write is lost when the contention outlasts the busy timeout. Both writes are
agent-facing state — a memory's weight, and the record of a spaced review.

The fix adds a `poolMemExecer` adapter (a pool plus a budget) and passes it
on the two pool call sites. The transactional call sites are untouched, and
so is the shared implementation.

```go
func (dm *DatabaseManager) WeakenMemory(id string, delta int) error {
	return weakenMemoryOn(poolMemExecer{db: dm.db, retries: 5}, id, delta)
}
```

**The consequence for the guard is stated plainly, because it is not
comfortable.** The ratchet is syntactic: it sees `ex.Exec(…)` — an `Exec` on
a receiver that is not named `tx` — and it cannot see which concrete executor
a call site passes. So it keeps flagging both shared sites, and they stay in
the allowlist under a new reason, `shared-primitive`.

That is the right direction for the ratchet: a NEW bare write through the
same primitive must still trip its surplus check. But it means the allowlist
entry alone does not prove the pool path is budgeted. So the proof is a
separate guard — §5.2.

### 4.3 The third shared primitive — found by the guard's own reach check

The guard of §5.2 was written with a **reach check**: it discovers every
function in the runtime that takes an `Exec`-bearing interface parameter, and
fails if such a function is not classified. That check found a third shared
primitive this slice had not covered — `createLessonsViewAndTriggers` in
`internal/core/db.go`, which takes an `execQuerier` (`*sql.DB` and `*sql.Tx`
both satisfy it) and is called from four places: three transactional ones
and **one pool one** (`dm.db`, at the interrupted-migration repair path).

The pool call site is a bare pool write of DDL: the `lessons` view and the
three `INSTEAD OF` triggers. The ratchet records all four `q.Exec(…)` sites
under `reasonStartupDDL`, which is the right classification.

**It is deliberately not converted, and the reason is not "DDL is
untouchable".** It is that a statement budget would make it *wrong*:

- `CREATE TRIGGER` has no `IF NOT EXISTS`, so a **retry** of the trigger
  creation fails with "trigger already exists". A retry is not a safe
  remedy for a half-applied DDL sequence.
- The path is self-healing anyway: the next boot detects the missing
  trigger (`missing == 3` is the healthy case) and repairs it again.
- The requirement for this slice was explicit — do not mechanically convert
  migration code — and this is migration code.

**What actually makes the pool call site safe is the self-healing, not a
transaction.** Three of the four call sites are transactional
(`db.go:3730`, `db.go:3883` and `db.go:3947`, the last through
`node.Tx()`), and there the transaction owns the atomicity. The fourth —
the interrupted-migration repair path at `db.go:3917` — passes `dm.db`, the
**pool**, and runs the four DDL statements as four separate `Exec` calls
with **no transaction around them**. Its safety is the second bullet: the
next boot repairs what a half-applied sequence left behind. The remedy for
that call site, if one were wanted, is `dm.WithTx` — one line, and it would
inherit the snapshot-restart budget the design rationale assumes. It is not
done here, because §3.3's rule is that migration code is not mechanically
converted, and this record does not want to claim a property the code does
not have.

So the guard classifies it, and the classification is **checked** rather
than asserted: a shared primitive classified as migration DDL must execute
**no DML**. If `createLessonsViewAndTriggers` ever starts writing a row,
the classification is wrong, and the guard fails. That is assertion 4 of
§5.2, and it is mutation 8 of §8.

**What "the guard fails" means, precisely.** The check is that the
primitive's SQL statements do not *begin* with a row-writing verb. The
review of 2026-11-06 found that the check as written could be defeated by
a leading SQL comment — `^\s*` matched only whitespace, so a row-writing
statement written under an explanatory comment was invisible. That is fixed
(§5.2): the detection now strips leading whitespace and leading SQL
comments before matching. The match stays **anchored** at the first token
of the statement, so the row-writing DML inside
`CREATE TRIGGER … BEGIN DELETE FROM lessons_base … END` remains classified
as DDL — that DML is part of the schema definition, not a row the primitive
writes. The controls that pin this are
`TestSharedPrimitiveDDLOnlyDetectsRowWritingPrimitive` and
`TestSharedPrimitiveDDLOnlySeesThroughLeadingComments` (§5.3).

This is the third blind spot of this work's own instruments, and the second
one this work's own guard found: the audit's type-resolving scanner resolved
the shared-primitive question **per call site** and so saw the pool path of
`createLessonsViewAndTriggers` — but the slice's own classification step
only looked at the two primitives it had already found, and the reach check
is what made the third visible.

---

## 5. The permanent guards

### 5.1 The ratchet, extended

`internal/core/bare_pool_write_ratchet_test.go` is the permanent AST
regression guard for this class, and it was already on `main` (it landed in
`81eb8d41`, `08a9ff45`, `3d571d18`). This work **extends** it rather than
adding a second guard, because a second guard would be a second thing to keep
in step with the first.

Four changes:

1. **`bareWriteVerb` now matches `PRAGMA` and `VACUUM`** — and, since the
   independent review of 2026-11-06, `ATTACH`, `DETACH` and `REINDEX` as
   well (§5.1.1). The `PRAGMA`/`VACUUM` extension closed the detector gap
   of §1.3: the twenty-seven newly-visible sites are recorded
   in the allowlist with reasons — `startup-ddl` for the connection
   configuration PRAGMAs, `migration-once` for the pinned-connection
   `PRAGMA foreign_keys` pair inside the two table rebuilds, `best-effort`
   for the checkpoint, `propagated` for the `VACUUM INTO` snapshots.
2. **A sixth reason, `shared-primitive`**, with the semantics of §4.2.
3. **A seventh reason, `connection-scoped`**, with the semantics below.
4. **The twenty converted sites' entries were deleted** from the allowlist,
   which is how the ratchet is told that a write is fixed.

### 5.1.1 `ATTACH`, `DETACH` and `REINDEX` — the false claim, and the site behind it

This subsection records the independent review's finding M1 (2026-11-06),
which was correct and which the guard did not survive.

The ratchet's comment said:

> `REINDEX`, `ATTACH` and `DETACH` are deliberately NOT listed: the tree
> contains no bare pool write of any of them, and a verb in this list that
> no site can ever match is a claim the guard cannot back.

**The claim was false.** `internal/core/similarity_candidates_store.go:972`
issues `ATTACH DATABASE '%s' AS shared` through a pinned `*sql.Conn` drawn
from the substrate's pool:

```go
conn, err := dm.db.Conn(ctx)          // similarity_candidates_store.go:246
defer conn.Close()
…
stmt := fmt.Sprintf("ATTACH DATABASE '%s' AS shared", …)   // :971
if _, err := conn.ExecContext(ctx, stmt); err != nil {      // :972
```

The write is bare — no retry budget — and the ratchet's own receiver rule
counts it: `isPoolReceiver` treats every identifier other than `tx` as a
pool receiver, deliberately failing closed. The site was invisible **only**
because the verb was missing from the list. Adding it surfaces the site
immediately.

Both halves of the old rationale were wrong in opposite directions. "A verb
that no site can ever match is a claim the guard cannot back" is true of a
verb nothing can match — but the fix is to make the list **complete**, not
to leave a hole and then assert the hole is empty. A verb a site *can*
match and the list omits is a hole in the guard, and the hole is invisible
precisely because the detector cannot see it. The list is therefore now
about **detection capability**, not about inventory.

**What exists today, exactly: one bare `ATTACH`, and no bare `DETACH` and
no bare `REINDEX`.**

**How the new site is classified — and why it is not converted.** The
review suggested `startup-ddl`; that is not what the site is, and the
distinction matters because the remedy follows from it.

- SQLite's `ATTACH` is **per connection**, not per database. The schema it
  attaches is visible only to the connection that attached it. That is why
  the site runs on a **pinned** `*sql.Conn` at all, and why the surrounding
  code checks `PRAGMA database_list` on that same connection before
  attaching.
- It is therefore **not** startup DDL. The attachment is not made once per
  boot and left there; it is made per connection, on whichever connection
  the caller pinned, and the pool is deliberately multi-connection.
- **A pool retry budget would be wrong, not merely redundant.** The
  substrate's budget is spent by `execTracked`, which writes through the
  `*sql.DB` — so a retried `ATTACH` can land on a **different** pooled
  connection than the one holding the attachment the caller is about to
  use. A retried `ATTACH` on the **same** connection, meanwhile, fails with
  "database shared is already in use" once the attachment is in place.
  Neither is the behaviour the site needs. This is the rule the finding
  names: a connection-scoped `ATTACH` must not be converted into a retrying
  pool write.
- The site's own guard is the `PRAGMA database_list` check that precedes
  it on the same connection: the attachment is made only if it is missing,
  so the operation is idempotent per connection **without** a retry.
- The failure is reported to the caller as `ErrSharedUnavailable`, and the
  caller treats a shared source that cannot be attached as a broken
  installation rather than silently scanning half the corpus.

So the site is allowlisted with the new reason `connection-scoped`, and the
reason's own documentation says what the remedy for a future
connection-scoped write is: a per-connection retry that keeps the **same**
connection, not a pool budget. No such helper exists today, and this site
does not need one.

### 5.2 The shared-primitive guard

`internal/core/shared_primitive_budget_guard_test.go` is new. It is an AST
guard over the whole runtime and asserts four things:

1. **REACH** — every function in the runtime that takes an `Exec`-bearing
   interface parameter is **classified**. A shared primitive that nobody
   listed cannot slip past the guard by being written: the guard finds it
   and fails until it is classified. This is what found
   `createLessonsViewAndTriggers` (§4.3).
2. **BUDGET** — for a shared primitive that writes agent-facing state:
   - every construction of a `poolMemExecer` carries a **non-zero** retry
     budget — a zero budget is the #164 defect class in a new place;
   - every call site passes either a `poolMemExecer` (the pool path) or a
     transaction (the transactional path) — passing a raw pool handle there
     would reintroduce the bare write this slice removed.
3. **ADAPTER** — the **real** `poolMemExecer.Exec` routes through
   `execTracked` with the stored budget — otherwise the budget would be
   carried and then silently dropped. The check requires the receiver's own
   `retries` **field**: a literal would be a second, independent budget
   that could drift from the one the adapter carries.
4. **DDL-ONLY** — a shared primitive classified as migration DDL must
   execute **no DML**. That classification is the reason its pool path is
   allowed to pass a raw pool handle, so it is a checked property rather
   than a comment. The check strips leading whitespace and leading SQL
   comments (`--` to end of line, `/* … */`) before matching, and stays
   anchored at the first token of the statement — so a row-writing
   statement written under an explanatory comment is seen, and the
   row-writing DML inside a `CREATE TRIGGER … BEGIN DELETE … END` stays
   DDL.

**Every one of the four assertions is now pinned by a synthetic control.**
That sentence was not true of the first version of this guard, and the
independent review of 2026-11-06 (finding M2) is what made it true — see
§5.3.1. The controls are:

| Assertion | Control |
|---|---|
| ADAPTER | `TestPoolMemExecerRoutesThroughTheTrackedPolicy` |
| REACH | `TestSharedPrimitiveReachDetectsUnclassifiedPrimitive` |
| DDL-ONLY | `TestSharedPrimitiveDDLOnlyDetectsRowWritingPrimitive` and `TestSharedPrimitiveDDLOnlySeesThroughLeadingComments` |
| BUDGET | `TestSharedPrimitivePoolPathsCarryAStatementLevelBudget` (tree-wide; the budget assertions are structural — a zero budget is a literal, and a literal is read, not inferred) |

### 5.3 The negative controls

**Synthetic.** The ratchet's existing control table
(`TestBarePoolScannerFlagsRepresentativeShapes`) was extended with four
cases that fail against the old verb list and pass against the current one:

- a bare `PRAGMA` write is caught;
- a bare `VACUUM` write is caught;
- a bare `PRAGMA` write through `ExecContext` is caught;
- a shared primitive's `ex.Exec` is caught (the scanner fails closed on an
  interface-typed receiver).

and — for the M1 finding — with four more:

- a bare `ATTACH` is caught;
- a bare `DETACH` is caught;
- a bare `REINDEX` is caught;
- a bare `ATTACH` on a **pinned connection** is caught (the receiver rule
  fails closed on every identifier that is not `tx`).

`TestBarePoolAllowlistDetectsUnapprovedAttach` pins the M1 site itself: the
classifier must see the connection-scoped `ATTACH` shape, the shape must be
allowlisted **only** under its reviewed file, and therefore a newly
introduced `ATTACH` anywhere else fails the completeness guard by shape
rather than by luck.

**By mutation.** The synthetic controls prove the guard fires on the SHAPE.
To prove it fires on THIS TREE, the tree was mutated in a disposable copy and
the guard was run against each mutation. The results are in §8.

### 5.3.1 The guard's own self-verification (finding M2)

The independent review of 2026-11-06 found that this record cited a
synthetic control — `TestPoolMemExecerRoutesThroughTheTrackedPolicy` — that
**did not exist in the tree**, and that three of the guard's four assertions
could be switched off with the whole suite staying green:

| assertion broken | the suite |
|---|---|
| `routesThroughExecTracked` made to always return true | **green** |
| the reach check made to never fire | **green** |
| `sqlDMLPattern` made to match nothing | **green** |

That is the record's own §4.3 argument turned back on the guard itself: a
guard whose detection can be disabled without anything failing is not
pinned. The fix was to move the detection into **pure functions over
(rel, src)** — the idiom the bare-pool ratchet already used — so that a
synthetic source can be fed through them and the resulting **finding**
asserted directly. The controls are listed in §5.2's table; each one feeds
a deliberately broken shape and requires the corresponding finding.

Two further controls were added for the same reason:

- `TestSQLDMLPatternSeesThroughLeadingComments` — the unit-level control
  for the comment stripping itself, so the behaviour is pinned independently
  of the guard that consumes it; and
- `TestPoolMemExecerWaitsOutContention`
  (`internal/core/bare_pool_write_contention_test.go`) — the **runtime**
  half of the adapter's contract. The AST control says the adapter routes
  through the tracked policy; the runtime control says the tracked policy
  is worth routing through: the real `poolMemExecer.Exec` waits ordinary
  contention out and lands its write, where a bare pool write of the same
  statement loses the artifact.

---

## 6. The contention reproduction

`internal/core/bare_pool_write_contention_test.go` is new. It runs on
**disposable databases only** — a temporary directory, a temporary SQLite
file, two independent connections to that file. It never opens the operator's
`~/.mpm` and it never touches a running service.

`TestBarePoolWriteLosesItsArtifactToContention` asserts both halves of the
defect class on the same database, with the same held lock:

1. the **bare** pool write (`db.Exec`) fails with a lock contention while the
   lock is held, and the row it was supposed to write is **not there** — the
   artifact is lost;
2. the **tracked** route (`execTracked(db, query, 5, …)`) waits the same
   contention out, and the row **is** there.

`TestConvertedMemoryWriteSurvivesContention` drives the **converted
production path** — `DatabaseManager.SetMemoryWeight`, one of the twenty
sites this slice moved — under the same contention, and asserts the weight
lands.

The lock is taken with `BEGIN IMMEDIATE` (which holds the write lock for the
whole transaction) and released from a separate goroutine while the tracked
write is in flight, so the tracked write has to **wait it out** — the
success below is the result of a retry, not of a write that never
contended.

**Why the busy timeout is short here.** The production DSN asks SQLite to
wait five seconds for a lock (`_busy_timeout=5000`). A test that reproduced
the production behaviour literally would have to hold the lock for longer
than five seconds, which makes the test slow without making it more
convincing: the defect is that the write has **no retry**, not that the
timeout is five seconds. The timeout here is 100 milliseconds, so the
contention outlasts it in ~100ms. The policy under test — *an ordinary
SQLITE_BUSY is a wait, and the budget decides how long the substrate waits
it out* — is identical at either timeout.

**Relationship to the phase-0 reproductions.** The unmerged branch
`audit/issue-178-bare-write-risk` carries
`internal/core/bare_write_contention_repro_test.go` (§9), which reproduces
the same claim on the **production** DSN, and adds two more: that a
dropped-error write loses the artifact with no signal, and that a retried
`SQLITE_BUSY` statement does not double-apply. That second one is the
precondition for the whole correction strategy — it is why the remedy here
is a retry budget rather than "the caller should try again". Those
reproductions are not on `main`, so this slice does not depend on them; it
restates the precondition it relies on in its own terms. The two sets are
complementary: the phase-0 set establishes the claim on the shipping
configuration, this one establishes that **the converted production path**
uses the policy.

---

## 7. What remains

After the slice, on the branch, the ratchet's inventory (which now also sees
`PRAGMA`/`VACUUM` and the connection-scoped `ATTACH`/`DETACH`/`REINDEX`) is:

| | occurrences | files |
|---|---|---|
| bare pool writes, ratchet (syntactic, fail-closed) | 141 | 53 |
| bare pool writes, type-resolved | 133 | 49 |
| of which: already budgeted on the pool path (the shared primitives) | 2 | 2 |

and the class histogram is:

| Class | Base | After the slice |
|---|---|---|
| `ordinary` — un-fixed | 71 | **49** |
| `ordinary` — budgeted on the pool path (shared primitives) | 0 | 2 |
| `migration` | 52 | 52 |
| `maintenance` | 38 | 38 |
| `tx-owned` | 0 | 0 |
| **total** | **161** | **141** |

(The ratchet's pre-extension list measured **133** at base. The 28-site
difference between 133 and 161 is the detector gap closing: the 27
`PRAGMA`/`VACUUM` sites of §1.3 plus the one connection-scoped `ATTACH` of
§5.1.1 — of which 20 were converted by this slice, so the head measures
**141**.)

So **49 ordinary bare pool writes remain**, across 22 files, and they are
concentrated in:

| File | Ordinary writes | Why they were not in this slice |
|---|---|---|
| `internal/core/db.go` | 13 | Session summaries, system config, topics, works, lessons, global rules, and the shared-schema writes. Several are schema DDL; the rest are a separate surface (session, work and lesson state) that deserves its own slice. |
| `cmd/mpm/simple_cmds.go` | 4 | The CLI's own memory writes (`rm`, `promote`, `set-weight`, `snooze`) — the same class as the slice, on the CLI surface. |
| `internal/core/cluster_proposals.go` | 3 | Cluster-proposal status. |
| `internal/core/lesson_proposals.go` | 3 | Lesson-proposal promotion and rejection. |
| `internal/core/memory.go` | 3 | The exemptions of §3.3 — the topic deactivation, and the two unreachable bulk deletes. |
| `internal/core/skill_db.go` | 3 | The skill save, the global promotion, and the skill shred. |
| the rest (16 files) | 20 | The handoff, ingestion, scheduled-task, blobstore, self-heal, contradiction, admission, arbitration, epistemology and reference paths. |

**The next slice should be `internal/core/db.go`'s session and work state**
(`UpdateSessionSummary`, `AddWork`, `updateWorkStatus`, `SaveSystemConfig`,
`DeleteSystemConfig`, `CreateTopic`, `DeleteLesson`, `RestoreLesson`) —
the same ordinary class, the same loss, and the same one-line conversion. It
is deliberately not in this PR: the slice is meant to be small enough to
review as a whole. `cmd/mpm/simple_cmds.go`'s four memory writes are the
other obvious candidate — they are the CLI's twin of the sites this slice
converted.

### 7.1 The zero-budget guard cannot see the store's write surface

While checking the slice's own conversions against the repository's
existing **zero-budget guard** (`internal/core/tx_write_budget_guard_test.go`,
the guard for issue #164), a second fail-open gap surfaced. It is not this
issue's class, and it is not fixed here — but it is recorded because the
guard's own header claims it covers the whole runtime, and it does not.

**The gap.** The guard classifies a write when the call's method name is
`ExecTracked`:

```go
sel, ok := call.Fun.(*ast.SelectorExpr)
if !ok || sel.Sel.Name != "ExecTracked" {
    return true
}
```

That is the **DatabaseManager's** method. The substrate has exactly two
tracked write surfaces: `DatabaseManager.ExecTracked` and
`MemoryStore.execTracked` (the store's helper, lowercase). **The store's
surface is invisible to the guard**, because the match is on the method
*name*, not on the surface.

**What it hides.** An AST sweep of the whole runtime for tracked writes with
a literal budget of zero finds exactly **seven**:

| Site | Surface | Covered by the zero-budget guard? |
|---|---|---|
| `internal/core/reference_db.go:106, 152, 175, 198` | `dm.ExecTracked` | **yes** — all four are in the guard's exemption table, with the `IngestReference` outer owner named |
| `internal/core/memory.go:2010` | `s.execTracked` | **no** |
| `internal/core/memory.go:2930` | `s.execTracked` | **no** |
| `internal/core/memory.go:3037` | `s.execTracked` | **no** |

The three `memory.go` sites are the **dedup and consolidation sweeps** — the
soft-delete of a cluster's non-best members, of exact duplicates, and of
near-duplicates. Their zero budget is **defensible on the merits**: a sweep
whose loss costs a re-run, not a state, is the maintenance class of §2, and
the statements are idempotent, so a budget would buy little.

**Why it is not fixed here — and why it cannot be fixed with the existing
tool.** The guard's exemption vocabulary has exactly one shape: an
exemption names the **outer layer that owns the busy class** and the reason
a statement-level budget would *multiply* that owner's budget. The three
sweep sites have **no outer owner**: nothing re-drives the dedup pass on a
busy error, so there is no budget to multiply and no owner to name. Making
the guard see them therefore means extending the exemption vocabulary with a
"no outer owner, deliberately best-effort" case — a change to a guard that
has been stable since issue #164's corrections, which deserves its own
record rather than a paragraph in this one.

**Why it still matters.** The gap is fail-open in the direction that
matters: a **future** zero-budget write on the store's surface would not be
caught by the guard that exists to catch exactly that. It is recorded here
so the next person to touch `MemoryStore.execTracked` knows the write
surface is unguarded, and so the guard's "covers the whole runtime" claim
is not read as covering both write surfaces.

### 7.2 The ratchet's parameter-SQL gap

The ratchet's `literalScope` binds package-level constants, local constants
and local variables — but **not function parameters**, and **not range
variables**. A bare pool write whose SQL comes from either is therefore
invisible to it.

**The parameter case.** The one such site in the tree was
`MemoryStore.execTracked`'s fallback (§4.1), and this slice fixed it. But
the gap remains for the next one, and it is in the fail-open direction.

**The range-variable case.** `literalScope` binds `*ast.AssignStmt` and
`*ast.ValueSpec`; a `*ast.RangeStmt` (`for _, sql := range BaseTables`) is
neither, so the range variable is never bound and `resolveSQL` returns `""` —
the verb regex never sees it. In this tree today that hides thirteen bare
pool writes, all of them startup DDL:

```
internal/core/memory.go:316,323,328,335,341,393    s.DB.Exec(sql)     over BaseTables /
                                                                 ReferenceTables /
                                                                 ReferenceIndexes /
                                                                 CommonIndexes /
                                                                 WorkTables / triggers
internal/core/db.go:3071,3424,3623,4277             dm.db.Exec(sql|t|b) over CommonIndexes /
                                                                 revisionTriggers / trigger
                                                                 cleanup / FTS backfill
```

Nothing agent-facing is hidden today — they are all startup DDL — but the
shape is unguarded, and the record used to tell the next person that the gap
was only about parameters. It is not.

Both are recorded here rather than fixed, because fixing them means
extending the ratchet's scope resolution — a change to a guard that has been
stable since `81eb8d41`, which deserves its own record. The review of
2026-11-06 that named the range-variable case (finding L1) did not treat it
as merge-blocking for the same reason: the disclosure is what matters here,
and the disclosure was incomplete.

---

## 8. The mutation demonstration

The negative control has to prove two different things. The synthetic
controls prove the guard fires **on the shape**; the mutations prove it fires
**on this tree**. Each mutation was applied to the branch, the guard was run,
and the mutation was reverted.

| # | Mutation | Guard that fired | Result |
|---|---|---|---|
| 1 | Revert one converted site (`memory_tools.go` `SetMemoryWeight`) to a bare `dm.db.Exec(…)` | `TestBarePoolWriteAllowlistIsComplete` | **FAIL** — the reverted site is reported as a surplus bare pool write, by file and statement |
| 2 | Add a bare `dm.db.Exec("PRAGMA wal_checkpoint(TRUNCATE)")` to `internal/core/db.go` | `TestBarePoolWriteAllowlistIsComplete` | **FAIL** — the new bare PRAGMA is reported as a surplus. This mutation is caught **only** because the verb list was extended; against the pre-extension list it passes, which is what mutation 6 demonstrates |
| 3 | Set a `poolMemExecer`'s budget to `0` | `TestSharedPrimitivePoolPathsCarryAStatementLevelBudget` | **FAIL** — the zero budget is named, with the #164 defect class quoted |
| 4 | Pass a raw pool handle (`dm.db`) to a shared primitive | `TestSharedPrimitivePoolPathsCarryAStatementLevelBudget` | **FAIL** — the call site is named as a bare pool write |
| 5 | Make `poolMemExecer.Exec` drop the budget (call `p.db.Exec` directly) | `TestSharedPrimitivePoolPathsCarryAStatementLevelBudget` | **FAIL** — the adapter's write path no longer routes through `execTracked` |
| 6 | Revert the `bareWriteVerb` extension to the pre-extension list | `TestBarePoolAllowlistHasNoStaleEntries` | **FAIL** — the twenty-seven `PRAGMA`/`VACUUM` sites become **stale**: the guard can no longer see them, so their allowlist entries no longer have a live site. Reverting the verb extension does not merely hide the new sites; it makes the recorded inventory a lie, and the guard says so |
| 7 | Remove a shared primitive from the guard's classification | `TestSharedPrimitivePoolPathsCarryAStatementLevelBudget` | **FAIL** — the reach check names the function and says it needs a classification |
| 8 | Add a row-writing statement to a migration-DDL shared primitive (`createLessonsViewAndTriggers`) | `TestSharedPrimitivePoolPathsCarryAStatementLevelBudget` | **FAIL** — the DDL-only check names the function and the statement, and says the classification must change |

### 8.1 The seven mutations added for the M1/M2/M3 findings

The eight above are the mutations this slice documented. The independent
review of 2026-11-06 found three guard defects and, with them, three ways to
defeat the guard that the eight mutations did not cover. Seven further
mutations were added; each one breaks a protection and the corresponding
test must fail.

| # | Mutation | Guard that fired | Result |
|---|---|---|---|
| 9 | Revert the `ATTACH`/`DETACH`/`REINDEX` verb extension | `TestBarePoolAllowlistHasNoStaleEntries` | **FAIL** — the recorded connection-scoped `ATTACH` becomes **stale**: the guard can no longer see the site's own allowlist entry, which is the same "the recorded inventory is a lie" failure as mutation 6 |
| 10 | Introduce a **new** bare pool `ATTACH` in the tree | `TestBarePoolWriteAllowlistIsComplete` | **FAIL** — the unapproved statement is reported as a surplus, by file and statement. This is the "a newly introduced unapproved statement is detected" case |
| 11 | Make `routesThroughExecTracked` always return true (the review's probe) | `TestPoolMemExecerRoutesThroughTheTrackedPolicy` | **FAIL** — the synthetic adapter that spends a literal budget is no longer flagged. Before the fix this mutation left the whole suite green |
| 12 | Make the reach check never fire (the review's probe) | `TestSharedPrimitiveReachDetectsUnclassifiedPrimitive` | **FAIL** — the unclassified synthetic primitive is no longer flagged. Before the fix this mutation left the whole suite green |
| 13 | Make `sqlDMLPattern` match nothing (the review's probe) | `TestSharedPrimitiveDDLOnlyDetectsRowWritingPrimitive`, `TestSharedPrimitiveDDLOnlySeesThroughLeadingComments`, `TestSQLDMLPatternSeesThroughLeadingComments` | **FAIL** — the row-writing synthetic primitives are no longer flagged, and the stripping is no longer pinned. Before the fix this mutation left the whole suite green |
| 14 | Reintroduce the comment bypass — the old `^\s*` pattern and no comment stripping | `TestSharedPrimitiveDDLOnlySeesThroughLeadingComments`, `TestSQLDMLPatternSeesThroughLeadingComments` | **FAIL** — the row-writing statements behind comments are no longer seen |
| 15 | Add a **comment-prefixed** row-writing statement to the real `createLessonsViewAndTriggers` | `TestSharedPrimitivePoolPathsCarryAStatementLevelBudget` | **FAIL** — the review's exact reproduction, which passed silently before the fix |

Mutations 11–13 are the reviewer's own probes. Their **red** state was
measured on the pre-fix head (`c13b5b70`), where each of them left the whole
suite green; mutations 9, 10, 14 and 15 are the red-to-green proofs for M1
and M3, measured the same way. §11.2 gives the commands.

Each mutation was applied to the branch, the guard was run, and the mutation
was reverted; the demonstration asserts that the revert left the tree clean.

---

## 9. A dangling citation — found, and located

`internal/core/bare_pool_write_ratchet_test.go` cites
`docs/dev/bare-write-risk-assessment-2026-10-29.md` as the audit document
that describes the shapes that defeat the scanner's classification, and
`docs/dev/issue-178-corrections-phase-1-2026-10-29.md` lists it in its
artifact table.

**That document is not on `main`.** The citation therefore dangles for every
reader of `main` — the ratchet's own justification is unreadable.

**It is not lost, and this record has to say where it is.** It was found on
the unmerged branch `audit/issue-178-bare-write-risk`, in the single commit
`74eac183` (*"docs(audit): issue #178's bare-write risk assessment — the
inventory, the reproductions, and the bounded correction strategy"*), which
adds three files:

| File | What it is |
|---|---|
| `docs/dev/bare-write-risk-assessment-2026-10-29.md` | the phase-0 assessment the ratchet cites |
| `internal/audit/barewritescan/main.go` | the phase-0 scanner |
| `internal/core/bare_write_contention_repro_test.go` | four contention reproductions, on the **production** DSN (`_busy_timeout=5000`) |

That branch has not been merged: `git merge-base --is-ancestor
origin/audit/issue-178-bare-write-risk origin/main` is false. The document
was written on 2026-10-29 against `f38a36ea`, and it is a record of that
tree's state, not of this one.

**This PR does not merge that branch.** It is a separate line of work, its
document belongs to its author, and merging someone else's unmerged commit
into a "small first slice" would conflate two records and make the review
harder, not easier. The citation is left in place, and the defect is
recorded here so that the next person to read the ratchet knows **both**
that the citation dangles and exactly which ref carries it. Restoring it is
a one-line decision for whoever owns that branch, and it should be that
person's decision.

Two things in that branch bear on this record, and are acknowledged rather
than re-derived:

- **The phase-0 scanner** (`internal/audit/barewritescan`) is the scanner
  whose numbers appear in circulation. The audit instrument of §1.2 was
  written independently and out of tree, and the reconciliation of §1.3 is
  between that instrument and the **merged** ratchet — not against the
  phase-0 scanner, whose exact code is not on `main`.
- **The phase-0 reproductions** use the production DSN and therefore hold
  the lock for longer than the five-second busy timeout. The reproduction
  in §6 deliberately does not (§6 says why): the policy under test is the
  same at either timeout, and the short one makes the suite fast. The two
  are complementary, not competing.

### 9.1 A second defect: a pre-existing flake in the release gate

The release gate failed on the first exact-head CI run, with two tests in
`cmd/mpm`:

```
--- FAIL: TestOnboardInspectionNeverMutates/fresh
--- FAIL: TestOnboardInspectionNeverMutates/full
--- FAIL: TestOnboardModelsScopeNeverWritesWithoutATerminal/onboard --yes
```

The assertion output names the cause exactly — the workspace gained a file:

```
before: "src/db/mpm.db... src/db/mpm.db.lock src/db/mpm.db.schema-init.lock"
 after: "... src/db/watchdog.jsonl{"v":2,"level":"warn","op":"exec",
         "reason":"slow","duration_ms":141,
         "sql_shape":"DROP TRIGGER IF EXISTS lessons_instead_of_insert", ...}"
```

**This is not caused by this change, and the proof is an experiment rather
than an argument.** The watchdog logs a statement that took longer than
`slowQueryThreshold` (100 ms) by appending a line to `src/db/watchdog.jsonl`
— **inside the workspace**. `TestOnboardInspectionNeverMutates` asserts that
the workspace tree is byte-identical before and after an `onboard` run, so a
watchdog line breaks it. Under `-race` in a loaded CI container, the
lessons-trigger repair's `DROP TRIGGER` took 141 ms and 104 ms — just over
the threshold.

That `DROP TRIGGER` is at `internal/core/db.go` in the interrupted-migration
repair path. **This slice did not touch it**, and it already went through
`ExecTracked` before this change.

To show the failure is pre-existing rather than timing-luck, the threshold
was temporarily set to zero on the **base commit** (`653e4633`, with none of
this change present) and the same test was run:

```
--- FAIL: TestOnboardInspectionNeverMutates/fresh
--- FAIL: TestOnboardInspectionNeverMutates/partial
--- FAIL: TestOnboardInspectionNeverMutates/full
--- FAIL: TestOnboardInspectionNeverMutates/models-skipped
--- FAIL: TestOnboardInspectionNeverMutates/broken
```

**Five of five subtests fail on the base commit**, with the identical
assertion, the moment a statement crosses the slow threshold. The probe was
reverted immediately afterwards.

The defect is therefore: **the "onboard never mutates" contract and the
watchdog's slow-query log are in conflict**, and the winner depends on
machine load rather than on behaviour.

#### The second flake, and why this branch's CI exposed it

The same CI run set exposed a **second**, independent pre-existing flake:

```
--- FAIL: TestMcpBootstrapsModeAndPersonaDirsOnFreshWorkspace
    D-013 bootstrap: expected mode/ to exist after mpm-mcp boot in fresh
    workspace "..."; stat err=stat .../mode: no such file or directory.
```

That test starts `mpm-mcp`, waits a **fixed 800 ms**, kills the process and
then checks that the bootstrap created `mode/` and `persona/`. The 800 ms
is a timing assumption about the boot, and the boot is not bounded by the
code: it opens a database, runs the schema initialisation and the
migrations, and builds the router. On a loaded 2-vCPU CI runner that can
take longer than any constant chosen here, and the test then reports a
regression that did not happen — the directories simply had not been
created **yet**.

The measurement that clears this change of the failure is direct: the boot
time to create `mode/` was measured on **both** binaries, interleaved,
sixteen runs each (the base commit `653e4633` and this branch):

| | median | max | over the 800 ms budget |
|---|---|---|---|
| base `653e4633` | 88–107 ms | 118 ms | 0/16 |
| this branch | 90–91 ms | 99 ms | 0/16 |

The branch is **not slower** than the base; the two are indistinguishable
at this resolution, and both sit about 8× under the test's budget on an
unloaded machine. The boot path does not touch any statement this slice
converted.

#### Both flakes are fixed here — in a separate commit, with the proofs

Both are **test-only** defects, both are **pre-existing**, and both make
the exact-head CI unreliable, which is a deliverable of this work. So they
are fixed — as a **separate commit** on this branch, so that a reviewer can
read them as their own unit and the slice's review is not contaminated:

| Commit | Change |
|---|---|
| the slice | the 23 conversions, the ratchet extension, the shared-primitive guard, the contention reproduction, the two records |
| the flake fixes | `cmd/mpm/onboard_cmds_test.go`: exclude `watchdog.jsonl` from the tree digest, the way the test already excludes SQLite's WAL/SHM siblings — a watchdog line is a diagnostic, not a state mutation. `cmd/mpm-mcp/fresh_workspace_bootstrap_test.go`: wait for the bootstrap directories to **appear** instead of for a fixed interval to elapse. |

**The proofs that each fix addresses its exact failure:**

1. **The onboard fix.** With the slow-query threshold temporarily set to
   zero — so that **every** statement is logged — the **base commit** (with
   none of this change present) fails **five of five** subtests with the
   identical assertion; this branch, **with the exclusion and the same
   threshold**, **passes**. The exclusion is therefore precisely what makes
   the test assert behaviour instead of a timing accident.
2. **The mcp-mcp fix.** The measured boot times above show the budget, not
   the branch, is the problem. Waiting for the effect is also *faster* in
   the normal case — the directories appear as soon as the bootstrap loop
   runs — and the ceiling is generous (30 s) with an early exit when the
   process dies.

Both probes were reverted immediately; neither left a trace in the tree.

**No production code is touched by the flake fixes.** They are assertions
about timing, and the fix is to assert the effect.

### 9.2 A third defect: a stale entry in the CLI-limits gate

The repository's own pre-commit gate `scripts/doc-gate-cli-limits.sh` fails
on the base commit, with:

```
doc-gate: 'maxRefBytes' listed in gate is missing from source.
  Source-existence regex 'const maxRefBytes = 100 << 20' matched no
  production file in cmd/mpm/.
```

`maxRefBytes` does not exist anywhere in the tree — the constant was
renamed or removed, and the gate's `REQUIRED_CLI_LIMITS` list was not
updated. The gate that exists to prove a CLI limit is still enforced is
instead proving that the limit it names is gone.

This is **pre-existing** — it fails identically on `653e4633` — and it is
**not fixed by this change**: it is outside the scope of a bare-write
slice, and the right fix (which limit replaced `maxRefBytes`, and where it
lives now) is a question about the CLI's reference-size limit, not about
write budgets. It is recorded here so that whoever runs the pre-commit
gate next knows the failure is not theirs.

---

## 10. What this does not close

- **The scheduler's duplicate-delivery crash window** remains open by design.
  It is architectural, and no retry budget closes it. See the phase-3 record
  §5 for the migration plan.
- **49 ordinary bare pool writes remain** (§7). This is the first slice of
  a multi-slice remediation, not the remediation.
- **The parameter-SQL and range-variable detector gaps** remain (§7.2).
- **The zero-budget guard's blind spot on the store's write surface**
  remains (§7.1): `MemoryStore.execTracked` is invisible to it, and the
  three deliberate zero-budget sweep sites it hides are named there.
- **The atomic review path has no retry budget at all** — see §10.1.
- **A pooled `ATTACH` whose retry can land on a different connection** —
  see §10.2.
- **The stale `maxRefBytes` entry in the CLI-limits gate** remains (§9.2). It
  is pre-existing and unrelated to this class.
- **The "onboard never mutates" / watchdog slow-log conflict** and the
  **mcp-mcp boot-budget flake** were found here, proved pre-existing, and
  **fixed in a separate commit** (§9.1). They are test-only and unrelated
  to this class.
- Issue #178 stays **open**.

### 10.1 The atomic review path writes the same rows with no retry at all

`internal/core/web_db.go:2020` — `RecordMemoryReviewsAndApply` — opens a
**bare** `*sql.Tx` (`tx, err := dm.db.Begin()`), not `dm.WithTx`. The
design rationale for leaving the transactional path unbudgeted is stated at
`internal/core/review.go:126-130`:

> the transactional path — whose retry policy belongs to the transaction, not
> the statement — exactly as it was.

**For this transaction there is no retry policy.** `dm.WithTx` owns the
`SQLITE_BUSY_SNAPSHOT` restart; a bare `Begin()` owns nothing. So
`RecordMemoryReviewsAndApply` — the CLI's review-and-apply path — writes
the same `memory_reviews` row and the same weight change as the two
primitives this slice budgeted, and under ordinary contention the whole
batch is rolled back and nothing is recorded.

Neither guard sees it: the ratchet exempts every `tx` receiver by name, and
the shared-primitive guard checks only the **pool** path.

This is **pre-existing**, it is **reported** to the caller rather than
silently lost, and it is atomic rather than partial. It is recorded here
because it is the same defect class on the same write path, and because the
"what remains" list should not omit it. The clean fix is `dm.WithTx` — one
line, and it would inherit the snapshot-restart budget the rationale
already assumes. It is not done here: it is a **production** change, and
this slice's second commit was deliberately test-only.

### 10.2 A pooled `ATTACH` whose retry can land on a different connection

`internal/core/db.go:2121` — `attachShared` — runs
`dm.ExecTracked(stmt, 5)` where `stmt` is
`ATTACH DATABASE '…' AS shared`.

SQLite's `ATTACH` is **per connection**, and the substrate's pool is
deliberately multi-connection (`internal/core/db.go:1674`: *"SetMaxOpenConns(1)
is INTENTIONALLY NOT called here"*). The repository already knows this:
`similarity_candidates_store.go:927-939` pins a connection for exactly this
reason, and §5.1.1 is the analysis of the site it protects.

So the retry here is not *duplicating* the side effect; it can **misplace**
it: an `ATTACH` that succeeded on connection A is fine, but a retried
`ATTACH` may land on connection B, and the attachment the caller believes
it has will not be there.

This is **pre-existing** and **outside this slice's scope** — it is not one
of the 23 corrections, and it is not a bare pool write (it goes through
`ExecTracked`, so the ratchet does not see it). It is recorded here because
it is the same statement as the M1 site, and because a reader deciding
whether to add `ATTACH` to the verb list should know both halves of the
picture: the connection-scoped site, and this pooled one.


---

## 11. Reproducing this work

```bash
# the guards
cd internal/core && go test -tags fts5 -run 'TestBarePool|TestSharedPrimitive|TestPoolMemExecer|TestSQLDMLPattern' .
cd internal/core && go test -tags fts5 -run 'TestBarePoolWriteLosesItsArtifact|TestConvertedMemoryWriteSurvives|TestPoolMemExecerWaitsOutContention' .

# the whole runtime
make test
make test-race
make docs-check
go run ./cmd/mpm-lint --gate
```

### 11.0 The validation results

| Gate | Result |
|---|---|
| `make test` | **PASS** (exit 0) on the branch head |
| `make test-race` | **PASS** (exit 0) on the branch head |
| `make docs-check` | **PASS** — 13 checks, 0 review items, 0 errors |
| `go run ./cmd/mpm-lint --gate` | **PASS** — all nine classes (ctx, go, mutex, sql, fd, imports) |
| `scripts/doc-gate-threshold-table.sh` | **PASS** |
| `.build/bin/mpm ops lint` (the router linter) | **PASS** |
| `scripts/doc-gate-cli-limits.sh` | **FAIL** — pre-existing, §9.2 |

The branch head is the exact commit the PR carries; the CI run linked from
the PR is the exact-head run.

**The branch carries three commits.** The first is the slice. The second is
the test-only fix for the two pre-existing CI flakes of §9.1 — a separate
commit so that a reviewer can read it as its own unit, and so that the
slice's review is not contaminated by an unrelated change. The third is the
guard-layer remediation of §12.3: the ratchet's verb list and its reasons,
the shared-primitive guard's synthetic controls, and this record's
corrections. None of the three touches production code outside the slice's
own 23 conversions; the third touches **no** production code at all.

The audit instrument is not in the tree — it was a throwaway analysis tool
built for this work, and the ratchet is the permanent artefact. What the
instrument did that the ratchet cannot do is resolve the receiver's TYPE,
which is what made the reconciliation of §1.3 possible.

### 11.2 The mutation demonstration, and how to re-run it

The fifteen mutations of §8 are re-runnable. Each one is applied to the
tree, the relevant test is run, and the mutation is reverted byte-for-byte:

```bash
# the seven mutations added for the M1/M2/M3 findings
python3 /tmp/opencode/audit/mutate_remediation.py

# the red state: the pre-fix head, with the defects present
python3 /tmp/opencode/audit/mutate_prefix_red.py
```

The first asserts that **every** mutation is DETECTED; the second asserts
the **red** state on the pre-fix head — that the reviewer's three probes
left the suite green, that the record's named control did not exist, and
that the comment-prefixed row write passed the guard silently.

The ratchet's own inventory can be re-measured from the tree with the
guard's own scanner (this is the half of §1.3 that **is** reproducible):

```bash
cd internal/core && go test -tags fts5 -run 'TestBarePoolWriteAllowlistIsComplete' -v .
```

The type-resolving scanner's half is **not** reproducible from this PR — see
§1.3.

### 11.1 One note on running the suite with a redirected workspace

`PinIsolatedWorkspace` pins `MPM_WORKSPACE` to a per-test temporary
directory **only when it is unset**:

```go
func PinIsolatedWorkspace(t *testing.T) {
	t.Helper()
	if ws := os.Getenv("MPM_WORKSPACE"); ws != "" {
		return
	}
	t.Setenv("MPM_WORKSPACE", t.TempDir())
}
```

So exporting `MPM_WORKSPACE` for the whole suite — which is a natural thing
to do to keep a test run away from the operator's live `~/.mpm` — **defeats
that per-test isolation**. Ten `cmd/mpm` tests then fail, and they fail for
the right reason: the GC failure-reporting tests make a read fail and
expect a non-zero exit, and the Onboard tests assert that an inspection
mutated nothing. With one shared workspace, neither the failure nor the
mutation is observable.

Redirecting `HOME` instead has a different, narrower failure:
`TestDrillE2E_ClaudeCode` shells out to the real `claude` CLI, and the CLI
needs its credentials from the real `~/.claude`, so the drill reports "Not
logged in" and no audit rows are written. That test composes its own
`t.TempDir()` workspace and sets `MPM_WORKSPACE` to it, so it never touches
the operator's workspace — it only needs the real HOME for the CLI.

The suite is hermetic by design: the repo carries an AST guard
(`internal/core/test_db_safety_test.go`) that fails any test file
constructing `NewDatabaseManager("")` or `NewMemoryStore(...)` — the two
constructors whose path argument is discarded and which therefore resolve
to `$HOME/.mpm/src/db/mpm.db`. So the canonical invocation is the plain one:

```bash
make test
```

Both findings were reached the hard way: the first full run of this slice
was done with `MPM_WORKSPACE` exported and produced ten failures that had
nothing to do with the change; the second was done with `HOME` redirected
and produced one. Every one of them passes on the same tree with the
canonical invocation, and on the base commit too.

---

## 12. The review

**This work is not to be merged without independent review.** The PR
requests it explicitly, and this section records what was actually run and
— just as importantly — what could not be run here.

### 12.1 The review that was run

The substrate's own `mpm_request_review` was run against **the production
diff itself** (the four changed files, in context), with a prompt that asked
for a **verdict on seven specific questions** rather than for a summary:

1. **DOUBLE-APPLY** — is every converted statement safe to re-run after a
   `SQLITE_BUSY` failure? The premise is that a busy failure applied
   nothing; the review was asked to check that premise against the
   statements rather than accept it.
2. **READ-BACK ASSERTIONS** — does the conversion change what the
   `RowsAffected` and read-back assertions prove?
3. **ERROR BEHAVIOUR** — is the returned error the same value and type,
   outside the retry?
4. **THE FALLBACK** — is the `MemoryStore.execTracked` fallback
   behaviourally equivalent to the raw `Exec` it replaced, apart from the
   retry?
5. **THE ADAPTER** — is the adapter's write path equivalent, and are the
   transaction call sites of the shared primitive untouched?
6. **ARGUMENT ORDER** — does the new budget argument sit in the position the
   helper expects in **every** converted call, with the bind values neither
   shifted, dropped, nor reordered?
7. **ANYTHING ELSE** — a wrong, risky, or unintended change.

The result was **seven passes and no defect found**. Question 6 was checked
by counting placeholders against bind values per converted call (the
fourteen-placeholder insert retained fourteen values; the reinforce/weaken
statements retained delta, the computed gain or loss, and the id; and so
on). Two caveats the review itself raised are worth carrying forward: a
statement using the current time recomputes that timestamp on a retry
rather than reusing the failed attempt's, and the equivalence claims about
the tracked helper rest on its stated contract, which the diff does not
itself show.

### 12.2 Why that is NOT independent review

The live substrate has **exactly one** configured LLM profile:

```json
"profiles": {
  "default":  { "provider": "custom", "model": "MiniMax-M3", ... },
  "embedding":{ "provider": "ollama", "model": "nomic-embed-text", ... }
},
"components": { "embedding": "embedding", "memory": "default" }
```

`critic` is not a configured component at all — it falls back to `default`,
which is **the same model that produced this work**. So what ran is a
**fresh-context pass over the diff by the same model**: worth having (it
checked argument order and double-apply semantics against the actual text,
which is more than this record asserts), but it does **not** discharge the
requirement.

**Independence has to come from a human reviewer, or from a differently
configured profile.** If a second profile is pointed at this PR, the same
review should be re-run there and the result posted.

The reviewer's own verdict on the seven questions, and the caveats above,
are reproduced verbatim in the PR's review comment, so that a human reviewer
sees the machine pass and its limits side by side rather than a summary of
them.

### 12.3 The independent review of 2026-11-06, and what it changed

An independent reviewer (a fresh context, not this session, and not the
substrate's own review tool) reviewed the branch at `c13b5b70` against
`main` @ `653e4633` and returned **REQUEST CHANGES**, narrowly, on the
**guard layer**. Its verdict on the production layer was that the 23
corrections are correct as they stand and that it "could not find a single
production defect"; its three findings were all about the guards and this
record.

| Finding | What it was | What this branch does about it |
|---|---|---|
| **M1** | The ratchet's comment claimed the tree contains no bare pool `ATTACH`/`DETACH`/`REINDEX`. It does contain one (`similarity_candidates_store.go:972`), and the review reproduced the contradiction in one line of diff | §5.1.1: the verbs are added, the site is classified `connection-scoped` and allowlisted, and the comment states what is actually true. The site is **not** converted — §5.1.1 says why a pool retry budget would be wrong for it |
| **M2** | This record cited a synthetic control (`TestPoolMemExecerRoutesThroughTheTrackedPolicy`) that **did not exist**, and three of the guard's four assertions could be switched off with the whole suite staying green | §5.3.1: the detection moved into pure functions over (rel, src); the named control now exists, and there is one for the reach check and one for the DDL-only check. The **runtime** half of the adapter's contract is pinned too (`TestPoolMemExecerWaitsOutContention`) |
| **M3** | The DDL-only assertion was defeated by a leading SQL comment, so this record's claim that "if the primitive ever starts writing a row … the guard fails" was false | §5.2 and §4.3: the detection strips leading whitespace and leading SQL comments; the claim is now true, and precisely stated (the match stays anchored, so a trigger body stays DDL) |

The review also recorded four **LOW** findings that are **not** addressed by
this branch, and one **note**:

- **L1** — the ratchet's scope gap also covers **range variables**, not
  only parameters. The disclosure in §7.2 is extended; the gap itself is
  not closed (it needs a change to a guard stable since `81eb8d41`).
- **L2** — the type-resolving scanner is not shipped, so §1.3's numbers
  cannot be reproduced from the PR alone. §1.3 now says so.
- **L3** — the atomic review path has **no retry budget at all**. Recorded
  in §10.1; not fixed here, because the fix is a production change.
- **L4** — the CLI twin of four converted sites still loses the write. This
  was already disclosed in §7's next-slice paragraph; the review confirms
  it is not a defect of this slice.
- **Note** — a **pooled** `ATTACH` whose retry can land on a different
  connection. Recorded in §10.2.

The review also independently confirmed the slice's central claims: that the
23 corrections are durable, idempotent under retry, preserve their error and
transaction semantics, and write to the same pools; that the idempotency
premise holds (it tested it directly, with its own lock holder, rather than
accepting the assertion); and that both of the CI fixes in the second commit
are sound and pre-existing.

**What this branch does not do:** it does not re-open the production layer,
and it does not expand into L1–L4. The independent re-review of this
branch is the gate.

