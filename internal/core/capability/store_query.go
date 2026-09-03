package capability

import (
	"database/sql"
	"errors"
	"fmt"
	"strings"
)

// =============================================================================
// store_query.go — read-only Store methods
//
// Reads do not need transactions; every method here calls QueryTracked /
// QueryRowTracked directly. Each method is a single SQL statement so
// there is no atomicity concern at this layer.
//
// All public methods return (result, ErrNotFound) where ErrNotFound is
// the canonical "no row matched" signal — handlers check via errors.Is.
// =============================================================================

// scanColumns is the canonical column list for Capability rows. Centralized
// here so SELECTs and scans stay in lockstep — adding a column means
// updating BOTH the SELECT and the Scan order.
const scanColumns = `id, name, purpose, source_code, source_language, source_hash,
	state, execution_domain, state_changed_at,
	author_theory_id, author_agent, created_from_id, superseded_by_id,
	success_count, failure_count, fracture_count,
	last_invoked_at, last_failure_at, last_failure_stderr, avg_latency_ms,
	probation_required_success_count, probation_max_failure_rate, promoted_at,
	embedding, tags,
	created_at, updated_at, deleted_at, metadata`

// selectCapabilityBase returns the canonical SELECT statement for
// capability rows. Use `?placeholders` substitution if filtering by id.
const selectCapabilityBase = `SELECT ` + scanColumns + ` FROM capabilities`

// scanCapability reads one row into a Capability. The column order
// MUST match scanColumns. Callers are responsible for advancing the
// rows cursor (rows.Next).
func scanCapability(row interface{ Scan(...interface{}) error }) (*Capability, error) {
	var c Capability
	err := row.Scan(
		&c.ID, &c.Name, &c.Purpose, &c.SourceCode, &c.SourceLanguage, &c.SourceHash,
		&c.State, &c.ExecutionDomain, &c.StateChangedAt,
		&c.AuthorTheoryID, &c.AuthorAgent, &c.CreatedFromID, &c.SupersededByID,
		&c.SuccessCount, &c.FailureCount, &c.FractureCount,
		&c.LastInvokedAt, &c.LastFailureAt, &c.LastFailureStderr, &c.AvgLatencyMs,
		&c.ProbationRequiredSuccessCount, &c.ProbationMaxFailureRate, &c.PromotedAt,
		&c.Embedding, &c.Tags,
		&c.CreatedAt, &c.UpdatedAt, &c.DeletedAt, &c.Metadata,
	)
	if err != nil {
		return nil, err
	}
	if err := c.State.Validate(); err != nil {
		return nil, fmt.Errorf("scan: %w", err)
	}
	if err := c.ExecutionDomain.Validate(); err != nil {
		return nil, fmt.Errorf("scan: %w", err)
	}
	return &c, nil
}

// GetCapability returns the capability row with the given id, ignoring
// soft-deleted rows. Returns ErrNotFound if no row matches.
//
// This is the canonical "by id" lookup; for the live (active, not
// superseded) version of a name, use GetLiveVersion.
func (s *Store) GetCapability(id string) (*Capability, error) {
	if strings.TrimSpace(id) == "" {
		return nil, fmt.Errorf("capability: GetCapability: id is required")
	}
	row := s.dm.QueryRowTracked(
		selectCapabilityBase+` WHERE id = ? AND deleted_at IS NULL`, id,
	)
	c, err := scanCapability(row)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, wrapDBError("GetCapability", err)
	}
	return c, nil
}

// GetLiveVersion returns the currently-live capability for the given
// name — that is, the row with state='active' AND superseded_by_id IS NULL.
// Soft-deleted rows are ignored.
//
// Most callers want the live version, not the historical archive of
// retired/forked/revised rows. If multiple rows match (data corruption),
// the most recently created wins (deterministic via ORDER BY created_at DESC).
func (s *Store) GetLiveVersion(name string) (*Capability, error) {
	if strings.TrimSpace(name) == "" {
		return nil, fmt.Errorf("capability: GetLiveVersion: name is required")
	}
	row := s.dm.QueryRowTracked(
		selectCapabilityBase+`
			WHERE name = ? AND state = 'active' AND superseded_by_id IS NULL
			  AND deleted_at IS NULL
			ORDER BY created_at DESC LIMIT 1`,
		name,
	)
	c, err := scanCapability(row)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, wrapDBError("GetLiveVersion", err)
	}
	return c, nil
}

// ListByState returns all non-deleted capabilities currently in the
// given state, newest first. Pagination via limit (capped at 1000;
// callers should narrow by other filters for large corpora).
//
// Use this for `mpm skill list --state=active` style queries.
func (s *Store) ListByState(state CapabilityState, limit int) ([]*Capability, error) {
	if err := state.Validate(); err != nil {
		return nil, fmt.Errorf("capability: ListByState: %w", err)
	}
	if limit <= 0 || limit > 1000 {
		limit = 1000
	}
	rows, err := s.dm.QueryTracked(
		selectCapabilityBase+`
			WHERE state = ? AND deleted_at IS NULL
			ORDER BY created_at DESC LIMIT ?`,
		state, limit,
	)
	if err != nil {
		return nil, wrapDBError("ListByState", err)
	}
	defer rows.Close()

	var out []*Capability
	for rows.Next() {
		c, err := scanCapability(rows)
		if err != nil {
			return nil, wrapDBError("ListByState", err)
		}
		out = append(out, c)
	}
	if err := rows.Err(); err != nil {
		return nil, wrapDBError("ListByState", err)
	}
	return out, nil
}

// GetDependencies returns the capabilities that `id` directly depends on
// (the upstream chain — things `id` calls). Ordered by added_at for
// deterministic cascade behaviour.
func (s *Store) GetDependencies(id string) ([]*Capability, error) {
	if strings.TrimSpace(id) == "" {
		return nil, fmt.Errorf("capability: GetDependencies: id is required")
	}
	rows, err := s.dm.QueryTracked(
		selectCapabilityBase+`
			WHERE id IN (
				SELECT depends_on_id FROM capability_dependencies
				WHERE capability_id = ?
			) AND deleted_at IS NULL
			ORDER BY name ASC`,
		id,
	)
	if err != nil {
		return nil, wrapDBError("GetDependencies", err)
	}
	defer rows.Close()

	var out []*Capability
	for rows.Next() {
		c, err := scanCapability(rows)
		if err != nil {
			return nil, wrapDBError("GetDependencies", err)
		}
		out = append(out, c)
	}
	if err := rows.Err(); err != nil {
		return nil, wrapDBError("GetDependencies", err)
	}
	return out, nil
}

// GetDependents returns the capabilities that directly depend on `id`
// (the downstream chain — callers of `id`). This is the seed set for
// the §5.3 shatter walker; the recursive CTE that follows lives in
// store_cascade.go.
func (s *Store) GetDependents(id string) ([]*Capability, error) {
	if strings.TrimSpace(id) == "" {
		return nil, fmt.Errorf("capability: GetDependents: id is required")
	}
	rows, err := s.dm.QueryTracked(
		selectCapabilityBase+`
			WHERE id IN (
				SELECT capability_id FROM capability_dependencies
				WHERE depends_on_id = ?
			) AND deleted_at IS NULL
			ORDER BY name ASC`,
		id,
	)
	if err != nil {
		return nil, wrapDBError("GetDependents", err)
	}
	defer rows.Close()

	var out []*Capability
	for rows.Next() {
		c, err := scanCapability(rows)
		if err != nil {
			return nil, wrapDBError("GetDependents", err)
		}
		out = append(out, c)
	}
	if err := rows.Err(); err != nil {
		return nil, wrapDBError("GetDependents", err)
	}
	return out, nil
}

// GetInvocationHistory returns up to `limit` invocation records for the
// capability, newest first. Pass sinceUnix=0 to disable the time filter.
// The cascade_invalidated flag is exposed so callers can filter to
// "clean" invocations for metrics (e.g. probation success-rate calc).
func (s *Store) GetInvocationHistory(capabilityID string, sinceUnix int64, limit int) ([]*Invocation, error) {
	if strings.TrimSpace(capabilityID) == "" {
		return nil, fmt.Errorf("capability: GetInvocationHistory: id is required")
	}
	if limit <= 0 || limit > 1000 {
		limit = 1000
	}

	// Build the query dynamically to keep the time filter optional.
	// Parameterised either way — no string interpolation of caller input.
	q := `SELECT id, capability_id, invoked_at, exit_code, duration_ms,
	             stderr, invocation_context, cascade_invalidated
	      FROM capability_invocations
	      WHERE capability_id = ?`
	args := []interface{}{capabilityID}
	if sinceUnix > 0 {
		q += ` AND invoked_at >= ?`
		args = append(args, sinceUnix)
	}
	q += ` ORDER BY invoked_at DESC LIMIT ?`
	args = append(args, limit)

	rows, err := s.dm.QueryTracked(q, args...)
	if err != nil {
		return nil, wrapDBError("GetInvocationHistory", err)
	}
	defer rows.Close()

	var out []*Invocation
	for rows.Next() {
		var inv Invocation
		if err := rows.Scan(
			&inv.ID, &inv.CapabilityID, &inv.InvokedAt, &inv.ExitCode,
			&inv.DurationMs, &inv.Stderr, &inv.InvocationContext,
			&inv.CascadeInvalidated,
		); err != nil {
			return nil, wrapDBError("GetInvocationHistory", err)
		}
		out = append(out, &inv)
	}
	if err := rows.Err(); err != nil {
		return nil, wrapDBError("GetInvocationHistory", err)
	}
	return out, nil
}

// GetEvents returns up to `limit` synthetic event records for the
// capability, newest first. Pass sinceUnix=0 to disable the time filter.
//
// Powers `mpm skill audit <id>`. The synthetic-vs-invocation split is
// strict here: only capability_events rows, never capability_invocations.
func (s *Store) GetEvents(capabilityID string, sinceUnix int64, limit int) ([]*Event, error) {
	if strings.TrimSpace(capabilityID) == "" {
		return nil, fmt.Errorf("capability: GetEvents: id is required")
	}
	if limit <= 0 || limit > 1000 {
		limit = 1000
	}

	q := `SELECT id, capability_id, event_type, occurred_at, actor,
	             from_state, to_state, reason, related_id, metadata
	      FROM capability_events
	      WHERE capability_id = ?`
	args := []interface{}{capabilityID}
	if sinceUnix > 0 {
		q += ` AND occurred_at >= ?`
		args = append(args, sinceUnix)
	}
	q += ` ORDER BY occurred_at DESC LIMIT ?`
	args = append(args, limit)

	rows, err := s.dm.QueryTracked(q, args...)
	if err != nil {
		return nil, wrapDBError("GetEvents", err)
	}
	defer rows.Close()

	var out []*Event
	for rows.Next() {
		var ev Event
		// from_state / to_state are nullable TEXT columns mapped to
		// *CapabilityState. Scanners handle sql.NullString natively
		// because the underlying type is a string-based enum.
		var fromState, toState sql.NullString
		if err := rows.Scan(
			&ev.ID, &ev.CapabilityID, &ev.EventType, &ev.OccurredAt,
			&ev.Actor, &fromState, &toState, &ev.Reason, &ev.RelatedID,
			&ev.Metadata,
		); err != nil {
			return nil, wrapDBError("GetEvents", err)
		}
		if fromState.Valid {
			s := CapabilityState(fromState.String)
			ev.FromState = &s
		}
		if toState.Valid {
			s := CapabilityState(toState.String)
			ev.ToState = &s
		}
		out = append(out, &ev)
	}
	if err := rows.Err(); err != nil {
		return nil, wrapDBError("GetEvents", err)
	}
	return out, nil
}

// Compile-time guard: the Store relies on *internal.DatabaseManager
// satisfying the method shape it consumes. Because the Store takes the
// concrete type directly (not an interface), any signature drift
// surfaces at the first call site rather than here. This comment
// exists so future readers know the contract is enforced by the
// call sites, not by an explicit assertion.

// EmbeddingRef is a lightweight (id, name, embedding-bytes) triple
// returned by ListNonRetiredEmbeddings. The Forge's dedup check
// loads these to compute cosine similarity against a proposed
// purpose — it doesn't need the full Capability row, just enough
// to (a) report the match to the agent and (b) compute similarity.
//
// Embedding is the raw JSON bytes from the column; the Forge
// unmarshals with json.Unmarshal. The store doesn't decode here
// because that's the Forge's job (different consumers may want
// the bytes raw for hashing, etc.).
type EmbeddingRef struct {
	ID              string
	Name            string
	State           CapabilityState
	Embedding       []byte // JSON-encoded []float32
	EmbeddingSource string // 'provider' | 'hash' | 'null'
}

// ListNonRetiredEmbeddings returns the (id, name, embedding) for
// every non-deleted, non-retired capability. Used by the dedup
// check (FG-5) — the Forge needs to compare a proposed purpose's
// embedding against every existing capability's stored embedding.
//
// Retired capabilities are excluded: a retired cap is a historical
// artifact, not a live tool. The fork flow (replaces_id) can revive
// a retired one, but a fresh proposal is not "duplicate" of something
// the operator has already marked dead.
//
// `limit` caps the result set. Default 1024 is enough for the
// current design (capability corpora are small — dozens to low
// hundreds). Larger corpora would need an ANN index; that's
// outside the current scope.
func (s *Store) ListNonRetiredEmbeddings(limit int) ([]*EmbeddingRef, error) {
	if limit <= 0 || limit > 4096 {
		limit = 1024
	}
	rows, err := s.dm.QueryTracked(
		`SELECT id, name, state, embedding, embedding_source
		 FROM capabilities
		 WHERE deleted_at IS NULL
		   AND state != 'retired'
		   AND state != 'rolled_back'
		   AND embedding_source != 'hash'
		 ORDER BY created_at DESC
		 LIMIT ?`,
		limit,
	)
	if err != nil {
		return nil, wrapDBError("ListNonRetiredEmbeddings", err)
	}
	defer rows.Close()

	var out []*EmbeddingRef
	for rows.Next() {
		ref := &EmbeddingRef{}
		var state string
		if err := rows.Scan(&ref.ID, &ref.Name, &state, &ref.Embedding, &ref.EmbeddingSource); err != nil {
			return nil, wrapDBError("ListNonRetiredEmbeddings:scan", err)
		}
		ref.State = CapabilityState(state)
		out = append(out, ref)
	}
	if err := rows.Err(); err != nil {
		return nil, wrapDBError("ListNonRetiredEmbeddings:rows", err)
	}
	return out, nil
}
