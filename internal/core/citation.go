package internal

// citation.go — typed citations from a proposal to what it rests on.
//
// WHY THIS EXISTS.
//
// mpm-event/0.1 §3.7 (M5) requires that a proposal cite RESOLVABLE evidence
// and that a proposal whose evidence cannot be resolved is REJECTED — not
// silently downgraded to an unevidenced proposal.
//
// The substrate's historical `source_ids` path could not satisfy that
// requirement, for two independent reasons:
//
//  1. The cited id was NEVER validated. A proposal citing an id that
//     resolves to nothing was accepted with "success": true.
//  2. The source type was NEVER carried. `recordSourceCitationsNode` passed
//     an empty source_type, so `recordProvenanceNode` resolved the id
//     against the artifact tables and fell back to "memory"
//     (cascade_provenance.go). A citation of a tool invocation was stored as
//     a citation of a memory, and a citation of nothing at all was stored
//     just the same.
//
// This file adds the typed path. It reuses `epistemic_provenance`, which
// already provides all three required properties:
//
//	source_type TEXT NOT NULL   — the type is carried
//	event_id    TEXT NOT NULL   — the cited observation is identifiable
//	UNIQUE (source_id, downstream_id, event_id) — re-submission is idempotent
//
// and it needs NO schema change.
//
// THE TWO PATHS.
//
// A citation with an EMPTY Type is a LEGACY citation: it is recorded exactly
// as the historical `source_ids` path recorded it — type auto-resolved, not
// validated, event id = the downstream id. Callers that do not supply the
// new optional field keep today's behaviour, byte for byte.
//
// A citation with a NON-EMPTY Type is a TYPED citation: the type must be in
// the vocabulary below, the cited id must resolve AS THAT TYPE, and the row
// records the declared type verbatim with the cited id as the event id.
//
// An invalid typed citation REJECTS the whole proposal. Because the
// proposal and its citations are written in ONE transaction, the rejection
// leaves no partial proposal record — no theory row, no dependency column
// patch, no citation row.

import (
	"database/sql"
	"errors"
	"fmt"
	"sort"
	"strings"
)

// Citation is a typed reference from a proposal to the observation or
// artifact it rests on.
//
// An empty Type marks the LEGACY (untyped, unvalidated) path — see the file
// header. A non-empty Type must be one of the vocabulary constants below.
type Citation struct {
	// ID is the cited identifier. It must resolve under Type.
	ID string
	// Type is the declared source type. Empty means the legacy path.
	Type string
}

// The canonical citation source-type vocabulary.
//
// The five artifact types mirror the `evidence` table's CHECK-constrained
// artifact_type vocabulary (internal/core/schema.go). `tool_invocation` is
// added for a citation that points at the substrate's tool-call audit — the
// citation kind mpm-event/0.1 §3.7 explicitly contemplates ("each of them
// must resolve to a stored event (or to a tool_invocations row)").
const (
	CitationSourceMemory         = "memory"
	CitationSourceTheory         = "theory"
	CitationSourceDecision       = "decision"
	CitationSourceLesson         = "lesson"
	CitationSourceWork           = "work"
	CitationSourceToolInvocation = "tool_invocation"
)

// citationSourceTypes returns the vocabulary in a stable, sorted order, for
// error messages and for the tool registry's enum.
func citationSourceTypes() []string {
	out := make([]string, 0, len(citationSourceQueries))
	for t := range citationSourceQueries {
		out = append(out, t)
	}
	sort.Strings(out)
	return out
}

// CitationSourceTypes returns the canonical citation source-type vocabulary.
func CitationSourceTypes() []string { return citationSourceTypes() }

// ValidCitationSourceType reports whether t is part of the canonical
// citation source-type vocabulary.
func ValidCitationSourceType(t string) bool {
	_, ok := citationSourceQueries[t]
	return ok
}

// citationSourceQueries maps each source type to the query that proves an id
// is a row OF THAT TYPE — not merely a row that exists.
//
// The artifact types are distinguished by their `memories.collection`
// discriminator (the same mapping `collectionToArtifactType` uses), and a
// soft-deleted row does not count: a citation must point at something that
// is still there.
//
// `tool_invocations` has no deleted_at — the audit table is append-only, and
// a shredded row is removed rather than tombstoned.
var citationSourceQueries = map[string]string{
	CitationSourceMemory: `SELECT 1 FROM memories
	    WHERE id = ? AND collection IN ('', 'memories') AND deleted_at IS NULL LIMIT 1`,
	CitationSourceTheory: `SELECT 1 FROM memories
	    WHERE id = ? AND collection = 'theories' AND deleted_at IS NULL LIMIT 1`,
	CitationSourceDecision: `SELECT 1 FROM memories
	    WHERE id = ? AND collection = 'decisions' AND deleted_at IS NULL LIMIT 1`,
	CitationSourceLesson: `SELECT 1 FROM lessons_base
	    WHERE id = ? AND deleted_at IS NULL LIMIT 1`,
	CitationSourceWork:           `SELECT 1 FROM works WHERE id = ? LIMIT 1`,
	CitationSourceToolInvocation: `SELECT 1 FROM tool_invocations WHERE id = ? LIMIT 1`,
}

// citationSourceExists reports whether the cited id exists AND is of the
// declared type.
//
// The read runs on the supplied DBNode, so it happens INSIDE the caller's
// transaction: the resolution and the citation write see the same snapshot,
// and a citation that cannot be resolved cannot be written.
//
// An unknown source type is an ERROR, not a false result: it is a caller
// mistake, and silently reporting "not found" would hide it.
func citationSourceExists(node DBNode, id, sourceType string) (bool, error) {
	query, ok := citationSourceQueries[sourceType]
	if !ok {
		return false, fmt.Errorf("unknown citation source type %q (valid: %s)",
			sourceType, strings.Join(citationSourceTypes(), ", "))
	}
	var one int
	err := node.QueryRowTracked(query, id).Scan(&one)
	if errors.Is(err, sql.ErrNoRows) {
		return false, nil
	}
	if err != nil {
		return false, fmt.Errorf("resolve citation source %q as type %q: %w",
			id, sourceType, err)
	}
	return true, nil
}

// legacyCitations converts a historical source_ids list into untyped
// citations.
//
// An empty Type marks the legacy path: recorded with the substrate's
// auto-resolved source_type and NOT validated. This is what keeps callers
// that do not supply the new optional field behaving exactly as before.
func legacyCitations(sourceIDs []string) []Citation {
	if len(sourceIDs) == 0 {
		return nil
	}
	out := make([]Citation, 0, len(sourceIDs))
	for _, id := range sourceIDs {
		out = append(out, Citation{ID: id})
	}
	return out
}
