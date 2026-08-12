package internal

import (
	"strings"
	"testing"
)

// Refactor 2026-08-12: replaced direct NewDatabaseManager("") calls with
// NewTestSharedDM / NewTestLocalOnlyDM. The previous pattern set
// MPM_SHARED_DB to a per-test tmpfile but left the local DB pointed at
// config.GetMPMDir() — which under a normal `go test ./...` invocation
// defaults to ~/.mpm/src/db/mpm.db and pollutes the live production
// database with per-test fixtures. NewTestSharedDM / NewTestLocalOnlyDM
// set both MPM_WORKSPACE and MPM_SHARED_DB to t.TempDir()-rooted paths,
// severing the link to production state.

// TestWakeContext_IncludesGlobalRules verifies that when MPM_SHARED_DB
// is attached and the shared DB has is_global rows, those rows appear
// in the wake context payload. This is the Phase 2c feature — every
// agent on the workstation should see shared house rules on wake.
func TestWakeContext_IncludesGlobalRules(t *testing.T) {
	dm := NewTestSharedDM(t)

	// Insert a shared rule.
	_, err := dm.SQLDB().Exec(`
		INSERT INTO shared.memories
		    (id, collection, content, weight, reinforcement_count,
		     created_at, updated_at, deleted_at, is_global)
		VALUES ('rule-001', 'rules',
		        'Never expose API keys to the LLM context',
		        10, 0, '2026-06-26', '2026-06-26', NULL, 1)
	`)
	if err != nil {
		t.Fatalf("insert: %v", err)
	}

	ctx, err := dm.GatherWakeContext()
	if err != nil {
		t.Fatalf("GatherWakeContext: %v", err)
	}
	if len(ctx.GlobalRules) != 1 {
		t.Fatalf("expected 1 GlobalRule, got %d", len(ctx.GlobalRules))
	}
	if !strings.Contains(ctx.GlobalRules[0].Content, "API keys") {
		t.Errorf("unexpected rule content: %q", ctx.GlobalRules[0].Content)
	}
	if ctx.GlobalRules[0].Weight != 10 {
		t.Errorf("expected weight=10, got %d", ctx.GlobalRules[0].Weight)
	}
}

// TestWakeContext_NoGlobalRulesWhenSharedDBEmpty verifies that with
// MPM_SHARED_DB attached but no is_global rows, the wake context
// payload has empty GlobalRules (not nil-with-panic).
func TestWakeContext_NoGlobalRulesWhenSharedDBEmpty(t *testing.T) {
	dm := NewTestSharedDM(t)

	ctx, err := dm.GatherWakeContext()
	if err != nil {
		t.Fatalf("GatherWakeContext: %v", err)
	}
	if len(ctx.GlobalRules) != 0 {
		t.Errorf("expected empty GlobalRules, got %d", len(ctx.GlobalRules))
	}
}

// TestWakeContext_LocalOnlyOmitsGlobalRules verifies that without
// MPM_SHARED_DB, GlobalRules is empty (not populated, no error). The
// field's JSON tag is omitempty so it's omitted from the wire format
// entirely — agents don't see the field at all in local-only mode.
func TestWakeContext_LocalOnlyOmitsGlobalRules(t *testing.T) {
	dm := NewTestLocalOnlyDM(t)

	ctx, err := dm.GatherWakeContext()
	if err != nil {
		t.Fatalf("GatherWakeContext: %v", err)
	}
	if ctx.GlobalRules != nil {
		t.Errorf("expected nil GlobalRules in local-only mode, got %v", ctx.GlobalRules)
	}
}

// TestReadWakeContext_IncludesGlobalRulesSection verifies that the
// formatted wake context string includes a "Global Rules" header
// when shared rules exist. The agent reads this on wake and applies
// the rules to subsequent behavior.
func TestReadWakeContext_IncludesGlobalRulesSection(t *testing.T) {
	dm := NewTestSharedDM(t)

	_, err := dm.SQLDB().Exec(`
		INSERT INTO shared.memories
		    (id, collection, content, weight, reinforcement_count,
		     created_at, updated_at, deleted_at, is_global)
		VALUES ('rule-001', 'rules',
		        'Never expose API keys to the LLM context',
		        10, 0, '2026-06-26', '2026-06-26', NULL, 1)
	`)
	if err != nil {
		t.Fatalf("insert: %v", err)
	}

	out, err := dm.ReadWakeContext()
	if err != nil {
		t.Fatalf("ReadWakeContext: %v", err)
	}
	if !strings.Contains(out, "Global Rules") {
		t.Errorf("formatted wake context missing 'Global Rules' section:\n%s", out)
	}
	if !strings.Contains(out, "API keys") {
		t.Errorf("formatted wake context missing rule content:\n%s", out)
	}
	if !strings.Contains(out, "[w=10]") {
		t.Errorf("formatted wake context missing weight marker:\n%s", out)
	}
}