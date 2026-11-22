// Package evidence publishes and verifies the engineering evidence of a
// Continuity 001 assignment tree.
//
// # WHAT THIS IS
//
// PR 4 gave MPM a way to RUN work and to RECOVER it. It did not give
// another session, a reviewer or an operator a way to DISCOVER that work
// and to CHECK it. This package is that layer: it turns the assignment
// store's claims and GitHub's facts into a versioned, machine-readable
// record, publishes it to a location an independent reader can reach, and
// verifies the record against GitHub before anybody believes it.
//
// THE THREE THINGS THIS PACKAGE NEVER CONFLATES
//
//  1. A RUNTIME REPORTING SUCCESS is not task completion.
//  2. CAPTURED FINDINGS are not a review verdict.
//  3. A REVIEW VERDICT is not an operator approval.
//
// Each of them is a different fact, lives in a different field, and carries
// a different provenance category. A record that blurred them would be
// worse than no record, because it would be believed.
//
// THE PROVENANCE CATEGORIES
//
//   - observed: a fact about the outside world, checked against GitHub
//   - recorded: a claim the assignment store makes about itself
//   - decision: a reviewer's verdict, or an operator's approval
//
// A record says which category each of its fields belongs to, so a reader
// never has to guess whether a value was CHECKED or merely ASSERTED.
//
// # WHAT THIS PACKAGE DOES NOT DO
//
// It does not publish a review verdict that no reviewer recorded, and it
// does not publish an operator approval that no operator gave. Where the
// evidence is missing, the field says so explicitly.
package evidence

import (
	"fmt"
	"strings"
)

// FormatVersion is the version of the published evidence format.
//
// It is in every record and in every index. A reader that does not
// recognise the version says so rather than guessing at the fields.
//
// # THE VERSION COVERS THE DERIVATIONS TOO
//
// A record is not only its fields: the queue's filing is DERIVED from
// them, and the human artefacts are RENDERED from them. Those rules
// are part of the contract, so a change to a derivation or a rendering
// changes what a reader must accept — and it changes THIS. Otherwise a
// reader would check an old publication's bytes against rules that
// publication never used and call legitimate evidence forged, while
// the only compatibility signal said nothing had changed.
const FormatVersion = "mpm.evidence/v2"

// Unknown is the explicit value of a field nobody knows.
//
// It is a VALUE, not an absence: an unknown SHA is the string "unknown",
// never the current branch head, never an empty string that a reader
// might mistake for a fact. A field that is unknown says so in the place
// a reader will look.
const Unknown = "unknown"

// The three provenance categories.
const (
	// CategoryObserved is a fact about the outside world. It was checked
	// against GitHub, and a reader can check it again.
	CategoryObserved = "observed"

	// CategoryRecorded is a claim the assignment store makes about
	// itself. It is what the work reported; it is not independently
	// established.
	CategoryRecorded = "recorded"

	// CategoryDecision is somebody's decision: a reviewer's verdict, or
	// an operator's approval. It is never inferred from anything else.
	CategoryDecision = "decision"
)

// The dispositions — where a task sits in the engineering queue.
//
// These are the SECTIONS the `check` report is built from, and they are
// derived from the record's own fields by one function, so the human
// report and the machine report cannot disagree about a task's place.
const (
	// DispositionActive is work in flight.
	DispositionActive = "active"

	// DispositionAwaitingReview is implemented work that no reviewer
	// has recorded a verdict for.
	DispositionAwaitingReview = "awaiting_review"

	// DispositionAwaitingCI is work whose CI is not green — or whose CI
	// is not known yet.
	DispositionAwaitingCI = "awaiting_ci"

	// DispositionAwaitingOperator is work that is waiting for a person.
	DispositionAwaitingOperator = "awaiting_operator"

	// DispositionBlocked is work that cannot proceed: it was cancelled,
	// or it was refused.
	DispositionBlocked = "blocked"

	// DispositionAmbiguous is work whose outcome could not be
	// determined. It is not a failure, and it is not a success.
	DispositionAmbiguous = "ambiguous"

	// DispositionCompleted is work that finished.
	DispositionCompleted = "completed"
)

// The dispositions, in the order the report prints them.
var dispositionOrder = []string{
	DispositionActive,
	DispositionAwaitingReview,
	DispositionAwaitingCI,
	DispositionAwaitingOperator,
	DispositionBlocked,
	DispositionAmbiguous,
	DispositionCompleted,
}

// Dispositions returns the disposition vocabulary, in report order.
func Dispositions() []string {
	out := make([]string, len(dispositionOrder))
	copy(out, dispositionOrder)
	return out
}

// The review dispositions — what is known about a review.
//
// The DISTINCTION is the point. Captured findings are BYTES the host
// captured over what a reviewer produced; a verdict is the reviewer's own
// fenced transition. A review that produced findings but never recorded a
// verdict is a review whose findings exist and whose verdict does not.
const (
	// ReviewDispositionNone means no review evidence exists at all.
	ReviewDispositionNone = "none"

	// ReviewDispositionFindingsCaptured means the host captured the
	// reviewer's findings, and the DIGEST of those bytes is recorded.
	// It does NOT mean the review approved anything.
	ReviewDispositionFindingsCaptured = "findings_captured"

	// ReviewDispositionVerdict means a reviewer recorded a verdict —
	// passed or changes_requested — through the fenced review path.
	ReviewDispositionVerdict = "verdict"

	// ReviewDispositionLocallyReported means a review is asserted by
	// something outside the substrate: a session that reported it, or a
	// note an operator wrote. It is a CLAIM, and it is published as a
	// claim.
	ReviewDispositionLocallyReported = "locally_reported"
)

// The review verdicts the assignment service records.
const (
	VerdictPassed           = "passed"
	VerdictChangesRequested = "changes_requested"
)

// The REPORTED verdicts.
//
// A review that happened OUTSIDE the substrate — in a contained session,
// in a note an operator wrote — has a verdict the service never
// recorded. It is published as what it is: a REPORTED verdict, with its
// own vocabulary, and never as the service's verdict.
//
// # WHY THE VOCABULARY IS PREFIXED
//
// The service records `passed` and `changes_requested`. A reader that saw
// `changes_requested` could not tell a reported one from a recorded one
// unless it also read the review disposition — and the whole point of
// keeping the two apart is that a reader should not have to.
// So every reported verdict says so in the value itself: `reported_…`.
// The vocabulary is not a nicety; it is the difference between a claim
// and a record, made visible without looking anything else up.
const (
	ReportedApproved         = "reported_approved"
	ReportedChangesRequested = "reported_changes_requested"
)

// EvidenceRef points at the bytes a piece of evidence consists of.
//
// The reference is a POINTER — a URL, a path, an identifier — and the
// digest is the CHECK over those bytes. A reference without a digest
// points at something nobody can verify, and a digest without a reference
// points at nothing.
type EvidenceRef struct {
	// Ref is where the bytes live.
	Ref string `json:"ref"`
	// SHA256 is the digest of the bytes, in the form "sha256:<hex>".
	//
	// It is Unknown when the bytes were never captured durably. It is
	// NEVER the digest of something else: a missing digest is a missing
	// digest.
	SHA256 string `json:"sha256"`
	// Status says whether the publisher CHECKED the digest against the
	// bytes.
	//
	// It is "verified" when the bytes were read and they hash to the
	// digest, "unverifiable" when they could not be read, and
	// "contradicted" when they were read and they do NOT hash to it.
	//
	// A digest that was never checked says so here, so a reader can
	// tell a checked digest from an unchecked one without looking
	// anywhere else.
	Status string `json:"status,omitempty"`
}

// Known reports whether the digest is a real digest.
func (e *EvidenceRef) Known() bool {
	return e != nil && e.SHA256 != "" && e.SHA256 != Unknown
}

// CIRun is one CI workflow run, as GitHub reported it.
//
// This is an OBSERVED fact: the publisher reads it from GitHub, and a
// reader checks it against GitHub again.
type CIRun struct {
	// ID is the workflow run id.
	ID int64 `json:"id"`
	// Name is the workflow's name.
	Name string `json:"name"`
	// HeadSHA is the commit the run belongs to.
	//
	// A run against a DIFFERENT commit is not CI for this head, and the
	// verifier says so rather than counting it.
	HeadSHA string `json:"head_sha"`
	// Status is the run's status: queued, in_progress or completed.
	Status string `json:"status"`
	// Conclusion is the run's conclusion. It is empty while the run has
	// not finished.
	Conclusion string `json:"conclusion"`
	// URL points at the run.
	URL string `json:"url,omitempty"`
	// Jobs are the run's jobs, as GitHub reported them.
	//
	// They are published because a run's own conclusion and its jobs'
	// conclusions are not the same claim, and a reader must be able to
	// see that a job is missing.
	Jobs []CIJob `json:"jobs,omitempty"`
}

// CIJob is one job of a workflow run.
type CIJob struct {
	Name       string `json:"name"`
	Status     string `json:"status"`
	Conclusion string `json:"conclusion"`
	URL        string `json:"url,omitempty"`
}

// Approval is an operator's approval, as the assignment service recorded
// it.
//
// This is a DECISION. It is published only when the store holds one.
type Approval struct {
	// Action is what the operator approved.
	Action string `json:"action"`
	// Revision is the revision the operator approved.
	Revision string `json:"revision"`
	// Source is where the approval came from.
	Source string `json:"source,omitempty"`
	// Evidence points at the evidence behind the approval.
	Evidence string `json:"evidence,omitempty"`
	// ApprovedAt is when it was recorded.
	ApprovedAt int64 `json:"approved_at,omitempty"`
}

// GitHubFacts are the facts GitHub reported about a pull request.
//
// Everything in here is OBSERVED. Nothing in here came from the
// assignment store, and a reader can check every field of it again.
type GitHubFacts struct {
	// Repository is "owner/name".
	Repository string `json:"repository"`
	// Number is the pull request number.
	Number int `json:"number"`
	// State is open, closed or merged.
	State string `json:"state"`
	// IsDraft says whether the pull request is still a draft.
	IsDraft bool `json:"is_draft"`
	// Title is the pull request's title.
	Title string `json:"title,omitempty"`
	// HeadSHA is the pull request's current head.
	HeadSHA string `json:"head_sha"`
	// BaseSHA is the pull request's base.
	BaseSHA string `json:"base_sha,omitempty"`
	// MergeCommit is the merge commit, when it is merged.
	MergeCommit string `json:"merge_commit,omitempty"`
	// MergedAt is when it was merged.
	MergedAt string `json:"merged_at,omitempty"`
	// URL points at the pull request.
	URL string `json:"url,omitempty"`
	// Reviews are the pull request's OWN GitHub reviews.
	//
	// They are NOT the host-captured findings of a contained reviewer:
	// a contained reviewer's verdict is a decision in the assignment
	// store, and these are GitHub's own review objects. Publishing one
	// as the other would invent a GitHub review that does not exist.
	Reviews []GitHubReview `json:"reviews,omitempty"`
}

// GitHubReview is one GitHub-native review.
type GitHubReview struct {
	Author string `json:"author,omitempty"`
	State  string `json:"state"`
	Body   string `json:"body,omitempty"`
	// SHA is the commit the review was made against.
	SHA string `json:"sha,omitempty"`
}

// Provenance says which category each of the record's fields belongs to.
//
// It is published WITH the record rather than implied by it, so a reader
// does not have to know this package's rules to know what it is reading.
type Provenance struct {
	// Observed are the fields verified against GitHub.
	Observed []string `json:"observed"`
	// Recorded are the fields the assignment store asserts.
	Recorded []string `json:"recorded"`
	// Decisions are the fields that are somebody's decision.
	Decisions []string `json:"decisions"`
}

// Record is one task's engineering evidence.
//
// It is the unit that is published, and it is IMMUTABLE: the same
// evidence always produces the same bytes, and therefore the same digest.
// What changes between publications is the index that points at the
// records, not the records.
type Record struct {
	// FormatVersion is the evidence format this record conforms to.
	FormatVersion string `json:"format_version"`

	// Repository is "owner/name" — where the work is visible.
	Repository string `json:"repository"`

	// Project is the project the work belongs to, as the assignment
	// store names it.
	Project string `json:"project"`

	// RootID is the root assignment of the tree.
	RootID string `json:"root_id"`

	// TaskID is the assignment this record is about.
	TaskID string `json:"task_id"`

	// Title is the assignment's title.
	Title string `json:"title"`

	// Objective is the assignment's one-line objective.
	//
	// It is the sentence a reader reads first, so it is bounded and it
	// is REDACTED like every other free-text field: an objective that
	// named a credential or a private path would be withheld.
	Objective string `json:"objective"`

	// Recipient is who the work was addressed to.
	Recipient string `json:"recipient"`

	// Role is the assignment's role: implementer or reviewer.
	Role string `json:"role"`

	// State is the assignment's state, as the store reports it.
	State string `json:"state"`

	// Disposition is where this work sits in the queue.
	Disposition string `json:"disposition"`

	// IssueRef is the issue the work answers.
	IssueRef string `json:"issue_ref,omitempty"`

	// PRRef is the pull request the work produced.
	PRRef string `json:"pr_ref,omitempty"`

	// BaseSHA is the implementation's base commit.
	BaseSHA string `json:"base_sha"`

	// HeadSHA is the implementation's head commit.
	HeadSHA string `json:"head_sha"`

	// ReviewedSHA is the exact revision that was reviewed.
	//
	// It is Unknown when no review happened. It is NEVER the current
	// head: a record that substituted the head for an unknown reviewed
	// revision would turn "nothing was reviewed" into "the current work
	// was reviewed".
	ReviewedSHA string `json:"reviewed_sha"`

	// ReviewEvidence points at the review's bytes and their digest.
	ReviewEvidence *EvidenceRef `json:"review_evidence,omitempty"`

	// ReviewDisposition says what is known about the review.
	ReviewDisposition string `json:"review_disposition"`

	// ReviewVerdict is the reviewer's verdict, when one was recorded.
	//
	// It is empty unless ReviewDisposition is "verdict". A captured
	// finding is not a verdict, and this field is where that difference
	// is visible.
	ReviewVerdict string `json:"review_verdict,omitempty"`

	// CI are the workflow runs GitHub reported for the head.
	CI []CIRun `json:"ci,omitempty"`

	// PendingOperatorAction is what the operator is being asked for.
	//
	// It is empty when nothing is pending, and it is NEVER a merge that
	// happened: an action that was taken is not pending.
	PendingOperatorAction string `json:"pending_operator_action,omitempty"`

	// OperatorApproval is the operator's approval, when one was
	// recorded.
	OperatorApproval *Approval `json:"operator_approval,omitempty"`

	// GitHub are the facts GitHub reported about the pull request.
	GitHub *GitHubFacts `json:"github,omitempty"`

	// Provenance says which category each field belongs to.
	Provenance Provenance `json:"provenance"`

	// Redactions names the fields whose content was withheld because it
	// looked like a secret or a private path.
	//
	// It names the FIELDS, never the content. A reader learns that a
	// field was withheld without learning what was in it.
	Redactions []string `json:"redactions,omitempty"`
}

// The record's field names, as the provenance categories list them.
//
// They are constants rather than string literals scattered across the
// package, so a field cannot be renamed in one place and forgotten in
// another: the provenance lists and the validation walk the same names.
const (
	FieldRepository        = "repository"
	FieldProject           = "project"
	FieldRootID            = "root_id"
	FieldTaskID            = "task_id"
	FieldTitle             = "title"
	FieldObjective         = "objective"
	FieldRecipient         = "recipient"
	FieldRole              = "role"
	FieldState             = "state"
	FieldDisposition       = "disposition"
	FieldIssueRef          = "issue_ref"
	FieldPRRef             = "pr_ref"
	FieldBaseSHA           = "base_sha"
	FieldHeadSHA           = "head_sha"
	FieldReviewedSHA       = "reviewed_sha"
	FieldReviewEvidence    = "review_evidence"
	FieldReviewDisposition = "review_disposition"
	FieldReviewVerdict     = "review_verdict"
	FieldCI                = "ci"
	FieldPendingOperator   = "pending_operator_action"
	FieldOperatorApproval  = "operator_approval"
	FieldGitHub            = "github"
)

// observedFields are the fields a reader checks against GitHub.
//
// The GitHub facts themselves are observed, and so are the fields that
// GitHub is the only authority for.
var observedFields = []string{
	FieldRepository,
	FieldGitHub,
	FieldCI,
}

// recordedFields are the fields the assignment store asserts.
//
// # THE DISPOSITION IS A DERIVATION, NOT AN ASSERTION
//
// `disposition` is listed here because it is computed from the record's
// OWN fields — the state, the verdict, the pending action — and every one
// of those is something the store asserts about itself. But the
// derivation also folds in the OBSERVED CI, so the category says where
// the record's fields come from, not that the disposition is a bare
// assertion.
//
// A reader told "disposition: recorded" must not read it as "the store
// said so". So the record says what the disposition IS: it is derived,
// by one function, from the fields the record carries — and the record
// carries those fields, with their own categories, so the reader can
// take the derivation apart.
var recordedFields = []string{
	FieldProject,
	FieldRootID,
	FieldTaskID,
	FieldTitle,
	FieldObjective,
	FieldRecipient,
	FieldRole,
	FieldState,
	FieldDisposition,
	FieldIssueRef,
	FieldPRRef,
	FieldBaseSHA,
	FieldHeadSHA,
	FieldReviewedSHA,
	FieldReviewEvidence,
	FieldReviewDisposition,
	FieldPendingOperator,
}

// decisionFields are the fields that are somebody's decision.
var decisionFields = []string{
	FieldReviewVerdict,
	FieldOperatorApproval,
}

// ProvenanceFor returns the provenance a record carries.
//
// The categories are ASSIGNED, not chosen by the caller: a field's
// category is a property of what the field IS, and a publisher that could
// label a verdict as an observation would be able to lie by labelling.
func ProvenanceFor() Provenance {
	return Provenance{
		Observed:  append([]string(nil), observedFields...),
		Recorded:  append([]string(nil), recordedFields...),
		Decisions: append([]string(nil), decisionFields...),
	}
}

// CategoryOf returns the provenance category of a field.
func CategoryOf(field string) string {
	for _, f := range observedFields {
		if f == field {
			return CategoryObserved
		}
	}
	for _, f := range decisionFields {
		if f == field {
			return CategoryDecision
		}
	}
	for _, f := range recordedFields {
		if f == field {
			return CategoryRecorded
		}
	}
	return ""
}

// Validate checks that a record is well formed before it is published.
//
// It refuses the things that would make a record believable but wrong:
//
//   - a SHA that is neither a commit id nor the explicit unknown;
//   - a verdict without a verdict disposition;
//   - a disposition outside the vocabulary;
//   - a provenance category that does not match the field's category.
//
// A record that fails validation is not published. A record that is
// published but wrong is worse, because it is believed.
func Validate(r *Record) error {
	if r == nil {
		return fmt.Errorf("evidence: there is no record to validate")
	}
	if r.FormatVersion != FormatVersion {
		return fmt.Errorf("evidence: the record's format version is %q, want %q",
			r.FormatVersion, FormatVersion)
	}
	if strings.TrimSpace(r.Repository) == "" {
		return fmt.Errorf("evidence: the record names no repository")
	}
	if strings.TrimSpace(r.TaskID) == "" {
		return fmt.Errorf("evidence: the record names no task")
	}
	if strings.TrimSpace(r.RootID) == "" {
		return fmt.Errorf("evidence: the record names no root assignment")
	}
	for name, value := range map[string]string{
		FieldBaseSHA:     r.BaseSHA,
		FieldHeadSHA:     r.HeadSHA,
		FieldReviewedSHA: r.ReviewedSHA,
	} {
		if err := validSHA(value); err != nil {
			return fmt.Errorf("evidence: %s: %w", name, err)
		}
	}
	if !knownDisposition(r.Disposition) {
		return fmt.Errorf("evidence: the disposition %q is not in the vocabulary",
			r.Disposition)
	}
	if !knownReviewDisposition(r.ReviewDisposition) {
		return fmt.Errorf("evidence: the review disposition %q is not in the vocabulary",
			r.ReviewDisposition)
	}
	if r.ReviewVerdict != "" {
		switch r.ReviewDisposition {
		case ReviewDispositionVerdict:
			if r.ReviewVerdict != VerdictPassed && r.ReviewVerdict != VerdictChangesRequested {
				return fmt.Errorf("evidence: the verdict %q is not in the recorded vocabulary",
					r.ReviewVerdict)
			}
		case ReviewDispositionLocallyReported:
			if r.ReviewVerdict != ReportedApproved &&
				r.ReviewVerdict != ReportedChangesRequested {
				return fmt.Errorf("evidence: the reported verdict %q is not in the reported vocabulary",
					r.ReviewVerdict)
			}
		default:
			return fmt.Errorf(
				"evidence: the record carries the verdict %q but its review disposition is %q; "+
					"a verdict is only a verdict when a reviewer recorded one",
				r.ReviewVerdict, r.ReviewDisposition)
		}
	}
	if r.OperatorApproval != nil {
		if strings.TrimSpace(r.OperatorApproval.Action) == "" {
			return fmt.Errorf("evidence: the record carries an approval with no action")
		}
		if r.ReviewDisposition == "" {
			return fmt.Errorf("evidence: the record carries an approval and no review " +
				"disposition; an approval is not a review")
		}
	}
	if r.PendingOperatorAction != "" && r.OperatorApproval != nil {
		// The store keeps the AWAITED action on the row after the
		// operator acts on it, so a record may carry both an approval
		// and the action that was asked for. What it may not carry is
		// the SAME action as both pending and taken.
		if r.PendingOperatorAction == r.OperatorApproval.Action {
			return fmt.Errorf(
				"evidence: the record carries the approval of %q and asks for %q; "+
					"an action that was taken is not pending",
				r.OperatorApproval.Action, r.PendingOperatorAction)
		}
	}
	want := ProvenanceFor()
	if !sameStrings(r.Provenance.Observed, want.Observed) ||
		!sameStrings(r.Provenance.Recorded, want.Recorded) ||
		!sameStrings(r.Provenance.Decisions, want.Decisions) {
		return fmt.Errorf("evidence: the record's provenance does not match the fields it carries")
	}
	return nil
}

// validSHA accepts a commit id or the explicit unknown, and nothing else.
//
// The point of the unknown is that it is DISTINGUISHABLE. A validator
// that accepted an empty string would make "nobody said" and "somebody
// said nothing" the same value, and a reader could not tell them apart.
func validSHA(value string) error {
	switch value {
	case Unknown:
		return nil
	case "":
		return fmt.Errorf("the value is empty; say %q instead", Unknown)
	}
	if len(value) != 40 {
		return fmt.Errorf("the value %q is not a commit id and not %q", value, Unknown)
	}
	for _, c := range value {
		if !strings.ContainsRune("0123456789abcdef", c) {
			return fmt.Errorf("the value %q is not a commit id and not %q", value, Unknown)
		}
	}
	return nil
}

func knownDisposition(d string) bool {
	for _, known := range dispositionOrder {
		if known == d {
			return true
		}
	}
	return false
}

func knownReviewDisposition(d string) bool {
	switch d {
	case ReviewDispositionNone, ReviewDispositionFindingsCaptured,
		ReviewDispositionVerdict, ReviewDispositionLocallyReported:
		return true
	}
	return false
}

func sameStrings(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}
