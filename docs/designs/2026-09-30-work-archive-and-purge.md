# Work Archive and Logical Purge — Design Specification

> **Status:** Approved for implementation. Not yet implemented.
> **Date:** 2026-09-30
> **Scope:** two independent capabilities for the `mpm_work` primitive —
> (1) non-destructive lifecycle archival, (2) administrative logical
> purge. Both are additive; neither changes existing `mpm_work`
> semantics except where §3.1 explicitly widens the `list` surface.
>
> **This document is a specification, not a report.** It states
> normative decisions. Where an investigation found something that
> contradicts an earlier assumption, the finding is recorded with its
> evidence so a later reader does not re-derive it incorrectly.

**Repository:** `/home/v/workspace/projects/mpm`
**Authoritative product document:** `docs/SPEC.md` (must be updated
alongside implementation — §10).
**Contributor rules:** `CLAUDE.md` (validation gate, SQLite
invariants, test isolation).

---

## 0. Vocabulary

Three distinct operations. Conflating them was the original defect
that motivated this work.

| Term | Meaning | Destructive? | Reversible? |
|---|---|---|---|
| **lifecycle transition** | `complete` / `cancel` / `reopen` — records what happened to the work | no | `reopen` |
| **archive** | remove from operational views, preserve everything | no | yes (`unarchive`) |
| **purge** | remove the artifact from the active substrate | yes | **no** |

A work item that is `done` is still fully visible in operational
views. Archive is what takes it out of those views. Purge is what
takes it out of existence.

---

## 1. Archive model

### 1.1 Storage

`works` gains one nullable column:

```sql
ALTER TABLE works ADD COLUMN archived_at INTEGER;
```

`NULL` = not archived. **No backfill** — every existing row is
correctly non-archived by construction.

`archived_at` is a **derived projection column**, exactly like
`completed_at`. It is written by `AppendWorkEvent` in the same
transaction as the event INSERT, and is reproduced by
`RecomputeWorkProjection`. It is never written by an ad-hoc `UPDATE`.

This preserves the repository invariant stated at `db.go:5933`:
*"No UPDATE works queries outside AppendWorkEvent."*

### 1.2 Ledger events

Two new event types:

| Event | Effect on projection |
|---|---|
| `archived` | `archived_at = <event created_at>` |
| `unarchived` | `archived_at = NULL` |

Both must be added to the `event_type` CHECK constraint in **all
three** DDL sites:

- `internal/core/schema.go:766` (canonical)
- `internal/core/db.go:6946` (`migrateWorkEvents`)
- `internal/core/db.go:7054` (`migrateWorkEventsCheck` recreation)

### 1.3 Terminal-only archive

**`archive` refuses `status = 'open'`.**

```
status ∈ {done, cancelled}  → archived_at set, `archived` event appended
status = open              → REFUSE, no writes
```

The column remains **independent of `status`** — this is a guard on
the command, not a constraint on the data model. The invariant is
simply that a work item must reach a terminal state before it can
leave the operational view.

**Rationale.** Without this guard, an open item can vanish from wake
context while still being an unfinished commitment, and the only
signal is a wake context that silently omits live work. That failure
is worse than the one archive exists to fix. The supported flow is:

```
open → complete | cancel → archive
```

### 1.4 Unarchive restores visibility only

`unarchive` sets `archived_at = NULL` and **never touches `status`**.

- An item archived while `cancelled` and later unarchived returns to
  `cancelled` — **not** `open`.
- An item archived while `done` and later unarchived returns to
  `done`.
- Re-entering `open_works` requires an explicit `reopen`.

**This is the single most likely contract to regress and must be
pinned by test** (§9.1).

### 1.5 Idempotency and errors

| Condition | Result |
|---|---|
| archive an already-archived item | `success: true`, `already_archived: true`, **no new event** |
| unarchive a non-archived item | error, no writes |
| unknown work id (either direction) | error, no writes, **no work row created** |

Erroring on an unknown id follows the `SoftDeleteMemory` contract
(`memory_tools.go:783`): *"the substrate never lies about whether a
write actually happened."*

---

## 2. Status and visibility are independent axes

### 2.1 The two filters

```
status     ∈ open | done | cancelled | all     default: open      (UNCHANGED)
visibility ∈ active | archived | all           default: active    (NEW)
```

They are validated independently and neither is derived from the
other. `status` validation is untouched (`validWorkStatuses`,
`work_handlers.go:90`); `visibility` is a new enum with its own
validation.

An invalid `visibility` produces a W-010-class error
(`work_handlers.go:105`) — a clear message, never a silent zero-row
result indistinguishable from "no items exist".

### 2.2 Query semantics

| `status` | `visibility` | Meaning |
|---|---|---|
| `open` (default) | `active` (default) | open operational work — **today's default result, unchanged** |
| `all` | `active` | all operational work — the review view |
| `all` | `all` | true complete inventory |
| `done` | `archived` | archived completed work |

### 2.3 `status=all` is NOT redefined

`status=all` continues to mean "every status". Archived rows were
never in scope of that contract, because nothing could archive. For
any database containing no archived items, `status=all, visibility=active`
returns **exactly** today's result set. This is pinned by a regression
test (§9.2).

### 2.4 Rejected alternative

An earlier draft proposed a boolean `archived: true` on `list`. It is
**rejected** because a boolean cannot express the three required
states as cleanly as `visibility=active|archived|all`. A boolean
admits only "archived or not" per request, so returning the
active+archived union would require a second flag with inverted
meaning, and the two together have no single unambiguous reading.
`visibility=all` is the deliberate union of active and archived work;
`status=all, visibility=all` is therefore the true complete inventory
(§2.2).

---

## 3. Surfaces

### 3.1 Excluded when `visibility = active` (the default)

Add `AND archived_at IS NULL` to each of these:

| Site | Surface |
|---|---|
| `wake_context.go:1544` `gatherOpenWorks` | wake `open_works` |
| `wake_context.go:1608` `gatherCompletedWorks` | wake `completed_works` |
| `contextual_focus.go:342` `gatherActiveWorkIDs` | focus structural input |
| `contextual_candidates_sources.go:41` `addWorkCandidates` (open scan) | contextual routing |
| `db.go:5734` `ListWorks` | legacy default-open path |
| `db.go:5772` `ListAllWorks` | backs `status=all` |
| `cmd/mpm/handlers_session.go:364` | session-surface completed refs |

**The risk this list exists to prevent:** a partial filter. If
`gatherOpenWorks` is filtered but `gatherCompletedWorks` is not, an
archived item leaks into exactly one surface. **One test per site**
(§9.2), not one integration test.

### 3.2 Never filtered — explicit by-id access

Archived work remains fully reachable once its id is known:

`GetWork` (`db.go:5517`) · `mpm_work show` · `mpm_work history` ·
`evidence_tools.go:80` · `mpm why` (`service_why.go:473`) ·
`contextual_candidates_sources.go:87` (explicit `q.WorkIDs`) ·
`:803` `detectKind`

The explicit-`WorkIDs` case matters: when a caller names a work id,
that is already a deliberate query. Filtering it would make routing
behave differently depending on how the item was reached.

### 3.3 Unchanged surfaces

- **`recent_activity`** — derived from `tool_invocations` joined to
  `work_events` by `invocation_id` (`recent_activity.go:533`). The
  ledger is immutable; an archived item's *past* activity should
  remain visible. The archive/unarchive invocations themselves
  appearing in the feed is correct.
- **contextual candidate state is not materialized.** There is no
  `contextual_candidates` table (verified against `sqlite_master`);
  routing state is computed per query. There is no cached projection
  to invalidate on archive.
- **FTS** — there is no `works_fts`. Nothing to reindex.
- **`status` enum and the F-B1 state machine** — untouched.

### 3.4 Indexes

None required. `idx_works_status_updated(status, updated_at DESC)`
still serves the wake query and SQLite filters the new predicate from
the same index scan. A partial index
(`WHERE archived_at IS NULL`) is **deferred** — add only if measured,
per `CLAUDE.md` §5.

### 3.5 Interaction with verification

None. `DeriveWorkVerification` (`db.go:6293`) reads `status`, not
`archived_at`. Archive must not touch `verification`. Cancelling
locks verification below `verified`; archiving is orthogonal to that.

---

## 4. Archive — actions and schema

### 4.1 `mpm_work` actions

```
archive    params: work_id (required), note (optional)
unarchive  params: work_id (required), note (optional)
```

Both accept the `id` alias, matching `complete`
(`work_handlers.go:172`).

`list` gains `visibility` (string enum, default `active`).

### 4.2 CLI

```
mpm work item archive   <work_id> [--note <text>]
mpm work item unarchive <work_id> [--note <text>]
mpm work item list [--status <s>] [--visibility <v>] [--limit <n>]
```

### 4.3 Wire shape

`Work` (`work.go:47`) and `WorkRowToMap` (`work_rows.go:61`) gain:

```go
ArchivedAt *int64 `json:"archived_at,omitempty"`
```

`omitempty`, mirroring `completed_at` exactly. Absent on the wire
means not archived — one source of truth, no second boolean.

### 4.4 Registry and help strings

- `registry_list.go:744` — action enum gains `archive`, `unarchive`
- `registry_list.go:746+` — two new `oneOf` branches
- `work_handlers.go:68` — hardcoded action list in the error string
- `cmd/mpm/handlers_work.go:515` — hardcoded subcommand list
- `cmd/mpm/handlers_work.go:727+` — `printWorkItemHelp`

All five must change in the same commit, or agents receive a
"valid actions include …" string that is a lie.

---

## 5. Purge — definition

### 5.1 The normative definition

**Purge is logical removal from the active MPM substrate.**

After a successful forced purge, verified by post-write read-back
(`CLAUDE.md` §4, "Write verification"):

- no `works` row for the id
- no `work_events` rows for the id
- no work-owned `evidence`, `artifact_provenance`, or
  `epistemic_provenance` rows
- no operational or read surface can retrieve the artifact — absent
  from `show`, `history`, `note`, `archive`, wake context, focus,
  contextual routing, `mpm why`, `list`
- only `work_purge_audit` metadata remains, in which no field is
  populated from the work artifact (the operator-supplied `note` is
  independent input and survives the purge)

That is the entire contract.

### 5.2 What purge explicitly does NOT guarantee

**No forensic or privacy-grade erasure from any physical copy.**
Purge makes no guarantee about:

- the bytes remaining in `mpm.db`, the WAL, a rollback journal, or
  freelist pages
- any pre-existing backup, including `backups/critic-pre/`
- Timeshift, btrfs/ZFS snapshots, LVM, or any filesystem snapshot
- user-created `mpm backup` dumps or external backup/sync systems
- content already transcribed into a handoff summary, memory body,
  or the operator's own note

**`privacy` is not a v1 use case and is not a v1 reason code** (§5.6).
Documentation must state in plain words that **MPM has no
secure-erasure capability in v1.**

### 5.3 Command surface

```
mpm work item purge <work_id>
    --reason-code <enum>
    [--note <text>]
    [--backup <path>]
    [--force]
```

**`--reason` does not exist.** `--reason-code` is an enum (§5.6).
There is no free-text `--reason` anywhere in the interface, the
schema, the help output, or the error strings.

**CLI-only.** Not exposed on `mpm_work` (MCP) and **not** on
`mpm_system` (MCP) — `mpm_system` is agent-reachable and describes
itself as *"for system health, not for daily agent work."* The
precedent is explicit in `gc_tools.go:24-28`: destructive modes
*"stay on the `mpm gc` CLI where humans can see what they're doing."*

### 5.4 Gating

| Gate | Rule |
|---|---|
| exact id | full 16-hex id; **no prefix matching**, no bulk form in v1 |
| `--reason-code` | required, non-empty, must be a valid enum member |
| dry-run | **default**; without `--force` nothing is written |
| transaction | single transaction (§5.5) |
| verification | post-write read-back that the rows are actually gone |

Dry-run output must report what *would* be deleted, every referrer
found, and the residual-exposure locations named in §5.7.

### 5.5 Delete set

| Record | Action | Reason |
|---|---|---|
| `work_events WHERE work_id = ?` | DELETE | holds `note`/`title`/`content` |
| `works WHERE id = ?` | DELETE | holds `title`/`content` |
| `evidence WHERE artifact_id = ? AND artifact_type='work'` | DELETE | work-owned |
| `artifact_provenance WHERE artifact_id = ? AND artifact_type='work'` | DELETE | work-owned |
| `epistemic_provenance WHERE downstream_id = ?` | DELETE | work-owned citation edges — see §5.5.1 |
| `tool_invocations` | KEEP | substrate execution ledger, not work-owned; stores `payload_hash`, not payload |
| `system_audit_log` | KEEP | operator justification |

Deleting `work_events` is **mandatory, not optional**: the event rows
carry the same free text as the `works` row, so a purge that left them
would defeat its own purpose.

#### 5.5.1 `epistemic_provenance` direction — corrected

An earlier draft of this design specified
`DELETE FROM epistemic_provenance WHERE source_id = ?` and a preflight
that matched `source_id = ? OR downstream_id = ?`. **Both were wrong**,
and in a way that would have left the delete unreachable. The column
names are the opposite of what they look like.

Verified from the repository, not inferred:

- **Writer** — `recordSourceCitationsNode`
  (`epistemology_tools.go:677`) takes the ids a caller is *citing* as
  `sourceIDs` and the id it just *minted* as `downstreamID`, then
  writes the former into `source_id` and the latter into `downstream_id`
  via `recordProvenanceNode`. The comment at
  `cascade_provenance.go:159` corroborates it: *"The downstream side is
  NOT resolved because callers know what they just minted."*
- **Consumer** — `discoverPositiveCascadeTargets`
  (`cascade_outbox.go:857`) resolves a foundation event with
  `WHERE ep.source_id = ?` and reads back `ep.downstream_id`, i.e. the
  artifacts affected when a foundation changes are the **downstream**
  ones. `ListDownstreamCitations` (`cascade_provenance.go:286`) filters
  on `source_id` for the same reason.

So: **`source_id` is the artifact being relied upon;
`downstream_id` is the artifact doing the relying.**

| Edge | Meaning | Purge behaviour |
|---|---|---|
| `downstream_id = work` | the work *cites* something — a citation the work made, owned by the work | **DELETE** with the work |
| `source_id = work` | another artifact *cites* the work — an inbound reference the operator must resolve | **REFUSE**, enumerate, mutate nothing |

The symmetric preflight was the worse of the two errors: because it
matched both columns, a work item's *own* outbound citation tripped the
referrer check, so every work that had ever cited anything became
un-purgeable and the `downstream_id` delete could never execute. Each
single-direction test still passed, which is why the mistake survived
review; only a fixture holding **both** edges on one work item at once
distinguishes them. `TestPurge_ProvenanceDirectionsAreAsymmetric` is
that fixture.

**No tombstone row.** A scrubbed `works` row would be the only row in
the table that `RecomputeWorkProjection` actively corrupts — a
`title='[purged]'` row with no events replays to `status='open'`. Full
DELETE is correct for this reason, in addition to being simpler.

**Projection invariant after purge:** `RecomputeWorkProjection` on a
purged id short-circuits on the existing `if !hasEvents { return nil }`
guard (`db.go:7673`) and performs no write. No special-casing is
required. Verified: `RecomputeWorkProjection` is declared on `CoreDB`
(`core.go:457`) and called from **no** non-test site, so purge cannot
race a background rebuild.

### 5.6 Audit record and reason codes

```sql
CREATE TABLE work_purge_audit (
  id           TEXT PRIMARY KEY,
  work_id      TEXT NOT NULL,
  purged_at    INTEGER NOT NULL,
  reason_code  TEXT NOT NULL
                 CHECK (reason_code IN (
                   'test_debris','accidental','corrupted',
                   'migration_cleanup','administrative','other')),
  note         TEXT,
  operator     TEXT,
  counts       JSON NOT NULL
);
CREATE INDEX idx_work_purge_audit_work ON work_purge_audit(work_id);
CREATE INDEX idx_work_purge_audit_time ON work_purge_audit(purged_at DESC);
```

`counts` example:
`{"work_events":4,"evidence":2,"artifact_provenance":1,"epistemic_provenance":0}`

**No field is populated from the purged artifact.** The table has
**no** `title`, `content`, or work-note column. No value in any column
is ever copied from the work item's title, content, notes, or event
rows — the purge path never reads them into the audit record. This is
a property of the write path, not only of the schema.

**`note` semantics.** Optional free text, **operator-authored and
independent input** — it is supplied by the operator at purge time and
is not derived from the work item in any way. It **permanently survives
the purge**.

- The `note` column is the only free-text field touching purge.
- Because it is independent operator input, it **can** contain anything
  the operator types, including text related to the purged work.
- It is **not** validated against the work content. An earlier draft
  proposed scanning it against the title/content; that was rejected as
  brittle — a paraphrase, password fragment, or unrelated secret would
  pass any such comparison, producing false assurance rather than
  assurance.
- Documentation must state: *a note is copied by the operator into a
  permanent record; purge does not remove information you manually
  place in it.*

**No `privacy` code.** A v1 purge does not offer privacy-grade or
forensic erasure (§5.2), so a code named `privacy` would be misleading
even with a disclaimer. `administrative` covers operator housekeeping.

**Schema registration:** `work_purge_audit` requires a
`CanonicalMPMSchema` entry in `sql_dump_validator.go` alongside
`works` / `work_events` to keep the `restore-db` allow-list in sync.
This is guarded by `TestCanonicalSchemaSync`
(`sql_dump_canonical_schema_test.go:22`).

### 5.7 Residual exposure — mandatory dry-run disclosure

Dry-run **must** name these as known residual locations:

- the SQLite WAL and rollback journal
- `backups/critic-pre/` — **verified automatic and rotating**:
  `SnapshotHandler` (`internal/scheduler/scheduler.go:797`) writes full
  SQLite copies on scheduler-triggered wakes and rotates at 7
  (`scheduler.go:831`). Two retained snapshots were observed on the
  development machine. **Any snapshot taken before a purge retains the
  purged content until rotation.**
- user-created `mpm backup` dumps
- filesystem-level snapshots, outside MPM's reach

### 5.8 `--backup` semantics

Explicit and opt-in. Purge **creates no backup of its own**.

`--backup <path>` writes a dump **containing the very material being
purged**; the help text must say so. No automatic deletion of any
backup is proposed — deleting backups is a far more dangerous
operation than purging one work item.

---

## 6. Preflight — structural references only

### 6.1 No cascade

There is **no `--cascade` flag** and no code path rewrites
`memories.dependencies`, handoffs, or any other substrate record.

This deliberately diverges from `ShredMemoryWithCascade`
(`memory_tools.go:632`), which *does* enqueue cascade intents on
memory shred. Memory shred is routine retention with a well-worn
cascade contract; purge is an exceptional administrative act where
silently mutating unrelated memories is the wrong default.

### 6.2 Refusal semantics

If any inbound reference exists, purge **refuses**, enumerates the
exact referrers, and **makes no changes**. The operator resolves the
references independently and retries.

### 6.3 Structural vs textual

**Structural (checked, causes refusal):**

| Source | Detection |
|---|---|
| `memories.dependencies` | `EXISTS (SELECT 1 FROM json_each(dependencies) WHERE value = ?)` — JSON membership |
| `epistemic_provenance` | `source_id = ?` **only** — an exact column match on the *cited* side, i.e. another artifact citing this work. See §5.5.1; matching `downstream_id` too would make a work item's own citation veto its own deletion |
| `evidence` citing this work from another artifact | `artifact_id = ? AND artifact_type != 'work'` |
| `cascade_outbox` already enqueued for this id | exact column match |

**Textual (NOT a reference, ignored):** the id appearing in a handoff
`summary`, a memory's free-text `content`, a work `note`, or a wake
payload. A 16-hex id occurring incidentally in prose is not a
reference; treating it as one would make purge refuse on noise.

**This distinction is the substance of the preflight.** The repository
already draws the line correctly in `discoverCascadeTargets`
(`cascade_outbox.go:543`), which resolves `memories.dependencies`
through `json_each` and reads typed provenance from
`epistemic_provenance`. Purge reuses that discipline.
**Substring search over prose is not used anywhere.**

Preflight runs **before** the deletes, in the same transaction —
following `ShredMemoryWithCascade` (`memory_tools.go:672`), discovery
must happen while the row context still exists.

---

## 7. Findings that constrain this design

Recorded so a later reader does not re-derive them incorrectly.

### 7.1 `works` is a *partial* projection — corrected premise

An earlier draft asserted that `works` is "entirely derivable from
`work_events`." **That is false.** `RecomputeWorkProjection`
(`db.go:7680`) writes exactly five columns:

```sql
UPDATE works SET status = ?, title = ?, content = ?, updated_at = ?, completed_at = ?
```

Five columns are **not** ledger-derived, and `work_events` has no
column that could derive them (verified via `PRAGMA table_info` on both
tables — `work_events` has no `session_id` column):

| Column | Actual source |
|---|---|
| `verification` | `evidence` rows via `DeriveWorkVerification` |
| `created_at` | never rewritten by replay |
| `session_id` | set at creation, never replayed |
| `migrated_at` | one-time migration marker |
| `id` | primary key |

**The conclusion is unchanged** — full DELETE is still correct — but
the correct reason is stronger than "the projection is pure":

> A deleted row satisfies the projection vacuously, and the existing
> `hasEvents` guard already handles it. A scrubbed tombstone would be
> the only row the projection actively corrupts.

`archived_at` lands cleanly on the derivable side and introduces no
new non-derivable column.

### 7.2 `secure_delete` is OFF in MPM's build — verified empirically

The system `sqlite3` CLI reports `secure_delete = 1`. **MPM's binary
does not.** `go-sqlite3` gates `SQLITE_SECURE_DELETE` behind a
`sqlite_secure_delete` build tag
(`sqlite3_opt_secure_delete.go:8`), and the Makefile passes only
`-tags fts5` with `-DSQLITE_ENABLE_FTS5=1` (`Makefile:110`).

A probe compiled with MPM's exact build flags reports:

```
secure_delete = 0
SECURE_DELETE in compile_options: 0
```

### 7.3 Byte-level consequence

Canary test through MPM's real configuration:

| Scenario | WAL after delete | main DB after close |
|---|---|---|
| plain `DELETE` | canary present | **canary present** |
| `DELETE` + checkpoint | canary present | canary present |
| `DELETE` + `secure_delete=1` | canary present | canary gone |

With `secure_delete = 0`, a deleted row's bytes are written to the WAL,
and on connection close the checkpoint folds the pre-delete page image
back into `mpm.db`. Freed space is not overwritten.

### 7.4 Consequences for this design

- **Purge does not enable `secure_delete`.** That is a
  substrate-wide write-amplification decision affecting every delete
  path including the routine `gc` decay sweep. It requires its own
  analysis.
- **Purge does not `VACUUM`.** It rewrites the entire database and
  holds a full exclusive lock; on a live substrate it would contend
  with the main daemon, the watch daemon, and the scheduler. Not
  benchmarked here; no cost claim is made.
- **Purge does not checkpoint or truncate the WAL.**
- **Purge creates no implicit backup.**

---

## 8. Test plan

All tests use `t.TempDir()` and an isolated `MPM_WORKSPACE`
(`CLAUDE.md` §3). None may resolve to the production database.

### 8.1 Archive

- refuses `status=open`; no writes on refusal
- happy paths from `done` and from `cancelled`
- sets `archived_at`; appends exactly one `archived` event at the
  correct `event_index`
- does **not** alter `status`, `completed_at`, or `verification`
- **archived-then-unarchived `cancelled` item returns to `cancelled`,
  not `open`** (§1.4 — highest regression risk)
- archived-then-unarchived `done` item returns to `done`
- idempotency both directions
- unknown id errors on both; no work row created
- `RecomputeWorkProjection` after archive preserves `archived_at`

### 8.2 Visibility

- one exclusion test per site in §3.1 (seven sites)
- inclusion tests for every by-id path in §3.2
- all four `status` × `visibility` combinations
- invalid `visibility` errors (W-010 class), not silent zero rows
- regression: `status=all, visibility=active` returns exactly today's
  result set for a database with no archived items (§2.3)
- archived item's prior activity still present in `recent_activity`
- CLI/MCP parity — `work_rows.go:33` is shared, so assert both
  envelopes agree on the archived set

### 8.3 Purge — logical contract

- dry-run is the default; writes nothing
- missing `--force` refuses; missing `--reason-code` refuses;
  invalid `--reason-code` errors
- forced purge deletes the full set in §5.5
- `tool_invocations` and `system_audit_log` survive
- `work_purge_audit` populated with correct `counts`
- **`work_purge_audit` has no title/content column** — asserted via
  `PRAGMA table_info`
- purged id is not found on every read surface in §5.1
- `RecomputeWorkProjection(purgedID)` is a clean no-op — no error, no
  row created, `works` count unchanged
- dump → restore round-trip does not resurrect the item in wake context
- transaction atomicity: an induced mid-purge failure leaves zero
  partial deletions

### 8.4 Purge — absence of side effects (correction 3)

These pin the *absence* of behaviour, so a future change cannot
silently acquire erasure semantics or a lock-holding side effect.

- **no `secure_delete` pragma is issued by the purge path**
- **no `VACUUM` is issued by the purge path**
- **no `wal_checkpoint` is issued by the purge path**
- **no backup file is created** by dry-run or forced purge; assert
  nothing new appears under `backups/`
- `SnapshotHandler` is not invoked on any purge path
- `secure_delete` reads `0` in the test build — pins the baseline so a
  future build-tag change fails loudly rather than silently

The byte-level canary experiment (§7.3) is retained as **documented
evidence and optional integration probing only.** It is deliberately
**not** a main-suite assertion: ordinary page reuse by later writes
makes such a test flaky, and asserting it would wrongly imply that
purge provides erasure. See §8.7.

### 8.5 Preflight

- a memory whose `dependencies` JSON contains the id (via `json_each`)
  → refuse, name the memory, and assert the memory's `dependencies` is
  **byte-identical** afterward
- an `epistemic_provenance` citation **into** the work
  (`source_id = work`) → refuse
- a citation the work made itself (`downstream_id = work`) →
  **must not** refuse; it is work-owned and is deleted with the work
- both edges present on one work item simultaneously → refuse on the
  inbound edge alone, and leave the outbound edge untouched
- a foreign `evidence` row citing the work → refuse
- **a prose mention in a handoff summary must NOT refuse**
- refusal writes nothing and leaves every referrer unchanged

### 8.6 Audit record

- invalid `reason_code` errors
- `note` accepted for all codes
- `counts` reflects actual deleted row counts

### 8.7 Schema guards

- `migrateWorkEventsCheck` upgrades a legacy `work_events` table
  (the `hasArchived` probe regression — §9.2)
- `TestCanonicalSchemaSync` green, including `work_purge_audit`
- `mpm help work` parity (`cmd/mpm/help_parity_test.go`)

---

## 9. Migration and schema impact

### 9.1 `works`

```go
{"works", "archived_at", "INTEGER"},
```

Nullable, `NULL` = not archived, no backfill. `works` is already in
the `addColumnIfNotExists` allow-list (`memory.go:212`); no allow-list
edit needed.

### 9.2 `work_events`

The CHECK enum gains `'archived'` and `'unarchived'` in all three DDL
sites (§1.2).

**The trap:** `migrateWorkEventsCheck` (`db.go:7025`) probes the live
DDL and early-returns when the shape looks current:

```go
if hasClaimed && hasDirective && !hasActor && !hasGit { return nil }
```

Without adding a `hasArchived` probe to that condition, **existing
databases will never be upgraded**, and the first `archive` call fails
its CHECK constraint. The table-recreation and `INSERT ... SELECT`
copy path already exists and handles the data; only the probe
predicate needs extending.

### 9.3 New table

`work_purge_audit` — fresh `CREATE TABLE IF NOT EXISTS`; see §5.6 for
the `CanonicalMPMSchema` registration.

### 9.4 No new FTS

There is no `works_fts`. Nothing to reindex.

---

## 10. Compatibility and documentation impact

### 10.1 Behavioral breaks

1. **`list --status all` narrows** — now excludes archived. `all`
   continues to mean "every status"; archived was never in scope
   (§2.3). `docs/SPEC.md:2959` must be updated.
2. **Wake context counts change** — `open_works` / `completed_works`
   shrink once anything is archived. The wake envelope is consumed by
   the managed instruction block.
3. **Seven query sites must move together** (§3.1) — a partial filter
   leaks archived items into exactly one surface.
4. **Enum/help-string updates** (§4.4) across five locations in one
   commit.

### 10.2 Not broken

Existing tests: `f2_1_completed_works_visibility_test.go` and
`wake_open_works_regression_test.go` seed no archived items, so
`NULL archived_at` leaves them passing unchanged. Unchanged: the F-B1
state machine, FTS, and every existing `CoreDB` method.

### 10.3 Documentation to update at implementation time

- `docs/SPEC.md` §work primitive and the `list` envelope (`:2959`)
- `docs/SPEC.md` — purge semantics and the explicit no-erasure
  statement (§5.2)
- `mpm help work` / `mpm work item --help`
- `mpm_work` registry description (`registry_list.go:737`) — the
  current text claims *"A work item is never truly 'done' until Git
  evidence is attached"*; verify that claim still holds before
  restating it

---

## 11. Review focus

The five things most likely to bite an implementer:

1. **The `hasArchived` probe omission** (§9.2) — a migration that
   silently does nothing on existing databases, discovered only at the
   first `archive` call in production.
2. **Partial visibility filtering** (§3.1) — seven sites, and missing
   one leaks archived work into a single surface. The
   `gatherOpenWorks` / `gatherCompletedWorks` / `gatherActiveWorkIDs`
   trio is the most likely miss.
3. **Unarchive accidentally reopening** (§1.4) — an easy slip that
   would silently re-enter `open_works` with no `reopen` event.
4. **A scrubbed tombstone creeping back in** (§5.5) — it is tempting
   and it reintroduces a projection corruption path.
5. **Purge acquiring a side effect** (§8.4) — a well-meaning
   `secure_delete`, `VACUUM`, or auto-backup would each silently
   change the lock profile, the performance profile, or the erasure
   semantics the operator was told about.

---

## 12. Explicitly out of scope for v1

- **Secure erasure** as a command. If pursued later it must be a
  separate, explicitly-named operator maintenance command, and three
  things must be answered first: whether `secure_delete` is enabled
  substrate-wide or per-connection (with write-amplification cost
  measured); whether `VACUUM` is required at all (benchmarked, not
  assumed); and what happens to `backups/critic-pre/` and user dumps.
  A command named "secure" that leaves a 15 MB snapshot containing the
  material is misleading by name.
- Bulk purge (`--all-archived` and similar).
- Scheduled or automatic archival.
- Any change to the F-B1 status state machine.
- Any change to `mpm_memory` delete/shred/restore semantics.
