# Pointer Architecture for MCP Output Bounding — Design Spec

**Date:** 2026-08-21
**Status:** Design — pending implementation
**Scope:** Phase 1 of a pointer architecture for mpm. Adds a generic pointer substrate (`internal/pointer/`), a consumer-neutral blob store (`internal/blobstore/`), and a decision-only output policy (`OutputPolicy`) applied at the `mpm-mcp` boundary only. Ships three new tools (`mpm_resolve`, `mpm_blob_read`, `mpm_blob_search`) and one new CLI command (`mpm blob gc`). CLI semantics are unchanged. No existing tool returns pointer-native results in Phase 1 — pointers are introduced only as the spill mechanism for oversized MCP tool responses.

---

## Spec-top orientation

> **A tool produces a result. The consumer decides how that result may be represented.**

The substrate persists cognitive artifacts (memories, lessons, decisions, theories, evidence, projections). Phase 1 does not change any of that. It adds a single new behavior: when an MCP tool result would exceed a configurable serialized byte budget, `mpm-mcp` writes the result to a filesystem blob, replaces it with a small envelope containing a `mpm://blob/<id>` pointer, and exposes bounded primitives (`mpm_blob_read`, `mpm_blob_search`) so the agent can paginate the spill on demand.

This is a **transport / context protection layer**, not a redesign of mpm's cognitive semantics. `mpm recall` still returns full content when called via MCP, until the result exceeds the budget — at which point it returns a pointer envelope. Phase 2 will extend this pattern to make pointer projection a first-class retrieval mode (`mpm://memory/<id>`, `mpm://lesson/<id>`, projections, etc.). Phase 1 deliberately scopes to blob-only resolution to ship the substrate without committing to Phase 2's surface.

The architectural principle that makes this safe: the consumer boundary owns the representation policy. The tool boundary owns the semantic correctness. They do not overlap.

---

## Architectural invariants

These nine invariants are load-bearing. Every implementation decision must preserve them.

1. **MCP is the only Phase 1 enforcement point.** `OutputPolicy.Apply` is invoked from `mcpAdapter()` in `cmd/mpm-mcp/tools.go` only. `internal/core/tools` remains unaware that one consumer happens to be an LLM context window.
2. **CLI semantics are unchanged.** `mpm call <tool>` returns the same full result as before Phase 1 (regression criterion, not opinion).
3. **`OutputPolicy` is decision-only.** It measures serialized result bytes and returns `Pass | Spill`. It does **not** call `BlobStore` or construct pointers — `mcpAdapter()` orchestrates those.
4. **The size boundary is the final serialized MCP tool result.** Threshold is strict: `serialized_bytes > MPM_MCP_MAX_RESULT_BYTES → Spill` (not `>=`). The limit applies to the bytes that would otherwise cross the MCP wire — not an intermediate Go representation.
5. **The pointer parser accepts the generic `mpm://<kind>/<id>` URI grammar.** Phase 1 resolution supports only `mpm://blob/<id>`; other kinds parse successfully but return `ErrUnsupportedKind`.
6. **Blob storage is not authoritative state.** If a blob disappears because its TTL expired, that's acceptable. Persistent mpm artifacts stay in SQLite. The substrate's cognitive semantics do not depend on blob existence.
7. **Spill failure is never silent.** If a result exceeds the limit and `BlobStore.Put` fails, `mcpAdapter` returns a bounded explicit MCP error. It never returns a partial payload and never claims success. **No successful MCP result is ever truncated.**
8. **Blob publication and metadata insertion are not a single atomic operation.** Crash-induced inconsistencies are recovered by orphan GC. **Blob metadata and payload may temporarily disagree after a crash; neither inconsistency may cause incorrect data to be returned.**
9. **Phase 1's in-flight unlink semantics are guaranteed on POSIX.** Windows behavior is explicitly outside this guarantee and must not be inferred from the POSIX model.

### State machine (the no-fourth-branch invariant)

```
Apply → Decision ─┬── Pass ──────────► success / full result
                  │
                  └── Spill
                       │
                       ├── Put succeeds ─► success / pointer envelope
                       │
                       └── Put fails    ─► bounded error
```

There is no fourth branch. A spill failure is a failure, not a partial success.

---

## The architectural boundary

```
                  ┌──────────────────────────┐
                  │ internal/core/tools      │
                  │ Tool implementations     │
                  │ + Registry               │
                  │ (unchanged semantics)    │
                  └────────────┬─────────────┘
                               │
                  ┌────────────┴────────────┐
                  │                         │
                  ▼                         ▼
          CLI consumer                MCP consumer
          cmd/mpm/call                cmd/mpm-mcp
                  │                         │
                  │                         │ OutputPolicy (decision only)
                  │                         │   │
                  │                         │   ├── Pass  ─► return result
                  │                         │   │
                  │                         │   └── Spill ─► BlobStore.Put
                  │                         │                  │
                  │                         │                  ▼
                  │                         │              mpm://blob/<id>
                  │                         │                  │
                  │                         │                  ▼
                  │                         │             spill envelope
                  ▼                         ▼
              stdout                   MCP result
           (full result)             (bounded result)
```

---

## Components

### `internal/blobstore/` — consumer-neutral filesystem payload + SQLite metadata

```go
type BlobStore interface {
    Put(ctx context.Context, content io.Reader, meta Metadata) (Pointer, error)
    Get(ctx context.Context, id string, opts GetOptions) (io.ReadCloser, Metadata, error)
    Search(ctx context.Context, id string, query SearchQuery) ([]Match, error)
    Delete(ctx context.Context, id string) error
    GCExpired(ctx context.Context, now time.Time) (GCStats, error)
    GCSweepOrphans(ctx context.Context, grace time.Duration) (GCStats, error)
}
```

**Filesystem layout:** `$MPM_WORKSPACE/blobs/<id>` (no extension — content_type lives in metadata).

**Atomic publication ordering** (in `Put`):

```
write <id>.tmp → fsync → rename(<id>) → INSERT metadata → return Pointer
```

Stale `.tmp` cleanup is **GC's job, not Put's**. Put never reaps filesystem garbage.

**`Put` failure semantics:**

- Temp write fails → no orphan; return error
- Fsync fails → `.tmp` left behind; GC sweeps after grace
- Rename fails (target exists, UUID collision 2⁻¹²²) → retry with new UUID
- INSERT fails → filesystem orphan; GC sweeps after grace (1h)

**`Get` semantics:**

| State | Behavior |
|---|---|
| DB row + file present | Stream bytes (offset + max_bytes range) |
| DB row + file missing | Return `ErrBlobMissing` (recoverable MCP error) |
| DB row + file truncated | Return available bytes up to actual size; `next_offset = size` |
| `offset >= size` | Return `{content: "", bytes_returned: 0, next_offset: offset, has_more: false}` (success, EOF) |
| DB row absent | Return `ErrBlobNotFound` |

**`Get` POSIX unlink-after-open contract:** GC may unlink a blob while a reader has it open. On POSIX, the open file descriptor remains readable, so an in-flight read completes normally. A subsequent read using the pointer observes `ErrBlobMissing`.

**`Delete` semantics:** Best-effort, idempotent. Order: `DELETE FROM blobs WHERE id=?` → `os.Remove(payload_path)`. If the second fails, the next GC sweep picks it up as an orphan. Caller treats both as success if DB row is gone.

**`GCExpired` ordering:** identify expired IDs → delete payloads → delete DB rows. **GC is idempotent and any crash between filesystem and metadata cleanup leaves only a recoverable orphan/missing-row state.**

**`GCSweepOrphans`:** Walk `blobs/` for files. For each: if file age (mtime) > grace period AND no DB row → delete. For each: if DB row present but file missing → log warning, delete orphan DB row.

### `internal/pointer/` — consumer-neutral URI grammar

```go
type Pointer struct {
    Kind string  // "blob" in Phase 1
    ID   string
}

func Parse(uri string) (Pointer, error)
func (p Pointer) URI() string

type Resolver interface {
    Resolve(ctx context.Context, p Pointer, opts ResolveOptions) (Resolution, error)
}

type Resolution struct {
    ContentType string
    Reader      io.ReadCloser
    Metadata    map[string]any
}

var ErrUnsupportedKind = errors.New("pointer: unsupported kind for Phase 1")
```

Phase 1 supports `mpm://blob/<id>` only. Parser accepts any `mpm://<kind>/<id>` syntactically; resolver rejects `kind != "blob"` with `ErrUnsupportedKind`.

**Pointer parsing errors:**

| Input | Result |
|---|---|
| `mpm://blob/abc123` | `Pointer{Kind: "blob", ID: "abc123"}` ✓ |
| `mpm://memory/42` | `Pointer{Kind: "memory", ID: "42"}` (parses; resolve rejects) |
| `mpm://` | `ErrPointerMalformed` |
| `mpm://blob/` | `ErrPointerMalformed` |
| `mpm://blob/abc!def` | `ErrPointerMalformed` (ID must match `[a-z0-9-]+`) |
| `not-a-uri` | `ErrPointerMalformed` |
| `https://...` | `ErrPointerWrongScheme` |

### `internal/core/tools/output_policy.go` — decision only

```go
type Decision int
const (
    DecisionPass Decision = iota // serialized_bytes <= MPM_MCP_MAX_RESULT_BYTES
    DecisionSpill                  // serialized_bytes > MPM_MCP_MAX_RESULT_BYTES
)

type OutputPolicy interface {
    Apply(ctx context.Context, result any) (Decision, int, error)
}

func DefaultOutputPolicy() OutputPolicy
```

`DefaultOutputPolicy` reads `MPM_MCP_MAX_RESULT_BYTES` (default 10240), marshals result to JSON to measure the actual wire bytes, returns the decision. Strict inequality (`>`, not `>=`) — exactly-at-limit results pass.

### `cmd/mpm-mcp/tools.go` — orchestration

```go
decision, bytes, err := outputPolicy.Apply(ctx, result)
switch decision {
case DecisionPass:
    // unchanged — return result as MCP text content
case DecisionSpill:
    // mcpAdapter serializes once (reuse the bytes from Apply),
    // calls BlobStore.Put, builds spill envelope.
    ptr, putErr := blobStore.Put(ctx, bytes.NewReader(serialized), metadata)
    if putErr != nil {
        return mcp.NewToolResultError("internal: spill failed; result suppressed"), nil
    }
    return mcp.NewToolResultText(spillEnvelope(ptr, bytes, metadata)), nil
}
```

### Spill envelope (stable schema, locked in Phase 1)

```json
{
  "status": "spilled",
  "pointer": "mpm://blob/9a8b7c",
  "size_bytes": 145000,
  "content_type": "application/json",
  "source_tool": "mpm_recall",
  "preview": {
    "kind": "json",
    "approx_items": 2431,
    "first_keys": ["...", "..."]
  },
  "expires_at": "2026-08-22T14:00:00Z"
}
```

`status: "spilled"` — never `"success_truncated"`. Preview answers "should I retrieve?", never "do I know the artifact?". `approx_items` is genuinely approximate; it must not become a factual claim. `first_keys` returns up to 5 top-level JSON keys (truncated to 80 chars each) when `content_type` is `application/json`; absent for other content types.

### Three new MCP tools (entries in `internal/core/tools/registry_list.go`)

| Tool | Args | Returns |
|---|---|---|
| `mpm_resolve` | `{"uri": "mpm://blob/<id>", "max_bytes": N}` | Bounded materialization. Phase 1 only `mpm://blob/<id>`; other kinds → `ErrUnsupportedKind` |
| `mpm_blob_read` | `{"id": "...", "offset": N, "max_bytes": N}` | Server-side byte cap (default 50 KB, ceiling 256 KB); returns `{content, content_type, offset, bytes_returned, next_offset, has_more}`. `content` is returned inline for `text/*` and `application/json` content types; binary content types return a bounded MCP error (no base64 transport in Phase 1). |
| `mpm_blob_search` | `{"id": "...", "query": "...", "regex": bool, "case_insensitive": bool, "max_matches": N, "max_bytes": N}` | Server-side grep using Go's RE2 regex engine (no catastrophic-backtracking risk); bounded by `max_matches` (default 20, ceiling 100) AND `max_bytes` (default 50 KB, ceiling 256 KB); regex max length 512 chars, query max length 256 chars, max scan size 100 MB. Returns `{matches: [{line_no, byte_offset, snippet}], match_count, truncated, bytes_returned}` where `snippet` is the matched line. |

Available to both CLI (`mpm call mpm_resolve ...`) and MCP — by construction, since they're registry entries. No special-casing; if a future tool wants CLI gating, that's a registry flag, not a duplication.

### `mpm blob gc [--dry-run]` — CLI command

Two-pass execution:

1. **Expired registered blobs:** `DELETE FROM blobs WHERE expires_at < ?` then remove their filesystem payloads.
2. **Orphan filesystem blobs:** walk `blobs/` and `blobs/*.tmp`; delete files with `mtime > MPM_BLOB_ORPHAN_GRACE` and no DB row. For DB rows with missing files: log warning, delete orphan DB row.

`--dry-run` reports counts and bytes without deleting. Crash-safe: each pass is idempotent.

---

## Data flow

### Pass path (small result)

```
tool handler returns result
  │
  ▼
mcpAdapter receives result
  │
  ▼
OutputPolicy.Apply(result, ctx)
  │  marshal to JSON; measure bytes
  │  serialized_bytes <= MPM_MCP_MAX_RESULT_BYTES
  ▼
DecisionPass
  │
  ▼
return mcp.NewToolResultText(json.Marshal(result))   // unchanged behavior
```

### Spill path (large result)

```
tool handler returns result
  │
  ▼
OutputPolicy.Apply(result, ctx)
  │  marshal to JSON; measure bytes
  │  serialized_bytes > MPM_MCP_MAX_RESULT_BYTES
  ▼
DecisionSpill, N bytes
  │
  ▼
build Metadata from ac + tool + call_id
  │
  ▼
BlobStore.Put(bytes.NewReader(serialized), meta)
  │  write <id>.tmp → fsync → rename(<id>) → INSERT metadata
  │
  ├── success → Pointer{Kind: "blob", ID: <id>}
  │              │
  │              ▼
  │            build spill envelope (bounded; no payload)
  │              │
  │              ▼
  │            return mcp.NewToolResultText(envelope)
  │
  └── failure → mcp.NewToolResultError("internal: spill failed; result suppressed")
```

The exact bytes measured by `OutputPolicy.Apply` are the exact bytes supplied to `BlobStore.Put`. No double-marshal.

---

## Database schema

Added to `internal/core/schema.go:BaseTables`. SQLite WAL + 5s `busy_timeout` per existing invariants.

```sql
CREATE TABLE blobs (
    id             TEXT PRIMARY KEY,
    source_tool    TEXT NOT NULL,
    source_call_id TEXT,
    session_id     TEXT,
    size_bytes     INTEGER NOT NULL,
    content_type   TEXT NOT NULL DEFAULT 'application/json',
    created_at     INTEGER NOT NULL,  -- unix seconds
    expires_at     INTEGER NOT NULL,  -- persisted at Put time
    checksum       TEXT               -- sha256, nullable Phase 1
);
CREATE INDEX idx_blobs_expires_at ON blobs(expires_at);
CREATE INDEX idx_blobs_session_id ON blobs(session_id);
```

`expires_at` is computed at Put time from `time.Now() + ttl_seconds` and **persisted**. Changing `MPM_BLOB_TTL` does not extend existing blobs.

`source_call_id` reuses the existing invocation correlation identifier emitted by `Intercept()` in `internal/core/tools/registry_intercept.go:63`. No separate blob-call-id namespace.

---

## Failure semantics

### Concurrency model

- One `*sql.DB` per process (CLAUDE.md invariant).
- `mpm-mcp` is stdio-per-host → one process per host. Multiple hosts run concurrent `mpm-mcp` processes by design.
- SQLite WAL + 5s `busy_timeout` serializes writers across processes.
- Blob writes: `rename(2)` atomic within filesystem namespace; `INSERT` atomic under WAL. Compound operation is **not** linearizable; crash recovery handles the gap.

### GC cleanup races

- **`Put` mid-flight + GC scan:** grace period (1h) protects freshly-created files via `mtime` check. No locking.
- **Reader + GC:** POSIX open/unlink — reader's open fd remains readable after GC unlinks. New reads via stale pointer observe `ErrBlobMissing`.
- **Two GC processes concurrently:** idempotent — first writer wins, second no-ops on `os.Remove`.
- **Read while DB row disappears:** reader already holds the fd; read completes. Subsequent reads via stale pointer observe `ErrBlobMissing`.

### MCP error envelope contract

Phase 1 explicitly does **not** spill error messages. All errors bounded ≤500 chars. Long diagnostic context goes to slog (server logs), never back to the model. Spill system is **not** recursive.

| Layer | Failure | MCP response |
|---|---|---|
| Tool handler | returns `(nil, err)` | `mcp.NewToolResultError` (existing behavior) |
| OutputPolicy.Apply | marshal fails | `mcp.NewToolResultError("internal: result not marshalable")` |
| OutputPolicy.Apply | ctx canceled | `mcp.NewToolResultError("internal: policy check canceled")` |
| BlobStore.Put | any failure | `mcp.NewToolResultError("internal: spill failed; result suppressed")` |
| Pointer.Parse | malformed | `mcp.NewToolResultError("invalid pointer URI")` |
| Pointer.Resolve | unsupported kind | `mcp.NewToolResultError("unsupported pointer kind for Phase 1")` |
| Pointer.Resolve | blob missing | `mcp.NewToolResultError("blob deleted; pointer stale")` |

### UTF-8 boundary protection (text content only)

Blob store stays byte-oriented internally. MCP presentation is text-oriented for `text/*` and `application/json`. Other content types → bounded MCP error: "binary materialization not supported in Phase 1; use a tool that returns text."

For text content types, when reading bytes in `[offset, offset+max_bytes]` range, the implementation scans forward up to 4 bytes from the cut point to the nearest valid UTF-8 rune boundary. The returned slice never splits a multi-byte rune. No dynamic `content_type` switching per segment.

### `mpm_blob_read` boundedness

```
effective_offset = max(0, min(requested_offset, actual_size))
effective_bytes = min(requested_max_bytes, server_max_bytes)   # server_max = 256 KB
```

EOF returns empty content with `has_more: false` (success, not error). Binary content types return bounded MCP error.

### `mpm_blob_search` boundedness

```
effective_matches = min(requested_max_matches, server_max_matches)   # server_max = 100
effective_bytes = min(requested_max_bytes, server_max_bytes)           # server_max = 256 KB
```

Response: `{matches: [{line_no, byte_offset, snippet}], match_count, truncated, bytes_returned}`. When `truncated: true`, caller refines `query` or `max_*`. No `next_cursor` in Phase 1.

### CLI regression boundary

Not literal byte-for-byte goldens. Two-tier strategy:

1. **Deterministic tools** (`mpm status` with frozen fixtures, etc.): golden byte comparison.
2. **Nondeterministic tools** (timestamps, generated IDs, SQLite row order, env-dependent paths): normalize volatile fields before comparison.

Rule: **Every existing CLI tool must preserve its observable result semantics.** Determinism-class tools use exact byte comparison; non-deterministic tools use semantic normalization. New Phase 1 tools get their own golden suite (separate from the regression suite).

---

## Configuration

| Variable | Default | Description |
|---|---|---|
| `MPM_MCP_MAX_RESULT_BYTES` | `10240` (10 KB) | Serialized result bytes above which MCP adapter spills |
| `MPM_BLOB_TTL` | `24h` | Blob TTL persisted as `expires_at` at Put time |
| `MPM_BLOB_ORPHAN_GRACE` | `1h` | Mtime grace before filesystem orphan sweep deletes |

Server-side (compiled-in, not env-configurable in Phase 1):

| Knob | Default | Ceiling |
|---|---|---|
| `mpm_blob_read` `max_bytes` | 50 KB | 256 KB |
| `mpm_blob_search` `max_matches` | 20 | 100 |
| `mpm_blob_search` `max_bytes` | 50 KB | 256 KB |
| Regex length | — | 512 chars |
| Query length | — | 256 chars |
| Search scan size | — | 100 MB |

No `mpm_config.json` schema change. All runtime knobs are environment variables, matching the existing convention (`MPM_REQUIRED_DB_PATH`, etc.).

---

## Observability

Telemetry events are **structured diagnostic events**, not metrics. Required events have required fields; incidental fields can evolve without breaking tests.

Required events (Phase 1):

| Event | Required fields |
|---|---|
| `mcp_output_policy` | `decision`, `serialized_bytes`, `threshold_bytes`, `tool`, `call_id`, `session_id` |
| `mcp_spill` | `blob_id`, `size_bytes`, `content_type`, `expires_at_unix`, `tool`, `call_id` |
| `mcp_spill_failed` | `error`, `serialized_bytes`, `tool`, `call_id` |
| `blob_gc_run` | `scanned`, `expired_deleted`, `orphans_deleted`, `orphans_skipped_grace`, `freed_bytes`, `duration_ms`, `success` |
| `blob_gc_failed` | (emitted when GC itself errors; `blob_gc_run` with `success=true` is still emitted for successful runs that find zero eligible blobs) |

Tests assert these events fire with required fields, but do not pin every slog field across the codebase.

---

## Testing strategy

### Test pyramid

```
              Whole-system invariants
                       ▲
                       │
              MCP integration
                       ▲
                       │
              BlobStore integration
                       ▲
                       │
                Unit contracts
                       ▲
                       │
                OutputPolicy
```

Plus orthogonal observability coverage at each layer.

### Load-bearing test cases

| Test | Proves |
|---|---|
| `TestOutputPolicy_ThresholdBoundary` | `N → Pass`, `N+1 → Spill` (exact threshold; `>` not `>=`) |
| `TestOutputPolicy_OnlyMCPEnforces` | CLI never invokes OutputPolicy; only `mcpAdapter` does |
| `TestOutputPolicy_NoBlobStoreReference` | Static architecture guard: `output_policy` package has no blobstore import |
| `TestMCPAdapter_BytesMeasuredEqualBytesSpilled` | OutputPolicy measured bytes == BlobStore.Put received bytes (no double-marshal) |
| `TestMCPAdapter_NoSuccessfulResultIsTruncated` | Three-state machine: Pass/Spill-success/Spill-failure only; no fourth branch |
| `TestMCPAdapter_SpillFailureIsBoundedError` | Spill-fail → `IsError=true`, no success/spill envelope, no partial original payload, error size ≤ MCP error limit |
| `TestSpillEnvelope_Bounded` | 100 MB input → envelope < configured MCP result ceiling; original payload absent |
| `TestBlobSpill_IsNotAuthoritativeMemoryState` | Behavioral: blob expiry/deletion does not affect persistent memory artifacts |
| `TestBlobPut_AtomicOrdering` | `write tmp → fsync → rename → INSERT` ordering |
| `TestBlobPut_StaleTmpCleanedByGC` | `.tmp` left after crash is removed by GC orphan sweep |
| `TestBlobGet_EOF` | `offset == size` → success, empty content, `has_more: false` |
| `TestBlobGet_StalePointer` | DB-row-exists-file-missing → `ErrBlobMissing` (recoverable MCP error) |
| `TestBlobGet_POSIXUnlinkAfterOpen` | Reader's open fd readable to EOF after GC unlinks |
| `TestPointer_UnsupportedKind` | `mpm://memory/42` parses but `Resolve` returns `ErrUnsupportedKind` |
| `TestPointer_MalformedInputs` | All malformed forms from pointer parsing errors table |
| `TestBlobRead_ByteOffsetAccuracy` | Offset interpretation is byte-based; UTF-8 boundary protection for text |
| `TestBlobRead_BinaryRejection` | Binary content types → bounded MCP error |
| `TestBlobRead_ServerCeiling` | `max_bytes` clamped to server ceiling |
| `TestBlobSearch_BothBoundsEnforced` | Both `max_matches` AND `max_bytes` clamps active |
| `TestBlobSearch_RegexLimits` | Regex length / query length caps enforced |
| `TestBlobGC_ExpiredPass` | Expired blobs deleted; counts/bytes reported |
| `TestBlobGC_OrphanSweepRespectsGrace` | `mtime < grace` files preserved; aged past grace, deleted |
| `TestBlobGC_DryRun` | Reports without deleting |
| `TestBlobGC_CrashRecovery_BothDirections` | File-exists-no-metadata AND metadata-exists-no-file both recovered |
| `TestBlobGC_FailureObservability` | `blob_gc_failed` emitted on GC errors; `blob_gc_run` distinguishes zero-eligible from failed |
| `TestMultiMCP_NoSilentInconsistency` | Two `mpm-mcp` processes sharing workspace; concurrent Put+Read+GC+write cycles observe consistent state; no `SQLITE_BUSY` escapes to MCP client |
| `TestCLI_UnchangedAfterPhase1` | Per-tool regression: byte-equal for deterministic, normalized for nondeterministic |
| `TestBackupRestore_BlobsNotIncluded` | `mpm backup-db` does not back up blob payloads; restored DB + missing blob → stale pointer → `ErrBlobMissing` |
| `TestTelemetry_RequiredEvents` | Each required event fires with required fields |

### Fuzz testing

```
FuzzPointerParse:
  properties:
    - parser never panics
    - malformed input never produces an apparently valid pointer
    - wrong scheme rejected
    - IDs outside [a-z0-9-]+ rejected
    - Parse(p.URI()) == p (round-trip)
```

FuzzPointerParse must run successfully under the repository's configured fuzz budget with zero panics or invariant violations. CI maintains a committed corpus of discovered edge cases.

### Test infrastructure

- Temp workspace per test (`t.TempDir()` + isolated `MPM_WORKSPACE`).
- FTS5 enabled in CI per CLAUDE.md.
- Crash simulation: `exec.Command` + `Process.Kill()` at the right syscall boundary.
- `-race` detector mandatory for any test spawning concurrent goroutines.
- Multi-process test: two `mpm-mcp` children via `exec.Command`, each with its own `MPM_WORKSPACE` pointing at the same parent DB via shared path (WAL allows this).

---

## Acceptance criteria (ship gates)

Phase 1 ships when **every** gate below passes. Each is a runnable assertion.

**A. Invariant gates**

- All 9 architectural invariants asserted as runnable tests, all pass.
- `TestOutputPolicy_NoBlobStoreReference` (architecture guard) and behavioral equivalents both pass.
- `TestBlobSpill_IsNotAuthoritativeMemoryState` proves behaviorally.

**B. Component gates**

- All Section "load-bearing test cases" pass under `go test -race`.
- FuzzPointerParse passes the configured fuzz budget with zero panics.
- Multi-process consistency test passes.
- Both crash-recovery directions pass.
- Grace-period test passes (both `.tmp` and final blob).
- Spill envelope boundedness test passes (100 MB input → envelope < ceiling).
- Spill decision determinism test passes (`N → Pass`, `N+1 → Spill` without BlobStore).

**C. Operational gates**

- Smoke test: launch `mpm-mcp` against existing workspace; exercise three tools (`mpm_recall`, `mpm_status`, `mpm_help`); confirm bounded responses where expected, unbounded where the result fits.
- Migration test: pre-Phase-1 DB upgrades cleanly to Phase-1 schema (new `blobs` table created; existing data untouched).
- Backup/restore roundtrip: `mpm backup-db` → wipe → restore → blob write → blob read → consistent; stale pointer after restore → `ErrBlobMissing`.
- Documentation: `README.md` updated with new tools; `mpm_help` reflects new commands.

---

## Compatibility

### Database

Additive migration. `blobs` table added to `internal/core/schema.go:BaseTables`. Pre-existing rows unaffected. SafeMigrations path runs on first Phase-1 boot. Operators `mpm backup-db` before upgrade per existing CLAUDE.md convention.

### CLI binary (`mpm`)

Zero behavior change. Every existing command produces the same output. `mpm call` envelope unchanged. New CLI subcommand: `mpm blob gc`. New MCP tools accessible via `mpm call <tool>` only (no separate top-level verbs).

### MCP server (`mpm-mcp`)

Three new tools added (`mpm_resolve`, `mpm_blob_read`, `mpm_blob_search`). Existing tools' responses are now bounded where the result exceeds `MPM_MCP_MAX_RESULT_BYTES`. **Existing MCP tool schemas remain unchanged, but existing tools may now return a spill envelope instead of their complete serialized result when the result exceeds the configured output limit. Consumers must therefore handle the `status: "spilled"` result form.**

### `tools.Registry`

Three new entries; `Intercept()` machinery untouched. No existing handler signatures change.

### Versioning

Minor version bump. Phase 1 is additive — no breaking changes to CLI surface, no breaking changes to existing MCP tool signatures. The bounded-output semantics for existing tools is a **behavioral change** (downgrade of "unbounded" to "bounded with pointer on overflow"), not a signature change.

---

## Rollout / rollback

### Rollout

1. Operators run `mpm backup-db` (existing convention).
2. Replace `bin/mpm` and `bin/mpm-mcp` with Phase-1 binaries.
3. Restart `mpm-mcp` per host. SQLite migration runs on first open; idempotent.
4. New `$MPM_WORKSPACE/blobs/` directory created on first spill.
5. Telemetry events visible via `slog` default output.

No coordination required across hosts. Each `mpm-mcp` is independent.

### Rollback

**Binary-only rollback:** restore pre-Phase-1 binary against Phase-1 DB. Safe — `blobs` table is additive and ignored by older code (pre-Phase-1 handlers never reference the table; SQLite ignores unknown tables on read). **Binary rollback does not require database rollback.** No DB restoration required. The `blobs` table persists in the SQLite file but is functionally invisible to pre-Phase-1 code.

**Database rollback:** restore pre-Phase-1 DB snapshot. Required only when operator intentionally wants to revert schema/state. Phase-1 blob metadata disappears; orphan payloads may remain in `$MPM_WORKSPACE/blobs/` (clean up via filesystem rm, not via any tool).

### Forward compatibility

Phase-1 binaries can read pre-Phase-1 databases. The additive migration creates the `blobs` table during first Phase-1 startup; existing data is untouched.

---

## Non-goals (Phase 1 explicit)

The following are out of scope. Listed so future contributors don't accidentally implement them, and reviewers can reject out-of-scope PRs cleanly.

- **Phase 2 pointer kinds** (`mpm://memory/<id>`, `mpm://lesson/<id>`, `mpm://theory/<id>`, projections, etc.). Parser supports the grammar; resolver rejects all but `mpm://blob/<id>`.
- **Windows unlink-after-open semantics.** POSIX/macOS/Linux only.
- **Cross-filesystem blob storage** (NFS, SMB). Single local filesystem where `rename(2)` is atomic.
- **Blob encryption at rest.** Stored as raw bytes.
- **Blob compression.** Stored uncompressed in Phase 1. Compression may be considered later if storage or transfer characteristics justify it.
- **Per-tool output budgets.** Default applies uniformly. Per-tool tuning is Phase 2/3 territory.
- **Configurable spill envelopes.** Envelope schema is fixed in this spec.
- **Blob lifecycle via background goroutine.** Explicit `mpm blob gc` only.
- **Spilling error responses.** Errors stay bounded; full diagnostics in slog only.
- **Bounded `OutputPolicy` for CLI.** CLI returns full results; MCP is the only enforcement point.
- **Distributed content-addressable pointer graph.** Single-process SQLite is the source of truth.
- **Phase 1 does not require agents or MCP clients to understand pointer URIs generically.** The server returns a structured envelope and provides `mpm_resolve` / `mpm_blob_read` / `mpm_blob_search`. Phase 2 can make pointer-aware retrieval a richer protocol.

---

## What Phase 1 is not

Phase 1 does **not** turn `mpm recall`, `mpm status`, or any other tool into a pointer-by-default consumer. Those tools still return full results when called via MCP; they get bounded only when the result exceeds `MPM_MCP_MAX_RESULT_BYTES`. The pointer architecture applies to **oversized results**, not to **all results**.

Phase 1 is a **transport / context protection layer**, not a rewrite of mpm's cognitive semantics. The layering remains:

```
Tool result ──► MCP representation ──┬── small ──► inline
                                     └── large ──► blob pointer
```

Phase 2 will introduce `memory / lesson / theory / projection → pointer-native retrieval`. Phase 1 deliberately does not.