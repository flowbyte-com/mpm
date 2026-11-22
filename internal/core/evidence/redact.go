package evidence

import (
	"fmt"
	"regexp"
	"strings"
)

// The REDACTION rules.
//
// A published record is a thing strangers read. It must not carry a
// credential, a private path, or a transcript of somebody's session.
//
// The rules are applied at BUILD time, before the digest, so what is
// published is what was digested: a reader that re-hashes the record
// gets the same bytes the publisher hashed.
//
// # REDACT, DO NOT REFUSE
//
// A record with a secret in its title is still evidence, and refusing to
// publish it would hide the work. So the offending VALUE is replaced with
// the explicit unknown, and the record NAMES the field that was
// withheld. A reader learns that a field was withheld without learning
// what was in it.
//
// # TRANSCRIPTS ARE PREVENTED STRUCTURALLY
//
// The record has no field that carries model output. A review's findings
// are published as a DIGEST and a pointer, never as text. The length and
// line rules below are the second line of defence, for the fields that do
// carry free text.

// maxPublishedString is the longest string a record may carry.
//
// It is short enough that a transcript cannot fit, and long enough for a
// title, an action name and a pointer.
const maxPublishedString = 240

// maxPublishedWords is the longest run of words a published string may
// carry.
//
// It is set where an operator's one-line objective still fits and a
// pasted paragraph does not.
const maxPublishedWords = 48

// The secret shapes.
//
// They are the shapes of the credentials this codebase actually handles:
// provider keys, GitHub tokens, cloud access keys, chat tokens and
// bearer headers. The list is a floor, not a ceiling — a record that
// carries a credential is wrong whatever the credential looks like.
var secretPatterns = []*regexp.Regexp{
	regexp.MustCompile(`sk-[A-Za-z0-9_-]{16,}`),
	regexp.MustCompile(`ghp_[A-Za-z0-9]{20,}`),
	regexp.MustCompile(`gho_[A-Za-z0-9]{20,}`),
	regexp.MustCompile(`ghu_[A-Za-z0-9]{20,}`),
	regexp.MustCompile(`ghs_[A-Za-z0-9]{20,}`),
	regexp.MustCompile(`github_pat_[A-Za-z0-9_]{20,}`),
	regexp.MustCompile(`AKIA[0-9A-Z]{16}`),
	regexp.MustCompile(`xox[baprs]-[A-Za-z0-9-]{10,}`),
	regexp.MustCompile(`(?i)bearer\s+[A-Za-z0-9._~+/-]{16,}=*`),
	regexp.MustCompile(`(?i)(api[_-]?key|secret|password|passwd|token)\s*[:=]\s*\S{8,}`),
	regexp.MustCompile(`(?i)\bBEGIN [A-Z ]*PRIVATE KEY\b`),
}

// The private path shapes.
//
// The operator's home, the data root, and the platform-specific homes are
// all private: they name where the evidence lives, and they are not
// useful to a reader.
var privatePathPatterns = []*regexp.Regexp{
	regexp.MustCompile(`/home/[A-Za-z0-9._-]+/`),
	regexp.MustCompile(`/Users/[A-Za-z0-9._-]+/`),
	regexp.MustCompile(`/root/`),
	regexp.MustCompile(`(?i)\bC:\\Users\\[A-Za-z0-9._-]+\\`),
	regexp.MustCompile(`(?i)(^|\s)~/(\.mpm|\.config|\.ssh|\.aws)`),
}

// Redactor applies the redaction rules to the values a record carries.
//
// It is a FUNCTION and not a set of helpers at the call sites, because the
// rules have to be applied to every string a record carries, and a rule
// applied "somewhere" is a rule that will be forgotten somewhere.
type Redactor struct {
	// Withheld names the fields whose content was withheld.
	//
	// It names the FIELDS. A redaction list that carried the content it
	// withheld would defeat the redaction.
	Withheld []string
}

// Redact returns the value to publish for one field, and whether the
// value was withheld.
//
// The rules, in order:
//
//   - an empty value stays empty (an absent optional field is not a
//     secret);
//   - a value that looks like a credential is withheld;
//   - a value that names a private path is withheld;
//   - a value that is not a single line, or is longer than the cap, is
//     withheld — that is the shape of a transcript.
func (r *Redactor) Redact(field, value string) (string, bool) {
	if value == "" {
		return value, false
	}
	if secretLike(value) {
		r.note(field)
		return Unknown, true
	}
	if privatePathLike(value) {
		r.note(field)
		return Unknown, true
	}
	if transcriptLike(value) {
		r.note(field)
		return Unknown, true
	}
	return value, false
}

func (r *Redactor) note(field string) {
	for _, f := range r.Withheld {
		if f == field {
			return
		}
	}
	r.Withheld = append(r.Withheld, field)
}

// secretLike reports whether a value looks like a credential.
func secretLike(value string) bool {
	for _, p := range secretPatterns {
		if p.MatchString(value) {
			return true
		}
	}
	return false
}

// privatePathLike reports whether a value names a private path.
func privatePathLike(value string) bool {
	for _, p := range privatePathPatterns {
		if p.MatchString(value) {
			return true
		}
	}
	return false
}

// transcriptLike reports whether a value has the shape of a transcript
// rather than of a title.
func transcriptLike(value string) bool {
	if len(value) > maxPublishedString {
		return true
	}
	if strings.ContainsAny(value, "\n\r") {
		return true
	}
	// A long run of prose is a pasted paragraph, not a label.
	//
	// The threshold is a JUDGEMENT, and it is set where an operator's
	// one-line objective still fits and a pasted transcript does not.
	// The structural guarantee is the one that matters — the record has
	// no field that carries model output — and this is the second line
	// of defence for the fields that do carry free text.
	words := strings.Fields(value)
	if len(words) > maxPublishedWords {
		return true
	}
	return false
}

// AssertPublishable refuses a value that must not be published at all.
//
// It is the last gate before the bytes leave: the redaction rules decide
// what is withheld, and this decides whether the RESULT is publishable.
// A record that still carries something secret-like is refused rather
// than published, because a redactor that can be defeated is not a
// redactor.
func AssertPublishable(r *Record) error {
	if r == nil {
		return fmt.Errorf("evidence: there is no record to check")
	}
	checks := map[string]string{
		FieldTitle:             r.Title,
		FieldObjective:         r.Objective,
		FieldIssueRef:          r.IssueRef,
		FieldPRRef:             r.PRRef,
		FieldPendingOperator:   r.PendingOperatorAction,
		FieldRecipient:         r.Recipient,
		FieldRole:              r.Role,
		FieldState:             r.State,
		FieldDisposition:       r.Disposition,
		FieldReviewDisposition: r.ReviewDisposition,
		FieldReviewVerdict:     r.ReviewVerdict,
	}
	if r.ReviewEvidence != nil {
		checks[FieldReviewEvidence+".ref"] = r.ReviewEvidence.Ref
	}
	if r.OperatorApproval != nil {
		checks[FieldOperatorApproval+".source"] = r.OperatorApproval.Source
		checks[FieldOperatorApproval+".evidence"] = r.OperatorApproval.Evidence
		checks[FieldOperatorApproval+".action"] = r.OperatorApproval.Action
	}
	for field, value := range checks {
		if value == "" || value == Unknown {
			continue
		}
		if secretLike(value) {
			return fmt.Errorf("evidence: %s carries something that looks like a credential", field)
		}
		if privatePathLike(value) {
			return fmt.Errorf("evidence: %s carries a private filesystem path", field)
		}
		if transcriptLike(value) {
			return fmt.Errorf("evidence: %s carries what looks like a transcript", field)
		}
	}
	return nil
}
