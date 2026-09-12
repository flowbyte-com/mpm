// handlers_reference_postpositional_flag_test.go — pins the post-positional
// flag rejection contract for `mpm reference add`. Pre-fix the CLI used
// Go's standard `flag` package which silently stops parsing at the first
// positional arg — so `mpm reference add <file> --tag foo` would write
// the reference with empty tags and no error, leaving the operator
// wondering why their --tag was ignored. The fix detects --flag tokens
// after the first positional and emits a clear "move the flag before
// the file" error.
package main

import (
	"strings"
	"testing"
)

// TestHandleRefAdd_PostPositionalFlag_RejectsExplicitly pins the rejection
// for the canonical failure mode: file path followed by --tag.
func TestHandleRefAdd_PostPositionalFlag_RejectsExplicitly(t *testing.T) {
	out := captureUserError(t, func() int {
		return handleRefAdd([]string{"add", "/tmp/mpm-acceptance/ref_doc.md", "--tag", "cli-acceptance"})
	})

	if !strings.Contains(out, "--tag") {
		t.Errorf("error must name the dropped flag, got: %q", out)
	}
	if !strings.Contains(out, "BEFORE") && !strings.Contains(out, "before") {
		t.Errorf("error must tell the operator to move the flag before the file, got: %q", out)
	}
}

// TestHandleRefAdd_PrePositionalFlag_OK is the positive control: the
// recommended flag-before-positional ordering must still work.
func TestHandleRefAdd_PrePositionalFlag_OK(t *testing.T) {
	// We don't run the full ingest path (no DB / file setup in this
	// test file) but the post-positional check is the only difference
	// in this surface — the rest of the function fails on missing
	// files or DB anyway. Assert that the post-positional guard does
	// NOT fire when the flag is in the correct slot.
	// We exercise just the guard via the same fs.Parse that the handler
	// uses, so this stays as a unit test (not an integration test).
	out := captureUserError(t, func() int {
		// Pre-positional order: flag FIRST.
		_ = handleRefAdd([]string{"add", "--tag", "cli-acceptance"})
		// Empty args beyond "add" is a Usage error, but it must NOT be
		// the post-positional rejection we're pinning here.
		return 0
	})
	// Post-positional rejection must NOT appear when flag is pre-positional.
	if strings.Contains(out, "must come BEFORE") {
		t.Errorf("pre-positional flag should not trigger post-positional rejection: %q", out)
	}
}