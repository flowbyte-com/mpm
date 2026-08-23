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

// SummarizeWork truncates a work title to maxChars, breaking at rune boundaries.
func SummarizeWork(title string, maxChars int) string {
	return SummarizeBounded(title, maxChars)
}
