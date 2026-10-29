// retention_test.go — the registry-membership model for directive
// retention (issue #59, Option B), tested at the seed layer.
//
// THE CONTRACT
// ------------
//   - Registry membership is the authority for "is this row still standing
//     policy?", and it is a function of the SeedDirectives slice and
//     nothing else.
//   - A row the canonical seeder wrote is recognisable by its provenance
//     marker; a row the operator wrote is not.
//   - A registry-backed row whose ID has left the registry is an ORPHAN: it
//     is ELIGIBLE for explicit, auditable cleanup.
//   - Eligibility is not authorisation. Nothing here deletes anything.
//
// The generic-pruning protections live in the core package — see
// internal/core/directive_retention_policy_test.go.
//
// Disposable databases only.
package seed_test

import (
	"database/sql"
	"encoding/json"
	"fmt"
	"testing"

	internal "github.com/flowbyte-com/mpm-core"
	"github.com/flowbyte-com/mpm-core/seed"
)

// seedTestDM opens a disposable database with the baseline applied.
func seedTestDM(t *testing.T) (*sql.DB, seed.SeedSummary) {
	t.Helper()
	dm := internal.NewTestDM(t)

	summary, err := seed.ApplyDirectives(dm)
	if err != nil {
		t.Fatalf("seed directives: %v", err)
	}
	return dm.SQLDB(), summary
}

// insertDirectiveRow writes a directive row with the given metadata.
func insertDirectiveRow(t *testing.T, db *sql.DB, id, content, metadata string) {
	t.Helper()
	if _, err := db.Exec(`
		INSERT INTO memories (id, collection, content, metadata, is_prime_directive, weight,
		                      reinforcement_count, is_long_term, created_at, updated_at)
		VALUES (?, 'directives', ?, ?, 1, 10, 0, 0, strftime('%s','now'), strftime('%s','now'))`,
		id, content, metadata); err != nil {
		t.Fatalf("insert directive %s: %v", id, err)
	}
}

// ── 1. Registry membership ────────────────────────────────────────────────

// TestRegistryDirectiveIDs_MatchesTheRegistry is the tautology that keeps
// the two from drifting: every registry entry is a registry member, and
// nothing else is.
func TestRegistryDirectiveIDs_MatchesTheRegistry(t *testing.T) {
	ids := seed.RegistryDirectiveIDs()
	if len(ids) != len(seed.SeedDirectives) {
		t.Fatalf("RegistryDirectiveIDs has %d entries, want %d", len(ids), len(seed.SeedDirectives))
	}
	for _, sd := range seed.SeedDirectives {
		if !seed.IsRegistryDirectiveID(sd.StableID) {
			t.Errorf("%s is in the registry but IsRegistryDirectiveID says no", sd.StableID)
		}
	}
	if seed.IsRegistryDirectiveID("not-in-the-registry") {
		t.Error("IsRegistryDirectiveID accepted an id that is not in the registry")
	}
}

// ── 2. The provenance marker ──────────────────────────────────────────────

// TestIsRegistryBackedDirective_RecognisesOnlySeededRows pins the
// distinction the whole orphan report rests on.
func TestIsRegistryBackedDirective_RecognisesOnlySeededRows(t *testing.T) {
	seeded, err := json.Marshal(map[string]interface{}{
		"is_prime_directive": 1,
		"scope":              "global",
		"provenance": map[string]interface{}{
			"agent": "mpm_ops_init", "compute": "absolute",
			"model": "direct", "persona": "operator",
			"source": seed.DirectiveProvenanceSource,
		},
	})
	if err != nil {
		t.Fatalf("marshal metadata: %v", err)
	}
	var seededMeta map[string]interface{}
	if err := json.Unmarshal(seeded, &seededMeta); err != nil {
		t.Fatalf("unmarshal metadata: %v", err)
	}
	if !seed.IsRegistryBackedDirective(seededMeta) {
		t.Error("a row carrying the seeder's provenance marker was not recognised")
	}

	// An operator-authored directive: same collection, no marker.
	operatorMeta := map[string]interface{}{"scope": "global"}
	if seed.IsRegistryBackedDirective(operatorMeta) {
		t.Error("an operator-authored directive was treated as registry-backed — " +
			"it was never in the registry and can never be an orphan")
	}

	// A nil metadata.
	if seed.IsRegistryBackedDirective(nil) {
		t.Error("a row with no metadata was treated as registry-backed")
	}
}

// The marker the seeder WRITES must be the marker the reader LOOKS FOR.
// This is the drift guard: change the writer without changing the reader
// and every seeded row silently stops being recognisable.
func TestSeededRowsCarryTheMarkerTheReaderLooksFor(t *testing.T) {
	db, summary := seedTestDM(t)
	if len(summary.Created) == 0 {
		t.Fatal("the baseline was not created")
	}
	for _, sd := range seed.SeedDirectives {
		var metadata string
		if err := db.QueryRow(`SELECT metadata FROM memories WHERE id = ?`, sd.StableID).Scan(&metadata); err != nil {
			t.Fatalf("read metadata for %s: %v", sd.StableID, err)
		}
		var meta map[string]interface{}
		if err := json.Unmarshal([]byte(metadata), &meta); err != nil {
			t.Fatalf("parse metadata for %s: %v", sd.StableID, err)
		}
		if !seed.IsRegistryBackedDirective(meta) {
			t.Errorf("the seeded row %s does not carry the marker the reader looks for "+
				"(%q) — the writer and the reader have drifted", sd.StableID, seed.DirectiveProvenanceSource)
		}
	}
}

// ── 3. The orphan report ──────────────────────────────────────────────────

// TestOrphanedDirectives_EmptyOnAFreshInstall is the fresh-install case:
// nothing has left the registry, so there are no candidates.
func TestOrphanedDirectives_EmptyOnAFreshInstall(t *testing.T) {
	db, _ := seedTestDM(t)
	dm := wrapDB(db)

	orphans, err := seed.OrphanedDirectives(dm)
	if err != nil {
		t.Fatalf("orphaned directives: %v", err)
	}
	if len(orphans) != 0 {
		t.Errorf("a fresh install reported %d orphans: %v", len(orphans), orphans)
	}
}

// TestOrphanedDirectives_DetectsARegistryRemoval is the registry-change
// case.
//
// Removing an entry from SeedDirectives is a CODE change, so the substrate
// cannot observe the moment of de-registration. What it CAN observe is the
// marker the seeder wrote: a row that carries it and whose ID is no longer
// in the registry is a former standing policy.
func TestOrphanedDirectives_DetectsARegistryRemoval(t *testing.T) {
	db, _ := seedTestDM(t)
	dm := wrapDB(db)

	// A row that LOOKS like a seeded one but whose ID is not in the
	// registry — exactly what a de-registered directive looks like.
	meta := fmt.Sprintf(`{"is_prime_directive":1,"scope":"global","provenance":{"agent":"mpm_ops_init","compute":"absolute","model":"direct","persona":"operator","source":%q}}`,
		seed.DirectiveProvenanceSource)
	insertDirectiveRow(t, db, "mpm-seed-removed-from-registry", "the de-registered policy", meta)

	orphans, err := seed.OrphanedDirectives(dm)
	if err != nil {
		t.Fatalf("orphaned directives: %v", err)
	}
	if len(orphans) != 1 {
		t.Fatalf("orphans = %d, want 1: %v", len(orphans), orphans)
	}
	if orphans[0].ID != "mpm-seed-removed-from-registry" {
		t.Errorf("orphan ID = %q, want mpm-seed-removed-from-registry", orphans[0].ID)
	}
	if orphans[0].Content != "the de-registered policy" {
		t.Errorf("orphan content = %q — the operator must be able to SEE what they would remove",
			orphans[0].Content)
	}
}

// TestOrphanedDirectives_NeverReportsUserAuthoredDirectives is the
// "user-authored directives are not accidentally deleted merely because
// registry membership changes" requirement.
//
// A user-authored directive is not in the registry and never was. Registry
// membership says nothing about it, so it must never be reported as an
// orphan — and therefore never becomes eligible for cleanup.
func TestOrphanedDirectives_NeverReportsUserAuthoredDirectives(t *testing.T) {
	db, _ := seedTestDM(t)
	dm := wrapDB(db)

	// An operator-authored directive: collection='directives', no marker.
	insertDirectiveRow(t, db, "operator-authored", "the operator's own policy",
		`{"scope":"global"}`)

	// A legacy directive: the legacy column marker, no provenance marker.
	if _, err := db.Exec(`
		INSERT INTO memories (id, collection, content, is_prime_directive, weight,
		                      reinforcement_count, is_long_term, created_at, updated_at)
		VALUES ('legacy-directive', 'memories', 'a legacy directive', 1, 10, 0, 0,
		        strftime('%s','now'), strftime('%s','now'))`); err != nil {
		t.Fatalf("insert legacy directive: %v", err)
	}

	orphans, err := seed.OrphanedDirectives(dm)
	if err != nil {
		t.Fatalf("orphaned directives: %v", err)
	}
	for _, o := range orphans {
		if o.ID == "operator-authored" || o.ID == "legacy-directive" {
			t.Errorf("%s was reported as an orphan — a directive the registry never "+
				"claimed is never a candidate for cleanup", o.ID)
		}
	}
}

// TestOrphanedDirectives_ExcludesSoftDeletedRows — a row the operator has
// already soft-deleted is out of the read path; there is nothing left to
// decide about it.
func TestOrphanedDirectives_ExcludesSoftDeletedRows(t *testing.T) {
	db, _ := seedTestDM(t)
	dm := wrapDB(db)

	meta := fmt.Sprintf(`{"provenance":{"source":%q}}`, seed.DirectiveProvenanceSource)
	insertDirectiveRow(t, db, "mpm-seed-removed-and-deleted", "gone already", meta)
	if _, err := db.Exec(
		`UPDATE memories SET deleted_at = CAST(strftime('%s','now') AS INTEGER) WHERE id = ?`,
		"mpm-seed-removed-and-deleted"); err != nil {
		t.Fatalf("soft-delete: %v", err)
	}

	orphans, err := seed.OrphanedDirectives(dm)
	if err != nil {
		t.Fatalf("orphaned directives: %v", err)
	}
	for _, o := range orphans {
		if o.ID == "mpm-seed-removed-and-deleted" {
			t.Error("a soft-deleted row was reported as an orphan — it is already out " +
				"of the read path")
		}
	}
}

// TestOrphanedDirectives_IsReadOnly is the eligibility/authorisation
// distinction, expressed as a test: calling the report must not change
// anything.
//
// The report is called by an operator command. If it deleted, revived or
// re-seeded anything, it would be an automatic cleanup mechanism — which
// the policy forbids.
func TestOrphanedDirectives_IsReadOnly(t *testing.T) {
	db, _ := seedTestDM(t)
	dm := wrapDB(db)

	meta := fmt.Sprintf(`{"provenance":{"source":%q}}`, seed.DirectiveProvenanceSource)
	insertDirectiveRow(t, db, "mpm-seed-removed-from-registry", "the de-registered policy", meta)

	before := snapshotMemories(t, db)
	for i := 0; i < 3; i++ {
		if _, err := seed.OrphanedDirectives(dm); err != nil {
			t.Fatalf("orphaned directives: %v", err)
		}
	}
	after := snapshotMemories(t, db)

	if len(before) != len(after) {
		t.Fatalf("row count changed: %d -> %d", len(before), len(after))
	}
	for id, row := range before {
		if after[id] != row {
			t.Errorf("row %s changed: %q -> %q", id, row, after[id])
		}
	}
}

// snapshotMemories captures every row's identity and content.
func snapshotMemories(t *testing.T, db *sql.DB) map[string]string {
	t.Helper()
	rows, err := db.Query(`SELECT id, content, COALESCE(deleted_at, -1) FROM memories ORDER BY id`)
	if err != nil {
		t.Fatalf("snapshot: %v", err)
	}
	defer rows.Close()
	out := map[string]string{}
	for rows.Next() {
		var id, content string
		var deleted int64
		if err := rows.Scan(&id, &content, &deleted); err != nil {
			t.Fatalf("snapshot scan: %v", err)
		}
		out[id] = fmt.Sprintf("%s|%d", content, deleted)
	}
	return out
}

// TestFormatOrphanedDirective_ShowsTheIDAndAPreview — the report shows the
// operator what they would be acting on, and a report is a log, so it
// carries a preview rather than a whole document.
func TestFormatOrphanedDirective_ShowsTheIDAndAPreview(t *testing.T) {
	long := ""
	for i := 0; i < 50; i++ {
		long += "a very long directive body. "
	}
	out := seed.FormatOrphanedDirective(seed.OrphanedDirective{
		ID: "mpm-seed-removed", Content: long,
	})
	if out == "" {
		t.Fatal("empty rendering")
	}
	if len(out) > 200 {
		t.Errorf("rendering is %d bytes — a report line should carry a preview", len(out))
	}
}

// wrapDB adapts a *sql.DB to the narrow interface the seed package takes,
// so the tests can drive the query directly.
type dbOnly struct{ db *sql.DB }

func (d dbOnly) SQLDB() *sql.DB { return d.db }

func wrapDB(db *sql.DB) dbOnly { return dbOnly{db: db} }
