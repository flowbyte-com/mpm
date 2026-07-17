package internal

import "testing"

func TestIsSuperseded(t *testing.T) {
	cases := []struct {
		in   string
		want bool
	}{
		{"", false},
		{"plain", false},
		{"superseded", true},
		{"superseded-by:abc123", true},
		{"foo, superseded, bar", true},
		{"foo, SUPERSEDED, bar", false}, // case-sensitive — intentional
	}
	for _, c := range cases {
		got := isSupersededTagsString(c.in)
		if got != c.want {
			t.Errorf("isSupersededTagsString(%q) = %v, want %v", c.in, got, c.want)
		}
	}
}
