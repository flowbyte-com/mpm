// directive_retention_test.go — pin the retention policy for standing
// directives (issue #59).
//
// Directives are policy, not memories awaiting reinforcement. Their read
// path never writes last_accessed_at, so every access-based retention
// rule ("reclaim rows never accessed") matches them forever. Before this
// policy they were soft-deleted by MaintenanceCycle's
// AutoPrunePolicy(NeverAccessedMaxDays: 30) on every run, then revived at
// the next boot by seed.ApplyDirectives — churn that writes revisions and
// audit rows, and that can lose operator edits if gc --purge hard-deletes
// the row between the two.
//
// The contract these tests pin:
//   - directive rows are exempt from access-based retention in BOTH prune
//     paths (AutoPrunePolicy never-accessed + low-weight, and
//     PruneNeverAccessed);
//   - the exemption recognises both canonical identifiers
//     (collection='directives' and is_prime_directive=1);
//   - operator edits survive;
//   - ordinary memories are pruned exactly as before.
package internal

import (
	"database/sql"
	"strings"
	"testing"

	"github.com/flowbyte-com/mpm-core/seed"
)

// isLive reports whether the row is not soft-deleted (sentinel-agnostic).
func isLive(t *testing.T, dm *DatabaseManager, id string) bool {
	t.Helper()
	var deleted *int64
	if err := dm.SQLDB().QueryRow(`SELECT deleted_at FROM memories WHERE id = ?`, id).Scan(&deleted); err != nil {
		if err == sql.ErrNoRows {
			return false // hard-deleted: definitively not live
		}
		t.Fatalf("read deleted_at for %s: %v", id, err)
	}
	return deleted == nil || *deleted <= 0
}

// directiveContent reads a directive's content.
func directiveContent(t *testing.T, dm *DatabaseManager, id string) string {
	t.Helper()
	var content string
	if err := dm.SQLDB().QueryRow(`SELECT content FROM memories WHERE id = ?`, id).Scan(&content); err != nil {
		t.Fatalf("read content for %s: %v", id, err)
	}
	return content
}

// backdate makes a row stale for every age-based retention rule.
func backdate(t *testing.T, dm *DatabaseManager, id string, days int) {
	t.Helper()
	if _, err := dm.SQLDB().Exec(
		`UPDATE memories SET updated_at = CAST(strftime('%s','now','-' || ? || ' days') AS INTEGER) WHERE id = ?`,
		days, id,
	); err != nil {
		t.Fatalf("backdate %s: %v", id, err)
	}
}

// insertPlainMemory writes an ordinary, never-accessed memory.
func insertPlainMemory(t *testing.T, dm *DatabaseManager, id, content string, weight int) {
	t.Helper()
	if _, err := dm.SQLDB().Exec(`
		INSERT INTO memories (id, collection, content, weight, reinforcement_count, is_long_term, created_at, updated_at)
		VALUES (?, 'memories', ?, ?, 0, 0, strftime('%s','now','-40 days'), strftime('%s','now','-40 days'))`,
		id, content, weight,
	); err != nil {
		t.Fatalf("insert memory %s: %v", id, err)
	}
}

func newRetentionStore(dm *DatabaseManager) *MemoryStore {
	return &MemoryStore{DM: dm, DB: &SQLiteConnection{DB: dm.SQLDB()}}
}

// TestAutoPrunePolicy_PreservesBootstrapDirectives is the primary
// regression: the MaintenanceCycle prune that reproduced the defect
// (never-accessed, 30 days) must leave every seeded directive live.
func TestAutoPrunePolicy_PreservesBootstrapDirectives(t *testing.T) {
	dm := NewTestDM(t)
	if _, err := seed.ApplyDirectives(dm); err != nil {
		t.Fatalf("seed directives: %v", err)
	}
	for _, sd := range seed.SeedDirectives {
		backdate(t, dm, sd.StableID, 40)
	}
	insertPlainMemory(t, dm, "mem-never-accessed", "ordinary never-accessed memory", 1)
	// The ordinary row must stay eligible for the never-accessed rule; a
	// directive will not be pruned by it after the fix.

	store := newRetentionStore(dm)
	pruned, err := store.AutoPrunePolicy(AutoPruneConfig{
		NeverAccessedMaxDays: 30,
		LowWeightMaxDays:     90,
	})
	if err != nil {
		t.Fatalf("auto-prune: %v", err)
	}
	if pruned != 1 {
		t.Errorf("pruned = %d, want 1 (only the ordinary memory)", pruned)
	}
	for _, sd := range seed.SeedDirectives {
		if !isLive(t, dm, sd.StableID) {
			t.Errorf("bootstrap directive %s was soft-deleted by access-based retention", sd.StableID)
		}
	}
	if isLive(t, dm, "mem-never-accessed") {
		t.Error("ordinary never-accessed memory was not pruned; ordinary retention behaviour changed")
	}
}

// TestAutoPrunePolicy_PreservesOperatorEditedDirective pins that the
// exemption is about the row's role, not its content: an operator edit
// must not make it look like an ordinary memory to the prune.
func TestAutoPrunePolicy_PreservesOperatorEditedDirective(t *testing.T) {
	dm := NewTestDM(t)
	if _, err := seed.ApplyDirectives(dm); err != nil {
		t.Fatalf("seed directives: %v", err)
	}
	const id = "mpm-seed-read-wake-context"
	if _, err := dm.SQLDB().Exec(
		`UPDATE memories SET content = ?, updated_at = CAST(strftime('%s','now','-40 days') AS INTEGER) WHERE id = ?`,
		"operator-written directive text", id,
	); err != nil {
		t.Fatalf("edit directive: %v", err)
	}

	store := newRetentionStore(dm)
	if _, err := store.AutoPrunePolicy(AutoPruneConfig{NeverAccessedMaxDays: 30}); err != nil {
		t.Fatalf("auto-prune: %v", err)
	}
	if !isLive(t, dm, id) {
		t.Fatal("operator-edited directive was soft-deleted")
	}
	if got := directiveContent(t, dm, id); got != "operator-written directive text" {
		t.Errorf("operator edit lost: content = %q", got)
	}
}

// TestAutoPrunePolicy_DirectivePredicateCoversBothIdentifiers pins the
// canonical predicate's two arms. Legacy rows may carry only one of the
// markers, and both mean "this row is policy".
func TestAutoPrunePolicy_DirectivePredicateCoversBothIdentifiers(t *testing.T) {
	dm := NewTestDM(t)
	// Legacy column-only marker, collection='memories'.
	if _, err := dm.SQLDB().Exec(`
		INSERT INTO memories (id, collection, content, weight, is_prime_directive, reinforcement_count, is_long_term, created_at, updated_at)
		VALUES ('dir-flag-only', 'memories', 'legacy flagged directive', 1, 1, 0, 0,
		        strftime('%s','now','-40 days'), strftime('%s','now','-40 days'))`); err != nil {
		t.Fatalf("seed flagged directive: %v", err)
	}
	// Collection-only marker, flag unset.
	if _, err := dm.SQLDB().Exec(`
		INSERT INTO memories (id, collection, content, weight, is_prime_directive, reinforcement_count, is_long_term, created_at, updated_at)
		VALUES ('dir-collection-only', 'directives', 'collection directive', 1, 0, 0, 0,
		        strftime('%s','now','-40 days'), strftime('%s','now','-40 days'))`); err != nil {
		t.Fatalf("seed collection directive: %v", err)
	}

	store := newRetentionStore(dm)
	if _, err := store.AutoPrunePolicy(AutoPruneConfig{
		NeverAccessedMaxDays: 30,
		LowWeightMaxDays:     90,
	}); err != nil {
		t.Fatalf("auto-prune: %v", err)
	}
	for _, id := range []string{"dir-flag-only", "dir-collection-only"} {
		if !isLive(t, dm, id) {
			t.Errorf("%s was pruned; the directive predicate must cover both identifiers", id)
		}
	}
}

// TestPruneNeverAccessed_PreservesDirectives pins the second prune path.
// It is dormant for seeded directives today (weight 10), but weight is
// operator-mutable, and the rule must not become a directive reaper.
func TestPruneNeverAccessed_PreservesDirectives(t *testing.T) {
	dm := NewTestDM(t)
	if _, err := seed.ApplyDirectives(dm); err != nil {
		t.Fatalf("seed directives: %v", err)
	}
	const id = "mpm-seed-wake-triage-policy"
	if _, err := dm.SQLDB().Exec(`UPDATE memories SET weight = 1 WHERE id = ?`, id); err != nil {
		t.Fatalf("set weight: %v", err)
	}
	insertPlainMemory(t, dm, "mem-weight-one", "ordinary weight-1 memory", 1)

	pruned, err := dm.PruneNeverAccessed()
	if err != nil {
		t.Fatalf("prune never accessed: %v", err)
	}
	if pruned != 1 {
		t.Errorf("pruned = %d, want 1 (only the ordinary weight-1 memory)", pruned)
	}
	if !isLive(t, dm, id) {
		t.Error("directive was deleted by PruneNeverAccessed")
	}
	if isLive(t, dm, "mem-weight-one") {
		t.Error("ordinary weight-1 never-accessed memory survived; ordinary retention behaviour changed")
	}
}

// TestDirectiveRetention_OrdinaryRowsUnaffected is a narrow guard that
// the exclusion clause did not accidentally widen to every row.
func TestDirectiveRetention_OrdinaryRowsUnaffected(t *testing.T) {
	dm := NewTestDM(t)
	insertPlainMemory(t, dm, "mem-a", "first ordinary memory", 1)
	insertPlainMemory(t, dm, "mem-b", "second ordinary memory", 1)

	store := newRetentionStore(dm)
	pruned, err := store.AutoPrunePolicy(AutoPruneConfig{NeverAccessedMaxDays: 30})
	if err != nil {
		t.Fatalf("auto-prune: %v", err)
	}
	if pruned != 2 {
		t.Fatalf("pruned = %d, want 2 ordinary memories", pruned)
	}
}

// TestDirectiveRevival_StillPreservesOperatorEdits is the interaction
// guard: even if a directive is deleted through an explicit lifecycle
// operation, the documented seed revival must keep the operator's text.
func TestDirectiveRevival_StillPreservesOperatorEdits(t *testing.T) {
	dm := NewTestDM(t)
	if _, err := seed.ApplyDirectives(dm); err != nil {
		t.Fatalf("seed directives: %v", err)
	}
	const id = "mpm-seed-session-end-cluster-triage"
	const edited = "operator-edited cluster triage directive"
	if _, err := dm.SQLDB().Exec(`UPDATE memories SET content = ? WHERE id = ?`, edited, id); err != nil {
		t.Fatalf("edit: %v", err)
	}
	// Explicit lifecycle operation: soft-delete, mimicking an operator
	// retirement (the prune no longer does this).
	if _, err := dm.SQLDB().Exec(
		`UPDATE memories SET deleted_at = CAST(strftime('%s','now') AS INTEGER) WHERE id = ?`, id,
	); err != nil {
		t.Fatalf("soft-delete: %v", err)
	}

	summary, err := seed.ApplyDirectives(dm)
	if err != nil {
		t.Fatalf("revive: %v", err)
	}
	if !isLive(t, dm, id) {
		t.Fatal("revival did not restore the directive")
	}
	if got := directiveContent(t, dm, id); got != edited {
		t.Errorf("revival replaced the operator edit: %q", got)
	}
	found := false
	for _, revived := range summary.Revived {
		if revived == id {
			found = true
		}
	}
	if !found && !strings.Contains(strings.Join(summary.Created, ","), id) {
		t.Errorf("seed summary does not report the revived directive: %+v", summary)
	}
}
