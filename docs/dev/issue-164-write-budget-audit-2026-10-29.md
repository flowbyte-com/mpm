# Issue #164 — the write-budget audit

**Date:** 2026-10-29
**Base:** `origin/main` at `2727bf3c` (post-#172)
**Issue:** #164, option 3 — *"Audit every `retries=0` write call site and decide, per site, whether it genuinely cannot contend."*

## What this pass found

PR #169 corrected the write paths issue #164 named and stated that it had
audited the `retries=0` call sites. **It had not.** The claim was wrong,
and the gap was 28 write sites across the substrate.

The reason the earlier audit missed them is worth recording, because it is
the kind of mistake that recurs: the audit was a **regular expression**
over the source, and a regex cannot see two things the AST can.

1. **A retry budget inside a SQL literal.** `capability/store_invocation.go`
   inserts `cascade_invalidated) VALUES (?, ?, ?, ?, ?, ?, ?, 0)` — a `, 0)`
   inside the SQL text. A regex reads that as a zero retry budget. The
   call site's real budget is `2`.
2. **A statement whose SQL is a variable.** `handleFlushScratchpad` passes
   a `const query`, not a literal. A scan that only reads string literals
   does not see the call at all.

So the audit here is an **AST scan** (`go/parser`, `go/ast`), with
identifiers resolved to their string values before the SQL is classified.

## The method

Every non-test `.go` file under `internal/core` (the substrate, including
the nested `tools` module) is parsed. For each `ExecTracked(query, N, …)`
call:

- `N` must be the literal `0` for the site to be in scope;
- `query` is resolved — a literal, a concatenation, a `fmt.Sprintf`
  format string, or an identifier bound to a string constant or variable —
  and classified as a WRITE by its leading verb;
- the site is then checked against the exemption list.

The scan is now a standing test:
`internal/core/tx_write_budget_guard_test.go`.

## The decision rule

One rule, applied per site:

> A write is exempt from the statement-level budget **only** when an
> outer layer owns the same busy class for that path. Otherwise the
> statement layer owns it, and the budget is 5.

The rationale is issue #164's own: *"several call sites read `0` as 'this
cannot be contended', which is not true of any write."*

## The results

**28 sites. 24 given the documented budget of 5. 4 exempt.**

### The 24 that were fixed

| File | Line | The write | Why it can contend |
|---|---|---|---|
| `cascade_provenance.go` | 179 | `INSERT INTO epistemic_provenance` | The transaction's first write. |
| `cascade_provenance.go` | 205 | `INSERT INTO shared.epistemic_provenance` | The ATTACHED database carries its own write lock, independent of the local one the statement above took. |
| `confidence_maintenance.go` | 259 | `UPDATE memories SET confidence_checked_at` | The recompute above does not write when the confidence did not change, so this is sometimes the transaction's first write. |
| `db.go` | 6606 | `INSERT INTO works` | The transaction's first write. |
| `epistemology_tools.go` | 211 | `UPDATE memories SET dependencies` | The theory's dependency edges. |
| `epistemology_tools.go` | 363 | `UPDATE memories SET metadata` (theory resolve) | A theory that is resolved but not written stays pending forever. |
| `epistemology_tools.go` | 576 | `UPDATE memories SET metadata` (mark superseded) | Two live decisions. |
| `epistemology_tools.go` | 605 | `UPDATE memories SET tags` (superseded) | The tag is what stops the original being surfaced as live. |
| `epistemology_tools.go` | 620 | `UPDATE memories SET confidence` (supersede) | The ranking and the live view disagree. |
| `epistemology_tools.go` | 626 | `INSERT INTO confidence_history` (supersede) | The audit trail of the confidence drop. |
| `epistemology_tools.go` | 671 | `UPDATE memories SET metadata` (mark invalidated) | A withdrawn decision stays live. |
| `epistemology_tools.go` | 689 | `UPDATE memories SET tags` (invalidated) | The tag is what stops the withdrawn decision being surfaced as live. |
| `epistemology_tools.go` | 697 | `UPDATE memories SET confidence` (invalidate) | An invalidated decision is "no evidence". |
| `epistemology_tools.go` | 703 | `INSERT INTO confidence_history` (invalidate) | The audit trail of the confidence drop. |
| `evidence_store.go` | 216 | `UPDATE evidence SET reference_url` | Durable provenance. |
| `evidence_store.go` | 622 | `INSERT INTO confidence_history` | The history row every later recompute reads. |
| `evidence_store.go` | 851 | `UPDATE <artifact> SET confidence` | The derived value every consumer reads. |
| `gc_tools.go` | 383 | `UPDATE memories SET metadata` (stale theory) | A theory resolved but not written stays pending forever. |
| `theory_resolve_hook.go` | 156 | `UPDATE memories SET metadata` (theory resolve) | A theory resolved but not written stays pending forever. |
| `tools/handlers.go` | 5459 | `INSERT INTO ephemeral_scratchpad` (flush) | The scratchpad flush. |
| `tools/handlers.go` | 5502 | `DELETE FROM ephemeral_scratchpad` (discard) | A failed discard leaves a stale thesis behind. |
| `tools/handlers.go` | 5572 | `DELETE FROM ephemeral_scratchpad` (promote) | The promote transaction's final statement. |
| `vector_index.go` | 350 | `INSERT OR REPLACE INTO vector_assignments` | Idempotent on `(memory_id, cluster_id)`, so a retry is harmless. |
| `work_purge.go` | 556 | `INSERT INTO work_purge_audit` | The record that the purge happened. |

### The 4 that are exempt, and why

All four are in `internal/core/reference_db.go`, inside `applyReference`.

**An outer layer owns the ordinary busy class for that path.**
`IngestReference` re-drives the whole operation (attempts = 5) on an
ordinary busy error, and it re-resolves the identity each attempt, so the
loser of a concurrent ingest applies an update to the winner's row
instead of failing. The loop's own comment says so:

> The loop keeps its budget for the class it was written for: ordinary
> contention that the statement-level retry could not clear.

A statement-level budget there would multiply the two budgets — 5 × 6 =
30 transactions, where the outer budget says 5 — for no additional
resilience. That is the multiplication trap `AGENTS.md` §3 names: **the
layer that owns a busy class owns its budget.**

The exemption is keyed to the four exact SQL statements, not to the file,
so a **new** write in `reference_db.go` is caught by the guard like any
other. The reason is recorded at each call site too.

### One residual, recorded rather than fixed

`AddReference(doc, chunks)` is an exported entry point that calls
`applyReference` directly, so it has no outer loop. It has no caller in
the tree today — every live path goes through `IngestReference` — so this
is a gap in an API with no traffic rather than a live defect. It is
recorded here rather than silently left.

## Why the sites are safe to retry

A statement-level retry re-runs a statement that **failed**. A failed
SQLite write applies nothing, so the retry is a clean re-attempt, not a
double-apply. Every one of the 24 sites is additionally idempotent on
its own terms — `ON CONFLICT DO NOTHING` / `DO UPDATE`, `INSERT OR
REPLACE`, an `UPDATE`, or a `DELETE` — and the ids they use are computed
**before** the call, so a retry re-uses them rather than minting new
ones.

Inside a transaction the retry is also safe against the snapshot class:
`txNode.ExecTracked` returns `SQLITE_BUSY_SNAPSHOT` **immediately**
rather than retrying it, so the error reaches `WithTx`, which restarts the
transaction with a fresh snapshot. The statement-level budget therefore
does not compete with the transaction layer's budget — it hands the
snapshot class to the layer that owns it.

## The evidence

**Behavioural — the two tool-surface paths.** `internal/core/tools/scratchpad_write_budget_test.go`
runs the flush and the discard against a file-backed WAL database with the
production DSN, with a second pool holding the WAL write lock.

| | Before the fix | After the fix |
|---|---|---|
| `handleFlushScratchpad` | `flush scratchpad: database is locked` — the row was never written | the row lands, exactly once |
| `handleDiscardScratchpad` | `discard scratchpad: database is locked` — the stale row stayed | the row is gone |

Both assertions are on the **outcome** (the row exists / does not exist),
not on a proxy. The collision is detected through the retry counter, which
moves only *after* a failed write attempt — so the test cannot pass
vacuously by having never met the lock.

**Structural — the whole substrate.** `internal/core/tx_write_budget_guard_test.go`
fails when any write in the substrate carries a zero retry budget without a
recorded exemption. Its negative control was checked: setting the scratchpad
flush back to `0` makes the guard fail, with the site named.

## What this does not change

No behaviour beyond the retry budget. No DDL, no schema, no new table, no
new interface, no new configuration.
