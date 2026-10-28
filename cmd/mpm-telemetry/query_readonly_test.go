package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/flowbyte-com/mpm/internal/telemetry"
)

// The query surface is a READ surface, and it used to open the store
// with `telemetry.Open` — which applies the DDL, and therefore
// CREATES the database file when it is missing.
//
// So `mpm-telemetry query since 0` on a machine where the collector had
// never run CREATED an empty telemetry.db. A read that manufactures the
// thing it reads about is not a read: it cannot distinguish "the
// collector has not run" from "the collector ran and recorded nothing",
// and it leaves a file behind on every invocation.
//
// The correction: the read surfaces open the store READ-ONLY, and a
// missing store is a CLEAR ERROR that names the path and says what to
// do — not a fabricated empty result, and not a file on disk.

// queryEnv builds a disposable workspace with the canonical store
// location and no store in it.
func queryEnv(t *testing.T) (string, string) {
	t.Helper()
	ws := t.TempDir()
	storePath := telemetry.StorePath(ws)
	t.Setenv("MPM_WORKSPACE", ws)
	t.Setenv("MPM_TELEMETRY_DB", "")
	return ws, storePath
}

// TestQuery_DoesNotCreateTheStoreItReads is the regression.
//
// The old path called telemetry.Open, which applies the DDL and creates
// the file. The parent directory is created FIRST, deliberately: without
// it the write-open would fail with "unable to open database file" and
// the store would not be created either — so the assertion below would
// pass for the wrong reason, and the test would prove nothing.
func TestQuery_DoesNotCreateTheStoreItReads(t *testing.T) {
	_, storePath := queryEnv(t)
	if err := os.MkdirAll(filepath.Dir(storePath), 0o700); err != nil {
		t.Fatalf("mkdir: %v", err)
	}

	err := runQuery([]string{"since", "1"})
	if err == nil {
		t.Fatal("runQuery succeeded with no telemetry store; it must fail clearly " +
			"instead of creating the store it reads")
	}
	if !strings.Contains(err.Error(), storePath) {
		t.Errorf("error = %v, want it to name the store %q", err, storePath)
	}
	if _, statErr := os.Stat(storePath); !os.IsNotExist(statErr) {
		t.Errorf("the query created the store at %s (stat err = %v); "+
			"a read surface must not create the store it reads", storePath, statErr)
	}
}

// TestQuery_ReportsAMissingStoreClearly pins that the failure is
// legible: it names the path and says what to do.
func TestQuery_ReportsAMissingStoreClearly(t *testing.T) {
	_, storePath := queryEnv(t)

	err := runQuery([]string{"since", "1"})
	if err == nil {
		t.Fatal("runQuery succeeded with no telemetry store")
	}
	for _, want := range []string{storePath, "serve"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error = %v, want it to mention %q", err, want)
		}
	}
}

// TestQuery_ReadsAnExistingStore pins that the happy path still works —
// and that it does not MODIFY the store it reads.
func TestQuery_ReadsAnExistingStore(t *testing.T) {
	_, storePath := queryEnv(t)
	seedStore(t, storePath, "inv-1", "ses-1")

	before := storeFingerprint(t, storePath)

	out := captureQueryStdout(t, func() error {
		return runQuery([]string{"invocation", "inv-1"})
	})
	if out.err != nil {
		t.Fatalf("runQuery invocation = %v", out.err)
	}
	if !strings.Contains(out.stdout, "inv-1") {
		t.Errorf("the invocation was not read:\n%s", out.stdout)
	}

	if after := storeFingerprint(t, storePath); after != before {
		t.Errorf("the store changed while it was read: %s -> %s", before, after)
	}
}

// TestQuery_UsesTheCanonicalStoreLocation pins that the query surface
// reads where the collector writes.
func TestQuery_UsesTheCanonicalStoreLocation(t *testing.T) {
	ws, storePath := queryEnv(t)
	seedStore(t, storePath, "inv-canonical", "ses-canonical")

	// A ledger at the SPEC's path must NOT be found.
	specPath := filepath.Join(ws, "telemetry.db")
	seedStore(t, specPath, "inv-spec", "ses-spec")

	out := captureQueryStdout(t, func() error {
		return runQuery([]string{"invocation", "inv-canonical"})
	})
	if out.err != nil {
		t.Fatalf("runQuery invocation = %v; the query surface does not read the "+
			"canonical store location", out.err)
	}
	if !strings.Contains(out.stdout, "inv-canonical") {
		t.Errorf("the canonical invocation was not read:\n%s", out.stdout)
	}
}

// TestQuery_HonoursTheTelemetryDBOverride pins that the collector's
// override is honoured by the reader, so a collector running with a
// non-default store is still read.
func TestQuery_HonoursTheTelemetryDBOverride(t *testing.T) {
	ws, _ := queryEnv(t)
	override := filepath.Join(ws, "custom", "ledger.db")
	t.Setenv("MPM_TELEMETRY_DB", override)
	seedStore(t, override, "inv-override", "ses-override")

	out := captureQueryStdout(t, func() error {
		return runQuery([]string{"invocation", "inv-override"})
	})
	if out.err != nil {
		t.Fatalf("runQuery invocation = %v", out.err)
	}
	if !strings.Contains(out.stdout, "inv-override") {
		t.Errorf("the overridden invocation was not read:\n%s", out.stdout)
	}
}
