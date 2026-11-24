// dispatch_test.go — Continuity 001, PR 4: the dispatcher's behavioural
// tests.
//
// # WHAT IS UNDER TEST
//
// The dispatcher's JOB is to be wrong in the safe direction. Every test
// here asserts a REFUSAL, a COUNT or a RECORD — never a happy path alone:
//
//   - a DISABLED dispatcher starts nothing at all;
//   - an enabled dispatcher dispatches exactly through PR 3's adapter;
//   - a running older-epoch session BLOCKS a second writer;
//   - an ambiguous outcome never becomes an invented success;
//   - a reviewer never gains the worker's surface.
//
// The runtime is a FAKE. These tests are about the DISPATCHER's decisions,
// and the real OpenCode boundary has its own gate (internal/core/opencode).
// A fake cannot prove that OpenCode honours `--session`; it can prove that
// the dispatcher does not confuse a session's existence with success, and
// that it never launches twice.
package dispatch

import (
	"context"
	"errors"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	core "github.com/flowbyte-com/mpm-core"
	"github.com/flowbyte-com/mpm-core/assignment"
	oc "github.com/flowbyte-com/mpm-core/opencode"
)

// ── the fake runtime ─────────────────────────────────────────────────────

// fakeRuntime is a scriptable stand-in for the OpenCode boundary.
//
// It counts the calls, because "the dispatcher launched once" and "the
// dispatcher launched twice" are the difference between a dispatcher and a
// duplicate.
type fakeRuntime struct {
	mu sync.Mutex
	// launches counts the worker launches.
	launches int
	// reviews counts the reviewer runs.
	reviews int
	// inspections counts the session inspections.
	inspections int
	// states is what the runtime knows about a session.
	states map[string]*oc.SessionState
	// launchState is what a launch reports. Nil means the launch produced
	// no state, which the adapter records as AMBIGUOUS.
	launchState *oc.SessionState
	// launchErr and inspectErr force failures.
	launchErr  error
	inspectErr error
	// workspace is the workspace the last launch was given.
	workspace string
	// agent is the agent the last launch was given.
	agent string
	// inspectHook, when set, decides what an inspection returns. It
	// lets a test script a session that reports an outcome only after
	// the first inspection — the shape a drain waits for.
	inspectHook func(sessionID string) *oc.SessionState
}

func newFakeRuntime() *fakeRuntime {
	return &fakeRuntime{states: map[string]*oc.SessionState{}}
}

func (f *fakeRuntime) AgentAllowed(name string) bool { return name == "mpm-worker" }

func (f *fakeRuntime) VerifyAgent(_ context.Context, name string) error {
	if name != "mpm-worker" {
		return errors.New("not allow-listed")
	}
	return nil
}

func (f *fakeRuntime) Launch(_ context.Context, req oc.LaunchRequest) (*oc.LaunchResult, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.launches++
	f.workspace = req.Workspace
	f.agent = req.Agent
	if f.launchErr != nil {
		return nil, f.launchErr
	}
	if f.launchState == nil {
		// NO STATE. The adapter must record this as AMBIGUOUS, not as a
		// success.
		return &oc.LaunchResult{SessionID: req.SessionID}, nil
	}
	state := *f.launchState
	state.ID = req.SessionID
	if f.states == nil {
		f.states = map[string]*oc.SessionState{}
	}
	f.states[req.SessionID] = &state
	return &oc.LaunchResult{SessionID: req.SessionID, State: &state}, nil
}

func (f *fakeRuntime) Review(_ context.Context, req oc.ReviewRequest) (*oc.ReviewResult, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.reviews++
	if f.launchState == nil {
		return nil, errors.New("the reviewer produced no state")
	}
	return &oc.ReviewResult{
		SessionID:         req.SessionID,
		ReviewedRevision:  strings.Repeat("1", 40),
		FindingsSHA256:    strings.Repeat("e", 64),
		Outcome:           "succeeded",
		Agent:             "mpm-reviewer",
		Model:             DefaultReviewModel,
		Findings:          "the findings",
		AssistantFindings: "the findings\nVERDICT: APPROVED",
		EvidencePath:      "/evidence/review.json",
	}, nil
}

func (f *fakeRuntime) Inspect(ctx context.Context, sessionID string) (*oc.SessionState, error) {
	// A CANCELLED CONTEXT FAILS THE INSPECTION, the way a real command
	// does. A fake that ignored it would hide the difference between
	// "the runtime went away" and "the caller stopped the run".
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	f.inspections++
	if f.inspectErr != nil {
		return nil, f.inspectErr
	}
	if f.inspectHook != nil {
		state := f.inspectHook(sessionID)
		if state == nil {
			return nil, oc.ErrSessionNotFound
		}
		return state, nil
	}
	state, ok := f.states[sessionID]
	if !ok {
		return nil, oc.ErrSessionNotFound
	}
	return state, nil
}

func (f *fakeRuntime) Launches() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.launches
}

func (f *fakeRuntime) Reviews() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.reviews
}

// ── the fixtures ─────────────────────────────────────────────────────────

// testStore is a disposable, file-backed database and the assignment
// service over it.
func testStore(t *testing.T) (*assignment.Store, string) {
	t.Helper()
	store, _, projectID := testStoreDB(t)
	return store, projectID
}

// testStoreDB is the same store, with the database manager as well —
// for the tests that need to look at the rows themselves.
func testStoreDB(t *testing.T) (*assignment.Store, *core.DatabaseManager, string) {
	t.Helper()
	dm := core.NewTestLocalOnlyDM(t)
	store := assignment.NewStore(dm)
	id := "cpj-" + strings.Repeat("a", 16)
	if _, err := dm.SQLDB().Exec(
		`INSERT INTO projects (id, created_by) VALUES (?, 'dispatch-test')`, id); err != nil {
		t.Fatalf("seed the project: %v", err)
	}
	return store, dm, id
}

// dispatchRoot creates a root with the budgets a test needs.
func dispatchRoot(t *testing.T, store *assignment.Store, projectID string) *assignment.Assignment {
	t.Helper()
	return dispatchRootWith(t, store, projectID, 8, 2)
}

// dispatchRootWith creates a root with explicit budgets.
//
// The budgets are the point: the tree's ceilings are what the service
// enforces atomically, and a test that wants to see a refusal needs a
// tree that is actually full.
func dispatchRootWith(t *testing.T, store *assignment.Store, projectID string,
	maxTasks, maxConcurrency int64) *assignment.Assignment {
	t.Helper()
	_, base := testRepository(t)
	a, _, err := store.CreateRoot(assignment.NewLocalOperatorAuthority("operator"),
		assignment.CreateRootParams{
			ProjectID:           projectID,
			Title:               "the title",
			Objective:           "the objective",
			Recipient:           "alpha",
			IdempotencyKey:      "idem:dispatch:root:" + t.Name(),
			PermittedOperations: []string{"deliver", "accept", "claim", "start", "complete"},
			EligibleRecipients:  []string{"alpha", "charlie"},
			MaxDepth:            2,
			MaxTasks:            maxTasks,
			MaxConcurrency:      maxConcurrency,
			MaxResourceUnits:    100,
			BaseSHA:             base,
		})
	if err != nil {
		t.Fatalf("create the root: %v", err)
	}
	return a
}

// dispatchChild delegates a child and advances it to `accepted`.
func dispatchChild(t *testing.T, store *assignment.Store, parent *assignment.Assignment,
	recipient, role, key string) *assignment.Assignment {
	t.Helper()
	child, _, err := store.Delegate(assignment.DelegateParams{
		ParentID:       parent.ID,
		Title:          "the child",
		Objective:      "the objective",
		Recipient:      recipient,
		Role:           role,
		IdempotencyKey: key,
		BaseSHA:        parent.BaseSHA,
	})
	if err != nil {
		t.Fatalf("delegate: %v", err)
	}
	actor := assignment.Actor{Address: recipient, Kind: "agent"}
	for _, step := range []func(assignment.FencedParams) (*assignment.Assignment, error){
		store.Deliver, store.Acknowledge, store.Accept,
	} {
		if _, err := step(assignment.FencedParams{ID: child.ID, Actor: actor}); err != nil {
			t.Fatalf("advance the child: %v", err)
		}
	}
	return child
}

// testConfig is an ENABLED configuration a test can use.
func testConfig(t *testing.T) Config {
	t.Helper()
	cfg := DefaultConfig()
	cfg.Enabled = true
	cfg.Concurrency = 1
	cfg.Owner = "mpm-dispatcher"
	cfg.LeaseSeconds = 900
	cfg.RuntimeTimeoutSeconds = 600
	cfg.LeaseMarginSeconds = 300
	cfg.MaxDispatchAttempts = 3
	cfg.RetryBackoffSeconds = 0
	cfg.CandidateLimit = 20
	cfg.WorkspaceRoot = filepath.Join(t.TempDir(), "workspaces")
	repo, _ := testRepository(t)
	cfg.RepositoryPath = repo
	cfg.ReviewRepoPath = t.TempDir()
	cfg.Model = "mpm-sidecar/test"
	return cfg
}

// testEngine builds an engine over a fake runtime.
func testEngine(t *testing.T, store *assignment.Store, cfg Config) (*Engine, *fakeRuntime) {
	t.Helper()
	fake := newFakeRuntime()
	built := 0
	engine, err := NewEngine(store, cfg, func(_ context.Context, a *assignment.Assignment, workspace string) (
		assignment.Runtime, assignment.ReviewRuntime, error) {
		built++
		if a.Role == assignment.RoleReviewer {
			return nil, fake, nil
		}
		return fake, nil, nil
	})
	if err != nil {
		t.Fatalf("build the engine: %v", err)
	}
	engine.now = func() time.Time { return time.Unix(1_700_000_000, 0).UTC() }
	_ = built
	return engine, fake
}

// okState is a session state the adapter ACCEPTS: the right directory,
// the right agent and a named model. The adapter refuses a session that
// does not verify, and a fake that reports a bare state would be testing
// the refusal rather than the dispatcher.
func okState(agent, outcome string) *oc.SessionState {
	return &oc.SessionState{
		Outcome:  outcome,
		Agent:    agent,
		Model:    oc.SessionModel{ID: "test", ProviderID: "mpm-sidecar"},
		Location: oc.SessionLocation{Directory: oc.PrefixWorker},
	}
}

// outcomeFor finds an outcome by assignment id.
func outcomeFor(r *Report, id string) (Outcome, bool) {
	for _, o := range r.Outcomes {
		if o.AssignmentID == id {
			return o, true
		}
	}
	return Outcome{}, false
}

// ── 1. DISABLED BY DEFAULT ───────────────────────────────────────────────

// TestDispatchIsDisabledByDefaultAndStartsNothing is requirement 1.
//
// A dispatcher that is off must not read a row, build a runtime, or launch
// anything. The factory is the observable: it is what builds the runtime,
// and a runtime is what can spend money.
func TestDispatchIsDisabledByDefaultAndStartsNothing(t *testing.T) {
	store, projectID := testStore(t)
	root := dispatchRoot(t, store, projectID)
	child := dispatchChild(t, store, root, "alpha", assignment.RoleImplementer, "idem:dispatch:off")

	factoryCalls := 0
	engine, err := NewEngine(store, DefaultConfig(), func(context.Context, *assignment.Assignment, string) (
		assignment.Runtime, assignment.ReviewRuntime, error) {
		factoryCalls++
		return newFakeRuntime(), nil, nil
	})
	if err != nil {
		t.Fatalf("build the engine: %v", err)
	}

	report, err := engine.RunOnce(context.Background())
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	if report.Enabled {
		t.Error("the report says the dispatcher is enabled")
	}
	if factoryCalls != 0 {
		t.Errorf("the runtime factory was called %d times while disabled; a disabled "+
			"dispatcher must not build a runtime", factoryCalls)
	}
	if report.Dispatched != 0 {
		t.Errorf("%d assignments were dispatched while disabled", report.Dispatched)
	}
	if len(report.Outcomes) != 1 || report.Outcomes[0].Reason != ReasonDisabled {
		t.Errorf("the report does not say the dispatcher is disabled: %+v", report.Outcomes)
	}
	// And the assignment is untouched: no claim, no attempt.
	got, err := store.Get(child.ID)
	if err != nil {
		t.Fatalf("read the child: %v", err)
	}
	if got.ClaimEpoch != 0 {
		t.Errorf("the child was claimed while the dispatcher was disabled")
	}
	attempts, err := store.AttemptsFor(child.ID)
	if err != nil {
		t.Fatalf("read the attempts: %v", err)
	}
	if len(attempts) != 0 {
		t.Errorf("the child has %d attempts; a disabled dispatcher must not create one",
			len(attempts))
	}
}

// TestAnInvalidEnabledConfigurationIsRefused proves that "enabled but
// unusable" is an ERROR and not a reason to guess.
func TestAnInvalidEnabledConfigurationIsRefused(t *testing.T) {
	store, _ := testStore(t)
	cfg := testConfig(t)
	cfg.LeaseSeconds = 60 // shorter than the runtime window
	if _, err := NewEngine(store, cfg, func(context.Context, *assignment.Assignment, string) (
		assignment.Runtime, assignment.ReviewRuntime, error) {
		return newFakeRuntime(), nil, nil
	}); err == nil {
		t.Fatal("a lease that cannot cover the runtime window was accepted")
	} else if !strings.Contains(err.Error(), "does not cover") {
		t.Errorf("refused, but not as a lease problem: %v", err)
	}
}

// ── 2. AN ENABLED DISPATCH GOES THROUGH PR 3's ADAPTER ──────────────────

// TestAnEnabledDispatcherDispatchesExactlyThroughTheAdapter is
// requirement 2.
//
// The dispatcher must not have an execution path of its own: the launch
// happens inside PR 3's adapter, and the adapter's durable record — the
// attempt — is what proves it.
func TestAnEnabledDispatcherDispatchesExactlyThroughTheAdapter(t *testing.T) {
	store, projectID := testStore(t)
	root := dispatchRoot(t, store, projectID)
	child := dispatchChild(t, store, root, "alpha", assignment.RoleImplementer, "idem:dispatch:on")

	engine, fake := testEngine(t, store, testConfig(t))
	fake.launchState = okState("mpm-worker", "succeeded")

	report, err := engine.RunOnce(context.Background())
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	if fake.Launches() != 1 {
		t.Fatalf("the runtime was launched %d times, want 1", fake.Launches())
	}
	if report.Dispatched != 1 {
		t.Errorf("the report dispatched %d assignments, want 1", report.Dispatched)
	}
	// THE COMPLETION IS COUNTED. A report whose "completed" is always
	// zero is a report that cannot be read.
	if report.Completed != 1 {
		t.Errorf("the report completed %d assignments, want 1", report.Completed)
	}
	if report.Refused != 0 || report.Deferred != 0 || report.Ambiguous != 0 {
		t.Errorf("the report counts a refusal, a deferral or an ambiguity: %+v", report)
	}

	// THE ADAPTER'S OWN RECORD. The attempt row is what makes the launch
	// recoverable, and its existence proves the adapter ran.
	attempts, err := store.AttemptsFor(child.ID)
	if err != nil {
		t.Fatalf("read the attempts: %v", err)
	}
	if len(attempts) != 1 {
		t.Fatalf("the child has %d attempts, want 1", len(attempts))
	}
	if attempts[0].LaunchState != assignment.SessionCompleted {
		t.Errorf("the attempt is %q, want %q", attempts[0].LaunchState, assignment.SessionCompleted)
	}
	if attempts[0].NativeSessionID == "" {
		t.Error("the attempt recorded no session id")
	}
	if attempts[0].IntendedAgent != "mpm-worker" {
		t.Errorf("the attempt names the agent %q, want mpm-worker", attempts[0].IntendedAgent)
	}
	// The claim was taken through the fenced API.
	got, err := store.Get(child.ID)
	if err != nil {
		t.Fatalf("read the child: %v", err)
	}
	if got.ClaimEpoch == 0 {
		t.Error("the dispatcher did not claim the assignment")
	}
	if got.State != assignment.StateCompleted {
		t.Errorf("the assignment is %q, want %q", got.State, assignment.StateCompleted)
	}
}

// TestTheDispatcherUsesTheOperatorsWorkspace proves the dispatcher does not
// invent a workspace: the worktree is inside the configured root.
func TestTheDispatcherUsesTheOperatorsWorkspace(t *testing.T) {
	store, projectID := testStore(t)
	root := dispatchRoot(t, store, projectID)
	child := dispatchChild(t, store, root, "alpha", assignment.RoleImplementer, "idem:dispatch:ws")

	cfg := testConfig(t)
	engine, fake := testEngine(t, store, cfg)
	fake.launchState = okState("mpm-worker", "succeeded")

	if _, err := engine.RunOnce(context.Background()); err != nil {
		t.Fatalf("run: %v", err)
	}
	want := filepath.Join(cfg.WorkspaceRoot, child.ID)
	if fake.workspace != want {
		t.Errorf("the launch used the workspace %q, want %q", fake.workspace, want)
	}
}

// ── 3. DISABLING STOPS NEW LAUNCHES ─────────────────────────────────────

// TestDisablingTheDispatcherStopsNewLaunches is requirement 10.
//
// Disabling must prevent new launches WITHOUT rewriting existing task
// history: the attempt records of the earlier run stay exactly as they
// were.
func TestDisablingTheDispatcherStopsNewLaunches(t *testing.T) {
	store, projectID := testStore(t)
	root := dispatchRoot(t, store, projectID)
	first := dispatchChild(t, store, root, "alpha", assignment.RoleImplementer, "idem:dispatch:off2:a")

	cfg := testConfig(t)
	engine, fake := testEngine(t, store, cfg)
	fake.launchState = okState("mpm-worker", "succeeded")
	if _, err := engine.RunOnce(context.Background()); err != nil {
		t.Fatalf("the first run: %v", err)
	}
	if fake.Launches() != 1 {
		t.Fatalf("the first run launched %d times, want 1", fake.Launches())
	}
	before, err := store.AttemptsFor(first.ID)
	if err != nil {
		t.Fatalf("read the attempts: %v", err)
	}

	// A SECOND, ELIGIBLE assignment, and the dispatcher switched off.
	second := dispatchChild(t, store, root, "alpha", assignment.RoleImplementer, "idem:dispatch:off2:b")
	off := cfg
	off.Enabled = false
	offEngine, offFake := testEngine(t, store, off)
	report, err := offEngine.RunOnce(context.Background())
	if err != nil {
		t.Fatalf("the disabled run: %v", err)
	}
	if offFake.Launches() != 0 {
		t.Errorf("the disabled dispatcher launched %d times", offFake.Launches())
	}
	if report.Dispatched != 0 {
		t.Errorf("the disabled dispatcher dispatched %d assignments", report.Dispatched)
	}

	// The history is untouched.
	after, err := store.AttemptsFor(first.ID)
	if err != nil {
		t.Fatalf("read the attempts: %v", err)
	}
	if len(after) != len(before) {
		t.Errorf("the attempts changed: %d -> %d", len(before), len(after))
	}
	// And the untouched assignment has none.
	secondAttempts, err := store.AttemptsFor(second.ID)
	if err != nil {
		t.Fatalf("read the attempts: %v", err)
	}
	if len(secondAttempts) != 0 {
		t.Errorf("the disabled dispatcher created %d attempts", len(secondAttempts))
	}
}

// ── 4. AMBIGUITY IS NEVER AN INVENTED SUCCESS ───────────────────────────

// TestAnAmbiguousOutcomeNeverBecomesAnInventedSuccess is requirement 9.
//
// A launch that produced no session state is AMBIGUOUS: the process may
// have started and done work before the adapter lost it. The assignment
// must NOT complete, and the report must say so.
func TestAnAmbiguousOutcomeNeverBecomesAnInventedSuccess(t *testing.T) {
	store, projectID := testStore(t)
	root := dispatchRoot(t, store, projectID)
	child := dispatchChild(t, store, root, "alpha", assignment.RoleImplementer, "idem:dispatch:ambiguous")

	engine, fake := testEngine(t, store, testConfig(t))
	fake.launchState = nil // NO STATE: the ambiguous case.

	report, err := engine.RunOnce(context.Background())
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	if report.Ambiguous != 1 {
		t.Errorf("the report counted %d ambiguous outcomes, want 1", report.Ambiguous)
	}
	outcome, ok := outcomeFor(report, child.ID)
	if !ok {
		t.Fatalf("the report has no outcome for %s: %+v", child.ID, report.Outcomes)
	}
	if outcome.Reason != ReasonAmbiguous {
		t.Errorf("the outcome is %q, want %q", outcome.Reason, ReasonAmbiguous)
	}

	got, err := store.Get(child.ID)
	if err != nil {
		t.Fatalf("read the child: %v", err)
	}
	if got.State == assignment.StateCompleted {
		t.Error("an ambiguous execution completed the assignment")
	}
	attempts, err := store.AttemptsFor(child.ID)
	if err != nil {
		t.Fatalf("read the attempts: %v", err)
	}
	if len(attempts) != 1 || attempts[0].LaunchState != assignment.SessionAmbiguous {
		t.Errorf("the attempt is %v, want one AMBIGUOUS attempt", attempts)
	}
}

// ── 5. A REVIEWER IS NOT A WORKER ───────────────────────────────────────

// TestAReviewerCannotGainWorkerPrivileges is requirement 11.
//
// The reviewer runs through the REVIEW path, with its own definition, and
// the worker's launch surface is never touched. The review's EVIDENCE is
// recorded; the review's VERDICT is not, because the verdict is the
// reviewer's own fenced transition and the dispatcher does not make it.
func TestAReviewerCannotGainWorkerPrivileges(t *testing.T) {
	store, projectID := testStore(t)
	root := dispatchRoot(t, store, projectID)
	subject := dispatchChild(t, store, root, "alpha", assignment.RoleImplementer, "idem:dispatch:rev:subject")

	// THE SUBJECT IS FINISHED, so it is not a dispatch candidate and the
	// reviewer is the only work this run has to do.
	subjectActor := assignment.Actor{Address: "alpha", Kind: "agent"}
	claimed, err := store.Claim(assignment.ClaimParams{
		ID: subject.ID, ExpectedEpoch: 0, Owner: "alpha", LeaseSeconds: 600,
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
	if _, err := store.Complete(assignment.FencedParams{
		ID: subject.ID, Epoch: claimed.ClaimEpoch, Actor: subjectActor,
	}); err != nil {
		t.Fatalf("complete the subject: %v", err)
	}

	// THE REVIEWER NAMES ITS SUBJECT, the way a delegation does. The
	// target is fixed by the delegation that creates the reviewer.
	reviewer, _, err := store.Delegate(assignment.DelegateParams{
		ParentID:       root.ID,
		Title:          "the review",
		Objective:      "review the work",
		Recipient:      "charlie",
		Role:           assignment.RoleReviewer,
		IdempotencyKey: "idem:dispatch:rev:reviewer",
		HeadSHA:        strings.Repeat("1", 40),
		Payload:        map[string]interface{}{assignment.PayloadKeyReviewTarget: subject.ID},
	})
	if err != nil {
		t.Fatalf("delegate the reviewer: %v", err)
	}
	actor := assignment.Actor{Address: "charlie", Kind: "agent"}
	for _, step := range []func(assignment.FencedParams) (*assignment.Assignment, error){
		store.Deliver, store.Acknowledge, store.Accept,
	} {
		if _, err := step(assignment.FencedParams{ID: reviewer.ID, Actor: actor}); err != nil {
			t.Fatalf("advance the reviewer: %v", err)
		}
	}

	engine, fake := testEngine(t, store, testConfig(t))
	fake.launchState = okState("mpm-reviewer", "succeeded")

	report, err := engine.RunOnce(context.Background())
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	if fake.Reviews() != 1 {
		t.Fatalf("the reviewer ran %d times, want 1", fake.Reviews())
	}
	if fake.Launches() != 0 {
		t.Errorf("the worker launched %d times for a review; the reviewer must not "+
			"have the worker's execution surface", fake.Launches())
	}

	outcome, ok := outcomeFor(report, reviewer.ID)
	if !ok {
		t.Fatalf("the report has no outcome for the reviewer: %+v", report.Outcomes)
	}
	if outcome.FindingsSHA256 == "" || outcome.ReviewedRevision == "" {
		t.Errorf("the review recorded no evidence: %+v", outcome)
	}
	// THE HOST TRANSCRIBES THE REVIEWER'S OWN TERMINAL VERDICT. It is
	// not an operator approval, and the subject moves only because the
	// captured assistant text said APPROVED.
	got, err := store.Get(reviewer.ID)
	if err != nil {
		t.Fatalf("read the reviewer: %v", err)
	}
	if got.State != assignment.StateReviewPassed {
		t.Errorf("the reviewer is %q, want review_passed after transcription", got.State)
	}
	subjectRow, err := store.Get(subject.ID)
	if err != nil {
		t.Fatalf("read the subject: %v", err)
	}
	if subjectRow.State != assignment.StateReviewPassed {
		t.Errorf("the subject is %q, want review_passed", subjectRow.State)
	}
	if got.ApprovedAt != nil || subjectRow.ApprovedAt != nil {
		t.Error("the dispatcher recorded an operator approval")
	}
}

// ── 6. THE SELECTION IS DETERMINISTIC AND DOES NOT MONOPOLISE ──────────

// TestTheSelectionIsDeterministic proves two dispatchers see the same
// order.
func TestTheSelectionIsDeterministic(t *testing.T) {
	store, projectID := testStore(t)
	root := dispatchRoot(t, store, projectID)
	first := dispatchChild(t, store, root, "alpha", assignment.RoleImplementer, "idem:dispatch:order:a")
	second := dispatchChild(t, store, root, "alpha", assignment.RoleImplementer, "idem:dispatch:order:b")

	candidates, err := store.DispatchCandidates(assignment.DispatchQuery{
		Limit: 10,
	})
	if err != nil {
		t.Fatalf("candidates: %v", err)
	}
	if len(candidates) != 2 {
		t.Fatalf("%d candidates, want 2", len(candidates))
	}
	// The order is (created_at, id), so two assignments created in the
	// same second are ordered by id. What matters is that it is
	// REPRODUCIBLE, not that it is the order they were created in.
	seen := map[string]bool{candidates[0].ID: true, candidates[1].ID: true}
	if !seen[first.ID] || !seen[second.ID] {
		t.Errorf("the candidates are %s and %s; want %s and %s",
			candidates[0].ID, candidates[1].ID, first.ID, second.ID)
	}
	// And a second read gives the same order.
	again, err := store.DispatchCandidates(assignment.DispatchQuery{
		Limit: 10,
	})
	if err != nil {
		t.Fatalf("candidates: %v", err)
	}
	for i := range again {
		if again[i].ID != candidates[i].ID {
			t.Fatalf("the order changed between reads")
		}
	}
}

// TestAFailingAssignmentCannotMonopoliseTheDispatcher proves the backoff
// and the attempt cap.
func TestAFailingAssignmentCannotMonopoliseTheDispatcher(t *testing.T) {
	store, projectID := testStore(t)
	root := dispatchRoot(t, store, projectID)
	failing := dispatchChild(t, store, root, "alpha", assignment.RoleImplementer, "idem:dispatch:fail:a")
	other := dispatchChild(t, store, root, "alpha", assignment.RoleImplementer, "idem:dispatch:fail:b")

	cfg := testConfig(t)
	cfg.MaxDispatchAttempts = 1
	engine, fake := testEngine(t, store, cfg)
	// The runtime reports a FAILED outcome, so the attempt is terminal
	// and failed. (A launch that produced no state at all is AMBIGUOUS,
	// which is a different case and is tested as one.)
	fake.launchState = okState("mpm-worker", "failed")

	if _, err := engine.RunOnce(context.Background()); err != nil {
		t.Fatalf("the first run: %v", err)
	}

	// WHICH ONE WAS ATTEMPTED is the dispatcher's business, not the
	// test's: the selection is ordered by (created_at, id), and two
	// assignments created in the same second are ordered by id.
	attempted, untouched := failing, other
	if len(mustAttempts(t, store, failing.ID)) == 0 {
		attempted, untouched = other, failing
	}
	attempts := mustAttempts(t, store, attempted.ID)
	if len(attempts) != 1 {
		t.Fatalf("the attempted assignment has %d attempts, want 1", len(attempts))
	}
	if attempts[0].LaunchState != assignment.SessionFailed {
		t.Errorf("the attempt is %q, want %q", attempts[0].LaunchState, assignment.SessionFailed)
	}

	// THE NEXT RUN DEFERS IT, because it has used its dispatch attempts.
	report, err := engine.RunOnce(context.Background())
	if err != nil {
		t.Fatalf("the second run: %v", err)
	}
	outcome, ok := outcomeFor(report, attempted.ID)
	if !ok {
		t.Fatalf("the report has no outcome for the attempted assignment: %+v", report.Outcomes)
	}
	if outcome.Reason != ReasonDeferred {
		t.Errorf("the attempted assignment is %q, want %q", outcome.Reason, ReasonDeferred)
	}
	// And the OTHER assignment was still dispatched: the failing one did
	// not monopolise the run.
	otherOutcome, ok := outcomeFor(report, untouched.ID)
	if !ok {
		t.Fatalf("the report has no outcome for the other assignment: %+v", report.Outcomes)
	}
	if otherOutcome.Reason == ReasonDeferred {
		t.Errorf("the other assignment was deferred too: %+v", otherOutcome)
	}
}

// mustAttempts reads an assignment's attempts.
func mustAttempts(t *testing.T, store *assignment.Store, id string) []*assignment.SessionAttempt {
	t.Helper()
	out, err := store.AttemptsFor(id)
	if err != nil {
		t.Fatalf("read the attempts of %s: %v", id, err)
	}
	return out
}
