// drill_claude_code_harness_test.go — pin the Claude Code harness
// contract without spawning the agent.
//
// Behavioral role: this harness is the FIRST real-framework adapter.
// Its job is to drive `claude -p <prompt>` against mpm-mcp and then
// reconstruct the tool-call evidence from tool_invocations. The
// components tested here — isAvailable, writeMcpConfig,
// readInvocationsForSession — are the pure units that don't depend
// on `claude` running. The full end-to-end (spawn, agent, score)
// lives in TestDrillE2E_ClaudeCode (Task 16), gated on `claude` and
// `mpm-mcp` both being on PATH.

package internal

import (
	"context"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
)

func TestClaudeCodeHarness_IsAvailable_MissingBinary(t *testing.T) {
	h := NewClaudeCodeHarness(t.TempDir(), "/nonexistent/claude", "/nonexistent/mpm-mcp")
	err := h.isAvailable()
	if err == nil {
		t.Fatal("isAvailable must report missing claude binary")
	}
	if !strings.Contains(err.Error(), "claude") {
		t.Errorf("error should mention claude; got: %v", err)
	}
}

func TestClaudeCodeHarness_IsAvailable_MissingMCP(t *testing.T) {
	// Provide a path that resolves to a real binary for claude but
	// is intentionally broken for mpm-mcp. Use /bin/echo or
	// /usr/bin/true for the claude shim since both exist on every
	// Linux box and only need to be executable LookPath-able.
	shim := "/bin/true"
	if _, err := os.Stat(shim); err != nil {
		t.Skipf("no /bin/true shim available: %v", err)
	}
	h := NewClaudeCodeHarness(t.TempDir(), shim, "/nonexistent/mpm-mcp")
	err := h.isAvailable()
	if err == nil {
		t.Fatal("isAvailable must report missing mpm-mcp")
	}
	if !strings.Contains(err.Error(), "mpm-mcp") {
		t.Errorf("error should mention mpm-mcp; got: %v", err)
	}
}

func TestClaudeCodeHarness_WriteMcpConfig(t *testing.T) {
	dir := t.TempDir()
	h := NewClaudeCodeHarness(dir, "/bin/true", "/bin/echo")
	h.sessionID = "test-session-xyz"

	cfgPath, err := h.writeMcpConfig(DrillSpec{ID: "demo"})
	if err != nil {
		t.Fatalf("writeMcpConfig: %v", err)
	}

	// File exists in the drill subdir and contains the server spec.
	want := filepath.Join(dir, ".drill", "test-session-xyz", "mcp.json")
	if cfgPath != want {
		t.Errorf("cfg path = %q, want %q", cfgPath, want)
	}
	body, err := os.ReadFile(cfgPath)
	if err != nil {
		t.Fatalf("read cfg: %v", err)
	}
	if !strings.Contains(string(body), "mpm-mcp") && !strings.Contains(string(body), "/bin/echo") {
		t.Errorf("cfg body missing expected server: %s", body)
	}
	if !strings.Contains(string(body), "test-session-xyz") {
		t.Errorf("cfg body missing session_id: %s", body)
	}
}

func TestClaudeCodeHarness_ReadInvocationsForSession(t *testing.T) {
	dm := NewTestDM(t)
	sessionID := "harness-test-session"
	otherSession := "harness-test-other"

	// Seed mixed rows so the filter is actually exercised.
	rows := []struct {
		sess    string
		tool    string
		action  string
		started int64
	}{
		{sessionID, "mpm_context", "read_wake_context", 1700000001},
		{otherSession, "mpm_lessons", "save", 1700000002}, // different session — must NOT be picked up
		{sessionID, "mpm_lessons", "save", 1700000003},
		{otherSession, "mpm_system", "health_check", 1700000004},
	}
	for i, r := range rows {
		_, err := dm.SQLDB().Exec(`
			INSERT INTO tool_invocations
			    (id, session_id, tool_name, action, invocation_id,
			     actor_kind, framework_name, payload_hash, result_status,
			     started_at, completed_at, duration_ms)
			VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
			"inv-"+r.sess+"-"+r.tool+"-"+r.action+"-"+strconv.Itoa(i),
			r.sess, r.tool, r.action, "uuid-"+r.tool+"-"+r.action,
			"agent", "mcp", "sha256:x", "success",
			r.started, r.started+1, 1000,
		)
		if err != nil {
			t.Fatalf("seed %+v: %v", r, err)
		}
	}

	calls, err := readInvocationsForSession(dm.SQLDB(), sessionID)
	if err != nil {
		t.Fatalf("readInvocations: %v", err)
	}

	// Two rows belong to sessionID, two to the other. The function
	// must return exactly the two sessionID rows in started_at order.
	if len(calls) != 2 {
		t.Fatalf("got %d calls, want 2: %+v", len(calls), calls)
	}
	if calls[0].ToolName != "mpm_context" || calls[0].Action != "read_wake_context" {
		t.Errorf("first call = %+v, want mpm_context:read_wake_context", calls[0])
	}
	if calls[1].ToolName != "mpm_lessons" || calls[1].Action != "save" {
		t.Errorf("second call = %+v, want mpm_lessons:save", calls[1])
	}
}

func TestClaudeCodeHarness_FinishWithoutBeginFails(t *testing.T) {
	h := NewClaudeCodeHarness(t.TempDir(), "/bin/true", "/bin/echo")
	_, err := h.Finish(context.Background(), nil)
	if err == nil {
		t.Fatal("Finish without Begin should error")
	}
	if !strings.Contains(err.Error(), "Begin") {
		t.Errorf("error should mention Begin; got: %v", err)
	}
}
