package internal

import (
	"testing"
)

func TestParseTimestampArg(t *testing.T) {
	cases := []struct {
		name    string
		input   string
		want    int64
		wantErr bool
	}{
		{"unix-int", "1785421960", 1785421960, false},
		{"unix-negative", "-1", -1, false},
		{"rfc3339", "2026-07-30T14:32:40Z", 1785421960, false},
		{"rfc3339-offset", "2026-07-30T14:32:40+00:00", 1785421960, false},
		{"garbage", "not-a-time", 0, true},
		{"empty", "", 0, true},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got, err := ParseTimestampArg(c.input)
			if c.wantErr {
				if err == nil {
					t.Errorf("ParseTimestampArg(%q) succeeded, want error", c.input)
				}
				return
			}
			if err != nil {
				t.Errorf("ParseTimestampArg(%q) error: %v", c.input, err)
			}
			if got != c.want {
				t.Errorf("ParseTimestampArg(%q) = %d, want %d", c.input, got, c.want)
			}
		})
	}
}
