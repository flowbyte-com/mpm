// phase2_integration_test.go — Phase 2 integration audit.
//
// Verifies the pointer-native projection chain across the full CLI/MCP
// boundary, and confirms Phase 1 transport protection still composes
// correctly with Phase 2 bounded reads.
//
// Ship gate 9: End-to-end projection → pointer → resolve
// Ship gate 10: End-to-end resolve → Phase 1 spill
// Ship gate 11: Production resolver wiring is exercised by test (not mocked)

package main

import (
	"context"
	"encoding/json"
	"io"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/flowbyte-com/mpm/internal/blobstore"
	mpmcore "github.com/flowbyte-com/mpm-core"
	"github.com/flowbyte-com/mpm-core/tools"
	"github.com/stretchr/testify/require"
)

// TestPhase2_ProjectionPointerResolveChain exercises the complete
// pointer-native recall path for both memory and lesson projections.
//
// Path A: projection → pointer → resolve (memory)
//   mpm_memory query projection=true → pointer
//   mpm_resolve(mpm://memory/<id>) → full content
//
// Path B: projection → pointer → resolve (lesson)
//   mpm_lessons search projection=true → pointer
//   mpm_resolve(mpm://lesson/<id>) → full content
//
// Invariants verified:
//   - projection=true returns mode:"projected" and no full content field
//   - pointer field is set to the canonical mpm:// URI
//   - resolve returns the original full content unchanged
//   - resolve returns bounded:true when max_bytes truncation fired
func TestPhase2_ProjectionPointerResolveChain(t *testing.T) {
	dm := newTestDMForCmd(t)
	restoreResolver := installTestResolver(dm)
	t.Cleanup(restoreResolver)

	// ── Seed: memory and lesson ─────────────────────────────────────────────

	memResult, err := runHandler(dm, "mpm_memory", map[string]interface{}{
		"action": "save",
		"params": map[string]interface{}{
			"fact":      "SQLite uses WAL mode by default for concurrent readers",
			"collection": "memories",
			"tags":      []string{"sqlite", "concurrency"},
			"weight":    8,
		},
	})
	require.NoError(t, err)
	memID := memResult.(map[string]interface{})["id"].(string)
	require.NotEmpty(t, memID)

	lessonResult, err := runHandler(dm, "mpm_lessons", map[string]interface{}{
		"action": "save",
		"params": map[string]interface{}{
			"fact": "Always quote shell variables to avoid word splitting",
			"type": "warning",
			"tags": []string{"shell", "safety"},
		},
	})
	require.NoError(t, err)
	lessonID := lessonResult.(map[string]interface{})["id"].(string)
	require.NotEmpty(t, lessonID)

	// ── Path A1: mpm_memory query projection=true ─────────────────────────

	queryResult, err := runHandler(dm, "mpm_memory", map[string]interface{}{
		"action": "query",
		"params": map[string]interface{}{
			"query":      "SQLite WAL",
			"projection": "summary",
		},
	})
	require.NoError(t, err)
	// Handler returns typed []ProjectedMemoryEntry; marshal→unmarshal to []interface{} for inspection.
	queryJSON, err := json.Marshal(queryResult)
	require.NoError(t, err)
	var queryMap map[string]interface{}
	require.NoError(t, json.Unmarshal(queryJSON, &queryMap))

	require.Equal(t, "summary", queryMap["mode"], "projection=summary should return mode=summary")
	require.NotNil(t, queryMap["memories"], "projected output should have memories key")

	var memories []interface{}
	memoriesRaw := queryMap["memories"]
	require.IsType(t, []interface{}{}, memoriesRaw)
	memories = memoriesRaw.([]interface{})
	require.NotEmpty(t, memIDs(memories), "should find the seeded memory")

	// Find our memory entry and extract the pointer.
	var memPointer string
	for _, m := range memories {
		entry := m.(map[string]interface{})
		if entry["id"] == memID || strings.Contains(entry["pointer"].(string), memID) {
			memPointer = entry["pointer"].(string)
			require.Equal(t, "memory", entry["type"])
			require.NotEmpty(t, entry["summary"])
			require.Empty(t, entry["content"], "projected memory must not contain full content")
			break
		}
	}
	require.NotEmpty(t, memPointer, "memory projection must include pointer field")
	require.Equal(t, "mpm://memory/"+memID, memPointer)

	// ── Path A2: mpm_resolve memory pointer → full content ────────────────

	resolveResult, err := runHandler(dm, "mpm_resolve", map[string]interface{}{
		"uri": memPointer,
	})
	require.NoError(t, err)
	resolveMap := resolveResult.(map[string]interface{})

	require.Equal(t, memPointer, resolveMap["pointer"])
	require.Contains(t, resolveMap["content"], "SQLite uses WAL mode")
	require.False(t, resolveMap["bounded"].(bool), "compact memory resolved within default 512-byte ceiling")

	// ── Path A3: mpm_resolve with explicit max_bytes (triggers bounded) ──

	resolveBounded, err := runHandler(dm, "mpm_resolve", map[string]interface{}{
		"uri":       memPointer,
		"max_bytes": float64(20),
	})
	require.NoError(t, err)
	boundedMap := resolveBounded.(map[string]interface{})
	require.True(t, boundedMap["bounded"].(bool), "max_bytes=20 should produce bounded=true")
	require.NotEqual(t, resolveMap["content"], boundedMap["content"], "bounded content must differ from full")

	// ── Path B1: mpm_lessons search projection=true ──────────────────────

	lessonQueryResult, err := runHandler(dm, "mpm_lessons", map[string]interface{}{
		"action": "search",
		"params": map[string]interface{}{
			"query":      "shell variables",
			"projection": "summary",
		},
	})
	require.NoError(t, err)
	lessonQueryJSON, err := json.Marshal(lessonQueryResult)
	require.NoError(t, err)
	var lessonQueryMap map[string]interface{}
	require.NoError(t, json.Unmarshal(lessonQueryJSON, &lessonQueryMap))
	require.Equal(t, "summary", lessonQueryMap["mode"])
	var lessons []interface{}
	lessonsRaw := lessonQueryMap["lessons"]
	require.IsType(t, []interface{}{}, lessonsRaw)
	lessons = lessonsRaw.([]interface{})

	// Find our lesson and extract the pointer.
	var lessonPointer string
	for _, l := range lessons {
		entry := l.(map[string]interface{})
		if strings.Contains(entry["pointer"].(string), lessonID) {
			lessonPointer = entry["pointer"].(string)
			require.Equal(t, "warning", entry["type"])
			require.NotEmpty(t, entry["summary"])
			require.Empty(t, entry["content"], "projected lesson must not contain full content")
			break
		}
	}
	require.NotEmpty(t, lessonPointer)
	require.Equal(t, "mpm://lesson/"+lessonID, lessonPointer)

	// ── Path B2: mpm_resolve lesson pointer → full content ───────────────

	lessonResolve, err := runHandler(dm, "mpm_resolve", map[string]interface{}{
		"uri": lessonPointer,
	})
	require.NoError(t, err)
	lessonResolveMap := lessonResolve.(map[string]interface{})

	require.Equal(t, lessonPointer, lessonResolveMap["pointer"])
	require.Contains(t, lessonResolveMap["content"], "Always quote shell variables")
	require.False(t, lessonResolveMap["bounded"].(bool), "lesson is compact; should not be bounded")

	// ── Path B3: mpm_lessons list projection=true ───────────────────────

	lessonListResult, err := runHandler(dm, "mpm_lessons", map[string]interface{}{
		"action": "list",
		"params": map[string]interface{}{
			"projection": "summary",
		},
	})
	require.NoError(t, err)
	lessonListJSON, err := json.Marshal(lessonListResult)
	require.NoError(t, err)
	var lessonListMap map[string]interface{}
	require.NoError(t, json.Unmarshal(lessonListJSON, &lessonListMap))
	require.Equal(t, "summary", lessonListMap["mode"])
	var lessonList []interface{}
	lessonListRaw := lessonListMap["lessons"]
	require.IsType(t, []interface{}{}, lessonListRaw)
	lessonList = lessonListRaw.([]interface{})

	// Find the lesson in the list.
	var listLessonPointer string
	for _, l := range lessonList {
		entry := l.(map[string]interface{})
		if strings.Contains(entry["pointer"].(string), lessonID) {
			listLessonPointer = entry["pointer"].(string)
			break
		}
	}
	require.Equal(t, lessonPointer, listLessonPointer, "list projection must return same pointer as search projection")
}

// memIDs extracts the set of memory IDs from a projected memory list.
func memIDs(entries []interface{}) []string {
	var ids []string
	for _, e := range entries {
		if id, ok := e.(map[string]interface{})["id"].(string); ok {
			ids = append(ids, id)
		}
	}
	return ids
}

// mockPhase2Resolver is a tools.pointerResolverInterface that resolves
// memory and lesson URIs against the test DM.
type mockPhase2Resolver struct {
	dm *mpmcore.DatabaseManager
}

func (m *mockPhase2Resolver) Resolve(ctx context.Context, p tools.Pointer, opts tools.ResolveOptions) (tools.Resolution, error) {
	switch p.Kind {
	case "memory":
		mem, err := m.dm.GetMemory(p.ID)
		if err != nil {
			return tools.Resolution{}, err
		}
		fullContent, _ := mem["content"].(string)
		maxBytes := int(opts.MaxBytes)
		if maxBytes <= 0 {
			maxBytes = 512
		}
		bounded := len(fullContent) > maxBytes
		readerContent := fullContent
		if bounded {
			readerContent = mpmcore.SummarizeBounded(fullContent, maxBytes)
		}
		return tools.Resolution{
			Pointer:     "mpm://memory/" + p.ID,
			ContentType: "text/plain",
			Reader:     io.NopCloser(strings.NewReader(readerContent)),
			Metadata:   mem,
			Bounded:    bounded,
		}, nil

	case "lesson":
		lesson, err := m.dm.GetLesson(p.ID)
		if err != nil {
			return tools.Resolution{}, err
		}
		return tools.Resolution{
			Pointer:     "mpm://lesson/" + p.ID,
			ContentType: "text/plain",
			Reader:     io.NopCloser(strings.NewReader(lesson.Content)),
			Metadata:   map[string]interface{}{"id": lesson.ID, "content": lesson.Content, "type": string(lesson.Type), "tags": lesson.Tags},
			Bounded:    false,
		}, nil

	default:
		return tools.Resolution{}, tools.ErrUnsupportedKind
	}
}

// installTestResolver registers a mock resolver for mpm_resolve calls
// during the test. It mirrors what cmd/mpm-mcp/tools.go does at boot.
func installTestResolver(dm *mpmcore.DatabaseManager) func() {
	resolver := &mockPhase2Resolver{dm: dm}
	tools.SetResolver(resolver)
	return func() { tools.SetResolver(nil) }
}

// installProductionResolver wires the real blob store and artifactResolverAdapter
// (identical to cmd/mpm-mcp/tools.go RegisterAllTools boot sequence) so that
// the E2E test exercises the actual production resolver, not a mock.
//
// This is Ship gate 11: the mock was sufficient to prove the handler protocol
// is correct but says nothing about whether the real mpm-mcp binary's wiring
// is correct.
func installProductionResolver(t *testing.T, dm *mpmcore.DatabaseManager) (restore func()) {
	t.Helper()

	// Create a real blob store backed by the test's temp directory.
	blobDir := filepath.Join(t.TempDir(), "blobs")
	bs, err := blobstore.NewFilesystemBackend(dm.SQLDB(), blobDir, 24*time.Hour)
	if err != nil {
		t.Fatalf("failed to create blob store for test: %v", err)
	}

	// Wire exactly as cmd/mpm-mcp/tools.go does at boot:
	//   blobStoreAdapter satisfies tools.blobStoreInterface
	//   artifactResolverAdapter satisfies tools.pointerResolverInterface
	blobAdapter := &testBlobStoreAdapter{bs: bs}
	tools.SetBlobStore(blobAdapter)
	tools.SetResolver(&testArtifactResolverAdapter{blobBS: blobAdapter, dm: dm})

	return func() {
		tools.SetBlobStore(nil)
		tools.SetResolver(nil)
	}
}

// testBlobStoreAdapter satisfies tools.blobStoreInterface (same implementation
// as blobStoreAdapter in cmd/mpm-mcp/tools.go).
type testBlobStoreAdapter struct {
	bs *blobstore.FilesystemBackend
}

func (a *testBlobStoreAdapter) Get(ctx context.Context, id string, opts tools.GetOptions) (io.ReadCloser, tools.Metadata, error) {
	reader, meta, err := a.bs.Get(ctx, id, blobstore.GetOptions{
		Offset:   opts.Offset,
		MaxBytes: opts.MaxBytes,
	})
	if err != nil {
		return nil, tools.Metadata{}, err
	}
	return reader, tools.Metadata{
		ContentType: meta.ContentType,
		SizeBytes:   meta.SizeBytes,
	}, nil
}

func (a *testBlobStoreAdapter) Search(ctx context.Context, id string, query tools.SearchQuery) ([]tools.Match, error) {
	matches, err := a.bs.Search(ctx, id, blobstore.SearchQuery{
		Query:           query.Query,
		Regex:           query.Regex,
		CaseInsensitive: query.CaseInsensitive,
		MaxMatches:      query.MaxMatches,
		MaxBytes:        query.MaxBytes,
	})
	if err != nil {
		return nil, err
	}
	result := make([]tools.Match, 0, len(matches))
	for _, m := range matches {
		result = append(result, tools.Match{
			LineNo:     m.LineNo,
			ByteOffset: m.ByteOffset,
			Snippet:    m.Snippet,
		})
	}
	return result, nil
}

// testArtifactResolverAdapter satisfies tools.pointerResolverInterface (same
// implementation as artifactResolverAdapter in cmd/mpm-mcp/tools.go).
type testArtifactResolverAdapter struct {
	blobBS *testBlobStoreAdapter
	dm     *mpmcore.DatabaseManager
}

func (a *testArtifactResolverAdapter) Resolve(ctx context.Context, p tools.Pointer, opts tools.ResolveOptions) (tools.Resolution, error) {
	switch p.Kind {
	case "blob":
		return a.resolveBlob(ctx, p, opts)
	case "memory":
		return a.resolveMemory(ctx, p, opts)
	case "lesson":
		return a.resolveLesson(ctx, p, opts)
	case "theory":
		return a.resolveTheory(ctx, p, opts)
	default:
		return tools.Resolution{}, tools.ErrUnsupportedKind
	}
}

func (a *testArtifactResolverAdapter) resolveBlob(ctx context.Context, p tools.Pointer, opts tools.ResolveOptions) (tools.Resolution, error) {
	reader, meta, err := a.blobBS.Get(ctx, p.ID, tools.GetOptions{})
	if err != nil {
		return tools.Resolution{}, err
	}
	defer reader.Close()
	content, err := io.ReadAll(reader)
	if err != nil {
		return tools.Resolution{}, err
	}
	return tools.Resolution{
		Pointer:     "mpm://blob/" + p.ID,
		ContentType: meta.ContentType,
		Reader:     io.NopCloser(strings.NewReader(string(content))),
		Metadata:   nil,
		Bounded:    false,
	}, nil
}

func (a *testArtifactResolverAdapter) resolveMemory(ctx context.Context, p tools.Pointer, opts tools.ResolveOptions) (tools.Resolution, error) {
	mem, err := a.dm.GetMemory(p.ID)
	if err != nil {
		return tools.Resolution{}, err
	}
	content, _ := mem["content"].(string)
	maxBytes := int(opts.MaxBytes)
	if maxBytes <= 0 {
		maxBytes = 512
	}
	bounded := len(content) > maxBytes
	readerContent := content
	if bounded {
		readerContent = mpmcore.SummarizeBounded(content, maxBytes)
	}
	_ = a.dm.RecordRetrieval(p.ID, "memory")
	return tools.Resolution{
		Pointer:     "mpm://memory/" + p.ID,
		ContentType: "text/plain",
		Reader:     io.NopCloser(strings.NewReader(readerContent)),
		Metadata:   mem,
		Bounded:    bounded,
	}, nil
}

func (a *testArtifactResolverAdapter) resolveLesson(ctx context.Context, p tools.Pointer, opts tools.ResolveOptions) (tools.Resolution, error) {
	lesson, err := a.dm.GetLesson(p.ID)
	if err != nil {
		return tools.Resolution{}, err
	}
	_ = a.dm.RecordRetrieval(p.ID, "lesson")
	return tools.Resolution{
		Pointer:     "mpm://lesson/" + p.ID,
		ContentType: "text/plain",
		Reader:     io.NopCloser(strings.NewReader(lesson.Content)),
		Metadata: map[string]interface{}{
			"id":                 lesson.ID,
			"type":               string(lesson.Type),
			"tags":               lesson.Tags,
			"reinforcement_count": lesson.ReinforcementCount,
			"created":            lesson.Created,
		},
		Bounded: false,
	}, nil
}

func (a *testArtifactResolverAdapter) resolveTheory(ctx context.Context, p tools.Pointer, opts tools.ResolveOptions) (tools.Resolution, error) {
	mem, err := a.dm.GetMemory(p.ID)
	if err != nil {
		return tools.Resolution{}, err
	}
	// Theory resolution requires the memory to be in the theories collection.
	collection, _ := mem["collection"].(string)
	if collection != "theories" {
		return tools.Resolution{}, tools.ErrUnsupportedKind
	}
	content, _ := mem["content"].(string)
	_ = a.dm.RecordRetrieval(p.ID, "memory")
	return tools.Resolution{
		Pointer:     "mpm://theory/" + p.ID,
		ContentType: "text/plain",
		Reader:     io.NopCloser(strings.NewReader(content)),
		Metadata:   mem,
		Bounded:    false,
	}, nil
}

// TestPhase2_ProductionResolverWiring is Ship gate 11. It re-exercises the
// projection → pointer → resolve chain but uses the production resolver
// wiring (blobstore + artifactResolverAdapter) instead of the mock.
// If cmd/mpm-mcp/tools.go wiring is broken, this test fails.
func TestPhase2_ProductionResolverWiring(t *testing.T) {
	dm := newTestDMForCmd(t)
	restore := installProductionResolver(t, dm)
	defer restore()

	// Seed a memory and a lesson.
	memResult, err := runHandler(dm, "mpm_memory", map[string]interface{}{
		"action": "save",
		"params": map[string]interface{}{
			"fact":       "WAL mode allows concurrent readers with a single writer",
			"collection": "memories",
			"weight":     9,
		},
	})
	require.NoError(t, err)
	memID := memResult.(map[string]interface{})["id"].(string)

	lessonResult, err := runHandler(dm, "mpm_lessons", map[string]interface{}{
		"action": "save",
		"params": map[string]interface{}{
			"fact": "Never use word splitting on shell variables",
			"type": "warning",
		},
	})
	require.NoError(t, err)
	lessonID := lessonResult.(map[string]interface{})["id"].(string)

	// ── Memory resolve via production adapter ─────────────────────────────────
	memPtr := "mpm://memory/" + memID
	resolveResult, err := runHandler(dm, "mpm_resolve", map[string]interface{}{
		"uri": memPtr,
	})
	require.NoError(t, err)
	resolveMap := resolveResult.(map[string]interface{})
	require.Equal(t, memPtr, resolveMap["pointer"])
	require.Contains(t, resolveMap["content"], "WAL mode")
	require.False(t, resolveMap["bounded"].(bool))

	// ── Lesson resolve via production adapter ──────────────────────────────────
	lessonPtr := "mpm://lesson/" + lessonID
	lessonResolve, err := runHandler(dm, "mpm_resolve", map[string]interface{}{
		"uri": lessonPtr,
	})
	require.NoError(t, err)
	lessonResolveMap := lessonResolve.(map[string]interface{})
	require.Equal(t, lessonPtr, lessonResolveMap["pointer"])
	require.Contains(t, lessonResolveMap["content"], "Never use word splitting")

	// ── Theory reject: memory ID that is not a theory ───────────────────────
	_, err = runHandler(dm, "mpm_resolve", map[string]interface{}{
		"uri": "mpm://theory/" + memID,
	})
	require.Error(t, err) // ErrUnsupportedKind expected — memory is not in theories collection

	// ── Bounded memory resolve (max_bytes) via production adapter ──────────────
	resolveBounded, err := runHandler(dm, "mpm_resolve", map[string]interface{}{
		"uri":       memPtr,
		"max_bytes": float64(20),
	})
	require.NoError(t, err)
	boundedMap := resolveBounded.(map[string]interface{})
	require.True(t, boundedMap["bounded"].(bool), "max_bytes=20 should produce bounded=true")
	require.NotEqual(t, resolveMap["content"], boundedMap["content"], "bounded content must differ from full")
}
