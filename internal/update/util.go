package update

import (
	"fmt"
	"time"
)

// epochToRFC3339 renders a build epoch the way rb_write_release does.
func epochToRFC3339(epoch int64) string {
	return time.Unix(epoch, 0).UTC().Format("2006-01-02T15:04:05Z")
}

// ShortSHA abbreviates a commit for display.
func ShortSHA(sha string) string {
	if len(sha) > 8 {
		return sha[:8]
	}
	return sha
}

// IsPlaceholderCommit reports whether a stamped commit carries no real
// revision (a Git-free export, an unstamped binary).
func IsPlaceholderCommit(commit string) bool {
	switch commit {
	case "", "unknown", "unknown-dirty":
		return true
	}
	return false
}

// FormatUpdateError renders a failure for the operator: what was being
// done, what went wrong, and where the recovery point is. It never
// includes credentials or paths the operator did not already know.
func FormatUpdateError(stage string, err error) string {
	return fmt.Sprintf("update failed at %s: %v", stage, err)
}
