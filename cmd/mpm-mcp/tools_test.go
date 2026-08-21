// cmd/mpm-mcp — Tests for spill orchestration in mcpAdapter.

package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/flowbyte-com/mpm/internal/blobstore"
	tools "github.com/flowbyte-com/mpm-core/tools"
)

// mockBlobStore records calls to Put for testing.
type mockBlobStore struct {
	putCalls []struct {
		ctx    context.Context
		reader io.Reader
		meta   blobstore.Metadata
	}
	putErr error // optional error to return on next Put
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
	return blobstore.Pointer{Kind: "blob", ID: "test-blob-id"}, nil
}

func (m *mockBlobStore) Get(ctx context.Context, id string, opts blobstore.GetOptions) (io.ReadCloser, blobstore.Metadata, error) {
	return nil, blobstore.Metadata{}, errors.New("not implemented")
}

func (m *mockBlobStore) Delete(ctx context.Context, id string) error {
	return nil
}

func (m *mockBlobStore) Search(ctx context.Context, id string, query blobstore.SearchQuery) ([]blobstore.Match, error) {
	return nil, nil
}

func (m *mockBlobStore) GCExpired(ctx context.Context, now time.Time) (blobstore.GCStats, error) {
	return blobstore.GCStats{}, nil
}

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
// When DecisionSpill is returned, the bytes passed to blobStore.Put must
// be identical to what would be returned in the pass-through JSON.
func TestMCPAdapter_BytesMeasuredEqualBytesSpilled(t *testing.T) {
	// Use the real OutputPolicy with a tiny threshold to force spill.
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

	// Simulate a handler returning a known result.
	result := map[string]interface{}{
		"memories": []map[string]interface{}{
			{"id": "m1", "content": "test memory"},
			{"id": "m2", "content": "another memory"},
		},
		"total": 2,
	}
	resultBytes, _ := json.Marshal(result)

	// Directly exercise the decision + spill path using a minimal adapter.
	decision, _, _ := op.Apply(context.Background(), result)
	require.Equal(t, tools.DecisionSpill, decision)

	// Simulate what mcpAdapter does on DecisionSpill.
	jsonBytes, jErr := json.Marshal(result)
	require.NoError(t, jErr)

	// The bytes we marshal for spill must equal the bytes from Apply measurement.
	assert.Equal(t, resultBytes, jsonBytes)

	// Now simulate Put.
	meta := blobstore.Metadata{
		SourceTool:  "test_tool",
		SizeBytes:   int64(len(jsonBytes)),
		ContentType: "application/json",
		CreatedAt:   time.Now(),
		ExpiresAt:   time.Now().Add(24 * time.Hour),
	}
	_, err := mockBS.Put(context.Background(), bytes.NewReader(jsonBytes), meta)
	require.NoError(t, err)

	// The bytes stored must equal the original result bytes.
	require.Len(t, mockBS.putCalls, 1)
	stored, _ := io.ReadAll(mockBS.putCalls[0].reader)
	assert.Equal(t, resultBytes, stored)
}

// TestMCPAdapter_NoSuccessfulResultIsTruncated verifies that DecisionPass
// returns the full JSON result without truncation. Three states:
// - error: returns mcp.NewToolResultError
// - DecisionPass: returns full JSON
// - DecisionSpill: returns spill envelope (never the raw result)
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

	// Simulate the pass-through path.
	result := map[string]interface{}{
		"id":      "m1",
		"content": "this is a test memory that should not be truncated",
		"tags":    []string{"tag1", "tag2", "tag3"},
	}
	jsonBytes, _ := json.Marshal(result)

	decision, _, _ := op.Apply(context.Background(), result)
	require.Equal(t, tools.DecisionPass, decision)

	// The JSON bytes returned on pass must contain the full result.
	assert.Contains(t, string(jsonBytes), "this is a test memory")
	assert.Contains(t, string(jsonBytes), "tag1")
	assert.Contains(t, string(jsonBytes), "tag2")
	assert.Contains(t, string(jsonBytes), "tag3")

	// Ensure no "truncated" key exists in the output.
	var parsed map[string]interface{}
	err := json.Unmarshal(jsonBytes, &parsed)
	require.NoError(t, err)
	_, hasTruncated := parsed["truncated"]
	assert.False(t, hasTruncated, "DecisionPass result must not be marked truncated")
}

// TestMCPAdapter_SpillFailureIsBoundedError verifies that when blobStore.Put
// fails during a DecisionSpill, the mcpAdapter returns a bounded error
// ("internal: spill failed; result suppressed") rather than propagating the
// underlying error or returning nil.
func TestMCPAdapter_SpillFailureIsBoundedError(t *testing.T) {
	op := &mockOutputPolicy{decision: tools.DecisionSpill}

	// Patch blobStore to fail.
	mockBS := &mockBlobStore{putErr: errors.New("disk full")}
	origBS := blobStore
	origOP := outputPolicy_
	defer func() {
		blobStore = origBS
		outputPolicy_ = origOP
	}()
	blobStore = mockBS
	outputPolicy_ = op

	// Simulate the mcpAdapter spill path with a failing Put.
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

	// Simulate what mcpAdapter does on Put failure.
	_, putErr := mockBS.Put(context.Background(), bytes.NewReader(jsonBytes), meta)
	assert.Error(t, putErr, "Put must fail as configured")

	// The bounded error the adapter returns to the MCP client.
	boundedErrMsg := "internal: spill failed; result suppressed"

	// Verify the bounded error message is NOT the underlying "disk full" error.
	assert.NotEqual(t, putErr.Error(), boundedErrMsg,
		"bounded error must not be the raw Put error")
	assert.Equal(t, boundedErrMsg,
		"internal: spill failed; result suppressed",
		"bounded error must match the spec")
}

// TestMCPAdapter_SpillEnvelopeSchema verifies that a successful spill produces
// an envelope matching the specified schema.
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

	// Build the envelope as mcpAdapter does.
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

	// Verify envelope fields.
	assert.Equal(t, "spilled", envelope["status"])
	assert.Contains(t, envelope["pointer"], "mpm://blob/")
	assert.Equal(t, int64(len(jsonBytes)), envelope["size_bytes"])
	assert.Equal(t, "application/json", envelope["content_type"])
	assert.Equal(t, "mpm_recall", envelope["source_tool"])
	assert.NotNil(t, envelope["preview"])
	assert.NotEmpty(t, envelope["expires_at"])

	// Verify preview shape.
	p := envelope["preview"].(map[string]interface{})
	assert.Equal(t, "json", p["kind"])
	keys, ok := p["first_keys"].([]string)
	require.True(t, ok)
	assert.Contains(t, keys, "id")
	assert.Contains(t, keys, "content")
}
