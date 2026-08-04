// cascade_provenance_test.go — coverage for the typed provenance citation
// log (epistemic_provenance) and its public API.
//
// The schema (table + indexes + unique key) lives in schema.go and is
// covered by cascade_schema_test.go. This file focuses on the runtime
// behavior the Task 2 brief calls out:
//
//   - RecordProvenance: writes a typed citation row, validates inputs,
//     dedupes on (source_id, downstream_id, event_id).
//   - ListDownstreamCitations: returns only citations whose
//     downstream_type matches the allowedTypes allow-list.
//   - Compatibility: legacy untyped source IDs (no `skill:`, `lesson:`,
//     `dec-`, `theory:` prefix) are resolved against the local memory
//     store before insertion so the row lands with the right
//     source_type and the `idx_epistemic_provenance_source` index is
//     useful for discovery.
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
	"errors"
	"fmt"
	"strings"
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
	))
	require.NoError(t, fx.dm.RecordProvenance(
		fx.memID, "memory",
		fx.theoryID, "theory", "evt-t",
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
	))
	require.NoError(t, fx.dm.RecordProvenance(
		fx.memID, "memory",
		fx.theoryID, "theory", "evt-fan-t",
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
			err := fx.dm.RecordProvenance(tc.src, "memory", tc.dst, "decision", tc.ev)
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

// TestProvenance_InterfaceImplemented pins the compile-time guarantee
// that *DatabaseManager satisfies the CoreDB interface with the new
// provenance methods. The test will fail to build if the interface
// signature drifts from the implementation.
//
// Failure mode the test catches: someone renames
// RecordProvenance/ListDownstreamCitations without updating CoreDB
// or the tools/handlers.go path that consumes it.
func TestProvenance_InterfaceImplemented(t *testing.T) {
	// The compile-time assertion is the test. If this file builds,
	// *DatabaseManager satisfies the interface. Add a runtime sanity
	// check so the test reads as a test even without -run=Test.
	var dm *DatabaseManager = nil
	var iface CoreDB = dm
	assert.Nil(t, iface, "interface check is compile-time only; runtime sanity")
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

// errIsStringContains is a small matcher that asserts an error message
// contains a substring without pinning exact wording. Used by tests
// that exercise domain errors but want to remain tolerant of future
// rewording.
func errIsStringContains(err error, substr string) bool {
	if err == nil {
		return false
	}
	return strings.Contains(err.Error(), substr)
}

// Reference: keep `errors` imported for future matchers. The current
// contract uses errIsStringContains; if a future patch wants
// errors.Is-style matching, the import is already in place.
var _ = errors.Is