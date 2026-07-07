# Arc 2: Active Dissemination — Draft Design

**Status:** Draft for v's review. No code committed yet.  
**Locked parameters** (from prior conversation):
1. Topology: heartbeat-discovered via `shared.sessions`
2. Dedup: deterministic event-wake IDs (`sha256(memory_id + target_session_id + content_hash)[:12]`)
3. Payload: must include rationale
4. Command: `mpm ops broadcast <memory_id> [--kind=rule|resolution] [--rationale="..."] [--to=agent_id,...]`

---

## 0. The Question This Arc Answers

Arc 1 (Conflict Resolution) made the shared DB *write* and *self-heal*. But there's a gap: if agent A resolves a contradiction and produces a new resolution memory, how does agent B learn about it on the next boot? Right now: by re-running the federated search and hoping the resolution scores high enough to surface. That's passive, lossy, and slow.

Arc 2's answer: **active push.** When an epistemic shift happens, fan out deterministic event wakes to every active session. Receiving agents see the new rule/resolution in their wake context the next time they call any MPM tool — no search required, no cold-start cost.

---

## 1. The Three New Tables (all in `shared.`)

### 1.1 `shared.sessions` — active agent registry (heartbeat-discovered)

Replaces nothing. There's an existing local `sessions` table but it stores *handoff content* (the text bodies), not "this agent is alive right now." We need a separate, shared registry.

```sql
CREATE TABLE IF NOT EXISTS shared.sessions (
    session_id      TEXT PRIMARY KEY,          -- the agent's session UUID (matches local sessions.id)
    agent_id        TEXT NOT NULL,             -- who this is ("808", "alice-on-vm", etc)
    hostname        TEXT,                       -- for human debugging
    last_heartbeat  DATETIME NOT NULL DEFAULT CURRENT_TIMESTAMP,
    boot_at         DATETIME NOT NULL DEFAULT CURRENT_TIMESTAMP,
    metadata        TEXT                        -- JSON: model, mode, capabilities, etc
);

CREATE INDEX shared.idx_sessions_heartbeat
    ON shared.sessions (last_heartbeat);

-- Sweep-on-write: when a heartbeat lands and the agent hasn't checked in
-- for >24h, the next broadcast treats it as inactive. We don't auto-delete
-- rows; audit wants the trail.
```

**Heartbeat mechanism.** Two ways to write to `shared.sessions`:
- **Passive (default):** every MPM tool call that hits the shared DB silently `INSERT OR REPLACE INTO shared.sessions ... last_heartbeat=NOW()`. No new daemon, no operator action. The "heartbeat" is just "this agent called something that touched shared."
- **Active (optional):** `mpm ops heartbeat [--agent-id=...] [--metadata=...]` — explicit ping for tooling that doesn't run an agent loop. Useful for cron jobs, batch processors, and external integrations.

**Discovery query:**
```sql
SELECT session_id, agent_id, hostname, last_heartbeat, metadata
FROM shared.sessions
WHERE last_heartbeat > datetime('now', '-24 hours')
ORDER BY last_heartbeat DESC;
```

### 1.2 `shared.event_wakes` — the fan-out surface

```sql
CREATE TABLE IF NOT EXISTS shared.event_wakes (
    wake_id         TEXT PRIMARY KEY,           -- sha256(memory_id + target_session_id + content_hash)[:12]
    target_session  TEXT NOT NULL,              -- who the wake is for
    source_agent    TEXT NOT NULL,              -- who broadcast it
    memory_id       TEXT NOT NULL,              -- what was broadcast
    kind            TEXT NOT NULL,              -- 'rule' | 'resolution'
    content_hash    TEXT NOT NULL,              -- SHA-256 of memory.content + memory.tags at broadcast time
    rationale       TEXT,                        -- the "why" (v's call 3 — non-negotiable)
    fired           INTEGER NOT NULL DEFAULT 0, -- 0 = pending, 1 = already picked up by target
    fired_at        DATETIME,
    created_at      DATETIME NOT NULL DEFAULT CURRENT_TIMESTAMP,
    metadata        TEXT,                        -- JSON: queue_id, contradiction_id, etc
    FOREIGN KEY (target_session) REFERENCES shared.sessions(session_id)
);

CREATE INDEX shared.idx_event_wakes_target
    ON shared.event_wakes (target_session, fired, created_at DESC);

CREATE INDEX shared.idx_event_wakes_source
    ON shared.event_wakes (source_agent, created_at DESC);
```

**Critical design choice:** `wake_id` is a PRIMARY KEY, not a UNIQUE index. So `INSERT OR IGNORE` on a duplicate ID silently dedupes — no error, no spam. This is the entire dedup mechanism: deterministic ID + PK = O(1) no-op for re-broadcasts.

### 1.3 `shared.agents` — agent profile cache (optional but cheap)

```sql
CREATE TABLE IF NOT EXISTS shared.agents (
    agent_id        TEXT PRIMARY KEY,
    first_seen      DATETIME NOT NULL DEFAULT CURRENT_TIMESTAMP,
    last_seen       DATETIME NOT NULL DEFAULT CURRENT_TIMESTAMP,
    total_heartbeats INTEGER NOT NULL DEFAULT 0,
    metadata        TEXT                        -- JSON: canonical metadata across reboots
);
```

This is the *stable* identity for an agent. `shared.sessions` is "this run of this agent." `shared.agents` is "this agent across all its runs." Lets us answer questions like "how often does agent 808 broadcast vs. receive?" without aggregating session rows. Same `INSERT OR REPLACE` pattern.

**The passive heartbeat also bumps `shared.agents.last_seen` and increments `total_heartbeats`.**

---

## 2. The Broadcast Command

### 2.1 CLI surface (locked from v's call 4)

```bash
mpm ops broadcast <memory_id> \
    [--kind=rule|resolution]        # default: auto-detect from memory.collection
    [--rationale="..."]              # required if memory.kind != 'rule' (rules already have rationale)
    [--to=agent_id,...]              # restrict fan-out to specific agents (default: ALL active in last 24h)
    [--dry-run]                      # compute target list + payload, don't actually INSERT
    [--json]                         # machine-readable output
```

### 2.2 Operational logic

```go
func BroadcastMemory(ctx, memoryID string, opts BroadcastOpts) (BroadcastReport, error) {
    // 1. Load the memory from shared.memories. Refuse if it doesn't exist
    //    or is in the 'resolutions' / 'theories' collections UNLESS
    //    --kind=resolution is explicitly set.
    
    // 2. Determine kind if not explicit:
    //    - 'rule'         → memory.collection == 'global_rules'
    //    - 'resolution'   → memory.collection == 'resolutions'
    //    - 'arbitration'  → memory.collection == 'theories'
    //    - 'memory'       → default (a regular fact being pushed to peers)
    
    // 3. Determine rationale:
    //    - Explicit --rationale wins.
    //    - Else: if kind == 'resolution', pull from
    //      memory.metadata (which the Arc 1 path writes: conclusion,
    //      winner_id, loser_id, queue_id).
    //    - Else: required (refuse without rationale).
    
    // 4. Compute content_hash = sha256(memory.content + "|" + canonical(tags)).
    //    canonical(tags) = sorted, JSON-encoded array.
    //    Used as a tie-breaker input to the wake ID so that two
    //    different content versions of the same memory produce
    //    different wakes.
    
    // 5. Discover targets:
    //    a. If --to is set: use exactly those agent_ids (verify they
    //       have a row in shared.sessions in the last 24h; refuse if
    //       the agent is offline — silent skip would be worse than
    //       explicit refusal because it gives a false "broadcast
    //       succeeded" impression).
    //    b. Else: SELECT session_id, agent_id FROM shared.sessions
    //       WHERE last_heartbeat > now - 24h ORDER BY last_heartbeat DESC.
    //       That's the full active fleet.
    
    // 6. For each target, compute wake_id =
    //    sha256(memory_id + ":" + target_session + ":" + content_hash)[:12].
    //    INSERT OR IGNORE INTO shared.event_wakes with that wake_id.
    //    The OR IGNORE makes the operation idempotent across re-runs
    //    and across "two agents broadcast the same rule" races.
    
    // 7. Return BroadcastReport:
    //    {
    //      "memory_id":     "...",
    //      "kind":          "...",
    //      "rationale":     "...",
    //      "content_hash":  "...",
    //      "targets":       [{"session_id":..., "agent_id":..., "wake_id":..., "status":"new|deduped"}],
    //      "new_wakes":     N,
    //      "deduped_wakes": M,
    //    }
}
```

### 2.3 Why INSERT OR IGNORE, not a "should I broadcast?" pre-check

Pre-checking adds a SELECT round-trip per target and a TOCTOU race ("two broadcasts at once both see 'no row' and both INSERT"). The PK collision on `wake_id` IS the dedup mechanism. Database-level enforcement, no application-level state.

---

## 3. The Wake Fan-Out (Receiving Side)

### 3.1 The existing `CheckPendingWakes` is LOCAL-only

It reads `scheduled_wakes`, not `shared.event_wakes`. We need a sibling: `CheckPendingEventWakes(ctx, sessionID)`.

```go
// CheckPendingEventWakes returns every shared.event_wakes row where
// target_session = sessionID AND fired = 0, marks them fired = 1
// with fired_at = now (in a transaction), and returns them in
// chronological order.
//
// Idempotency: same atomicity contract as CheckPendingWakes —
// concurrent calls on different connections see disjoint subsets
// (SQLite's BEGIN IMMEDIATE).
func (dm *DatabaseManager) CheckPendingEventWakes(sessionID string) ([]EventWake, error) {
    tx, _ := dm.db.Begin()
    defer tx.Rollback()
    
    rows, _ := tx.Query(`
        SELECT wake_id, source_agent, memory_id, kind, content_hash,
               rationale, created_at, metadata
        FROM shared.event_wakes
        WHERE target_session = ? AND fired = 0
        ORDER BY created_at ASC
        LIMIT 100
    `, sessionID)
    defer rows.Close()
    
    var wakes []EventWake
    var wakeIDs []string
    for rows.Next() {
        var w EventWake
        rows.Scan(&w.WakeID, &w.SourceAgent, &w.MemoryID, &w.Kind,
                  &w.ContentHash, &w.Rationale, &w.CreatedAt, &w.Metadata)
        wakes = append(wakes, w)
        wakeIDs = append(wakeIDs, w.WakeID)
    }
    
    if len(wakeIDs) > 0 {
        // Build "UPDATE ... WHERE wake_id IN (?, ?, ...)"
        placeholders := strings.Repeat("?,", len(wakeIDs))
        placeholders = placeholders[:len(placeholders)-1]
        args := make([]interface{}, 0, len(wakeIDs)+1)
        for _, id := range wakeIDs {
            args = append(args, id)
        }
        _, err := tx.Exec(`
            UPDATE shared.event_wakes
            SET fired = 1, fired_at = CURRENT_TIMESTAMP
            WHERE wake_id IN (`+placeholders+`)
        `, args...)
        if err != nil { return nil, err }
    }
    
    tx.Commit()
    return wakes, nil
}
```

### 3.2 Hook into the existing wake surfacing path

The current `CheckPendingWakes` is called by the tool dispatcher and the result is folded into a `WakesPending` block on every MPM response. The new function piggybacks on the same hook:

```go
// In handlers.go dispatcher (pseudocode):
localWakes, _ := dm.CheckPendingWakes()
eventWakes, _ := dm.CheckPendingEventWakes(currentSessionID)

// Both go into the response's WakesPending block. Format:
type EventWake struct {
    WakeID        string  // the deterministic sha256 prefix
    SourceAgent   string  // who broadcast
    MemoryID      string  // what to fetch
    Kind          string  // rule|resolution|arbitration|memory
    ContentHash   string  // for the receiving agent to detect drift
    Rationale     string  // v's call 3 — the WHY
    CreatedAt     int64
    Metadata      string  // JSON
}
```

The receiving agent's prompt formatter turns this into:
```xml
<EventWakesPending count="3">
  <Wake kind="rule" from="808" memory_id="abc" content_hash="..." rationale="...">
    The pattern for X changed because we hit a contradiction with Y; this rule supersedes.
  </Wake>
  <Wake kind="resolution" from="808" memory_id="def" content_hash="..." rationale="...">
    mem-A wins over mem-B because [scoring]. mem-B's status=challenged; trust mem-A going forward.
  </Wake>
</EventWakesPending>
```

### 3.3 Cold-start path (the "first time this agent runs")

If a session has been offline for a long time, the 24h heartbeat window won't show it as active. Should it still see old wakes? **No** — by design. Arc 2's contract is "real-time, not history." If an agent wants to catch up on old resolutions, that's what `query_long_term_memory` is for. Wakes are a low-latency push channel for *current* events.

(The trade-off is real: a workstation that's been off for a week misses a week of resolutions. The counter-argument is that cold-start is the same cost as cold-starting against a fresh DB today; we're not making it worse, just making the steady-state better.)

---

## 4. The Payload

### 4.1 What goes into `rationale`

Two paths:
- **Explicit (`--rationale="..."`):** operator types the "why." Used for `kind=rule` where the operator is asserting a new pattern.
- **Auto-extracted (`kind=resolution`):** built from the resolution memory's metadata:
  ```
  "Contradiction (queue_id=N) resolved: mem-A survived against mem-B.
   Margin: X.X. Resolution: resolution-XXX. Reasoning: <operator conclusion text>."
  ```

### 4.2 What does NOT go into the payload

The full memory content. The receiving agent does its own `mpm query memory <memory_id>` if it needs the full text. The wake is a *signal*, not a *transport*. This keeps wake rows small (~1KB each) so a 100-target fan-out is ~100KB total, not 10MB.

If we ever need a "firehose" mode, that's a separate `--include-content` flag and a separate review.

### 4.3 Deterministic content hash — what counts

```go
func computeContentHash(memory Memory) string {
    h := sha256.New()
    h.Write([]byte(memory.Content))
    
    // Sort tags for determinism
    tags := append([]string(nil), memory.Tags...)
    sort.Strings(tags)
    tagJSON, _ := json.Marshal(tags)
    h.Write([]byte("|"))
    h.Write(tagJSON)
    
    return hex.EncodeToString(h.Sum(nil))
}
```

**Locked answer:** content + sorted tags. NOT metadata (metadata changes when resolution lands, but the rule body doesn't). NOT dependencies (dependencies are computed, not part of the "rule text").

If the rule body changes, content_hash changes, new wake fires. That's correct: it's a new event. If the rule is re-broadcast with the same body (e.g., a manual `mpm ops broadcast` to refresh peers), the content_hash is identical and the wake_id is identical and INSERT OR IGNORE drops it.

---

## 5. The Pass-Through Auto-Broadcast (the "wakes ARE the reason" hook)

Arc 1's resolution loop produces a `shared.memories` row of kind `resolution`. Arc 2 should auto-broadcast resolutions at the moment of resolution, with no operator action required.

In `applyResolution` and `applyArbitrationResolution`, after the resolution memory is written:
```go
// Auto-broadcast: every resolution is an event. The operator doesn't
// need to remember to push it.
_, _ = dm.BroadcastMemory(ctx, resolutionID, BroadcastOpts{
    Kind:      "resolution",
    Rationale: "",  // auto-extract from metadata
    ToAgents:  nil, // all active
})
```

The `BroadcastMemory` call is **non-fatal**: a failure here (e.g., shared DB locked, no active sessions) does NOT roll back the resolution. The resolution is the ground truth; the broadcast is a notification. Resolutions survive even if no one is listening.

The same hook applies to `mpm ops record-global-rule` (the operator is creating a new global rule → auto-broadcast). And to `ResolveArbitrationTheory` (the operator picked a winner → that resolution is an event).

**Counter-argument considered and rejected:** "what if an agent wants a rule but doesn't want to broadcast?" → fine, use a new `--no-broadcast` flag on the write command. Default behavior is broadcast; opt-out is explicit.

---

## 6. Test Plan

### 6.1 Unit tests (8 new)

| Test | Verifies |
|---|---|
| `TestSharedSessions_HeartbeatInserts` | first call inserts a row; second call within 24h is a no-op |
| `TestSharedSessions_HeartbeatUpdates` | second call within 24h bumps last_heartbeat |
| `TestDiscoverActiveSessions_FiltersExpired` | rows with last_heartbeat > 24h are excluded |
| `TestBroadcastMemory_DeterministicID` | broadcasting the same memory twice produces the same wake_id |
| `TestBroadcastMemory_DifferentContent` | broadcasting with new content produces a different wake_id (and thus new wake row) |
| `TestBroadcastMemory_TargetedFanout` | `--to=agent-A,agent-B` only inserts rows for those targets |
| `TestBroadcastMemory_RejectsOfflineTargets` | broadcasting to an agent with no recent heartbeat errors explicitly |
| `TestCheckPendingEventWakes_MarksFired` | calling twice returns disjoint sets (idempotent pick-up) |
| `TestBroadcastMemory_DryRun_NoInserts` | `--dry-run` populates the report but writes zero rows |

### 6.2 Smoke (scripts/smoke_arc2.sh, 6 steps)

1. Boot a "station A" — heartbeat into shared.sessions.
2. Boot a "station B" — same.
3. `mpm ops broadcast <rule_memory_id> --rationale="..."` — verify 2 rows in `shared.event_wakes`.
4. Re-broadcast same memory — verify 0 new rows (dedup).
5. `mpm ops broadcast` from station B with a *different* rule — verify a third wake, but no wake for station B (it doesn't broadcast to itself).
6. `mpm ops check-event-wakes` — verify station A sees the rule B broadcast.

### 6.3 What I'm NOT testing (yet)

- **Real agent fleet behavior.** The smoke is two simulated stations; running it against actual `808` instances is a Phase 3 thing.
- **Latency at scale.** 1k active sessions × 1 broadcast/sec → 1k INSERTs/sec. The PK on wake_id gives O(1) dedup. I'd bet on this holding but I don't have a benchmark.

---

## 7. Open Questions (Surfaced Before Code)

I have answers to most of these but want v's confirmation:

1. **Self-broadcast?** When station A broadcasts, do we skip station A's own session in the target list? **My lean: yes.** Reasoning: A already wrote the memory, doesn't need a wake to learn about its own write. (Save the wake row write.) — **confirm?**
2. **24h heartbeat window — too long?** The tradeoff is "24h means a workstation that's been on for 25h gets dropped from broadcasts." Should this be 12h? 48h? **My lean: 24h.** Reasoning: matches typical workstation uptime cycle; any longer and a dead-but-shared-DB-mounted station would spam other stations forever. — **confirm?**
3. **What if `shared.sessions` doesn't exist on first broadcast?** CREATE TABLE IF NOT EXISTS handles it. The schema patch runs at attachShared time. **Confirm: I add a new `SharedEventWakesDDL` constant and install it in attachShared like Arc 1's `SharedContradictionLogDDL`.** — **confirm?**
4. **Auto-broadcast from inside resolution transactions** — see §5. Is this safe? The transaction is already committing the resolution; the broadcast call would be outside the transaction. If broadcast fails, resolution still lands. If broadcast succeeds but the receiver's CheckPendingEventWakes is mid-flight, the receiver might get the wake *before* it can read the memory itself. **My lean: this race is acceptable.** The receiver's first `mpm query memory` will succeed because the row is committed by the time the wake fires. If the race does surface, it's a one-time miss; the next wake refresh (e.g., on heartbeat change) fills the gap. — **confirm?**
5. **`--kind` auto-detection from collection.** Rules live in `global_rules`, resolutions in `resolutions`, arbitrations in `theories`. What about a "regular" memory being broadcast (e.g., "we just learned a new fact about FTS5")? **My lean: default kind = the collection name, lowercased. Operator can override with `--kind`.** — **confirm?**

---

## 8. Estimated Diff

~500-700 lines across:
- `internal/core/broadcast.go` (new, ~280 LoC): BroadcastMemory, CheckPendingEventWakes, Heartbeat, DiscoverActiveSessions, computeContentHash, deterministic wake_id
- `internal/core/broadcast_schema_patch.go` (new, ~70 LoC): SharedSessionsDDL, SharedEventWakesDDL, SharedAgentsDDL constants + indexes
- `internal/core/broadcast_test.go` (new, ~280 LoC): 9 unit tests
- `cmd/mpm/ops_broadcast_cmds.go` (new, ~180 LoC): CLI wiring + `--dry-run` / `--json` / `--to` flag parsing
- `cmd/mpm/router.go` (+5 LoC): register the new subcommand
- `internal/core/contradiction_log.go` (+30 LoC): auto-broadcast on resolution
- `internal/core/tools/handlers.go` (+30 LoC): dispatch auto-broadcast on rule-write
- `internal/core/core.go` (+5 LoC): add BroadcastMemory + CheckPendingEventWakes + Heartbeat to CoreDB interface
- `internal/core/db.go` (+10 LoC): install the three new DDLs in attachShared
- `scripts/smoke_arc2.sh` (new, ~150 LoC): 6-step end-to-end proof
- `internal/core/wake_context.go` (+40 LoC): format EventWakesPending block in the agent's response

---

## 9. The One Decision v Should Make Before I Code

Everything above has my best engineering judgment. The one thing I'd lock with v before writing code:

> **Is Arc 2 still operator-only (no auto-broadcast), or is "the resolution itself is an event" (§5) in scope for this atomic epic?**

- If **operator-only:** arc 2 = the `mpm ops broadcast` command + wake fan-out + dedup. No changes to resolution/write paths. ~450 LoC. Lower risk.
- If **auto-broadcast included:** arc 2 = the above + auto-broadcast hooks on every resolution, every global-rule write, every arbitration resolution. ~650 LoC. Higher risk but matches the "active dissemination" name.

My recommendation: **include auto-broadcast.** The whole point of Arc 2 is that epistemic events propagate. Manual broadcasting is Arc 1.5; auto-broadcast is the real arc.

— 808