package internal

// SummarizeBounded truncates a string to maxChars, breaking at rune boundaries.
// If the string is shorter than maxChars, it is returned unchanged.
func SummarizeBounded(s string, maxChars int) string {
	if maxChars <= 0 {
		return ""
	}
	// Convert to runes to handle multi-byte characters correctly
	runes := []rune(s)
	if len(runes) <= maxChars {
		return s
	}
	return string(runes[:maxChars])
}

// SummarizeMemory truncates content to maxChars, breaking at rune boundaries.
// Used by wake-context Phase 2C to produce bounded memory summaries.
func SummarizeMemory(content string, maxChars int) string {
	return SummarizeBounded(content, maxChars)
}

// SummarizeWork truncates a work title to maxChars, breaking at rune boundaries.
func SummarizeWork(title string, maxChars int) string {
	return SummarizeBounded(title, maxChars)
}

// SummarizeMemoryWithEllipsis truncates content to maxChars runes and appends
// a fixed suffix when truncation occurred, so callers can flag bounded echoes
// without an external boolean round-trip. Empty input never gets the suffix.
// Multibyte-safe: cut point lands on a rune boundary.
func SummarizeMemoryWithEllipsis(content string, maxChars int) (string, bool) {
	if maxChars <= 0 {
		return "", false
	}
	runes := []rune(content)
	if len(runes) <= maxChars {
		return content, false
	}
	const suffix = "... [truncated, resolve pointer for full text]"
	return string(runes[:maxChars]) + suffix, true
}
