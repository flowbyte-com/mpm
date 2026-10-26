// directive_purge_test.go — pin that the hard-delete purge does not
// destroy standing directives (issue #59).
//
// A directive that is soft-deleted (by an agent's `mpm_memory
// action=delete`, by the CLI's `mpm rm`, or by the churn this policy
// exists to stop) sits in `raw` state until the boot-time seed revives
// it. If `mpm ops gc --purge` hard-deletes it in the meantime, the row
// is gone for good and the seed re-creates it from the canonical text —
// the operator's edits are lost.
//
// The retention policy exempts directives from every reclaimable path.
// This test pins the last one: the purge.
package main

import (
	"strings"
	"testing"
)

// seedSoftDeletedMemory inserts one soft-deleted memory row with the
// given collection, old enough for the 30-day purge window.
func seedSoftDeletedMemory(t *testing.T, id, collection, content string) {
	t.Helper()
	dm := getDB()
	if dm == nil {
		t.Fatal("getDB returned nil")
	}
	if _, err := dm.SQLDB().Exec(
		`INSERT INTO memories (id, collection, content, weight, deleted_at, created_at, updated_at)
		 VALUES (?, ?, ?, 1,
		         CAST(strftime('%s','now','-40 days') AS INTEGER),
		         CAST(strftime('%s','now','-400 days') AS INTEGER),
		         CAST(strftime('%s','now','-40 days') AS INTEGER))`,
		id, collection, content,
	); err != nil {
		t.Fatalf("seed %s: %v", id, err)
	}
}

// rowExists reports whether a row with the given id is still present.
func rowExists(t *testing.T, id string) bool {
	t.Helper()
	dm := getDB()
	var n int
	if err := dm.SQLDB().QueryRow(`SELECT COUNT(*) FROM memories WHERE id = ?`, id).Scan(&n); err != nil {
		t.Fatalf("count %s: %v", id, err)
	}
	return n > 0
}

// TestGC_PurgePreservesDirectives pins that the purge removes ordinary
// soft-deleted memories and leaves directives alone.
func TestGC_PurgePreservesDirectives(t *testing.T) {
	workspace(t)
	seedSoftDeletedMemory(t, "directive-purge-test", "directives", "standing policy")
	seedSoftDeletedMemory(t, "ordinary-purge-test", "memories", "an ordinary deleted memory")

	code, out := runCLI(t, "ops", "gc", "--purge")
	if code != 0 {
		t.Fatalf("gc --purge exited %d: %q", code, out)
	}
	if !strings.Contains(out, "Purged 1") {
		t.Errorf("purge did not report removing the ordinary row: %q", out)
	}
	if !rowExists(t, "directive-purge-test") {
		t.Error("directive was hard-deleted by the purge; a directive removed by a sweep " +
			"is re-seeded from the canonical text and the operator's edits are lost")
	}
	if rowExists(t, "ordinary-purge-test") {
		t.Error("ordinary soft-deleted memory survived the purge; ordinary retention behaviour changed")
	}
}

// TestGC_PurgePreservesPrimeDirectiveRows pins the second canonical
// directive identifier: a row marked with is_prime_directive=1 is
// exempt too, not only one whose collection says 'directives'.
func TestGC_PurgePreservesPrimeDirectiveRows(t *testing.T) {
	workspace(t)
	dm := getDB()
	if dm == nil {
		t.Fatal("getDB returned nil")
	}
	if _, err := dm.SQLDB().Exec(
		`INSERT INTO memories (id, collection, content, weight, is_prime_directive, deleted_at, created_at, updated_at)
		 VALUES ('prime-directive-purge-test', 'memories', 'legacy prime directive', 1, 1,
		         CAST(strftime('%s','now','-40 days') AS INTEGER),
		         CAST(strftime('%s','now','-400 days') AS INTEGER),
		         CAST(strftime('%s','now','-40 days') AS INTEGER))`,
	); err != nil {
		t.Fatalf("seed prime directive: %v", err)
	}

	if code, out := runCLI(t, "ops", "gc", "--purge"); code != 0 {
		t.Fatalf("gc --purge exited %d: %q", code, out)
	}
	if !rowExists(t, "prime-directive-purge-test") {
		t.Error("is_prime_directive row was hard-deleted by the purge")
	}
}
