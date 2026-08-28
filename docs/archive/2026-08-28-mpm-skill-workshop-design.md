# MPM Skill Workshop — Design Spec

**Date:** 2026-08-28
**Status:** Awaiting user review
**Author:** Claude (architectural-path brainstorming per `superpowers:brainstorming`)
**Feature-freeze boundary:** Substrate is feature-frozen (post-alpha-3, 2026-08-27). This spec covers an explicit "agent-workflow feature" exception that builds on existing skill persistence and validation — it does not modify core memory, temporal, pointer, telemetry, scheduler, provenance, or skill-storage schema.

---

## 1. Overview

The **MPM Skill Workshop** is a structured **skill-formation and validation** workflow that helps an agent turn a repeated, non-obvious experience into a durable, reusable skill. The workflow has two halves:

- **Formation** (host-side, lives in the canonical agent protocol and the §11.2 prompt template): the agent reasons about the experience, fills in the prompt template, and produces a candidate proposal.
- **Validation** (server-side, lives in the workshop action): the workshop enforces the contract — decision-model thresholds, `when_to_use` quality, duplicate detection, frontmatter and scanner checks, idempotence, and durable skill identity.

The seam between formation and validation is the workshop's `save_payload`: the protocol helps the agent formulate; the workshop validates and (when criteria pass) publishes. This split avoids a server-side LLM dependency for content generation while still giving the agent a controlled path from "I keep doing this" to "future-me now has a reusable procedure for it."

The workshop extends — does not replace — the existing `mpm__mpm_skills` MCP tool with a single `workshop` action. The action runs synchronously and returns one of three outcomes. No in-flight proposal state is exposed to agents or operators.

The workshop addresses a gap surfaced during the alpha-3 release (2026-08-27): the MPM substrate persists skills but offers no authoring workflow. Skills today flow through `SaveSkill` with only frontmatter validation — no proposal stage, no lint, no duplicate check, no decision model. The capability subsystem has a 6-step forge pipeline with a 10-state lifecycle (`internal/core/capability/forge.go`), but it is not wired for skills. The workshop brings similar structure to the skill surface without introducing a parallel registry or new persistence tables.

**Core principle:** the workshop is a judgment aid, not a skill-spam cannon. It must reject trivial and one-off experiences, prefer refinement of existing skills over creating duplicates, and use the existing skill persistence and validation architecture as the source of truth for identity and versioning.

---

## 2. Goals & Non-Goals

### Goals

1. Provide a guided, repeatable authoring workflow for skills.
2. Reject trivial / one-off / fact-not-procedure experiences.
3. Detect duplicate procedures against the existing skill catalog before publishing.
4. Support refinement of existing skills when failures are reported.
5. Populate `when_to_use` deliberately with task-oriented phrasing, not skill titles.
6. Use the existing `SaveSkill` path for publication; preserve versioning and history.
7. Honor feature freeze: zero new tables, zero new columns, zero schema migrations.
8. Expose one MCP action (`workshop`) and one CLI subcommand (`mpm skill workshop`).
9. Maintain idempotence and concurrency safety through an in-memory single-flight cache.
10. Document the formation rule in the canonical agent protocol and per-host managed sections.

### Non-Goals

1. Replace the existing `mpm_skills` tool or add parallel tools.
2. Introduce a `skill_proposals` collection or in-flight proposal state.
3. Add a dedicated `deprecate` or `supersede` MCP action.
4. Persist a `workshop_key` cache across daemon restarts.
5. Auto-create skills on every session, task, or successful action.
6. Replace, destabilize, or codify OpenClaw's host-specific skill behavior — none currently exists in the repo (see §11.3).
7. Add cron, watchdog, or network services to the workshop path.
8. Modify the wake-context `<available_skills>` catalog (it already surfaces published skills automatically).

---

## 3. Architectural Commitment

The workshop extends the existing `mpm__mpm_skills` MCP tool. The action enum grows from `[save, read, list, delete, promote_to_global]` to `[..., workshop]`. Internally, the workshop runs a synchronous pipeline (input validation → decision model → `when_to_use` validation → duplicate check → identity check → non-mutating validation → publish-or-return). Agents see only the outcome; pipeline stages are implementation detail.

```
┌───────────────────────────────────────────────────────────────┐
│ FORMATION (host-side)                                         │
│   Canonical protocol §11.2 prompt template                    │
│   → agent reasons, fills in proposal + decision_model         │
└────────────────────────┬──────────────────────────────────────┘
                         │  save_payload + decision_model
                         ▼
┌───────────────────────────────────────────────────────────────┐
│ mpm__mpm_skills workshop (single new action)                 │
└────────────────────────┬──────────────────────────────────────┘
                         ▼
┌───────────────────────────────────────────────────────────────┐
│ VALIDATION (server-side, single-flight sync.Map)             │
│                                                               │
│  1. Input validation     — size limits, schema check          │
│  2. Decision model       — 4-axis scoring + boundary check    │
│  3. when_to_use check    — length / verb / conjunction quality│
│  4. Duplicate check      — substring + FTS5 (refine excludes) │
│  5. Identity check       — durable (name,version) idempotence │
│  6. Validation           — dm.ValidateSkill (NON-MUTATING)    │
│  7. Publication          — dm.SaveSkill / SaveSkillAndDeprecate│
│  8. Audit + idempotence  — LogAudit + sync.Map cache          │
└────────────────────────┬──────────────────────────────────────┘
                         ▼
            src/db/mpm.db (memories, collection='skills')
```

**Critical safety property:** stage 6 (`Validation`) calls a non-mutating helper `dm.ValidateSkill(...)` that reuses `SaveSkill`'s parser/scanner internals but does **no DB writes**. Only stage 7 (`Publication`) is allowed to persist. A `candidate` outcome can never accidentally appear in `memories` because the validation stage never touched the database.

Three outcomes (`published`, `candidate`, `rejected`) map directly to the spec's SKILL-WORTHY / CANDIDATE / NOT-SKILL-WORTHY categories. `published` writes through the existing `SaveSkill` path and returns the new `skill_id`. `candidate` returns a complete `save_payload` so the agent can decide via the existing `save` action. `rejected` returns a concise reason and writes nothing.

Global promotion remains a separate, existing lifecycle operation (`promote_to_global`). The workshop never promotes on its own.

---

## 4. Workshop Contract

### 4.1 Request shape

```json
{
  "action": "workshop",
  "mode": "form" | "refine",
  "intent": "<skill name>",
  "change_type": "correction" | "extension" | "restructuring" | "purpose_change",
  "decision_model": {
    "reusability":     <0|1|2>,
    "non_obviousness": <0|1|2>,
    "stability":       <0|1|2>,
    "leverage":        <0|1|2>,
    "boundary":        "procedure" | "fact" | "preference" | "one_off"
  },
  "proposal": {
    "name":         "<kebab-case skill name>",
    "version":      "<semver>",
    "domain":       "<area>",
    "description":  "<one-line purpose, ≤120 chars>",
    "when_to_use":  "<comma-separated task phrases, ≥30 chars>",
    "steps":        [{"title": "...", "body": "..."}],
    "constraints":  ["..."]
  },
  "task_context":         "<string, ≤50KB>",
  "workflow_description": "<string, ≤50KB>",
  "failure_recovery":     "<optional string, ≤20KB, refine mode only>",
  "recent_actions":       ["<string, ≤20 entries>"],
  "evidence": {
    "memory_ids":    ["<≤10 mpm memory ids>"],
    "lesson_ids":    ["<≤5 mpm lesson ids>"],
    "reference_ids": ["<≤5 mpm reference ids>"]
  },
  "workshop_key":         "<optional opaque string for idempotence>"
}
```

**`change_type`** (required for `mode: "refine"`; ignored for `mode: "form"`): classifies the nature of the refinement and drives a deterministic version bump (see §9). Eliminates the agent's ability to choose a version that doesn't match the change's nature.

**`intent` semantics:**
- `mode: "form"` — `intent` is the proposed new skill name. The workshop validates that no skill with that exact name already exists at the proposed version.
- `mode: "refine"` — `intent` is the existing skill name to look up. The workshop fetches the latest version via `dm.ReadSkill(intent, "")` and uses its `id` as the prior-version ID for `SaveSkillAndDeprecatePrior`.

**Pointers** (`memory_ids`, `lesson_ids`, `reference_ids`) carry IDs only. The workshop server-side dereferences to current content via the existing pointer/resolve machinery. The workshop payload never carries raw conversation transcripts. Total request size cap: 256KB.

The `decision_model` block is required: the workshop validates the agent-supplied scores against §6 thresholds. The `proposal` block is required and must be a complete skill frontmatter + steps payload; the workshop does not generate proposal content from context (per §6, the integers are agent-assigned and the proposal content is too — no LLM call server-side).

### 4.2 Response — `published`

```json
{
  "outcome": "published",
  "skill_id": "skill:<name>-v<semver>",
  "version":  "<semver>",
  "publication": {"status": "active"},
  "audit_id":   "<system_audit_log row id>"
}
```

The skill row exists in `memories WHERE collection='skills' AND id='<skill_id>'`. `is_latest=1` on the new row; prior versions remain with `is_latest=0`. Read-back assertion (per the substrate defense triad) confirms persistence before returning.

### 4.3 Response — `candidate`

```json
{
  "outcome": "candidate",
  "decision_model": {
    "reusability":     "high|medium|low",
    "non_obviousness": "high|medium|low",
    "stability":       "high|medium|low",
    "leverage":        "high|medium|low",
    "boundary":        "procedure|fact|preference|one_off",
    "total":           <0-8>
  },
  "duplicate_check": {
    "exact_match": false,
    "close_matches": [
      {
        "name":         "...",
        "id":           "skill:...-v...",
        "overlap_score": <0.0-1.0>,
        "reason":       "..."
      }
    ]
  },
  "validation": {
    "status":   "passed_with_warnings" | "failed",
    "warnings": ["weak_when_to_use", "missing_description"],
    "errors":   []
  },
  "save_payload": {
    "action":       "save",
    "name":         "<as supplied or workshop-adjusted>",
    "version":      "<semver>",
    "domain":       "<area>",
    "description":  "<one-line purpose, ≤120 chars>",
    "when_to_use":  "<comma-separated task phrases, ≥30 chars>",
    "steps":        [{"title":"...","body":"..."}],
    "constraints":  ["..."]
  }
}
```

The `save_payload` is the wire shape the agent passes straight to `mpm_skills save` if it accepts the candidate. The workshop may adjust fields in the payload based on validation (e.g., a `weak_when_to_use` warning may be addressed by adding suggested phrasing). The workshop surfaces such adjustments under `validation.adjustments[]` when applicable. The candidate persists only in the agent's conversation context; the workshop does not write it to the database.

### 4.4 Response — `rejected`

```json
{
  "outcome": "rejected",
  "reason":  "one_off_action" | "trivial_procedure" | "fact_not_procedure" | "already_covered" | "low_confidence",
  "decision_model": {
    "reusability":     "...",
    "non_obviousness": "...",
    "stability":       "...",
    "leverage":        "...",
    "boundary":        "...",
    "total":           <0-8>
  }
}
```

No database writes. No `audit_id` field.

---

## 5. Internal Pipeline

The workshop runs the following stages in order, returning at the first terminal stage:

```
input validation
   │
   ├─ size limits / schema check
   ▼
[refine mode] fetch existing skill via ReadSkill(name, "")
   │
   ▼
decision model  ──── 4-axis scoring + boundary check (§6)
   │
   ├─ boundary != "procedure"  → return rejected
   ├─ total ≤ 3                → return rejected
   │
   ▼
when_to_use validation  (§7)
   │
   ├─ weak (length < 30, no verb, equals name, no conjunctions)
   │     │
   │     ▼
   │  if mode == "form" and warning count >= 2 → downgrade to candidate
   │  if mode == "refine" and warning count >= 2 → return rejected
   │
   ▼
duplicate check  (§8)
   │
   ├─ top match overlap_score > 0.6  → return candidate with duplicate_check populated
   │
   ▼
identity check  (§5.1, §10)  ──── durable idempotence at (name, version) level
   │
   ├─ (name, version) exists with identical content  → return published with existing skill_id
   ├─ (name, version) exists with different content   → return candidate with version_collision
   ├─ (name, version) does not exist                  → continue
   │
   ▼
[refine mode] change_type → version-bump check
   │
   ├─ proposal.version doesn't match deterministic bump from change_type
   │     → return candidate with version_bump_mismatch
   │
   ▼
validation  — dm.ValidateSkill (§9.2, NON-MUTATING)
   │
   ├─ errors present   → return candidate with validation.errors
   ├─ warnings only    → return candidate with validation.warnings
   ├─ clean            → continue to publication
   │
   ▼
publication  ──── single-flight sync.Map claim (§10)
   │
   ├─ first writer: run dm.SaveSkill (or dm.SaveSkillAndDeprecatePrior for refine)
   │                  + LogAudit("skill_workshop", "published", ...)
   │                  + store response in cache entry + close done channel
   │
   ├─ concurrent caller with same workshop_key: block on entry.done, return cached response
   │
   ▼
return published response
```

**Critical safety property:** the `validation` stage runs `dm.ValidateSkill(...)` — a non-mutating helper that performs the parse + scanner + `when_to_use` checks with **no DB writes**. Only the `publication` stage is allowed to write. A `candidate` or `rejected` outcome can never accidentally appear in `memories`, because the workshop's validation path never touches the database.

### 5.1 Stage details

**Input validation** — request size ≤ 256KB; `task_context` ≤ 50KB; `workflow_description` ≤ 50KB; `failure_recovery` ≤ 20KB; `recent_actions` ≤ 20 entries; `evidence.memory_ids` ≤ 10; `evidence.lesson_ids` ≤ 5; `evidence.reference_ids` ≤ 5. Out-of-bounds returns `rejected` with `reason: "low_confidence"`.

**Refine-mode fetch** — `dm.ReadSkill(intent, "")` resolves to the latest version. If not found, returns `rejected` with `reason: "already_covered"` semantics inverted — the intent points to a nonexistent skill.

**Decision model** — see §6.

**`when_to_use` validation** — see §7.

**Duplicate check** — see §8.

**Identity check (durable idempotence)** — before validation/publish, look up `dm.ReadSkill(proposal.name, proposal.version)`. If a row exists with the **identical** `content` (compared by stable content hash), the workshop returns `published` immediately with the existing `skill_id`. This makes the operation safely idempotent at the durable skill-identity level — even if the `workshop_key` cache is lost across a daemon restart, re-executing with the same `(name, version)` and identical content produces the same `published` result, never a `UNIQUE(id)` constraint error. If the row exists with **different** content, the workshop returns `candidate` with `validation.errors: ["version_collision"]` (the agent must bump the version).

**Validation** — calls `dm.ValidateSkill(proposal)`, a **non-mutating** helper extracted from `SaveSkill`'s parser/scanner internals (see §9.2). The helper runs the frontmatter parser, the 19-pattern secret/poison scanner, and `when_to_use` rule checks against the proposal; it returns `(warnings []string, errors []string)` with no DB writes. Failures are accumulated (not fail-fast) and surfaced in `validation.errors`. The scanner is invoked on the proposal body and `when_to_use` text.

**Publication** — calls `dm.SaveSkill` (form mode) or `dm.SaveSkillAndDeprecatePrior` (refine mode). Both methods perform a read-back assertion before returning. The read-back is the load-bearing persistence guarantee (substrate defense triad §3). Only this stage is allowed to write to the `memories` table.

**Idempotence cache** — see §10.

---

## 6. Decision Model

A lightweight, structured 4-axis assessment. No numeric scoring machinery is introduced beyond integer buckets. The four axes are:

| Axis | 0 | 1 | 2 |
|---|---|---|---|
| **Reusability** | one-off (happened once, no signal of recurrence) | project-local (likely to recur within this project) | cross-project (likely to recur across projects) |
| **Non-obviousness** | obvious from tool help or `mpm skill list` (no procedure needed) | needs some thought (heavily context-dependent) | genuinely non-obvious (sequence is not documented anywhere) |
| **Stability** | brittle (relies on current state that will change) | mostly stable (depends on stable interfaces) | long-lived (depends on durable primitives) |
| **Leverage** | trivial savings (saves seconds) | meaningful work saved (saves minutes) | saves hours of reasoning (avoids re-deriving) |

`boundary` is assessed separately and is the gating criterion:

| Boundary | Skill-worthy? |
|---|---|
| `procedure` (a reusable method, sequence of steps, decision flow) | yes (gates everything) |
| `fact` (a single durable observation) | no — use `mpm memory save` instead |
| `preference` (a style / taste / judgment) | no |
| `one_off` (a single event) | no |

**Total** = sum of the 4 axes (range 0–8). Outcome mapping:

| Outcome | Condition |
|---|---|
| `rejected` | `boundary != "procedure"` OR `total ≤ 3` |
| `candidate` | `total` is 4–5 OR `total ≥ 6` with `when_to_use` validation warnings OR duplicate detected (overlap > 0.6) |
| `published` | `total ≥ 6` AND `boundary == "procedure"` AND `when_to_use` validation clean AND duplicate top match overlap ≤ 0.6 AND validation clean |

The workshop does not invent a new scoring subsystem. The integers are agent-assigned in the prompt template (see §11.2); they are not computed by an LLM call server-side. The workshop simply checks the boundary and total.

---

## 7. `when_to_use` Validation

`when_to_use` is a structured task-phrase taxonomy, surfaced to the agent through the wake-context `<available_skills>` catalog and surfaced again through `proactive_recall_hint` via case-insensitive substring match (per `directive_tools.go:194` and `:239`). Existing skills (`docs-cleanup-pass`, `idempotent-installer`) demonstrate the gold standard: comma-separated task phrases, not skill titles.

**Validation rules** (applied at the workshop):

| Rule | Severity |
|---|---|
| Length ≥ 30 characters | warning |
| Contains at least one verb-ing form (`\b\w+ing\b`) OR `to <verb>` pattern | warning |
| Does not equal `name` (skill title alone is never task-oriented) | error |
| Contains ≥ 2 noun phrases separated by commas, conjunctions (`and`, `or`), or prepositions | warning |
| Does not duplicate an existing skill's `when_to_use` (substring match, ≥ 80% overlap) | warning (close-match candidate path) |

Errors block `published` (downgrade to `candidate`). Two or more warnings downgrade to `candidate`. One warning may pass `published` if all other stages are clean.

---

## 8. Duplicate Detection

The workshop detects duplicates before publication. The detection runs against `ListSkills(scope="all")` and the existing `memories_fts` virtual table.

**Algorithm:**

1. Substring overlap: case-insensitive substring match of the proposed `when_to_use` against every existing skill's `when_to_use` (per `directive_tools.go:239` `keywordOverlap`). Score = `matched_chars / len(proposed_when_to_use)`.

2. FTS5 search: `BuildFTS5Query(name + " " + when_to_use + " " + first-step-text)` against `memories_fts` filtered by `collection='skills'`. Score = BM25 normalized to [0, 1].

3. Combined score: `0.6 × substring_score + 0.4 × ft5_score` per existing skill.

4. Top 5 skills by combined score returned in `duplicate_check.close_matches`.

5. Threshold: combined score > 0.6 on the top match → return `candidate` (not `published`) with `duplicate_check` populated. The agent sees the close match and decides whether to refine the existing skill via `mode: "refine"` instead of creating a new one.

6. Exact match (combined score ≥ 0.95 AND `name` matches existing skill name) → return `candidate` with `reason: "already_covered"` semantics in the decision model (the workshop treats an exact match as a strong signal to refine rather than re-publish).

**Refine-mode exclusion:** when `mode: "refine"` is active, the skill being refined (matched by `intent`) is **excluded** from both substring and FTS5 candidate sets. Otherwise the prior version would always flag as a top match and the workshop would refuse to publish the refinement. Other versions of the same logical skill (e.g., the prior-to-prior version) are still scanned — if a much older version surfaces as the top match (overlap > 0.6), the workshop returns `candidate` with that finding for review.

The detection is best-effort and runs in O(N) over the skill catalog. The catalog is small (typically < 50 skills per scope) so a full scan is acceptable. FTS5 lookup uses the existing `memories_fts` index.

---

## 9. Refinement Mode

`mode: "refine"` is the workflow for revising an existing skill based on observed failure or incompleteness. The workshop accepts `existing_skill_name` (via the `intent` field) and `failure_recovery` text.

**Flow:**

1. **Fetch existing skill** via `dm.ReadSkill(intent, "")`. If not found, return `rejected` with `reason: "already_covered"` semantics inverted.

2. **Inspect current state**: parse the existing frontmatter and body. Read `metadata.superseded_by` if present — if set, the skill is already deprecated; short-circuit with `candidate` (the workshop does not chain refines).

3. **Decision model re-scored** with `failure_recovery` context:
   - `reusability` += 1 if `failure_recovery` describes a recurring failure mode
   - `non_obviousness` += 1 if `failure_recovery` describes a non-obvious gap
   - Effective `total` is the sum, capped at 8

4. **Generate new version**:
   - Same `name` as the existing skill.
   - Version bump rule (deterministic, derived from the `change_type` field supplied in the workshop request — the agent must not pick the version directly):

     | `change_type` | Semver bump | Example |
     |---|---|---|
     | `correction` | patch | `1.0.0` → `1.0.1` |
     | `extension` | minor | `1.0.0` → `1.1.0` |
     | `restructuring` | minor | `1.0.0` → `1.1.0` |
     | `purpose_change` | major | `1.0.0` → `2.0.0` |

     If the `proposal.version` in the request doesn't match the deterministic bump from `change_type`, the workshop returns `candidate` with `validation.errors: ["version_bump_mismatch"]` and a suggested version.
   - New `when_to_use` extends (does not replace) the prior phrasing, plus any new task phrases surfaced by the failure mode.
   - Body preserves prior knowledge; adds new steps or correction notes.

5. **Transactional publication** via `dm.SaveSkillAndDeprecatePrior(newSkill, priorSkillID)`:
   - Both writes (`SaveSkill(newSkill)` + `DeprecateSkillVersion(priorSkillID)`) execute inside a single `*sql.Tx`.
   - If either write fails, both roll back. No partial state.
   - The prior version row (by specific `id`) gets `metadata.deprecated=true` and `metadata.superseded_by=<new_skill_id>`.
   - Older versions (beyond the immediate prior) are untouched and remain readable.

6. **Read-back assertion** on both rows before returning `published`.

The transactional coherence is implemented via the new method `dm.SaveSkillAndDeprecatePrior(...)` which wraps the two writes in a single transaction. This avoids the failure mode the user flagged: `v1 deprecated` succeeded but `v2 publication` failed, leaving the skill effectively unavailable. With transactional coherence, that failure mode is structurally prevented.

The deprecation marker is attached to the **specific prior version row** (by `id`, not by `name`), so multiple versions of the same logical skill can have different `deprecated` states if necessary (e.g., if a future workshop implementation chains refines).

### 9.1 `dm.SaveSkillAndDeprecatePrior` contract

```go
// SaveSkillAndDeprecatePrior publishes a new skill version and marks the
// specific prior version (by id) as deprecated in a single transaction.
//
// If either write fails, both roll back. Read-back assertions confirm
// persistence of both rows before returning.
//
// priorSkillID must match an existing skill row. The row is identified
// by id (not by name) so older versions sharing the logical skill name
// remain readable and unmodified.
func (dm *DatabaseManager) SaveSkillAndDeprecatePrior(newSkill *Skill, priorSkillID string) (*Skill, error)
```

**Touchpoints for implementation:**
- New method on `DatabaseManager` (`internal/core/db.go` or new `internal/core/skill_db.go` extension).
- Uses the existing `*sql.DB` from `DatabaseManager`. Per H-5 in CLAUDE.md, transactions are passed `*sql.Tx` through helpers — never use bare `*sql.DB` inside a transaction.
- Read-back assertion pattern follows `AddLesson` (`internal/core/db.go`).

### 9.2 `dm.ValidateSkill` contract (non-mutating)

```go
// ValidateSkill is a non-mutating helper extracted from SaveSkill's parser
// and scanner internals. It runs the frontmatter parser, the 19-pattern
// secret/poison scanner, the when_to_use rule checks, and returns the
// accumulated warnings and errors WITHOUT writing to the database.
//
// This is the safety seam between the workshop's validation stage and
// its publication stage: a `candidate` outcome from the workshop has
// never touched the database, because validation goes through this
// helper rather than the write API.
func (dm *DatabaseManager) ValidateSkill(skill *Skill) (warnings []string, errors []string, err error)
```

**Touchpoints for implementation:**
- New method on `DatabaseManager`. Refactors existing `SaveSkill` internals so that the parse + scan + validate pass runs against an in-memory `*Skill` value without performing any `INSERT`/`UPDATE`. The write step in `SaveSkill` is split out and reused by both `ValidateSkill` (skipped) and the publication stage (invoked).
- The scanner runs against the proposal body and `when_to_use` text, exactly as it would in `SaveSkill`. Coverage is structurally identical (the same scanner function is called).
- Returns `(warnings, errors, err)`: `err` is for unexpected failures (e.g., DB already unreachable); `errors` is for content-validation failures that surface to the agent; `warnings` is for soft issues.

---

## 10. Idempotence & Concurrency

The workshop provides idempotence at two levels:

1. **Cache level (ephemeral)** — in-memory `sync.Map` keyed on `workshop_key` provides first-writer-wins deduplication within a single daemon lifetime. Cleared on restart.
2. **Identity level (durable)** — every `published` outcome is checked against `dm.ReadSkill(name, version)`. If a row with the identical content hash already exists, the workshop returns the existing `skill_id` as `published` without re-writing. This makes the operation safely idempotent at the durable skill-identity level: even if the cache is lost across a daemon restart, re-executing with the same `(name, version)` and identical content produces the same `published` result. See §5.1 ("Identity check" stage) and §10.3 for the full correctness boundary.

The cache is a deduplication convenience, not part of skill correctness. The identity check is the durable idempotence guarantee.

### 10.1 First-writer-wins claim pattern (single-flight)

```go
type workshopCacheEntry struct {
    done     chan struct{}  // closed when first writer finishes
    response json.RawMessage
    err      error
}

var workshopCache sync.Map  // map[string]*workshopCacheEntry

// claimOrWait atomically claims the workshop_key slot. If the slot is
// unclaimed, the caller is the writer and receives the new entry. If the
// slot is already claimed, the caller blocks on entry.done and returns
// the cached response.
func claimOrWait(ctx context.Context, key string, ttl time.Duration) (*workshopCacheEntry, bool, error) {
    newEntry := &workshopCacheEntry{done: make(chan struct{})}
    actual, loaded := workshopCache.LoadOrStore(key, newEntry)
    entry := actual.(*workshopCacheEntry)

    if loaded {
        // Another caller already claimed. Wait for their response.
        select {
        case <-entry.done:
            return entry, true, nil  // cached result
        case <-ctx.Done():
            return nil, false, ctx.Err()
        case <-time.After(workshopWaitTimeout):
            return nil, false, errWorkshopWaitTimeout
        }
    }

    // We're the first writer. Schedule cleanup after TTL.
    time.AfterFunc(ttl, func() {
        workshopCache.CompareAndDelete(key, entry)
    })
    return newEntry, false, nil
}

// publishResult stores the response on the entry and unblocks waiters.
func publishResult(entry *workshopCacheEntry, response json.RawMessage, err error) {
    entry.response = response
    entry.err = err
    close(entry.done)
}
```

### 10.2 Concurrency guarantees

| Scenario | Behavior |
|---|---|
| Two concurrent calls with same `workshop_key` | One runs the pipeline; the other blocks on `entry.done` and returns the same response. Neither performs duplicate publication. |
| Same `workshop_key` called twice serially within TTL | Second call returns cached response. No DB write. |
| Same `workshop_key` called twice serially after TTL | First call expires (via `CompareAndDelete`); second call proceeds as a fresh execution. May produce a different outcome (e.g., if the underlying skills catalog changed). |
| Daemon restart, then call with same `(name, version)` and identical content | Identity check (§5.1) finds existing row with same content hash; returns `published` with the existing `skill_id`. No `UNIQUE(id)` constraint error. |
| Daemon restart, same `workshop_key`, then call with same `(name, version)` but different content | Identity check finds existing row with different content; returns `candidate` with `validation.errors: ["version_collision"]`. Agent must bump version. |
| Daemon restart, same `workshop_key`, completely fresh skill | Pipeline runs end-to-end. `published` outcome follows normal path. |
| No `workshop_key` provided | Each call proceeds independently. No caching. Identity check still provides durable idempotence. |

### 10.3 Correctness boundary

The idempotence cache is a **deduplication convenience**, not a correctness mechanism. The durable identity-level idempotence is provided by the §5.1 "Identity check" stage: every `published` outcome first checks whether `(name, version)` already exists with identical content, and if so returns the existing `skill_id` without re-writing. The source of truth for skill identity is the `(name, version)` pair on the `memories` row; the source of truth for "latest" is the `is_latest` flag. A daemon restart that drops the cache may cause re-execution; the workshop guarantees that:

- Re-execution of a successful `published` with identical content produces the same `published` outcome with the same `skill_id` (no `UNIQUE(id)` constraint error, no duplicate row).
- Re-execution with the same `(name, version)` but **different** content surfaces a `version_collision` candidate (agent must bump version).
- Re-execution of a `SaveSkillAndDeprecatePrior` is naturally safe: the prior version is already deprecated, the new version is already published; the identity check at the new version's `(name, version)` is what matters.
- Corrupted `is_latest` flags — prevented by `SaveSkill`'s transactional version-flip.
- Skipped deprecation — prevented by `SaveSkillAndDeprecatePrior`'s transactional coherence.

The contract is: **cache loss may cause re-execution, but never an ugly duplicate-publication failure for the same logical skill.**

### 10.4 TTL

24 hours, implemented via `time.AfterFunc(workshopCacheTTL, CompareAndDelete)`. The TTL is short enough that stale entries don't accumulate indefinitely; long enough that burst retries (e.g., agent retrying after a transient network error) hit the cache.

---

## 11. Host Integration

### 11.1 Canonical protocol

The canonical MPM agent protocol at `~/.mpm/agent_installation/mpm-agent-protocol.md` Section 3 ("SKILL DISCOVERY") gets a new "SKILL FORMATION" subsection. Contents:

1. **When to invoke the workshop** (4 trigger categories from spec §14 of the workshop spec):
   - Repeated manual procedure (you've executed the same steps ≥ 3 times across sessions)
   - Non-obvious debugging sequence (the resolution path is not documented anywhere)
   - Successful recovery pattern (you fixed a failure that would recur)
   - Recurring operational process (setup, integration, or maintenance that recurs)

2. **When NOT to invoke**:
   - Trivial commands or one-line fixes
   - One-off events or single observations
   - Procedures already covered by an existing skill (use proactive discovery first)
   - Facts or preferences (use `mpm memory save` instead)

3. **The 3 outcomes and what to do with each**:
   - `published`: skill is live and surfaced in `<available_skills>`. Read it via `mpm_skills read` and add to your procedural memory.
   - `candidate`: workshop generated a proposal but did not publish. Inspect `decision_model`, `duplicate_check`, `validation`, and `proposal`. If acceptable, call `mpm_skills save` with the `save_payload`. If not, discard and optionally save a memory or lesson.
   - `rejected`: not skill-worthy. Optionally save a memory or lesson capturing the insight.

4. **The `save_payload` hand-off pattern** — show the exact MCP call.

5. **Prefer refinement over creation** — when `duplicate_check.close_matches` is non-empty, prefer `mode: "refine"` with the matching skill name.

### 11.2 Decision-model prompt template

The canonical protocol includes a copy-pasteable decision-model template the agent fills in before calling the workshop:

```markdown
## Skill Formation Assessment

**Intent:** <skill name candidate>
**Mode:** form | refine (existing skill: <name>)

### Decision Model

- **Reusability** (0–2): <score> — <one-line reasoning>
- **Non-obviousness** (0–2): <score> — <one-line reasoning>
- **Stability** (0–2): <score> — <one-line reasoning>
- **Leverage** (0–2): <score> — <one-line reasoning>
- **Boundary**: procedure | fact | preference | one_off

**Total**: <0–8>
**Decision**: publish if total ≥ 6 AND boundary = procedure; else candidate / rejected

### Skill Proposal (if publishing or returning candidate)

- **Name**: <kebab-case>
- **Version**: <semver>
- **Domain**: <area, e.g., "docs", "release", "telemetry">
- **Description**: <one-line purpose, ≤120 chars>
- **When to use**: <comma-separated task phrases, ≥30 chars>
- **Steps**: <numbered procedure>
- **Constraints**: <edge cases, gotchas>
- **Evidence**: <memory/lesson/reference ids that informed the proposal>
```

The agent fills this template, then submits it (as JSON, matching §4.1) to the workshop. The workshop validates the decision model and returns the outcome.

### 11.3 Per-host managed sections

Each host adapter's managed section (the MPM-CANONICAL-BLOCK or equivalent in `~/.claude/CLAUDE.md`, the OpenClaw / OpenCode / Hermes / Pi adapter files) gets a brief appended note:

> **Skill formation.** When a workflow proves repeatable, non-obvious, and useful enough that future work would benefit from a reusable procedure, query `mpm__mpm_skills(action: "workshop")` (or, for non-MCP hosts, `mpm call mpm_skills '{"action":"workshop",...}'`). Follow the canonical protocol's "SKILL FORMATION" subsection for the decision model and outcome handling. Prefer refinement over creation when an existing skill is close.

The Claude Code adapter gets the full canonical block update automatically on next `install.sh` run.

---

## 12. Documentation

| Doc | Location | Contents |
|---|---|---|
| **Canonical protocol** | `~/.mpm/agent_installation/mpm-agent-protocol.md` Section 3 ("SKILL FORMATION" subsection) | Formation rule, decision model template, outcome handling, refinement preference |
| **Installation manual** | `~/.mpm/agent_installation/INSTALL.md` | How each host invokes the workshop; per-host invocation snippet |
| **Project README** | `agent_installation/README.md` | Brief mention linking to the canonical protocol section |
| **This design spec** | `docs/archive/2026-08-28-mpm-skill-workshop-design.md` | Architectural commitment, contract, pipeline, freeze audit |
| **Implementation plan** | `docs/archive/2026-08-28-mpm-skill-workshop-plan.md` (written via `superpowers:writing-plans` after spec approval) | TDD-ordered implementation steps |

The existing `docs-cleanup-pass` skill (`skill:docs-cleanup-pass-v1.0.0`) should be consulted during documentation updates that involve moving or renaming tracked docs. The existing `idempotent-installer` skill (`skill:idempotent-installer-v1.0.0`) should be consulted when the per-host managed sections are updated (each host's installer runs idempotently via backup + managed-block pattern).

---

## 13. Regression Tests

Per the spec's §16 and the substrate defense triad (§3, read-back assertions):

| Test | What it proves |
|---|---|
| `TestWorkshop_PublishedWorthyWorkflow` | Worthy input → `outcome: published`; row exists in `memories WHERE collection='skills'` with the returned `skill_id`; read-back assertion passes |
| `TestWorkshop_RejectsTrivialAction` | One-off action input → `outcome: rejected`; `reason: "one_off_action"`; no DB writes (verified by row count delta) |
| `TestWorkshop_CandidateSoftWarning` | Low confidence → `outcome: candidate` with `save_payload`; `decision_model.total` is 4–5 |
| `TestWorkshop_DuplicateDetection` | Existing skill with overlapping `when_to_use` → `duplicate_check.close_matches` populated with the existing skill; outcome is `candidate` when overlap > 0.6 |
| `TestWorkshop_ValidationFailure` | Malformed frontmatter → `candidate` with `validation.errors` populated |
| `TestWorkshop_RefineExistingSkill` | Refine mode + `failure_recovery` → new version published, prior version's `metadata.deprecated=true` and `metadata.superseded_by=<new_id>`; transactional coherence verified by injecting a failure in the deprecation step and asserting rollback |
| `TestWorkshop_RefineChangeTypeVersionBump` | Refine mode with each `change_type` (`correction`/`extension`/`restructuring`/`purpose_change`) → version bump matches the deterministic mapping; mismatch in request's `proposal.version` → `candidate` with `version_bump_mismatch` error |
| `TestWorkshop_Idempotence` | Same `workshop_key` twice within TTL → same response; second call observes the cached entry (no second `SaveSkill` invocation) |
| `TestWorkshop_IdentityCheckSameContent` | After a `published` outcome, calling workshop again with same `(name, version)` and identical content → returns `published` with the original `skill_id`; no `UNIQUE(id)` constraint error; no duplicate row |
| `TestWorkshop_IdentityCheckDifferentContent` | After a `published` outcome, calling workshop again with same `(name, version)` but **different** content → returns `candidate` with `validation.errors: ["version_collision"]`; no row written |
| `TestWorkshop_ConcurrencyFirstWriterWins` | Two goroutines call workshop simultaneously with same `workshop_key` → only one performs publication; the other returns the cached response; no duplicate rows in DB |
| `TestWorkshop_WhenToUseValidation` | Too short / no verb / equals name → `weak_when_to_use` warning; outcome downgraded |
| `TestWorkshop_ValidateSkillNonMutating` | `dm.ValidateSkill(...)` returns warnings/errors with no DB writes (verified by row count delta) — proves the safety seam between validation and publication stages |
| `TestWorkshop_NoNewTables` | Feature-freeze audit: schema unchanged after workshop run (compared via SQLite `sqlite_master` snapshot before/after) |
| `TestWorkshop_ScannerRuns` | Secret/poison scanner invoked through workshop path (matches existing `TestScannerCoverage_AllMemoriesWritersScanContent` contract) |
| `TestWorkshop_BoundedContext` | Inputs exceeding size limits → `rejected` with `reason: "low_confidence"`; no DB writes |
| `TestWorkshop_PriorVersionReadability` | After refine, prior version still readable via `ReadSkill(id, _)` for history/audit |

Plus a **dogfood** test (spec §17): a real MPM workflow with a non-obvious repeatable procedure (e.g., the docs reorganization that produced this spec, or the agent-installation managed-section updates) → invoke workshop → publish → later phrased differently → `proactive_recall_hint` surfaces it → reuse.

---

## 14. Feature-Freeze Compliance Audit

| Forbidden surface | Workshop status |
|---|---|
| Core memory semantics | **unchanged** — uses existing `SaveMemoryNode` for metadata updates via the same scanner contract |
| Temporal semantics | **unchanged** — uses existing `created_at` semantics; no new timestamp fields |
| Pointer architecture | **unchanged** — workshop consumes pointer IDs only; dereferences via existing `mpm_resolve` |
| Telemetry architecture | **unchanged** — uses existing `LogAudit` (`system_audit_log` table); no new telemetry tables, no watchdog events emitted |
| Scheduler architecture | **unchanged** — no cron / scheduler / watchdog integration |
| Provenance model | **unchanged** — no new provenance columns or env-var dependencies |
| Skill storage schema | **unchanged** — skills remain in `memories` table with `collection='skills'` |
| New database tables | **0** |
| New database columns | **0** |
| New MCP tools | **0** (extends `mpm__mpm_skills` action enum by 1: `workshop`) |
| New CLI commands | **1** (`mpm skill workshop` subcommand under existing `mpm skill`) |
| New schema migrations | **0** |
| New `DatabaseManager` methods | **2** — `SaveSkillAndDeprecatePrior(...)` (§9.1, transactional) and `ValidateSkill(...)` (§9.2, non-mutating helper). Both are pure-Go refactors of existing `SaveSkill` internals; no schema impact, no contract changes to `SaveSkill` itself. |

The only persistence changes are:
- New `metadata.deprecated` and `metadata.superseded_by` keys on existing skill rows (set via the existing JSON metadata column; no schema migration)
- New `LogAudit` component value `"skill_workshop"` (existing enum is open-text, no migration)

---

## 15. Open Questions / Future Work

Deferred per the user's directive ("do not expose separate `publish_proposal` or `list_proposals` MCP actions unless implementation evidence later shows agents or operators genuinely need them"):

1. **`mpm skill list_proposals`** — an operator-facing listing of recent workshop invocations and their outcomes. Could be derived from `system_audit_log` filtered by `component='skill_workshop'`. **Defer** until dogfood shows operators need it.

2. **Persistent `workshop_key` cache** — survive daemon restarts. **Defer** unless dogfood shows in-process cache is insufficient.

3. **LLM-assisted decision-model scoring** — let the workshop call an LLM to score the 4 axes instead of trusting the agent's prompt-template response. **Defer** — adds LLM dependency for marginal benefit; the prompt template is sufficient per the alpha-3 lean posture.

4. **Multi-language skill content** — current schema is English-only; `when_to_use` substring match is locale-agnostic but quality varies. **Defer** — future skill-system design (per `docs/archive/2026-07-25-skills-in-mpm-design.md` open question #3).

5. **Workshop-driven skill deprecation without replacement** — a "this skill is wrong, mark it deprecated" pathway without a successor. **Defer** — out of scope for the formation loop; if needed, would be a separate `mpm_skills deprecate` action later.

6. **Per-host invocation ceremony differences** — Claude Code's MCP integration differs from OpenClaw's `mpm call` flow. Both work; neither is preferred. **Defer** optimization.

---

## 16. Acceptance Criteria

| Acceptance criterion (from spec) | Spec section |
|---|---|
| Workshop uses existing MPM skill/forge architecture | §3, §5, §9 |
| No parallel skill registry created | §3, §14 |
| Agent can identify skill-worthy experiences | §6, §11.2 |
| Trivial/one-off experiences are rejected | §4.4, §13 |
| Existing skills are discovered before duplicates are created | §8, §11.1.5 |
| Revision/refinement is supported | §9 |
| `when_to_use` is populated intentionally | §7, §11.2 |
| Proposals enter the existing validation pipeline | §5 (validation stage), §9 (publication via existing `SaveSkill`) |
| Published skills become discoverable | §11 (wake-context already surfaces skills via `populateAvailableSkills`) |
| End-to-end experience → skill → discovery → reuse is proven | §13 (dogfood test) |
| Context/token overhead remains bounded | §4.1 (size caps, pointer-only evidence) |
| Canonical agent protocol documents the formation rule | §11.1, §12 |
| Host-specific details remain host-specific | §11.3 |
| OpenClaw host-specific skill behavior remains outside the canonical MPM workshop contract | §3, §11.3 |
| Feature freeze remains intact | §14 |
| No cron/watchdog/network service added | §14 |
| Full tests/build/lint pass | §13 (regression tests + spec §20 verification) |

---

## 17. Verification Commands

Run from the repository root (`/home/v/workspace/projects/mpm`):

```bash
# Build
make build

# Full test suite (with FTS5)
go test -tags fts5 -race ./...

# Vet
go vet -tags fts5 ./...

# Targeted workshop tests
go test -tags fts5 -race -v ./internal/core -run TestWorkshop

# Feature-freeze audit (no new tables / columns)
./scripts/audit-feature-freeze.sh   # written as part of the plan

# Dogfood (real workflow → workshop → discovery → reuse)
./scripts/dogfood-skill-workshop.sh # written as part of the plan
```

---

**End of spec. Awaiting user review before invoking `superpowers:writing-plans` to produce the implementation plan.**
