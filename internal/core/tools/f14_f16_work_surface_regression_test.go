// f14_f16_work_surface_regression_test.go — regressions for audit findings
// F14 (work list/history bare-array shape) and F16 (framework/model hidden
// from user-facing history; works.session_id always NULL).
package tools

import (
	"fmt"
	"strings"
	"testing"

	internal "github.com/flowbyte-com/mpm-core"
)

// TestF16_HistorySurfacesFrameworkModel: provenance is authoritative; the
// user-facing history must project it instead of hiding it.
func TestF16_HistorySurfacesFrameworkModel(t *testing.T) {
	dm := newTestIsolatedDM(t)

	ac := internal.ActiveContext{
		SessionID:     "sess-f16",
		FrameworkName: "claude-code",
		Model:         "test-model-1",
	}
	w, err := dm.CreateWorkWithContext("f16 provenance work", "", "", ac)
	if err != nil {
		t.Fatalf("create: %v", err)
	}

	// session_id must be populated from the ActiveContext when the caller
	// omits it (F16 second symptom: works.session_id always NULL).
	if w.SessionID != "sess-f16" {
		t.Errorf("works.session_id = %q, want sess-f16 (ActiveContext default)", w.SessionID)
	}

	result, err := handleMpmWork(dm, ac, map[string]interface{}{
		"action": "history",
		"params": map[string]interface{}{"work_id": w.ID},
	})
	if err != nil {
		t.Fatalf("history: %v", err)
	}
	raw := fmt.Sprint(result)
	if !strings.Contains(raw, "claude-code") {
		t.Errorf("F16 REGRESSION: history does not surface framework_name: %s", raw)
	}

	// Missing optional metadata stays absent, not fabricated.
	w2, err := dm.CreateWorkWithContext("f16 bare work", "", "", internal.ActiveContext{})
	if err != nil {
		t.Fatalf("create2: %v", err)
	}
	res2, err := handleMpmWork(dm, internal.ActiveContext{}, map[string]interface{}{
		"action": "history",
		"params": map[string]interface{}{"work_id": w2.ID},
	})
	if err != nil {
		t.Fatalf("history2: %v", err)
	}
	env := res2.(map[string]interface{})
	events := env["events"].([]map[string]interface{})
	for _, e := range events {
		if v, ok := e["framework_name"]; ok && fmt.Sprint(v) == "fabricated" {
			t.Errorf("fabricated framework_name leaked")
		}
	}
}

// TestF16_ResolveHelperCoversBothSources pins the batched resolver:
// tool_invocations supplies framework; artifact_provenance supplies model;
// absence stays empty.
func TestF16_ResolveHelperCoversBothSources(t *testing.T) {
	dm := newTestIsolatedDM(t)

	if _, err := dm.SQLDB().Exec(`
		INSERT INTO tool_invocations (id, session_id, tool_name, action, invocation_id,
		                             actor_kind, framework_name, payload_hash, result_status, started_at)
		VALUES ('ti-1', 's', 'mpm_work', 'create', 'inv-abc', 'agent', 'opencode', 'h', 'success', 1)
	`); err != nil {
		t.Fatalf("seed tool_invocation: %v", err)
	}

	got := dm.ResolveFrameworkModelForInvocations("no-such-work", []string{"inv-abc"})
	fm, ok := got["inv-abc"]
	if !ok || fm.FrameworkName != "opencode" {
		t.Fatalf("framework from tool_invocations not resolved: %#v", got["inv-abc"])
	}
	if len(got) != 1 {
		t.Errorf("resolver returned %d entries for 1 invocation id", len(got))
	}
}
