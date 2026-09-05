// limit_strict_regression_test.go — 2026-09-05 audit remediation pass 2.
//
// Audit found three handlers that silently coerced non-positive `limit`
// values rather than enforcing the documented parseLimitStrict contract
// (omitted → default, 0 → 0 literally, negative → error, non-integer →
// error):
//
//   - mpm_memory.query  — uses parseLimitStrict at handler level, BUT
//     the deeper HybridSearch (hybrid_search.go:110) still has
//     `if cfg.Limit <= 0 { cfg.Limit = 15 }` which overrides the
//     validated limit back to 15. hybridSearchScopeAll also has
//     `if fetch < 10 { fetch = 10 }`.
//   - mpm_decisions.list / .query  — handler does not call
//     parseLimitStrict; DM (epistemology_tools.go:749,866) silently
//     coerces `limit <= 0` to 50.
//   - mpm_theories.list / .query   — same pattern as decisions.
//
// Pre-fix reproductions (live CLI probes):
//
//   $ mpm call mpm_memory query --payload '{"action":"query","params":{"query":"test","limit":0,"scope":"local"}}'
//     count=15 items=0                # wanted: 0
//   $ mpm call mpm_decisions list --payload '{"action":"list","params":{"limit":0}}'
//     count=50                        # wanted: 0
//   $ mpm call mpm_theories list --payload '{"action":"list","params":{"limit":0}}'
//     count=50                        # wanted: 0
//
// Canonical contract (already in place for mpm_memory.query via
// parseLimitStrict at handlers.go:5792):
//
//   omitted  → default (5 for query; 50 for list/query surfaces
//              that the audit explicitly classified as "coerce to 50"
//              before the fix — see DecisionFilter and TheoryFilter
//              struct definitions for the documented default page
//              size)
//   0        → 0, literally "no results"
//   1        → 1
//   -1       → error: limit must be >= 0
//   -10      → error: limit must be >= 0
//   non-integer (e.g. 1.5, "abc") → error: limit must be an integer
//
// Fix scope: apply parseLimitStrict at the public handler boundary
// for every affected surface, and remove the deeper silent-coercion
// sites in HybridSearch / hybridSearchScopeAll / DM list/query
// methods that override the validated limit.

package tools

import (
	"fmt"
	"strings"
	"testing"

	"github.com/flowbyte-com/mpm-core"
)

// TestMemoryQuery_LimitZeroReturnsZero pins the canonical contract:
// limit=0 means "no results", not "default page size".
func TestMemoryQuery_LimitZeroReturnsZero(t *testing.T) {
	dm := newTestSharedDM(t)
	seedMemories(t, dm, "limit-zero probe", 5)

	res, err := handleMpmMemory(dm, defaultACForPatch(), map[string]interface{}{
		"action": "query",
		"params": map[string]interface{}{
			"query": "limit-zero probe",
			"limit": float64(0),
			"scope": "local",
		},
	})
	if err != nil {
		t.Fatalf("query with limit=0: %v", err)
	}
	m, ok := res.(map[string]interface{})
	if !ok {
		t.Fatalf("expected map result, got %T", res)
	}
	items, _ := m["items"].([]map[string]interface{})
	if len(items) != 0 {
		t.Errorf("limit=0 must return 0 items; got %d", len(items))
	}
	if c, _ := m["count"].(int); c != 0 {
		t.Errorf("count = %d, want 0", c)
	}
}

// TestMemoryQuery_LimitOneReturnsOne pins the boundary: 1 → 1.
func TestMemoryQuery_LimitOneReturnsOne(t *testing.T) {
	dm := newTestSharedDM(t)
	seedMemories(t, dm, "limit-one probe", 5)

	res, err := handleMpmMemory(dm, defaultACForPatch(), map[string]interface{}{
		"action": "query",
		"params": map[string]interface{}{
			"query": "limit-one probe",
			"limit": float64(1),
			"scope": "local",
		},
	})
	if err != nil {
		t.Fatalf("query with limit=1: %v", err)
	}
	m := res.(map[string]interface{})
	items, _ := m["items"].([]map[string]interface{})
	if len(items) > 1 {
		t.Errorf("limit=1 must return at most 1 item; got %d", len(items))
	}
}

// TestMemoryQuery_LimitNegativeErrors pins: negative limits error
// at the public boundary, before persistence.
func TestMemoryQuery_LimitNegativeErrors(t *testing.T) {
	for _, lim := range []float64{-1, -10, -1000} {
		t.Run(fmt.Sprintf("limit=%v", lim), func(t *testing.T) {
			dm := newTestSharedDM(t)
			_, err := handleMpmMemory(dm, defaultACForPatch(), map[string]interface{}{
				"action": "query",
				"params": map[string]interface{}{
					"query": "neg probe",
					"limit": lim,
					"scope": "local",
				},
			})
			if err == nil {
				t.Fatalf("limit=%v must error", lim)
			}
			if !strings.Contains(err.Error(), "limit") {
				t.Errorf("error must mention 'limit', got: %v", err)
			}
		})
	}
}

// TestMemoryQuery_LimitNonIntegerErrors pins: non-integer limits
// error rather than being silently floored/truncated.
func TestMemoryQuery_LimitNonIntegerErrors(t *testing.T) {
	for _, lim := range []float64{1.5, 2.7, -0.5} {
		t.Run(fmt.Sprintf("limit=%v", lim), func(t *testing.T) {
			dm := newTestSharedDM(t)
			_, err := handleMpmMemory(dm, defaultACForPatch(), map[string]interface{}{
				"action": "query",
				"params": map[string]interface{}{
					"query": "frac probe",
					"limit": lim,
					"scope": "local",
				},
			})
			if err == nil {
				t.Fatalf("limit=%v must error", lim)
			}
			if !strings.Contains(err.Error(), "integer") {
				t.Errorf("error must mention 'integer', got: %v", err)
			}
		})
	}
}

// TestDecisionsList_LimitZeroReturnsZero pins the canonical contract
// for mpm_decisions.list. Pre-fix the DM silently returned 50.
func TestDecisionsList_LimitZeroReturnsZero(t *testing.T) {
	dm := newTestSharedDM(t)
	seedDecisions(t, dm, "decision-limit-zero probe", 3)

	res, err := handleMpmDecisions(dm, defaultACForPatch(), map[string]interface{}{
		"action": "list",
		"params": map[string]interface{}{
			"limit": float64(0),
		},
	})
	if err != nil {
		t.Fatalf("decisions list with limit=0: %v", err)
	}
	m := res.(map[string]interface{})
	if c, _ := m["count"].(int); c != 0 {
		t.Errorf("decisions list limit=0 count = %d, want 0", c)
	}
}

// TestDecisionsList_LimitNegativeErrors pins: negative limits error.
func TestDecisionsList_LimitNegativeErrors(t *testing.T) {
	dm := newTestSharedDM(t)
	_, err := handleMpmDecisions(dm, defaultACForPatch(), map[string]interface{}{
		"action": "list",
		"params": map[string]interface{}{
			"limit": float64(-1),
		},
	})
	if err == nil {
		t.Fatalf("limit=-1 must error")
	}
	if !strings.Contains(err.Error(), "limit") {
		t.Errorf("error must mention 'limit', got: %v", err)
	}
}

// TestDecisionsQuery_LimitZeroReturnsZero pins the canonical contract
// for mpm_decisions.query.
func TestDecisionsQuery_LimitZeroReturnsZero(t *testing.T) {
	dm := newTestSharedDM(t)
	seedDecisions(t, dm, "decision-query-zero probe", 3)

	res, err := handleMpmDecisions(dm, defaultACForPatch(), map[string]interface{}{
		"action": "query",
		"params": map[string]interface{}{
			"query": "decision-query-zero probe",
			"limit": float64(0),
		},
	})
	if err != nil {
		t.Fatalf("decisions query with limit=0: %v", err)
	}
	m := res.(map[string]interface{})
	if c, _ := m["count"].(int); c != 0 {
		t.Errorf("decisions query limit=0 count = %d, want 0", c)
	}
}

// TestTheoriesList_LimitZeroReturnsZero pins the canonical contract
// for mpm_theories.list.
func TestTheoriesList_LimitZeroReturnsZero(t *testing.T) {
	dm := newTestSharedDM(t)
	seedTheories(t, dm, "theory-limit-zero probe", 3)

	res, err := handleMpmTheories(dm, defaultACForPatch(), map[string]interface{}{
		"action": "list",
		"params": map[string]interface{}{
			"limit":  float64(0),
			"status": "all",
		},
	})
	if err != nil {
		t.Fatalf("theories list with limit=0: %v", err)
	}
	m := res.(map[string]interface{})
	if c, _ := m["count"].(int); c != 0 {
		t.Errorf("theories list limit=0 count = %d, want 0", c)
	}
}

// TestTheoriesList_LimitZeroReturnsZero_BoundedSanity pins that
// status="all" actually surfaces the seeded theories, so the limit=0
// path above is meaningfully exercised (not coincidentally empty due
// to the default status="pending" filter).
func TestTheoriesList_LimitZeroReturnsZero_BoundedSanity(t *testing.T) {
	dm := newTestSharedDM(t)
	seedTheories(t, dm, "theory-sanity probe", 3)

	res, err := handleMpmTheories(dm, defaultACForPatch(), map[string]interface{}{
		"action": "list",
		"params": map[string]interface{}{
			"limit":  float64(100),
			"status": "all",
		},
	})
	if err != nil {
		t.Fatalf("theories list with limit=100: %v", err)
	}
	m := res.(map[string]interface{})
	if c, _ := m["count"].(int); c < 3 {
		t.Errorf("sanity: theories list limit=100 status=all must include seeded rows, got count=%d", c)
	}
}

// TestTheoriesQuery_LimitZeroReturnsZero pins the canonical contract
// for mpm_theories.query.
func TestTheoriesQuery_LimitZeroReturnsZero(t *testing.T) {
	dm := newTestSharedDM(t)
	seedTheories(t, dm, "theory-query-zero probe", 3)

	res, err := handleMpmTheories(dm, defaultACForPatch(), map[string]interface{}{
		"action": "query",
		"params": map[string]interface{}{
			"query": "theory-query-zero probe",
			"limit": float64(0),
		},
	})
	if err != nil {
		t.Fatalf("theories query with limit=0: %v", err)
	}
	m := res.(map[string]interface{})
	if c, _ := m["count"].(int); c != 0 {
		t.Errorf("theories query limit=0 count = %d, want 0", c)
	}
}

// TestTheoriesList_LimitNegativeErrors pins: negative limits error
// for theories too.
func TestTheoriesList_LimitNegativeErrors(t *testing.T) {
	dm := newTestSharedDM(t)
	_, err := handleMpmTheories(dm, defaultACForPatch(), map[string]interface{}{
		"action": "list",
		"params": map[string]interface{}{
			"limit":  float64(-10),
			"status": "all",
		},
	})
	if err == nil {
		t.Fatalf("theories list limit=-10 must error")
	}
}

// --- helpers ---

// seedMemories inserts n memories into the memories table with
// collection='memories' for limit-test queries.
func seedMemories(t *testing.T, dm internal.CoreDB, fact string, n int) {
	t.Helper()
	for i := 0; i < n; i++ {
		id := fmt.Sprintf("mem-seed-%s-%d", strings.ReplaceAll(fact, " ", "_"), i)
		_, err := dm.SQLDB().Exec(`
			INSERT INTO memories (id, collection, content, weight, tags, metadata, deleted_at, created_at, updated_at)
			VALUES (?, 'memories', ?, 5, '[]', '{}', NULL, CAST(strftime('%s','now') AS INTEGER), CAST(strftime('%s','now') AS INTEGER))
		`, id, fact)
		if err != nil {
			t.Fatalf("seed memory %s: %v", id, err)
		}
	}
}

// seedDecisions inserts n decision memories for limit-test queries.
func seedDecisions(t *testing.T, dm internal.CoreDB, fact string, n int) {
	t.Helper()
	for i := 0; i < n; i++ {
		id := fmt.Sprintf("dec-seed-%s-%d", strings.ReplaceAll(fact, " ", "_"), i)
		_, err := dm.SQLDB().Exec(`
			INSERT INTO memories (id, collection, content, weight, tags, metadata, deleted_at, created_at, updated_at)
			VALUES (?, 'decisions', ?, 5, '[]', '{}', NULL, CAST(strftime('%s','now') AS INTEGER), CAST(strftime('%s','now') AS INTEGER))
		`, id, fact)
		if err != nil {
			t.Fatalf("seed decision %s: %v", id, err)
		}
	}
}

// seedTheories inserts n theory memories for limit-test queries.
func seedTheories(t *testing.T, dm internal.CoreDB, fact string, n int) {
	t.Helper()
	for i := 0; i < n; i++ {
		id := fmt.Sprintf("thy-seed-%s-%d", strings.ReplaceAll(fact, " ", "_"), i)
		_, err := dm.SQLDB().Exec(`
			INSERT INTO memories (id, collection, content, weight, tags, metadata, deleted_at, created_at, updated_at)
			VALUES (?, 'theories', ?, 5, '[]', '{}', NULL, CAST(strftime('%s','now') AS INTEGER), CAST(strftime('%s','now') AS INTEGER))
		`, id, fact)
		if err != nil {
			t.Fatalf("seed theory %s: %v", id, err)
		}
	}
}
