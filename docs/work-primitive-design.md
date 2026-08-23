# Work Primitive Design: MPM

**Date:** 2026-08-23  
**Status:** Design — ready for implementation  
**Author:** Claude Code investigation, revised per design review

---

## Executive Conclusion

**Yes — MPM should add a first-class `Work` primitive.**

The existing substrate has a genuine semantic gap: it persists *what the agent knows* but has no durable representation for *what the agent intends to do next*.

The failure mode is concrete:

> Agent: "Next I need to investigate why telemetry admission isn't firing."

That statement disappears with the session. It isn't a memory. It isn't a lesson. It isn't a theory. It isn't a handoff. It is an *intention* — and MPM has nowhere to put it.

**The design principle:**

> Work is a durable representation of an intended action that survives the session in which the intention was formed.

Not a task manager. Not a project management layer. Not a duplicate memory system. A cognitive primitive.

---

## The Broader Vocabulary

There is a surprisingly coherent progression emerging across MPM's artifacts:

```
Reference  → "What does the external world say?"
Memory     → "What do I know?"
Lesson     → "What have I learned?"
Theory     → "What do I believe might be true?"
Work       → "What do I intend to do?"
Wake       → "When should I reconsider something?"
Scratchpad → "What am I thinking right now?"
```

This is a small vocabulary for persistent agent cognition. Work is the answer to the last missing question.

The moment Work becomes "project management" it fails. The moment it inherits task-manager concepts — boards, assignees, deadlines, priorities, subtasks, Gantt — it has failed. The design resists this by keeping the surface area minimal and deferring everything that can be deferred.

---

## Semantic Model

### What Work Is

- A **persistent intended action** — something the agent has explicitly committed to doing
- **Cross-session durable** — survives session boundaries
- **State-machine governed** — explicit terminal states
- **Agent-declared** — created via explicit `mpm_work create`, never auto-extracted from prose
- **Projection-capable** — appears in wake context as bounded summaries

### What Work Is Not

| Concept | Work? | Why |
|---------|--------|-----|
| Memory | No | Memory describes the world; Work describes intended change |
| Lesson | No | Lesson encodes learned guidance; Work encodes future intention |
| Theory | No | Theory is a hypothesis; Work is a commitment to act |
| Scratchpad | No | Scratchpad is ephemeral; Work persists |
| Scheduled wake | No | Wake fires once and is done; Work persists with state |
| Session handoff | No | Handoff describes the prior session; Work describes future action |
| Reference | No | Reference is external evidence; Work is internal commitment |

### Lifetime Taxonomy

This creates a clean distinction:

| Primitive | Lifetime |
|-----------|----------|
| Scratchpad | ephemeral / TTL |
| Memory | durable, subject to cognitive decay |
| Lesson | durable learned guidance |
| Reference | persistent external evidence |
| Work | durable until explicitly closed |
| Wake | time-triggered callback |

---

## Minimal Data Model

### Schema

```sql
CREATE TABLE IF NOT EXISTS works (
    id            TEXT PRIMARY KEY,
    title        TEXT NOT NULL,
    content      TEXT NOT NULL DEFAULT '',
    status       TEXT NOT NULL DEFAULT 'open'
                 CHECK (status IN ('open', 'done', 'cancelled')),
    created_at   INTEGER NOT NULL DEFAULT (CAST(strftime('%s','now') AS INTEGER)),
    updated_at   INTEGER NOT NULL DEFAULT (CAST(strftime('%s','now') AS INTEGER)),
    completed_at INTEGER,                      -- NULL when not done
    session_id   TEXT                          -- FK to sessions.id; who created it
);

CREATE INDEX IF NOT EXISTS idx_works_status ON works(status);
CREATE INDEX IF NOT EXISTS idx_works_session ON works(session_id);
CREATE INDEX IF NOT EXISTS idx_works_created ON works(created_at DESC);
```

### Go Types

```go
type Work struct {
    ID          string     `json:"id"`
    Title       string     `json:"title"`
    Content     string     `json:"content,omitempty"`  // detailed description; empty = same as title
    Status      WorkStatus `json:"status"`
    CreatedAt   int64      `json:"created_at"`
    UpdatedAt   int64      `json:"updated_at"`
    CompletedAt *int64     `json:"completed_at,omitempty"`  // NULL if not done
    SessionID   string     `json:"session_id,omitempty"`
}

type WorkStatus string
const (
    WorkStatusOpen      WorkStatus = "open"
    WorkStatusDone     WorkStatus = "done"
    WorkStatusCancelled WorkStatus = "cancelled"
)
```

### Wake Context Projection

```go
type WakeContextWork struct {
    ID        string     `json:"id"`
    Title     string     `json:"title"`   // truncated to 120 chars
    Status    WorkStatus `json:"status"`
    Pointer   string     `json:"pointer"` // "mpm://work/<id>"
    CreatedAt int64      `json:"created_at"`
}
```

---

## State Machine

### States

| State | Meaning |
|-------|---------|
| `open` | Created, not yet done. Default. |
| `done` | Completed. Terminal. |
| `cancelled` | Abandoned. Terminal. |

### Transitions

```
open ──────────► done
  │
  │
  ▼
cancelled
```

### Transition Matrix

| From | To | Legal? |
|------|----|--------|
| open | done | Yes |
| open | cancelled | Yes |
| done | open | Yes (re-open) |
| cancelled | open | Yes (re-activate) |

`completed_at` is set to `NOW` when transitioning to `done`; cleared to `NULL` on re-open.

**Why no `in_progress`?**

`in_progress` sounds useful but risks recording every micro-fluctuation in agent attention. An agent touching a work item doesn't mean it's meaningfully in progress — it might inspect it, decide it's wrong, come back tomorrow, or forget it entirely.

The distinction worth preserving: `open` vs `done/cancelled`. Was this intention completed or not? The session activity layer handles the rest.

If empirical use demonstrates that `in_progress` carries genuinely useful signal — measurable via telemetry, not speculation — it can be added in a later phase without breaking the v1 schema.

---

## Tool Surface

### `mpm_work` Tool

```json
{
  "type": "object",
  "properties": {
    "action": {
      "type": "string",
      "enum": ["create", "list", "show", "update", "complete", "cancel"]
    },
    "params": {
      "type": "object",
      "properties": {
        "title":    { "type": "string" },   // required for create
        "content":  { "type": "string" },   // optional, defaults to ""
        "status":   { "type": "string", "enum": ["open","done","cancelled"] },
        "work_id":  { "type": "string" }    // required for show/update/complete/cancel
      }
    }
  },
  "required": ["action"]
}
```

### Action Semantics

| Action | Required Params | Returns |
|--------|----------------|---------|
| `create` | `title` | created Work |
| `list` | — | `[]Work` (open items only, ordered by created_at DESC) |
| `show` | `work_id` | Work |
| `update` | `work_id`, `status` | updated Work |
| `complete` | `work_id` | Work with `status=done` |
| `cancel` | `work_id` | Work with `status=cancelled` |

### Usability Principle

Work must be **extremely cheap to create**. The failure mode being fixed is:

> Agent: "Next steps: 1. investigate telemetry, 2. test admission, 3. review FTS"  
> → session ends  
> → those statements disappear

If creating Work requires a verbose JSON payload with metadata, priority, and relationships, the agent will simply stop creating Work. The friction of recording intention must be lower than the friction of losing it.

Minimum viable create:
```
mpm_work create title="Investigate telemetry admission"
```

Everything else is optional.

### Explicit Non-Operations

These are explicitly excluded from v1:

- `reopen` — achieved via `update` with `status="open"`
- `delete` — work items are not deleted, only cancelled (audit trail)
- `search` — v1 has no FTS on work; `list` returns all open items
- `priority` — ordering is by recency only; explicit priority can be added if telemetry demonstrates need

---

## Pointer Architecture

### `mpm://work/<id>`

**Yes — work gets a pointer kind.**

When open work appears in wake context, the agent needs a deterministic way to resolve the full item when it decides to act on it. Pointer resolution is the established pattern from Phase 2.

### Registration Points

**`cmd/mpm-mcp/tools.go`** — add `case "work"` to `artifactResolverAdapter.Resolve()`:
```go
case "work":
    return a.resolveWork(ctx, p, opts)
```

**`internal/core/tools/handlers.go`** — add `"work"` to kind validation in `handleMpmResolve`.

**`resolveWork`** — uses `dm.GetWork(id)`, bounded to 512 bytes default, records retrieval telemetry.

### Projection Mode

For `mpm_work list` with `projection=true`:
```go
type ProjectedWorkEntry struct {
    ID        string     `json:"id"`
    Title     string     `json:"title"`    // truncated to 256 chars
    Status    WorkStatus `json:"status"`
    Pointer   string     `json:"pointer"` // "mpm://work/<id>"
    CreatedAt int64      `json:"created_at"`
}
```

---

## Wake Context Integration

### Where Work Appears

In `WakeContextData`, add to the "Attention & Pending Work" section:

```go
type WakeContextData struct {
    // ... existing fields ...
    OpenWorks []WakeContextWork `json:"open_works"`  // status='open' only
}
```

### Query

```sql
SELECT id, title, status, created_at
FROM works
WHERE status = 'open'
ORDER BY created_at DESC
LIMIT 5
```

### Bounded Representation

```go
type WakeContextWork struct {
    ID        string     `json:"id"`
    Title     string     `json:"title"`    // truncated to 120 chars
    Status    WorkStatus `json:"status"`
    Pointer   string     `json:"pointer"` // "mpm://work/<id>"
    CreatedAt int64      `json:"created_at"`
}
```

### No Silent Disappearance

Work items persist until explicitly closed. There is no TTL, no decay, no garbage collection. The only lifecycle end is explicit `complete` or `cancel`.

This creates a sharp semantic distinction from scratchpads (ephemeral) and memories (subject to cognitive decay).

---

## Telemetry

### Lifecycle Events

Add to `system_audit_log`:

| Event | When |
|-------|------|
| `work_created` | Work item inserted |
| `work_completed` | `open → done` |
| `work_cancelled` | `open → cancelled` |
| `work_reopened` | `done/cancelled → open` |

These are observational. `works.status` is the authoritative state.

### Retrieval Telemetry

Work items are added to `retrieval_metadata` when resolved via `mpm://work/<id>`. This enables future analytics:
- Which work items are retrieved most often
- Correlation between retrieval and completion
- Time-to-completion measurements

---

## Testing Strategy

### Canonical Test: Cross-Session Persistence

This is the acceptance gate — not CRUD. CRUD proves SQLite works; cross-session persistence proves MPM's cognitive model works.

```go
func TestWork_CrossSessionPersistence(t *testing.T) {
    // Session A
    sessionA := startSession(t, dm)
    work := createWork(t, dm, "Investigate telemetry admission")
    assertEqual(t, work.Status, "open")
    endSession(t, dm, sessionA)

    // Session B
    sessionB := startSession(t, dm)

    // Wake context contains bounded work
    ctx := GatherWakeContext(t, dm)
    assertContainsWork(t, ctx.OpenWorks, work.ID)
    assertEqual(t, ctx.OpenWorks[0].Status, "open")

    // Agent resolves pointer
    resolved := resolveWork(t, dm, "mpm://work/"+work.ID)
    assertEqual(t, resolved.Title, "Investigate telemetry admission")

    // Agent completes
    completed := completeWork(t, dm, work.ID)
    assertEqual(t, completed.Status, "done")
    assertNotNil(t, completed.CompletedAt)
}
```

### No Silent Disappearance Test

```go
func TestWork_NoSilentDisappearance(t *testing.T) {
    work := createWork(t, dm, "Investigate telemetry admission")

    // Simulate time passing
    time.Sleep(24 * time.Hour)

    // New session
    sessionB := startSession(t, dm)

    // Work still exists
    retrieved := getWork(t, dm, work.ID)
    assertEqual(t, retrieved.Status, "open")

    // Not in scratchpad orphans or anywhere else
    // It is simply where it was put
}
```

### State Transition Tests

```go
func TestWorkStateTransitions(t *testing.T) {
    transitions := []struct {
        from, to   WorkStatus
        shouldSucceed bool
    }{
        {"open", "done", true},
        {"open", "cancelled", true},
        {"done", "open", true},       // re-open
        {"cancelled", "open", true},  // re-activate
        {"done", "cancelled", false},  // illegal
        {"cancelled", "done", false}, // illegal
    }
    // ... test each
}
```

### Pointer Resolution Test

```go
func TestMpmResolve_Work(t *testing.T) {
    work := createWork(t, dm, "Investigate telemetry")
    result := resolvePointer(t, dm, "mpm://work/"+work.ID)
    assertEqual(t, result.Title, "Investigate telemetry")
    assertTrue(t, result.Bounded)  // exceeds 512 bytes only if content is long
}
```

---

## Migration/Compatibility

### Schema Migration

Add via `SafeMigrations` in `schema.go`. No existing tables modified. No data migration.

### Backward Compatibility

- Existing tools unchanged
- Existing pointer kinds unchanged
- `WakeContextData` wire format unchanged — `open_works` is a pure addition
- `mpm_resolve` continues to work for existing kinds

### Backup/Restore

`mpm backup-db` / `mpm restore-db` automatically include `works` via full SQL dump. No changes needed.

---

## Deferred Decisions

These are explicitly not in v1. They can be added based on empirical use:

### 1. `in_progress` status

Can be added if telemetry shows agents genuinely need to distinguish "I'm looking at this" from "I'm done with this". Not added preemptively.

### 2. Explicit priority field

Ordering in v1 is recency only. If agents consistently create work and then need to signal urgency, explicit priority can be added. The telemetry (retrieval count, completion rate, time-to-close) is a better signal than self-reported priority anyway.

### 3. `related_memory_id` relationship

The conceptual link "Work informed by Memory" is clearly useful. But it shouldn't be baked into v1 before the relationship model is demonstrated. Pointer architecture handles cross-artifact references at the tool layer. If work-to-memory linking becomes a pattern, it gets added.

### 4. FTS search on work

v1 `list` returns all open work items ordered by recency. If agents need to search work content, FTS5 on `works` can be added. Not needed for the core hypothesis.

### 5. Work → memory/lesson on completion

When completing work produces durable knowledge, the agent can create a memory or lesson explicitly. Auto-generating knowledge from work completion is the kind of hidden workflow the design explicitly avoids. Let telemetry tell you whether this is needed.

---

## Implementation Phases

### Phase 1: Core Work Primitive

**Objective:** Minimal viable work item — create, list, show, update, complete/cancel, pointer resolution, wake-context projection, cross-session persistence.

**Files:**

| File | Change |
|------|--------|
| `internal/core/schema.go` | Add `works` table + indexes |
| `internal/core/work.go` | New file: `Work` struct, `WorkStatus` type, constants |
| `internal/core/db.go` | `AddWork`, `GetWork`, `ListWorks`, `UpdateWork`, `CompleteWork`, `CancelWork` |
| `internal/core/summarize.go` | `SummarizeWork` (truncate title to 256 chars) |
| `internal/core/wake_context.go` | `OpenWorks []WakeContextWork` in `WakeContextData`; `gatherOpenWorks()` |
| `internal/core/tools/handlers.go` | `handleMpmWork` |
| `internal/core/tools/registry_list.go` | `mpm_work` tool entry |
| `cmd/mpm-mcp/tools.go` | `resolveWork` in `artifactResolverAdapter` |

**Schema:** one table, three indexes.

**Acceptance criteria:**
- `mpm_work create title="X"` → work with `status=open`
- `mpm_work list` → all open works, ordered by created_at DESC
- `mpm_work complete work_id=<id>` → `status=done`, `completed_at=NOW`
- `mpm_work cancel work_id=<id>` → `status=cancelled`
- `mpm_work update work_id=<id> status=open` → re-open works
- `mpm://work/<id>` resolves via `mpm_resolve`
- Wake context shows up to 5 open works
- Cross-session continuity test passes
- No silent disappearance test passes

**Compatibility:** Fully additive. No existing behavior changes.

---

### Phase 2: Provenance + Verification

**Date:** 2026-08-23  
**Trigger:** Aug 23 incident — Claude Code claimed documentation updated, but README unchanged and changelog uncommitted. The session ended normally; the failure was invisible until forensic replay.

**Core insight:** Agents can be confidently wrong about completion. "done" as currently recorded means "the agent declared this complete" — not "the evidence supports that declaration". These are categorically different facts.

**Design principle:** Work items distinguish three layers that must never be conflated:

1. **Agent assertion** — what the agent claims happened
2. **Observed evidence** — git status, file state at action time
3. **Persisted state** — what actually committed

**Schema changes:**

`works` table gains:
```sql
verification TEXT CHECK (verification IN ('unverified','verified','partial','contradicted'))
```

`work_events` table gains evidence fields:
```sql
git_head_before  TEXT,
git_head_after   TEXT,
dirty_before     INTEGER,   -- 0 or 1
dirty_after      INTEGER,   -- 0 or 1
changed_files     TEXT,      -- JSON array of file paths
committed        INTEGER    -- 0 or 1
```

**Verification status transitions:**

| Status | Meaning |
|--------|--------|
| `unverified` | Default on `claimed_complete` event. No evidence collected yet. |
| `verified` | Evidence confirms all expected conditions met. |
| `partial` | Some evidence observed but incomplete (e.g. changelog modified, README unchanged). |
| `contradicted` | Evidence directly contradicts claim (e.g. agent claims done, git shows no changes). |

**Event flow for a completion claim:**

```
work_claimed_complete
  actor: {model, session, invocation}
  evidence: {
    git_head_before: "abc123",
    git_head_after:  "def456",    -- or same if nothing committed
    dirty_before:    true,
    dirty_after:     false,        -- true if uncommitted changes remain
    changed_files:   ["README.md", "changelog.md"],
    committed:      false         -- did the git head move?
  }

→ verification = 'partial'  (if committed=false but changed_files non-empty)
→ verification = 'verified' (if committed=true and all expected files in changed_files)
→ verification = 'contradicted' (if committed=false and changed_files empty)
```

**Why separate `claimed_complete` from `completed`:**

`completed` in Phase 1 is the agent assertion. Phase 2 introduces `claimed_complete` as the primary completion event — capturing what the agent believed it accomplished at the moment of declaration. The `verification` field is computed from the evidence, not assigned by the agent.

This means a Work item can be:
- `status=done, verification=unverified` — agent declared complete, no evidence collected yet
- `status=done, verification=partial` — evidence shows incomplete persistence (Aug 23 case)
- `status=done, verification=verified` — evidence confirms completion
- `status=done, verification=contradicted` — agent claimed done, evidence shows nothing happened

**Prompt hash by default:** Do not store full prompts. Store `prompt_hash` (SHA-256), `prompt_length`, and optionally `prompt_id` from provenance system. This prevents MPM from becoming an accidental transcript warehouse while retaining enough provenance to answer "was this the same instruction?"

**Non-Goals (Phase 2):**
- Automatic verification execution (Phase 3)
- Verification via external CI/build checks
- Storing full prompts or conversation context

---

## Explicit Non-Goals

These are not in scope for Work, now or (in most cases) ever:

| Excluded | Reason |
|----------|--------|
| Kanban / boards | Visual management. Not a cognitive primitive. |
| Gantt / calendar | Scheduling. `scheduled_wakes` handles time-based callbacks. |
| Assignees / teams | No multi-agent model in scope. |
| Permissions / access control | Out of scope for single-agent substrate. |
| Recurring tasks | One-shot by design. `scheduled_wakes` for recurring callbacks. |
| Projects / grouping | Work items exist independently. |
| Automatic extraction from prose | Explicit `mpm_work create` only. |
| Separate task database | Lives in the same SQLite. |
| Priority field | Ordered by recency in v1. |
| Due dates | Optional `due_at` deferred. |
| Time tracking | Computable from `created_at`, `updated_at`, `completed_at`. |
| Subtasks / dependencies | Generic graph relationships deferred. |
| Work → memory/lesson on completion | Agent creates knowledge explicitly if needed. |
| `in_progress` status | Derived from session activity, not explicit state. |

---

## Files Referenced

| File | Purpose |
|------|---------|
| `internal/core/schema.go` | Table definitions, migrations |
| `internal/core/db.go` | DatabaseManager, ID generation, write/read patterns |
| `internal/core/work.go` | **New** — Work struct, status constants |
| `internal/core/wake_context.go` | WakeContextData, bounded projection, size enforcement |
| `internal/core/tools/handlers.go` | Handler dispatch, multi-action tools |
| `internal/core/tools/registry_list.go` | Tool registry |
| `internal/pointer/pointer.go` | URI grammar |
| `cmd/mpm-mcp/tools.go` | MCP adapter, artifact resolver adapter |
| `internal/core/summarize.go` | SummarizeBounded |
| `internal/core/handoff.go` | Handoff struct and lifecycle |
