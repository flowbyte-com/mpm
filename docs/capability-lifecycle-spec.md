# Capability Lifecycle — Technical Specification

**Status:** DRAFT
**Date:** 2026-08-05
**Precedent:** `docs/audit-2026-07-16.md`, `docs/EPISTEMIC_CASCADES.md`
**Companion:** `agent_plugins/mpm-auto-route/` (context injection transport)

---

## Overview

The Capability Lifecycle subsumes the agent's growing toolbox into the same
empirical, observable framework that already governs memories, decisions, and
theories. A capability is a **stateful artifact**, not an executable blob. Its
lifecycle — `draft → linted → validated → probation → active → degraded →
fractured → needs_revision → rolled_back → retired` — is enforced by a strict
state machine in SQLite, and every transition is auditable in a dedicated
events table.

Trust is earned, not granted. A capability proposes for a `requested_domain`
of `sandbox` or `restricted`; `trusted` is reached only after track record
accumulates in the lower domains. Execution is wrapped in `bwrap` per domain,
and `source_hash` (SHA-256 of the source code) is verified on every invocation
to defeat runtime tampering.

This subsystem adds four database tables, a standalone execution wrapper, an
external dependency on `bwrap`, and a scheduler-driven validation pipeline.
It is a focused capability manager, not a general-purpose plugin system; the
substrate owns the lifecycle and the agent merely proposes capabilities.

The implementation MUST follow the architectural philosophy established by
`docs/EPISTEMIC_CASCADES.md`: every state transition is an observable row in
the events table, every cascade is an idempotent intent in an outbox, every
escalation is a wake to a human or agent. Failure is a state transition, not
an exception. There is no silent recovery, no "magic" constants, no
unverified execution.

---

## 1. Lifecycle

### 1.1 State diagram

```
                          operator approval / auto (sandbox only)
                                       │
   ┌────────┐  lint   ┌────────┐  dry-run  ┌───────────┐  probation   ┌────────┐
   │ draft  ├────────▶│ linted├───────────▶│ validated ├─────────────▶│active │
   └────┬───┘         └────┬───┘            └───────────┘              └───┬────┘
        │                  │                                               │
        │ update           │ dependency                                    │
        ▼                  ▼ fracture                                      │
   ┌────────┐         ┌───────────┐                                        │
   │  draft │         │   needs_  │�──────────────┐                        │
   │ (rev)  │         │ revision  │               │                        │
   └────────┘         └─────┬─────┘               │                        │
        ▲                   │                     │                        │
        │                   │ agent submits fix   │                        │
        └───────────────────┘                     │                        │
                                                  │                        │
   ┌──────────┐ 3 fail/60s  ┌──────────┐ tolerance breach                  │
   │ degraded ├────────────▶│fractured ├───────────────────────────────────▶┤
   │          │ recovery    │          │      ┌──────────────┐            │
   │          │◀────────────┤          │      │rolled_back   │◀───────────┘
   └──────────┘             └────┬─────�      └──────┬───────┘
                                 │ operator          │ line walk
                                 ▼                    ▼
                            ┌─────────┐          ┌──────────┐
                            │ retired │          │ ancestor │
                            └─────────┘          │ revived  │
                                                  └──────────┘
```

### 1.2 State semantics

| State | Description | Callable? | Cascade emitter? |
|---|---|---|---|
| `draft` | Initial state after `propose_skill`; awaiting forge validation | no | no |
| `linted` | Static analysis passed (shellcheck, py_compile, ruff) | no | no |
| `validated` | Dry-run in `bwrap` sandbox passed; ready for probation gate | no | no |
| `probation` | Receiving invocations to accumulate track record; not yet `trusted` | yes (sandbox) | no |
| `active` | Live version; passed probation; observation window begins | yes (per domain) | yes (on fracture) |
| `degraded` | Failure rate exceeded soft threshold; still callable | yes (with warning) | no |
| `fractured` | 3 failures in 60s; emitted cascade (if `author_theory_id` set) | no | **yes** |
| `needs_revision` | Demoted from `fractured`; agent can rewrite or abandon | no | no |
| `rolled_back` | Revision failed its observation window; lineage preserved | no | no |
| `retired` | Permanent end-of-life; superseded, abandoned, or operator-killed | no | no |

### 1.3 Transition table

| From | To | Trigger | Side effects |
|---|---|---|---|
| `draft` | `linted` | forge lint pass | none |
| `linted` | `validated` | forge dry-run exit 0 in `bwrap` | none |
| `validated` | `probation` | operator approval OR auto (if `requested_domain='sandbox'`) | snapshot predecessor → `metadata.baseline_at_proposal` (if revision); set `A.superseded_by_id = B.id` |
| `probation` | `active` | `success_count >= probation_required_success_count` AND `failure_rate <= probation_max_failure_rate` | `promoted_at = now`; predecessor → `retired` |
| `active` | `degraded` | `failure_rate > soft_threshold` (default 0.20) | none — still callable |
| `degraded` | `active` | `failure_rate < soft_threshold` for 5 consecutive invocations | none |
| `degraded` | `fractured` | 3 failures in 60s | emit cascade wake; if `author_theory_id` set, write to `epistemic_cascade_outbox` |
| `active` | `needs_revision` | operator explicit flag | attach operator note to `metadata.operator_notes` |
| `fractured` | `needs_revision` | automatic on detection | attach `last_failure_stderr` to `metadata.failure_trace` |
| `needs_revision` | `draft` | agent submits fix (`propose_skill` with `created_from_id`) | old row → `retired` (revision parent lineage preserved) |
| `active` | `rolled_back` | observation_window tolerance breach | full rollback transaction (§5.2) including downstream shatter (§5.3) |
| `*` | `retired` | explicit operator OR superseded by successful promotion | none |

Any transition not in this table is a forge bug. The validator refuses.

### 1.4 Soft-threshold and observation-window configuration

| Parameter | Default | Description |
|---|---|---|
| `probation_required_success_count` | `5` | N successful invocations to exit probation |
| `probation_max_failure_rate` | `0.10` | Failure rate ceiling during probation |
| `soft_threshold_failure_rate` | `0.20` | Above this, `active` → `degraded` |
| `fracture_window_seconds` | `60` | Time window for 3-failure cluster |
| `fracture_count_threshold` | `3` | Failures within the window to trigger `fractured` |
| `observation_window_invocations` | `100` | Invocations after promotion during which rollback is possible |
| `observation_window_hours` | `24` | Time bound on observation window |
| `success_rate_delta_tolerance` | `-0.02` | Minimum acceptable `success_rate` delta vs baseline |
| `latency_ratio_tolerance` | `2.0` | Maximum acceptable `avg_latency_ms` ratio vs baseline |
| `invocation_ttl_sandbox_hours` | `24` | GC retention for sandbox-domain invocations |
| `invocation_ttl_restricted_hours` | `72` | GC retention for restricted-domain invocations |
| `invocation_ttl_trusted_hours` | `168` | GC retention for trusted-domain invocations |
| `invocation_ttl_operator_hours` | `0` | GC retention for operator-domain invocations (`0` = indefinite) |

All parameters are stamped at proposal time into the capability's
`metadata.policy_snapshot` for audit; runtime reads the column-stamped values
rather than live config.

---

## 2. Schema

The capability subsystem introduces four tables. All use INTEGER Unix-epoch
seconds (per the `timestamps_unified_v1` convention) and follow the
`created_at`/`updated_at`/`deleted_at` soft-delete pattern established by
`internal/core/schema.go`.

### 2.1 `capabilities` table

```sql
CREATE TABLE capabilities (
    id              TEXT PRIMARY KEY,                     -- ULID
    name            TEXT NOT NULL UNIQUE,                 -- slug: ^[a-z][a-z0-9_]{2,63}$
    purpose         TEXT NOT NULL,                        -- 20–500 chars; dedup input
    source_code     TEXT NOT NULL,                        -- ≤64 KB, ≤500 lines (enforced by forge)
    source_language TEXT NOT NULL DEFAULT 'bash',         -- bash | python | jq
    source_hash     TEXT NOT NULL,                        -- SHA-256 of source_code at proposal time

    -- Lifecycle
    state            TEXT NOT NULL DEFAULT 'draft'
                     CHECK (state IN (
                         'draft','linted','validated','probation','active',
                         'degraded','needs_revision','fractured','rolled_back','retired'
                     )),
    execution_domain TEXT NOT NULL DEFAULT 'sandbox'
                     CHECK (execution_domain IN (
                         'sandbox','restricted','trusted','operator'
                     )),
    state_changed_at INTEGER NOT NULL,

    -- Lineage (causal-graph participation)
    author_theory_id TEXT,                                -- optional FK → theories(id)
    author_agent     TEXT,                                -- who proposed it
    created_from_id  TEXT,                                -- revision parent
    superseded_by_id TEXT,                                -- set when forge dedup'd or shadowed
    -- depends_on is in capability_dependencies (§2.3), not here

    -- Empirical metrics
    success_count       INTEGER NOT NULL DEFAULT 0,
    failure_count       INTEGER NOT NULL DEFAULT 0,
    fracture_count      INTEGER NOT NULL DEFAULT 0,       -- distinct from failure_count
    last_invoked_at     INTEGER,
    last_failure_at     INTEGER,
    last_failure_stderr TEXT,                             -- surfaced in needs_revision
    avg_latency_ms      REAL    NOT NULL DEFAULT 0,

    -- Probation policy (stamped at proposal; not magic constants)
    probation_required_success_count INTEGER NOT NULL DEFAULT 5,
    probation_max_failure_rate       REAL    NOT NULL DEFAULT 0.10,
    promoted_at                      INTEGER,            -- when state hit 'active'

    -- Discovery (FTS5 + cosine dedup)
    embedding BLOB,                                       -- vec(embedding_dim)
    tags     TEXT NOT NULL DEFAULT '[]',                  -- JSON array

    -- Standard (matches memories)
    created_at INTEGER NOT NULL,
    updated_at INTEGER NOT NULL,
    deleted_at INTEGER,                                   -- soft-delete
    metadata   TEXT NOT NULL DEFAULT '{}',                -- state_history, policy_snapshot, baseline_at_proposal, etc.

    FOREIGN KEY (author_theory_id)  REFERENCES theories(id)     ON DELETE SET NULL,
    FOREIGN KEY (created_from_id)   REFERENCES capabilities(id) ON DELETE SET NULL,
    FOREIGN KEY (superseded_by_id)  REFERENCES capabilities(id) ON DELETE SET NULL
);

CREATE INDEX idx_capabilities_state         ON capabilities(state) WHERE deleted_at IS NULL;
CREATE INDEX idx_capabilities_domain        ON capabilities(execution_domain) WHERE deleted_at IS NULL;
CREATE INDEX idx_capabilities_author        ON capabilities(author_theory_id) WHERE deleted_at IS NULL;
CREATE INDEX idx_capabilities_superseded    ON capabilities(superseded_by_id) WHERE deleted_at IS NULL;
CREATE INDEX idx_capabilities_last_invoked  ON capabilities(last_invoked_at DESC) WHERE deleted_at IS NULL;
CREATE INDEX idx_capabilities_state_changed ON capabilities(state_changed_at) WHERE deleted_at IS NULL;
```

**Critical queries this table serves:**

```sql
-- The single canonical "live tool" lookup (the executor uses this)
SELECT * FROM capabilities
WHERE name = ? AND state = 'active' AND superseded_by_id IS NULL AND deleted_at IS NULL
LIMIT 1;

-- Capabilities in observation window (scheduler tick reads this)
SELECT id, name FROM capabilities
WHERE state = 'active'
  AND promoted_at IS NOT NULL
  AND (promoted_at + 86400) > strftime('%s','now')   -- observation_window_hours
  AND deleted_at IS NULL;

-- Probation completion check (forge reads this per invocation)
SELECT
    success_count,
    failure_count,
    (failure_count * 1.0 / NULLIF(success_count + failure_count, 0)) AS failure_rate
FROM capabilities WHERE id = ?;
```

### 2.2 `capability_invocations` table

Pure execution telemetry. Synthetic events (rollbacks, dependency shatter,
promotions) go to `capability_events` (§2.4) — never here.

```sql
CREATE TABLE capability_invocations (
    id                   TEXT PRIMARY KEY,
    capability_id        TEXT NOT NULL,
    invoked_at           INTEGER NOT NULL,
    exit_code            INTEGER NOT NULL,
    duration_ms          INTEGER NOT NULL,
    stderr               TEXT,                            -- truncated; full in metadata
    invocation_context   TEXT,                            -- JSON: caller, args (sanitized)
    cascade_invalidated  BOOLEAN NOT NULL DEFAULT 0,      -- did this fire a 'stale foundation' wake?

    FOREIGN KEY (capability_id) REFERENCES capabilities(id) ON DELETE CASCADE
);

CREATE INDEX idx_invocations_capability_time ON capability_invocations(capability_id, invoked_at DESC);
CREATE INDEX idx_invocations_recent_failures ON capability_invocations(capability_id, exit_code)
                                              WHERE exit_code != 0;
CREATE INDEX idx_invocations_gc              ON capability_invocations(invoked_at);  -- for TTL sweep
```

### 2.3 `capability_dependencies` junction table

A capability declares which other capabilities it depends on. Reverse
lookups power the cascade walkers (fracture cascade §5.1, rollback
cascade §5.2, downstream shatter §5.3).

```sql
CREATE TABLE capability_dependencies (
    capability_id TEXT NOT NULL,
    depends_on_id TEXT NOT NULL,
    added_at      INTEGER NOT NULL,
    PRIMARY KEY (capability_id, depends_on_id),
    FOREIGN KEY (capability_id) REFERENCES capabilities(id) ON DELETE CASCADE,
    FOREIGN KEY (depends_on_id)  REFERENCES capabilities(id) ON DELETE CASCADE
);

CREATE INDEX idx_deps_depends_on ON capability_dependencies(depends_on_id);
```

**Recursive cascade walker (used by §5.1 and §5.3):**

```sql
WITH RECURSIVE downstream AS (
    SELECT capability_id, 0 AS depth FROM capability_dependencies
    WHERE depends_on_id = ?root
    UNION ALL
    SELECT cd.capability_id, d.depth + 1
    FROM capability_dependencies cd
    JOIN downstream d ON cd.depends_on_id = d.capability_id
    WHERE d.depth < ?max_depth
)
SELECT c.id, c.name, c.state, c.execution_domain, d.depth
FROM capabilities c JOIN downstream d ON c.id = d.capability_id
WHERE c.state IN ('active','degraded','probation')
ORDER BY d.depth ASC;
```

`max_depth` defaults to `5`; deeper transitive dependencies are surfaced as
operator warnings rather than auto-shattered.

### 2.4 `capability_events` table

State transitions and synthetic events. **Strict separation from
`capability_invocations`** so execution telemetry remains mathematically
pure for metrics aggregation.

```sql
CREATE TABLE capability_events (
    id            TEXT PRIMARY KEY,
    capability_id TEXT NOT NULL,
    event_type    TEXT NOT NULL,                          -- see §2.4.1
    occurred_at   INTEGER NOT NULL,
    actor         TEXT NOT NULL,                          -- 'forge' | 'scheduler' | 'operator:<id>' | 'agent:<id>'
    from_state    TEXT,
    to_state      TEXT,
    reason        TEXT,                                   -- human-readable explanation
    related_id    TEXT,                                   -- e.g. predecessor on rollback; dead dep on shatter
    metadata      TEXT NOT NULL DEFAULT '{}',             -- JSON: tolerance_breached, cascade_depth, etc.

    FOREIGN KEY (capability_id) REFERENCES capabilities(id) ON DELETE CASCADE
);

CREATE INDEX idx_events_capability_time ON capability_events(capability_id, occurred_at DESC);
CREATE INDEX idx_events_type           ON capability_events(event_type, occurred_at DESC);
CREATE INDEX idx_events_actor          ON capability_events(actor);
```

#### 2.4.1 Event types

| `event_type` | When emitted | Required fields |
|---|---|---|
| `promotion` | state → `active` (from probation) | `from_state='probation'`, `to_state='active'`, `metadata.policy_snapshot` |
| `demotion` | any state regression not from observation | `from_state`, `to_state` |
| `fracture` | state → `fractured` | `metadata.failure_cluster`, `metadata.triggering_invocations` |
| `rollback` | observation window tolerance breach | `related_id=predecessor.id`, `metadata.tolerance_breached` |
| `dependency_shatter` | downstream shatter | `related_id=upstream.id`, `metadata.cascade_depth` |
| `retirement` | state → `retired` | `from_state`, `metadata.reason` |
| `supersede` | shadow flag set during probation | `related_id=candidate.id` |
| `source_hash_mismatch` | runtime tamper detected | `metadata.observed_hash`, `metadata.expected_hash` |

### 2.5 Schema migrations

Add to `internal/core/schema.go`'s `SafeMigrations` (column additions for
upgrade-in-place). The four tables above are additions; no existing table
is altered. Migration order matters: capabilities → invocations →
dependencies → events. Indexes follow their tables in the same migration.

---

## 3. Forge

The forge is the validation pipeline that turns a `propose_skill` payload
into a `linted → validated → probation` capability. It is implemented as
three handlers:

- `mpm skill forge` — operator/agent-facing entry point (proposal intake)
- `mpm skill forge-now <id>` — urgent foreground validation (operator-gated)
- `mpm-scheduler` tick `skill_forge_tick` — 60s background sweep

All three write through the same validation sequence (§3.2) wrapped in a
single SQLite transaction.

### 3.1 `propose_skill` payload shape

```json
{
  "name": "git_status_porcelain",
  "purpose": "Show git working tree status in porcelain v1 format, suitable for parsing",
  "source_code": "git status --porcelain",
  "source_language": "bash",
  "requested_domain": "restricted",
  "tags": ["git", "vcs", "status"],
  "depends_on": ["cap_abc123_git_auth_v1"],
  "author_theory_id": "theo_abc123",
  "expected_io": {
    "args": "none",
    "stdout_shape": "newline-delimited porcelain v1",
    "exit_codes": {"0": "ok", "128": "not a git repo"}
  },
  "metadata": {
    "rationale": "Agent repeatedly needs to check working tree state before commit",
    "failure_modes": ["not in git repo", "permission denied"]
  }
}
```

#### 3.1.1 Field contracts

| Field | Type | Required | Validation |
|---|---|---|---|
| `name` | string | yes | slug `^[a-z][a-z0-9_]{2,63}$`; unique against `state IN ('active','probation','linted','validated')` |
| `purpose` | string | yes | 20–500 chars (dedup input; too short = meaningless, too long = hallucination) |
| `source_code` | string | yes | ≤64 KB, ≤500 lines (structural cap; bigger = refuse) |
| `source_language` | enum | yes | `bash` \| `python` \| `jq` (extend cautiously; each new language = new validator) |
| `requested_domain` | enum | yes | `sandbox` \| `restricted` \| `trusted` (**never `operator`** — agents cannot self-propose operator domain) |
| `tags` | string[] | optional | FTS5 surface; also drives `list_skills` filters |
| `depends_on` | capability_id[] | optional | every ID must be `state='active'` at proposal time |
| `author_theory_id` | theory_id | optional | epistemic lineage; if set, `fractured` state fires the epistemic cascade |
| `replaces_id` | capability_id | optional | when set: bypasses dedup similarity check for that specific ID (see §3.6 fork flow); forge sets `created_from_id=replaces_id` for lineage walker |
| `expected_io` | object | optional | contract declaration; compared against actual at probation |
| `metadata` | object | optional | free-form; survives round-trip |

#### 3.1.2 Rejection shapes

The forge returns structured rejections (mirroring the existing
`mirror.jsonl` blocked-content pattern):

```json
{
  "status": "rejected",
  "capability_id": null,
  "reasons": [
    {"step": "scanner", "pattern": "rm_rf_variable", "line": 3, "snippet": "rm -rf $TARGET"},
    {"step": "domain_policy", "message": "draft cannot request domain=trusted"}
  ]
}
```

### 3.2 Validation pipeline (in order, atomic)

Each step must pass; failure aborts the transaction.

| # | Step | Validator | On failure |
|---|---|---|---|
| 1 | Schema validation | Go struct tags + manual checks | reject with field-level errors |
| 2 | Name uniqueness | `SELECT 1 FROM capabilities WHERE name=? AND state NOT IN ('retired','rolled_back')` | reject (or accept as revision if `created_from_id` provided) |
| 3 | Source size cap | `len(source_code) <= 65536 AND line_count <= 500` | reject |
| 4 | Scanner pass | Extend `internal/core/memory.go:isSensitiveContent` + new `isPoisonedCode` patterns (§7.1) | reject; mirror to `mirror.jsonl` |
| 5 | Linter pass | `shellcheck -f json` for bash; `python -m py_compile` + `ruff check` for python | reject with lint errors |
| 6 | Dependency check | Every `depends_on` ID exists and is `state='active'` | reject with the dead dep ID |
| 7 | Domain policy | `requested_domain != 'trusted'` for draft capabilities | reject |
| 8 | Dedup check | Cosine similarity of `purpose` embedding vs `state IN ('active','probation')` capabilities; threshold 0.92. **Bypass:** if `replaces_id` is set, skip dedup for that specific ID only. | return `superseded_by` pointer to existing capability (unless bypassed via `replaces_id`) |
| 9 | Dry-run in sandbox | `bwrap` execute with dummy args; expect `exit_code=0` | reject |
| 10 | Source hash | `SHA-256(source_code)` → `source_hash` column | — |
| 11 | Insert | `INSERT INTO capabilities ... state='validated'` (after dry-run) OR state='linted' (lint-only if dry-run skipped for sandbox) | — |
| 12 | Audit | `INSERT INTO capability_events ... event_type='lint_pass'\|'dry_run_pass'` | — |

### 3.3 Domain policy (earned trust)

Capabilities request a `requested_domain`; the substrate grants the *floor*
based on track record, not the request. Promotion across domains requires
accumulated invocations at the lower domain.

| Current domain | To reach next | Required |
|---|---|---|
| `sandbox` | `restricted` | 50 successful invocations, `failure_rate ≤ 0.05`, no `fractured` events |
| `restricted` | `trusted` | 500 successful invocations, `failure_rate ≤ 0.02`, no `fractured` events in last 30 days |
| `trusted` | `operator` | **Never** — only operators can elevate to `operator` via `mpm skill elevate` |

The promotion emits a `promotion` event; the new domain becomes the
capability's `execution_domain` and the executor wrapper
(§4) adjusts accordingly.

### 3.4 Probation mechanics

Once a capability reaches `state='probation'`:

- Each successful invocation increments `success_count`.
- Each failed invocation increments `failure_count`.
- The forge scheduler reads the row on each invocation and checks:
  - `success_count >= probation_required_success_count` AND
  - `failure_count / (success_count + failure_count) <= probation_max_failure_rate`
- When both conditions hold: state → `active`, `promoted_at = now`,
  `event_type='promotion'` written.

If probation criteria fail to materialize within `probation_max_hours`
(default 168 = 1 week), the capability auto-transitions to
`needs_revision` with reason="probation_timeout" in metadata.

### 3.5 Revision flow

A revision (`mpm skill update <id>`) creates a **new** row with
`created_from_id = <parent.id>` and re-enters the validation pipeline from
step 1. The original row:

- Stays `state='active'` until the revision reaches `probation`.
- At that point, `original.superseded_by_id = revision.id` (shadow flag).
- When the revision reaches `active`, the original transitions to
  `retired` (via `event_type='retirement'` with
  `metadata.reason='superseded_by_revision'`).

The "live version" query is:

```sql
SELECT * FROM capabilities
WHERE name = ? AND state = 'active' AND superseded_by_id IS NULL
LIMIT 1;
```

### 3.6 Fork flow (`replaces_id`)

When the agent explicitly intends to replace a known-broken capability
with a better implementation — typically because the existing one has
flaws the agent has diagnosed but that haven't yet surfaced as
`fractured` — the `propose_skill` payload includes `replaces_id:
<id>`. Without this field, the dedup check (§3.2 step 8) would
permanently block the proposal because the new capability's `purpose`
would necessarily score above the 0.92 threshold against the flawed
predecessor.

Forge behavior when `replaces_id` is set:

1. **Dedup bypass for that specific ID only** (§3.2 step 8). All other
   capabilities are still checked against the threshold.
2. **`created_from_id` is automatically set to `replaces_id`**, giving
   the rollback cascade walker (§5.2) a lineage path to revive the
   predecessor if the fork itself fails.
3. The fork flows through the standard pipeline: `draft → linted →
   validated → probation → active`.
4. Shadow/revive mechanics are identical to §3.5.

**Semantic distinction between `created_from_id` and `replaces_id`:**

| Field | Agent intent | Database mechanics |
|---|---|---|
| `created_from_id` | Continuous iteration (small improvements, bug fixes on a working concept) | New row; lineage walker can find parent |
| `replaces_id` | Explicit fork (existing capability is known-broken; agent walks away from its lineage) | New row; dedup bypass for that ID; lineage walker still finds parent (forge sets `created_from_id` automatically) |

For the system, both fields produce the same database mechanics
(revision chain, shadow flag, retirement on promotion, revival on
rollback). The distinction lives in the agent's intent and the audit
log — `event_type='retirement'` records `metadata.reason` as
`'superseded_by_revision'` for `created_from_id` and
`'superseded_by_fork'` for `replaces_id`.

---

## 4. Executor

The executor is the single Go function that turns a `(capability, args,
env)` triple into a `(exit_code, stdout, stderr, duration_ms)` result.
Every invocation — agent, scheduler, operator — flows through it.

```go
package capability

type Invoker interface {
    Invoke(ctx context.Context, cap *Capability, args []string, env map[string]string) (*Result, error)
}

type Result struct {
    ExitCode   int
    Stdout     string
    Stderr     string
    DurationMs int64
}

// Invoke is the single entry point. It enforces:
//   1. capability is live (state='active', superseded_by_id IS NULL)
//   2. source_hash matches DB row (tamper detection)
//   3. execution_domain matches wrapper selection
//   4. bwrap wrapper for sandbox/restricted; direct exec for trusted/operator
//   5. timeout from capability.metadata.max_runtime_ms (default 30000)
//   6. write invocation record + update capability metrics
//   7. evaluate fracture cluster (3-in-60s)
func (e *Executor) Invoke(ctx context.Context, cap *Capability, args []string, env map[string]string) (*Result, error)
```

### 4.1 Per-domain `bwrap` arguments

**sandbox** (the default for new proposals):

```
bwrap \
    --ro-bind /usr /usr \
    --ro-bind /lib /lib \
    --ro-bind /bin /bin \
    --ro-bind /etc/resolv.conf /etc/resolv.conf \
    --tmpfs /tmp \
    --tmpfs /home \
    --unshare-net \
    --unshare-pid \
    --new-session \
    --die-with-parent \
    --chdir /tmp \
    -- <script> <args>
```

**restricted** (sandbox + project bind mount + capability-declared paths):

```
bwrap \
    --ro-bind /usr /usr \
    --ro-bind /lib /lib \
    --ro-bind /bin /bin \
    --bind <project_dir> <project_dir> \
    --bind <declared_path_1> <declared_path_1> \
    --bind <declared_path_2> <declared_path_2> \
    --tmpfs /tmp \
    --unshare-pid \
    --new-session \
    --die-with-parent \
    --chdir <project_dir> \
    -- <script> <args>
```

Network remains unshared in restricted. The proposal declares
`metadata.allowed_paths` listing every writable path; any other write
attempt causes a `bwrap` failure (which the executor maps to a
`fracture`-eligible error).

**trusted** (direct execution, source_hash enforced):

```
echo "<source_code>" | sha256sum    # verify against DB
echo "<source_code>" | bash -s -- <args>
```

**operator** (operator-only; requires `metadata.operator_approved_at`):

```
# Same as trusted, but the call site must pass an operator token
# enforced at the CLI/MCP layer before Invoke is reached
echo "<source_code>" | bash -s -- <args>
```

### 4.2 `source_hash` verification (mandatory before every invocation)

```go
func verifySourceHash(cap *Capability, liveSourceCode string) error {
    sum := sha256.Sum256([]byte(liveSourceCode))
    if hex.EncodeToString(sum[:]) != cap.SourceHash {
        // Emit a source_hash_mismatch event (§2.4.1)
        // Transition to fractured (the runtime has been tampered with)
        return ErrSourceHashMismatch
    }
    return nil
}
```

This check fires regardless of domain. If the live source code
(loaded from disk) doesn't match what the DB recorded at proposal
time, the capability fractures immediately and the operator gets a
wake. There is no "skip verification" mode.

### 4.3 Timeout and resource limits

| Resource | Default | Configurable via |
|---|---|---|
| Wall clock | 30s | `metadata.max_runtime_ms` |
| Memory | 512 MB | `metadata.max_memory_mb` (bwrap `--rlimit-as`) |
| Open FDs | 256 | `metadata.max_fds` (bwrap `--rlimit-nofile`) |
| Output size | 16 MB | hard cap; stderr/stdout truncate beyond this |

A timeout emits `event_type='fracture'` with
`metadata.reason='timeout'`. Memory/FD exhaustion same.

---

## 5. Cascade

Two cascade patterns. Both are symmetric to the existing epistemic
cascade (`docs/EPISTEMIC_CASCADES.md`): the upstream event fires an
outbox intent, the materializer (running as `mpm cascade materialize` or
the skill-specific tick) drains the outbox and applies the effect.

### 5.1 Fracture cascade

When a capability transitions to `fractured`:

1. **Epistemic cascade (if `author_theory_id` set).** Write a row to
   `epistemic_cascade_outbox` with `dead_artifact_id=<theory>`,
   `reason='capability_fractured'`. The existing `mpm cascade
   materialize` tick handles it.

2. **Capability cascade (always).** Walk `capability_dependencies` for
   downstream capabilities (§2.3 recursive CTE). For each downstream in
   `state IN ('active','degraded','probation')`:
   - Transition to `needs_revision`.
   - Write `event_type='dependency_shatter'` with `related_id=<upstream>`.
   - If the downstream has `author_theory_id` set, also write to
     `epistemic_cascade_outbox`.

3. **Wake.** Emit a `notification` wake for the agent:
   `"Skill '<name>' fractured. Downstream skills demoted: <list>."`

The fracture cascade is read-only on `capabilities` until the transaction
commits; the outbox row + the `needs_revision` state transitions + the
events + the wake all happen in one transaction.

### 5.2 Rollback cascade (observation window)

When an active capability in its observation window breaches tolerance
(§1.4: `success_rate_delta < -0.02` OR `latency_ratio > 2.0x`):

```sql
BEGIN IMMEDIATE;

-- 1. Walk lineage backward to find the closest retired ancestor.
--    Cycle-safe via depth cap (32) and path-string revisit guard.
--    ORDER BY depth ASC selects the immediate parent (not a sibling
--    branch that happened to retire later).
WITH RECURSIVE ancestors(
    id, state, created_from_id, depth, path
) AS (
    SELECT id, state, created_from_id, 0, ',' || id || ','
    FROM capabilities
    WHERE id = ?revision_id AND deleted_at IS NULL

    UNION ALL

    SELECT c.id, c.state, c.created_from_id, a.depth + 1,
           a.path || c.id || ','
    FROM capabilities c
    JOIN ancestors a ON c.id = a.created_from_id
    WHERE a.depth < 32
      AND instr(a.path, ',' || c.id || ',') = 0
)
SELECT id FROM ancestors
WHERE state = 'retired'
ORDER BY depth ASC
LIMIT 1;
-- Returns predecessor.id, or NULL if revision was a fresh skill

-- 2. Demote the failed revision
UPDATE capabilities
SET state = 'rolled_back', state_changed_at = strftime('%s','now')
WHERE id = ?revision_id;

INSERT INTO capability_events
    (id, capability_id, event_type, occurred_at, actor, from_state, to_state, reason, metadata)
VALUES
    (?, ?revision_id, 'rollback', strftime('%s','now'), 'scheduler',
     'active', 'rolled_back',
     ?reason, json_object('tolerance_breached', ?tolerance,
                          'predecessor_id', ?predecessor_id));

-- 3. Revive the predecessor — ONLY if it's healthy
UPDATE capabilities
SET state = 'active', superseded_by_id = NULL, state_changed_at = strftime('%s','now')
WHERE id = ?predecessor_id AND state = 'retired';
-- If rowcount == 0, predecessor is also broken: ABORT and escalate (§5.2.1)

INSERT INTO capability_events
    (id, capability_id, event_type, occurred_at, actor, from_state, to_state, reason)
VALUES
    (?, ?predecessor_id, 'promotion', strftime('%s','now'), 'scheduler',
     'retired', 'active', 'rollback_revive');

-- 4. Downstream shatter (§5.3). Cycle-safe: depth cap of 5 (matches
--    spec §5.2) plus path-string revisit guard against diamond deps.
--    The `path` is captured into event metadata so operators can read
--    metadata.shatter_path = ",C,B,A," from the audit log to see
--    exactly how the blast radius reached the shattered node.
WITH RECURSIVE downstream(
    capability_id, depth, path
) AS (
    SELECT capability_id, 0, ',' || capability_id || ','
    FROM capability_dependencies
    WHERE depends_on_id = ?revision_id

    UNION ALL

    SELECT cd.capability_id, d.depth + 1,
           d.path || cd.capability_id || ','
    FROM capability_dependencies cd
    JOIN downstream d ON cd.depends_on_id = d.capability_id
    WHERE d.depth < 5
      AND instr(d.path, ',' || cd.capability_id || ',') = 0
)
UPDATE capabilities
SET state = 'needs_revision', state_changed_at = strftime('%s','now')
WHERE id IN (SELECT capability_id FROM downstream);

WITH RECURSIVE downstream(
    capability_id, depth, path, from_state
) AS (
    SELECT cd.capability_id, 0, ',' || cd.capability_id || ',', c.state
    FROM capability_dependencies cd
    JOIN capabilities c ON c.id = cd.capability_id
    WHERE cd.depends_on_id = ?revision_id

    UNION ALL

    SELECT cd.capability_id, d.depth + 1,
           d.path || cd.capability_id || ',', c.state
    FROM capability_dependencies cd
    JOIN downstream d ON cd.depends_on_id = d.capability_id
    JOIN capabilities c ON c.id = cd.capability_id
    WHERE d.depth < 5
      AND instr(d.path, ',' || cd.capability_id || ',') = 0
)
INSERT INTO capability_events
    (id, capability_id, event_type, occurred_at, actor, from_state, to_state, reason, related_id, metadata)
SELECT
    ?, capability_id, 'dependency_shatter', strftime('%s','now'), 'scheduler',
    from_state, 'needs_revision', 'upstream_rolled_back', ?revision_id,
    json_object('cascade_depth', depth, 'shatter_path', path)
FROM downstream;

-- 5. Wake the agent
INSERT INTO scheduled_wakes
    (reason, target_time, metadata)
VALUES
    ('capability_rollback', strftime('%s','now'),
     json_object('skill_id', ?revision_id, 'reason', ?reason,
                 'downstream_shattered', ?downstream_count));

COMMIT;
```

#### 5.2.1 "Both broken" escalation

If the predecessor revival UPDATE returns `rowcount=0`:

```sql
-- ROLLBACK the transaction
ROLLBACK;

BEGIN IMMEDIATE;

-- Mark revision as escalated (rolled_back + metadata.escalated=true)
UPDATE capabilities
SET state = 'rolled_back', state_changed_at = strftime('%s','now'),
    metadata = json_set(metadata, '$.escalated', json('true'))
WHERE id = ?revision_id;

INSERT INTO capability_events
    (id, capability_id, event_type, occurred_at, actor, from_state, to_state, reason, metadata)
VALUES
    (?, ?revision_id, 'rollback', strftime('%s','now'), 'scheduler',
     'active', 'rolled_back',
     'predecessor_unavailable', json_object('escalated', 'true'));

-- Operator wake (highest priority)
INSERT INTO scheduled_wakes
    (reason, target_time, metadata)
VALUES
    ('capability_rollback_escalated', strftime('%s','now'),
     json_object('skill_id', ?revision_id, 'predecessor_id', ?predecessor_id,
                 'reason', 'predecessor_unavailable'));

COMMIT;
```

The `escalated` flag is observable via `mpm skill show <id>` but is NOT a
separate state — keeping the state enum clean. The substrate refuses to
silently pick a poison.

### 5.3 Downstream shatter (standalone reference)

Already integrated into §5.2 step 4. When invoked standalone (e.g., for
`fracture` cascade §5.1 step 2), the same recursive CTE pattern applies:

```sql
WITH RECURSIVE downstream AS (
    SELECT capability_id, 0 AS depth FROM capability_dependencies
    WHERE depends_on_id = ?root
    UNION ALL
    SELECT cd.capability_id, d.depth + 1
    FROM capability_dependencies cd
    JOIN downstream d ON cd.depends_on_id = d.capability_id
    WHERE d.depth < ?max_depth
)
UPDATE capabilities
SET state = 'needs_revision', state_changed_at = strftime('%s','now')
WHERE id IN (SELECT capability_id FROM downstream)
  AND state IN ('active','degraded','probation');
```

### 5.4 Chain rollback (A→B→C)

If A → B → C and C rolls back, B is the revival target. The recursive CTE
in §5.2 step 1 walks all the way back. If B is itself fractured, the walk
continues to A. If A is also broken, escalate (§5.2.1). The walk is
bounded by `created_from_id` lineage depth — no infinite loops.

---

## 6. Observability

### 6.1 CLI command surface

```
mpm skill propose      --payload <json>            # submit a draft
mpm skill show         <id>                        # inspect (state, metrics, lineage)
mpm skill list         [--state=X] [--domain=Y]    # filterable inventory
mpm skill audit        <id> [--limit=N] [--since=T]   # full lineage diff (pipe to xed)
mpm skill update       <id> --payload <json>       # propose a revision
mpm skill rollback     <id> [--reason=X]           # explicit operator rollback
mpm skill retire       <id> [--reason=X]           # explicit operator retirement
mpm skill forge-now    <id>                        # urgent foreground validation (operator-gated)
mpm skill invoke       <id> [args...]              # direct invocation (testing/operator)
mpm skill promote      <id>                        # operator override of probation gate
mpm skill elevate      <id>                        # operator-only domain elevation
mpm skill gc                                     # invocation table TTL sweep
```

### 6.2 `mpm skill audit <id>` output format

```text
Capability: git_status_porcelain (cap_abc123)
Current state: active (since 2026-08-05T10:00:00Z)
Execution domain: restricted
Success: 487  Failure: 3  Fracture: 0  Avg latency: 52ms

Lineage:
  ┌─ cap_orig_v1 (retired, superseded_by=cap_abc123, 2026-07-15)
  ├─ cap_abc123 (active, current version)
  └─ cap_rev2   (draft, pending lint, proposed 2026-08-05T11:00:00Z by agent:claude)

State history (cap_abc123):
  2026-08-05T09:00:00Z  draft      → linted      forge      "shellcheck passed"
  2026-08-05T09:00:05Z  linted     → validated   forge      "dry-run exit 0"
  2026-08-05T09:01:00Z  validated  → probation   operator   "approved"
  2026-08-05T09:30:00Z  probation  → active      scheduler  "5/5 successes, 0% failure"

Recent invocations (last 10):
  2026-08-05T10:42:13Z  exit=0   48ms
  2026-08-05T10:41:55Z  exit=0   51ms
  ...

Downstream dependents (capability_dependencies):
  cap_xyz789  (active)  depth=1  "uses git_status_porcelain in pre-commit check"

Observation window: 47/100 invocations remaining (expires 2026-08-06T09:00:00Z)
```

This output is diff-friendly: every section is a stable header followed by
uniform lines. Pipe to `xed`, `diff`, or `less`.

**Pagination flags.** A single successful tool can accumulate thousands of
invocation rows; without pagination, the audit output floods the terminal
buffer and becomes impossible to pipe into an external editor for review.

| Flag | Default | Description |
|---|---|---|
| `--limit=N` | `50` | Max rows per section (state history, invocations, events). `--limit=0` shows all rows. |
| `--since=T` | none | Only rows newer than `T`. Accepts ISO-8601 timestamp (`2026-08-01T00:00:00Z`) or relative duration (`24h`, `7d`, `30d`). |
| `--until=T` | now | Only rows older than `T`. Same format as `--since`. |

Examples:

```bash
# Last 24 hours of audit data, capped at 200 rows per section
mpm skill audit cap_abc123 --limit=200 --since=24h

# Everything since a specific timestamp, unbounded
mpm skill audit cap_abc123 --limit=0 --since=2026-08-01T00:00:00Z

# Pipe a deep review session into xed
mpm skill audit cap_abc123 --limit=0 --since=7d | xed -

# Last 50 rows (default), no time filter — quick sanity check
mpm skill audit cap_abc123
```

Section headers are stable regardless of pagination; rows within each
section are line-oriented and time-descending. The `mpm skill audit`
output remains diff-friendly at any scale.

### 6.3 Context injection via `mpm-auto-route`

The `agent_plugins/mpm-auto-route/` plugin builds the agent's bootstrap
context. Extend it to append a `<capability-updates>` block when state
transitions have occurred since the agent's last seen cursor:

```text
<capability-updates>
[2026-08-05T11:00:00Z] Skill 'jq_parse' promoted to active (state: probation → active, 5 successes).
[2026-08-05T11:05:00Z] Skill 'git_status_porcelain' entered observation window (100 invocations or 24h).
[2026-08-05T11:30:00Z] Skill 'rm_helper' fractured (3 failures in 60s). Downstream 'cleanup_workflow' demoted to needs_revision. Stderr available via 'mpm skill show cap_rm_helper'.
</capability-updates>
```

The cursor is stored per-session in
`metadata.last_seen_capability_event_at`. The plugin reads
`capability_events` rows newer than the cursor and formats them. This
makes the lifecycle **legible to the model** rather than silently
appearing/disappearing from its tool schema.

### 6.4 `mpm skill list` output

```
ID                          NAME                       STATE       DOMAIN      SUCCESS/FAIL   LAST INVOKED
cap_abc123                  git_status_porcelain       active      restricted  487/3          2m ago
cap_def456                  jq_parse                   active      sandbox     1240/8         5m ago
cap_ghi789                  rm_helper                  fractured   sandbox     12/4           1h ago
cap_jkl012                  cleanup_workflow           needs_revision  -          0/0            never
cap_mno345                  git_status_porcelain_v2    draft       -           0/0            never
```

Filters: `--state=X`, `--domain=Y`, `--tag=Z`, `--author-theory=<id>`,
`--fractured-only`, `--in-observation-window`.

---

## 7. Security

The security model is **defense in depth**, not a single chokepoint. Each
layer is a backstop for the previous; none is sufficient alone.

| Layer | Mechanism | Role | Bypassable? |
|---|---|---|---|
| 1 | Regex scanner (§7.1) | Fast-fail heuristic; refuses obvious cases early to save compute | **Yes** — LLMs trivially bypass via string concatenation, indirect expansion, encoding. Not a security boundary. |
| 2 | Linter (§3.2 step 5) | Catches semantic issues regex misses (unquoted vars, command injection patterns) | **Yes** — same class of bypass as Layer 1 |
| 3 | Dry-run in `bwrap` sandbox (§3.2 step 9) | Catches runtime errors before promotion | Partial — catches crashes, not intent |
| 4 | `bwrap` execution wrapper (§4) | **The actual security boundary.** Read-only bind mounts, `--unshare-net`, no writable paths outside declared surfaces | No — structural; the kernel enforces |
| 5 | `source_hash` verification (§7.3) | Defeats runtime tampering of the source file on disk | No — comparison is deterministic |
| 6 | Domain ladder (§7.2) | Capability-claimed scope vs. granted scope; promotion across domains requires track record | No — enforced at promotion time |

**The regex scanner is NOT the security boundary.** A hallucinated
`TARGET="r"; TARGET+="m"; $TARGET -rf /` will pass Layer 1 (the literal
string `rm -rf` never appears), pass Layer 2 (the linter sees only a
script that builds a string at runtime), and reach Layer 4 — where
`bwrap` refuses to write outside the bind mounts and the script dies.
The system remains safe **even if every regex pattern is bypassed**,
because `bwrap` is structural and the kernel enforces it.

The scanner's job is to refuse obvious cases early and reduce validation
cost. The `bwrap` wrapper's job is to make arbitrary damage impossible.
Conflating the two is how security reviews go wrong.

### 7.1 Scanner patterns (extend `internal/core/memory.go`)

The existing 19-pattern secret/poison scanner lives in `SaveMemoryNode`
and covers all write paths. Extend with these `isPoisonedCode`-specific
patterns for `source_code`:

| Pattern ID | Regex (illustrative) | Severity |
|---|---|---|
| `rm_rf_variable` | `rm\s+-rf?\s+"?\$\{?[A-Za-z_][A-Za-z0-9_]*\}?"?` | critical |
| `rm_rf_root` | `rm\s+-rf?\s+/\s*($\|\s)` | critical |
| `curl_pipe_sh` | `curl[^|]*\|\s*(sh\|bash\|zsh)` | critical |
| `wget_pipe_sh` | `wget[^|]*\|\s*(sh\|bash\|zsh)` | critical |
| `reverse_shell_bash` | `bash\s+-i\s+>&\s*/dev/tcp/` | critical |
| `reverse_shell_nc` | `nc\s+-[a-z]*e\s` | critical |
| `hardcoded_aws_key` | `AKIA[0-9A-Z]{16}` | critical (already covered) |
| `hardcoded_private_key` | `-----BEGIN (RSA \|EC \|OPENSSH \|DSA \|PGP )?PRIVATE KEY-----` | critical (already covered) |
| `absolute_path_write_root` | `>\s*/(etc\|usr\|var\|bin\|sbin\|boot\|sys\|proc)/` | high |
| `chmod_777` | `chmod\s+(-R\s+)?777` | medium |
| `dd_destructive` | `dd\s+.*of=/dev/(sd[a-z]\|hd[a-z]\|nvme[0-9])` | critical |
| `mkfs_unmounted` | `mkfs(\.\w+)?\s+/dev/` | critical |

Blocked proposals are mirrored to `mirror.jsonl` (existing pattern) with
the `capability_source_code` tag, never reaching the database.

### 7.2 Domain ladder (earned trust)

```
sandbox
  │  50 successful invocations, failure_rate ≤ 0.05, no fractures
  ▼
restricted
  │  500 successful invocations, failure_rate ≤ 0.02, no fractures in 30d
  ▼
trusted
  ▲
  │  NEVER reachable by capability proposal
  │
operator  ◀──── mpm skill elevate (operator-only)
```

The `trusted` domain is reachable only via accumulated track record. The
`operator` domain is reachable only via explicit operator elevation —
agents can never self-propose it.

### 7.3 `source_hash` (tamper detection)

- Computed at proposal time: `source_hash = SHA-256(source_code)`.
- Stored in the `capabilities` row.
- Verified before every invocation (§4.2).
- On mismatch: transition to `fractured`, emit
  `event_type='source_hash_mismatch'` with observed vs. expected hash,
  wake the operator.

This defeats runtime tampering — disk-level corruption, accidental
edits outside the engine, or any process that modifies the source file
without going through the forge.

### 7.4 Network and filesystem boundaries

| Domain | Network | Filesystem writes |
|---|---|---|
| `sandbox` | none (`--unshare-net`) | none (read-only bind mounts + tmpfs) |
| `restricted` | none | declared paths only (bwrap `--bind`) |
| `trusted` | host network | full filesystem (operator-trusted) |
| `operator` | host network | full filesystem (operator-trusted) |

A capability that needs network access MUST be promoted to `trusted`
first. The forge's domain policy (§3.3) makes this structurally
enforced.

---

## 8. Implementation Tasks

Ordered by dependency. Each task has acceptance criteria. Estimate units
are rough (S=hours, M=day, L=multi-day).

### 8.1 Foundation

| ID | Task | Estimate | Acceptance |
|---|---|---|---|
| F-1 | Schema migration: add 4 tables + indexes to `internal/core/schema.go` SafeMigrations | S | Migration runs cleanly on existing DB; rollback tested |
| F-2 | Go structs in `internal/core/capability/` for Capability, Invocation, Dependency, Event | S | Compile-time assertions against table columns |
| F-3 | Constants for state machine, domains, event types | S | Single source of truth; no string literals scattered |

### 8.2 Forge

| ID | Task | Estimate | Acceptance |
|---|---|---|---|
| FG-1 | `propose_skill` payload parser + schema validation | S | Invalid payloads rejected with structured errors |
| FG-2 | Scanner extension: `isPoisonedCode` patterns in `internal/core/memory.go` | M | All patterns from §7.1 covered; mirror.jsonl fires |
| FG-3 | Linter integration: `shellcheck -f json`, `python -m py_compile`, `ruff check` | M | Lint failures block proposal with line-level errors |
| FG-4 | Domain policy: enforce draft ≠ trusted | S | Trusted request from draft → reject with reason |
| FG-5 | Dedup: cosine similarity check on `purpose` embedding | M | Above threshold → return `superseded_by` pointer |
| FG-6 | Dependency validation: every `depends_on` ID exists and is active | S | Broken dep → reject with dead dep ID |
| FG-7 | Dry-run in `bwrap` sandbox | M | Non-zero exit → reject; hash mismatch → reject |
| FG-8 | Forge tick handler: `skill_forge_tick` in `mpm-scheduler` | M | Drains `draft` and `linted` rows; idempotent |
| FG-9 | `forge-now` foreground path (operator-gated) | S | Bypasses tick; same validation sequence |
| FG-10 | Probation completion check | S | State advances on criteria met; emits `promotion` event |

### 8.3 Executor

| ID | Task | Estimate | Acceptance |
|---|---|---|---|
| EX-1 | `Executor.Invoke(ctx, cap, args, env) (*Result, error)` interface | M | All four domains route correctly |
| EX-2 | `bwrap` wrapper for `sandbox` and `restricted` | M | Network unshare verified; bind mounts honored |
| EX-3 | Direct exec wrapper for `trusted` and `operator` | S | source_hash enforced; timeout enforced |
| EX-4 | `source_hash` verification (§4.2) | S | Mismatch → fracture + event + wake |
| EX-5 | Resource limit enforcement (timeout, memory, FDs, output size) | M | Exceeded limits → fracture-eligible error |
| EX-6 | Invocation record write + metric updates | S | Atomic with execution result |
| EX-7 | Fracture cluster detector (3-in-60s) | M | Fires state transition + cascade correctly |

### 8.4 Cascade

| ID | Task | Estimate | Acceptance |
|---|---|---|---|
| C-1 | Fracture cascade: epistemic outbox write (§5.1) | M | Outbox row appears; `cascade materialize` consumes it |
| C-2 | Fracture cascade: capability downstream walk (§5.1 step 2) | M | Recursive CTE walks `capability_dependencies` correctly |
| C-3 | Rollback cascade: lineage walker (§5.2 step 1) | M | Returns immediate retired ancestor or NULL |
| C-4 | Rollback cascade: predecessor revival (§5.2 step 3) | M | `rowcount=0` triggers escalation (§5.2.1) |
| C-5 | Downstream shatter SQL (§5.3, integrated into §5.2 step 4) | M | Downstream capabilities → `needs_revision`; events + wakes fire |
| C-6 | Observation window tick: `skill_observation_tick` in `mpm-scheduler` | M | Tolerance breach triggers full §5.2 transaction |
| C-7 | `escalated` flag handling (rolled_back + metadata.escalated) | S | Operator wake fires; `mpm skill show` displays flag |

### 8.5 CLI / MCP

| ID | Task | Estimate | Acceptance |
|---|---|---|---|
| CLI-1 | `mpm skill propose` / `show` / `list` | M | All filters work; output formats match §6 |
| CLI-2 | `mpm skill audit` (lineage diff format) | M | §6.2 output stable across runs; pipe-friendly |
| CLI-3 | `mpm skill update` / `rollback` / `retire` | M | Each emits correct event; permissions enforced |
| CLI-4 | `mpm skill forge-now` / `promote` / `elevate` | M | Operator-gated; `elevate` refuses non-operator |
| CLI-5 | `mpm skill invoke` (direct, for testing) | S | Bypasses agent permission model; full audit trail |
| CLI-6 | `mpm skill gc` (invocation TTL sweep) | S | Honors per-domain TTL from §1.4 |
| MCP-1 | MCP tools: `propose_skill`, `invoke_skill`, `list_skills`, `skill_status`, `update_skill`, `retire_skill` | M | Mirror CLI surface; payload contracts identical |

### 8.6 Observability integration

| ID | Task | Estimate | Acceptance |
|---|---|---|---|
| O-1 | Extend `mpm-auto-route` plugin to inject `<capability-updates>` block | M | Cursor-tracked; events newer than cursor surface |
| O-2 | State history tracking in `metadata.state_history` | S | Append on every transition; queryable for audit |
| O-3 | Per-session cursor (`metadata.last_seen_capability_event_at`) | S | Read on session start; advanced on context injection |

### 8.7 Tests

| ID | Task | Estimate | Acceptance |
|---|---|---|---|
| T-1 | Unit: scanner patterns cover all §7.1 cases | S | Static-analysis test enforces coverage |
| T-2 | Unit: state machine transition table enforced | S | Illegal transition → error |
| T-3 | Unit: dedup threshold tuning | S | Below threshold → no supersede; above → supersede |
| T-4 | Integration: full lifecycle `draft → retired` | M | Golden test against in-memory DB |
| T-5 | Integration: fracture cascade (capability downstream + epistemic outbox) | M | Both cascades fire; outbox row visible to `cascade materialize` |
| T-6 | Integration: rollback cascade with shatter | M | Predecessor revived; downstream → needs_revision; events written |
| T-7 | Integration: `escalated` path (both broken) | S | Predecessor revival returns rowcount=0; escalation fires |
| T-8 | Integration: `bwrap` wrappers per domain | L | Real `bwrap` execution; network unshare verified via `curl` |
| T-9 | Integration: source_hash tamper detection | S | Modified source file → fracture + wake |
| T-10 | Integration: observation window tolerance breach | M | Synthetic invocations trigger rollback transaction |
| T-11 | Stress: 10k capabilities, 100k invocations, GC sweep latency | M | GC sweep < 5s; queries still indexed |

### 8.8 Documentation

| ID | Task | Estimate | Acceptance |
|---|---|---|---|
| D-1 | Operator runbook entry in `docs/` | S | Mirrors `cascade_drain` runbook format |
| D-2 | Update `docs/WISHLIST.md` — mark "self-evolving skills" as shipped | S | One-line update |
| D-3 | Update top-level `CLAUDE.md` — add capability subsystem to architecture diagram | S | Brief; defers detail to this spec |

---

## Deferred items

These don't need to be locked before implementation; they are refinement
during PR review.

- **Exact `bwrap` argument lists per workload.** Refinement; default
  templates in §4.1 are the starting point.
- **`expected_io` contract comparator.** How declared vs. observed
  shapes are compared at probation. Spec it in a follow-up PR once the
  basic probation flow lands.
- **Dedup threshold tuning.** 0.92 default; expose as
  `mpm_config.json:capability.dedup_threshold` for operator tuning.
- **Probation metric defaults.** 5 successes, 10% failure rate; expose
  as proposal-time parameters.
- **Audit command output format.** §6.2 is a starting point; iterate
  based on operator feedback.
- **Synthetic invocation records.** Currently rejected as a design
  choice (§2.2 vs §2.4); revisit if metrics aggregation needs them.
- **Per-language validators.** Only bash + python + jq initially; add
  new languages cautiously (each = new validator + linter).
- **Capability versioning scheme.** Lineage via `created_from_id`
  suffices; no separate version column needed.

---

## Appendix A: Naming conventions (terminology fix)

The original conversation used `validation_window` for the post-promotion
observation period. This collides with the `validated` state in prose.
The spec uses **`observation_window`** throughout. The semantics are:

- **Probation:** pre-active. The capability is being judged before
  reaching `active`.
- **Observation window:** post-active. The capability is being watched
  for regressions before being trusted as the canonical live version.

If the operator prefers a different name (e.g., `trust_watch`,
`confidence_period`), update §1.3, §5.2, and §6.2 consistently.

---

## Appendix B: Relationship to existing subsystems

| Subsystem | Relationship |
|---|---|
| `epistemic_cascade_outbox` (`EPISTEMIC_CASCADES.md`) | Capability fracture writes to this outbox when `author_theory_id` is set. The existing `mpm cascade materialize` drains it. |
| `mpm-scheduler` (recent commits) | New tick handlers: `skill_forge_tick`, `skill_observation_tick`, `skill_gc_tick`. All follow the same isolated-handler pattern as `cascade_drain`. |
| `mpm-auto-route` plugin | Extends context injection to surface `<capability-updates>` (§6.3). |
| Scanner in `SaveMemoryNode` | Extended with `isPoisonedCode` patterns (§7.1). Coverage enforced by extending `TestScannerCoverage_AllMemoriesWritersScanContent`. |
| `mpm call` universal interface | New tools: `propose_skill`, `invoke_skill`, `list_skills`, `skill_status`, etc. Registered alongside the existing ~80 commands. |

---

**End of specification. Implementation may begin against F-1.**
