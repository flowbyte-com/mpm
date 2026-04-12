package core

import (
	"path/filepath"
	"testing"
)

func TestAnchorInsert(t *testing.T) {
	dir := t.TempDir()
	dbPath := filepath.Join(dir, "mini-bot.db")
	InitMiniBotDB(dbPath)

	db, err := OpenDBForPath(dbPath)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()

	// Insert anchor
	err = InsertAnchor(db, "test anchor content", "test context", "session123", 3)
	if err != nil {
		t.Fatalf("InsertAnchor failed: %v", err)
	}

	// Verify it exists
	anchors, err := GetRecentAnchors(db, 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(anchors) == 0 {
		t.Error("expected at least one anchor")
	}
	// Verify full anchor data
	if anchors[0].Content != "test anchor content" {
		t.Errorf("expected content 'test anchor content', got %q", anchors[0].Content)
	}
	if anchors[0].Context != "test context" {
		t.Errorf("expected context 'test context', got %q", anchors[0].Context)
	}
	if anchors[0].SessionID != "session123" {
		t.Errorf("expected session_id 'session123', got %q", anchors[0].SessionID)
	}
	if anchors[0].Weight != 3 {
		t.Errorf("expected weight 3, got %d", anchors[0].Weight)
	}
}

func TestAnchorIdempotent(t *testing.T) {
	dir := t.TempDir()
	dbPath := filepath.Join(dir, "mini-bot.db")
	InitMiniBotDB(dbPath)

	db, err := OpenDBForPath(dbPath)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()

	// Insert same anchor twice with different weights
	if err := InsertAnchor(db, "same content", "same context", "session456", 2); err != nil {
		t.Fatalf("InsertAnchor failed: %v", err)
	}
	if err := InsertAnchor(db, "same content", "same context", "session456", 5); err != nil {
		t.Fatalf("InsertAnchor failed: %v", err)
	}

	// Should have MAX(2,5) = 5, not two anchors
	anchors, _ := GetRecentAnchors(db, 10)
	if len(anchors) != 1 {
		t.Errorf("expected 1 anchor (idempotent), got %d", len(anchors))
	}
	if anchors[0].Weight != 5 {
		t.Errorf("expected weight MAX(2,5)=5, got %d", anchors[0].Weight)
	}
}