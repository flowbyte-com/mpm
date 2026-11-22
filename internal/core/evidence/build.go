package evidence

import (
	"fmt"
	"regexp"
	"sort"
	"strconv"
	"strings"
)

// The BUILD step: claims and facts become a record.
//
// # THE TWO SIDES
//
// A CLAIM is what the assignment store says about a task: who it was
// addressed to, what state it is in, which revision was reviewed, what
// the findings digest was. The store is the authority for those, and it
// is the only authority.
//
// A FACT is what GitHub says: the pull request's head, whether it is
// merged, what the workflow runs concluded. GitHub is the authority for
// those, and the only authority.
//
// The record carries BOTH, in different fields, with different provenance
// categories. It never merges them into one "status" that a reader could
// not take apart.

// Claims are what the assignment store says about one task.
//
// This is the RECORDED side. It is a claim, and publishing it does not
// make it true.
type Claims struct {
	// ID is the assignment.
	ID string
	// RootID is the tree it belongs to.
	RootID string
	// ProjectID is the project it belongs to.
	ProjectID string
	// Title is the assignment's title.
	Title string
	// Objective is the assignment's one-line objective.
	Objective string
	// Recipient is who the work was addressed to.
	Recipient string
	// Role is implementer or reviewer.
	Role string
	// State is the assignment's state.
	State string
	// IssueRef is the issue the work answers.
	IssueRef string
	// PRRef is the pull request the work produced.
	PRRef string
	// BaseSHA is the implementation's base.
	BaseSHA string
	// HeadSHA is the implementation's head.
	HeadSHA string
	// ReviewedSHA is the revision the reviewer examined.
	ReviewedSHA string
	// FindingsSHA256 is the digest of the host-captured findings.
	FindingsSHA256 string
	// EvidenceRef is where the captured evidence lives.
	EvidenceRef string
	// ReviewTarget is the assignment a reviewer reviews.
	//
	// It comes from the reviewer's payload, which the DELEGATION fixed:
	// a reviewer cannot be re-pointed at another subject afterwards.
	ReviewTarget string
	// AwaitingAction is the operator action the task is waiting for.
	AwaitingAction string
	// Approval is the operator's approval, when one was recorded.
	Approval *Approval
	// Verdict is the reviewer's recorded verdict, when the ledger
	// carries one.
	//
	// It is read from the ledger, not inferred from a state: a verdict
	// is a recorded decision, and a state is not a decision.
	Verdict string
	// UpdatedAt is the task's last update.
	UpdatedAt int64
}

// Facts are what GitHub says about one task's pull request.
//
// This is the OBSERVED side.
type Facts struct {
	// GitHub are the pull request's facts.
	GitHub *GitHubFacts
	// CI are the workflow runs GitHub reported.
	CI []CIRun
}

// Tree is one root assignment and its tasks.
type Tree struct {
	// RootID is the root assignment.
	RootID string
	// Tasks are the assignments in the tree, in any order.
	Tasks []Claims
}

// ReviewInfo is what is known about the review of one task.
//
// It is SEPARATE from the task's own claims because a review is recorded
// on TWO rows: the reviewer carries the captured evidence, and the
// subject carries the verdict.
type ReviewInfo struct {
	// Exists says a reviewer was delegated for this task.
	Exists bool
	// ReviewerID is that reviewer.
	ReviewerID string
	// ReviewedSHA is the revision the reviewer examined.
	ReviewedSHA string
	// FindingsSHA256 is the digest of the host-captured findings.
	FindingsSHA256 string
	// EvidenceRef is where the captured evidence lives.
	EvidenceRef string
	// Verdict is the recorded verdict, if any.
	Verdict string
	// LocallyReported says the review is asserted from OUTSIDE the
	// substrate — a session that reported it, or an operator's note.
	//
	// Such a review is published as a CLAIM. It is never dressed up as
	// a verdict the service recorded.
	LocallyReported bool
}

// Build turns one tree and its facts into the tree's records.
//
// The records are returned in a stable order — by root, then by task id —
// so the publication is deterministic.
func Build(tree Tree, facts map[string]Facts) ([]*Record, error) {
	if tree.RootID == "" {
		return nil, fmt.Errorf("evidence: the tree names no root assignment")
	}
	reviews := correlateReviews(tree)
	claims := append([]Claims(nil), tree.Tasks...)
	sortClaims(claims)

	out := make([]*Record, 0, len(claims))
	for _, c := range claims {
		rec, err := buildOne(c, reviews[c.ID], facts[c.ID])
		if err != nil {
			return nil, err
		}
		out = append(out, rec)
	}
	return out, nil
}

// buildOne turns one task's claims, its review and its facts into a
// record.
func buildOne(c Claims, review ReviewInfo, facts Facts) (*Record, error) {
	if c.ID == "" {
		return nil, fmt.Errorf("evidence: a task has no id")
	}
	r := &Redactor{}
	rec := &Record{
		FormatVersion:     FormatVersion,
		Repository:        Unknown,
		Project:           c.ProjectID,
		RootID:            c.RootID,
		TaskID:            c.ID,
		Title:             c.Title,
		Objective:         c.Objective,
		Recipient:         c.Recipient,
		Role:              c.Role,
		State:             c.State,
		IssueRef:          c.IssueRef,
		PRRef:             c.PRRef,
		BaseSHA:           unknownSHA(c.BaseSHA),
		HeadSHA:           unknownSHA(c.HeadSHA),
		ReviewedSHA:       Unknown,
		ReviewDisposition: ReviewDispositionNone,
		Provenance:        ProvenanceFor(),
	}

	// THE REDACTION PASS. It runs over every string the record carries,
	// and it runs BEFORE the digest, so what is published is what was
	// digested.
	rec.Title, _ = r.Redact(FieldTitle, rec.Title)
	rec.Objective, _ = r.Redact(FieldObjective, rec.Objective)
	rec.IssueRef, _ = r.Redact(FieldIssueRef, rec.IssueRef)
	rec.PRRef, _ = r.Redact(FieldPRRef, rec.PRRef)
	rec.PendingOperatorAction, _ = r.Redact(FieldPendingOperator, c.AwaitingAction)
	rec.Recipient, _ = r.Redact(FieldRecipient, rec.Recipient)
	if c.Approval != nil {
		approval := *c.Approval
		approval.Action, _ = r.Redact(FieldOperatorApproval+".action", approval.Action)
		approval.Source, _ = r.Redact(FieldOperatorApproval+".source", approval.Source)
		approval.Evidence, _ = r.Redact(FieldOperatorApproval+".evidence", approval.Evidence)
		rec.OperatorApproval = &approval
	}

	// THE REVIEW.
	//
	// Three things are kept apart, and the record shows which one it
	// has: the reviewer's captured findings, the reviewer's recorded
	// verdict, and a review asserted from outside the substrate.
	switch {
	case review.LocallyReported:
		rec.ReviewDisposition = ReviewDispositionLocallyReported
	case review.Exists && review.Verdict != "":
		rec.ReviewDisposition = ReviewDispositionVerdict
		rec.ReviewVerdict = review.Verdict
	case review.Exists && (review.FindingsSHA256 != "" || review.EvidenceRef != ""):
		rec.ReviewDisposition = ReviewDispositionFindingsCaptured
	default:
		rec.ReviewDisposition = ReviewDispositionNone
	}
	if review.ReviewedSHA != "" {
		rec.ReviewedSHA = unknownSHA(review.ReviewedSHA)
	}
	if rec.ReviewDisposition != ReviewDispositionNone {
		rec.ReviewEvidence = &EvidenceRef{
			Ref:    review.EvidenceRef,
			SHA256: unknownDigest(review.FindingsSHA256),
		}
		if rec.ReviewEvidence.Ref == "" {
			rec.ReviewEvidence.Ref = Unknown
		}
	}

	// THE OBSERVED SIDE.
	if facts.GitHub != nil {
		gh := *facts.GitHub
		rec.GitHub = &gh
		if gh.Repository != "" {
			rec.Repository = gh.Repository
		}
	}
	if len(facts.CI) > 0 {
		rec.CI = facts.CI
	}

	// THE DISPOSITION. It is derived from the record's OWN fields, by
	// one function, so the human report and the machine report cannot
	// disagree about where a task sits.
	rec.Disposition = DeriveDisposition(rec)

	if len(r.Withheld) > 0 {
		rec.Redactions = r.Withheld
	}
	if err := Validate(rec); err != nil {
		return nil, err
	}
	if err := AssertPublishable(rec); err != nil {
		return nil, err
	}
	return rec, nil
}

// DeriveDisposition says where a task sits in the engineering queue.
//
// The rules, in order:
//
//  1. CANCELLED work is blocked.
//  2. A review that ASKED FOR CHANGES is blocked: the work needs a new
//     assignment, and the old one is the record of what was tried.
//  3. A PENDING OPERATOR ACTION is waiting for a person.
//  4. FINISHED work with no green CI is waiting for CI.
//  5. FINISHED work with green CI and nothing pending is completed.
//  6. FINISHED work with no review verdict is waiting for a review.
//  7. Everything else is active.
func DeriveDisposition(r *Record) string {
	switch r.State {
	case "cancelled":
		return DispositionBlocked
	}
	if r.ReviewVerdict == VerdictChangesRequested {
		return DispositionBlocked
	}
	if r.PendingOperatorAction != "" && r.PendingOperatorAction != Unknown {
		return DispositionAwaitingOperator
	}
	if r.State == "awaiting_operator" {
		return DispositionAwaitingOperator
	}
	finished := r.State == "completed" || r.State == "review_passed" ||
		r.State == "operator_approved"
	if finished {
		green, known, _ := CIVerdict(r.CI, r.HeadSHA)
		if !green {
			if !known {
				// The CI is UNKNOWN, and an unknown CI is not a green
				// one: the work is waiting for a CI that nobody has
				// established yet.
				return DispositionAwaitingCI
			}
			return DispositionAwaitingCI
		}
		return DispositionCompleted
	}
	if r.State == "enqueued" || r.State == "delivered" || r.State == "acknowledged" ||
		r.State == "accepted" || r.State == "in_progress" {
		return DispositionActive
	}
	return DispositionActive
}

// CIVerdict says whether the CI for a head is green.
//
// It returns (green, known, reason). "Known" says whether the runs were
// visible at all: an invisible CI is not a green one, and the two are
// different claims.
//
// The runs are matched to the HEAD: a run against another commit is not
// CI for this head, and counting it would be exactly the mistake the
// evidence contract exists to prevent.
func CIVerdict(runs []CIRun, headSHA string) (green bool, known bool, reason string) {
	if headSHA == "" || headSHA == Unknown {
		return false, false, "the head is not known, so no run can be attributed to it"
	}
	matched := make([]CIRun, 0, len(runs))
	for _, run := range runs {
		if run.HeadSHA == headSHA {
			matched = append(matched, run)
		}
	}
	if len(matched) == 0 {
		return false, false, "no workflow run is recorded for the head"
	}
	for _, run := range matched {
		if run.Status != "" && run.Status != "completed" {
			return false, true, "a workflow run is still " + run.Status
		}
		if run.Conclusion != "success" {
			if run.Conclusion == "" {
				return false, true, "a workflow run has no conclusion yet"
			}
			return false, true, "a workflow run concluded " + run.Conclusion
		}
		if len(run.Jobs) == 0 {
			// The run's own conclusion is GitHub's aggregate, but the
			// JOBS are what the reader needs to see, and a run whose
			// jobs cannot be read cannot be shown to be green.
			return false, false, "the workflow run's jobs could not be read"
		}
		for _, job := range run.Jobs {
			if job.Status != "" && job.Status != "completed" {
				return false, true, "the job " + job.Name + " is still " + job.Status
			}
			if job.Conclusion != "success" {
				if job.Conclusion == "" {
					return false, true, "the job " + job.Name + " has no conclusion yet"
				}
				return false, true, "the job " + job.Name + " concluded " + job.Conclusion
			}
		}
	}
	return true, true, ""
}

// correlateReviews finds the review of every task in a tree.
//
// A reviewer names its subject in its payload, and the delegation fixed
// that name when the reviewer was created. So the correlation is read, not
// guessed.
//
// A task with SEVERAL reviewers takes the most recently updated one: the
// later review is the one that says what the task's state is now.
func correlateReviews(tree Tree) map[string]ReviewInfo {
	out := map[string]ReviewInfo{}
	reviewers := make([]Claims, 0, len(tree.Tasks))
	for _, c := range tree.Tasks {
		if c.Role == "reviewer" && c.ReviewTarget != "" {
			reviewers = append(reviewers, c)
		}
	}
	sortClaims(reviewers)
	for _, rev := range reviewers {
		info := ReviewInfo{
			Exists:         true,
			ReviewerID:     rev.ID,
			ReviewedSHA:    rev.ReviewedSHA,
			FindingsSHA256: rev.FindingsSHA256,
			EvidenceRef:    rev.EvidenceRef,
			Verdict:        rev.Verdict,
		}
		// A reviewer's own record is about the REVIEW it performed.
		out[rev.ID] = info
		// The reviewers are sorted by update time, so the LAST one to
		// touch a subject is the one that says what the subject's
		// state is now. An earlier review of the same subject is
		// history, and publishing it as the subject's review would
		// publish a superseded verdict.
		out[rev.ReviewTarget] = info
	}
	return out
}

// sortClaims orders claims by update time, then by id.
//
// The order is total and deterministic, so the correlation and the
// publication are too.
func sortClaims(claims []Claims) {
	sort.SliceStable(claims, func(i, j int) bool {
		a, b := claims[i], claims[j]
		if a.UpdatedAt != b.UpdatedAt {
			return a.UpdatedAt < b.UpdatedAt
		}
		return a.ID < b.ID
	})
}

// PRNumber extracts the pull request number from a reference.
//
// It accepts "#262", "262" and a full pull URL. A reference it cannot
// read is an error rather than a guess: a wrong number would publish a
// pointer to somebody else's pull request.
func PRNumber(ref string) (int, error) {
	ref = strings.TrimSpace(ref)
	if ref == "" || ref == Unknown {
		return 0, fmt.Errorf("evidence: the reference names no pull request")
	}
	if m := pullURLPattern.FindStringSubmatch(ref); m != nil {
		n, err := strconv.Atoi(m[1])
		if err != nil {
			return 0, fmt.Errorf("evidence: the pull request reference %q names no number", ref)
		}
		return n, nil
	}
	trimmed := strings.TrimPrefix(ref, "#")
	n, err := strconv.Atoi(trimmed)
	if err != nil {
		return 0, fmt.Errorf("evidence: the pull request reference %q names no number", ref)
	}
	return n, nil
}

var pullURLPattern = regexp.MustCompile(`/pull/(\d+)`)

// unknownSHA returns the explicit unknown for a SHA nobody knows.
//
// It NEVER returns the current head, and it never returns an empty
// string: an empty field reads as "nobody said", and a missing SHA has
// to read as "unknown".
func unknownSHA(value string) string {
	value = strings.TrimSpace(value)
	if value == "" {
		return Unknown
	}
	return value
}

// unknownDigest returns the explicit unknown for a digest nobody knows.
func unknownDigest(value string) string {
	value = strings.TrimSpace(value)
	if value == "" {
		return Unknown
	}
	return value
}
