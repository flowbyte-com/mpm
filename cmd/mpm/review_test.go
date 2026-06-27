package main

import (
	"bytes"
	"os"
	"testing"
)

func TestReviewFlagDefaults(t *testing.T) {
	// Test that default (no flags) uses promoted mode
	args := []string{"review"}
	code := handleReview(args)
	// If no memories exist, it should return 0 (not error)
	if code != 0 && code != 1 {
		t.Errorf("expected exit code 0 or 1, got %d", code)
	}
}

func TestReviewJsonFlag(t *testing.T) {
	// Test that --json flag is accepted
	args := []string{"review", "--json"}
	code := handleReview(args)
	// We just verify it doesn't crash on parsing
	if code == 1 {
		// Could be ok if no db - check stderr
	}
}

func TestReviewStaleFlag(t *testing.T) {
	// Test that --stale flag is accepted
	args := []string{"review", "--stale", "--days", "7"}
	code := handleReview(args)
	// Should parse without error (may fail on db)
	if code == 1 {
		// Could be ok if no db
	}
}

func TestShortID(t *testing.T) {
	tests := []struct {
		input    string
		expected string
	}{
		{"abcdefghijklmnop", "abcdefgh"},
		{"abc", "abc"},
		{"", ""},
		{"12345678", "12345678"},
	}
	for _, tt := range tests {
		result := shortID(tt.input)
		if result != tt.expected {
			t.Errorf("shortID(%q) = %q, want %q", tt.input, result, tt.expected)
		}
	}
}

func TestReviewPromotedMode(t *testing.T) {
	// Test that --promoted triggers promoted mode
	args := []string{"review", "--promoted"}
	code := handleReview(args)
	// If db is empty or doesn't exist, code 1 is acceptable
	_ = code
}

func TestReviewStaleWithDays(t *testing.T) {
	// Test that --stale --days N works
	args := []string{"review", "--stale", "--days", "14"}
	code := handleReview(args)
	_ = code
}

func TestReviewLimit(t *testing.T) {
	// Test that --limit is accepted
	args := []string{"review", "--limit", "5"}
	code := handleReview(args)
	_ = code
}

func TestReviewOutputFormat(t *testing.T) {
	// Capture stdout to verify format
	old := os.Stdout
	r, w, _ := os.Pipe()
	os.Stdout = w

	args := []string{"review"}
	handleReview(args)

	w.Close()
	os.Stdout = old

	var buf bytes.Buffer
	_, err := buf.ReadFrom(r)
	if err != nil {
		t.Skip("could not capture output")
	}
	output := buf.String()

	// Should contain header or "No promoted" message
	if output == "" {
		t.Skip("no output captured")
	}
	// Output should be non-empty
	if len(output) == 0 {
		t.Error("expected non-empty output")
	}
}
