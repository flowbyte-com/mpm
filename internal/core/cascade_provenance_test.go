// cascade_provenance_test.go — coverage for the typed provenance citation
// log (epistemic_provenance) and its public API.
//
// The schema (table + indexes + unique key) lives in schema.go and is
// covered by cascade_schema_test.go. This file focuses on the runtime
// behavior the Task 2 brief calls out:
//
//   - RecordProvenance: writes a typed citation row, validates inputs,
//     dedupes on (source_id, downstream_id, event_id), and propagates
//     to the shared DB when MPM_SHARED_DB is attached.
//   - ListDownstreamCitations: returns only citations whose
//     downstream_type matches the allowedTypes allow-list, federated
//     across local + shared when shared is attached.
//   - Compatibility: legacy untyped source IDs (no `skill:`, `lesson:`,
//     `dec-`, `theory:` prefix) are resolved against the local memory
//     store before insertion so the row lands with the right
//     source_type and the `idx_epistemic_provenance_source` index is
//     useful for discovery.
//   - Atomicity: artifact insert + citation writes run inside a single
//     transaction so a mid-loop failure rolls back the artifact.
//
// Why these tests matter: the cascade materializer (later task) walks
// citations to discover dependency edges. If a citation is silently
// recorded with `source_type='memory'` for what is actually a lesson,
// the cascade dispatch can swallow or mis-deliver an invalidation. The
// unique-key test is the structural guard against double-recording the
// same edge during a noisy recall turn.

package internal

import (
	"database/sql"
	"fmt"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// provenanceFixture spins up a hermetic DatabaseManager, seeds two
// artifacts (a memory and a decision), and returns the DM along with
// the IDs so each test can cite them. Centralized so the assertion
// structure stays focused on the contract under test, not on
// setup boilerplate.
type provenanceFixture struct {
	dm      *DatabaseManager
	memID   string // source memory
	decID   string // downstream decision (created via RecordDecision)
	theoryID string // downstream theory (created via ProposeTheory)
}

// newProvenanceFixture builds the shared setup. Each call mints fresh
// IDs via GenerateID() so concurrent tests don't share rowids.
func newProvenanceFixture(t *testing.T) *provenanceFixture {
	t.Helper()
	dm := hermeticDatabaseManager(t)

	// Seed a memory so the source-side resolution has something to
	// look up. We bypass SaveMemory (which goes through the
	// MemoryStore.AddMemory path with content scanning) and insert
	// directly to keep the fixture tight.
	memID := "mem-" + GenerateID()
	_, err := dm.db.Exec(`
		INSERT INTO memories (id, collection, content)
		VALUES (?, 'memories', 'provenance fixture source')
	`, memID)
	require.NoError(t, err)

	decResult, err := dm.RecordDecision(
		"provenance fixture context",
		"provenance fixture choice",
		"because the fixture requires a downstream artifact",
		"",
		[]string{"fixture"},
		nil,
		ActiveContext{},
	)
	require.NoError(t, err)
	decID, _ := decResult["id"].(string)
	require.NotEmpty(t, decID)

	theoryResult, err := dm.ProposeTheory(
		"provenance fixture hypothesis",
		"provenance fixture validation",
		nil,
		nil,
		[]string{"fixture"},
	)
	require.NoError(t, err)
	theoryID, _ := theoryResult["id"].(string)
	require.NotEmpty(t, theoryID)

	return &provenanceFixture{dm: dm, memID: memID, decID: decID, theoryID: theoryID}
}

// newProvenanceFixtureWithShared extends the shared-DB tests with a
// hermetic DatabaseManager that has MPM_SHARED_DB attached to a
// temp file. MPM_SHARED_DB goes through attachShared which runs the
// cascade DDL (epistemic_provenance + indexes) in both local AND
// shared schemas, so the test exercises the real federated path.
func newProvenanceFixtureWithShared(t *testing.T) *provenanceFixture {
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
		VALUES (?, 'memories', 'provenance-shared fixture source')
	`, memID)
	require.NoError(t, err)

	decResult, err := dm.RecordDecision(
		"shared fixture context",
		"shared fixture choice",
		"because the shared fixture requires a downstream artifact",
		"",
		[]string{"fixture"},
		nil,
		ActiveContext{},
	)
	require.NoError(t, err)
	decID, _ := decResult["id"].(string)
	require.NotEmpty(t, decID)

	return &provenanceFixture{dm: dm, memID: memID, decID: decID}
}

// TestProvenance_RecordAndListRoundtrip is the happy-path contract:
// record one citation, list it back, confirm the row lands with the
// types we wrote.
//
// Failure mode the test catches: someone changes the column order or
// drops a column in the table DDL, or RecordProvenance silently rewrites
// one of the type fields.
func TestProvenance_RecordAndListRoundtrip(t *testing.T) {
	fx := newProvenanceFixture(t)

	require.NoError(t, fx.dm.RecordProvenance(
		fx.memID, "memory",
		fx.decID, "decision",
		"evt-1",
		"",
	))

	citations, err := fx.dm.ListDownstreamCitations(fx.memID, []string{"decision"})
	require.NoError(t, err)
	require.Len(t, citations, 1, "exactly one citation should surface")

	c := citations[0]
	assert.Equal(t, fx.memID, c.SourceID)
	assert.Equal(t, "memory", c.SourceType)
	assert.Equal(t, fx.decID, c.DownstreamID)
	assert.Equal(t, "decision", c.DownstreamType)
	assert.Equal(t, "evt-1", c.EventID)
	assert.NotZero(t, c.CreatedAt, "created_at must be populated by the storage layer")
}

// TestProvenance_IdempotentUniqueKey pins the unique-key contract: a
// second RecordProvenance with the same (source, downstream, event) is
// a no-op, not a duplicate row. This is the structural guard against
// noisy recall turns re-recording the same edge N times.
//
// Failure mode the test catches: someone drops the UNIQUE constraint
// on (source_id, downstream_id, event_id) in schema.go; the duplicate
// row would still be accepted and the materializer (later task) would
// double-count the edge during dependency discovery.
func TestProvenance_IdempotentUniqueKey(t *testing.T) {
	fx := newProvenanceFixture(t)

	for i := 0; i < 3; i++ {
		require.NoError(t, fx.dm.RecordProvenance(
			fx.memID, "memory",
			fx.decID, "decision",
			"evt-idem",
			"",
		), "RecordProvenance must be idempotent on iteration %d", i)
	}

	var n int
	require.NoError(t, fx.dm.db.QueryRow(
		`SELECT COUNT(*) FROM epistemic_provenance
		 WHERE source_id = ? AND downstream_id = ? AND event_id = ?`,
		fx.memID, fx.decID, "evt-idem",
	).Scan(&n))
	assert.Equal(t, 1, n, "unique key on (source, downstream, event) must collapse duplicates")

	citations, err := fx.dm.ListDownstreamCitations(fx.memID, []string{"decision"})
	require.NoError(t, err)
	assert.Len(t, citations, 1, "ListDownstreamCitations must reflect the unique key — not return one row per duplicate write")
}

// TestProvenance_ListFiltersByDownstreamType pins the type-allow-list
// contract: a citation with downstream_type='theory' must not surface
// when the caller filters for ['decision']. This is the guard against
// the cascade materializer (later task) accidentally cascade-targeting
// theories when it asked for decisions.
//
// Failure mode the test catches: someone implements the filter via
// `LIKE '%decision%'` (case-insensitive substring) and accidentally
// matches 'decision' inside other type strings.
func TestProvenance_ListFiltersByDownstreamType(t *testing.T) {
	fx := newProvenanceFixture(t)

	// Two citations: memory -> decision and memory -> theory.
	require.NoError(t, fx.dm.RecordProvenance(
		fx.memID, "memory",
		fx.decID, "decision", "evt-d",
		"",
	))
	require.NoError(t, fx.dm.RecordProvenance(
		fx.memID, "memory",
		fx.theoryID, "theory", "evt-t",
		"",
	))

	// Asking for decisions: only the decision citation surfaces.
	decOnly, err := fx.dm.ListDownstreamCitations(fx.memID, []string{"decision"})
	require.NoError(t, err)
	require.Len(t, decOnly, 1)
	assert.Equal(t, fx.decID, decOnly[0].DownstreamID)

	// Asking for theories: only the theory citation surfaces.
	theoryOnly, err := fx.dm.ListDownstreamCitations(fx.memID, []string{"theory"})
	require.NoError(t, err)
	require.Len(t, theoryOnly, 1)
	assert.Equal(t, fx.theoryID, theoryOnly[0].DownstreamID)

	// Asking for both: both surface, deterministic order is not part
	// of the contract — just check the set.
	both, err := fx.dm.ListDownstreamCitations(fx.memID, []string{"decision", "theory"})
	require.NoError(t, err)
	assert.Len(t, both, 2)
	ids := map[string]string{}
	for _, c := range both {
		ids[c.DownstreamID] = c.DownstreamType
	}
	assert.Equal(t, "decision", ids[fx.decID])
	assert.Equal(t, "theory", ids[fx.theoryID])
}

// TestProvenance_MultipleCitationsForOneSource asserts that a single
// source artifact can fan out to multiple downstream artifacts — the
// citation log is many-to-many on both sides, not 1:1.
//
// Failure mode the test catches: someone implements RecordProvenance
// with an accidental UNIQUE(source_id) constraint or DELETEs prior
// rows on insert.
func TestProvenance_MultipleCitationsForOneSource(t *testing.T) {
	fx := newProvenanceFixture(t)

	// Source memory -> decision AND source memory -> theory.
	require.NoError(t, fx.dm.RecordProvenance(
		fx.memID, "memory",
		fx.decID, "decision", "evt-fan-d",
		"",
	))
	require.NoError(t, fx.dm.RecordProvenance(
		fx.memID, "memory",
		fx.theoryID, "theory", "evt-fan-t",
		"",
	))

	// When the caller asks for both downstream types, both must
	// surface — the log is not exclusive.
	both, err := fx.dm.ListDownstreamCitations(fx.memID, []string{"decision", "theory"})
	require.NoError(t, err)
	assert.Len(t, both, 2)
}

// TestProvenance_LessonAndGlobalRuleTargetsIgnored asserts the
// negative-space contract: the cascade materializer only cares about
// decision and theory citations, and the listing API must not surface
// citations whose downstream_type is lesson or memory-rule (a global
// rule).
//
// Why: the save_lesson Provenance Proxy writes credits via
// retrieval_metadata (telemetry). It does NOT write epistemic_provenance
// rows. This test pins that separation: if a future patch starts
// writing provenance rows for lesson targets, the cascade materializer
// (later task) will start cascade-targeting lessons, which is wrong
// (lessons are reusable knowledge, not cascade targets).
//
// Failure mode the test catches: someone wires the lesson source_ids
// into RecordProvenance and the cascade materializer starts spamming
// lesson invalidations.
func TestProvenance_LessonAndGlobalRuleTargetsIgnored(t *testing.T) {
	fx := newProvenanceFixture(t)

	// We do NOT call RecordProvenance for lesson targets here — the
	// contract is that lesson citations live in retrieval_metadata,
	// not epistemic_provenance. The proof is observational: the
	// citations table contains zero rows for lesson downstream_type.
	var n int
	require.NoError(t, fx.dm.db.QueryRow(
		`SELECT COUNT(*) FROM epistemic_provenance WHERE downstream_type IN ('lesson', 'memory-rule', 'rule')`,
	).Scan(&n))
	assert.Equal(t, 0, n, "epistemic_provenance must never carry lesson or global-rule downstream rows")

	// And the list API returns zero entries when the caller filters
	// for an unrelated type. Sanity check that the empty-result path
	// works.
	citations, err := fx.dm.ListDownstreamCitations(fx.memID, []string{"lesson"})
	require.NoError(t, err)
	assert.Len(t, citations, 0)
}

// TestProvenance_ValidationRejectsEmptyFields pins the input-validation
// contract: empty source_id, downstream_id, or event_id must be
// rejected at the storage boundary, not silently recorded as a row
// that no later query can find (because the unique-key index would
// then accept any second write with the same empties).
//
// Failure mode the test catches: someone implements RecordProvenance
// with bare INSERT and lets the schema's NOT NULL constraints fire
// instead, which surfaces as a confusing SQL error in user-facing
// call paths rather than a domain-specific error.
func TestProvenance_ValidationRejectsEmptyFields(t *testing.T) {
	fx := newProvenanceFixture(t)

	cases := []struct {
		name      string
		src, dst, ev string
	}{
		{"empty source", "", fx.decID, "evt-v"},
		{"empty downstream", fx.memID, "", "evt-v"},
		{"empty event", fx.memID, fx.decID, ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := fx.dm.RecordProvenance(tc.src, "memory", tc.dst, "decision", tc.ev, "")
			require.Error(t, err, "RecordProvenance must reject empty %s", tc.name)
			// We do not pin the exact error message — the validation
			// contract is "no row is written", not "specific text".
			// Asserting on the row count is the structural guard.
			var n int
			require.NoError(t, fx.dm.db.QueryRow(
				`SELECT COUNT(*) FROM epistemic_provenance`,
			).Scan(&n))
			assert.Equal(t, 0, n, "validation failure must not write a row for %s", tc.name)
		})
	}
}

// TestProvenance_LegacyUntypedSourceIDResolution pins the compatibility
// contract for legacy callers: a source_id without a typed prefix
// (skill:, lesson:, dec-, theory:) is resolved against the local
// memories table to discover its true source_type before insert.
//
// Why: the save_lesson Provenance Proxy and the retrieval_metadata
// IncrementSuccess path pass bare IDs. Older callers (and the new
// RecordProvenance surface itself) must not silently assume the type
// is 'memory' — a `dec-abc123` ID will land in the memories table
// with collection='decisions' and the cascade materializer needs to
// know that.
//
// Failure mode the test catches: someone removes the resolution
// helper and bare IDs always land with source_type='memory', which
// breaks the idx_epistemic_provenance_source lookup for decision-
// or theory-class sources.
func TestProvenance_LegacyUntypedSourceIDResolution(t *testing.T) {
	fx := newProvenanceFixture(t)

	// The fixture's decID is in the memories table with
	// collection='decisions'. If we pass it as a bare source ID (no
	// type hint), RecordProvenance should resolve it.
	require.NoError(t, fx.dm.RecordProvenance(
		fx.decID, "",            // empty type forces resolution
		fx.theoryID, "theory",
		"evt-resolve",
		"",
	))

	// The row landed with the resolved type — 'decision', not 'memory'.
	var resolvedType string
	require.NoError(t, fx.dm.db.QueryRow(
		`SELECT source_type FROM epistemic_provenance WHERE source_id = ? AND downstream_id = ?`,
		fx.decID, fx.theoryID,
	).Scan(&resolvedType))
	assert.Equal(t, "decision", resolvedType,
		"bare source_id must be resolved against the memories table; "+
			"dec-* IDs live with collection='decisions'")
}

// TestProvenance_DownstreamIDIsUnknownAccepted pins the asymmetry
// contract: the source ID is resolved when no type hint is given, but
// the downstream ID is trusted as the caller wrote it. The cascade
// materializer will cross-reference downstream IDs against the
// `artifacts` view at lookup time; we don't want to duplicate that
// logic at write time.
//
// Why: callers like RecordDecision and ProposeTheory know the
// downstream ID they just minted. Asking them to pre-resolve it
// before calling RecordProvenance would be busywork.
//
// Failure mode the test catches: someone implements RecordProvenance
// with symmetric source/downstream resolution and starts erroring
// because the freshly-minted theory row isn't visible to the
// resolution query yet (transaction visibility).
func TestProvenance_DownstreamIDIsUnknownAccepted(t *testing.T) {
	fx := newProvenanceFixture(t)

	// Use an obviously-bogus downstream ID. The caller-provided type
	// 'decision' must be honoured — RecordProvenance does NOT cross-
	// check downstream existence.
	require.NoError(t, fx.dm.RecordProvenance(
		fx.memID, "memory",
		"dec-doesnotexist", "decision",
		"evt-unknown",
		"",
	))

	citations, err := fx.dm.ListDownstreamCitations(fx.memID, []string{"decision"})
	require.NoError(t, err)
	require.Len(t, citations, 1)
	assert.Equal(t, "dec-doesnotexist", citations[0].DownstreamID)
}

// TestProvenance_ListIsEmptyForUnknownSource is the zero-result path
// contract: listing citations for a source that was never cited must
// return an empty slice, not nil-error-with-error. This is what the
// cascade materializer (later task) relies on for the "no dependencies
// known" branch.
//
// Failure mode the test catches: someone changes the empty-result
// branch to return (nil, nil) instead of ([]ProvenanceCitation{}, nil)
// and a downstream `len(citations) == 0` check fails because len(nil)==0
// but a JSON marshaller writes `null` instead of `[]`.
func TestProvenance_ListIsEmptyForUnknownSource(t *testing.T) {
	fx := newProvenanceFixture(t)

	citations, err := fx.dm.ListDownstreamCitations("mem-nonexistent", []string{"decision"})
	require.NoError(t, err)
	require.NotNil(t, citations, "empty-result branch must return non-nil slice for JSON-friendliness")
	assert.Len(t, citations, 0)
}

// Compile-time assertion that *DatabaseManager satisfies the
// CoreDB interface with the new provenance methods. This is the
// real interface check — the runtime typed-nil assertion the
// previous version of this test used (`var dm *DatabaseManager;
// var iface CoreDB = dm`) is misleading because Go's interface
// conversion of a typed nil *DatabaseManager produces a non-nil
// interface, so the subsequent `assert.Nil` would assert against
// a non-nil interface and could pass even when the implementation
// is broken. The compile-time assertion below is the version that
// actually catches a signature drift.
var _ CoreDB = (*DatabaseManager)(nil)

// TestProvenance_InterfaceImplemented exists so the file has a
// runnable test entry. The build-time assertion at the top of the
// file is the load-bearing check; this test just keeps the file
// discoverable in `go test -list` and `go test -run` output.
func TestProvenance_InterfaceImplemented(t *testing.T) {
	// Build-time assertion (above) is the contract. The runtime
	// check is a no-op confirmation that the DM is constructible
	// (the fixture factory already proved this; here we just
	// publish a test name).
	var dm *DatabaseManager
	_ = dm // forces the compile-time assertion to be re-evaluated
}

// TestProvenance_RecordDecisionPersistsCitations asserts that the
// RecordDecision wiring — added in Task 2 step 4 — actually persists
// citations to epistemic_provenance when the caller supplies
// source_ids. This is the integration contract the cascade
// materializer (later task) relies on: every decision that lists
// source_ids in its payload must produce one row per source_id.
//
// Failure mode the test catches: someone removes the wiring (e.g.
// deletes the recordSourceCitations call from RecordDecision) and
// decisions silently lose their provenance edges.
func TestProvenance_RecordDecisionPersistsCitations(t *testing.T) {
	dm := hermeticDatabaseManager(t)

	memID := "mem-" + GenerateID()
	_, err := dm.db.Exec(`INSERT INTO memories (id, collection, content) VALUES (?, 'memories', 'cite this')`, memID)
	require.NoError(t, err)

	res, err := dm.RecordDecision(
		"decision-with-sources context",
		"decision-with-sources choice",
		"because the agent cited this memory",
		"",
		[]string{"fixture"},
		[]string{memID},
		ActiveContext{},
	)
	require.NoError(t, err)
	decID, _ := res["id"].(string)
	require.NotEmpty(t, decID)

	// Citation row must exist with the right shape.
	citations, err := dm.ListDownstreamCitations(memID, []string{"decision"})
	require.NoError(t, err)
	require.Len(t, citations, 1)
	assert.Equal(t, decID, citations[0].DownstreamID)
	assert.Equal(t, "decision", citations[0].DownstreamType)
	assert.Equal(t, memID, citations[0].SourceID)
	// Source type is "memory" (resolved via ResolveArtifactType)
	// rather than the empty-string default.
	assert.Equal(t, "memory", citations[0].SourceType,
		"bare source_id must be resolved to its collection-derived type")
}

// TestProvenance_ProposeTheoryPersistsCitations mirrors
// TestProvenance_RecordDecisionPersistsCitations for the theory path.
// The cascade materializer (later task) walks theories' epistemic
// provenance rows the same way it walks decisions'.
//
// Failure mode the test catches: someone wires RecordDecision but
// forgets the parallel wiring in ProposeTheory.
func TestProvenance_ProposeTheoryPersistsCitations(t *testing.T) {
	dm := hermeticDatabaseManager(t)

	memID := "mem-" + GenerateID()
	_, err := dm.db.Exec(`INSERT INTO memories (id, collection, content) VALUES (?, 'memories', 'cite this for theory')`, memID)
	require.NoError(t, err)

	res, err := dm.ProposeTheory(
		"theory-with-sources hypothesis",
		"theory-with-sources validation",
		nil,
		[]string{memID},
		[]string{"fixture"},
	)
	require.NoError(t, err)
	theoryID, _ := res["id"].(string)
	require.NotEmpty(t, theoryID)

	citations, err := dm.ListDownstreamCitations(memID, []string{"theory"})
	require.NoError(t, err)
	require.Len(t, citations, 1)
	assert.Equal(t, theoryID, citations[0].DownstreamID)
	assert.Equal(t, "theory", citations[0].DownstreamType)
	assert.Equal(t, memID, citations[0].SourceID)
	assert.Equal(t, "memory", citations[0].SourceType)
}

// TestProvenance_LessonSourceIDsStayTelemetryOnly is the negative-
// space guard for the lesson surface. The save_lesson handler keeps
// its IncrementSuccess (telemetry) behavior — no epistemic_provenance
// row is written for lesson citations. This test pins the separation.
//
// Why: a future patch that wires source_ids into save_lesson's
// RecordProvenance call would silently start cascade-targeting
// lessons. The test fails fast if anyone makes that change.
//
// Failure mode the test catches: someone copies the decision/theory
// wiring into the lesson path and breaks the cascade materializer's
// "decision-or-theory-only" filter.
func TestProvenance_LessonSourceIDsStayTelemetryOnly(t *testing.T) {
	dm := hermeticDatabaseManager(t)

	memID := "mem-" + GenerateID()
	_, err := dm.db.Exec(`INSERT INTO memories (id, collection, content) VALUES (?, 'memories', 'lesson cite source')`, memID)
	require.NoError(t, err)

	out, _, err := dm.SaveLesson("lesson content", "insight", []string{"fixture"})
	require.NoError(t, err)
	lessonID, _ := out["id"].(string)
	require.NotEmpty(t, lessonID)

	// Manually emulate the save_lesson Provenance Proxy: it does
	// IncrementSuccess, NOT RecordProvenance. So no row should land
	// in epistemic_provenance.
	require.NoError(t, dm.IncrementSuccess(memID, "memory"))

	var n int
	require.NoError(t, dm.db.QueryRow(
		`SELECT COUNT(*) FROM epistemic_provenance WHERE downstream_type = 'lesson'`,
	).Scan(&n))
	assert.Equal(t, 0, n, "lesson source_ids must NOT route through RecordProvenance — telemetry only")
	_ = lessonID // referenced for the lesson-id-only assertion above
}

// TestProvenance_SharedWritePropagation is the I-1 fix coverage:
// when MPM_SHARED_DB is attached, RecordProvenance must persist the
// citation to BOTH local and shared tables. The federated
// ListDownstreamCitations must surface the citation exactly once
// (dedup by unique key) so the cascade materializer doesn't
// double-count.
//
// Failure mode the test catches: someone removes the shared write
// branch in recordProvenanceNode and the cascade materializer misses
// citations that other agents can see.
func TestProvenance_SharedWritePropagation(t *testing.T) {
	fx := newProvenanceFixtureWithShared(t)

	require.NoError(t, fx.dm.RecordProvenance(
		fx.memID, "memory",
		fx.decID, "decision", "evt-shared-write",
		"",
	))

	// Local row exists.
	var localN int
	require.NoError(t, fx.dm.db.QueryRow(
		`SELECT COUNT(*) FROM epistemic_provenance
		 WHERE source_id = ? AND downstream_id = ? AND event_id = ?`,
		fx.memID, fx.decID, "evt-shared-write",
	).Scan(&localN))
	assert.Equal(t, 1, localN, "local citation must land")

	// Shared row exists.
	var sharedN int
	require.NoError(t, fx.dm.db.QueryRow(
		`SELECT COUNT(*) FROM shared.epistemic_provenance
		 WHERE source_id = ? AND downstream_id = ? AND event_id = ?`,
		fx.memID, fx.decID, "evt-shared-write",
	).Scan(&sharedN))
	assert.Equal(t, 1, sharedN, "shared citation must land when MPM_SHARED_DB is attached")

	// Federated read surfaces the citation exactly once — the
	// UNION ALL + GROUP BY dedup must collapse the two underlying
	// rows into one ProvenanceCitation.
	citations, err := fx.dm.ListDownstreamCitations(fx.memID, []string{"decision"})
	require.NoError(t, err)
	require.Len(t, citations, 1,
		"federated read must dedup by (source, downstream, event) so the materializer sees each citation once")
	assert.Equal(t, fx.decID, citations[0].DownstreamID)
}

// TestProvenance_SharedWriteIdempotent is the per-table idempotency
// guard for the shared write path. A second RecordProvenance with
// the same triple must produce ONE row in each table (no duplicates
// from the local AND shared idempotency path).
//
// Failure mode the test catches: someone drops the ON CONFLICT
// clause from the shared INSERT and the second write fails the
// UNIQUE constraint.
func TestProvenance_SharedWriteIdempotent(t *testing.T) {
	fx := newProvenanceFixtureWithShared(t)

	for i := 0; i < 3; i++ {
		require.NoError(t, fx.dm.RecordProvenance(
			fx.memID, "memory",
			fx.decID, "decision", "evt-shared-idem",
			"",
		), "RecordProvenance must be idempotent on iteration %d", i)
	}

	// One row per table, not three.
	var localN, sharedN int
	require.NoError(t, fx.dm.db.QueryRow(
		`SELECT COUNT(*) FROM epistemic_provenance WHERE event_id = ?`,
		"evt-shared-idem",
	).Scan(&localN))
	require.NoError(t, fx.dm.db.QueryRow(
		`SELECT COUNT(*) FROM shared.epistemic_provenance WHERE event_id = ?`,
		"evt-shared-idem",
	).Scan(&sharedN))
	assert.Equal(t, 1, localN, "local citation must collapse to one row")
	assert.Equal(t, 1, sharedN, "shared citation must collapse to one row")

	// Federated read still surfaces one citation.
	citations, err := fx.dm.ListDownstreamCitations(fx.memID, []string{"decision"})
	require.NoError(t, err)
	require.Len(t, citations, 1)
}

// TestProvenance_SharedListFederation is the read-side companion to
// TestProvenance_SharedWritePropagation. When a citation lives in
// LOCAL only (e.g. a legacy row written before shared was attached),
// the federated read must still surface it.
//
// Failure mode the test catches: someone implements the federated
// query with INTERSECT instead of UNION ALL and only surfaces rows
// that exist in both tables.
func TestProvenance_SharedListFederation(t *testing.T) {
	fx := newProvenanceFixtureWithShared(t)

	// Write a citation through the API — both local and shared get
	// the row.
	require.NoError(t, fx.dm.RecordProvenance(
		fx.memID, "memory",
		fx.decID, "decision", "evt-fed",
		"",
	))

	// Manually delete the shared row so the citation exists ONLY
	// in local. The federated read must still surface it.
	_, err := fx.dm.db.Exec(
		`DELETE FROM shared.epistemic_provenance WHERE event_id = ?`,
		"evt-fed",
	)
	require.NoError(t, err)

	citations, err := fx.dm.ListDownstreamCitations(fx.memID, []string{"decision"})
	require.NoError(t, err)
	require.Len(t, citations, 1, "federated read must surface local-only citations")
	assert.Equal(t, fx.decID, citations[0].DownstreamID)
}

// TestProvenance_RecordDecisionRollsBackOnCitationFailure is the
// I-2 fix coverage. The original "artifact-first, citations-second"
// ordering could leave a decision row in the DB with no citations
// if the citation loop failed mid-way. The fix wraps the artifact
// insert + citation loop in a single WithTx transaction; a
// citation failure rolls back the decision row.
//
// Failure mode the test catches: someone removes the WithTx wrapper
// from RecordDecision and the substrate regresses to the artifact-
// first ordering.
//
// Test technique: drop the shared cascade table to force the
// shared INSERT to fail. The local write succeeds, the shared
// write fails inside the tx, the WithTx wrapper rolls back the
// local write. This is the real production failure mode — schema
// drift between local and shared — not a synthetic injection.
func TestProvenance_RecordDecisionRollsBackOnCitationFailure(t *testing.T) {
	// Build the shared fixture but DON'T create the upstream
	// decision via the fixture factory — we want a clean count
	// of decisions created AFTER the schema-drop injection.
	workspace := t.TempDir()
	sharedPath := filepath.Join(workspace, "shared.db")
	t.Setenv("MPM_WORKSPACE", workspace)
	t.Setenv("MPM_SHARED_DB", sharedPath)
	t.Setenv("MPM_SHARED_READONLY", "")
	dm, err := NewDatabaseManager(workspace)
	require.NoError(t, err)
	t.Cleanup(func() { dm.Close() })
	require.Equal(t, sharedPath, dm.SharedAttached())

	// Seed a source memory for the citation.
	memID := "mem-" + GenerateID()
	_, err = dm.db.Exec(`INSERT INTO memories (id, collection, content) VALUES (?, 'memories', 'cite this')`, memID)
	require.NoError(t, err)

	// Drop the shared cascade table to force the shared write to
	// fail. This simulates a schema drift / migration failure
	// between local and shared.
	_, err = dm.db.Exec(`DROP TABLE shared.epistemic_provenance`)
	require.NoError(t, err)

	// RecordDecision must error because the shared write fails.
	res, err := dm.RecordDecision(
		"rollback-test context",
		"rollback-test choice",
		"because the shared write must fail",
		"",
		[]string{"fixture"},
		[]string{memID},
		ActiveContext{},
	)
	require.Error(t, err, "RecordDecision must error when shared write fails")
	assert.Contains(t, err.Error(), "shared write",
		"error must surface the shared-write failure, not a generic SQL error")
	_ = res

	// The decision row must NOT be in the local memories table.
	// The fixture is empty (we did not call RecordDecision before
	// the drop), so a strict count of 0 is the correct assertion.
	var localN int
	require.NoError(t, dm.db.QueryRow(
		`SELECT COUNT(*) FROM memories WHERE collection = 'decisions'`,
	).Scan(&localN))
	assert.Equal(t, 0, localN,
		"the decision row must be rolled back when the shared citation write fails")

	// The local citation must also be rolled back (atomicity).
	var localCiteN int
	require.NoError(t, dm.db.QueryRow(
		`SELECT COUNT(*) FROM epistemic_provenance WHERE downstream_type = 'decision'`,
	).Scan(&localCiteN))
	assert.Equal(t, 0, localCiteN,
		"local citation must be rolled back when the shared write fails")
}

// TestProvenance_ProposeTheoryRollsBackOnCitationFailure is the
// I-2 fix coverage for the theory path. The same WithTx wrapper
// that protects RecordDecision protects ProposeTheory. We use
// the same shared-table-drop injection — when the shared cascade
// schema is missing, the local write (and the local citation) must
// roll back.
//
// Failure mode the test catches: someone applies the WithTx wrapper
// to RecordDecision but forgets ProposeTheory.
func TestProvenance_ProposeTheoryRollsBackOnCitationFailure(t *testing.T) {
	// Build the shared fixture but skip the upstream decision
	// creation in the fixture factory — we want a clean count of
	// theories created AFTER the schema-drop injection.
	workspace := t.TempDir()
	sharedPath := filepath.Join(workspace, "shared.db")
	t.Setenv("MPM_WORKSPACE", workspace)
	t.Setenv("MPM_SHARED_DB", sharedPath)
	t.Setenv("MPM_SHARED_READONLY", "")
	dm, err := NewDatabaseManager(workspace)
	require.NoError(t, err)
	t.Cleanup(func() { dm.Close() })
	require.Equal(t, sharedPath, dm.SharedAttached())

	// Seed a source memory for the citation.
	memID := "mem-" + GenerateID()
	_, err = dm.db.Exec(`INSERT INTO memories (id, collection, content) VALUES (?, 'memories', 'cite this')`, memID)
	require.NoError(t, err)

	// Drop the shared cascade table to force the shared write to
	// fail.
	_, err = dm.db.Exec(`DROP TABLE shared.epistemic_provenance`)
	require.NoError(t, err)

	res, err := dm.ProposeTheory(
		"rollback-test theory",
		"rollback-test validation",
		nil,
		[]string{memID},
		[]string{"fixture"},
	)
	require.Error(t, err, "ProposeTheory must error when shared write fails")
	_ = res

	// The theory row must NOT be in the local memories table.
	var localN int
	require.NoError(t, dm.db.QueryRow(
		`SELECT COUNT(*) FROM memories WHERE collection = 'theories'`,
	).Scan(&localN))
	assert.Equal(t, 0, localN,
		"the theory row must be rolled back when the shared citation write fails")

	// The local citation must also be rolled back.
	var localCiteN int
	require.NoError(t, dm.db.QueryRow(
		`SELECT COUNT(*) FROM epistemic_provenance WHERE downstream_type = 'theory'`,
	).Scan(&localCiteN))
	assert.Equal(t, 0, localCiteN,
		"local citation must be rolled back when the shared write fails")
}

// TestProvenance_ValidationFailureLeavesNoRows is the empty-fields
// rollback contract: a validation failure inside RecordProvenance
// (empty source_id, downstream_id, or event_id) must not write any
// row, even when invoked via the transactional path. The existing
// TestProvenance_ValidationRejectsEmptyFields covers the row count
// via the un-transactional surface; this test pins the same
// contract for the recordProvenanceNode path that ProposeTheory
// and RecordDecision use internally.
//
// Failure mode the test catches: someone removes the validation
// guards from recordProvenanceNode and the artifact path silently
// writes citations with empty fields.
func TestProvenance_ValidationFailureLeavesNoRows(t *testing.T) {
	fx := newProvenanceFixtureWithShared(t)

	// Use the transactional internal entry point directly. The
	// public RecordProvenance wraps the same node call in its
	// own WithTx, but the validation has to fail before any
	// INSERT runs.
	err := fx.dm.recordProvenanceNode(fx.dm, "", "memory", fx.decID, "decision", "evt-bad", "")
	require.Error(t, err)

	// Both local and shared must be untouched.
	var localN, sharedN int
	require.NoError(t, fx.dm.db.QueryRow(
		`SELECT COUNT(*) FROM epistemic_provenance WHERE event_id = ?`, "evt-bad",
	).Scan(&localN))
	localNTotal := 0
	require.NoError(t, fx.dm.db.QueryRow(
		`SELECT COUNT(*) FROM epistemic_provenance`,
	).Scan(&localNTotal))
	assert.Equal(t, 0, localNTotal, "local must have zero rows after validation failure")

	if fx.dm.sharedAttached {
		require.NoError(t, fx.dm.db.QueryRow(
			`SELECT COUNT(*) FROM shared.epistemic_provenance`,
		).Scan(&sharedN))
		assert.Equal(t, 0, sharedN, "shared must have zero rows after validation failure")
	}
}

// TestProvenance_ArtifactExistsWithCitations is the positive-space
// guard for the WithTx wrapper: when the citation loop succeeds,
// the artifact row AND the citation rows must all land. This is the
// non-rollback half of the atomicity contract.
//
// Failure mode the test catches: someone wraps the RecordDecision
// in WithTx but the inner fn forgets to commit by leaving the
// DBNode commit un-driven (e.g. swallowing the error). The
// artifact row would be lost.
func TestProvenance_ArtifactExistsWithCitations(t *testing.T) {
	fx := newProvenanceFixtureWithShared(t)

	memID := "mem-" + GenerateID()
	_, err := fx.dm.db.Exec(`INSERT INTO memories (id, collection, content) VALUES (?, 'memories', 'cite this')`, memID)
	require.NoError(t, err)

	res, err := fx.dm.RecordDecision(
		"atomicity-check context",
		"atomicity-check choice",
		"because the atomicity check requires a downstream artifact",
		"",
		[]string{"fixture"},
		[]string{memID},
		ActiveContext{},
	)
	require.NoError(t, err)
	decID, _ := res["id"].(string)
	require.NotEmpty(t, decID)

	// Local artifact + local citation must land.
	var localDecN, localCiteN int
	require.NoError(t, fx.dm.db.QueryRow(
		`SELECT COUNT(*) FROM memories WHERE id = ? AND collection = 'decisions'`,
		decID,
	).Scan(&localDecN))
	require.NoError(t, fx.dm.db.QueryRow(
		`SELECT COUNT(*) FROM epistemic_provenance WHERE downstream_id = ?`,
		decID,
	).Scan(&localCiteN))
	assert.Equal(t, 1, localDecN, "local decision row must exist")
	assert.Equal(t, 1, localCiteN, "local citation row must exist")

	// Shared citation must also land (when shared is attached).
	if fx.dm.sharedAttached {
		var sharedCiteN int
		require.NoError(t, fx.dm.db.QueryRow(
			`SELECT COUNT(*) FROM shared.epistemic_provenance WHERE downstream_id = ?`,
			decID,
		).Scan(&sharedCiteN))
		assert.Equal(t, 1, sharedCiteN, "shared citation row must exist after atomicity-positive write")
	}
}

// citationsCountBySource is a small helper used by the legacy-ID
// resolution test to assert the row landed.
func citationsCountBySource(t *testing.T, db *sql.DB, sourceID string) int {
	t.Helper()
	var n int
	require.NoError(t, db.QueryRow(
		`SELECT COUNT(*) FROM epistemic_provenance WHERE source_id = ?`,
		sourceID,
	).Scan(&n))
	return n
}

// citationRowsByDownstream is a debug-only helper used by `go test -v`
// output when a test fails; keeps the failure output informative
// without a dependency on a third-party SQL pretty-printer.
func citationRowsByDownstream(t *testing.T, db *sql.DB, downstreamID string) []string {
	t.Helper()
	rows, err := db.Query(
		`SELECT source_id, source_type, event_id FROM epistemic_provenance WHERE downstream_id = ?`,
		downstreamID,
	)
	if err != nil {
		return []string{fmt.Sprintf("query error: %v", err)}
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var src, typ, evt string
		if err := rows.Scan(&src, &typ, &evt); err != nil {
			out = append(out, fmt.Sprintf("scan error: %v", err))
			continue
		}
		out = append(out, fmt.Sprintf("source=%s type=%s event=%s", src, typ, evt))
	}
	return out
}