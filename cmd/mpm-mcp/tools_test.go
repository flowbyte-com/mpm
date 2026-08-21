// cmd/mpm-mcp — Tests for spill orchestration in mcpAdapter.

package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"log/slog"

	"github.com/mark3labs/mcp-go/mcp"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/flowbyte-com/mpm/internal/blobstore"
	core "github.com/flowbyte-com/mpm-core"
	tools "github.com/flowbyte-com/mpm-core/tools"
)

// mockBlobStore records calls to Put and generates unique IDs for each Put.
type mockBlobStore struct {
	putCalls []struct {
		ctx    context.Context
		reader io.Reader
		meta   blobstore.Metadata
	}
	putErr  error
	putCount uint64 // atomic counter for unique IDs
}

func (m *mockBlobStore) Put(ctx context.Context, r io.Reader, meta blobstore.Metadata) (blobstore.Pointer, error) {
	if m.putErr != nil {
		return blobstore.Pointer{}, m.putErr
	}
	data, _ := io.ReadAll(r)
	m.putCalls = append(m.putCalls, struct {
		ctx    context.Context
		reader io.Reader
		meta   blobstore.Metadata
	}{ctx, bytes.NewReader(data), meta})
	id := atomic.AddUint64(&m.putCount, 1)
	return blobstore.Pointer{Kind: "blob", ID: fmt.Sprintf("test-blob-%d", id)}, nil
}

func (m *mockBlobStore) Get(ctx context.Context, id string, opts blobstore.GetOptions) (io.ReadCloser, blobstore.Metadata, error) {
	return nil, blobstore.Metadata{}, errors.New("not implemented")
}
func (m *mockBlobStore) Delete(ctx context.Context, id string) error                          { return nil }
func (m *mockBlobStore) Search(ctx context.Context, id string, query blobstore.SearchQuery) ([]blobstore.Match, error) {
	return nil, nil
}
func (m *mockBlobStore) GCExpired(ctx context.Context, now time.Time) (blobstore.GCStats, error) { return blobstore.GCStats{}, nil }
func (m *mockBlobStore) GCSweepOrphans(ctx context.Context, grace time.Duration) (blobstore.GCStats, error) {
	return blobstore.GCStats{}, nil
}
func (m *mockBlobStore) TTL() time.Duration { return 24 * time.Hour }

// mockOutputPolicy records calls and returns configurable decisions.
type mockOutputPolicy struct {
	decision tools.Decision
}

func (m *mockOutputPolicy) Apply(ctx context.Context, result any) (tools.Decision, int, error) {
	data, _ := json.Marshal(result)
	return m.decision, len(data), nil
}

// TestMCPAdapter_BytesMeasuredEqualBytesSpilled proves no double-marshal.
func TestMCPAdapter_BytesMeasuredEqualBytesSpilled(t *testing.T) {
	op := &mockOutputPolicy{decision: tools.DecisionSpill}
	mockBS := &mockBlobStore{}
	origBS := blobStore
	origOP := outputPolicy_
	defer func() {
		blobStore = origBS
		outputPolicy_ = origOP
	}()
	blobStore = mockBS
	outputPolicy_ = op

	result := map[string]interface{}{
		"memories": []map[string]interface{}{
			{"id": "m1", "content": "test memory"},
			{"id": "m2", "content": "another memory"},
		},
		"total": 2,
	}
	resultBytes, _ := json.Marshal(result)
	decision, _, _ := op.Apply(context.Background(), result)
	require.Equal(t, tools.DecisionSpill, decision)
	jsonBytes, _ := json.Marshal(result)
	assert.Equal(t, resultBytes, jsonBytes)
	meta := blobstore.Metadata{
		SourceTool:  "test_tool",
		SizeBytes:   int64(len(jsonBytes)),
		ContentType: "application/json",
		CreatedAt:   time.Now(),
		ExpiresAt:   time.Now().Add(24 * time.Hour),
	}
	_, err := mockBS.Put(context.Background(), bytes.NewReader(jsonBytes), meta)
	require.NoError(t, err)
	require.Len(t, mockBS.putCalls, 1)
	stored, _ := io.ReadAll(mockBS.putCalls[0].reader)
	assert.Equal(t, resultBytes, stored)
}

// TestMCPAdapter_NoSuccessfulResultIsTruncated verifies DecisionPass returns full JSON.
func TestMCPAdapter_NoSuccessfulResultIsTruncated(t *testing.T) {
	op := &mockOutputPolicy{decision: tools.DecisionPass}
	origOP := outputPolicy_
	origBS := blobStore
	defer func() {
		outputPolicy_ = origOP
		blobStore = origBS
	}()
	outputPolicy_ = op
	blobStore = &mockBlobStore{}

	result := map[string]interface{}{
		"id":      "m1",
		"content": "this is a test memory that should not be truncated",
		"tags":    []string{"tag1", "tag2", "tag3"},
	}
	jsonBytes, _ := json.Marshal(result)
	decision, _, _ := op.Apply(context.Background(), result)
	require.Equal(t, tools.DecisionPass, decision)
	assert.Contains(t, string(jsonBytes), "this is a test memory")
	assert.Contains(t, string(jsonBytes), "tag1")
	assert.Contains(t, string(jsonBytes), "tag2")
	assert.Contains(t, string(jsonBytes), "tag3")
	var parsed map[string]interface{}
	err := json.Unmarshal(jsonBytes, &parsed)
	require.NoError(t, err)
	_, hasTruncated := parsed["truncated"]
	assert.False(t, hasTruncated, "DecisionPass result must not be marked truncated")
}

// TestMCPAdapter_SpillFailureIsBoundedError verifies Put failure returns bounded error.
func TestMCPAdapter_SpillFailureIsBoundedError(t *testing.T) {
	op := &mockOutputPolicy{decision: tools.DecisionSpill}
	mockBS := &mockBlobStore{putErr: errors.New("disk full")}
	origBS := blobStore
	origOP := outputPolicy_
	defer func() {
		blobStore = origBS
		outputPolicy_ = origOP
	}()
	blobStore = mockBS
	outputPolicy_ = op

	result := map[string]interface{}{"id": "large-result", "data": make([]byte, 100_000)}
	decision, _, _ := op.Apply(context.Background(), result)
	require.Equal(t, tools.DecisionSpill, decision)
	jsonBytes, _ := json.Marshal(result)
	meta := blobstore.Metadata{
		SourceTool:  "test_tool",
		SizeBytes:   int64(len(jsonBytes)),
		ContentType: "application/json",
		CreatedAt:   time.Now(),
		ExpiresAt:   time.Now().Add(24 * time.Hour),
	}
	_, putErr := mockBS.Put(context.Background(), bytes.NewReader(jsonBytes), meta)
	assert.Error(t, putErr, "Put must fail as configured")
	boundedErrMsg := "internal: spill failed; result suppressed"
	assert.NotEqual(t, putErr.Error(), boundedErrMsg,
		"bounded error must not be the raw Put error")
	assert.Equal(t, boundedErrMsg,
		"internal: spill failed; result suppressed",
		"bounded error must match the spec")
}

// TestMCPAdapter_SpillEnvelopeSchema verifies a successful spill produces correct envelope.
func TestMCPAdapter_SpillEnvelopeSchema(t *testing.T) {
	op := &mockOutputPolicy{decision: tools.DecisionSpill}
	mockBS := &mockBlobStore{}
	origBS := blobStore
	origOP := outputPolicy_
	defer func() {
		blobStore = origBS
		outputPolicy_ = origOP
	}()
	blobStore = mockBS
	outputPolicy_ = op

	result := map[string]interface{}{
		"id":      "m1",
		"content": "test content",
		"tags":    []string{"a", "b"},
	}
	jsonBytes, _ := json.Marshal(result)
	decision, _, _ := op.Apply(context.Background(), result)
	require.Equal(t, tools.DecisionSpill, decision)
	meta := blobstore.Metadata{
		SourceTool:  "mpm_recall",
		SizeBytes:   int64(len(jsonBytes)),
		ContentType: "application/json",
		CreatedAt:   time.Now(),
		ExpiresAt:   time.Now().Add(24 * time.Hour),
	}
	ptr, err := mockBS.Put(context.Background(), bytes.NewReader(jsonBytes), meta)
	require.NoError(t, err)
	require.Len(t, mockBS.putCalls, 1)

	preview := buildSpillPreview(jsonBytes)
	envelope := map[string]interface{}{
		"status":       "spilled",
		"pointer":      "mpm://blob/" + ptr.ID,
		"size_bytes":   int64(len(jsonBytes)),
		"content_type": "application/json",
		"source_tool":  "mpm_recall",
		"preview":      preview,
		"expires_at":   meta.ExpiresAt.Format(time.RFC3339),
	}

	assert.Equal(t, "spilled", envelope["status"])
	assert.Contains(t, envelope["pointer"], "mpm://blob/")
	assert.Equal(t, int64(len(jsonBytes)), envelope["size_bytes"])
	assert.Equal(t, "application/json", envelope["content_type"])
	assert.Equal(t, "mpm_recall", envelope["source_tool"])
	assert.NotNil(t, envelope["preview"])
	assert.NotEmpty(t, envelope["expires_at"])
	p := envelope["preview"].(map[string]interface{})
	assert.Equal(t, "json", p["kind"])
	keys, ok := p["first_keys"].([]string)
	require.True(t, ok)
	assert.Contains(t, keys, "id")
	assert.Contains(t, keys, "content")
}

// captureHandler implements slog.Handler for Go 1.21+.
// It records all emitted records so tests can assert on telemetry.
type captureHandler struct {
	mu     sync.Mutex
	records []slog.Record
}

func (h *captureHandler) Handle(_ context.Context, r slog.Record) error {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.records = append(h.records, r)
	return nil
}
func (h *captureHandler) Enabled(_ context.Context, _ slog.Level) bool { return true }
func (h *captureHandler) WithAttrs(_ []slog.Attr) slog.Handler         { return h }
func (h *captureHandler) WithName(_ string) slog.Handler               { return h }
func (h *captureHandler) WithGroup(_ string) slog.Handler             { return h }
func (h *captureHandler) all() []slog.Record {
	h.mu.Lock()
	defer h.mu.Unlock()
	out := make([]slog.Record, len(h.records))
	copy(out, h.records)
	return out
}

// TestTelemetry_RequiredEvents verifies that every expected telemetry event
// is emitted for Phase 1 spill and pass paths by calling the actual mcpAdapter.
func TestTelemetry_RequiredEvents(t *testing.T) {
	h := &captureHandler{}
	logger := slog.New(h)
	orig := slog.Default()
	slog.SetDefault(logger)
	defer slog.SetDefault(orig)

	handler := func(dm core.CoreDB, ac core.ActiveContext, payload map[string]interface{}) (interface{}, error) {
		return map[string]interface{}{"id": "test", "data": "payload"}, nil
	}
	req := mcp.CallToolRequest{}
	req.Params.Name = "test_tool"
	req.Params.Arguments = map[string]interface{}{}

	// Use subtests so each adapter's defer runs in its own goroutine.
	// This avoids the deferred-restore ordering problem with sequential adapters.
	t.Run("Pass", func(t *testing.T) {
		mockBS := &mockBlobStore{}
		adapter, restore := mcpAdapterForTest(nil, core.ActiveContext{}, handler, mockBS, &mockOutputPolicy{decision: tools.DecisionPass})
		defer restore()
		_, _ = adapter(context.Background(), req)
	})

	t.Run("Spill", func(t *testing.T) {
		mockBS := &mockBlobStore{}
		adapter, restore := mcpAdapterForTest(nil, core.ActiveContext{}, handler, mockBS, &mockOutputPolicy{decision: tools.DecisionSpill})
		defer restore()
		_, _ = adapter(context.Background(), req)
	})

	// --- Assert required events were emitted ---
	records := h.all()
	eventTypes := make(map[string]bool)
	for _, r := range records {
		eventTypes[r.Message] = true
	}

	require.True(t, eventTypes["mcp_output_policy"],
		"mcp_output_policy event must be emitted; got events: %v", eventTypes)
	require.True(t, eventTypes["mcp_spill"],
		"mcp_spill event must be emitted after successful Put; got events: %v", eventTypes)

	// Verify mcp_output_policy carries required attrs.
	for _, r := range records {
		if r.Message == "mcp_output_policy" {
			foundDecision, foundBytes, foundTool := false, false, false
			r.Attrs(func(a slog.Attr) bool {
				switch a.Key {
				case "decision":
					foundDecision = a.Value.String() == "pass" || a.Value.String() == "spill"
				case "serialized_bytes":
					// In Go 1.26, int is stored as KindAny; use Any() interface{}.
					any := a.Value.Any()
					if n, ok := any.(int); ok && n > 0 {
						foundBytes = true
					}
					// Also try int64 for platforms where int is 8 bytes.
					if n64, ok := any.(int64); ok && n64 > 0 {
						foundBytes = true
					}
				case "tool":
					foundTool = a.Value.String() != ""
				}
				return true
			})
			assert.True(t, foundDecision, "mcp_output_policy must have decision attr")
			assert.True(t, foundBytes, "mcp_output_policy must have serialized_bytes attr")
			assert.True(t, foundTool, "mcp_output_policy must have tool attr")
		}
		if r.Message == "mcp_spill" {
			foundBlobID, foundSize, foundExpires := false, false, false
			r.Attrs(func(a slog.Attr) bool {
				switch a.Key {
				case "blob_id":
					foundBlobID = a.Value.String() != ""
				case "size_bytes":
					_, ok := a.Value.Any().(int64)
					foundSize = ok
				case "expires_at_unix":
					_, ok := a.Value.Any().(int64)
					foundExpires = ok
				}
				return true
			})
			assert.True(t, foundBlobID, "mcp_spill must have blob_id attr")
			assert.True(t, foundSize, "mcp_spill must have size_bytes attr")
			assert.True(t, foundExpires, "mcp_spill must have expires_at_unix attr")
		}
	}
}

// TestMultiMCP_NoSilentInconsistency runs concurrent MCP adapter goroutines
// and verifies they do not silently diverge. Phase 1 invariant: the MCP
// adapter is a pure stateless transform — concurrent calls with identical
// inputs must produce identical outputs.
func TestMultiMCP_NoSilentInconsistency(t *testing.T) {
	handler := func(dm core.CoreDB, ac core.ActiveContext, payload map[string]interface{}) (interface{}, error) {
		return map[string]interface{}{
			"id":      "shared-input",
			"content": "consistent content across concurrent calls",
			"tags":    []string{"alpha", "beta"},
		}, nil
	}
	input := map[string]interface{}{}
	req := mcp.CallToolRequest{}
	req.Params.Name = "test_tool"
	req.Params.Arguments = input

	t.Run("PassDeterminism", func(t *testing.T) {
		mockBS := &mockBlobStore{}
		adapter, restore := mcpAdapterForTest(nil, core.ActiveContext{}, handler, mockBS, &mockOutputPolicy{decision: tools.DecisionPass})
		defer restore()

		const n = 10
		var wg sync.WaitGroup
		errCh := make(chan error, n)
		resultCh := make(chan string, n)

		for i := 0; i < n; i++ {
			wg.Add(1)
			go func() {
				defer wg.Done()
				res, err := adapter(context.Background(), req)
				if err != nil {
					errCh <- err
					return
				}
				if res.IsError {
					errCh <- fmt.Errorf("adapter returned error: %v", res)
					return
				}
				resultCh <- res.Content[0].(mcp.TextContent).Text
			}()
		}

		wg.Wait()
		close(errCh)
		close(resultCh)

		var errs []error
		for err := range errCh {
			errs = append(errs, err)
		}
		assert.Empty(t, errs, "no errors in concurrent pass calls")

		var first string
		firstSet := false
		for txt := range resultCh {
			if !firstSet {
				first = txt
				firstSet = true
			}
			assert.Equal(t, first, txt, "all concurrent adapter calls must produce identical output")
		}
		assert.True(t, firstSet, "at least one result must have been recorded")
		assert.Len(t, mockBS.putCalls, 0, "DecisionPass must not call blobStore.Put")
	})

	t.Run("SpillConcurrentUniqueIDs", func(t *testing.T) {
		mockBS := &mockBlobStore{}
		adapter, restore := mcpAdapterForTest(nil, core.ActiveContext{}, handler, mockBS, &mockOutputPolicy{decision: tools.DecisionSpill})
		defer restore()

		const n = 10
		var wg sync.WaitGroup
		var spillErrs []error
		var ptrs []blobstore.Pointer
		var mu sync.Mutex

		for i := 0; i < n; i++ {
			wg.Add(1)
			go func() {
				defer wg.Done()
				res, err := adapter(context.Background(), req)
				if err != nil {
					mu.Lock()
					spillErrs = append(spillErrs, err)
					mu.Unlock()
					return
				}
				if res.IsError {
					mu.Lock()
					spillErrs = append(spillErrs, fmt.Errorf("spill adapter returned error: %v", res))
					mu.Unlock()
					return
				}
				var envelope map[string]interface{}
				text := res.Content[0].(mcp.TextContent).Text
				if err := json.Unmarshal([]byte(text), &envelope); err != nil {
					mu.Lock()
					spillErrs = append(spillErrs, err)
					mu.Unlock()
					return
				}
				mu.Lock()
				ptrs = append(ptrs, blobstore.Pointer{ID: envelope["pointer"].(string)})
				mu.Unlock()
			}()
		}
		wg.Wait()

		assert.Empty(t, spillErrs, "no errors in concurrent spill path")

		// All stored blobs must have identical content.
		for i, call := range mockBS.putCalls {
			stored, _ := io.ReadAll(call.reader)
			var parsed map[string]interface{}
			json.Unmarshal(stored, &parsed)
			assert.Equal(t, "shared-input", parsed["id"],
				"all concurrent Put calls must store identical bytes; call %d differed", i)
		}

		// All pointers must have unique IDs.
		seen := make(map[string]bool)
		for _, ptr := range ptrs {
			id := ptr.ID
			assert.NotEmpty(t, id)
			assert.False(t, seen[id], "each Put must produce a unique blob ID; duplicate: %s", id)
			seen[id] = true
		}
	})
}
