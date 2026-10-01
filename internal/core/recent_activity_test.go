// recent_activity_test.go — Stage 2 integration tests for the
// recent_activity surface. Covers the cross-surface behavior
// (#25), scratchpad history (#26), work enrichment (#27),
// historical framework normalization (#28), human CLI (#29),
// secret redaction (#30), failed invocation (#31), ordering/
// filters (#32), observer effect (#33), and wake-context
// integration (#34).
package internal

import (
	"context"
	"database/sql"
	"encoding/json"
	"strings"
	"testing"
	"time"
)

// seedToolInvocation inserts a synthetic tool_invocations row
// for a given (tool, action, framework, actor_kind, result_status).
// Used to construct deterministic test fixtures without going
// through the full CLI/MCP dispatcher.
func seedToolInvocation(t *testing.T, dm *DatabaseManager, args seedArgs) {
	t.Helper()
	startedAt := args.StartedAt
	if startedAt == 0 {
		startedAt = time.Now().Unix()
	}
	completedAt := args.CompletedAt
	if completedAt == 0 {
		completedAt = startedAt + 1
	}
	sessionID := args.SessionID
	if sessionID == "" {
		sessionID = "test-session"
	}
	invocationID := args.InvocationID
	if invocationID == "" {
		invocationID = "inv-" + args.Tool + "-" + args.Action
	}
	status := args.ResultStatus
	if status == "" {
		status = "success"
	}
	_, err := dm.SQLDB().Exec(`
		INSERT INTO tool_invocations
		    (id, session_id, tool_name, action, invocation_id,
		     actor_kind, framework_name, payload_hash, result_status,
		     started_at, completed_at, duration_ms, error_message)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		args.ID, sessionID, args.Tool, args.Action, invocationID,
		args.ActorKind, args.FrameworkName, args.PayloadHash, status,
		startedAt, completedAt, completedAt-startedAt, args.ErrorMessage,
	)
	if err != nil {
		t.Fatalf("seedToolInvocation: %v", err)
	}
}

type seedArgs struct {
	ID            string
	Tool          string
	Action        string
	FrameworkName string
	ActorKind     string
	SessionID     string
	InvocationID  string
	PayloadHash   string
	ResultStatus  string
	ErrorMessage  string
	StartedAt     int64
	CompletedAt   int64
}

// TestRecentActivity_CrossSurface (#25): in a disposable DB,
// seed semantic writes across all six families, query
// recent_activity, assert all six appear.
func TestRecentActivity_CrossSurface(t *testing.T) {
	dm := NewTestSharedDM(t)
	now := time.Now().Unix()
	seeds := []seedArgs{
		{ID: "t-mem", Tool: "mpm_memory", Action: "save", FrameworkName: "openclaw", ActorKind: "human", StartedAt: now, CompletedAt: now + 1, InvocationID: "inv-mem"},
		{ID: "t-les", Tool: "mpm_lessons", Action: "save", FrameworkName: "pi", ActorKind: "human", StartedAt: now + 1, CompletedAt: now + 2, InvocationID: "inv-les"},
		{ID: "t-ho", Tool: "mpm_handoff", Action: "write", FrameworkName: "claude-code", ActorKind: "human", StartedAt: now + 2, CompletedAt: now + 3, InvocationID: "inv-ho"},
		{ID: "t-sp", Tool: "mpm_scratchpad", Action: "flush", FrameworkName: "opencode", ActorKind: "human", StartedAt: now + 3, CompletedAt: now + 4, InvocationID: "inv-sp"},
		{ID: "t-wk", Tool: "mpm_work", Action: "create", FrameworkName: "openclaw", ActorKind: "human", StartedAt: now + 4, CompletedAt: now + 5, InvocationID: "inv-wk"},
		{ID: "t-wa", Tool: "mpm_wakes", Action: "schedule", FrameworkName: "mcp", ActorKind: "agent", StartedAt: now + 5, CompletedAt: now + 6, InvocationID: "inv-wa"},
	}
	for _, s := range seeds {
		seedToolInvocation(t, dm, s)
	}
	events, err := dm.RecentActivity(RecentActivityQueryParams{Limit: 50})
	if err != nil {
		t.Fatalf("RecentActivity: %v", err)
	}
	got := map[string]bool{}
	for _, ev := range events {
		key := ev.Tool + "/" + ev.Action
		got[key] = true
	}
	wantKeys := []string{
		"mpm_memory/save",
		"mpm_lessons/save",
		"mpm_handoff/write",
		"mpm_scratchpad/flush",
		"mpm_work/create",
		"mpm_wakes/schedule",
	}
	for _, k := range wantKeys {
		if !got[k] {
			t.Errorf("missing cross-surface activity for %q (got %d events)", k, len(events))
		}
	}
}

// TestRecentActivity_ScratchpadHistory (#26): scratchpad activity
// must remain visible after the ephemeral_scratchpad row disappears.
// We seed the tool_invocations row directly with no current
// scratchpad row present and assert the activity is visible.
func TestRecentActivity_ScratchpadHistory(t *testing.T) {
	dm := NewTestSharedDM(t)
	now := time.Now().Unix()
	seedToolInvocation(t, dm, seedArgs{
		ID: "t-sp-discard", Tool: "mpm_scratchpad", Action: "discard",
		FrameworkName: "openclaw", ActorKind: "human",
		StartedAt: now, CompletedAt: now + 1,
		InvocationID: "inv-sp-discard",
	})
	seedToolInvocation(t, dm, seedArgs{
		ID: "t-sp-promote", Tool: "mpm_scratchpad", Action: "promote",
		FrameworkName: "openclaw", ActorKind: "human",
		StartedAt: now + 1, CompletedAt: now + 2,
		InvocationID: "inv-sp-promote",
	})
	events, err := dm.RecentActivity(RecentActivityQueryParams{
		Limit: 50, ArtifactType: "mpm_scratchpad",
	})
	if err != nil {
		t.Fatalf("RecentActivity: %v", err)
	}
	if len(events) < 2 {
		t.Fatalf("expected >=2 scratchpad events, got %d", len(events))
	}
	// The scratchpad rows themselves are not in ephemeral_scratchpad;
	// the tool_invocations rows preserve the history.
	row := dm.SQLDB().QueryRow(`SELECT COUNT(*) FROM ephemeral_scratchpad`)
	var n int
	if err := row.Scan(&n); err != nil {
		t.Fatalf("count ephemeral_scratchpad: %v", err)
	}
	if n != 0 {
		t.Fatalf("ephemeral_scratchpad should be empty (got %d) — but recent_activity should still surface the historical actions", n)
	}
}

// TestRecentActivity_WorkEnrichment (#27): create/update/complete
// a work item; assert work_events join yields deterministic
// artifact_id and event_type in enrichment.
func TestRecentActivity_WorkEnrichment(t *testing.T) {
	dm := NewTestSharedDM(t)
	now := time.Now().Unix()

	// Seed a work_events row tied to the invocation_id.
	_, err := dm.SQLDB().Exec(`
		INSERT INTO work_events (id, work_id, event_index, event_type, created_at, invocation_id)
		VALUES (?, ?, ?, ?, ?, ?)`,
		"we-1", "w-test", 1, "created", now, "inv-wk-create",
	)
	if err != nil {
		t.Fatalf("insert work_events: %v", err)
	}
	seedToolInvocation(t, dm, seedArgs{
		ID: "t-wk-create", Tool: "mpm_work", Action: "create",
		FrameworkName: "openclaw", ActorKind: "human",
		StartedAt: now, CompletedAt: now + 1,
		InvocationID: "inv-wk-create",
	})

	events, err := dm.RecentActivity(RecentActivityQueryParams{
		Limit: 50, ArtifactType: "mpm_work",
	})
	if err != nil {
		t.Fatalf("RecentActivity: %v", err)
	}
	if len(events) != 1 {
		t.Fatalf("expected 1 work event, got %d", len(events))
	}
	ev := events[0]
	if ev.ArtifactID != "w-test" {
		t.Errorf("ArtifactID = %q, want w-test", ev.ArtifactID)
	}
	if !strings.Contains(ev.EnrichmentHint, "work_event:created") {
		t.Errorf("EnrichmentHint = %q, want work_event:created", ev.EnrichmentHint)
	}
}

// TestRecentActivity_HistoricalFrameworkNormalization (#28):
// seed rows with the historical bug pattern (raw actor_kind=human +
// framework_name=openclaw/pi/opencode/claude-code). Assert that
// the recent_activity output reports them with effective
// actor_kind=agent.
func TestRecentActivity_HistoricalFrameworkNormalization(t *testing.T) {
	dm := NewTestSharedDM(t)
	now := time.Now().Unix()
	frameworks := []string{"openclaw", "pi", "opencode", "claude-code"}
	for i, fwk := range frameworks {
		seedToolInvocation(t, dm, seedArgs{
			ID: "t-hist-" + fwk, Tool: "mpm_memory", Action: "save",
			FrameworkName: fwk, ActorKind: "human",
			StartedAt: now + int64(i), CompletedAt: now + int64(i) + 1,
			InvocationID: "inv-hist-" + fwk,
		})
	}
	events, err := dm.RecentActivity(RecentActivityQueryParams{Limit: 50})
	if err != nil {
		t.Fatalf("RecentActivity: %v", err)
	}
	if len(events) != len(frameworks) {
		t.Fatalf("expected %d events, got %d", len(frameworks), len(events))
	}
	for _, ev := range events {
		if ev.ActorKind != ActorKindAgent {
			t.Errorf("framework=%s raw=human should normalize to actor=agent, got %q",
				ev.FrameworkName, ev.ActorKind)
		}
	}
}

// TestRecentActivity_HumanCLI (#29): framework_name=mpm-cli +
// actor_kind=human must remain human.
func TestRecentActivity_HumanCLI(t *testing.T) {
	dm := NewTestSharedDM(t)
	now := time.Now().Unix()
	seedToolInvocation(t, dm, seedArgs{
		ID: "t-cli-1", Tool: "mpm_memory", Action: "save",
		FrameworkName: "mpm-cli", ActorKind: "human",
		StartedAt: now, CompletedAt: now + 1,
		InvocationID: "inv-cli-1",
	})
	events, err := dm.RecentActivity(RecentActivityQueryParams{Limit: 50})
	if err != nil {
		t.Fatalf("RecentActivity: %v", err)
	}
	if len(events) != 1 {
		t.Fatalf("expected 1 event, got %d", len(events))
	}
	if events[0].ActorKind != ActorKindHuman {
		t.Errorf("mpm-cli + human should remain human, got %q", events[0].ActorKind)
	}
}

// TestRecentActivity_SecretRedaction (#30): summaries must not
// contain payload-derived strings or sentinel secret tokens.
func TestRecentActivity_SecretRedaction(t *testing.T) {
	dm := NewTestSharedDM(t)
	const sentinel = "MPM_ACTIVITY_SECRET_MUST_NOT_LEAK_alpha"
	now := time.Now().Unix()
	// The payload_hash column gets the sentinel so any leakage
	// via payload-derived output would surface here.
	seedToolInvocation(t, dm, seedArgs{
		ID: "t-sec", Tool: "mpm_memory", Action: "save",
		FrameworkName: "openclaw", ActorKind: "human",
		StartedAt: now, CompletedAt: now + 1,
		InvocationID: "inv-sec", PayloadHash: sentinel,
	})
	events, err := dm.RecentActivity(RecentActivityQueryParams{Limit: 50})
	if err != nil {
		t.Fatalf("RecentActivity: %v", err)
	}
	for _, ev := range events {
		b, _ := json.Marshal(ev)
		if strings.Contains(string(b), sentinel) {
			t.Fatalf("recent_activity event leaks sentinel:\n%s", string(b))
		}
		if strings.Contains(ev.Summary, sentinel) {
			t.Fatalf("summary leaks sentinel: %q", ev.Summary)
		}
	}
}

// TestRecentActivity_FailedInvocationExcluded (#31): failed
// (result_status=error) invocations are excluded by default.
//
// include_system is NOT the opt-in — it has been a strict no-op for
// several arcs and does not affect this filter. The actual opt-in is
// RecentActivityQueryParams.ResultStatus; see
// recent_activity_result_status_test.go. This test pins the default
// half of that contract.
func TestRecentActivity_FailedInvocationExcluded(t *testing.T) {
	dm := NewTestSharedDM(t)
	now := time.Now().Unix()
	seedToolInvocation(t, dm, seedArgs{
		ID: "t-ok", Tool: "mpm_memory", Action: "save",
		FrameworkName: "openclaw", ActorKind: "human",
		StartedAt: now, CompletedAt: now + 1,
		InvocationID: "inv-ok", ResultStatus: "success",
	})
	seedToolInvocation(t, dm, seedArgs{
		ID: "t-err", Tool: "mpm_memory", Action: "save",
		FrameworkName: "openclaw", ActorKind: "human",
		StartedAt: now + 1, CompletedAt: now + 2,
		InvocationID: "inv-err", ResultStatus: "error",
		ErrorMessage: "synthetic failure",
	})
	events, err := dm.RecentActivity(RecentActivityQueryParams{Limit: 50})
	if err != nil {
		t.Fatalf("RecentActivity: %v", err)
	}
	// Only the successful one should be visible. The failed one
	// has the same action class (mutating) but is excluded by
	// status filter — see recent_activity.go implementation.
	if len(events) != 1 {
		t.Fatalf("expected 1 successful event, got %d", len(events))
	}
	if events[0].Status != "success" {
		t.Errorf("status = %q, want success", events[0].Status)
	}
}

// TestRecentActivity_OrderingAndFilters (#32): newest-first,
// deterministic tie-break, since filter, actor_kind filter,
// framework filter.
func TestRecentActivity_OrderingAndFilters(t *testing.T) {
	dm := NewTestSharedDM(t)
	now := time.Now().Unix()
	for i := 0; i < 5; i++ {
		seedToolInvocation(t, dm, seedArgs{
			ID: "t-ord-" + string(rune('a'+i)),
			Tool: "mpm_memory", Action: "save",
			FrameworkName: "openclaw", ActorKind: "human",
			StartedAt: now + int64(i), CompletedAt: now + int64(i) + 1,
			InvocationID: "inv-ord-" + string(rune('a'+i)),
		})
	}
	seedToolInvocation(t, dm, seedArgs{
		ID: "t-cli-extra", Tool: "mpm_lessons", Action: "save",
		FrameworkName: "mpm-cli", ActorKind: "human",
		StartedAt: now + 10, CompletedAt: now + 11,
		InvocationID: "inv-cli-extra",
	})

	// newest-first ordering
	events, err := dm.RecentActivity(RecentActivityQueryParams{Limit: 50})
	if err != nil {
		t.Fatalf("RecentActivity: %v", err)
	}
	for i := 1; i < len(events); i++ {
		if events[i].Timestamp > events[i-1].Timestamp {
			t.Errorf("not newest-first at index %d: %d > %d", i, events[i].Timestamp, events[i-1].Timestamp)
		}
	}

	// limit enforcement
	events, err = dm.RecentActivity(RecentActivityQueryParams{Limit: 2})
	if err != nil {
		t.Fatalf("RecentActivity: %v", err)
	}
	if len(events) != 2 {
		t.Errorf("limit=2 returned %d events", len(events))
	}

	// hard max
	events, err = dm.RecentActivity(RecentActivityQueryParams{Limit: 9999})
	if err != nil {
		t.Fatalf("RecentActivity: %v", err)
	}
	if len(events) > RecentActivityHardMaxLimit {
		t.Errorf("hard max breached: %d > %d", len(events), RecentActivityHardMaxLimit)
	}

	// framework filter
	events, err = dm.RecentActivity(RecentActivityQueryParams{
		Limit: 50, FrameworkName: "mpm-cli",
	})
	if err != nil {
		t.Fatalf("RecentActivity: %v", err)
	}
	if len(events) != 1 {
		t.Errorf("framework filter returned %d events, want 1", len(events))
	}

	// since filter
	events, err = dm.RecentActivity(RecentActivityQueryParams{
		Limit: 50, Since: now + 5,
	})
	if err != nil {
		t.Fatalf("RecentActivity: %v", err)
	}
	for _, ev := range events {
		if ev.Timestamp < now+5 {
			t.Errorf("since filter passed event with timestamp %d", ev.Timestamp)
		}
	}

	// actor_kind filter
	events, err = dm.RecentActivity(RecentActivityQueryParams{
		Limit: 50, ActorKind: "human",
	})
	if err != nil {
		t.Fatalf("RecentActivity: %v", err)
	}
	if len(events) == 0 {
		t.Errorf("actor_kind=human returned 0 events")
	}
}

// TestRecentActivity_ObserverEffect (#33): calling recent_activity
// must not itself appear as semantic activity. The current
// implementation does not write to tool_invocations from
// (dm *DatabaseManager).RecentActivity, so calling it twice must
// not double-count or include itself.
func TestRecentActivity_ObserverEffect(t *testing.T) {
	dm := NewTestSharedDM(t)
	now := time.Now().Unix()
	seedToolInvocation(t, dm, seedArgs{
		ID: "t-pre", Tool: "mpm_memory", Action: "save",
		FrameworkName: "openclaw", ActorKind: "human",
		StartedAt: now, CompletedAt: now + 1,
		InvocationID: "inv-pre",
	})

	before, err := dm.RecentActivity(RecentActivityQueryParams{Limit: 50})
	if err != nil {
		t.Fatalf("RecentActivity before: %v", err)
	}
	after, err := dm.RecentActivity(RecentActivityQueryParams{Limit: 50})
	if err != nil {
		t.Fatalf("RecentActivity after: %v", err)
	}
	if len(after) != len(before) {
		t.Errorf("observer effect: before=%d, after=%d", len(before), len(after))
	}
	for _, ev := range after {
		if ev.Tool == "mpm_context" && ev.Action == "recent_activity" {
			t.Errorf("recent_activity must not appear in its own output: %+v", ev)
		}
	}
}

// TestRecentActivity_WakeContextIntegration (#34): seed writes
// across all six families, call gatherWakeContext, assert the
// recent_activity section contains all expected actions; assert
// existing specialized sections still present; assert handoff
// consume semantics are unchanged.
func TestRecentActivity_WakeContextIntegration(t *testing.T) {
	dm := NewTestSharedDM(t)
	now := time.Now().Unix()

	// Seed a handoff so we can assert wake-context consume semantics.
	h, err := dm.EndSession("test-sess", "wake-ctx integration test", HandoffClean, []string{}, []string{})
	if err != nil {
		t.Fatalf("EndSession: %v", err)
	}

	seeds := []seedArgs{
		{ID: "t-mem", Tool: "mpm_memory", Action: "save", FrameworkName: "openclaw", ActorKind: "human", StartedAt: now, CompletedAt: now + 1, InvocationID: "inv-mem"},
		{ID: "t-les", Tool: "mpm_lessons", Action: "save", FrameworkName: "pi", ActorKind: "human", StartedAt: now + 1, CompletedAt: now + 2, InvocationID: "inv-les"},
		{ID: "t-ho", Tool: "mpm_handoff", Action: "write", FrameworkName: "claude-code", ActorKind: "human", StartedAt: now + 2, CompletedAt: now + 3, InvocationID: "inv-ho2"},
		{ID: "t-sp", Tool: "mpm_scratchpad", Action: "flush", FrameworkName: "opencode", ActorKind: "human", StartedAt: now + 3, CompletedAt: now + 4, InvocationID: "inv-sp"},
		{ID: "t-wk", Tool: "mpm_work", Action: "create", FrameworkName: "openclaw", ActorKind: "human", StartedAt: now + 4, CompletedAt: now + 5, InvocationID: "inv-wk"},
		{ID: "t-wa", Tool: "mpm_wakes", Action: "schedule", FrameworkName: "mcp", ActorKind: "agent", StartedAt: now + 5, CompletedAt: now + 6, InvocationID: "inv-wa"},
	}
	for _, s := range seeds {
		seedToolInvocation(t, dm, s)
	}

	// Gather wake context in consume mode.
	data, err := dm.GatherWakeContext()
	if err != nil {
		t.Fatalf("GatherWakeContext: %v", err)
	}

	// existing sections preserved
	if data.LastHandoff == nil || data.LastHandoff.ID != h.ID {
		t.Errorf("LastHandoff missing or wrong; want id=%s got %v", h.ID, data.LastHandoff)
	}
	if data.LastHandoff != nil && data.LastHandoff.ReadBy != "wake-context" {
		t.Errorf("LastHandoff.ReadBy = %q, want wake-context (consume semantics preserved)",
			data.LastHandoff.ReadBy)
	}

	// new section present
	if len(data.RecentActivity) == 0 {
		t.Fatalf("RecentActivity section empty in wake context")
	}
	tools := map[string]bool{}
	for _, ev := range data.RecentActivity {
		tools[ev.Tool+"/"+ev.Action] = true
	}
	wantKeys := []string{
		"mpm_memory/save",
		"mpm_lessons/save",
		"mpm_handoff/write",
		"mpm_scratchpad/flush",
		"mpm_work/create",
		"mpm_wakes/schedule",
	}
	for _, k := range wantKeys {
		if !tools[k] {
			t.Errorf("RecentActivity missing %q in wake context", k)
		}
	}

	// Truncation flag default = false
	if data.RecentActivityTruncated {
		t.Errorf("RecentActivityTruncated = true unexpectedly")
	}
}

// TestRecentActivity_LimitsEnforced pins the canonical defaults.
func TestRecentActivity_LimitsEnforced(t *testing.T) {
	dm := NewTestSharedDM(t)
	now := time.Now().Unix()
	for i := 0; i < 50; i++ {
		seedToolInvocation(t, dm, seedArgs{
			ID: "t-bulk-" + string(rune('a'+i%26)) + string(rune('a'+(i/26)%26)),
			Tool: "mpm_memory", Action: "save",
			FrameworkName: "openclaw", ActorKind: "human",
			StartedAt: now + int64(i), CompletedAt: now + int64(i) + 1,
			InvocationID: "inv-bulk-" + string(rune('a'+i%26)) + string(rune('a'+(i/26)%26)),
		})
	}
	// default limit
	events, err := dm.RecentActivity(RecentActivityQueryParams{})
	if err != nil {
		t.Fatalf("RecentActivity: %v", err)
	}
	if len(events) != RecentActivityDefaultLimit {
		t.Errorf("default limit = %d, want %d", len(events), RecentActivityDefaultLimit)
	}

	// hard max caps huge requests
	events, err = dm.RecentActivity(RecentActivityQueryParams{Limit: 100000})
	if err != nil {
		t.Fatalf("RecentActivity: %v", err)
	}
	if len(events) > RecentActivityHardMaxLimit {
		t.Errorf("hard max breached: %d > %d", len(events), RecentActivityHardMaxLimit)
	}
}

// ensure imports stay used
var _ = context.Background
var _ sql.NullString