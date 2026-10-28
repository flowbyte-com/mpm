package main

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"io"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/flowbyte-com/mpm/internal/telemetry"
)

// ── helpers for the query-surface correction ─────────────────────────────

// seedStore creates a telemetry store at path with one invocation.
func seedStore(t *testing.T, path, invocationID, sessionID string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	store, err := telemetry.Open(path)
	if err != nil {
		t.Fatalf("open telemetry store %s: %v", path, err)
	}
	defer store.Close()
	sid := sessionID
	now := time.Now().Unix()
	in, out := int64(1000), int64(2000)
	res, err := store.InsertFrame(context.Background(), telemetry.Frame{
		SchemaVersion: "1", EventType: "invocation",
		InvocationID: invocationID, SessionID: &sid,
		Framework: "claude-code", Provider: "anthropic", Model: "test-model",
		StartedAt: now - 60, CompletedAt: now - 30, Status: "completed",
		InputTokens: &in, OutputTokens: &out,
		ProviderMetadata: json.RawMessage(`{}`),
	})
	if err != nil {
		t.Fatalf("InsertFrame: %v", err)
	}
	if !res.Inserted {
		t.Fatal("InsertFrame did not insert")
	}
}

// storeFingerprint hashes the store's database file, so a test can prove
// that reading it did not modify it.
func storeFingerprint(t *testing.T, path string) string {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read store %s: %v", path, err)
	}
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:])
}

// queryOutput is what a captured run produced.
type queryOutput struct {
	stdout string
	err    error
}

// captureQueryStdout runs fn with os.Stdout redirected.
func captureQueryStdout(t *testing.T, fn func() error) queryOutput {
	t.Helper()
	orig := os.Stdout
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatalf("pipe: %v", err)
	}
	os.Stdout = w
	runErr := fn()
	_ = w.Close()
	os.Stdout = orig
	out, _ := io.ReadAll(r)
	_ = r.Close()
	return queryOutput{stdout: string(out), err: runErr}
}
