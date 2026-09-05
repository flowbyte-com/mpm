// memory_patch_validation_test.go — regression for the 2026-09-05
// audit P2 finding: mpm_memory.patch silently accepts non-object
// patches (null, scalar, array) and forwards them to JSONPatch,
// producing destructive or undefined behaviour against the
// metadata JSON column.
//
// Canonical contract (per the registry description and the patch
// semantics of SQLite's json_patch function): a patch is a JSON
// object — non-object inputs are user errors and must be rejected
// at the public handler boundary, BEFORE the patch reaches the
// persistence layer.
//
// The pre-fix handler (handlers.go:1092) accepted any
// JSON-marshalable value (including nil, primitives, arrays) and
// relied on the DM (UpdateMemoryMetadata → json.Valid) to reject
// "invalid JSON" — but json.Valid accepts null/primitives/arrays
// as valid JSON. The handler's claim "A nil/primitive patch is
// rejected upstream by the DM" was false; the DM accepted everything
// that was valid JSON.

package tools

import (
	"database/sql"
	"encoding/json"
	"strings"
	"testing"

	"github.com/flowbyte-com/mpm-core"
)

// TestMpmMemoryPatch_NullRejected pins the canonical contract:
// `patch = null` (or absent) is rejected with a deterministic error.
// The metadata must NOT change after the rejected call.
func TestMpmMemoryPatch_NullRejected(t *testing.T) {
	dm := newTestSharedDM(t)
	seed := mustSeedMemoryForPatch(t, dm, "patch-null probe")

	preMeta := mustReadMetadata(t, dm, seed)

	_, err := handleMpmMemory(dm, defaultACForPatch(), map[string]interface{}{
		"action": "patch",
		"params": map[string]interface{}{
			"memory_id": seed,
			"patch":     nil,
		},
	})
	if err == nil {
		t.Fatalf("expected error for null patch")
	}
	if !strings.Contains(err.Error(), "object") {
		t.Errorf("expected error message to mention 'object', got: %v", err)
	}

	postMeta := mustReadMetadata(t, dm, seed)
	if !mapsEqualForPatch(preMeta, postMeta) {
		t.Errorf("metadata must not change after rejected null patch: pre=%v post=%v", preMeta, postMeta)
	}
}

// TestMpmMemoryPatch_ScalarRejected pins: scalar patches (string,
// number, bool) are rejected with a deterministic error.
func TestMpmMemoryPatch_ScalarRejected(t *testing.T) {
	dm := newTestSharedDM(t)
	seed := mustSeedMemoryForPatch(t, dm, "patch-scalar probe")

	for label, val := range map[string]interface{}{
		"string": "a string",
		"number": float64(42),
		"bool":   true,
	} {
		t.Run(label, func(t *testing.T) {
			preMeta := mustReadMetadata(t, dm, seed)
			_, err := handleMpmMemory(dm, defaultACForPatch(), map[string]interface{}{
				"action": "patch",
				"params": map[string]interface{}{
					"memory_id": seed,
					"patch":     val,
				},
			})
			if err == nil {
				t.Fatalf("expected error for %s patch", label)
			}
			if !strings.Contains(err.Error(), "object") {
				t.Errorf("%s patch error must mention 'object', got: %v", label, err)
			}
			postMeta := mustReadMetadata(t, dm, seed)
			if !mapsEqualForPatch(preMeta, postMeta) {
				t.Errorf("metadata must not change after rejected %s patch: pre=%v post=%v", label, preMeta, postMeta)
			}
		})
	}
}

// TestMpmMemoryPatch_ArrayRejected pins: array patches are rejected
// with a deterministic error. The canonical MPM contract is
// object-only — array patches produce undefined metadata shapes
// against an object target.
func TestMpmMemoryPatch_ArrayRejected(t *testing.T) {
	dm := newTestSharedDM(t)
	seed := mustSeedMemoryForPatch(t, dm, "patch-array probe")

	preMeta := mustReadMetadata(t, dm, seed)
	_, err := handleMpmMemory(dm, defaultACForPatch(), map[string]interface{}{
		"action": "patch",
		"params": map[string]interface{}{
			"memory_id": seed,
			"patch":     []interface{}{"a", "b", "c"},
		},
	})
	if err == nil {
		t.Fatalf("expected error for array patch, got nil")
	}
	if !strings.Contains(err.Error(), "object") {
		t.Errorf("array patch error must mention 'object', got: %v", err)
	}
	postMeta := mustReadMetadata(t, dm, seed)
	if !mapsEqualForPatch(preMeta, postMeta) {
		t.Errorf("metadata must not change after rejected array patch: pre=%v post=%v", preMeta, postMeta)
	}
}

// TestMpmMemoryPatch_ValidObjectMerged pins the happy path: a
// non-empty object patch merges into metadata. This proves the fix
// did not regress the documented behaviour.
func TestMpmMemoryPatch_ValidObjectMerged(t *testing.T) {
	dm := newTestSharedDM(t)
	seed := mustSeedMemoryForPatch(t, dm, "patch-valid probe")

	_, err := handleMpmMemory(dm, defaultACForPatch(), map[string]interface{}{
		"action": "patch",
		"params": map[string]interface{}{
			"memory_id": seed,
			"patch": map[string]interface{}{
				"new_key": "new_value",
				"nested":  map[string]interface{}{"a": float64(1)},
			},
		},
	})
	if err != nil {
		t.Fatalf("valid object patch: %v", err)
	}

	postMeta := mustReadMetadata(t, dm, seed)
	v, ok := postMeta["new_key"].(string)
	if !ok || v != "new_value" {
		t.Errorf("new_key not merged into metadata: %v", postMeta)
	}
}

// TestMpmMemoryPatch_EmptyObjectAccepted pins that an empty object
// patch is a no-op merge (not destructive, not an error).
func TestMpmMemoryPatch_EmptyObjectAccepted(t *testing.T) {
	dm := newTestSharedDM(t)
	seed := mustSeedMemoryForPatch(t, dm, "patch-empty probe")

	preMeta := mustReadMetadata(t, dm, seed)
	_, err := handleMpmMemory(dm, defaultACForPatch(), map[string]interface{}{
		"action": "patch",
		"params": map[string]interface{}{
			"memory_id": seed,
			"patch":     map[string]interface{}{},
		},
	})
	if err != nil {
		t.Fatalf("empty object patch should be a valid no-op, got: %v", err)
	}
	postMeta := mustReadMetadata(t, dm, seed)
	if !mapsEqualForPatch(preMeta, postMeta) {
		t.Errorf("empty object patch should leave metadata unchanged: pre=%v post=%v", preMeta, postMeta)
	}
}

// TestMpmMemoryPatch_NilKeyRejected pins: `params` without a `patch`
// key is rejected. Same semantic as null patch.
func TestMpmMemoryPatch_NilKeyRejected(t *testing.T) {
	dm := newTestSharedDM(t)
	seed := mustSeedMemoryForPatch(t, dm, "patch-nil-key probe")

	preMeta := mustReadMetadata(t, dm, seed)
	_, err := handleMpmMemory(dm, defaultACForPatch(), map[string]interface{}{
		"action": "patch",
		"params": map[string]interface{}{
			"memory_id": seed,
			// patch key absent → handler reads nil
		},
	})
	if err == nil {
		t.Fatalf("expected error when 'patch' key is absent")
	}
	postMeta := mustReadMetadata(t, dm, seed)
	if !mapsEqualForPatch(preMeta, postMeta) {
		t.Errorf("metadata must not change after absent-patch: pre=%v post=%v", preMeta, postMeta)
	}
}

// --- helpers ---

func mustSeedMemoryForPatch(t *testing.T, dm internal.CoreDB, fact string) string {
	t.Helper()
	id := "mem-patch-" + strings.ReplaceAll(fact, " ", "_")
	_, err := dm.SQLDB().Exec(`
		INSERT INTO memories (id, collection, content, weight, deleted_at, created_at, updated_at)
		VALUES (?, 'memories', ?, 5, NULL, '2026-09-05', '2026-09-05')
	`, id, fact)
	if err != nil {
		t.Fatalf("seed memory %s: %v", id, err)
	}
	return id
}

func mustReadMetadata(t *testing.T, dm internal.CoreDB, id string) map[string]interface{} {
	t.Helper()
	var raw sql.NullString
	err := dm.SQLDB().QueryRow(
		`SELECT metadata FROM memories WHERE id = ?`, id,
	).Scan(&raw)
	if err != nil {
		t.Fatalf("read metadata %s: %v", id, err)
	}
	out := map[string]interface{}{}
	if raw.Valid && raw.String != "" {
		if err := json.Unmarshal([]byte(raw.String), &out); err != nil {
			t.Fatalf("unmarshal metadata %s: %v", id, err)
		}
	}
	return out
}

func defaultACForPatch() internal.ActiveContext {
	return internal.ActiveContext{
		SessionID: "patch-validation-test",
		Agent:     "audit",
		Model:     "audit-m",
	}
}

func mapsEqualForPatch(a, b map[string]interface{}) bool {
	if len(a) != len(b) {
		return false
	}
	for k, v := range a {
		if !equalJSONForPatch(v, b[k]) {
			return false
		}
	}
	return true
}

func equalJSONForPatch(a, b interface{}) bool {
	if a == nil && b == nil {
		return true
	}
	if a == nil || b == nil {
		return false
	}
	return a == b
}
