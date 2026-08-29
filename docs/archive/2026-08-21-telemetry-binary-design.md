# Telemetry Binary — Design Spec

**Date:** 2026-08-21
**Status:** Design — pending implementation
**Scope:** Post-alpha observational infrastructure for execution economics; one new sibling binary (`mpm-telemetry`), one new SQLite file (`telemetry.db`), one framework adapter (`claude-code-mpm`), one economic observation command. Substrate (`mpm` core, `mpm.db`) is unchanged.

---

## Spec-top orientation

> **Telemetry is observational infrastructure. It records execution economics and correlates them with MPM artifacts and outcomes. It does not participate in artifact correctness, retrieval ranking, confidence, or lifecycle decisions.**

The substrate records execution. The telemetry binary records execution economics. The two meet at exactly one key — the `invocation_id` string — and nowhere else. Neither system reaches into the other's database.

This is observational sidecar architecture. The substrate is the authoritative cognitive store; telemetry is a journal kept beside it.

---

## Architectural invariants

These five invariants are load-bearing. Every implementation decision must preserve them.

1. **Telemetry availability.** A running LLM invocation must never depend on telemetry availability for correctness. If `mpm-telemetry` is down, the adapter drops the frame after a short timeout; the agent loop continues. If the collector cannot persist, the response is `DROPPED` and the agent continues. The substrate's existing artifact path is unaffected.
2. **Substrate ignorance of billing.** `mpm` core has zero tables, columns, or code paths that hold token counts, pricing, or cost projections. The `telemetry_invocation` table is owned exclusively by `mpm-telemetry.db`. The substrate's `artifact_provenance` carries `invocation_id` as a string for cross-correlation; it does not carry token data.
3. **Exact correlation key.** Substrate artifacts and telemetry invocations meet through `invocation_id`. No fuzzy timestamp matching, no session-window heuristics. Both sides record the same string; future queries join on the string.
4. **Declared, not inferred, execution.** The collector records what the framework declares. Missing fields stay NULL. The collector does not infer `cache_read_tokens` from other fields. Schema versions are explicit on every frame.
5. **Two processes, two databases, two connection owners.** `mpm` owns `mpm.db` exclusively (per CLAUDE.md H-5 — single-connection-pool). `mpm-telemetry` owns `telemetry.db` exclusively. Neither process opens the other's database. Cross-database correlation happens at query time via the `invocation_id` string, not via cross-DB joins.

---

## The architectural boundary

```
Framework (claude-code-mpm)
   │
   │ NDJSON frame, one per completed invocation
   ▼
Unix socket (canonical MPM runtime dir)
   │
   ▼
mpm-telemetry (process, owns telemetry.db)
   │
   ├── raw invocation ledger (telemetry_invocation)
   │
   ├── pricing projection (read-time, external catalog)
   │
   └── economic observations
          │ mpm call mpm_lessons --payload {"action":"save",...}
          ▼
      mpm (substrate, owns mpm.db)
       │
       └── artifact_provenance (existing)
              │
              └── invocation_id ←── exact correlation key
```

Independent of telemetry:

```
Framework
   │
   ▼
mpm (substrate, unchanged)
   │
   └── artifact_provenance
          │
          ├── invocation_id (correlation key)
          ├── parent_invocation_id
          ├── session_id
          ├── model, framework, provider
          └── ... (existing fields)
```

The two flows never meet inside either binary. They meet only when an operator queries both, or when `mpm-telemetry observe` calls `mpm call mpm_provenance --payload '{"action":"count_by_session","session_id":"<id>"}'` to perform the cross-DB artifact lookup for its observation command.

---

## Motivation

`mpm-agent`, `claude-code`, `opencode`, `pi`, `hermes`, and `openclaw` agents cost real money. The 2026 Stanford finding that agentic runs can consume ~1000× the tokens of chat, with input tokens dominating cost and per-task variance up to 30×, makes the question "is MPM worth its token overhead?" unavoidable.

The substrate cannot answer that question on its own. It knows which memories, decisions, and theories were produced — but not what they cost. Telemetry cannot answer it on its own either. It knows what was spent — but not what was produced or whether the spend was avoided work.

The answer requires **correlating** the two: raw execution economics joined to artifact outcomes. That requires both sides to record the same `invocation_id`.

This plan delivers the minimum telemetry contract that makes that future analysis possible, without building the analytics layer prematurely.

---

## What this plan ships

| Component | Location | Purpose |
|---|---|---|
| `mpm-telemetry` binary | `cmd/mpm-telemetry/main.go` | Unix-socket collector + read-only query surface |
| Telemetry library | `internal/telemetry/` | DB schema, socket protocol, validation, persistence |
| `telemetry.db` | `$MPM_WORKSPACE/telemetry.db` | Owned exclusively by the collector |
| `claude-code-mpm` adapter | `agent_installation/claude-code-mpm/` | Emit one NDJSON frame per completed invocation |
| `observe` command | `mpm-telemetry observe` | Runs the `HighTokenNoArtifactHunt`; emits findings via `mpm call` |
| Smoke script | `scripts/smoke_telemetry.sh` | Hermetic end-to-end test |
| Stub pricing catalog | `internal/telemetry/testdata/one-model.json` | Single-entry test fixture for the `cost` subcommand |
| Tests | `internal/telemetry/*_test.go` | Unit + integration |

## What this plan deliberately does not ship

- Analytics dashboards, cost-over-time plots, or any visualisation layer.
- A pricing catalog beyond a single stub JSON shipped in `internal/telemetry/testdata/`.
- Adapter integrations for `hermes-mpm`, `openclaw-mpm-memory`, `opencode-mpm`, `pi-mpm`, `mpm-auto-route`.
- Durable disk spool for retries across `mpm-telemetry` restarts.
- Retention / pruning policy for `telemetry.db`.
- Request-id / attempt correlation fields (left as room in `provider_metadata`).
- Any change to `mpm` core, `mpm.db`, the critic, the synthesis worker, or any substrate lifecycle path.

---

## Components in detail

### 1. `mpm-telemetry` binary

Subcommands:

```
mpm-telemetry serve                        # default; runs the socket collector
mpm-telemetry ping                         # handshake; returns collector_version, protocol_version, schema_version, queue_depth
mpm-telemetry query invocation <id>        # read one row
mpm-telemetry query session <id>           # read all rows for a session, aggregated
mpm-telemetry query since <cutoff>         # read all rows since a Unix timestamp
mpm-telemetry cost --pricing catalog.json  # read-time pricing projection (no DB write)
mpm-telemetry observe                      # runs HighTokenNoArtifactHunt
```

The collector runs as a single goroutine per accepted connection, parses NDJSON line-delimited frames, validates the schema, and persists to `telemetry.db`. No goroutine pool — write traffic is low and serialization is cheap; the architecture stays simple. Concurrency cap: `runtime.GOMAXPROCS(32)` inherited from `main.go` discipline.

The socket path is derived from the same canonical MPM runtime directory used by the rest of MPM's IPC surfaces. All integrations read the same env var (`$MPM_WORKSPACE`) to find the socket. No per-adapter hard-coding.

### 2. `telemetry.db` schema

One table. Pricing is **not** persisted in this database — it is read-time external data, applied by the `cost` subcommand from a JSON file. Consistent with the Projection Principle.

```sql
CREATE TABLE IF NOT EXISTS telemetry_invocation (
  invocation_id        TEXT PRIMARY KEY,
  parent_invocation_id TEXT,                                  -- nullable; sub-invocations
  session_id           TEXT,                                  -- nullable; some frameworks don't expose
  framework            TEXT NOT NULL,
  framework_version    TEXT,                                  -- nullable if unknown
  provider             TEXT NOT NULL,
  model                TEXT NOT NULL,
  model_revision       TEXT,                                  -- nullable if unknown

  started_at           INTEGER NOT NULL,                      -- Unix epoch seconds
  completed_at         INTEGER NOT NULL,
  received_at          INTEGER NOT NULL,                      -- when the collector accepted

  status               TEXT NOT NULL,                         -- completed | failed | cancelled | timed_out
  stop_reason          TEXT,                                  -- opaque provider/framework string

  -- Raw provider counters. NULL means the framework/provider did not
  -- report this field; 0 means the framework reported zero. The two
  -- are analytically distinct and must remain distinct on disk.
  input_tokens         INTEGER,
  output_tokens        INTEGER,
  cache_read_tokens    INTEGER,
  cache_write_tokens   INTEGER,
  reasoning_tokens     INTEGER,

  duration_ms          INTEGER,                               -- client-measured wall time
  provider_metadata    TEXT NOT NULL DEFAULT '{}',            -- opaque JSON object
  schema_version       TEXT NOT NULL
);

CREATE INDEX IF NOT EXISTS idx_tel_session     ON telemetry_invocation(session_id);
CREATE INDEX IF NOT EXISTS idx_tel_parent_inv  ON telemetry_invocation(parent_invocation_id);
CREATE INDEX IF NOT EXISTS idx_tel_started_at  ON telemetry_invocation(started_at);
CREATE INDEX IF NOT EXISTS idx_tel_framework   ON telemetry_invocation(framework);
```

The `PRIMARY KEY` on `invocation_id` gives free idempotency for identical retry payloads. Conflicting duplicate payloads (same `invocation_id`, different bytes) are rejected — see §5.

### 3. Wire format (NDJSON frame)

One frame per completed LLM invocation. Self-contained. Schema-versioned.

```json
{
  "schema_version": "v1",
  "event_type": "invocation_completed",
  "invocation_id": "inv_01HXY...",
  "parent_invocation_id": "inv_01HWX...",
  "session_id": "sess_01HXY...",
  "framework": "claude-code",
  "framework_version": "1.2.3",
  "provider": "anthropic",
  "model": "claude-fable-5",
  "model_revision": null,
  "started_at": 1756000000,
  "completed_at": 1756000012,
  "status": "completed",
  "stop_reason": "end_turn",
  "input_tokens": 18234,
  "output_tokens": 4123,
  "cache_read_tokens": 8901,
  "cache_write_tokens": 1200,
  "reasoning_tokens": 3200,
  "duration_ms": 12000,
  "provider_metadata": {},
  "request_id": "req_...",
  "attempt": 1
}
```

Field semantics:

- `schema_version` — required. Unknown versions are rejected with a structured error.
- `event_type` — required. `invocation_completed` is the only type in v1.
- `invocation_id` — required. UUID-ish string. The framework should use the same ID it passes to `MPM_PROVENANCE` (if applicable), or generate a fresh one if not. The collector does not require it to match any substrate-side ID.
- `parent_invocation_id` — nullable. Set when an agent spawns a sub-invocation (Hermes → Claude Code → memory).
- `session_id` — nullable. When set, should match the substrate's session ID for cross-correlation.
- `framework` / `framework_version` — required / nullable. Free strings.
- `provider` / `model` / `model_revision` — required / required / nullable.
- `started_at` / `completed_at` — required. Unix epoch seconds. Must satisfy `completed_at >= started_at`.
- `status` — required. One of `completed | failed | cancelled | timed_out`. Adapter maps provider-specific terminations onto these four.
- `stop_reason` — nullable. Opaque. Preserved verbatim so future analytics can recover provider-specific signals (e.g. `end_turn`, `max_tokens`, `tool_use`).
- `*_tokens` — nullable. See schema note above. Provider-reported values; framework does not infer.
- `duration_ms` — nullable. Client-measured wall time between send and receive. Distinct from `completed_at - started_at`, which is provider-measured.
- `provider_metadata` — required but may be `{}`. Opaque JSON object. Reserved for `request_id`, `attempt`, and any provider-specific fields not enumerated above. The collector validates it is a JSON object and persists it verbatim.
- `request_id` / `attempt` — inside `provider_metadata` in this version (kept out of the top level for v1 simplicity). Future versions may promote them if a clear schema emerges.

The frame does **not** include artifact references. Artifacts live in the substrate; correlation happens at query time via `invocation_id`.

### 4. `claude-code-mpm` adapter

The adapter wraps the framework's LLM call site. After each `messages.create` (or equivalent), the adapter captures the response's `usage` block plus timing and emits one NDJSON frame.

Adapter contract:

- The adapter generates a fresh `invocation_id` per LLM invocation (a single `messages.create` / equivalent call). It does **not** reuse `ToolBuffer.CallID`, which is scoped to substrate tool calls (file reads, web searches, etc.) — distinct from an LLM turn. The adapter MAY emit the same `invocation_id` it would pass to `MPM_PROVENANCE` if the framework exposes one; otherwise it generates a UUIDv7-style string locally. This guarantees exact correlation with any `artifact_provenance` rows that subsequently land, as long as both sides agree on the ID.
- Connect to the socket with a 100ms timeout. On failure, increment the local `telemetry_dropped_frames_total` counter and return.
- On socket write error mid-frame, append the frame to an in-memory ring buffer (≤1000 frames, oldest evicted). Drain the buffer on next successful send.
- The collector's `ACCEPTED` / `DROPPED` / `REJECTED` response is parsed by the adapter's send goroutine, which ticks `adapter.telemetry_accepted_total` / `adapter.telemetry_dropped_total` / `adapter.telemetry_rejected_total` counters. The LLM call has already returned by the time the send goroutine runs; the counters are diagnostic, not blocking.
- Never blocks the LLM call path on socket I/O. The adapter enqueues the frame into a per-process channel (buffered, length 1000); a dedicated send goroutine drains it. The LLM call returns immediately.

### 5. Error handling & durability

| Failure | Outcome | Why |
|---|---|---|
| `mpm-telemetry` not running | Adapter connect-attempt times out at 100ms. Frame dropped. Local counter ticks. | Cold start; the agent must keep working. |
| Socket write error mid-frame | Adapter buffers the frame in a per-process in-memory ring (≤1000 frames, FIFO eviction). Drops oldest on overflow. | Brief daemon restart. |
| Schema/version rejection | Collector returns `{status: "REJECTED", reason: "unknown_schema_version"}`. Adapter logs at warn, drops, continues. | Forward-compat with future schema migrations. |
| Conflicting duplicate `invocation_id` (same ID, different payload bytes) | Collector returns `{status: "REJECTED", reason: "invocation_id_payload_conflict"}`. Adapter logs at warn. | Adapter bug or replay attack. |
| Idempotent duplicate (same ID, same payload bytes) | Collector returns `{status: "ACCEPTED", inserted: false}`. | Retry safety. |
| Successful insert | Collector returns `{status: "ACCEPTED", inserted: true}`. | Normal path. |
| SQLite write failure inside the collector | Collector returns `{status: "DROPPED", reason: "persistence_failed"}`. Adapter counter ticks. **Does not return ACCEPTED.** | Don't lie about persistence. |

The collector increments a `telemetry_dropped_frames_total` counter on every DROP / REJECT outcome. Exposed via a future `mpm-telemetry stats` subcommand (out of scope for v1; counters are queryable directly via SQL on `telemetry.db` instead).

Deliberately **not** in this plan:

- Durable disk spool for retries across process restarts. Adding this turns the design into a JSONL recovery daemon, which is exactly the trap the architecture avoids.
- Retries with exponential backoff. Would amplify cost of a stuck daemon.

### 6. Socket handshake

`mpm-telemetry ping` returns:

```json
{
  "collector_version": "0.1.0",
  "protocol_version": "v1",
  "schema_version": "v1",
  "queue_depth": 0,
  "uptime_seconds": 1234
}
```

`queue_depth` is the depth of the collector's pending-write queue (frames accepted-but-not-yet-persisted to SQLite). In v1 the collector is single-goroutine per connection and parses-and-inserts inline, so `queue_depth` is always `0`. The field is reserved for a future buffered-write architecture and exposed now so onboarding scripts can rely on a stable shape.

Onboarding scripts call `ping` to verify the host can accept telemetry before kicking off a real workload. No fake LLM invocation required.

---

## The `observe` command — `HighTokenNoArtifactHunt`

This is the one economic observation the plan ships. It is **not** an MPM ROI metric. It is an anomaly detector: high-token sessions that yielded no new artifacts.

### Framing

> The hunt answers "which sessions burned a lot of tokens without producing any substrate artifacts?" It does **not** answer "did MPM cost more tokens than it saved?" The latter requires instrumentation this plan does not yet ship, and is explicitly deferred.

The framing matters because a session that retrieves three memories and fixes a bug without writing any new artifact is a high-value session. The hunt would flag it. The flag is observation, not verdict. Human arbitration decides.

### Mechanics

`mpm-telemetry observe [--since <cutoff>] [--high-token-threshold <n>] [--min-invocations <n>]`

Defaults: `--since=now-7d`, `--high-token-threshold=100000`, `--min-invocations=1`.

For each session above threshold:

1. Read invocations from `telemetry_invocation` (local SQL, no cross-DB).
2. Compute `total_input_tokens` and `total_output_tokens` **as descriptive aggregates**, not monetary cost. The output of the hunt is the aggregate sum, not a dollar figure.
3. Call `mpm call mpm_provenance --payload '{"action":"count_by_session","session_id":"<id>"}'` to get the count of artifacts with `session_id = <id>` in `artifact_provenance`.
4. If the artifact count is zero, emit a finding via `mpm call mpm_lessons --payload '{"action":"save","fact":"...","type":"observation","tags":[...]}'`.

Finding payload:

```json
{
  "fact": "Session <id> burned <N> tokens across <K> invocations (input <X>, output <Y>) with zero new artifacts in artifact_provenance. Investigate: was this retrieval-driven work, retry-loop, or failed session?",
  "type": "observation",
  "tags": ["telemetry", "high-token-no-artifact", "auto", "cycle_<n>"]
}
```

The finding is saved by `mpm` core into the lessons table — the standard substrate pathway. Future critic hunts or human review pick it up like any other finding.

### Why this hunt lives in `mpm-telemetry`, not in `mpm-critic`

The critic is a pure linter of the substrate; it knows about `memories`, `decisions`, `theories`, `lessons`, and `artifact_provenance`. It does **not** know about token counts or provider billing. Adding a hunt that reads `telemetry_invocation` would couple the critic to the telemetry binary's data model.

By placing the observation command in `mpm-telemetry` instead:

- `mpm-telemetry` owns telemetry semantics (raw usage + cross-DB artifact lookup).
- `mpm-critic` owns cognitive findings (substrate integrity, decay, contradiction).
- The coupling point is `mpm call mpm_provenance --payload '{"action":"count_by_session","session_id":"<id>"}'` — a stable, versioned substrate API surface, not an internal-to-`mpm-telemetry` data structure.

The critic can later consume the observations produced by `mpm-telemetry observe` if it wants to escalate them, but it does not need to know `mpm-telemetry` exists.

### Why `tokens_total` is descriptive, not monetary

A future cost projection must use provider/model pricing rules with versioned effective dates. Pricing rules change. Cached-input rates differ from input rates. Reasoning tokens have separate pricing on some providers. Baking any of that into the hunt would make the hunt wrong the moment a provider changes pricing.

The hunt records raw counters. The future `mpm-telemetry cost --pricing catalog.json` subcommand applies pricing at read time. Until that subcommand ships (out of scope), the data is preserved exactly as the provider reported it.

---

## Configuration

The collector reads:

- `MPM_WORKSPACE` — to find the socket path (same convention as the rest of MPM's path resolution; see CLAUDE.md "Path Resolution").
- `MPM_TELEMETRY_SOCKET` — optional explicit override; takes precedence over the derived path.

The derived socket path is `$MPM_WORKSPACE/runtime/mpm-telemetry.sock` (the `runtime/` subdirectory is created if absent; `0600` permissions on the socket). All adapters resolve the socket through `MPM_WORKSPACE` so they agree on the location without coordination.

The collector writes nothing to `mpm_config.json`. Telemetry is opt-out by not installing `mpm-telemetry` or by pointing adapters at a non-existent socket.

---

## Testing

### Unit tests (`internal/telemetry/*_test.go`)

- Schema validation: good frame, unknown `schema_version`, missing required field, malformed timestamp, `completed_at < started_at`, non-object `provider_metadata`.
- Persistence idempotency: same `invocation_id` + identical bytes → one row, second call returns `inserted=false`.
- Persistence conflict: same `invocation_id` + different bytes → `REJECTED`.
- Non-blocking guarantee: collector peer that never reads does not block other concurrent senders.
- Null vs 0: token fields with `null` JSON persist as SQL NULL; `0` persists as `0`.
- Status enum: only the four allowed statuses are accepted; others rejected.

### Integration test (`scripts/smoke_telemetry.sh`)

- Boot `mpm-telemetry serve` with a temp `telemetry.db`.
- `mpm-telemetry ping` → handshake response includes all four fields.
- Send 3 synthetic frames (1 with `parent_invocation_id` set, 1 with `status=failed`, 1 with NULL token fields).
- Assert: 3 rows in `telemetry_invocation`, parent link preserved, NULL token fields persisted as SQL NULL.
- Send a duplicate with identical bytes → assert `inserted=false`.
- Send a duplicate with different bytes → assert `REJECTED`.
- Run `mpm-telemetry observe --since=0` against a seeded scenario with 1 high-token no-artifact session → assert a finding is saved via `mpm call mpm_lessons`.
- `mpm-telemetry cost --pricing testdata/one-model.json` → assert the projected output shape.

---

## Migration & schema versioning

`telemetry.db` is a new file; no migration from any prior state.

Frame schema is `v1`. Unknown versions are rejected. A future `v2` will ship with a migration script and a one-release overlap window where the collector accepts both versions.

---

## Open questions deferred to follow-up plans

- Pricing catalog format and source-of-truth.
- Retention / pruning policy for `telemetry.db`.
- Durable spool for adapter-side retries.
- Adapter integrations for the other 5 frameworks.
- Analytics / dashboards / cost-over-time visualisation.
- Promotion of `request_id` / `attempt` from `provider_metadata` to top-level.
- Whether `observe` should emit findings to the audit log directly (bypassing `mpm_lessons save`) — depends on whether the operator wants durable substrate artifacts or transient annotations.