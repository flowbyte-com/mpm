# TASK: Autonomous Epistemological Pruning — `mpm challenge` + Immune System

## 1. Problem Statement

MPM is an exceptional archivist — it synthesizes, retrieves, and proactively surfaces. But it still treats every stored memory as load-bearing gospel. A biological mind doesn't just store; it **forgets, prunes, and actively suppresses** outdated mental models when reality proves them wrong.

This is the jump from **database** to **immune system**.

## 2. Core Principles (Zero-Debt Architecture)

- **No new tables** — reuses existing mechanics: `weaken`, theories, `decisions`, `gc`, synthesis
- **Cannot auto-shred silently** — must pass through the theory lifecycle (pending → proven → shred)
- **No runaway amnesia** — human still governs the shred threshold
- **AIITL-aligned** — 808 autonomously identifies contradictions, but the theory lifecycle provides the audit trail

## 3. The Workflow

### Phase 1: Contradiction Detection (Passive, No New Code)

During conversation, after `mpm recall` or proactive hook fires, 808 naturally notices when v says something that contradicts a stored memory. This is already possible with the existing system — 808 just needs to be **aware it can act on this**.

### Phase 2: `mpm challenge` Command

New CLI + MCP tool. Syntax:

```bash
mpm challenge <memory_id> "<evidence>"
```

**Example:**
```bash
mpm challenge abc123 "The last 4 successful builds all used the 'premature abstraction' pattern. The directive in programming.md is wrong."
```

**What happens internally:**

1. A **new theory** is created:
   ```
   HYPOTHESIS: Memory abc123 is obsolete.
   RATIONALE: The last 4 successful builds used premature abstraction. The directive in programming.md consistently produced broken CI.
   STATUS: pending
   VALIDATION_CRITERIA: Compare weight trend of abc123 over last 30 days. If consistently declining and new evidence is strong, mark proven.
   ```

2. The challenged memory is **weakened** immediately:
   ```bash
   mpm weaken abc123 3
   # Drops weight by 3 — pulled below active retrieval threshold
   ```

3. A **decision** is recorded:
   ```
   CONTEXT: Memory abc123 (programming.md directive: "avoid premature abstraction") challenged
   CHOICE: Weaken by 3, create pending theory for final resolution
   RATIONALE: 4 consecutive successful builds contradict the directive. Evidence threshold met.
   ```

### Phase 3: GC Sweep (Automated, But Audited)

```bash
mpm ops gc --shred-negative
```

Existing GC logic already finds expired/weak memories. Extend it to:

- Find memories with `weight < 0`
- For each, check if a **proven theory** exists linking to this memory
- If proven theory exists → shred permanently
- If weight is negative but no theory → **do not shred**, flag for review

This means negative weight alone is not sufficient for shred. The theory provides the evidence chain.

## 4. MCP Tool: `challenge_memory`

In `openclaw/mpm-plugin/src/index.ts`:

```typescript
{ names: ["challenge_memory"], ... }
```

Arguments: `{ memory_id: string, evidence: string }`

Behavior: calls `handleChallenge(memoryID, evidence)` → creates theory + weaken + decision.

## 5. New CLI Command: `handleChallenge`

In `cmd/mpm/handlers.go`:

```go
func handleChallenge(args []string) int {
    // mpm challenge <id> "<evidence>"
    if len(args) < 2 {
        return respond("", "Usage: mpm challenge <id> \"<evidence>\"\n", 1)
    }
    id := args[0]
    evidence := strings.Join(args[1:], " ")

    // 1. Get the memory to log what was challenged
    dm, err := mpminternal.NewDatabaseManager("")
    if err != nil {
        return respond("", fmt.Sprintf("Error: %v\n", err), 1)
    }
    defer dm.Close()

    mem, err := dm.GetMemory(id)
    if err != nil || mem == nil {
        return respond("", fmt.Sprintf("Memory not found: %s\n", id), 1)
    }

    // 2. Weaken it
    dm.UpdateMemoryWeight(id, -3)

    // 3. Create pending theory
    theoryContent := fmt.Sprintf(
        "HYPOTHESIS: Memory %s is obsolete.\nRATIONALE: %s\nSTATUS: pending\nVALIDATION_CRITERIA: Check weight trend over 30 days. If declining and evidence is strong, mark proven.",
        id, evidence)
    dm.SaveMemory("theories", theoryContent, "", nil, nil, nil, false, 1)

    // 4. Record decision
    decisionContent := fmt.Sprintf(
        "CONTEXT: Challenged memory %s (%s)\nCHOICE: Weaken by 3, create pending theory for resolution\nRATIONALE: %s",
        id, mem["collection"], evidence)
    dm.SaveMemory("decisions", decisionContent, "", nil, nil, nil, false, 1)

    fmt.Printf("⚡ Memory challenged. Weakened by 3. Theory pending review.\n")
    fmt.Printf("   Memory: %s\n", id)
    fmt.Printf("   Evidence: %s\n", evidence)
    return 0
}
```

## 6. Extend GC: Shred Negative Weight with Proven Theory

In existing `handleGC` or `gc` logic in `simple_cmds.go` or `handlers.go`:

```go
func handleGC(args []string) int {
    fs := flag.NewFlagSet("gc", flag.ContinueOnError)
    shredNeg := fs.Bool("shred-negative", false, "Shred memories with negative weight that have proven theories")
    dryRun := fs.Bool("dry-run", false, "Show what would be shredded")
    fs.Parse(args[1:])

    dm, err := mpminternal.NewDatabaseManager("")
    if err != nil {
        fmt.Fprintf(os.Stderr, "Error: %v\n", err)
        return 1
    }
    defer dm.Close()

    // Find negative-weight memories
    negMemories, err := dm.GetNegativeWeightMemories()
    if err != nil {
        fmt.Fprintf(os.Stderr, "Error: %v\n", err)
        return 1
    }

    if len(negMemories) == 0 {
        fmt.Println("No negative-weight memories found.")
        return 0
    }

    fmt.Printf("Found %d negative-weight memories:\n\n", len(negMemories))
    for _, m := range negMemories {
        id := m["id"].(string)
        weight := m["weight"].(int64)
        theory := dm.GetProvenTheoryForMemory(id)
        if theory != nil {
            action := "SHRED (proven theory)"
            if !*dryRun {
                dm.ShredMemory(id)
            }
            fmt.Printf("  [%s] weight=%d → %s\n", id, weight, action)
        } else {
            fmt.Printf("  [%s] weight=%d → SKIP (no proven theory)\n", id, weight)
        }
    }
    return 0
}
```

## 7. Files to Modify

- `cmd/mpm/handlers.go` — add `handleChallenge`
- `cmd/mpm/router.go` — register `challenge` command
- `cmd/mpm/simple_cmds.go` — add `handleGC` with `--shred-negative` flag
- `openclaw/mpm-plugin/src/index.ts` — add `challenge_memory` tool
- `internal/web_db.go` — add `GetNegativeWeightMemories()`, `GetProvenTheoryForMemory()`
- `internal/db.go` or `memory.go` — add `ShredMemory()` if not existing

## 8. Verification

| Test | Expected |
|---|---|
| `mpm challenge abc123 "new evidence"` | Theory created, memory weakened, decision logged |
| `mpm ops gc --dry-run` | Shows negative-weight memories with theory status |
| `mpm ops gc --shred-negative` | Shreds only those with proven theories |
| Memory with weight −3 and no theory | NOT shredded — flagged for review |
| `mpm theories pending` | Shows the challenge theory |
| `mpm decisions` | Shows the challenge decision |

## 9. Why This is the Ultimate Endgame

It means 808 doesn't just passively ingest mode files or accept every markdown note as gospel. If a directive consistently produces broken builds, 808 synthesizes a **decision**: *"CHOICE: Ignoring the 'avoid premature abstraction' directive. RATIONALE: Last 4 test suites passed."*

**You didn't build a memory system. You built the infrastructure for an AI to change its mind.**