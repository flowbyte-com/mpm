package internal

// Alpha-4.1 F-007 / W-002 regression: `mpm_system query_audit_log` must
// NOT include the multi-KB stack_trace payload in default projections.
// Operators opt into the stack_trace via `include_stack=true` for
// incident triage.
//
// Pre-fix shape: every audit row carried stack_trace, bloating the
// per-row payload by 1-3 KB even when the caller only needed the
// headline (id, level, component, message, context). The audit
// surface is the agent's first stop when triaging "what went wrong",
// so the default projection needs to be the small one.
//
// Post-fix contract:
//   - include_stack=false (default) → row has NO stack_trace field
//   - include_stack=true           → row has the stack_trace string

import (
	"path/filepath"
	"strings"
	"testing"
)

// TestF007_AuditStack_OptInByDefault confirms that the default
// projection from QueryAuditLog omits the stack_trace field. The
// caller must pass include_stack=true to receive it.
func TestF007_AuditStack_OptInByDefault(t *testing.T) {
	dm, cleanup := setupTestDB(t)
	defer cleanup()

	// Seed an audit row with a deliberate stack trace so we can tell
	// whether it leaked into the default projection.
	const wantStack = "alpha-4.1 f007 opt-in sentinel"
	dm.LogAudit(AuditError, "alpha-4.1-f007", "f007 default-projection test", wantStack, AuditContext{"origin": "regression"})

	// Default projection — includeStack=false.
	rows, err := dm.QueryAuditLog("", "alpha-4.1-f007", "", 1, 10, false)
	if err != nil {
		t.Fatalf("query audit (default): %v", err)
	}
	if len(rows) == 0 {
		t.Fatalf("seed row missing from audit query")
	}
	for _, r := range rows {
		if _, has := r["stack_trace"]; has {
			t.Errorf("default projection leaked stack_trace: %v", r["stack_trace"])
		}
	}

	// Opt-in projection — includeStack=true.
	rowsOpt, err := dm.QueryAuditLog("", "alpha-4.1-f007", "", 1, 10, true)
	if err != nil {
		t.Fatalf("query audit (opt-in): %v", err)
	}
	if len(rowsOpt) == 0 {
		t.Fatalf("opt-in query returned no rows")
	}
	foundStack := false
	for _, r := range rowsOpt {
		st, ok := r["stack_trace"].(string)
		if !ok {
			t.Errorf("opt-in projection missing stack_trace string: %#v", r["stack_trace"])
			continue
		}
		if strings.Contains(st, wantStack) {
			foundStack = true
		}
	}
	if !foundStack {
		t.Errorf("opt-in projection did not surface the seeded stack sentinel")
	}
}

// TestF007_AuditStack_DefaultOmitsEvenOnMultipleRows confirms the
// opt-in behaviour holds for batched results — the entire page must
// be free of stack_trace when include_stack=false.
func TestF007_AuditStack_DefaultOmitsEvenOnMultipleRows(t *testing.T) {
	dm, cleanup := setupTestDB(t)
	defer cleanup()

	for i := 0; i < 3; i++ {
		dm.LogAudit(AuditWarn, "alpha-4.1-f007-batch", "batch row", "stack-payload-ignore-me", nil)
	}
	rows, err := dm.QueryAuditLog("", "alpha-4.1-f007-batch", "", 1, 10, false)
	if err != nil {
		t.Fatalf("query audit: %v", err)
	}
	if len(rows) != 3 {
		t.Fatalf("expected 3 rows, got %d", len(rows))
	}
	for i, r := range rows {
		if _, has := r["stack_trace"]; has {
			t.Errorf("row %d leaked stack_trace in default projection", i)
		}
	}
}

// TestF007_AuditStack_DoesNotMutateHeaderFields confirms that adding
// the includeStack parameter did not disturb the stable keys the
// agent's wake-context code relies on. The header fields (id, level,
// component, message, created_at) must be present in BOTH projections.
func TestF007_AuditStack_DoesNotMutateHeaderFields(t *testing.T) {
	dm, cleanup := setupTestDB(t)
	defer cleanup()

	dm.LogAudit(AuditError, "alpha-4.1-f007-headers", "header test", "stack-data", AuditContext{"k": "v"})

	for _, opt := range []bool{false, true} {
		rows, err := dm.QueryAuditLog("", "alpha-4.1-f007-headers", "", 1, 10, opt)
		if err != nil {
			t.Fatalf("opt=%v: %v", opt, err)
		}
		if len(rows) == 0 {
			t.Fatalf("opt=%v: no rows", opt)
		}
		row := rows[0]
		for _, k := range []string{"id", "level", "component", "message", "created_at"} {
			if _, has := row[k]; !has {
				t.Errorf("opt=%v: header field %q missing", opt, k)
			}
		}
	}
}

// setupTestDB wires a fresh in-memory database for the alpha-4.1
// audit regression tests. The pattern mirrors the other alpha-4.1
// regression suites — TestMain in the core package owns the global
// fixtures, so we open a private DatabaseManager that the alpha-4.1
// audit log writes don't need to share.
func setupTestDB(t *testing.T) (*DatabaseManager, func()) {
	t.Helper()
	tmp := filepath.Join(t.TempDir(), "mpm-audit-f007.db")
	dm, err := NewDatabaseManager(tmp)
	if err != nil {
		t.Fatalf("NewDatabaseManager: %v", err)
	}
	return dm, func() {
		dm.Close()
	}
}
