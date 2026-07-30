package internal

import "time"

// FormatUnixSeconds returns the RFC3339-formatted UTC string for an int64
// Unix-epoch seconds value. Used at every CLI/MCP display boundary to convert
// the int64 stored in the DB to a human-readable timestamp.
func FormatUnixSeconds(sec int64) string {
	return time.Unix(sec, 0).UTC().Format(time.RFC3339)
}

// FormatOptionalUnixSeconds returns the RFC3339-formatted UTC string for an
// int64 pointer, or empty string if the pointer is nil. Used for nullable
// timestamp columns.
func FormatOptionalUnixSeconds(sec *int64) string {
	if sec == nil {
		return ""
	}
	return time.Unix(*sec, 0).UTC().Format(time.RFC3339)
}
