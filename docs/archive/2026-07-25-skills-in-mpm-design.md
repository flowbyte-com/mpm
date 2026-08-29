# Skills in MPM — Native Procedural Memory Layer

**Date:** 2026-07-25
**Status:** Design — awaiting review

## Goal

Make MPM the substrate for an agent's **procedural memory** ("how to act"), alongside its existing **declarative** ("what is true" — lessons) and **episodic** ("what happened" — memories) layers. Today, procedural knowledge ships as static markdown files alongside plugins/themes (e.g. `agent-shell-theme/skills/agentshell/SKILL.md`). MPM can't see, version, share, or rank them. The agent has no discovery path beyond remembering the file exists.

This design adds a new first-class collection, `collection='skills'`, that turns skills into queryable, versionable, shareable, decay-aware artifacts — owned by MPM end-to-end.

## Taxonomy: three pillars of cognitive architecture

```
collection='memories'    → "What happened"  (raw, ephemeral, event-log)
collection='lessons'     → "What is true"   (durable synthesized truths)
collection='directives'  → "Wake bootstrap" (always-on prose the agent
                                              reads on every session)
collection='skills'      → "How to act"     (on-demand procedures, this
                                              design — NEW)
```

Skills and directives are distinct on purpose: directives are short, passive prose the agent reads on every wake (≤2KB; e.g. "always call `read_wake_context` first"). Skills are richer, structured procedures the agent pulls when relevant, with frontmatter + steps + constraints.

## Architecture decisions

| Decision | Choice | Rationale |
|---|---|---|
| Storage location | New `collection='skills'` on existing `memories` table | Reuses FTS5 indexing, single-connection discipline, `SaveMemoryNode` scanner; no schema fork |
| Relationship to directives | Distinct collection | Directives = wake bootstrap (always-on). Skills = on-demand procedure. Different shapes, different lifecycles |
| Activation depth | Passive — stored + surfaced; agent interprets | Matches Claude Code's Skill tool semantics. Substrate stays a data plane, not a workflow engine |
| Discovery | Three-tier: `list_skills` + `read_skill` + `proactive_recall_hint` extended | Mirrors existing decision/theory surfacing; agent sees catalogue on every wake without an extra round-trip |
| Versioning | Slug-suffixed: `skill:<name>-v<semver>`; `is_latest` flag; `supersedes` linkage | Mirrors npm/git-tag convention; no version table needed |
| Cross-agent sharing | `is_global` flag → replicated to `shared.memories` | Reuses existing Shared Premium scoring; operator-gated promotion |
| Authoring | File ingestion (watch daemon, `kind: skill` frontmatter) + `mpm call save_skill` (MCP) + `mpm save-skill --file` (CLI) | Three entry points, all going through `SaveMemoryNode` so the scanner is structurally guaranteed |
| Frontmatter location | Inside `content` column | Single source of truth; FTS5 searches the whole doc; "drop a SKILL.md into a watched dir" just works |
| Wake-context block | `<available_skills>` prepended, bounded top 20 by weight + total count | Persistent procedural memory visible on every wake without bloat |
| Decay floor | 90 days vs memories' 30 days | Skills are operational artifacts, not raw facts |

## Schema

### Row shape

```
id:        skill:<name>-v<semver>            (e.g. skill:agentshell-v2.0.0)
collection:'skills'
content:   <YAML frontmatter> + <markdown body>
tags:      ["skill", "<domain-tags>"]
metadata:  {
             is_latest: 1,
             supersedes: "skill:agentshell-v1.0.0",
             promoted_at: <unix>,
             content_hash: <sha256>,
             author: <agent-name>,
             scope: "local" | "global",
             decay_floor_days: 90
           }
is_global: 0 | 1
weight:    default 5; LTM-promoted at weight ≥ 10
```

### Frontmatter schema (parsed out of `content` on read)

```yaml
---
name: agentshell
description: Use when working with the AgentShell WordPress theme. Covers MCP tools, config via REST API, safe HTML injection.
when_to_use: working with the AgentShell theme, agentshell-mcp tools, agentshell-blocks widgets
domain: wordpress
version: 2.0.0
constraints:
  - "never edit header.php, footer.php fixed sections"
  - "always call agentshell_get_config before mutations"
steps:
  - call: agentshell_get_config
  - call: agentshell_set_css_var
    args_from: user_input
---
```

The body is free-form markdown: procedure, rationale, examples, troubleshooting.

### Schema migration

No new tables. The `memories` table already accepts arbitrary `collection` values; the existing 2026-07-23 audit gap closure confirmed this. The decay floor (90 days) lives in `metadata.decay_floor_days` — no new column needed. `handlers_gc.go` reads it during the decay sweep and uses it as the override threshold when present.

## Read surface

### Tier 1 — `list_skills`

Lightweight inventory. Returns one row per `is_latest=1` skill with `name`, `when_to_use`, `is_global`, `weight`. Used for "what's available?" checks and the wake-context block.

```go
// Signature
func (dm *DatabaseManager) ListSkills(scope string) ([]SkillSummary, error)
// scope: "local" | "shared" | "all" — default "all"
```

### Tier 2 — `read_skill {name, version?}`

Full body. Returns the parsed frontmatter + markdown body + metadata. `version` omitted → resolves latest. `version` given → exact semver match.

```go
// Signature
func (dm *DatabaseManager) ReadSkill(name, version, scope string) (*Skill, error)
// name without version → find is_latest=1 row
// name with version → exact id match (skill:<name>-v<version>)
```

Resolution pattern mirrors `handleUpsertScheduledTask` in `internal/core/tools/handlers.go:1824-1833` — single indexed lookup, fail-fast on miss.

### Tier 3 — `proactive_recall_hint` extended

Existing `ProactiveRecallHint` (`internal/core/directive_tools.go:55`) extracts keywords from the conversation snippet and finds overlapping decisions/theories via `FindEpistemologyOverlaps`. Extend it to also scan `collection='skills'` rows where the parsed frontmatter `when_to_use` field overlaps the conversation keywords. Same scoring math, same delivery block format (`<system_*_hint>` prepended to the tool's Content array).

Skills become a fourth hint source alongside decisions, theories, and lessons.

### Wake context

`read_wake_context` gains an `<available_skills>` block appended below the existing content. Bounded to top 20 by weight with the total count surfaced. Agents see the catalogue on every wake without an extra round-trip. The block is opt-out via a future flag; default-on matches the design's "persistent procedural memory" goal.

## Write surface

### Three entry points, all through `SaveMemoryNode`

| Path | Use case | Format |
|---|---|---|
| File ingestion (existing watch daemon) | Organic capture — drop a `.md` file with frontmatter into a watched `skills/` dir | markdown file → memory row, `collection='skills'` derived from frontmatter `kind: skill` or parent dir name |
| `mpm call save_skill` (MCP) | Programmatic creation by the agent | payload `{name, version, when_to_use, body, constraints, steps, ...}` → row |
| `mpm save-skill --file path` (CLI) | Operator curation from terminal | reads file, parses frontmatter, calls same code path as MCP |

The watch daemon already routes markdown memories through `SaveMemoryNode` with the scanner. Adding a `kind: skill` frontmatter convention makes file ingestion zero-effort: operators can copy `agentshell/SKILL.md` into a watched dir and it becomes a queryable skill row.

### Versioning mechanics

- `save_skill` with same `name` and a *new* `version` → updates `is_latest` on the old row to 0, sets `is_latest=1` on the new row, stores `supersedes` linkage.
- `save_skill` with same name+version → reject unless `force=true` (no silent overwrites; matches the seed engine's "operator edits are visible" voice).
- `save_skill` with same name, no version → requires explicit `version` arg (no implicit auto-bump; agents should think about versioning).

### Shared-DB promotion

`mpm call promote_skill_to_global {skill_id, confirm: true}` → copy row to `shared.memories` with `is_global=1`, store `metadata.derived_from_skill_id` for lineage. Same operator-gate as `promote_to_global`. Read path accepts `scope` param (`local` | `shared` | `all`); shared rows get the existing Shared Premium boost from recall scoring.

### Deletion

`mpm call delete_skill {skill_id}` → soft-delete via the existing `shred_memory` path (cascades through `topic_memberships` and `challenged_theories`). Hard deletion is not exposed in v1; for forensics, version history stays queryable.

## Security

### Poison scanner

`SaveMemoryNode` already runs the 20-pattern secret/poison scanner on every write. Skill content goes through it for free. The static-analysis test `TestScannerCoverage_AllMemoriesWritersScanContent` structurally guarantees no new write path can skip the scanner.

### Skill authority

Two risks:

1. **Adversarial injection** — a shared skill that tells the agent to do something dangerous. Mitigation: shared skills are operator-promoted only (`confirm: true` gate). Trust gating lives on the shared-DB promote path, not on the read path.
2. **Drift** — local edits to a seeded skill silently diverge. Mitigation: skill rows participate in the same drift-detection pattern as `seed.ApplyDirectives` (content_hash comparison, surfaced in audit). A future `mpm ops init skills` bootstrap command can mirror `mpm ops init directives`.

### LTM & decay

Skills follow the existing `weight ≥ 10 → LTM` rule. Decay floor of 90 days (vs memories' 30 days) — skills are operational artifacts, not raw facts. Tuning knob in `internal/core/handlers_gc.go`.

## File changes (implementation outline, refined by the plan)

```
internal/core/schema.go              — no new column; decay_floor_days lives in
                                        metadata JSON (already supported)
internal/core/directive_tools.go     — ReadSkill, ListSkills, extended
                                        ProactiveRecallHint
internal/core/tools/handlers.go      — handleSaveSkill, handleReadSkill,
                                        handleListSkills,
                                        handlePromoteSkillToGlobal,
                                        handleDeleteSkill (soft)
internal/core/tools/registry.go      — register new tools
internal/core/seed/                  — new skills.go registry + engine
                                        support, new `mpm ops init skills`
cmd/mpm/ops_init_skills.go           — mirror of ops_init_directives.go
cmd/mpm/watch.go                     — kind:skill frontmatter →
                                        collection='skills'
internal/core/wake_context.go        — append <available_skills> block,
                                        bounded top 20
docs/                                 — README "Skills" section,
                                        MIGRATIONS note
tests:
  internal/core/skills_test.go            — frontmatter parser, version
                                             resolution, is_latest flip
  internal/core/skills_parity_test.go     — mirrors directives_parity
  internal/core/tools/schema_guard_test.go — new tool schemas
  internal/core/skills_discovery_test.go  — proactive_recall_hint surfaces
                                             skills on keyword overlap
  internal/core/skills_security_test.go   — scanner coverage, force flag,
                                             shared-DB promotion gate
```

## Testing strategy

- **Unit**: frontmatter parser, version resolution (`name` → latest row), `is_latest` flip on save with version bump, scope filtering on read.
- **Integration**: file ingestion of a `.md` with `kind: skill` frontmatter produces a row in `collection='skills'`; MCP `read_skill` returns parsed frontmatter + body; `save_skill` with conflicting version rejected without `force`; `promote_skill_to_global` requires `confirm: true`; wake context includes `<available_skills>` block.
- **Discovery**: `proactive_recall_hint` surfaces a skill when the conversation mentions the `when_to_use` keyword, doesn't surface when it doesn't.
- **Scanner coverage**: `TestScannerCoverage_AllMemoriesWritersScanContent` structurally guarantees the new write paths get scanned. Add an assertion that `handleSaveSkill` calls `SaveMemoryNode`.
- **Drift parity**: `skills_parity_test.go` mirrors `directives_parity_test.go` — same id, same content hash semantics.

## Migration / rollout

No data migration required for v1 — `collection='skills'` is a new value, not a transformation of existing data. Existing `directives` and `lessons` rows are untouched.

Rollout order:
1. No schema migration — `decay_floor_days` lives in metadata JSON; `handlers_gc.go` learns to read it
2. Core DM methods: `ReadSkill`, `ListSkills`
3. MCP tools: register, wire to handlers
4. CLI command: `mpm save-skill`, `mpm list-skills`, `mpm read-skill`
5. Watch daemon: `kind: skill` frontmatter convention
6. Wake context: `<available_skills>` block
7. Shared-DB promotion: `promote_skill_to_global`
8. Bootstrap: `seed/skills.go` registry + `mpm ops init skills` (mirrors directives)

Each step independently shippable behind its own commit.

## Open questions

1. **Skill dependency graph** — should a skill be able to declare `requires: [skill:foo-v1]` and refuse to read if missing? Useful for tooling chains; out of scope for v1. Document as future work.
2. **Skill execution telemetry** — should `read_skill` log a "skill X was loaded for conversation Y" event for analytics? Out of scope for v1; can layer on via existing `audit_log` table.
3. **Multi-language skills** — body in non-English? `when_to_use` keyword overlap assumes ASCII; semantic overlap via embeddings may be needed for multilingual agents. Out of scope for v1; the existing embedding infrastructure handles this if we choose to wire it.
4. **Skill deprecation** — should there be a `deprecated_at` timestamp that hides the skill from `list_skills` but keeps it queryable? Useful for migrations; document as future work, mirror the cluster `snoozed`/`resolved` lifecycle.

## Acceptance criteria

The arc is "done" when:

1. `mpm ops init skills` (or equivalent) seeds a baseline skill library without silent mutation of operator-edited rows
2. Dropping `agentshell/SKILL.md` into a watched dir produces a queryable skill row
3. `mpm call read_skill {name: "agentshell"}` returns parsed frontmatter + body
4. `mpm call list_skills` returns the catalogue; the wake context includes an `<available_skills>` block bounded to top 20
5. `proactive_recall_hint` surfaces a skill when conversation keywords overlap its `when_to_use` field
6. `mpm call save_skill` with conflicting version rejects without `force: true`; with a new version, flips `is_latest` correctly and stores `supersedes`
7. `mpm call promote_skill_to_global` requires `confirm: true`; the shared row is queryable from a second agent's MPM via `scope=all`
8. `TestScannerCoverage_AllMemoriesWritersScanContent` continues to pass with the new write paths
9. Existing tests for directives, lessons, memories continue to pass — no regression on the parallel collections
10. README has a "Skills" section documenting the format, authoring paths, and versioning model
