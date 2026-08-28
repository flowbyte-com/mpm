# Pointer Architecture Phase 1 — Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Implement Phase 1 of the pointer architecture — a generic pointer substrate, consumer-neutral blob store, and MCP output policy that spills oversized results to filesystem blobs.

**Architecture:** Three new packages (`internal/pointer/`, `internal/blobstore/`, `internal/core/tools/output_policy/`), one new database table (`blobs`), three new MCP tools, one new CLI command (`mpm blob gc`). MCP is the only enforcement point; CLI semantics are unchanged.

**Tech Stack:** Go 1.21+, `mattn/go-sqlite3` (CGO with FTS5), standard `io`/`os`/`io/fs`, `golang.org/x/net/re2` for regex

**Spec:** `docs/superpowers/specs/2026-08-21-pointer-architecture-design.md`

---

## Global Constraints

| Constraint | Value |
|---|---|
| Serialization boundary | `>` (strict, not `>=`) — exactly-at-limit results pass |
| Blob filesystem layout | `$MPM_WORKSPACE/blobs/<id>` (no extension) |
| Atomic Put ordering | `write .tmp → fsync → rename → INSERT metadata` |
| Default result byte limit | `MPM_MCP_MAX_RESULT_BYTES = 10240` |
| Default blob TTL | `MPM_BLOB_TTL = 24h` |
| Orphan grace period | `MPM_BLOB_ORPHAN_GRACE = 1h` |
| Server ceiling `max_bytes` | 256 KB |
| Server ceiling `max_matches` | 100 |
| BlobStore interface location | `internal/blobstore/store.go` |
| Pointer interface location | `internal/pointer/pointer.go` |

---

## File Structure

```
internal/
  pointer/
    pointer.go          # Pointer struct, Parse(), ErrUnsupportedKind, Resolver interface
    pointer_test.go     # Parse tests + fuzz
    resolve.go          # Resolve() implementation (Phase 1: blob only)
    resolve_test.go

  blobstore/
    store.go            # BlobStore interface + Metadata + GetOptions + SearchQuery + Match
    fs.go               # FilesystemBackend: Put/Get/Delete/GC implementation
    fs_test.go          # Put atomicity, Get EOF/staleness, POSIX unlink-after-open
    doc.go              # Package docs

  core/
    tools/
      output_policy.go          # OutputPolicy interface + DefaultOutputPolicy + Decision + DecisionPass/Spill
      output_policy_test.go     # Threshold boundary, marshal equivalence

cmd/
  mpm-mcp/
    main.go             # Modify: build BlobStore + OutputPolicy, thread to mcpAdapter
    tools.go            # Create OR modify: spill orchestration in mcpAdapter closure

  mpm/
    handlers_blob_gc.go  # New: `mpm blob gc` command
    handlers_blob_gc_test.go
```

**Schema changes** — `internal/core/schema.go:BaseTables`:
- Add `CREATE TABLE blobs (...)` DDL string
- Add index DDL strings for `idx_blobs_expires_at`, `idx_blobs_session_id`

**Registry changes** — `internal/core/tools/registry_list.go`:
- Add three `Tool{}` entries: `mpm_resolve`, `mpm_blob_read`, `mpm_blob_search`

**Handler changes** — `internal/core/tools/handlers.go`:
- Add `handleMpmResolve`, `handleMpmBlobRead`, `handleMpmBlobSearch`

---

## Task 1: `internal/pointer/` — URI grammar and resolver

**Files:**
- Create: `internal/pointer/pointer.go`
- Create: `internal/pointer/pointer_test.go`
- Create: `internal/pointer/resolve.go`
- Create: `internal/pointer/resolve_test.go`

**Interfaces:**
- Consumes: nothing (standalone package)
- Produces: `Pointer` struct, `Parse(uri string) (Pointer, error)`, `ErrUnsupportedKind`, `ErrPointerMalformed`, `ErrPointerWrongScheme`, `Resolver` interface, `Resolve(ctx, Pointer, ResolveOptions) (Resolution, error)`

### `pointer.go`

```go
package pointer

import "errors"

var (
    ErrPointerMalformed   = errors.New("pointer: malformed URI")
    ErrPointerWrongScheme = errors.New("pointer: wrong scheme")
    ErrUnsupportedKind   = errors.New("pointer: unsupported kind for Phase 1")
)

type Pointer struct {
    Kind string
    ID   string
}

func Parse(uri string) (Pointer, error) {
    // Accepts only mpm://<kind>/<id> where kind is non-empty,
    // id matches [a-z0-9-]+, uri must not contain fragment or query.
    // Returns ErrPointerMalformed for mpm://, mpm://blob/, mpm://blob/abc!def
    // Returns ErrPointerWrongScheme for non-mpm:// URIs
}

func (p Pointer) URI() string

type ResolveOptions struct {
    MaxBytes int64 // 0 = no limit
}

type Resolution struct {
    ContentType string
    Reader      io.ReadCloser
    Metadata    map[string]any
}

type Resolver interface {
    Resolve(ctx context.Context, p Pointer, opts ResolveOptions) (Resolution, error)
}
```

### `resolve.go`

Phase 1 resolver supports only `mpm://blob/<id>`. Other kinds return `ErrUnsupportedKind`. The actual blob resolution is delegated to `blobstore.BlobStore` (imported). This avoids a cycle since blobstore will import pointer for type references.

---

## Task 2: `internal/blobstore/` — filesystem blob store + SQLite metadata

**Files:**
- Create: `internal/blobstore/store.go` (interface + types)
- Create: `internal/blobstore/fs.go` (FilesystemBackend implementation)
- Create: `internal/blobstore/fs_test.go`

**Interfaces:**
- Consumes: `internal/pointer.Pointer` (for `Put` return type)
- Produces: `BlobStore` interface, `Metadata`, `GetOptions`, `SearchQuery`, `Match`, `GCStats`, `ErrBlobMissing`, `ErrBlobNotFound`

### `store.go`

```go
package blobstore

import (
    "context"
    "io"
    "time"
)

type Pointer struct { // duplicated from pointer to avoid import cycle; identical
    Kind string
    ID   string
}

type Metadata struct {
    SourceTool    string
    SourceCallID  string
    SessionID     string
    SizeBytes     int64
    ContentType   string
    CreatedAt     time.Time
    ExpiresAt     time.Time
    Checksum      string // sha256, nullable Phase 1
}

type GetOptions struct {
    Offset    int64
    MaxBytes  int64 // 0 = read to EOF
}

type SearchQuery struct {
    Query           string
    Regex           bool
    CaseInsensitive bool
    MaxMatches      int
    MaxBytes        int64
}

type Match struct {
    LineNo     int
    ByteOffset int64
    Snippet    string
}

type GCStats struct {
    Scanned           int
    ExpiredDeleted    int
    OrphansDeleted    int
    OrphansSkipped    int
    FreedBytes        int64
    DurationMs        int64
    Success           bool
}

var (
    ErrBlobMissing   = errors.New("blobstore: file missing (DB row exists)")
    ErrBlobNotFound  = errors.New("blobstore: no DB row for blob")
)

type BlobStore interface {
    Put(ctx context.Context, content io.Reader, meta Metadata) (Pointer, error)
    Get(ctx context.Context, id string, opts GetOptions) (io.ReadCloser, Metadata, error)
    Delete(ctx context.Context, id string) error
    Search(ctx context.Context, id string, query SearchQuery) ([]Match, error)
    GCExpired(ctx context.Context, now time.Time) (GCStats, error)
    GCSweepOrphans(ctx context.Context, grace time.Duration) (GCStats, error)
}
```

### `fs.go`

`FilesystemBackend` struct holds `db *sql.DB`, `blobDir string`, `ttl time.Duration`.

**`Put` ordering:** `write <id>.tmp → fsync → rename(<id>) → INSERT metadata → return Pointer`. UUID generated via `github.com/google/uuid`. Stale `.tmp` left on error — GC's job.

**`Get` semantics:**
- `DB row + file present` → stream bytes (offset + max_bytes range)
- `DB row + file missing` → `ErrBlobMissing`
- `DB row + file truncated` → return bytes up to actual size; `next_offset = size`
- `offset >= size` → return `{content: "", bytes_returned: 0, next_offset: offset, has_more: false}` (success, EOF)
- DB row absent → `ErrBlobNotFound`

**UTF-8 boundary protection for text content types:** When reading bytes in `[offset, offset+max_bytes]` range, scan forward up to 4 bytes from cut point to nearest valid UTF-8 rune boundary. Return slice never splits a multi-byte rune.

**`Delete` semantics:** Best-effort idempotent. Order: `DELETE FROM blobs WHERE id=?` → `os.Remove(payload_path)`. Caller treats both as success if DB row is gone.

**`GCExpired`:** identify expired IDs → delete payloads → delete DB rows. Idempotent; crash between steps leaves recoverable orphan/missing-row.

**`GCSweepOrphans`:**
- Walk `blobs/` for files. For each: if file age (mtime) > grace AND no DB row → delete.
- For each DB row with missing file → log warning, delete orphan DB row.

**Filesystem layout:** `$MPM_WORKSPACE/blobs/<id>` (no extension — content_type in metadata).

---

## Task 3: `internal/core/tools/output_policy.go` — decision only

**Files:**
- Create: `internal/core/tools/output_policy.go`
- Create: `internal/core/tools/output_policy_test.go`

**Architecture guard:** `output_policy.go` must NOT import `internal/blobstore`. This is enforced by `TestOutputPolicy_NoBlobStoreReference`.

```go
package tools

import (
    "context"
    "encoding/json"
    "os"
)

type Decision int

const (
    DecisionPass  Decision = iota // serialized_bytes <= MPM_MCP_MAX_RESULT_BYTES
    DecisionSpill                   // serialized_bytes > MPM_MCP_MAX_RESULT_BYTES
)

type OutputPolicy interface {
    Apply(ctx context.Context, result any) (Decision, int, error)
}

type defaultOutputPolicy struct {
    threshold int
}

func DefaultOutputPolicy() OutputPolicy {
    threshold := 10240 // MPM_MCP_MAX_RESULT_BYTES default
    if e := os.Getenv("MPM_MCP_MAX_RESULT_BYTES"); e != "" {
        // parse int
    }
    return &defaultOutputPolicy{threshold}
}

func (p *defaultOutputPolicy) Apply(ctx context.Context, result any) (Decision, int, error) {
    // Marshal to JSON; measure len of encoded bytes
    // Strict > threshold → DecisionSpill
    // Marshal failure → error (NOT DecisionSpill)
    // ctx canceled → error
    // Returns (decision, serializedByteCount, nil/error)
}
```

---

## Task 4: `cmd/mpm-mcp/main.go` — wire BlobStore + OutputPolicy

**Files:**
- Modify: `cmd/mpm-mcp/main.go`
- Modify: `cmd/mpm-mcp/tools.go` (or create if it doesn't exist)

Pass `MPM_WORKSPACE` env to construct the `blobDir` path. Build `*blobstore.FilesystemBackend` and `tools.DefaultOutputPolicy()` at startup. Thread both to `mcpAdapter`.

**No import cycle:** `blobstore` does not import `pointer` (it defines its own `Pointer` struct locally). `pointer` imports `blobstore` in `resolve.go`. All other imports follow existing patterns.

---

## Task 5: MCP spill orchestration in `mcpAdapter`

**Files:**
- Modify: `cmd/mpm-mcp/tools.go`

```go
decision, bytes, err := outputPolicy.Apply(ctx, result)
switch decision {
case tools.DecisionPass:
    return mcp.NewToolResultText(json.Marshal(result))
case tools.DecisionSpill:
    ptr, putErr := blobStore.Put(ctx, bytes.NewReader(serialized), metadata)
    if putErr != nil {
        return mcp.NewToolResultError("internal: spill failed; result suppressed"), nil
    }
    return mcp.NewToolResultText(spillEnvelope(ptr, bytes, metadata)), nil
}
```

**Key invariants to preserve:**
- Exact bytes from `Apply` are passed to `BlobStore.Put` (no double-marshal)
- Spill failure returns bounded error — never partial payload
- `spillEnvelope` builds the JSON envelope per spec (status: "spilled", pointer, size_bytes, content_type, source_tool, preview, expires_at)
- Three-state machine only: Pass / Spill-success / Spill-failure. No fourth branch.

---

## Task 6: Database schema — add `blobs` table

**Files:**
- Modify: `internal/core/schema.go`

Add to `BaseTables`:

```sql
CREATE TABLE blobs (
    id             TEXT PRIMARY KEY,
    source_tool    TEXT NOT NULL,
    source_call_id TEXT,
    session_id     TEXT,
    size_bytes     INTEGER NOT NULL,
    content_type   TEXT NOT NULL DEFAULT 'application/json',
    created_at     INTEGER NOT NULL,
    expires_at     INTEGER NOT NULL,
    checksum       TEXT
);
```

Add to `CommonIndexes`:

```sql
CREATE INDEX idx_blobs_expires_at ON blobs(expires_at);
CREATE INDEX idx_blobs_session_id ON blobs(session_id);
```

**Migration safety:** This is an additive migration. Existing data is untouched.

---

## Task 7: Three new MCP tools — registry entries + handlers

**Files:**
- Modify: `internal/core/tools/registry_list.go` — append three `Tool{}` entries
- Modify: `internal/core/tools/handlers.go` — add three handlers

### Tool schemas (JSON)

**`mpm_resolve`:**
```json
{
  "name": "mpm_resolve",
  "description": "Resolve a mpm:// URI to its content. Phase 1 supports mpm://blob/<id> only.",
  "schema": {
    "type": "object",
    "properties": {
      "uri": {"type": "string"},
      "max_bytes": {"type": "integer", "default": 0}
    },
    "required": ["uri"]
  }
}
```

**`mpm_blob_read`:**
```json
{
  "name": "mpm_blob_read",
  "description": "Read a blob with byte offset and server-side max_bytes cap.",
  "schema": {
    "type": "object",
    "properties": {
      "id": {"type": "string"},
      "offset": {"type": "integer", "default": 0},
      "max_bytes": {"type": "integer", "default": 51200}
    },
    "required": ["id"]
  }
}
```

**`mpm_blob_search`:**
```json
{
  "name": "mpm_blob_search",
  "description": "Server-side regex search within a blob.",
  "schema": {
    "type": "object",
    "properties": {
      "id": {"type": "string"},
      "query": {"type": "string", "maxLength": 256},
      "regex": {"type": "boolean", "default": false},
      "case_insensitive": {"type": "boolean", "default": false},
      "max_matches": {"type": "integer", "default": 20},
      "max_bytes": {"type": "integer", "default": 51200}
    },
    "required": ["id", "query"]
  }
}
```

### Handler implementations

**`handleMpmResolve`:** Call `pointer.Resolve()` with `ResolveOptions.MaxBytes`. Return bounded JSON or error envelope.

**`handleMpmBlobRead`:** Apply server ceiling (256 KB). Reject binary content types with bounded MCP error. UTF-8 boundary protection on text. Return `{content, content_type, offset, bytes_returned, next_offset, has_more}`.

**`handleMpmBlobSearch`:** Apply server ceiling (256 KB, 100 matches). RE2 regex, no catastrophic backtracking. Return `{matches: [{line_no, byte_offset, snippet}], match_count, truncated, bytes_returned}`.

---

## Task 8: `mpm blob gc` CLI command

**Files:**
- Create: `cmd/mpm/handlers_blob_gc.go`
- Create: `cmd/mpm/handlers_blob_gc_test.go`

### Two-pass execution

1. **`mpm blob gc --dry-run`:** Report counts without deleting
2. **`mpm blob gc`:** Execute both passes

**Pass 1 — Expired registered blobs:** `DELETE FROM blobs WHERE expires_at < ?` → remove filesystem payloads.

**Pass 2 — Orphan sweep:** Walk `$MPM_WORKSPACE/blobs/`. Files with `mtime > MPM_BLOB_ORPHAN_GRACE` and no DB row → delete. DB rows with missing files → log warning + delete orphan row.

Both passes are idempotent. Crash between passes: next run picks up where it left off.

### Command registration

Register in `cmd/mpm/router.go` under a new `blob` subcommand group (`mpm blob gc`). Follow existing handler registration pattern.

---

## Task 9: Schema version bump + telemetry scaffolding

**Files:**
- Modify: `internal/core/version.go` (or wherever version is defined — check `db.go` for `synthVersion` or similar pattern)

Phase 1 is a minor version bump (additive). Add a `synthVersion` or equivalent for the blob subsystem.

Add `blob_gc_run` and `blob_spill` telemetry events following the existing `slog` pattern used in `internal/core/logging/`. Events defined in the spec's observability table.

---

## Task 10: Regression + load-bearing tests

**Files:**
- Create: per-component `_test.go` files as noted above
- Add to existing test suites where applicable

### Critical tests (must pass)

| Test | File | What it proves |
|---|---|---|
| `TestOutputPolicy_ThresholdBoundary` | `output_policy_test.go` | `N → Pass`, `N+1 → Spill` (strict `>` not `>=`) |
| `TestOutputPolicy_OnlyMCPEnforces` | `output_policy_test.go` | CLI path never calls Apply |
| `TestOutputPolicy_NoBlobStoreReference` | `output_policy_test.go` | Static: output_policy package imports blobstore → compile error |
| `TestMCPAdapter_BytesMeasuredEqualBytesSpilled` | `tools_test.go` | Apply bytes == Put bytes (no double-marshal) |
| `TestMCPAdapter_NoSuccessfulResultIsTruncated` | `tools_test.go` | Three-state machine only |
| `TestMCPAdapter_SpillFailureIsBoundedError` | `tools_test.go` | Spill-fail → bounded MCP error |
| `TestBlobPut_AtomicOrdering` | `fs_test.go` | `write tmp → fsync → rename → INSERT` ordering |
| `TestBlobGet_EOF` | `fs_test.go` | `offset == size` → empty content + `has_more: false` |
| `TestBlobGet_StalePointer` | `fs_test.go` | DB-row-exists-file-missing → `ErrBlobMissing` |
| `TestBlobGet_POSIXUnlinkAfterOpen` | `fs_test.go` | Reader fd readable after GC unlinks |
| `TestPointer_UnsupportedKind` | `pointer_test.go` | `mpm://memory/42` parses; `Resolve` → `ErrUnsupportedKind` |
| `TestPointer_MalformedInputs` | `pointer_test.go` | All malformed forms rejected |
| `TestBlobGC_ExpiredPass` | `handlers_blob_gc_test.go` | Expired deleted; counts reported |
| `TestBlobGC_OrphanSweepRespectsGrace` | `handlers_blob_gc_test.go` | Fresh files preserved; aged deleted |
| `TestBlobGC_DryRun` | `handlers_blob_gc_test.go` | Reports without deleting |
| `TestCLI_UnchangedAfterPhase1` | regression suite | Existing commands unchanged |

### Fuzz test

**`FuzzPointerParse`** in `pointer_test.go`:
- Parser never panics
- Malformed input never produces apparently valid pointer
- Wrong scheme rejected
- IDs outside `[a-z0-9-]+` rejected
- `Parse(p.URI()) == p` (round-trip)

Run under `go test -fuzz=FuzzPointerParse -fuzztime=60s` in CI.

---

## Task 11: Documentation update

**Files:**
- Modify: `mpm/CLAUDE.md` — add `internal/blobstore/`, `internal/pointer/` to directory structure
- Modify: `docs/ARCHITECTURE_SPLIT.md` or `docs/architecture.md` — add pointer architecture section (brief)
- Modify: `README.md` — document new CLI command (`mpm blob gc`) and new MCP tools

---

## Task Dependencies

```
Task 1: pointer/          ──┐
                             ├── Task 2: blobstore/ ── Task 4: mcp wiring
Task 3: output_policy/  ───┤                        │
                             │                        ▼
                             │              Task 5: mcpAdapter spill
                             │
Task 6: schema (blobs table) ─┤
                             │
                             ├──── Task 7: 3 tools ──┤
                             │                       │
                             │                       ▼
Task 8: blob gc CLI     ─────┴─── Task 9: telemetry ── Task 10: tests
                                                              │
                                                              ▼
                                                         Task 11: docs
```

**Dependency notes:**
- Task 2 (blobstore) imports Task 1 (pointer) for `Pointer.URI()` usage in `resolve.go`
- Task 5 (mcpAdapter) needs Tasks 1+2+3 ready
- Task 7 (tools) needs Task 1 (pointer) for `pointer.Parse`
- Task 10 (tests) needs Tasks 1-9

---

## Spec Coverage Check

| Spec Section | Tasks |
|---|---|
| 9 architectural invariants | Tasks 1-5 (structural enforcement) + Task 10 (test assertions) |
| BlobStore interface + semantics | Task 2 |
| Pointer grammar + resolver | Task 1 |
| OutputPolicy decision only | Task 3 |
| MCP spill orchestration | Task 5 |
| Three new MCP tools | Task 7 |
| `mpm blob gc` command | Task 8 |
| Database schema | Task 6 |
| Observability events | Task 9 |
| Test pyramid | Task 10 |
| Acceptance gates | Task 10 |

---

## Placeholder Scan

- No `TBD` / `TODO` in this plan — all interfaces and semantics are fully specified
- All type names, method signatures, and constants use exact names from the spec
- Step sequences are self-contained per task (no "similar to Task N")
