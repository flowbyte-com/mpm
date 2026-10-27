# Session A — P5: the durable `mpm-event/0.1` ingest

**Status:** proposed 2026-10-27. Storage decision made; server implementation
follows C's confirmation of the request/receipt contract.

This is the implementation half of `docs/dev/event-contract.md` §3.10, against
the storage question §6.1 left open. It is deliberately the smallest thing
that satisfies the contract: **one new table, one new tool action, one new
counter action, no new persistence for counters.**

---

## 1. The storage decision

§6.1 asked: *where do lifecycle events live?* and answered *"revisit only
against a **measured** gap."* The gap is now measured — P4 (`#119`, merged as
`d3405d6`) produces real `mpm-event/0.1` envelopes with strict per-event
identity, and there is nowhere for them to go.

### 1.1 Every existing candidate, and why each is wrong

| Candidate | Why it cannot hold a lifecycle event |
|---|---|
| `tool_invocations` | `result_status TEXT NOT NULL CHECK (result_status IN ('success','error'))` — the envelope's `result_status` vocabulary is `success｜error｜unknown`, and `unknown` is **not** permitted (§6.10 relies on exactly this: "`unknown` is envelope-only and never enters the `tool_invocations` CHECK constraint"). Its `id` is a fresh UUID, not the event identity, and it has no `event_id`, `event_type`, `observed_at_ms` or `host_seq` column. It is the substrate's audit of **its own dispatch** — §4 is explicit that `tool.outcome` is *"distinct from MPM's own `tool_invocations` row: the former is the host's observation, the latter is MPM's audit of its own dispatch."* |
| `evidence` | `artifact_id TEXT NOT NULL` + `artifact_type TEXT NOT NULL CHECK (artifact_type IN ('memory','theory','decision','lesson','work'))` — there is no lifecycle-event artifact type. It also carries `strength` and `independence_factor`: **epistemic** semantics. An observation is not evidence, and storing one as evidence would let a single un-corroborated observation acquire a confidence weight it never earned. |
| `system_audit_log` | the substrate's own operational log. `event_code` is a bounded ASCII identifier and `level` is a fixed enum; there is **no unique event identity** and therefore **no dedup**, and no distinction between "observed at" and "ingested at". Conflating "the host did something" with "the substrate did something" in one table would make the audit trail lie about its own scope. |
| `work_events` | scoped to a work item (`work_id`). Lifecycle events are not work-item-scoped. |
| `capability_events` | scoped to a capability (`capability_id`, `invocation_id`). Same objection. |
| `memories` / `raw_memories` | a knowledge surface. §3.9 rule 2: *"Capturing an observation creates no memory, lesson or decision row."* Writing observations here would be automatic promotion — the exact thing M5 forbids. |
| `audit_cluster_proposals` | stores `message_hash` + `count`, no content. A cluster surface, not an event store. |

**Conclusion: no existing table preserves lifecycle semantics and strict event
identity without distortion.** §6.1's condition — *"one PR in
`internal/core/schema.go` with its contract tests"* — is met.

### 1.2 The migration

**One narrow, core-owned table: `lifecycle_events`.**

Mechanism: `internal/core/schema.go` → `BaseTables` (the DDL) and
`CommonIndexes` (the indexes). Every statement is `CREATE TABLE IF NOT
EXISTS` / `CREATE INDEX IF NOT EXISTS`, and `BaseTables` is executed on every
`DatabaseManager` init (`internal/core/db.go:2565`), so the table appears on
the next open of any workspace — **existing and new alike** — with no data
migration, no backfill and no downtime.

`SafeMigrations` is **not** used: it is a `[3]string` of
`{table, column, type}` triples for **adding columns to existing tables**, and
this change adds a table, not a column. There is no existing row to migrate,
so there is nothing for a column-level migration to do.

The table lives in `BaseTables`, not `CommonIndexes`, so that the shared-schema
propagation loop (`db.go:1770`) creates it in the `shared` attachment too —
consistent with every other core table.

### 1.3 The DDL

```sql
CREATE TABLE IF NOT EXISTS lifecycle_events (
    -- STRICT EVENT IDENTITY (§3.5). The primary key IS the dedup
    -- mechanism: a repeated strict-mode event_id is a no-op at the
    -- storage level, which is exactly the guarantee §3.5 claims
    -- ("at most one stored row per event_id").
    event_id             TEXT PRIMARY KEY,

    -- The envelope's own idempotency mode. It is stored, not inferred:
    -- a reader must be able to tell a content-derived id from a
    -- random one without re-deriving it.
    idempotency          TEXT NOT NULL CHECK (idempotency IN ('strict','best_effort')),

    schema_version       TEXT NOT NULL,
    event_type           TEXT NOT NULL,
    observation          TEXT NOT NULL CHECK (observation = 'observed'),
    trust                TEXT NOT NULL CHECK (trust = 'observation'),

    -- Observation order and ingestion order are DIFFERENT (§3.6) and
    -- are never interchangeable. `ingested_at_ms` is NEVER an ordering
    -- key for "what happened".
    observed_at_ms       INTEGER NOT NULL,
    ingested_at_ms       INTEGER NOT NULL,
    host_seq             INTEGER,

    host                 TEXT NOT NULL,
    host_version         TEXT NOT NULL,
    source               TEXT NOT NULL,
    adapter_version      TEXT NOT NULL,

    project_id           TEXT,
    project_id_source    TEXT NOT NULL,
    framework_name       TEXT NOT NULL,
    framework_session_id TEXT,

    operation            TEXT NOT NULL CHECK (operation IN ('present','absent')),
    result_status        TEXT NOT NULL CHECK (result_status IN ('success','error','unknown')),
    outcome_class        TEXT,
    outcome_code         TEXT,

    -- The ONLY field bag in mpm-event/0.1 (§3.8), stored verbatim as
    -- the adapter emitted it. The core re-validates the allowlist and
    -- the bounds on the way in; it never rewrites a value.
    meta                 TEXT NOT NULL DEFAULT '{}'
);
```

**Deliberate omissions, and why each is right:**

* **No `payload` column.** The envelope's `payload` is *always null* in 0.1
  (§3.8). A permanently-null column is persisted fiction and fails the
  Projection Test in `AGENTS.md` §2 — it cannot be computed from
  authoritative state (it is always null) and it persists a value the
  contract reserves for a future version. When 0.2 needs it, it is one
  `SafeMigrations` column addition.
* **No `event_type` CHECK.** The vocabulary is closed (§4) and validated in
  **Go**, where the error can name the offending value. A SQL CHECK would
  make a minor version bump (§4: *"Adding a type is additive and minor"*)
  require a table rebuild — a migration for a vocabulary change that the
  contract says is additive.
* **No `outcome_class` CHECK.** The existing nine-value
  `internal.ToolOutcomeClass` enum is reused verbatim (§3.7, M7). It is
  validated in Go, exactly as `tool_invocations.outcome_class` is — the
  table's own comment says *"The CHECK only enforces a vocabulary on
  `outcome_class`; codes are free-form within their class"*, and this
  table follows the same rule rather than inventing a second one.
* **No `created_at`.** It would duplicate `ingested_at_ms` at second
  resolution.
* **No `updated_at`.** The table is append-only; §3.8 forbids pruning, so
  no row is ever rewritten.

### 1.4 The index plan

| Index | Columns | Serves |
|---|---|---|
| `idx_lifecycle_events_order` | `(observed_at_ms, (host_seq IS NULL), host_seq, event_id)` | **the contract's total ordering key** (§3.6) |
| `idx_lifecycle_events_session` | `(framework_session_id, observed_at_ms)` | "what did this host session do", and the per-session read |
| `idx_lifecycle_events_type` | `(event_type, observed_at_ms DESC)` | the counters' `GROUP BY event_type`, and "what of this type happened" |

The ordering index deserves a note. §3.6 fixes the read order as
`(observed_at_ms ASC, host_seq ASC, event_id ASC)` **with `host_seq` NULL
sorting *after* all non-NULL `host_seq` values at the same `observed_at_ms`**,
and `event_id` compared as an ASCII string.

SQLite sorts NULLs **first** in `ASC` order, so a plain
`(observed_at_ms, host_seq, event_id)` index would return the opposite order.
The index therefore carries the `host_seq IS NULL` term explicitly, and the
read uses the identical expression list, so SQLite matches the index
exactly:

```sql
ORDER BY observed_at_ms ASC, (host_seq IS NULL) ASC, host_seq ASC, event_id ASC
```

Verified: `EXPLAIN QUERY PLAN` reports `SCAN … USING COVERING INDEX
idx_lifecycle_events_order`.

`host_seq` is nullable because no measured host supplies a monotonic counter
yet (§3.6: *"refined by `host_seq` when the host provides a monotonic
counter"*). The column exists so that §3.6's key is honoured verbatim rather
than approximated, and so a future host that does supply one needs no
migration.

---

## 2. The read-backed receipt

§6.4 is explicit: *"The receipt's new-or-duplicate determination must come
from a read-back of the stored row, not from the `INSERT` result"* — the
Substrate Defense Triad rule 3 (`AGENTS.md` §3).

The mechanism:

```sql
INSERT INTO lifecycle_events (…) VALUES (…)
ON CONFLICT(event_id) DO NOTHING
RETURNING event_id, ingested_at_ms
```

* **A row comes back** → this statement wrote it. `RETURNING` yields the
  values **as actually stored**, so the receipt's `ingested_at_ms` is a
  read-back of the stored row, not the value the caller hoped to write.
  `duplicate = false`.
* **No row comes back, no error** → the `event_id` already existed. The
  core then issues a plain `SELECT ingested_at_ms FROM lifecycle_events
  WHERE event_id = ?` and the receipt carries the **original** stored
  timestamp. `duplicate = true`.
* **No row comes back *and* the `SELECT` finds nothing** → the insert did
  not persist and there is no pre-existing row. That is a **failure**, not a
  duplicate, and it is returned as an error. This is the Triad check: a
  silent write is never reported as success.

### Why not compare timestamps

The obvious alternative — write `now`, read back, and call it a duplicate if
the stored value differs — has two defects this design does not:

1. **It is flaky.** Two ingests of the same event inside one millisecond
   produce equal timestamps, so the second is misreported as new. A test
   that ingests twice in a row would be non-deterministic.
2. **It is wrong under concurrency.** Two processes racing on the same
   `event_id` both compute the same `now`, so the loser cannot tell that the
   winner wrote the row.

`RETURNING` sidesteps both: the write and the read-back are the same
statement, so the answer is exact by construction. `RETURNING` requires
SQLite ≥ 3.35; the bundled version is **3.51.3** (measured), and
`ON CONFLICT … DO NOTHING … RETURNING` is supported.

### The `host_call_id` rule

Two stored envelopes that share a `meta.host_call_id` are **one**
observation, not two independent corroborations. The core stores
`meta.host_call_id` verbatim for pairing and **never** uses it as an
identity, a uniqueness key, or an independence signal. This is the
core-side half of A1; the adapter half (never deriving identity from it) is
already merged in P4.

---

## 3. The counters

§3.10 step 8 requires that every rejection be **counted with a
machine-readable reason**, because *"a silent drop is indistinguishable from
a working system"*.

**No counter rows are persisted.** Both counter families are computed from
authoritative state at read time (Projection Test, `AGENTS.md` §2):

| Counter | Source | Why it is authoritative |
|---|---|---|
| `stored_total`, `stored_by_event_type`, `stored_by_schema_version` | `lifecycle_events` | the events themselves |
| `rejected_total`, `rejected_by_reason` | `tool_invocations` | the substrate's own dispatch audit — a rejected ingest **is** a failed dispatch, and it is already recorded with `result_status='error'` and a typed `outcome_code` |

The rejection counter needs no new table because the audit trail already
carries the reason: the ingest returns a **typed sentinel error**, and
`internal/core.ClassifyError` maps it to
`outcome_class='validation'` with `outcome_code` set to the contract's reason
token (`unknown_version`, `unknown_event_type`, `invalid_envelope`,
`limit_exceeded`). Both audit hooks — `cmd/mpm/audit_hook.go` and
`cmd/mpm-mcp/audit_hook.go` — already classify through that one shared seam,
so the MCP surface and the CLI surface cannot drift.

`outcome_class='validation'` is the right class: a rejected envelope is
**deliberate policy enforcement, not a substrate fault** — the same
reasoning the scanner rejections already use.

**Duplicates are deliberately not counted.** A duplicate leaves no row (the
`event_id` is the primary key), so a durable duplicate total is **not
derivable** from authoritative state. The receipt tells the caller
`duplicate: true` at the moment it matters. Persisting a counter that could
drift from the rows would be worse than not having one.

**`unsupported_host_event` is adapter-side.** The core never observes an
unmapped host event — the mapping happens in the adapter (§4, M2). C's
adapter already counts it (`counters.unsupported`) and reports it through
`summariseCounters`. The core's counter surface covers the four reasons it
can observe. This is a split of responsibility, not a gap.

---

## 4. The contract clarification P5 requires

§3.10 step 1 currently reads *"Validates `schema_version`; unrecognised →
ignore + count"*, and §3.9 rule 9 repeats it.

**For the ingest endpoint this must be "reject + count".**

The receipt is the only success signal in the protocol. If the endpoint
"ignored" an unrecognised version and returned success, the caller would
record a durable observation that was never stored — precisely the failure
mode §3.10 step 8 exists to prevent. "Ignore and count" is correct for a
**consumer** that meets an envelope of a version it does not recognise (the
adapter — which already does this) and wrong for the endpoint that **is** the
version authority.

The amendment distinguishes the two by role. It changes nothing in the
adapter's behaviour.

---

## 5. Rollback

**Rollback is a single `DROP TABLE`.**

```sql
DROP INDEX IF EXISTS idx_lifecycle_events_type;
DROP INDEX IF EXISTS idx_lifecycle_events_session;
DROP INDEX IF EXISTS idx_lifecycle_events_order;
DROP TABLE IF EXISTS lifecycle_events;
```

* **No data migration is reversed** — nothing else references
  `lifecycle_events`. No foreign key points at it, because a lifecycle event
  is an *observation*, not an artifact other artifacts depend on. (If a
  future version wants `critic.proposal` events to point at a proposal,
  that is a *reference*, not an FK: §3.9 rule 2 says capturing an
  observation creates no artifact.)
* **No other table changes.** The rollback touches exactly one table and
  three indexes.
* **The counters survive the rollback** — they are derived from
  `tool_invocations`, not from `lifecycle_events`. After the rollback,
  `event_counters` reports rejections but no stored events, which is the
  truthful state.
* **Re-applying is free.** The table's DDL is `CREATE TABLE IF NOT EXISTS`,
  so re-deploying the code recreates an empty table. There is no data to
  restore, because the table only ever held observations — and observations
  are, by §3.5, best-effort and non-replayable anyway.
* **The rollback does not touch the live workspace.** It is a statement an
  operator can run against a workspace; nothing in this PR runs it
  automatically, and nothing in this PR is deployed.

---

## 6. The regression plan

### 6.1 Storage contract

| Test | Asserts |
|---|---|
| `TestLifecycleEvents_TableIsCreatedByBaseTables` | the table and its three indexes exist after a bare `InitSchema` — the migration is self-applying |
| `TestLifecycleEvents_SchemaMatchesTheContract` | every column, its nullability, and each CHECK constraint, read from `PRAGMA table_info` / `sqlite_master` — the DDL and the contract cannot drift |
| `TestLifecycleEvents_NoPayloadColumn` | there is no `payload` column (§3.8) |
| `TestLifecycleEvents_RollbackDropsOnlyThisTable` | the rollback statements touch exactly one table |

### 6.2 Ingest contract

| Test | Asserts |
|---|---|
| `TestEventIngest_StoresAValidEnvelope` | a valid envelope is stored, and the receipt's `event_id` matches the envelope's |
| `TestEventIngest_ReceiptIsReadBackFromTheStoredRow` | the receipt's `ingested_at_ms` equals the value **in the row**, re-read independently — not the value the caller passed |
| `TestEventIngest_StrictDuplicateIsOneRowAndReportsDuplicate` | the same strict `event_id` twice → exactly **one** row, `duplicate: true`, and the receipt carries the **original** `ingested_at_ms` (§3.5, §6.4) |
| `TestEventIngest_BestEffortIdsAreNeverDeduplicated` | two `best_effort` envelopes with different ids → two rows; the same id twice → still one row (the key is the id, whatever the mode) |
| `TestEventIngest_RejectsUnknownVersion` | `unknown_version`, no row written, counted |
| `TestEventIngest_RejectsUnknownEventType` | `unknown_event_type`, no row written — **never** a nearest-neighbour mapping (§3.9 rule 6) |
| `TestEventIngest_RejectsNonNullPayload` | `invalid_envelope` (§3.8) |
| `TestEventIngest_RejectsOverLimitEnvelope` | `limit_exceeded`, and the event is **not** truncated into a valid-looking event (§3.9 rule 8) |
| `TestEventIngest_RejectsUnknownMetaKey` | a `meta` key outside the §3.8 allowlist is rejected — the allowlist is enforced at the boundary, not merely documented |
| `TestEventIngest_RejectsMetaOverItsBound` | the per-key and total `meta` bounds are enforced |
| `TestEventIngest_RejectsHostCallIDAsIdentity` | two envelopes differing only in `meta.host_call_id` are two rows; the same `host_call_id` on two rows is **one** observation, and the counters never treat them as two independent corroborations |
| `TestEventIngest_CapturesNoKnowledgeArtifact` | ingesting creates **no** memory, lesson, decision or theory row (§3.9 rule 2) |
| `TestEventIngest_RejectionIsClassifiedWithATypedOutcomeCode` | the returned error maps through `ClassifyError` to `validation` + the contract's reason token |

### 6.3 Counters

| Test | Asserts |
|---|---|
| `TestEventCounters_ReportStoredEventsByType` | the counts match the rows |
| `TestEventCounters_ReportRejectionsByReason` | the counts match the audit rows, and the reason tokens are the contract's vocabulary |
| `TestEventCounters_AreNotPersistedSeparately` | no counter table exists — the counters are computed on read |
| `TestEventCounters_DuplicatesAreNotCounted` | a duplicate does not inflate `stored_total` |

### 6.4 Non-vacuity

Every gate is proved by **removing it and watching the test fail** — the
discipline used for the P2b gates:

* remove the `ON CONFLICT … DO NOTHING` → the duplicate test fails;
* replace the read-back with the `INSERT` result → the read-back test fails;
* drop the `meta` allowlist check → the unknown-meta-key test fails;
* map an unknown `event_type` onto a neighbour → the unknown-type test fails;
* relax the `trust` CHECK → the observation-only test fails.

### 6.5 Gates

`gofmt` clean · `mpm-lint --gate` PASS · `make build` ✅ ·
`make test-race` EXIT=0 · CI green on the PR head.

---

## 7. Scope

**Not in P5:**

* No batch ingest (§6.9 stays open — one envelope per call is the smallest
  correct thing).
* No retention (§3.8 — nothing prunes an event until a policy is measured).
* No promotion of any kind.
* No change to the adapter, the critic, the scheduler, or the P2b gate.
* No deployment. The live runtime is untouched.

**The trust boundary is unchanged:** the P2b gate still refuses the critic's
automatic path at both the arming and the dispatch boundary, and the critic
still writes trusted lessons directly (D2 — that is Session B's P3, gated).
