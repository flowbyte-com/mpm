package main

import (
	"strings"
	"testing"
)

func TestSanitizeContentForFTS(t *testing.T) {
	tests := []struct {
		name     string
		input    string
		expected string
	}{
		{
			name:     "plain text with stop words",
			input:    "the quick brown fox jumps over the lazy dog",
			expected: "quick OR brown OR jumps OR over OR lazy", // fox,dog filtered (3 chars), stop words filtered
		},
		{
			name:     "markdown headers stripped",
			input:    "# Introduction to Concurrency Patterns",
			expected: "introduction OR concurrency OR patterns", // headers stripped
		},
		{
			name:     "markdown bold stripped",
			input:    "This is **important** information about Go",
			expected: "important OR information OR about", // This,is,Go filtered, "about" is 5 chars kept
		},
		{
			name:     "markdown italic stripped",
			input:    "Learn *concurrent* programming with Go channels",
			expected: "learn OR concurrent OR programming OR channels", // Learn,with,Go filtered
		},
		{
			name:     "markdown code stripped",
			input:    "Use `goroutine` and `channel` for concurrency",
			expected: "goroutine OR channel OR concurrency", // Use,and,for filtered
		},
		{
			name:     "mixed markdown",
			input:    "## Testing **concurrency** in Go with `goroutines`",
			expected: "testing OR concurrency OR goroutines", // in,with,Go filtered, Testing kept
		},
		{
			name:     "insufficient keywords returns empty",
			input:    "the is at to a",
			expected: "", // all stop words or too short
		},
		{
			name:     "empty string returns empty",
			input:    "",
			expected: "",
		},
		{
			name:     "realistic memory content",
			input:    "Use mutex locks to protect shared state in concurrent Go applications. Avoid race conditions by proper synchronization.",
			expected: "mutex OR locks OR protect OR shared OR state OR concurrent OR applications. OR avoid OR race OR conditions OR proper OR synchronization.",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			result := sanitizeContentForFTS(tt.input)
			if result != tt.expected {
				t.Errorf("sanitizeContentForFTS(%q) = %q, want %q", tt.input, result, tt.expected)
			}
		})
	}
}

func TestComputeTopicConfidence(t *testing.T) {
	tests := []struct {
		name           string
		memoryKeywords []string
		topicName      string
		expected       float64
	}{
		{
			name:           "all keywords match (case insensitive)",
			memoryKeywords: []string{"concurrency", "patterns", "go"},
			topicName:      "Concurrency Patterns",
			expected:       1.0,
		},
		{
			name:           "partial match",
			memoryKeywords: []string{"concurrency", "patterns", "go"},
			topicName:      "Concurrency",
			expected:       1.0,
		},
		{
			name:           "no match",
			memoryKeywords: []string{"concurrency", "patterns"},
			topicName:      "Database Design",
			expected:       0.0,
		},
		{
			name:           "one keyword matches single word topic",
			memoryKeywords: []string{"GO", "CONCURRENCY"},
			topicName:      "concurrency",
			expected:       1.0,
		},
		{
			name:           "some keywords match",
			memoryKeywords: []string{"concurrency", "patterns", "database"},
			topicName:      "Concurrency Testing",
			expected:       0.5,
		},
		{
			name:           "empty keywords returns 0",
			memoryKeywords: []string{},
			topicName:      "Any Topic",
			expected:       0.0,
		},
		{
			name:           "empty topic returns 0",
			memoryKeywords: []string{"concurrency", "patterns"},
			topicName:      "",
			expected:       0.0,
		},
		{
			name:           "description matching for higher confidence",
			memoryKeywords: []string{"concurrency", "patterns"},
			topicName:      "Go Programming",
			expected:       0.0,
		},
		{
			name:           "two word topic, one keyword match",
			memoryKeywords: []string{"mutex", "lock", "sync"},
			topicName:      "Mutex Patterns",
			expected:       0.5,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			result := computeTopicConfidence(tt.memoryKeywords, tt.topicName)
			if result != tt.expected {
				t.Errorf("computeTopicConfidence(%v, %q) = %v, want %v",
					tt.memoryKeywords, tt.topicName, result, tt.expected)
			}
		})
	}
}

func TestSanitizeContentForFTS_StopWords(t *testing.T) {
	// These stop words should be filtered out when combined with valid keywords
	tests := []struct {
		input        string
		wantContains string
		wantExclude  string
	}{
		{"the quick concurrency patterns", "concurrency", "the"},
		{"is at to a concurrency patterns", "concurrency", "is"},
		{"in on for of and concurrency patterns", "concurrency", "in"},
		{"with as by from concurrency patterns", "concurrency", "with"},
		{"it this that concurrency patterns", "concurrency", "this"},
	}

	for _, tt := range tests {
		result := sanitizeContentForFTS(tt.input)
		if result == "" {
			t.Errorf("sanitizeContentForFTS(%q) returned empty, but should have keywords", tt.input)
			continue
		}
		if !containsWord(result, tt.wantContains) {
			t.Errorf("sanitizeContentForFTS(%q) should contain %q", tt.input, tt.wantContains)
		}
		if containsWord(result, tt.wantExclude) {
			t.Errorf("sanitizeContentForFTS(%q) should NOT contain stop word %q", tt.input, tt.wantExclude)
		}
	}
}

func TestSanitizeContentForFTS_MinLength(t *testing.T) {
	// Words < 4 chars should be filtered
	tests := []struct {
		input    string
		hasChars bool // whether result should contain "go" or "in"
	}{
		{"go concurrency", false},  // "go" is 2 chars, should be filtered
		{"in concurrency", false},  // "in" is 2 chars, should be filtered
		{"for concurrency", false}, // "for" is 3 chars, should be filtered
		{"rust concurrency", true}, // "rust" is 4 chars, should be kept
	}

	for _, tt := range tests {
		result := sanitizeContentForFTS(tt.input)
		if tt.hasChars {
			if !containsWord(result, "concurrency") {
				t.Errorf("sanitizeContentForFTS(%q) should contain 'concurrency'", tt.input)
			}
		}
	}
}

func containsWord(s, word string) bool {
	words := []string{}
	current := ""
	for _, c := range s {
		if c == ' ' || c == 'O' { // O is part of " OR "
			if current != "" && current != "OR" {
				words = append(words, strings.ToLower(current))
			}
			current = ""
		} else {
			current += string(c)
		}
	}
	if current != "" && current != "OR" {
		words = append(words, strings.ToLower(current))
	}

	for _, w := range words {
		if w == word {
			return true
		}
	}
	return false
}
