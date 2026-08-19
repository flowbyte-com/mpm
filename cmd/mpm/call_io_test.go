package main

import (
	"bytes"
	"encoding/json"
	"io"
	"os"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

// TestWriteEnvelope_RoutesToGivenWriter pins the stdout/stderr contract at
// the helper level: writeEnvelope must always write JSON to the writer it
// is given. The handleCall dispatcher passes os.Stdout for both success
// and error paths, so the contract test is simple — the helper must not
// reach for os.Stderr (or any global) on its own.
//
// This is the regression test for lesson 2b22765cd1b13a81 (theory
// d8c64fe9b7528d53): the previous error path used fmt.Fprintf(os.Stderr, ...)
// which routed the JSON envelope to stderr, breaking every agent adapter
// that follows the documented "JSON envelope on stdout, zap logs on stderr"
// contract.
func TestWriteEnvelope_RoutesToGivenWriter(t *testing.T) {
	tests := []struct {
		name    string
		payload interface{}
		assert  func(t *testing.T, line []byte)
	}{
		{
			name:    "success-shaped map",
			payload: map[string]interface{}{"success": true, "value": 42},
			assert: func(t *testing.T, line []byte) {
				var got map[string]interface{}
				decodeEnvelope(t, line, &got)
				if got["success"] != true {
					t.Fatalf("success field: got %v, want true", got["success"])
				}
				if got["value"] != float64(42) { // JSON numbers decode as float64
					t.Fatalf("value field: got %v, want 42", got["value"])
				}
			},
		},
		{
			name:    "error-shaped map",
			payload: map[string]interface{}{"success": false, "error": "fact is required"},
			assert: func(t *testing.T, line []byte) {
				var got map[string]interface{}
				decodeEnvelope(t, line, &got)
				if got["success"] != false {
					t.Fatalf("success field: got %v, want false", got["success"])
				}
				if got["error"] != "fact is required" {
					t.Fatalf("error field: got %v, want %q", got["error"], "fact is required")
				}
			},
		},
		{
			name:    "defensive fallback on marshal failure",
			payload: make(chan int), // json.Marshal cannot marshal channels
			assert: func(t *testing.T, line []byte) {
				var got map[string]interface{}
				decodeEnvelope(t, line, &got)
				if got["success"] != false {
					t.Fatalf("success field: got %v, want false", got["success"])
				}
				if got["error"] != "envelope marshal failed" {
					t.Fatalf("error field: got %v, want %q", got["error"], "envelope marshal failed")
				}
			},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var buf bytes.Buffer
			writeEnvelope(&buf, tt.payload)
			line := bytes.TrimRight(buf.Bytes(), "\n")
			tt.assert(t, line)
		})
	}
}

// TestCallErrorEnvelope_RoutesToStdout exercises the full handleCall
// dispatcher against a deliberately invalid payload. The envelope must
// arrive on stdout (the documented contract), NOT on stderr. With the
// pre-fix code at cmd/mpm/call.go:100, the JSON envelope was written to
// stderr — this test would have failed because stdout would be empty.
//
// The dispatcher opens the workspace DB via openCallDM() (no temp DB
// override is needed; the empty-payload error path doesn't touch any
// table beyond the audit insert, which is a benign log row).
func TestCallErrorEnvelope_RoutesToStdout(t *testing.T) {
	// Capture stdout so we can assert the JSON envelope appears here.
	// We restore on cleanup so other tests in the package aren't
	// affected if they happen to run after this one.
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

	// Trigger a handler-level error: mpm_memory.save requires params.fact;
	// passing an empty params object forces the handler to return an
	// error which the dispatcher must wrap as a JSON envelope.
	exit := handleCall([]string{
		"mpm_memory",
		"--payload", `{"action":"save","params":{}}`,
	})

	// Close the writer so the read below sees EOF.
	if err := w.Close(); err != nil {
		t.Fatalf("close pipe writer: %v", err)
	}

	var buf bytes.Buffer
	if _, err := buf.ReadFrom(r); err != nil {
		t.Fatalf("read captured stdout: %v", err)
	}
	stdoutCaptured := buf.String()

	// Exit code contract: errors must exit non-zero so agent harnesses
	// can branch on the shell-level result.
	if exit == 0 {
		t.Errorf("handleCall with bad payload: exit = 0, want non-zero")
	}

	// The JSON envelope MUST be on stdout (the documented contract).
	// We allow trailing whitespace but require the envelope to be the
	// final non-empty line so a regression where the JSON slips to
	// stderr would surface as "stdout empty".
	lines := strings.Split(strings.TrimSpace(stdoutCaptured), "\n")
	if len(lines) == 0 || strings.TrimSpace(lines[len(lines)-1]) == "" {
		t.Fatalf("handleCall produced no stdout content; the error envelope should be on stdout per the documented contract.\ncaptured stdout: %q", stdoutCaptured)
	}
	envelopeLine := strings.TrimSpace(lines[len(lines)-1])

	var envelope map[string]interface{}
	if err := json.Unmarshal([]byte(envelopeLine), &envelope); err != nil {
		t.Fatalf("stdout envelope is not valid JSON: %v\nline: %s", err, envelopeLine)
	}
	if envelope["success"] != false {
		t.Errorf("envelope.success: got %v, want false (handleCall error path must surface success:false)", envelope["success"])
	}
	if errMsg, ok := envelope["error"].(string); !ok || !strings.Contains(errMsg, "fact") {
		t.Errorf("envelope.error: got %v, want a string mentioning the missing field", envelope["error"])
	}
}

// TestCallSuccessEnvelope_RoutesToStdout mirrors the error-path test for
// the success path. Both envelopes must be on stdout so the contract is
// uniform — a future refactor that routes one path to stdout and the
// other to stderr would regress this assertion.
func TestCallSuccessEnvelope_RoutesToStdout(t *testing.T) {
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

	exit := handleCall([]string{
		"mpm_system",
		"--payload", `{"action":"health_check","params":{}}`,
	})

	if err := w.Close(); err != nil {
		t.Fatalf("close pipe writer: %v", err)
	}
	var buf bytes.Buffer
	if _, err := buf.ReadFrom(r); err != nil {
		t.Fatalf("read captured stdout: %v", err)
	}
	stdoutCaptured := buf.String()

	if exit != 0 {
		t.Errorf("handleCall health_check: exit = %d, want 0", exit)
	}
	lines := strings.Split(strings.TrimSpace(stdoutCaptured), "\n")
	envelopeLine := strings.TrimSpace(lines[len(lines)-1])

	var envelope map[string]interface{}
	if err := json.Unmarshal([]byte(envelopeLine), &envelope); err != nil {
		t.Fatalf("success stdout envelope is not valid JSON: %v\nline: %s", err, envelopeLine)
	}
	// health_check returns ok:true — the success-path signal that the
	// dispatcher reached the success branch of the if/else.
	if envelope["ok"] != true {
		t.Errorf("envelope.ok: got %v, want true (handleCall success path must surface ok:true)", envelope["ok"])
	}
}

// decodeEnvelope is a tiny shim around json.Unmarshal that fails the test
// cleanly if the line is empty or not valid JSON. Used by the table
// above; named to avoid shadowing the testify "require" package that
// other tests in this package import.
func decodeEnvelope(t *testing.T, line []byte, dst interface{}) {
	t.Helper()
	if len(line) == 0 {
		t.Fatalf("envelope line is empty")
	}
	if err := json.Unmarshal(line, dst); err != nil {
		t.Fatalf("unmarshal envelope: %v\nline: %s", err, string(line))
	}
}

// (compile-time guard: io is imported to keep the file compiling under
// go vet, which would otherwise flag unused-import errors if someone
// removed the bytes.Buffer usage above.)
var _ = io.Discard
var _ = require.NotNil // explicit reference to the testify import so future edits don't accidentally drop it
