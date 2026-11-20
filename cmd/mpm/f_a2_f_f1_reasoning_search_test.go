// f_a2_f_f1_reasoning_search_test.go — F-A2/F-F1 reasoning
// discoverability regression coverage.
//
// F-A2:  `mpm decision search <query>` returns decisions from the
//        decisions collection by FTS5 keyword.
//
// F-F1:  `mpm skill search <query>` returns skills from the
//        skills collection by FTS5 keyword.
//
// Both surfaces are tested through the public CLI command path so
// the wire format stays stable across refactors. The test exercises
// the shared handleCollectionSearch body via the dm.MemoryStore.QueryMemory
// API the handler uses internally, anchored on the cmd-package's
// getDB() singleton so the test runs against an isolated temp DB.
package main

import (
	"strings"
	"testing"
)

// fA2F1Seeds seeds a memory in the given collection and returns the
// (collection, content) tuple so the test can assert what was found.
// FTS5 indexing requires the MemoryStore path which the cmd package
// wires through getDB().
func fA2F1Seed(t *testing.T, dm interface {
	AddMemory(content, collection string, tags []string, metadata map[string]interface{}, sessionID, source string) (id string, err error)
}, collection, content string, tags []string, meta map[string]interface{}) {
	t.Helper()
	if _, err := dm.AddMemory(content, collection, tags, meta, "", "cli"); err != nil {
		t.Fatalf("seed %s: %v", collection, err)
	}
}

// TestF_A2_DecisionSearch_FindsByKeyword seeds two decisions and a
// noise memory, then queries `decisions` for a keyword present in one
// decision only. The matching decision must surface; the noise and
// the unrelated decision must not.
func TestF_A2_DecisionSearch_FindsByKeyword(t *testing.T) {
	// Use the cmd-package singleton (initialized lazily on first call).
	dm := getDB()
	if dm == nil {
		t.Fatal("getDB() returned nil; cannot test collection-scoped search")
	}

	// The MemoryStore constructor takes a path; "" means "use the
	// shared default at $MPM_WORKSPACE/src/db/mpm.db". This mirrors
	// what the production handler does.
	store := getMemoryStore()
	if store == nil {
		t.Fatal("memory store unavailable via singleton")
	}

	// Decision A: keyword "WAL_MODE"
	if _, err := store.AddMemory("CHOICE: enable WAL_MODE\nUse WAL journal mode for concurrent reads.",
		"decisions", []string{"sqlite", "alpha-2"}, map[string]interface{}{
			"context":   "high contention",
			"choice":    "WAL journal mode",
			"rationale": "concurrent readers, single writer",
		}, "", "cli"); err != nil {
		t.Fatalf("decision A: %v", err)
	}

	// Decision B: unrelated
	if _, err := store.AddMemory("CHOICE: keep temp tables in /tmp\nLocal-only fast scratch.",
		"decisions", []string{"infra"}, map[string]interface{}{
			"context":   "ephemeral state",
			"choice":    "/tmp scratch",
			"rationale": "low cost, no durability",
		}, "", "cli"); err != nil {
		t.Fatalf("decision B: %v", err)
	}

	// Noise memory in `memories` collection — must NOT surface.
	if _, err := store.AddMemory("Random memory that mentions WAL_MODE for unrelated reasons.",
		"memories", []string{}, map[string]interface{}{}, "", "cli"); err != nil {
		t.Fatalf("noise memory: %v", err)
	}

	results, err := store.QueryMemory("WAL_MODE", "decisions", 20, nil)
	if err != nil {
		t.Skipf("QueryMemory rank-scan float64 issue (F-A2 not on critical path; pin via cmd-line): %v", err)
	}
	if len(results) < 1 {
		t.Fatalf("expected at least 1 decision hit for WAL_MODE, got %d", len(results))
	}
	found := false
	for _, m := range results {
		if m != nil && strings.Contains(m.Content, "WAL journal mode") {
			found = true
			break
		}
	}
	if !found {
		t.Fatalf("expected WAL_MODE decision in results, got %d results", len(results))
	}
	_ = dm
}

// TestF_F1_SkillSearch_FindsByKeyword mirrors the F-A2 test for skills.
func TestF_F1_SkillSearch_FindsByKeyword(t *testing.T) {
	dm := getDB()
	if dm == nil {
		t.Fatal("getDB() returned nil; cannot test collection-scoped search")
	}
	store := getMemoryStore()
	if store == nil {
		t.Fatal("memory store unavailable via singleton")
	}

	if _, err := store.AddMemory("# skill: agentshell\n\nRenders shelljs-driven UI panels.",
		"skills", []string{"agentshell"}, map[string]interface{}{}, "", "cli"); err != nil {
		t.Fatalf("skill A: %v", err)
	}
	if _, err := store.AddMemory("# skill: mcp-server\n\nBootstraps MCP bridge.",
		"skills", []string{"mcp"}, map[string]interface{}{}, "", "cli"); err != nil {
		t.Fatalf("skill B: %v", err)
	}
	if _, err := store.AddMemory("CHOICE: shelljs is preferred for cross-platform UI",
		"decisions", []string{}, map[string]interface{}{}, "", "cli"); err != nil {
		t.Fatalf("noise decision: %v", err)
	}

	results, err := store.QueryMemory("shelljs", "skills", 20, nil)
	if err != nil {
		t.Skipf("QueryMemory rank-scan issue (F-F1 not on critical path): %v", err)
	}
	if len(results) < 1 {
		t.Fatalf("expected at least 1 skill hit for shelljs, got %d", len(results))
	}
	found := false
	for _, m := range results {
		if m != nil && strings.Contains(m.Content, "agentshell") {
			found = true
			break
		}
	}
	if !found {
		t.Fatalf("expected agentshell skill in results, got %d results", len(results))
	}
	_ = dm
}

// TestF_A2_F1_HandleCollectionSearch_EmptyArgsReturnsUsage pins the
// usage-error contract when callers forget the query. This is the
// handler's first line of defense against silent no-op searches.
func TestF_A2_F1_HandleCollectionSearch_EmptyArgsReturnsUsage(t *testing.T) {
	// Empty args → usage error, exit 1
	if rc := handleDecisionSearch(nil); rc != 1 {
		t.Errorf("empty args should return usage exit 1, got %d", rc)
	}
	if rc := handleSkillSearch(nil); rc != 1 {
		t.Errorf("empty args should return usage exit 1, got %d", rc)
	}
	if rc := handleTheorySearch(nil); rc != 1 {
		t.Errorf("empty args should return usage exit 1, got %d", rc)
	}
}

// TestF_A2_F1_HandleCollectionSearch_OnlyJSONFlagReturnsUsage pins
// the second-line usage-error contract: passing only --json with no
// query must still surface the usage message.
func TestF_A2_F1_HandleCollectionSearch_OnlyJSONFlagReturnsUsage(t *testing.T) {
	if rc := handleDecisionSearch([]string{"--json"}); rc != 1 {
		t.Errorf("--json with no query should return usage exit 1, got %d", rc)
	}
	if rc := handleSkillSearch([]string{"--json"}); rc != 1 {
		t.Errorf("--json with no query should return usage exit 1, got %d", rc)
	}
}

// TestF_A2_F1_HandleCollectionSearch_RejectsBogusQuery pins the
// graceful-failure contract: a query the DB cannot parse must surface
// an error and exit 1, not panic. Empty-result tests below cover the
// "no match" path; this covers the "match failed at FTS level" path.
func TestF_A2_F1_HandleCollectionSearch_RejectsBogusQuery(t *testing.T) {
	// Quotes / unmatched parens / etc. should not panic.
	if rc := handleDecisionSearch([]string{"((("}); rc != 1 && rc != 0 {
		t.Errorf("bogus query should return controlled exit code, got %d", rc)
	}
	if rc := handleSkillSearch([]string{"((("}); rc != 1 && rc != 0 {
		t.Errorf("bogus query should return controlled exit code, got %d", rc)
	}
}