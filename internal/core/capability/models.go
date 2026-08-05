package capability

import (
	"database/sql/driver"
	"encoding/json"
	"fmt"
)

// Capability is the canonical row representation. One row per
// proposal/revision; the live version is identified by
// state='active' AND superseded_by_id IS NULL.
type Capability struct {
	ID             string          `db:"id"`
	Name           string          `db:"name"`
	Purpose        string          `db:"purpose"`
	SourceCode     string          `db:"source_code"`
	SourceLanguage string          `db:"source_language"`
	SourceHash     string          `db:"source_hash"`

	State           CapabilityState `db:"state"`
	ExecutionDomain ExecutionDomain `db:"execution_domain"`
	StateChangedAt  int64           `db:"state_changed_at"`

	// Lineage (§3.5 / §3.6 of the spec)
	AuthorTheoryID *string `db:"author_theory_id"`
	AuthorAgent    string  `db:"author_agent"`
	CreatedFromID  *string `db:"created_from_id"`
	SupersededByID *string `db:"superseded_by_id"`

	// Empirical metrics (§2.1 of the spec)
	SuccessCount      int     `db:"success_count"`
	FailureCount      int     `db:"failure_count"`
	FractureCount     int     `db:"fracture_count"`
	LastInvokedAt     *int64  `db:"last_invoked_at"`
	LastFailureAt     *int64  `db:"last_failure_at"`
	LastFailureStderr *string `db:"last_failure_stderr"`
	AvgLatencyMs      float64 `db:"avg_latency_ms"`

	// Probation policy (stamped at proposal; not magic constants)
	ProbationRequiredSuccessCount int     `db:"probation_required_success_count"`
	ProbationMaxFailureRate       float64 `db:"probation_max_failure_rate"`
	PromotedAt                    *int64  `db:"promoted_at"`

	// Discovery (FTS5 surface + cosine dedup)
	Embedding []byte      `db:"embedding"`
	Tags      StringSlice `db:"tags"`

	// Standard audit fields
	CreatedAt int64             `db:"created_at"`
	UpdatedAt int64             `db:"updated_at"`
	DeletedAt *int64            `db:"deleted_at"`
	Metadata  CapabilityMetadata `db:"metadata"`
}

// CapabilityMetadata is the JSON blob stored in capabilities.metadata.
// The Scanner/Valuer interface pushes the marshal/unmarshal down to the
// database driver layer — handlers never see byte slices or json.RawMessage.
type CapabilityMetadata map[string]interface{}

// Scan implements sql.Scanner. Accepts []byte (mattn/go-sqlite3 default),
// string (manual scan paths / tests), or nil.
func (m *CapabilityMetadata) Scan(src interface{}) error {
	if src == nil {
		*m = nil
		return nil
	}
	var data []byte
	switch v := src.(type) {
	case []byte:
		data = v
	case string:
		data = []byte(v)
	default:
		return fmt.Errorf("capability: metadata: unsupported scan type %T", src)
	}
	if len(data) == 0 {
		*m = nil
		return nil
	}
	return json.Unmarshal(data, m)
}

// Value implements driver.Valuer. Empty maps marshal to `{}` (not NULL)
// so the column's NOT NULL DEFAULT '{}' constraint is satisfied when
// the caller passes no metadata. Matches the column's expected
// representation in every storage layer.
func (m CapabilityMetadata) Value() (driver.Value, error) {
	if len(m) == 0 {
		return []byte("{}"), nil
	}
	return json.Marshal(m)
}

// StringSlice is a JSON-encoded []string. Used for the `tags` column
// and any future list-typed columns. Same rationale as CapabilityMetadata.
type StringSlice []string

// Scan implements sql.Scanner.
func (s *StringSlice) Scan(src interface{}) error {
	if src == nil {
		*s = nil
		return nil
	}
	var data []byte
	switch v := src.(type) {
	case []byte:
		data = v
	case string:
		data = []byte(v)
	default:
		return fmt.Errorf("capability: string slice: unsupported scan type %T", src)
	}
	if len(data) == 0 {
		*s = nil
		return nil
	}
	return json.Unmarshal(data, s)
}

// Value implements driver.Valuer. Empty slices marshal to `[]` (not
// NULL) so the column's NOT NULL DEFAULT '[]' constraint is satisfied
// when the caller passes no tags. Matches the column's expected
// representation in every storage layer.
func (s StringSlice) Value() (driver.Value, error) {
	if len(s) == 0 {
		return []byte("[]"), nil
	}
	return json.Marshal(s)
}

// Invocation is one row in capability_invocations. Pure execution
// telemetry — no synthetic events. See §2.2 of the spec.
//
// The hard separation between this table and capability_events keeps
// the metrics aggregation pipeline free of synthetic-state-change
// filtering.
type Invocation struct {
	ID                 string  `db:"id"`
	CapabilityID       string  `db:"capability_id"`
	InvokedAt          int64   `db:"invoked_at"`
	ExitCode           int     `db:"exit_code"`
	DurationMs         int64   `db:"duration_ms"`
	Stderr             *string `db:"stderr"`
	InvocationContext  *string `db:"invocation_context"` // JSON; callers handle parse
	CascadeInvalidated bool    `db:"cascade_invalidated"`
}

// TelemetryPayload is the Executor-side write shape for one
// invocation. Distinct from Invocation (the read-model): the
// Executor builds a TelemetryPayload from a successful Invoke,
// Store.RecordInvocation writes it to capability_invocations
// + updates capability counters in one transaction.
//
// The split mirrors the Capability / CapabilityProposal pattern
// already used elsewhere in this package — write side is
// purpose-built for the call site that produces the data; read
// side is the canonical row shape consumed by queries.
//
// Field semantics:
//
//   CapabilityID: foreign key into capabilities.id. The Executor
//     pulls this from req.Capability.ID.
//   Language: bash | python | jq. Recorded so the observability
//     layer can answer "how many python capability invocations
//     failed today" without re-parsing source.
//   StartedAt / FinishedAt: Unix-epoch seconds. The Executor's
//     clock is the source of truth — never time.Now() inline,
//     so tests can inject a frozen clock deterministically.
//   ExitCode: as reported by the process. NOT -1 on driver
//     failure (driver failures short-circuit before this struct
//     is built — see Executor.Invoke).
//   Stdout / Stderr: already-truncated to ResourceLimits.MaxOutputBytes
//     by the time the payload is built. The Store never writes
//     unbounded blobs.
//   Truncated: true if EITHER stdout or stderr hit the cap. Lets
//     queries surface "this invocation lost output" without
//     comparing byte lengths.
//   ArgsHash: sha256 hex of NUL-separated argv IN CALLER ORDER
//     (not sorted — argv order is semantically significant for
//     shell-style invocations; "git checkout -b x" and "git -b
//     checkout x" are different intents).
//   EnvKeys: sorted list of env var NAMES that the caller passed
//     (values are NEVER recorded — secrets stay out of the
//     ledger; keys give the router enough to cluster env-
//     dependent failures without leaking data).
//   DriverName: "bwrap" | "direct" | "fake". Lets queries
//     answer "do failures correlate with a specific driver?"
//     without joining elsewhere.
type TelemetryPayload struct {
	CapabilityID string
	Language     SourceLanguage
	StartedAt    int64
	FinishedAt   int64
	ExitCode     int
	DurationMs   int64
	Stdout       []byte
	Stderr       []byte
	Truncated    bool
	ArgsHash     string
	EnvKeys      []string
	DriverName   string
}

// Dependency is one row in capability_dependencies. Self-referencing
// junction; reverse lookups (`WHERE depends_on_id = ?`) power the
// cascade walkers in §5.
type Dependency struct {
	CapabilityID string `db:"capability_id"`
	DependsOnID  string `db:"depends_on_id"`
	AddedAt      int64  `db:"added_at"`
}

// Event is one row in capability_events. State transitions and
// synthetic events only — NOT real invocations. See §2.4 of the spec.
type Event struct {
	ID           string            `db:"id"`
	CapabilityID string            `db:"capability_id"`
	EventType    EventType         `db:"event_type"`
	OccurredAt   int64             `db:"occurred_at"`
	Actor        string            `db:"actor"`
	FromState    *CapabilityState  `db:"from_state"`
	ToState      *CapabilityState  `db:"to_state"`
	Reason       *string           `db:"reason"`
	RelatedID    *string           `db:"related_id"`
	Metadata     EventMetadata     `db:"metadata"`
}

// EventMetadata is the JSON blob stored in capability_events.metadata.
// Mirrors CapabilityMetadata but kept as a distinct type so future
// divergence (e.g. typed fields for specific event_types) doesn't
// ripple across the schema.
type EventMetadata map[string]interface{}

// Scan implements sql.Scanner.
func (m *EventMetadata) Scan(src interface{}) error {
	if src == nil {
		*m = nil
		return nil
	}
	var data []byte
	switch v := src.(type) {
	case []byte:
		data = v
	case string:
		data = []byte(v)
	default:
		return fmt.Errorf("capability: event metadata: unsupported scan type %T", src)
	}
	if len(data) == 0 {
		*m = nil
		return nil
	}
	return json.Unmarshal(data, m)
}

// Value implements driver.Valuer. Empty maps marshal to `{}` (not
// NULL) so the column's NOT NULL DEFAULT '{}' constraint is satisfied.
func (m EventMetadata) Value() (driver.Value, error) {
	if len(m) == 0 {
		return []byte("{}"), nil
	}
	return json.Marshal(m)
}
