package blobstore

// fs_roundtrip_spot_test.go — manual byte-for-byte round-trip spot-check
// across real blob IDs on this host. Proves the production Get path
// returns the bytes that were Put, before adding any automated test.
//
// Run with: go test -tags fts5 -v -run TestRoundTrip_Spot ./internal/blobstore
// Skips on systems without /home/v/.mpm/blobs (no-op in CI).

import (
	"context"
	"database/sql"
	"io"
	"os"
	"path/filepath"
	"testing"

	_ "github.com/mattn/go-sqlite3"
)

const liveDB = "/home/v/workspace/projects/mpm/src/db/mpm.db"

func TestRoundTrip_Spot(t *testing.T) {
	const blobDir = "/home/v/.mpm/blobs"
	if _, err := os.Stat(blobDir); err != nil {
		t.Skipf("live blob dir %s not present; skipping spot-check (%v)", blobDir, err)
	}

	// 12 real blob IDs spanning every source-tool family in the audit.
	// Stable on this host only; skipped elsewhere.
	samples := []struct {
		id   string
		tool string
	}{
		{"55fa72e0-d105-41fe-a915-f617ca0372ac", "mpm_blob_read"},
		{"1e11de64-919a-4762-a832-631eccd88b9c", "mpm_blob_read"},
		{"b05c9308-a7a5-4ec5-b0d5-66bc4240b1a0", "mpm_context"},
		{"5d70fe41-f1f9-425b-89fa-923d5def45c4", "mpm_context"},
		{"d37724c2-648d-43d8-a945-cbffcc8661b2", "mpm_handoff"},
		{"83511b03-38c7-462e-bef2-64d6bf0af109", "mpm_lessons"},
		{"de7af01a-ac85-4d32-9026-accf705e39d5", "mpm_lessons"},
		{"296da47c-f405-419a-b6f0-20eaf8dfd6d6", "mpm_memory"},
		{"38cb2b57-11b8-401a-bacf-c6ef6d35d5f0", "mpm_resolve"},
		{"bfefd986-3380-4160-bc91-136d5c55f3f9", "mpm_resolve"},
		{"e1825dca-54b5-4801-bc0e-31442b37eff3", "mpm_work"},
		{"481a85cb-9d45-460e-b1de-b8c3b37ede1f", "mpm_work"},
	}

	db, err := sql.Open("sqlite3", "file:"+liveDB+"?mode=ro&immutable=1")
	if err != nil {
		t.Skipf("live DB %s not reachable: %v", liveDB, err)
	}
	defer db.Close()
	if _, err := db.Conn(context.Background()); err != nil {
		t.Skipf("live DB not reachable: %v", err)
	}

	// Open the production filesystem backend directly against the live dir
	// so we are exercising the same code path the mcpAdapter envelope
	// resolver hits when mpm_blob_read or mpm_resolve gets a pointer.
	fs, err := NewFilesystemBackend(db, blobDir, 0)
	if err != nil {
		t.Fatalf("NewFilesystemBackend(%s): %v", blobDir, err)
	}

	const serverMax = 256 * 1024 // same cap handlers.go uses

	passed, failed := 0, 0
	for _, s := range samples {
		// 1) on-disk bytes (independent ground truth).
		disk, err := os.ReadFile(filepath.Join(blobDir, s.id))
		if err != nil {
			t.Errorf("[%s/%s] read on-disk: %v", s.id, s.tool, err)
			failed++
			continue
		}

		// 2) Get via production code (same path callers hit via the
		//    `mpm://blob/<id>` resolver and `mpm_blob_read` handler).
		reader, meta, err := fs.Get(context.Background(), s.id, GetOptions{
			Offset:   0,
			MaxBytes: serverMax,
		})
		if err != nil {
			t.Errorf("[%s/%s] Get: %v", s.id, s.tool, err)
			failed++
			continue
		}
		got, rerr := io.ReadAll(reader)
		reader.Close()
		if rerr != nil {
			t.Errorf("[%s/%s] ReadAll: %v", s.id, s.tool, rerr)
			failed++
			continue
		}

		// 3) meta sanity.
		if meta.SizeBytes != int64(len(disk)) {
			t.Errorf("[%s/%s] SizeBytes=%d != disk len=%d (metadata-vs-disk drift)",
				s.id, s.tool, meta.SizeBytes, len(disk))
			failed++
			continue
		}

		// 4) byte-for-byte.
		if !bytesEqual(got, disk) {
			t.Errorf("[%s/%s] BYTE MISMATCH: get returned %d bytes (first diff at byte %d); disk %d bytes",
				s.id, s.tool, len(got), firstDiffIdx(got, disk), len(disk))
			failed++
			continue
		}
		t.Logf("PASS [%s] %s : %d B round-trip identical", s.id, s.tool, len(got))
		passed++
	}

	if failed > 0 {
		t.Fatalf("spot-check: %d/%d passed, %d failed", passed, len(samples), failed)
	}
	t.Logf("spot-check: %d/%d PASS across all source-tool families", passed, len(samples))
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
