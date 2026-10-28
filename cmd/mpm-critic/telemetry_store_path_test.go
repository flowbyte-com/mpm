package main

import (
	"context"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"testing"
	"time"

	mpmcore "github.com/flowbyte-com/mpm-core"

	"github.com/flowbyte-com/mpm/internal/telemetry"
)

// The critic's telemetry hunt looks for the telemetry ledger at
// "<projectRoot>/telemetry.db" — the path docs/SPEC.md §6.6 names.
//
// The COLLECTOR has never written there. It writes to
// "<projectRoot>/src/db/telemetry.db", the same "src/db/" layout the
// substrate uses for mpm.db. So the hunt was reading a path nothing
// writes to.
//
// It did not fail loudly, because `telemetry.Open` applies the DDL and
// therefore CREATES an empty ledger at the path it was given. The hunt
// then found nothing and reported zero observations, every cycle,
// forever — a diagnostic that could never fire.
//
// The correction merged in #138 made the hunt read-only but kept the
// wrong path, so it stayed inert; and its test seeded the same wrong
// path the code used, which made the test self-consistent and
// unrepresentative.
//
// These tests pin the CANONICAL path.

// criticWorkspace builds a disposable substrate WITHOUT a telemetry
// ledger, so each test can decide whether one exists.
func criticWorkspace(t *testing.T) (string, *mpmcore.DatabaseManager) {
	t.Helper()
	t.Setenv("HOME", t.TempDir())
	ws := t.TempDir()
	t.Setenv("MPM_WORKSPACE", ws)
	t.Setenv("MPM_DB_PATH", filepath.Join(ws, "src", "db", "mpm.db"))
	// The override is deliberately unset: the canonical resolution is
	// what these tests exercise.
	t.Setenv("MPM_TELEMETRY_DB", "")

	dm, err := mpmcore.NewDatabaseManager(ws)
	if err != nil {
		t.Fatalf("NewDatabaseManager(%q): %v", ws, err)
	}
	t.Cleanup(func() { _ = dm.Close() })
	if err := dm.InitSchema(); err != nil {
		t.Fatalf("InitSchema: %v", err)
	}
	return ws, dm
}

// seedLedgerAt writes a qualifying invocation into the ledger at path.
func seedLedgerAt(t *testing.T, path, sessionID string) {
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
	in, out := int64(120000), int64(10000)
	res, err := store.InsertFrame(context.Background(), telemetry.Frame{
		SchemaVersion: "1", EventType: "invocation",
		InvocationID: "inv-critic-" + sessionID, SessionID: &sid,
		Framework: "claude-code", Provider: "anthropic", Model: "test-model",
		StartedAt: now - 60, CompletedAt: now - 30, Status: "completed",
		InputTokens: &in, OutputTokens: &out,
		ProviderMetadata: []byte(`{}`),
	})
	if err != nil {
		t.Fatalf("InsertFrame: %v", err)
	}
	if !res.Inserted {
		t.Fatal("InsertFrame did not insert")
	}
}

// TestTelemetryHunt_UsesTheCanonicalStorePath is the regression.
//
// The hunt must find the ledger where the COLLECTOR writes it.
func TestTelemetryHunt_UsesTheCanonicalStorePath(t *testing.T) {
	ws, dm := criticWorkspace(t)
	seedLedgerAt(t, telemetry.StorePath(ws), "ses-canonical")

	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	findings, err := runTelemetryHunt(context.Background(), logger, ws, dm.SQLDB(), 7)
	if err != nil {
		t.Fatalf("runTelemetryHunt: %v", err)
	}
	if len(findings) != 1 {
		t.Fatalf("expected 1 observation from the canonical ledger, got %d — "+
			"the hunt is looking somewhere the collector does not write", len(findings))
	}
	if findings[0].SessionID != "ses-canonical" {
		t.Errorf("observation session = %q, want %q", findings[0].SessionID, "ses-canonical")
	}

	// The SPEC's path must not have been used — and, because the hunt
	// is read-only, it must not have been CREATED either.
	legacy := filepath.Join(ws, "telemetry.db")
	if _, err := os.Stat(legacy); !os.IsNotExist(err) {
		t.Errorf("the hunt created a ledger at the legacy path %s (stat err = %v); "+
			"a read-only hunt must not materialise a store", legacy, err)
	}
}

// TestTelemetryHunt_WithoutALedgerFindsNothing pins that a missing
// ledger is "no observations", not a failure — and that the hunt does
// not create one.
func TestTelemetryHunt_WithoutALedgerFindsNothing(t *testing.T) {
	ws, dm := criticWorkspace(t)

	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	findings, err := runTelemetryHunt(context.Background(), logger, ws, dm.SQLDB(), 7)
	if err != nil {
		t.Fatalf("runTelemetryHunt = %v; a workspace with no ledger has no observations", err)
	}
	if len(findings) != 0 {
		t.Errorf("expected 0 observations, got %d", len(findings))
	}
	if _, err := os.Stat(telemetry.StorePath(ws)); !os.IsNotExist(err) {
		t.Errorf("the hunt created the canonical ledger (stat err = %v); "+
			"a read-only hunt must not materialise a store", err)
	}
}

// TestTelemetryHunt_HonoursTheTelemetryDBOverride pins that the
// collector's override is honoured by the hunt, so a collector running
// with a non-default store is still read.
func TestTelemetryHunt_HonoursTheTelemetryDBOverride(t *testing.T) {
	ws, dm := criticWorkspace(t)
	override := filepath.Join(ws, "custom", "ledger.db")
	t.Setenv("MPM_TELEMETRY_DB", override)
	seedLedgerAt(t, override, "ses-override")

	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	findings, err := runTelemetryHunt(context.Background(), logger, ws, dm.SQLDB(), 7)
	if err != nil {
		t.Fatalf("runTelemetryHunt: %v", err)
	}
	if len(findings) != 1 {
		t.Fatalf("expected 1 observation from the overridden ledger, got %d", len(findings))
	}
	if findings[0].SessionID != "ses-override" {
		t.Errorf("observation session = %q, want %q", findings[0].SessionID, "ses-override")
	}
}
