// review.go — the spaced-repetition REVIEW contract.
//
// WHAT THIS FILE IS
// -----------------
// One rule, one query, one place. The operator-facing surfaces (the
// doctor's "Review backlog" check, the dashboard's readiness row, and the
// `mpm ops review` command) all used to answer the same question three
// different ways:
//
//   - doctor      GetSpacedReinforcementReview(14, 100)
//   - readiness   GetSpacedReinforcementReview(14, 100)
//   - ops review  GetSpacedReinforcementReview(*days /* default 30 */, 20)
//
// So the doctor's warning named a threshold the review command did not
// use, the review command silently ignored its own --limit, and both
// counted a *truncated* sample as if it were the total. The operator who
// followed the doctor's instruction was told to look at 30-day-old
// memories while the doctor had been counting 14-day-old ones.
//
// The rule now lives here, and every surface calls it.
//
// THE RULE
// --------
// A memory is DUE FOR REVIEW when all of these hold:
//
//  1. it is not soft-deleted, and it is not a directive
//     (DirectiveExclusionClause — the canonical predicate);
//  2. it is durable enough to be worth reviewing: is_long_term = 1 or
//     weight >= 5;
//  3. its REVIEW CLOCK is older than the window.
//
// The REVIEW CLOCK is:
//
//     COALESCE(reviewed_at, last_accessed_at, created_at)
//
// read from three places, in order of authority:
//
//   reviewed_at       an explicit operator review (memory_reviews). The
//                     strongest signal: a human looked at this memory.
//   last_accessed_at  the last time it was retrieved. A proxy for "it
//                     has been in use" — good enough when no human has
//                     reviewed it, but NOT a review: an agent can
//                     retrieve a memory a thousand times, and that must
//                     not silently clear an operator's backlog.
//   created_at        the fallback for a memory that was never accessed
//                     and never reviewed.
//
// The created_at fallback is what stops a brand-new memory from being
// instantly due. The previous rule was `last_accessed_at IS NULL OR
// last_accessed_at < cutoff`, so EVERY never-accessed memory was due
// immediately — including one created a second ago. A never-accessed
// memory is now due only once it is itself older than the window.
//
// A REVIEW IS NOT A READ
// ----------------------
// Recording a review is an explicit operator act (RecordMemoryReview).
// It is never a side effect of displaying a memory, of retrieving one,
// or of any background sweep. `mpm show` does not write last_accessed_at
// and does not write a review.
//
// A review also never changes a memory's weight, its reinforcement count
// or its content by itself. The "weaken" decision is an explicit request
// that the caller applies through the existing WeakenMemory path; this
// package only records the DECISION.
package internal

import (
	"database/sql"
	"fmt"
	"time"
)

// ReviewDueAfterDays is THE review window.
//
// One constant, used by the doctor's backlog check, the dashboard's
// readiness row and the review command, so the three can never disagree
// about what "due" means. The previous state — 14 days in the doctor and
// 30 in the CLI — was the defect this constant exists to prevent.
const ReviewDueAfterDays = 14

// ReviewDecisionKeep and ReviewDecisionWeaken are the operator's
// decisions about a reviewed memory.
//
// "keep" is the honest default: the memory was reviewed and stands.
// "weaken" is an explicit instruction to reduce the memory's weight; the
// caller applies it through WeakenMemory, which is the existing,
// separately-audited weakening path. This package records the decision;
// it does not apply it.
//
// There is deliberately no "delete" decision here. Deletion already has
// its own command, its own confirmation semantics and its own
// irreversibility. Putting it inside a review flag would create a second,
// quieter path to a destructive act — the kind of duplication this
// repository treats as a defect.
const (
	ReviewDecisionKeep   = "keep"
	ReviewDecisionWeaken = "weaken"
)

// ReviewCandidate is one memory whose spaced review is due.
//
// It carries the review-relevant facts explicitly rather than as a bag of
// optional maps, because the surface that renders it has to be HONEST:
// a nil LastAccessedAt means "never accessed", and the renderer must say
// so rather than invent an age. The previous implementation passed a
// *int64 through a formatter that did not understand that shape, and
// every never-accessed memory rendered as "106751d ago" — 292 years.
type ReviewCandidate struct {
	ID                 string `json:"id"`
	Collection         string `json:"collection"`
	Content            string `json:"content"`
	Weight             int    `json:"weight"`
	ReinforcementCount int    `json:"reinforcement_count"`
	CreatedAt          int64  `json:"created_at"`
	// LastAccessedAt is nil when the memory has NEVER been accessed.
	LastAccessedAt *int64 `json:"last_accessed_at"`
	// ReviewedAt is nil when the memory has NEVER been explicitly
	// reviewed by an operator.
	ReviewedAt *int64 `json:"reviewed_at"`
	// ReviewClock is the timestamp the eligibility rule measured:
	// COALESCE(reviewed_at, last_accessed_at, created_at). It is never
	// zero for a real row.
	ReviewClock int64 `json:"review_clock"`
	// DaysOverdue is how many days ago the review clock was, at the
	// moment the candidate was read.
	DaysOverdue int `json:"days_overdue"`
}

// reviewCandidateQuery is THE eligibility query.
//
// It is a constant rather than an inline string so that the count query
// and the candidate query cannot drift apart: CountMemoriesDueForReview
// counts over the same WHERE clause, in the same statement text.
//
// The JOIN to memory_reviews is a LEFT JOIN, so a memory that has never
// been reviewed is still a candidate — with reviewed_at NULL, and the
// review clock falling through to last_accessed_at and then created_at.
const reviewCandidateQuery = `
	SELECT memories.id,
	       memories.collection,
	       memories.content,
	       COALESCE(memories.reinforcement_count, 0),
	       COALESCE(memories.weight, 1),
	       memories.created_at,
	       memories.last_accessed_at,
	       memory_reviews.reviewed_at,
	       COALESCE(memory_reviews.reviewed_at, memories.last_accessed_at, memories.created_at)
	FROM memories
	LEFT JOIN memory_reviews ON memory_reviews.memory_id = memories.id
	WHERE memories.deleted_at IS NULL
	  AND ` + DirectiveExclusionClause + `
	  AND (memories.is_long_term = 1 OR COALESCE(memories.weight, 1) >= 5)
	  AND COALESCE(memory_reviews.reviewed_at, memories.last_accessed_at, memories.created_at)
	      < CAST(strftime('%s','now','-' || ? || ' days') AS INTEGER)
	ORDER BY 9 ASC, memories.reinforcement_count ASC
`

// MemoriesDueForReview returns the memories whose review is due,
// oldest review clock first.
//
// limit <= 0 means "no limit". Callers that need the TOTAL rather than
// a sample must use CountMemoriesDueForReview: a truncated list is a
// SAMPLE, and reporting a sample's length as the total is how the
// doctor's "60 memories due" could have been 100.
func (s *MemoryStore) MemoriesDueForReview(days int, limit int) ([]ReviewCandidate, error) {
	if s.DB == nil {
		if err := s.InitSQLite(); err != nil {
			return nil, err
		}
	}
	if days <= 0 {
		days = ReviewDueAfterDays
	}

	query := reviewCandidateQuery
	args := []interface{}{days}
	if limit > 0 {
		query += " LIMIT ?"
		args = append(args, limit)
	}

	rows, err := s.DB.Query(query, args...)
	if err != nil {
		return nil, fmt.Errorf("query memories due for review: %w", err)
	}
	defer rows.Close()

	now := time.Now()
	out := make([]ReviewCandidate, 0, 16)
	for rows.Next() {
		var c ReviewCandidate
		var collection sql.NullString
		var content sql.NullString
		var reinforcement sql.NullInt64
		var weight sql.NullFloat64
		var createdAt sql.NullInt64
		var lastAccessed sql.NullInt64
		var reviewed sql.NullInt64
		var clock sql.NullInt64

		if err := rows.Scan(&c.ID, &collection, &content, &reinforcement, &weight,
			&createdAt, &lastAccessed, &reviewed, &clock); err != nil {
			return nil, fmt.Errorf("scanning review candidate: %w", err)
		}
		c.Collection = collection.String
		c.Content = content.String
		c.ReinforcementCount = int(reinforcement.Int64)
		c.Weight = int(weight.Float64)
		c.CreatedAt = createdAt.Int64
		if lastAccessed.Valid {
			v := lastAccessed.Int64
			c.LastAccessedAt = &v
		}
		if reviewed.Valid {
			v := reviewed.Int64
			c.ReviewedAt = &v
		}
		c.ReviewClock = clock.Int64
		if c.ReviewClock > 0 {
			c.DaysOverdue = int(now.Sub(time.Unix(c.ReviewClock, 0)).Hours() / 24)
			if c.DaysOverdue < 0 {
				c.DaysOverdue = 0
			}
		}
		out = append(out, c)
	}
	return out, rows.Err()
}

// CountMemoriesDueForReview returns the TOTAL number of memories whose
// review is due — not a sample.
//
// The doctor's backlog check used to take the first 100 candidates and
// report len(candidates) as the backlog size. With more than 100 due
// that under-reports: the operator sees "60 memories due" when 160 are
// due, and clearing 60 leaves the warning unchanged. A count and a
// sample are different facts and the surfaces that need the count must
// ask for the count.
func (s *MemoryStore) CountMemoriesDueForReview(days int) (int, error) {
	if s.DB == nil {
		if err := s.InitSQLite(); err != nil {
			return 0, err
		}
	}
	if days <= 0 {
		days = ReviewDueAfterDays
	}

	var n int
	err := s.DB.QueryRow(`
		SELECT COUNT(*)
		FROM memories
		LEFT JOIN memory_reviews ON memory_reviews.memory_id = memories.id
		WHERE memories.deleted_at IS NULL
		  AND `+DirectiveExclusionClause+`
		  AND (memories.is_long_term = 1 OR COALESCE(memories.weight, 1) >= 5)
		  AND COALESCE(memory_reviews.reviewed_at, memories.last_accessed_at, memories.created_at)
		      < CAST(strftime('%s','now','-' || ? || ' days') AS INTEGER)
	`, days).Scan(&n)
	if err != nil {
		return 0, fmt.Errorf("count memories due for review: %w", err)
	}
	return n, nil
}

// RecordMemoryReview records the operator's review of a memory.
//
// This is the ONLY way a review is created. It is never called by a read
// path, a display path or a background sweep.
//
// The decision is recorded; it is not applied here. A "weaken" decision
// asks the caller to route through WeakenMemory — the existing,
// separately-audited path — so that the weight change and the review
// record cannot be produced by different code with different rules.
//
// The write is an UPSERT: the table holds the LATEST review per memory.
func (s *MemoryStore) RecordMemoryReview(memoryID string, decision string, note string) error {
	if s.DB == nil {
		if err := s.InitSQLite(); err != nil {
			return err
		}
	}
	if memoryID == "" {
		return fmt.Errorf("record memory review: the memory id is empty")
	}
	switch decision {
	case ReviewDecisionKeep, ReviewDecisionWeaken:
	default:
		return fmt.Errorf("record memory review: unknown decision %q (expected %q or %q)",
			decision, ReviewDecisionKeep, ReviewDecisionWeaken)
	}

	// The memory must exist and must not be soft-deleted. A review of a
	// memory that is not there is a mistake worth reporting, not a
	// silent no-op.
	var alive int
	err := s.DB.QueryRow(
		`SELECT COUNT(*) FROM memories WHERE id = ? AND deleted_at IS NULL`,
		memoryID).Scan(&alive)
	if err != nil {
		return fmt.Errorf("record memory review: %w", err)
	}
	if alive == 0 {
		return fmt.Errorf("record memory review: no memory with id %s (not found, deleted, or expired)", memoryID)
	}

	var noteValue interface{}
	if note != "" {
		noteValue = note
	}

	_, err = s.DB.Exec(`
		INSERT INTO memory_reviews (memory_id, reviewed_at, decision, note)
		VALUES (?, ?, ?, ?)
		ON CONFLICT(memory_id) DO UPDATE SET
			reviewed_at = excluded.reviewed_at,
			decision    = excluded.decision,
			note        = excluded.note
	`, memoryID, time.Now().Unix(), decision, noteValue)
	if err != nil {
		return fmt.Errorf("record memory review: %w", err)
	}
	return nil
}

// MemoryReview returns the recorded review for a memory, or nil when it
// has never been reviewed.
func (s *MemoryStore) MemoryReview(memoryID string) (*MemoryReviewRecord, error) {
	if s.DB == nil {
		if err := s.InitSQLite(); err != nil {
			return nil, err
		}
	}
	var rec MemoryReviewRecord
	var note sql.NullString
	err := s.DB.QueryRow(
		`SELECT memory_id, reviewed_at, decision, note FROM memory_reviews WHERE memory_id = ?`,
		memoryID).Scan(&rec.MemoryID, &rec.ReviewedAt, &rec.Decision, &note)
	if err == sql.ErrNoRows {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("read memory review: %w", err)
	}
	rec.Note = note.String
	return &rec, nil
}

// MemoryReviewRecord is one recorded review.
type MemoryReviewRecord struct {
	MemoryID   string `json:"memory_id"`
	ReviewedAt int64  `json:"reviewed_at"`
	Decision   string `json:"decision"`
	Note       string `json:"note,omitempty"`
}
