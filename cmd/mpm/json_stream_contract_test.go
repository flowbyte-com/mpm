// json_stream_contract_test.go — --json payloads belong on stdout.
//
// WHAT THIS FILE PINS
// -------------------
// Successful `--json` invocations must emit their JSON envelope on
// STDOUT: that is what shell pipelines, `jq` and agent integrations
// read. Several write-path commands routed the envelope through
// respond()'s second argument, which renders on STDERR, leaving
// stdout empty while reporting exit 0:
//
//	$ mpm decide --choice c --rationale r --json | jq .
//	(nothing — the envelope went to stderr)
//
// The defect survived because the existing JSON tests assert envelope
// SHAPE on synthetic strings (cli_json_envelope_test.go) without ever
// asserting which STREAM carries the payload. Every test here runs the
// real binary as a subprocess and captures the two streams separately,
// so a stream inversion fails loudly.
//
// The contract, per stream:
//   - success + --json → valid, nonempty JSON on stdout; the payload
//     must NOT also appear on stderr (no duplication);
//   - failure (any exit != 0) → diagnostics on stderr, stdout empty;
//   - human-readable success (no --json) → unchanged (stderr), stdout empty.
//
// Every test runs against an isolated workspace via testenv.WithExtra:
// a pinned MPM_WORKSPACE/HOME pair, blanked provider keys and a
// disabled scheduler nudge. Nothing here can reach the operator's
// live installation (internal/testenv.TestNoUnisolatedMPMSubprocess
// enforces the shape).
package main

import (
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/flowbyte-com/mpm/internal/testenv"
)

// jsonStreamRun is one subprocess invocation with the streams kept apart.
type jsonStreamRun struct {
	stdout string
	stderr string
	code   int
}

// runJSONStream runs the built mpm binary with args in an isolated
// environment and returns stdout, stderr and the exit code separately.
//
// The binary must exist (run `make build` first); the test skips
// otherwise, following the convention of the other CLI subprocess
// suites. The database is initialised through the public boundary
// (`call mpm_system health_check`), never by touching files directly.
func runJSONStream(t *testing.T, args ...string) jsonStreamRun {
	t.Helper()
	bin := mpmBinForTest()
	if bin == "" {
		t.Skip("mpm binary not found; run `make build` first")
	}
	abs, err := filepath.Abs(bin)
	if err != nil {
		t.Fatalf("resolve test binary: %v", err)
	}
	env := testenv.WithExtra(t)
	init := exec.Command(abs, "call", "mpm_system", "--payload", `{"action":"health_check"}`)
	init.Env = env
	if out, err := init.CombinedOutput(); err != nil {
		t.Fatalf("initialise test database: %v\n%s", err, out)
	}
	cmd := exec.Command(abs, args...)
	cmd.Env = env
	var stdout, stderr strings.Builder
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	err = cmd.Run()
	code := 0
	if exitErr, ok := err.(*exec.ExitError); ok {
		code = exitErr.ExitCode()
	} else if err != nil {
		t.Fatalf("run mpm %v: %v\nstdout=%s\nstderr=%s", args, err, stdout.String(), stderr.String())
	}
	return jsonStreamRun{stdout: stdout.String(), stderr: stderr.String(), code: code}
}

// requireStdoutJSON asserts stdout holds one valid, nonempty JSON
// document and returns it decoded. A stream inversion (payload on
// stderr, stdout empty) fails here with the stderr content shown,
// so the failure points at the stream, not the shape.
func requireStdoutJSON(t *testing.T, run jsonStreamRun, args ...string) map[string]interface{} {
	t.Helper()
	trimmed := strings.TrimSpace(run.stdout)
	if trimmed == "" {
		t.Fatalf("`mpm %s` exited %d with EMPTY stdout; stderr was:\n%s",
			strings.Join(args, " "), run.code, run.stderr)
	}
	var doc map[string]interface{}
	if err := json.Unmarshal([]byte(trimmed), &doc); err != nil {
		t.Fatalf("`mpm %s` stdout is not valid JSON: %v\nstdout=%q\nstderr=%q",
			strings.Join(args, " "), err, run.stdout, run.stderr)
	}
	return doc
}

// requireNoStderrDuplication asserts the machine payload is not ALSO
// on stderr. Diagnostics may mention the command, but the envelope
// itself must appear exactly once, on stdout.
func requireNoStderrDuplication(t *testing.T, run jsonStreamRun, args ...string) {
	t.Helper()
	if strings.Contains(run.stderr, `"success":true`) {
		t.Errorf("`mpm %s` duplicated its success envelope on stderr:\n%s",
			strings.Join(args, " "), run.stderr)
	}
}

// requireSuccessEnvelope checks the shared shape: success:true plus a
// string id the operator can follow up on.
func requireSuccessEnvelope(t *testing.T, doc map[string]interface{}, args ...string) {
	t.Helper()
	if doc["success"] != true {
		t.Errorf("`mpm %s` envelope success=%v, want true: %v",
			strings.Join(args, " "), doc["success"], doc)
	}
	id, ok := doc["id"].(string)
	if !ok || id == "" {
		t.Errorf("`mpm %s` success envelope carries no string id: %v",
			strings.Join(args, " "), doc)
	}
}

func TestJSONStream_DecideRecordEnvelopeOnStdout(t *testing.T) {
	args := []string{"decide", "--choice", "go left", "--rationale", "trail markers", "--json"}
	run := runJSONStream(t, args...)
	if run.code != 0 {
		t.Fatalf("exit=%d, want 0\nstderr=%s", run.code, run.stderr)
	}
	doc := requireStdoutJSON(t, run, args...)
	requireSuccessEnvelope(t, doc, args...)
	requireNoStderrDuplication(t, run, args...)
}

func TestJSONStream_TheorizeEnvelopeOnStdout(t *testing.T) {
	args := []string{"theorize", "HYPOTHESIS: standing water breeds stillness", "--json"}
	run := runJSONStream(t, args...)
	if run.code != 0 {
		t.Fatalf("exit=%d, want 0\nstderr=%s", run.code, run.stderr)
	}
	doc := requireStdoutJSON(t, run, args...)
	requireSuccessEnvelope(t, doc, args...)
	if doc["status"] != "pending" {
		t.Errorf("theorize envelope status=%v, want pending: %v", doc["status"], doc)
	}
	requireNoStderrDuplication(t, run, args...)
}

func TestJSONStream_ProposeTheoryEnvelopeOnStdout(t *testing.T) {
	args := []string{"propose_theory", "HYPOTHESIS: slow rivers run clear", "--json"}
	run := runJSONStream(t, args...)
	if run.code != 0 {
		t.Fatalf("exit=%d, want 0\nstderr=%s", run.code, run.stderr)
	}
	doc := requireStdoutJSON(t, run, args...)
	requireSuccessEnvelope(t, doc, args...)
	requireNoStderrDuplication(t, run, args...)
}

func TestJSONStream_RecordDecisionEnvelopeOnStdout(t *testing.T) {
	args := []string{"record_decision", "--choice", "go right", "--rationale", "river crossing", "--json"}
	run := runJSONStream(t, args...)
	if run.code != 0 {
		t.Fatalf("exit=%d, want 0\nstderr=%s", run.code, run.stderr)
	}
	doc := requireStdoutJSON(t, run, args...)
	requireSuccessEnvelope(t, doc, args...)
	if doc["action"] != "record" {
		t.Errorf("record_decision envelope action=%v, want record: %v", doc["action"], doc)
	}
	requireNoStderrDuplication(t, run, args...)
}

func TestJSONStream_DecisionSupersedeInvalidateOnStdout(t *testing.T) {
	seed := runJSONStream(t, "decide", "--choice", "ford the river", "--rationale", "bridge out", "--json")
	if seed.code != 0 {
		t.Fatalf("seed decision: exit=%d\nstderr=%s", seed.code, seed.stderr)
	}
	seedDoc := requireStdoutJSON(t, seed, "decide", "--json")
	oldID, _ := seedDoc["id"].(string)
	if oldID == "" {
		t.Fatalf("seed decision carried no id: %v", seedDoc)
	}

	supArgs := []string{"decision", "supersede", oldID, "--choice", "take the ferry", "--rationale", "safer", "--json"}
	sup := runJSONStream(t, supArgs...)
	if sup.code != 0 {
		t.Fatalf("exit=%d, want 0\nstderr=%s", sup.code, sup.stderr)
	}
	supDoc := requireStdoutJSON(t, sup, supArgs...)
	requireSuccessEnvelope(t, supDoc, supArgs...)
	if supDoc["action"] != "supersede" {
		t.Errorf("supersede envelope action=%v, want supersede: %v", supDoc["action"], supDoc)
	}
	requireNoStderrDuplication(t, sup, supArgs...)

	newID, _ := supDoc["id"].(string)
	invArgs := []string{"decision", "invalidate", newID, "--json"}
	inv := runJSONStream(t, invArgs...)
	if inv.code != 0 {
		t.Fatalf("exit=%d, want 0\nstderr=%s", inv.code, inv.stderr)
	}
	invDoc := requireStdoutJSON(t, inv, invArgs...)
	requireSuccessEnvelope(t, invDoc, invArgs...)
	if invDoc["action"] != "invalidate" {
		t.Errorf("invalidate envelope action=%v, want invalidate: %v", invDoc["action"], invDoc)
	}
	requireNoStderrDuplication(t, inv, invArgs...)
}

func TestJSONStream_HintEmptyArrayOnStdout(t *testing.T) {
	// The empty result is the case most likely to regress silently:
	// `[]` on stderr with exit 0 looks like success to a caller that
	// never reads stderr, while a stdout reader sees EOF.
	args := []string{"hint", "qqq-no-conceivable-match-zzz", "--json"}
	run := runJSONStream(t, args...)
	if run.code != 0 {
		t.Fatalf("exit=%d, want 0\nstderr=%s", run.code, run.stderr)
	}
	trimmed := strings.TrimSpace(run.stdout)
	if trimmed == "" {
		t.Fatalf("empty hint printed NOTHING on stdout; stderr was:\n%s", run.stderr)
	}
	var arr []interface{}
	if err := json.Unmarshal([]byte(trimmed), &arr); err != nil {
		t.Fatalf("hint stdout is not a JSON array: %v\nstdout=%q\nstderr=%q", err, run.stdout, run.stderr)
	}
	if len(arr) != 0 {
		t.Errorf("expected an empty hint array, got %d element(s): %v", len(arr), arr)
	}
	if strings.Contains(run.stderr, "[") {
		t.Errorf("hint duplicated its payload on stderr:\n%s", run.stderr)
	}
}

func TestJSONStream_HintNonEmptyArrayOnStdout(t *testing.T) {
	seed := runJSONStream(t, "theorize", "HYPOTHESIS: river valley observatory monitors water flow", "--json")
	if seed.code != 0 {
		t.Fatalf("seed theory: exit=%d\nstderr=%s", seed.code, seed.stderr)
	}
	args := []string{"hint", "river valley observatory water", "--json"}
	run := runJSONStream(t, args...)
	if run.code != 0 {
		t.Fatalf("exit=%d, want 0\nstderr=%s", run.code, run.stderr)
	}
	var arr []interface{}
	if err := json.Unmarshal([]byte(strings.TrimSpace(run.stdout)), &arr); err != nil {
		t.Fatalf("hint stdout is not a JSON array: %v\nstdout=%q\nstderr=%q", err, run.stdout, run.stderr)
	}
	if len(arr) == 0 {
		t.Fatalf("expected at least one hint for seeded keywords, got none\nstderr=%s", run.stderr)
	}
	if strings.Contains(run.stderr, `"collection"`) {
		t.Errorf("hint duplicated its payload on stderr:\n%s", run.stderr)
	}
}

func TestJSONStream_FailureEnvelopeStaysOnStderr(t *testing.T) {
	// The failure path is already correct: a JSON error on stderr
	// with a nonzero exit. This pins it so the stdout migration of
	// the success paths cannot drag the failure paths along — a
	// caller distinguishing "no output, nonzero exit" from success
	// depends on stdout staying empty here.
	args := []string{"decide", "--json"}
	run := runJSONStream(t, args...)
	if run.code == 0 {
		t.Fatalf("exit=0, want nonzero for a missing --choice\nstdout=%s", run.stdout)
	}
	if strings.TrimSpace(run.stdout) != "" {
		t.Errorf("failure must leave stdout empty, got:\n%s", run.stdout)
	}
	var doc map[string]interface{}
	if err := json.Unmarshal([]byte(strings.TrimSpace(run.stderr)), &doc); err != nil {
		t.Fatalf("failure stderr is not a JSON envelope: %v\nstderr=%q", err, run.stderr)
	}
	if doc["success"] != false {
		t.Errorf("failure envelope success=%v, want false: %v", doc["success"], doc)
	}
}

func TestJSONStream_HumanOutputUnchanged(t *testing.T) {
	// Without --json nothing moves: the human-readable confirmation
	// stays on stderr and stdout stays empty. The fix is about the
	// machine payload only, not a rebalancing of the human surface.
	args := []string{"decide", "--choice", "wade across", "--rationale", "shallow here"}
	run := runJSONStream(t, args...)
	if run.code != 0 {
		t.Fatalf("exit=%d, want 0\nstderr=%s", run.code, run.stderr)
	}
	if strings.TrimSpace(run.stdout) != "" {
		t.Errorf("human-readable success must leave stdout empty, got:\n%s", run.stdout)
	}
	if !strings.Contains(run.stderr, "Decision recorded:") {
		t.Errorf("human confirmation missing from stderr:\n%s", run.stderr)
	}
}

// TestJSONStream_JQRoundTrip is the end-to-end form of the original
// complaint: a pipeline that parses stdout must observe the id.
func TestJSONStream_JQRoundTrip(t *testing.T) {
	if _, err := exec.LookPath("jq"); err != nil {
		t.Skip("jq not installed")
	}
	bin := mpmBinForTest()
	if bin == "" {
		t.Skip("mpm binary not found; run `make build` first")
	}
	abs, err := filepath.Abs(bin)
	if err != nil {
		t.Fatalf("resolve test binary: %v", err)
	}
	env := testenv.WithExtra(t)
	init := exec.Command(abs, "call", "mpm_system", "--payload", `{"action":"health_check"}`)
	init.Env = env
	if out, err := init.CombinedOutput(); err != nil {
		t.Fatalf("initialise test database: %v\n%s", err, out)
	}
	mpm := exec.Command(abs, "decide", "--choice", "follow the riverbank", "--rationale", "firm ground", "--json")
	mpm.Env = env
	mpmOut, err := mpm.StdoutPipe()
	if err != nil {
		t.Fatalf("stdout pipe: %v", err)
	}
	mpm.Stderr = os.Stderr
	jq := exec.Command("jq", "-r", ".id")
	jq.Stdin = mpmOut
	// Start the producer first, then run the consumer to completion:
	// jq reads until EOF, which arrives when mpm exits. No goroutine
	// is involved — both processes run concurrently under the OS, and
	// the test process blocks in jq.Output, then reaps mpm. (The
	// reverse order — jq.Output before mpm.Start — deadlocks, because
	// nothing is producing the EOF jq waits for.)
	if err := mpm.Start(); err != nil {
		t.Fatalf("start mpm: %v", err)
	}
	jqOut, err := jq.Output()
	if err != nil {
		_ = mpm.Wait()
		t.Fatalf("jq over mpm stdout failed: %v", err)
	}
	if err := mpm.Wait(); err != nil {
		if exitErr, ok := err.(*exec.ExitError); !ok || exitErr.ExitCode() != 0 {
			t.Fatalf("mpm exited nonzero: %v", err)
		}
	}
	id := strings.TrimSpace(string(jqOut))
	if id == "" || id == "null" {
		t.Fatalf("jq -r .id over mpm stdout produced %q; the envelope is not on stdout", string(jqOut))
	}
}
