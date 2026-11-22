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
	// Repository is "owner/name" — where the work is visible.
	//
	// It is a CLAIM, and it is what the publisher reads the
	// assignment's own pull request reference from. The OBSERVED
	// repository — the one GitHub reports — takes precedence when the
	// facts could be read, and this one is what a reader is given when
	// they could not.
	Repository string
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
	// CaptureStatus says whether the publisher CHECKED that digest
	// against the bytes.
	//
	// It is set by the publisher, not by the store: the store holds a
	// claim, and the publisher is what checks it.
	CaptureStatus string
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
	// CaptureStatus says whether the publisher CHECKED that digest
	// against the bytes.
	//
	// It is set by the publisher, not by the store: the store holds a
	// claim, and the publisher is what checks it.
	CaptureStatus string
	// EvidenceRef is where the captured evidence live.
	EvidenceRef string
	// Verdict is the recorded verdict, if any.
	Verdict string
	// LocallyReported says the review is asserted from OUTSIDE the
	// substrate — a session that reported it, or an operator's note.
	//
	// Such a review is published as a CLAIM. It is never dressed up as
	// a verdict the service recorded.
	LocallyReported bool
	// UpdatedAt is when the review was last updated.
	//
	// It is what decides which of two reviews of the same subject is
	// the current one.
	UpdatedAt int64
}

// Build turns one tree and its facts into the tree's records.
//
// The records are returned in a stable order — by root, then by task id —
// so the publication is deterministic.
func Build(tree Tree, facts map[string]Facts) ([]*Record, error) {
	return BuildWithReviews(tree, facts, nil)
}

// BuildWithReviews turns one tree, its facts and an extra set of review
// facts into the tree's records.
//
// # THE NEWEST REVIEW WINS
//
// Two reviewers can review one subject: the head advanced and the work
// was re-reviewed. The LATER review says what the subject's state is
// now, and publishing the earlier one would publish a SUPERSEDED
// verdict as the current one — the failure the evidence contract exists
// to prevent.
//
// So every candidate review for a task is compared and only the newest
// is used. The tie is broken by the reviewer's id, so the result does
// not depend on the order the candidates arrived in.
//
// # TWO ACCOUNTS OF ONE REVIEW ARE MERGED, NOT CHOSEN BETWEEN
//
// A review can be described twice: the tree's own correlation reads the
// reviewer's CLAIM, and the corpus's candidates read the LEDGER. Those
// are two halves of ONE review, not two accounts of it — so they are
// merged, and the candidate's verdict and capture status fill in what
// the claim did not carry.
//
// Before this, the two competed: the claim won the tie, and the
// subject's record was published with NO verdict at all — which reads
// as "no review happened" and files finished work under
// "awaiting review" forever.
//
// # THE ACCOUNTS ARE A SET, AND THE ORDER DECIDES NOTHING
//
// The published review is a function of the ACCOUNTS, not of the order
// they arrived in: the order is the caller's accident, not a fact about
// the work.
//
// So the accounts are GROUPED by the review they describe — the
// reviewer's id and the moment — and each group is merged on its own.
// Two accounts of one review are two halves of it, and the merge keeps
// what EITHER of them knows.
//
// A merge that folded the accounts in ARRIVAL order could not deliver
// that: the first account to fill a gap took the gap, and the account
// that should have filled it later could not — so the same two
// accounts, in the other order, published a different verdict.
//
// # THE GROUPS COMPETE, AND THE LATER ONE WINS
//
// Two DIFFERENT reviews of one subject compete: the head advanced and
// the work was re-reviewed. The LATER review says what the subject's
// state is now, and publishing the earlier one would publish a
// SUPERSEDED verdict as the current one.
//
// # A SUPERSEDED GROUP IS DROPPED WHOLE
//
// A group whose moment is EARLIER than the current group's describes the
// review as it WAS. Its verdict is a decision the current account does
// not carry, and publishing it would file the work as blocked on the
// strength of a decision that has been superseded.
//
// So a superseded group contributes NOTHING — not even a field the
// current one happens to be missing. That is the hole the first merge
// had: it filled the gap from whichever account it held, so an older
// account's verdict came back through the gap, and the tie rule could
// not take it back because the gap was no longer empty.
//
// # AN ACCOUNT WITH NO MOMENT IS NOT SUPERSEDED
//
// "I do not know when this happened" is not "this happened before the
// newer account". So a group with no moment is never superseded, and —
// when it is about the SAME review — it still fills the gaps the
// current one leaves. It is how a REPORTED review, which names no
// reviewer and carries no moment, reaches the reader.
//
// The rule lives HERE, and not in one caller, because it is part of the
// contract: a caller that built its own map could get it wrong, and the
// map is not what a reader sees.
func BuildWithReviews(tree Tree, facts map[string]Facts,
	extra map[string][]ReviewInfo) ([]*Record, error) {
	if tree.RootID == "" {
		return nil, fmt.Errorf("evidence: the tree names no root assignment")
	}
	reviews := correlateReviews(tree)
	for taskID, candidates := range extra {
		// THE TREE'S OWN ACCOUNT IS ONE OF THE ACCOUNTS.
		//
		// The correlation reads the reviewer's CLAIM and the candidates
		// read the LEDGER. They are accounts of the same review, and
		// they are resolved together — so the resolution cannot depend
		// on which of them the caller happened to hold first.
		accounts := make([]ReviewInfo, 0, len(candidates)+1)
		accounts = append(accounts, candidates...)
		if own, ok := reviews[taskID]; ok {
			accounts = append(accounts, own)
		}
		reviews[taskID] = resolveReview(accounts)
	}
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

// resolveReview resolves the accounts of one review into the account
// that is published.
//
// It is a function of the SET of accounts: the same accounts in any
// order resolve to the same published review.
func resolveReview(accounts []ReviewInfo) ReviewInfo {
	if len(accounts) == 0 {
		return ReviewInfo{}
	}
	// THE ACCOUNTS, GROUPED BY THE REVIEW THEY DESCRIBE.
	groups := map[string][]ReviewInfo{}
	keys := make([]string, 0, len(accounts))
	for _, a := range accounts {
		key := reviewGroupKey(a)
		if _, seen := groups[key]; !seen {
			keys = append(keys, key)
		}
		groups[key] = append(groups[key], a)
	}
	// The groups are visited in a FIXED order, so the resolution does
	// not depend on the order the accounts arrived in.
	sort.Strings(keys)

	// THE GROUPS, EACH MERGED ON ITS OWN.
	resolved := make([]ReviewInfo, 0, len(keys))
	for _, key := range keys {
		resolved = append(resolved, unionReview(groups[key]))
	}

	// THE CURRENT REVIEW. The latest moment wins, and a tie is broken by
	// the accounts' own fields.
	sort.SliceStable(resolved, func(i, j int) bool {
		return newerReview(resolved[i], resolved[j])
	})
	current := resolved[0]
	out := current
	for _, other := range resolved[1:] {
		if supersededReview(other, current) {
			// A SUPERSEDED GROUP IS DROPPED WHOLE.
			//
			// It describes the review as it was, and folding it in
			// would publish a decision the current account does not
			// carry.
			continue
		}
		if !fillsTheGaps(other, current) {
			// A DIFFERENT REVIEW. It competed, and it lost.
			continue
		}
		out = mergeReview(out, other)
	}
	return out
}

// reviewGroupKey is the identity of the review an account describes.
//
// It is the reviewer's id and the moment: two accounts with the same
// key are two halves of ONE review, and two accounts with different
// keys are two DIFFERENT reviews that compete.
func reviewGroupKey(a ReviewInfo) string {
	return a.ReviewerID + "\x00" + strconv.FormatInt(a.UpdatedAt, 10)
}

// unionReview merges the accounts of ONE review into one account.
//
// It is a pure union: every field is taken from the account that
// carries it, and where two accounts carry different values for the
// same field, the one whose fields compare GREATEST wins.
//
// So the union does not depend on the order the accounts arrived in —
// and nothing that only ONE account knows is lost.
func unionReview(accounts []ReviewInfo) ReviewInfo {
	ordered := make([]ReviewInfo, len(accounts))
	copy(ordered, accounts)
	sort.SliceStable(ordered, func(i, j int) bool {
		return reviewKey(ordered[i]) > reviewKey(ordered[j])
	})
	out := ReviewInfo{}
	for _, a := range ordered {
		out = mergeReview(out, a)
	}
	return out
}

// newerReview says whether a review is more recent than another.
//
// The comparison is on the update time, and an account whose time is
// UNKNOWN never wins a comparison it cannot make: "I do not know when
// this happened" is not "this happened later".
//
// A tie is broken by the accounts' OWN fields, in a fixed order, so two
// accounts of the same moment are resolved the same way whatever order
// they arrived in.
func newerReview(a, b ReviewInfo) bool {
	switch {
	case a.UpdatedAt == 0 && b.UpdatedAt == 0:
		return reviewKey(a) > reviewKey(b)
	case a.UpdatedAt == 0:
		return false
	case b.UpdatedAt == 0:
		return true
	case a.UpdatedAt != b.UpdatedAt:
		return a.UpdatedAt > b.UpdatedAt
	}
	return reviewKey(a) > reviewKey(b)
}

// reviewKey is an account's own fields, in a fixed order.
//
// It is the tie-break: two accounts of the same moment that disagree
// are resolved by it. It is NOT the order the accounts arrived in — it
// is a property of the accounts — so the published review never depends
// on the caller's accident.
//
// The rule is arbitrary but FIXED, and that is what matters: the
// contract says the published review is a function of the accounts, not
// of the order the caller found them in. The account whose fields
// compare GREATEST is the one that wins a disagreement.
func reviewKey(a ReviewInfo) string {
	return strings.Join([]string{
		a.ReviewerID,
		a.ReviewedSHA,
		a.FindingsSHA256,
		a.CaptureStatus,
		a.EvidenceRef,
		strconv.FormatBool(a.LocallyReported),
		strconv.FormatBool(a.Exists),
		a.Verdict,
	}, "\x00")
}

// supersededReview says whether an account describes the review as it
// WAS.
//
// An account is superseded when the current account knows a LATER
// moment and this one knows an EARLIER moment. An account with no
// moment is never superseded: nobody can say it happened before.
func supersededReview(account, current ReviewInfo) bool {
	if account.UpdatedAt == 0 || current.UpdatedAt == 0 {
		return false
	}
	return account.UpdatedAt < current.UpdatedAt
}

// fillsTheGaps says whether an account may fill the gaps of the
// published one.
//
// An account that names the SAME reviewer is an account of the same
// review — the claim and the ledger's account of it — so it fills what
// the published one does not carry.
//
// An account that names NO reviewer is a REPORTED review: one that
// happened outside the substrate, and that the publisher was told
// about. It cannot be told apart from the account of the same review,
// so it is merged rather than chosen between — because a dropped
// candidate is a verdict that never reaches the reader.
//
// An account that names a DIFFERENT reviewer is a different review: it
// competed for the subject, and it lost.
func fillsTheGaps(other, current ReviewInfo) bool {
	if other.ReviewerID == "" {
		return true
	}
	return other.ReviewerID == current.ReviewerID
}

// mergeReview folds an account into the one that is published.
//
// It only FILLS GAPS: where the published account already says
// something, the account folded in does not overrule it.
//
// # WHY THE FOLDED ACCOUNT DOES NOT OVERRULE
//
// The published account is the CURRENT one — the latest moment — and a
// disagreement between the current account and another is a
// disagreement between the review as it is and the review as it was.
// The current one wins.
//
// # WHY A SUPERSEDED ACCOUNT MAY NOT EVEN FILL A GAP
//
// A superseded account is dropped whole, before it reaches this
// function. Before that rule existed, the merge filled the gaps from
// whichever account it held — so an older account's verdict came back
// through a gap the current account happened to have, and the tie rule
// could not take it back because the gap was no longer empty.
//
// # THE CORRELATED FIELDS TRAVEL TOGETHER
//
// A verdict says something ABOUT A REVISION, and a status says
// something about BYTES: it is the publisher's answer ABOUT that digest.
// Those are two facts, and each is carried by one account.
//
// So the verdict brings its revision with it, and the capture's digest
// brings its status and its pointer with it. Taking the revision from
// one account and the verdict from another would publish a decision
// about a revision that no account ever paired with it — and taking
// the digest from one account and the status from another would
// publish an answer about bytes that account never saw.
//
// # THE VERDICT AND ITS PROVENANCE TRAVEL TOGETHER
//
// A verdict reported from outside the substrate is published as one, and
// a verdict the substrate recorded is published as one. Merging them
// apart would let a record claim a reported verdict as a recorded one.
func mergeReview(base, other ReviewInfo) ReviewInfo {
	out := base
	if other.Exists {
		out.Exists = true
	}
	if out.ReviewerID == "" {
		out.ReviewerID = other.ReviewerID
	}
	// THE DECISION AND THE REVISION IT IS ABOUT.
	if out.Verdict == "" && other.Verdict != "" {
		out.Verdict = other.Verdict
		out.LocallyReported = other.LocallyReported
		// The revision comes with the verdict — even when that means
		// the published revision is EMPTY, because a revision from
		// another account would be a revision that account never
		// paired with this verdict.
		out.ReviewedSHA = other.ReviewedSHA
	} else if out.Verdict == "" && out.ReviewedSHA == "" {
		// No account has said anything about a revision, so the
		// revision may be taken on its own.
		out.ReviewedSHA = other.ReviewedSHA
	}
	// THE CAPTURE AND THE PUBLISHER'S ANSWER ABOUT IT.
	//
	// The DIGEST-bearing account owns the triple: a pointer with no
	// digest is an unchecked pointer, and the account that names bytes
	// is the one whose status and pointer are about those bytes.
	if out.FindingsSHA256 == "" {
		out.FindingsSHA256 = other.FindingsSHA256
		out.CaptureStatus = other.CaptureStatus
		out.EvidenceRef = other.EvidenceRef
	} else if SameDigest(out.FindingsSHA256, other.FindingsSHA256) {
		// The SAME digest: the answer and the pointer may be filled
		// in, because they are about these bytes.
		if out.CaptureStatus == "" {
			out.CaptureStatus = other.CaptureStatus
		}
		if out.EvidenceRef == "" {
			out.EvidenceRef = other.EvidenceRef
		}
	}
	if other.UpdatedAt > out.UpdatedAt {
		out.UpdatedAt = other.UpdatedAt
	}
	return out
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
		Repository:        unknownOr(c.Repository),
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
		// AN ACTION THAT WAS TAKEN IS NOT PENDING.
		//
		// The store keeps the awaited action on the row after the
		// operator acts on it, because the action is the record of
		// what was asked. A record that published both would say the
		// same action is pending and taken, and the queue would show
		// the work twice.
		if rec.PendingOperatorAction == approval.Action {
			rec.PendingOperatorAction = ""
		}
	}

	// THE REVIEW.
	//
	// Three things are kept apart, and the record shows which one it
	// has: the reviewer's captured findings, the reviewer's recorded
	// verdict, and a review asserted from outside the substrate.
	switch {
	case review.LocallyReported:
		rec.ReviewDisposition = ReviewDispositionLocallyReported
		// The REPORTED verdict is carried as what it is: a verdict
		// that was reported outside the substrate, published with its
		// own vocabulary and never as the service's verdict.
		rec.ReviewVerdict = review.Verdict
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
			Status: review.CaptureStatus,
		}
		// THE POINTER IS SCREENED LIKE EVERY OTHER STRING.
		//
		// The store records the capture where the HOST wrote it, which
		// is an absolute path on the publisher's machine. Publishing it
		// would put the operator's disk layout inside the
		// content-addressed bytes, and it would make the checker fetch
		// a path the record chose. So a pointer to a local path is
		// withheld — and the record names the field it withheld.
		rec.ReviewEvidence.Ref, _ = r.Redact(FieldReviewEvidence+".ref", rec.ReviewEvidence.Ref)
		if rec.ReviewEvidence.Ref == "" {
			rec.ReviewEvidence.Ref = Unknown
		}
		if rec.ReviewEvidence.Status == "" {
			rec.ReviewEvidence.Status = CaptureNone
		}
		// A DIGEST THE PUBLISHER DID NOT ESTABLISH IS NOT PUBLISHED.
		//
		// Two statuses mean the publisher could not establish the
		// digest: `unverifiable` (the bytes are somewhere the
		// publisher cannot read) and `contradicted` (the bytes are
		// there and they do NOT hash to the digest). In the first
		// case the digest is an UNCHECKED claim; in the second it is
		// a REFUTED one. Publishing either would publish a claim the
		// publisher knows it cannot stand behind — and the digest
		// lives inside the content-addressed bytes, so it would
		// become part of the record's permanent identity.
		//
		// So both become the explicit unknown, and the STATUS says
		// which of them happened.
		if rec.ReviewEvidence.Status == CaptureContradicted ||
			rec.ReviewEvidence.Status == CaptureUnverifiable {
			rec.ReviewEvidence.SHA256 = Unknown
		}
	}

	// THE OBSERVED SIDE.
	//
	// The facts are COPIED, deeply: the redaction below rewrites the
	// copies, and the reader's own values must not change underneath
	// it. And they are SCREENED, because the pull request's title, a
	// review's body and a workflow's name are third-party text that the
	// substrate never saw.
	if facts.GitHub != nil {
		gh := *facts.GitHub
		gh.Reviews = append([]GitHubReview(nil), facts.GitHub.Reviews...)
		rec.GitHub = &gh
		if gh.Repository != "" {
			rec.Repository = gh.Repository
		}
	}
	if len(facts.CI) > 0 {
		rec.CI = copyRuns(facts.CI)
	}
	r.redactObserved(rec.GitHub, rec.CI)

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

// copyRuns copies a set of workflow runs and their jobs.
//
// The copy is DEEP: the redaction rewrites the names inside the jobs,
// and the reader's own runs must not change underneath it.
func copyRuns(runs []CIRun) []CIRun {
	out := make([]CIRun, 0, len(runs))
	for _, run := range runs {
		copied := run
		if len(run.Jobs) > 0 {
			copied.Jobs = append([]CIJob(nil), run.Jobs...)
		}
		out = append(out, copied)
	}
	return out
}

// DeriveDisposition says where a task sits in the engineering queue.
//
// The rules, in order:
//
//  1. CANCELLED work is blocked.
//  2. A REVIEWER'S OWN RECORD IS FILED BY THE REVIEW (see below).
//  3. A review that ASKED FOR CHANGES is blocked: the work needs a new
//     assignment, and the old one is the record of what was tried.
//  4. A PENDING OPERATOR ACTION is waiting for a person.
//  5. FINISHED work with no verdict of any kind is waiting for a review.
//  6. FINISHED work whose CI is not green is waiting for CI.
//  7. FINISHED work whose review covered ANOTHER revision is ambiguous:
//     the outcome for the current head cannot be determined.
//  8. FINISHED work with green CI and nothing pending is completed.
//  9. Work in a state this publisher knows is active.
//  10. Work in a state this publisher does NOT know is ambiguous: the
//     substrate says something the record cannot place, and a record
//     that claimed a place would be inventing one.
//
// # A REVIEWER'S OWN RECORD IS FILED BY THE REVIEW
//
// The rules below are the SUBJECT's rules: they ask whether the WORK's
// CI is green and whether a verdict about the WORK is outstanding.
// Neither question is about the review itself.
//
// A reviewer's assignment IS the review. So a finished review is DONE
// work, and it is filed as completed — whatever verdict it reached
// belongs to the subject it judged, and the subject's own record
// carries that verdict. Without this rule a finished review was filed
// under "awaiting review", which tells an operator the queue is waiting
// for a reviewer who has already finished.
//
// # WHY A REPORTED VERDICT COUNTS AS A REVIEW
//
// Rule 5 asks whether there is a VERDICT, not whether the substrate
// recorded one. A review that happened in a contained session and was
// reported afterwards has a verdict; it is published as a REPORTED
// verdict, the record says so, and the review-evidence check reports
// that its bytes cannot be verified. Filing it as "awaiting review"
// would say that no review happened, which is false.
func DeriveDisposition(r *Record) string {
	switch r.State {
	case "cancelled":
		return DispositionBlocked
	}
	if r.Role == "reviewer" {
		if finishedState(r.State) {
			return DispositionCompleted
		}
		if activeState(r.State) {
			return DispositionActive
		}
		return DispositionAmbiguous
	}
	if r.ReviewVerdict == VerdictChangesRequested || r.ReviewVerdict == ReportedChangesRequested {
		return DispositionBlocked
	}
	if r.PendingOperatorAction != "" && r.PendingOperatorAction != Unknown {
		return DispositionAwaitingOperator
	}
	if r.State == "awaiting_operator" {
		return DispositionAwaitingOperator
	}
	if finishedState(r.State) {
		if r.ReviewVerdict == "" {
			// FINISHED work with no verdict is waiting for a REVIEWER,
			// not for CI. Filing it under CI would tell an operator
			// the queue is waiting on the wrong thing.
			return DispositionAwaitingReview
		}
		green, known, _ := CIVerdict(r.CI, r.HeadSHA)
		if !green {
			// The CI is UNKNOWN, and an unknown CI is not a green
			// one: the work is waiting for a CI that nobody has
			// established yet.
			_ = known
			return DispositionAwaitingCI
		}
		if r.ReviewedSHA != Unknown && r.HeadSHA != Unknown && r.ReviewedSHA != r.HeadSHA {
			// The review covered another revision, so the work's
			// OUTCOME for this head cannot be determined: the approval
			// is not about what is in front of the reader.
			return DispositionAmbiguous
		}
		return DispositionCompleted
	}
	if activeState(r.State) {
		return DispositionActive
	}
	return DispositionAmbiguous
}

// finishedState says whether a state means the work is DONE.
func finishedState(state string) bool {
	switch state {
	case "completed", "review_passed", "operator_approved":
		return true
	}
	return false
}

// activeState says whether a state means the work is IN FLIGHT.
func activeState(state string) bool {
	switch state {
	case "enqueued", "delivered", "acknowledged", "accepted", "in_progress":
		return true
	}
	return false
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
			CaptureStatus:  rev.CaptureStatus,
			EvidenceRef:    rev.EvidenceRef,
			Verdict:        rev.Verdict,
			UpdatedAt:      rev.UpdatedAt,
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

// RepositoryFromRef reads "owner/name" out of a repository reference.
//
// A pull request's URL names the repository the work is visible in, and
// that is where a publisher looks for it. It is the store's own claim
// about where the work went, and the OBSERVED repository replaces it
// when GitHub could be asked.
//
// A reference it cannot read is an error rather than a guess: a wrong
// repository would publish the evidence about somebody else's project.
func RepositoryFromRef(ref string) (string, error) {
	ref = strings.TrimSpace(ref)
	if ref == "" || ref == Unknown {
		return "", fmt.Errorf("evidence: the reference names no repository")
	}
	rest := ref
	if i := strings.Index(ref, "://"); i >= 0 {
		scheme := ref[:i]
		rest = ref[i+3:]
		if scheme != "http" && scheme != "https" {
			return "", fmt.Errorf("evidence: the reference %q is not an HTTP reference", ref)
		}
		if j := strings.Index(rest, "/"); j >= 0 {
			rest = rest[j+1:]
		} else {
			rest = ""
		}
	}
	parts := strings.Split(rest, "/")
	if len(parts) < 2 || parts[0] == "" || parts[1] == "" {
		return "", fmt.Errorf("evidence: the reference %q names no repository", ref)
	}
	if len(parts) > 2 {
		switch parts[2] {
		case "pull", "pulls", "issues", "commit", "commits", "tree", "blob":
			parts = parts[:2]
		default:
			return "", fmt.Errorf("evidence: the reference %q names no repository", ref)
		}
	}
	if _, _, err := ParseRepository(parts[0] + "/" + parts[1]); err != nil {
		return "", err
	}
	return parts[0] + "/" + parts[1], nil
}

// unknownOr returns the explicit unknown for an empty value.
func unknownOr(value string) string {
	value = strings.TrimSpace(value)
	if value == "" {
		return Unknown
	}
	return value
}

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
