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
		EligibleRecipients:  []string{recipient, "charlie"},
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

// TestExecuteDoesNotRepromptATerminalSession is the reviewer's F1.
//
// A session that already reported an outcome has FINISHED. The adapter must
// record what the runtime said, not launch the same objective into the same
// conversation a second time. The attempt row would still show one attempt,
// so the duplicate would be invisible in the record — which is exactly why
// this needs its own test.
func TestExecuteDoesNotRepromptATerminalSession(t *testing.T) {
	store, _, _, projectID := newTestStore(t)
	ws := t.TempDir()
	id, epoch := claimedAssignment(t, store, projectID, "alpha")

	intent, err := store.BeginAttempt(BeginAttemptParams{
		TaskID: id, Epoch: epoch, Runtime: RuntimeOpenCode,
		IntendedAgent: "mpm-worker", NativeSessionID: "ses_mpm_terminal_0001",
		CorrelationRef: "corr-terminal", WorkspaceID: "ws", WorkspacePath: ws,
	})
	if err != nil {
		t.Fatalf("intent: %v", err)
	}

	// The session already succeeded, but the adapter never recorded it.
	rt := &fakeRuntime{
		allowed:  map[string]bool{"mpm-worker": true},
		sessions: map[string]*oc.SessionState{intent.NativeSessionID: okState(ws, "permitted-model", "mpm-worker")},
	}

	res, err := store.Execute(context.Background(), rt, executeParams(id, epoch, ws))
	if err != nil {
		t.Fatalf("execute: %v", err)
	}
	if rt.launches != 0 {
		t.Fatalf("the adapter re-prompted a TERMINAL session (%d launches)", rt.launches)
	}
	if res.LaunchState != SessionCompleted {
		t.Errorf("launch state %q, want %q", res.LaunchState, SessionCompleted)
	}
	if res.Assignment.State != StateCompleted {
		t.Errorf("the assignment is %q; the observed success was not recorded", res.Assignment.State)
	}
}

// TestExecuteStartsANewAttemptAfterTheEpochAdvances is the reviewer's F2.
//
// An attempt stamped with the OLD epoch can never record its result: the
// state write is fenced on both the attempt's epoch and the assignment's
// current one. Resuming it would launch a session whose outcome could not be
// recorded, and every retry would launch again. A new epoch is a new attempt.
func TestExecuteStartsANewAttemptAfterTheEpochAdvances(t *testing.T) {
	store, _, _, projectID := newTestStore(t)
	ws := t.TempDir()
	id, epoch := claimedAssignment(t, store, projectID, "alpha")

	// An attempt under the OLD epoch, still at `intent`.
	old, err := store.BeginAttempt(BeginAttemptParams{
		TaskID: id, Epoch: epoch, Runtime: RuntimeOpenCode,
		IntendedAgent: "mpm-worker", NativeSessionID: "ses_mpm_old_epoch_01",
		CorrelationRef: "corr-old", WorkspaceID: "ws", WorkspacePath: ws,
	})
	if err != nil {
		t.Fatalf("intent: %v", err)
	}

	// The claim is renewed: the epoch advances.
	renewed, err := store.Renew(RenewParams{
		ID: id, ExpectedEpoch: epoch, Owner: "alpha", LeaseSeconds: 7200,
		Actor: Actor{Address: "alpha", Kind: "agent"},
	})
	if err != nil {
		t.Fatalf("renew: %v", err)
	}
	if renewed.ClaimEpoch == epoch {
		t.Fatal("the renewal did not advance the epoch; the fixture is wrong")
	}

	rt := &fakeRuntime{
		allowed:     map[string]bool{"mpm-worker": true},
		launchState: okState(ws, "permitted-model", "mpm-worker"),
	}
	res, err := store.Execute(context.Background(), rt, executeParams(id, renewed.ClaimEpoch, ws))
	if err != nil {
		t.Fatalf("execute: %v", err)
	}
	if res.Attempt.NativeSessionID == old.NativeSessionID {
		t.Error("the adapter RESUMED an attempt from an older claim instead of starting a new one")
	}
	if res.Attempt.Attempt != 2 {
		t.Errorf("attempt number %d, want 2 (a visible retry)", res.Attempt.Attempt)
	}
	if res.LaunchState != SessionCompleted {
		t.Errorf("launch state %q, want %q (reason %q)",
			res.LaunchState, SessionCompleted, res.Attempt.LastError)
	}
}

// TestCaptureReviewEvidenceRefusesNonReviewersAndIsWriteOnce is the
// reviewer's F7: the path had no tests at all.
//
// The separation from the VERDICT is structural (the verb is absent from the
// tool registry), but the evidence itself must also be fenced and
// write-once, or "the record of what was examined" could be replaced after
// the fact.
func TestCaptureReviewEvidenceRefusesNonReviewersAndIsWriteOnce(t *testing.T) {
	store, _, _, projectID := newTestStore(t)
	implID, implEpoch := claimedAssignment(t, store, projectID, "alpha")

	goodSHA := strings.Repeat("a", 40)
	goodDigest := strings.Repeat("b", 64)

	// 1. An IMPLEMENTER has no review to evidence.
	_, err := store.CaptureReviewEvidence(ReviewEvidenceParams{
		ReviewerID: implID, Epoch: implEpoch,
		ReviewedSHA: goodSHA, FindingsSHA256: goodDigest, EvidenceRef: "evidence/x",
	})
	if err == nil {
		t.Error("an implementer's assignment accepted review evidence")
	}

	// 2. A REVIEWER, in progress, at the right epoch: accepted once.
	rev := reviewFixture(t, store, implID, "charlie")
	if _, err := store.CaptureReviewEvidence(ReviewEvidenceParams{
		ReviewerID: rev.id, Epoch: rev.epoch,
		ReviewedSHA: goodSHA, FindingsSHA256: goodDigest, EvidenceRef: "evidence/x",
	}); err != nil {
		t.Fatalf("a well-formed capture was refused: %v", err)
	}

	// 3. A SECOND capture is refused: the record is write-once.
	if _, err := store.CaptureReviewEvidence(ReviewEvidenceParams{
		ReviewerID: rev.id, Epoch: rev.epoch,
		ReviewedSHA: strings.Repeat("c", 40), FindingsSHA256: strings.Repeat("d", 64),
		EvidenceRef: "evidence/y",
	}); err == nil {
		t.Error("the captured evidence was rewritten")
	}

	// 4. A malformed digest is refused, so a placeholder cannot be stored.
	if _, err := store.CaptureReviewEvidence(ReviewEvidenceParams{
		ReviewerID: rev.id, Epoch: rev.epoch,
		ReviewedSHA: "not-a-sha", FindingsSHA256: goodDigest, EvidenceRef: "e",
	}); err == nil {
		t.Error("a malformed reviewed revision was accepted")
	}

	// 5. And the row still carries the FIRST capture.
	got, err := store.Get(rev.id)
	if err != nil {
		t.Fatalf("read the reviewer: %v", err)
	}
	if got.ReviewedSHA != goodSHA || got.FindingsSHA256 != goodDigest {
		t.Errorf("the evidence is %q/%q, want %q/%q",
			got.ReviewedSHA, got.FindingsSHA256, goodSHA, goodDigest)
	}
}

type reviewFixtureResult struct {
	id    string
	epoch int64
}

// reviewFixture builds a delegated REVIEWER assignment for subjectID, in
// progress and claimed.
func reviewFixture(t *testing.T, store *Store, subjectID, recipient string) reviewFixtureResult {
	t.Helper()
	rev, _, err := store.Delegate(DelegateParams{
		ParentID:       subjectID,
		Title:          "independent review",
		Objective:      "review the work",
		Recipient:      recipient,
		Role:           RoleReviewer,
		IdempotencyKey: "idem:reviewer",
		Actor:          Actor{Address: "alpha", Kind: "agent"},
		Payload:        map[string]interface{}{PayloadKeyReviewTarget: subjectID},
	})
	if err != nil {
		t.Fatalf("delegate the reviewer: %v", err)
	}
	actor := Actor{Address: recipient, Kind: "agent"}
	for _, step := range []func(FencedParams) (*Assignment, error){
		store.Deliver, store.Acknowledge, store.Accept,
	} {
		if _, err := step(FencedParams{ID: rev.ID, Actor: actor}); err != nil {
			t.Fatalf("advance the reviewer: %v", err)
		}
	}
	claimed, err := store.Claim(ClaimParams{
		ID: rev.ID, Owner: recipient, LeaseSeconds: 3600, Actor: actor,
	})
	if err != nil {
		t.Fatalf("claim the reviewer: %v", err)
	}
	if _, err := store.Start(FencedParams{
		ID: rev.ID, Epoch: claimed.ClaimEpoch, Actor: actor,
	}); err != nil {
		t.Fatalf("start the reviewer: %v", err)
	}
	return reviewFixtureResult{id: rev.ID, epoch: claimed.ClaimEpoch}
}

// TestExecuteRefusesASecondWriterAcrossEpochs is finding 1.
//
// The epoch advancing does not prove the previous holder's session stopped.
// Renewing while an attempt is still running is exactly that situation, and a
// new dispatch would put TWO runtimes into the same worktree.
func TestExecuteRefusesASecondWriterAcrossEpochs(t *testing.T) {
	store, _, _, projectID := newTestStore(t)
	ws := t.TempDir()
	id, epoch := claimedAssignment(t, store, projectID, "alpha")

	running := okState(ws, "permitted-model", "mpm-worker")
	running.Outcome = "" // still working

	first, err := store.BeginAttempt(BeginAttemptParams{
		TaskID: id, Epoch: epoch, Runtime: RuntimeOpenCode,
		IntendedAgent: "mpm-worker", NativeSessionID: "ses_mpm_epoch1_running",
		CorrelationRef: "corr-1", WorkspaceID: "ws", WorkspacePath: ws,
	})
	if err != nil {
		t.Fatalf("intent: %v", err)
	}
	if _, err := store.MarkAttemptLaunched(SessionStateParams{AttemptID: first.ID, Epoch: epoch}); err != nil {
		t.Fatalf("mark launched: %v", err)
	}

	// The lease is renewed: the epoch advances while the first run lives.
	renewed, err := store.Renew(RenewParams{
		ID: id, ExpectedEpoch: epoch, Owner: "alpha", LeaseSeconds: 7200,
		Actor: Actor{Address: "alpha", Kind: "agent"},
	})
	if err != nil {
		t.Fatalf("renew: %v", err)
	}

	rt := &fakeRuntime{
		allowed:  map[string]bool{"mpm-worker": true},
		sessions: map[string]*oc.SessionState{first.NativeSessionID: running},
	}
	res, err := store.Execute(context.Background(), rt, executeParams(id, renewed.ClaimEpoch, ws))
	if err == nil {
		t.Fatalf("a second writer was dispatched while the first was running (state %q)", res.LaunchState)
	}
	if !strings.Contains(err.Error(), "still running") {
		t.Errorf("refused, but not as a concurrent writer: %v", err)
	}
	if rt.launches != 0 {
		t.Errorf("the runtime was launched %d times", rt.launches)
	}
}

// TestExecuteReportsCompletionFailureSeparately is finding 2.
//
// The runtime can succeed while the ASSIGNMENT fails to complete. Reporting
// nil there would claim a completion that never happened, so the two facts
// are reported separately and the call errors.
func TestExecuteReportsCompletionFailureSeparately(t *testing.T) {
	store, _, _, projectID := newTestStore(t)
	ws := t.TempDir()
	id, epoch := claimedAssignment(t, store, projectID, "alpha")

	// A DETERMINISTIC fault: the completion's transition fails, and only
	// that one. Start is allowed to succeed first.
	started := false
	store.fault = func(stage string) error {
		if stage == "transition" && started {
			return errors.New("injected completion failure")
		}
		if stage == "transition" {
			started = true
		}
		return nil
	}
	t.Cleanup(func() { store.fault = nil })

	rt := &fakeRuntime{
		allowed:     map[string]bool{"mpm-worker": true},
		launchState: okState(ws, "permitted-model", "mpm-worker"),
	}
	res, err := store.Execute(context.Background(), rt, executeParams(id, epoch, ws))
	if err == nil {
		t.Fatal("a completion failure was reported as success")
	}
	if res == nil {
		t.Fatal("the result was discarded, so the runtime's success is invisible")
	}
	if !res.RuntimeSucceeded {
		t.Error("the result does not record that the RUNTIME succeeded")
	}
	if res.CompletionError == "" {
		t.Error("the result does not name why the assignment did not complete")
	}
	if res.Assignment.State == StateCompleted {
		t.Error("the assignment completed despite the injected failure")
	}
}

// TestExecuteReviewRequiresAnInProgressReviewer pins the review preconditions.
func TestExecuteReviewRequiresAnInProgressReviewer(t *testing.T) {
	store, _, _, projectID := newTestStore(t)
	implID, _ := claimedAssignment(t, store, projectID, "alpha")

	// An IMPLEMENTER is not a reviewer.
	_, err := store.ExecuteReview(context.Background(), &fakeReviewRuntime{}, ExecuteReviewParams{
		ID: implID, Epoch: 1, Brief: "b",
	})
	if err == nil {
		t.Error("an implementer was asked to review")
	}

	rev := reviewFixture(t, store, implID, "charlie")
	// A STALE epoch is refused.
	_, err = store.ExecuteReview(context.Background(), &fakeReviewRuntime{}, ExecuteReviewParams{
		ID: rev.id, Epoch: rev.epoch - 1, Brief: "b",
	})
	if err == nil {
		t.Error("a stale review epoch was accepted")
	}
}

// TestExecuteReviewRecordsOnlyHostDerivedEvidence is finding 3.
//
// The digest and the revision come FROM THE RESULT, never from a parameter.
// This test gives the adapter no way to supply either.
func TestExecuteReviewRecordsOnlyHostDerivedEvidence(t *testing.T) {
	store, _, _, projectID := newTestStore(t)
	implID, _ := claimedAssignment(t, store, projectID, "alpha")
	rev := reviewFixture(t, store, implID, "charlie")

	const hostRev = "1111111111111111111111111111111111111111"
	fake := &fakeReviewRuntime{result: &oc.ReviewResult{
		SessionID:        "ses_mpm_review_01",
		ReviewedRevision: hostRev,
		FindingsSHA256:   strings.Repeat("e", 64),
		EvidencePath:     "/evidence/review.json",
		Findings:         "the findings",
		Outcome:          "succeeded",
	}}

	res, err := store.ExecuteReview(context.Background(), fake, ExecuteReviewParams{
		ID: rev.id, Epoch: rev.epoch, Brief: "review it",
	})
	if err != nil {
		t.Fatalf("execute-review: %v", err)
	}
	if res.ReviewedRevision != hostRev {
		t.Errorf("reviewed revision %q, want the host-observed %q", res.ReviewedRevision, hostRev)
	}
	if res.FindingsSHA256 != strings.Repeat("e", 64) {
		t.Errorf("digest %q, want the host-derived one", res.FindingsSHA256)
	}
	// And the row carries it.
	got, err := store.Get(rev.id)
	if err != nil {
		t.Fatalf("read the reviewer: %v", err)
	}
	if got.ReviewedSHA != hostRev || got.FindingsSHA256 != strings.Repeat("e", 64) {
		t.Errorf("the row carries %q/%q", got.ReviewedSHA, got.FindingsSHA256)
	}
	// The verdict is untouched: evidence is not a verdict.
	if got.State == StateReviewPassed {
		t.Error("capturing evidence recorded a review verdict")
	}
}

// TestExecuteReviewRefusesWhenTheReviewerCannotRun pins the fail-closed rule.
func TestExecuteReviewRefusesWhenTheReviewerCannotRun(t *testing.T) {
	store, _, _, projectID := newTestStore(t)
	implID, _ := claimedAssignment(t, store, projectID, "alpha")
	rev := reviewFixture(t, store, implID, "charlie")

	fake := &fakeReviewRuntime{err: oc.ErrReviewerUnavailable}
	_, err := store.ExecuteReview(context.Background(), fake, ExecuteReviewParams{
		ID: rev.id, Epoch: rev.epoch, Brief: "review it",
	})
	if err == nil {
		t.Fatal("a review with no available reviewer was accepted")
	}
	if !errors.Is(err, oc.ErrReviewerUnavailable) {
		t.Errorf("refused, but not as an unavailable reviewer: %v", err)
	}
	// The assignment carries NO evidence.
	got, _ := store.Get(rev.id)
	if got.ReviewedSHA != "" || got.FindingsSHA256 != "" {
		t.Error("evidence was recorded for a review that never ran")
	}
}

type fakeReviewRuntime struct {
	result *oc.ReviewResult
	err    error
}

func (f *fakeReviewRuntime) Review(_ context.Context, req oc.ReviewRequest) (*oc.ReviewResult, error) {
	if f.err != nil {
		return nil, f.err
	}
	r := *f.result
	r.SessionID = req.SessionID
	return &r, nil
}
