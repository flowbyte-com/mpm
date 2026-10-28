package main

import (
	"context"
	"database/sql"
	"encoding/json"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	mpmcore "github.com/flowbyte-com/mpm-core"

	"github.com/flowbyte-com/mpm/internal/attribution"
	"github.com/flowbyte-com/mpm/internal/telemetry"
)

// The critic's telemetry hunt is a READ-ONLY DIAGNOSTIC.
//
// It used to end in `mpm call mpm_lessons --payload {...}` — a TRUSTED
// lesson, trusted on arrival, because `lessons_base` has no status
// column. That was the last path in the critic that wrote to the
// trusted surface, and the Session A adjudication of 2026-10-27 closed
// it: the observation's own evidence (token counts) lives in
// telemetry.db, which P2's citation vocabulary cannot address, and the
// observation is about an ABSENCE — a session that recorded nothing — so
// there is nothing in mpm.db to cite.
//
// It ALSO used to shell out to `mpm call mpm_provenance` for the
// artifact count. No such tool exists. The hunt therefore failed on
// every qualifying session and `mpm-critic --telemetry-hunt` exited 1.
//
// These tests seed a telemetry session that actually QUALIFIES (high
// token burn, zero artifacts) so the hunt reaches the count and the
// report. An empty store would produce no finding, and every assertion
// below would pass vacuously.
//
// THE PREVIOUS TESTS IN THIS FILE PINNED THE OPPOSITE CONTRACT — that
// the lesson call ran the resolved binary. They used a FAKE `mpm` that
// answered `mpm_provenance` with `{"count":0}`, so they could never
// notice that the tool does not exist. The contract is now the
// opposite: this path must need NO binary, must spawn NO process, and
// must leave the substrate byte-identical.

const huntSessionID = "session-hunt-1"

func i64(v int64) *int64   { return &v }
func str(v string) *string { return &v }

// seedQualifyingSession writes one invocation whose token total exceeds
// the hunt's 100k threshold, so the hunt calls countFn and, on a zero
// count, produces a finding.
func seedQualifyingSession(t *testing.T, projectRoot string) {
	t.Helper()
	// The CANONICAL path — <workspace>/src/db/telemetry.db — which is
	// where the collector writes and where the hunt now looks.
	//
	// The hunt used to look in "<projectRoot>/telemetry.db", the path
	// docs/SPEC.md §6.6 names, and telemetry.Open CREATES the store it
	// is given, so the hunt silently created an empty ledger at a path
	// nothing ever wrote to and reported zero observations forever.
	// Seeding the canonical path is what makes this test represent the
	// real deployment.
	store, err := telemetry.Open(telemetry.StorePath(projectRoot))
	if err != nil {
		t.Fatalf("open telemetry store: %v", err)
	}
	now := time.Now().Unix()
	res, err := store.InsertFrame(context.Background(), telemetry.Frame{
		SchemaVersion:    "1",
		EventType:        "invocation",
		InvocationID:     "inv-hunt-1",
		SessionID:        str(huntSessionID),
		Framework:        "claude-code",
		Provider:         "anthropic",
		Model:            "test-model",
		StartedAt:        now - 60,
		CompletedAt:      now - 30,
		Status:           "completed",
		InputTokens:      i64(120000),
		OutputTokens:     i64(10000),
		ProviderMetadata: json.RawMessage(`{}`),
	})
	if err != nil {
		store.Close()
		t.Fatalf("InsertFrame: %v", err)
	}
	if !res.Inserted {
		t.Error("InsertFrame did not insert; the hunt would find nothing to do")
	}
	store.Close()
}

// huntWorkspace builds a disposable substrate with an initialized schema
// and a qualifying telemetry session. It returns the workspace root and
// the substrate's handle.
func huntWorkspace(t *testing.T) (string, *mpmcore.DatabaseManager) {
	t.Helper()
	t.Setenv("HOME", t.TempDir())
	ws := t.TempDir()
	t.Setenv("MPM_WORKSPACE", ws)
	t.Setenv("MPM_DB_PATH", filepath.Join(ws, "src", "db", "mpm.db"))
	// The override is deliberately unset: the canonical resolution is
	// what this test exercises.
	t.Setenv("MPM_TELEMETRY_DB", "")

	dm, err := mpmcore.NewDatabaseManager(ws)
	if err != nil {
		t.Fatalf("NewDatabaseManager(%q): %v", ws, err)
	}
	t.Cleanup(func() { _ = dm.Close() })
	if err := dm.InitSchema(); err != nil {
		t.Fatalf("InitSchema: %v", err)
	}
	seedQualifyingSession(t, ws)
	return ws, dm
}

// TestTelemetryHunt_WritesNothingToTheSubstrate is the regression this
// whole change exists for.
//
// The old path ended in `mpm call mpm_lessons save`. This asserts that
// the substrate is untouched by a hunt that produced a finding: no
// trusted lesson, no theory, no artifact, no wake — and no tool
// invocation of any kind, because the path no longer spawns a process.
func TestTelemetryHunt_WritesNothingToTheSubstrate(t *testing.T) {
	ws, dm := huntWorkspace(t)

	var logs strings.Builder
	logger := slog.New(slog.NewTextHandler(&logs, &slog.HandlerOptions{Level: slog.LevelInfo}))

	// The substrate must be UNCHANGED. The counts are snapshotted
	// BEFORE the hunt rather than compared to zero, because the schema
	// bootstrap seeds baseline directives of its own — the assertion is
	// "the hunt changed nothing", not "the substrate was empty".
	before := snapshotCounts(t, dm.SQLDB())

	findings, err := runTelemetryHunt(context.Background(), logger, ws, dm.SQLDB(), 7)
	if err != nil {
		t.Fatalf("runTelemetryHunt: %v", err)
	}

	// Non-vacuity: the hunt must actually have produced an
	// observation. Without this, "nothing was written" would be
	// trivially true.
	if len(findings) != 1 {
		t.Fatalf("expected exactly 1 telemetry observation, got %d — "+
			"the assertions below would be vacuous", len(findings))
	}
	if findings[0].SessionID != huntSessionID {
		t.Errorf("observation session = %q, want %q", findings[0].SessionID, huntSessionID)
	}
	if findings[0].TotalInputTokens != 120000 || findings[0].TotalOutputTokens != 10000 {
		t.Errorf("observation tokens = %d/%d, want 120000/10000",
			findings[0].TotalInputTokens, findings[0].TotalOutputTokens)
	}

	// The TRUSTED surface must be untouched.
	after := snapshotCounts(t, dm.SQLDB())
	for _, table := range []string{"lessons_base", "memories", "tool_invocations",
		"scheduled_wakes", "epistemic_provenance", "works"} {
		if after[table] != before[table] {
			t.Errorf("%s went from %d to %d row(s); the telemetry hunt must write nothing",
				table, before[table], after[table])
		}
	}
	if after["tool_invocations"] != 0 {
		t.Errorf("tool_invocations has %d row(s); the telemetry hunt must not call mpm at all",
			after["tool_invocations"])
	}
	if after["lessons_base"] != 0 {
		t.Errorf("lessons_base has %d row(s); the telemetry hunt must not write a trusted lesson",
			after["lessons_base"])
	}

	// The observation must still be REPORTED.
	if !strings.Contains(logs.String(), huntSessionID) {
		t.Errorf("the observation was not reported:\n%s", logs.String())
	}
	if !strings.Contains(logs.String(), "read-only") {
		t.Errorf("the report does not say the observation is read-only:\n%s", logs.String())
	}
}

// snapshotCounts counts the rows of every surface the hunt could
// plausibly touch.
func snapshotCounts(t *testing.T, db *sql.DB) map[string]int {
	t.Helper()
	out := map[string]int{}
	for _, table := range []string{"lessons_base", "memories", "tool_invocations",
		"scheduled_wakes", "epistemic_provenance", "works"} {
		var n int
		if err := db.QueryRow("SELECT COUNT(*) FROM " + table).Scan(&n); err != nil {
			t.Fatalf("count %s: %v", table, err)
		}
		out[table] = n
	}
	return out
}

// TestTelemetryHunt_NeedsNoBinaryAndSpawnsNoProcess pins that the
// hunt has no subprocess at all.
//
// The old implementation resolved an `mpm` binary and shelled out
// twice. The count is now computed in-process, so there is nothing to
// resolve: with no MPM_BIN, no sibling binary and a hostile PATH, the
// hunt must still succeed — and the hostile binary must never run.
//
// The old test asserted the OPPOSITE (that the hunt fails when no
// binary is resolvable). That assertion pinned the subprocess as a
// requirement; the subprocess was the defect.
func TestTelemetryHunt_NeedsNoBinaryAndSpawnsNoProcess(t *testing.T) {
	ws, dm := huntWorkspace(t)

	hostileDir := t.TempDir()
	hostileLog := filepath.Join(hostileDir, "hostile.log")
	hp := filepath.Join(hostileDir, "mpm")
	if err := os.WriteFile(hp, []byte("#!/bin/sh\necho HOSTILE >> "+shellQuotePath(hostileLog)+"\nexit 0\n"), 0o755); err != nil {
		t.Fatalf("write hostile mpm: %v", err)
	}
	t.Setenv("PATH", hostileDir)
	t.Setenv("MPM_BIN", "")
	t.Setenv("MPM_BINARY", "")

	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	findings, err := runTelemetryHunt(context.Background(), logger, ws, dm.SQLDB(), 7)
	if err != nil {
		t.Fatalf("runTelemetryHunt: %v — the hunt must not need an mpm binary: %v", err, err)
	}
	if len(findings) != 1 {
		t.Fatalf("expected 1 observation, got %d", len(findings))
	}
	if h := readLogFile(t, hostileLog); h != "" {
		t.Errorf("a subprocess was spawned; the read-only hunt must run entirely in-process: %q", h)
	}
}

// TestSessionArtifactCount_UsesTheSubstratesOwnAttribution pins the
// count the hunt now uses.
//
// The rule is the one the missed-opportunity hunt applies: an artifact
// belongs to a session when the substrate attributed it to one of that
// session's invocations.
func TestSessionArtifactCount_UsesTheSubstratesOwnAttribution(t *testing.T) {
	_, dm := huntWorkspace(t)
	ctx := context.Background()

	if n, err := attribution.SessionArtifactCount(ctx, dm.SQLDB(), huntSessionID); err != nil || n != 0 {
		t.Fatalf("count for an unattributed session = %d (err %v), want 0", n, err)
	}

	// A session with an invocation and an attributed artifact.
	const session = "ses-with-artifact"
	invID := "inv-attributed-1"
	if _, err := dm.SQLDB().Exec(
		`INSERT INTO tool_invocations
		 (id, session_id, tool_name, action, invocation_id, actor_kind,
		  framework_name, payload_hash, result_status, started_at, mpm_session_id,
		  framework_session_id)
		 VALUES (?,?,?,?,?,?,?,?,?,?,?,?)`,
		"ti-attributed-1", "mpm-sess-1", "mpm_memory", "save", invID,
		"agent", "opencode", "sha256:x", "success", 1793000000, "mpm-sess-1", session,
	); err != nil {
		t.Fatalf("seed tool_invocations: %v", err)
	}
	if _, err := dm.SQLDB().Exec(
		`INSERT INTO artifact_provenance
		 (id, artifact_id, artifact_type, created_at, actor_kind, invocation_id, framework_name)
		 VALUES (?,?,?,?,?,?,?)`,
		"ap-1", "mem-attributed-1", "memory", 1793000000, "agent", invID, "opencode",
	); err != nil {
		t.Fatalf("seed artifact_provenance: %v", err)
	}

	if n, err := attribution.SessionArtifactCount(ctx, dm.SQLDB(), session); err != nil || n != 1 {
		t.Fatalf("count for a session with one attributed artifact = %d (err %v), want 1", n, err)
	}
	// A different session must not see it.
	if n, err := attribution.SessionArtifactCount(ctx, dm.SQLDB(), "ses-other"); err != nil || n != 0 {
		t.Fatalf("count for an unrelated session = %d (err %v), want 0", n, err)
	}
	// An empty session id is zero, not an error.
	if n, err := attribution.SessionArtifactCount(ctx, dm.SQLDB(), ""); err != nil || n != 0 {
		t.Fatalf("count for an empty session = %d (err %v), want 0", n, err)
	}
}

func shellQuotePath(s string) string {
	return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'"
}

func readLogFile(t *testing.T, path string) string {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return ""
		}
		t.Fatalf("read log: %v", err)
	}
	return string(b)
}
