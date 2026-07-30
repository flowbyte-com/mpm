package internal

import (
	"fmt"
	"strconv"
	"time"
)

// ParseTimestampArg accepts either a unix-epoch integer (seconds) or an
// RFC3339-formatted string. Used by CLI flags like --as-of, --since,
// --until, and snooze_until so users can opt into the simpler integer form
// when scripting. Returns the int64 Unix-epoch seconds representation.
func ParseTimestampArg(s string) (int64, error) {
	if s == "" {
		return 0, fmt.Errorf("empty timestamp")
	}
	// Try unix-epoch integer first (cheaper than parsing RFC3339).
	if n, err := strconv.ParseInt(s, 10, 64); err == nil {
		return n, nil
	}
	if t, err := time.Parse(time.RFC3339, s); err == nil {
		return t.Unix(), nil
	}
	return 0, fmt.Errorf("invalid timestamp: %q (want unix-epoch integer or RFC3339)", s)
}
