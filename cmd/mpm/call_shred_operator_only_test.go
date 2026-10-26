// call_shred_operator_only_test.go — pin the irreversible-deletion
// boundary at the universal machine interface.
//
// The regression test in internal/core/tools proves the registry
// handler refuses a shred. That is necessary but not sufficient: an
// agent does not call the handler directly, it reaches the substrate
// through `mpm call` (and through MCP, which shares the same handler).
// This test drives the real `mpm call` entry point, including the
// documented self-authorization shapes an agent might try
// (`confirm=true`, `force=true`), and proves that the memory/lesson
// rows survive.
package main

import (
	"bytes"
	"encoding/json"
	"os"
	"strings"
	"testing"

	mpminternal "github.com/flowbyte-com/mpm-core"
)

// callMPMCapture runs handleCall with stdout captured and returns the
// exit code plus the decoded JSON envelope.
func callMPMCapture(t *testing.T, args ...string) (int, map[string]interface{}) {
	t.Helper()

	origStdout := os.Stdout
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatalf("os.Pipe: %v", err)
	}
	os.Stdout = w
	exit := handleCall(args)
	if err := w.Close(); err != nil {
		os.Stdout = origStdout
		t.Fatalf("close pipe writer: %v", err)
	}
	os.Stdout = origStdout
	defer r.Close()

	var buf bytes.Buffer
	if _, err := buf.ReadFrom(r); err != nil {
		t.Fatalf("read captured stdout: %v", err)
	}

	var envelope map[string]interface{}
	if err := json.Unmarshal(bytes.TrimSpace(buf.Bytes()), &envelope); err != nil {
		t.Fatalf("decode envelope %q: %v", buf.String(), err)
	}
	return exit, envelope
}

// TestHandleCall_ShredRefusedAndRowsSurvive is the end-to-end RED/GREEN
// anchor for the operator-only shred boundary.
func TestHandleCall_ShredRefusedAndRowsSurvive(t *testing.T) {
	// workspace() pins MPM_WORKSPACE to a fresh temp dir AND re-arms the
	// process-wide DatabaseManager singleton, so this test cannot read or
	// write the workspace another test pinned.
	ws := workspace(t)

	// Seed a memory through the same interface an agent uses.
	exit, saved := callMPMCapture(t, "mpm_memory", "--payload",
		`{"action":"save","params":{"fact":"memory that must survive an agent shred via mpm call"}}`)
	if exit != 0 {
		t.Fatalf("seed memory via mpm call: exit %d, envelope %v", exit, saved)
	}
	memID, _ := saved["id"].(string)
	if memID == "" {
		t.Fatalf("seed memory returned no id: %v", saved)
	}

	// Every self-authorization shape must be refused, and the exit
	// contract must report a Go-level error so shell-callers cannot
	// mistake the refusal for success.
	for _, extra := range []string{``, `,"confirm":true`, `,"force":true`, `,"confirm":true,"force":true`} {
		payload := `{"action":"shred","params":{"memory_id":"` + memID + `"` + extra + `}}`
		exit, envelope := callMPMCapture(t, "mpm_memory", "--payload", payload)
		if exit != 1 {
			t.Errorf("shred via mpm call (extra=%q): exit = %d, want 1", extra, exit)
		}
		if success, _ := envelope["success"].(bool); success {
			t.Errorf("shred via mpm call (extra=%q) reported success: %v", extra, envelope)
		}
		msg, _ := envelope["error"].(string)
		if !strings.Contains(msg, "operator-only") {
			t.Errorf("shred via mpm call (extra=%q): error must name the operator-only boundary, got: %q", extra, msg)
		}
	}

	// Seed and attempt to shred a lesson the same way.
	exit, savedLesson := callMPMCapture(t, "mpm_lessons", "--payload",
		`{"action":"save","params":{"fact":"lesson that must survive an agent shred via mpm call","type":"insight"}}`)
	if exit != 0 {
		t.Fatalf("seed lesson via mpm call: exit %d, envelope %v", exit, savedLesson)
	}
	lessonID, _ := savedLesson["id"].(string)
	if lessonID == "" {
		t.Fatalf("seed lesson returned no id: %v", savedLesson)
	}
	exit, envelope := callMPMCapture(t, "mpm_lessons", "--payload",
		`{"action":"shred","params":{"id":"`+lessonID+`","confirm":true}}`)
	if exit != 1 {
		t.Errorf("lesson shred via mpm call: exit = %d, want 1", exit)
	}
	if msg, _ := envelope["error"].(string); !strings.Contains(msg, "operator-only") {
		t.Errorf("lesson shred via mpm call: error must name the operator-only boundary, got: %q", msg)
	}

	// The data must still be there. Reopen the workspace DB the same
	// way an external verifier would.
	dm, err := mpminternal.NewDatabaseManager(ws)
	if err != nil {
		t.Fatalf("NewDatabaseManager: %v", err)
	}
	defer dm.Close()

	if _, err := dm.GetMemory(memID); err != nil {
		t.Errorf("memory %s did not survive the refused shred: %v", memID, err)
	}
	if _, err := dm.GetLesson(lessonID); err != nil {
		t.Errorf("lesson %s did not survive the refused shred: %v", lessonID, err)
	}

	// Reversible delete/restore must remain reachable through the same
	// interface: the fix removes permanence, not lifecycle management.
	if exit, envelope := callMPMCapture(t, "mpm_memory", "--payload",
		`{"action":"delete","params":{"memory_id":"`+memID+`"}}`); exit != 0 {
		t.Errorf("reversible memory delete via mpm call failed: exit %d, %v", exit, envelope)
	}
	if exit, envelope := callMPMCapture(t, "mpm_memory", "--payload",
		`{"action":"restore","params":{"memory_id":"`+memID+`"}}`); exit != 0 {
		t.Errorf("memory restore via mpm call failed: exit %d, %v", exit, envelope)
	}
}

// TestOperatorShredCLI_StillHardDeletes proves the boundary is a
// surface restriction, not a removed capability: the operator verb
// still performs the irreversible cascade delete.
func TestOperatorShredCLI_StillHardDeletes(t *testing.T) {
	ws := workspace(t)

	exit, saved := callMPMCapture(t, "mpm_memory", "--payload",
		`{"action":"save","params":{"fact":"memory the operator will shred"}}`)
	if exit != 0 {
		t.Fatalf("seed memory: exit %d, envelope %v", exit, saved)
	}
	memID, _ := saved["id"].(string)
	if memID == "" {
		t.Fatalf("seed memory returned no id: %v", saved)
	}

	code, out := runCLI(t, "memory", "shred", memID)
	if code != 0 {
		t.Fatalf("operator CLI shred failed: exit %d, output %q", code, out)
	}
	if !strings.Contains(out, "Memory shredded") {
		t.Errorf("operator CLI shred did not report success: %q", out)
	}

	dm, err := mpminternal.NewDatabaseManager(ws)
	if err != nil {
		t.Fatalf("NewDatabaseManager: %v", err)
	}
	defer dm.Close()

	var n int
	if err := dm.SQLDB().QueryRow(
		`SELECT COUNT(*) FROM memories WHERE id = ?`, memID,
	).Scan(&n); err != nil {
		t.Fatalf("memories probe: %v", err)
	}
	if n != 0 {
		t.Errorf("operator CLI shred left %d row(s) for %s", n, memID)
	}
}
