package assignment

import (
	"context"
	"fmt"
	"strings"

	oc "github.com/flowbyte-com/mpm-core/opencode"
)

// The REVIEW side of the adapter.
//
// WHAT IT ADDS TO Execute
//
// `Execute` runs an implementer and records the runtime's outcome. A review
// is different in one decisive way: the evidence it records must be CAPTURED
// BY THE HOST. A digest the implementer supplies is a claim; a digest computed
// over bytes the runtime actually produced is evidence.
//
// So this path:
//
//  1. validates the REVIEWER assignment (role, claim, lease, and that its
//     target exists and is reviewable);
//  2. asks the runtime to run the reviewer, which itself refuses unless the
//     reviewer definition and the `sandbox_exec` plugin are both available;
//  3. takes the reviewed revision and the findings digest FROM THE RESULT —
//     never from a parameter — and records them through the fenced evidence
//     capture.
//
// It records EVIDENCE. It does NOT record a verdict: a verdict is the
// reviewer's own fenced transition, and keeping the two apart is what stops a
// completed review from being manufactured by the party whose work is judged.

// ExecuteReviewParams selects one reviewer assignment.
type ExecuteReviewParams struct {
	// ID is the REVIEWER assignment.
	ID string
	// Epoch is its claim epoch.
	Epoch int64
	// Actor is attribution, not identity.
	Actor Actor
	// Brief is the bounded review brief.
	Brief string
	// Title labels the native session.
	Title string
	// ExpectedRevision is what the operator believes is under review. The
	// host checks it against what it observes; a mismatch is a refusal.
	ExpectedRevision string
}

// ExecuteReviewResult is what one review produced.
type ExecuteReviewResult struct {
	Assignment *Assignment `json:"assignment"`
	// SessionID is the native reviewer session.
	SessionID string `json:"session_id"`
	// ReviewedRevision is the HOST-observed revision.
	ReviewedRevision string `json:"reviewed_revision"`
	// FindingsSHA256 is the digest of the HOST-captured findings.
	FindingsSHA256 string `json:"findings_sha256"`
	// EvidenceRef is the pointer recorded on the assignment.
	EvidenceRef string `json:"evidence_ref"`
	// RuntimeOutcome is the reviewer's own report.
	RuntimeOutcome string `json:"runtime_outcome"`
	// Findings is the bounded captured text.
	Findings string `json:"findings"`
}

// ReviewRuntime is the reviewer boundary.
type ReviewRuntime interface {
	// MintSessionID returns the id the review will use. The adapter
	// persists it before the launch, exactly as it does for the worker.
	Review(ctx context.Context, req oc.ReviewRequest) (*oc.ReviewResult, error)
	// Inspect reads a session's own report WITHOUT running anything.
	//
	// It is here so a dispatcher can recover an interrupted review: it
	// has to be able to ask what became of a recorded session before it
	// decides whether another reviewer may run. An interface that could
	// not be inspected could only ever be reported, never recovered.
	Inspect(ctx context.Context, sessionID string) (*oc.SessionState, error)
}

// ExecuteReview runs one independent review and records its evidence.
func (s *Store) ExecuteReview(ctx context.Context, rt ReviewRuntime, p ExecuteReviewParams) (*ExecuteReviewResult, error) {
	if rt == nil {
		return nil, fmt.Errorf("%w: the adapter has no review runtime", ErrConstraint)
	}
	if strings.TrimSpace(p.ID) == "" {
		return nil, fmt.Errorf("%w: execute-review needs an assignment id", ErrConstraint)
	}

	rev, err := s.Get(p.ID)
	if err != nil {
		return nil, err
	}
	if err := s.validateReviewable(rev, p); err != nil {
		return nil, err
	}

	// A REVIEW THAT ALREADY CAPTURED ITS EVIDENCE CANNOT BE RUN AGAIN.
	//
	// The capture is write-once, so a second run would spend a full model
	// session and then be refused at the recording step — after paying for
	// it. The refusal therefore happens HERE, before OpenCode is invoked.
	// A review that FAILED recorded no evidence, and is retryable.
	if rev.ReviewedSHA != "" || rev.FindingsSHA256 != "" {
		return nil, fmt.Errorf(
			"%w: %s already carries captured evidence (%s); a completed review "+
				"is not re-run — the rework is a NEW assignment delegated from the "+
				"subject", ErrConstraint, rev.ID, rev.ReviewedSHA)
	}

	// PRIOR REVIEW ATTEMPTS ARE RECONCILED BEFORE A NEW ONE STARTS.
	//
	// A review is the most expensive thing this package runs, and running
	// two of them against one assignment would produce two findings
	// captures for a single verdict. So: a LIVE attempt refuses; an
	// AMBIGUOUS one refuses (whether it did work is unknowable, and
	// re-running could double the evidence); a terminal one is history and
	// a new attempt follows it. Draining ambiguity is PR 4's job.
	if err := s.reconcileReviewAttempts(rev.ID, p.Epoch); err != nil {
		return nil, err
	}

	// The session id is minted HERE and the intent persisted BEFORE the
	// launch, so an interrupted review is recoverable by id.
	sessionID, err := mintSessionID()
	if err != nil {
		return nil, err
	}
	corr, err := mintCorrelationRef()
	if err != nil {
		return nil, err
	}
	attempt, err := s.BeginAttempt(BeginAttemptParams{
		TaskID:          rev.ID,
		Epoch:           p.Epoch,
		Runtime:         RuntimeOpenCode,
		IntendedAgent:   "mpm-reviewer",
		NativeSessionID: sessionID,
		CorrelationRef:  corr,
		WorkspaceID:     "review",
		WorkspacePath:   rev.ID,
	})
	if err != nil {
		return nil, err
	}
	_ = attempt

	res, err := rt.Review(ctx, oc.ReviewRequest{
		SessionID:        sessionID,
		Brief:            p.Brief,
		Title:            p.Title,
		ExpectedRevision: p.ExpectedRevision,
	})
	if err != nil {
		// A review that could not run is a REFUSAL, recorded truthfully.
		_, _ = s.MarkAttemptFailed(SessionStateParams{
			AttemptID: attempt.ID, Epoch: p.Epoch, Error: boundedReason(err),
		})
		return nil, err
	}

	// WHAT THE REVIEWER ACTUALLY WAS, before its output is believed.
	if res.Agent != "mpm-reviewer" {
		_, _ = s.MarkAttemptFailed(SessionStateParams{
			AttemptID: attempt.ID, Epoch: p.Epoch,
			Error: "the review ran as agent " + res.Agent,
		})
		return nil, fmt.Errorf("%w: the review ran as agent %q, not mpm-reviewer",
			ErrConstraint, res.Agent)
	}
	if res.Outcome != "succeeded" {
		_, _ = s.MarkAttemptFailed(SessionStateParams{
			AttemptID: attempt.ID, Epoch: p.Epoch, Outcome: res.Outcome,
			Error: "the reviewer did not report a successful terminal outcome",
		})
		return nil, fmt.Errorf("%w: the reviewer's terminal outcome is %q, not %q",
			ErrConstraint, res.Outcome, "succeeded")
	}
	if strings.TrimSpace(res.Findings) == "" {
		_, _ = s.MarkAttemptFailed(SessionStateParams{
			AttemptID: attempt.ID, Epoch: p.Epoch, Outcome: res.Outcome,
			Error: "the reviewer produced no findings",
		})
		return nil, fmt.Errorf("%w: the reviewer produced no findings", ErrConstraint)
	}

	// THE EVIDENCE IS DERIVED, NOT SUPPLIED.
	if res.FindingsSHA256 == "" || res.ReviewedRevision == "" {
		_, _ = s.MarkAttemptAmbiguous(SessionStateParams{
			AttemptID: attempt.ID, Epoch: p.Epoch,
			Error: "the review produced no capture",
		})
		return nil, fmt.Errorf("%w: the review produced no captured evidence", ErrConstraint)
	}

	updated, err := s.CaptureReviewEvidence(ReviewEvidenceParams{
		ReviewerID:     rev.ID,
		Epoch:          p.Epoch,
		ReviewedSHA:    res.ReviewedRevision,
		FindingsSHA256: res.FindingsSHA256,
		EvidenceRef:    res.EvidencePath,
		Actor:          p.Actor,
	})
	if err != nil {
		_, _ = s.MarkAttemptFailed(SessionStateParams{
			AttemptID: attempt.ID, Epoch: p.Epoch, Error: boundedReason(err),
		})
		return nil, err
	}

	outcome := res.Outcome
	if outcome == "" {
		outcome = "unspecified"
	}
	if _, err := s.MarkAttemptCompleted(SessionStateParams{
		AttemptID: attempt.ID, Epoch: p.Epoch, Outcome: res.Outcome,
	}); err != nil {
		return nil, err
	}

	return &ExecuteReviewResult{
		Assignment:       updated,
		SessionID:        res.SessionID,
		ReviewedRevision: res.ReviewedRevision,
		FindingsSHA256:   res.FindingsSHA256,
		EvidenceRef:      res.EvidencePath,
		RuntimeOutcome:   outcome,
		Findings:         res.Findings,
	}, nil
}

// validateReviewable applies the reviewer preconditions.
func (s *Store) validateReviewable(rev *Assignment, p ExecuteReviewParams) error {
	if rev.Role != RoleReviewer {
		return fmt.Errorf("%w: %s is a %s and does not review", ErrConstraint, rev.ID, rev.Role)
	}
	if p.Epoch != rev.ClaimEpoch {
		return fmt.Errorf("%w: %s is at epoch %d", ErrStaleClaim, rev.ID, rev.ClaimEpoch)
	}
	if rev.ClaimEpoch == 0 {
		return fmt.Errorf("%w: %s holds no claim", ErrConstraint, rev.ID)
	}
	if rev.LeaseExpiresAt == nil || *rev.LeaseExpiresAt-s.Now() < minLeaseSeconds {
		return fmt.Errorf("%w: %s has no usable lease", ErrConstraint, rev.ID)
	}
	if rev.State != StateInProgress {
		return fmt.Errorf("%w: %s is %s; a review runs from in_progress",
			ErrIllegalTransition, rev.ID, rev.State)
	}
	// The target is fixed by the delegation that created the reviewer.
	target, _ := rev.Payload[PayloadKeyReviewTarget].(string)
	if target == "" {
		return fmt.Errorf("%w: %s names no review target", ErrConstraint, rev.ID)
	}
	subject, err := s.Get(target)
	if err != nil {
		return err
	}
	if subject.Role != RoleImplementer {
		return fmt.Errorf("%w: %s is not reviewable", ErrConstraint, subject.ID)
	}
	return nil
}

// reconcileReviewAttempts refuses a review while an earlier one is live or
// ambiguous.
func (s *Store) reconcileReviewAttempts(taskID string, epoch int64) error {
	attempts, err := s.AttemptsFor(taskID)
	if err != nil {
		return err
	}
	if len(attempts) == 0 {
		return nil
	}
	last := attempts[len(attempts)-1]

	// AN ATTEMPT FROM AN OLDER CLAIM CANNOT WRITE ITS RESULT — the state
	// writes are fenced on both the attempt's epoch and the assignment's
	// current one — so it is HISTORY, not a live review. Without this, a
	// reviewer whose lease expired and was reclaimed would keep a stale
	// attempt in `launched` forever and refuse every future review of that
	// assignment.
	if last.ClaimEpoch != epoch {
		return nil
	}

	switch last.LaunchState {
	case SessionCompleted, SessionFailed:
		// History. A new attempt follows it, visibly.
		return nil
	case SessionCancelled:
		return nil
	case SessionIntent, SessionLaunched, SessionRunning:
		return fmt.Errorf("%w: a previous review attempt (%s) is still %s; "+
			"refusing to start a second reviewer", ErrConstraint, last.ID, last.LaunchState)
	case SessionAmbiguous:
		return fmt.Errorf("%w: a previous review attempt (%s) is AMBIGUOUS — whether it "+
			"produced findings cannot be determined, so a second reviewer could "+
			"double the evidence", ErrConstraint, last.ID)
	default:
		return fmt.Errorf("%w: a previous review attempt (%s) is in an unknown state %q",
			ErrConstraint, last.ID, last.LaunchState)
	}
}
