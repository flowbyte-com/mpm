# MPM Embedding Provider Separation — Design Spec

**Date:** 2026-09-03
**Status:** Draft, pending user review
**Scope:** Replace the silent, env-only `EmbedText()` fallback chain with an explicit, config-driven, four-state embedding subsystem. Surface the existing/active state at every operator-visible point. Remediate the 791 poisoned HashEmbed rows and 65 fabricated collision records already in the live DB without deleting historical artifacts.

## 1. Background

The audit found that `internal/core/embeddings.go` exposes only two providers (`OllamaProvider` and `NullProvider`) and resolves embedding configuration entirely through `OLLAMA_ENDPOINT` and `OLLAMA_MODEL` env vars at startup, with a 2-second silent HTTP probe of `localhost:11434`. When the configured or probed provider is unreachable, `EmbedText()` substitutes a SHA-256-derived 256-dim vector (`HashEmbed`).

Measured over 19,900 unrelated text pairs, `HashEmbed` produces:

- mean cosine: **0.756**
- maximum cosine: **0.924**
- fraction above the 0.85 contradiction threshold: **2.16%**

The `hybrid_search.go` contradiction detector (lines 344-536) pairwise-compares the top-15 candidates and auto-challenges any pair above 0.85, slashing memory weight. The live database (`src/db/mpm.db`, 1081 active memories, all 256-dim) has already been damaged by this:

- **791 HashEmbed rows** (every stored vector).
- **65 fabricated collision records** (25 semantic + 4 provenance + 36 unresolved state).
- **194 challenged memories** with weight reductions derived from HashEmbed cosine.

`cmd/mpm/doctor` reports `175 / 1081 missing (16%) — semantic search degraded`, implying the other 84% are fine when they are SHA-256 hashes. The doctor also over-counts usable embeddings by 115 (it counts `IS NULL` but treats the literal string `'null'` as embedded; `VectorMatch` excludes both).

The brief asked whether LLM provider and embedding provider are accidentally coupled. **They are not.** LLM selection flows through `Config.ProfileFor(component)` against `mpm_config.json`; embedding selection flows through env vars + silent probe. The two paths never intersect, and `Config` has no embedding field. The brief's stated hypothesis is disproven, but the brief's success criterion — *"No silent 'everything works, except semantic retrieval is secretly a no-op' state"* — turns out to understate the real defect. The state today is worse than a no-op: it is active trust-machinery corruption.

This spec fixes that.

## 2. Goals

1. Embedding configuration is **explicit and operator-visible**. Every embedding write path (`mpm add`, MCP `SaveMemory`, `synthesis_auto.go:536`, `mpm ops backfill-embeddings`) reads from the same canonical resolution.
2. The system has **four operator-visible states**: `disabled`, `configured`, `unavailable`, `misconfigured`, plus the natural `absent` case. None of them silently substitutes a non-semantic vector.
3. **LLM and embedding configurations are structurally independent.** Local Ollama embeddings with a remote LLM is a first-class configuration, not a workaround.
4. **Back-compat is preserved** for operators who already use `OLLAMA_ENDPOINT` / `OLLAMA_MODEL`. Their setups keep working with the same effect, but their config is now reported as legacy fallback.
5. **The existing 791 HashEmbed rows and 65 fabricated collision records are classified forensically**, marked with provenance, and declassified from trust machinery without silent deletion.
6. **The latent `cosineSimilarity` panic** (`internal/core/db.go:3568`, unguarded length mismatch) is fixed at the function root, not at every call site.

## 3. Non-goals

- New embedding providers beyond the existing `ollama` (and any OpenAI-compatible endpoints the existing `Profile` struct already supports). No new vendor integrations.
- Changing the `Profile` struct shape or the LLM resolution path.
- Changing the storage format of the `embedding` column. The column stays as JSON-encoded float arrays or SQL NULL.
- Removing the legacy `SynthConfig` migration path. Out of scope.
- Changing `memory.go`'s `HashEmbed` function signature. It is removed from the default `EmbedText()` path; the function stays available as an internal helper for the migration's `embedding_source` forensic classifier and is otherwise unreachable.

## 4. Architecture

### 4.1 Canonical runtime precedence

```
components.embedding == "disabled"      →  NullProvider + IntentionallyDisabled=true   (NEVER falls through to env)
components.embedding == "<profile>"     →  resolve Profile via ProfileFor; check reachability on demand
components.embedding absent or ""       →  OLLAMA_ENDPOINT / OLLAMA_MODEL env fallback
neither                                  →  NullProvider
```

No network probing anywhere in `mpm add`, recall, synthesis, MCP, or daemon startup. Probing moves into an explicit operator-driven command (`mpm config detect-embedding`).

### 4.2 Configuration shape (`mpm_config.json`)

Embedding reuses the existing `profiles` / `components` abstraction. There is no top-level `embedding` block.

```json
{
  "profiles": {
    "local-ollama": {
      "provider": "ollama",
      "model": "nomic-embed-text",
      "base_url": "http://localhost:11434"
    }
  },
  "components": {
    "llm": "default",
    "embedding": "local-ollama"
  }
}
```

> **Note.** The `Profile` struct supports `base_url` and `api_key`, so an OpenAI-compatible embedding profile would slot in here once a corresponding provider implementation exists. This spec adds only `ollama`; other provider names return `Misconfigured` from `buildProvider`.

For "intentionally disabled":

```json
{
  "components": {
    "embedding": "disabled"
  }
}
```

The string `"disabled"` is a reserved sentinel for `components["embedding"]` only. It has no meaning for any other component and is rejected by `ProfileFor` for non-embedding components.

### 4.3 EmbeddingConfig (rebuilt)

```go
// internal/core/embeddings.go

type EmbeddingSource int

const (
    EmbeddingSourceProfile EmbeddingSource = iota
    EmbeddingSourceEnvFallback
    EmbeddingSourceDisabled
    EmbeddingSourceAbsent
)

func (s EmbeddingSource) String() string {
    switch s {
    case EmbeddingSourceProfile:
        return "profile"
    case EmbeddingSourceEnvFallback:
        return "env"
    case EmbeddingSourceDisabled:
        return "disabled"
    case EmbeddingSourceAbsent:
        return "absent"
    }
    return "unknown"
}

type EmbeddingStatus int

const (
    EmbeddingStatusConfigured EmbeddingStatus = iota // source reachable
    EmbeddingStatusUnreachable                       // profile set, provider did not respond
    EmbeddingStatusMisconfigured                     // profile set, provider/model/base_url invalid
    EmbeddingStatusNull                              // source is absent or env fallback did not produce a usable provider
)

func (s EmbeddingStatus) String() string {
    switch s {
    case EmbeddingStatusConfigured:
        return "configured"
    case EmbeddingStatusUnreachable:
        return "unavailable"
    case EmbeddingStatusMisconfigured:
        return "misconfigured"
    case EmbeddingStatusNull:
        return "null"
    }
    return "unknown"
}

type EmbeddingConfig struct {
    Source                EmbeddingSource
    ProfileName           string           // populated when Source=EmbeddingSourceProfile
    ProviderName          string           // "ollama:<model>", "openai:<model>", "null"
    Provider              EmbeddingProvider
    Status                EmbeddingStatus
    IntentionallyDisabled bool             // true iff Source=EmbeddingSourceDisabled
    LastError             error            // non-nil when Status=EmbeddingStatusUnreachable or EmbeddingStatusMisconfigured
}
```

`DefaultEmbeddingConfig()` (cached via `sync.Once`) returns this struct. Callers receive a snapshot of the resolved config; reachability is not probed at startup but is checked on demand by `EmbedText()` when `Status=EmbeddingStatusConfigured` is assumed.

`EmbedText(text string) ([]float32, error)` returns:

- `(vec, nil)` when the configured provider produced a vector of positive length.
- `(nil, nil)` when `Source=EmbeddingSourceDisabled` or `Source=EmbeddingSourceAbsent` and no env fallback is active.
- `(nil, err)` when the configured provider is reachable in principle but the call failed (network error, model-not-found, dimension zero, etc.).

The signature changes from `[]float32` to `([]float32, error)` so callers can surface the failure. Existing callers that previously called `EmbedText(text)` and assigned the result now call `vec, err := EmbedText(text)` and decide. HashEmbed is never returned.

### 4.4 Write-time contract

| Operation                        | Persist memory? | Embedding | Result |
| -------------------------------- | :-------------: | :-------: | :----: |
| Provider available               |       Yes       |  vector   | success |
| Provider disabled (sentinel)     |       Yes       |   NULL    | success |
| Provider env fallback available  |       Yes       |  vector   | success (legacy) |
| Provider env fallback unavailable|       Yes       |   NULL    | success |
| Provider unreachable             |       Yes       |   NULL    | **error** (CLI exit ≠ 0; MCP error response; synthesis operation fails) |
| Provider misconfigured           |       Yes       |   NULL    | **error** |
| Provider absent, no env          |       Yes       |   NULL    | success |

Invariant: **a transient embedding outage must never cause durable memory loss**. Persistence failure → no memory. Embedding failure → memory survives, operation reports failure.

`mpm add` on unreachable:

```
Memory saved, but embedding generation failed:
  embedding provider "local-ollama" is unreachable
The memory has been stored without an embedding.
Run `mpm ops backfill-embeddings` after the provider is available.
```

Exit code: non-zero.

MCP `SaveMemory` on unreachable:

```json
{
  "memory_persisted": true,
  "embedding_status": "unavailable",
  "backfill_required": true,
  "error": "embedding provider \"local-ollama\" is unreachable"
}
```

The MCP response is a structured error (not a generic success), so calling agents can distinguish persisted-without-embedding from full success.

Synthesis (`internal/core/synthesis_auto.go:536`) inherits the same rule. The only call site uses the embedding solely for storage; synthesis is not vector-dependent for candidate selection, ranking, or dedup. The call becomes:

```go
embedding, embedErr := EmbedText(result.Content)
if embedErr != nil {
    dm.LogAudit(AuditWarn, "synthesis", fmt.Sprintf("synthesis succeeded but embedding failed (synth=%s): %v", newSynthID, embedErr), "", AuditContext{})
    // embedErr surfaces to the AutoSynthesize caller; persisted memory has embedding=NULL.
}
```

The synthesizer records the warning so the audit log captures both halves of the result.

## 5. Schema migration

### 5.1 New columns on `memories`

```sql
ALTER TABLE memories ADD COLUMN embedding_source TEXT NOT NULL DEFAULT 'provider';
ALTER TABLE memories ADD COLUMN embedding_dimension INTEGER;
```

`embedding_source` ∈ `{'provider', 'hash', 'null'}`. The default `'provider'` is wrong for legacy rows but is corrected by the migration. `embedding_dimension` is the vector length; NULL when no embedding.

### 5.2 New indexes

```sql
CREATE INDEX IF NOT EXISTS idx_memories_embedding_source
  ON memories(embedding_source)
  WHERE deleted_at IS NULL;

CREATE INDEX IF NOT EXISTS idx_memories_embedding_dimension
  ON memories(embedding_dimension)
  WHERE deleted_at IS NULL;
```

Both are partial-indexes over live rows. `idx_memories_embedding_source` lets the contradiction detector, dedup, and doctor skip `embedding_source='hash'` rows cheaply. `idx_memories_embedding_dimension` lets dimension-aware vector queries find compatible rows.

### 5.3 New audit table

```sql
CREATE TABLE IF NOT EXISTS embedding_migration_log (
  id           INTEGER PRIMARY KEY AUTOINCREMENT,
  memory_id    TEXT NOT NULL,
  old_weight   REAL NOT NULL,
  new_weight   REAL NOT NULL,
  reason       TEXT NOT NULL,
  theory_id    TEXT,
  migrated_at  INTEGER NOT NULL,  -- Unix seconds
  operator     TEXT NOT NULL DEFAULT 'mpm-migration'  -- 'mpm-migration' or human
);

CREATE INDEX IF NOT EXISTS idx_embedding_migration_log_memory_id
  ON embedding_migration_log(memory_id);
```

The migration is reversible: every `embedding_migration_log` row is replayable in reverse by an undo command, restoring the original weight.

### 5.3.1 New column on `memories` for theory rows

```sql
ALTER TABLE memories ADD COLUMN synthetic INTEGER NOT NULL DEFAULT 0;
```

Theories in MPM are stored as `memories` rows with `collection = 'theories'` — there is no separate `theories` table. The `synthetic` column therefore lives on `memories`, and the migration applies the marker to theory-type rows via `WHERE collection = 'theories'`. The marker is set to `1` for theory rows whose evidence was a HashEmbed cosine. Default `0` for all existing rows. The migration flips to `1` for the 65 fabricated collision records (kind ∈ {`semantic_collision`, `provenance_collision`, `unresolved_state_collision`} where the source memory has `embedding_source='hash'`).

### 5.4 Forensic classifier

The classifier runs inside the migration transaction. It establishes provenance without mutating the live data; the mutations are applied in a separate, audit-logged step.

For every row in `memories WHERE deleted_at IS NULL`:

| Existing `embedding` value                      | Classified `embedding_source` | `embedding_dimension` |
| ----------------------------------------------- | :----------------------------: | :-------------------: |
| JSON-parseable `[]float32` of length 256        |           `'hash'`             |         256           |
| JSON-parseable `[]float32` of length ≠ 256      |           `'provider'`         |        (length)       |
| SQL `NULL`                                       |           `'null'`             |         NULL          |
| Literal string `'null'` (the storage convention) |           `'null'`             |         NULL          |

The audit confirmed every stored vector in the live DB is 256-dim and there is no reachable real provider, so every classified `'hash'` row is de facto HashEmbed. The classifier is deterministic: same input → same output. It is implemented as a single Go function with a unit test that pins the classifier output against a fixture of all four input shapes.

### 5.5 Provenance-gated un-challenge

The migration does **not** auto-un-challenge any memory whose `embedding_source` is `'hash'`. The gate is a pre-filter plus a strict gate plus an action-precondition. A memory `X` is un-challenged iff **all three** are true:

1. **Pre-filter (scope).** There exists at least one `theory` row `T` (i.e. a `memories` row with `collection = 'theories'`) with `T.memory_id = X` and `T.kind IN ('semantic_collision', 'provenance_collision', 'unresolved_state_collision')` AND `T.synthetic = 1` (after the marker is applied). This restricts the migration to candidate memories the audit identified as at-risk, avoiding a full-table scan.
2. **Strict gate.** **Every** `theory` row challenging `X` (regardless of kind, regardless of `synthetic`) has been marked `synthetic = 1`. If a single non-synthetic challenge remains against `X`, the migration does not modify `X`.
3. **Action-precondition.** `X.deleted_at IS NULL` AND `X.weight < X.origin_weight` (the memory is alive and currently has a reduced weight, so the un-challenge actually restores something).

If any of these fail, the migration does not modify `X`'s weight. The migration logs the decision per row (whether unchallenged, skipped, or no-op).

This avoids converting "the embedding signal was invalid" into "every trust decision derived from it was definitely wrong." If a memory happens to have both a HashEmbed-derived challenge and an independent challenge (e.g., a later operator-applied manual challenge, or a non-collision-kind theory), the memory keeps its weight reduction.

### 5.6 Idempotency

The migration command records a sentinel row in `embedding_migration_log` with `reason='migration_applied'` at the end of a successful run. The next invocation reads that sentinel and short-circuits with:

```
Migration already applied at <timestamp>.
hash rows: <n>
provider rows: <n>
null rows: <n>
synthetic theories: <n>
unchallenged memories: <n>
```

The sentinel is itself inside the `embedding_migration_log` table, so it is reversible by the same undo command.

### 5.7 Reversibility

Before any mutation, the migration:

1. Closes any open transaction.
2. Calls `VACUUM INTO '<workspace>/migrations/embeddings-<timestamp>.db.bak'` (a complete SQLite copy). Requires SQLite ≥ 3.27; the project already depends on a newer SQLite via `mattn/go-sqlite3`, so this is not a new constraint.
3. Begins the migration transaction.
4. Runs the forensic classifier (read-only pass that logs intended mutations).
5. Applies mutations inside the transaction.
6. Commits or rolls back.

`mpm ops migrate-embeddings --undo <timestamp>` replays `embedding_migration_log` in reverse: weight restoration first, then theory marker removal, then column drop. The backup is the last-resort rollback if the replay fails.

### 5.8 Trust-machinery updates

After the migration:

- `internal/core/hybrid_search.go` line 974 (`VectorMatch`) adds an `embedding_source != 'hash'` filter.
- `internal/core/hybrid_search.go` line 344-536 (`contradictionDetector`) skips candidates where `embedding_source IN ('hash', 'null')`.
- `internal/core/forge_dedup.go:174` adds the same `embedding_source != 'hash'` filter.
- `internal/core/vector_index.go:244` (vector index lookup) and `:624` (`Rebalance`) keep their existing dimension-mismatch warnings but additionally skip `embedding_source='hash'` rows.
- `internal/core/db.go:3568` (`cosineSimilarity`) gains a defensive length guard at the top:

```go
func cosineSimilarity(a, b []float32) float32 {
    if len(a) == 0 || len(b) == 0 {
        return 0
    }
    n := len(a)
    if len(b) < n {
        n = len(b)
    }
    var dotProduct, normA, normB float32
    for i := 0; i < n; i++ {
        dotProduct += a[i] * b[i]
        normA += a[i] * a[i]
        normB += b[i] * b[i]
    }
    if normA == 0 || normB == 0 {
        return 0
    }
    return dotProduct / (float32(math.Sqrt(float64(normA))) * float32(math.Sqrt(float64(normB))))
}
```

The existing guarded variant (`memory.go:1705 cosineSimilarityFloat32`) stays as-is. The new guard unifies both call sites against the same panic-free behavior.

## 6. Operator commands

### 6.1 `mpm config show`

Adds an `Embedding:` block:

```
Embedding
  source:    profile
  profile:   local-ollama
  provider:  ollama
  model:     nomic-embed-text
  status:    configured
```

For env fallback:

```
Embedding
  source:    env (legacy fallback)
  provider:  ollama
  model:     <model-from-env>
  status:    configured
```

For disabled:

```
Embedding
  source:    intentionally disabled
```

For unavailable:

```
Embedding
  source:    profile
  profile:   local-ollama
  provider:  ollama
  model:     nomic-embed-text
  status:    unavailable
  error:     connection refused
```

### 6.2 `mpm config detect-embedding`

Probes `localhost:11434` and any other common Ollama-compatible endpoints declared in the config (default: just `localhost:11434`). Reports reachable status and discovered models. Never mutates configuration.

```
Embedding provider detection

Ollama
  http://localhost:11434
  reachable: yes
  models:
    nomic-embed-text
    all-minilm:latest

No configuration has been changed.
```

### 6.3 `mpm config detect-embedding --apply <profile-name>`

Same probe; on success, writes `<profile-name>` into `profiles` and sets `components["embedding"] = <profile-name>`. Logs the diff:

```
Wrote profile "local-ollama" (provider=ollama, model=nomic-embed-text).
Set components["embedding"] = "local-ollama".
```

This is an explicit configuration change. Without `--apply`, no mutation occurs. The command refuses to overwrite an existing profile unless `--force` is supplied.

### 6.4 `mpm ops backfill-embeddings`

Already refuses to run with no provider at `cmd/mpm/backfill_embeddings.go:69-71`. Tightens the message and uses the canonical resolution:

```
Refusing to backfill: no embedding provider is configured and reachable.
Resolve one of:
  - Run `mpm config detect-embedding --apply <name>` to discover and configure.
  - Set `components["embedding"]` in mpm_config.json.
  - Set OLLAMA_ENDPOINT and OLLAMA_MODEL in the environment.
```

### 6.5 `mpm ops migrate-embeddings`

One-shot migration command. Idempotent. Reversible.

```
$ mpm ops migrate-embeddings
Pre-migration backup: <workspace>/migrations/embeddings-2026-09-03T14-50-00Z.db.bak
Forensic classification: 791 hash, 290 provider, 0 null
Theories marked synthetic: 65
Memories auto-unchallenged: 194 (out of <candidates>)
Migration applied.
```

`mpm ops migrate-embeddings --undo <timestamp>` reverses the migration using the same backup + audit log.

## 7. Diagnostics — Doctor & Readiness

### 7.1 `mpm doctor` — `checkEmbeddings()`

Counts by `embedding_source` and reports:

| Condition                                    | Status  | Message                                                           |
| -------------------------------------------- | :-----: | ----------------------------------------------------------------- |
| `hash=0 AND null/total ≤ 0.10`               | `PASS`  | `<n> memories, all from real provider`                            |
| `hash=0 AND null/total > 0.10`               | `WARN`  | `<n> / <total> without embedding — run `mpm ops backfill-embeddings` |
| `hash>0`                                     | `WARN`  | `<n> legacy hash rows — run `mpm ops migrate-embeddings`         |
| `hash>0 AND provider=0` (no real embeddings) | `FAIL`  | `<n> legacy hash rows; no real embeddings persisted; semantic search fully degraded` |

The new `checkEmbeddingProvider()` runs after `checkEmbeddings()`. It reads the canonical `EmbeddingConfig` and reports:

| `EmbeddingConfig.Status` | Doctor status |
| ------------------------ | :-----------: |
| `EmbeddingStatusConfigured` + source profile + at least one successful `EmbedText()` this process | `PASS` |
| `EmbeddingStatusConfigured` + source profile + no `EmbedText()` yet this process | `PASS` with note "configured (not yet verified at runtime)" |
| `EmbeddingStatusConfigured` + source env     | `PASS` (with note "legacy env fallback") |
| `EmbeddingStatusUnreachable`                  | `WARN`  |
| `EmbeddingStatusMisconfigured`                | `WARN`  |
| `EmbeddingStatusNull` + disabled             | `PASS` (with note "intentionally disabled") |
| `EmbeddingStatusNull` + absent               | `WARN` (no provider configured; backfill will be a no-op) |

`checkEmbeddingProvider()` does **not** perform a network probe. It reads the cached `EmbeddingConfig` and surfaces whatever `EmbedText()` would see at this moment. For a fresh process where no `EmbedText()` call has happened yet, `Status=EmbeddingStatusConfigured` is reported as "configured (not yet verified at runtime)" rather than overclaiming reachability.

### 7.2 `mpm readiness` — `checkEmbeddings()`

Replaces the unconditional `OK: true` with the four-state model:

| State                       | Readiness result                                                  |
| --------------------------- | ----------------------------------------------------------------- |
| `disabled`                  | `OK: true, note: "embedding intentionally disabled"`              |
| `configured`                | `OK: true, note: "embedding provider reachable"`                  |
| `unavailable`               | `FAIL: "embedding provider configured but unreachable"`          |
| `misconfigured`             | `FAIL: "embedding provider misconfigured: <reason>"`              |
| `absent`                    | `WARN: "no embedding provider configured"`                        |

## 8. Configuration resolution details

### 8.1 New `ProfileFor("embedding")` semantics

```go
func (c *Config) ProfileFor(component string) *Profile {
    if c == nil {
        return nil
    }
    if component == "embedding" {
        if c.Components != nil {
            if name, ok := c.Components["embedding"]; ok {
                if name == "disabled" {
                    return nil  // sentinel; resolver maps to NullProvider + IntentionallyDisabled=true
                }
                if name != "" {
                    if p, ok := c.Profiles[name]; ok {
                        cp := p
                        cp.Name = name
                        return &cp
                    }
                    // ProfileFor returns nil here; the embedding resolver
                    // treats "named but missing" as misconfigured.
                }
            }
        }
        // components["embedding"] absent or empty → env fallback
        return nil  // resolver probes env
    }
    // existing logic for non-embedding components, unchanged.
    ...
}
```

For other components, `"disabled"` is **not** a sentinel. If a non-embedding component has `components["x"] = "disabled"`, `ProfileFor` returns nil (same as a missing binding), and the resolver treats it as misconfigured for that component. The string `"disabled"` is reserved for embedding only.

### 8.2 Resolution function

```go
// internal/core/embeddings.go

func resolveEmbeddingConfig(c *config.Config) *EmbeddingConfig {
    // 1. components.embedding == "disabled"
    if c != nil && c.Components != nil {
        if name, ok := c.Components["embedding"]; ok && name == "disabled" {
            return &EmbeddingConfig{
                Source:                EmbeddingSourceDisabled,
                ProviderName:          "null",
                Provider:              NullProvider{},
                Status:                EmbeddingStatusNull,
                IntentionallyDisabled: true,
            }
        }
    }

    // 2. components.embedding == "<profile>" via ProfileFor
    if c != nil && c.Components != nil {
        if name, ok := c.Components["embedding"]; ok && name != "" {
            p := c.ProfileFor("embedding")
            if p == nil {
                return &EmbeddingConfig{
                    Source:       EmbeddingSourceProfile,
                    ProfileName:  name,
                    ProviderName: "null",
                    Provider:     NullProvider{},
                    Status:       EmbeddingStatusMisconfigured,
                    LastError:    fmt.Errorf("profile %q referenced by components.embedding does not exist", name),
                }
            }
            // Validate the profile has the minimum fields for an embedding provider.
            if err := validateEmbeddingProfile(p); err != nil {
                return &EmbeddingConfig{
                    Source:       EmbeddingSourceProfile,
                    ProfileName:  name,
                    ProviderName: "null",
                    Provider:     NullProvider{},
                    Status:       EmbeddingStatusMisconfigured,
                    LastError:    err,
                }
            }
            prov, provErr := buildProvider(p)
            if provErr != nil {
                return &EmbeddingConfig{
                    Source:       EmbeddingSourceProfile,
                    ProfileName:  name,
                    ProviderName: providerName(p),
                    Provider:     NullProvider{},
                    Status:       EmbeddingStatusMisconfigured,
                    LastError:    provErr,
                }
            }
            return &EmbeddingConfig{
                Source:       EmbeddingSourceProfile,
                ProfileName:  name,
                ProviderName: providerName(p),
                Provider:     prov,
                Status:       EmbeddingStatusConfigured,  // optimistic; embed() may downgrade on first failure
            }
        }
    }

    // 3. components.embedding absent or "" → env fallback
    endpoint := os.Getenv("OLLAMA_ENDPOINT")
    model := os.Getenv("OLLAMA_MODEL")
    if endpoint != "" || model != "" {
        if endpoint == "" {
            endpoint = "http://localhost:11434/api/embeddings"
        }
        if model == "" {
            model = "nomic-embed-text"
        }
        return &EmbeddingConfig{
            Source:       EmbeddingSourceEnvFallback,
            ProviderName: "ollama:" + model,
            Provider:     NewOllamaProvider(endpoint, model),
            Status:       EmbeddingStatusConfigured,  // optimistic; same caveat
        }
    }

    // 4. neither
    return &EmbeddingConfig{
        Source:       EmbeddingSourceAbsent,
        ProviderName: "null",
        Provider:     NullProvider{},
        Status:       EmbeddingStatusNull,
    }
}
```

`buildProvider(p *Profile)` branches on `p.Provider`: `"ollama"` returns `NewOllamaProvider(p.BaseURL, p.Model)`. The audit found exactly two embedding-provider implementations in the codebase (`OllamaProvider` and `NullProvider`), so any other provider name returns `Misconfigured`. This is a deliberate constraint of this spec — adding OpenAI-compatible embedding support would require a new provider implementation and is out of scope here. The `Profile` struct already supports `BaseURL`/`APIKey`, so a future spec can add a generic OpenAI-compatible embedding client without changing the config shape.

`validateEmbeddingProfile(p)` enforces minimum fields: `Provider != ""`, `Model != ""`. For `"ollama"`, `BaseURL` may be empty (the constructor defaults to `localhost:11434`). Any other provider name fails validation immediately — there is no constructor for it yet.

`providerName(p)` formats the human-readable name: `"ollama:nomic-embed-text"`, `"openai:text-embedding-3-small"`, etc.

### 8.3 `EmbedText` reachability downgrade

```go
func EmbedText(text string) ([]float32, error) {
    cfg := DefaultEmbeddingConfig()
    switch cfg.Source {
    case EmbeddingSourceDisabled, EmbeddingSourceAbsent:
        return nil, nil
    case EmbeddingSourceProfile, EmbeddingSourceEnvFallback:
        vec, err := cfg.Provider.Embed(text)
        if err != nil {
            // Downgrade status to Unreachable for this process's lifetime.
            // The cached config is mutated; sync.Once is not re-invoked.
            cfg.Status = EmbeddingStatusUnreachable
            cfg.LastError = err
            return nil, err
        }
        if len(vec) == 0 {
            return nil, fmt.Errorf("embed: provider %q returned zero-length vector", cfg.ProviderName)
        }
        return vec, nil
    }
    return nil, nil
}
```

The status downgrade is in-process. A daemon restart re-resolves. There is no background probe — the next `EmbedText()` after a successful call leaves the status at `Configured`. The first failure after a success transitions to `Unreachable`; the diagnostics surfaces it.

## 9. Testing strategy

### 9.1 Unit tests

- `resolveEmbeddingConfig`: every precedence branch (disabled, profile+valid, profile+missing-profile, profile+invalid, env fallback, absent).
- `EmbedText`: returns `nil, nil` for `disabled` and `absent`; returns `nil, err` when provider fails; returns `vec, nil` when provider succeeds.
- `cosineSimilarity`: panic-free across `len(a) != len(b)` including the historically-panicking 384 vs 256 case.
- `HashEmbed` removal: every previously-calling site now calls `EmbedText` and handles the error path.
- `validateEmbeddingProfile`: rejects profiles missing required fields; accepts Ollama without `api_key`; rejects non-Ollama without `api_key`.

### 9.2 Regression tests

- Forensic classifier: a fixture row set (one of each input shape: 256-dim JSON, 384-dim JSON, SQL NULL, literal `'null'`) is classified exactly as specified in §5.4. The fixture is committed; the test pins the output.
- Live-DB regression: a read-only test that opens the live DB (or a fixture copy) and asserts that the migration would mark the expected counts. This test skips when the DB doesn't exist.
- Sentinel resolution: `ProfileFor("embedding") == "disabled"` returns nil and the resolver produces `IntentionallyDisabled=true`. `ProfileFor("llm") == "disabled"` returns nil and the resolver treats it as misconfigured.

### 9.3 Integration tests

- `mpm add`: writes the memory with `embedding` populated when the configured provider is reachable; writes NULL and exits non-zero when unreachable; writes NULL and exits zero when disabled/absent.
- MCP `SaveMemory`: returns the structured error shape from §4.4 when provider unreachable; returns success when reachable; returns success-with-NULL when disabled.
- `mpm ops migrate-embeddings`: first run applies the migration; second run short-circuits with the idempotent message; `--undo` restores the original state.
- `mpm config detect-embedding --apply`: writes a profile and a `components["embedding"]` binding; refuses to overwrite without `--force`.

### 9.4 Probe-removal proof

A test with a mocked HTTP transport counts requests during a full `mpm add` cycle with `components.embedding` absent and env vars unset. **Zero requests** are permitted. This test fails if any code path reintroduces silent probing.

### 9.5 Live Ollama verification (when available)

When `OLLAMA_ENDPOINT` is reachable from the test environment, an end-to-end test runs:

1. Configure `components["embedding"] = "<test-profile>"` pointing at the test endpoint.
2. Run `mpm add` with a known content string.
3. Assert the persisted memory has `embedding_source='provider'`, `embedding_dimension=384` (or whatever the configured model returns), and a non-null `embedding` column.
4. Run `mpm recall --semantic "<query>"` and assert the just-added memory appears in the top results.

This test is gated on environment availability and is skipped (not failed) when no Ollama is reachable.

## 10. Documentation

### 10.1 New: `docs/embedding-config.md`

Operator-facing guide covering:

- The four states (`disabled`, `configured`, `unavailable`, `misconfigured`, plus `absent`).
- The canonical precedence (§4.1) and why each step exists.
- Worked examples for Ollama and the `disabled` sentinel. (OpenAI-compatible embedding support requires a new provider implementation; out of scope for this spec.)
- The env-var fallback (when it kicks in, why it is preserved, why operators should migrate to the profile).
- Migration from env-only setups: `mpm config detect-embedding --apply <name>` writes the equivalent profile.
- `mpm ops migrate-embeddings` — what it does, what it doesn't, and how to undo it.

### 10.2 Updates to existing docs

- `mpm_config.json` example: add `components.embedding`.
- `internal/core/embeddings.go` package doc: rewritten to describe the four states and the canonical precedence.
- `docs/CONTRIBUTING.md`: remove "set `OLLAMA_*` in your env" instructions; replace with the profile-based workflow.
- `docs/INSTALL.md`: remove any "after install, set Ollama env vars" steps.
- `docs/SECURITY.md`: no change (the env-var fallback does not relax any existing security boundary).

### 10.3 Inline code comments

- `internal/core/embeddings.go`: top-of-file comment block naming the canonical precedence and the rationale for removing `probeEmbeddingConfig` from runtime.
- `internal/core/memory.go`: the `HashEmbed` function gets a doc comment explaining it is **not** called by `EmbedText` and is retained only as a forensic-classifier helper.
- `internal/core/hybrid_search.go`: line 344 comment explaining the new `embedding_source != 'hash'` filter.
- `internal/core/db.go:3568`: the `cosineSimilarity` function gets a doc comment describing the panic-free contract.

## 11. Rollout

### 11.1 Phasing

The change is delivered as a series of focused commits:

1. **Schema-only migration** (additive): `embedding_source` and `embedding_dimension` columns on `memories` + indexes; `synthetic` column on `theories`; `embedding_migration_log` table. Backfill the existing 1081 rows with the classifier output. No behavior change yet — `HashEmbed` is still the default fallback, but every row now carries provenance.
2. **Probe removal** (silent → explicit): `probeEmbeddingConfig()` is removed from the runtime path. `DefaultEmbeddingConfig()` reads profile → env → null. `mpm config detect-embedding` is added. Existing operators with `OLLAMA_*` set are unaffected.
3. **HashEmbed removal**: `EmbedText` returns `nil, err` instead of falling back. All call sites (`cmd/mpm/simple_cmds.go:102`, MCP, synthesis) gain explicit error handling.
4. **Trust-machinery updates**: contradiction detector, dedup, vector index, `cosineSimilarity` guard. These activate only after step 1 has classified the rows.
5. **Diagnostics**: doctor + readiness + `mpm config show` gain the four-state model.
6. **Migration command**: `mpm ops migrate-embeddings` becomes the operator-driven un-challenge path, idempotent and reversible.

Each step is independently committable, runnable, and testable. Steps 1-4 land before step 6 because step 6 only makes sense once the columns exist.

### 11.2 Back-compat matrix

| Pre-change operator setup                       | Post-change behavior                                                                                  |
| ----------------------------------------------- | ----------------------------------------------------------------------------------------------------- |
| `OLLAMA_ENDPOINT` set, Ollama reachable, no `components.embedding` | `Source=EmbeddingSourceEnvFallback`, memory stored with real vector. Identical to today's success path. |
| `OLLAMA_ENDPOINT` set, Ollama **down**, no `components.embedding` | `Source=EmbeddingSourceEnvFallback`, memory stored with NULL embedding, CLI exit non-zero. Differs from today (today would have produced HashEmbed silently). This is the intended "no silent semantic failure" fix. |
| No env vars, no `components.embedding`           | `Source=EmbeddingSourceAbsent`, `EmbedText` returns `nil` (was: HashEmbed). Operator must configure |
| `components.embedding = "disabled"`             | `Source=EmbeddingSourceDisabled`, `EmbedText` returns `nil`. New behavior                            |
| `components.embedding = "<profile>"` (new)      | `Source=EmbeddingSourceProfile`, uses the profile                                                     |

### 11.3 Failure modes & operator recovery

| Symptom                                          | Operator action                                                              |
| ------------------------------------------------ | ---------------------------------------------------------------------------- |
| `mpm add` exits non-zero with "embedding unreachable" | Check the provider; `mpm config show` reports status.                       |
| `mpm doctor` shows `hash>0` after migration      | The migration was never run; `mpm ops migrate-embeddings` runs it.          |
| `mpm ops backfill-embeddings` refuses            | No provider reachable; configure or wait.                                    |
| `mpm ops migrate-embeddings --undo` is needed    | The undo path replays `embedding_migration_log` in reverse.                  |
| Migration accidentally re-run                    | Idempotent: short-circuits with "already applied" message.                   |

## 12. Open questions / future work

- **Per-process reachability cache TTL.** Today the `EmbeddingConfig` is cached for the process lifetime and the status downgrade on first failure persists. A future enhancement could re-probe on a timer, but this spec explicitly avoids that complexity.
- **Schema for `embedding_source='provider'` dimension column.** Currently the column is INTEGER. A future spec could move to a typed `(model_id, dim)` pair if multiple real embedding models become first-class.
- **Vector index lifecycle.** `vector_index.Rebalance` already handles mixed dimensions by warning-and-skipping. A future spec could partition the index by dimension.
- **Operator-driven manual challenge resolution.** Today an operator who wants to manually clear a challenge runs SQL. A future `mpm ops challenges` command could expose this.
- **Profile hot-reload.** Today `mpm_config.json` is read at process start. Future: SIGHUP / config-watcher could re-resolve without restart.

## 13. Verification — required before merge

- All existing tests pass (`go test -tags fts5 ./internal/core/...` and `./cmd/...`).
- New unit tests pass (resolution, EmbedText, HashEmbed removal, cosine guard).
- New regression tests pass (forensic classifier fixture, live-DB pin).
- New integration tests pass (`mpm add`, MCP, migration, detect-embedding).
- Probe-removal proof test passes (zero HTTP requests during `mpm add` with no embedding config).
- Live Ollama verification test passes when an endpoint is reachable; skips otherwise.
- `mpm-lint --gate` passes (no fatal-class violations).
- `git diff --stat` shows changes scoped to: `internal/core/embeddings.go`, `internal/core/db.go`, `internal/core/memory.go`, `internal/core/hybrid_search.go`, `internal/core/vector_index.go`, `internal/core/forge_dedup.go`, `internal/core/config/config.go`, `internal/core/synthesis_auto.go`, `cmd/mpm/simple_cmds.go`, `cmd/mpm/backfill_embeddings.go`, `cmd/mpm/service_doctor.go`, `cmd/mpm/readiness.go`, `cmd/mpm/migrate_embeddings.go` (new), `cmd/mpm/detect_embedding.go` (new), `mpm-agent/...` MCP surface, plus the new docs and migration fixture.
