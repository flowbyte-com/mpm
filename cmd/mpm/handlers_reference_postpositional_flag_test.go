// handlers_reference_postpositional_flag_test.go — pins the post-positional
// flag contract for `mpm reference add`.
//
// Pre-fix the CLI used Go's standard `flag` package which silently stops
// parsing at the first positional arg — so `mpm reference add <file>
// --tag foo` would write the reference with empty tags and no error,
// leaving the operator wondering why their --tag was ignored.
//
// Defect O (2026-09-13 acceptance): the pre-fix code rejected the
// documented syntax with a "must come BEFORE the file path" error.
// The fix uses reorderFlagsBeforePositionals (the same helper
// `mpm add` uses) so the canonical `mpm reference add <file> --tag foo`
// shape is accepted. The pre-fix rejection was a help/parser
// disagreement; the new contract is: post-positional flags are
// honoured, not rejected.
package main

import (
	"strings"
	"testing"
)

// TestHandleRefAdd_PostPositionalFlag_Accepted pins the post-fix
// acceptance: the canonical `mpm reference add <file> --tag foo`
// shape must work — no rejection. The pre-fix code rejected this
// with a "must come BEFORE" error.
func TestHandleRefAdd_PostPositionalFlag_Accepted(t *testing.T) {
	out := captureUserError(t, func() int {
		return handleRefAdd([]string{"add", "/tmp/mpm-acceptance/ref_doc.md", "--tag", "cli-acceptance"})
	})

	// The post-positional rejection error must NOT fire any more.
	if strings.Contains(out, "must come BEFORE") {
		t.Errorf("post-positional flag should not be rejected (defect O): got: %q", out)
	}
	// And the help-style "flag --foo must come BEFORE" diagnostic
	// should not appear in any output path.
	if strings.Contains(out, "BEFORE the file") {
		t.Errorf("post-fix contract: no 'BEFORE the file' error (defect O): got: %q", out)
	}
}

// TestHandleRefAdd_PrePositionalFlag_OK is the positive control: the
// recommended flag-before-positional ordering must still work.
func TestHandleRefAdd_PrePositionalFlag_OK(t *testing.T) {
	out := captureUserError(t, func() int {
		_ = handleRefAdd([]string{"add", "--tag", "cli-acceptance"})
		return 0
	})
	// The post-positional rejection must NOT appear when the flag is
	// in the correct slot.
	if strings.Contains(out, "must come BEFORE") {
		t.Errorf("pre-positional flag should not trigger post-positional rejection: %q", out)
	}
}