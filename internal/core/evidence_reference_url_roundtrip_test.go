// evidence_reference_url_roundtrip_test.go — the durable-storage contract for
// evidence.reference_url.
//
// Validation is proven in evidence_reference_url_test.go. This file pins the
// four claims that make the feature usable rather than merely well-formed:
//
//	1. A supplied URL round-trips byte-identically through every read surface.
//	2. Omitting it leaves rows and reads exactly as they were before.
//	3. A pre-feature database gains the column with its rows intact.
//	4. URL presence changes NOTHING about confidence or work verification.

package internal

import (
	"database/sql"
	"os"
	"strings"
	"testing"
	"time"

	_ "github.com/mattn/go-sqlite3"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// legacyEvidenceWithoutReferenceURL is the canonical PRE-FEATURE evidence
// table. Hand-rolled rather than derived from BaseTables because the point is
// to reproduce a database created before reference_url existed.
//
// The artifact_type CHECK deliberately omits 'work', which is the state a
// pre-work-release database is in. That combination — no reference_url
// column AND a CHECK that migrateEvidenceWorkType must widen — is what makes
// the upgrade path in TestReferenceURL_LegacyDatabaseUpgrade meaningful:
// migrateEvidenceWorkType hardcodes the destination table shape, so a
// column added between the live table and that hardcoded DDL breaks the copy.
const legacyEvidenceWithoutReferenceURL = `
CREATE TABLE evidence (
    id                  TEXT PRIMARY KEY,
    artifact_id         TEXT NOT NULL,
    artifact_type       TEXT NOT NULL CHECK (artifact_type IN ('memory','theory','decision','lesson')),
    type                TEXT NOT NULL,
    source_group        TEXT NOT NULL,
    strength            REAL NOT NULL CHECK (strength >= -1.0 AND strength <= 1.0),
    independence_factor REAL NOT NULL DEFAULT 1.0,
    created_by          TEXT NOT NULL,
    created_at          INTEGER NOT NULL,
    expires_at          INTEGER,
    notes               TEXT
);
`

// seedLegacyEvidenceDB produces a file-backed database in the state a real
// pre-feature install would be in: every table present, but `evidence`
// downgraded to its pre-feature shape (no reference_url column, and a
// CHECK that does not yet accept 'work').
//
// The downgrade is applied to a fully-initialized database rather than to a
// hand-built single table. That matters: a database containing only `evidence`
// fails InitSchema for an unrelated, pre-existing reason (the `artifacts` view
// references memories.retrieval_priority, which the base schema does not yet
// have). Reproducing that would have made this test assert nothing about
// reference_url. A real legacy install has all its tables, so that is what
// this builds.
//
// Uses a real file DSN (per the test_db_safety_test.go allowlist) because the
// migration needs on-disk persistence to model an upgrade of an existing
// install.
func seedLegacyEvidenceDB(t *testing.T) (*DatabaseManager, func()) {
	t.Helper()
	PinIsolatedWorkspace(t)

	path := t.TempDir() + "/legacy.db"

	// Pass 1: build a complete database, then downgrade `evidence` to the
	// pre-feature shape.
	db, err := sql.Open("sqlite3", path)
	require.NoError(t, err)
	dm1 := NewDatabaseManagerForDB(db)
	require.NoError(t, dm1.InitSchema())

	_, err = db.Exec(`DROP TABLE evidence`)
	require.NoError(t, err)
	_, err = db.Exec(legacyEvidenceWithoutReferenceURL)
	require.NoError(t, err)

	// A legacy row with non-empty notes plus a NULL-notes counterpart, so
	// the upgrade test proves row CONTENT survives, not just row count.
	_, err = db.Exec(`INSERT INTO evidence
		(id, artifact_id, artifact_type, type, source_group, strength,
		 independence_factor, created_by, created_at, expires_at, notes)
		VALUES ('ev-legacy-1','mem-legacy','memory','observation','filesystem',0.4,
		        1.0,'legacy-user',1700000000,NULL,'a legacy observation')`)
	require.NoError(t, err)
	_, err = db.Exec(`INSERT INTO evidence
		(id, artifact_id, artifact_type, type, source_group, strength,
		 independence_factor, created_by, created_at, expires_at, notes)
		VALUES ('ev-legacy-2','mem-legacy','memory','observation','filesystem',0.4,
		        1.0,'legacy-user',1700000001,NULL,NULL)`)
	require.NoError(t, err)
	require.NoError(t, db.Close())

	// Pass 2: reopen through the normal init path so the production upgrade
	// (SafeMigrations, then migrateEvidenceWorkType) runs for real.
	db2, err := sql.Open("sqlite3", path)
	require.NoError(t, err)
	dm := NewDatabaseManagerForDB(db2)
	require.NoError(t, dm.InitSchema())

	return dm, func() { _ = db2.Close() }
}

// TestEvidenceReferenceURL_AddWithoutURL pins backward compatibility.
//
// This is the compatibility pin: it must compile and pass with
// EvidenceInput.ReferenceURL never mentioned, exactly as every pre-existing
// caller leaves it. If adding the field ever broke the omit-the-field path,
// this test goes red.
func TestEvidenceReferenceURL_AddWithoutURL(t *testing.T) {
	dm := newTestDM(t)
	memID := "refurl-absent-1"
	_, err := dm.ExecTracked(
		`INSERT INTO memories (id, collection, content) VALUES (?, 'memories', 'x')`, 0, memID)
	require.NoError(t, err)

	// Deliberately no ReferenceURL field set.
	err = AddEvidence(dm, EvidenceInput{
		ArtifactID:   memID,
		ArtifactType: "memory",
		Type:         "observation",
		SourceGroup:  "filesystem",
		Strength:     0.4,
		CreatedBy:    "tester",
		CreatedAt:    time.Now(),
	})
	require.NoError(t, err)

	// Absent persists as SQL NULL, not an empty string — the distinction
	// keeps "no reference supplied" queryable.
	var refURL sql.NullString
	require.NoError(t, dm.QueryRowTracked(
		`SELECT reference_url FROM evidence WHERE artifact_id = ?`, memID).Scan(&refURL))
	assert.False(t, refURL.Valid, "an omitted reference_url must persist as NULL, not ''")

	rows, err := ListEvidenceForArtifact(dm, memID, "memory")
	require.NoError(t, err)
	require.Len(t, rows, 1)
	assert.Equal(t, "", rows[0].ReferenceURL, "a NULL reads back as the empty string")
}

// TestEvidenceReferenceURL_ExactRoundTripAllReadPaths is the central
// round-trip pin: what the user supplied is what every surface returns.
//
// It deliberately uses a URL carrying a fragment, a descending query order,
// percent-encoding and mixed case — every shape a normalizer would rewrite.
// The assertion is byte equality on all four read surfaces.
func TestEvidenceReferenceURL_ExactRoundTripAllReadPaths(t *testing.T) {
	const ref = "https://Example.COM/Path/To/Spec.md?v=2&a=1&z=9#Section-3"

	dm := newTestDM(t)
	memID := "refurl-roundtrip-1"
	_, err := dm.ExecTracked(
		`INSERT INTO memories (id, collection, content) VALUES (?, 'memories', 'x')`, 0, memID)
	require.NoError(t, err)

	require.NoError(t, AddEvidence(dm, EvidenceInput{
		ArtifactID:   memID,
		ArtifactType: "memory",
		Type:         "external_reference",
		SourceGroup:  "external",
		Strength:     0.6,
		CreatedBy:    "tester",
		CreatedAt:    time.Now(),
		ReferenceURL: ref,
	}))

	t.Run("raw column", func(t *testing.T) {
		var got string
		require.NoError(t, dm.QueryRowTracked(
			`SELECT reference_url FROM evidence WHERE artifact_id = ?`, memID).Scan(&got))
		assert.Equal(t, ref, got, "the stored bytes must be exactly what was supplied")
	})

	t.Run("ListEvidenceForArtifact", func(t *testing.T) {
		rows, err := ListEvidenceForArtifact(dm, memID, "memory")
		require.NoError(t, err)
		require.Len(t, rows, 1)
		assert.Equal(t, ref, rows[0].ReferenceURL)
	})

	t.Run("ListEvidence (unfiltered CLI path)", func(t *testing.T) {
		rows, err := ListEvidence(dm)
		require.NoError(t, err)
		require.Len(t, rows, 1)
		assert.Equal(t, ref, rows[0].ReferenceURL)
	})

	t.Run("ListEvidence (MCP-shaped map path)", func(t *testing.T) {
		res, err := dm.ListEvidence(memID, "memory")
		require.NoError(t, err)
		rows, ok := res["evidence"].([]map[string]interface{})
		require.True(t, ok, "expected the MCP row-map shape")
		require.Len(t, rows, 1)
		got, present := rows[0]["reference_url"]
		require.True(t, present, "the MCP row map must always carry a reference_url key")
		assert.Equal(t, ref, got)
	})
}

// TestEvidenceReferenceURL_RejectsInvalidWithoutPartialState pins that a bad
// URL leaves nothing behind. Evidence is a ledger; a rejected write that
// still inserted a row would be worse than the rejection itself.
func TestEvidenceReferenceURL_RejectsInvalidWithoutPartialState(t *testing.T) {
	dm := newTestDM(t)
	memID := "refurl-reject-1"
	_, err := dm.ExecTracked(
		`INSERT INTO memories (id, collection, content) VALUES (?, 'memories', 'x')`, 0, memID)
	require.NoError(t, err)

	for _, bad := range []string{
		"ftp://example.com/x",
		"file:///etc/passwd",
		"example.com/no-scheme",
		"https://",
		"not a url",
	} {
		err := AddEvidence(dm, EvidenceInput{
			ArtifactID:   memID,
			ArtifactType: "memory",
			Type:         "observation",
			SourceGroup:  "filesystem",
			Strength:     0.4,
			CreatedBy:    "tester",
			CreatedAt:    time.Now(),
			ReferenceURL: bad,
		})
		require.Error(t, err, "%q must be rejected", bad)
		assert.Contains(t, err.Error(), "reference_url",
			"the error should name the field that failed validation")
	}

	var count int
	require.NoError(t, dm.QueryRowTracked(
		`SELECT COUNT(*) FROM evidence WHERE artifact_id = ?`, memID).Scan(&count))
	assert.Equal(t, 0, count, "no evidence row may be persisted when the URL is rejected")
}

// TestEvidenceReferenceURL_ScannerRejectsCredentialInURL pins that a URL
// query string is scanned like every other user-supplied evidence field.
// Signed links and API endpoints are a more common credential vector than
// free text, so an unscanned reference_url would be a net regression.
func TestEvidenceReferenceURL_ScannerRejectsCredentialInURL(t *testing.T) {
	dm := newTestDM(t)
	memID := "refurl-secret-1"
	_, err := dm.ExecTracked(
		`INSERT INTO memories (id, collection, content) VALUES (?, 'memories', 'x')`, 0, memID)
	require.NoError(t, err)

	err = AddEvidence(dm, EvidenceInput{
		ArtifactID:   memID,
		ArtifactType: "memory",
		Type:         "observation",
		SourceGroup:  "filesystem",
		Strength:     0.4,
		CreatedBy:    "tester",
		CreatedAt:    time.Now(),
		ReferenceURL: "https://api.example.com/v1/data?api_key=sk-abcdefghijklmnopqrstuv",
	})
	require.Error(t, err, "a credential in the query string must be rejected")
	assert.Contains(t, err.Error(), "reference_url")
}

// TestEvidenceReferenceURL_NoUniquenessConstraint pins cardinality.
//
// The same URL legitimately cites many things — a spec referenced by fifty
// evidence rows is normal, not duplication. A UNIQUE constraint would make
// the second citation impossible, so it is pinned absent. No index is
// created either: nothing queries by URL.
func TestEvidenceReferenceURL_NoUniquenessConstraint(t *testing.T) {
	dm := newTestDM(t)
	const shared = "https://example.com/shared-spec"

	var indexed int
	require.NoError(t, dm.QueryRowTracked(
		`SELECT COUNT(*) FROM pragma_index_list('evidence') WHERE name LIKE '%reference%'`).Scan(&indexed))
	assert.Equal(t, 0, indexed, "nothing queries by reference URL, so it must not be indexed")

	ids := []string{"refurl-dup-a", "refurl-dup-b", "refurl-dup-c"}
	for _, id := range ids {
		_, err := dm.ExecTracked(
			`INSERT INTO memories (id, collection, content) VALUES (?, 'memories', 'x')`, 0, id)
		require.NoError(t, err)
		require.NoError(t, AddEvidence(dm, EvidenceInput{
			ArtifactID:   id,
			ArtifactType: "memory",
			Type:         "external_reference",
			SourceGroup:  "external",
			Strength:     0.6,
			CreatedBy:    "tester",
			CreatedAt:    time.Now(),
			ReferenceURL: shared,
		}))
	}

	var count int
	require.NoError(t, dm.QueryRowTracked(
		`SELECT COUNT(*) FROM evidence WHERE reference_url = ?`, shared).Scan(&count))
	assert.Equal(t, len(ids), count, "the same URL must be storable on distinct evidence rows")

	// And each is independently readable.
	for _, id := range ids {
		rows, err := ListEvidenceForArtifact(dm, id, "memory")
		require.NoError(t, err)
		require.Len(t, rows, 1)
		assert.Equal(t, shared, rows[0].ReferenceURL)
	}
}

// TestEvidenceReferenceURL_FreshDatabaseHasColumn pins that a brand-new
// database gets the column from BaseTables — not only upgraded ones. It also
// pins the column ORDER, which must match what ALTER TABLE produces on an
// upgraded table so the two database shapes agree.
func TestEvidenceReferenceURL_FreshDatabaseHasColumn(t *testing.T) {
	dm := newTestDM(t)

	rows, err := dm.QueryTracked(`PRAGMA table_info(evidence)`)
	require.NoError(t, err)
	defer rows.Close()

	var names []string
	for rows.Next() {
		var cid int
		var name, ctype string
		var notnull, pk int
		var dflt interface{}
		require.NoError(t, rows.Scan(&cid, &name, &ctype, &notnull, &dflt, &pk))
		names = append(names, name)
	}
	require.NoError(t, rows.Err())

	require.Contains(t, names, "reference_url")
	assert.Equal(t, "reference_url", names[len(names)-1],
		"reference_url must be the last column, matching the order ALTER TABLE ADD COLUMN produces")
}

// TestReferenceURL_LegacyDatabaseUpgrade is the upgrade-path pin, and the
// one that would have caught a real boot failure.
//
// A pre-feature database is created, seeded, then opened through the normal
// init path. Two things must hold: the column arrives, and the
// migrateEvidenceWorkType table rebuild — which hardcodes its destination
// table shape — copies every legacy row without an arity error and without
// dropping stored notes.
func TestReferenceURL_LegacyDatabaseUpgrade(t *testing.T) {
	dm, cleanup := seedLegacyEvidenceDB(t)
	defer cleanup()

	// The column arrived.
	require.True(t, dm.hasColumn("evidence", "reference_url"),
		"SafeMigrations must add reference_url to an upgraded database")

	// Reaching this line at all means the migrateEvidenceWorkType rebuild
	// succeeded: its guard only passes on a pre-'work' CHECK, so a table
	// seeded from legacyEvidenceWithoutReferenceURL exercises the copy.

	// Legacy rows survived with their content intact and no backfill.
	var notes sql.NullString
	var refURL sql.NullString
	require.NoError(t, dm.QueryRowTracked(
		`SELECT notes, reference_url FROM evidence WHERE id = 'ev-legacy-1'`).Scan(&notes, &refURL))
	assert.True(t, notes.Valid)
	assert.Equal(t, "a legacy observation", notes.String, "legacy notes must survive the rebuild")
	assert.False(t, refURL.Valid, "no backfill: a legacy row's reference_url stays NULL")

	// The NULL-notes row survived too.
	require.NoError(t, dm.QueryRowTracked(
		`SELECT notes FROM evidence WHERE id = 'ev-legacy-2'`).Scan(&notes))
	assert.False(t, notes.Valid, "a legacy NULL notes value must still be NULL, not become ''")

	var count int
	require.NoError(t, dm.QueryRowTracked(`SELECT COUNT(*) FROM evidence`).Scan(&count))
	assert.Equal(t, 2, count, "the rebuild must not lose or duplicate rows")

	// The upgraded database accepts a new reference and stores it exactly.
	_, err := dm.ExecTracked(
		`INSERT INTO memories (id, collection, content) VALUES (?, 'memories', 'x')`, 0, "refurl-upgraded")
	require.NoError(t, err)
	const ref = "https://example.com/added-after-upgrade?x=1"
	require.NoError(t, AddEvidence(dm, EvidenceInput{
		ArtifactID:   "refurl-upgraded",
		ArtifactType: "memory",
		Type:         "external_reference",
		SourceGroup:  "external",
		Strength:     0.6,
		CreatedBy:    "tester",
		CreatedAt:    time.Now(),
		ReferenceURL: ref,
	}))
	var got string
	require.NoError(t, dm.QueryRowTracked(
		`SELECT reference_url FROM evidence WHERE artifact_id = 'refurl-upgraded'`).Scan(&got))
	assert.Equal(t, ref, got)
}

// TestEvidenceReferenceURL_DoesNotAffectConfidence is the isolation pin, and
// the direct answer to "can storing a URL be mistaken for MPM having verified
// the referenced content?"
//
// Two otherwise-identical artifacts receive identical evidence, differing
// ONLY in whether a reference URL is attached. Their derived confidence must
// be identical. If URL presence could move confidence, then a URL would be
// functioning as corroboration — exactly the implication the feature forbids.
func TestEvidenceReferenceURL_DoesNotAffectConfidence(t *testing.T) {
	dm := newTestDM(t)

	withRef := "refurl-iso-with"
	withoutRef := "refurl-iso-without"
	for _, id := range []string{withRef, withoutRef} {
		_, err := dm.ExecTracked(
			`INSERT INTO memories (id, collection, content) VALUES (?, 'memories', 'same content')`, 0, id)
		require.NoError(t, err)
	}

	// Identical in every confidence-relevant dimension.
	base := func() EvidenceInput {
		return EvidenceInput{
			ArtifactType:       "memory",
			Type:               "external_reference",
			SourceGroup:        "external",
			Strength:           0.6,
			IndependenceFactor: 1.0,
			CreatedBy:          "tester",
			CreatedAt:          time.Unix(1700000000, 0),
		}
	}
	inWith := base()
	inWith.ArtifactID = withRef
	inWith.ReferenceURL = "https://example.com/cited-spec"

	inWithout := base()
	inWithout.ArtifactID = withoutRef

	require.NoError(t, AddEvidence(dm, inWith))
	require.NoError(t, AddEvidence(dm, inWithout))

	var confWith, confWithout float64
	require.NoError(t, dm.QueryRowTracked(
		`SELECT confidence FROM memories WHERE id = ?`, withRef).Scan(&confWith))
	require.NoError(t, dm.QueryRowTracked(
		`SELECT confidence FROM memories WHERE id = ?`, withoutRef).Scan(&confWithout))
	// Compared with a tolerance, not exactly: confidence carries a
	// time-decay term evaluated against the wall clock, so two recomputes
	// microseconds apart differ around 1e-13. The claim under test is "the
	// URL changes nothing", not "the clock does not move".
	assert.InDelta(t, confWithout, confWith, 1e-9,
		"an explicit reference URL must not change derived confidence")

	// The reasoning trace agrees, not just the cached number.
	expWith, err := ExplainConfidence(dm, withRef, "memory")
	require.NoError(t, err)
	expWithout, err := ExplainConfidence(dm, withoutRef, "memory")
	require.NoError(t, err)
	assert.InDelta(t, expWithout.Confidence, expWith.Confidence, 1e-9,
		"the explanation breakdown must agree")

	// And the recompute path — the one an operator triggers by hand —
	// reaches the same answer.
	require.NoError(t, RecomputeConfidence(dm, withRef, "memory", RecomputeReasonManual))
	require.NoError(t, RecomputeConfidence(dm, withoutRef, "memory", RecomputeReasonManual))
	require.NoError(t, dm.QueryRowTracked(
		`SELECT confidence FROM memories WHERE id = ?`, withRef).Scan(&confWith))
	require.NoError(t, dm.QueryRowTracked(
		`SELECT confidence FROM memories WHERE id = ?`, withoutRef).Scan(&confWithout))
	assert.InDelta(t, confWithout, confWith, 1e-9,
		"a manual recompute must not let reference_url move confidence")
}

// TestEvidenceReferenceURL_DoesNotAffectWorkVerification is the isolation
// pin for works, where verification state is derived rather than scored.
//
// Attaching a URL to a work's evidence must leave its verification exactly
// where identical evidence without a URL left it. A URL is not an outcome;
// treating it as one would mean MPM verified something by being handed a
// string.
func TestEvidenceReferenceURL_DoesNotAffectWorkVerification(t *testing.T) {
	dm := newTestDM(t)

	workA, err := dm.AddWork("reference isolation A", "body", "session-refurl")
	require.NoError(t, err)
	workB, err := dm.AddWork("reference isolation B", "body", "session-refurl")
	require.NoError(t, err)

	mk := func(workID, refURL string) EvidenceInput {
		return EvidenceInput{
			ArtifactID:         workID,
			ArtifactType:       "work",
			Type:               "observation",
			SourceGroup:        "filesystem",
			Strength:           0.4,
			IndependenceFactor: 1.0,
			CreatedBy:          "tester",
			CreatedAt:          time.Unix(1700000000, 0),
			ReferenceURL:       refURL,
		}
	}
	require.NoError(t, AddEvidence(dm, mk(workA.ID, "https://example.com/pr-1234")))
	require.NoError(t, AddEvidence(dm, mk(workB.ID, "")))

	verA, err := dm.DeriveWorkVerification(workA.ID)
	require.NoError(t, err)
	verB, err := dm.DeriveWorkVerification(workB.ID)
	require.NoError(t, err)
	assert.Equal(t, verB, verA,
		"a reference URL on work evidence must not move derived verification")

	var storedA, storedB string
	require.NoError(t, dm.QueryRowTracked(
		`SELECT verification FROM works WHERE id = ?`, workA.ID).Scan(&storedA))
	require.NoError(t, dm.QueryRowTracked(
		`SELECT verification FROM works WHERE id = ?`, workB.ID).Scan(&storedB))
	assert.Equal(t, storedB, storedA)
}

// TestEvidenceReferenceURL_NotReadByConfidenceLoaders pins the mechanism
// behind the isolation guarantee, so it cannot be undone silently.
//
// The isolation above is a behavioural claim about confidence output. This
// pins WHY it holds: the loaders the confidence machinery uses do not select
// the column at all. If someone later adds reference_url to one of these
// SELECTs, this goes red and points at the coupling before it can matter.
func TestEvidenceReferenceURL_NotReadByConfidenceLoaders(t *testing.T) {
	src, err := os.ReadFile("evidence_store.go")
	require.NoError(t, err)

	// Locate loadEvidenceForRecompute's body and assert it never names the
	// column. Scoped to that function so an unrelated query elsewhere in the
	// file is not mistaken for a violation.
	const fn = "func loadEvidenceForRecompute("
	start := strings.Index(string(src), fn)
	require.NotEqual(t, -1, start, "loadEvidenceForRecompute should still exist in evidence_store.go")
	body := string(src)[start:]
	if next := strings.Index(body, "\nfunc "); next > 0 {
		body = body[:next]
	}
	assert.NotContains(t, body, "reference_url",
		"the confidence loader must not select reference_url; URL presence must not "+
			"participate in confidence derivation")
}
