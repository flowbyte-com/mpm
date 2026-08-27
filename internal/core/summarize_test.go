package internal

import "testing"

func TestSummarizeMemoryWithEllipsis(t *testing.T) {
	const suffix = "... [truncated, resolve pointer for full text]"

	cases := []struct {
		name      string
		input     string
		maxChars  int
		wantOut   string
		wantTrunc bool
	}{
		{"empty stays empty", "", 256, "", false},
		{"short content untouched", "hello", 256, "hello", false},
		{"exact length untouched", "x", 1, "x", false},
		{"long content gets suffix", "xx", 1, "x" + suffix, true},
		{"truncates at rune boundary", "héllo", 2, "hé" + suffix, true},
		{"multi-byte safety", "ümlaut ümlaut ümlaut", 6, "ümlaut" + suffix, true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, trunc := SummarizeMemoryWithEllipsis(tc.input, tc.maxChars)
			if got != tc.wantOut {
				t.Errorf("summary = %q, want %q", got, tc.wantOut)
			}
			if trunc != tc.wantTrunc {
				t.Errorf("truncated = %v, want %v", trunc, tc.wantTrunc)
			}
		})
	}
}
