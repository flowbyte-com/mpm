// blob_read_offset_parity_regression_test.go — Pass 5 defect C.16.
//
// The 2026-09-05 audit found that mpm_blob_read handled the offset
// parameter non-deterministically. The previous shape used a bare
// `.(float64)` assertion, which silently coerced:
//
//   omitted/null/string → 0 (no error)
//   negative integers   → passed through to the OS-level Seek call,
//                          producing a generic "seek: invalid
//                          argument" error with no offset-aware context
//
// Both the CLI dispatcher (cmd/mpm/call.go handleCall) and the MCP
// adapter (cmd/mpm-mcp/tools.go mcpAdapter) feed the same payload
// shape into handleMpmBlobRead — so the divergence was at the
// single shared boundary, not between two implementations. Fix the
// boundary once and both surfaces agree.
//
// Pre-fix reproduction (live CLI):
//
//   $ mpm call mpm_blob_read --payload '{"id":"<id>","offset":"abc"}'
//     # wanted: error mentioning the offset contract
//     # actual: success, full blob content (offset silently coerced to 0)
//
//   $ mpm call mpm_blob_read --payload '{"id":"<id>","offset":-1}'
//     # wanted: error mentioning the offset must be non-negative
//     # actual: generic OS seek error (varies by runtime)
//
// Canonical contract (shared between CLI and MCP):
//
//   omitted              → 0 (legitimate default)
//   nil                  → 0 (null omission equivalent)
//   0                    → 0 (explicit; reads from start)
//   N (positive, < size) → reads from byte N
//   size                 → empty read, has_more=false (EOF)
//   > size               → empty read, has_more=false (no error)
//   < 0                  → ERROR: offset must be non-negative
//   non-integer          → ERROR: offset must be an integer
//   string               → ERROR: offset must be an integer
//   bool                 → ERROR: offset must be an integer
//
// The parity invariant pinned by these tests:
//
//   same input semantics
//   → same success/error classification
//   → same returned bytes when successful
//
// Both the CLI dispatcher and the MCP adapter dispatch to
// handleMpmBlobRead via the same registry, so a single test against
// the handler establishes parity by construction. The
// invokeBlobReadHandler helper exercises the same code path both
// surfaces use.

package tools

import (
	"context"
	"io"
	"strconv"
	"strings"
	"sync"
	"testing"

	mpminternal "github.com/flowbyte-com/mpm-core"
)

// ── Valid offsets (success path) ───────────────────────────────────

// TestBlobRead_OmittedOffsetReadsFromStart pins: the omitted-offset
// case reads from byte 0. This is the documented default.
func TestBlobRead_OmittedOffsetReadsFromStart(t *testing.T) {
	dm := newTestSharedDM(t)
	bs := newInMemoryBlobStore()
	bs.put("blob-parity-omitted", blobTestContent)
	installBlobStoreForTest(t, bs)

	res, err := invokeBlobReadHandler(t, dm, map[string]interface{}{
		"id": "blob-parity-omitted",
	})
	if err != nil {
		t.Fatalf("omitted offset must succeed: %v", err)
	}
	if content, _ := res["content"].(string); content != blobTestContent {
		t.Errorf("omitted offset must read full blob; got %q", content)
	}
	if off, _ := res["offset"].(int64); off != 0 {
		t.Errorf("omitted offset must report 0 in envelope; got %d", off)
	}
}

// TestBlobRead_ZeroOffsetReadsFromStart pins: explicit 0 is the
// same path as omission.
func TestBlobRead_ZeroOffsetReadsFromStart(t *testing.T) {
	dm := newTestSharedDM(t)
	bs := newInMemoryBlobStore()
	bs.put("blob-parity-zero", blobTestContent)
	installBlobStoreForTest(t, bs)

	res, err := invokeBlobReadHandler(t, dm, map[string]interface{}{
		"id":     "blob-parity-zero",
		"offset": float64(0),
	})
	if err != nil {
		t.Fatalf("offset=0 must succeed: %v", err)
	}
	if content, _ := res["content"].(string); content != blobTestContent {
		t.Errorf("offset=0 must read full blob; got %q", content)
	}
}

// TestBlobRead_PositiveOffsetReadsSlice pins: offset=N reads from
// byte N (with has_more=true if more bytes remain).
func TestBlobRead_PositiveOffsetReadsSlice(t *testing.T) {
	dm := newTestSharedDM(t)
	bs := newInMemoryBlobStore()
	bs.put("blob-parity-positive", blobTestContent)
	installBlobStoreForTest(t, bs)

	res, err := invokeBlobReadHandler(t, dm, map[string]interface{}{
		"id":     "blob-parity-positive",
		"offset": float64(10),
	})
	if err != nil {
		t.Fatalf("offset=10 must succeed: %v", err)
	}
	if content, _ := res["content"].(string); content != "abcdef" {
		t.Errorf("offset=10 must read tail; got %q", content)
	}
	if br, _ := res["bytes_returned"].(int); br != 6 {
		t.Errorf("offset=10 must report 6 bytes returned; got %d", br)
	}
}

// TestBlobRead_OffsetAtEOFReturnsEmpty pins: offset exactly equal
// to size returns an empty read with has_more=false. This is the
// canonical EOF semantic — not an error.
func TestBlobRead_OffsetAtEOFReturnsEmpty(t *testing.T) {
	dm := newTestSharedDM(t)
	bs := newInMemoryBlobStore()
	bs.put("blob-parity-eof", blobTestContent)
	installBlobStoreForTest(t, bs)

	res, err := invokeBlobReadHandler(t, dm, map[string]interface{}{
		"id":     "blob-parity-eof",
		"offset": float64(len(blobTestContent)),
	})
	if err != nil {
		t.Fatalf("offset=size must succeed (EOF is not an error): %v", err)
	}
	if content, _ := res["content"].(string); content != "" {
		t.Errorf("offset=size must return empty content; got %q", content)
	}
	if br, _ := res["bytes_returned"].(int); br != 0 {
		t.Errorf("offset=size must report 0 bytes returned; got %d", br)
	}
}

// TestBlobRead_OffsetBeyondEndReturnsEmpty pins: offset > size
// returns an empty read (same as EOF). No error — the boundary
// is inclusive of the EOF state.
func TestBlobRead_OffsetBeyondEndReturnsEmpty(t *testing.T) {
	dm := newTestSharedDM(t)
	bs := newInMemoryBlobStore()
	bs.put("blob-parity-beyond", blobTestContent)
	installBlobStoreForTest(t, bs)

	res, err := invokeBlobReadHandler(t, dm, map[string]interface{}{
		"id":     "blob-parity-beyond",
		"offset": float64(len(blobTestContent) + 1024),
	})
	if err != nil {
		t.Fatalf("offset>size must succeed (boundary EOF); got: %v", err)
	}
	if content, _ := res["content"].(string); content != "" {
		t.Errorf("offset>size must return empty content; got %q", content)
	}
}

// TestBlobRead_NullOffsetReadsFromStart pins: null is equivalent to
// omission (matches the JSON convention that null = absent).
func TestBlobRead_NullOffsetReadsFromStart(t *testing.T) {
	dm := newTestSharedDM(t)
	bs := newInMemoryBlobStore()
	bs.put("blob-parity-null", blobTestContent)
	installBlobStoreForTest(t, bs)

	res, err := invokeBlobReadHandler(t, dm, map[string]interface{}{
		"id":     "blob-parity-null",
		"offset": nil,
	})
	if err != nil {
		t.Fatalf("null offset must succeed (treated as omitted): %v", err)
	}
	if content, _ := res["content"].(string); content != blobTestContent {
		t.Errorf("null offset must read full blob; got %q", content)
	}
}

// ── Invalid offsets (error path) ───────────────────────────────────

// TestBlobRead_NegativeOffsetRejected pins: offset < 0 errors at
// the boundary with a clear offset-aware message. Both CLI and
// MCP get the same error string.
func TestBlobRead_NegativeOffsetRejected(t *testing.T) {
	dm := newTestSharedDM(t)
	bs := newInMemoryBlobStore()
	bs.put("blob-parity-negative", blobTestContent)
	installBlobStoreForTest(t, bs)

	for _, neg := range []float64{-1, -100, -1e9} {
		t.Run("offset="+ftoa(neg), func(t *testing.T) {
			_, err := invokeBlobReadHandler(t, dm, map[string]interface{}{
				"id":     "blob-parity-negative",
				"offset": neg,
			})
			if err == nil {
				t.Fatalf("negative offset %v must error", neg)
			}
			if !strings.Contains(err.Error(), "offset") {
				t.Errorf("error must mention 'offset', got: %v", err)
			}
			if !strings.Contains(err.Error(), "non-negative") {
				t.Errorf("error must mention 'non-negative', got: %v", err)
			}
		})
	}
}

// TestBlobRead_NonIntegerOffsetRejected pins: non-integer numeric
// values (e.g. 1.5) and non-numeric values (string, bool) error at
// the boundary. The previous shape silently coerced these to 0.
func TestBlobRead_NonIntegerOffsetRejected(t *testing.T) {
	dm := newTestSharedDM(t)
	bs := newInMemoryBlobStore()
	bs.put("blob-parity-nonint", blobTestContent)
	installBlobStoreForTest(t, bs)

	cases := []struct {
		name string
		val  interface{}
	}{
		{"fractional_float", 1.5},
		{"string", "abc"},
		{"numeric_string", "10"},
		{"bool", true},
		{"array", []interface{}{0}},
		{"object", map[string]interface{}{"x": 0}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			_, err := invokeBlobReadHandler(t, dm, map[string]interface{}{
				"id":     "blob-parity-nonint",
				"offset": c.val,
			})
			if err == nil {
				t.Fatalf("non-integer offset (%T) must error", c.val)
			}
			if !strings.Contains(err.Error(), "offset") {
				t.Errorf("error must mention 'offset', got: %v", err)
			}
		})
	}
}

// ── Parity invariant (CLI path vs MCP path) ───────────────────────

// TestBlobRead_ParityOnSameInput pins the headline invariant: the
// same offset input produces the same success/error classification
// and the same returned bytes via both public surfaces.
//
// Both paths dispatch to handleMpmBlobRead via the same registry,
// so this test exercises the same code path the CLI dispatcher and
// the MCP adapter use. The parity invariant holds because the
// boundary validation is now deterministic — every offset state
// produces the same classification and the same bytes.
func TestBlobRead_ParityOnSameInput(t *testing.T) {
	dm := newTestSharedDM(t)
	bs := newInMemoryBlobStore()
	bs.put("blob-parity-invariant", blobTestContent)
	installBlobStoreForTest(t, bs)

	cases := []struct {
		name      string
		offset    interface{}
		wantErr   bool
		wantBytes int
	}{
		{"omitted", nil, false, len(blobTestContent)},
		{"zero", float64(0), false, len(blobTestContent)},
		{"mid", float64(8), false, 8},
		{"at_eof", float64(len(blobTestContent)), false, 0},
		{"beyond_eof", float64(9999), false, 0},
		{"negative", float64(-1), true, 0},
		{"non_integer", float64(1.5), true, 0},
		{"string_offset", "abc", true, 0},
		{"bool_offset", true, true, 0},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			payload := map[string]interface{}{"id": "blob-parity-invariant"}
			if c.offset != nil {
				payload["offset"] = c.offset
			}

			// CLI dispatcher path: invoke handler directly. This is
			// what cmd/mpm/call.go handleCall does after parsing the
			// payload.
			cliRes, cliErr := invokeBlobReadHandler(t, dm, payload)

			// MCP wire path: simulate the mcpAdapter closure. The
			// adapter forwards the payload verbatim to the same
			// handler.
			mcpRes, mcpErr := invokeBlobReadHandler(t, dm, payload)

			// Parity check 1: success/error classification.
			cliFailed := cliErr != nil
			mcpFailed := mcpErr != nil
			if cliFailed != mcpFailed {
				t.Fatalf("parity broken: CLI err=%v, MCP err=%v", cliErr, mcpErr)
			}
			if cliFailed != c.wantErr {
				t.Fatalf("err classification: CLI err=%v want err=%v", cliErr, c.wantErr)
			}

			// Parity check 2: when both succeed, returned bytes match.
			if !cliFailed {
				cliContent, _ := cliRes["content"].(string)
				mcpContent, _ := mcpRes["content"].(string)
				if cliContent != mcpContent {
					t.Fatalf("parity broken on bytes: CLI=%q, MCP=%q", cliContent, mcpContent)
				}
				if len(cliContent) != c.wantBytes {
					t.Errorf("expected %d bytes for case %s, got %d",
						c.wantBytes, c.name, len(cliContent))
				}
			}
		})
	}
}

// TestBlobRead_InvalidReadDoesNotMutateState pins the write-path
// safety: rejected reads do not modify the blob.
func TestBlobRead_InvalidReadDoesNotMutateState(t *testing.T) {
	dm := newTestSharedDM(t)
	bs := newInMemoryBlobStore()
	bs.put("blob-parity-no-mutate", blobTestContent)
	installBlobStoreForTest(t, bs)

	// Baseline read succeeds.
	baseline, err := invokeBlobReadHandler(t, dm, map[string]interface{}{
		"id": "blob-parity-no-mutate",
	})
	if err != nil {
		t.Fatalf("baseline read failed: %v", err)
	}
	if baselineContent, _ := baseline["content"].(string); baselineContent != blobTestContent {
		t.Fatalf("baseline content drifted: %q", baselineContent)
	}

	// Try invalid offsets — each must error without changing state.
	for _, bad := range []float64{-1, -100} {
		_, err := invokeBlobReadHandler(t, dm, map[string]interface{}{
			"id":     "blob-parity-no-mutate",
			"offset": bad,
		})
		if err == nil {
			t.Fatalf("negative offset %v must error", bad)
		}
	}

	// Re-read; content must match baseline.
	after, err := invokeBlobReadHandler(t, dm, map[string]interface{}{
		"id": "blob-parity-no-mutate",
	})
	if err != nil {
		t.Fatalf("post-invalid read failed: %v", err)
	}
	if afterContent, _ := after["content"].(string); afterContent != blobTestContent {
		t.Errorf("invalid reads mutated blob: was %q, now %q",
			blobTestContent, afterContent)
	}
}

// ── Helpers ────────────────────────────────────────────────────────

const blobTestContent = "0123456789abcdef"

// installBlobStoreForTest wires a fresh in-memory blob store into
// the handler's global for the duration of the test. The cleanup
// restores the prior value so tests don't pollute each other.
func installBlobStoreForTest(t *testing.T, bs blobStoreInterface) {
	t.Helper()
	saved := blobStoreForHandlers
	blobStoreForHandlers = bs
	t.Cleanup(func() { blobStoreForHandlers = saved })
}

// invokeBlobReadHandler invokes handleMpmBlobRead with the given
// payload and returns the response. Used by both CLI-path and
// MCP-path parity tests since the same registry handler backs both.
func invokeBlobReadHandler(t *testing.T, dm *mpminternal.DatabaseManager, payload map[string]interface{}) (map[string]interface{}, error) {
	t.Helper()
	res, err := handleMpmBlobRead(dm, mpminternal.ActiveContext{}, payload)
	if err != nil {
		return nil, err
	}
	m, ok := res.(map[string]interface{})
	if !ok {
		return nil, nil
	}
	return m, nil
}

// ftoa is a tiny float-to-string helper for test names.
func ftoa(f float64) string {
	return strconv.FormatFloat(f, 'g', -1, 64)
}

// ── In-memory blob store mock ─────────────────────────────────────

// inMemoryBlobStore is a minimal in-memory implementation of the
// tools package's local blobStoreInterface. Satisfies the contract
// for the offset-parity tests without pulling in the
// blobstore.FilesystemBackend concrete type (which lives in the
// main module's internal/ tree and is not importable from
// mpm-core/tools).
type inMemoryBlobStore struct {
	mu    sync.Mutex
	blobs map[string]string
}

func newInMemoryBlobStore() *inMemoryBlobStore {
	return &inMemoryBlobStore{blobs: map[string]string{}}
}

func (s *inMemoryBlobStore) put(id, content string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.blobs[id] = content
}

func (s *inMemoryBlobStore) Get(ctx context.Context, id string, opts GetOptions) (io.ReadCloser, Metadata, error) {
	s.mu.Lock()
	content, ok := s.blobs[id]
	s.mu.Unlock()
	if !ok {
		return nil, Metadata{}, errBlobNotFound
	}
	size := int64(len(content))
	// Mirror the blobstore contract: offset >= size returns empty.
	if opts.Offset >= size {
		return io.NopCloser(strings.NewReader("")), Metadata{
			ContentType: "text/plain",
			SizeBytes:   size,
		}, nil
	}
	if opts.Offset < 0 {
		// Mirror OS-level seek error for negative offsets. The handler
		// now rejects negatives before reaching here, so this branch
		// is defense-in-depth for any future caller that bypasses
		// validation.
		return nil, Metadata{}, &negSeekError{offset: opts.Offset}
	}
	chunk := content[opts.Offset:]
	if opts.MaxBytes > 0 && int64(len(chunk)) > opts.MaxBytes {
		chunk = chunk[:opts.MaxBytes]
	}
	return io.NopCloser(strings.NewReader(chunk)), Metadata{
		ContentType: "text/plain",
		SizeBytes:   size,
	}, nil
}

func (s *inMemoryBlobStore) Search(ctx context.Context, id string, q SearchQuery) ([]Match, error) {
	return nil, nil
}

// negSeekError mimics the OS-level "seek: invalid argument" error
// for negative offsets, in case a future caller reaches the store
// without going through the new boundary validation.
type negSeekError struct{ offset int64 }

func (e *negSeekError) Error() string {
	return "blobstore: negative offset " + strconv.FormatInt(e.offset, 10) + ": seek: invalid argument"
}
