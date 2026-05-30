# Epistemological Pruning Loop — v1.2 Automation Spec

## Status

Current state (v1.0/v1.1): `mpm challenge` creates a theory and weakens the memory, but leaves both the theory and the memory in an open state. No automatic resolution path. No cross-link. No warning injected at recall time.

Target state (v1.2): fully closed loop with bidirectional links, automatic theory cleanup, and LLM-visible warning on challenged memories.

---

## 1. Metadata Schema — Forward and Back Links

### Memory metadata after challenge
```json
{
  "status": "challenged",
  "challenged_theory_id": "<theory_id>"
}
```

### Theory metadata (collection: "theories")
```json
{
  "status": "pending",
  "type": "challenge",
  "memory_id": "<memory_id>"
}
```

### Design rationale
- Forward link (`challenged_theory_id`) lets `challenge restore` resolve the theory in one lookup.
- Back link (`memory_id`) lets `shred` find and delete the theory in one query.
- Both links are created atomically in the same transaction as the status patch.
- `null` key removal (RFC 7396) clears the status on restore — no leftover metadata.

---

## 2. handleChallenge Rewrite — Transactional + Linked

**Location:** `cmd/mpm/handlers.go`

**Behavior:**
1. Open SQLite transaction.
2. Fetch memory, verify it exists.
3. Patch memory metadata: `json_patch(COALESCE(metadata, '{}'), '{"status":"challenged","challenged_theory_id":"<id>"}')`.
4. Generate theory ID, save theory memory with `memory_id` back-link in metadata.
5. Commit transaction. Rollback on any failure.
6. Output: memory ID, theory ID, evidence summary.

**Key invariant:** theory ID is generated *before* the theory save so the forward link can be stored in the memory metadata.

```go
// Pseudocode
tx, _ := dm.SQLDB().Begin()
defer tx.Rollback()

theoryID := mpminternal.GenerateID()
theoryMeta := map[string]interface{}{
    "status":   "pending",
    "type":     "challenge",
    "memory_id": id,
}
theoryMetaJSON, _ := json.Marshal(theoryMeta)

// Patch memory metadata with forward link
patch := map[string]interface{}{
    "status":                "challenged",
    "challenged_theory_id": theoryID,
}
patchJSON, _ := json.Marshal(patch)
tx.Exec(`UPDATE memories SET metadata = json_patch(COALESCE(metadata,'{}'), ?) WHERE id = ?`, string(patchJSON), id)

// Save theory with back link
tx.Exec(`INSERT INTO memories (id, collection, content, metadata, created_at, weight) VALUES (?, 'theories', ?, ?, ?, 1)`,
    theoryID, theoryContent, string(theoryMetaJSON), now)

tx.Commit()
```

---

## 3. handleChallengeRestore — Auto-Resolves Linked Theory

**Location:** `cmd/mpm/handlers.go`

**Behavior:**
1. Fetch memory, extract `challenged_theory_id` from metadata.
2. If theory ID exists: update theory metadata status from `pending` → `disproven`, clear `memory_id`.
3. Patch memory metadata: `json_patch(COALESCE(metadata,'{}'), '{"status":null,"challenged_theory_id":null}')` — null removes both keys (RFC 7396).
4. All in one transaction.
5. Output: memory ID, theory resolution note.

```go
// Pseudocode
meta := parseMetadata(mem["metadata"])
theoryID := meta["challenged_theory_id"]

tx, _ := dm.SQLDB().Begin()
defer tx.Rollback()

if theoryID != "" {
    // Resolve theory as disproven
    resolvePatch := map[string]interface{}{"status": "disproven", "memory_id": nil}
    resolveJSON, _ := json.Marshal(resolvePatch)
    tx.Exec(`UPDATE memories SET metadata = json_patch(COALESCE(metadata,'{}'), ?) WHERE id = ?`,
        string(resolveJSON), theoryID)
}

// Clear challenged status from memory (null removes the keys)
clearPatch := map[string]interface{}{"status": nil, "challenged_theory_id": nil}
clearJSON, _ := json.Marshal(clearPatch)
tx.Exec(`UPDATE memories SET metadata = json_patch(COALESCE(metadata,'{}'), ?) WHERE id = ?`,
    string(clearJSON), id)

tx.Commit()
```

---

## 4. handleShred — Cascade Delete Linked Theory

**Location:** `cmd/mpm/simple_cmds.go`

**Behavior:**
1. Fetch memory, extract `challenged_theory_id` from metadata.
2. Transaction: DELETE from `topic_memberships WHERE memory_id = ?` → DELETE theory if exists → DELETE memory.
3. FTS trigger handles `memories_fts` cleanup automatically.
4. Output: ⚡ Memory <id> shredded. Theory <theory_id> purged.

```go
// Pseudocode
meta := parseMetadata(mem["metadata"])
theoryID := meta["challenged_theory_id"]

tx, _ := dm.SQLDB().Begin()
defer tx.Rollback()

tx.Exec(`DELETE FROM topic_memberships WHERE memory_id = ?`, id)
if theoryID != "" {
    tx.Exec(`DELETE FROM memories WHERE id = ?`, theoryID)
}
tx.Exec(`DELETE FROM memories WHERE id = ?`, id)

tx.Commit()
```

---

## 5. Recall Warning Injector — LLM Path

**Location:** `cmd/mpm/recall.go`

**Behavior:** Same as current — check `strings.Contains(e.metadata, `"status":"challenged"`)`, prepend `[Note: This memory is challenged — treat as unverified]\n` to content for LLM path. Human path already has `[CHALLENGED]` chip.

**No changes needed here** if current implementation is correct. Verify on implementation.

---

## 6. handleLs / handleShow — [CHALLENGED] Chip

**Location:** `cmd/mpm/simple_cmds.go`

**Behavior:** Already implemented — `strings.Contains(meta, `"status":"challenged"`) → [CHALLENGED]` chip. Verify on implementation.

---

## 7. MCP Tool — Already Wired

**Location:** `openclaw/mpm-plugin/src/index.ts`

`challenge_memory` tool already exists and delegates to `mpm challenge`. No changes needed if the Go backend is backward-compatible with the existing call signature (`id` + `rationale`).

Verify: does the existing MCP tool work with the rewritten `handleChallenge`? The rewrite changes internal behavior but the CLI interface (`mpm challenge <id> "<rationale>"`) is identical. MCP tool should be unaffected.

---

## 8. Backward Compatibility Check

- [ ] `mpm challenge <id> "<evidence>"` — existing syntax must continue to work
- [ ] `mpm ls` / `mpm show` — existing output must not break
- [ ] `mpm recall` — existing LLM output format must not break
- [ ] MCP `challenge_memory` — existing tool call must continue to work

---

## 9. Files to Modify

| File | Change |
|------|--------|
| `cmd/mpm/handlers.go` | Rewrite `handleChallenge` with transaction + links; add `handleChallengeRestore` |
| `cmd/mpm/simple_cmds.go` | Add cascade theory delete to `handleShredMem` |
| `cmd/mpm/recall.go` | Verify warning injector present |
| `cmd/mpm/simple_cmds.go` | Verify `[CHALLENGED]` chip in `handleLs`/`handleShow` |
| `docs/MPM_WISHLIST.md` | Add v1.2 item: "Automatic theory resolution on challenge/restore/shred" |

---

## 10. Future: mpm resolve (Optional v1.3)

Not in scope for v1.2, but the architecture supports it:

`mpm resolve <theory_id> --proven|--disproven`

Reads `memory_id` from theory metadata, resolves theory, optionally restores memory weight if disproven.

v1.2 closes the automatic cleanup loop. `mpm resolve` is manual adjudication for edge cases where the human wants to override the auto-resolution.
