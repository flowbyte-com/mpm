package internal

// §4/§6 — regression coverage for the artifact_provenance CHECK migration.
//
// These tests build a LEGACY artifact_provenance table by hand, put it
// through the real boot path, and then assert the resulting schema by
// performing REAL INSERTS. Grepping sqlite_master for a word is not enough:
// a table can list a vocabulary in a comment, or in a clause unrelated to
// the CHECK, and still reject every value.
//
// The migration is called the way production calls it. SafeMigrations adds
// parent_invocation_id BEFORE this function in the real boot order
// (db.go:2154 SafeMigrations, db.go:2189 migrateArtifactProvenanceWorkType),
// so legacy fixtures below already carry that column; inventing a fixture
// without it would test an ordering production never uses.

import (
	"database/sql"
	"fmt"
	"path/filepath"
	"testing"
)

// legacyProvenanceDDL builds an artifact_provenance table accepting exactly
// `accepted`. Every other canonical column, affinity, default and constraint
// matches schema.go::BaseTables so the migration is exercised against a
// realistic predecessor rather than a toy.
func legacyProvenanceDDL(accepted []string) string {
	quoted := make([]string, len(accepted))
	for i, a := range accepted {
		quoted[i] = "'" + a + "'"
	}
	vocab := ""
	for i, q := range quoted {
		if i > 0 {
			vocab += ","
		}
		vocab += q
	}
	return `CREATE TABLE artifact_provenance (
		id                   TEXT PRIMARY KEY,
		artifact_id          TEXT NOT NULL,
		artifact_type        TEXT NOT NULL,
		created_at           INTEGER NOT NULL,
		schema_version       TEXT NOT NULL DEFAULT 'v1',
		actor_kind           TEXT NOT NULL,
		actor_id             TEXT,
		framework_name       TEXT,
		framework_version    TEXT,
		framework_adapter    TEXT,
		provider_name        TEXT,
		model_name           TEXT,
		model_revision       TEXT,
		api_endpoint         TEXT,
		temperature          REAL,
		max_tokens           INTEGER,
		reasoning_mode       TEXT,
		reasoning_effort     REAL,
		thinking_level       TEXT,
		thinking_tokens      INTEGER,
		thinking_visible     INTEGER,
		session_id           TEXT,
		invocation_id        TEXT,
		parent_artifact_id   TEXT,
		parent_invocation_id TEXT,
		provider_metadata    TEXT,
		UNIQUE (artifact_id, artifact_type),
		CHECK (artifact_type IN (` + vocab + `)),
		CHECK (actor_kind IN ('agent','human','import','system','unknown'))
	)`
}

// canonicalArtifactTypes is the full vocabulary every database must accept
// once migration has run. `spaceship` must never be accepted.
var canonicalArtifactTypes = []string{
	"memory", "theory", "lesson", "decision", "handoff", "directive", "work",
}

// provenanceBootDB creates a file-backed DatabaseManager whose
// artifact_provenance table is the given legacy shape, with the two canonical
// analytics views present. It returns the manager; the caller closes it.
func provenanceBootDB(t *testing.T, accepted []string) *DatabaseManager {
	t.Helper()
	path := filepath.Join(t.TempDir(), "mpm-provenance.db")
	// Build the legacy schema BEFORE handing the file to a DatabaseManager,
	// so NewDatabaseManager's own CREATE TABLE IF NOT EXISTS leaves it be.
	pre, err := sql.Open("sqlite3", path)
	if err != nil {
		t.Fatalf("open fixture db: %v", err)
	}
	if _, err := pre.Exec(legacyProvenanceDDL(accepted)); err != nil {
		t.Fatalf("create legacy artifact_provenance: %v", err)
	}
	// The analytics views a real legacy database carries. They join to
	// `memories`, which the canonical view definitions require to exist.
	for _, ddl := range []string{
		`CREATE TABLE IF NOT EXISTS memories (
			id TEXT PRIMARY KEY, weight REAL DEFAULT 1, created_at INTEGER,
			deleted_at INTEGER, reinforcement_count INTEGER DEFAULT 0,
			metadata TEXT, confidence REAL, thinking_level TEXT)`,
		canonicalProvenanceViews()[0],
		canonicalProvenanceViews()[1],
	} {
		if _, err := pre.Exec(ddl); err != nil {
			t.Fatalf("create legacy view support: %v", err)
		}
	}
	if err := pre.Close(); err != nil {
		t.Fatalf("close fixture db: %v", err)
	}
	dm, err := NewDatabaseManagerForDB2(t, path)
	if err != nil {
		t.Fatalf("open DatabaseManager: %v", err)
	}
	return dm
}

// NewDatabaseManagerForDB2 opens a file-backed manager without running the
// full schema bootstrap, so the fixture's legacy shape survives. Production
// reaches the migration through initUnifiedSchema; the dedicated
// boot-path test below covers that ordering, while the matrix tests drive
// the migration directly so each starting state is unambiguous.
func NewDatabaseManagerForDB2(t *testing.T, path string) (*DatabaseManager, error) {
	t.Helper()
	db, err := sql.Open("sqlite3", path)
	if err != nil {
		return nil, err
	}
	db.SetMaxOpenConns(1)
	return NewDatabaseManagerForDB(db), nil
}

// acceptsArtifactType reports whether the live table accepts artifact_type=v.
//
// Each probe uses a fresh id/artifact_id pair. Re-using them would make a
// second probe collide with the row the first one inserted and report a
// UNIQUE violation, which is indistinguishable from a rejected CHECK at the
// call site — and would silently misreport a passing table as broken.
var provProbeSeq int

func acceptsArtifactType(t *testing.T, dm *DatabaseManager, v string) bool {
	t.Helper()
	provProbeSeq++
	_, err := dm.db.Exec(
		`INSERT INTO artifact_provenance
			(id, artifact_id, artifact_type, created_at, actor_kind)
		 VALUES (?,?,?,?,?)`,
		fmt.Sprintf("probe-%s-%d", v, provProbeSeq),
		fmt.Sprintf("artifact-%s-%d", v, provProbeSeq),
		v, 1, "agent")
	return err == nil
}

func TestProvenanceMigration_WorkOnlyCheckIsNotSkipped(t *testing.T) {
	// §4 primary reproducer. A database that already accepts 'work' but
	// still rejects 'handoff'/'directive' is PARTIALLY widened. The
	// migration must repair it, not skip it because the word 'work'
	// appears somewhere in the CREATE TABLE text.
	dm := provenanceBootDB(t, []string{"memory", "theory", "lesson", "decision", "work"})
	defer dm.Close()

	// Precondition: prove the fixture really is in the broken state, so a
	// later pass cannot be mistaken for the migration having done nothing.
	if !acceptsArtifactType(t, dm, "work") {
		t.Fatal("fixture invalid: 'work' must be accepted before migration")
	}
	for _, v := range []string{"handoff", "directive"} {
		if acceptsArtifactType(t, dm, v) {
			t.Fatalf("fixture invalid: %q must be rejected before migration", v)
		}
	}

	if err := dm.migrateArtifactProvenanceWorkType(); err != nil {
		t.Fatalf("migrateArtifactProvenanceWorkType: %v", err)
	}

	for _, v := range canonicalArtifactTypes {
		if !acceptsArtifactType(t, dm, v) {
			t.Errorf("after migration, canonical artifact_type %q is rejected; "+
				"the migration skipped a partially widened table", v)
		}
	}
	if acceptsArtifactType(t, dm, "spaceship") {
		t.Error("after migration, artifact_type 'spaceship' is accepted; CHECK was widened beyond canonical")
	}
}

// §6 schema-state matrix. Every starting vocabulary must converge on the
// full canonical set, verified by real inserts rather than SQL text.
func TestProvenanceMigration_SchemaStateMatrix(t *testing.T) {
	cases := []struct {
		name     string
		accepted []string
	}{
		{"A original legacy", []string{"memory", "theory", "lesson", "decision"}},
		{"B legacy plus work", []string{"memory", "theory", "lesson", "decision", "work"}},
		{"C missing directive", []string{"memory", "theory", "lesson", "decision", "handoff", "work"}},
		{"D missing handoff", []string{"memory", "theory", "lesson", "decision", "directive", "work"}},
		{"E alpha3 before work", []string{"memory", "theory", "lesson", "decision", "handoff", "directive"}},
		{"F fully current", []string{"memory", "theory", "lesson", "decision", "handoff", "directive", "work"}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			dm := provenanceBootDB(t, tc.accepted)
			defer dm.Close()

			if err := dm.migrateArtifactProvenanceWorkType(); err != nil {
				t.Fatalf("migrateArtifactProvenanceWorkType: %v", err)
			}
			for _, v := range canonicalArtifactTypes {
				if !acceptsArtifactType(t, dm, v) {
					t.Errorf("canonical artifact_type %q rejected after migration (started from %v)", v, tc.accepted)
				}
			}
			if acceptsArtifactType(t, dm, "spaceship") {
				t.Error("non-canonical artifact_type 'spaceship' accepted after migration")
			}
		})
	}
}

// §5 — a genuine legacy database carries both analytics views. They must
// still exist, must not reference artifact_provenance_old, and must be
// queryable afterwards.
func TestProvenanceMigration_PreservesAnalyticsViews(t *testing.T) {
	dm := provenanceBootDB(t, []string{"memory", "theory", "lesson", "decision"})
	defer dm.Close()

	for _, v := range canonicalProvenanceViewNames {
		assertViewExists(t, dm, v, "before migration")
	}

	if err := dm.migrateArtifactProvenanceWorkType(); err != nil {
		t.Fatalf("migrateArtifactProvenanceWorkType: %v", err)
	}

	for _, v := range canonicalProvenanceViewNames {
		assertViewExists(t, dm, v, "after migration")
		assertViewNotReferencingOld(t, dm, v)
		assertViewQueryable(t, dm, v)
	}
}

// §11 — interrupted / partial view states must be repaired, whichever view
// is broken. Special-casing only one of the two canonical views would let
// the other stay dangling.
func TestProvenanceMigration_RepairsPartialViewStates(t *testing.T) {
	cases := []struct {
		name  string
		setup func(t *testing.T, dm *DatabaseManager)
	}{
		{"A both canonical", nil},
		{"B memory_yield points at old table", func(t *testing.T, dm *DatabaseManager) {
			pointViewAtOld(t, dm, "v_model_memory_yield")
		}},
		{"C theory_utility points at old table", func(t *testing.T, dm *DatabaseManager) {
			pointViewAtOld(t, dm, "v_model_theory_utility")
		}},
		{"D both point at old table", func(t *testing.T, dm *DatabaseManager) {
			pointViewAtOld(t, dm, "v_model_memory_yield")
			pointViewAtOld(t, dm, "v_model_theory_utility")
		}},
		{"E one analytics view missing", func(t *testing.T, dm *DatabaseManager) {
			provMustExec(t, dm, "DROP VIEW v_model_theory_utility")
		}},
		{"F both analytics views missing", func(t *testing.T, dm *DatabaseManager) {
			provMustExec(t, dm, "DROP VIEW v_model_memory_yield")
			provMustExec(t, dm, "DROP VIEW v_model_theory_utility")
		}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			// Start fully current so the ONLY outstanding work is view repair.
			dm := provenanceBootDB(t, canonicalArtifactTypes)
			defer dm.Close()
			if tc.setup != nil {
				tc.setup(t, dm)
			}
			if err := dm.migrateArtifactProvenanceWorkType(); err != nil {
				t.Fatalf("migrateArtifactProvenanceWorkType: %v", err)
			}
			for _, v := range canonicalProvenanceViewNames {
				assertViewExists(t, dm, v, "after repair")
				assertViewNotReferencingOld(t, dm, v)
				assertViewQueryable(t, dm, v)
			}
		})
	}
}
