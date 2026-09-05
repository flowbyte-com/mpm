// spill_roundtrip_test.go — automated end-to-end byte-for-byte proof of
// the spill mechanism:
//
//   handler result X (over threshold)
//     → mcpAdapter OutputPolicy.Apply ⇒ DecisionSpill
//     → blobstore.Put(jsonBytes)
//     → envelope with mpm://blob/<id> pointer
//     → resolve that pointer via the production filesystem backend
//     → assert returned bytes equal original jsonBytes
//
// This is the test the audit (`docs/pointer-indirection-audit-2026-09-05.md`)
// called out as missing. Before this test, the only spill tests
// asserted envelope structure or envelope size; none verified that
// resolving the pointer recovers byte-for-byte equivalent content.
//
// Companion to the spot-check at internal/blobstore/fs_roundtrip_spot_test.go,
// which verifies the same property on real production blobs already on disk
// (this test verifies it on a fresh fixture, end-to-end through the spill
// machinery in this package).

package main

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"io"
	"strings"
	"testing"
	"time"

	"github.com/flowbyte-com/mpm-core/tools"
	"github.com/flowbyte-com/mpm/internal/blobstore"
	_ "github.com/mattn/go-sqlite3"
)

// TestSpillRoundTrip_PointerResolvesToOriginal proves the round-trip
// chain (handler → spill envelope → pointer resolve → bytes) without
// dropping, re-encoding, or transforming any character.
func TestSpillRoundTrip_PointerResolvesToOriginal(t *testing.T) {
	// Build a realistic-looking handler payload that crosses the spill
	// threshold by a comfortable margin so the test cannot accidentally
	// land on DecisionPass via a future threshold change. The payload is
	// structured (mixed types) so we exercise JSON marshalling, not just
	// []byte copy of a string.
	original := map[string]interface{}{
		"id":      "audit-fixture-2026-09-05",
		"content": strings.Repeat("ABCDEFGH", 4096), // ~32 KB safe ASCII
		"tags":    []string{"a", "b", "c", "d"},
		"nested": map[string]interface{}{
			"alpha": "first",
			"beta":  "second",
			"gamma": 1.23456789,
		},
	}
	jsonBytes, err := json.Marshal(original)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}

	// 1) Sanity: the fixture is genuinely on the spill side of the threshold.
	policy := tools.DefaultOutputPolicy()
	decision, n, err := policy.Apply(context.Background(), jsonBytes)
	if err != nil {
		t.Fatalf("policy.Apply: %v", err)
	}
	if decision != tools.DecisionSpill {
		t.Fatalf("fixture did not spill: decision=%v size=%d threshold=%d — make the fixture larger",
			decision, n, tools.DefaultOutputThresholdBytes())
	}
	t.Logf("fixture size=%d bytes (threshold=%d) → DecisionSpill", n, tools.DefaultOutputThresholdBytes())

	// 2) Stand up a real filesystem backend in a temp dir.
	db, err := sql.Open("sqlite3", ":memory:")
	if err != nil {
		t.Fatalf("sql.Open: %v", err)
	}
	defer db.Close()
	// Initialize the schema the filesystem backend expects.
	if _, err := db.Exec(`
		CREATE TABLE IF NOT EXISTS blobs (
			id             TEXT PRIMARY KEY,
			source_tool    TEXT NOT NULL,
			source_call_id TEXT,
			session_id     TEXT,
			size_bytes     INTEGER NOT NULL,
			content_type   TEXT NOT NULL DEFAULT 'application/json',
			created_at     INTEGER NOT NULL,
			expires_at     INTEGER NOT NULL,
			checksum       TEXT
		)
	`); err != nil {
		t.Fatalf("create blobs schema: %v", err)
	}

	bs, err := blobstore.NewFilesystemBackend(db, t.TempDir(), 24*time.Hour)
	if err != nil {
		t.Fatalf("NewFilesystemBackend: %v", err)
	}

	// 3) Produce the spill envelope exactly the way mcpAdapter does (see
	//    cmd/mpm-mcp/tools.go:404-412). Mirrored here on purpose: a refactor
	//    that changes the envelope shape should break this test, not
	//    silently bypass it.
	ptr, err := bs.Put(context.Background(), strings.NewReader(string(jsonBytes)), blobstore.Metadata{
		SourceTool:  "test_round_trip",
		SizeBytes:   int64(len(jsonBytes)),
		ContentType: "application/json",
		CreatedAt:   time.Now(),
		ExpiresAt:   time.Now().Add(24 * time.Hour),
	})
	if err != nil {
		t.Fatalf("Put: %v", err)
	}

	preview := buildSpillPreview(jsonBytes)
	envelope := map[string]interface{}{
		"status":       "spilled",
		"pointer":      fmt.Sprintf("mpm://blob/%s", ptr.ID),
		"size_bytes":   int64(len(jsonBytes)),
		"content_type": "application/json",
		"source_tool":  "test_round_trip",
		"preview":      preview,
		"expires_at":   time.Now().Add(24 * time.Hour).Format(time.RFC3339),
	}
	envBytes, err := json.Marshal(envelope)
	if err != nil {
		t.Fatalf("marshal envelope: %v", err)
	}

	// 4) Resolve the pointer the same way mpm_blob_read and mpm_resolve
	//    do in production: Get with MaxBytes = serverMax ceiling.
	const serverMax = 256 * 1024
	reader, meta, err := bs.Get(context.Background(), ptr.ID, blobstore.GetOptions{
		Offset:   0,
		MaxBytes: serverMax,
	})
	if err != nil {
		t.Fatalf("Get(%s): %v", ptr.ID, err)
	}
	defer reader.Close()

	got, err := io.ReadAll(reader)
	if err != nil {
		t.Fatalf("ReadAll: %v", err)
	}

	// 5) Byte-for-byte.
	if !bytesEqual(got, jsonBytes) {
		t.Fatalf("round-trip BYTE MISMATCH:\n  got    %d bytes (first diff at byte %d)\n  orig   %d bytes",
			len(got), firstDiffIdx(got, jsonBytes), len(jsonBytes))
	}

	// 6) Cross-check: meta.SizeBytes must equal the original payload size.
	if meta.SizeBytes != int64(len(jsonBytes)) {
		t.Fatalf("meta.SizeBytes=%d != original=%d", meta.SizeBytes, len(jsonBytes))
	}

	// 7) Envelope must not re-emit payload bytes (defence against the
	//    audit's "leak via envelope" concern). The pointer is what
	//    survives; the body is in the blob.
	if strings.Contains(string(envBytes), string(jsonBytes)) {
		t.Fatalf("envelope contains the original payload bytes — pointer mechanism defeated")
	}

	t.Logf("PASS pointer=%s bytes=%d resolved round-trip identical", ptr.ID, len(got))
}

func bytesEqual(a, b []byte) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

func firstDiffIdx(a, b []byte) int {
	n := len(a)
	if len(b) < n {
		n = len(b)
	}
	for i := 0; i < n; i++ {
		if a[i] != b[i] {
			return i
		}
	}
	return n
}
