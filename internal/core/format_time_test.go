package internal

import (
	"testing"
)

func TestFormatUnixSeconds(t *testing.T) {
	cases := []struct {
		sec      int64
		expected string
	}{
		{0, "1970-01-01T00:00:00Z"},
		{1785421960, "2026-07-30T14:32:40Z"},
	}
	for _, c := range cases {
		got := FormatUnixSeconds(c.sec)
		if got != c.expected {
			t.Errorf("FormatUnixSeconds(%d) = %q, want %q", c.sec, got, c.expected)
		}
	}
}

func TestFormatOptionalUnixSeconds(t *testing.T) {
	zero := int64(0)
	cases := []struct {
		name     string
		sec      *int64
		expected string
	}{
		{"nil", nil, ""},
		{"zero", &zero, "1970-01-01T00:00:00Z"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := FormatOptionalUnixSeconds(c.sec)
			if got != c.expected {
				t.Errorf("FormatOptionalUnixSeconds(%v) = %q, want %q", c.sec, got, c.expected)
			}
		})
	}
}
