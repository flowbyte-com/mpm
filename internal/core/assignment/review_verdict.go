package assignment

import (
	"errors"
	"fmt"
	"strings"
)

// ErrReviewVerdict says the captured assistant-authored text does not carry
// exactly one unambiguous terminal verdict.
//
// It is a RECOVERY-REQUIRED outcome, not a failure to approve: the review may
// have produced evidence, but the host will not invent the reviewer's
// decision.
var ErrReviewVerdict = errors.New("assignment: the captured review has no unambiguous assistant verdict")

// ErrReviewAmbiguous says a reviewer process produced a result, but the host
// could not accept it as THIS review: a different session, agent or model
// ran; a session-level permission override was present; the outcome was not
// success; the capture was missing or named a different revision.
//
// It is RECOVERY-REQUIRED, not a retryable failure. The model may already
// have run and produced billable output, so starting a second reviewer could
// duplicate inference. The attempt is marked AMBIGUOUS and a later dispatch
// refuses until an operator reconciles it.
var ErrReviewAmbiguous = errors.New("assignment: the review attempt needs operator recovery")

// The only accepted terminal verdict lines. They are exact: no markdown, no
// punctuation and no case folding.
const (
	VerdictLineApproved         = "VERDICT: APPROVED"
	VerdictLineChangesRequested = "VERDICT: CHANGES REQUESTED"
)

// ExtractReviewVerdict reads the reviewer's OWN assistant-authored text and
// returns the assignment verdict it declares.
//
// THE RULES ARE STRICT ON PURPOSE:
//
//   - only the final non-empty line may be the declaration;
//   - it must be exactly one of the two canonical lines;
//   - any other occurrence of `VERDICT:` anywhere in the assistant text is
//     contradictory or quoted material, and the review is refused.
//
// The caller must pass assistant-authored text only. User prompts, tool
// output and diagnostics are not authorised sources; the OpenCode adapter
// returns `AssistantFindings` separately for this reason.
func ExtractReviewVerdict(assistant string) (string, error) {
	text := strings.ReplaceAll(assistant, "\r\n", "\n")
	lines := strings.Split(text, "\n")

	var nonEmpty []string
	for _, line := range lines {
		line = strings.TrimSpace(line)
		if line != "" {
			nonEmpty = append(nonEmpty, line)
		}
	}
	if len(nonEmpty) == 0 {
		return "", fmt.Errorf("%w: the assistant wrote no text", ErrReviewVerdict)
	}

	verdictLines := 0
	for _, line := range nonEmpty {
		if strings.Contains(line, "VERDICT:") {
			verdictLines++
		}
	}
	if verdictLines == 0 {
		return "", fmt.Errorf("%w: no terminal VERDICT line", ErrReviewVerdict)
	}
	if verdictLines > 1 {
		return "", fmt.Errorf("%w: more than one VERDICT occurrence", ErrReviewVerdict)
	}

	final := nonEmpty[len(nonEmpty)-1]
	switch final {
	case VerdictLineApproved:
		return VerdictPassed, nil
	case VerdictLineChangesRequested:
		return VerdictChangesRequested, nil
	default:
		return "", fmt.Errorf("%w: the final VERDICT line is not one of %q or %q",
			ErrReviewVerdict, VerdictLineApproved, VerdictLineChangesRequested)
	}
}
