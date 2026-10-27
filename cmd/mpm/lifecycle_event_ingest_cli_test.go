// lifecycle_event_ingest_cli_test.go — the exact BYTES a caller reads
// from the mpm-event/0.1 ingest (P5).
//
// The handler-level tests (internal/core/tools/lifecycle_event_ingest_tool_test.go)
// pin the shape the handler returns. These pin the shape that actually
// reaches a caller, because the reference adapter does NOT read a
// handler's return value directly: it spawns `mpm call mpm_system
// --payload …`, takes the LAST JSON line of stdout, and hands the WHOLE
// parsed object to its transport as `res.payload`.
//
// The transport then reads `res.payload.receipt`. So the receipt must be
// a TOP-LEVEL SIBLING of "success" in the `mpm call` stdout envelope.
//
// This test therefore execs the real binary, against a real isolated
// workspace, and asserts the bytes. A change that nests the receipt under
// a "payload" key — or that puts it anywhere but the top level — fails
// here, not in production.

package main

import (
	"bytes"
	"encoding/json"
	"os"
	"os/exec"
	"strings"
	"testing"
)

// lifecycleCallMPM runs the freshly-built mpm binary against ONE
// isolated workspace and returns stdout, stderr and the exit error.
//
// The workspace is shared across the calls of a single test so that a
// second `mpm call` sees what the first one stored — which is the whole
// point of the dedup and counter assertions.
func lifecycleCallMPM(t *testing.T, home string, args ...string) (stdout, stderr string, err error) {
	t.Helper()
	if mpmBin == "" {
		t.Skip("mpm binary not built (see TestMain); skipping the CLI ingest regression")
	}
	cmd := exec.Command(mpmBin, args...)
	cmd.Env = []string{
		"PATH=" + os.Getenv("PATH"),
		"HOME=" + home,
		"MPM_SCHEDULER_DISABLED=1",
	}
	var so, se bytes.Buffer
	cmd.Stdout = &so
	cmd.Stderr = &se
	err = cmd.Run()
	return so.String(), se.String(), err
}

// lifecycleLastJSONLine returns the LAST parseable JSON object on stdout.
//
// The adapter parses the last JSON line, not the whole stream, so the
// test parses the same way.
func lifecycleLastJSONLine(t *testing.T, stdout string) map[string]interface{} {
	t.Helper()
	lines := strings.Split(strings.TrimSpace(stdout), "\n")
	for i := len(lines) - 1; i >= 0; i-- {
		line := strings.TrimSpace(lines[i])
		if !strings.HasPrefix(line, "{") || !strings.HasSuffix(line, "}") {
			continue
		}
		var obj map[string]interface{}
		if err := json.Unmarshal([]byte(line), &obj); err == nil {
			return obj
		}
	}
	t.Fatalf("no parseable JSON object on stdout:\n%s", stdout)
	return nil
}

// lifecycleCLIEnvelope is a VALID mpm-event/0.1 envelope, as JSON.
const lifecycleCLIEnvelope = `{
  "schema_version": "mpm-event/0.1",
  "event_id": "ev1_aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa",
  "idempotency": "strict",
  "event_type": "session.observed",
  "observation": "observed",
  "observed_at_ms": 1793021872123,
  "host": "opencode",
  "host_version": "2.1.4",
  "source": "opencode-plugin",
  "adapter_version": "abc1234",
  "project_id": null,
  "project_id_source": "unresolved",
  "framework_name": "opencode",
  "framework_session_id": "ses_abc",
  "operation": "absent",
  "result_status": "unknown",
  "trust": "observation",
  "meta": {"host_event_type": "session.text.delta", "host_call_id": "call_shared"},
  "payload": null
}`

// TestCallEventIngest_ReceiptIsAtTheTopLevelOfTheStdoutEnvelope is the
// contract test for the shape the reference adapter reads.
//
// The receipt must be reachable as envelope["receipt"], and the envelope
// must NOT carry a "payload" key holding it.
func TestCallEventIngest_ReceiptIsAtTheTopLevelOfTheStdoutEnvelope(t *testing.T) {
	home := t.TempDir()

	payload := `{"action":"event_ingest","params":{"schema_version":"mpm-event/0.1","event":` +
		lifecycleCLIEnvelope + `}}`

	stdout, stderr, err := lifecycleCallMPM(t, home, "call", "mpm_system", "--payload", payload)
	if err != nil {
		t.Fatalf("event_ingest failed: %v\nstdout=%s\nstderr=%s", err, stdout, stderr)
	}

	env := lifecycleLastJSONLine(t, stdout)
	if env["success"] != true {
		t.Fatalf("the envelope must carry success: true, got %v (stdout=%s)", env["success"], stdout)
	}
	if _, nested := env["payload"]; nested {
		t.Error("the envelope must NOT nest the receipt under a \"payload\" key — " +
			"the reference adapter reads envelope[\"receipt\"]")
	}
	receipt, ok := env["receipt"].(map[string]interface{})
	if !ok {
		t.Fatalf("the envelope must carry a top-level \"receipt\" object, got %v (stdout=%s)", env["receipt"], stdout)
	}
	if receipt["event_id"] != "ev1_aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa" {
		t.Errorf("the receipt's event_id = %v", receipt["event_id"])
	}
	if _, ok := receipt["ingested_at_ms"].(float64); !ok {
		t.Errorf("the receipt's ingested_at_ms must be a number, got %T", receipt["ingested_at_ms"])
	}
	if _, ok := receipt["duplicate"].(bool); !ok {
		t.Errorf("the receipt's duplicate must be a boolean, got %T", receipt["duplicate"])
	}
	if receipt["duplicate"] != false {
		t.Error("the first ingest must not report duplicate: true")
	}
}

// TestCallEventIngest_DuplicateIsReportedAcrossProcesses pins that a
// repeated strict event id is one stored row, reported as a duplicate,
// with the ORIGINAL ingestion timestamp.
//
// The two calls are separate PROCESSES against the same workspace, which
// is how the adapter actually delivers.
func TestCallEventIngest_DuplicateIsReportedAcrossProcesses(t *testing.T) {
	home := t.TempDir()
	payload := `{"action":"event_ingest","params":{"schema_version":"mpm-event/0.1","event":` +
		lifecycleCLIEnvelope + `}}`

	stdout1, _, err := lifecycleCallMPM(t, home, "call", "mpm_system", "--payload", payload)
	if err != nil {
		t.Fatalf("the first event_ingest failed: %v\nstdout=%s", err, stdout1)
	}
	first := lifecycleLastJSONLine(t, stdout1)["receipt"].(map[string]interface{})

	stdout2, _, err := lifecycleCallMPM(t, home, "call", "mpm_system", "--payload", payload)
	if err != nil {
		t.Fatalf("the second event_ingest failed: %v\nstdout=%s", err, stdout2)
	}
	second := lifecycleLastJSONLine(t, stdout2)["receipt"].(map[string]interface{})

	if second["duplicate"] != true {
		t.Error("the second ingest must report duplicate: true")
	}
	if second["ingested_at_ms"] != first["ingested_at_ms"] {
		t.Errorf("the duplicate's ingested_at_ms = %v, want the ORIGINAL %v",
			second["ingested_at_ms"], first["ingested_at_ms"])
	}
}

// TestCallEventCounters_Surface pins the counter surface's bytes.
func TestCallEventCounters_Surface(t *testing.T) {
	home := t.TempDir()

	payload := `{"action":"event_ingest","params":{"schema_version":"mpm-event/0.1","event":` +
		lifecycleCLIEnvelope + `}}`
	if stdout, _, err := lifecycleCallMPM(t, home, "call", "mpm_system", "--payload", payload); err != nil {
		t.Fatalf("event_ingest failed: %v\nstdout=%s", err, stdout)
	}

	stdout, stderr, err := lifecycleCallMPM(t, home, "call", "mpm_system",
		"--payload", `{"action":"event_counters","params":{}}`)
	if err != nil {
		t.Fatalf("event_counters failed: %v\nstdout=%s\nstderr=%s", err, stdout, stderr)
	}
	env := lifecycleLastJSONLine(t, stdout)
	if env["success"] != true {
		t.Fatalf("the envelope must carry success: true, got %v (stdout=%s)", env["success"], stdout)
	}
	counters, ok := env["counters"].(map[string]interface{})
	if !ok {
		t.Fatalf("the envelope must carry a top-level \"counters\" object, got %v (stdout=%s)", env["counters"], stdout)
	}
	if counters["stored_total"].(float64) != 1 {
		t.Errorf("stored_total = %v, want 1", counters["stored_total"])
	}
	byType, ok := counters["stored_by_event_type"].(map[string]interface{})
	if !ok {
		t.Fatalf("stored_by_event_type must be an object, got %T", counters["stored_by_event_type"])
	}
	if byType["session.observed"].(float64) != 1 {
		t.Errorf("stored_by_event_type = %v", byType)
	}
}

// TestCallEventIngest_RejectionIsAMachineReadableEnvelope pins the
// rejection path: a non-zero exit, a parseable envelope on stdout, and
// an error message whose FIRST segment is the machine-readable reason.
//
// The reference adapter maps a non-empty error to `rejected`, and reads
// the reason from that first segment.
func TestCallEventIngest_RejectionIsAMachineReadableEnvelope(t *testing.T) {
	home := t.TempDir()

	// An envelope of a version this endpoint does not know.
	bad := strings.Replace(lifecycleCLIEnvelope, `"mpm-event/0.1"`, `"mpm-event/0.9"`, 1)
	payload := `{"action":"event_ingest","params":{"schema_version":"mpm-event/0.1","event":` + bad + `}}`

	stdout, _, err := lifecycleCallMPM(t, home, "call", "mpm_system", "--payload", payload)
	if err == nil {
		t.Fatalf("expected a non-zero exit on a rejected envelope\nstdout=%s", stdout)
	}
	env := lifecycleLastJSONLine(t, stdout)
	if env["success"] != false {
		t.Errorf("the envelope must carry success: false, got %v (stdout=%s)", env["success"], stdout)
	}
	msg, _ := env["error"].(string)
	if !strings.HasPrefix(msg, "unknown_version:") {
		t.Errorf("the error message must start with %q, got %q", "unknown_version:", msg)
	}
	// The rejection message must NOT carry the envelope's content.
	if strings.Contains(msg, "session.text.delta") || strings.Contains(msg, "call_shared") {
		t.Errorf("the rejection message must not carry envelope content, got %q", msg)
	}
}
