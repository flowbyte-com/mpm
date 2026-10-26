// directive_retention_age_test.go — pin the age-based prune path for
// standing directives (issue #59).
//
// The retention policy exempts directives from every reclaimable
// retention path, and the comment on the sibling path says so
// explicitly ("they must have no implicit reclamation route"). The
// age-based hard delete was left out of that exemption, so the two
// flags of the SAME command (`mpm prune`) disagreed:
//
//	mpm prune --never-accessed   -> directives preserved
//	mpm prune --older-than 90d    -> directives hard-deleted
//
// `PruneOlderThan` is a hard DELETE (not a soft delete), so a directive
// removed by it is gone: the boot-time seed re-creates it from the
// canonical text, and the operator's edits are lost with it.
//
// This file pins that the age-based prune preserves directives the same
// way the access-based prune does, and that ordinary memories are still
// pruned exactly as before.
package internal

import (
	"testing"
	"time"

	"github.com/flowbyte-com/mpm-core/seed"
)

// backdateCreatedAt makes a row old enough for the age-based prune.
func backdateCreatedAt(t *testing.T, dm *DatabaseManager, id string, days int) {
	t.Helper()
	if _, err := dm.SQLDB().Exec(
		`UPDATE memories SET created_at = CAST(strftime('%s','now','-' || ? || ' days') AS INTEGER) WHERE id = ?`,
		days, id,
	); err != nil {
		t.Fatalf("backdate created_at for %s: %v", id, err)
	}
}

// TestPruneOlderThan_PreservesDirectives is the regression: the
// age-based hard delete must leave directives alone, exactly as the
// access-based prune does.
func TestPruneOlderThan_PreservesDirectives(t *testing.T) {
	dm := NewTestDM(t)
	if _, err := seed.ApplyDirectives(dm); err != nil {
		t.Fatalf("seed directives: %v", err)
	}
	const directiveID = "mpm-seed-wake-triage-policy"
	backdateCreatedAt(t, dm, directiveID, 400)
	insertPlainMemory(t, dm, "mem-old-ordinary", "an ordinary old memory", 1)

	// 90 days ago: the ordinary memory (created 40 days ago) is not
	// older than the cutoff either, so it is inserted with an explicit
	// old created_at below.
	backdateCreatedAt(t, dm, "mem-old-ordinary", 400)

	cutoff := nowUnixForRetention() - 90*24*3600
	pruned, err := dm.PruneOlderThan(cutoff)
	if err != nil {
		t.Fatalf("prune older than: %v", err)
	}
	if pruned != 1 {
		t.Errorf("pruned = %d, want 1 (only the ordinary memory)", pruned)
	}
	if !isLive(t, dm, directiveID) {
		t.Error("directive was hard-deleted by the age-based prune")
	}
	if isLive(t, dm, "mem-old-ordinary") {
		t.Error("ordinary old memory survived; ordinary retention behaviour changed")
	}
}

// TestPruneOlderThan_PreservesOperatorEditedDirective pins that the
// exemption is about the row's role, not its content or age: an
// operator-edited directive is still a directive.
func TestPruneOlderThan_PreservesOperatorEditedDirective(t *testing.T) {
	dm := NewTestDM(t)
	if _, err := seed.ApplyDirectives(dm); err != nil {
		t.Fatalf("seed directives: %v", err)
	}
	const directiveID = "mpm-seed-wake-triage-policy"
	if _, err := dm.SQLDB().Exec(
		`UPDATE memories SET content = ? WHERE id = ?`,
		"the operator's own text for this directive", directiveID,
	); err != nil {
		t.Fatalf("edit directive: %v", err)
	}
	backdateCreatedAt(t, dm, directiveID, 400)

	cutoff := nowUnixForRetention() - 90*24*3600
	if _, err := dm.PruneOlderThan(cutoff); err != nil {
		t.Fatalf("prune older than: %v", err)
	}
	if !isLive(t, dm, directiveID) {
		t.Fatal("operator-edited directive was hard-deleted by the age-based prune")
	}
	if got := directiveContent(t, dm, directiveID); got != "the operator's own text for this directive" {
		t.Errorf("directive content = %q, want the operator's edit", got)
	}
}

// nowUnixForRetention returns the current unix epoch in seconds.
func nowUnixForRetention() int64 {
	return time.Now().Unix()
}
