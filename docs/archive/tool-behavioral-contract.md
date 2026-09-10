# Tool Behavioral Contract

**Audience:** anyone designing, modifying, or reviewing an MPM tool/action surface. New-tool authors must read this before adding a registry entry; reviewers should reference it during the schema-guard gate and registry-dispatcher parity checks.

**Purpose:** the 2026-09-05 behavioural audit (see `docs/full-tool-behavioural-audit-2026-09-05.md`) surfaced four recurring categories of inconsistency, each decided per-tool or per-action, sometimes by inference, sometimes inconsistently with siblings. C.9 (silent-no-op vs error on missing), C.20 (id-key vocabulary), and the broader G.2 family (silent coercion/defaults vs explicit errors) all hit the same shape: a question that should have had a single project-wide answer, instead got one answer per occurrence. This document is the canonical answer for each.

The four sections below state a rule, audit every current tool/action against it, and call out backlog items that don't yet comply. The catalog is the reference for future remediation passes — do not re-derive these decisions from first principles each time.

---

## 1. Not-found semantics

### Rule

The correct response to "caller passed an id for a resource that doesn't exist" depends on the **mutation verb**, not the tool family. The split:

| Verb class | Behaviour on unknown id | Why |
|---|---|---|
| **Read / show / list / query / search** | **Error** (e.g. `"X not found"`, `sql.ErrNoRows`) | A read for a non-existent resource is always a caller bug or stale-id race; both deserve a clear signal so the caller can recover. |
| **Soft delete** (sets `deleted_at`, row recoverable) | **Idempotent silent success** with structured feedback (e.g. `{"success":true, "shredded":false, "rows_deleted":0}`) | The row stays in the DB; a stale-id race against another agent's earlier delete is benign. Precedent: `mpm_handoff.shred` (commit `0583bea`), `mpm_skills.delete` (silent-on-missing since inception). |
| **Hard delete** (row removed, irrecoverable) | **Error** (e.g. `"memory not found or deleted"`) | A wrong id before a destructive operation deserves a loud diagnostic. Precedent: `mpm_memory.shred`. |
| **State transition** (complete, cancel, supersede, invalidate, reopen, promote) | **Error** (e.g. `"work not found"`, `"decision already invalidated"`) | Transitioning a non-existent or already-terminal item is a caller bug. |
| **Idempotent upsert / membership** (`link`, `upsert_task`, `save` with on-conflict clause) | **Silent success** on duplicate | The unique-key collision is the no-op path. Precedent: `mpm_topics.link` (`INSERT OR IGNORE`), `mpm_wakes.upsert_task` (UPSERT). |
| **Mutation on existing row** (reinforce, weaken, snooze, set_weight, patch) | **Error** on missing | The mutation needs a row to mutate; a wrong id is a caller bug. |

**Empty / missing required id** is always an error regardless of verb — there is no scenario where `id=""` should succeed.

### Audit against current HEAD

| Tool.Action | Verb class | Current behavior | Compliant? |
|-------------|-------------|-----------------|------------|
| `mpm_memory.shred` | hard delete | Error: `"memory not found or deleted: <id>"` (`db.go:3369`) | ✓ |
| `mpm_memory.reinforce` / `weaken` / `snooze` / `set_weight` / `patch` / `promote` | row mutation | Errors on missing (per the `requireMemoryID` + DM `sql.ErrNoRows` chain) | ✓ |
| `mpm_memory.show` / `query` | read | Errors on missing id (`db.go:3324-3332`) | ✓ |
| `mpm_memory.challenge` / `restore_challenge` | state transition | Errors on missing (`challenge_restore.go:50`) | ✓ |
| `mpm_skills.delete` | soft delete | **Silent** on missing — returns `{"success":true, "skill_id":<id>}` (pre-fix `45e616f` contract, restored in this session's revert). Precedent: `mpm_handoff.shred`. | ✓ |
| `mpm_skills.read` | read | Errors on missing (`skill_db.go` `ReadSkill` returns error) | ✓ |
| `mpm_handoff.shred` | soft delete | Silent on missing — returns `{"success":true, "shredded":false, "rows_deleted":0, "message":"handoff not found; nothing to shred"}` (`handlers.go:3448-3457`) | ✓ |
| `mpm_handoff.read` | read | Errors on missing (`handlers.go:2957-3010` propagates `sql.ErrNoRows`) | ✓ |
| `mpm_work.complete` / `cancel` / `reopen` | state transition | Errors on missing or invalid state via `workNotFoundHint` translation | ✓ |
| `mpm_wakes.delete_task` | **config delete** (irrecoverable config) | Errors on missing: `"delete scheduled_task: no row with id <id>"` (`scheduled_tasks.go:334-336`) | ✓ (treated as hard-delete-equivalent for config) |
| `mpm_wakes.upsert_task` | idempotent upsert | Silent on duplicate (UPSERT) | ✓ |
| `mpm_lessons.save` | idempotent on `(fact, type)` | Returns existing lesson (reinforcement++), no error | ✓ |
| `mpm_lessons.list` / `search` | read/list | Empty result with `success:true` on no-match; error on invalid query | ✓ |
| `mpm_decisions.supersede` / `invalidate` | state transition | Errors on missing (`epistemology_tools.go:527-531` + `:594-599`) | ✓ |
| `mpm_decisions.show` | read | Errors with `"decision not found: <id>"` (`epistemology_tools.go:725-726`) | ✓ |
| `mpm_theories.show` / `list` / `query` | read | Empty result on no-match; error on invalid query | ✓ |
| `mpm_theories.resolve` | state transition | Errors on missing / wrong status | ✓ |
| `mpm_topics.link` | membership upsert | Silent on duplicate (`AddMemoryToTopic` uses `INSERT OR IGNORE`) | ✓ |
| `mpm_topics.create` / `search` / `list` / `show` | various | Per-verb rules; compliant | ✓ |
| `mpm_scratchpad.flush` | idempotent upsert (24h TTL) | UPSERT pattern, silent on re-flush | ✓ |
| `mpm_scratchpad.discard` | idempotent destroy (no-op on missing) | Silent on missing (per `handlers.go:3989-4013`) | ✓ |
| `mpm_scratchpad.read` | read | Errors on missing session_id | ✓ |
| `mpm_evidence.add` | append-only | Errors on invalid `artifact_id`/`artifact_type`/`source_group` | ✓ |
| `mpm_references.add` / `read` / `list` / `search` | read / add | Errors on missing id (read); silent on duplicate (add has no UNIQUE constraint to collide with) | ✓ |
| `mpm_confidence.*` | read / recompute | Errors on missing `artifact_id` | ✓ |
| `mpm_retrieval_diagnose` | read | Returns empty result with diagnostic, not error | ✓ |
| `mpm_blob_read` / `mpm_blob_search` | read | Empty result; no missing-id case (offset is positional, not id-based) | ✓ |
| `mpm_resolve` | read | Errors on malformed URI; empty result envelope for missing content | ✓ |
| `mpm_system.*` | various | Per-verb rules; errors on missing where applicable (e.g. `cluster_key` lookup) | ✓ |
| `mpm_skills.promote_to_global` | state transition | Errors on missing (`handlers.go:3359-3388`) | ✓ |
| `mpm_skills.workshop` | orchestration | Errors on missing directive_id | ✓ |

### Backlog (inconsistencies with this rule)

None at HEAD `8d5dd8a`. The C.9 question (the original motivation for this section) was the last open one — see §M.1 of the audit for its resolution. Future drift in this category should be caught by extending the existing per-tool regression tests with a "missing id" probe for each verb class.

---

## 2. Id-parameter naming

### Rule

Every tool family uses a **canonical primary key parameter** as the first-class field name on the wire. Aliases are accepted for backward compatibility but the canonical name is what appears in the JSON schema's `required` array and what the handler reads first.

| Tool family | Canonical id | Aliases | Notes |
|-------------|--------------|---------|-------|
| `mpm_memory` | `memory_id` | `id` | D-8.1 normalization |
| `mpm_work` | `work_id` | `id` | D-8.1 normalization |
| `mpm_handoff` | `id` | `handoff_id` (legacy), `session_id` (convenience lookup) | Tool predates the `<verb>_id` convention |
| `mpm_skills` | `skill_id` | `name`+`version` | C.20 normalization (commit `c8b0da1`); read accepts `name` alone for latest-version lookup |
| `mpm_lessons` | `id` | — | Tool predates convention |
| `mpm_decisions` | `id` for read/show | `original_id` for supersede, `decision_id` for invalidate — see backlog below | **Inconsistent across actions** |
| `mpm_theories` | `id` | — | — |
| `mpm_topics` | `memory_id`+`topic_id` for link, `id` for show/lookup | — | — |
| `mpm_references` | `id` | — | — |
| `mpm_evidence` | `artifact_id`+`artifact_type` | — | Composite key |
| `mpm_confidence` | `artifact_id` | — | — |
| `mpm_wakes` | `id` (delete_task) | — | `target_time`/`reason`/`name` for schedule/upsert_task are not id params |
| `mpm_scratchpad` | `session_id` | — | Session-scoped |
| `mpm_resolve` | `uri` | — | Different shape (mpm:// URI) |

**Decision rule for new tools:** if the tool targets a single resource, use `<resource>_id` as the canonical name (e.g. `lesson_id`, `handoff_id`). Accept `id` as a D-8.1 alias unless the tool predates the convention (handoff, lesson). Document any deviation in the registry description.

### Audit against current HEAD

The C.20 fix (`c8b0da1`) unified the `mpm_skills` vocabulary via the `resolveSkillID(p, requireVersion)` helper at `handlers.go:2884-2922`. The D-8.1 normalization covers `mpm_memory` and `mpm_work`. The remaining legacy tools (`mpm_handoff`, `mpm_lessons`, `mpm_references`, `mpm_wakes`) use bare `id` because they predate the convention; this is documented but not enforced as a violation.

### Backlog (id-key vocabulary inconsistencies)

1. **`mpm_decisions.supersede` uses `original_id`; `mpm_decisions.invalidate` uses `decision_id`** — same tool, different canonical id names for the same target. The audit's `mpm_decisions supersede` reproduction (`docs/full-tool-behavioural-audit-2026-09-05.md` §C.1) used `original_id`; a parallel `invalidate` reproduction revealed `decision_id` is required. Pre-fix consistency check: `handleShowDecision` reads `p["id"]` per D-8.1, so `show` and `list` are consistent with each other but `supersede`/`invalidate` are inconsistent with `show`/`list` *and* with each other. **Recommended fix:** standardise all four on `decision_id` (canonical) and accept `id` as D-8.1 alias; document the historical `original_id` as a deprecated alias with deprecation note in the schema.

2. **`mpm_skills` `delete` / `promote_to_global` accept `skill_id` or `name`+`version`; `read` accepts `name` alone (latest-version lookup)** — this is intentional per the C.20 fix but the asymmetry ("read allows name-only, delete requires version if you use the name alias") is worth documenting in the registry description so callers know to pass both. The `resolveSkillID(p, requireVersion)` helper already encodes the rule; the schema description is what needs the cross-reference.

---

## 3. Silent coercion vs explicit error

### Rule

Handlers **never silently coerce** or substitute defaults for malformed input. Every numeric, enum, type-mismatch, or out-of-range input is rejected with a clear error at the handler boundary. The pattern is the same one F12-1 introduced for `save.weight` and the recent remediation arc extended to all mutation inputs:

| Input class | Behaviour | Helper |
|-------------|-----------|--------|
| Numeric (weight, delta, days, limit, snooze_days, etc.) | Reject string/bool/object/null; accept only JSON number | `parseFloatStrict`, `parseIntStrict`, `parseLimitStrict` |
| Enum (type, scope, format, kind, etc.) | Reject anything outside the documented set | explicit `switch` with clear error |
| Type-mismatch (where a specific JSON shape is required, e.g. `patch` must be an object) | Reject with `"must be a JSON object"` / equivalent | direct type assertion with explicit error |
| Out-of-range (limit ≤ 0, days ≤ 0, etc.) | Reject with `"must be positive"` / equivalent | explicit check |
| Empty required id / fact / summary | Reject with `"X is required"` | pre-check at handler entry |

**The only legitimate silent-defaulting cases** are:
- `scope` defaulting to `"all"` on `mpm_skills.list` when omitted (per C.15 fix; documented in the schema description)
- `limit` defaulting to a positive integer when omitted (e.g. `defaultResolveLimit = 5`, `defaultDecisionsListLimit = 50`) — these are documented defaults, not silent coercions of invalid input

**No silent coercion of malformed input.** A caller that passes `weight: "5"` (string instead of number) gets an error, never a silent `weight: 5`. This is the explicit-error pattern, matching the §6.4 threshold table's positive-presence gate and the §8 CLI-side-limits table.

### Audit against current HEAD

The 2026-09-05 remediation arc closed all 8 silent-coercion defects catalogued by the audit:
- C.5 (`limit=0` returns 15/10) — fixed in `2316676`
- C.6 (`limit<=0` silently coerces to 50) — fixed in `2316676`
- C.7 (silent `ParseFloatOr` on reinforce/weaken/snooze/set_weight/review) — fixed in `0341735`
- C.8 (invalid lesson `type` returns empty) — fixed in `0e6a0d8`
- C.15 (invalid skill `scope` defaults to "all") — fixed in `fda6fe8`
- C.16 (invalid `format="bogus"` silently falls through) — fixed in `08720e6`

### Backlog

None. The pattern is now uniform across all numeric/enum/type-mismatch inputs.

---

## 4. Idempotency

### Rule

Every mutating action has a documented idempotency posture. The default is **not idempotent** unless the action is designed to be re-call-safe, and the action's description in `internal/core/tools/registry_list.go` must say which.

| Idempotency class | Examples | Caller contract |
|-------------------|----------|-----------------|
| **Idempotent (safe to retry)** | `mpm_topics.link`, `mpm_wakes.upsert_task`, `mpm_scratchpad.flush` / `discard`, `mpm_lessons.save` (on `(fact, type)`), `mpm_skills.delete` (soft), `mpm_handoff.shred` (soft), `mpm_skills.save` with `force=true` | Safe to retry with the same inputs; result is the same terminal state |
| **Not idempotent (each call mutates)** | `mpm_memory.save` (creates new row), `mpm_memory.reinforce`/`weaken`/`snooze`/`set_weight` (numeric mutation), `mpm_memory.patch` (object merge), `mpm_memory.shred` (irrecoverable destroy), `mpm_work.complete`/`cancel` (state transition), `mpm_decisions.record`/`supersede`/`invalidate` (state transition), `mpm_wakes.schedule` (each call adds a new wake) | Caller must guard against double-application (e.g. by reading state first, or by gating on a flag) |
| **Idempotent on duplicate, terminal on first call** | `mpm_scratchpad.promote` (atomic SELECT → INSERT → DELETE in a single tx; double-call would promote a now-empty scratchpad) | Treat as a one-shot; document the atomicity |

### Audit against current HEAD

All listed actions declare their idempotency class in their handler comments and registry descriptions. The D-8.1 alias work made the parameter naming consistent across `mpm_memory`/`mpm_work`/`mpm_handoff` so callers can use the same `id` field shape regardless of which tool they're invoking.

### Backlog

None. Idempotency is the most under-documented of the four rules; the recommendation for future tool authors is to **state the idempotency class explicitly in the registry description** (e.g. "Idempotent — safe to retry") so callers don't have to read the handler source.

---

## How to extend

A new tool or action must:

1. **Pick its verb class** from §1 (read / soft-delete / hard-delete / state-transition / upsert / row-mutation).
2. **Pick its canonical id name** from §2 (or document the deviation).
3. **Validate inputs at the handler boundary** per §3 (`parseFloatStrict`, `parseLimitStrict`, enum switches, type assertions).
4. **Document its idempotency class** in the registry description per §4.
5. **Pass the existing gates**: `TestSchemaSupersetOfHandlerPayloadReads`, `TestRegistry_AllToolsExecuteWithoutPanic`, the per-tool regression tests, the §6.4 threshold-table gate, the §8 CLI-side-limits gate, and `make test-race`.
6. **Get a `assertParityForTool` lock** in `registry_dispatcher_parity_test.go` to prevent future dispatcher/registry drift.

If a new action appears to need behaviour that doesn't fit the rules above, **update this document first**, then write the action — don't mass-rewrite the existing 21 tools' behavior to match a one-off exception. The rules are intentional defaults with named exceptions (see §3 for the legitimate silent-default cases).

---

## Cross-references

- `docs/full-tool-behavioural-audit-2026-09-05.md` §C (defects), §K (audit verdict), §M (revised §I backlog), §N (final alpha-readiness verdict)
- `internal/core/tools/registry_list.go` — authoritative tool inventory
- `internal/core/tools/registry_dispatcher_parity_test.go` — registry/dispatcher parity gate
- `internal/core/tools/schema_guard_test.go` — schema/handler parity gate
- `internal/core/build_config_invariants_test.go` — build/test-config invariants including the pre-commit Makefile routing
- `CONTRIBUTING.md` — project contribution conventions (will gain a pointer to this doc)