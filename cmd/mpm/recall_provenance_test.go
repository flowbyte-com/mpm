package main

import (
	"strings"
	"testing"
)

func TestFtsHitTerms_ExtractsMatch(t *testing.T) {
	content := "the project uses sqlite for storage"
	hits := ftsHitTerms(content, "sqlite storage")
	want := []string{"sqlite", "storage"}
	if strings.Join(hits, ",") != strings.Join(want, ",") {
		t.Errorf("ftsHitTerms = %v, want %v", hits, want)
	}
}

func TestFtsHitTerms_FiltersStopwords(t *testing.T) {
	content := "the project uses sqlite"
	hits := ftsHitTerms(content, "the project sqlite")
	// "the" and "project" should appear; "the" is a stopword (filtered),
	// "project" is not (≥3 chars and not in stop set).
	if strings.Contains(strings.Join(hits, ","), "the") {
		t.Errorf("expected stopword 'the' to be filtered, got %v", hits)
	}
	if !strings.Contains(strings.Join(hits, ","), "project") {
		t.Errorf("expected 'project' in hits, got %v", hits)
	}
}

func TestFtsHitTerms_SkipsShortWords(t *testing.T) {
	content := "ab cdef ghij"
	hits := ftsHitTerms(content, "ab cdef ghij")
	for _, h := range hits {
		if len(h) < 3 {
			t.Errorf("expected words < 3 chars to be filtered, got %q", h)
		}
	}
}

func TestFtsHitTerms_EmptyQuery(t *testing.T) {
	hits := ftsHitTerms("anything", "")
	if hits != nil {
		t.Errorf("expected nil for empty query, got %v", hits)
	}
}

func TestFtsHitTerms_NoMatch(t *testing.T) {
	hits := ftsHitTerms("the project", "xyz123")
	if len(hits) != 0 {
		t.Errorf("expected no hits, got %v", hits)
	}
}
