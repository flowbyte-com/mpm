// fts5_query.go — FTS5 query construction that matches the index's tokenizer.
//
// Index schema (lessons_fts, memories_fts, shared.memories_fts):
//
//	CREATE VIRTUAL TABLE .fts USING fts5(..., tokenize='porter unicode61')
//
// The porter unicode61 tokenizer splits on whitespace AND punctuation
// (including hyphens), lowercases, and applies Porter stemming for suffix
// variants. Two bugs surface when queries don't match the index's
// tokenization:
//
//	(1) Bare hyphenated queries like "lazy-start" are interpreted by FTS5
//	    as column-filter syntax: "lazy" is treated as a column name,
//	    "-start" as an exclusion term. Result: "no such column" error or
//	    silent zero results.
//
//	(2) Wrapping the query in double quotes (the previous workaround) forces
//	    phrase matching: "lazy start" only matches the literal two-word
//	    phrase. But the porter unicode61 tokenizer splits "lazy-start" into
//	    the tokens "lazy" and "start" — those tokens are stored separately,
//	    so the literal phrase never appears in the index. Result: silent
//	    zero results.
//
// The fix: tokenize the query the same way the index tokenizes content
// (split on whitespace + hyphens + underscores + dots), and apply prefix
// wildcard to each token so partial matches work. Tokens joined with space
// gives FTS5 implicit AND. This contract is what the MCP tool prompt at
// `search_lessons` and `query_long_term_memory` promises; the implementation
// now matches it.
package internal

import (
	"strings"
	"unicode"
)

// BuildFTS5Query converts a user query into an FTS5 MATCH expression
// compatible with the porter unicode61 tokenizer. Each token gets a `*`
// suffix for prefix matching; tokens are joined with whitespace (FTS5
// implicit AND).
//
// Tokenization contract: the unicode61 indexer treats every character
// except alphanumerics (and in-word apostrophes) as a separator. The
// builder mirrors that exactly — queries like "HTTP/1.1" or "foo.bar"
// produce the same token sequence the indexer stored, so technical
// identifiers remain discoverable.
//
// Edge cases:
//   - Empty/whitespace-only input returns "" so the caller can short-circuit.
//   - FTS5 special characters (`^ " ( ) : *`) are separators, so they can
//     never cause syntax errors or silent no-op filters.
//
// Example transformations:
//
//	"lazy-start"        -> "lazy* start*"
//	"lazy start"        -> "lazy* start*"
//	"ecryptfs"          -> "ecryptfs*"
//	"lazy-start-mount"  -> "lazy* start* mount*"
//	"HTTP/1.1"          -> "http* 1* 1*"
//	"  hello   world  " -> "hello* world*"
//	"\""                -> "" (empty after stripping)
//
// Returns "" for empty input. Callers must handle the empty case (e.g. fall
// back to LIKE or return zero results); FTS5 MATCH "" raises an error.
func BuildFTS5Query(query string) string {
	if strings.TrimSpace(query) == "" {
		return ""
	}
	fields := strings.FieldsFunc(query, func(r rune) bool {
		if r == '\'' {
			return false // unicode61 keeps in-word apostrophes as token chars
		}
		return !isTokenChar(r)
	})
	// Drop tokens with no alphanumeric content (e.g. a standalone
	// apostrophe): unicode61 only preserves apostrophes INSIDE words.
	filtered := make([]string, 0, len(fields))
	for _, f := range fields {
		if strings.IndexFunc(f, isTokenChar) >= 0 {
			filtered = append(filtered, f)
		}
	}
	fields = filtered
	if len(fields) == 0 {
		return ""
	}
	parts := make([]string, len(fields))
	for i, f := range fields {
		parts[i] = f + "*"
	}
	return strings.Join(parts, " ")
}

// isTokenChar reports whether r belongs to a unicode61 token: Unicode
// letters or digits.
func isTokenChar(r rune) bool {
	return unicode.IsLetter(r) || unicode.IsDigit(r)
}
