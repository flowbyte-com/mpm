package internal

import (
	"strings"
	"testing"
)

// TestParseMarkdownFacts_SingleSection verifies the simplest case: one
// ## heading, one body. The heading should become a tag and the body
// should be the fact content.
func TestParseMarkdownFacts_SingleSection(t *testing.T) {
	content := `## Foo

This is the body of the fact.`

	facts := ParseMarkdownFacts(content, "/tmp/test.md")

	if len(facts) != 1 {
		t.Fatalf("expected 1 fact, got %d: %+v", len(facts), facts)
	}
	if !strings.Contains(facts[0].Content, "body of the fact") {
		t.Errorf("content missing body: %q", facts[0].Content)
	}
	if facts[0].Heading != "Foo" {
		t.Errorf("expected heading 'Foo', got %q", facts[0].Heading)
	}
	if !containsTag(facts[0].Tags, "foo") {
		t.Errorf("expected slug tag 'foo', got %v", facts[0].Tags)
	}
}

// TestParseMarkdownFacts_MultipleSections splits cleanly on ## headings.
func TestParseMarkdownFacts_MultipleSections(t *testing.T) {
	content := `## One

First body.

## Two

Second body.

## Three

Third body.`

	facts := ParseMarkdownFacts(content, "/tmp/test.md")

	if len(facts) != 3 {
		t.Fatalf("expected 3 facts, got %d", len(facts))
	}
	expectedHeadings := []string{"One", "Two", "Three"}
	for i, f := range facts {
		if f.Heading != expectedHeadings[i] {
			t.Errorf("fact %d: expected heading %q, got %q", i, expectedHeadings[i], f.Heading)
		}
	}
}

// TestParseMarkdownFacts_SectionSeparator splits a section into multiple
// atomic facts via the § inline separator. The heading slug should appear
// as a tag on all sub-facts (so whole-section queries surface them).
func TestParseMarkdownFacts_SectionSeparator(t *testing.T) {
	content := `## Facts

First fact body.
§
Second fact body.
§
Third fact body.`

	facts := ParseMarkdownFacts(content, "/tmp/test.md")

	if len(facts) != 3 {
		t.Fatalf("expected 3 facts (one per §), got %d: %+v", len(facts), facts)
	}
	for i, f := range facts {
		if !containsTag(f.Tags, "facts") {
			t.Errorf("fact %d: missing section slug tag, got %v", i, f.Tags)
		}
	}
}

// TestParseMarkdownFacts_ExtractsMetadata verifies Tags:/Weight:/TTL:
// lines are stripped from content and applied to the MigratedFact.
func TestParseMarkdownFacts_ExtractsMetadata(t *testing.T) {
	content := `## Config

The cache TTL is 24 hours.

Tags: cache, config, ttl
Weight: 9
TTL: 0`

	facts := ParseMarkdownFacts(content, "/tmp/test.md")

	if len(facts) != 1 {
		t.Fatalf("expected 1 fact, got %d", len(facts))
	}
	f := facts[0]
	if f.Weight != 9 {
		t.Errorf("expected weight 9, got %d", f.Weight)
	}
	if f.TTL != "0" {
		t.Errorf("expected ttl '0', got %q", f.TTL)
	}
	if !containsTag(f.Tags, "cache") || !containsTag(f.Tags, "config") {
		t.Errorf("expected cache+config tags, got %v", f.Tags)
	}
	// The metadata lines should NOT appear in the content
	if strings.Contains(f.Content, "Tags:") {
		t.Errorf("content still contains Tags: line: %q", f.Content)
	}
}

// TestParseMarkdownFacts_KeepsShortNonEmpty verifies that short but
// non-empty paragraphs ARE kept as facts (only blank paragraphs are
// dropped). Real MEMORY.md style uses terse atomic facts; aggressive
// length filtering loses them.
func TestParseMarkdownFacts_KeepsShortNonEmpty(t *testing.T) {
	content := `## A
Hi.
## B
This is a longer body that should also survive.`

	facts := ParseMarkdownFacts(content, "/tmp/test.md")

	if len(facts) != 2 {
		t.Fatalf("expected 2 facts (short 'Hi.' kept, longer kept), got %d: %+v", len(facts), facts)
	}
	if !strings.Contains(facts[0].Content, "Hi.") {
		t.Errorf("fact 0 should contain 'Hi.', got %q", facts[0].Content)
	}
}

// TestParseMarkdownFacts_HeadingSlug verifies the heading-slug tag is
// safe (lowercased, punctuation stripped, length-capped).
func TestParseMarkdownFacts_HeadingSlug(t *testing.T) {
	content := `## MiniMax TTS (speech-2.8-hd) — HEX not Base64!

Body content.`

	facts := ParseMarkdownFacts(content, "/tmp/test.md")

	if len(facts) != 1 {
		t.Fatalf("expected 1 fact, got %d", len(facts))
	}
	slug := facts[0].Tags[0]
	// Hyphens are OK (they're word separators). Check for unsafe chars only.
	if strings.ContainsAny(slug, "()!?—–&',.\"") {
		t.Errorf("slug still has unsafe chars: %q", slug)
	}
	if len(slug) > 60 {
		t.Errorf("slug exceeds 60 chars: %q (len %d)", slug, len(slug))
	}
}

// TestSlugifyHeading exercises the slug helper directly.
func TestSlugifyHeading(t *testing.T) {
	cases := []struct {
		in, want string
	}{
		{"MiniMax TTS", "minimax-tts"},
		{"MPM (Memory Persistence Module)", "mpm-memory-persistence-module"},
		{"Foo & Bar", "foo-bar"},
		{"   Spaces   Around   ", "spaces-around"},
	}
	for _, c := range cases {
		got := slugifyHeading(c.in)
		if got != c.want {
			t.Errorf("slugifyHeading(%q) = %q, want %q", c.in, got, c.want)
		}
	}
}

// TestParseMarkdownFacts_SourceID_Unique verifies each fact gets a unique
// source ID for traceability back to the original file position.
func TestParseMarkdownFacts_SourceID_Unique(t *testing.T) {
	content := `## A

Body A.

## B

Body B.`

	facts := ParseMarkdownFacts(content, "/tmp/source.md")

	if len(facts) != 2 {
		t.Fatalf("expected 2 facts, got %d", len(facts))
	}
	if facts[0].SourceID == facts[1].SourceID {
		t.Errorf("source IDs should be unique: both %q", facts[0].SourceID)
	}
	if !strings.Contains(facts[0].SourceID, "source.md#") {
		t.Errorf("source ID should reference filename: %q", facts[0].SourceID)
	}
}

func containsTag(tags []string, want string) bool {
	for _, t := range tags {
		if t == want {
			return true
		}
	}
	return false
}