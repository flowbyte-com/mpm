package main

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"net"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	_ "github.com/mattn/go-sqlite3"

	"github.com/flowbyte-com/mpm/internal/telemetry"
	"github.com/flowbyte-com/mpm/internal/testenv"
)

// THIS FILE EXERCISES THE REAL EXECUTABLES AGAINST THE REAL REGISTRY.
//
// The tests that came before it — and the ones this work replaced —
// had two failure modes worth naming, because both of them make a
// suite look green while proving nothing:
//
//   * A FAKE `mpm`. The old observe tests injected a shell script that
//     answered `mpm_provenance` with `{"count":0}` — a tool that has
//     never existed in the registry. The suite was green because the
//     fake agreed with the code, and the command was dead in
//     production.
//
//   * AN UNREACHABLE THRESHOLD. The hermetic E2E ran observe with
//     `--threshold 999999999` "to avoid cross-DB shell-out", so the
//     path under test was never entered.
//
// So: no fakes here, and no unreachable thresholds. The binaries below
// are the REAL ones, built from this tree; the registry is the REAL
// one, with the REAL tool list; and the threshold is reachable, so the
// hunt genuinely runs.
//
// The point of the registry assertion is not decoration. The defect
// this work exists to fix was a call to a tool that is not in the
// registry. Proving the tool is still absent — with the real binary,
// against the real registry — is what keeps that from being
// reintroduced by a well-meaning "just add the tool" patch.

// ── the real binaries ────────────────────────────────────────────────────

var (
	realBinsOnce sync.Once
	realBinsDir  string
	realBinsErr  error
	realMpmPath  string
	realTelPath  string
)

// buildRealBinaries builds the real `mpm` and `mpm-telemetry` once per
// package run.
//
// They are BUILT here rather than discovered in `.build/bin`, because a
// test that skips when a binary is missing is a test that does not
// exercise anything. The FTS5 build flags are required — the substrate
// links against SQLite's bm25(), which needs libm — and they are
// inherited from the test process when the Makefile sets them; if they
// are absent they are supplied here.
func buildRealBinaries(t *testing.T) (mpmBin, telemetryBin string) {
	t.Helper()
	if _, err := exec.LookPath("go"); err != nil {
		t.Skip("go toolchain not available")
	}
	realBinsOnce.Do(func() {
		dir, err := os.MkdirTemp("", "mpm-observe-e2e-")
		if err != nil {
			realBinsErr = err
			return
		}
		realBinsDir = dir
		for _, spec := range []struct {
			pkg string
			out string
		}{
			{"./cmd/mpm", filepath.Join(dir, "mpm")},
			{"./cmd/mpm-telemetry", filepath.Join(dir, "mpm-telemetry")},
		} {
			if err := buildRealBinary(spec.pkg, spec.out); err != nil {
				realBinsErr = err
				return
			}
		}
		realMpmPath = filepath.Join(dir, "mpm")
		realTelPath = filepath.Join(dir, "mpm-telemetry")
	})
	if realBinsErr != nil {
		t.Fatalf("build the real binaries: %v", realBinsErr)
	}
	return realMpmPath, realTelPath
}

// buildRealBinary builds one binary from the repository root.
func buildRealBinary(pkg, out string) error {
	args := []string{"build", "-tags", "fts5", "-o", out, pkg}
	cmd := exec.Command("go", args...)
	// The test runs in cmd/mpm-telemetry; the module root is two levels up.
	cmd.Dir = "../.."
	cmd.Env = os.Environ()
	// The substrate's INSTEAD OF / AFTER triggers and the FTS5 shadow
	// tables assume FTS5 is compiled in, and FTS5's bm25() calls log(),
	// which lives in libm. Without -lm the link fails on a fresh
	// container. The Makefile sets both; supply them if the test was
	// started without them.
	if os.Getenv("CGO_CFLAGS") == "" {
		cmd.Env = append(cmd.Env, "CGO_CFLAGS=-DSQLITE_ENABLE_FTS5=1")
	}
	if os.Getenv("CGO_LDFLAGS") == "" {
		cmd.Env = append(cmd.Env, "CGO_LDFLAGS=-lm")
	}
	if out, err := cmd.CombinedOutput(); err != nil {
		return fmt.Errorf("go build %s: %v\n%s", pkg, err, out)
	}
	return nil
}

// ── the real workspace ───────────────────────────────────────────────────

// e2eWorkspace is a disposable, fully-initialised MPM workspace.
type e2eWorkspace struct {
	workspace   string
	mpmBin      string
	telemetryIn string
	substrateIn string
	env         []string
}

// newE2EWorkspace builds the workspace with the REAL `mpm` binary, so
// the substrate has the real schema and the real registry — not a
// hand-rolled approximation of them.
func newE2EWorkspace(t *testing.T) *e2eWorkspace {
	t.Helper()
	mpmBin, telBin := buildRealBinaries(t)
	_ = telBin

	ws := &e2eWorkspace{
		mpmBin:      mpmBin,
		telemetryIn: "",
		env:         testenv.Env(t),
	}
	ws.workspace = testenv.Workspace(t)
	ws.substrateIn = filepath.Join(ws.workspace, "src", "db", "mpm.db")
	ws.telemetryIn = filepath.Join(ws.workspace, "src", "db", "telemetry.db")

	// Initialise the substrate with the REAL binary and the REAL
	// registry. `mpm call` with a read-only action is the smallest
	// thing that creates the workspace and applies the schema.
	//
	// cmd.Env is ALWAYS set. A child that inherits the ambient
	// environment resolves the operator's $HOME/.mpm — which is how a
	// test reaches production, and how this file reached it once
	// already. internal/testenv exists for exactly this; every child
	// in this file goes through it.
	initCmd := exec.Command(ws.mpmBin, "call", "mpm_context",
		"--payload", `{"action":"query_global_rules","params":{}}`)
	initCmd.Env = ws.env
	out, err := initCmd.CombinedOutput()
	if err != nil {
		t.Fatalf("initialise the substrate with the real mpm binary: %v\n%s", err, out)
	}
	if _, statErr := os.Stat(ws.substrateIn); statErr != nil {
		t.Fatalf("the real mpm binary did not create the substrate at %s: %v",
			ws.substrateIn, statErr)
	}
	return ws
}

// seedLedger writes one qualifying invocation into the canonical
// telemetry ledger.
func (w *e2eWorkspace) seedLedger(t *testing.T, sessionID string, in, out int64) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(w.telemetryIn), 0o700); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	store, err := telemetry.Open(w.telemetryIn)
	if err != nil {
		t.Fatalf("open the ledger: %v", err)
	}
	defer store.Close()
	sid := sessionID
	now := time.Now().Unix()
	res, err := store.InsertFrame(context.Background(), telemetry.Frame{
		SchemaVersion: "1", EventType: "invocation",
		InvocationID: "inv-e2e-" + sessionID, SessionID: &sid,
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

// run runs a child with the HERMETIC environment. Every subprocess in
// this file goes through here, so none of them can inherit the
// ambient environment and resolve the operator's $HOME/.mpm.
func (w *e2eWorkspace) run(t *testing.T, bin string, args ...string) (stdout, stderr string, err error) {
	t.Helper()
	cmd := exec.Command(bin, args...)
	cmd.Env = w.env
	var outBuf, errBuf strings.Builder
	cmd.Stdout = &outBuf
	cmd.Stderr = &errBuf
	err = cmd.Run()
	return outBuf.String(), errBuf.String(), err
}

// runObserve runs the REAL mpm-telemetry binary.
func (w *e2eWorkspace) runObserve(t *testing.T, args ...string) (stdout, stderr string, err error) {
	t.Helper()
	_, telBin := buildRealBinaries(t)
	return w.run(t, telBin, append([]string{"observe"}, args...)...)
}

// substrateCounts counts the rows of every surface that could be
// written by a trusted-knowledge write.
func (w *e2eWorkspace) substrateCounts(t *testing.T) map[string]int {
	t.Helper()
	out := map[string]int{}
	for _, q := range []struct{ name, sql string }{
		{"lessons_base", "SELECT COUNT(*) FROM lessons_base"},
		{"memories", "SELECT COUNT(*) FROM memories"},
		{"theories", "SELECT COUNT(*) FROM memories WHERE collection = 'theories'"},
		{"tool_invocations", "SELECT COUNT(*) FROM tool_invocations"},
		{"epistemic_provenance", "SELECT COUNT(*) FROM epistemic_provenance"},
		{"artifact_provenance", "SELECT COUNT(*) FROM artifact_provenance"},
	} {
		cmd := exec.Command("sqlite3", w.substrateIn, q.sql)
		cmd.Env = w.env
		outb, err := cmd.Output()
		if err != nil {
			// sqlite3 may be absent; fall back to the Go driver.
			n, gerr := countViaGo(w.substrateIn, q.sql)
			if gerr != nil {
				t.Fatalf("count %s: %v (sqlite3: %v)", q.name, gerr, err)
			}
			out[q.name] = n
			continue
		}
		var n int
		if _, err := fmt.Sscanf(strings.TrimSpace(string(outb)), "%d", &n); err != nil {
			t.Fatalf("parse %s count %q: %v", q.name, outb, err)
		}
		out[q.name] = n
	}
	return out
}

// countViaGo counts rows with the Go driver, for hosts without the
// sqlite3 CLI. The handle is opened read-only: the test must not be
// able to modify the substrate it is measuring.
func countViaGo(path, query string) (int, error) {
	abs, err := filepath.Abs(path)
	if err != nil {
		return 0, err
	}
	uri := (&url.URL{
		Scheme:   "file",
		Path:     abs,
		RawQuery: "mode=ro&_busy_timeout=5000",
	}).String()
	db, err := sql.Open("sqlite3", uri)
	if err != nil {
		return 0, err
	}
	defer db.Close()
	var n int
	if err := db.QueryRow(query).Scan(&n); err != nil {
		return 0, err
	}
	return n, nil
}

// ── the tests ────────────────────────────────────────────────────────────

// TestE2E_ChildrenRunInAHermeticWorkspace is the guard.
//
// This file's first draft initialised the substrate with a child that
// INHERITED the ambient environment. The child therefore resolved the
// operator's $HOME/.mpm — and wrote four rows into the live
// installation's audit trail. internal/testenv's own doc comment names
// this hazard ("the single most common way a test reaches production"),
// and the failure mode is invisible: the test still passed, because the
// live substrate has the same schema.
//
// So the guard is explicit: the environment handed to every child MUST
// pin MPM_WORKSPACE and HOME at temporary directories, and MUST NOT be
// the operator's.
func TestE2E_ChildrenRunInAHermeticWorkspace(t *testing.T) {
	w := newE2EWorkspace(t)

	env := map[string]string{}
	for _, kv := range w.env {
		if i := strings.IndexByte(kv, '='); i > 0 {
			env[kv[:i]] = kv[i+1:]
		}
	}

	ws, ok := env["MPM_WORKSPACE"]
	if !ok || ws == "" {
		t.Fatal("the child environment does not pin MPM_WORKSPACE")
	}
	if !strings.HasPrefix(ws, os.TempDir()) && !strings.Contains(ws, "TestE2E") {
		t.Errorf("MPM_WORKSPACE = %q, which is not a temporary directory", ws)
	}
	home, ok := env["HOME"]
	if !ok || home == "" {
		t.Fatal("the child environment does not pin HOME")
	}
	if home == os.Getenv("HOME") {
		t.Errorf("HOME = %q, which is the operator's own home; a child could "+
			"resolve $HOME/.mpm and reach the live installation", home)
	}
	// And the live workspace must not be the one the children use.
	live := os.Getenv("HOME") + "/.mpm"
	if ws == live {
		t.Errorf("MPM_WORKSPACE = %q is the LIVE installation", ws)
	}
}

// TestE2E_ObserveReportsAndWritesNothing is the end-to-end proof of
// the policy: the REAL binary, the REAL substrate, the REAL registry,
// and a REACHABLE threshold — and not one row of trusted knowledge
// created.
func TestE2E_ObserveReportsAndWritesNothing(t *testing.T) {
	w := newE2EWorkspace(t)
	w.seedLedger(t, "ses-e2e", 120000, 120000)

	before := w.substrateCounts(t)

	stdout, stderr, err := w.runObserve(t, "--since", "1", "--threshold", "100000")
	if err != nil {
		t.Fatalf("the real mpm-telemetry observe failed: %v\nstdout:\n%s\nstderr:\n%s",
			err, stdout, stderr)
	}

	// The observation is reported, and labelled.
	if !strings.Contains(stdout, "ses-e2e") {
		t.Fatalf("the observation was not reported:\n%s", stdout)
	}
	for _, want := range []string{`"status": "unverified-observation"`, `"wrote": "nothing"`} {
		if !strings.Contains(stdout, want) {
			t.Errorf("the output does not contain %s:\n%s", want, stdout)
		}
	}
	for _, banned := range []string{`"action"`, `"type"`, `"fact"`} {
		if strings.Contains(stdout, banned) {
			t.Errorf("the output contains %s — it is shaped like a trusted write:\n%s",
				banned, stdout)
		}
	}

	after := w.substrateCounts(t)
	for table, n := range before {
		if after[table] != n {
			t.Errorf("%s went from %d to %d row(s); the real binary must write nothing",
				table, n, after[table])
		}
	}
	if after["lessons_base"] != 0 {
		t.Errorf("lessons_base has %d row(s); a trusted lesson was created", after["lessons_base"])
	}
	if after["theories"] != 0 {
		t.Errorf("theories has %d row(s); a pending theory was created", after["theories"])
	}
}

// TestE2E_ObserveReadsTheCanonicalStoreLocation pins the canonical
// path against the REAL collector's behaviour.
//
// The collector is started with NO MPM_TELEMETRY_DB override, exactly as
// the systemd unit configures it, and a real frame is pushed through
// its real socket. The observer — a separate real process — must then
// find and report that frame.
//
// This is the whole path, with no fakes and no seeding: the collector
// writes, the observer reads, and the frame is only counted as an
// observation if the two agree on where the ledger lives.
func TestE2E_ObserveReadsTheCanonicalStoreLocation(t *testing.T) {
	w := newE2EWorkspace(t)
	// The collector's own default: MPM_TELEMETRY_DB unset.
	env := testenv.WithExtra(t, "MPM_TELEMETRY_SOCKET="+filepath.Join(w.workspace, "runtime", "tel.sock"))
	_, telBin := buildRealBinaries(t)
	cmd := exec.Command(telBin, "serve", "--quiet")
	cmd.Env = env
	if err := cmd.Start(); err != nil {
		t.Fatalf("start the real collector: %v", err)
	}
	t.Cleanup(func() {
		_ = cmd.Process.Kill()
		_ = cmd.Wait()
	})

	// Wait for the collector's socket, then push a real frame.
	sock := filepath.Join(w.workspace, "runtime", "tel.sock")
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if _, err := os.Stat(sock); err == nil {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	pushFrame(t, sock, "inv-e2e-collector", "ses-e2e-collector", 150000, 50000)

	// The ledger the collector creates must be at the CANONICAL path.
	if _, err := os.Stat(w.telemetryIn); err != nil {
		t.Fatalf("the real collector did not create the canonical ledger at %s: %v",
			w.telemetryIn, err)
	}
	// And nothing at the SPEC's path.
	specPath := filepath.Join(w.workspace, "telemetry.db")
	if _, err := os.Stat(specPath); err == nil {
		t.Errorf("the real collector created a ledger at the spec's path %s", specPath)
	}

	// The observer — a separate real process — must find the frame the
	// collector wrote, with no seeding and no override.
	stdout, stderr, err := w.runObserve(t, "--since", "1", "--threshold", "100000")
	if err != nil {
		t.Fatalf("the real mpm-telemetry observe failed: %v\nstdout:\n%s\nstderr:\n%s",
			err, stdout, stderr)
	}
	if !strings.Contains(stdout, "ses-e2e-collector") {
		t.Errorf("the observer did not find the frame the real collector wrote:\n%s", stdout)
	}
	if !strings.Contains(stdout, `"total_tokens": 200000`) {
		t.Errorf("the observation does not carry the collector's token counts:\n%s", stdout)
	}
}

// pushFrame sends one real frame to the collector's socket.
func pushFrame(t *testing.T, socket, invocationID, sessionID string, in, out int64) {
	t.Helper()
	now := time.Now().Unix()
	frame := map[string]any{
		"schema_version":    "v1",
		"event_type":        "invocation_completed",
		"invocation_id":     invocationID,
		"session_id":        sessionID,
		"framework":         "claude-code",
		"framework_version": "1.0",
		"provider":          "anthropic",
		"model":             "claude-fable-5",
		"started_at":        now - 60,
		"completed_at":      now - 30,
		"status":            "completed",
		"stop_reason":       "end_turn",
		"input_tokens":      in,
		"output_tokens":     out,
		"duration_ms":       30000,
		"provider_metadata": map[string]any{},
	}
	body, err := json.Marshal(frame)
	if err != nil {
		t.Fatalf("marshal frame: %v", err)
	}
	conn, err := net.DialTimeout("unix", socket, 2*time.Second)
	if err != nil {
		t.Fatalf("dial the collector socket: %v", err)
	}
	defer conn.Close()
	_ = conn.SetDeadline(time.Now().Add(3 * time.Second))
	if _, err := conn.Write(append(body, '\n')); err != nil {
		t.Fatalf("write the frame: %v", err)
	}
	buf := make([]byte, 512)
	n, err := conn.Read(buf)
	if err != nil {
		t.Fatalf("read the collector's acknowledgement: %v", err)
	}
	if !strings.Contains(string(buf[:n]), "ACCEPTED") {
		t.Errorf("the collector did not accept the frame: %s", buf[:n])
	}
}

// TestE2E_TheRegistryStillHasNoProvenanceTool is the assertion that
// keeps the original defect from being reintroduced by a well-meaning
// patch.
//
// The old implementation called `mpm call mpm_provenance`. That tool is
// not in the registry, and never was — but the old tests used a FAKE
// `mpm` that answered it, so the suite was green while the command was
// dead.
//
// Here the REAL binary is asked, and the REAL registry answers.
func TestE2E_TheRegistryStillHasNoProvenanceTool(t *testing.T) {
	w := newE2EWorkspace(t)

	out, _, err := w.run(t, w.mpmBin, "call", "mpm_provenance",
		"--payload", `{"action":"count_by_session","params":{"session_id":"ses-e2e"}}`)

	if err == nil {
		t.Fatalf("the registry answered mpm_provenance: %s — the tool the old code "+
			"called must not exist", out)
	}
	if !strings.Contains(string(out), "unknown tool") {
		t.Errorf("the registry's answer does not say the tool is unknown:\n%s", out)
	}
	if !strings.Contains(string(out), "mpm_provenance") {
		t.Errorf("the registry's answer does not name the tool:\n%s", out)
	}
}

// TestE2E_ObserveAndTheRealRegistryAgreeThatNothingWasStored closes the
// loop: after a real observe run against a real substrate, the real
// registry reports no lesson and no theory.
func TestE2E_ObserveAndTheRealRegistryAgreeThatNothingWasStored(t *testing.T) {
	w := newE2EWorkspace(t)
	w.seedLedger(t, "ses-e2e", 120000, 120000)

	if _, _, err := w.runObserve(t, "--since", "1", "--threshold", "100000"); err != nil {
		t.Fatalf("the real mpm-telemetry observe failed: %v", err)
	}

	// Ask the REAL registry, through the REAL binary.
	out, _, err := w.run(t, w.mpmBin, "call", "mpm_lessons",
		"--payload", `{"action":"list","params":{"limit":50}}`)
	if err != nil {
		t.Fatalf("the real mpm binary could not list lessons: %v\n%s", err, out)
	}
	if strings.Contains(string(out), "high-token-no-artifact") {
		t.Errorf("the registry holds a lesson from the telemetry observation:\n%s", out)
	}
}
