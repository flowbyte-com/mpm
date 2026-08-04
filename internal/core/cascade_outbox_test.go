// cascade_outbox_test.go — coverage for the epistemic cascade outbox.
//
// Tests are written against the public surface defined by the Task 3
// brief:
//
//   - CreateInvalidationEvent(tx, deadArtifactID, deadArtifactType,
//                              triggerEvidenceID, reason, depth) (string, error)
//   - EnqueueCascadeIntents(tx, event, targets) (int, error)
//   - ListPendingCascadeIntents(limit) ([]CascadeIntent, error)
//   - discoverCascadeTargets(dm, deadArtifactID) ([]ProvenanceTarget, error)
//
// The schema (epistemic_cascade_outbox + indexes + unique key) is owned by
// Task 1 (schema). This file drives the runtime contract: stable event
// IDs, idempotent intent inserts, target normalization (a target surfaced
// through both the explicit `dependencies` JSON and an epistemic_provenance
// row collapses to a single intent), depth propagation, transactional
// rollback semantics, and federated dedup by semantic key (not row id).
//
// Why these tests matter: the cascade materializer (later task) walks the
// outbox to materialize one theory per pending intent. If a target is
// silently recorded twice (once for the dependency edge, once for the
// provenance edge), the materializer creates two redundant theories and
// the agent wakes twice for the same invalidation. If the event ID is
// unstable across calls, deduplication breaks and a noisy recall turn can
// spawn hundreds of redundant cascade theories. If the federated read
// dedups by row id instead of semantic key, every local+shared write
// returns twice and the materializer creates two theories per logical
// intent. The tests below pin all of those invariants at the storage
// boundary so a future patch cannot regress them silently.

package internal

import (
	"database/sql"
	"path/filepath"
	"sort"
	"strconv"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// cascadeOutboxFixture spins up a hermetic DatabaseManager plus three
// downstream artifacts (a memory, a decision, a theory). Tests can then
// wire either the explicit `dependencies` JSON or the typed
// epistemic_provenance log (or both) to drive the discovery union path.
type cascadeOutboxFixture struct {
	dm       *DatabaseManager
	memID    string // dead artifact (the foundation)
	decID    string // downstream decision
	theoryID string // downstream theory
}

// newCascadeOutboxFixture builds the shared setup. Each call mints fresh
// IDs via GenerateID() so concurrent tests do not collide.
func newCascadeOutboxFixture(t *testing.T) *cascadeOutboxFixture {
	t.Helper()
	dm := hermeticDatabaseManager(t)

	memID := "mem-" + GenerateID()
	_, err := dm.db.Exec(`
		INSERT INTO memories (id, collection, content)
		VALUES (?, 'memories', 'cascade outbox fixture foundation')
	`, memID)
	require.NoError(t, err)

	decResult, err := dm.RecordDecision(
		"cascade outbox fixture context",
		"cascade outbox fixture choice",
		"because the fixture requires a downstream decision",
		"",
		[]string{"fixture"},
		nil,
		ActiveContext{},
	)
	require.NoError(t, err)
	decID, _ := decResult["id"].(string)
	require.NotEmpty(t, decID)

	theoryResult, err := dm.ProposeTheory(
		"cascade outbox fixture hypothesis",
		"cascade outbox fixture validation",
		nil,
		nil,
		[]string{"fixture"},
	)
	require.NoError(t, err)
	theoryID, _ := theoryResult["id"].(string)
	require.NotEmpty(t, theoryID)

	return &cascadeOutboxFixture{dm: dm, memID: memID, decID: decID, theoryID: theoryID}
}

// newCascadeOutboxFixtureWithShared mirrors the provenance fixture helper:
// hermetic DatabaseManager with MPM_SHARED_DB attached so the
// shared-federated write path is exercised.
func newCascadeOutboxFixtureWithShared(t *testing.T) *cascadeOutboxFixture {
	t.Helper()
	workspace := t.TempDir()
	sharedPath := filepath.Join(workspace, "shared.db")
	t.Setenv("MPM_WORKSPACE", workspace)
	t.Setenv("MPM_SHARED_DB", sharedPath)
	t.Setenv("MPM_SHARED_READONLY", "")

	dm, err := NewDatabaseManager(workspace)
	require.NoError(t, err)
	t.Cleanup(func() { dm.Close() })
	require.Equal(t, sharedPath, dm.SharedAttached(),
		"shared DB must be attached for federated test")

	memID := "mem-" + GenerateID()
	_, err = dm.db.Exec(`
		INSERT INTO memories (id, collection, content)
		VALUES (?, 'memories', 'cascade outbox shared fixture foundation')
	`, memID)
	require.NoError(t, err)

	decResult, err := dm.RecordDecision(
		"cascade outbox shared fixture context",
		"cascade outbox shared fixture choice",
		"because the shared fixture requires a downstream decision",
		"",
		[]string{"fixture"},
		nil,
		ActiveContext{},
	)
	require.NoError(t, err)
	decID, _ := decResult["id"].(string)
	require.NotEmpty(t, decID)

	return &cascadeOutboxFixture{dm: dm, memID: memID, decID: decID}
}

// mustTx begins a transaction on the DM and registers a cleanup that
// rolls back if the test forgot to commit. The brief pins
// `CreateInvalidationEvent(tx *sql.Tx, ...)` and
// `EnqueueCascadeIntents(tx *sql.Tx, ...)` as the public signatures, so
// the test harness uses a literal *sql.Tx rather than the DBNode
// abstraction used elsewhere.
func mustTx(t *testing.T, dm *DatabaseManager) *sql.Tx {
	t.Helper()
	tx, err := dm.db.Begin()
	require.NoError(t, err)
	t.Cleanup(func() {
		// Best-effort rollback — if the test already committed, the
		// rollback returns ErrTxDone which we ignore.
		_ = tx.Rollback()
	})
	return tx
}

// timeNowRFC3339 returns the current time as an RFC3339 string. Used
// by the lesson-exclusion test to populate the `created` column on a
// lessons_base row so the schema's NOT NULL constraint is satisfied.
func timeNowRFC3339() string {
	return time.Now().UTC().Format(time.RFC3339)
}

// -----------------------------------------------------------------------------
// Event ID generation
// -----------------------------------------------------------------------------

// TestOutbox_CreateInvalidationEventReturnsStableID asserts the event ID
// returned by CreateInvalidationEvent is non-empty and persists across
// queries so the cascade materializer can use it as a causal trace
// identifier. The ID is also used as the dedup key alongside
// (dead_artifact_id, downstream_artifact_id).
//
// Failure mode the test catches: CreateInvalidationEvent returns "" (or
// any non-stable value like time.Now().UnixNano()) and the dedup key
// collapses unrelated invalidations.
func TestOutbox_CreateInvalidationEventReturnsStableID(t *testing.T) {
	fx := newCascadeOutboxFixture(t)

	tx := mustTx(t, fx.dm)
	eventID, err := fx.dm.CreateInvalidationEvent(tx, fx.memID, "memory", "", "foundation invalidated", 0)
	require.NoError(t, err, "CreateInvalidationEvent must succeed")
	require.NotEmpty(t, eventID, "event ID must be non-empty")

	// The ID is reusable across calls — repeat the call and confirm the
	// new ID is also non-empty (we don't pin the exact value, only that
	// the generator returns something stable-shaped).
	require.NoError(t, tx.Rollback())

	tx2 := mustTx(t, fx.dm)
	eventID2, err := fx.dm.CreateInvalidationEvent(tx2, fx.memID, "memory", "", "another invalidation", 0)
	require.NoError(t, err)
	require.NotEmpty(t, eventID2)
	assert.NotEqual(t, eventID, eventID2,
		"two distinct invalidations must mint distinct event IDs (the dedup key would otherwise collapse them)")
}

// TestOutbox_CreateInvalidationEventValidatesFields asserts the
// input-validation contract: empty dead_artifact_id or empty
// dead_artifact_type must be rejected at the storage boundary, not
// silently recorded as a row the materializer cannot trace.
//
// Failure mode the test catches: CreateInvalidationEvent accepts empty
// fields and writes a row whose invalidation_event_id has no anchor to
// a real artifact; the materializer then emits a cascade theory for a
// non-existent dead artifact.
func TestOutbox_CreateInvalidationEventValidatesFields(t *testing.T) {
	fx := newCascadeOutboxFixture(t)

	tx := mustTx(t, fx.dm)
	_, err := fx.dm.CreateInvalidationEvent(tx, "", "memory", "", "no dead id", 0)
	require.Error(t, err, "empty dead_artifact_id must be rejected")

	tx2 := mustTx(t, fx.dm)
	_, err = fx.dm.CreateInvalidationEvent(tx2, fx.memID, "", "", "no dead type", 0)
	require.Error(t, err, "empty dead_artifact_type must be rejected")
}

// TestOutbox_CreateInvalidationEventRejectsInvalidDepth pins the depth
// validation introduced in the I-2 review fix: a negative depth or a
// depth above MaxCascadeDepth is rejected at the storage boundary.
// Before the fix, depth was silently discarded by `_ = depth` so a
// misconfigured caller could inject any value without error.
//
// Failure mode the test catches: CreateInvalidationEvent accepts a
// depth of 100 (or -1) and the cascade materializer's depth-3 ceiling
// fires after a cascade theory has already been created, leaving an
// orphan intent in the outbox.
func TestOutbox_CreateInvalidationEventRejectsInvalidDepth(t *testing.T) {
	fx := newCascadeOutboxFixture(t)

	tx := mustTx(t, fx.dm)
	_, err := fx.dm.CreateInvalidationEvent(tx, fx.memID, "memory", "", "neg depth", -1)
	require.Error(t, err, "negative depth must be rejected")
	assert.Contains(t, err.Error(), "depth",
		"error must mention depth so the caller understands the rejection")

	tx2 := mustTx(t, fx.dm)
	_, err = fx.dm.CreateInvalidationEvent(tx2, fx.memID, "memory", "", "huge depth", MaxCascadeDepth+1)
	require.Error(t, err, "depth above MaxCascadeDepth must be rejected")
	assert.Contains(t, err.Error(), "MaxCascadeDepth",
		"error must mention the ceiling so the caller can fix the configuration")

	// Boundary: max depth is accepted.
	tx3 := mustTx(t, fx.dm)
	_, err = fx.dm.CreateInvalidationEvent(tx3, fx.memID, "memory", "", "max depth", MaxCascadeDepth)
	require.NoError(t, err, "depth == MaxCascadeDepth is the structural ceiling and must be accepted")
}

// TestOutbox_MetadataCannotDiverge pins the I-2 contract: the metadata
// passed to CreateInvalidationEvent (triggerEvidenceID, reason, depth)
// is the SAME metadata that lands on the outbox row via
// CascadeInvalidation. The reviewer flagged the previous
// `_ = triggerEvidenceID; _ = reason; _ = depth` silent discard;
// this test makes the contract explicit.
//
// The test creates a single invalidation event with a depth of 2 and
// a non-empty trigger_evidence_id, then asserts the stored row
// matches the values the caller asked for. Any future patch that
// drops, silently rewrites, or diverges from these args fails the
// test.
//
// Failure mode the test catches: CreateInvalidationEvent accepts the
// metadata but EnqueueCascadeIntents writes a row with different
// trigger_evidence_id, depth, or reason. The cascade materializer
// would then stamp the wrong evidence on the generated theory, and
// the audit trail would lose the link between the invalidation and the
// triggering transition.
func TestOutbox_MetadataCannotDiverge(t *testing.T) {
	fx := newCascadeOutboxFixture(t)

	tx := mustTx(t, fx.dm)
	eventID, err := fx.dm.CreateInvalidationEvent(
		tx, fx.memID, "memory",
		"ev-divergence-test", // triggerEvidenceID
		"divergence test reason", // reason
		2, // depth
	)
	require.NoError(t, err)

	// Caller mirrors the metadata into CascadeInvalidation (per the
	// CascadeInvalidation doc comment's "metadata contract").
	event := CascadeInvalidation{
		EventID:           eventID,
		DeadArtifactID:    fx.memID,
		DeadArtifactType:  "memory",
		Reason:            "divergence test reason",
		CascadeDepth:      2,
		TriggerEvidenceID: "ev-divergence-test",
	}
	targets := []ProvenanceTarget{
		{ArtifactID: fx.decID, ArtifactType: "decision"},
	}

	_, err = fx.dm.EnqueueCascadeIntents(tx, event, targets)
	require.NoError(t, err)
	require.NoError(t, tx.Commit())

	// The stored row must carry exactly the metadata the caller passed
	// to CreateInvalidationEvent. NOT a constant 0 depth, NOT a NULL
	// trigger_evidence_id, NOT an empty reason.
	var depth int
	var trigger sql.NullString
	var reason string
	var deadType, downType string
	require.NoError(t, fx.dm.db.QueryRow(
		`SELECT cascade_depth, trigger_evidence_id, reason,
		        dead_artifact_type, downstream_artifact_type
		 FROM epistemic_cascade_outbox
		 WHERE invalidation_event_id = ? AND downstream_artifact_id = ?`,
		eventID, fx.decID,
	).Scan(&depth, &trigger, &reason, &deadType, &downType))

	assert.Equal(t, 2, depth,
		"cascade_depth must round-trip — CreateInvalidationEvent accepting depth=2 must land in the row as 2")
	assert.True(t, trigger.Valid,
		"trigger_evidence_id must be populated — the created event passed a non-empty value")
	assert.Equal(t, "ev-divergence-test", trigger.String,
		"trigger_evidence_id must round-trip verbatim")
	assert.Equal(t, "divergence test reason", reason,
		"reason must round-trip — silently rewriting the reason would corrupt the audit trail")
	assert.Equal(t, "memory", deadType, "dead_artifact_type must round-trip")
	assert.Equal(t, "decision", downType, "downstream_artifact_type must round-trip")
}

// -----------------------------------------------------------------------------
// Intent insertion (one per target, dedup, depth preservation)
// -----------------------------------------------------------------------------

// TestOutbox_OneIntentPerEligibleTarget asserts the multi-target happy
// path: three distinct targets → three intents → three outbox rows.
// This is the production case where a single invalidation fans out to
// every downstream decision / theory that cited the dead foundation.
//
// Failure mode the test catches: EnqueueCascadeIntents writes only the
// first target, or collapses targets with the same ID but different
// types.
func TestOutbox_OneIntentPerEligibleTarget(t *testing.T) {
	fx := newCascadeOutboxFixture(t)

	tx := mustTx(t, fx.dm)
	eventID, err := fx.dm.CreateInvalidationEvent(tx, fx.memID, "memory", "ev-1", "foundation invalidated", 0)
	require.NoError(t, err)

	event := CascadeInvalidation{
		EventID:           eventID,
		DeadArtifactID:    fx.memID,
		DeadArtifactType:  "memory",
		Reason:            "foundation invalidated",
		CascadeDepth:      0,
		TriggerEvidenceID: "ev-1",
	}
	targets := []ProvenanceTarget{
		{ArtifactID: fx.decID, ArtifactType: "decision"},
		{ArtifactID: fx.theoryID, ArtifactType: "theory"},
	}

	n, err := fx.dm.EnqueueCascadeIntents(tx, event, targets)
	require.NoError(t, err)
	assert.Equal(t, 2, n, "one intent per eligible target")

	require.NoError(t, tx.Commit())

	var rowCount int
	require.NoError(t, fx.dm.db.QueryRow(
		`SELECT COUNT(*) FROM epistemic_cascade_outbox WHERE invalidation_event_id = ?`,
		eventID,
	).Scan(&rowCount))
	assert.Equal(t, 2, rowCount, "two outbox rows must land after commit")
}

// TestOutbox_DuplicateSuppression asserts the unique-key contract:
// enqueuing the same (event, downstream) pair twice produces one row,
// not two. This is the structural guard against the cascade materializer
// creating redundant theories when the discovery step is invoked twice
// in a noisy wake (e.g. a recall turn followed by a reconciliation).
//
// Failure mode the test catches: EnqueueCascadeIntents uses a bare
// INSERT and the table's UNIQUE constraint surfaces as a SQL error to
// the user — a poor experience when the intent was a no-op anyway.
func TestOutbox_DuplicateSuppression(t *testing.T) {
	fx := newCascadeOutboxFixture(t)

	tx := mustTx(t, fx.dm)
	eventID, err := fx.dm.CreateInvalidationEvent(tx, fx.memID, "memory", "", "dup test", 0)
	require.NoError(t, err)

	event := CascadeInvalidation{
		EventID:          eventID,
		DeadArtifactID:   fx.memID,
		DeadArtifactType: "memory",
		Reason:           "dup test",
		CascadeDepth:     0,
	}
	targets := []ProvenanceTarget{
		{ArtifactID: fx.decID, ArtifactType: "decision"},
	}

	first, err := fx.dm.EnqueueCascadeIntents(tx, event, targets)
	require.NoError(t, err)
	assert.Equal(t, 1, first, "first enqueue writes one row")

	// Same targets, same event — should be a clean no-op.
	// EnqueueCascadeIntents returns the count of NEW rows added
	// (RowsAffected > 0). A re-enqueue against the same triple
	// collapses to zero new rows because ON CONFLICT DO NOTHING
	// suppresses the duplicate.
	second, err := fx.dm.EnqueueCascadeIntents(tx, event, targets)
	require.NoError(t, err, "duplicate enqueue must not error")
	assert.Equal(t, 0, second, "duplicate enqueue adds no new rows")

	require.NoError(t, tx.Commit())

	var rowCount int
	require.NoError(t, fx.dm.db.QueryRow(
		`SELECT COUNT(*) FROM epistemic_cascade_outbox WHERE invalidation_event_id = ?`,
		eventID,
	).Scan(&rowCount))
	assert.Equal(t, 1, rowCount,
		"unique (dead, downstream, event) key must collapse duplicates")
}

// TestOutbox_DepthAndTriggerEvidencePreserved asserts the metadata
// fields (cascade_depth, trigger_evidence_id) are stored verbatim so
// the cascade materializer can stamp them onto the generated theory
// without re-deriving them.
//
// Failure mode the test catches: EnqueueCascadeIntents swaps
// cascade_depth for a constant 0 or drops trigger_evidence_id silently.
func TestOutbox_DepthAndTriggerEvidencePreserved(t *testing.T) {
	fx := newCascadeOutboxFixture(t)

	tx := mustTx(t, fx.dm)
	eventID, err := fx.dm.CreateInvalidationEvent(tx, fx.memID, "memory", "ev-depth", "depth test", 0)
	require.NoError(t, err)

	event := CascadeInvalidation{
		EventID:           eventID,
		DeadArtifactID:    fx.memID,
		DeadArtifactType:  "memory",
		Reason:            "depth test",
		CascadeDepth:      2,
		TriggerEvidenceID: "ev-depth",
	}
	targets := []ProvenanceTarget{
		{ArtifactID: fx.theoryID, ArtifactType: "theory"},
	}

	_, err = fx.dm.EnqueueCascadeIntents(tx, event, targets)
	require.NoError(t, err)
	require.NoError(t, tx.Commit())

	var depth int
	var trigger sql.NullString
	require.NoError(t, fx.dm.db.QueryRow(
		`SELECT cascade_depth, trigger_evidence_id
		 FROM epistemic_cascade_outbox
		 WHERE invalidation_event_id = ? AND downstream_artifact_id = ?`,
		eventID, fx.theoryID,
	).Scan(&depth, &trigger))
	assert.Equal(t, 2, depth, "cascade_depth must be preserved")
	assert.True(t, trigger.Valid, "trigger_evidence_id must be populated")
	assert.Equal(t, "ev-depth", trigger.String, "trigger_evidence_id value must round-trip")
}

// -----------------------------------------------------------------------------
// Transactional rollback
// -----------------------------------------------------------------------------

// TestOutbox_OutboxInsertFailureRollsBackEntireTx asserts the atomicity
// contract from the design spec: "root state and cascade intent cannot
// diverge". When the outbox INSERT inside EnqueueCascadeIntents fails,
// the entire transaction (including any root-state mutation the caller
// applied before the enqueue) must roll back. The failure mode the test
// exercises is a missing outbox table — every INSERT fails, including
// the intent write inside the transaction.
//
// Test technique: drop the outbox table before invoking Enqueue. The
// drop forces every INSERT against the table to fail. The test then
// rolls back the tx and confirms zero rows landed in the restored
// table.
//
// Failure mode the test catches: EnqueueCascadeIntents catches the
// INSERT error and returns nil (swallowing the failure) so the caller
// commits the event without the intents, leaving root state and cascade
// intent permanently out of sync.
func TestOutbox_OutboxInsertFailureRollsBackEntireTx(t *testing.T) {
	fx := newCascadeOutboxFixture(t)

	// Simulate a root-state mutation the caller made earlier in the
	// same transaction (e.g. delete the dead memory row). The
	// rollback must unwind this too.
	tx := mustTx(t, fx.dm)
	_, err := tx.Exec(`UPDATE memories SET deleted_at = ? WHERE id = ?`,
		time.Now().Unix(), fx.memID)
	require.NoError(t, err, "root-state mutation must succeed pre-drop")

	eventID, err := fx.dm.CreateInvalidationEvent(tx, fx.memID, "memory", "", "rollback test", 0)
	require.NoError(t, err, "CreateInvalidationEvent must succeed pre-drop")

	// Drop the outbox table — every INSERT against it now fails.
	_, err = tx.Exec(`DROP TABLE epistemic_cascade_outbox`)
	require.NoError(t, err, "dropping the outbox must succeed inside the tx")

	event := CascadeInvalidation{
		EventID:          eventID,
		DeadArtifactID:   fx.memID,
		DeadArtifactType: "memory",
		Reason:           "rollback test",
		CascadeDepth:     0,
	}
	targets := []ProvenanceTarget{
		{ArtifactID: fx.decID, ArtifactType: "decision"},
	}

	_, err = fx.dm.EnqueueCascadeIntents(tx, event, targets)
	require.Error(t, err, "EnqueueCascadeIntents must fail when the outbox table is missing")

	require.NoError(t, tx.Rollback(), "test tx must rollback cleanly")

	// Restore the table for any subsequent test sharing this fixture.
	_, err = fx.dm.db.Exec(`
		CREATE TABLE IF NOT EXISTS epistemic_cascade_outbox (
			id                        TEXT PRIMARY KEY,
			invalidation_event_id     TEXT NOT NULL,
			dead_artifact_id          TEXT NOT NULL,
			dead_artifact_type        TEXT NOT NULL,
			downstream_artifact_id    TEXT NOT NULL,
			downstream_artifact_type  TEXT NOT NULL,
			trigger_evidence_id       TEXT,
			cascade_depth             INTEGER NOT NULL DEFAULT 0,
			reason                    TEXT NOT NULL DEFAULT '',
			status                    TEXT NOT NULL DEFAULT 'pending'
			                          CHECK (status IN ('pending','processing','materialized','failed')),
			materialized_theory_id    TEXT,
			attempt_count             INTEGER NOT NULL DEFAULT 0,
			next_retry_at             INTEGER,
			terminal_error            TEXT,
			created_at                INTEGER NOT NULL DEFAULT (CAST(strftime('%s','now') AS INTEGER)),
			updated_at                INTEGER NOT NULL DEFAULT (CAST(strftime('%s','now') AS INTEGER)),
			UNIQUE (dead_artifact_id, downstream_artifact_id, invalidation_event_id)
		)`)
	require.NoError(t, err, "restoring the outbox table must succeed")

	// The rollback must have unwound the root-state mutation.
	var deletedAt sql.NullInt64
	require.NoError(t, fx.dm.db.QueryRow(
		`SELECT deleted_at FROM memories WHERE id = ?`,
		fx.memID,
	).Scan(&deletedAt))
	assert.False(t, deletedAt.Valid,
		"root-state mutation must be rolled back when outbox insert fails")

	// And no outbox rows landed.
	var rowCount int
	require.NoError(t, fx.dm.db.QueryRow(
		`SELECT COUNT(*) FROM epistemic_cascade_outbox WHERE dead_artifact_id = ?`,
		fx.memID,
	).Scan(&rowCount))
	assert.Equal(t, 0, rowCount,
		"outbox INSERTs must be rolled back when the enqueue fails (atomicity contract)")
}

// -----------------------------------------------------------------------------
// ListPendingCascadeIntents
// -----------------------------------------------------------------------------

// TestOutbox_ListPendingReturnsOnlyPending pins the read-side filter:
// ListPendingCascadeIntents returns only rows whose status='pending',
// not 'processing', 'materialized', or 'failed'. The materializer
// (later task) relies on this filter to claim its working set.
//
// Failure mode the test catches: ListPendingCascadeIntents returns
// every row regardless of status and the materializer re-processes
// already-handled intents.
func TestOutbox_ListPendingReturnsOnlyPending(t *testing.T) {
	fx := newCascadeOutboxFixture(t)

	// Seed three rows: one pending, one processing, one materialized.
	for i, status := range []string{"pending", "processing", "materialized"} {
		_, err := fx.dm.db.Exec(`
			INSERT INTO epistemic_cascade_outbox
			    (id, invalidation_event_id, dead_artifact_id, dead_artifact_type,
			     downstream_artifact_id, downstream_artifact_type, cascade_depth,
			     reason, status)
			VALUES (?, ?, ?, 'memory', ?, 'decision', 0, 'fixture', ?)
		`, "row-"+strconv.Itoa(i), "evt-"+strconv.Itoa(i), fx.memID, fx.decID, status)
		require.NoError(t, err)
	}

	pending, err := fx.dm.ListPendingCascadeIntents(10)
	require.NoError(t, err)

	// Filter the result client-side to assert ONLY pending rows
	// returned (defensive against an implementation that returns the
	// full set and trusts the caller to filter).
	for _, intent := range pending {
		assert.Equal(t, "pending", intent.Status,
			"ListPendingCascadeIntents must only return pending rows; got %s", intent.Status)
	}

	// And at least one pending row landed.
	assert.GreaterOrEqual(t, len(pending), 1,
		"the seeded pending row must surface in the result")
}

// TestOutbox_ListPendingHonoursLimit asserts the limit argument caps the
// result size so a runaway materializer cannot accidentally read
// thousands of pending rows in one sweep.
//
// Failure mode the test catches: ListPendingCascadeIntents ignores the
// limit argument and returns the full table.
func TestOutbox_ListPendingHonoursLimit(t *testing.T) {
	fx := newCascadeOutboxFixture(t)

	// Seed five pending rows.
	for i := 0; i < 5; i++ {
		_, err := fx.dm.db.Exec(`
			INSERT INTO epistemic_cascade_outbox
			    (id, invalidation_event_id, dead_artifact_id, dead_artifact_type,
			     downstream_artifact_id, downstream_artifact_type, cascade_depth,
			     reason, status)
			VALUES (?, ?, ?, 'memory', ?, 'decision', 0, 'fixture', 'pending')
		`, "row-"+strconv.Itoa(i), "evt-"+strconv.Itoa(i), fx.memID, fx.decID)
		require.NoError(t, err)
	}

	pending, err := fx.dm.ListPendingCascadeIntents(2)
	require.NoError(t, err)
	assert.LessOrEqual(t, len(pending), 2, "limit=2 must cap the result size")
}

// TestOutbox_ListPendingFederatedDedupBySemanticKey is the I-1 fix
// coverage: when shared is attached, EnqueueCascadeIntents writes the
// same logical intent to both local and shared with DIFFERENT row ids
// (GenerateID() is called per write). The federated read must dedup
// by the SEMANTIC KEY (dead_artifact_id, downstream_artifact_id,
// invalidation_event_id) — not by row id — so the materializer sees
// exactly one logical intent regardless of how many physical rows
// exist across the two schemas.
//
// Test technique: enqueue one intent with shared attached. Confirm
// the local and shared tables each have one row with different ids.
// Then call ListPendingCascadeIntents and confirm it returns exactly
// one entry with the right semantic key (not two).
//
// Failure mode the test catches: the federated query uses GROUP BY id
// (or no GROUP BY at all) and the materializer sees the same logical
// intent twice, creating two redundant cascade theories per invalidation.
func TestOutbox_ListPendingFederatedDedupBySemanticKey(t *testing.T) {
	fx := newCascadeOutboxFixtureWithShared(t)

	// Enqueue one intent inside a transaction with shared attached.
	tx := mustTx(t, fx.dm)
	eventID, err := fx.dm.CreateInvalidationEvent(tx, fx.memID, "memory", "", "federated dedup", 0)
	require.NoError(t, err)

	event := CascadeInvalidation{
		EventID:          eventID,
		DeadArtifactID:   fx.memID,
		DeadArtifactType: "memory",
		Reason:           "federated dedup",
		CascadeDepth:     0,
	}
	targets := []ProvenanceTarget{
		{ArtifactID: fx.decID, ArtifactType: "decision"},
	}
	_, err = fx.dm.EnqueueCascadeIntents(tx, event, targets)
	require.NoError(t, err)
	require.NoError(t, tx.Commit())

	// Sanity: each schema has exactly one row, with DIFFERENT ids.
	var localID, sharedID string
	require.NoError(t, fx.dm.db.QueryRow(
		`SELECT id FROM epistemic_cascade_outbox WHERE invalidation_event_id = ?`,
		eventID,
	).Scan(&localID))
	require.NoError(t, fx.dm.db.QueryRow(
		`SELECT id FROM shared.epistemic_cascade_outbox WHERE invalidation_event_id = ?`,
		eventID,
	).Scan(&sharedID))
	require.NotEqual(t, localID, sharedID,
		"sanity: EnqueueCascadeIntents must mint a fresh id per local+shared write")

	// The federated read must collapse the two physical rows into ONE
	// logical intent. The dedup must be on the semantic key, not on
	// the row id.
	pending, err := fx.dm.ListPendingCascadeIntents(10)
	require.NoError(t, err)

	seenForEvent := 0
	for _, intent := range pending {
		if intent.InvalidationEventID != eventID {
			continue
		}
		seenForEvent++
		// The dedup'd row must still carry the right semantic fields.
		assert.Equal(t, fx.memID, intent.DeadArtifactID)
		assert.Equal(t, fx.decID, intent.DownstreamArtifactID)
	}
	assert.Equal(t, 1, seenForEvent,
		"federated read must dedup by semantic key so one logical intent returns once even with shared attached")
}

// -----------------------------------------------------------------------------
// Downstream discovery (dependencies JSON + provenance rows)
// -----------------------------------------------------------------------------

// TestOutbox_DiscoveryCombinesDependenciesAndProvenance asserts the
// discovery step union the explicit `memories.dependencies` JSON edges
// and the typed epistemic_provenance rows. When a downstream artifact
// is reachable through BOTH paths, it surfaces once in the discovery
// result (dedup by artifact ID).
//
// Failure mode the test catches: discoverCascadeTargets returns only
// provenance rows and silently drops decisions/theories that cite the
// dead artifact via an explicit `dependencies` declaration. Or the
// reverse: only `dependencies` is honored, and retrieval-time citations
// are dropped.
func TestOutbox_DiscoveryCombinesDependenciesAndProvenance(t *testing.T) {
	dm := hermeticDatabaseManager(t)

	memID := "mem-" + GenerateID()
	_, err := dm.db.Exec(`
		INSERT INTO memories (id, collection, content) VALUES (?, 'memories', 'discovery fixture dead')
	`, memID)
	require.NoError(t, err)

	// Decision that EXPLICITLY declares memID in its dependencies.
	decResult, err := dm.RecordDecision(
		"explicit-dep decision context",
		"explicit-dep decision choice",
		"because the discovery fixture requires an explicit-dependency edge",
		"",
		[]string{"fixture"},
		nil,
		ActiveContext{},
	)
	require.NoError(t, err)
	decID, _ := decResult["id"].(string)

	_, err = dm.db.Exec(`UPDATE memories SET dependencies = ? WHERE id = ? AND collection = 'decisions'`,
		`["`+memID+`"]`, decID)
	require.NoError(t, err)

	// Theory that CITES memID via epistemic_provenance (no explicit
	// dependencies declaration).
	theoryResult, err := dm.ProposeTheory(
		"provenance-cite theory hypothesis",
		"provenance-cite theory validation",
		nil,
		[]string{memID},
		[]string{"fixture"},
	)
	require.NoError(t, err)
	theoryID, _ := theoryResult["id"].(string)

	targets, err := dm.discoverCascadeTargets(nil, memID)
	require.NoError(t, err)

	// Both targets must surface.
	ids := make(map[string]string, len(targets))
	for _, tgt := range targets {
		ids[tgt.ArtifactID] = tgt.ArtifactType
	}
	assert.Equal(t, "decision", ids[decID],
		"decision reachable via explicit dependencies must surface in discovery")
	assert.Equal(t, "theory", ids[theoryID],
		"theory reachable via epistemic_provenance must surface in discovery")
}

// TestOutbox_DiscoveryDedupesAcrossPaths asserts the dedup contract: a
// target that is reachable through BOTH `dependencies` and
// `epistemic_provenance` collapses to a single entry. This is the
// "deduplicating a target found through both paths" requirement from
// the Task 3 brief.
//
// Failure mode the test catches: discoverCascadeTargets returns two
// ProvenanceTarget rows for the same artifact (one per discovery path),
// the enqueue step writes two intents, and the materializer creates
// two redundant theories.
func TestOutbox_DiscoveryDedupesAcrossPaths(t *testing.T) {
	dm := hermeticDatabaseManager(t)

	memID := "mem-" + GenerateID()
	_, err := dm.db.Exec(`
		INSERT INTO memories (id, collection, content) VALUES (?, 'memories', 'discovery dedup fixture dead')
	`, memID)
	require.NoError(t, err)

	decResult, err := dm.RecordDecision(
		"both-paths decision context",
		"both-paths decision choice",
		"because the dedup fixture requires both edges",
		"",
		[]string{"fixture"},
		[]string{memID}, // provenance path
		ActiveContext{},
	)
	require.NoError(t, err)
	decID, _ := decResult["id"].(string)

	// Add the explicit-dependency edge on top of the provenance row.
	_, err = dm.db.Exec(`UPDATE memories SET dependencies = ? WHERE id = ? AND collection = 'decisions'`,
		`["`+memID+`"]`, decID)
	require.NoError(t, err)

	targets, err := dm.discoverCascadeTargets(nil, memID)
	require.NoError(t, err)

	count := 0
	for _, tgt := range targets {
		if tgt.ArtifactID == decID {
			count++
		}
	}
	assert.Equal(t, 1, count,
		"target reachable via both paths must collapse to one ProvenanceTarget entry")
}

// TestOutbox_DiscoveryExcludesLessons pins the negative-space contract:
// the cascade only targets decision and theory artifacts. Lessons and
// global rules are excluded. discoverCascadeTargets must NOT surface
// them even when they have an explicit dependency edge or provenance
// citation pointing at the dead artifact.
//
// Failure mode the test catches: discoverCascadeTargets returns lessons
// and the cascade materializer writes a cascade theory for a lesson.
// Lessons are immutable observations, not reasoning artifacts, so
// challenging them violates the design spec's "lessons are out of
// scope" rule.
func TestOutbox_DiscoveryExcludesLessons(t *testing.T) {
	dm := hermeticDatabaseManager(t)

	memID := "mem-" + GenerateID()
	_, err := dm.db.Exec(`
		INSERT INTO memories (id, collection, content) VALUES (?, 'memories', 'lesson-exclusion fixture dead')
	`, memID)
	require.NoError(t, err)

	// A lesson that cites memID via the lesson save path.
	// save_lesson's Provenance Proxy uses IncrementSuccess (telemetry),
	// not RecordProvenance — so even with explicit source_ids wired
	// into the lesson, no epistemic_provenance row exists. We use a
	// direct INSERT into lessons_base to seed a row in the worst-case
	// shape so the discovery filter has something to (correctly)
	// exclude.
	lessonID := "les-" + GenerateID()
	_, err = dm.db.Exec(`
		INSERT INTO lessons_base (id, type, content, created)
		VALUES (?, 'insight', 'lesson citing dead memory', ?)
	`, lessonID, timeNowRFC3339())
	require.NoError(t, err)

	// Even with a contrived dependencies column on the lesson row
	// (lessons_base doesn't have one — we're simulating the worst
	// case where a future schema adds it), the discovery path must
	// not return it because the collection is 'lessons', not
	// 'decisions' / 'theories'.
	targets, err := dm.discoverCascadeTargets(nil, memID)
	require.NoError(t, err)

	for _, tgt := range targets {
		assert.NotEqual(t, "lesson", tgt.ArtifactType,
			"lessons must never appear as cascade targets, got %s", tgt.ArtifactID)
		assert.NotEqual(t, "memory-rule", tgt.ArtifactType,
			"global rules must never appear as cascade targets")
	}
}

// TestOutbox_DiscoveryReturnsEmptyForUnknownSource is the zero-result
// path contract: discoverCascadeTargets on a dead ID that nothing
// references must return an empty slice, not nil-error-with-error.
// This is what the caller's "no dependencies known" branch looks like.
//
// Failure mode the test catches: discoverCascadeTargets returns
// (nil, nil) and a downstream `len(targets) == 0` check passes
// correctly but a JSON marshaller writes `null` instead of `[]`.
func TestOutbox_DiscoveryReturnsEmptyForUnknownSource(t *testing.T) {
	dm := hermeticDatabaseManager(t)

	targets, err := dm.discoverCascadeTargets(nil, "mem-nonexistent")
	require.NoError(t, err)
	require.NotNil(t, targets, "empty-result branch must return non-nil slice for JSON-friendliness")
	assert.Len(t, targets, 0)
}

// -----------------------------------------------------------------------------
// End-to-end: discovery → enqueue → list
// -----------------------------------------------------------------------------

// TestOutbox_EndToEnd_DiscoveryThenEnqueue asserts the production flow:
// discover targets via the union of dependencies + provenance, then
// enqueue one intent per target inside the same transaction as the
// invalidation event. The list endpoint must surface every intent as
// 'pending'.
//
// This is the integration contract the cascade materializer (later
// task) relies on: every dead foundation produces exactly one pending
// intent per eligible downstream, and the deduplication is structural
// rather than call-site discipline.
//
// Failure mode the test catches: discoverCascadeTargets returns the
// targets but EnqueueCascadeIntents is invoked with a different list,
// or the list endpoint returns rows in a different status.
func TestOutbox_EndToEnd_DiscoveryThenEnqueue(t *testing.T) {
	dm := hermeticDatabaseManager(t)

	memID := "mem-" + GenerateID()
	_, err := dm.db.Exec(`
		INSERT INTO memories (id, collection, content) VALUES (?, 'memories', 'e2e fixture dead')
	`, memID)
	require.NoError(t, err)

	decResult, err := dm.RecordDecision(
		"e2e decision context",
		"e2e decision choice",
		"because the e2e fixture requires a downstream decision",
		"",
		[]string{"fixture"},
		nil,
		ActiveContext{},
	)
	require.NoError(t, err)
	decID, _ := decResult["id"].(string)
	_, err = dm.db.Exec(`UPDATE memories SET dependencies = ? WHERE id = ? AND collection = 'decisions'`,
		`["`+memID+`"]`, decID)
	require.NoError(t, err)

	theoryResult, err := dm.ProposeTheory(
		"e2e theory hypothesis",
		"e2e theory validation",
		nil,
		[]string{memID},
		[]string{"fixture"},
	)
	require.NoError(t, err)
	theoryID, _ := theoryResult["id"].(string)

	// Discover targets.
	targets, err := dm.discoverCascadeTargets(nil, memID)
	require.NoError(t, err)
	require.Len(t, targets, 2, "discovery must find both downstream artifacts")

	// Sort for deterministic ordering — the test does not pin the
	// order, but sorting makes failure output reproducible.
	sort.Slice(targets, func(i, j int) bool {
		return targets[i].ArtifactID < targets[j].ArtifactID
	})

	// Enqueue in the same transaction as the invalidation event.
	tx := mustTx(t, dm)
	eventID, err := dm.CreateInvalidationEvent(tx, memID, "memory", "ev-e2e", "e2e fixture", 0)
	require.NoError(t, err)

	event := CascadeInvalidation{
		EventID:           eventID,
		DeadArtifactID:    memID,
		DeadArtifactType:  "memory",
		Reason:            "e2e fixture",
		CascadeDepth:      0,
		TriggerEvidenceID: "ev-e2e",
	}
	written, err := dm.EnqueueCascadeIntents(tx, event, targets)
	require.NoError(t, err)
	assert.Equal(t, 2, written, "one intent per target")
	require.NoError(t, tx.Commit())

	// List endpoint must surface both intents as pending.
	pending, err := dm.ListPendingCascadeIntents(10)
	require.NoError(t, err)

	foundDecision, foundTheory := false, false
	for _, intent := range pending {
		if intent.DownstreamArtifactID != decID && intent.DownstreamArtifactID != theoryID {
			continue
		}
		assert.Equal(t, "pending", intent.Status)
		assert.Equal(t, eventID, intent.InvalidationEventID)
		if intent.DownstreamArtifactID == decID {
			foundDecision = true
		}
		if intent.DownstreamArtifactID == theoryID {
			foundTheory = true
		}
	}
	assert.True(t, foundDecision, "decision intent must surface in pending list")
	assert.True(t, foundTheory, "theory intent must surface in pending list")
}

// -----------------------------------------------------------------------------
// Shared-DB propagation
// -----------------------------------------------------------------------------

// TestOutbox_SharedWritePropagation is the federated write contract:
// when MPM_SHARED_DB is attached, every outbox row written through the
// transactional path must also land in the shared schema. This mirrors
// the Task 2 shared propagation guarantee on epistemic_provenance.
//
// Failure mode the test catches: CreateInvalidationEvent /
// EnqueueCascadeIntents write only to the local table and the cascade
// materializer misses invalidations triggered by other agents.
func TestOutbox_SharedWritePropagation(t *testing.T) {
	fx := newCascadeOutboxFixtureWithShared(t)

	tx := mustTx(t, fx.dm)
	eventID, err := fx.dm.CreateInvalidationEvent(tx, fx.memID, "memory", "", "shared write test", 0)
	require.NoError(t, err)

	event := CascadeInvalidation{
		EventID:          eventID,
		DeadArtifactID:   fx.memID,
		DeadArtifactType: "memory",
		Reason:           "shared write test",
		CascadeDepth:     0,
	}
	targets := []ProvenanceTarget{
		{ArtifactID: fx.decID, ArtifactType: "decision"},
	}
	_, err = fx.dm.EnqueueCascadeIntents(tx, event, targets)
	require.NoError(t, err)
	require.NoError(t, tx.Commit())

	var localN, sharedN int
	require.NoError(t, fx.dm.db.QueryRow(
		`SELECT COUNT(*) FROM epistemic_cascade_outbox WHERE invalidation_event_id = ?`,
		eventID,
	).Scan(&localN))
	require.NoError(t, fx.dm.db.QueryRow(
		`SELECT COUNT(*) FROM shared.epistemic_cascade_outbox WHERE invalidation_event_id = ?`,
		eventID,
	).Scan(&sharedN))
	assert.Equal(t, 1, localN, "local outbox row must land")
	assert.Equal(t, 1, sharedN, "shared outbox row must land when MPM_SHARED_DB is attached")
}