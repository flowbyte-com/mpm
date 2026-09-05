// memory_mutation_parsefloat_regression_test.go — 2026-09-05 audit
// remediation pass 2.
//
// Audit D-7 (P2) found mpm_memory mutation paths that use silent
// ParseFloatOr, allowing malformed caller input (string "5", empty
// string, malformed number) to silently become a valid mutation
// with a different value. For mutation APIs this is generally worse
// than rejecting the request — the audit trail records the wrong
// weight without flagging the contract violation.
//
// F12-1 already fixed the canonical mpm_memory.save.weight boundary
// (parseWeightStrict at handlers.go:119). This regression suite
// extends the same strict-type discipline to the other mpm_memory
// mutation paths:
//
//   - reinforce  (delta)   — handlers.go:1061
//   - weaken     (delta)   — handlers.go:1070
//   - snooze     (days)    — handlers.go:1079
//   - set_weight (weight)  — handlers.go:1088
//   - review     (days/limit) — handlers.go:1262
//
// Canonical contract:
//
//   omitted            → legitimate default (1 for reinforce/weaken/
//                        snooze delta/days; 0 for set_weight; 30
//                        for review days; 20 for review limit)
//   0                  → 0 (preserved; not coerced)
//   valid number       → parsed as int
//   string "5"         → ERROR (no silent coerce; the audit
//                        found this is the exact class the
//                        alpha-4.1.1 fix on memory.save.weight
//                        rejected)
//   string "abc"       → ERROR
//   ""                 → ERROR (empty is not a valid number)
//   whitespace         → ERROR
//
// Distinguishing omission from present-but-invalid input: present
// values are validated strictly; omitted values fall through to the
// legitimate default. This matches the F12-1 contract on save.weight.

package tools

import (
	"fmt"
	"strings"
	"testing"

	"github.com/flowbyte-com/mpm-core"
)

// --- set_weight ---

func TestSetWeight_ValidNumericPersists(t *testing.T) {
	dm := newTestSharedDM(t)
	seed := mustSeedMemoryForD2(t, dm, "set-weight valid probe")
	res, err := handleMpmMemory(dm, defaultACForPatch(), map[string]interface{}{
		"action": "set_weight",
		"params": map[string]interface{}{
			"memory_id": seed,
			"weight":    float64(7),
		},
	})
	if err != nil {
		t.Fatalf("set_weight with valid numeric: %v", err)
	}
	m := res.(map[string]interface{})
	if w, _ := m["weight"].(float64); w != 7 {
		t.Errorf("weight = %v, want 7", w)
	}
}

func TestSetWeight_StringFiveRejected(t *testing.T) {
	dm := newTestSharedDM(t)
	seed := mustSeedMemoryForD2(t, dm, "set-weight string-5 probe")
	_, err := handleMpmMemory(dm, defaultACForPatch(), map[string]interface{}{
		"action": "set_weight",
		"params": map[string]interface{}{
			"memory_id": seed,
			"weight":    "5",
		},
	})
	if err == nil {
		t.Fatalf("set_weight with weight='5' must error (F12-1 contract)")
	}
	if !strings.Contains(err.Error(), "weight") {
		t.Errorf("error must mention 'weight', got: %v", err)
	}
}

func TestSetWeight_StringAbcRejected(t *testing.T) {
	dm := newTestSharedDM(t)
	seed := mustSeedMemoryForD2(t, dm, "set-weight string-abc probe")
	_, err := handleMpmMemory(dm, defaultACForPatch(), map[string]interface{}{
		"action": "set_weight",
		"params": map[string]interface{}{
			"memory_id": seed,
			"weight":    "abc",
		},
	})
	if err == nil {
		t.Fatalf("set_weight with weight='abc' must error (F12-1 contract)")
	}
}

func TestSetWeight_EmptyStringRejected(t *testing.T) {
	dm := newTestSharedDM(t)
	seed := mustSeedMemoryForD2(t, dm, "set-weight empty-string probe")
	_, err := handleMpmMemory(dm, defaultACForPatch(), map[string]interface{}{
		"action": "set_weight",
		"params": map[string]interface{}{
			"memory_id": seed,
			"weight":    "",
		},
	})
	if err == nil {
		t.Fatalf("set_weight with weight='' must error (not silent default)")
	}
}

// --- snooze ---

func TestSnooze_StringDaysRejected(t *testing.T) {
	dm := newTestSharedDM(t)
	seed := mustSeedMemoryForD2(t, dm, "snooze string-days probe")
	_, err := handleMpmMemory(dm, defaultACForPatch(), map[string]interface{}{
		"action": "snooze",
		"params": map[string]interface{}{
			"memory_id": seed,
			"days":      "abc",
		},
	})
	if err == nil {
		t.Fatalf("snooze with days='abc' must error")
	}
	if !strings.Contains(err.Error(), "days") {
		t.Errorf("error must mention 'days', got: %v", err)
	}
}

func TestSnooze_NegativeDaysRejected(t *testing.T) {
	dm := newTestSharedDM(t)
	seed := mustSeedMemoryForD2(t, dm, "snooze negative-days probe")
	_, err := handleMpmMemory(dm, defaultACForPatch(), map[string]interface{}{
		"action": "snooze",
		"params": map[string]interface{}{
			"memory_id": seed,
			"days":      float64(-5),
		},
	})
	if err == nil {
		t.Fatalf("snooze with days=-5 must error (not silent default to 1)")
	}
}

// --- reinforce ---

func TestReinforce_StringDeltaRejected(t *testing.T) {
	dm := newTestSharedDM(t)
	seed := mustSeedMemoryForD2(t, dm, "reinforce string-delta probe")
	_, err := handleMpmMemory(dm, defaultACForPatch(), map[string]interface{}{
		"action": "reinforce",
		"params": map[string]interface{}{
			"memory_id": seed,
			"delta":     "abc",
		},
	})
	if err == nil {
		t.Fatalf("reinforce with delta='abc' must error")
	}
	if !strings.Contains(err.Error(), "delta") {
		t.Errorf("error must mention 'delta', got: %v", err)
	}
}

// --- weaken ---

func TestWeaken_StringDeltaRejected(t *testing.T) {
	dm := newTestSharedDM(t)
	seed := mustSeedMemoryForD2(t, dm, "weaken string-delta probe")
	_, err := handleMpmMemory(dm, defaultACForPatch(), map[string]interface{}{
		"action": "weaken",
		"params": map[string]interface{}{
			"memory_id": seed,
			"delta":     "abc",
		},
	})
	if err == nil {
		t.Fatalf("weaken with delta='abc' must error")
	}
	if !strings.Contains(err.Error(), "delta") {
		t.Errorf("error must mention 'delta', got: %v", err)
	}
}

// --- review ---

func TestReview_StringDaysRejected(t *testing.T) {
	dm := newTestSharedDM(t)
	_, err := handleMpmMemory(dm, defaultACForPatch(), map[string]interface{}{
		"action": "review",
		"params": map[string]interface{}{
			"days": "abc",
		},
	})
	if err == nil {
		t.Fatalf("review with days='abc' must error")
	}
	if !strings.Contains(err.Error(), "days") {
		t.Errorf("error must mention 'days', got: %v", err)
	}
}

func TestReview_StringLimitRejected(t *testing.T) {
	dm := newTestSharedDM(t)
	_, err := handleMpmMemory(dm, defaultACForPatch(), map[string]interface{}{
		"action": "review",
		"params": map[string]interface{}{
			"limit": "abc",
		},
	})
	if err == nil {
		t.Fatalf("review with limit='abc' must error")
	}
	if !strings.Contains(err.Error(), "limit") {
		t.Errorf("error must mention 'limit', got: %v", err)
	}
}

// --- omitted-input semantics: still legitimate default ---

// TestSetWeight_OmittedUsesDefault pins the audit distinction:
// OMISSION (key absent) is distinct from PRESENT-BUT-INVALID (key
// present with bad value). For set_weight, omission is a legitimate
// default to weight=0 (matches the pre-fix ParseFloatOr default),
// because weight=0 is a meaningful "neutral signal" value. The audit
// was explicit on this distinction.
func TestSetWeight_OmittedUsesDefault(t *testing.T) {
	dm := newTestSharedDM(t)
	seed := mustSeedMemoryForD2(t, dm, "set-weight omitted probe")

	res, err := handleMpmMemory(dm, defaultACForPatch(), map[string]interface{}{
		"action": "set_weight",
		"params": map[string]interface{}{
			"memory_id": seed,
			// weight key absent — omission is legitimate; default is 0
		},
	})
	if err != nil {
		t.Fatalf("set_weight with weight omitted (legitimate default): %v", err)
	}
	m := res.(map[string]interface{})
	if w, _ := m["weight"].(float64); w != 0 {
		t.Errorf("omitted weight default = %v, want 0", w)
	}
}

// --- helpers ---

func mustSeedMemoryForD2(t *testing.T, dm internal.CoreDB, fact string) string {
	t.Helper()
	id := fmt.Sprintf("mem-d2-%s-%s", strings.ReplaceAll(fact, " ", "_"), strings.ReplaceAll(t.Name(), "/", "_"))
	_, err := dm.SQLDB().Exec(`
		INSERT INTO memories (id, collection, content, weight, tags, metadata, deleted_at, created_at, updated_at)
		VALUES (?, 'memories', ?, 5, '[]', '{}', NULL, CAST(strftime('%s','now') AS INTEGER), CAST(strftime('%s','now') AS INTEGER))
	`, id, fact)
	if err != nil {
		t.Fatalf("seed memory %s: %v", id, err)
	}
	return id
}
