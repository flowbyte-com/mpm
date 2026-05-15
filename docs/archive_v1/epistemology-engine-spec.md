# MPM Epistemology Engine — Phase 1 Spec

**Date:** 2026-05-15  
**Author:** 808 (∓808)  
**Status:** Draft for review

---

## Overview

Adding two new cognitive capabilities to MPM:

1. **Decision** — an append-only ledger of choices + rationale
2. **Theory** — a hypothesis tracker with validation lifecycle

Evidence (source lineage + certainty) is deferred to Phase 2 — it requires UX work to surface confidence scores in a way that doesn't just dump JSON at v. Worth doing properly, not rushing.

**Design principle:** No new tables. No new FTS indices. No Go schema migration. The `memories` table is the substrate; collection labels + content format conventions are the protocol.

---

## What Exists vs What Changes

### Existing Foundation

```
memories table:
  id, collection, content, session_id, tags JSON,
  metadata JSON, embedding BLOB, content_hash TEXT,
  created_at, deleted_at, is_long_term, weight, ...

memories_fts (FTS5):
  content, collection, session_id, tags  ← already indexed, no changes

SaveMemory() already accepts:
  collection, content, metadata map[string]interface{}, tags
```

### Changes Required

| Layer | What's New |
|-------|-----------|
| Go backend | Zero. `mpm add --collection decisions` and `--collection theories` work today. |
| TypeScript tools | 3 new tools: `record_decision()`, `propose_theory()`, `resolve_theory()` |
| recall / query | `--collection` filtering already works in Go; need new query shortcuts |
| System prompt | Tool descriptions that tell 808 *when* to call these, not just *how* |

---

## Phase 1A: Decision Ledger

### Collection
`decisions` — stored in `memories` table, `collection = "decisions"`

### Content Format

```
CONTEXT: <what was the situation or problem>
CHOICE: <what was decided or chosen>
RATIONALE: <why this choice over alternatives>
OUTCOME: <optional — what happened when it was executed>
```

### TypeScript Tool: `record_decision()`

**Arguments:**
- `context` (string, required) — the situation or problem that prompted a choice
- `choice` (string, required) — what was decided
- `rationale` (string, required) — why this choice over alternatives
- `tags` (string[], optional) — e.g. `["architecture", "wp-plugin", "mpm"]`
- `weight` (number, optional) — default `0.5`

**Execution:** Formats args into structured content string → calls `mpm add --collection decisions --json "<content>" [--tag <t>] [--weight <w>]`

**When to call:** After any non-trivial architectural decision, library choice, file modification, or any moment where 808 chose path A over path B. If unsure, record it anyway — an empty rationale is more useful than a missing record.

**Prompt trigger:** "After making an architectural decision" is too late. The trigger should be: **"When about to choose between two approaches, or when just chose one."**

### Example

```typescript
record_decision({
  context: "The MPM plugin needed a way to persist widget CSS without WordPress stripping style blocks from post content",
  choice: "Route all widget CSS through agentshell_register_widget → stored in wp_options → injected in <head> by widgets.php, not post content",
  rationale: "WordPress ksips <style> blocks from post content via wp_kses_post() even for admin users. The widget init JS also needs a footer injection point. Both requirements pointed to wp_options as the store and a dedicated loader as the mechanism.",
  tags: ["agent-shell", "wordpress", "widget-injection", "architecture"],
  weight: 0.8
})
```

---

## Phase 1B: Theory Tracker

### Collection
`theories` — stored in `memories` table, `collection = "theories"`

### Content Format

```
HYPOTHESIS: <what 808 thinks is true>
VALIDATION_CRITERIA: <specific test, observation, or check that would prove or disprove it>
STATUS: pending | proven | disproven
CONCLUSION: <optional — what was found when tested>
```

### TypeScript Tools

#### `propose_theory()`

**Arguments:**
- `hypothesis` (string, required) — what 808 thinks is true
- `validationCriteria` (string, required) — what specific evidence would prove or disprove it
- `tags` (string[], optional)

**Execution:** Formats → `mpm add --collection theories --json "<structured content>" [--tag <t>]`

**When to call:** When 808 notices a pattern, makes an assumption, or has a gut feeling about causality. "I think passing `--json` before the positional query is causing the parsing bug" is a theory. Log it before writing the fix — the act of writing the criteria often reveals whether the assumption is solid.

#### `resolve_theory()`

**Arguments:**
- `theoryId` (string, required) — the MPM memory ID of the theory to resolve
- `conclusion` (string, required) — what was found when the hypothesis was tested
- `newStatus` (string, required) — `proven` or `disproven`

**Execution:** Formats → `mpm add --collection theories --json "<resolved content with STATUS=proven/disproven>" [--tag resolved]`

**When to call:** After running the test described in `validationCriteria`, or after any empirical check that confirms or refutes a pending theory. Proven theories should also be saved as regular memories with weight=1.0 and a note linking back to the theory ID.

**Note:** `resolve_theory` updates the existing record, not creates a new one. The Go backend needs an `UpdateMemory()` method or we use the soft-delete + re-insert pattern. Check if `UpdateMemory()` exists.

---

## Go Backend Changes (Minimal)

### 1. New query shortcuts for recall convenience

Add to `simple_cmds.go` or a new `epistemology_cmds.go`:

```bash
mpm decisions          # shorthand: mpm recall --collection decisions
mpm theories           # shorthand: mpm recall --collection theories
mpm theories pending   # filter to pending theories
mpm theories resolved  # filter to proven/disproven
```

### 2. Check if UpdateMemory() exists

Needed for `resolve_theory()` to update STATUS in-place rather than insert a new record.

```bash
grep -n "UpdateMemory\|updateMemory" /home/v/workspace/projects/mpm/internal/db.go
```

If it doesn't exist, implement a lightweight version that sets `metadata = JSON_MERGE(metadata, '{"status":"proven","conclusion":"..."}')` — SQLite JSON patch without a full re-index.

### 3. Optional: new `--quiet` flag for recall

Suppresses the rationalization/explanation text from output, returns raw memory list for parsing by tool layer.

---

## TypeScript Changes

### 3 new tools in `openclaw/mpm-plugin/src/index.ts`

Each follows the existing pattern of `runMpm(args)` → `parseMpmResult()`.

**Tool registration order:** after existing `save_to_memory`, before directive tools.

```typescript
// Decision tool
api.registerTool((ctx) => makeRecordDecisionTool(ctx),
  { names: ["record_decision"], optional: false });

// Theory tools
api.registerTool((ctx) => makeProposeTheoryTool(ctx),
  { names: ["propose_theory"], optional: false });
api.registerTool((ctx) => makeResolveTheoryTool(ctx),
  { names: ["resolve_theory"], optional: false });
```

Each tool needs:
- JSDoc description with trigger conditions (when to call, not just how)
- Zod/input schema
- `execute()` that formats structured content and calls `mpm add --collection <collection>`

---

## Exact Tool Descriptions & Schemas

These are the exact `description` strings and input schemas to use in `index.ts`. The description is what 808 reads to decide *when* to call the tool — not just *what* the tool does.

---

### Tool 1: `record_decision()`

**Schema:**
```typescript
const RECORD_DECISION_SCHEMA = {
  type: "object",
  properties: {
    context: {
      type: "string",
      description:
        "The situation or problem that forced a choice between competing options.",
    },
    choice: {
      type: "string",
      description:
        "What was decided — the specific path, approach, or action taken.",
    },
    rationale: {
      type: "string",
      description:
        "Why this choice won over the alternatives. What evidence or reasoning made it the right call.",
    },
    outcome: {
      type: "string",
      description:
        "Optional: what actually happened when this decision was executed.",
    },
    tags: {
      type: "array",
      items: { type: "string" },
      description: "Optional tags for retrieval.",
      default: [],
    },
    weight: {
      type: "number",
      description: "Importance weight 0–1 (default 0.5). Use 0.8+ for architectural decisions.",
      default: 0.5,
    },
  },
  required: ["context", "choice", "rationale"],
  additionalProperties: false,
} as const;
```

**Tool `description` field (the trigger text 808 reads):**
```typescript
description:
  "Record an architectural decision, library choice, or any moment where you chose " +
  "path A over path B. Call this BEFORE or AFTER the decision — not just after. " +
  "The rationale is the most important field: it is what makes past decisions " +
  "reusable when you encounter a similar problem weeks later. " +
  "Trigger: whenever you weigh tradeoffs and choose one, or whenever you notice " +
  "you just picked an approach without recording why.",
```

---

### Tool 2: `propose_theory()`


**Schema:**
```typescript
const PROPOSE_THEORY_SCHEMA = {
  type: "object",
  properties: {
    hypothesis: {
      type: "string",
      description:
        "What you think is true — a causal assumption, a noticed pattern, or a gut feeling about why something is broken.",
    },
    validationCriteria: {
      type: "string",
      description:
        "A specific, executable test or observation that would prove or disprove the hypothesis. " +
        "Be concrete: 'run the benchmark with --json flag before positional arg and compare parse time' " +
        "— not 'test it somehow'.",
    },
    tags: {
      type: "array",
      items: { type: "string" },
      description: "Optional tags.",
      default: [],
    },
  },
  required: ["hypothesis", "validationCriteria"],
  additionalProperties: false,
} as const;
```

**Tool `description` field:**
```typescript
description:
  "Log a hypothesis about causality before writing a fix. " +
  "When you think 'X is probably causing Y' — propose it, define the test, then run the test. " +
  "Writing the validation criteria forces you to confront whether the assumption is actually testable, " +
  "and often collapses a false hypothesis before it wastes an hour of debugging time. " +
  "Trigger: whenever you form a 'I think X is causing Y' assumption during debugging or design work.",
```

---

### Tool 3: `resolve_theory()`

**Schema:**
```typescript
const RESOLVE_THEORY_SCHEMA = {
  type: "object",
  properties: {
    theoryId: {
      type: "string",
      description: "The MPM memory ID of the theory to resolve (from propose_theory output).",
    },
    conclusion: {
      type: "string",
      description: "What the test or observation actually found. Be specific about the result.",
    },
    newStatus: {
      type: "string",
      enum: ["proven", "disproven"],
      description: "proven if the hypothesis was confirmed; disproven if it was not.",
    },
  },
  required: ["theoryId", "conclusion", "newStatus"],
  additionalProperties: false,
} as const;
```

**Tool `description` field:**
```typescript
description:
  "Close the loop on a pending theory after running its validation criteria. " +
  "If the hypothesis was confirmed, save the confirmed result as a permanent memory " +
  "(weight=1.0, include the theory_id as a tag for traceability). " +
  "If it was disproven, record what actually caused the problem instead. " +
  "Trigger: immediately after executing the test described in a pending theory's validationCriteria.",
```

---

## Exact System Prompt Additions

These additions go into `AGENTS.md` alongside the existing memory rules. They are the *when*, not just the *how*.

### Decision rule (add to Memory section in AGENTS.md)

```markdown
## Decision Recall & Recording

**Before answering questions about past architectural choices:**
Always query the decisions collection first. Decisions are the audit trail of reasoning.

**When you choose between two approaches:**
Call `record_decision()` before executing. Record context, the choice made, and the rationale.
The act of writing the rationale often reveals whether the choice is actually sound.

Example trigger: "I'm going to use a separate FTS index per collection"
→ record_decision({ context, choice, rationale, tags })
```

### Theory rule (add to Memory section in AGENTS.md)

```markdown
## Theory Lifecycle

**When debugging or designing and you form a hypothesis about causality:**
Call `propose_theory()` before writing the fix. Define the specific validation criteria.
If you can't write a concrete criteria, the hypothesis is not well-formed.

**After running the test described in a pending theory:**
Call `resolve_theory({ theoryId, conclusion, newStatus })` immediately.

**Proven theory → permanent memory:**
After resolving as `proven`, call `save_to_memory` with weight=1.0 and a tag `theories:<theory_id>`
for traceability back to the original hypothesis.
```

---

## TypeScript Execution Template

Each tool formats its structured arguments into a single text block and calls `mpm add --collection <collection>`. Example for `record_decision`:

```typescript
// Format the structured content
const content = [
  `CONTEXT: ${context}`,
  `CHOICE: ${choice}`,
  `RATIONALE: ${rationale}`,
  outcome ? `OUTCOME: ${outcome}` : ``,
]
  .filter(line => line.length > `CONTEXT: `.length)
  .join("\n");

const args = [
  "add", "--json", "--collection", "decisions",
  "--", content,
];
for (const tag of tags) { args.push("--tag", tag); }
if (weight !== 0.5) { args.push("--weight", String(weight)); }

const result = await runMpm(args);
```

`propose_theory` follows the same pattern with `--collection theories` and content formatted as:
```
HYPOTHESIS: <hypothesis>
VALIDATION_CRITERIA: <validationCriteria>
STATUS: pending
```

`resolve_theory` requires in-place update of the existing record's `metadata`. This needs a new Go function `UpdateMemoryMetadata(id, patch)` — see Open Questions.

---

## Open Questions

1. **resolve_theory() update strategy:** Implement `UpdateMemoryMetadata(id, patch)` in Go. Reads existing record, JSON-merges `patch` into `metadata` JSON field, writes back. Does NOT touch `content` so FTS index stays consistent. No re-index needed.

2. **Theory provenance link:** Proven theories: resulting memory gets tag `theories:<theory_id>` for traceability.

3. **Decisions deduplication:** `content_hash` column already exists. On insert to `decisions`, check for existing hash — skip if duplicate. Prevents re-recording identical decisions.

4. **Evidence (Phase 2):** Deferred. UX concern — certainty display needs deliberate design.

---

## Files to Modify

| File | Change |
|------|--------|
| `openclaw/mpm-plugin/src/index.ts` | Add 3 tools: schemas, descriptions, execute(), registration |
| `cmd/mpm/simple_cmds.go` | Add `decisions` / `theories` CLI shorthand commands |
| `internal/db.go` | Add `UpdateMemoryMetadata()` for resolve_theory in-place updates |
| `AGENTS.md` | Add Decision + Theory trigger rules (exact text above) |
| `MEMORY.md` | Add this spec as an architectural record |