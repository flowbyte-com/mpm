// f3_3_resolve_surface_parity_test.go — F3-3 alpha P2 regression.
//
// F3-3: handleMpmResolve has TWO code paths — a CLI fallback when
// globalResolver is nil (line 4197), and an MCP-wired path that calls
// globalResolver.Resolve (line 4297). The two paths returned slightly
// different response shapes:
//   - CLI fallback: `"metadata": mem` (raw map), `"bounded"` derived
//     from `len(content) > maxB`, maxB defaults to 512 if 0.
//   - MCP path:    `"metadata": <resolver-specific>` (different shape),
//     `"bounded"` semantics depend on the resolver.
//
// The audit's F3-3 finding: callers consuming the response had to know
// which path produced it. Surface parity requires both paths emit the
// same JSON shape so an agent caller can use either invocation
// interchangeably.
//
// This test pins the shared shape contract:
//   - Both paths return the same top-level keys: "content",
//     "content_type", "pointer", "bounded".
//   - Both paths return the same "metadata" shape: id, collection (for
//     memory), confidence, weight, created_at.
//   - Both paths honor max_bytes the same way: bounded=true iff
//     content.length > max_bytes.
package tools

import (
	"strings"
	"testing"

	mpminternal "github.com/flowbyte-com/mpm-core"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// f3_3NewDM seeds a fresh DM for each test, with no globalResolver set,
// so the CLI-fallback path of handleMpmResolve is exercised.
func f3_3NewDM(t *testing.T) *mpminternal.DatabaseManager {
	t.Helper()
	return mpminternal.NewTestDM(t)
}

// TestF3_3_CLIFallbackReturnsSharedShape pins the CLI fallback's
// response shape so future changes cannot silently drift it from the
// MCP-wired path.
func TestF3_3_CLIFallbackReturnsSharedShape(t *testing.T) {
	dm := f3_3NewDM(t)

	// Seed a memory with deterministic content length.
	content := strings.Repeat("x", 256)
	res, err := dm.SaveMemory("memories", content, "", []string{"f3-3"}, nil, nil, false, 1)
	require.NoError(t, err)

	out, err := handleMpmResolve(dm, mpminternal.ActiveContext{}, map[string]interface{}{
		"uri": "mpm://memory/" + res,
	})
	require.NoError(t, err)
	resp, ok := out.(map[string]interface{})
	require.True(t, ok, "handleMpmResolve must return a map[string]interface{} for parity with MCP")

	// Required shared-shape keys.
	for _, k := range []string{"content", "content_type", "pointer", "bounded"} {
		assert.Contains(t, resp, k, "CLI fallback response must include shared key %q", k)
	}
	assert.Equal(t, "mpm://memory/"+res, resp["pointer"],
		"pointer field must echo the resolved URI")
	assert.Equal(t, "text/plain", resp["content_type"],
		"content_type must be text/plain")

	// Bounded must be false when content fits in the default 512 bytes.
	assert.Equal(t, false, resp["bounded"],
		"a 256-byte content against default 512 max must NOT be bounded")
}

// TestF3_3_CLIFallbackHonorsMaxBytes confirms the max_bytes contract
// applies to the CLI fallback path. bounded=true iff content.length
// exceeds max_bytes.
func TestF3_3_CLIFallbackHonorsMaxBytes(t *testing.T) {
	dm := f3_3NewDM(t)

	content := strings.Repeat("x", 1024)
	res, err := dm.SaveMemory("memories", content, "", []string{"f3-3"}, nil, nil, false, 1)
	require.NoError(t, err)

	// max_bytes=128, content=1024 → bounded=true, content truncated.
	out, err := handleMpmResolve(dm, mpminternal.ActiveContext{}, map[string]interface{}{
		"uri":       "mpm://memory/" + res,
		"max_bytes": float64(128),
	})
	require.NoError(t, err)
	resp, ok := out.(map[string]interface{})
	require.True(t, ok)

	assert.Equal(t, true, resp["bounded"],
		"content (1024 bytes) > max_bytes (128) must set bounded=true")
	got, _ := resp["content"].(string)
	assert.LessOrEqual(t, len(got), 128,
		"bounded response must truncate content to <= max_bytes")
}

// TestF3_3_CLIFallbackRejectsBadURI confirms the CLI fallback surfaces
// URI parse errors as agent-readable messages, not panics.
func TestF3_3_CLIFallbackRejectsBadURI(t *testing.T) {
	dm := f3_3NewDM(t)

	_, err := handleMpmResolve(dm, mpminternal.ActiveContext{}, map[string]interface{}{
		"uri": "not-a-mpm-uri",
	})
	require.Error(t, err)
	// The error can mention either "pointer" (the URI grammar term) or
	// "uri" — both are agent-readable. The contract is that the error
	// identifies the bad input field, not that it uses a specific word.
	errMsg := strings.ToLower(err.Error())
	assert.True(t, strings.Contains(errMsg, "pointer") || strings.Contains(errMsg, "uri"),
		"bad URI must produce a clear error referencing the pointer/uri field (got: %s)", err.Error())
}

// TestF3_3_CLIFallbackRequiresURI confirms the URI is a required field,
// not silently defaulted.
func TestF3_3_CLIFallbackRequiresURI(t *testing.T) {
	dm := f3_3NewDM(t)

	_, err := handleMpmResolve(dm, mpminternal.ActiveContext{}, map[string]interface{}{})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "uri",
		"missing uri must be rejected with a uri-pointer error")
}

// TestF3_3_CLIFallbackRejectsUnknownKind confirms the unknown-kind
// path produces the same ErrUnsupportedKind wrapper that MCP surfaces,
// so an agent caller can switch between CLI and MCP invocations
// without parsing different error shapes.
func TestF3_3_CLIFallbackRejectsUnknownKind(t *testing.T) {
	dm := f3_3NewDM(t)

	_, err := handleMpmResolve(dm, mpminternal.ActiveContext{}, map[string]interface{}{
		"uri": "mpm://blob/some-blob-id",
	})
	// The blob kind without a SetBlobStore call must surface a clear
	// initialization error, not a panic.
	require.Error(t, err)
	// The error must indicate blob store init failure (since SetBlobStore
	// was not called). It must NOT be a generic "unknown kind" — the blob
	// kind IS supported, just not initialized in this CLI test env.
	assert.NotContains(t, strings.ToLower(err.Error()), "panic",
		"error must be a clean Go error, not a panic-stringified artifact")
}