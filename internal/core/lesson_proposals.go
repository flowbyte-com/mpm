package internal

// lesson_proposals.go — the PENDING surface for a lesson an agent has
// proposed and an operator has not yet accepted.
//
// THE TRUST CONTRACT
// ------------------
// A lesson row in `lessons_base` is TRUSTED ON ARRIVAL: the table has
// no status column, and the row is immediately inside the trusted
// retrieval set (lessons_fts, `mpm lesson search`, `mpm lesson list`,
// the wake context's recent_lessons). An agent that can write there can
// therefore put unverified speculation into the substrate's knowledge
// without a review step.
//
// This file is the other half of the fix: it gives the substrate a
// place to hold a proposal that is NOT trusted yet.
//
// WHY A SEPARATE TABLE
// --------------------
// The obvious alternative — a `status` column on `lessons_base` —
// would mean every trusted read path has to filter on it. `lessons_fts`
// would still index the pending content, the wake context would still
// have to exclude it, `mpm lesson search` would still have to join to
// it, and every future read path would have to remember. A read path
// that forgets is a pending lesson silently promoted to trusted
// knowledge — exactly the failure this file exists to make impossible.
//
// A separate table makes the isolation STRUCTURAL. No trusted read path
// queries `lesson_proposals`, so there is no filter to forget, no index
// to keep in sync, and no FTS entry to exclude. A proposal is invisible
// to the substrate's retrieval machinery — full-text search, vectors,
// synthesis, the wake context — until an operator promotes it.
//
// THE STATE MACHINE
// -----------------
//
//	propose ──▶ pending ──▶ promoted   (an operator accepted it; a
//	                            │       lesson now exists in
//	                            │       lessons_base)
//	                            └──▶ rejected  (an operator declined it)
//
// The transitions are made by the OPERATOR path (`mpm lesson promote`,
// `mpm lesson reject`). An agent can only create a `pending` row: the
// agent-reachable surface is `mpm_lessons action=propose`, and it has
// no transition action.
//
// The `status` CHECK constraint in the schema is the backstop: a typo
// in a status value fails at the storage layer rather than inventing a
// fourth state.
//
// PROVENANCE
// ----------
// A proposal records who proposed it: the session, the framework and
// the model, taken from the caller's ActiveContext. A promotion records
// when it was reviewed and which lesson it became. The chain
// "proposal → operator decision → lesson" is therefore complete and
// auditable, and the promoted lesson carries the same provenance
// treatment as any other lesson (it is written through AddLesson, so it
// gets the sensitive-content scan, the poison-phrase scan, the
// content-hash dedup and the artifact-provenance row).
//
// A promoted lesson's `source_session_id` is the SESSION THAT PROPOSED
// IT, not the session that promoted it. The content came from the
// agent; saying otherwise would launder the proposal's origin. The
// promotion itself is recorded on the proposal row, which is where the
// operator's decision belongs.

import (
	"database/sql"
	"fmt"
	"strings"
	"time"
)

// Lesson proposal lifecycle states. These are the ONLY values the
// `status` CHECK constraint accepts.
const (
	LessonProposalPending  = "pending"
	LessonProposalPromoted = "promoted"
	LessonProposalRejected = "rejected"
)

// LessonProposal is a lesson an agent has proposed and an operator has
// not yet accepted.
type LessonProposal struct {
	ID      string `json:"id"`
	Type    string `json:"type"`
	Content string `json:"content"`
	Tags    []string `json:"tags"`
	Created string `json:"created"`
	Status  string `json:"status"`
	// SourceSessionID is the session that proposed the lesson.
	SourceSessionID string `json:"source_session_id"`
	// FrameworkName identifies the calling framework ("opencode", "mcp",
	// "mpm-cli", …). Empty when the caller supplied no identity.
	FrameworkName string `json:"framework_name"`
	// ModelName identifies the model that was running when the proposal
	// was made. Empty when unknown.
	ModelName string `json:"model_name"`
	// SourceIDs are the cognitive nodes the proposer distilled the
	// lesson from. They are credited when the proposal is PROMOTED —
	// that is the moment the lesson becomes durable knowledge, which is
	// what the credit means.
	SourceIDs []string `json:"source_ids"`
	// PromotedLessonID is the lesson this proposal became. Empty until
	// an operator promotes it.
	PromotedLessonID string `json:"promoted_lesson_id"`
	// ReviewedAt is when an operator acted on the proposal (unix epoch).
	// Zero when the proposal is still pending.
	ReviewedAt sql.NullInt64 `json:"reviewed_at"`
}

// validLessonProposalStatus reports whether a status is a member of the
// proposal lifecycle.
func validLessonProposalStatus(status string) bool {
	switch status {
	case LessonProposalPending, LessonProposalPromoted, LessonProposalRejected:
		return true
	default:
		return false
	}
}

// AddLessonProposal records a lesson proposal in the PENDING state.
//
// The write is read-back asserted (Substrate Defense Triad, rule 3):
// an INSERT that returns nil does not prove persistence, so the row is
// re-read before the caller is told it exists.
//
// The content is scanned for sensitive content and for poison phrases
// exactly as a lesson write is — a proposal is a candidate for the
// lesson table, and a payload that would be blocked at promotion time
// has no business being stored now either. Blocking it here also means
// the operator never has to look at a proposal that could not have been
// promoted.
func (dm *DatabaseManager) AddLessonProposal(content string, lessonType string, tags []string, sourceIDs []string, ac ...ActiveContext) (*LessonProposal, error) {
	if strings.TrimSpace(content) == "" {
		return nil, fmt.Errorf("fact is required")
	}
	if lessonType == "" {
		lessonType = "insight"
	}
	if err := ValidateLessonType(lessonType); err != nil {
		return nil, err
	}
	if isSensitive, name := isSensitiveContent(content); isSensitive {
		return nil, fmt.Errorf("lesson proposal content blocked: %s detected", name)
	}
	if poisoned, reason := dm.scanPoisoned(content); poisoned {
		return nil, fmt.Errorf("lesson proposal content blocked: poison phrase detected: %s", reason)
	}

	ctx := firstActiveContext(ac)
	id := GenerateID()
	tagsJSON, _ := MarshalJSON(tags)
	sourceIDsJSON, _ := MarshalJSON(sourceIDs)
	now := time.Now().UTC().Format(time.RFC3339)

	_, err := dm.db.Exec(`
		INSERT INTO lesson_proposals
			(id, type, content, tags, created, status, source_session_id, framework_name, model_name, source_ids)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
	`, id, lessonType, content, tagsJSON, now, LessonProposalPending,
		sourceSessionIDOrNil(ctx.SessionID), ctx.FrameworkName, ctx.Model, sourceIDsJSON)
	if err != nil {
		return nil, fmt.Errorf("insert lesson proposal: %w", err)
	}

	// Read-back assertion. A proposal that was not persisted must not
	// be reported as pending review.
	proposal, err := dm.GetLessonProposal(id)
	if err != nil {
		return nil, fmt.Errorf("lesson proposal insert read-back failed (commit did not persist): %w", err)
	}
	return proposal, nil
}

// GetLessonProposal reads one proposal by id.
func (dm *DatabaseManager) GetLessonProposal(id string) (*LessonProposal, error) {
	row := dm.db.QueryRow(`
		SELECT id, type, content, COALESCE(tags,'[]'), created, status,
		       COALESCE(source_session_id, ''), COALESCE(framework_name, ''), COALESCE(model_name, ''),
		       COALESCE(source_ids,'[]'), COALESCE(promoted_lesson_id, ''), reviewed_at
		FROM lesson_proposals WHERE id = ?`, id)

	var p LessonProposal
	var tagsJSON string
	var sourceIDsJSON string
	var reviewedAt sql.NullInt64
	err := row.Scan(&p.ID, &p.Type, &p.Content, &tagsJSON, &p.Created, &p.Status,
		&p.SourceSessionID, &p.FrameworkName, &p.ModelName, &sourceIDsJSON,
		&p.PromotedLessonID, &reviewedAt)
	if err != nil {
		if err == sql.ErrNoRows {
			return nil, fmt.Errorf("lesson proposal %q not found", id)
		}
		return nil, fmt.Errorf("read lesson proposal: %w", err)
	}
	p.ReviewedAt = reviewedAt
	if tagsJSON != "" {
		UnmarshalJSON(tagsJSON, &p.Tags)
	}
	if sourceIDsJSON != "" {
		UnmarshalJSON(sourceIDsJSON, &p.SourceIDs)
	}
	return &p, nil
}

// ListLessonProposals returns proposals in the given status.
//
// An empty status means "every status", which is what the operator's
// review view wants; "pending" is what a triage listing wants.
//
// The list is newest-first and bounded. A proposal is an inbox, not a
// log: an operator reviews the recent ones, and the default limit keeps
// a large backlog from flooding the terminal.
func (dm *DatabaseManager) ListLessonProposals(status string, limit int) ([]*LessonProposal, error) {
	if status != "" && !validLessonProposalStatus(status) {
		return nil, fmt.Errorf("invalid proposal status %q: must be one of pending, promoted, rejected", status)
	}
	if limit <= 0 {
		limit = 50
	}

	query := `
		SELECT id, type, content, COALESCE(tags,'[]'), created, status,
		       COALESCE(source_session_id, ''), COALESCE(framework_name, ''), COALESCE(model_name, ''),
		       COALESCE(source_ids,'[]'), COALESCE(promoted_lesson_id, ''), reviewed_at
		FROM lesson_proposals`
	args := []interface{}{}
	if status != "" {
		query += ` WHERE status = ?`
		args = append(args, status)
	}
	query += ` ORDER BY created DESC LIMIT ?`
	args = append(args, limit)

	rows, err := dm.db.Query(query, args...)
	if err != nil {
		return nil, fmt.Errorf("list lesson proposals: %w", err)
	}
	defer rows.Close()

	out := make([]*LessonProposal, 0, limit)
	for rows.Next() {
		var p LessonProposal
		var tagsJSON string
		var sourceIDsJSON string
		var reviewedAt sql.NullInt64
		if err := rows.Scan(&p.ID, &p.Type, &p.Content, &tagsJSON, &p.Created, &p.Status,
			&p.SourceSessionID, &p.FrameworkName, &p.ModelName, &sourceIDsJSON,
			&p.PromotedLessonID, &reviewedAt); err != nil {
			return nil, fmt.Errorf("scan lesson proposal: %w", err)
		}
		p.ReviewedAt = reviewedAt
		if tagsJSON != "" {
			UnmarshalJSON(tagsJSON, &p.Tags)
		}
		if sourceIDsJSON != "" {
			UnmarshalJSON(sourceIDsJSON, &p.SourceIDs)
		}
		out = append(out, &p)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("list lesson proposals: %w", err)
	}
	return out, nil
}

// PromoteLessonProposal turns a pending proposal into a TRUSTED lesson.
//
// This is the operator path, and it is the ONLY path from a proposal to
// a lesson. It is not reachable from the agent tool surface (MCP,
// `mpm call`, adapters) — the lesson tool's `propose` action creates a
// proposal and has no transition.
//
// The lesson is written through AddLesson, so it gets exactly the same
// treatment as a lesson the operator typed by hand: the sensitive-
// content scan, the poison-phrase scan, the content-hash dedup and the
// artifact-provenance row.
//
// TWO STATEMENTS, NOT ONE TRANSACTION, AND WHY THAT IS SAFE
// ---------------------------------------------------------
// The lesson write and the proposal's state change are two statements.
// AddLesson is not transaction-aware, so forcing them into one
// transaction would mean either refactoring AddLesson (out of scope) or
// nesting a second connection inside an open transaction — the
// single-connection deadlock trap the substrate explicitly forbids.
//
// The ordering is chosen so that neither failure mode loses content:
//
//   - the LESSON is written first. If the state update then fails, the
//     lesson exists and the proposal is still pending. The operator is
//     told the promotion failed, and a retry is SAFE: AddLesson's
//     content-hash dedup finds the identical lesson and REINFORCES it
//     instead of creating a second one. So a failed promotion leaves a
//     lesson and a pending proposal — recoverable, and the operator
//     sees the failure;
//   - the reverse order (mark promoted, then write the lesson) would
//     leave a proposal marked promoted with no lesson, which is
//     content loss.
//
// The state update is conditional on the proposal still being pending
// (`WHERE id = ? AND status = 'pending'`), so two concurrent
// promotions cannot both mark it promoted; the loser is told the
// proposal was reviewed concurrently.
func (dm *DatabaseManager) PromoteLessonProposal(id string, ac ...ActiveContext) (*Lesson, *LessonProposal, int, error) {
	proposal, err := dm.GetLessonProposal(id)
	if err != nil {
		return nil, nil, 0, err
	}
	if proposal.Status != LessonProposalPending {
		return nil, nil, 0, fmt.Errorf(
			"lesson proposal %q is %s, not pending — only a pending proposal can be promoted", id, proposal.Status)
	}

	ctx := firstActiveContext(ac)

	lesson, err := dm.AddLesson(proposal.Content, LessonType(proposal.Type), proposal.Tags,
		proposal.SourceSessionID, ctx)
	if err != nil {
		return nil, nil, 0, fmt.Errorf("promote lesson proposal: %w", err)
	}

	res, err := dm.db.Exec(`
		UPDATE lesson_proposals
		SET status = ?, promoted_lesson_id = ?, reviewed_at = ?
		WHERE id = ? AND status = ?`,
		LessonProposalPromoted, lesson.ID, time.Now().UTC().Unix(), id, LessonProposalPending)
	if err != nil {
		return nil, nil, 0, fmt.Errorf("mark lesson proposal promoted: %w", err)
	}
	affected, err := res.RowsAffected()
	if err != nil {
		return nil, nil, 0, fmt.Errorf("mark lesson proposal promoted (rows affected): %w", err)
	}
	if affected != 1 {
		return nil, nil, 0, fmt.Errorf(
			"lesson proposal %q was reviewed concurrently — it was not marked promoted", id)
	}

	updated, err := dm.GetLessonProposal(id)
	if err != nil {
		return nil, nil, 0, err
	}

	// The Provenance Proxy: each cognitive node the proposal cited is
	// credited with a success_count increment.
	//
	// The credit lands HERE, at the promotion, and not at the proposal.
	// `success_count` means "this node helped produce durable
	// knowledge", and a pending proposal is not durable knowledge — so
	// a proposal that is never promoted must not inflate the retrieval
	// signal of the nodes it drew on.
	//
	// The node type is resolved from the artifact id against the
	// database (not from a prefix heuristic), so a cited id that names
	// a real artifact is credited as that artifact's type.
	//
	// The credit is best-effort: a telemetry failure must not undo a
	// promotion that has already committed.
	credited := 0
	for _, sourceID := range proposal.SourceIDs {
		if sourceID == "" {
			continue
		}
		nodeType := "memory"
		if resolved, rerr := dm.ResolveArtifactType(sourceID); rerr == nil && resolved != "" {
			nodeType = resolved
		}
		if cerr := dm.IncrementSuccess(sourceID, nodeType); cerr == nil {
			credited++
		}
	}

	return lesson, updated, credited, nil
}

// RejectLessonProposal declines a pending proposal.
//
// The proposal's content is retained, not deleted: a declined proposal
// is evidence of what the agent thought, and a later operator may want
// to see why it was declined. It is simply no longer pending, so it
// leaves the review queue.
func (dm *DatabaseManager) RejectLessonProposal(id string) (*LessonProposal, error) {
	proposal, err := dm.GetLessonProposal(id)
	if err != nil {
		return nil, err
	}
	if proposal.Status != LessonProposalPending {
		return nil, fmt.Errorf(
			"lesson proposal %q is %s, not pending — only a pending proposal can be rejected", id, proposal.Status)
	}

	res, err := dm.db.Exec(`
		UPDATE lesson_proposals
		SET status = ?, reviewed_at = ?
		WHERE id = ? AND status = ?`,
		LessonProposalRejected, time.Now().UTC().Unix(), id, LessonProposalPending)
	if err != nil {
		return nil, fmt.Errorf("mark lesson proposal rejected: %w", err)
	}
	affected, err := res.RowsAffected()
	if err != nil {
		return nil, fmt.Errorf("mark lesson proposal rejected (rows affected): %w", err)
	}
	if affected != 1 {
		return nil, fmt.Errorf(
			"lesson proposal %q was reviewed concurrently — it was not marked rejected", id)
	}

	return dm.GetLessonProposal(id)
}
