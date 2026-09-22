// correlation_tool_origin_test.go — pin the end-to-end tool-originated
// audit correlation contract for the MCP mpm_memory save dispatch.
//
// Behavioral role: every MCP tools/call that targets mpm_memory.save and
// triggers the security/poison scanner must leave TWO correlated rows:
//
//   - one tool_invocations row (recordToolInvocation in mcpAdapter)
//   - one system_audit_log row (saveMemoryRow → emitMemorySaveAudit)
//
// sharing the same invocation_id / mpm_session_id / framework_name /
// framework_session_id. This mirrors the CLI fixture in
// cmd/mpm/correlation_tool_origin_test.go for the MCP dispatch surface.
//
// Important: these tests invoke the real MCP adapter (mcpAdapter), NOT
// a hand-rolled recordToolInvocation call. The dispatcher is the
// system under test.

package main

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"io"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/mark3labs/mcp-go/mcp"

	core "github.com/flowbyte-com/mpm-core"
	"github.com/flowbyte-com/mpm-core/tools"
	"github.com/flowbyte-com/mpm/internal/blobstore"
)

// mockBlobStoreCorr implements blobstore.BlobStore with no-ops. The
// mcpAdapter only touches blobStore when the OutputPolicy decides to
// spill; our fixture returns DecisionPass, so the store is never
// invoked. The full interface is satisfied to keep type-checking honest.
type mockBlobStoreCorr struct {
	putCount uint64
}

func (m *mockBlobStoreCorr) Put(_ context.Context, _ io.Reader, _ blobstore.Metadata) (blobstore.Pointer, error) {
	id := atomic.AddUint64(&m.putCount, 1)
	return blobstore.Pointer{Kind: "blob", ID: "blob-corr-" + fmt.Sprintf("%d", id)}, nil
}
func (m *mockBlobStoreCorr) Get(_ context.Context, _ string, _ blobstore.GetOptions) (io.ReadCloser, blobstore.Metadata, error) {
	return nil, blobstore.Metadata{}, errors.New("not implemented")
}
func (m *mockBlobStoreCorr) Delete(_ context.Context, _ string) error { return nil }
func (m *mockBlobStoreCorr) Search(_ context.Context, _ string, _ blobstore.SearchQuery) ([]blobstore.Match, error) {
	return nil, nil
}
func (m *mockBlobStoreCorr) GCExpired(_ context.Context, _ time.Time) (blobstore.GCStats, error) {
	return blobstore.GCStats{}, nil
}
func (m *mockBlobStoreCorr) GCSweepOrphans(_ context.Context, _ time.Duration) (blobstore.GCStats, error) {
	return blobstore.GCStats{}, nil
}
func (m *mockBlobStoreCorr) TTL() time.Duration { return 24 * time.Hour }

// mockOutputPolicyCorr returns DecisionPass so the adapter never
// reaches the spill path during these tests.
type mockOutputPolicyCorr struct{}

func (m *mockOutputPolicyCorr) Apply(_ context.Context, b []byte) (tools.Decision, int, error) {
	return tools.DecisionPass, len(b), nil
}

// driveMCPSaveSensitive runs the real MCP adapter on an mpm_memory
// save payload that triggers the sensitive-content scanner. The fact
// shape `sk-proj-DEADBEEF...` matches the OpenAI Project Key pattern
// (sensitivePatterns[0]).
func driveMCPSaveSensitive(t *testing.T, dm *core.DatabaseManager, ac core.ActiveContext) {
	t.Helper()
	handler := tools.MustByName("mpm_memory").Handler
	adapter, restore := mcpAdapterForTest(dm, ac, handler,
		&mockBlobStoreCorr{},
		&mockOutputPolicyCorr{})
	defer restore()

	req := mcp.CallToolRequest{}
	req.Params.Name = "mpm_memory"
	req.Params.Arguments = map[string]interface{}{
		"action": "save",
		"params": map[string]interface{}{
			"fact": "sk-proj-DEADBEEF1234567890abcdef",
		},
	}

	// Sensitive content is a hard rejection. The MCP transport returns
	// a non-nil err string in the result envelope, but the DB rows are
	// what we assert on.
	_, _ = adapter(context.Background(), req)
}

// TestCorrelation_AutomaticInvocationID_MCP_Save pins the canonical
// MCP-tool-originated correlation contract: when env is unset, the
// same auto-generated UUID appears in BOTH the tool_invocations row
// (from mcpAdapter's recordToolInvocation) and the system_audit_log
// row (from the security scanner via the shared mpm_memory handler).
func TestCorrelation_AutomaticInvocationID_MCP_Save(t *testing.T) {
	dm := newIsolatedTestDM(t)

	driveMCPSaveSensitive(t, dm, core.ActiveContext{SessionID: "mcp-corr-session"})

	// Query the tool_invocations row.
	var tiInv string
	var tiMPMSess, tiFW, tiFWS sql.NullString
	if err := dm.SQLDB().QueryRow(`
		SELECT invocation_id, mpm_session_id, framework_name, framework_session_id
		FROM tool_invocations
		ORDER BY rowid DESC LIMIT 1`,
	).Scan(&tiInv, &tiMPMSess, &tiFW, &tiFWS); err != nil {
		t.Fatalf("read tool_invocations: %v", err)
	}
	// Query the security audit row.
	var auInv string
	var auMPMSess, auFW, auFWS, eventCode sql.NullString
	if err := dm.SQLDB().QueryRow(`
		SELECT invocation_id, mpm_session_id, framework_name, framework_session_id, event_code
		FROM system_audit_log
		WHERE component = 'security'
		ORDER BY rowid DESC LIMIT 1`,
	).Scan(&auInv, &auMPMSess, &auFW, &auFWS, &eventCode); err != nil {
		t.Fatalf("read system_audit_log: %v", err)
	}

	if tiInv == "" {
		t.Fatalf("tool_invocations.invocation_id is empty; mcpAdapter auto-generation did not happen")
	}
	if auInv == "" {
		t.Fatalf("system_audit_log.invocation_id is empty; ActiveContext did not thread to the scanner audit")
	}
	if tiInv != auInv {
		t.Errorf("invocation_id mismatch: tool_invocations=%q system_audit_log=%q (must be identical)", tiInv, auInv)
	}
	if tiMPMSess.String != auMPMSess.String || tiMPMSess.Valid != auMPMSess.Valid {
		t.Errorf("mpm_session_id mismatch: ti=%v audit=%v (must be identical)", tiMPMSess, auMPMSess)
	}
	if tiFW.String != auFW.String || tiFW.Valid != auFW.Valid {
		t.Errorf("framework_name mismatch: ti=%v audit=%v (must be identical)", tiFW, auFW)
	}
	if tiFWS.Valid || tiFWS.String != "" {
		t.Errorf("tool_invocations.framework_session_id = %q (valid=%v), want empty (SQL NULL)", tiFWS.String, tiFWS.Valid)
	}
	if auFWS.Valid || auFWS.String != "" {
		t.Errorf("system_audit_log.framework_session_id = %q (valid=%v), want empty (SQL NULL)", auFWS.String, auFWS.Valid)
	}
	if !strings.HasPrefix(eventCode.String, "memory_save_") {
		t.Errorf("event_code = %q, want memory_save_* prefix", eventCode.String)
	}
}

// TestCorrelation_ExplicitInvocationID_MCP_Save pins the
// caller-supplied-ID branch for MCP dispatch: an explicit ID on the
// boot ActiveContext flows through the same path and surfaces on BOTH
// rows.
func TestCorrelation_ExplicitInvocationID_MCP_Save(t *testing.T) {
	dm := newIsolatedTestDM(t)

	const explicitID = "mcp-explicit-invocation-id-12345"
	ac := core.ActiveContext{
		SessionID:    "mcp-explicit-session",
		InvocationID: explicitID,
		// FrameworkSessionID intentionally empty to verify the NULL invariant.
	}
	driveMCPSaveSensitive(t, dm, ac)

	var tiInv string
	if err := dm.SQLDB().QueryRow(
		`SELECT invocation_id FROM tool_invocations ORDER BY rowid DESC LIMIT 1`,
	).Scan(&tiInv); err != nil {
		t.Fatalf("read tool_invocations: %v", err)
	}
	var auInv string
	if err := dm.SQLDB().QueryRow(
		`SELECT invocation_id FROM system_audit_log WHERE component='security' ORDER BY rowid DESC LIMIT 1`,
	).Scan(&auInv); err != nil {
		t.Fatalf("read system_audit_log: %v", err)
	}

	if tiInv != explicitID {
		t.Errorf("tool_invocations.invocation_id = %q, want %q", tiInv, explicitID)
	}
	if auInv != explicitID {
		t.Errorf("system_audit_log.invocation_id = %q, want %q", auInv, explicitID)
	}
}

// TestCorrelation_MCP_FrameworkNameIsMcp pins the framework_name
// surface for MCP dispatch: the adapter pins framework_name="mcp"
// when the boot ActiveContext leaves it empty.
func TestCorrelation_MCP_FrameworkNameIsMcp(t *testing.T) {
	dm := newIsolatedTestDM(t)

	driveMCPSaveSensitive(t, dm, core.ActiveContext{SessionID: "mcp-fw-session"})

	var tiFW, auFW string
	if err := dm.SQLDB().QueryRow(
		`SELECT framework_name FROM tool_invocations ORDER BY rowid DESC LIMIT 1`,
	).Scan(&tiFW); err != nil {
		t.Fatalf("read tool_invocations.framework_name: %v", err)
	}
	if err := dm.SQLDB().QueryRow(
		`SELECT framework_name FROM system_audit_log WHERE component='security' ORDER BY rowid DESC LIMIT 1`,
	).Scan(&auFW); err != nil {
		t.Fatalf("read system_audit_log.framework_name: %v", err)
	}
	if tiFW != "mcp" {
		t.Errorf("tool_invocations.framework_name = %q, want mcp", tiFW)
	}
	if auFW != "mcp" {
		t.Errorf("system_audit_log.framework_name = %q, want mcp", auFW)
	}
}

// TestCorrelation_MCP_NativeSessionProvided pins the
// "host has native session" branch for MCP dispatch. When the boot
// ActiveContext carries a FrameworkSessionID, the value flows through
// to BOTH rows identically.
func TestCorrelation_MCP_NativeSessionProvided(t *testing.T) {
	dm := newIsolatedTestDM(t)

	const fwsID = "mcp-host-native-session-fixture"
	ac := core.ActiveContext{
		SessionID:          "mcp-fws-session",
		InvocationID:       "mcp-fws-inv",
		FrameworkSessionID: fwsID,
	}
	driveMCPSaveSensitive(t, dm, ac)

	var tiFWS, auFWS string
	if err := dm.SQLDB().QueryRow(
		`SELECT framework_session_id FROM tool_invocations ORDER BY rowid DESC LIMIT 1`,
	).Scan(&tiFWS); err != nil {
		t.Fatalf("read tool_invocations.framework_session_id: %v", err)
	}
	if err := dm.SQLDB().QueryRow(
		`SELECT framework_session_id FROM system_audit_log WHERE component='security' ORDER BY rowid DESC LIMIT 1`,
	).Scan(&auFWS); err != nil {
		t.Fatalf("read system_audit_log.framework_session_id: %v", err)
	}
	if tiFWS != fwsID {
		t.Errorf("tool_invocations.framework_session_id = %q, want %q", tiFWS, fwsID)
	}
	if auFWS != fwsID {
		t.Errorf("system_audit_log.framework_session_id = %q, want %q", auFWS, fwsID)
	}
}

// stringOrNull is no longer needed (we use sql.NullString from the
// standard library directly), but the type alias is kept for future
// test additions that need a custom scanner.
