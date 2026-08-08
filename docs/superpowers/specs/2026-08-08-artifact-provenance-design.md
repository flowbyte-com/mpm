# Artifact Provenance — Design Spec

**Date:** 2026-08-08
**Status:** Alpha implementation
**Scope:** Post-alpha data infrastructure for telemetry, with alpha CLI surface

---

## Spec-top orientation

> **Alpha telemetry is observational, not retrospective.** Provenance begins at the point this feature is deployed. Existing artifacts are not reconstructed or assigned synthetic provenance. Analytics must distinguish "no provenance recorded" from "provenance recorded with an unknown field."

## Architectural invariants

These three invariants are load-bearing. Every implementation decision must preserve them.

1. **Telemetry availability.** An artifact must never depend on telemetry availability for correctness. Provenance rows are observability; failing to record one is audit-logged, never transacted away.
2. **Provenance semantics.** Provenance describes *how* an artifact was created. It does not determine whether the artifact is true, useful, trusted, or healthy. The system measures *correlations* between provenance and downstream outcomes; it never smuggles model reputation into the substrate as a correctness signal.
3. **Declared execution.** MPM records **declared** execution metadata only. MPM does not infer model reasoning characteristics. If an adapter or framework reports `thinking_level = high`, MPM records it. If MPM does not know, the field stays NULL. MPM does not infer `thinking_level` from `temperature`, `max_tokens`, or any other field. The telemetry dataset contains measurements, not assumptions.

## The architectural boundary

```
provenance
    │
describes execution
    │
    ▼
artifact
    │
    ▼
lifecycle outcomes
    │
    ▼
analytics
```

Not:

```
model → quality score
```

The substrate answers *"do these models produce knowledge that survives?"* by correlating provenance with downstream outcomes (reinforcements, contradictions, decays, promotions). It does not declare a model "better" because its provenance label is prestigious.

---

## Motivation

The `metadata.provenance.{agent,model,source,compute}` JSON block injected by `saveMemoryWithContextImpl` (via `ActiveContext.provenanceMeta()`) is a partial, opaque, and lossy record. It holds four fields. It cannot answer:

- Which model produced which memory, in a queryable, indexed form?
- Did the memory the model produced survive 30 days, or was it decayed?
- Why did the operator run the model with `thinking_level = high` and what happened next?
- Did the framework's MCP timeout correlate with higher contradiction rates?

The current shape forces future analytics to either re-parse JSON in SQL (slow, fragile) or accept that the answer is unrecoverable. This feature replaces the JSON block with a first-class relational table that captures the *full* declared execution context of every artifact, while preserving the legacy JSON block in existing rows for forensic history.

---

## Schema

### `artifact_provenance` table

```sql
CREATE TABLE IF NOT EXISTS artifact_provenance (
    id                   TEXT PRIMARY KEY,
    artifact_id          TEXT NOT NULL,
    artifact_type        TEXT NOT NULL
                          CHECK (artifact_type IN ('memory','theory','lesson','decision')),
    created_at           INTEGER NOT NULL
                          DEFAULT (CAST(strftime('%s','now') AS INTEGER)),

    -- Schema versioning (semantic, not additive)
    schema_version       TEXT NOT NULL DEFAULT 'v1',

    -- Actor taxonomy
    actor_kind           TEXT NOT NULL,             -- 'agent','human','import','system','unknown'
    actor_id             TEXT,                      -- agent name, human user, or tool

    -- Framework & adapter
    framework_name       TEXT,                      -- 'mpm_cli','claude_code','mini-bot'
    framework_version    TEXT,                      -- '1.4.2'
    framework_adapter    TEXT,                      -- 'cli','mcp','mpm_call','telegram'

    -- Model & provider
    provider_name        TEXT,                      -- 'anthropic','openrouter','minimax'
    model_name           TEXT,                      -- 'claude-sonnet-4-5'
    model_revision       TEXT,                      -- '20241022'
    api_endpoint         TEXT,                      -- normalized endpoint (no query/auth)

    -- Inference telemetry
    temperature          REAL,
    max_tokens           INTEGER,
    reasoning_mode       TEXT,                      -- 'high','medium','low','none','adaptive'
    reasoning_effort     REAL,                      -- 0.0–1.0
    thinking_level       TEXT,                      -- NULL = unexposed; 'none' explicitly disabled
    thinking_tokens      INTEGER,
    thinking_visible     INTEGER,                   -- 0/1 (INTEGER for SQLite portability)

    -- Execution context
    session_id           TEXT,
    invocation_id        TEXT,                      -- correlation identity (one model/agent turn)
    parent_artifact_id   TEXT,                      -- causal relationship (e.g., cascade-derived)

    -- Escape hatch
    provider_metadata    TEXT,                      -- raw JSON object string (NOT NULL implies valid JSON)

    UNIQUE (artifact_id, artifact_type),            -- one provenance row per artifact
    CHECK (actor_kind IN ('agent','human','import','system','unknown'))
);

CREATE INDEX IF NOT EXISTS idx_provenance_artifact
    ON artifact_provenance(artifact_id, artifact_type);
CREATE INDEX IF NOT EXISTS idx_provenance_model
    ON artifact_provenance(provider_name, model_name);
CREATE INDEX IF NOT EXISTS idx_provenance_actor
    ON artifact_provenance(actor_kind, framework_name);
CREATE INDEX IF NOT EXISTS idx_provenance_session
    ON artifact_provenance(session_id);
CREATE INDEX IF NOT EXISTS idx_provenance_invocation
    ON artifact_provenance(invocation_id);
```

### Schema design notes

- **No `FOREIGN KEY` on `session_id`.** Sessions live in `memories` with `collection='sessions'`; a FK to a non-table is invalid. `session_id` is treated as a string.
- **No `FOREIGN KEY` on `artifact_id`.** Four possible target tables (memories, theories, lessons, decisions); a single FK would force one target. The CHECK on `artifact_type` is the structural guard.
- **`thinking_visible` is INTEGER (0/1), not BOOLEAN.** SQLite's `BOOLEAN` is an alias for `INTEGER` but not all migrations preserve it through `sql.Dump`. INTEGER is portable.
- **`UNIQUE (artifact_id, artifact_type)`** is the structural guard against duplicate recording. Two distinct code paths cannot both record provenance for the same artifact; the database refuses the second attempt.
- **`schema_version` is semantic, not additive.** Additive nullable fields (e.g., `cache_read_tokens`, `reasoning_tokens`, `tool_choice`) do **not** require a version bump. A version bump is reserved for semantic changes to the interpretation of existing fields. The default is `v1`. The column is `NOT NULL` so the absence of a meaningful schema version is structurally impossible.
- **`created_at` at the provenance level is conceptually distinct from `artifact.created_at`.** They are usually identical. Eventually they may differ (async telemetry, replayed imports, repaired gaps). The provenance timestamp exists so future analysts can reconstruct event chronology without inferring from unrelated timestamps.

### NULL semantics

| Field | NULL means | Value means |
|---|---|---|
| `actor_kind` | n/a (column is NOT NULL) | declaration; `unknown` is a valid value |
| `framework_name` | not exposed / not declared | framework reported |
| `model_name` | not exposed / not declared | model reported |
| `provider_name` | not exposed / not declared | provider reported |
| `thinking_level` | framework does not expose thinking telemetry | `'none'` explicitly disabled; `'low'/'med'/'high'/'max'/'adaptive'` declared |
| `thinking_tokens` | not reported | integer count |
| `provider_metadata` | no metadata | valid JSON object string |
| `api_endpoint` | not declared | normalized URL |

Empty-string inputs from env vars or per-call parameters are normalized to NULL at write time. The test `TestProvenance_NullVsEmptySemantics` enforces the distinction.

### Analytics views

`v_model_memory_yield` and `v_model_theory_utility` are defined in `schema.go` for queryability via `mpm exec-sql`. The `model-yield` CLI command consumes the first view.

```sql
CREATE VIEW IF NOT EXISTS v_model_memory_yield AS
SELECT
    p.provider_name || '/' || p.model_name AS model_spec,
    p.framework_name,
    p.framework_adapter,
    COUNT(m.id) AS total_created,
    SUM(CASE WHEN m.deleted_at IS NULL AND m.weight >= 1
              AND (CAST(strftime('%s','now') AS INTEGER) - m.created_at) >= 2592000
             THEN 1 ELSE 0 END) AS survived_30d,
    ROUND(CAST(SUM(CASE WHEN m.deleted_at IS NULL AND m.weight >= 1
                          AND (CAST(strftime('%s','now') AS INTEGER) - m.created_at) >= 2592000
                         THEN 1 ELSE 0 END) AS REAL)
          / NULLIF(SUM(CASE WHEN (CAST(strftime('%s','now') AS INTEGER) - m.created_at) >= 2592000
                            THEN 1 ELSE 0 END), 0) * 100, 1) AS survival_30d_pct,
    SUM(m.reinforce_count) AS total_reinforcements,
    SUM(m.challenge_count) AS total_challenges,
    COUNT(DISTINCT c.memory_id) AS contradicted_count
FROM artifact_provenance p
JOIN memories m ON p.artifact_id = m.id AND p.artifact_type = 'memory'
LEFT JOIN contradiction_log c ON m.id = c.memory_id
GROUP BY p.provider_name, p.model_name, p.framework_name, p.framework_adapter;

CREATE VIEW IF NOT EXISTS v_model_theory_utility AS
SELECT
    p.provider_name || '/' || p.model_name AS model_spec,
    p.thinking_level,
    COUNT(t.id) AS theories_proposed,
    SUM(CASE WHEN json_extract(t.metadata, '$.status') = 'proven' THEN 1 ELSE 0 END) AS theories_proven,
    SUM(CASE WHEN json_extract(t.metadata, '$.status') = 'disproven' THEN 1 ELSE 0 END) AS theories_refuted,
    ROUND(AVG(t.confidence), 2) AS avg_final_confidence
FROM artifact_provenance p
JOIN memories t ON p.artifact_id = t.id AND p.artifact_type = 'theory'
GROUP BY p.provider_name, p.model_name, p.thinking_level;
```

**`survival_30d` is a descriptive lifecycle measure, not a quality score.** The formula is:

> `survived_30d == (artifact.deleted_at IS NULL AND artifact.weight >= 1 AND now - artifact.created_at >= 30 days)`

This is documented in the view's metadata and in the `mpm provenance model-yield` help text. The feature does not claim that "higher survival = better model." It provides a mechanistic count downstream consumers can interpret.

---

## Resolver architecture

### Conceptual flow

```
        execution context (env vars, headers, per-call args)
                       │
                       ▼
                provenance resolver
                  (process-cached, not frozen)
                       │
        base + per-invocation overrides
                       │
                       ▼
              effective provenance
                       │
         ┌─────────────┴─────────────┐
         ▼                           ▼
    memory creation             lesson creation
    (saveMemoryRow)             (AddLesson)
         │                           │
         └──────────┬────────────────┘
                    ▼
             artifact_provenance
                    │
             best-effort telemetry
                    │
          failure → audit + continue
```

### The resolver is cached, not frozen

```go
// internal/core/provenance.go

type ProvenanceResolver struct {
    base *CreationProvenance  // from env, cached at start
}

func NewFromEnv() *ProvenanceResolver { /* reads env once */ }

// Resolve produces the effective provenance for one artifact write.
// sessionID/invocationID/parentArtifactID are per-call.
//
//   - sessionID override:     per-call ambient session (vs. process base)
//   - invocationID:           correlation identity — one model/agent turn
//   - parentArtifactID:       causal chain (e.g., cascade-derived theory)
//
// For `mpm call` (process-scoped), Resolve() returns the base provenance.
// For long-lived MCP servers, the base is process-wide but the effective
// provenance is per-call. The schema does not change; the resolver
// architecture supports both.
func (r *ProvenanceResolver) Resolve(
    sessionID, invocationID, parentArtifactID string,
) *EffectiveProvenance
```

### Mounting the resolver

`DatabaseManager` holds the resolver as a field, not a global:

```go
type DatabaseManager struct {
    // ...
    ProvenanceResolver *ProvenanceResolver  // defaults to NewFromEnv() at init
}
```

Tests override via `dm.ProvenanceResolver = &ProvenanceResolver{base: testProv}` and restore in `t.Cleanup`. **No package-level `SetOverride` / `ResetOverride` global mutability.** No parallel-test contamination.

### `NewFromEnv` priority chain

The resolver reads in this priority order:

1. `MPM_PROVENANCE` — JSON blob, parsed into `CreationProvenance`. If absent or malformed, log a warning and fall through.
2. `MPM_PROVENANCE_*` flat env vars — `MPM_PROVENANCE_MODEL`, `MPM_PROVENANCE_FRAMEWORK`, `MPM_PROVENANCE_VERSION`, `MPM_PROVENANCE_ADAPTER`, `MPM_PROVENANCE_PROVIDER`, `MPM_PROVENANCE_REVISION`, `MPM_PROVENANCE_API`, `MPM_PROVENANCE_TEMPERATURE`, `MPM_PROVENANCE_MAX_TOKENS`, `MPM_PROVENANCE_REASONING_MODE`, `MPM_PROVENANCE_REASONING_EFFORT`, `MPM_PROVENANCE_THINKING_LEVEL`, `MPM_PROVENANCE_THINKING_TOKENS`, `MPM_PROVENANCE_THINKING_VISIBLE`, `MPM_PROVENANCE_INVOCATION_ID`, `MPM_PROVENANCE_PARENT`, `MPM_PROVENANCE_ACTOR`, `MPM_PROVENANCE_ACTOR_ID`.
3. `MPM_SESSION_ID`, `MPM_AGENT_ID`, `MPM_HOSTNAME` — already-existing env vars contribute to `session_id` and `actor_id`.
4. Defaults — `actor_kind="unknown"` (no TTY heuristic). All other fields default to NULL.

### Default actor inference

The TTY heuristic is unreliable: a non-interactive human script looks like an agent; an agent with a terminal looks like a human. The data model must reflect what we *know*, not what we guess.

| What we know | `actor_kind` |
|---|---|
| `MPM_PROVENANCE_ACTOR` env var set explicitly | the explicit value |
| Caller passed `--as-agent` / `--as-human` CLI flag | same — explicit |
| Neither | `"unknown"` |

`unknown` is a meaningful observation, not a placeholder. Analytics can answer "which model produced the most provenance with `actor_kind='unknown'`?" — that's a real question about humans in the loop.

`FRAMEWORK_NAME` and `MODEL_NAME` default to `NULL` (not `"unknown"`), so an unbranded artifact is NULL in the column and `GROUP BY` doesn't accidentally cluster it.

---

## Field validation

Three hard rules in `RecordArtifactProvenance`, enforced before the INSERT:

1. **APIEndpoint** — must not contain `?` (query string) or `@` (userinfo). Reject with audit row. The endpoint must not contain credentials, authorization headers, query-string secrets, or request payloads. The telemetry system must never become a secret-collection mechanism.
2. **ProviderMetadata** — must be either empty (→ NULL) or a valid JSON object. Reject JSON arrays, scalars, or malformed strings with audit row. The contract is:
   > `provider_metadata` preserves the supplied JSON representation without parsing/re-serialization.
   MPM does not beautify or normalize the byte content. The principle is **capture broadly, interpret narrowly**. MPM does not interpret arbitrary provider metadata — it stores raw facts for downstream layers to interpret.
3. **All fields** — the only constraint is the schema CHECK. If a field is unset, NULL; if set, the value. No auto-conversion.

---

## Transaction safety

`RecordArtifactProvenance` is given a `*sql.Tx` (the same one the artifact INSERT used). Three failure modes, three explicit behaviors:

| Failure | Tx state | Result |
|---|---|---|
| Provenance INSERT succeeds | tx is fine | `Recorded=true` |
| Validation rejection (bad APIEndpoint, malformed JSON) | tx is fine | `Recorded=false`, `ValidationReason` set, audit row |
| SQLite error mid-INSERT (constraint, schema mismatch, connection) | **tx may be poisoned** | `Recorded=false`, `SQLError` set, audit row, tx untouched beyond SAVEPOINT |

The provenance INSERT runs inside a `SAVEPOINT`:

```sql
SAVEPOINT prov_rec;
INSERT INTO artifact_provenance (...) VALUES (...);
RELEASE SAVEPOINT prov_rec;
```

On INSERT failure, `ROLLBACK TO SAVEPOINT prov_rec; RELEASE SAVEPOINT prov_rec;` keeps the artifact's INSERT committed. The function **never** returns `error` (the artifact insert succeeded); it returns a `ProvenanceRecordResult` struct that future observability layers can use to track telemetry degradation.

```go
type ProvenanceRecordResult struct {
    Recorded         bool
    ValidationReason string
    SQLError         string
}

func (dm *DatabaseManager) RecordArtifactProvenance(
    tx *sql.Tx,
    artifactID, artifactType string,
    prov *EffectiveProvenance,
) ProvenanceRecordResult
```

The artifact-create caller's contract:

```go
artifactID, err := dm.SaveMemoryNode(...)
// err handling ...

res := dm.RecordArtifactProvenance(tx, artifactID, "memory", dm.ProvenanceResolver.Resolve(...))
if !res.Recorded {
    // Observable but non-fatal. The audit row has the cause.
    // Telemetry counter: provenance_recorded{success=0,reason=...}
}
```

The test `TestProvenanceFailure_NeverPoisonsArtifactTransaction` enforces this invariant by forcing a provenance INSERT failure (e.g., via temporary table rename) and asserting the artifact row is still committed.

---

## Write hooks

Two integration points are the complete artifact creation surface for this codebase:

```go
// internal/core/db.go : saveMemoryRow, AFTER the scanner passes and INSERT succeeds
res := dm.RecordArtifactProvenance(
    txGetArtifactInsertTx(dm), id, artifactTypeFromCollection(collection),
    dm.ProvenanceResolver.Resolve("", "", ""),
)
// res.Recorded may be false; the artifact row is already committed.
```

```go
// internal/core/db.go : AddLesson, AFTER the scanner passes and INSERT succeeds
res := dm.RecordArtifactProvenance(
    txGetLessonInsertTx(dm), lesson.ID, "lesson",
    dm.ProvenanceResolver.Resolve("", "", ""),
)
```

`artifactTypeFromCollection(collection)` returns `"memory"` for the `memories` collection, `"theory"` for `theories`, `"decision"` for `decisions`. Lessons go through `AddLesson` directly with `artifact_type = "lesson"`.

### What gets hooked

- `remember` / `add` → `saveMemoryNode` → `saveMemoryRow` ✓
- `theorize` / `propose_theory` → `saveMemoryNode` → `saveMemoryRow` (collection='theories') ✓
- `decide` / `record_decision` → `saveMemoryNode` → `saveMemoryRow` (collection='decisions') ✓
- `save_lesson` → `AddLesson` ✓
- `promote_scratchpad` → `SaveMemoryNode` (collection='memories') ✓
- `cascade_materializer` → `SaveMemoryNode` (collection='theories') with `parent_artifact_id` set to the dead artifact ✓

Sessions are excluded. They self-describe via `session_id` and `metadata`.

### Future external-ingestion interception

A generalized ingestion interceptor is **not** in this alpha. The two write paths (`saveMemoryRow`, `AddLesson`) are the complete artifact creation surface for the current codebase. The centralized hooks mean that a future `FutureExternalIngestionInterception` component (placeholder name) will only need to call those two functions — not invent a new write path. The design doc explicitly defers this to post-alpha when external adapters (MCP servers, framework integrations) actually land.

---

## CLI surface

Three commands form the coherent alpha surface. They progress from "look at one artifact" to "correlate an invocation" to "compare observed yield."

### `mpm provenance <artifact_id>`

Returns the `artifact_provenance` row for the given artifact. JSON output via `--json`. Errors when artifact is not found or has no provenance recorded.

```
$ mpm provenance mem-abc123
Artifact:    mem-abc123 (memory)
Created:     2026-08-08 14:32:11 UTC
Provenance:
  schema_version:    v1
  actor_kind:        agent
  actor_id:          808
  framework:         claude_code
  framework_version: 1.4.2
  framework_adapter: cli
  provider:          anthropic
  model:             claude-sonnet-4-5
  model_revision:    20241022
  thinking_level:    high
  thinking_tokens:   12450
  session_id:        sess-xyz
  invocation_id:     inv-789
```

### `mpm provenance inspect --invocation <id>`

Lists all artifacts created under one invocation. This is the correlation identity query.

```
$ mpm provenance inspect --invocation inv-789
Invocation: inv-789
Artifacts:
  mem-abc123  memory   2026-08-08 14:32:11 UTC
  the-def456  theory   2026-08-08 14:32:14 UTC
  les-ghi789  lesson   2026-08-08 14:32:18 UTC
```

### `mpm provenance model-yield [--framework X] [--days 30]`

Runs the `v_model_memory_yield` view. Returns provider/model, total created, survival %, reinforcements, contradictions.

```
$ mpm provenance model-yield --days 30
model              framework      created  reinforced  challenged  survived  survival_30d_pct
claude-sonnet-4-5  claude_code    812      421         37          421       51.8%
minimax-01         openclaw       421      188         12          231       54.9%
```

Per the spec-top orientation, the view is described as a **descriptive lifecycle measure, not a quality score**. The help text says:

> `model-yield` reports a descriptive lifecycle measure. Higher survival does not imply a better model. Survival is defined as: artifact has not been deleted, weight ≥ 1, and 30 days have elapsed since creation.

The `v_model_theory_utility` view is available via `mpm exec-sql` only. No dedicated CLI command in alpha — avoids premature CLI surface for an analytics model that will grow.

`mpm-critic` (03:00 UTC diagnostic report) does **not** incorporate model survival metrics in alpha. The data infrastructure is in place; the periodic report can read it once the substrate has accumulated enough rows to make a single-day delta meaningful.

---

## Migration plan

| Phase | Description |
|---|---|
| 1. Schema migration | Add `artifact_provenance` to `BaseTables` in `internal/core/schema.go`. `CREATE TABLE IF NOT EXISTS` means existing databases pick up the table on next launch. New `schema_fingerprint` test ensures the migration completes. |
| 2. Resolver plumbing | `NewFromEnv`, `Resolve`, `provenance.NewFromEnv()` integration. `DatabaseManager.ProvenanceResolver` field. |
| 3. Write hooks | `saveMemoryRow` and `AddLesson` call `RecordArtifactProvenance`. |
| 4. CLI surface | `mpm provenance <id>`, `mpm provenance inspect --invocation`, `mpm provenance model-yield`. |
| 5. Coverage tests | The 15 tests below. |
| 6. Schema fingerprint | The new schema is the new fingerprint. |

Each phase is independently PR-able. The schema migration can land first as a no-op; the write hooks land after the resolver is stable; the CLI surface comes last.

### Legacy `metadata.provenance.*` JSON

`saveMemoryWithContextImpl` stops injecting the JSON block. Existing rows are untouched. **No backfill of legacy JSON into the new table in alpha.** Old artifacts without provenance are not reconstructed or assigned synthetic provenance. The legacy rows are forensic history, not source data.

The existing `db.go` index that reads `json_extract(metadata, '$.provenance.model')` stays in place for back-compat with rows that pre-date the new table. Removing it is post-alpha.

---

## Test plan (15 tests)

| Test | Asserts |
|---|---|
| `TestProvenance_AllArtifactWritersRecord` | Every write path through `saveMemoryRow` and `AddLesson` produces exactly one `artifact_provenance` row. Mirrors `TestScannerCoverage_AllMemoriesWritersScanContent`. |
| `TestProvenance_InvocationCorrelatesMultipleArtifacts` | One `invocation_id` with N writes produces N rows; querying by `invocation_id` returns all N. |
| `TestProvenance_ValidationRejectsBadAPIEndpoint` | APIEndpoint with `?` or `@` is rejected; audit row written; no INSERT. |
| `TestProvenance_ValidationRejectsMalformedProviderMetadata` | ProviderMetadata that isn't a JSON object is rejected. |
| `TestProvenanceFailure_NeverPoisonsArtifactTransaction` | A forced provenance INSERT failure (e.g., schema mismatch) does not roll back the artifact INSERT. Counts are exact. |
| `TestProvenance_NewFromEnv_PriorityJSON` | `MPM_PROVENANCE` JSON wins over flat `MPM_PROVENANCE_*` vars. |
| `TestProvenance_NewFromEnv_FlatFallback` | Without `MPM_PROVENANCE`, flat vars populate the struct. |
| `TestProvenance_NewFromEnv_DefaultsUnknown` | With no env vars, `actor_kind="unknown"`, `framework_name`, `model_name` are NULL. |
| `TestProvenance_ResolverResolvePerCallOverrides` | Per-call sessionID/invocationID override the base. |
| `TestProvenance_DeclaredNotInferred` | A `CreationProvenance` with all fields empty produces `framework_name=NULL`, `model_name=NULL`, `thinking_level=NULL` — no inference from env vars or other fields. |
| `TestProvenance_SchemaVersionDefaultV1` | Every new row has `schema_version='v1'`. |
| `TestProvenance_OpaqueProviderMetadataRawPreservation` | Raw JSON byte-for-byte preservation: input `"{\"a\":1,\"b\":2}"` is stored as `"{\"a\":1,\"b\":2}"`, not re-marshaled. Whitespace and key order preserved. |
| `TestProvenance_ViewsReturnExpectedSchema` | `v_model_memory_yield` and `v_model_theory_utility` have the columns the design specifies. |
| `TestProvenance_NoDuplicateArtifactRecords` | A second `RecordArtifactProvenance` call for the same `(artifact_id, artifact_type)` returns `Recorded=false` with `SQLError` describing the UNIQUE constraint violation. |
| `TestProvenance_NullVsEmptySemantics` | env var unset → NULL; env var set to `""` → NULL; env var set to `"foo"` → `"foo"`. Distinct for `model_name`, `framework_name`, `thinking_level`, `actor_kind`. |

---

## Out of scope (post-alpha)

- Generalized external-ingestion interception (MCP server parsing, framework headers).
- Conflict resolution in MPM, federation, or shared DB.
- `mpm provenance audit` / `mpm provenance drift` commands.
- `mpm-critic` integration of model survival metrics.
- Backfill of legacy `metadata.provenance.*` JSON into the new table.
- Removal of the legacy `db.go` index that reads `metadata.provenance.model`.
- `mpm provenance theory-yield` (a CLI surface for `v_model_theory_utility`).
- `cache_read_tokens`, `cache_creation_tokens`, `reasoning_tokens`, `stop_reason`, `native_thinking_level`, `tool_choice`, `sampling_seed`, `context_window` — additive nullable fields. They will not bump `schema_version` (per the additive rule).

---

## Closing architectural principle

> **Provenance records what was declared about execution. Outcomes determine what that execution produced. MPM does not infer quality from provenance.**

This protects the telemetry system from becoming a disguised model-ranking system while leaving an unusually rich dataset for discovering what actually happens in the wild.
