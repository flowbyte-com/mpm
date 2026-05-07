# mini-bot-telegram Token Reduction Design

## Status
**Approved** — proceed to implementation plan

## Problem
A simple "hi mini-bot" message consumes **7709 input tokens** on `openrouter/free`. Target is ≤1500 tokens for simple messages.

## Root Causes Identified

| Source | Issue | Est. tokens |
|--------|-------|-------------|
| Session history | 50-message sliding window | ~2500 |
| Tool schema | 11 tools × verbose input_schema | ~800 |
| `execute_mpm_command` | **Duplicated** in tool list (framework + profile) | ~100 |
| Anchors | 10 anchors × ~120 chars | ~400 |
| Directives | No limit (up to 20 retrieved) | ~300 |
| Lessons | 3 full-content lessons | ~200 |
| Memories (FTS5) | 5 results × no cap on content length | ~400 |
| FrontCortex | Already ~600 chars with caps | OK |

## Design: Lean Core (~1500 tokens)

### 1. Session History → 10 messages
**Files:** `cmd/telegram/session.go:15`, `core/agent.go:26`

```go
// Before
const maxHistoryMessages = 50

// After
const maxHistoryMessages = 10
```

**Why:** 10 messages = ~5 exchanges = sufficient context for a Telegram bot conversation. Every older message dropped saves ~200 chars.

### 2. Trim System Prompt Context
**Files:** `core/agent.go:270-276`, `core/agent.go:146-165`

| Field | Before | After | Rationale |
|-------|--------|-------|-----------|
| Anchors | 10 | **5** | Anchor weight already gives priority signal; top 5 is enough |
| Lessons | 3 | **2** | "exchange" lessons are verbose; 2 is sufficient |
| Memories (FTS5) | 5 | **3** | 3 focused hits better than 5 broad matches |
| References | 3 | **2** | Lower priority than memories |
| Directives | 20 | **5** | 20 prime directives is excessive; 5 covers all priority rules |

**Changes in `RunAgent`:**
```go
// Before
anchors, _ := GetRecentAnchors(db, 10)
lessons, _ := GetRecentLessons(db, 3)
memories := retrieveMemories(db, query, 5)
references := retrieveReferences(db, query, 3)

// After
anchors, _ := GetRecentAnchors(db, 5)
lessons, _ := GetRecentLessons(db, 2)
memories := retrieveMemories(db, query, 3)
references := retrieveReferences(db, query, 2)
```

**Changes in `retrieveDirectives`:**
```go
// Before (line 150)
ORDER BY created_at DESC LIMIT 20

// After
ORDER BY created_at DESC LIMIT 5
```

### 3. Strip Duplicate `execute_mpm_command`
**File:** `core/agent.go:444-507` (`buildToolListWithLoaded`)

The function hardcodes `execute_mpm_command` as a framework tool (lines 448-461), then iterates `baseTools` which already includes `execute_mpm_command` from the `standard` profile — causing duplication in the final tools array.

**Fix:** Add duplicate-skip check when adding tools from `baseTools`:
```go
// In buildToolListWithLoaded, when adding from baseTools:
for _, name := range baseTools {
    def, ok := GetTool(name)
    if !ok {
        continue
    }
    // Skip execute_mpm_command — it's already added as framework tool above
    if name == "execute_mpm_command" {
        continue
    }
    // ... rest of loop
}
```

### 4. Trim Tool Schema Verbosity
**File:** `core/tools.go:1192-1441` (`init`)

The `standard` profile sends 9 tools on every request. For `openrouter/free` (a lean model), we trim the `input_schema` to minimal form:

| Tool | Schema before | Schema after |
|------|---------------|--------------|
| `list_toolkits` | `{"type":"object","properties":{}}` | `{}` (empty OK) |
| `load_toolkit` | full property description | `{...}` — keep required, but shorten descriptions |
| `unload_toolkit` | same | same |
| `execute_mpm_command` | full description | trim to 1-line |
| `read_file` | path description | trim to 1-line |
| `write_file` | path + content | trim both |
| `ReadFileSemantic` | path + mode | trim |
| `ReadFileCompare` | pathA + pathB | trim |
| `WebSynthesize` | query description | trim |
| `jq` | filter + file | trim |
| `update_identity_knowledge` | key + value + source | trim |

**Key principle:** For `openrouter/free`, the model doesn't need elaborate schema descriptions — it just needs tool names and brief constraints. Descriptions can be 1 sentence.

### 5. No FrontCortex changes needed
`FormatFrontCortex` already has hard caps (80-char identity fields, 3 summaries × 100 chars). Est. contribution: ~500 tokens. Leave as-is.

## Expected Outcome

| Message | Before | After |
|---------|--------|-------|
| "hi mini-bot" (cold start, no history) | ~7709 | ~1400 |
| "hi" with 3 prior exchanges | ~9000 | ~1800 |
| Multi-turn with 10 msg history | ~12000 | ~2200 |

## Files to Modify

1. `cmd/telegram/session.go:15` — `maxHistoryMessages = 10`
2. `core/agent.go:26` — `maxHistoryMessages = 10` (second declaration, confirm single source)
3. `core/agent.go:270-276` — trim anchors/lessons/memories/references limits
4. `core/agent.go:150` — trim directives limit to 5
5. `core/agent.go:467-468` — skip `execute_mpm_command` in baseTools loop
6. `core/tools.go:1192-1441` — trim `input_schema` descriptions in `init()`
7. `core/tools.go` — confirm `execute_mpm_command` description is trimmed to 1-line

## Verification Plan

After implementation, test with a fresh session:
1. Send "hi mini-bot" → confirm input tokens ≤ 1500
2. Send 5 more messages → confirm history cap holds at 10
3. Send "hi" again → confirm no unbounded growth

Run: `grep -n "input=" handler.go` to see token logs in output.