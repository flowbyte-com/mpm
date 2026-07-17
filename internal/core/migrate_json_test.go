package internal

import (
	"strings"
	"testing"
)

// TestParseJsonFacts_ArrayOfObjects verifies the most common shape:
// bare JSON array of memory objects.
func TestParseJsonFacts_ArrayOfObjects(t *testing.T) {
	content := `[
		{"content": "First fact.", "tags": ["a", "b"], "weight": 7},
		{"content": "Second fact.", "tags": ["c"]}
	]`

	facts, err := ParseJsonFacts(content, "/tmp/test.json")
	if err != nil {
		t.Fatalf("parse failed: %v", err)
	}

	if len(facts) != 2 {
		t.Fatalf("expected 2 facts, got %d", len(facts))
	}
	if facts[0].Content != "First fact." {
		t.Errorf("expected 'First fact.', got %q", facts[0].Content)
	}
	if facts[0].Weight != 7 {
		t.Errorf("expected weight 7, got %d", facts[0].Weight)
	}
	if !containsTag(facts[0].Tags, "a") || !containsTag(facts[0].Tags, "b") {
		t.Errorf("expected tags a+b, got %v", facts[0].Tags)
	}
}

// TestParseJsonFacts_WrappedShape verifies the wrapped {memories: [...]} shape.
func TestParseJsonFacts_WrappedShape(t *testing.T) {
	content := `{
		"source": "anthropic changelog",
		"memories": [
			{"content": "Claude 4.5 released with 1M context."}
		]
	}`

	facts, err := ParseJsonFacts(content, "/tmp/wrapped.json")
	if err != nil {
		t.Fatalf("parse failed: %v", err)
	}

	if len(facts) != 1 {
		t.Fatalf("expected 1 fact, got %d", len(facts))
	}
	if !strings.Contains(facts[0].Content, "1M context") {
		t.Errorf("unexpected content: %q", facts[0].Content)
	}
}

// TestParseJsonFacts_ContentFieldAliases verifies the parser accepts
// content / fact / text / body as the content field.
func TestParseJsonFacts_ContentFieldAliases(t *testing.T) {
	cases := []struct {
		name    string
		content string
		want    string
	}{
		{"content field", `[{"content":"alpha"}]`, "alpha"},
		{"fact field", `[{"fact":"beta"}]`, "beta"},
		{"text field", `[{"text":"gamma"}]`, "gamma"},
		{"body field", `[{"body":"delta"}]`, "delta"},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			facts, err := ParseJsonFacts(c.content, "/tmp/test.json")
			if err != nil {
				t.Fatalf("parse failed: %v", err)
			}
			if len(facts) != 1 || facts[0].Content != c.want {
				t.Errorf("expected %q, got %+v", c.want, facts)
			}
		})
	}
}

// TestParseJsonFacts_TagsAsString verifies tags can be a comma-separated
// string (Hermes-style) in addition to an array.
func TestParseJsonFacts_TagsAsString(t *testing.T) {
	content := `[{"content":"x","tags":"alpha, beta, gamma"}]`

	facts, err := ParseJsonFacts(content, "/tmp/test.json")
	if err != nil {
		t.Fatalf("parse failed: %v", err)
	}
	if len(facts) != 1 {
		t.Fatalf("expected 1 fact, got %d", len(facts))
	}
	for _, want := range []string{"alpha", "beta", "gamma"} {
		if !containsTag(facts[0].Tags, want) {
			t.Errorf("missing tag %q in %v", want, facts[0].Tags)
		}
	}
}

// TestParseJsonFacts_WeightClamping verifies out-of-range weights are clamped.
func TestParseJsonFacts_WeightClamping(t *testing.T) {
	cases := []struct {
		in   string
		want int
	}{
		{`[{"content":"x","weight":50}]`, 50},
		{`[{"content":"x","weight":150}]`, 100},
		{`[{"content":"x","weight":0}]`, 1},
		{`[{"content":"x","weight":-5}]`, 1},
		{`[{"content":"x"}]`, 5}, // default
	}
	for i, c := range cases {
		facts, _ := ParseJsonFacts(c.in, "/tmp/test.json")
		if facts[0].Weight != c.want {
			t.Errorf("case %d: expected weight %d, got %d", i, c.want, facts[0].Weight)
		}
	}
}

// TestParseJsonFacts_PartialRecovery verifies malformed entries are
// skipped but well-formed siblings still parse (fail-soft, not fail-batch).
func TestParseJsonFacts_PartialRecovery(t *testing.T) {
	content := `[
		{"content": "good first"},
		{"no_content_field": "skipped"},
		{"content": ""},
		{"content": "good last"}
	]`

	facts, err := ParseJsonFacts(content, "/tmp/test.json")
	if err != nil {
		t.Fatalf("parse failed: %v", err)
	}

	// Two should survive (good first, good last); two should be skipped
	// (no content field, empty content field)
	if len(facts) != 2 {
		t.Fatalf("expected 2 facts (partial recovery), got %d: %+v", len(facts), facts)
	}
	if facts[0].Content != "good first" {
		t.Errorf("expected 'good first', got %q", facts[0].Content)
	}
	if facts[1].Content != "good last" {
		t.Errorf("expected 'good last', got %q", facts[1].Content)
	}
}

// TestParseJsonFacts_InvalidJSON verifies malformed JSON surfaces as error.
func TestParseJsonFacts_InvalidJSON(t *testing.T) {
	_, err := ParseJsonFacts("not json", "/tmp/bad.json")
	if err == nil {
		t.Error("expected error for invalid JSON, got nil")
	}
}

// TestParseJsonFacts_MigratedTagAlwaysAdded verifies the migration
// provenance tag is always appended (for traceability).
func TestParseJsonFacts_MigratedTagAlwaysAdded(t *testing.T) {
	facts, _ := ParseJsonFacts(`[{"content":"x"}]`, "/tmp/test.json")
	if !containsTag(facts[0].Tags, "migrated") {
		t.Errorf("expected 'migrated' tag, got %v", facts[0].Tags)
	}
}