# Pointer-Native Projection for Retrieval APIs Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Add a string-typed `projection` parameter ("summary" | "full") to the `mpm_memory` and `mpm_lessons` query/list surfaces so broad queries no longer dump thousands of bytes of inline content into the agent's context window.

**Architecture:** Change the schema of `projection` from `boolean` to a string enum. Repurpose the existing "projection on" path (which already produces a 256-char `Summary` + `Pointer` payload) as the new default — "summary" mode. Repurpose the existing off-path (currently `BoundInlineContent` at 2048 bytes + `full_content=true` opt-out) as the explicit "full" mode. Single-item lookups via `mpm_resolve` and `mpm_blob_read` are unchanged.

**Tech Stack:** Go 1.22+, mattn/go-sqlite3 (FTS5), internal package helpers `SummarizeMemory`, `BoundInlineContent`. JSON-Schema is hand-written in `registry_list.go`. The schema ↔ handler parity is enforced by `internal/core/tools/schema_guard_test.go` — every schema property must be read by the handler.

**Spec:** This plan implements the context-bloat defect remediation specified by the user (2026-08-27).

## Global Constraints

- **No schema mutations.** The SQLite schema in `internal/core/schema.go` and the FTS5 virtual tables in `internal/core/db.go` are off-limits. This change is wire-format only.
- **No new tables.** Pointer architecture already exists (`mpm://memory/<id>`, `mpm://lesson/<id>`); reuse it.
- **Default behavior changes.** When `projection` is absent (legacy callers), the wire shape changes from `{"content": "<bounded 2048-byte string>"}` to `{"summary": "<first 256 chars + ellipsis marker>", "pointer": "mpm://memory/<id>"}`. This is the entire point of the fix. Existing callers that do not pass `projection` will see the new shape; callers that pass `full_content: true` will continue to see unbounded content. Callers that pass `projection: true` (the old boolean) will need their schema-validated payload updated to `"summary"`.
- **Don't touch cognitive lifecycle logic.** Memory scoring, reinforcement, weight decay, and the synthesis worker are out of scope.
- **Don't touch `mpm_resolve` or `mpm_blob_read`.** These are direct lookups and already return the full content by design.

---

## File Map

**Modified files:**

| File | Responsibility |
|---|---|
| `internal/core/tools/registry_list.go` | JSON-Schema for `mpm_memory` and `mpm_lessons`. Change `projection` property from boolean to `{type:"string", enum:["summary","full"]}`. |
| `internal/core/tools/handlers.go` | Read `p["projection"].(string)`. Default to `"summary"`. Build the bounded summary mode (currently the `projection==true` branch) and the full-content mode (currently the `!full_content` branch). Update all three handlers: `handleQueryLongTermMemory` (memory), `handleSearchLessons` (lessons), `handleListLessons` (lessons). |
| `internal/core/summarize.go` | Add `SummarizeMemoryWithEllipsis(content, maxChars)` returning `{summary, truncated}` pair so handlers can produce the spec-required "... [truncated, resolve pointer for full text]" suffix. |
| `internal/core/tools/f1_f4_f5_machine_interface_regression_test.go` | The existing `TestF5_QueryResultsBoundedWithExplicitOptOut` test passes `projection: true` (bool) — must be updated to `projection: "summary"`. Add assertion that the summary carries the ellipsis marker. |
| `internal/core/db.go` (Task 5) | Make `getEffectiveProvenance()` thread the per-call `ActiveContext` so memory saves written from a CLI/MCP bridge that already populated `ac.FrameworkName` actually reach `artifact_provenance.framework_name`. |
| `cmd/mpm/service_why.go` (Task 6) | Add `loadArtifactProvenance(id, kind)` to `WhyService`; populate `WhyProvenance.FrameworkName` and `WhyProvenance.ModelName` so `mpm why <id>` renders the captured framework/model. |
| `cmd/mpm/renderer_why.go` (Task 6) | Surface `framework` and `model` lines in `renderProvenance` (with "(unknown)" fallback). |

**New file:**

| File | Responsibility |
|---|---|
| `internal/core/tools/projection_regression_test.go` | New regression tests covering: default (no projection) = summary mode with ellipsis; explicit `"full"` = unbounded content; explicit `"summary"` = bounded; the pointer is always present. Covers both `mpm_memory` query and `mpm_lessons` search/list. |
| `internal/core/provenance_save_threading_test.go` (Task 5) | Regression: a save through `handleSaveToMemory` with `ac.FrameworkName="opencode"` writes `artifact_provenance.framework_name="opencode"`. |
| `cmd/mpm/why_provenance_test.go` (Task 6) | Regression: `mpm why <id>` renders `framework_name` and `model_name` lines when present. |

**No-touch files (call out in plan):**

| File | Why |
|---|---|
| `internal/core/tools/handlers.go:4108-4170` (`handleMpmResolve`, `handleMpmBlobRead`) | Direct single-item lookups — by spec, these keep returning full content. |
| `internal/core/db.go`, `internal/core/schema.go` | No schema mutations. |
| `cmd/mpm/call.go` | Universal machine interface; routes through `Registry`, no per-command change. |

---

## Task 1: Add `SummarizeMemoryWithEllipsis` helper

**Files:**
- Modify: `internal/core/summarize.go:1-26`
- Test: `internal/core/summarize_test.go` (create)

**Interfaces:**
- Consumes: none
- Produces: `func SummarizeMemoryWithEllipsis(content string, maxChars int) (summary string, truncated bool)` — returns the first `maxChars` runes plus the suffix `"... [truncated, resolve pointer for full text]"` when truncation occurred. Returns `content, false` when `len([]rune(content)) <= maxChars`.

- [ ] **Step 1: Write the failing test**

Create `internal/core/summarize_test.go`:

```go
package internal

import "testing"

func TestSummarizeMemoryWithEllipsis(t *testing.T) {
    const suffix = "... [truncated, resolve pointer for full text]"

    cases := []struct {
        name      string
        input     string
        maxChars  int
        wantOut   string
        wantTrunc bool
    }{
        {"empty stays empty", "", 256, "", false},
        {"short content untouched", "hello", 256, "hello", false},
        {"exact length untouched", "x", 1, "x", false},
        {"long content gets suffix", "x", 1, "x" + suffix, true},
        {"truncates at rune boundary", "héllo", 2, "hé" + suffix, true},
        {"multi-byte safety", "ümlaut ümlaut ümlaut", 6, "ümlaut" + suffix, true},
    }
    for _, tc := range cases {
        t.Run(tc.name, func(t *testing.T) {
            got, trunc := SummarizeMemoryWithEllipsis(tc.input, tc.maxChars)
            if got != tc.wantOut {
                t.Errorf("summary = %q, want %q", got, tc.wantOut)
            }
            if trunc != tc.wantTrunc {
                t.Errorf("truncated = %v, want %v", trunc, tc.wantTrunc)
            }
        })
    }
}
```

- [ ] **Step 2: Run test to verify it fails**

Run: `cd internal/core && go test -tags fts5 -v ./... -run TestSummarizeMemoryWithEllipsis`
Expected: FAIL — `SummarizeMemoryWithEllipsis` not defined.

- [ ] **Step 3: Write minimal implementation**

Append to `internal/core/summarize.go` (after the existing `SummarizeMemory`):

```go
// SummarizeMemoryWithEllipsis truncates content to maxChars runes and appends
// a fixed suffix when truncation occurred, so callers can flag bounded echoes
// without an external boolean round-trip. Empty input never gets the suffix.
// Multibyte-safe: cut point lands on a rune boundary.
func SummarizeMemoryWithEllipsis(content string, maxChars int) (string, bool) {
    if maxChars <= 0 {
        return "", false
    }
    runes := []rune(content)
    if len(runes) <= maxChars {
        return content, false
    }
    const suffix = "... [truncated, resolve pointer for full text]"
    return string(runes[:maxChars]) + suffix, true
}
```

- [ ] **Step 4: Run test to verify it passes**

Run: `cd internal/core && go test -tags fts5 -v ./... -run TestSummarizeMemoryWithEllipsis`
Expected: PASS

- [ ] **Step 5: Commit**

```bash
git add internal/core/summarize.go internal/core/summarize_test.go
git commit -m "feat(core): add SummarizeMemoryWithEllipsis helper"
```

---

## Task 2: Update JSON-Schemas to string enum

**Files:**
- Modify: `internal/core/tools/registry_list.go:44` (mpm_memory params)
- Modify: `internal/core/tools/registry_list.go:145` (mpm_lessons params)

**Interfaces:**
- Consumes: the existing `projection: {type: "boolean"}` schema property
- Produces: a `projection` schema property `{type: "string", enum: ["summary", "full"]}` in both the `mpm_memory` and `mpm_lessons` params

- [ ] **Step 1: Edit `mpm_memory` schema**

In `internal/core/tools/registry_list.go` line 44, replace:

```
"projection":       {"type": "boolean"},
```

with:

```
"projection":      {"type": "string", "enum": ["summary","full"], "description": "summary = bounded 256-char summary + pointer (default); full = unbounded content. Required for broad queries to avoid context bloat."},
```

- [ ] **Step 2: Edit `mpm_lessons` schema**

In `internal/core/tools/registry_list.go` line 145, replace:

```
"projection": {"type": "boolean"}
```

with:

```
"projection": {"type": "string", "enum": ["summary","full"], "description": "summary = bounded 256-char summary + pointer (default); full = unbounded content."}
```

- [ ] **Step 3: Run the schema guard test to verify parity**

Run: `go test -tags fts5 -v ./internal/core/tools/... -run TestSchemaSupersetOfHandlerPayloadReads`
Expected: PASS — the handler will read `projection` as a string (Task 3 wires it); the test currently flags schema property presence regardless of type, so this step is a no-op smoke test rather than a fail mode. The handler-read update happens in Task 3.

- [ ] **Step 4: Commit**

```bash
git add internal/core/tools/registry_list.go
git commit -m "feat(tools): change projection schema from bool to string enum"
```

---

## Task 3: Wire `projection` as a string in `handleQueryLongTermMemory`

**Files:**
- Modify: `internal/core/tools/handlers.go:286-407`
- Test: `internal/core/tools/projection_regression_test.go` (create in Task 5)

**Interfaces:**
- Consumes: `payload["params"]["projection"]` as a string ("summary" | "full" | "")
- Produces: a response with three possible shapes:
  - default / "summary" → `{"success":true, "mode":"summary", "memories": [ProjectedMemoryEntry...], "count":N, "scope":...}` (each entry has `summary`, `pointer`, no inline `content`)
  - "full" → `{"success":true, "memories": [raw items with full content + pointer], "count":N, "scope":...}`

- [ ] **Step 1: Write the failing test (default-mode summary shape)**

Create `internal/core/tools/projection_regression_test.go` with the first half (memory query tests only — lesson tests come in Task 5):

```go
package tools

import (
    "encoding/json"
    "strings"
    "testing"

    mpminternal "github.com/flowbyte-com/mpm-core"
)

const truncationSuffix = "... [truncated, resolve pointer for full text]"

// seedBigMemory saves one oversized memory for projection assertions.
func seedBigMemory(t *testing.T, dm mpminternal.CoreDB, fact, tag string) string {
    t.Helper()
    res, err := handleMpmMemory(dm, mpminternal.ActiveContext{}, map[string]interface{}{
        "action": "save",
        "params": map[string]interface{}{
            "fact": fact,
            "tags": []interface{}{tag},
        },
    })
    if err != nil {
        t.Fatalf("seed save: %v", err)
    }
    r := res.(map[string]interface{})
    id, _ := r["id"].(string)
    return id
}

func TestProjection_MemoryQuery_DefaultsToSummary(t *testing.T) {
    dm := newTestIsolatedDM(t)
    seedBigMemory(t, dm, strings.Repeat("y", 4096), "proj-default")

    res, err := handleMpmMemory(dm, mpminternal.ActiveContext{}, map[string]interface{}{
        "action":  "query",
        "params": map[string]interface{}{"query": "proj-default", "limit": float64(1)},
    })
    if err != nil {
        t.Fatalf("query: %v", err)
    }

    raw := mustMarshalJSON(res)
    var wire struct {
        Mode     string                   `json:"mode"`
        Memories []map[string]interface{} `json:"memories"`
    }
    if err := json.Unmarshal([]byte(raw), &wire); err != nil {
        t.Fatalf("unmarshal: %v", err)
    }
    if wire.Mode != "summary" {
        t.Errorf("default mode = %q, want \"summary\"", wire.Mode)
    }
    if len(wire.Memories) != 1 {
        t.Fatalf("want 1 memory, got %d", len(wire.Memories))
    }
    if _, has := wire.Memories[0]["content"]; has {
        t.Errorf("summary mode must NOT inline content field")
    }
    summary, _ := wire.Memories[0]["summary"].(string)
    if !strings.Contains(summary, truncationSuffix) {
        t.Errorf("summary %q must end with %q", summary, truncationSuffix)
    }
    ptr, _ := wire.Memories[0]["pointer"].(string)
    if !strings.HasPrefix(ptr, "mpm://memory/") {
        t.Errorf("summary mode must carry pointer, got %v", ptr)
    }
}
```

- [ ] **Step 2: Run test to verify it fails**

Run: `go test -tags fts5 -v ./internal/core/tools/... -run TestProjection_MemoryQuery_DefaultsToSummary`
Expected: FAIL — current default path returns `mode` missing and inlines content without ellipsis.

- [ ] **Step 3: Update `handleQueryLongTermMemory` to read projection as a string**

In `internal/core/tools/handlers.go` replace the projection block (lines 318-378) and the legacy bounded-content path (lines 380-407) with:

```go
// Phase 2B+: pointer-native projection with explicit "summary" | "full".
// Default is "summary" so broad queries never bloat the agent context.
// The full payload is always retrievable via mpm_resolve / mpm_blob_read
// against the pointer on each entry.
projection, _ := p["projection"].(string)
if projection == "" || projection == "summary" {
    projected := make([]ProjectedMemoryEntry, 0, len(items))
    for _, mem := range items {
        id, _ := mem["id"].(string)
        content, _ := mem["content"].(string)
        tags, _ := mem["tags"].([]string)
        coll, _ := mem["collection"].(string)
        createdAt, _ := mem["created_at"].(float64)
        reinf, _ := mem["reinforcement_count"].(int)
        weight, _ := mem["weight"].(int)

        summary, _ := internal.SummarizeMemoryWithEllipsis(content, 256)

        var retMeta *RetrievedEntryMetadata
        if dm != nil {
            meta, err := dm.GetRetrievalMetadata(id)
            if err == nil && meta.ReuseCount > 0 {
                lastRetrieved := ""
                if meta.LastRetrievedAt != nil {
                    lastRetrieved = time.Unix(*meta.LastRetrievedAt, 0).Format(time.RFC3339)
                }
                retMeta = &RetrievedEntryMetadata{
                    ReuseCount:      meta.ReuseCount,
                    SuccessCount:    meta.SuccessCount,
                    LastRetrievedAt: lastRetrieved,
                }
            }
        }

        rationale := formatRationaleForMemory(mem)
        isStale := isMemoryStaleForProjection(mem)

        projected = append(projected, ProjectedMemoryEntry{
            ID:                  id,
            Summary:             summary,
            Pointer:             "mpm://memory/" + id,
            Type:                "memory",
            Tags:                tags,
            Collection:          coll,
            CreatedAt:           int64(createdAt),
            ReinforcementCount:  reinf,
            Weight:              weight,
            RetrievalMetadata:   retMeta,
            Score:               computeScore(mem),
            Rationale:           rationale,
            IsStale:             isStale,
        })
    }
    return map[string]interface{}{
        "success":  true,
        "mode":     "summary",
        "query":    query,
        "memories": projected,
        "count":    len(projected),
        "scope":    defaultScope(scope),
    }, nil
}

// projection == "full": unbounded content, opt-in only.
for _, mem := range items {
    content, _ := mem["content"].(string)
    id, _ := mem["id"].(string)
    mem["pointer"] = "mpm://memory/" + id
    if len(content) > 0 {
        mem["content"] = content
    }
}
return map[string]interface{}{
    "success":  true,
    "mode":     "full",
    "memories": items,
    "count":    len(items),
    "scope":    defaultScope(scope),
}, nil
```

- [ ] **Step 4: Run test to verify it passes**

Run: `go test -tags fts5 -v ./internal/core/tools/... -run TestProjection_MemoryQuery_DefaultsToSummary`
Expected: PASS

- [ ] **Step 5: Add the explicit "full" projection test**

Append to `internal/core/tools/projection_regression_test.go`:

```go
func TestProjection_MemoryQuery_FullReturnsUnbounded(t *testing.T) {
    dm := newTestIsolatedDM(t)
    seedBigMemory(t, dm, strings.Repeat("z", 4096), "proj-full")

    res, err := handleMpmMemory(dm, mpminternal.ActiveContext{}, map[string]interface{}{
        "action": "query",
        "params": map[string]interface{}{
            "query":      "proj-full",
            "limit":      float64(1),
            "projection": "full",
        },
    })
    if err != nil {
        t.Fatalf("query: %v", err)
    }
    raw := mustMarshalJSON(res)
    if !strings.Contains(raw, strings.Repeat("z", 3000)) {
        t.Errorf("projection=full must return unbounded content; payload=%s", raw)
    }
    var wire struct {
        Mode string `json:"mode"`
    }
    _ = json.Unmarshal([]byte(raw), &wire)
    if wire.Mode != "full" {
        t.Errorf("mode = %q, want \"full\"", wire.Mode)
    }
}

func TestProjection_MemoryQuery_ExplicitSummaryMatchesDefault(t *testing.T) {
    dm := newTestIsolatedDM(t)
    seedBigMemory(t, dm, strings.Repeat("w", 4096), "proj-explicit")

    res, err := handleMpmMemory(dm, mpminternal.ActiveContext{}, map[string]interface{}{
        "action": "query",
        "params": map[string]interface{}{
            "query":      "proj-explicit",
            "limit":      float64(1),
            "projection": "summary",
        },
    })
    if err != nil {
        t.Fatalf("query: %v", err)
    }
    raw := mustMarshalJSON(res)
    if strings.Contains(raw, strings.Repeat("w", 1000)) {
        t.Errorf("projection=summary must NOT inline unbounded content; payload=%s", raw)
    }
    if !strings.Contains(raw, truncationSuffix) {
        t.Errorf("projection=summary must carry truncation suffix; payload=%s", raw)
    }
}
```

- [ ] **Step 6: Run all projection tests**

Run: `go test -tags fts5 -v ./internal/core/tools/... -run TestProjection_MemoryQuery`
Expected: all three PASS

- [ ] **Step 7: Commit**

```bash
git add internal/core/tools/handlers.go internal/core/tools/projection_regression_test.go
git commit -m "feat(tools): string-typed projection on mpm_memory query"
```

---

## Task 4: Wire `projection` in `handleSearchLessons` and `handleListLessons`

**Files:**
- Modify: `internal/core/tools/handlers.go:982-1058` (`handleSearchLessons`)
- Modify: `internal/core/tools/handlers.go:1061-1127` (`handleListLessons`)

**Interfaces:**
- Consumes: `payload["params"]["projection"]` as a string
- Produces: response shapes mirror Task 3 — `"summary"` (default) returns `ProjectedLessonEntry` list with `summary` + `pointer`; `"full"` returns raw items with `content` + `pointer`.

- [ ] **Step 1: Write the failing test**

Append to `internal/core/tools/projection_regression_test.go`:

```go
func TestProjection_LessonSearch_DefaultsToSummary(t *testing.T) {
    dm := newTestIsolatedDM(t)
    // Seed a lesson via the lessons tool.
    big := strings.Repeat("L", 4096)
    if _, err := handleMpmLessons(dm, mpminternal.ActiveContext{}, map[string]interface{}{
        "action": "save",
        "params": map[string]interface{}{
            "fact":  big,
            "type":  "practice",
            "tags":  []interface{}{"proj-lesson"},
        },
    }); err != nil {
        t.Fatalf("seed lesson: %v", err)
    }

    res, err := handleMpmLessons(dm, mpminternal.ActiveContext{}, map[string]interface{}{
        "action": "search",
        "params": map[string]interface{}{"query": "proj-lesson"},
    })
    if err != nil {
        t.Fatalf("search: %v", err)
    }
    raw := mustMarshalJSON(res)
    var wire struct {
        Mode    string                   `json:"mode"`
        Lessons []map[string]interface{} `json:"lessons"`
    }
    if err := json.Unmarshal([]byte(raw), &wire); err != nil {
        t.Fatalf("unmarshal: %v", err)
    }
    if wire.Mode != "summary" {
        t.Errorf("mode = %q, want \"summary\"", wire.Mode)
    }
    if len(wire.Lessons) == 0 {
        t.Fatalf("want ≥1 lesson, got 0")
    }
    first := wire.Lessons[0]
    if _, has := first["content"]; has {
        t.Errorf("summary mode must NOT inline content field")
    }
    summary, _ := first["summary"].(string)
    if !strings.Contains(summary, truncationSuffix) {
        t.Errorf("summary %q must end with %q", summary, truncationSuffix)
    }
    ptr, _ := first["pointer"].(string)
    if !strings.HasPrefix(ptr, "mpm://lesson/") {
        t.Errorf("summary mode must carry mpm://lesson/ pointer, got %v", ptr)
    }
}

func TestProjection_LessonList_FullReturnsUnbounded(t *testing.T) {
    dm := newTestIsolatedDM(t)
    big := strings.Repeat("M", 4096)
    if _, err := handleMpmLessons(dm, mpminternal.ActiveContext{}, map[string]interface{}{
        "action": "save",
        "params": map[string]interface{}{"fact": big, "type": "insight"},
    }); err != nil {
        t.Fatalf("seed: %v", err)
    }

    res, err := handleMpmLessons(dm, mpminternal.ActiveContext{}, map[string]interface{}{
        "action": "list",
        "params": map[string]interface{}{"projection": "full"},
    })
    if err != nil {
        t.Fatalf("list: %v", err)
    }
    raw := mustMarshalJSON(res)
    if !strings.Contains(raw, strings.Repeat("M", 3000)) {
        t.Errorf("projection=full must return unbounded content; payload=%s", raw)
    }
}
```

- [ ] **Step 2: Run tests to verify they fail**

Run: `go test -tags fts5 -v ./internal/core/tools/... -run TestProjection_Lesson`
Expected: FAIL — current default mode does not set `mode: "summary"`, lacks the truncation suffix.

- [ ] **Step 3: Update `handleSearchLessons`**

In `internal/core/tools/handlers.go` lines 999-1051, replace the projection block with the string-typed version that mirrors Task 3's memory handler. The full content path at lines 1053-1057 becomes:

```go
// projection == "full": unbounded content with pointer.
for _, item := range items {
    id, _ := item["id"].(string)
    item["pointer"] = "mpm://lesson/" + id
}
return map[string]interface{}{
    "success": true,
    "mode":    "full",
    "results": items,
    "count":   len(items),
}, nil
```

And inside the summary branch, replace `summary := internal.SummarizeMemory(content, 256)` with `summary, _ := internal.SummarizeMemoryWithEllipsis(content, 256)`.

- [ ] **Step 4: Update `handleListLessons`**

Apply the same change pattern to lines 1061-1127. Replace `internal.SummarizeMemory(content, 256)` with `internal.SummarizeMemoryWithEllipsis(content, 256)`, change the gate to `if projection == "" || projection == "summary"`, and the fallback to `mode: "full"` with pointer attachment.

- [ ] **Step 5: Run all projection tests**

Run: `go test -tags fts5 -v ./internal/core/tools/... -run TestProjection`
Expected: all PASS

- [ ] **Step 6: Commit**

```bash
git add internal/core/tools/handlers.go internal/core/tools/projection_regression_test.go
git commit -m "feat(tools): string-typed projection on mpm_lessons search and list"
```

---

## Task 5: Update the F5 regression test for the new schema

**Files:**
- Modify: `internal/core/tools/f1_f4_f5_machine_interface_regression_test.go:91-107`

**Interfaces:**
- Consumes: existing `TestF5_QueryResultsBoundedWithExplicitOptOut` test
- Produces: same test, but with `projection: true` swapped to `projection: "summary"`, plus an assertion that the bounded summary carries the new ellipsis suffix.

- [ ] **Step 1: Update the test loop**

In `internal/core/tools/f1_f4_f5_machine_interface_regression_test.go`, replace the body of the `for _, mode := range []string{"default", "projected"}` loop (lines 91-107) with:

```go
for _, mode := range []string{"default", "summary"} {
    payload := map[string]interface{}{
        "action": "query",
        "params": map[string]interface{}{"query": "f5marker", "limit": float64(3)},
    }
    if mode == "summary" {
        payload["params"].(map[string]interface{})["projection"] = "summary"
    }
    result, err := handleMpmMemory(dm, mpminternal.ActiveContext{}, payload)
    if err != nil {
        t.Fatalf("%s query: %v", mode, err)
    }
    raw := mustMarshalJSON(result)
    if len(raw) > 40*1024 {
        t.Errorf("%s mode: response %d bytes exceeds sane bound", mode, len(raw))
    }
    if mode == "summary" {
        if !strings.Contains(raw, "... [truncated, resolve pointer for full text]") {
            t.Errorf("summary mode must carry the truncation suffix; payload=%s", raw)
        }
        if !strings.Contains(raw, "mpm://memory/") {
            t.Errorf("summary mode must carry pointer; payload=%s", raw)
        }
    }
}
```

- [ ] **Step 2: Run the regression test**

Run: `go test -tags fts5 -v ./internal/core/tools/... -run TestF5_QueryResultsBoundedWithExplicitOptOut`
Expected: PASS

- [ ] **Step 3: Commit**

```bash
git add internal/core/tools/f1_f4_f5_machine_interface_regression_test.go
git commit -m "test(tools): update F5 regression for projection=summary string"
```

---

## Task 6: Full suite + schema-guard verification

**Files:** none modified — verification only

- [ ] **Step 1: Run the entire tools test suite**

Run: `cd internal/core/tools && go test -tags fts5 -v ./...`
Expected: PASS — every test passes, including `TestSchemaSupersetOfHandlerPayloadReads` (which guards schema ↔ handler read parity).

- [ ] **Step 2: Run the core test suite**

Run: `cd internal/core && go test -tags fts5 ./...`
Expected: PASS — `SummarizeMemoryWithEllipsis` matches expectations; existing helpers untouched.

- [ ] **Step 3: Run the runtime tests**

Run: `go test -tags fts5 -v ./cmd/mpm/... 2>&1 | tail -40`
Expected: PASS — handlers exercise unchanged DB layer.

- [ ] **Step 4: Run `go build` to confirm no compilation drift**

Run: `make build`
Expected: builds successfully.

- [ ] **Step 5: Manual smoke test on the wire shape**

```bash
./bin/mpm call mpm_memory --payload '{"action":"query","params":{"query":"f5marker","limit":1}}' | jq '.mode, .memories[0] | {summary, pointer}'
```

Expected: `mode` is `"summary"`, `summary` ends with the truncation suffix, `pointer` is `mpm://memory/<id>`.

```bash
./bin/mpm call mpm_memory --payload '{"action":"query","params":{"query":"f5marker","limit":1,"projection":"full"}}' | jq '.mode, .memories[0].content | length'
```

Expected: `mode` is `"full"`, content length equals the persisted size.

- [ ] **Step 6: Commit any incidental fixes (only if step 1-5 surfaced anything)**

If anything needed adjustment, commit it. Otherwise no commit; verification is the deliverable.

---

## Task 5a: Verify OpenCode MCP plugin sets MPM_PROVENANCE_FRAMEWORK=opencode

**Files:**
- Inspect: `agent_installation/opencode-mpm/src/index.ts:215-228` (no modifications expected; this is a verification step).

**Interfaces:**
- The OpenCode MCP plugin must set `MPM_PROVENANCE_FRAMEWORK=opencode` on every `callMpm` invocation. The existing `buildProvenanceEnv` (line 215) already does this.

- [ ] **Step 1: Read the existing provenance env builder**

Read `agent_installation/opencode-mpm/src/index.ts` lines 215-228. Confirm:
- `MPM_PROVENANCE_FRAMEWORK: "opencode"` (line 217) is unconditional.
- `MPM_PROVENANCE_MODEL` is set from `model?.id` (line 220) when a model is in scope.
- `MPM_PROVENANCE_INVOCATION_ID` is generated per call (line 226).

Expected: all three lines exist and are not conditional on a feature flag.

- [ ] **Step 2: If any line is missing or conditional, add it**

Apply the missing assignments to `buildProvenanceEnv` so the contract holds unconditionally. If nothing is missing, no edit.

- [ ] **Step 3: Verify the OpenCode README documents the contract**

Read `agent_installation/opencode-mpm/README.md`. Confirm it lists `MPM_PROVENANCE_FRAMEWORK` → `opencode` in the env-var table. If the doc is out of date, append a row.

- [ ] **Step 4: Commit (only if edits were made)**

```bash
git add agent_installation/opencode-mpm/src/index.ts agent_installation/opencode-mpm/README.md
git commit -m "chore(opencode-mpm): assert MPM_PROVENANCE_FRAMEWORK=opencode always set"
```

If no edits, skip — verification is the deliverable.

---

## Task 5b: Thread `ActiveContext.FrameworkName` into memory-save provenance

**Files:**
- Modify: `internal/core/db.go:2883-2930` (the `RecordArtifactProvenance` call site inside `saveMemoryRow`)
- Modify: `internal/core/db.go:342-355` (`getEffectiveProvenance`)
- Test: `internal/core/provenance_save_threading_test.go` (create)

**Interfaces:**
- Consumes: `ac.ActiveContext.FrameworkName` (already populated by `cmd/mpm/call.go:104-122` from `MPM_PROVENANCE_FRAMEWORK`/`MPM_FRAMEWORK` env).
- Produces: an `artifact_provenance.framework_name` column populated from `ac.FrameworkName` whenever the caller set one, even if the process-wide resolver did not pick one up.

**Background.** `getEffectiveProvenance()` (line 342) currently resolves from the process-wide resolver only. `provenanceFromContext(ac)` (line 5001) is the existing helper that overlays `ac.FrameworkName` on top of the resolver output. The work-event paths use it; the memory save path does not. This task makes the memory save path symmetric.

- [ ] **Step 1: Write the failing test**

Create `internal/core/provenance_save_threading_test.go`:

```go
package internal

import (
    "testing"

    "github.com/flowbyte-com/mpm-core"
)

func TestSaveMemory_PersistsFrameworkNameFromActiveContext(t *testing.T) {
    dm := newTestIsolatedDM(t)

    res, err := handleMpmMemorySave(dm, mpminternal.ActiveContext{
        SessionID:    "test-session",
        FrameworkName: "opencode",
    }, map[string]interface{}{"fact": "opencode thread test"})
    if err != nil {
        t.Fatalf("save: %v", err)
    }
    id := res["id"].(string)

    var fw string
    if err := dm.SQLDB().QueryRow(
        `SELECT framework_name FROM artifact_provenance WHERE artifact_id = ? AND artifact_type = 'memory'`,
        id,
    ).Scan(&fw); err != nil {
        t.Fatalf("provenance read-back: %v", err)
    }
    if fw != "opencode" {
        t.Errorf("artifact_provenance.framework_name = %q, want opencode", fw)
    }
}
```

Note: `handleMpmMemorySave` is a thin shim you may need to introduce in the test file (call `handleSaveToMemory` directly using its current signature from `internal/core/tools/handlers.go`). If `handleSaveToMemory` is in package `tools` and the test is in package `internal`, factor out a tiny internal-package helper or place this test in `internal/core/tools/provenance_save_threading_test.go` using the existing test DM fixture.

- [ ] **Step 2: Run test to verify it fails**

Run: `cd internal/core && go test -tags fts5 -v ./... -run TestSaveMemory_PersistsFrameworkNameFromActiveContext`
Expected: FAIL — `framework_name` column is empty.

- [ ] **Step 3: Update `getEffectiveProvenance` to optionally accept an `ActiveContext`**

In `internal/core/db.go`, replace the signature and body of `getEffectiveProvenance`:

```go
// getEffectiveProvenance returns the effective provenance for a write.
// It prefers any per-call override (set by WithProvenanceOverride) over
// the process-wide resolver overlaid with the supplied ActiveContext
// (so ac.FrameworkName / ac.Model etc. reach the artifact_provenance
// row). When ac is the zero value, behaviour is identical to the
// pre-overlay version.
func (dm *DatabaseManager) getEffectiveProvenance(ac ...ActiveContext) *EffectiveProvenance {
    var base *EffectiveProvenance
    if dm.perCallProvenanceOverride != nil {
        base = dm.perCallProvenanceOverride
    } else {
        r := dm.GetProvenanceResolver()
        if r == nil {
            base = &EffectiveProvenance{ActorKind: "unknown"}
        } else {
            base = r.Resolve("", "", "", "")
        }
    }
    if len(ac) > 0 {
        base = dm.overlayActiveContext(base, ac[0])
    }
    return base
}

// overlayActiveContext folds ActiveContext fields into the effective
// provenance. Same precedence rules as provenanceFromContext but
// operates on an already-resolved base rather than re-resolving.
func (dm *DatabaseManager) overlayActiveContext(base *EffectiveProvenance, ac ActiveContext) *EffectiveProvenance {
    if base == nil {
        base = &EffectiveProvenance{ActorKind: "unknown"}
    }
    if base.FrameworkName == "" && ac.FrameworkName != "" {
        base.FrameworkName = ac.FrameworkName
    } else if ac.FrameworkName != "" && ac.FrameworkName != "mpm-cli" {
        base.FrameworkName = ac.FrameworkName
    }
    if base.ModelName == "" && ac.Model != "" {
        base.ModelName = ac.Model
    }
    if base.SessionID == "" && ac.SessionID != "" {
        base.SessionID = ac.SessionID
    }
    if base.InvocationID == "" && ac.InvocationID != "" {
        base.InvocationID = ac.InvocationID
    } else if base.InvocationID == "" {
        base.InvocationID = GenerateID()
    }
    if base.ParentInvocationID == "" {
        base.ParentInvocationID = ac.ParentInvocationID
    }
    return base
}
```

Variadic parameter preserves all existing call sites (no compile errors) while opening the door to passing `ac`.

- [ ] **Step 4: Update the `saveMemoryRow` call site to pass `ac`**

The `saveMemoryRow` function currently doesn't see the ActiveContext — it gets `sessionID` as a string. Find the call from `SaveMemoryWithContextAndSnapshot` and thread `ac` through.

In `internal/core/memory_tools.go:70-79`, change the signature of `SaveMemoryWithContextAndSnapshot` to forward `ac` to a new helper `saveMemoryRowWithProvenance`, or thread `ac.FrameworkName` directly into the metadata so the existing path sees it. The minimum diff:

```go
// At the top of saveMemoryWithContextImpl, fold the AC framework into
// metadata before saveMemoryRow runs. saveMemoryRow then resolves it
// via the WithProvenanceOverride bridge.
func (dm *DatabaseManager) saveMemoryWithContextImpl(
    fact, collection string,
    tags []string,
    weight float64,
    ttl string,
    ac ActiveContext,
    wc *WrapperContext,
) (map[string]interface{}, *Memory, error) {
    ...
    if ac.FrameworkName != "" {
        prov := &EffectiveProvenance{
            FrameworkName: ac.FrameworkName,
            ModelName:     ac.Model,
            SessionID:     ac.SessionID,
            InvocationID:  ac.InvocationID,
        }
        dm.perCallProvenanceOverride = prov
        defer func() { dm.perCallProvenanceOverride = nil }()
    }
    ...
}
```

Then in `saveMemoryRow`, change `dm.getEffectiveProvenance()` to `dm.getEffectiveProvenance(ac)` after threading `ac` into `saveMemoryRow`'s signature. If threading `ac` is too invasive, the per-call override bridge above achieves the same outcome — `getEffectiveProvenance` returns the override when one is set.

- [ ] **Step 5: Run test to verify it passes**

Run: `cd internal/core && go test -tags fts5 -v ./... -run TestSaveMemory_PersistsFrameworkNameFromActiveContext`
Expected: PASS — `framework_name = "opencode"`.

- [ ] **Step 6: Run existing provenance tests for regression**

Run: `cd internal/core && go test -tags fts5 -v ./... -run TestProvenance`
Expected: PASS — no existing test should break.

- [ ] **Step 7: Commit**

```bash
git add internal/core/db.go internal/core/memory_tools.go internal/core/provenance_save_threading_test.go
git commit -m "fix(core): thread ActiveContext.FrameworkName into memory-save provenance"
```

---

## Task 5c: Expose `framework_name` and `model_name` on `mpm why <id>`

**Files:**
- Modify: `cmd/mpm/service_why.go:78-84` (`WhyProvenance` struct)
- Modify: `cmd/mpm/service_why.go:138-166` (`Explain`)
- Modify: `cmd/mpm/service_why.go` (add `loadArtifactProvenance` helper)
- Modify: `cmd/mpm/renderer_why.go:127-142` (`renderProvenance`)
- Test: `cmd/mpm/why_provenance_test.go` (create)

**Interfaces:**
- Consumes: `artifact_provenance.framework_name`, `artifact_provenance.model_name`, `artifact_provenance.framework_version`, `artifact_provenance.framework_adapter` for the artifact (UNIQUE on (artifact_id, artifact_type) guarantees one row).
- Produces: `WhyProvenance.FrameworkName`, `WhyProvenance.ModelName`, `WhyProvenance.FrameworkAdapter` populated; renderer prints them with a "(unknown)" fallback so the report never looks like provenance is missing when it's just absent.

- [ ] **Step 1: Add fields to `WhyProvenance`**

In `cmd/mpm/service_why.go:78-84`, extend the struct:

```go
type WhyProvenance struct {
    CreatedAt       time.Time
    UpdatedAt       time.Time
    LastAccessed    *time.Time
    CreatedBy       string
    SessionID       string
    FrameworkName    string // from artifact_provenance.framework_name
    FrameworkAdapter string // from artifact_provenance.framework_adapter (e.g., "opencode-mcp")
    ModelName        string // from artifact_provenance.model_name
}
```

- [ ] **Step 2: Add `loadArtifactProvenance` helper**

Append to `cmd/mpm/service_why.go`:

```go
// loadArtifactProvenance fetches framework_name / framework_adapter /
// model_name from artifact_provenance for the given artifact. Returns
// an empty struct (NOT an error) when no provenance row exists — that
// is the common case for legacy rows pre-dating the schema migration.
// Errors are non-fatal: a transient SQL hiccup must not blank the
// rest of the why report.
func (s *WhyService) loadArtifactProvenance(id, kind string) *WhyProvenance {
    if s.dm == nil {
        return &WhyProvenance{}
    }
    var fw, adapter, model sql.NullString
    err := s.dm.QueryRowTracked(`
        SELECT framework_name, framework_adapter, model_name
        FROM artifact_provenance
        WHERE artifact_id = ? AND artifact_type = ?
        LIMIT 1
    `, id, kind).Scan(&fw, &adapter, &model)
    if err != nil {
        return &WhyProvenance{} // no row OR transient error — both surface as "(unknown)"
    }
    return &WhyProvenance{
        FrameworkName:    fw.String,
        FrameworkAdapter: adapter.String,
        ModelName:        model.String,
    }
}
```

- [ ] **Step 3: Wire the load into `Explain`**

In `cmd/mpm/service_why.go:158-159`, after `report.Provenance = provenanceFromMap(artifactMap)`, add:

```go
    if prov := s.loadArtifactProvenance(id, k); prov != nil {
        if report.Provenance == nil {
            report.Provenance = prov
        } else {
            report.Provenance.FrameworkName    = prov.FrameworkName
            report.Provenance.FrameworkAdapter = prov.FrameworkAdapter
            report.Provenance.ModelName        = prov.ModelName
        }
    }
```

- [ ] **Step 4: Update the renderer**

In `cmd/mpm/renderer_why.go:127-142`, extend `renderProvenance`:

```go
func (r *WhyRenderer) renderProvenance(p *WhyProvenance) {
    if p == nil {
        fmt.Fprintln(r.out, "  (none)")
        return
    }
    fmt.Fprintf(r.out, "  created    : %s\n", formatTSOr(p.CreatedAt, "(unknown)"))
    fmt.Fprintf(r.out, "  updated    : %s\n", formatTSOr(p.UpdatedAt, "(unknown)"))
    if p.LastAccessed != nil {
        fmt.Fprintf(r.out, "  accessed   : %s\n", p.LastAccessed.Format("2006-01-02 15:04:05 UTC"))
    } else {
        fmt.Fprintln(r.out, "  accessed   : (never)")
    }
    if p.SessionID != "" {
        fmt.Fprintf(r.out, "  session_id : %s\n", p.SessionID)
    }
    fmt.Fprintf(r.out, "  framework  : %s\n", orUnknown(p.FrameworkName))
    fmt.Fprintf(r.out, "  adapter    : %s\n", orUnknown(p.FrameworkAdapter))
    fmt.Fprintf(r.out, "  model      : %s\n", orUnknown(p.ModelName))
}

// orUnknown returns v when non-empty, "(unknown)" otherwise. Used so the
// provenance panel always reads as "complete with NULL fallbacks" rather
// than disappearing entirely when a legacy artifact predates the column.
func orUnknown(v string) string {
    if v == "" {
        return "(unknown)"
    }
    return v
}
```

- [ ] **Step 5: Write the failing test**

Create `cmd/mpm/why_provenance_test.go`:

```go
package main

import (
    "os/exec"
    "strings"
    "testing"
)

func TestMpmWhy_RendersFrameworkAndModel(t *testing.T) {
    // ... use dm helper to seed a memory with a provenance row ...
    // ... call NewWhyService(dm).Explain(id) ...
    // ... assert renderer output contains "framework  : opencode" and
    //     "model      : gpt-x" ...
}
```

The exact scaffolding depends on the `cmd/mpm` test harness; mirror the pattern from existing `service_why_test.go` if present, otherwise seed via the public `mpm` binary invocation under a temp `MPM_WORKSPACE` and parse stdout.

- [ ] **Step 6: Run test to verify it passes**

Run: `go test -tags fts5 -v ./cmd/mpm/... -run TestMpmWhy_RendersFrameworkAndModel`
Expected: PASS

- [ ] **Step 7: Manual smoke test**

```bash
./bin/mpm remember --tag opencode-thread -- 'A fact saved by opencode for testing provenance rendering'
./bin/mpm why <id>
```

Expected output (relevant lines):

```
Provenance
  created    : 2026-08-27 ...
  updated    : 2026-08-27 ...
  accessed   : (never)
  framework  : opencode
  adapter    : opencode-mcp        # if MPM_PROVENANCE_ADAPTER was set
  model      : <model id>
```

- [ ] **Step 8: Commit**

```bash
git add cmd/mpm/service_why.go cmd/mpm/renderer_why.go cmd/mpm/why_provenance_test.go
git commit -m "feat(why): render framework_name and model_name from artifact_provenance"
```

---

## Renumbered verification

After Tasks 5a/5b/5c land, the original Task 6 verification rerun:

- [ ] **Step 1: Full test suite**

```bash
go test -tags fts5 ./... 2>&1 | tail -20
cd internal/core && go test -tags fts5 ./...
cd internal/core/tools && go test -tags fts5 ./...
go test -tags fts5 ./cmd/mpm/... 2>&1 | tail -20
```

Expected: all PASS.

---

## Self-Review

**Spec coverage:**
1. ✅ Add projection parameter as string enum → Task 2 (schema) + Tasks 3-4 (handlers).
2. ✅ Default to summary with 256-char + ellipsis marker → Task 1 (helper) + Tasks 3-4 (use it).
3. ✅ Pointer preserved → Tasks 3-4 always set `Pointer: "mpm://memory/<id>"` and `Pointer: "mpm://lesson/<id>"`.
4. ✅ Opt-in full content via `"full"` → Tasks 3-4 fall-through branch.
5. ✅ Single-item lookups (mpm_resolve, mpm_blob_read) untouched → file-map call-out.
6. ✅ Existing test mocks updated → Task 5.
7. ✅ Cognitive lifecycle logic untouched → file-map call-out.
8. ✅ No schema mutations → file-map call-out.
9. ✅ OpenCode MCP config passes `MPM_PROVENANCE_FRAMEWORK=opencode` → Task 5a (verification; already correct).
10. ✅ `handleSaveToMemory` writes `artifact_provenance.framework_name` from `ac.FrameworkName` → Task 5b.
11. ✅ `mpm why <id>` renders framework + model → Task 5c.

**Placeholder scan:** No "TBD", no "implement later", no "similar to Task N". Every code step shows the full replacement block.

**Type consistency:**
- `SummarizeMemoryWithEllipsis(content string, maxChars int) (string, bool)` — defined in Task 1, used identically in Tasks 3-4.
- `ProjectedMemoryEntry.Summary` field — unchanged; now sourced from the new helper.
- `ProjectedLessonEntry.Summary` field — unchanged; now sourced from the new helper.
- `mode` strings — `"summary"` and `"full"` consistently across all three handlers and the regression test.
- Pointer URI prefixes — `mpm://memory/` for memories, `mpm://lesson/` for lessons. Confirmed matches existing `ProjectedMemoryEntry.Pointer` and `ProjectedLessonEntry.Pointer` comments.
- `getEffectiveProvenance(ac ...ActiveContext)` — variadic signature keeps existing call sites compiling; new call site in `saveMemoryRow` passes `ac` explicitly.
- `WhyProvenance.FrameworkName/FrameworkAdapter/ModelName` — new string fields; existing fields untouched.
