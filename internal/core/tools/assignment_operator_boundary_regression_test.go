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
// # The operator's side is a genuine capability boundary WITHIN THE
// DISPATCH SURFACE
//
// A root is the work order that everything else inherits its budgets
// from, and an approval is the operator's statement that something was
// authorised. An agent that could reach either through the agent-facing
// surface could:
//
//   - mint its own budget (a root with max_tasks = 1000), and
//   - approve its own work.
//
// # WHAT THIS BOUNDARY IS NOT
//
// It is a boundary on the DISPATCH SURFACE, not on the account.
//
// MPM is a single-operator trust domain. An agent running under the
// operator's own account can read the operator's files and run the
// operator's binaries, so it can run `mpm assignment create-root`
// itself. Nothing in this package, and nothing in the service, can tell
// the two apart: MPM has no identity to check — it records who a caller
// SAYS it is, and that is not authentication.
//
// What the boundary DOES guarantee is structural and checkable:
//
//   - no action of any registry tool creates a root or records an
//     approval, under any payload shape;
//   - the operator's verbs are a DIFFERENT entry point, so there is no
//     action to dispatch to; and
//   - the service's own defence is an interface with an UNEXPORTED
//     method, so nothing outside the package can fabricate the
//     authority.
//
// The boundary is not a substitute for an identity provider. PR 2
// deliberately does not build one, and nothing here claims to.
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
//
// The operator's own path is proved in cmd/mpm (assignment_cmds_test.go),
// which drives the CLI verbs end to end.
package tools

import (
	"encoding/json"
	"testing"

	mpminternal "github.com/flowbyte-com/mpm-core"
	"github.com/flowbyte-com/mpm-core/assignment"
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
// a review verdict — and this test proves it by DOING both through the
// agent-facing handler.
//
// A test that only checked that a delegation FAILED would pass with a
// handler that refused everything: "the boundary holds" and "the tool is
// broken" look the same from the outside. So the delegation here
// SUCCEEDS, and the review verdict SUCCEEDS.
func TestAssignment_TheAgentSurfaceStillDoesItsWork(t *testing.T) {
	dm := newTestDMForTools(t)

	if _, err := dm.SQLDB().Exec(
		`INSERT INTO projects (id, created_by) VALUES ('cpj-agent-surface', 'boundary-test')`); err != nil {
		t.Fatalf("seed the project: %v", err)
	}

	// The operator's root, created through the SERVICE — which is what
	// the operator's CLI verb calls. The agent-facing surface has no
	// way to make this.
	store := assignment.NewStore(dm)
	root, created, err := store.CreateRoot(
		assignment.NewLocalOperatorAuthority("operator"),
		assignment.CreateRootParams{
			ProjectID:           "cpj-agent-surface",
			Title:               "the root",
			Objective:           "the objective",
			Recipient:           "alpha",
			IdempotencyKey:      "idem:agent-surface:root",
			PermittedOperations: []string{"read", "write"},
			EligibleRecipients:  []string{"alpha", "beta", "charlie"},
			MaxDepth:            2,
			MaxTasks:            8,
			MaxConcurrency:      2,
			MaxResourceUnits:    100,
		})
	if err != nil || !created {
		t.Fatalf("create the root: %v (created=%v)", err, created)
	}

	// The agent-facing surface delegates a bounded child, and it
	// SUCCEEDS.
	child, err := handleMpmAssignment(dm, mpminternal.ActiveContext{}, map[string]interface{}{
		"action": "delegate",
		"params": map[string]interface{}{
			"parent_id":       root.ID,
			"title":           "the child",
			"objective":       "the work",
			"recipient":       "beta",
			"role":            "implementer",
			"idempotency_key": "idem:agent-surface:child",
		},
	})
	if err != nil {
		t.Fatalf("the agent surface could not delegate a bounded child: %v", err)
	}
	childAssignment, ok := child.(*assignment.Assignment)
	if !ok {
		t.Fatalf("the delegation returned %T, want an assignment", child)
	}
	if childAssignment.ParentID != root.ID {
		t.Errorf("the child names the parent %q, want %q", childAssignment.ParentID, root.ID)
	}
	if childAssignment.Depth != 1 {
		t.Errorf("the child is at depth %d, want 1", childAssignment.Depth)
	}
	if childAssignment.RootID != root.ID {
		t.Errorf("the child is in the tree %q, want %q", childAssignment.RootID, root.ID)
	}

	// And the review's verdict, through the same surface, and it
	// SUCCEEDS too. The reviewer is a SIBLING of the subject, and its
	// target is fixed by the delegation that created it — so the
	// reviewer cannot be re-pointed at something else afterwards.
	completed := advanceToCompleted(t, store, childAssignment)

	reviewer, created, err := store.Delegate(assignment.DelegateParams{
		ParentID:       root.ID,
		Title:          "the reviewer",
		Objective:      "review the work",
		Recipient:      "charlie",
		Role:           assignment.RoleReviewer,
		IdempotencyKey: "idem:agent-surface:reviewer",
		Payload:        map[string]interface{}{assignment.PayloadKeyReviewTarget: completed.ID},
	})
	if err != nil || !created {
		t.Fatalf("create the reviewer: %v (created=%v)", err, created)
	}
	reviewing := advanceToInProgress(t, store, reviewer)

	verdict, err := handleMpmAssignment(dm, mpminternal.ActiveContext{}, map[string]interface{}{
		"action": "record-review",
		"params": map[string]interface{}{
			"id":      reviewing.ID,
			"verdict": "passed",
			"epoch":   reviewing.ClaimEpoch,
			"actor":   "charlie",
		},
	})
	if err != nil {
		t.Fatalf("the agent surface could not record the review verdict: %v", err)
	}
	reviewed, ok := verdict.(*assignment.Assignment)
	if !ok {
		t.Fatalf("the verdict returned %T, want an assignment", verdict)
	}
	if reviewed.State != assignment.StateReviewPassed {
		t.Errorf("the reviewer is %q after the verdict, want %q",
			reviewed.State, assignment.StateReviewPassed)
	}

	// The SUBJECT moved too, which is the whole point of the verdict.
	after, err := store.Get(completed.ID)
	if err != nil {
		t.Fatalf("read the subject: %v", err)
	}
	if after.State != assignment.StateReviewPassed {
		t.Errorf("the subject is %q after the verdict, want %q",
			after.State, assignment.StateReviewPassed)
	}
}

// advanceToCompleted drives an assignment through the implementer's path
// to `completed`, through the service.
func advanceToCompleted(t *testing.T, store *assignment.Store, a *assignment.Assignment) *assignment.Assignment {
	t.Helper()
	cur := advanceToAccepted(t, store, a)
	cur = assignmentStep(t, cur, func() (*assignment.Assignment, error) {
		return store.Claim(assignment.ClaimParams{
			ID: cur.ID, ExpectedEpoch: cur.ClaimEpoch, Owner: cur.Recipient, LeaseSeconds: 600,
			Actor: assignment.Actor{Address: cur.Recipient, Kind: "agent"},
		})
	})
	cur = assignmentStep(t, cur, func() (*assignment.Assignment, error) {
		return store.Start(assignment.FencedParams{
			ID: cur.ID, Epoch: cur.ClaimEpoch, Actor: assignment.Actor{Address: cur.Recipient, Kind: "agent"},
		})
	})
	return assignmentStep(t, cur, func() (*assignment.Assignment, error) {
		return store.Complete(assignment.FencedParams{
			ID: cur.ID, Epoch: cur.ClaimEpoch, Actor: assignment.Actor{Address: cur.Recipient, Kind: "agent"},
		})
	})
}

// advanceToInProgress drives an assignment through the implementer's path
// to `in_progress`, which is where a reviewer must be to record a
// verdict.
func advanceToInProgress(t *testing.T, store *assignment.Store, a *assignment.Assignment) *assignment.Assignment {
	t.Helper()
	cur := advanceToAccepted(t, store, a)
	cur = assignmentStep(t, cur, func() (*assignment.Assignment, error) {
		return store.Claim(assignment.ClaimParams{
			ID: cur.ID, ExpectedEpoch: cur.ClaimEpoch, Owner: cur.Recipient, LeaseSeconds: 600,
			Actor: assignment.Actor{Address: cur.Recipient, Kind: "agent"},
		})
	})
	return assignmentStep(t, cur, func() (*assignment.Assignment, error) {
		return store.Start(assignment.FencedParams{
			ID: cur.ID, Epoch: cur.ClaimEpoch, Actor: assignment.Actor{Address: cur.Recipient, Kind: "agent"},
		})
	})
}

// advanceToAccepted drives an assignment to `accepted`.
func advanceToAccepted(t *testing.T, store *assignment.Store, a *assignment.Assignment) *assignment.Assignment {
	t.Helper()
	actor := assignment.Actor{Address: a.Recipient, Kind: "agent"}
	cur := assignmentStep(t, a, func() (*assignment.Assignment, error) {
		return store.Deliver(assignment.FencedParams{ID: a.ID, Epoch: a.ClaimEpoch, Actor: actor})
	})
	cur = assignmentStep(t, cur, func() (*assignment.Assignment, error) {
		return store.Acknowledge(assignment.FencedParams{ID: cur.ID, Epoch: cur.ClaimEpoch, Actor: actor})
	})
	return assignmentStep(t, cur, func() (*assignment.Assignment, error) {
		return store.Accept(assignment.FencedParams{ID: cur.ID, Epoch: cur.ClaimEpoch, Actor: actor})
	})
}

// assignmentStep runs one transition and fails the test if it does not
// succeed.
func assignmentStep(t *testing.T, a *assignment.Assignment,
	op func() (*assignment.Assignment, error)) *assignment.Assignment {
	t.Helper()
	out, err := op()
	if err != nil {
		t.Fatalf("advance %s: %v", a.ID, err)
	}
	return out
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
