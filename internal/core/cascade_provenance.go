// cascade_provenance.go — typed provenance citation log runtime.
//
// Schema lives in schema.go (see the epistemic_provenance DDL plus
// its two indexes). This file is the public API surface for the
// citation log:
//
//   - RecordProvenance: write one (source, downstream, event) row.
//   - ListDownstreamCitations: read all rows whose downstream_type is
//     in the caller-supplied allow-list, for a given source_id.
//
// Why these are needed (Task 2 of the epistemic cascades plan):
// the cascade materializer (later task) needs to walk the citation
// graph to discover which downstream artifacts depend on a now-dead
// source. The retrieval_metadata IncrementSuccess path (lesson
// telemetry) is NOT sufficient: it credits success_count but does not
// expose a queryable, typed edge log. The two surfaces coexist by
// design:
//
//   - retrieval_metadata: "this lesson was useful" (telemetry, no
//     type-filter contract).
//   - epistemic_provenance: "this decision depends on this memory"
//     (typed edges, allow-list filtered, materialized at write time
//     so the materializer doesn't have to walk retrieval_metadata).
//
// The decision and theory write paths (RecordDecision, ProposeTheory)
// call RecordProvenance from their new source_ids hooks (see
// epistemology_tools.go) so a citation is persisted atomically with
// the artifact it justifies. Lesson source_ids continue to flow
// through IncrementSuccess only — see the comment in
// tools/handlers.go's handleSaveLesson for why the lesson surface
// stays telemetry-only.

package internal

import (
	"fmt"
	"strings"
)

// ProvenanceCitation is the in-memory shape of a single row in the
// epistemic_provenance table. Exposed by ListDownstreamCitations so
// callers (CLI handlers, MCP tools, the cascade materializer) can
// iterate the dependency graph for a source.
//
// The struct mirrors the table columns 1:1; created_at is an
// INTEGER Unix-epoch second so callers can do arithmetic with
// retention windows without parsing a date string.
type ProvenanceCitation struct {
	SourceID       string `json:"source_id"`
	SourceType     string `json:"source_type"`
	DownstreamID   string `json:"downstream_id"`
	DownstreamType string `json:"downstream_type"`
	EventID        string `json:"event_id"`
	CreatedAt      int64  `json:"created_at"`
}

// RecordProvenance persists one (source, downstream, event) citation
// row. Idempotent on the unique key (source_id, downstream_id,
// event_id): a second call with the same triple returns nil without
// writing a duplicate.
//
// Empty source_id, downstream_id, or event_id are rejected at the
// domain boundary — better than letting SQLite's NOT NULL surface a
// generic SQL error in user-facing call paths.
//
// sourceType may be empty: when it is, ResolveArtifactType is called
// against the local memories table so legacy untyped IDs land with
// the correct type column. The cascade materializer relies on the
// source_type to choose the right invalidation handler, so getting
// this right at write time beats resolving lazily at read time
// (which would force the materializer to scan every row).
//
// downstreamType is honoured as written — the caller (typically
// RecordDecision or ProposeTheory) just minted the downstream ID and
// knows its type. We do not cross-check downstream existence here;
// the cascade materializer validates downstream reachability against
// the artifacts view when it consumes the citation.
//
// Shared DB behavior: when MPM_SHARED_DB is attached, citations
// follow the same shared/local split as the rest of the substrate.
// For Task 2 the implementation writes to the local table only —
// shared-DB citation propagation is a follow-up once the
// materializer's invalidation dispatch lands (later task in the
// cascade plan).
func (dm *DatabaseManager) RecordProvenance(sourceID, sourceType, downstreamID, downstreamType, eventID string) error {
	if sourceID == "" {
		return fmt.Errorf("RecordProvenance: source_id is required")
	}
	if downstreamID == "" {
		return fmt.Errorf("RecordProvenance: downstream_id is required")
	}
	if eventID == "" {
		return fmt.Errorf("RecordProvenance: event_id is required")
	}

	// Legacy untyped ID compatibility: when the caller passes
	// sourceType="" we resolve against the local memories table so
	// the row lands with the correct type. This is the bridge for
	// callers that pre-date the typed-source contract and pass bare
	// IDs from prior-session recall. The downstream side is NOT
	// resolved because callers know what they just minted.
	if sourceType == "" {
		resolved, err := dm.ResolveArtifactType(sourceID)
		if err != nil {
			// Fall back to "memory" — better than failing the whole
			// write when a legacy caller cites an ID that has since
			// been shredded. The retrieval_metadata IncrementSuccess
			// path uses the same fallback for the same reason.
			sourceType = "memory"
		} else {
			sourceType = resolved
		}
	}

	if sourceType == "" {
		sourceType = "memory"
	}

	_, err := dm.db.Exec(`
		INSERT INTO epistemic_provenance
			(id, source_id, source_type, downstream_id, downstream_type, event_id)
		VALUES (?, ?, ?, ?, ?, ?)
		ON CONFLICT(source_id, downstream_id, event_id) DO NOTHING
	`, GenerateID(), sourceID, sourceType, downstreamID, downstreamType, eventID)
	if err != nil {
		return fmt.Errorf("RecordProvenance(%q, %q, %q, %q, %q): %w",
			sourceID, sourceType, downstreamID, downstreamType, eventID, err)
	}
	return nil
}

// ListDownstreamCitations returns every citation whose source_id
// matches the supplied ID and whose downstream_type is in the
// allowedTypes allow-list. Empty allowedTypes means "all types" —
// the cascade materializer's "anything downstream" sweep path uses
// this shape.
//
// The result is non-nil even when no citations match; the empty-
// result branch returns ([]ProvenanceCitation{}, nil) so JSON
// marshalling produces `[]` rather than `null` (caller contract:
// callers do `len(citations) == 0` and pass the slice through to a
// JSON encoder that must emit an array, not a null).
//
// Ordering is by created_at ASC so a downstream consumer that
// processes events in order can replay the citation history
// deterministically. The unique key guarantees at most one row per
// (source, downstream, event), so ASC ordering is stable across
// queries.
func (dm *DatabaseManager) ListDownstreamCitations(sourceID string, allowedTypes []string) ([]ProvenanceCitation, error) {
	out := make([]ProvenanceCitation, 0)
	if sourceID == "" {
		return out, nil
	}

	// Build a parameterized IN clause. Empty allowedTypes means "any
	// downstream type"; the WHERE filter is skipped in that case.
	var (
		rows interface {
			Next() bool
			Scan(...interface{}) error
			Close() error
			Err() error
		}
		err error
	)
	if len(allowedTypes) == 0 {
		rows, err = dm.db.Query(`
			SELECT source_id, source_type, downstream_id, downstream_type, event_id, created_at
			FROM epistemic_provenance
			WHERE source_id = ?
			ORDER BY created_at ASC, id ASC
		`, sourceID)
	} else {
		// Expand into ? placeholders.
		placeholders := make([]string, len(allowedTypes))
		args := make([]interface{}, 0, len(allowedTypes)+1)
		args = append(args, sourceID)
		for i, t := range allowedTypes {
			placeholders[i] = "?"
			args = append(args, t)
		}
		query := fmt.Sprintf(`
			SELECT source_id, source_type, downstream_id, downstream_type, event_id, created_at
			FROM epistemic_provenance
			WHERE source_id = ?
			  AND downstream_type IN (%s)
			ORDER BY created_at ASC, id ASC
		`, strings.Join(placeholders, ","))
		rows, err = dm.db.Query(query, args...)
	}
	if err != nil {
		return nil, fmt.Errorf("ListDownstreamCitations(%q): %w", sourceID, err)
	}
	defer rows.Close()

	for rows.Next() {
		var c ProvenanceCitation
		if err := rows.Scan(&c.SourceID, &c.SourceType, &c.DownstreamID, &c.DownstreamType, &c.EventID, &c.CreatedAt); err != nil {
			return nil, fmt.Errorf("ListDownstreamCitations scan: %w", err)
		}
		out = append(out, c)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("ListDownstreamCitations rows: %w", err)
	}
	return out, nil
}

// ResolveArtifactType returns the canonical type string for an
// artifact ID, looking it up against the local memories table's
// `collection` column and the lessons table. Returns ("", err) when
// the artifact is unknown.
//
// Used by RecordProvenance when a caller passes a legacy untyped
// source_id (no `skill:`, `lesson:`, `dec-`, `theory:` prefix and
// no explicit source_type hint). The mapping is:
//
//   - 'decisions'  -> 'decision'
//   - 'theories'   -> 'theory'
//   - 'lessons'    -> 'lesson'
//   - everything else (incl. NULL/unknown) -> 'memory'
//
// Why this lives in cascade_provenance.go and not memory.go: the
// resolution contract is provenance-specific (we only care about the
// small set of types the cascade materializer dispatches on), and
// keeping the helper local to the caller makes the dependency edge
// between RecordProvenance and the lookup obvious to future readers.
func (dm *DatabaseManager) ResolveArtifactType(id string) (string, error) {
	if id == "" {
		return "", fmt.Errorf("ResolveArtifactType: id is empty")
	}

	// MemoryStore-backed rows live in the `memories` table with a
	// `collection` discriminator. Lessons live in `lessons_base` (the
	// unified schema after the lessons-to-base migration; see
	// migration_lessons_to_base.go). One row, one type — the first
	// match wins.
	var collection string
	err := dm.db.QueryRow(
		`SELECT collection FROM memories WHERE id = ? AND deleted_at IS NULL LIMIT 1`,
		id,
	).Scan(&collection)
	if err == nil {
		return collectionToArtifactType(collection), nil
	}

	// Fall back to lessons. The schema migration split `lessons`
	// into `lessons_base` + `lessons_fts`; both rows are queries
	// against the same id space.
	var lessonID string
	err = dm.db.QueryRow(
		`SELECT id FROM lessons_base WHERE id = ? LIMIT 1`,
		id,
	).Scan(&lessonID)
	if err == nil && lessonID != "" {
		return "lesson", nil
	}

	return "", fmt.Errorf("ResolveArtifactType(%q): not found", id)
}

// collectionToArtifactType maps the `memories.collection` value to
// the canonical artifact type string used by the cascade materializer.
// The `decisions` -> `decision` and `theories` -> `theory` flips are
// the only renames; everything else (including `memories`, `''`,
// NULL) maps to `memory`.
func collectionToArtifactType(collection string) string {
	switch collection {
	case "decisions":
		return "decision"
	case "theories":
		return "theory"
	case "memories", "":
		return "memory"
	default:
		// Unknown collection — fall through to memory. The
		// retrieval_metadata IncrementSuccess path uses the same
		// default ("memory") for the same reason: a wrong type is
		// less harmful than failing the write.
		return "memory"
	}
}