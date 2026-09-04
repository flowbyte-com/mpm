// f2_whitespace_input_regression_test.go — regression guard for the
// 2026-09-04 residual-inventory finding F-2.
//
// F-2 P1: handleMemoryAdd and handleRecordDecision accepted whitespace-only
// --fact and --choice strings as if they were valid, bypassing the
// empty-string guard. Pre-fix:
//
//   - mpm memory add --fact "     "  → stored row with content="     "
//   - mpm record_decision --choice "   " --context "test"  → stored a
//     decision with choice="   " and context="test"
//
// Every other CLI input path uses `strings.TrimSpace(input) == ""` for
// emptiness checks (handlers_epistemology.go:81, 94, 190 for theory
// hypothesis / tags / legacy token form). The two handlers above broke
// the pattern; this test pins the corrected shape.
//
// The invariant:
//
//   For each demonstrated required field (fact, choice), whitespace-only
//   input must be rejected at the CLI boundary with a non-zero exit
//   code and an error message. No row may be persisted.
package main

import (
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

// TestF2_MemoryAdd_WhitespaceOnlyFactRejected verifies that
// `--fact "   "` is treated as empty. Pre-fix: the row landed in the
// memories table with content="   ". Post-fix: handler returns non-zero
// and the row is NOT persisted.
func TestF2_MemoryAdd_WhitespaceOnlyFactRejected(t *testing.T) {
	// Variants of "whitespace-only" that must all be rejected.
	variants := []string{"   ", "\t", "\n", " \t \n \t"}

	for _, variant := range variants {
		variant := variant
		t.Run("variant="+strings.ReplaceAll(strings.ReplaceAll(variant, "\n", "\\n"), "\t", "\\t"), func(t *testing.T) {
			// Re-setup so each subtest starts with a clean DB.
			dm := setupMemoryAddTest(t)

			code := handleMemoryAdd([]string{"--fact", variant})
			require.NotEqual(t, 0, code,
				"handleMemoryAdd must reject whitespace-only --fact %q (got exit %d)", variant, code)

			// Verify no row was persisted with this content.
			var count int
			row := dm.SQLDB().QueryRow(
				`SELECT COUNT(*) FROM memories WHERE TRIM(content) = ''`,
			)
			require.NoError(t, row.Scan(&count))
			require.Equal(t, 0, count,
				"no row with empty/trim-empty content must be persisted for --fact=%q", variant)
		})
	}
}

// TestF2_RecordDecision_WhitespaceOnlyChoiceRejected verifies that
// `--choice "   "` is treated as empty. Pre-fix: the row landed in
// the decisions collection with content="   ". Post-fix: handler
// returns non-zero and no row is persisted.
//
// Decisions are stored in the `memories` table with collection='decisions'.
// The CLI handler joins choice/context/rationale into `content` (D-007
// cross-surface fix); the empty-choice guard must trip BEFORE that join
// so whitespace-only content never reaches the row.
func TestF2_RecordDecision_WhitespaceOnlyChoiceRejected(t *testing.T) {
	variants := []string{"   ", "\t", "\n", " \t \n \t"}

	for _, variant := range variants {
		variant := variant
		t.Run("variant="+strings.ReplaceAll(strings.ReplaceAll(variant, "\n", "\\n"), "\t", "\\t"), func(t *testing.T) {
			dm := setupMemoryAddTest(t)

			code := handleRecordDecision([]string{
				"--choice", variant,
				"--context", "test-context",
			})
			require.NotEqual(t, 0, code,
				"handleRecordDecision must reject whitespace-only --choice %q (got exit %d)", variant, code)

			var count int
			row := dm.SQLDB().QueryRow(
				`SELECT COUNT(*) FROM memories WHERE collection = 'decisions'`,
			)
			require.NoError(t, row.Scan(&count))
			require.Equal(t, 0, count,
				"no decisions row must be persisted for --choice=%q", variant)
		})
	}
}

// TestF2_MemoryAdd_NonWhitespaceFactStillAccepted is the negative-space
// guard: a real --fact value must still land in the database. The fix
// must not over-reject (it should reject whitespace-only, not content
// with leading/trailing whitespace).
func TestF2_MemoryAdd_NonWhitespaceFactStillAccepted(t *testing.T) {
	dm := setupMemoryAddTest(t)

	code := handleMemoryAdd([]string{"--fact", "  real-content  "})
	require.Equal(t, 0, code,
		"handleMemoryAdd must still accept --fact with surrounding whitespace around real content")

	var dbContent string
	row := dm.SQLDB().QueryRow(
		`SELECT content FROM memories WHERE TRIM(content) = 'real-content'`,
	)
	require.NoError(t, row.Scan(&dbContent))
	require.Equal(t, "real-content", dbContent,
		"content with surrounding whitespace should be trimmed before persistence")
}

// TestF2_RecordDecision_NonWhitespaceChoiceStillAccepted is the
// negative-space guard for decisions: a real --choice value with
// leading/trailing whitespace must still land in the database, with
// the value trimmed.
//
// The CLI handler joins choice/context/rationale into the row's
// `content` column (D-007 cross-surface fix at
// handlers_epistemology.go:361-371). Metadata does NOT carry `choice`
// on the CLI surface — that's a deliberate cross-surface alignment
// with the MCP path. We assert on content here.
func TestF2_RecordDecision_NonWhitespaceChoiceStillAccepted(t *testing.T) {
	dm := setupMemoryAddTest(t)

	code := handleRecordDecision([]string{
		"--choice", "  chose-this-option  ",
		"--context", "because",
	})
	require.Equal(t, 0, code,
		"handleRecordDecision must still accept --choice with surrounding whitespace around real content")

	var dbContent string
	row := dm.SQLDB().QueryRow(
		`SELECT content FROM memories WHERE collection = 'decisions'`,
	)
	require.NoError(t, row.Scan(&dbContent))
	require.Contains(t, dbContent, "chose-this-option",
		"choice with surrounding whitespace should be trimmed before persistence; content=%q", dbContent)
	require.NotContains(t, dbContent, "  chose-this-option  ",
		"raw input with leading/trailing whitespace must not be persisted; content=%q", dbContent)
}
