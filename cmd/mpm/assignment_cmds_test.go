// assignment_cmds_test.go — the operator's own CLI path.
//
// # WHY THIS FILE EXISTS
//
// The boundary test in internal/core/tools proves that no AGENT-facing
// action can create a root or record an approval. This file proves the
// OTHER half of the same claim: the operator's own verbs WORK.
//
// A boundary that only has a "no" is not a boundary, it is a deletion.
// The operator must still be able to create the root and record the
// approval — through the CLI, end to end — or the capability has been
// removed rather than separated.
//
// The tests here drive the real router against a DISPOSABLE workspace:
// MPM_WORKSPACE points at a temp directory and the process-wide
// database singleton is reset, so nothing here can reach a real
// workspace.

package main

import (
	"bytes"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/flowbyte-com/mpm-core"
	"github.com/flowbyte-com/mpm-core/assignment"
	"github.com/flowbyte-com/mpm-core/usererror"
)

// runAssignmentCLI runs the CLI with the user-facing output captured.
//
// The usererror package binds os.Stderr once, at init, so swapping
// os.Stderr at test time does NOT redirect it — the refusals would go to
// the real stderr and the test would see nothing.
//
// It returns the exit code, the command's own output, and the
// user-facing refusals.
func runAssignmentCLI(t *testing.T, argv ...string) (int, string, string) {
	t.Helper()
	var buf bytes.Buffer
	prev := usererror.GetWriter()
	usererror.SetWriter(&buf)
	t.Cleanup(func() { usererror.SetWriter(prev) })
	code, out := runCLI(t, argv...)
	return code, out, buf.String()
}

// assignmentCLIState is the shape of the CLI's --json output.
type assignmentCLIState struct {
	ID         string                 `json:"id"`
	State      string                 `json:"state"`
	ClaimEpoch int64                  `json:"claim_epoch"`
	RootID     string                 `json:"root_id"`
	ParentID   string                 `json:"parent_id"`
	Depth      int                    `json:"depth"`
	Payload    map[string]interface{} `json:"payload"`
}

// assignmentCLIFixture points the CLI at a disposable workspace and
// returns its database.
func assignmentCLIFixture(t *testing.T) *internal.DatabaseManager {
	t.Helper()
	t.Setenv("MPM_WORKSPACE", t.TempDir())
	resetDBSingleton(t)
	dm := getDBConcrete()
	if dm == nil {
		t.Fatal("the disposable workspace database did not open")
	}
	if _, err := dm.SQLDB().Exec(
		`INSERT INTO projects (id, created_by) VALUES ('cpj-cli', 'cli-test')`); err != nil {
		t.Fatalf("seed the project: %v", err)
	}
	return dm
}

// TestAssignmentCLITheListFlagsAccumulate proves that a REPEATED
// list-valued flag does not lose values.
//
// Go's flag package overwrites a string flag's value on every Set, so
// the obvious way to write six permitted operations —
// `--permitted-operation deliver --permitted-operation accept ...` —
// silently kept only the LAST one. The root was then created with a
// permitted set of one, and the work order could not be delivered.
//
// The failure was in the safe direction (a lost value narrows what the
// root permits and never widens it), but a silent narrowing is still a
// defect: the operator asked for six and got one.
//
// So the flag accumulates, and both the comma-separated form and the
// repeated form work — and neither loses the other's values.
func TestAssignmentCLITheListFlagsAccumulate(t *testing.T) {
	dm := assignmentCLIFixture(t)
	_ = dm

	// The REPEATED form.
	code, out, errOut := runAssignmentCLI(t,
		"assignment", "create-root",
		"--title", "the repeated form",
		"--objective", "six permitted operations",
		"--actor", "operator",
		"--actor-kind", "operator",
		"--project-id", "cpj-cli",
		"--idempotency-key", "cli-list-repeated",
		"--recipient", "worker-a",
		"--eligible-recipient", "worker-a",
		"--eligible-recipient", "worker-b",
		"--permitted-operation", "deliver",
		"--permitted-operation", "acknowledge",
		"--permitted-operation", "accept",
		"--permitted-operation", "claim",
		"--permitted-operation", "start",
		"--permitted-operation", "complete",
		"--max-depth", "2",
		"--max-tasks", "4",
		"--max-concurrency", "2",
		"--max-resource-units", "10",
		"--json")
	if code != 0 {
		t.Fatalf("create-root: exit %d\n%s\n%s", code, out, errOut)
	}

	var state assignmentCLIState
	if err := json.Unmarshal([]byte(out), &state); err != nil {
		t.Fatalf("the output is not JSON: %v\n%s", err, out)
	}
	if state.ID == "" {
		t.Fatalf("the root has no id: %s", out)
	}

	_, show, _ := runAssignmentCLI(t, "assignment", "show", "--id", state.ID, "--json")
	var shown struct {
		PermittedOperations []string `json:"permitted_operations"`
		EligibleRecipients  []string `json:"eligible_recipients"`
	}
	if err := json.Unmarshal([]byte(show), &shown); err != nil {
		t.Fatalf("the show output is not JSON: %v\n%s", err, show)
	}
	wantPermitted := []string{"deliver", "acknowledge", "accept", "claim", "start", "complete"}
	if len(shown.PermittedOperations) != len(wantPermitted) {
		t.Fatalf("the repeated form kept %d permitted operation(s) (%v), want %d (%v) — "+
			"a repeated flag silently lost values",
			len(shown.PermittedOperations), shown.PermittedOperations,
			len(wantPermitted), wantPermitted)
	}
	for i, want := range wantPermitted {
		if shown.PermittedOperations[i] != want {
			t.Errorf("permitted operation %d is %q, want %q (the whole list: %v)",
				i, shown.PermittedOperations[i], want, shown.PermittedOperations)
		}
	}
	if len(shown.EligibleRecipients) != 2 {
		t.Errorf("the repeated form kept %d eligible recipient(s) (%v), want 2",
			len(shown.EligibleRecipients), shown.EligibleRecipients)
	}

	// The COMMA-SEPARATED form, and the two forms together: neither
	// loses the other's values.
	code, out, errOut = runAssignmentCLI(t,
		"assignment", "create-root",
		"--title", "the mixed form",
		"--objective", "a comma-separated list and a repeated flag",
		"--actor", "operator",
		"--actor-kind", "operator",
		"--project-id", "cpj-cli",
		"--idempotency-key", "cli-list-mixed",
		"--recipient", "worker-a",
		"--eligible-recipient", "worker-a,worker-b",
		"--eligible-recipient", "worker-c",
		"--permitted-operation", "deliver,acknowledge",
		"--permitted-operation", "accept",
		"--max-depth", "2",
		"--max-tasks", "4",
		"--max-concurrency", "2",
		"--max-resource-units", "10",
		"--json")
	if code != 0 {
		t.Fatalf("create-root (mixed): exit %d\n%s\n%s", code, out, errOut)
	}
	var mixed struct {
		ID string `json:"id"`
	}
	if err := json.Unmarshal([]byte(out), &mixed); err != nil {
		t.Fatalf("the output is not JSON: %v\n%s", err, out)
	}
	_, show, _ = runAssignmentCLI(t, "assignment", "show", "--id", mixed.ID, "--json")
	if err := json.Unmarshal([]byte(show), &shown); err != nil {
		t.Fatalf("the show output is not JSON: %v\n%s", err, show)
	}
	wantMixed := []string{"deliver", "acknowledge", "accept"}
	if len(shown.PermittedOperations) != len(wantMixed) {
		t.Fatalf("the mixed form kept %v, want %v — one form lost the other's values",
			shown.PermittedOperations, wantMixed)
	}
	for i, want := range wantMixed {
		if shown.PermittedOperations[i] != want {
			t.Errorf("permitted operation %d is %q, want %q (the whole list: %v)",
				i, shown.PermittedOperations[i], want, shown.PermittedOperations)
		}
	}
	wantRecipients := []string{"worker-a", "worker-b", "worker-c"}
	if len(shown.EligibleRecipients) != len(wantRecipients) {
		t.Fatalf("the mixed form kept %v, want %v", shown.EligibleRecipients, wantRecipients)
	}
	for i, want := range wantRecipients {
		if shown.EligibleRecipients[i] != want {
			t.Errorf("eligible recipient %d is %q, want %q (the whole list: %v)",
				i, shown.EligibleRecipients[i], want, shown.EligibleRecipients)
		}
	}
}

// TestAssignmentCLITheOperatorCanCreateARoot proves the operator's own
// front door creates a root, end to end through the router.
func TestAssignmentCLITheOperatorCanCreateARoot(t *testing.T) {
	assignmentCLIFixture(t)

	code, out := runCLI(t, "assignment", "create-root",
		"--project-id", "cpj-cli",
		"--title", "the root",
		"--objective", "the objective",
		"--recipient", "alpha",
		"--idempotency-key", "idem:cli:root",
		"--permitted-operation", "read,write",
		"--eligible-recipient", "alpha,beta",
		"--max-depth", "2",
		"--max-tasks", "8",
		"--max-concurrency", "2",
		"--max-resource-units", "100",
		"--json",
	)
	if code != 0 {
		t.Fatalf("the operator's create-root exited %d: %s", code, out)
	}

	var created assignmentCLIState
	if err := json.Unmarshal([]byte(out), &created); err != nil {
		t.Fatalf("the create-root output is not JSON: %v\n%s", err, out)
	}
	if created.ID == "" {
		t.Fatalf("the create-root output names no assignment: %s", out)
	}
	if created.State != assignment.StateEnqueued {
		t.Errorf("the root is %q, want %q", created.State, assignment.StateEnqueued)
	}
	if created.Depth != 0 {
		t.Errorf("the root is at depth %d, want 0", created.Depth)
	}
	if created.ParentID != "" {
		t.Errorf("the root names the parent %q, want none", created.ParentID)
	}
	if created.RootID != created.ID {
		t.Errorf("the root's tree is %q, want its own id %q", created.RootID, created.ID)
	}
}

// assignmentCLIGatedSubject builds an assignment that is AT the
// operator's gate.
//
// The work, the review and the hand-off are built with the service,
// because those are the AGENT's steps. What the operator's verbs then
// do with the result is the thing under test.
//
// It has to be a real gate: the state check comes BEFORE the fence in
// RecordApproval, so an assignment that is not at the gate would be
// refused by the state check and the fence would never be consulted.
func assignmentCLIGatedSubject(t *testing.T, store *assignment.Store, key string) *assignment.Assignment {
	t.Helper()

	// The work: a root, a child that finishes, an independent reviewer
	// that passes it, and the hand-off to the operator's gate.
	root, created, err := store.CreateRoot(
		assignment.NewLocalOperatorAuthority("operator"),
		assignment.CreateRootParams{
			ProjectID:           "cpj-cli",
			Title:               "the root",
			Objective:           "the objective",
			Recipient:           "alpha",
			IdempotencyKey:      "idem:cli:" + key + ":root",
			PermittedOperations: []string{"read", "write"},
			EligibleRecipients:  []string{"alpha", "charlie"},
			MaxDepth:            2,
			MaxTasks:            8,
			MaxConcurrency:      2,
			MaxResourceUnits:    100,
		})
	if err != nil || !created {
		t.Fatalf("create the root: %v (created=%v)", err, created)
	}

	subject, created, err := store.Delegate(assignment.DelegateParams{
		ParentID:       root.ID,
		Title:          "the subject",
		Objective:      "the work",
		Recipient:      "alpha",
		Role:           assignment.RoleImplementer,
		IdempotencyKey: "idem:cli:" + key + ":subject",
	})
	if err != nil || !created {
		t.Fatalf("delegate the subject: %v (created=%v)", err, created)
	}
	subject = cliAdvance(t, store, subject, assignment.StateCompleted)

	reviewer, created, err := store.Delegate(assignment.DelegateParams{
		ParentID:       root.ID,
		Title:          "the reviewer",
		Objective:      "review the work",
		Recipient:      "charlie",
		Role:           assignment.RoleReviewer,
		IdempotencyKey: "idem:cli:" + key + ":reviewer",
		Payload:        map[string]interface{}{assignment.PayloadKeyReviewTarget: subject.ID},
	})
	if err != nil || !created {
		t.Fatalf("delegate the reviewer: %v (created=%v)", err, created)
	}
	reviewing := cliAdvance(t, store, reviewer, assignment.StateInProgress)

	// TR18: a verdict needs host-captured evidence. The fixture records
	// it the way the adapter would before transcribing the verdict.
	if _, err := store.CaptureReviewEvidence(assignment.ReviewEvidenceParams{
		ReviewerID: reviewing.ID, Epoch: reviewing.ClaimEpoch,
		ReviewedSHA: strings.Repeat("1", 40), FindingsSHA256: strings.Repeat("e", 64),
		EvidenceRef: "evidence/cli.json", Actor: assignment.Actor{Address: "charlie", Kind: "agent"},
	}); err != nil {
		t.Fatalf("capture the review evidence: %v", err)
	}
	if _, err := store.RecordReviewResult(assignment.ReviewResultParams{
		ReviewerID: reviewing.ID,
		Verdict:    assignment.VerdictPassed,
		Epoch:      reviewing.ClaimEpoch,
		Actor:      assignment.Actor{Address: "charlie", Kind: "agent"},
	}); err != nil {
		t.Fatalf("record the verdict: %v", err)
	}
	cur, err := store.Get(subject.ID)
	if err != nil {
		t.Fatalf("read the subject: %v", err)
	}
	gated, err := store.HandoffToOperator(assignment.HandoffParams{
		ID: cur.ID, Epoch: cur.ClaimEpoch, AwaitingAction: "merge PR #260",
		Actor: assignment.Actor{Address: "alpha", Kind: "agent"},
	})
	if err != nil {
		t.Fatalf("hand the work to the gate: %v", err)
	}
	if gated.State != assignment.StateAwaitingOperator {
		t.Fatalf("the assignment is %q, want %q — the fence must be reachable",
			gated.State, assignment.StateAwaitingOperator)
	}
	return gated
}

// TestAssignmentCLITheOperatorCanRecordAnApproval proves the operator's
// own front door records an approval, end to end through the router.
//
// The fixture — the work, the review and the hand-off — is built with
// the service, because those are the AGENT's steps. The thing under
// test is the operator's VERB.
func TestAssignmentCLITheOperatorCanRecordAnApproval(t *testing.T) {
	dm := assignmentCLIFixture(t)
	store := assignment.NewStore(dm)
	gated := assignmentCLIGatedSubject(t, store, "approval")

	// The operator's verb.
	code, out := runCLI(t, "assignment", "record-approval",
		"--id", gated.ID,
		"--action", "merge PR #260",
		"--revision", "abc123",
		"--source", "operator",
		"--evidence", "ev://the-approval",
		"--epoch", "1",
		"--json",
	)
	if code != 0 {
		t.Fatalf("the operator's record-approval exited %d: %s", code, out)
	}
	var approved assignmentCLIState
	if err := json.Unmarshal([]byte(out), &approved); err != nil {
		t.Fatalf("the record-approval output is not JSON: %v\n%s", err, out)
	}
	if approved.State != assignment.StateOperatorApproved {
		t.Errorf("the assignment is %q after the approval, want %q",
			approved.State, assignment.StateOperatorApproved)
	}

	// And the row records the operator's claims.
	after, err := store.Get(gated.ID)
	if err != nil {
		t.Fatalf("read the assignment: %v", err)
	}
	if after.ApprovalAction != "merge PR #260" {
		t.Errorf("the approval names the action %q, want %q", after.ApprovalAction, "merge PR #260")
	}
	if after.ApprovalRevision != "abc123" {
		t.Errorf("the approval names the revision %q, want %q", after.ApprovalRevision, "abc123")
	}
	if after.ApprovalEvidence != "ev://the-approval" {
		t.Errorf("the approval names the evidence %q, want %q",
			after.ApprovalEvidence, "ev://the-approval")
	}
}

// TestAssignmentCLITheApprovalRefusesAStaleEpoch proves the operator's
// verb is a FENCED write like every other one.
//
// The operator read the assignment at one epoch and records the
// approval at another: the approval does not land.
//
// The fixture is an assignment that is AT the gate, because the state
// check comes before the fence: an assignment that was not at the gate
// would be refused by the state check, and the fence would never be
// consulted — the test would pass without ever reaching it.
func TestAssignmentCLITheApprovalRefusesAStaleEpoch(t *testing.T) {
	dm := assignmentCLIFixture(t)
	store := assignment.NewStore(dm)
	gated := assignmentCLIGatedSubject(t, store, "stale")

	// The operator names an epoch the assignment is not at. The verb
	// refuses, and says so.
	code, out, refusals := runAssignmentCLI(t, "assignment", "record-approval",
		"--id", gated.ID,
		"--action", "merge PR #260",
		"--revision", "abc123",
		"--source", "operator",
		"--evidence", "ev://the-approval",
		"--epoch", "7",
	)
	if code == 0 {
		t.Fatalf("the approval presenting a stale epoch was accepted: %s", out)
	}
	if !strings.Contains(refusals, "stale") {
		t.Errorf("the refusal is not the fence's: %s", refusals)
	}

	// And nothing moved: the assignment is still at the gate.
	after, err := store.Get(gated.ID)
	if err != nil {
		t.Fatalf("read the assignment: %v", err)
	}
	if after.State != assignment.StateAwaitingOperator {
		t.Errorf("the assignment is %q after the refused approval, want %q",
			after.State, assignment.StateAwaitingOperator)
	}
	if after.ApprovedAt != nil {
		t.Errorf("the refused approval recorded a moment: %v", after.ApprovedAt)
	}
}

// TestAssignmentCLITheApprovalRefusesAnOmittedEpoch proves the omission
// is called out.
//
// An approval that names NO epoch names no revision of the assignment,
// and the refusal says so — and names the epoch the assignment is
// actually at, so the operator can re-read and retry.
func TestAssignmentCLITheApprovalRefusesAnOmittedEpoch(t *testing.T) {
	dm := assignmentCLIFixture(t)
	store := assignment.NewStore(dm)
	gated := assignmentCLIGatedSubject(t, store, "omitted")

	// No --epoch at all.
	code, out, refusals := runAssignmentCLI(t, "assignment", "record-approval",
		"--id", gated.ID,
		"--action", "merge PR #260",
		"--revision", "abc123",
		"--source", "operator",
		"--evidence", "ev://the-approval",
	)
	if code == 0 {
		t.Fatalf("the approval naming no epoch was accepted: %s", out)
	}
	if !strings.Contains(refusals, "must name the claim epoch") {
		t.Errorf("the refusal does not say the epoch was not named: %s", refusals)
	}
	if !strings.Contains(refusals, "epoch 1") {
		t.Errorf("the refusal does not name the epoch the assignment is at: %s", refusals)
	}
}

// TestAssignmentCLIPayloadCarriesTheReviewersTarget proves the
// operator's CLI can build the reviewer its own design requires.
//
// The reviewer's target is fixed by the delegation that creates the
// reviewer, and that delegation carries a PAYLOAD. Without a way to set
// a payload, the operator's front door could not create a reviewer at
// all — the authority split would be inverted for the one part of the
// tree that must be created deliberately.
func TestAssignmentCLIPayloadCarriesTheReviewersTarget(t *testing.T) {
	assignmentCLIFixture(t)

	code, out := runCLI(t, "assignment", "create-root",
		"--project-id", "cpj-cli",
		"--title", "the root",
		"--objective", "the objective",
		"--recipient", "alpha",
		"--idempotency-key", "idem:cli:payload:root",
		"--permitted-operation", "read",
		"--eligible-recipient", "alpha",
		"--max-depth", "1",
		"--max-tasks", "2",
		"--max-concurrency", "1",
		"--max-resource-units", "10",
		"--payload", `{"review_target":"at-0123456789abcdef"}`,
		"--json",
	)
	if code != 0 {
		t.Fatalf("the create-root with a payload exited %d: %s", code, out)
	}
	var created assignmentCLIState
	if err := json.Unmarshal([]byte(out), &created); err != nil {
		t.Fatalf("the create-root output is not JSON: %v\n%s", err, out)
	}
	if created.Payload["review_target"] != "at-0123456789abcdef" {
		t.Errorf("the payload is %v, want the review target", created.Payload)
	}
}

// TestAssignmentCLIMalformedPayloadIsRefused proves a payload the
// operator got wrong is an ERROR, not a silent empty object.
//
// A caller that meant to name a review target and spelled it wrong
// must not end up with an assignment that reviews nothing.
func TestAssignmentCLIMalformedPayloadIsRefused(t *testing.T) {
	assignmentCLIFixture(t)

	code, out, refusals := runAssignmentCLI(t, "assignment", "create-root",
		"--project-id", "cpj-cli",
		"--title", "the root",
		"--objective", "the objective",
		"--recipient", "alpha",
		"--idempotency-key", "idem:cli:bad-payload:root",
		"--permitted-operation", "read",
		"--eligible-recipient", "alpha",
		"--max-depth", "1",
		"--max-tasks", "2",
		"--max-concurrency", "1",
		"--max-resource-units", "10",
		"--payload", "{not json",
	)
	if code == 0 {
		t.Fatalf("a malformed payload was accepted: %s", out)
	}
	if !strings.Contains(refusals, "--payload") {
		t.Errorf("the refusal does not name the flag at fault: %s", refusals)
	}
}

// cliAdvance drives an assignment through the implementer's path to the
// given state, through the service.
func cliAdvance(t *testing.T, store *assignment.Store, a *assignment.Assignment, to string) *assignment.Assignment {
	t.Helper()
	actor := assignment.Actor{Address: a.Recipient, Kind: "agent"}
	step := func(op func() (*assignment.Assignment, error)) *assignment.Assignment {
		t.Helper()
		out, err := op()
		if err != nil {
			t.Fatalf("advance %s: %v", a.ID, err)
		}
		return out
	}
	cur := step(func() (*assignment.Assignment, error) {
		return store.Deliver(assignment.FencedParams{ID: a.ID, Epoch: a.ClaimEpoch, Actor: actor})
	})
	cur = step(func() (*assignment.Assignment, error) {
		return store.Acknowledge(assignment.FencedParams{ID: cur.ID, Epoch: cur.ClaimEpoch, Actor: actor})
	})
	cur = step(func() (*assignment.Assignment, error) {
		return store.Accept(assignment.FencedParams{ID: cur.ID, Epoch: cur.ClaimEpoch, Actor: actor})
	})
	cur = step(func() (*assignment.Assignment, error) {
		return store.Claim(assignment.ClaimParams{
			ID: cur.ID, ExpectedEpoch: cur.ClaimEpoch, Owner: cur.Recipient, LeaseSeconds: 600,
			Actor: actor,
		})
	})
	if to == assignment.StateInProgress {
		return step(func() (*assignment.Assignment, error) {
			return store.Start(assignment.FencedParams{ID: cur.ID, Epoch: cur.ClaimEpoch, Actor: actor})
		})
	}
	cur = step(func() (*assignment.Assignment, error) {
		return store.Start(assignment.FencedParams{ID: cur.ID, Epoch: cur.ClaimEpoch, Actor: actor})
	})
	// TR13: completion needs a current-epoch completed+succeeded
	// attempt. The fixture proves one rather than completing bare.
	att, err := store.BeginAttempt(assignment.BeginAttemptParams{
		TaskID: cur.ID, Epoch: cur.ClaimEpoch, Runtime: assignment.RuntimeOpenCode,
		IntendedAgent: "mpm-worker", NativeSessionID: "ses-cli-" + cur.ID,
		CorrelationRef: "corr-cli", WorkspaceID: "ws", WorkspacePath: "/tmp/ws",
	})
	if err != nil {
		t.Fatalf("begin the completion attempt: %v", err)
	}
	if _, err := store.MarkAttemptCompleted(assignment.SessionStateParams{
		AttemptID: att.ID, Epoch: cur.ClaimEpoch, Outcome: "succeeded"}); err != nil {
		t.Fatalf("complete the completion attempt: %v", err)
	}
	return step(func() (*assignment.Assignment, error) {
		return store.Complete(assignment.FencedParams{ID: cur.ID, Epoch: cur.ClaimEpoch, Actor: actor})
	})
}

// TestAssignmentCLITheOperatorCanCreateAProject proves the operator's own
// front door creates a project, end to end through the router, and that
// the project can then hold a root.
func TestAssignmentCLITheOperatorCanCreateAProject(t *testing.T) {
	assignmentCLIFixture(t)
	id := "cpj-0011223344556677"

	code, out := runCLI(t, "assignment", "create-project", "--id", id, "--json")
	if code != 0 {
		t.Fatalf("the operator's create-project exited %d: %s", code, out)
	}
	var created struct {
		ID      string `json:"id"`
		Created bool   `json:"created"`
	}
	if err := json.Unmarshal([]byte(out), &created); err != nil {
		t.Fatalf("the create-project output is not JSON: %v\n%s", err, out)
	}
	if created.ID != id {
		t.Errorf("the created project's id is %q, want %q", created.ID, id)
	}
	if !created.Created {
		t.Error("the first create-project reported created=false")
	}

	// THE REPEAT IS IDEMPOTENT.
	code, out = runCLI(t, "assignment", "create-project", "--id", id, "--json")
	if code != 0 {
		t.Fatalf("the repeated create-project exited %d: %s", code, out)
	}
	if err := json.Unmarshal([]byte(out), &created); err != nil {
		t.Fatalf("the repeated output is not JSON: %v\n%s", err, out)
	}
	if created.Created {
		t.Error("the repeated create-project reported created=true")
	}

	// AND THE PROJECT IS USABLE: a root can be created inside it.
	code, out = runCLI(t, "assignment", "create-root",
		"--project-id", id,
		"--title", "a root in the new project",
		"--objective", "the objective",
		"--recipient", "alpha",
		"--idempotency-key", "idem:cli:root:new-project",
		"--permitted-operation", "read",
		"--eligible-recipient", "alpha",
		"--max-depth", "1",
		"--max-tasks", "2",
		"--max-concurrency", "1",
		"--max-resource-units", "10",
		"--json",
	)
	if code != 0 {
		t.Fatalf("create-root in the new project exited %d: %s", code, out)
	}
}

// TestAssignmentCLICreateProjectRefusesANonIdentity proves the operator's
// verb validates the project model's shape.
func TestAssignmentCLICreateProjectRefusesANonIdentity(t *testing.T) {
	assignmentCLIFixture(t)
	for _, id := range []string{"", "pilot", "cpj-XYZ"} {
		code, out := runCLI(t, "assignment", "create-project", "--id", id, "--json")
		if code == 0 {
			t.Errorf("the invalid project id %q was accepted: %s", id, out)
		}
	}
}

// TestAssignmentCLITheOperatorCanSupersede proves the operator's recovery
// verb works end to end through the router: an ambiguous task is
// superseded, the lineage names it, and the replacement and reviewer
// start unclaimed while the original stands unchanged.
//
// The fixture — the work and its lost launch — is built with the
// service, because those are the runtime's steps. The thing under test
// is the operator's VERB. The claim uses a one-second lease and the test
// outlives it, so the supersession authorises against an expired lease
// on real time.
func TestAssignmentCLITheOperatorCanSupersede(t *testing.T) {
	dm := assignmentCLIFixture(t)
	store := assignment.NewStore(dm)
	auth := assignment.NewLocalOperatorAuthority("operator")
	root, created, err := store.CreateRoot(auth, assignment.CreateRootParams{
		ProjectID: "cpj-cli", Title: "the root", Objective: "the objective",
		Recipient: "alpha", IdempotencyKey: "idem:cli:sup:root",
		PermittedOperations: []string{"read", "write"},
		EligibleRecipients:  []string{"alpha", "charlie"},
		MaxDepth:            2, MaxTasks: 8, MaxConcurrency: 2, MaxResourceUnits: 100,
	})
	if err != nil || !created {
		t.Fatalf("create the root: %v (created=%v)", err, created)
	}
	subject, created, err := store.Delegate(assignment.DelegateParams{
		ParentID: root.ID, Title: "the work", Objective: "do it",
		Recipient: "alpha", Role: assignment.RoleImplementer,
		IdempotencyKey: "idem:cli:sup:subject",
	})
	if err != nil || !created {
		t.Fatalf("delegate the subject: %v (created=%v)", err, created)
	}
	subjectActor := assignment.Actor{Address: "alpha", Kind: "agent"}
	for _, step := range []func(assignment.FencedParams) (*assignment.Assignment, error){
		store.Deliver, store.Acknowledge, store.Accept,
	} {
		if _, err := step(assignment.FencedParams{ID: subject.ID, Actor: subjectActor}); err != nil {
			t.Fatalf("advance the subject: %v", err)
		}
	}
	claimed, err := store.Claim(assignment.ClaimParams{
		ID: subject.ID, ExpectedEpoch: 0, Owner: "alpha", LeaseSeconds: 1,
		Actor: subjectActor,
	})
	if err != nil {
		t.Fatalf("claim the subject: %v", err)
	}
	if _, err := store.Start(assignment.FencedParams{
		ID: subject.ID, Epoch: claimed.ClaimEpoch, Actor: subjectActor,
	}); err != nil {
		t.Fatalf("start the subject: %v", err)
	}
	att, err := store.BeginAttempt(assignment.BeginAttemptParams{
		TaskID: subject.ID, Epoch: claimed.ClaimEpoch, Runtime: assignment.RuntimeOpenCode,
		IntendedAgent: "mpm-worker", NativeSessionID: "ses-cli-sup",
		CorrelationRef: "corr-cli-sup", WorkspaceID: "ws", WorkspacePath: "/tmp/ws",
	})
	if err != nil {
		t.Fatalf("begin the attempt: %v", err)
	}
	if _, err := store.MarkAttemptAmbiguous(assignment.SessionStateParams{
		AttemptID: att.ID, Epoch: claimed.ClaimEpoch, Error: "lost"}); err != nil {
		t.Fatalf("mark the attempt ambiguous: %v", err)
	}
	time.Sleep(1200 * time.Millisecond)

	code, out, refusals := runAssignmentCLI(t, "assignment", "supersede",
		"--id", subject.ID,
		"--attempt", att.ID,
		"--epoch", "1",
		"--replacement-title", "redo the work",
		"--replacement-objective", "redo it",
		"--replacement-recipient", "beta",
		"--replacement-units", "1",
		"--base-sha", strings.Repeat("a", 40),
		"--reviewer-title", "review the redo",
		"--reviewer-objective", "review it",
		"--reviewer-recipient", "charlie",
		"--reviewer-units", "1",
		"--evidence-ref", "gh:flowbyte-com/mpm#258",
		"--justification", "the original launch is ambiguous",
		"--actor", "operator",
		"--json",
	)
	if code != 0 {
		t.Fatalf("the operator's supersede exited %d: %s\nrefusals: %s", code, out, refusals)
	}
	var res struct {
		Lineage struct {
			ID         string `json:"id"`
			Superseded string `json:"superseded_task_id"`
			Attempt    string `json:"attempt_id"`
		} `json:"lineage"`
		Replacement assignmentCLIState `json:"replacement"`
		Reviewer    assignmentCLIState `json:"reviewer"`
		Created     bool               `json:"created"`
	}
	if err := json.Unmarshal([]byte(out), &res); err != nil {
		t.Fatalf("the supersede output is not JSON: %v\n%s", err, out)
	}
	if !res.Created {
		t.Errorf("the supersession reports created=false")
	}
	if res.Lineage.Superseded != subject.ID || res.Lineage.Attempt != att.ID {
		t.Errorf("the lineage names %s/%s, want %s/%s",
			res.Lineage.Superseded, res.Lineage.Attempt, subject.ID, att.ID)
	}
	if res.Replacement.State != assignment.StateEnqueued || res.Replacement.ClaimEpoch != 0 {
		t.Errorf("the replacement is %s epoch %d, want enqueued epoch 0",
			res.Replacement.State, res.Replacement.ClaimEpoch)
	}
	if res.Reviewer.State != assignment.StateEnqueued {
		t.Errorf("the reviewer is %s, want enqueued", res.Reviewer.State)
	}
	after, err := store.Get(subject.ID)
	if err != nil {
		t.Fatalf("re-read the original: %v", err)
	}
	if after.State != assignment.StateInProgress {
		t.Errorf("the original is %q after supersession, want in_progress", after.State)
	}
}

// TestAssignmentCLIReadVerbsCarryExecutionState proves the operator's
// read verbs carry the derived picture: show names the effective state,
// tree names the tree/recovery picture with per-task effective states,
// and lineage returns the exact immutable authorisation.
func TestAssignmentCLIReadVerbsCarryExecutionState(t *testing.T) {
	assignmentCLIFixture(t)

	code, out := runCLI(t, "assignment", "create-root",
		"--project-id", "cpj-cli",
		"--title", "the root",
		"--objective", "the objective",
		"--recipient", "alpha",
		"--idempotency-key", "idem:cli:readview:root",
		"--permitted-operation", "read",
		"--eligible-recipient", "alpha,beta",
		"--max-depth", "3",
		"--max-tasks", "16",
		"--max-concurrency", "2",
		"--max-resource-units", "100",
		"--json",
	)
	if code != 0 {
		t.Fatalf("create-root exited %d: %s", code, out)
	}
	var created assignmentCLIState
	if err := json.Unmarshal([]byte(out), &created); err != nil {
		t.Fatalf("create-root output is not JSON: %v\n%s", err, out)
	}

	// Show names the effective state alongside the stored one.
	_, show, _ := runAssignmentCLI(t, "assignment", "show", "--id", created.ID, "--json")
	var shown struct {
		ID             string `json:"id"`
		State          string `json:"state"`
		EffectiveState string `json:"effective_state"`
	}
	if err := json.Unmarshal([]byte(show), &shown); err != nil {
		t.Fatalf("show output is not JSON: %v\n%s", err, show)
	}
	if shown.ID != created.ID || shown.EffectiveState == "" {
		t.Fatalf("show carries %+v, want the assignment with an effective state", shown)
	}

	// Tree names the derived picture and every task's effective state.
	_, treeOut, _ := runAssignmentCLI(t, "assignment", "tree", "--id", created.ID, "--json")
	var tree struct {
		RootID      string `json:"root_id"`
		Tree        string `json:"tree_status"`
		Recovery    string `json:"recovery_status"`
		Assignments []struct {
			ID             string `json:"id"`
			EffectiveState string `json:"effective_state"`
		} `json:"assignments"`
	}
	if err := json.Unmarshal([]byte(treeOut), &tree); err != nil {
		t.Fatalf("tree output is not JSON: %v\n%s", err, treeOut)
	}
	if tree.RootID != created.ID || tree.Tree == "" || tree.Recovery == "" {
		t.Fatalf("tree carries %+v, want the derived picture", tree)
	}
	if len(tree.Assignments) == 0 || tree.Assignments[0].EffectiveState == "" {
		t.Fatalf("tree items carry no effective state: %+v", tree.Assignments)
	}

	// Lineage of a missing id is a refusal, not an empty row.
	if code, _, errOut := runAssignmentCLI(t, "assignment", "lineage", "--id", "no-such-lineage"); code == 0 {
		t.Fatalf("a missing lineage exited 0")
	} else if errOut == "" {
		t.Fatalf("a missing lineage named no reason")
	}
}
