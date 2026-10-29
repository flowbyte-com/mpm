// retention_deregistration_test.go — the registry-CHANGE case: what
// happens when a directive leaves the registry.
//
// WHY THIS IS A SEPARATE FILE
// ---------------------------
// De-registration is a CODE change: an entry is removed from the
// SeedDirectives slice. That is the only way a directive ever becomes an
// orphan, so the only faithful way to test the orphan report is to remove
// an entry and see what the report says.
//
// The registry slice is the single source of truth for membership, and
// IsRegistryDirectiveID reads it LIVE — so removing an entry here really
// does de-register it, for the report and for everything else. Every
// mutation here is restored in a t.Cleanup.
//
// Disposable databases only.
package seed_test

import (
	"database/sql"
	"strings"
	"testing"

	internal "github.com/flowbyte-com/mpm-core"
	"github.com/flowbyte-com/mpm-core/seed"
)

// deRegister removes one registry entry for the duration of the test and
// restores it afterwards.
//
// The removal is what a code change would do: the entry leaves the
// SeedDirectives slice, and every other part of the substrate sees the
// registry without it.
func deRegister(t *testing.T, stableID string) {
	t.Helper()
	saved := seed.SeedDirectives
	t.Cleanup(func() { seed.SeedDirectives = saved })

	var kept []seed.SeedDirective
	found := false
	for _, sd := range seed.SeedDirectives {
		if sd.StableID == stableID {
			found = true
			continue
		}
		kept = append(kept, sd)
	}
	if !found {
		t.Fatalf("%s is not in the registry", stableID)
	}
	seed.SeedDirectives = kept
}

// insertRow writes a directive row with the given metadata.
func insertRow(t *testing.T, db *sql.DB, id, content, metadata string) {
	t.Helper()
	if _, err := db.Exec(`
		INSERT INTO memories (id, collection, content, metadata, is_prime_directive, weight,
		                      reinforcement_count, is_long_term, created_at, updated_at)
		VALUES (?, 'directives', ?, ?, 1, 10, 0, 0, strftime('%s','now'), strftime('%s','now'))`,
		id, content, metadata); err != nil {
		t.Fatalf("insert %s: %v", id, err)
	}
}

// TestDeRegisteredDirective_IsReportedAsAnOrphan is the registry-change
// case, end to end.
//
// A directive leaves the registry. Its row is still on disk. The report
// must name it, so the operator can decide.
func TestDeRegisteredDirective_IsReportedAsAnOrphan(t *testing.T) {
	dm := internal.NewTestDM(t)
	if _, err := seed.ApplyDirectives(dm); err != nil {
		t.Fatalf("seed directives: %v", err)
	}
	// Pick one registry entry and de-register it.
	var victim string
	for _, sd := range seed.SeedDirectives {
		victim = sd.StableID
		break
	}
	deRegister(t, victim)

	// Before the change, it was a registry member.
	if seed.IsRegistryDirectiveID(victim) {
		t.Fatalf("%s is still a registry member after de-registration", victim)
	}

	orphans, err := seed.OrphanedDirectives(dm)
	if err != nil {
		t.Fatalf("orphaned directives: %v", err)
	}
	var found bool
	for _, o := range orphans {
		if o.ID == victim {
			found = true
		}
	}
	if !found {
		t.Fatalf("%s was not reported as an orphan: %v", victim, orphans)
	}
}

// TestDeRegisteredDirective_IsStillProtectedFromPruning is the other half.
//
// De-registration makes a directive ELIGIBLE for the operator's explicit
// cleanup. It does NOT make it a candidate for automatic reclamation: the
// policy says MPM never reclaims a directive automatically, whatever the
// registry status.
//
// The pruning protections live in the core package and are tested there.
// What is pinned here is that the de-registration itself changes nothing
// about the row's protection — the row is still a directive row, and the
// directive exclusion is a predicate on the row, not on the registry.
func TestDeRegisteredDirective_IsStillProtectedFromPruning(t *testing.T) {
	dm := internal.NewTestDM(t)
	if _, err := seed.ApplyDirectives(dm); err != nil {
		t.Fatalf("seed directives: %v", err)
	}

	var victim string
	for _, sd := range seed.SeedDirectives {
		victim = sd.StableID
		break
	}
	deRegister(t, victim)

	// The row is still a directive row by the canonical predicate.
	var isDirective int
	if err := dm.SQLDB().QueryRow(`
		SELECT COUNT(*) FROM memories
		WHERE id = ? AND deleted_at IS NULL
		  AND (collection = 'directives' OR COALESCE(is_prime_directive, 0) = 1)`,
		victim).Scan(&isDirective); err != nil {
		t.Fatalf("check directive predicate: %v", err)
	}
	if isDirective != 1 {
		t.Errorf("the de-registered row is no longer recognised as a directive — " +
			"de-registration must not change how the row is protected")
	}
}

// TestDeRegisteredDirective_ReportsRegistryCrossReferences is the
// cross-reference safety.
//
// The baseline nightly compaction task names a directive as the one the
// agent reads when the wake fires. De-registering that directive does not
// break the task on THIS database (the row is still there) — but on a
// fresh install the task seed would find no directive and skip the task.
//
// The operator should see that before they act on an orphan.
func TestDeRegisteredDirective_ReportsRegistryCrossReferences(t *testing.T) {
	dm := internal.NewTestDM(t)
	if _, err := seed.ApplyDirectives(dm); err != nil {
		t.Fatalf("seed directives: %v", err)
	}

	// Find a directive that a seeded scheduled task points at.
	var referenced string
	for _, st := range seed.SeedScheduledTasks {
		referenced = st.DirectiveID
		break
	}
	if referenced == "" {
		t.Skip("no seeded scheduled task references a directive")
	}
	deRegister(t, referenced)

	orphans, err := seed.OrphanedDirectives(dm)
	if err != nil {
		t.Fatalf("orphaned directives: %v", err)
	}
	var found *seed.OrphanedDirective
	for i := range orphans {
		if orphans[i].ID == referenced {
			found = &orphans[i]
		}
	}
	if found == nil {
		t.Fatalf("%s was not reported as an orphan: %v", referenced, orphans)
	}
	if len(found.ReferencedBy) == 0 {
		t.Errorf("%s is still referenced by a seeded scheduled task, but the report "+
			"does not say so — the operator would remove policy a live task depends on",
			referenced)
	}
	// And the rendering carries it.
	if got := seed.FormatOrphanedDirective(*found); got == "" {
		t.Error("empty rendering")
	} else if !contains(got, "referenced by") {
		t.Errorf("rendering %q does not mention the cross-reference", got)
	}
}

// TestDeRegisteredDirective_IsNotSilentlyReSeeded pins the other half of
// the contract: a de-registered directive is not re-created by the seed.
//
// The seed applies the registry. If the directive is no longer in the
// registry, the seed does not re-create it — the row that is already there
// is left alone. The seed never deletes either; that is the report's job,
// not the seed's.
func TestDeRegisteredDirective_IsNotSilentlyReSeeded(t *testing.T) {
	dm := internal.NewTestDM(t)
	summary, err := seed.ApplyDirectives(dm)
	if err != nil {
		t.Fatalf("seed directives: %v", err)
	}
	var victim string
	for _, sd := range seed.SeedDirectives {
		victim = sd.StableID
		break
	}
	deRegister(t, victim)

	summary, err = seed.ApplyDirectives(dm)
	if err != nil {
		t.Fatalf("re-seed: %v", err)
	}
	for _, id := range summary.Created {
		if id == victim {
			t.Errorf("the seed re-created the de-registered directive %s — "+
				"de-registration must not silently resurrect it", victim)
		}
	}
	// And the row is still on disk: the seed never deletes.
	var n int
	if err := dm.SQLDB().QueryRow(
		`SELECT COUNT(*) FROM memories WHERE id = ?`, victim).Scan(&n); err != nil {
		t.Fatalf("count victim row: %v", err)
	}
	if n != 1 {
		t.Errorf("the de-registered directive's row was deleted by the seed — " +
			"the seed never deletes; cleanup is an explicit operator act")
	}
}

// TestUserAuthoredDirective_IsNeverAnOrphan is the safety requirement,
// tested at the source of truth.
//
// A directive the operator wrote was never in the registry. De-registering
// registry entries cannot make it an orphan.
func TestUserAuthoredDirective_IsNeverAnOrphan(t *testing.T) {
	dm := internal.NewTestDM(t)
	if _, err := seed.ApplyDirectives(dm); err != nil {
		t.Fatalf("seed directives: %v", err)
	}
	db := dm.SQLDB()

	// An operator-authored directive: collection='directives', no marker.
	insertRow(t, db, "operator-authored", "the operator's own policy", `{"scope":"global"}`)

	// De-register everything the registry has.
	saved := seed.SeedDirectives
	t.Cleanup(func() { seed.SeedDirectives = saved })
	seed.SeedDirectives = nil

	orphans, err := seed.OrphanedDirectives(dm)
	if err != nil {
		t.Fatalf("orphaned directives: %v", err)
	}
	for _, o := range orphans {
		if o.ID == "operator-authored" {
			t.Errorf("an operator-authored directive was reported as an orphan — " +
				"it was never in the registry and can never be one")
		}
	}
}

// contains is a tiny helper so the test does not import strings for one call.
func contains(haystack, needle string) bool {
	return len(haystack) >= len(needle) && strings.Contains(haystack, needle)
}
