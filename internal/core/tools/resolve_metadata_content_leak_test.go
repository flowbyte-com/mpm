// resolve_metadata_content_leak_test.go — Sept 2026 launch-block fix.
//
// The Context Economics pass surfaced a model-facing context leak:
// `mpm_resolve` correctly bounded the top-level `content` field, but
// the response's `metadata` map was the verbatim row from the
// DatabaseManager, which contains `metadata.content` = the original
// unbounded stored content. For a 1 MB memory with `max_bytes=512`,
// the bounded top-level content was 512 bytes, but the complete
// serialized response carried the full 1 MB body via metadata.content.
//
// This file pins four invariants:
//
//   Test A: a large memory resolved with a tight max_bytes has bounded
//           top-level content AND no occurrence of the full sentinel
//           payload anywhere in the serialized response.
//   Test B: a small memory still resolves with its content + metadata.
//   Test C: every supported artifact type (memory, lesson, theory,
//           work) keeps its legitimate metadata fields intact.
//   Test D: mpm_memory query projection=summary is unaffected (the
//           pointer projection lives in a separate code path and is
//           not part of this fix site).
//
// Storage state is NOT mutated — only the response projection is
// changed via resolveMetadataFor() / stripUnboundedContent(). The fix
// drops the `content` key from the metadata projection; everything
// else (id, collection, tags, weight, created_at, etc.) flows
// through unchanged.

package tools

import (
	"encoding/json"
	"fmt"
	"strings"
	"testing"

	mpminternal "github.com/flowbyte-com/mpm-core"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// canaryPrefix is unique to this regression test. We seed a
// deterministic payload that repeats this prefix many times so the
// "no full payload in serialized response" check has unambiguous
// signal: any single occurrence in the serialized response means the
// leak is back.
const canaryPrefix = "CANARY-META-LEAK-PROBE-2026-X9QZ"

// seedLargeMemory inserts a memory with `canaryPrefix` repeated
// enough to make a sentinel scan unambiguous. The CLI fallback path
// of handleMpmResolve is exercised by clearing globalResolver for
// the duration of the test.
func seedLargeMemory(t *testing.T, dm *mpminternal.DatabaseManager, bodyLen int) string {
	t.Helper()
	// Each chunk is 50 bytes (49 chars + newline). 600 chunks = ~30 KB.
	chunk := strings.Repeat(canaryPrefix, 4) + " " // ~ 30 chars
	body := strings.Repeat(chunk+"\n", 1+bodyLen/len(chunk))
	body = body[:bodyLen]
	id := "mem-meta-leak-" + sanitizeForPointerURI(t.Name())
	_, err := dm.SQLDB().Exec(`
		INSERT INTO memories (id, collection, content, weight, tags, metadata, deleted_at, created_at, updated_at)
		VALUES (?, 'memories', ?, 5, '[]', '{}', NULL, CAST(strftime('%s','now') AS INTEGER), CAST(strftime('%s','now') AS INTEGER))
	`, id, body)
	require.NoError(t, err)
	return id
}

// TestA_LargeArtifactBoundedResolveNoLeak is the primary regression.
// Before the fix this test fails: top-level content is bounded but
// metadata.content echoes the full original body.
func TestA_LargeArtifactBoundedResolveNoLeak(t *testing.T) {
	dm := newTestSharedDM(t)
	ac := defaultACForPatch()

	// Force CLI fallback path.
	saved := globalResolver
	globalResolver = nil
	t.Cleanup(func() { globalResolver = saved })

	// Seed a 30 KB memory saturated with the canary prefix.
	const bodyLen = 30_000
	id := seedLargeMemory(t, dm, bodyLen)

	const maxBytes = 512
	res, err := handleMpmResolve(dm, ac, map[string]interface{}{
		"uri":       "mpm://memory/" + id,
		"max_bytes": float64(maxBytes),
	})
	require.NoError(t, err)
	resp, ok := res.(map[string]interface{})
	require.True(t, ok, "handleMpmResolve must return a map for CLI parity")

	// 1. Top-level content is bounded.
	content, _ := resp["content"].(string)
	require.True(t, len(content) <= maxBytes,
		"top-level content must be <= max_bytes (got %d)", len(content))
	bounded, _ := resp["bounded"].(bool)
	assert.True(t, bounded, "bounded flag must be true when content is truncated")

	// 2. Recursive scan: the full canary payload must NOT appear
	// anywhere in the serialized response. This catches the leak in
	// metadata.content OR in any other field that might echo the row.
	serialized, err := json.Marshal(resp)
	require.NoError(t, err)
	// The bounded top-level content can contain the canary (it is a
	// substring of the truncated body) — that is fine. The leak is
	// the FULL canary payload repeated through the body.
	canaryChunk := strings.Repeat(canaryPrefix, 4) + " " // ~30 chars
	fullPayload := strings.Repeat(canaryChunk+"\n", 1+bodyLen/len(canaryChunk))
	fullPayload = fullPayload[:bodyLen]
	if strings.Contains(string(serialized), fullPayload) {
		t.Fatalf("leak: full unbounded payload (len=%d) appears in serialized resolve response (size=%d)",
			bodyLen, len(serialized))
	}

	// 3. metadata.content specifically must be absent. (Belt and braces
	// alongside the recursive scan.)
	meta, ok := resp["metadata"].(map[string]interface{})
	if ok {
		if _, hasContent := meta["content"]; hasContent {
			t.Errorf("metadata.content must be absent in the resolve response (leak regressed)")
		}
	}

	// 4. Legitimate metadata is preserved (id should still be there).
	if ok {
		if _, hasID := meta["id"]; !hasID {
			t.Errorf("metadata.id must be preserved")
		}
	}
}

// TestB_SmallArtifactResolvesCorrectly pins that ordinary, small
// memories still resolve with both their content AND legitimate
// metadata fields. The fix MUST NOT strip small content (where the
// canary scan is not the relevant signal — the bounded top-level is
// the full content).
func TestB_SmallArtifactResolvesCorrectly(t *testing.T) {
	dm := newTestSharedDM(t)
	ac := defaultACForPatch()

	saved := globalResolver
	globalResolver = nil
	t.Cleanup(func() { globalResolver = saved })

	body := "small probe body for TestB"
	id := "mem-test-b-" + sanitizeForPointerURI(t.Name())
	_, err := dm.SQLDB().Exec(`
		INSERT INTO memories (id, collection, content, weight, tags, metadata, deleted_at, created_at, updated_at)
		VALUES (?, 'memories', ?, 5, '[]', '{}', NULL, CAST(strftime('%s','now') AS INTEGER), CAST(strftime('%s','now') AS INTEGER))
	`, id, body)
	require.NoError(t, err)

	res, err := handleMpmResolve(dm, ac, map[string]interface{}{
		"uri":       "mpm://memory/" + id,
		"max_bytes": float64(512),
	})
	require.NoError(t, err)
	resp := res.(map[string]interface{})

	// Top-level content must be the full body (it fits within max_bytes).
	got, _ := resp["content"].(string)
	assert.Equal(t, body, got, "small body must round-trip intact")
	bounded, _ := resp["bounded"].(bool)
	assert.False(t, bounded, "small body within max_bytes must be bounded=false")

	// Legitimate metadata fields preserved.
	meta, ok := resp["metadata"].(map[string]interface{})
	require.True(t, ok, "metadata must be present")
	assert.Equal(t, id, meta["id"])
	assert.Equal(t, "memories", meta["collection"])
	assert.NotNil(t, meta["created_at"], "created_at must be preserved")
}

// TestC_LessonResolveHonorsBounds pins the lesson CLI fallback. The
// lesson branch already used a hand-built metadata map (no leak), but
// we verify the bounded contract is intact and unchanged.
func TestC_LessonResolveHonorsBounds(t *testing.T) {
	dm := newTestSharedDM(t)
	ac := defaultACForPatch()

	saved := globalResolver
	globalResolver = nil
	t.Cleanup(func() { globalResolver = saved })

	// Seed a lesson in the lessons_base table directly.
	longContent := strings.Repeat("lesson content padding. ", 100) // ~2.3 KB
	lessonID := "lesson-meta-leak-" + sanitizeForPointerURI(t.Name())
	_, err := dm.SQLDB().Exec(`
		INSERT INTO lessons_base (id, type, content, tags, reinforcement_count, created)
		VALUES (?, 'insight', ?, '[]', 1, CAST(strftime('%s','now') AS TEXT))
	`, lessonID, longContent)
	require.NoError(t, err)

	res, err := handleMpmResolve(dm, ac, map[string]interface{}{
		"uri":       "mpm://lesson/" + lessonID,
		"max_bytes": float64(50),
	})
	require.NoError(t, err)
	resp := res.(map[string]interface{})

	content, _ := resp["content"].(string)
	assert.LessOrEqual(t, len(content), 50, "lesson content must be bounded to <= max_bytes")
	bounded, _ := resp["bounded"].(bool)
	assert.True(t, bounded, "lesson bounded flag must reflect truncation")

	// Lesson metadata is hand-built (id + type). Verify intact.
	meta, ok := resp["metadata"].(map[string]interface{})
	require.True(t, ok)
	assert.Equal(t, lessonID, meta["id"])
	assert.Equal(t, "insight", meta["type"])
}

// TestC_TheoryResolveHonorsBounds pins the theory CLI fallback.
// Theory used to leak via `metadata: mem` (raw row); the fix
// applies resolveMetadataFor so metadata.content is dropped.
func TestC_TheoryResolveHonorsBounds(t *testing.T) {
	dm := newTestSharedDM(t)
	ac := defaultACForPatch()

	saved := globalResolver
	globalResolver = nil
	t.Cleanup(func() { globalResolver = saved })

	longContent := strings.Repeat("theory content padding. ", 100) // ~2.3 KB
	theoryID := "theory-meta-leak-" + sanitizeForPointerURI(t.Name())
	_, err := dm.SQLDB().Exec(`
		INSERT INTO memories (id, collection, content, weight, tags, metadata, deleted_at, created_at, updated_at)
		VALUES (?, 'theories', ?, 5, '[]', '{}', NULL, CAST(strftime('%s','now') AS INTEGER), CAST(strftime('%s','now') AS INTEGER))
	`, theoryID, longContent)
	require.NoError(t, err)

	res, err := handleMpmResolve(dm, ac, map[string]interface{}{
		"uri":       "mpm://theory/" + theoryID,
		"max_bytes": float64(50),
	})
	require.NoError(t, err)
	resp := res.(map[string]interface{})

	content, _ := resp["content"].(string)
	assert.LessOrEqual(t, len(content), 50, "theory content must be bounded to <= max_bytes")

	// Theory metadata is the row with content stripped.
	meta, ok := resp["metadata"].(map[string]interface{})
	require.True(t, ok)
	assert.Equal(t, theoryID, meta["id"])
	if _, hasContent := meta["content"]; hasContent {
		t.Errorf("theory metadata.content must be absent (was: len=%d)",
			len(meta["content"].(string)))
	}
	// The full theory body must not appear anywhere in the serialized response.
	serialized, _ := json.Marshal(resp)
	if strings.Contains(string(serialized), longContent) {
		t.Errorf("theory full content (len=%d) leaked into serialized response", len(longContent))
	}
}

// TestD_PointerProjectionUnchanged pins that the
// mpm_memory query projection=summary path is NOT affected by this
// fix. The pointer projection lives in handleQueryLongTermMemory —
// completely separate code path from handleMpmResolve. A regression
// here would imply somebody wired the metadata-strip into the wrong
// site.
func TestD_PointerProjectionUnchanged(t *testing.T) {
	dm := newTestSharedDM(t)
	ac := defaultACForPatch()

	body := "small test body for TestD pointer projection " + strings.Repeat("X-padding-to-outgrow-summary-cap-", 20) + "UNIQUE-SENTINEL-PROBE-X9QZ-MUST-NOT-LEAK"
	_, err := dm.SaveMemory("memories", body, "", nil, nil, nil, false, 1)
	require.NoError(t, err)

	res, err := handleQueryLongTermMemory(dm, ac, map[string]interface{}{
		"query":      body,
		"projection": "summary",
		"limit":      5,
	})
	require.NoError(t, err)
	resp, ok := res.(map[string]interface{})
	require.True(t, ok, "summary projection must return a map")

	// Pointer projection returns []ProjectedMemoryEntry. Marshal to JSON
	// and inspect the wire shape — that's what actually reaches the model.
	raw, err := json.Marshal(resp)
	require.NoError(t, err)
	var wire map[string]interface{}
	require.NoError(t, json.Unmarshal(raw, &wire))

	memoriesRaw, ok := wire["memories"].([]interface{})
	require.True(t, ok, "memories array must be present in wire projection")
	if len(memoriesRaw) == 0 {
		t.Fatalf("expected at least one result row")
	}
	row, ok := memoriesRaw[0].(map[string]interface{})
	require.True(t, ok, "memory row must be an object")

	// Pointer projection: each row has summary + pointer, NOT full content.
	require.Contains(t, row, "summary")
	require.Contains(t, row, "pointer")
	summary, _ := row["summary"].(string)
	// Summary is bounded to 256 runes + a fixed suffix
	// "... [truncated, resolve pointer for full text]" (47 chars).
	assert.LessOrEqual(t, len(summary), 256+50,
		"summary must be bounded to 256 runes + truncation suffix")
	if _, hasContent := row["content"]; hasContent {
		t.Errorf("summary projection must NOT include content (pointer architecture contract)")
	}
	// The summary is bounded to 256 chars and may legitimately contain
	// the leading slice of the body. The leak signal is anything past
	// the 256-rune cutoff appearing in the wire — a unique sentinel
	// appended at the end of the body is a precise signal.
	//
	// The response envelope echoes the original query string at
	// `.query`, which is by design (agents correlate the result with
	// the request). The query here contains the full body as a
	// substring of the search term, so we exclude that one path from
	// the leak check.
	const sentinel = "UNIQUE-SENTINEL-PROBE-X9QZ-MUST-NOT-LEAK"
	var foundPath []string
	var walk func(v interface{}, path []string)
	walk = func(v interface{}, path []string) {
		switch x := v.(type) {
		case map[string]interface{}:
			for k, vv := range x {
				walk(vv, append(path, k))
			}
		case []interface{}:
			for i, vv := range x {
				walk(vv, append(path, fmt.Sprintf("[%d]", i)))
			}
		case string:
			if len(path) > 0 && path[len(path)-1] == "query" {
				return // legitimate query echo, not a leak
			}
			if strings.Contains(x, sentinel) {
				foundPath = append([]string{}, path...)
			}
		}
	}
	walk(wire, nil)
	if len(foundPath) > 0 {
		t.Errorf("summary projection leaked content past the 256-rune cutoff; sentinel found at path=%v",
			strings.Join(foundPath, "."))
	}
}

// truncateForDiag returns the first n bytes of s for diagnostic output.
func truncateForDiag(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "..."
}

// TestE_ResolveMetadataForHelper pins the projection helper directly.
// It is the contract other call sites depend on.
func TestE_ResolveMetadataForHelper(t *testing.T) {
	src := map[string]interface{}{
		"id":         "abc",
		"collection": "memories",
		"content":    "FULL-UNBOUNDED-CONTENT-PAYLOAD-1234567890",
		"tags":       "[]",
		"weight":     5.0,
	}
	out := resolveMetadataFor(src)

	// Content dropped.
	if _, has := out["content"]; has {
		t.Errorf("resolveMetadataFor must drop the content key")
	}
	// Other fields preserved.
	assert.Equal(t, "abc", out["id"])
	assert.Equal(t, "memories", out["collection"])
	assert.Equal(t, "[]", out["tags"])
	assert.Equal(t, 5.0, out["weight"])

	// Input not mutated (stored state is preserved).
	if src["content"] != "FULL-UNBOUNDED-CONTENT-PAYLOAD-1234567890" {
		t.Errorf("resolveMetadataFor must NOT mutate the input map (stored-state invariant)")
	}
}
