package internal

import (
	"os"
	"testing"
)

// ==================== Stats / reader surface ====================

// TestStats_MirrorEventsIsCanonical pins the v2 reader contract. The
// canonical name is "mirror_events"; the legacy "mirror_entries" name
// is kept for one release as a deprecated alias to keep dashboards
// stable. The test is the structural surface: both names are present
// and equal, and neither is the misleading "mirrored memories" form.
func TestStats_MirrorEventsIsCanonical(t *testing.T) {
	dm, path := newTestDMWithMirror(t)

	// Two v2 records: one with content, one tombstone. Stats should
	// count lines, not embed any of the content.
	if err := appendMirrorLine(dm.mirrorPath, NewMirrorLessonEvent("l-1", "title-one", "mpm")); err != nil {
		t.Fatal(err)
	}
	if err := appendMirrorLine(dm.mirrorPath, NewMirrorShredEvent("m-1", "memories")); err != nil {
		t.Fatal(err)
	}

	store := &MemoryStore{MirrorFile: path}
	stats := store.Stats()
	if stats["mirror_events"] != 2 {
		t.Errorf("mirror_events = %v, want 2", stats["mirror_events"])
	}
	if stats["mirror_entries"] != 2 {
		t.Errorf("mirror_entries alias = %v, want 2 (deprecated alias)", stats["mirror_entries"])
	}
	if _, present := stats["mirrored_memories"]; present {
		t.Errorf("stats exposes the legacy 'mirrored memories' label, which the design forbids")
	}
	if _, err := os.Stat(path); err != nil {
		t.Fatalf("mirror file missing: %v", err)
	}
}