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
	// Branch is the branch the evidence lives on.
	//
	// It is the READER's configuration, not the record's: a record that
	// named its own branch could be replayed against another one. And
	// it must be threaded through, because the review's bytes live on
	// the same branch as the index — and a verifier that looked for them
	// somewhere else would report a missing capture for a capture that
	// is right there.
	Branch string
	// Repository is the repository the EVIDENCE lives in.
	//
	// It is the READER's configuration, and it is not the record's:
	// a record names the repository the WORK is visible in, which is
	// not necessarily the repository the evidence was published to.
	//
	// It must be threaded through for the same reason the branch is.
	// A verifier that fetched the review's bytes from the RECORD's
	// repository would read from wherever a published record says to
	// read — and the operator's publication target is a choice, not a
	// field of the work.
	Repository string
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
	v.checkReviewVerdict(out, rec, facts)
	v.checkApproval(out, rec, facts)
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
		// THE RECORD'S OWN ACCOUNT OF THE RUN. A record that names a
		// workflow run for ANOTHER commit is contradicting itself, and
		// the contradiction is reported before the run is even
		// fetched.
		if recorded.HeadSHA != "" && recorded.HeadSHA != rec.HeadSHA {
			out.add(CheckCI, CheckFailed, fmt.Sprintf(
				"the record's workflow run %d belongs to %s and the record's head is %s",
				recorded.ID, recorded.HeadSHA, rec.HeadSHA))
			continue
		}
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
		// THE PUBLISHER'S OWN ANSWER IS THE DETAIL.
		//
		// The record says whether the publisher CHECKED the digest,
		// and a reader deserves to know why there is no digest to
		// check: a refuted digest and an unpublished one are
		// different facts.
		detail := "the review's bytes are not published, so the digest cannot be checked"
		if rec.ReviewEvidence != nil {
			switch rec.ReviewEvidence.Status {
			case CaptureContradicted:
				detail = "the publisher refuted the digest: the bytes did not hash to it"
			case CaptureUnverifiable:
				detail = "the publisher could not read the bytes, so the digest was not published"
			}
		}
		out.add(CheckReviewEvidence, CheckMissing, detail)
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
	// THE POINTER IS DATA.
	//
	// It comes out of a published record, and a published record is
	// something a reader may have fetched from anywhere. So the
	// verifier does not fetch whatever it is told to: it fetches a
	// path inside the evidence repository, and nothing else.
	if !safeEvidencePath(ref) {
		out.add(CheckReviewEvidence, CheckFailed, fmt.Sprintf(
			"the evidence's pointer %q does not name a path inside the evidence repository", ref))
		return
	}
	repository := v.evidenceRepository()
	if repository == "" {
		// THE READER DID NOT SAY WHERE THE EVIDENCE LIVES.
		//
		// There is no fallback to the record's own repository: a record
		// is DATA, and a verifier that read from wherever a record
		// said to read would fetch from a repository nobody chose —
		// with the reader's own credential. So the check reports that
		// it could not be made.
		out.add(CheckReviewEvidence, CheckUnverified,
			"the reader did not say which repository the evidence lives in")
		return
	}
	raw, err := v.GitHub.Content(ctx, repository, v.evidenceRef(), ref)
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

// checkReviewVerdict checks the verdict a record carries.
//
// # WHAT IT CAN AND CANNOT ESTABLISH
//
// A verdict is the substrate's own record: a reviewer's fenced
// transition. GitHub has no idea it happened, so this check CANNOT
// establish that it did — and it does not pretend to.
//
// What it DOES establish is everything a reader can check from here:
// that the claim is well formed, that it is labelled as what it is,
// and that it covers the revision the pull request is actually at. A
// verdict of "passed" for a revision the head has moved past is STALE,
// and saying "verified" would tell a reader that the work in front of
// them was approved.
func (v *Verifier) checkReviewVerdict(out *Verification, rec *Record, facts *GitHubFacts) {
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
		if stale, detail := staleVerdict(rec, facts); stale {
			out.add(CheckReviewVerdict, CheckStale, detail)
			return
		}
		// The verdict is a CLAIM, and the record says so. What is
		// verified here is the claim's SHAPE and its REVISION — not
		// that the review happened, which is the substrate's business
		// and is reported by the evidence check above.
		out.add(CheckReviewVerdict, CheckVerified, fmt.Sprintf(
			"the verdict %q is reported, not recorded, and it covers the pull request's head",
			rec.ReviewVerdict))
	case rec.ReviewVerdict != VerdictPassed && rec.ReviewVerdict != VerdictChangesRequested:
		out.add(CheckReviewVerdict, CheckFailed,
			fmt.Sprintf("the verdict %q is not one the service records", rec.ReviewVerdict))
	case rec.ReviewDisposition != ReviewDispositionVerdict:
		out.add(CheckReviewVerdict, CheckFailed, fmt.Sprintf(
			"the record carries the verdict %q but its review disposition is %q",
			rec.ReviewVerdict, rec.ReviewDisposition))
	default:
		if stale, detail := staleVerdict(rec, facts); stale {
			out.add(CheckReviewVerdict, CheckStale, detail)
			return
		}
		out.add(CheckReviewVerdict, CheckVerified, fmt.Sprintf(
			"the record carries the recorded verdict %q and it covers the pull request's head",
			rec.ReviewVerdict))
	}
}

// staleVerdict says whether a verdict covers a revision the pull
// request has moved past.
func staleVerdict(rec *Record, facts *GitHubFacts) (bool, string) {
	if facts == nil || facts.HeadSHA == "" {
		return false, ""
	}
	if rec.ReviewedSHA == Unknown || rec.ReviewedSHA == "" {
		return false, ""
	}
	if facts.HeadSHA == rec.ReviewedSHA {
		return false, ""
	}
	return true, fmt.Sprintf("the verdict covered %s and the pull request is at %s",
		rec.ReviewedSHA, facts.HeadSHA)
}

// checkApproval checks the approval a record carries.
//
// # WHAT IT CAN AND CANNOT ESTABLISH
//
// An approval is the OPERATOR's decision. GitHub has no idea it
// happened, so this check cannot establish that it did — and it does
// not pretend to.
//
// What it does establish is what a reader can check from here: that the
// approval names a revision, that the revision is the one the pull
// request is at, and that an approval to merge is not published for a
// pull request that is not merged.
func (v *Verifier) checkApproval(out *Verification, rec *Record, facts *GitHubFacts) {
	if rec.OperatorApproval == nil {
		out.add(CheckApproval, CheckVerified, "no approval is claimed")
		return
	}
	if rec.OperatorApproval.Action == "" {
		out.add(CheckApproval, CheckFailed, "the record carries an approval with no action")
		return
	}
	// THE REVISION. An approval of another revision is an approval of
	// something else.
	if facts != nil && facts.HeadSHA != "" && rec.OperatorApproval.Revision != "" &&
		rec.OperatorApproval.Revision != Unknown && facts.HeadSHA != rec.OperatorApproval.Revision {
		out.add(CheckApproval, CheckStale, fmt.Sprintf(
			"the approval covered %s and the pull request is at %s",
			rec.OperatorApproval.Revision, facts.HeadSHA))
		return
	}
	// THE ACTION. An approval to merge a pull request that is not
	// merged is a contradiction the reader can see.
	if facts != nil && mentionsMerge(rec.OperatorApproval.Action) &&
		facts.State != "merged" {
		out.add(CheckApproval, CheckFailed, fmt.Sprintf(
			"the record carries an approval to %q and the pull request is %s",
			rec.OperatorApproval.Action, prState(facts)))
		return
	}
	out.add(CheckApproval, CheckVerified, fmt.Sprintf(
		"the record carries the operator's approval of %q", rec.OperatorApproval.Action))
}

// mentionsMerge says whether an approval's action is about merging.
//
// It is a WORD test, and it is deliberately narrow: the approval's
// action is free text, and the one action anybody can check against
// GitHub is a merge.
func mentionsMerge(action string) bool {
	return strings.Contains(strings.ToLower(action), "merge")
}

// safeEvidencePath says whether a pointer names a path inside the
// evidence repository.
//
// A pointer is DATA: it comes out of a published record, and a
// published record may have been fetched from anywhere. So the verifier
// fetches a path inside the evidence repository and nothing else — no
// absolute path, no parent directory, no scheme.
func safeEvidencePath(ref string) bool {
	if ref == "" || strings.Contains(ref, "://") {
		return false
	}
	if strings.HasPrefix(ref, "/") || strings.Contains(ref, "\\") {
		return false
	}
	for _, part := range strings.Split(ref, "/") {
		if part == "" || part == "." || part == ".." {
			return false
		}
	}
	return true
}

// evidenceRef returns the ref the evidence branch's files live on.
//
// It is the branch the record was published to. The record does not carry
// it — a record that carried its own branch would be a record that could
// be replayed against another branch — so the verifier is told.
func (v *Verifier) evidenceRef() string {
	if v.Branch != "" {
		return v.Branch
	}
	return EvidenceBranch
}

// evidenceRepository returns the repository the evidence files live in.
//
// It is the READER's repository — the one the index was read from — and
// not the record's: the record names the repository the WORK is visible
// in, and the operator's publication target is a separate choice.
//
// # THERE IS NO FALLBACK
//
// A record is DATA: it may have been fetched from anywhere, and its
// repository is a field in it. A verifier that fell back to it would
// read from wherever a published record said to read — with the
// reader's own credential — and a forged record would choose the
// repository. So a verifier that was not told where the evidence lives
// says that the check could not be made.
func (v *Verifier) evidenceRepository() string {
	return v.Repository
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
