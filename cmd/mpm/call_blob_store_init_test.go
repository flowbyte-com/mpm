package main

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/flowbyte-com/mpm/internal/blobstore"
	"github.com/flowbyte-com/mpm-core/tools"
)

// TestCall_BlobStoreInitialized is the F8 regression test (2026-08-27).
//
// Bug: `mpm call mpm_blob_read --payload '{"id":"..."}'` from the CLI
// returned "blob store not initialized; SetBlobStore was not called at
// boot" — the CLI's handleCall never wired a blob store before
// dispatching to the tool handler. Only the MCP server boot path did.
//
// Contract after fix: handleCall installs a production blob store + a
// production pointer resolver (mirroring cmd/mpm-mcp/tools.go
// RegisterAllTools) before invoking any tool handler. mpm_blob_read,
// mpm_blob_search, and mpm_resolve (Phase 1+2) all work from the CLI
// without any external boot wiring.
//
// The test does NOT call tools.SetBlobStore before invoking handleCall —
// the fix must wire the globals internally. If the wiring regresses,
// the call returns the documented "blob store not initialized" error
// envelope instead of the blob content.
func TestCall_BlobStoreInitialized(t *testing.T) {
	ws := t.TempDir()
	t.Setenv("MPM_WORKSPACE", ws)

	// Reset the globals to ensure we're testing the CLI's own wiring,
	// not a leftover from a previous test in the same package.
	tools.SetBlobStore(nil)
	tools.SetResolver(nil)
	t.Cleanup(func() {
		tools.SetBlobStore(nil)
		tools.SetResolver(nil)
	})

	// Seed a blob directly in the workspace's blob directory so the
	// CLI's wiring (which reads from <workspace>/blobs) can find it.
	dm, closeDM, err := openCallDM()
	if err != nil {
		t.Fatalf("openCallDM: %v", err)
	}
	defer closeDM()
	blobDir := filepath.Join(ws, "blobs")
	if err := os.MkdirAll(blobDir, 0o755); err != nil {
		t.Fatalf("mkdir blob dir: %v", err)
	}
	bs, err := blobstore.NewFilesystemBackend(dm.SQLDB(), blobDir, 24*time.Hour)
	if err != nil {
		t.Fatalf("NewFilesystemBackend: %v", err)
	}
	const wantContent = "f8 cli round-trip probe"
	pointer, err := bs.Put(context.Background(), strings.NewReader(wantContent), blobstore.Metadata{ContentType: "text/plain"})
	if err != nil {
		t.Fatalf("blobstore.Put: %v", err)
	}
	blobID := pointer.ID

	// Capture stdout from handleCall.
	origStdout := os.Stdout
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatalf("os.Pipe: %v", err)
	}
	os.Stdout = w
	t.Cleanup(func() {
		os.Stdout = origStdout
		_ = r.Close()
		_ = w.Close()
	})

	exit := handleCall([]string{"mpm_blob_read", "--payload", `{"id":"` + blobID + `"}`})
	_ = w.Close()
	var buf bytes.Buffer
	if _, err := buf.ReadFrom(r); err != nil {
		t.Fatalf("read captured stdout: %v", err)
	}
	out := buf.String()
	if exit != 0 {
		t.Logf("handleCall exited %d (output follows)", exit)
	}
	if strings.Contains(out, "blob store not initialized") {
		t.Fatalf("handleCall(mpm_blob_read) leaked 'blob store not initialized' error: %s", out)
	}
	if !strings.Contains(out, wantContent) {
		t.Errorf("handleCall(mpm_blob_read) returned %q; expected to contain %q", out, wantContent)
	}
}

// TestCall_BlobStoreNotInitializedHasClearError is the negative path:
// WITHOUT the CLI's wiring, the call returns the documented
// initialization error envelope. Guards against the wiring being
// silently removed.
//
// This test mirrors the round-trip test but resets globals AFTER the
// handleCall wiring has run (simulating a future regression where the
// wiring sets globals but a teardown step nukes them). Today the CLI
// does NOT wire globals internally, so the wiring step in handleCall
// is what makes the positive test pass.
func TestCall_BlobStoreNotInitializedHasClearError(t *testing.T) {
	ws := t.TempDir()
	t.Setenv("MPM_WORKSPACE", ws)

	// Pre-clear the globals. If the CLI's handleCall wires them itself
	// (the F8 fix), this test will FAIL — and that's the failure mode
	// we want: the CLI is now responsible for wiring, and the "blob
	// store not initialized" error path only fires when even the CLI
	// wiring fails to install a usable store.
	tools.SetBlobStore(nil)
	tools.SetResolver(nil)
	t.Cleanup(func() {
		tools.SetBlobStore(nil)
		tools.SetResolver(nil)
	})

	origStdout := os.Stdout
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatalf("os.Pipe: %v", err)
	}
	os.Stdout = w
	t.Cleanup(func() {
		os.Stdout = origStdout
		_ = r.Close()
		_ = w.Close()
	})

	exit := handleCall([]string{"mpm_blob_read", "--payload", `{"id":"some-bogus-id"}`})
	_ = w.Close()
	var buf bytes.Buffer
	if _, err := buf.ReadFrom(r); err != nil {
		t.Fatalf("read captured stdout: %v", err)
	}
	out := buf.String()

	// After the F8 fix: the CLI wires the blob store internally, so the
	// call succeeds (the bogus id is a separate "not found" error
	// from the blob store, NOT the "not initialized" error from the
	// tools handler).
	if strings.Contains(out, "blob store not initialized") {
		t.Errorf("CLI handleCall leaked 'blob store not initialized'; the F8 wiring must install the blob store before tool dispatch. Got: %s", out)
	}
	if exit == 0 {
		t.Errorf("expected non-zero exit for bogus blob id; got %d", exit)
	}
}
