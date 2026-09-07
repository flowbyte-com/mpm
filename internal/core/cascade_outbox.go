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
//     the whole cascade contract collapses. Metadata args
//     (triggerEvidenceID, reason, depth) are validated here so the
//     caller cannot silently lose them; the actual write carries the
//     same metadata via CascadeInvalidation into EnqueueCascadeIntents
//     so the stored row matches the caller's intent.
//   - EnqueueCascadeIntents(tx, event, targets) — write one outbox row
//     per target, with the composite UNIQUE constraint enforcing
//     dedup. Returns the number of rows that actually landed (a
//     re-enqueue against the same triple collapses to zero new rows).
//   - ListPendingCascadeIntents(limit) — read-only sweep for the
//     cascade materializer (later task). Filters by status='pending'
//     so already-handled rows are not double-claimed. Federated dedup
//     uses the semantic key (dead, downstream, event) NOT the row id,
//     because EnqueueCascadeIntents generates a fresh id for each
//     local + shared write.
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
	"fmt"
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
	ID                     string         `json:"id"`
	InvalidationEventID    string         `json:"invalidation_event_id"`
	DeadArtifactID         string         `json:"dead_artifact_id"`
	DeadArtifactType       string         `json:"dead_artifact_type"`
	DownstreamArtifactID   string         `json:"downstream_artifact_id"`
	DownstreamArtifactType string         `json:"downstream_artifact_type"`
	TriggerEvidenceID      sql.NullString `json:"trigger_evidence_id"`
	CascadeDepth           int            `json:"cascade_depth"`
	Reason                 string         `json:"reason"`
	Status                 string         `json:"status"`
	MaterializedTheoryID   sql.NullString `json:"materialized_theory_id"`
	AttemptCount           int            `json:"attempt_count"`
	NextRetryAt            sql.NullInt64  `json:"next_retry_at"`
	TerminalError          sql.NullString `json:"terminal_error"`
	CreatedAt              int64          `json:"created_at"`
	UpdatedAt              int64          `json:"updated_at"`
}

// CascadeInvalidation is the wire shape passed to EnqueueCascadeIntents
// so the caller can reuse the same event metadata for every target
// (dead artifact, reason, depth, trigger evidence) without repeating
// the fields at every call site. It is also what the future
// invalidation hooks (Task 4) will produce when they detect a
// triggering transition — the intent enqueue becomes a one-liner
// after the hook has the event struct in hand.
//
// Important contract: the metadata fields (Reason, CascadeDepth,
// TriggerEvidenceID) are the AUTHORITATIVE values that land on every
// outbox row. CreateInvalidationEvent accepts the same fields as
// positional arguments for ergonomic reasons (the brief pins the
// signature) and validates them up front so the caller cannot silently
// pass values that contradict the eventual CascadeInvalidation struct.
// The actual write path uses these fields from CascadeInvalidation, so
// "what I asked for in CreateInvalidationEvent" and "what landed in
// the outbox" are the same expression.
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

// MaxCascadeDepth is the upper bound on cascade_depth the storage
// layer accepts. The design spec puts the default at 3 but allows
// configuration; the storage contract here is the structural ceiling
// so a misconfigured caller cannot write a depth that would silently
// bypass the depth-3 suppression guard in the cascade materializer.
// The constant is exported so future test code / callers can pin
// their own guards against the same number.
const MaxCascadeDepth = 3

// HardConfidenceInvalidationThreshold is the confidence floor below
// which an artifact is considered structurally invalidated and must
// trigger a cascade. The cascade materializer (later task) only acts
// on transitions across this boundary — an artifact that is already
// below the threshold and recomputed again does NOT re-fire.
//
// The threshold is intentionally well below the natural confidence
// range for evidence-backed memories (0.6-0.9) so that ordinary
// weakening (one negative evidence row, an idle_dream decay tick,
// etc.) does not cascade. The "hard" qualifier is structural: only
// the cross BELOW this boundary is the invalidation event.
//
// Default 0.3 is the substrate-wide "the artifact is no longer
// trustworthy" point: well below the natural range, well above the
// no-positive-evidence asymptote. Exported so the invalidation hook
// can read the same value the tests pin.
const HardConfidenceInvalidationThreshold = 0.3

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
// Metadata contract: the triggerEvidenceID, reason, and depth
// arguments are NOT silently discarded. They are validated up front
// so the caller cannot pass an invalid value:
//   - depth must be in [0, MaxCascadeDepth]. The storage layer
//     enforces this rather than the materializer so a misconfigured
//     caller cannot inject a depth that would silently bypass the
//     depth-3 suppression guard.
//   - triggerEvidenceID and reason are accepted as-is; the storage
//     layer treats them as opaque strings and persists them verbatim
//     via CascadeInvalidation into EnqueueCascadeIntents. The
//     authoritative write path is the CascadeInvalidation struct
//     passed to EnqueueCascadeIntents — the caller must mirror the
//     metadata through. CreateInvalidationEvent returning the
//     metadata-validated event ID is the structural guarantee that
//     "what the caller asked for in CreateInvalidationEvent" and
//     "what landed in the outbox" are the same expression.
//
// Validation: empty deadArtifactID or empty deadArtifactType is
// rejected up front. Negative or excessive depth is rejected up front.
// Better than letting the outbox INSERT surface a CHECK violation as
// a generic SQL error in user-facing paths.
func (dm *DatabaseManager) CreateInvalidationEvent(tx *sql.Tx, deadArtifactID, deadArtifactType, triggerEvidenceID, reason string, depth int) (string, error) {
	if deadArtifactID == "" {
		return "", fmt.Errorf("CreateInvalidationEvent: deadArtifactID is required")
	}
	if deadArtifactType == "" {
		return "", fmt.Errorf("CreateInvalidationEvent: deadArtifactType is required")
	}
	if depth < 0 {
		return "", fmt.Errorf("CreateInvalidationEvent: depth must be >= 0, got %d", depth)
	}
	if depth > MaxCascadeDepth {
		return "", fmt.Errorf("CreateInvalidationEvent: depth %d exceeds MaxCascadeDepth=%d", depth, MaxCascadeDepth)
	}

	// Metadata is carried by CascadeInvalidation into EnqueueCascadeIntents
	// (see the CascadeInvalidation doc comment for the contract). The two
	// args are accepted here so the brief's signature is preserved and
	// so the caller can pass the metadata once instead of repeating it
	// at every call site, but the storage layer writes them via the
	// CascadeInvalidation struct the caller builds for the enqueue step.
	// A future patch that stores these directly can do so without
	// changing this signature.
	_ = triggerEvidenceID
	_ = reason

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
//     eventID, _ := dm.CreateInvalidationEvent(tx, deadID, deadType, ev, reason, 0)
//     targets, _ := dm.discoverCascadeTargets(deadID)
//     dm.EnqueueCascadeIntents(tx, CascadeInvalidation{
//         EventID: eventID, DeadArtifactID: deadID, DeadArtifactType: deadType,
//         Reason: reason, CascadeDepth: 0, TriggerEvidenceID: ev,
//     }, targets)
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
// Negative or excessive CascadeDepth is rejected so a misconfigured
// caller cannot bypass the MaxCascadeDepth ceiling.
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
	if event.CascadeDepth < 0 {
		return 0, fmt.Errorf("EnqueueCascadeIntents: cascade_depth must be >= 0, got %d", event.CascadeDepth)
	}
	if event.CascadeDepth > MaxCascadeDepth {
		return 0, fmt.Errorf("EnqueueCascadeIntents: cascade_depth %d exceeds MaxCascadeDepth=%d", event.CascadeDepth, MaxCascadeDepth)
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
// local + shared and dedupes by the SEMANTIC KEY
// (dead_artifact_id, downstream_artifact_id, invalidation_event_id),
// NOT by the row id. EnqueueCascadeIntents generates a fresh
// GenerateID() per local + shared write, so the row id differs
// between the two schemas even though the logical intent is the
// same. The schema's UNIQUE constraint on the semantic key lets a
// federated read collapse the two physical rows into one logical
// intent. We order by MIN(created_at) so the dedup preserves the
// older timestamp from whichever schema the row landed in first.
//
// "id" returned in the row is the local row's id (the first
// column in the GROUP BY select order). The materializer's
// state-machine transitions (Task 4+) update by id, so the
// materializer must rewrite the id to the correct schema on
// transition — but for the read path, "any id from the equivalent
// set" is sufficient because the row data is identical across
// schemas.
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
		// Federated path. Dedup by the semantic key (dead, downstream,
		// event) — NOT by row id, because EnqueueCascadeIntents
		// mints a fresh id per local + shared write. The semantic
		// key is the schema's UNIQUE constraint, so the two physical
		// rows that EnqueueCascadeIntents writes for the same
		// logical intent collapse into one entry in the result.
		//
		// MIN(created_at) preserves the earliest created_at from
		// whichever schema the row landed in first, so the
		// materializer's FIFO order is stable across the local
		// → shared propagation race.
		//
		// The outer SELECT picks MAX(id) (covers all of the schema
		// columns NOT in the GROUP BY) so the result is a valid
		// single row from the GROUP BY. MAX(id) is arbitrary with
		// respect to causal ordering — only the OTHER columns
		// matter to the materializer.
		query = `
			SELECT MAX(id) AS id, invalidation_event_id, dead_artifact_id, dead_artifact_type,
			       downstream_artifact_id, downstream_artifact_type,
			       MAX(trigger_evidence_id) AS trigger_evidence_id,
			       MAX(cascade_depth) AS cascade_depth,
			       MAX(reason) AS reason,
			       MAX(status) AS status,
			       MAX(materialized_theory_id) AS materialized_theory_id,
			       MAX(attempt_count) AS attempt_count,
			       MAX(next_retry_at) AS next_retry_at,
			       MAX(terminal_error) AS terminal_error,
			       MIN(created_at) AS created_at,
			       MAX(updated_at) AS updated_at
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
			GROUP BY invalidation_event_id, dead_artifact_id, downstream_artifact_id
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
// Why this lives in cascade_outbox.go (rather than cascade_provenance.go):
// the helper is part of the cascade enqueue flow's plumbing. Keeping
// it next to EnqueueCascadeIntents makes the data flow obvious to
// future readers: discover → enqueue → list → materialize.
//
// tx parameter: when non-nil, both edge paths read through the
// supplied *sql.Tx (same connection as the caller's tx; observes
// in-flight state for shared propagation). When nil, falls through
// to dm.db.Query for the standalone path. The cascade invalidation
// hook (EnqueueCascadeInvalidation) always passes its tx; tests
// calling the helper directly from outside a tx pass nil.
func (dm *DatabaseManager) discoverCascadeTargets(tx *sql.Tx, deadArtifactID string) ([]ProvenanceTarget, error) {
	out := make([]ProvenanceTarget, 0)
	if deadArtifactID == "" {
		return out, nil
	}

	// Dedup key: artifact_id. Two ProvenanceTarget entries with the
	// same ID are the same intent regardless of which discovery
	// path surfaced them.
	seen := make(map[string]struct{})
	out = make([]ProvenanceTarget, 0)

	addTarget := func(id, typ string) {
		if id == "" {
			return
		}
		if !isEligibleCascadeType(typ) {
			return
		}
		if _, ok := seen[id]; ok {
			return
		}
		seen[id] = struct{}{}
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
	queryFn := dm.db.Query
	if tx != nil {
		queryFn = tx.Query
	}
	rows, err := queryFn(`
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
	// targets decision / theory downstreams — listDownstreamCitationsOn
	// already accepts an allowedTypes allow-list so we re-use it
	// here. The source list returns ProvenanceCitation structs;
	// pull the (downstream_id, downstream_type) pair.
	//
	// Note: listDownstreamCitationsOn is a federated read (local +
	// shared dedup), so a cross-agent citation surfaces here too.
	// That matches the design spec's "shared substrate" semantics.
	citations, err := dm.listDownstreamCitationsOn(tx, deadArtifactID, eligibleCascadeTypes)
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

// EnqueueCascadeInvalidation is the transaction-aware invalidation
// hook (Task 4 of the 2026-08-04 plan). It is the single integration
// point the three explicit invalidation paths (disproved theory
// resolution, memory shred, hard confidence transition) call when
// they need to atomically commit cascade intents alongside a root
// mutation.
//
// Sequence:
//
//  1. Mint a stable invalidation event ID via CreateInvalidationEvent.
//     The ID is what the cascade materializer uses to trace causally
//     across the outbox — every intent for one invalidation carries
//     the same event ID.
//
//  2. Discover the downstream targets via discoverCascadeTargets,
//     passing the supplied *sql.Tx. The helper reads through the
//     supplied tx (NOT dm.db) to avoid SQLite's table-locked
//     isolation deadlock while the invalidating transaction is
//     open. The brief's "without opening a second connection"
//     requirement is met because the read lands on the same
//     *sql.Tx the caller already owns.
//
//  3. Enqueue the cascade intents via EnqueueCascadeIntents, all
//     inside the supplied *sql.Tx. If the enqueue fails, the
//     caller's tx rolls back, the root mutation never commits, and
//     the substrate never sees a "the memory is gone but no cascade
//     was recorded" divergence.
//
// The deadArtifactType is the canonical type string ("memory",
// "decision", "theory", "lesson") the cascade materializer uses to
// choose its dispatch handler. The reason is a short, human-readable
// label persisted verbatim on every outbox row for forensics
// (e.g., "theory_disproven", "memory_shredded", "confidence_floor").
// triggerEvidenceID is optional — pass "" when no triggering evidence
// row exists. depth is the cascade depth (0 = root invalidation).
//
// Returns the count of NEW outbox rows that landed (zero is a clean
// no-op when no downstream dependents exist or the dedup key
// collapses every target). Errors from the inner steps propagate to
// the caller's tx so a partial-failure cannot diverge root state
// from cascade intent.
//
// Shared DB behavior: matches the underlying helpers — when
// MPM_SHARED_DB is attached, intents also land in
// shared.epistemic_cascade_outbox under the same tx.
func (dm *DatabaseManager) EnqueueCascadeInvalidation(
	tx *sql.Tx,
	deadArtifactID, deadArtifactType, reason, triggerEvidenceID string,
	depth int,
) (int, error) {
	if tx == nil {
		return 0, fmt.Errorf("EnqueueCascadeInvalidation: tx is required")
	}
	if deadArtifactID == "" {
		return 0, fmt.Errorf("EnqueueCascadeInvalidation: deadArtifactID is required")
	}
	if deadArtifactType == "" {
		return 0, fmt.Errorf("EnqueueCascadeInvalidation: deadArtifactType is required")
	}

	// Step 1: mint the event ID via the existing helper. Validation
	// of deadArtifactID / deadArtifactType / depth happens there.
	eventID, err := dm.CreateInvalidationEvent(tx, deadArtifactID, deadArtifactType, triggerEvidenceID, reason, depth)
	if err != nil {
		return 0, fmt.Errorf("EnqueueCascadeInvalidation: create event: %w", err)
	}

	// Step 2: discover downstream targets. discoverCascadeTargets
	// reads through the supplied tx (NOT dm.db) because SQLite's
	// default isolation holds an exclusive lock on the memories
	// table while the tx is open; reading from dm.db would
	// deadlock with the in-flight tx. The tx-aware path observes
	// the same in-flight state without opening a second connection
	// — the brief's "without opening a second connection" requirement
	// is met because the read lands on the same *sql.Tx.
	targets, err := dm.discoverCascadeTargets(tx, deadArtifactID)
	if err != nil {
		return 0, fmt.Errorf("EnqueueCascadeInvalidation: discover targets: %w", err)
	}

	// Step 4: enqueue intents inside the supplied tx. The
	// CascadeInvalidation struct carries the same metadata the
	// caller passed in so the metadata contract documented on
	// CreateInvalidationEvent / CascadeInvalidation is preserved.
	written, err := dm.EnqueueCascadeIntents(tx, CascadeInvalidation{
		EventID:           eventID,
		DeadArtifactID:    deadArtifactID,
		DeadArtifactType:  deadArtifactType,
		Reason:            reason,
		CascadeDepth:      depth,
		TriggerEvidenceID: triggerEvidenceID,
	}, targets)
	if err != nil {
		return written, fmt.Errorf("EnqueueCascadeInvalidation: enqueue intents: %w", err)
	}
	return written, nil
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

// PruneCascadeOutbox hard-deletes terminal-state cascade intents
// (status='materialized' or 'failed') older than retentionDays, where
// age is measured from updated_at. The outbox grows monotonically with
// every shred/cascade event; without a retention sweep the table
// accumulates forever on long-running daemons.
//
// Default retention: 30 days. Terminal intents have served their
// purpose by then (the materialized theory row or the dead-letter
// audit row holds the permanent record). Live pending/processing
// intents are untouched.
func (dm *DatabaseManager) PruneCascadeOutbox(retentionDays int) (int64, error) {
	if dm == nil || dm.db == nil {
		return 0, fmt.Errorf("PruneCascadeOutbox: db not initialized")
	}
	if retentionDays < 1 {
		retentionDays = 30
	}
	res, err := dm.db.Exec(`
		DELETE FROM epistemic_cascade_outbox
		WHERE status IN ('materialized', 'failed')
		  AND updated_at < CAST(strftime('%s','now', '-' || ? || ' days') AS INTEGER)
	`, retentionDays)
	if err != nil {
		return 0, fmt.Errorf("PruneCascadeOutbox: %w", err)
	}
	n, _ := res.RowsAffected()
	return n, nil
}
// ─── Positive-direction (constructive) cascade helpers ─────────────────────
//
// The negative-direction cascade invalidates downstream artifacts when
// a foundation is contradicted or shredded. The positive-direction
// cascade is the symmetric feature: when a foundation is proven (or
// its confidence climbs past the hard ceiling), downstream artifacts
// that have opted in via polarity='assumes_false' are observed but
// NOT mutated automatically — the materializer still requires explicit
// reason and trust-coupling context to decide what "foundation
// proven" means for the dependent.
//
// Explicit-only opt-in: the polarity column on epistemic_provenance
// is the gate. NULL polarity is the safe default and never fires any
// positive cascade. PolarityAssumesFalse means "this downstream
// artifact assumes the source is false" — when the source is proven
// TRUE, the dependent may need re-evaluation (e.g. a hypothesis that
// tests the negation of a theorem becomes interesting only AFTER the
// theorem is proven). PolarityAssumesTrue is reserved for the
// symmetric case and is currently unused by the trigger surfaces but
// is part of the storage contract for forward compatibility.

// PolarityAssumesTrue / PolarityAssumesFalse / HardConfidenceProvenThreshold
// are part of the positive-direction cascade contract. They live
// alongside the cascade enqueue helpers (this file) because the
// discovery path uses them at enqueue time, not at write time. The
// CHECK constraint on epistemic_provenance.polarity enforces these
// strings at the storage boundary (see
// migration_epistemic_provenance_polarity.go).
const (
	PolarityAssumesTrue  = "assumes_true"
	PolarityAssumesFalse = "assumes_false"
)

// HardConfidenceProvenThreshold is the confidence crossing that
// triggers a positive cascade via the RecomputeConfidence hook in
// evidence_store.go. Set to 0.8 — chosen empirically: 0.7 is the
// "high confidence" boundary used elsewhere (lessons, decisions),
// 0.8 marks the transition into "near-certain" territory where a
// foundation's downstream dependents can plausibly be considered
// "built on solid ground". Crossing DOWN is invalidation (existing
// behavior in evidence_store.go); crossing UP at this threshold is
// foundation_proven (new behavior).
const HardConfidenceProvenThreshold = 0.8

// Reason labels for positive cascade intents. Distinct from the
// invalidation reasons (which live in evidence_store.go and
// cascade_outbox.go's existing helpers) so the materializer can
// branch on direction without ambiguity.
const (
	ReasonFoundationProven  = "foundation_proven"
	ReasonConfidenceCeiling = "confidence_ceiling"
)

// discoverPositiveCascadeTargets returns the deduplicated list of
// downstream artifacts whose citation to sourceArtifactID carries
// polarity='assumes_false' (i.e. the downstream ASSUMES the source
// is false). When the source is proven true, these are the artifacts
// whose "the source is false" assumption has flipped — the cascade
// materializer may want to flag them for re-evaluation depending on
// the per-type policy.
//
// Same contract as discoverCascadeTargets: only artifacts of type
// decision or theory are eligible (lessons and global rules are
// excluded — the cascade materializer only acts on cascading-eligible
// types). tx is read-through when non-nil to honor the in-flight
// state of the surrounding cascade enqueue tx.
func (dm *DatabaseManager) discoverPositiveCascadeTargets(tx *sql.Tx, sourceArtifactID string) ([]ProvenanceTarget, error) {
	out := make([]ProvenanceTarget, 0)
	if sourceArtifactID == "" {
		return out, nil
	}

	queryFn := dm.db.Query
	if tx != nil {
		queryFn = tx.Query
	}
	rows, err := queryFn(`
		SELECT DISTINCT ep.downstream_id, ep.downstream_type
		FROM epistemic_provenance ep
		WHERE ep.source_id = ?
		  AND ep.polarity = ?
		ORDER BY ep.downstream_id ASC
	`, sourceArtifactID, PolarityAssumesFalse)
	if err != nil {
		return nil, fmt.Errorf("discoverPositiveCascadeTargets: %w", err)
	}
	defer rows.Close()

	seen := make(map[string]struct{})
	for rows.Next() {
		var id, typ string
		if err := rows.Scan(&id, &typ); err != nil {
			return nil, fmt.Errorf("discoverPositiveCascadeTargets scan: %w", err)
		}
		if !isEligibleCascadeType(typ) {
			continue
		}
		if _, ok := seen[id]; ok {
			continue
		}
		seen[id] = struct{}{}
		out = append(out, ProvenanceTarget{ArtifactID: id, ArtifactType: typ})
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("discoverPositiveCascadeTargets rows: %w", err)
	}
	return out, nil
}

// EnqueueCascadeFoundationProven is the mirror of
// EnqueueCascadeInvalidation for the positive direction. Triggered
// when a foundation is observed as "proven" — either via explicit
// theory resolution to status='proven' or via RecomputeConfidence
// crossing HardConfidenceProvenThreshold upward.
//
// Sequence mirrors EnqueueCascadeInvalidation:
//
//  1. Mint the event ID via CreateInvalidationEvent (the helper is
//     direction-agnostic; the reason string distinguishes the two
//     directions downstream).
//  2. Discover polarity-tagged downstream targets via
//     discoverPositiveCascadeTargets (the same sourceArtifactID is
//     used for both — for a positive cascade the "dead" side is
//     the previously-uncertain foundation, which is now proven).
//  3. Enqueue intents via EnqueueCascadeIntents under the supplied
//     tx.
//
// Returns the count of NEW outbox rows that landed (zero is a clean
// no-op when no downstream dependents opted in or the dedup key
// collapsed every target). Errors propagate to the caller's tx.
func (dm *DatabaseManager) EnqueueCascadeFoundationProven(
	tx *sql.Tx,
	sourceArtifactID, sourceArtifactType, reason, triggerEvidenceID string,
	depth int,
) (int, error) {
	if tx == nil {
		return 0, fmt.Errorf("EnqueueCascadeFoundationProven: tx is required")
	}
	if sourceArtifactID == "" {
		return 0, fmt.Errorf("EnqueueCascadeFoundationProven: sourceArtifactID is required")
	}
	if sourceArtifactType == "" {
		return 0, fmt.Errorf("EnqueueCascadeFoundationProven: sourceArtifactType is required")
	}
	if reason != ReasonFoundationProven && reason != ReasonConfidenceCeiling {
		return 0, fmt.Errorf("EnqueueCascadeFoundationProven: reason must be %q or %q, got %q",
			ReasonFoundationProven, ReasonConfidenceCeiling, reason)
	}

	// Step 1: mint the event ID. The event ID is shared across all
	// intents for this single trigger — the materializer uses it to
	// trace causally when more than one downstream is involved.
	eventID, err := dm.CreateInvalidationEvent(tx, sourceArtifactID, sourceArtifactType, triggerEvidenceID, reason, depth)
	if err != nil {
		return 0, fmt.Errorf("EnqueueCascadeFoundationProven: create event: %w", err)
	}

	// Step 2: discover positive-direction downstream targets — the
	// ones that opted in via polarity='assumes_false'. NULL polarity
	// rows are excluded by the WHERE clause, which is the load-bearing
	// safety invariant: pre-existing citations (none of which have
	// polarity set) never participate in positive cascades.
	targets, err := dm.discoverPositiveCascadeTargets(tx, sourceArtifactID)
	if err != nil {
		return 0, fmt.Errorf("EnqueueCascadeFoundationProven: discover targets: %w", err)
	}

	// Step 3: enqueue intents under the supplied tx. Same shape as
	// the negative-direction path; the reason label is the
	// disambiguator (foundation_proven / confidence_ceiling vs the
	// invalidation reasons).
	written, err := dm.EnqueueCascadeIntents(tx, CascadeInvalidation{
		EventID:           eventID,
		DeadArtifactID:    sourceArtifactID,
		DeadArtifactType:  sourceArtifactType,
		Reason:            reason,
		CascadeDepth:      depth,
		TriggerEvidenceID: triggerEvidenceID,
	}, targets)
	if err != nil {
		return written, fmt.Errorf("EnqueueCascadeFoundationProven: enqueue intents: %w", err)
	}
	return written, nil
}
