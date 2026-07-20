package internal

import "testing"

// TestIsMirroredCollection_LocksAllowList pins the mirror allow-list so a
// future "let's mirror lessons too" PR can't quietly bloat mirror.jsonl.
//
// The allow-list is the source-of-truth audit trail for sync/sharing
// decisions. Lessons have their own table as source; the scratchpad
// collection is ephemeral by definition. Adding them would double-write
// rows that have no canonical join key on the consumer side.
func TestIsMirroredCollection_LocksAllowList(t *testing.T) {
	want := map[string]bool{
		"changelog":      true,
		"memories":       true,
		"theories":       true,
		"decisions":      true,
		"knowledge":      true,
		"directives":     true,
		"mpm-projects":   true,
		"world-cup-2026": true,
	}

	// Negative cases — explicitly NOT mirrored. If a future change adds
	// one of these to the allow-list, the test fails loudly.
	notMirrored := []string{
		"lessons",
		"scratchpad_orphans",
		"ephemeral_scratchpad",
		"", // empty collection should never be mirrored
		"random_unmapped_collection",
	}

	for coll, wantV := range want {
		if got := isMirroredCollection(coll); got != wantV {
			t.Errorf("isMirroredCollection(%q) = %v, want %v", coll, got, wantV)
		}
	}

	for _, coll := range notMirrored {
		if isMirroredCollection(coll) {
			t.Errorf("isMirroredCollection(%q) = true, want false (must not be in allow-list)", coll)
		}
	}
}

// TestMirroredCollectionsSet_MatchesAllowList guards against drift between
// the set constant and the helper. They should be wired through the same
// source of truth — the map — so this is mostly belt-and-braces.
func TestMirroredCollectionsSet_MatchesAllowList(t *testing.T) {
	if len(mirroredCollections) == 0 {
		t.Fatal("mirroredCollections map is empty; either the allow-list was wiped or the symbol moved")
	}
	for coll := range mirroredCollections {
		if !isMirroredCollection(coll) {
			t.Errorf("mirroredCollections contains %q but isMirroredCollection returns false", coll)
		}
	}
}