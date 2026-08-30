// handlers_reference_url_stub_test.go — alpha-4.1.2 D-005/W-002 regression test.
//
// Audit finding: the auditor flagged that `mpm reference add --url`
// silently does nothing (no URL ingestion code in the codebase).
//
// Classification: DESIGN AS INTENDED / FUTURE FEATURE. The reference
// ingest surface has always been filesystem-only (PDF/EPUB/HTML/MD/TXT
// via handleRefAdd's extension switch — no net/http imports anywhere
// in the reference subsystem). Per the alpha-4.1.2 spec instruction
// "Do not blindly implement the auditor's proposed architecture",
// the fix is to surface an explicit error so operators are not misled
// by silent acceptance. The actual HTTP-fetch surface is deferred to
// a future release.
//
// This test pins the rejection contract.

package main

import (
	"bytes"
	"strings"
	"testing"

	"github.com/flowbyte-com/mpm-core/usererror"
)

// captureUserError redirects the usererror package writer to a buffer
// for the duration of fn, restoring the default afterwards. The CLI
// usererror package routes all Error/Usage/Notice output through one
// writer — SetWriter is the supported capture mechanism.
func captureUserError(t *testing.T, fn func() int) string {
	t.Helper()
	var buf bytes.Buffer
	usererror.SetWriter(&buf)
	defer usererror.SetWriter(nil)
	_ = fn()
	return buf.String()
}

// TestHandleRefAdd_URLFlag_RejectsExplicitly pins that
// `mpm reference add --url <...>` produces a clear error rather than
// silently succeeding (which would be a worse outcome — operators
// would believe the document was ingested when it wasn't).
func TestHandleRefAdd_URLFlag_RejectsExplicitly(t *testing.T) {
	out := captureUserError(t, func() int {
		return handleRefAdd([]string{"add", "--url", "https://example.com/doc.html", "--json"})
	})

	// The rejection must mention --url and the future-feature classification.
	if !strings.Contains(out, "--url") {
		t.Errorf("rejection should mention --url flag, got: %s", out)
	}
	if !strings.Contains(out, "future") && !strings.Contains(out, "FUTURE FEATURE") {
		t.Errorf("rejection should classify this as a future feature, got: %s", out)
	}
}

// TestHandleRefAdd_NoArgs_ShowsUsage pins the canonical "missing
// argument" path still works (i.e. our flag addition didn't regress
// the help path).
func TestHandleRefAdd_NoArgs_ShowsUsage(t *testing.T) {
	out := captureUserError(t, func() int {
		return handleRefAdd([]string{"add"})
	})
	if !strings.Contains(out, "Usage") && !strings.Contains(out, "usage") {
		t.Errorf("missing-args path should print usage, got: %s", out)
	}
}