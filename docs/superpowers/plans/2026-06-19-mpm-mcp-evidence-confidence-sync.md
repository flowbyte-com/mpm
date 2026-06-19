# mpm-mcp Evidence/Confidence Sync Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Add 9 evidence/confidence tools to the native Go MCP server (`cmd/mpm-mcp`) and lift the inline SQL in the matching 9 CLI handlers in `cmd/mpm/call.go` into shared `DatabaseManager` methods in `internal/call_helpers.go`.

**Architecture:** Move the `artifactTable` helper to `internal/`. Add 9 new `dm` methods to `internal/call_helpers.go`. Each dm method returns the same JSON-ready map the existing call handler returns (so CLI output stays byte-identical). Refactor the 9 `call*` handlers to delegate to the dm methods. Add 9 new tool specs and handlers in `cmd/mpm-mcp/tools.go`. CLI and MCP both pass the dm result through unchanged. TDD throughout.

**Tech Stack:** Go 1.x, `mattn/go-sqlite3` (FTS5), `mark3labs/mcp-go`, `stretchr/testify`.

**Spec:** `docs/superpowers/specs/2026-06-19-mpm-mcp-evidence-confidence-sync-design.md`

---

## File Structure

- `internal/artifact_table.go` — **new.** One-function file: `ArtifactTable(artifactType) string`.
- `internal/call_helpers.go` — **modify.** Add 9 new `dm` methods (lines 64x → 64x+~280).
- `internal/call_helpers_test.go` — **new.** Table-driven tests for each new dm method using `newTestDM(t)`.
- `cmd/mpm/call.go` — **modify.** Replace 9 handler bodies (~250 lines of inline SQL) with one-line dispatches. Remove the local `artifactTable` function. Update imports.
- `cmd/mpm-mcp/tools.go` — **modify.** Add 9 `toolX()` specs and 9 `handleX(dm)` functions. Register all 9 in `RegisterAllTools`.

No changes to: `cmd/mpm-mcp/main.go`, `cmd/mpm/call.go`'s `toolRegistry` map, `internal/evidence*.go`, `internal/confidence*.go`, `internal/db.go` schema.

---

## Conventions

**Test helper.** Use the existing `newTestDM(t)` from `internal/evidence_store_test.go:319`. It opens a temp SQLite, calls `NewDatabaseManagerForDB` + `InitSchema`, and registers `t.Cleanup` for close. No new helper needed.

**Build/test commands.** Per `CLAUDE.md`:
```bash
cd /home/v/workspace/projects/mpm
go test -tags fts5 -v ./internal/... -run TestFunctionName
go build -tags fts5 ./cmd/mpm
go build -tags fts5 ./cmd/mpm-mcp
```

**Output-shape rule.** Each dm method returns the exact map the existing `call*` handler returns today. Both CLI and MCP pass it through. Existing CLI callers see byte-identical output. New MCP clients see the same shape.

**Validation rule.** `dm.AddEvidence` does required-arg preflight (art_id, type, source_group, created_by) and reads back confidence. Deeper validation (sensitive content, evidence type registry) lives in `internal.AddEvidence`. The dm method is the single entry point for both surfaces, so they cannot diverge.

---

## Task 1: Move `artifactTable` to `internal/`

**Files:**
- Create: `internal/artifact_table.go`
- Modify: `cmd/mpm/call.go:1128-1133` (remove function), `cmd/mpm/call.go:822,1059` (update call sites)

- [ ] **Step 1: Create `internal/artifact_table.go`**

Create the file with this exact content:

```go
package internal

// ArtifactTable maps an artifact type to its underlying SQLite table name.
// Used by ShowConfidence/RecomputeConfidence to read the live confidence
// column after an evidence write. Memories and lessons are the only v1
// artifact types — everything else (including empty) maps to memories.
func ArtifactTable(artifactType string) string {
	if artifactType == "lesson" {
		return "lessons"
	}
	return "memories"
}
```

- [ ] **Step 2: Update `cmd/mpm/call.go` to use `internal.ArtifactTable`**

In `cmd/mpm/call.go`:
- Line 822: change `artifactTable(artifactType)` to `internal.ArtifactTable(artifactType)`
- Line 1059: change `artifactTable(artifactType)` to `internal.ArtifactTable(artifactType)`
- Lines 1128-1133: delete the `func artifactTable(artifactType string) string { ... }` block

- [ ] **Step 3: Build both binaries**

```bash
go build -tags fts5 ./cmd/mpm
go build -tags fts5 ./cmd/mpm-mcp
```

Expected: both compile with no errors.

- [ ] **Step 4: Commit**

```bash
git add internal/artifact_table.go cmd/mpm/call.go
git commit -m "refactor(internal): move artifactTable from cmd/mpm to internal package

Both CLI and MCP evidence/confidence code paths need the artifact_type
to table name mapping. Moving it to internal/ removes the upcoming
duplication when the new dm methods are wired.

Co-Authored-By: Claude Opus 4.7 <noreply@anthropic.com>"
```

---

## Task 2: `dm.AddEvidence` + MCP `add_evidence`

**Files:**
- Modify: `internal/call_helpers.go` (add `AddEvidence` method)
- Create: `internal/call_helpers_test.go` (test for `AddEvidence`)
- Modify: `cmd/mpm/call.go` (refactor `callAddEvidence` lines 772-831)
- Modify: `cmd/mpm-mcp/tools.go` (add `toolAddEvidence` + `handleAddEvidence`)

- [ ] **Step 1: Write the failing test**

Create `internal/call_helpers_test.go`:

```go
package internal

import (
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestCallHelpers_AddEvidence_HappyPath(t *testing.T) {
	dm := newTestDM(t)
	_, err := dm.ExecTracked(`INSERT INTO memories (id, collection, content) VALUES (?, 'memories', 'parser error observed')`, 0, "mem-1")
	require.NoError(t, err)

	out, err := dm.AddEvidence(EvidenceInput{
		ArtifactID:   "mem-1",
		ArtifactType: "memory",
		Type:         "reproduction",
		SourceGroup:  "test-rig-1",
		Strength:     0.85,
		CreatedBy:    "test",
		CreatedAt:    time.Now(),
	})
	require.NoError(t, err)
	assert.Contains(t, out, "success")
	assert.Equal(t, true, out["success"])
	assert.Contains(t, out, "confidence")
	conf, ok := out["confidence"].(float64)
	require.True(t, ok)
	assert.Greater(t, conf, 0.5, "positive evidence should raise confidence above initial 0.8")

	// Evidence row should exist.
	var n int
	require.NoError(t, dm.QueryRowTracked(`SELECT COUNT(*) FROM evidence WHERE artifact_id = ?`, "mem-1").Scan(&n))
	assert.Equal(t, 1, n)
}

func TestCallHelpers_AddEvidence_RejectsInvalidType(t *testing.T) {
	dm := newTestDM(t)
	_, err := dm.ExecTracked(`INSERT INTO memories (id, collection, content) VALUES (?, 'memories', 'x')`, 0, "mem-2")
	require.NoError(t, err)

	_, err = dm.AddEvidence(EvidenceInput{
		ArtifactID:   "mem-2",
		ArtifactType: "memory",
		Type:         "bogus_type",
		SourceGroup:  "test",
		CreatedBy:    "test",
	})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "invalid evidence type")
}

func TestCallHelpers_AddEvidence_RequiresArtifactID(t *testing.T) {
	dm := newTestDM(t)
	_, err := dm.AddEvidence(EvidenceInput{
		ArtifactID:   "",
		ArtifactType: "memory",
		Type:         "reproduction",
		SourceGroup:  "test",
		CreatedBy:    "test",
	})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "artifact_id")
}
```

- [ ] **Step 2: Run tests, verify failure**

```bash
go test -tags fts5 -v ./internal/ -run TestCallHelpers_AddEvidence
```

Expected: compile error `dm.AddEvidence undefined`.

- [ ] **Step 3: Implement `dm.AddEvidence` in `internal/call_helpers.go`**

Add at the bottom of `internal/call_helpers.go`:

```go
// AddEvidence inserts an evidence row and returns the resulting confidence
// for the artifact. Wraps internal.AddEvidence (which performs the
// sensitive-content scan and evidence-type validation) so both the
// `mpm call add_evidence` CLI and the `add_evidence` MCP tool share one
// validation + write path.
//
// Returns the same map the previous callAddEvidence returned:
//   {"success": true, "confidence": <float>}
//
// Required fields: artifact_id, type, source_group, created_by. Returns
// an error if any are missing; underlying evidence type / content
// validation is enforced by internal.AddEvidence.
func (dm *DatabaseManager) AddEvidence(in EvidenceInput) (map[string]interface{}, error) {
	if in.ArtifactID == "" {
		return nil, fmt.Errorf("artifact_id is required")
	}
	if in.Type == "" {
		return nil, fmt.Errorf("type is required")
	}
	if in.SourceGroup == "" {
		return nil, fmt.Errorf("source_group is required")
	}
	if in.CreatedBy == "" {
		return nil, fmt.Errorf("created_by is required")
	}
	if in.ArtifactType == "" {
		in.ArtifactType = "memory"
	}
	// Fill strength from the registry default if the caller passed 0.
	if in.Strength == 0 {
		if def, ok := DefaultStrength(in.Type); ok {
			in.Strength = def
		}
	}
	// Default independence to 1.0 to match the call handler behavior.
	if in.IndependenceFactor == 0 {
		in.IndependenceFactor = 1.0
	}

	if err := AddEvidence(dm, in); err != nil {
		return nil, err
	}

	var conf float64
	if err := dm.QueryRowTracked(
		fmt.Sprintf(`SELECT confidence FROM %s WHERE id = ?`, ArtifactTable(in.ArtifactType)),
		in.ArtifactID,
	).Scan(&conf); err != nil {
		return nil, fmt.Errorf("read confidence: %w", err)
	}
	return map[string]interface{}{
		"success":    true,
		"confidence": conf,
	}, nil
}
```

Add `"time"` to the imports of `internal/call_helpers.go` if not already present.

- [ ] **Step 4: Run tests, verify pass**

```bash
go test -tags fts5 -v ./internal/ -run TestCallHelpers_AddEvidence
```

Expected: PASS (3 tests).

- [ ] **Step 5: Refactor `callAddEvidence` in `cmd/mpm/call.go`**

Replace lines 770-831 (the entire `callAddEvidence` body) with:

```go
// callAddEvidence inserts a new evidence row and returns the resulting
// confidence. Thin shim over dm.AddEvidence.
func callAddEvidence(payload map[string]interface{}) (interface{}, error) {
	artifactType, _ := payload["artifact_type"].(string)
	if artifactType == "" {
		artifactType = "memory"
	}
	var strength float64
	if s, ok := payload["strength"].(float64); ok {
		strength = s
	}
	var independence float64 = 1.0
	if i, ok := payload["independence_factor"].(float64); ok {
		independence = i
	}

	dm, closeDM, err := openCallDM()
	if err != nil {
		return nil, err
	}
	defer closeDM()

	return dm.AddEvidence(EvidenceInput{
		ArtifactID:         stringArg(payload["artifact_id"]),
		ArtifactType:       artifactType,
		Type:               stringArg(payload["type"]),
		SourceGroup:        stringArg(payload["source_group"]),
		Strength:           strength,
		IndependenceFactor: independence,
		CreatedBy:          stringArg(payload["created_by"]),
		CreatedAt:          time.Now(),
		Notes:              stringArg(payload["notes"]),
	})
}
```

Required-arg preflight and `IsValidEvidenceType` check now live in `dm.AddEvidence` (one level up from the call handler). The error messages are preserved.

- [ ] **Step 6: Add `toolAddEvidence` and `handleAddEvidence` in `cmd/mpm-mcp/tools.go`**

In `cmd/mpm-mcp/tools.go`, add after the existing tool specs (anywhere before the handlers section; e.g. after `toolLogToChangelog`):

```go
// ── add_evidence ──────────────────────────────────────────────────────────

func toolAddEvidence() mcp.Tool {
	return mcp.NewTool("add_evidence",
		mcp.WithDescription(
			"Insert a new evidence row and return the resulting confidence for the artifact. "+
				"Required: artifact_id, type, source_group, created_by. Optional: artifact_type "+
				"(default 'memory'), strength (default from type registry), independence_factor "+
				"(default 1.0), notes."),
		mcp.WithString("artifact_id", mcp.Required(), mcp.Description("Artifact this evidence applies to.")),
		mcp.WithString("artifact_type", mcp.Description("Artifact type: 'memory' or 'lesson'. Default 'memory'.")),
		mcp.WithString("type", mcp.Required(), mcp.Enum("observation", "test", "reproduction", "challenge", "decision_outcome", "external_reference"),
			mcp.Description("Evidence type — must be a known v1 type.")),
		mcp.WithString("source_group", mcp.Required(), mcp.Description("Source group label (e.g. 'user-X', 'test-rig-1').")),
		mcp.WithNumber("strength", mcp.Description("Evidence strength 0–1; default from the type registry.")),
		mcp.WithNumber("independence_factor", mcp.DefaultNumber(1.0), mcp.Description("Independence factor 0–1 (default 1.0).")),
		mcp.WithString("created_by", mcp.Required(), mcp.Description("Who/what created this evidence.")),
		mcp.WithString("notes", mcp.Description("Optional free-text notes.")),
	)
}

func handleAddEvidence(dm *internal.DatabaseManager) server.ToolHandlerFunc {
	return func(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
		args := req.GetArguments()
		artifactID, _ := args["artifact_id"].(string)
		if artifactID == "" {
			return mcp.NewToolResultError("artifact_id is required"), nil
		}
		evType, _ := args["type"].(string)
		if evType == "" {
			return mcp.NewToolResultError("type is required"), nil
		}
		source, _ := args["source_group"].(string)
		if source == "" {
			return mcp.NewToolResultError("source_group is required"), nil
		}
		createdBy, _ := args["created_by"].(string)
		if createdBy == "" {
			return mcp.NewToolResultError("created_by is required"), nil
		}
		artifactType, _ := args["artifact_type"].(string)
		var strength float64
		if s, ok := args["strength"].(float64); ok {
			strength = s
		}
		var independence float64 = 1.0
		if i, ok := args["independence_factor"].(float64); ok {
			independence = i
		}
		notes, _ := args["notes"].(string)

		out, err := dm.AddEvidence(internal.EvidenceInput{
			ArtifactID:         artifactID,
			ArtifactType:       artifactType,
			Type:               evType,
			SourceGroup:        source,
			Strength:           strength,
			IndependenceFactor: independence,
			CreatedBy:          createdBy,
			CreatedAt:          time.Now(),
			Notes:              notes,
		})
		if err != nil {
			return mcp.NewToolResultErrorFromErr("add_evidence failed", err), nil
		}
		return jsonResult(out), nil
	}
}
```

Add `"time"` to the imports of `cmd/mpm-mcp/tools.go` if not already present (it isn't — `tools.go` currently imports only `context`, `encoding/json`, `fmt`, `strings`, plus the mcp and internal packages).

- [ ] **Step 7: Register the tool in `RegisterAllTools`**

In `cmd/mpm-mcp/tools.go` line 62 (the `s.AddTool` block), add a new line just before the closing `}`:

```go
	s.AddTool(toolAddEvidence(), handleAddEvidence(dm))
```

Place it after `s.AddTool(toolLogToChangelog(), handleLogToChangelog(dm))` to keep the tools grouped logically.

- [ ] **Step 8: Build both binaries**

```bash
go build -tags fts5 ./cmd/mpm
go build -tags fts5 ./cmd/mpm-mcp
```

Expected: both compile with no errors.

- [ ] **Step 9: Run dm tests, verify pass**

```bash
go test -tags fts5 -v ./internal/ -run TestCallHelpers_AddEvidence
```

Expected: PASS (3 tests).

- [ ] **Step 10: Commit**

```bash
git add internal/call_helpers.go internal/call_helpers_test.go cmd/mpm/call.go cmd/mpm-mcp/tools.go
git commit -m "feat(mcp): add_evidence — wire evidence validation through dm.AddEvidence

Lifts callAddEvidence's required-arg preflight and strength default into
the new dm.AddEvidence method, which calls internal.AddEvidence (the
deeper validation layer) and reads back confidence. Both CLI and MCP
share the new path. CLI output shape is preserved byte-for-byte.

Co-Authored-By: Claude Opus 4.7 <noreply@anthropic.com>"
```

---

## Task 3: `dm.ListEvidence` + MCP `list_evidence`

**Files:**
- Modify: `internal/call_helpers.go` (add `ListEvidence` method)
- Modify: `internal/call_helpers_test.go` (add `ListEvidence` tests)
- Modify: `cmd/mpm/call.go` (refactor `callListEvidence` lines 834-877)
- Modify: `cmd/mpm-mcp/tools.go` (add `toolListEvidence` + `handleListEvidence`)

- [ ] **Step 1: Add failing tests**

Append to `internal/call_helpers_test.go`:

```go
func TestCallHelpers_ListEvidence_EmptyArtifact(t *testing.T) {
	dm := newTestDM(t)
	_, err := dm.ExecTracked(`INSERT INTO memories (id, collection, content) VALUES (?, 'memories', 'x')`, 0, "mem-1")
	require.NoError(t, err)

	items, err := dm.ListEvidence("mem-1", "memory")
	require.NoError(t, err)
	assert.Empty(t, items)
}

func TestCallHelpers_ListEvidence_FiltersByType(t *testing.T) {
	dm := newTestDM(t)
	_, err := dm.ExecTracked(`INSERT INTO memories (id, collection, content) VALUES (?, 'memories', 'x')`, 0, "mem-1")
	require.NoError(t, err)
	_, err = dm.ExecTracked(`INSERT INTO lessons (id, collection, content) VALUES (?, 'lessons', 'y')`, 0, "les-1")
	require.NoError(t, err)
	require.NoError(t, AddEvidence(dm, EvidenceInput{ArtifactID: "mem-1", ArtifactType: "memory", Type: "observation", SourceGroup: "g", Strength: 0.4, CreatedBy: "t", CreatedAt: time.Now()}))
	require.NoError(t, AddEvidence(dm, EvidenceInput{ArtifactID: "les-1", ArtifactType: "lesson", Type: "observation", SourceGroup: "g", Strength: 0.4, CreatedBy: "t", CreatedAt: time.Now()}))

	memItems, err := dm.ListEvidence("mem-1", "memory")
	require.NoError(t, err)
	assert.Len(t, memItems, 1)
	assert.Equal(t, "mem-1", memItems[0]["artifact_id"])
}

func TestCallHelpers_ListEvidence_RequiresArtifactID(t *testing.T) {
	dm := newTestDM(t)
	_, err := dm.ListEvidence("", "memory")
	require.Error(t, err)
	assert.Contains(t, err.Error(), "artifact_id")
}
```

- [ ] **Step 2: Run tests, verify failure**

```bash
go test -tags fts5 -v ./internal/ -run TestCallHelpers_ListEvidence
```

Expected: compile error `dm.ListEvidence undefined`.

- [ ] **Step 3: Implement `dm.ListEvidence` in `internal/call_helpers.go`**

Add after `AddEvidence`:

```go
// ListEvidence returns all evidence rows for an artifact, newest first.
// Returns the list slice (empty if none) under the "evidence" key of the
// result map — same shape as the previous callListEvidence.
//
// Required: artifact_id. Optional: artifact_type (default "memory").
func (dm *DatabaseManager) ListEvidence(artifactID, artifactType string) (map[string]interface{}, error) {
	if artifactID == "" {
		return nil, fmt.Errorf("artifact_id is required")
	}
	if artifactType == "" {
		artifactType = "memory"
	}
	rows, err := dm.QueryTracked(`
		SELECT id, artifact_id, artifact_type, type, source_group, strength,
		       independence_factor, created_by, created_at, expires_at, notes
		FROM evidence
		WHERE artifact_id = ? AND artifact_type = ?
		ORDER BY created_at DESC
	`, artifactID, artifactType)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []map[string]interface{}{}
	for rows.Next() {
		var id, aid, atype, t, src, by, notes string
		var strength, ind float64
		var createdAt int64
		var expiresAt *int64
		if err := rows.Scan(&id, &aid, &atype, &t, &src, &strength, &ind, &by, &createdAt, &expiresAt, &notes); err != nil {
			return nil, err
		}
		row := map[string]interface{}{
			"id": id, "artifact_id": aid, "artifact_type": atype,
			"type": t, "source_group": src, "strength": strength,
			"independence_factor": ind, "created_by": by,
			"created_at": createdAt, "notes": notes,
		}
		if expiresAt != nil {
			row["expires_at"] = *expiresAt
		}
		out = append(out, row)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	return map[string]interface{}{"evidence": out}, nil
}
```

- [ ] **Step 4: Run tests, verify pass**

```bash
go test -tags fts5 -v ./internal/ -run TestCallHelpers_ListEvidence
```

Expected: PASS (3 tests).

- [ ] **Step 5: Refactor `callListEvidence` in `cmd/mpm/call.go`**

Replace lines 834-877 (the entire `callListEvidence` body) with:

```go
// callListEvidence returns all evidence rows for an artifact.
func callListEvidence(payload map[string]interface{}) (interface{}, error) {
	dm, closeDM, err := openCallDM()
	if err != nil {
		return nil, err
	}
	defer closeDM()
	return dm.ListEvidence(stringArg(payload["artifact_id"]), stringArg(payload["artifact_type"]))
}
```

- [ ] **Step 6: Add `toolListEvidence` and `handleListEvidence` in `cmd/mpm-mcp/tools.go`**

```go
// ── list_evidence ─────────────────────────────────────────────────────────

func toolListEvidence() mcp.Tool {
	return mcp.NewTool("list_evidence",
		mcp.WithDescription("List all evidence rows for an artifact, newest first."),
		mcp.WithString("artifact_id", mcp.Required(), mcp.Description("Artifact ID to list evidence for.")),
		mcp.WithString("artifact_type", mcp.Description("Artifact type: 'memory' or 'lesson'. Default 'memory'.")),
	)
}

func handleListEvidence(dm *internal.DatabaseManager) server.ToolHandlerFunc {
	return func(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
		args := req.GetArguments()
		artifactID, _ := args["artifact_id"].(string)
		if artifactID == "" {
			return mcp.NewToolResultError("artifact_id is required"), nil
		}
		artifactType, _ := args["artifact_type"].(string)
		out, err := dm.ListEvidence(artifactID, artifactType)
		if err != nil {
			return mcp.NewToolResultErrorFromErr("list_evidence failed", err), nil
		}
		return jsonResult(out), nil
	}
}
```

- [ ] **Step 7: Register the tool**

In `cmd/mpm-mcp/tools.go` `RegisterAllTools`, add:

```go
	s.AddTool(toolListEvidence(), handleListEvidence(dm))
```

Place it right after `s.AddTool(toolAddEvidence(), handleAddEvidence(dm))`.

- [ ] **Step 8: Build both binaries**

```bash
go build -tags fts5 ./cmd/mpm
go build -tags fts5 ./cmd/mpm-mcp
```

Expected: both compile.

- [ ] **Step 9: Commit**

```bash
git add internal/call_helpers.go internal/call_helpers_test.go cmd/mpm/call.go cmd/mpm-mcp/tools.go
git commit -m "feat(mcp): list_evidence — share dm.ListEvidence between CLI and MCP

Co-Authored-By: Claude Opus 4.7 <noreply@anthropic.com>"
```

---

## Task 4: `dm.QueryConfidenceHistory` + MCP `query_confidence_history`

**Files:**
- Modify: `internal/call_helpers.go` (add `QueryConfidenceHistory`)
- Modify: `internal/call_helpers_test.go` (add tests)
- Modify: `cmd/mpm/call.go` (refactor `callQueryConfidenceHistory` lines 880-929)
- Modify: `cmd/mpm-mcp/tools.go` (add `toolQueryConfidenceHistory` + `handleQueryConfidenceHistory`)

- [ ] **Step 1: Add failing tests**

Append to `internal/call_helpers_test.go`:

```go
func TestCallHelpers_QueryConfidenceHistory_DefaultLimit(t *testing.T) {
	dm := newTestDM(t)
	_, err := dm.ExecTracked(`INSERT INTO memories (id, collection, content) VALUES (?, 'memories', 'x')`, 0, "mem-1")
	require.NoError(t, err)

	// Insert 60 history rows to exceed the default limit.
	for i := 0; i < 60; i++ {
		_, err := dm.ExecTracked(`INSERT INTO confidence_history (artifact_id, artifact_type, computed_at, confidence, evidence_count, trigger) VALUES (?, 'memory', ?, 0.5, 0, 'manual_recompute')`, 0, "mem-1", int64(1700000000+i))
		require.NoError(t, err)
	}

	out, err := dm.QueryConfidenceHistory("mem-1", "memory", 0)
	require.NoError(t, err)
	hist, ok := out["history"].([]map[string]interface{})
	require.True(t, ok)
	assert.Len(t, hist, 50, "default limit is 50")
}

func TestCallHelpers_QueryConfidenceHistory_CustomLimit(t *testing.T) {
	dm := newTestDM(t)
	_, err := dm.ExecTracked(`INSERT INTO memories (id, collection, content) VALUES (?, 'memories', 'x')`, 0, "mem-1")
	require.NoError(t, err)
	for i := 0; i < 5; i++ {
		_, err := dm.ExecTracked(`INSERT INTO confidence_history (artifact_id, artifact_type, computed_at, confidence, evidence_count, trigger) VALUES (?, 'memory', ?, 0.5, 0, 'manual_recompute')`, 0, "mem-1", int64(1700000000+i))
		require.NoError(t, err)
	}

	out, err := dm.QueryConfidenceHistory("mem-1", "memory", 2)
	require.NoError(t, err)
	hist, _ := out["history"].([]map[string]interface{})
	assert.Len(t, hist, 2)
}

func TestCallHelpers_QueryConfidenceHistory_RequiresArtifactID(t *testing.T) {
	dm := newTestDM(t)
	_, err := dm.QueryConfidenceHistory("", "memory", 10)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "artifact_id")
}
```

- [ ] **Step 2: Run tests, verify failure**

```bash
go test -tags fts5 -v ./internal/ -run TestCallHelpers_QueryConfidenceHistory
```

Expected: compile error `dm.QueryConfidenceHistory undefined`.

- [ ] **Step 3: Implement `dm.QueryConfidenceHistory` in `internal/call_helpers.go`**

```go
// QueryConfidenceHistory returns the confidence timeline for an artifact,
// newest first. limit <= 0 defaults to 50.
//
// Returns the rows under the "history" key — same shape as the previous
// callQueryConfidenceHistory.
func (dm *DatabaseManager) QueryConfidenceHistory(artifactID, artifactType string, limit int) (map[string]interface{}, error) {
	if artifactID == "" {
		return nil, fmt.Errorf("artifact_id is required")
	}
	if artifactType == "" {
		artifactType = "memory"
	}
	if limit <= 0 {
		limit = 50
	}
	rows, err := dm.QueryTracked(`
		SELECT computed_at, confidence, evidence_count, trigger
		FROM confidence_history
		WHERE artifact_id = ? AND artifact_type = ?
		ORDER BY computed_at DESC
		LIMIT ?
	`, artifactID, artifactType, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []map[string]interface{}{}
	for rows.Next() {
		var computedAt int64
		var conf float64
		var evidenceCount int
		var trigger string
		if err := rows.Scan(&computedAt, &conf, &evidenceCount, &trigger); err != nil {
			return nil, err
		}
		out = append(out, map[string]interface{}{
			"computed_at": computedAt, "confidence": conf,
			"evidence_count": evidenceCount, "trigger": trigger,
		})
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	return map[string]interface{}{"history": out}, nil
}
```

- [ ] **Step 4: Run tests, verify pass**

```bash
go test -tags fts5 -v ./internal/ -run TestCallHelpers_QueryConfidenceHistory
```

Expected: PASS (3 tests).

- [ ] **Step 5: Refactor `callQueryConfidenceHistory` in `cmd/mpm/call.go`**

Replace lines 880-929 with:

```go
// callQueryConfidenceHistory returns the confidence timeline for an artifact.
func callQueryConfidenceHistory(payload map[string]interface{}) (interface{}, error) {
	limit := 50
	if l, ok := payload["limit"].(float64); ok && l > 0 {
		limit = int(l)
	}
	dm, closeDM, err := openCallDM()
	if err != nil {
		return nil, err
	}
	defer closeDM()
	return dm.QueryConfidenceHistory(stringArg(payload["artifact_id"]), stringArg(payload["artifact_type"]), limit)
}
```

- [ ] **Step 6: Add `toolQueryConfidenceHistory` and `handleQueryConfidenceHistory` in `cmd/mpm-mcp/tools.go`**

```go
// ── query_confidence_history ──────────────────────────────────────────────

func toolQueryConfidenceHistory() mcp.Tool {
	return mcp.NewTool("query_confidence_history",
		mcp.WithDescription("Return the confidence timeline for an artifact, newest first."),
		mcp.WithString("artifact_id", mcp.Required(), mcp.Description("Artifact ID to query.")),
		mcp.WithString("artifact_type", mcp.Description("Artifact type: 'memory' or 'lesson'. Default 'memory'.")),
		mcp.WithNumber("limit", mcp.DefaultNumber(50), mcp.Description("Max rows to return. Default 50.")),
	)
}

func handleQueryConfidenceHistory(dm *internal.DatabaseManager) server.ToolHandlerFunc {
	return func(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
		args := req.GetArguments()
		artifactID, _ := args["artifact_id"].(string)
		if artifactID == "" {
			return mcp.NewToolResultError("artifact_id is required"), nil
		}
		artifactType, _ := args["artifact_type"].(string)
		limit := int(parseNum(args["limit"], 50))
		out, err := dm.QueryConfidenceHistory(artifactID, artifactType, limit)
		if err != nil {
			return mcp.NewToolResultErrorFromErr("query_confidence_history failed", err), nil
		}
		return jsonResult(out), nil
	}
}
```

- [ ] **Step 7: Register the tool**

In `cmd/mpm-mcp/tools.go` `RegisterAllTools`, add:

```go
	s.AddTool(toolQueryConfidenceHistory(), handleQueryConfidenceHistory(dm))
```

- [ ] **Step 8: Build both binaries**

```bash
go build -tags fts5 ./cmd/mpm
go build -tags fts5 ./cmd/mpm-mcp
```

Expected: both compile.

- [ ] **Step 9: Commit**

```bash
git add internal/call_helpers.go internal/call_helpers_test.go cmd/mpm/call.go cmd/mpm-mcp/tools.go
git commit -m "feat(mcp): query_confidence_history — share dm method

Co-Authored-By: Claude Opus 4.7 <noreply@anthropic.com>"
```

---

## Task 5: `dm.QueryConfidenceChanges` + MCP `query_confidence_changes`

**Files:**
- Modify: `internal/call_helpers.go` (add `QueryConfidenceChanges`)
- Modify: `internal/call_helpers_test.go` (add tests)
- Modify: `cmd/mpm/call.go` (refactor `callQueryConfidenceChanges` lines 941-975)
- Modify: `cmd/mpm-mcp/tools.go` (add `toolQueryConfidenceChanges` + `handleQueryConfidenceChanges`)

- [ ] **Step 1: Add failing tests**

Append to `internal/call_helpers_test.go`:

```go
func TestCallHelpers_QueryConfidenceChanges_SinceSecondsAgo(t *testing.T) {
	dm := newTestDM(t)
	out, err := dm.QueryConfidenceChanges(ConfidenceChangesFilter{
		Since: time.Now().Add(-1 * time.Hour),
		Limit: 10,
	})
	require.NoError(t, err)
	assert.Contains(t, out, "changes")
	assert.Contains(t, out, "count")
}

func TestCallHelpers_QueryConfidenceChanges_FilterByArtifact(t *testing.T) {
	dm := newTestDM(t)
	_, err := dm.ExecTracked(`INSERT INTO memories (id, collection, content) VALUES (?, 'memories', 'x')`, 0, "mem-1")
	require.NoError(t, err)

	out, err := dm.QueryConfidenceChanges(ConfidenceChangesFilter{
		ArtifactID:   "mem-1",
		ArtifactType: "memory",
		Limit:        10,
	})
	require.NoError(t, err)
	changes, _ := out["changes"].([]map[string]interface{})
	for _, c := range changes {
		assert.Equal(t, "mem-1", c["artifact_id"])
	}
}
```

- [ ] **Step 2: Run tests, verify failure**

```bash
go test -tags fts5 -v ./internal/ -run TestCallHelpers_QueryConfidenceChanges
```

Expected: compile error.

- [ ] **Step 3: Implement `dm.QueryConfidenceChanges` in `internal/call_helpers.go`**

```go
// QueryConfidenceChanges returns recent confidence-altering events with
// delta and trigger. Wraps internal.QueryConfidenceChanges.
//
// Returns: {"changes": [...], "count": N} — same shape as the previous
// callQueryConfidenceChanges.
func (dm *DatabaseManager) QueryConfidenceChanges(filter ConfidenceChangesFilter) (map[string]interface{}, error) {
	changes, err := QueryConfidenceChanges(dm, filter)
	if err != nil {
		return nil, fmt.Errorf("query confidence changes: %w", err)
	}
	return map[string]interface{}{
		"changes": changes,
		"count":   len(changes),
	}, nil
}
```

- [ ] **Step 4: Run tests, verify pass**

```bash
go test -tags fts5 -v ./internal/ -run TestCallHelpers_QueryConfidenceChanges
```

Expected: PASS.

- [ ] **Step 5: Refactor `callQueryConfidenceChanges` in `cmd/mpm/call.go`**

Replace lines 941-975 with:

```go
// callQueryConfidenceChanges returns recent confidence-altering events
// with delta and trigger.
func callQueryConfidenceChanges(payload map[string]interface{}) (interface{}, error) {
	var filter internal.ConfidenceChangesFilter
	if secs, ok := payload["since_seconds_ago"].(float64); ok && secs > 0 {
		filter.Since = time.Now().Add(-time.Duration(secs) * time.Second)
	} else if sinceF, ok := payload["since"].(float64); ok && sinceF > 0 {
		filter.Since = time.Unix(int64(sinceF), 0)
	}
	if l, ok := payload["limit"].(float64); ok && l > 0 {
		filter.Limit = int(l)
	}
	if v, ok := payload["artifact_id"].(string); ok {
		filter.ArtifactID = v
	}
	if v, ok := payload["artifact_type"].(string); ok {
		filter.ArtifactType = v
	}

	dm, closeDM, err := openCallDM()
	if err != nil {
		return nil, err
	}
	defer closeDM()
	return dm.QueryConfidenceChanges(filter)
}
```

- [ ] **Step 6: Add `toolQueryConfidenceChanges` and `handleQueryConfidenceChanges` in `cmd/mpm-mcp/tools.go`**

```go
// ── query_confidence_changes ──────────────────────────────────────────────

func toolQueryConfidenceChanges() mcp.Tool {
	return mcp.NewTool("query_confidence_changes",
		mcp.WithDescription("Return recent confidence-altering events with delta and trigger. "+
			"Distinct from query_confidence_history (full timeline): this answers 'what moved, by how much, and why, since when?'"),
		mcp.WithNumber("since_seconds_ago", mcp.Description("Look back N seconds. Alternative to `since`.")),
		mcp.WithNumber("since", mcp.Description("Unix timestamp cutoff. Default: last 24h.")),
		mcp.WithNumber("limit", mcp.DefaultNumber(50), mcp.Description("Max rows. Default 50.")),
		mcp.WithString("artifact_id", mcp.Description("Filter to a single artifact.")),
		mcp.WithString("artifact_type", mcp.Description("Filter to a single artifact type.")),
	)
}

func handleQueryConfidenceChanges(dm *internal.DatabaseManager) server.ToolHandlerFunc {
	return func(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
		args := req.GetArguments()
		var filter internal.ConfidenceChangesFilter
		if secs, ok := args["since_seconds_ago"].(float64); ok && secs > 0 {
			filter.Since = time.Now().Add(-time.Duration(secs) * time.Second)
		} else if sinceF, ok := args["since"].(float64); ok && sinceF > 0 {
			filter.Since = time.Unix(int64(sinceF), 0)
		}
		if l, ok := args["limit"].(float64); ok && l > 0 {
			filter.Limit = int(l)
		}
		if v, ok := args["artifact_id"].(string); ok {
			filter.ArtifactID = v
		}
		if v, ok := args["artifact_type"].(string); ok {
			filter.ArtifactType = v
		}
		out, err := dm.QueryConfidenceChanges(filter)
		if err != nil {
			return mcp.NewToolResultErrorFromErr("query_confidence_changes failed", err), nil
		}
		return jsonResult(out), nil
	}
}
```

- [ ] **Step 7: Register the tool**

In `cmd/mpm-mcp/tools.go` `RegisterAllTools`, add:

```go
	s.AddTool(toolQueryConfidenceChanges(), handleQueryConfidenceChanges(dm))
```

- [ ] **Step 8: Build both binaries**

```bash
go build -tags fts5 ./cmd/mpm
go build -tags fts5 ./cmd/mpm-mcp
```

- [ ] **Step 9: Commit**

```bash
git add internal/call_helpers.go internal/call_helpers_test.go cmd/mpm/call.go cmd/mpm-mcp/tools.go
git commit -m "feat(mcp): query_confidence_changes — share dm method

Co-Authored-By: Claude Opus 4.7 <noreply@anthropic.com>"
```

---

## Task 6: `dm.QueryConfidenceTrend` + MCP `query_confidence_trend`

**Files:**
- Modify: `internal/call_helpers.go`
- Modify: `internal/call_helpers_test.go`
- Modify: `cmd/mpm/call.go`
- Modify: `cmd/mpm-mcp/tools.go`

- [ ] **Step 1: Add failing tests**

Append to `internal/call_helpers_test.go`:

```go
func TestCallHelpers_QueryConfidenceTrend_HappyPath(t *testing.T) {
	dm := newTestDM(t)
	_, err := dm.ExecTracked(`INSERT INTO memories (id, collection, content) VALUES (?, 'memories', 'x')`, 0, "mem-1")
	require.NoError(t, err)

	out, err := dm.QueryConfidenceTrend("mem-1", "memory", 30)
	require.NoError(t, err)
	assert.Contains(t, out, "trend")
}

func TestCallHelpers_QueryConfidenceTrend_DefaultWindow(t *testing.T) {
	dm := newTestDM(t)
	_, err := dm.ExecTracked(`INSERT INTO memories (id, collection, content) VALUES (?, 'memories', 'x')`, 0, "mem-1")
	require.NoError(t, err)

	out, err := dm.QueryConfidenceTrend("mem-1", "memory", 0)
	require.NoError(t, err)
	_, ok := out["trend"]
	assert.True(t, ok)
}

func TestCallHelpers_QueryConfidenceTrend_RequiresArtifactID(t *testing.T) {
	dm := newTestDM(t)
	_, err := dm.QueryConfidenceTrend("", "memory", 30)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "artifact_id")
}
```

- [ ] **Step 2: Run tests, verify failure**

```bash
go test -tags fts5 -v ./internal/ -run TestCallHelpers_QueryConfidenceTrend
```

Expected: compile error.

- [ ] **Step 3: Implement `dm.QueryConfidenceTrend` in `internal/call_helpers.go`**

```go
// QueryConfidenceTrend returns the trajectory projection of confidence
// over a time window. windowDays <= 0 defaults to 30.
//
// Returns: {"success": true, "trend": <ConfidenceTrend>}.
func (dm *DatabaseManager) QueryConfidenceTrend(artifactID, artifactType string, windowDays int) (map[string]interface{}, error) {
	if artifactID == "" {
		return nil, fmt.Errorf("artifact_id is required")
	}
	if artifactType == "" {
		artifactType = "memory"
	}
	if windowDays <= 0 {
		windowDays = 30
	}
	trend, err := QueryConfidenceTrend(dm, artifactID, artifactType, windowDays)
	if err != nil {
		return nil, fmt.Errorf("query confidence trend: %w", err)
	}
	return map[string]interface{}{
		"success": true,
		"trend":   trend,
	}, nil
}
```

- [ ] **Step 4: Run tests, verify pass**

```bash
go test -tags fts5 -v ./internal/ -run TestCallHelpers_QueryConfidenceTrend
```

Expected: PASS.

- [ ] **Step 5: Refactor `callQueryConfidenceTrend` in `cmd/mpm/call.go`**

Replace lines 988-1016 with:

```go
// callQueryConfidenceTrend returns the trajectory projection of confidence
// over a time window.
func callQueryConfidenceTrend(payload map[string]interface{}) (interface{}, error) {
	dm, closeDM, err := openCallDM()
	if err != nil {
		return nil, err
	}
	defer closeDM()
	windowDays := 30
	if w, ok := payload["window_days"].(float64); ok && w > 0 {
		windowDays = int(w)
	}
	return dm.QueryConfidenceTrend(stringArg(payload["artifact_id"]), stringArg(payload["artifact_type"]), windowDays)
}
```

- [ ] **Step 6: Add `toolQueryConfidenceTrend` and `handleQueryConfidenceTrend` in `cmd/mpm-mcp/tools.go`**

```go
// ── query_confidence_trend ────────────────────────────────────────────────

func toolQueryConfidenceTrend() mcp.Tool {
	return mcp.NewTool("query_confidence_trend",
		mcp.WithDescription("Return the trajectory projection of confidence over a time window "+
			"(velocity + trend label). Complements query_confidence_history and "+
			"query_confidence_changes."),
		mcp.WithString("artifact_id", mcp.Required(), mcp.Description("Artifact ID to query.")),
		mcp.WithString("artifact_type", mcp.Description("Artifact type: 'memory' or 'lesson'. Default 'memory'.")),
		mcp.WithNumber("window_days", mcp.DefaultNumber(30), mcp.Description("Window size in days. Default 30.")),
	)
}

func handleQueryConfidenceTrend(dm *internal.DatabaseManager) server.ToolHandlerFunc {
	return func(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
		args := req.GetArguments()
		artifactID, _ := args["artifact_id"].(string)
		if artifactID == "" {
			return mcp.NewToolResultError("artifact_id is required"), nil
		}
		artifactType, _ := args["artifact_type"].(string)
		windowDays := int(parseNum(args["window_days"], 30))
		out, err := dm.QueryConfidenceTrend(artifactID, artifactType, windowDays)
		if err != nil {
			return mcp.NewToolResultErrorFromErr("query_confidence_trend failed", err), nil
		}
		return jsonResult(out), nil
	}
}
```

- [ ] **Step 7: Register the tool**

In `cmd/mpm-mcp/tools.go` `RegisterAllTools`, add:

```go
	s.AddTool(toolQueryConfidenceTrend(), handleQueryConfidenceTrend(dm))
```

- [ ] **Step 8: Build both binaries**

```bash
go build -tags fts5 ./cmd/mpm
go build -tags fts5 ./cmd/mpm-mcp
```

- [ ] **Step 9: Commit**

```bash
git add internal/call_helpers.go internal/call_helpers_test.go cmd/mpm/call.go cmd/mpm-mcp/tools.go
git commit -m "feat(mcp): query_confidence_trend — share dm method

Co-Authored-By: Claude Opus 4.7 <noreply@anthropic.com>"
```

---

## Task 7: `dm.QueryMemoryQuality` + MCP `query_memory_quality`

**Files:**
- Modify: `internal/call_helpers.go`
- Modify: `internal/call_helpers_test.go`
- Modify: `cmd/mpm/call.go`
- Modify: `cmd/mpm-mcp/tools.go`

- [ ] **Step 1: Add failing tests**

Append to `internal/call_helpers_test.go`:

```go
func TestCallHelpers_QueryMemoryQuality_ReturnsPerSource(t *testing.T) {
	dm := newTestDM(t)
	_, err := dm.ExecTracked(`INSERT INTO memories (id, collection, content, metadata) VALUES (?, 'memories', 'x', ?)`, 0, "mem-1", `{"provenance":{"model":"gpt-4o"}}`)
	require.NoError(t, err)

	out, err := dm.QueryMemoryQuality()
	require.NoError(t, err)
	assert.Contains(t, out, "sources")
	assert.Contains(t, out, "count")
}
```

- [ ] **Step 2: Run tests, verify failure**

```bash
go test -tags fts5 -v ./internal/ -run TestCallHelpers_QueryMemoryQuality
```

Expected: compile error.

- [ ] **Step 3: Implement `dm.QueryMemoryQuality` in `internal/call_helpers.go`**

```go
// QueryMemoryQuality returns per-creator memory statistics. Surfaces
// which models/agents produce memories that survive.
//
// Returns: {"success": true, "sources": [...], "count": N}.
func (dm *DatabaseManager) QueryMemoryQuality() (map[string]interface{}, error) {
	stats, err := QueryMemoryQualityBySource(dm)
	if err != nil {
		return nil, fmt.Errorf("query memory quality: %w", err)
	}
	return map[string]interface{}{
		"success": true,
		"sources": stats,
		"count":   len(stats),
	}, nil
}
```

- [ ] **Step 4: Run tests, verify pass**

```bash
go test -tags fts5 -v ./internal/ -run TestCallHelpers_QueryMemoryQuality
```

Expected: PASS.

- [ ] **Step 5: Refactor `callQueryMemoryQuality` in `cmd/mpm/call.go`**

Replace lines 1022-1038 with:

```go
// callQueryMemoryQuality returns per-creator memory statistics.
func callQueryMemoryQuality(payload map[string]interface{}) (interface{}, error) {
	dm, closeDM, err := openCallDM()
	if err != nil {
		return nil, err
	}
	defer closeDM()
	return dm.QueryMemoryQuality()
}
```

- [ ] **Step 6: Add `toolQueryMemoryQuality` and `handleQueryMemoryQuality` in `cmd/mpm-mcp/tools.go`**

```go
// ── query_memory_quality ─────────────────────────────────────────────────

func toolQueryMemoryQuality() mcp.Tool {
	return mcp.NewTool("query_memory_quality",
		mcp.WithDescription("Return per-creator memory statistics — which models/agents "+
			"produce memories that survive. Driven by the memory_source_evidence_ai trigger "+
			"that auto-attributes each new memory to its writer."),
	)
}

func handleQueryMemoryQuality(dm *internal.DatabaseManager) server.ToolHandlerFunc {
	return func(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
		out, err := dm.QueryMemoryQuality()
		if err != nil {
			return mcp.NewToolResultErrorFromErr("query_memory_quality failed", err), nil
		}
		return jsonResult(out), nil
	}
}
```

- [ ] **Step 7: Register the tool**

In `cmd/mpm-mcp/tools.go` `RegisterAllTools`, add:

```go
	s.AddTool(toolQueryMemoryQuality(), handleQueryMemoryQuality(dm))
```

- [ ] **Step 8: Build both binaries**

```bash
go build -tags fts5 ./cmd/mpm
go build -tags fts5 ./cmd/mpm-mcp
```

- [ ] **Step 9: Commit**

```bash
git add internal/call_helpers.go internal/call_helpers_test.go cmd/mpm/call.go cmd/mpm-mcp/tools.go
git commit -m "feat(mcp): query_memory_quality — share dm method

Co-Authored-By: Claude Opus 4.7 <noreply@anthropic.com>"
```

---

## Task 8: `dm.ShowConfidence` + MCP `show_confidence`

**Files:**
- Modify: `internal/call_helpers.go`
- Modify: `internal/call_helpers_test.go`
- Modify: `cmd/mpm/call.go`
- Modify: `cmd/mpm-mcp/tools.go`

This is the composed tool. The result shape is `{current, history: {history: [...]}}` — the inner `history` map is itself the result of `dm.QueryConfidenceHistory`. The byte-for-byte shape must match the previous `callShowConfidence`.

- [ ] **Step 1: Add failing tests**

Append to `internal/call_helpers_test.go`:

```go
func TestCallHelpers_ShowConfidence_NestedHistoryShape(t *testing.T) {
	dm := newTestDM(t)
	_, err := dm.ExecTracked(`INSERT INTO memories (id, collection, content) VALUES (?, 'memories', 'x')`, 0, "mem-1")
	require.NoError(t, err)
	_, err = dm.ExecTracked(`INSERT INTO confidence_history (artifact_id, artifact_type, computed_at, confidence, evidence_count, trigger) VALUES (?, 'memory', 1700000000, 0.5, 0, 'manual_recompute')`, 0, "mem-1")
	require.NoError(t, err)

	out, err := dm.ShowConfidence("mem-1", "memory")
	require.NoError(t, err)
	assert.Contains(t, out, "current")
	_, hasCurrent := out["current"].(float64)
	assert.True(t, hasCurrent, "current must be a float64")
	hist, hasHist := out["history"].(map[string]interface{})
	assert.True(t, hasHist, "history must be a nested map (NOT a slice)")
	_, hasInnerHist := hist["history"].([]map[string]interface{})
	assert.True(t, hasInnerHist, "history.history must be the rows slice")
}
```

- [ ] **Step 2: Run tests, verify failure**

```bash
go test -tags fts5 -v ./internal/ -run TestCallHelpers_ShowConfidence
```

Expected: compile error.

- [ ] **Step 3: Implement `dm.ShowConfidence` in `internal/call_helpers.go`**

```go
// ShowConfidence returns the current confidence and history for an artifact.
//
// The result shape is {"current": <float>, "history": {"history": [...]}} —
// the nested "history" map is the result of QueryConfidenceHistory, which
// is itself wrapped in a {"history": rows} map. This double-nesting is
// load-bearing: existing CLI callers and tests parse `result.history.history`.
// Do not flatten it.
func (dm *DatabaseManager) ShowConfidence(artifactID, artifactType string) (map[string]interface{}, error) {
	if artifactID == "" {
		return nil, fmt.Errorf("artifact_id is required")
	}
	if artifactType == "" {
		artifactType = "memory"
	}
	var conf float64
	if err := dm.QueryRowTracked(
		fmt.Sprintf(`SELECT confidence FROM %s WHERE id = ?`, ArtifactTable(artifactType)),
		artifactID,
	).Scan(&conf); err != nil {
		return nil, fmt.Errorf("read confidence: %w", err)
	}
	hist, err := dm.QueryConfidenceHistory(artifactID, artifactType, 50)
	if err != nil {
		return nil, err
	}
	return map[string]interface{}{
		"current": conf,
		"history": hist,
	}, nil
}
```

- [ ] **Step 4: Run tests, verify pass**

```bash
go test -tags fts5 -v ./internal/ -run TestCallHelpers_ShowConfidence
```

Expected: PASS.

- [ ] **Step 5: Refactor `callShowConfidence` in `cmd/mpm/call.go`**

Replace lines 1041-1073 with:

```go
// callShowConfidence returns the current confidence and history for an artifact.
func callShowConfidence(payload map[string]interface{}) (interface{}, error) {
	dm, closeDM, err := openCallDM()
	if err != nil {
		return nil, err
	}
	defer closeDM()
	return dm.ShowConfidence(stringArg(payload["artifact_id"]), stringArg(payload["artifact_type"]))
}
```

- [ ] **Step 6: Add `toolShowConfidence` and `handleShowConfidence` in `cmd/mpm-mcp/tools.go`**

```go
// ── show_confidence ───────────────────────────────────────────────────────

func toolShowConfidence() mcp.Tool {
	return mcp.NewTool("show_confidence",
		mcp.WithDescription("Return the current confidence and history for an artifact. "+
			"Result shape: {current: <float>, history: {history: [...]}}."),
		mcp.WithString("artifact_id", mcp.Required(), mcp.Description("Artifact ID to inspect.")),
		mcp.WithString("artifact_type", mcp.Description("Artifact type: 'memory' or 'lesson'. Default 'memory'.")),
	)
}

func handleShowConfidence(dm *internal.DatabaseManager) server.ToolHandlerFunc {
	return func(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
		args := req.GetArguments()
		artifactID, _ := args["artifact_id"].(string)
		if artifactID == "" {
			return mcp.NewToolResultError("artifact_id is required"), nil
		}
		artifactType, _ := args["artifact_type"].(string)
		out, err := dm.ShowConfidence(artifactID, artifactType)
		if err != nil {
			return mcp.NewToolResultErrorFromErr("show_confidence failed", err), nil
		}
		return jsonResult(out), nil
	}
}
```

- [ ] **Step 7: Register the tool**

In `cmd/mpm-mcp/tools.go` `RegisterAllTools`, add:

```go
	s.AddTool(toolShowConfidence(), handleShowConfidence(dm))
```

- [ ] **Step 8: Build both binaries**

```bash
go build -tags fts5 ./cmd/mpm
go build -tags fts5 ./cmd/mpm-mcp
```

- [ ] **Step 9: Commit**

```bash
git add internal/call_helpers.go internal/call_helpers_test.go cmd/mpm/call.go cmd/mpm-mcp/tools.go
git commit -m "feat(mcp): show_confidence — share composed dm method

Preserves the {current, history:{history:[...]}} double-nested shape
that the legacy callShowConfidence returned.

Co-Authored-By: Claude Opus 4.7 <noreply@anthropic.com>"
```

---

## Task 9: `dm.RecomputeConfidence` + MCP `recompute_confidence`

**Files:**
- Modify: `internal/call_helpers.go`
- Modify: `internal/call_helpers_test.go`
- Modify: `cmd/mpm/call.go`
- Modify: `cmd/mpm-mcp/tools.go`

- [ ] **Step 1: Add failing tests**

Append to `internal/call_helpers_test.go`:

```go
func TestCallHelpers_RecomputeConfidence_TriggersRecompute(t *testing.T) {
	dm := newTestDM(t)
	_, err := dm.ExecTracked(`INSERT INTO memories (id, collection, content) VALUES (?, 'memories', 'x')`, 0, "mem-1")
	require.NoError(t, err)
	require.NoError(t, AddEvidence(dm, EvidenceInput{ArtifactID: "mem-1", ArtifactType: "memory", Type: "reproduction", SourceGroup: "g", Strength: 0.85, CreatedBy: "t", CreatedAt: time.Now()}))

	out, err := dm.RecomputeConfidence("mem-1", "memory")
	require.NoError(t, err)
	// Returns the same shape as ShowConfidence.
	_, hasCurrent := out["current"].(float64)
	assert.True(t, hasCurrent)
	_, hasHist := out["history"].(map[string]interface{})
	assert.True(t, hasHist)
}

func TestCallHelpers_RecomputeConfidence_RequiresArtifactID(t *testing.T) {
	dm := newTestDM(t)
	_, err := dm.RecomputeConfidence("", "memory")
	require.Error(t, err)
	assert.Contains(t, err.Error(), "artifact_id")
}
```

- [ ] **Step 2: Run tests, verify failure**

```bash
go test -tags fts5 -v ./internal/ -run TestCallHelpers_RecomputeConfidence
```

Expected: compile error.

- [ ] **Step 3: Implement `dm.RecomputeConfidence` in `internal/call_helpers.go`**

```go
// RecomputeConfidence forces a manual confidence recompute for an
// artifact and returns the new snapshot. Returns the same shape as
// dm.ShowConfidence so callers can read `result.current` and
// `result.history.history` consistently.
func (dm *DatabaseManager) RecomputeConfidence(artifactID, artifactType string) (map[string]interface{}, error) {
	if artifactID == "" {
		return nil, fmt.Errorf("artifact_id is required")
	}
	if artifactType == "" {
		artifactType = "memory"
	}
	if err := RecomputeConfidence(dm, artifactID, artifactType, RecomputeReasonManual); err != nil {
		return nil, err
	}
	return dm.ShowConfidence(artifactID, artifactType)
}
```

- [ ] **Step 4: Run tests, verify pass**

```bash
go test -tags fts5 -v ./internal/ -run TestCallHelpers_RecomputeConfidence
```

Expected: PASS.

- [ ] **Step 5: Refactor `callRecomputeConfidence` in `cmd/mpm/call.go`**

Replace lines 1076-1096 with:

```go
// callRecomputeConfidence forces a manual recompute and returns the snapshot.
func callRecomputeConfidence(payload map[string]interface{}) (interface{}, error) {
	dm, closeDM, err := openCallDM()
	if err != nil {
		return nil, err
	}
	defer closeDM()
	return dm.RecomputeConfidence(stringArg(payload["artifact_id"]), stringArg(payload["artifact_type"]))
}
```

- [ ] **Step 6: Add `toolRecomputeConfidence` and `handleRecomputeConfidence` in `cmd/mpm-mcp/tools.go`**

```go
// ── recompute_confidence ──────────────────────────────────────────────────

func toolRecomputeConfidence() mcp.Tool {
	return mcp.NewTool("recompute_confidence",
		mcp.WithDescription("Force a manual confidence recompute and return the new snapshot. "+
			"Result shape: {current, history: {history: [...]}} (same as show_confidence)."),
		mcp.WithString("artifact_id", mcp.Required(), mcp.Description("Artifact ID to recompute.")),
		mcp.WithString("artifact_type", mcp.Description("Artifact type: 'memory' or 'lesson'. Default 'memory'.")),
	)
}

func handleRecomputeConfidence(dm *internal.DatabaseManager) server.ToolHandlerFunc {
	return func(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
		args := req.GetArguments()
		artifactID, _ := args["artifact_id"].(string)
		if artifactID == "" {
			return mcp.NewToolResultError("artifact_id is required"), nil
		}
		artifactType, _ := args["artifact_type"].(string)
		out, err := dm.RecomputeConfidence(artifactID, artifactType)
		if err != nil {
			return mcp.NewToolResultErrorFromErr("recompute_confidence failed", err), nil
		}
		return jsonResult(out), nil
	}
}
```

- [ ] **Step 7: Register the tool**

In `cmd/mpm-mcp/tools.go` `RegisterAllTools`, add:

```go
	s.AddTool(toolRecomputeConfidence(), handleRecomputeConfidence(dm))
```

- [ ] **Step 8: Build both binaries**

```bash
go build -tags fts5 ./cmd/mpm
go build -tags fts5 ./cmd/mpm-mcp
```

- [ ] **Step 9: Commit**

```bash
git add internal/call_helpers.go internal/call_helpers_test.go cmd/mpm/call.go cmd/mpm-mcp/tools.go
git commit -m "feat(mcp): recompute_confidence — share composed dm method

Co-Authored-By: Claude Opus 4.7 <noreply@anthropic.com>"
```

---

## Task 10: `dm.ExplainConfidence` + MCP `explain_confidence`

**Files:**
- Modify: `internal/call_helpers.go`
- Modify: `internal/call_helpers_test.go`
- Modify: `cmd/mpm/call.go`
- Modify: `cmd/mpm-mcp/tools.go`

- [ ] **Step 1: Add failing tests**

Append to `internal/call_helpers_test.go`:

```go
func TestCallHelpers_ExplainConfidence_ComponentBreakdown(t *testing.T) {
	dm := newTestDM(t)
	_, err := dm.ExecTracked(`INSERT INTO memories (id, collection, content) VALUES (?, 'memories', 'x')`, 0, "mem-1")
	require.NoError(t, err)
	require.NoError(t, AddEvidence(dm, EvidenceInput{ArtifactID: "mem-1", ArtifactType: "memory", Type: "reproduction", SourceGroup: "g", Strength: 0.85, CreatedBy: "t", CreatedAt: time.Now()}))

	out, err := dm.ExplainConfidence("mem-1", "memory")
	require.NoError(t, err)
	assert.Equal(t, true, out["success"])
	assert.Contains(t, out, "explanation")
	_, isMap := out["explanation"].(map[string]interface{})
	assert.True(t, isMap, "explanation must be a map (ConfidenceExplanation JSON shape)")
}

func TestCallHelpers_ExplainConfidence_RequiresArtifactID(t *testing.T) {
	dm := newTestDM(t)
	_, err := dm.ExplainConfidence("", "memory")
	require.Error(t, err)
	assert.Contains(t, err.Error(), "artifact_id")
}
```

- [ ] **Step 2: Run tests, verify failure**

```bash
go test -tags fts5 -v ./internal/ -run TestCallHelpers_ExplainConfidence
```

Expected: compile error.

- [ ] **Step 3: Implement `dm.ExplainConfidence` in `internal/call_helpers.go`**

```go
// ExplainConfidence returns the reasoning trace for an artifact's
// confidence: the full component breakdown of f(evidence, decay).
// Distinct from query_confidence_history (audit trail) — this answers
// "why did I get this number?"
//
// Returns: {"success": true, "explanation": <ConfidenceExplanation>}.
func (dm *DatabaseManager) ExplainConfidence(artifactID, artifactType string) (map[string]interface{}, error) {
	if artifactID == "" {
		return nil, fmt.Errorf("artifact_id is required")
	}
	if artifactType == "" {
		artifactType = "memory"
	}
	exp, err := ExplainConfidence(dm, artifactID, artifactType)
	if err != nil {
		return nil, fmt.Errorf("explain confidence: %w", err)
	}
	return map[string]interface{}{
		"success":     true,
		"explanation": exp,
	}, nil
}
```

- [ ] **Step 4: Run tests, verify pass**

```bash
go test -tags fts5 -v ./internal/ -run TestCallHelpers_ExplainConfidence
```

Expected: PASS.

- [ ] **Step 5: Refactor `callExplainConfidence` in `cmd/mpm/call.go`**

Replace lines 1101-1125 with:

```go
// callExplainConfidence returns the reasoning trace for an artifact's confidence.
func callExplainConfidence(payload map[string]interface{}) (interface{}, error) {
	dm, closeDM, err := openCallDM()
	if err != nil {
		return nil, err
	}
	defer closeDM()
	return dm.ExplainConfidence(stringArg(payload["artifact_id"]), stringArg(payload["artifact_type"]))
}
```

- [ ] **Step 6: Add `toolExplainConfidence` and `handleExplainConfidence` in `cmd/mpm-mcp/tools.go`**

```go
// ── explain_confidence ────────────────────────────────────────────────────

func toolExplainConfidence() mcp.Tool {
	return mcp.NewTool("explain_confidence",
		mcp.WithDescription("Return the reasoning trace for an artifact's confidence: "+
			"the full component breakdown of f(evidence, decay). Distinct from "+
			"query_confidence_history (audit trail) — this answers 'why did I get this number?'"),
		mcp.WithString("artifact_id", mcp.Required(), mcp.Description("Artifact ID to explain.")),
		mcp.WithString("artifact_type", mcp.Description("Artifact type: 'memory' or 'lesson'. Default 'memory'.")),
	)
}

func handleExplainConfidence(dm *internal.DatabaseManager) server.ToolHandlerFunc {
	return func(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
		args := req.GetArguments()
		artifactID, _ := args["artifact_id"].(string)
		if artifactID == "" {
			return mcp.NewToolResultError("artifact_id is required"), nil
		}
		artifactType, _ := args["artifact_type"].(string)
		out, err := dm.ExplainConfidence(artifactID, artifactType)
		if err != nil {
			return mcp.NewToolResultErrorFromErr("explain_confidence failed", err), nil
		}
		return jsonResult(out), nil
	}
}
```

- [ ] **Step 7: Register the tool**

In `cmd/mpm-mcp/tools.go` `RegisterAllTools`, add:

```go
	s.AddTool(toolExplainConfidence(), handleExplainConfidence(dm))
```

- [ ] **Step 8: Build both binaries**

```bash
go build -tags fts5 ./cmd/mpm
go build -tags fts5 ./cmd/mpm-mcp
```

- [ ] **Step 9: Commit**

```bash
git add internal/call_helpers.go internal/call_helpers_test.go cmd/mpm/call.go cmd/mpm-mcp/tools.go
git commit -m "feat(mcp): explain_confidence — share dm method

Co-Authored-By: Claude Opus 4.7 <noreply@anthropic.com>"
```

---

## Task 11: Final verification

- [ ] **Step 1: Run the full internal test suite**

```bash
go test -tags fts5 ./internal/...
```

Expected: all tests pass.

- [ ] **Step 2: Build both binaries and confirm no warnings**

```bash
go build -tags fts5 -v ./cmd/mpm
go build -tags fts5 -v ./cmd/mpm-mcp
```

Expected: both compile cleanly.

- [ ] **Step 3: Smoke test the MCP server**

Start the server and send a `tools/list` request over stdio. From a shell:

```bash
echo '{"jsonrpc":"2.0","id":1,"method":"tools/list"}' | ./bin/mpm-mcp | head -200
```

Expected: JSON response containing all 29 tool names: `read_wake_context`, `query_long_term_memory`, `save_to_memory`, `challenge_memory`, `save_lesson`, `search_lessons`, `list_lessons`, `create_topic`, `search_topics`, `link_topic`, `add_reference`, `search_references`, `list_references`, `read_directives`, `propose_theory`, `resolve_theory`, `record_decision`, `proactive_recall_hint`, `route`, `log_to_changelog`, `add_evidence`, `list_evidence`, `query_confidence_history`, `query_confidence_changes`, `query_confidence_trend`, `query_memory_quality`, `show_confidence`, `recompute_confidence`, `explain_confidence`.

(If a fresh DB is required to start the server, run `mpm init` first.)

- [ ] **Step 4: Verify CLI output is byte-identical for an existing call**

Run an `add_evidence` call against a known memory. Compare the JSON output to what the previous `callAddEvidence` produced (the schema is `{"success": true, "confidence": <float>}` — record a baseline before the refactor if you haven't already). The shape must match exactly.

- [ ] **Step 5: Commit any stragglers and final review**

If any verification surfaced an issue, fix it and commit. Then:

```bash
git log --oneline -15
git status
```

Expected: a clean tree and 10 commits in this order (1 for the artifactTable move, 9 for the tools, in task order).

---

## Self-Review

### Spec coverage

- ✅ Goal 1 (add 9 tools) — Tasks 2-10.
- ✅ Goal 2 (lift inline SQL to dm methods) — Tasks 2-10.
- ✅ Goal 3 (match existing handler shape, required-arg + DB error patterns) — every task.
- ✅ Goal 4 (CLI output byte-identical) — ShowConfidence task explicitly tests the nested shape; all other tasks preserve the existing JSON shape.
- ✅ Goal 5 (no schema changes, no documentation of tool counts) — no doc edits, no schema migrations.

### Type consistency

- `EvidenceInput` used in `dm.AddEvidence(EvidenceInput)` matches the `internal.EvidenceInput` already used by `internal.AddEvidence`. ✓
- `ConfidenceChangesFilter` matches `internal.ConfidenceChangesFilter`. ✓
- `ArtifactTable` returns `string` consistently. ✓
- `dm.RecomputeConfidence` (method) and `internal.RecomputeConfidence` (function) coexist — different signatures (method is `(string, string) (map, error)`, function is `(DBNode, string, string, RecomputeReason) error`). No naming conflict at the call site.
- All map types use `map[string]interface{}` and slice types use `[]map[string]interface{}` consistently.

### Placeholder scan

No "TBD", "TODO", "implement later", or "fill in details" anywhere in the plan. Every code step shows the actual code. Every test step shows the actual test.
