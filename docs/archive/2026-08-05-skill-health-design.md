# Skill Health — Technical Design

**Status:** DRAFT (proposal under critical review)
**Date:** 2026-08-05
**Precedents:** `docs/superpowers/specs/2026-07-25-skills-in-mpm-design.md`,
`docs/capability-lifecycle-spec.md`
**Read alongside:** `internal/core/skill.go`, `internal/core/skill_db.go`,
`internal/core/capability/state.go`, `internal/core/capability/forge.go`,
`internal/core/audit.go`, `internal/scheduler/scheduler.go`.

---

## Executive Summary (read this first)

**This document is deliberately negative.** Its thesis is that "Skill Health"
is **two different concepts** in MPM, both of which either already exist or
should *not* exist.

1. **For executable capabilities** (`internal/core/capability/`): the
   state machine (`active`, `degraded`, `fractured`, `needs_revision`,
   `rolled_back`, `retired`) already IS the health system. The empirical
   metrics (`success_count`, `failure_count`, `fracture_count`,
   `last_invoked_at`, `last_failure_at`, `avg_latency_ms`) already feed it.
   The `capability_health` tier-1 primitive already exposes it. Three
   planner-but-unimplemented scheduler ticks (`skill_forge_tick`,
   `skill_observation_tick`, `skill_gc_tick`) are referenced in the
   capability spec but not yet coded. **No new schema is needed.**

2. **For Skills** (`collection='skills'`, markdown procedures read by LLMs):
   there is no state machine, no execution, no degradation. Every signal a
   health view would need already exists in either the memories row
   (`weight`, `reinforcement_count`, `last_accessed_at`, `deleted_at`,
   `is_global`, `is_long_term`), the skill metadata (`is_latest`,
   `content_hash`, `promoted_at`, `derived_from_skill_id`,
   `decay_floor_days`), or the retrieval observability layer
   (`retrieval_metadata.reuse_count`, `last_retrieved_at`, `success_count`).
   **A separate "skill health" concept would duplicate these signals in a
   narrower, less authoritative form.**

**Recommendation: do not introduce a `skill_health` artifact.** Build the
two scheduler ticks that the capability spec already commits to. Add a
read-only `mpm skill inspect <name>` projection for the collection='skills'
case if operator visibility is the goal. Otherwise, the question dissolves
when held next to the existing substrate.

The full reasoning, with section-by-section answers to the 13 prompts, is
below. The answers for sections 1–4, 6, 8 are short: "this already exists"
or "this shouldn't exist." Sections 5, 7, 9–13 carry the load.

---

## 1. What "Skill Health" actually represents

**Disambiguate first.** "Skill" maps to two distinct substrate kinds:

| Kind | Storage | Executor | Failure modes |
|---|---|---|---|
| Skill | `memories` row with `collection='skills'` | never — read by LLMs as text | content drift, supersession, soft-delete |
| Capability | `capabilities` row, execution guarded by `bwrap` | `internal/core/capability/executor.go` | exit_code!=0, fracture cluster, source_hash drift |

A health concept only makes sense where there is a process that can fail.
Markdown skills cannot fail in their lifecycle — they can only be
*contextually stale*. The right word for that condition is "drift," and
the existing `metadata.content_hash` (SHA-256 of the body, written by
`SaveSkill`) is the drift-detection primitive already wired.

**Health, for the capability case, is "the current position in the
lifecycle state machine plus the empirical metrics that justify that
position."** For the skill case, "health" is a misnomer for "freshness" +
"usefulness," and both are already queryable as derived signals.

**Strong claim:** naming the concept "Skill Health" risks collapsing two
substrates that MPM has gone to some length to keep separate. The cognitive
taxonomy (memory 2026-07-25) puts skills as "How to act" — procedural
memory that survives validation. Capabilities are runtime artifacts with
trust domains. The Capability Lifecycle spec keeps them joined through the
forge pipeline, but only by metaphor; in storage, they live in different
tables.

---

## 2. Observable events that influence health

### Capabilities (already implemented or specified)

| Event | Source | Influence |
|---|---|---|
| `INSERT INTO capability_invocations` | executor | drives failure_rate, fracture cluster detection |
| `UPDATE capabilities SET state=...` | forge / scheduler / operator | state-machine transition |
| `INSERT INTO capability_events` | forge / cascade / scheduler | audit trail; source of truth for "why this state" |
| `last_failure_stderr` updated | executor | surfaced on demotion |
| `source_hash` mismatch at invocation | executor | `EventSourceHashMismatch`, immediate retire |
| `mpm capability grant-operator` | operator-only CS-3 | `EventOperatorApproval`, `metadata.operator_approved_at` |

### Skills (collection='skills')

| Event | Source | Influence (as a *derived* signal) |
|---|---|---|
| `SaveSkill` write | operator / ingest | `created_at`, `updated_at`, `content_hash` change |
| `SaveSkill` same-name flip | tx with semver compare | `is_latest` toggled on prior row |
| `mpm reinforce` | agent | `reinforcement_count`, `weight`, `last_accessed_at` |
| `mpm weaken` | operator | delta on the same columns |
| `ShredSkill` | operator | `deleted_at` |
| `PromoteSkillToGlobal` | operator `confirm=true` | `is_global=1`, `metadata.promoted_at` |
| FTS5 surface hit | retrieval observability layer | `retrieval_metadata.reuse_count`, `success_count` |
| `ChallengeMemory` consumes `skill:<name>-v<ver>` | contradiction_log | (skill stays; only `challenge_memory` row is asserted) |

All of these are already persistent.

---

## 3. Events that should NEVER influence health

These would violate the principles in the prompt and the substrate's
existing guardrails:

- **LLM inferenced "intent to use" or "would have been useful."** No
  proxy scores. No embeddings-derived utility estimates. The 2026-07-07
  security audit explicitly pushed the scanner into `SaveMemoryNode` so
  observability is structural, not opt-in; health must follow the same
  posture — observational, not inferred.

- **`updated_at` as mtime.** Use `metadata.content_hash` for drift; do
  not surface "last touched" as a health signal (encourages no-op touches).

- **Operator-only reads (`mpm read-skill` in a TUI without follow-up).**
  Reading a skill is not validation. The retrieval observability layer's
  `reuse_count` only increments through the hint surface
  (`proactive_recall_hint`), not via every read.

- **Anything that depends on an LLM call to interpret.** Health must be
  explainable from recorded evidence (per the prompt). The scheduler is
  forbidden by the existing architecture from making LLM-calls in
  critical-path handlers; it may only invoke them via evidence-channel
  tools (the synthesis worker DLQ is the precedent for "LLM work is
  off the hot path").

- **Cross-DB shared-DB signals from `shared.memories`.** Federation scope
  is `'all'`, `'local'`, or `'shared'`; health for shared skills must
  not silently double-count under two DBs. The retrieval layer already
  enforces this; copying it is what sharing-local copiers do.

- **Reuse of any cache that disagrees with the source row.** The
  retrieval observability layer explicitly chose: "Search ranking is NOT
  altered by this table; the data is observability only until a future
  ranker chooses to consume it." Health must inherit this stance.

---

## 4. How health differs from neighbouring concepts

This is the section that argues for not building new things.

| Concept | What it answers | Where it lives | Health's relationship to it |
|---|---|---|---|
| **State** | "Where is this in its lifecycle?" | `capabilities.state` (capabilities); `metadata.is_latest` (skills); `deleted_at IS NULL` | Health *consumes* state. For capabilities: state is the lifecycle; the empirical metrics round it out. For skills: state semantics are trivial (latest/non-latest, active/deleted, local/global) and don't need rounding out. |
| **Trust** | "What is this *allowed* to do?" | `capabilities.execution_domain` (sandbox/restricted/trusted/operator) — earned by track record | Health influences trust (probation → active is a health-driven promotion), but trust *gates execution*, health *informs inspection*. Distinct. |
| **Promotion** | "When did this level up?" | `metadata.promoted_at` (skills); `promoted_at INTEGER` column (capabilities) | Discrete event. Health is continuous assessment across time. |
| **Capability** (the noun) | "What is this callable thing?" | The row itself | Health is one facet of a capability, not a separate object. |
| **Usage** | "How often is this referenced?" | `reinforcement_count`, `success_count` + failure_count, `retrieval_metadata.reuse_count` | Health *uses* usage as input. Usage alone is the wrong abstraction — a 100-success 0-failure capability is merely "battle-tested," not "healthy"; health must combine success+failure + recency + state. |

The failure mode I want to flag: a "health column" on `memories` or
`capabilities` would crowd *each* of these into a single JSON column,
because health is a vector over them. That is exactly the anti-pattern MPM
guards against in the cognitive taxonomy — collapsing distinguishable
concepts into one bag. The substrate already provides the disaggregated
signal; the projection is the right shape.

---

## 5. Additive, decaying, event-sourced, recomputed, cached?

Recommended answer per concept (with rationale rooted in MPM today):

| Concern | Capabilities | Skills | Why |
|---|---|---|---|
| Source of truth | **Event-sourced** (`capability_events` + `capability_invocations`) | Pure derived query (no new write path) | Capabilities have causal transitions worth replaying; skills don't, but every input signal is already in `memories` / `retrieval_metadata`. |
| Computation | **Recomputed** view (small aggregates over indexed state) | **Recomputed** view (single SQL projection) | Both are cheap because state-indexed. Caching hides divergence. |
| Caching | **No.** | **No.** | The 2026-07-07 audit's "embedding probe cached via sync.Once" fix shows the failure mode: cache surfaced fresh-write content as stale for the duration of the TTL. Health views must not repeat that. |
| Decaying | **No additional decay** (the existing `metrics decay_window` from `capability_invocations` suffices — a 90-day TTL in `invocation_ttl_*_hours` already controls cohort size). | **No.** (Skills already have `is_long_term=1` + `weight > 1` LTM clamp via `DecayWeights`.) | Both substrates already have weighted decay. The skill-side `decay_floor_days: 90` is documentary; the LTM rate IS the enforcement. A separate "health decay" would double-decay. |
| Additive | **No** — the metrics counters increment via `UPDATE` in the executor; they ARE the addition. | **No** — reinforcement_count is already the additive signal. | Adding on top of additive columns is double-counting. |

The single rule that covers all five: **promote no field to "health" that
isn't already health-shaped in the existing schema. Compute the rest.**

---

## 6. Minimum persistent schema

### For Capabilities: zero new tables.

The capability-lifecycle spec already defines:
- `capabilities.state` (lifecycle column with CHECK constraint)
- `capabilities.success_count`, `failure_count`, `fracture_count`,
  `last_invoked_at`, `last_failure_at`, `last_failure_stderr`,
  `avg_latency_ms`, `promoted_at`
- `capability_invocations` (telemetry)
- `capability_events` (audit trail with `event_type` taxonomy of 9
  types, including promotion, demotion, fracture, rollback,
  dependency_shatter, retirement, supersede, source_hash_mismatch,
  operator_approval)

A `health` column on `capabilities` would either:
- duplicate state (violating "the state column is canonical"), or
- aggregate state + metrics (collapsing the disaggregated counts the
  state machine depends on to detect transitions).

Neither is acceptable. **Do not add it.**

### For Skills: zero new tables.

For an aggregate view of one skill, the existing inputs are:

```sql
SELECT
  m.id, m.collection, m.tags, m.weight, m.is_long_term,
  m.is_global, m.reinforcement_count, m.last_accessed_at,
  m.created_at, m.updated_at, m.deleted_at,
  json_extract(m.metadata, '$.is_latest')          AS is_latest,
  json_extract(m.metadata, '$.content_hash')       AS content_hash,
  json_extract(m.metadata, '$.author')             AS author,
  json_extract(m.metadata, '$.promoted_at')        AS promoted_at,
  json_extract(m.metadata, '$.derived_from_skill_id') AS derived_from_skill_id,
  rm.reuse_count, rm.last_retrieved_at, rm.success_count
FROM memories m
LEFT JOIN retrieval_metadata rm ON rm.node_id = m.id
WHERE m.collection = 'skills' AND m.id = ? AND m.deleted_at IS NULL;
```

All twelve columns and three JSON-extracted metadata fields already
exist. Indexes that serve this query:
- `idx_memories_collection_deleted_created (collection, deleted_at, created_at)`
- `idx_memories_reinforcement (collection, deleted_at, reinforcement_count, weight)`
- `idx_memories_decay (collection, deleted_at, is_long_term, weight)`
- `idx_memories_accessed (last_accessed_at)`

The query plan is index-scan on `collection='skills' AND id=?`, fine
for the SKILL layer's expected size (dozens, not millions).

### What I would refuse, if asked

| Asked-for | Why refuse |
|---|---|
| `skill_health` table | Duplicates retrieval_metadata + memories columns; introduces a third source of truth that must be transactionally synced |
| `skill_health_score` column on `memories` | Combines weighted signals by an opaque formula — breaks "explainable from recorded evidence" |
| `skill_events` table | No state transitions exist on skills; events require events |
| `last_health_check_at` column | Implies a periodic computation; double-counts decay |

---

## 7. Scheduler behaviour

### Already specified for capabilities — implement, don't extend

The capability-lifecycle spec commits to three tick handlers, none of
which exist yet in `internal/scheduler/scheduler.go`. They are:

| Tick handler | Cadence | Purpose |
|---|---|---|
| `skill_forge_tick` | 60s | drain `draft` and `linted` rows; idempotent (§3.2 pipeline) |
| `skill_observation_tick` | 60s | sweep `active` rows in observation window; tolerance breach triggers §5.2 rollback |
| `skill_gc_tick` | 60s | TTL sweep on `capability_invocations` using `invocation_ttl_*_hours` |

These belong on the existing `Scheduler.RegisterTickHandler` surface
(already used by `cascade_drain`). No new pattern.

**Do not add a `skill_health_tick`.** Periodic health computation caches
state that drifts from the underlying metrics; the right cadence is "on
demand from CLI" or "on the same 60s tick that already touches state."

### For skills: no scheduler work is justified.

A 60s tick that walks `collection='skills'` to bump a "freshness"
counter has no consumer — freshness is already a derived query.
The LTM decay sweep (`DecayWeights`) runs through `gc_run` already.

### What if operator wants weekly "skill audit"?

Build it as a CLI command, not a tick:

```bash
mpm skill audit [--name <pattern>] [--json]
```

A command is observable, has a single function, and doesn't write to a
long-lived schedule. It can be wired into `cron` by the operator if
desired; MPM doesn't need to own the cadence.

### Recommended scheduler additions

- Implement the three capability ticks (already spec-committed).
- Add a `Kind="skill_audit"` wake handler so a wake row with
  `metadata.kind="skill_audit"` triggers `mpm skill audit`. This lets
  agents or humans schedule an audit without a 60s background tick.
- That's it. Resist anything else.

---

## 8. Lifecycle transitions driven by health

### Capabilities: fully designed and off-limits to redesign

The lifecycle matrix in `capability/state.go` is authoritative:

```
draft → linted → validated → probation → active
                                       ↓
                            degraded ⇄ (recovery)
                                ↓ (3 fails / 60s)
                           fractured → needs_revision → draft
                                ↓
                           rolled_back (terminal, lineage kept)
                                ↓
                           retired (terminal, operator / supersede)
```

The health-driven paths are `active → degraded`, `degraded → fractured`,
`degraded → active` (recovery), `fractured → needs_revision`. Each is
defined in `state.go:validTransitions` and tested in `state_test.go`.
The scheduler's `skill_observation_tick` drives them; the executor
records invocations; events table audits. **This part of the design is
done; don't reopen it under the name "skill health."**

### Skills: there is no lifecycle to drive

Skill rows in `collection='skills'` have:
- `is_latest` (true/false) — version currency
- `is_global` (0/1) — promotion
- `deleted_at` — soft-delete

There are no transitions among these as a result of empirical signal.
A skill that is never read stays at `weight=5, reinforcement_count=0`
forever (the LTM clamp). The architecture's stance: a skill that is
unused has the same right to remain as one that is constantly surfaced;
the retrieval ranker chooses who to show, not the lifecycle.

If the operator decides a skill is obsolete, they `ShredSkill` it (or
`mpm skill shred <name>`). That's the only "driven by health" action a
skill takes, and it's operator-driven, not automated. The
`metadata.is_latest` flag handles version obsolescence automatically
when a newer version is saved.

**Adding health-driven transitions on skills would invent a behaviour
the substrate deliberately omits.** Don't.

---

## 9. Failure recovery

### Capabilities

| Failure | Recovery mechanism | Already in spec? |
|---|---|---|
| Source-code tamper (`source_hash` mismatch) | `EventSourceHashMismatch` → retire | §2.4.1 |
| 3 failures / 60s | `degraded → fractured` → cascade (if `author_theory_id` set) → `needs_revision` | §1.3, §5.1 |
| Pre-promotion regression | probation gates (5 success, ≤0.10 failure rate) | §1.4 |
| Post-promotion regression in observation window | full rollback (§5.2) including downstream shatter (§5.3) | §5 |
| Dependency fracture | recursive cascade walker (§5.3, depth ≤ 5) | §2.3 CTE |
| `bwrap` infrastructure failure | forge fail-closed (linter failure = hard-reject) | `forge.go:135-148` |
| Operator override | `* → retired` wildcard transition | `state.go:CanTransitionTo` |

Recovery is *transactional and idempotent*. That's the existing design.
No "skill health" recovery needs to be invented.

### Skills

| Failure | Recovery | Notes |
|---|---|---|
| Garbage content | `ShredSkill`, then re-`SaveSkill` | Soft-delete preserves forensics; the row is invisible to retrieval due to `deleted_at IS NULL` filter |
| Wrong frontmatter | `SaveSkill` returns `frontmatter missing` error pre-DB | No DB write |
| Scanner blocks content | `saveMemoryRow` redirect to `mirror.jsonl` | Pre-existing audit guarantee; skills inherit it via the shared write primitive |
| Supersession | save v2 → prior row's `is_latest` flips in same tx | Automatic, no operator step |

There's nothing to add.

### What I would NOT propose

- "Self-healing" content re-write. That is an LLM-in-the-loop
  correction, which the prompt's first principle forbids ("never
  speculative").
- Auto-revert on a retrieval hit. A skill being useful isn't a sign it
  was wrong before.

---

## 10. Operator overrides

### Capabilities — already designed

- `mpm capability grant-operator` (CS-3, shipped) → `EventOperatorApproval` + `metadata.operator_approved_at`
- `mpm capability retire <id>` (planned) → `* → retired` transition
- `mpm capability rollback <id>` (planned) → manual rollback outside observation window
- Direct SQL: forbidden by the audit-only read invariant.

### Skills — already designed

| Operator action | MPM surface | Effect |
|---|---|---|
| Save new version | `mpm save-skill --file <f>` | inserts; flips prior `is_latest` |
| Force overwrite | `mpm save-skill --force ...` | updates `content`, `metadata`; preserves `weight`, `reinforcement_count` per `skill_db.go:278-335` design comment |
| Promote to global | `mpm promote-skill <id> --confirm` | `is_global=1`, stamps `promoted_at` |
| Shred | `mpm shred-skill <id>` (the public name, behind which `ShredSkill` is the CoreDB method) | soft-delete via `deleted_at` |
| Reinforce / weaken | `mpm reinforce <id>` / `mpm weaken <id>` | reward or penalize the `weight` + `reinforcement_count` |

These compose to cover "the operator decided to retire this skill"
without any health concept: `ShredSkill` + a new save is enough.

**No new override is needed.**

---

## 11. Integration with the Skill Forge

### What is the "Skill Forge"?

The Capability Forge in `internal/core/capability/forge.go`. Per the
specification it owns the validation pipeline
(`schema → scanner → linter → dedup → dry-run → store-insert`).
Scheduler tick `skill_forge_tick` drains pending proposals.

### How health integrates

The forge never *uses* health. Its outcome is deterministic from the
payload + the prior state of the substrate. Health is a downstream
consumer of forge outputs:

```
forge.go → INSERT capabilities(state=validated) ← forge outcome
                ↓
         scheduler tick → capability is now in probation
                ↓
         executor → INSERT capability_invocations (every call)
                ↓
         scheduler tick → probability → state transitions
                ↓
         capability_events → audit + projection
                ↓
         mpm capability health <id> → operator view
```

Health sits at the read edge. It does not feed back into the forge, the
executor, or the tick decisions; it surfaces "what is the current
aggregate view?" to the operator.

### What this rules out

- A "healthy enough" gate in the forge ("don't lint if recent failure
  rate is fine"). Adds probabilistic input to a deterministic pipeline.
- A "boost trust domain on high health score" hook. Trust promotion is
  earned through `success_count` against the probation policy, which is
  count-based, not score-based.

---

## 12. CLI inspection

### Capabilities — exists, surface it

```bash
mpm capability health <name|id> [--json]
```

The `capability_health` tier-1 primitive is already seeded
(`internal/core/seed/capabilities.go:418-476`); the underlying
`capability-health` tool is registered in the registry. The format is:

```
target:           cap-foo-bar-v1
state:            degraded
success_count:    42
failure_count:    3
failure_rate:     0.067
fracture_count:   1
last_invoked_at:  2026-08-04 18:22
last_failure_at:  2026-08-04 17:01
avg_latency_ms:   230
last_failure_stderr: timeout: No such file or directory
```

This is the canonical health view. **Nothing to add here.**

### Skills — propose `mpm skill audit` as a read-only projection

There is currently no `mpm skill audit <name>` command. The closest is
`mpm read-skill <name>` (full body + parsed frontmatter). An audit
view would append:

```
skill:                 agentshell v1.0.0
is_latest:             true
is_global:             false
author:                cli
created_at:            2026-07-25 10:00
updated_at:            2026-07-25 10:00
weight:                5
is_long_term:          true (decay floor 1)
reinforcement_count:   0
last_accessed_at:      never
content_hash:          b0e1...
retrieval:
  reuse_count:         0
  last_retrieved_at:   never
  success_count:       0
promotion:
  promoted_at:         —
  derived_from:        —
state:
  deleted:             no
```

Implementation: a single SQL query (the one in §6), Go-side unmarshal,
form-stringified output. The audit is a *projection*, not a *fact*. No
write to the DB, no audit row, no event.

A flag `--json` produces machine-readable output for agent consumption.
A flag `--all` lists each skill with a one-line summary (extends
`mpm list-skills`).

This is the *only* new CLI surface I would build, and it is
read-only and projection-only.

---

## 13. Architectural inconsistencies this would introduce

Read each row as a reason to **not** add the thing it describes.

| What would be added | Architectural violation |
|---|---|
| A `health` column on `capabilities` | Collapses lifecycle state with empirical metrics; the state machine depends on the disaggregated counters to detect transitions (e.g. `3 fails / 60s` reads `capability_invocations`, not a precomputed score). |
| A `skill_health` table | Duplicates `retrieval_metadata` (already a 1:1 observability surface) and `memories` columns; introduces a third source of truth that must be transactionally synced. |
| A `skill_health_score` column on `memories` | Opaque formula breaks "explainable from recorded evidence" (prompt principle 3) and "scheduler reasons without LLM" (principle 5). |
| Periodic `skill_health_tick` | Cache drift; pairs with the 2026-07-07 audit's "embedding probe cached via sync.Once" footgun; the substrate heals *on-demand* via SQL, not via tick-driven materialization. |
| Health-driven state transitions on `collection='skills'` | Invents transitions where the architecture deliberately omits them; cognitive-taxonomy reasoning says skills are stable procedural memory, not stateful artifacts. |
| `skill_health` JSON column on `memories` | Bypasses the FTS5 + index story: a JSON column is unindexable for the precise queries `list_skills` and `read_skill` already serve. |
| LLM-judged health | Crosses the security boundary the 2026-07-07 audit drew between scanner (structural) and synthesis (off the hot path). |
| A separate `skill_events` table | No state transitions exist; events without semantics are noise. |

### Inconsistency I would actually flag

The capability spec uses the word "skill" liberally
(`propose_skill`, `skill_forge`, `skill_forge_tick`,
`skill_observation_tick`, `skill_gc_tick`) for what is actually a
*capability*. This is a naming hazard, not a design bug. It is the
single source of friction that makes "skill health" feel like it should
exist as a sibling — but it should *not*; the two words name two
substrates.

A small documentation pass to:

- rename `propose_skill` → `propose_capability`,
- rename `skill_forge_tick` → `capability_forge_tick`,
- rename `skill_observation_tick` → `capability_observation_tick`,
- rename `skill_gc_tick` → `capability_gc_tick`,

…in CLI surfaces and spec text, would defuse the confusion without
changing a line of code in `forge.go`, `state.go`, or the executor. The
result: "capability" is the noun for the executable artifact with
health; "skill" is the noun for the procedural document with freshness.
Two concepts, two vocabularies, no shared "health" abstraction.

---

## Recommendation (one paragraph)

**Do not build "Skill Health" as a single concept.** Treat it as a
disambiguated pair: (a) capability health is the existing lifecycle
state machine + capability_invocations, surfaces through
`mpm capability health`, and is what the three pending scheduler ticks
deliver; (b) skill "health" is a derived SQL projection over
`memories` + `retrieval_metadata`, surfaced through `mpm skill audit`.
Implement the three committed scheduler ticks (capability scope).
Add the read-only CLI projection for skills. Resolve the "skill" /
"capability" naming hazard in the capability spec. Stop there.

If this is rejected and a unified health concept is required: build it
as a derived view, not a stored column, and do not let it participate in
any state machine, scheduler tick, or event source. It remains an
inspection surface; the substrate's causal records remain the source
of truth.

---

## Appendix: tests that already pin the existing guarantees

These tests will fail if the proposed "Skill Health" schema is added on
top of the existing substrate in a way that violates any contract. They
are the safety net for saying "no":

| Test | Location | Property |
|---|---|---|
| `TestSaveSkill_SetsIsLongTerm` | `internal/core/skills_decay_test.go:25` | skills are LTM at write |
| `TestDecayFloor_SkillsSurviveAggressiveDecay` | line 79 | LTM clamp prevents weight<1 |
| `TestDecayFloor_NonLTMRowUntouchedByDecaySweep` | line 107 | sweep is LTM-gated |
| `TestState_*` family | `internal/core/capability/state_test.go` | every transition is matrix-validated |
| `TestScannerCoverage_AllMemoriesWritersScanContent` | (referenced in CLAUDE.md) | no write path bypasses scanner |
| `TestSqlOpenOwner_OnlyWhitelistedCallSites` | `internal/core/sqlopen_owner_test.go` | no new DB handles |
| `TestForbidHealthCheckComponents` (if it existed; would add) | — | would gate that the new audit CLI is read-only |

Adding "skill_health" must not break any of the above without a written
justification in `audit.md` (per the CLAUDE.md security section's
"before adding a new external surface" rule).
