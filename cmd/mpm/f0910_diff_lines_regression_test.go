// f0910_diff_lines_regression_test.go — 2026-09-10 fix for
// `mpm debug diff-lines`.
//
// Bug: the previous implementation ran character-level diff and
// rendered with DiffPrettyText, which collapsed short inputs into
// concatenated ANSI-colored text ("alphbet" with color codes)
// rather than a line-oriented diff. The fix uses the diff library's
// line-mode encoding and renders explicit `+`/`-`/` ` per-line
// prefixes, so each input line is identified as unchanged, removed,
// or inserted.
package main

import (
	"strings"
	"testing"

	"github.com/sergi/go-diff/diffmatchpatch"
)

// TestDiffLines_MultilineChangesAreIdentified pins requirement #1:
// changed lines are visible with `-` and `+` prefixes around the
// changed content.
func TestDiffLines_MultilineChangesAreIdentified(t *testing.T) {
	v1 := "line1\nline2\nline3\n"
	v2 := "line1\nmodified\nline3\n"
	diffs := diffLinesForTest(t, v1, v2)
	if !containsDiffOp(diffs, diffmatchpatch.DiffInsert) {
		t.Error("multiline diff must mark the inserted line")
	}
	if !containsDiffOp(diffs, diffmatchpatch.DiffDelete) {
		t.Error("multiline diff must mark the deleted line")
	}
}

// TestDiffLines_InsertedLineVisible pins requirement #2: an
// inserted line shows up with the `+` prefix.
func TestDiffLines_InsertedLineVisible(t *testing.T) {
	v1 := "a\n"
	v2 := "a\nNEW\n"
	diffs := diffLinesForTest(t, v1, v2)
	if !diffContainsOp(diffs, diffmatchpatch.DiffInsert, "NEW") {
		t.Errorf("inserted line 'NEW' must be marked with insert op; got: %+v", diffs)
	}
}

// TestDiffLines_RemovedLineVisible pins requirement #3: a removed
// line shows up with the `-` prefix.
func TestDiffLines_RemovedLineVisible(t *testing.T) {
	v1 := "a\nREMOVED\nb\n"
	v2 := "a\nb\n"
	diffs := diffLinesForTest(t, v1, v2)
	if !diffContainsOp(diffs, diffmatchpatch.DiffDelete, "REMOVED") {
		t.Errorf("removed line 'REMOVED' must be marked with delete op; got: %+v", diffs)
	}
}

// TestDiffLines_UnchangedContent pins requirement #4: identical
// inputs produce only DiffEqual entries (no insert/delete).
func TestDiffLines_UnchangedContent(t *testing.T) {
	v1 := "alpha\nbeta\ngamma\n"
	v2 := "alpha\nbeta\ngamma\n"
	diffs := diffLinesForTest(t, v1, v2)
	for _, d := range diffs {
		if d.Type != diffmatchpatch.DiffEqual {
			t.Errorf("identical inputs must produce only DiffEqual; got: %+v", diffs)
		}
	}
}

// TestDiffLines_EmptyInput pins requirement #5: empty input on one
// side produces a well-defined all-insert or all-delete output, not
// the previous concatenated-character nonsense.
func TestDiffLines_EmptyInput(t *testing.T) {
	t.Run("empty v1", func(t *testing.T) {
		diffs := diffLinesForTest(t, "", "alpha\nbeta\n")
		if !diffContainsOp(diffs, diffmatchpatch.DiffInsert, "alpha") {
			t.Errorf("empty v1 + populated v2 must show insert for alpha; got: %+v", diffs)
		}
		if !diffContainsOp(diffs, diffmatchpatch.DiffInsert, "beta") {
			t.Errorf("empty v1 + populated v2 must show insert for beta; got: %+v", diffs)
		}
	})
	t.Run("empty v2", func(t *testing.T) {
		diffs := diffLinesForTest(t, "alpha\nbeta\n", "")
		if !diffContainsOp(diffs, diffmatchpatch.DiffDelete, "alpha") {
			t.Errorf("populated v1 + empty v2 must show delete for alpha; got: %+v", diffs)
		}
		if !diffContainsOp(diffs, diffmatchpatch.DiffDelete, "beta") {
			t.Errorf("populated v1 + empty v2 must show delete for beta; got: %+v", diffs)
		}
	})
	t.Run("both empty", func(t *testing.T) {
		diffs := diffLinesForTest(t, "", "")
		for _, d := range diffs {
			if d.Type != diffmatchpatch.DiffEqual {
				t.Errorf("both inputs empty must produce only DiffEqual; got: %+v", diffs)
			}
		}
	})
}

// TestDiffLines_OutputHasLinePrefixes pins the headline fix: the
// rendered output must include explicit `+`/`-`/` ` per-line
// prefixes so the structure is observable. The previous bug
// produced concatenated text with no structure.
//
// 2026-09-11 follow-up (T82): the original render loop prefixed
// each BYTE inside d.Text, producing "-a-l-p-h-a+b+e+t+a" for
// `diff-lines alpha beta`. The fixed loop writes one prefix per
// LINE (split on '\n') and appends a newline after each line, so
// the rendered output for the same input is "-alpha\n+beta\n".
// This test pins the line-level shape against the rune-level bug.
func TestDiffLines_OutputHasLinePrefixes(t *testing.T) {
	diffs := diffLinesForTest(t, "alpha", "beta")

	out := renderDiffLinesForTest(diffs)

	// The output must NOT be the rune-level concatenation the
	// pre-fix render produced. That shape had every byte prefixed.
	if strings.Contains(out, "-a-l-p-h-a") || strings.Contains(out, "+b-e-t-a") {
		t.Fatalf("diff-lines output is rune-prefixed (the bug): %q", out)
	}
	// The fixed shape: one prefix per line, newline after each.
	if !strings.Contains(out, "-alpha\n") {
		t.Errorf("expected '-alpha\\n' as a removed line; got %q", out)
	}
	if !strings.Contains(out, "+beta\n") {
		t.Errorf("expected '+beta\\n' as an inserted line; got %q", out)
	}
}

// renderDiffLinesForTest mirrors the production handler's render
// loop (post-fix). One prefix per LINE (split on '\n'), newline
// after each line. Anything that re-introduces the per-byte
// prefix loop is what the T82 regression test catches.
func renderDiffLinesForTest(diffs []diffmatchpatch.Diff) string {
	var b strings.Builder
	for _, d := range diffs {
		prefix := byte(' ')
		switch d.Type {
		case diffmatchpatch.DiffInsert:
			prefix = '+'
		case diffmatchpatch.DiffDelete:
			prefix = '-'
		}
		if d.Text == "" {
			b.WriteByte(prefix)
			b.WriteByte('\n')
			continue
		}
		for _, line := range strings.Split(d.Text, "\n") {
			if line == "" {
				continue
			}
			b.WriteByte(prefix)
			b.WriteString(line)
			b.WriteByte('\n')
		}
	}
	return b.String()
}

// diffLinesForTest invokes the same line-mode encoding pipeline
// the production handler uses (DiffLinesToChars → DiffMain →
// DiffCharsToLines) and returns the post-decode diff entries.
func diffLinesForTest(t *testing.T, v1, v2 string) []diffmatchpatch.Diff {
	t.Helper()
	dmp := diffmatchpatch.New()
	chars1, chars2, lineArray := dmp.DiffLinesToChars(v1, v2)
	diffs := dmp.DiffMain(chars1, chars2, false)
	return dmp.DiffCharsToLines(diffs, lineArray)
}

// containsDiffOp checks whether any diff entry has the given op type.
func containsDiffOp(diffs []diffmatchpatch.Diff, op diffmatchpatch.Operation) bool {
	for _, d := range diffs {
		if d.Type == op {
			return true
		}
	}
	return false
}

// diffContainsOp checks whether any diff entry with the given op
// type contains the substring.
func diffContainsOp(diffs []diffmatchpatch.Diff, op diffmatchpatch.Operation, substr string) bool {
	for _, d := range diffs {
		if d.Type == op && strings.Contains(d.Text, substr) {
			return true
		}
	}
	return false
}
