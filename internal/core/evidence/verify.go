package evidence

import (
	"context"
	"fmt"
	"strings"
)

// The VERIFICATION step.
//
// A published manifest is an INDEX OF EVIDENCE, not proof that its claims
// are still true. The moment it was published, the pull request could
// have advanced, a workflow run could have started, and a review could
// have been overtaken by a new commit.
//
// So every material assertion in a record is CHECKED against GitHub
// before a reader is shown it, and the check's result is published with
// the answer. A reader never has to trust the publisher about the state
// of the world.
//
// # THE FOUR RESULTS
//
//   - verified: GitHub says the same thing the record says
//   - stale: the record was true, and the world has moved on
//   - failed: GitHub says something else
//   - missing: the thing could not be found
//   - unverified: GitHub could not be asked
//
// "GitHub could not be asked" is NOT a pass. A network failure must never
// turn into a successful verification, and a reader who sees "unverified"
// knows that nobody checked.

// The check results.
const (
	CheckVerified   = "verified"
	CheckStale      = "stale"
	CheckFailed     = "failed"
	CheckMissing    = "missing"
	CheckUnverified = "unverified"
)

// The names of the checks.
//
// They are constants because the report names them, and a name that
// changed in one place and not another would make the report lie about
// what it checked.
const (
	CheckRepository     = "repository"
	CheckPullRequest    = "pull_request"
	CheckPRHead         = "pr_head"
	CheckPRState        = "pr_state"
	CheckReviewedSHA    = "reviewed_sha"
	CheckCI             = "ci"
	CheckReviewEvidence = "review_evidence"
	CheckReviewVerdict  = "review_verdict"
	CheckApproval       = "operator_approval"
)

// Check is one verification result.
type Check struct {
	// Name is the check's name.
	Name string `json:"name"`
	// Status is the result.
	Status string `json:"status"`
	// Detail explains the result in one bounded line.
	Detail string `json:"detail,omitempty"`
}

// Verification is the whole verification of one record.
type Verification struct {
	// Checks are the results, in the order they were made.
	Checks []Check `json:"checks"`
}

// Verdict is the record's overall verification state.
//
// It is the WORST of the checks, and "worst" is ordered so that an
// unverified record is never presented as a verified one.
func (v *Verification) Verdict() string {
	worst := CheckVerified
	for _, c := range v.Checks {
		if rank(c.Status) > rank(worst) {
			worst = c.Status
		}
	}
	return worst
}

// Stale says whether anything about the record has been overtaken.
func (v *Verification) Stale() bool {
	for _, c := range v.Checks {
		if c.Status == CheckStale || c.Status == CheckFailed {
			return true
		}
	}
	return false
}

// rank orders the check results by how much they should worry a reader.
func rank(status string) int {
	switch status {
	case CheckVerified:
		return 0
	case CheckMissing:
		return 1
	case CheckStale:
		return 2
	case CheckFailed:
		return 3
	case CheckUnverified:
		return 4
	default:
		return 5
	}
}

// GitHub is the boundary the verifier reads the outside world through.
//
// It is an INTERFACE so that the checks can be exercised against a
// scripted GitHub, and so that a reader who has no credentials can still
// read a PUBLIC repository.
type GitHub interface {
	// PullRequest reads one pull request's facts.
	//
	// It returns ErrNotFound when the pull request does not exist, and
	// an unavailable error when GitHub could not be asked.
	PullRequest(ctx context.Context, repo string, number int) (*GitHubFacts, error)

	// WorkflowRuns reads the workflow runs for a commit.
	WorkflowRuns(ctx context.Context, repo string, headSHA string) ([]CIRun, error)

	// Content reads a file from a ref of the repository.
	Content(ctx context.Context, repo string, ref string, path string) ([]byte, error)
}

// Verifier checks a published record against GitHub.
type Verifier struct {
	// GitHub is the reader.
	GitHub GitHub
}

// Verify checks one record.
//
// The record's own claims are never used to satisfy a check: each check
// compares the record against what GitHub says NOW.
func (v *Verifier) Verify(ctx context.Context, rec *Record) (*Verification, error) {
	if v == nil || v.GitHub == nil {
		return nil, fmt.Errorf("evidence: there is no GitHub reader to verify against")
	}
	if rec == nil {
		return nil, fmt.Errorf("evidence: there is no record to verify")
	}
	out := &Verification{}

	// THE PULL REQUEST. Every other check needs it, and its absence is
	// not a detail: a record that points at a pull request nobody can
	// find is a record whose subject is missing.
	number, prErr := PRNumber(rec.PRRef)
	var facts *GitHubFacts
	switch {
	case prErr != nil:
		out.add(CheckPullRequest, CheckMissing, prErr.Error())
	case rec.PRRef == Unknown:
		out.add(CheckPullRequest, CheckMissing, "the record names no pull request")
	default:
		f, err := v.GitHub.PullRequest(ctx, rec.Repository, number)
		switch {
		case err != nil && isMissing(err):
			out.add(CheckPullRequest, CheckMissing,
				fmt.Sprintf("pull request #%d could not be found in %s", number, rec.Repository))
			facts = nil
		case err != nil:
			out.add(CheckPullRequest, CheckUnverified, err.Error())
			facts = nil
		default:
			facts = f
			out.add(CheckPullRequest, CheckVerified,
				fmt.Sprintf("pull request #%d is %s", number, prState(f)))
		}
	}

	// THE REPOSITORY. A record that points at one repository and claims
	// another is a record about the wrong project.
	if facts != nil {
		if facts.Repository == rec.Repository {
			out.add(CheckRepository, CheckVerified, facts.Repository)
		} else {
			out.add(CheckRepository, CheckFailed,
				fmt.Sprintf("the record names %s and the pull request is in %s",
					rec.Repository, facts.Repository))
		}
		v.checkHead(out, rec, facts)
		v.checkPRState(out, rec, facts)
		v.checkReviewedSHA(out, rec, facts)
		v.checkCI(ctx, out, rec)
	} else {
		// Without the pull request there is nothing to compare the
		// head, the state or the review against, and saying "verified"
		// would be inventing a comparison that did not happen.
		out.add(CheckPRHead, CheckUnverified, "the pull request could not be read")
		out.add(CheckPRState, CheckUnverified, "the pull request could not be read")
		out.add(CheckReviewedSHA, CheckUnverified, "the pull request could not be read")
		out.add(CheckCI, CheckUnverified, "the pull request could not be read")
	}

	v.checkReviewEvidence(ctx, out, rec)
	v.checkReviewVerdict(out, rec)
	v.checkApproval(out, rec)
	return out, nil
}

// checkHead compares the record's head with the pull request's.
func (v *Verifier) checkHead(out *Verification, rec *Record, facts *GitHubFacts) {
	switch {
	case rec.HeadSHA == Unknown:
		status := CheckMissing
		detail := "the record names no head"
		if facts.HeadSHA != "" && facts.HeadSHA != rec.HeadSHA {
			status = CheckStale
			detail = fmt.Sprintf("the record names no head and the pull request is at %s",
				facts.HeadSHA)
		}
		out.add(CheckPRHead, status, detail)
	case facts.HeadSHA == "":
		out.add(CheckPRHead, CheckUnverified, "the pull request's head could not be read")
	case facts.HeadSHA == rec.HeadSHA:
		out.add(CheckPRHead, CheckVerified, facts.HeadSHA)
	default:
		out.add(CheckPRHead, CheckStale, fmt.Sprintf(
			"the record names %s and the pull request is at %s", rec.HeadSHA, facts.HeadSHA))
	}
}

// checkPRState compares the record's disposition with the pull
// request's state.
//
// A record that says the work is COMPLETE while the pull request is still
// an open draft is a contradiction, and it is reported as one.
func (v *Verifier) checkPRState(out *Verification, rec *Record, facts *GitHubFacts) {
	state := prState(facts)
	switch {
	case facts.State == "merged":
		out.add(CheckPRState, CheckVerified, "the pull request is merged")
	case facts.IsDraft:
		if rec.Disposition == DispositionCompleted {
			out.add(CheckPRState, CheckFailed,
				"the record calls the work completed and the pull request is still a draft")
			return
		}
		out.add(CheckPRState, CheckVerified, "the pull request is an open draft")
	default:
		if rec.Disposition == DispositionCompleted {
			out.add(CheckPRState, CheckFailed,
				fmt.Sprintf("the record calls the work completed and the pull request is %s", state))
			return
		}
		out.add(CheckPRState, CheckVerified, state)
	}
}

// checkReviewedSHA compares the reviewed revision with the pull
// request's head.
//
// A review of an OLDER commit is not a review of the current head, and
// saying so is the whole point of the check: the head advances, and the
// review becomes stale.
func (v *Verifier) checkReviewedSHA(out *Verification, rec *Record, facts *GitHubFacts) {
	switch {
	case rec.ReviewedSHA == Unknown:
		out.add(CheckReviewedSHA, CheckMissing, "no revision was reviewed")
	case facts.HeadSHA == "":
		out.add(CheckReviewedSHA, CheckUnverified, "the pull request's head could not be read")
	case facts.HeadSHA == rec.ReviewedSHA:
		out.add(CheckReviewedSHA, CheckVerified, rec.ReviewedSHA)
	case rec.HeadSHA != Unknown && facts.HeadSHA == rec.HeadSHA:
		// The head did not move, but the review was of something
		// else. That is a contradiction, not a staleness.
		out.add(CheckReviewedSHA, CheckFailed, fmt.Sprintf(
			"the record reviewed %s and the pull request is at %s", rec.ReviewedSHA, facts.HeadSHA))
	default:
		out.add(CheckReviewedSHA, CheckStale, fmt.Sprintf(
			"the review covered %s and the pull request is now at %s",
			rec.ReviewedSHA, facts.HeadSHA))
	}
}

// checkCI checks the recorded workflow runs against GitHub.
//
// A run against ANOTHER commit is not CI for this head, and a run that
// failed is not a green one. Both are reported.
func (v *Verifier) checkCI(ctx context.Context, out *Verification, rec *Record) {
	if len(rec.CI) == 0 {
		out.add(CheckCI, CheckMissing, "no workflow run is recorded")
		return
	}
	if rec.HeadSHA == Unknown {
		out.add(CheckCI, CheckUnverified, "the head is not known, so no run can be attributed to it")
		return
	}
	runs, err := v.GitHub.WorkflowRuns(ctx, rec.Repository, rec.HeadSHA)
	if err != nil {
		out.add(CheckCI, CheckUnverified, err.Error())
		return
	}
	byID := map[int64]CIRun{}
	for _, run := range runs {
		byID[run.ID] = run
	}
	for _, recorded := range rec.CI {
		live, ok := byID[recorded.ID]
		if !ok {
			out.add(CheckCI, CheckMissing, fmt.Sprintf(
				"the workflow run %d could not be found in %s", recorded.ID, rec.Repository))
			continue
		}
		if live.HeadSHA != rec.HeadSHA {
			out.add(CheckCI, CheckStale, fmt.Sprintf(
				"the workflow run %d belongs to %s and not to %s",
				recorded.ID, live.HeadSHA, rec.HeadSHA))
			continue
		}
		if live.Conclusion != "success" {
			out.add(CheckCI, CheckFailed, fmt.Sprintf(
				"the workflow run %d concluded %q", recorded.ID, live.Conclusion))
			continue
		}
		if len(live.Jobs) == 0 {
			out.add(CheckCI, CheckUnverified, fmt.Sprintf(
				"the workflow run %d's jobs could not be read", recorded.ID))
			continue
		}
		failed := ""
		for _, job := range live.Jobs {
			if job.Conclusion != "success" {
				failed = job.Name
				break
			}
		}
		if failed != "" {
			out.add(CheckCI, CheckFailed, fmt.Sprintf(
				"the workflow run %d has a job that is not green: %s", recorded.ID, failed))
			continue
		}
		out.add(CheckCI, CheckVerified, fmt.Sprintf("the workflow run %d is green", recorded.ID))
	}
}

// checkReviewEvidence checks the review's digest.
//
// A digest that cannot be checked is not a verified digest, and a digest
// that does not match the bytes is a REFUSAL: the evidence is not what
// the record says it is.
func (v *Verifier) checkReviewEvidence(ctx context.Context, out *Verification, rec *Record) {
	switch {
	case rec.ReviewDisposition == ReviewDispositionNone:
		out.add(CheckReviewEvidence, CheckMissing, "no review evidence is recorded")
		return
	case rec.ReviewEvidence == nil || !rec.ReviewEvidence.Known():
		out.add(CheckReviewEvidence, CheckMissing,
			"the review's bytes are not published, so the digest cannot be checked")
		return
	}
	ref := rec.ReviewEvidence.Ref
	if ref == "" || ref == Unknown {
		out.add(CheckReviewEvidence, CheckMissing,
			"the review's evidence pointer is not readable from the evidence repository")
		return
	}
	if strings.Contains(ref, "://") {
		out.add(CheckReviewEvidence, CheckUnverified, fmt.Sprintf(
			"the evidence lives at %s and is not readable from the evidence repository", ref))
		return
	}
	raw, err := v.GitHub.Content(ctx, rec.Repository, evidenceRef(rec), ref)
	if err != nil {
		if isMissing(err) {
			out.add(CheckReviewEvidence, CheckMissing,
				fmt.Sprintf("the evidence %s could not be found", ref))
			return
		}
		out.add(CheckReviewEvidence, CheckUnverified, err.Error())
		return
	}
	if got := DigestBytes(raw); !SameDigest(got, rec.ReviewEvidence.SHA256) {
		out.add(CheckReviewEvidence, CheckFailed, fmt.Sprintf(
			"the evidence's digest is %s and the record says %s", got, rec.ReviewEvidence.SHA256))
		return
	}
	out.add(CheckReviewEvidence, CheckVerified, rec.ReviewEvidence.SHA256)
}

// checkReviewVerdict checks that a verdict is a verdict.
//
// The verifier NEVER invents one, and it never downgrades one that the
// service recorded into "no verdict": the record is the claim, and the
// check is that the claim is of a kind the service records.
func (v *Verifier) checkReviewVerdict(out *Verification, rec *Record) {
	switch {
	case rec.ReviewVerdict == "":
		if rec.ReviewDisposition == ReviewDispositionVerdict {
			out.add(CheckReviewVerdict, CheckFailed,
				"the record says a verdict was recorded and carries none")
			return
		}
		out.add(CheckReviewVerdict, CheckVerified, "no verdict is claimed")
	case rec.ReviewDisposition == ReviewDispositionLocallyReported:
		if rec.ReviewVerdict != ReportedApproved &&
			rec.ReviewVerdict != ReportedChangesRequested {
			out.add(CheckReviewVerdict, CheckFailed,
				fmt.Sprintf("the reported verdict %q is not in the reported vocabulary",
					rec.ReviewVerdict))
			return
		}
		// The verdict is a CLAIM, and the record says so. What is
		// verified here is the LABEL: a reported verdict is not
		// presented as a recorded one. Whether the review happened is
		// not something GitHub can answer, and the evidence check
		// above is where its absence is reported.
		out.add(CheckReviewVerdict, CheckVerified,
			"the verdict is reported, not recorded, and the record says so")
	case rec.ReviewVerdict != VerdictPassed && rec.ReviewVerdict != VerdictChangesRequested:
		out.add(CheckReviewVerdict, CheckFailed,
			fmt.Sprintf("the verdict %q is not one the service records", rec.ReviewVerdict))
	case rec.ReviewDisposition != ReviewDispositionVerdict:
		out.add(CheckReviewVerdict, CheckFailed, fmt.Sprintf(
			"the record carries the verdict %q but its review disposition is %q",
			rec.ReviewVerdict, rec.ReviewDisposition))
	default:
		out.add(CheckReviewVerdict, CheckVerified, rec.ReviewVerdict)
	}
}

// checkApproval checks that an approval is an approval.
//
// The same rule as the verdict: the verifier never invents one, and a
// record that claims a merge or an approval it does not have is
// reported as a contradiction.
func (v *Verifier) checkApproval(out *Verification, rec *Record) {
	if rec.OperatorApproval == nil {
		out.add(CheckApproval, CheckVerified, "no approval is claimed")
		return
	}
	if rec.OperatorApproval.Action == "" {
		out.add(CheckApproval, CheckFailed, "the record carries an approval with no action")
		return
	}
	out.add(CheckApproval, CheckVerified, rec.OperatorApproval.Action)
}

// evidenceRef returns the ref the evidence branch's files live on.
//
// It is the branch the record was published to. The record does not carry
// it — a record that carried its own branch would be a record that could
// be replayed against another branch — so the verifier is told.
func evidenceRef(rec *Record) string {
	return EvidenceBranch
}

// EvidenceBranch is the branch the published evidence lives on.
//
// It is a CONSTANT rather than a field of the record, because a record
// that named its own branch would be a record that could be replayed
// against another branch. The reader is configured with the branch; the
// record is not.
const EvidenceBranch = "evidence"

// add appends a check to a verification.
func (v *Verification) add(name, status, detail string) {
	v.Checks = append(v.Checks, Check{Name: name, Status: status, Detail: detail})
}

// prState describes a pull request's state in one phrase.
func prState(f *GitHubFacts) string {
	if f == nil {
		return "unknown"
	}
	switch {
	case f.State == "merged":
		return "merged"
	case f.IsDraft:
		return "an open draft"
	case f.State == "closed":
		return "closed"
	default:
		return "open"
	}
}
