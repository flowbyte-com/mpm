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
//   - the OPERATOR's side: creating a PROJECT identity, creating the
//     ROOT assignment, and recording an OPERATOR APPROVAL.
//
// # The operator's side is a genuine capability boundary WITHIN THE
// DISPATCH SURFACE
//
// A project is the identity every root must name; a root is the work
// order that everything else inherits its budgets from; and an approval
// is the operator's statement that something was authorised. An agent
// that could reach any of them through the agent-facing surface could:
//
//   - invent a project identity to write into,
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
//   - no action of any registry tool creates a project, creates a root
//     or records an approval, under any payload shape;
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
// `mpm assignment create-project`, `mpm assignment create-root` and
// `mpm assignment record-approval` verbs — so the agent-facing surface
// can simply NOT HAVE the capability.
//
// "Absent" is a stronger property than "refused": there is no action to
// dispatch to, no schema to advertise, and no payload field that could
// unlock it.
//
// # The contract these tests enforce
//
//   - NO registry action can create a project or a root.
//   - NO registry action can record an approval.
//   - NO payload field unlocks any of them — not `confirm`, not
//     `operator`, not `authority`, not `force`, not `approve`.
//   - The advertised action vocabulary names none.
//   - The agent-facing surface still does the work it is supposed to
//     do, so the boundary is not enforced by removing the capability.
//   - The operator's own path still works, so the capability is
//     preserved rather than deleted.
//
// The operator's own path is proved in cmd/mpm (assignment_cmds_test.go),
// which drives the CLI verbs end to end.
package tools

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	mpminternal "github.com/flowbyte-com/mpm-core"
	"github.com/flowbyte-com/mpm-core/assignment"
)

// operatorOnlyAssignmentActions are the operator-only operations the
// agent-facing surface must not be able to reach, under ANY action name.
var operatorOnlyAssignmentActions = []string{
	"create-project",
	"create_project",
	"createproject",
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
	// THE VALID VOCABULARY, NOT THE FORBIDDEN NAMES. The point is that no
	// payload field turns a VALID agent action into an operator one.
	enum := schemaEnum(t, "mpm_assignment")
	if len(enum) == 0 {
		t.Fatal("mpm_assignment has no advertised actions")
	}
	for _, tc := range operatorOnlyAssignmentPayloads {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			dm := newTestDMForTools(t)
			for _, action := range enum {
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
				// The call may be legal for the action (for example
				// `inbox` with no matching work). What it must never do
				// is write a project, a root or an approval.
				_, _ = handleMpmAssignment(dm, mpminternal.ActiveContext{},
					map[string]interface{}{
						"action": action,
						"params": params,
					})
			}
			projects := tableCount(t, dm, `SELECT COUNT(*) FROM projects`)
			roots := tableCount(t, dm, `SELECT COUNT(*) FROM agent_tasks WHERE depth = 0`)
			tasks := tableCount(t, dm, `SELECT COUNT(*) FROM agent_tasks`)
			if projects != 0 || roots != 0 || tasks != 0 {
				t.Errorf("the payload %v unlocked an operator write: projects=%d roots=%d tasks=%d",
					tc.extra, projects, roots, tasks)
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

	// T84/A2: the verdict must name HOST-captured evidence. The host
	// captures it through the supported path (a real findings file and
	// its digest); the agent surface then records the verdict.
	findings := []byte(`{"findings":"the child is correct"}`)
	findingsPath := filepath.Join(t.TempDir(), "findings.json")
	if err := os.WriteFile(findingsPath, findings, 0o600); err != nil {
		t.Fatalf("write the findings: %v", err)
	}
	sum := sha256.Sum256(findings)
	if _, err := store.CaptureReviewEvidence(assignment.ReviewEvidenceParams{
		ReviewerID:     reviewing.ID,
		Epoch:          reviewing.ClaimEpoch,
		ReviewedSHA:    strings.Repeat("a", 40),
		FindingsSHA256: hex.EncodeToString(sum[:]),
		EvidenceRef:    findingsPath,
		Actor:          assignment.Actor{Address: "charlie", Kind: "agent"},
	}); err != nil {
		t.Fatalf("capture the review evidence: %v", err)
	}

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
//
// A2: completion requires a CURRENT-EPOCH attempt at `completed` with
// `runtime_outcome='succeeded'`. The fixture proves that evidence the
// way the adapter does — begin the attempt, record the runtime outcome,
// then Complete — rather than completing a bare row. The completion
// guard is not weakened to keep a fixture short.
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
	att, err := store.BeginAttempt(assignment.BeginAttemptParams{
		TaskID: cur.ID, Epoch: cur.ClaimEpoch, Runtime: assignment.RuntimeOpenCode,
		IntendedAgent: "mpm-worker", NativeSessionID: "ses_boundary_fixture_" + cur.ID,
		CorrelationRef: "corr-boundary-fixture-" + cur.ID,
		WorkspaceID:    "ws", WorkspacePath: "/tmp/ws-boundary-fixture",
	})
	if err != nil {
		t.Fatalf("begin the fixture attempt for %s: %v", cur.ID, err)
	}
	if _, err := store.MarkAttemptCompleted(assignment.SessionStateParams{
		AttemptID: att.ID, Epoch: cur.ClaimEpoch, Outcome: "succeeded",
	}); err != nil {
		t.Fatalf("complete the fixture attempt for %s: %v", cur.ID, err)
	}
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

// tableCount is one table's row count.
func tableCount(t *testing.T, dm *mpminternal.DatabaseManager, query string) int {
	t.Helper()
	var n int
	if err := dm.SQLDB().QueryRow(query).Scan(&n); err != nil {
		t.Fatalf("count (%s): %v", query, err)
	}
	return n
}

// TestAssignment_NoRegistryActionCreatesAProject proves the project
// boundary by name: no action of any registry tool can create a project,
// and the advertised vocabulary names none.
func TestAssignment_NoRegistryActionCreatesAProject(t *testing.T) {
	names := []string{"create-project", "create_project", "createproject"}
	for _, tool := range Registry {
		tool := tool
		t.Run(tool.Name, func(t *testing.T) {
			dm := newTestDMForTools(t)
			before := tableCount(t, dm, `SELECT COUNT(*) FROM projects`)
			for _, action := range names {
				_, err := tool.Handler(dm, mpminternal.ActiveContext{},
					map[string]interface{}{
						"action": action,
						"params": map[string]interface{}{"id": "cpj-0011223344556677"},
					})
				if err == nil {
					t.Errorf("the %s tool accepted %q; a project is operator-created only",
						tool.Name, action)
				}
			}
			if after := tableCount(t, dm, `SELECT COUNT(*) FROM projects`); after != before {
				t.Errorf("the %s tool wrote %d project row(s) while refusing", tool.Name, after-before)
			}
		})
	}
	enum := schemaEnum(t, "mpm_assignment")
	if enum == nil {
		t.Fatal("mpm_assignment has no action enum; cannot verify the advertised vocabulary")
	}
	for _, action := range enum {
		for _, forbidden := range names {
			if action == forbidden {
				t.Errorf("the advertised vocabulary offers the operator-only action %q", action)
			}
		}
	}
}

// TestAssignmentReadSurfacesCarryExecutionStateIsF9 proves the
// AGENT-facing read surface exposes the same derived execution authority
// the CLI does: an agent reading its own assignment sees that it is
// superseded or recovery-required, not merely its stored state.
func TestAssignmentReadSurfacesCarryExecutionStateIsF9(t *testing.T) {
	dm := newTestDMForTools(t)
	if _, err := dm.SQLDB().Exec(
		`INSERT INTO projects (id, created_by) VALUES ('cpj-agent-readstate', 'boundary-test')`); err != nil {
		t.Fatalf("seed the project: %v", err)
	}
	// A controllable clock: the supersession requires the original's
	// lease to have expired, and sleeping for it would make the test
	// slow for no benefit.
	now := time.Date(2026, 11, 26, 9, 0, 0, 0, time.UTC)
	store := assignment.NewStoreWithClock(dm, func() time.Time { return now })
	auth := assignment.NewLocalOperatorAuthority("boundary-test")
	root, _, err := store.CreateRoot(auth, assignment.CreateRootParams{
		ProjectID: "cpj-agent-readstate", Title: "root", Objective: "o", Recipient: "alpha",
		IdempotencyKey: "idem:f9:root", PermittedOperations: []string{"read"},
		EligibleRecipients: []string{"alpha", "beta"}, MaxDepth: 3, MaxTasks: 16,
		MaxConcurrency: 2, MaxResourceUnits: 100,
	})
	if err != nil {
		t.Fatalf("create the root: %v", err)
	}
	child, _, err := store.Delegate(assignment.DelegateParams{
		ParentID: root.ID, Title: "child", Objective: "o", Recipient: "alpha",
		Role: assignment.RoleImplementer, IdempotencyKey: "idem:f9:child",
		Actor: auth.Actor(),
	})
	if err != nil {
		t.Fatalf("delegate: %v", err)
	}
	actor := assignment.Actor{Address: "alpha", Kind: "agent"}
	for _, step := range []func(assignment.FencedParams) (*assignment.Assignment, error){
		store.Deliver, store.Acknowledge, store.Accept,
	} {
		if _, err := step(assignment.FencedParams{ID: child.ID, Actor: actor}); err != nil {
			t.Fatalf("advance: %v", err)
		}
	}
	claimed, err := store.Claim(assignment.ClaimParams{
		ID: child.ID, ExpectedEpoch: 0, Owner: "alpha", LeaseSeconds: 600, Actor: actor,
	})
	if err != nil {
		t.Fatalf("claim: %v", err)
	}
	if _, err := store.Start(assignment.FencedParams{
		ID: child.ID, Epoch: claimed.ClaimEpoch, Actor: actor}); err != nil {
		t.Fatalf("start: %v", err)
	}
	att, err := store.BeginAttempt(assignment.BeginAttemptParams{
		TaskID: child.ID, Epoch: claimed.ClaimEpoch, Runtime: assignment.RuntimeOpenCode,
		IntendedAgent: "mpm-worker", NativeSessionID: "ses-f9",
		CorrelationRef: "corr-f9", WorkspaceID: "ws", WorkspacePath: "/tmp/ws"})
	if err != nil {
		t.Fatalf("begin attempt: %v", err)
	}
	if _, err := store.MarkAttemptAmbiguous(assignment.SessionStateParams{
		AttemptID: att.ID, Epoch: claimed.ClaimEpoch, Error: "lost"}); err != nil {
		t.Fatalf("mark ambiguous: %v", err)
	}

	// show: the derived state distinguishes "stored in_progress" from
	// "cannot proceed".
	shownRaw, err := handleMpmAssignment(dm, mpminternal.ActiveContext{}, map[string]interface{}{
		"action": "show", "params": map[string]interface{}{"id": child.ID},
	})
	if err != nil {
		t.Fatalf("show: %v", err)
	}
	shown, ok := shownRaw.(*assignment.AssignmentView)
	if !ok {
		t.Fatalf("show returned %T, want the execution-aware view", shownRaw)
	}
	if shown.State != assignment.StateInProgress {
		t.Fatalf("the stored state is %q, want in_progress", shown.State)
	}
	if shown.EffectiveState != "recovery_required" {
		t.Fatalf("effective_state=%q, want recovery_required", shown.EffectiveState)
	}

	// tree: the same statuses, per task.
	treeRaw, err := handleMpmAssignment(dm, mpminternal.ActiveContext{}, map[string]interface{}{
		"action": "tree", "params": map[string]interface{}{"root_id": root.ID},
	})
	if err != nil {
		t.Fatalf("tree: %v", err)
	}
	tree, ok := treeRaw.(*assignment.TreeView)
	if !ok {
		t.Fatalf("tree returned %T, want the tree view", treeRaw)
	}
	if tree.Tree == "" || tree.Recovery == "" {
		t.Fatalf("the tree carries no derived status: %+v", tree)
	}
	if tree.Count != len(tree.Assignments) {
		t.Fatalf("tree count %d, assignments %d", tree.Count, len(tree.Assignments))
	}
	found := false
	for _, a := range tree.Assignments {
		if a.ID != child.ID {
			continue
		}
		found = true
		if a.EffectiveState != "recovery_required" {
			t.Fatalf("tree item effective_state=%q, want recovery_required", a.EffectiveState)
		}
	}
	if !found {
		t.Fatalf("the tree does not list the child")
	}

	// After an operator supersession the same read says `superseded`.
	now = now.Add(2 * time.Hour)
	if _, err := store.Supersede(auth, assignment.SupersedeParams{
		SupersededID: child.ID, AttemptID: att.ID, Epoch: claimed.ClaimEpoch,
		Replacement: assignment.SupersedeTaskParams{
			Title: "redo", Objective: "o", Recipient: "beta",
			ResourceUnits: 1, BaseSHA: strings.Repeat("a", 40)},
		Reviewer: assignment.SupersedeTaskParams{
			Title: "review", Objective: "o", Recipient: "alpha", ResourceUnits: 1},
		EvidenceRef: "gh:test/f9#1", Justification: "fixture",
	}); err != nil {
		t.Fatalf("supersede: %v", err)
	}
	afterRaw, err := handleMpmAssignment(dm, mpminternal.ActiveContext{}, map[string]interface{}{
		"action": "show", "params": map[string]interface{}{"id": child.ID},
	})
	if err != nil {
		t.Fatalf("show after supersession: %v", err)
	}
	after := afterRaw.(*assignment.AssignmentView)
	if after.EffectiveState != "superseded" {
		t.Fatalf("effective_state=%q after supersession, want superseded", after.EffectiveState)
	}
}
