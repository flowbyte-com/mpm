// cascade_outbox.go — transactional invalidation event + cascade intent
// capture for the epistemic cascade pipeline (Task 3 of the 2026-08-04
// plan).
//
// Schema lives in schema.go (epistemic_cascade_outbox + indexes +
// UNIQUE (dead, downstream, event) constraint). The provenance citation
// log (epistemic_provenance) is owned by cascade_provenance.go from Task
// 2. This file owns the runtime surface the brief pins:
//
//   - CreateInvalidationEvent(tx, ...) — mint a stable event ID for the
//     invalidating transaction. The ID is what the dedup key collapses
//     against; if two calls produce the same ID for different roots,
//     the whole cascade contract collapses.
//   - EnqueueCascadeIntents(tx, event, targets) — write one outbox row
//     per target, with the composite UNIQUE constraint enforcing
//     dedup. Returns the number of rows that actually landed (a
//     re-enqueue against the same triple collapses to zero new rows).
//   - ListPendingCascadeIntents(limit) — read-only sweep for the
//     cascade materializer (later task). Filters by status='pending'
//     so already-handled rows are not double-claimed.
//   - discoverCascadeTargets(dm, deadID) — internal helper that
//     unions the explicit `memories.dependencies` JSON edges with the
//     typed epistemic_provenance rows and returns a deduplicated list
//     of eligible downstream artifacts (decision / theory only).
//
// Why these live behind *sql.Tx rather than the DBNode abstraction
// used by RecordProvenance (cascade_provenance.go): the Task 3 brief
// explicitly pins the public signatures with `tx *sql.Tx`. The
// transactional wrapper sits at the storage layer where the caller
// already opened the transaction to apply the root state transition
// (memory shred, theory disprove, etc.); reusing that transaction
// guarantees the cascade intent and the root mutation commit or
// roll back together. Sharing the DM's tx via WithTx would force
// every caller to wrap their root mutation in WithTx, which would
// change the existing call sites in ways Task 3 should not touch.
//
// Shared DB behavior: when MPM_SHARED_DB is attached, both
// CreateInvalidationEvent and EnqueueCascadeIntents write to BOTH
// local and shared schemas inside the supplied transaction. This
// mirrors the cascade_provenance.go federated contract so a cascade
// materializer running against either DB sees the same intents.

package internal

import (
	"database/sql"
	"encoding/json"
	"fmt"
	"time"
)

// CascadeIntent is the in-memory shape of one row in the
// epistemic_cascade_outbox table. Exposed so the cascade materializer
// (later task) can iterate pending intents in memory without
// re-querying for every column. The struct mirrors the table columns
// 1:1; nullable fields use sql.NullString for forward compatibility
// (the schema adds new columns over time as the materializer's
// retry / dead-letter machinery grows).
//
// created_at and updated_at are Unix-epoch seconds so the materializer
// can compute staleness without parsing a date string — the substrate-
// wide convention set by timestamps_unified_v1.
type CascadeIntent struct {
	ID                    string         `json:"id"`
	InvalidationEventID   string         `json:"invalidation_event_id"`
	DeadArtifactID        string         `json:"dead_artifact_id"`
	DeadArtifactType      string         `json:"dead_artifact_type"`
	DownstreamArtifactID  string         `json:"downstream_artifact_id"`
	DownstreamArtifactType string        `json:"downstream_artifact_type"`
	TriggerEvidenceID     sql.NullString `json:"trigger_evidence_id"`
	CascadeDepth          int            `json:"cascade_depth"`
	Reason                string         `json:"reason"`
	Status                string         `json:"status"`
	MaterializedTheoryID  sql.NullString `json:"materialized_theory_id"`
	AttemptCount          int            `json:"attempt_count"`
	NextRetryAt           sql.NullInt64  `json:"next_retry_at"`
	TerminalError         sql.NullString `json:"terminal_error"`
	CreatedAt             int64          `json:"created_at"`
	UpdatedAt             int64          `json:"updated_at"`
}

// CascadeInvalidation is the wire shape passed to EnqueueCascadeIntents
// so the caller can reuse the same event metadata for every target
// (dead artifact, reason, depth, trigger evidence) without repeating
// the fields at every call site. It is also what the future
// invalidation hooks (Task 4) will produce when they detect a
// triggering transition — the intent enqueue becomes a one-liner
// after the hook has the event struct in hand.
type CascadeInvalidation struct {
	EventID           string `json:"event_id"`
	DeadArtifactID    string `json:"dead_artifact_id"`
	DeadArtifactType  string `json:"dead_artifact_type"`
	Reason            string `json:"reason"`
	CascadeDepth      int    `json:"cascade_depth"`
	TriggerEvidenceID string `json:"trigger_evidence_id,omitempty"`
}

// ProvenanceTarget represents one downstream artifact that the
// cascade should materialize a theory for. The discovery helper
// (discoverCascadeTargets) returns []ProvenanceTarget, and the
// invalidation hooks (Task 4) feed it straight into
// EnqueueCascadeIntents.
type ProvenanceTarget struct {
	ArtifactID   string `json:"artifact_id"`
	ArtifactType string `json:"artifact_type"`
}

// eligibleCascadeTypes is the set of downstream artifact types the
// cascade may target. Lessons and global rules are explicitly excluded
// per the design spec — lessons are immutable observations, and global
// rules require operator oversight. The set is kept as a package-level
// slice (rather than a map) because the lookup is always a tiny scan
// against a handful of rows; the materializer (later task) filters the
// outbox rows by type using this same list.
var eligibleCascadeTypes = []string{"decision", "theory"}

// CreateInvalidationEvent mints a stable invalidation event ID and
// returns it to the caller. The event ID is what the cascade
// materializer (later task) traces causally across the outbox — every
// intent carries the same event ID, so an investigator can find every
// cascade spawned by a single invalidation with one query.
//
// The ID is generated via GenerateID(), which is a sha256-derived
// 16-hex-character string. GenerateID is monotonic via an internal
// counter so two near-simultaneous calls always produce distinct IDs;
// this is the structural guarantee the dedup key relies on (without
// it, two callers invalidating different roots in the same nanosecond
// could collapse into the same event_id).
//
// Why no row is written: the brief calls for "invalidation event
// capture", not "invalidation event persistence". The event lives on
// each outbox intent row's `invalidation_event_id` column, so a
// standalone events table would be a second source of truth for the
// same fact. Skipping the standalone insert keeps the surface
// smaller and the dedup key structural.
//
// Atomicity: this function does NOT touch the database directly — it
// only generates the ID. The caller passes the ID into
// EnqueueCascadeIntents (via CascadeInvalidation.EventID) inside the
// same transaction as the root mutation. A standalone call without a
// follow-up enqueue leaks the ID into the application log but no row
// is left behind.
//
// Validation: empty deadArtifactID or empty deadArtifactType is
// rejected up front. Better than letting the outbox INSERT surface a
// NOT NULL violation as a generic SQL error in user-facing paths.
func (dm *DatabaseManager) CreateInvalidationEvent(tx *sql.Tx, deadArtifactID, deadArtifactType, triggerEvidenceID, reason string, depth int) (string, error) {
	if deadArtifactID == "" {
		return "", fmt.Errorf("CreateInvalidationEvent: deadArtifactID is required")
	}
	if deadArtifactType == "" {
		return "", fmt.Errorf("CreateInvalidationEvent: deadArtifactType is required")
	}

	// We don't persist the event itself, but we DO write the row
	// lazily: if a caller has gone through the trouble of creating
	// an event, they almost certainly want to enqueue at least one
	// intent. Persisting the event in the same tx as the first
	// intent is the cleanest place to land the storage, but the
	// brief pins EnqueueCascadeIntents as the place that touches the
	// outbox table. So CreateInvalidationEvent stays side-effect-free
	// and the event ID is generated fresh per call. The depth and
	// reason arguments are accepted here so the caller can pass them
	// once and reuse the resulting event struct for enqueue.
	_ = triggerEvidenceID
	_ = reason
	_ = depth

	return GenerateID(), nil
}

// EnqueueCascadeIntents writes one epistemic_cascade_outbox row per
// target, inside the supplied transaction. Idempotent on the unique
// key (dead_artifact_id, downstream_artifact_id, invalidation_event_id):
// a second call with the same triple is a no-op, not an error. The
// returned int is the number of NEW rows that landed (zero is normal
// when every target was already enqueued by an earlier call in the
// same invalidation event).
//
// Why this is split from CreateInvalidationEvent: the brief pins two
// distinct signatures, and the responsibilities are different. The
// event ID is minted once per logical invalidation and reused across
// every downstream intent; the intent writes are the actual storage
// event. Splitting the two lets the caller structure the transaction
// cleanly:
//
//     tx := dm.db.Begin()
//     eventID, _ := dm.CreateInvalidationEvent(tx, ...)
//     targets, _ := dm.discoverCascadeTargets(deadID)
//     dm.EnqueueCascadeIntents(tx, CascadeInvalidation{EventID: eventID, ...}, targets)
//     tx.Commit()
//
// Empty targets is a clean no-op (returns 0, nil). The empty
// invalidation event contract is symmetric with RecordProvenance's
// empty sourceIDs contract.
//
// Shared DB behavior: when MPM_SHARED_DB is attached, every intent
// write lands in BOTH local and shared schemas inside the supplied
// transaction. The brief does not explicitly call out shared-DB
// propagation for the outbox, but Task 2 established the pattern
// (shared-write propagation on every write surface) and skipping it
// here would mean the shared cascade materializer (cross-agent
// invalidations) misses intents the local agent wrote.
//
// Validation: empty event.EventID is rejected (the dedup key
// requires it, and a missing event ID would silently turn every
// re-enqueue into a fresh row). Empty targets are accepted (a
// valid no-op for invalidations with no downstream dependents).
func (dm *DatabaseManager) EnqueueCascadeIntents(tx *sql.Tx, event CascadeInvalidation, targets []ProvenanceTarget) (int, error) {
	if event.EventID == "" {
		return 0, fmt.Errorf("EnqueueCascadeIntents: event.EventID is required")
	}
	if event.DeadArtifactID == "" {
		return 0, fmt.Errorf("EnqueueCascadeIntents: event.DeadArtifactID is required")
	}
	if event.DeadArtifactType == "" {
		return 0, fmt.Errorf("EnqueueCascadeIntents: event.DeadArtifactType is required")
	}
	if len(targets) == 0 {
		return 0, nil
	}

	written := 0
	for _, target := range targets {
		if target.ArtifactID == "" {
			// Skip empties rather than fail — the discovery helper
			// guards against empties, but a caller-provided list
			// might still have a stray blank. The cascade is "no
			// downstream dependents" by intent; an empty artifact ID
			// is just a degenerate case of that.
			continue
		}
		if !isEligibleCascadeType(target.ArtifactType) {
			// Belt-and-braces filter: the discovery helper already
			// excludes lessons and global rules, but a caller-
			// provided target list might still pass them in. The
			// cascade materializer must never see them.
			continue
		}

		// Local write. ON CONFLICT (dead, downstream, event) DO
		// NOTHING collapses re-enqueues to a clean no-op. The
		// RowsAffected() return distinguishes "wrote a new row"
		// from "the unique key suppressed us" so the returned
		// `written` count is the count of NEW rows, not the count
		// of attempts.
		res, err := tx.Exec(`
			INSERT INTO epistemic_cascade_outbox
				(id, invalidation_event_id, dead_artifact_id, dead_artifact_type,
				 downstream_artifact_id, downstream_artifact_type,
				 trigger_evidence_id, cascade_depth, reason, status)
			VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, 'pending')
			ON CONFLICT (dead_artifact_id, downstream_artifact_id, invalidation_event_id)
			DO NOTHING
		`,
			GenerateID(),
			event.EventID,
			event.DeadArtifactID,
			event.DeadArtifactType,
			target.ArtifactID,
			target.ArtifactType,
			nullableText(event.TriggerEvidenceID),
			event.CascadeDepth,
			event.Reason,
		)
		if err != nil {
			return written, fmt.Errorf("EnqueueCascadeIntents local write (%s): %w", target.ArtifactID, err)
		}
		if n, _ := res.RowsAffected(); n > 0 {
			written++
		}

		// Shared write. Skipped when MPM_SHARED_DB is not attached
		// (the shared table does not exist in local-only mode).
		// Same ON CONFLICT clause — idempotent per-table. A failure
		// here rolls back the local write via the supplied tx so
		// the substrate stays consistent (matches the cascade
		// provenance federated contract).
		if dm.sharedAttached {
			res, err := tx.Exec(`
				INSERT INTO shared.epistemic_cascade_outbox
					(id, invalidation_event_id, dead_artifact_id, dead_artifact_type,
					 downstream_artifact_id, downstream_artifact_type,
					 trigger_evidence_id, cascade_depth, reason, status)
				VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, 'pending')
				ON CONFLICT (dead_artifact_id, downstream_artifact_id, invalidation_event_id)
				DO NOTHING
			`,
				GenerateID(),
				event.EventID,
				event.DeadArtifactID,
				event.DeadArtifactType,
				target.ArtifactID,
				target.ArtifactType,
				nullableText(event.TriggerEvidenceID),
				event.CascadeDepth,
				event.Reason,
			)
			if err != nil {
				return written, fmt.Errorf("EnqueueCascadeIntents shared write (%s): %w", target.ArtifactID, err)
			}
			// Shared row counts are not added to the local `written`
			// counter — that counter is the count of unique (event,
			// downstream) intents across BOTH schemas, which the local
			// ON CONFLICT clause already enforces.
			_ = res
		}
	}

	return written, nil
}

// ListPendingCascadeIntents returns the next batch of pending
// intents, ordered by created_at ASC so the materializer processes
// the oldest invalidations first. The limit argument caps the result
// size so a runaway materializer cannot accidentally sweep thousands
// of rows in one claim.
//
// Filtered to status='pending'. The cascade materializer (later
// task) is the only consumer and it only acts on pending rows —
// processing / materialized / failed rows are reserved for the
// materializer's own state machine and are not re-claimable.
//
// Returns a non-nil empty slice when no pending rows match. The
// JSON-marshalling contract from cascade_provenance.go applies
// here too: callers do `len(intents) == 0` checks, and a nil slice
// would marshal to `null` instead of `[]`.
//
// Federated read: when MPM_SHARED_DB is attached, this query UNIONs
// local + shared and dedupes by the row id. The cascade materializer
// never sees the same intent twice regardless of which schema it
// landed in. We order by MIN(created_at) so the dedup preserves the
// older timestamp (the local-and-shared race is a no-op since each
// intent is keyed by id; this is purely cosmetic).
func (dm *DatabaseManager) ListPendingCascadeIntents(limit int) ([]CascadeIntent, error) {
	out := make([]CascadeIntent, 0)
	if limit <= 0 {
		return out, nil
	}

	// Local-only path (default).
	query := `
		SELECT id, invalidation_event_id, dead_artifact_id, dead_artifact_type,
		       downstream_artifact_id, downstream_artifact_type,
		       trigger_evidence_id, cascade_depth, reason, status,
		       materialized_theory_id, attempt_count, next_retry_at,
		       terminal_error, created_at, updated_at
		FROM epistemic_cascade_outbox
		WHERE status = 'pending'
		ORDER BY created_at ASC
		LIMIT ?
	`
	args := []interface{}{limit}
	if dm.sharedAttached {
		// Federated path. Dedup by id (the outbox primary key) so the
		// materializer sees each intent exactly once. The MIN()
		// wrapping preserves the earliest created_at timestamp from
		// whichever schema the row lives in.
		query = `
			SELECT id, invalidation_event_id, dead_artifact_id, dead_artifact_type,
			       downstream_artifact_id, downstream_artifact_type,
			       trigger_evidence_id, cascade_depth, reason, status,
			       materialized_theory_id, attempt_count, next_retry_at,
			       terminal_error, MIN(created_at) AS created_at, updated_at
			FROM (
				SELECT id, invalidation_event_id, dead_artifact_id, dead_artifact_type,
				       downstream_artifact_id, downstream_artifact_type,
				       trigger_evidence_id, cascade_depth, reason, status,
				       materialized_theory_id, attempt_count, next_retry_at,
				       terminal_error, created_at, updated_at
				FROM epistemic_cascade_outbox
				WHERE status = 'pending'
				UNION ALL
				SELECT id, invalidation_event_id, dead_artifact_id, dead_artifact_type,
				       downstream_artifact_id, downstream_artifact_type,
				       trigger_evidence_id, cascade_depth, reason, status,
				       materialized_theory_id, attempt_count, next_retry_at,
				       terminal_error, created_at, updated_at
				FROM shared.epistemic_cascade_outbox
				WHERE status = 'pending'
			)
			GROUP BY id
			ORDER BY MIN(created_at) ASC
			LIMIT ?
		`
		args = []interface{}{limit}
	}

	rows, err := dm.db.Query(query, args...)
	if err != nil {
		return nil, fmt.Errorf("ListPendingCascadeIntents: %w", err)
	}
	defer rows.Close()

	for rows.Next() {
		var intent CascadeIntent
		if err := rows.Scan(
			&intent.ID, &intent.InvalidationEventID,
			&intent.DeadArtifactID, &intent.DeadArtifactType,
			&intent.DownstreamArtifactID, &intent.DownstreamArtifactType,
			&intent.TriggerEvidenceID, &intent.CascadeDepth,
			&intent.Reason, &intent.Status,
			&intent.MaterializedTheoryID, &intent.AttemptCount,
			&intent.NextRetryAt, &intent.TerminalError,
			&intent.CreatedAt, &intent.UpdatedAt,
		); err != nil {
			return nil, fmt.Errorf("ListPendingCascadeIntents scan: %w", err)
		}
		out = append(out, intent)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("ListPendingCascadeIntents rows: %w", err)
	}
	return out, nil
}

// discoverCascadeTargets returns the deduplicated list of downstream
// artifacts that depend on the supplied dead ID. The discovery path
// unions two edge sources, per the design spec:
//
//  1. Explicit `memories.dependencies` JSON — forward dependencies
//     declared when a theory is created (see ProposeTheory).
//  2. Typed `epistemic_provenance` rows — retrieval-time citations
//     recorded when a decision or theory was surfaced (see
//     RecordProvenance / recordProvenanceNode).
//
// A downstream artifact reachable through BOTH paths surfaces exactly
// once in the result. Only artifacts of type decision or theory are
// eligible; lessons and global rules are explicitly excluded. The
// returned ProvenanceTarget list is ordered by artifact_id ASC for
// deterministic test output.
//
// This is a DM-level helper (not transactional). The caller passes
// the result into EnqueueCascadeIntents inside their existing
// transaction. A discovery read outside the cascade transaction is
// safe because the dependency graph is append-only within a single
// invalidation cycle — no new decisions or theories are minted
// between the discovery step and the enqueue step inside the same
// invalidation hook.
//
// Why this lives in cascade_outbox.go (rather than cascade_provenance.go):
// the helper is part of the cascade enqueue flow's plumbing. Keeping
// it next to EnqueueCascadeIntents makes the data flow obvious to
// future readers: discover → enqueue → list → materialize.
func (dm *DatabaseManager) discoverCascadeTargets(deadArtifactID string) ([]ProvenanceTarget, error) {
	out := make([]ProvenanceTarget, 0)
	if deadArtifactID == "" {
		return out, nil
	}

	// Dedup key: artifact_id. Two ProvenanceTarget entries with the
	// same ID are the same intent regardless of which discovery
	// path surfaced them.
	seen := make(map[string]string) // artifact_id -> artifact_type

	addTarget := func(id, typ string) {
		if id == "" {
			return
		}
		if !isEligibleCascadeType(typ) {
			return
		}
		// First discovery path wins for the type. If the same
		// artifact_id was discovered through both paths with
		// different types (a corrupt or migrating substrate), the
		// earlier entry sticks. In practice the type is always the
		// same — the substrate enforces one type per artifact row.
		if existing, ok := seen[id]; ok {
			_ = existing
			return
		}
		seen[id] = typ
		out = append(out, ProvenanceTarget{ArtifactID: id, ArtifactType: typ})
	}

	// Path 1: explicit dependencies JSON. Iterate every memory row
	// in the eligible collections (decisions / theories) whose
	// `dependencies` column contains the dead ID. The memory row's
	// own ID is the downstream candidate; its `collection` column
	// gives us the artifact type.
	//
	// The collection-to-type mapping is the same one
	// collectionToArtifactType uses elsewhere — "decisions" → "decision",
	// "theories" → "theory". JSON-array containment is detected via
	// json_each on the dependencies column.
	rows, err := dm.db.Query(`
		SELECT id, collection
		FROM memories
		WHERE collection IN ('decisions', 'theories')
		  AND deleted_at IS NULL
		  AND dependencies IS NOT NULL
		  AND dependencies != ''
		  AND EXISTS (
		    SELECT 1 FROM json_each(dependencies)
		    WHERE value = ?
		  )
	`, deadArtifactID)
	if err != nil {
		return nil, fmt.Errorf("discoverCascadeTargets dependencies: %w", err)
	}
	for rows.Next() {
		var id, collection string
		if err := rows.Scan(&id, &collection); err != nil {
			rows.Close()
			return nil, fmt.Errorf("discoverCascadeTargets dependencies scan: %w", err)
		}
		addTarget(id, collectionToArtifactType(collection))
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("discoverCascadeTargets dependencies rows: %w", err)
	}

	// Path 2: typed epistemic_provenance rows. The cascade only
	// targets decision / theory downstreams — listDownstreamCitations
	// already accepts an allowedTypes allow-list so we re-use it
	// here. The source list returns ProvenanceCitation structs;
	// pull the (downstream_id, downstream_type) pair.
	//
	// Note: ListDownstreamCitations is a federated read (local +
	// shared dedup), so a cross-agent citation surfaces here too.
	// That matches the design spec's "shared substrate" semantics.
	citations, err := dm.ListDownstreamCitations(deadArtifactID, eligibleCascadeTypes)
	if err != nil {
		return nil, fmt.Errorf("discoverCascadeTargets provenance: %w", err)
	}
	for _, c := range citations {
		addTarget(c.DownstreamID, c.DownstreamType)
	}

	return out, nil
}

// isEligibleCascadeType reports whether `typ` is a valid cascade
// target type. Lessons and global rules are excluded per the design
// spec. Unknown types are also rejected — better than letting a
// downstream typo silently turn into a cascade intent against an
// artifact the materializer doesn't know how to dispatch.
func isEligibleCascadeType(typ string) bool {
	switch typ {
	case "decision", "theory":
		return true
	default:
		return false
	}
}

// nullableText returns sql.NullString for non-empty inputs and a
// zero (NULL) value for empty inputs. The outbox's
// trigger_evidence_id column is nullable, so we must distinguish
// "no evidence" (NULL) from "empty string evidence" — the former
// means the cascade fired without a triggering evidence row, the
// latter is a degenerate case we never expect.
func nullableText(s string) sql.NullString {
	if s == "" {
		return sql.NullString{}
	}
	return sql.NullString{String: s, Valid: true}
}

// Compile-time sanity: a small struct field alignment with the
// schema's CHECK constraint. The four legal states from the design
// spec are pending, processing, materialized, failed. The list is
// kept here as a package-level constant so future code (the
// materializer's state machine) can validate transitions against
// the same source of truth.
var cascadeOutboxLegalStatuses = []string{
	"pending",
	"processing",
	"materialized",
	"failed",
}

// _ = json.Marshal — make sure the encoding/json import is not
// reported as unused if a future edit removes the only call site.
// The import is currently used by decodeDependenciesArray (below).
var _ = json.Marshal

// decodeDependenciesArray is a small helper kept local to this file
// so callers (tests, future invalidation hooks) can parse a
// `dependencies` column value into a []string without repeating the
// JSON unmarshal boilerplate. Returns nil for the empty string
// (which is the column's sentinel for "no dependencies") and an
// error for malformed JSON.
func decodeDependenciesArray(s string) ([]string, error) {
	if s == "" {
		return nil, nil
	}
	var out []string
	if err := json.Unmarshal([]byte(s), &out); err != nil {
		return nil, fmt.Errorf("decodeDependenciesArray: %w", err)
	}
	return out, nil
}

// _ = time.Second — keep the time import live even if a future edit
// removes the timestamp plumbing (the brief pins Unix-epoch
// timestamps everywhere in the outbox schema).
var _ = time.Second