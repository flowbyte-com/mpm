# Issue #178 — the ratchet must see the `ExecContext` shape

**Date:** 2026-11-03
**Base:** `fix/issue-178-cascade-materialization-idempotency` @ `08a9ff45`
**Issue:** #178 — *"90 bare pool writes still lose their artifact to ordinary lock contention"*

This is the **second** PR of issue #178's phase 3. The first is
[`issue-178-corrections-phase-3-2026-11-03.md`](issue-178-corrections-phase-3-2026-11-03.md),
which corrects the cascade materializer's state transitions and records the
full reconciliation. This PR closes a **detection** hole that the
reconciliation found: **neither the audit's scanner nor the merged ratchet can
see a write expressed as `ExecContext`.**

**Issue #178 is not closed by this work.** The scheduler's duplicate-delivery
crash window remains open by design; the first PR's record proposes the
migration plan for closing it.

---

## 1. The defect

### 1.1 The shape

```go
_, err := dm.db.ExecContext(ctx,
    `UPDATE some_table SET some_column = ? WHERE id = ?`, value, id)
```

`ExecContext(ctx, query, args...)` takes the **context first**. The query is
the **second** argument.

Both scanners read the SQL from a call's **first** argument:

| Scanner | How it finds the SQL | What it sees for `ExecContext` |
|---|---|---|
| Phase-0 audit scanner (`internal/audit/barewritescan`, unmerged branch `audit/issue-178-bare-write-risk` @ `74eac183`) | matched the method name `ExecContext`, then read `Args[0]` | `Args[0]` is `ctx` — an identifier with no literal binding — so the resolved SQL is the empty string, no write verb matches, and the site is **skipped as a non-write** |
| The merged ratchet (`internal/core/bare_pool_write_ratchet_test.go`) | matched the method name `Exec` only | the method name does not match at all, so the site is **never visited** |

The first is the more dangerous of the two, because it *looks* like it handles
the shape: it names `ExecContext` in its method list, so a reader of the
scanner would reasonably believe the shape was covered.

### 1.2 What was hidden

A dedicated sweep of every `Exec*` call in the tree found **17 write sites
that neither scanner can see** — 12 on a pool receiver and 5 on a transaction
handle:

| Site | Receiver | Consequence of a lost write |
|---|---|---|
| `internal/core/wake_tools.go:117` | pool | The cascade-summary wake. It is a *report*; the next tick produces another one. |
| `internal/core/cascade_wake_reconcile.go:108` | pool | The **atomic claim** of a wake. A failure rolls the wake back rather than stranding it. |
| `internal/core/compact.go:1019` | pool | The compaction ledger entry (`compaction.last_run`). The reader treats a missing row as "no pressure" — the safe default. |
| `internal/core/reference_db.go:349`, `:702` | pool | One reference chunk's embedding. The chunk is re-embeddable; the counter advances only on success. |
| `internal/core/active_uptime.go:223` | pool | A settling baseline. `INSERT … ON CONFLICT DO NOTHING` — idempotent by construction. |
| `internal/core/similarity_candidates_store.go:1258` | pool (a pinned `*sql.Conn`) | A `shared.contradiction_log` entry, written `INSERT OR IGNORE`. The error is propagated; the write is simply not retried. |
| `internal/blobstore/fs.go:268` | pool | The blob's metadata row. The orphan file is removed on failure. |
| `internal/blobstore/fs.go:397`, `:577` | pool | A blob row deletion. `Delete` is idempotent and returns the error. |
| `internal/blobstore/fs.go:697` | pool | An orphan row deletion in the GC sweep. **The only one of the 12 that discarded its error** — see §3.2. |
| `cmd/mpm/backfill_embeddings.go:289` | pool | A bulk embedding clear. The backfill is re-runnable. |
| `internal/core/backfill_snapshots.go:363` | transaction | Transaction-owned. |
| `internal/core/compact.go:911` | transaction | Transaction-owned. |
| `internal/core/compact_requeue.go:265` | transaction | Transaction-owned. |
| `internal/core/migration_memories_affinity_rebuild.go:560` | transaction | Transaction-owned (a migration). |
| `internal/core/migration_memories_created_at_not_null.go:443` | transaction | Transaction-owned (a migration). |

**None of the 12 bare-pool sites has a stranded-or-duplicated consequence.**
So this is a **detection** hole, not a live-defect hole — but a detector that
cannot see a write shape cannot keep that shape honest, and the shape is one
line away from every future caller. The ratchet's whole value proposition is
that a NEW bare write fails the build; a shape the ratchet cannot parse is a
shape it cannot police.

### 1.3 What the count therefore was

The phase-3 reconciliation's numbers are **lower bounds**. At `main`:

- the phase-0 scanner reported **127** bare pool writes;
- the merged ratchet allowed **124** occurrences across **122** identities.

The true bare-pool count was **139** (127 + the 12 invisible `ExecContext`
sites). The first PR's record says so explicitly, so the two PRs agree.

---

## 2. The correction — the scanner resolves the SQL from the right argument

`internal/core/bare_pool_write_ratchet_test.go`:

```go
// bareWriteMethod returns the argument index at which the SQL lives for
// a write method, and whether the method is a write method at all.
//
//	Exec(query, args...)         → Args[0]
//	ExecContext(ctx, query, ...) → Args[1]
func bareWriteMethod(name string) (sqlArgIndex int, ok bool) {
	switch name {
	case "Exec":
		return 0, true
	case "ExecContext":
		return 1, true
	}
	return 0, false
}
```

The scan then resolves the SQL from `call.Args[sqlIdx]` instead of
`call.Args[0]`.

Everything else about the classification is unchanged and was re-verified:
the pool-receiver rule (`*.db`, `*.DB`, `*.SQLDB()`, a local handle — and
`node.Tx()` is transaction-owned), the `WithTx`-callback exclusion, the
write-verb match (DML and DDL alike), and the multiset identity
(`file` + normalized SQL, with a recorded `Count`).

**The receiver rule needed no change for `ExecContext`**, and that is worth
stating: `tx.ExecContext(...)` has an `*ast.Ident` receiver named `tx`,
which the existing rule already excludes as transaction-owned. The 5
transaction-owned `ExecContext` sites are therefore still classified as
transaction-owned and are still not counted.

### 2.1 The counts after the correction

| | Before | After |
|---|---|---|
| Ratchet identities | 119 | **130** |
| Ratchet occurrences | 121 | **133** |
| Bare-pool `Exec` sites | 121 | 121 |
| Bare-pool `ExecContext` sites | **0 (invisible)** | **12** |

The 12 new sites are recorded with a reason each, exactly like the `Exec`
sites: 11 `propagated`, 1 `migration-once` (the `cmd/mpm` embedding
backfill, whose sibling entry already carries that reason).

---

## 3. The one site that discarded its error

### 3.1 The finding

`internal/blobstore/fs.go`'s `GCSweepOrphans` deleted an orphan DB row and
**dropped the error**:

```go
for _, id := range orphanIDs {
	log.Printf("blobstore: orphan DB row %q has no payload file, removing", id)
	_, err := f.db.ExecContext(ctx, `DELETE FROM blobs WHERE id = ?`, id)
	if err == nil {
		stats.OrphansDeleted++
	}
}
```

Every *other* database failure in that same function is reported — the
`exists` check logs a warning and counts `stats.OrphansSkipped++`. The delete
was the odd one out.

The consequence is mild: the row stays in the table and the next sweep
retries it, so a lost delete is a **delay**, not a stranded artifact. But a
silently discarded durable-state write is exactly the class issue #178
audits, and it was invisible only because the detector could not see the
shape.

### 3.2 The correction

The failure is now reported the same way as every other database failure in
that function — a warning and a counter — and the delete is skipped rather
than counted as deleted:

```go
if _, err := f.db.ExecContext(ctx, `DELETE FROM blobs WHERE id = ?`, id); err != nil {
	slog.Warn("blob_gc: orphan row delete failed; the row stays and the next sweep retries it",
		"id", id, "err", err)
	stats.OrphansSkipped++
	continue
}
stats.OrphansDeleted++
```

This also removes an ambiguity from the baseline: `DELETE FROM blobs WHERE id
= ?` appears **twice** in that file — at `fs.go:397` (the public `Delete`,
which returns the error) and at `fs.go:697` (the sweep). The ratchet's
identity is `(file, normalized SQL)`, so the two share one entry, and the
entry carries one reason. After this correction, **`propagated` is true of
both**, and the entry says so honestly.

---

## 4. The non-vacuity proofs

### 4.1 Synthetic controls (keep proving the SHAPE as the tree changes)

`TestBarePoolScannerFlagsRepresentativeShapes` gains five cases:

| Case | Expected |
|---|---|
| `bare dm.db.ExecContext is caught` | 1 |
| `bare dm.SQLDB().ExecContext is caught` | 1 |
| `bare local db.ExecContext is caught` | 1 |
| `tx.ExecContext inside a WithTx callback is not flagged` | 0 |
| `a read via ExecContext is not flagged` | 0 |

`TestBarePoolAllowlistDetectsReintroducedExecContextWrite` is the dedicated
control. It asserts, from both ends:

1. a synthetic `dm.db.ExecContext(ctx, "UPDATE probe_target …", …)` **is**
   detected as a bare pool write, and the detected site carries the
   **resolved SQL** rather than the context; and
2. the allowlist does **not** contain that identity, so a live
   reintroduction fails the completeness guard rather than passing because a
   detector could not see it.

The same test also checks the `Exec` spelling of the identical statement, so
the two spellings cannot hide behind each other.

### 4.2 A live mutation of the real tree (proves the guard fires)

A bare `dm.db.ExecContext(ctx, "UPDATE scheduled_wakes SET reason = ? WHERE
id = ?", …)` was injected into `internal/core/wake_tools.go` and
`TestBarePoolWriteAllowlistIsComplete` failed, naming the file, the line,
and the normalized SQL:

```
--- FAIL: TestBarePoolWriteAllowlistIsComplete (0.18s)
    bare pool writes without an allowlist entry — record each one with a reason, or route it through ExecTracked (budget 5), or move it inside a transaction. A second copy of an allowlisted statement also lands here, as surplus occurrences:
          internal/core/wake_tools.go:1206: UPDATE scheduled_wakes SET reason = ? WHERE id = ?
```

The mutation was reverted and the tree verified clean.

**Before this PR, that same injection passed the guard silently.**

---

## 5. What this PR does NOT close

- **The scheduler's duplicate-delivery crash window.** Unchanged by this PR.
  It is a process-death window, not a write-contention window, and it needs
  the delivery-state redesign proposed in the phase-3 record.
- **The 4 transaction-owned writes whose error is not propagated**
  (`cmd/mpm/handlers_gc.go:437`, `internal/core/db.go:3649`, `:4791`, `:4799`).
  They are transaction-owned: the enclosing transaction's outcome decides
  whether they land, and the FTS shadow is rebuilt by `fts_recovery` if it
  drifts. Recorded, not fixed.
- **The 121 bare-pool `Exec` sites.** They are classified and pinned. The
  ratchet's job is to stop the class from GROWING, not to remove every
  instance of it — a lower bare-write count is not, by itself, a stronger
  correctness guarantee.

---

## 6. Verification

The full gate list — `make test`, `make test-race`, `make test-scripts`,
`make docs-check`, `mpm-lint --gate`, and the release tests — is run on this
PR's exact head; the evidence is on the PR.

The ratchet's own tests are the substantive ones here:

```
TestBarePoolWriteAllowlistIsComplete                    PASS
TestBarePoolAllowlistHasNoStaleEntries                  PASS
TestBarePoolAllowlistIdentitiesAreUnique                PASS
TestBarePoolMultisetIdentitiesDetected                  PASS
TestBarePoolAllowlistDetectsReintroducedWrite           PASS
TestBarePoolAllowlistDetectsReintroducedExecContextWrite PASS
TestBarePoolScannerFlagsRepresentativeShapes            PASS  (14 sub-cases)
TestBarePoolScanReachesEveryRuntimePackage              PASS
```
