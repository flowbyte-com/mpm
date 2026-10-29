// ops_init_directives_orphans_test.go — the registry-membership report on
// `mpm ops init directives` (issue #59, Option B).
//
// WHAT THIS PINS
// --------------
//   1. A fresh install reports NO orphans. Registry membership and the
//      rows on disk agree, so there is nothing for the operator to decide.
//   2. A row that has left the registry IS reported, with its ID and a
//      preview of its content, so the operator can see what they would be
//      acting on.
//   3. The report NEVER names a user-authored directive. A directive the
//      operator wrote was never in the registry, so registry membership
//      says nothing about it and it is never a candidate.
//   4. The report DELETES NOTHING. Eligibility is not authorisation, and
//      the command is not an automatic cleanup mechanism.
//
// The rendering lives in cmd/mpm/ops_init_directives.go; the policy lives
// in internal/core/seed/retention.go and docs/SPEC.md.

package main

import (
	"fmt"
	"strings"
	"testing"

	"github.com/flowbyte-com/mpm-core"
	"github.com/flowbyte-com/mpm-core/seed"
)

// orphanTestDM opens a disposable database with the baseline applied.
func orphanTestDM(t *testing.T) *internal.DatabaseManager {
	t.Helper()
	dm := newTestDMForCmd(t)
	if _, err := seed.ApplyDirectives(dm); err != nil {
		t.Fatalf("seed directives: %v", err)
	}
	return dm
}

// insertSeededDirectiveRow writes a row that carries the seeder's
// provenance marker, as a de-registered directive would.
func insertSeededDirectiveRow(t *testing.T, dm *internal.DatabaseManager, id, content string) {
	t.Helper()
	meta := fmt.Sprintf(`{"is_prime_directive":1,"scope":"global","provenance":{"agent":"mpm_ops_init","compute":"absolute","model":"direct","persona":"operator","source":%q}}`,
		seed.DirectiveProvenanceSource)
	if _, err := dm.SQLDB().Exec(`
		INSERT INTO memories (id, collection, content, metadata, is_prime_directive, weight,
		                      reinforcement_count, is_long_term, created_at, updated_at)
		VALUES (?, 'directives', ?, ?, 1, 10, 0, 0, strftime('%s','now'), strftime('%s','now'))`,
		id, content, meta); err != nil {
		t.Fatalf("insert directive %s: %v", id, err)
	}
}

// TestPrintOrphanedDirectives_ReportsDeRegisteredRows is the report's
// whole job: making the candidate visible.
func TestPrintOrphanedDirectives_ReportsDeRegisteredRows(t *testing.T) {
	out := captureStdout(t, func() {
		printOrphanedDirectives([]seed.OrphanedDirective{
			{ID: "mpm-seed-removed-from-registry", Content: "the de-registered policy"},
		})
	})

	if !strings.Contains(out, "mpm-seed-removed-from-registry") {
		t.Errorf("the report does not name the orphan:\n%s", out)
	}
	if !strings.Contains(out, "the de-registered policy") {
		t.Errorf("the report does not show the content:\n%s", out)
	}
	// The wording must not promise a deletion, and must not claim the
	// rows are broken.
	if strings.Contains(strings.ToLower(out), "will be deleted") ||
		strings.Contains(strings.ToLower(out), "has been deleted") {
		t.Errorf("the report reads as if a deletion happened or is scheduled:\n%s", out)
	}
	if !strings.Contains(strings.ToLower(out), "eligible") {
		t.Errorf("the report does not say the row is ELIGIBLE for cleanup — that is "+
			"the distinction the policy turns on:\n%s", out)
	}
}

// TestPrintOrphanedDirectives_SilentWhenThereAreNone — a fresh install
// produces no orphan section at all. A report that always prints a section
// trains the operator to ignore it.
func TestPrintOrphanedDirectives_SilentWhenThereAreNone(t *testing.T) {
	out := captureStdout(t, func() {
		printOrphanedDirectives(nil)
	})
	if strings.TrimSpace(out) != "" {
		t.Errorf("the report printed something with no orphans:\n%s", out)
	}
}

// TestOrphanReport_NeverNamesUserAuthoredDirectives is the safety
// requirement at the report layer.
//
// An operator-authored directive is not in the registry and never was. If
// the report named it, the operator could reasonably read "eligible for
// cleanup" as "this is stale policy", and delete their own policy on the
// strength of a registry change.
func TestOrphanReport_NeverNamesUserAuthoredDirectives(t *testing.T) {
	dm := orphanTestDM(t)

	// A user-authored directive: collection='directives', no provenance
	// marker.
	if _, err := dm.SQLDB().Exec(`
		INSERT INTO memories (id, collection, content, metadata, weight,
		                      reinforcement_count, is_long_term, created_at, updated_at)
		VALUES ('operator-authored', 'directives', 'the operator''s own policy',
		        '{"scope":"global"}', 10, 0, 0, strftime('%s','now'), strftime('%s','now'))`); err != nil {
		t.Fatalf("insert operator directive: %v", err)
	}
	// And one that HAS left the registry, so the report has something to
	// say.
	insertSeededDirectiveRow(t, dm, "mpm-seed-removed-from-registry", "the de-registered policy")

	orphans, err := seed.OrphanedDirectives(dm)
	if err != nil {
		t.Fatalf("orphaned directives: %v", err)
	}
	if len(orphans) != 1 {
		t.Fatalf("orphans = %d, want 1: %v", len(orphans), orphans)
	}
	if orphans[0].ID == "operator-authored" {
		t.Fatal("the operator's own directive was reported as an orphan")
	}

	out := captureStdout(t, func() {
		printOrphanedDirectives(orphans)
	})
	if strings.Contains(out, "operator-authored") {
		t.Errorf("the report names an operator-authored directive:\n%s", out)
	}
}

// TestOrphanReport_DeletesNothing is the eligibility/authorisation
// distinction, at the layer an operator actually runs.
//
// The report is a REPORT. It must leave every row exactly as it found it.
func TestOrphanReport_DeletesNothing(t *testing.T) {
	dm := orphanTestDM(t)
	insertSeededDirectiveRow(t, dm, "mpm-seed-removed-from-registry", "the de-registered policy")

	before := countLiveRows(t, dm)
	out := captureStdout(t, func() {
		orphans, err := seed.OrphanedDirectives(dm)
		if err != nil {
			t.Fatalf("orphaned directives: %v", err)
		}
		printOrphanedDirectives(orphans)
	})
	after := countLiveRows(t, dm)

	if before != after {
		t.Errorf("live row count changed from %d to %d — the report deleted something", before, after)
	}
	var content string
	if err := dm.SQLDB().QueryRow(
		`SELECT content FROM memories WHERE id = 'mpm-seed-removed-from-registry'`).Scan(&content); err != nil {
		t.Fatalf("the orphan row is gone: %v", err)
	}
	if content != "the de-registered policy" {
		t.Errorf("the orphan's content changed to %q", content)
	}
	if !strings.Contains(out, "mpm-seed-removed-from-registry") {
		t.Errorf("the report did not mention the orphan:\n%s", out)
	}
}

// countLiveRows counts the live rows in the memories table.
func countLiveRows(t *testing.T, dm *internal.DatabaseManager) int {
	t.Helper()
	var n int
	if err := dm.SQLDB().QueryRow(
		`SELECT COUNT(*) FROM memories WHERE deleted_at IS NULL`).Scan(&n); err != nil {
		t.Fatalf("count live rows: %v", err)
	}
	return n
}
