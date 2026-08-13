# WISHLIST — Multi-Agent Shared Epistemology

**Status:** Core ATTACH architecture shipped (commits `18226c8`, `b37cab0`).
The runtime substrate is in production. Remaining items below are
incremental UX/scoping work, not architectural gaps.

---

## Goal

Every MPM instance on a workstation should be able to attach a single shared SQLite database so house rules, cross-project decisions, and durable conventions live in one place that every agent can see, query, and (with operator approval) extend.

**Use case:** the agent in `/home/v/workspace/projects/mpm` and the agent in `/home/v/workspace/projects/openclaw-mpm-plugin` both need to know "we don't expose secrets to the LLM" — that's a house rule, not a per-project fact. Today it's duplicated (or missing); with shared epistemology it lives once in `~/.mpm/shared/rules.db` and both agents see it.

**Non-goals:**
- Multi-region / multi-host replication (this is a single-workstation scope)
- Access control beyond file permissions (assume operator-managed)
- Shared *secrets* storage (this is for *rules and decisions*, not credentials)

---

## Architecture

### SQLite ATTACH

`mpm.db` is the per-workspace store. `shared.db` is the workstation-wide shared store. On `DatabaseManager` init:

```sql
ATTACH DATABASE '~/.mpm/shared/shared.db' AS shared;
```

The shared DB uses the **same schema** as the local DB for tables it shares (`memories`, `theories`, `decisions`). Cross-DB queries use `shared.memories` etc. SQL grammar is standard SQLite 3.

### Config

| Var | Purpose |
|---|---|
| `MPM_SHARED_DB` | Path to shared DB. If unset or path missing, shared features are disabled (local-only mode). |
| `MPM_SHARED_READONLY` | If `1`, attach the shared DB in read-only mode. Default: read+write. |

The shared DB file path is intentionally NOT hardcoded — operator picks the location. `~/.mpm/shared/shared.db` is the recommended default but not enforced.

### Schema overlap

Shared DB uses the **same table names** as local DB (full schema, not a subset). This keeps SQL simple — `SELECT ... FROM shared.memories WHERE ...` is the same shape as `SELECT ... FROM memories WHERE ...`. Migrations apply to both DBs on startup.

A `is_global INTEGER DEFAULT 0` column on `memories` marks rows that originated as shared rules. Local writes always set `is_global = 0`; shared writes always set `is_global = 1`. This makes "where did this row come from" queryable without a second schema.

### FTS5 across schemas

Each DB has its own FTS5 virtual tables (`memories_fts`, `topics_fts`, etc.). Cross-DB recall runs both FTS queries and UNIONs results:

```sql
-- local
SELECT m.id, m.content, ..., 'local' AS source
FROM memories m JOIN memories_fts fts ON m.rowid = fts.rowid
WHERE memories_fts MATCH ? AND m.deleted_at IS NULL
UNION ALL
-- shared
SELECT m.id, m.content, ..., 'shared' AS source
FROM shared.memories m JOIN shared.memories_fts fts ON m.rowid = fts.rowid
WHERE shared.memories_fts MATCH ? AND m.deleted_at IS NULL
ORDER BY <combined_score>
```

FTS5 ranking is per-DB, so we score locally then merge with a stable secondary sort. Hybrid scoring combines them: shared results get a small boost (house rules outrank noisy local memories).

### Concurrency

SQLite ATTACH is per-connection. Each MPM instance attaches its own connection. WAL mode on both DBs allows concurrent local writers without blocking. **Cross-DB transactions are NOT supported** — every shared write is its own atomic transaction on the shared DB. This is fine for the use case (rule records are append-mostly).

---

## What's shared

| Content type | Shared? | Rationale |
|---|---|---|
| `collection='rules'` memories | **Yes** | House rules, conventions, persona overlays — the core use case |
| `collection='decisions'` marked `is_cross_project=1` | **Yes** | Durable architectural decisions spanning projects |
| `collection='theories'` marked `is_global=1` | **Yes** | Cross-project hypotheses worth surfacing everywhere |
| `collection='lessons'` marked `is_global=1` | **Yes** | Lessons learned that apply broadly |
| `collection='memories'` (default) | No | Project-specific facts |
| Sessions, theories w/o global flag, references | No | Project-scoped |
| Topics | No | Project-scoped taxonomy |

The shared DB only stores rows where some `is_global` flag is set. Reads filter `WHERE is_global = 1`. This keeps the shared DB small and curated.

---

## Tools (MCP + `mpm call`)

### Read

- `query_global_rules` — `SELECT ... FROM shared.memories WHERE collection = 'rules' AND deleted_at IS NULL`
  - Args: `query` (optional, FTS5 keyword search), `limit` (default 50)
  - Returns: list of rule memories with `id`, `content`, `weight`, `created_at`, `source: 'shared'`

- Modified `query_long_term_memory` — UNION local + shared by default. Adds `source` field to each result (`local` or `shared`). Add `scope` param: `all` (default) | `local` | `shared`.

### Write

- `record_global_rule` — write a memory to shared DB with `collection='rules'`, `is_global=1`.
  - Args: `fact` (required), `tags` (optional), `weight` (default 0.8), `provenance` (optional — operator's name/reason)
  - Returns: shared memory id + confirmation
  - Operator-only? **Yes** — no agent should be writing house rules autonomously. CLI + MCP both gated on a `confirm: true` argument; agents calling without confirm get an error.

- `promote_to_global` — copy a local memory to shared. Source row stays in local DB.
  - Args: `memory_id` (required), `confirm: true` (required, prevents accidental promotion)
  - Returns: new shared memory id, original local id (preserved), lineage record linking them

### Recall integration

`query_long_term_memory` default behavior changes: results include `source` field. Shared results appear with weight boost (+0.5 by default) so house rules dominate when relevant.

Wake context: append a "Global Rules Active" section if any rules exist. Cheap to query, high signal.

---

## Recall scoring (shared boost)

```
local row              → final = combined                          (raw relevance)
shared row (non-rule)  → final = min(combined * 1.20, 1.0)        (Shared Premium)
shared rule            → final = min(combined * 1.35, 1.0)        (rules collection)
```

The multiplier scales with relevance: an irrelevant shared row at
combined=-0.2 stays at -0.24 (still below any local match), while a
relevant shared row at combined=0.7 promotes to 0.84 and edges out a
similarly-relevant local row. This avoids the additive-with-cap model
that fights the BM25+semantic normalisation — see commit `7e886cd`
for the rationale discussion.

Why multiplicative, not additive+cap:

- **Additive `+0.5` floors zero-relevance items.** A shared rule
  with combined=-0.2 (irrelevant) gets forced to +0.3 — which is
  indistinguishable from a mildly relevant local match. The
  multiplicative model has zero-relevance items stay at zero, so
  truly irrelevant shared content cannot artificially promote.
- **Stacking preserves the two-tier hierarchy.** `1.20×` (shared) and
  `1.35×` (rules) keeps "house rules > shared decisions > local"
  as a strict monotone ranking by relevance tier.
- **The 1.0 cap is a normalisation invariant.** Without it, a
  near-perfect relevant score compounds unboundedly and breaks the
  hybrid scoring curve.

Rank order is `final` desc, then `weight` desc, then `collection` asc
(stable + predictable; ties broken by convention).

Fetch buffer: each side is requested at `limit × 2` before merge so
the multiplier-induced reordering doesn't truncate at the per-side
limit. After merge, the combined array is sliced to the requested
`limit`.

This is **not** a global override — a local memory with clearly
higher raw relevance still wins. The Shared Premium is a
relevance-preserving tiebreaker, not an elevation above relevance.

---

## Phases

### Phase 1 — Plumbing

- `MPM_SHARED_DB` env var support in `DatabaseManager` init
- ATTACH on startup, log if MPM_SHARED_DB unset (info-level, not warning — most operators won't have it)
- Graceful degradation: if ATTACH fails (file missing, permission denied), log warning and continue with local-only mode
- Migration runner applies to both DBs on startup (idempotent — same migrations, same checksums)
- Tests: ATTACH happy path, ATTACH failure path, migration on both DBs

### Phase 2 — Read tools

- `query_global_rules` MCP tool + `mpm call` entry
- Modified `query_long_term_memory` with `source` field + `scope` param
- Wake context surfaces shared rules section
- Tests: shared rule appears in recall, scope filter works, wake shows shared rules

### Phase 3 — Write tools (operator-gated)

- `record_global_rule` MCP tool + `mpm call` entry (with `confirm` gate)
- `promote_to_global` MCP tool + `mpm call` entry (with `confirm` gate)
- Tests: write to shared, lineage record, confirm gate rejects missing arg

### Phase 4 — Integration polish

- Documentation in README (single source of truth — see the README's new "Multi-Agent Shared Epistemology" section once Phase 1 lands)
- Performance: FTS5 cross-DB query latency benchmark
- Documentation of the rollback path (how to disable shared mode)

---

## Open questions

1. **Promotion lineage** — when `promote_to_global` copies a local memory, should the shared copy carry a metadata field linking back to the original local id? Useful for "this rule came from project X on date Y" provenance, but adds schema noise. Default: yes, store as `metadata.derived_from_local_id`.

2. **Rule expiration** — should `is_global` rows have a TTL? Some rules are timeless ("no secrets to LLM"); others are project-phase ("during the migration, all writes go to staging"). Default: no TTL, operators delete manually.

3. **Shared DB bootstrap** — first-time setup is operator-initiated (`mpm ops shared init`?). Or auto-create on first attach? Default: auto-create with empty schema on first `MPM_SHARED_DB` reference. Bootstrap creates `~/.mpm/shared/` directory and applies migrations.

4. **Multi-machine shared** — what if operator wants the same shared DB on two machines? Network filesystem (NFS) is the obvious answer but SQLite + NFS has well-documented risks. Default: out of scope, single-machine only.

5. **Cross-agent visibility of writes** — if agent A writes a rule, when does agent B see it? ATTACH is per-process, so B only sees new writes on next startup. Should we add a shared DB notification (e.g., LISTEN/NOTIFY)? Default: out of scope, restart-on-write-change is acceptable for the rare-event use case.

---

## References

- Original wishlist entry: README's [Roadmap](#roadmap) section (first item)
- SQLite ATTACH docs: <https://www.sqlite.org/lang_attach.html>
- FTS5 across schemas: <https://www.sqlite.org/fts5.html> — query each DB's FTS table, UNION in app layer
- MCP tool surface (current 33 tools): README's [MCP tool surface](#mcp-tool-surface) section

---

## Acceptance criteria

The arc is "done" when:

1. Two MPM instances on the same workstation (different `MPM_WORKSPACE`) see the same global rules in their recall output
2. `mpm ops shared status` reports the shared DB path, attached status, and rule count
3. The README's Roadmap section reflects that "Multi-Agent Shared Epistemology" is shipped
4. Zero data-loss regressions on the local DB path (ATTACH failures must not corrupt local state)
5. Wake context shows shared rules section when rules exist

Last revised: 2026-06-26.