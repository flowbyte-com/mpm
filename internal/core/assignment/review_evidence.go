package assignment

import (
	"fmt"
	"regexp"
	"strings"

	internal "github.com/flowbyte-com/mpm-core"
)

// The review-evidence capture.
//
// # WHAT THIS IS, AND WHAT IT IS NOT
//
// PR 1's `agent_tasks` carries two columns nothing in PR 1 or PR 2 ever
// writes after creation: `reviewed_sha` and `findings_sha256`. They are the
// right place for "the revision the reviewer examined" and "the digest of
// what the reviewer produced", but they need a WRITE PATH that is not also
// a verdict.
//
// This is that path. It records:
//
//   - the EXACT revision the reviewer examined;
//   - the digest of the findings that were CAPTURED by the host; and
//   - the evidence pointer they came from.
//
// It does NOT record a verdict. A verdict is `RecordReviewResult`, which is
// the reviewer's own fenced transition. Keeping the two apart is the whole
// point: an implementer that can write `reviewed_sha` still cannot write
// `review_passed`, and a reviewer whose findings were captured by the host
// cannot substitute its own digest for them.
//
// WHY IT TAKES NO `OperatorAuthority`
//
// The operator's two verbs are the root's creation and the approval, and
// both change what the OPERATOR authorises. Capturing a digest writes
// nothing about authorisation — it records what a runtime produced, as
// observed by the host. It is fenced on the reviewer's own claim instead,
// so only the holder of the review can attach evidence to it, and only
// while the review is live.
//
// # WHY IT IS NOT IN THE TOOL REGISTRY
//
// Same rule as `create-root` and `record-approval`: an agent-facing action
// named "capture evidence" would let an implementer manufacture the record
// its own work is judged by. It is reachable only through the CLI verb
// `mpm assignment capture-review-evidence`, which the adapter drives.
var (
	// A git object id: 40 or 64 lowercase hex.
	shaPattern = regexp.MustCompile(`^[0-9a-f]{40}$|^[0-9a-f]{64}$`)
	// A SHA-256 digest.
	sha256Pattern = regexp.MustCompile(`^[0-9a-f]{64}$`)
)

// ReviewEvidenceParams names the review and the evidence.
type ReviewEvidenceParams struct {
	// ReviewerID is the REVIEWER assignment.
	ReviewerID string
	// Epoch is the reviewer's claim epoch. It fences the write.
	Epoch int64
	// ReviewedSHA is the exact revision examined.
	ReviewedSHA string
	// FindingsSHA256 is the digest of the findings the HOST captured.
	FindingsSHA256 string
	// EvidenceRef is a pointer to where the captured findings live. It is
	// a pointer, never the findings themselves.
	EvidenceRef string
	// Actor is attribution, not identity.
	Actor Actor
}

// CaptureReviewEvidence records the reviewed revision and the findings
// digest on a reviewer's assignment.
//
// THE WRITE IS FENCED and it is only legal while the review is live: the
// reviewer must be a reviewer, must be `in_progress`, and the presented
// epoch must be its current claim. A reviewer that already recorded a
// verdict can no longer attach evidence, so the verdict it published
// cannot be back-filled with a different revision afterwards.
func (s *Store) CaptureReviewEvidence(p ReviewEvidenceParams) (*Assignment, error) {
	if !shaPattern.MatchString(p.ReviewedSHA) {
		return nil, fmt.Errorf("%w: the reviewed revision must be a full object id, got %q",
			ErrConstraint, p.ReviewedSHA)
	}
	if !sha256Pattern.MatchString(p.FindingsSHA256) {
		return nil, fmt.Errorf("%w: the findings digest must be a SHA-256, got %q",
			ErrConstraint, p.FindingsSHA256)
	}
	if strings.TrimSpace(p.EvidenceRef) == "" {
		return nil, fmt.Errorf("%w: captured evidence must name where it is", ErrConstraint)
	}
	if strings.Contains(p.EvidenceRef, "\n") {
		return nil, fmt.Errorf("%w: the evidence pointer must be one line", ErrConstraint)
	}

	var out *Assignment
	err := s.db.WithTx(func(node internal.DBNode) error {
		rev, err := getAssignmentTx(node, p.ReviewerID)
		if err != nil {
			return err
		}
		if rev.Role != RoleReviewer {
			return fmt.Errorf("%w: %s is a %s and has no review to evidence",
				ErrConstraint, rev.ID, rev.Role)
		}
		if p.Epoch != rev.ClaimEpoch {
			return fmt.Errorf("%w: %s is at epoch %d", ErrStaleClaim, rev.ID, rev.ClaimEpoch)
		}
		if rev.State != StateInProgress {
			return fmt.Errorf("%w: %s is %s; evidence is captured while the review is live",
				ErrIllegalTransition, rev.ID, rev.State)
		}

		now := s.Now()
		res, err := node.ExecTracked(`
			UPDATE agent_tasks
			   SET reviewed_sha = ?, findings_sha256 = ?, evidence_ref = ?, updated_at = ?
			 WHERE id = ? AND claim_epoch = ? AND role = 'reviewer' AND state = ?
			   AND reviewed_sha IS NULL AND findings_sha256 IS NULL`,
			5, p.ReviewedSHA, p.FindingsSHA256, p.EvidenceRef, now,
			p.ReviewerID, p.Epoch, StateInProgress)
		if err != nil {
			return storageRefusal(err)
		}
		affected, err := res.RowsAffected()
		if err != nil {
			return fmt.Errorf("assignment: read the affected rows: %w", err)
		}
		if affected != 1 {
			// THREE DIFFERENT CAUSES, and the caller needs to know which.
			// A review that already carries evidence is not stale — it is
			// already recorded, and a second capture would REPLACE the
			// record of what was examined with a claim about something
			// else.
			if rev.ReviewedSHA != "" || rev.FindingsSHA256 != "" {
				return fmt.Errorf("%w: %s already carries captured evidence (%s); "+
					"the record of what was examined is write-once",
					ErrConstraint, rev.ID, rev.ReviewedSHA)
			}
			return fmt.Errorf("%w: %s moved while the evidence was being captured",
				ErrStaleClaim, p.ReviewerID)
		}

		// WRITE-PATH READ-BACK, and one more rule the WHERE clause
		// cannot state: the ROW must now carry exactly what was
		// presented.
		got, err := getAssignmentTx(node, p.ReviewerID)
		if err != nil {
			return err
		}
		if got == nil {
			return fmt.Errorf("%w: %s disappeared while its evidence was captured",
				ErrNotFound, p.ReviewerID)
		}
		if got.ReviewedSHA != p.ReviewedSHA || got.FindingsSHA256 != p.FindingsSHA256 {
			return fmt.Errorf("assignment: the captured evidence did not persist on %s", p.ReviewerID)
		}
		out = got
		return nil
	})
	if err != nil {
		return nil, err
	}
	return out, nil
}
