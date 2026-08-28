# Directives Architecture & Multi-Framework Scoping

## 1. Overview

Directives define the core operational invariants and behavioral guidelines for agents interacting with the MPM substrate. In a multi-agent or multi-framework environment, directives are **evaluated and filtered by the MPM core**, not by individual agent plugins or harnesses.

Filtering at the substrate layer guarantees that:

1. Universal invariants (the Substrate Defense Triad) are enforced consistently across all callers.
2. Framework-specific instructions (e.g., tool calling conventions for OpenClaw vs. OpenCode) do not pollute unrelated agent contexts.
3. Client plugins remain thin transport shims with zero duplicated policy logic.

---

## 2. Scope Grammar

Directive scopes are strictly defined as flat, case-sensitive strings:

```text
scope = "global" | "framework:<id>"
```

* `global`: Applied to every agent session regardless of framework or runtime.
* `framework:<id>`: Applied only when the active session matches `<id>`.
* Standard framework tokens: `openclaw`, `opencode`, `pi`, `claude-code`, `hermes`.



Wildcards (`*`), hierarchical paths (`framework:openclaw:subagent`), and dynamic expressions are unsupported by design.

---

## 3. Storage & Schema Model

Directives are stored in the primary `memories` table with `is_prime_directive = 1`.

Scope identity lives within the row's `metadata` JSON payload rather than requiring a dedicated column:

```json
{
 "scope": "framework:openclaw",
 "stable_id": "openclaw-tool-conventions-v1",
 "tags": ["directives", "openclaw"]
}
```

### Defaults & Backwards Compatibility

* Any directive lacking `metadata.scope` implicitly evaluates as `"global"`.
* All existing baseline seed directives default to `"global"`.

### Tiered Fallback Seeding (2026-08-19)

The four constitutional baseline directives (`mpm-seed-*` in
`internal/core/seed/directives.go`) are **auto-seeded into the LOCAL store
at boot**: `NewDatabaseManager` runs `seed.ApplyDirectives` after
`initUnifiedSchema`, so a standalone runtime without an attached shared DB
is never directive-blind. The hermetic test constructor
(`NewDatabaseManagerForDB` + `InitSchema`) deliberately does not seed, so
fixtures assert on their own rows.

```text
Tier 1  LOCAL bootstrap  ── always seeded at NewDatabaseManager boot
Tier 2  SHARED overlay   ── union when MPM_SHARED_DB is attached
```

Seeding is idempotent by stable-id primary key:

* Live row, matching content → skipped.
* Live row, drifted content → operator's edit preserved, flagged (`Updated`).
* Row absent → inserted (`Created`).
* Row soft-deleted → **revived** with the current seed content (`Created`).
  A shredded baseline still occupies the stable-id PK, so a plain
  `INSERT OR IGNORE` would be silently swallowed while reporting
  "Created" — the baseline is non-negotiable, so re-init UNDELETEs it.
  This makes `mpm ops init directives` the documented recovery path after
  an accidental shred or a decay sweep.

Liveness is **sentinel-agnostic**: `deleted_at` NULL and `0` both mean
live (legacy installs wrote `0`; current code writes NULL and soft-deletes
with a Unix-epoch value). `ReadDirectivesForFramework` and the seed
dedup use `COALESCE(deleted_at, 0) = 0`, so legacy databases with
`deleted_at = 0` rows surface their directives instead of silently
reporting an empty baseline. Operators with pre-2026-08-19 databases
holding `deleted_at = 0` rows should normalize once:

```sql
UPDATE memories SET deleted_at = NULL WHERE deleted_at = 0;
```

(This also restores visibility of any other `deleted_at = 0` memories,
which every current reader treats as live.)

---

## 4. Resolution & Evaluation Pipeline

When an agent requests active directives (via `mpm_context.read_directives`), MPM evaluates scope using an **additive union model**:

$$\text{Active Directives} = \text{Directives}(\text{scope} = \text{"global"}) \cup \text{Directives}(\text{scope} = \text{"framework:"} + \text{ActiveFramework})$$

```
┌──────────────────────────────────────────────────────────┐
│                       All Directives                      │
├────────────────────────────┬─────────────────────────────┤
│    Global Invariants       │   Framework-Specific Blocks │
│ (Triad, Storage Rules)     │   (OpenClaw, OpenCode, etc.) │
└─────────────┬──────────────┴──────────────┬──────────────┘
              │                             │
              ▼                             ▼
    [ scope = "global" ]      [ scope = "framework:X" ]
              │                             │
              └──────────────┬──────────────┘
                             ▼
              Filtered Set: Global + Active
                             │
                             ▼
           Sorted deterministically by StableID
```

### Precedence & Conflicts

1. **Additive, Not Replacement:** Framework-specific directives augment global directives; they do not overwrite or suppress global invariants.
2. **Tiered Overlay:** With a shared DB attached, `ReadDirectivesForFramework` returns the union of the local baseline and the shared overlay, deduplicated by id — on an id collision the shared row wins (the shared DB is the multi-agent authority). Sorting is by ascending id, so the returned set is byte-stable across runs.
3. **Deterministic Ordering:** Within an active set, directives are ordered deterministically by ascending `StableID`.
4. **Conflict Boundaries:** If two directives contain contradictory behavioral rules, they are considered an authoring defect in the seed registry. There is no runtime override weighting or numerical priority field.

---

## 5. Runtime Transport Contract

### Ingestion via Environment

Because `mpm-mcp` is spawned per-session via standard I/O, the hosting harness injects its identity at launch:

```bash
MPM_WORKSPACE="/home/user/.mpm"
MPM_FRAMEWORK="openclaw"
```

1. `cmd/mpm-mcp/main.go` reads `MPM_FRAMEWORK` at startup and binds it to `internal.ActiveContext`.
2. Dispatcher calls to `mpm_context.read_directives` pass `ActiveContext.Framework` into `ReadDirectivesForFramework()`.
3. If `MPM_FRAMEWORK` is unset or empty, the query resolves only `scope = "global"` directives.

---

## 6. Authoring Rules for Directives

1. **Constitutional Invariants Must Be Global:** Any rule governing storage integrity, transaction safety, write read-backs, or audit retention must use `scope = "global"`.
2. **No Framework Assumption in Global Directives:** Global directives must be written in third-person, framework-neutral language. Never reference specific CLI flags, JSON RPC envelope quirks, or harness hooks in a global directive.
3. **Framework Directives Must Be Additive:** Framework-scoped directives should focus purely on integration mechanics, local context constraints, and tool invocation ergonomics.
