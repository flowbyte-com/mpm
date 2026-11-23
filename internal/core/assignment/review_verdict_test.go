package assignment

import (
	"errors"
	"testing"
)

func TestExtractReviewVerdictAcceptsExactlyOneTerminalLine(t *testing.T) {
	for _, tc := range []struct {
		name string
		text string
		want string
	}{
		{"approved", "a review\n\nVERDICT: APPROVED\n", VerdictPassed},
		{"changes", "a review\nVERDICT: CHANGES REQUESTED", VerdictChangesRequested},
		{"trailing spaces", "findings\n  VERDICT: APPROVED   ", VerdictPassed},
		{"crlf", "findings\r\nVERDICT: APPROVED\r\n", VerdictPassed},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, err := ExtractReviewVerdict(tc.text)
			if err != nil {
				t.Fatalf("extract: %v", err)
			}
			if got != tc.want {
				t.Fatalf("verdict %q, want %q", got, tc.want)
			}
		})
	}
}

func TestExtractReviewVerdictRefusesAbsentAmbiguousOrConflicting(t *testing.T) {
	for _, tc := range []struct {
		name string
		text string
	}{
		{"empty", ""},
		{"no verdict", "the code is fine"},
		{"malformed", "VERDICT: APPROVE"},
		{"lowercase", "verdict: approved"},
		{"markdown", "**VERDICT: APPROVED**"},
		{"punctuation", "VERDICT: APPROVED."},
		{"nonterminal", "VERDICT: APPROVED\nthen some more prose"},
		{"conflicting", "VERDICT: APPROVED\nVERDICT: CHANGES REQUESTED"},
		{"quoted earlier", "The previous VERDICT: APPROVED was wrong.\nVERDICT: CHANGES REQUESTED"},
		{"same twice", "VERDICT: APPROVED\nVERDICT: APPROVED"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if _, err := ExtractReviewVerdict(tc.text); err == nil {
				t.Fatalf("%q was accepted", tc.text)
			} else if !errors.Is(err, ErrReviewVerdict) {
				t.Fatalf("refused with %v, want ErrReviewVerdict", err)
			}
		})
	}
}
