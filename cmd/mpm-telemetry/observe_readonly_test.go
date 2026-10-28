package main

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	mpmcore "github.com/flowbyte-com/mpm-core"

	"github.com/flowbyte-com/mpm/internal/telemetry"
)

// `mpm-telemetry observe` is a READ-ONLY DIAGNOSTIC.
//
// It used to end in `mpm call mpm_lessons --payload {...}` — a TRUSTED
// lesson, trusted on arrival, because `lessons_base` has no status
// column. It ALSO used to obtain the artifact count from `mpm call
// mpm_provenance` — a tool that does not exist in the registry — so the
// hunt failed on every qualifying session and the command exited 1.
//
// These tests pin the contract that replaced it:
//
//   - the observation is REPORTED, and nothing is written;
//   - neither store is created or modified by a read-only run;
//   - the count is the substrate's own attribution, measured in-process;
//   - the default mode refuses, loudly, and writes nothing;
//   - no process is spawned, ever.
//
// Every test here uses DISPOSABLE databases. Nothing touches a live
// telemetry ledger, a live substrate, a credential, or a deployment.

// ── helpers ──────────────────────────────────────────────────────────────

// observeEnv builds a disposable workspace with the telemetry store at
// the canonical location and the substrate at the canonical location.
type observeEnv struct {
	workspace    string
	telemetryDB  string
	substrateDB  string
	telemetrySet bool
}

func newObserveEnv(t *testing.T) *observeEnv {
	t.Helper()
	ws := t.TempDir()
	telemetryDB := filepath.Join(ws, "src", "db", "telemetry.db")
	substrateDB := filepath.Join(ws, "src", "db", "mpm.db")
	t.Setenv("MPM_WORKSPACE", ws)
	// The override is deliberately UNSET: the canonical resolution is
	// what these tests exercise.
	t.Setenv("MPM_TELEMETRY_DB", "")
	t.Setenv("MPM_DB_PATH", "")
	return &observeEnv{workspace: ws, telemetryDB: telemetryDB, substrateDB: substrateDB}
}

// seedSubstrate creates a disposable substrate with the real schema.
func seedSubstrate(t *testing.T, ws string) *mpmcore.DatabaseManager {
	t.Helper()
	dm, err := mpmcore.NewDatabaseManager(ws)
	if err != nil {
		t.Fatalf("NewDatabaseManager(%q): %v", ws, err)
	}
	t.Cleanup(func() { _ = dm.Close() })
	if err := dm.InitSchema(); err != nil {
		t.Fatalf("InitSchema: %v", err)
	}
	return dm
}

// seedTelemetrySession writes one invocation into the telemetry ledger
// with the given token counts, so the hunt's threshold is met.
func seedTelemetrySession(t *testing.T, path, sessionID string, in, out int64) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatalf("mkdir telemetry dir: %v", err)
	}
	store, err := telemetry.Open(path)
	if err != nil {
		t.Fatalf("open telemetry store: %v", err)
	}
	defer store.Close()
	sid := sessionID
	now := time.Now().Unix()
	res, err := store.InsertFrame(context.Background(), telemetry.Frame{
		SchemaVersion:    "1",
		EventType:        "invocation",
		InvocationID:     "inv-" + sessionID,
		SessionID:        &sid,
		Framework:        "claude-code",
		Provider:         "anthropic",
		Model:            "test-model",
		StartedAt:        now - 60,
		CompletedAt:      now - 30,
		Status:           "completed",
		InputTokens:      &in,
		OutputTokens:     &out,
		ProviderMetadata: json.RawMessage(`{}`),
	})
	if err != nil {
		t.Fatalf("InsertFrame: %v", err)
	}
	if !res.Inserted {
		t.Fatal("InsertFrame did not insert; the hunt would find nothing to do")
	}
}

// attributeArtifactToSession seeds the substrate so that one session has
// an attributed artifact and the other does not.
func attributeArtifactToSession(t *testing.T, dm *mpmcore.DatabaseManager, session, invocation string) {
	t.Helper()
	if _, err := dm.SQLDB().Exec(
		`INSERT INTO tool_invocations
		 (id, session_id, tool_name, action, invocation_id, actor_kind,
		  framework_name, payload_hash, result_status, started_at, mpm_session_id,
		  framework_session_id)
		 VALUES (?,?,?,?,?,?,?,?,?,?,?,?)`,
		"ti-"+invocation, "mpm-sess-1", "mpm_memory", "save", invocation,
		"agent", "opencode", "sha256:x", "success", 1793000000, "mpm-sess-1", session,
	); err != nil {
		t.Fatalf("seed tool_invocations: %v", err)
	}
	if _, err := dm.SQLDB().Exec(
		`INSERT INTO artifact_provenance
		 (id, artifact_id, artifact_type, created_at, actor_kind, invocation_id, framework_name)
		 VALUES (?,?,?,?,?,?,?)`,
		"ap-"+invocation, "mem-"+invocation, "memory", 1793000000, "agent", invocation, "opencode",
	); err != nil {
		t.Fatalf("seed artifact_provenance: %v", err)
	}
}

// substrateCounts counts the rows of every surface the observe path
// could plausibly touch.
func substrateCounts(t *testing.T, db *sql.DB) map[string]int {
	t.Helper()
	out := map[string]int{}
	for _, table := range []string{"lessons_base", "memories", "tool_invocations",
		"artifact_provenance", "scheduled_wakes", "works"} {
		var n int
		if err := db.QueryRow("SELECT COUNT(*) FROM " + table).Scan(&n); err != nil {
			t.Fatalf("count %s: %v", table, err)
		}
		out[table] = n
	}
	// Pending theories are memories with collection='theories' — the
	// substrate's own vocabulary for an unproven, unresolved claim.
	// A telemetry observation must never produce one.
	var theories int
	if err := db.QueryRow(
		"SELECT COUNT(*) FROM memories WHERE collection = 'theories'").Scan(&theories); err != nil {
		t.Fatalf("count theories: %v", err)
	}
	out["theories"] = theories
	return out
}

// telemetryRowCount counts the rows in the telemetry ledger.
func telemetryRowCount(t *testing.T, path string) int {
	t.Helper()
	store, err := telemetry.OpenReadOnly(path)
	if err != nil {
		t.Fatalf("open telemetry store %s: %v", path, err)
	}
	defer store.Close()
	var n int
	if err := store.DB().QueryRow("SELECT COUNT(*) FROM telemetry_invocation").Scan(&n); err != nil {
		t.Fatalf("count telemetry rows: %v", err)
	}
	return n
}

// captureStdout runs fn with os.Stdout redirected, and returns what was
// printed.
func captureStdout(t *testing.T, fn func() error) (string, error) {
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
	return string(out), runErr
}

// TestObserve_HelpDoesNotAdvertiseTheCrossDBShellOut pins that the help
// page does not tell an operator to expect a subprocess.
//
// The old help said "--mpm  path to mpm binary for cross-DB lookups"
// and "--dry-run  print findings instead of calling mpm" — advertising
// a cross-DB shell-out that no longer exists. A help page that
// describes a mechanism the command does not use is how an operator
// ends up believing the command still writes.
func TestObserve_HelpDoesNotAdvertiseTheCrossDBShellOut(t *testing.T) {
	var buf bytes.Buffer
	writeHelpObserve(&buf, "test-build")
	out := buf.String()

	for _, banned := range []string{"cross-DB lookups", "mpm_provenance", "instead of calling mpm"} {
		if strings.Contains(out, banned) {
			t.Errorf("the observe help advertises %q:\n%s", banned, out)
		}
	}
	for _, want := range []string{"READ-ONLY", "--mpm", "--dry-run"} {
		if !strings.Contains(out, want) {
			t.Errorf("the observe help does not mention %q:\n%s", want, out)
		}
	}
}

// ── the contract ─────────────────────────────────────────────────────────

// TestObserve_DryRunReportsAndWritesNothing is the regression this whole
// change exists for.
//
// The old path ended in `mpm call mpm_lessons save`. This asserts that a
// dry run that PRODUCED a finding still leaves every trusted surface
// untouched — and that the finding is reported.
func TestObserve_DryRunReportsAndWritesNothing(t *testing.T) {
	env := newObserveEnv(t)
	dm := seedSubstrate(t, env.workspace)
	seedTelemetrySession(t, env.telemetryDB, "ses-qualify", 120000, 120000)

	before := substrateCounts(t, dm.SQLDB())
	telemetryBefore := telemetryRowCount(t, env.telemetryDB)

	stdout, err := captureStdout(t, func() error {
		return runObserve([]string{"--dry-run", "--since", "1"})
	})
	if err != nil {
		t.Fatalf("runObserve --dry-run = %v", err)
	}

	// NON-VACUITY: the hunt must actually have produced an
	// observation. Without this, "nothing was written" would be
	// trivially true.
	if !strings.Contains(stdout, "ses-qualify") {
		t.Fatalf("the observation was not reported:\n%s", stdout)
	}
	if !strings.Contains(stdout, `"status": "unverified-observation"`) {
		t.Errorf("the observation does not declare its standing:\n%s", stdout)
	}
	if strings.Contains(stdout, `"action"`) || strings.Contains(stdout, `"type"`) {
		t.Errorf("the observation is shaped like a lesson write:\n%s", stdout)
	}

	// The TRUSTED surface must be untouched.
	after := substrateCounts(t, dm.SQLDB())
	for table, n := range before {
		if after[table] != n {
			t.Errorf("%s went from %d to %d row(s); a dry run must write nothing",
				table, n, after[table])
		}
	}
	if after["lessons_base"] != 0 {
		t.Errorf("lessons_base has %d row(s); a dry run must not create a trusted lesson",
			after["lessons_base"])
	}
	if after["tool_invocations"] != 0 {
		t.Errorf("tool_invocations has %d row(s); the count must not shell out to mpm",
			after["tool_invocations"])
	}
	if got := telemetryRowCount(t, env.telemetryDB); got != telemetryBefore {
		t.Errorf("the telemetry ledger went from %d to %d row(s); a dry run must not write",
			telemetryBefore, got)
	}
}

// TestObserve_DryRunDoesNotCreateEitherStore pins that "read-only"
// means read-only at the FILESYSTEM level.
//
// The old dry run called telemetry.Open, which applies the DDL and
// therefore CREATED the telemetry database it was looking for. A dry
// run that creates a database is not a dry run.
//
// The parent directory is created FIRST, deliberately: without it the
// write-open would fail with "unable to open database file" and the
// store would not be created either — so the assertion below would
// pass for the wrong reason, and the test would prove nothing.
func TestObserve_DryRunDoesNotCreateEitherStore(t *testing.T) {
	env := newObserveEnv(t)
	if err := os.MkdirAll(filepath.Dir(env.telemetryDB), 0o700); err != nil {
		t.Fatalf("mkdir telemetry dir: %v", err)
	}
	if err := os.MkdirAll(filepath.Dir(env.substrateDB), 0o700); err != nil {
		t.Fatalf("mkdir substrate dir: %v", err)
	}

	stdout, err := captureStdout(t, func() error {
		return runObserve([]string{"--dry-run", "--since", "1"})
	})
	if err == nil {
		t.Fatal("runObserve --dry-run succeeded with no telemetry store; " +
			"it must fail clearly instead of creating the store")
	}
	if stdout != "" {
		t.Errorf("the failed dry run printed to stdout: %q", stdout)
	}
	if !strings.Contains(err.Error(), env.telemetryDB) {
		t.Errorf("error = %v, want it to name the store it could not open %q",
			err, env.telemetryDB)
	}

	if _, statErr := os.Stat(env.telemetryDB); !os.IsNotExist(statErr) {
		t.Errorf("the dry run created the telemetry store at %s (stat err = %v)",
			env.telemetryDB, statErr)
	}
	if _, statErr := os.Stat(env.substrateDB); !os.IsNotExist(statErr) {
		t.Errorf("the dry run created the substrate at %s (stat err = %v)",
			env.substrateDB, statErr)
	}
}

// TestObserve_DefaultReportsAndWritesNothing pins the default mode.
//
// The documented default used to be "write a lesson". That write was
// the trust boundary this issue is about, and the approved policy
// (2026-10-28) makes the default a READ-ONLY DIAGNOSTIC: it REPORTS
// the observation and writes nothing.
//
// This is the assertion that matters most in the whole file: a
// qualifying session, reported on stdout, with EVERY trusted surface
// untouched — no lesson, no theory, no artifact, no tool invocation.
func TestObserve_DefaultReportsAndWritesNothing(t *testing.T) {
	env := newObserveEnv(t)
	dm := seedSubstrate(t, env.workspace)
	seedTelemetrySession(t, env.telemetryDB, "ses-qualify", 120000, 120000)

	before := substrateCounts(t, dm.SQLDB())

	stdout, err := captureStdout(t, func() error {
		return runObserve([]string{"--since", "1"})
	})
	if err != nil {
		t.Fatalf("runObserve = %v; the default mode is a read-only diagnostic and must succeed", err)
	}

	// The observation must be REPORTED.
	if !strings.Contains(stdout, "ses-qualify") {
		t.Fatalf("the observation was not reported:\n%s", stdout)
	}
	// And it must be labelled as an unverified diagnostic.
	if !strings.Contains(stdout, `"status": "unverified-observation"`) {
		t.Errorf("the observation does not declare its standing:\n%s", stdout)
	}
	if !strings.Contains(stdout, `"wrote": "nothing"`) {
		t.Errorf("the observation does not say that nothing was written:\n%s", stdout)
	}
	// And it must NOT be shaped like a lesson write.
	for _, banned := range []string{`"action"`, `"type"`, `"fact"`} {
		if strings.Contains(stdout, banned) {
			t.Errorf("the observation is shaped like a lesson write (%s):\n%s", banned, stdout)
		}
	}

	after := substrateCounts(t, dm.SQLDB())
	for table, n := range before {
		if after[table] != n {
			t.Errorf("%s went from %d to %d row(s); the default mode must write nothing",
				table, n, after[table])
		}
	}
	if after["lessons_base"] != 0 {
		t.Errorf("lessons_base has %d row(s); the default mode must not create a trusted lesson",
			after["lessons_base"])
	}
	if after["theories"] != 0 {
		t.Errorf("theories has %d row(s); the default mode must not create a pending theory",
			after["theories"])
	}
}

// TestObserve_DefaultSucceedsOnAQuietStore pins that the refusal is
// about the WRITE, not about the hunt.
//
// A store with no qualifying session has nothing to write, so the
// command succeeds and stays silent — the behaviour it has always had.
func TestObserve_DefaultSucceedsOnAQuietStore(t *testing.T) {
	env := newObserveEnv(t)
	seedSubstrate(t, env.workspace)
	seedTelemetrySession(t, env.telemetryDB, "ses-quiet", 10, 10)

	stdout, err := captureStdout(t, func() error {
		return runObserve([]string{"--since", "1"})
	})
	if err != nil {
		t.Fatalf("runObserve on a quiet store = %v; there is nothing to write", err)
	}
	if stdout != "" {
		t.Errorf("a quiet store printed %q", stdout)
	}
}

// TestObserve_CountIsTheSubstratesOwnAttribution pins that the count
// is MEASURED, in-process, with the substrate's own rule.
//
// One session has an attributed artifact and must NOT be reported; the
// other has none and must be.
func TestObserve_CountIsTheSubstratesOwnAttribution(t *testing.T) {
	env := newObserveEnv(t)
	dm := seedSubstrate(t, env.workspace)

	// A session with an attributed artifact.
	seedTelemetrySession(t, env.telemetryDB, "ses-with-artifact", 120000, 120000)
	attributeArtifactToSession(t, dm, "ses-with-artifact", "inv-with-artifact")

	// A session with none.
	seedTelemetrySession(t, env.telemetryDB, "ses-without", 120000, 120000)

	stdout, err := captureStdout(t, func() error {
		return runObserve([]string{"--dry-run", "--since", "1"})
	})
	if err != nil {
		t.Fatalf("runObserve --dry-run = %v", err)
	}
	if strings.Contains(stdout, "ses-with-artifact") {
		t.Errorf("a session WITH an attributed artifact was reported:\n%s", stdout)
	}
	if !strings.Contains(stdout, "ses-without") {
		t.Errorf("a session with NO attributed artifact was not reported:\n%s", stdout)
	}
}

// TestObserve_SpawnsNoProcessAndIgnoresTheMpmFlag pins that the count
// is in-process.
//
// The old implementation resolved an `mpm` binary and shelled out
// twice. The count is now computed in-process, so there is nothing to
// resolve: with no MPM_BIN, no sibling binary and a hostile PATH, the
// hunt must still succeed — and the hostile binary must never run.
//
// The `--mpm` flag used to select that binary. It is still accepted (so
// an operator's existing invocation does not break on an unknown flag)
// but it is IGNORED — and it WARNS, loudly, because silently accepting a
// flag that does nothing is how an operator keeps believing the command
// writes.
func TestObserve_SpawnsNoProcessAndIgnoresTheMpmFlag(t *testing.T) {
	env := newObserveEnv(t)
	seedSubstrate(t, env.workspace)
	seedTelemetrySession(t, env.telemetryDB, "ses-qualify", 120000, 120000)

	hostileDir := t.TempDir()
	hostileLog := filepath.Join(hostileDir, "hostile.log")
	hostile := filepath.Join(hostileDir, "mpm")
	body := "#!/bin/sh\necho \"$0 $*\" >> " + hostileLog + "\nexit 0\n"
	if err := os.WriteFile(hostile, []byte(body), 0o755); err != nil {
		t.Fatalf("write hostile mpm: %v", err)
	}
	t.Setenv("PATH", hostileDir)
	t.Setenv("MPM_BIN", "")
	t.Setenv("MPM_BINARY", "")

	captured := captureStreams(t, func() error {
		// A --mpm path that does not exist: if it were honoured, the
		// run would fail.
		return runObserve([]string{"--dry-run", "--since", "1", "--mpm",
			filepath.Join(hostileDir, "not-installed-mpm")})
	})
	if captured.err != nil {
		t.Fatalf("runObserve --dry-run = %v; the hunt must not need an mpm binary", captured.err)
	}
	if !strings.Contains(captured.stdout, "ses-qualify") {
		t.Errorf("the observation was not reported:\n%s", captured.stdout)
	}
	if b, readErr := os.ReadFile(hostileLog); readErr == nil && len(b) > 0 {
		t.Errorf("a subprocess was spawned; the hunt must run entirely in-process: %q", b)
	}
	// The deprecated flag must WARN, not be silently ignored.
	if !strings.Contains(captured.stderr, "DEPRECATED") {
		t.Errorf("--mpm did not warn that it is deprecated:\n%s", captured.stderr)
	}
	if !strings.Contains(captured.stderr, "--mpm") {
		t.Errorf("the warning does not name the flag:\n%s", captured.stderr)
	}
}

// TestObserve_AnUninitialisedSubstrateIsAClearError pins "clear errors
// rather than silent failures".
//
// A substrate whose schema is missing is NOT the same claim as "this
// session produced nothing". The first is a misconfiguration; the
// second is a measurement.
func TestObserve_AnUninitialisedSubstrateIsAClearError(t *testing.T) {
	env := newObserveEnv(t)
	seedTelemetrySession(t, env.telemetryDB, "ses-qualify", 120000, 120000)

	// A substrate file that is not a database at all.
	if err := os.MkdirAll(filepath.Dir(env.substrateDB), 0o700); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	if err := os.WriteFile(env.substrateDB, []byte("not a database"), 0o600); err != nil {
		t.Fatalf("write substrate: %v", err)
	}

	_, err := captureStdout(t, func() error {
		return runObserve([]string{"--dry-run", "--since", "1"})
	})
	if err == nil {
		t.Fatal("runObserve succeeded against an unreadable substrate")
	}
	if !strings.Contains(err.Error(), "substrate") {
		t.Errorf("error = %v, want it to name the substrate", err)
	}
}

// TestObserve_NoSubstrateMeansZeroArtifacts covers the honest reading of
// an absent substrate: nothing could have been attributed to anything,
// so every session counts as zero-artifact. The run says so out loud.
func TestObserve_NoSubstrateMeansZeroArtifacts(t *testing.T) {
	env := newObserveEnv(t)
	seedTelemetrySession(t, env.telemetryDB, "ses-qualify", 120000, 120000)

	stdout, err := captureStdout(t, func() error {
		return runObserve([]string{"--dry-run", "--since", "1"})
	})
	if err != nil {
		t.Fatalf("runObserve --dry-run = %v", err)
	}
	if !strings.Contains(stdout, "ses-qualify") {
		t.Errorf("the observation was not reported:\n%s", stdout)
	}
	if _, statErr := os.Stat(env.substrateDB); !os.IsNotExist(statErr) {
		t.Errorf("the run created the substrate at %s (stat err = %v)",
			env.substrateDB, statErr)
	}
}
