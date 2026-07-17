package internal

import "testing"

// TestIsStructuralPrefix_KnownTemplates verifies the predicate catches the
// template markers we actually see in production (decisions, log entries,
// section headings, quoted speech).
func TestIsStructuralPrefix_KnownTemplates(t *testing.T) {
	cases := []struct {
		in   string
		want bool
	}{
		// Decisions / log entries (the c0d7c5807 vs 077e9b207aa6be1e case)
		{"CHOICE: We will move to SQLite.", true},
		{"## Heading text", true},
		{"### Subheading", true},
		{"Fact: the dashboard shows 4.79B tokens.", true},
		{"Note: this is important", true},
		{"Decision: ship on Friday", true},
		{"Update: rolled back", true},
		{"TODO: refactor this", true},
		{"FIXME: broken", true},
		{"WARNING: deprecated", true},
		{"ERROR: out of disk", true},
		{"PR: #1234 fix the bug", true},
		{"RFC: 001 design", true},

		// Quoted speech prefixes (v: USER:)
		{"v: I think we should ship.", true},
		{"USER: hello", true},

		// Negative cases (should NOT be flagged)
		{"The first project was chaos.", false},
		{"A choice was made.", false}, // contains "choice" but not as prefix
		{"Plain prose with no marker.", false},
		{"", false},
		{"   ", false},
		{"a", false}, // too short
	}

	for _, c := range cases {
		got := IsStructuralPrefix(c.in)
		if got != c.want {
			t.Errorf("IsStructuralPrefix(%q) = %v, want %v", c.in, got, c.want)
		}
	}
}

// TestIsStructuralPrefix_HandlesWhitespace verifies leading whitespace
// (common in indented markdown) doesn't defeat the check.
func TestIsStructuralPrefix_HandlesWhitespace(t *testing.T) {
	cases := []string{
		"   CHOICE: indented decision",
		"\t## tab-indented heading",
		"\n\nFact: blank-line prefix",
	}
	for _, in := range cases {
		if !IsStructuralPrefix(in) {
			t.Errorf("expected true for whitespace-prefixed %q", in)
		}
	}
}

// TestDiscountAppliedForBothStructural simulates the discount logic at the
// unit level: when BOTH candidates share a structural prefix, the cosine
// score should be halved before threshold comparison. This pins the
// behavior without requiring a full hybrid search.
func TestDiscountAppliedForBothStructural(t *testing.T) {
	originalSim := 0.90 // above the 0.85 contradiction threshold

	applyDiscount := func(contentA, contentB string, sim float64) float64 {
		if IsStructuralPrefix(contentA) && IsStructuralPrefix(contentB) {
			return sim * 0.5
		}
		return sim
	}

	// Both structural → discount kicks in
	got := applyDiscount("CHOICE: foo", "CHOICE: bar", originalSim)
	if got != 0.45 {
		t.Errorf("expected discounted 0.45, got %v", got)
	}

	// Only one structural → no discount
	got = applyDiscount("CHOICE: foo", "Plain prose about bar.", originalSim)
	if got != originalSim {
		t.Errorf("expected unchanged %v, got %v", originalSim, got)
	}

	// Neither structural → no discount
	got = applyDiscount("Plain A", "Plain B", originalSim)
	if got != originalSim {
		t.Errorf("expected unchanged %v, got %v", originalSim, got)
	}
}

// TestDiscountBringsFalsePositiveBelowThreshold is the regression test for
// the c0d7c5807 vs 077e9b207aa6be1e false positive: cosine=0.88 with both
// starting "CHOICE: CHOICE:" should drop below 0.85 after discount.
func TestDiscountBringsFalsePositiveBelowThreshold(t *testing.T) {
	const threshold = 0.85
	originalSim := 0.88 // the measured false-positive cosine

	contentA := "CHOICE: Decommissioned Hermes built-in memory subsystem."
	contentB := "CHOICE: CHOICE: Option C — digest_wakes returns age-bucketed counts."

	sim := originalSim
	if IsStructuralPrefix(contentA) && IsStructuralPrefix(contentB) {
		sim = sim * 0.5
	}

	if sim >= threshold {
		t.Errorf("expected discounted sim < %v, got %v", threshold, sim)
	}
	if sim != 0.44 {
		t.Errorf("expected exactly 0.44, got %v", sim)
	}
}