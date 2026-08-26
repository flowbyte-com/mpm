// output_limits.go — central agent-facing payload bounds (audit findings
// F4/F5).
//
// The audit observed ~30 KB payloads echoed back through the MCP memory
// surface: mpm_memory save echoed the full stored content and query
// returned complete content inline for every hit. Persistence is never
// trimmed — only the WIRE representation is bounded, explicitly flagged,
// and paired with a pointer for full retrieval.
//
// One constant, one helper: no scattered magic numbers. The environment
// override exists so integrations with larger context windows can raise
// the bound deliberately instead of forking the code.
package internal

import (
	"os"
	"strconv"
	"unicode/utf8"
)

// DefaultMaxInlineContentBytes is the default wire-format bound for a
// single memory's inline content in tool responses.
const DefaultMaxInlineContentBytes = 2048

// MaxInlineContentBytes returns the effective inline-content bound.
func MaxInlineContentBytes() int {
	if v := os.Getenv("MPM_MAX_INLINE_CONTENT_BYTES"); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n > 0 {
			return n
		}
	}
	return DefaultMaxInlineContentBytes
}

// BoundInlineContent bounds content to MaxInlineContentBytes WITHOUT
// splitting a UTF-8 rune. Returns the (possibly identical) string and
// whether truncation occurred. Empty input is never reported truncated.
//
// Multibyte safety: walk back at most 3 bytes from the cut point to find
// a rune boundary; utf8.RuneLen guards make this exact.
func BoundInlineContent(content string) (string, bool) {
	limit := MaxInlineContentBytes()
	if len(content) <= limit {
		return content, false
	}
	cut := limit
	for cut > 0 && !utf8.RuneStart(content[cut]) {
		cut--
	}
	return content[:cut], true
}
