// Direct regression test for the 2026-09-11 T82 rune-level diff-lines bug.
//
// The 2026-09-10 fix used the diff library's line-mode encoding
// (DiffLinesToChars → DiffMain → DiffCharsToLines) and rendered with
// per-byte prefix markers in a byte-level for-loop. For inputs like
// `diff-lines alpha beta` the rendered output was:
//
//   -a-l-p-h-a+b+e+t-a
//
// i.e. each byte of each line was prefixed — exactly the buggy
// shape the user reported across four rounds.
//
// The render loop now writes one prefix per LINE (split on '\n') and
// appends '\n' after each line. This test invokes the actual
// production handler (handleDiffLines), captures its stdout via
// redirect, and pins the line-level shape:
//
//   -alpha
//   +beta
//
// Anything that re-introduces the per-byte prefix loop is caught by
// the explicit no-rune-concatenation assertion.

package main

import (
	"io"
	"os"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

// captureStdoutForDiff redirects os.Stdout while fn runs, captures
// the bytes written, and restores stdout. handleDiffLines writes
// its output via fmt.Print, so redirecting os.Stdout at the OS
// level captures the full rendered shape including the trailing
// newline.
//
// (Renamed from captureStdout to avoid collision with the existing
// helper in f8_f10_regression_test.go which uses a different pipe
// model; the collision would shadow one of them.)
func captureStdoutForDiff(t *testing.T, fn func()) string {
	t.Helper()
	r, w, err := os.Pipe()
	require.NoError(t, err)
	orig := os.Stdout
	os.Stdout = w
	t.Cleanup(func() { os.Stdout = orig })

	done := make(chan string, 1)
	go func() {
		all, _ := io.ReadAll(r)
		done <- string(all)
	}()

	fn()
	require.NoError(t, w.Close())
	return <-done
}

// TestHandleDiffLines_RuneLevelRenderFixed is the headline T82
// regression: the production handler must NOT produce the
// rune-prefixed concatenation that has been observed across four
// rounds. Inputs "alpha" and "beta" must render as `-alpha\n+beta\n`,
// not `-a-l-p-h-a+b-e-t-a` or any byte-by-byte prefix shape.
func TestHandleDiffLines_RuneLevelRenderFixed(t *testing.T) {
	out := captureStdoutForDiff(t, func() {
		code := handleDiffLines([]string{"diff-lines", "alpha", "beta"})
		require.Equal(t, 0, code, "handleDiffLines must exit 0 on a clean diff")
	})

	// The previous bug produced "-a-l-p-h-a+b+e-t-a". Pin that
	// shape as explicitly forbidden so the regression cannot
	// silently re-appear.
	if strings.Contains(out, "-a-l-p-h-a") {
		t.Fatalf("diff-lines rendered rune-prefixed concatenation (the T82 bug): %q", out)
	}
	if strings.Contains(out, "+b-e-t-a") {
		t.Fatalf("diff-lines rendered rune-prefixed insertion (the T82 bug): %q", out)
	}

	// The fixed shape: one prefix per line, newline after each.
	require.Contains(t, out, "-alpha\n",
		"removed line must be rendered as '-alpha\\n' (line-level prefix), got %q", out)
	require.Contains(t, out, "+beta\n",
		"inserted line must be rendered as '+beta\\n' (line-level prefix), got %q", out)
}

// TestHandleDiffLines_MultilineInput pins multi-line behavior: every
// line must receive exactly one prefix and end with '\n'. Pre-fix
// the render loop iterated over bytes inside each line, prefixing
// every byte — so a 3-character line produced a 6-byte output with
// 3 prefixes. Post-fix, a 3-character line produces a 4-byte
// output ('-foo\n').
func TestHandleDiffLines_MultilineInput(t *testing.T) {
	out := captureStdoutForDiff(t, func() {
		code := handleDiffLines([]string{"diff-lines", "foo\nbar\nbaz", "foo\nBAR\nbaz"})
		require.Equal(t, 0, code)
	})

	// Two unchanged lines, one changed (BAR vs bar).
	require.Contains(t, out, " foo\n", "unchanged 'foo' line must render with leading space")
	require.Contains(t, out, "-bar\n", "deleted 'bar' line must render as '-bar\\n'")
	require.Contains(t, out, "+BAR\n", "inserted 'BAR' line must render as '+BAR\\n'")
	require.Contains(t, out, " baz\n", "unchanged 'baz' line must render with leading space")

	// Pre-fix shape: '-b-a-r' was inside the rendered output. Pin
	// its absence — the byte-level prefix loop would have produced
	// '-b-a-r\n' for the deleted 'bar' line.
	require.NotContains(t, out, "-b-a-r",
		"rendered 'bar' must be line-prefixed, not byte-prefixed: %q", out)
}

// TestHandleDiffLines_EmptyInput covers the empty-string branch —
// both empty and one-side-empty. Empty input must not panic, must
// produce a well-defined output, and must not regress to the
// concatenated-character garbage the pre-fix loop would produce.
func TestHandleDiffLines_EmptyInput(t *testing.T) {
	t.Run("both empty", func(t *testing.T) {
		out := captureStdoutForDiff(t, func() {
			code := handleDiffLines([]string{"diff-lines", "", ""})
			require.Equal(t, 0, code)
		})
		// Both empty → no DiffInsert/DiffDelete entries. The output
		// should be empty (or just an unchanged-line marker); the
		// important property is no panic and no concatenation.
		if strings.Contains(out, "+-") || strings.Contains(out, "-+") {
			t.Errorf("empty/empty diff should not produce insertion/deletion pairs: %q", out)
		}
	})

	t.Run("v1 empty", func(t *testing.T) {
		out := captureStdoutForDiff(t, func() {
			code := handleDiffLines([]string{"diff-lines", "", "alpha\nbeta"})
			require.Equal(t, 0, code)
		})
		require.Contains(t, out, "+alpha\n")
		require.Contains(t, out, "+beta\n")
	})

	t.Run("v2 empty", func(t *testing.T) {
		out := captureStdoutForDiff(t, func() {
			code := handleDiffLines([]string{"diff-lines", "alpha\nbeta", ""})
			require.Equal(t, 0, code)
		})
		require.Contains(t, out, "-alpha\n")
		require.Contains(t, out, "-beta\n")
	})
}

// TestHandleDiffLines_LongLinePreservesContent pins that the render
// loop does NOT truncate, transform, or break long lines. The
// pre-fix rune-level loop could only corrupt small inputs visibly;
// for long inputs the corruption pattern was hidden. the fixed
// render loop preserves the entire line content verbatim.
func TestHandleDiffLines_LongLinePreservesContent(t *testing.T) {
	long := "this is a much longer line than alpha or beta used in earlier tests"
	out := captureStdoutForDiff(t, func() {
		code := handleDiffLines([]string{"diff-lines", long, "replacement"})
		require.Equal(t, 0, code)
	})
	require.Contains(t, out, "-"+long+"\n",
		"long deletion line must be preserved verbatim: %q", out)
	require.Contains(t, out, "+replacement\n",
		"replacement line must be preserved verbatim: %q", out)
}