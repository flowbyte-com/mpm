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

	facts := []string{"fact=test anchor content"}
	err = InsertAnchor(db, facts, "test summary", []string{"test"}, "test context", "session123", 3)
	if err != nil {
		t.Fatalf("InsertAnchor failed: %v", err)
	}

	anchors, err := GetRecentAnchors(db, 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(anchors) == 0 {
		t.Error("expected at least one anchor")
	}
	if anchors[0].Summary != "test summary" {
		t.Errorf("expected summary 'test summary', got %q", anchors[0].Summary)
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

	facts1 := []string{"fact=same content"}
	if err := InsertAnchor(db, facts1, "same summary", []string{"test"}, "same context", "session456", 2); err != nil {
		t.Fatalf("InsertAnchor failed: %v", err)
	}
	facts2 := []string{"fact=same content updated"}
	if err := InsertAnchor(db, facts2, "same summary", []string{"test"}, "same context", "session456", 5); err != nil {
		t.Fatalf("InsertAnchor failed: %v", err)
	}

	anchors, _ := GetRecentAnchors(db, 10)
	if len(anchors) != 1 {
		t.Errorf("expected 1 anchor (idempotent), got %d", len(anchors))
	}
	if anchors[0].Weight != 5 {
		t.Errorf("expected weight MAX(2,5)=5, got %d", anchors[0].Weight)
	}
}

func TestExtractFactsFromText(t *testing.T) {
	tests := []struct {
		input       string
		minExpected int
	}{
		{"I work as a doctor", 1},
		{"I have a standing desk", 1},
		{"I like programming", 1},
		{"I hate waiting in lines", 1},
		{"I have an issue with my back", 2},
		{"hello world", 1},
	}

	for _, tt := range tests {
		facts := ExtractFactsFromText(tt.input)
		if len(facts) < tt.minExpected {
			t.Errorf("ExtractFactsFromText(%q) = %d facts, want at least %d", tt.input, len(facts), tt.minExpected)
		}
	}
}

func TestExtractTagsFromText(t *testing.T) {
	tests := []struct {
		input string
		has   []string
	}{
		{"I work as a doctor", []string{"work", "health"}},
		{"I love playing video games", []string{"hobby"}},
		{"I have a problem with my code", []string{"tech"}},
	}

	for _, tt := range tests {
		tags := ExtractTagsFromText(tt.input)
		for _, want := range tt.has {
			found := false
			for _, tag := range tags {
				if tag == want {
					found = true
					break
				}
			}
			if !found {
				t.Errorf("ExtractTagsFromText(%q) missing tag %q, got %v", tt.input, want, tags)
			}
		}
	}
}
