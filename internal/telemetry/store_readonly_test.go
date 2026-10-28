package telemetry

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"
)

// `Open` applies the DDL, which CREATES the telemetry database it is
// given. A read-only diagnostic must not materialise a store — the
// sidecar's "dry run" used to create one, and the critic's read-only
// hunt used to create one on every cycle.
//
// These tests pin that `OpenReadOnly` neither creates nor modifies.

// TestOpenReadOnly_DoesNotCreateTheStore is the regression.
func TestOpenReadOnly_DoesNotCreateTheStore(t *testing.T) {
	path := filepath.Join(t.TempDir(), "telemetry.db")

	_, err := OpenReadOnly(path)
	if err == nil {
		t.Fatal("OpenReadOnly succeeded on a missing store")
	}
	if !errors.Is(err, ErrStoreNotFound) {
		t.Errorf("error = %v, want ErrStoreNotFound", err)
	}
	if _, statErr := os.Stat(path); !os.IsNotExist(statErr) {
		t.Errorf("OpenReadOnly created the store at %s (stat err = %v)", path, statErr)
	}
}

// TestOpenReadOnly_ReadsAnExistingStore pins that the happy path works.
func TestOpenReadOnly_ReadsAnExistingStore(t *testing.T) {
	path := filepath.Join(t.TempDir(), "telemetry.db")
	store, err := Open(path)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	sid := "ses-1"
	in, out := int64(1000), int64(2000)
	now := time.Now().Unix()
	if _, err := store.InsertFrame(context.Background(), Frame{
		SchemaVersion: "1", EventType: "invocation",
		InvocationID: "inv-1", SessionID: &sid,
		Framework: "claude-code", Provider: "anthropic", Model: "m",
		StartedAt: now - 10, CompletedAt: now - 5, Status: "completed",
		InputTokens: &in, OutputTokens: &out,
		ProviderMetadata: []byte(`{}`),
	}); err != nil {
		t.Fatalf("InsertFrame: %v", err)
	}
	store.Close()

	ro, err := OpenReadOnly(path)
	if err != nil {
		t.Fatalf("OpenReadOnly: %v", err)
	}
	defer ro.Close()

	rows, err := ro.QuerySince(context.Background(), 0)
	if err != nil {
		t.Fatalf("QuerySince: %v", err)
	}
	if len(rows) != 1 {
		t.Fatalf("read %d row(s), want 1", len(rows))
	}
}

// TestOpenReadOnly_CannotWrite pins that the handle is read-only at the
// SQLite level, not merely by convention.
func TestOpenReadOnly_CannotWrite(t *testing.T) {
	path := filepath.Join(t.TempDir(), "telemetry.db")
	store, err := Open(path)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	store.Close()

	ro, err := OpenReadOnly(path)
	if err != nil {
		t.Fatalf("OpenReadOnly: %v", err)
	}
	defer ro.Close()

	if _, err := ro.DB().Exec(
		"INSERT INTO telemetry_invocation (invocation_id, framework, provider, model, " +
			"started_at, completed_at, received_at, status, provider_metadata, schema_version) " +
			"VALUES ('x','f','p','m',1,2,3,'completed','{}','1')"); err == nil {
		t.Fatal("a write through the read-only handle succeeded")
	}
}

// TestOpenReadOnly_RejectsAMissingSchema pins a clear error for a store
// whose ledger table is absent, rather than a confusing failure later in
// the hunt.
func TestOpenReadOnly_RejectsAMissingSchema(t *testing.T) {
	path := filepath.Join(t.TempDir(), "telemetry.db")
	if err := os.WriteFile(path, []byte("not a database"), 0o600); err != nil {
		t.Fatalf("write: %v", err)
	}
	_, err := OpenReadOnly(path)
	if err == nil {
		t.Fatal("OpenReadOnly succeeded on a file that is not a database")
	}
	if errors.Is(err, ErrStoreNotFound) {
		t.Errorf("error = %v, want a schema error, not ErrStoreNotFound", err)
	}
}

// TestOpenReadOnly_RejectsADirectory pins a clear error for a path that
// is not a file.
func TestOpenReadOnly_RejectsADirectory(t *testing.T) {
	dir := t.TempDir()
	_, err := OpenReadOnly(dir)
	if err == nil {
		t.Fatal("OpenReadOnly succeeded on a directory")
	}
	if errors.Is(err, ErrStoreNotFound) {
		t.Errorf("error = %v, want a directory error, not ErrStoreNotFound", err)
	}
}
