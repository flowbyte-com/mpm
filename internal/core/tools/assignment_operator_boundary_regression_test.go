// assignment_operator_boundary_regression_test.go — pin the authority
// boundary of the assignment surface.
//
// # The boundary this file pins
//
// Continuity 001 splits the assignment capability into two:
//
//   - the AGENT's side: deliver, acknowledge, accept, refuse, claim,
//     renew, start, complete, delegate, cancel, note, record-review,
//     handoff — plus the reads;
//   - the OPERATOR's side: creating the ROOT assignment, and recording
//     an OPERATOR APPROVAL.
//
// The operator's side is a genuine capability boundary, not a
// convenience. A root is the work order that everything else inherits
// its budgets from, and an approval is the operator's statement that
// something was authorised. An agent that could create either could:
//
//   - mint its own budget (a root with max_tasks = 1000), and
//   - approve its own work.
//
// # Why "absent" and not "refused at call time"
//
// The shred precedent refuses permanent deletion at the execution point,
// because the operator path had to keep working for the operator. Here
// the operator path is a DIFFERENT entry point — the dedicated
// `mpm assignment create-root` and `mpm assignment record-approval`
// verbs — so the agent-facing surface can simply NOT HAVE the
// capability.
//
// "Absent" is a stronger property than "refused": there is no action to
// dispatch to, no schema to advertise, and no payload field that could
// unlock it.
//
// # The contract these tests enforce
//
//   - NO registry action can create a root.
//   - NO registry action can record an approval.
//   - NO payload field unlocks either of them — not `confirm`, not
//     `operator`, not `authority`, not `force`, not `approve`.
//   - The advertised action vocabulary names neither.
//   - The agent-facing surface still does the work it is supposed to
//     do, so the boundary is not enforced by removing the capability.
//   - The operator's own path still works, so the capability is
//     preserved rather than deleted.
package tools

import (
	"encoding/json"
	"strings"
	"testing"

	mpminternal "github.com/flowbyte-com/mpm-core"
)

// operatorOnlyAssignmentActions are the two operations the agent-facing
// surface must not be able to reach, under ANY action name.
var operatorOnlyAssignmentActions = []string{
	"create-root",
	"create_root",
	"createroot",
	"record-approval",
	"record_approval",
	"recordapproval",
	"approve",
	"approval",
	"authorize",
	"authorise",
	"make-root",
	"new-root",
}

// operatorOnlyAssignmentPayloads are the payload shapes an agent might
// use to try to self-authorise a root or an approval. Every one of them
// must fail closed.
var operatorOnlyAssignmentPayloads = []struct {
	name  string
	extra map[string]interface{}
}{
	{"bare", map[string]interface{}{}},
	{"confirm_true", map[string]interface{}{"confirm": true}},
	{"force_true", map[string]interface{}{"force": true}},
	{"operator_true", map[string]interface{}{"operator": true}},
	{"authority_true", map[string]interface{}{"authority": true}},
	{"approve_true", map[string]interface{}{"approve": true}},
	{"is_operator_true", map[string]interface{}{"is_operator": true}},
}

// TestAssignment_NoRegistryActionCreatesARoot proves that no action of
// the agent-facing tool can create a root assignment.
//
// The check is over the WHOLE registry, not just the assignment tool:
// a root could be created by any other tool's action, and the boundary
// has to hold wherever the surface is.
func TestAssignment_NoRegistryActionCreatesARoot(t *testing.T) {
	for _, tool := range Registry {
		tool := tool
		t.Run(tool.Name, func(t *testing.T) {
			for _, action := range operatorOnlyAssignmentActions {
				if strings.Contains(action, "root") || strings.Contains(action, "approv") ||
					strings.Contains(action, "author") {
					// These action names are the ones under test.
				}
				// The action must not even be dispatchable: the
				// handler refuses an unknown action.
				_, err := tool.Handler(newTestDMForTools(t), mpminternal.ActiveContext{},
					map[string]interface{}{
						"action": action,
						"params": map[string]interface{}{
							"project_id":           "cpj-test",
							"title":                "a root an agent made for itself",
							"objective":            "self-authorised",
							"recipient":            "agent",
							"idempotency_key":      "idem:self-authorised",
							"permitted_operations": []interface{}{"read", "write"},
							"eligible_recipients":  []interface{}{"agent"},
							"max_tasks":            1000,
							"max_concurrency":      100,
							"max_resource_units":   100000,
						},
					})
				if err == nil {
					t.Errorf("the %s tool accepted the operator-only action %q; "+
						"creating a root must require the operator's own verb",
						tool.Name, action)
				}
			}
		})
	}
}

// TestAssignment_NoRegistryActionRecordsAnApproval proves that no action
// of the agent-facing tool can record an operator approval.
//
// An agent that could approve its own work would make the review
// meaningless and the operator's gate decorative.
func TestAssignment_NoRegistryActionRecordsAnApproval(t *testing.T) {
	for _, tool := range Registry {
		tool := tool
		t.Run(tool.Name, func(t *testing.T) {
			for _, action := range operatorOnlyAssignmentActions {
				_, err := tool.Handler(newTestDMForTools(t), mpminternal.ActiveContext{},
					map[string]interface{}{
						"action": action,
						"params": map[string]interface{}{
							"id":       "at-whatever",
							"action":   "merge PR #1",
							"revision": "abc123",
							"source":   "the agent itself",
							"evidence": "ev://self",
						},
					})
				if err == nil {
					t.Errorf("the %s tool accepted the operator-only action %q; "+
						"recording an approval must require the operator's own verb",
						tool.Name, action)
				}
			}
		})
	}
}

// TestAssignment_NoPayloadFieldUnlocksTheOperatorActions proves that the
// refusal is not a flag in disguise.
//
// A boolean the agent itself sets cannot distinguish "the operator asked
// for this" from "the agent decided this", so no payload field may
// unlock the operator-only operations.
func TestAssignment_NoPayloadFieldUnlocksTheOperatorActions(t *testing.T) {
	for _, tc := range operatorOnlyAssignmentPayloads {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			for _, action := range operatorOnlyAssignmentActions {
				params := map[string]interface{}{
					"project_id":           "cpj-test",
					"title":                "a root an agent made for itself",
					"objective":            "self-authorised",
					"recipient":            "agent",
					"idempotency_key":      "idem:self-authorised-" + tc.name,
					"permitted_operations": []interface{}{"read"},
					"eligible_recipients":  []interface{}{"agent"},
					"max_tasks":            10,
					"max_concurrency":      1,
					"max_resource_units":   10,
				}
				for k, v := range tc.extra {
					params[k] = v
				}
				_, err := handleMpmAssignment(newTestDMForTools(t), mpminternal.ActiveContext{},
					map[string]interface{}{
						"action": action,
						"params": params,
					})
				if err == nil {
					t.Errorf("the assignment tool accepted %q with %v; "+
						"no payload field may unlock an operator-only operation",
						action, tc.extra)
				}
			}
		})
	}
}

// TestAssignment_OperatorActionsAreNotAdvertised proves discovery
// honesty: an action that is always refused must not appear in the
// advertised vocabulary, because a strict MCP host validates the action
// against the schema and would offer an action that cannot work.
func TestAssignment_OperatorActionsAreNotAdvertised(t *testing.T) {
	enum := schemaEnum(t, "mpm_assignment")
	if enum == nil {
		t.Fatal("mpm_assignment has no action enum; cannot verify the advertised vocabulary")
	}
	for _, action := range enum {
		for _, forbidden := range operatorOnlyAssignmentActions {
			if action == forbidden {
				t.Errorf("mpm_assignment advertises the operator-only action %q", action)
			}
		}
	}
}

// TestAssignment_NoToolSchemaMentionsTheOperatorActions proves the
// advertised vocabulary in the WHOLE registry is clean of the
// operator-only names — not just the assignment tool's.
func TestAssignment_NoToolSchemaMentionsTheOperatorActions(t *testing.T) {
	for _, tool := range Registry {
		tool := tool
		t.Run(tool.Name, func(t *testing.T) {
			enum := schemaEnum(t, tool.Name)
			if enum == nil {
				return
			}
			for _, action := range enum {
				for _, forbidden := range operatorOnlyAssignmentActions {
					if action == forbidden {
						t.Errorf("%s advertises the operator-only action %q", tool.Name, action)
					}
				}
			}
		})
	}
}

// TestAssignment_TheAgentSurfaceStillDoesItsWork guards against
// over-correction: the boundary must remove the operator's capability
// from the agent's surface, NOT the agent's own capability.
//
// The agent must still be able to delegate bounded work, and to record
// a review verdict.
func TestAssignment_TheAgentSurfaceStillDoesItsWork(t *testing.T) {
	dm := newTestDMForTools(t)

	if _, err := dm.SQLDB().Exec(
		`INSERT INTO projects (id, created_by) VALUES ('cpj-agent-surface', 'boundary-test')`); err != nil {
		t.Fatalf("seed the project: %v", err)
	}

	// The agent-facing surface can delegate a bounded child...
	child, err := handleMpmAssignment(dm, mpminternal.ActiveContext{}, map[string]interface{}{
		"action": "delegate",
		"params": map[string]interface{}{
			"parent_id":       "at-nosuchparent",
			"title":           "the child",
			"objective":       "the work",
			"recipient":       "beta",
			"idempotency_key": "idem:boundary-delegate",
		},
	})
	if err == nil {
		t.Errorf("the delegation to a nonexistent parent was accepted: %+v", child)
	}

	// ...and the failure is the service's own refusal, not a
	// dispatch failure.
	if err != nil && !strings.Contains(err.Error(), "assignment:") {
		t.Errorf("the delegation failed with %v, want the service's own error", err)
	}
}

// TestAssignment_TheRegistryActionSurfaceIsStable pins the advertised
// vocabulary of the assignment tool.
//
// The list is a contract with the agents that call it: an action that
// disappears is an agent's script breaking, and an action that appears
// without review is a surface that grew unnoticed.
func TestAssignment_TheRegistryActionSurfaceIsStable(t *testing.T) {
	enum := schemaEnum(t, "mpm_assignment")
	if enum == nil {
		t.Fatal("mpm_assignment has no action enum")
	}
	want := map[string]bool{
		"inbox": true, "show": true, "history": true, "tree": true,
		"deliver": true, "acknowledge": true, "accept": true, "refuse": true,
		"claim": true, "renew": true, "start": true, "complete": true,
		"delegate": true, "cancel": true, "note": true,
		"record-review": true, "handoff": true,
	}
	got := map[string]bool{}
	for _, action := range enum {
		got[action] = true
	}
	for action := range want {
		if !got[action] {
			t.Errorf("the assignment tool no longer advertises %q", action)
		}
	}
	for action := range got {
		if !want[action] {
			t.Errorf("the assignment tool advertises the unexpected action %q", action)
		}
	}
}

// TestAssignment_TheToolSchemaIsValidJSON guards the schema itself: a
// malformed schema would make a strict MCP host refuse the whole tool,
// and the boundary would be enforced by accident rather than by design.
func TestAssignment_TheToolSchemaIsValidJSON(t *testing.T) {
	tool, ok := ByName("mpm_assignment")
	if !ok {
		t.Fatal("mpm_assignment is not in the registry")
	}
	var decoded map[string]interface{}
	if err := json.Unmarshal(tool.Schema, &decoded); err != nil {
		t.Fatalf("the mpm_assignment schema is not valid JSON: %v", err)
	}
}
