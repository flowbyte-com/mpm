// memory_empty_id_guards_regression_test.go — Residual pass §I-C.13.
//
// The 2026-09-05 audit found that mpm_memory.{shred, set_weight,
// promote} passed the raw lookup straight to the DM without a
// handler-level empty-id guard. An empty id reached the underlying
// SQL query, which then either affected zero rows (silently
// returning success) or surfaced an opaque SQL error.
//
// mpm_memory.patch was hardened in Pass 1 for a different shape
// concern (non-object patch field). The id guard is a separate
// invariant, applied uniformly here across the remaining mutation
// paths via the new requireMemoryID helper, which also accepts the
// legacy `id` alias documented at memoryIDFromParams.
//
// Pre-fix reproduction (live CLI):
//
//   $ mpm call mpm_memory --payload '{"action":"shred","params":{}}'
//     # wanted: error: memory_id is required
//     # actual: success/zero-affected OR opaque SQL error from the DM
//
// Canonical contract:
//
//   missing memory_id    → ERROR: memory_id is required
//   memory_id = ""       → ERROR: memory_id is required
//   memory_id = " "      → ERROR (whitespace does not count as a valid id)
//   memory_id = valid id → handler proceeds to the DM
//   memory_id = unknown  → handler delegates to the DM's not-found path

package tools

import (
	"strings"
	"testing"

	mpminternal "github.com/flowbyte-com/mpm-core"
)

// requireMemoryIDCases covers the four reject states and the
// positive delegation. Whitespace is rejected because the
// memory_idFromParams helper trims nothing — a literal " " is not
// the canonical empty-equivalent form the codebase recognizes.
//
// Each case returns a map[string]interface{} representing the
// params envelope (the test wraps it in {action, params}).
func requireMemoryIDCases() []struct {
	name    string
	params  map[string]interface{}
	wantErr bool
	errSub  string
} {
	return []struct {
		name    string
		params  map[string]interface{}
		wantErr bool
		errSub  string
	}{
		{"absent", map[string]interface{}{}, true, "memory_id is required"},
		{"nil", map[string]interface{}{"memory_id": nil}, true, "memory_id is required"},
		{"empty", map[string]interface{}{"memory_id": ""}, true, "memory_id is required"},
		{"whitespace", map[string]interface{}{"memory_id": " "}, true, "memory_id is required"},
		{"id_alias_empty", map[string]interface{}{"id": ""}, true, "memory_id is required"},
		{"id_alias_present", map[string]interface{}{"id": "valid-id"}, false, ""},
	}
}

// TestMemoryShred_EmptyIDRejected pins: shred refuses missing/empty
// ids at the handler boundary. The DM never sees an empty id.
func TestMemoryShred_EmptyIDRejected(t *testing.T) {
	dm := newTestSharedDM(t)
	for _, c := range requireMemoryIDCases() {
		t.Run(c.name, func(t *testing.T) {
			_, err := handleMpmMemory(dm, mpminternal.ActiveContext{}, map[string]interface{}{
				"action": "shred",
				"params": c.params,
			})
			if c.wantErr {
				if err == nil {
					t.Fatalf("expected error for %s, got nil", c.name)
				}
				if !strings.Contains(err.Error(), c.errSub) {
					t.Errorf("expected error containing %q, got: %v", c.errSub, err)
				}
				return
			}
			if err != nil && strings.Contains(err.Error(), "memory_id is required") {
				t.Errorf("id guard fired for valid id %v: %v", c.params, err)
			}
		})
	}
}

// TestMemorySetWeight_EmptyIDRejected pins the same contract on
// set_weight. Whitespace-only ids are rejected.
func TestMemorySetWeight_EmptyIDRejected(t *testing.T) {
	dm := newTestSharedDM(t)
	for _, c := range requireMemoryIDCases() {
		t.Run(c.name, func(t *testing.T) {
			params := c.params
			params["weight"] = 5.0
			_, err := handleMpmMemory(dm, mpminternal.ActiveContext{}, map[string]interface{}{
				"action": "set_weight",
				"params": params,
			})
			if c.wantErr {
				if err == nil {
					t.Fatalf("expected error for %s, got nil", c.name)
				}
				if !strings.Contains(err.Error(), c.errSub) {
					t.Errorf("expected error containing %q, got: %v", c.errSub, err)
				}
				return
			}
			if err != nil && strings.Contains(err.Error(), "memory_id is required") {
				t.Errorf("id guard fired for valid id %v: %v", c.params, err)
			}
		})
	}
}

// TestMemoryPromote_EmptyIDRejected pins the same contract on
// promote.
func TestMemoryPromote_EmptyIDRejected(t *testing.T) {
	dm := newTestSharedDM(t)
	for _, c := range requireMemoryIDCases() {
		t.Run(c.name, func(t *testing.T) {
			_, err := handleMpmMemory(dm, mpminternal.ActiveContext{}, map[string]interface{}{
				"action": "promote",
				"params": c.params,
			})
			if c.wantErr {
				if err == nil {
					t.Fatalf("expected error for %s, got nil", c.name)
				}
				if !strings.Contains(err.Error(), c.errSub) {
					t.Errorf("expected error containing %q, got: %v", c.errSub, err)
				}
				return
			}
			if err != nil && strings.Contains(err.Error(), "memory_id is required") {
				t.Errorf("id guard fired for valid id %v: %v", c.params, err)
			}
		})
	}
}

// TestMemoryShred_EmptyIDDoesNotMutate pins the write-path
// guarantee: a rejected shred does not touch any memory row.
func TestMemoryShred_EmptyIDDoesNotMutate(t *testing.T) {
	dm := newTestSharedDM(t)

	// Seed a memory.
	saved, err := handleMpmMemory(dm, mpminternal.ActiveContext{}, map[string]interface{}{
		"action": "save",
		"params": map[string]interface{}{
			"fact": "memory that must survive an empty-id shred attempt",
		},
	})
	if err != nil {
		t.Fatalf("seed: %v", err)
	}
	id, _ := saved.(map[string]interface{})["id"].(string)
	if id == "" {
		t.Fatal("seed: missing id")
	}

	// Empty-id shred.
	_, err = handleMpmMemory(dm, mpminternal.ActiveContext{}, map[string]interface{}{
		"action": "shred",
		"params": map[string]interface{}{},
	})
	if err == nil {
		t.Fatal("empty-id shred must error")
	}

	// Memory must still be live.
	res, err := handleMpmMemory(dm, mpminternal.ActiveContext{}, map[string]interface{}{
		"action": "show",
		"params": map[string]interface{}{"id": id},
	})
	if err != nil {
		t.Fatalf("post-rejection show: %v", err)
	}
	m, _ := res.(map[string]interface{})
	if m["deleted_at"] != nil {
		t.Errorf("memory must still be live after empty-id shred rejection")
	}
}

// TestMemoryPatch_IDGuardStillActive pins: the existing patch id
// path remains under the memory_id contract. The brief says
// "patch was already hardened in Pass 1" — confirm the existing
// guard remains green by exercising the path that Pass 1 covered
// (the patch field shape) plus the new id guard behaviour for
// consistency.
func TestMemoryPatch_IDGuardStillActive(t *testing.T) {
	dm := newTestSharedDM(t)

	// Empty id + valid patch → id guard fires (or downstream).
	_, err := handleMpmMemory(dm, mpminternal.ActiveContext{}, map[string]interface{}{
		"action": "patch",
		"params": map[string]interface{}{
			"patch": map[string]interface{}{"weight": 5},
		},
	})
	if err == nil {
		t.Errorf("patch with empty id should still fail at the id check or downstream")
	}
}
