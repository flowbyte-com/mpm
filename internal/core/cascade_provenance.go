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
// Shared DB behavior: when MPM_SHARED_DB is attached, RecordProvenance
// writes to BOTH `epistemic_provenance` (local) AND
// `shared.epistemic_provenance` (shared) under the same transaction.
// ListDownstreamCitations UNIONs both scopes and dedupes by the
// unique key (source_id, downstream_id, event_id) so the cascade
// materializer sees each citation exactly once regardless of which
// DB the row was written to. This matches the substrate's existing
// "write to local AND shared when shared is attached" pattern (see
// arbitrary.go's applyArbitrationResolution, which writes both
// shared.memories and shared.evidence under one tx).
//
// Lesson surface stays telemetry-only: the save_lesson handler
// credits source_ids via IncrementSuccess (retrieval_metadata
// success_count) and does NOT route through RecordProvenance. This
// keeps the cascade materializer's "decision-or-theory-only" filter
// clean — see tools/handlers.go's handleSaveLesson for the rationale.

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
// row, in BOTH the local epistemic_provenance table AND the shared
// table (if MPM_SHARED_DB is attached). Idempotent on the unique key
// (source_id, downstream_id, event_id): a second call with the same
// triple returns nil without writing a duplicate.
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
// Atomicity: the local + shared writes happen inside a single
// transaction (via WithTx). If either write fails, neither row
// lands. This matches the rest of the substrate's shared-write
// pattern (see arbitration.go's applyArbitrationResolution).
func (dm *DatabaseManager) RecordProvenance(sourceID, sourceType, downstreamID, downstreamType, eventID string) error {
	return dm.WithTx(func(node DBNode) error {
		return dm.recordProvenanceNode(node, sourceID, sourceType, downstreamID, downstreamType, eventID)
	})
}

// recordProvenanceNode is the transactional inner core of
// RecordProvenance. It accepts a DBNode so it can be called from
// inside a larger transaction (e.g. ProposeTheory wraps the artifact
// insert + citation loop in one WithTx so a mid-loop failure rolls
// back the artifact itself).
//
// When MPM_SHARED_DB is attached, the citation is written to BOTH
// local and shared schemas under the same transaction. The shared
// write is a no-op when shared is not attached (the shared table
// doesn't exist). The unique key collapse is per-table, so a
// duplicate within local collapses to one row, and a duplicate
// within shared also collapses to one row — but a write to local
// and a write to shared are independent rows. The read surface
// (ListDownstreamCitations) dedupes by the unique key in the
// application layer.
//
// The INSERT uses ON CONFLICT(... ) DO NOTHING so the
// idempotency contract holds when the same citation is written
// twice (e.g. by a noisy recall turn or a retry).
func (dm *DatabaseManager) recordProvenanceNode(node DBNode, sourceID, sourceType, downstreamID, downstreamType, eventID string) error {
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
	//
	// Resolution happens against the DM (not node) because the
	// ResolveArtifactType lookup is a read that doesn't need to be
	// in the transaction. The caller may already be inside a tx,
	// so we use dm.db directly rather than node.
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

	// Local write. Always executes — the local table is the
	// always-on substrate.
	if _, err := node.ExecTracked(`
		INSERT INTO epistemic_provenance
			(id, source_id, source_type, downstream_id, downstream_type, event_id)
		VALUES (?, ?, ?, ?, ?, ?)
		ON CONFLICT(source_id, downstream_id, event_id) DO NOTHING
	`, 0, GenerateID(), sourceID, sourceType, downstreamID, downstreamType, eventID); err != nil {
		return fmt.Errorf("RecordProvenance local write: %w", err)
	}

	// Shared write. Skipped when MPM_SHARED_DB is not attached
	// (shared table does not exist in local-only mode). The
	// ON CONFLICT clause is the same shape — idempotent per-table.
	//
	// Failure modes that propagate as errors (and thus roll back
	// the local write via the outer tx):
	//   - source_id/downstream_id/event_id fail shared's NOT NULL
	//     check (shouldn't happen — already validated above).
	//   - shared DB is read-only (test fixture).
	//   - shared DB schema is missing the table (init drift).
	//
	// All of these are operator-visible failures (the local write
	// succeeded but the shared copy didn't, so the substrate is
	// inconsistent). Rolling back the local copy is the correct
	// outcome: a half-written citation is worse than a clean
	// failure.
	if dm.sharedAttached {
		if _, err := node.ExecTracked(`
			INSERT INTO shared.epistemic_provenance
				(id, source_id, source_type, downstream_id, downstream_type, event_id)
			VALUES (?, ?, ?, ?, ?, ?)
			ON CONFLICT(source_id, downstream_id, event_id) DO NOTHING
		`, 0, GenerateID(), sourceID, sourceType, downstreamID, downstreamType, eventID); err != nil {
			return fmt.Errorf("RecordProvenance shared write: %w", err)
		}
	}
	return nil
}

// ListDownstreamCitations returns every citation whose source_id
// matches the supplied ID and whose downstream_type is in the
// allowedTypes allow-list. Empty allowedTypes means "all types" —
// the cascade materializer's "anything downstream" sweep path uses
// this shape.
//
// Federated read: when MPM_SHARED_DB is attached, this query UNIONs
// the local and shared tables and dedupes by the unique key
// (source_id, downstream_id, event_id). The cascade materializer
// never sees a citation twice regardless of which DB it landed in.
// The created_at column comes from the row that won the dedup — for
// the cascade materializer that's a stable timestamp one would
// inspect via a separate query if needed.
//
// The result is non-nil even when no citations match; the empty-
// result branch returns ([]ProvenanceCitation{}, nil) so JSON
// marshalling produces `[]` rather than `null` (caller contract:
// callers do `len(citations) == 0` and pass the slice through to a
// JSON encoder that must emit an array, not a null).
//
// Ordering is by created_at ASC so a downstream consumer that
// processes events in order can replay the citation history
// deterministically.
func (dm *DatabaseManager) ListDownstreamCitations(sourceID string, allowedTypes []string) ([]ProvenanceCitation, error) {
	out := make([]ProvenanceCitation, 0)
	if sourceID == "" {
		return out, nil
	}

	// Build the type allow-list clause. Empty allowedTypes means
	// "any downstream type" — the WHERE filter is skipped.
	allowedClause := ""
	args := []interface{}{sourceID}
	if len(allowedTypes) > 0 {
		placeholders := make([]string, len(allowedTypes))
		for i, t := range allowedTypes {
			placeholders[i] = "?"
			args = append(args, t)
		}
		allowedClause = fmt.Sprintf("AND downstream_type IN (%s)", strings.Join(placeholders, ","))
	}

	// Local-only query when shared is not attached.
	query := fmt.Sprintf(`
		SELECT source_id, source_type, downstream_id, downstream_type, event_id, MIN(created_at)
		FROM (
			SELECT source_id, source_type, downstream_id, downstream_type, event_id, created_at
			FROM epistemic_provenance
			WHERE source_id = ? %s
			%s
		)
		GROUP BY source_id, downstream_id, event_id
		ORDER BY MIN(created_at) ASC
	`, allowedClause, "")
	if dm.sharedAttached {
		// Federated query: UNION ALL local + shared, dedup by the
		// unique key. The outer SELECT collapses the duplicate and
		// keeps the earliest created_at so the dedup is stable.
		//
		// Args layout for the federated query: source_id (local),
		// allowedTypes (local), source_id (shared), allowedTypes
		// (shared). Reconstruct from scratch rather than appending
		// to the original args slice to keep the order obvious.
		query = fmt.Sprintf(`
			SELECT source_id, source_type, downstream_id, downstream_type, event_id, MIN(created_at)
			FROM (
				SELECT source_id, source_type, downstream_id, downstream_type, event_id, created_at
				FROM epistemic_provenance
				WHERE source_id = ? %s
				UNION ALL
				SELECT source_id, source_type, downstream_id, downstream_type, event_id, created_at
				FROM shared.epistemic_provenance
				WHERE source_id = ? %s
			)
			GROUP BY source_id, downstream_id, event_id
			ORDER BY MIN(created_at) ASC
		`, allowedClause, allowedClause)
		args = make([]interface{}, 0, 2*(1+len(allowedTypes)))
		args = append(args, sourceID)
		for _, t := range allowedTypes {
			args = append(args, t)
		}
		args = append(args, sourceID)
		for _, t := range allowedTypes {
			args = append(args, t)
		}
	}

	rows, err := dm.db.Query(query, args...)
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

	// Shared lookup: when MPM_SHARED_DB is attached, a cited
	// artifact may live in shared.memories (cross-agent / global
	// rules). Same collection mapping applies.
	if dm.sharedAttached {
		var sharedCollection string
		err := dm.db.QueryRow(
			`SELECT collection FROM shared.memories WHERE id = ? AND deleted_at IS NULL LIMIT 1`,
			id,
		).Scan(&sharedCollection)
		if err == nil {
			return collectionToArtifactType(sharedCollection), nil
		}
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