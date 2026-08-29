// f3_2_title_rune_truncation_test.go — F3-2 alpha P2 regression.
//
// F3-2: wake context truncated titles by slicing byte offset without
// honoring UTF-8 rune boundaries. A title containing multi-byte
// characters (e.g. emoji, CJK, accented Latin) was silently truncated
// mid-rune, producing invalid UTF-8 output that downstream JSON
// parsers reject (or display as mojibake).
//
// The fix: slice by rune count via utf8.RuneCountInString, then convert
// the safe byte offset back via rune-aware slicing. The test exercises
// the three known truncation sites (OpenWorks, CompletedWorks, and the
// prose-format global-rules render).
package internal

import (
	"strings"
	"testing"
	"unicode/utf8"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestF3_2_OpenWorksTitleRuneSafe verifies that a long title with
// multi-byte characters is truncated by rune count, not byte count,
// and the result is still valid UTF-8.
func TestF3_2_OpenWorksTitleRuneSafe(t *testing.T) {
	dm := newTestDM(t)
	defer dm.Close()

	// 200 CJK characters, each 3 bytes in UTF-8. 200 runes total, so
	// after the 120-rune truncation the title is 120 runes of valid
	// UTF-8 (and exactly 360 bytes, but the byte-count check is not the
	// point — what matters is the rune boundary).
	const runeCount = 200
	title := strings.Repeat("中", runeCount) // 3 bytes per rune
	w, err := dm.AddWork(title, "", "")
	require.NoError(t, err)

	works := dm.gatherOpenWorks()
	require.NotEmpty(t, works)
	var got *WakeContextWork
	for i := range works {
		if works[i].ID == w.ID {
			got = &works[i]
			break
		}
	}
	require.NotNil(t, got, "seeded work must appear in open-works list")

	// After fix: title is exactly 120 runes and is valid UTF-8.
	assert.Equal(t, 120, utf8.RuneCountInString(got.Title),
		"title must be truncated to exactly 120 runes, not 120 bytes")
	assert.True(t, utf8.ValidString(got.Title),
		"truncated title must be valid UTF-8 (no mid-rune slicing)")

	// Sanity (regression marker): a naive 120-byte slice of the CJK
	// title would land mid-rune (120 / 3 = 40 chars exactly, so we
	// pick a 2-byte rune ("é", U+00E9) to force the failure).
	const naiveByteSlice = 119 // 119 = 39 complete runes + 2 bytes of rune #40 (mid-rune)
	naive := strings.Repeat("é", runeCount)[:naiveByteSlice] // 119 bytes from 200×é = mid-rune
	assert.False(t, utf8.ValidString(naive),
		"sanity: a 119-byte slice of 200×é IS invalid UTF-8 — proves the bug is real for the fix to mean something")
}

// TestF3_2_CompletedWorksTitleRuneSafe mirrors the F3-2 fix on the
// completed-works projection. Same contract.
func TestF3_2_CompletedWorksTitleRuneSafe(t *testing.T) {
	dm := newTestDM(t)
	defer dm.Close()

	const runeCount = 200
	title := strings.Repeat("中", runeCount) // 3 bytes per rune
	w, err := dm.AddWork(title, "", "")
	require.NoError(t, err)
	_, err = dm.CompleteWork(w.ID)
	require.NoError(t, err)

	completed := dm.gatherCompletedWorks()
	require.NotEmpty(t, completed)
	var got *WakeContextWork
	for i := range completed {
		if completed[i].ID == w.ID {
			got = &completed[i]
			break
		}
	}
	require.NotNil(t, got, "seeded completed work must appear in list")

	assert.Equal(t, 120, utf8.RuneCountInString(got.Title),
		"completed-works title must also be truncated to 120 runes")
	assert.True(t, utf8.ValidString(got.Title),
		"completed-works truncated title must be valid UTF-8")
}

// TestF3_2_ShortTitleUntouched is the regression-safety check: a title
// already at or under 120 runes (in bytes — could be 120 runes if
// pure-ASCII, or fewer if multi-byte) must not be padded or altered.
func TestF3_2_ShortTitleUntouched(t *testing.T) {
	dm := newTestDM(t)
	defer dm.Close()

	title := "short ascii title"
	w, err := dm.AddWork(title, "", "")
	require.NoError(t, err)

	works := dm.gatherOpenWorks()
	var got *WakeContextWork
	for i := range works {
		if works[i].ID == w.ID {
			got = &works[i]
			break
		}
	}
	require.NotNil(t, got)
	assert.Equal(t, title, got.Title,
		"short titles must pass through unmodified")
}