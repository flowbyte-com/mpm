package internal

import "testing"

// TestIsLTMMemory pins the two-flag LTM definition. Any change here
// is load-bearing — drift silently re-enables decay on memories that
// were promoted by weight alone.
func TestIsLTMMemory(t *testing.T) {
	cases := []struct {
		name     string
		isLTM    bool
		weight   int
		expected bool
	}{
		{"explicit LTM flag, low weight", true, 1, true},
		{"explicit LTM flag, high weight", true, 10, true},
		{"weight exactly 10, flag false", false, 10, true},
		{"weight 9, flag false", false, 9, false},
		{"weight 0, flag false", false, 0, false},
		{"both false", false, 0, false},
		{"both true", true, 11, true},
		{"weight 100, flag false (extreme)", false, 100, true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := IsLTMMemory(tc.isLTM, tc.weight)
			if got != tc.expected {
				t.Errorf("IsLTMMemory(isLTM=%v, weight=%d) = %v, want %v",
					tc.isLTM, tc.weight, got, tc.expected)
			}
		})
	}
}