// operator_only_shred_regression_test.go — pin the security boundary
// around irreversible ("shred") deletion.
//
// # The defect this file pins
//
// `mpm_memory action=shred` and `mpm_lessons action=shred` were both
// reachable from every agent-facing surface (the native MCP tool, the
// compact MCP wrapper, `mpm call`, and the adapters that sit on top of
// them). A shred is an irreversible hard delete: memory shred also
// purges a linked theory and sweeps topic memberships, revisions and
// confidence history via ShredMemoryWithCascade. An autonomous agent
// could therefore destroy durable substrate state — and, through the
// memory cascade, a theory it did not author — with a single tool
// call, without any operator involvement.
//
// `confirm=true` is used elsewhere in MPM as an operator gate
// (record_global_rule, retire_global_rule, migrate), but a boolean the
// agent itself sets is not a credential: it cannot distinguish "the
// operator asked for this" from "the agent decided this". These tests
// therefore pin that NO payload field unlocks a shred.
//
// # The contract these tests enforce
//
//   - The agent-facing tool surface (MCP and `mpm call`, which share
//     the same registry handlers) refuses both permanent shred actions.
//   - The refusal is fail-closed and names the operator path.
//   - The refusal happens at the execution point, before any DELETE:
//     the row survives the attempt, including under confirm=true.
//   - Reversible `delete` / `restore` remain available to agents.
//   - The permanent-destruction capability itself is preserved for the
//     operator: the DatabaseManager hard-delete primitives still work
//     (the dedicated CLI verbs call them directly).
//   - The registry schemas stop advertising the operator-only actions,
//     so strict MCP hosts do not offer an action that will be refused.
package tools

import (
	"strings"
	"testing"

	mpminternal "github.com/flowbyte-com/mpm-core"
)

// operatorOnlyParams are the payload shapes an agent might use to try
// to self-authorize a shred. Every one of them must fail closed.
var operatorOnlyParams = []struct {
	name  string
	extra map[string]interface{}
}{
	{"bare", map[string]interface{}{}},
	{"confirm_true", map[string]interface{}{"confirm": true}},
	{"force_true", map[string]interface{}{"force": true}},
	{"operator_true", map[string]interface{}{"operator": true}},
	{"confirm_force", map[string]interface{}{"confirm": true, "force": true}},
}

// TestAgentShred_RefusedAndMemorySurvives pins the memory half of the
// boundary: the tool action refuses, the row is untouched, and no
// self-supplied authorization field changes the outcome.
func TestAgentShred_RefusedAndMemorySurvives(t *testing.T) {
	for _, tc := range operatorOnlyParams {
		t.Run(tc.name, func(t *testing.T) {
			dm := newTestDMForTools(t)

			saved, err := handleMpmMemory(dm, mpminternal.ActiveContext{}, map[string]interface{}{
				"action": "save",
				"params": map[string]interface{}{"fact": "memory that must survive an agent shred attempt"},
			})
			if err != nil {
				t.Fatalf("seed save: %v", err)
			}
			id, _ := saved.(map[string]interface{})["id"].(string)
			if id == "" {
				t.Fatal("seed save returned no id")
			}

			params := map[string]interface{}{"memory_id": id}
			for k, v := range tc.extra {
				params[k] = v
			}

			_, err = handleMpmMemory(dm, mpminternal.ActiveContext{}, map[string]interface{}{
				"action": "shred",
				"params": params,
			})
			if err == nil {
				t.Fatal("agent-accessible shred succeeded; permanent deletion must require the operator CLI")
			}
			if !strings.Contains(err.Error(), "operator-only") {
				t.Errorf("refusal must identify the boundary as operator-only, got: %v", err)
			}
			if strings.Contains(err.Error(), "memory_id is required") {
				t.Errorf("id guard fired before the authorization gate, got: %v", err)
			}

			if _, err := dm.GetMemory(id); err != nil {
				t.Fatalf("memory %s did not survive the refused shred: %v", id, err)
			}
		})
	}
}

// TestAgentShred_RefusedAndLessonSurvives is the lessons counterpart.
// ShredLesson is an irreversible hard delete of the row and its FTS
// entry; it must not be reachable from the agent surface either.
func TestAgentShred_RefusedAndLessonSurvives(t *testing.T) {
	for _, tc := range operatorOnlyParams {
		t.Run(tc.name, func(t *testing.T) {
			dm := newTestDMForTools(t)

			out, _, err := dm.SaveLesson("lesson that must survive an agent shred attempt", "insight", nil)
			if err != nil {
				t.Fatalf("seed lesson: %v", err)
			}
			id, _ := out["id"].(string)
			if id == "" {
				t.Fatal("seed lesson returned no id")
			}

			params := map[string]interface{}{"id": id}
			for k, v := range tc.extra {
				params[k] = v
			}

			_, err = handleMpmLessons(dm, mpminternal.ActiveContext{}, map[string]interface{}{
				"action": "shred",
				"params": params,
			})
			if err == nil {
				t.Fatal("agent-accessible lesson shred succeeded; permanent deletion must require the operator CLI")
			}
			if !strings.Contains(err.Error(), "operator-only") {
				t.Errorf("refusal must identify the boundary as operator-only, got: %v", err)
			}

			if _, err := dm.GetLesson(id); err != nil {
				t.Fatalf("lesson %s did not survive the refused shred: %v", id, err)
			}
		})
	}
}

// TestAgentShred_ReversibleDeleteStillWorks guards against
// over-correction: the fix must remove permanent destruction from the
// agent surface, not the ability to retire a memory or lesson safely.
func TestAgentShred_ReversibleDeleteStillWorks(t *testing.T) {
	dm := newTestDMForTools(t)

	saved, err := handleMpmMemory(dm, mpminternal.ActiveContext{}, map[string]interface{}{
		"action": "save",
		"params": map[string]interface{}{"fact": "reversibly deletable memory"},
	})
	if err != nil {
		t.Fatalf("seed save: %v", err)
	}
	id, _ := saved.(map[string]interface{})["id"].(string)

	if _, err := handleMpmMemory(dm, mpminternal.ActiveContext{}, map[string]interface{}{
		"action": "delete",
		"params": map[string]interface{}{"memory_id": id},
	}); err != nil {
		t.Fatalf("reversible delete must remain available to agents: %v", err)
	}
	if _, err := handleMpmMemory(dm, mpminternal.ActiveContext{}, map[string]interface{}{
		"action": "restore",
		"params": map[string]interface{}{"memory_id": id},
	}); err != nil {
		t.Fatalf("restore must remain available to agents: %v", err)
	}

	out, _, err := dm.SaveLesson("reversibly deletable lesson", "insight", nil)
	if err != nil {
		t.Fatalf("seed lesson: %v", err)
	}
	lessonID, _ := out["id"].(string)
	if _, err := handleMpmLessons(dm, mpminternal.ActiveContext{}, map[string]interface{}{
		"action": "delete",
		"params": map[string]interface{}{"id": lessonID},
	}); err != nil {
		t.Fatalf("reversible lesson delete must remain available: %v", err)
	}
	if _, err := handleMpmLessons(dm, mpminternal.ActiveContext{}, map[string]interface{}{
		"action": "restore",
		"params": map[string]interface{}{"id": lessonID},
	}); err != nil {
		t.Fatalf("lesson restore must remain available: %v", err)
	}
}

// TestOperatorShredPrimitiveStillWorks proves the capability is
// preserved, not deleted: the dedicated operator CLI verbs
// (`mpm memory shred`, `mpm lesson shred`) call these DatabaseManager
// primitives directly.
func TestOperatorShredPrimitiveStillWorks(t *testing.T) {
	dm := newTestDMForTools(t)

	saved, err := handleMpmMemory(dm, mpminternal.ActiveContext{}, map[string]interface{}{
		"action": "save",
		"params": map[string]interface{}{"fact": "operator-shredded memory"},
	})
	if err != nil {
		t.Fatalf("seed save: %v", err)
	}
	id, _ := saved.(map[string]interface{})["id"].(string)

	if _, err := dm.ShredMemoryWithCascade(id); err != nil {
		t.Fatalf("operator shred primitive must remain available: %v", err)
	}
	if _, err := dm.GetMemory(id); err == nil {
		t.Fatalf("operator shred primitive did not remove memory %s", id)
	}

	out, _, err := dm.SaveLesson("operator-shredded lesson", "insight", nil)
	if err != nil {
		t.Fatalf("seed lesson: %v", err)
	}
	lessonID, _ := out["id"].(string)
	if err := dm.ShredLesson(lessonID); err != nil {
		t.Fatalf("operator lesson shred primitive must remain available: %v", err)
	}
	if _, err := dm.GetLesson(lessonID); err == nil {
		t.Fatalf("operator lesson shred primitive did not remove lesson %s", lessonID)
	}
}

// TestAgentShred_NotAdvertisedInRegistrySchema pins discovery honesty:
// an action that will always be refused must not appear in the
// advertised action vocabulary that strict MCP hosts validate against.
func TestAgentShred_NotAdvertisedInRegistrySchema(t *testing.T) {
	for _, tool := range []string{"mpm_memory", "mpm_lessons"} {
		tool := tool
		t.Run(tool, func(t *testing.T) {
			enum := schemaEnum(t, tool)
			if enum == nil {
				t.Fatalf("%s has no action enum; cannot verify the advertised vocabulary", tool)
			}
			for _, action := range enum {
				if action == "shred" {
					t.Errorf("%s advertises operator-only action %q; strict MCP hosts would "+
						"offer an action that is always refused", tool, action)
				}
			}
		})
	}
}
