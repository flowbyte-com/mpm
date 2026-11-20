// execute_test.go — Continuity 001, PR 3: the adapter's behavioural tests.
//
// # WHAT IS UNDER TEST
//
// The adapter's JOB is to be wrong in the safe direction. Every test here
// asserts a REFUSAL or a TRUTHFUL RECORD, never a happy path alone:
//
//   - an unknown agent is refused BEFORE a session exists;
//   - a session-level permission grant is refused even when the runtime
//     reports success;
//   - a session that ran in a different directory is refused;
//   - a stale claim cannot record an attempt, and cannot publish a result;
//   - a retry reconciles on evidence rather than launching again;
//   - an interruption with no observable outcome is recorded AMBIGUOUS
//     rather than assumed to have succeeded or failed.
//
// The runtime is a FAKE. That is deliberate: these tests are about the
// adapter's decisions, and the real OpenCode boundary has its own gate
// (internal/core/opencode, and the real-host demonstration recorded in the
// PR). A fake cannot prove that OpenCode honours `--session`; it can prove
// that the adapter does not confuse a session's existence with success.
package assignment

import (
	"context"
	"errors"
	"strings"
	"testing"

	oc "github.com/flowbyte-com/mpm-core/opencode"
)

// fakeRuntime is a scriptable stand-in for the OpenCode boundary.
type fakeRuntime struct {
	allowed   map[string]bool
	verifyErr error
	// state is what Inspect reports for a known session. A nil entry
	// means "the runtime does not know that session".
	sessions map[string]*oc.SessionState
	// launchState is what Launch reports.
	launchState *oc.SessionState
	launchErr   error
	// launches counts real Launch calls, so "no duplicate" is observable.
	launches int
	// inspectErr forces Inspect to fail.
	inspectErr error
}

func (f *fakeRuntime) AgentAllowed(name string) bool { return f.allowed[name] }

func (f *fakeRuntime) VerifyAgent(_ context.Context, name string) error {
	if !f.allowed[name] {
		return errors.New("not allow-listed")
	}
	return f.verifyErr
}

func (f *fakeRuntime) Launch(_ context.Context, req oc.LaunchRequest) (*oc.LaunchResult, error) {
	f.launches++
	if f.launchErr != nil {
		return nil, f.launchErr
	}
	if f.launchState == nil {
		return &oc.LaunchResult{SessionID: req.SessionID}, nil
	}
	state := *f.launchState
	state.ID = req.SessionID
	if f.sessions == nil {
		f.sessions = map[string]*oc.SessionState{}
	}
	f.sessions[req.SessionID] = &state
	return &oc.LaunchResult{
		SessionID: req.SessionID,
		ExitCode:  0,
		Output:    "runtime output",
		State:     &state,
	}, nil
}

func (f *fakeRuntime) Inspect(_ context.Context, sessionID string) (*oc.SessionState, error) {
	if f.inspectErr != nil {
		return nil, f.inspectErr
	}
	if s, ok := f.sessions[sessionID]; ok {
		return s, nil
	}
	return nil, oc.ErrSessionNotFound
}

// okState is a well-formed successful state for the given workspace/agent.
func okState(workspace, model, agent string) *oc.SessionState {
	return &oc.SessionState{
		Agent:    agent,
		Outcome:  "succeeded",
		Location: oc.SessionLocation{Directory: workspace},
		Model:    oc.SessionModel{ID: model, ProviderID: "mpm-sidecar"},
	}
}

// claimedAssignment prepares a root assignment in `accepted` with a live
// lease, returning its id and epoch.
func claimedAssignment(t *testing.T, store *Store, projectID, recipient string) (string, int64) {
	t.Helper()
	a, _, err := store.CreateRoot(NewLocalOperatorAuthority("test"), CreateRootParams{
		ProjectID:           projectID,
		Title:               "adapter test",
		Objective:           "exercise the adapter",
		Recipient:           recipient,
		IdempotencyKey:      "idem:" + recipient,
		PermittedOperations: []string{"deliver", "accept", "claim", "start", "complete"},
		EligibleRecipients:  []string{recipient},
		MaxDepth:            1,
		MaxTasks:            4,
		MaxConcurrency:      2,
		MaxResourceUnits:    10,
	})
	if err != nil {
		t.Fatalf("create the root: %v", err)
	}
	actor := Actor{Address: recipient, Kind: "agent"}
	mustDo(t, func() (*Assignment, error) {
		return store.Deliver(FencedParams{ID: a.ID, Actor: actor})
	})
	mustDo(t, func() (*Assignment, error) {
		return store.Acknowledge(FencedParams{ID: a.ID, Actor: actor})
	})
	mustDo(t, func() (*Assignment, error) {
		return store.Accept(FencedParams{ID: a.ID, Actor: actor})
	})
	claimed, err := store.Claim(ClaimParams{ID: a.ID, Owner: recipient, LeaseSeconds: 3600, Actor: Actor{Address: recipient, Kind: "agent"}})
	if err != nil {
		t.Fatalf("claim: %v", err)
	}
	return a.ID, claimed.ClaimEpoch
}

func executeParams(id string, epoch int64, workspace string) ExecuteParams {
	return ExecuteParams{
		ID:        id,
		Epoch:     epoch,
		Actor:     Actor{Address: "alpha", Kind: "agent"},
		Agent:     "mpm-worker",
		Workspace: workspace,
		// Contained runtimes report the bind-mounted path; the fake
		// reports whatever the test tells it, so name it here.
		ExpectedRuntimeDir: workspace,
		WorkspaceID:        "ws",
		Model:              "mpm-sidecar/permitted-model",
		Title:              "t",
	}
}

// TestExecuteRefusesUnknownAgentBeforeAnySession is the F3 fix.
//
// The measured host behaviour is that `session.create` accepts an unknown
// agent name and the failure appears only when the run starts. Relying on
// that would mean creating a session — and burning a run — before learning
// the name was wrong. The adapter refuses first, and no session is created.
func TestExecuteRefusesUnknownAgentBeforeAnySession(t *testing.T) {
	store, _, _, projectID := newTestStore(t)
	id, epoch := claimedAssignment(t, store, projectID, "alpha")

	rt := &fakeRuntime{allowed: map[string]bool{"some-other-agent": true}}
	_, err := store.Execute(context.Background(), rt, executeParams(id, epoch, t.TempDir()))
	if err == nil {
		t.Fatal("an assignment was executed with an agent the runtime does not allow")
	}
	if !strings.Contains(err.Error(), "allow-list") {
		t.Errorf("refused, but not by the allow-list rule: %v", err)
	}
	if rt.launches != 0 {
		t.Errorf("the runtime was launched %d times; the refusal must come first", rt.launches)
	}
	// And no attempt was recorded, so a later retry starts cleanly.
	if attempts, _ := store.AttemptsFor(id); len(attempts) != 0 {
		t.Errorf("an attempt was recorded for a refused execution: %d", len(attempts))
	}
}

// TestExecuteRefusesSessionLevelPermissionGrant is the F4 boundary.
//
// The host HONOURS a session-level `permissions` grant over the agent
// definition's denies — measured on v2.3.2 — so a grant on a worker session
// is a privilege widening. The adapter must refuse even though the runtime
// reported success.
func TestExecuteRefusesSessionLevelPermissionGrant(t *testing.T) {
	store, _, _, projectID := newTestStore(t)
	ws := t.TempDir()
	id, epoch := claimedAssignment(t, store, projectID, "alpha")

	state := okState(ws, "permitted-model", "mpm-worker")
	state.Permissions = []oc.SessionPerm{{Action: "shell", Resource: "*", Effect: "allow"}}
	rt := &fakeRuntime{allowed: map[string]bool{"mpm-worker": true}, launchState: state}

	res, err := store.Execute(context.Background(), rt, executeParams(id, epoch, ws))
	if err != nil {
		t.Fatalf("execute: %v", err)
	}
	if res.LaunchState != SessionFailed {
		t.Errorf("a session with a permission grant ended %q, want %q", res.LaunchState, SessionFailed)
	}
	if !strings.Contains(res.Attempt.LastError, "SESSION-LEVEL permission grant") {
		t.Errorf("the refusal did not name the grant: %q", res.Attempt.LastError)
	}
	// AND THE ASSIGNMENT DID NOT COMPLETE.
	if res.Assignment.State == StateCompleted {
		t.Error("the assignment completed on a session whose permissions were widened")
	}
}

// TestExecuteRefusesWrongWorkspace is the F2 boundary.
//
// The runtime binds its working directory from the PROCESS CWD and IGNORES
// `session.create`'s directory parameter, so a session can run somewhere
// else. The adapter compares what the runtime reports with the authorised
// worktree before believing any result.
func TestExecuteRefusesWrongWorkspace(t *testing.T) {
	store, _, _, projectID := newTestStore(t)
	ws := t.TempDir()
	id, epoch := claimedAssignment(t, store, projectID, "alpha")

	rt := &fakeRuntime{
		allowed:     map[string]bool{"mpm-worker": true},
		launchState: okState("/somewhere/else", "permitted-model", "mpm-worker"),
	}
	res, err := store.Execute(context.Background(), rt, executeParams(id, epoch, ws))
	if err != nil {
		t.Fatalf("execute: %v", err)
	}
	if res.LaunchState != SessionFailed {
		t.Errorf("a session that ran elsewhere ended %q, want %q", res.LaunchState, SessionFailed)
	}
	if !strings.Contains(res.Attempt.LastError, "authorised workspace") {
		t.Errorf("the refusal did not name the workspace: %q", res.Attempt.LastError)
	}
}

// TestExecuteSuccessCompletesTheAssignment is the one happy path, kept so
// the refusals above are not vacuously true.
func TestExecuteSuccessCompletesTheAssignment(t *testing.T) {
	store, _, _, projectID := newTestStore(t)
	ws := t.TempDir()
	id, epoch := claimedAssignment(t, store, projectID, "alpha")

	rt := &fakeRuntime{
		allowed:     map[string]bool{"mpm-worker": true},
		launchState: okState(ws, "permitted-model", "mpm-worker"),
	}
	res, err := store.Execute(context.Background(), rt, executeParams(id, epoch, ws))
	if err != nil {
		t.Fatalf("execute: %v", err)
	}
	if res.LaunchState != SessionCompleted {
		t.Fatalf("launch state %q, want %q (reason %q)", res.LaunchState, SessionCompleted, res.Attempt.LastError)
	}
	if res.Assignment.State != StateCompleted {
		t.Errorf("the assignment is %q, want %q", res.Assignment.State, StateCompleted)
	}
	if res.Evidence.OutputSHA256 == "" {
		t.Error("no output digest was recorded")
	}
	if !strings.Contains(res.Evidence.RuntimeDirectory, ws) {
		t.Errorf("the evidence did not record the runtime directory: %q", res.Evidence.RuntimeDirectory)
	}
}

// TestExecuteRefusesAStaleEpoch is the fence.
//
// A worker whose lease was reclaimed presents an old epoch. It cannot even
// record an attempt, and it certainly cannot complete the assignment.
func TestExecuteRefusesAStaleEpoch(t *testing.T) {
	store, _, _, projectID := newTestStore(t)
	ws := t.TempDir()
	id, epoch := claimedAssignment(t, store, projectID, "alpha")

	rt := &fakeRuntime{allowed: map[string]bool{"mpm-worker": true}, launchState: okState(ws, "permitted-model", "mpm-worker")}

	_, err := store.Execute(context.Background(), rt, executeParams(id, epoch-1, ws))
	if err == nil {
		t.Fatal("a stale epoch was accepted")
	}
	if !errors.Is(err, ErrStaleClaim) {
		t.Errorf("refused, but not as a stale claim: %v", err)
	}
	if rt.launches != 0 {
		t.Errorf("the runtime was launched %d times for a stale epoch", rt.launches)
	}
}

// TestExecuteRecordsAmbiguityRatherThanAssuming pins the honest answer for
// an interruption.
//
// When the runtime cannot say what happened, the adapter records
// AMBIGUOUS. It must not complete the assignment, and it must not report
// failure either — both would be inventions.
func TestExecuteRecordsAmbiguityRatherThanAssuming(t *testing.T) {
	store, _, _, projectID := newTestStore(t)
	ws := t.TempDir()
	id, epoch := claimedAssignment(t, store, projectID, "alpha")

	rt := &fakeRuntime{allowed: map[string]bool{"mpm-worker": true}}
	res, err := store.Execute(context.Background(), rt, executeParams(id, epoch, ws))
	if err != nil {
		t.Fatalf("execute: %v", err)
	}
	if res.LaunchState != SessionAmbiguous {
		t.Errorf("launch state %q, want %q", res.LaunchState, SessionAmbiguous)
	}
	if res.Assignment.State == StateCompleted {
		t.Error("the assignment completed on an ambiguous run")
	}
	if res.Evidence.OutputSHA256 != "" {
		t.Error("an ambiguous run recorded an output digest as if it had output")
	}
}

// TestExecuteRuntimeFailureIsTruthful pins that a reported failure is
// recorded as a failure and never as a completion.
func TestExecuteRuntimeFailureIsTruthful(t *testing.T) {
	store, _, _, projectID := newTestStore(t)
	ws := t.TempDir()
	id, epoch := claimedAssignment(t, store, projectID, "alpha")

	state := okState(ws, "permitted-model", "mpm-worker")
	state.Outcome = "failed"
	rt := &fakeRuntime{allowed: map[string]bool{"mpm-worker": true}, launchState: state}

	res, err := store.Execute(context.Background(), rt, executeParams(id, epoch, ws))
	if err != nil {
		t.Fatalf("execute: %v", err)
	}
	if res.LaunchState != SessionFailed {
		t.Errorf("launch state %q, want %q", res.LaunchState, SessionFailed)
	}
	if res.RuntimeOutcome != "failed" {
		t.Errorf("runtime outcome %q, want %q", res.RuntimeOutcome, "failed")
	}
	if res.Assignment.State == StateCompleted {
		t.Error("a failed run completed the assignment")
	}
}

// TestExecuteRefusesConcurrentDuplicateLaunch is the duplicate-prevention
// rule.
//
// An attempt that is still running is not a licence to start a second one.
// The fixture reproduces the state a crash mid-run leaves: the intent was
// written, the attempt was launched, and the runtime still reports the
// session as working.
func TestExecuteRefusesConcurrentDuplicateLaunch(t *testing.T) {
	store, _, _, projectID := newTestStore(t)
	ws := t.TempDir()
	id, epoch := claimedAssignment(t, store, projectID, "alpha")

	intent, err := store.BeginAttempt(BeginAttemptParams{
		TaskID: id, Epoch: epoch, Runtime: RuntimeOpenCode,
		IntendedAgent: "mpm-worker", NativeSessionID: "ses_mpm_running_0001",
		CorrelationRef: "corr-running", WorkspaceID: "ws", WorkspacePath: ws,
	})
	if err != nil {
		t.Fatalf("intent: %v", err)
	}
	if _, err := store.MarkAttemptLaunched(SessionStateParams{
		AttemptID: intent.ID, Epoch: epoch,
	}); err != nil {
		t.Fatalf("mark launched: %v", err)
	}

	// The runtime reports the session as working: no outcome yet.
	running := okState(ws, "permitted-model", "mpm-worker")
	running.Outcome = ""
	rt := &fakeRuntime{
		allowed:  map[string]bool{"mpm-worker": true},
		sessions: map[string]*oc.SessionState{intent.NativeSessionID: running},
	}

	_, err = store.Execute(context.Background(), rt, executeParams(id, epoch, ws))
	if err == nil {
		t.Fatal("a second launch was started while the first was running")
	}
	if !strings.Contains(err.Error(), "still running") {
		t.Errorf("refused, but not as a concurrent launch: %v", err)
	}
	if rt.launches != 0 {
		t.Errorf("the runtime was launched %d times; a running attempt is not re-launched", rt.launches)
	}
}

// TestExecuteReconcilesAnIntentWhoseSessionNeverExisted is the crash case
// the record exists for.
//
// The intent is durable; the runtime has never heard of the session. That
// is the SAFE case: nothing was created, so the SAME id is used and no
// duplicate session can appear.
func TestExecuteReconcilesAnIntentWhoseSessionNeverExisted(t *testing.T) {
	store, _, _, projectID := newTestStore(t)
	ws := t.TempDir()
	id, epoch := claimedAssignment(t, store, projectID, "alpha")

	// Simulate the crash: the intent was written, then the process died.
	intent, err := store.BeginAttempt(BeginAttemptParams{
		TaskID: id, Epoch: epoch, Runtime: RuntimeOpenCode,
		IntendedAgent: "mpm-worker", NativeSessionID: "ses_mpm_crashed_0001",
		CorrelationRef: "corr-crashed", WorkspaceID: "ws", WorkspacePath: ws,
	})
	if err != nil {
		t.Fatalf("write the launch intent: %v", err)
	}

	rt := &fakeRuntime{allowed: map[string]bool{"mpm-worker": true}, launchState: okState(ws, "permitted-model", "mpm-worker")}
	res, err := store.Execute(context.Background(), rt, executeParams(id, epoch, ws))
	if err != nil {
		t.Fatalf("execute: %v", err)
	}
	if !res.Reconciled {
		t.Error("the recovery was not reported as a reconciliation")
	}
	if res.Attempt.NativeSessionID != intent.NativeSessionID {
		t.Errorf("the retry used session %q, want the recorded %q",
			res.Attempt.NativeSessionID, intent.NativeSessionID)
	}
	if rt.launches != 1 {
		t.Errorf("launches = %d, want exactly 1", rt.launches)
	}
}

// TestExecuteRecordsAmbiguityWhenALaunchedSessionIsLost is the case the
// contract refuses to paper over.
//
// The intent says the runtime was launched, and the runtime no longer knows
// the session. Whether work happened is UNKNOWABLE, so the adapter records
// the ambiguity and does not launch again.
func TestExecuteRecordsAmbiguityWhenALaunchedSessionIsLost(t *testing.T) {
	store, _, _, projectID := newTestStore(t)
	ws := t.TempDir()
	id, epoch := claimedAssignment(t, store, projectID, "alpha")

	intent, err := store.BeginAttempt(BeginAttemptParams{
		TaskID: id, Epoch: epoch, Runtime: RuntimeOpenCode,
		IntendedAgent: "mpm-worker", NativeSessionID: "ses_mpm_lost_0001",
		CorrelationRef: "corr-lost", WorkspaceID: "ws", WorkspacePath: ws,
	})
	if err != nil {
		t.Fatalf("intent: %v", err)
	}
	if _, err := store.MarkAttemptLaunched(SessionStateParams{AttemptID: intent.ID, Epoch: epoch}); err != nil {
		t.Fatalf("mark launched: %v", err)
	}

	// The runtime does not know the session.
	rt := &fakeRuntime{allowed: map[string]bool{"mpm-worker": true}}
	res, err := store.Execute(context.Background(), rt, executeParams(id, epoch, ws))
	if err != nil {
		t.Fatalf("execute: %v", err)
	}
	if res.LaunchState != SessionAmbiguous {
		t.Errorf("launch state %q, want %q", res.LaunchState, SessionAmbiguous)
	}
	if rt.launches != 0 {
		t.Errorf("the adapter launched %d times on an ambiguous record", rt.launches)
	}
	if res.Assignment.State == StateCompleted {
		t.Error("an ambiguous record completed the assignment")
	}
}

// TestExecuteWorksForAChildAssignment pins that the tree's operations are
// read from the ROOT.
//
// A child carries no `permitted_operations` of its own — the schema says so,
// and the bound it inherits is the ROOT's. The first version of the adapter
// checked the child's own empty list and therefore refused every child
// assignment; this test fails without the fix.
func TestExecuteWorksForAChildAssignment(t *testing.T) {
	store, _, _, projectID := newTestStore(t)
	rootID, rootEpoch := claimedAssignment(t, store, projectID, "alpha")
	ws := t.TempDir()

	// The root is `accepted` and claimed; delegate a child from it.
	child, _, err := store.Delegate(DelegateParams{
		ParentID:       rootID,
		Title:          "child work",
		Objective:      "do the child work",
		Recipient:      "alpha",
		Role:           RoleImplementer,
		IdempotencyKey: "idem:child",
		Actor:          Actor{Address: "alpha", Kind: "agent"},
	})
	if err != nil {
		t.Fatalf("delegate: %v", err)
	}
	if child.Depth == 0 {
		t.Fatal("the delegated assignment is not a child")
	}
	if len(child.PermittedOperations) != 0 {
		t.Fatalf("the child carries its own operation list; the schema should forbid that")
	}

	actor := Actor{Address: "alpha", Kind: "agent"}
	for _, step := range []func(FencedParams) (*Assignment, error){
		store.Deliver, store.Acknowledge, store.Accept,
	} {
		if _, err := step(FencedParams{ID: child.ID, Actor: actor}); err != nil {
			t.Fatalf("advance the child: %v", err)
		}
	}
	claimed, err := store.Claim(ClaimParams{
		ID: child.ID, Owner: "alpha", LeaseSeconds: 3600, Actor: actor,
	})
	if err != nil {
		t.Fatalf("claim the child: %v", err)
	}
	_ = rootEpoch

	rt := &fakeRuntime{
		allowed:     map[string]bool{"mpm-worker": true},
		launchState: okState(ws, "permitted-model", "mpm-worker"),
	}
	res, err := store.Execute(context.Background(), rt,
		executeParams(child.ID, claimed.ClaimEpoch, ws))
	if err != nil {
		t.Fatalf("execute the child: %v", err)
	}
	if res.LaunchState != SessionCompleted {
		t.Errorf("child launch state %q, want %q (reason %q)",
			res.LaunchState, SessionCompleted, res.Attempt.LastError)
	}
}
