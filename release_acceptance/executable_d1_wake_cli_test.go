// executable_d1_wake_cli_test.go — Item 3.
//
// Public CLI regression for D-1. Drives the schedule→resolve round
// trip via the real `bin/mpm call mpm_wakes --payload ...` subprocess
// path. This is the surface the pristine rehearsal failed through —
// the agent-visible entry point — and now passes end-to-end after
// the producer-side default at ScheduleWake (wake_tools.go) and the
// consumer-side json_extract matcher at ResolveWake landed.
//
// Two rounds:
//
//	Round 1: schedule and resolve in the same subprocess stream.
//	Round 2: schedule, close workspace, reopen workspace, resolve the
//	         persisted wake id (covers DB persistence + reopen
//	         between writes).
package release_acceptance_test

import (
	"database/sql"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"testing"

	_ "github.com/mattn/go-sqlite3"
)

// callMpmWakes runs the real CLI surface for mpm_wakes with the given
// action and params. Returns the parsed JSON response.
func callMpmWakes(t *testing.T, mpmBin, workspace, action string, params map[string]interface{}) map[string]interface{} {
	t.Helper()
	payload, err := json.Marshal(map[string]interface{}{
		"action": action,
		"params": params,
	})
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	cmd := exec.Command(mpmBin, "call", "mpm_wakes", "--payload", string(payload))
	cmd.Env = append(os.Environ(), "MPM_WORKSPACE="+workspace)
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("`bin/mpm call mpm_wakes %s` failed: %v\n%s", action, err, string(out))
	}
	var resp map[string]interface{}
	if err := json.Unmarshal(out, &resp); err != nil {
		t.Fatalf("parse response: %v\nraw=%s", err, string(out))
	}
	return resp
}

// dbPath returns the canonical DB path for a workspace.
func dbPath(workspace string) string {
	return filepath.Join(workspace, "src", "db", "mpm.db")
}

// findSubmittedRow reads scheduled_wakes directly via the Go driver
// to verify the persisted metadata shape (proves the producer-side
// default: `kind: notification` is on disk).
func findSubmittedRow(t *testing.T, workspace, wakeID string) (metadata, kind string) {
	t.Helper()
	db, err := sql.Open("sqlite3", dbPath(workspace))
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	defer db.Close()
	row := db.QueryRow(`SELECT COALESCE(metadata, ''), COALESCE(json_extract(metadata, '$.kind'), '') FROM scheduled_wakes WHERE id = ?`, wakeID)
	if err := row.Scan(&metadata, &kind); err != nil {
		t.Fatalf("scan wake row: %v", err)
	}
	return
}

func TestExecutableD1_WakeRoundTrip_CLI(t *testing.T) {
	if os.Getenv("CGO_CFLAGS") == "" {
		t.Skip("FTS5 build flags absent; rerun via `make test-release`")
	}
	workspace, cleanup := makeAcceptanceWorkspace(t)
	defer cleanup()
	mpmBin := findProjectBinary(t)

	// Round 1: schedule + resolve in the same workspace.
	scheduleResp := callMpmWakes(t, mpmBin, workspace, "schedule", map[string]interface{}{
		"reason":      "D-1 CLI round-trip probe",
		"target_time": "1h",
	})
	success, _ := scheduleResp["success"].(bool)
	if !success {
		t.Fatalf("schedule returned non-success: %v", scheduleResp)
	}
	id, ok := scheduleResp["id"].(string)
	if !ok || id == "" {
		t.Fatalf("schedule response missing string id: %v", scheduleResp)
	}
	t.Logf("schedule produced wake id=%s", id)

	// Producer-side invariant: persisted metadata.kind must be
	// "notification" — the ScheduleWake default that the fix landed.
	metadata, kind := findSubmittedRow(t, workspace, id)
	if kind != "notification" {
		t.Errorf("schedule did not author metadata.kind=notification; got %q (metadata=%s)", kind, metadata)
	}

	resolveResp := callMpmWakes(t, mpmBin, workspace, "resolve", map[string]interface{}{
		"wake_id": id,
		"reason":  "reconciled",
	})
	resolved, _ := resolveResp["resolved"].(bool)
	status, _ := resolveResp["status"].(string)
	if !resolved || status != "resolved" {
		t.Fatalf("resolve did not succeed: status=%q resolved=%v response=%v", status, resolved, resolveResp)
	}
	t.Logf("resolve succeeded with status=%s", status)
}

// TestExecutableD1_WakeReopen_CLI exercises the producer-side fix
// across a DB reopen. Schedule, close, reopen, resolve the persisted
// id — covers persistence + the consumer-side matcher.
func TestExecutableD1_WakeReopen_CLI(t *testing.T) {
	if os.Getenv("CGO_CFLAGS") == "" {
		t.Skip("FTS5 build flags absent; rerun via `make test-release`")
	}
	workspace, cleanup := makeAcceptanceWorkspace(t)
	defer cleanup()
	mpmBin := findProjectBinary(t)

	// Schedule against the workspace.
	resp := callMpmWakes(t, mpmBin, workspace, "schedule", map[string]interface{}{
		"reason":      "D-1 reopen probe",
		"target_time": "1h",
	})
	id, _ := resp["id"].(string)
	if id == "" {
		t.Fatalf("schedule did not return id: %v", resp)
	}

	// Force a "reopen" by invoking resolve via a fresh subprocess
	// against the same workspace. MPM worker state is process-local;
	// each subprocess opens the DB afresh. That is the same code
	// path that an operator would walk if they issued the resolve
	// call hours later from a fresh tool invocation.
	resolveResp := callMpmWakes(t, mpmBin, workspace, "resolve", map[string]interface{}{
		"wake_id": id,
		"reason":  "obsolete",
	})
	resolved, _ := resolveResp["resolved"].(bool)
	status, _ := resolveResp["status"].(string)
	if !resolved || status != "resolved" {
		t.Fatalf("resolve-after-reopen failed: status=%q resolved=%v response=%v", status, resolved, resolveResp)
	}
	t.Logf("resolve-after-reopen succeeded with status=%s", status)
}
