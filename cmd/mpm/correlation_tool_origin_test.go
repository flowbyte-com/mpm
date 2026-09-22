// correlation_tool_origin_test.go — pin the end-to-end tool-originated
// audit correlation contract for `mpm call mpm_memory save`.
//
// Behavioral role: every `mpm call mpm_memory save` that triggers the
// security/poison scanner (sensitive content blocked, poison content
// blocked) must leave TWO correlated rows:
//
//   - one tool_invocations row (audit_hook.go:67-70)
//   - one system_audit_log row (saveMemoryRow → emitMemorySaveAudit)
//
// sharing the same invocation_id / mpm_session_id / framework_name /
// framework_session_id. This is the bridge that proves the dispatcher
// identity propagates all the way to the operational audit ledger.
//
// Invariant pinned by this file:
//
//   * MPM_PROVENANCE_INVOCATION_ID unset → auto-generated UUID flows
//     from call.go:149-152 → ActiveContext → audit_hook →
//     SaveMemoryNodeForInvocation → system_audit_log.invocation_id.
//
//   * MPM_PROVENANCE_INVOCATION_ID=explicit → the explicit value flows
//     through the same path; no UUID regeneration occurs downstream.
//
//   * MPM_PROVENANCE_FRAMEWORK_SESSION_ID unset → framework_session_id
//     is SQL NULL on both rows (empty-string → NULL invariant).

package main

import (
	"bytes"
	"database/sql"
	"os"
	"strings"
	"testing"

	mpminternal "github.com/flowbyte-com/mpm-core"
)

// captureStdoutForCorrelation swaps os.Stdout for a pipe and returns
// the captured bytes. Named to avoid collision with the project's
// existing captureStdout helper (f8_f10_regression_test.go).
func captureStdoutForCorrelation(t *testing.T) (restore func()) {
	t.Helper()
	origStdout := os.Stdout
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatalf("os.Pipe: %v", err)
	}
	os.Stdout = w
	return func() {
		_ = w.Close()
		var buf bytes.Buffer
		_, _ = buf.ReadFrom(r)
		_ = r.Close()
		os.Stdout = origStdout
	}
}

// driveToolSaveSensitive runs `mpm call mpm_memory save` through the
// real CLI handler chain with a fact that triggers the sensitive-
// content scanner. The fact shape `sk-proj-DEADBEEF...` matches the
// OpenAI Project Key pattern (sensitivePatterns[0]).
func driveToolSaveSensitive(t *testing.T, ws string) {
	t.Helper()
	restore := captureStdoutForCorrelation(t)
	defer restore()

	exit := handleCall([]string{
		"mpm_memory",
		"--payload", `{"action":"save","params":{"fact":"sk-proj-DEADBEEF1234567890abcdef"}}`,
	})
	// Sensitive content is a hard rejection, not an exit-1 error from
	// handleCall's perspective — the tool still returned a structured
	// failure envelope. We don't assert exit; we assert the database
	// state below.
	_ = exit
}

// readToolInvocationRow fetches the most recent tool_invocations row.
func readToolInvocationRow(t *testing.T, dm *mpminternal.DatabaseManager) (invID, mpmsess, fw, fws string, err error) {
	t.Helper()
	var fwsNull sql.NullString
	var mpmsessNull sql.NullString
	row := dm.SQLDB().QueryRow(`
		SELECT invocation_id, mpm_session_id, framework_name, framework_session_id
		FROM tool_invocations
		ORDER BY rowid DESC LIMIT 1`)
	if scanErr := row.Scan(&invID, &mpmsessNull, &fw, &fwsNull); scanErr != nil {
		return "", "", "", "", scanErr
	}
	if mpmsessNull.Valid {
		mpmsess = mpmsessNull.String
	}
	if fwsNull.Valid {
		fws = fwsNull.String
	}
	return invID, mpmsess, fw, fws, nil
}

// readSecurityAuditRow fetches the most recent security audit row.
func readSecurityAuditRow(t *testing.T, dm *mpminternal.DatabaseManager) (invID, mpmsess, fw, fws, ec string, err error) {
	t.Helper()
	var invIDNull, mpmsessNull, fwNull, fwsNull, ecNull sql.NullString
	row := dm.SQLDB().QueryRow(`
		SELECT invocation_id, mpm_session_id, framework_name, framework_session_id, event_code
		FROM system_audit_log
		WHERE component = 'security'
		ORDER BY rowid DESC LIMIT 1`)
	if scanErr := row.Scan(&invIDNull, &mpmsessNull, &fwNull, &fwsNull, &ecNull); scanErr != nil {
		return "", "", "", "", "", scanErr
	}
	if invIDNull.Valid {
		invID = invIDNull.String
	}
	if mpmsessNull.Valid {
		mpmsess = mpmsessNull.String
	}
	if fwNull.Valid {
		fw = fwNull.String
	}
	if fwsNull.Valid {
		fws = fwsNull.String
	}
	if ecNull.Valid {
		ec = ecNull.String
	}
	return invID, mpmsess, fw, fws, ec, nil
}

// TestCorrelation_AutomaticInvocationID_ToolOriginated_Save pins the
// canonical tool-originated correlation contract: when env is unset,
// the same auto-generated UUID appears in BOTH the tool_invocations row
// and the system_audit_log row emitted by the security scanner.
//
// No direct INSERT — both come from the production dispatch path:
// call.go → audit_hook.go → handleSaveToMemory →
// SaveMemoryWithContextAndSnapshot → saveMemoryWithContextImpl →
// AddMemoryWithWeightForInvocation → SaveMemoryNodeForInvocation →
// saveMemoryRow → emitMemorySaveAudit.
func TestCorrelation_AutomaticInvocationID_ToolOriginated_Save(t *testing.T) {
	ws := t.TempDir()
	t.Setenv("MPM_WORKSPACE", ws)

	// Explicitly unset the provenance invocation ID — the dispatcher
	// is responsible for minting a fresh identity.
	if err := os.Unsetenv("MPM_PROVENANCE_INVOCATION_ID"); err != nil {
		t.Fatalf("unset MPM_PROVENANCE_INVOCATION_ID: %v", err)
	}
	if err := os.Unsetenv("MPM_PROVENANCE_FRAMEWORK_SESSION_ID"); err != nil {
		t.Fatalf("unset MPM_PROVENANCE_FRAMEWORK_SESSION_ID: %v", err)
	}

	driveToolSaveSensitive(t, ws)

	// Open the workspace DB to query the rows.
	dm, err := mpminternal.NewDatabaseManager(ws)
	if err != nil {
		t.Fatalf("NewDatabaseManager(%q): %v", ws, err)
	}
	defer dm.Close()

	tiInv, tiMPMSess, tiFW, tiFWS, err := readToolInvocationRow(t, dm)
	if err != nil {
		t.Fatalf("read tool_invocations row: %v", err)
	}
	auInv, auMPMSess, auFW, auFWS, eventCode, err := readSecurityAuditRow(t, dm)
	if err != nil {
		t.Fatalf("read system_audit_log row: %v", err)
	}

	if tiInv == "" {
		t.Fatalf("tool_invocations.invocation_id is empty; auto-generation did not happen at dispatcher")
	}
	if auInv == "" {
		t.Fatalf("system_audit_log.invocation_id is empty; ActiveContext did not thread to the scanner audit")
	}
	if tiInv != auInv {
		t.Errorf("invocation_id mismatch: tool_invocations=%q system_audit_log=%q (must be identical)", tiInv, auInv)
	}
	if tiMPMSess != auMPMSess {
		t.Errorf("mpm_session_id mismatch: ti=%q audit=%q (must be identical)", tiMPMSess, auMPMSess)
	}
	if tiFW != auFW {
		t.Errorf("framework_name mismatch: ti=%q audit=%q (must be identical)", tiFW, auFW)
	}
	// Absent-native-session: framework_session_id must be NULL on both
	// rows. The empty-string → SQL NULL invariant is enforced by
	// nullStrForAudit (audit.go:222) and audit_hook.go:111-115.
	if tiFWS != "" {
		t.Errorf("tool_invocations.framework_session_id = %q, want empty (SQL NULL)", tiFWS)
	}
	if auFWS != "" {
		t.Errorf("system_audit_log.framework_session_id = %q, want empty (SQL NULL)", auFWS)
	}
	if !strings.HasPrefix(eventCode, "memory_save_") {
		t.Errorf("event_code = %q, want memory_save_* prefix (bounded operational code)", eventCode)
	}
}

// TestCorrelation_ExplicitInvocationID_ToolOriginated_Save pins the
// caller-supplied-ID branch: when MPM_PROVENANCE_INVOCATION_ID is set
// the explicit value flows through the same dispatch chain and
// surfaces on BOTH rows. No downstream UUID regeneration.
func TestCorrelation_ExplicitInvocationID_ToolOriginated_Save(t *testing.T) {
	ws := t.TempDir()
	t.Setenv("MPM_WORKSPACE", ws)

	const explicitID = "explicit-test-invocation-id-12345"
	t.Setenv("MPM_PROVENANCE_INVOCATION_ID", explicitID)
	if err := os.Unsetenv("MPM_PROVENANCE_FRAMEWORK_SESSION_ID"); err != nil {
		t.Fatalf("unset MPM_PROVENANCE_FRAMEWORK_SESSION_ID: %v", err)
	}

	driveToolSaveSensitive(t, ws)

	dm, err := mpminternal.NewDatabaseManager(ws)
	if err != nil {
		t.Fatalf("NewDatabaseManager(%q): %v", ws, err)
	}
	defer dm.Close()

	tiInv, _, tiFW, _, err := readToolInvocationRow(t, dm)
	if err != nil {
		t.Fatalf("read tool_invocations row: %v", err)
	}
	auInv, _, auFW, _, _, err := readSecurityAuditRow(t, dm)
	if err != nil {
		t.Fatalf("read system_audit_log row: %v", err)
	}

	if tiInv != explicitID {
		t.Errorf("tool_invocations.invocation_id = %q, want %q", tiInv, explicitID)
	}
	if auInv != explicitID {
		t.Errorf("system_audit_log.invocation_id = %q, want %q", auInv, explicitID)
	}
	// Default framework_name when only invocation id is supplied: mpm-cli.
	if tiFW != "mpm-cli" {
		t.Errorf("tool_invocations.framework_name = %q, want mpm-cli", tiFW)
	}
	if auFW != "mpm-cli" {
		t.Errorf("system_audit_log.framework_name = %q, want mpm-cli", auFW)
	}
}

// TestCorrelation_NativeSessionProvided_ToolOriginated_Save pins the
// "host has native session" branch: when
// MPM_PROVENANCE_FRAMEWORK_SESSION_ID is supplied, the value flows
// into both rows identically.
func TestCorrelation_NativeSessionProvided_ToolOriginated_Save(t *testing.T) {
	ws := t.TempDir()
	t.Setenv("MPM_WORKSPACE", ws)

	const fwsID = "host-native-session-fixture"
	t.Setenv("MPM_PROVENANCE_INVOCATION_ID", "explicit-with-fws")
	t.Setenv("MPM_PROVENANCE_FRAMEWORK_SESSION_ID", fwsID)

	driveToolSaveSensitive(t, ws)

	dm, err := mpminternal.NewDatabaseManager(ws)
	if err != nil {
		t.Fatalf("NewDatabaseManager(%q): %v", ws, err)
	}
	defer dm.Close()

	_, _, _, tiFWS, err := readToolInvocationRow(t, dm)
	if err != nil {
		t.Fatalf("read tool_invocations row: %v", err)
	}
	_, _, _, auFWS, _, err := readSecurityAuditRow(t, dm)
	if err != nil {
		t.Fatalf("read system_audit_log row: %v", err)
	}

	if tiFWS != fwsID {
		t.Errorf("tool_invocations.framework_session_id = %q, want %q", tiFWS, fwsID)
	}
	if auFWS != fwsID {
		t.Errorf("system_audit_log.framework_session_id = %q, want %q", auFWS, fwsID)
	}
}

// TestCorrelation_OrdinarySave_OnlyToolInvocationRow pins the
// write-amplification invariant: a save that PASSES the scanner must
// leave exactly ONE tool_invocations row and ZERO security audit rows.
// A blocked save leaves BOTH rows. No duplicate audits.
func TestCorrelation_OrdinarySave_OnlyToolInvocationRow(t *testing.T) {
	ws := t.TempDir()
	t.Setenv("MPM_WORKSPACE", ws)
	if err := os.Unsetenv("MPM_PROVENANCE_INVOCATION_ID"); err != nil {
		t.Fatalf("unset MPM_PROVENANCE_INVOCATION_ID: %v", err)
	}

	// Ordinary save — no sensitive content.
	restore := captureStdoutForCorrelation(t)
	defer restore()
	exit := handleCall([]string{
		"mpm_memory",
		"--payload", `{"action":"save","params":{"fact":"ordinary fact, no secrets"}}`,
	})
	if exit != 0 {
		t.Fatalf("ordinary save exited %d, want 0", exit)
	}

	dm, err := mpminternal.NewDatabaseManager(ws)
	if err != nil {
		t.Fatalf("NewDatabaseManager(%q): %v", ws, err)
	}
	defer dm.Close()

	var tiCount, auditCount int
	if err := dm.SQLDB().QueryRow(
		`SELECT COUNT(*) FROM tool_invocations WHERE tool_name = 'mpm_memory'`,
	).Scan(&tiCount); err != nil {
		t.Fatalf("count tool_invocations: %v", err)
	}
	if err := dm.SQLDB().QueryRow(
		`SELECT COUNT(*) FROM system_audit_log WHERE component = 'security'`,
	).Scan(&auditCount); err != nil {
		t.Fatalf("count security audits: %v", err)
	}

	if tiCount != 1 {
		t.Errorf("ordinary save tool_invocations rows = %d, want 1", tiCount)
	}
	if auditCount != 0 {
		t.Errorf("ordinary save security audit rows = %d, want 0 (no scanner block fired)", auditCount)
	}
}
