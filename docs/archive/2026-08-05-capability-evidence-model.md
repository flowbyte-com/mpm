# Capability Evidence Model

**Status:** DRAFT (proposal under critical review)
**Date:** 2026-08-05
**Precedents:** `docs/superpowers/specs/2026-08-05-skill-health-design.md`,
`docs/capability-lifecycle-spec.md`
**Read alongside:** `internal/core/capability/state.go`,
`internal/core/capability/store_lifecycle.go`,
`internal/core/capability/store_invocation.go`,
`internal/core/schema.go` (lines 331–473).

---

## Thesis (one paragraph)

The capability substrate already records every fact an operator needs to
judge trustworthiness — identity at proposal time, append-only state
transitions, immutable per-call telemetry, an append-only junction for
lineage, and typed JSON for policy snapshots. The right "evidence model"
is the substrate as it stands today, **minus the two fields that
duplicate derivable data**. No new schema. No new tables. No "health"
projection. The judgement "is this capability trustworthy?" is an
operator's reading of four deterministic views (Audit, Timeline,
Invocations, Lineage). The substrate's job ends at the views; the
operator's job starts there.

---

## 1. Evidence that already exists in the capability substrate

What follows is an inventory of facts that the substrate currently
records. **Nothing here is invented.** Every row, column, and event
already has a writer; the goal is to surface what they collectively
prove.

### 1.1 Identity (immutable from creation)

Recorded at proposal time, never changes except via `name` UNIQUE
collision (in which case the prior row is retired):

- `capabilities.id` — UUID; PK
- `capabilities.name` — slug; UNIQUE
- `capabilities.purpose` — 20–500 chars; the dedup input
- `capabilities.source_code` — the executable artifact
- `capabilities.source_language` — `bash` | `python` | `jq`
- `capabilities.source_hash` — SHA-256 of `source_code` at proposal time
- `capabilities.author_agent` — who proposed
- `capabilities.author_theory_id` — epistemic lineage pointer (FK →
  `memories(collection='theories')`)
- `capabilities.created_at` — proposal epoch

### 1.2 Lifecycle (state machine)

- `capabilities.state` — current state; CHECK constraint enumerates
  all 10 legal values
- `capabilities.execution_domain` — earned trust level
- `capabilities.state_changed_at` — epoch of last transition
- `capabilities.promoted_at` — epoch of probation→active transition
  (NULL ⇒ never promoted)
- `capabilities.probation_required_success_count` — policy stamped at
  proposal
- `capabilities.probation_max_failure_rate` — policy stamped at
  proposal
- `capability_events` rows — append-only log of every transition
  (see §1.6)

### 1.3 Execution (telemetry + cached counters)

- `capability_invocations` rows — per-call immutable record
  (see §1.4)
- `capabilities.success_count` — `+1` on `exit_code=0` (cached)
- `capabilities.failure_count` — `+1` on `exit_code≠0` (cached)
- `capabilities.fracture_count` — `+1` per fracture transition
  (cached; written by `MarkFractured`)
- `capabilities.last_invoked_at` — most recent `capability_invocations.invoked_at`
- `capabilities.last_failure_at` — most recent non-success invocation
- `capabilities.last_failure_stderr` — denormalized stderr from the
  most recent failure (limit N bytes per executor truncation)
- `capabilities.avg_latency_ms` — **recomputable from
  capability_invocations; *candidate for deletion* (see §5)**
- `capabilities.updated_at` — last write epoch
- `capabilities.deleted_at` — soft-delete (rare; capabilities
  normally retire, not delete)

### 1.4 Per-invocation record (`capability_invocations` row)

Inserted by `Executor.Invoke` via `Store.RecordInvocation`, in the
same transaction as the counter update. Immutable from that point:

- `id` (UUID)
- `capability_id` (FK)
- `invoked_at` (epoch seconds; the Executor's clock)
- `exit_code` (process reported)
- `duration_ms`
- `stderr` (nullable, truncated at `ResourceLimits.MaxOutputBytes`)
- `invocation_context` (JSON; schema-versioned with
  `language`, `truncated`, `args_hash`, `env_keys`, `driver_name`,
  `kind`)
- `cascade_invalidated` (0/1)

**Invariant the substrate pins** (EX-7): `success_count + failure_count ==
COUNT(*) FROM capability_invocations WHERE capability_id = ?`. A write
that breaks this invariant rolls back the entire transaction. The cached
counter IS the count, by construction.

### 1.5 Lineage (FK graph)

- `capabilities.created_from_id` — FK to the revision parent (NULL for
  initial proposals)
- `capabilities.superseded_by_id` — FK to the successor (shadow flag
  during probation; non-NULL once retired)
- `capability_dependencies` rows — junction `(capability_id,
  depends_on_id, added_at)`; powers the recursive cascade walkers

### 1.6 Append-only event log (`capability_events`)

Written by every transition method via `insertTransitionEventTx`, in
the same transaction as the row update. Immutable from that point.
The substrate has 9 event_type values — see §2.5 for the taxonomy:

```
event_type           | from-state | to-state    | actor written
---------------------|------------|-------------|---------------
promotion            | varies     | varies      | forge|scheduler|operator
demotion             | varies     | varies      | scheduler|operator
fracture             | degraded   | fractured   | scheduler
rollback             | active     | rolled_back | scheduler
dependency_shatter   | (n/a)      | (n/a)       | scheduler
retirement           | varies     | retired     | operator|scheduler
supersede            | (n/a)      | (n/a)       | scheduler
source_hash_mismatch | active     | retired     | executor (synthetic)
operator_approval    | (n/a)      | (n/a)       | operator
```

Each row also carries `reason` (operator-supplied or auto-generated),
`related_id` (the predecessor on rollback, the dead dep on shatter),
and a JSON `metadata` blob for typed-event-specific fields.

### 1.7 Typed metadata keys already in use

The substrate already shapes the `metadata` JSON via typed keys:

- `policy_snapshot` — stamped at proposal; probation policy with the
  values in force when the capability was proposed
- `baseline_at_proposal` — predecessor's success/failure/latency
  snapshot for observation-window comparison
- `operator_notes` — operator-supplied flagging note (MarkNeedsRevision)
- `failure_trace` — most recent stderr (MarkFractured stamps this too)
- `operator_approved_at` — int64 epoch for `EventOperatorApproval`
  (the executor gate; readable at invoke-time via
  `metadata.int64FromMeta`)

### 1.8 Exogenous evidence that may apply to a capability

These tables are not capability-specific but a capability's `id`
appears in them when:

- **`system_audit_log`** — `component='capability'` rows for runtime
  anomalies; correlation via context JSON
- **`epistemic_cascade_outbox`** — written when a fracture has
  `author_theory_id` set; the cascade materializer walks this
- **`epistemic_provenance`** — a capability row can be the `source_id`
  for a decision or memory that cites it
- **`memories(collection='theories')`** — `author_theory_id` target

These are visible to projections via standard `JOIN`s; the capability
subsystem does not own or denormalize them.

---

## 2. Evidence categories

The eight categories in the prompt collapse into **seven lenses over
the substrate tables**, not eight new structures. Each lens is a SQL
predicate or join; nothing here is its own table.

| Category | Question it answers | Lens (i.e. WHERE clause or JOIN) |
|---|---|---|
| **Identity** | What is this? | `capabilities WHERE id=?` (immutable columns) |
| **Lifecycle** | Where is it in its state machine? | `capabilities.state` (current); `capability_events` (history) |
| **Execution** | What has it done recently? | `capability_invocations WHERE capability_id=? ORDER BY invoked_at DESC` |
| **Validation** | Was its source vetted? | `capability_events.event_type='promotion' AND from_state='linted'` plus `source_hash` (the source_hash IS the dry-run's frozen sha) |
| **Recovery** | Was it ever restored? | `capability_events.event_type IN ('rollback','dependency_shatter','recovery_event')` |
| **Governance** | Who decided what, and why? | `capability_events.actor` + `event.reason` + `event.metadata` |
| **Scheduler** | What did automated observers do? | `capability_events.actor='scheduler'` |
| **Operator** | What did humans decide? | `capability_events.actor='operator'` |

**Why each category exists** — each lens addresses a distinct operator
question that the underlying evidence already supports, but only when
filtered:

- Identity — "is this the capability I think it is?" (no source
  drift, no rename without audit trail)
- Lifecycle — "is it allowed to be invoked right now?" (gate; the
  CheckProbationCriteria + IsCallable answer)
- Execution — "how has it actually performed in the wild?"
- Validation — "did anyone review this code, and is the code on disk
  still the code that was reviewed?"
- Recovery — "did anything go wrong, and what fixed it?"
- Governance — "who's accountable?" (per-row `actor` + `reason`)
- Scheduler / Operator — split Governance by *origin of the action*,
  because the substrate's authority model distinguishes them

**Eight requested categories** reduces to these seven, plus
**Lineage** (FK graph) as a separate join lens that no prompt category
covers but the substrate obviously supports.

A category that DOESN'T exist above: **"Trust"**. Trust is `state' +
execution_domain` plus the empirical metrics, and it is not a
lens — it is a *current state*, observed via the Lifecycle and
Execution lenses combined. Trust has no storage of its own; reading
the substrate for it requires both lenses at once.

---

## 3. Per-fact property table

For every distinct piece of evidence above, what does the substrate do
with it?

| Fact | Origin (writer) | When created | Immutable? | Append-only? | Recomputable? | Where it lives |
|---|---|---|---|---|---|---|
| `capabilities.id` | `forge.go` (UUID gen) | proposal | yes | n/a (single row) | no — random | row PK |
| `capabilities.name` | proposal intake | proposal | yes | n/a | no | row column, UNIQUE |
| `capabilities.source_hash` | `InsertCapabilityProposal` (sha256) | proposal | yes (until force-overwrite) | n/a | yes — recomputable from source_code | row column |
| `capabilities.source_code` | proposal intake | proposal | yes | n/a | n/a | row column |
| `capabilities.purpose` | proposal intake | proposal | yes | n/a | n/a | row column |
| `capabilities.author_agent` | proposal intake | proposal | yes | n/a | n/a | row column |
| `capabilities.author_theory_id` | proposal intake | proposal | yes (FK SET NULL on delete) | n/a | no | row column |
| `capabilities.created_at` | proposal intake | proposal | yes | n/a | no | row column |
| `capabilities.state` | transition methods | every transition | no (CHECK enforced) | n/a (current value only) | yes — `SELECT to_state FROM capability_events WHERE capability_id=? ORDER BY occurred_at DESC LIMIT 1` | row column |
| `capabilities.execution_domain` | proposal intake + `grant-operator` | proposal / operator override | yes (post-proposal) | n/a | no | row column, CHECK enforced |
| `capabilities.state_changed_at` | every transition method | every transition | no | n/a | yes — same as `state` | row column |
| `capabilities.promoted_at` | `PromoteToActive` | probation→active | yes (once set) | n/a | yes — same | row column |
| `capabilities.probation_required_success_count` | proposal intake | proposal | yes (policy snapshot) | n/a | no | row column |
| `capabilities.probation_max_failure_rate` | proposal intake | proposal | yes (policy snapshot) | n/a | no | row column |
| `capabilities.success_count` | `RecordInvocation` | every invocation | no (concurrent increment) | n/a | yes — `SELECT COUNT(*) WHERE exit_code=0` | row column, **cached** |
| `capabilities.failure_count` | `RecordInvocation` | every invocation | no | n/a | yes — `SELECT COUNT(*) WHERE exit_code≠0` | row column, **cached** |
| `capabilities.fracture_count` | `MarkFractured` | every fracture | no | n/a | yes — `SELECT COUNT(*) FROM capability_events WHERE capability_id=? AND event_type='fracture'` | row column, **cached** |
| `capabilities.last_invoked_at` | `RecordInvocation` | every invocation | no (overwrites) | n/a | yes — `SELECT MAX(invoked_at)` | row column, **cached** |
| `capabilities.last_failure_at` | `RecordInvocation` | every failed invocation | no (overwrites) | n/a | yes — `SELECT MAX(invoked_at) WHERE exit_code≠0` | row column, **cached** |
| `capabilities.last_failure_stderr` | `RecordInvocation` / `MarkFractured` | every failed invocation or fracture | no (overwrites) | n/a | yes — most recent stderr from capability_invocations | row column, **cached, truncated** |
| `capabilities.avg_latency_ms` | `RecordInvocationOutcome` | every invocation | no (running mean) | n/a | yes — `SELECT AVG(duration_ms)` | row column — **CANDIDATE FOR DELETION** (§5.1) |
| `capabilities.updated_at` | every UPDATE | every write | no | n/a | yes | row column |
| `capabilities.deleted_at` | manual delete | rare | yes (once set) | n/a | no | row column |
| `capabilities.metadata` | proposal + transition methods + grant-operator | multiple | mostly yes (typed keys) | yes (typed keys) | no — typed keys are author-time | row JSON column |
| `capability_invocations.*` (every row) | `Executor.Invoke` → `RecordInvocation` | every invocation | **yes** | **yes (append-only)** | n/a (already immutable) | separate table, indexed `(capability_id, invoked_at DESC)` |
| `capability_events.*` (every row) | `insertTransitionEventTx` | every transition | **yes** | **yes (append-only)** | n/a (already immutable) | separate table, indexed `(capability_id, occurred_at DESC)` + `(event_type, occurred_at DESC)` + `actor` |
| `capability_dependencies.*` (every row) | `AddDependency` | proposal / revision | no (CASCADE on delete) | yes (intent) | yes — derives the recursive CTE | junction table |
| `system_audit_log` rows where `component='capability'` | `LogAudit` from executor / scheduler / forge | various | yes | yes | n/a | substrate-wide table |

**Observations on this table:**

1. **No row is currently used as a cache that disagrees with its
   source**. Every cached column has a recompute formula and is in
   the same transaction as the source write. This is the invariant
   the 2026-07-07 audit's "no caches that diverge from source" rule
   depends on.

2. **The substrate holds append-only sources where it must**
   (capability_invocations, capability_events) and cached counters
   only where re-derivation is too expensive on a hot path
   (CheckProbationCriteria reads success_count on every transition).

3. **Metadata JSON columns serve as the typed extension points**
   — they carry structured records (policy_snapshot, baseline_at_proposal,
   operator_notes, failure_trace, operator_approved_at) that the rest
   of the substrate does not column-ize.

---

## 4. Minimum persistent schema (delete-not-add)

This section is a **deletion list**, not an addition list. The
principle: any fact that is deducible from another fact at acceptable
cost should not have its own column.

### 4.1 Net change to the schema: **−1 column, 0 tables**

#### Deletion: `capabilities.avg_latency_ms`

**Why it's deletable.** The cached column can be replaced by a
sub-millisecond indexed aggregation over `capability_invocations`:

```sql
SELECT AVG(duration_ms) FROM capability_invocations WHERE capability_id = ?;
```

The `idx_invocations_capability_time (capability_id, invoked_at DESC)`
index already serves this query. The running-mean update logic in
`RecordInvocationOutcome` (lines 304–313 of `store_lifecycle.go`)
exists only to maintain this column; removing the column removes
~12 lines of code, the `(c.AvgLatencyMs*float64(total) + float64(latencyMs))/float64(newTotal)`
arithmetic, and the test surface that pins that arithmetic.

**Why the cache existed in the first place is itself the reason to
delete it.** MPM was designed for SQLite-scale data, where an indexed
average on tens of millions of rows would be slow; on capability-scale
data (high-write for one capability, low total), an indexed `AVG()`
per audit call is cheaper than the maintenance cost. **The cache
solves a problem that the workload doesn't pose.**

**Risks of deletion:**

- `CheckProbationCriteria` reads `SuccessCount` and `FailureCount` but
  NOT `AvgLatencyMs` — no functional change.
- The observation window baseline in §5.2 of the capability spec uses
  `baseline_at_proposal` (proposal-time snapshot, not current), so
  the live value isn't on the rollback path.
- The seed primitive `capability_health` surfaces `avg_latency_ms`;
  deleting the column means re-deriving it at primitive time. **Two
  lines of primitive change.** The primitive becomes more honest
  about what it's aggregating.

**Action:** remove the column. Migrate via the `SafeMigrations`
slice in `schema.go`: `{"capabilities", "avg_latency_ms",
"DROP COLUMN"}` in a future migration. (SQLite supports `ALTER TABLE
… DROP COLUMN` from 3.35.0; verify the bundled driver.)

#### What I am explicitly NOT adding

I refuse to add:

- A `health` column. **There is no computation here that is not
  already a query over evidence.**
- A `health_score` view. Same reason.
- An `evidence_summary` projection table. Counts are derivable;
  drift is observable via existing FK + state machinery; the cached
  counters already exist.
- A `last_audit_at` column. Auditing is `event.occurred_at`; a column
  that says "someone looked at this" is fiction.
- An `operator_pin` flag. Operators retire capabilities they want to
  kill; they don't pin them to stay alive forever.

---

## 5. Projections

A projection is a **read-time assembly of evidence**, deterministic,
reproducible, free of LLM calls, free of cached state. The
projection table is a contract for tool authors, not for the
substrate: the substrate writes no projection rows; tools read
directly from the source tables.

The principle below each projection: **a projection is a SQL
template**, not a Go type, not a JSON snapshot, not a row in a new
table. If a projection requires code beyond the SQL, that's a sign
the projection is doing more than reading.

### 5.1 Capability Audit

**Purpose:** complete evidence dump for one capability. Powers
`mpm capability audit <id>` and ad-hoc forensics.

**Inputs:** `capabilities`, `capability_invocations`, `capability_events`,
`capability_dependencies`.

**Template:**

```sql
-- Identity + Lifecycle (one row)
SELECT id, name, purpose, source_language, source_hash,
       author_agent, author_theory_id, created_at, updated_at, deleted_at,
       state, execution_domain, state_changed_at, promoted_at,
       probation_required_success_count, probation_max_failure_rate,
       success_count, failure_count, fracture_count,
       last_invoked_at, last_failure_at, last_failure_stderr,
       tags, metadata
FROM capabilities
WHERE id = ? AND deleted_at IS NULL;

-- Execution telemetry (capped; e.g. last 100)
SELECT id, invoked_at, exit_code, duration_ms, stderr, cascade_invalidated
FROM capability_invocations
WHERE capability_id = ?
ORDER BY invoked_at DESC
LIMIT 100;

-- Lifecycle event log (full; CHECK the count)
SELECT id, event_type, occurred_at, actor, from_state, to_state,
       reason, related_id, metadata
FROM capability_events
WHERE capability_id = ?
ORDER BY occurred_at ASC;

-- Lineage (dependency graph, depth-bounded)
WITH RECURSIVE upstream AS (
    SELECT capability_id, depends_on_id, 1 AS depth
    FROM capability_dependencies WHERE capability_id = ?
    UNION ALL
    SELECT cd.capability_id, cd.depends_on_id, u.depth + 1
    FROM capability_dependencies cd
    JOIN upstream u ON cd.capability_id = u.depends_on_id
    WHERE u.depth < 5
)
SELECT u.depth, u.depends_on_id, c.name, c.state, c.execution_domain
FROM upstream u JOIN capabilities c ON c.id = u.depends_on_id
ORDER BY u.depth;

WITH RECURSIVE downstream AS (
    SELECT capability_id, depends_on_id, 1 AS depth
    FROM capability_dependencies WHERE depends_on_id = ?
    UNION ALL
    SELECT cd.capability_id, cd.depends_on_id, d.depth + 1
    FROM capability_dependencies cd
    JOIN downstream d ON cd.capability_id = d.depends_on_id
    WHERE d.depth < 5
)
SELECT d.depth, d.capability_id, c.name, c.state, c.execution_domain
FROM downstream d JOIN capabilities c ON c.id = d.capability_id
ORDER BY d.depth;
```

**Properties:**

- **Deterministic.** Same inputs → same output.
- **Reproducible.** Any operator with read access can re-run it.
- **No LLM call.** Pure SQL.
- **No cache.** SQLite plan cache is invisible to users.

### 5.2 Capability Timeline

**Purpose:** chronological event log only. Powers
`mpm capability history <id>` and `mpm capability list --recent`.

**Inputs:** `capability_events`.

**Template:**

```sql
SELECT id, event_type, occurred_at, actor, from_state, to_state,
       reason, related_id
FROM capability_events
WHERE capability_id = ?
ORDER BY occurred_at ASC;
```

**Note:** the JSON `metadata` field is excluded from the default view
because typed event metadata varies per event_type; surface on demand
with a `--with-metadata` flag.

### 5.3 Capability Invocations

**Purpose:** chronological execution log. Powers
`mpm capability invocations <id>` and the fracture-detector's sliding
window.

**Inputs:** `capability_invocations`.

**Template:**

```sql
SELECT id, invoked_at, exit_code, duration_ms,
       CASE WHEN stderr IS NULL THEN '...' ELSE substr(stderr, 1, 200) END AS stderr_excerpt,
       invocation_context, cascade_invalidated
FROM capability_invocations
WHERE capability_id = ?
ORDER BY invoked_at DESC;
```

**Fracture-detection view** (operator may want this):

```sql
SELECT id, invoked_at, exit_code
FROM capability_invocations
WHERE capability_id = ?
  AND invoked_at >= ?
  AND exit_code != 0
ORDER BY invoked_at DESC
LIMIT 10;
```

This is exactly what `RecentInvocations(ctx, id, since, 10)` already
returns, modulo the EX-7 failure filter (`exit_code != 0` is the
caller's responsibility).

### 5.4 Capability Lineage

**Purpose:** revision graph. Powers `mpm capability lineage <id>`.

**Inputs:** `capabilities.created_from_id`, `capabilities.superseded_by_id`.

**Template:**

```sql
-- Revision chain (ancestors + descendants)
WITH RECURSIVE ancestors(id, depth) AS (
    SELECT created_from_id, 1 FROM capabilities WHERE id = ?
    UNION ALL
    SELECT c.created_from_id, a.depth + 1
    FROM capabilities c JOIN ancestors a ON c.id = a.id
    WHERE a.depth < 10 AND c.created_from_id IS NOT NULL
)
SELECT c.id, c.name, c.state, c.created_at, a.depth
FROM ancestors a JOIN capabilities c ON c.id = a.id
ORDER BY a.depth;
```

### 5.5 Capability Diagnostics

**Purpose:** surface anti-patterns without naming them. Powers
`mpm capability diagnostics <id>` and the scheduler's
observation-tick preconditions.

**Diagnostics are evidence-indexed questions, not scores.** Each
diagnostic is a SQL fragment that returns zero rows when the
condition is healthy and at least one row when not. Output is a
list of named concerns; the operator decides what to do.

| Diagnostic | SQL fragment | When concerning |
|---|---|---|
| `fracture_recent` | `SELECT 1 FROM capability_events WHERE capability_id=? AND event_type='fracture' AND occurred_at >= strftime('%s','now','-1 hour')` | recent fracture |
| `source_hash_drift` | recompute `sha256(source_code)` and compare with `source_hash` column | runtime tamper (the executor already enforces this; the diagnostic surfaces what the executor saw) |
| `long_state_stall` | `SELECT 1 WHERE (strftime('%s','now') - state_changed_at) > ?` (e.g. 24h) | state has been stuck > threshold |
| `invocation_window_failures` | `SELECT COUNT(*) FROM capability_invocations WHERE capability_id=? AND invoked_at >= ? AND exit_code != 0` (e.g. last 60s, last 100) | ≥ 3 (fracture cluster pre-fracture) |
| `cascade_invalidated_unacked` | `SELECT id FROM capability_invocations WHERE capability_id=? AND cascade_invalidated=1` not yet followed by a fracture event | orphaned cascade signal |
| `supersession_orphan` | `SELECT 1 FROM capabilities WHERE superseded_by_id=? AND state != 'retired'` | shadow flag stuck |
| `policy_stale` | `SELECT 1 WHERE metadata->>'policy_snapshot' IS NULL AND state IN ('probation','active')` | policy not stamped at proposal (rare legacy row) |

Each diagnostic surfaces **the row that concerns the operator**, not
a verdict. The operator reads the row and decides. **The substrate
does not opine.**

### 5.6 What I am explicitly NOT projecting

I refuse to project:

- **"Capability Health"** — the substrate has no such thing; every
  evidence row already lives somewhere a projection can read.
- **"Failure Rate"** — `failure_count / (success_count + failure_count)`
  is a one-line computation; the substrate is not in the business of
  computing decimal scores.
- **"Reliability Score"** — same; **and the prompt forbids opaque
  weighting**.
- **"Trustworthiness Indicator"** — the state machine encodes trust
  via `execution_domain`; presenting it as a number would be a
  state-machine violation.
- **A "Health" view that combines multiple projections** — combining
  state + execution + lineage into one bag is the category error the
  cognitive taxonomy warns against.

---

## 6. Should a "Health" projection exist?

**No.** Here is the architectural reasoning, not the rhetorical one:

### 6.1 The substrate's authority is the state column

If two observers disagree about whether a capability is "healthy,"
the substrate's contract must say: read `state`. State is the only
column with a CHECK constraint that enumerates the legal answers.
Every other read is a projection, and projections must be reproducible.

A Health projection that disagrees with `state` would create two
authorities. The cognitive taxonomy says: state is canonical; do not
co-locate it with derived values.

### 6.2 The substrate's authority for "why this state" is the event log

A Health projection cannot answer "why is this in `degraded`?" — that
question's only source is `capability_events` (a `demotion` event
with `reason=` and `metadata.failure_trace`). Any Health view that
attempts to answer "why" must either (a) encode the event log
itself or (b) lie. We will not (b).

### 6.3 The substrate's authority for "is this code safe to run?" is `execution_domain` + `state_changed_at`

A Health projection that puts a green/yellow/red flag on a capability
in domain `sandbox` would tell the operator nothing. The `state`
column already says `active` or `draft`. Execution is gated by
`CanTransitionTo(state)`, not by Health.

### 6.4 The substrate's authority for "is the live code the proposed code?" is `source_hash`

The executor already enforces this on every invocation. A Health
projection that re-checks drift would duplicate enforcement without
improving it.

### 6.5 What the operator actually wants

The operator wants **to answer four questions**:

1. *Is this capability callable right now, and in what state?* → read
   `state`, `state_changed_at`, `execution_domain`. Already a
   one-row read.

2. *Has it ever been downgraded, and why?* → the event log filtered
   to `event_type IN ('demotion','fracture','rollback')`.

3. *What does the code look like? Has it actually run?* → the
   invocations table.

4. *What's the trust chain?* → the lineage CTE.

Each question has a deterministic answer. None requires a "Health"
column or projection. **The substrate already gives the operator
what they need; the only thing missing is the operator surface that
asks the right questions.**

If a future use case truly requires a "rolled-up" view, that view
belongs in the *consumer* (the CLI, the MCP tool, the wake-context
block), not in the substrate. The substrate's job ends at projections.

### 6.6 The principled position

> A "Health" projection becomes authoritative by social convention
> ("the dashboard says green; we're fine"). The substrate cannot
> prevent this convention from forming, but it can refuse to
> certify the projection by persisting it. The projection lives in
> the moment of the read; the audit is the source; the row that
> disagrees with the projection is the one to trust.

This is the position the principles block (prompt):
"State transitions are authoritative. Scores are never authoritative."
A Health projection is a score. The substrate does not produce scores.

---

## 7. Critique (deletion-first)

Every section above went under the question: *what architectural
truth does this reveal that is not already observable elsewhere?*
This section audits my own work with the same question.

### 7.1 What I added that the substrate already had

| Section I wrote | Already-existing equivalent | Why I could have just cited it |
|---|---|---|
| §1 evidence inventory | `state.go`, `store_lifecycle.go`, `store_invocation.go` | The current design already records all of this; the section is restating, not inventing. |
| §2 categories | implicit in SQL joins | Categorizing is the reader's job; the substrate does not need them named. |
| §4 deletion of `avg_latency_ms` | a one-line `DROP COLUMN` migration | The substrate's refactor kit already supports it. |
| §5 projections | one-line SQL templates with example query | Tells a tool author what to read; doesn't need a section. |

The critique: **this document is mostly a description of the
substrate.** That is not a failure; the user asked for an evidence
model, and the evidence model is the substrate. But it means the
deliverable is *less of a design* and *more of an explanation.*
That's the goal.

### 7.2 What I added that may be too much

| Candidate | Why I added it | Why I could have cut it |
|---|---|---|
| §5.5 Capability Diagnostics | Concrete list of evidence-indexed questions | Each diagnostic is one SQL fragment; listing them invites scope creep. |
| §1.8 Exogenous evidence | Surfaces that the substrate has cross-table signals | A reader who needs them joins on `id`; listing them invites people to denormalize. |
| §1.7 Typed metadata keys | Already documented in `metadata_int.go` and comments | A reader who reads the substrate sees them; restating risks giving an outdated list. |
| §6 numbered subsections (6.1–6.6) | Arguments for why no Health projection | The case for absence is one paragraph; six subsections over-argue it. |

**Cut §6.2, §6.3, §6.4 if editorializing.** They restate the same
architectural fact (don't co-locate authority) from three angles.
**§6.1, §6.5, §6.6** carry the load.

### 7.3 What I added that may not even be true

| Claim | Risk |
|---|---|
| "Removing `avg_latency_ms` requires only a SafeMigration and 2 primitive lines." | Migration ordering with `SafeMigrations` may need a one-shot backfill if the migration runs against an older DB without `DROP COLUMN` support. Need to verify SQLite version in CI. |
| "The fracture-detection sliding window reads from `RecentInvocations`, not from cached `fracture_count`." | I read the executor's description ("Recents" is fetched per detection) but did not pull the actual call site in `executor_fracture.go`. The cached counter exists for one of these reasons or both; verify before deleting. |
| "The seed primitive `capability_health` is a thin shell over the projection." | I did not read `capability_health` source — only saw it in `seed/capabilities.go`. Verify its actual implementation before claiming a 2-line change is enough. |

The honest critique is that **the deletion of `avg_latency_ms` is
proposed without verifying that no hot-path code depends on it**.
That verification belongs in the implementation PR, not in this
design. The design's stance — "this column is derivable at acceptable
cost" — is correct regardless of the exact dependency graph.

### 7.4 What a hostile reviewer might say

A reviewer looking at this for a public alpha would push on:

1. **"The substrate already had this; you wrote nothing new."**
   Correct. The evidence model is the substrate.

2. **"§5.5 (Capability Diagnostics) invents new surface area."**
   Each diagnostic is a SQL fragment in the substrate's audit-and-go
   pattern (the precedent is `query_audit_log`). They're projections,
   not tables; the surface is a CLI command, not a new schema.
   Acceptable.

3. **"§4.1 says delete `avg_latency_ms`; that's a breaking change
   in alpha."** Correct. Either:
   - **Defer the deletion** to a post-alpha release. Document it as
     "candidate for future deletion." The model still works with
     the column present.
   - **Or accept the deletion now** as evidence that the substrate
     prunes its own cruft. Public alpha is the right time for it.

   Recommendation: **defer**. The deletion is correct but tangential
   to the evidence model; bundling it risks merging the wrong story
   ("we cut a column") with the right story ("we explained how
   evidence already works").

4. **"What about a deleted capability? Can a forensics tool still
   read its evidence?"** Yes, `WHERE deleted_at IS NULL` filters out
   soft-deletes. Forensics queries that want soft-deleted rows drop
   the predicate (operator-only).

5. **"Where is the policy for what an operator is allowed to read
   from this evidence?"** Out of scope for this design; covered by
   MPM's existing capability gate (`internal/core/capability` is a
   privileged namespace). Soft-delete aside, projections are open
   to anyone with read access to the DB.

6. **"A score is not always authoritative."** Correct. The substrate
   does not produce scores; consumers may. That's the right place for
   the conversation.

### 7.5 What the document's net change is

If I cut everything I'd want to cut:

- §6.2, §6.3, §6.4 — fold into §6.1
- §1.8 — fold into §1.3 footnote
- §1.7 — drop (let the reader find them in the source)
- §5.5 — keep as bullet list under §5
- §4.1 deletion of `avg_latency_ms` — defer to a follow-up note

The document shrinks from ~7 sections to 4. What remains:
**§1** (evidence inventory), **§2** (categories as lenses),
**§5** (projections), **§6** (no Health projection, with the §6.1/6.5/6.6
reasons in three short bullets).

The rest is *naming what's already there*. That's the entire design.

---

## 8. The minimal design, restated after critique

What an alpha reader needs to know:

> **The capability substrate already is the evidence model.**
>
> A capability's life produces three immutable kinds of evidence:
> an append-only event log (`capability_events`), an append-only
> execution log (`capability_invocations`), and a current state row
> (`capabilities`). Lineage joins on `created_from_id`,
> `superseded_by_id`, and the `capability_dependencies` junction.
>
> **Six deterministic views cover every question an operator might
> ask:** Audit (the full capability row), Timeline (event log),
> Invocations (execution log), Lineage (revision + dependency CTE),
> Diagnostics (a bag of evidence-indexed questions, e.g. "recent
> fracture," "source drift," "long state stall"), and the Audit's
> joined view of the state machine.
>
> **There is no Health projection.** The substrate does not produce
> scores. Consumers may. The substrate's authority is the state
> column; its evidence is the event log; everything else is a
> read.
>
> **One optional deletion** exists: `capabilities.avg_latency_ms`
> is recomputable from `capability_invocations` at low cost
> (`SELECT AVG(duration_ms)` over an indexed column). Defer the
> deletion until alpha exit; the model is correct with the column
> present.

A reviewer can verify the design in two steps:

1. Open `internal/core/schema.go` lines 331–473 (the four capability
   tables). The schema is the evidence model.
2. Open `internal/core/capability/state.go` line 47
   (`validTransitions`). The state machine is the lifecycle.

There is nothing else. The substrate already does this; the deliverable
of this design is to stop adding to it.

---

## Appendix: tests that pin the existing guarantees

These tests fail if the substrate's evidence invariants break. The
alpha reader should expect them to exist and to pass.

| Test | Location | Property |
|---|---|---|
| `TestState_Transitions` family | `internal/core/capability/state_test.go` | Every transition is matrix-validated; illegal transitions refused. |
| `TestRecordInvocation_CounterInvariant` (or equivalent) | `internal/core/capability/store_invocation_test.go` | After every invocation, `success_count + failure_count == COUNT(*) FROM capability_invocations`. |
| `TestCapabilityEvents_AppendOnly` (or equivalent) | `internal/core/capability/store_lifecycle_test.go` | Transition writes row + event in one tx; event row never UPDATEd. |
| `TestSourceHashMismatch_RetiresCapability` | `internal/core/capability/executor_test.go` | Runtime tamper → `EventSourceHashMismatch` → retire. |
| `TestProbation_NotEligibleUntilCriteriaMet` | `internal/core/capability/store_lifecycle_test.go` | `PromoteToActive` from probation refuses if criteria unmet. |
| `TestFractureDetection_3Failures60s` | `internal/core/capability/executor_fracture_test.go` | Sliding window recognises cluster; refuses false positives. |
| `TestSqlOpenOwner_OnlyWhitelistedCallSites` | `internal/core/sqlopen_owner_test.go` | No new DB handles; capability subsystem uses DatabaseManager. |

The evidence model does not introduce new tests because it does not
introduce new persistence. The substrate's tests already cover the
invariants the model depends on.

---

## Provenance (architectural lineage)

This design was reached by applying the question *"what architectural
truth does this reveal that is not already observable elsewhere?"* to
every proposed element. Successive rounds of deletion produced the
document above. The major deletions from earlier drafts:

- "Capability reliability score" (deleted in earlier rounds)
- "Health score breakdown by dimension" (deleted — score in any form)
- "Failure rate widget" (deleted — single SQL fragment, not a table)
- "Trend line of invocations per day" (deleted — operationally
  expensive, returns to scoring)
- "Trust ladder as a separate column" (deleted — already
  `execution_domain`)
- "Cache `avg_latency_ms` for fast observation" (this column itself;
  defer for deletion post-alpha)
- "Cached projection `capability_audit_summary`" (deleted — every
  field is a SQL fragment; no composite type needed)

The substrate has the architecture that was waiting to be described.
The design's contribution is not invention — it is restraint.
